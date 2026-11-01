#!/bin/sh
# us16-migrate-drill.sh —— US-16 迁移演练（T078）：导出 → 复核 → 导入 → 目标复核 → 切换 →
# 回退窗口断言 → 回退（丢弃目标库），并逐项留证三维回退判据的本次读数。
#
# 运行环境是**硬性前置**，缺一即失败、不跳过：
#   - `BASALT_TEST_PG_DSN`：只由 dev-env/scripts/with-postgres.sh 注入，本脚本从不回显；
#   - `psql` 与 `go`：Windows Git Bash 没有 psql，本演练**必须**经 with-postgres.sh 在
#     WSL 内运行——同一 stage 的 us16-e2e 也在同一环境里临时构建二进制，两处
#     `binary_sha256` 因此才可能一致（runner 要求 process 级 check 跨 check 同哈希）。
#
# 为什么在这里自己构建：`scripts/acceptance` 的 process 级 check 必须在日志里给出
# **恰好一处** `binary_sha256=`，且与同 stage 的 us16-e2e 相符。构建参数逐字沿用
# internal/e2e/harness_test.go 的 buildBinary（`-trimpath`、同一 `-ldflags`、CGO 关闭），
# 同样的源码与工具链下产物逐字节相同；设置了 BASALT_E2E_BINARY 时两处都直接用候选
# 文件，同样相符。
#
# 本演练**只**在临时工作区与临时 schema 上动作：不碰既有实例、不起 serve、不发任何
# 外部请求；结束（无论成败）都删临时目录并 DROP 演练 schema。
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

fail() {
    printf 'drill: %s\n' "$*" >&2
    exit 1
}

# ── 前置：DSN 与工具链（fail-closed，绝不降级为跳过） ──────────────────────────
if [ -z "${BASALT_TEST_PG_DSN:-}" ]; then
    fail "缺少 BASALT_TEST_PG_DSN——本演练必须经 dev-env/scripts/with-postgres.sh 运行（WSL 内），缺 DSN 即失败，不跳过"
fi
command -v psql >/dev/null 2>&1 ||
    fail "找不到 psql——Windows Git Bash 没有 psql，请在 WSL 内经 dev-env/scripts/with-postgres.sh 运行"
command -v go >/dev/null 2>&1 || fail "找不到 go 工具链"
command -v sha256sum >/dev/null 2>&1 || fail "找不到 sha256sum"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/us16-migrate-drill.XXXXXX")
LOG_DIR="$TMP/logs"
mkdir -p "$LOG_DIR"

# schema 名只允许 [a-z0-9_]：它会被直接拼进 DDL / 查询，先自证字符集再使用。
SCHEMA="us16_drill_migrate_$(date -u +%Y%m%d%H%M%S)_$$"
case "$SCHEMA" in
    *[!a-z0-9_]*) fail "内部错误：演练 schema 名含非法字符 $SCHEMA" ;;
esac
SCHEMA_CREATED=0

cleanup() {
    status=$?
    if [ "$SCHEMA_CREATED" = 1 ]; then
        psql -X -q -v ON_ERROR_STOP=1 -d "$BASALT_TEST_PG_DSN" \
            -c "DROP SCHEMA IF EXISTS $SCHEMA CASCADE" >/dev/null 2>&1 || true
    fi
    rm -rf "$TMP"
    exit "$status"
}
trap cleanup EXIT INT TERM

# ── 被测二进制：候选优先，否则按 e2e 的临时构建口径构建 ────────────────────────
build_binary() {
    mkdir -p "$TMP/bin"
    target="$TMP/bin/basalt"
    if ! (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath \
        -ldflags "-X main.version=us15-e2e" -o "$target" ./cmd/basalt) >"$LOG_DIR/build.log" 2>&1; then
        cat "$LOG_DIR/build.log" >&2
        return 1
    fi
    printf '%s' "$target"
}

