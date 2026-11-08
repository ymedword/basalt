# US25 验收报告

对应模块卡：`docs/us/us25-compose-multiworker.md` R3。review ① 溯源为 1906 行 `sha256=9b13034e659c48ab85c68b62dcf5c035f4560fed4851cdaef934559c0bde6d64`；现值 1917 行 `sha256=22e203fb2ef72cdeaafa02f637ad87993642ddc0dee1afb70a4939f4d91b9973`，差异仅为 §6.2 review ① 记录，无设计变化（run-state §1）。基线提交 `3f8d0dc`，分支 `025-compose-multiworker`。

四态口径：每个验收点标 **通过 / 失败 / 跳过 / 未验证**；必验项处于跳过或未验证时，该项验收保持未完成。

## 1. 门禁与验收运行读数

### 1.1 `make check` 七目标（宪法口径）

`dev-env/scripts/with-postgres.sh make check`（WSL2 Ubuntu，用户 `ym`，本地 PostgreSQL 测试夹具经 `with-postgres.sh` 注入 `BASALT_TEST_PG_DSN`；Go 工具链 `go1.27.3`），2026-11-08 19:45:19–20:23:32（CST），**退出 0**。过程日志 `/tmp/us25-make-check-final.log` 属临时物，收口时按纪律删除；读数逐项抄录于本表。

| 目标 | 四态 | 读数 |
| --- | --- | --- |
| `fmt` | **通过** | `sh scripts/check-format.sh`：`check-format: gofmt / gofumpt 均干净`，退出 0 |
| `lint` | **通过** | `go vet -tags=integration ./...` + `staticcheck -tags=integration -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...`，零诊断、退出 0（判定集显式指定；`-tags=integration` 装载集成层，见开发操作 §2.2） |
| `sec` | **通过** | `gosec -quiet ./...` 零告警；`govulncheck ./...`：`No vulnerabilities found` / `Your code is affected by 0 vulnerabilities`。**如实登记**：依赖模块层仍有 2 条未被本仓代码调用的已知漏洞（`2 vulnerabilities in modules you require`），`govulncheck` 判定不可达，属既有依赖基线；本 US 零新增坐标 |
| `test` | **通过** | `go test -race -cover ./...`：**40 包 ok、2 个 `no test files`（`examples` / `internal/e2e`）、0 FAIL**；`internal/app` 43.990s（untagged 覆盖率 21.5%，集成层在下一个目标） |
| `test-integration` | **通过** | `go test -tags=integration -count=1 -timeout=20m ./...`：**42 包 ok、2 个 `no test files`、0 FAIL**；长尾 `internal/app` 396.862s、`internal/storage/postgres` 270.407s |
| `cover` | **通过** | `sh scripts/check-coverage.sh`（自身带 `-tags=integration` 重跑）：`check-coverage: 全部包达标`，逐包读数见 §1.3 |
| `build` | **通过** | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=3f8d0dc" -o bin/basalt ./cmd/basalt`，退出 0；产物 `bin/basalt` **41,234,592 字节** |

`-race` 与 `CGO_ENABLED=0` **分属不同目标**（`test` / `cover` 开 CGO，`build` 关 CGO），未混用。最终轮一次全绿；**未新增抑制注释换取绿态**（计数见 §6.2）。

### 1.2 Harness stage（公共 profile + 四份本 US stage）

四轮全部在源码冻结后运行（最后计数文件改动 18:28:35，见 §1.4），命令均带 `--executor ai --run-kind original` 与仓库相对 `--stage` 路径；公共 profile 11 项（`fmt` / `lint` / `sec` / `test` / `test-integration` / `test-pg` / `cover` / `build` / `eval-build` / `behavior` / `injection`）由 `harness/acceptance.json` 提供，不在此重抄。

**运行 1：base（full profile 11 项 + us25-base 6 项）** — 运行目录 `.loop/runs/accept-20261108T122608.875827008`，`scope=full/us25-base`，`status=passed`，`exit_code=0`，`started_at=2026-11-08T12:26:20Z`，指纹 `d7c285f22bff1fbcff365c5eebdfc17eaee420227687a35305894dcc738575d6`，`base_commit=3f8d0dc`，`dirty=true`。

| Check | 四态 | exit | 耗时 | passed | skipped |
| --- | --- | --- | --- | --- | --- |
| `fmt` | **通过** | 0 | 30s | 0 | 0 |
| `lint` | **通过** | 0 | 4m7s | 0 | 0 |
| `sec` | **通过** | 0 | 6m25s | 0 | 0 |
| `test` | **通过** | 0 | 3m47s | 0 | 0 |
| `test-integration` | **通过** | 0 | 10m35s | 0 | 0 |
| `test-pg` | **通过** | 0 | 4m41s | 0 | 0 |
| `cover` | **通过** | 0 | 11m56s | 0 | 0 |
| `build` | **通过** | 0 | 4s | 0 | 0 |
| `eval-build` | **通过** | 0 | 4s | 0 | 0 |
| `behavior` | **通过** | 0 | 3s | 0 | 0 |
| `injection` | **通过** | 0 | 2s | 0 | 0 |
| `us25-base-first-install` | **通过** | 0 | 13s | 72 | 0 |
| `us25-base-history` | **通过** | 0 | 3s | 6 | 0 |
| `us25-base-cli` | **通过** | 0 | 2s | 11 | 0 |
| `us25-base-demo` | **通过** | 0 | 5s | 11 | 0 |
| `us25-base-releasekit` | **通过** | 0 | 1s | 1 | 0 |
| `us25-base-race` | **通过** | 0 | 17s | 3 | 0 |

**运行 2：docs（stage-only，1 项）** — 运行目录 `.loop/runs/accept-20261108T130927.440661039`，`scope=stage-only/us25-docs`，`status=passed`，`exit_code=0`，`started_at=2026-11-08T13:09:36Z`，指纹 `d7c285f22bff1fbcff365c5eebdfc17eaee420227687a35305894dcc738575d6`，`base_commit=3f8d0dc`，`dirty=true`。

| Check | 四态 | exit | 耗时 | passed | skipped |
| --- | --- | --- | --- | --- | --- |
| `us25-docs` | **通过** | 0 | 1s | 4 | 0 |

**运行 3：compose（stage-only，2 项）** — 运行目录 `.loop/runs/accept-20261108T130947.747702982`，`scope=stage-only/us25-compose`，`status=passed`，`exit_code=0`，`started_at=2026-11-08T13:09:56Z`，指纹 `d7c285f22bff1fbcff365c5eebdfc17eaee420227687a35305894dcc738575d6`，`base_commit=3f8d0dc`，`dirty=true`。

