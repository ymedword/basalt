# run-state — US-25 Compose 多 Worker 交付

> 1. 本文件是**过程状态，非设计工件**——speckit 命令**不得**把它当设计输入。
> 2. 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）。
> 3. 唯一允许原地翻转的是未验证清单的「未验证 → 已验证」，翻转必须注日期与证据。

## 1. 基线

| 项 | 值 |
| --- | --- |
| US 编号 / 短名 | US-25 / `compose-multiworker` |
| 模块类型 | 开发类（台账 `docs/us/REGISTRY.md` US-25 行，2026-11-08 登记） |
| 起始 commit | `3f8d0dcf656298cddc7cdf188b806f3369e264f6`（US-24 收尾 docs 记录提交，local = origin/main = 远端 head） |
| Feature 分支 | `025-compose-multiworker`（2026-11-08 由 `3f8d0dc` 手动创建；`.specify/extensions.yml` 无 `before_specify` hook） |
| worktree | `D:\code\basalt`（主工作区，非 worktree） |
| 开跑时未提交改动 | 仅 `?? specs/025-compose-multiworker/`（本 US 新建的派生工件目录）；受保护既有改动：无 |
| 平台 | Windows 11 + Git Bash；PostgreSQL 夹具 127.0.0.1:55432（WSL） |

### 模块卡人工 review ① 的真实决定

| 项 | 值 |
| --- | --- |
| 决定 | 2026-11-08 用户明确「us-25 人工 review1 我授权通过」——批准 R3 定稿、登记与范围内实施 |
| 批准正文 | `docs/us/us25-compose-multiworker.md`，1906 行 / SHA-256 `9b13034e659c48ab85c68b62dcf5c035f4560fed4851cdaef934559c0bde6d64` |
| 当前文件 | 1917 行 / SHA-256 `22e203fb2ef72cdeaafa02f637ad87993642ddc0dee1afb70a4939f4d91b9973`；与批准版的差异仅 §6.2 review ① 通过记录（含批准行本身），无设计正文改动 |
| Q 裁决 | Q-25-01～14 全部「接受」（用户 2026-11-08）；明细见模块卡 §6.4 |
| 决策授权 | 「us25 的登记和开发需要决策的地方自动选择你推荐的选项，我只在 us25 最后收尾人工走查时介入」 |

### 前序改造点接缝核对（步骤 1，软门禁）

2026-11-08 用 5 个只读核查代理（workflow `us25-seam-verify` / run `wf_2bf51525-7b9`，193 次工具调用）逐项核对 P-01～P-18 对当前代码（HEAD `3f8d0dc`）：

| 条目组 | 条目 | 结论 |
| --- | --- | --- |
| A 命令行与初始化 | P-01～P-04 | 全部 UNCHANGED |
| B 密钥判定 | P-05～P-08 | 全部 UNCHANGED |
| C Agent / 工作区 | P-09～P-11 | 全部 UNCHANGED |
| D 容器 | P-12～P-14 | 全部 UNCHANGED |
| E 文档 / harness / releasekit | P-15～P-18 | 全部 UNCHANGED |

无 DRIFTED、无 MISSING，模块卡 §2.5 无需修订。实施时须注意的实测细节：

- P-18 `Deps.MasterKeyCheck` / `masterKeyCheckPort` 当前**均不存在**，属**新增**（模块卡已按新增标注，不是修改）；`offlineSecretsCommands` 现为 `{recover-record, adopt-legacy}`。
- P-12 Worker 分支依赖 P-18 的 `secrets check-key` 先落地；Dockerfile 需新增 Worker 模板 COPY、预建 0700 `/run/basalt-secrets`，并经 P-17 暂存保证 US-18 镜像构建保持绿。
- P-15 的 §8.3 与 `TestDocs_*` 当前不存在，属本 US 新增；`docs/Operations.md:137` 的「不得先执行 `basalt init`」段落需按卡改写。
- P-09 的 `AgentAdmin.SubmitFiles` 在 US-24 后追加了资产发布调用（FR-24-42），属增量不属漂移。
- P-19 组的 `init_test.go:43` 需补 `FirstInstall` 桩；`TestScope_NoMultiWorkerClaim` 扫描字段名，新字段不触发其判据。

### 本 US 允许触碰的文件范围

模块卡 §3 交付物清单 + §2.5 前序改造点 P-01～P-18：

- 新建：`docker/compose.multiworker.yaml`、`docker/basalt.worker.yaml`、`docker/export-master-key.sh`、`docker/seed-workspace.sh`、`internal/cli/demo_workflow_prepare.go`（或同义文件，以 tasks 为准）、`harness/stages/us25-base.json`、`us25-compose.json`、`us25-fault.json`、`us25-docs.json`
- 修改：`internal/cli/{deps,ports,init,secrets,run?}.go`、`internal/app/{instance_secrets,run,wire}.go`、`internal/storage/postgres/instance_secrets_store.go`、`docker/{Dockerfile,entrypoint.sh,init-secrets.sh}`、`scripts/releasekit/{image.go,cmd/releasekit/main.go}`、四个文档面、`harness/acceptance.json`（**不改**）、`release/support-matrix.json`（**不改**）
- 不触碰：`docker/compose.yaml`（US-18 单实例）、`go.mod` / `go.sum`（零新增依赖）、schema 与迁移

### 三件输入指纹（步骤 5 冻结，2026-11-08）

| 输入 | 路径 | SHA-256 / 行数 |
| --- | --- | --- |
| 模块卡 | `docs/us/us25-compose-multiworker.md` | `22e203fb2ef72cdeaafa02f637ad87993642ddc0dee1afb70a4939f4d91b9973` / 1917 行（批准正文 `9b13034e659c48ab85c68b62dcf5c035f4560fed4851cdaef934559c0bde6d64` / 1906 行，其后仅追加 §6.2 通过记录） |
| spec | `specs/025-compose-multiworker/spec.md` | `f2642e4de8101a2ed06b09757929b3ee8f5f16b1e8d12b97f1c273a1aa101483` / 237 行 |
| plan | `specs/025-compose-multiworker/plan.md` | `2f311be4f3d7e2e2037a1d431c9b6162609b4aeb5686c3428e43229404b04801` / 158 行 |
| tasks 正文指纹 | `specs/025-compose-multiworker/tasks.md` | `600534b27e7441bb5fb572466f93e0d973688bc466fc25d4bded169bb2b229d4` / 150 行（勾选位归一后同值：全部未勾选） |

### AI 任务比对与冻结（步骤 5，2026-11-08）

- 任务清单 ↔ 模块卡 §3 逐项对齐：§3.1.1 → T003/T004；§3.1.2 → T005/T006；§3.1.3 → T003/T017/T018/T019；§3.1.4 → T008/T019；§3.1.5 → T012；§3.2 全部 45 条测试逐一落点（app 首次安装 3 + 密钥 4、storage 历史 2、cli init 2 / demo 2 / check-key 1 / worker 配置 1 / docs 4、demo 计划应用 8 + 集成 1、releasekit 1、e2e 首次安装 3 / compose 5 / seed 1 / workers 1 / schedule 1 / takeover 1 / outage 1 / asset 1 / shutdown 1 / migration 3 / upgrade 1）；§3.3 → T002/T009/T010/T011/T023/T024；§3.4 → T005；§3.5 → T023/T024；§3.6 → Notes 逐条。
- 无缺失、无超范围新增；T001/T025/T026 为流程任务（基线与收口），不属交付物扩张。测试任务与实现同任务落地并注明红态先行（`TestCLI_InitPreparesFirstInstallBeforeBootstrap`、`TestDocs_*`、`TestFirstInstall_SecretsEligibility` 等关键拒绝路径）。
- stage 映射：11 个 check id 全部落入四个 stage 文件，`Check 映射` 节给出 check → 任务对照；公共检查引用 `harness/acceptance.json`，未复制定义。
- 结论：**齐**（无缺项 / 无多余范围项），冻结上述三件输入与 tasks 正文指纹；后续输入变化只允许追加式修订并重跑 analyze。

### 步骤 5 执行记录（T001 / T002，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T001 基线 | 分支 `025-compose-multiworker`、HEAD `3f8d0dcf656298cddc7cdf188b806f3369e264f6`、`git status --porcelain` 仅 `?? specs/025-compose-multiworker/`；模块卡 SHA-256 与批准正文一致；台账 US-25 行已登记（§1 已记） |
| T002 stage 文件 | `harness/stages/us25-base.json` `846544c8…`、`us25-compose.json` `0b997e18…`、`us25-fault.json` `c1f2083a…`、`us25-docs.json` `802b5b58…` |
| 派生依据 | 11 个 check 逐条照抄模块卡 §4.8 的命令参数、超时、`required_tests`、`level=source`；`tasks` 按 tasks.md「Check 映射」（T004/T005/T006/T008/T009/T012/T017/T018/T019 → base；T022/T023/T024 → docs；T007/T013 → compose；T014/T015/T016/T020/T021 → fault） |
| 装载验证 | 四份各跑 `go run ./scripts/acceptance -stage harness/stages/<id>.json -task ZZZ`，均到达 `unknown stage task`（即 `loadStage` 全项通过：version / ID / approval / inputs 与 protect 存在性 / required_tests 命名 / task→check 引用 / 非项目门禁）；四 stage 均为本地专用，未进 `current.json` 或 CI |
| `protect` 口径 | 只列**创建时已存在**的 §4.9 路径 + `docs/contracts/**` 十一个文件 + `harness/acceptance.json`（loader 对每条路径做 `Lstat`，与 `us24-*.json` 先例一致）；本 US 新建的 §4.9 文件（`first_install.go` / `demo_prepare.go` / `master_key_check.go` / `demo_prepare_test.go` / `docs_first_run_test.go` / `secrets_check_key_test.go` / `instance_secrets_store_test.go` / `us25_*.go` / `compose.multiworker.yaml` / `basalt.worker.yaml` / `seed-workspace.sh` / `export-master-key.sh`）在文件落地后、T025 全量前追加进四份 stage 的 `protect` 并复核装载 |
| 勾选指纹 | tasks.md 勾选 T001/T002 后原始哈希 `185213cb…`；勾选位归一哈希仍 `600534b2…`（与冻结值一致，无正文改动） |
| 未验证 | 11 个 check 尚未执行（对应测试与实现尚未落地）；容器宿主（Docker / Linux 容器或 WSL2）与本地 PG 夹具未在本条核验 |

