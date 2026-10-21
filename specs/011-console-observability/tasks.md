# Tasks: US-11 管理台与运行观测

**Input**: Design documents from `specs/011-console-observability/`
**Prerequisites**: [plan.md](plan.md)（必需）、[spec.md](spec.md)（必需）、[research.md](research.md)、[data-model.md](data-model.md)、[quickstart.md](quickstart.md)
**模块卡（人审 R3）**: [docs/us/us11-console-observability.md](../../docs/us/us11-console-observability.md) §3.1 / §3.2 / §3.3 / §4.4 / §6.2

**Tests**: 本 US 要求测试先行或伴随实现（模块卡 harness 先行口径）。测试函数名一律英文 `TestXxx_yyy`；模块卡中的中文用例名以注释保留在对应测试函数上方。

**Organization**: 任务按依赖顺序分七阶段；Phase 3 起每阶段对应 spec 的一个用户故事，可独立验收。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属用户故事（US1–US7），仅故事阶段使用
- 每个任务给出精确文件路径；`check:` 标注模块卡 §4.4 的专属检查

## Path Conventions

- 仓库根：`D:\code\basalt`；Go module `github.com/ymedword/basalt`
- Go 代码：`internal/<pkg>/`；前端工程：`assets/console/`；嵌入入口：`assets/embed.go`
- 迁移 SQL：`internal/storage/migrations/`
- Harness 工件：`harness/stages/`

---

## Phase 1: Setup（共享前置）

**Purpose**: 依赖、迁移、领域类型、配置、HTTP 骨架与前端工程脚手架；此阶段不交付任何用户故事。

- [x] T001 [P] 落地已批准依赖 `fsnotify v1.10.1`：修改 `go.mod` / `go.sum`（`GOPROXY=https://goproxy.cn go get github.com/fsnotify/fsnotify@v1.10.1 && go mod tidy`），核实 `go list -m github.com/fsnotify/fsnotify` 输出与 TechnicalDesign §1.3 一致
- [x] T002 [P] 新建迁移 `internal/storage/migrations/0002_us11_us12_resources.sql`（七张表 + 索引，只追加、幂等），修改 `internal/storage/migrate.go`（`SupportedSchemaVersion` 1 → 2，高版本拒绝启动）；测试 `internal/storage/migration_v2_test.go`（10 例：新库直达 v2 / 旧库前向迁移 / 重复迁移幂等 / 高版本拒绝 / 表与索引存在性）与 `internal/storage/migrate_test.go`（+4）；`check: 011-storage-migration`
- [x] T003 [P] 新建 `internal/core/management.go`：`ManagementOperation` 领域类型、四种 actor 字面量、`op-<unix_millis>-<6 alnum>` 生成与校验、状态与 `verification` 常量（字面量逐字取自 `docs/contracts/schema.sql` v2 段）
- [x] T004 [P] 修改 `internal/cli/config.go`：解析 `http.admin_auth`（`mode` / `token` / `allowed_hosts` / `allowed_origins` / `trusted_proxies`，键名逐字取自 `docs/contracts/run-and-errors.md` §9.1），启动期校验模式、凭据可解析性与明文拒绝；修改 `basalt.yaml.example`；测试 `internal/cli/config_test.go`（+8）
- [x] T005 [P] 修改 `internal/web/envelope.go`（新增固定错误码：`UNAUTHORIZED` / `FORBIDDEN` / `AUTH_RATE_LIMITED` / `AGENT_UNAVAILABLE` / `EDIT_UNSUPPORTED` / `PRECONDITION_FAILED` / `CONFLICT`，文案不泄露内部信息）与 `internal/web/server.go`（`Options` 增 `Auth` / `Registry` / `Lifecycle` / `RunService` / `Resources` 端口、`routeTable()` 集中登记）；测试 `internal/web/server_test.go`（+8）；`check: 011-web-auth`
- [x] T006 [P] 建立前端工程骨架 `assets/console/`：`package.json`、`package-lock.json`（实施首日按 npm registry 实测锁定 Vue 3 / Vite / TypeScript / Vitest 版本并写入实施报告）、`.nvmrc`、`tsconfig.json`、`vite.config.ts`、`index.html`、`scripts/emit-integrity.mjs`（产出 `integrity.json`：path → sha256）
- [x] T007 [P] 在 `internal/core/snapshot.go` 中定义准入清单的规整序列化与 `manifest_hash` 规则（键排序、MCP 按名字、技能按绑定声明序；不含时间戳），供 T015 实现引用

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: 存储层、生命周期权威读写、资源账、注册表与装配底座。

**⚠️ CRITICAL**: 本阶段完成前不得开始任何用户故事阶段。

