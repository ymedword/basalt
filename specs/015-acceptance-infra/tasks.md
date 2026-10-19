# Tasks: US-15 工程验收基础设施

**Input**: Design documents from `/specs/015-acceptance-infra/`

**Prerequisites**: [plan.md](plan.md)、[spec.md](spec.md)、[research.md](research.md)、[data-model.md](data-model.md)、[quickstart.md](quickstart.md)；事实源为已人审模块卡 `docs/us/us15-acceptance-infra.md`（sha256 `19b70290…`，1322 行）§3 交付物清单、§4 Harness、§4.5 授权范围。

**Tests**: 必须。每个实现任务先写或同批写对应测试；关键回归先取逐字失败文案再实现。首批回填记录发布前的覆盖用例红态是预期 bootstrap 状态，不得用 `t.Skip`、删测试或放宽断言规避。

## Format: `- [ ] TNNN [P?] [US?] Description with file path`

- `[P]` 只用于不同文件、无未完成依赖的任务。
- 用户故事阶段必须带 `[US1]`～`[US5]`；Setup / Foundational / Polish 不带。
- 每个任务都写出确切的仓库相对文件路径。

## Path Conventions

- 验收工具：`scripts/acceptance/`
- 进程级回填：`internal/e2e/`
- stage 与发布面：`harness/stages/`、`harness/acceptance/`
- CI 与正文：`.github/workflows/ci.yml`、`docs/DevelopmentLoop.md`

---

## Phase 1: Setup

- [x] T001 核对 `go version`、`GOPROXY=https://goproxy.cn`、分支 `015-acceptance-infra` 与工作区无基线外改动；把 `harness/acceptance.json` 与三个历史 stage 的 sha256 补记到 `specs/015-acceptance-infra/run-state.md`
- [x] T002 [P] 创建发布面骨架：`harness/acceptance/ledger.md`（固定列序表头与分隔行）、`harness/acceptance/records/README.md`（成对发布步骤、脱敏要求、原始日志保留位置）、目录 `harness/acceptance/records/reports/`

---

## Phase 2: Foundational（阻塞所有用户故事）

- [x] T003 [P] 在 `scripts/acceptance/config.go` 为 `stage` 增加 `SupersededChecks`、为 `check` 增加 `Level`，并校验层级枚举、缺省 `source`、替代目标存在且不自我替代、未知 check 引用拒绝
- [x] T004 [P] 在 `scripts/acceptance/execute.go` 把 `checkTests` 改为返回 `(passed, skipped []string, err error)`，给 `result` 增加 `level` / `passed_tests` / `skipped_tests`，给 `acceptanceReport` 增加 `run_id` / `source_fingerprint` / `base_commit` / `dirty` / `environment` / `tested_binary_sha256` / `authorization`，保持既有判卷逻辑不变
- [x] T005 在 `scripts/acceptance/record.go` 定义 `recordOptions`、`authorization`、`authorizationFlags`、`record` 及嵌套结构、`catalogItem`、`statusKey`、`statusEntry` 与 `fileSHA256`，字段逐字对应模块卡 §3.3 与 `data-model.md` E1

---

## Phase 3: User Story 1 - 每次验收运行都留下可复跑的证据（Priority: P1）

**Goal**: 每次运行写 `record.json`，记录与报告同次绑定；层级由 check 声明决定，缺标记不降级；external 与授权旗标成对；报告日志路径投影为仓库相对路径。

**Independent Test**: 跑一次 stage；`record_id` 等于运行目录名、报告哈希相符；process check 删掉全部 `binary_sha256=` 行 → 退出 2 且无记录；external 缺授权旗标 → 退出 2；仓库外 `--out` → 退出 2。

### Tests for User Story 1

