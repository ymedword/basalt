# Tasks: US25 Compose 多 Worker 交付

**Input**: spec.md / plan.md / research.md / data-model.md / contracts/README.md / quickstart.md；颗粒度文档 `docs/us/us25-compose-multiworker.md` R3（review ① 已通过，2026-11-08；现行正文 1917 行 / SHA-256 `22e203fb2ef72cdeaafa02f637ad87993642ddc0dee1afb70a4939f4d91b9973`，批准正文 1906 行 / SHA-256 `9b13034e659c48ab85c68b62dcf5c035f4560fed4851cdaef934559c0bde6d64`，其后仅追加 §6.2 通过记录）。

**Tests**: 已审 Harness 的必跑测试与关键拒绝路径必须落地；测试函数名用模块卡 §3.2 / §4.6 已给出的英文名（`TestXxx_yyy` 形态），中文验收点写进注释。新增安全 / 并发 / 凭证 / 迁移类回归红态先行（逆改点见模块卡 §4.4 / §4.6）；不删断言、不无理由跳过（`t.Skip` 必须写理由并进 run-state 未验证清单）、不放宽阈值。容器级用例（`-tags=e2e`）在 Windows 原生不可用时跳过并登记未验证，不得伪造通过。

## Phase 1: Setup

- [x] T001 核对 review ① 依据与实施基线：确认台账 US-25 行、分支 `025-compose-multiworker`、起点 `3f8d0dc`、工作树仅 `?? specs/025-compose-multiworker/`，把模块卡哈希、spec / plan 指纹、允许触碰范围写入 `specs/025-compose-multiworker/run-state.md` 基线；DoD：登记唯一、哈希可复算、无范围外改动、失败预算计数为 0。
- [x] T002 从模块卡 §4.8 派生 `harness/stages/us25-base.json` / `us25-compose.json` / `us25-fault.json` / `us25-docs.json`：11 个 check id（`us25-base-first-install` / `us25-base-history` / `us25-base-cli` / `us25-base-demo` / `us25-base-releasekit` / `us25-base-race` / `us25-docs` / `us25-compose-roles` / `us25-compose-seed` / `us25-fault-execution` / `us25-fault-migration`）逐条照抄命令参数、超时与 `required_tests`；`protect` 照 §4.9 列表 + `docs/contracts/**` + `harness/acceptance.json`；`approval` 记录 review ① 哈希与日期、容器宿主前置（Docker / Linux 容器或 WSL2）；`tasks` 映射本文件；DoD：四份 JSON 合法、四个 stage 均为本地专用（不进 `current.json` / CI）、不复制公共检查定义、命令在本仓可执行。

## Phase 2: Foundational

**Purpose**: 命令面端口类型——US1 / US2 / US4 的共同前置（纯类型，无行为）

- [x] T003 在 `internal/cli/ports.go` 追加命令面端口与报告类型：`FirstInstallReport` / `FirstInstall`、`DemoPrepareRequest` / `DemoCollectionPlan` / `DemoDocumentPlan` / `DemoBindingPlan` / `DemoPreparePlan` / `DemoCollectionResult` / `DemoDocumentResult` / `DemoBindingResult` / `DemoPrepareResult` / `DemoPrepare`、`MasterKeyCheckReport` / `MasterKeyCheck`；在 `internal/cli/deps.go` 追加 `FirstInstall` / `DemoPrepare` / `MasterKeyCheck` 三个字段；DoD：签名与模块卡 §3.1.1 / §3.1.3 / §3.1.4 逐字一致、`cli` 不 import `app`（报告类型分开）、既有命令零行为变化、`CGO_ENABLED=0 go build ./...` 通过。

## Phase 3: User Story 1 - 首次安装可 init 优先并支持中断重试 (P1)

**Goal**: `init` 先写配置 / 首次进度 / 名册再建 SQLite 源库；首次 `serve` 中断后重试可续做并补发主密钥；已有业务历史的实例不自动铸钥。

**Independent Test**: 临时目录只跑 `init`，断言准备先于建库；模拟首次 `serve` 缺入口限流退出后重跑，断言密钥补发；已有运行历史的库上断言不生成新钥；并发两个首次启动只有一个成功写入。