- [x] T008 [P] 新建 `internal/storage/management_store.go`：`CreatePending` / `Finish` / `Get` / `List`（固定排序与窗口）/ `ListPending` / `Recover` SQL；测试 `internal/storage/management_store_test.go`（12 例：预写、终态回填、过滤、脱敏 detail、pending 查询、不可删除，并断言管理操作不写运行账本 / 调用审计、不挂 `run_id`——FR-012）；`check: 011-storage-management`
- [x] T009 [P] 新建 `internal/storage/resource_store.go`：版本、指针、引用与清单的读写 SQL（含 `UNIQUE(resource_kind, resource_id, version_no)` 冲突处理）；测试 `internal/storage/resource_store_test.go`（14 例：版本不可变、指针更新、引用释放、清单落库）；`check: 011-storage-resources`
- [x] T010 [P] 新建 `internal/storage/run_query.go`：`GET /runs` 的固定排序 `started_at DESC, run_id DESC`、`limit` 缺省 50 / 上限 200、三过滤参数；SQL 不含字符串拼接排序；测试 `internal/storage/run_query_test.go`（12 例：参数矩阵、legacy 零写入、`final_text` 取最近助手消息）；`check: 011-storage-resources`
- [x] T011 [P] 修改 `internal/storage/bootstrap.go` / `internal/storage/db.go`：v2 基线所需的事务入口与只读查询连接参数（不新增表）
- [x] T012 [P] 新建 `internal/workspace/lifecycle.go`：`.basalt/control/agents/<agent>.json` 的固定字段顺序、校验和、原子写（临时文件 + rename）、读取与校验；测试 `internal/workspace/lifecycle_test.go`（18 例：缺失 / 损坏 / 校验和不符 / 原子写 / 控制目录排除）；`check: 011-workspace-lifecycle`
- [x] T013 [P] 新建 `internal/app/registry.go`：不可变 `AgentRegistry` 快照与原子替换、注册不变量（无发布资源不注册）；修改 `internal/app/providers.go` / `internal/app/mcp.go` 提供注册所需快照；测试 `internal/app/registry_test.go`（12 例：集合一致、原子替换、不变量拒绝）；`check: 011-app-registry`
- [x] T014 [P] 新建 `internal/core/lifecycle.go`：`LifecycleState` / `AgentGate`（每 Agent 一把 RWMutex）/ `LifecyclePort` 与准入重读规则；测试 `internal/core/lifecycle_test.go`（14 例：重读、缺失 / 损坏 409、启停锁、归档只读）；`check: 011-core-lifecycle`
- [x] T015 [P] 新建 `internal/core/resources.go`：`ResourceGate`（进程级唯一 RWMutex）、`RunResourceManifest` 构造与哈希、视图与持久形态分离；测试 `internal/core/resources_test.go`（16 例：哈希稳定、MCP 顺序、legacy 标记、闸门读写互斥）；`check: 011-core-resources`
- [x] T016 [P] 新建 `internal/app/baseline.go`（迁移后、开放服务前执行 v2 基线：`system.baseline_v2` 先写 pending、按 `content_hash` 可重入、既有 Agent 建 `state=disabled` / `generation=1`、失败拒绝启动）；修改 `internal/app/run.go` 启动顺序；测试 `internal/app/baseline_test.go`（16 例：幂等、部分补齐、损坏拒绝、禁用初始态）与 `internal/app/recovery_operations_test.go`（10 例：只查证不重做、`unknown` 保留、`recovered_at` / `verification` 回填）；`check: 011-app-baseline`
- [x] T017 [P] 修改 `internal/core/service.go`：`ServiceOptions` 注入 `LifecyclePort` / `ResourceGate` / Registry 快照；测试 `internal/core/service_test.go`（+6）；`check: 011-core-lifecycle`
- [x] T018 [P] 修改 `internal/tool/policy.go` / `internal/tool/filetools.go`：`.basalt/control/` 在文件工具白名单中一律拒绝（真实路径投影后比前缀），错误码固定；在 `internal/tool/policy_test.go` / `internal/tool/filetools_test.go` 补充控制目录用例（不新增测试文件）；`check: 011-workspace-lifecycle`
- [x] T019 [P] 修改 `internal/storage/run_store.go` / `internal/storage/recovery.go`：运行行 + 清单 + 全部引用单事务写入、终态同事务释放且只更新仍为 `running` 的行、崩溃恢复不补造历史；测试 `internal/storage/run_store_test.go`（+8）与 `internal/storage/recovery_test.go`（+3）；`check: 011-storage-resources`
- [x] T020 [P] 修改 `internal/app/wire.go`：装配 `ResourceGate` / `AgentGate` / `LifecyclePort` / `ManagementStore` / `ResourceStore` / Registry，创建顺序满足"基线先于开放服务"；测试 `internal/app/wire_test.go`（+4）

**Checkpoint**: 存储 v2、生命周期权威、资源账与注册表就绪，用户故事可并行开始。

---

## Phase 3: User Story 1 - 管理认证闸门与必填配置 (Priority: P1) 🎯 MVP

**Goal**: 开放 HTTP 管理面的命令启动即校验认证配置；全部非公开端点先过 Host / Origin 守卫与来源归属再认证；同来源失败限速。

**Independent Test**: 四种配置启动矩阵 + 六类请求矩阵（缺 / 错凭据、错方案、Host 不符、Origin 不符、公开路径）与第 10 次限速。