| Check | 四态 | exit | 耗时 | passed | skipped |
| --- | --- | --- | --- | --- | --- |
| `us25-compose-roles` | **通过** | 0 | 3m41s | 14 | 0 |
| `us25-compose-seed` | **通过** | 0 | 1m50s | 8 | 0 |

**运行 4：fault（stage-only，2 项）** — 运行目录 `.loop/runs/accept-20261108T131537.993049302`，`scope=stage-only/us25-fault`，`status=passed`，`exit_code=0`，`started_at=2026-11-08T13:15:48Z`，指纹 `d7c285f22bff1fbcff365c5eebdfc17eaee420227687a35305894dcc738575d6`，`base_commit=3f8d0dc`，`dirty=true`。

| Check | 四态 | exit | 耗时 | passed | skipped |
| --- | --- | --- | --- | --- | --- |
| `us25-fault-execution` | **通过** | 0 | 5m54s | 5 | 0 |
| `us25-fault-migration` | **通过** | 0 | 5m12s | 5 | 0 |

**作废轮次（如实登记）**：同日 08:42 / 09:23 / 09:53（UTC）的 base / docs / compose / fault 四轮读数（指纹 `da119d17…` / `553679d5…`）产生于 T025 前修正（analyze findings、独立复核修复、逆改点实证）落树之前，**不作本报告证据**、不计通过；本轮四轮全部重跑于冻结树。全程未修改 runner、未跳过检查、未放宽超时或断言。

**过程日志**：`/tmp/us25-{base,docs,compose,fault}-final.log` 是外层 shell 的过程日志，收口时按临时物纪律删除；判读与复跑依据是 `.loop/runs/*/report.json`（含逐 check 日志），报告保留在 `.loop/runs/`（不进版本库）。

### 1.3 逐包覆盖率（`cover` 目标读数）

`cover` 自身带 `-tags=integration` 重跑；`check-coverage.sh` 判定 `全部包达标`（退出 0）。分档：`core` / `tool` / `memory` / `storage` / `postgres` / `assets` ≥ 70%，其余 ≥ 50%。

| 包 | 覆盖率 | 分档 | 四态 |
| --- | --- | --- | --- |
| `internal/storage/postgres` | 72.21% | ≥70 | **通过** |
| `internal/storage` | 73.14% | ≥70 | **通过** |
| `internal/assets` | 74.85% | ≥70 | **通过** |
| `internal/core` | 78.68% | ≥70 | **通过** |
| `internal/tool` | 78.37% | ≥70 | **通过** |
| `internal/memory` | 86.01% | ≥70 | **通过** |
| `internal/flow` | 63.93% | ≥50 | **通过** |
| `internal/app` | 74.63% | ≥50 | **通过** |
| `internal/callers` | 88.05% | ≥50 | **通过** |
| `internal/cli` | 81.90% | ≥50 | **通过** |
| `internal/web` | 85.48% | ≥50 | **通过** |
| 其余包（最低 `internal/testutil` 51.52%、`postgres/pgtemp` 54.55%；`internal/diagnostics` 68.61%） | — | ≥50 | **通过** |

`scripts`（`coverage: [no statements]`）按既有规则跳过。本 US 新增代码落在 `internal/app` / `internal/cli` / `internal/storage/postgres` / `scripts/releasekit`，四包均高于各自阈值；没有包因本 US 跌破底线。

### 1.4 源码冻结证据

- **最后计数文件改动**：`2026-11-08 18:28:35`（`internal/app/instance_bootstrap.go` / `demo_prepare.go`）；`run-state.md`（指纹按 basename 排除）18:41。此后到四轮 stage 结束**未编辑任何计入指纹的文件**。
- **冻结指纹**：`d7c285f22bff1fbcff365c5eebdfc17eaee420227687a35305894dcc738575d6`（19:48 由独立重算脚本生成逐文件清单 `/tmp/us25-fp-freeze.txt`，253,458 字节；脚本与清单为临时物，收口时删除）。算法与 `scripts/acceptance/fingerprint.go` 一致：`git ls-files --cached --others --exclude-standard`、排除 `.git` / `.loop` / `.basalt` / `bin` / `evals/results` / `harness/acceptance/records` / `harness/acceptance/ledger.md`、跳过 `run-state.md` / `basalt.yaml` / `.env` / `*.local.env` / `.tmp-*`、归一化 tasks 勾选。
- **四轮报告指纹**：base `d7c285f2…`、docs `d7c285f2…`、compose `d7c285f2…`、fault `d7c285f2…`，四轮同为 `d7c285f2…`——与冻结指纹同值；`base_commit=3f8d0dc…`、`dirty=true`（修复尚未提交，属预期）。
- **回填后差异证明**：本文件（`acceptance.md`）与 `run-state.md` 的回填发生在四轮跑动之后；`run-state.md` 被指纹按 basename 排除，tasks 勾选被归一化，因此重算指纹相对冻结清单**只应新增本文件一行**，逐文件差异见 §9。

## 2. Harness 与逆改判别力

### 2.1 结构（引用公共配置，不复制）

四份 stage 只声明本 US 专属 check 与 `required_tests`；公共门禁由 `harness/acceptance.json` 的 full profile 提供。四份 `protect` 各 **65 项**（`Lstat` 精确路径，装载时全部存在），覆盖本 US 新建 Go 文件、20 个新测试文件、部署资产与 11 份契约文件；`docs/contracts/**` 因 loader 要求逐文件 `Lstat` 而显式列出。

