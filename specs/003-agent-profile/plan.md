# Implementation Plan: US-3 Agent 目录与 Profile 加载

**Branch**: `003-agent-profile` | **Date**: 2026-10-16 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/003-agent-profile/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 3` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us3-agent-profile.md](../../docs/us/us3-agent-profile.md) §2 / §3 为准，冲突时以 [docs/TechnicalSolution.md](../../docs/TechnicalSolution.md) 为准。

## Summary

把「一个目录 = 一个 Agent」从产品命题变成**可装载定义**：`.basalt/agents/<name>/AGENT.md` 经 frontmatter 解析（显式 snake_case 映射 + `*T` 区分「未声明」与「显式零值」）与**八条加载期校验**（任一条不过**整只不注册** + 点名 WARN，单只失败不拖垮整批）派生成 `core.AgentEntry` 结果集；system prompt **第一段**（identity.prompt → 自我称谓 → 正文 → bootstrap 文件 → 技能卡片 L1 → 当前时间）作为**无状态原语**可组装；`profile list / show / create / delete` 四条子命令可用（create / delete 走写模式实例锁准入；delete 语义为归档）。

技术路线的三个要点：**① 一份解析与校验服务两个入口**——`AgentLoader.LoadAll(root)` 是唯一装载入口（**不提供 `LoadOne`**），`profile show` 复用同一结果集过滤，「注册判定」与「加载」之间不存在第二套代码（FR-012 / 技术方案 §8.7「不得为同一语义写两份实现」）；**② 段一是无状态原语**——`SectionOne(root, entry, now)` 每次调用**重读**全部素材、时间由调用方**显式传入**（宪法 VII 的直接执行面：不缓存、不在函数内取真实当前时间、不用包级变量模拟 ThreadLocal），只接受 `Registered` 的条目；**③ 写模式准入是最小面 OS 锁**——`windows.LockFileEx` / `unix.Flock` 锁 `.basalt/basalt.lock` 的 `[0,1)` 一字节锁区，诊断记录（pid / 开始时间）写在锁定区**之外**的固定偏移 `4096` 起（Windows 排他锁阻止其他句柄读锁定区——诊断必须在区外，竞争者才能读到「是谁占着」）；强杀后由内核释放、不靠心跳过期判活（R1 裁决：最小面前移 US-3，重路径接线与恢复扫描归 US-4）。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）

**Primary Dependencies**: 既有坐标不动（`spf13/cobra` v1.10.2 / `goccy/go-yaml` v1.19.2 / `sashabaranov/go-openai` v1.43.0 / `modernc.org/sqlite` v1.60.1 / `stretchr/testify` v1.12.1）；**新增唯一坐标** `github.com/robfig/cron/v3` v3.0.1（角色：6 段 cron 解析与触发时间计算——装载期校验与共享验收样例的「未来十次触发」判据）；`golang.org/x/sys` v0.48.0 由 indirect **迁直接依赖**（角色：平台锁面 `windows.LockFileEx` / `unix.Flock`；**版本不变、无新增坐标**）。两者均在宪法技术栈表与固定技术栈句内，**直接落地**。无 `import _` 副作用、无 `init()`、不启动后台 goroutine。标准库：`os.Root`（钉根读写与目录扫描）、`time`（`LoadLocation`）、`log/slog`、`testing`。

**Storage**: **无**——本 US 不建表、不开库、不写 SQL（颗粒度文档 §3.4）；`profile list / show` 轻路径不开数据库、不取实例锁、不做恢复；写模式命令只动文件系统（`.basalt/basalt.lock` + `agents/` 与 `archive/` 目录）；Agent 注册结果走**日志与命令输出两轨**。

**Testing**: `go test -race ./...` 默认层——**六文件 66 条新增**（agent_test.go 18 / schedule_test.go 16 / context_loader_test.go 7 / agent_files_test.go 9 / lock_test.go 8 / cli/profile_test.go 8，逐条与颗粒度文档 §3.2 对齐）+ `app/ports_test.go` 追加 2 条（合计 68 条）；夹具 `t.TempDir()`、**不访问真实网络**、时间一律显式传入 `now time.Time`；跨进程判据用 **helper-process**（exec 自身测试二进制 + 环境变量分派 + 子进程 stdout 行同步——**不得**用睡眠等待）；**无集成层**（本 US 不触外部依赖）。平台分支读数分别登记：`lock_windows.go` 本机 Windows 11 执行（门禁读数），`lock_unix.go` 在 Ubuntu CI 编译 + 运行——**不得**因 CI 绿宣称 Windows 分支已验。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物四平台交叉编译（`CGO_ENABLED=0`）；`-race` 与发布构建分属不同命令 / job。

**Project Type**: CLI + 服务单二进制（本 US 交付命令面第 3 组子命令 `profile`（四子命令）+ 内部装载 / 拼装 / 锁面；HTTP 面归 US-9）

**Performance Goals**: 无显式吞吐 / 延迟指标（底座零件类 feature）；判据是**行为顺序**与**失败可解释**——校验先于注册、段一素材读取失败即报错点名、拒锁零写入、扫描序显式确定。

**Constraints**: 三条包纪律（依赖方向 / 接口定义在消费方 / 第三方实现放消费方包内）+ `core` 只依赖标准库且**只放声明（零可执行语句）**；`cli` **不得** import `workspace`（端口 `Agents` 定义在消费方、适配器在 `app`）；文件读写一律经 `os.Root`（含目录扫描；`0o750` / `0o644`；不覆盖已有文件）；无网络、无 DB 写入、无 `context.WithValue`；日志固定键 `agent` / `reason` / `detail`，不回显 YAML 原文与未知键值；覆盖率自本 US 起 `internal/workspace` 纳入 ≥ 50% 判卷（`internal/core` 按「只放声明」设计保持零语句、跳过）。

**Scale/Scope**: 约 9 个实现文件（core 1 + workspace 7 + cli 1）+ 6 个新增测试文件与 2 个测试文件改造；新增坐标 1 个 + 1 个迁直接（版本不变）；前序改造点 9 项（8 处文件 + 1 项占位点登记，逐条软门禁——§3.5）

**环境实测读数（2026-10-16，本轮当场命令）**: `go version` → `go1.27.2 windows/amd64`（与 `toolchain go1.27.2` 一致，退出码 0）；`GOPROXY=https://goproxy.cn go list -m -versions github.com/robfig/cron/v3` → `v3.0.0-rc1 v3.0.0 v3.0.1`（末位 `v3.0.1` 为拟用 GA，退出码 0）；`go.mod` 三行读取：`go 1.27` / `toolchain go1.27.2` / `golang.org/x/sys v0.48.0 // indirect`；本地模块缓存已含 `robfig/cron/v3@v3.0.1` 与 `golang.org/x/sys@v0.48.0`（H3 门禁可就地核实 API，无需网络）；`GOROOT` = `C:\Program Files\Go`。第三方 API 存在性读数（robfig `NewParser` / `Parse` / `CRON_TZ=`、`windows.LockFileEx`、`unix.Flock`、`os.Root` 方法面——**无 `ReadDir`**）逐条登记在 [research.md](./research.md)。工具链与静态检查工具就绪读数沿用 US-1 plan；本轮未复测的项不在此声称。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | **部分适用**：新增坐标 `robfig/cron/v3` 是**调度库**（非 Agent 框架——`go.mod` 框架坐标 grep 零命中判据不变）；循环体归 US-4，本 US 不实现 | 通过 |
| II | LLM 客户端只做协议调用 | 不适用——本 US 不触 LLM 客户端与会话历史（消息仓储零写入、零追加路径） | 通过 |
| III | Provider 必须显式映射 | **部分适用**：装载期的 provider 校验用**显式注入**的 `AgentLoaderOptions.Providers []string`（不遍历推断、不读映射表内部）；本 US 无路由面 | 通过 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露 | **适用（首个完整落地面）**：加载单元 = `agents/` 下每个子目录、`AGENT.md` 唯一定义入口；绑定**只**由 frontmatter `skills:` 声明（本地 `skills/` 目录被无视 + WARN）；L1 卡片渐进披露、**不得**预载正文、**不得**新增 `use_skill` 工具；声明名无实体 → 整只不注册（收尾 grep `use_skill` 零命中） | 通过 |
| V | 审计 day-one 且 fail-closed | 不适用——本 US 无模型调用、无工具执行、无审计写入（无网络面）；审计写入路径不受影响 | 通过 |
| VI | 授权与执行分离（两接口） | 不适用——本 US 不触 `Policy` / `CommandRunner` | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用（强形态）**：段一「`now` 显式传入、每次调用重读、不缓存」是「业务状态显式传递、**不得**用包级变量模拟 ThreadLocal」的直接执行面；全程同步、无异步编排；`context.WithValue` **零使用**（收尾 grep） | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：新增唯一坐标在技术栈表内（直接落地）+ `x/sys` 迁直接（**版本不变、无新增坐标**）；robfig / x/sys 均纯 Go、无 CGO 影响；`-race` 与发布构建分属不同命令 / job | 通过 |
| IX | 不用 ORM，手写 SQL | 不适用——本 US 无 SQL、无表、无迁移（§3.4「无」）；已发布迁移不受影响 | 通过 |
| X | 五大能力优先，范围卡紧 | 通过——交付面对照颗粒度文档 §5 明确不做清单逐项边界（契约字段表之外零新增概念）；实例锁**最小面前移** US-3 经用户 R1 裁决落案（§2.4 注 1），重路径与恢复扫描归 US-4 | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 唯一装配层——本 US 的落法：`cli` 自声明 `Agents` 端口（消费方定义）、`app.agentsPort` 是适配器（唯一允许 import 实现包的地方）；接口定义在消费方；无环由 CI 用 `go list` 复核。**cron 适配包的适用范围说明（防漂移）**：宪法架构约束「cron 等第三方库的适配**必须**放在独立内部包」——本 US 只交付**表达式规范化**（`CronSpec`）与**装载期解析校验**，**无调度注册与执行**（§5 明确不做，归 US-10），故本 US **不建** `internal/adapters/cron`；robfig 只出现在 `internal/workspace`，US-10 落地注册面时按宪法建适配包。同类语义不得第二套实现：装载 / 注册判定共用 `LoadAll`，调度 id 占用单次装载内整只提交。凭证纪律不受影响（本 US 无凭证面——`AGENT.md` 内 `${ENV_VAR}` 解析归后续模块，§5）。

