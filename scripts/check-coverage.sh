#!/bin/sh
#
# check-coverage.sh —— 按包判覆盖率底线（宪法·开发工作流：门禁必须有可执行判据）。
# check-coverage.sh - per-package coverage thresholds: `go test -cover` never
# fails on a low number, so the gate needs this judge.
#
# 用法 / usage:
#   check-coverage.sh [--profile <file>] [--tree <dir>]
#     --profile <file>  以既有 coverage profile 为输入（自检注入合成输入用）
#     --tree <dir>      模块树根；缺省 = 本脚本所在目录的上一级（仓库根）
#
# 环境依赖 / environment: 自生成 profile 的模式带 `-tags=integration` 跑测试（覆盖率
# 口径含 PostgreSQL 存储），因此要求 BASALT_TEST_PG_DSN；缺省即失败（D-01，不跳过）。
# `--profile` 注入模式不跑测试，不要求 DSN。
# Generating the profile runs `-tags=integration`, so BASALT_TEST_PG_DSN is required and
# its absence is a failure (D-01); the injected-profile mode runs no tests.
#
# 阈值 / thresholds: core / tool / memory / storage / postgres >= 70%，其余 >= 50%。
# `postgres` 是 `internal/storage/postgres` 的基名——它不在 `internal/storage` 包内
# （两个谱系各自成包），基名匹配单列，否则它会落到 50% 档（US-16 §4.7 / T136 / T138）。
# `postgres` here is the base name of `internal/storage/postgres`, which lives outside
# the `internal/storage` package, so it needs its own entry in the 70% tier.
#
# 判据六类 / six verdicts:
#   1) 低于阈值 -> 失败                2) 恰好达标（>=）-> 通过
#   3) 无语句包 -> 跳过                4) 有语句但无测试文件 -> 计 0% 判失败
#   5) go test 自身失败 -> 非零        6) 输入异常（不可读 / 头非法 / 正文损坏）-> 非零 + 可读错误
#
# 退出码 / exit codes: 0 全达标；1 判据不成立；2 用法或输入错误。
set -u

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd) || exit 2
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd) || exit 2

profile_arg=""
tree=""

while [ $# -gt 0 ]; do
	case "$1" in
	--profile)
		[ $# -ge 2 ] || { echo "check-coverage: --profile 缺少参数（用法: check-coverage.sh [--profile <file>] [--tree <dir>]）" >&2; exit 2; }
		profile_arg=$2
		shift 2
		;;
	--tree)
		[ $# -ge 2 ] || { echo "check-coverage: --tree 缺少参数（用法: check-coverage.sh [--profile <file>] [--tree <dir>]）" >&2; exit 2; }
		tree=$2
		shift 2
		;;
	*)
		echo "check-coverage: 未知参数 $1（用法: check-coverage.sh [--profile <file>] [--tree <dir>]）" >&2
		exit 2
		;;
	esac
done

[ -n "$tree" ] || tree=$repo_root
[ -d "$tree" ] || { echo "check-coverage: 树根目录不存在: $tree" >&2; exit 2; }

work=$(mktemp -d "${TMPDIR:-/tmp}/basalt-cov.XXXXXX") || { echo "check-coverage: 无法创建临时目录" >&2; exit 2; }
trap 'rm -rf "$work"' EXIT INT TERM

# 覆盖率输入：注入的 profile，或就地跑一次 go test 生成。
# 第 5 类判据（go test 自身失败）在此拦下——测试失败绝不能被当成「没有输出所以通过」。
profile=$work/coverage.out
if [ -n "$profile_arg" ]; then
	[ -r "$profile_arg" ] || { echo "check-coverage: profile 不可读: $profile_arg" >&2; exit 2; }
	cp "$profile_arg" "$profile" 2>/dev/null || { echo "check-coverage: profile 无法读取: $profile_arg" >&2; exit 2; }