- [x] T004 [US1] 在 `internal/app/first_install.go` 新增无状态 `firstInstallAdapter`（`Prepare` 调 `ensureNewInstanceConfig(workDir, programRuntimeRoot())`，`firstInstallReport` 字段一一映射且不改 `Fresh` 语义）；在 `internal/cli/init.go` 把 `deps.FirstInstall.Prepare(deps.WorkDir)` 插到 `Workspace.Init` 之后、`Store.Bootstrap` 之前（nil 返回装配错误且不建库）；在 `internal/app/run.go` 两条 `cli.Deps` 构造路径注入 `FirstInstall: firstInstallAdapter{}`（`gateway` 不调用 `Prepare`）；在 `internal/app/instance_bootstrap_test.go` 追加 `TestFirstInstall_PrepareOrderAndIdempotency` / `TestFirstInstall_ConcurrentStartOneWinner` / `TestUS25_FirstInstall_HalfWrittenRosterFailsClosed`，在 `internal/cli/init_test.go` 追加 `TestCLI_InitPreparesFirstInstallBeforeBootstrap` / `TestCLI_InitNilFirstInstallFailsClosed`；DoD：红态先行（先建库后准备时 `TestCLI_InitPreparesFirstInstallBeforeBootstrap` 逐字红）、第二次调用零写入、并发恰好一个成功、名册半截 / 不可解析失败关闭且提示人工处理、`init` 不取实例锁。
- [x] T005 [US1] 在 `internal/storage/postgres/instance_secrets_store.go` 新增 `HasRuntimeHistory(ctx) (bool, error)` 与包内 `runtimeHistoryTables = [...]string{"runs", "task_executions", "agent_lifecycle", "kb_documents"}`（单条只读 `EXISTS` / `UNION ALL … LIMIT 1`，不整表读内存；任一查询失败返回错误，不吞成 false）；新建 `internal/storage/postgres/instance_secrets_store_test.go` 落 `TestRuntimeHistory_Tables` / `TestRuntimeHistory_ManagementOperationsExcluded`（`integration`，本地 PG 夹具）；DoD：四表逐一非空命中、`management_operations` 不计入、空库为 false、查询受调用方 `ctx` 超时约束、不新增迁移 / 索引。
- [x] T006 [US1] 在 `internal/app/instance_secrets.go` 扩展 `SecretsFacts`（`ProvisioningPending` / `RuntimeHistory` / `RuntimeHistoryErr`）、改 `EvaluateSecrets` 的 create 支为“无记录 + 无密钥 + 零托管密文 + 零业务历史 + `auto_init` +（`fresh` 或 `pending`）”、改 `gatherFacts` 调 `adminTokenProvisioningPending` 与 `Store.HasRuntimeHistory`（读历史失败写 `RuntimeHistoryErr` 落 `await_init`）；在 `internal/app/instance_secrets_test.go` 追加 `TestFirstInstall_SecretsEligibility` / `TestFirstInstall_ExistingInstanceNeverMints` / `TestFirstInstall_RecoveryNeverCreates` / `TestFirstInstall_KeyCreationAudited`（既有 `TestInstanceSecrets_ExistingInstanceNotAutoInit` / `TestInstanceSecrets_FreshInstanceStillAutoInitializes` 保留）；DoD：判定表七场景逐行成立、create 前预写 `pending` 审计（C-27）、恢复态与就绪态绝不落 create、D-40 语义与既有字段不变。
- [x] T007 [US1] 新建 `internal/e2e/us25_first_install_test.go`（`e2e`）落 `TestUS25_FirstInstall_InitThenServe` / `TestUS25_FirstInstall_RetryAfterIngressExit` / `TestUS25_FirstInstall_WorkerSeedSkipsBootstrap`；DoD：容器宿主执行（执行前置 T013 的镜像与种子资产）、`init` 后三件同在且准备先于建库、缺 `BASALT_INGRESS_*` 退出后重试补发主密钥、Worker 种子启动不写 `instance.json` / 不生成 admin token / 不建 SQLite 库且 `instance_id` 不变；缺 Docker 时 `t.Skip` 并登记未验证。

## Phase 4: User Story 2 - 双角色 Compose 编排与 Worker 启动边界 (P1)

