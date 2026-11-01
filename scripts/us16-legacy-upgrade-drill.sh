#!/bin/sh
# us16-legacy-upgrade-drill.sh —— US-16 schema 3～5 受控升级隔离演练（T080）。
#
# 覆盖颗粒度文档 §4.3 的 `us16-legacy-upgrade-drill` 判据：升级副本记录数 / 名册存在性 /
# 源端漂移拒绝（库 + 同级 `basalt.yaml`）/ 配置哈希四值（`source_config_sha256` /
# `backup_config_sha256` / `copy_config_sha256` / `target_config_sha256`）/ 未记录副本
# 配置修改拒绝 / 有引用但越界修改拒绝 / 受控调整后继续 / 无名册顺序（记录 → 名册 →
# 身份 / 配置核对 → 切换 → 离线门 → 验证窗口 `ready`）/ 缺或非法名册时导出与交接校验
# 拒绝。**产物只留非敏感摘要**——明文、密钥字节、nonce 与密文一律不出现在任何一行输出里。
#
# 运行环境是**硬性前置**，缺一即失败、不跳过：
#   - `BASALT_TEST_PG_DSN`：只由 dev-env/scripts/with-postgres.sh 注入，本脚本从不回显；
#   - `psql` / `go` / `sha256sum` / `curl`：Windows Git Bash 没有 psql，本演练**必须**
#     经 with-postgres.sh 在 WSL 内运行——同一 stage 的 us16-e2e 也在同一环境里临时
#     构建二进制，两处 `binary_sha256` 因此才可能一致（runner 要求 process 级 check
#     跨 check 同哈希）。
#
# 夹具由 `scripts/_fixtures/us16fixture` 生成：产品 CLI 造不出 schema 3～5 的存量源端
# （`serve` 只连 PG、`instance adopt` 案例 4 只写名册），没有真夹具这条链路就只能
# "看起来跑过"。`inspect` 子命令只读汇报升级副本的记录数与名册存在性。
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

TMP=$(mktemp -d "${TMPDIR:-/tmp}/us16-legacy-upgrade-drill.XXXXXX")
LOG_DIR="$TMP/logs"
mkdir -p "$LOG_DIR" "$TMP/keyring"
WINDOW_PID=""

# 用户数据根指到临时目录：`instance adopt` / `serve` 都会解析它，演练结束时它必须仍是空的。
DATA_ROOT="$TMP/data-root"
mkdir -p "$DATA_ROOT"
export XDG_DATA_HOME="$DATA_ROOT" LOCALAPPDATA="$DATA_ROOT"

# schema 名只允许 [a-z0-9_]：它会被直接拼进 DDL / 查询，先自证字符集再使用。
SCHEMA="us16_drill_legacy_$(date -u +%Y%m%d%H%M%S)_$$"
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

# 运行期库连接：DSN 只经环境变量进入子进程，不落盘明文、不回显。夹具把配置写成
# `database.dsn: ${BASALT_DRILL_DSN}`，演练里的每个命令都靠它连到演练 schema。
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

# fixture_field <键名> <fixture 输出>：从 `key=value` 摘要里取一项。
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

# 拒绝路径的零写入快照：在 snapshot 基础上再排除 SQLite 的 WAL 旁路文件。
#
# 读 WAL 模式的库时 SQLite 会在同目录**新建** `basalt.db-shm`（状态页）与 0 字节
# `basalt.db-wal`，这是只读打开的协调文件，不是实例数据写入：库字节、表行数、名册都不变。
# 产品自己的零写入用例（internal/app/instance_adopt_test.go requireUnchanged）同样以
# 「名册存在性 + 两张表行数 + 库字节」为判据，不含旁路文件——本函数与那个口径对齐。
# The WAL sidecar files are created by a read-only open itself; they are not data writes.
refusal_snapshot() {
    (cd "$1" && find . -type f ! -name 'basalt.lock' ! -name 'basalt.db-shm' ! -name 'basalt.db-wal' \
        ! -name 'basalt.db-journal' -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) |
        sha256sum | awk '{print $1}'
}

file_sha() {
    sha256sum "$1" | awk '{print $1}'
}

# copy_legacy_copy <源端根> <副本根>：按 §2.12.0b 的副本形态逐字节复制。
copy_legacy_copy() {
    mkdir -p "$2/.basalt"
    cp "$1/.basalt/basalt.db" "$2/.basalt/basalt.db"
    cp "$1/basalt.yaml" "$2/basalt.yaml"
}

# inspect_ws <名字> <工作区>：只读汇报升级副本的 schema / 各表行数 / 名册存在性。
# 结果留在 $INSPECT_OUT，供 inspect_field 取项。
inspect_ws() {
    name=$1
    ws=$2
    INSPECT_OUT=$("$FIXTURE" inspect --workspace "$ws") || fail "inspect $name 失败"
    printf '%s\n' "----- inspect $name -----" "$INSPECT_OUT"
}

