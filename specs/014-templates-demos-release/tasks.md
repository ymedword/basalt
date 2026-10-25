# Tasks: US-14 MCP 目录与模板 + 五个每日业务 Demo + 发布收尾

**Input**: Design documents from `/specs/014-templates-demos-release/`

**Prerequisites**: [plan.md](./plan.md), [spec.md](./spec.md), [research.md](./research.md), [data-model.md](./data-model.md), [quickstart.md](./quickstart.md)；模块卡 [docs/us/us14-templates-demos-release.md](../../docs/us/us14-templates-demos-release.md)（R3，review ① 2026-10-25 通过，sha256 `ff6594655edc6f669488d744587d14b6d624c994c9bb2b47b2e58e068d284cc6`）

**Tests**: 本 US 明确要求测试先行或伴随实现（模块卡 §4 与本仓 `module-dev` 测试纪律）；每个实现任务列出对应测试任务与 check id。

**Organization**: 按 spec 三个用户故事分片——US1 = 14A（P1，MCP 目录 / 闸门 / 诊断 / 测试记录 / 工具来源 / 控制台），US2 = 14B（P2，五 Demo / 八模板 / docs-readonly / 公开增量），US3 = 14C（P3，发布收尾）。

**Format**: `[ID] [P?] [Story] 描述与文件路径（check id）`

## Phase 1: Setup（契约与纯类型）

**Purpose**: 冻结契约与 `core` 纯类型、只读目录包；后续所有分片依赖本阶段。

- [x] T001 [P] 按 D-01–D-10 更新 `docs/contracts/openapi.yaml`：新增 `GET /api/v1/catalog/mcp`（D-01）与 `GET /api/v1/mcp-servers/{name}/diagnostics`（D-02）路径与 schema；`McpServerTestResult` 追加 `operation_id` / `tested_version_id` / `stage` / `failure_class`（D-03）；测试端点补 503 / 504 与 `AdminError.Data.operation_id`（D-04 / D-08）；`McpServerView.tool_disposition`（D-06）；`OperationDetail.detail` 固定字段（D-07）；保存 / 发布 503 响应（D-08）。逐字对齐模块卡 §3.2；不改既有字段语义（check `mcp-contracts`）
- [x] T002 [P] 新增 `internal/core/mcp_catalog.go`：`MCPCatalog{Version int; Items []MCPCatalogEntry}`、`MCPCatalogEntry`（ID / DisplayName / Summary / Category / Transport / Command / URL / SourceURL / VerificationNote / VerifiedAt / Args / Requires / VerifiedPlatforms / Warnings / Env / Verified）、`MCPCatalogEnv{Key, Placeholder, Description string; Required bool}`；JSON tag 与 D-01 一致（check `mcp-catalog`）
- [x] T003 [P] 新增 `internal/core/mcp_diagnostics.go`：`MCPServerDiagnosis{Name, Transport string; PublishedVersionID, AppliedVersionID int64; PendingRestart bool; Command *MCPCommandDiagnosis; Env []MCPEnvDiagnosis; Warnings []string; CheckedAt time.Time}`、`MCPCommandDiagnosis{State, Path, Reason string}`、`MCPEnvDiagnosis{Key string; Set bool}`；JSON tag 与 D-02 一致（check `mcp-contracts`）
- [x] T004 [P] 新增 `internal/core/mcp_disposition.go`：`MCPToolDisposition{Offered, Accepted int; Skipped []MCPToolSkip}`、`MCPToolSkip{Name, WinnerSource, Reason string}`；JSON tag 与 D-06 一致（check `mcp-disposition`）
- [x] T005 [P] 修改 `internal/core/resource_admin.go`：`ResourceTestResult` 追加 `OperationID string`、`TestedVersionID int64`、`Stage string`、`FailureClass string`；既有字段与零值语义不变（check `mcp-test-record`）
- [x] T006 [P] 新增 `internal/catalog/catalog_test.go`：`TestCatalog_Load` / `TestCatalog_UniqueIDs` / `TestCatalog_EnvPlaceholders` / `TestCatalog_RecipesParse`；最后一项把 YAML 条目转成 `mcp_servers.yaml` 片段，经 `workspace.ParseMCPServerBlock` 验证可装载；覆盖每个校验拒绝分支（check `mcp-catalog`）
- [x] T007 新增 `internal/catalog/catalog.go`：`func Load(raw []byte) (core.MCPCatalog, error)`，实现模块卡 §3.1.1 全部校验（version / id 唯一 kebab-case / transport 闭集 / 传输互斥 / placeholder 与 key 一致 / env 键唯一 / source_url https / verified 四字段 / warnings 闭集）；纯解析，不读工作区（check `mcp-catalog`）
- [x] T008 [P] 新增 `internal/catalog/data/mcp_catalog.yaml`：六个条目（`github` / `brave-search` / `fetch` / `playwright` / `filesystem-readonly` / `dbhub`），字段与模块卡 §3.4 逐字一致；`verified: false` 且 `verification_note` 非空、`verified_at` 空、`verified_platforms: []`；`args` 的 `<...>` 只是编辑标记（check `mcp-catalog`）
- [x] T009 新增 `internal/catalog/data.go`：`//go:embed data/mcp_catalog.yaml` + `func Builtin() (core.MCPCatalog, error)`；启动装配时解析一次，请求期不重读（check `mcp-catalog`）

**Checkpoint**: `go test -count=1 ./internal/catalog` 全绿；目录配方可被既有解析器装载。

---

## Phase 2: Foundational（tool 连接与构造）

**Purpose**: 诊断与测试记录共用的命令构造 / 结构化失败；US1 依赖，US2 / US3 复用。

