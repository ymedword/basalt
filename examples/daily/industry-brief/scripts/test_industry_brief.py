#!/usr/bin/env python3
"""D5 `industry-brief` 的业务脚本单测（颗粒度文档 §4.4「D5 双 MCP」的落点）。

运行 / Run：

    cd examples/daily/industry-brief/scripts
    py -3 -B -m unittest -v

覆盖：跨查询来源去重、内容哈希与访问时间、未抓正文只作线索、抓取失败与空正文、
日期只认 ISO、来源排序、空结果不编造、页面越界 / 重复 / 非法 URL 拒收、
`--verify-report` 的引用校验（线索被当结论引用必须非零退出）、CLI 零写入与可复现。

夹具直接从 `../fixtures/search.json` / `pages.json` 读**真实内容**：断言与夹具不得
各自漂移。`-B` 保证不产生 `__pycache__`（发布干净检出要求工作区干净）。
"""

from __future__ import annotations

import contextlib
import hashlib
import io
import json
import tempfile
import unittest
from pathlib import Path

import industry_brief
from industry_brief import ScriptError

FIXTURES = Path(__file__).resolve().parents[1] / "fixtures"
NOW = "2026-10-25T09:30:00+08:00"
TOPIC = "企业级 AI Agent 平台"


def fixture(name: str) -> dict:
    return json.loads((FIXTURES / name).read_text(encoding="utf-8"))


def search_of(*queries: tuple[str, list[dict]]) -> dict:
    return {"queries": [{"query": text, "results": results} for text, results in queries]}


def result(url: str, *, title: str = "示例文章", page_age: str | None = None) -> dict:
    entry = {"title": title, "url": url}
    if page_age is not None:
        entry["page_age"] = page_age
    return entry


def page(url: str, *, content: str = "正文一段。", status: int = 200, error: str = "") -> dict:
    entry = {"url": url, "status": status, "content": content}
    if error:
        entry["error"] = error
    return entry


def build(search: dict, pages: dict, *, now: str = NOW, topic: str = TOPIC) -> dict:
    return industry_brief.build_brief(search, pages, now=now, topic=topic)


def run_cli(search: dict, pages: dict, *, root: Path, output: Path, report: str | None = None,
            now: str = NOW) -> tuple[int, str, str]:
    search_path = root / "search.json"
    pages_path = root / "pages.json"
    search_path.write_text(json.dumps(search, ensure_ascii=False), encoding="utf-8")
    pages_path.write_text(json.dumps(pages, ensure_ascii=False), encoding="utf-8")
    argv = [
        "--search", str(search_path),
        "--pages", str(pages_path),
        "--output", str(output),
        "--now", now,
        "--topic", TOPIC,
    ]
    if report is not None:
        report_path = root / "brief.md"
        report_path.write_text(report, encoding="utf-8")
        argv += ["--verify-report", str(report_path)]
    out, err = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
        code = industry_brief.main(argv)
    return code, out.getvalue(), err.getvalue()


