#!/bin/sh
# Basalt 备份恢复：把四件套恢复到**全新项目名**（新卷、新端口），最后 up -d。
# 恢复顺序：放 secret 并重设属主 → create（建卷与容器，不启动）→ 解包 → 起库并
# pg_restore → 启动。相对卡片 §2.9 的「最后放 secret」有一步顺序调整：Compose 的
# 文件 secret 是 bind mount，创建容器时就校验源文件存在，secret 必须先就位；最终
# 状态与属主 / 权限判据不变。覆盖已有实例不在本脚本能力内，脚本先检查目标项目为空。
# 恢复目标要与源实例使用**独立部署目录**（复制 docker/ 资产与 .env）：file secret
# 绑定部署目录，同目录恢复不同实例的备份会覆盖源实例凭据，脚本检测到不一致即拒绝。
# Restore order: place secrets, create (volumes and containers, no start), unpack,
# pg_restore, start. Secrets move before create because compose file secrets are bind
# mounts validated at container creation; the final ownership/permission contract is
# unchanged. Overwriting an existing instance is out of scope.
#
# 用法：
#   COMPOSE_PROJECT_NAME=<源项目> sh docker/restore.sh <backup-dir> <new-project>
#
# 免宿主 root：解包与写 secret 都在一次性 --user 0 容器里做。

set -eu

if [ "$#" -ne 2 ]; then
    echo "usage: restore.sh <backup-dir> <new-project-name>" >&2
    exit 64
fi
backup_dir=$1
new_project=$2

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose_file="$script_dir/compose.yaml"

fail() {
    echo "basalt: $*" >&2
    exit 1
}

# 备份来源项目名：只在没给 BASALT_IMAGE 时用来找源容器（取助手镜像）。脚本不猜
# 项目名——猜错会拿别的实例当来源。
# Source project name: used only to locate the source container when BASALT_IMAGE is
# absent. The script never guesses a project name.
project=${COMPOSE_PROJECT_NAME:-}
[ -n "$project" ] || fail "必须显式设置 COMPOSE_PROJECT_NAME（备份来源项目），脚本不猜项目名"

# 恢复中途失败会留下**本次运行**创建的卷 / 容器，下次重跑会被“项目已有卷”挡住；
# 只在本次运行确实开始创建项目资源后打印清理命令。前置检查失败的路径（目标项目
# 已有卷 / 容器、备份校验失败等）绝不提示 down -v——那会删掉别人的数据。
# A failed restore may leave volumes or containers created by THIS run behind; print
# the cleanup command only after this run started creating project resources. Precheck
# refusals (an existing project, a bad checksum) never suggest `down -v`: that would
# delete data the restore must not touch.
restore_done=0
created_project=0
cleanup_hint() {
    status=$?
    if [ "$restore_done" -ne 1 ] && [ "$status" -ne 0 ] && [ "$created_project" -eq 1 ]; then
        echo "basalt: 恢复未完成；本次运行已在项目 $new_project 创建卷或容器。清理后重试：" >&2
        echo "  docker compose -p $new_project down -v" >&2
    fi
}
trap cleanup_hint EXIT

[ -f "$backup_dir/SHA256SUMS" ] || fail "$backup_dir 不是 Basalt 备份目录（缺 SHA256SUMS）"
(cd "$backup_dir" && sha256sum -c SHA256SUMS >/dev/null) || fail "备份校验和不符，拒绝恢复"

if [ -n "$(docker volume ls --filter "label=com.docker.compose.project=$new_project" --format '{{.Name}}')" ]; then
    fail "项目 $new_project 已存在卷——恢复到全新项目名，不覆盖现有实例"
fi
if [ -n "$(docker ps -a --filter "label=com.docker.compose.project=$new_project" --format '{{.ID}}')" ]; then
    fail "项目 $new_project 已存在容器——恢复到全新项目名，不覆盖现有实例"
fi

# COMPOSE_FILE 已设时交给 compose 自己解析（支持冒号分隔的多文件，容器 e2e 用它带
# 测试 overlay）；未设时用脚本同目录的 compose.yaml。
# When COMPOSE_FILE is set, compose resolves it itself; otherwise the neighbouring
# compose.yaml is authoritative.
restore_compose() {
    if [ -n "${COMPOSE_FILE:-}" ]; then
        docker compose -p "$new_project" "$@"
    else
        docker compose -p "$new_project" -f "$compose_file" "$@"
    fi
}

# source_compose 指向备份来源项目：只用来找源容器（取助手镜像），不改变任何状态。
# source_compose addresses the backup's source project, used only to find its
# container image.
source_compose() {
    if [ -n "${COMPOSE_FILE:-}" ]; then
        docker compose -p "$project" "$@"
    else
        docker compose -p "$project" -f "$compose_file" "$@"
    fi
}

# 助手容器镜像：优先取 BASALT_IMAGE（compose 的同一来源），否则取源项目已有的
# basalt 容器——目标项目此时还没有容器，不能等 create 之后再取。
# Helper image: BASALT_IMAGE when exported, else the source project's container.
helper_image=${BASALT_IMAGE:-}
if [ -z "$helper_image" ]; then
    source_container=$(source_compose ps -a -q basalt 2>/dev/null | head -n 1 || true)
    [ -n "$source_container" ] || fail "无法确定助手容器镜像：请设置 BASALT_IMAGE，或保证源项目 $project 存在 basalt 容器"
    helper_image=$(docker inspect --format '{{.Config.Image}}' "$source_container")