**Goal**: 一个控制面 `serve` + 至少一个 Worker `gateway` + 共享 PostgreSQL；Worker 无 HTTP、无管理面密码、不自建实例；种子带逐文件清单并拒绝篡改。

**Independent Test**: 解析并启动双角色编排，断言控制面有端口、Worker 无端口且以既有后台模式运行；缺种子或缺密钥时预检退出 78；被篡改种子拒绝导入；两个 Worker 同时注册且实例标识不同。

- [x] T008 [US2] 在 `internal/app/master_key_check.go` 新增无状态 `masterKeyCheckPort.Check(path, workspaceRoot)`（复用 `LoadMasterKey` 语义与 `MasterKeyFingerprint`，`Key.Zero()` 后返回，不留密钥材料）；在 `internal/cli/secrets.go` 的 `newSecretsCommand` 增加 `check-key` 子命令（`--path <file>`、`--workspace <dir>`，缺省 `<WorkDir>/.basalt`，退出 0 / 1）；在 `internal/app/run.go` 把 `check-key` 加入 `offlineSecretsCommands` 并只在轻路径注入 `MasterKeyCheck`（不取锁、不迁移、不连库）；新建 `internal/cli/secrets_check_key_test.go` 落 `TestCLI_SecretsCheckKeyOffline`；DoD：`0700` 目录 + `0400` 文件通过、同文件放进 `0755` 父目录被拒且退出非 0、不打印密钥内容、错误文案不含密钥字节。
- [x] T009 [US2] 新建 `docker/basalt.worker.yaml`（`database.dsn` / `secrets.master_key_file` / `secrets.auto_init: false` / `worker.name` + `scheduler` / `assets` / `server` / `budget` / `flows` / `embedding` / `providers` 示例段，无 `http` / `callers`）；改 `docker/entrypoint.sh` 增加 `BASALT_ROLE=worker` 分支（`resolve_dsn` / `seed_config` / `require_worker_seed`：预检名册、配置、主密钥文件与父目录权限，通过后停写并 `exec /opt/basalt/basalt gateway`，失败退出 78；控制面缺省仍 `serve`）与 `BASALT_CONFIG_TEMPLATE`；改 `docker/Dockerfile` 增加 `COPY basalt.worker.yaml`、`COPY export-master-key.sh seed-workspace.sh`、预建 `chown 10001:10001 && chmod 0700 /run/basalt-secrets`；扩展 `internal/cli/worker_config_test.go` 落 `TestUS25_Compose_WorkerConfigParsesWithoutControlSecrets`；DoD：模板可独立解析（控制面专属 secret / 变量缺省时仍通过加载）、Worker 分支不写 `instance.json` / 不生成 admin token / 不建 SQLite 库、镜像仍 `USER 10001` 且不换基础镜像。
- [x] T010 [US2] 新建 `docker/compose.multiworker.yaml`（单文件 `profiles: control|worker`；同 profile 的一次性 `basalt-preflight` 预检服务——同镜像、同安全不变量、`read_only`；拒绝空 / `0.0.0.0` 的 `BASALT_DB_BIND` 与缺失的六项入口限流，`postgres` 与 `basalt-control` 以 `depends_on: service_completed_successfully` 挂在它后面；`postgres` + `basalt-control` 挂 `database_dsn` + `postgres_password`；仅 `basalt-control` 发布 8080 并做 `/api/v1/health` 健康检查；`basalt-worker` 跑 `gateway`、无端口、只挂 `database_dsn` secret 与 `master_key` 只读 bind mount 到 `/run/basalt-secrets/master_key`（**不得**走 `/run/secrets`）、健康检查自行解析 DSN 后跑 `basalt workers list`、无自动重启；卷 `postgres-data` / `control-workspace` / `control-private` / `worker-workspace` / `worker-private`；两角色 `user 10001:10001`、`read_only`、`cap_drop: [ALL]`、`no-new-privileges`、`init: true`、tmpfs、无 Docker socket、镜像固定摘要）；新建 `docker/.env.multiworker.example`（`BASALT_IMAGE` / `BASALT_DB_IMAGE` / `BASALT_DB_BIND` / `BASALT_HTTP_BIND` / `BASALT_HTTP_PORT` / `POSTGRES_DB` / `POSTGRES_USER` / 六项 `BASALT_INGRESS_*` / `BASALT_WORKER_NAME`，无密码 / 密钥）；改 `docker/init-secrets.sh` 增 `--copy-from <dir>` / `--db-host <host>`（只复制 `database_dsn` 并改 host，不生成新密码，默认模式逐字不变）；新建 `docker/export-master-key.sh`（控制面私有卷 → 宿主 `docker/secrets/master.key`，`0400`、`10001:10001`，宿主目录 `0700`、`10001:10001`，拒绝覆盖、不打印密钥）；DoD：`docker compose -f docker/compose.multiworker.yaml config` 通过、`BASALT_DB_BIND` 为空或 `0.0.0.0` 被拒绝、只有控制面发布端口、`docker/compose.yaml`（US-18 单实例）零改动。
- [x] T011 [P] [US2] 新建 `docker/seed-workspace.sh`（`seed_export` / `seed_verify` / `seed_import [--force]` / `seed_manifest_write` / `seed_manifest_check` / `seed_path_allowed`）：归档 = `tar` + `seed-manifest.json`（`format_version=1`、`created_at`、`source_instance_id`、`files[] {path,size,sha256}`）；排除配置、SQLite 库、锁文件、日志、`runs/`、`state/*.json`；导入前逐项校验存在性 / `size` / `sha256`，拒绝篡改、截断、`..` / 绝对路径 / 反斜杠绕行、符号链接且目标工作区零落盘；DoD：清单可复算、无热同步路径、导出端源停写前提写入用法说明。
- [x] T012 [P] [US2] 在 `scripts/releasekit/image.go` 提取 `stageImageContext`（暂存 8 个文件：`basalt` / `Dockerfile` / `entrypoint.sh` / `basalt.yaml` / `LICENSE` / `basalt.worker.yaml`（0644）/ `export-master-key.sh` / `seed-workspace.sh`（0755），返回可清理临时目录）；`ImageInputs` 增 `WorkerConfigFile` / `ExportKeyScript` / `SeedScript`（必填，`validate()` 的 `os.Stat` 清单加三项，缺件在 `docker build` 之前报错）；`ImageRecord` 增三个 `json:",omitempty"` SHA-256 字段与 `Validate()` 兼容规则（空 = 旧记录；非空必须 64 位小写十六进制；新构建返回前断言非空）；改 `scripts/releasekit/cmd/releasekit/main.go` 接线；`internal/e2e/us18_stack_test.go` 的 `us18BuildImageOnce` 补同参数；扩展 `scripts/releasekit/image_test.go` 落 `TestUS25_Image_ContextStagesWorkerAssets`；DoD：US-18 既有镜像记录断言不变、不改 SBOM / provenance schema / `release/image-lock.json` / `release/support-matrix.json`。
- [x] T013 [US2] 新建容器级 e2e：`internal/e2e/us25_compose_test.go`（`TestUS25_Compose_TwoRolesAndVolumes` / `WorkerRequiresSeedAndKey` / `DatabasePrivateAddress` / `ReadinessAndRestartPolicy` / `SecurityInvariants`）、`internal/e2e/us25_seed_test.go`（`TestUS25_Seed_ManifestAndTamper`）、`internal/e2e/us25_workers_test.go`（`TestUS25_Workers_BothRegistered`）；DoD：两角色启动与卷 / 端口断言、Worker 缺种子或缺密钥或 `0755` 父目录退出 78 且不写业务表、数据库单宿主回环 / 双宿主私网、健康检查语义（Worker 只表示数据库可达）、安全不变量逐条、两 Worker 各一行且 `InstanceID` 不同且心跳在 TTL 内；容器宿主缺失时 `t.Skip` 登记未验证。

