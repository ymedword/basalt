#!/bin/sh
# stage-runtime.sh 按 release/runtime-manifest.json 组装随包运行时组件。
# stage-runtime.sh stages the bundled runtime components declared by
# release/runtime-manifest.json for one full-tier target.
#
# 用法 / usage:
#   sh scripts/release-package/stage-runtime.sh <windows|linux> <outdir> [cache-dir]
#
# 纪律 / discipline:
#   * 下载物一律先校验 SHA-256 再用；校验失败即退出，不"先解压再说"。
#   * 版本只有一处：本脚本与 release/runtime-manifest.json 的 version 必须一致，
#     末尾按清单逐项回显版本，不匹配即失败。
#   * 只写 <outdir> 与缓存目录；不读工作区之外的状态，不打印任何凭据。
#   * docs-readonly 从本仓源码构建（CGO_ENABLED=0），不下载第三方二进制。
# Every download is checksum-verified before use; versions echo against the manifest.
set -eu

GOOS=${1:-}
OUT=${2:-}
CACHE=${3:-"${TMPDIR:-/tmp}/basalt-runtime-cache"}

usage() {
	echo "usage: stage-runtime.sh <windows|linux> <outdir> [cache-dir]" >&2
	exit 2
}

[ -n "$GOOS" ] && [ -n "$OUT" ] || usage
case "$GOOS" in
windows | linux) ;;
*) usage ;;
esac

REPO=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
MANIFEST="$REPO/release/runtime-manifest.json"
[ -f "$MANIFEST" ] || { echo "缺少 runtime 清单: $MANIFEST" >&2; exit 1; }

NODE_VERSION=24.22.0
NODE_LINUX_URL="https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-linux-x64.tar.xz"
NODE_LINUX_SHA256=cc2f601cee82d9a5a233302fe307abc600727035fd7d2c3a3571755989e95008
NODE_WINDOWS_URL="https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-win-x64.zip"
NODE_WINDOWS_SHA256=f9683a19877e12a9b167305c8db71040d812876359de21f0d2e97c3f321f80c3

PBS_TAG=20250409
PBS_VERSION=3.12.10
PBS_LINUX_URL="https://github.com/astral-sh/python-build-standalone/releases/download/$PBS_TAG/cpython-$PBS_VERSION+$PBS_TAG-x86_64-unknown-linux-gnu-install_only.tar.gz"
PBS_LINUX_SHA256=e9cf6f7da499a4400ba30ae1da8f7ef25ce97827bd8c1084717aa05438035186
PBS_WINDOWS_URL="https://github.com/astral-sh/python-build-standalone/releases/download/$PBS_TAG/cpython-$PBS_VERSION+$PBS_TAG-x86_64-pc-windows-msvc-install_only.tar.gz"
PBS_WINDOWS_SHA256=5ac66ae49a2104efeba985c1dc1cb40987757ee81cc6c7f218d10b801f3276ed

GHMCP_VERSION=2.0.3
GHMCP_LINUX_URL="https://github.com/github/github-mcp-server/releases/download/v$GHMCP_VERSION/github-mcp-server_Linux_x86_64.tar.gz"
GHMCP_LINUX_SHA256=f797c20fe02a755865d0e2d46184ab47e31bf1619b0855b8d89c531e45a651c2
GHMCP_WINDOWS_URL="https://github.com/github/github-mcp-server/releases/download/v$GHMCP_VERSION/github-mcp-server_Windows_x86_64.zip"
GHMCP_WINDOWS_SHA256=f0e1b9880a1b88730d1250d1c25cfee39617ec3e800a7a252f2279cd0c8394e0

DBHUB_VERSION=1.5.0
FETCH_VERSION=2026.10.24

if [ -d "$OUT" ] && [ -n "$(ls -A "$OUT" 2>/dev/null)" ]; then
	echo "输出目录非空，请先删除：$OUT" >&2
	exit 1
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/basalt-runtime-stage-XXXXXX")
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$OUT" "$CACHE"

echo "stage-runtime: goos=$GOOS out=$OUT cache=$CACHE"