- [x] T010 [P] 扩展 `internal/tool/mcp_test.go`：`TestMCPPool_ConnectDetailed`（表驱动子测试覆盖每个 stage × failure_class：策略拒绝 / 找不到命令 / 权限 / 握手 / 401 / 不可达 / tools_list / 超时 / 未知→`dependency_unconfirmed`）、`TestMCPPool_Connect_LegacySemantics`（单 server 失败被吸收、WARN、继续）、`TestNewMCPCommand_MatchesLaunch`（构造参数与 `dialCommand` 实际启动一致、空命令拒绝）；在 `internal/tool/registry_test.go` 新增 `TestRegistry_DuplicateSemanticsUnchanged`（重名拒绝、保留原注册、WARN 不变；既有 `TestRegistry_RefusesDuplicateNameAndKeepsOriginal` 不改）；先写失败测试（checks `mcp-connect` / `mcp-disposition`）
- [x] T011 修改 `internal/tool/mcp.go`：新增 `MCPConnectFailure{Server, Stage, Class string; Err error}` 与 `func (p *MCPPool) ConnectDetailed(ctx, specs) ([]MCPConnection, []MCPConnectFailure, error)`；`Connect` 改为调用 `ConnectDetailed` 并保持旧行为（吸收单 server 失败）；`mcpConnector` 接口不变；分类只来自结构化失败，不解析 stderr（check `mcp-test-record`）
- [x] T012 修改 `internal/tool/mcp.go`：新增 `func NewMCPCommand(spec core.MCPServerConfig, root string, envPassthrough []string) (*exec.Cmd, error)`（空命令拒绝、`exec.Command`、`Dir`、`mergeChildEnv`、`configureMCPChildProcess`，不 `Start`）；`dialCommand` 改为调用它，不复制构造逻辑；跑 T010 通过（check `mcp-diagnostics`）

**Checkpoint**: `go test -count=1 ./internal/tool` 全绿；`Connect` 旧语义有回归用例。

---

## Phase 3: User Story 1 - 管理台配置 MCP server（Priority: P1）——14A

**Goal**: 管理员能用只读目录配方预填配置；缺 env 时 fail-closed；诊断不启动进程；测试记录带被测版本 / 阶段 / 分类；工具来源可解释；控制台四轴分开展示。

**Independent Test**: `go test -count=1 ./internal/catalog ./internal/app ./internal/web -run 'TestMCP'` + `npm --prefix assets/console test` + `npm --prefix assets/console run build`；人工走查目录页 / 诊断 / 测试详情。

### Tests for User Story 1

- [x] T013 [P] [US1] 新增 `internal/app/mcp_env_test.go`：`TestMCPEnvGate_CreateMissingEnvNoWrite` / `_UpdateMissingEnvNoWrite` / `_PublishMissingEnvNoWrite` / `_RollbackMissingEnvNoWrite` / `_IdempotentPublishSkips` / `_ErrorOmitsValues`；断言 503 + `operation_id`、文件 / 版本 / 指针零变化、detail 无秘密、幂等发布跳过校验且不宣称已满足（check `mcp-env-gate`）
- [x] T014 [P] [US1] 新增 `internal/app/mcp_diagnostics_test.go`：`TestMCPDiagnostics_NotConfigured` / `_LocatedLookPath` / `_LocatedAbsolute` / `_NotFound` / `_Permission` / `_CmdShimUnconfirmed` / `_RelativePathUnconfirmed` / `_EnvSetBoolean` / `_PendingRestartIndependent` / `_NoProcessNoOperation`；每个用例断言零进程、零操作行、applied / published 不变（check `mcp-diagnostics`）
- [x] T015 [P] [US1] 新增 `internal/app/mcp_disposition_test.go`：`TestMCPToolDisposition_BuiltinWins` / `_TwoServers` / `_InvalidTool` / `_NilWhenUnknown`；断言 `accepted + len(skipped) == offered`、赢家来源具体（check `mcp-disposition`）
- [x] T016 [P] [US1] 扩展 `internal/app/resource_admin_test.go`：`TestMCPTest_RecordsVersionAndTools` / `TestMCPTest_FailureClassification` / `TestMCPTest_EmptyToolList` / `TestMCPTest_LegacyOperationNotPass` / `TestMCPTest_UnconfirmedEntryStillAttempts` / `TestMCPTest_NoOperationOnPrewriteFailure`（模块卡 §4.5.6 / §4.5.12 / §4.5.13 的用例名逐字保留）；覆盖 §2.4.3 状态矩阵每一行。另在 `internal/app/notify_mode_test.go` 新增 `TestNotifyMode_DeliverToRemovesNotify`（模块卡 §4.5.9：`deliver_to` 的 Run 不得暴露 `notify`、零模型通知、一条系统代投记录）（checks `mcp-test-record` / `notify-exclusion`）
- [x] T017 [P] [US1] 扩展 `internal/web/*_test.go`：`TestMCPCatalog_ReadOnly`（§4.5.4：六条、零文件写、零操作行、零 CommandRunner）与 `TestMCPDiagnostics_DoesNotStartProcess`（§4.5.5：哨兵未创建、三态非空、env 只回 key/set）；`TestMCPContracts_CatalogResponse`（排序 / 空串 / 四字段 / warnings 闭集）、`TestMCPContracts_DiagnosticsResponse`（三态 / reason 组合 / env 排序与布尔 / http `command=null` / `pending_restart`）、`TestMCPContracts_DiagnosticsNotFound`（未知 server 404 `NOT_FOUND`，不返回诊断对象）、`TestMCPContracts_TestResultFields`（D-03 / D-07）、`TestMCPContracts_TestEndpoint503And504`（带 `operation_id` 且不返回结果对象）、`TestMCPContracts_ToolDisposition`（D-06）（check `mcp-contracts`）