## Phase 5: User Story 3 - 双角色执行权与故障边界 (P1)

**Goal**: 同一计划触发只执行一次；控制面停机后存活 Worker 只接管收尾旧执行且零重放；数据库中断不自动重放；资产缺失准入拒绝；停机先结算后交权再关库。

**Independent Test**: 两 Worker 竞争同一计划时刻断言执行记录恰一条；停控制面后断言旧执行被收尾且零重放、旧持有者回写被拒；中断数据库断言无自动重放；删除 bundle 断言准入拒绝；停机断言审计终态先于执行权释放。

- [x] T014 [US3] 新建 `internal/e2e/us25_schedule_test.go`（`TestUS25_Schedule_ExecutesOnceAcrossWorkers`）与 `internal/e2e/us25_takeover_test.go`（`TestUS25_Takeover_FencesOldHolder`）；DoD：同一 `(task_id, planned_at)` 只一条执行记录、强制 Worker B 执行时租约 owner 是 B 的 `InstanceID`；接管两段——① 回收器 `ClaimExpired → RecoverExecution → fenced FinishExecution` 只收尾（零新 Run、零消息、零模型 / 工具调用、零重放、调用与工具计数不变），旧持有者回写被 fencing 拒绝；② 后续新的 `planned_at` 由存活 Worker 正常领取；Flow 根只由控制面推进、Worker 只回收不推进；有限截止内未完成收尾或回收即失败，报告只记实测耗时、不写产品 SLA。
- [x] T015 [US3] 新建 `internal/e2e/us25_outage_test.go`（`TestUS25_DatabaseOutage_NoReplay`）与 `internal/e2e/us25_asset_test.go`（`TestUS25_AssetMissing_RejectsNewRun`）；DoD：数据库短暂中断恢复后未确认的未知副作用标停止 / 待查、不自动重发、Run 终态与审计一致；bundle 缺失或哈希不符时新 Run 准入被拒、Worker 不执行资产安装或发布。
- [x] T016 [US3] 新建 `internal/e2e/us25_shutdown_test.go`（`TestUS25_Shutdown_ReleasesBeforeClose`）；DoD：停机顺序逐段断言——关监听与停止领取 → drain 期间继续续约 → 到点先取消 → join（各执行先完成审计终态、钩子与投递结算再交还执行权）→ 关观测面 → 释放租约（编排层 no-op）→ Worker 下线 → 关 MCP → 最后关 PostgreSQL，全程在 grace 内。