- [x] T006 [P] [US1] 在 `scripts/acceptance/record_test.go` 写 `TestRecord_BuildFromReport`、`TestRecord_BuildFromSourceChanged`、`TestRecord_ValidateRequiredFields`、`TestRecord_ValidateNotVerified`、`TestRecord_OptionsDefaults`、`TestRecord_ValidateRejectsNAWithoutBasis`、`TestRecord_ValidateRejectsAbsolutePath`、`TestRecord_ValidateRejectsSecret`、`TestRecord_ValidateRedStateRequiresAppliesTo`、`TestRecord_ExternalAuthorization`，先取得逐字预期失败文案
- [x] T007 [US1] 在 `scripts/acceptance/record.go` 实现 `buildRecord` / `validateRecord` / `validateBackfillBinding` / `resolveAuthorization` / `computeRecordStatus` / `writeRecord`，落实 §3.3 校验规则 1–11 与报告路径投影后的字段绑定
- [x] T008 [P] [US1] 在 `scripts/acceptance/acceptance_test.go` 仅做 `accept` / `checkTests` 签名适配并新增 `TestAccept_ReportPathsRelative`、`TestAccept_ExternalRequiresAuthorizationFlags`，既有断言逐条保留、零删除
- [x] T009 [US1] 在 `scripts/acceptance/execute.go` 实现日志路径投影（仓库相对、`/` 分隔）、`--out` 必须位于仓库根内、process / external 唯一 `binary_sha256=` 提取与缺失 / 多值退出 2、报告与记录授权逐项相等
- [x] T010 [US1] 在 `scripts/acceptance/main.go` 增加 `--stage-only`、`--lint-stages`、`--authorization-ref`、`--authorization-scope`、`--executor`、`--run-kind`，实现 stage-only 与显式 profile 冲突退出 2、外部 check 与授权旗标成对、运行结束写 `record.json`
- [x] T011 [P] [US1] 在 `scripts/acceptance/records_test.go` 写 `TestFingerprint_IgnoresAcceptanceEvidence`（先红），断言发布记录与台账不计入源码指纹
- [x] T012 [US1] 在 `scripts/acceptance/fingerprint.go` 把 `harness/acceptance/records/` 与 `harness/acceptance/ledger.md` 加入忽略面
- [x] T013 [US1] 运行 `go test -json -count=1 ./scripts/acceptance -run '^(TestRecord_|TestAccept_ReportPathsRelative|TestAccept_ExternalRequiresAuthorizationFlags|TestFingerprint_)'`，修复至全部具名 PASS，并把红 / 绿读数记入 `specs/015-acceptance-infra/run-state.md`

---

## Phase 4: User Story 2 - 现行累计验收持续回归，历史定义零漂移（Priority: P2）

**Goal**: `current.json` 聚合 33 个 check；历史 stage 零改写；US-8 失配以 `superseded_checks` 显式映射；stage lint 只读预检，current 失配 fail-closed、历史失配仅 WARN。

**Independent Test**: 对 current 跑 lint 退出 0；三个历史 stage 哈希不变；从 current 删一个历史 check 或改历史 stage 任一字节 → 相应用例红；`-list` 指向不存在测试 → lint 点名缺失。

### Tests for User Story 2

- [x] T014 [P] [US2] 在 `scripts/acceptance/stagelint_test.go` 写 `TestStageLint_TransformListArgs`、`TestStageLint_DetectsMissingTest`，覆盖去执行参数、保留 tag 与包参数、子测试截断为顶层名、缺失测试被点名
- [x] T015 [US2] 在 `scripts/acceptance/stagelint.go` 实现 `transformListArgs`、`lintFinding`、`lintStages`、`runStageLint`，现行 stage 失配非 0、历史 stage 只 WARN
- [x] T016 [US2] 在 `scripts/acceptance/stages_test.go` 写 `TestStages_CurrentCoversHistorical`、`TestStages_CurrentMatchesHistorical`、`TestStages_HistoricalFilesPinned`、`TestStages_PinsUS15Checks`（后者在 T035 / T047 补齐四个 stage 后执行），历史钉住集合固定为三个文件、按 LF 归一哈希
- [x] T017 [US2] 新建 `harness/stages/current.json`：33 个 check（US-8 6 + `008-ports-ten` 替代 1、US-9 9、US-10 13、US-15 4）、按 US 聚合的 `tasks`、模块卡 §3.2 的 `inputs` / `protect`、按人审版本写入 `approval`
- [x] T018 [US2] 运行 `sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/current.json`：在 US3 e2e 测试落地前，`015-*` 缺失属预期红并逐条记录；历史 check 与替代项必须可预检
- [x] T019 [US2] 运行 `git diff --stat -- harness/stages/us8-notify-r1.json harness/stages/us9-web.json harness/stages/us10-scheduler.json` 确认为空，并运行 `go test -json -count=1 ./scripts/acceptance -run '^TestStages_(Current|Historical)'`

---

## Phase 5: User Story 3 - 历史人工项全量回填为进程级证据（Priority: P3）

**Goal**: `internal/e2e` 构建真实二进制，以临时工作区与回环外部服务执行 US-1~10 甲类项；回填目录与冻结全集双向相等；每条目录项有已发布 `manual_item` 记录并绑定同次报告；三视图冲突显式。

**Independent Test**: `go test -tags=e2e ./internal/e2e` 全部具名 PASS；每个目录项至少有 1 条已发布记录；删一条覆盖记录或改报告字节 → 相应用例红；同项目跨版本相反结论 → 冲突位显式。

### Tests and fixtures for User Story 3

