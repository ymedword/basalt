# US24 验收报告

对应模块卡：`docs/us/us24-state-distributed.md` R7。review ① 溯源为 3385 行 `sha256=02e9f9418b6bca0616ac01c0b941cc4f23864301317396d25ba0fa3fbe934395`；review ② 按用户裁决补记勘误（§7.4.5，把「保持 blocked」的读法对齐 FR-24-37 / C-13；§7.4.6，把 `Joined` 的作用域对齐 review ② 第 7 项的任务子树实现）后现值 3411 行 `sha256=c6a2658e64e5ab9b381d7bd609a20799891b668100d854e86cde9f451bd70cf6`（勘误一后中间值 3397 行 / `c5b2413a…`），修订记录与复算命令见 run-state §1.4。基线提交 `138382f`，分支 `024-state-distributed`。

> **2026-11-08 状态**：§1.1–§1.3 的 2026-11-07 读数（指纹 `845002664a36…`）对应 review ② 打回后的**修复前**源码树，只作历史保留，不作送审依据。送审依据是 §1.4 / §8.5 的最终复跑：源码冻结后重跑 `make check`、base stage、fault stage 与定向 e2e，两轮 stage 报告指纹同为 `75ca47f3…`。

四态口径：每个验收点标 **通过 / 失败 / 跳过 / 未验证**；必验项处于跳过或未验证时，该项验收保持未完成。

## 1. 门禁与验收运行读数

### 1.1 `make check` 七目标（宪法口径）

> 2026-11-07 轮次读数，**已被 §1.4 取代**，保留为历史记录（对应修复前源码树）。

`dev-env/scripts/with-postgres.sh make check`（WSL，用户 `ym`，PostgreSQL 17.11，`BASALT_TEST_PG_DSN` 由 `with-postgres.sh` 注入），2026-11-07 20:10:53–20:49，末行 **`RC=0`**。过程日志 `/tmp/us24-make-check-3.log` 属临时物，收口时已删；读数逐项抄录于本表。

| 目标 | 四态 | 读数 |
| --- | --- | --- |
| `fmt` | **通过** | `check-format: gofmt / gofumpt 均干净`（`sh scripts/check-format.sh`，退出 0） |
| `lint` | **通过** | `go vet -tags=integration ./...` + `staticcheck -tags=integration -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...`，均零诊断、退出 0（判定集显式指定；**不带 tag 时只判半个代码库**，见 Makefile 注释） |
| `sec` | **通过** | `gosec -quiet ./...` 零告警；`govulncheck ./...`：`No vulnerabilities found` / `Your code is affected by 0 vulnerabilities`。**如实登记**：依赖模块层仍有 2 条未被本仓代码调用的已知漏洞（`This scan also found 0 vulnerabilities in packages you import and 2 vulnerabilities in modules you require`），`govulncheck` 判定不可达，属既有依赖基线，本 US 零新增坐标故未变化 |
| `test` | **通过** | `go test -race -cover ./...`：**40 个包 ok、2 个 `no test files`、0 FAIL**；`internal/app` 46.6s（untagged 覆盖率 21.8%，集成层在下一个目标） |
| `test-integration` | **通过** | `go test -tags=integration -count=1 -timeout=20m ./...`：**42 个包 ok、2 个 `no test files`、0 FAIL**；长尾 `internal/app` 366.4s、`internal/storage` 等全部 ok |
| `cover` | **通过** | `sh scripts/check-coverage.sh`（自身带 `-tags=integration` 重跑）：`check-coverage: 全部包达标`，逐包读数见 §1.3 |
| `build` | **通过** | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=…" -o bin/basalt ./cmd/basalt`，退出 0；产物 `bin/basalt` 41,136,288 字节（`make check` 构建于 20:46；其后 base stage 的 `build` check 于 22:09 重建同一路径，字节数不变） |

`-race` 与 `CGO_ENABLED=0` **分属不同目标**（`test` / `cover` 开 CGO，`build` 关 CGO），未混用。本次门禁共跑了三轮才全绿：首跑 `fmt` 红（未格式化测试文件）、二跑 `lint` 红（ST1005）、三跑 `sec` 红（gosec G202）；三次修复都改代码、未加抑制注释、未放宽断言，逐条记在 [run-state §2](run-state.md)。

### 1.2 Harness stage（公共 profile + 本 US 两份 stage）

> 2026-11-07 轮次读数，**已被 §1.4 取代**，保留为历史记录（对应修复前源码树）。

**全量 base**：`dev-env/scripts/with-postgres.sh sh scripts/check-acceptance.sh --stage harness/stages/us24-base.json`，2026-11-07 21:33:45–22:14:52（CST），日志 `/tmp/us24-accept-base3.log`，报告 `.loop/runs/accept-20261107T133336.958949812/report.json`，**`status=passed`、`exit_code=0`**、`fingerprint=845002664a36…`、`base_commit=138382f`。公共 profile 11 项（`fmt` / `lint` / `sec` / `test` / `test-integration` / `test-pg` / `cover` / `build` / `eval-build` / `behavior` / `injection`）全部 `passed`、exit 0，其中 `behavior` 8 例 7 PASS + 1 允许 SKIP（`outcome-real-model-answer`，用例声明真模型档、本次未给 `--provider`），`injection` 4 PASS。

| stage check | 四态 | 具名测试 | 读数 |
| --- | --- | --- | --- |
| `us24-base-core` | **通过** | 112 | `TestLease_*` / `TestKeepLease_*` / `TestRun_RenewFailureCancelsBeforeNextAction` / `TestScheduler_*` 全部具名 PASS，`required_missing=0` |
| `us24-base-assets` | **通过** | 10 | `TestAssets_*` 四例（含 round-trip、截断不破坏原目录、旧 Run 钉旧 hash、路径三处一致）PASS |
| `us24-base-pg` | **通过** | 141 | `us24-base.json` 的 38 个 `required_tests` 全出现（含子例），`required_missing=0` |
| `us24-base-app` | **通过** | 236 | 44 个 `required_tests` 全出现（含子例），`required_missing=0` |
| `us24-base-callers` | **通过** | 5 | `TestLimiter_QPSRemainsPerWorkerSoftLimit` 在列 |
| `us24-base-cli` | **通过** | 4 | `TestCLI_AssetsExportInstallVerifyRoundTrip` / `TestCLI_WorkersListReadOnly` 在列 |
| `us24-base-web` | **通过** | 2 | `TestWeb_FlowRootBusyIs409` / `TestWeb_RunViewLeaseFields` |
| `us24-base-console-test` | **通过** | — | `npm --prefix assets/console test` exit 0 |
| `us24-base-console-build` | **通过** | — | `npm --prefix assets/console run build` exit 0 |
| `us24-base-race` | **通过** | 6 | `-race -tags=integration` 单列命令：`TestShutdown_*` 6 例全部顶层 PASS、无子例；stage 的 5 个 `required_tests` 全在其中，`required_missing=0` |

**故障 stage**：`sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us24-fault.json`，2026-11-07 21:32:56–21:33:24，日志 `/tmp/us24-accept-fault1.log`，报告 `.loop/runs/accept-20261107T133246.346462913/report.json`，**`status=passed`、`exit_code=0`**、`fingerprint` 同上、`scope=stage-only/us24-fault`。

| stage check | 四态 | 具名测试 | 读数 |
| --- | --- | --- | --- |
| `us24-fault-pg` | **通过** | 20 | 13 个 `required_tests` 全出现（含 `WindowGoneControlledClose` 5 子例、`Admission_BlocksUntilRecoveryCommits` 2 子例），`-race`，`report` 逐条 `pass` 且有真实耗时 |
| `us24-fault-app` | **通过** | 13 | 9 个 `required_tests` 全出现（`TestDistributed_*` 6 例 + `OrphanExtractionWaitsForConcurrentRenewal` 2 子例 + `Archive_AdmissionLockOrder` 2 子例 + `NormalHandoffRefusesUnresolvedAction`），`-race` |

**作废轮次（如实登记）**：同日 20:50–21:32 的一次同参数全量 base 跑，21 项 check 全部 `passed`，但终判 `source_changed`（exit 2）——跑动期间编辑了本文件，工作树指纹前后不一致。按门禁纪律该轮不作证据、不计通过，改用上面无并发编辑的两轮；全程未修改 runner、未跳过检查。逐条见 [run-state §2](run-state.md)。

**过程日志**：`/tmp/us24-accept-base3.log` / `/tmp/us24-accept-fault1.log` 是外层 shell 的过程日志，收口时已按临时物纪律删除；判读与复跑依据是 `.loop/runs/*/report.json`（含 `record.json` 与逐 check 日志），两轮报告均保留在 `.loop/runs/`。

### 1.3 逐包覆盖率（`cover` 目标读数）

> 2026-11-07 轮次读数，**已被 §1.4 取代**，保留为历史记录（对应修复前源码树）。

`cover` 自身带 `-tags=integration` 重跑；`check-coverage.sh` 判定 `全部包达标`（退出 0）。分档：`core` / `tool` / `memory` / `storage` / `postgres` / `assets` ≥ 70%，其余 ≥ 50%。

| 包 | 覆盖率 | 分档 | 四态 |
| --- | --- | --- | --- |
| `internal/storage/postgres` | 72.1% | ≥70 | **通过** |
| `internal/storage` | 73.2% | ≥70 | **通过** |
| `internal/assets` | 74.9% | ≥70 | **通过** |
| `internal/core` | 78.5% | ≥70 | **通过** |
| `internal/tool` | 78.4% | ≥70 | **通过** |
| `internal/memory` | 86.0% | ≥70 | **通过** |
| `internal/flow` | 64.0% | ≥50 | **通过** |
| `internal/app` | 74.1% | ≥50 | **通过** |
| `internal/callers` | 88.0% | ≥50 | **通过** |
| `internal/cli` | 81.6% | ≥50 | **通过** |
| `internal/web` | 85.5% | ≥50 | **通过** |
| 其余包（最低 `internal/testutil` 51.5%、`postgres/pgtemp` 54.5%） | — | ≥50 | **通过** |

`scripts`（`coverage: [no statements]`）按既有规则跳过。本 US 新增的 `internal/assets` 由 `≥50` 收紧到 `≥70`，实测 74.9%；`internal/flow` 与 `internal/diagnostics` 是既有短板包，本 US 未扩大缺口（`flow` 64.0%、`diagnostics` 68.6%，分档均为 ≥50）。

### 1.4 最终验收复跑（2026-11-08，源码冻结后）

review ② 打回后的定向修复改了源码（`internal/core` / `internal/flow` / `internal/app` / `internal/storage/postgres` 及其测试），§1.1–§1.3 的读数不再代表当前树，按「源码变了就必须重跑全量」重跑一轮。运行期间不编辑任何 tracked / 未忽略文件。