### Implementation for User Story 1

- [x] T018 [US1] 新增 `internal/app/mcp_env.go`：`validateMCPEntryEnv(entryRaw []byte, lookup func(string) (string, bool)) error`（复用既有 `entryBlockRaw` / `resolveMCPEnv`，不新增 YAML 路径）+ `mcpEnvGateDetail{ErrorCode, Stage, FailureClass, ErrorMessage string}`；错误只点名 server / 键 / 变量名（check `mcp-env-gate`）
- [x] T019 [US1] 修改 `internal/app/resource_admin.go`：创建 / 修改 / 发布 / 回滚四个入口在第一次写文件前调用 `validateMCPEntryEnv`；缺失或空值 → 预写 `pending` 操作行后返回 503 `NOT_CONFIGURED` + `operation_id`；幂等发布（无变更）在闸门之前短路、不新建版本；启动期手工 YAML 对账保持 fail-closed（check `mcp-env-gate`）
- [x] T020 [US1] 新增 `internal/app/mcp_diagnostics.go`：`diagnoseMCPCommand(spec, root, envPassthrough) core.MCPCommandDiagnosis` 调 `tool.NewMCPCommand` 且不 `Start`，映射三态 × 七原因；`func (s *ResourceAdmin) DiagnoseMCPServer(ctx, actor, name) (core.MCPServerDiagnosis, error)` 读 published / parse / 诊断，零操作行、零进程；`ResourceAdminDeps` 追加 `DiagnoseMCP` 并在 `wire.go` 注入（check `mcp-diagnostics`）
- [x] T021 [US1] 修改 `internal/app/mcp_supervisor.go`：`Test` 优先使用 `ConnectDetailed`（连接器实现该能力时），否则回退 `Connect` 并标 `dependency_unconfirmed`；填 `Tools` / `Stage` / `FailureClass`；超时 / 策略 / 连接失败映射保持既有；`tested_version_id` 取实际被测版本（check `mcp-test-record`）
- [x] T022 [US1] 修改 `internal/app/resource_admin.go` 的 `TestMCPServer`：`BeginOperation` 前设置 `RequestedVersionID`；结果填 `OperationID` / `TestedVersionID` / `Stage` / `FailureClass`；操作 `detail_json` 写 D-07 固定字段且脱敏；503 / 504 不返回结果对象；预写失败不执行、不留行、不启动子进程（check `mcp-test-record`）
- [x] T023 [US1] 新增 `internal/app/mcp_disposition.go`：`mcpDispositionTable{sources map[string]string; byServer map[string]core.MCPToolDisposition}` + `recordBuiltin` / `recordMCP(server, tool string, registerErr error)` / `view(server string) *core.MCPToolDisposition`；不修改 `internal/tool/registry.go`（check `mcp-disposition`）
- [x] T024 [US1] 修改 `internal/app/mcp.go` 与 `internal/app/resources.go`：`mcpRuntime` 追加 `dispositions` 与 `ToolDisposition(name)`；`mcpServerView` 投影 `ToolDisposition`，`Tools` 保持 offered 语义（check `mcp-disposition`）
- [x] T025 [US1] 修改 `internal/app/wire.go`：内置工具注册处 `recordBuiltin`、MCP `OnConnected` 注册处 `recordMCP`；`mcpRT` 注入 dispositions；`ResourceAdminDeps.DiagnoseMCP` 注入（含 root / envPassthrough）；`web.Options.MCPCatalog` 注入 `catalog.Builtin()` 读者；注册顺序与重名 WARN 不变（check `mcp-disposition` / `mcp-catalog`）
- [x] T026 [US1] 修改 `internal/web/ports_admin.go` 与 `internal/web/server.go`：`ResourceAdminService` 追加 `DiagnoseMCPServer(...)`；新增 `MCPCatalogReader interface { ListMCPCatalog(ctx) (core.MCPCatalog, error) }`；`Options` 追加 `MCPCatalog`，nil 时目录端点 500 装配缺失（check `mcp-contracts`）
- [x] T027 [US1] 修改 `internal/web/admin_routes.go`：注册 `GET /api/v1/catalog/mcp` 与 `GET /api/v1/mcp-servers/{name}/diagnostics`（既有管理路由注册处，**不是** `routes_admin.go`）；管理认证；闭集路由（check `mcp-contracts`）
- [x] T028 [US1] 修改 `internal/web/handlers_admin_resources.go`：`handleAdminMCPCatalog` / `handleAdminMCPDiagnostics`（只读、错误映射、不写操作行）；`handleAdminTestMCPServer` 透传新字段与 503 / 504 的 `AdminError.Data.operation_id`（check `mcp-contracts`）
- [x] T029 [US1] 修改 `internal/web/views.go` 与 `internal/web/views_admin.go`：`McpServerView` 追加 `ToolDisposition *MCPToolDispositionView`；新增 `MCPCatalogView` / `McpServerDiagnosticsView`；`McpServerTestResultView` 追加四字段；空数组输出 `[]`、http `command` 为 `null`（check `mcp-contracts`）
- [x] T030 [P] [US1] 修改 `assets/console/src/api.ts` 新增目录 / 诊断 / 工具来源类型与测试结果字段；新增 `assets/console/src/api-catalog.ts`（`getMCPCatalog()` / `getMCPDiagnostics(name)`，只读 GET）（check `console-test`）
- [x] T031 [US1] 修改 `assets/console/src/router.ts` 加 `catalog/mcp` 路由；新增 `assets/console/src/views/catalog.ts`（表格 / 筛选 / 详情 / "添加到配置"，预填不提交；`verified=false` 显示"配方未实测"）；修改 `internal/web` 对应静态资源构建入口如需（check `console-test`）
- [x] T032 [US1] 修改 `assets/console/src/views/resources.ts`（MCP 目录入口、连接 / 测试列不混轴）、`assets/console/src/views/resource-detail.ts`（诊断区块 / 测试详情 / `operation_id` 链接 / 历史记录"字段不全"提示）、`assets/console/src/components/resource-form.ts`（可选 `prefill`，不新增写路径）；禁止"已验证"徽章（check `console-test`）
- [x] T033 [US1] 新增 `assets/console/tests/catalog.test.ts`、扩展 `assets/console/tests/resource-detail.test.ts`：目录渲染 / 预填 / 零写调用 / `verified=false` 警告 / 诊断与测试分轴 / 待重启独立 / 旧记录提示（check `console-test`）
- [x] T034 [US1] 执行 `npm --prefix assets/console run build` 重建 `assets/console/dist`，同步 `assets/console/integrity.json`；`git status --porcelain -- assets/console/dist` 为空（check `console-build`）
- [x] T035 [US1] 更新 14A 事实同步：`docs/TechnicalDesign.md` §8.1 / §8.1.1 / §8.1.2 的目录 / 诊断 / 测试记录 / 工具来源段与 §8.2 US-14 行；`docs/Requirements.md` §7.2 US-14 行；`docs/Roadmap.md` §6 M0 US-14 行；不改章节编号（check `acceptance`）

