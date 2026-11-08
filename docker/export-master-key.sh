#!/bin/sh
# Basalt 主密钥交接（US-25）：把控制面私有卷里的主密钥导出到宿主，供 Worker 只读挂载。
# Exports the control plane's master key to the host so a worker can mount it read-only.
#
# 用法：
#   sudo sh docker/export-master-key.sh <compose-project> docker/secrets/master.key
# 例：`docker compose -f docker/compose.multiworker.yaml --profile control up -d` 之后，
# project 是 compose 的项目名（缺省取目录名，可用 -p 显式指定）。
#
# 为什么必须手工交接：两个角色必须共用**同一把**主密钥。Worker 自己铸钥会解不开控制面
# 已加密的托管凭据（`secrets.auto_init: false` 是这套纪律的配置面表达）。本脚本只做
# 复制与权限设置，不做任何轮换，也不打印密钥内容。
# Both roles must share one key: a worker that mints its own cannot decrypt the control
# plane's managed credentials. This script copies and sets permissions; nothing else.
#
# 约束（C-08 / Operations §6.1）：
# - 目标文件已存在一律拒绝覆盖；轮换走单独流程（两个角色必须成套更换）。
# - 宿主目标目录必须 `0700`、属主 `10001:10001`：bind mount 保留宿主 uid/gid，目录
#   不可被 10001 穿越时，容器内即使密钥是 0400 也打不开。
# - 密钥文件 `0400`、属主 `10001:10001`；写后逐项 stat 复核（9p / drvfs 会静默忽略
#   chown / chmod，复核不过即非零退出）。
# - **不得**打印、上传或写进镜像层；本脚本只在宿主文件系统与 Docker 卷之间搬运。
# The destination directory must be 0700 and the file 0400, both owned by 10001:10001;
# the key is never printed, uploaded or baked into an image.

set -eu

usage() {
    echo "usage: export-master-key.sh <compose-project> <dest-file>" >&2
    return 64
}

# 属主 / 权限的完成判据：stat 复核是这类脚本唯一可靠的信号——9p / drvfs 上
# chown / chmod 返回 0 却不生效。$2 形如 `10001:10001:400`。
# The stat re-check is the only reliable completion signal on 9p / drvfs mounts.
require_stat() {
    path=$1
    want=$2
    actual=$(stat -c '%u:%g:%a' "$path")
    if [ "$actual" != "$want" ]; then
        echo "basalt: $path 的属主 / 权限为 $actual，期望 $want——文件系统可能忽略 chown/chmod（9p/drvfs），请换 Linux 原生路径" >&2
        return 1
    fi
}

# verify_key_ownership <file>：只核对既有密钥文件的属主与权限，供部署核对与测试复用。
# Read-only check of an existing key file's ownership and mode.
verify_key_ownership() {
    [ "$#" -eq 1 ] || {
        echo "usage: verify_key_ownership <file>" >&2
        return 64
    }
    [ -f "$1" ] || {
        echo "basalt: $1 不存在或不是普通文件" >&2
        return 1
    }
    require_stat "$1" "10001:10001:400"
}