- [x] T020 [P] [US3] 新建 `internal/e2e/doc.go` 与 `internal/e2e/harness_test.go`：`TestMain` 构建 `CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=us15-e2e"` 的真实二进制，helper / `-list` 短路不构建；实现 `repoRoot` / `buildBinary` / `newHarness` / `run` / `start` / `writeConfig` / `writeAgent` / `waitHTTP` / `signal` / `kill` / `waitExit` / `openDB` / `queryRows`，打印 `binary_sha256=`
- [x] T021 [P] [US3] 新建 `internal/e2e/fixtures_test.go`：假模型脚本（文本 / `tool_calls` / `reasoning_content` / `usage` / 400 / 脚本用尽）、回环接收器、MCP helper 启动与 `TestMCPHelperProcess` 子进程入口；复用 `internal/testutil`，不重写夹具
- [x] T022 [P] [US3] 新建 `internal/e2e/bootstrap_test.go`：`TestBackfill_Bootstrap` 覆盖 US-1 / US-2 / US-3 的装配、凭证、init 幂等、坏配置、不可达端点审计
- [x] T023 [P] [US3] 新建 `internal/e2e/agentloop_test.go`：`TestBackfill_AgentLoop` 覆盖 US-3 / US-4 / US-5 的多轮、读文件、越界拒绝、推理回传、注入拒绝、沙箱四档与脱敏
- [x] T024 [P] [US3] 新建 `internal/e2e/toolsandbox_test.go`：`TestBackfill_ToolSandbox` 覆盖 US-4 / US-5 的进程树、超时清理、cwd 与 stderr
- [x] T025 [P] [US3] 新建 `internal/e2e/memory_test.go`：`TestBackfill_Memory` 覆盖 US-6 的两档、编辑时机、配额、并发与损坏文件
- [x] T026 [P] [US3] 新建 `internal/e2e/mcp_test.go`：`TestBackfill_MCP` 覆盖 US-7 的坏 command、两档展示、env 负向与停机残留
- [x] T027 [P] [US3] 新建 `internal/e2e/notify_test.go`：`TestBackfill_Notify` 覆盖 US-8 的渠道登记、域名拒绝、环境负向、生效时机与哨兵脱敏，只走拒绝路径与登记面
- [x] T028 [P] [US3] 新建 `internal/e2e/web_test.go`：`TestBackfill_Web` 覆盖 US-9 的 HTTP 面与进程模式、409 文案、invoke 建会话、跨源、预算与归档后写入
- [x] T029 [P] [US3] 新建 `internal/e2e/lock_test.go`：`TestBackfill_CrossProcessLock` 覆盖 US-1 / US-3 / US-9 的实例锁与重命令互斥
- [x] T030 [P] [US3] 新建 `internal/e2e/recovery_test.go`：`TestBackfill_RestartRecovery` 覆盖 US-1 / US-4 / US-9 / US-10 的崩溃恢复、重启与 missed fire 不补跑
- [x] T031 [P] [US3] 新建 `internal/e2e/scheduler_test.go`：`TestBackfill_Scheduler` 覆盖 US-10 的 cron 触发、归属守卫、`CRON_TZ` 与互斥
- [x] T032 [P] [US3] 新建 `internal/e2e/console_test.go`：`TestBackfill_ConsolePipe`（单顶层测试、不拆子测试）覆盖管道 UTF-8 往返
- [x] T033 [P] [US3] 新建 `internal/e2e/signal_linux_test.go`（`//go:build e2e && linux`）：`TestBackfill_SignalLinux` 覆盖 SIGTERM 五阶段、gateway 停机与预算，子测试名 `grace_exhausted`、`sigterm_order`
- [x] T034 [P] [US3] 新建 `internal/e2e/slow_test.go`（`//go:build e2e && slow`）：`TestBackfill_SlowTimeout/run_timeout` 覆盖 US-10 的 300s 运行超时
- [x] T035 [US3] 新建 `harness/stages/us15-linux.json` 与 `harness/stages/us15-slow.json`：各 1 个 `level = process` check（`015-signal-linux` / `015-slow-timeout`），`tasks = us15`，`inputs` / `protect` / `approval` 按模块卡 §3.2
- [x] T036 [US3] 在 `scripts/acceptance/stages_test.go` 增加 `backfillCatalog` 与 `requiredBackfillItems`，逐项转写模块卡 §2.6（item / check / test / level），并写 `TestStages_BackfillCatalogMatchesRequiredTests`、`TestStages_BackfillCatalogCoversRequiredSet`（双向相等，含“同时删目录项与 required_test 仍红”反例）
- [x] T037 [US3] 在 `scripts/acceptance/records_test.go` 写 `TestRecords_BackfillCoversCatalog`、`TestRecords_BackfillBindsToExecution`、`TestRecords_StatusDoesNotMaskConflicts`、`TestRecords_PublishedConform`、`TestRecords_NoSecretsOrAbsolutePaths`、`TestLedger_IndexesAllRecords`，覆盖四类绑定反例与四类遮蔽反例
- [x] T038 [US3] 运行 `go test -tags=e2e -count=1 ./internal/e2e`、`go vet -tags=e2e,slow,real ./internal/e2e` 与已落地 check 的 `go test -list` 预检，修复至 e2e 具名 PASS、vet 退出 0；`015-invariants` 的测试文件到 T051 才存在，完整 stage lint 在 T053 收口；记录各命令读数
- [x] T039 [US3] 执行一次回填运行：`sh scripts/check-acceptance.sh --stage harness/stages/current.json --stage-only --run-kind backfill --executor ai --out .loop/runs/<新目录>`；该轮因覆盖用例尚未发布、`015-invariants` 尚未实现可能整体退出 1，属预期 bootstrap；必须确认 `015-backfill` check 为 passed 并保留其 `passed_tests` 与 `report.json`，不得复用已有目录
- [x] T040 [US3] 确定 Linux 执行通道（本机 WSL 优先；不可用则停下报告，不得伪造）：运行 `harness/stages/us15-linux.json` 并保留同次 `report.json`；Windows 平台限制项按四态记未验证并写明原因
- [x] T041 [US3] 收口时执行慢速项：`sh scripts/check-acceptance.sh --stage harness/stages/us15-slow.json --stage-only --out .loop/runs/<新目录>`，保留同次 `report.json`
- [x] T042 [US3] 用一次性生成器（仓库外脚本；包内绑定校验用临时 `_test.go` 探针调用 `validateBackfillBinding`，用毕即删）从 T039–T041 的真实报告中**只读取承载该目录项的 check 段**（T039 取 `015-backfill`，T040 取 `015-signal-linux`，T041 取 `015-slow-timeout`）的 `status` / `passed_tests` 与 `backfillCatalog` 程序化生成每条 `manual_item` 记录，整体 `run_status` 不参与判断；值取自报告、不手填结论；先过执行绑定校验，再成对复制到 `harness/acceptance/records/` 与 `harness/acceptance/records/reports/`，同名即停、不得覆盖
- [x] T043 [US3] 在 `harness/acceptance/ledger.md` 为每条已发布记录追加一行（固定列序、链接存在），复跑 `go test -json -count=1 ./scripts/acceptance -run '^(TestRecords_|TestLedger_|TestStages_Backfill)'` 至全绿，并把记录 ID 与台账行数记入 `specs/015-acceptance-infra/run-state.md`