## Phase 6: User Story 4 - Demo 知识库准备命令 (P2)

**Goal**: `basalt demo workflow prepare` 可预演、可幂等应用；两个集合、九个文档、两个 Agent 只读绑定；冲突拒绝、412 视为计划过期、部分成功退出码 3。

**Independent Test**: 对桩服务运行预演断言零写请求与费用提示；应用后按服务端读回断言逐项一致；第二次断言去重与不变；构造冲突绑定与并发修改断言拒绝与部分成功退出码。

- [x] T017 [US4] 新建 `internal/app/demo_prepare.go` 的只读面：`DemoPrepareDeps` / `DemoPrepareService` / `NewDemoPrepareService`（`WorkDir` / `WorkspaceRoot` 必填，`HTTPClient` nil 建有界默认）/ `resolveEndpoint`（`--server` 缺省从 `http.addr` 派生、通配监听改写同族回环、配置装载失败且未给 `--server` 直接失败；明文 HTTP 只允许 `localhost` / 回环；token 文件 ≤ 4096 字节、去空白非空且不含 NUL / CR / LF / 空格 / `${`；不生成 token）/ `listCollections` / `readAgentFilesETag`（先于正文）/ `readAgentContent` / `bindingConflicts` / `readAdminTokenForClient` / `demoWorkflowDocuments`（固定 9 项，不扫描目录）/ `demoWorkflowBindings` / `buildPlan` / `Plan`（只发 GET、零写入、`Warnings` 含 embedding 调用与费用提示）；新建 `internal/app/demo_prepare_test.go` 落 `TestDemoPrepare_PlanNoWrites` / `TestDemoPrepare_TokenSources` / `TestDemoPrepare_RemotePlainHTTPRejected`；DoD：计划逐项列出集合 / 文档 / 绑定与动作、请求纪律（无重定向、响应 ≤ 2 MiB、超时 65s、错误文案只回 scheme + host）。
- [x] T018 [US4] 在 `internal/app/demo_prepare.go` 落应用面：`createCollection`（409 时重读复用）/ `ingestDocument`（同步，状态取服务端 `active` / `failed`）/ `submitAgentKnowledge`（只提交 `AGENT.md`，`If-Match` 必须是基础集合 ETag，412 上抛“计划过期”不重提）/ `Apply`（写前用读到的正文重判冲突，`workspace.ApplyChanges` 生成正文，不手写 YAML）；扩展 `internal/app/demo_prepare_test.go` 落 `TestDemoPrepare_ApplyBindsAndIngests` / `TestDemoPrepare_SecondRunIdempotent` / `TestDemoPrepare_RefusesConflictingBinding` / `TestDemoPrepare_ETagPinnedAcrossReads` / `TestDemoPrepare_PartialBindingResumable`；DoD：ETag 钉住（读取与提交之间他人改动导致 412 → 该 Agent 记 `stale` 并停止，不用新 ETag 重提旧正文）、第二次幂等（集合复用、文档去重、绑定不变、无新增模型调用）、冲突内容不变且非零退出、`Partial=true` 语义。
- [x] T019 [US4] 在 `internal/cli/demo_workflow.go` 新增 `newDemoWorkflowPrepareCommand`（flags `--server` / `--token-file` / `--yes`）与 `runDemoWorkflowPrepare`（计划总是执行，`--yes` 才应用；逐项输出；部分成功退出 3、硬失败退出 1），挂在既有 `workflow` 组下（`install` 行为不变）；在 `internal/app/run.go` 新增 `newDemoPrepareService` 并注入 `Deps.DemoPrepare`（配置装载后构造；不从数据库取 token）；扩展 `internal/cli/demo_workflow_test.go` 落 `TestCLI_DemoWorkflowPreparePlanNoWrites` / `TestCLI_DemoWorkflowPrepareRequiresYesForWrites`；新建 `internal/app/demo_prepare_integration_test.go`（`integration`）落 `TestDemoPrepare_HTTPFilesAndKnowledge`；DoD：未安装模板 / 未启用角色时直接失败并提示先安装启用、不自动安装；输出不得只凭 HTTP 200 或退出码 0 判成功；`Plan` / `Apply` 两段可分别调用。

