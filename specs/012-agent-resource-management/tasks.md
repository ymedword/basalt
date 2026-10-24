# Tasks: US-12 Agent 与公共资源动态管理

**Input**: Design documents from `specs/012-agent-resource-management/`
**Prerequisites**: [plan.md](plan.md)（必需）、[spec.md](spec.md)（必需）、[research.md](research.md)、[data-model.md](data-model.md)、[quickstart.md](quickstart.md)
**模块卡（人审 R3.1）**: [docs/us/us12-agent-resource-management.md](../../docs/us/us12-agent-resource-management.md) §3.1 / §3.2 / §3.3 / §4.4 / §6.2

**Tests**: 本 US 要求测试先行或伴随实现（模块卡 harness 先行口径）。测试函数名一律英文 `TestXxx_yyy`；模块卡中的中文用例名以注释保留在对应测试函数上方。

**Organization**: 任务按依赖顺序分十三阶段；Phase 3 起每阶段对应 spec 的一个用户故事，可独立验收。专属 check 标注 `check:`；check ID 以模块卡 §4.4 的十八条为唯一枚举。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属用户故事（US1–US9），仅故事阶段使用
- 每个任务给出精确文件路径

## Path Conventions

- 仓库根：`D:\code\basalt`；Go module `github.com/ymedword/basalt`
- Go 代码：`internal/<pkg>/`；前端工程：`assets/console/`；嵌入入口：`assets/embed.go`
- Harness 工件：`harness/stages/`

---

## Phase 1: Setup（共享前置）

**Purpose**: 领域类型、错误分类、配置与样例；此阶段不交付任何用户故事。

- [x] T001 修改 `internal/core/errors.go`：新增 `ErrConflict` / `ErrBusy` / `ErrImportRejected` 分类与 `data` 载体；`ErrEditUnsupported` / `ErrPreconditionFailed` 复用 `internal/core/management.go` 既有声明（M6），不重复声明
- [x] T002 [P] 修改 `internal/core/mcp.go`：`MCPServerConfig` 追加 `Transport` / `Args` / `URL`（既有 `Name` / `Command` / `Env` 语义不变）；追加 `PublishedMCPServer{VersionID, Config}`；不新建同义配置类型
- [x] T003 [P] 新建 `internal/core/author.go`（`AuthorConfig` / `AuthorFile` / `AuthorReferences` / `BuildAuthorPrompt` / `ParseAuthorDraft`）与 `internal/core/author_test.go`（6 例：提示词稳定、草稿解析、非法路径）；`check: 012-core-author-agent`
- [x] T004 [P] 新建 `internal/core/agent_admin.go`（`AgentSpec` / `AgentFileInfo` / `AgentArchiveRecord`）与 `internal/core/agent_admin_test.go`（5 例：名称规则、归档记录字段）；`check: 012-core-author-agent`
- [x] T005 [P] 新建 `internal/core/resource_admin.go`（资源种类 / 版本 / 指针 / 发布请求 / 引用 / 测试结果类型与校验，`ParseResourceKind` 只收三个冻结字面量；动作 / 目标类型字面量常量逐字取契约）与 `internal/core/resource_admin_test.go`（8 例：种类字面量、**`TestManagementActions_*` 动作 / 目标字面量**、版本 / 指针校验、ETag 形状）；`check: 012-core-resource-types`、`012-core-author-agent`
- [x] T006 [P] 新建 `internal/cli/author.go`（`ParseAuthorSection`：缺段零值、缺键不启动失败、temperature 缺省 0.2 范围 `[0,2]`、未知键报错）；修改 `internal/cli/config.go` 只追加 `author:` 段类型化解析（`credentialKey` 与 `providers:` 不变）；测试 `internal/cli/author_test.go`（6 例）与 `internal/cli/config_test.go`（+3）；`check: 012-cli-author`
- [x] T007 [P] 修改 `basalt.yaml.example`：`author:` 从占位注释改为示例段并注明"删除整段即关闭一句话生成"；`shell.allowed_commands` 注释补"MCP 命令必须在此列内"

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: YAML 块编辑、定义文件读写、名称规则、存储读写、ETag / 端口 / 路由骨架与 Provider 发布目录。

**⚠️ CRITICAL**: 本阶段完成前不得开始任何用户故事阶段。

- [x] T008 [P] 新建 `internal/workspace/yaml_block.go`（顶层列表条目块定位 / 替换 / 追加 / 移除；块外逐字保留；不可安全保留 → `ErrEditUnsupported`）与 `internal/workspace/yaml_block_test.go`（14 例，关键回归 `TestYAMLBlock_ReplacePreservesCommentsAndSurroundings` 原样落地）；`check: 012-workspace-yaml-provider`
- [x] T009 [P] 新建 `internal/workspace/provider_file.go`（`providers:` 条目解析 / 渲染 / 原子写；`${VAR}` 校验；不解析无关段环境变量）与 `internal/workspace/provider_file_test.go`（12 例）；`check: 012-workspace-yaml-provider`
- [x] T010 [P] 修改 `internal/workspace/agent_files.go`（`ValidateAgentName` 增前导点与 `.staging` / `.backup` 后缀拒绝；删除 `CreateAgent` / `DeleteAgent`）、`internal/workspace/templates.go`（模板接受 `description` / `model`，默认值不变）、`internal/workspace/agent.go`（只做 frontmatter `name` / `identity.agent_name` 读写）；测试 `internal/workspace/agent_files_test.go`（+10，含 `TestCreateAgent_DefaultsUnchanged` / `TestLifecycle_SingleWriter` / `TestDeleteAgent_ArchivesWithoutTouchingData`）、`internal/workspace/templates_test.go`（+3）、`internal/workspace/agent_test.go`（+3）；`check: 012-workspace-agent`
- [x] T011 [P] 修改 `internal/workspace/skill.go`：暴露工作副本路径与不可变版本目录的映射；测试 `internal/workspace/context_loader_test.go`（+3）；`check: 012-workspace-skill`
- [x] T012 [P] 新建 `internal/storage/agent_admin_store.go`（`InsertArchive` / `MarkRestored` / `ArchiveByID` / `ListArchives`；归档行与操作终态同事务，事务内调用 `finishOperationTx`；**不提供** `UpsertLifecycle`）与 `internal/storage/agent_admin_store_test.go`（8 例）；`check: 012-storage-admin`
- [x] T013 [P] 新建 `internal/web/etag.go`（`parseIfMatch` 只接受 `^"sha256:[0-9a-f]{64}"$`；`*` / 弱 / 逗号列表 412；单文件与集合 ETag 不可互换）与 `internal/web/etag_test.go`（9 例）；`check: 012-web-etag-routes`
- [x] T014 [P] 新建 `internal/web/ports_admin.go`（`AgentAdminService` / `ResourceAdminService` / `NotifyAdminService` / `WorkspaceService` 四个端口接口）与 `internal/web/views_admin.go`（写面视图与错误 `data` 投影、`ResourceVersionDetailView` 字段集合）及 `internal/web/views_admin_test.go`（6 例）；`check: 012-web-etag-routes`
- [x] T015 [P] 新建 `internal/web/admin_routes.go`（写面路由表与条件写中间件；只依赖端口接口；`version_id` 非数字 / ≤0 / 不存在 → 404 `NOT_FOUND`）与 `internal/web/admin_routes_test.go`（7 例：路由表、方法不允许、全部非公开新路由无凭据 401）；`check: 012-web-etag-routes`
- [x] T016 修改 `internal/web/envelope.go`（**只**新增 `IMPORT_REJECTED`，其余复用既有常量）、`internal/web/server.go`（挂载写面路由与条件写中间件）、`internal/app/web.go`（Server `Options` 增写面端口）；测试 `internal/web/contract_test.go`（+4）、`internal/web/server_test.go`（+4）、`internal/app/web_test.go`（+3）；`check: 012-web-etag-routes`
- [x] T017 修改 `internal/app/providers.go`：用 `newProviderCatalogFromPublished(ctx, store, override, logger)` **替换** `newProviderCatalogWithOverride`（唯一生产调用方 `wire.go:445`）；`override` 时每个已发布名各一条替身、不构造真实客户端、零请求；构建失败保留旧表；`newProviderCatalog(cfg)` 保留；测试 `internal/app/providers_test.go`（+8）；`check: 012-app-resource-admin`

**Checkpoint**: 领域类型、文件原语、存储、ETag / 端口 / 路由与 Provider 目录就绪。

---

## Phase 3: User Story 1 - Agent 创建、作者草稿与多文件提交 (Priority: P1) 🎯 MVP

**Goal**: 一句话草稿 → 页内编辑 → 提交创建（默认停用、generation=1、schedules 空）→ 启用后可运行；失败零残余；多文件提交用受管文件集合 ETag。

**Independent Test**: 模板与草稿各创建一个 Agent，检查目录 / 状态 / 注册；同名与非法名 400 零残余；提交受管文件后未列出文件保留；作者模型未配置 503、预写失败 500 且磁盘零变化。

- [x] T018 [P] [US1] 新建 `internal/workspace/agent_draft.go`（候选树校验、完整副本、两次 rename、未列出文件保留、崩溃残留前滚 / 回滚）与 `internal/workspace/agent_draft_test.go`（11 例，含集合 ETag 过期 412）；`check: 012-workspace-agent`
- [x] T019 [P] [US1] 新建 `internal/app/author.go`（`ValidateAuthorConfig` 只查成对键与 temperature；`Draft` 经 `ProviderService.Chat`，`RunID=agent-author-<name>`，不建 Run、不写操作行、草稿不落盘）与 `internal/app/author_test.go`（10 例：503、审计、无 Run、无操作行、草稿限制）；`check: 012-app-notify-author`
- [x] T020 [US1] 新建 `internal/workspace/agent_admin.go`（`CreateAgentStaged` / 复制 / 恢复 / 归档清单与哈希；不改状态文件、不写归档行）与 `internal/workspace/agent_admin_test.go`（17 例，含 `TestRewriteAgentName_*`、原名恢复 generation、异名 Bootstrap、清单冲突）；复制 / 恢复在 US4 验收；`check: 012-workspace-agent`
- [x] T021 [US1] 新建 `internal/app/agent_admin.go` 的创建 / 草稿 / 多文件提交部分（Agent 级写锁内 `SubmitFiles` 重读并比较集合 ETag；失败零残余）与 `internal/app/agent_admin_test.go` 的创建 / 提交用例（测试文件共 25 例，归档 / 复制 / 恢复用例在 T033 补全）；`check: 012-app-agent-admin`
- [x] T022 [US1] 新建 `internal/web/handlers_admin_agents.go`（`POST /agents`、`POST /agents/generate-files`、`POST /agents/{name}/copy`；`GET` / `POST /agents/{name}/files`，响应头 `ETag` 与视图 `etag` 同为集合 ETag）与 `internal/web/handlers_admin_agents_test.go`（12 例：响应 / 错误码、GET files 集合 ETag、过期提交 412 零操作行）；`check: 012-web-admin-agents`
- [x] T023 [US1] 修改 `internal/app/wire.go`：装配作者服务与 `AgentAdmin`，写面端口注入 `web.Options`；保持既有 `Options.ProviderClient` 评测注入路径不变
- [x] T024 [P] [US1] 前端：新增 `assets/console/src/api-admin.ts`（写面端点、`If-Match`、错误映射）、`assets/console/src/views/agents-new.ts`（创建 + 草稿编辑 + 提交）、`assets/console/src/components/agent-files.ts`（多文件编辑与提交，写后重读）与 `assets/console/tests/agent-files.test.ts`（412 重读、409 分支、无乐观更新）

**Checkpoint**: US1 可独立验收——草稿、创建、提交与启用链路成立。

---

