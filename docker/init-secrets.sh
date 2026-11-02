#!/bin/sh
# Basalt 部署凭据生成：写 secrets/postgres_password 与 secrets/database_dsn。
# Generates the two deployment secrets and sets the ownership the containers require.
#
# 用法：
#   有宿主 root：            sudo sh docker/init-secrets.sh docker/secrets
#   无宿主 root（一次性容器）：
#     docker run --rm --user 0 \
#       -v "$PWD/docker/secrets:/secrets" \
#       -v "$PWD/docker/init-secrets.sh:/init-secrets.sh:ro" \
#       alpine:3.23@sha256:<release/image-lock.json 的 app_base digest> \
#       sh /init-secrets.sh /secrets
#    一次性容器看不到宿主 `.env`，自定义库名 / 用户名要加 -e POSTGRES_DB=… -e POSTGRES_USER=…。
#
# 库名 / 用户名缺省取 secrets 目录同级 `.env` 的 POSTGRES_DB / POSTGRES_USER（不
# source 整个文件），都没有时用 basalt——必须与 compose 建库时的取值一致。
#
# 约束（§3.1）：
# - file 型 secret 是绑定挂载，宿主属主 / 权限原样进入容器，必须显式设置：
#   postgres_password root:root 0400（PostgreSQL entrypoint 降权前读）、
#   database_dsn 10001:10001 0400（应用进程读）。
# - 文件已存在一律拒绝覆盖，没有 --force；轮换走单独流程。
# - 写后用 stat 复核：9p / drvfs（WSL 的 /mnt/*）会静默忽略 chown / chmod，
#   复核不过即非零退出，绝不把不可用的部署路径当成功。

set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: init-secrets.sh <secrets-dir>" >&2
    exit 64
fi
secrets_dir=$1

if [ "$(id -u)" -ne 0 ]; then
    echo "basalt: init-secrets.sh 需要 root 设置属主与权限；没有宿主 root 时用一次性 root 容器：" >&2
    echo "  docker run --rm --user 0 \\" >&2
    echo "    -v \"\$PWD/secrets:/secrets\" \\" >&2
    echo "    -v \"\$PWD/init-secrets.sh:/init-secrets.sh:ro\" \\" >&2
    echo "    alpine:3.23@sha256:<app_base digest> sh /init-secrets.sh /secrets" >&2
    exit 77
fi

mkdir -p "$secrets_dir"

# 先整体检查再写任何一个文件：半套 secret 比没有更难排查。
# Check both names before writing either one; a half-written set is worse than none.
for name in postgres_password database_dsn; do
    if [ -e "$secrets_dir/$name" ]; then
        echo "basalt: $secrets_dir/$name 已存在，拒绝覆盖。" >&2
        echo "  首次初始化失败后的重试：确认没有实例在用后，删除这两个文件再重跑；" >&2
        echo "  凭据轮换走单独流程（两个文件必须成套更换，不得只删一个）。" >&2
        exit 1
    fi
done

# 32 字节 /dev/urandom 转 hex：URL-safe，不依赖 openssl CLI。
# 32 random bytes as hex: URL-safe without depending on the openssl CLI.
random_hex() {
    od -An -N 32 -tx1 /dev/urandom | tr -d ' \n'
}

password=$(random_hex)

# 库名 / 用户名必须与 compose 建库建角色时用的一致：`.env` 由 compose 自动读取，
# 但 sudo 不会把 shell 环境带进脚本，因此这里直接解析 secrets 目录同级的 `.env`
# 的这两个键（不 source 整个文件）。一次性容器用法拿不到宿主 `.env`，用 -e 传入。
# Database and user must match compose's POSTGRES_DB / POSTGRES_USER. compose reads
# `.env`, but sudo drops the caller's environment, so parse those two keys from the
# `.env` next to the secrets directory without sourcing the file. The one-shot
# container path cannot see the host `.env`; pass the values with -e instead.
env_file=$(dirname -- "$secrets_dir")/.env
env_value() {
    [ -f "$env_file" ] || return 1
    value=$(sed -n "s/^$1=//p" "$env_file" | tail -n 1 | tr -d '\r')
    value=$(printf '%s' "$value" | sed "s/^[\"']//; s/[\"']$//")
    [ -n "$value" ] || return 1
    printf '%s' "$value"
}
db_name=${POSTGRES_DB:-$(env_value POSTGRES_DB || true)}
db_user=${POSTGRES_USER:-$(env_value POSTGRES_USER || true)}
db_name=${db_name:-basalt}
db_user=${db_user:-basalt}
case "$db_name" in
    *[!A-Za-z0-9_]*) echo "basalt: POSTGRES_DB=$db_name 含非法字符，只允许 [A-Za-z0-9_]" >&2; exit 1 ;;
esac
case "$db_user" in
    *[!A-Za-z0-9_]*) echo "basalt: POSTGRES_USER=$db_user 含非法字符，只允许 [A-Za-z0-9_]" >&2; exit 1 ;;
esac

umask 077
printf '%s' "$password" > "$secrets_dir/postgres_password"
chmod 0400 "$secrets_dir/postgres_password"
chown 0:0 "$secrets_dir/postgres_password"

printf 'postgres://%s:%s@postgres:5432/%s?sslmode=disable' \
    "$db_user" "$password" "$db_name" > "$secrets_dir/database_dsn"
chmod 0400 "$secrets_dir/database_dsn"
chown 10001:10001 "$secrets_dir/database_dsn"

# stat 复核是这类脚本唯一可靠的完成判据：chown / chmod 在 9p 上返回 0 但不生效。
# The stat re-check is the only reliable completion signal: 9p returns 0 for
# chown / chmod without applying them.
check_stat() {
    path=$1
    want=$2
    actual=$(stat -c '%u:%g:%a' "$path")
    if [ "$actual" != "$want" ]; then
        echo "basalt: $path 的属主 / 权限为 $actual，期望 $want——文件系统可能忽略 chown/chmod（9p/drvfs），请换 Linux 原生路径" >&2
        exit 1
    fi
}

check_stat "$secrets_dir/postgres_password" "0:0:400"
check_stat "$secrets_dir/database_dsn" "10001:10001:400"

echo "basalt: 已生成 $secrets_dir/postgres_password（0:0 0400）与 $secrets_dir/database_dsn（10001:10001 0400）"
