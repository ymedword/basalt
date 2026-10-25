#!/usr/bin/env python3
"""GitHub 开源雷达的确定性去重与星差计算（颗粒度文档 §4.5.10 / 图 2-8）。

只用标准库。读模型落盘的 GitHub 搜索结果 JSON 与上一次的状态文件，算出本次推荐、
去重数与裁剪数，写下 report.json，**在报告写成功之后**原子替换 state/seen.json。

用法 / Usage：

    python github_radar.py --input github_search.json --state state/seen.json
    python github_radar.py --input github_search.json --state state/seen.json \
        --output runs/20261025T093000/report.json \
        --now 2026-10-25T09:30:00+08:00 --top 5 --window-days 30

设计约束（Why）：

* 数字只由脚本算：模型负责选、解释与写报告，星差不得口算（模块卡 C-04）。
* 首跑没有基线：`baseline_missing=true`，产物里**不出现** `star_delta` 字段，
  绝不编造增长率（§4.5.10 第一个用例的逆改点）。
* 去重按窗口：窗口内已 recommended 的仓库不再推荐；被 top N 裁掉的仓库仍然记进
  `observed`，所以它的星差只会在“有基线且此前未被推荐”时出现——这正是第二次运行
  能算出星差、而首次运行算不出的结构性原因。
* 写序：先 report.json，再原子替换 seen.json。两步之间崩溃只会导致“下次可能重复
  推荐”，不会导致“数据没了却记成已推荐”。
* 输入非法一律抛 `ScriptError`（CLI 非零退出），**不静默补默认值**。

The script owns every number the report quotes. It never calls the network: the model
saves the MCP result to JSON, and this script turns that JSON plus the previous state
into a report, then replaces the state atomically.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
import tempfile
from datetime import datetime
from pathlib import Path

AGENT_NAME = "github-radar"
SCHEMA_VERSION = 1
DEFAULT_TOP = 5
DEFAULT_WINDOW_DAYS = 30

# full_name 只接受 owner/repo 形态；GitHub 的合法字符集是字母数字与 - . _。
# Only owner/repo shape is accepted; anything else is a malformed search result.
FULL_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$")


class ScriptError(Exception):
    """输入非法或状态文件损坏；调用方按错误处理，不吞。"""


def force_utf8_stdio() -> None:
    """把 stdout / stderr 固定为 UTF-8。

    原因：Basalt 的 shell 执行器按字节读子进程输出，再由上层按 UTF-8 解码；Windows
    控制台默认代码页（cp936 等）会让中文变成乱码。示例脚本一律自己钉住 UTF-8，不依赖
    运行环境的代码页。
    The shell executor captures raw bytes and decodes UTF-8; the examples pin UTF-8
    themselves instead of relying on the host code page.
    """
    for stream in (sys.stdout, sys.stderr):
        reconfigure = getattr(stream, "reconfigure", None)
        if reconfigure is not None:
            reconfigure(encoding="utf-8", errors="replace")


def sha256_of(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def parse_timestamp(value: str) -> datetime | None:
    """解析 ISO 8601；接受 `Z` 后缀与纯日期。解析不了返回 None，由调用方报错。"""
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        return datetime.fromisoformat(text)
    except ValueError:
        return None


def parse_clock(value: str | None) -> datetime:
    """运行时钟（t0）。缺省取本机当前时间；显式给出时必须带时区偏移。

    带偏移是硬要求：没有偏移的 `--now` 会让“今天”取决于解释器所在时区，
    同一条命令在两台机器上可能算出不同日期。
    An explicit clock must carry an offset; otherwise "today" depends on the host zone.
    """
    if value is None:
        return datetime.now().astimezone()
    parsed = parse_timestamp(value)
    if parsed is None or parsed.tzinfo is None:
        raise ScriptError(f"--now 必须是带时区偏移的 ISO 8601 时间戳: {value!r}")
    return parsed


def load_json(path: Path, label: str) -> object:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ScriptError(f"读取{label}失败: {path}: {exc}") from exc
    try:
        return json.loads(text)
    except json.JSONDecodeError as exc:
        raise ScriptError(f"{label}不是合法 JSON: {path}: {exc}") from exc


def write_atomic(path: Path, text: str) -> None:
    """同目录临时文件 + 原子替换；父目录不存在时创建。

    父目录创建是必要的：模型写不了新目录（write_file 不建父目录），而
    `runs/<时间戳>/` 与 `state/` 都是脚本自己造出来的。
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    handle, temp_name = tempfile.mkstemp(prefix=path.name + ".", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(handle, "w", encoding="utf-8", newline="\n") as stream:
            stream.write(text)
        os.replace(temp_path, path)
    except BaseException:
        temp_path.unlink(missing_ok=True)
        raise


def normalize_item(raw: object, index: int) -> dict:
    """把一条搜索结果校验并归一化成内部形态；任何异味都抛 ScriptError。"""
    if not isinstance(raw, dict):
        raise ScriptError(f"第 {index} 项不是对象，实际 {type(raw).__name__}")

    name = raw.get("full_name")
    if not isinstance(name, str) or not FULL_NAME.match(name.strip()):
        raise ScriptError(f"第 {index} 项的 full_name 不是 owner/repo: {name!r}")
    name = name.strip()

    # 官方 GitHub MCP 的搜索结果是 stargazers_count；手工整理过的输入常用 stars。
    # Both spellings are accepted so the model never has to rename fields by hand.
    if "stars" in raw:
        stars = raw["stars"]
    elif "stargazers_count" in raw:
        stars = raw["stargazers_count"]
    else:
        raise ScriptError(f"{name}: 缺 stars（或 stargazers_count）")
    if isinstance(stars, bool) or not isinstance(stars, int) or stars < 0:
        raise ScriptError(f"{name}: stars 必须是非负整数，实际 {stars!r}")

    pushed_at = raw.get("pushed_at")
    if not isinstance(pushed_at, str) or parse_timestamp(pushed_at) is None:
        raise ScriptError(f"{name}: pushed_at 必须是 ISO 8601 时间戳，实际 {pushed_at!r}")

    url = raw.get("html_url")
    if url is None:
        url = f"https://github.com/{name}"
    elif not isinstance(url, str) or not url.startswith(("https://", "http://")):
        raise ScriptError(f"{name}: html_url 必须是 http(s) 地址，实际 {url!r}")

    item = {"full_name": name, "stars": stars, "pushed_at": pushed_at, "url": url}

    description = raw.get("description")
    if description is not None:
        if not isinstance(description, str):
            raise ScriptError(f"{name}: description 必须是字符串，实际 {type(description).__name__}")
        if description.strip():
            item["description"] = description.strip()

    # readme_url 由模型从 MCP 响应的 download_url 原样抄来（模块卡 §2.4.5 地址规则：
    # 不得假设默认分支是 main，也不得手拼 raw.githubusercontent.com 地址）。
    # readme_url comes verbatim from the MCP download_url field; hand-built raw URLs
    # are rejected because the default branch is not always main.
    readme_url = raw.get("readme_url")
    if readme_url is not None:
        if not isinstance(readme_url, str) or not readme_url.startswith(("https://", "http://")):
            raise ScriptError(f"{name}: readme_url 必须是 http(s) 地址（取自 MCP 的 download_url）")
        item["readme_url"] = readme_url
    return item


def normalize_items(payload: object) -> list[dict]:
    """校验整份输入，按 full_name 合并同一响应内的重复条目（保留星数高的一条）。

    接受两种形态：裸数组，或 GitHub MCP 搜索结果对象 `{"items": [...]}`。
    `candidates` 统计的是合并后的**去重仓库数**，这样
    `candidates == deduped + trimmed + recommended` 恒成立，报告里的数能自洽复算。
    """
    if isinstance(payload, dict) and "items" in payload:
        payload = payload["items"]
    if not isinstance(payload, list):
        raise ScriptError('输入必须是 JSON 数组，或 {"items": [...]} 形态的搜索结果')

    merged: dict[str, dict] = {}
    for index, raw in enumerate(payload):
        item = normalize_item(raw, index)
        previous = merged.get(item["full_name"])
        if previous is None or item["stars"] > previous["stars"]:
            merged[item["full_name"]] = item
    return list(merged.values())


def empty_state() -> dict:
    return {"schema_version": SCHEMA_VERSION, "baseline_date": None, "observed": {}, "recommended": []}


def validate_state(raw: object) -> dict:
    """校验上一次的状态文件；缺失（None）视为空状态，损坏则报错不猜。"""
    if raw is None:
        return empty_state()
    if not isinstance(raw, dict):
        raise ScriptError(f"状态文件必须是 JSON 对象，实际 {type(raw).__name__}")

    state = empty_state()
    baseline_date = raw.get("baseline_date")
    if baseline_date is not None:
        if not isinstance(baseline_date, str) or parse_timestamp(baseline_date) is None:
            raise ScriptError(f"状态文件的 baseline_date 不是日期: {baseline_date!r}")
        state["baseline_date"] = baseline_date

    observed = raw.get("observed") or {}
    if not isinstance(observed, dict):
        raise ScriptError("状态文件的 observed 必须是对象")
    for name, seen in observed.items():
        if not isinstance(name, str) or not FULL_NAME.match(name.strip()):
            raise ScriptError(f"状态文件 observed 的键不是 owner/repo: {name!r}")
        if not isinstance(seen, dict):
            raise ScriptError(f"状态文件 observed[{name!r}] 必须是对象")
        last_stars = seen.get("last_stars")
        if isinstance(last_stars, bool) or not isinstance(last_stars, int) or last_stars < 0:
            raise ScriptError(f"状态文件 observed[{name!r}] 的 last_stars 必须是非负整数")
        entry = {"last_stars": last_stars}
        for key in ("first_seen", "last_seen"):
            value = seen.get(key)
            if value is None:
                continue
            if not isinstance(value, str) or parse_timestamp(value) is None:
                raise ScriptError(f"状态文件 observed[{name!r}] 的 {key} 不是日期: {value!r}")
            entry[key] = value
        state["observed"][name.strip()] = entry

    recommended = raw.get("recommended") or []
    if not isinstance(recommended, list):
        raise ScriptError("状态文件的 recommended 必须是数组")
    for entry in recommended:
        if not isinstance(entry, dict):
            raise ScriptError("状态文件 recommended 的元素必须是 {full_name, recommended_on}")
        name = entry.get("full_name")
        on = entry.get("recommended_on")
        if not isinstance(name, str) or not FULL_NAME.match(name.strip()):
            raise ScriptError(f"状态文件 recommended 的 full_name 非法: {name!r}")
        if not isinstance(on, str) or parse_timestamp(on) is None:
            raise ScriptError(f"状态文件 recommended[{name.strip()}] 的 recommended_on 不是日期: {on!r}")
        state["recommended"].append({"full_name": name.strip(), "recommended_on": on})
    return state


def recommended_on(state: dict, full_name: str) -> list[datetime]:
    """该仓库在状态里出现过的推荐日期（可能多条，取最新一条判定即可）。"""
    return [
        parsed
        for parsed in (
            parse_timestamp(entry["recommended_on"])
            for entry in state["recommended"]
            if entry["full_name"] == full_name
        )
        if parsed is not None
    ]


def is_recently_recommended(state: dict, full_name: str, today, window_days: int) -> bool:
    """窗口内已推荐过 → 本次不再推荐。window_days=0 表示窗口关闭、永不抑制。"""
    if window_days <= 0:
        return False
    dates = recommended_on(state, full_name)
    if not dates:
        return False
    return (today - max(dates).date()).days < window_days


def prune_recommended(entries: list[dict], today, window_days: int) -> list[dict]:
    """只保留仍在窗口内的推荐记录，使状态文件不无限增长。

    淘汰规则与 `is_recently_recommended` 用同一个判据：出窗即重新可推荐。
    """
    kept = []
    for entry in entries:
        parsed = parse_timestamp(entry["recommended_on"])
        if parsed is None:
            continue
        if window_days > 0 and (today - parsed.date()).days < window_days:
            kept.append(entry)
    return kept


def radar(
    payload: object,
    state: object = None,
    *,
    now: str | None = None,
    top: int = DEFAULT_TOP,
    window_days: int = DEFAULT_WINDOW_DAYS,
) -> dict:
    """纯函数：输入（搜索结果 + 上一次状态）→ 报告 + 下一次状态。

    返回值同时含 `state`（下一次状态）；CLI 写盘时把 `state` 单独落到 seen.json，
    报告文件里不重复一份状态，避免两处副本漂移。
    The returned dict carries the next state under `state`; the CLI writes that to
    seen.json and keeps it out of report.json so the two copies cannot drift.
    """
    if isinstance(top, bool) or not isinstance(top, int) or top < 1:
        raise ScriptError(f"top 必须是 ≥ 1 的整数，实际 {top!r}")
    if isinstance(window_days, bool) or not isinstance(window_days, int) or window_days < 0:
        raise ScriptError(f"window_days 必须是 ≥ 0 的整数，实际 {window_days!r}")

    clock = parse_clock(now)
    today = clock.date()
    items = normalize_items(payload)
    book = validate_state(state)

    baseline_date = book["baseline_date"]
    baseline_missing = baseline_date is None

    candidates = len(items)
    remaining: list[dict] = []
    deduped = 0
    for item in items:
        if is_recently_recommended(book, item["full_name"], today, window_days):
            deduped += 1
            continue
        entry = dict(item)
        seen = book["observed"].get(item["full_name"])
        if seen is not None:
            # 有基线才出星差；无基线条目干脆没有这两个字段（§4.5.10 的逆改点）。
            entry["baseline_stars"] = seen["last_stars"]
            entry["star_delta"] = item["stars"] - seen["last_stars"]
        remaining.append(entry)

    remaining.sort(key=lambda candidate: (-candidate["stars"], candidate["full_name"]))
    recommended = remaining[:top]
    trimmed = len(remaining) - len(recommended)

    next_observed = {name: dict(seen) for name, seen in book["observed"].items()}
    for item in items:
        first_seen = next_observed.get(item["full_name"], {}).get("first_seen")
        if not isinstance(first_seen, str):
            first_seen = today.isoformat()
        next_observed[item["full_name"]] = {
            "first_seen": first_seen,
            "last_seen": today.isoformat(),
            "last_stars": item["stars"],
        }

    next_recommended = prune_recommended(book["recommended"], today, window_days)
    for entry in recommended:
        next_recommended.append({"full_name": entry["full_name"], "recommended_on": today.isoformat()})

    next_state = {
        "schema_version": SCHEMA_VERSION,
        "baseline_date": baseline_date or today.isoformat(),
        "observed": next_observed,
        "recommended": next_recommended,
    }

    if candidates == 0:
        message = "本次无新增：搜索结果为空"
    elif not recommended:
        message = "本次无新增：候选仓库均已在窗口内推荐过，或被 top N 裁剪"
    else:
        message = (
            f"本次推荐 {len(recommended)} 个仓库"
            f"（候选 {candidates}，去重 {deduped}，裁剪 {trimmed}）"
        )

    return {
        "schema_version": SCHEMA_VERSION,
        "agent": AGENT_NAME,
        "observed_at": clock.isoformat(timespec="seconds"),
        "source": {"status": "ok" if candidates else "empty", "candidates": candidates},
        "baseline_date": baseline_date,
        "baseline_missing": baseline_missing,
        "window": {"top": top, "days": window_days},
        "counts": {
            "candidates": candidates,
            "deduped": deduped,
            "trimmed": trimmed,
            "recommended": len(recommended),
        },
        "items": recommended,
        "message": message,
        "state": next_state,
    }


def default_output(clock: datetime) -> Path:
    """缺省产物路径：runs/<时间戳>/report.json（相对 Agent 目录，即 shell 的 cwd）。"""
    return Path("runs") / clock.strftime("%Y%m%dT%H%M%S") / "report.json"


def main(argv: list[str] | None = None) -> int:
    force_utf8_stdio()
    parser = argparse.ArgumentParser(
        description="GitHub 开源雷达：把搜索结果折算成去重后的推荐、星差与下一次状态（产物不进版本库）。"
    )
    parser.add_argument("--input", required=True, type=Path, help="模型落盘的搜索结果 JSON")
    parser.add_argument("--state", required=True, type=Path, help="state/seen.json；存在则读，报告写成功后原子替换")
    parser.add_argument("--output", type=Path, default=None, help="report.json 路径；缺省 runs/<时间戳>/report.json")
    parser.add_argument("--now", default=None, help="运行时钟（ISO 8601，带时区偏移）；缺省取本机当前时间")
    parser.add_argument("--top", type=int, default=DEFAULT_TOP, help=f"每个主题保留条数（缺省 {DEFAULT_TOP}）")
    parser.add_argument(
        "--window-days",
        type=int,
        default=DEFAULT_WINDOW_DAYS,
        help=f"去重窗口天数（缺省 {DEFAULT_WINDOW_DAYS}；0 表示不抑制重复推荐）",
    )
    args = parser.parse_args(argv)

    try:
        payload = load_json(args.input, "搜索结果")
        state_path: Path = args.state
        previous = load_json(state_path, "状态文件") if state_path.exists() else None
        clock = parse_clock(args.now)
        output: Path = args.output if args.output is not None else default_output(clock)
        result = radar(payload, previous, now=clock.isoformat(), top=args.top, window_days=args.window_days)
    except ScriptError as exc:
        # 输入非法：一个字节都不写（报告与 seen 都不动），非零退出把判断权交给模型。
        print(f"github_radar: {exc}", file=sys.stderr)
        return 1

    next_state = result.pop("state")
    try:
        write_atomic(output, json.dumps(result, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
        write_atomic(state_path, json.dumps(next_state, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
    except OSError as exc:
        print(f"github_radar: 写入失败: {exc}", file=sys.stderr)
        return 1

    summary = {
        "input": {"path": str(args.input), "sha256": sha256_of(args.input)},
        "report": {"path": str(output), "sha256": sha256_of(output)},
        "state": {"path": str(state_path), "sha256": sha256_of(state_path)},
        "observed_at": result["observed_at"],
        "baseline_date": result["baseline_date"],
        "baseline_missing": result["baseline_missing"],
        "counts": result["counts"],
        "message": result["message"],
    }
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
