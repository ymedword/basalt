#!/usr/bin/env python3
"""D1 `github-radar` 的业务脚本单测（颗粒度文档 §4.5.10 逐字保留）。

运行 / Run：

    cd examples/daily/github-radar/scripts
    py -3 -m unittest -v

前三个用例是模块卡 §4.5.10 的原文（断言逻辑逐条保真，含中文逆改点注释）；
其余用例补空结果、计数自洽、重复条目合并、CLI 退出码与产物写序。

The first three cases are the module card's §4.5.10 tests verbatim; the rest cover
the empty result, count arithmetic, duplicate merging and the CLI contract.
"""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import github_radar
from github_radar import ScriptError

NOW = "2026-10-25T09:30:00+08:00"

# 首跑：星数最高的两个会被推荐，星数最低的 `fringe/repo` 被 top N 裁掉但记进 observed。
# 这决定了第二次运行里谁还能算出星差——只有“有基线且此前未被推荐”的条目才有基线。
SEARCH_1 = [
    {
        "full_name": "old/repo",
        "stars": 100,
        "pushed_at": "2026-10-24T10:00:00Z",
        "html_url": "https://github.com/old/repo",
        "description": "首轮已推荐过的仓库",
    },
    {
        "full_name": "top/repo",
        "stars": 50,
        "pushed_at": "2026-10-23T10:00:00Z",
        "html_url": "https://github.com/top/repo",
    },
    {
        "full_name": "fringe/repo",
        "stars": 10,
        "pushed_at": "2026-10-22T10:00:00Z",
        "html_url": "https://github.com/fringe/repo",
    },
]

# 第二次运行：`old/repo` 在窗口内已推荐 → 去重；`fringe/repo` 有基线（10 → 52）→ 星差 42；
# `new/two` 首次出现 → 无星差。
SEARCH_2 = [
    {
        "full_name": "old/repo",
        "stars": 140,
        "pushed_at": "2026-10-25T08:00:00Z",
        "html_url": "https://github.com/old/repo",
    },
    {
        "full_name": "fringe/repo",
        "stars": 52,
        "pushed_at": "2026-10-25T07:00:00Z",
        "html_url": "https://github.com/fringe/repo",
        "readme_url": "https://raw.githubusercontent.com/fringe/repo/trunk/README.md",
    },
    {
        "full_name": "new/two",
        "stars": 30,
        "pushed_at": "2026-10-25T06:00:00Z",
        "html_url": "https://github.com/new/two",
    },
]


def run_script(*, input, state, now=NOW, top=2, window_days=30):
    """测试入口：固定运行时钟，`top=2` 让窗口裁剪可判定。

    `top=2` 是刻意的，不是随手写的默认值：首跑必须裁掉一个仓库（`fringe/repo`），
    它才会“被观测但未被推荐”，第二次运行才有条目既在窗口外又带着基线——
    星差的存在性与去重的存在性因此在同一组输入里都能被断言。
    `top=2` is deliberate: trimming one repo on the first run is what leaves a
    baseline-bearing, not-yet-recommended entry for the second run to score.
    """
    return github_radar.radar(input, state, now=now, top=top, window_days=window_days)