## Phase 4: User Story 2 - 公共资源版本化发布与失败保旧 (Priority: P1)

**Goal**: 模型连接等公共资源保存并发布；版本只追加；发布失败保留旧文件与旧指针；撤回用旧内容创建新版本；陈旧 ETag 412 零写入。

**Independent Test**: 新增模型连接并发布，检查版本与指针；非法 `base_url` / 缺失环境变量发布失败且文件哈希与指针不变；发布旧版本，版本号递增且来源版本正确；陈旧 `If-Match` 412 零操作行。

- [x] T025 [P] [US2] 新建 `internal/app/resource_admin.go` 的 `Upsert` / 发布 / 模型连接测试部分（锁序：资源锁 → 定义文件锁 → 重读 → 分歧闸门 → 块编辑 → 原子写 → `ResourceGate` 写锁内同事务写版本 / 指针 / 终态；失败逆序补偿并检查文件身份）与 `internal/app/resource_admin_test.go` 的发布 / 失败保旧 / 撤回 / 模型测试用例（测试文件共 32 例，删除 / 引用 / 版本详情用例在 T040 / T042 补全），关键回归 `TestResourceAdmin_PublishFailureKeepsOldPointer` 原样落地；`check: 012-app-resource-admin`
- [x] T026 [US2] 新建 `internal/web/handlers_admin_resources.go` 的模型连接端点与版本 / 发布端点，及 `internal/web/handlers_admin_resources_test.go` 对应用例（测试文件共 19 例，其余在 T041 / T042 补全），关键回归 `TestAdminResources_StaleETag412NoSnapshot` 原样落地；`check: 012-web-admin-resources`
- [x] T027 [US2] 修改 `internal/app/wire.go` / `internal/app/web.go`：装配 `ResourceAdmin` 服务并注入写面端口
- [x] T028 [P] [US2] 前端：新增 `assets/console/src/components/resource-form.ts`（模型连接 / MCP / 技能新建表单，页内）与 `assets/console/tests/resource-form.test.ts`（新建 / 412 / 400 `problems`）
- [x] T029 [P] [US2] 前端：新增 `assets/console/src/views/resource-detail.ts`（版本列表 / 详情、发布 / 撤回 / 删除 / 引用 / 测试；412 重读 ETag 并保留输入）

**Checkpoint**: US2 可独立验收——发布、失败保旧与撤回语义成立。

---

## Phase 5: User Story 3 - 发布—准入一致、启动对账与目录构建 (Priority: P1)

**Goal**: 发布只影响下一次准入；启动先对账文件与已发布版本，再构建 Provider 目录 / MCP 集合；启动顺序固定。

**Independent Test**: 发布新版本后核对在跑 Run 与新建 Run 的映射差异；重启核对对账决策与构建来源；MCP 发布未重启时标"待重启"，重启后追平。

- [x] T030 [P] [US3] 新建 `internal/app/definition_reconcile.go`（四种决策：一致 / 文件新 / 版本新 / 文件无效；相同哈希跳过；非法文件保留旧版本并 WARN；对账操作幂等）与 `internal/app/definition_reconcile_test.go`（12 例，关键回归 `TestReconcile_InvalidEditKeepsPublished` 原样落地）；`check: 012-app-reconcile`
- [x] T031 [US3] 修改 `internal/app/run.go`：`serve` 启动顺序接入定义对账与 supervisor（配置 → 迁移 / 恢复 → 基线 → 管理恢复 → 对账 → 目录 → 注册表 → supervisor → 服务）；运行 `Runtime` 在 `FinishRun` 后释放；测试 `internal/app/run_test.go`（+6，含 `TestStartupOrder_*`、`TestRunAdmission_*`、`TestRunRuntime_*`）；`check: 012-app-agent-admin`、`012-app-resource-admin`
- [x] T032 [US3] 修改 `internal/app/wire.go`：对账与启动顺序依赖接线；注册表 / Provider 目录 / MCP 集合只由已发布版本构建（测试断言构建来源）

**Checkpoint**: US3 可独立验收——发布—准入一致与启动顺序成立。

---

## Phase 6: User Story 4 - Agent 复制、归档与恢复 (Priority: P2)

**Goal**: 复制改名停用、零残余；归档按固定顺序并完整回滚；原名 / 异名两条恢复路径；CLI `profile delete` 写真实归档行。

**Independent Test**: 复制检查名字 / 身份 / 停用 / 空定时；归档空闲与长跑 Agent 检查目录、状态、记录与超时回滚；原名与异名恢复检查代次与名字；rename 失败回退。

- [x] T033 [US4] 完成 `internal/app/agent_admin.go` 的复制 / 归档 / 恢复编排（归档：注销定时 → 移出注册 → 30 秒有界等待 → 状态迁移 → 目录移入 → 归档行 + 终态；三档回滚矩阵；`now.UTC()`）与 `internal/app/agent_admin_test.go` 剩余用例（总计 25 例），关键回归 `TestAgentArchive_TimeoutRollsBack` 原样落地；`check: 012-app-agent-admin`
- [x] T034 [US4] 新建 `internal/web/handlers_admin_archived.go`（`DELETE /agents/{name}`、`GET /archived-agents`、`POST /archived-agents/{archive_id}/restore`）与 `internal/web/handlers_admin_archived_test.go`（10 例：归档 409、原名恢复保留 generation、异名 Bootstrap、列表）；`check: 012-web-admin-agents`
- [x] T035 [US4] 修改 `internal/app/schedule.go`：归档注销 / 恢复不自动启用；写锁顺序；测试 `internal/app/schedule_test.go`（+4）；`check: 012-app-agent-admin`
- [x] T036 [US4] 修改 `internal/app/ports.go`：`agentsPort.Create/Delete` 改为 CLI 写路径的完整生命周期所有者——取**一次**实例锁 → 打开并迁移 DB → 装配最小管理组件 → `AgentAdmin` → 关闭并释放；测试 `internal/app/ports_test.go`（+4）与 `internal/cli/profile_test.go`（+2，`TestCLIProfileDelete_WritesArchiveRow`）；`check: 012-cli-author`
- [x] T037 [US4] 修改 `internal/app/wire.go` / `internal/app/web.go`：装配归档路由与服务；`/archived-agents` 含 CLI 归档行
- [x] T038 [P] [US4] 前端：新增 `assets/console/src/views/archived-agents.ts`（归档列表与恢复，含改名二次确认）

**Checkpoint**: US4 可独立验收——复制、归档、两条恢复与 CLI 归档可见性成立。

---

## Phase 7: User Story 5 - 引用视图与受控删除 (Priority: P2)

**Goal**: 删除前检查 Agent 定义（解析失败按可能引用）与未释放运行引用；被引用 400 点名，无 force；版本历史保留，同名重建版本号继续递增；版本详情端点交付。

**Independent Test**: 被引用技能删除 400 点名；活动 Run 引用删除 400 点名；解除后删除成功且历史可查；重建后 `version_no` 递增；版本详情成功与脱敏 fail-closed。

- [x] T039 [P] [US5] 修改 `internal/storage/resource_store.go`：追加 `MarkDeleted`（只置 `deleted_at` / `operation_id`，不删指针行、不删版本）；活动引用复用 `ReferencedBy`；测试 `internal/storage/resource_store_test.go`（+8）；`check: 012-storage-admin`
- [x] T040 [US5] 完成 `internal/app/resource_admin.go` 的删除 / 引用联合视图（Agent 注册表 + 归档扫描；运行引用来自 `released_at IS NULL`；解析失败按可能引用）与 `TestReferences_*` / `TestResourceDelete_*` / `TestResourceRecreate_*` 用例；`check: 012-app-resource-admin`
- [x] T041 [US5] 完成 `internal/web/handlers_admin_resources.go` 的删除 / 引用 / 版本列表端点与对应测试；`check: 012-web-admin-resources`
- [x] T042 [US5] 交付 `GET /resource-versions/{version_id}`：路由注册在 `admin_routes.go`、handler 在 `handlers_admin_resources.go`、投影 `ResourceVersionDetailView`；只读不写操作行；`definition_yaml` 对模型连接 / MCP 返回已发布块原文、技能恒空串；脱敏 fail-closed（任何凭证值非精确 `${VAR}` → 500 固定文案，不返回部分内容）；测试 `TestResourceVersionDetail_*`（app）与 handler 成功 / 脱敏 / 技能空串用例（web）；`check: 012-app-resource-admin`、`012-web-admin-resources`
- [x] T043 [US5] 修改 `internal/app/wire.go`：删除 / 引用 / 版本详情端点接线

**Checkpoint**: US5 可独立验收——引用闸门、版本保留与版本详情成立。

---

## Phase 8: User Story 6 - MCP 双传输、授权与待重启 (Priority: P2)

**Goal**: stdio / http 双传输；按传输分支授权；发布不生效、重启追平；测试端点先审计后调用、15 秒超时、零 Run / 零 `llm_calls`。

**Independent Test**: stdio 发布后 `pending_restart=true`、`connected=false`；重启后按策略连接；http 测试走请求授权、零进程；IP 字面量 / 非默认端口拒绝；测试返回工具发现且不产生运行。

- [x] T044 [P] [US6] 修改 `internal/workspace/mcp.go`：`ParseMCPServers` 统一返回扩展后的 `core.MCPServerConfig`；严格候选校验（transport / args / url 互斥、env 精确占位、重复名）；既有文件兼容多词 `command` 拆分并 WARN；`args` 与多词 `command` 并存拒绝；测试 `internal/workspace/mcp_test.go`（+8）；`check: 012-workspace-yaml-provider`
- [x] T045 [P] [US6] 修改 `internal/tool/mcp.go`：`dialCommand` 改用 `exec.Command(spec.Command, spec.Args...)`，**删除** `strings.Fields`；工具来源为 supervisor 快照；测试 `internal/tool/mcp_test.go`（+5）；`check: 012-app-mcp-supervisor`
- [x] T046 [US6] 修改 `internal/app/mcp.go`：由 supervisor 接管连接；启动走 `Policy.Check` + 审计；测试 `internal/app/mcp_test.go`（+4）；`check: 012-app-mcp-supervisor`
- [x] T047 [US6] 新建 `internal/app/mcp_supervisor.go`（stdio `SHELL_COMMAND` + 进程树回收；http `ActionHTTPRequest` + 域名白名单、零进程；连接成功才写 `applied`；`pending_restart` 派生；测试 15 秒与清理）与 `internal/app/mcp_supervisor_test.go`（17 例，关键回归 `TestMCPSupervisor_PolicyDenied` 原样落地）；`check: 012-app-mcp-supervisor`
- [x] T048 [US6] 完成 `internal/web/handlers_admin_resources.go` 的 MCP 端点（创建 / 更新 / 删除 / 发布 / 测试；仅回可执行名与环境变量键名）与对应测试；`check: 012-web-admin-resources`
- [x] T049 [US6] 修改 `internal/app/wire.go` / `internal/app/run.go`：supervisor 在注册表之后、服务开放之前启动；发布 / 应用版本接线

**Checkpoint**: US6 可独立验收——双传输、授权、待重启与测试端点成立。

---

## Phase 9: User Story 7 - 技能导入边界与不可变版本 (Priority: P3)

**Goal**: 导入边界全部拒绝；工作副本 + 不可变版本快照；全量替换与两次 rename；校验端点与写入同口径。

**Independent Test**: 越界 / 软链接 / 超限大小 / 超限文件数 / 大小写重复全部 400；合法导入工作副本 + 只读快照；校验端点不落盘、不写操作行；同名导入 409。