**Checkpoint**: 14A 全部 check 绿（`mcp-*`、`console-*`）；`make check` 七目标绿；D-01–D-10 实现与契约逐字一致；零迁移、零新 Go 依赖；不进入 14B 直至通过。

---

## Phase 4: User Story 2 - 五个每日 Demo 与八个模板（Priority: P2）——14B

**Goal**: 仓库自带可复制的五 Demo（六段证据 + 业务断言）与八模板（档位固定），含共享 `docs-readonly` MCP 与逐目录计数测试；公开材料只做增量。

**Independent Test**: `go -C examples/shared/mcp/docs-readonly test -count=1 ./...`；`python scripts/run-example-tests.py`；`go test -json -tags=e2e -count=1 ./internal/e2e -run 'TestUS14Demos_Fixture|TestLegacyD1_HttpEndpoints|TestTemplates_'`。**注意**：D5 Brave / Fetch 实跑与 D1 / D2 真实 GitHub / 通知接收方属外部授权项（T060）——未完成时 14B 不完成（模块卡 §5.4）。

### Shared assets & examples

- [x] T036 [P] [US2] 新增 `examples/shared/mcp/docs-readonly/go.mod`（独立 module，`go 1.27`，零依赖）与 `main.go`（JSON-RPC 2.0 over stdio；`initialize` / `tools/list` / `tools/call`；工具 `list_documents` / `read_document` / `document_metadata`；纯标准库；根目录约束；拒绝 `..` / 绝对路径 / 软链接 / 写操作）（check `docs-readonly-tests`）
- [x] T037 [P] [US2] 新增 `examples/shared/mcp/docs-readonly/main_test.go`：`TestDocsReadOnly_ToolNames` / `TestDocsReadOnly_RejectsEscape` / `TestDocsReadOnly_RejectsAbsolutePath` / `TestDocsReadOnly_RejectsSymlink` / `TestDocsReadOnly_ReadOnly` / `TestDocsReadOnly_ProtocolErrors`；由 `go -C … test ./...` 单独运行（check `docs-readonly-tests`）
- [x] T038 [P] [US2] 新增 `examples/shared/fixtures/docs/*.md`：D4 / onboarding-buddy 共用只读资料，内容脱敏、固定哈希（check `demo-fixtures`）
- [x] T039 [P] [US2] 新增 `examples/shared/scripts/make_sample_db.py`：用标准库生成 D3 / ticket-insights 的 SQLite 样例库（含缺失 / 重复 / 非法值三类异常）；不提交 `.db` 二进制（check `script-tests`）
- [x] T040 [P] [US2] 新增 `examples/README.md`：五 Demo / 八模板 / 共享素材总览、依赖与证据档位、公开可读、不链接私有路径（check `template-quality`）

### Five daily demos（每个：`AGENT.md` / `README.md` / `SETUP.md` / `scripts` + 测试 / `fixtures`；README 与 SETUP 按模块卡 §3.5 固定包含项）

