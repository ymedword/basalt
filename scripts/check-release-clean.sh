#!/bin/sh
#
# check-release-clean.sh —— 发布干净检出：只从**已提交的候选提交**导出源码，在临时目录里跑
# full profile 与 make e2e（模块卡 §4.5 图 2-12、C-18）。发布证据必须能被第三方用
# commit + 归档逐字节复现，工作区里的未提交改动一律不算候选提交。
# check-release-clean.sh - export the committed candidate and run the full profile plus
# make e2e inside a temporary tree. An uncommitted worktree is never a release candidate.
#
# 判据（缺一即非零退出，**不降级为跳过**）：
#   1. 工作区干净：`git status --porcelain` 必须为空；
#   2. 临时树只含候选提交的已跟踪内容：`git worktree add --detach <tmp> HEAD` 检出，
#      再用 `git archive HEAD` 解出第二份树，**逐文件比对**（内容 / 权限 / 相对路径）
#      必须一致——这就是 C-18 要的「`git archive HEAD` 复现记录」的机器判据；
#   3. 在临时树跑 `make acceptance`（full profile）与 `make e2e`。
#
# 为什么不是"只解档、不带 .git"：`scripts/acceptance` 的源码指纹要求工作树是 Git
# 工作树（`fingerprint.go` 的 `sourceFingerprint`：`git ls-files --cached --others
# --exclude-standard`），纯解档目录没有 `.git`，full profile 根本无法开跑。
# 早前用 `GIT_DIR` / `GIT_WORK_TREE` 借用主仓身份的做法被实测否决：这两个变量会
# 泄漏进被调起的 `go test`，把 `scripts/acceptance` 自己的临时仓库劫持成主仓，
# 使 `TestAccept_RejectsSourceChangedDuringChecks` 变红（2026-10-25 克隆实测）。
# `git worktree add --detach` 给临时树一份**自己的** `.git`（指向同一对象库），
# 无需任何环境变量，内容仍严格等于候选提交。
# Why a detached worktree rather than a bare archive: the acceptance runner fingerprints
# through `git ls-files`, and exporting GIT_DIR/GIT_WORK_TREE into `go test` hijacks the
# runner's own fixtures (observed red: TestAccept_RejectsSourceChangedDuringChecks).
# A detached worktree carries its own `.git` file, and its content is byte-compared
# against `git archive HEAD` before any gate runs.
#
# 前置：Go 工具链、gofumpt / staticcheck / gosec / govulncheck、C 编译器（-race）、
# Python 3（make e2e 的示例单测；本机只有 `py -3` 可用，见 Makefile 的 PYTHON）。
# 缺前置 = 失败，不是跳过。

set -eu

root=$(git rev-parse --show-toplevel)
cd "$root"

if [ -n "$(git status --porcelain)" ]; then
	echo "clean-checkout: 工作区不干净——发布证据只能绑定已提交的候选提交" >&2
	git status --porcelain | sed -n '1,40p' >&2
	exit 1
fi

commit=$(git rev-parse HEAD)
version=$(git describe --tags --always)
tmp=$(mktemp -d)
# 原生 Windows make / go 不认 MSYS 的 /tmp 映射，先把临时目录转成盘符路径（pwd -W）；
# 非 MSYS 平台 pwd -W 不存在，回落到 pwd。
tmp=$(cd "$tmp" && { pwd -W 2>/dev/null || pwd; })
tree="$tmp/tree"
mirror="$tmp/archive"
cleanup() {
	git worktree remove --force "$tree" >/dev/null 2>&1 || true
	git worktree prune >/dev/null 2>&1 || true
	rm -rf "$tmp"
}
trap cleanup EXIT INT TERM HUP

# 1) 候选提交的干净检出：detached worktree，内容 = HEAD，状态必须干净。
git worktree add --detach --quiet "$tree" HEAD
if [ -n "$(git -C "$tree" status --porcelain)" ]; then
	echo "clean-checkout: 临时树不干净——检出结果与 HEAD 不符" >&2
	git -C "$tree" status --porcelain | sed -n '1,40p' >&2
	exit 1
fi

# 2) 归档等价核验：`git archive HEAD` 解出的树必须与工作树逐文件一致。
mkdir -p "$mirror"
git archive HEAD | tar -x -C "$mirror"
if ! diff -r --exclude=.git "$tree" "$mirror" >/dev/null; then
	echo "clean-checkout: 干净检出与 git archive HEAD 不一致" >&2
	diff -r --exclude=.git "$tree" "$mirror" | sed -n '1,40p' >&2
	exit 1
fi
files=$(find "$mirror" -type f | wc -l | tr -d ' ')
echo "clean-checkout: commit=$commit version=$version files=$files tree=$tree"

# 3) 在临时树跑发布门禁；两处都不注入 GIT_*（见文件头说明）。
make -C "$tree" acceptance
make -C "$tree" e2e

echo "clean-checkout: passed (commit=$commit version=$version)"