- [x] T021 [US1] 新建 `internal/web/auth.go`：`AdminAuthConfig` 校验、`token` 模式常量时间比较、`local_trust` 三重条件（回环监听 + 回环直接对端 + `X-Basalt-Local-Trust: 1`；任一转发头 → 403）、actor 字面量生成；测试 `internal/web/auth_test.go`（26 例，关键回归 `TestAuth_Matrix` 原样落地：六类请求 × 两模式）；`check: 011-web-auth`
- [x] T022 [US1] 新建 `internal/web/auth_limit.go`：60 秒窗口计数，只计 401 与 `local_trust` 403，第 1–9 次原状态码、第 10 次起 429 + `Retry-After`（ceil），成功不重置，窗口过期清理；用例并入 `internal/web/auth_test.go`（`TestAuthLimit_*`）
- [x] T023 [US1] 新建 `internal/web/middleware.go`：Host 逐字匹配（含端口、大小写不敏感）、Origin 完整匹配（`null` 拒绝）、可信代理才解析转发头（最右非可信地址）；修改 `internal/app/web.go` 启动期拒绝缺配置 / 非法模式 / 明文凭据；测试 `internal/app/web_test.go`（+5）；`check: 011-web-auth`
- [x] T024 [US1] 修改 `internal/web/handlers_test.go`（+6，既有端点认证矩阵）、`internal/web/handlers_schedules_test.go`（+3，调度端点需认证）、`internal/web/handlers_sessions.go`（actor 显式参数传递，不写 context）；`check: 011-web-auth`
- [x] T025 [US1] 在 `internal/web/server.go` 完成公开面闭集（`/api/v1/health`、`/console/*` 静态壳、`/` 重定向）与其余端点统一认证挂载；补充 `internal/web/server_test.go` 路由矩阵（本任务新增部分）；`check: 011-web-auth`

**Checkpoint**: US1 可独立验收——认证矩阵全绿即安全底座成立。

---

## Phase 4: User Story 2 - 运行资源账与运行观测 (Priority: P1)

**Goal**: 准入固定 Provider / MCP / 技能版本并单事务记账；运行列表与详情可核对；历史运行标 `legacy_unrecorded`；取消是信号。

**Independent Test**: 一次运行的三表关联、终态释放、历史库 legacy、取消语义（重复 / 404 / 超时不强写）。

- [x] T026 [US2] 修改 `internal/core/run.go` / `internal/core/store.go` / `internal/core/snapshot.go`：准入固定资源集合、构造清单并随运行创建传入仓储；`check: 011-core-resources`
- [x] T027 [US2] 新建 `internal/app/run_service.go`：`RunService` 适配器（`Run` / `Get` / `List` / `Cancel`），清单持久形态 → HTTP 视图投影；测试 `internal/app/run_service_test.go`（8 例）；`check: 011-app-runtime-admin`
- [x] T028 [US2] 新建 `internal/web/handlers_runs.go`：`GET /runs`（固定排序 / 窗口 / 三过滤，非法参数 400）、`GET /runs/{id}`（含清单，legacy 视图）、`POST /runs/{id}/cancel`（200 + 当前视图，404 未知）；测试 `internal/web/handlers_runs_test.go`（18 例）；`check: 011-web-observability`
- [x] T029 [US2] 新建 `internal/core/cancel_test.go`（10 例）并在 `internal/core/service.go` 落地取消语义：只发 `context` 取消、最多等 30 秒、终态由循环写入、重复取消返回当前状态；`check: 011-core-cancel`
- [x] T030 [US2] 新建 `internal/web/handlers_operations.go`：`GET /operations` / `GET /operations/{id}`（固定排序、脱敏 `detail_json`、`unknown` 提示、404）；测试 `internal/web/handlers_operations_test.go`（10 例）；`check: 011-web-observability`
- [x] T031 [US2] 修改 `internal/web/views.go`：`RunView` / `RunDetailView` / `OperationView` 只投影契约字段（持久形态不直接序列化）；修改 `internal/web/contract_test.go`（+8，逐字段对 `docs/contracts/openapi.yaml`）；`check: 011-web-observability`

**Checkpoint**: US1 + US2 可独立验收——安全闸门 + 可核对运行账。

---

## Phase 5: User Story 3 - Agent 生命周期准入 (Priority: P1)

**Goal**: 状态文件唯一权威；停用 / 归档硬门拒绝新运行；停用不取消在途；启用要求版本匹配；管理操作先留痕。

**Independent Test**: 基线后既有 Agent `disabled` → 启用 → 运行；手改 / 删除状态文件 → 409；停用后在途运行完成；启用 `generation` 冲突被拒。