| Stage | Check | 命令要点（args 原文） | 具名测试 |
| --- | --- | --- | --- |
| `us25-base` | `us25-base-first-install` | `go test -json -count=1 -tags=integration -timeout=10m ./internal/app -run ^(TestFirstInstall_\|TestUS25_FirstInstall_\|TestInstanceSecrets_)`（900s） | 9 |
| `us25-base` | `us25-base-history` | `go test -json -count=1 -tags=integration -timeout=10m ./internal/storage/postgres -run ^TestRuntimeHistory_`（900s） | 2 |
| `us25-base` | `us25-base-cli` | `go test -json -count=1 -timeout=5m ./internal/cli -run ^(TestCLI_(Init\|DemoWorkflow\|SecretsCheckKey)\|TestUS25_Compose_WorkerConfigParsesWithoutControlSecrets)`（600s） | 6 |
| `us25-base` | `us25-base-demo` | `go test -json -count=1 -tags=integration -timeout=15m ./internal/app -run ^TestDemoPrepare_`（1200s） | 9 |
| `us25-base` | `us25-base-releasekit` | `go test -json -count=1 -timeout=5m ./scripts/releasekit -run ^TestUS25_Image_ContextStagesWorkerAssets$`（600s） | 1 |
| `us25-base` | `us25-base-race` | `go test -json -count=1 -race -tags=integration -timeout=15m ./internal/app -run ^(TestFirstInstall_ConcurrentStartOneWinner\|TestUS25_FirstInstall_HalfWrittenRosterFailsClosed\|TestDemoPrepare_ETagPinnedAcrossReads)$`（1200s） | 3 |
| `us25-compose` | `us25-compose-roles` | `go test -json -count=1 -tags=e2e -timeout=25m ./internal/e2e -run ^TestUS25_(Compose\|Workers)_`（1800s） | 6 |
| `us25-compose` | `us25-compose-seed` | `go test -json -count=1 -tags=e2e -timeout=25m ./internal/e2e -run ^TestUS25_(Seed\|FirstInstall)_`（1800s） | 4 |
| `us25-docs` | `us25-docs` | `go test -json -count=1 -timeout=5m ./internal/cli -run ^TestDocs_`（600s） | 4 |
| `us25-fault` | `us25-fault-execution` | `go test -json -count=1 -tags=e2e -timeout=40m ./internal/e2e -run ^TestUS25_(Schedule\|Takeover\|DatabaseOutage\|AssetMissing\|Shutdown)_`（2700s） | 5 |
| `us25-fault` | `us25-fault-migration` | `go test -json -count=1 -tags=e2e -timeout=40m ./internal/e2e -run ^TestUS25_(Migration\|Upgrade)_`（2700s） | 4 |

四份 stage 的 `required_tests` 去重后 **50 个具名测试**（53 条声明、3 条跨 check 重复），逐名用「源码里存在 `func <name>(`」校验，**0 缺失**。`-race` 判据在 `us25-base-race`，与 `CGO_ENABLED=0` 发布构建**分属不同命令**。

### 2.2 逆改判别力（逐任务红态）

每条「产品的守点」都有一次**故意破坏 → 逐字转红 → 逐字节还原 → 复绿**的实测；变异施加于源码后立即还原，`grep 临时逆改` 零命中。**未改树、未加抑制注释、未放宽阈值**。

| 守点 | 逆改内容 | 逐字红态 | 记录 |
| --- | --- | --- | --- |
| Demo 绑定读取顺序（§4.6.6） | A：调换 `applyBinding` 内 `readAgentFilesETag` 与 `readAgentContent` 的次序 | `TestDemoPrepare_ApplyBindsAndIngests` / `TestDemoPrepare_ETagPinnedAcrossReads` 红（`"5" is not less than "4"`） | T025；还原后复绿 |
| 同上，断言自身有效性 | A′：首版断言取**首次**出现，计划阶段同样先读 files 会掩盖应用阶段次序——变异 A 下仍绿，证明断言无效；改为取**最后一次**（应用阶段在计划之后） | 修正后变异 A **必红** | T025；复绿 |
| 首次准备顺序（配置 → pending → 意图 → 名册） | B：在 `recordProvisioningPending` 之前提前写名册 | `TestFirstInstall_ProvisioningFailureNeverWritesRoster` / `TestFirstInstall_IntentFailureNeverWritesRoster` 红（`instance.json exists`） | T025；还原后复绿 |

## 3. 交付物核对

模块卡 §3 抽取的 65 条仓库路径逐条判存在性（脚本为临时物，收口时删除）：**64 条存在**；唯一 `MISS` 是 `docker/secrets/master.key`——它是 `docker/export-master-key.sh` 行描述的**运行时导出目标**，`.gitignore:45` 有意排除（`docker/secrets/*` 仅放行 `.gitkeep`），**不是仓库交付文件**，部署时由运维导出生成（`0400`、`10001:10001`）。

| 类别 | 条目 | 结果 |
| --- | --- | --- |
| 新增部署资产 | `docker/compose.multiworker.yaml`、`basalt.worker.yaml`、`export-master-key.sh`、`seed-workspace.sh`、`.env.multiworker.example` | 通过（5/5 存在） |
| 修改部署资产 | `docker/Dockerfile`、`entrypoint.sh`、`init-secrets.sh`；`docker/compose.yaml` 未改（对照） | 通过（3 改 1 冻结） |
| 新增 Go 生产代码 | `internal/app/{first_install,demo_prepare,master_key_check}.go` | 通过（3/3 存在） |
| 修改 Go 生产代码 | `internal/app/{instance_secrets,run}.go`、`internal/cli/{demo_workflow,deps,init,ports,secrets}.go`、`internal/storage/postgres/instance_secrets_store.go`、`scripts/releasekit/{image.go,cmd/releasekit/main.go}` | 通过（10/10 存在） |
| 测试文件 | tracked 改动 8 个 + 新增未跟踪 20 个（`internal/app` 3、`internal/cli` 3、`internal/e2e` 13、`internal/storage/postgres` 1；41 个 `Test*`） | 通过（28/28 存在） |
| 文档四文档面 | `docs/Operations.md`（新增 §8.3）、`README.md`、`release/package-README.txt`、`basalt.yaml.example`；另 `release/notes/TEMPLATE.md` | 通过（5/5 存在） |
| Harness | `harness/stages/us25-{base,compose,docs,fault}.json` | 通过（4/4，各 65 项 protect） |
| schema 与迁移 | `internal/storage/postgres/connect.go:59` `SupportedSchemaVersion = 12`；迁移目录与 `schema.pg.sql` 空 diff | 通过（不新增迁移） |
| 冻结面 | `go.mod` / `go.sum` / `docs/contracts/{schema.sql,schema.pg.sql,openapi.yaml}` / `release/support-matrix.json`（schema 仍为 1）/ `harness/acceptance.json` / `docker/compose.yaml` | 通过（`git diff 3f8d0dc` 全部空输出） |

## 4. 前序回归与判据面

### 4.1 判据面变更（逐路径）

「判据面变更」指改变验收语义的改动（删断言、改判据口径、换执行者、改阈值）。全部逐条列出，供 review ② 复核。

逐函数统计（可复算）：`git diff -U0 3f8d0dc -- '*_test.go' | grep -cE '^\+func Test'` = **13**，`^-func Test` = **0**；另有 **20 个新增未跟踪测试文件贡献 41 个 `Test*`**。tracked 测试改动 8 个文件、**+783/−1 行**（唯一删除行是 `us18_stack_test.go` 中 `OutDir` 字段换行重排，见下表 #5）。新增用例集中在本 US 的首次安装 / 密钥资格 / Demo 准备 / releasekit 暂存 / 容器 e2e 五域。