---

## Phase 6: User Story 4 - 已授权真实项的执行与留证（Priority: P4）

**Goal**: 真实模型与真实飞书外发在用户授权后执行；缺环境即 FAIL 不 SKIP；授权字段与报告逐项相等；调用 / 发送封顶；输出只出现 `scheme://host`。

**Independent Test**: 不带授权旗标运行 `us15-real` → 退出 2；带旗标但缺环境变量 → 两个 check FAIL（非 SKIP）；环境齐备 → 两 check PASS，记录与报告授权相等，次数不超限。

- [x] T044 [P] [US4] 新建 `internal/e2e/real_test.go`（`//go:build e2e && real`）：`TestBackfill_Real` 的 11 个子测试（`provider_call` / `multiturn` / `chat_tool` / `weather_tool` / `memory_save` / `mcp_tool` / `scheduler_fire` / `scheduler_manage_self` / `scheduler_manage_other_denied` / `restart_session` / `web_invoke_tool`）与 `TestBackfill_RealFeishu`（`send` / `redaction`）；四个 `BASALT_REAL_*` 缺失即 `t.Fatalf`，原子计数封顶 30 次模型调用与 5 次外发，不复用 `ANTHROPIC_*`
- [x] T045 [US4] 在 `scripts/acceptance/stages_test.go` 增加 `requiredRealItems`（逐项转写模块卡 §4.5 覆盖表）与 `TestStages_RealCatalogCoversAuthorizedSet`（外部目录与冻结全集双向相等，含同时删项反例）
- [x] T046 [US4] 在 `scripts/acceptance/records_test.go` 写 `TestRecords_RealBindsToExecution`，断言 external 记录与同次报告的 `authorization` 逐项相等、二进制哈希与报告一致
- [x] T047 [US4] 新建 `harness/stages/us15-real.json`：`015-real-model`（900s）/ `015-real-feishu`（180s），均 `level = external`，`tasks = us15`，`inputs` / `protect` / `approval` 按模块卡 §3.2；stage 内不得出现凭证或授权值；重跑 `TestStages_PinsUS15Checks` 至绿
- [x] T048 [US4] 收口时、用户授权后：在执行 shell 中核对 `BASALT_REAL_LLM_BASE_URL` / `BASALT_REAL_LLM_MODEL` / `BASALT_REAL_LLM_API_KEY` / `BASALT_REAL_FEISHU_URL` 均已注入且不打印值；缺失则停在执行前并按未验证登记，不得以空值运行
- [x] T049 [US4] 收口时、用户授权后：以 `--authorization-ref user-2026-10-19-real-llm-webhook` 与模块卡 §4.5 的固定 scope 运行 `harness/stages/us15-real.json`，新目录 `--out .loop/runs/<时间戳>-real`；为两个 external check 生成 `manual_item` 记录并成对发布、追加 `harness/acceptance/ledger.md`
- [x] T050 [US4] 核验真实项证据：记录与报告 `authorization` 逐项相等、模型调用 ≤30、飞书发送 ≤5、输出 / 记录 / 报告只出现 `scheme://host`；失败如实记 `failed`，不降级为未验证或跳过；把计数与缺失变量名记入 `specs/015-acceptance-infra/run-state.md`

