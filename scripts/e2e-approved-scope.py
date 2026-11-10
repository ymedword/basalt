#!/usr/bin/env python3
"""Judge the full e2e suite against the approved release scope.

Background: the full `make e2e` suite contains the US-20 native container cases
(`TestUS20_GVisor_*` / `TestUS20_OCI_*`). They fail closed by design on any host
that does not provide a non-root Linux UID, the local-authorization marker, an
output tmpfs, a real Docker daemon and the runc/runsc runtimes — that fail-closed
gate is the US-20 acceptance rule and must not be weakened into a skip.

F-06 (user ruling, 2026-11-09) keeps every test in the suite but excludes exactly
these US-20 container cases, item by item, from this release's verification set;
they stay recorded as unverified. This script implements that ruling in CI with
an exact-name allowlist:

* any failure outside the allowlist -> red;
* a timeout / truncated suite -> red;
* a missing `binary_source=candidate` marker -> red (the release evidence must
  come from the built candidate, never from a possibly older prebuilt binary);
* an allowlisted failure whose output does not carry one of the documented
  environment-gate messages -> red (the exclusion only covers the gate itself,
  not an arbitrary US-20 regression).

Usage: python3 scripts/e2e-approved-scope.py --log <e2e-console.json> --exit-code <n>
       python3 scripts/e2e-approved-scope.py --self-test
"""

from __future__ import annotations

import argparse
import json
import sys

# F-06 批准排除的 US-20 原生容器用例（逐条点名，不按 `TestUS20_*` 通配）。
# The US-20 native container cases excluded item by item under the F-06 ruling.
APPROVED_EXCLUSIONS = frozenset(
    {
        "TestUS20_GVisor_ArtifactRoundTrip",
        "TestUS20_GVisor_ImageTamper",
        "TestUS20_GVisor_MCPBoundary",
        "TestUS20_GVisor_CodeExecution",
        "TestUS20_GVisor_NoNetwork",
        "TestUS20_GVisor_ReadOnlyRoot",
        "TestUS20_GVisor_NoSecrets",
        "TestUS20_GVisor_ArtifactRejectsSymlink",
        "TestUS20_GVisor_ArtifactLimits",
        "TestUS20_GVisor_ArtifactNestedDirCleanup",
        "TestUS20_GVisor_ArtifactOddNameCleanup",
        "TestUS20_GVisor_ResourceExhaustion",
        "TestUS20_OCI_ShellLifecycle",
        "TestUS20_OCI_ImageNotApproved",
        "TestUS20_OCI_ImageDigestMismatch",
        "TestUS20_OCI_Lifecycle",
        "TestUS20_OCI_CancelTimeout",
        "TestUS20_OCI_NoNetwork",
        "TestUS20_OCI_NoHostSecrets",
        "TestUS20_OCI_ReadOnlyRoot",
        "TestUS20_OCI_ResourceLimits",
    }
)

# 环境闸门的固定文案：被排除的失败必须命中其中之一，其它失败原因不放行。
# The documented environment-gate messages; any other failure text is rejected.
GATE_MARKERS = (
    "真实 Linux 必验",
    "服务必须以非 root UID 验证",
    "缺本地隔离验收授权登记",
    "需要专用 tmpfs 验收挂载",
    "必验运行时缺失",
    "docker",
)

CANDIDATE_MARKER = "binary_source=candidate"
TIMEOUT_MARKER = "panic: test timed out"


def parse_log(text: str) -> tuple[dict[str, list[str]], bool, bool]:
    """Return failures per top-level test, candidate marker, timeout flag.

    每个顶层用例收集其后全部输出行（含子用例），供闸门文案核对。
    Collect every output line per top-level test (subtests included).
    """
    outputs: dict[str, list[str]] = {}
    failed: dict[str, list[str]] = {}
    candidate = False
    timed_out = False
    for line in text.splitlines():
        if CANDIDATE_MARKER in line:
            candidate = True
        if TIMEOUT_MARKER in line:
            timed_out = True
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        test = event.get("Test")
        if not test:
            continue
        top = test.split("/", 1)[0]
        chunk = event.get("Output") or ""
        outputs.setdefault(top, []).append(chunk)
        if event.get("Action") == "fail" and "/" not in test:
            failed.setdefault(top, []).append("")
    return {name: outputs.get(name, []) for name in failed}, candidate, timed_out


