#!/usr/bin/env python3
"""D2 `repo-digest` 的业务脚本单测（颗粒度文档 §2.4.5 D2 判据）。

运行 / Run：

    cd examples/daily/repo-digest/scripts
    py -3 -m unittest -v

覆盖三条不变量：数字只由脚本算、缺字段标"待确认"、游标只前进到已处理项。
`逆改点` 注释标出每个用例对应的实现级变化——把那条改回去，这个用例就该变红。

Covers the three invariants from the module card: counts come from the script, an
unconfirmable field is reported instead of guessed, and the cursor never advances past
an item that was not processed.
"""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

import repo_digest
from repo_digest import ScriptError

# 固定运行时钟：2026-10-25T08:45:00+08:00 = 2026-10-25T00:45:00Z。
NOW = "2026-10-25T08:45:00+08:00"
PREV = "2026-10-24T08:00:00Z"
# 状态文件里的时间统一规范化成带偏移的 ISO 8601：输入写 `Z`，落盘是 `+00:00`。
PREV_OUT = "2026-10-24T08:00:00+00:00"


def pr(number, *, updated, created="2026-10-24T00:00:00Z", state="open", **extra):
    item = {
        "number": number,
        "title": f"PR {number}",
        "state": state,
        "draft": False,
        "created_at": created,
        "updated_at": updated,
        "html_url": f"https://example.invalid/pull/{number}",
    }
    item.update(extra)
    return item


def issue(number, *, updated, created="2026-10-24T00:00:00Z", state="open", comments=1, **extra):
    item = {
        "number": number,
        "title": f"Issue {number}",
        "state": state,
        "created_at": created,
        "updated_at": updated,
        "comments": comments,
        "html_url": f"https://example.invalid/issues/{number}",
    }
    item.update(extra)
    return item


def release(ident, *, updated, draft=False, prerelease=False, published="2026-10-24T07:00:00Z", **extra):
    item = {
        "id": ident,
        "tag_name": f"v{ident}",
        "draft": draft,
        "prerelease": prerelease,
        "published_at": published,
        "updated_at": updated,
        "html_url": f"https://example.invalid/releases/{ident}",
    }
    item.update(extra)
    return item


def prs_full():
    return [
        pr(1, updated="2026-10-24T09:00:00Z"),
        pr(2, updated="2026-10-24T10:00:00Z", created="2026-10-10T00:00:00Z"),
        pr(3, updated="2026-10-24T11:00:00Z", created="2026-10-01T00:00:00Z", draft=True),
        pr(4, updated="2026-10-24T12:00:00Z", state="closed", merged_at="2026-10-20T10:00:00Z"),
        pr(5, updated="2026-10-24T13:00:00Z", state="closed", merged_at=None),
        pr(6, updated="2026-10-24T14:00:00Z", state="closed"),
    ]


def issues_full():
    return [
        issue(11, updated="2026-10-24T09:30:00Z", comments=3),
        issue(12, updated="2026-10-24T10:30:00Z", created="2026-10-15T00:00:00Z", comments=0),
        issue(13, updated="2026-10-24T11:30:00Z", created="2026-09-01T00:00:00Z", comments=5),
        issue(14, updated="2026-10-24T12:30:00Z", state="closed", comments=2),
        issue(15, updated="2026-10-24T13:30:00Z", comments=None),
    ]


def releases_full():
    return [
        release(1001, updated="2026-10-24T08:00:00Z"),
        release(1002, updated="2026-10-24T09:00:00Z", prerelease=True),
        release(1003, updated="2026-10-24T10:00:00Z", draft=True, published=None),
        {k: v for k, v in release(1004, updated="2026-10-24T11:00:00Z").items() if k != "published_at"},
    ]


def run_digest(*, pull_requests, issues=None, releases=None, cursor=None, now=NOW):
    return repo_digest.digest(
        pull_requests,
        issues if issues is not None else [],
        releases if releases is not None else [],
        cursor,
        now=now,
    )