fi

# 目标部署目录的 secret 若已存在且与备份不同，**不得**覆盖：file secret 绑定的是
# 部署目录，同目录下可能还有正在运行的源实例，覆盖会让它下次重启时读到别的实例
# 的凭据。恢复一份不同实例的备份时，先把 docker/ 资产（compose、initdb、.env 与
# 本脚本）复制到独立目录，再在那里执行恢复。
# Refuse to overwrite different secrets: file secrets are bound to the deployment
# directory, and a running source instance may share it. Restore a different
# instance's backup from a copied deployment directory instead.
echo "basalt: 核对目标 secret 是否与备份一致"
if ! docker run --rm --user 0 \
    -v "$backup_dir/secrets:/src:ro" \
    -v "$script_dir/secrets:/dst" \
    --entrypoint sh "$helper_image" -c '
        set -eu
        for name in postgres_password database_dsn; do
            if [ -e "/dst/$name" ]; then
                cmp -s "/src/$name" "/dst/$name" || {
                    echo "basalt: $name 与备份不一致" >&2
                    exit 3
                }
            fi
        done
    '; then
    fail "目标 $script_dir/secrets 与备份的 secret 不同：不得覆盖仍在使用的部署目录；请把 docker/ 资产复制到独立目录后在该目录恢复"
fi

# secret 必须**先于** create 就位：Compose 的 file secret 是 bind mount，创建容器时
# 就要求源文件存在（实测报 bind source path does not exist）。因此卡片的「建卷 →
# 恢复 → 最后放 secret」在 Compose 下落到「create 前放好 secret 并重设属主」，
# 最终状态与判据（root:root 0400 / 10001:10001 0400）不变。
# Secrets must exist before create: file secrets are bind mounts whose source is
# validated at container creation.
echo "basalt: 放入 secret 并重设属主 / 权限"
docker run --rm --user 0 \
    -v "$backup_dir/secrets:/src:ro" \
    -v "$script_dir/secrets:/dst" \
    --entrypoint sh "$helper_image" -c '
        set -eu
        mkdir -p /dst
        cp /src/postgres_password /dst/postgres_password
        chown 0:0 /dst/postgres_password
        chmod 0400 /dst/postgres_password
        cp /src/database_dsn /dst/database_dsn
        chown 10001:10001 /dst/database_dsn
        chmod 0400 /dst/database_dsn
        [ "$(stat -c "%u:%g:%a" /dst/postgres_password)" = "0:0:400" ]
        [ "$(stat -c "%u:%g:%a" /dst/database_dsn)" = "10001:10001:400" ]
    '

echo "basalt: 创建 $new_project 的卷与容器（不启动）"
# `create` 本身就不启动容器；`--no-start` 是 `up` 的旗标，Compose v5.6.1 会以
# unknown flag 拒绝（实测）。
# `create` never starts containers; `--no-start` belongs to `up` and is rejected by
# Compose v5.6.1 as an unknown flag.
# 从这一行起项目资源由本次运行创建：前面两条前置检查已确认项目没有卷 / 容器，
# 后续失败提示 down -v 只针对本次运行留下的资源。
# From here on the project resources belong to this run: the prechecks above verified
# the project has no volumes or containers, so a failure hint may name them.
created_project=1
restore_compose create >/dev/null

container_id=$(restore_compose ps -a -q basalt | head -n 1)
[ -n "$container_id" ] || fail "创建后找不到 basalt 容器"
app_image=$(docker inspect --format '{{.Config.Image}}' "$container_id")

volume_name() {
    docker volume ls \
        --filter "label=com.docker.compose.project=$new_project" \
        --filter "label=com.docker.compose.volume=$1" \
        --format '{{.Name}}' | head -n 1
}
workspace_volume=$(volume_name workspace)
private_volume=$(volume_name private)
[ -n "$workspace_volume" ] || fail "找不到新项目的 workspace 卷"
[ -n "$private_volume" ] || fail "找不到新项目的 private 卷"

echo "basalt: 恢复工作区与实例私有卷（保留归档内的属主）"
docker run --rm --user 0 \
    -v "$workspace_volume:/dst" \
    -v "$backup_dir:/backup:ro" \
    --entrypoint sh "$app_image" -c 'tar -xzf /backup/workspace.tar.gz -C /dst'
docker run --rm --user 0 \
    -v "$private_volume:/dst" \
    -v "$backup_dir:/backup:ro" \
    --entrypoint sh "$app_image" -c 'tar -xzf /backup/private.tar.gz -C /dst'

echo "basalt: 先起数据库并等待健康"
restore_compose up -d --wait postgres >/dev/null

echo "basalt: 恢复数据库"
restore_compose exec -T postgres sh -c \
    'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --no-privileges' \
    < "$backup_dir/database.dump"

echo "basalt: 启动 $new_project"
restore_compose up -d --wait >/dev/null
restore_done=1
echo "basalt: 恢复完成：项目 $new_project（端口见 BASALT_HTTP_PORT）"
