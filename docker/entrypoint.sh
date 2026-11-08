#!/bin/sh
# Basalt 容器入口：解析 DSN secret、按角色播种配置、Worker 启动预检、exec 二进制。
# Basalt container entrypoint: resolve the DSN secret, seed the role's config, run the
# worker preflight and exec the binary.
#
# 约束（C-12 / §3.1）：
# - 不下载任何东西、不写镜像层；启动期只读 secret 与写一次工作区。
# - 永不打印 DSN；secret 读不到时不静默降级，把 Permission denied 留给 stderr。
# - exec 让二进制成为 tini（init: true 下的 PID 1）的直接子进程，信号由它转发。
# - 预检**不解析 JSON**（模块卡 P-12）：名册只判存在与非空，字段语义归 Go 解析器；
#   密钥形态与权限由 `basalt secrets check-key` 的 `LoadMasterKey` 判。
# Constraints: no downloads, no image-layer writes, never print the DSN, and exec so
# the binary is tini's direct child and receives forwarded signals. The preflight never
# parses JSON; the roster is checked for existence and non-emptiness only, while the
# key's shape and permissions are judged by the Go parser behind `secrets check-key`.

set -eu

# 角色只决定用哪份模板与是否做 Worker 预检；未设时逐字保持 US-18 控制面行为。
# The role selects the template and the worker preflight; unset keeps US-18 behaviour.
ROLE="${BASALT_ROLE:-control}"
DSN_FILE="${BASALT_DATABASE_DSN_FILE:-/run/secrets/database_dsn}"
CONFIG_FILE="/data/basalt.yaml"
WORKSPACE_DIR="/data/.basalt"
WORKER_ROSTER="$WORKSPACE_DIR/instance.json"

# 模板缺省按角色区分，BASALT_CONFIG_TEMPLATE 可覆盖。Worker 模板没有 http / callers
# 两段——入口限流与 admin token 是控制面专属，Worker 带了也只会制造配置漂移。
# The template default depends on the role and BASALT_CONFIG_TEMPLATE overrides it. The
# worker template has no http or callers section: both belong to the control plane.
if [ "$ROLE" = "worker" ]; then
    CONFIG_TEMPLATE="${BASALT_CONFIG_TEMPLATE:-/opt/basalt/basalt.worker.yaml}"
else
    CONFIG_TEMPLATE="${BASALT_CONFIG_TEMPLATE:-/opt/basalt/basalt.yaml}"
fi

# fail 统一用 78（EX_CONFIG）：配置 / secret 类启动失败与业务退出区分开。
# fail exits with 78 (EX_CONFIG) so configuration failures are distinguishable.
fail() {
    echo "basalt: $*" >&2
    exit 78
}

# resolve_dsn 把 DSN secret 文件读进环境变量；文件缺失退出 78，不可读时保留 cat
# 的 Permission denied 并以非零退出。命令替换会去掉结尾换行，不裁剪内容本身。
# resolve_dsn exports the DSN from the secret file; a missing file exits 78 and an
# unreadable file keeps cat's Permission denied on stderr and fails non-zero.
resolve_dsn() {
    if [ ! -f "$DSN_FILE" ]; then
        fail "缺少 DSN secret 文件 $DSN_FILE——先用 docker/init-secrets.sh 生成 secrets/database_dsn"
    fi
    if ! dsn=$(cat "$DSN_FILE"); then
        fail "无法读取 DSN secret 文件 $DSN_FILE（属主 / 权限不符；要求 10001:10001 0400）"
    fi
    if [ -z "$dsn" ]; then
        fail "DSN secret 文件 $DSN_FILE 为空"
    fi
    export BASALT_DATABASE_DSN="$dsn"
}

# seed_config 只做首启播种：已存在的 /data/basalt.yaml 永不覆盖，升级补键由应用
# fail-closed 点名后由部署者手工补。cp 失败（工作区不可写）让脚本非零退出，
# 错误信息里带着目标路径。
# seed_config seeds the config on first boot only and never overwrites an existing
# one; a read-only workspace makes cp fail non-zero with the target path named.
seed_config() {
    if [ -f "$CONFIG_FILE" ]; then
        return 0
    fi
    cp "$CONFIG_TEMPLATE" "$CONFIG_FILE"
}

# require_worker_seed 是 Worker 的启动预检（§2.3.5）：工作区必须来自控制面的种子，
# 主密钥必须是控制面那一把。
#
# 四项判据依次是：名册存在（未种子的空工作区在这里停下，不建库、不写名册、不生成
# 密钥）、名册非空（半截写入）、配置存在（seed_config 刚播种过，仍缺即工作区不可写）、
# 主密钥通过 Go 解析器（`LoadMasterKey` 语义：普通文件、不经链接、工作区之外、
# 文件与**直接父目录**的组 / 其他位为零、JSON 形态、32 字节）。
#
# 主密钥路径缺省指向镜像内预建的 `/run/basalt-secrets`（0700、10001:10001）。
# **不得**改挂到 `/run/secrets`：该目录默认 0755，一次正确的密钥也会被父目录判据拒绝。
# require_worker_seed gates the worker on a seeded workspace and on the control plane's
# key; the default mount point is the image's pre-created 0700 /run/basalt-secrets, and
# the 0755 /run/secrets would refuse even a correct key on the parent-directory rule.
require_worker_seed() {
    : "${BASALT_MASTER_KEY_FILE:=/run/basalt-secrets/master_key}"
    export BASALT_MASTER_KEY_FILE

    if [ ! -f "$WORKER_ROSTER" ]; then
        fail "Worker 工作区未种子：缺少实例名册 $WORKER_ROSTER——先用 docker/seed-workspace.sh 从控制面导出并导入，Worker 不得自建实例"
    fi
    if [ ! -s "$WORKER_ROSTER" ]; then
        fail "实例名册 $WORKER_ROSTER 为空（半截写入）——重新导入种子，不要手工补写"
    fi
    if [ ! -f "$CONFIG_FILE" ]; then
        fail "Worker 配置缺失 $CONFIG_FILE——工作区不可写或种子不完整"
    fi
    # check-key 只打印 key_id 与公开指纹；失败时错误只点名缺失项，不会回显密钥字节。
    # check-key prints the key id and the public fingerprint only, never key material.
    if ! /opt/basalt/basalt secrets check-key --path "$BASALT_MASTER_KEY_FILE" --workspace "$WORKSPACE_DIR"; then
        fail "Worker 主密钥预检失败：$BASALT_MASTER_KEY_FILE（要求属主 10001:10001、文件 0400、直接父目录 0700；不得放进默认 0755 的 /run/secrets）"
    fi
}

main() {
    resolve_dsn
    seed_config
    if [ "$ROLE" = "worker" ]; then
        require_worker_seed
    fi
    if [ "$#" -eq 0 ]; then
        # 镜像自带 CMD ["serve"]，只有显式清空命令时才走到这里；Worker 缺省跑 gateway。
        # The image ships CMD ["serve"], so this branch only fires with an empty command.
        if [ "$ROLE" = "worker" ]; then
            set -- gateway
        else
            set -- serve
        fi
    fi
    exec /opt/basalt/basalt "$@"
}

main "$@"