---

## Phase 7: User Story 5 - CI 接线与仓库不变量（Priority: P5）

**Goal**: 既有 `test` job 内插入 lint-stages、current、Linux 三步；所有运行目录含 run id + attempt；六个仓库不变量机器判卷。

**Independent Test**: 工作流文本含三步且所有 `--out` 带 `${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}`；把任一 `--out` 改回固定名 → `TestRepoInvariants_CIRunDirsUnique` 红；在 `go.mod` 加入被禁坐标或 ORM → 相应用例红。

- [x] T051 [P] [US5] 新建 `scripts/acceptance/invariants_test.go`：`TestRepoInvariants_NoForbiddenAgentFramework`、`TestRepoInvariants_NoProcessLocalTimezone`、`TestRepoInvariants_NoPlaintextCredential`、`TestRepoInvariants_NoORM`、`TestRepoInvariants_TestOnlyDependencyNotLinked`、`TestRepoInvariants_CIRunDirsUnique`，扫描面与凭证模式按模块卡 §4.2
- [x] T052 [US5] 修改 `.github/workflows/ci.yml`：在 Unified acceptance 与 Upload acceptance evidence 之间插入 lint-stages、current、`us15-linux` 三步；既有 `ci` 与全部新 `--out` / 控制台日志改为 `*-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}`；不新增 job、不改 job 名、不接入 `us15-real`
- [x] T053 [US5] 运行 `go test -json -count=1 ./scripts/acceptance -run '^TestRepoInvariants_'` 至全绿；运行完整 `sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/current.json` 至退出 0（四个 `015-*` 测试文件此时齐全）；对 `.github/workflows/ci.yml` 逐行核对三步与目录后缀；真实 CI 运行结论待用户推送后在 review ② 证据中登记

---

## Phase 8: Polish 与收口

- [x] T054 [P] 更新 `harness/README.md`：新旗标、current / 历史策略、记录字段表、成对发布、路径投影、指纹排除，修正第 30 行“仓库不预置具体阶段文件”的过时表述
- [x] T055 [P] 更新 `harness/project.md`：补充 current 清单、`--stage-only`、`record.json` + 同次报告、台账与绑定校验、外部 stage 入口与凭证只走环境变量
- [x] T056 修改 `docs/DevelopmentLoop.md`：§5.1 第 6 项整项替换为模块卡 §2.4 注 C 最终文本；§9.3 增加 current 清单、lint、记录台账引用；§6.2 第 3 项 / §6.3 凭证纪律原文不动
- [x] T057 运行 `make check`（fmt / lint / sec / test / test-integration / cover / build）至七目标全绿；核对 `git diff --exit-code -- go.mod go.sum internal/storage/schema.sql docs/contracts/schema.sql Makefile harness/acceptance.json` 为空（零新增坐标 / 迁移 / 配置 / 门禁 profile 变更）；逐目标四态与读数写入 `specs/015-acceptance-infra/run-state.md`
- [x] T058 按 `specs/015-acceptance-infra/quickstart.md` 场景 1–10 逐条复跑（真实项已在 T049 执行；缺失环境项记未验证），把每条命令的退出码与关键输出写入收口证据
- [x] T059 更新 `specs/015-acceptance-infra/run-state.md`：tasks 正文指纹（勾选位归一）、33 个现行 check + 4 个独立 stage check 到任务 ID 的映射、未验证清单（缺环境 / 未授权 / 平台限制逐条原因）、停止预算计数
- [x] T060 执行独立 AI 复核（独立上下文优先）：输入冻结规格 + 判据面 diff，不读作者叙述；输出 findings 表与未覆盖面清单，逐条处置后把结论写入 `specs/015-acceptance-infra/run-state.md`
- [x] T061 组装人工 review ② 收口包（七项证据 + 四段变更总结 + 判据面变更逐类 + 记录 / 台账索引），写入 `specs/015-acceptance-infra/run-state.md` 并在对话中报告；等待用户实际决定，不预写通过

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup（T001–T002）**：无依赖，先完成。
- **Foundational（T003–T005）**：依赖 Setup；阻塞全部用户故事。
- **US1（T006–T013）**：依赖 Foundational。T006 → T007；T008 → T009 → T010；T011 → T012；T013 在全部之后。
- **US2（T014–T019）**：T014 → T015；T016 / T017 可在 T015 后并行编写，T018 / T019 依赖两者；`TestStages_PinsUS15Checks` 等 T035 / T047 补齐四个 stage 后才能绿。
- **US3（T020–T043）**：依赖 US1（记录与 CLI）与 T017（current stage）。T020 / T021 先落地，T022–T034 才能并行；T035 → T036 → T037 → T038；T039–T043 严格按序（回填运行 → Linux / 慢速 → 生成发布 → 台账与复验）。
- **US4（T044–T050）**：依赖 US3 的 harness 与 fixtures（T020 / T021）；T044 可与 T045–T047 并行编写；T045 / T046 依赖 T036 / T037 的类型与目录；T048–T050 严格按序，且只在收口、用户授权后执行。
- **US5（T051–T053）**：T051 可与 US3 / US4 并行；T052 依赖 T017 / T035；T053 依赖二者。
- **Polish（T054–T061）**：依赖全部前置；T060 必须在 T057 / T058 之后、T061 之前。

