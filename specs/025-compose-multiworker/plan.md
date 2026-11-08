# Implementation Plan: Compose 多 Worker 交付（首次安装补全、双角色编排、迁移与发布口径）

**Branch**: `025-compose-multiworker` | **Date**: 2026-11-08 | **Spec**: [spec.md](spec.md)

**Input**: US-25 模块执行颗粒度文档 R3 定稿（`docs/us/us25-compose-multiworker.md`，review ① 由用户 2026-11-08 授权通过；批准正文 1906 行 / SHA-256 `9b13034e659c48ab85c68b62dcf5c035f4560fed4851cdaef934559c0bde6d64`，当前文件 1917 行 / `22e203fb…`，差异仅 §6.2 通过记录）。Q-25-01～14 全部「接受」。本批准不代表实现或测试已通过。

## Summary

把 US-24 已交付的数据库租约、fencing、调度去重、回收、Flow 根租约与内容寻址资产变成可部署、可安装、可迁移、可复核的双角色 Compose 交付：

1. **首次安装补全**：`basalt init` 在写 SQLite 源库之前先完成首次准备（配置 → `provisioning.pending` → 内置安装意图 → 名册），使 init-first 不再把全新目录判成存量实例；自动建钥资格再加两条——`provisioning.pending` 挂起或全新实例，且四张业务历史表（`runs` / `task_executions` / `agent_lifecycle` / `kb_documents`，不含 `management_operations`）为零；读历史失败落 `await_init`，不吞成“无历史”。
2. **Demo 准备命令**：新增 `basalt demo workflow prepare [--server URL] [--token-file PATH] [--yes]`，只读预演 → 创建两个集合、摄取九个文档、给两个 Agent 写只读知识绑定；用基础集合 ETag 作 `If-Match` 只提交 `AGENT.md`，412 视为计划过期不重提；部分成功退出码 3、第二次运行幂等。
3. **双角色 Compose**：新增 `docker/compose.multiworker.yaml`（单文件 `profiles: control|worker`，控制面 profile 内另有一次性 `basalt-preflight` 在启动前拒绝空 / `0.0.0.0` 的 `BASALT_DB_BIND` 与缺失的六项入口限流）与 `docker/basalt.worker.yaml`；`entrypoint.sh` 按 `BASALT_ROLE=worker` 走 Worker 分支（预检种子与主密钥 → 停写 → 只跑 `gateway`）；Worker 复用既有装配、调度、租约、回收与心跳，无 HTTP 监听、不生成管理面密码、不做首次安装；种子用 `tar` + `seed-manifest.json`（`format_version=1`，逐文件 `path/size/sha256`）人工传输，篡改 / 截断 / 越界路径 / 符号链接一律拒绝。
4. **预检入口**：新增离线命令 `basalt secrets check-key`（复用 `LoadMasterKey`；文件与直接父目录都要 `0700/0400` 私有）；命令自身失败返回非零（0/1），`docker/entrypoint.sh` 的 Worker 预检失败映射退出 78。
5. **迁移与升级**：M1 SQLite v6 → PostgreSQL v12 九步与对象级核对（源无 `flow_*` / 学习审批 / `kb_*` 对象按“源无对象、目标新建空表”登记，知识引用为 0 并写明原因）；M2 单实例 → 多 Worker；两节点升级停全部旧进程、备份、先控制面后 Worker、跨 schema 用旧制品验证拒绝、同 schema 验整组回滚。
6. **发布口径**：releasekit 最小扩展把 Worker 模板与两个部署脚本放进镜像构建上下文并记录摘要；四个文档面（README / Operations §1.5+§8.3 / release 说明 / `basalt.yaml.example`）写同一套首次配置顺序、容器隔离 opt-in 警告与多 Worker 五要素；支持矩阵只写 Compose 多 Worker + `LocalRunner`，`release/support-matrix.json` schema 不动。

零新增 Go / npm 坐标、零新迁移、schema 保持 v12；不改 `docker/compose.yaml`；不新增执行 / 消息 / 审计路径。

## Technical Context

**Language/Version**: Go 构建版本由 go.mod toolchain 指令与 CI setup-go 固定（当前 go1.27.3；go 行 go 1.27 仅为最低要求，本机基础安装 1.27.2 不是构建版本，2026-10-27 实测）；单 module；发布 `CGO_ENABLED=0`。