- [x] T032 [US3] 新建 `internal/app/agent_console.go`：`AgentAdmin` 适配器（enable / disable：先写 pending → 状态文件原子写 → 索引同步 → 回填终态；期望 `generation` 校验；失败保旧）；测试 `internal/app/agent_console_test.go`（8 例，含预写失败零写入）；`check: 011-app-runtime-admin`
- [x] T033 [US3] 新建 `internal/web/handlers_lifecycle.go`：`POST /agents/{name}/enable` / `disable` 与状态视图；测试 `internal/web/handlers_lifecycle_test.go`（14 例：409 `AGENT_UNAVAILABLE`、版本冲突、在途不受影响、actor 记录）；`check: 011-web-content-lifecycle`
- [x] T034 [US3] 在 `internal/core/service.go` / `internal/core/run.go` 集成准入重读：Agent 级读锁内重读状态文件并校验，失败分类固定；HTTP / CLI / 定时三入口共用同一准入判定（不得出现第二份生命周期缓存）；补充 `internal/core/lifecycle_test.go` 的准入用例（`TestLifecycle_Admission*`，含三入口一致性）；`check: 011-core-lifecycle`
- [x] T035 [US3] 补充 `internal/app/baseline_test.go` 的"既有 Agent 基线 disabled"与 `internal/app/recovery_operations_test.go` 的"启停操作恢复查证"用例（不新增文件）；`check: 011-app-baseline`

**Checkpoint**: 三个 P1 故事全部可独立验收，安全、可核对、可停用。

---

## Phase 6: User Story 4 - Agent 配置内容条件写 (Priority: P2)

**Goal**: 强 ETag + `If-Match` 条件写；补丁保留未改行；无法安全保留时 400；并发保存只有一个成功。

**Independent Test**: 旧 ETag → 412 零写入；补丁保留注释 / 行尾；正文补丁 → 400 `EDIT_UNSUPPORTED`；双请求并发仅一成功。

- [x] T036 [US4] 新建 `internal/workspace/content_edit.go`：原始字节 sha256 ETag、行级 frontmatter 补丁（保留注释 / 行尾 / 未改行）、不可安全保留判定、原子写；测试 `internal/workspace/content_edit_test.go`（22 例：412 零写入、注释保留、CRLF、锚点不唯一、并发写）；`check: 011-workspace-content`
- [x] T037 [US4] 新建 `internal/web/handlers_agent_content.go`：`GET /agents/{name}/content`（ETag）、`PUT`（`If-Match` 必填，缺失 / 弱比较 / `*` / 不匹配 412；`content` / `changes` 二选一）、成功返回 `AgentFilesView` + 新 ETag；测试 `internal/web/handlers_agent_content_test.go`（20 例）；`check: 011-web-content-lifecycle`
- [x] T038 [US4] 将内容写接入管理操作留痕与资源闸门（预写 pending → 生成候选并校验 → 原子写 → 快照替换 → 回填终态；任一步失败磁盘与快照保持原样），失败路径同时写 WARN 日志与审计；用例并入 `internal/web/handlers_agent_content_test.go`（`TestAgentContent_Operation*`）；`check: 011-web-content-lifecycle`

**Checkpoint**: US4 独立验收——条件写与留痕闭环。

---

## Phase 7: User Story 5 - 配置热加载与边界 (Priority: P2)

**Goal**: 非递归监听 + 防抖全量 rescan；坏文件保留旧快照；在途运行不受影响；需重启配置明确提示；技能按发布版本冻结。

**Independent Test**: 改 `AGENT.md` → 新运行生效；改坏 → 旧条目保留 + WARN；改 `mcp_servers.yaml` → 提示需重启；技能版本冻结。

- [x] T039 [US5] 新建 `internal/workspace/watcher.go`（fsnotify 非递归、新建目录补监听、防抖、失败保旧、控制目录排除）；测试 `internal/workspace/watcher_test.go`（12 例）；`check: 011-workspace-watcher`
- [x] T040 [US5] 新建 `internal/app/watcher.go` 并修改 `internal/core/scheduler.go` / `internal/app/schedule.go`：注册表原子替换后调度器同步增减；用例并入 `internal/app/registry_test.go`（`TestRegistrySync_*` / `TestStartWatcher_*`，不新增测试文件）；`check: 011-app-registry`
- [x] T041 [US5] 新建 `internal/workspace/skill_version.go` 并修改 `internal/workspace/agent.go` / `internal/workspace/context_loader.go` / `internal/workspace/skill.go`：技能发布版本与不可变目录解析、L1/L2/L3 哈希、声明名无实体 → 整只 Agent 不注册；测试 `internal/workspace/skill_version_test.go`（14 例）；`check: 011-workspace-watcher`
- [x] T042 [US5] 在 `internal/web/views.go` 增加"需重启"提示投影（MCP 定义 / 沙箱白名单 / Provider 段 / HTTP 段），并补充 `internal/web/contract_test.go` 对应用例（本任务新增部分）；`check: 011-app-registry`
- [x] T043 [US5] 覆盖 `internal/core/snapshot.go` 的在途运行隔离：运行内只读准入时固定的技能版本目录；用例并入 `internal/core/resources_test.go`（`TestResources_SkillFreeze*`）；`check: 011-core-resources`

**Checkpoint**: US5 独立验收——热加载边界有证据。

---

## Phase 8: User Story 6 - 只读资源面与系统概览 (Priority: P3)

