# Tasks: US-1 工程地基

**Input**: Design documents from `/specs/001-foundation/`

**Prerequisites**: [plan.md](./plan.md)（必需）、[spec.md](./spec.md)（用户故事与优先级）、[research.md](./research.md)、[data-model.md](./data-model.md)、[contracts/](./contracts/)

**Tests**: **必须包含**——本项目「harness 先行」是硬纪律：颗粒度文档 §3.2 列出全部用例，实现与测试一起落地（implement 阶段不允许先实现后补测）。

**Organization**: 任务按用户故事分组，每个故事可独立实现与独立验证。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属用户故事（US1 / US2 / US3）——Spec-Kit 对**本 feature 内部**用户故事的编号（对应 spec.md 的三个 User Story），**不是**项目台账（`docs/us/REGISTRY.md`）里的 US-1 / US-2 / US-3；本 feature 的三个故事同属项目 US-1（工程地基）
- 每条任务都给出精确文件路径

## Path Conventions

单 Go module：`cmd/basalt/`、`internal/`、`scripts/`、仓库根（`makefile` 的兄弟文件）。测试与被测包同目录（Go 惯例）。

---

## Phase 1: Setup（共享前置）

**Purpose**: 仓库级工程文件与依赖核实——不涉任何用户故事

- [x] T001 第三方坐标与 API 本地核实：`GOPROXY=https://goproxy.cn go list -m -versions` 复核四个坐标（cobra v1.10.2 / goccy/go-yaml v1.19.2 / modernc.org/sqlite v1.60.1 / testify v1.12.1）；随后装模块后核实 research.md「写前必须实测核实清单」#1/#2/#3/#7/#8（modernc DSN `_pragma` 语法、`BEGIN IMMEDIATE` 取得姿势、多语句脚本执行、cobra `--version` 输出、gosec 版本回显），读数回填 `specs/001-foundation/research.md`——核实不到即软门禁停下报告
- [x] T002 [P] 仓库工程文件：`.gitignore` 增补（已在库中且已忽略 `.basalt/` 与 `/bin/`——本任务只需补 `basalt.yaml` 实例配置排除项）+ 新建 `basalt.yaml.example`（六段骨架、`http.addr: 127.0.0.1:8080`、凭证一律 `${ENV_VAR}`）
- [x] T003 [P] `internal/` 十包骨架：`internal/{core,cli,app,workspace,storage,provider,memory,tool,channel,web}/doc.go`（仅包声明 + 中英并列包注释、中文在前；`provider` / `memory` / `tool` 本 US 无语句）

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: 模块、装配骨架与测试基建——三个故事都建立在其上

**⚠️ CRITICAL**: 本阶段完成前，任何用户故事都不得开工

- [x] T004 `go.mod` / `go.sum`：`go 1.27`（最低版本要求）+ `toolchain go1.27.2` + 四个坐标（版本以 T001 实测为准）；**不得**写 ORM 或 Agent 框架坐标
- [x] T005 `internal/core`：`core.go`（`SchemaIntrospector` 接口 / `MetricsRegistry` 接口 + `NoopMetrics`）+ `errors.go`（五个哨兵错误）——只依赖标准库，是依赖倒置的接口提供方
- [x] T006 `internal/app`：`run.go`（`Run(ctx, args) int` 装配骨架 + 退出码唯一出口）+ `logger.go`（`NewLogger(cfg *cli.Config) *slog.Logger`，dev 文本 / prod JSON 双档）
- [x] T007 `internal/cli`：`root.go`（`NewRootCommand(deps Deps) *cobra.Command` + `--version` 全局选项 + `init` / `status` 注册点）+ `deps.go`（`Config` / `Deps` 显式类型化载体：`WorkDir` / `SupportedSchemaVersion` / `Logger`）
- [x] T008 `cmd/basalt`：`main.go`（唯一 `main`：`os.Exit(run(os.Args[1:]))` + `import _ "time/tzdata"`）+ `run.go`（薄入口包装 `run(args []string) int`，main 之外零逻辑）
- [x] T009 `internal/testutil`：`workspace.go`（`TempWorkspace(t *testing.T) string`）+ `httpserver.go`（`NewHTTPServer(t *testing.T, h http.Handler) *httptest.Server`）+ `testutil_test.go`（`TestTestutil_HTTPServer`——本地回环、不访问真实网络）

**Checkpoint**: 骨架就位，三个用户故事可按优先级推进

---