### 步骤 6 执行记录（T003–T005，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T003 端口类型 | `internal/cli/ports.go` 追加 `FirstInstallReport` / `FirstInstall`、`DemoPrepareRequest` / `DemoPreparePlan` / `DemoPrepareResult` 一族、`MasterKeyCheckReport` / `MasterKeyCheck`，签名逐字取模块卡 §3.1.1 / §3.1.3 / §3.1.4；`internal/cli/deps.go` 追加 `FirstInstall` / `DemoPrepare` / `MasterKeyCheck` 三字段（nil = 装配缺件，不得回落） |
| T003 验证 | `CGO_ENABLED=0 go build ./...` 通过；`go test -count=1 ./internal/cli ./internal/app` 绿——`cli` 不 import `app`（报告类型分开定义），既有命令零行为变化由既有用例守卫 |
| T004 实现 | `internal/app/first_install.go` 新增无状态 `firstInstallAdapter`：`Prepare` 调 `ensureNewInstanceConfig(workDir, programRuntimeRoot())`（既有、不改），非 fresh 时额外 `readInstanceMarker` 一次，半截 / 不可解析名册失败关闭（FR-25-05 / C-04，`ensureNewInstanceConfig` 只判文件存在）；`internal/cli/init.go` 在 `Workspace.Init` 之后、`Store.Bootstrap` 之前调 `Prepare`（nil → 装配错误、不建库），输出「首次安装：新建 配置 / 初始化进度 / 实例名册」或「已就绪，零写」；`internal/app/run.go` 唯一一处 `cli.Deps{}` 字面量注入 `FirstInstall: firstInstallAdapter{}`，两条构造路径共用 |
| T004 测试 | `internal/cli/init_test.go` 追加 `TestCLI_InitPreparesFirstInstallBeforeBootstrap` / `TestCLI_InitNilFirstInstallFailsClosed`；`internal/app/instance_bootstrap_test.go` 追加 `TestFirstInstall_PrepareOrderAndIdempotency` / `TestFirstInstall_ConcurrentStartOneWinner` / `TestUS25_FirstInstall_HalfWrittenRosterFailsClosed`（含工作区逐字节快照与 8 并发单 winner） |
| T004 红态实测 | 临时把 `Prepare` 块移到 `Bootstrap` 之后 → `TestCLI_InitPreparesFirstInstallBeforeBootstrap` 逐字红（expected `[workspace prepare bootstrap]` / actual `[workspace bootstrap prepare]`）；恢复原位后 `go test -count=1 ./internal/cli ./internal/app` → `ok internal/cli 18.585s` / `ok internal/app 23.033s`，`git diff --stat -- internal/cli/init.go` 仅 +39 行 |
| T005 实现 | `internal/storage/postgres/instance_secrets_store.go` 新增包内 `runtimeHistoryTables = [...]string{"runs", "task_executions", "agent_lifecycle", "kb_documents"}` 与 `HasRuntimeHistory(ctx)`：单条只读 `SELECT EXISTS(..) OR …`（首行短路，不整表入内存），任一读失败返回错误不吞成 false；不新增迁移 / 索引 |
| T005 测试 | 新建 `internal/storage/postgres/instance_secrets_store_test.go`（`integration`）：`TestRuntimeHistory_Tables` 四表逐一子用例（空库 false → 播种 true，顺序与清单逐项比对，取消上下文必须报错）+ `TestRuntimeHistory_ManagementOperationsExcluded`（只插 `management_operations` pending 行仍 false，再插 run 变 true） |
| T005 实测 | DSN 经 `dev-env/secrets/postgres-test.clixml` 在 PowerShell 进程内解密注入（密码未打印、未落盘），`go test -count=1 -tags=integration ./internal/storage/postgres -run '^(TestRuntimeHistory_)' -v` → 5 个子用例全 PASS，`ok internal/storage/postgres 2.029s` |
| 未验证 | T006 尚未实施（判定表扩展前，`TestUS25_FirstInstall_HalfWrittenRosterFailsClosed` 的 `EvaluateSecrets` 断言只覆盖既有字段）；T007 e2e 依赖 T013 镜像与容器宿主，未执行；本 US 全部 check 尚未运行（按 stage 依赖关系留到共享 check 的任务齐备后） |

### 步骤 6 执行记录（T006，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T006 实现 | `SecretsFacts` 追加 `ProvisioningPending` / `RuntimeHistory` / `RuntimeHistoryErr`（既有字段与 D-40 语义不变）；`EvaluateSecrets` 的"无记录、无密钥"分支改为 §2.3.3 判定表：零密文之后依次判读历史失败 → 有历史 → `auto_init` 关闭 → `fresh` 或 `pending` → create，其余 await_init；`gatherFacts` 调 `adminTokenProvisioningPending(s.deps.WorkDir)` 与 `Store.HasRuntimeHistory(ctx)`，读失败写 `RuntimeHistoryErr`（不吞成 false、不提前返回错误）；新增 `uninitializedDecision` 收敛三元组（纯重构，行为不变） |
| T006 测试 | `internal/app/instance_secrets_test.go` 追加 `TestFirstInstall_SecretsEligibility`（判定表 8 格：fresh / pending 续做 / 历史优先 / 读失败 / auto_init 关闭 / 无资格 / 密文恢复态 / 就绪态不变）、`TestFirstInstall_ExistingInstanceNeverMints`（有历史不铸钥、删历史恢复 create、`init` 的 pending 续做）、`TestFirstInstall_RecoveryNeverCreates`（密文无记录无密钥、有记录缺密钥两个恢复子例）、`TestFirstInstall_KeyCreationAudited`（预写失败零密钥零记录；成功路径一条 success 且 operation_id 一致）；既有 `TestInstanceSecrets_ExistingInstanceNotAutoInit` / `TestInstanceSecrets_FreshInstanceStillAutoInitializes` 保留 |
| 夹具调整 | 资源夹具为准入预置一条 `agent_lifecycle` 行，会使"新实例自动铸钥"用例被新规则判成存量库；`newInstanceSecretsFixture` 与 `TestInstanceSecrets_AdminTokenSeparateFile` 先调 `clearBusinessHistory` 还原"从未跑过业务"的空库状态（四表逐表判据仍在 postgres 包） |
| 实测 | `CGO_ENABLED=0 go build ./...`、`go vet -tags=integration ./internal/app ./internal/cli ./internal/storage/postgres` 均无输出；`go test -count=1 -tags=integration ./internal/app -run` 匹配 `TestFirstInstall_` 与 `TestInstanceSecrets_` 前缀 → `ok internal/app 25.520s`（全部 PASS，含 4 个新用例与既有 secrets 套件）；`go test -count=1 ./internal/app ./internal/cli` → `ok 25.200s` / `ok 18.711s`；`gofumpt -l` 对全部改动文件零输出 |
| 实施顺序决定 | T007 的 DoD 明确"执行前置 T013 的镜像与种子资产"，当前无可运行的容器制品；按依赖顺序留到 T013 之后实施，届时与 `us25-compose` stage 一起跑，不先写空壳用例 |
| 未验证 | T007 未实施；`us25-base-*` stage 尚未运行（`us25-base-first-install` 依赖 T006 与 T007 同 check，`us25-base-cli` 依赖 T008/T009/T019） |

### 步骤 6 执行记录（T008，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T008 实现 | 新建 `internal/app/master_key_check.go`：无状态 `masterKeyCheckPort.Check(path, workspaceRoot)` 调 `LoadMasterKey` + `MasterKeyFingerprint`，`defer key.Zero()` 后返回 `cli.MasterKeyCheckReport`（只有 KeyID / Fingerprint，无密钥字段）；`internal/cli/secrets.go` 在 `secrets` 组挂 `check-key`（`--path` 必填、`--workspace` 缺省 `<WorkDir>/.basalt`），成功打印 `key_id` + 指纹、失败 `refuseCommand(exitValidation)` → 退出 1，nil 端口报离线端口缺件；`internal/app/run.go` 把 `"check-key"` 加入 `offlineSecretsCommands`（不取锁、不迁移、不连库），并把 `Deps.MasterKeyCheck` 注入改到轻路径分支 |
| T008 测试 | 新建 `internal/cli/secrets_check_key_test.go` 落 `TestCLI_SecretsCheckKeyOffline`（5 个子用例：缺省工作区推导、显式覆盖、端口失败退出 1 且不回显密钥、缺 `--path` 零调用、nil 端口装配错误）；新增 `internal/app/master_key_check_test.go` 落 `TestMasterKeyCheck_ReusesLoadKeyPermissions`（真实 `LoadMasterKey` 两侧判据：`0700`+`0400` 通过、同文件 `0755` 父目录拒绝并点名 0700、工作区内路径拒绝、报告与错误均不含 base64 密钥）；`internal/app/run_test.go` 的 `TestRun_OfflineSecretsCommandsSkipHeavyPath` 追加 `check-key` 不进重路径断言 |
| 测试归属说明 | 权限语义的实测放在 `internal/app`（新增测试，模块卡未逐条列举）：`cli` 不得 import `app`（端口报告类型分开正是这条依赖方向的守卫），命令面用例只能用桩端口；`TestMasterKeyCheck_*` 在 Windows 按平台跳过（权限位为合成值，DACL 由既有 Windows 用例判） |
| 实测 | `CGO_ENABLED=0 go build ./...` 与 `go vet ./internal/cli ./internal/app` 无输出；`go test -count=1 ./internal/cli -run '^TestCLI_SecretsCheckKeyOffline$'` → 5 个子用例全 PASS；`go test -count=1 ./internal/cli ./internal/app` → `ok internal/cli 19.422s` / `ok internal/app 24.868s`；`gofumpt -l` 对 6 个改动文件零输出；`markdownlint-cli` 对 `specs/025-compose-multiworker/run-state.md` 零诊断（工具经控制探针验证可检出错误） |
| 未验证 | `us25-base-cli` stage 尚未运行（同 check 内 `TestUS25_Compose_WorkerConfigParsesWithoutControlSecrets` 依赖 T009、`TestCLI_DemoWorkflow*` 依赖 T019）；`TestMasterKeyCheck_*` 的 Unix 分支本机（Windows）跳过，留待 CI / Linux 执行 |

### 步骤 6 执行记录（T009，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T009 实现（模板） | 新建 `docker/basalt.worker.yaml`：`database.dsn`、`secrets.master_key_file`、`secrets.auto_init: false`、`worker.name` 四处占位，`server` / `budget` / `flows` / `scheduler` / `assets` 显式值，`embedding` / `providers` 以注释示例给出（引用了模型连接与凭据，写死占位会让没有这些变量的 Worker 装载失败）；**无** `http` / `callers` 两段 |
| T009 实现（入口） | `docker/entrypoint.sh` 增 `BASALT_ROLE`（缺省 `control`）、按角色选 `BASALT_CONFIG_TEMPLATE`（Worker 为 `/opt/basalt/basalt.worker.yaml`）、`require_worker_seed`（名册存在、名册非空、配置存在、`basalt secrets check-key` 通过；主密钥路径缺省 `/run/basalt-secrets/master_key` 并导出供配置占位解析）、零参数时 Worker 缺省 `gateway`；控制面分支与 US-18 逐字一致（`resolve_dsn` → `seed_config` → `serve`） |
| T009 实现（镜像） | `docker/Dockerfile` 增 `COPY basalt.worker.yaml`、`COPY export-master-key.sh seed-workspace.sh`、预建 `mkdir -p /run/basalt-secrets && chown 10001:10001 && chmod 0700`，并把两个脚本并入 `chmod 0755`；基础镜像、`USER 10001`、`CMD ["serve"]` 不变 |
| T009 测试 | `internal/cli/worker_config_test.go` 落 `TestUS25_Compose_WorkerConfigParsesWithoutControlSecrets`：只给 `BASALT_DATABASE_DSN` / `BASALT_MASTER_KEY_FILE` / `BASALT_WORKER_NAME` 三个变量装载真实模板，断言装载通过、`auto_init` 为 false、主密钥路径、Worker 名、`http.addr` 为空、`ingressMissing` 非空、`ValidateServeIngress` 报错、`worker` / `scheduler` / `assets` 三段落 §3.3 缺省 |
| 设计裁决（名册"可解析"） | 模块卡 P-12 要求预检**不得自行解析 JSON**，而 `require_worker_seed` 的伪代码写"名册可解析"。核对后：现有轻命令里没有读名册的入口（`instance` 组只有 `adopt` 且会写；`isolation check` 仅在启用隔离时才读名册），`check-key` 的契约按 §3.1.4 只判密钥文件与直接父目录。因此脚本只判**存在与非空**（半截写入的常见形态），JSON 语义归 Go 解析器，字节完整性由种子的逐文件 SHA-256 清单（T011）保证；不新增命令、不扩大 `check-key` 契约 |
| 逆改控制探针 | 往模板尾部临时追加 `http.addr: ${BASALT_HTTP_ADDR}` → 新用例红，错误点名"配置项 http.addr 引用的环境变量 BASALT_HTTP_ADDR 不存在"；还原后复绿，证明该用例真的在判"无控制面专属占位" |
| 实测 | `sh -n docker/entrypoint.sh` 通过；`go test -count=1 ./internal/cli` → `ok internal/cli 17.434s`；`CGO_ENABLED=0 go build ./...` 与 `go vet -tags=integration ./internal/cli ./internal/app` 无输出；`gofumpt -l` 对改动文件零输出 |
| 未验证 | 镜像构建此刻**必然失败**：`docker/Dockerfile` 已 COPY `export-master-key.sh`（T010）与 `seed-workspace.sh`（T011），两者尚未创建，按模块卡的交付顺序留到 T013 与容器用例一起验证；entrypoint 的 Worker 分支（退出 78、不写名册、不建库）与 `USER 10001` 断言同属容器级判据，归 `TestUS25_Compose_WorkerRequiresSeedAndKey`（T013）；`us25-base-cli` stage 仍未运行（同 check 内还依赖 T019） |