- [x] T041 [P] [US2] D1 `examples/daily/github-radar/`：`AGENT.md`（`mcp_servers: [github]`、6 段 cron + `Asia/Shanghai`、`notify` 一次、工具声明）、`scripts/github_radar.py` + `scripts/test_github_radar.py`（`test_first_run_has_no_baseline` / `test_second_run_dedups_and_computes_delta` / `test_invalid_input_fails_nonzero`，模块卡 §4.5.10 逐字保留；空结果"本次无新增"；README 地址规则用 `download_url`）、`fixtures/search.json`；`notify_mode` 基线 `none`，SETUP 覆盖（check `demo-fixtures`）
- [x] T042 [P] [US2] D2 `examples/daily/repo-digest/`：`AGENT.md`（`skills: [repo-digest]`、`schedules[].deliver_to`）、`skills/repo-digest/SKILL.md`（L1 卡片 / L2 正文 / L3 参考）、`scripts/repo_digest.py` + 测试（PR / Issue / Release 计数、缺字段"待确认"、游标只前进到已处理项）、`fixtures/*.json`（check `demo-fixtures`）
- [x] T043 [P] [US2] D3 `examples/daily/biz-daily/`：`AGENT.md`（`shell.interpreters: [python]`、`notify_mode: none`、DBHub 只读 SQLite）、`scripts/biz_daily.py` + 测试（缺失 / 重复 / 非法值逐类计数、输出截断标记）、`fixtures/schema.sql` / `fixtures/rows.json`；零通知（check `demo-fixtures`）
- [x] T044 [P] [US2] D4 `examples/daily/docs-diff/`：`AGENT.md`（`mcp_servers: [docs-readonly]`、三只读工具 + 记忆 / 文件 / `notify`）、`scripts/docs_diff.py` + 测试（新增 / 修改 / 删除三集合、内容哈希、首跑无基线、无变化仍报告）、`fixtures/docs/`、`state/baseline.json` 不提交；零历史会话读记忆（check `demo-fixtures`）
- [x] T045 [P] [US2] D5 `examples/daily/industry-brief/`：`AGENT.md`（`mcp_servers: [brave-search, fetch]`、`deliver_to`）、`scripts/industry_brief.py` + 测试（双 MCP 调用、来源表 URL / 标题 / 日期 / 访问时间、未抓正文只作线索）、`fixtures/search.json` / `fixtures/pages.json`（check `demo-fixtures`）

### Eight templates（每个 `AGENT.md` / `README.md` 与模块卡 §3.6 的夹具；README 固定包含档位 / 依赖 / 证据状态 / 已知限制）

- [x] T046 [P] [US2] `examples/templates/weather-brief/`：`http_get` open-meteo、记忆偏好、无凭据、`fixtures/city.json`；档位 A（check `template-quality`）
- [x] T047 [P] [US2] `examples/templates/arxiv-tracker/`：`http_get` `export.arxiv.org`、运行时间戳、记忆、`fixtures/response.xml`；档位 A（check `template-quality`）
- [x] T048 [P] [US2] `examples/templates/policy-watch/`：指定官方来源、差异对比、`skills/policy-watch/SKILL.md`、`fixtures/announcements.json`；档位 A（check `template-quality`）
- [x] T049 [P] [US2] `examples/templates/ticket-insights/`：DBHub 只读 SQLite、分类、`skills/ticket-insights/SKILL.md`、`fixtures/schema.sql` / `fixtures/rows.json`；档位 A（check `template-quality`）
- [x] T050 [P] [US2] `examples/templates/onboarding-buddy/`：自研 `docs-readonly`、运行时间戳、`skills/onboarding/SKILL.md`、`fixtures/docs/*.md`；档位 A（check `template-quality`）
- [x] T051 [P] [US2] `examples/templates/dependency-watch/`：依赖文件 + OSV HTTP、历史、`fixtures/dependencies.txt` / `fixtures/osv.json`；档位 A（check `template-quality`）
- [x] T052 [P] [US2] `examples/templates/web-watch/`：HTTP + Playwright、`fixtures/pages.json`；档位 **A\***（浏览器未就绪保持 A\*，不改 B）（check `template-quality`）
- [x] T053 [P] [US2] `examples/templates/funding-watch/`：Brave + Fetch，无夹具业务断言，README 明写"未验证，不要当可运行示例"；档位 **B**（check `template-quality`）

### Harness, docs, public increments

- [x] T054 [US2] 新增 `internal/e2e/us14_demos_test.go`：`TestUS14Demos_Fixture`（五 Demo 子测试：业务断言 + 六段证据 + 通知计数 + 自动 / 人工 + 空 / 无变化 / 失败）、`TestLegacyD1_HttpEndpoints`（R0 三端点 200 / 500 / 不可达 + 一次通知汇总，不计入五 Demo）（check `demo-fixtures` / `legacy-d1`）
- [x] T055 [US2] 新增 `internal/e2e/us14_templates_test.go`：`TestTemplates_LoadAndTools`（八模板可装载、内置工具可解析、所选 MCP 工具 ⊆ 目标 server `tools/list`、无凭据）、`TestTemplates_TierEvidence`（A / A\* / B 档位与 README 文案一致；A 不通过不得降 B）（check `template-quality`）
- [x] T056 [US2] 新增 `scripts/run-example-tests.py`：固定目录清单（五个 `examples/daily/*/scripts`）逐目录 `unittest discover`，解析 `Ran N tests` 并断言每目录 N ≥3；0 发现 / 低于下限 / 非零退出 / 缺 Python = 失败（check `script-tests`）
- [x] T057 [US2] 更新 14B 事实同步：`docs/Requirements.md` §2.5（新五场景与旧条件映射注）/ §7.5（`docs-readonly` + 八模板）；`docs/TechnicalDesign.md` §8.1 demo 段；`docs/DevelopmentLoop.md` §4.5.2（五 Demo / 三片 / 文件量 82–110）与 §5.3（补七场景中既有 / 新增的映射，不改既有 e2e 规则）；`docs/Operations.md` §1.2 例外表与 §7 依赖描述，并新增"示例 / 模板依赖与出网边界"小节（check `acceptance`）
- [x] T058 [US2] 修改 `README.md`：英文单语，加 Templates & demos / MCP catalog / Run a demo / What is verified 四节，链接 `examples/README.md`，不链接私有路径；同步 `internal/cli/public_docs_test.go` 的公开文档断言（新四节 / `examples/` 链接允许 / 私有路径拒绝）并保持绿（check `website-test`）
- [x] T059 [US2] 新增 `website/.vitepress/data/templates.mjs`（五 Demo + 八模板 + 六目录，双语字段）与 `website/.vitepress/theme/components/TemplateTable.vue`；修改 `website/docs/index.md` / `website/zh/docs/index.md` 加区块（不新增一级路由）；修改 `website/tests/content.test.mjs` / `website/scripts/check-links.mjs`（模板表一致性、允许 `examples/`、拒绝私有路径）。**不改** US-13 已验收视觉文件（`Layout.vue` / `HomeHero.vue` / `CapabilityGrid.vue` / `CliDemo.vue` / `DiagramFigure.vue` / `LanguageSwitch.vue` / `SiteFooter.vue` / `custom.css` / `index.ts`）与 `website/public/**` / `website/diagrams/**`；收口时以哈希核对 SC-011（checks `website-test` / `website-build` / `website-check`）
- [ ] T060 [US2] **（阻塞：外部授权；manual，不绑定自动 check）** 按模块卡 §4.5 / §6 记录 D1 GitHub PAT、D2 通知接收方、D4 DBHub、D5 Brave / Fetch 的真实实跑证据；全部外部服务当前**未授权**，只做夹具并在 run-state 未验证清单登记；不得用 HN / 内置 HTTP 替代，不得把夹具写成真实实跑