- [x] T050 [P] [US7] 新建 `internal/workspace/skill_admin.go`（导入边界、工作副本写入、版本快照、staging 全量替换、删除归档）与 `internal/workspace/skill_admin_test.go`（16 例，含 `TestSkillImport_TextOnly`）；`check: 012-workspace-skill`
- [x] T051 [US7] 完成 `internal/app/resource_admin.go` 的技能创建 / 导入 / 更新 / 发布路径与对应用例（全量替换、快照不可变、失败回滚）及 `internal/app/resource_admin_test.go` 技能用例；`check: 012-app-resource-admin`
- [x] T052 [US7] 完成 `internal/web/handlers_admin_resources.go` 的技能端点（含 `POST /skills/validate`，与写入共用校验函数、不落盘、不写操作行、不执行脚本）与对应测试；`check: 012-web-admin-resources`
- [x] T053 [US7] 修改 `internal/app/wire.go`：技能写路径接线（工作副本 / 快照目录）

**Checkpoint**: US7 可独立验收——导入边界与不可变版本成立。

---

## Phase 10: User Story 8 - 通知渠道 CRUD 与引用闸门 (Priority: P3)

**Goal**: 渠道创建可省略地址、同名 400、更新留空保留原值、删除整词扫描（含归档）、`?force=true` 才删除、列表只回 host；发送读 DB 渠道。

**Independent Test**: 非法 URL 400、列表只回 host；Agent 正文整词引用删除 400 点名、force 才删；更新留空保留；发送时环境变量缺失记 ERROR。

- [x] T054 [P] [US8] 新建 `internal/workspace/notify_scan.go`（活动与归档 `AGENT.md` 整词扫描；解析失败按可能引用）与 `internal/workspace/notify_scan_test.go`（8 例）；`check: 012-workspace-skill`
- [x] T055 [P] [US8] 新建 `internal/storage/notify_admin_store.go`（通知渠道 CRUD；空 URL 保存、更新留空保留原值、唯一名；发送读取同一行）与 `internal/storage/notify_admin_store_test.go`（7 例）；`check: 012-storage-admin`
- [x] T056 [US8] 新建 `internal/app/notify_admin.go`（占位校验、空 URL、脱敏、引用删除、force）与 `internal/app/notify_admin_test.go`（10 例）；修改 `internal/app/notify.go`（发送读取 DB 渠道，`${ENV_VAR}` 发送时解析，缺失记 ERROR 不回退明文）；测试 `internal/app/notify_test.go`（+5）；`check: 012-app-notify-author`
- [x] T057 [US8] 新建 `internal/web/handlers_admin_notify.go`（POST / PUT / DELETE；GET 列表 / 类型沿用 US-11 `handlers_notify_read.go`，不重复注册；同名创建 400）与 `internal/web/handlers_admin_notify_test.go`（9 例）；`check: 012-web-admin-notify-workspace`
- [x] T058 [US8] 修改 `internal/app/wire.go`：通知写面与发送路径接线
- [x] T059 [P] [US8] 前端：新增 `assets/console/src/components/notify-channels.ts`（CRUD、URL 占位符、只显示 host、force 二次确认，挂 `/console/settings/notifications` 页内）与 `assets/console/tests/notify-channels.test.ts`

**Checkpoint**: US8 可独立验收——通知写面、引用闸门与发送读取成立。

---

## Phase 11: User Story 9 - 只读工作区浏览与管理台写面 (Priority: P3)

**Goal**: `/console/workspace` 只读浏览 Agent 目录；越界 / 软链接 / 超限拒绝；四条新路由静态先于动态；写面不做乐观更新。

**Independent Test**: 符号链接不跟随、>256 KiB 返回 400 不截断、`.staging` / `.backup` 不可见；`/console/agents/new` 不被 `/console/agents/:name` 抢先匹配；未知路径 404。

- [x] T060 [P] [US9] 新建 `internal/app/workspace_browse.go`（`os.Root` 打开 `.basalt/agents/`；拒绝 `.` 之外的 `..`；符号链接只报类型；读前 `Stat` 判大小，>256 KiB 直接 400、不读正文、不标截断）与 `internal/app/workspace_browse_test.go`（8 例）；`check: 012-app-notify-author`
- [x] T061 [US9] 新建 `internal/web/handlers_admin_workspace.go`（`GET /workspace/tree`、`GET /workspace/file`）与 `internal/web/handlers_admin_workspace_test.go`（6 例）；`check: 012-web-admin-notify-workspace`
- [x] T062 [US9] 修改 `internal/app/wire.go`：工作区浏览服务接线
- [x] T063 [P] [US9] 前端：新增 `assets/console/src/views/workspace-browser.ts`（`.basalt/agents/` 只读树与文件预览；文件名为适配 US-11 既有 `views/workspace.ts`，路由仍为 `/console/workspace`）
- [x] T064 [P] [US9] 前端：修改 `assets/console/src/router.ts` 追加 4 条路由（`agents/new`、`resources/:kind/:id`、`archived`、`workspace`；静态段先于动态段）与新增 `assets/console/tests/router.test.ts`（静态先于动态、未知路径 404）

**Checkpoint**: US9 可独立验收——工作区边界与四条新路由成立。

---

## Phase 12: Polish & Cross-Cutting Concerns

**Purpose**: 端到端、操作视图回归、文档、stage 与全量验收。

- [x] T065 [P] 新建 `internal/e2e/admin_console_test.go`（6 例：创建 → 启用 → 归档 → 恢复 + `TestUpgradeNotes_Documented` 对 `docs/Operations.md` 的迁移说明做文档断言）并修改 `internal/e2e/harness_test.go`（+4 写面 fixture）；`check: 012-e2e`
- [x] T066 [P] 修改 `internal/web/handlers_operations_test.go`（+3：新动作组的操作视图）；`check: 012-web-etag-routes`
- [x] T067 [P] 文档：修改 `docs/Operations.md`（MCP 白名单 / 域名边界、**旧多词 `command` 拆分为 `command` + `args` 的迁移示例**、归档恢复两条路径、CLI `profile delete` 归档可见、通知 `${ENV_VAR}`、工作区边界）与 `docs/GettingStarted.md`（管理台写面入口链接，不复制流程）
- [x] T067A 扩展 `internal/app/baseline.go` 的 `verifyOperation` 分派到本 US 全部动作组（`agent.*`、`model_connection.*`、`mcp_server.*`、`skill.*`、`notify_channel.*`；`agent.generate_files` 无操作行除外），按动作查证 `applied` / `not_applied` / `indeterminate`，补偿失败标 `unknown`、**不自动补做**；在 `internal/app/agent_admin_test.go` / `resource_admin_test.go` 增加 `TestRecoverAdmin_ActionMatrix`、`TestRecoverTest_UnknownNotResent`（测试动作不重发）与删除 / 发布 / 原名恢复崩溃窗口用例；`check: 012-app-reconcile`
- [x] T067B 补齐 `012-race` 的 12 个精确并发用例（分散在既有新增测试文件，不新增文件）：`TestResourceAdmin_ConcurrentPublish`、`TestResourceAdmin_DeleteDuringPublish`、`TestAgentAdmin_ConcurrentCreate`、`TestAgentArchive_ConcurrentRun`、`TestAgentRestore_ConcurrentCopy`、`TestMCPSupervisor_ConcurrentRestart`、`TestReconcile_ConcurrentStartup`、`TestNotifyAdmin_ConcurrentDelete`、`TestResourceStore_ConcurrentVersionNo`、`TestYAMLBlock_ConcurrentReaders`、`TestAdminResources_ConcurrentIfMatch`、`TestWorkspaceBrowse_ConcurrentRead`；`check: 012-race`
- [x] T067C 前端构建产物同步（本机无 Node，在 CI Node job 执行 `npm ci` → `npm test` → `npm run build`）：更新 `assets/console/dist` 与 `integrity.json`，校验 path 集合一致、逐文件 sha256 相等且 `git status --porcelain -- assets/console/dist` 为空（`??` 也算不一致）；`make check` / stage 不调用 Node
- [x] T068 派生 `harness/stages/us12-agent-resource-management.json`：十八条 check（模块卡 §4.4，含 T067A 的 `TestRecoverAdmin_*` / `TestRecoverTest_*` 与 T067B 的 12 个并发用例）、`required_tests` 按 `go test -list` 精确展开、`protect` 逐文件枚举（20 修改生产 + `internal/app/baseline.go` + 20 既有测试 + 28 新增测试 + 四份契约 + `go.mod` / `go.sum` / `Makefile` / `harness/acceptance.json` + 11 个脚本文件）、`inputs` = 模块卡 / `spec.md` / `plan.md` / `tasks.md`；**不进** `current.json`、不进 CI
- [x] T069 运行 `GOPROXY=https://goproxy.cn make check` 七目标，逐目标记录四态读数；修复失败（只改实现，不放宽判据）
- [x] T070 运行 `sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json`（Windows `.\scripts\check-acceptance.ps1`），逐 check 记录读数与 `required_tests` 命中
- [x] T071 按 `quickstart.md` §5 执行 Demo A–I 并留证（不含凭证）；结果写入本 US 验收报告，未执行项标"未验证"
- [x] T072 按模块卡 R3.3 修订 `TestResourceAdmin_PublishFailureKeepsOldPointer`：前置改为已发布内容不同的 v1、v2 且指针 / 运行时在 v2；显式撤回 v1（当前 v2 ETag）并注入版本插入失败；断言注入确已触发、AdminError + `operation_id`、文件恢复为操作前字节、published / applied 仍为 v2、运行时同一 v2 客户端与默认模型、无残留新版本、操作行 failed；幂等覆盖保留在 `TestPublish_Idempotent` / `TestPublish_SkillIdempotent`；`check: 012-app-resource-admin`

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，可立即开始
- **Foundational (Phase 2)**: 依赖 Setup；**阻塞全部用户故事**
- **User Stories (Phase 3–11)**: 依赖 Foundational；按优先级 US1 → US9 顺序执行（本仓单 Agent 串行）
- **Polish (Phase 12)**: 依赖全部用户故事完成

### User Story Dependencies

- **US1 (P1)**: 依赖 Foundational；无其他故事依赖
- **US2 (P1)**: 依赖 Foundational；发布目录供 US3 消费
- **US3 (P1)**: 依赖 US2（发布事实）与 Foundational（Provider 目录）
- **US4 (P2)**: 依赖 US1（Agent 目录原语）与 Foundational（归档存储）
- **US5 (P2)**: 依赖 US2（资源写面）与 Foundational（存储）
- **US6 (P2)**: 依赖 US2（发布）与 Foundational（`core.MCPServerConfig` 扩展）
- **US7 (P3)**: 依赖 US2（资源发布）与 Foundational（`workspace/skill.go` 映射）
- **US8 (P3)**: 依赖 Foundational（存储、扫描原语）；与 US2–US7 独立
- **US9 (P3)**: 依赖 Foundational（端口 / 路由）；与 US2–US8 独立

### Within Each User Story

- 测试与实现同任务或先行（模块卡 harness 先行口径）；关键回归测试名逐字落地，其余测试函数名逐字遵循模块卡 §4.4 族名（如 `TestAuthorService_*`、`TestAgentCopy_*`、`TestStartupOrder_*`）
- 模型 / 类型 → 服务 → handler → 接线
- 同一文件跨故事复用的任务不加 `[P]`（如 `internal/app/resource_admin.go` 在 US2 / US5 / US7 分阶段补齐）

### Parallel Opportunities

- Setup 的 `[P]` 任务可并行（不同文件）
- Foundational 的 `[P]` 任务可并行（YAML 原语、存储、ETag / 视图 / 路由互不依赖）
- 故事阶段内标 `[P]` 的前端任务与 Go 任务可并行（不同技术栈、不同目录）
- **不得**并行修改同一文件；`internal/app/wire.go` 的接线按故事顺序追加

---

## Parallel Example: User Story 1

