#!/bin/sh
# Basalt 四件套一致性备份：数据库 dump + 工作区卷 + 实例私有卷 + 宿主 secret，
# 最后写 SHA256SUMS 并恢复应用运行状态。
# Consistent four-part backup: database dump, workspace volume, instance private
# volume and the host secret files, followed by SHA256SUMS and the app restart.
#
# 用法：在 docker/ 目录或任意路径执行，项目名必须显式给出（与部署时的 -p 一致）：
#   COMPOSE_PROJECT_NAME=basalt sh docker/backup.sh /var/backups/basalt-20261102
#
# 免宿主 root：所有需要读 0400 文件 / root 属主的动作都在一次性 --user 0 容器里做
# （卡片 §3.1 的「root 或 docker --user 0」）。宿主用户只需要 docker 权限。
# No host root is required: every privileged read happens inside one-shot --user 0
# containers, so a docker-group user is enough.

set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: backup.sh <backup-dir>" >&2
    exit 64
fi
backup_dir=$1

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose_file="$script_dir/compose.yaml"

project=${COMPOSE_PROJECT_NAME:-}
if [ -z "$project" ]; then
    echo "basalt: 必须显式设置 COMPOSE_PROJECT_NAME（与部署时的 -p / .env 一致），脚本不猜项目名" >&2
    exit 64
fi

# COMPOSE_FILE 已设时交给 compose 自己解析（它支持冒号分隔的多文件，容器 e2e 用它带
# 测试 overlay）；未设时用脚本同目录的 compose.yaml——部署目录的形状。
# When COMPOSE_FILE is set, compose resolves it itself (it accepts a colon-separated
# list, which the container e2e uses to add its overlay); otherwise the neighbouring
# compose.yaml is authoritative.
compose() {
    if [ -n "${COMPOSE_FILE:-}" ]; then
        docker compose -p "$project" "$@"
    else
        docker compose -p "$project" -f "$compose_file" "$@"
    fi
}

fail() {
    echo "basalt: $*" >&2
    exit 1
}

# 应用镜像从现有容器取，保证归档与本次部署的版本一致；容器不存在说明还没 up 过。
# The image comes from the existing container so the backup matches what is deployed.
container_id=$(compose ps -a -q basalt 2>/dev/null | head -n 1 || true)
[ -n "$container_id" ] || fail "项目 $project 没有 basalt 容器——先 docker compose up -d"
app_image=$(docker inspect --format '{{.Config.Image}}' "$container_id")
[ -n "$app_image" ] || fail "无法读取 basalt 容器的镜像引用"

# 卷名由 compose 加项目前缀，用标签取实际名字，避免自造命名规则。
# Compose prefixes volume names; the labels give the real names.
volume_name() {
    docker volume ls \
        --filter "label=com.docker.compose.project=$project" \
        --filter "label=com.docker.compose.volume=$1" \
        --format '{{.Name}}' | head -n 1
}
workspace_volume=$(volume_name workspace)
private_volume=$(volume_name private)
[ -n "$workspace_volume" ] || fail "找不到项目 $project 的 workspace 卷"
[ -n "$private_volume" ] || fail "找不到项目 $project 的 private 卷"

mkdir -p "$backup_dir"
chmod 0700 "$backup_dir"

# 备份失败也必须把应用拉回来：否则一次磁盘写满就变成停服事故。拉不回来时脚本
# **不得**报成功——备份制品仍有效，但运维必须知道服务还在停着。
# Always bring the app back, even when the backup fails halfway. If it cannot come
# back, the script must not report success: the archive is valid but the service is
# still down.
restart_app() {
    if compose start basalt >/dev/null 2>&1 &&
        [ -n "$(compose ps --status running -q basalt 2>/dev/null)" ]; then
        return 0
    fi
    echo "basalt: 应用未能重新启动（备份制品本身仍有效）；用 docker compose -p $project ps 定位原因后手动启动" >&2
    return 1
}
trap restart_app EXIT

echo "basalt: 停止应用容器（保持数据库在线）"
compose stop basalt >/dev/null

echo "basalt: 导出数据库"
compose exec -T postgres sh -c 'pg_dump -U "$POSTGRES_USER" -Fc "$POSTGRES_DB"' \
    > "$backup_dir/database.dump"

echo "basalt: 归档工作区与实例私有卷"
docker run --rm --user 0 \
    -v "$workspace_volume:/src:ro" \
    -v "$backup_dir:/backup" \
    --entrypoint sh "$app_image" -c 'tar -czf /backup/workspace.tar.gz -C /src .'
docker run --rm --user 0 \
    -v "$private_volume:/src:ro" \
    -v "$backup_dir:/backup" \
    --entrypoint sh "$app_image" -c 'tar -czf /backup/private.tar.gz -C /src .'

echo "basalt: 复制宿主 secret（备份副本 0600）"
docker run --rm --user 0 \
    -v "$script_dir/secrets:/src:ro" \
    -v "$backup_dir:/backup" \
    --entrypoint sh "$app_image" -c '
        umask 077
        mkdir -p /backup/secrets
        cp /src/database_dsn /src/postgres_password /backup/secrets/
        chmod 0600 /backup/secrets/database_dsn /backup/secrets/postgres_password
    '

# 一次性 root 容器留下的归档与 secret 副本默认归 root：宿主的 sha256sum 读不了
# （实测 Permission denied），备份目录的清理也会被 0700 的 secrets 目录挡住。脚本
# 承诺「免宿主 root」，所以在这里把整份备份交回调用者属主。
# Root one-shot containers leave root-owned artifacts; hand the whole backup back to
# the invoking user so the host-side sha256sum and cleanup work without root.
docker run --rm --user 0 \
    -v "$backup_dir:/backup" \
    -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
    --entrypoint sh "$app_image" -c 'chown -R "$HOST_UID:$HOST_GID" /backup'

echo "basalt: 写 SHA256SUMS"
(
    cd "$backup_dir"
    sha256sum database.dump workspace.tar.gz private.tar.gz \
        secrets/database_dsn secrets/postgres_password > SHA256SUMS
)

trap - EXIT
restart_app || exit 1
echo "basalt: 备份完成：$backup_dir（数据库 + 工作区 + 私有卷 + secret）"