else
	# 缺 DSN 在起跑前判失败：带 -tags=integration 的 go test 若让用例全部跳过，
	# profile 只含单元测试，覆盖率会被静默低估甚至"达标"——这正是 D-01 禁止的假绿。
	# Fail before the run: an all-skipped integration suite would silently produce a
	# profile that under-counts (or even passes), which is the false green D-01 forbids.
	[ -n "${BASALT_TEST_PG_DSN:-}" ] || {
		echo "check-coverage: 未设置 BASALT_TEST_PG_DSN——覆盖率判卷带 -tags=integration，需要真实 PostgreSQL（D-01：缺省即失败，不跳过）" >&2
		echo "check-coverage: 排查路径：本地启动 dev-env 测试库并经 dev-env/scripts/with-postgres.sh 运行；环境准备见 docs/DevelopmentLoop.md §10；CI 由 postgres service 注入 DSN" >&2
		exit 1
	}
	# 显式 -timeout：go test 默认每包 10 分钟。2026-11-05 实测 internal/app 单跑
	# 601.7s、全仓并行下 cover 轮 900.1s（贴着 15m 边界）；用户裁决内层 15m→17m。
	# 同日晚夹具闸门（pgtemp）落地后权重上升：仍为 17m 时 cover 轮的 internal/app
	# 在 1020.637s 撞线（同一源码不带 -cover 时 949.793s），用户裁决 17m→22m，
	# 外层 harness 预算 1200s→1500s。只放宽时限，断言、包范围与覆盖率阈值不变。
	# Explicit -timeout: internal/app measured 601.7s solo and 900.1s under the
	# cover run's parallel contention; the inner bound went 15m→17m→22m as measured
	# (it hit 1020.637s at 17m while the same tree took 949.793s without -cover),
	# and the harness budget is now 1500s. Assertions, package scope and coverage
	# thresholds are unchanged.
	( cd "$tree" && go test -tags=integration -count=1 -timeout=22m -coverprofile="$profile" ./... ) || {
		echo "check-coverage: go test 失败——覆盖率判据不成立（不得把测试失败当通过）" >&2
		exit 1
	}
fi

# 第 6 类判据（输入异常）：空输入与坏 profile 都要给出可读错误，不得静默通过。
# 校验从严：mode 头必须取合法值、正文每行必须匹配记录格式——「合法头 + 损坏正文」
# 若不拦，非法记录会被聚合器静默跳过 / 强转，把"输入坏了"判成"全部达标"
# （实测：mode: banana 加合法记录与垃圾行，旧版按合法输入继续判卷）。
# 错误文案不回显输入内容（只报行号与应有格式）——profile 原则上不含敏感值，
# 但判卷脚本没有回显任意输入的职责。
[ -s "$profile" ] || { echo "check-coverage: 覆盖率输入为空: $profile" >&2; exit 1; }

mode_line=$(head -n 1 "$profile")
case "$mode_line" in
"mode: set" | "mode: count" | "mode: atomic") ;;
*)
	echo "check-coverage: profile 格式非法（首行必须是 mode: set / mode: count / mode: atomic 之一）: $profile" >&2
	exit 1
	;;
esac

# 校验正文：第 1 行是 mode 头（上面已单独校验），自第 2 行起必须全部匹配记录
# 格式，空行容忍（含尾部空行）。判定留在主 shell（**不得**用 `tail -n +2 | while`
# 改法：管道把循环放进子 shell，`exit 1` 只退出子 shell，父进程照旧往下判卷，
# 损坏输入反被静默放行），但校验本身**必须**是单次 awk 而不是逐行起 `grep` 进程
# ——Windows 上每次进程创建约 30 ms，34k 行 profile 会空转十几分钟（2026-10-30
# 实测判卷阶段 >15 min；改单次 awk 后 <1 s）。
# One awk pass finds the first offending record; the verdict still exits from the
# main shell. A per-line `grep` costs ~30 ms/row on Windows (measured 2026-10-30).
bad_line=$(awk '
NR == 1 { next }
$0 == "" { next }
$0 !~ /^[^[:space:]]+:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+ [0-9]+ [0-9]+$/ { print NR; exit }
' "$profile") || {
	echo "check-coverage: profile 正文校验执行失败（awk 不可用或读取失败）: $profile" >&2
	exit 1
}
if [ -n "$bad_line" ]; then
	echo "check-coverage: profile 正文记录损坏（第 $bad_line 行；格式应为 <文件>:<行>.<列>,<行>.<列> <语句数> <命中次数>）: $profile" >&2
	exit 1
fi