```text
# 可并行：前端三件（不同文件）
Task: "api-admin.ts + agents-new.ts + agent-files.ts + agent-files.test.ts"

# 可并行：workspace 原语与 app 作者服务（不同包）
Task: "workspace/agent_draft.go + agent_draft_test.go"
Task: "app/author.go + author_test.go"
```

---

## Implementation Strategy

### MVP First (User Story 1)

1. 完成 Phase 1 Setup
2. 完成 Phase 2 Foundational（阻塞全部故事）
3. 完成 Phase 3 US1
4. **停下验证**：草稿 / 创建 / 提交 / 启用链路独立验收
5. 再进入 US2 / US3

### Incremental Delivery

1. Setup + Foundational → 底座就绪
2. US1 → 独立验证（MVP）
3. US2 → 独立验证（发布 / 失败保旧）
4. US3 → 独立验证（准入 / 对账 / 启动顺序）
5. US4–US5 → 独立验证（归档恢复 / 引用删除）
6. US6–US9 → 独立验证（MCP / 技能 / 通知 / 工作区）
7. Polish → 全量门禁、stage、Demo A–I
8. Phase 13 → 页面操作闭环（Demo J）独立验证

### 判据面纪律

- 允许的既有测试适配仅限模块卡 §3.5 的九项；其余删除断言 / `t.Skip` / 放宽阈值 / 改 `-run` 选择面 → **停下确认**
- 每个写路径任务必须包含"失败零写入"断言：目标文件 / 目录字节快照 + `management_operations` 行数
- 412 用例断言零操作行、响应无 `operation_id`；409 / 400 / 500 / 503 / 504 在 pending 之后必须带可查 `operation_id`

---

## Notes

- `[P]` = 不同文件、无未完成依赖
- `[Story]` 标签映射到 spec 的用户故事，便于追溯
- 零新增 Go 坐标、零迁移：任何需要新坐标或新表的发现 → 停下报告
- 前端构建与 Vitest 只在 CI Node job 执行；`make check` 与 stage **不得**调用 Node / npm
- commit / push / tag 归用户，本阶段不自动执行

---

## Phase 13: 页面操作闭环（R3.4 增量，2026-10-22 用户裁决）

**Purpose**: 后端端点已交付但页面无法完成新建 / 复制 / 归档 / 恢复时，产品交付仍有缺口。本阶段把入口与操作路径补进 US-11 交付的 Agents 列表页与 Agent 详情页，并补齐操作路径测试；不新增路由、API、依赖或正式契约修订。

**独立验收**: `npm test` 的 `agent-actions.test.ts` 8 条全绿；浏览器从列表新建 → 复制 → 归档 → 恢复走通；模块卡 Demo J 留证。

- [x] T073 修改 `assets/console/src/views/agents.ts`：页头追加「新建 Agent」（主操作）与「查看已归档」，空状态提供同一新建入口，归档跳转后以 `?archived=1` 显示到达说明
- [x] T074 修改 `assets/console/src/views/agent-detail.ts`：页头「复制配置」对话框（新名称输入、默认停用与清空定时提示、成功进入新 Agent 详情 `?copied=1`）；页尾危险操作区「归档 Agent」（逐字确认文案、409 `AGENT_BUSY` 点名 `data.run_ids` 并刷新状态、网络错误不自动重试、执行期间禁用重复提交）
- [x] T075 修改 `assets/console/src/views/archived-agents.ts`：恢复成功后进入恢复后的 Agent 详情（`?restored=1`），由详情页显示停用徽标与到达说明；失败保持原页面并显示服务端文案
- [x] T076 修改 `assets/console/src/styles.css`：追加 `.page-actions`、`.card.danger-zone`、`.empty button` 最小样式；沿用既有 Basalt 设计，不新增依赖
- [x] T077 [P] 新建 `assets/console/tests/agent-actions.test.ts`：入口可达 / 空状态入口 / 复制提交与跳转 / 取消零请求 / 归档成功跳转并重读 / 409 点名 Run 并刷新 / 重复点击只发一次 / 恢复落点停用（8 条，全部经真实视图与路由）
- [x] T078 同步模块卡：§3.1.10 页面—路由表与目录树、文件计数、§4.2 R38、§6.1 第 25 项、§6.2 Demo J、R3.4 修订记录；US-11 视图修改记为 US-12 增量
- [x] T079 更新 `specs/012-agent-resource-management/run-state.md` 与 `harness/stages/us12-agent-resource-management.json` 审批文本；记录前端测试、构建、dist / integrity 核对、`CGO_ENABLED=0` 二进制、stage 复跑与 Demo J 浏览器走查证据
- [x] T080 在 `assets/console/tests/agent-actions.test.ts` 补注册表滞后回归：复制到达页在注册表尚未刷新时不得显示「未找到」，随后有界重试读取到新 Agent（该文件共 9 条）；`agent-detail.ts` 同步改为重试期间显示「正在确认新 Agent 的注册状态…」（约 5 秒、20 × 250ms）

---

## Phase 14: 详情页操作路径（R3.5 增量，2026-10-22 用户实测六项）

**Purpose**: 后端与资源写面已交付，但 Agent 详情页缺少会话 / 能力 / 定时任务的可见操作路径，资源详情页与新建页存在误导文案。本阶段按用户实测清单补齐入口与交互，不新增路由、API、依赖、正式契约或迁移；工具为内置注册表，只提供绑定 / 解绑。

**独立验收**: `npm test` 的 `agent-operations.test.ts` 7 条全绿；`npm run typecheck` / `npm run build` 通过；dist 与 `integrity.json` 哈希一致；本机 demo 实例走通「新建会话 → 发送 → 归档」并可对 demo-agent 观察记忆、技能与定时任务。

- [x] T081 修改 `assets/console/src/views/agent-detail.ts`：会话页签新建 / 发送并运行 / 归档（确认框；409 `SESSION_BUSY` 不重试；归档后清空详情并重读列表）
- [x] T082 修改 `assets/console/src/views/agent-detail.ts`：归档会话后「新建会话」收到 `archived` 摘要时换 `console-<UTC 时间戳>` 用户标识再建一次，横幅说明归档记录仍保留（服务端不复活已归档行）
- [x] T083 修改 `assets/console/src/views/agent-detail.ts`：能力页签工具 / 技能勾选与保存（新鲜 ETag + `changes.tools / changes.skills`；目录外旧绑定保留）
- [x] T084 修改 `assets/console/src/views/agent-detail.ts`：定时页签新建 / 编辑 / 启停 / 立即运行 / 删除，写完重读列表与运行记录
- [x] T085 修改 `assets/console/src/views/resources.ts` / `views/resource-detail.ts` / `components/resource-form.ts`：资源新建 / 目录导入入口可达；技能文件改版带详情 ETag；MCP 整条替换的环境变量闸门；模型连接编辑
- [x] T086 修改 `assets/console/src/components/notify-channels.ts`：点击「编辑」滚动到表单并显示编辑横幅，消除长列表下的“无反应”误判
- [x] T087 修改 `assets/console/src/views/agents-new.ts`：模板大小显示「`<n>` 字节」；创建后注册表有界重试（20 × 250ms）期间显示「正在等待」，不误报未找到
- [x] T088 [P] 新建 `assets/console/tests/agent-operations.test.ts`：会话 / 归档后重开 / 能力绑定 / 定时 CRUD / 技能改版 / MCP env 闸门（7 条，全部经真实视图与路由）
- [x] T089 同步模块卡：§3.1.10 页面操作路径与测试树、§6.1 第 26 项、§6.2 Demo K、R3.5 修订记录；US-11 视图修改继续记为 US-12 增量
- [x] T090 更新 `specs/012-agent-resource-management/run-state.md`：记录 `npm test` 94/94、typecheck / build、dist / integrity 核对、`CGO_ENABLED=0` 二进制、`go test ./internal/web -run 'Console|Integrity'`、demo 实例实测与 8080 / 8099 交还状态

---

## Phase 15: Agent 通知渠道声明与渠道状态（R3.6 增量，2026-10-23 用户人工验收三问）

**Purpose**: 用户自建 Agent 的运行失败（模板占位模型名）、Agent 页面无法选择可用渠道、`${ENV_VAR}` 渠道被误显示为「地址为空」三个问题。本阶段在已审范围内补齐 `AGENT.md` 的 `notify_channels` 声明（空 = 不限制、非空 = 准入冻结白名单）与渠道视图的配置 / 引用状态；保留调用时显式指定渠道，不引入默认渠道或自动发送；零新增路由、依赖与迁移，绑定复用 `PUT /agents/{name}/content`。

**独立验收**: `go test ./internal/core ./internal/workspace ./internal/web ./internal/tool ./internal/app` 全绿；`npm test` 96/96、`npm run typecheck` / `npm run build` 通过且 dist 与 `integrity.json` 一致；stage 的 notify / workspace / core 检查包含新增 `required_tests` 并全绿；页面三态与文档引用列可人工复核。

- [x] T091 修改 `internal/workspace/agent.go`：解析 `AGENT.md` 的 `notify_channels`（空白元素 WARN `invalid notify_channels`；未登记渠道名保持可加载）；补 `agent_test.go` 的解析 / 空白拒绝 / 未登记可加载三条用例
- [x] T092 修改 `internal/workspace/content_edit.go`：新增 `changes.notify_channels` 块补丁（nil = 不改、`[]` = 清空），只改目标块；补 `TestApplyChanges_NotifyChannelsPatchOnly`
- [x] T093 修改 `internal/core/profile.go` / `internal/core/run.go` / `internal/core/service.go`：`Profile.NotifyChannels` 在 Run 准入时冻结进 `RunEnv` 并由 `ToolEnv` 携带；补 `TestAgentService_Resources_ToolEnvCarriesFrozenNotifyChannels`
- [x] T094 修改 `internal/tool/notify.go`：发送前按冻结声明收紧注册表（声明外 `not_found` 且点名 `notify_channels` 限制，不写「不存在」；声明均未登记给专门文案；空声明不限制）；补三条工具用例
- [x] T095 修改 `internal/web/views.go` / `handlers_notify_read.go` / `handlers_admin_notify.go` / `server.go`：`NotifyChannelView` 增 `url_configured` / `referenced_by`；引用端口读取失败列表固定 500 失败关闭，写成功后扫描失败只降级空引用并记 WARN；补读取与写面各两条用例
- [x] T096 修改 `internal/app/web.go`：装配 `NotifyReferences` 端口（与删除闸门共用 `ScanAgentDocuments` / `notifyReferenceHits`，不建第二套扫描）
- [x] T097 修改 `assets/console/src/components/notify-channels.ts` / `views/workspace.ts` / `app.ts`：目标服务三态（host / 环境变量引用 / 未配置）、「文档引用的 Agent」列与扫描说明、页面命名统一为「通知渠道」；补 `notify-channels.test.ts` 三态与引用断言
- [x] T098 修改 `assets/console/src/views/agent-detail.ts` / `agents-new.ts`：能力页签通知渠道多选并保存 `notify_channels`（未改动时不重写）、全局渠道页链接；模板占位模型名在详情页告警、新建页说明留空后果；补 `agent-operations.test.ts` 绑定与占位告警用例
- [x] T099 同步正式契约：`docs/contracts/openapi.yaml`（`url_configured` / `referenced_by` / `notify_channels` 补丁字段）与 `docs/contracts/agent-md.md`（声明行、校验表第 9 条、可用范围）
- [x] T100 同步文档：`docs/Operations.md` §9 通知渠道行（三态、文档引用非绑定、白名单语义）；模块卡 R3.6 修订记录、§4.2 R39、§6.1 第 27 项
- [x] T101 更新 `harness/stages/us12-agent-resource-management.json`：approval 补 R3.6 说明；protect 补本轮触及的 Go 生产 / 测试文件；`012-workspace-agent`、`012-core-resource-types`、`012-app-notify-author`、`012-web-admin-notify-workspace` 的 `-run` 与 `required_tests` 追加新用例（只新增锚点，不改既有断言）
- [x] T102 复跑验收并记 `run-state.md` §1.76：受影响 Go 包、`npm test` / typecheck / build、`make check` 七目标与 stage 全量读数