def judge(exit_code: int, failures: dict[str, list[str]], candidate: bool, timed_out: bool) -> tuple[bool, list[str]]:
    """Return (ok, reasons). `reasons` lists every rejection, one per line."""
    problems: list[str] = []
    if not candidate:
        problems.append(f"日志缺少 {CANDIDATE_MARKER} 标记：验收必须运行构建产物")
    if timed_out:
        problems.append("套件被 watchdog 截断（panic: test timed out）：判卷无效")
    unexpected = sorted(set(failures) - APPROVED_EXCLUSIONS)
    for name in unexpected:
        problems.append(f"未批准范围的失败：{name}")
    for name in sorted(set(failures) & APPROVED_EXCLUSIONS):
        text = "".join(failures[name])
        if not any(marker in text for marker in GATE_MARKERS):
            problems.append(f"被排除用例的失败原因不在环境闸门文案内：{name}")
    if exit_code == 0 and failures:
        problems.append("退出码为 0 却存在失败事件：日志与退出码不一致")
    if exit_code != 0 and not failures:
        problems.append(f"退出码 {exit_code} 且日志无失败事件：判卷无效")
    return (not problems), problems


def report(failures: dict[str, list[str]], problems: list[str]) -> None:
    excluded = sorted(set(failures) & APPROVED_EXCLUSIONS)
    print(f"e2e 失败顶层用例 {len(failures)} 条；其中按 F-06 批准排除 {len(excluded)} 条：")
    for name in excluded:
        print(f"  排除：{name}（US-20 原生容器环境闸门，保留未验证）")
    if problems:
        print("判卷失败：", file=sys.stderr)
        for problem in problems:
            print(f"  - {problem}", file=sys.stderr)
    else:
        print("判卷通过：失败集合与 F-06 批准范围一致。")


def self_test() -> int:
    """Exercise the judge on synthetic logs; exit non-zero on any mismatch."""
    def log(*events: str) -> str:
        return "\n".join(events) + "\n"

    def ev(test: str, action: str, output: str = "") -> str:
        return json.dumps({"Action": action, "Test": test, "Output": output})

    cases = [
        (
            "全绿",
            log('{"Action":"output","Output":"binary_source=candidate"}', ev("TestA", "pass")),
            0,
            True,
        ),
        (
            "批准排除项以闸门文案失败",
            log(
                '{"Action":"output","Output":"binary_source=candidate"}',
                ev("TestUS20_OCI_Lifecycle", "output", "服务必须以非 root UID 验证"),
                ev("TestUS20_OCI_Lifecycle", "fail"),
            ),
            1,
            True,
        ),
        (
            "未批准失败",
            log(
                '{"Action":"output","Output":"binary_source=candidate"}',
                ev("TestSomethingElse", "output", "boom"),
                ev("TestSomethingElse", "fail"),
            ),
            1,
            False,
        ),
        (
            "排除项非闸门原因",
            log(
                '{"Action":"output","Output":"binary_source=candidate"}',
                ev("TestUS20_OCI_Lifecycle", "output", "assertion broke"),
                ev("TestUS20_OCI_Lifecycle", "fail"),
            ),
            1,
            False,
        ),
        (
            "超时截断",
            log('{"Action":"output","Output":"binary_source=candidate"}', '{"Action":"output","Output":"panic: test timed out after 60m0s"}'),
            1,
            False,
        ),
        (
            "缺少候选标记",
            log(ev("TestA", "pass")),
            0,
            False,
        ),
        (
            "含子用例的排除项",
            log(
                '{"Action":"output","Output":"binary_source=candidate"}',
                ev("TestUS20_GVisor_ArtifactLimits/bytes", "output", "缺本地隔离验收授权登记"),
                ev("TestUS20_GVisor_ArtifactLimits/bytes", "fail"),
                ev("TestUS20_GVisor_ArtifactLimits", "fail"),
            ),
            1,
            True,
        ),
    ]
    failed = 0
    for name, text, code, want in cases:
        failures, candidate, timed_out = parse_log(text)
        ok, reasons = judge(code, failures, candidate, timed_out)
        if ok != want:
            failed += 1
            print(f"自检失败：{name}（期望 {'通过' if want else '拒绝'}，实际 {'通过' if ok else '拒绝'}）{reasons}", file=sys.stderr)
    if failed:
        return 1
    print(f"自检通过：{len(cases)} 个合成用例")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description="按 F-06 批准范围判卷完整 e2e 日志")
    parser.add_argument("--log", help="`make e2e` 的 console 日志（JSON 行）")
    parser.add_argument("--exit-code", type=int, help="`make e2e` 的退出码")
    parser.add_argument("--self-test", action="store_true", help="运行判卷器自检")
    args = parser.parse_args()
    if args.self_test:
        return self_test()
    if not args.log or args.exit_code is None:
        parser.error("--log 与 --exit-code 必填（或使用 --self-test）")
    with open(args.log, encoding="utf-8", errors="replace") as handle:
        text = handle.read()
    failures, candidate, timed_out = parse_log(text)
    ok, problems = judge(args.exit_code, failures, candidate, timed_out)
    report(failures, problems)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