**Checkpoint**: `docs-readonly-tests` / `script-tests` / `demo-fixtures` / `legacy-d1` / `template-quality` 绿；六段证据与业务断言可核对；T060 的外部授权未解除时 14B 不完成，不宣称 US-14 完成。

---

## Phase 5: User Story 3 - 发布收尾（Priority: P3）——14C

**Goal**: 候选二进制绑定、七场景 e2e、干净检出、stage、依赖 / 许可 / 平台 / 升级证据，形成 `v0.1.0` 锚点（tag 由用户手动）。

**Independent Test**: `make e2e`（`binary_source=candidate` + SHA-256）；`sh scripts/check-release-clean.sh`；`.\scripts\check-acceptance.ps1 --stage harness/stages/us14-release.json`。

- [x] T061 [US3] 修改 `Makefile`：新增 `example-tests`（`go -C examples/shared/mcp/docs-readonly test -count=1 ./...` + `python scripts/run-example-tests.py`）与 `e2e`（依赖 `build` + `example-tests`，导出 `BASALT_E2E_BINARY=$(CURDIR)/bin/basalt` 后跑 `go test -tags=e2e -count=1 ./internal/e2e`）；两者不加入 `check` / `acceptance` / `acceptance.json` / CI（check `e2e`）
- [x] T062 [US3] 修改 `internal/e2e/harness_test.go` 的 `TestMain`：存在 `BASALT_E2E_BINARY` 时直接使用该绝对路径、重算 SHA-256、打印 `binary_source=candidate` / `binary_path` / `binary_sha256`，**不得**另行构建；缺失时保留现有 temp 构建回退并打印 `binary_source=temp`（供 `demo-fixtures` / `legacy-d1` / `template-quality` 直接 `go test -tags=e2e` 使用）。仅 `make e2e` / stage `e2e` 断言标记为 `candidate`；标记缺失或为 `temp` = 发布证据失败（模块卡 §6.11 第 7 条）（check `e2e`）
- [x] T063 [US3] 新增 `internal/e2e/us14_release_test.go`：`TestReleaseE2E_Cancel`（Run `cancelled` + 无子进程残留 + 会话可继续）、`TestReleaseE2E_ToolTimeout`（工具审计 `failed` + `timeout`，Run 继续 `completed`，失败结果回填）、`TestReleaseE2E_HotReload`（运行中改写 `AGENT.md` 不影响本次、下一次取新正文）、`TestReleaseE2E_SevenScenarioMap`（七场景映射断言）；取消与超时**分开**写、分开 required（check `e2e`）
- [x] T064 [US3] 新增 `scripts/check-release-clean.sh`：要求 `git status --porcelain` 为空；`git archive HEAD` 到临时目录；在临时目录构建 / 门禁 / e2e；缺候选提交 / 脏工作区 = 失败（check `clean-checkout`）
- [x] T065 [US3] 新增 `harness/stages/us14-release.json`：`inputs` / `protect` / `checks` / `tasks`；check 集含 `acceptance` / `e2e` / `mcp-admin` / `console-test` / `console-build` / `website-*` / `templates` / `example-tests` 与 §4.3 逐项命令；`protect` 按模块卡 §3.8 列表；不注册进 `current.json`，不进 CI，`external` 保持未授权（check `acceptance`）
- [x] T066 [US3] 产出发布证据（按 `harness/acceptance/records` / `reports` 既有约定）：版本 + commit + 实际执行的候选路径与 SHA-256（与分发文件逐字节一致）、`make acceptance` / `make e2e` 读数、七场景逐项、Windows + Linux 双平台走查、依赖与许可表（Go / Node / Python / MCP 包，版本与许可可溯源）、升级 / 恢复演练（Operations §5）；Linux 走查与外部项由用户环境执行，未执行即登记未验证（check `acceptance`）
- [x] T067 [US3] 如升级 / 恢复演练发现缺口，更新 `docs/Operations.md` 发布 / 升级 / 恢复说明；不改已批准流程语义（check `acceptance`）

**Checkpoint**: 候选二进制七场景绿；`check-release-clean.sh` 通过；stage 全绿；双平台与许可 / 升级证据齐全；`v0.1.0` tag **由用户手动**。

---

## Phase 6: Polish & Cross-Cutting