## Phase 7: User Story 5 - 迁移与两节点升级 (P2)

**Goal**: M1 九步迁移与对象级核对；M2 单实例 → 多 Worker；两节点升级停全部旧进程、备份、先控制面后 Worker、跨 schema 拒绝、同 schema 整组回滚。

**Independent Test**: 冻结 v6 样例库跑九步迁移断言交接校验与抽样一致；单实例库扩多 Worker 断言身份与计数不变；模拟混合版本与整组回滚断言拒绝与恢复集完整。

- [x] T020 [US5] 新建 `internal/e2e/us25_migration_test.go` 落 `TestUS25_Migration_V6ToV12VerifiesObjects` / `TestUS25_Migration_KnowledgeAbsentRecorded`；DoD：M1 九步全跑通 + `basalt db verify --handoff` 通过；运行 / 审计 / 资源引用 / 技能版本计数与内容抽样一致；源无 `flow_*` / 学习审批 / `kb_*` 对象按“源无对象、目标新建空表”逐项登记；知识引用计数 0 且报告写明原因、文档不出现“知识已迁移”；跨 schema 用旧版本制品验证拒绝；容器宿主缺失时跳过并登记。
- [x] T021 [US5] 扩展 `internal/e2e/us25_migration_test.go` 落 `TestUS25_Migration_SingleInstanceToMultiworker`，新建 `internal/e2e/us25_upgrade_test.go` 落 `TestUS25_Upgrade_TwoNodeAbortAndRollback`；DoD：M2 后生命周期索引与 Agent UID 保留、准入通过、runs / flows / 审计 / 知识计数一致、旧 bundle 可读、非空 Flow 与学习审批完整性在 M2 判定；两节点升级按“停全部旧进程 → 备份 PG + 每个工作区 + 密钥 + 旧二进制 → 新控制面迁移 → 新 Worker”；同 schema（v12）不宣称制品版本门禁、只验停全部旧进程、制品一致与整组回滚。

## Phase 8: User Story 6 - 文档与首发支持矩阵口径 (P3)

**Goal**: 四个文档面同序九锚点、隔离警告五要素、多 Worker 五要素；支持矩阵只写 Compose 多 Worker 与 `LocalRunner`。

**Independent Test**: 对四个文档面跑断言：九锚点顺序一致、隔离警告五要素齐全、多 Worker 五要素齐全；发布说明与运维手册断言否定项，支持矩阵 schema 未变。