# export_master_key <compose-project> <dest-file>：从控制面私有卷导出主密钥。
# Copies the key out of <project>_control-private without ever echoing its bytes.
export_master_key() {
    [ "$#" -eq 2 ] || {
        usage
        return 64
    }
    project=$1
    dest=$2
    case "$project" in
        "") usage; return 64 ;;
    esac
    case "$dest" in
        "") usage; return 64 ;;
    esac

    if [ "$(id -u)" -ne 0 ]; then
        echo "basalt: export-master-key.sh 需要 root 设置属主与权限（chown 10001:10001）" >&2
        return 77
    fi

    # 目标形态与前置校验**先于任何副作用**：旧次序在目标已存在、卷不存在或卷里没有
    # 密钥时也会先 `mkdir -p` 并把父目录 chown 成 10001——`sudo ... /etc/master.key`
    # 之类的手误会把系统目录改属主。现在的次序是：路径形态 → 已存在 → 卷 / 密钥 →
    # 最后才建目录并设置属主。
    # Validate the destination shape before any side effect: the old order chowned the
    # parent directory even when the destination already existed or the key was missing.
    case "$dest" in
        */) echo "basalt: $dest 是目录路径，dest 必须是文件" >&2; return 64 ;;
    esac
    dest_dir=$(dirname -- "$dest")
    case "$dest_dir" in
        ""|.|/|/bin|/bin/*|/boot|/boot/*|/dev|/dev/*|/etc|/etc/*|/lib|/lib/*|/lib64|/lib64/*|/proc|/proc/*|/root|/root/*|/run|/run/*|/sbin|/sbin/*|/sys|/sys/*|/usr|/usr/*|/var|/var/*)
            echo "basalt: 拒绝把 $dest_dir 当作部署目录改写属主与权限——dest 应位于部署目录（如 secrets/master.key）" >&2
            return 1
            ;;
    esac
    if [ -L "$dest" ] || [ -L "$dest_dir" ]; then
        echo "basalt: $dest 或它的父目录是符号链接，拒绝写入（密钥可能被落到别的路径）" >&2
        return 1
    fi

    if [ -e "$dest" ]; then
        echo "basalt: $dest 已存在，拒绝覆盖。" >&2
        echo "  轮换主密钥要两个角色成套更换：先停全部进程，再按 Operations §6 流程重做。" >&2
        return 1
    fi

    volume="${project}_control-private"
    if ! volume_path=$(docker volume inspect -f '{{ .Mountpoint }}' "$volume" 2>/dev/null); then
        echo "basalt: 找不到 Docker 卷 $volume——先在控制面宿主用 compose 起过 --profile control（卷名是 <project>_control-private）" >&2
        return 1
    fi
    if [ ! -d "$volume_path" ]; then
        echo "basalt: 卷 $volume 的挂载点 $volume_path 在当前主机上不可读——本脚本要求在本机 Docker 守护进程上执行" >&2
        return 1
    fi

    # 私有目录布局：<XDG_DATA_HOME>/basalt/instances/<inst_id>/master.key。一个卷里正常
    # 只有一把；多于一把说明卷被复用，指向哪个都不安全——报出候选路径（路径不是机密）
    # 后退出。glob 不匹配时 `$1` 就是未展开的字面串，存在性检查会挡住。
    # Layout: <root>/basalt/instances/<id>/master.key. More than one key means the volume
    # was reused; picking one silently would ship the wrong key.
    set -- "$volume_path"/basalt/instances/*/master.key
    if [ ! -e "$1" ]; then
        echo "basalt: 卷 $volume 里没有主密钥（$volume_path/basalt/instances/*/master.key）——" >&2
        echo "  先让控制面完成首次启动（或管理台初始化托管凭据），再导出。" >&2
        return 1
    fi
    if [ "$#" -ne 1 ]; then
        echo "basalt: 卷 $volume 里有 $# 个主密钥，无法判断该导出哪一个：" >&2
        for candidate in "$@"; do
            echo "  $candidate" >&2
        done
        return 1
    fi
    source_key=$1
    if [ ! -s "$source_key" ]; then
        echo "basalt: 卷里的主密钥 $source_key 为空（半截写入）——检查控制面是否已完整初始化" >&2
        return 1
    fi

    # 到这里前置条件才全部成立，现在才创建目标目录并设置属主。目标目录必须是应用
    # UID 的 0700（与镜像内 /run/basalt-secrets 同判据）；bind mount 保留宿主 uid/gid。
    # Only now, with every precondition satisfied, create and own the destination
    # directory. The bind mount preserves the host uid/gid and needs 0700.
    mkdir -p "$dest_dir"
    chown 10001:10001 "$dest_dir"
    chmod 0700 "$dest_dir"
    require_stat "$dest_dir" "10001:10001:700" || return $?

    # 复制：umask 077 先建后收紧，写入过程中不留可读窗口；全程不回显内容。
    # Copy under umask 077 and tighten afterwards; the content is never echoed.
    umask 077
    if ! cat -- "$source_key" > "$dest"; then
        rm -f "$dest"
        echo "basalt: 写入 $dest 失败" >&2
        return 1
    fi
    chmod 0400 "$dest"
    chown 10001:10001 "$dest"
    verify_key_ownership "$dest" || return $?

    echo "basalt: 已导出主密钥到 $dest（0400、10001:10001；宿主目录 0700、10001:10001）"
    echo "  两宿主部署：把该文件与 $dest_dir 的属主 / 权限原样复制到 B，再以只读 bind mount 挂到"
    echo "  /run/basalt-secrets/master_key——**不得**放进 /run/secrets（默认 0755，会被父目录判据拒绝）。"
}

# 直接执行时才跑；被测试脚本 `source` 时只定义函数，不产生副作用。
# Only run when executed; sourcing (as the tests do) defines the functions and stops.
case "${0##*/}" in
    export-master-key.sh)
        export_master_key "$@"
        ;;
esac