| 轮次 | 命令 | 窗口（CST） | 结果 |
| --- | --- | --- | --- |
| 七目标 | `dev-env/scripts/with-postgres.sh make check` | 2026-11-08 00:18:14–00:55:48 | **退出 0**；七目标全绿，`check-coverage: 全部包达标` |
| 全量 base stage | `sh scripts/check-acceptance.sh --stage harness/stages/us24-base.json` | 00:56:18–01:39:14 | **21/21 check passed**，`status=passed`、`exit_code=0`，报告 `.loop/runs/accept-20261107T165607.936093205/` |
| 故障 stage | `sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us24-fault.json` | 01:39:44–01:40:13 | **2/2 check passed**，`status=passed`、`exit_code=0`，报告 `.loop/runs/accept-20261107T173936.251595917/` |
| 定向 e2e | `with-postgres.sh go test -tags=e2e -count=1 -timeout=30m -run '^(TestFlow_.*\|TestBackfill_RestartRecovery)$' ./internal/e2e` | 01:40:19–01:46:26 | 退出 0，`ok github.com/ymedword/basalt/internal/e2e 365.551s` |

**源码冻结证据**：两轮 stage 报告的 `fingerprint` 同为 `75ca47f3a9f469de281d22d36bb75f67277ddd427f97d6fcc56600f5d2d302bd`（2176 个文件；`base_commit=138382f`、`dirty=true`——修复尚未提交），e2e 结束后未再改树。作废轮次纪律同 §1.2：本轮没有任何在跑验收期间编辑 tracked / 未忽略文件的记录。

**base stage 逐 check（21/21，全部 exit 0）**：

| check | 耗时 | 读数 |
| --- | --- | --- |
| `fmt` | 55.0s | `check-format: gofmt / gofumpt 均干净` |
| `lint` | 279.6s | `-tags=integration` 的 go vet / staticcheck，零诊断 |
| `sec` | 291.0s | gosec 零告警；govulncheck 无可达漏洞（模块层 2 条既有不可达漏洞未变） |
| `test` | 187.7s | 40 包 ok、2 个 `no test files`、0 FAIL |
| `test-integration` | 490.3s | 42 包 ok、2 个 `no test files`、0 FAIL |
| `test-pg` | 256.2s | 3 包 ok、0 FAIL |
| `cover` | 633.9s | `check-coverage: 全部包达标`，逐包见下表 |
| `build` | 3.2s | `CGO_ENABLED=0 go build -trimpath -ldflags … -o bin/basalt ./cmd/basalt`，产物 41,148,576 字节 |
| `eval-build` | 4.0s | 退出 0 |
| `behavior` | 3.1s | 8 例 7 PASS + 1 允许 SKIP（`outcome-real-model-answer`，用例声明 `provider=deepseek`，本次未给 `--provider`） |
| `injection` | 1.8s | 4 PASS |
| `us24-base-core` | 3.1s | 112 例 PASS，0 skip |
| `us24-base-assets` | 0.4s | 10 例 PASS |
| `us24-base-pg` | 41.9s | 143 例 PASS（2026-11-07 为 141，新增 2 例是 §8 修复的定向回归） |
| `us24-base-app` | 110.4s | 240 例 PASS（2026-11-07 为 236，新增 4 例同上） |
| `us24-base-callers` | 0.5s | 5 例 PASS |
| `us24-base-cli` | 1.4s | 4 例 PASS |
| `us24-base-web` | 3.1s | 2 例 PASS |
| `us24-base-console-test` | 167.6s | exit 0 |
| `us24-base-console-build` | 15.3s | exit 0 |
| `us24-base-race` | 19.2s | 6 例 PASS（`-race`，与 `CGO_ENABLED=0` 发布构建分属不同 check） |

**故障 stage（2/2，全部 exit 0）**：

| check | 耗时 | 读数 |
| --- | --- | --- |
| `us24-fault-pg` | 10.1s | 20 例 PASS（`-race -tags=integration`），0 skip |
| `us24-fault-app` | 11.4s | 13 例 PASS（`-race -tags=integration`），0 skip |

**逐包覆盖率（本轮 `cover` 目标）**：

| 包 | 覆盖率 | 分档 | 四态 |
| --- | --- | --- | --- |
| `internal/storage/postgres` | 72.18% | ≥70 | **通过** |
| `internal/storage` | 73.14% | ≥70 | **通过** |
| `internal/assets` | 74.85% | ≥70 | **通过** |
| `internal/core` | 78.65% | ≥70 | **通过** |
| `internal/tool` | 78.37% | ≥70 | **通过** |
| `internal/memory` | 86.01% | ≥70 | **通过** |
| `internal/flow` | 63.98% | ≥50 | **通过** |
| `internal/app` | 74.11% | ≥50 | **通过** |
| `internal/callers` | 88.05% | ≥50 | **通过** |
| `internal/cli` | 81.64% | ≥50 | **通过** |
| `internal/web` | 85.48% | ≥50 | **通过** |
| 其余包（最低 `internal/testutil` 51.52%、`postgres/pgtemp` 54.55%；`internal/diagnostics` 68.61%） | — | ≥50 | **通过** |

过程日志 `.loop/reviews/us24-repair-20261107/final-make-check.log` 与 `final-e2e.log` 保存在 `.loop/`（整体不进版本库），stage 判读依据是 `.loop/runs/*/report.json` 与逐 check 日志；回填后工作树与冻结指纹的逐文件差异见 §8.5。

> **2026-11-08 后注**：本节读数对应提交 `9c01b2e` 的树。合入 main 后 CI 在 `015-backfill` 发现 e2e 夹具身份缺陷，修复该缺陷又暴露一处 US-24 产品回归（cron / zone 变更不重排 `next_fire_at`）；修复改动了 3 个文件（见 §8.8），因此本节的「源码冻结」结论只对该提交成立。修复后的完整复验以推送后 CI 的全绿读数为准，本地定向复跑记录在 §8.8 与 run-state §5。

## 2. Harness 与逆改判别力

### 2.1 结构（引用公共配置，不复制）

两份 stage 只声明本 US 专属的 check 与 `required_tests`，公共门禁（`fmt` / `lint` / `sec` / `test` / `test-integration` / `cover` / `build` / `eval-build` / `behavior` / `injection`）由 `harness/acceptance.json` 的 full profile 提供，不在此重抄。

| Stage | Check | 命令要点 | 具名测试 |
| --- | --- | --- | --- |
| `us24-base` | `us24-base-core` | `go test ./internal/core -run ^(TestLease_\|TestKeepLease_\|TestRun_RenewFailure…\|TestScheduler_)` | 4 |
| `us24-base` | `us24-base-assets` | `go test ./internal/assets -run ^TestAssets_` | 4 |
| `us24-base` | `us24-base-pg` | `-tags=integration ./internal/storage/postgres` | 38 |
| `us24-base` | `us24-base-app` | `-tags=integration ./internal/app`（含 `TestDeliveryService_`） | 44 |
| `us24-base` | `us24-base-callers` | `go test ./internal/callers -run ^TestLimiter_` | 1 |
| `us24-base` | `us24-base-cli` | `go test ./internal/cli -run ^TestCLI_(Assets\|Workers)` | 2 |
| `us24-base` | `us24-base-web` | `go test ./internal/web -run ^TestWeb_(FlowRootBusy\|RunViewLeaseFields)` | 2 |
| `us24-base` | `us24-base-console-test` | `npm --prefix assets/console test` | 0（Vitest，校验器只认 `go test -json`） |
| `us24-base` | `us24-base-console-build` | `npm --prefix assets/console run build` | 0（同上） |
| `us24-base` | `us24-base-race` | `-race -tags=integration ./internal/app -run ^TestShutdown_` | 5 |
| `us24-fault` | `us24-fault-pg` | `-race -tags=integration ./internal/storage/postgres` | 13 |
| `us24-fault` | `us24-fault-app` | `-race -tags=integration ./internal/app` | 9 |

两份 stage 的 `required_tests` 去重后 **100 个具名测试**（独立复核后新增两条：`TestScheduler_DeliveredRunReclaimedAtHigherEpochSkipsSettlement`、`TestDeliveryService_DeliverSettledRowFailsWithoutSend`），逐名用「源码里存在 `func <name>(`」校验，0 缺失——T039 analyze 复跑时按 98 个校验全过，`check_required.sh` 在补入两条后复算 `required_total=100 found=100`。`-race` 判据分布在 `us24-base-race` 与两个 fault check，与 `CGO_ENABLED=0` 发布构建**分属不同命令**。

### 2.2 逆改判别力（逐任务红态）

每条「产品的守点」都有一次**故意破坏 → 逐字转红 → 还原转绿**的实测，探针副本一律建在仓库外临时目录、用毕即删；**未改树、未加抑制注释、未放宽阈值**。

| 守点 | 逆改内容 | 逐字红态 | 记录 |
| --- | --- | --- | --- |
| 归属 fencing | 把 fence 的归属判定改为只读不拒 | `run_lease_test.go:133/:194` `Expected error with "core: lease ownership lost" in chain but got nil` | T008 |
| 学习抽取三写面 | A 重试绑定改 `retry.RunID` / B 触发 Run 改 `row.RunID` / C 去掉预期旧尝试 CAS / D 去掉同来源判定 / E Begin 走未 fenced 路径 | 五处分别逐字失败（`run_lease_test.go:256/:174` 等） | T009 |
| Run 终态 fencing | 中性化 `runFencedTx` 归属判定 | `run_lease_test.go:521/:553` 同上文案 | T010 |
| 单事务准入 | A 去掉事务内会话繁忙返回 / B 停用事务内任务记账 / C 会话判据 `OR`→`AND` | `跨 Worker 同会话必须 409 SESSION_BUSY` / `sql: no rows in result set` / `恢复未收尾旧 Run 时…必须 409` | T012 |
| 准入优先级 | A 交换会话门禁与实例上限 / B 实例计数改包级共享 | `expected: "SESSION_BUSY" actual: "TOO_MANY_RUNS"` / `第二个 Worker 必须能开始自己的 Run` | T013 |
| 触发去重（三层阶梯） | ① 删 planned 预检 → ② 再删 active 预检 → ③ 再中性化 CAS | `scheduler: task busy` → `core: scheduled next_fire_at changed` → `duplicate key … ux_task_executions_cron_planned (SQLSTATE 23505)` | T015 / T016 |
| 回收作用域 | 去掉 `expires_at <= now` / 审计 `run_id = ANY(...)` / 单轮超时改 `WithCancel` / 执行域状态字面量 | 四条逐字失败（`只有过期租约被领取` 等） | T017 |
| 投递恢复作用域 | A B2 所有权谓词中性化 / B B1 终态集合删两项 | `reclaimer_test.go:493` `expected: "pending" actual: "skipped"` / `"0" is not greater than or equal to "2"` | T018 |
| B3 查证 × 续约 | A 写入侧 `FOR UPDATE` 换 `EXISTS` / B 只删预检 / C 预检 + CAS 同删 / D 中性化孤儿谓词 | `查证在续约提交前就返回了` / `恢复结论写入未生效` / `扫描后的合法重试不得被旧结论覆盖` / `should have 1 item(s), but has 2` | T019 |
| 停机顺序与在途上限 | A 跳过 join 直接释放 / B `joinDeadline` 去掉 `min(…)` / C 超界分支先关库 / D 交互取槽改阻塞 | `观测面在收尾退出之后关闭` / `join 必须使用剩余 grace 预算` / `超界分支不得关闭数据库` / `expected: *core.InstanceFullError` | T026 |
| 资产与资产指针 | 逐文件哈希 / 代次守卫 / 准入零占用 | 见 T029 / T032 行 | T029 / T032 |
| 未决动作门序 | （产品修复后补的红态）`require.Never` 断言未 Release | `TestFlow_InFlightAttemptWithUnresolvedActionAbandons` | T038 item08 |