if [ -n "${BASALT_E2E_BINARY:-}" ]; then
    case "$BASALT_E2E_BINARY" in
        /*) ;;
        *) fail "BASALT_E2E_BINARY 必须是绝对路径（实际 $BASALT_E2E_BINARY）" ;;
    esac
    [ -f "$BASALT_E2E_BINARY" ] || fail "BASALT_E2E_BINARY 指向的文件不存在：$BASALT_E2E_BINARY"
    BIN=$BASALT_E2E_BINARY
    BIN_SOURCE=candidate
else
    BIN=$(build_binary) || fail "构建被测二进制失败（见上方 go build 输出）"
    BIN_SOURCE=temp
fi
BIN_SHA=$(sha256sum "$BIN" | awk '{print $1}')
if [ -n "${BASALT_E2E_SHA256:-}" ]; then
    declared=$(printf '%s' "$BASALT_E2E_SHA256" | tr 'A-F' 'a-f')
    [ "$declared" = "$BIN_SHA" ] || fail "候选二进制哈希不符：BASALT_E2E_SHA256 声明 $declared，实际 $BIN_SHA"
fi
printf 'binary_source=%s\n' "$BIN_SOURCE"
printf 'binary_sha256=%s\n' "$BIN_SHA"

# ── 临时工作区与配置（DSN 只经环境变量进入子进程，不落盘明文、不回显） ─────────
WORK="$TMP/work"
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

# ── 判卷助手 ──────────────────────────────────────────────────────────────────
q() {
    psql -X -q -A -t -v ON_ERROR_STOP=1 -d "$BASALT_TEST_PG_DSN" -c "$1" || {
        printf 'drill: psql 查询失败：%s\n' "$1" >&2
        exit 1
    }
}

step() {
    name=$1
    shift
    log="$LOG_DIR/$name.log"
    if ! (cd "$WORK" && "$BIN" "$@") >"$log" 2>&1; then
        printf 'drill: %s 失败（退出码非 0），完整输出如下：\n' "$name" >&2
        cat "$log" >&2
        exit 1
    fi
    printf '%s\n' "----- $name -----"
    cat "$log"
}

step_must_fail() {
    name=$1
    shift
    log="$LOG_DIR/$name.log"
    set +e
    (cd "$WORK" && "$BIN" "$@") >"$log" 2>&1
    code=$?
    set -e
    [ "$code" -ne 0 ] || fail "$name 本应失败但退出码为 0"
    printf '%s\n' "----- $name（预期失败，退出码 $code） -----"
    cat "$log"
    EXPECTED_FAIL_CODE=$code
}

expect_contains() {
    log="$LOG_DIR/$1.log"
    needle=$2
    what=$3
    grep -qF -- "$needle" "$log" || {
        printf 'drill: %s 缺少预期内容「%s」（日志 %s）\n' "$what" "$needle" "$log" >&2
        exit 1
    }
}

expect_eq() {
    what=$1
    want=$2
    got=$3
    if [ "$want" != "$got" ]; then
        printf 'drill: %s 不符：期望 %s，实际 %s\n' "$what" "$want" "$got" >&2
        exit 1
    fi
}

field() {
    sed -n "s/^$2//p" "$LOG_DIR/$1.log" | head -n 1
}

# 工作区逐文件快照：路径与内容一起进摘要，任何新增 / 删除 / 改写都会变。
snapshot() {
    (cd "$1" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) | sha256sum | awk '{print $1}'
}

# ── 演练开始 ──────────────────────────────────────────────────────────────────
printf '%s\n' "演练 schema：$SCHEMA（结束即 DROP）"
q "CREATE SCHEMA $SCHEMA" >/dev/null
SCHEMA_CREATED=1

printf '%s\n' '===== ① 引导 SQLite v6 源实例（init） ====='
step init init
expect_contains init "schema v6" "init 必须报告 SQLite 源谱系版本"

printf '%s\n' '===== ② 产品路径补写实例名册（instance adopt，离线、零外呼） ====='
step adopt instance adopt
expect_contains adopt "命中判定：案例 4" "全新工作区应命中案例 4（无记录、无密文）"
expect_contains adopt "写实例记录：false（采纳只写名册）" "adopt 只写名册"
expect_contains adopt "名册：" "adopt 必须报出名册路径"

printf '%s\n' '===== ③ 导出迁移包（db export） ====='
step export db export --out "$TMP/pkg"
expect_contains export "未归类 0 条" "全新工作区不得有未归类文件"
expect_contains export "核查状态：not_required（可交付）" "无未归类文件时应为可交付包"
EXPORT_BATCH=$(field export "迁移批次：")
EXPORT_SNAPSHOT=$(field export "源快照 SHA-256：")
EXPORT_INSTANCE=$(field export "源实例：")
printf '演练读数：批次 %s / 源实例 %s / 源快照 %s\n' "$EXPORT_BATCH" "$EXPORT_INSTANCE" "$EXPORT_SNAPSHOT"

printf '%s\n' '===== ④ 离线复核迁移包（db verify --dump） ====='
step verify-dump db verify --dump "$TMP/pkg"
expect_contains verify-dump "结论：通过（" "迁移包离线复核必须通过"

printf '%s\n' '===== ⑤ 导入空目标库（db import，单事务） ====='
step import db import --dump "$TMP/pkg"
expect_contains import "合计 " "导入必须报出合计行数"
if grep -qF "storage_cutover" "$LOG_DIR/import.log"; then
    fail "storage_cutover 不在迁移清单内，导入输出不应出现该表"
fi
CUTOVER_IMPORTED=$(q "SELECT count(*) FROM $SCHEMA.storage_cutover")
expect_eq "导入后 storage_cutover 行数" 0 "$CUTOVER_IMPORTED"
MGMT_AFTER_IMPORT=$(q "SELECT count(*) FROM $SCHEMA.management_operations")
printf '演练读数：storage_cutover 行数=%s / management_operations 行数=%s（导入基线）\n' \
    "$CUTOVER_IMPORTED" "$MGMT_AFTER_IMPORT"

printf '%s\n' '===== ⑥ 复核目标库（db verify --target，只读会话） ====='
step verify-target db verify --dump "$TMP/pkg" --target
expect_contains verify-target "结论：通过（" "目标库复核必须通过"

printf '%s\n' '===== ⑦ 断言切换前状态：无切换记录、源库未变 ====='
SRC_DB_SHA_BEFORE=$(sha256sum "$WORK/.basalt/basalt.db" | awk '{print $1}')
ROSTER_SHA_BEFORE=$(sha256sum "$WORK/.basalt/instance.json" | awk '{print $1}')
FIRST_WRITE_BEFORE=$(q "SELECT coalesce(max(first_write_at)::text, 'NULL') FROM $SCHEMA.storage_cutover")
printf '演练读数：first_write_at（切换前）=%s\n' "$FIRST_WRITE_BEFORE"
expect_eq "切换前 first_write_at" NULL "$FIRST_WRITE_BEFORE"

printf '%s\n' '===== ⑧ 切换（db switch --workspace，四道门禁含源端重盘） ====='
step switch db switch --dump "$TMP/pkg" --workspace "$WORK/.basalt"
expect_contains switch "回退窗口：开放（first_write_at 仍为 NULL）" "首次切换必须留下开放的回退窗口"
expect_contains switch "源端重盘：已执行（源工作区与清单一致）" "给出 --workspace 时必须执行第四道门禁"
expect_contains switch "交接就绪（handoff）：**未核对**" "switch 不得声称跑过 handoff 门禁"
expect_contains switch "源 schema 版本：v6" "切换记录的源谱系必须是 v6"
CUTOVER_ID=$(field switch "切换记录：")
SWITCH_MANIFEST=$(field switch "绑定清单 SHA-256：")
CUTOVER_MANIFEST=$(q "SELECT manifest_sha256 FROM $SCHEMA.storage_cutover")
expect_eq "storage_cutover.manifest_sha256 与报告一致" "$SWITCH_MANIFEST" "$CUTOVER_MANIFEST"
CUTOVER_ROWS=$(q "SELECT count(*) FROM $SCHEMA.storage_cutover")
expect_eq "切换后 storage_cutover 行数" 1 "$CUTOVER_ROWS"
FIRST_WRITE_AFTER=$(q "SELECT coalesce(max(first_write_at)::text, 'NULL') FROM $SCHEMA.storage_cutover")
printf '演练读数：%s / first_write_at（切换后）=%s\n' "$CUTOVER_ID" "$FIRST_WRITE_AFTER"
expect_eq "切换后 first_write_at" NULL "$FIRST_WRITE_AFTER"

printf '%s\n' '===== ⑨ 三维回退判据的本次读数 ====='
DB_TARGET_OK=$(q "SELECT count(*) FROM $SCHEMA.management_operations")
expect_eq "数据库维：切换不得改写业务行（management_operations）" "$MGMT_AFTER_IMPORT" "$DB_TARGET_OK"
NOTIFY_ROWS=$(q "SELECT count(*) FROM $SCHEMA.notify_deliveries")
TOOL_ROWS=$(q "SELECT count(*) FROM $SCHEMA.tool_invocations")
LLM_ROWS=$(q "SELECT count(*) FROM $SCHEMA.llm_calls")
expect_eq "外部维：notify_deliveries" 0 "$NOTIFY_ROWS"
expect_eq "外部维：tool_invocations" 0 "$TOOL_ROWS"
expect_eq "外部维：llm_calls" 0 "$LLM_ROWS"
printf '演练读数：数据库维 management_operations=%s（与导入基线一致）/ 外部维 notify=%s tool=%s llm=%s（演练未启动 serve，零外呼）\n' \
    "$DB_TARGET_OK" "$NOTIFY_ROWS" "$TOOL_ROWS" "$LLM_ROWS"

# 文件维基线取“切换时刻之后”：切换本身只读工作区（--workspace 只做资产重盘），
# 之后的任何变化都属于切换后窗口。
WS_SNAPSHOT_AT_CUTOVER=$(snapshot "$WORK/.basalt")

printf '%s\n' '===== ⑩ 切换后复核目标库（数据库维仍然干净） ====='
step verify-target-after-switch db verify --dump "$TMP/pkg" --target
expect_contains verify-target-after-switch "结论：通过（" "切换后目标库仍须与清单一致"
if grep -qF "storage_cutover" "$TMP/pkg/manifest.json"; then
    fail "storage_cutover 不应出现在迁移清单中（它是 PG 侧切换记录，不是源表）"
fi

printf '%s\n' '===== ⑪ 失败注入：同一批次二次切换必须被拒 ====='
step_must_fail switch-second db switch --dump "$TMP/pkg" --workspace "$WORK/.basalt"
expect_contains switch-second "同一导出批次已切换过" "重复切换必须命中唯一索引并拒绝"
CUTOVER_ROWS_AFTER=$(q "SELECT count(*) FROM $SCHEMA.storage_cutover")
expect_eq "二次切换被拒后 storage_cutover 行数" 1 "$CUTOVER_ROWS_AFTER"

printf '%s\n' '===== ⑫ 回退（演练环境：丢弃目标库；源端一字未改） ====='
SRC_DB_SHA_AFTER=$(sha256sum "$WORK/.basalt/basalt.db" | awk '{print $1}')
ROSTER_SHA_AFTER=$(sha256sum "$WORK/.basalt/instance.json" | awk '{print $1}')
WS_SNAPSHOT_BEFORE_ROLLBACK=$(snapshot "$WORK/.basalt")
expect_eq "文件维：切换—回退窗口内工作区文件快照" "$WS_SNAPSHOT_AT_CUTOVER" "$WS_SNAPSHOT_BEFORE_ROLLBACK"
expect_eq "回退后源库字节未变" "$SRC_DB_SHA_BEFORE" "$SRC_DB_SHA_AFTER"
expect_eq "回退后名册字节未变" "$ROSTER_SHA_BEFORE" "$ROSTER_SHA_AFTER"
q "DROP SCHEMA $SCHEMA CASCADE" >/dev/null
SCHEMA_CREATED=0
SCHEMA_LEFT=$(q "SELECT count(*) FROM pg_namespace WHERE nspname = '$SCHEMA'")
expect_eq "回退后演练 schema 已丢弃" 0 "$SCHEMA_LEFT"

printf '%s\n' '===== 演练结论 ====='
printf '源快照 SHA-256：%s\n' "$EXPORT_SNAPSHOT"
printf '绑定清单 SHA-256：%s\n' "$SWITCH_MANIFEST"
printf '切换记录：%s\n' "$CUTOVER_ID"
printf 'first_write_at：切换前=%s / 切换后=%s（回退窗口开放）\n' "$FIRST_WRITE_BEFORE" "$FIRST_WRITE_AFTER"
printf '（a）导出条目：%s\n' "$(field export "资产条目：")"
printf '失败注入点：同一导出批次二次切换 → 退出码 %s，拒绝文案「同一导出批次已切换过」，行数仍为 %s\n' \
    "$EXPECTED_FAIL_CODE" "$CUTOVER_ROWS_AFTER"
printf '%s\n' '三维判据：数据库维=干净（目标库与清单一致、业务行与导入基线相同）/ 文件维=干净（工作区快照未变）/ 外部维=干净（零外呼、零投递、零工具调用）'
printf '%s\n' '前滚分支：未由产品路径产生——MarkFirstWrite 当前无生产调用方，first_write_at 非空只能由 TestMarkFirstWrite_* 与 TestRollback_AfterFirstWriteForwardOnly 覆盖；本演练只断言开窗（first_write_at IS NULL）。'
printf '%s\n' '回退编排：AssessRollback 尚未由 CLI 暴露，本演练的回退动作取「丢弃目标库 + 证明源端未变」，行保留语义（storage_cutover 不改写历史行）由 TestRollback_* 覆盖。'
printf '%s\n' '演练通过。'