### Story Completion Order

```text
Setup → Foundational → US1（记录） → US2（现行清单） → US3（回填） → US4（真实项）
                                                   ↘ US5（CI 与不变量） ↗
US3 / US4 / US5 → Polish（文档、门禁、独立复核、review ② 收口包）
```

### Key Dependencies

- `current.json` 的 `015-*` check 在 `internal/e2e` 测试文件落地前会让 lint 预期红（T018）；T038 覆盖 e2e 与 vet 的具名测试，`015-invariants` 到 T051 才有测试文件，完整 stage lint 到 T053 转绿。
- `TestRecords_BackfillCoversCatalog` 在首批记录发布前预期红（T037）；T042 发布后 process 侧 114 项转绿，外部 15 项须待 T049 发布 external 记录后才转绿——T043 的「至全绿」实际由 T049 完成。
- `TestStages_PinsUS15Checks` 依赖四个 stage 文件齐全（T017 / T035 / T047）。
- `us15-linux.json` 的 Windows 执行受平台限制；T040 必须先确定 WSL / CI 通道，拿不到通道即停下报告，不得伪造记录。
- 真实项只在 T048 / T049 收口执行；授权 `ref` 只对本次运行有效。

---

## Parallel Example: User Story 3

```sh
# T020 / T021 落地后，以下场景文件可在不同文件上并行编写：
T022 internal/e2e/bootstrap_test.go
T023 internal/e2e/agentloop_test.go
T024 internal/e2e/toolsandbox_test.go
T025 internal/e2e/memory_test.go
T026 internal/e2e/mcp_test.go
T027 internal/e2e/notify_test.go
T028 internal/e2e/web_test.go
T029 internal/e2e/lock_test.go
T030 internal/e2e/recovery_test.go
T031 internal/e2e/scheduler_test.go
T032 internal/e2e/console_test.go
T033 internal/e2e/signal_linux_test.go
T034 internal/e2e/slow_test.go
```

同一包内的测试文件共享 `harness_test.go` / `fixtures_test.go` 的 helper；并行编写时只追加本文件内容，不改他人文件。

---

## Implementation Strategy

### MVP First

可独立演示的最小增量是 **US1 + US2**：单次运行有记录、报告路径可投影、current 清单可 lint。但本 US 的完成定义要求 US3 回填、US4 授权真实项（可执行时）与 US5 CI，不得以 MVP 替代收口。

### Bootstrap Order（模块卡 §4.3）

1. 实现记录工具与 stage lint（US1 / US2）。
2. 实现 `internal/e2e` 与回填目录（US3 T020–T038）。
3. 执行回填、Linux / 慢速运行并发布 `records/` 与台账（T039–T043）。
4. 发布真实项记录（T048–T050，收口、用户授权后）。
5. 最后跑 `make check` 与独立复核（T057–T061）。

首批记录发布前覆盖用例红是预期状态；不得用跳过、删测试或放宽断言规避。

---

## 任务 ↔ 交付物比对（AI 自检）

### scripts/acceptance

| 交付物 | 动作 | 任务 |
| --- | --- | --- |
| `scripts/acceptance/main.go` | 修改 | T010 |
| `scripts/acceptance/execute.go` | 修改 | T004、T009 |
| `scripts/acceptance/config.go` | 修改 | T003 |
| `scripts/acceptance/fingerprint.go` | 修改 | T012 |
| `scripts/acceptance/acceptance_test.go` | 仅签名适配 + 2 用例 | T008 |
| `scripts/acceptance/record.go` | 新增 | T005、T007 |
| `scripts/acceptance/stagelint.go` | 新增 | T015 |
| `scripts/acceptance/record_test.go` | 新增 | T006 |
| `scripts/acceptance/stagelint_test.go` | 新增 | T014 |
| `scripts/acceptance/stages_test.go` | 新增 | T016、T036、T045 |
| `scripts/acceptance/records_test.go` | 新增 | T011、T037、T046 |
| `scripts/acceptance/invariants_test.go` | 新增 | T051 |

### internal/e2e（模块卡 §3.1.1 表共 17 行）