## 3. 交付物核对

模块卡 §3 的 60 条路径逐条判存在性（脚本 `.loop/t039/deliverables.txt`，逐行 `OK`；过程产物，收口时已删），另核对三张新表与 schema 版本字面量。

| 类别 | 条目 | 结果 |
| --- | --- | --- |
| 端口与类型 | `internal/core/{lease,store,task,scheduler}.go` | 通过（4/4 存在） |
| Flow | `internal/flow/{ports,worker,control,revision,execution}.go` | 通过（5/5 存在） |
| 装配 | `internal/app/{flow,worker_runtime,reclaimer,assets,assets_publish,learning_recovery,resources,wire,run,schedule,delivery,concurrency_limits,run_quota,agent_admin,baseline,flow_http}.go` | 通过（16/16 存在） |
| 存储 | `internal/storage/postgres/{lease_store,subtree_gate,worker_store,schedule_coord,reclaimer_store,run_store,task_store,flow_store,flow_close_store,flow_revision_store,delivery_store,resource_store,learning_store,recovery}.go` | 通过（14/14 存在） |
| 迁移 | `internal/storage/postgres/migrations/0012_distributed_execution.sql` | 通过；含 `worker_nodes`（:31）/ `execution_leases`（:48）/ `lease_events`（:69）三表 |
| 新增包 | `internal/assets` | 通过（包存在，入 70% 覆盖档） |
| 其余生产代码 | `internal/callers/limiter.go`、`internal/memory/learning.go`、`internal/adapters/cron/cron.go`、`internal/provider/{client,service}.go`、`internal/cli/{assets,workers,config}.go`、`internal/web/{views,handlers_flow,envelope}.go` | 通过（12/12 存在） |
| 前端 | `assets/console/dist/index.html`、`assets/console/dist/integrity.json` | 通过；US-24 改动 `api.ts` / `views/{diagnostics,overview,run-detail}.ts` 后由 `npm run build` 重建，`dist/assets/` 两个包由 `index-CgqLzF5n.js` / `workflow-template-CHPW9ZVb.js` 换为 `index-C68SfmIA.js` / `workflow-template-CR2ODnTn.js`，`index-*.css` 哈希不变 |
| 契约 | `docs/contracts/{run-and-errors.md,flow.md,agent-md.md,openapi.yaml}` | 通过（4/4，`schema.pg.sql` 未改） |
| Harness | `harness/stages/us24-base.json`（10 check）、`harness/stages/us24-fault.json`（2 check） | 通过 |
| schema 版本 | `internal/storage/postgres/connect.go:59` `SupportedSchemaVersion = 12` | 通过 |

## 4. 前序回归与判据面

### 4.1 判据面变更（逐路径）

「判据面变更」指改变验收语义的改动（删断言、改判据口径、换执行者、改阈值）。全部逐条列出，供 review ② 复核。

逐函数统计（可复算）：tracked 测试文件相对基线 `git diff -U0 138382f -- '*_test.go' | grep -cE '^[-+]func Test'` → **删除 48 行 / 新增 69 行**（新增比初稿的 67 多 2 条，即独立复核 #4 补的两条用例）；另有 **32 个新增未跟踪测试文件贡献 142 个 `Test*`**（`git ls-files --others --exclude-standard -- '*_test.go'`）。按**唯一函数名**去重后相对基线净变化 **−34 / +198**（`comm` 于两侧排序去重名单；差值是文件搬移与更名造成的同名增删相抵）。新增用例集中在租约 / 回收 / 资产 / 停机 / Flow 接管五域，删除集中在进程内 cron 触发与启动恢复扫描两处（下表 #1–#3）。

| # | 路径 | 变更 | 依据 / 裁决记录 |
| --- | --- | --- | --- |
| 1 | `internal/core/scheduler_test.go`、`internal/app/schedule_test.go` | 进程内 cron 触发（`Fire_*` / `CronEngine.Add/Start/Stop` / `locks` / `gens`）整体改为数据库到期扫描（`Dispatch_*`）；旧 50 例 → 新 55 例 | 模块卡 P-03 明列「判据面变更：改为等价的数据库断言」，§4.8 / Q3 冻结等价映射；逐条映射见 run-state T016 行 ①②③ |
| 2 | `internal/adapters/cron/stop_lifecycle_test.go`（整文件删除）+ `cron_test.go` 两例 | 删除 `TestEngine_AddAndStop` / `TestEngine_StopWaitsForJobs` 与五例停机生命周期；`TestEngine_ParsesSixFieldWithCRONTZ` → `TestEngine_NextParsesSixFieldWithCRONTZ` | P-02「`CronEngine` 增 `Next`、删 `Add/Start/Stop`」——被删断言的对象已从产品中移除；六段 / `CRON_TZ` 判据在改名用例中逐字保留。见 run-state T014 裁决 ⑪ / T016 裁决 ④ |
| 3 | `internal/core/task.go`（`RecoverStartedExecutions` 删除）→ `internal/storage/postgres/task_store_test.go` | 「启动把上次 `started` 收成 failed」的断言不保留在端口层，改由 `TestTaskStore_NoStartupRecovery` 反射断言端口不存在扫描 + 回收器用例承接行为 | FR-24-02「启动不得全局扫描」；行为迁到 T017 / T018 的回收器用例，见 run-state T015 裁决 ① / T016 裁决 ② |
| 4 | `internal/e2e/{us19_action,us19_recovery,us19_lifecycle,us19_fixture}_test.go` | 关闭控制由固定 200 改为 `requireCloseAccepted`（接受 200/202 且 `code=OK`），另补精确终态等待 | `flowWriteView` 对 accepted 返回 202 是 US-19 起的既定契约；旧用例钉 200 属过度指定。**收敛仍由精确终态证明，零重放判据未动**，计 0。run-state §2 2026-11-07 行 |
| 5 | `internal/e2e/*_test.go`（`TestFlow_BinaryRestartReconciles32OpenWithin30Seconds`） | `flow_node_attempts` 固定总数 32 改为三条不变量（≥32 根各一次领取 / 重试的 `interrupted` 从未准入 / 逻辑尝试号 ≤ `max_attempts_per_node`） | `YieldUnadmitted` 对未准入尝试留历史行并退款，行数随调度时序浮动（实测 36/38）。零重放仍由 `calls==33` 与四条动作守卫承担，**不放宽阈值**。run-state §2 2026-11-07 行、§3「预算越界说明（T037 续）」 |
| 6 | `harness/stages/us24-{base,fault}.json` | 从 `required_tests` 各删一条**已不存在**的旧测试名 `TestFencedWrites_InterleavedTakeover`（替换名 `TestFencedWrites_OldHolderBlocksTakeover` 原本已在两份 stage 中） | 模块卡 R5 / R7 §4 表 5 已判定该时序在行锁下不可能发生并改名；stage 是派生工件，R7 是权威。run-state T034 ⑤ / T008 裁决 ① |
| 7 | `scripts/check-coverage.sh` | `assets` 加入 70% 覆盖档（原会按基名落到 50%） | 模块卡 §4.7 判「按 core 口径 ≥70%」——**收紧**判据，非放宽 |
| 8 | `specs/024-state-distributed/contracts/README.md` | openapi 增量由「5 个错误码」更正为「6 个新业务码」 | T039 analyze 复跑发现 1（LOW）；与 `internal/web/envelope.go:123-128` 六个常量对齐 |

`§4.8 / Q3` 的七项冻结口径（6 段 cron / `CRON_TZ` / 跳过 / 定义哈希 / 退休 / 错失记录 / 唯一执行）逐条有对应用例，映射表见 run-state T016 行 ①–⑦。

**review ② 裁决（2026-11-07，用户明确）**：上表八项（#1–#8）判据面调整被**接受为架构适配**——它们随已批准的租约 / 准入 / 回收 / 资产设计而来，不改变 FR 级语义；该接受**不代表当前实现被接受**（review ② 同时对三项已复现缺陷与三项高风险静态发现打回，修复见 §8）。修复轮新增的判据（准入回查异主拒绝、Provider 凭据 pin 窗口、清单 × 执行快照一致、子 Flow 任务作用域）均为**加严**，不构成放宽或删除，逐条并入 §8，历史行不改写。

### 4.2 前序模块回归

见 §1.1 `test` / `test-integration` / `cover` 三个目标的读数：`go test -race -cover ./...` 与 `go test -tags=integration ./...` 全仓执行，前序 22 个 US 的用例与 US-24 新增用例在同一轮判定。

## 5. 全局不变量自查（机器可判）

逐条复核命令与当场读数（脚本 `.loop/t039/invariants.txt`；过程产物，收口时已删，命令保留于本表可按行复跑）。