class IndustryBriefTest(unittest.TestCase):
    def test_fixture_counts_are_conserved(self):
        brief = build(fixture("search.json"), fixture("pages.json"))
        counts = brief["counts"]
        self.assertEqual(counts["queries"], 3)
        self.assertEqual(counts["results"], 10)
        self.assertEqual(counts["sources"], 8)
        self.assertEqual(counts["duplicates"], 2)
        self.assertEqual(counts["duplicates"], counts["results"] - counts["sources"])
        self.assertEqual(counts["fetched"], 5)
        self.assertEqual(counts["leads"], 3)
        self.assertEqual(counts["fetched"] + counts["leads"], counts["sources"])
        self.assertEqual(counts["fetch_failures"], 1)
        self.assertEqual(counts["missing_dates"], 1)
        self.assertIn("命中 10 条结果（去重 2 条）", brief["message"])

    def test_dedupe_across_queries_merges_sources(self):
        search = search_of(
            ("q1", [result("https://example.com/a", page_age="2026-10-20")]),
            ("q2", [result("https://example.com/a"), result("https://example.com/b")]),
        )
        brief = build(search, {"pages": []})
        self.assertEqual([s["url"] for s in brief["sources"]],
                         ["https://example.com/a", "https://example.com/b"])
        merged = brief["sources"][0]
        self.assertEqual(merged["queries"], ["q1", "q2"])
        self.assertEqual(merged["published_at"], "2026-10-20")  # 首次命中的日期保留
        self.assertEqual(brief["counts"]["duplicates"], 1)

    def test_dedupe_ignores_fragment(self):
        search = search_of(("q1", [
            result("https://example.com/a#top"),
            result("https://example.com/a#bottom"),
        ]))
        brief = build(search, {"pages": []})
        self.assertEqual(brief["counts"]["sources"], 1)
        self.assertEqual(brief["sources"][0]["url"], "https://example.com/a")

    def test_fetched_sources_are_citable_with_access_time(self):
        target = "https://example.com/analysis/audit-trail-design"
        pages = fixture("pages.json")
        body = next(p for p in pages["pages"] if p["url"] == target)["content"]
        brief = build(fixture("search.json"), pages)
        fetched = [s for s in brief["sources"] if s["status"] == industry_brief.STATUS_FETCHED]
        self.assertEqual(len(fetched), 5)
        record = fetched[0]
        self.assertTrue(record["citable"])
        self.assertIn(record["url"], brief["citable_urls"])
        self.assertEqual(record["accessed_at"], NOW)
        self.assertEqual(record["note"], "")
        self.assertTrue(record["excerpt"])
        self.assertNotIn("\n", record["excerpt"])
        self.assertLessEqual(len(record["excerpt"]), industry_brief.EXCERPT_CHARS)
        by_url = {s["url"]: s for s in brief["sources"]}
        checked = by_url[target]
        self.assertEqual(checked["content_sha256"], hashlib.sha256(body.encode("utf-8")).hexdigest())
        self.assertEqual(checked["content_bytes"], len(body.encode("utf-8")))

    def test_unfetched_urls_are_leads_not_citable(self):
        brief = build(fixture("search.json"), fixture("pages.json"))
        by_url = {s["url"]: s for s in brief["sources"]}
        lead = by_url["https://example.com/vendor/agent-os-comparison"]
        self.assertEqual(lead["status"], industry_brief.STATUS_LEAD)
        self.assertFalse(lead["citable"])
        self.assertIsNone(lead["accessed_at"])
        self.assertIsNone(lead["content_sha256"])
        self.assertEqual(lead["note"], "未抓取正文：只作线索")
        self.assertIn(lead["url"], brief["lead_urls"])
        self.assertNotIn(lead["url"], brief["citable_urls"])

    def test_fetch_failure_stays_a_lead(self):
        search = search_of(("q1", [result("https://example.com/gone")]))
        pages = {"pages": [page("https://example.com/gone", status=404, error="HTTP 404 Not Found", content="")]}
        brief = build(search, pages)
        record = brief["sources"][0]
        self.assertEqual(record["status"], industry_brief.STATUS_LEAD)
        self.assertFalse(record["citable"])
        self.assertEqual(record["note"], "抓取失败：HTTP 404 Not Found")
        self.assertEqual(brief["failures"], [{"url": "https://example.com/gone", "reason": "HTTP 404 Not Found"}])
        self.assertEqual(brief["counts"]["fetch_failures"], 1)

    def test_empty_body_is_a_fetch_failure(self):
        search = search_of(("q1", [result("https://example.com/blank")]))
        pages = {"pages": [page("https://example.com/blank", content="  \n\t ")]}
        brief = build(search, pages)
        record = brief["sources"][0]
        self.assertFalse(record["citable"])
        self.assertEqual(record["note"], "抓取失败：正文为空")
        self.assertEqual(brief["failures"][0]["reason"], "正文为空")

    def test_published_date_comes_from_page_age_only(self):
        search = search_of(("q1", [
            result("https://example.com/iso", page_age="2026-10-23T08:00:00Z"),
            result("https://example.com/date", page_age="2026-10-19"),
            result("https://example.com/human", page_age="3 days ago"),
            result("https://example.com/bad", page_age="2026-13-40"),
            result("https://example.com/missing"),
        ]))
        brief = build(search, {"pages": []})
        by_url = {s["url"]: s for s in brief["sources"]}
        self.assertEqual(by_url["https://example.com/iso"]["published_at"], "2026-10-23")
        self.assertEqual(by_url["https://example.com/date"]["published_at"], "2026-10-19")
        # 自然语言与非法日期都不猜，一律「未确认」。
        self.assertIsNone(by_url["https://example.com/human"]["published_at"])
        self.assertIsNone(by_url["https://example.com/bad"]["published_at"])
        self.assertIsNone(by_url["https://example.com/missing"]["published_at"])
        self.assertEqual(brief["counts"]["missing_dates"], 3)

    def test_sources_are_sorted_by_url(self):
        search = search_of(("q1", [
            result("https://example.com/z"),
            result("https://example.com/a"),
            result("https://example.com/m"),
        ]))
        brief = build(search, {"pages": []})
        urls = [s["url"] for s in brief["sources"]]
        self.assertEqual(urls, sorted(urls))

    def test_pages_outside_search_are_refused(self):
        search = search_of(("q1", [result("https://example.com/a")]))
        pages = {"pages": [page("https://example.com/other")]}
        with self.assertRaises(ScriptError) as ctx:
            build(search, pages)
        self.assertIn("不在检索结果里", str(ctx.exception))

    def test_duplicate_page_entry_is_refused(self):
        search = search_of(("q1", [result("https://example.com/a")]))
        pages = {"pages": [page("https://example.com/a"), page("https://example.com/a")]}
        with self.assertRaises(ScriptError) as ctx:
            build(search, pages)
        self.assertIn("同一 URL 出现多次", str(ctx.exception))

    def test_non_http_url_is_refused(self):
        search = search_of(("q1", [result("file:///etc/passwd")]))
        with self.assertRaises(ScriptError) as ctx:
            build(search, {"pages": []})
        self.assertIn("http(s) 绝对地址", str(ctx.exception))

    def test_url_with_credentials_is_refused(self):
        search = search_of(("q1", [result("https://user:secret@example.com/a")]))
        with self.assertRaises(ScriptError) as ctx:
            build(search, {"pages": []})
        self.assertIn("不得包含用户名或密码", str(ctx.exception))

    def test_missing_results_array_is_refused(self):
        with self.assertRaises(ScriptError) as ctx:
            build({"queries": [{"query": "q1"}]}, {"pages": []})
        self.assertIn("缺少 results 数组", str(ctx.exception))

    def test_empty_search_produces_no_conclusions(self):
        brief = build({"queries": []}, {"pages": []})
        self.assertEqual(brief["counts"]["sources"], 0)
        self.assertEqual(brief["sources"], [])
        self.assertEqual(brief["citable_urls"], [])
        self.assertEqual(brief["message"], "本次未获取到来源：不产出任何结论（不编造）")

    def test_verify_report_accepts_fetched_citations(self):
        brief = build(fixture("search.json"), fixture("pages.json"))
        text = (
            "# 行业简报\n\n"
            "- 审计与留痕是首要门槛。 [来源: https://example.com/reports/agent-platform-observability-2026]\n"
        )
        self.assertEqual(industry_brief.verify_report_text(text, brief), [])

    def test_verify_report_rejects_lead_citation(self):
        brief = build(fixture("search.json"), fixture("pages.json"))
        text = "- 采用率七成。 [来源: https://example.com/news/enterprise-agent-adoption]\n"
        problems = industry_brief.verify_report_text(text, brief)
        self.assertEqual(len(problems), 1)
        self.assertIn("未抓正文的 URL", problems[0])

    def test_verify_report_rejects_unknown_url(self):
        brief = build(fixture("search.json"), fixture("pages.json"))
        problems = industry_brief.verify_report_text("- 结论。 [来源: https://example.com/unknown]\n", brief)
        self.assertEqual(len(problems), 1)
        self.assertIn("不存在的来源", problems[0])

    def test_verify_report_requires_a_citation_when_sources_exist(self):
        brief = build(fixture("search.json"), fixture("pages.json"))
        problems = industry_brief.verify_report_text("# 行业简报\n\n没有任何引用的结论。\n", brief)
        self.assertEqual(len(problems), 1)
        self.assertIn("没有任何", problems[0])

    def test_verify_report_allows_citation_free_report_without_sources(self):
        brief = build({"queries": []}, {"pages": []})
        self.assertEqual(industry_brief.verify_report_text("# 行业简报\n\n本次没有来源。\n", brief), [])

    def test_cli_writes_brief_json_and_prints_summary(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            output = root / "runs" / "first" / "brief.json"
            code, out, err = run_cli(fixture("search.json"), fixture("pages.json"), root=root, output=output)
            self.assertEqual((code, err), (0, ""))
            self.assertTrue(output.exists())
            summary = json.loads(out)
            self.assertEqual(summary["counts"]["sources"], 8)
            self.assertEqual(summary["report"]["sha256"],
                             hashlib.sha256(output.read_bytes()).hexdigest())

    def test_cli_verify_failure_exits_nonzero_after_writing_brief(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            output = root / "runs" / "second" / "brief.json"
            report = "- 引用一个线索。 [来源: https://example.com/vendor/agent-os-comparison]\n"
            code, out, err = run_cli(fixture("search.json"), fixture("pages.json"),
                                     root=root, output=output, report=report)
            self.assertEqual(code, 1)
            self.assertEqual(out, "")
            self.assertIn("未抓正文的 URL", err)
            self.assertTrue(output.exists())  # 事实文件照写，只有正文引用不合格

    def test_cli_failure_writes_nothing(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            output = root / "runs" / "x" / "brief.json"
            code, _, err = run_cli({"queries": "not-a-list"}, {"pages": []}, root=root, output=output)
            self.assertEqual(code, 1)
            self.assertIn("缺少 queries 数组", err)
            self.assertFalse(output.exists())
            self.assertFalse(output.parent.exists())  # 父目录也不建

    def test_report_is_reproducible(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            first = root / "a" / "brief.json"
            second = root / "b" / "brief.json"
            run_cli(fixture("search.json"), fixture("pages.json"), root=root, output=first)
            run_cli(fixture("search.json"), fixture("pages.json"), root=root, output=second)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            third = root / "c" / "brief.json"
            run_cli(fixture("search.json"), fixture("pages.json"), root=root, output=third,
                    now="2026-10-25T09:31:00+08:00")
            self.assertNotEqual(first.read_bytes(), third.read_bytes())
            self.assertEqual(json.loads(third.read_text(encoding="utf-8"))["observed_at"],
                             "2026-10-25T09:31:00+08:00")


if __name__ == "__main__":
    unittest.main()
