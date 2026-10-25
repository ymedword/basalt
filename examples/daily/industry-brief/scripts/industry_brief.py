#!/usr/bin/env python3
"""行业 / 竞品简报的来源去重、可引用性判定与引用校验（颗粒度文档 §3.5 / §4.4 D5）。

只用标准库。读模型落盘的两份 MCP 结果——Brave 检索（多次查询聚合）与 Fetch 抓取
（逐 URL 正文）——算出来源表，把每个来源分成两类：

* `fetched`：有正文，`citable=true`，可以作结论来源；
* `lead`：没有正文（没抓 / 抓失败 / 正文为空），`citable=false`，**只能列作线索**。

用法 / Usage：

    python industry_brief.py --search search.json --pages pages.json \
        --output runs/20261025T093000/brief.json --now 2026-10-25T09:30:00+08:00 \
        --topic 企业级 AI Agent 平台
    python industry_brief.py --search search.json --pages pages.json \
        --output runs/20261025T093000/brief.json --now 2026-10-25T09:30:00+08:00 \
        --verify-report runs/20261025T093000/brief.md

设计约束（Why）：

* **未抓正文的 URL 不作结论来源**（模块卡 §4.4 D5 的逆改点）。脚本把可引用集合
  显式写进 `brief.json`，`--verify-report` 再按同一集合检查简报正文的每一条
  `[来源: <url>]`——线索被当结论引用会非零退出，而不是靠模型自觉。
* **来源去重**：同一 URL 出现在多个查询里只算一个来源，`queries` 合并；去重数 =
  命中条数 - 去重后来源数，两个数字都进 `counts`，可复算。
* **日期只认 `page_age` 的 ISO 形态**：取日期部分；缺失或不可解析 → `null`，报告
  写「未确认」。不解析 `age` 里的自然语言（"3 days ago" 不可核对）。
* **访问时间**取下本 Run 的固定时钟（`--now`）：Fetch 结果不回传逐页时间戳，逐页
  时间只能靠模型口述，不可核对。
* 输入非法一律抛 `ScriptError`（CLI 非零退出），**不静默补默认值**，且**一个字节
  都不写**（连输出父目录都不创建）。

The script owns the source table and the citable / lead split. It never calls the
network: the model saves Brave and Fetch results to JSON, and this script turns them
into a brief, then optionally verifies that the written report cites only fetched
sources. Every count in the report is recomputable from the two input files.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import tempfile
from datetime import date, datetime
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

AGENT_NAME = "industry-brief"
SCHEMA_VERSION = 1
STATUS_FETCHED = "fetched"
STATUS_LEAD = "lead"
EXCERPT_CHARS = 200

# `[来源: https://…]`（中英文冒号都接受）；URL 到右方括号或空白为止。
CITATION_RE = re.compile(r"\[来源[:：]\s*(https?://[^\]\s]+)\s*\]")
# `page_age` 只接受以 `YYYY-MM-DD` 开头的 ISO 形态，后面跟不跟时间都行。
DATE_PREFIX = re.compile(r"^(\d{4}-\d{2}-\d{2})(?:[T ].*)?$")


class ScriptError(Exception):
    """输入不合规：CLI 非零退出，一个字节都不写。"""


# ---------------------------------------------------------------------------
# 基础工具
# ---------------------------------------------------------------------------


def force_utf8_stdio():
    """把 stdout / stderr 固定成 UTF-8：LocalRunner 按 UTF-8 解码子进程输出。"""
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if reconfigure is not None:
            reconfigure(encoding="utf-8")


def sha256_of(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(65536), b""):
            digest.update(chunk)
    return digest.hexdigest()


def parse_timestamp(value):
    """解析带时区偏移的 ISO 8601；`Z` 视为 `+00:00`。无时区 / 非法 → None。"""
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        return None
    return parsed


def parse_clock(value):
    if value is None:
        return datetime.now().astimezone()
    parsed = parse_timestamp(value)
    if parsed is None:
        raise ScriptError(f"--now 必须是带时区偏移的 ISO 8601 时间：{value!r}")
    return parsed


def load_json(path, *, what):
    try:
        raw = Path(path).read_text(encoding="utf-8")
    except FileNotFoundError as exc:
        raise ScriptError(f"{what} 不存在：{path}") from exc
    except OSError as exc:
        raise ScriptError(f"{what} 读取失败：{exc}") from exc
    try:
        return json.loads(raw)
    except json.JSONDecodeError as exc:
        raise ScriptError(f"{what} 不是合法 JSON（{path}）：{exc}") from exc


def write_atomic(path, payload):
    """同目录 mkstemp + os.replace：要么旧内容完整，要么新内容完整，没有半截文件。"""
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    data = json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n"
    fd, tmp = tempfile.mkstemp(dir=str(target.parent), prefix=".tmp-", suffix=".json")
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
            handle.write(data)
        os.replace(tmp, target)
    except BaseException:
        Path(tmp).unlink(missing_ok=True)
        raise


def normalize_url(value, *, what="URL"):
    """只接受不带凭据的 http(s) 绝对地址；去掉 `#fragment` 作为去重口径。"""
    if not isinstance(value, str) or not value.strip():
        raise ScriptError(f"{what} 必须是非空字符串")
    parts = urlsplit(value.strip())
    if parts.scheme not in ("http", "https"):
        raise ScriptError(f"{what} 必须是 http(s) 绝对地址：{value!r}")
    if not parts.netloc:
        raise ScriptError(f"{what} 缺少主机名：{value!r}")
    if parts.username or parts.password:
        raise ScriptError(f"{what} 不得包含用户名或密码：{value!r}")
    return urlunsplit((parts.scheme, parts.netloc, parts.path, parts.query, ""))


def published_date(value):
    """`page_age` → `YYYY-MM-DD`；缺失、非 ISO 或日期非法 → None（报告写「未确认」）。"""
    if not isinstance(value, str):
        return None
    match = DATE_PREFIX.match(value.strip())
    if not match:
        return None
    try:
        date.fromisoformat(match.group(1))
    except ValueError:
        return None
    return match.group(1)


def excerpt_of(text):
    """正文压成单行、截前 200 字：给报告做摘要用，不替代正文本身。"""
    collapsed = " ".join(text.split())
    return collapsed[:EXCERPT_CHARS]


# ---------------------------------------------------------------------------
# 输入校验
# ---------------------------------------------------------------------------


def parse_search(payload):
    """校验 Brave 检索结果，返回 `(entries, queries)`。空查询列表是合法的。"""
    if not isinstance(payload, dict):
        raise ScriptError('Brave 检索结果必须是 {"queries": [...]} 形态的对象')
    queries = payload.get("queries")
    if not isinstance(queries, list):
        raise ScriptError('Brave 检索结果缺少 queries 数组')
    entries = []
    query_texts = []
    for q_index, query in enumerate(queries, start=1):
        if not isinstance(query, dict):
            raise ScriptError(f"queries 第 {q_index} 项不是对象")
        text = query.get("query")
        if not isinstance(text, str) or not text.strip():
            raise ScriptError(f"queries 第 {q_index} 项的 query 必须是非空字符串")
        text = text.strip()
        results = query.get("results")
        if not isinstance(results, list):
            raise ScriptError(f"查询 {text!r} 缺少 results 数组")
        query_texts.append(text)
        for r_index, result in enumerate(results, start=1):
            where = f"查询 {text!r} 的第 {r_index} 条结果"
            if not isinstance(result, dict):
                raise ScriptError(f"{where}不是对象")
            title = result.get("title")
            if not isinstance(title, str) or not title.strip():
                raise ScriptError(f"{where}缺少 title")
            page_age = result.get("page_age")
            if page_age is not None and not isinstance(page_age, str):
                raise ScriptError(f"{where}的 page_age 必须是字符串或省略")
            entries.append(
                {
                    "query": text,
                    "title": title.strip(),
                    "url": normalize_url(result.get("url"), what=f"{where}的 url"),
                    "page_age": page_age,
                }
            )
    return entries, query_texts


def parse_pages(payload, *, known_urls):
    """校验 Fetch 结果；页面 URL 必须在本次检索结果里（防止混入别的 Run）。"""
    if not isinstance(payload, dict):
        raise ScriptError('Fetch 结果必须是 {"pages": [...]} 形态的对象')
    pages = payload.get("pages")
    if not isinstance(pages, list):
        raise ScriptError('Fetch 结果缺少 pages 数组')
    by_url = {}
    for index, page in enumerate(pages, start=1):
        where = f"pages 第 {index} 项"
        if not isinstance(page, dict):
            raise ScriptError(f"{where}不是对象")
        url = normalize_url(page.get("url"), what=f"{where}的 url")
        if url not in known_urls:
            raise ScriptError(f"{where}的 URL 不在检索结果里：{url}（混入了别的 Run？）")
        if url in by_url:
            raise ScriptError(f"pages 里同一 URL 出现多次：{url}")
        status = page.get("status", 200)
        if not isinstance(status, int) or isinstance(status, bool):
            raise ScriptError(f"{where}的 status 必须是整数")
        content = page.get("content")
        if content is not None and not isinstance(content, str):
            raise ScriptError(f"{where}的 content 必须是字符串")
        title = page.get("title", "")
        if not isinstance(title, str):
            raise ScriptError(f"{where}的 title 必须是字符串")
        error = page.get("error", "")
        if not isinstance(error, str):
            raise ScriptError(f"{where}的 error 必须是字符串")
        by_url[url] = {"status": status, "content": content or "", "title": title, "error": error}
    return by_url


def merge_sources(entries):
    """同一 URL 合并成一个来源：标题取首次出现，`queries` 去重合并，按 URL 升序输出。"""
    merged = {}
    for entry in entries:
        source = merged.get(entry["url"])
        if source is None:
            source = {
                "url": entry["url"],
                "title": entry["title"],
                "page_age": entry["page_age"],
                "queries": [],
            }
            merged[entry["url"]] = source
        if entry["query"] not in source["queries"]:
            source["queries"].append(entry["query"])
        if source["page_age"] is None and entry["page_age"]:
            source["page_age"] = entry["page_age"]
    return [merged[url] for url in sorted(merged)]


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------


def build_brief(search_payload, pages_payload, *, now=None, topic=""):
    """把检索 + 抓取两份快照算成一份 `brief.json`（不写文件）。"""
    clock = parse_clock(now)
    entries, query_texts = parse_search(search_payload)
    known = {entry["url"] for entry in entries}
    pages = parse_pages(pages_payload, known_urls=known)

    sources = []
    failures = []
    for source in merge_sources(entries):
        record = {
            "url": source["url"],
            "title": source["title"],
            "published_at": published_date(source["page_age"]),
            # 线索没有访问时间：没抓就没有"访问"这回事，不拿 Run 时钟冒充。
            "accessed_at": None,
            "status": STATUS_LEAD,
            "citable": False,
            "queries": source["queries"],
            "content_sha256": None,
            "content_bytes": None,
            "excerpt": "",
            "note": "未抓取正文：只作线索",
        }
        page = pages.get(source["url"])
        if page is not None:
            body = page["content"]
            if page["status"] != 200:
                reason = page["error"] or f"HTTP {page['status']}"
                record["note"] = f"抓取失败：{reason}"
                failures.append({"url": source["url"], "reason": reason})
            elif not body.strip():
                record["note"] = "抓取失败：正文为空"
                failures.append({"url": source["url"], "reason": "正文为空"})
            else:
                encoded = body.encode("utf-8")
                record["status"] = STATUS_FETCHED
                record["citable"] = True
                record["accessed_at"] = clock.isoformat()
                record["content_sha256"] = hashlib.sha256(encoded).hexdigest()
                record["content_bytes"] = len(encoded)
                record["excerpt"] = excerpt_of(body)
                record["note"] = ""  # 抓到正文就不再是线索，清掉初始标记
        sources.append(record)

    fetched = sum(1 for record in sources if record["status"] == STATUS_FETCHED)
    counts = {
        "queries": len(query_texts),
        "results": len(entries),
        "sources": len(sources),
        "duplicates": len(entries) - len(sources),
        "fetched": fetched,
        "leads": len(sources) - fetched,
        "fetch_failures": len(failures),
        "missing_dates": sum(1 for record in sources if record["published_at"] is None),
    }
    if sources:
        message = (
            f"本次检索 {counts['queries']} 个查询，命中 {counts['results']} 条结果"
            f"（去重 {counts['duplicates']} 条），来源 {counts['sources']} 个："
            f"已抓正文 {counts['fetched']} 个、仅线索 {counts['leads']} 个"
        )
    else:
        message = "本次未获取到来源：不产出任何结论（不编造）"

    brief = {
        "schema_version": SCHEMA_VERSION,
        "agent": AGENT_NAME,
        "observed_at": clock.isoformat(),
        "topic": topic,
        "counts": counts,
        "sources": sources,
        "citable_urls": [record["url"] for record in sources if record["citable"]],
        "lead_urls": [record["url"] for record in sources if not record["citable"]],
        "failures": failures,
        "message": message,
    }
    return brief


def verify_report_text(text, brief):
    """检查简报正文的 `[来源: <url>]` 只指向已抓正文的来源；返回问题列表（空 = 通过）。"""
    problems = []
    citable = set(brief["citable_urls"])
    leads = set(brief["lead_urls"])
    citations = [url.split("#")[0] for url in CITATION_RE.findall(text)]
    for url in citations:
        if url in leads:
            problems.append(f"引用了未抓正文的 URL（只能作线索）：{url}")
        elif url not in citable:
            problems.append(f"引用了不存在的来源：{url}")
    if brief["counts"]["sources"] and not citations:
        problems.append("简报没有任何「[来源: <url>]」标注：结论必须逐条可追溯到已抓正文的来源")
    return problems


def default_output(clock):
    return Path("runs") / clock.strftime("%Y%m%dT%H%M%S") / "brief.json"


def main(argv=None):
    force_utf8_stdio()
    parser = argparse.ArgumentParser(description="行业简报：来源去重、可引用性判定与报告引用校验")
    parser.add_argument("--search", required=True, help='Brave 检索结果的落盘文件（{"queries": [...]}）')
    parser.add_argument("--pages", required=True, help='Fetch 结果的落盘文件（{"pages": [...]}）')
    parser.add_argument("--output", help="brief.json 路径（缺省 runs/<时间戳>/brief.json）")
    parser.add_argument("--now", help="运行时钟 t0，必须带时区偏移；显式给出可复跑")
    parser.add_argument("--topic", default="", help="简报主题，写进 brief.json（缺省空）")
    parser.add_argument("--verify-report", help="校验这份简报正文的引用只指向已抓正文的来源")
    args = parser.parse_args(argv)

    try:
        clock = parse_clock(args.now)
        search = load_json(args.search, what="Brave 检索结果")
        pages = load_json(args.pages, what="Fetch 抓取结果")
        if not isinstance(args.topic, str):
            raise ScriptError("--topic 必须是字符串")
        brief = build_brief(search, pages, now=args.now, topic=args.topic.strip())

        output_path = Path(args.output) if args.output else default_output(clock)
        write_atomic(output_path, brief)

        if args.verify_report:
            try:
                text = Path(args.verify_report).read_text(encoding="utf-8")
            except OSError as exc:
                raise ScriptError(f"简报读不到：{args.verify_report}（{exc}）") from exc
            problems = verify_report_text(text, brief)
            if problems:
                for problem in problems:
                    print(f"industry_brief: {problem}", file=sys.stderr)
                return 1
    except ScriptError as exc:
        print(f"industry_brief: {exc}", file=sys.stderr)
        return 1
    except OSError as exc:
        print(f"industry_brief: 写文件失败：{exc}", file=sys.stderr)
        return 1

    summary = {
        "agent": AGENT_NAME,
        "observed_at": brief["observed_at"],
        "topic": brief["topic"],
        "input": {
            "search": {"path": args.search, "sha256": sha256_of(args.search)},
            "pages": {"path": args.pages, "sha256": sha256_of(args.pages)},
        },
        "report": {"path": str(output_path), "sha256": sha256_of(output_path)},
        "counts": brief["counts"],
        "citable_urls": brief["citable_urls"],
        "lead_urls": brief["lead_urls"],
        "message": brief["message"],
        "verified_report": args.verify_report,
    }
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