- [x] T022 [US6] 新建 `internal/cli/docs_first_run_test.go` 落 `TestDocs_FirstRunChecklistOrder` / `TestDocs_IsolationOptInWarning` / `TestDocs_MultiworkerQuickStartAndSecrets` / `TestDocs_SupportMatrixOnlyComposeMultiworker`（锚点常量作为文档契约一部分）；DoD：红态先行（文档未改时四条逐字红）、断言读取真实文档面（`README.md` / `docs/Operations.md` / `release/package-README.txt` / `basalt.yaml.example` / `release/notes/TEMPLATE.md`）、锚点顺序判定与否定项判定可定位到行。
- [x] T023 [US6] 改写文档面一：`docs/Operations.md` §1.5（删除“不得在全新目录先执行 `basalt init`”；改为 `init` 与首次 `serve` 都可作为第一步；重试续做并补发主密钥；保留“先建集合再绑定”“模板在四角色发布后载入”；新增多 Worker 角色说明）、§5.1（多 Worker 升级顺序与整组回滚）、§5.6（M1 九步 + 知识引用为零核对项）、§6.1（主密钥导出 / 共享与 `stat -c '%u:%g:%a'` 复核，不得放 `/run/secrets`）、新增 §8.3（多 Worker 拓扑 / profiles / 两个 secrets 目录 / DSN 复制 / 种子 / 启动顺序 / 健康检查 / 停止接管 / 升级回滚 / 故障处置）、分布式执行段同步；`README.md` Quick start 增加多 Worker 入口与首配顺序；`docs/GettingStarted.md` 导航指向 §1.5 / §8.3（不复制步骤）；DoD：`TestDocs_*` 对应断言转绿、不写真实双宿主已验证。
- [x] T024 [P] [US6] 改写文档面二：`release/package-README.txt`（步骤 6 固定顺序与 Operations §1.5 同文；步骤 7 改为重试会补主密钥、仍失败走控制台诊断或 `basalt secrets init`；新增多 Worker 安装指向 Operations §8.3）、`basalt.yaml.example`（`secrets` / `worker` / `scheduler` / `assets` 注释、首配顺序注释、容器隔离 opt-in 警告、不写真实密钥 / DSN）、`release/notes/TEMPLATE.md`（部署支持矩阵只写 Compose 多 Worker + `LocalRunner`，明确 K8s / DB HA / 真实双宿主未验证，多 Worker 升级顺序与整组回滚）；DoD：`TestDocs_*` 余下断言转绿、`release/support-matrix.json` schema 与判卷逻辑零改动。

## Phase 9: Polish & Cross-Cutting Concerns

- [x] T025 全量验收：去掉 `--task`、用仓库相对路径传 `--stage` 跑 `.\scripts\check-acceptance.ps1 --stage harness/stages/us25-base.json` / `harness/stages/us25-docs.json`（Windows 原生）与 `--stage harness/stages/us25-compose.json` / `harness/stages/us25-fault.json`（Linux 容器宿主），再跑 `make check` 七目标（`fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build`）；DoD：逐 check 记四态（通过 / 失败 / 跳过 / 未验证）与本轮读数，跳过项进 run-state 未验证清单，`gofumpt -l .` 只读复核无无关文件重排。
- [x] T026 收口三件套：`/speckit-analyze` 复跑（CRITICAL 清零）→ 独立复核（冻结规格 + 判据面 diff，输出 findings 表与未覆盖面清单）→ 七项证据 DoD（门禁逐目标四态、harness 映射逐测试对号、交付物存在性核对、前序模块回归、13 条全局不变量自查、剩余人工项清单、变更总结）；DoD：证据绑定当前源码指纹、run-state 收口结论写明待人工 review ②、不宣称人工已接受。

## Check 映射（stage `us25-base` / `us25-compose` / `us25-fault` / `us25-docs` 派生依据）

