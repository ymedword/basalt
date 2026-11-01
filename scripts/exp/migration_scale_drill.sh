#!/bin/sh
# migration_scale_drill.sh —— US-16 T008c / D-27：迁移单事务的**逐级放大**演练。
#
# 冻结条件（颗粒度文档 D-27）：单事务 + 失败整体回滚是正确性要求，B **不**做分段导入；
# 演练只给**本机、该测试数据下的观察读数及限制**，不冻结条数或字节上限、不外推成普遍
# 容量保证（review ② D-2，2026-11-01 裁决）；超限或资源不足时给"扩大维护窗口 /
# 评估分区"的明确指引，**不得**静默改成部分提交。本脚本每一级都量同一组读数。
#
# 每级量六项（缺一项该级作废，不拿别级的数凑）：
#   ① 消息行数 / 源库字节      ② 导出耗时 / dump 字节
#   ③ 导入耗时（单事务）        ④ 目标表体积（表 + 索引 + TOAST）
#   ⑤ WAL 总增量（导入前后 LSN 差）⑥ WAL 采样峰值（50 ms 间隔的最大单位时间增量）
#
# **峰值是采样值，不是瞬时真峰值**：采样线程自己也要连库，间隔抖动按实测间隔折算；
# 报数时一律带采样口径，不得把采样峰值写成"WAL 峰值"。
#
# 运行环境是**硬性前置**，缺一即失败、不跳过（口径同 scripts/us16-migrate-drill.sh）：
#   - `BASALT_TEST_PG_DSN`：只由 dev-env/scripts/with-postgres.sh 注入，本脚本从不回显；
#   - `psql` 与 `go`：Windows Git Bash 没有 psql，必须经 with-postgres.sh 在 WSL 内跑。
#
# 本演练**只**在临时工作区与临时 schema 上动作：不碰既有实例、不起 serve、不发外部请求；
# 每级结束（无论成败）都删临时目录并 DROP 演练 schema。
#
# 用法：
#   sh scripts/exp/migration_scale_drill.sh                 # 默认 3000 30000 300000
#   sh scripts/exp/migration_scale_drill.sh 1000 10000      # 自定义规模
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

fail() {
    printf 'scale-drill: %s\n' "$*" >&2
    exit 1
}

if [ -z "${BASALT_TEST_PG_DSN:-}" ]; then
    fail "缺少 BASALT_TEST_PG_DSN——本演练必须经 dev-env/scripts/with-postgres.sh 运行（WSL 内），缺 DSN 即失败，不跳过"
fi
command -v psql >/dev/null 2>&1 ||
    fail "找不到 psql——Windows Git Bash 没有 psql，请在 WSL 内经 dev-env/scripts/with-postgres.sh 运行"
command -v go >/dev/null 2>&1 || fail "找不到 go 工具链"
command -v sha256sum >/dev/null 2>&1 || fail "找不到 sha256sum"
command -v date >/dev/null 2>&1 || fail "找不到 date"

SCALES=${*:-"3000 30000 300000"}

TMP=$(mktemp -d "${TMPDIR:-/tmp}/us16-scale-drill.XXXXXX")
LOG_DIR="$TMP/logs"
mkdir -p "$LOG_DIR"

SCHEMA=""
SCHEMA_CREATED=0
SAMPLER_PID=""

cleanup() {
    status=$?
    if [ -n "$SAMPLER_PID" ]; then
        kill "$SAMPLER_PID" >/dev/null 2>&1 || true
        wait "$SAMPLER_PID" 2>/dev/null || true
    fi
    if [ "$SCHEMA_CREATED" = 1 ] && [ -n "$SCHEMA" ]; then
        psql -X -q -v ON_ERROR_STOP=1 -d "$BASALT_TEST_PG_DSN" \
            -c "DROP SCHEMA IF EXISTS $SCHEMA CASCADE" >/dev/null 2>&1 || true
    fi
    rm -rf "$TMP"
    exit "$status"
}
trap cleanup EXIT INT TERM