## Phase 16: R4 范围修订（身份 / 范围 / 绑定 / 托管凭据 / 迁移 0003，2026-10-23 用户批准）

**Purpose**: 补齐四类结构性缺口——Agent 与公共资源的稳定身份（同名重建不复用旧引用）、资源使用范围（共享 / 专属）、`AGENT.md` 内的绑定权威与可重建投影、通知模式三态与托管凭据（AES-256-GCM，主密钥在工作区之外）。落在 schema v3（26 张表）与迁移 `0003_credentials_scope.sql`；既有 105 个任务与勾选不动，本阶段从 T103 起追加。

**独立验收**: `012-core-identity`、`012-core-scope`、`012-workspace-bindings`、`012-storage-identity`、`012-migration-v3`、`012-app-binding`、`012-app-scope`、`012-app-notify-mode`、`012-credential-managed`、`012-credential-leak-sentinel`、`012-web-cfg-structured`、`012-web-credential`、`012-console-config`、`012-e2e-v3` 十四条 check 全绿；R3 十八条检查只增断言不改既有；`make check` 七目标 + `sh scripts/check-acceptance.sh` 全量通过，旧证据目录不被覆盖。

### B1 领域类型（core）

- [x] T103 新增 `internal/core/identity.go`：`AgentUID` / `ResourceUID` 字符串类型、`ag_` / `rs_` 前缀、26 位 Crockford Base32 载荷（拒绝 `I` / `L` / `O` / `U`）、生成与校验（长度、前缀、字符集、大小写不敏感解码）、`NewAgentUID` / `NewResourceUID` 去重；新增 `internal/core/identity_test.go`（`TestIdentity_Format`、`TestIdentity_RejectsConfusableChars`、`TestIdentity_UniqueGeneration`）
- [x] T104 新增 `internal/core/scope.go`：`ResourceScope` 枚举（`shared` / `exclusive`，拒绝其它字面量；命名为 `ResourceScope` 以免与 `memory.go` 已有的 `Scope`（记忆分层）重名）、所有权 `OwnerAgentUID`、资格判定 `Eligible(scope, owner, caller)` = 共享 ∪ 本人专属、`RESOURCE_SCOPE_IN_USE` 判定所需输入（占用 Agent 列表 + 未释放 Run 列表 + 消息路由）；新增 `internal/core/scope_test.go`（`TestScope_Eligibility`、`TestScope_ExclusiveOwnerOnly`、`TestScope_InvalidLiteral`）
- [x] T105 新增 `internal/core/binding.go`：`ResourceBindings` 领域模型（`model_connection` / `notify_channel` 两组 `{name, uid}` 条目）、与 `providers[].name` / `notify_channels` 的一一对应校验、未知键 / 重复 name / 重复 uid / 空值 / 前缀不符拒绝；新增 `internal/core/binding_test.go`（`TestBinding_RoundTripShape`、`TestBinding_RejectsDuplicateName`、`TestBinding_RejectsUnknownKind`）
- [x] T106 新增 `internal/core/credential.go`：`CredentialMode`（`env` / `managed`）、身份状态（`active` / `revoked` / `retired`）、版本状态（`active` / `superseded` / `revoked` / `retired`）及其合法迁移表、`CredentialRef` 只读传输类型（`cred_...` 前缀 + 尾号）、`TargetBinding` 输入契约；新增 `internal/core/credential_test.go`（`TestCredential_ModeLiterals`、`TestCredential_VersionStateMachine`、`TestCredential_RevocationIsTerminal`）
- [x] T107 修改 `internal/core/errors.go` / `internal/core/resources.go` / `internal/core/run.go` / `internal/core/service.go`：新增 12 个哨兵错误与 `notify_disabled` 工具 kind；`resources.go` 的 `RunResourceManifest` 升 `format_version: 2`（`agent_uid`、`ProviderVersionRef` 的 `resource_uid` / `model` / `credential_ref` / `credential_version` / `target_binding`、`notify.mode` + 具体渠道 UID），`run.go` 补 `RunEnv.AgentUID` / `ToolEnv.AgentUID` / `NotifyMode` 与通知渠道 UID 语义；范围 / 绑定 / 凭据端口接口；补 `resources_test.go` / `service_test.go` 的 manifest 断言（`internal/core/run_test.go` 不存在）

### B2 文件层（workspace）

- [x] T108 新增 `internal/workspace/bindings.go`：`resource_bindings` 块解析、序列化、块编辑（块外字节逐字保留、无块返回空结构、块缺失时有安全插入前提）；新增 `internal/workspace/bindings_test.go`（`TestBindings_ParseSerializeRoundTripBytes`、`TestBindings_EditPreservesOutsideBytes`、`TestBindings_RejectsMalformedBlock`）
- [x] T109 新增 `internal/workspace/identity_state.go`：生命周期状态文件 v2 读写（契约字段：`schema_version` / `agent` / `agent_uid` / `state` / `generation` / `updated_at` / `operation_id` / `checksum`；`uid_source` 是数据库镜像列，**不**写进状态文件）、v1 → v2 原子升级（先备份、临时文件 + rename、checksum 重算、失败回滚）；新增 `internal/workspace/identity_state_test.go`（`TestIdentityState_UpgradeV1ToV2`、`TestIdentityState_ChecksumRecomputed`、`TestIdentityState_UpgradeFailureKeepsV1`）
- [x] T110 修改 `internal/workspace/agent.go` / `content_edit.go` / `lifecycle.go`：解析并暴露 `resource_bindings` 与 `agent_uid`；`changes.bindings` 数组补丁（契约 agent-md §2.3：`[{resource_kind, ref_name, resource_uid}]`；nil = 不改、数组 = 整体替换）；归档 / 恢复保留 UID；补 `agent_test.go` / `content_edit_test.go` / `lifecycle_test.go` 既有用例

### B3 存储层与迁移 0003（storage）

- [x] T111 修改 `internal/storage/schema.sql`：26 表 DDL（8 张新表 + `resource_pointers` / `resource_versions` / `run_resource_refs` / `notify_channels` / `agent_lifecycle` / `agent_archives` 改列）、`ux_notify_channels_active_name` 部分唯一索引、`uid_source` CHECK 约束、迁移纪律注释；同步 `docs/contracts/schema.sql` 并补 `schema_contract_test.go`
- [x] T112 新增 `internal/storage/migrations/0003_credentials_scope.sql`：单事务 DDL + UID 回填 + `resource_versions` 重建（保留 `id`）+ `notify_channels` 重建（PK 换 `resource_uid`）+ **先** `agent_lifecycle` / `agent_archives` 加列 `agent_uid` / `uid_source`（契约 3g）**再**回填（2c 的回填不得早于加列；`uid_source` 逐表取值：lifecycle = `state_file` / `legacy_unrecorded`，archives = `archive` / `legacy_unrecorded`）+ `schema_version(3, <now>, 'credentials_scope')`
- [x] T113 新增 `internal/storage/identity_store.go`：`resource_identities` 一次性写入、删除置 `deleted` 不物理删除、按 UID 查名称快照、按 (kind, uid) 分配版本号；新增 `identity_store_test.go`（`TestIdentityStore_Tombstone`、`TestIdentityStore_VersionScopeByUID`、`TestIdentityStore_SameNameRebuildStartsAtOne`）
- [x] T114 新增 `internal/storage/binding_store.go`：`agent_resource_bindings` 投影全量重建（单事务删增）、按 Agent 读取、按资源反查引用者；新增 `binding_store_test.go`（`TestBindingStore_RebuildIsIdempotent`、`TestBindingStore_ReverseLookup`、`TestBindingStore_RebuildReplacesStaleRows`）
- [x] T115 新增 `internal/storage/credential_store.go`：`credentials` / `credential_versions` / `resource_version_credentials` 的 SQL、版本号递增、撤销清空密文并升 `revocation_epoch`、状态迁移；新增 `credential_store_test.go`（`TestCredentialStore_VersionAppendOnly`、`TestCredentialStore_RevokeClearsCiphertext`、`TestCredentialStore_NeverRevives`）
- [x] T116 新增 `internal/storage/run_credential_store.go` / `notify_delivery_store.go`：`run_credential_refs` / `run_notify_refs` 冻结写入与释放、`notify_deliveries` 以 `tool_invocation_id` 为主键的一对一记录；新增 `run_credential_store_test.go` / `notify_delivery_store_test.go`（`TestRunCredentialStore_FreezeAndRelease`、`TestNotifyDeliveryStore_OnePerInvocation`）
- [x] T117 修改 `internal/storage/migrate.go`：`SupportedSchemaVersion` 2 → 3、`0003_credentials_scope.sql` 增量登记、缺文件硬拒绝、单版本单事务；补 `migrate_test.go`，新增 `migration_v3_test.go`（`TestMigrationV3_ForwardFromV2`、`TestMigrationV3_InterruptedRefusesStartup`、`TestMigrationV3_PublishedFilesUnchanged`）
- [x] T118 修改 `internal/storage/resource_store.go` / `run_store.go` / `notify_admin_store.go` / `notify_store.go`：`resource_uid` 列、墓碑过滤（默认活跃视图）、范围 / 所有者读取、按 UID 查版本；`run_resource_refs` 插入 `resource_uid` / `resource_name`；通知渠道写 / 读按 UID、`scope`、`operation_id`、凭据列重写（主键已换 `resource_uid`，按名称更新会命中同名墓碑行）；补 `resource_store_test.go` / `run_store_test.go` / `run_query_test.go` / `notify_admin_store_test.go` 既有用例并新增 UID、墓碑与引用断言
- [x] T119 新增 `internal/app/migrate_v3.go`：六步编排（`system.baseline_v3` 预写 → 状态文件 v1→v2 → SQL 迁移 → 绑定回填写入 `AGENT.md` → 守卫 → 报告），失败分离（DB 回滚 / 文件不回滚 + 启动中止点名恢复），迁移不读主密钥、不按名猜 UID；新增 `internal/app/migrate_v3_test.go`（`TestMigrateV3_StepOrder`、`TestMigrateV3_BaselinePendingFailureWritesNothing`、`TestMigrateV3_BindingBackfillProofRule`）

### B4 应用层（绑定 / 范围 / 通知三态）