# fetch_pinned 下载并校验一个固定版本的组件；缓存里已有且校验通过就不重复下载。
# 只有最终路径写 stdout（命令替换取它）；进度与提示一律走 stderr。
# fetch_pinned prints only the verified path on stdout; all progress goes to stderr.
fetch_pinned() {
	url=$1 sha=$2
	name=$(basename "$url")
	file="$CACHE/$name"
	if [ -f "$file" ] && echo "$sha  $file" | sha256sum -c - >/dev/null 2>&1; then
		echo "cache-hit $name" >&2
		echo "$file"
		return 0
	fi
	echo "download $url" >&2
	rm -f "$file"
	curl -fsSL --retry 3 --retry-delay 2 -o "$file.part" "$url"
	echo "$sha  $file.part" | sha256sum -c - >/dev/null 2>&1 || {
		echo "SHA-256 校验失败: $name" >&2
		rm -f "$file.part"
		exit 1
	}
	mv "$file.part" "$file"
	echo "verified $name" >&2
	echo "$file"
}

extract_zip() {
	archive=$1 dest=$2
	if command -v unzip >/dev/null 2>&1; then
		unzip -q -o "$archive" -d "$dest"
	elif tar -xf "$archive" -C "$dest" 2>/dev/null; then
		:
	else
		# Windows 自带 tar 是 bsdtar，zip 由它处理;PowerShell 是最后兜底。
		powershell -NoProfile -Command "Expand-Archive -LiteralPath '$archive' -DestinationPath '$dest' -Force" >/dev/null
	fi
}

# --- node ---------------------------------------------------------------
if [ "$GOOS" = linux ]; then
	node_archive=$(fetch_pinned "$NODE_LINUX_URL" "$NODE_LINUX_SHA256")
	mkdir -p "$WORK/node"
	tar -xf "$node_archive" -C "$WORK/node"
	mv "$WORK/node/node-v$NODE_VERSION-linux-x64" "$OUT/node"
else
	node_archive=$(fetch_pinned "$NODE_WINDOWS_URL" "$NODE_WINDOWS_SHA256")
	mkdir -p "$WORK/node"
	extract_zip "$node_archive" "$WORK/node"
	mv "$WORK/node/node-v$NODE_VERSION-win-x64" "$OUT/node"
fi

# --- python（python-build-standalone install_only，包内自带 python/ 目录） ---
if [ "$GOOS" = linux ]; then
	pbs_archive=$(fetch_pinned "$PBS_LINUX_URL" "$PBS_LINUX_SHA256")
	tar -xf "$pbs_archive" -C "$OUT"
else
	pbs_archive=$(fetch_pinned "$PBS_WINDOWS_URL" "$PBS_WINDOWS_SHA256")
	tar -xf "$pbs_archive" -C "$OUT"
fi
# PBS 根目录只有 lib/python3.12/LICENSE.txt；清单要求 python/LICENSE.txt。
if [ ! -f "$OUT/python/LICENSE.txt" ]; then
	for candidate in "$OUT/python/lib/python3.12/LICENSE.txt" "$OUT/python/LICENSE"; do
		if [ -f "$candidate" ]; then
			cp "$candidate" "$OUT/python/LICENSE.txt"
			break
		fi
	done
fi
[ -f "$OUT/python/LICENSE.txt" ] || { echo "python 缺少 LICENSE.txt（PBS 布局变化？）" >&2; exit 1; }

# --- github-mcp-server --------------------------------------------------
mkdir -p "$OUT/mcp/github-mcp-server/$GHMCP_VERSION" "$WORK/ghmcp"
if [ "$GOOS" = linux ]; then
	gh_archive=$(fetch_pinned "$GHMCP_LINUX_URL" "$GHMCP_LINUX_SHA256")
else
	gh_archive=$(fetch_pinned "$GHMCP_WINDOWS_URL" "$GHMCP_WINDOWS_SHA256")
fi
case "$gh_archive" in
*.tar.gz) tar -xf "$gh_archive" -C "$WORK/ghmcp" ;;
*) extract_zip "$gh_archive" "$WORK/ghmcp" ;;
esac
gh_bin=$(find "$WORK/ghmcp" -type f \( -name "github-mcp-server" -o -name "github-mcp-server.exe" \) | head -1)
[ -n "$gh_bin" ] || { echo "github-mcp-server 归档里找不到可执行文件" >&2; exit 1; }
if [ "$GOOS" = windows ]; then
	cp "$gh_bin" "$OUT/mcp/github-mcp-server/$GHMCP_VERSION/github-mcp-server.exe"
else
	cp "$gh_bin" "$OUT/mcp/github-mcp-server/$GHMCP_VERSION/github-mcp-server"
	chmod 0755 "$OUT/mcp/github-mcp-server/$GHMCP_VERSION/github-mcp-server"
fi

