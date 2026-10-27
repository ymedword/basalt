#!/usr/bin/env python3
"""D4 `docs-diff` 的业务脚本：只读 MCP 快照 → 新增 / 修改 / 删除三集合 + 内容哈希 + 基线。

运行 / Run：

    python scripts/docs_diff.py --documents documents.json --metadata metadata.json \\
        --state state/baseline.json [--output runs/<时间戳>/changes.json] [--now ISO8601]

四条不变量（模块卡 §2.4.5 的 D4 判据）：

1. **变更只由内容哈希判定**：`document_metadata` 的 SHA-256 是唯一判据——哈希相同即
   「未变」（重新 checkout 只改 mtime 不算修改），不同即「修改」；只在当前清单里的
   是「新增」，只在基线里的是「删除」。
2. **首跑不编造基线**：`--state` 指向的文件不存在时输出 `mode=baseline_created`、
   `baseline_missing=true`，四类集合全空，只记录当前快照的哈希；**不得**把首次记录
   的文档写成「新增」，也不出现 `changes_detected` 这个词（没有比较就无所谓变化）。
3. **两份输入必须同源**：`list_documents` 与 `document_metadata` 的路径集合、size、
   modified_at 必须逐份一致；任何一处不一致都说明两次读取之间文件被改过（或混入了
   上一次的元信息），脚本拒绝出报告，而不是挑一个说法。
4. **无变化也出报告**：基线存在且哈希全一致时照样写 `changes.json`，给出
   `changes_detected=false` 与「本次无变化」的 `message`；沉默不是结果。

Four invariants in English: content hashes alone decide the diff; a first run records a
baseline instead of inventing additions; the two MCP snapshots must agree path by path or
the script refuses; and a no-change run still produces a report.

**可复现性**：报告里不写输入文件路径、不写 `modified_at`，只写内容哈希与字节数——
同一份内容 + 同一个 `--now` 在任何机器上得到逐字节相同的 `changes.json`。`modified_at`
只用于上面的同源核对，不参与变更判定，也不进基线。

失败语义：输入 / 基线不合规 → 抛 `ScriptError`，CLI 非零退出且**一个字节都不写**
（不写报告、不动基线）。写序：先落报告，后原子替换 `state/baseline.json`；基线写失败
时报告已在，下一次运行会按旧基线重算同样的差异（幂等，不丢判定）。
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

AGENT_NAME = "docs-diff"
SCHEMA_VERSION = 1

# MAX_DOCUMENTS 是「演示库」的份数上限，不是产品限制。每份文档最多两次工具调用
# （document_metadata + 可能的 read_document），缺省预算 budget.max_tool_calls = 50、
# 固定步骤约 10 次，所以 20 份是仍能跑完的上限；超过它说明库根不是样例文档库
# （没传 -root 时落在进程当前目录 = 整个工作区），继续算只会把预算烧光。
# MAX_DOCUMENTS caps the demo library, not the product: the default tool budget (50 calls)
# fits ~20 docs at two calls each plus the fixed steps.
MAX_DOCUMENTS = 20

# STATUS_BASELINE 只在首跑出现：它表示"这一份被记进了基线"，与"新增"是两回事。
# STATUS_BASELINE only appears on the first run: recorded into the baseline, not "added".
STATUS_BASELINE = "baseline"
STATUS_ADDED = "added"
STATUS_MODIFIED = "modified"
STATUS_DELETED = "deleted"
STATUS_UNCHANGED = "unchanged"


class ScriptError(Exception):
    """输入或状态不合规：CLI 非零退出，一个字节都不写。"""


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

    Go 的 time.RFC3339 对 UTC 输出 `Z`，docs-readonly 的 modified_at 就是这种形态。
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


def entries_of(payload, *, what):
    if isinstance(payload, list):
        return payload
    if isinstance(payload, dict) and isinstance(payload.get("documents"), list):
        return payload["documents"]
    raise ScriptError(f'{what} 必须是 JSON 数组，或 {{"documents": [...]}} 形态的列表')


# ---------------------------------------------------------------------------
# 两份 MCP 快照的校验与同源核对
# ---------------------------------------------------------------------------


def _is_hex64(value):
    return isinstance(value, str) and len(value) == 64 and all(ch in "0123456789abcdef" for ch in value)


def _check_path(value, *, source, index):
    """只接受 docs-readonly 会返回的根内相对路径（`/` 分隔、无 `.` / `..` 段）。

    这里的严格不是重复服务端的检查，而是防止一份写坏的落盘文件把路径当成键空间：
    `a\\b.md` 与 `a/b.md` 在字符串上不同，混进来会让同一份文档被算成两份。
    """
    if not isinstance(value, str):
        raise ScriptError(f"{source} 第 {index} 项的 path 必须是字符串，实际 {type(value).__name__}")
    if not value.strip():
        raise ScriptError(f"{source} 第 {index} 项的 path 不能为空")
    if value != value.strip():
        raise ScriptError(f"{source} 第 {index} 项的 path 首尾有空白：{value!r}")
    if "\x00" in value or "\\" in value or value.startswith("/"):
        raise ScriptError(f"{source} 第 {index} 项的 path 不是根内相对路径：{value!r}")
    if len(value) >= 2 and value[0].isalpha() and value[1] == ":":
        raise ScriptError(f"{source} 第 {index} 项的 path 不得带盘符：{value!r}")
    if any(part in ("", ".", "..") for part in value.split("/")):
        raise ScriptError(f"{source} 第 {index} 项的 path 含空段 / . / ..：{value!r}")
    return value


def _check_size(value, *, source, path):
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ScriptError(f"{source} 文档 {path} 的 size 必须是非负整数，实际 {value!r}")
    return value


def _check_modified_at(value, *, source, path):
    if parse_timestamp(value) is None:
        raise ScriptError(f"{source} 文档 {path} 的 modified_at 必须是带时区的 RFC3339 时间，实际 {value!r}")
    return value


def parse_snapshot(payload):
    """解析 `list_documents` 的结果：path → {size, modified_at}。"""
    entries = entries_of(payload, what="list_documents 结果")
    check_library_root([item.get("path") for item in entries if isinstance(item, dict)])
    snapshot = {}
    for index, item in enumerate(entries, start=1):
        if not isinstance(item, dict):
            raise ScriptError(f"list_documents 第 {index} 项必须是对象，实际 {type(item).__name__}")
        path = _check_path(item.get("path"), source="list_documents", index=index)
        if path in snapshot:
            raise ScriptError(f"list_documents 出现重复路径：{path}")
        snapshot[path] = {
            "size": _check_size(item.get("size"), source="list_documents", path=path),
            "modified_at": _check_modified_at(item.get("modified_at"), source="list_documents", path=path),
        }
    return snapshot


def check_library_root(paths):
    """拒绝"根没指向只读文档库"的清单：像工作区、或份数超过演示上限。

    只读 MCP server 不传 `-root` 时落在进程当前目录——真实实例上那就是整个工作区，
    `agents/`、`versions/`、`skills/` 这些生成物都会被算成文档。这个判据放在**任何
    逐份调用之前**：每份文档要一次 `document_metadata`，36 份就把工具预算烧完
    （2026-10-27 实测：50 次调用上限用尽、run 记 limit_reached），而真正的问题是配置，
    不是文档太多。
    """
    # 只认 Basalt 工作区**独有**的文件；通用目录名（docs/、logs/、skills/）不认，
    # 免得把用户自己的文档库误判成工作区。
    # 这里**不列** MCP 实例配置文件：示例脚本按便携基线判据不得出现该文件名字面量
    # （扫描会把"脚本可能在自动导入 / 改写实例配置"判红），而工作区根另有
    # `AGENTS.md` / `basalt.yaml` 等标记，去掉这一项不降低本判据的判别力。
    # Only Basalt-workspace-only files count; generic folder names would misfire on real
    # libraries. The MCP instance-config filename is deliberately absent: example scripts
    # must not mention it (portable-baseline judgment), and the other markers already
    # identify a workspace root.
    workspace_markers = ("AGENTS.md", "basalt.yaml", "basalt.db", "memory/MEMORY.md")
    for path in paths:
        if isinstance(path, str) and path in workspace_markers:
            raise ScriptError(
                f"文档清单里出现工作区文件（{path}）：只读库根疑似指向整个工作区——"
                "给 docs-readonly 的 args 补 -root <样例文档库绝对路径> 后重跑；本次拒绝计算"
            )
    if len(paths) > MAX_DOCUMENTS:
        raise ScriptError(
            f"文档数 {len(paths)} 超过演示上限 {MAX_DOCUMENTS}（每份要一次元信息调用）："
            "只读库根疑似指向整个工作区——给 docs-readonly 的 args 补 -root <样例文档库"
            "绝对路径> 后重跑，或把库裁到演示范围内；本次拒绝计算"
        )


def parse_metadata(payload):
    """解析 `document_metadata` 的集合：path → {sha256, size, modified_at}。"""
    entries = entries_of(payload, what="document_metadata 结果")
    metadata = {}
    for index, item in enumerate(entries, start=1):
        if not isinstance(item, dict):
            raise ScriptError(f"document_metadata 第 {index} 项必须是对象，实际 {type(item).__name__}")
        path = _check_path(item.get("path"), source="document_metadata", index=index)
        if path in metadata:
            raise ScriptError(f"document_metadata 出现重复路径：{path}")
        digest = item.get("sha256")
        if not _is_hex64(digest):
            raise ScriptError(f"document_metadata 文档 {path} 的 sha256 不是 64 位小写十六进制：{digest!r}")
        metadata[path] = {
            "sha256": digest,
            "size": _check_size(item.get("size"), source="document_metadata", path=path),
            "modified_at": _check_modified_at(item.get("modified_at"), source="document_metadata", path=path),
        }
    return metadata


def cross_check(snapshot, metadata):
    """两份快照必须覆盖同一批文档且 size / modified_at 逐份一致。

    不一致说明两次读取之间文件被动过（或落盘的是两轮的混合）——这时任何"差异"都
    无法归因到文档本身，只能整轮重取。
    """
    missing = sorted(set(snapshot) - set(metadata))
    if missing:
        raise ScriptError(
            f"metadata 缺少文档的元信息：{', '.join(missing)}"
            "（list_documents 有、document_metadata 没有；本 Run 没取全？）"
        )
    extra = sorted(set(metadata) - set(snapshot))
    if extra:
        raise ScriptError(
            f"metadata 多出清单外的文档：{', '.join(extra)}"
            "（不在 list_documents 里；是不是混入了上一次的元信息？）"
        )
    documents = {}
    for path in sorted(snapshot):
        listed = snapshot[path]
        meta = metadata[path]
        if listed["size"] != meta["size"]:
            raise ScriptError(
                f"文档 {path} 的 size 在 list 与 metadata 之间不一致："
                f"{listed['size']} != {meta['size']}（两次读取之间文件变了？整轮重取）"
            )
        if listed["modified_at"] != meta["modified_at"]:
            raise ScriptError(
                f"文档 {path} 的 modified_at 在 list 与 metadata 之间不一致："
                f"{listed['modified_at']} != {meta['modified_at']}（两次读取之间文件变了？整轮重取）"
            )
        documents[path] = {"sha256": meta["sha256"], "size": meta["size"]}
    return documents


# ---------------------------------------------------------------------------
# 基线状态
# ---------------------------------------------------------------------------


def validate_baseline(state):
    if state is None:
        return None
    if not isinstance(state, dict):
        raise ScriptError("状态文件必须是 JSON 对象")
    version = state.get("schema_version")
    if version != SCHEMA_VERSION:
        raise ScriptError(f"状态文件 schema_version 必须是 {SCHEMA_VERSION}，实际 {version!r}")
    observed_at = state.get("observed_at")
    if parse_timestamp(observed_at) is None:
        raise ScriptError(f"状态文件 observed_at 必须是带时区的 ISO 8601 时间，实际 {observed_at!r}")
    documents = state.get("documents")
    if not isinstance(documents, dict):
        raise ScriptError("状态文件 documents 必须是对象（path → {sha256, size}）")
    kept = {}
    for index, path in enumerate(sorted(documents), start=1):
        checked = _check_path(path, source="状态文件", index=index)
        value = documents[path]
        if not isinstance(value, dict):
            raise ScriptError(f"状态文件 documents.{checked} 必须是对象")
        digest = value.get("sha256")
        if not _is_hex64(digest):
            raise ScriptError(f"状态文件 documents.{checked} 的 sha256 不是 64 位小写十六进制：{digest!r}")
        kept[checked] = {
            "sha256": digest,
            "size": _check_size(value.get("size"), source="状态文件", path=checked),
        }
    return {"observed_at": observed_at, "documents": kept}


def load_baseline(path):
    """读出基线原始 JSON（不存在 = 首跑 → None）；形态校验由 `diff_documents` 统一做。

    返回原始 payload 而不是校验后的精简结构：校验只做一遍，避免把 `schema_version`
    在校验过程中丢掉、下一层再判时误报"版本缺失"。
    """
    target = Path(path)
    if not target.exists():
        return None
    return load_json(target, what="基线状态")


# ---------------------------------------------------------------------------
# 主流程
# ---------------------------------------------------------------------------


def _document_record(path, *, status, current, baseline):
    record = {
        "path": path,
        "status": status,
        "sha256": current["sha256"] if current else None,
        "previous_sha256": baseline["sha256"] if baseline else None,
        "size": current["size"] if current else None,
        "previous_size": baseline["size"] if baseline else None,
    }
    return record


def diff_documents(documents_payload, metadata_payload, baseline_payload, *, now=None):
    """把两份 MCP 快照 + 上次基线算成一份 `changes.json`（不写文件）。

    返回 `{"report": <dict>, "state": <dict>}`：报告只含业务事实（模式 / 集合 / 逐份
    哈希 / 计数 / message），不嵌输入文件路径或文件哈希——那些由 CLI 的 stdout 给出。
    这样同一份快照内容 + 同一个 `--now` 得到逐字节相同的报告，与输入 JSON 的序列化
    形态、文件路径、本机 mtime 都无关。
    """
    clock = parse_clock(now)
    current = cross_check(parse_snapshot(documents_payload), parse_metadata(metadata_payload))
    baseline = validate_baseline(baseline_payload)

    sets = {"added": [], "modified": [], "deleted": [], "unchanged": []}
    records = []
    if baseline is None:
        # 首跑的四类集合保持全空：没有基线就没有"变更"可言，只逐份记录当前哈希。
        for path in sorted(current):
            records.append(_document_record(path, status=STATUS_BASELINE, current=current[path], baseline=None))
    else:
        previous = baseline["documents"]
        for path in sorted(set(current) | set(previous)):
            if path in current and path in previous:
                status = STATUS_UNCHANGED if current[path]["sha256"] == previous[path]["sha256"] else STATUS_MODIFIED
            elif path in current:
                status = STATUS_ADDED
            else:
                status = STATUS_DELETED
            sets[status].append(path)
            records.append(_document_record(
                path,
                status=status,
                current=current.get(path),
                baseline=previous.get(path),
            ))

    counts = {
        "current": len(current),
        "baseline": 0 if baseline is None else len(baseline["documents"]),
        "added": len(sets["added"]),
        "modified": len(sets["modified"]),
        "deleted": len(sets["deleted"]),
        "unchanged": len(sets["unchanged"]),
    }
    if baseline is None:
        mode = "baseline_created"
        message = (
            f"无历史基线：本次记录 {counts['current']} 份文档的内容哈希作为基线，"
            "不判定新增 / 修改 / 删除"
        )
    elif counts["added"] or counts["modified"] or counts["deleted"]:
        mode = "compared"
        message = (
            f"本次检测到变更：新增 {counts['added']} / 修改 {counts['modified']} / "
            f"删除 {counts['deleted']}，未变 {counts['unchanged']}"
            f"（基线观察时间 {baseline['observed_at']}）"
        )
    else:
        mode = "compared"
        message = (
            f"本次无变化：{counts['unchanged']} 份文档的内容哈希与基线一致"
            f"（基线观察时间 {baseline['observed_at']}）"
        )

    report = {
        "schema_version": SCHEMA_VERSION,
        "agent": AGENT_NAME,
        "observed_at": clock.isoformat(),
        "mode": mode,
        "baseline_missing": baseline is None,
        "baseline_observed_at": None if baseline is None else baseline["observed_at"],
        "snapshot": {
            "total": counts["current"],
            "size_bytes": sum(entry["size"] for entry in current.values()),
        },
        "counts": counts,
        "sets": sets,
        "documents": records,
        "message": message,
    }
    if baseline is not None:
        # 首跑不写这个键：没有比较时 false 会被读成"确认无变化"。
        report["changes_detected"] = bool(counts["added"] or counts["modified"] or counts["deleted"])

    state = {
        "schema_version": SCHEMA_VERSION,
        "agent": AGENT_NAME,
        "observed_at": clock.isoformat(),
        "documents": {path: {"sha256": entry["sha256"], "size": entry["size"]}
                      for path, entry in sorted(current.items())},
    }
    return {"report": report, "state": state}


def default_output(clock):
    return Path("runs") / clock.strftime("%Y%m%dT%H%M%S") / "changes.json"


def main(argv=None):
    force_utf8_stdio()
    parser = argparse.ArgumentParser(description="文档变更日报：新增 / 修改 / 删除三集合、内容哈希、基线")
    parser.add_argument("--documents", required=True, help="list_documents 的原始结果（数组）")
    parser.add_argument("--metadata", required=True, help='document_metadata 的集合（{"documents": [...]}）')
    parser.add_argument("--state", required=True, help="state/baseline.json；不存在 = 首跑，报告写成功后原子替换")
    parser.add_argument("--output", help="报告路径（缺省 runs/<时间戳>/changes.json）")
    parser.add_argument("--now", help="运行时钟 t0，必须带时区偏移；显式给出可复跑")
    args = parser.parse_args(argv)

    try:
        clock = parse_clock(args.now)
        documents = load_json(args.documents, what="list_documents 输入")
        metadata = load_json(args.metadata, what="document_metadata 输入")
        state_path = Path(args.state)
        baseline = load_baseline(state_path)

        result = diff_documents(documents, metadata, baseline, now=args.now)
        report = result["report"]
        state = result["state"]

        output_path = Path(args.output) if args.output else default_output(clock)
        write_atomic(output_path, report)
        write_atomic(state_path, state)
    except ScriptError as exc:
        print(f"docs_diff: {exc}", file=sys.stderr)
        return 1
    except OSError as exc:
        print(f"docs_diff: 写文件失败：{exc}", file=sys.stderr)
        return 1

    summary = {
        "agent": AGENT_NAME,
        "observed_at": report["observed_at"],
        "mode": report["mode"],
        "baseline_missing": report["baseline_missing"],
        "changes_detected": report.get("changes_detected"),
        "input": {
            "documents": {"path": args.documents, "sha256": sha256_of(args.documents)},
            "metadata": {"path": args.metadata, "sha256": sha256_of(args.metadata)},
        },
        "report": {"path": str(output_path), "sha256": sha256_of(output_path)},
        "state": {"path": str(state_path), "sha256": sha256_of(state_path)},
        "counts": report["counts"],
        "sets": report["sets"],
        "message": report["message"],
    }
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
