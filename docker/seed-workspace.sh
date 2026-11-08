#!/bin/sh
# Basalt 工作区种子（US-25）：把控制面的 .basalt/ 子树打成带逐文件 SHA-256 清单的归档，
# 供 Worker 在另一台机器上导入后启动。Ships the control plane's .basalt/ subtree as a
# tar archive with a per-file SHA-256 manifest.
#
# 用法（子命令与函数等价，函数供测试 source 后直接调用）：
#   sh docker/seed-workspace.sh export <源工作区目录> <归档文件>
#   sh docker/seed-workspace.sh verify <归档文件>
#   sh docker/seed-workspace.sh import <归档文件> <目标工作区目录> [--force]
#
# **导出前必须先停掉源实例**：这是人工冷备，不是热同步。运行中的实例会继续写 agents/、
# memory/、assets/，导出的字节没有一致性的权威时刻。
# Stop the source instance before exporting: this is a manual cold copy, not hot sync.
#
# 归档内容与排除项（C-10 / FR-25-17）：
# - 只收 `.basalt/` 子树里的普通文件 + 工作区根的清单；`basalt.yaml`、SQLite 库与
#   日志/运行记录等一律不进归档。
# - 排除：`basalt.db`（含 -wal / -shm / -journal）、`basalt.lock`、`logs/`、`runs/`、
#   `agents/*/state/`、临时文件（`*.staging` / `*.backup` / `*.tmp` / `*~` 等）。
# - 拒绝符号链接、设备 / FIFO / socket：种子只承载普通文件，链接会把外部路径带进另一
#   台机器。
# Only regular files under .basalt/ are shipped; symlinks and special files are refused.
#
# 清单 `seed-manifest.json`（Q-25-03）：
#   {"format_version":1,"created_at":"…Z","source_instance_id":"inst_…",
#    "files":[{ "path":".basalt/…","size":N,"sha256":"…" }, …]}
# 每个条目单独一行、键顺序与空白固定——校验侧用一条严格模式匹配解析，格式外的内容一律
# 判失败（不用 sed 去"理解"任意 JSON）。
# One entry per line with a fixed shape; anything else fails the strict check.

set -eu

usage() {
    echo "usage: seed-workspace.sh export <source-workspace-dir> <archive>" >&2
    echo "       seed-workspace.sh verify <archive>" >&2
    echo "       seed-workspace.sh import <archive> <target-workspace-dir> [--force]" >&2
    return 64
}

die() {
    echo "basalt: $*" >&2
    return 1
}

# seed_path_shape_ok <path>：只判路径**形态**（相对、无穿越、无特殊字符）。
# 导出与导入两侧共用：形状不合法就不是种子里的合法成员。
seed_path_shape_ok() {
    p=${1%/}
    [ -n "$p" ] || return 1
    case "$p" in
        /*) return 1 ;;
        *\\*) return 1 ;;
        *\"*) return 1 ;;
        *[[:space:]]*) return 1 ;;
        *[[:cntrl:]]*) return 1 ;;
    esac
    rest=$p
    first=1
    while [ -n "$rest" ]; do
        case "$rest" in
            */*) comp=${rest%%/*}; rest=${rest#*/} ;;
            *) comp=$rest; rest= ;;
        esac
        case "$comp" in
            ""|"."|"..") return 1 ;;
        esac
        if [ "$first" -eq 1 ]; then
            [ "$comp" = ".basalt" ] || return 1
            first=0
        fi
    done
    return 0
}