## Phase 3: User Story 1 - 一条命令验证整个工程（Priority: P1）🎯 MVP

**Goal**: 从干净检出到一条聚合门禁命令七目标全绿 + `CGO_ENABLED=0` 静态单二进制（`--version` 含注入版本）

**Independent Test**: 运行 `make check` 判退出码与关键输出；负向对照（未格式化文件 / 语法错误文件）必须判红；`./bin/basalt --version` 含注入值

### Tests for User Story 1

- [x] T010 [P] [US1] `scripts/stmtcount/count.go` + `main.go`（标准库 `go/parser` 统计按包可执行语句数；薄 `main` + 可测 `run` 包装）
- [x] T011 [P] [US1] `scripts/check-coverage.sh`（按包阈值判失败：`core` / `tool` / `memory` / `storage` ≥ 70%、其余 ≥ 50%；有语句无测试包计 0% 不豁免；无语句包跳过；支持 `--profile` / `--tree` 注入）
- [x] T012 [US1] 脚本自检用例：`scripts/check_coverage_test.go`（六类判据：低于阈值失败 / 恰好达标通过 / 无语句跳过 / 有语句无测试计 0 / 测试命令失败传导 / 输入异常报错）+ `scripts/stmtcount/count_test.go`（`TestStmtCount_CountsExecutableStatements` / `_NoStatements` / `_ParseError`）
- [x] T013 [P] [US1] `cmd/basalt/run_test.go`（`TestEntryRun_VersionFlag`：`--version` 退出 0 且输出含注入版本；`TestEntryRun_UnknownCommand`：非零）
- [x] T014 [P] [US1] `internal/app/run_test.go`（`TestRun_ExitCodes`：成功 0 / 失败 1 分派）
- [x] T015 [P] [US1] `cmd/basalt/tzdata_test.go`（`TestTZData_LoadLocationPortable`：进程内 `Asia/Shanghai` 解析成功——默认层**不**判隔离有效性）
- [x] T016 [P] [US1] `cmd/basalt/tzdata_isolated_test.go`（`//go:build tzisolated`：`TestTZData_IsolatedEmbeddedFallback` 主组 + `TestTZData_IsolatedControlWithoutImport` 对照——对照**必须以「时区加载失败」失败**：退出码 3 + `TZ_LOAD_FAILED` 标记；探针 main 契约以注释保留）

### Implementation for User Story 1

- [x] T017 [US1] `Makefile` 七目标（`check = fmt lint sec test test-integration cover build`）：`fmt` 判卷**先查退出码再判输出**（gofmt 与 gofumpt 各查一遍）；`lint` = `go vet` + staticcheck 显式判定集 `-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022`；`sec` = gosec + govulncheck；`test` = `go test -race -cover ./...`；`test-integration` = `go test -tags=integration ./...`；`cover` = 调 `scripts/check-coverage.sh`；`build` = `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$$(git describe --tags --always)" -o bin/basalt ./cmd/basalt`（`$$(...)` 必须双写；顶部 `SHELL := /bin/sh`）
- [x] T018 [US1] `.github/workflows/ci.yml`：`test` job（默认 CGO，跑 `make fmt lint sec test test-integration cover`）+ `build` job（`CGO_ENABLED=0` 四平台交叉编译，**不得**跑 `-race`）；两 job 钉 `go-version: 1.27.2`；四检查工具 `go install` 固定版本（module 路径：`mvdan.cc/gofumpt@v0.12.0` / `honnef.co/go/tools/cmd/staticcheck@v0.8.2` / `github.com/securego/gosec/v2/cmd/gosec@v2.29.0` / `golang.org/x/vuln/cmd/govulncheck@v1.9.0`）；test job 内含 tzdata 隔离容器步骤（删系统 zoneinfo 与 GOROOT `zoneinfo.zip` → **断言删除成功**（fail-closed）→ 跑 `-tags=tzisolated`）
- [x] T019 [US1] 门禁自证：本机跑 `make check` 全绿并留存关键输出；负向对照实测（未格式化文件 → 非零且点名；语法错误文件 → 非零且输出为空——**判卷顺序**读数当场记录）
- [x] T020 [US1] 发布构建实测：`CGO_ENABLED=0` 构建通过 + `./bin/basalt --version` 输出含注入值（读数记录）；（Linux 产物的「无动态依赖」静态断言属**人工项**——本机 Windows 无法直接验 Linux 二进制，归 T048 剩余人工项清单，不在本任务内伪装成已验）

