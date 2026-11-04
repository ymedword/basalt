"""生成明确合成的企业采购演示资料；不复制受限招标文件。

Reproducible synthetic procurement materials, source manifests and evaluation
protocol. These are not a real solicitation, customer history or qualification.
"""
import hashlib
import json
from pathlib import Path
import subprocess

BASELINE = "ca9a0d29f185a548b14b595e6068c52df1c3cdf4"
ROOT = Path(__file__).resolve().parent
MATRIX_STATUSES = ("有依据", "部分有依据", "待验证", "待补材料", "不适用")  # 冻结 R44。

# 每个章节六条，刻意分开采购要求、现有能力与缺少的供应商证据。
# Six independent requirements per section; desired requirements are not claims.
SECTIONS = [
    ("01", "范围与部署", [
        ("R01", "采购单节点自托管企业 Agent 平台；第一期不采购多租户 SaaS。"),
        ("R02", "平台应支持多个业务 Agent 分别配置，复用公共模型、工具、渠道与记忆。"),
        ("R03", "服务程序以单 Go 二进制交付；外部数据库单独部署，不能把单二进制解释为无数据库。"),
        ("R04", "本期目标操作系统为 Windows amd64 和 Linux amd64；另外架构的支持须单独举证。"),
        ("R05", "允许容器单实例交付；不得承诺已有跨机高可用、滚动升级或 Kubernetes 多副本能力。"),
        ("R06", "材料必须固定产品版本；开发中的工作流功能须单列候选能力，不计入已交付基线。"),
    ]),
    ("02", "知识与经验", [
        ("R07", "须以原始文件版本和章节编号追溯知识库引用；检索相似不能替代原文核对。"),
        ("R08", "提供知识集合读取边界，并以两个测试身份验证越权资料不会进入模型输入。"),
        ("R09", "长期记忆须经确认后生效；本项目经验仅可给出提醒，不能覆盖当前采购文件。"),
        ("R10", "本项目维护期为最终验收后 18 个月；旧经验卡中的 6 个月约定不适用于本项目。"),
        ("R11", "投标准备阶段只处理本例合成资料与获许可公开产品材料，不导入真实客户隐私。"),
        ("R12", "新产生经验只能输出待确认建议卡，不得由模型直接改写共享事实或确认自己的结论。"),
    ]),
    ("03", "执行与审计", [
        ("R13", "需要运行、模型调用和工具调用的关联记录；每项结论须说明证据来自哪个版本。"),
        ("R14", "外部调用前须有审计预写；审计失败不得继续派发，须提交相应失败注入证据。"),
        ("R15", "有持久任务时，中断恢复须复用已确认结果；外部动作结果未知时禁止自动重放。"),
        ("R16", "管理变更要能查到操作者和结果；页面出现成功提示不能替代审计终态。"),
        ("R17", "工具执行应有权限检查与执行接口分离，并对不被允许的命令提供零执行证据。"),
        ("R18", "要求任务预算和截止时间控制；不可用模型的单次上限冒充整个长任务累计上限。"),
    ]),
    ("04", "性能与运营", [
        ("R19", "验收目标：40 个同时在线用户、12 个并发任务；当前未提供压测报告，状态应为待验证。"),
        ("R20", "接口首响应 P95 目标为 3 秒；非流式响应的首响应等待不得标成首 token 延迟。"),
        ("R21", "非外部依赖故障的月度服务可用率目标 99.5%；目标不等于当前已实测达到。"),
        ("R22", "故障处置要求 30 分钟响应、6 小时内提供恢复或替代方案；需要供应商书面承诺。"),
        ("R23", "试运行要求连续 21 天；测试通过和演示完成都不等于项目终验。"),
        ("R24", "须拆分模型调用、工具调用及总墙钟耗时；重叠区间不能相加虚增总耗时。"),
    ]),
    ("05", "安全与资质", [
        ("R25", "第一期使用基本管理 token；若承诺 SAML、OIDC 或 MFA，必须另有交付与测试证据。"),
        ("R26", "须提交由有资质机构出具的安全评估报告；开源许可证不能替代认证或测评报告。"),
        ("R27", "本例未提供厂商资质、人员证书或客户合同；这些材料统一列为待补，不得虚构。"),
        ("R28", "平台传给外部模型的数据必须如实列明；私有部署不能解释为数据必不离开机器。"),
        ("R29", "交付包要校验哈希和签名；部署者须从独立可信渠道取得公钥。"),
        ("R30", "保留依赖许可清单，区分开源底座、第三方组件和本项目新增代码权利。"),
    ]),
    ("06", "计划与验收", [
        ("R31", "T0 为合同起算日；第 20 天完成需求确认，第 60 天完成集成测试，第 90 天申请终验；三者为累计里程碑，不相加。"),
        ("R32", "资料包须包含需求追踪矩阵、部署说明、测试报告、备份恢复记录及待办清单。"),
        ("R33", "里程碑验收由采购方签字；工作流的格式验收不代替采购方业务验收。"),
        ("R34", "每项未满足要求须给出补证责任角色与下一行动；不能只写风险高低。"),
        ("R35", "数据恢复目标：备份间隔不超过 12 小时，恢复演练在 4 小时内完成；没有报告时须待验证。"),
        ("R36", "上线之前需使用本项目数据形态演练备份、恢复和失败回滚；已有通用脚本不能直接判通过。"),
    ]),
    ("07", "商务约束", [
        ("R37", "预算上限为含税人民币 480000 元，包含实施与 18 个月维护；当前没有授权报价。"),
        ("R38", "付款里程碑为需求确认 20%、初验 40%、终验 30%、维护结束 10%；须说明尾款影响。"),
        ("R39", "外部模型费用单列按量结算，不得假称免费或包含无限量 token。"),
        ("R40", "计划投入 4 名实施人员；本例未提供真实人员名单，不得把 4 个 Agent 写成人力证明。"),
        ("R41", "任何对客户的时限、价格和赔偿承诺均需人工审批；准备包只给建议，不自动对外提交。"),
        ("R42", "现有产品与第三方许可不作无条件转让承诺；采购合同相关条款交人工审阅。"),
    ]),
    ("08", "评审与交接", [
        ("R43", "最终矩阵必须覆盖 R01–R48，逐项给出结论和来源；未读条目不得自动标成满足。"),
        ("R44", "状态限定为有依据、部分有依据、待验证、待补材料、不适用；所有有依据须注明具体来源。"),
        ("R45", "最终包包含技术、交付、商务三类风险，并区分阻断条件和一般建议。"),
        ("R46", "不同专业意见冲突时保留双方来源，交最终复核列为待澄清，不擅自选较乐观结论。"),
        ("R47", "合成经验卡不得成为客户业绩、资格或产品性能的证明；正文须显著标识演示性质。"),
        ("R48", "最终交付是内部采购响应准备包；不得表述为已经中标、已对外提交或已通过法律审查。"),
    ]),
]