class GithubRadarTest(unittest.TestCase):
    def test_first_run_has_no_baseline(self):
        out = run_script(input=SEARCH_1, state={})
        self.assertTrue(out["baseline_missing"])
        self.assertNotIn("star_delta", out["items"][0])  # 逆改点：首跑编造增长率 → 红

    def test_second_run_dedups_and_computes_delta(self):
        state = run_script(input=SEARCH_1, state={})["state"]
        out = run_script(input=SEARCH_2, state=state)
        names = [i["full_name"] for i in out["items"]]
        self.assertNotIn("old/repo", names)              # 逆改点：不去重 → 红
        self.assertEqual(out["items"][0]["star_delta"], 42) # 逆改点：模型口算 / 错误基线 → 红

    def test_invalid_input_fails_nonzero(self):
        with self.assertRaises(ScriptError):
            run_script(input={"bad": "shape"}, state={}) # 逆改点：静默补默认值 → 红

    def test_first_run_reports_what_it_recommended(self):
        out = run_script(input=SEARCH_1, state={})
        self.assertEqual([i["full_name"] for i in out["items"]], ["old/repo", "top/repo"])
        self.assertEqual(out["counts"], {"candidates": 3, "deduped": 0, "trimmed": 1, "recommended": 2})
        self.assertEqual(out["items"][0]["url"], "https://github.com/old/repo")
        # 缺 html_url 时按 owner/repo 推导仓库页地址；不猜默认分支、不拼 raw 地址。
        self.assertEqual(out["items"][1]["url"], "https://github.com/top/repo")
        self.assertNotIn("readme_url", out["items"][1])

    def test_second_run_keeps_counts_consistent(self):
        state = run_script(input=SEARCH_1, state={})["state"]
        out = run_script(input=SEARCH_2, state=state)
        counts = out["counts"]
        self.assertEqual(counts["candidates"], counts["deduped"] + counts["trimmed"] + counts["recommended"])
        self.assertEqual(counts, {"candidates": 3, "deduped": 1, "trimmed": 0, "recommended": 2})
        self.assertFalse(out["baseline_missing"])
        self.assertEqual(out["baseline_date"], "2026-10-25")
        # readme_url 由模型从 MCP 的 download_url 原样抄入，脚本只校验不改造。
        self.assertEqual(
            out["items"][0]["readme_url"],
            "https://raw.githubusercontent.com/fringe/repo/trunk/README.md",
        )
        self.assertEqual(out["items"][1]["full_name"], "new/two")
        self.assertNotIn("star_delta", out["items"][1])

    def test_empty_search_reports_no_new_items(self):
        out = run_script(input=[], state={})
        self.assertEqual(out["items"], [])
        self.assertTrue(out["baseline_missing"])
        self.assertIn("本次无新增", out["message"])
        self.assertEqual(out["source"], {"status": "empty", "candidates": 0})

    def test_duplicate_full_name_merges_to_highest_stars(self):
        duplicated = [
            {"full_name": " same/repo ", "stars": 10, "pushed_at": "2026-10-24T10:00:00Z"},
            {"full_name": "same/repo", "stargazers_count": 99, "pushed_at": "2026-10-24T11:00:00Z"},
            {"full_name": "same/repo", "stars": 7, "pushed_at": "2026-10-24T12:00:00Z"},
        ]
        out = run_script(input=duplicated, state={}, top=5)
        self.assertEqual(out["counts"]["candidates"], 1)
        self.assertEqual(out["items"][0]["full_name"], "same/repo")
        self.assertEqual(out["items"][0]["stars"], 99)

    def test_missing_pushed_at_is_rejected(self):
        with self.assertRaises(ScriptError):
            run_script(input=[{"full_name": "a/b", "stars": 1}], state={})

    def test_cli_writes_report_then_state_and_leaves_nothing_on_failure(self):
        with tempfile.TemporaryDirectory() as raw:
            workdir = Path(raw)
            search = workdir / "github_search.json"
            search.write_text(json.dumps(SEARCH_1, ensure_ascii=False), encoding="utf-8")
            state = workdir / "state" / "seen.json"
            first = workdir / "runs" / "first" / "report.json"

            code = github_radar.main(
                ["--input", str(search), "--state", str(state), "--output", str(first), "--now", NOW, "--top", "2"]
            )
            self.assertEqual(code, 0)
            report = json.loads(first.read_text(encoding="utf-8"))
            self.assertNotIn("state", report)  # 报告不复制状态，避免两处副本漂移
            self.assertTrue(state.exists())
            recommended = [entry["full_name"] for entry in report["items"]]
            self.assertEqual(recommended, ["old/repo", "top/repo"])

            # 第二次运行：同一份状态读回后，窗口内的仓库被去重。
            search.write_text(json.dumps(SEARCH_2, ensure_ascii=False), encoding="utf-8")
            second = workdir / "runs" / "second" / "report.json"
            code = github_radar.main(
                ["--input", str(search), "--state", str(state), "--output", str(second), "--now", NOW, "--top", "2"]
            )
            self.assertEqual(code, 0)
            report = json.loads(second.read_text(encoding="utf-8"))
            self.assertEqual(report["counts"]["deduped"], 1)

            # 输入非法：非零退出，且不产生新报告、不动已有状态。
            state_before = state.read_bytes()
            broken = workdir / "broken.json"
            broken.write_text(json.dumps({"bad": "shape"}), encoding="utf-8")
            third = workdir / "runs" / "third" / "report.json"
            code = github_radar.main(
                ["--input", str(broken), "--state", str(state), "--output", str(third), "--now", NOW]
            )
            self.assertEqual(code, 1)
            self.assertFalse(third.exists())
            self.assertEqual(state.read_bytes(), state_before)


if __name__ == "__main__":
    unittest.main()
