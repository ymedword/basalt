"""固定编号矩阵 v1：原文和模型意见分别展示，不以格式代替业务裁决。
Fixed-key matrix v1 preserves source text separately from model judgments.
"""
import json
import re
from pathlib import Path

from scenario import MATRIX_STATUSES

FORMAT = "fixed-matrix-v1"
SOURCE_DIR = Path(__file__).resolve().parent / "sources"


def unique_object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate JSON key: " + key)
        value[key] = item
    return value


def expected_ids(group):
    if type(group) is not int or group not in range(1, 5):
        raise ValueError("group must be 1..4")
    return [f"R{number:02d}" for number in range((group - 1) * 12 + 1, group * 12 + 1)]


def object_schema(properties):
    return {"type": "object", "properties": properties, "required": list(properties), "additionalProperties": False}


def matrix_schema(group):
    identifiers = expected_ids(group)
    return object_schema({"report": object_schema({
        "result": object_schema({key: {"type": "string", "minLength": 1} for key in identifiers}),
        "statuses": object_schema({key: {"type": "string", "enum": list(MATRIX_STATUSES)} for key in identifiers}),
        "sources": {"type": "string", "minLength": 1},
        "open_questions": {"type": "string", "minLength": 1},
    })})


def parse_review(raw, group):
    value = json.loads(raw, object_pairs_hook=unique_object)
    if not isinstance(value, dict) or set(value) != {"report"}:
        raise ValueError("exact report wrapper required")
    report = value["report"]
    if not isinstance(report, dict) or set(report) != {"result", "statuses", "sources", "open_questions"}:
        raise ValueError("exact four handoff fields required")
    for field in ("sources", "open_questions"):
        if not isinstance(report[field], str) or not report[field].strip():
            raise ValueError(field + " must contain non-whitespace text")
    identifiers = expected_ids(group)
    for field in ("result", "statuses"):
        if not isinstance(report[field], dict) or set(report[field]) != set(identifiers):
            raise ValueError(field + " requires its exact 12 IDs")
    rows = []
    for identifier in identifiers:
        status, analysis = report["statuses"][identifier], report["result"][identifier]
        if not isinstance(status, str) or status not in MATRIX_STATUSES:
            raise ValueError(identifier + ": unsupported status")
        if not isinstance(analysis, str) or not analysis.strip():
            raise ValueError(identifier + ": empty analysis")
        parts = re.fullmatch(r"\s*依据：(.*?)；缺口：(.*?)；责任：(.*?)；行动：(.*?)\s*", analysis, re.DOTALL)
        if parts is None or not all(part.strip() for part in parts.groups()):
            raise ValueError(identifier + ": basis/gap/owner/action must each contain text")
        rows.append({"id": identifier, "status": status, **dict(zip(
            ("basis", "gap", "owner", "action"), (part.strip() for part in parts.groups())))})
    return {"rows": rows, "sources": report["sources"], "open_questions": report["open_questions"],
            "business_correctness": "requires independent source review; not established by this parser"}


def coverage(reviews):
    errors, seen = [], set()
    for group in range(1, 5):
        try:
            parsed = parse_review(reviews[f"review_{group}"], group)
            seen.update(row["id"] for row in parsed["rows"])
        except (ValueError, KeyError, TypeError) as error:
            errors.append(f"review_{group}: {error}")
    return {"unique_requirements": len(seen), "errors": errors,
            "structure_complete": not errors and len(seen) == 48,
            "business_correctness": "not established by structural checks"}


def source_requirement(identifier):
    if re.fullmatch(r"R\d{2}", identifier) is None or not 1 <= int(identifier[1:]) <= 48:
        raise ValueError("unknown original requirement")
    filename = f"request-{(int(identifier[1:]) - 1) // 6 + 1:02d}.md"
    text = (SOURCE_DIR / filename).read_text(encoding="utf-8")
    matches = re.findall(rf"^## {identifier}\s*\n(.*?)(?=^## R\d{{2}}\s*$|\Z)", text, re.MULTILINE | re.DOTALL)
    if len(matches) != 1 or not matches[0].strip():
        raise ValueError("original requirement absent or ambiguous")
    return matches[0].strip(), filename + "#" + identifier


def render(parsed):
    def cell(value):
        return value.replace("|", "\\|").replace("\r", "").replace("\n", " ")
    lines = ["原文与来源列由冻结材料原样引用；其余列是待人工复核的模型意见。", "",
             "| 要求 | 状态 | 产品依据 | 证据缺口 | 责任 | 下一行动 | 原始来源 |",
             "| --- | --- | --- | --- | --- | --- | --- |"]
    for row in parsed["rows"]:
        original, source = source_requirement(row["id"])
        fields = [row["id"] + " · " + original, row["status"], row["basis"], row["gap"], row["owner"], row["action"], source]
        lines.append("| " + " | ".join(cell(field) for field in fields) + " |")
    lines += ["", "模型引用（真实性另行核对）：" + parsed["sources"], "", "待确认：" + parsed["open_questions"]]
    return "\n".join(lines)
