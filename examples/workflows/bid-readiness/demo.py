"""显式操作隔离演示实例的原管理 API；不实现 Agent 循环或自动重放。
Explicit setup / bounded demonstration through the product's original HTTP API.
"""
import argparse
import hashlib
import json
from pathlib import Path

from scenario import MATRIX_STATUSES
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parent
REPO = ROOT.parents[2]
LOOP = REPO / ".loop"
ADMIN = "e2e-admin-token-not-a-secret"  # Isolated fixture only; never a production token.
OUTPUT = {"type": "object", "properties": {key: {"type": "string"} for key in ["result", "sources", "open_questions"]}, "required": ["result", "sources", "open_questions"], "additionalProperties": False}
INPUT = {"type": "object", "properties": {"request": {"type": "string"}}, "required": ["request"], "additionalProperties": False}
LIMITS = {"max_total_tokens": 200000, "max_tool_calls": 70, "max_duration_seconds": 1800, "max_depth": 3, "max_children": 16, "max_attempts_per_node": 1, "max_revisions": 2}


def save(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def manifest_digest():
    return hashlib.sha256((ROOT / "source-manifest.json").read_bytes()).hexdigest()


def approved_sources():
    manifest = json.loads((ROOT / "source-manifest.json").read_text(encoding="utf-8"))
    for row in manifest["files"] + manifest["context_files"]:
        raw = (ROOT / row["path"]).read_bytes()
        if len(raw) != row["bytes"] or hashlib.sha256(raw).hexdigest() != row["sha256"]:
            raise RuntimeError("source changed: " + row["path"])
    return manifest


class API:
    def __init__(self):
        self.fixture = json.loads((LOOP / "us19-business-fixture.json").read_text(encoding="utf-8"))
        self.base = self.fixture["base"]
        if not self.base.startswith("http://127.0.0.1:"):
            raise RuntimeError("isolated loopback fixture required")
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def call(self, method, path, body=None, extra=None, timeout=120, allowed_codes=("OK",)):
        headers = {"Authorization": "Bearer " + ADMIN, "Content-Type": "application/json"} | (extra or {})
        request = urllib.request.Request(self.base + path, data=None if body is None else json.dumps(body, ensure_ascii=False).encode(), headers=headers, method=method)
        try:
            with self.opener.open(request, timeout=timeout) as response:
                envelope = json.load(response)
                if envelope["code"] not in allowed_codes:
                    raise RuntimeError("product refusal: " + envelope["code"])
                return envelope["data"], dict(response.headers)
        except urllib.error.HTTPError as error:
            envelope = json.loads(error.read())
            raise RuntimeError(f"{method} {path}: {error.code} {envelope.get('code')} {envelope.get('message')}") from None


def setup(api, ingest=False):
    manifest = approved_sources()
    receipt_path = LOOP / "us19-business-setup.json"
    receipt = json.loads(receipt_path.read_text(encoding="utf-8")) if receipt_path.exists() else {"collections": {}, "memories": {}, "documents": {}, "source_manifest_sha256": hashlib.sha256((ROOT / "source-manifest.json").read_bytes()).hexdigest()}
    if receipt["source_manifest_sha256"] != hashlib.sha256((ROOT / "source-manifest.json").read_bytes()).hexdigest():
        raise RuntimeError("setup source revision changed; reconcile existing setup")
    for name in ["bid-request", "bid-product", "bid-commercial"]:
        if name not in receipt["collections"]:
            item, _ = api.call("POST", "/api/v1/collections", {"name": name})
            receipt["collections"][name] = item["collection_id"]
            save(receipt_path, receipt)
    roles = json.loads((ROOT / "roles.json").read_text(encoding="utf-8"))
    catalog, _ = api.call("GET", "/api/v1/admin/flow-agents")
    registered = {item["name"] for item in catalog["items"]}
    for name, role in roles.items():
        if name not in registered:
            files, headers = api.call("GET", f"/api/v1/agents/{name}/files")
            content = (Path(api.fixture["workspace"]) / ".basalt" / "agents" / name / "AGENT.md").read_text(encoding="utf-8")
            api.call("POST", f"/api/v1/agents/{name}/files", {"files": [{"path": "AGENT.md", "content": content}]}, {"If-Match": headers.get("ETag", files.get("etag", ""))})
            deadline = time.monotonic() + 20
            while True:
                current, _ = api.call("GET", "/api/v1/admin/flow-agents?limit=100")
                if any(item["name"] == name and item["eligible"] for item in current["items"]):
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError("Agent registration did not become ready: " + name)
                time.sleep(0.2)
        if name not in receipt["memories"]:
            proposed, _ = api.call("POST", f"/api/v1/agents/{name}/memory/proposals", {"section": "core", "content": role["memory"]})
            receipt["memories"][name] = {"proposal": proposed}
            save(receipt_path, receipt)
        if "confirmation" not in receipt["memories"][name]:
            entry = receipt["memories"][name]["proposal"]["entry_id"]
            confirmation, _ = api.call("POST", f"/api/v1/memory/entries/{entry}/confirm", {})
            receipt["memories"][name]["confirmation"] = confirmation
            save(receipt_path, receipt)
            if confirmation["state"] != "active":
                raise RuntimeError("memory confirmation did not activate the proposal")
    if ingest:
        for row in manifest["files"]:
            name = Path(row["path"]).name
            if name in receipt["documents"] and receipt["documents"][name].get("ingest", {}).get("status") == "active":
                continue
            if name in receipt["documents"] and receipt["documents"][name].get("dispatch"):
                raise RuntimeError("original ingestion requires read-only reconciliation, not replay: " + name)
            if name not in receipt["documents"]:
                uploaded, _ = api.call("POST", "/api/v1/admin/knowledge/uploads", {"name": name, "content": (ROOT / row["path"]).read_text(encoding="utf-8")})
                receipt["documents"][name] = {"upload": uploaded, "sha256": row["sha256"]}
                save(receipt_path, receipt)
            collection = "bid-request" if name.startswith("request-") else "bid-product" if name == "product-main.md" else "bid-commercial"
            uploaded = receipt["documents"][name]["upload"]
            receipt["documents"][name]["dispatch"] = {"state": "pending", "collection_id": receipt["collections"][collection], "path": uploaded["path"]}
            save(receipt_path, receipt)
            result, _ = api.call("POST", "/api/v1/collections/" + receipt["collections"][collection] + "/documents", {"path": uploaded["path"]})
            receipt["documents"][name]["ingest"] = result
            receipt["documents"][name]["dispatch"]["state"] = result["status"]
            save(receipt_path, receipt)
            print("ingest", name, result["status"], flush=True)
            if result["status"] != "active":
                raise RuntimeError("ingestion failed; do not retry unknown consumption")
    print("setup", len(receipt["collections"]), "collections", len(receipt["memories"]), "confirmed memories", len(receipt["documents"]), "documents", flush=True)


def bundle(name, specs, agents, goal):
    nodes = []
    complete_report = name == "bid-readiness-full"
    output_schema = {"type": "object", "properties": {"report": OUTPUT}, "required": ["report"], "additionalProperties": False} if complete_report else OUTPUT
    for index, spec in enumerate(specs):
        node_id, agent, instruction, source, upstream = spec
        inputs = [{"target": "request", "source": "input.request"}, {"target": "instruction", "literal": instruction}]
        if complete_report and node_id.startswith("requirements_"):
            inputs = [{"target": "instruction", "literal": instruction}]
        if source:
            inputs.append({"target": "source_path", "literal": source})
        for previous in upstream:
            for field in (["report"] if complete_report else ["result", "sources", "open_questions"]):
                inputs.append({"target": previous + "_" + field, "source": "outputs." + previous + "." + field})
        node = {"id": node_id, "kind": "agent", "agent": agents[agent], "inputs": inputs, "output_schema": output_schema, "failure": {"max_attempts": 1}}
        if index + 1 < len(specs):
            node["next"] = specs[index + 1][0]
        nodes.append(node)
    return {"format_version": 1, "name": name, "entry": nodes[0]["id"], "task_contract": "TASK.json", "limits": LIMITS, "nodes": nodes,
            "contract": {"contract_version": 1, "goal": goal, "constraints": ["全部企业与采购材料均为合成演示，不能冒充真实客户。", "不得虚构资质、报价、性能证明；当前采购条款优先于历史经验。", "只输出内部准备材料，不对外提交、承诺或确认新的长期记忆。"], "input_schema": INPUT, "output_schema": output_schema, "budget": LIMITS,
                         "artifact_spec": [{"id": "report", "format": "json", "required": True}], "acceptance": [{"id": "report-shape", "version": 1, "kind": "deterministic", "target_kind": "artifact", "target_id": "report", "required": True, "schema": output_schema}], "stop_conditions": ["budget", "deadline", "unknown"], "escalation": "保留证据缺口，交人工核对；格式通过不代表业务通过。"}}


def build_definitions(agents):
    roles = json.loads((ROOT / "roles.json").read_text(encoding="utf-8"))
    if set(roles) - set(agents):
        raise RuntimeError("Agents not ready: " + ",".join(set(roles) - set(agents)))
    specs = []
    product = (ROOT / "sources/product-main.md").read_text(encoding="utf-8")
    commercial = (ROOT / "sources/commercial-synthetic.md").read_text(encoding="utf-8")
    for group in range(4):
        sid = str(group + 1)
        first, second = f"{group * 2 + 1:02d}", f"{group * 2 + 2:02d}"
        second_text = (ROOT / "sources" / ("request-" + second + ".md")).read_text(encoding="utf-8")
        specs.append(("requirements_" + sid, "bid-requirements", "读取source_path第一章；以下是相邻第二章的冻结全文。提取两章全部12项：编号、原始要求、数字/单位/边界和来源锚点。逐项保留，不判断供应商已满足。result紧凑12行，900字以内。\n" + second_text, "request-" + first + ".md", []))
    for group in range(4):
        sid = str(group + 1)
        specs.append(("technical_" + sid, "bid-technical", "对照上游12项与以下product-main.md冻结全文，逐项给编号、状态、能力或证据缺口、来源、补证角色。采购期望不是测评证明。非技术事项交商务。result紧凑12行，900字以内。无需再次读取同一文件。\n" + product, None, ["requirements_" + sid]))
    for group in range(4):
        sid = str(group + 1)
        specs.append(("delivery_" + sid, "bid-delivery", "复核上游12项：期限起算、维护/付款/人员/验收责任和缺证。逐项保留准确单位，当前文件优先旧经验。result紧凑12行，900字以内。以下commercial-synthetic.md冻结全文无需再读：\n" + commercial, None, ["requirements_" + sid, "technical_" + sid]))
    for group in range(4):
        sections = [f"{group * 2 + 1:02d}", f"{group * 2 + 2:02d}"]
        originals = "\n".join((ROOT / "sources" / ("request-" + sid + ".md")).read_text(encoding="utf-8") for sid in sections)
        upstream = ["technical_" + str(group + 1), "delivery_" + str(group + 1)]
        instruction = "独立复核这12项。上游是待复核意见；以以下冻结原文核对。result必须恰为12行矩阵，不加表头：R编号|要求关键数字或事实|状态（" + "/".join(MATRIX_STATUSES) + "五选一，遵从R44）|来源文件.md#R编号|责任角色|缺口与下一行动。每行6列，不遗漏，不做无依据承诺。总字数1000以内。这四组原始矩阵会作为完整附件保留，不由最终摘要替代。\n" + originals
        specs.append((f"review_{group + 1}", "bid-reviewer", instruction, None, upstream))
    specs.append(("final_pack", "bid-reviewer", "基于四组完整矩阵生成内部准备包的决策摘要。result显著标明合成演示；列三类主要阻断、责任角色、下一行动及可继续/待补证的建议，不压缩重写48行矩阵。完整条款依据保留在review_1至review_4原Run的最终输出，作为准备包附件。sources保留四组来源版本与章节；open_questions汇总尚需人工确认的资质、报价、性能、维护等。摘要600字以内；不得宣称业务验收通过。", None, [f"review_{number}" for number in range(1, 5)]))
    definitions = {"long": bundle("bid-readiness-full", specs, agents, "覆盖48项采购要求，产出可追溯的内部采购响应准备包")}
    protocol = json.loads((ROOT / "evaluation-protocol.json").read_text(encoding="utf-8"))
    for case in protocol["paired_cases"]:
        for arm in ["B", "C"]:
            selected = ["bid-general"] * 3 if arm == "B" else ["bid-technical", "bid-delivery", "bid-reviewer"]
            stages = [("technical", selected[0], "逐项检查本次8条要求的产品能力、版本与技术证据缺口。把要求和已验证事实分开，保留编号/来源。以下为冻结product-main.md全文，无须再次读取：\n" + (ROOT / "sources/product-main.md").read_text(encoding="utf-8"), None, []),
                      ("delivery", selected[1], "复核上游与本次8条要求的交付、商业、数字、资质和证据缺口。保留每条编号，当前要求优先历史经验。以下为冻结commercial-synthetic.md全文，无须再次读取：\n" + (ROOT / "sources/commercial-synthetic.md").read_text(encoding="utf-8"), None, ["technical"]),
                      ("review", selected[2], "独立核对本次8条要求，输出8行矩阵，每行含编号、准确事实/数字、来源和状态/缺证边界；不要遗漏。另给责任角色和下一行动。result 900字以内。", None, ["technical", "delivery"])]
            definitions[case["id"] + "-" + arm] = bundle("bid-" + case["id"] + "-" + arm.lower(), stages, agents, "对照评测：8条采购要求的内部准备结果")
    return definitions


def publish(api, fixed_matrix=False):
    approved_sources()
    catalog, _ = api.call("GET", "/api/v1/admin/flow-agents?limit=100")
    agents = {row["name"]: row["agent"] for row in catalog["items"] if row["eligible"]}
    definitions = build_definitions(agents)
    if fixed_matrix:
        from matrix_definition import LABEL, build
        config_path = LOOP / "us19-matrix-agent-config.json"
        config = json.loads(config_path.read_text(encoding="utf-8"))
        if config.get("source_manifest_sha256") != manifest_digest():
            raise RuntimeError("corrected configuration source revision changed; reconcile without replay")
        for name in ("bid-delivery", "bid-reviewer"):
            entry = config["agents"][name]
            if entry["state"] != "confirmed" or entry["agent"] != agents[name]:
                raise RuntimeError("corrected Agent configuration is not pinned: " + name)
        definitions = {LABEL: build(definitions["long"], ROOT)}
    receipt_path = LOOP / "us19-business-published.json"
    receipts = json.loads(receipt_path.read_text(encoding="utf-8")) if receipt_path.exists() else {}
    directory = LOOP / "us19-business-bundles"
    directory.mkdir(exist_ok=True)
    for label, definition in definitions.items():
        old = receipts.get(label)
        path = directory / (label + ".json")
        if old and json.loads(path.read_text(encoding="utf-8")) == definition:
            continue
        if fixed_matrix and old:
            raise RuntimeError("corrected publication already exists; reconcile it without overwriting")
        api.call("POST", "/api/v1/admin/flows/validate", {"definition": definition})
        if old:
            save(directory / (label + "-v" + str(old["version"]) + ".json"), json.loads(path.read_text(encoding="utf-8")))
        body = {"operation_id": "us19-bid-publish-" + label + "-r4", "expected_version": old["version"] if old else 0, "definition": definition}
        if old:
            body["definition_uid"] = old["definition_uid"]
        pending_path = directory / (label + "-publication-pending.json")
        if fixed_matrix:
            if pending_path.exists():
                raise RuntimeError("original publication requires read-only reconciliation")
            save(pending_path, {"request": body, "source_manifest_sha256": hashlib.sha256((ROOT / "source-manifest.json").read_bytes()).hexdigest()})
        result, _ = api.call("POST", "/api/v1/admin/flows/publish", body)
        save(path, definition)
        receipts[label] = result
        save(receipt_path, receipts)
        print("published", label, result["definition_uid"], flush=True)


def prepare_matrix_agents(api):
    # 仅修改隔离演示中两个角色的检索绑定；保留原文件、确认记忆及所有运行回执。
    # Narrow two isolated roles, retaining prior configuration and all execution evidence.
    approved_sources()
    from matrix_definition import narrow_agent, without_fixture_binding
    path = LOOP / "us19-matrix-agent-config.json"
    receipt = json.loads(path.read_text(encoding="utf-8")) if path.exists() else {"agents": {}, "source_manifest_sha256": manifest_digest()}
    if receipt.get("source_manifest_sha256") != manifest_digest():
        raise RuntimeError("corrected configuration source revision changed; reconcile without replay")
    for name in ("bid-delivery", "bid-reviewer"):
        if name in receipt["agents"]:
            entry = receipt["agents"][name]
            if entry["state"] != "confirmed":
                raise RuntimeError("original Agent update requires read-only reconciliation: " + name)
            current, _ = api.call("GET", "/api/v1/admin/flow-agents?limit=100")
            location = Path(api.fixture["workspace"]) / ".basalt" / "agents" / name / "AGENT.md"
            if not any(row["name"] == name and row["eligible"] and row["agent"] == entry["agent"] for row in current["items"]) or location.read_text(encoding="utf-8").strip() != entry["after"].strip():
                raise RuntimeError("confirmed Agent configuration changed: " + name)
            continue
        files, headers = api.call("GET", f"/api/v1/agents/{name}/files")
        location = Path(api.fixture["workspace"]) / ".basalt" / "agents" / name / "AGENT.md"
        original = location.read_text(encoding="utf-8")
        expected = (ROOT / "agents" / name / "AGENT.md").read_text(encoding="utf-8")
        if without_fixture_binding(original).strip() != expected.strip():
            raise RuntimeError("isolated Agent has unexpected configuration: " + name)
        before_catalog, _ = api.call("GET", "/api/v1/admin/flow-agents?limit=100")
        before = next(row["agent"] for row in before_catalog["items"] if row["name"] == name and row["eligible"])
        corrected = narrow_agent(original)
        receipt["agents"][name] = {"state": "pending", "before": original, "before_agent": before, "after": corrected}
        save(path, receipt)
        api.call("POST", f"/api/v1/agents/{name}/files", {"files": [{"path": "AGENT.md", "content": corrected}]}, {"If-Match": headers.get("ETag", files.get("etag", ""))})
        deadline = time.monotonic() + 20
        while True:
            catalog, _ = api.call("GET", "/api/v1/admin/flow-agents?limit=100")
            matches = [row for row in catalog["items"] if row["name"] == name and row["eligible"]]
            if matches and matches[0]["agent"] != before and location.read_text(encoding="utf-8").strip() == corrected.strip():
                receipt["agents"][name].update(state="confirmed", agent=matches[0]["agent"])
                save(path, receipt)
                break
            if time.monotonic() >= deadline:
                raise RuntimeError("updated Agent is not ready: " + name)
            time.sleep(0.2)
        print("prepared", name, "original confirmed memory retained", flush=True)


def case_input(case):
    from scenario import SECTIONS
    wanted = set(case["requirements"])
    text = "只处理以下8项合成采购条款，逐项保留编号、数字、来源和证据边界。产品基线固定main；企业资质/性能/报价未提供材料。\n"
    for section, _, rows in SECTIONS:
        for identifier, statement in rows:
            if identifier in wanted:
                text += f"来源 request-{section}.md#{identifier}：{statement}\n"
    return text


def run(api, label):
    approved_sources()
    log_path = LOOP / "us19-business-runs.json"
    runs = json.loads(log_path.read_text(encoding="utf-8")) if log_path.exists() else {}
    if label in runs:
        raise RuntimeError("original run already recorded; inspect it instead of replaying")
    protocol = json.loads((ROOT / "evaluation-protocol.json").read_text(encoding="utf-8"))
    is_ordinary = label.endswith("-A")
    request_text = "完成SYN-AI-2026内部采购响应准备包，覆盖R01–R48；各专业分别核对后再最终复核。保留所有证据缺口，不对外提交。"
    if not label.startswith("long"):
        case = next(item for item in protocol["paired_cases"] if item["id"] == label.rsplit("-", 1)[0])
        request_text = case_input(case)
    started = time.time()
    from matrix_definition import LABEL
    from matrix_report import FORMAT
    matrix_run = label in (LABEL, "long-fixed-v2")
    runs[label] = {"state": "dispatch_pending", "started_at": started, "label": label,
                   "report_format": FORMAT if matrix_run else "legacy-markdown",
                   "source_manifest_sha256": hashlib.sha256((ROOT / "source-manifest.json").read_bytes()).hexdigest()}
    save(log_path, runs)
    if is_ordinary:
        message = {"input": {"request": request_text, "instruction": "完整处理本次8条的需求、技术和商务核对并最终复查。只读product-main.md一次。result输出8行矩阵，每行有编号、准确事实/数字、来源和状态/缺证边界，末尾写责任角色和下一行动。900字以内。", "source_path": "product-main.md"}, "output_schema": OUTPUT}
        result, _ = api.call("POST", "/api/v1/agents/bid-general/invoke", {"message": json.dumps(message, ensure_ascii=False)}, timeout=1800, allowed_codes=("OK", "LIMIT_REACHED"))
        runs[label].update(state=result["status"], result=result, elapsed_seconds=round(time.time() - started, 3))
        save(log_path, runs)
        print(label, result["status"], runs[label]["elapsed_seconds"], flush=True)
        return
    published = json.loads((LOOP / "us19-business-published.json").read_text(encoding="utf-8"))
    publication = published[LABEL if matrix_run else "long" if label.startswith("long") else label]
    runs[label]["publication"] = publication
    save(log_path, runs)
    accepted, _ = api.call("POST", "/api/v1/flow-tasks", {"definition_uid": publication["definition_uid"], "definition_version": publication["version"], "input": {"request": request_text}}, {"Idempotency-Key": "us19-bid-run-" + label})
    task_id = accepted["task_id"]
    runs[label].update(state=accepted["status"], task_id=task_id, acceptance_receipt=accepted)
    save(log_path, runs)
    if accepted["definition_uid"] != publication["definition_uid"] or accepted["definition_version"] != publication["version"]:
        raise RuntimeError("accepted task differs from pinned publication; inspect original task")
    last = ""
    while time.time() - started < 1800:
        detail, _ = api.call("GET", "/api/v1/flow-tasks/" + task_id + "?limit=100")
        mark = detail["status"] + ":" + detail.get("current_node", "")
        if mark != last:
            print(label, task_id, mark, round(time.time() - started), "seconds", flush=True)
            last = mark
        runs[label].update(state=detail["status"], detail=detail, elapsed_seconds=round(time.time() - started, 3))
        save(log_path, runs)
        if detail["status"] not in ["accepted", "running", "queued"]:
            artifacts, _ = api.call("GET", "/api/v1/flow-tasks/" + task_id + "/artifacts")
            runs[label]["artifacts"] = artifacts
            save(log_path, runs)
            return
        time.sleep(2)
    raise RuntimeError("30-minute monitoring limit reached; inspect original task, never replay automatically")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["setup", "ingest", "publish", "prepare-matrix", "publish-matrix", "run"])
    parser.add_argument("label", nargs="?")
    args = parser.parse_args()
    if args.action == "run" and not args.label:
        parser.error("run requires an unused explicit label")
    api = API()
    if args.action in ["setup", "ingest"]:
        setup(api, args.action == "ingest")
    elif args.action in ("publish", "publish-matrix"):
        publish(api, args.action == "publish-matrix")
    elif args.action == "prepare-matrix":
        prepare_matrix_agents(api)
    else:
        run(api, args.label)


if __name__ == "__main__":
    main()