- [x] T068 运行 `make check`、`make acceptance`、`make example-tests`、`make e2e` 与 `.\scripts\check-acceptance.ps1 --stage harness/stages/us14-release.json`，按 `quickstart.md` 逐节记录四态读数（通过 / 失败 / 跳过 / 未验证）并更新 run-state 未验证清单；同时核对 `git diff --stat -- go.mod go.sum docs/contracts/schema.sql harness/acceptance.json harness/stages/current.json .github/workflows/ci.yml` 为空（FR-025 零迁移 / 零依赖 / 不改门禁与 CI）
- [x] T069 按 `quickstart.md` 做管理台人工走查（目录 / 预填 / 503 提示 / 诊断 / 测试详情 / 四轴文案），记录截图或文字证据；不得出现"已验证"徽章
- [x] T070 收口三件套（module-dev 步骤 7）：`/speckit-analyze` 复跑 → 独立复核（子代理）→ 七项证据 DoD + 变更总结四段；判据面变更逐路径核对；提交人工 review ② 包，等待用户决定

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1（Setup）**：无依赖，立即开始；T001–T009 中 T007 依赖 T002，T009 依赖 T007 / T008。
- **Phase 2（Foundational）**：依赖 Phase 1 类型；阻塞 US1。
- **US1（Phase 3）**：依赖 Phase 1 + Phase 2；其 Tests（T013–T017）先于实现（T018–T029）。
- **US2（Phase 4）**：依赖 US1 的目录 / 诊断 / 测试 / 工具来源能力；T036–T053 可并行；T054–T056 依赖对应示例；T060 有外部阻塞（T058 / T059 的 US-13 门已解除）。
- **US3（Phase 5）**：依赖 US1 + US2 + US-13 + US-15 验收设施；T061 依赖 T036–T056。
- **Polish（Phase 6）**：依赖全部切片。

### User Story Dependencies

- **US1（P1）**：独立完成、独立验收；是 US2 / US3 的前置。
- **US2（P2）**：依赖 US1；`examples/**` 与 `internal/e2e` 只读消费产品能力；README / 官网增量在 US-13 收口后按 T058 / T059 的冻结面执行。
- **US3（P3）**：依赖 US1 + US2；发布 e2e 复用既有 `TestBackfill_*`，不修改其语义。

### Within Each Story

- 测试先于或伴随实现；同一任务内实现与测试一起跑；失败即时修复，不累积。
- 冻结输入（模块卡 R3 / D-01–D-10）不得修改；判据面变更停下确认。
- 同一问题修复 3 次不收敛 → 停走 `/debug`；累计预算见 run-state。

### Parallel Opportunities

- T001–T006 可并行（不同文件）；T008 / T009 在 T007 后。
- T010 可并行于 T006–T009；T011 / T012 串行。
- US1 的 T013–T017 可并行；实现按依赖串行（T018 → T019；T020 → T021 → T022；T023 → T024 → T025；T026 → T027 → T028 → T029）。
- T030–T033 控制台可与 Go 侧并行（不同文件）；T034 最后。
- US2 的 T036–T053 大部分可并行（不同目录）；T054–T056 在对应文件后。
- US3 的 T061–T065 部分可并行（不同文件）；T066 最后（依赖全部读数）。

---

## Task → Check Mapping

| Check id | 任务 |
| --- | --- |
| `mcp-catalog` | T002, T006–T009 |
| `mcp-env-gate` | T013, T018–T019 |
| `mcp-diagnostics` | T012, T014, T020 |
| `mcp-test-record` | T005, T011, T016, T021–T022 |
| `mcp-disposition` | T004, T010, T015, T023–T025 |
| `mcp-contracts` | T001, T003, T017, T026–T029 |
| `demo-fixtures` | T038, T041–T045, T054 |
| `legacy-d1` | T054 |
| `template-quality` | T040, T046–T053, T055 |
| `docs-readonly-tests` | T036–T037 |
| `script-tests` | T039, T056 |
| `e2e` | T061–T063 |
| `console-test` / `console-build` | T030–T034 |
| `website-test` / `website-build` / `website-check` | T058–T059 |
| `acceptance` / `clean-checkout` | T035, T057, T064–T068 |

## Blocked / Unauthorized Items（不得伪装完成）

| 项 | 阻塞条件 | 处置 |
| --- | --- | --- |
| T060 D1 / D2 / D4 / D5 真实外部实跑 | 模型 / GitHub PAT / Brave / Fetch / Playwright / DBHub / 通知接收方**全部未授权** | 只做夹具；证据标"未验证"；14B 不完成 |
| T066 Linux 走查 / 依赖许可核实 / 升级恢复演练 | 需用户提供 Linux 环境与许可来源；AI 不代打 tag | 如实登记；14C 不完成 |
| `v0.1.0` tag | 用户手动执行 | AI 不代打，不写 tag 证据为已完成 |

## Implementation Strategy

### MVP First（US1 = 14A）

完成 Phase 1–3：契约 / 类型 / 目录 / 闸门 / 诊断 / 测试记录 / 工具来源 / 控制台全部绿后独立验收 14A；不进入 14B 直至 14A 检查全绿。

### Incremental Delivery

1. 14A → 独立验收（`mcp-*` + `console-*` + `make check`）；
2. 14B → 独立验收（`docs-readonly-tests` / `script-tests` / `demo-fixtures` / `legacy-d1` / `template-quality`）；外部授权（T060）解除前保持未完成；
3. 14C → 独立验收（`make e2e` / `clean-checkout` / stage / 发布证据）；候选提交与平台环境由用户提供。

### Notes

- 任务 ID 按执行顺序；勾选由控制侧验收后更新，实现者不得自行勾选。
- 所有测试函数名英文；中文验收点以注释保留（模块卡原文逐条对号）。
- `go.mod` / `go.sum` / `schema.sql` / `harness/acceptance.json` / 既有 stage / CI 不在允许修改面。