| # | 路径 | 变更 | 依据 / 裁决记录 |
| --- | --- | --- | --- |
| 1 | `internal/app/instance_secrets_test.go`（+247/−0） | 扩充自动建钥资格判定表：四张业务历史表、`management_operations` 排除、恢复态、既有实例不铸钥；**原断言全部保留** | FR-25-03 / FR-25-04；模块卡 §4.4 |
| 2 | `internal/app/instance_bootstrap_test.go`（+206/−0） | 首次准备顺序 / 幂等 / 并发单胜者 / 名册半截失败关闭；**原断言全部保留** | FR-25-01 / FR-25-05 |
| 3 | `internal/cli/init_test.go`（+125/−0） | `init` 在 `Store.Bootstrap` 之前调用 `FirstInstall.Prepare`；端口为 nil 时失败关闭 | FR-25-01 |
| 4 | `internal/cli/worker_config_test.go`（+45/−0） | Worker 单文件配置在缺控制面专属 secret / 变量时仍可解析 | FR-25-33 |
| 5 | `internal/e2e/us18_stack_test.go`（+7/−1） | `us18BuildImageOnce` 传入 `ImageInputs` 的三个新字段（`WorkerConfigFile` / `ExportKeyScript` / `SeedScript`）；**US-18 的镜像断言不变**，删除行只是 `OutDir` 行位移 | FR-25-32 接口扩展的编译适配 |
| 6 | `scripts/releasekit/image_test.go`（+148/−0） | 新增 `TestUS25_Image_ContextStagesWorkerAssets`（暂存 8 文件、缺件在建镜像前报错、三条 SHA-256 `omitempty`）；**原 image 测试全部保留** | FR-25-32 |
| 7 | `internal/app/{instance_admin_token_test.go,run_test.go}`（+3/+2） | 装配断言随 `run.go` 端口扩展 | 已批准接口适配 |
| 8 | 新增 20 个测试文件（41 个 `Test*`） | `internal/app` 3 个文件 12 例、`internal/cli` 3 个文件 7 例、`internal/storage/postgres` 1 个文件 2 例、`internal/e2e` 13 个文件 20 例（含 2 个夹具文件 0 例）；`scripts/releasekit` 计入 #6 | 模块卡 §4.4 / §4.8 |
| 9 | `internal/cli/docs_first_run_test.go` 的区块结束标记 | `TestDocs_SupportMatrixOnlyComposeMultiworker` 的 Operations 区块结束标记由 `"\n### "` 改为 `"\n## "`：原标记会把 §8.3 之后的无关章节（含 K8s 规划提法）卷进区块；新区块仍只含矩阵小节，断言与词表未减 | T024 判据面说明（run-state §步骤 6） |
| 10 | 模块卡与实现差异 | `C-25-03`：模块卡 §4.6.4 示例骨架写 8 锚点，§4.4 / spec 为 9 锚点，以 §4.4 为准；`C-25-04`：§3.3 写「不发布」，FR-25-14 / data-model 允许「不发布或只绑回环」，实现选回环（T010 裁决）；`F-25-01`：第四服务 `basalt-preflight` 已同步到 data-model / plan / tasks，模块卡冻结正文不改 | run-state T025 前修正表；留给 review ② 追认 |
| 11 | 遗留人工证据 | `D-25-03`：`init-secrets.sh --copy-from/--db-host` 与 `export-master-key.sh` 的人工九组读数在 T010 段，属**人工证据、非自动覆盖**；`D-25-07`：`TestUS25_Migration_CrossSchemaRefusal` 未列入 `required_tests`（§4.9 规则 2 需用户确认），由 fault stage 正则实际执行 | run-state T025 前修正表；留给 review ② 裁决 |

**spec / plan / tasks 与 run-state §1.2 冻结哈希的漂移**（如实登记）：`spec.md` 现值 `c9ad17bd…`（冻结 `f2642e4d…`，237 行不变）、`plan.md` 现值 `4a2fa6ac…`（冻结 `2f311be4…`，158 行不变）、`tasks.md` 现值 `bc91e9f9…`（冻结 `600534b2…`，150 → 152 行）。差异全部来自 already-approved 范围内的 analyze / clarify 派生修正（`A-25-02` 澄清 78 是 entrypoint 映射、`C-25-02` 空值语义、`D-25-05` T010/T011 映射、`D-25-07` 注释、`F-25-01` 第四服务），**不改范围、权限、公共契约与验收语义**；逐条见 run-state「T025 前修正」各表。

### 4.2 前序模块回归

见 §1.1 `test` / `test-integration` / `cover` 三个目标的读数：`go test -race -cover ./...` 与 `go test -tags=integration ./...` 全仓执行，前序 24 个 US 的用例与 US-25 新增用例在同一轮判定（40 + 42 包全 ok、0 FAIL）。base stage 另外重跑前序承重用例：`TestInstanceSecrets_*`（US-18 自动建钥）、`TestCLI_Init*`、`TestDemoWorkflow*`、`scripts/releasekit` 既有镜像用例。`docker/compose.yaml`（US-18）、`release/support-matrix.json`、`harness/acceptance.json` 均空 diff；`us18_stack_test.go` 只做新字段适配（#5），断言不变。

### 4.3 收口后的 CI 修复与历史 stage 回归（2026-11-08）