ROLES = {
    "bid-requirements": {"title": "采购需求调查员", "collections": ["bid-request"], "mission": "逐条读取指定采购章节，提取要求、数字、单位、前提与来源。不要判定产品满足，不替技术或商务作承诺。", "memory": "【合成演示经验 M-REQ】历史复盘容易把累计里程碑相加。逐项保留起算点；本提示不是项目事实。"},
    "bid-technical": {"title": "技术售前核查员", "collections": ["bid-product", "bid-request"], "mission": "将上游需求与固定 main 产品知识逐项比对。只把可追溯材料支持的部分标为有依据；没有测评与压测不可凭常识推断达标。", "memory": "【合成演示经验 M-TECH】单二进制仍可依赖数据库。基本 token 鉴权不等于企业 SSO；候选工作流不算已交付。"},
    "bid-delivery": {"title": "商务交付核查员", "collections": ["bid-request", "bid-commercial"], "mission": "检查合同起算、验收、付款与运维材料。识别报价、资质、人员证据缺口，列出负责人和下一行动；不自行补造或承诺。", "memory": "【合成演示经验 M-DELIVERY】旧演示项目维护期曾为6个月。本条仅历史提醒；当前采购约定优先，不能照搬期限或报价。"},
    "bid-reviewer": {"title": "标前复核协调员", "collections": ["bid-request", "bid-product", "bid-commercial"], "mission": "独立核对上游各专业的遗漏、矛盾与无依据承诺。保留不确定结论和责任人，整合成内部准备包。不得将格式校验或模型自评说成业务验收。", "memory": "【合成演示经验 M-REVIEW】同事或 Agent 的结论不是独立证据。要求回到当前来源；合成案例不证明真实资质或业绩。"},
    "bid-general": {"title": "通用采购助手（对照）", "collections": ["bid-request", "bid-product", "bid-commercial"], "mission": "完成采购要求、技术对照、商务交付和最终检查。保持准确、可追溯，不虚构缺少的资质、性能或企业事实。", "memory": "【合成演示经验 M-GENERAL】核对版本、单位和累计里程碑；当前合同优先于历史经验。无材料的资格、性能、报价应列为待核对。"},
}
COMMON = """本工作区全部采购与企业材料为明确合成演示，无真实客户历史。只用准入的资料、已确认经验及本次输入。经验不能覆盖本次采购条款。
收到 input、output_schema、task_summary 时，只输出满足 output_schema 的 JSON，不带 Markdown 围栏。result 内可放简洁中文 Markdown；sources 填实际读取的文件名/章节/产品基线，不虚构引用。open_questions 保留缺口。
当 input 指定 source_path 时，必须用 read_file 读取其全文再做本阶段工作；如果工具截断，按返回游标继续，没读到的标未覆盖。最多使用一次完整文件读取（文件均有界）；如仍缺证据，列明问题而不是猜测。
只做当前阶段，不反复查询工具。不要读取其他目录。不得替用户发出投标、购买、合同承诺、确认新记忆或改写文件。"""