## Phase 7: 已批准范围变更（2026-10-26）——内嵌 Demo 安装器与 DBHub 1.5.0 对齐

用户 2026-10-26 裁决：Basalt 默认附带五个 Demo 素材，通过独立安装命令按需准备；现有 DBHub 配方按实测 1.5.0 一并修正。追加任务不改既有勾选与编号。

- [ ] T071 [US2] 交付内嵌素材与独立安装命令：`examples/embed.go`（`//go:embed daily shared`）、`internal/cli/demo.go`、`internal/cli/demo_test.go`、`internal/cli/root.go` 注册 `demo install`；在目标工作区写 Demo 目录、`<ws>/demos/install.json` 清单、`<ws>/demos/mcp/dbhub.toml`（`[[sources]]` 指样例库 + `[[tools]] name="execute_sql" readonly=true max_rows=500`）、可直接粘贴的 `mcp_servers.yaml` 片段与逐项待配置提示；两个 GitHub Demo 共用一份 GitHub MCP；`basalt init` 职责不变、模型留空、Demo 停用、安装器不接收也不回显凭据（check `acceptance`）
- [ ] T072 [US2] 按实测 DBHub 1.5.0 对齐配方与夹具：`examples/daily/biz-daily/**`、`examples/templates/ticket-insights/**`、`internal/catalog/data/mcp_catalog.yaml`（dbhub 改 `--transport stdio` + `--config`，移除已不存在的 `--readonly`；github 增 `--read-only`）、`internal/e2e/us14_demos_test.go` 的 `execute_sql` 夹具与 `demoDBHubEnvelope`、`internal/e2e/us14_templates_test.go` 的 `templateCandidateFaces["dbhub"] = {"execute_sql"}`；只读由服务端实际拒绝写操作验证，业务断言强度不变（check `mcp-catalog` / `demo-fixtures` / `template-quality`）
- [ ] T073 [US2] 交付凭据获取指引：`examples/README.md` 的 Credentials 节与 github-radar / repo-digest / industry-brief 的 SETUP（官方创建入口、最小只读权限、有效期、套餐或费用要求、`${ENV_VAR}` 本机配置与验证命令、不可达环境的实测声明）；不落盘任何凭据值（check `script-tests`）
- [ ] T074 [US3] 变更后重跑受影响目标并完整重跑 stage 出 r5（`harness/acceptance/records/us14-release-gate-r5.json` 与 `records/reports/us14-release-gate-r5.json`），逐项四态记录；在 run-state §2.2 登记 DBHub 判据面变更（check `demo-fixtures` / `template-quality` / `e2e` / `acceptance` / `clean-checkout`）

## Phase 8: 源码单一事实源与制品级安装验收（2026-10-26 第二轮裁决）

用户 2026-10-26 追加裁决：`go:embed` 跨不过嵌套 module，`docs-readonly` 并回主 module；安装器必须实际导出源码并保留独立构建体验；新增制品级验收。追加任务不改既有勾选与编号；T071 / T074 正文里的 `acceptance` 为早期 check 名（该 check 已于 2026-10-25 因独占锁移除），实际映射以 stage 的 `demo-installer` / `stage-lint` / `release-records` 为准。

- [ ] T075 [US2] `docs-readonly` 并回主 module 并保持源码单一事实源：删除 `examples/shared/mcp/docs-readonly/go.mod`，`//go:embed daily shared` 覆盖其源码；安装器把源码导出到 `<ws>/demos/src/docs-readonly` 并生成最小 `go.mod`（`module basalt-docs-readonly` + `go 1.27`）与 `deps/` 目录；`examples/embed.go`、`Makefile` 的 `example-tests`、`internal/e2e/us14_demos_test.go` 的 `go build` 随主 module 调整；素材仍纯标准库、零第三方依赖、不被产品二进制导入、仍作为独立 MCP 进程运行（check `docs-readonly-tests` / `demo-installer` / `go-build`）
- [ ] T076 [US2] 增加安装器回归断言：源码导出与生成的 `go.mod` 内容、`<ws>/demos/deps` 目录、报告中的实际路径与按路径渲染的构建命令、Agent 目录内不再有 `skills/` 副本而 `<ws>/skills/<skill>` 存在、既有 `basalt.yaml` / `mcp_servers.yaml` 逐字节不变、凭据提示指向本次选中 Demo 的 SETUP（check `demo-installer`）
- [ ] T077 [US3] 增加制品级验收 `internal/e2e/us14_demo_install_test.go`：只用构建后的二进制与已声明的 Go 工具链，在临时工作区执行 `basalt init` → `basalt demo install --demo docs-diff`，按报告打印的命令原样构建导出源码，再用真实 MCP 客户端完成连接、工具发现（三件套）与一次 `read_document` 正文读取（check `demo-fixtures` / `e2e`）
- [ ] T078 [US2] 同步判定口径与事实：`spec.md` FR-018 / FR-019 + SC-013；模块卡 C-16 / §3.1.8 / §3.10 / §5.3 / §6.10（J-11 / J-12）与 Q-03；`plan.md` / `research.md` / `quickstart.md`；仓库正文 Requirements / TechnicalDesign / Operations / DevelopmentLoop；`examples/**` 文档（biz-daily / docs-diff / ticket-insights / onboarding-buddy / repo-digest / examples/README）；删除全部 `--readonly` 与"嵌套 module"旧口径并保留实测声明（check `docs-consistency` / `mcp-catalog` / `demo-fixtures` / `template-quality`）