### 步骤 6 执行记录（T010，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T010 实现（编排） | 新建 `docker/compose.multiworker.yaml`：`postgres` / `basalt-control`（profile `control`）、`basalt-worker`（profile `worker`）；五卷；控制面挂 `database_dsn` + `postgres_password`（逐字按 §3.3），Worker 只挂 `database_dsn` secret + `./secrets/master.key:/run/basalt-secrets/master_key:ro`；控制面 `command: ["serve"]`、发布 `${BASALT_HTTP_BIND:-127.0.0.1}:${BASALT_HTTP_PORT:-8080}:8080`；Worker `command: ["gateway"]`、无 `ports`、无 `restart`、无 `depends_on` |
| T010 实现（环境模板） | 新建 `docker/.env.multiworker.example`：§3.3 列出的 13 个变量，无密码 / 密钥；注明 B 宿主只需 `BASALT_IMAGE` / 库名用户 / `BASALT_WORKER_NAME` |
| T010 实现（DSN 复制） | `docker/init-secrets.sh` 增 `--copy-from` / `--db-host`（成对出现，否则退出 64）；复制模式只写 `database_dsn`、不生成密码、不碰 `postgres_password`、拒绝覆盖；默认模式代码逐行未改（仅 `check_stat` 上移） |
| T010 实现（主密钥导出） | 新建 `docker/export-master-key.sh`：`export_master_key <compose-project> <dest-file>` 与 `verify_key_ownership <file>` 两个函数 + 直接执行守卫（被 `source` 时只定义函数）；从 `<project>_control-private` 卷的 `<root>/basalt/instances/*/master.key` 取键，**必须恰好一个**否则失败并列出候选路径；目标目录 `0700`、文件 `0400`、属主 `10001:10001`，逐项 stat 复核；拒绝覆盖；全程不回显密钥字节 |
| 设计裁决（预检服务） | Compose 只能插值、不能对取值做断言：`${VAR:?}` 挡不住 `0.0.0.0`；且插值发生在**按 profile 过滤之前**，把 `:?` 写进控制面服务会让 `--profile worker` 的渲染一并失败（实测：stderr 报 `required variable … is missing`，stdout 为空）。因此新增同 profile 的一次性 `basalt-preflight`（同一镜像、同安全不变量、`read_only`），`postgres` 与 `basalt-control` 用 `depends_on: service_completed_successfully` 挂在它后面——预检不过则数据库与控制面都不启动，端口不会先发布后失败（N-25-14 的“拒绝启动”） |
| 设计裁决（`$$` 转义） | 实测确认 Compose 对 `$NAME` 与 `${NAME}` **都**做插值（首版预检脚本把宿主值烘进了命令文本）。预检脚本里所有留给容器 shell 的 `$` 一律写成 `$$`；`$$2` / `$$value` 等位置参数与循环变量同样处理 |
| 设计裁决（PG 回环发布） | 编排把 PostgreSQL 发布到 `${BASALT_DB_BIND:-127.0.0.1}:5432`，空值落回环而不是交给 Docker；FR-25-14 / C-07 允许“不发布**或**只绑回环”，选后者是为了单宿主取证（psql 核对计数）与双宿主同一份文件 |
| 设计裁决（`postgres_password` 位置） | 按 §3.3 逐字：控制面两个服务都挂 `database_dsn` + `postgres_password`；Worker 只挂 `database_dsn`。应用进程不读 `postgres_password`，它是 PostgreSQL 容器 entrypoint 的 `POSTGRES_PASSWORD_FILE` |
| 设计裁决（DSN 切分） | 主机改写用纯参数展开，切分点是**唯一的** `@`；出现多个 `@` 一律失败关闭（密码里未编码的 `@` 会把密码截断成半截——执行矩阵里实测到过，已修）。主机 / 端口 / 路径 / 查询串分别覆盖 |
| 实测（编排渲染） | WSL2 Ubuntu + Docker 29.9.2 / Compose v5.6.1：`--profile worker config` 只渲染 `basalt-worker`（`ports: None`，主密钥为 `read_only` bind）；`--profile control config` 渲染 `basalt-preflight` / `postgres` / `basalt-control`，仅控制面 8080 与 PostgreSQL 回环 5432 两个 ingress 端口；两次 `EXIT:0`。PG 镜像摘要与 `docker/compose.yaml`、`release/image-lock.json` 逐字一致；`git status docker/compose.yaml` 无输出（US-18 零改动） |
| 实测（预检判据） | 从 `config --format json` 取出预检脚本（`$$` 还原为 `$`）后跑五组：`127.0.0.1` 通过、`10.0.0.12` 通过、`0.0.0.0` 退出 1 并点名暴露、空值退出 1、缺一项 ingress 退出 1 |
| 实测（凭据脚本） | `sh -n` 对 `entrypoint.sh` / `init-secrets.sh` / `export-master-key.sh` 均通过。`init-secrets.sh` 在 WSL root 下跑九组：带端口 + 查询串复制（密码 `%40` 原样保留、主机改写、`10001:10001:400`、无 `postgres_password`）、无端口无查询串、重复执行拒绝覆盖、`0.0.0.0` 拒绝、非法字符拒绝、单 flag 退出 64、无 `@` 拒绝、多个 `@` 拒绝、空主机段拒绝、默认模式仍生成两文件且属主权限不变 |
| 未验证 | `basalt-preflight` 的真实容器执行（需先构建镜像，T013）；`export-master-key.sh` 的 `docker volume inspect` 分支（需真实控制面卷与 root，随 §6.1 人工走查 / T017 之后的容器夹具）；镜像构建仍缺 `docker/seed-workspace.sh`（T011）而必然失败 |

### 步骤 6 执行记录（T011，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T011 实现（脚本） | 新建 `docker/seed-workspace.sh`（POSIX sh）：`seed_export <src> <archive>`、`seed_verify <archive>`、`seed_import <archive> <target> [--force]`、`seed_manifest_write`、`seed_manifest_check`、`seed_path_allowed`，另有 `seed_list` / `seed_stage` / `seed_archive_entry_ok` / `seed_path_shape_ok` / `seed_path_excluded` / `seed_instance_id` 六个内部 helper；`${0##*/}` 守卫使直接执行走子命令、被 `source` 时只定义函数（对应模块卡 §4.6.5 的 `runSeedFn` 用法） |
| T011 实现（归档内容） | 只收 `.basalt/` 子树里的**普通文件** + 归档根的 `seed-manifest.json`；`basalt.yaml`、SQLite 库（含 `-wal`/`-shm`/`-journal`）、`basalt.lock`、`logs/`、`runs/`、`agents/*/state/`、临时文件按 C-10 排除；`.basalt/assets/` 随种子走（FR-25-22 的 bundle 可读前提） |
| T011 实现（清单） | `seed-manifest.json`：`format_version=1`、`created_at`（UTC `Z`）、`source_instance_id`、`files[] {path,size,sha256}`；一条一行、键顺序固定、条目间行尾逗号；`python3 -m json.tool` 对独立生成与归档内两份清单均通过（生成的是合法 JSON，不只是"我们的解析器认得"） |
| 设计裁决（清单解析） | 解析侧**不用**通用 JSON 解析器（镜像与宿主都不保证有 jq），也不用正则：awk 按字面键名 `","size":` / `,"sha256":"` 顺序切段（`index()` + `substr()`），段值再逐项校验（路径无引号、size 全数字、hash 64 位小写十六进制）。`files[]` 区段里的行数与成功解析数不等即失败——清单里出现任何"看不懂的行"都按篡改处理 |
| 设计裁决（条目校验两侧） | 导出侧：符号链接、非普通文件、名字含空白 / 引号 / 反斜杠 / 控制字符的成员一律失败关闭；导入侧：`tar -tf` 先过一遍条目（允许 `./` 前缀、目录尾斜杠与根清单，其余必须匹配 `.basalt/` 形态），`../` 与绝对路径在解压前就被拒 |
| 设计裁决（排除项按整路径） | `.staging` / `.backup` 是**目录**（`internal/workspace/recovery.go:44-45` 的 `agents/<name>.staging`、`.backup`），只比对 basename 会把目录里的正文放进来；改为整条路径匹配 |
| 设计裁决（`instance_id` 只作元数据） | 产品形态是 `inst_` + 16 位小写十六进制（`internal/app/instance_identity.go:51`），但脚本只收 `[A-Za-z0-9._:-]{1,64}` 且要求**恰好命中一次**：种子脚本不重复产品的标识语义校验，值只进清单，且不允许引号 / 斜杠，既不破坏 JSON 也不能被拼成路径（模块卡 §4.6.5 的样例名册是 `i_1`） |
| 设计裁决（属主：警告而非失败） | 导入成功后尝试 `chown -R 10001:10001` 与 `chmod -R a+rX`；用 `stat` 复核，不是 `10001:10001` 就打警告并给出补救（以 root 执行 / 换 Linux 原生路径），**不**改变退出码——归档内容的正确性与宿主权限能力是两件事，L3 用例以普通用户跑 `t.TempDir()` 时也不应因此变红 |
| 设计裁决（零落盘与幂等） | `seed_stage` 全程在 `mktemp -d` 里做：清单核对通过**之后**才复制到目标；目标已有 `.basalt/instance.json` 时拒绝（`--force` 才覆盖）；归档已存在时 `seed_export` 拒绝覆盖 |
| 实测（函数矩阵） | WSL2 Ubuntu（`sh` = dash）跑 29 条：`sh -n`；导出 → 归档条目清单（3 个排除类目逐项不在归档里、`basalt.yaml` 不在）；清单逐条 `sha256sum` 复算；`verify` 通过；`import` 落盘 7 个文件；重复 import 拒绝、`--force` 通过；篡改一个字节（大小不符）与同长度篡改（sha256 不符）分别拒绝；截断拒绝；`../outside.txt` 条目拒绝；夹带清单外文件拒绝；源工作区含符号链接时导出拒绝且不产出归档；归档内符号链接拒绝；**失败时目标目录零条目**；归档已存在拒绝覆盖；无名册拒绝导出；文件名含空格拒绝导出；`seed_manifest_write` 独立调用（不带 list 参数）产出 6 条；`source` 无副作用。29/29 通过 |
| 实测（drvfs 属主路径） | 以 uid 1000 导入到 `/mnt/d/...`：`stat` 为 `1000:1000`，脚本打印属主警告与补救说明，退出码仍为 0；探针目录用完即删 |
| 未验证 | 真实实例工作区的种子（需控制面首启产物，T013）；把种子导入**命名卷**的一次性容器路径（T013 / §6.1 人工走查）；`us25-compose-seed` stage 未运行（同 stage 还含 T007 的首启用例） |