# ── 被测二进制与夹具：与 us16-migrate-drill.sh 同一构建口径 ─────────────────────
printf '%s\n' '===== 构建被测二进制与夹具 ====='
mkdir -p "$TMP/bin"
(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=us16-scale-drill" \
    -o "$TMP/bin/basalt" ./cmd/basalt) >"$LOG_DIR/build.log" 2>&1 ||
    { cat "$LOG_DIR/build.log" >&2; fail "构建 basalt 失败"; }
# 夹具在 `_fixtures/`：不进通配符构建，必须显式路径单独构建。
(cd "$ROOT" && CGO_ENABLED=0 go build -trimpath \
    -o "$TMP/bin/us16fixture" ./scripts/_fixtures/us16fixture) >"$LOG_DIR/build-fixture.log" 2>&1 ||
    { cat "$LOG_DIR/build-fixture.log" >&2; fail "构建 us16fixture 失败"; }
BIN="$TMP/bin/basalt"
printf 'basalt_sha256=%s\n' "$(sha256sum "$BIN" | awk '{print $1}')"
printf 'fixture_sha256=%s\n' "$(sha256sum "$TMP/bin/us16fixture" | awk '{print $1}')"
printf 'pg_version=%s\n' "$(psql -X -A -t -d "$BASALT_TEST_PG_DSN" -c 'SHOW server_version')"
printf 'scales=%s\n' "$SCALES"

# ── 判卷助手 ──────────────────────────────────────────────────────────────────
# q 直接连集群（不带 search_path）：建 / 删 schema 与读 LSN 都走这一条。
q() {
    psql -X -q -A -t -v ON_ERROR_STOP=1 -d "$BASALT_TEST_PG_DSN" -c "$1" || {
        printf 'scale-drill: psql 查询失败：%s\n' "$1" >&2
        exit 1
    }
}

now_ns() {
    date +%s%N
}

# 目录累计字节（dump 包：snapshot + manifest + 逐表 JSONL）。
dir_bytes() {
    find "$1" -type f -exec stat -c %s {} + | awk '{s+=$1} END {printf "%d", s}'
}

# 每一步都用 `/usr/bin/time -v` 包一层：import 的峰值内存是与 WAL 并列的**另一项**读数
# （WAL 管磁盘，RSS 管内存），只记 WAL 会低估该级的资源占用。没有该二进制时降级为直跑，
# RSS 记空并在汇总里标明——不编一个数出来。
TIME_BIN=""
if [ -x /usr/bin/time ]; then
    TIME_BIN=/usr/bin/time
fi

step() {
    name=$1
    shift
    log="$LOG_DIR/$name.log"
    status=0
    if [ -n "$TIME_BIN" ]; then
        (cd "$WORK" && "$TIME_BIN" -v "$BIN" "$@") >"$log" 2>&1 || status=$?
    else
        (cd "$WORK" && "$BIN" "$@") >"$log" 2>&1 || status=$?
    fi
    if [ "$status" -ne 0 ]; then
        printf 'scale-drill: %s 失败（退出码 %s），完整输出如下：\n' "$name" "$status" >&2
        cat "$log" >&2
        exit 1
    fi
}

# rss_of 读 `/usr/bin/time -v` 的峰值 RSS（KiB）；没有该工具时输出空串。
rss_of() {
    sed -n 's/.*Maximum resident set size (kbytes): //p' "$LOG_DIR/$1.log" | head -n 1
}

field() {
    sed -n "s/^$2//p" "$LOG_DIR/$1.log" | head -n 1
}

# 目标表的物理体积：表本体 + TOAST + 全部索引。归一到 KiB 便于跨级比较。
table_bytes() {
    q "SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0)
         FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = '$SCHEMA'
          AND (c.relname = '$1' OR c.relname IN (
                SELECT indexrelname FROM pg_stat_user_indexes
                 WHERE schemaname = '$SCHEMA' AND relname = '$1'))"
}

schema_bytes() {
    q "SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0)
         FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = '$SCHEMA' AND c.relkind IN ('r','m','i','t')"
}

# ── 逐级放大 ──────────────────────────────────────────────────────────────────
printf 'scale,messages,sessions,runs,source_bytes,dump_bytes,export_ms,verify_dump_ms,import_ms,verify_target_ms,window_ms,export_rss_kb,import_rss_kb,wal_bytes,wal_peak_bytes_per_s,target_total_bytes,messages_total_bytes,messages_index_bytes\n' \
    >"$TMP/readings.csv"

for SCALE in $SCALES; do
    case "$SCALE" in
        ''|*[!0-9]*) fail "规模必须是正整数（实收 $SCALE）" ;;
    esac
    SESSIONS=$((SCALE / 50))
    [ "$SESSIONS" -ge 1 ] || SESSIONS=1
    [ "$SESSIONS" -le "$SCALE" ] || SESSIONS=$SCALE

    printf '\n%s\n' "===== 规模 $SCALE 条消息（$SESSIONS 个会话） ====="

    SCHEMA="us16_drill_scale_${SCALE}_$(date -u +%Y%m%d%H%M%S)_$$"
    case "$SCHEMA" in
        *[!a-z0-9_]*) fail "内部错误：演练 schema 名含非法字符 $SCHEMA" ;;
    esac
    q "CREATE SCHEMA $SCHEMA" >/dev/null
    SCHEMA_CREATED=1

    WORK="$TMP/work-$SCALE"
    mkdir -p "$WORK"
    cat >"$WORK/basalt.yaml" <<'YAML'