**Primary Dependencies**: 技术栈固定句（逐字照抄，本 US 零新增坐标）：`Go 构建版本由 go.mod toolchain 指令与 CI setup-go 固定（当前 go1.27.3；go 行 go 1.27 仅为最低要求，本机基础安装 1.27.2 不是构建版本，2026-10-27 实测）+ 单 Go module（cmd/basalt + internal/ 分包）+ spf13/cobra v1.10.2 + goccy/go-yaml v1.19.2 + sashabaranov/go-openai v1.43.0（只做协议调用，禁自动工具循环）+ modernc.org/sqlite v1.60.1 + robfig/cron/v3 v3.0.1（6 段：cron.NewParser(Second|Minute|Hour|Dom|Month|Dow)，每任务时区经 CRON_TZ= 前缀）+ modelcontextprotocol/go-sdk v1.8.0 + fsnotify v1.10.1 + 标准库 net/http + ServeMux + log/slog + testify v1.12.1 + [测试专用] github.com/santhosh-tekuri/jsonschema/v6 v6.0.3（仅 *_test.go 导入；必要传递依赖如 golang.org/x/text 以 go mod tidy 实测为准，运行时不进发布二进制）；凭证 ${ENV_VAR} 占位；不用 ORM（手写 SQL + schema.sql 幂等）；CGO_ENABLED=0；本技术栈句与已批准计划内的依赖直接落地、表外新增坐标或版本变更先问用户；GOPROXY=https://goproxy.cn；写坐标前先 go list -m -versions 核实`。本 US 追加：US-16 的 `github.com/jackc/pgx/v5` v5.11.0（`database/sql` stdlib 模式）沿用；**零新增 Go / npm 坐标**——种子清单哈希用 `crypto/sha256`、tar 归档用 `archive/tar`、随机标识用 `crypto/rand`。`go.mod` / `go.sum` 与本 US 起点（`3f8d0dc`）必须**空 diff**。

**Storage**: PostgreSQL v12 不变，**不新增迁移**。主密钥资格查询复用既有 `runs` / `task_executions` / `agent_lifecycle` / `kb_documents` 四表（`management_operations` 排除），以单条只读 `EXISTS` 查询实现 `HasRuntimeHistory`；若现有索引不足先以 `EXPLAIN` 证明再评估，US-25 不新增 DDL。SQLite 仅作为迁移源（`db bootstrap` / `db migrate`）保持既有 9 步不变。种子归档是普通 `tar` + `seed-manifest.json`，不是数据库对象。

**Testing**: 按模块卡 §3.2 / §4：`internal/app`（`instance_secrets_test.go` / `instance_bootstrap_test.go` 扩展，`demo_prepare_test.go` / `demo_prepare_integration_test.go` 新增）、`internal/storage/postgres/instance_secrets_store_test.go`（新）、`internal/cli`（`init_test.go` / `demo_workflow_test.go` / `worker_config_test.go` 扩展，`secrets_check_key_test.go` / `docs_first_run_test.go` 新增）、`scripts/releasekit/image_test.go` 扩展、`internal/e2e` 的 13 个 `us25_*` 测试文件（其中 2 个夹具文件）共 20 个测试函数。全部使用本地协议桩、冻结夹具与本机 PostgreSQL；不调用真实模型、不注入真实凭据、不连外网。阶段派生 `harness/stages/us25-base.json` / `us25-compose.json` / `us25-fault.json` / `us25-docs.json`（本地专用，不进 `current.json` / CI）；`make check` 七目标仍是完成的定义。

**Target Platform**: Windows 11 + Git Bash 开发机；Linux/amd64 发布目标；`us25-base-*` 可在 Windows 原生跑，`us25-compose-*` / `us25-fault-*` 只在 Linux 容器宿主（Docker / WSL2）跑，缺环境按 `t.Skip` 并登记未验证。

**Performance Goals**: 无跨节点时延 SLA（Q-25-06）。接管测试设有限截止（默认与 US-24 的 TTL + 回收轮询量级一致），截止内未完成收尾或回收即失败；报告只记录实测值，不写产品 SLA。首次安装与 Demo 准备不设吞吐目标；种子导入大小上限由既有工件大小约束。

**Constraints**: 单二进制、无消息队列、无 etcd、无第二套执行路径；Worker 不启 HTTP、不生成管理面密码、不做首次安装；种子只人工传输，不做热同步；文件型 Agent 状态每 Worker 独立、非一致（Q-25-12）；容器隔离（OCI/gVisor）不在本 US 验证范围；`go.mod` / `go.sum` 空 diff；不改已发布迁移与 `docs/contracts/**` 固定字面量；台账 / docs 正文 / `docs/us/` 为私有面。

