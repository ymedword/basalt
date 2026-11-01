#!/bin/sh
# us16-handoff-drill.sh —— US-16 主密钥交接隔离演练（T079）。
#
# 覆盖颗粒度文档 §4.3 的 `us16-handoff-drill` 判据：`(a)` / `(b)` 计数、`first_write_at`
# 前后值、密钥指纹与密文计数、不可达密钥路径的 K1 / K2 取钥、启动验证窗口的三维判据，
# 以及 `instance adopt` 的零外呼。**产物只留非敏感摘要**——明文、密钥字节、nonce 与密文
# 一律不出现在任何一行输出里。
#
# 运行环境是**硬性前置**，缺一即失败、不跳过：
#   - `BASALT_TEST_PG_DSN`：只由 dev-env/scripts/with-postgres.sh 注入，本脚本从不回显；
#   - `psql` / `go` / `sha256sum` / `curl`：Windows Git Bash 没有 psql，本演练**必须**
#     经 with-postgres.sh 在 WSL 内运行——同一 stage 的 us16-e2e 也在同一环境里临时
#     构建二进制，两处 `binary_sha256` 因此才可能一致（runner 要求 process 级 check
#     跨 check 同哈希）。
#
# 夹具由 `scripts/_fixtures/us16fixture` 生成：产品 CLI 造不出"带托管密文的 SQLite
# 源实例"（`serve` 只连 PG，`instance adopt` 案例 4 只写名册），没有真夹具这条链路
# 就只能"看起来跑过"。
#
# 本演练**只**在临时工作区与临时 schema 上动作：不碰既有实例、不对真实服务发请求；
# 结束（无论成败）都结束窗口进程、删临时目录并 DROP 演练 schema。
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
command -v curl >/dev/null 2>&1 || fail "找不到 curl（验证窗口的 ready 面探针需要它）"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/us16-handoff-drill.XXXXXX")
LOG_DIR="$TMP/logs"
mkdir -p "$LOG_DIR"
WINDOW_PID=""

# schema 名只允许 [a-z0-9_]：它会被直接拼进 DDL / 查询，先自证字符集再使用。
SCHEMA="us16_drill_handoff_$(date -u +%Y%m%d%H%M%S)_$$"
case "$SCHEMA" in
    *[!a-z0-9_]*) fail "内部错误：演练 schema 名含非法字符 $SCHEMA" ;;
esac
SCHEMA_CREATED=0

