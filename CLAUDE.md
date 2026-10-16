# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Basalt — Claude Code 项目指南

Basalt 是用 Go 实现的面向企业场景的 **Agent OS**：自托管单 Go 二进制，作为统一底座运行多个业务 Agent，共享渠道接入、模型路由、工具调用、记忆系统与沙箱执行。工作区与持久化数据由企业自托管；接入外部模型或工具时，相应请求数据按配置发送到服务端。定位严监管企业（银行、政府、能源），差异化是私有部署、可审计、单二进制交付。

Basalt 是 **NivroOS（Java 实现）的 Go 移植**——同一份 Agent 资产协议（`AGENT.md` 目录、工具契约、API 形状），逐项差异与承诺记在 [docs/Compatibility.md](docs/Compatibility.md)。**两仓各自演进**：本仓的实现事实只以 basalt 自己的文档为准；`D:\code\nivroos` 只作只读参考，**不得**回写。

### 文档体系（各司其职）

| 文档 | 角色 | 裁决顺位 |
| --- | --- | --- |
| [.specify/memory/constitution.md](.specify/memory/constitution.md) | 原则（十条 + 三条架构约束 + 开发工作流） | **最高**——原则冲突一律以它为准 |
| [docs/TechnicalSolution.md](docs/TechnicalSolution.md) | How（技术方案） | 文档之间**最权威**——冲突时以它为准 |
| [docs/DemandAnalysis.md](docs/DemandAnalysis.md) | What（需求与验收） | 次之 |
| [docs/AiProgrammingGuide.md](docs/AiProgrammingGuide.md) | How 落地（US 拆解 / 门禁 / 陷阱表） | 次之 |
| [docs/IndustryResearch.md](docs/IndustryResearch.md) | Why（业界调研） | 次之 |
| [docs/Compatibility.md](docs/Compatibility.md) | 与 NivroOS 的逐项差异与承诺 | 配套工件，冲突以技术方案为准 |
| [docs/EngineeringStandards.md](docs/EngineeringStandards.md) | 工程规范（Go / 测试 / 数据库 / 日志错误 / 提交与分支） | 配套工件，冲突以技术方案为准；提交格式以其 §5 为准 |
| [docs/contracts/](docs/contracts/) | 契约四件（run-and-errors / agent-md / schema.sql / openapi.yaml） | 固定字面量以此为准 |
| [docs/Operations.md](docs/Operations.md) | 运维手册（装 / 看 / 备份 / 升级 / 凭证） | 配套工件 |
| [docs/GettingStarted.md](docs/GettingStarted.md) | 起步手册（开发机环境 → `git init` → 三步链逐 US 操作） | 配套工件 |
| [docs/Roadmap.md](docs/Roadmap.md) | 增量阶段规划 | 规划类，**不入**权威序列；冲突以技术方案为准 |

**权威顺位（与宪法 Governance 同一份口径）**：**原则约束**以[宪法](.specify/memory/constitution.md)（经采纳的版本）为准；**实现设计**在四份权威文档之间以[技术方案](docs/TechnicalSolution.md)为准；**数据结构与 API 的固定字面量**以 [docs/contracts/](docs/contracts/) 为准。四份权威文档**不得**互相"修正"；有冲突先裁决再动手；冲突涉及原则变更时**必须**走宪法修订程序。

## 不可违背的原则（十条）

正式文本在 [.specify/memory/constitution.md](.specify/memory/constitution.md)（每条带「违反时的症状」与「复核方式」），与 [docs/TechnicalSolution.md](docs/TechnicalSolution.md) §1.1 逐条对齐（编号与顺序一致）。动工前**必须**逐条对照。