**开发工作流**：门禁一条命令 `make check`（七目标，**Makefile 无改动**——新增测试随既有 `go test` 目标执行，`workspace` 覆盖率判卷自本 US 起自动纳入；`core` 保持 `[no statements]` 跳过）；三条实测纪律适用（格式判卷先退出码后输出 / `-race` 与 `CGO_ENABLED=0` 分离 / 本 US **无集成层**，`test-integration` 跑既有用例）；提交纪律（AI 不自动 commit / push）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `core` 新增 `profile.go` 为**纯声明**（零语句、零 import）；`workspace` 依赖 `core`（+ robfig / x/sys，仅在实现与平台文件内）；`cli` 依赖 `core` 并新增 `Agents` 端口——**不得** import `workspace`（既有 `internal/cli/ports_test.go` 的禁止清单**已含** `workspace`，本 US 不新增判据文件，只保持纪律）；`app` 装配全部；`cmd/basalt` 不动。无反向依赖（收尾 `go list -deps` 复核） |
| 审计写入路径唯一 | 本 US **无审计写入**（无网络调用）；SQL 零新增 |
| 无框架坐标 | `go.mod` 新增唯一坐标 `robfig/cron/v3`（调度库，非 Agent 框架）；收尾 `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中 |
| 无 `context.WithValue` 业务态 | 本 US `context.WithValue` 零使用（收尾 grep 零命中）；业务状态（Profile / providers 名集 / now）一律显式类型化参数 |
| 单二进制构建通过 | robfig / x/sys 均纯 Go；平台锁分支经构建约束分文件（`lock_windows.go` / `lock_unix.go`）；`build` 目标 `CGO_ENABLED=0 go build`；`-race` 只在 `test` 目标 |

**Phase 1 后复评（post-design）**：设计产物（[research.md](./research.md) / [data-model.md](./data-model.md) / [quickstart.md](./quickstart.md)）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/003-agent-profile/
├── spec.md              # /speckit-specify 输出（需求规格，含 Clarifications 会话 2026-10-16）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（决策 + 写前必须实测核实清单）
├── data-model.md        # Phase 1 输出（实体、状态机与校验规则）
├── quickstart.md        # Phase 1 输出（可复制执行的验证步骤）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

**`contracts/` 本 feature 不新建**（与 US-1 的差异，理由同 US-2）：本 US 的固定字面量**已有唯一事实源**——AGENT.md 字段表与八条校验在 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md) §2、锁语义在 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md) §4.3.1、渲染模板 / 日志键 / schedules detail 表在颗粒度文档 §6「本文固定口径」、导出签名逐字来源在颗粒度文档 §3.1、兼容判据在 [docs/Compatibility.md](../../docs/Compatibility.md) §5.1。再抄一份到 `specs/` 就是**第二份字面量**——抄一份必漂移一处（颗粒度文档 §3.3 的同一理由：契约文件不新建，只引用）。plan 的契约节只**引用**，不复制。

### Source Code (repository root)

```text
internal/
├── core/
│   └── profile.go            # 新增：Profile / Identity / ProviderRef / Channel / Settings / Schedule /
│                             #       AgentEntry（**纯声明，零语句**；不带 yaml 标签——映射归 workspace DTO）
├── workspace/
│   ├── agent.go              # 新增：AgentLoaderOptions / AgentLoader / NewAgentLoader / LoadAll / CronSpec /
│   │                         #       frontmatterDTO 全族 + 非导出辅助（围栏切分、未知键扫描、intoProfile、
│   │                         #       缺省填充、validateSchedules、reject、frontmatterErrorSummary…）
│   ├── skill.go              # 新增：skillCard（name / description / path 三要素）
│   ├── context_loader.go     # 新增：ContextLoader / NewContextLoader / SectionOne（段一无状态原语）
│   ├── agent_files.go        # 新增：ValidateAgentName / CreateAgent / DeleteAgent（写模式内部取锁）
│   ├── lock.go               # 新增：InstanceLock / AcquireInstanceLock / Release / LockBusyError（公共形态）
│   ├── lock_windows.go       # 新增：Windows 分支（windows.LockFileEx，构建约束分文件）
│   ├── lock_unix.go          # 新增：Unix 分支（unix.Flock；Ubuntu CI 编译 + 运行）
│   ├── templates.go          # 改造：新增 Agent 模板构造（frontmatter 经 yaml.Marshal 编码，不插值）
│   ├── doc.go                # 改造：包职责句补 AgentLoader / ContextLoader / 实例锁
│   ├── agent_test.go         # 新增：18 条（§3.2）
│   ├── schedule_test.go      # 新增：16 条
│   ├── context_loader_test.go # 新增：7 条
│   ├── agent_files_test.go   # 新增：9 条
│   └── lock_test.go          # 新增：8 条（含 helper-process 跨进程主判据）
├── cli/
│   ├── profile.go            # 新增：newProfileCommand 四子命令 + runProfile* 四函数 + renderProfile* 两函数
│   ├── profile_test.go       # 新增：8 条
│   ├── ports.go              # 改造：新增 Agents 端口（List / Create / Delete 三方法）
│   ├── deps.go               # 改造：Deps 新增 Agents 字段
│   ├── root.go               # 改造：注册 newProfileCommand(deps)
│   └── ports_test.go         # 改造：既有依赖方向判据沿用（计数不变）
├── app/
│   ├── ports.go              # 改造：新增 agentsPort 适配器（三方法转调 workspace）
│   ├── ports_test.go         # 改造：追加 2 条（List 转发 loader 条目 / Create+Delete 达 workspace）
│   └── run.go                # 改造：装配注入 Agents: agentsPort{WorkDir, Logger}
└── （其余包不动：channel / memory / provider / storage / tool / web / testutil）