**Goal**: 技能 / MCP / 模型连接 / 会话 / 记忆 / 沙箱 / 通知只读面无凭证泄露；系统概览采样不伪造。

**Independent Test**: 每个只读端点返回契约字段；凭证只出现环境变量名；首次采样 `null` + `unavailable`，5 秒缓存。

- [x] T044 [US6] 新建 `internal/web/handlers_system.go`、`internal/web/sampler.go`、`internal/web/sampler_windows.go`、`internal/web/sampler_linux.go`、`internal/web/sampler_other.go`：概览计数与 DB 状态、5 秒缓存、无差值基线 `null` + `unavailable`；测试 `internal/web/handlers_system_test.go`（12 例）；`check: 011-web-observability`
- [x] T045 [US6] 新建 `internal/web/handlers_resources.go` 与 `internal/app/resources.go`：技能 / MCP / 模型连接只读投影，只回环境变量引用名与可执行名；测试 `internal/web/handlers_resources_test.go`（12 例）与 `internal/app/resources_test.go`（11 例）；`check: 011-web-observability`
- [x] T046 [US6] 新建 `internal/web/handlers_sessions_query.go`、`internal/web/handlers_memory.go`、`internal/web/handlers_sandbox.go`、`internal/web/handlers_notify_read.go`，并修改 `internal/storage/session_store.go` / `internal/web/handlers_sessions.go` 提供固定排序与过滤；测试 `internal/web/handlers_sessions_query_test.go`（10）、`internal/web/handlers_memory_test.go`（6）、`internal/web/handlers_sandbox_test.go`（6）、`internal/web/handlers_notify_read_test.go`（8）、`internal/storage/session_store_test.go`（+4）；`check: 011-web-readonly-meta`
- [x] T056 [US2] （派生补齐 2026-10-21；依据模块卡 §3.1.24 的 `workspace/context_loader.go` / `workspace/skill.go` / `tool/policy.go` / `core/snapshot.go` 行、§3.3 行 34、约束 31 与 N1）修改 `internal/workspace/context_loader.go`：新增 `PublishedSkills`（技能名 → 发布目录 `storage_path`）与 `SnapshotPublished`——卡片行展示发布版本真路径、快照清单仍记逻辑路径 `.basalt/skills/<name>/SKILL.md`，声明技能缺目录或 `SKILL.md` 读不到一律失败关闭，`Snapshot` 的工作副本口径不变；修改 `internal/tool/policy.go`：`checkFileRead` 追加 `.basalt/versions/skills/` 只读放行、`checkFileWrite` 对同一前缀按 `skills_read_only` 拒绝（控制目录规则仍先行）；在 `internal/app/resources.go` 新增 `runSnapshotBuilder` 实现 `core.RunSnapshotBuilder`（消费 `ResolvedResources.SkillDirs`）；`internal/app/providers.go` 增加覆盖感知的目录构造并让 `modelClient` 复用同一张映射表；`internal/app/wire.go` 一次构建技能库与 Provider 目录，传 `ServiceOptions.Resources` / `RunSnapshots`，`heavyRuntime` 保留 `versions` / `mcpRT` / `skills` / `ops` 句柄；测试 `internal/workspace/skill_version_test.go`（+3）、`internal/tool/filetools_test.go`（+2，发布版本路径矩阵）、`internal/app/resources_test.go`（+3）、`internal/app/wire_test.go`（+1）；`check: 011-workspace-watcher`、`011-app-runtime-admin`（tool 用例无专属选择器，由全量 `make check` 判卷）
- [x] T057 [US6] （派生补齐 2026-10-21；依据模块卡 §3.3 行 1510、`storage/db.go` 行 10、契约 `SystemOverview` 与 FR19）修改 `internal/storage/run_query.go`：新增 `CountRuns`（运行中 / 近 24 小时 / 近 24 小时失败三条计数，手写 SQL）；修改 `internal/app/web.go`：`webOptions` 装配 `Registry` / `Lifecycle` / `AgentAdmin` / `Operations` / `System` / `Sampler` / `Resources` 七个端口，并新增 `systemReader` 适配（`Ping` + `SchemaVersion` + `CountRuns` + `ListPointers` 计数，任一读失败返错由 handler 标 `unavailable`）；测试 `internal/storage/run_query_test.go`（+2）、`internal/app/web_test.go`（+2）、`internal/app/resources_test.go`（+1）；`check: 011-storage-resources`、`011-app-runtime-admin`

**Checkpoint**: US6 独立验收——只读面齐全且不泄露。

---

## Phase 9: User Story 7 - 内嵌管理台 (Priority: P3)

**Goal**: Vue + Vite 产物嵌入单二进制；八条固定路由；token 只在内存；产物与完整性清单一致。

**Independent Test**: 深链接刷新 / 未知路径 404 / 未保存离开 / 401 回登录；`integrity.json` 与 `dist/` 逐文件哈希相等；Go 门禁不调用 Node。

