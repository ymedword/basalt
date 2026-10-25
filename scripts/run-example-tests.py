#!/usr/bin/env python3
"""逐个目录跑五个每日 Demo 的 Python 单测，并断言每目录的用例数下限（US-14 T056）。

运行 / Run：

    py -3 scripts/run-example-tests.py

判失败的六类情况（全部显式报出，不静默放过）：

* `unittest` 非零退出（有失败 / 有错误）；
* 目录不存在或不是目录（清单写死，改名即失败）；
* 扫描到 0 个用例（`unittest discover` 零发现时的退出码随版本而异：Python 3.12
  起为 5，更早版本为 0——所以必须独立判 `Ran N tests`，不能只信退出码）；
* 用例数低于每目录下限（默认 3）；
* 解析不到 `Ran N tests` 行（输出格式变了，不按 0 猜）；
* 找不到可用的 Python 解释器或版本低于 3.8（本脚本自身就在解释器里跑，所以
  这一条落在 `sys.executable` 为空时；`py -3` 缺失则连进程都起不来，验收工具
  按非零退出判失败）。

为什么清单写死、不用通配：通配会让"某个 Demo 的测试目录整体消失"变成静默通过。
为什么带 `-B` 且设 `PYTHONDONTWRITEBYTECODE=1`：单测默认在示例目录写
`__pycache__/`，而 T064 的干净检出检查要求工作区干净——这里不靠 `.gitignore`
兜底；脚本跑完还会复核一遍，真出现字节码目录按失败报出。

Why a fixed list instead of a glob: a glob turns "a demo's test directory vanished"
into a silent pass. Why `-B`: bytecode caches would dirty the worktree that the
clean-checkout gate (T064) judges; `.gitignore` is not a substitute here.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

# 每目录用例数下限：低于它说明测试被删 / 被发现规则漏掉，不是"刚好够用"。
MIN_TESTS_PER_DIR = 3

# 固定清单（五个 Demo × `scripts/`），顺序与 `examples/README.md` 的表一致。
DEMO_SCRIPT_DIRS = (
    "examples/daily/github-radar/scripts",
    "examples/daily/repo-digest/scripts",
    "examples/daily/biz-daily/scripts",
    "examples/daily/docs-diff/scripts",
    "examples/daily/industry-brief/scripts",
)

RAN_PATTERN = re.compile(r"^Ran (\d+) tests? in ", re.MULTILINE)
BYTECODE_DIR = "__pycache__"


def decode(raw: bytes) -> str:
    """按 UTF-8 解码子进程输出；坏字节替换而不是抛错（判卷只认 ASCII 行）。"""
    return raw.decode("utf-8", errors="replace")


def bytecode_dirs(directory: Path) -> list[str]:
    """列出目录下残留的 `__pycache__`（含子目录），用于证明 `-B` 确实生效。"""
    return sorted(
        str(path)
        for path in directory.rglob(BYTECODE_DIR)
        if path.is_dir()
    )


def run_one(repo_root: Path, relative: str) -> tuple[bool, int | None, list[str]]:
    """跑一个目录，返回（是否通过、解析到的用例数、失败原因列表）。"""
    problems: list[str] = []
    directory = repo_root / relative
    if not directory.is_dir():
        return False, None, [f"{relative} 不是目录（清单写死，改名即失败）"]

    env = dict(os.environ)
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env["PYTHONIOENCODING"] = "utf-8"
    command = [
        sys.executable,
        "-B",
        "-m",
        "unittest",
        "discover",
        "-v",
        "-s",
        ".",
        "-p",
        "test_*.py",
        "-t",
        ".",
    ]
    completed = subprocess.run(
        command,
        cwd=directory,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        check=False,
    )
    output = decode(completed.stdout) + decode(completed.stderr)

    if completed.returncode != 0:
        problems.append(f"unittest 退出码 {completed.returncode}（非零即失败）")

    match = RAN_PATTERN.search(output)
    if match is None:
        problems.append("输出里没有 `Ran N tests` 行，无法确认实际跑了多少用例")
        ran = None
    else:
        ran = int(match.group(1))
        if ran == 0:
            problems.append("发现 0 个用例（退出码随 Python 版本而异，必须独立拦住）")
        elif ran < MIN_TESTS_PER_DIR:
            problems.append(
                f"仅 {ran} 个用例，低于每目录下限 {MIN_TESTS_PER_DIR}"
            )

    leftovers = bytecode_dirs(directory)
    if leftovers:
        problems.append(
            "残留字节码目录（`-B` 未生效？）：" + ", ".join(leftovers)
        )

    return not problems, ran, problems


def main() -> int:
    # 统一 UTF-8 输出：`.loop/runs` 与 CI 日志按 UTF-8 采集；Windows 旧代码页下
    # 用 replace 兜底，而不是在打印中文时报 UnicodeEncodeError。
    for stream in (sys.stdout, sys.stderr):
        if hasattr(stream, "reconfigure"):
            stream.reconfigure(encoding="utf-8", errors="replace")

    if not sys.executable:
        print("run-example-tests: 找不到可用的 Python 解释器", file=sys.stderr)
        return 1
    if sys.version_info < (3, 8):
        print(
            f"run-example-tests: Python 版本过低（{sys.version.split()[0]}，要求 >= 3.8）",
            file=sys.stderr,
        )
        return 1

    repo_root = Path(__file__).resolve().parents[1]
    print(f"run-example-tests: {len(DEMO_SCRIPT_DIRS)} 个目录，每目录下限 {MIN_TESTS_PER_DIR} 个用例")
    print(f"run-example-tests: 解释器 {sys.executable}（{sys.version.split()[0]}）")

    failures = 0
    total = 0
    for relative in DEMO_SCRIPT_DIRS:
        passed, ran, problems = run_one(repo_root, relative)
        summary = "无法解析用例数" if ran is None else f"Ran {ran} tests"
        if passed and ran is not None:
            print(f"PASS {relative}: {summary}")
            total += ran
            continue
        failures += 1
        print(f"FAIL {relative}: {summary}")
        for problem in problems:
            print(f"     - {problem}")

    if failures:
        # 两个流在 CI 里会合并采集；先刷标准输出，失败摘要才不会跑到前面。
        sys.stdout.flush()
        print(f"run-example-tests: {failures}/{len(DEMO_SCRIPT_DIRS)} 个目录未通过", file=sys.stderr)
        return 1

    print(
        f"run-example-tests: 全部 {len(DEMO_SCRIPT_DIRS)} 个目录通过（累计 {total} 个用例）"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