- [x] T120 新增 `internal/app/bindings.go`：按 `AGENT.md` 全量重建绑定投影（多余删、缺失补、UID 以文件为准）、归档 Agent 参与、`projection_error` 时闸门 fail-closed、准入点名缺失 / 非法 / 不可用；新增 `internal/app/bindings_test.go`（`TestBindingProjection_RebuildsFromAgentMD`、`TestBindingProjection_ErrorFailsClosed`、`TestBindingAdmission_NamesMissingBinding`、`TestBindingProjection_ConcurrentRebuild`）
- [x] T121 新增 `internal/app/scope.go`：范围转换检查（被他人绑定 / 未释放 Run / 消息路由阻断 → `RESOURCE_SCOPE_IN_USE`）与无阻断时原子生效、失败零写入、按名不按 UID 的调用被拒；新增 `internal/app/scope_test.go`（`TestScopeConversion_BlockedByBinding`、`TestScopeConversion_BlockedByRun`、`TestScopeConversion_BlockedByRoute`、`TestScopeConversion_AppliesAtomically`、`TestScopeConversion_Concurrent`）
- [x] T122 新增 `internal/app/notify_mode.go`：三态解析（`none` / `all_eligible` / `allowlist`）、空数组 = `all_eligible`、兼容读取旧声明、`all_eligible` 冻结具体 UID 列表（不含 `'*'`）、删除闸门读取；新增 `internal/app/notify_mode_test.go`（`TestNotifyMode_ParsesThreeStates`、`TestNotifyMode_EmptyArrayIsAllEligible`、`TestNotifyMode_NoneDisablesNotify`、`TestNotifyMode_FreezesConcreteUIDs`、`TestNotifyMode_MidRunChangeKeepsFrozen`）
- [x] T123 修改 `internal/app/resources.go`：条件 ETag 组成（内容 + UID + revision + scope + 凭据状态）、墓碑与同名重建、范围与凭据字段写语义（write-only `api_key` / `url`）；补 `resources_test.go`（`TestResources_EtagIncludesScopeAndCredential`、`TestResources_TombstoneOnDelete`）
- [x] T124 修改 `internal/app/run.go`：准入冻结 `agent_uid` 与资源 UID、绑定校验、`CredentialGuard` 检查点、冻结通知 UID 列表；补 `run_test.go`（`TestRun_AdmissionRejectsUnbound`、`TestRun_FrozenNotifyUIDsUnaffectedByMidRunChange`）
- [x] T125 修改 `internal/app/notify.go` / `internal/tool/notify.go`：按 UID 解析渠道、`none` 返回 `notify_disabled` 且不投递、`allowlist` 声明外拒绝并回填可用名、每次发送解析凭据；删除闸门按 `resource_uid` + 绑定 / 冻结集合 / `AGENT.md` 整词引用扫描（`?force=true` 仍执行同一扫描、扫描失败一律拒绝、`detail_json` 记 `forced`）；补 `notify_test.go` / `internal/tool/notify_test.go`（含 `TestNotifyDelete_ForceStillScans`、`TestNotifyDelete_NamesReferrers`）
- [x] T126 修改 `internal/app/ports.go`：新增 `CredentialStore` / `CredentialResolver` / `CredentialGuard` / `BindingStore` / `IdentityStore` / `ScopeService` / `Migrator` / `SecretsService` 端口；`ports_test.go` 的 `view.TableNames` 表数断言 18 → 26 张，新端口以编译期接口断言落地
- [x] T127 修改 `internal/app/baseline.go`：恢复分派扩展新动作组四证（凭据写入 / 撤销 / 范围转换）、启动对账后重建绑定投影、v3 编排接入启动顺序且 v2 基线语义保留；补 `recovery_operations_test.go` / `baseline_test.go`
- [x] T128 修改 `internal/app/agent_console.go` / `agent_admin.go`：创建 / 复制 / 恢复 / 归档动作后重建绑定投影与 UID 传递、复制 `resource_mapping` 与 `AGENT_COPY_BLOCKED`；补 `agent_console_test.go` / `agent_admin_test.go`

### B5 托管凭据与 CLI

- [x] T129 新增 `internal/app/credential_crypto.go`：AES-256-GCM 加解密、六段 AAD `basalt/v1|owner_kind|owner_uid|credential_id|version_no|target_binding`、`target_binding = SHA-256(canonical(base_url|full_url))`、主密钥文件校验（JSON 形态、32 字节、工作区外、权限）；新增 `internal/app/credential_crypto_test.go`（`TestCredentialCrypto_AADFieldChangeFails`、`TestCredentialCrypto_MasterKeyOutsideWorkspace`、`TestCredentialCrypto_KeyPermissions`）
- [x] T130 新增 `internal/app/credential_resolver.go`：按资源 UID 解析当前活跃版本、每次调用重读主密钥（不常驻缓存）、启动依赖仅「当前托管模型连接」、四个按需加载场景；新增 `internal/app/credential_resolver_test.go`（`TestCredentialResolver_OnDemandScenarios`、`TestCredentialResolver_StartupDependencyScope`）
- [x] T131 新增 `internal/app/credential_guard.go`：`CredentialGuard.Check(ctx, ref)` 调用检查点（`active` 可继续、`revoked` / `retired` / 版本不可解密 → 固定错误）、并发安全；新增 `internal/app/credential_guard_test.go`（`TestCredentialGuard_RejectsRevoked`、`TestCredentialGuard_ConcurrentCheck`）
- [x] T132 新增 `internal/app/credential_ops.go`：写入（新模式 → 新版本）、撤销（清密文 + `revocation_epoch+1` + 永不复活）、四证恢复三档（applied / not_applied / indeterminate）；新增 `internal/app/credential_ops_test.go`（`TestCredentialOps_RevokeIsTerminal`、`TestCredentialRecovery_FourEvidenceOutcomes`）
- [x] T133 修改 `internal/app/providers.go`：托管凭据来源接入显式映射（键仍来自已发布版本条目，不遍历推断）；补 `providers_test.go`（`TestProviders_ManagedCredentialSource`、`TestProviders_RevokedCredentialKeepsOldTable`）
- [x] T134 修改 `internal/app/wire.go`：装配 `CredentialResolver` / `CredentialGuard` / `Migrator` / `SecretsService`，启动顺序 = 迁移编排 → 状态文件 → 注册表 → MCP supervisor → 服务开放；补 `wire_test.go`
- [x] T135 新增 `internal/cli/secrets.go`：`init` / `status` / `verify` / `revoke-managed` 子命令（init 生成主密钥并设权限、不打印密钥；verify 只报格式与指纹）；新增 `internal/cli/secrets_test.go`
- [x] T136 修改 `internal/cli/config.go`：`secrets.master_key_file` 段解析与缺失 / 越界校验；补 `config_test.go`（`TestConfig_MasterKeyFileRequired`、`TestConfig_MasterKeyInsideWorkspaceRejected`）
- [x] T137 新增 `internal/app/credential_leak_test.go`：泄漏哨兵 golden 扫描（明文 Key / 完整 URL 不出现在响应体、`detail_json`、审计、manifest、`basalt.yaml`、日志、错误信息、前端产物）（`TestCredentialLeak_NoPlaintextInSurfaces`、`TestCredentialLeak_GoldenFixtures`）

### B6 Web 层

- [x] T138 新增 `internal/web/handlers_agent_config.go`：`GET /agents/{name}/config` 结构化视图（基本信息 / 正文 / 模型 / 工具 / 技能 / MCP / 通知 / 绑定 / 执行设置 / 定时摘要 / 原文入口），与 `GET /agents/{name}/content` 共用 `content_etag`，条件读；新增 `handlers_agent_config_test.go`（`TestAgentConfig_SharesContentETag`、`TestAgentConfig_RequiresBindingsOnContentSubmit`、`TestAgentConfig_DoesNotAutoFillBindings`）
- [x] T139 新增 `internal/web/handlers_credentials.go`：`POST /resources/{kind}/{name}/credential/revoke` 与 `POST /resources/{kind}/{name}/scope` 的动作字面量、条件写、错误映射；新增 `handlers_credentials_test.go`（`TestCredentialHandlers_ActionLiterals`、`TestCredentialHandlers_ErrorCodeMapping`、`TestCredentialHandlers_WriteOnlyFields`、`TestScopeHandlers_ConditionalWrite`）
- [x] T140 修改 `internal/web/envelope.go`：12 个错误码映射逐字（保存 400 / 准入 409 分段、`CREDENTIAL_UNAVAILABLE` 503）；补 `internal/web/contract_test.go` / `admin_routes_test.go` 的错误映射断言
- [x] T141 修改 `internal/web/handlers_resources.go` / `handlers_agent_content.go` / `handlers_admin_agents.go` / `handlers_notify_read.go` / `handlers_admin_notify.go`：write-only `api_key` / `url`、`credential_ref` 只读、绑定必填、墓碑与范围字段；补各 `*_test.go`
- [x] T142 修改 `internal/web/contract_test.go`：openapi 契约测试覆盖新端点 / 字段 / 错误码；复核 `docs/contracts/openapi.yaml` 与实现逐字一致

### B7 管理台前端

- [x] T143 修改 `assets/console/src/api.ts`：新端点封装（config / credential revoke / scope / copy mapping）与 12 个错误码文案；补 `api.test.ts`
- [x] T144 新增 `assets/console/src/components/credential-field.ts` / `scope-picker.ts` / `binding-list.ts`：write-only 密码输入 + 尾号 / 状态展示、范围选择、绑定列表；新增三份组件测试；保存成功后清空输入框 DOM 值、只保留状态与尾号
- [x] T145 新增 `assets/console/src/views/agent-config.ts`：结构化配置 + 绑定编辑（保存时显式提交 `resource_bindings`，不自动补绑定）；新增 `agent-config.test.ts`；保存成功后清空 Key 输入并重新读取配置，不得乐观更新
- [x] T146 修改 `assets/console/src/views/resource-detail.ts`（R3 / T029 已交付，不是新建）：版本 / 引用 / 范围 / 凭据卡片与撤销确认；新增 `assets/console/tests/resource-detail.test.ts`
- [x] T147 修改 `assets/console/src/views/agents.ts` / `resources.ts` / `workspace.ts` / `components/notify-channels.ts`：复制映射对话框、范围与凭据列、绑定跳转、通知模式与凭据（`views/notify.ts` 不存在，渠道组件是 `components/notify-channels.ts`）；补对应视图 / 组件测试
- [x] T148 修改 `assets/console/src/router.ts`：**只新增** `/console/agents/:name/config` 一条路由；资源详情复用既有 `/console/resources/:kind/:id`（`id` 即名称），不得并存同形的 `:name` 路由；补 `tests/router.test.ts`
- [x] T149 复跑前端门禁：`npm ci && npm test && npm run typecheck && npm run build`，校验 `assets/console/dist/integrity.json` 与产物一致、无危险 DOM（`v-html` / `innerHTML`）

### B8 收口

- [x] T150 同步文档：`docs/Operations.md`（主密钥、`secrets` 命令、迁移 0003 与回滚）、`docs/Compatibility.md`（31–44 条映射）、`docs/DevelopmentLoop.md` 陷阱表（如本轮有实测新增）、`basalt.yaml.example` 复核；模块卡 R4.17 验证状态更新。根目录 `README.md` 归 US-13，本 US 不改
- [x] T151 更新 `harness/stages/us12-agent-resource-management.json`：approval 追加 R4 段落（卡与 tasks 的 sha256 + 行数，按最终工件重算；R4 spec / plan 的哈希与行数一并记入，R3 的 368 / 208 行标注为历史值）、新增 14 条 check 及 `args` / `timeout_seconds` / `required_tests`、`protect` 追加 R4 触及的既有文件（`internal/core/resources.go`、`internal/core/resources_test.go`、`internal/storage/run_store.go`、`internal/storage/run_store_test.go`、`internal/storage/run_query_test.go`、`internal/storage/notify_admin_store.go`、`internal/storage/notify_store.go`、`internal/storage/migration_v2_test.go`、`internal/cli/status_test.go`、`internal/e2e/bootstrap_test.go`、`internal/web/handlers_runs_test.go`、`docs/DevelopmentLoop.md`）与 T153–T157 新建的 `internal/e2e/v3_test.go`、`internal/web/console_config_test.go` 等；既有 18 条检查名不删，`012-race` 的 `required_tests` 追加 3 条。**执行本任务前必须先完成 T153–T157**
- [x] T152 执行 Demo L–O、统一验收（`make check` 七目标 + `sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json`）并记 `run-state.md` §1.77；旧证据目录保留，收口报告列出判据面变更（`TableNames` 18→26 张、schema_version 2→3、manifest format_version 1→2、`012-race` +3 required_tests）

### B9 analyze 派生补充（T153–T157）

**执行顺序**：先完成 T153–T157，再执行 T151（protect 纳入新文件）与 T152（统一验收）。任务编号按追加顺序分配，文档位置在 B8 之后仅为保持既有编号不变。