cleanup() {
    status=$?
    if [ -n "$WINDOW_PID" ]; then
        kill -TERM "$WINDOW_PID" 2>/dev/null || true
        wait "$WINDOW_PID" 2>/dev/null || true
        WINDOW_PID=""
    fi
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

mkdir -p "$TMP/bin"
FIXTURE="$TMP/bin/us16fixture"
if ! (cd "$ROOT" && CGO_ENABLED=0 go build -trimpath -o "$FIXTURE" ./scripts/_fixtures/us16fixture) \
    >"$LOG_DIR/fixture-build.log" 2>&1; then
    cat "$LOG_DIR/fixture-build.log" >&2
    fail "构建夹具生成器失败（scripts/_fixtures/us16fixture）"
fi

# ── 判卷助手 ──────────────────────────────────────────────────────────────────
q() {
    psql -X -q -A -t -v ON_ERROR_STOP=1 -d "$BASALT_TEST_PG_DSN" -c "$1" || {
        printf 'drill: psql 查询失败：%s\n' "$1" >&2
        exit 1
    }
}

# run_in <dir> <name> <args...>：在指定工作目录跑一条产品命令，输出进日志并回显。
run_in() {
    dir=$1
    name=$2
    shift 2
    log="$LOG_DIR/$name.log"
    if ! (cd "$dir" && "$BIN" "$@") >"$log" 2>&1; then
        printf 'drill: %s 失败（退出码非 0），完整输出如下：\n' "$name" >&2
        cat "$log" >&2
        exit 1
    fi
    printf '%s\n' "----- $name -----"
    cat "$log"
}

run_must_fail() {
    dir=$1
    name=$2
    shift 2
    log="$LOG_DIR/$name.log"
    set +e
    (cd "$dir" && "$BIN" "$@") >"$log" 2>&1
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

expect_absent() {
    log="$LOG_DIR/$1.log"
    needle=$2
    what=$3
    if grep -qF -- "$needle" "$log"; then
        printf 'drill: %s 出现了不该有的内容「%s」（日志 %s）\n' "$what" "$needle" "$log" >&2
        exit 1
    fi
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

# fixture_field <变量名> <fixture 输出>：从 `key=value` 摘要里取一项。
fixture_field() {
    printf '%s\n' "$2" | sed -n "s/^$1=//p" | head -n 1
}

# 工作区逐文件快照：路径与内容一起进摘要，任何新增 / 删除 / 改写都会变。
#
# `basalt.lock` 不在判据内：它是**取实例锁本身**留下的运行期文件（每个离线命令都必须先
# 取锁才有资格拒绝），不属于实例数据。它进摘要会让"拒绝时零写入"变成"必须先取过锁"，
# 判的就不再是数据。库 / 名册 / 配置 / 密钥等实例数据仍在摘要里。
# basalt.lock is excluded: taking the instance lock creates it, and it is not instance data.
snapshot() {
    (cd "$1" && find . -type f ! -name 'basalt.lock' -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) |
        sha256sum | awk '{print $1}'
}

# copy_legacy_copy <源端根> <副本根>：按 §2.12.0b 的副本形态逐字节复制。
copy_legacy_copy() {
    mkdir -p "$2/.basalt"
    cp "$1/.basalt/basalt.db" "$2/.basalt/basalt.db"
    cp "$1/basalt.yaml" "$2/basalt.yaml"
}

# run_window <dir> <name> <extra-args...>：后台起一次验证窗口，回填 WINDOW_ADDR。
# 调用方负责收尾（stop_window）。
run_window() {
    dir=$1
    name=$2
    shift 2
    log="$LOG_DIR/$name.log"
    (cd "$dir" && exec "$BIN" serve --verification-window "$@") >"$log" 2>&1 &
    WINDOW_PID=$!
    waited=0
    WINDOW_ADDR=""
    while [ "$waited" -lt 60 ]; do
        if ! kill -0 "$WINDOW_PID" 2>/dev/null; then
            printf 'drill: %s 的窗口进程提前退出，完整输出如下：\n' "$name" >&2
            cat "$log" >&2
            exit 1
        fi
        WINDOW_ADDR=$(sed -n 's/.* addr=\([^ ]*\).*/\1/p' "$log" | head -n 1)
        if [ -n "$WINDOW_ADDR" ]; then
            return 0
        fi
        sleep 0.5
        waited=$((waited + 1))
    done
    printf 'drill: %s 在 30s 内没有报出监听地址，完整输出如下：\n' "$name" >&2
    cat "$log" >&2
    exit 1
}

# stop_window <name>：SIGTERM 结束窗口并回收退出码（正常停止必须为 0）。
stop_window() {
    name=$1
    set +e
    kill -TERM "$WINDOW_PID" 2>/dev/null
    wait "$WINDOW_PID"
    code=$?
    set -e
    WINDOW_PID=""
    printf '%s\n' "----- $name（窗口停止，退出码 $code） -----"
    cat "$LOG_DIR/$name.log"
    expect_eq "$name 正常停止的退出码" 0 "$code"
}

# ═══════════════════════════════════════════════════════════════════════════════
# 第一部分：迁移准备副本的交接校验 → 切换 → 启动验证窗口
# ═══════════════════════════════════════════════════════════════════════════════
COPY="$TMP/copy"
printf '%s\n' '===== ① 造迁移准备副本夹具（临时工作区 + **测试密钥**） ====='
FIXTURE_OUT=$("$FIXTURE" handoff-copy --root "$COPY") || fail "生成迁移准备副本失败"
printf '%s\n' "$FIXTURE_OUT"
COPY_KEY_FILE=$(fixture_field key_file "$FIXTURE_OUT")
COPY_KEY_ID=$(fixture_field key_id "$FIXTURE_OUT")
COPY_FINGERPRINT=$(fixture_field fingerprint "$FIXTURE_OUT")
COPY_KEY_SHA=$(fixture_field key_file_sha256 "$FIXTURE_OUT")
COPY_CONFIG="$COPY/basalt.yaml"
COPY_WS="$COPY/.basalt"
[ -f "$COPY_KEY_FILE" ] || fail "夹具未产出主密钥文件：$COPY_KEY_FILE"

# 原密钥与配置先留一份：③④⑤ 会就地改掉其中一个，⑥ 要用它还原。
mkdir -p "$TMP/keyring"
cp "$COPY_KEY_FILE" "$TMP/keyring/original.key"
chmod 600 "$TMP/keyring/original.key"
cp "$COPY_CONFIG" "$TMP/keyring/original.yaml"

# 运行期库连接：DSN 只经环境变量进入子进程，不落盘明文、不回显。交接校验本身不需要
# 它，但配置装载会因占位符未展开而告警——先备好，让③～⑥ 的读数只剩被注入的那一条差异。
case "$BASALT_TEST_PG_DSN" in
    *\?*) export BASALT_DRILL_DSN="$BASALT_TEST_PG_DSN&search_path=$SCHEMA,public" ;;
    *) export BASALT_DRILL_DSN="$BASALT_TEST_PG_DSN?search_path=$SCHEMA,public" ;;
esac

printf '%s\n' '===== ② 交接校验通过：(a) / (b) 计数、密钥指纹与密文计数 ====='
run_in "$COPY" handoff-ready db verify --handoff "$COPY_WS"
expect_contains handoff-ready "结论：交接就绪" "健康副本必须交接就绪"
expect_contains handoff-ready "历史密文：检查 1 条，失败 0 条" "(a) 历史密文必须逐条可解"
expect_contains handoff-ready "当前可用性：检查 1 条，失败 0 条" "(b) 当前可用性必须逐条通过"
expect_contains handoff-ready "目标配置核对：已核对" "迁移准备副本必须核对配置基线"
expect_contains handoff-ready "路径已持久化：是" "交接要求密钥路径已持久化"
RECORD_LINE=$(field handoff-ready "主密钥记录：")
KEY_LINE=$(field handoff-ready "密钥核对：")
expect_eq "记录指纹与夹具一致" "key_id=$COPY_KEY_ID 指纹=$COPY_FINGERPRINT" "$RECORD_LINE"
expect_eq "密钥核对与记录一致" "key_id=$COPY_KEY_ID 指纹=$COPY_FINGERPRINT" "$KEY_LINE"
printf '演练读数：(a) 1/0、(b) 1/0、密文 1 条、密钥指纹 %s\n' "$COPY_FINGERPRINT"

printf '%s\n' '===== ③ 失败注入：密钥路径不可达 → recovery_required ====='
sed -i "s|^  master_key_file: .*|  master_key_file: '$TMP/unreachable-host/master.key'|" "$COPY_CONFIG"
run_must_fail "$COPY" handoff-key-unreachable db verify --handoff "$COPY_WS"
expect_contains handoff-key-unreachable "结论：**不就绪**（recovery_required" \
    "密钥不可达必须落在 recovery_required"
expect_contains handoff-key-unreachable "主密钥文件不可用" "原因必须指出密钥文件不可用"
expect_contains handoff-key-unreachable "恢复指引：" "每个拒绝都要给恢复指引"
KEY_UNREACHABLE_CODE=$EXPECTED_FAIL_CODE
cp "$TMP/keyring/original.yaml" "$COPY_CONFIG"

printf '%s\n' '===== ④ 失败注入：采纳之后配置又被改过 → instance_identity_mismatch ====='
# 只加一行注释：密钥与名册都不动，报出来的必须正是"配置漂移"，而不是被密钥门先挡住。
printf '\n# drill: 配置在采纳之后又被改动\n' >>"$COPY_CONFIG"
run_must_fail "$COPY" handoff-config-edited db verify --handoff "$COPY_WS"
expect_contains handoff-config-edited "结论：**不就绪**（instance_identity_mismatch" \
    "配置漂移必须落在 instance_identity_mismatch"
expect_contains handoff-config-edited "采纳之后配置又被改过" "原因必须点名配置漂移"
expect_contains handoff-config-edited "目标配置核对：**未完成**" "两侧哈希都要落报告，运维才知道是哪一方变了"
CONFIG_EDITED_CODE=$EXPECTED_FAIL_CODE
cp "$TMP/keyring/original.yaml" "$COPY_CONFIG"
chmod 600 "$COPY_CONFIG"

printf '%s\n' '===== ⑤ 失败注入：密钥不是记录那一把 → instance_identity_mismatch ====='
# 配置字节保持不变，只把**同一路径上的文件**换成另一把钥匙：配置基线仍对得上，
# 报出来的必须正是"密钥与记录不符"。
"$FIXTURE" handoff-copy --root "$TMP/other-key/root" >/dev/null || fail "生成对照密钥失败"
cp "$TMP/other-key/root/private/master.key" "$COPY_KEY_FILE"
run_must_fail "$COPY" handoff-key-mismatch db verify --handoff "$COPY_WS"
expect_contains handoff-key-mismatch "结论：**不就绪**（instance_identity_mismatch" \
    "密钥与记录不符必须落在 instance_identity_mismatch"
expect_contains handoff-key-mismatch "主密钥与记录不符" "原因必须点名密钥与记录不符"
KEY_MISMATCH_CODE=$EXPECTED_FAIL_CODE

printf '%s\n' '===== ⑥ 还原原密钥与配置：交接重新就绪（拒绝不是一次性判定） ====='
cp "$TMP/keyring/original.key" "$COPY_KEY_FILE"
chmod 600 "$COPY_KEY_FILE"
run_in "$COPY" handoff-restored db verify --handoff "$COPY_WS"
expect_contains handoff-restored "结论：交接就绪" "还原原密钥与配置后必须重新就绪"
expect_eq "还原后的密钥指纹" "key_id=$COPY_KEY_ID 指纹=$COPY_FINGERPRINT" \
    "$(field handoff-restored "密钥核对：")"

printf '%s\n' "演练 schema：$SCHEMA（结束即 DROP）"
q "CREATE SCHEMA $SCHEMA" >/dev/null
SCHEMA_CREATED=1

printf '%s\n' '===== ⑦ 切换前 first_write_at 基线 ====='
CUTOVER_EXISTS=$(q "SELECT count(*) FROM information_schema.tables WHERE table_schema = '$SCHEMA' AND table_name = 'storage_cutover'")
expect_eq "导入前 storage_cutover 尚未建表" 0 "$CUTOVER_EXISTS"

printf '%s\n' '===== ⑧ 导出 → 导入空目标库（交接副本走完迁移链路） ====='
run_in "$COPY" export db export --out "$TMP/pkg"
expect_contains export "未归类 0 条" "交接副本不得有未归类文件"
expect_contains export "核查状态：not_required（可交付）" "无未归类文件时应为可交付包"
EXPORT_ENTRIES=$(field export "资产条目：")
run_in "$COPY" import db import --dump "$TMP/pkg"
expect_contains import "合计 " "导入必须报出合计行数"
FIRST_WRITE_BEFORE=$(q "SELECT coalesce(max(first_write_at)::text, 'NULL') FROM $SCHEMA.storage_cutover")
printf '演练读数：first_write_at（切换前）=%s\n' "$FIRST_WRITE_BEFORE"
expect_eq "切换前 first_write_at" NULL "$FIRST_WRITE_BEFORE"

printf '%s\n' '===== ⑨ 切换（db switch --workspace）与回退窗口 ====='
run_in "$COPY" switch db switch --dump "$TMP/pkg" --workspace "$COPY_WS"
expect_contains switch "回退窗口：开放（first_write_at 仍为 NULL）" "首次切换必须留下开放的回退窗口"
expect_contains switch "源端重盘：已执行（源工作区与清单一致）" "给出 --workspace 时必须执行第四道门禁"
expect_contains switch "交接就绪（handoff）：**未核对**" "switch 不得声称跑过 handoff 门禁"
SWITCH_MANIFEST=$(field switch "绑定清单 SHA-256：")
CUTOVER_MANIFEST=$(q "SELECT manifest_sha256 FROM $SCHEMA.storage_cutover")
expect_eq "storage_cutover.manifest_sha256 与报告一致" "$SWITCH_MANIFEST" "$CUTOVER_MANIFEST"
FIRST_WRITE_AFTER=$(q "SELECT coalesce(max(first_write_at)::text, 'NULL') FROM $SCHEMA.storage_cutover")
printf '演练读数：first_write_at（切换后）=%s\n' "$FIRST_WRITE_AFTER"
expect_eq "切换后 first_write_at" NULL "$FIRST_WRITE_AFTER"

printf '%s\n' '===== ⑩ 切换后再核交接：证据不因切换而失效 ====='
run_in "$COPY" handoff-after-switch db verify --handoff "$COPY_WS"
expect_contains handoff-after-switch "结论：交接就绪" "切换后交接证据仍必须成立"
expect_contains handoff-after-switch "历史密文：检查 1 条，失败 0 条" "切换不得让 (a) 失效"
expect_contains handoff-after-switch "当前可用性：检查 1 条，失败 0 条" "切换不得让 (b) 失效"

printf '%s\n' '===== ⑪ 启动验证窗口：三维判据 allowed ====='
run_window "$COPY" window-allowed
READY_BODY=$(curl -sS --max-time 10 "http://$WINDOW_ADDR/api/v1/ready") || fail "ready 面不可达（$WINDOW_ADDR）"
printf 'ready 响应：%s\n' "$READY_BODY"
printf '%s\n' "$READY_BODY" | grep -qF '"verification_window":true' ||
    fail "ready 响应必须声明自己在验证窗口姿态下"
printf '%s\n' "$READY_BODY" | grep -qF '"status":"ready"' || fail "ready 响应必须报 ready"
HEALTH_BODY=$(curl -sS --max-time 10 "http://$WINDOW_ADDR/api/v1/health") || fail "health 面不可达（$WINDOW_ADDR）"
printf 'health 响应：%s\n' "$HEALTH_BODY"
printf '%s\n' "$HEALTH_BODY" | grep -qF '"status":"ok"' || fail "health 响应必须报 ok"
stop_window window-allowed
expect_contains window-allowed "验证窗口：serve 已在验证姿态下正常停止" "窗口必须报告正常停止"
expect_contains window-allowed "decision=allowed" "三维都干净时结论必须是 allowed"
WINDOW_ALLOWED_ADDR=$WINDOW_ADDR
printf '演练读数：窗口地址 %s / decision=allowed\n' "$WINDOW_ALLOWED_ADDR"

printf '%s\n' '===== ⑫ 启动验证窗口：窗口内改工作区文件 → blocked:workspace_changed ====='
run_window "$COPY" window-workspace-changed
printf '窗口内改动\n' >"$COPY_WS/drill-window-change.txt"
stop_window window-workspace-changed
expect_contains window-workspace-changed "decision=blocked:workspace_changed" \
    "窗口内出现文件变化时结论必须是 blocked:workspace_changed"
expect_absent window-workspace-changed "decision=allowed" "工作区已变的窗口不得给出 allowed"
rm -f "$COPY_WS/drill-window-change.txt"

printf '%s\n' '===== ⑬ 资产维度：给出导出包时逐项重盘 ====='
run_window "$COPY" window-assets "--asset-package=$TMP/pkg"
stop_window window-assets
expect_contains window-assets "decision=allowed" "资产一致时结论必须是 allowed"
expect_contains window-assets "assets_checked=true" "给出 --asset-package 时必须真的核对过资产"
# 期望值取目标库实测，不写死字面量：PG 谱系每升一版（US-16 起 v2，D-19 的 `0002`
# 迁移）都写死会让演练与契约脱钩——窗口必须报出**目标库当前的** schema 版本。
WINDOW_EXPECTED_SCHEMA=$(q "SELECT max(version) FROM $SCHEMA.schema_version")
expect_eq "窗口内的 schema 版本" "$WINDOW_EXPECTED_SCHEMA" "$(sed -n 's/.* schema_version=\([0-9]*\).*/\1/p' "$LOG_DIR/window-assets.log" | head -n 1)"

printf '%s\n' '===== ⑭ 失败注入：工作区多出资产清单外文件 → assets_mismatch ====='
printf 'drill: 清单外的文件\n' >"$COPY_WS/drill-extra-asset.txt"
run_must_fail "$COPY" window-assets-mismatch serve --verification-window "--asset-package=$TMP/pkg"
expect_contains window-assets-mismatch "assets_mismatch" "清单外的文件必须让离线门拒绝"
expect_contains window-assets-mismatch "listener_opened=false" "离线门失败时端口不得打开"
expect_absent window-assets-mismatch "health / ready 已就绪" "被拒绝的窗口不得报出监听就绪"
ASSETS_MISMATCH_CODE=$EXPECTED_FAIL_CODE
rm -f "$COPY_WS/drill-extra-asset.txt"

# ═══════════════════════════════════════════════════════════════════════════════
# 第二部分：取钥三档（K1 / K2 / 配置路径）与 `instance adopt` 零外呼
#
# 三个源端都用 `legacy-source` 造：schema 5 的真库 + 一条托管凭据。区别只在
# `secrets.master_key_file` 这一行的形态——那正是副本主机上"读不读得到原密钥"的全部差异。
# ═══════════════════════════════════════════════════════════════════════════════
# provision_legacy <标签> <key-mode>：造源端 + 备份 + 副本，回填 SRC/BAK/CP/MANIFEST。
provision_legacy() {
    label=$1
    mode=$2
    SRC="$TMP/legacy-$label/src"
    BAK="$TMP/legacy-$label/bak"
    CP="$TMP/legacy-$label/copy"
    legacy_out=$("$FIXTURE" legacy-source --root "$SRC" --schema 5 --key-mode "$mode") ||
        fail "生成 $label 源端失败"
    printf '源端（%s）：%s\n' "$mode" "$(printf '%s\n' "$legacy_out" | tr '\n' ' ')"
    backup_out=$("$FIXTURE" backup --source "$SRC" --out "$BAK") || fail "生成 $label 备份失败"
    MANIFEST=$(fixture_field manifest_sha256 "$backup_out")
    [ -n "$MANIFEST" ] || fail "$label 备份没有报出清单哈希"
    copy_legacy_copy "$SRC" "$CP"
    SRC_KEY_SHA=$(fixture_field key_file_sha256 "$legacy_out")
    SRC_KEY_FILE=$(fixture_field key_file "$legacy_out")
}

# adopt_legacy <副本根> <名字> <额外参数...>：以当前源端 / 备份为基准跑一次受控升级。
adopt_legacy() {
    cp_root=$1
    name=$2
    shift 2
    (cd "$cp_root" && "$BIN" secrets adopt-legacy \
        --workspace "$cp_root/.basalt" --backup "$BAK" \
        --expect-backup-sha256 "$MANIFEST" --source "$SRC/.basalt" --confirm "$@")
}

adopt_legacy_must_fail() {
    cp_root=$1
    name=$2
    shift 2
    log="$LOG_DIR/$name.log"
    set +e
    adopt_legacy "$cp_root" "$name" "$@" >"$log" 2>&1
    code=$?
    set -e
    [ "$code" -ne 0 ] || fail "$name 本应失败但退出码为 0"
    printf '%s\n' "----- $name（预期失败，退出码 $code） -----"
    cat "$log"
    EXPECTED_FAIL_CODE=$code
}

printf '%s\n' '===== ⑮ K1 形态：配置没有密钥路径 → master_key_path_unpersisted ====='
# K1（备份内副本）在取钥阶梯里排在"配置无路径"之后，但掩码门**先于**取钥拒绝：
# 算不出 `config_without_key_path_sha256` 就无法证明后续受控调整是否越界。因此这条
# 演练证明的是"K1 收钥档在当前实现里不可达"，而不是它取到了钥。
provision_legacy absent absent
cp_before=$(snapshot "$CP/.basalt")
adopt_legacy_must_fail "$CP" adopt-legacy-no-key-path
expect_contains adopt-legacy-no-key-path "master_key_path_unpersisted" \
    "配置缺密钥路径必须在掩码门就被拒"
expect_contains adopt-legacy-no-key-path "没有 secrets.master_key_file" "原因必须点名缺失的配置键"
K1_UNREACHABLE_CODE=$EXPECTED_FAIL_CODE
expect_eq "K1 形态被拒后副本工作区零改动" "$cp_before" "$(snapshot "$CP/.basalt")"
printf '演练读数：K1（备份内副本）取钥档 = 不可达（掩码门先拒），退出码 %s\n' "$K1_UNREACHABLE_CODE"

printf '%s\n' '===== ⑯ K2 形态：配置路径不可达且无覆盖 → key_unavailable ====='
provision_legacy unreachable unreachable
cp_before=$(snapshot "$CP/.basalt")
adopt_legacy_must_fail "$CP" adopt-legacy-key-unavailable
expect_contains adopt-legacy-key-unavailable "key_unavailable" "读不到配置所指密钥必须落在 key_unavailable"
expect_contains adopt-legacy-key-unavailable "读取原密钥" "原因必须点名取钥动作"
expect_contains adopt-legacy-key-unavailable "--master-key-file" "拒绝必须给出受控读取的下一步"
KEY_UNAVAILABLE_CODE=$EXPECTED_FAIL_CODE
expect_eq "K2 无覆盖被拒后副本工作区零改动" "$cp_before" "$(snapshot "$CP/.basalt")"

printf '%s\n' '===== ⑰ K2 形态：覆盖取钥没有锚点 → key_unverified ====='
adopt_legacy_must_fail "$CP" adopt-legacy-key-no-anchor --master-key-file "$SRC_KEY_FILE"
expect_contains adopt-legacy-key-no-anchor "key_unverified" "备份外取钥缺锚点必须落在 key_unverified"
expect_contains adopt-legacy-key-no-anchor "必须带 --expect-key-sha256" "原因必须点名缺失的外部锚点"
KEY_NO_ANCHOR_CODE=$EXPECTED_FAIL_CODE
expect_eq "K2 缺锚点被拒后副本工作区零改动" "$cp_before" "$(snapshot "$CP/.basalt")"

printf '%s\n' '===== ⑱ K2 形态：覆盖取钥锚点不符 → key_unverified ====='
adopt_legacy_must_fail "$CP" adopt-legacy-key-wrong-anchor \
    --master-key-file "$SRC_KEY_FILE" --expect-key-sha256 "$(printf '0%.0s' 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24 25 26 27 28 29 30 31 32 33 34 35 36 37 38 39 40 41 42 43 44 45 46 47 48 49 50 51 52 53 54 55 56 57 58 59 60 61 62 63 64)"
expect_contains adopt-legacy-key-wrong-anchor "key_unverified" "锚点不符必须落在 key_unverified"
expect_contains adopt-legacy-key-wrong-anchor "与外部锚点不符" "原因必须点名锚点不符"
KEY_WRONG_ANCHOR_CODE=$EXPECTED_FAIL_CODE
expect_eq "K2 错锚点被拒后副本工作区零改动" "$cp_before" "$(snapshot "$CP/.basalt")"

printf '%s\n' '===== ⑲ K2 形态：覆盖取钥锚点相符 → 取钥 source=external_anchor ====='
adopt_legacy "$CP" adopt-legacy-external-anchor \
    --master-key-file "$SRC_KEY_FILE" --expect-key-sha256 "$SRC_KEY_SHA" >"$LOG_DIR/adopt-legacy-external-anchor.log" 2>&1 ||
    { cat "$LOG_DIR/adopt-legacy-external-anchor.log" >&2; fail "带对锚点的覆盖取钥本应成功"; }
printf '%s\n' '----- adopt-legacy-external-anchor -----'
cat "$LOG_DIR/adopt-legacy-external-anchor.log"
expect_contains adopt-legacy-external-anchor "取钥：source=external_anchor" "覆盖取钥必须记成 external_anchor"
expect_contains adopt-legacy-external-anchor "schema：5 → 6" "受控升级必须把副本带到 v6"
expect_contains adopt-legacy-external-anchor "密文：1 条" "迁移后必须逐条复核托管密文"
expect_contains adopt-legacy-external-anchor "名册：存在=false" "源端无名册，升级不写名册"
expect_contains adopt-legacy-external-anchor "配置调整：false" "覆盖取钥不改写配置"

printf '%s\n' '===== ⑳ instance adopt：迁移准备模式补写名册，且零外呼 ====='
provision_legacy local local
adopt_legacy "$CP" adopt-legacy-config-path >"$LOG_DIR/adopt-legacy-config-path.log" 2>&1 ||
    { cat "$LOG_DIR/adopt-legacy-config-path.log" >&2; fail "配置路径可达时受控升级本应成功"; }
printf '%s\n' '----- adopt-legacy-config-path -----'
cat "$LOG_DIR/adopt-legacy-config-path.log"
expect_contains adopt-legacy-config-path "取钥：source=config_path" "配置路径可达时走 config_path 档"
expect_contains adopt-legacy-config-path "schema：5 → 6" "受控升级必须把副本带到 v6"

# 零外呼的可判定形态：进程内不装配任何外呼组件（e2e 的
# TestInstanceAdopt_NoRuntimeSideEffectsOnCopy 是本条的正面实现断言）；演练这一侧能
# 观测到的是"只写名册"与"用户数据根一个文件都没写"。
DATA_ROOT="$TMP/data-root"
mkdir -p "$DATA_ROOT"
export XDG_DATA_HOME="$DATA_ROOT" LOCALAPPDATA="$DATA_ROOT"
copy_config_sha_before=$(sha256sum "$CP/basalt.yaml" | awk '{print $1}')
adopt_out=$(cd "$CP" && "$BIN" instance adopt) || fail "instance adopt 在已升级副本上失败"
printf '%s\n' "$adopt_out"
printf '%s\n' "$adopt_out" >"$LOG_DIR/instance-adopt.log"
# 案件编号以 `instance_adopt.go:337-346` 的判定为准：1 = 有记录有密文，2 = 有记录无密文，
# 3 = 无记录有密文，4 = 无记录无密文。受控升级写下的正是"记录 + 1 条密文"，所以是案例 1。
expect_contains instance-adopt "命中判定：案例 1" "有记录有密文的副本应命中案例 1"
expect_contains instance-adopt "托管密文：1 条（逐条可解 true，不可解 0 条）" "采纳必须逐条验证密文"
expect_contains instance-adopt "写实例记录：false（采纳只写名册）" "采纳只写名册"
expect_contains instance-adopt "名册：" "采纳必须报出名册路径"
expect_contains instance-adopt "目标配置哈希：" "迁移准备副本必须核对配置基线"
[ -f "$CP/.basalt/instance.json" ] || fail "采纳后副本必须出现 instance.json"
expect_eq "采纳不得改写配置" "$copy_config_sha_before" "$(sha256sum "$CP/basalt.yaml" | awk '{print $1}')"
expect_eq "采纳不得写用户数据根" "" "$(find "$DATA_ROOT" -mindepth 1 -print -quit)"
ADOPT_INSTANCE_ID=$(field instance-adopt "实例标识：")
printf '演练读数：采纳写出名册，实例标识 %s，用户数据根保持为空\n' "$ADOPT_INSTANCE_ID"

printf '%s\n' '===== ㉑ 升级 + 采纳后的副本再过一次交接校验 ====='
run_in "$CP" handoff-after-upgrade db verify --handoff "$CP/.basalt"
expect_contains handoff-after-upgrade "结论：交接就绪" "受控升级 + 采纳后的副本必须交接就绪"
expect_contains handoff-after-upgrade "历史密文：检查 1 条，失败 0 条" "(a) 必须成立"
expect_contains handoff-after-upgrade "当前可用性：检查 1 条，失败 0 条" "(b) 必须成立"
expect_contains handoff-after-upgrade "目标配置核对：已核对" "采纳审计必须记下目标配置哈希"

printf '%s\n' '===== 演练结论 ====='
printf '副本导出条目：%s\n' "$EXPORT_ENTRIES"
printf '绑定清单 SHA-256：%s\n' "$SWITCH_MANIFEST"
printf 'first_write_at：切换前=%s / 切换后=%s（回退窗口开放）\n' "$FIRST_WRITE_BEFORE" "$FIRST_WRITE_AFTER"
printf '(a) 历史密文：1 条 / 失败 0 条；(b) 当前可用性：1 条 / 失败 0 条\n'
printf '密钥指纹：%s（key_id=%s）\n' "$COPY_FINGERPRINT" "$COPY_KEY_ID"
printf '失败注入点：密钥不可达 → 退出码 %s（recovery_required）/ 密钥不符 → %s（instance_identity_mismatch）/ 配置漂移 → %s（instance_identity_mismatch）\n' \
    "$KEY_UNREACHABLE_CODE" "$KEY_MISMATCH_CODE" "$CONFIG_EDITED_CODE"
printf '启动验证窗口：allowed / blocked:workspace_changed / 资产一致 allowed / 资产不符 → 退出码 %s（assets_mismatch）\n' \
    "$ASSETS_MISMATCH_CODE"
printf '取钥阶梯：K1（备份内副本）=不可达（掩码门先拒，退出码 %s）/ K2 无覆盖 → %s（key_unavailable）/ K2 无锚点 → %s / K2 错锚点 → %s / K2 对锚点 → external_anchor\n' \
    "$K1_UNREACHABLE_CODE" "$KEY_UNAVAILABLE_CODE" "$KEY_NO_ANCHOR_CODE" "$KEY_WRONG_ANCHOR_CODE"
printf '%s\n' '未由 CLI 产生的分支：'
printf '%s\n' '  - 窗口的 blocked:external_action 与 blocked:unknown 无法由命令行产出——本路径不装配能产生外部副作用的组件，'
printf '%s\n' '    "MCP 已启动 / 模型已外呼 / 通知已发出 / 结果未知" 只能由进程内注入取证覆盖，见 TestStartupVerification_RollbackBoundary（四个回退结论）与 TestStartupVerification_OfflineGateBeforeServe（离线门先于监听）。'
printf '%s\n' '  - 窗口监听地址只由装配层给缺省回环值，命令行**没有**地址开关，因此 listener_not_loopback 不在本演练的可达面内，由 TestStartupVerification_OfflineGateBeforeServe 的 non_loopback_address_is_refused 子用例覆盖。'
printf '%s\n' '  - K1（备份内副本）取钥档由 TestSecretsAdoptLegacy_KeyOverrideRequiresProvenance 的 config_without_key_path_is_refused 子用例覆盖其不可达性。'
printf '%s\n' '演练通过。'