| # | 不变量 | 复核命令 | 读数 |
| --- | --- | --- | --- |
| 1 | `go.mod` 无 Agent 框架坐标 | `grep -Ei 'langchain\|llamaindex\|autogen\|crewai\|semantic-kernel' go.mod` | 无命中（rc=1） |
| 2 | `context.WithValue` 只允许追踪标识 | `grep -rn 'context.WithValue' --include=*.go internal/` | 生产代码仅 `internal/web/middleware.go:41/79`（request-ID），其余为测试 |
| 3 | 无自动工具循环（`BeginCall` 不进 `core`） | `grep -rn 'BeginCall(' --include=*.go internal/core/` | 无命中（rc=1） |
| 4 | 无明文凭证落盘 | 凭证字面量扫描（`sk-` 前缀 / `Bearer` 头 / 私钥头） | matches=0 |
| 5 | 不用 ORM | `grep -Ei 'gorm\|sqlx\|entgo\|ent\\.' go.mod` | 无命中（rc=1） |
| 6 | 不使用 `time.Local` | `grep -rn 'time.Local\|\.Local()' --include=*.go internal/` | 生产代码零命中，仅两处注释说明逆改点 |
| 7 | 不使用 `math/rand` | `grep -rn 'math/rand' --include=*.go internal/` | 无命中（rc=1） |
| 8 | 无 dot import | `grep -rn '^\\s*\\.\\s*"` --include=*.go internal/` | 无命中（rc=1） |
| 9 | `internal/core` 只依赖标准库 | `go list -f '{{join .Imports "\\n"}}' ./internal/core` | 全部为标准库 + 本仓 `internal/core` 自身 |
| 10 | `internal/core` 无第三方坐标 | `go list -deps ./internal/core \| grep -v '^internal\|^github.com/ymedword/basalt'` | 仅标准库 |
| 11 | 门禁链路不引入 Node / npm | `grep -rn 'npm\|node ' Makefile scripts/check-*.sh` | 无命中（rc=1） |
| 12 | 生产代码不使用 SQLite | `grep -rn 'sqlite' --include=*.go internal/app internal/web internal/core` | 仅测试文件命中 |
| 13 | 零新增依赖 | `git diff 138382f -- go.mod go.sum` | 空输出（0 行） |
| 14 | `CGO_ENABLED=0` 发布构建 | `make build` | 见 §1.1 `build` 目标读数 |

## 6. 复核结论与剩余人工项

### 6.1 独立复核（逐条处置）

复核轮次 2026-11-07 16:25，独立只读复核在**未看本报告**的前提下遍历 US-24 工作树与模块卡，产出 28 条发现（原始编号 0–27：2 blocker / 15 major / 11 minor），raw 存 `.loop/t039/review_findings.json`（收口前删除）。修复批次 18:41–19:04 落树；随后由四个只读核对代理逐条对当前树核验，27 条 FIXED、1 条 PARTIAL（#4），PARTIAL 的残余窗口本轮补修并新增用例，全部 28 条关闭。修复过程中未放宽任何断言、未新增抑制注释。

| # | 级别 | 发现 | 处置 | 证据（当前树） |
| --- | --- | --- | --- | --- |
| 0 | major | e2e 套件仍等待已删除的启动恢复日志，且不安装 / 发布准入所需的 bundle | 已修 | `internal/e2e/recovery_test.go:100` 改 `waitNoRunningRuns` 轮询库终态（reason=`worker_lost`），日志文案不再是判据；`internal/e2e/harness_test.go:1529` `publishAgentBundles` 逐个 `assets install` 发布内容寻址 bundle；`TestBackfill_RestartRecovery` PASS 17.39s、定向 e2e 全绿（包 384.701s，见 run-state §4） |
| --- | --- | --- | --- | --- |
| 1 | major | `Fire→Start` 改名丢掉了 capped-enumeration 与零 `last_run` 的错失触发断言 | 已修 | `internal/core/scheduler_test.go:1730-1773`（三子用例 + `fakeEngine` 记录 limit `:253-258`）；生产 `scheduler.go:1492/:1501` |
| 2 | minor | `scheduler-` RunID 前缀断言被删且全树无替代 | 已修 | `scheduler_test.go:1165` 正则 `^scheduler-\d{13}-[a-z0-9]{6}$`，`:1175-1176` 与执行行等值比较；PG 侧 `schedule_coord_test.go:191` |
| 3 | minor | busy-skip 执行行不再断言 `TriggerCron` | 已修 | `schedule_coord_test.go:210`（真实调用 `RecordBusyTrigger` 后断言落库值） |
| 4 | **blocker** | 失权后调度器仍按重读租约发投递并补收尾 | 首修复（owner 比较）+ 本轮补修（代次 + 预写行状态） | `internal/core/scheduler.go:826-841`：守卫同时比较 `OwnerWorkerID` 与 `OwnershipEpoch != 1`（准入代次恒为 1，`run_store.go:293-296`；续约不改代次，`lease_store.go:321-330`）；`internal/app/delivery.go:136-147`：`Deliver` 在预写行非 `pending` 时一律不发送；用例 `TestScheduler_DeliveredRunReclaimedAtHigherEpochSkipsSettlement`、`TestDeliveryService_DeliverSettledRowFailsWithoutSend` |
| 5 | major | 本地未注册 Agent 的到期行永久堵在批次头部 | 已修 | `scheduler.go:880` 优先 `AgentDueLister`；`schedule_coord.go:60-66` 按 `agent_name = ANY` 只取本地行；`wire.go:1523` |
| 6 | major | 启动对账把本工作区缺席的其他 Agent 任务全部退休 | 已修 | `scheduler.go:343` `warnForeignAgentsLocked` 只记 INFO；`commitAgentLocked` 只收本 Agent 行 `:1337-1345` |
| 7 | major | 错失宽限与 next-fire 用进程时钟而非数据库时钟 | 已修 | `scheduler.go:895` 优先 `DBNowReader`；`schedule_coord.go:114-120` 返回 `clock_timestamp()`；`dispatchOnce` `:536-539/:573` 同一次读数 |
| 8 | major | 跨 Worker 准入先返回 `SESSION_BUSY` / `TOO_MANY_RUNS`，晚于权威 `CALLER_CONCURRENCY_LIMITED` | 已修 | `internal/core/service.go:313` `CheckAdmission` 前移；`internal/app/run_admission.go:120` precheck 排在 `acquireInflight` 前 |
| 9 | major | CLI 归档路径跳过跨 Worker 取消与有界等待 | 已修 | `internal/app/agent_admin.go:1230/:1243-1262/:1658-1662`（`awaitArchiveRuns` 移出 `if live`，CLI 注入 `RunCanceller`） |
| 10 | minor | 409 `AGENT_BUSY` 在 HTTP 边界丢 `data.unmet_predicates` | 已修 | `internal/web/views_admin.go:620/:658-660` |
| 11 | major | 子 Run 创建只看"某条 flow_root 租约有效"，不校验调用方 token | 已修 | `internal/storage/postgres/flow_store.go:437-443`（`FOR UPDATE` 锁行 + owner + epoch）；`run_store.go:1114-1117` INSERT EXISTS 同谓词；`internal/flow/execution.go:174` 透传 token |
| 12 | major | `Recover` 依据事务前 `Isolated` 快照且不锁子树 run 租约行 | 已修 | `flow_close_store.go:186` `FOR UPDATE OF l` + 事务内数据库时钟重读（`subtree_gate.go:73-82`） |
| 13 | minor | 竞争失败的 rework 把 root 的 A3 路由原因改写成 `advance_unavailable` | 已修 | `internal/flow/worker.go:323` 把 `ErrFlowRootBusy` 归入"不动行"分支；`worker_test.go:58-72` |
| 14 | major | Precheck→准入指针漂移返回 503，但 FR 要求 409 `RESOURCE_VERSION_MISMATCH` | 已修 | `docs/contracts/run-and-errors.md:534`；`openapi.yaml:4883/:4922`；`internal/web/handlers_sessions.go:387-395`；用例 `TestContract_ResourceVersionMismatchDualStatus` |
| 15 | major | 契约写 `hashtextextended(session_id)`，代码实际是 Go 侧 `sha256("basalt/session/"+id)` | 已修 | `run-and-errors.md:214`；`run_store.go:320`；`migrate.go:60-64` |
| 16 | minor | `us24-fault` 审批记录声称 `-count=2`，实际两份 check 都是 `-count=1` | 已修 | `harness/stages/us24-fault.json:4/:73` |
| 17 | minor | openapi 称已释放 Run 的 `worker_instance_id` 为空，实际保留最后属主 | 已修 | `openapi.yaml:6504`；`internal/web/views.go:460-463` |
| 18 | minor | `run-and-errors.md:286` 仍称工作区文件锁保证"库只有一个写者" | 已修 | `run-and-errors.md:283` 与 `:286` 已同口径 |
| 19 | **blocker** | 执行期从不读准入固定的 bundle 目录，实际执行内容来自工作区 | 已修 | `internal/assets/reader.go`（新增 `OpenBundleReader`，fail-closed）；`internal/app/resources.go:820`；`workspace/context_loader.go:133-168`；用例 `assets_admission_test.go:417-463` |
| 20 | major | MCP `applied_version_id` 为 NULL 时返回 500 而非 503 | 已修 | `resources.go:704` 返回 `ErrResourceVersionMismatch`；`handlers_sessions.go:387-391` 双形态映射；用例 `assets_admission_test.go:690-707` |
| 21 | major | CLI 归档在远端 Run 仍在跑时可完成移动 | 已修 | 同 #9；用例 `TestArchive_CLIPathRejectsRemoteLiveRun` |
| 22 | major | FR-24-44 用例手拼执行期路径，发现不了"执行期不读 bundle" | 已修 | `internal/assets/bundle_test.go:349` 改用 `OpenBundleReader`；整链判据移到 `assets_admission_test.go:453` 的生产构造函数 |
| 23 | minor | 首个 bundle 没有出货构建入口，文档化首装步骤不可执行 | 已修 | `internal/assets/command.go:111`；`internal/cli/assets.go:87-107`；`distributed_port.go:107-131`；`docs/Operations.md:1720` |
| 24 | major | `renew_failed` 租约事件没有生产写入方 | 已修 | `internal/core/lease_keeper.go:119/:126` 经 `RenewFailureRecorder` 写；`postgres.LeaseStore` 实现并在生产装配传入 |
| 25 | minor | 续约截止停止丢弃取消原因，Run 原因成 `context canceled` | 已修 | `internal/core/run.go:543` 接收 `context.Cause`，映射 `renew_failed`；各调用点传 `context.Cause(ctx)` |
| 26 | minor | FR-24-35 用例未断言 `ttl - renewInterval` 下界 | 已修 | `internal/core/lease_test.go:197` 记录 start 并断言 `elapsed >= ttl - renewInterval` |
| 27 | minor | A1 把未重放动作标 `unknown` 但不写诊断事件 | 已修 | `internal/storage/postgres/recovery.go:130` 返回逐行 `UnreplayedAction`；`internal/app/reclaimer.go:265-304` 提交后写 `actor=recovery` / `reason_code=recovery_unknown`；`wire.go:1518` |

**复核后新增的判据面变更**（均为加严，逐条登记）：`us24-base-core` 的 `required_tests` 增加 `TestScheduler_DeliveredRunReclaimedAtHigherEpochSkipsSettlement`；`us24-base-app` 的 `-run` 正则增加 `TestDeliveryService_` 并新增 `TestDeliveryService_DeliverSettledRowFailsWithoutSend`。两条分别护住 #4 的代次与预写行状态两半；无删除、无放宽。

**补修引入的静态门禁问题**：`internal/app/assets_publish.go:211` 的 `fmt.Errorf("Agent …")` 触发 ST1005（首字母大写），已改为小写 `agent`，未加任何抑制注释。

### 6.2 静态门禁妥协点（需 review ② 确认）

**本轮新增代码零抑制注释**：`git diff 138382f -- '*.go' | grep -cE '^\+.*(//nolint|#nosec|//lint:ignore|//nosec)'` = **0**。早前任务首跑 gosec 报的 4 处 G304（`internal/assets/bundle.go:660/:715/:823`、`internal/app/assets_publish.go:141`）全部改为 `os.OpenRoot` 根句柄，**未用 `#nosec` 换取绿态**（run-state §2 2026-11-07 行）。