- [x] T047 [US7] 实现 `assets/console/src/` 的路由、API 客户端、认证状态与安全渲染器（History API 八路由、无第三方路由 / Markdown 库、token 仅内存、固定标签子集渲染）；含 `src/main.ts` / `src/router.ts` / `src/api.ts` / `src/auth.ts` / `src/render.ts`
- [x] T048 [US7] 实现管理台视图：总览 / 登录 / Agent 列表 / Agent 详情（六页签）/ 运行列表 / 运行详情 / 资源（`/console/resources/:kind`，skills｜mcp-servers｜model-connections）/ 操作 / 404；含未保存离开确认与 401 跳转保留 `next`；Vitest 单元用例覆盖路由、渲染与认证状态
- [x] T049 [US7] 执行 `npm run build` 产出 `assets/console/dist/` 与 `assets/console/integrity.json`；新建 `assets/embed.go`（`go:embed`）；新建 `internal/web/console.go`（静态壳、SPA 回退、CSP）；测试 `internal/web/console_test.go`（15 例：产物集合与逐文件 sha256、深链接、404、CSP、无第三方资源）；`check: 011-web-content-lifecycle`
- [x] T050 [US7] 修改 `.github/workflows/ci.yml`：新增独立 Node job（`npm ci` → `npm test` → `npm run build`，随后校验 `git status --porcelain -- assets/console/dist` 为空且集合 / 哈希与 `integrity.json` 相等），固定 Node 版本（`.nvmrc`）；Go job 不调用 Node

**Checkpoint**: US7 独立验收——管理台可见、可交互、构建可复现。

---

## Phase 10: Polish & Cross-Cutting Concerns

**Purpose**: 文档、验收配置、全量门禁与人工 Demo 证据。

- [x] T051 [P] 更新 `docs/Operations.md`（管理认证配置、备份 / 升级含 v2 迁移、控制目录说明）与 `docs/GettingStarted.md`（管理台入口与认证步骤）
- [x] T052 [P] 派生 `harness/stages/us11-console-observability.json`：十七个 check、`required_tests` 精确匹配、task 绑定与保护路径；**不进** `current.json`、不进 CI（`inputs` 含私有 `docs/us/*`）
- [x] T053 运行全量门禁：`make check`（七目标 + 覆盖率判卷）、`go test -race ./internal/...`、`go test -tags=integration ./...`、`CGO_ENABLED=0 go build ./...`；记录逐目标四态
- [x] T054 运行专属 stage：`.\scripts\check-acceptance.ps1 --stage harness/stages/us11-console-observability.json`，逐 check 记录读数；随后按 §6.2 执行 Demo A–F 并留证（不含凭证）
- [x] T055 收口前清理临时物：`basalt-mdlink-923`、`basalt-contract-check-270`、`basalt-private-restore-afy12cvz`、`C:/Users/76562/AppData/Local/Temp/basalt-us11-baseline-kngsjv`，并如实报告删除结果

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 → Phase 2**：T001–T007 完成后才能开始 Foundational（迁移与依赖是所有后续任务的前提）
- **Phase 2 → Phase 3–9**：T008–T020 是硬阻塞；完成前用户故事无法编译或通过认证
- **Phase 3（US1）** 是其余故事的前置：所有 HTTP 验收都需认证通过
- **US2 / US3** 可在 US1 后并行；**US4** 依赖 US3 的操作留痕与闸门；**US5** 依赖 US2 的注册表冻结与 US3 的状态文件权威；**US6** 依赖 US2 的视图层；**US7** 依赖 US1–US6 的全部端点
- **Phase 10** 必须在全部故事完成后执行

### Within Each User Story

测试文件与实现同任务落地（harness 先行）；关键回归测试先取红态再实现；每个任务结束跑 `go test ./internal/<pkg>/...` 与对应 `--task` stage。

### Parallel Opportunities

- Phase 1：T001–T007 全部 `[P]`（文件互不重叠）
- Phase 2：T008–T020 全部 `[P]`（不同包 / 不同文件）；同一包内 T008 / T009 / T010 / T019 分属不同文件，可并行
- Phase 3：T021–T023 可并行；T024 / T025 依赖 T021
- Phase 4：T026 / T027 / T030 可并行；T028 依赖 T027；T029 与 T026 同包需串行
- Phase 5：T032 / T033 可并行；T034 / T035 依赖 T032
- Phase 6：T036 先行；T037 / T038 依赖 T036
- Phase 7：T039 / T041 可并行；T040 依赖 T039；T042 / T043 依赖前序视图与清单
- Phase 8：T044 / T045 / T046 可并行
- Phase 9：T047 → T048 → T049 → T050 严格顺序（同一前端工程）

## Implementation Strategy

**MVP first**: US1（认证闸门）是唯一必须先独立完成的增量——它把既有无认证 HTTP 面收敛为安全面，其余故事在其上叠加。

**Incremental delivery**: US2 → US3（同为 P1），随后 US4 → US5（P2），最后 US6 → US7（P3）。每个 checkpoint 均可用专属 stage 的对应 check 独立验收，不依赖后续故事。

**Evidence**: 每个任务的 `check:` 标注即 stage 映射来源；T052 的 stage 文件必须能追溯到模块卡 §4.4 的十七个 check，不得新增或合并检查。

