#!/bin/sh
# Basalt 容器入口：解析 DSN secret、首启播种配置、exec 二进制。
# Basalt container entrypoint: resolve the DSN secret, seed the config on first boot
# and exec the binary.
#
# 约束（C-12 / §3.1）：
# - 不下载任何东西、不写镜像层；启动期只读 secret 与写一次工作区。
# - 永不打印 DSN；secret 读不到时不静默降级，把 Permission denied 留给 stderr。
# - exec 让二进制成为 tini（init: true 下的 PID 1）的直接子进程，信号由它转发。
# Constraints: no downloads, no image-layer writes, never print the DSN, and exec so
# the binary is tini's direct child and receives forwarded signals.

set -eu

DSN_FILE="${BASALT_DATABASE_DSN_FILE:-/run/secrets/database_dsn}"
CONFIG_TEMPLATE="/opt/basalt/basalt.yaml"
CONFIG_FILE="/data/basalt.yaml"

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

main() {
    resolve_dsn
    seed_config
    if [ "$#" -eq 0 ]; then
        set -- serve
    fi
    exec /opt/basalt/basalt "$@"
}

main "$@"
