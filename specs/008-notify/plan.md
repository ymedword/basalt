# Implementation Plan: US-8 通知工具（notify）

**Branch**: `008-notify` | **Date**: 2026-10-18 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/008-notify/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 8` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us8-notify.md](../../docs/us/us8-notify.md) §2 / §3 为准，冲突时以 [docs/TechnicalDesign.md](../../docs/TechnicalDesign.md) 为准（§1.1 原则 II / V / VI / VII、§1.3 零新增坐标、§3.7 与 §6.4 的 `${VAR}` 解析时机裁决、§4.1~§4.5 工具契约与审计、§6.4 通知渠道与发送边界、§7.1.2 包纪律、§8.2 US-8 行）；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准（`agent-md.md` §5.5 `notify` 参数契约、`run-and-errors.md` §7.2 两阶段写 / §7.3 `error_kind` 七值、`schema.sql` `notify_channels` 表）。

## Summary

给 Agent 装上**出站推送闭环**的最后一段：模型在 ReAct 循环里调用 `notify`，把巡检结论 / 摘要 / 告警推到管理员登记过的渠道，不再只能"说给终端里的用户听"。四件可判卷的价值：**① 出站推送闭环**（登记渠道 → 一次调用 → 接收端真实收到 → 固定成功文案）；**② 凭证零明文落盘**（库表只存 `${ENV_VAR}` 引用，发送时才解析；任何可观察面只出 `scheme://host`）；**③ 投递真值**（飞书业务失败同样回 HTTP 200，成功判据是回执 `code == 0`；不可解析一律判失败，"结果未知"不冒充成功）；**④ 类型支持集只有一份真值**（`type → adapter` 只在装配层一份映射，工具内核零类型分支，加类型只改一处）。

技术路线的五个要点：**① 共享判定收敛**——`internal/core/envref.go` 的 `EnvPlaceholderName` 成为 `${VAR}` 整串判定的**唯一实现**，`workspace/mcp.go` 与 `cli/config.go` 两处既有同义实现改调它（等价重构，判据面零变更）；**② 只读注册表**——`internal/storage/notify_store.go` 的 `NotifyChannelRepo` 走只读连接池（`db.read`），SQL 固定 `SELECT name, type, url FROM notify_channels ORDER BY name`，每次发送读一次、零缓存、零迁移；**③ 工具纯判定**——`internal/tool/notify.go` 的 `NotifyTool` 依次做解码 → 20 KiB 上限 → 查表 → 类型分派 → `${VAR}` 形态 → 发送时环境解析 → `Policy.Check` → 发送，任一步失败都**零外呼**；工具不落审计、不写消息、不记日志；**④ 适配器语义分离**——`internal/tool/notify_adapters.go` 的 webhook（2xx 即成功，仅表示接收端接受请求）与 feishu（2xx 且 `code == 0`；回执最多 64 KiB 且显式检测超限）共用 `notifyPost` 发送与脱敏链，不跟随重定向、不读环境代理、ctx 身份优先；**⑤ 装配唯一**——`internal/app/notify.go` 的 `newNotifyAdapters()` 是类型支持集唯一真值（恰好 `webhook` / `feishu` 两键），`wire.go` 在等值门禁**之前**注册第 9 件内置工具，`declaredToolNames` 同步九名（排序），轻 / 重两条路径结论一致。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）

**Primary Dependencies**: **零新增坐标**——`go.mod` / `go.sum` **零 diff**（判据 `git diff -- go.mod go.sum` 为空）。全部既有：标准库（`context` / `encoding/json` / `errors` / `fmt` / `io` / `net/http` / `net/url` / `os` / `strings` / `unicode/utf8`）+ 既有 `net/http` 客户端（复用 `internal/tool/httptools.go` 的 `newHTTPClient` / `httpDoer` / `httpTransportReason` / `ctxPreferred` / `discardBody`）+ 既有 `stretchr/testify v1.12.1`（测试）。零 `import _` 副作用注册。