## UI-R2 追加任务（2026-10-22）

用户逐页复核后的选项 A 扩展，旧 T047–T050 的验收记录保留；本节补充当前有效范围，不把历史勾选当新制品证据。

- [x] UI-R2-01 [US7] 保留十二路由 + 404；更新约束 41、FR-043、plan / research、路由注释 / 用例与 §3.1.25 文件清单；四条新增只读页面复用已有受保护 API。
- [x] UI-R2-02 [US7] 统一 Agent 详情、运行列表 / 详情、三类资源、管理操作与 404 的页眉 / 说明 / 区块；运行页不高亮总览；工具与 MCP 仅一个主入口；修复表格断词、重复最终文本、总览等高与窄屏最近执行；说明后端路径模板。
- [x] UI-R2-03 [US7] 增补导航 / 单一最终文本 / 路径模板 / 主内容焦点 / 详情 ID 回归；重新执行前端测试、类型检查与生产构建（60 项通过，三产物完整性清单）。
- [x] UI-R2-04 [US7] 对当前修订执行增量 analyze、独立只读复核、完整 US-11 stage 与嵌入制品 / 演示实例核验，并提交人工 review；证据与待确认面追加到 run-state。

## 2026-10-22 五项裁决修复（F-01 / F-03 / F-04 / INFO-1 / INFO-2）

用户五项裁决与 Demo F 登记项的落地记录；本节只追加，不改既有勾选与正文。

- [x] ADJ-01 F-01 A（措辞对齐，实现不变）：`spec.md` FR-019 与模块卡约束 17 改为"准入读锁只覆盖重读并校验状态文件，判定后立即释放；资源一致性由 `ResourceGate` 读租约保障；线性化点 = 状态放行判定"，明确不声称"停用返回后不再出现新 StartRun"；复核锚点补 `TestRunResources_ReadLockLeasesUntilReleaseAdmission` / `TestAgentService_Resources_AdmissionLeaseSpansStartRun`；约束 46 点名的 `TestRunAdmission_PublishGateConsistent` / `TestRunRuntime_ReleaseAfterFinish` 属 US-12，不冒充本 US 证据。
- [x] ADJ-02 F-03 A（注册过滤）：新增 `internal/app/registry.go` 的 `skillPointerSource` / `publishedSkillNames` / `missingPublishedSkills` / `registryEntriesWithSkillPublication`，启动装配（`wire.go`）与 watcher（`watcher.go`，`newRegistrySync` 增 `versions` 来源）共用同一判定；无发布指针 → 启动剔除、运行期退出注册表并 `SetRegistered(false)`，WARN `skill version missing: <name>`；指针读取失败 → 启动拒绝、watcher 保留旧注册表。测试：`internal/app/baseline_test.go` 增 `TestRegistryEntries_SkillWithoutPublishedPointerIsNotRegistered` / `TestRegistryEntries_PointerLookupFailureRejectsStartup`，`internal/app/registry_test.go` 增 `TestRegistrySync_RetiresAgentWithoutPublishedSkill` / `TestRegistrySync_PointerLookupFailureKeepsOldRegistry`；`check: 011-app-baseline`、`011-app-registry`。
- [x] ADJ-03 F-04 B1（显式运行标记）：`core.ToolEnv` 增 `RunBound bool`（`RunEnv.ToolEnv` 置 true）；`core.Action` 增同名字段，文件工具读 / 列目录显式传递；`tool.WhitelistPolicy.checkFileRead` 对 `.basalt/skills` 工作副本在 `RunBound` 时拒绝（规则 `技能工作副本在运行中不可读`），非 Run 维持可读；契约 `run-and-errors.md` §1.1 增字段并注明不得由 `RunID` / `SkillDirs` 推断。测试：`internal/tool/filetools_test.go` 增 `TestFileTools_RunBoundRefusesSkillWorkCopy`，`internal/tool/policy_test.go` 增 `TestWhitelistPolicy_RefusesSkillWorkCopyReadInsideRun`，`internal/core/service_test.go` 的 `TestAgentService_Resources_ToolEnvCarriesBoundSkillDirs` 增加 `RunBound` 断言；`check: 011-race`。
- [x] ADJ-04 INFO-2 A（局部契约说明）：`docs/contracts/openapi.yaml` 的 `AgentFilesView.lifecycle` 增 `$ref` 同级 `description`，说明本响应只保证 `state` / `valid` / `lifecycle_error` 的内容投影，其余零值不代表状态文件缺失；`AgentLifecycleView` 全局形状不变。
- [x] ADJ-05 DEMO-INFO-1（favicon 404）：`assets/console/index.html` 增 data-URI SVG `rel="icon"`（无新路由 / 新文件 / CDN）；`npm test` 60/60、`npm run build` 重建 `dist` 与 `integrity.json`（三产物，CSS / JS 哈希不变，`index.html` 哈希更新）。
- [x] ADJ-06 INFO-1（US-12 集成义务）：记录为 US-12 必须完成的集成义务（Agent 配置内容写的管理操作结果查证 / 恢复接线），US-11 不改实现；已写入 run-state 收口结论。
- [x] ADJ-07 stage 映射更新：`harness/stages/us11-console-observability.json` 的 `approval` 追加本轮裁决与冻结输入说明；`011-app-baseline` 选择器增 `TestRegistryEntries_` 并增两条 required_tests，`011-app-registry` 增两条，`011-race` 增两条；`tasks` 映射增 `ADJ-02` / `ADJ-03`；未删任何 required_tests、未弱化断言。
- [x] ADJ-08 收口复跑：`/speckit-analyze`（CRITICAL 清零）、独立只读复核、完整 US-11 stage 全绿并重算记录哈希；更新 run-state 与收口包，提交人工 review ②。