**Scale/Scope**: 本 US 自动化只覆盖同机双角色（同一 Docker 网络）与模拟两宿主；真实双宿主归首发后 A+B 演示（未验证）。Worker 数量：1 控制面 + 1～2 Worker。

## Constitution Check

*GATE: 实施前与设计后各核对一次。*

| 原则 | 本次落实 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct | 不触碰循环体与工具调度；Worker 运行既有 `gateway` 装配，不新增执行循环 | 通过 |
| II 客户端只做协议调用；唯一消息仓储 | Demo 准备只发管理面 HTTP（集合 / 文档 / `agent.files.update`），复用既有管理审计；不新增消息追加路径 | 通过 |
| III Provider 显式映射 | 不触碰；Worker 装配沿用 US-24 已发布映射 | 通过 |
| IV 一目录一 Agent；技能声明式绑定 | Demo 准备只改写 `AGENT.md` 的 `knowledge:` 绑定，用既有 `ApplyChanges` 与 `SubmitFiles`，不新增绑定机制、不预载技能正文 | 通过 |
| V 审计 day-one fail-closed | 自动建钥的 pending 审计沿用既有预写路径；Demo 准备与管理面调用走既有审计端点；种子导入不写业务表 | 通过 |
| VI 授权与执行分离 | 不触碰 `Policy` / `CommandRunner`；Worker 预检不执行工具 | 通过 |
| VII 同步 + 有界后台例外 | 不新增后台任务类型；Worker 复用 US-24 已批准的四个有界维护循环；首次准备是 `init` 中的同步步骤，不取实例锁 | 通过 |
| VIII 单二进制 | 零新增 Go / npm 坐标；`CGO_ENABLED=0` 发布构建不变；`-race` 与发布构建分属不同命令；镜像只多三个静态资产 | 通过 |
| IX 手写 SQL、迁移只追加 | 不新增迁移，`HasRuntimeHistory` 是只读 `EXISTS` 查询；`schema.pg.sql` 冻结基线不改 | 通过 |
| X 五大能力优先、范围卡紧 | 只交付已批 US-25 范围；K8s、数据库 HA、真实双宿主、S3/NFS、容器隔离适配、自动重启全部明确不做 | 通过 |

**依赖方向**：`cli` 只追加消费方端口与报告类型（`FirstInstall` / `DemoPrepare` / `MasterKeyCheck`），不 import `app`；`app` 新增 `first_install.go` / `demo_prepare.go` 适配器与装配，`demo_prepare.go` 只依赖 `cli` 类型、`core` 与标准库（无数据库句柄）；`storage/postgres` 新增只读查询；`docker/` 与 `scripts/releasekit` 为构建 / 部署面；`docs` 为文本交付。`core` 不新增代码。

**设计后复核**：Phase 1 产物（`data-model.md` / `contracts/README.md` / `quickstart.md`）未引入新依赖、新公开写端点、新动作字面量或第二套执行 / 审计路径；`FirstInstallReport` 是纯只读结构，`DemoPreparePlan` / `Result` 是 CLI 侧类型，不泄漏 `app` / `postgres` 类型。结论维持「通过」，无 Complexity Tracking 项。

## Project Structure

### Documentation (this feature)

```text
specs/025-compose-multiworker/
├── plan.md              # 本文件
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── contracts/README.md  # Phase 1 输出（指向权威契约）
├── spec.md              # 已审规格（review ① 派生）
├── checklists/requirements.md
├── run-state.md         # 过程状态（非设计工件）
└── tasks.md             # Phase 2 输出（/speckit-tasks 生成）
```

### Source Code (repository root)

