#!/usr/bin/env python3
"""D2 `repo-digest` 的业务脚本：PR / Issue / Release 计数、待跟进、待确认与游标推进。

运行 / Run：

    python scripts/repo_digest.py --pull-requests pull_requests.json \\
        --issues issues.json --releases releases.json \\
        --state state/cursor.json [--output runs/<时间戳>/digest.json] [--now ISO8601]

三条不变量（模块卡 §2.4.5 的 D2 判据）：

1. **数字只由脚本算**：计数、排序、待跟进判定、游标推进全在这里完成；模型只解释。
2. **缺字段标"待确认"**：`merged_at` / `comments` / `published_at` 等字段**键缺失**或
   不可解析 → 该条进 `unconfirmed` 列表且计数进 `with_unconfirmed_fields`，绝不猜默认值
   （尤其不把"缺 `merged_at` 的 closed PR"当成 merged 或 closed_unmerged）。
3. **游标只前进到已处理项**：关键字段（`number` / `updated_at`）无法确认的条目**不处理**，
   游标不得越过它；时间相同的条目也按"严格小于"处理，避免下一次被 `>` 比较永久跳过。

Three invariants, in English: counts and ordering come from this script only; a field that
cannot be confirmed is reported as unconfirmed rather than guessed; and the cursor never
advances past an item that was not processed (ties included, by a strict `<`).

失败语义：任何输入或状态不合规 → 抛 `ScriptError`，CLI 以非零退出且**一个字节都不写**
（不写报告、不动游标）。写序：先落 `digest.json`，成功后原子替换 `cursor.json`。
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import tempfile
from datetime import datetime
from pathlib import Path

AGENT_NAME = "repo-digest"
SCHEMA_VERSION = 1
DEFAULT_STALE_DAYS = 7
DEFAULT_LONG_OPEN_DAYS = 30

# SOURCES 是游标与报告的固定来源顺序；报告里的列表按这个顺序排，便于逐条核对。
SOURCES = ("pull_requests", "issues", "releases")

# MISSING 是"键不存在"的哨兵——必须与 JSON 的 null 区分：
# `"merged_at": null` 是**明确的**无值（open 或 closed 未合并），键缺失才是"无法确认"。
# MISSING marks an absent key, which is different from an explicit JSON null.
MISSING = object()


class ScriptError(Exception):
    """输入或状态不合规：CLI 非零退出，一个字节都不写。"""


class ItemError(Exception):
    """单项的关键字段无法确认：该项不处理，游标不得越过它。"""

    def __init__(self, field_name, reason):
        super().__init__(f"{field_name}: {reason}")
        self.field = field_name
        self.reason = reason


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
    """解析带时区偏移的 ISO 8601；`Z` 视为 `+00:00`。无时区 / 非法 → None。

    没有时区的时间无法与游标比较（本机时区不是事实），一律按不可确认处理。
    """
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


def items_of(payload, *, name):
    if isinstance(payload, list):
        return payload
    if isinstance(payload, dict) and isinstance(payload.get("items"), list):
        return payload["items"]
    raise ScriptError(f"{name} 必须是 JSON 数组，或 {{\"items\": [...]}} 形态的列表响应")


def field(item, key):
    return item[key] if key in item else MISSING


def text_of(item, key):
    value = field(item, key)
    if isinstance(value, str):
        return value.strip()
    return ""


class Notes(list):
    """收集单项的"待确认"字段：只记字段与原因，不回填默认值。"""

    def flag(self, field_name, reason):
        self.append({"field": field_name, "reason": reason})
        return None


# ---------------------------------------------------------------------------
# 单项归一化
# ---------------------------------------------------------------------------


def _missing_reason(value):
    """键缺失 / 显式 null → missing；其余（类型不对、解析不了）→ unparseable。"""
    return "missing" if value is MISSING or value is None else "unparseable"


def _identity_of(item, keys):
    for key in keys:
        value = field(item, key)
        if isinstance(value, int) and not isinstance(value, bool):
            return value
    raise ItemError(keys[0], "missing_or_invalid")


def _updated_at_of(item):
    value = field(item, "updated_at")
    parsed = parse_timestamp(value)
    if parsed is None:
        raise ItemError("updated_at", _missing_reason(value))
    return parsed


def _state_of(item, notes):
    value = text_of(item, "state")
    if value not in ("open", "closed"):
        notes.flag("state", "missing_or_invalid")
        return None
    return value


def _created_at_of(item, notes):
    value = field(item, "created_at")
    parsed = parse_timestamp(value)
    if parsed is None:
        notes.flag("created_at", _missing_reason(value))
    return parsed


def _bool_of(item, key, notes):
    value = field(item, key)
    if isinstance(value, bool):
        return value
    notes.flag(key, _missing_reason(value))
    return False


def _age_days(clock, created_at):
    return None if created_at is None else (clock - created_at).days


def normalize_pull_request(item, *, now, stale_days, long_open_days):
    notes = Notes()
    number = _identity_of(item, ("number",))
    updated_at = _updated_at_of(item)
    state = _state_of(item, notes)
    created_at = _created_at_of(item, notes)
    draft = _bool_of(item, "draft", notes) if field(item, "draft") is not MISSING else False

    if state == "open":
        bucket = "draft_open" if draft else "open"
    elif state == "closed":
        raw = field(item, "merged_at")
        merged_at = parse_timestamp(raw) if isinstance(raw, str) else None
        if raw is MISSING:
            # 键缺失 = 无法确认是否合并；显式 null 才是"closed 未合并"。
            notes.flag("merged_at", "missing")
            bucket = "unknown"
        elif raw is None:
            bucket = "closed_unmerged"
        elif merged_at is None:
            notes.flag("merged_at", "unparseable")
            bucket = "unknown"
        else:
            bucket = "merged"
    else:
        bucket = "unknown"

    return {
        "source": "pull_requests",
        "number": number,
        "title": text_of(item, "title"),
        "url": text_of(item, "html_url") or text_of(item, "url"),
        "state": state,
        "bucket": bucket,
        "updated_at": updated_at,
        "created_at": created_at,
        "age_days": _age_days(now, created_at),
        "draft": draft,
        "notes": list(notes),
    }


def normalize_issue(item, *, now, stale_days, long_open_days):
    notes = Notes()
    number = _identity_of(item, ("number",))
    updated_at = _updated_at_of(item)
    state = _state_of(item, notes)
    created_at = _created_at_of(item, notes)

    comments = field(item, "comments")
    if isinstance(comments, bool) or not isinstance(comments, int):
        notes.flag("comments", _missing_reason(comments))
        comments_value = None
    else:
        comments_value = comments

    if state == "open":
        bucket = "open"
    elif state == "closed":
        bucket = "closed"
    else:
        bucket = "unknown"

    return {
        "source": "issues",
        "number": number,
        "title": text_of(item, "title"),
        "url": text_of(item, "html_url") or text_of(item, "url"),
        "state": state,
        "bucket": bucket,
        "updated_at": updated_at,
        "created_at": created_at,
        "age_days": _age_days(now, created_at),
        "comments": comments_value,
        "notes": list(notes),
    }


def normalize_release(item, *, now, stale_days, long_open_days):
    notes = Notes()
    # release 的身份是 `id`（没有 `number`）；报告里统一放在 `number` 键上。
    number = _identity_of(item, ("id", "number"))
    updated_at = _updated_at_of(item)
    draft = _bool_of(item, "draft", notes)
    prerelease = _bool_of(item, "prerelease", notes)

    raw_published = field(item, "published_at")
    published_at = parse_timestamp(raw_published)
    if draft:
        bucket = "draft"
    elif published_at is not None:
        bucket = "prerelease" if prerelease else "published"
    elif raw_published is None:
        # 未起草却没有发布时间：状态自相矛盾，归"未发布"而不是猜。
        bucket = "unpublished"
    else:
        notes.flag("published_at", _missing_reason(raw_published))
        bucket = "unknown"

    return {
        "source": "releases",
        "number": number,
        "title": text_of(item, "tag_name") or text_of(item, "name"),
        "url": text_of(item, "html_url") or text_of(item, "url"),
        "state": "draft" if draft else "published",
        "bucket": bucket,
        "updated_at": updated_at,
        "created_at": published_at,
        "age_days": None,
        "prerelease": prerelease,
        "notes": list(notes),
    }


NORMALIZERS = {
    "pull_requests": normalize_pull_request,
    "issues": normalize_issue,
    "releases": normalize_release,
}


# ---------------------------------------------------------------------------
# 待跟进判定
# ---------------------------------------------------------------------------


def followup_reason(record, *, stale_days, long_open_days):
    """返回该条的待跟进原因（无 → None）。

    规则单点定义在这里，README 与技能的 REFERENCE.md 各抄一份表。顺序即优先级：
    同一条命中多条规则时只出一条，取最先命中的原因。
    """
    if record["source"] == "pull_requests":
        if record["state"] == "open" and not record["draft"]:
            if record["age_days"] is not None and record["age_days"] >= stale_days:
                return "stale_open"
        return None

    if record["source"] == "issues":
        if record["state"] != "open":
            return None
        if record["comments"] == 0 and record["age_days"] is not None and record["age_days"] >= stale_days:
            return "unanswered_open"
        if record["age_days"] is not None and record["age_days"] >= long_open_days:
            return "long_open"
        return None

    return None


# ---------------------------------------------------------------------------
# 游标
# ---------------------------------------------------------------------------


def validate_cursor(state):
    if state is None:
        return {"schema_version": SCHEMA_VERSION, "sources": {}}
    if not isinstance(state, dict):
        raise ScriptError("状态文件必须是 JSON 对象")
    version = state.get("schema_version")
    if version != SCHEMA_VERSION:
        raise ScriptError(f"状态文件 schema_version 必须是 {SCHEMA_VERSION}，实际 {version!r}")
    sources = state.get("sources", {})
    if not isinstance(sources, dict):
        raise ScriptError("状态文件 sources 必须是对象")
    kept = {}
    for name, value in sources.items():
        if name not in SOURCES:
            raise ScriptError(f"状态文件含未知来源：{name!r}")
        if value is not None and parse_timestamp(value) is None:
            raise ScriptError(f"状态文件 sources.{name} 不是带时区的 ISO 8601 时间：{value!r}")
        kept[name] = value
    return {"schema_version": SCHEMA_VERSION, "sources": kept}


def advance_cursor(prev, processed, held):
    """按"只前进到已处理项"推进一个来源的游标。

    返回 `(下一游标, 阻挡原因, 上限)`：

    - 没有受阻项：下一游标 = 已处理项里最大的 `updated_at`（没有已处理项则保持 `prev`）。
    - 有受阻项且都有可解析时间：下一游标 **严格小于** 受阻项最早时间——等于也不行，
      因为下一次比较是 `>`，相等会让受阻项被当成已处理而永久跳过。
    - 有任何受阻项连时间都无法确认：游标原地不动（fail-closed，不猜）。
    """
    if held:
        stamps = [entry["updated_at"] for entry in held]
        if any(stamp is None for stamp in stamps):
            return prev, "unparseable_updated_at", None
        limit = min(stamps)
        eligible = [record["updated_at"] for record in processed if record["updated_at"] < limit]
        candidate = max(eligible) if eligible else prev
        if prev is not None and (candidate is None or candidate < prev):
            candidate = prev
        return candidate, "unprocessed_item", limit

    candidates = [record["updated_at"] for record in processed]
    if not candidates:
        return prev, None, None
    candidate = max(candidates)
    if prev is not None and candidate < prev:
        candidate = prev
    return candidate, None, None


def iso_or_none(value):
    return None if value is None else value.isoformat()


def _held_entry(item, exc):
    raw_updated = parse_timestamp(field(item, "updated_at"))
    raw_number = field(item, "number")
    if not isinstance(raw_number, int) or isinstance(raw_number, bool):
        raw_number = field(item, "id")
        if not isinstance(raw_number, int) or isinstance(raw_number, bool):
            raw_number = None
    return {
        "number": raw_number,
        "field": exc.field,
        "reason": exc.reason,
        "updated_at": raw_updated,
    }


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------


def digest(pull_requests, issues, releases, cursor, *, now=None, stale_days=DEFAULT_STALE_DAYS,
           long_open_days=DEFAULT_LONG_OPEN_DAYS):
    """把三个来源的原始响应 + 上次游标算成一份 `digest.json`（不写文件）。

    返回 `{"report": <dict>, "state": <dict>}`：报告里**不**复制状态，避免两处副本漂移。
    """
    clock = parse_clock(now)
    book = validate_cursor(cursor)

    payloads = {"pull_requests": pull_requests, "issues": issues, "releases": releases}
    source_reports = {}
    followups = []
    unconfirmed = []
    next_sources = {}
    held_by = {}

    for source in SOURCES:
        items = items_of(payloads[source], name=source)
        prev_raw = book["sources"].get(source)
        prev = parse_timestamp(prev_raw) if prev_raw else None

        processed = []
        held = []
        for item in items:
            if not isinstance(item, dict):
                held.append({"number": None, "field": None, "reason": "item_not_object", "updated_at": None})
                continue
            try:
                record = NORMALIZERS[source](item, now=clock, stale_days=stale_days,
                                             long_open_days=long_open_days)
            except ItemError as exc:
                held.append(_held_entry(item, exc))
                continue
            record["is_new"] = prev is None or record["updated_at"] > prev
            processed.append(record)

        # 时间无法确认的受阻项按"可能未处理"处理（保守：不让游标越过它）。
        in_scope = [record for record in processed if record["is_new"]]
        in_scope_held = [
            entry for entry in held
            if entry["updated_at"] is None or prev is None or entry["updated_at"] > prev
        ]

        next_cursor, blocked, limit = advance_cursor(prev, in_scope, in_scope_held)
        next_sources[source] = iso_or_none(next_cursor)
        if blocked is not None:
            held_by[source] = blocked

        counts = {}
        for record in in_scope:
            counts[record["bucket"]] = counts.get(record["bucket"], 0) + 1
        counts["with_unconfirmed_fields"] = sum(1 for record in in_scope if record["notes"])

        for record in in_scope:
            for note in record["notes"]:
                unconfirmed.append({
                    "source": source,
                    "number": record["number"],
                    "title": record["title"],
                    "field": note["field"],
                    "reason": note["reason"],
                })
            reason = followup_reason(record, stale_days=stale_days, long_open_days=long_open_days)
            if reason is not None:
                followups.append({
                    "source": source,
                    "number": record["number"],
                    "title": record["title"],
                    "url": record["url"],
                    "reason": reason,
                    "age_days": record["age_days"],
                })

        source_report = {
            "total": len(items),
            "new": len(in_scope) + len(in_scope_held),
            "processed": len(in_scope),
            "held_back": [
                {"number": entry["number"], "field": entry["field"], "reason": entry["reason"],
                 "updated_at": iso_or_none(entry["updated_at"])}
                for entry in in_scope_held
            ],
            "counts": dict(sorted(counts.items())),
        }
        if blocked is not None:
            source_report["cursor_held"] = blocked
            source_report["cursor_limit"] = iso_or_none(limit)
        source_reports[source] = source_report

    source_order = {name: index for index, name in enumerate(SOURCES)}
    followups.sort(key=lambda entry: (source_order[entry["source"]], -(entry["age_days"] or 0), entry["number"]))
    unconfirmed.sort(key=lambda entry: (source_order[entry["source"]], entry["number"], entry["field"]))

    processed_total = sum(report["processed"] for report in source_reports.values())
    held_total = sum(len(report["held_back"]) for report in source_reports.values())
    if processed_total == 0:
        message = "本次无新增：三个来源都没有晚于游标的新条目"
    else:
        message = (
            f"本次处理 PR {source_reports['pull_requests']['processed']} / "
            f"Issue {source_reports['issues']['processed']} / "
            f"Release {source_reports['releases']['processed']} 项，"
            f"待跟进 {len(followups)} 项，待确认 {len(unconfirmed)} 项"
        )
    if held_total:
        message += f"；{held_total} 项关键字段无法确认，游标未越过它们"

    report = {
        "schema_version": SCHEMA_VERSION,
        "agent": AGENT_NAME,
        "observed_at": clock.isoformat(),
        "window": {"stale_days": stale_days, "long_open_days": long_open_days},
        "sources": source_reports,
        "followups": followups,
        "unconfirmed": unconfirmed,
        "cursor": {
            "previous": {name: book["sources"].get(name) for name in SOURCES},
            "next": next_sources,
            "advanced": {name: book["sources"].get(name) != next_sources[name] for name in SOURCES},
            "held_by": held_by,
        },
        "message": message,
    }
    state = {"schema_version": SCHEMA_VERSION, "sources": next_sources}
    return {"report": report, "state": state}


def default_output(clock):
    return Path("runs") / clock.strftime("%Y%m%dT%H%M%S") / "digest.json"


def main(argv=None):
    force_utf8_stdio()
    parser = argparse.ArgumentParser(description="PR / Issue / Release 日报：计数、待跟进、待确认、游标")
    parser.add_argument("--pull-requests", required=True, help="list_pull_requests 的原始响应")
    parser.add_argument("--issues", required=True, help="list_issues 的原始响应")
    parser.add_argument("--releases", required=True, help="list_releases 的原始响应")
    parser.add_argument("--state", required=True, help="state/cursor.json；存在则读，报告写成功后原子替换")
    parser.add_argument("--output", help="报告路径（缺省 runs/<时间戳>/digest.json）")
    parser.add_argument("--now", help="运行时钟 t0，必须带时区偏移；显式给出可复跑")
    parser.add_argument("--stale-days", type=int, default=DEFAULT_STALE_DAYS,
                        help="PR / Issue 多久算待跟进（缺省 7）")
    parser.add_argument("--long-open-days", type=int, default=DEFAULT_LONG_OPEN_DAYS,
                        help="Issue 长期未关阈值（缺省 30）")
    args = parser.parse_args(argv)

    try:
        clock = parse_clock(args.now)
        pull_requests = load_json(args.pull_requests, what="pull_requests 输入")
        issues = load_json(args.issues, what="issues 输入")
        releases = load_json(args.releases, what="releases 输入")

        state_path = Path(args.state)
        cursor = None
        if state_path.exists():
            cursor = load_json(state_path, what="游标状态")

        result = digest(pull_requests, issues, releases, cursor, now=args.now,
                        stale_days=args.stale_days, long_open_days=args.long_open_days)
        report = result["report"]
        state = result["state"]

        output_path = Path(args.output) if args.output else default_output(clock)
        write_atomic(output_path, report)
        write_atomic(state_path, state)
    except ScriptError as exc:
        print(f"repo_digest: {exc}", file=sys.stderr)
        return 1

    summary = {
        "agent": AGENT_NAME,
        "observed_at": report["observed_at"],
        "input": {
            "pull_requests": {"path": args.pull_requests, "sha256": sha256_of(args.pull_requests)},
            "issues": {"path": args.issues, "sha256": sha256_of(args.issues)},
            "releases": {"path": args.releases, "sha256": sha256_of(args.releases)},
        },
        "report": {"path": str(output_path), "sha256": sha256_of(output_path)},
        "state": {"path": str(state_path), "sha256": sha256_of(state_path)},
        "counts": {name: report["sources"][name]["counts"] for name in SOURCES},
        "followups": len(report["followups"]),
        "unconfirmed": len(report["unconfirmed"]),
        "cursor": report["cursor"],
        "message": report["message"],
    }
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