## 2026-10-22 收口复核修复（R-01 … R-06）

独立只读复核 findings 的处置记录；本节只追加，不改既有勾选与正文。

- [x] FIX-R01 嵌入壳 favicon 改为 base64 data URI（同一 SVG，`index.html` 不再含字面 `http://`），重建 `assets/console/dist` 与 `integrity.json`；`TestConsoleDist_NoExternalReferences` / `_IntegrityMatchesFiles` / `_NoStrayFiles` 全部通过（CSS / JS 哈希不变）。收尾自检发现首版 payload 把 `#` 误编码成 `%23`（base64 解码后是非法颜色值），已改为解码即 `fill='#2b2f3a'` / `stroke='#e8e4d8'` 并重跑 `npm run build`；dist `index.html` sha256 = `217ea495a8c8d6d580f83b3a5432b71a6ed5cb7028b5db93b1ca44663304d235`。
- [x] FIX-R02 契约 `run-and-errors.md` §3.4 改为两道锁分工：`AgentGate` 读锁只覆盖"重读并校验状态文件"，`ResourceGate` 读租约覆盖"资源解析 → 固定资源 → `StartRun` 成功"，线性化点 = 状态放行判定；删除"读锁覆盖建 Run 行 + 写引用"的过时表述。
- [x] FIX-R03 stage `011-web-content-lifecycle` 选择器增 `TestConsoleDist_`，required_tests 增 `TestConsoleDist_IntegrityMatchesFiles` / `TestConsoleDist_NoExternalReferences` / `TestConsoleDist_NoStrayFiles`；模块卡 §4.4 R28 同步，选择器表与 argv 块保留 §4.4 推导基线（追加在 §4.4 注中登记，stage JSON 为唯一执行事实源）；stage `approval` 追加增补说明，未删任何 required_tests、未弱化断言。
- [x] FIX-R04 契约 `agent-md.md` §2.1 规则 5 写明多缺失名格式（升序、逗号加空格、一条 WARN）；新增 `TestRegistryEntries_MultipleMissingSkillsSortedJoin` 钉住该字面量，并加入 `011-app-baseline` required_tests。
- [x] FIX-R05 契约 `run-and-errors.md` §1.1 `ToolEnv` 构造者改为"ReAct 循环经 `RunEnv.ToolEnv` 构造、值传递；ToolExecutor 只接收并执行"。
- [x] FIX-R06 路径勘误：T049 与 plan 原写 `assets/console/integrity.json`，实际产物为 `assets/console/dist/integrity.json`；T049 已勾选不回改，plan 与本节为准。

## 2026-10-22 独立复核意见处置（C1–C4 / F-05）

独立只读复核结论为"带条件通过"（无 BLOCKER / HIGH）；以下为逐条处置。只追加，不改既有勾选与正文。

- [x] C1（F-01 MEDIUM）`agent-md.md` §2.1 规则 5 拆回两种失败形态：库里无实体（加载期）→ `skill not found: <name>`；有实体但无已发布版本（注册装配期）→ `skill version missing: <name>[, <name>…]`；字段表同步写明两种拒载原因。代码与测试未改——两种字面量本就并存于 `internal/workspace/agent.go` 与 `internal/app/registry.go`。
- [x] C2（F-02 LOW）`run-and-errors.md` §3.4 线性化点笔误：`状态释放判定` → `状态放行判定`。
- [x] C3（F-03 LOW）`run-and-errors.md` §3.4 补 FR-019 保留条款：禁用写锁取得**之前**已判定放行的准入仍可能完成 `StartRun`，**不得**声称禁用返回即保证没有新 Run 开始。
- [x] C4（F-04 LOW）模块卡 §4.4：选择器表回退为 §4.4 推导基线（`TestConsoleDist_` 的追加改记于同节"后续裁决追加"注），argv 块与表同步注明"基线 + approval 追加、stage JSON 为唯一事实源"；R28 证据行保留 `TestConsoleDist_*`。
- [x] F-05（INFO）`TestConsoleDist_NoExternalReferences` 只扫 `index.html`：复核确认当前无违规（JS 内只有惰性命名空间与错误提示 URL，无 fetch / import / CDN）。收紧为全产物扫描属于判据面变更，不在收口轮实施；登记为已知测试边界，交后续裁决。