| 文件 | 任务 |
| --- | --- |
| `internal/e2e/doc.go` | T020 |
| `internal/e2e/harness_test.go` | T020 |
| `internal/e2e/fixtures_test.go` | T021 |
| `internal/e2e/bootstrap_test.go` | T022 |
| `internal/e2e/agentloop_test.go` | T023 |
| `internal/e2e/toolsandbox_test.go` | T024 |
| `internal/e2e/memory_test.go` | T025 |
| `internal/e2e/mcp_test.go` | T026 |
| `internal/e2e/notify_test.go` | T027 |
| `internal/e2e/web_test.go` | T028 |
| `internal/e2e/lock_test.go` | T029 |
| `internal/e2e/recovery_test.go` | T030 |
| `internal/e2e/scheduler_test.go` | T031 |
| `internal/e2e/console_test.go` | T032 |
| `internal/e2e/signal_linux_test.go` | T033 |
| `internal/e2e/slow_test.go` | T034 |
| `internal/e2e/real_test.go` | T044 |

**计数差异登记**：模块卡 §3.1.1 表实际列出 17 个文件（`doc.go` + 16 个 `_test.go`）；输入摘要的“16 个文件（`doc.go` + 15 个 `_test.go`）”少计 1 个测试文件（plan 的同项已在 analyze 阶段按本表修正为 17）。任务按表逐行覆盖 17 个文件，不新增表外文件、不遗漏清单中的任何文件。

### Harness 与 CI

| 交付物 | 任务 |
| --- | --- |
| `harness/stages/current.json` | T017 |
| `harness/stages/us15-linux.json` | T035 |
| `harness/stages/us15-slow.json` | T035 |
| `harness/stages/us15-real.json` | T047 |
| `harness/acceptance/ledger.md` | T002、T043、T049 |
| `harness/acceptance/records/README.md` | T002 |
| `harness/acceptance/records/reports/` | T002、T042、T049 |
| `harness/README.md` | T054 |
| `harness/project.md` | T055 |
| `.github/workflows/ci.yml` | T052 |
| `docs/DevelopmentLoop.md` | T056 |

### 33 个现行 check 的可追溯任务

| 来源 | check 数 | 任务 |
| --- | --- | --- |
| US-8 历史 6 + 替代 `008-ports-ten` | 7 | T016、T017 |
| US-9 历史 | 9 | T016、T017、T019 |
| US-10 历史 | 13 | T016、T017、T019 |
| `015-acceptance-infra` | 1 | T006–T013、T014–T016、T036–T037、T045–T046 |
| `015-invariants` | 1 | T051、T053 |
| `015-backfill` | 1 | T020–T043 |
| `015-e2e-vet` | 1 | T038、T053 |

### 4 个独立 stage check

| check | stage | 任务 |
| --- | --- | --- |
| `015-signal-linux` | `us15-linux.json` | T033、T035、T040 |
| `015-slow-timeout` | `us15-slow.json` | T034、T035、T041 |
| `015-real-model` | `us15-real.json` | T044、T047、T048–T050 |
| `015-real-feishu` | `us15-real.json` | T044、T047、T048–T050 |

### 关键回归与逆改点落点