**Checkpoint**: US1 独立可验收——门禁一条命令全绿、产物可版本化识别

---

## Phase 4: User Story 2 - 一键初始化工作区且二跑幂等（Priority: P2）

**Goal**: `basalt init` 幂等生成工作区骨架（6 目录 + 5 文件）与数据库（11 表 + `schema_version` = 1）；并发引导恰好一个创建者

**Independent Test**: 临时目录连续 init 两次比对清单与文件内容（逐字节）；清点表与版本记录；跑并发用例

### Tests for User Story 2

- [x] T021 [P] [US2] `internal/workspace/init_test.go`（`TestInit_CreatesFullSkeleton` / `TestInit_TemplateContent`（MEMORY.md 两分区 + `mcp_servers.yaml` 空表可解析）/ `TestInit_Idempotent` / `TestInit_DoesNotOverwriteExisting`）
- [x] T022 [P] [US2] `internal/storage/schema_contract_test.go`（`TestSchemaFile_MatchesContract`：`internal/storage/schema.sql` 与 `docs/contracts/schema.sql` **逐字节一致**）
- [x] T023 [P] [US2] `internal/storage/migrate_test.go`（8 条：`_NewDatabaseCreatesAllTables` / `_Idempotent` / `_AppliesIncrementalWithFixtures`（合成集 1→2→3）/ `_RejectsNewerSchema` / `_NewDatabaseFailureRollsBack`（夹具先建**可观察残留**再断言两者均不存在——非空虚断言）/ `_IncrementalFailureRollsBack` / `_EmptyVersionTableRejected` / `_MissingMigrationFileRejected`）
- [x] T024 [P] [US2] `internal/storage/openwritable_test.go`（7 条：`_CreatesNewDatabase` / `_OpensExistingCurrent` / `_OldVersionOpensWithoutMigrating` / `_RejectsTooNew` / `_RejectsCorrupt` / `_PragmasOnEveryConnection` / `_JournalModeWALPersists`）
- [x] T025 [P] [US2] `internal/storage/bootstrap_test.go`（6 条：`_CreatesNewDatabase` / `_KeepsExistingCurrent` / `_RefusesExistingOutdated`（target 注入合成值）/ `_RefusesTooNew` / `_RefusesCorrupt` / `_ConcurrentInitRace`（4 goroutine：恰好一个 `created=true`、其余 Kept 无错、终态一致））

### Implementation for User Story 2

- [x] T026 [US2] `internal/workspace/workspace.go` + `templates.go`（`Init(root) (InitReport, error)` 幂等创建：6 目录 + 5 文件；已存在逐字节不覆盖；返回 `Created` / `Kept` 清单；`memory/agents/` 不预建）
- [x] T027 [US2] `internal/storage/db.go` + `pragmas.go`（`DB` 类型 + 读写池装配（写池 max 1）+ 逐物理连接 PRAGMA（DSN `?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)`）+ WAL 事务外执行一次 + `SchemaVersion` / `TableNames` / `Close`）
- [x] T028 [US2] `internal/storage/schema.sql`（契约文件的**嵌入副本**，`go:embed` 载入）+ `internal/storage/migrations/` 目录就位（本 US 无增量文件；放 `.gitkeep` 占位——空目录在 git 中不可入库）
- [x] T029 [US2] `internal/storage/db.go`：`OpenWritable(ctx, path, target) (*DB, error)`——打开 + `BEGIN IMMEDIATE` 取得写权后**重判态**并按状态矩阵分支（新库创建同事务 / 当前空事务提交 / 旧库打开不迁移 / 过高 `ErrSchemaTooNew` / 零行 `ErrSchemaCorrupt`）
- [x] T030 [US2] `internal/storage/migrate.go`：`Migrate(ctx, db, fsys, target) error`（新库全量 / 旧库逐版前向每版同事务 / 过高拒绝 / 零行拒绝 / 缺迁移文件拒绝并点名）+ `SupportedSchemaVersion = 1`
- [x] T031 [US2] `internal/storage/bootstrap.go`：`Bootstrap(ctx, path, target) (created bool, err error)`（`OpenWritable` 薄封装 + 旧库 `ErrSchemaOutdated` 拒绝；`created=true` 仅限实际创建者，竞争方写锁释放后重判为 Kept）
- [x] T032 [US2] `internal/cli/init.go`：`init` 子命令（先 `workspace.Init` 再 `storage.Bootstrap`；输出 Created / Kept 清单；失败 → ERROR 日志 + 可读文案 + 退出码 1；不取锁、不恢复、不触碰锁文件）+ 在 `root.go` 注册
- [x] T033 [US2] 端到端实测：临时目录 `basalt init` 双跑 + 文件清单逐字节比对 + 表与版本记录清点 + 并发 init 用例（读数记录）