1. **自实现 ReAct 循环**——`go.mod` **不得**出现 Agent 框架坐标；循环体是本仓库代码，协议 SDK 不得接管循环或自动执行工具
2. **LLM 客户端只做协议调用**——**不得**启用任何自动工具循环；工具调度唯一归属 `ToolExecutor`；会话历史由**一条消息仓储**承载——正常运行中模型响应与工具结果由 `ReActLoop` 统一追加、用户输入由 `AgentService` 写入、崩溃 / 取消后的半截工具组由恢复路径补齐；**不得**出现第二套追加路径、**不得**重复追加
3. **Provider 必须显式映射**——装配处一次构建 `provider name → 客户端实例` 映射表，路由只查表；**不得**遍历推断（Go map 遍历顺序随机）
4. **一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露**——绑定只由 `AGENT.md` frontmatter 的 `skills:` 声明（声明即绑定，各平台一致）；**不得**引入软连接类机制；技能按 L1/L2/L3 渐进披露，**不得**预载正文、**不得**新增 `use_skill` 工具；声明名无实体 → 该 Agent 整只不注册
5. **审计 day-one 且 fail-closed**——**必须**先写 `pending` 审计记录再发调用，预写失败**不得**发出；返回后更新终态；**不得**以"日志够了"跳过落库
6. **授权与执行分离（两接口）**——`Policy.Check(ctx, action) error` 与 `CommandRunner.Run(ctx, cmd) (CommandResult, error)` 两接口；调用方**不得**依赖 `WhitelistPolicy` / `LocalRunner` 等具体实现
7. **同步执行 + context 只承载取消 / 截止 / 追踪**——**不得**引入异步编排；`context.WithValue` 只允许追踪标识，Profile / Session 等业务状态**必须**显式传递；**不得**用包级变量模拟 ThreadLocal
8. **单二进制纪律**——发布产物**必须** `CGO_ENABLED=0` 构建；`-race` 与发布构建**分属不同命令 / job**；依赖最小化——本仓技术栈表与已批准计划（spec / plan / 颗粒度文档）内的依赖**直接落地**，表外新增坐标或版本变更先问用户
9. **不用 ORM，手写 SQL**——`schema.sql` 幂等 + `schema_version` 受控前向迁移；已发布迁移只追加、**不得**改写
10. **五大能力优先，范围卡紧**——核心阶段范围**以需求文档 §6 与已批准的 US 清单（台账）为准**（管理台与观测、Agent 动态管理、每日 Demo、发布收尾等已登记交付物同属核心阶段）；企业级治理（认证 / 多租户 / 审计查询 / Tool Policy）放扩展阶段，完不成的挪阶段而**不得**砍验收

## 工程与门禁

- **门禁一条命令**：`make check`（= `fmt lint sec test test-integration cover build` 七目标聚合）全绿是"完成"的定义
- 工具链与全部依赖坐标见 [docs/TechnicalSolution.md](docs/TechnicalSolution.md) §1.2（2026-10-14 实测）；`go.mod` / `go.sum` 是版本唯一事实源。**版本语义**：`go` 行写的是**最低版本要求**（`go 1.27`），**不是**准确版本锁定——准确构建版本由 `toolchain` 指令（`toolchain go1.27.2`）与 CI 工具链配置（`actions/setup-go`）固定；**不得**写成"`go.mod` 锁定 1.27.2"
- **模块代理**：显式 `GOPROXY=https://goproxy.cn`（本机直连不可达——实测读数与设置见[起步手册 §2.6](docs/GettingStarted.md)）；写坐标前先 `go list -m -versions <module>` 核实
- 覆盖率底线（**必须有可执行判据**，`go test -cover` 不会自动失败）：`core` / `tool`（沙箱）/ `memory` / `storage` ≥ 70%，其余 ≥ 50%，由 US-1 交付的 `make cover`（纳入 `make check`）按包判失败——有可执行语句但无测试文件的包计 0%、未达标即失败，无可执行语句的包（`[no statements]`）跳过
- 格式修复只许 `gofumpt -w <本轮改动的文件>`（gofumpt 是 gofmt 的严格超集），禁手改格式；全仓格式检查（`gofumpt -l .`）**只读**——**不得**顺手重排无关文件（`/init-foundation` 的全仓初始格式化是唯一例外）
- 静态检查判定集**显式指定**：staticcheck 按 `-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022` 运行（US-1 的 Makefile `lint` 目标落地）——命名与注释规范由门禁判卷，见 [docs/EngineeringStandards.md](docs/EngineeringStandards.md) §1

## 工作流（三步链）