### 步骤 6 执行记录（T012，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T012 实现（暂存提取） | `scripts/releasekit/image.go` 新增 `func (in ImageInputs) stageImageContext() (string, func(), error)`，把原 `BuildImage` 内联块整体搬出；暂存 8 个文件（`basalt` 0755 / `Dockerfile` 0644 / `entrypoint.sh` 0755 / `basalt.yaml` 0644 / `LICENSE` 0644 / `basalt.worker.yaml` 0644 / `export-master-key.sh` 0755 / `seed-workspace.sh` 0755），失败时自行 `RemoveAll`，成功返回 cleanup；`BuildImage` 改为 `defer cleanupContext()` |
| T012 实现（输入与校验） | `ImageInputs` 增 `WorkerConfigFile` / `ExportKeyScript` / `SeedScript`；`validate()` 的必填循环与 `os.Stat` 清单各加三项——缺件 / 空值都在 `docker build` 之前失败（`validate` 是 `BuildImage` 第一条语句） |
| T012 实现（记录字段） | `ImageRecord` 增 `WorkerConfigSHA256` / `ExportKeySHA256` / `SeedScriptSHA256`（`json:",omitempty"`，键名 `worker_config_sha256` / `export_key_sha256` / `seed_script_sha256`）；`Validate()` 对三者逐个判“空 = 旧记录放行，非空必须 64 位小写十六进制”（新增 `isLowerHex64`，比既有 `isHex64` 多一条大小写判定）；`BuildImage` 在 `Validate()` 之后显式断言三者非空，防止新构建借兼容规则漏记 |
| T012 实现（接线） | `scripts/releasekit/cmd/releasekit/main.go` 的 `runImageBuild` 增 `--worker-config` / `--export-key-script` / `--seed-script`（缺省与 `--dockerfile` 同目录）；`internal/e2e/us18_stack_test.go` 的 `us18BuildImageOnce` 补齐三个参数，US-18 镜像证据重建路径与判据不变 |
| T012 实测（新用例） | `TestUS25_Image_ContextStagesWorkerAssets`：8 文件内容逐字比对、权限位逐项（非 Windows 宿主）、多余文件即失败、`cleanup` 后目录消失；三个新工件分别缺失 / 空值时 `validate()` 与 `BuildImage`（未触 docker）均点名报错；旧记录（三摘要为空）仍通过、带三个小写摘要的记录通过、大写 / 非法 / 65 位三种形态分别点名拒绝 |
| 设计裁决（摘要大小写） | 既有六个摘要用 `isHex64`（收大写），三个新摘要按模块卡 §3.1.5 收紧为**小写**：新记录只由本函数以小写落盘，大小写混写只可能是手改 |
| 设计裁决（兼容判定范围） | 按模块卡逐字段判“空 / 非空”，不额外要求三者同空或同满：旧记录天然三项全空，新构建由 `BuildImage` 单独断言全满，`Validate` 只判形态不判组合 |
| 实测（包级） | `go test ./scripts/releasekit/... -count=1` 全绿（含 `TestUS18_Image_*` 八个既有用例原样通过）；`go vet -tags=integration ./scripts/... ./internal/e2e/...` 与 `staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 -tags=integration ./scripts/...` 零输出 |
| 未验证 | 真实 `docker build` 带 8 文件上下文的构建（需容器宿主，T013 的 `TestUS25_Compose_*` 与 US-18 栈用例）；`us25-base-releasekit` stage 未运行（同 stage 还含 T007/T013 的容器用例） |
| DoD 核对 | SBOM / provenance schema 未改（`buildImageSBOM` / `BuildImageProvenance` 零改动）；`release/image-lock.json`、`release/support-matrix.json` 零改动；US-18 记录断言（`validImageRecord`）与 `TestUS18_Image_*` 全部保持通过 |

### 步骤 6 执行记录（T013，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T013 实现（编排与角色） | 新建 `internal/e2e/us25_compose_test.go`：`TestUS25_Compose_TwoRolesAndVolumes`（两角色启动、端口只属控制面、卷与只读 bind、无 docker socket、卷内名册不变）、`TestUS25_Compose_WorkerRequiresSeedAndKey`（`noseed` / `nokey` / `widekey` 三个负向服务各自退出 78 且日志点名各自理由；正向服务 0700 父目录 + 0400 文件起得来；容器内 `basalt secrets check-key` 跑通；负向路径不写 `worker_nodes` / `runs` / `task_executions`）、`TestUS25_Compose_DatabasePrivateAddress`（loopback / private / empty-defaults / zero-rejected / private-accepted / missing-ingress-rejected 六个子项）、`TestUS25_Compose_ReadinessAndRestartPolicy`、`TestUS25_Compose_SecurityInvariants` |
| T013 实现（种子与两 Worker） | 新建 `internal/e2e/us25_seed_test.go`：真实脚本导出 / 校验 / 成员清单 / 清单逐项复算 / 同长度单字节篡改（sha256 判据）/ 截断 / `traversal` / `absolute` / `symlink` / `extra-file` 四类拒导；归档含 `.basalt/assets/**`（内容寻址资产随种子分发）。新建 `internal/e2e/us25_workers_test.go`：两 Worker 各一行、`InstanceID` 互异且 `wrk_` + 32 位小写十六进制、名称按配置落库、心跳在 30s 内、容器内 `workers list` 与 `--json` 各见两行、三个卷内名册 `instance_id` 均为控制面那一份 |
| 实测（us25-compose-roles） | WSL2 Ubuntu + Docker：`TestUS25_Compose_TwoRolesAndVolumes` 58.43s、`WorkerRequiresSeedAndKey` 32.57s、`DatabasePrivateAddress` 4.60s（子项 0.21–0.81s）、`ReadinessAndRestartPolicy` 55.15s、`SecurityInvariants` 36.25s、`TestUS25_Workers_BothRegistered` 30.38s；包 `ok 221.860s`，0 失败 0 跳过 |
| 实测（us25-compose-seed） | `TestUS25_Seed_ManifestAndTamper` 10.97s（四类拒导子项 1.09–1.20s）；同包另跑 T007 三条（读数见下节）；包 `ok 106.384s`，0 失败 0 跳过 |
| 修复（workers list 表头） | 容器内 `workers list` 把启动日志（`出网受控拨号已装配`）和数据行写在同一流；原断言按非空行计数把日志行算进表格 → 改为先定位 `INSTANCE` 表头、再判表头后恰好两行。逆改点不变：坏实现（共用 InstanceID / 心跳不续 / 少一行）仍逐条红 |
| 事实（`serve` 也是 Worker） | `internal/app/run.go` 的 `needsSchedules` 覆盖 `serve` 与 `gateway`，两条路径都走 `startWorkerRuntime` 并登记 `worker_nodes`。因此 `WorkerSeedSkipsBootstrap` 判「控制面行 + 本 Worker 新增行 = 2」，「恰好两行」的用例先清空名册再起两个 Worker；心跳是 UPDATE、不回插，清空后不会被控制面补回 |
| 分工（C-11 的 verify 缺口） | C-11 的「Worker 只 `assets verify`、无 `assets install` / 重建 / 发布指针推进」断言不在 T013 落地：它与其反例是同一条资产准入面，随 `TestUS25_AssetMissing_RejectsNewRun`（T015）一起判——届时用 `volumeTree` 判种子导入前后 `.basalt/assets/**` 字节不变、且 Worker 不写发布指针。T013 已判种子含资产且逐项 sha256 可复算 |
| 残留（宿主目录权限） | 夹具的宿主密钥目录是 `0755`（drvfs 保不住 0700），容器用例只断言容器内父目录 `10001:10001:700` 与文件 `10001:10001:400`；`export-master-key.sh` 对宿主目录设 `0700/10001:10001` 的那条由 T010 的脚本函数矩阵覆盖，不在容器用例内重复断言 |
| DoD 核对 | 容器宿主缺失时 `us25NewStack` 走 `t.Skip`（Linux-only，`REQUIRE_DOCKER=1` 时改为硬失败）；本轮在 WSL2 Ubuntu 实际执行，无跳过项；不删断言、不放宽阈值 |

### 步骤 6 执行记录（T007，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T007 实现 | 新建 `internal/e2e/us25_first_install_test.go`：`TestUS25_FirstInstall_InitThenServe`（全新卷先 `init` 再 `serve`）、`TestUS25_FirstInstall_RetryAfterIngressExit`（六项 `BASALT_INGRESS_*` 全空 → 监听前退出 → 补配重试续做）、`TestUS25_FirstInstall_WorkerSeedSkipsBootstrap`（Worker 只登记） |
| 实测 | `InitThenServe` 41.45s、`RetryAfterIngressExit` 21.31s、`WorkerSeedSkipsBootstrap` 28.16s，全部 PASS（`us25-compose-seed` 包 `ok 106.384s`） |
| 事实（`init` 的输出面） | 容器里入口脚本先按角色播种配置（`seed_config` 从不覆盖），`init` 本次真正新建的是「初始化进度 + 实例名册」，输出逐项为「首次安装：新建 初始化进度 / 实例名册」与「已创建（schema v6）」——断言按实测字面量落地，不把 `config_created=true` 写成预期 |
| 事实（续做语义） | 首次 `serve` 失败后工作区保留名册与 `provisioning.pending`，不铸钥、不发密码、不建 SQLite 源库（`serve` 从不建源库，那是 `init` 的职责）；补配后第二次启动沿用同一 `instance_id`，进度走到 `minted`，日志出现「首次初始化未完成：配置与实例名册已在盘上」与「实例加密：主密钥已生成并写入初始化记录」 |
| 事实（Worker 首启边界） | Worker 从种子启动：名册 `instance_id` 与控制面一致、不建 `basalt.db`、私有卷没有 `master.key` / `admin-token` / `provisioning.json`、日志不含控制面首次安装语义、卷内 `/basalt.yaml` 无 `http:` / `callers:` 段 |
| 修复（主密钥模式） | 原断言把 C-08 的 `0400` 套到卷内主密钥上 → 实测卷内发布的写出口径是 `0o600`（`instance_secrets_publish.go` 以 `O_EXCL` + `0o600` 写出）；断言改为实测形态 `10001:10001:600`，并在注释里写明 `0400` 只属 `export-master-key.sh` 的宿主副本 |
| DoD 核对 | 三条 DoD 逐项有断言：三件同在（`/basalt.yaml` / `/.basalt/instance.json` / `/.basalt/basalt.db`）、准备先于建库（`init` 输出与 `TestCLI_InitPreparesFirstInstallBeforeBootstrap` 分工：本文件判结果、CLI 单测判顺序）、重试补发主密钥、Worker 不写名册 / 不铸钥 / 不建库；无 `t.Skip` |

### 步骤 6 执行记录（T014，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T014 实现（调度去重） | 新建 `internal/e2e/us25_schedule_test.go`：`TestUS25_Schedule_ExecutesOnceAcrossWorkers` 把同一段到期扫描 → `ClaimDueTrigger` → 执行行 + 租约的路径在三种宿主组合上各跑一次——A 只有控制面（`serve` 自身也是 Worker）、B 只有 Worker（控制面已停）、C 两者同时在线；每段判 `(task_id, planned_at)` 只一行（`assertSingleExecution`）、owner 是真实 `worker_nodes.instance_id`、租约 owner/epoch/released 三读数、三次上弦的 planned_at 两两不同 |
| T014 实现（接管两段） | 新建 `internal/e2e/us25_takeover_test.go`：`TestUS25_Takeover_FencesOldHolder`——`docker pause` 冻结在模型调用中途的控制面 → Worker 上线 → 回收器认领过期租约并**只收尾**（执行行 failed、Run `failed:worker_lost`、未确认调用 pending→unknown、租约 owner=Worker 且 epoch=2、`reclaimed` 事件逐字命中）；零重放指纹 `runs\|messages\|llm_calls\|tool_invocations` 逐项相同；解冻后旧 holder 以「租约失权，停止发起新动作」+「执行行收尾失败」被 fencing；段 ② 新的 planned_at 由存活 Worker 领取并产生**真实 Run**，`flow_tasks`=0 |
| 实测（us25-fault-execution 的 T014 部分） | WSL2 Ubuntu + Docker：`TestUS25_Schedule_ExecutesOnceAcrossWorkers` PASS 74.74s、`TestUS25_Takeover_FencesOldHolder` PASS 73.71s，包 `ok 152.535s`，0 失败 0 跳过，退出码 0 |
| 修复（`schedules:` 格式串） | 探针 AGENT.md 由 `fmt.Sprintf` 生成，模板写成 `%schedules:`——`%s` 吃掉后留下字面量 `chedules`，frontmatter 多出一个未知顶层键、`schedules` 块整段丢失（容器日志 WARN `detail=chedules`），`scheduled_tasks` 一行都没有，两用例都卡在 `waitProbeTaskRow`（读数 `0:-`）。改为 `%sschedules:` 后任务行按期望出现 |
| 修复（Worker 侧不得重写 AGENT.md） | 原 `prepareWorkerForProbe` 复用控制面脚本，在种子导入**之后**又把**未绑定**的 `AGENT.md` 写进 Worker 卷，覆盖掉种子里带 `resource_bindings` UID 的已发布定义；准入的 `checkDefinitionMatchesBundle` 逐字节比对工作副本与 bundle 内同名文件，从此必然不符，报固定资产原因（`Cause` 只经 `Unwrap`，日志与 `error_message` 都看不到）→ 阶段 B 执行行 failed 而探针 Run 数为 0。拆出只写 `basalt.yaml` 的 `us25ProbeConfigScript`；控制面首启前仍用带未绑定定义的那份（那时还没有资源 UID，随后由安装覆盖） |
| 补强（段 ② 断言） | takeover 段 ② 原先只判执行行终态 failed——准入被拒的行同样 failed，用例会在"领到但拒之门外"时误绿。补 `probeRunID`（无 Run 即红）+ Run 必须离开 running，使「由在运行 Worker 正常领取执行」有真实执行证据 |
| 事实（首启不自动发布 + 实例锁全程持有） | 新 Agent 没有 `agent_bundle` 发布指针时首启不自动发布；`serve` / `gateway` 全程持有实例锁，写模式的 `assets install` 只能在停机窗口执行。夹具因此固定「起 → 停 → 装 → 再起」，这条同时是 FR-25-22「Worker 不执行资产安装 / 发布」的物理边界 |
| DoD 核对 | 三段宿主组合各判唯一性 + owner；接管两段含零重放、fencing、真实 Run、`flow_tasks`=0；deadline 有限（90s / 3min / 5min），报告只记实测耗时、不写产品 SLA。未删断言、未放宽阈值；容器宿主缺失时 `us25NewStack` 走跳过，本轮在 WSL2 实跑无跳过 |