树内既有抑制注释未被本 US 触碰：`git grep -nE '(#nosec|//nolint|//lint:ignore)' -- '*.go' | wc -l` 在基线 `138382f` 与本轮工作树均为 **198** 行（74 个文件；其中 6 个文件本 US 有改动，但改动未触及抑制行）；52 个新增未跟踪 `.go` 文件中抑制注释为 **0**。**新增 `t.Skip(` = 0**（tracked diff 与 52 个新增文件均为 0）。

### 6.3 人工验证项（M-01～M-14，模块卡 §6.1）

执行方式 `.loop/t038/drill.sh all`（WSL root；两个 `basalt serve` 进程、两个独立工作区、同一 PostgreSQL 库；模型与 webhook 为本地协议桩；二进制由当轮 `CGO_ENABLED=0` 源码构建）。时间 2026-11-07 14:55:10–15:02:35，日志 220 行，逐项读数原落 `/tmp/us24drill/evidence/item*/run.txt`。**原始脚本与输出均已删除（收口时按临时物纪律清理），不可再复核**；下表逐项读数只是当轮转录，与 run-state T038 行同源。

**14 项中 13 项保留本机读数（当轮 PASS），M-12 降为未验证**（review ② 裁决：DB 侧 `expires_at` 注入不能证明宿主时钟偏移无影响）。受本轮修复影响的判据以 `.loop/reviews/us24-repair-20261107/` 的定向反例与回归为准（§8），**不把旧演练转录当成本轮复验证据**。

| 项 | 场景 | 注入方式 | 实测结果 | 是否重放 |
| --- | --- | --- | --- | --- |
| M-01 | 定时触发不重不漏 | 两 Worker 抢同一 `*/5` 任务，运行 2 分钟 | 非 skipped 执行 25 / claimed 事件 25 / 重叠 0 / skipped 1 且不回填旧 `planned_at` | 否 |
| M-02 | 崩溃接管（kill -9） | 属主进程 `kill -9` | B 合法接管，Run `failed` + `worker_lost`；B 自己的在跑 Run `completed` | 否（甲消息模型请求恰 1 次） |
| M-03 | 暂停接管（SIGSTOP） | 属主进程 `SIGSTOP` | 同 M-02；A 恢复后不覆盖 B 的终态 | 否 |
| M-04 | 恢复窗口内同会话串行 | 恢复期并发 5 次同会话准入 | 5 次全部 409 `SESSION_BUSY`，Run 行数恒 1；租约到期接管收尾后放行 | 否 |
| M-05 | 跨 Worker 取消 | 非属主发起取消 | 200 且如实返回 `running`；属主观察标志并收敛 `cancelled` | 否 |
| M-06 | 投递 `unknown` | 投递结果不可证明 | 落 `unknown`，webhook 恰一次，之后不重发 | 否 |
| M-07 | 资产 / 生命周期准入 fail-closed | manifest 尾随字节 / 内容哈希 / 低代次 / 高代次 / 删目录 | 409 `ASSET_VERSION_MISMATCH` / 409 `AGENT_LIFECYCLE_STALE` / 409 `AGENT_LIFECYCLE_MISMATCH` / 503 `ASSET_UNAVAILABLE`，四次拒绝均零占用；重装后 200 | 否 |
| M-08 | 未决动作不推进 | Flow 根带 `dispatching` 动作，属主暂停 | 根停在 `running` 不推进；n1 只 1 次尝试 1 条 Run；n2 不重派；`flow_actions.outcome` 未被改写为 succeeded；10s 内租约事件 3 条（≤12） | 否 |
| M-09 | 停机顺序 | SIGTERM | drain 期间续约继续；Run 跑完 → 租约释放（1794034839096）→ worker 停止（1794034839434）；连接归零；停机后同会话立即可准入 | 否 |
| M-10 | v11 库停机迁移 | 停机后前向迁 12 | runs / task_executions 回填 `legacy` / 代次 0 / 已过期并被首轮回收收尾 `worker_lost`；Flow 根回填类由 `TestUpgrade_StopTheWorldLeaseBackfill` 判卷 | 否 |
| M-11 | 数据库不可达 | 停 TCP 代理 14s | 失联属主被接管 `failed`；重连后心跳恢复且不覆盖接管结果；中断期间模型请求 1 次 | 否 |
| M-12 | 只用数据库时间 | DB 侧 `expires_at` ±10min（**不足以证明宿主时钟偏移无影响**） | 原轮次转录：+10min 时 20s 内 0 次接管；-10min 时 30s 内被接管（host 与 db 差 -125ms）。**四态：未验证**（判据不足，见 run-state §4） | 不适用 |
| M-13 | 投递不重复 | 终端残留 + 无 Run 前置投递 | 与 M-06 同口径：`unknown` / `skipped`，之后不重发 | 否 |
| M-14 | 网络分区 | 分区后属主失联 | 被接管 `failed`；重连后不覆盖；同会话仍 1 条 Run | 否 |

**偏差（如实登记）**：item07 用「篡改 manifest 尾随字节 / 篡改内容 + 改生命周期代次 + 删目录 + 重装」覆盖模块卡 §6.1 项 7 的「安装旧 bundle」路径，判据与错误码不变；item10 的 Flow 根回填类改由真实 PG 用例判卷；item12 WSL 无法按进程改实时钟，注入改为 DB 侧 `expires_at` ±10min（判据仍是「只用数据库时间」）；item11 / 14 以停 TCP 代理等价于停 PG / 网络分区。

**边界**：真实双宿主（A+B 云服务器）、S3 / 对象存储、数据库主从与自动切换、真实模型 / 真实 webhook 均**未测量 / 未授权**；本机双进程不构成跨机证据。

### 6.4 收口前 analyze 复跑（三件套第一项）

2026-11-07 只读复跑，分析对象 = `spec.md` + `plan.md`（现值）+ `tasks.md`（37 勾选 / 2 未勾选）+ `research.md` / `data-model.md` / `contracts/README.md` / `quickstart.md`，对照模块卡 R7（SHA-256 复算一致、3385 行）§2.5 P-01～P-21、§3 交付物、§4 验收映射与宪法十条。完整记录在 [run-state §1.2](run-state.md)「T039 analyze 复跑」，摘要：

| 项 | 读数 |
| --- | --- |
| CRITICAL | **0**（无宪法冲突、无零覆盖 FR、无缺失核心工件） |
| 宪法抽查 | `go.mod` / `go.sum` 与起点空 diff（原则 8）；`0012` 只追加、`SupportedSchemaVersion = 12`（原则 9）；四个有界维护循环未扩第五个（原则 7） |
| 覆盖 | 44 FR / 9 SC / 8 US 与模块卡一致；39 任务与 10 + 2 个 check 一一对上；`required_tests` 98 个全部存在（补两条后 100/100）；模块卡 §3 的 60 条交付物路径全部存在 |
| 发现 1（LOW，已修） | `specs/024-state-distributed/contracts/README.md` 写「5 个错误码」，实际 6 个新业务码（`internal/web/envelope.go:123-128`）；已按当前 US 草案可修订口径改为「6 个新业务码」 |
| 发现 2（LOW，只登记） | 模块卡点名 `assets.TestAssets_InstallResolveAndExecutionUseSamePath`，实现与 stage 用 `TestAssets_InstallResolveAndPinnedReaderUseSamePath`（`internal/assets/bundle_test.go:323`）；语义不变（FR-24-44 三处逐字相等判据齐备，只把断言对象写明「固定读取」），不重写已审模块卡，列 review ② 知悉 |
| 结论 | **CRITICAL 清零，复跑通过**，随即进入 T037 全量门禁 |

`plan.md` 相对 §1.1 冻结值有漂移，已在 run-state 逐条溯源：两处 `Edit` 都是文件落点文字（`flow_http.go` 的 409 映射位置、`internal/cli` 清单补 `worker_config.go`），不改范围 / 权限 / 契约 / 验收语义，属已审范围内派生修正。

### 6.5 七项证据对照（模块卡 §6.3 / G5）

| # | 证据 | 落点 | 读数 / 结论 |
| --- | --- | --- | --- |
| 1 | 自动化门禁 | §1.4 | `make check` 七目标读数见 §1.4（2026-11-08 冻结轮，`RC=0`）；stage 见 §1.4 / §2；`test-pg` 与 `-race` 分属不同命令（`us24-base-race` + 两个 fault check，与 `CGO_ENABLED=0` 的 `build` 目标不同 job） |
| 2 | 多 Worker 行为 | §6.3 | M-01～M-14 逐项转录；**13/14 有当轮本机读数、M-12 未验证**；原始脚本与输出已删除，不可再复核；本轮修复的定向复验见 §8 |
| 3 | 故障矩阵 | §6.3 | 十四项逐项给出「注入方式 / 实测结果 / 是否重放」；数据库中断（停 TCP 代理 14s，M-11）、响应丢失与事件重投（`unknown` + 不重发，M-06 / M-13）、网络分区（M-14）为真实注入；**时钟项 M-12 的注入方式不足以证明宿主时钟偏移，按未验证计** |
| 4 | 审计与诊断 | 见下 | 见下方三段 |
| 5 | 契约、文档与依赖 | §3 / §5 | 契约四件增量见 §3；`git diff --exit-code 138382f -- docs/contracts/schema.pg.sql` **rc=0、0 行**；迁移 `0001`–`0011` 未改、只追加 `0012_distributed_execution.sql`；`git diff 138382f -- go.mod go.sum` **0 行**（§5 第 13 行） |
| 6 | 未验证清单 | run-state §4 | 真实双宿主 / S3 / DB HA / 正式升级工具链 / 真实 webhook / 真实模型逐项标 `未测量` 或 `未授权`；另有两条待 review ② 裁决（§6.1 #2 类措辞差异、判据面修订） |
| 7 | 独立复核 | §6.1 / §8 | 独立 AI 复核 28 条发现全部处置；review ② 首轮打回后 7 项 findings 修复闭环（§8），用户 2026-11-08 授权本轮直接通过（run-state §5），不把机器通过当作人工接受 |

**证据 4（审计与诊断）的三段读数**：

- **终态对账**：被回收 Run 的 `llm_calls` / `tool_invocations` 中仍为 `pending` 的行由 `internal/storage/postgres/recovery.go:393` / `:416` 的**同一条 UPDATE** 归一化为 `unknown` + `error_kind='unknown'` + `recovered_at`，`finished_at` / `duration_ms` 保持 NULL（进程没走到那一步，不编结束时刻）；作用域由 `run_id = ANY($5)` 限定在**本次合法领取的 run_id 集合**，无租约的 pending 行只报告不改写（`TestReclaimer_AuditNormalizationScopedToClaimedRuns`）。Flow 侧同一收敛写 `flow_node_attempts.status='interrupted'`（`TestFlow_RecoverConvergesWholeSubtree`）。租约侧对账依据 `lease_events`。
- **`lease_events` 单一写入方**：生产代码只有 `internal/storage/postgres/lease_store.go` 写该表（`grep -rln 'lease_events' --include='*.go' internal` 的其余命中全是测试），续约失败事件经 `core.RenewFailureRecorder` 回到同一个 `LeaseStore`，没有第二个写入口。
- **没有第二套追加路径**：消息追加只有 `Session.AppendMessage` 一条（`internal/core/session.go:98`），有租约时自动分流到 `FencedAppendMessage`（`:71`），用户输入由 `AgentService` 走同一条（`internal/core/service.go:671`）；`BeginCall` 在 `internal/core` 零命中（§5 第 3 行）；审计 pending 预写与学习抽取三写面全部经 `FencedTx`（FR-24-36），由 `TestFencedWrites_*` 五例守。
- **诊断归属**：`TestDiagnostics_LeaseAttrsWithoutGlobalSequence` 断言 attrs 里只有 `worker_instance_id` + `ownership_epoch`，没有跨节点全局序号；同进程两 Recorder 不构成跨进程证据，跨进程读数由 §6.3 的 M-01～M-14 双进程日志承担。

