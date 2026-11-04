"""修正演示定义：逐组原文、闭合编号与独立复核依据；不联网或执行任务。
Build a grounded fixed-key demo revision without network access or execution.
"""
import copy
import hashlib
import re

from matrix_report import matrix_schema

LABEL = "long-fixed-v1"


def without_fixture_binding(content):
    # 只忽略夹具注入的显式模型身份绑定；其他差异必须拒绝。
    # Ignore only the fixture's exact model-identity block, not arbitrary edits.
    return re.sub(r"(?m)^resource_bindings:\n  # e2e-fixture-bindings\n  model_connection:\n    - name: demo-qwen\n      uid: rs_[A-Z0-9]+\n", "", content)


def narrow_agent(original):
    # 原角色继续用于历史对照；此修改只应用在显式的新长任务修订中。
    # The historical role files stay intact; apply this only to the new demo revision.
    parts = original.split("---", 2)
    if len(parts) != 3:
        raise ValueError("expected Agent frontmatter")
    header, count = re.subn(r"(?m)^knowledge:\n(?:  .*\n)+", "knowledge: []\n", parts[1])
    if count != 1:
        raise ValueError("expected original knowledge binding")
    body = parts[2] + "\n本修订的本组采购、产品和商务原文已随节点输入冻结提供；不再自动检索重复全文。已确认核心经验仍作为提醒。result按output_schema可为字符串或编号对象，不要把对象强制转成Markdown字符串。\n"
    return "---" + header + "---" + body


def frozen_source(root, name):
    content = (root / "sources" / name).read_bytes()
    return f"\n来源：{name}；SHA-256：{hashlib.sha256(content).hexdigest()}\n" + content.decode("utf-8")


def count_properties(value):
    if isinstance(value, dict):
        return len(value) + sum(count_properties(item) for item in value.values())
    if isinstance(value, list):
        return sum(count_properties(item) for item in value)
    return 0


def build(original, root):
    candidate = copy.deepcopy(original)
    candidate["name"] = "bid-readiness-matrix-v1"
    for node in candidate["nodes"]:
        if not node["id"].startswith(("technical_", "delivery_", "review_")):
            continue
        group = int(node["id"].split("_")[1])
        originals = "\n".join(frozen_source(root, f"request-{chapter:02d}.md")
                              for chapter in (group * 2 - 1, group * 2))
        instruction = next(item for item in node["inputs"] if item["target"] == "instruction")
        if node["id"].startswith("review_"):
            node["output_schema"] = matrix_schema(group)
            instruction["literal"] = (
                "独立复核下列12条原始要求与上游意见。严格按output_schema输出report：result和statuses各含且仅含本组12个R编号。"
                "result每项按‘依据：…；缺口：…；责任：…；行动：…’四段填写，尽量80字以内；每段不可空白。statuses逐项用规定五状态。"
                "采购期望不能当产品证明；原文将由报告器原样展示，不必改写数字和条款。每项仍须真实核查，不能靠复制原文算完成。"
                "不得把合同履约事项改成缺少某个软件模块；已有付款触发条件不可说缺失。区分真实公开产品基线与合成经验。"
                "sources只引用本次实际输入或读到的资料，产品全文无此声明时不可写成直接引文。open_questions保留不确定项。\n"
                + originals
                + "\n以下是独立复核的冻结原文。上游模型意见不是新的事实来源。公开基线只证明明确写出的能力，未写出不等于已证明不支持；企业登记是合成案例。\n"
                + frozen_source(root, "product-main.md") + frozen_source(root, "commercial-synthetic.md"))
        elif node["id"].startswith("delivery_"):
            instruction["literal"] = (
                "复核本组12项：期限起算、维护/付款/人员/验收责任和缺证。逐项保留准确单位，当前文件优先旧经验。"
                "result紧凑12行，900字以内。合同履约事项不等于缺少软件模块；采购预算不是报价。"
                "商业材料只以以下登记为准，产品能力结论仍是待复核的上游意见。冻结全文无需再读：\n"
                + frozen_source(root, "commercial-synthetic.md") + "\n本组采购原文，冲突时以原文为准：\n" + originals)
        else:
            instruction["literal"] += "\n本组采购原文，冲突时以原文为准，不只信上游转述：\n" + originals
    if count_properties(candidate) > 1024:
        raise ValueError("corrected demo exceeds definition property limit")
    return candidate