inspect_field() {
    printf '%s\n' "$INSPECT_OUT" | sed -n "s/^$1=//p" | head -n 1
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
# 第一部分：schema 5 源端的受控升级主链路
# ═══════════════════════════════════════════════════════════════════════════════

# provision_legacy <标签> <schema> <key-mode>：造源端 + 备份 + 副本，回填
# SRC / BAK / CP / MANIFEST / SRC_CFG_SHA / SRC_KEY_FILE / ADOPT_OUT。
provision_legacy() {
    label=$1
    schema=$2
    mode=$3
    SRC="$TMP/legacy-$label/src"
    BAK="$TMP/legacy-$label/bak"
    CP="$TMP/legacy-$label/copy"
    legacy_out=$("$FIXTURE" legacy-source --root "$SRC" --schema "$schema" --key-mode "$mode") ||
        fail "生成 $label 源端失败"
    printf '%s\n' "----- fixture legacy-source（$label） -----" "$legacy_out"
    SRC_CFG_SHA=$(fixture_field config_sha256 "$legacy_out")
    SRC_DB_SHA=$(fixture_field db_sha256 "$legacy_out")
    SRC_KEY_FILE=$(fixture_field key_file "$legacy_out")
    [ -n "$SRC_CFG_SHA" ] || fail "$label 源端没有报出配置哈希"
    [ -n "$SRC_DB_SHA" ] || fail "$label 源端没有报出库哈希"
    [ -f "$SRC_KEY_FILE" ] || fail "$label 夹具未产出主密钥文件：$SRC_KEY_FILE"
    backup_out=$("$FIXTURE" backup --source "$SRC" --out "$BAK") || fail "生成 $label 备份失败"
    printf '%s\n' "----- fixture backup（$label） -----" "$backup_out"
    MANIFEST=$(fixture_field manifest_sha256 "$backup_out")
    [ -n "$MANIFEST" ] || fail "$label 备份没有报出清单哈希"
    expect_eq "$label 备份自证配置哈希必须等于源端" "$SRC_CFG_SHA" \
        "$(fixture_field config_sha256 "$backup_out")"
    expect_eq "$label 备份里的名册存在性" absent "$(fixture_field roster "$backup_out")"
    copy_legacy_copy "$SRC" "$CP"
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

adopt_legacy_ok() {
    cp_root=$1
    name=$2
    shift 2
    log="$LOG_DIR/$name.log"
    if ! adopt_legacy "$cp_root" "$name" "$@" >"$log" 2>&1; then
        printf 'drill: %s 本应成功，完整输出如下：\n' "$name" >&2
        cat "$log" >&2
        exit 1
    fi
    printf '%s\n' "----- $name -----"
    cat "$log"
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

printf '%s\n' '===== ① 造 schema 5 源端 + 原生备份 + 隔离副本 ====='
provision_legacy main 5 local
SRC_MAIN=$SRC
BAK_MAIN=$BAK
MAN_MAIN=$MANIFEST
CP_MAIN=$CP
SRC_KEY_MAIN=$SRC_KEY_FILE
SRC_CFG_MAIN=$SRC_CFG_SHA
SRC_DB_MAIN=$SRC_DB_SHA
SRC_SNAPSHOT=$(snapshot "$SRC")
printf '演练读数：源端 schema=5、托管凭据 1 条、名册 absent、配置哈希 %s\n' "$SRC_CFG_MAIN"

inspect_ws copy-before-upgrade "$CP_MAIN/.basalt"
expect_eq "升级前副本 schema" 5 "$(inspect_field schema)"
expect_eq "升级前副本 instance_secrets" 0 "$(inspect_field instance_secrets)"
expect_eq "升级前副本 credentials" 1 "$(inspect_field credentials)"
expect_eq "升级前副本名册存在性" absent "$(inspect_field roster)"

printf '%s\n' '===== ② adopt-legacy：schema 5 → 6、写记录、不写名册（配置哈希四值之三） ====='
adopt_legacy_ok "$CP_MAIN" adopt-legacy-main
expect_contains adopt-legacy-main "schema：5 → 6" "受控升级必须把副本带到 v6"
expect_contains adopt-legacy-main "取钥：source=config_path" "配置路径可达时必须走 config_path 档"
expect_contains adopt-legacy-main "密文：1 条（迁移后逐字节复核）" "迁移后必须逐条复核托管密文"
expect_contains adopt-legacy-main "名册：存在=false（本命令不写名册）" "受控升级不写名册"
expect_contains adopt-legacy-main "配置调整：false（覆盖取钥不改写配置）" "受控升级不改写配置"
FOUR_SOURCE=$(field adopt-legacy-main "配置哈希：" | sed -n 's/^source=\([0-9a-f]*\) .*/\1/p')
FOUR_BACKUP=$(field adopt-legacy-main "配置哈希：" | sed -n 's/.* backup=\([0-9a-f]*\) .*/\1/p')
FOUR_COPY=$(field adopt-legacy-main "配置哈希：" | sed -n 's/.* copy=\([0-9a-f]*\) .*/\1/p')
FOUR_WITHOUT=$(field adopt-legacy-main "配置哈希：" | sed -n 's/.* without_key_path=\([0-9a-f]*\)$/\1/p')
expect_eq "四值 source_config_sha256" "$SRC_CFG_MAIN" "$FOUR_SOURCE"
expect_eq "四值 backup_config_sha256" "$SRC_CFG_MAIN" "$FOUR_BACKUP"
expect_eq "四值 copy_config_sha256" "$SRC_CFG_MAIN" "$FOUR_COPY"
case "$FOUR_WITHOUT" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
    *) fail "config_without_key_path_sha256 形态非法：$FOUR_WITHOUT" ;;
esac
[ "${#FOUR_WITHOUT}" -eq 64 ] || fail "config_without_key_path_sha256 长度不是 64：$FOUR_WITHOUT"
[ "$FOUR_WITHOUT" != "$FOUR_SOURCE" ] || fail "掩码摘要与整文件哈希相同——密钥路径没有被掩码，越界判定将失效"
printf '演练读数：配置哈希 source=backup=copy=%s / without_key_path=%s\n' "$FOUR_SOURCE" "$FOUR_WITHOUT"

inspect_ws copy-after-upgrade "$CP_MAIN/.basalt"
expect_eq "升级后副本 schema" 6 "$(inspect_field schema)"
expect_eq "升级后副本 instance_secrets（记录数）" 1 "$(inspect_field instance_secrets)"
expect_eq "升级后副本 credentials" 1 "$(inspect_field credentials)"
expect_eq "升级后副本 credential_versions" 1 "$(inspect_field credential_versions)"
expect_eq "升级后副本 resource_versions" 1 "$(inspect_field resource_versions)"
expect_eq "升级后副本 resource_pointers" 1 "$(inspect_field resource_pointers)"
expect_eq "升级后副本名册存在性" absent "$(inspect_field roster)"
MAIN_RECORDS=$(inspect_field instance_secrets)
MAIN_ROSTER=$(inspect_field roster)
MAIN_CREDENTIAL_VERSIONS=$(inspect_field credential_versions)
expect_eq "受控升级不得改动源端" "$SRC_SNAPSHOT" "$(snapshot "$SRC_MAIN")"

printf '%s\n' '===== ③ 无名册：导出、交接校验、启动验证窗口三处都拒绝 ====='
run_must_fail "$CP_MAIN" export-no-roster db export --out "$TMP/pkg-no-roster"
expect_contains export-no-roster "instance_identity_missing" "无名册的导出必须落在 instance_identity_missing"
expect_contains export-no-roster "不得自动补写名册" "原因必须说明名册只经显式采纳建立"
EXPORT_NO_ROSTER_CODE=$EXPECTED_FAIL_CODE

run_must_fail "$CP_MAIN" handoff-no-roster db verify --handoff "$CP_MAIN/.basalt"
expect_contains handoff-no-roster "结论：**不就绪**（instance_identity_missing" \
    "无名册的交接校验必须落在 instance_identity_missing"
expect_contains handoff-no-roster "实例名册：**缺失**" "报告必须点名名册缺失"
expect_contains handoff-no-roster "没有实例名册" "原因必须说明缺的是名册"
expect_contains handoff-no-roster "恢复指引：" "每个拒绝都要给恢复指引"
HANDOFF_NO_ROSTER_CODE=$EXPECTED_FAIL_CODE

run_must_fail "$CP_MAIN" window-no-roster serve --verification-window
expect_contains window-no-roster "code=instance_identity_missing" "离线门必须在监听之前拒绝"
expect_contains window-no-roster "listener_opened=false" "离线门失败时端口不得打开"
expect_absent window-no-roster "health / ready 已就绪" "被拒绝的窗口不得报出监听就绪"
WINDOW_NO_ROSTER_CODE=$EXPECTED_FAIL_CODE
printf '演练读数：无名册三处拒绝 → 导出 %s / 交接校验 %s / 验证窗口 %s（端口未开）\n' \
    "$EXPORT_NO_ROSTER_CODE" "$HANDOFF_NO_ROSTER_CODE" "$WINDOW_NO_ROSTER_CODE"

printf '%s\n' '===== ④ 顺序第二段：instance adopt 补写名册（记录已在，只写名册） ====='
CP_MAIN_CFG_SHA=$(file_sha "$CP_MAIN/basalt.yaml")
run_in "$CP_MAIN" instance-adopt instance adopt
expect_contains instance-adopt "命中判定：案例 1" "有记录有密文的副本应命中案例 1"
expect_contains instance-adopt "托管密文：1 条（逐条可解 true，不可解 0 条）" "采纳必须逐条验证密文"
expect_contains instance-adopt "写实例记录：false（采纳只写名册）" "采纳只写名册"
expect_contains instance-adopt "目标配置哈希：$CP_MAIN_CFG_SHA（config_adjusted=false）" \
    "未调整副本的目标配置哈希必须等于当前文件哈希"
expect_absent instance-adopt "配置变更引用：" "未调整副本不得出现变更引用"
MAIN_INSTANCE_ID=$(field instance-adopt "实例标识：")
[ -n "$MAIN_INSTANCE_ID" ] || fail "采纳必须报出实例标识"
[ -f "$CP_MAIN/.basalt/instance.json" ] || fail "采纳后副本必须出现 instance.json"
expect_eq "采纳不得改写配置" "$CP_MAIN_CFG_SHA" "$(file_sha "$CP_MAIN/basalt.yaml")"
inspect_ws copy-after-adopt "$CP_MAIN/.basalt"
expect_eq "采纳后副本名册存在性" present "$(inspect_field roster)"
expect_eq "采纳后副本实例标识" "$MAIN_INSTANCE_ID" "$(inspect_field instance_id)"
expect_eq "采纳后副本记录数不变" 1 "$(inspect_field instance_secrets)"
printf '演练读数：采纳写出名册，实例标识 %s，目标配置哈希 %s（config_adjusted=false）\n' \
    "$MAIN_INSTANCE_ID" "$CP_MAIN_CFG_SHA"

printf '%s\n' '===== ⑤ 顺序第三段：身份 / 配置核对（交接就绪） ====='
run_in "$CP_MAIN" handoff-ready db verify --handoff "$CP_MAIN/.basalt"
expect_contains handoff-ready "结论：交接就绪" "记录 + 名册齐备后必须交接就绪"
expect_contains handoff-ready "历史密文：检查 1 条，失败 0 条" "(a) 历史密文必须逐条可解"
expect_contains handoff-ready "当前可用性：检查 1 条，失败 0 条" "(b) 当前可用性必须逐条通过"
expect_contains handoff-ready "目标配置核对：已核对（当前 $CP_MAIN_CFG_SHA == 审计 $CP_MAIN_CFG_SHA）" \
    "目标配置哈希必须与审计一致"
expect_contains handoff-ready "路径已持久化：是" "交接要求密钥路径已持久化"
expect_contains handoff-ready "实例名册：$MAIN_INSTANCE_ID" "报告必须给出采纳写下的实例标识"

printf '%s\n' '===== ⑥ 非法名册：导出与交接校验都拒绝（还原后重新就绪） ====='
cp "$CP_MAIN/.basalt/instance.json" "$TMP/keyring/roster.json"
# 6a：合法 JSON 但 instance_id 形态非法
printf '{"version":1,"instance_id":"bad"}' >"$CP_MAIN/.basalt/instance.json"
run_must_fail "$CP_MAIN" export-bad-roster db export --out "$TMP/pkg-bad-roster"
expect_contains export-bad-roster "instance_id 形态非法" "非法名册必须让导出拒绝"
ROSTER_BAD_EXPORT_CODE=$EXPECTED_FAIL_CODE
run_must_fail "$CP_MAIN" handoff-bad-roster db verify --handoff "$CP_MAIN/.basalt"
expect_contains handoff-bad-roster "结论：**不就绪**（instance_identity_mismatch" \
    "非法名册必须落在 instance_identity_mismatch"
expect_contains handoff-bad-roster "实例名册不可用" "原因必须点名名册不可用"
ROSTER_BAD_HANDOFF_CODE=$EXPECTED_FAIL_CODE
# 6b：根本不是 JSON（解析失败路径）
printf '{not json' >"$CP_MAIN/.basalt/instance.json"
run_must_fail "$CP_MAIN" export-bad-roster-json db export --out "$TMP/pkg-bad-roster-json"
expect_contains export-bad-roster-json "不是合法 JSON" "坏 JSON 名册必须让导出拒绝"
ROSTER_BAD_JSON_CODE=$EXPECTED_FAIL_CODE
run_must_fail "$CP_MAIN" handoff-bad-roster-json db verify --handoff "$CP_MAIN/.basalt"
expect_contains handoff-bad-roster-json "结论：**不就绪**（instance_identity_mismatch" \
    "坏 JSON 名册同样落在 instance_identity_mismatch"
cp "$TMP/keyring/roster.json" "$CP_MAIN/.basalt/instance.json"
run_in "$CP_MAIN" handoff-roster-restored db verify --handoff "$CP_MAIN/.basalt"
expect_contains handoff-roster-restored "结论：交接就绪" "还原名册后必须重新就绪"
printf '演练读数：非法名册 → 导出 %s / 交接校验 %s；坏 JSON → 导出 %s；还原后重新就绪\n' \
    "$ROSTER_BAD_EXPORT_CODE" "$ROSTER_BAD_HANDOFF_CODE" "$ROSTER_BAD_JSON_CODE"

printf '%s\n' "演练 schema：$SCHEMA（结束即 DROP）"
q "CREATE SCHEMA $SCHEMA" >/dev/null
SCHEMA_CREATED=1

printf '%s\n' '===== ⑦ 顺序第四段：导出 → 导入 → 切换（回退窗口开放） ====='
CUTOVER_EXISTS=$(q "SELECT count(*) FROM information_schema.tables WHERE table_schema = '$SCHEMA' AND table_name = 'storage_cutover'")
expect_eq "导入前 storage_cutover 尚未建表" 0 "$CUTOVER_EXISTS"
run_in "$CP_MAIN" export-main db export --out "$TMP/pkg"
expect_contains export-main "核查状态：not_required（可交付）" "无未归类文件时应为可交付包"
EXPORT_ENTRIES=$(field export-main "资产条目：")
run_in "$CP_MAIN" import-main db import --dump "$TMP/pkg"
expect_contains import-main "合计 " "导入必须报出合计行数"
FIRST_WRITE_BEFORE=$(q "SELECT coalesce(max(first_write_at)::text, 'NULL') FROM $SCHEMA.storage_cutover")
expect_eq "切换前 first_write_at" NULL "$FIRST_WRITE_BEFORE"
run_in "$CP_MAIN" switch-main db switch --dump "$TMP/pkg" --workspace "$CP_MAIN/.basalt"
expect_contains switch-main "回退窗口：开放（first_write_at 仍为 NULL）" "首次切换必须留下开放的回退窗口"
expect_contains switch-main "源端重盘：已执行（源工作区与清单一致）" "给出 --workspace 时必须执行第四道门禁"
SWITCH_MANIFEST=$(field switch-main "绑定清单 SHA-256：")
CUTOVER_MANIFEST=$(q "SELECT manifest_sha256 FROM $SCHEMA.storage_cutover")
expect_eq "storage_cutover.manifest_sha256 与报告一致" "$SWITCH_MANIFEST" "$CUTOVER_MANIFEST"
FIRST_WRITE_AFTER=$(q "SELECT coalesce(max(first_write_at)::text, 'NULL') FROM $SCHEMA.storage_cutover")
expect_eq "切换后 first_write_at" NULL "$FIRST_WRITE_AFTER"
printf '演练读数：导出条目 %s，切换后 first_write_at=%s（回退窗口开放）\n' "$EXPORT_ENTRIES" "$FIRST_WRITE_AFTER"

printf '%s\n' '===== ⑧ 顺序第五、六段：切换后离线门仍就绪 → 验证窗口 ready ====='
run_in "$CP_MAIN" handoff-after-switch db verify --handoff "$CP_MAIN/.basalt"
expect_contains handoff-after-switch "结论：交接就绪" "切换后交接证据仍必须成立"
expect_contains handoff-after-switch "目标配置核对：已核对" "切换不得让目标配置核对失效"
run_window "$CP_MAIN" window-ready
READY_BODY=$(curl -sS --max-time 10 "http://$WINDOW_ADDR/api/v1/ready") || fail "ready 面不可达（$WINDOW_ADDR）"
printf 'ready 响应：%s\n' "$READY_BODY"
printf '%s\n' "$READY_BODY" | grep -qF '"verification_window":true' ||
    fail "ready 响应必须声明自己在验证窗口姿态下"
printf '%s\n' "$READY_BODY" | grep -qF '"status":"ready"' || fail "ready 响应必须报 ready"
HEALTH_BODY=$(curl -sS --max-time 10 "http://$WINDOW_ADDR/api/v1/health") || fail "health 面不可达（$WINDOW_ADDR）"
printf 'health 响应：%s\n' "$HEALTH_BODY"
printf '%s\n' "$HEALTH_BODY" | grep -qF '"status":"ok"' || fail "health 响应必须报 ok"
stop_window window-ready
expect_contains window-ready "验证窗口：serve 已在验证姿态下正常停止" "窗口必须报告正常停止"
expect_contains window-ready "decision=allowed" "三维都干净时结论必须是 allowed"
# 期望值取目标库实测，不写死字面量：PG 谱系每升一版（US-16 起 v2，D-19 的 `0002`
# 迁移）都写死会让演练与契约脱钩——窗口必须报出**目标库当前的** schema 版本。
WINDOW_EXPECTED_SCHEMA=$(q "SELECT max(version) FROM $SCHEMA.schema_version")
expect_contains window-ready "schema_version=$WINDOW_EXPECTED_SCHEMA" \
    "验证窗口必须连到 PostgreSQL 当前谱系版本（库内实测 $WINDOW_EXPECTED_SCHEMA）"
WINDOW_READY_ADDR=$WINDOW_ADDR
printf '演练读数：验证窗口 %s / decision=allowed / schema_version=%s\n' "$WINDOW_READY_ADDR" "$WINDOW_EXPECTED_SCHEMA"

# ═══════════════════════════════════════════════════════════════════════════════
# 第二部分：受控调整（§2.12.0b）——未记录拒绝 / 越界拒绝 / 带引用继续
# ═══════════════════════════════════════════════════════════════════════════════
printf '%s\n' '===== ⑨ 受控调整：无引用拒绝 → 越界拒绝 → 只改密钥路径则继续 ====='
CP_ADJ="$TMP/legacy-main/copy-adj"
copy_legacy_copy "$SRC_MAIN" "$CP_ADJ"
SRC=$SRC_MAIN
BAK=$BAK_MAIN
MANIFEST=$MAN_MAIN
adopt_legacy_ok "$CP_ADJ" adopt-legacy-adjust
expect_contains adopt-legacy-adjust "schema：5 → 6" "副本必须先完成受控升级"

# 受控调整的产物：副本本机可读的密钥副本 + 只改指向它的那一行配置。
# 目录 0700 / 文件 0600 是凭证守卫的硬要求（0755 目录直接被判 `recovery_required`）。
mkdir -p "$CP_ADJ/keys"
chmod 700 "$CP_ADJ/keys"
cp "$SRC_MAIN/private/master.key" "$CP_ADJ/keys/master.key"
chmod 600 "$CP_ADJ/keys/master.key"
sed -i "s|^  master_key_file: .*|  master_key_file: '$CP_ADJ/keys/master.key'|" "$CP_ADJ/basalt.yaml"
cp "$CP_ADJ/basalt.yaml" "$TMP/keyring/adjusted.yaml"
ADJ_SHA=$(file_sha "$CP_ADJ/basalt.yaml")
[ "$ADJ_SHA" != "$FOUR_COPY" ] || fail "受控调整没有改变副本配置的整文件哈希"

adj_before=$(refusal_snapshot "$CP_ADJ/.basalt")
run_must_fail "$CP_ADJ" adopt-config-unattested instance adopt
expect_contains adopt-config-unattested "config_changed_unattested" \
    "配置调整过又没带变更引用必须落在 config_changed_unattested"
expect_contains adopt-config-unattested "受控调整必须先在变更单登记" "原因必须点名变更单登记"
UNATTESTED_CODE=$EXPECTED_FAIL_CODE
[ "$UNATTESTED_CODE" -eq 1 ] || fail "config_changed_unattested 的退出码应为 1，实际 $UNATTESTED_CODE"
expect_eq "无引用被拒后副本工作区零改动" "$adj_before" "$(refusal_snapshot "$CP_ADJ/.basalt")"
[ ! -f "$CP_ADJ/.basalt/instance.json" ] || fail "无引用被拒后不得写出名册"

printf '\n# drill: 密钥路径以外的改动\n' >>"$CP_ADJ/basalt.yaml"
run_must_fail "$CP_ADJ" adopt-config-out-of-scope instance adopt --config-change-ref CHG-US16-DRILL-1
expect_contains adopt-config-out-of-scope "config_change_out_of_scope" \
    "有引用但密钥路径以外的字节也变了必须落在 config_change_out_of_scope"
expect_contains adopt-config-out-of-scope "受控调整只允许改密钥路径" "原因必须点名越界面"
OUT_OF_SCOPE_CODE=$EXPECTED_FAIL_CODE
[ "$OUT_OF_SCOPE_CODE" -eq 1 ] || fail "config_change_out_of_scope 的退出码应为 1，实际 $OUT_OF_SCOPE_CODE"
expect_eq "越界被拒后副本工作区零改动" "$adj_before" "$(refusal_snapshot "$CP_ADJ/.basalt")"
[ ! -f "$CP_ADJ/.basalt/instance.json" ] || fail "越界被拒后不得写出名册"

cp "$TMP/keyring/adjusted.yaml" "$CP_ADJ/basalt.yaml"
run_in "$CP_ADJ" adopt-config-attested instance adopt --config-change-ref CHG-US16-DRILL-1
expect_contains adopt-config-attested "命中判定：案例 1" "调整后的副本仍应命中案例 1"
expect_contains adopt-config-attested "目标配置哈希：$ADJ_SHA（config_adjusted=true）" \
    "受控调整必须把调整后的文件哈希记成目标配置哈希"
expect_contains adopt-config-attested "配置变更引用：CHG-US16-DRILL-1" "报告必须回显变更单引用"
ADJ_INSTANCE_ID=$(field adopt-config-attested "实例标识：")
[ -f "$CP_ADJ/.basalt/instance.json" ] || fail "受控调整通过后必须写出名册"
run_in "$CP_ADJ" handoff-after-adjust db verify --handoff "$CP_ADJ/.basalt"
expect_contains handoff-after-adjust "结论：交接就绪" "受控调整后的副本必须交接就绪"
expect_contains handoff-after-adjust "目标配置核对：已核对（当前 $ADJ_SHA == 审计 $ADJ_SHA）" \
    "目标配置哈希必须等于调整后的当前文件哈希"
expect_eq "受控调整不得改回源端配置哈希" "$FOUR_SOURCE" "$(file_sha "$SRC_MAIN/basalt.yaml")"
printf '演练读数：四值之四 target_config_sha256=%s（config_adjusted=true，引用 CHG-US16-DRILL-1）\n' "$ADJ_SHA"
printf '演练读数：未记录副本配置修改 → %s；有引用但越界 → %s\n' "$UNATTESTED_CODE" "$OUT_OF_SCOPE_CODE"

# ═══════════════════════════════════════════════════════════════════════════════
# 第三部分：源端漂移反例（库 + 同级 basalt.yaml）
# ═══════════════════════════════════════════════════════════════════════════════
printf '%s\n' '===== ⑩ 源端漂移：basalt.yaml → source_changed，副本零改动 ====='
SRC=$SRC_MAIN
BAK=$BAK_MAIN
MANIFEST=$MAN_MAIN
CP_DRIFT="$TMP/legacy-main/copy-drift"
copy_legacy_copy "$SRC_MAIN" "$CP_DRIFT"
cp "$SRC_MAIN/basalt.yaml" "$TMP/keyring/source.yaml"
cp "$SRC_MAIN/.basalt/basalt.db" "$TMP/keyring/source.db"
drift_before=$(refusal_snapshot "$CP_DRIFT/.basalt")
printf '\n# drill: 备份之后源端配置被改\n' >>"$SRC_MAIN/basalt.yaml"
adopt_legacy_must_fail "$CP_DRIFT" adopt-legacy-source-config-drift
expect_contains adopt-legacy-source-config-drift "source_changed" "源端配置漂移必须落在 source_changed"
expect_contains adopt-legacy-source-config-drift "源端 basalt.yaml 在备份之后发生了变化" \
    "原因必须点名配置漂移"
CONFIG_DRIFT_CODE=$EXPECTED_FAIL_CODE
[ "$CONFIG_DRIFT_CODE" -eq 1 ] || fail "source_changed 的退出码应为 1，实际 $CONFIG_DRIFT_CODE"
expect_eq "配置漂移被拒后副本工作区零改动" "$drift_before" "$(refusal_snapshot "$CP_DRIFT/.basalt")"
cp "$TMP/keyring/source.yaml" "$SRC_MAIN/basalt.yaml"
expect_eq "还原后的源端配置哈希" "$SRC_CFG_MAIN" "$(file_sha "$SRC_MAIN/basalt.yaml")"

printf '%s\n' '===== ⑪ 源端漂移：库 → source_changed，还原后同一副本可继续 ====='
printf 'drift' >>"$SRC_MAIN/.basalt/basalt.db"
adopt_legacy_must_fail "$CP_DRIFT" adopt-legacy-source-db-drift
expect_contains adopt-legacy-source-db-drift "source_changed" "源端库漂移必须落在 source_changed"
expect_contains adopt-legacy-source-db-drift "源端库在备份之后发生了变化" "原因必须点名库漂移"
DB_DRIFT_CODE=$EXPECTED_FAIL_CODE
[ "$DB_DRIFT_CODE" -eq 1 ] || fail "source_changed 的退出码应为 1，实际 $DB_DRIFT_CODE"
expect_eq "库漂移被拒后副本工作区零改动" "$drift_before" "$(refusal_snapshot "$CP_DRIFT/.basalt")"
cp "$TMP/keyring/source.db" "$SRC_MAIN/.basalt/basalt.db"
expect_eq "还原后的源端库哈希" "$SRC_DB_MAIN" "$(file_sha "$SRC_MAIN/.basalt/basalt.db")"
adopt_legacy_ok "$CP_DRIFT" adopt-legacy-drift-recovered
expect_contains adopt-legacy-drift-recovered "schema：5 → 6" "还原源端后同一副本必须能重跑成功"
printf '演练读数：源端漂移 → 配置 %s / 库 %s（均 source_changed，副本零改动，还原后重跑成功）\n' \
    "$CONFIG_DRIFT_CODE" "$DB_DRIFT_CODE"

# ═══════════════════════════════════════════════════════════════════════════════
# 第四部分：schema 3 与 schema 4 的受控升级
# ═══════════════════════════════════════════════════════════════════════════════
printf '%s\n' '===== ⑫ schema 3 与 schema 4：同样升级到 v6 并写一条记录 ====='
for legacy_schema in 3 4; do
    provision_legacy "s$legacy_schema" "$legacy_schema" local
    inspect_ws "s${legacy_schema}-before" "$CP/.basalt"
    expect_eq "schema $legacy_schema 升级前版本" "$legacy_schema" "$(inspect_field schema)"
    expect_eq "schema $legacy_schema 升级前记录数" 0 "$(inspect_field instance_secrets)"
    expect_eq "schema $legacy_schema 升级前名册存在性" absent "$(inspect_field roster)"
    adopt_legacy_ok "$CP" "adopt-legacy-s$legacy_schema"
    expect_contains "adopt-legacy-s$legacy_schema" "schema：$legacy_schema → 6" \
        "schema $legacy_schema 必须被带到 v6"
    expect_contains "adopt-legacy-s$legacy_schema" "密文：1 条（迁移后逐字节复核）" \
        "schema $legacy_schema 迁移后必须逐字节复核密文"
    expect_contains "adopt-legacy-s$legacy_schema" "名册：存在=false（本命令不写名册）" \
        "schema $legacy_schema 的受控升级不写名册"
    inspect_ws "s${legacy_schema}-after" "$CP/.basalt"
    expect_eq "schema $legacy_schema 升级后版本" 6 "$(inspect_field schema)"
    expect_eq "schema $legacy_schema 升级后记录数" 1 "$(inspect_field instance_secrets)"
    expect_eq "schema $legacy_schema 升级后 credentials" 1 "$(inspect_field credentials)"
    expect_eq "schema $legacy_schema 升级后名册存在性" absent "$(inspect_field roster)"
    printf '演练读数：schema %s → 6，记录 1 条，名册 absent\n' "$legacy_schema"
done

printf '%s\n' '===== 演练结论 ====='
DATA_ROOT_ENTRIES=$(find "$DATA_ROOT" -mindepth 1 -print -quit)
expect_eq "用户数据根必须保持为空" "" "$DATA_ROOT_ENTRIES"
printf '升级副本（schema 5 → 6）：记录 %s 条、密文版本 %s 条、名册 %s\n' \
    "$MAIN_RECORDS" "$MAIN_CREDENTIAL_VERSIONS" "$MAIN_ROSTER"
printf '配置哈希四值：source=%s / backup=%s / copy=%s / target=%s\n' \
    "$FOUR_SOURCE" "$FOUR_BACKUP" "$FOUR_COPY" "$ADJ_SHA"
printf '源端漂移：配置 → 退出码 %s、库 → 退出码 %s（source_changed，副本零改动）\n' \
    "$CONFIG_DRIFT_CODE" "$DB_DRIFT_CODE"
printf '配置调整：未记录 → 退出码 %s（config_changed_unattested）/ 越界 → %s（config_change_out_of_scope）/ 只改密钥路径 → 继续（config_adjusted=true）\n' \
    "$UNATTESTED_CODE" "$OUT_OF_SCOPE_CODE"
printf '无名册顺序：记录 → 名册（%s）→ 身份 / 配置核对 → 导出 %s → 切换 → 离线门 → 验证窗口 %s\n' \
    "$MAIN_INSTANCE_ID" "$EXPORT_ENTRIES" "$WINDOW_READY_ADDR"
printf '缺名册拒绝：导出 %s / 交接校验 %s / 验证窗口 %s（listener_opened=false）\n' \
    "$EXPORT_NO_ROSTER_CODE" "$HANDOFF_NO_ROSTER_CODE" "$WINDOW_NO_ROSTER_CODE"
printf '非法名册拒绝：导出 %s / 交接校验 %s（instance_identity_mismatch）/ 坏 JSON 导出 %s\n' \
    "$ROSTER_BAD_EXPORT_CODE" "$ROSTER_BAD_HANDOFF_CODE" "$ROSTER_BAD_JSON_CODE"
printf '%s\n' '未由 CLI 产生的分支：'
printf '%s\n' '  - `config_ambiguous`（配置重复定义 / 别名 / 合并锚点）需要构造歧义 YAML，不在本演练的可达面内，由 TestInstanceAdopt_ConfigChangeRequiresRecord 的歧义子用例覆盖。'
printf '%s\n' '  - `legacy_adopt_evidence_missing`（记录指向的 legacy-adopt 操作行缺失或非 success）要求篡改审计行，产品 CLI 不产生该状态，由 TestInstanceAdopt_ConfigChangeRequiresRecord / TestSecretsAdoptLegacy_RecordWithoutRosterIsNotExportable 覆盖。'
printf '%s\n' '  - 部分迁移副本的重试拒绝（`ErrSchemaOutdated` → recovery_required）需要中途打断迁移，命令行无法稳定复现，由 TestImport_FailedImportLeavesEmptyTables 与 handoff 校验的 schema 拒绝分支覆盖。'
printf '%s\n' '演练通过。'