**Checkpoint**: US2 独立可验收——一键就绪且可重复执行

---

## Phase 5: User Story 3 - 无需配置的诊断报告（Priority: P3）

**Goal**: `basalt status` 三节只读诊断（工作区 / 配置 / 存储），无配置时仍 exit 0；全部异常态点名原因并 exit 1

**Independent Test**: init 后不准备任何配置直接 `basalt status`（exit 0）；未初始化 / 配置无效 / 库三态 / 明文凭证逐项验证文案与退出码

### Tests for User Story 3

- [x] T034 [P] [US3] `internal/storage/openreadonly_test.go`（5 条：`_ReadsCurrent` / `_RejectsTooNew` / `_RejectsOutdated`（拒绝后版本不变——只校验不迁移）/ `_RejectsCorrupt` / `_MissingDatabase`（不代建））
- [x] T035 [P] [US3] `internal/cli/config_test.go`（6 条：`_ResolvesEnvPlaceholders` / `_RejectsMissingEnvVar`（点名键路径与变量名）/ `_RejectsEmptyCredential` / `_RejectsMalformedPlaceholder` / `_RejectsPlaintextCredential`（**文案不含明文值**）/ `_RejectsInvalidValues`）——失败用例同时断言 `Deps.Logger` 注入的缓冲 handler 至少收到一条 ERROR / WARN（FR-006 失败路径留痕，用既有测试基建、不引入新概念）
- [x] T036 [P] [US3] `internal/cli/contracts_test.go`（`TestContractYAMLParses`：`docs/contracts/*.yaml` 全解析通过）
- [x] T037 [P] [US3] `internal/cli/status_test.go`（5 条：`_ReadyWithoutConfig`（exit 0，节 2 报「未发现配置（路径明示）」不计失败）/ `_UninitializedWorkspace` / `_ConfigInvalidFails` / `_SchemaStateFails`（过高 / 损坏 / 旧版本逐子测试）/ `_NoCredentialLeakInOutputs`（logger 缓冲 + stdout + stderr **三路**均不含凭证值））

### Implementation for User Story 3

- [x] T038 [US3] `internal/workspace/workspace.go`：`Inspect(root) (Report, error)`（只读清点；未初始化 → `core.ErrWorkspaceNotInitialized`）
- [x] T039 [US3] `internal/storage/db.go`：`OpenReadOnly(ctx, path, target) (*DB, error)`（库缺失 → `core.ErrDatabaseMissing` 不代建；只读打开 + 状态矩阵校验；**只校验、不迁移**；不取锁、不恢复、不触碰锁文件）
- [x] T040 [US3] `internal/cli/config.go`：`LoadConfig(path, lookupEnv) (*Config, error)`（goccy/go-yaml 解析 + `${VAR}` 递归解析（注入 `lookupEnv`，缺省 `os.LookupEnv`）+ 四类 fail-closed 拒绝 + 必填 / 类型校验；错误文案不回显凭证值；**失败路径必须经 `Deps.Logger` 记 ERROR / WARN**（FR-006 / 约束 15，不吞错）
- [x] T041 [US3] `internal/cli/status.go`：`status` 子命令（三节逐节尽力报告、节内失败不阻断后续节；唯一工作区路径 = 当前工作目录 `.basalt/`；汇总全通过 exit 0 否则 exit 1；**三节任一失败必须记 ERROR / WARN 日志**——FR-006 失败路径留痕）+ 在 `root.go` 注册
- [x] T042 [US3] 端到端实测：无配置 `basalt status`（exit 0）+ 四异常态（未初始化 / 配置无效 / 库三态 / 明文凭证）逐项读数记录 + 三路输出不泄露复核

**Checkpoint**: 三个用户故事全部独立可验收

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: 收口证据与跨故事一致性

