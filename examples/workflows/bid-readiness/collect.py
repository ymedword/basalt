"""只读收集原 Run 及完整矩阵；结构覆盖不是业务正确性判决。
Read original Runs without replay; retain raw responses alongside coverage checks.
"""
import argparse
import hashlib
import json
import re

from demo import API, LOOP, ROOT, approved_sources, save
from scenario import MATRIX_STATUSES
import matrix_report


def original_text(record, run):
    # 普通同步调用的原文可能只在已保存的 invoke 回执中；不能据空投影判模型空答。
    # Bind the original synchronous receipt to the same Run; never call again.
    receipt = record.get("result")
    if receipt is not None:
        if receipt.get("run_id") != run.get("run_id"):
            raise ValueError("invoke receipt does not match original Run")
        return receipt.get("final_text", ""), "invoke_receipt"
    return run.get("final_text", ""), "run_detail"


def report_value(raw):
    value = json.loads(raw)
    if not isinstance(value, dict):
        raise ValueError("report must be an object")
    report = value.get("report", value)
    if not isinstance(report, dict) or set(report) != {"result", "sources", "open_questions"}:
        raise ValueError("report must contain the exact three handoff fields")
    if not all(isinstance(field, str) for field in report.values()):
        raise ValueError("report handoff fields must be strings")
    return report


def matrix_rows(raw):
    # 原样响应另存；仅把模型字符串中的转义换行规范化用于显示和行检查。
    text = report_value(raw)["result"].replace("\\n", "\n")
    rows = []
    for line in text.splitlines():
        fields = [field.strip() for field in line.strip().strip("|").split("|")]
        if fields and re.fullmatch(r"R\d{2}", fields[0]):
            rows.append(fields)
    return rows


def coverage(reviews):
    errors, seen = [], set()
    for group in range(1, 5):
        raw = reviews.get(f"review_{group}")
        if raw is None:
            errors.append(f"review_{group}: original output absent")
            continue
        try:
            rows = matrix_rows(raw)
        except (ValueError, KeyError, TypeError) as error:
            errors.append(f"review_{group}: invalid report: {error}")
            continue
        wanted = {f"R{number:02d}" for number in range((group - 1) * 12 + 1, group * 12 + 1)}
        actual = [row[0] for row in rows]
        if len(actual) != 12 or set(actual) != wanted:
            errors.append(f"review_{group}: requires its exact 12 unique IDs")
        for row in rows:
            identifier = row[0]
            if identifier in seen:
                errors.append(identifier + ": duplicate")
            seen.add(identifier)
            if len(row) != 6 or not all(row):
                errors.append(identifier + ": requires six nonempty columns")
                continue
            if row[2] not in MATRIX_STATUSES:
                errors.append(identifier + ": unsupported status")
            chapter = (int(identifier[1:]) - 1) // 6 + 1
            if f"request-{chapter:02d}.md#{identifier}" not in row[3]:
                errors.append(identifier + ": missing original requirement anchor")
    return {"unique_requirements": len(seen), "errors": errors,
            "structure_complete": not errors and len(seen) == 48,
            "business_correctness": "not established by structural checks"}


def collect(api, label):
    records = json.loads((LOOP / "us19-business-runs.json").read_text(encoding="utf-8"))
    record = records[label]
    report_format = record.get("report_format", "legacy-markdown")
    if report_format not in ("legacy-markdown", matrix_report.FORMAT):
        raise ValueError("unknown recorded report format")
    if report_format == matrix_report.FORMAT:
        approved_sources()
        if record.get("source_manifest_sha256") != hashlib.sha256((ROOT / "source-manifest.json").read_bytes()).hexdigest():
            raise ValueError("original matrix source revision differs; retain originals without regenerating")
    target = LOOP / "us19-business-output" / label
    target.mkdir(parents=True, exist_ok=True)
    originals, reviews = {}, {}
    if "task_id" in record:
        detail, _ = api.call("GET", "/api/v1/flow-tasks/" + record["task_id"] + "?limit=100")
        save(target / "task.json", detail)
        attempts = detail["attempts"]
    else:
        detail = None
        attempts = [{"node_id": "single", "run_id": record["result"]["run_id"], "state": record["state"]}]
    for attempt in attempts:
        run_id = attempt.get("run_id")
        if not run_id:
            continue
        run, _ = api.call("GET", "/api/v1/runs/" + run_id)
        raw, text_source = original_text(record, run)
        node = attempt["node_id"]
        save(target / (node + "-run.json"), run)
        if text_source == "invoke_receipt":
            save(target / (node + "-invoke-receipt.json"), record["result"])
        (target / (node + ".txt")).write_text(raw, encoding="utf-8", newline="\n")
        originals[node] = {"run_id": run_id, "node_state": attempt["state"], "run_status": run["status"],
                           "sha256": hashlib.sha256(raw.encode()).hexdigest(), "bytes": len(raw.encode()),
                           "text_source": text_source}
        if node.startswith("review_") and raw:
            reviews[node] = raw
    assess = matrix_report.coverage if report_format == matrix_report.FORMAT else coverage
    assessment = assess(reviews) if label.startswith("long") else {"business_correctness": "requires paired rubric review"}
    result = {"label": label, "task_id": record.get("task_id"), "status": detail["status"] if detail else record["state"],
              "elapsed_seconds": record.get("elapsed_seconds"), "original_runs": originals, "coverage": assessment,
              "report_format": report_format}
    save(target / "collection.json", result)
    text = ["# 合成采购响应准备包 · 原始运行证据", "", f"组别：{label}；状态：{result['status']}。", "",
            "此包只供内部人工复核。结构验收不等于事实、资质、报价、性能或业务验收。", ""]
    selected = ["final_pack", *[f"review_{group}" for group in range(1, 5)]] if label.startswith("long") else [attempts[-1]["node_id"]]
    for node in selected:
        if node not in originals:
            continue
        meta = originals[node]
        text.extend([f"## {node}", "", f"原 Run：{meta['run_id']}；SHA-256：`{meta['sha256']}`。", ""])
        raw = (target / (node + ".txt")).read_text(encoding="utf-8")
        try:
            if report_format == matrix_report.FORMAT and node.startswith("review_"):
                parsed = matrix_report.parse_review(raw, int(node.split("_")[1]))
                text.extend([matrix_report.render(parsed), ""])
                continue
            report = report_value(raw)
            text.extend([report["result"].replace("\\n", "\n"), "", "来源：" + report["sources"], "", "待确认：" + report["open_questions"], ""])
        except (ValueError, KeyError, TypeError):
            text.extend(["原始输出不是完整预期 JSON；见同目录原文，不能计为通过。", ""])
    (target / "report.md").write_text("\n".join(text), encoding="utf-8", newline="\n")
    print(json.dumps(result, ensure_ascii=False))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("label")
    collect(API(), parser.parse_args().label)