# seed_path_excluded <path>：C-10 的排除项（返回 0 表示应跳过）。
# The C-10 exclusions; 0 means "skip this path".
seed_path_excluded() {
    p=${1%/}
    case "$p" in
        .basalt/basalt.db|.basalt/basalt.db-*|.basalt/basalt.lock) return 0 ;;
        .basalt/logs/*|.basalt/runs/*) return 0 ;;
        .basalt/agents/*/state/*) return 0 ;;
    esac
    # 临时件按整条路径匹配：`agents/<name>.staging/…` 与 `<name>.backup/…` 是目录，
    # 只比对文件名会把目录里的正文放进来。
    # Temp artifacts match on the whole path: .staging / .backup are directories.
    case "$p" in
        *.staging|*.staging/*|*.backup|*.backup/*) return 0 ;;
        *.tmp|*.tmp/*|*.temp|*.temp/*) return 0 ;;
        *.swp|*.swo|*.part|*~|*~/*|.DS_Store) return 0 ;;
    esac
    return 1
}

# seed_path_allowed <path>：形态合法且不在排除项内，才允许出现在清单与归档里。
seed_path_allowed() {
    seed_path_shape_ok "$1" || return 1
    seed_path_excluded "$1" && return 1
    return 0
}

# seed_archive_entry_ok <path>：校验 **tar 条目**（允许 `./` 前缀、目录尾斜杠，以及
# 归档根上的清单文件本身）；其余成员仍必须是 `.basalt/` 下的合法路径。
# Validates raw tar member names: `./` prefixes and trailing slashes are normal,
# and the manifest itself lives at the archive root.
seed_archive_entry_ok() {
    p=${1%/}
    while :; do
        case "$p" in
            ./*) p=${p#./} ;;
            *) break ;;
        esac
    done
    case "$p" in
        ""|".") return 0 ;;
        "seed-manifest.json") return 0 ;;
    esac
    seed_path_shape_ok "$p"
}

# seed_instance_id <workspace-dir>：提取 `instance_id` 字面量，必须恰好命中一次。
# 只收 `[A-Za-z0-9._:-]{1,64}`——产品形态是 `inst_` + 16 位小写十六进制，但**产品语义
# 不在这里重复校验**（那是 Go 解析器的事）；这里的值只进清单做来源标记，且不允许引号 /
# 斜杠 / 反斜杠，因此既不会破坏清单 JSON，也不能被拼成路径。
# The value is manifest metadata only; it may not contain quotes or path separators.
seed_instance_id() {
    roster=$1/.basalt/instance.json
    if [ ! -f "$roster" ]; then
        die "缺少实例名册 $roster——先让控制面完成首次启动" >&2
        return 1
    fi
    found=$(sed -n 's/.*"instance_id"[[:space:]]*:[[:space:]]*"\([A-Za-z0-9._:-]\{1,64\}\)".*/\1/p' "$roster")
    count=$(printf '%s\n' "$found" | grep -c '^[A-Za-z0-9._:-]')
    if [ "$count" != "1" ]; then
        die "名册 $roster 里的 instance_id 命中 $count 次（要求恰好 1 次，值只允许 [A-Za-z0-9._:-]，最长 64）" >&2
        return 1
    fi
    printf '%s' "$found"
}

# seed_list <workspace-dir> <out-file>：列出允许导出的相对路径（排序后稳定可比）。
# 符号链接必须失败关闭：种子只承载普通文件。
seed_list() {
    ws=$1
    out=$2
    if [ ! -d "$ws/.basalt" ]; then
        die "$ws 里没有 .basalt/，不是 Basalt 工作区" >&2
        return 1
    fi
    : > "$out"
    (
        cd "$ws" || exit 1
        find .basalt \( -type f -o -type l \) -print
    ) | LC_ALL=C sort > "$out.raw" || return 1
    while IFS= read -r rel; do
        [ -n "$rel" ] || continue
        if seed_path_excluded "$rel"; then
            continue
        fi
        if ! seed_path_shape_ok "$rel"; then
            die "路径不符合种子规则：$rel（只允许 .basalt/ 下的普通相对路径，不含空白、引号、反斜杠与控制字符）" >&2
            rm -f "$out" "$out.raw"
            return 1
        fi
        if [ -L "$ws/$rel" ]; then
            die "工作区含符号链接：$rel——种子只承载普通文件，请先人工处理" >&2
            rm -f "$out" "$out.raw"
            return 1
        fi
        if [ ! -f "$ws/$rel" ]; then
            die "不是普通文件：$rel" >&2
            rm -f "$out" "$out.raw"
            return 1
        fi
        printf '%s\n' "$rel" >> "$out"
    done < "$out.raw"
    rm -f "$out.raw"
    return 0
}