```text
internal/cli/
├── ports.go                 # 追加：FirstInstallReport / FirstInstall / DemoPrepareRequest / Demo*Plan|Result / DemoPrepare / MasterKeyCheck 端口（纯 cli 侧类型）
├── deps.go                  # 追加：FirstInstall FirstInstall；DemoPrepare DemoPrepare；MasterKeyCheck MasterKeyCheck
├── init.go                  # 修改：Workspace.Init 之后、Store.Bootstrap 之前调用 deps.FirstInstall.Prepare；nil 报装配错误
├── demo_workflow.go         # 修改：新增 prepare 子命令（--server / --token-file / --yes）与 runDemoWorkflowPrepare
├── secrets.go               # 修改：新增 check-key 子命令（离线；门禁读文件与父目录）
├── docs_first_run_test.go   # 新增：TestDocs_FirstRunChecklistOrder / IsolationOptInWarning / MultiworkerQuickStartAndSecrets / SupportMatrixOnlyComposeMultiworker
├── secrets_check_key_test.go# 新增：TestCLI_SecretsCheckKeyOffline
├── init_test.go             # 扩展：TestCLI_InitPreparesFirstInstallBeforeBootstrap / TestCLI_InitNilFirstInstallFailsClosed
├── demo_workflow_test.go    # 扩展：TestCLI_DemoWorkflowPreparePlanNoWrites / RequiresYesForWrites
└── worker_config_test.go    # 扩展：TestUS25_Compose_WorkerConfigParsesWithoutControlSecrets

internal/app/
├── first_install.go         # 新增：firstInstallAdapter.Prepare → ensureNewInstanceConfig；firstInstallReport 字段映射
├── demo_prepare.go          # 新增：DemoPrepareService（Plan / Apply / resolveEndpoint / listCollections / createCollection / ingestDocument / readAgent* / bindingConflicts / submitAgentKnowledge / readAdminTokenForClient / demoWorkflowDocuments）
├── demo_prepare_test.go     # 新增：TestDemoPrepare_*（8 条 + -race 组合之一）
├── demo_prepare_integration_test.go # 新增（integration）：TestDemoPrepare_HTTPFilesAndKnowledge
├── instance_secrets.go      # 修改：SecretsFacts 三字段；EvaluateSecrets create 支；gatherFacts 调 pending 与 HasRuntimeHistory
├── instance_secrets_test.go # 扩展：TestFirstInstall_SecretsEligibility / ExistingInstanceNeverMints / RecoveryNeverCreates / KeyCreationAudited
├── instance_bootstrap_test.go# 扩展：TestFirstInstall_PrepareOrderAndIdempotency / ConcurrentStartOneWinner / TestUS25_FirstInstall_HalfWrittenRosterFailsClosed
├── run.go                   # 修改：deps 构造注入 FirstInstall / DemoPrepare / MasterKeyCheck；offlineSecretsCommands 加 check-key
└── first_install_test.go（或并入 instance_bootstrap_test.go）# 按 tasks 落点

internal/storage/postgres/
├── instance_secrets_store.go      # 新增：HasRuntimeHistory + runtimeHistoryTables
└── instance_secrets_store_test.go # 新增：TestRuntimeHistory_Tables / ManagementOperationsExcluded

docker/
├── compose.multiworker.yaml # 新增：单文件 profiles control / worker；一次性 basalt-preflight 预检；双角色共享网络；数据库私网绑定
├── basalt.worker.yaml       # 新增：Worker 单文件配置模板（不含控制面专属 secret）
├── export-master-key.sh     # 新增：控制面侧从托管密文导出主密钥到 0700 目录
├── seed-workspace.sh        # 新增：打 tar + seed-manifest.json（format_version=1）；导入端校验
├── Dockerfile               # 修改：COPY Worker 模板与两个脚本；预建 0700 /run/basalt-secrets
├── entrypoint.sh            # 修改：BASALT_ROLE=worker 分支（预检 → 停写 → exec gateway）
└── init-secrets.sh          # 修改：--copy-from / --db-host（只复制 database_dsn 并改 host）

scripts/releasekit/
├── image.go                 # 修改：提取 stageImageContext；ImageInputs 加三必填；ImageRecord 加三 omitempty 摘要
├── image_test.go            # 扩展：TestUS25_Image_ContextStagesWorkerAssets
└── cmd/releasekit/main.go   # 修改：flags 与 ImageInputs 接线

internal/e2e/
├── us25_first_install_test.go # 新增（e2e）
├── us25_compose_test.go       # 新增（e2e）
├── us25_seed_test.go          # 新增（e2e）
├── us25_workers_test.go       # 新增（e2e）
├── us25_schedule_test.go      # 新增（e2e）
├── us25_takeover_test.go      # 新增（e2e）
├── us25_outage_test.go        # 新增（e2e）
├── us25_asset_test.go         # 新增（e2e）
├── us25_shutdown_test.go      # 新增（e2e）
├── us25_migration_test.go     # 新增（e2e）
└── us25_upgrade_test.go       # 新增（e2e）

README.md / docs/Operations.md（§1.5 改写 + 新增 §8.3）/ release/package-README.txt / basalt.yaml.example  # 四文档面
harness/stages/us25-base.json / us25-compose.json / us25-fault.json / us25-docs.json
```