- [x] T153 新增 `internal/e2e/v3_test.go`（`TestV3E2E_MigrateAdmitNotify`、`TestV3E2E_CredentialLifecycle`、`TestV3E2E_ScopeAndCopyMapping`）并修改 `internal/e2e/harness_test.go` 补齐 v3 fixture；`check: 012-e2e-v3`
- [x] T154 新增 `internal/web/console_config_test.go`（`TestConsoleConfig_NoDangerousDOM`、`TestConsoleConfig_ErrorCopyPresent`、`TestConsoleConfig_BackfillEvidenceCopy`，最后一条断言「引用未记录（迁移前无法证明身份）」文案）；同一 check 复跑既有 `TestConsoleDist_IntegrityMatchesFiles`（US-11，`internal/web/console_test.go`，产物与 `integrity.json` 一致）；`check: 012-console-config`
- [x] T155 补 `internal/app/resources_test.go`（`TestResources_EtagKeyOnlyChange`、`TestResources_KeyChangeDoesNotBreakOtherAgentSave`），证明只换 Key 使资源 ETag 变化、其他 Agent 正文保存不 412；`check: 012-credential-managed`
- [x] T156 补 `internal/cli/secrets_test.go`（`TestSecrets_InitRefusesWhenKeyExists`、`TestSecrets_InitRefusesWithManagedDependency`、`TestSecrets_VerifyReportsFingerprint`、`TestSecrets_RevokeManaged`）；`check: 012-credential-managed`（args 含 `./internal/cli`）
- [x] T157 补 `internal/app/credential_crypto_test.go` / `credential_ops_test.go`（`TestCredentialCrypto_NonceUniquePerKey`、`TestCredentialOps_PreWriteFailureWritesNothing`），落实宪法 v2.1.0 的 nonce 不重复与托管凭据预写失败零写入；`check: 012-credential-managed`

### B10 R4 review ② 页面缺口修复（T158–T165，2026-10-23 追加）

**来源**：用户 review ② 前的人工复核（六项缺口）。已交付任务与历史证据不动，本节只追加：页面接线、页面集成测试与重新验收。任务编号顺延，不改既有勾选。

- [x] T158 修改 `assets/console/src/views/agent-config.ts`：模型连接候选列出全部「已发布且对本 Agent 有资格」的连接（共享 + 本人专属）；用户显式选择后把连接名称与 UID 一并写入 `changes.bindings`（服务端同写边界重新校验，不按名称自动补绑）；就地新建连接 = 完整 Key 录入 + 范围 / 所有者 + 创建即发布 → 重新读取 → 保存绑定。派生列表改为按需函数（本环境下候选重读后 computed→computed 链未刷新，改为按需派生；见文件内注释）。补 `tests/agent-config.test.ts`（`creates an exclusive connection inline and binds it after publishing`、`keeps the inline-created connection and says it is not yet written when the save fails`、`creates a notify channel inline, adds it to the allowlist and binds it`、`requires at least one channel when switching to allowlist`）
- [x] T159 修改 `assets/console/src/components/resource-form.ts`：把托管凭据输入组件接到模型连接创建 / 编辑（完整 Key 录入、替换、状态 / 版本 / 尾号只读展示；env 为高级模式）；新建资源可选共享 / 专属并指定所有者；补 `tests/resource-form.test.ts`（12 条，含托管 / env 两种模式的请求体与 412 重取 ETag）
- [x] T160 修改 `assets/console/src/components/notify-channels.ts`：通知渠道创建 / 编辑接入同一凭据组件与范围选择；托管模式空地址拒绝并提示切 env，env 空地址允许（暂不可发送）；补 `tests/notify-channels.test.ts`（新增 8 条）
- [x] T161 修改 `assets/console/src/components/binding-list.ts`：只读绑定的「管理入口」列——模型连接打开资源详情（按当前名称）、通知渠道打开渠道管理、已删除 / 孤儿绑定不给入口；补 `tests/binding-list.test.ts`（新增 5 条只读用例）
- [x] T162 交叉核对「组件存在但页面未接线」：Agent 创建（`views/agents.ts`）不引入第二套凭据入口；复制映射（`views/agent-detail.ts`）按来源 UID + 目标 UID 显式提交，只列共享且已发布候选；通知渠道页与 Agent 内联创建共用同一组件与同一校验
- [x] T163 复跑前端门禁：`npm test`（18 文件 / 169 条全绿）、`npm run typecheck`、`npm run build`；重生成 `assets/console/dist` 与 `integrity.json`
- [x] T164 真实浏览器验证：选择／新建模型连接 → 保存绑定 → 启用 Agent → 通过 HTTP 跑通一次运行；另跑一次真实通知发送 Demo。读数与截图记入证据
- [x] T165 复跑统一验收（`make check` 七目标 + `sh scripts/check-acceptance.sh --stage harness/stages/us12-agent-resource-management.json`），`run-state.md` 只追加新证据；人工 review ② 继续待用户确认

### B11 R5 管理台体验统一与页面闭环（T166–T179，2026-10-23 追加）

**来源**：用户 2026-10-23 的九组要求（两个现场问题 + 页面闭环）。本节只追加，不改 T001–T165 的勾选与历史证据；T164 的「真实通知发送」仍未执行，保持未勾选。新增产品语义只在模块卡 R5.3 登记提案，待 review ①，本节不含其实施任务。

- [x] T166 修改 `internal/core/management.go` / `internal/workspace/content_edit.go` / `internal/web/handlers_agent_content.go`：结构化补丁支持正文整段替换（`body`）、`notify_mode`、`notify_channels` 与 `provider.temperature: null`（移除行，省略仍为不修改）；保存期资源绑定校验在目标写入前执行；补 `internal/workspace/content_edit_test.go` 与 `internal/web/handlers_agent_content_test.go`
- [x] T167 修改 `internal/storage/run_query.go`：历史 manifest 按**已存储字节**校验（`verifyManifestBytes`），不再把 v1 行解析进 v2 结构体后重算；补 `TestRunQuery_GetRunVerifiesLegacyV1ManifestBytes`、`TestRunQuery_TamperedManifestBytesStillRejected`；`check: 012-web-runs`（既有运行详情判据）
- [x] T168 修改 `assets/console/src/views/agent-detail.ts` / `views/agents.ts`：详情页「配置」页签内嵌与独立路由 `/console/agents/:name/config` 同一组件；列表卡片配置链接、详情页以「配置」页签为唯一入口；旧路由兼容；补 `tests/agent-actions.test.ts`、`tests/agent-operations.test.ts`
- [x] T169 修改 `assets/console/src/views/agent-config.ts`：七分区（基本信息 / 系统指令 / 模型 / 工具与技能 / MCP / 通知 / 高级设置）；模型连接搜索 + 就地新建；工具 / 技能 / MCP 目录候选与可用性标注；bootstrap 工作区文件选择；目录外绑定保留并解释；文本字段保持自由输入；补 `tests/agent-config.test.ts`、`tests/agent-operations.test.ts`
- [x] T170 修改 `assets/console/src/views/agent-config.ts`：温度「使用模型默认值 / 自定义 / 恢复默认」三态（提交 `null` / 数字 / 省略）；诊断信息显示 UID、ETag 与字段名；系统指令高度与角色提示区分；统一生效说明（下一次新运行 vs 重启）；补温度 3 条测试
- [x] T171 修改 `assets/console/src/views/agents.ts` / `views/agent-config.ts` / `views/agent-detail.ts`：通知缺口三处提示（列表卡片「已声明通知渠道，但未绑定 notify 工具」、配置页通知区、定时页签「工具列表里没有 notify」）；不自动补工具；补断言
- [x] T172 修改 `assets/console/src/views/agent-detail.ts`：归档入口移入「更多操作」并改名「移出工作区（可恢复）」；确认框写明保留项、恢复后默认停用、409 完整回滚；不做闲置自动归档；补 `tests/agent-operations.test.ts` / `tests/agent-actions.test.ts`
- [x] T173 修改 `assets/console/src/views/agent-detail.ts`：记忆页改为「常驻记忆 / 可检索记忆」，写明关键词召回、非自动抽取、空态来源、全局层独立且不回退叠加；补 `tests/agent-operations.test.ts`
- [x] T174 修改 `assets/console/src/views/agents.ts` / `views/agents-new.ts`：列表「一句话创建 Agent」入口只导航；创建页自动草稿、结构化预览、创建（默认停用）→ 提交文件 → 启用 → 详情；失败保留已创建身份并提示不要重复创建；补 `tests/agent-actions.test.ts`
- [x] T175 修改 `assets/console/src/views/agents-new.ts`：创建页加载「已发布且共享」的模型连接，选择后把连接名称与 UID 写入 `changes.bindings` 与 `provider`，再提交其余草稿文件（此前缺该步骤导致 `POST /files` 400「缺少绑定」）；补 `tests/agent-actions.test.ts`「binds the selected model connection when submitting the drafted files」「blocks draft submission without a published shared connection」
- [x] T176 复跑前端门禁：`npm test`（18 文件 / **184 条**全绿）、`npm run typecheck`、`npm run build`，重生成 `assets/console/dist` 与 `integrity.json`（`index-Fuk748Ra.js` / `index-CDmxpTG4.css`）
- [x] T177 无头 Edge 走查 11 步（登录、一句话入口、通知缺口卡片、创建预览、创建 / 提交 / 启用、试运行、配置页、定时提示、记忆文案、历史运行详情、归档与恢复）；证据 `.loop/demo-us12-r5/result.json` 与 `shots/01`–`12`
- [x] T178 复跑统一验收（`make check` 七目标 + stage 全量）并对改动文档跑 markdownlint；读数追加到 `run-state.md`；人工 review ② 继续待用户确认
- [x] T179 在模块卡登记新增范围提案（`display_name`、问答式补全、定时任务一句话、记忆自动抽取）的最小范围与契约影响，待 review ①；本轮不实施

### B12 R6 新增范围（T180–T194，2026-10-24 追加）

**来源**：模块卡 R6（review ① 裁决：`display_name` 采纳、问答式补全信息不足才提问、定时任务一句话生成由服务端计算三次触发）。本节只追加，不改 T001–T179 的勾选与历史证据；T164 的真实通知发送仍未执行，保持未勾选。测试随对应实现同步交付。