| 关键回归 | 任务 | 逆改点（实现怎么改它会红） |
| --- | --- | --- |
| `TestRecord_BuildFromReport` | T006 / T007 | 删掉 base commit / fingerprint / inputs；process check 缺 `binary_sha256=` 仍放行 |
| `TestRecord_BuildFromSourceChanged` | T006 / T007 | 把 `source_changed` 映射成 `passed` |
| `TestRecord_ExternalAuthorization` | T006 / T007 | external 缺授权或记录与报告授权不等仍通过 |
| `TestAccept_ReportPathsRelative` | T008 / T009 | 报告保留绝对路径 / 反斜杠；仓库外 `--out` 未被拒绝 |
| `TestAccept_ExternalRequiresAuthorizationFlags` | T008 / T010 | 缺旗标或多给旗标未退出 2 |
| `TestFingerprint_IgnoresAcceptanceEvidence` | T011 / T012 | 把 `records/` 或 `ledger.md` 计入指纹 |
| `TestStageLint_TransformListArgs` | T014 / T015 | 保留 `-run`、丢掉 `-tags` 或包参数、子测试未截断 |
| `TestStageLint_DetectsMissingTest` | T014 / T015 | lint 忽略缺失的 required_test |
| `TestStages_CurrentCoversHistorical` | T016 / T017 | 从 current 删历史 check 或删替代映射 |
| `TestStages_CurrentMatchesHistorical` | T016 / T017 | 改非替代项定义或替代目标不精确 |
| `TestStages_HistoricalFilesPinned` | T016 / T019 | 改写三个历史 stage 任一字节 |
| `TestStages_BackfillCatalogMatchesRequiredTests` | T036 | 删 stage required_test、把 linux / slow 项塞进 `015-backfill`、写重号或非法 item ID |
| `TestStages_BackfillCatalogCoversRequiredSet` | T036 | 同时删目录项与其 required_test，或只删冻结全集一项 |
| `TestStages_RealCatalogCoversAuthorizedSet` | T045 | 外部目录与冻结全集任一侧漏项 |
| `TestStages_PinsUS15Checks` | T016 / T047 | 改 `015-*` 或 `008-ports-ten` 的 args / timeout / level，删 tag 或降级 external |
| `TestRecords_BackfillCoversCatalog` | T037 / T043 | 删一条 `subject.items` 或把唯一通过记录改成未验证 |
| `TestRecords_BackfillBindsToExecution` | T037 / T042 | 记录写 passed 但报告无该测试 PASS、引用别的 check 或运行、报告字节被改 |
| `TestRecords_RealBindsToExecution` | T046 / T049 | 记录与报告授权不等、二进制哈希不一致 |
| `TestRecords_StatusDoesNotMaskConflicts` | T037 | 跨平台较新通过盖住旧失败、按 `run_kind` 过滤用户复跑、同键冲突静默取最新 |
| `TestRecords_PublishedConform` | T037 / T042 | 发布缺字段或枚举非法的记录 |
| `TestRecords_NoSecretsOrAbsolutePaths` | T037 / T042 | 记录或报告出现 `sk-…` / 盘符路径 |
| `TestLedger_IndexesAllRecords` | T037 / T043 | 删台账行或留下悬空链接 |
| `TestRepoInvariants_*` | T051 | `go.mod` 加 eino / gorm、生产代码加 `time.Local`、发布面写凭证、固定 CI 目录 |
| `TestBackfill_Bootstrap` | T022 | `init` 非幂等、坏配置不拒绝、不可达端点不落审计 |
| `TestBackfill_AgentLoop` | T023 | 越界不拒、审计不落 failed、推理不回传、注入当普通对话 |
| `TestBackfill_ToolSandbox` | T024 | 超时后子进程存活、cwd 不生效、stderr 丢失 |
| `TestBackfill_Memory` | T025 | 改文件当轮生效、sqlite 档改写 `MEMORY.md`、超配额放行 |
| `TestBackfill_MCP` | T026 | 坏 command 拖垮整池、停机残留子进程、明文 env 放行 |
| `TestBackfill_Notify` | T027 | 域名拒绝前已发送、缺 env 仍发送、错误回显完整 URL |
| `TestBackfill_Web` | T028 | 409 文案变化、invoke 建会话、跨源写放行、归档后仍可写 |
| `TestBackfill_CrossProcessLock` | T029 | 第二进程不因实例锁失败 |
| `TestBackfill_RestartRecovery` | T030 | 半截工具组不补齐、重启不恢复、missed fire 被补跑 |
| `TestBackfill_Scheduler` | T031 | cron 不触发、归属守卫失效、`CRON_TZ` 被忽略 |
| `TestBackfill_ConsolePipe` | T032 | 管道输出被按 ANSI 代码页截断 |
| `TestBackfill_SignalLinux` | T033 | 信号不触发优雅停机或事件序错乱 |
| `TestBackfill_SlowTimeout` | T034 | 超时不终止、进程挂死 |
| `TestBackfill_Real` / `TestBackfill_RealFeishu` | T044 | 缺环境却记通过、调用超限、真实响应落盘、子测试 SKIP |

### 判据面变更核对

任务只覆盖模块卡 §3.4 列出的变更：

| §3.4 条目 | 任务 |
| --- | --- |
| `accept` / `checkTests` / `result` / 报告字段与路径投影 | T004、T008、T009 |
| `fingerprint.go` 排除记录与台账 | T012 |
| `config.go` 的 `superseded_checks` / `level` | T003 |
| `record.go` / `stagelint.go` / `main.go` 与 CLI 旗标 | T005–T010、T015 |
| `external` 层级与授权旗标 | T006、T007、T010 |
| 回填目录 / 记录绑定 / 三视图测试 | T036、T037、T045、T046 |
| `internal/e2e/*` 与 build tag | T020–T034、T044 |
| `current.json` 的四个 `015-*` check | T017 |
| `us15-linux` / `us15-slow` / `us15-real` stage | T035、T047 |
| CI 三步与目录唯一化 | T052 |
| `harness/README.md` / `project.md` / `DevelopmentLoop.md` | T054–T056 |

`scripts/acceptance/acceptance_test.go` 仅签名适配 + 两个新增用例；既有断言零删除、零跳过、零放宽。`harness/acceptance.json`、`Makefile`、`go.mod` / `go.sum`、`schema.sql`、历史 stage 与历史 US 文档均不作为修改项。

---

## Notes

- 命令统一从仓库根运行；Windows 用 `.\scripts\check-acceptance.ps1`，Git Bash / Linux 用 `sh scripts/check-acceptance.sh`。
- 所有 `--out` 必须是尚不存在的新目录；发布记录同名即停，不得覆盖。
- 真实凭证只经运行时环境变量注入，值不落盘、不进对话、不进记录、不进报告、不进日志。
- 未经用户发话不得 commit / push / tag；本任务清单不授权提交。
