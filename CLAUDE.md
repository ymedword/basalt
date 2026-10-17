# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Basalt — Claude Code 项目指南

Basalt 是用 Go 实现的面向企业场景的 **Agent OS**：自托管单 Go 二进制，作为统一底座运行多个业务 Agent，共享渠道接入、模型路由、工具调用、记忆系统与沙箱执行。工作区与持久化数据由企业自托管；接入外部模型或工具时，相应请求数据按配置发送到服务端。定位严监管企业（银行、政府、能源），差异化是私有部署、可审计、单二进制交付。

Basalt 的产品范围、架构与实现事实以本仓需求、技术方案、契约和验收证据为准。项目介绍直接说明自身设计；历史来源与迁移对照集中在 [docs/Compatibility.md](docs/Compatibility.md)。开发不依赖读取或修改其他项目仓库。

四份正文现为 `Requirements.md`、`IndustrySurvey.md`、`TechnicalDesign.md`、`DevelopmentLoop.md`。历史 US、规格及迁移前台账中的旧文件名和章节号，先按 [文档引用索引](docs/Compatibility.md#document-reference-index) 换算；不得把旧编号直接套到新正文。新模块记录正文路径和语义锚点，章节号只作辅助。

### 文档体系（各司其职）

| 文档 | 角色 | 裁决顺位 |
| --- | --- | --- |
| [.specify/memory/constitution.md](.specify/memory/constitution.md) | 原则（十条 + 三条架构约束 + 开发工作流） | **最高**——原则冲突一律以它为准 |
| [docs/TechnicalDesign.md](docs/TechnicalDesign.md) | How（技术方案） | 文档之间**最权威**——冲突时以它为准 |
| [docs/Requirements.md](docs/Requirements.md) | What（需求与验收） | 次之 |
| [docs/DevelopmentLoop.md](docs/DevelopmentLoop.md) | 统一开发正文（环境 / US 拆解 / Harness / Loop / 门禁 / 陷阱） | 次之 |
| [docs/IndustrySurvey.md](docs/IndustrySurvey.md) | Why（业界调研） | 次之 |
| [harness/README.md](harness/README.md) | 开发 Harness 与 Loop 协议（统一入口 / 判据 / 持久化与恢复） | 配套工件，与开发操作 §9 同口径 |
| [.claude/skills/agent-harness/SKILL.md](.claude/skills/agent-harness/SKILL.md) | 跨项目公共 Harness 判据与复核方法 | 通过 [Basalt 适配](harness/project.md) 使用本仓规则、命令与状态，不替代产品契约 |
| [.claude/skills/loop-programming/SKILL.md](.claude/skills/loop-programming/SKILL.md) | 当前 Agent 驱动的实现 / 验收 / 修复 / 恢复 | 项目命令与预算通过 [Basalt 适配](harness/project.md) 绑定 |
| [docs/Compatibility.md](docs/Compatibility.md) | 兼容范围、迁移与历史对照 | 配套工件，冲突以技术方案为准 |
| [docs/EngineeringStandards.md](docs/EngineeringStandards.md) | 工程规范（Go / 测试 / 数据库 / 日志错误 / 提交与分支） | 配套工件，冲突以技术方案为准；提交格式以其 §5 为准 |
| [docs/contracts/](docs/contracts/) | 契约四件（run-and-errors / agent-md / schema.sql / openapi.yaml） | 固定字面量以此为准 |
| [docs/Operations.md](docs/Operations.md) | 运维手册（装 / 看 / 备份 / 升级 / 凭证） | 配套工件 |
| [docs/GettingStarted.md](docs/GettingStarted.md) | 旧入口与章节索引；正文统一到开发操作 §9 / §10 | 导航，不另立流程 |
| [docs/Roadmap.md](docs/Roadmap.md) | 增量阶段规划 | 规划类，**不入**权威序列；冲突以技术方案为准 |

**权威顺位（与宪法 Governance 同一份口径）**：**原则约束**以[宪法](.specify/memory/constitution.md)（经采纳的版本）为准；**实现设计**在四份权威文档之间以[技术方案](docs/TechnicalDesign.md)为准；**数据结构与 API 的固定字面量**以 [docs/contracts/](docs/contracts/) 为准。四份权威文档**不得**互相"修正"；有冲突先裁决再动手；冲突涉及原则变更时**必须**走宪法修订程序。

## 不可违背的原则（十条）

正式文本在 [.specify/memory/constitution.md](.specify/memory/constitution.md)（每条带「违反时的症状」与「复核方式」），与 [docs/TechnicalDesign.md](docs/TechnicalDesign.md) §1.1 逐条对齐（编号与顺序一致）。动工前**必须**逐条对照。

1. **自实现 ReAct 循环**——`go.mod` **不得**出现 Agent 框架坐标；循环体是本仓库代码，协议 SDK 不得接管循环或自动执行工具
2. **LLM 客户端只做协议调用**——**不得**启用任何自动工具循环；工具调度唯一归属 `ToolExecutor`；会话历史由**一条消息仓储**承载——正常运行中模型响应与工具结果由 `ReActLoop` 统一追加、用户输入由 `AgentService` 写入、崩溃 / 取消后的半截工具组由恢复路径补齐；**不得**出现第二套追加路径、**不得**重复追加
3. **Provider 必须显式映射**——装配处一次构建 `provider name → 客户端实例` 映射表，路由只查表；**不得**遍历推断（Go map 遍历顺序随机）
4. **一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露**——绑定只由 `AGENT.md` frontmatter 的 `skills:` 声明（声明即绑定，各平台一致）；**不得**引入软连接类机制；技能按 L1/L2/L3 渐进披露，**不得**预载正文、**不得**新增 `use_skill` 工具；声明名无实体 → 该 Agent 整只不注册
5. **审计 day-one 且 fail-closed**——**必须**先写 `pending` 审计记录再发调用，预写失败**不得**发出；返回后更新终态；**不得**以"日志够了"跳过落库
6. **授权与执行分离（两接口）**——`Policy.Check(ctx, action) error` 与 `CommandRunner.Run(ctx, cmd) (CommandResult, error)` 两接口；调用方**不得**依赖 `WhitelistPolicy` / `LocalRunner` 等具体实现
7. **同步执行 + context 只承载取消 / 截止 / 追踪**——**不得**引入异步编排；`context.WithValue` 只允许追踪标识，Profile / Session 等业务状态**必须**显式传递；**不得**用包级变量模拟 ThreadLocal
8. **单二进制纪律**——发布产物**必须** `CGO_ENABLED=0` 构建；`-race` 与发布构建**分属不同命令 / job**；依赖最小化——本仓技术栈表与已批准计划（spec / plan / 颗粒度文档）内的依赖**直接落地**，表外新增坐标或版本变更先问用户
9. **不用 ORM，手写 SQL**——`schema.sql` 幂等 + `schema_version` 受控前向迁移；已发布迁移只追加、**不得**改写
10. **五大能力优先，范围卡紧**——核心阶段范围**以[需求文档的交付边界](docs/Requirements.md#release-scope)与已批准的 US 清单（台账）为准**（管理台与观测、Agent 动态管理、每日 Demo、发布收尾等已登记交付物同属核心阶段）；企业级治理（认证 / 多租户 / 审计查询 / Tool Policy）放扩展阶段，完不成的挪阶段而**不得**砍验收

## 工程与门禁

- **统一自动化验收**：`sh scripts/check-acceptance.sh` / `.\scripts\check-acceptance.ps1`（`make acceptance` 同入口），汇总原七目标 + 固定行为 / 注入评测；阶段配置再加任务断言。**完成的定义仍是宪法口径的 `make check` 七目标全绿**，完整验收是它的超集入口——不得用 evals-only 或阶段子集替代。自动通过之后仍按模块要求做独立复核与人工验收
- **两层 Harness**：公共规则集中维护，公共机器清单以 `harness/acceptance.json` 为准；US 专属验收和特有边界写进每份颗粒度文档，stage 从中派生执行映射。集中定义、每轮复用，不复制公共清单；独立复核属于审核方式。细则见 [开发操作 §9.3](docs/DevelopmentLoop.md#loop-protocol-3)
- 工具链与全部依赖坐标见 [docs/TechnicalDesign.md](docs/TechnicalDesign.md) §1.3（2026-10-14 实测）；`go.mod` / `go.sum` 是版本唯一事实源。**版本语义**：`go` 行写的是**最低版本要求**（`go 1.27`），**不是**准确版本锁定——准确构建版本由 `toolchain` 指令（`toolchain go1.27.2`）与 CI 工具链配置（`actions/setup-go`）固定；**不得**写成"`go.mod` 锁定 1.27.2"
- **模块代理**：显式 `GOPROXY=https://goproxy.cn`（本机直连不可达——历史实测与设置见[开发操作 §10.2.6](docs/DevelopmentLoop.md)）；写坐标前先 `go list -m -versions <module>` 核实
- 覆盖率底线（**必须有可执行判据**，`go test -cover` 不会自动失败）：`core` / `tool`（沙箱）/ `memory` / `storage` ≥ 70%，其余 ≥ 50%，由 US-1 交付的 `make cover`（纳入 `make check`）按包判失败——有可执行语句但无测试文件的包计 0%、未达标即失败，无可执行语句的包（`[no statements]`）跳过
- 格式修复只许 `gofumpt -w <本轮改动的文件>`（gofumpt 是 gofmt 的严格超集），禁手改格式；全仓格式检查（`gofumpt -l .`）**只读**——**不得**顺手重排无关文件（`/init-foundation` 的全仓初始格式化是唯一例外）
- 静态检查判定集**显式指定**：staticcheck 按 `-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022` 运行（US-1 的 Makefile `lint` 目标落地）——命名与注释规范由门禁判卷，见 [docs/EngineeringStandards.md](docs/EngineeringStandards.md) §1

## 工作流（三步链）

- 环境准备见 [开发操作 §10](docs/DevelopmentLoop.md)，已有模块按 §9 恢复状态并执行 Loop；不要重复初始化仓库
- 后续 US 与未来扩展由 `/loop-programming` skill 指挥当前 Agent 实现 / 验收 / 修复，`/agent-harness` 提供判据方法，Basalt 规则取 [项目适配](harness/project.md)。验收工具只运行检查与报告，不启动编码 Agent；过程记录、累计预算和恢复位置由 skill 写入 run-state。控制侧保护判据，不弱化断言换取绿态。
- 每个 US 固定流程：AI 生成颗粒度文档、自检与独立 review → **人工 review ①** → AI 自动登记、spec / clarify / plan / tasks、任务比对、analyze → Loop 实施 / 验收 / 修复 → 收口三件套（analyze 复跑、独立 AI 复核、七项证据）→ **人工 review ②：US 收口**。中间不设登记、tasks 或实现前的固定人工确认；analyze 的 CRITICAL 未清零仍不得实施。已批准范围内自主推进，实质范围变更回到模块卡评审修订，不伪造批准或覆盖历史证据
- **US-1 也走三步链**；工程地基（`go.mod` / 包骨架 / Makefile / CI / 日志 / SQLite）的**装法**参照 `/init-foundation`（流程与验收仍以 US-1 颗粒度文档为准）
- **规范前置（编码类改动）**：写 / 改 Go 代码、SQL、日志、测试前**必须**先读 [docs/EngineeringStandards.md](docs/EngineeringStandards.md) 对应节（§1 Go / §2 测试 / §3 数据库 / §4 日志；提交口径 §5）——三步链 `/module-dev`、`/debug` 与一切直改均适用
- 台账 [docs/us/REGISTRY.md](docs/us/REGISTRY.md) 是 US 登记的**唯一登记处**（`/us-register N` 写入）；US 是**开放序列**（US-15 起顺延，编号只增不改），见 [docs/Roadmap.md](docs/Roadmap.md) §4
- **`git init` 由用户执行**——AI **不得**自行初始化仓库；同样**不得**自动 commit / push / tag
- 提交信息 = **英文** Conventional Commits 标题 + 四段**双语**正文（Problem / Design / Testing / Impact——每段先英文段、空行、再中文段逐段对照），规范原文见 [docs/EngineeringStandards.md](docs/EngineeringStandards.md) §5

## 常见陷阱（首版 6 条；全表 60 条见[开发操作 §7](docs/DevelopmentLoop.md)）

| 陷阱 | 症状 / 修复 |
| --- | --- |
| `gofmt -l` / `gofumpt -l` 判卷顺序写反 | 格式差异时退出码为 0 而输出非空；语法错误时退出码非 0 而输出为空——**必须先查退出码、再判输出** |
| `-race` 与 `CGO_ENABLED=0` 混用 | 竞态检测需 CGO 开启、发布构建要求 CGO 关闭——两者**分属不同命令 / job** |
| 假设 build tag 测试会自动跑 | `go test ./...` 默认不执行 `-tags=integration` 的测试，**必须**单列一条命令 |
| 直连 proxy.golang.org | 本机不可达（历史实测见[开发操作 §10.2.6](docs/DevelopmentLoop.md)）；**必须**显式带 `GOPROXY=https://goproxy.cn` |
| cron 表达式写 5 段 | `robfig/cron/v3` 默认 5 段制式，本项目要 **6 段**（秒 分 时 日 月 周）——**必须** `cron.NewParser(Second\|Minute\|Hour\|Dom\|Month\|Dow)`；时区用 `CRON_TZ=` 前缀，**不得**依赖进程时区 |
| 用 `winget` 装 Go | 历史快照中 winget 源版本落后（见[开发操作 §10.2.1 / §10.2.2](docs/DevelopmentLoop.md)）；从 [go.dev/dl](https://go.dev/dl/) 获取匹配构建版本的工具链；本机版本**必须** ≥ `toolchain` 指令声明的版本，且与 CI 一致 |

陷阱表随每个 US 的实测发现增补（每条带「怎么发现」与「怎么复核」）；全表见开发操作 §7。

## 环境与约定

- 本机：Windows 11 + Git Bash；环境准备与历史读数见[开发操作](docs/DevelopmentLoop.md) §10.2 / §10.8，就绪判据与排障见 §10.2.8 / §10.7；续跑时读取实际环境，不从历史安装快照推断当前状态。Go 工具链未就绪时，不运行 `go` / `make`，如实登记未验证
- 文档与代码注释用中文；关键注释（包级 / 导出符号级 / 复杂逻辑段）中英并列，中文在前
- 测试函数名**必须**英文（`TestXxx_yyy`）；文档 harness 的中文验收点以注释保留

## 硬性禁止

- 禁止自动 `git init` / commit / push / tag / 部署——时机由用户决定
- 禁止明文落盘凭证——一律 `${ENV_VAR}` 占位
- 禁止删断言 / 无理由跳过测试（`t.Skip` 须写明理由）/ 放宽阈值让门禁变绿；**判据面变更 = 变更**——改变验收语义须停下确认（允许新增测试、等价重构、随已批准接口适配；判据面口径与**单一权威枚举**见 [docs/EngineeringStandards.md](docs/EngineeringStandards.md) §2）
- `specs/**` 按状态区分：**当前 US 的草案可修订**（`clarify` / `analyze` 调整内容、`implement` 更新任务勾选）；**已批准范围的变更必须经用户确认**；**完成验收后的历史设计不得静默改写**（须用户裁决并在文档里留裁决记录）。已发布迁移脚本**不得**改写（只追加）。`specs/**/run-state.md` 是**过程状态**（非设计工件）：失败台账只追加、不改旧行；唯一可原地翻转的是未验证清单「未验证 → 已验证」（须注日期与证据）；收口后保留，**不**按临时物清理
- 禁止自行修订 [.specify/memory/constitution.md](.specify/memory/constitution.md)——宪法修订**必须**经用户确认（走 `/speckit-constitution`）