**Storage**: **零迁移、零表 / 列 / 索引改动**——`notify_channels` 自 US-1 初始 schema 就在 [internal/storage/schema.sql:216](../../internal/storage/schema.sql#L216) 与 [docs/contracts/schema.sql:216](../../docs/contracts/schema.sql#L216)（逐字相同）。本 US 只读：`internal/storage/notify_store.go` 的 `NotifyChannelRepo.ListNotifyChannels` 走 `db.read` 只读池，SQL 固定 `SELECT name, type, url FROM notify_channels ORDER BY name`；空表返回空切片。渠道增删改查归 US-12（发送路径每次读表，管理端改动下一发即生效）。审计继续落既有 `tool_invocations`（两阶段写仍由 `core.ToolExecutor` + `storage.ToolInvocationRepo` 承载，**本 US 零改动**）。判据：`git diff -- internal/storage/schema.sql docs/contracts/schema.sql` 为空；`internal/storage/migrations/` 零新增文件。

**Testing**: 默认层 `go test -race ./...`——**新增 5 个测试文件共 41 条**（`core/envref_test.go` **1** / `storage/notify_store_test.go` **4**（S1~S4）/ `tool/notify_test.go` **21**（N1~N21）/ `tool/notify_adapters_test.go` **13**（A1~A13）/ `app/notify_test.go` **2**（P1~P2））+ **改动 1 个测试文件 1 条**（`app/ports_test.go` 的 `TestToolListerPort_HeavyAssemblyRegistersEightTools` → `TestToolListerPort_HeavyAssemblyRegistersNineTools`，**判据面变更**，人工 review ① 已批准）= **42 条**。**默认层不访问真实网络**：适配器用例全部经 `fakeDoer`（`httptools_test.go` 既有夹具，见 [httptools_test.go:68](../../internal/tool/httptools_test.go#L68)）或 `testutil.NewHTTPServer`（仅回环，[internal/testutil/httpserver.go:14](../../internal/testutil/httpserver.go#L14)）；策略用 `spyPolicy` / `notifyPolicySpy`；不连真实 webhook / 飞书、不调模型、不起子进程。**无集成层**：真实 webhook 与飞书需要凭证与外部账号，不可在 CI 复现——归颗粒度文档 §6.1 人工项，`test-integration` 在本 US 维持"空跑 + 文档依据"的既有状态（陷阱 55：记「跳过 + 依据」，不得写成通过）。**测试缝纪律**：适配器经非导出构造器 `newWebhookNotifyAdapter(doer)` / `newFeishuNotifyAdapter(doer)` 注入 `fakeDoer`；工具经 `lookupEnv` 注入环境查询；**不得**新增导出测试 API。并发面由 `-race` 覆盖（R12 阻塞适配器 + 执行器两阶段审计；工具与适配器无共享可变状态）。关键回归 **R1~R12 与 A4 / A10** 原样落地，函数名英文、文档中文验收点以注释保留；每条带逆改点（见颗粒度文档 §4.3）。覆盖率：`core` / `tool` / `storage` ≥ 70%，`app` ≥ 50%，由既有 `make cover` 按包判失败（**不改脚本与阈值**）；`internal/core/notify.go` 只有类型与接口声明（包内 `[no statements]` 面），`envref.go` 有可执行语句并由 `envref_test.go` 覆盖。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 交叉编译（四目标编译矩阵是人工项，不进 `make check`）；`-race` 与发布构建**分属不同命令 / job**。HTTP 客户端在三个平台上同一套标准库实现，无平台分片。

**Project Type**: CLI + 服务单二进制；本 US **不新增内部包、不新增顶层目录、不新增命令**——全部落在既有 `internal/{core,storage,tool,app,workspace,cli}` 内。

**Performance Goals**: 无吞吐 / 延迟指标（底座零件类 feature）；判据是**行为边界**——`content` 上限 20 × 1024 字节（UTF-8 计，查表之前判）；飞书回执上限 64 × 1024 字节（显式判超限，不截断解析）；每次发送一次注册表读取（不缓存）；同步单请求，无重试 / 队列 / 限流（均属扩展阶段或后续 US）。

**Constraints**: 三条包纪律——`core` **只依赖标准库**（`envref.go` / `notify.go` 零 import 内部包）；`storage` 实现 `core.NotifyChannelReader`；`tool` **不得** import `app` / `cli` / `workspace`；`app` 是唯一装配层。**接口定义在消费方**：`NotifyChannelReader` 由 `core` 声明、`storage` 实现、`tool` 消费；`NotifyAdapter` 由 `tool` 本包声明、`app` 注入映射。**审计零改动**：工具与适配器不落审计、不写消息、不记日志（构造函数无 logger 参数），两阶段写仍只在既有执行器链路。**同步执行**：`Execute` / `Send` 同步；`context.WithValue` 零使用；不建 goroutine、不引入异步编排。**安全边界**：真实地址（端口 / userinfo / path / query）不进结果 / 日志 / 审计 / 错误文案，只出 `scheme://host`；沙箱拒绝保留 `Rule` / `Guidance` 原文、只重写 `Target`；传输错误只进 `ExternalError.Cause`。**范围零膨胀**：零新增配置键、`basalt.yaml` / `basalt.yaml.example` 零 diff、`Makefile` / CI / `scripts/**` 零改动、`schema.sql` 零 diff。**静态门禁**：抑制注释（`//nolint` / `#nosec` / `//lint:ignore`）**零使用**；测试函数名英文 `TestXxx_yyy`；格式只许 `gofumpt -w <本轮改动的文件>`。

**Scale/Scope**: 代码 **新增 6 文件 + 修改 3 文件**（`core` 2 新 / `storage` 1 新 / `tool` 2 新 / `app` 1 新 1 改 / `workspace` 1 改 / `cli` 1 改）；测试 **新增 5 文件 41 条 + 修改 1 文件 1 条**；`go.mod` / `go.sum` **零 diff**；零迁移；零新增配置键。前序改造点 6 项（颗粒度文档 §3.5）：其中仅 `ports_test.go` 八件 → 九件是**判据面变更**（已批准），`${VAR}` 收敛是**等价重构**（判据面零变更），`wire.go` 装配变更是生产装配由 R11 与 ports 用例覆盖。

**环境实测读数（2026-10-18，本轮当场命令）**: 分支 `008-notify`；HEAD `4c6f05643c9f07535f6d946c577a8605b652544b`。核对属实：注册循环在 [internal/app/wire.go:232-246](../../internal/app/wire.go#L232-L246)（注释逐字 "Eight tools"）；等值门禁在 [wire.go:253-257](../../internal/app/wire.go#L253-L257)（`slices.Equal(registry.Names(), declaredToolNames)`）；MCP 段注释在 [wire.go:259-264](../../internal/app/wire.go#L259-L264)；`declaredToolNames` 在 [wire.go:418-421](../../internal/app/wire.go#L418-L421)（八名排序）；`ports_test.go` 用例在 [ports_test.go:301-327](../../internal/app/ports_test.go#L301-L327)（`TestToolListerPort_HeavyAssemblyRegistersEightTools`，恰好八件 + 可解析循环）。`${VAR}` 两处既有实现：`isEnvPlaceholder` 在 [internal/workspace/mcp.go:137-157](../../internal/workspace/mcp.go#L137-L157)（逐字符判定 `[A-Za-z_][A-Za-z0-9_]*`，与文档算法逐字节等价）、`wholePlaceholder` 在 [internal/cli/config.go:155](../../internal/cli/config.go#L155)（`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`）；`placeholder`（替换用）在 [config.go:158](../../internal/cli/config.go#L158) 保留。HTTP 面：`httpDoer` 在 [internal/tool/httptools.go:64](../../internal/tool/httptools.go#L64)；`newHTTPClient` 在 [httptools.go:170-186](../../internal/tool/httptools.go#L170-L186)（`CheckRedirect` → `http.ErrUseLastResponse`、`Transport.Proxy = nil`）；`httpTransportReason` 在 [httptools.go:518-529](../../internal/tool/httptools.go#L518-L529)；`ctxPreferred` 在 [httptools.go:538-553](../../internal/tool/httptools.go#L538-L553)；`discardBody` 在 [httptools.go:561-566](../../internal/tool/httptools.go#L561-L566)。core 面：`Tool` 在 [core/tool.go:29-34](../../internal/core/tool.go#L29-L34)、`Result` 在 [core/tool.go:39-42](../../internal/core/tool.go#L39-L42)；`ToolEnv.AgentName` 在 [core/run.go:119-126](../../internal/core/run.go#L119-L126)；`Policy` 在 [core/store.go:108-112](../../internal/core/store.go#L108-L112)；`SandboxViolationError` / `ExternalError` / `InvalidInputError` 分别在 [core/errors.go:59](../../internal/core/errors.go#L59) / [:128](../../internal/core/errors.go#L128) / [:158](../../internal/core/errors.go#L158)。执行器分类判序在 [core/tool_exec.go:221-246](../../internal/core/tool_exec.go#L221-L246)（沙箱 → 入参 → ctx → 外部 → `fs.ErrNotExist` → 兜底 `runtime_error`）。仓储面：`DB.read` 在 [storage/db.go:24-26](../../internal/storage/db.go#L24-L26)；只读查询先例在 [storage/tool_invocations.go:124](../../internal/storage/tool_invocations.go#L124)。表在 [internal/storage/schema.sql:216-222](../../internal/storage/schema.sql#L216-L222)（`name` PK / `type` / `url` / `description` / `created_at`）。**本轮未复测的项不在此声称**（**未跑** `make check`、**未跑**覆盖率读数、**未跑**四目标编译）。工具链读数沿用 US-1 / US-7 plan。

### 文档 ↔ 仓库事实差异（实测发现）

**A. 与文档一致、作为落地基点的仓库事实**（逐条已核，不构成差异）：

| # | 文档写法 | 仓库实测 | 落地口径 |
| --- | --- | --- | --- |
| 1 | §3.1：`workspace/mcp.go` 删本地 `isEnvPlaceholder`，改调 `core.EnvPlaceholderName` | [mcp.go:142-157](../../internal/workspace/mcp.go#L142-L157) 判定算法 = 长度 ≥ 4 + 前缀 `${` + 后缀 `}` + 首字符字母/下划线 + 其余字母/数字/下划线 | `EnvPlaceholderName` 逐字节复刻该算法（新增注释保留原语义）；`parseMCPServers` 的 env 校验改调；错误文案不变 |
| 2 | §3.1：`cli/config.go` 删 `wholePlaceholder` 正则，改调 `core.EnvPlaceholderName` | [config.go:155](../../internal/cli/config.go#L155) 正则 `^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`；[config.go:158](../../internal/cli/config.go#L158) 的 `placeholder`（非锚定）是"串内查找替换"的另一操作 | 删除 `wholePlaceholder` 变量与 `regexp` 依赖中相应使用；凭证键整串判定改调 `EnvPlaceholderName`；`placeholder` 保留 |
| 3 | §3.1：`wire.go` 在记忆工具之后追加第 9 件、门禁之前 | [wire.go:232-246](../../internal/app/wire.go#L232-L246) 注册循环八件；门禁 [:253](../../internal/app/wire.go#L253) | `tool.NewNotifyTool(storage.NewNotifyChannelRepo(db), policy, newNotifyAdapters())` 追加到注册切片末尾；门禁表达式零改动 |
| 4 | §3.1：`declaredToolNames` 九名（排序）、三处注释八件 → 九件 | [wire.go:418-421](../../internal/app/wire.go#L418-L421) 八名；注释在 [:232](../../internal/app/wire.go#L232)、[:259](../../internal/app/wire.go#L259)、[:409](../../internal/app/wire.go#L409) | 加 `"notify"`（排序位于 `list_dir` 与 `read_file` 之间）；三处注释同步九件，并注明第 10 件归 US-10 |
| 5 | §3.1：`ports_test.go` 改名并加入 `notify` | [ports_test.go:301-327](../../internal/app/ports_test.go#L301-L327) 现为恰好八件 + 五件可解析断言 | 用例改名 `TestToolListerPort_HeavyAssemblyRegistersNineTools`；期望名集与可解析循环加入 `notify`；**保留精确集合比较**（判据面变更已批准） |
| 6 | §4.1：适配器用 `fakeDoer` / `testutil.NewHTTPServer`，不访真实网络 | `fakeDoer` 在 [httptools_test.go:68](../../internal/tool/httptools_test.go#L68)；`NewHTTPServer` 在 [testutil/httpserver.go:14](../../internal/testutil/httpserver.go#L14)（回环） | 适配器测试复用 `fakeDoer`；需要真实回环的 R8 用 `NewHTTPServer` 的同源夹具；不新增外呼夹具 |
| 7 | §3.1：仓储只读走 `db.read` | [db.go:24-26](../../internal/storage/db.go#L24-L26) `DB{write, read}`；只读查询先例 [tool_invocations.go:124](../../internal/storage/tool_invocations.go#L124) | `NotifyChannelRepo{db *DB}` 用 `r.db.read.QueryContext`；不触碰 `write` 池 |

**B. 实测差异**：本轮逐条核对 §3.1 的签名、行号与既有实现，**未发现文档与仓库的事实差异**（US-7 的两处签名漂移未在本 US 重现）。若实现期出现新差异，按颗粒度文档「待决事项」口径回文档登记，不静默自决。

**C. 文档内部口径的落地解释（一条）**：§4.1 写 `internal/core/notify.go` 只有类型与接口声明（`[no statements]`），按 `make cover` 既有规则跳过。`make cover` 的判卷单位是**包**而非文件——`core` 包整体有可执行语句（含本 US 新增的 `envref.go`），因此 `notify.go` 不触发"跳过"、也不降低包覆盖率；`envref.go` 的可执行语句由 `envref_test.go` 覆盖并参与 `core` 包判卷。落地时不新增覆盖率豁免。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | **适用（零改动）**：`go.mod` 零新增坐标、零框架；`notify` 是普通 `core.Tool`，工具调度唯一入口仍是 `core.ToolExecutor`；循环体零改动、不按工具来源分支 | 通过 |
| II | LLM 客户端只做协议调用 | **适用**：不触碰 LLM 客户端；`notify` 不写消息、不落审计、不记日志；历史写入仍由既有三入口分担，审计仍由 `ToolExecutor` 两阶段写 | 通过 |
| III | Provider 必须显式映射 | **不适用（本 US 不触碰 provider 路由）**；但 `type → adapter` 映射沿用同一纪律：装配层一次构建、工具只查表、不遍历推断 | 通过 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露 | **适用（弱形态）**：`notify` 进入内置工具池与 `declaredToolNames` 九名，`AGENT.md` 的 `tools:` 声明可解析到它；技能面零改动，不新增 `use_skill`、不预载技能正文 | 通过 |
| V | 审计 day-one 且 fail-closed | **适用（零改动 + 复用判据）**：`notify` 调用经既有执行器两阶段写（预写失败 → 不发出；R12 在适配器阻塞期间观察 `pending` 行）；工具与适配器自身零写入；审计 SQL 仍只在 `internal/storage` | 通过 |
| VI | 授权与执行分离（两接口） | **适用**：`notify` 只依赖 `core.Policy` 接口做发送前判定；不依赖 `WhitelistPolicy` 具体实现；不使用 `CommandRunner`（通知不是受控命令执行） | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用（强形态）**：`Execute` / `Send` 同步；无 goroutine / 队列 / 异步编排；`context.WithValue` 零使用；Agent 名经 `core.ToolEnv` 显式传递；无包级可变状态 | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：`CGO_ENABLED=0` 发布构建；`-race` 与发布构建分属不同命令；`go.mod` / `go.sum` 零 diff（零新增坐标） | 通过 |
| IX | 不用 ORM，手写 SQL | **适用（只读一条 SQL）**：新增唯一 SQL `SELECT name, type, url FROM notify_channels ORDER BY name`，手写、在 `storage`；零迁移、零 schema 改动 | 通过 |
| X | 五大能力优先，范围卡紧 | **适用**：交付面对照颗粒度文档 §5 逐项边界（渠道 CRUD / 钉钉企微原生协议 / 重试队列死信幂等 / 限流 / 通知历史 / 按渠道覆盖超时代理 TLS / CLI 子命令 / 环境热更新 / 读取缓存 / 迁移 / 审计表变更 / `description` 展示 **全不做**）；对外概念零发明（签名逐字以 §3.1 为准） | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 唯一装配层——本 US 的落法：`core` 新增 `envref.go` / `notify.go`（**零 import 内部包**，判据机器可判）；`storage` 新增 `notify_store.go` import `core` + `database/sql` + 标准库；`tool` 新增 `notify.go` / `notify_adapters.go` import `core` + 标准库（**不** import `app` / `cli` / `workspace` / `storage`）；`app` 新增 `notify.go` import `core` + `tool` + 标准库，`wire.go` 追加注册与名集；`workspace` / `cli` 各改一处调用点（import `core`，不新增依赖方向）。**同类语义唯一实现**：`${VAR}` 整串判定收敛为 `core.EnvPlaceholderName`；类型支持集收敛为 `app.newNotifyAdapters()`。无环由 `go list` 复核（现有 10 包不变）。

**开发工作流**：门禁一条命令 `make check`（七目标；`Makefile` / CI / 覆盖率脚本**零改动**）；三条实测纪律适用（格式判卷**先退出码后输出** / `-race` 与 `CGO_ENABLED=0` **分离** / 集成层维持空跑并登记依据）；覆盖率不可豁免（`core` / `tool` / `storage` ≥ 70%，`app` ≥ 50%）；提交纪律（AI **不自动** commit / push / tag）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `core` 新文件只 import 标准库（收尾 `GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core` 复跑）；`tool` 不 import `app` / `cli` / `workspace` / `storage`；`storage` 只 import `core` + 标准库；`app` 装配全部 |
| 审计写入路径唯一 | 工具与适配器不写审计（构造参数无审计依赖）；两阶段写仍只在 `core.ToolExecutor` → `storage.ToolInvocationRepo`；`grep -rn 'INSERT INTO tool_invocations' --include='*.go' internal` 只在 `internal/storage` |
| 无框架坐标 | `go.mod` 零 diff；`grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中 |
| 无 `context.WithValue` 业务态 | 收尾 `grep -rn "context.WithValue" --include='*.go' cmd internal` 每处命中人工确认；`notify` 的 Agent 名经 `core.ToolEnv` 显式传递 |
| 单二进制构建通过 | 本 US 无新平台分片文件；`build` 目标 `CGO_ENABLED=0 go build`；`-race` 只在 `test` 目标 |
| 静默与双轨可观测性 | 工具 / 适配器无 logger；失败路径的审计落库 + WARN 仍由 `ToolExecutor` 统一承担（R12 覆盖） |
| 凭证红线 | 全部对外文案只出 `scheme://host`；R1 / A5 / R4 与 §6.1 第 7 项人工 grep 覆盖 |

**Phase 1 后复评（post-design）**：设计产物（[research.md](./research.md) / [data-model.md](./data-model.md) / [quickstart.md](./quickstart.md)）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/008-notify/
├── spec.md              # /speckit-specify 输出（含 Clarifications 会话 2026-10-18：5 条收敛项）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（D1~D10 决策与写前核实清单）
├── data-model.md        # Phase 1 输出（实体、字段、校验与状态流转）
├── quickstart.md        # Phase 1 输出（可复制执行的验证步骤）
├── run-state.md         # 过程状态（module-dev 步骤 0 建；入库、收口不删）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出；16/16）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

**`contracts/` 本 feature 不新建**（与 US-1~US-7 同口径）：本 US 的固定字面量**已有唯一事实源**——`notify` 参数契约与成功文案在 [docs/contracts/agent-md.md](../../docs/contracts/agent-md.md) §5.5；失败分类七值与审计形状在 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md) §7.2 / §7.3；`notify_channels` 表在 [docs/contracts/schema.sql](../../docs/contracts/schema.sql)；导出签名逐字来源在颗粒度文档 §3.1、固定口径 25 条在 §6.2。再抄一份到 `specs/` 就是**第二份字面量**——抄一份必漂移一处。plan 的契约节只**引用**，不复制。

### Source Code (repository root)

```text
internal/
├── core/
│   ├── envref.go              # 新增：EnvPlaceholderName(s) (name string, ok bool)——${VAR} 整串判定
│   │                          #       唯一实现（workspace / cli / tool 三处共用）；零 import 内部包
│   └── notify.go              # 新增：NotifyChannel{Name,Type,URL} 纯类型 + NotifyChannelReader
│                              #       只读接口（ListNotifyChannels(ctx) ([]NotifyChannel, error)）
├── storage/
│   └── notify_store.go        # 新增：NotifyChannelRepo{db *DB} + NewNotifyChannelRepo(db) +
│                              #       ListNotifyChannels（db.read；SQL 固定 ORDER BY name；空表空切片）
├── tool/
│   ├── notify.go              # 新增：NotifyTool（channels / policy / adapters / lookupEnv）+
│   │                          #       NewNotifyTool（克隆映射、lookupEnv 缺省 os.LookupEnv）+ 四方法；
│   │                          #       decodeNotifyInput / findNotifyChannel / availableChannelNames /
│   │                          #       missingChannelError / unknownChannelError / notifyDisplayURL /
│   │                          #       redactNotifyPolicyError / notifyTypeUnregisteredError /
│   │                          #       availableNotifyTypes / notifyNotFoundError（Unwrap → fs.ErrNotExist）
│   ├── notify_adapters.go     # 新增：NotifyAdapter 接口 + WebhookNotifyAdapter / FeishuNotifyAdapter
│   │                          #       （导出无参构造器 + 非导出 doer 构造器测试缝）+ 载荷类型 +
│   │                          #       feishuNotifyReceipt（Code *int）+ notifyPost + feishuReceiptVerdict
│   └── httptools.go           # 零改动：复用 httpDoer / newHTTPClient / httpTransportReason /
│                              #       ctxPreferred / discardBody（只读依赖，不改文件）
├── app/
│   ├── notify.go              # 新增：newNotifyAdapters() map[string]tool.NotifyAdapter（唯一真值）
│   ├── wire.go                # 修改：注册第 9 件 notify（门禁之前）；declaredToolNames 九名；
│   │                          #       三处注释八件 → 九件（注明第 10 件归 US-10）
│   └── ports_test.go          # 修改：HeavyAssemblyRegistersEightTools → ……NineTools；名集与可解析
│                              #       循环加入 notify（判据面变更，人工 review ① 已批准）
├── workspace/
│   └── mcp.go                 # 修改：删本地 isEnvPlaceholder，env 校验改调 core.EnvPlaceholderName
│                              #       （等价重构，错误文案不变）
└── cli/
    └── config.go              # 修改：删 wholePlaceholder 正则，凭证键整串判定改调
                               #       core.EnvPlaceholderName；placeholder（替换用）保留
```

测试文件（新增 5 + 修改 1）：`internal/core/envref_test.go`（1）、`internal/storage/notify_store_test.go`（4）、`internal/tool/notify_test.go`（21）、`internal/tool/notify_adapters_test.go`（13）、`internal/app/notify_test.go`（2）、`internal/app/ports_test.go`（修改 1）。

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

无——宪法检查全项通过（或明确不适用），无需要justify的违规项；无新增依赖、无新增包、无迁移、无配置键。