收口提交 `c0a5e92` 推送后，main CI 运行 [42485963668](https://github.com/ymedword/basalt/actions/runs/42485963668) 的 test job 判红，其余六个 job（console / build / website / postgres / flow / container）全绿；判红点只有一个：累计 stage 的 `015-backfill` 里 `TestBackfill_Bootstrap/status_no_config` 输出缺少「未发现配置」。

- **根因（已复现）**：该用例是 US-1 的 e2e 夹具，仍假设 `basalt init` 不写配置文件；US-25 的 FR-25-01 批准了相反的行为（init 在 SQLite `Bootstrap` 之前写出 `basalt.yaml`）。本地按 `harness/stages/current.json` 的 `015-backfill` 原始命令复跑，得到同一失败子测试与同一文案（包耗时 181.53s 判红）。
- **修复（判据面不变）**：夹具在 init 之后、断言之前移除生成的 `basalt.yaml`，用例继续判「有骨架与源库、没有配置文件」的工作区（存量工作区，或运维删掉配置的现场）；断言、退出码与期望文案逐字未改。提交 `e98d55e`。
- **同题第二处**：历史 stage `us14-14d` 的 E10（`TestUS14D_ExistingNoConfigUntouched`）有同一类假设——它用 `init` 构造「存量实例缺配置」的夹具。修复改为移除 init 生成的两份文件（`basalt.yaml`、`.basalt/instance.json`）、保留源库，重建 US-25 之前的现场；存量判定（库在 → 非新实例）与全部断言逐字不变。提交 `4b7b246`。
- **本地验证**：修复后按 stage 原始命令复跑 `015-backfill` 全绿（0 失败，包耗时 163.41s）；`^TestUS14D` 组 10 个 `required_tests` 中 9 个通过（含修改前判红的 E10），`FixtureEndToEnd` 与 `TestUS14Demos_Fixture` 因非 root 绑 `127.0.0.1:80` 在夹具阶段失败（未调用任何产品路径），登记为本地未验证（run-state §4）；`gofumpt -l` 无差异，`go vet -tags=e2e,slow,real ./internal/e2e` 退出 0。
- **修复后的 CI（2026-11-08 回填）**：最终 HEAD `4b7b246` 的运行 [42492426068](https://github.com/ymedword/basalt/actions/runs/42492426068) **全 7 个 job 绿**。首次修复 `e98d55e` 的运行 [42492076028](https://github.com/ymedword/basalt/actions/runs/42492076028) 的 test job 已绿（证明 `015-backfill` 修复生效），仅 postgres job 红一次：`TestFlow_ContextKeepsHardConstraintsAndFailsClosed/overflow` 报 `write tcp 127.0.0.1:5432: i/o timeout`，同一套件在 `c0a5e92` 与 `4b7b246` 的运行中均绿，且本次改动只动 e2e build tag 文件、不参与 `-tags=integration` 编译——按 CI 基础设施颤动登记（run-state §2），不写成代码缺陷、不重试掩盖。
- **影响面**：只改两个 `_test.go` 文件（`internal/e2e/bootstrap_test.go` +10、`internal/e2e/us14_builtin_test.go` +15/−1），不改产品代码、契约、依赖、迁移与设计工件；四份 stage 报告与冻结指纹的依据仍是交付树 `c0a5e92`，见 §9 追加说明。

## 5. 全局不变量自查（机器可判）

逐条复核命令与当场读数（2026-11-08，冻结树；命令保留于本表可按行复跑，过程产物收口时已删）。

| # | 不变量 | 复核命令 | 读数 |
| --- | --- | --- | --- |
| 1 | 坐标零 Agent 框架 | `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` | 零命中 |
| 2 | `context.WithValue` 只允许追踪标识 | `grep -rn "context.WithValue" --include='*.go' cmd internal` | 生产代码仅 `internal/web/middleware.go:41/:79`（request-ID）+ 两处注释（`internal/core/lease.go:96`、`internal/web/middleware_business_auth.go:6`）；无业务状态 |
| 3 | 审计两表 day-one 且写路径唯一 | `grep -rnE "INSERT INTO (llm_calls\|tool_invocations)" --include='*.go' internal` 非 `internal/storage` 命中 | 全部为 `_test.go`（`internal/app` / `internal/dbmigrate` / `internal/e2e`）；三个新生产文件（`first_install.go` / `demo_prepare.go` / `master_key_check.go`）**零直接 SQL 写**；建钥预写 pending 审计由 `TestFirstInstall_KeyCreationAudited` 守卫 |
| 4 | 凭证零明文 | `grep -rnE "sk-[A-Za-z0-9]{16,}" --include='*.go' --include='*.yaml' .` | 零命中 |
| 5 | 无 ORM / 无自动迁移 | `grep -nE "gorm\|entgo\|ent\.io\|xorm" go.mod` | 零命中；迁移目录空 diff |
| 6 | 单二进制 `CGO_ENABLED=0` | `CGO_ENABLED=0 go build ./...` | `BUILD_OK`；另见 §1.1 `build`（41,234,592 字节） |
| 7 | 时区禁用词 | `grep -rnE "time\.Local\|[.]Local[(][)]\|time\.Now[(][)]\.Location[(][)]" --include='*.go' .`（排除 `_test.go`） | 零命中 |
| 8 | 随机源红线 | `grep -rn '"math/rand' --include='*.go' cmd internal` | 零命中（注释提及 `crypto/rand` 红线不计数） |
| 9 | 点导入零命中 | `grep -rnE '^import[[:space:]]+\.\|^[[:space:]]+\.[[:space:]]*"' --include='*.go' cmd internal` | 零命中 |
| 10 | `core` 只依赖标准库 | `GOPROXY=https://goproxy.cn go list -f '{{.Imports}}' ./internal/core` | 26 个标准库包（直接导入），零第三方、零 `internal/...` |
| 11 | LLM 审计入口唯一 | `grep -rn '\.BeginCall(' --include='*.go' internal/core` | 零命中（`core` 只声明接口，调用点在 `internal/provider`） |
| 12 | 前端构建不进 Go 门禁 | `grep -nE "node\|npm" Makefile scripts/check-acceptance.sh scripts/check-acceptance.ps1` | 零命中 |
| 13 | SQLite 不是运行期存储 | `grep -rn "modernc.org/sqlite" --include='*.go' internal/app internal/web` | 仅 6 个 `_test.go`（测试夹具）；生产零命中 |
| 14 | 零新增依赖 | `git diff 3f8d0dc --stat -- go.mod go.sum` | 空输出（0 行） |

## 6. 复核结论与剩余人工项

### 6.1 独立复核（逐条处置）

独立只读复核（与实现分离）产出 **23 条唯一发现**，其中 CONFIRMED **3 条（2×HIGH、1×MEDIUM）**全部修复，其余按「已处置 / 接受 / 留给 review ②」逐条记录。**未**改 `required_tests`、未删断言、未改模块卡冻结正文。

| 级别 | 发现 | 处置 | 证据（当前树） |
| --- | --- | --- | --- |
| HIGH | `docker/export-master-key.sh` 先 `mkdir`/`chown`/`chmod` 后校验 dest，失败会留下半改目录 | 已修：全部前置条件（dest 形态、系统目录、符号链接、已存在、卷、密钥可读）前移到任何目录改动之前 | `sh -n` 通过；T010 段人工九组读数来自修改后脚本 |
| HIGH | Operations / README 的 master key 命令在仓库根执行会得到 `docker/docker/secrets` | 已修：四个文档面统一为 `(cd docker && sudo sh export-master-key.sh basalt secrets/master.key)`，dest 相对 `docker/` | `docs/Operations.md` 步骤 2、`README.md`、`release/package-README.txt` |
| MEDIUM | Operations 种子步骤用宿主占位路径，实际是命名卷 | 已修：停机 → `compose run --rm --no-deps basalt-control assets install` → 一次性 root 容器导出 `basalt_control-workspace:/src` → `--profile worker create --no-recreate` → 导入 `basalt_worker-workspace:/data` | `docs/Operations.md` §8.3 步骤 5（行 1536–1559）；与 `internal/e2e/us25_fixture_test.go` 实测容器调用一致 |
| MEDIUM | 四份 stage `protect` 缺 4 个新建判据面 | 已修：各补 `demo_prepare_integration_test.go` / `instance_secrets_managed_write_test.go` / `master_key_check_test.go` / `demo_workflow_prepare_test.go`，各 **65 项** | `json.load` 计数四份均 65；stage lint 通过 |
| MEDIUM | §4.6.6 读取顺序无逆改点 | 已修：`demo_prepare_test.go` 增 `readOrder` 记录与 `requireFilesReadBeforeContent`；逆改实证见 §2.2 | 两用例红/绿读数 |
| LOW | 支持矩阵 `schema` 值无断言 | 已修：`docs_first_run_test.go` 增 `schema == 1` 断言 | `internal/cli/docs_first_run_test.go` |
| LOW | 种子导入目标侧无符号链接拒绝、`chmod` 过宽 | 已修：目标侧（target / `.basalt` / `find -type l`）符号链接拒绝；可读性 `chmod` 移到暂存树 | `docker/seed-workspace.sh`；`sh -n` 通过 |
| LOW | 预检 IPv6 等价拼写漏拒 | 已修：归一化判定拒绝 `[::0]` / `:::000` / 全零段 / `::ffff:0.0.0.0`，保留 `::1` | `docker/compose.multiworker.yaml` 预检段；`us25_compose_test.go` 子用例 |
| LOW | `compose exec` 缺 `-f`、缺 DSN 导出 | 已修：步骤 6 补 `-f docker/compose.multiworker.yaml` 与 `BASALT_DATABASE_DSN="$(cat /run/secrets/database_dsn)"` | `docs/Operations.md` |
| LOW | 资产安装步骤顺序错（运行中装不了） | 已修：资产安装移到停机维护窗口（实例锁），种子导出在其后；Worker 不执行资产安装或发布（FR-25-22） | `docs/Operations.md` §8.3 步骤 5 / 7；`README.md`；`release/package-README.txt` |
| LOW | 种子后配置一致性未写 | 已修：四处文档面写明种子不含 `basalt.yaml`，providers / embedding / flows 必须一致 | 四个文档面 |
| LOW | `.env` 目标目录不明 | 已修：步骤 1 明确 `docker/.env` 与 `-f compose.multiworker.yaml exec` | `docs/Operations.md` |
| LOW | 材料表未列 ingress / IMAGE / BIND | 已修：材料表补全六项与一致性要求 | `docs/Operations.md` |
| LOW | tasks T003 映射与 T025 裸 stage 名 | 已修：T025 改为仓库相对路径并补四 stage 覆盖说明 | `specs/025-compose-multiworker/tasks.md` |
| LOW | DemoPrepare 构造失败留下 typed-nil 接口 | 已修：`run.go` 用真正的 nil 接口（`var demoPrepare cli.DemoPrepare`） | `internal/app/run.go` |
| LOW | 镜像 digest 预检表述过强 | 已修：降级为部署纪律（预检不校验 digest 形式） | `docs/Operations.md` §8.3 |
| 接受 / 留给 review ② | `TestUS25_Migration_CrossSchemaRefusal` 未列入 `required_tests`（§4.9 规则 2 需用户确认）；`C-25-03` / `C-25-04` / `F-25-01` 为模块卡与实现差异说明 | 不改判据面；由 fault stage 正则实际执行并在此登记 | run-state §2 / 本文件 §4.1 #10–#11 |

### 6.2 静态门禁妥协点（需 review ② 确认）

- **新增抑制注释 1 处（仅测试代码）**：`scripts/releasekit/image_test.go:433` `// #nosec G304 -- 测试自建的暂存目录`。该读取对象是测试自己 `MkdirTemp` 出来的上下文目录，非用户输入；生产代码零新增抑制。
- 树内抑制注释计数：基线 `3f8d0dc` **198** → 本轮工作树 **199**（`git grep -nE '(#nosec|//nolint|//lint:ignore)' -- '*.go' | wc -l`；差异即上面 1 处）。
- **新增 `t.Skip` 1 处（Windows 分支，Linux 门禁不跳过）**：`internal/app/master_key_check_test.go:34` `t.Skip("Windows 权限位为合成值，DACL 校验见 credential_crypto_windows_test.go")`——0700/0755 权限位判据只在 Unix 有意义；WSL/Linux 的 `make check` 与 base stage 正常执行该用例。
- 未提高超时、未放宽覆盖率阈值、未删除测试；`test-integration` / `test-pg` 仍 fail-closed（缺 DSN 即失败）。

### 6.3 人工验证项（模块卡 §6.1，12 项）

模块卡 §6.1 把 1–9 列为人工项；实现将其纳入容器级 e2e（T013–T021，WSL2 Docker，本地协议桩与冻结夹具），判据未降低、证据为机器读数。**四态按证据强度如实标注**：1–9 记「通过（自动等价证据）」；10–12 保持未验证 / 未测量。

| # | 项目 | 四态 | 证据 / 说明 |
| --- | --- | --- | --- |
| 1 | 全新目录 init + serve | **通过（自动等价证据）** | `TestUS25_FirstInstall_InitThenServe`：`init` 写配置 / 名册 / 首次进度，首次 `serve` 生成主密钥、打印一次 token、不重复安装 |
| 2 | 缺 `BASALT_INGRESS_*` 首次失败后重试 | **通过（自动等价证据）** | `TestUS25_FirstInstall_RetryAfterIngressExit`：第二次启动补发主密钥，无「有实例无密钥」死局 |
| 3 | 单宿主 Compose 双角色 | **通过（自动等价证据）** | `TestUS25_Compose_TwoRolesAndVolumes` / `_SecurityInvariants` / `_ReadinessAndRestartPolicy`：`--profile control` 起控制面 + PG、`--profile worker` 起 Worker、Worker 无 8080 监听 |
| 4 | 种子导入 | **通过（自动等价证据）** | `TestUS25_Seed_ManifestAndTamper` / `TestUS25_Workers_BothRegistered` / `TestUS25_FirstInstall_WorkerSeedSkipsBootstrap`：两行名册、生命周期索引与 UID 一致 |
| 5 | 控制面停机接管 | **通过（自动等价证据）** | `TestUS25_Takeover_FencesOldHolder` / `TestUS25_Schedule_ExecutesOnceAcrossWorkers`：旧执行只收尾、零重放、旧 holder 回写被 fencing 拒绝；**有限观察窗口，不设产品接管 SLA** |
| 6 | PostgreSQL 短暂中断 | **通过（自动等价证据）** | `TestUS25_DatabaseOutage_NoReplay`：恢复后不自动重放，Run 终态与审计一致 |
| 7 | M1：SQLite v6 → PG v12 | **通过（自动等价证据）** | `TestUS25_Migration_V6ToV12VerifiesObjects` / `TestUS25_Migration_KnowledgeAbsentRecorded`：九步、`db verify --handoff`、源无对象逐项登记、知识引用 0 且写明原因 |
| 8 | M2：单实例 PG → 多 Worker | **通过（自动等价证据）** | `TestUS25_Migration_SingleInstanceToMultiworker`：种子启动、生命周期索引与 UID 保留、准入通过、计数一致、旧 bundle 可读；非空 Flow 与学习审批完整性在 M2 判定 |
| 9 | 两节点升级 / 回滚 | **通过（自动等价证据）** | `TestUS25_Upgrade_TwoNodeAbortAndRollback`：旧进程全停 → 备份 → 新控制面迁移 → 新 Worker；同 schema（v12）验证停全部旧进程、制品一致、整组回滚；跨 schema 拒绝由 `TestUS25_Migration_CrossSchemaRefusal` 正则覆盖（D-25-07） |
| 10 | 文档走查（新操作者从零走八个步骤） | **未验证** | 锚点与顺序由 `TestDocs_FirstRunChecklistOrder` 自动断言（通过）；**人工按文档从零走查未执行**，归用户 review ② |
| 11 | 隔离声明核对 | **未验证** | 四处文档的 opt-in 声明由 `TestDocs_IsolationOptInWarning` 自动断言（通过）；「执行档命令与回退 local 命令可用」未实际执行（容器隔离归 US-20 / US-25B） |
| 12 | 真实双宿主 A+B | **未测量** | 按范围不做；首发后按 `career/DeploymentTrialPlan.md` 执行，不把同机 / 模拟证据当跨机证据 |

### 6.4 收口前 analyze 复跑（三件套第一项）

2026-11-08 只读复跑，分析对象 = `spec.md` + `plan.md` + `tasks.md` + `research.md` / `data-model.md` / `contracts/README.md` / `quickstart.md`，对照模块卡 R3 §2.5 P-01～P-18、§3 交付物、§4 验收映射与宪法十条。完整记录在 run-state「T025 前修正」表，摘要：

| 项 | 读数 |
| --- | --- |
| CRITICAL | **0**（无宪法冲突、无零覆盖 FR、无缺失核心工件） |
| 宪法抽查 | `go.mod` / `go.sum` 与起点空 diff（原则 8）；不新增迁移、`SupportedSchemaVersion = 12`（原则 9）；无新增后台循环（原则 7）；`core` 零第三方（架构约束） |
| 覆盖 | 33 FR / 8 SC 与模块卡一致；20 任务与 11 个 check 一一对上；`required_tests` 50 个唯一名全部存在（53 条声明）；模块卡 §3 的 64 条交付物路径全部存在 |
| findings 处置 | 16 条（C-25-01～F-25-01 + 独立复核 2 条）逐条处置：protect 37→61→65、T010/T011 映射、IPv6 归零拒绝、空地址负例、`.env` 与 preflight 断言等；**未改 `required_tests`**（§4.9 规则 2） |
| 结论 | **CRITICAL 清零，复跑通过**，随即进入 T025 全量门禁 |

### 6.5 七项证据对照（模块卡 §6.3）

| # | 证据 | 落点 | 读数 / 结论 |
| --- | --- | --- | --- |
| 1 | 自动化门禁 | §1.1 / §1.2 | `make check` 七目标全绿（退出 0）；四份 stage 全部 `passed`、exit 0，同一指纹；`test-pg` 与 `-race` 分属不同命令，与 `CGO_ENABLED=0` 的 `build` 不同 job |
| 2 | 多 Worker 行为 | §6.3 #3–#9 | 双角色、注册、种子、单次执行、接管收尾零重放、停机顺序、迁移与升级均为容器级 e2e 读数 |
| 3 | 故障矩阵 | §6.3 #5/#6/#9 | 控制面停机接管、PG 中断不重放、资产缺失拒绝新 Run、停机释放租约、两节点升级 / 整组回滚 |
| 4 | 审计与诊断 | 见下 | 见下方三段 |
| 5 | 契约、文档与依赖 | §3 / §4.1 | 契约与 `schema.pg.sql` 空 diff；`support-matrix.json` schema 仍为 1 且未改；`git diff 3f8d0dc -- go.mod go.sum` **0 行** |
| 6 | 未验证清单 | run-state §4 | 真实双宿主 / 真实模型 / 真实 webhook / K8s / DB HA / 容器隔离 / Windows 原生 L3 / 发布 tag 逐项标未验证或未测量 |
| 7 | 独立复核 | §6.1 | 23 条唯一发现全部处置，3 条 CONFIRMED 修复；未把机器通过当作人工接受 |

**证据 4（审计与诊断）的三段读数**：

- **建钥审计唯一入口**：自动建钥的创建动作先写 `pending` 管理操作审计、失败即不铸钥，由 `TestFirstInstall_KeyCreationAudited` 守卫；三个新生产文件零直接 SQL 写入（§5 #3），全部经既有仓储 / 管理面路径。
- **业务历史判定只读**：`HasRuntimeHistory` 只读 `runs` / `task_executions` / `agent_lifecycle` / `kb_documents` 四表，`management_operations` 明确排除（`TestRuntimeHistory_ManagementOperationsExcluded`）。
- **无第二套写入路径**：Demo 准备经既有管理面 HTTP API（`files` / `knowledge` / `agents`）执行，按项输出结果；Worker 启动不写 `instance.json`、不铸 admin token、不建 SQLite（`TestUS25_FirstInstall_WorkerSeedSkipsBootstrap` 与 FR-25-06 用例）。

### 6.6 收口结论

| 项 | 四态 | 依据 |
| --- | --- | --- |
| 宪法口径 `make check` 七目标 | **通过** | §1.1（2026-11-08 冻结树，退出 0，覆盖率全部达标） |
| 全量 base stage（公共 profile + 6 项） | **通过** | §1.2（17/17 passed、exit 0、冻结指纹） |
| docs stage（1 项） | **通过** | §1.2（1/1 passed、exit 0、同一指纹） |
| compose stage（2 项） | **通过** | §1.2（2/2 passed、exit 0、同一指纹） |
| fault stage（2 项） | **通过** | §1.2（2/2 passed、exit 0、同一指纹） |
| 覆盖率底线 | **通过** | §1.3（六个 ≥70% 包全部达标，最低 `postgres` 72.21%） |
| 交付物核对 | **通过** | §3（64 条仓库路径存在，1 条运行时目标按 .gitignore 设计不存在） |
| 独立 AI 复核 | **通过（23 条全处置）** | §6.1 |
| 收口前 analyze 复跑 | **通过（CRITICAL 0）** | §6.4 |
| 人工项 1–9（自动等价证据） | **通过** | §6.3 |
| 人工项 10–11（文档走查 / 隔离声明核对） | **未验证** | §6.3：自动断言通过，人工走查未执行；归 review ② |
| 人工项 12（真实双宿主） | **未测量** | §6.3：按范围不做 |
| 人工 review ②（US 收口） | **通过（用户预授权直通，2026-11-08）** | 用户 2026-11-08 明确「review2 收尾时我授权可以直接通过，并且提交代码推送到 main 分支，要跟踪 CI 直到变绿」；授权原话与执行记录见 run-state §5 |
| 推送后 CI | **待跟踪（不在本报告结论内）** | 提交 / 推送后由 run-state §5 记录运行号与结果；变红先定位修复，不把进行中写成通过 |

**明确仍未验证**（不因本文件通过而翻转）：真实双宿主（A+B 云服务器）执行、接管与隔离；真实模型与真实 webhook；K8s 单实例（US-23）与 K8s 多 Worker（US-25A）；数据库 HA；容器隔离（OCI/gVisor）验证与 Demo 双模适配（US-20 / US-25B）；Windows 原生 L3 容器用例；`docs/GettingStarted.md` 的 T023 编辑（文件被 `.git/info/exclude` 排除，非交付面）；正式发布 tag / 签名 / Release 上传。逐项见 run-state §4。

## 7. 复跑与交接

```bash
# 0) 环境（WSL2；本地 PG 由 with-postgres.sh 注入 DSN，不打印凭据）
cd /mnt/d/code/basalt

# 1) 宪法口径七目标（fmt lint sec test test-integration cover build）
dev-env/scripts/with-postgres.sh make check

# 2) 全量 base stage（公共 profile + us25-base 六项）
dev-env/scripts/with-postgres.sh sh scripts/check-acceptance.sh \
  --stage harness/stages/us25-base.json --executor ai --run-kind original

# 3) 三个 stage-only（docs / compose / fault）
dev-env/scripts/with-postgres.sh sh scripts/check-acceptance.sh \
  --stage harness/stages/us25-docs.json --stage-only --executor ai --run-kind original
dev-env/scripts/with-postgres.sh sh scripts/check-acceptance.sh \
  --stage harness/stages/us25-compose.json --stage-only --executor ai --run-kind original
dev-env/scripts/with-postgres.sh sh scripts/check-acceptance.sh \
  --stage harness/stages/us25-fault.json --stage-only --executor ai --run-kind original

# 4) 冻结面：期望空输出
git diff 3f8d0dc -- go.mod go.sum internal/storage/postgres/migrations \
  docs/contracts/schema.pg.sql docs/contracts/openapi.yaml \
  release/support-matrix.json harness/acceptance.json docker/compose.yaml
```

交接说明：`specs/025-compose-multiworker/` 下 `run-state.md` 记录逐任务证据、失败台账与未验证清单，本文件只收口。正文提到的 `/tmp/us25-*` 与 `D:\temp-us25-fp\*` 是过程日志、重算脚本与探针产物，收口时已按临时物纪律删除；`.loop/runs/*/report.json` 是 harness 运行记录（`.loop/` 整体不进版本库），其判读读数已抄进本文件与 run-state。

## 8. review ② 处置（用户授权直通）

用户 2026-11-08 预授权：**review ② 收尾时直接通过，并由 AI 代执行提交、推送 `main` 与 CI 跟踪**（「review2 收尾时我授权可以直接通过，并且提交代码推送到 main 分支，要跟踪 CI 直到变绿」）。本报告是该授权下的收口依据：

- **无新增修复**：§6.1 的三条 CONFIRMED 已在 T025 前修复并复验；§4.1 #9–#11 的判据面 / 差异说明由本报告显式登记，供 review ② 追认。
- **授权边界**：仍不包含 tag、正式发布、部署与合并到其他分支之外的不可逆外部动作；发布仍按 US-17 流程由用户给出版本并触发。
- **提交与 CI**：提交 SHA、推送结果与 CI 运行号记录在 run-state §5（过程状态），本报告不预写未发生的 CI 结论。

## 9. 回填后指纹差异证明

`acceptance.md` 与 `run-state.md` 在本轮四份 stage 报告生成之后写入。按指纹算法：`run-state.md` 按 basename 排除，tasks 勾选被归一化（`^(\s*- \[)[ xX]\]` → `$1 ]`），因此重算指纹相对冻结清单**只应新增本文件一行**。逐文件差异（`diff` 冻结清单与回填后清单）：

```text
$ diff /tmp/us25-fp-freeze.txt /tmp/us25-fp-after.txt
2134a2135
> specs/025-compose-multiworker/acceptance.md\t-rwxrwxrwx\te9e26acb98a1fb57e5352be0a6770c1a023e378a97ffaeb1d963c9fc825821f2

清单行数 2217 → 2218；聚合指纹：冻结 d7c285f22bff1fbcff365c5eebdfc17eaee420227687a35305894dcc738575d6 → 回填后（本节写入前）694409daa3fcd274ddb089c9003efdc5ac353dac7bc03bdcffe89e13ba1d9564。
```

（原始 `diff` 输出以 TAB 作列分隔，上面以 `\t` 转义显示；路径与哈希逐字保留。）

本节写入会改变本文件自身的内容哈希，因此上面的逐文件哈希与聚合指纹是**本节写入前**的快照，不再回填（避免自指）。写入本节后再次重算清单并对比冻结清单：差异集合仍为且仅为 `specs/025-compose-multiworker/acceptance.md` 一条路径，无其他增删；复核判据是“diff 恰好一行且路径为本文件”，不以某个固定哈希值为判据。同一判据的机器证据是四份 `report.json` 的 `fingerprint` 字段同为 `d7c285f2…`（本轮四份 stage 在同一冻结树上执行）。

**收口后修复的范围说明（2026-11-08 追加）**：本节指纹快照取自交付树 `c0a5e92`。其后的两个修复提交只改 `internal/e2e/bootstrap_test.go`（+10）与 `internal/e2e/us14_builtin_test.go`（+15/−1），都是测试夹具（§4.3）；现在重算清单会在这两条路径上与本文件一起出现差异——这是收口后修复的预期结果，不是交付树漂移。产品代码、契约、迁移与设计工件未变，四份 stage 报告仍对应 `c0a5e92` 树。