# seed_manifest_write <workspace-dir> <manifest-path> [list-file]：写清单。
# 不给 list-file 时现场采集一次（供人工核对单文件用）。
seed_manifest_write() {
    ws=$1
    manifest=$2
    list=${3:-}
    cleanup_list=0
    if [ -z "$list" ]; then
        list=$(mktemp) || return 1
        cleanup_list=1
        if ! seed_list "$ws" "$list"; then
            rm -f "$list"
            return 1
        fi
    fi
    inst=$(seed_instance_id "$ws") || {
        [ "$cleanup_list" -eq 1 ] && rm -f "$list"
        return 1
    }
    created=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    {
        printf '{\n'
        printf '  "format_version": 1,\n'
        printf '  "created_at": "%s",\n' "$created"
        printf '  "source_instance_id": "%s",\n' "$inst"
        printf '  "files": [\n'
        first=1
        while IFS= read -r rel; do
            [ -n "$rel" ] || continue
            size=$(wc -c < "$ws/$rel" | tr -d ' ')
            hash=$(sha256sum "$ws/$rel")
            hash=${hash%% *}
            if [ "$first" -eq 1 ]; then
                first=0
            else
                printf ',\n'
            fi
            printf '{"path":"%s","size":%s,"sha256":"%s"}' "$rel" "$size" "$hash"
        done < "$list"
        printf '\n  ]\n}\n'
    } > "$manifest" || {
        [ "$cleanup_list" -eq 1 ] && rm -f "$list"
        return 1
    }
    [ "$cleanup_list" -eq 1 ] && rm -f "$list"
    return 0
}