| check id | stage 文件 | 本文件任务 |
| --- | --- | --- |
| `us25-base-first-install` | `us25-base.json` | T004、T006 |
| `us25-base-history` | `us25-base.json` | T005 |
| `us25-base-cli` | `us25-base.json` | T004、T008、T009、T019 |
| `us25-base-demo` | `us25-base.json` | T017、T018、T019 |
| `us25-base-releasekit` | `us25-base.json` | T012 |
| `us25-base-race` | `us25-base.json` | T004、T018 |
| `us25-docs` | `us25-docs.json` | T022、T023、T024 |
| `us25-compose-roles` | `us25-compose.json` | T010、T013 |
| `us25-compose-seed` | `us25-compose.json` | T007、T011、T013 |
| `us25-fault-execution` | `us25-fault.json` | T014、T015、T016 |
| `us25-fault-migration` | `us25-fault.json` | T020、T021 |

T001～T003 是准备任务（基线核对、四个 stage 派生、命令面端口类型），不单独绑定某个 check；T025 / T026 是收口任务，覆盖全部四个 stage 与 `make check` 七目标，因此不在上表重复映射。

## 依赖与执行顺序

```text
T001 → T002 → T003
                 ├─→ T004 → T005 → T006 → (T007 待 T013 资产)
                 ├─→ T008 → T009 → T010 → T013 → T007 / T014 → T015 → T016
                 │        └→ T011 [P] ─┘
                 │        └→ T012 [P]（独立于容器阶段）
                 └─→ T017 → T018 → T019
T020 → T021（需容器宿主）
T022 → T023 / T024 [P]（文档面）
T013 → T014 → T015 → T016
T025（全部任务后）→ T026
```

- **故事间顺序**：US1 的 T004～T006 只依赖 T003；T007 是容器级 e2e，显式依赖 T013 的镜像与种子资产（跨故事依赖，模块卡 §4.8 同口径）。
- **US2 内部**：T008（check-key）先于 T009（entrypoint 预检调用它）；T010 依赖 T009 的模板路径；T011 / T012 与 T009 / T010 无文件交集，可并行；T013 依赖 T008～T011。
- **US3 全部依赖 T013** 的双角色编排；T014 → T015 → T016 依次推进（同一容器夹具，避免端口与卷竞争）。
- **US4 内部**：T017（Plan）先于 T018（Apply）；T019 依赖两者。
- **US5 依赖 T013** 的镜像与迁移夹具；T020 先于 T021。
- **US6 无运行期依赖**，可在 T003 后任意时点启动，但 T025 要求 T022～T024 全绿。

## Parallel Example

```text
# 并行组 A（US2 部署资产，无文件交集）
Task: "T011 新建 docker/seed-workspace.sh 与清单校验"
Task: "T012 扩展 scripts/releasekit/image.go 与 image_test.go"

# 并行组 B（US6 文档面二与文档面一）
Task: "T023 Operations / README / GettingStarted"
Task: "T024 release/package-README.txt / basalt.yaml.example / release/notes/TEMPLATE.md"
```

## Implementation Strategy

**MVP first**: US1 + US2（P1，T001～T013）——首次安装链路与双角色部署资产构成“可部署”最小闭环；`us25-base` 与 `us25-compose` 两 stage 先行判卷。

**Incremental delivery**: US3（执行权与故障边界，T014～T016）→ US4（Demo 准备，T017～T019）→ US5（迁移与升级，T020～T021）→ US6（文档口径，T022～T024）→ T025 / T026 收口。每个故事完成即跑对应 stage 的 check，不等全量。

**环境分级**：`us25-base-*` 与 `us25-docs` 在 Windows 原生判卷；`us25-compose-*` / `us25-fault-*` 需要 Docker / Linux 容器或 WSL2，缺环境时 `t.Skip` 并登记未验证，**不得**以源码级证据冒充容器证据。

## Notes

- 本 US **零新增依赖**：`go.mod` / `go.sum` 与起点 `3f8d0dc` 空 diff；不新增迁移 / 表 / 端点 / 契约文件；`SupportedSchemaVersion` 保持 12。
- 不新增进程模式（无 `basalt worker` 子命令）；Worker 复用既有 `gateway` 全量装配。
- 真实双宿主、真实模型、真实 webhook、K8s、DB HA、容器隔离验证不在本 US 自动化范围；全部标未验证。
- 每个任务完成即写 run-state（证据位置 / 未验证项 / 下一条动作）；同一问题修复 3 次不收敛停下走 `/debug`。
- tasks 勾选由控制侧验收后更新；Loop 实现者不得改冻结输入或弱化判据。