# 演练最小配置：只有运行期库连接；DSN 经 ${BASALT_DRILL_DSN} 从环境注入。
database:
  dsn: ${BASALT_DRILL_DSN}
YAML
    case "$BASALT_TEST_PG_DSN" in
        *\?*) export BASALT_DRILL_DSN="$BASALT_TEST_PG_DSN&search_path=$SCHEMA,public" ;;
        *) export BASALT_DRILL_DSN="$BASALT_TEST_PG_DSN?search_path=$SCHEMA,public" ;;
    esac

    step "init-$SCALE" init
    step "adopt-$SCALE" instance adopt
    (cd "$WORK" && "$TMP/bin/us16fixture" seed-scale \
        --db "$WORK/.basalt/basalt.db" --messages "$SCALE" --sessions "$SESSIONS") \
        >"$LOG_DIR/seed-$SCALE.log" 2>&1 ||
        { cat "$LOG_DIR/seed-$SCALE.log" >&2; fail "灌数失败（规模 $SCALE）"; }
    SEEDED=$(sed -n 's/^messages=//p' "$LOG_DIR/seed-$SCALE.log" | head -n1)
    SOURCE_BYTES=$(sed -n 's/^db_bytes=//p' "$LOG_DIR/seed-$SCALE.log" | head -n1)
    [ "$SEEDED" = "$SCALE" ] || fail "灌数行数不符：期望 $SCALE，实得 $SEEDED"

    # ① 导出：计时 + 包体积。
    WINDOW_START=$(now_ns)
    PKG="$TMP/pkg-$SCALE"
    T0=$(now_ns)
    step "export-$SCALE" db export --out "$PKG"
    T1=$(now_ns)
    EXPORT_MS=$(( (T1 - T0) / 1000000 ))
    EXPORT_RSS=$(rss_of "export-$SCALE")
    DUMP_BYTES=$(dir_bytes "$PKG")
    T4=$(now_ns)
    step "verify-dump-$SCALE" db verify --dump "$PKG"
    T5=$(now_ns)
    VERIFY_DUMP_MS=$(( (T5 - T4) / 1000000 ))

    # ② 导入：单事务。WAL 用"导入前后 LSN 差"记总增量，另起采样线程记峰值。
    LSN_BEFORE=$(q "SELECT pg_current_wal_lsn()")
    SAMPLER="$TMP/lsn-$SCALE.txt"
    : >"$SAMPLER"
    (
        while :; do
            printf '%s %s\n' "$(now_ns)" "$(psql -X -A -t -d "$BASALT_TEST_PG_DSN" -c 'SELECT pg_current_wal_lsn()' 2>/dev/null)"
            sleep 0.05
        done
    ) >>"$SAMPLER" 2>/dev/null &
    SAMPLER_PID=$!

    T2=$(now_ns)
    step "import-$SCALE" db import --dump "$PKG"
    T3=$(now_ns)
    IMPORT_MS=$(( (T3 - T2) / 1000000 ))
    IMPORT_RSS=$(rss_of "import-$SCALE")

    kill "$SAMPLER_PID" >/dev/null 2>&1 || true
    wait "$SAMPLER_PID" 2>/dev/null || true
    SAMPLER_PID=""
    LSN_AFTER=$(q "SELECT pg_current_wal_lsn()")

    WAL_BYTES=$(q "SELECT pg_wal_lsn_diff('$LSN_AFTER'::pg_lsn, '$LSN_BEFORE'::pg_lsn)::bigint")
    # 峰值：相邻样本的 LSN 差 ÷ 相邻样本的真实间隔，取最大单位时间增量（B/s）。
    WAL_PEAK=$(awk '
        {
            lsn[$1] = $2
            stamp[NR] = $1
        }
        END {
            peak = 0
            for (i = 2; i <= NR; i++) {
                a = stamp[i-1]; b = stamp[i]
                if (b <= a || lsn[a] == "" || lsn[b] == "") continue
                bytes = lsn_bytes(lsn[b]) - lsn_bytes(lsn[a])
                if (bytes < 0) continue
                rate = bytes * 1000000000 / (b - a)
                if (rate > peak) peak = rate
            }
            printf "%.0f", peak
        }
        function lsn_bytes(v,   parts, hi, lo) {
            split(v, parts, "/")
            hi = hex2dec(parts[1]); lo = hex2dec(parts[2])
            return hi * 4294967296 + lo
        }
        function hex2dec(h,   i, c, n, d) {
            h = tolower(h); n = 0
            for (i = 1; i <= length(h); i++) {
                c = substr(h, i, 1)
                d = index("0123456789abcdef", c) - 1
                if (d < 0) return -1
                n = n * 16 + d
            }
            return n
        }' "$SAMPLER")
    [ -n "$WAL_PEAK" ] || WAL_PEAK=0

    T6=$(now_ns)
    step "verify-target-$SCALE" db verify --dump "$PKG" --target
    T7=$(now_ns)
    VERIFY_TARGET_MS=$(( (T7 - T6) / 1000000 ))
    WINDOW_MS=$(( (T7 - WINDOW_START) / 1000000 ))

    TARGET_BYTES=$(schema_bytes)
    MSG_TOTAL=$(table_bytes messages)
    # messages 的索引单独量：GIN(tsv) + GIN(trgm) 是导入开销的主项，混在合计里看不见。
    MSG_INDEX=$(q "SELECT coalesce(sum(pg_relation_size(i.indexrelid)), 0)
                     FROM pg_stat_user_indexes i
                    WHERE i.schemaname = '$SCHEMA' AND i.relname = 'messages'")
    IMPORTED_ROWS=$(q "SELECT count(*) FROM $SCHEMA.messages")
    [ "$IMPORTED_ROWS" = "$SCALE" ] || fail "导入行数不符：期望 $SCALE，实得 $IMPORTED_ROWS"

    printf '%s\n' "----- 规模 $SCALE 读数 -----"
    printf 'messages=%s sessions=%s runs=%s\n' "$SCALE" "$SESSIONS" "$SESSIONS"
    printf 'source_bytes=%s dump_bytes=%s\n' "$SOURCE_BYTES" "$DUMP_BYTES"
    printf 'export_ms=%s verify_dump_ms=%s import_ms=%s verify_target_ms=%s window_ms=%s\n' \
        "$EXPORT_MS" "$VERIFY_DUMP_MS" "$IMPORT_MS" "$VERIFY_TARGET_MS" "$WINDOW_MS"
    printf 'export_rss_kb=%s import_rss_kb=%s\n' "${EXPORT_RSS:-未测量}" "${IMPORT_RSS:-未测量}"
    printf 'wal_bytes=%s wal_peak_bytes_per_s=%s（50ms 采样峰值，非瞬时真峰值）\n' "$WAL_BYTES" "$WAL_PEAK"
    printf 'target_total_bytes=%s messages_total_bytes=%s messages_index_bytes=%s\n' \
        "$TARGET_BYTES" "$MSG_TOTAL" "$MSG_INDEX"
    printf 'import_report_rows=%s\n' "$IMPORTED_ROWS"

    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
        "$SCALE" "$SCALE" "$SESSIONS" "$SESSIONS" "$SOURCE_BYTES" "$DUMP_BYTES" \
        "$EXPORT_MS" "$VERIFY_DUMP_MS" "$IMPORT_MS" "$VERIFY_TARGET_MS" "$WINDOW_MS" \
        "${EXPORT_RSS:-}" "${IMPORT_RSS:-}" "$WAL_BYTES" "$WAL_PEAK" \
        "$TARGET_BYTES" "$MSG_TOTAL" "$MSG_INDEX" >>"$TMP/readings.csv"

    q "DROP SCHEMA $SCHEMA CASCADE" >/dev/null 2>&1
    SCHEMA_CREATED=0
    SCHEMA=""
    rm -rf "$PKG" "$WORK"
done

printf '\n%s\n' '===== 逐级放大读数汇总（CSV） ====='
cat "$TMP/readings.csv"
printf '%s\n' '口径：wal_bytes = 导入单事务前后 pg_current_wal_lsn 差；wal_peak 为 50ms 采样峰值（非瞬时真峰值）；'
printf '%s\n' '     target_total_bytes = 整个演练 schema 的 pg_total_relation_size 合计；messages_index_bytes 含 GIN(tsv) 与 GIN(trgm)。'
printf '%s\n' '所有规模均在空 schema 上完整跑通，未出现失败重试；单事务语义未做任何分段。'