**Structure Decision**: 沿用单 Go module + `internal/` 分包，不新增 Go 包；`first_install.go` / `demo_prepare.go` 落在 `internal/app`（装配层），CLI 侧只加消费方端口与命令；`docker/` 与 `scripts/releasekit/` 为部署与构建面；`internal/e2e` 复用既有 `e2e` build tag 与 US-18 容器夹具。`core` 不新增代码，`app` 仍是唯一装配层。

## Implementation sequence

1. **首次安装端口与 init 顺序**：`cli/ports.go` 的 `FirstInstallReport` / `FirstInstall`；`cli/deps.go` 字段；`cli/init.go` 在 `Workspace.Init` 与 `Store.Bootstrap` 之间调用；`app/first_install.go` 适配 `ensureNewInstanceConfig`；`run.go` 注入。判据：`TestCLI_InitPreparesFirstInstallBeforeBootstrap`、`TestCLI_InitNilFirstInstallFailsClosed`、`TestFirstInstall_PrepareOrderAndIdempotency`。
2. **主密钥资格**：`SecretsFacts` 三字段；`EvaluateSecrets` 的 create 支加“零历史 + auto_init +（fresh 或 pending）”；`gatherFacts` 调 `adminTokenProvisioningPending` 与 `HasRuntimeHistory`；`postgres` 的 `HasRuntimeHistory` + 四表清单。判据：`TestFirstInstall_SecretsEligibility` / `ExistingInstanceNeverMints` / `RecoveryNeverCreates` / `KeyCreationAudited`、`TestRuntimeHistory_Tables` / `ManagementOperationsExcluded`。
3. **Demo 准备服务与命令**：`cli/ports.go` 的 `Demo*` 类型与 `DemoPrepare` 端口；`app/demo_prepare.go` 的 Plan / Apply；`cli/demo_workflow.go` 的 prepare 子命令；`run.go` 注入。判据：`TestDemoPrepare_*` 8 条 + `TestCLI_DemoWorkflowPrepare*`，含 ETag 钉住、412 不重提、部分成功退出码 3、第二次幂等。
4. **Worker 离线预检入口**：`cli/ports.go` 的 `MasterKeyCheck`；`cli/secrets.go` 的 `check-key`；`run.go` 的 `offlineSecretsCommands` 与注入。判据：`TestCLI_SecretsCheckKeyOffline`（`0700` 目录 + `0400` 文件通过，`0755` 父目录被拒）。
5. **双角色 Compose 与部署资产**：`docker/compose.multiworker.yaml`、`basalt.worker.yaml`、`export-master-key.sh`、`seed-workspace.sh`；`Dockerfile` / `entrypoint.sh` / `init-secrets.sh` 修改；`cli/worker_config_test.go` 解析测试。判据：`TestUS25_Compose_WorkerConfigParsesWithoutControlSecrets`、`TestUS25_Compose_*`、`TestUS25_Seed_*`（容器宿主）。
6. **releasekit 最小扩展**：`image.go` 提取 `stageImageContext`、三必填输入与三 `omitempty` 摘要；`cmd/releasekit/main.go` 接线。判据：`TestUS25_Image_ContextStagesWorkerAssets`；US-18 既有镜像构建测试保持绿。
7. **双角色执行与故障边界**：容器级 `TestUS25_Workers_BothRegistered` / `Schedule_ExecutesOnceAcrossWorkers` / `Takeover_FencesOldHolder` / `DatabaseOutage_NoReplay` / `AssetMissing_RejectsNewRun` / `Shutdown_ReleasesBeforeClose`。判据：两段接管（只收尾 + 新计划时刻正常领取）、有限截止、零重放、旧持有者回写被拒。
8. **迁移与升级**：`TestUS25_Migration_V6ToV12VerifiesObjects` / `KnowledgeAbsentRecorded` / `SingleInstanceToMultiworker` / `Upgrade_TwoNodeAbortAndRollback`（容器宿主）。判据：九步 + 交接校验 + 对象计数 / 抽样 + 源无对象登记 + 整组回滚。
9. **四个文档面与文档断言**：README / Operations §1.5 改写与新增 §8.3 / release 说明 / `basalt.yaml.example`；`TestDocs_*` 四条。判据：九锚点顺序一致、隔离五要素、多 Worker 五要素、支持矩阵只含 Compose 多 Worker 与 `LocalRunner`。
10. **收尾**：逐任务 stage → 四个 stage + `make check` → 收口三件套（analyze 复跑 → 独立复核 → 七项证据）→ 人工 review ②。

## Complexity Tracking

> 无 Constitution 违规项，无需填写。

| 违反项 | 为什么需要 | 更简单方案被拒的原因 |
| --- | --- | --- |
| 无 | — | — |