- [x] T043 [P] 覆盖率判卷调平：`make cover` 全包达标（`core` / `storage` / `workspace` 等 ≥ 阈值；入口包 `cmd/basalt` 与 `scripts/stmtcount` ≥ 50% **不豁免**；`core` 若无测试文件需补 `internal/core/core_test.go` / `errors_test.go` 用例——这是覆盖率判据（约束 10）的必要项，非清单外概念）
- [x] T044 [P] 静态与安全门禁复核：`gofumpt -w` 只对本轮改动文件；`go vet` / staticcheck（显式判定集）/ `gosec -quiet ./...` / `govulncheck ./...` 零命中；抑制注释零使用
- [x] T045 [P] 全局不变量逐条 grep 自查（`module-dev` 配置区 9 条）+ 依赖方向无环复核（`go list -deps` / `go list` 判 `core` 不 import 其它 internal 包）
- [x] T046 [P] [quickstart.md](./quickstart.md) 四场景逐条执行（无 Docker 时隔离场景如实登记「CI 未实测」，不得记通过）
- [x] T047 文档一致性收口：交付物清单 ↔ 实现逐项核对（`docs/us/us1-foundation.md` §3.1/§3.2/§3.3/§3.4）+ harness 映射表逐条对号（§4.2）+ `/speckit-analyze` 收口复核；新发现陷阱按陷阱表惯例拟条（带「怎么发现」与「怎么复核」），经用户确认后入库
- [x] T048 `make check` 终局全绿 + 七项 DoD 证据留存（门禁输出 / 映射对号 / 存在性核对 / 前序回归（无前序，明示「无」）/ 不变量自查 / 剩余人工项清单 / 变更总结三段式）

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup（Phase 1）**: 无依赖，立即开始
- **Foundational（Phase 2）**: 依赖 Setup —— **阻塞全部用户故事**
- **User Stories（Phase 3+）**: 均依赖 Foundational；按 P1 → P2 → P3 顺序推进（本项目单人 + 依赖链，不并行开工）
- **Polish（Phase 6）**: 依赖三个故事完成

### User Story Dependencies

- **US1（P1）**: Foundational 后即可开工——无跨故事依赖（门禁与产物）
- **US2（P2）**: Foundational 后即可开工——消费 US1 建立的测试与构建基建（`make test` / 覆盖率判卷），但交付物独立
- **US3（P3）**: 消费 US2 的工作区与库（`status` 的工作区 / 存储两节需要 init 产物）；`OpenReadOnly` 与 `LoadConfig` 为 US3 自有交付物

### Within Each User Story

- 测试与实现**同批落地**（harness 先行：先写用例、确认其因缺实现而失败，再补实现）
- 实体 / 类型 → 服务 / 能力 → 命令接线 → 端到端实测
- 每个故事收尾必须「该故事的测试全绿」才进下一个故事

### Parallel Opportunities

- Setup：T002 / T003 可并行
- US1：T010–T016 七个测试文件互不同文件，可并行；T017 / T018（Makefile 与 CI）互相独立
- US2：T021–T025 五个测试文件可并行；T026（workspace）与 T027–T031（storage）可并行
- US3：T034–T037 四个测试文件可并行；T038（Inspect）与 T039（OpenReadOnly）与 T040（LoadConfig）可并行
- Polish：T043–T046 可并行

---

## Parallel Example: User Story 1

```text
# 测试文件可同时写（互不同文件）：
T010 scripts/stmtcount/count.go
T013 cmd/basalt/run_test.go
T015 cmd/basalt/tzdata_test.go
T016 cmd/basalt/tzdata_isolated_test.go

# 工程文件互相独立：
T017 Makefile（七目标）
T018 .github/workflows/ci.yml（双 job + 隔离步骤）
```

---

## Implementation Strategy

### MVP First（User Story 1）

1. Phase 1 Setup → Phase 2 Foundational
2. Phase 3（US1）：门禁一条命令全绿 + 静态单二进制
3. **停下验证**：`make check` + 负向对照 + `--version`
4. 本 feature 的 MVP 到此即可演示

### Incremental Delivery

1. Setup + Foundational → 骨架就绪
2. US1 → 门禁与产物（MVP）
3. US2 → 工作区与库引导（可演示 `basalt init` 幂等）
4. US3 → 只读诊断（可演示 `basalt status` 无配置报告）
5. Polish → 覆盖率 / 不变量 / quickstart / 收口证据

---

## Notes

- **[P]** = 不同文件、无未完成依赖
- 测试与实现同批交付；**不得**删断言 / 无理由 `t.Skip` / 放宽阈值让门禁变绿
- **不自动 commit / push / tag**——每个 US 的提交由用户执行；提交信息格式见 `docs/EngineeringStandards.md` §5
- 软门禁：交付物清单外的对外概念、已定字面量修改、第三方 API 核实不到 → **停下报告**