class RepoDigestTest(unittest.TestCase):
    def test_first_run_treats_everything_as_new(self):
        result = run_digest(pull_requests=prs_full(), issues=issues_full(), releases=releases_full())
        report = result["report"]
        self.assertEqual(report["cursor"]["previous"], {"pull_requests": None, "issues": None, "releases": None})
        self.assertEqual(report["sources"]["pull_requests"]["processed"], 6)
        self.assertEqual(report["sources"]["issues"]["processed"], 5)
        self.assertEqual(report["sources"]["releases"]["processed"], 4)
        self.assertEqual(report["cursor"]["next"]["pull_requests"], "2026-10-24T14:00:00+00:00")
        self.assertTrue(report["cursor"]["advanced"]["pull_requests"])
        # 逆改点：首跑既有的 `prev=None` 分支写成"没游标就跳过全部" → 红

    def test_second_run_with_same_inputs_reports_no_new_items(self):
        first = run_digest(pull_requests=prs_full(), issues=issues_full(), releases=releases_full())
        second = run_digest(
            pull_requests=prs_full(), issues=issues_full(), releases=releases_full(),
            cursor=first["state"],
        )
        report = second["report"]
        self.assertEqual(report["sources"]["pull_requests"]["processed"], 0)
        self.assertEqual(report["followups"], [])
        self.assertEqual(report["unconfirmed"], [])
        self.assertIn("本次无新增", report["message"])
        # 逆改点：比较写成 `>=` 而不是 `>` → 已处理条目被重复计入 → 红
        self.assertFalse(report["cursor"]["advanced"]["pull_requests"])
        self.assertEqual(second["state"], first["state"])

    def test_missing_merged_at_is_unconfirmed(self):
        result = run_digest(pull_requests=[pr(4, updated="2026-10-24T12:00:00Z", state="closed",
                                               merged_at="2026-10-20T10:00:00Z"),
                                           pr(6, updated="2026-10-24T14:00:00Z", state="closed")])
        report = result["report"]
        counts = report["sources"]["pull_requests"]["counts"]
        self.assertEqual(counts["merged"], 1)
        self.assertEqual(counts["unknown"], 1)
        self.assertEqual(counts["with_unconfirmed_fields"], 1)
        # 逆改点：把"缺 merged_at 的 closed PR"当 closed_unmerged（或 merged）→ 红
        self.assertNotIn("closed_unmerged", counts)
        self.assertEqual(
            report["unconfirmed"],
            [{"source": "pull_requests", "number": 6, "title": "PR 6",
              "field": "merged_at", "reason": "missing"}],
        )

    def test_explicit_null_merged_at_is_closed_unmerged(self):
        result = run_digest(pull_requests=[pr(5, updated="2026-10-24T13:00:00Z", state="closed", merged_at=None)])
        report = result["report"]
        self.assertEqual(report["sources"]["pull_requests"]["counts"], {"closed_unmerged": 1, "with_unconfirmed_fields": 0})
        self.assertEqual(report["unconfirmed"], [])
        # 逆改点：把显式 null 也当"缺字段" → 红

    def test_cursor_does_not_advance_past_unprocessed_item(self):
        broken = {"title": "缺 number 的 PR", "state": "open", "draft": False,
                  "created_at": "2026-10-10T00:00:00Z", "updated_at": "2026-10-24T10:00:00Z"}
        items = [
            pr(1, updated="2026-10-24T09:00:00Z"),
            broken,
            pr(3, updated="2026-10-24T11:00:00Z"),
        ]
        first = run_digest(pull_requests=items, cursor={"schema_version": 1, "sources": {"pull_requests": PREV}})
        report = first["report"]
        source = report["sources"]["pull_requests"]
        self.assertEqual(source["processed"], 2)
        self.assertEqual(source["held_back"], [{"number": None, "field": "number", "reason": "missing_or_invalid",
                                                "updated_at": "2026-10-24T10:00:00+00:00"}])
        self.assertEqual(source["cursor_held"], "unprocessed_item")
        self.assertEqual(source["cursor_limit"], "2026-10-24T10:00:00+00:00")
        # 逆改点：游标直接取 max(已处理) = 11:00 → 缺 number 的条目永远读不到 → 红
        self.assertEqual(report["cursor"]["next"]["pull_requests"], "2026-10-24T09:00:00+00:00")

        # 修好那条以后，它必须补进下一次日报（游标没越过它）；代价是同一来源里更晚的
        # 已处理条目会被重复计入一次——宁可重复，不可漏读。
        fixed = dict(broken, number=2)
        second = run_digest(pull_requests=[items[0], fixed, items[2]], cursor=first["state"])
        report = second["report"]
        self.assertEqual(report["sources"]["pull_requests"]["held_back"], [])
        self.assertEqual(report["sources"]["pull_requests"]["processed"], 2)
        self.assertEqual([(entry["source"], entry["number"], entry["reason"]) for entry in report["followups"]],
                         [("pull_requests", 2, "stale_open")])
        self.assertEqual(second["state"]["sources"]["pull_requests"], "2026-10-24T11:00:00+00:00")

    def test_cursor_tie_with_held_item_is_not_advanced(self):
        broken = {"title": "缺 number 的 PR", "state": "open", "draft": False,
                  "created_at": "2026-10-20T00:00:00Z", "updated_at": "2026-10-24T10:00:00Z"}
        items = [pr(1, updated="2026-10-24T10:00:00Z"), broken]
        result = run_digest(pull_requests=items, cursor={"schema_version": 1, "sources": {"pull_requests": PREV}})
        report = result["report"]
        # 已处理项与受阻项同一时间：游标必须停在 PREV，不能停在 10:00——
        # 否则下一次 `updated_at > prev` 为假，受阻项被当成已处理而永久跳过。
        # 逆改点：上限比较用 `<` 改成 `<=` → 游标到 10:00 → 红
        self.assertEqual(report["cursor"]["next"]["pull_requests"], PREV_OUT)
        self.assertEqual(report["sources"]["pull_requests"]["processed"], 1)

    def test_unparseable_held_time_freezes_the_cursor(self):
        broken = {"number": 2, "title": "时间坏掉的 PR", "state": "open", "draft": False,
                  "created_at": "2026-10-20T00:00:00Z", "updated_at": "not-a-time"}
        items = [pr(1, updated="2026-10-24T12:00:00Z"), broken]
        result = run_digest(pull_requests=items, cursor={"schema_version": 1, "sources": {"pull_requests": PREV}})
        report = result["report"]
        source = report["sources"]["pull_requests"]
        self.assertEqual(source["cursor_held"], "unparseable_updated_at")
        self.assertIsNone(source["cursor_limit"])
        # 逆改点：无时间的受阻项被忽略、游标照常推进到 12:00 → 红
        self.assertEqual(report["cursor"]["next"]["pull_requests"], PREV_OUT)
        self.assertEqual(source["held_back"][0]["reason"], "unparseable")

    def test_followups_for_prs_and_issues(self):
        result = run_digest(pull_requests=prs_full(), issues=issues_full())
        followups = [(entry["source"], entry["number"], entry["reason"]) for entry in result["report"]["followups"]]
        self.assertEqual(
            followups,
            [
                ("pull_requests", 2, "stale_open"),
                ("issues", 13, "long_open"),
                ("issues", 12, "unanswered_open"),
            ],
        )
        ages = [entry["age_days"] for entry in result["report"]["followups"]]
        self.assertEqual(ages, [15, 54, 10])
        # 逆改点：草稿 PR 不做例外 → #3 混进待跟进 → 红
        self.assertNotIn(3, [entry[1] for entry in followups])

    def test_release_buckets_and_missing_published_at(self):
        result = run_digest(pull_requests=[], releases=releases_full())
        report = result["report"]
        counts = report["sources"]["releases"]["counts"]
        self.assertEqual(counts, {"draft": 1, "prerelease": 1, "published": 1,
                                  "unknown": 1, "with_unconfirmed_fields": 1})
        self.assertEqual(
            report["unconfirmed"],
            [{"source": "releases", "number": 1004, "title": "v1004",
              "field": "published_at", "reason": "missing"}],
        )
        # 逆改点：草稿 release 计入 published → 红

    def test_release_without_updated_at_uses_published_at(self):
        # 真实 GitHub 的 release 对象没有 `updated_at`（MCP `list_releases` 的
        # 字段清单里也没有），发布事件的唯一时间锚是 `published_at`。
        item = {k: v for k, v in release(1005, updated="2026-10-24T08:00:00Z",
                                         published="2026-10-24T09:00:00Z").items()
                if k != "updated_at"}
        result = run_digest(
            pull_requests=[], releases=[item],
            cursor={"schema_version": repo_digest.SCHEMA_VERSION, "sources": {"releases": PREV}},
        )
        report = result["report"]
        self.assertEqual(report["sources"]["releases"]["processed"], 1)
        self.assertEqual(report["sources"]["releases"]["held_back"], [])
        self.assertEqual(report["cursor"]["next"]["releases"], "2026-10-24T09:00:00+00:00")
        # 逆改点：release 只认 `updated_at` → 真实 GitHub 的 release 全部被拒 → 红

    def test_release_with_unparseable_updated_at_is_held_back(self):
        item = release(1006, updated="not-a-timestamp", published="2026-10-24T09:00:00Z")
        report = run_digest(pull_requests=[], releases=[item])["report"]
        self.assertEqual(report["sources"]["releases"]["processed"], 0)
        self.assertEqual(
            report["sources"]["releases"]["held_back"],
            [{"number": 1006, "field": "updated_at", "reason": "unparseable", "updated_at": None}],
        )
        # 逆改点：解析失败也退回 published_at → 脏值被发布时间掩盖 → 红

    def test_state_case_is_normalized(self):
        # 实测 github-mcp-server 2.0.3：Issue 的 state 是大写 GraphQL 枚举（OPEN），
        # PR 是小写。大小写不改变语义，必须归一化后再分桶。
        result = run_digest(
            pull_requests=[pr(7, updated="2026-10-24T09:00:00Z", state="OPEN")],
            issues=[issue(17, updated="2026-10-24T09:30:00Z", state="CLOSED")],
        )
        report = result["report"]
        self.assertEqual(report["sources"]["pull_requests"]["counts"],
                         {"open": 1, "with_unconfirmed_fields": 0})
        self.assertEqual(report["sources"]["issues"]["counts"],
                         {"closed": 1, "with_unconfirmed_fields": 0})
        self.assertEqual(report["unconfirmed"], [])
        # 逆改点：只认小写 state → 真实 Issue 全部落 unknown → 红

    def test_issue_without_comments_is_unconfirmed(self):
        report = run_digest(pull_requests=[], issues=[issue(15, updated="2026-10-24T13:30:00Z", comments=None)])["report"]
        self.assertEqual(report["sources"]["issues"]["counts"], {"open": 1, "with_unconfirmed_fields": 1})
        self.assertEqual(report["unconfirmed"][0]["field"], "comments")
        self.assertEqual(report["followups"], [])
        # 逆改点：comments 缺失按 0 计 → 老 issue 会被误判 unanswered_open → 红

    def test_counts_agree_with_lists(self):
        report = run_digest(pull_requests=prs_full(), issues=issues_full(), releases=releases_full())["report"]
        for source, data in report["sources"].items():
            with self.subTest(source=source):
                buckets = {name: value for name, value in data["counts"].items() if name != "with_unconfirmed_fields"}
                self.assertEqual(sum(buckets.values()), data["processed"])
                self.assertEqual(data["processed"] + len(data["held_back"]), data["new"])
                flagged = {entry["number"] for entry in report["unconfirmed"] if entry["source"] == source}
                self.assertEqual(data["counts"]["with_unconfirmed_fields"], len(flagged))

    def test_invalid_input_fails(self):
        with self.assertRaises(ScriptError):
            run_digest(pull_requests={"bad": "shape"})  # 逆改点：静默补默认值 → 红
        with self.assertRaises(ScriptError):
            run_digest(pull_requests=[], cursor={"schema_version": 99, "sources": {}})
        with self.assertRaises(ScriptError):
            run_digest(pull_requests=[], cursor={"schema_version": 1, "sources": {"nope": PREV}})
        with self.assertRaises(ScriptError):
            run_digest(pull_requests=[], cursor={"schema_version": 1, "sources": {"issues": "2026-10-24"}})

    def test_cli_writes_digest_then_cursor_and_leaves_nothing_on_failure(self):
        with tempfile.TemporaryDirectory() as raw:
            workdir = Path(raw)
            paths = {}
            for name, payload in (("pull_requests", prs_full()), ("issues", issues_full()), ("releases", releases_full())):
                paths[name] = workdir / f"{name}.json"
                paths[name].write_text(json.dumps(payload, ensure_ascii=False), encoding="utf-8")
            state = workdir / "state" / "cursor.json"
            first = workdir / "runs" / "first" / "digest.json"

            code = repo_digest.main([
                "--pull-requests", str(paths["pull_requests"]),
                "--issues", str(paths["issues"]),
                "--releases", str(paths["releases"]),
                "--state", str(state), "--output", str(first), "--now", NOW,
            ])
            self.assertEqual(code, 0)
            report = json.loads(first.read_text(encoding="utf-8"))
            self.assertNotIn("state", report)  # 报告不复制状态，避免两处副本漂移
            self.assertTrue(state.exists())
            self.assertEqual(report["sources"]["pull_requests"]["counts"]["unknown"], 1)

            before = state.read_bytes()
            broken = workdir / "broken.json"
            broken.write_text(json.dumps({"bad": "shape"}), encoding="utf-8")
            third = workdir / "runs" / "third" / "digest.json"
            code = repo_digest.main([
                "--pull-requests", str(broken),
                "--issues", str(paths["issues"]),
                "--releases", str(paths["releases"]),
                "--state", str(state), "--output", str(third), "--now", NOW,
            ])
            self.assertEqual(code, 1)
            self.assertFalse(third.exists())
            # 逆改点：先写游标再写报告 → 失败时游标已被推进 → 红
            self.assertEqual(state.read_bytes(), before)


if __name__ == "__main__":
    unittest.main()