- 从零起步（开发机工具链、`git init`、逐 US 手动操作）见 [docs/GettingStarted.md](docs/GettingStarted.md)
- 每个 US 固定流程：`/module-doc-gen N` → `/us-register N` → `/module-dev N`；**`/speckit-analyze` 是实施前阻断检查**——`tasks` 确认后先跑一次，CRITICAL 未清零不得进 `implement`；收口再跑一次（实现与文档一致性核对）
- **US-1 也走三步链**；工程地基（`go.mod` / 包骨架 / Makefile / CI / 日志 / SQLite）的**装法**参照 `/init-foundation`（流程与验收仍以 US-1 颗粒度文档为准）
- **规范前置（编码类改动）**：写 / 改 Go 代码、SQL、日志、测试前**必须**先读 [docs/EngineeringStandards.md](docs/EngineeringStandards.md) 对应节（§1 Go / §2 测试 / §3 数据库 / §4 日志；提交口径 §5）——三步链 `/module-dev`、`/debug` 与一切直改均适用
- 台账 [docs/us/REGISTRY.md](docs/us/REGISTRY.md) 是 US 登记的**唯一登记处**（`/us-register N` 写入）；US 是**开放序列**（US-15 起顺延，编号只增不改），见 [docs/Roadmap.md](docs/Roadmap.md) §4
- **`git init` 由用户执行**——AI **不得**自行初始化仓库；同样**不得**自动 commit / push / tag
- 提交信息 = **英文** Conventional Commits 标题 + 四段**双语**正文（Problem / Design / Testing / Impact——每段先英文段、空行、再中文段逐段对照），规范原文见 [docs/EngineeringStandards.md](docs/EngineeringStandards.md) §5

## 常见陷阱（首版 6 条；全表 55 条见[编程指南 §7](docs/AiProgrammingGuide.md)）

| 陷阱 | 症状 / 修复 |
| --- | --- |
| `gofmt -l` / `gofumpt -l` 判卷顺序写反 | 格式差异时退出码为 0 而输出非空；语法错误时退出码非 0 而输出为空——**必须先查退出码、再判输出** |
| `-race` 与 `CGO_ENABLED=0` 混用 | 竞态检测需 CGO 开启、发布构建要求 CGO 关闭——两者**分属不同命令 / job** |
| 假设 build tag 测试会自动跑 | `go test ./...` 默认不执行 `-tags=integration` 的测试，**必须**单列一条命令 |
| 直连 proxy.golang.org | 本机不可达（实测读数见[起步手册 §2.6](docs/GettingStarted.md)）；**必须**显式带 `GOPROXY=https://goproxy.cn` |
| cron 表达式写 5 段 | `robfig/cron/v3` 默认 5 段制式，本项目要 **6 段**（秒 分 时 日 月 周）——**必须** `cron.NewParser(Second\|Minute\|Hour\|Dom\|Month\|Dow)`；时区用 `CRON_TZ=` 前缀，**不得**依赖进程时区 |
| 用 `winget` 装 Go | winget 源上的 Go 版本落后于本项目的构建版本（实测读数与装法见[起步手册 §2.1 / §2.2](docs/GettingStarted.md)）——**必须**从 [go.dev/dl](https://go.dev/dl/) 装官方 MSI；本机工具链**必须** ≥ `toolchain` 指令声明的版本，且与 CI 一致 |

陷阱表随每个 US 的实测发现增补（每条带「怎么发现」与「怎么复核」）；全表见编程指南 §7。

## 环境与约定

- 本机：Windows 11 + Git Bash；**开发机环境事实与实测读数一律见[起步手册](docs/GettingStarted.md) §2 / §8**，本节只留检查入口（工具链就绪判据、`GOPROXY` 回读、排障见起步手册 §2.8 / §7）；Go 工具链未就绪时，一切 `go` / `make` 命令**不跑**，如实登记为未验证
- 文档与代码注释用中文；关键注释（包级 / 导出符号级 / 复杂逻辑段）中英并列，中文在前
- 测试函数名**必须**英文（`TestXxx_yyy`）；文档 harness 的中文验收点以注释保留

## 硬性禁止

- 禁止自动 `git init` / commit / push / tag / 部署——时机由用户决定
- 禁止明文落盘凭证——一律 `${ENV_VAR}` 占位
- 禁止删断言 / 无理由跳过测试（`t.Skip` 须写明理由）/ 放宽阈值让门禁变绿
- `specs/**` 按状态区分：**当前 US 的草案可修订**（`clarify` / `analyze` 调整内容、`implement` 更新任务勾选）；**已批准范围的变更必须经用户确认**；**完成验收后的历史设计不得静默改写**（须用户裁决并在文档里留裁决记录）。已发布迁移脚本**不得**改写（只追加）
- 禁止自行修订 [.specify/memory/constitution.md](.specify/memory/constitution.md)——宪法修订**必须**经用户确认（走 `/speckit-constitution`）