### 步骤 6 执行记录（T015，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T015 实现（资产准入） | `internal/e2e/us25_asset_test.go`：`TestUS25_AssetMissing_RejectsNewRun` —— 正对照（哈希一致的 bundle 正常跑起来）→ 改一字节 → 新执行行在**执行权**上被拒 → 删 bundle → 同样被拒；被拒行不产生 Run、不发模型调用 |
| T015 实现（数据库中断） | `internal/e2e/us25_outage_test.go`：`TestUS25_DatabaseOutage_NoReplay` —— 调用挂起时 `docker stop` PostgreSQL、控制面进程不重启；续约失败累计到 `ttl - renew_interval`（30s−10s）有界自停 → 恢复后 pending 调用归一为 `unknown`、执行行 failed、Run 失权类终态、零重放指纹逐项相同、租约已释放且 owner 不变 |
| 实测 | WSL2 Ubuntu + Docker，`go test -tags=e2e -run '^TestUS25_(DatabaseOutage\|AssetMissing)_'` 退出码 0；asset 段实测日志「执行行 2 / 3 在执行权上被拒（resource unavailable: us25-probe … run `basalt assets install`）」、outage 段「执行行 1 收尾 failed、Run 收尾失权类终态、调用改判 unknown、零重放、租约已释放」；无 FAIL 行、无 `t.Skip` |
| 修复（夹具 `/data` 前缀） | asset test 最初把 volume 相对路径与容器绝对路径混用，一次性脚本里出现 `/data/data/...`，篡改与删除都落在错路径上，用例在「应当拒绝」处读到相反结果。改为：`volumeHas` / `volumeRead` / `volumeStat` 取 volume 相对路径，`execInVolume` 与一次性脚本取容器绝对路径 `/data/...` |
| 修复（Run 终态判据过窄） | outage 原断言只接受 `failed:worker_lost`——实测走的是活进程有界自停路径，Run 收尾 `cancelled:renew_failed`。判据改为「失权类终态之一」，两条路径都写进注释；仍拒绝 completed / running，不放宽到任意终态 |
| 修复（租约代次判据过窄） | 原断言要求 `epoch == 2`（回收器接管）。实测自停路径由活进程交还自己的租约：owner 不变、代次仍为 1、`released_at` 已置。判据改为 owner 不变 + 已释放 + 代次 ∈ {1,2}，仍拒绝「仍被持有」与「换了 owner」 |
| 事实（自停先于回收） | 长中断（超过 TTL）下活进程在 `ttl − renew_interval` 上停止发起后续动作，以 `cancelled:renew_failed` 收尾并释放自己的租约；恢复后的回收器 therefore 不再认领该行（实测代次 1、`released_at` 非空）。`failed:worker_lost` 只在进程被冻结 / 来不及自停时出现（T014 的 `docker pause` 场景）。两条都是 §2.3.6 允许的路径 |
| DoD 核对 | 资产准入：缺失与哈希不符两种拒绝都在执行权上、零模型调用（FR-25-22）；中断：不重放、unknown 归一、失权终态带原因、进程身份不变（FR-25-21）。未删断言、未放宽阈值；容器宿主缺失时走跳过，本轮 WSL2 实跑无跳过 |

### 步骤 6 执行记录（T017–T019，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T017 实现（只读面） | `internal/app/demo_prepare.go`：`DemoPrepareService` / `NewDemoPrepareService` / `resolveEndpoint`（缺省从 `http.addr` 派生、通配监听改写回环、明文 HTTP 只许回环、token 文件 ≤4096 且拒 NUL/CR/LF/空格/`${`）/ `listCollections` / `readAgentFilesETag`（先于正文）/ `readAgentContent` / `bindingConflicts` / `demoWorkflowDocuments`（固定 9 项）/ `demoWorkflowBindings` / `buildPlan` / `Plan`（只发 GET、`Warnings` 按待摄取份数计费提示） |
| T018 实现（应用面） | 同文件：`createCollection`（409 重读复用）/ `ingestDocument`（同步，状态取服务端 `active`/`failed`）/ `applyBinding`（重读 ETag → 重判冲突 → `workspace.ApplyChanges` → 条件 POST，412 记 `stale`）/ `Apply`（`Partial` 语义） |
| T019 实现（命令面与装配） | `internal/cli/demo_workflow.go`：`newDemoWorkflowPrepareCommand`（`--server`/`--token-file`/`--yes`）+ `runDemoWorkflowPrepare`（计划总是执行、`--yes` 才应用、逐项输出、部分成功退出 3、硬失败 1）；`internal/app/run.go` 注入 `Deps.DemoPrepare`；`internal/cli/demo_workflow_prepare_test.go` 两条命令面用例 |
| T019 实现（真实 HTTP 集成） | `internal/app/demo_prepare_integration_test.go`（`integration`）`TestDemoPrepare_HTTPFilesAndKnowledge` / `TestDemoPrepare_HTTPMissingAgentFailsClosed`：真实 web 服务 + 真实 PostgreSQL + 本地 `/embeddings` 桩（真实模型禁用）；覆盖计划零写入 → 第 3 次向量调用失败 → 逐项结果与独立 API 复核（失败文档无激活版本、版本行 `failed`/`embed_failed`）→ 重试只补 1 份且恰好 1 次向量调用 → 第三次幂等零写入零调用 → 冲突绑定部分成功（`Partial`、内容不变）→ 恢复后重绑；缺角色直接失败并提示 `basalt demo workflow install`、零建集合、零向量调用 |
| 实测 | WSL2 Ubuntu：`dev-env/scripts/with-postgres.sh go test -count=1 -tags=integration -timeout=15m ./internal/app -run '^TestDemoPrepare_'` 退出码 0；11 条用例全 PASS（含 stage 要求的 9 条 + 2 条新增），无 FAIL、无 `t.Skip`；`GOPROXY=https://goproxy.cn go vet -tags=integration ./internal/app` 无输出、`gofumpt -l` 无输出 |
| DoD 核对 | 未安装模板 / 未启用角色时直接失败并提示先安装启用、**不自动安装**（FR 断言零建集合 + 零向量调用）；成功判定按逐项 `status` 与独立 API 读数，不凭 HTTP 200 / 退出码 0；`Plan` / `Apply` 两段可分别调用；未删断言、未放宽阈值 |
| 夹具事实 | 失败摄取**不激活版本**：`kb_documents.latest_version_id` 保持空，文档列表里该行 `latest_status` 为空串；失败终态在 `kb_document_versions.status = failed` 与 `error_kind = embed_failed` 上（`GET /documents/{id}/versions` 可读） |

### 步骤 6 执行记录（T016，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T016 实现（停机段序） | `internal/e2e/us25_shutdown_test.go`：`TestUS25_Shutdown_ReleasesBeforeClose` —— 探针任务执行中给控制面发 SIGTERM，按 FR-25-23 的九段逐段断言：① 健康端点先关而容器仍 `running`；② drain 期间新探针不产生第二条执行行（停止领取）；③ 排空 36s 内续约仍在（`renewed_at > signal+2000ms`、`released_at` 空、执行行仍 `started`）；④ 到点取消 + join：执行行收尾 `failed`、`llm_calls` 0:1 pending→failed_timeout 且计数不变；⑤ 属主释放租约（owner 不变、epoch 1、已释放、无 `reclaimed` 事件）；⑥ Worker 下线（`worker_nodes.stopped_at` 非空）；⑦ 退出码 0、耗时 ∈ [33s, 80s]，日志含「停机排空超过 flows.shutdown_grace_seconds」与「msg=停机完成」、不含「停机超出 worker.shutdown_grace_seconds」/「释放租约的停机预算已耗尽」/「调度器停机超时」；⑧ 审计收尾：Run 为 `cancelled:` 或 `running:`（客户端断开路径），`scheduled_tasks.last_status = failed`、无 started 执行行；⑨ PostgreSQL 仍 `running` 且可查询（最后关） |
| 实测 | WSL2 Ubuntu + Docker：`REQUIRE_DOCKER=1 go test -count=1 -tags=e2e -timeout=40m ./internal/e2e -run '^TestUS25_Shutdown_ReleasesBeforeClose$' -v`（后台作业 `beftvk8lw`）→ `PASS` 90.34s，包 `ok 94.659s`，退出码 0；测试二进制 sha256 `b63bb49e2ecc25f8d25211afddfb658440d65528e5efbd4208606ade96508010`；用例自述日志「停机段序：监听先关 → 排空 36s 内继续续约 → 到点取消 → 属主释放（epoch 1）→ Worker 下线 → 退出 0」；无 FAIL、无 `t.Skip` |
| DoD 核对 | 九段顺序均有独立读数（监听 / 领取 / 续约 / 取消 / 租约 / 下线 / 退出与 grace 上界 / 审计终态 / PG 存活），不是只判退出码；未删断言、未放宽阈值（33s 下限与 80s 上限沿用）；容器宿主缺失时 `us25NewStack` 走跳过，本轮 WSL2 实跑无跳过 |

