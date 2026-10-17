# Basalt 的 Agent Harness 适配

这是 [agent-harness](../.claude/skills/agent-harness/SKILL.md) 与
[loop-programming](../.claude/skills/loop-programming/SKILL.md) 两个公共 skill 到 Basalt 的映射。
公共方法由 skill 定义，Basalt 的产品契约、工具链与数值门槛仍取本仓事实源。

| 映射 | Basalt 事实源 |
| --- | --- |
| 项目约束 | [CLAUDE.md](../CLAUDE.md)、[宪法](../.specify/memory/constitution.md)、[TechnicalDesign.md](../docs/TechnicalDesign.md)、[EngineeringStandards.md](../docs/EngineeringStandards.md)、[contracts](../docs/contracts/) |
| US 输入 | `docs/us/usN-*.md` 六节；§2 / §5 特有边界、§3 交付物、§4 专属 Harness、§6 执行与人工项；台账只在 `docs/us/REGISTRY.md` |
| 固定人工 review | 文档生成后和 US 收口时；批准决定、所审版本与未验项由 `specs/<feature>/run-state.md` 记录 |
| 项目编排 | module-doc-gen → 人审① → us-register → module-dev；AI 完成 spec / clarify / plan / tasks、比对、analyze 和实现，不增设 tasks 人工停点 |
| 公共机器清单 | [acceptance.json](acceptance.json)；七目标、固定行为 / 注入评测与必跑 / 跳过规则以此及 [README](README.md) 为准 |
| 专属执行映射 | 模块卡 §4 → `harness/stages/<stage>.json` 的 checks / task 绑定 / inputs / protect；模板不授予批准 |
| 实现与判据 | AI 控制侧持有冻结输入 / 外部断言并验收；当前 Agent 实现与修复；独立 AI review 先核对原始材料 |
| 快速反馈 | `--stage <stage> --task <task>`：task profile 与专属 / 受影响检查，具名 Go PASS 防止空匹配 |
| 完整工程验收 | `--stage <stage>`：full 与全部阶段断言并集；无 stage 的验收仅证明公共工程检查 |
| 外部 / 发布验证 | CI 同一入口；US-14 的发布 E2E、D1–D5、四目标产物与真实外部依赖分别留证 |
| 机器结果 | `.loop/runs/<run>/report.json`；passed / failed / error / not_run、原始退出码、源码指纹；SKIP 原因独立记录 |
| 恢复 | run-state 保存任务、输入版本、累计次数和证据；验收报告独立保存，不再写机器 checkpoint |

## 当前 Agent 通过 skill 驱动

Claude 入口为 `/loop-programming <已审 US 或任务>`，module-dev 完成派生与分析后
进入相同流程；不重新启动 Coding Agent CLI。验收程序只执行检查与报告，无 run 模式。
公共方法由 agent-harness 设计判据，Loop skill 按当前输入驱动实现、验收与修复，
项目路径和数值不写入公共 skill。跨项目复用时一起复制两个 skill 并提供项目适配。

从模块卡派生 stage 验收清单，登记实际批准、输入、保护路径、检查和 task 映射，
不包含 CLI、prompt 或重试次数。过程记录为 `specs/<feature>/run-state.md`；尝试前
追加次数与假设，验收后追加结果和下一步。既有失败历史及旧 checkpoint 保留，不靠
移除执行器清零预算。相同问题 3 次修复仍失败按 module-dev 进入 debug；同题累计
3 个假设失败进入设计讨论，每 US 最多 2 轮 debug，相邻尝试无新证据就停止盲试。

完整公共 / 定向 / 完整 US 的实际命令及退出码见 [README](README.md)。skill 的
恢复纪律依赖 Agent 持久化，不代表独立守护进程或不可篡改计数；未写出的状态不能声称已恢复。

## Basalt 的故障语义

| 风险 | 应有的专属判据 | 适用阶段 |
| --- | --- | --- |
| 审计不可写 | LLM / 工具预写失败时真实调用次数为零 | 已有运行链路，随相关 US 回归 |
| 动作成功但终态未写入 | 保留 pending，恢复按契约归 unknown，不自动重放外部动作 | 已有持久化 / 恢复契约 |
| 取消与资源清理 | 父 context 身份优先、预算收敛、子进程清理、会话可继续 | ReAct / Runner / MCP 与其消费方 |
| 状态或配置串用 | Agent / 会话隔离，运行内快照冻结，记忆配额与原子更新 | Profile / Memory / 配置消费方 |
| 入口和管理面装配 | CLI / API / 定时入口连接同一执行链；Run 与调用 / 调度记录可关联 | 相应模块交付后立即整合 |
| 旧持有者继续提交 / 接管错误 | 权威存储、归属代次与失权拒绝、隔离故障和恢复后对账 | Roadmap 的分布式扩展 US；当前不声称已实现 |

安全与故障断言按 [agent-harness 验收设计](../.claude/skills/agent-harness/references/acceptance-design.md) 的五问设计。
本仓不因课程案例引入 Redis / etcd / 时间轮，不采用“十秒接管”或“任何中断都自动补跑”
作为默认契约；定时错过、重复触发和外部不确定副作用遵守既有运行时契约。

## 系统整合与外部判据

运行内核整合验证 US-1～US-7 已交付能力的组合；入口 / 通知 / 调度 / 管理台交付后
增加对应真实数据流；候选发布再验证部署、D1–D5、观测 / 管理面与制品版本。
尚未实现或缺少环境的层保持未验证，不以替身充当真实集群、真模型或服务验收。

当前 `protect`、输入指纹和 CI 入口是已建立的协作与检查机制；本适配未核实远端分支
保护、验收策略独立写权限或远端 Actions 状态，不能宣称判据已与实现者实现权限隔离。
