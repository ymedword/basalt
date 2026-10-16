#!/bin/sh
#
# run-evals.sh —— 评测基座的**可选**包装：把「默认用例集 + 显式 GOPROXY + 透传参数」
# 收成一条命令。评测层**不进** `make check`（门禁化属构建步骤变更，走用户裁决）。
# run-evals.sh wraps the eval runner: default case set, explicit module proxy, passthrough
# args. The eval layer is deliberately not part of `make check`.
#
# 用法 / usage:
#   scripts/run-evals.sh                          # 默认用例集（fake 档恒跑，真模型档 SKIP）
#   scripts/run-evals.sh --cases evals/injection  # 换用例目录
#   scripts/run-evals.sh --provider deepseek      # 真模型档显式门控（凭证走环境变量）
#
# 透传的参数原样交给运行器（`"$@"`）；运行器自身只认 `-cases` 与 `-provider`。
# 退出码原样透传（0 无 FAIL / 1 有用例 FAIL / 2 运行器自身出错）——包装层不吞退出码。
#
# 必须在**仓库根**执行（用例工作区 / basalt.yaml / 结果目录都相对进程工作目录解析）；
# 本脚本先切到仓库根再跑，故从任意目录调用都成立。
set -u

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd) || exit 2
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd) || exit 2
cd "$repo_root" || exit 2

# 本机直连 proxy.golang.org 不可达（实测读数见 docs/GettingStarted.md §2.6）——
# 显式带模块代理是全部 go 命令的统一纪律。
GOPROXY=https://goproxy.cn
export GOPROXY

exec go run ./evals --cases evals/cases "$@"