### 步骤 6 执行记录（T020，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T020 实现（M1 九步） | `internal/e2e/us25_migration_test.go`：`TestUS25_Migration_V6ToV12VerifiesObjects` —— 真 `init` 建 SQLite v6 工作区，直写 **16 行跨 10 张表**（管理操作 2 / 会话 1 / 消息 2 / Run 2 / 模型调用 2 / 工具调用 1 / 资源版本 2 / 资源指针 1 / Run 资源引用 2 / 生命周期 1），`PRAGMA journal_mode=DELETE` 后关闭并断言无 `-wal`/`-shm`（否则 `compareDBFiles` 判源库漂移）；唯一未归类文件 `notes.txt` 走 ② 导出（**退出码 1**、`未归类摘要` 64 位十六进制）→ ③ 复核文件 → ④ `--seal`（`核查状态：sealed`、进包 = 条目总数）→ ⑤ `db verify --dump`（`复核模式：dump` / `结论：通过`）→ 目标先 `db migrate` → ⑥ `db import`（`合计 32 表 / 16 行`）→ ⑦ `--target`（`行摘要按设计不在目标侧比对` + `detail_json`）→ ⑧ `--handoff`（`结论：交接就绪`、`主密钥记录：**无**`、`目标配置核对：**未核对**`）→ ⑨ `db switch`（源 v6、回退窗口开放、源端重盘已执行、交接门禁**未核对**、`绑定清单 SHA-256` 与包内 `manifest.json` 实算一致） |
| T020 实现（对象核对） | 同文件 PG 侧抽样：runs 归属串 `demo-agent\|cli\|completed\|local-admin\|admin-builtin\|admin-cli; demo-agent\|scheduler\|failed\|local-admin\|admin-builtin\|scheduler`（含无会话 Run）；sessions `caller_id IS NULL` ×1；messages `seq=2` 正文 `world`；`management_operations.detail_json->>'note'` 两行；llm_calls `purpose='chat'` ×2（PG 缺省补值）与失败调用 `error_kind='timeout'`；tool_invocations `echo`；resource_versions 技能行 `skill\|demo-skill\|1\|<hash>`；资源指针发布 / 应用同指 v1；`run_resource_refs` 与 `resource_versions` 按 `version_id + content_hash` 双条件连接命中 2 行（改指同存在的另一版本只在这里能发现）；`agent_lifecycle` UID / 状态 / `valid` 布尔化；`storage_cutover` 源版本 6、`first_write_at IS NULL`、操作者 `cli:` |
| T020 实现（源无对象登记） | 源库 `sqlite_master` 实测 **33 张表**（32 业务表 + schema_version），`flow_*` / `kb_*` / `skill_admission_*` 均为 0 张；目标库逐张枚举 **17 / 7 / 4** 张表且逐表 `count(*) = 0`，日志逐组写「源无对象、目标新建空表……不冒充已迁移」 |
| T020 实现（知识缺席） | `TestUS25_Migration_KnowledgeAbsentRecorded`：目标 `kb_*` 7 张表零行、`agent_resource_bindings` 的 `knowledge_collection` 计数 0、`run_resource_refs` 闭集不含该 kind；日志写明原因（SQLite v6 谱系从未承载 kb_* 对象，迁移清单只覆盖 32 张源表）；四个文档面（README / Operations / package-README / basalt.yaml.example）按拼接字面量断言不出现「知识已迁移」 |
| T020 实现（跨 schema 拒绝） | `TestUS25_Migration_CrossSchemaRefusal`：`git archive` 取钉死提交 `138382f`（US-22 收口、`SupportedSchemaVersion = 11`）→ 解包（拒绝越界条目、跳过 pax 全局头）→ `CGO_ENABLED=0 go build` 出旧制品 → 在已迁到 **v12** 的隔离 schema 上跑 `db migrate`：退出码 1，输出含「库版本 12 高于二进制支持的 11」「旧二进制不得打开新库」，拒绝后 `max(version)` 仍为 12 |
| 实测 | WSL2 Ubuntu + 本地 PostgreSQL 夹具（**不需要容器宿主**）：`dev-env/scripts/with-postgres.sh go test -count=1 -tags=e2e -timeout=40m ./internal/e2e -run '^TestUS25_Migration_' -v` → 三条全 PASS（0.91s / 0.93s / 2.43s），包 `ok 8.063s`，无 FAIL、无 `t.Skip`；`go vet -tags=e2e ./internal/e2e` 无输出；`gofumpt -l` 无输出 |
| 修复（pax 全局头） | `git archive` 生成的 tar 首条是 `pax_global_header`（typeflag `g`），原解包器把它当未知类型判红。按元数据条目跳过 `TypeXGlobalHeader` / `TypeXHeader`；仍有越界拒绝与未知类型判红 |
| DoD 核对 | M1 九步逐条读数 + handoff 门禁在切换报告里显式「未核对」（不冒充已跑）；对象计数与内容抽样覆盖 FR-25-24 点名的四类；源无对象逐组登记为「新建空表」；知识引用 0 且原因成文、文档面无「知识已迁移」；跨 schema 用**旧制品**验证拒绝；本用例不依赖容器宿主，本轮实跑无跳过。未删断言、未放宽阈值 |

### 步骤 6 执行记录（T021，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T021 实现（M2） | `internal/e2e/us25_migration_test.go` 追加 `TestUS25_Migration_SingleInstanceToMultiworker`：控制面停写 → `seed-workspace.sh export` 种子 → 启动 Worker → 两个不同 InstanceID（`wrk_...`）同时登记；Agent UID / 生命周期索引 / 发布版本逐字不变；runs / flows / 审计 / 知识计数一致；旧 bundle 可读；非空 Flow 与学习审批完整性在 M2 判定 |
| T021 实现（两节点升级） | 新建 `internal/e2e/us25_upgrade_test.go` 落 `TestUS25_Upgrade_TwoNodeAbortAndRollback`：停全部旧进程 → 备份 PG + 两个工作区 + 密钥 + 旧制品 → 新控制面/新 Worker 启动（同 schema v12，不宣称制品版本门禁）→ 停全部旧进程、两容器同一镜像 ID、工作区漂移触发 `ASSET_*` 拒绝、整组回滚（库 + 两个工作区 + 密钥）后恢复可运行；阶段 4 反证「只恢复数据库不足」 |
| T021 实测（M2） | WSL2 Ubuntu + Docker：`go test -tags=e2e ./internal/e2e -run '^TestUS25_Migration_SingleInstanceToMultiworker$' -v` → PASS（124.70s）；日志「M2：控制面 wrk_aa83… → Worker wrk_f83d…；执行 #1 与 #2 都准入并钉住 bundle a96ca2ef…；种子导入与 Worker 启动零业务写入；流程 / 审计 / 知识 / 学习审批逐字保留」 |
| T021 实测（升级） | 同命令 `-run '^TestUS25_Upgrade_TwoNodeAbortAndRollback$'` 第 3 轮 → PASS（198.223s，`ok github.com/ymedword/basalt/internal/e2e`，无 FAIL、无 `t.Skip`）；`go vet -tags=e2e ./internal/e2e` 无输出、`gofumpt -l` 无输出 |
| T021 修复①（派生戳） | 首轮指纹把 `agent_resource_bindings.projected_at` 判成业务改动（`1:affe8c4f… → 1:76923001…`）：该列每次启动由 `BindingStore.Rebuild` 全量重建（DELETE + INSERT）刷新。新增 `us25DerivedStampColumns = {"scheduled_tasks": "updated_at", "agent_resource_bindings": "projected_at"}` 剔除派生时间戳，其余列与行数仍在指纹内 |
| T021 修复②（密钥权限） | 首轮读 `secrets/master.key` 报 permission denied：宿主文件是 `0400 10001:10001`，测试进程非属主。摘要 / 备份 / 恢复三处改走 root 一次性容器（`masterKeyDigest` / `backupMasterKey` / `restoreMasterKey`），不打印密钥字节与哈希 |
| T021 修复③（bundle 路径） | 第二轮读 bundle 内定义副本报 `No such file`：测试常量把卷相对路径 `/.basalt/agents/...` 直接拼到 bundle 后，实际布局是 `agent/<工作区根相对路径>`（`assets.ConfigBundleRel`，`deps.Root` = `/.basalt`）。改为 `<bundle>/agent/agents/<agent>/AGENT.md`；用例 bug，非产品缺陷 |
| DoD 核对 | M2：生命周期索引与 Agent UID 保留、准入通过、runs / flows / 审计 / 知识计数一致、旧 bundle 可读、非空 Flow 与学习审批完整性在 M2 判定；升级：停全部旧进程、同 schema（v12）不宣称制品版本门禁、制品一致、整组回滚；未删断言、未放宽阈值、未跳测 |

### 步骤 6 执行记录（T022–T024，2026-11-08）

| 项 | 证据 |
| --- | --- |
| T022 红态 | 新建 `internal/cli/docs_first_run_test.go` 后、文档未改时四条逐字红：`docs_first_run_test.go:70`「Operations §1.5 缺少首配锚点」、`:121`「docs/Operations.md 缺少容器隔离声明」、`:168`「docs/Operations.md 缺少多 Worker 声明」、`:197`「发布说明支持矩阵 缺少起始标记」；断言读真实文档面（README / Operations / package-README / basalt.yaml.example / release/notes/TEMPLATE.md / support-matrix.json），不做夹具 |
| T022 绿 | 文档面改完后 `go test ./internal/cli -run '^TestDocs_' -count=1` → `ok github.com/ymedword/basalt/internal/cli 0.214s`（四条全绿） |
| T023（Operations） | §1.5 删除「不得在全新目录先执行 `basalt init`」，改为 `init` / 首次 `serve` 两个合法入口；重试补发主密钥写清资格与人工路径；步骤 5 统一为「记忆确认（4 条）」；§5.1 增「多 Worker 拓扑升级顺序」；§5.6 增「源库没有的对象逐项登记」；§6.1 增「多 Worker 共享主密钥」与两条 `stat -c '%u:%g:%a'` 复核；新增 §8.3（拓扑表 / 启动顺序 / 七步首次部署 / 健康检查与接管 / 升级与整组回滚 / 多 Worker 排障 / 隔离 blockquote / 首发支持矩阵）；分布式执行段同步「控制面 `serve` + Worker `gateway`」 |
| T023（README / GettingStarted） | README 新增 `### First-run configuration order`（九个英文锚点按序）与 `### Multi-worker deployment (Compose)`（八个多 Worker 锚点、五条隔离声明、支持矩阵口径）；GettingStarted 顶部导航指向 `Operations.md#first-run` 与 `#multiworker-compose`，不复制步骤 |
| T024（package-README / yaml / 发布说明模板） | `release/package-README.txt` 步骤 6 九锚点与 Operations §1.5 同口径、`init` 可作为第一步，步骤 7 改为重试补发主密钥（失败走控制台诊断或 `basalt secrets init`），新增步骤 8 多 Worker 指向 §8.3、步骤 9 隔离声明；`basalt.yaml.example` 增首配顺序注释、`worker` / `scheduler` / `assets` 三段注释（未知键拒绝与缺省值）、多 Worker 与隔离注释，不写真实密钥 / DSN；`release/notes/TEMPLATE.md` 增 `## 部署支持矩阵`（只承诺 Compose 多 Worker + LocalRunner，K8s / 数据库 HA / 真实双宿主标「未验证」）与多 Worker 升级 / 整组回滚说明 |
| 判据面说明（非放宽） | `TestDocs_SupportMatrixOnlyComposeMultiworker` 的 Operations 区块结束标记由 `"\n### "` 改为 `"\n## "`：原标记会把 §8.3 之后的无关章节（含第 1604 行 K8s 规划提法）卷进区块；新区块仍只含矩阵小节，断言与词表未减 |
| T024 修正 | 文档绿态轮回中修正三处与判据的字面偏差：`# 首次配置顺序（固定）` 标记必须逐字、package README 需出现反引号 `` `init` ``、支持矩阵行出现「双宿主」必须同带「未验证」；`TEMPLATE.md` 因 `__VERSION__` 占位使 MD050 一致风格为下划线，新增段不用 `**` 加粗 |
| 校验 | markdownlint-cli 0.48.0（WSL Node v22）对 `docs/Operations.md` / `README.md` / `release/notes/TEMPLATE.md` / `docs/GettingStarted.md` → 退出码 0；`release/support-matrix.json` 零改动（`git status` 未列）；`basalt.yaml.example` 不是 markdown，未纳入 markdownlint |

### 步骤 6 执行记录（T025 前修正：analyze findings 处置，2026-11-08）

analyze（只读）结论 CRITICAL=0、33/33 FR 有 check、50/50 `required_tests` 与 §4.4 一致。下表逐条处置；**未**改 `required_tests`、未删断言、未改模块卡冻结正文。