- [x] T180 修改 `internal/core/profile.go` / `internal/core/management.go` / `internal/workspace/agent.go`：`Profile.DisplayName`、frontmatter `display_name`（DTO / 已知键 / 映射）、`AgentContentChanges.DisplayName`；补 `internal/workspace/agent_test.go`（存在 / 缺省 / 空值 / 未知键仍只 WARN）
- [x] T181 修改 `internal/workspace/content_edit.go` / `internal/web/handlers_agent_content.go`：`changes.display_name` 三态（缺省不改 / 空串移除该行 / 非空写入，字段不存在时移除为空操作且 ETag 不变）；结构化写面拒绝控制字符与超过 64 字符的值；补 `internal/workspace/content_edit_test.go`、`internal/web/handlers_agent_content_test.go`
- [x] T182 修改 `internal/web/views.go` / `internal/web/views_admin.go` / `internal/app/bindings.go` / `docs/contracts/openapi.yaml`：`ProfileView.display_name` 解析值（空则回退技术名）、`AgentConfigView.fields.display_name` 原文值；同步契约字段集测试；补 `internal/web/views_test.go`
- [x] T183 修改 `docs/contracts/agent-md.md`：frontmatter 表新增 `display_name` 行、`changes` 已知键与三态语义；写明不进提示词、不改身份
- [x] T184 修改 `assets/console/src/api.ts` / `views/agents.ts` / `views/agent-config.ts` / `views/agent-detail.ts` / `workspace.css`：列表 / 详情标题 / 面包屑使用显示名且技术名同时可见；配置页新增显示名字段；补 `tests/agent-actions.test.ts`、`tests/agent-config.test.ts`
- [x] T185 修改 `internal/core/author.go`：问答类型与提示词分支（信息足够直接出文件、不足才提问、≤ 3 个、不索要凭据）；草稿解析支持 `{questions}` 与文件数组双形态；补 `internal/core/author_test.go`
- [x] T186 修改 `internal/app/author.go` / `internal/app/agent_admin.go` / `internal/app/web.go` / `internal/web/handlers_admin_agents.go` / `internal/web/views_admin.go` / `docs/contracts/openapi.yaml`：`generate-files` 接受 `answers` 并返回 `questions`；答案上限与严格解码；补 `internal/app/author_test.go`、`internal/app/agent_admin_test.go`、`internal/web/handlers_admin_agents_test.go`
- [x] T187 修改 `assets/console/src/api-admin.ts` / `views/agents-new.ts`：问答卡（逐项必填、继续生成、两轮上限、离开丢弃）；补 `tests/agent-actions.test.ts`
- [x] T188 修改 `internal/core/author.go` / `internal/workspace/agent.go` / `internal/app/author.go` / `internal/app/agent_admin.go` / `internal/app/web.go` / `internal/web/handlers_admin_agents.go` / `internal/web/views_admin.go` / `internal/web/admin_routes.go` / `docs/contracts/openapi.yaml`：`POST /agents/{name}/schedule-draft`、`ValidateScheduleCandidate`、`DraftSchedule` 与 `Occurrences` 未来三次；补 core / workspace / app / web 测试
- [x] T189 修改 `assets/console/src/views/agent-detail.ts` / `api-admin.ts` / `workspace.css`：定时页一句话输入 → 候选卡（Agent、时区、频率、消息、通知渠道、服务端三次触发）→ 确认并保存走既有条件写；id 冲突阻止；补 `tests/agent-operations.test.ts`
- [x] T190 校准 `.loop/demo-us12-r6/mock/main.go` 与 `run.mjs`：问题 / 候选分支按新提示词；新增显示名、问答、定时候选三步
- [x] T191 复跑前端门禁：`npm test`、`npm run typecheck`、`npm run build` + `emit-integrity.mjs`，重生成 `assets/console/dist`
- [x] T192 修改 `harness/stages/us12-agent-resource-management.json`：R6 行为作为**新增断言**并入既有 check（只增不减）；复核 stage 输入仍为公开路径
- [x] T193 核对新构建实际服务的资源哈希（`dist` 文件 / `integrity.json` / 服务端响应字节一致），不使用 8080 旧包；浏览器走查新增三步并复跑既有 13 步，证据追加 `.loop/demo-us12-r6/`
- [x] T194 复跑统一验收（`make check` 七目标 + stage 全量）并对改动文档跑 markdownlint；读数追加 `run-state.md`；人工 review ② 继续待用户确认

### B13 R7 九项问题修复与系统代投（T195–T210，2026-10-24 追加）

**来源**：用户 2026-10-24 人工验证九项问题、A–E 裁决与三项修正。本节只追加，不改 T001–T194 的勾选与历史证据；T164 的真实通知发送仍未执行，保持未勾选。系统代投只发成功结果，未知不重发；`notify` 是唯一允许自动增删的工具。

- [x] T195 新增 `internal/core/delivery.go`：`ScheduleDelivery` 端口、五值状态与意图 / 结果结构；修改 `internal/core/scheduler.go`：运行准入前预写投递、成功后结算（独立超时）、失败 / 取消记 `skipped`、`SuppressNotify` 接线；补 `internal/core/scheduler_delivery_test.go`
- [x] T196 新增 `internal/app/delivery.go` / `internal/storage/delivery_store.go` / `internal/storage/notify_delivery_store.go`：按 Agent UID 解析、渠道资格复核、预写与终态、启动恢复（滞留 `pending` → `unknown` / `skipped`）；补 `internal/app/delivery_test.go`、`internal/storage/notify_delivery_store_test.go`
- [x] T197 新增 `internal/tool/delivery.go`：复用通知发送面与凭据解析，成功判定含飞书 `code == 0`，未知与失败分开；补 `internal/tool/delivery_test.go`
- [x] T198 新增 `internal/app/notify_assembly.go`：`none` 移除、`all_eligible` / `allowlist` 资格通过装配、非法 / 不合格不改写；修改 `internal/core/service.go` / `internal/core/run.go`：`SuppressNotify` 兜底；保存响应返回 `changes.tools_added` / `tools_removed`；补 `internal/app/notify_assembly_test.go`
- [x] T199 修改 `internal/app/bindings.go`：`deliver_to` 保存时按 UID、声明范围与资格校验；未知字段与空串语义；补 `internal/app/bindings_test.go`
- [x] T200 修改 `internal/web/views.go` / `internal/web/handlers_schedules.go` / `internal/web/views_admin.go`：`delivery_status` / `delivery_channel` / `delivery_detail` 与执行状态分开投影，空白不冒充成功；补 `internal/web/handlers_schedules_test.go`
- [x] T201 新增 `internal/storage/connection_test_store.go` / `internal/app/notify_admin.go`：渠道测试固定文案 `Basalt 连通性测试`、先写操作行、三值结论与 `last_test` 投影；修改 `docs/contracts/openapi.yaml`；补 `internal/app/notify_admin_connectivity_test.go`、`internal/storage/connection_test_store_test.go`
- [x] T202 修改 `internal/app/author.go` / `internal/app/agent_admin.go` / `internal/web/handlers_admin_agents.go`：作者模型未配置 503、调用失败 500 且替换通用文案，底层原因只进日志；补 `internal/app/author_test.go`、`internal/web/handlers_admin_agents_test.go`
- [x] T203 修改 `docs/contracts/agent-md.md`（`schedules[].deliver_to`、§2.6 受控装配、§3.2 代投）、`docs/contracts/openapi.yaml`（`deliver_to`、`delivery_*`、`availability` / `last_test`、渠道测试、`changes`）、`docs/contracts/schema.sql` 与新增 `internal/storage/migrations/0004_schedule_delivery.sql`、`docs/contracts/run-and-errors.md`（`model_connection.test` / `notify_channel.test`）
- [x] T204 修改 `assets/console/src/router.ts` / 新增 `views/session-detail.ts` / 修改 `app.ts`：会话详情深链 `agents/:name/sessions/:id`、返回 / 刷新 / 继续对话；列表「查看」跳转；补 `tests/router.test.ts`、`tests/agent-operations.test.ts`
- [x] T205 修改 `assets/console/src/views/resources.ts` / `views/resource-detail.ts` / `components/resource-form.ts`：连接列表只留名称 / 默认模型 / 范围 / 可用性，`availability` 与 `last_test` 分开；用户与引用者移入详情；补 `tests/resource-list.test.ts`、`tests/resource-detail.test.ts`
- [x] T206 修改 `assets/console/src/views/agent-detail.ts`：记忆页签固定说明会话历史与长期记忆两层关系（`save_memory` 才写入、不自动抽取）；补 `tests/agent-operations.test.ts`
- [x] T207 修改 `assets/console/src/views/agent-detail.ts` / `components/notify-channels.ts` / `api-admin.ts`：定时任务投递渠道选择与保存前资格校验、执行 / 投递分列、渠道测试三值展示；补 `tests/agent-operations.test.ts`、`tests/notify-channels.test.ts`
- [x] T208 新增 `scripts/import-credentials.ps1`：读取仓库外用户自填模板，经现有管理 API 更新既有 `deepseek` / `feishu-ops`，保留 `resource_uid`，幂等（尾号一致跳过、不一致且未 `-Rotate` 不覆盖），输出只含脱敏结果；`docs/Operations.md` 增加初始化导入与主密钥依赖说明
- [x] T209 新增 `scripts/redeploy-local.ps1`：识别 8080 监听 PID 并确认 `basalt serve`、构建、优雅停止（失败才强制并记录）、保留工作区 / 数据库 / 配置 / 主密钥、启动新构建、校验健康 / 管理认证 / 前端资源哈希；`docs/Operations.md` 增加重部署步骤
- [x] T210 8080 真实配置验收：执行初始化导入、重部署、真实连接测试、一句话创建定时任务与会话详情浏览器走查；审计并归档 8 个走查 Agent、停用测试任务、移除 mock 连接（保留用户创建物）；修完发现后冻结输入，统一跑 `make check` 与 stage，证据写 `run-state.md`

### B14 R8 六项裁决修复与范围修订（T211–T220，2026-10-24 追加）

**来源**：用户 2026-10-24 六项正式裁决。本节只追加，不改 T001–T210 的勾选与历史证据；人工 review ② 继续待人工。

- [x] T211 新增 `internal/core/network_policy.go` / `internal/storage/network_policy_store.go` / `internal/storage/migrations/0005_network_domain_policy.sql`：单行权威策略、规整 / ETag / 修订号 CAS、种子幂等；补 `internal/storage/network_policy_store_test.go`
- [x] T212 新增 `internal/app/network_allowlist.go`：预写 `pending`、`network_allowlist.update` 审计、CAS 落库、原子热替换、MCP http 撤销与三态恢复（`baseline.go`）；补 `internal/app/network_allowlist_test.go`、`internal/app/network_allowlist_recovery_test.go`
- [x] T213 修改 `internal/tool/policy.go` / `internal/web/handlers_sandbox.go` / `internal/web/views.go` / `docs/contracts/openapi.yaml` / `docs/contracts/run-and-errors.md` / `docs/contracts/schema.sql`：`PUT /sandbox/network-domains`、生效元数据、动作 / 目标类型 / 恢复行；补 `internal/tool/policy_hot_test.go`、`internal/web/handlers_sandbox_write_test.go`
- [x] T214 新增 `internal/web/errors_zh.go`：类型化 reason → 中文映射；执行历史安全中文投影；既有中文日志清单 `docs/us/us12-r8-chinese-log-inventory.md`；补 `internal/web/errors_zh_test.go`
- [x] T215 修改 `assets/console/src/views/workspace.ts` / `workspace.css`：执行环境白名单编辑器（保存、412 重读、409、失败保留输入、实际生效状态）；补 `assets/console/tests/sandbox-allowlist.test.ts`
- [x] T216 修改 `assets/console/src/api.ts` / 会话与工作区视图：可见性 / 焦点刷新、15–30s、去重、退避、定时器清理、滚动保留、最后更新时间、「最近使用」与来源标记；补 `assets/console/tests/live-refresh.test.ts`
- [x] T217 修改管理台展示文案（`agent-config.ts` / `resource-form.ts` / `credential-field.ts` / `notify-channels.ts` / `binding-list.ts` / `agent-detail.ts` / `agents-new.ts` / `resources.ts` / `login.ts` 等）与对应测试：术语归一、记忆说明、登录页身份说明；wire 字段 / 绑定 / 权限语义不变
- [x] T218 修改 `docs/TechnicalDesign.md` / `docs/Requirements.md` / `docs/Operations.md` / `docs/EngineeringStandards.md` / `docs/DevelopmentLoop.md` / `basalt.yaml.example`：权威源、热生效边界、错误语言分工、运维与陷阱表；`spec.md` / `plan.md` 追加 R8 节
- [x] T219 修改 `harness/stages/us12-agent-resource-management.json`：R8 行为作为**新增断言**并入既有 check（只增不减），并复核 `protect` 的新文件路径
- [x] T220 复跑 `npm test` / `npm run typecheck` / `npm run build`（重生成 `dist` 与 `integrity.json`）、`gofumpt` 触及文件、`go test ./internal/...`、`-race`（web / app / tool / storage）、`make check` 与 stage；读数追加 `run-state.md`；人工 review ② 继续待人工