# --- docs-readonly（本仓源码构建） --------------------------------------
mkdir -p "$OUT/mcp/docs-readonly/1.0.0"
docs_out="$OUT/mcp/docs-readonly/1.0.0/docs-readonly"
[ "$GOOS" = windows ] && docs_out="$docs_out.exe"
( cd "$REPO" && CGO_ENABLED=0 GOOS="$GOOS" GOARCH=amd64 \
	go build -trimpath -ldflags "-s -w" -o "$docs_out" ./examples/shared/mcp/docs-readonly )
chmod 0755 "$docs_out"

# --- dbhub（npm 包快照，含 node_modules） ------------------------------
# 用刚解出的 node/npm 安装，避免宿主 node 版本差异；--prefix 让 node_modules
# 落在清单声明的路径下。
mkdir -p "$OUT/mcp/dbhub/$DBHUB_VERSION"
if [ "$GOOS" = linux ]; then
	npm_node="$OUT/node/bin/node"
	npm_cli="$OUT/node/lib/node_modules/npm/bin/npm-cli.js"
else
	npm_node="$OUT/node/node.exe"
	npm_cli="$OUT/node/node_modules/npm/bin/npm-cli.js"
fi
"$npm_node" "$npm_cli" install --omit=dev --no-audit --no-fund \
	--prefix "$OUT/mcp/dbhub/$DBHUB_VERSION" "@bytebase/dbhub@$DBHUB_VERSION" >/dev/null

# --- fetch（pip 包快照 + 随包启动器） ----------------------------------
fetch_root="$OUT/mcp/fetch/$FETCH_VERSION"
mkdir -p "$fetch_root/site-packages"
cp "$REPO/release/runtime/fetch/run_fetch.py" "$fetch_root/run_fetch.py"
if [ "$GOOS" = linux ]; then
	python_bin="$OUT/python/bin/python3"
else
	python_bin="$OUT/python/python.exe"
fi
"$python_bin" -m pip install --disable-pip-version-check --no-warn-script-location \
	--target "$fetch_root/site-packages" "mcp-server-fetch==$FETCH_VERSION" >/dev/null

# --- 版本回显（与清单一致才算组装成功） --------------------------------
node_version=$("$npm_node" --version)
python_version=$("$python_bin" --version)
echo "node=$node_version"
echo "python=$python_version"
[ "$node_version" = "v$NODE_VERSION" ] || { echo "node 版本不符：$node_version != v$NODE_VERSION" >&2; exit 1; }
[ "$python_version" = "Python $PBS_VERSION" ] || { echo "python 版本不符：$python_version != Python $PBS_VERSION" >&2; exit 1; }

# 按清单逐项确认路径存在（出包前的最后一道自检）。
for path in \
	"mcp/docs-readonly/1.0.0/docs-readonly" \
	"python/LICENSE.txt" \
	"mcp/dbhub/$DBHUB_VERSION/node_modules/@bytebase/dbhub/dist/index.js" \
	"mcp/dbhub/$DBHUB_VERSION/node_modules/@bytebase/dbhub/LICENSE" \
	"mcp/fetch/$FETCH_VERSION/run_fetch.py"; do
	[ -f "$OUT/$path" ] || { echo "组装后缺少 $path" >&2; exit 1; }
done
if [ "$GOOS" = windows ]; then
	[ -f "$OUT/node/node.exe" ] && [ -f "$OUT/python/python.exe" ] || { echo "Windows 运行时缺少 node.exe / python.exe" >&2; exit 1; }
	[ -f "$OUT/mcp/docs-readonly/1.0.0/docs-readonly.exe" ] || { echo "缺少 docs-readonly.exe" >&2; exit 1; }
	[ -f "$OUT/mcp/github-mcp-server/$GHMCP_VERSION/github-mcp-server.exe" ] || { echo "缺少 github-mcp-server.exe" >&2; exit 1; }
else
	[ -f "$OUT/node/bin/node" ] && [ -f "$OUT/python/bin/python3" ] || { echo "Linux 运行时缺少 node / python3" >&2; exit 1; }
	[ -f "$OUT/mcp/docs-readonly/1.0.0/docs-readonly" ] || { echo "缺少 docs-readonly" >&2; exit 1; }
	[ -f "$OUT/mcp/github-mcp-server/$GHMCP_VERSION/github-mcp-server" ] || { echo "缺少 github-mcp-server" >&2; exit 1; }
fi
echo "stage-runtime: ok ($GOOS)"
