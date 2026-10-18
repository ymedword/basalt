# Implementation Plan: US-9 Web Service（HTTP 接入面与进程模式）

**Branch**: `009-web-service` | **Date**: 2026-10-18 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/009-web-service/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 9` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us9-web-service.md](../../docs/us/us9-web-service.md)（R4，人工 review ① 2026-10-18 通过）§2 / §3 / §4 为准，冲突时以 [docs/TechnicalDesign.md](../../docs/TechnicalDesign.md) 为准（§1.1 / §1.3 / §3.1.1 / §5.2 / §6.2.1 / §6.2.2 / §6.2.3 / §6.2.5 / §6.5 / §7.1.1 / §7.1.2 / §8.2）；固定字面量以 [docs/contracts/](../../docs/contracts/) 为准（`openapi.yaml` 的 10 端点 / 信封 / 错误码 / 视图、`run-and-errors.md` §3.1 冻结文案与 §8、`schema.sql` 的 `sessions` / `messages` 列）。设计面已冻结；本 plan 只做落位与验证映射，不引入文档外交付物。

## Summary

给 Basalt 装上**对外 HTTP 接入面**：在 `/api/v1` 下提供 10 个核心 REST 端点（9 条路径，`sessions/{id}` 同时有 GET 与 DELETE），以统一信封表达成功与失败，并落地 `serve` / `gateway` 两个进程模式。此前业务系统只能通过 CLI 使用 Agent；本模块让工单、审批、巡检平台在既有实例上创建会话、发消息、取历史、归档，一次性任务走无状态调用，而执行入口仍然只有一条。

四件可判卷的价值：**① 一个信封**（成功含 `data`、失败省略 `data`、时间戳 RFC3339 UTC、8 个错误码按类型映射）；**② 一条执行链**（HTTP 运行与 CLI 共享准入、Run 账本、审计、ReAct 循环与消息落库——HTTP 层不写消息、不执行工具、不另起循环）；**③ 一套互斥语义**（归档与运行准入在同一把服务级锁内串行化：在途运行期间归档 409、归档后追加 400、在途计数归零即删表项）；**④ 安全默认**（只监听 `127.0.0.1:8080`、跨源写保护默认开启且不输出任何 CORS 允许头、对外原因脱敏、五阶段优雅停机）。

技术路线的七个要点：**① 新包 `internal/web`**（信封 / 路由 / 中间件 / 视图 / handlers；只依赖 `internal/core` 与标准库，端口由 `app` 适配）；**② 单一写信封出口**——`writeOK` / `writeResult` / `writeError`，handler 不手写 JSON；DELETE 成功是唯一无载荷成功例外；**③ 路由只写一处**——Go 1.22+ `ServeMux` 方法模式 + 方法无关 catch-all，未匹配路径与方法统一 404 信封，不注册 `GET /sessions`；**④ 准入锁在 HTTP 服务层**——`admitRun` 锁内完成状态复核与在途登记，`archiveSession` 锁内拒绝在途会话，`releaseRun` 归零即删；**⑤ 运行预算显式传导**——`app.webBudget(rt.cfg)` 按 `cli.chatBudget` 同口径映射 `basalt.yaml` 的 `budget:` 段，**不**依赖 `mergeBudget` 的硬编码缺省；**⑥ 会话标识唯一实现**——`core.DeriveSessionID` 从 CLI 等价迁移（固定向量不变），恶意 `channel` 前缀回落 `ch`，`cli.deriveSessionID` 逐字委托；**⑦ 装配与进程模式**——`heavyCommands` 追加 `serve` / `gateway`，只有 `serve` 自动初始化工作区并装配 HTTP，`gateway` 占位日志 + 阻塞至信号，`runWithRuntime` 让「Serve 返回之后才释放共享资源」成为可测行为。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）。本轮实测 `go version go1.27.2 windows/amd64`。

**Primary Dependencies**: **零新增坐标**——`go.mod` / `go.sum` **零 diff**（判据 `git diff -- go.mod go.sum` 为空）。全部既有：标准库 `net/http`（含 Go 1.25+ 的 `http.CrossOriginProtection`，本轮 `go doc net/http.CrossOriginProtection` 已核实 `NewCrossOriginProtection` / `Check` / `SetDenyHandler` 存在）/ `http.ServeMux` / `context` / `encoding/json` / `errors` / `fmt` / `io` / `sync` / `crypto/rand` / `log/slog` / `time` / `regexp` / `fs` + 既有 `stretchr/testify v1.12.1`（测试）。零 `import _` 副作用注册。

**Storage**: **零迁移、`schema.sql` 零 diff、零新增表 / 列 / 索引**。本 US 只首次读写既有列：`sessions.status`（读 + 归档写）、`sessions.archived_at`（读 + 首次写）、`messages.created_at`（读）。四条 SQL 逐字见颗粒度文档 §3.1（`GetSession` 九列 SELECT；`GetOrCreateSession` 的 `INSERT … ON CONFLICT(session_id) DO NOTHING` 后读回；`ArchiveSession` 的 `UPDATE … SET status='archived', archived_at=COALESCE(archived_at, ?)`；`MessagesTail` 的 `ORDER BY seq DESC LIMIT ?` 后在 Go 内反转）。读走 `db.read`（同 `Messages` / `ListSessions`），写走 `db.write`（同 `UpsertSession`）；`ArchiveSession` 以 `RowsAffected() == 0` 判不存在并返回 `fs.ErrNotExist`；时间列一律 `UnixMilli` 落 `INTEGER`。判据：`git diff -- internal/storage/schema.sql docs/contracts/schema.sql` 为空；`internal/storage/migrations/` 零新增文件。

**Testing**: 默认层 `go test -race ./...`——**新增 7 个测试文件共 144 条**（`web/contract_test.go` **15** / `web/handlers_test.go` **74** / `web/server_test.go` **23** / `app/web_test.go` **11** / `core/sessionid_test.go` **5**（按测试函数计）/ `storage/session_api_test.go` **11** / `cli/serve_test.go` **5**；其中收口复核修复轮新增 4 个函数各计 1 条，独立复核修复轮在既有函数内扩 8 例，人工 review ② 二次退回小修轮再扩 4 例）+ **修改 3 个测试文件 5 条条目**（`app/run_test.go` **+3** = 2 条既有测试追加断言 + 1 个新函数；`cli/config_test.go` **+1**；`memory/memory_test.go` **+1**）= **149**（计数口径逐字见颗粒度文档 §3.2 表下注）。**默认层不访问外部网络**：handler 用 `httptest.NewRecorder` 直接驱动 `handler()`；存储用临时目录 + 真实 SQLite；`R16` / `R19` 用**进程内真实回环 TCP**（`httptest.NewUnstartedServer` 换入 `s.newHTTPServer(ctx)`；`recordingListener` + `s.Run`）；不连真实模型、不起 `basalt` 子进程。**真实进程验收**（`basalt serve` 子进程、端口绑定读数、SIGTERM、Demo 四 / 五）按颗粒度文档 §6.1 在 Loop 收尾以临时工作区执行，Windows 的 SIGTERM 用例记「未验证」并说明原因，**不得**静默记为通过。**测试缝纪律**：`Options` 的 `Listener` / `Now` / `Logger` / 三个非导出超时覆盖字段只供包内测试；生产装配不设置。关键回归 **R1~R24** 原样落地，函数名英文、文档中文验收点以注释保留；每条带逆改点（见颗粒度文档 §4.3）；R21~R24 为收口复核修复轮新增（监听异常退出统一收尾、收尾等待超时、panic 与完成日志共享标识、failed 内部诊断脱敏）。覆盖率：`core` / `storage` / `memory` ≥ 70%，`internal/web` / `app` / `cli` ≥ 50%，由既有 `make cover` 按包判失败（**不改脚本与阈值**）。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物 `CGO_ENABLED=0` 交叉编译；`-race` 与发布构建**分属不同命令 / job**。HTTP 面为纯标准库实现，无平台分片；真实 SIGTERM 验收按平台分支。

**Project Type**: CLI + 服务单二进制（web-service + cli）。本 US 新增一个内部包 `internal/web`（颗粒度文档 §3.1 的落位），不新增顶层目录、不新增第三方依赖。

**Performance Goals**: 无吞吐 / 延迟指标（接入面类 feature）；判据是**行为预算**——单次运行 60s 时限（到期 504）、写超时 = 读 60s + 运行 60s + 余量 10s = 130s、单条消息 32 KiB（UTF-8 字节）、请求体 1 MiB、停机宽限 70s + 有界收尾 5s、服务端读头 5s / 读 60s / 空闲 120s。准入不排队、不限流；同会话第二个运行立即 409、实例超限立即 429。

**Constraints**: 三条包纪律——`internal/web` **只依赖 `core` + 标准库**（`go list -deps ./internal/web` 复核）；`cli` **不得** import `web`（只认自己声明的 `Server` 端口，实现由 `app` 注入）；`app` 是唯一装配层。**单一写入路径**：`web` 不 import `storage` / `tool` / `memory`，不调用 `AppendMessage` / `ReActLoop` / `ToolExecutor`（`grep` 复核）。**审计零改动**：HTTP 层不写审计；Run 行与审计仍由 `core.AgentService` / `ToolExecutor` 承担。**同步执行**：handler 同步等待 `Run` 返回；不建后台 goroutine 编排业务；`context.WithValue` 零使用。**安全边界**：默认回环监听；无 ACAO 头；500 文案固定；200 失败原因经 `publicRunReason` 投影；`/info` 结构无 `api_key` / `base_url`。**范围零膨胀**：零新增配置键（`server.max_concurrent_runs` 是既有契约键落位）、`basalt.yaml` 零 diff、`Makefile` / CI / `scripts/**` 零改动、`schema.sql` 零 diff。**静态门禁**：抑制注释（`//nolint` / `#nosec` / `//lint:ignore`）**零使用**；测试函数名英文 `TestXxx_yyy`；格式只许 `gofumpt -w <本轮改动的文件>`。

**Scale/Scope**: 生产代码 **新增 9 文件 + 修改 12 文件 + `basalt.yaml.example`**（`web` 6 新 1 改 / `app` 1 新 2 改 / `cli` 1 新 5 改 / `core` 1 新 2 改 / `storage` 1 改 / `memory` 1 改）；测试 **新增 7 文件 144 条 + 修改 3 文件 5 条**（含收口复核 R5 +4 函数、独立复核 R6 +8 例、review ② 小修 R7 +4 例）；九个专属 check ID；`go.mod` / `go.sum` 零 diff；零迁移；零新增配置键。前序改造点 17 项（颗粒度文档 §3.5），其中判据面新增仅 §3.2「三处追加」，等价重构为 §3.5 第 6 / 10 / 16 条，契约增补为第 17 条（D-01 已批准）。

**环境实测读数（2026-10-18，本轮当场命令）**: 分支 `009-web-service`；起始 commit `1dbeebc41fa12d152e079ba0ceb88ee602985623`；工作区已有 `M docs/contracts/openapi.yaml`（D-01 已批准增补，sha256 `4be307f7…`，**保留不覆盖**）。核对属实：`heavyCommands` 在 [internal/app/run.go:41](../../internal/app/run.go#L41)（`{"chat", "tool"}`）；`defer rt.Close()` 闭包在 [run.go:110-114](../../internal/app/run.go#L110-L114)；`needsRuntime` 在 [run.go:160](../../internal/app/run.go#L160)；`commandName` 在 [run.go:174](../../internal/app/run.go#L174)。`heavyRuntime` 在 [internal/app/wire.go:479](../../internal/app/wire.go#L479)（`Close` 在 [:516](../../internal/app/wire.go#L516)，顺序 MCP → DB → 锁）；`core.NewAgentService` 构造在 [wire.go:361-372](../../internal/app/wire.go#L361-L372)（`ServiceOptions` 尚无 `MaxConcurrent`）；`mcpConns` 在 [wire.go:270](../../internal/app/wire.go#L270)；装载结果 `entries` 在 [wire.go:335](../../internal/app/wire.go#L335)。core：`defaultMaxConcurrentRuns = 32` 在 [internal/core/service.go:26](../../internal/core/service.go#L26)；`ServiceOptions.MaxConcurrent` 在 [:68](../../internal/core/service.go#L68)（`NewAgentService` 已在 [:94-95](../../internal/core/service.go#L94-L95) 把 `<=0` 归一为缺省）；`admit` 在 [:231](../../internal/core/service.go#L231)；`SessionBusyError` / `InstanceFullError` 在 [internal/core/errors.go:95](../../internal/core/errors.go#L95) / [:106](../../internal/core/errors.go#L106)；`SessionRecord` 在 [internal/core/store.go:167](../../internal/core/store.go#L167)（无 `Status` / `ArchivedAt`）；`Message` 在 [internal/core/provider.go:18](../../internal/core/provider.go#L18)（无 `CreatedAt`）。storage：`SessionStore` 只有 [AppendMessage:47](../../internal/storage/session_store.go#L47) / [Messages:64](../../internal/storage/session_store.go#L64) / [UpsertSession:116](../../internal/storage/session_store.go#L116) / [ListSessions:139](../../internal/storage/session_store.go#L139)；`UpsertSession` 的 `ON CONFLICT` 只改 `last_active_at`（**不写 `status`**，注释明说「归档态不该被刷新复活」）。memory：`Load` 在 [internal/memory/memory.go:79](../../internal/memory/memory.go#L79)、`LoadEffective` 在 [:103](../../internal/memory/memory.go#L103)、`coreSectionOf` 在 [:178](../../internal/memory/memory.go#L178)；`parseSections` 在 [markdown.go:195](../../internal/memory/markdown.go#L195)、`renderSection` 在 [markdown.go:283](../../internal/memory/markdown.go#L283)（**语义相反**：标题恒出、正文可空）。tool：`Names` 在 [internal/tool/registry.go:60](../../internal/tool/registry.go#L60)、`Specs` 在 [:72](../../internal/tool/registry.go#L72)；`MCPConnection` 在 [internal/tool/mcp.go:39](../../internal/tool/mcp.go#L39)；`ToolSpec` 在 [internal/core/prompt.go:53](../../internal/core/prompt.go#L53)。cli：`Config` 在 [internal/cli/config.go:30](../../internal/cli/config.go#L30)、`HTTPConfig` 在 [:43](../../internal/cli/config.go#L43)、`LoadConfig` 在 [:191](../../internal/cli/config.go#L191)、`validateConfig` 在 [:384](../../internal/cli/config.go#L384)；`deriveSessionID` 在 [internal/cli/chat.go:222](../../internal/cli/chat.go#L222)；`root.AddCommand` 在 [internal/cli/root.go:42-49](../../internal/cli/root.go#L42-L49)（七条，无 serve / gateway）；`Deps` 在 [internal/cli/deps.go:19](../../internal/cli/deps.go#L19)（无 `Server`）；`RunService` 端口在 [internal/cli/ports.go:103](../../internal/cli/ports.go#L103)。web：[internal/web/doc.go](../../internal/web/doc.go) 仍是「骨架占位」；`basalt.yaml.example` 有 `http:`（[:12](../../basalt.yaml.example#L12)）与 `budget:`（[:38](../../basalt.yaml.example#L38)），**无 `server:`**；`cmd/basalt/run.go` 传 `context.Background()`——信号由需要它的命令自行接线。**本轮未复测的项不在此声称**（**未跑** `make check`、**未跑**覆盖率读数、**未跑**四目标编译、**未跑**真实进程验收）。

### 文档 ↔ 仓库事实差异（实测发现）

**A. 与文档一致、作为落地基点的仓库事实**（逐条已核，不构成差异）：

| # | 文档写法 | 仓库实测 | 落地口径 |
| --- | --- | --- | --- |
| 1 | §3.1：`heavyCommands` 追加 `serve` / `gateway` | [run.go:41](../../internal/app/run.go#L41) 当前 `{"chat", "tool"}`，文件注释预留「serve 落位时追加」 | 追加两名；`needsRuntime` 按 `slices.Contains` 自然覆盖 |
| 2 | §3.5 #16：抽取 `runWithRuntime` | [run.go:110-114](../../internal/app/run.go#L110-L114) 为内联 `defer func(){ rt.Close() }()` | 收进 `runWithRuntime(rt runtimeCloser, logger, exec)`；生产调用点传 `*heavyRuntime`，行为等价（先 exec 后 Close，恰好一次） |
| 3 | §3.1：`heavyRuntime` 保留 `cfg` / `entries` / `mem` / `mcpConns` | [wire.go:479-509](../../internal/app/wire.go#L479-L509) 当前只有 lock / db / service / sessions / invocations / llmCalls / registry / mcp | 四个均为既有局部值的保留（`cfg` 来自 `newRuntime` 的生效值 `&cli.Config{}`，非 nil；`entries` 是 [:335](../../internal/app/wire.go#L335) 的全部装载结果；`mcpConns` 是 [:270](../../internal/app/wire.go#L270)） |
| 4 | §3.1：`ServiceOptions.MaxConcurrent` 接线 | [wire.go:361-372](../../internal/app/wire.go#L361-L372) 未设置；[service.go:68](../../internal/core/service.go#L68) 已有字段 | 加 `MaxConcurrent: cfg.Server.MaxConcurrentRuns`；`<=0` 的归一逻辑已在构造函数，不重复实现 |
| 5 | §3.1：`cli.ServerConfig` 与负值拒载 | [config.go:30](../../internal/cli/config.go#L30) 无 `Server`；[validateConfig:384](../../internal/cli/config.go#L384) | 新增 `ServerConfig{MaxConcurrentRuns int}` + `Config.Server` + 负值错误（点名 `server.max_concurrent_runs`） |
| 6 | §3.1：`root` 注册 serve / gateway | [root.go:42-49](../../internal/cli/root.go#L42-L49) 七条 | 追加两条；顺序在 `newToolCommand` 之后 |
| 7 | §3.1：`Deps.Server` 与 `cli.Server` 端口 | [deps.go:19](../../internal/cli/deps.go#L19) 无 `Server`；[ports.go:103](../../internal/cli/ports.go#L103) 有 `RunService` | 新增 `Server interface{ Run(ctx) error }` 与字段；`cli` 不 import `web` |
| 8 | §3.5 #6：`deriveSessionID` 委托 | [chat.go:222](../../internal/cli/chat.go#L222) 本地实现（域串 + `0x1F` + 长度前缀 + SHA-256 前 16 hex + `channel-`），**无前缀回落** | 迁入 `core.DeriveSessionID` 并加安全前缀；`cli` 函数名与签名不变，逐字委托；CLI 向量 `cli-a2025c15b5cbc303` 不变 |
| 9 | §3.1：`SessionStore` 四个新方法 | [session_store.go:29](../../internal/storage/session_store.go#L29) `SessionStore{db *DB}`；读走 `db.read`、写走 `db.write` | 四个方法按同一约定落地；既有 `Messages` / `UpsertSession` 零修改 |
| 10 | §3.1：`SessionRecord` / `Message` 附加字段 | [core/store.go:167](../../internal/core/store.go#L167) / [core/provider.go:18](../../internal/core/provider.go#L18) 无字段 | 追加 `Status` / `ArchivedAt *time.Time` 与 `CreatedAt time.Time json:"-"`；既有构造点零改动（零值保持） |
| 11 | §3.1：`memory.Sections` + `renderSectionOrEmpty` | [memory.go:178](../../internal/memory/memory.go#L178) `coreSectionOf` 经 `renderSection`；[markdown.go:283](../../internal/memory/markdown.go#L283) 标题恒出 | 抽取 `renderSectionOrEmpty`（空 → `""`）并让 `coreSectionOf` 改调它（输出逐字不变）；`Sections` 复用 `parseSections`；`renderSection` 保持原样 |
| 12 | §3.1：`/tools` 来源分类取 `declaredToolNames` 九件 | [wire.go](../../internal/app/wire.go) 的 `declaredToolNames` 九名（US-8 已交付）；`Registry.Names` 已排序 | `newToolCatalogPort` 用该名单构建 builtin 集合；MCP 工具来自 `mcpConns`；其余 → `code` |
| 13 | §3.1：`web/doc.go` 改写；`basalt.yaml.example` 加 `server:` | [doc.go](../../internal/web/doc.go) 骨架；[basalt.yaml.example:12](../../basalt.yaml.example#L12) 有 `http:`、无 `server:` | 改包注释；示例追加 `server:` 段（与契约键同名） |
| 14 | §3.3：`http.addr` / `http.max_request_bytes` 已可装载 | [config.go:43](../../internal/cli/config.go#L43) `HTTPConfig` | 不新增键；`app.newWebServer` 读取既有值 |

**B. 实测差异**：本轮逐条核对 §3.1 的签名、行号与既有实现，**未发现文档与仓库的事实差异**。实现期若出现新的签名漂移，按颗粒度文档「待决事项 / 停止边界」口径停下登记，不静默自决。

**C. 文档内部口径的落地解释（两条）**：

1. `/info.agents` 是**已注册** Agent 数（`AgentEntry.Registered == true` 的条目数），不是 `len(Profiles())`——后者含未注册项；两者在「装载 1 注册 + 1 未注册」夹具下分别是 1 与 2（约束 25 的验收点）。
2. `web.Server.Run` 的 `Listener` 字段是**测试接缝**：生产装配为 nil，`Run` 内 `net.Listen("tcp", opts.Addr)`；`R16` 用 `recordingListener` 观测「监听先关、在途运行后收尾」的事件序，不改变生产语义。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | **适用（零改动）**：`go.mod` 零新增坐标；HTTP 只经 `RunService.Run` 进入既有 `AgentService`，循环体零改动、不按入口分支；`web` 不 import `core` 的循环实现细节（只用 `RunRequest` / `RunResult` 与错误类型） | 通过 |
| II | LLM 客户端只做协议调用 | **适用（强约束）**：HTTP 层不写消息、不写审计、不执行工具；`grep -n 'AppendMessage\|NewReActLoop\|ToolExecutor' internal/web/` 零命中；消息追加仍由 `AgentService` / `ReActLoop` 单路径承担 | 通过 |
| III | Provider 必须显式映射 | **不适用（不触碰 provider 路由）**；但 `/info` 的 provider 摘要只读配置投影（`cfg.Providers` 顺序），不遍历推断、不含凭证 | 通过 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 + 渐进披露 | **适用（弱形态）**：`POST /sessions` 与 `invoke` 用 `registeredAgent` 查装载结果（注册判据只有 `workspace.AgentLoader` 一份）；不新增绑定机制、不预载技能 | 通过 |
| V | 审计 day-one 且 fail-closed | **适用（零改动）**：HTTP 层零审计写入；Run 行与工具审计仍由 `AgentService` / `ToolExecutor` 两阶段写；`web` 无 SQL | 通过 |
| VI | 授权与执行分离（两接口） | **不适用（不触碰 `Policy` / `CommandRunner`）**：本 US 不新增策略判定面；跨源保护是传输层拒绝，不是 `Policy` | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用（强形态）**：handler 同步等待 `Run`；`context.WithTimeout` 只设运行时限；`BaseContext` 用 `context.WithoutCancel` + `WithCancel` 只承载取消语义；`context.WithValue` 零使用；无包级可变状态 | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：`CGO_ENABLED=0` 发布构建；`-race` 与发布构建分属不同命令；`go.mod` / `go.sum` 零 diff | 通过 |
| IX | 不用 ORM，手写 SQL | **适用（四条手写 SQL）**：全部在 `internal/storage/session_store.go`；零迁移、`schema.sql` 零 diff；`MessagesTail` 的排序 / LIMIT 固定在 SQL，不接受请求派生 | 通过 |
| X | 五大能力优先，范围卡紧 | **适用**：交付面对照颗粒度文档 §5 逐项边界（认证 / 多租户 / 流式 / 会话列表 / 管理台 / 调度器 / Agent 写面 / 排序分页 / 限流 / TLS / 通知历史 **全不做**）；对外概念零发明（端点、错误码、视图字段逐字以契约为准） | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 唯一装配层——本 US 的落法：`web` 只 import `core` + 标准库（判据 `go list -deps ./internal/web` 无其他 `basalt/internal/...`）；`storage` 新增四个方法 import `core` + `database/sql`；`app/web.go` import `core` / `cli` / `web` / `tool` / `memory` / `storage`（装配层允许）；`cli/serve.go` 只认 `Deps.Server` 端口，**不** import `web`；`cli/chat.go` 委托 `core.DeriveSessionID`（import `core`，方向合法）。**同类语义唯一实现**：会话标识 = `core.DeriveSessionID`；信封出口 = `web.writeOK/writeResult/writeError`；准入与归档 = `web.Server` 的一把锁。无环由 `go list` 复核（新增 `internal/web` 后仍无环）。

**开发工作流**：门禁一条命令 `make check`（七目标；`Makefile` / CI / 覆盖率脚本**零改动**）；三条实测纪律适用（格式判卷**先退出码后输出** / `-race` 与 `CGO_ENABLED=0` **分离** / 集成层维持空跑并登记依据）；覆盖率不可豁免（`core` / `storage` / `memory` ≥ 70%，`web` / `app` / `cli` ≥ 50%）；提交纪律（AI **不自动** commit / push / tag）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `web` 只 import `core` + 标准库（收尾 `GOPROXY=https://goproxy.cn go list -deps ./internal/web`）；`cli` 不 import `web`；`storage` 只 import `core` + 标准库；`app` 装配全部 |
| 审计写入路径唯一 | HTTP 层零审计 / 零 SQL；两阶段写仍只在 `core.ToolExecutor` → `storage.ToolInvocationRepo`；`grep -rn 'INSERT INTO tool_invocations' --include='*.go' internal` 只在 `internal/storage` |
| 无框架坐标 | `go.mod` 零 diff；`grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中 |
| 无 `context.WithValue` 业务态 | 收尾 `grep -rn "context.WithValue" --include='*.go' cmd internal` 每处命中人工确认；`web` 零使用 |
| 单二进制构建通过 | `build` 目标 `CGO_ENABLED=0 go build`；`-race` 只在 `test` 目标 |
| 静默与双轨可观测性 | HTTP 失败路径：500 / panic 记 ERROR（原始原因只进日志）、客户端断开记 INFO；审计落库仍由既有执行链承担（两者都要） |
| 凭证红线 | `web.ProviderInfo` 无 `api_key` / `base_url` 字段；`/info` 只出 `configured`；500 文案固定；`grep -R 'json:"\(api_key\|base_url\)"' internal/web` 零命中 |

**Phase 1 后复评（post-design）**：设计产物（[research.md](./research.md) / [data-model.md](./data-model.md) / [quickstart.md](./quickstart.md)）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/009-web-service/
├── spec.md              # /speckit-specify 输出（含 Clarifications 会话 2026-10-18：5 条收敛项）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（D1~D13 决策与写前核实清单）
├── data-model.md        # Phase 1 输出（实体、字段、校验与状态流转）
├── quickstart.md        # Phase 1 输出（可复制执行的验证步骤）
├── run-state.md         # 过程状态（module-dev 步骤 0 建；入库、收口不删）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

**`contracts/` 本 feature 不新建**（与 US-1~US-8 同口径）：本 US 的固定字面量**已有唯一事实源**——10 个端点、信封、错误码与视图字段在 [docs/contracts/openapi.yaml](../../docs/contracts/openapi.yaml)；冻结文案与状态映射在 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md) §3.1 / §8；`sessions` / `messages` 列在 [docs/contracts/schema.sql](../../docs/contracts/schema.sql)；导出签名逐字来源在颗粒度文档 §3.1、冻结口径 40 条在 §6.3。再抄一份到 `specs/` 就是**第二份字面量**——抄一份必漂移一处。plan 的契约节只**引用**，不复制。

### Source Code (repository root)

```text
internal/
├── web/                         # 新包：只依赖 core + 标准库
│   ├── envelope.go              # 新增：Envelope / 错误码常量 / 固定文案 / limitReachedMessage /
│   │                            #       publicRunReason / writeOK / writeResult / writeError
│   ├── server.go                # 新增：Options / ProviderInfo / 四个端口接口 / Server / NewServer / Run /
│   │                            #       handler / routes / newHTTPServer / cancelBase / waitInflight /
│   │                            #       writeTimeout / admitRun / releaseRun / archiveSession / 超时常量
│   ├── middleware.go            # 新增：recover → requestLog → crossOrigin；statusRecorder；requestID
│   ├── views.go                 # 新增：Info/Provider/Profile/Tool/Memory/Session/Message/Run 视图与投影
│   ├── handlers_meta.go         # 新增：health / info / profiles / tools / memory + registeredAgent
│   ├── handlers_sessions.go     # 新增：建 / 读 / 归档 / 发消息 / invoke / 404 兜底 + 解码与错误映射
│   └── doc.go                   # 修改：移除「骨架占位」，改写包职责
├── app/
│   ├── web.go                   # 新增：newWebServer / webBudget / ensureWorkspaceForServe / webProviders /
│   │                            #       memoryBackendName / agentProfilesPort / toolCatalogPort
│   ├── run.go                   # 修改：heavyCommands 追加两名；needsServer / needsServeInit；
│   │                            #       serve 自动初始化与 Deps.Server 注入；抽取 runWithRuntime
│   └── wire.go                  # 修改：heavyRuntime 保留 cfg / entries / mem / mcpConns；
│                                #       ServiceOptions.MaxConcurrent 接线
├── cli/
│   ├── serve.go                 # 新增：newServeCommand / newGatewayCommand 与信号接线
│   ├── root.go                  # 修改：注册 serve / gateway
│   ├── ports.go                 # 修改：新增 Server 端口
│   ├── deps.go                  # 修改：Deps 新增 Server 字段
│   ├── chat.go                  # 修改：deriveSessionID 委托 core.DeriveSessionID（等价重构）
│   └── config.go                # 修改：ServerConfig + 负值拒载
├── core/
│   ├── sessionid.go             # 新增：DeriveSessionID（安全前缀回落）
│   ├── store.go                 # 修改：SessionRecord 追加 Status / ArchivedAt
│   └── provider.go              # 修改：Message 追加 CreatedAt（json:"-"）
├── storage/
│   └── session_store.go         # 修改：GetSession / GetOrCreateSession / ArchiveSession / MessagesTail
└── memory/
    └── memory.go                # 修改：Sections 读面 + renderSectionOrEmpty 抽取
```

测试文件（新增 7 + 修改 3）：`internal/web/contract_test.go`（15）、`internal/web/handlers_test.go`（61）、`internal/web/server_test.go`（20）、`internal/app/web_test.go`（11）、`internal/core/sessionid_test.go`（5）、`internal/storage/session_api_test.go`（11）、`internal/cli/serve_test.go`（5）；修改 `internal/app/run_test.go`（+3）、`internal/cli/config_test.go`（+1）、`internal/memory/memory_test.go`（+1）。

配置与文档：`basalt.yaml.example`（新增 `server:` 段）；`docs/TechnicalDesign.md` §6.2.1（§3.5 #15 文档同步）；`docs/contracts/openapi.yaml`（§3.5 #17，D-01 已批准）；`harness/stages/us9-web.json`（步骤 6 派生）。

## Complexity Tracking

> **Fill ONLY if Constitution Check has violations that must be justified**

无——宪法检查全项通过（或明确不适用），无需要 justify 的违规项；无新增依赖、无新增迁移、无新增配置键；新增一个内部包 `internal/web` 属颗粒度文档 §3.1 已审交付物，不构成架构偏离。