### 6.6 收口结论

| 项 | 四态 | 依据 |
| --- | --- | --- |
| 宪法口径 `make check` 七目标 | **通过** | §1.4（2026-11-08 源码冻结后 `RC=0`；修复前 2026-11-07 轮次见 §1.1） |
| 全量 base stage（公共 profile + 10 项） | **通过** | §1.4（21/21 passed、`status=passed`、exit 0、`fingerprint=75ca47f3…`） |
| 故障 stage（2 项，`-race`） | **通过** | §1.4（2/2 passed、exit 0，同一 fingerprint） |
| 覆盖率底线 | **通过** | §1.4（六个 ≥70% 包全部达标，最低 `postgres` 72.18%） |
| 本 US 定向 e2e | **通过** | §1.4 / §8.5（`TestFlow_*` 7 例 + `TestBackfill_RestartRecovery` 全部 PASS，包 365.551s；不裸跑全量 e2e） |
| 本机双进程 14 项人工验证 | **未验证（13/14）** | §6.3：13 项有当轮转录读数、M-12 未验证（判据不足）；原始脚本与输出已删除，不可再复核；真实双宿主未测量 |
| 独立 AI 复核 | **通过（28 条全关闭）** | §6.1 |
| 收口前 analyze 复跑 | **通过（CRITICAL 0）** | §6.4 |
| 人工 review ②（US 收口） | **通过（用户授权直通，2026-11-08）** | 2026-11-07 首轮打回后 7 项 findings 全部修复闭环（§8），用户授权本轮直接通过；授权原话见 run-state §5 |
| review ② 定向修复（7 项） | **通过（机器侧，2026-11-08）** | §8：三项已复现缺陷 + 三项高风险静态发现 + 子 Flow 恢复，逐项修改位置与定向证据；最终全量验收读数见 §1.4 / §8.5 |

**review ② 已裁决项（2026-11-07，用户明确）**：

1. §6.2 静态门禁：**零妥协**口径被接受（抑制注释计数 base=198 / tree=198 未变；初版两处自行关闭、一处删除文件的处置知悉）。
2. 模块卡 §6.1 项 8 措辞：**允许明确勘误**与 FR-24-37 / C-13 对齐（§7.4.5），不改实现与任何判据；`Joined` 子任务作用域按 review ② 第 7 项补记勘误二（§7.4.6）。勘误后模块卡现值 `sha256=c6a2658e…`（3411 行；勘误一后中间值 `c5b2413a…`），见 run-state §1.4。
3. §4.1 判据面调整：**接受为架构适配，不代表当前实现被接受**；修复轮新增判据均为加严，逐条见 §8。
4. M-12：**降为未验证**（DB 侧 `expires_at` 注入不足以证明宿主时钟偏移无影响）。

**明确仍未验证**（不因本文件通过而翻转）：真实双宿主（A+B 云服务器）接管延迟、S3 / MinIO / PVC / NFS 跨机资产分发、数据库 HA / 主从切换 / RPO / RTO、真实模型与真实 webhook、正式制品升级 / 回滚工具链——逐项见 run-state §4，归 US-25 / US-17 范围。

## 7. 复跑与交接

```bash
# 0) 环境（WSL，用户 ym；不要用 root——root 的 DAC_OVERRIDE 会让
#    TestUS22_Draft_ReadOnly 失效）
cd /mnt/d/code/basalt

# 1) 宪法口径七目标（make check = fmt lint sec test test-integration cover build）
dev-env/scripts/with-postgres.sh make check

# 2) 全量 stage（公共 profile + us24-base；Windows 需先装配便携 Node 与 DSN）
sh scripts/check-acceptance.sh --stage harness/stages/us24-base.json

# 3) 故障 stage（只跑 stage 专属 check）
sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us24-fault.json

# 4) 本 US 定向 e2e（不裸跑历史外部集合）
dev-env/scripts/with-postgres.sh go test -tags=e2e -count=1 -timeout=30m \
  -run '^(TestFlow_.*|TestBackfill_RestartRecovery)$' ./internal/e2e

# 5) 零新增依赖
git diff 138382f -- go.mod go.sum   # 期望空输出

# 6) 双进程演练（可选，需 WSL root 与独立 PG 库）
#    步骤与判据见模块卡 §6.1（M-01～M-14）；本届演练脚本是临时物，
#    已在收口时删除，按 §6.3 表的注入方式 / 实际结果重做
```

交接说明：`specs/024-state-distributed/` 下 `run-state.md` 记录逐任务证据与失败台账，本文件只收口；review ② 裁决与修复见 §8。正文提到的 `.loop/t039/*`、`.loop/t038/*` 与 `/tmp/us24*` 是过程脚本与探针产物，收口时已按临时物纪律删除；`.loop/runs/*/report.json` 是 harness 运行记录（`.loop/` 整体不进版本库），其判读读数已抄进本文件与 run-state。

## 8. review ② 定向修复（2026-11-07 / 11-08）

用户 2026-11-07 执行 review ② 并打回。本节按 finding 给出「结论 / 修改位置 / 定向证据」。证据目录：reviewer 原始复现材料在 `.loop/reviews/us24-review-20261107/`；本轮复跑反例、逆改探针与恢复后回归在 `.loop/reviews/us24-repair-20261107/`（`.loop/` 整体不进版本库）。修复轮未 commit / push / merge / tag / 部署；零新增第三方坐标；`schema.pg.sql` 冻结未动；零新增抑制注释；未提高超时、未放宽断言 / 覆盖率、未删除测试。

### 8.1 已复现缺陷（三项）

| # | finding | 结论 | 修改位置 | 定向证据 |
| --- | --- | --- | --- | --- |
| 1 | B1 回收与续约竞态：合法续约事务尚未提交时回收器已按扫描快照释放引用 | 已修 | `internal/storage/postgres/reclaimer_store.go:388` `lockLeaseRecheck`：先 `FOR UPDATE` 锁租约行，再用数据库时钟重读 `expires_at` / `released_at`（活性只由这两列决定，SQL **不**重读 owner / 代次），仍活则不动业务行；B2 同路径 `delivery_store.go:284` `lockOrphanDeliveryLeaseRecheck` 复用同一判据 | `app-reviewer-counterexample.log`：复用 reviewer 反例，`reclaim_error=锁定租约行…`、`referenceReleased=false`、`renewedLeaseLive=true`（PASS 3.28s）；定向回归 `app-targeted.log` / `postgres-targeted.log` |
| 2 | 归档补偿覆盖后续停用：恢复屏障无条件回写旧状态 | 已修 | `internal/storage/postgres/resource_store.go:1414` `RestoreLifecycleBarrier` 签名增 `barrierOperationID` + `expectedGeneration`：仅当行仍是**本归档操作**留下的 `disabled` 行（同 operation id、同代次）才写回，`applied=false` 表示已被后续管理操作推进、不覆盖 | reviewer 复现材料 `archive-restore.log` / `archive-restore_test.go.txt`；定向回归 `app-targeted.log` |
| 3 | 停用后的定时任务仍被领取 | 已修 | `internal/storage/postgres/schedule_coord.go:219-229`：`ClaimDueTrigger` 在 `lockTaskRow` 之后复查**最新已提交**的 `task.Enabled`，停用返回 `core.ErrTaskDisabled`（不产生 started 行与租约）；手动 `RunNow` 语义不变 | `postgres-reviewer-counterexample.log`：`claim error=core: task disabled after scan … startedRows=0`（PASS 0.26s）；reviewer 材料 `scheduler-disabled.log` / `scheduler-disabled_test.go.txt` |

### 8.2 高风险静态发现（三项，先查调用链再补最小判据）

| # | finding | 结论 | 修改位置 | 定向证据 |
| --- | --- | --- | --- | --- |
| 4 | 准入回查是否继承新属主 token | **成立**：`LookupAdmittedRun` 只按 run_id 返回持久 token（`run_store.go:639`），若原属主已失权、接管方持有新代次，直接采用该 token 会把别人的执行权继承给本次准入 | `internal/core/service.go:593-616`：回查结果在 `Worker` 非零时要求 `OwnerWorkerID == 本实例` 且 `OwnershipEpoch == 1`（准入代次恒为 1），否则按未知结局失败关闭并 `Warn`；`LookupAdmittedRun` 契约不变，归属判定在调用方 | `core-targeted.log`：`TestAgentService_UnknownAdmissionLookupRefusesForeignLease` 两子例（`foreign_lease_fails_closed` / `own_admission_lease_continues`）PASS；逆改 `inverse-mutation-4-foreign-lease.log` 中性化守卫后逐字转红 |
| 5 | Provider 实际 Key 与凭据 pin 是否错配 | **成立**：目录构建窗口内凭据轮换会让记录的 pin 与客户端实际装载的材料不一致 | `internal/app/providers.go:212-233`：解析 `catalogAPIKey` 前读 `pinBefore`，客户端构造后再读 `pin`；`pin != pinBefore` 返回 `core.ErrCredentialUnavailable`（「凭据在目录构建窗口内换代，拒绝记录与客户端材料不符的构建证据」） | `TestProviderCatalog_CredentialRotatedDuringBuildFailsClosed`（集成编译集）；逆改 `inverse-mutation-5-provider-pin.log`：`An error is expected but got nil`（`providers_test.go:551`）；恢复后 `probes-5-6-restored-green.log` PASS |
| 6 | 准入清单与执行快照是否使用不同 Agent 配置 | **成立**：清单侧取 `entry.Profile`，执行侧重读固定 bundle 的 `AGENT.md`，两者可漂移成「清单记 M1、实际调用 M2」 | `internal/app/resources.go:284`：准入解析时 `checkDefinitionMatchesBundle(entry, bundle)` 逐字节比对，不一致返回 409 `ASSET_VERSION_MISMATCH` / `resourceReasonAsset`；零值情形（无记录）跳过，生产装配下两者恒非空 | `TestAdmission_RejectsWorkCopyDefinitionDriftFromPinnedBundle`（真实 PG）；逆改 `inverse-mutation-6-definition-drift.log`：第 ③ 步 `resolve` 成功、`expected *core.ResourceUnavailableError` 未发生；恢复后 `probes-5-6-restored-green.log` PASS |