# seed_manifest_check <tree-root>：逐项核对清单，并拒绝清单外的文件与任何链接 / 特殊文件。
# 目标：篡改（sha256 不符）、截断（大小不符或文件缺失）、多带（清单外文件）都失败。
seed_manifest_check() {
    root=$1
    mf=$root/seed-manifest.json
    if [ ! -f "$mf" ]; then
        die "归档里没有 seed-manifest.json" >&2
        return 1
    fi
    fmt=$(sed -n 's/^  "format_version": \([0-9][0-9]*\),$/\1/p' "$mf")
    if [ "$fmt" != "1" ]; then
        die "清单 format_version=$fmt，只接受 1" >&2
        return 1
    fi
    inst=$(sed -n 's/^  "source_instance_id": "\([A-Za-z0-9._:-]\{1,64\}\)",$/\1/p' "$mf")
    if [ -z "$inst" ]; then
        die "清单缺少合法的 source_instance_id" >&2
        return 1
    fi
    entries=$(mktemp) || return 1
    bad=$(mktemp) || {
        rm -f "$entries"
        return 1
    }
    # 用 POSIX awk 按固定键名切段：路径里不允许出现双引号（seed_path_shape_ok 已保证），
    # 因此分隔点无歧义；不合固定形状的行记入 bad，随后与总数比对。
    # POSIX awk cuts on the literal key markers; paths cannot contain quotes, so the
    # cut points are unambiguous. Mismatched lines go to `bad` and fail the count check.
    listed=$(awk -v entries="$entries" -v bad="$bad" '
        /^  "files": \[$/ { infiles = 1; next }
        infiles && /^  \]$/ { infiles = 0; next }
        infiles {
            total++
            line = $0
            # 固定形状 {"path":"<p>","size":<n>,"sha256":"<h>"}：按字面键名顺序切段，
            # 不用正则（大括号、引号在各 awk 里的转义行为不一致），也不靠 count 猜。
            # Fixed shape; cut by literal key markers in order, no regex escapes.
            ok = (substr(line, 1, 9) == "{\"path\":\"")
            if (ok) {
                rest = substr(line, 10)
                i = index(rest, "\",\"size\":")
                ok = (i > 1)
                if (ok) {
                    path = substr(rest, 1, i - 1)
                    rest = substr(rest, i + length("\",\"size\":"))
                }
            }
            if (ok) {
                i = index(rest, ",\"sha256\":\"")
                ok = (i > 0)
                if (ok) {
                    size = substr(rest, 1, i - 1)
                    rest = substr(rest, i + length(",\"sha256\":\""))
                }
            }
            if (ok) {
                # 条目之间以行尾逗号分隔（最后一条没有），因此尾部允许一个 `,`。
                # Entries are comma-separated at end of line; allow one trailing comma.
                ok = (length(rest) > 2)
                if (ok && (substr(rest, length(rest)) == ",")) {
                    rest = substr(rest, 1, length(rest) - 1)
                }
                ok = ok && (length(rest) > 2) && (substr(rest, length(rest) - 1) == "\"}")
                if (ok) {
                    hash = substr(rest, 1, length(rest) - 2)
                }
            }
            if (ok) {
                ok = (path != "") && (index(path, "\"") == 0) &&
                     (size ~ /^[0-9]+$/) && (length(hash) == 64) && (hash ~ /^[0-9a-f]+$/)
            }
            if (!ok) {
                print line >> bad
                next
            }
            printf "%s\t%s\t%s\n", path, size, hash >> entries
            parsed++
        }
        END { print total + 0 }
    ' "$mf")
    parsed=$(wc -l < "$entries" | tr -d ' ')
    if [ "$listed" != "$parsed" ]; then
        rm -f "$entries" "$bad"
        die "清单 files[] 里有 $listed 行，但只有 $parsed 行符合固定格式（条目必须一行一条、键顺序固定）" >&2
        return 1
    fi
    rm -f "$bad"
    seen=$(mktemp) || {
        rm -f "$entries"
        return 1
    }
    tab=$(printf '\t')
    while IFS="$tab" read -r rel size hash; do
        [ -n "$rel" ] || continue
        if ! seed_path_allowed "$rel"; then
            rm -f "$entries" "$seen"
            die "清单条目不在允许范围：$rel" >&2
            return 1
        fi
        if [ ! -f "$root/$rel" ]; then
            rm -f "$entries" "$seen"
            die "清单里的文件缺失：$rel" >&2
            return 1
        fi
        actual_size=$(wc -c < "$root/$rel" | tr -d ' ')
        if [ "$actual_size" != "$size" ]; then
            rm -f "$entries" "$seen"
            die "大小不符：$rel（清单 $size，实际 $actual_size）" >&2
            return 1
        fi
        actual_hash=$(sha256sum "$root/$rel")
        actual_hash=${actual_hash%% *}
        if [ "$actual_hash" != "$hash" ]; then
            rm -f "$entries" "$seen"
            die "sha256 不符：$rel" >&2
            return 1
        fi
        printf '%s\n' "$rel" >> "$seen"
    done < "$entries"
    rm -f "$entries"
    # 清单外文件 / 任何非普通文件都拒绝：多一个文件说明归档被改过或打包逻辑有漏洞。
    find "$root" \( -type l -o -type p -o -type b -o -type c -o -type s \) -print > "$seen.special"
    if [ -s "$seen.special" ]; then
        bad=$(head -n 3 "$seen.special" | tr '\n' ' ')
        rm -f "$seen" "$seen.special"
        die "归档含链接或特殊文件：$bad" >&2
        return 1
    fi
    rm -f "$seen.special"
    find "$root" -type f -print | LC_ALL=C sort > "$seen.all"
    while IFS= read -r abs; do
        rel=${abs#"$root"/}
        [ "$rel" = "seed-manifest.json" ] && continue
        if ! grep -Fxq "$rel" "$seen"; then
            rm -f "$seen" "$seen.all"
            die "归档含清单外文件：$rel" >&2
            return 1
        fi
    done < "$seen.all"
    rm -f "$seen" "$seen.all"
    return 0
}

# seed_stage <archive> <stage-dir>：先查条目路径、再解压、再查特殊文件、最后核对清单。
# 解压到调用方给的临时目录；目标工作区在全部通过之前零落盘。
seed_stage() {
    archive=$1
    stage=$2
    if [ ! -f "$archive" ]; then
        die "归档不存在：$archive" >&2
        return 1
    fi
    if ! tar -tf "$archive" > "$stage.list" 2>/dev/null; then
        die "归档不可读或已截断：$archive" >&2
        return 1
    fi
    while IFS= read -r entry; do
        [ -n "$entry" ] || continue
        if ! seed_archive_entry_ok "$entry"; then
            die "归档条目路径越界或不合规：$entry" >&2
            return 1
        fi
    done < "$stage.list"
    mkdir -p "$stage/tree" || return 1
    if ! tar -xf "$archive" -C "$stage/tree" 2>/dev/null; then
        die "解压失败（截断或损坏）：$archive" >&2
        return 1
    fi
    for entry in "$stage/tree"/.basalt/*; do
        [ -e "$entry" ] || continue
        if [ -L "$entry" ]; then
            die "归档含符号链接：${entry#"$stage/tree"/}" >&2
            return 1
        fi
    done
    seed_manifest_check "$stage/tree"
}

# seed_export <source-workspace-dir> <archive>：生成归档（tar 主体 + 根目录清单）。
seed_export() {
    [ "$#" -eq 2 ] || {
        usage
        return 64
    }
    src=$1
    archive=$2
    if [ -e "$archive" ]; then
        die "归档已存在，拒绝覆盖：$archive" >&2
        return 1
    fi
    work=$(mktemp -d) || return 1
    list=$work/files.list
    if ! seed_list "$src" "$list"; then
        rm -rf "$work"
        return 1
    fi
    if ! seed_manifest_write "$src" "$work/seed-manifest.json" "$list"; then
        rm -rf "$work"
        return 1
    fi
    # busybox tar 没有 `-r`（追加），且 `-C` 是全局单值（后一个覆盖前一个）：先把成员
    # 解到暂存树，再以同一份文件清单一次性打包。清单因此落在归档根，正文保持 `.basalt/`
    # 前缀，且不会混进目录条目——归档成员与清单逐条对应。
    # busybox tar has neither `-r` nor a per-file `-C`: stage the members, then pack once
    # from an explicit file list, so the manifest sits at the archive root.
    stage=$work/tree
    if ! mkdir -p "$stage"; then
        rm -rf "$work"
        return 1
    fi
    if ! tar -cf "$work/members.tar" -C "$src" -T "$list"; then
        rm -rf "$work"
        die "打包失败：$src" >&2
        return 1
    fi
    if ! tar -xf "$work/members.tar" -C "$stage"; then
        rm -rf "$work"
        die "暂存种子文件失败：$src" >&2
        return 1
    fi
    if ! cp -f "$work/seed-manifest.json" "$stage/seed-manifest.json"; then
        rm -rf "$work"
        die "暂存清单失败：$work/seed-manifest.json" >&2
        return 1
    fi
    if ! cp -f "$list" "$work/pack.list" ||
        ! printf '%s\n' seed-manifest.json >> "$work/pack.list"; then
        rm -rf "$work"
        die "写入打包清单失败：$work/pack.list" >&2
        return 1
    fi
    if ! tar -cf "$work/seed.tar" -C "$stage" -T "$work/pack.list"; then
        rm -rf "$work"
        die "打包失败：$stage" >&2
        return 1
    fi
    if ! mv "$work/seed.tar" "$archive"; then
        rm -rf "$work"
        die "写入归档失败：$archive" >&2
        return 1
    fi
    count=$(wc -l < "$list" | tr -d ' ')
    rm -rf "$work"
    echo "basalt: 已导出种子到 $archive（$count 个文件 + seed-manifest.json）；源实例必须先停写"
    return 0
}

# seed_verify <archive>：只校验，不落任何目标工作区。
seed_verify() {
    [ "$#" -eq 1 ] || {
        usage
        return 64
    }
    stage=$(mktemp -d) || return 1
    if ! seed_stage "$1" "$stage"; then
        rm -rf "$stage"
        return 1
    fi
    count=$(grep -c '^{"path":' "$stage/tree/seed-manifest.json" || true)
    rm -rf "$stage"
    echo "basalt: 种子归档校验通过：$1（$count 个文件）"
    return 0
}

# seed_import <archive> <target-workspace-dir> [--force]：校验通过后才落盘。
seed_import() {
    case "$#" in
        2) force= ;;
        3) force=$3 ;;
        *) usage; return 64 ;;
    esac
    if [ -n "$force" ] && [ "$force" != "--force" ]; then
        usage
        return 64
    fi
    archive=$1
    target=$2
    stage=$(mktemp -d) || return 1
    if ! seed_stage "$archive" "$stage"; then
        rm -rf "$stage"
        return 1
    fi
    # 目标侧同样失败关闭：目标或 .basalt 是符号链接、或既有 .basalt 子树里已有符号
    # 链接时，`cp -R` 会顺着链接把种子写到链接指向的位置。归档侧的符号链接已在
    # seed_stage 拒绝，这里补上目标侧。
    # The target side fails closed too: cp -R would follow a symlink planted in the
    # destination and write the seed wherever it points.
    if [ -L "$target" ]; then
        rm -rf "$stage"
        die "目标 $target 是符号链接——拒绝把种子写进链接指向的目录" >&2
        return 1
    fi
    if [ -L "$target/.basalt" ]; then
        rm -rf "$stage"
        die "目标 $target/.basalt 是符号链接——拒绝把种子写进链接指向的目录" >&2
        return 1
    fi
    if [ -d "$target/.basalt" ] && [ -n "$(find "$target/.basalt" -type l 2>/dev/null)" ]; then
        rm -rf "$stage"
        die "目标 $target/.basalt 下已有符号链接——拒绝在链接树上覆盖，请先人工处理" >&2
        return 1
    fi
    if [ -e "$target/.basalt/instance.json" ] && [ "$force" != "--force" ]; then
        rm -rf "$stage"
        die "目标工作区已有实例名册 $target/.basalt/instance.json——换空目录，或确认后加 --force 覆盖" >&2
        return 1
    fi
    if ! mkdir -p "$target/.basalt"; then
        rm -rf "$stage"
        return 1
    fi
    # 可读性只在**暂存树**上设置：它只含本次要复制的种子文件，不碰目标目录里已有的
    # 内容（旧 runs、日志等）。属主能设成 10001 时本就不需要放开权限；这里做得更
    # 宽只是为了 chown 被文件系统忽略时 Worker 仍能读取。umask 022 让 cp 保留这组位。
    # Readability is applied to the staged tree only — never to pre-existing target
    # content. The wider bits are a fallback for filesystems where chown is ignored.
    chmod -R a+rX "$stage/tree/.basalt" "$stage/tree/seed-manifest.json" 2>/dev/null || true
    umask 022
    if ! cp -f "$stage/tree/seed-manifest.json" "$target/seed-manifest.json"; then
        rm -rf "$stage"
        die "写入清单失败：$target/seed-manifest.json" >&2
        return 1
    fi
    if ! cp -R "$stage/tree/.basalt/." "$target/.basalt/"; then
        rm -rf "$stage"
        die "复制种子内容失败：$target" >&2
        return 1
    fi
    rm -rf "$stage"
    # 容器以 10001:10001 运行；bind / 卷的属主必须跟着走，否则 Worker 既读不到也写不进
    # 工作区。设置失败不算导入失败（归档内容是对的），但必须把原因和补救说出来——
    # 非 root 与 WSL 的 /mnt/*（drvfs 会静默忽略 chown）都走这条警告。
    # The container runs as 10001:10001; when the ownership cannot be applied the
    # content is still correct, but the operator must be told why and how to fix it.
    chown -R 10001:10001 "$target/.basalt" "$target/seed-manifest.json" 2>/dev/null || true
    owner=$(stat -c '%u:%g' "$target/.basalt" 2>/dev/null || echo unknown)
    if [ "$owner" != "10001:10001" ]; then
        echo "basalt: 警告：$target/.basalt 的属主是 $owner，不是 10001:10001。" >&2
        echo "  Worker 以 10001 运行：请以 root 执行本命令；WSL 的 /mnt/* 会忽略 chown，请换 Linux 原生路径。" >&2
    fi
    echo "basalt: 已导入种子到 $target（含 seed-manifest.json）；启动 Worker 时配置须与源实例一致的 flows / providers / embedding"
    return 0
}

# 直接执行时才跑子命令；被测试 `source` 时只定义函数，不产生副作用。
case "${0##*/}" in
    seed-workspace.sh)
        cmd=${1:-}
        [ "$#" -gt 0 ] && shift
        case "$cmd" in
            export) seed_export "$@" ;;
            verify) seed_verify "$@" ;;
            import) seed_import "$@" ;;
            *) usage ;;
        esac
        ;;
esac