| Finding | 处置 | 证据 |
| --- | --- | --- |
| C-25-01（MEDIUM）protect 缺 §4.9 新建判据面 | 四份 stage 的 `protect` 由 37 项扩到 **61 项**（+24）：补 7 个新建 Go 文件（`first_install.go` / `demo_prepare.go` / `master_key_check.go` / `demo_prepare_test.go` / `docs_first_run_test.go` / `secrets_check_key_test.go` / `instance_secrets_store_test.go`）+ `internal/e2e/us25_*.go` 展开的 13 个文件 + 4 个部署资产（`compose.multiworker.yaml` / `basalt.worker.yaml` / `seed-workspace.sh` / `export-master-key.sh`）；`docs/contracts/**` 仍按 loader 的 `Lstat` 要求写 11 个显式文件；61 项 Lstat 全部存在 | 新哈希：base `9d132ff6…`、compose `e9812c5d…`、fault `671dac1d…`、docs `92710a84…`；`json.load` 校验四份逐字相等；`go test ./scripts/acceptance -run 'TestConfiguration\|TestStage\|TestPostgresChecksAreCoveredByFullProfile'` → `ok 5.513s` |
| D-25-05（MEDIUM）T010/T011 无 stage 映射 | `us25-compose.json` 的 `tasks` 增 `T010→us25-compose-roles`、`T011→us25-compose-seed`；tasks.md Check 映射表同步 | 同上测试含 `TestStage*`；映射表见 tasks.md §Check 映射 |
| A-25-01（LOW）“私网”无定义 | 定位为**部署纪律**，不加 RFC1918 校验（那会改判据面）；`docker/.env.multiworker.example` 与 `docs/Operations.md` §8.3 写明“私网 = RFC1918 段或回环；预检只拒空值与 0.0.0.0，不判定公网地址” | 两处文本；预检判据未变 |
| A-25-02（LOW）退出码 78 主体不明 | spec.md 澄清：`check-key` 命令本身 0 / 1，**78** 是 `entrypoint.sh` 的 `fail()` 映射 | spec.md Clarifications 2026-11-08 条目已改 |
| C-25-02（LOW）`BASALT_DB_BIND` 空值语义 | data-model §5 改为“空值由预检拒绝；`:-127.0.0.1` 只是第二道防线，不表示空值可用”；新增 live 子用例 `empty-address-rejected`（预检退出 1 且点名空值） | `internal/e2e/us25_compose_test.go` 子用例；e2e 跑动归 T025 |
| C-25-03（LOW）§4.6.4 骨架 8 锚点 | 模块卡冻结不改；以 §4.4 / spec 的 9 锚点为准，§4.6.4 为示例骨架——记入本表与验收报告差异说明 | 本行 |
| C-25-04（LOW）§3.3 写“不发布”vs 实现回环 | 模块卡冻结不改；FR-25-14 与 data-model §5 允许“不发布或只绑回环”，实现选回环（已由 T010 裁决记录）；差异记入本表与验收报告 | 本行；T010 设计裁决（PG 回环发布） |
| D-25-02（LOW）live 无空地址负例 | 新增 `empty-address-rejected` 子用例（同 `required_tests` 内，不改判据面） | 同上 |
| D-25-03（LOW）两个 shell 脚本无自动判据 | 保持人工证据口径：`init-secrets.sh --copy-from/--db-host`、`export-master-key.sh` 的人工九组读数记在 T010 段；T026 验收报告登记为“人工证据、非自动覆盖” | T010 段“实测（凭据脚本）”；acceptance.md |
| D-25-04（LOW）`.env.multiworker.example` 无断言 | `TestUS25_Compose_SecurityInvariants` 增文件断言：14 个变量赋值存在、默认 `BASALT_DB_BIND=127.0.0.1`、无凭据类赋值 | `internal/e2e/us25_compose_test.go`；e2e 跑动归 T025 |
| D-25-06（LOW）preflight 安全字段未断言 | 同用例增 `basalt-preflight` 五项断言（`user` / `read_only` / `init` / `cap_drop ALL` / `no-new-privileges`）；tmpfs 显式不适用（只读且不写文件） | 同上 |
| D-25-07（LOW）跨 schema 用例未命名 | **不**增列 `required_tests`（§4.9 规则 2 需用户确认）；`TestUS25_Migration_CrossSchemaRefusal` 由正则实际执行，其覆盖关系写进升级用例头注释并将在 T025 证据中显式登记 | `internal/e2e/us25_upgrade_test.go` 头注释 |
| F-25-01（LOW）第四服务未同步 | data-model §5、plan Summary/structure、tasks.md T010 补 `basalt-preflight`（一次性、同安全不变量、`depends_on: service_completed_successfully`）；模块卡冻结不改，差异由 review ② 追认 | 三处文本；compose 实现未变 |
| B-25-01（LOW）protect 四份复制 | 接受重复（四 stage 各自独立装载，loader 无共享清单机制）；以 C-25-01 的一步补齐消除漂移 | 四份哈希；`json.load` 相等性校验 |

| 独立复核 deploy-assets（HIGH）protect 缺 §4.9 路径 | 与 C-25-01 同一缺陷，按该行处置；四份各 61 项 | 复核结论 `{"severity":"HIGH", file harness/stages/us25-base.json}`；修复见 C-25-01 行 |
| 独立复核 go-correctness（MEDIUM）预检不拒 IPv6 全地址 | 预检 `case` 增 `::` / `[::]` / `0:0:0:0:0:0:0:0`（含 `:*` 形态）并在输出点名；新增 live 子用例 `ipv6-zero-address-rejected`；`.env` 示例、Operations §8.3、data-model §5、compose 端口注释同步 | `docker/compose.multiworker.yaml` 预检段；`internal/e2e/us25_compose_test.go` 子用例；e2e 跑动归 T025 |
| 独立复核 tests-criteria（MEDIUM）FR-25-27 用例未列 `required_tests` | 与 D-25-07 同一缺陷：**不**增列（§4.9 规则 2 需用户确认），该用例由正则实际执行，T025 证据与验收报告显式登记，留给 review ② 裁决 | 复核 verdict CONFIRMED；处置见 D-25-07 行 |

校验：`go vet -tags=e2e ./internal/e2e` 无输出；`gofumpt -l` 对两个改动的 e2e 文件零输出（IPv6 修正后再跑一次）。D-25-02/04/06 与 IPv6 子用例的新断言**尚未执行**（需容器宿主，随 T025 `us25-compose` stage 跑动）。

### 步骤 6 执行记录（T025 前修正：独立复核 findings 与逆改点实证，2026-11-08）

独立复核（只读、与实现分离）产出 23 条唯一发现，其中 CONFIRMED 3 条（2×HIGH、1×MEDIUM）全部修复；其余按「已处置 / 接受 / 留给 review ②」逐条记录。**未**改 `required_tests`、未删断言、未改模块卡冻结正文。

| 发现 | 处置 | 证据 |
| --- | --- | --- |
| HIGH `docker/export-master-key.sh` 先 `mkdir`/`chown`/`chmod` 后校验 dest | 全部前置条件（dest 形态、系统目录、符号链接、已存在、卷、密钥可读）前移到任何目录改动之前；新增系统目录与符号链接拒绝，目录创建与属主设置只在全部前置成立后执行 | `sh -n` 通过；T010 段人工九组读数来自修改后脚本 |
| HIGH Operations / README 的 master key 命令在仓库根执行会得到 `docker/docker/secrets` | 四个文档面统一为 `(cd docker && sudo sh export-master-key.sh basalt secrets/master.key)`，并写明 dest 相对 `docker/`；`seed-workspace.sh` 同理 | `docs/Operations.md` 步骤 2、`README.md`、`release/package-README.txt` |
| MEDIUM Operations 种子步骤用宿主占位路径，实际是命名卷 | 步骤 5 全量改写：停机 → `compose run --rm --no-deps basalt-control assets install --agent <agent>` → 一次性 root 容器导出 `basalt_control-workspace:/src` → `compose --profile worker create --no-recreate basalt-worker` → 导入 `basalt_worker-workspace:/data`；写明种子不含 `basalt.yaml`、providers/embedding/flows 必须一致 | `docs/Operations.md` §8.3 步骤 5（行 1536–1559）；与 `internal/e2e/us25_fixture_test.go` 的实测容器调用一致 |
| MEDIUM protect 缺 4 个新建判据面 | 四份 stage `protect` 各补 `demo_prepare_integration_test.go` / `instance_secrets_managed_write_test.go` / `master_key_check_test.go` / `demo_workflow_prepare_test.go`，各 **65 项** | WSL `python3 json.load` 计数：base/compose/docs/fault 均 65；stage lint 结果见下 |
| MEDIUM §4.6.6 读取顺序无逆改点 | `demo_prepare_test.go` 增 `readOrder` 记录与 `requireFilesReadBeforeContent`；**实证**见下方「逆改点实证」 | 两用例红/绿读数 |
| LOW 支持矩阵 schema 值无断言 | `docs_first_run_test.go` 增 `schema == 1` 断言（FR-25-31：schema 与判卷逻辑不改） | `internal/cli/docs_first_run_test.go` |
| LOW 种子导入目标侧无符号链接拒绝、chmod 过宽 | `seed_import` 增目标侧（target / target/.basalt / `find -type l`）符号链接拒绝；可读性 chmod 移到暂存树并在复制前 `umask 022`，删除对目标 `.basalt` 的无差别 `chmod -R a+rX` | `docker/seed-workspace.sh`；`sh -n` 通过 |
| LOW 预检 IPv6 等价拼写漏拒 | 预检改为归一化判定：去端口、去方括号、全零段（`[::0]`、`:::000`、`0:0:0:0:0:0:0:0`、`::ffff:0.0.0.0` 等）拒绝，`::1` 保留 | `docker/compose.multiworker.yaml` 预检段 |
| LOW `compose exec` 缺 `-f`、缺 DSN 导出 | 步骤 6 补 `-f docker/compose.multiworker.yaml` 与 `BASALT_DATABASE_DSN="$(cat /run/secrets/database_dsn)"` | `docs/Operations.md` |
| LOW 资产安装步骤顺序错（运行中装不了） | 资产安装移到停机维护窗口（实例锁），种子导出在其后；Worker 不执行资产安装或发布（FR-25-22） | `docs/Operations.md` §8.3 步骤 5 / 7；`README.md`；`release/package-README.txt` |
| LOW 种子后配置一致性未写 | 四处文档面写明种子不含 `basalt.yaml`，providers / embedding / flows 必须一致 | `docs/Operations.md`、`README.md`、`release/package-README.txt`、`basalt.yaml.example` |
| LOW `.env` 目标目录不明 | 步骤 1 明确 `docker/.env` 与 `-f compose.multiworker.yaml exec` | `docs/Operations.md` |
| LOW 材料表未列 ingress/IMAGE/BIND | 材料表补全六项与一致性要求 | `docs/Operations.md` |
| LOW tasks T003 映射与 T025 裸 stage 名 | T025 改为仓库相对路径并补四 stage 覆盖说明 | `specs/025-compose-multiworker/tasks.md` |
| LOW DemoPrepare 构造失败留下 typed-nil 接口 | `run.go` 用真正的 nil 接口（`var demoPrepare cli.DemoPrepare`） | `internal/app/run.go` |
| LOW 镜像 digest 预检表述过强 | 降级为部署纪律（预检不校验 digest 形式） | `docs/Operations.md` §8.3 |
| 接受 / 留给 review ② | `TestUS25_Migration_CrossSchemaRefusal` 未列入 `required_tests`（§4.9 规则 2 需用户确认，D-25-07）；`C-25-03` / `C-25-04` / `F-25-01` 为模块卡与实现差异说明 | 见 §2 台账与 acceptance.md |

**逆改点实证**（源码临时变异 → 定向测试变红 → 逐字节还原 → 复绿；已确认无残留，`grep 临时逆改` 零命中）：

| 变异 | 施加 | 结果 | 还原后 |
| --- | --- | --- | --- |
| A：调换 `applyBinding` 内 `readAgentFilesETag` 与 `readAgentContent` 的次序 | `internal/app/demo_prepare.go` | `TestDemoPrepare_ApplyBindsAndIngests` / `TestDemoPrepare_ETagPinnedAcrossReads` 红（`"5" is not less than "4"`，§4.6.6 文案） | 两用例复绿 |
| A′：断言自检修正 | 首版断言取**首次**出现，计划阶段同样先读 files 会掩盖应用阶段的次序——变异 A 下仍绿，证明断言无效；改为取**最后一次**（应用阶段在计划之后）后变异 A 必红 | `internal/app/demo_prepare_test.go` | 复绿 |
| B：在 `recordProvisioningPending` 之前提前写名册 | `internal/app/instance_bootstrap.go` | `TestFirstInstall_ProvisioningFailureNeverWritesRoster` / `TestFirstInstall_IntentFailureNeverWritesRoster` 红（`instance.json exists`） | 两用例复绿 |

### 步骤 7 执行记录（T025 全量验收，2026-11-08）

冻结树：最后一次源码改动 18:28:35（CST），冻结清单 19:48 生成（2217 条，指纹 `d7c285f22bff1fbcff365c5eebdfc17eaee420227687a35305894dcc738575d6`）；四份 stage 在 20:26:08（CST）/ 12:26:08Z 之后启动，全部落在冻结树上。08:42 / 09:23 / 09:53 三轮在冻结前执行，**作废**，不作为证据。