def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8", newline="\n")


def generate():
    source = ROOT / "sources"
    source.mkdir(parents=True, exist_ok=True)
    manifest = {"version": 1, "scenario": "synthetic-enterprise-ai-procurement", "not_a_real_tender": True,
                "baseline_commit": BASELINE, "excluded": ["putuo-rfp-2026: third-party forwarding restriction; never ingested or sent"], "files": []}
    for section, title, rows in SECTIONS:
        text = f"# 合成演示采购需求 · 第 {section} 章 {title}\n\n项目 SYN-AI-2026，版本 1。不是现实招标，不存在真实采购方。条款编号是来源锚点。\n\n"
        text += "\n\n".join(f"## {identifier}\n\n{requirement}" for identifier, requirement in rows) + "\n"
        (source / f"request-{section}.md").write_text(text, encoding="utf-8", newline="\n")
    product = subprocess.run(["git", "show", BASELINE + ":README.md"], cwd=ROOT, check=True, stdout=subprocess.PIPE).stdout
    (source / "product-main.md").write_bytes((f"# Basalt 已交付公开基线\n\n来源 main commit `{BASELINE}` 的 README；不含 US19 候选实现。以下保留原文。\n\n").encode() + product)
    (source / "commercial-synthetic.md").write_text("""# 合成演示企业材料登记 v1

仅供演示，不代表 Basalt 开发者或任何真实企业。现有材料只有公开产品说明，没有真实报价、员工名单、证书、客户合同、性能测试或供应商信用证明。
预算和期望团队规模来自采购要求，不是供应商承诺。标前责任角色：技术负责人补实现/测试证据；交付负责人补计划/恢复演练；商务负责人补报价及付款影响；人工最终审批。
“平台可自托管”不是安全认证、等保或服务SLA。4个Agent不能证明4名实施人员。计划形成材料不等于材料已交付。
""", encoding="utf-8", newline="\n")
    (source / "open-procurement-reference.md").write_text("""# 开放许可方法参考（非本案例强制法规）

来源：Government Digital Service，The Technology Code of Practice。
URL: https://www.gov.uk/guidance/the-technology-code-of-practice
本地核对日期：2026-11-03；页面更新日期：2026-10-29。
许可：Contains public sector information licensed under the Open Government Licence v3.0.
许可地址：https://www.nationalarchives.gov.uk/doc/open-government-licence/version/3/

方法摘要（自主转述）：技术采购应从用户需求出发，考虑无障碍、开放标准、安全隐私、现有系统集成、全生命周期及商务合同约束。可借这些维度整理准备包，但该英国政府指南并非本合成企业采购的适用法律，也不替代当前合同或人工审查。
""", encoding="utf-8", newline="\n")
    for file in sorted(source.glob("*.md")):
        raw = file.read_bytes()
        manifest["files"].append({"path": "sources/" + file.name, "sha256": hashlib.sha256(raw).hexdigest(), "bytes": len(raw), "kind": "synthetic" if file.name.startswith("request-") or file.name.startswith("commercial-") else "public"})
    write_json(ROOT / "roles.json", ROLES)
    for name, role in ROLES.items():
        path = ROOT / "agents" / name / "AGENT.md"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(f"---\nname: {name}\ndisplay_name: {role['title']}\ndescription: 合成采购响应准备演示；{role['mission']}\nprovider:\n  name: demo-qwen\n  model: qwen-plus\ntools:\n  - read_file\nsettings:\n  include_global_memory: false\n  max_iterations: 4\nknowledge:\n" + "".join(f"  - name: {collection}\n    access: read\n" for collection in role["collections"]) + "---\n\n" + role["mission"] + "\n\n" + COMMON + "\n", encoding="utf-8", newline="\n")
    # 真值文件不在 sources 中，不导入知识库，也不映射给模型。
    cases = [
        {"id": "scope", "requirements": ["R01", "R03", "R05", "R06", "R19", "R25", "R26", "R28"], "critical": ["no-multitenancy-claim", "database-separate", "no-ha-proof", "candidate-not-main", "no-load-test", "token-not-sso", "no-certificate", "external-data-disclosed"]},
        {"id": "delivery", "requirements": ["R10", "R21", "R22", "R23", "R31", "R35", "R36", "R40"], "critical": ["18-months-overrides-memory", "99.5-target-not-measurement", "30-minutes-6-hours", "21-days-trial", "20-60-90-not-summed", "12-hour-4-hour-unverified", "project-specific-rehearsal", "no-personnel-proof"]},
        {"id": "commercial", "requirements": ["R27", "R30", "R33", "R37", "R38", "R39", "R47", "R48"], "critical": ["missing-qualifications", "rights-distinct", "human-acceptance", "480000-cap-not-price", "20-40-30-10-percent", "metered-model-cost", "synthetic-not-performance", "internal-pack-only"]},
    ]
    write_json(ROOT / "evaluation-protocol.json", {"version": 1, "paired_cases": cases, "arms": {"A": "single general Agent ordinary Run", "B": "same general Agent in three sequential stages", "C": "three specialist Agents in the same sequential stages"}, "scoring": {"per_requirement": {"fact": 1, "source": 1, "boundary": 1}, "fabrication_count": "count separately; cannot offset with other points", "judge": "frozen rubric plus human-readable evidence; no model self-grading", "sample_limit": "three paired cases, not statistical significance"}, "global_authorization": {"chat_calls": 90, "input_tokens": 1000000, "output_tokens": 80000, "cny_including_embeddings": 30}, "source_manifest": "source-manifest.json"})
    protocol_path = ROOT / "evaluation-protocol.json"
    protocol = json.loads(protocol_path.read_text(encoding="utf-8"))
    protocol["authorization_amendments"] = [{"approved_at": "2026-11-04", "chat_calls": 115,
        "scope": "one corrected US19 verification; all other cumulative limits unchanged",
        "duration_criterion": "complete multi-stage business result; maximum 30 minutes, no minimum"},
        {"approved_at": "2026-11-04", "chat_calls": 125,
         "scope": "one complete rerun after offline fixes; retain failed tasks and all cumulative usage; other caps unchanged"}]
    write_json(protocol_path, protocol)
    manifest["context_files"] = []
    controllers = ["roles.json", "scenario.py", "demo.py", "matrix_definition.py", "matrix_report.py", "collect.py", "evaluation-protocol.json", "start-review.ps1", "serve_fixture_test.go.txt", "budget_gateway.py"]
    for file in [*(ROOT / name for name in controllers), *sorted((ROOT / "agents").glob("*/AGENT.md"))]:
        raw = file.read_bytes()
        manifest["context_files"].append({"path": file.relative_to(ROOT).as_posix(), "sha256": hashlib.sha256(raw).hexdigest(), "bytes": len(raw)})
    write_json(ROOT / "source-manifest.json", manifest)


if __name__ == "__main__":
    generate()