# 按包聚合 profile：包路径 = 源文件所在目录（Go 的一个包就是同目录的一批文件）。
# 记录格式已在上一步逐行校验——聚合器不再做静默跳过 / 数值强转（那会把损坏输入
# 悄悄算成达标）。
# The records were validated line by line above; the aggregator does not skip or
# coerce silently.
awk '
NR == 1 { next }
{
	loc = $1
	sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", loc)
	n = split(loc, parts, "/")
	pkg = parts[1]
	for (i = 2; i < n; i++) pkg = pkg "/" parts[i]
	stmt = $2 + 0
	total[pkg] += stmt
	if ($3 + 0 > 0) covered[pkg] += stmt
}
END {
	for (p in total) print p "\t" total[p] "\t" covered[p]
}
' "$profile" > "$work/agg.txt"

# 包清单来自 go list——「有语句但无测试文件」的包根本不出现在 profile 里，
# 只有包清单能揭示它们（第 4 类判据）。
( cd "$tree" && go list -f '{{.ImportPath}}|{{.Dir}}' ./... ) > "$work/pkgs.txt" 2>"$work/list.err" || {
	echo "check-coverage: go list 失败（树: $tree）:" >&2
	cat "$work/list.err" >&2
	exit 1
}
grep -v '^$' "$work/pkgs.txt" > "$work/pkgs.clean" || true
[ -s "$work/pkgs.clean" ] || { echo "check-coverage: 树里未发现任何包: $tree" >&2; exit 1; }

# 判定「有无可执行语句」需要 stmtcount——从本脚本所属仓库构建（而非 --tree，
# 因为 --tree 可能是自检用的合成树）。
stmtcount=$work/stmtcount
( cd "$repo_root" && go build -o "$stmtcount" ./scripts/stmtcount ) || {
	echo "check-coverage: 无法构建语句计数工具 scripts/stmtcount" >&2
	exit 1
}

failed=0
while IFS= read -r line; do
	import_path=${line%%|*}
	dir=${line#*|}
	base=${import_path##*/}

	case "$base" in
	core | tool | memory | storage | postgres) threshold=70 ;;
	*) threshold=50 ;;
	esac

	# 经 ENVIRON 传值而非 -v：Windows 目录名里的反斜杠会被 awk 的 -v 当转义序列
	# 处理（`D:\code\...` 变成 `D:code...`），ENVIRON 不做转义处理。
	hit=$(COV_IMPORT_PATH="$import_path" COV_PKG_DIR="$dir" awk \
		'$1 == ENVIRON["COV_IMPORT_PATH"] || $1 == ENVIRON["COV_PKG_DIR"] { print $2 " " $3; exit }' \
		"$work/agg.txt")
	if [ -n "$hit" ]; then
		total=${hit%% *}
		covered=${hit##* }
		if [ "$total" -eq 0 ]; then
			printf 'skip  %-58s 无可执行语句\n' "$import_path"
			continue
		fi
	else
		# 不在 profile 里：要么无可执行语句（跳过），要么有语句但没测试文件（计 0%）。
		count=$("$stmtcount" "$dir") || {
			echo "check-coverage: 统计语句数失败: $dir" >&2
			exit 1
		}
		if [ "$count" -eq 0 ]; then
			printf 'skip  %-58s 无可执行语句\n' "$import_path"
			continue
		fi
		total=$count
		covered=0
	fi

	# 阈值判定用**原始计数**：100 * covered >= threshold * total。先把百分比
	# 保留一位小数再判，会把 49.96% 抬成 50.0% 放过不达标包（实测：1249/2500
	# 判 ok）——舍入只用于显示，且显示取两位小数，避免"50.0% (< 50%)"式自相矛盾。
	# Judge on raw counts: 100*covered >= threshold*total; rounding is display-only.
	pct=$(awk -v c="$covered" -v t="$total" 'BEGIN { printf "%.2f", 100 * c / t }')
	if [ $((100 * covered)) -ge $((threshold * total)) ]; then
		printf 'ok    %-58s %6s%% (>= %s%%)\n' "$import_path" "$pct" "$threshold"
	else
		printf 'FAIL  %-58s %6s%% (< %s%%)\n' "$import_path" "$pct" "$threshold"
		failed=$((failed + 1))
	fi
done < "$work/pkgs.clean"

if [ "$failed" -gt 0 ]; then
	echo "check-coverage: $failed 个包低于覆盖率底线" >&2
	exit 1
fi
echo "check-coverage: 全部包达标"
exit 0