**环境偏差（如实登记）**：T025 原文写 base / docs 用 Windows 原生 `.\scripts\check-acceptance.ps1`；本轮四份 stage 全部在 WSL2 Linux 执行（本地 PG 夹具 `dev-env/scripts/with-postgres.sh` 注入 DSN，不打印凭据）。`args`、`required_tests`、`timeout_seconds` 与四份 stage 文件逐字一致，未改判据面；统一宿主使四份报告共享同一源码指纹。

`make check`（WSL，19:45:19–20:23:32，exit 0，七目标全绿）：`sec` = gosec 清静 + govulncheck `No vulnerabilities found`（0 影响；2 条 required 模块内真实不可达告警）；`test` 40 ok / 2 no-test-files；`test-integration` 42 ok / 2；`cover` 逐包两位小数判定 + `check-coverage: 全部包达标`；`build` 以 `CGO_ENABLED=0` 产出 `bin/basalt`。日志总 `ok` 行 164、`FAIL` 0。

| 运行 | 运行目录 | scope | 检查数 | 结果 | 耗时要点 |
| --- | --- | --- | --- | --- | --- |
| base | `.loop/runs/accept-20261108T122608.875827008` | `full/us25-base` | 17 | passed / exit 0 | test-integration 635s、cover 716s、sec 385s、lint 247s、test-pg 281s |
| docs | `.loop/runs/accept-20261108T130927.440661039` | `stage-only/us25-docs` | 1 | passed / exit 0 | 1s |
| compose | `.loop/runs/accept-20261108T130947.747702982` | `stage-only/us25-compose` | 2 | passed / exit 0 | roles 221s、seed 110s |
| fault | `.loop/runs/accept-20261108T131537.993049302` | `stage-only/us25-fault` | 2 | passed / exit 0 | execution 354s、migration 312s |

四份报告同 `fingerprint=d7c285f2…`、同 `base_commit=3f8d0dc`、`dirty=true`；`skipped_tests` 合计 **0**。具名测试通过数（含子测试）：first-install 72、history 6、cli 11、demo 11、releasekit 1、race 3、docs 4、compose-roles 14、compose-seed 8、fault-execution 5、fault-migration 5。逐 check 四态与耗时表见 `acceptance.md` §2；本轮通过**不**翻转 §4 未验证清单。

### 步骤 7 执行记录（T026 收口三件套，2026-11-08）

- **analyze 复跑**：33/33 FR 覆盖、50/50 `required_tests` 覆盖、CRITICAL **0**；分析面为 `specs/025-compose-multiworker/` 的 spec / plan / tasks 与模块卡 §3 / §4 映射。
- **独立复核**：23 条唯一发现，3 条 CONFIRMED（2×HIGH、1×MEDIUM）在 T025 前已修复复验；T025 后**无新增修复**。逐条处置与逆改点实证（A / A′ / B）见上一节。
- **七项证据 DoD**：门禁逐目标四态、harness 映射逐测试对号、交付物存在性（64 条仓库路径 + 1 条运行时目标）、前序模块回归、13 条全局不变量自查、剩余人工项清单、变更总结——汇总在 `acceptance.md`（§3–§6）。
- **指纹绑定**：四份报告指纹同为 `d7c285f2…`；`acceptance.md` 落盘并过 lint 后重算指纹清单，与冻结清单差异**恰好一行**（本文件），聚合指纹 `5a47a38468ce26c9c69ff4776dc7fbb4cd9ca794b140d865a19ec0479dbafaf3`（本记录写入时读数；run-state 按 basename 排除、tasks 勾选归一化，均不改变该值）。
- **markdownlint**：`acceptance.md` / `tasks.md` 零诊断（库直连读数 0 / 0；同内容注入 `# probe-h1` 后两文件各报 1 条 MD025，证明解析确实生效）。
- **review ②**：用户 2026-11-08 预授权「review2 收尾时我授权可以直接通过，并且提交代码推送到 main 分支，要跟踪 CI 直到变绿」；本记录不预写 CI 结论，提交与推送后追加运行号与结果。
- **人工项**：§6.1 的 1–9 由自动等价证据覆盖；10–11 仅自动断言通过、**未**人工走查；12 按范围不做。三项均在 `acceptance.md` §6.2 明列，不冒充人工已接受。

## 2. 失败台账

| 日期 | 问题 | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-11-08 | `markdownlint --json` 读数为空，无法区分「零诊断」与「未检查」 | 工具本身输出行为需要控制探针验证 | 控制探针（追加 `# probe` 后必报 MD025）+ `linesInFile` 字段共同证明文件被解析；后续一律附 `linesInFile` | 1（登记自检阶段，非本 US 代码问题） |
| 2026-11-08 | `us25_asset_test.go` 的一次性脚本把 volume 相对路径当绝对路径用，出现 `/data/data/...`，篡改 / 删除落在错路径，用例在应拒绝处读到相反结果 | 夹具路径口径混用（相对 vs 容器绝对） | 统一口径：`volumeHas` / `volumeRead` / `volumeStat` 取相对路径，`execInVolume` / 一次性脚本取 `/data/...`；复跑通过 | 1（夹具问题，非产品缺陷） |
| 2026-11-08 | `TestUS25_DatabaseOutage_NoReplay` 原断言只接受 Run `failed:worker_lost`，实测收尾为 `cancelled:renew_failed` | 判据把某一条合法丢失检测路径的私有字面量当成契约 | 改为「失权类终态之一（failed:worker_lost / cancelled:renew_failed）」并写明两条路径的来源；仍拒绝 completed / running | 1 |
| 2026-11-08 | 同用例原断言要求租约 `epoch == 2`，实测自停路径代次仍为 1 | 把回收器接管路径当成唯一路径 | 改为 owner 不变 + `released_at` 非空 + 代次 ∈ {1,2}；仍拒绝仍被持有与换了 owner | 1 |
| 2026-11-08 | `TestDemoPrepare_HTTPFilesAndKnowledge` 原断言失败文档在列表里 `latest_status == "failed"`，实测为空串 | 把「失败终态在版本行」当成「文档行有激活的失败版本」 | 改为：文档行存在但 `latest_status` / `latest_version_id` 为空，`GET /documents/{id}/versions` 最新版本 `failed` 且 `error_kind=embed_failed`；仍核对 8 份文档行与重试后 9 份 active | 1（用例判据，非产品缺陷） |
| 2026-11-08 | 同用例原断言第二次计划 `Warnings` 为空，实测有 1 条「共 1 次 embedding 调用」提示 | 把「第二次运行」当成「零摄取」；实际还要补摄取失败的那 1 份 | 改为第二次计划恰好 1 条提示且写明 1 次调用；零提示断言移到零摄取的第三次运行 | 1（用例判据，非产品缺陷） |
| 2026-11-08 | `TestUS25_Migration_CrossSchemaRefusal` 解包 `git archive` 输出的 tar 时判红：「归档条目 pax_global_header 类型 103 不受支持」 | 解包器把 tar 元数据条目当成未知文件类型 | 跳过 `TypeXGlobalHeader` / `TypeXHeader`（记录提交号的 pax 头不是文件）；越界路径与未知类型仍判红；复跑通过 | 1（夹具问题，非产品缺陷） |
| 2026-11-08 | 升级用例把 `agent_resource_bindings.projected_at` 判成业务改动（`1:affe8c4f… → 1:76923001…`） | 该列每次启动由 `BindingStore.Rebuild` 全量重建刷新，是派生时间戳不是业务事实 | 指纹投影剔除 `scheduled_tasks.updated_at` 与 `agent_resource_bindings.projected_at` 两列；行数与其余列仍在指纹内；复跑通过 | 1（用例判据，非产品缺陷） |
| 2026-11-08 | 升级用例读 `secrets/master.key` 报 permission denied | 宿主文件 `0400 10001:10001`，测试进程非属主；`0400` 是该密钥的正确权限，不是配置错误 | 摘要 / 备份 / 恢复改走 root 一次性容器，不打印密钥字节与哈希；复跑通过 | 1（用例执行方式，非产品缺陷） |
| 2026-11-08 | 升级用例读 bundle 内定义副本报 `No such file`：`…/<hash>/agent/.basalt/agents/us25-probe/AGENT.md` | 测试常量把卷相对路径拼到 bundle 后；实际布局是 `agent/<工作区根相对路径>`（`assets.ConfigBundleRel`） | 常量改为 `<bundle>/agent/agents/<agent>/AGENT.md`；复跑通过 | 1（用例问题，非产品缺陷） |

## 3. 停止预算与计数

| 计数项 | 当前值 | 上限 |
| --- | --- | --- |
| 同一问题修复次数 | 0 | 3（超过转 `/debug`） |
| `/debug` 轮次 | 0 | 2 |
| 当前问题假设数 | 0 | 3（超出停止报告） |

计数跨会话从本段恢复。

## 4. 未验证清单（四态口径的持久载体）

| 项 | 状态 | 原因 / 恢复条件 |
| --- | --- | --- |
| 真实双宿主（A+B 云服务器）执行、接管与隔离 | 未验证 | 用户已安排首发后演示；本 US 只用同机 / 模拟两宿主证据 |
| 真实模型调用 | 未验证 | 2026-11-04 用户裁决：除 US-19 收尾外禁止调用真实模型；本 US 用本地协议桩与冻结夹具 |
| 真实 webhook 投递 | 未验证 | 无外部授权；以本地接收器验证协议面 |
| K8s 单实例（US-23）与 K8s 多 Worker（US-25A） | 未验证 | 明确不在本 US 范围 |
| 数据库 HA / 主备 / 故障转移 | 未验证 | 明确不在本 US 范围；支持矩阵不得写入 |
| 容器隔离（OCI/gVisor）验证与 Demo 双模适配 | 未验证 | 归 US-20 / US-25B；本 US 只写文档警告 |
| Windows 原生 L3 容器用例 | 未验证 | 需 Linux 容器或 WSL2；缺环境按跳过登记，不冒充通过 |
| `3f8d0dc` 的 main 分支 CI 运行（42433483456） | 未验证 | 收尾文档提交触发的 CI 在开始实施时仍 in_progress；变红须先定位，不阻塞 US-25 实施本身 |
| 正式发布 tag / 签名 / Release 上传 | 未验证 | 归 US-17 流程，需用户明确触发 |
| `docs/GettingStarted.md` 的 T023 编辑 | 未入库（非交付面） | 该文件被 `.git/info/exclude:30` 排除且未受跟踪（`git ls-files --error-unmatch` 非零）；T023 的导航改动只留在本地工作区，**不得** `git add -f`；US-25 交付面不含此文件 |

## 5. 收口结论

| 项 | 结论 | 证据 |
| --- | --- | --- |
| AI 验收（四 stage + `make check`） | **通过** | 四份 `report.json` 全 passed / exit 0 / 同一指纹 `d7c285f2…`；`make check` 19:45:19–20:23:32 exit 0；见 `acceptance.md` §2 与上一节 |
| 独立复核 | **通过（23 条全处置）** | 3 条 CONFIRMED（2×HIGH、1×MEDIUM）修复复验；逆改点 A / A′ / B 实证；见上节与 `acceptance.md` §6.1 |
| analyze 复跑 | **通过（CRITICAL 0）** | 33/33 FR、50/50 `required_tests` 覆盖；见 `acceptance.md` §6.4 |
| 人工 review ② | **通过（用户 2026-11-08 预授权直通）** | 用户原话见 `acceptance.md` §8；本次为 AI 代执行的收口提交，非用户逐项走查 |
| 人工项 10–11（文档走查 / 隔离声明核对） | **未人工执行** | 自动断言通过；`acceptance.md` §6.2 明列，不写成人工已接受 |
| 人工项 12（真实双宿主） | **不做 / 未测量** | 按范围推迟到首发后 A+B 演示；§4 未验证清单保留 |
| 提交与推送 | **随本文件所在提交执行** | 分支 `025-compose-multiworker` → `main` 快进合并；提交 SHA 用 `git log -1` 核对（本文件不能记录自身提交的 SHA） |
| 推送后 CI | **待跟踪** | 变绿后回填运行号与结论；变红先定位修复，不把进行中写成通过 |
| 未验证清单（§4） | **保持原样（10 条）** | 收口不翻转任何"未验证"项 |

**待回填**：CI 运行号与结果（提交 / 推送后追加本行，不预写）。
