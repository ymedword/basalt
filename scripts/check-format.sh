#!/bin/sh
#
# check-format.sh —— 格式门禁：gofmt 与 gofumpt 各查一遍，两边都必须干净。
# check-format.sh - the formatting gate: gofmt and gofumpt, both must be clean.
#
# 判卷顺序（**必须先查退出码、再判输出**，见 docs/DevelopmentLoop.md §2.2）：
#   格式差异 → 退出码 0 但 stdout 非空（只把不合规文件打印出来，**不会自动失败**）
#   语法错误 → 退出码非 0 但 stdout 为空（报错打 stderr）
# 只判输出空会把「文件根本解析不了」判成通过；只判退出码会漏掉未格式化文件。
#
# 判卷顺序为何写在脚本而不是 Makefile 配方里：Windows 上的原生 make（ezwinports
# GNU Make 4.4.1）按 ANSI 代码页读取 Makefile，UTF-8 中文在解码错位后会破坏引号
# 配对——单行配方里的中文能把整条配方判成语法错误（本机实跑复现，见开发操作 §7）。
# shell 脚本由 sh 直接读取，没有这层转换。
#
# 退出码 / exit codes: 0 全部干净；1 有未格式化文件或工具异常退出。
set -u

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd) || exit 2
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd) || exit 2
cd "$repo_root" || exit 2

check_one() {
	tool=$1
	echo "== $tool -l . （扫描 $repo_root）=="
	out=$("$tool" -l .)
	status=$?
	if [ "$status" -ne 0 ]; then
		echo "$tool 异常退出（退出码 $status，多为语法错误）——报错见上方" >&2
		exit 1
	fi
	if [ -n "$out" ]; then
		echo "未格式化（$tool）：" >&2
		echo "$out" >&2
		exit 1
	fi
}

check_one gofmt
check_one gofumpt
echo "check-format: gofmt / gofumpt 均干净"
exit 0