### 8.3 子 Flow 恢复（先验证后修）

| # | finding | 结论 | 修改位置 | 定向证据 |
| --- | --- | --- | --- | --- |
| 7 | 子 Flow 恢复：父 subflow attempt 在途是否挡住已完成子 Run 的复用 | **成立**：`Joined` 原按整棵根子树判定，调用方自己在同一轮刚 Claim 的父 subflow attempt 会把子复用自我阻塞（`ErrCapacity` 被 Worker 当竞争错误吞掉，下一轮同样形态，永远推进不了） | `internal/app/flow.go:238-245` `inFlightFor`：根任务走 `SubtreeGate.HasInFlightNodeAttempt(root)`；子任务走 `FlowStore.HasInFlightNodeAttemptUnder(目标任务)`；`internal/storage/postgres/subtree_gate.go:147-176` 递归 CTE（锚点 + 后代，`interrupted` / `unknown` 不计入）。`HasInFlightNodeAttempt(root)` 语义不变 | `TestFlow_SubflowChildCompletedRunReusedUnderRunningParentAttempt` + `TestFlow_TaskScopedInFlightAttemptCoversDescendantAttempts`（真实 PG）；逆改 `inverse-mutation-7-subflow-scope.log`：`flow capacity exhausted` 逐字转红；恢复后 `probe-7-restored-green.log` 两例 PASS |

### 8.4 文档与证据口径

- **模块卡勘误**：`docs/us/us24-state-distributed.md` 两处。勘误一（§7.4.5）：3385 行 `sha256=02e9f941…`（review ① 溯源）→ 3397 行 `sha256=c5b2413a…`，只改「保持 blocked」一词的读法；勘误二（§7.4.6）：把 `Joined` 的作用域对齐 review ② 第 7 项的任务子树实现，现值 3411 行 `sha256=c6a2658e…`。两处都不改 FR / C 编号与判据。
- **M-12 降未验证**：见 §6.3 与 run-state §4（DB 侧 `expires_at` 注入不能证明宿主时钟偏移无影响）。
- **演练原始证据**：2026-11-07 的 `.loop/t038/drill.sh` 与 `/tmp/us24drill/` 已删除，**不可再复核**；§6.3 的读数只作转录。本轮修复的复验只针对受影响项，以 §8.1–§8.3 的定向反例与回归为证据，未重搭整套演练环境。
- **service.go 事故与恢复**：2026-11-07T15:42:34Z 一次探针恢复误用 `git checkout -- internal/core/service.go`，回滚全部 US-24 未提交改动；经会话记录取证（完整 diff 快照 + 四次 `Edit` 原文）按时间顺序重建，`sha256=64ead6ce…` 与事故前保存值逐字相等，build / core 单测 / 定向用例复验通过；完整记录见 `.loop/reviews/us24-repair-20261107/service-go-reconstruction.log`。
- **指纹差异**：2026-11-07 全量报告（`accept-20261107T133336.958949812`）的 `source_fingerprint=845002664a36…` 对应**修复前**的树；本轮修复改了源码与文档，指纹必然变化，因此需要重新跑全量验收（不属「仅文档回填」可豁免的情形）。
- **独立复核（2026-11-08）**：独立 AI 只读复核 7 项修复，**7/7 结论成立**（逐项 VERIFIED），未发现新的代码缺陷；`go.mod` / `go.sum` 空 diff、无新增 `nolint` / `#nosec` / `t.Skip`、六个修复测试文件均为**新增**（未删除既有断言）也经复核。两项文档级观察：① 子任务控制路径（`Cancel` / `Abandon` / `Recover` / `Revise`）复用同一 `Joined` 端口后没有专属用例，复核未能构造失败交错（`Recover` / `Revise` 经 `withRootLease` 持根租约、close 走版本 CAS），记**未覆盖**而非已验证；② 模块卡 C-14 仍把 `Joined` 写成整棵根子树，与 §8.3 第 7 项的任务作用域存在描述差，已在模块卡 §7.4.5 追加「勘误二」。
- **§8.1 表述校正（2026-11-08）**：第 1 行原写「重读 owner / 代次 / `expires_at` / `released_at`」；实读 `reclaimer_store.go:393-408`，SQL 只重读后两列（活性判定不需要 owner / 代次），已按实际语句改写，判据与结论不变。

### 8.5 最终验收读数（源码冻结后回填）

2026-11-08 在源码冻结状态下单列跑完四轮，读数与逐 check 明细见 §1.4。摘要：

| 轮次 | 结果 | 证据 |
| --- | --- | --- |
| `with-postgres.sh make check` | 退出 0，七目标全绿 | `.loop/reviews/us24-repair-20261107/final-make-check.log`（00:18:14–00:55:48） |
| base stage | 21/21 passed，`status=passed`、`exit_code=0` | `.loop/runs/accept-20261107T165607.936093205/report.json` |
| fault stage | 2/2 passed，`status=passed`、`exit_code=0` | `.loop/runs/accept-20261107T173936.251595917/report.json` |
| 定向 e2e | 退出 0，`ok …/internal/e2e 365.551s` | `.loop/reviews/us24-repair-20261107/final-e2e.log` |

- **指纹**：两轮 stage 均为 `75ca47f3a9f469de281d22d36bb75f67277ddd427f97d6fcc56600f5d2d302bd`（2176 个文件），`base_commit=138382f`、`dirty=true`。修复前 2026-11-07 报告是 `845002664a36…`，本轮改了源码，属「必须重跑」而不是「仅文档回填」。
- **回填后差异**：本节、§1.4 与文件头状态段的回填只改 `specs/024-state-distributed/acceptance.md`（`run-state.md` 不计入源码指纹）；回填完成后用与 `scripts/acceptance/fingerprint.go` 同一算法的取证工具复算指纹并输出逐文件 SHA-256 清单，与冻结时清单逐个 diff，差异必须只落在 `acceptance.md` 一行，结果与命令记录在 run-state §5。
- **送审口径**：机器侧以该轮为送审依据；M-12 仍为未验证，真实双宿主 / S3 / DB HA / 真实模型与 webhook 仍归 US-25 / US-17（§6.3、run-state §4），不因本轮全绿翻转。

### 8.6 收口后 CI 修复：累计 stage 的 superseded_checks（2026-11-08）

合入 main 后的 CI（run 42399360393，job `test` / 128483577470）在 step「Lint stage definitions (read-only existence precheck)」判红：`harness/stages/current.json` 的四个历史 check 仍按名索取被本 US 批准范围改写掉的测试，共 28 行 ERROR（21 + 3 + 1 + 3），其余 job 全绿。`make check` 不含 `--lint-stages`，因此七目标全绿不能发现这类失配；覆盖它的是 CI 的 stage lint step 与手工 `--lint-stages`（同一个只读检查）。

按 [开发操作](../../docs/DevelopmentLoop.md) §9.3 的既有机制处理：历史 stage 文件零改写（仍按 SHA-256 冻结），失配以 `superseded_checks` 显式登记，替代 check 的 `required_tests` 由 `scripts/acceptance/stages_test.go` 的 `pinnedRequiredTests` 逐字钉住，argv / 超时由 `pinnedStageChecks` 钉住。逐条承接如下（**只更名等价项或改 Approved 删除行为的承接断言，无一项是删断言转绿**）：

| 历史 check | 现行 check | 失配来源（模块卡依据） | 承接方式 |
| --- | --- | --- | --- |
| `010-core-scheduler`（46 项） | `010-core-scheduler-distributed`（47 项） | `Fire_*` → `Dispatch_*` 更名；`StartRecoversExecutions` 由 FR-24-02「启动不得全局扫描」删除；`StartRegistersEnabledOnly` / `SetEnabled_TogglesRegistration` 由注册模型改为 `SetRegistered`；`Start_TaskAddFailureStopsEngine` 随进程内 cron 生命周期移除；`OldCallbackAfterReconcile_NoRun` 随 cron 回调移除；`ReconcileAgent_AddFailureReturnsError` 改为 `ApplyIsAllOrNothing` 同一判据 | `Fire_*` → `Dispatch_*` 及其余两处同义改名（`StartReconcilesDefinitions` → `Start_ReconcilesDefinitions`、`Start_SecondAgentApplyFails_NoHandles` → `Start_SecondAgentApplyFails_NoScanStart`）逐条承接；删除项换为负向断言 `Start_DoesNotRecoverDeliveriesGlobally`、`Dispatch_SkipsRetiredAndUnregistered`、`SetEnabled_ControlsDispatch`、`Start_ApplyFailureIsNotLatched`、`AfterReconcile_NextDispatchUsesLatestDefinition`、`ReconcileAgent_ApplyIsAllOrNothing`；另补 `Dispatch_MissedGraceRecordsOneSkip`（FR-24-12），净增 1 项 |
| `010-cron-adapter`（9 项） | `010-cron-adapter-parser`（7 项） | `TestEngine_AddAndStop` / `TestEngine_StopWaitsForJobs` 随 cron 生命周期移除（适配器收敛为纯解析器，模块卡 §3）；`ParsesSixFieldWithCRONTZ` 配套更名 `NextParsesSixFieldWithCRONTZ` | 保留 7 项，其中 6 项逐字同名、1 项按更名承接 |
| `010-storage-tasks`（15 项） | `010-storage-tasks-distributed`（15 项） | `TestTaskStore_RecoverStartedExecutions` 由 FR-24-02 删除 | 换为负向断言 `TestTaskStore_NoGlobalRecoveryScan`，其余 14 项逐字保留 |
| `010-race`（14 项） | `010-race-distributed`（14 项） | 三个更名项同上 | `Fire_LockMissRecordsSkipped` → `Dispatch_BusyClaimRecordsSkipped`、`Fire_ConcurrentDistinctTasks` → `Dispatch_ConcurrentDistinctTasks`、`OldCallbackAfterReconcile_NoRun` → `AfterReconcile_NextDispatchUsesLatestDefinition`，其余 11 项逐字保留 |