go.mod / go.sum              # 新增唯一坐标 github.com/robfig/cron/v3 v3.0.1；golang.org/x/sys v0.48.0 迁直接（版本不变）
Makefile                     # 无改动（七目标不变；workspace 覆盖率判卷自本 US 起自动纳入）
.github/workflows/ci.yml     # 无改动——**不得**据此宣称覆盖 Windows 锁分支（CI 跑 lock_unix.go；lock_windows.go 只保证可编译）
```

**Structure Decision**: 单 Go module + `internal/` 分包（无多模块、无 `src/` 层）；本 US 新增实现文件 9 个（`core/profile.go`、`workspace/{agent,skill,context_loader,agent_files,lock,lock_windows,lock_unix}.go`、`cli/profile.go`）+ 新增测试文件 6 个 + 测试文件改造 2 个；修改 8 处文件（`cli/{ports,deps,root}.go`、`app/{ports,run}.go`、`workspace/{templates,doc}.go`——**均为颗粒度文档 §3.5 点名的前序改造点，逐条为软门禁**）+ `go.mod` / `go.sum`；§3.5 第 9 项为**占位点登记**（非文件改动：tools 门禁 → US-4、MCP 连接半条 → US-7、快照与原子发布 → US-4 / US-11、`schedules` 注册 → US-10、记忆注入 → US-6、REST / 管理台 → US-9 / US-12、锁重路径与恢复扫描 → US-4）。与台账「模块落位（摘要）」列一致：`go.mod`（新增 `robfig/cron/v3 v3.0.1`；`golang.org/x/sys` 迁直接）、`internal/{workspace,core,cli}`、`internal/app`（既有适配器改造）。

**分层落位（三条包纪律的逐条落地）**：

| 纪律 | 本 US 的落法 |
| --- | --- |
| 依赖方向无环 | `core` 新增纯声明（零语句、零 import）；实现进 `workspace`（robfig / x/sys 只在此包）；`cli` 消费 `core` 与自声明端口（**不** import `workspace`——既有 `ports_test.go` 禁止清单判卷）；`app` 装配全部 |
| 接口定义在消费方 | `cli.Agents` 三方法（`List` / `Create` / `Delete`）由 `app.agentsPort` 实现（编译期断言 `var _ Agents = agentsPort{}`）——`providers` 名集与 `now` 是**显式参数**（§2.4 注 2 / 注 11） |
| 第三方实现放消费方包内 | robfig 只在 `internal/workspace`（解析与 Next 计算；适配包 `internal/adapters/cron` 归 US-10，见 Constitution Check 说明）；`x/sys` 只在 `lock_windows.go` / `lock_unix.go` 两个平台文件内 |

**标签口径（`[US1]` 不混淆）**：`tasks.md` 中的 `[US1]` / `[US2]` / `[US3]` 是 Spec-Kit 对**本 feature 内部**用户故事的编号（对应 [spec.md](./spec.md) 的 User Story），**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 / US-2 / US-3——本 feature 的全部故事都落在项目 **US-3（Agent 目录与 Profile 加载）** 一个模块内。标签按工具产出保留，不另造编号。

## Complexity Tracking

> 仅当 Constitution Check 存在需辩护的违规时填写。

无违规——本 feature 无 Constitution Check 不通过项，故无复杂度辩护条目。