- **判据面变更登记**：这是判据面变更（累计 stage 的 check ID 改名与 required_tests 换项），来源是已审模块卡 §3「`scheduler_test.go`（改写）」与 FR-24-02 / FR-24-17 的范围改写，不是为转绿放水。净变化：46→47（更名项 1:1 承接，删除项换成负向 / 新语义断言，另补 1 项 FR-24-12 用例）、9→7（2 项随 cron 生命周期移除，无同名承接；触发时间线改由 `us24-base` 的调度与回收用例覆盖）、15→15（1 项换成负向断言）、14→14（3 项更名承接）。四个替代项全部纳入 `pinnedStageChecks` 的 argv / 超时 / 层级钉住。
- **验证读数**：`go test ./scripts/acceptance -run '^TestStages_'` 通过；WSL 上 `sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/current.json` → `stage lint: ok`，`ERROR` 零行（`current.json` 是本轮唯一 fail stage，其全部 `required_tests` 均存在）。同一条命令共打印 **509 行 WARN**、分布在 8 个历史 stage 文件（`us12` 309 / `us11` 72 / `us10` 53 / `us14-release` 31 / `us14-14d` 27 / `us9-web` 8 / `us16a` 6 / `us8-notify-r1` 3）：lint 会遍历 `harness/stages/*.json`，对**非 fail stage** 的每个缺失项只告警；历史 stage 按冻结口径不随本 US 改写，因此其中 `us10-scheduler.json` 的 28 行正是本轮改名 / 删除的测试名，其余 WARN 属历史 drift（含带 `//go:build integration` 的测试在无 `-tags=integration` 的旧 check 下不可列）。WARN 不影响退出码，但**不得**据此宣称「只有个别历史 stage 漂移」。
- **本地门禁**：`dev-env/scripts/with-postgres.sh make check`（WSL）`MAKE_CHECK2_EXIT=0`，七目标全绿（`check-format` 干净；`vet -tags=integration` / `staticcheck` / `gosec` / `govulncheck` 通过；`test` / `test-integration` 零 `FAIL`；`check-coverage: 全部包达标`；`CGO_ENABLED=0` 构建成功），详见 run-state §5。
- **未验证**：完整累计 stage 的执行与远端 CI 由推送后那轮 CI 复验，读数回填到 run-state §5。

### 8.7 收口后本地 -race 复验发现的产品竞态：调度注册集合的读未持锁（2026-11-08）

为复验 §8.6 的 `010-race-distributed`，本地按该 check 的 argv 在 WSL + PostgreSQL 上跑 `go test -race -tags=integration`（七个包）。`internal/app` 判红（430.380s），三个用例报 `race detected during execution of test`：`TestAgentArchive_RollbackRestoresRetirement`、`TestAgentRestore_DoesNotAutoEnableSchedules`、`TestRun_ServeStartsConfigWatcher`。

竞态对（同一张 `s.registry` map）：

```text
Read  (dispatch loop)  Scheduler.registeredAgentNames()   scheduler.go:1295
                       ← dueTasks() ← dispatchOnce() ← dispatchLoop() ← Start.gowrap2
Write (admin archive)  Scheduler.SetRegistered()          scheduler.go:1423 (delete)
                       ← scheduleEditorAdapter.SetRegistered ← TestAgentRestore_…
```

根因：`SetRegistered` 按 `stateMu` 写锁增删注册集合，而本 US 新增的到期扫描在 `dueTasks` 里**不持锁**直接遍历同一张 map；管理读面的 `visible()`（`List` / `Executions` / `SetEnabled` / `RunNow` 共用）同样不持锁读该 map，属同一类缺陷。这不是测试夹具泄漏——归档屏障在派发循环运行时正是生产路径（FR-24-41 准入屏障 + 运行期 `SetRegistered(false)`）。

- **为什么此前没暴露**：`make check` 的 `test` 目标虽有 `-race`，但不带 `-tags=integration`，归档 / 恢复类集成用例不在编译集；唯一覆盖组合是 stage check `010-race-distributed`（`-race` + `-tags=integration` + 七个包），而它在 §8.6 那轮 CI 中因 lint 先红未被执行。
- **修复**（`internal/core/scheduler.go`，仅加锁与快照，无行为改写）：
  - `dueTasks` 改用既有的 `registeredSnapshot()`（`stateMu.RLock` 内复制）再对新引入的 `sortedRegisteredNames` 排序，扫描路径不再触碰共享 map；
  - 原 `registeredAgentNames` 更名 `registeredAgentNamesLocked`，注释写明「调用方必须持有 `stateMu`」，唯一调用方 `Start` 在写锁内（`scheduler.go:310`）；
  - `visible()` 读 `s.registry` 前取 `stateMu.RLock()`（其四个调用方均不持锁，无重入死锁）。
- **判据面判定**：只改产品代码，未新增 / 删除 / 放宽任何断言、未提高超时、未加抑制注释；竞态检测器本身就是判据，用例在修复前红、修复后绿即为证据。
- **验证读数**：
  - 修复前：上述三例 `— FAIL` + `race detected`（本轮 WSL `-race -tags=integration`，`internal/app` FAIL 430.380s）；
  - 修复后定向复跑：`go test -race -tags=integration ./internal/app -run 'TestAgentArchive_RollbackRestoresRetirement|TestAgentRestore_DoesNotAutoEnableSchedules|TestRun_ServeStartsConfigWatcher'` → `ok … 5.226s`；
  - 修复后全量七包：`core 4.071s / cron 1.018s / storage 43.498s / workspace 17.414s / tool 20.551s / web 18.682s / app 387.446s`，全部 `ok`，日志内 `WARNING: DATA RACE` 零行，`RACE2_EXIT=0`。
- **未验证**：远端 CI 对该修复的复验读数在推送后回填 run-state §5；竞态检测依赖调度交错，本地单轮通过不构成「永不发生」的证明，但修复把所有访问点都置于同一把锁下，属结构性消除而非概率降低。

### 8.8 收口后 CI 修复 ②：e2e 夹具身份与调度游标重排（2026-11-08）

CI run 42409706693（提交 `9c01b2e`）step 9「Current cumulative stage」在 check `015-backfill` 判红，失败点是 `TestBackfill_Scheduler/archive_meets_cron`。修复过程中定位到两个独立缺陷（一个夹具、一个产品），逐项如下。

**缺陷一：夹具在「编辑 AGENT.md」时更换了 Agent 身份（`internal/e2e/harness_test.go`）**

- 失败读数：`scheduler_test.go:678: 等待文件出现 … 超时（1m0s）`；Stage B 进程日志只有 `Agent demo 的共享发布记录缺失或已删除`（`internal/storage/postgres/run_store.go:354` 的 `recheckAgentBundle`，503 `ASSET_UNAVAILABLE`）。
- 链路：`archive_meets_cron` 在 Stage B 用同一 `active` / 代次 2 重写 `AGENT.md` → 夹具 `writeLifecycleState` 再铸造一个新 `AgentUID` → 启动基线按「同代次 2 的内容与索引不一致」（`ErrAgentLifecycleMismatch`）只 WARN、索引保留 Stage A 身份 → `prepareHeavy` 的 `basalt assets install --agent` 按**索引** UID 发布 bundle → `BuildRunResourceManifest` 用**状态文件** UID → `StartRun` 的事务复查查不到 `resource_pointers(kind=agent_bundle, resource_id=状态文件 UID)` → 503。
- 归因：产品行为符合 FR-24-25（bundle 与共享发布记录不一致时 fail-closed）与 FR-24-42（代次一致写入）；缺陷在夹具把真实系统里「编辑 `AGENT.md` 不触碰生命周期状态、身份不随工作区编辑变化」建模成了「换身份」。
- 修复：`writeLifecycleState` 在状态与代次都没变时保持原文件字节、复用原 `AgentUID` 与原 `OperationID`（操作行补种 `ON CONFLICT DO NOTHING`）；只有文件不存在 / 不可解析时才铸造新身份。跨阶段身份稳定后，Stage B 的 `assets install` 按内容哈希发布新版本、指针 CAS 到新版本，准入与 `StartRun` 复查锁同一 UID。

**缺陷二：cron / zone 变更不重排 `next_fire_at`（`internal/storage/postgres/task_store.go`）**

- 失败读数：夹具修好后同一子例改在 `scheduler_test.go:688` 超时（`等待输出 "会话已归档，本轮跳过" 超时（1m0s）`）；Stage B 日志有 `trigger=manual`、无任何 `trigger=cron` 派发。
- 链路：Stage A 的任务行在首轮到期扫描时按 `farFutureCron()` 初始化 `next_fire_at`；Stage B 启动对账把 cron 改成 `now+15s`，但 `ApplyAgentTasks` 的 `ON CONFLICT DO UPDATE` 只写定义列，`next_fire_at` 保持远期值 → `ListDispatchable`（`next_fire_at IS NULL OR next_fire_at <= db_now`）永远不选中该行 → 新 cron 不生效。
- 归因：US-24 把触发权威从进程内 cron 句柄改为数据库游标；US-10 时代每次对账重注册句柄会按新表达式从当前时刻重算，迁移时漏掉了「表达式变化 → 重排游标」。属**产品回归**（不是夹具问题）：任何通过编辑 `AGENT.md` 修改 cron / zone 的部署都会受影响。
- 修复：`DO UPDATE` 增加 `next_fire_at = CASE WHEN scheduled_tasks.cron IS DISTINCT FROM excluded.cron OR scheduled_tasks.zone IS DISTINCT FROM excluded.zone THEN NULL ELSE scheduled_tasks.next_fire_at END`。置 NULL 后由下一轮到期扫描走既有「`next_fire_at IS NULL` → 初始化」分支（`internal/core/scheduler.go` 的 `dispatchOnce`：只 `SetNextFire` 并 `continue`，不执行），与模块卡「`next_fire_at` 由注册 / 推进时计算」一致；只在表达式本身变化时重排，仅改 message / deliver_to 时保留游标（避免对账把临近的触发点跳过）。
- 回归用例：`TestApplyAgentTasks_ScheduleChangeRearmsNextFire`（`internal/storage/postgres/task_store_port_test.go`）三子例：cron 变化清空游标、zone 变化清空游标、仅 message 变化保留原值。

**判据面**：零删除 / 零放宽——不删断言、不提高超时、不加抑制注释；新增一个用例；`make check` 的覆盖率与门禁口径不变。

**验证读数**：

| 项 | 读数 |
| --- | --- |
| 逆改（`HEAD` 版 SQL 经仓库外 `-overlay` 探针） | `cron_change` / `zone_change` 逐字 FAIL（`Should be false`），`message_only` PASS；探针 `/tmp/us24rearm` 用毕即删 |
| `TestBackfill_Scheduler/archive_meets_cron` | PASS 15.57s（修复前 61.27s 超时） |
| `TestBackfill_Scheduler` 全套 | 10/10 PASS，包 55.86s |
| CI step 9 同款 `015-backfill` 正则（11 个 `TestBackfill_*`） | RC=0，115 pass / 0 fail，包 130.554s；`.loop/reviews/us24-repair-20261107/backfill-20261108.json` |
| 本地七目标 `make check` | `MAKE_CHECK_EXIT=0`，七目标全绿：format 干净 / vet（`-tags=integration`）+ staticcheck + gosec + govulncheck 通过 / `test` 与 `test-integration` 全包 `ok` 零 `FAIL` / `check-coverage: 全部包达标`（postgres 72.18%）/ `CGO_ENABLED=0` 构建成功；日志 `.loop/reviews/us24-repair-20261107/make-check-20261108.log` |

**未验证**：远端 CI 对修复提交的读数在推送后回填（不把运行中写成通过）；完整累计 stage 的远端执行以 CI 为准，本地只复跑了失败的 check 与 scheduler 套件。
