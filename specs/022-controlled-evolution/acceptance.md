# US22 验收报告

日期：2026-11-06。状态：AI 自动验收完成，**人工 review ② 已通过**（用户 2026-11-06 决定，见 §6.4）。分支 `022-controlled-evolution`，基线 commit `54212c3f6041c05240edc1a743aab76587a70f1f`（US-21 收尾），工作树 `dirty=true`（US-22 全部交付未提交，属本 US 交付面）。`make check` 最终轮与全量 stage 读数见 §1.1；失败历史保留在 §1.2 与 [run-state](run-state.md) §2，不用后续通过覆盖更早的失败。效果类结论一律 **`未测量`**。本报告定稿时未 commit、push、merge、tag、发布或部署；提交与合回由用户在该决定中授权执行。

## 1. 门禁与取证基线

### 1.1 本轮读数（T032 最终轮 `make check` + 全量 stage）

> 第九轮的 `make check` 与第 10 次全量 stage 读数已被后续两批夹具余量改动（§4.1 批次 ①②）覆盖，**不再作为本轮证据**；其存在、时间与处置见 §1.2 第 7～10 行。以下填最终一轮的当场读数，回填前保持占位。

**A. `make check`（最终轮，2026-11-06）**

终端日志 `.loop/t032/make-check-17.log`（16:22 完成；此后产品代码、测试、stage 与契约未再变更，指纹见 §1.1-B）。

| 目标 | 四态 | 本轮读数 |
| --- | --- | --- |
| fmt（`sh scripts/check-format.sh`） | 通过 | `gofmt / gofumpt 均干净`，exit 0 |
| lint（`go vet -tags=integration` + `staticcheck -tags=integration` 固定 `-checks`） | 通过 | 零诊断，exit 0 |
| sec（`gosec -quiet` + `govulncheck`） | 通过 | `No vulnerabilities found`；代码与导入包受影响漏洞 0，依赖模块内 2 条未被调用 |
| test（`go test -race -cover ./...`） | 通过 | 38 条 `ok`、2 条 `?`（无测试文件）、**0 FAIL / 0 SKIP** |
| test-integration（`go test -tags=integration -count=1 -timeout=20m ./...`） | 通过 | 42 条 `ok`、2 条 `?`、**0 FAIL / 0 SKIP**；`internal/app 717.656s` |
| cover（`sh scripts/check-coverage.sh`） | 通过 | `check-coverage: 全部包达标`；`core 78.99%`、`memory 86.01%`、`storage 73.90%`、`storage/postgres 74.05%`、`tool 77.52%`（底线 70%），`app 74.87%`、`web 85.46%`、`flow 61.93%`（底线 50%）；`internal/e2e`、`scripts` 因无可执行语句按规则 skip |
| build（`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=…" -o bin/basalt.exe ./cmd/basalt`） | 通过 | exit 0；产物 `bin/basalt.exe` sha256 `b379ec95…`（附录 B） |
| **汇总** | **通过** | `MAKE_CHECK_EXIT=0`（七目标全绿） |

**B. 全量 stage（`harness/stages/us22-base.json`，full profile + 12 check，2026-11-06 18:50–19:10）**

| 项 | 读数 |
| --- | --- |
| run_id / 报告 | `accept-20261106T105018.044921100` / `.loop/runs/accept-20261106T105018.044921100/report.json` |
| 总体 | **`status=passed` / `exit=0`；23 / 23 check 通过** |
| scope / 环境 | `full/us22-base`；`os=windows`、`arch=amd64`、`go_version=go1.27.3`、`ci=false` |
| 源码指纹 | `1203e134ba64f181b9f7fed927863466cbaea0b86a0e4f3dcdb45cdd54815a49` |
| 基 commit / dirty | `54212c3f6041c05240edc1a743aab76587a70f1f` / `true`（US-22 交付未提交） |
| `tested_binary_sha256` | `c250e3c67e8fe73228d7e4239d0253ecc31ccd28dc927d0e330095d399c2ce6e`（`us22-base-e2e.log` 的 `binary_sha256` 标记；e2e 用例自建候选二进制，非 `bin/basalt.exe`） |

| 命令/检查 | 四态 | 本轮读数 | 证据 |
| --- | --- | --- | --- |
| fmt | 通过 | 3.7 s / exit 0 | fmt.log |
| lint | 通过 | 29.7 s / exit 0 | lint.log |
| sec | 通过 | 30.0 s / exit 0；`No vulnerabilities found` | sec.log |
| test（race） | 通过 | 148.0 s / exit 0 | test.log |
| test-integration | 通过 | 348.8 s / exit 0；`internal/app 337.975s` | test-integration.log |
| test-pg（39 个必跑用例） | 通过 | 216.9 s / exit 0 | test-pg.log |
| cover | 通过 | 364.0 s / exit 0；`internal/app 340.404s / 74.9%`，全部有语句包达标 | cover.log |
| build（`CGO_ENABLED=0`） | 通过 | 0.7 s / exit 0；产物 `bin/basalt.exe` | build.log |
| eval-build / behavior / injection | 通过 | 1.9 / 2.4 / 1.9 s，均 exit 0 | 同名 log |
| us22-base-core | 通过 | 8 必跑 / 15 通过，1.1 s | 附录 A |
| us22-base-memory | 通过 | 69 必跑 / 144 通过，2.5 s | 附录 A |
| us22-base-provider | 通过 | 11 必跑 / 12 通过，1.1 s | 附录 A |
| us22-base-app | 通过 | 60 必跑 / 75 通过，13.6 s | 附录 A |
| us22-base-pg | 通过 | 39 必跑 / 66 通过，14.7 s | 附录 A |
| us22-base-web | 通过 | 11 必跑 / 55 通过，1.3 s | 附录 A |
| us22-base-cli | 通过 | 6 必跑 / 8 通过，1.4 s | 附录 A |
| us22-base-e2e | 通过 | 6 必跑 / 6 通过，11.2 s | 附录 A |
| us22-base-console-test | 通过 | 前端 Vitest，14.2 s / exit 0 | console-test.log |
| us22-base-console-build | 通过 | 构建 + `integrity.json` 4 个文件，8.0 s / exit 0 | console-build.log |
| us22-base-race | 通过 | 6 必跑 / 6 通过，6.6 s | 附录 A |
| us22-base-memory-race | 通过 | 1 必跑 / 1 通过，3.0 s | 附录 A |
| **汇总** | **通过** | 23 / 23，`ACCEPTANCE_EXIT=0` | report.json |

### 1.2 失败历史（保留，不作通过证据）

失败预算默认 8 次，**已用 15 次（预算已用尽）**（[run-state](run-state.md) §3），逐条台账见 [run-state](run-state.md) §2。计入预算的门禁失败：

| 轮次 | 目标 | 首错 | 分类与处置 |
| --- | --- | --- | --- |
| 1 | lint | `learning_store_test.go:67` `func lsSeedMessage is unused (U1000)` | 缺陷：辅助函数无调用方 → 删除；复跑 |
| 2 | sec | 9 处 gosec（G202×2 / G304×1 / G301×1 / G302×3 / G306×2） | 缺陷 + 已审设计取舍：按语义修代码；6 处保持「已审设计」的抑制逐条写理由（见 §6） |
| 3 | test-integration | 两条前序版本钉（表数 82 / 版本 10） | 缺陷：迁移 v11 后的判据面适配 → 82→87、10→11（见 §4.1） |
| 6 | cover | `internal/app` 8 条用例「测试库不可达」/「删除隔离 schema 超时」 | 环境：开发容器 crash recovery 停机 + 一次性 PG 抖动 → 换一次性 PG 复跑 |
| 7 | test-integration | `TestFlow_ContextKeepsHardConstraintsAndFailsClosed/revision`（`flow checkpoint unsupported`）与 `TestFlow_HTTPResumeIdempotentFactBeforeCurrentVersion`（3 s 客户端超时） | 环境 / 时序：两条用例及其被测代码均未被本 US 改动；同树隔离合并定向 2 PASS、各自 `-count=10` 共 20/20 PASS；**不改断言、不放宽 3 s 超时**；在未变更的树上重跑第八轮。根因未确认，候选机制记于台账 |
| 8 | cover | `internal/app` 5 条用例 ~31.9 s 停在 `获取夹具迁移槽位: 取槽位连接: … wsarecv: An existing connection was forcibly closed`（`127.0.0.1:55434`） | 环境：容器未重启、`max_connections=100` 仅 7 条在用、零 FATAL / OOM；同一时刻 PG 日志 5 条 `invalid length of startup packet`（启动包未送达即断开），55434 上 1105 个 TIME_WAIT——判定为 WSL2 NAT localhost 转发在高 churn 下丢连接（假设，根因未确认）。**不改测试 / 判据**：一次性 PG 重建为发布 `0.0.0.0:55434`、`max_connections=300`，DSN 改 WSL 网卡 IP `172.19.88.190` 直连，重跑第九轮 |
| 9 | cover（stage 第 10 次） | `internal/provider` `TestProviderService_CancelDuringResponseReadRecordsTimeoutKind`（`service_test.go:681` 期望「未取完响应」实得「未取得响应」） | 环境 / 时序：该用例与被测 `client.go` 均未被本 US 改动；隔离 `-count=10` → 10/10 PASS，cover 模式 5/5 PASS；判定为 `readPhaseTrigger=500 ms` 余量在并行争用下被吃掉。**按用户裁决加长余量**（500 ms → 5 s），不改断言（见 §4.1） |
| 10 | cover（余量批次 ① 后的 `make check`） | `internal/app` `TestFlow_DefaultCancelAndShutdownClocksKeepResourcesUntilJoin`（43.47 s，`flow_runtime_integration_test.go:110` `original leaf did not join after release`，6 s join 看门狗） | 环境 / 时序（同族第 2 例）：该文件未被本 US 改动；按用户裁决把该文件七处看门狗余量 3 s / 6 s / 7 s → 15 s / 30 s / 20 s，**三条行为断言（`[30s, 32s)`、`≥35s`）与 50 ms 负向断言逐字保留**（见 §4.1） |
| 11 | cover（余量批次 ② 后的 `make check`） | `internal/app` 两条：`TestFlow_DefaultCancelAndShutdownClocksKeepResourcesUntilJoin`（`flow_runtime_integration_test.go:89` `"32.5669914s" is not less than "32s"`）与 `TestFlow_ProductionSourceVerificationAndClaimProjection/support-temporarily-unavailable`（`flow_support_integration_test.go:145` `flow dispatch rejected`） | 同轮 `test` / `test-integration` 在同一棵树上均通过。① 是**行为上界**（30 s 容忍期满后的落库 / 审计在并行 + cover 插桩下超 2 s 松弛）→ 用户裁决放宽到 40 s，下界逐字不动；② 隔离 `-count=5 -cover` 5/5 PASS，判定负载敏感，**不改测试**（详见 §4.1） |
| 12 | cover（第 12 轮 `make check`） | `internal/flow` `TestFlow_CoordinatorOriginalLedgerLifecycle/chain`（`coordinator_integration_test.go:233` 期望 `succeeded` 实得 `blocked`，3.65 s）；同轮 `internal/app` 三目标全绿，第 11 轮两条未复现 | 环境 / 时序：日志前置的 `rule_id=sources` WARN 来自同包其它用例的故意失败路径，该夹具未装配 Sources / Support，不是根因。链式用例唯一可达 `blocked` 的路径是叶子返回错误（`acceptance.go:71` `leaf_execution_unconfirmed`），夹具内唯一的固定预算是 `FinishRun` 1 s 上限；已用 1 ns 上限**诱导红态**复现逐字相同症状。上限 1 s → 15 s（断言逐字不动），同族夹具 `flow_advance_integration_test.go:43` 同步，并给状态断言补 `reason=%q` 失败信息（详见 §4.1） |
| 14 | test-integration（第 14 轮 `make check`，F-01 修复后） | `internal/app` `TestFlow_DiagnosticsDoNotAdvanceTask`（`flow_observation_integration_test.go:27` `Condition never satisfied`，5.98 s）与 `TestMCPApply_OldRunSnapshotUnchanged`（`pgtest.go:149` 删除隔离 schema `timeout: context deadline exceeded`，16.40 s）；同轮 `test` 通过（`internal/app ok 97.439s`） | 环境 / 时序（同族第 3 批）：两个测试文件与 `pgtest.go` 均未被本 US 改动；隔离复跑 flow `-count=10` 10/10 PASS（每条 0.83–1.28 s）、MCP `-count=5` 5/5 PASS（每条 0.38–1.47 s）；仓库外 `-overlay` 逆改预算（5 s→50 ms、DROP 10 s→30 ms）复现逐字相同失败文案。按用户裁决只放宽上限：5 s→20 s、10 s→30 s，断言逐字不动（详见 §4.1 批次 ③） |
| 15 | cover（第 15 轮 `make check`） | `internal/app` `TestKnowledgeRunE2E_AnswerCitesInjectedChunk`（`knowledge_run_integration_test.go:583`）连接 `172.19.88.190:55434` 报 `connectex: Only one usage of each socket address` | 环境：主机本地临时端口瞬时分配失败，非连接泄漏、非服务端耗尽（隔离 `-count=5` 通过；260 点采样峰值 TIME_WAIT 1,059，约 15,700 端口；established ≤32）。**不改测试 / 判据**，第 17 轮通过（详见 §7.1 第 2 条） |
| — | 全量 stage 第 11 次 | `test-integration` / `test-pg` / `cover` 全部失败，PG 报 `dial error: timeout: context deadline exceeded`；运行中途 WSL VM 停机 | 环境：WSL2 空闲停机导致容器内 PG 被杀（`wsl -l -v` 为 Stopped、PG 日志 `received fast shutdown request`）。改用常驻 WSL 会话 + bind mount 全新数据目录 |
| — | 全量 stage 第 12 次 | `test-integration` 失败（1,216,750 ms），PG 全程 `actively refused`；同一时刻 **C: 100% 满（仅剩 314 MB）** | 环境：WSL 的 `ext4.vhdx` 涨到 213 GB 写满 C: → vhdx 无法分配新块 → 文件系统 EIO → PG 被杀、dockerd 卷存储锁死。`go clean -cache` 释放 58 GB，发行版整体迁到 `D:\WSL\Ubuntu`；stage 第 13 次（最终轮）全绿 |

第 4、5 轮为源码变更后的主动停止（不计失败）。台账另有 20 余条任务级 / 环境级记录（开发容器 57P03 停机、tmpfs 写满 53100、`-overlay` 探针盘符大小写、stage `protect` 含已删除文件、§4.2 四个 app 用例无归属任务、T031 假模型响应体格式等），均逐条记录假设与处置。

收口前独立复核三轮另发现 **F-01（HIGH，产品缺陷）**：`FailExtraction` 对非 `pending` 行一律空操作，而 `window_gone` 只出现在重试路径（行仍是上一次尝试的 `failed`），终态转移被吞——行永久停留 `failed`、attempts 不增、`finished_at` 为空、游标不推进，同来源新窗口永久阻断（C-33 症状逐字命中）；日志却打印 `status=skipped`，与库相反。该缺陷在全绿的门禁与 stage 下存在，因为仓储用例把顺序写反（先 `BeginExtraction` 翻回 `pending`）掩盖了它（F-02）。F-01/F-02/F-03/F-04 均为复核发现，**不是门禁失败，不计入失败预算**（发现时计数为 11/8；此后第 15 轮与两次 stage 环境作废另计，最终累计 15/8，见 [run-state](run-state.md) §3）；发现时第 13 轮 `make check` 正在运行，按「已知问题收敛后再跑完整门禁」主动中止（日志不完整，不作通过 / 失败证据），修复后重跑最终轮。

### 1.3 基线与环境

| 项 | 值 |
| --- | --- |
| 分支 / 基线 | `022-controlled-evolution` / `54212c3f6041c05240edc1a743aab76587a70f1f` |
| 模块卡 | `docs/us/us22-controlled-evolution.md` R6 已审字节 SHA-256 `289b5e70712974002d9addd9ab63893144ff7c6de332de73d6a1dd95c41ddfe9`（2039 行）；当前文件（含批准横幅）`449d58f9b1348966a34186c0c6727b7a0956d77c4cbf33b0e807e4fcadb9d14a`（2042 行） |
| 工具链 | go1.27.3（`toolchain` 固定；基础安装 1.27.2 不是构建版本）；`GOPROXY=https://goproxy.cn`；`-race` 用 `C:\msys64\ucrt64\bin` 的 gcc |
| 测试 PG | 一次性容器 `basalt-us22-pg`（`pgvector/pgvector:0.8.7-pg17-bookworm`，发布 `0.0.0.0:55434`、`max_connections=300`，trust，db `basalt_test` 预装 `vector` 0.8.7 与 `pg_trgm`）；DSN 走 WSL 网卡 IP `172.19.88.190:55434` 直连，绕过 localhost 转发（第八轮丢连接后改，见 §1.2）；开发容器 `basalt-dev-postgres` 处于 crash-recovery 反复重启，**未触碰** |
| 前端 | 便携 Node 24.22.0（stage console-test / console-build 用），`make check` 不依赖 Node |
| 依赖 | `git diff go.mod go.sum assets/console/package.json` 为空（零新增 / 零版本变更） |

## 2. Harness 与逆改判别力

`harness/stages/us22-base.json`（`us22-base` v1，批准依据见文件 `approval` 字段）共 12 个 check、`required_tests` **217 条 / 210 个唯一名**、`protect` **70** 条路径：

| check | 必跑条目 | 覆盖 |
| --- | --- | --- |
| `us22-base-core` | 8 | `core` 钩子与哨兵 |
| `us22-base-memory` | 69 | 输出解析、校验、窗口、时延、草案、端口契约 |
| `us22-base-provider` | 11 | 抽取单次协议调用、预写、路由器不可抽取 |
| `us22-base-app` | 60 | 装配、单次触发、确认门禁、恢复、只读端口 |
| `us22-base-pg` | 39 | 迁移、仓储、闭包复核、并发、预算 |
| `us22-base-web` | 11 | 五 GET、无写端点、契约 |
| `us22-base-cli` | 6 | 五个只读命令 + handoff |
| `us22-base-e2e` | 6 | 六条真实二进制主链 |
| `us22-base-console-test` | 0 | 前端 7 组 `TestUS22_Console_*`（见 F-07 限制） |
| `us22-base-console-build` | 0 | 构建与 `integrity.json` |
| `us22-base-race` | 6 | 抽取 / 钩子并发与泄漏 |
| `us22-base-memory-race` | 1 | 内存端口并发 |

逐 check 的本轮读数见附录 A。全仓另有 221 个 `TestUS22_*` 测试函数定义（含跨包同名）；stage 外的用例随各自包在 `make check` 的 `test` / `test-integration` 中一并执行。

**关键缺陷的红态探针**（仓库外副本 + `go test -overlay` 逆改，要求逐字失败文案；探针用毕已删）：

| ID | 逆改点 | 四态 | 红态读数（逐字） |
| --- | --- | --- | --- |
| F-01（HIGH） | `run.go` 删 `Learning:` 字段（生产 CLI 未装配） | 通过 | `learning status 未通过：学习端口未注入`；`Error: 学习端口未注入——learning status 需要装配层注入的 LearningPort（internal/app 提供）`；`expected: 0, actual: 1` |
| F-02（MEDIUM） | 两后端 `ReleaseRunRefs` 删 `status <> 'running'` 守卫 | 通过 | SQLite `run_store_test.go:777` 与 PG `run_store_port_test.go:788`：`Should be false`，`running 的 Run 不得释放引用：mcp_server/browser` |
| R-01（MEDIUM） | `ConfirmCandidate` 退回只锁直接来源（不查闭包） | 通过 | `Expected error with "conflict" in chain but got nil` |
| R-02（MEDIUM） | `buildPrompt` 既有条目不做凭证过滤 | 通过 | 提示词出现 `"entry_id":44 … 部署令牌 sk-a1B2…`（完整值不回显） |
| R-03（LOW） | `keep_both` 携带替代目标仍走 400 | 通过 | `Target error should be in err chain` |

## 3. 交付物核对

按模块卡 §3.1 / §3.2 逐项 `ls` / `wc -l`（2026-11-06 当场）：

| 交付物 | 行数 | 核对 |
| --- | --- | --- |
| `internal/core/learning.go` | 388 | 存在；`LearningRunInput` / `LearningHook` / `MemoryExtractor` / 哨兵 |
| `internal/core/secret_scan.go` | 93 | 存在；`ScanFilesForSecrets` / `TextHasSecret` / `SourceHasSecret` |
| `internal/memory/learning.go` | 896 | 存在；端口、窗口、校验、草案 |
| `internal/memory/extract_output.go` | 118 | 存在；严格 JSON |
| `internal/memory/extract_validate.go` | 282 | 存在；逐项证据校验 |
| `internal/provider/service.go`（含抽取） | 436 | 存在；`ChatMemoryExtract` 与共享 `clientFor` |
| `internal/storage/postgres/learning_store.go` | 1217 | 存在；抽取、游标、预算、闭包 |
| `internal/storage/postgres/memory_confirm.go` | 395 | 存在；统一确认事务 |
| `internal/storage/postgres/migrations/0011_memory_learning.sql` | 114 | 存在；五表 + 两闭集扩展 |
| `internal/app/learning_admin.go` | 491 | 存在；只读管理面 + handoff |
| `internal/app/learning_recovery.go` | 117 | 存在；启动只查证 |
| `internal/app/learning_port.go` | 339 | 存在；生产 CLI 端口（F-01 修复） |
| `internal/app/memory_admin.go` | 828 | 存在；`ConfirmWithResolution` |
| `internal/web/handlers_learning.go` | 441（+测试 655） | 存在；五条 GET，无写端点 |
| `internal/cli/learning.go` | 774 | 存在；五个只读命令 + handoff |
| `internal/e2e/us22_learning_test.go` | 1115 | 存在；六用例（`ExtractConfirmRecall` / `RejectWrongCandidate` / `DeleteStopsRecall` / `DeleteSourceStopsDerived` / `RecoveryAfterCrash` / `DisabledNoSideEffects`） |
| `assets/console/src/views/learning-records.ts` | 978（+测试 598） | 存在；三页签、只读、双向深链 |
| `docs/contracts/memory-learning.md` | 319 | 存在；增量契约（含 §7.2 闭包、§8 恢复） |
| `harness/stages/us22-base.json` | 707 | 存在；12 check |
| **合计（上表 21 个文件）** | **11301** | 与 `.loop/t032/static-evidence.txt` 一致 |

契约面：`docs/contracts/openapi.yaml` 五条 GET（`/learning/status:712`、`/learning/extractions:736`、`/learning/extractions/{id}:770`、`/learning/drafts:799`、`/learning/drafts/{id}:829`）；任何写方法落 404 兜底。`run-and-errors.md:189-191` 三步收尾顺序，`:642` 学习抽取恢复行，`:609` `memory.confirm` 追加可选键。`docs/Operations.md:1601-1627` US-22 运维节（默认关闭、配置键、迁移 v11、草案布局与 handoff）。

前端构建：`assets/console/dist/integrity.json` 覆盖 4 个文件（`index.html`、`index-C2F8Lv3I.css`、`index-CgqLzF5n.js`、`workflow-template-CHPW9ZVb.js`，F-03 文案修正后重建）；左侧导航无独立「学习」入口；「学习记录」与方法草案无写按钮。

## 4. 前序回归与判据面

### 4.1 判据面变更（逐路径，`git diff --stat` 核对）

| 变更 | 路径 | 依据与形态 |
| --- | --- | --- |
| `FinishRun` 返回 `(bool, error)`：只有真正完成转换才 `applied=true`；终态与引用释放拆分 | `internal/core/store.go`、`internal/core/service.go`、`internal/storage/run_store.go`、`internal/storage/postgres/run_store.go`、`internal/app/run.go` | 模块卡 C-05 / §2.4.6 与 `run-and-errors.md:189-191`；同事务释放 → 三步固定顺序（终态 5 s `WithoutCancel` → 钩子 → 无条件 defer 释放） |
| `ReleaseRunRefs` 独立方法 + 两后端 `status <> 'running'` 守卫 | 同上 + 契约 `run-and-errors.md` | F-02 修复；释放失败不回滚终态，启动 `releaseTerminalRefs` 兜底 |
| 旧用例 `TestRunStore_FinishRunRollsBackWhenReleaseFails` → `TestUS22_FinishRunKeepsTerminalWhenReleaseFails`；新增 `TestRunStore_ReleaseRunRefsScopedToRun`、`TestUS22_ReleaseRunRefsSkipsWhileRunning` | `internal/storage/run_store_test.go`、`internal/storage/postgres/run_store_port_test.go`、`internal/storage/recovery_test.go` | 契约明文替换（见 `run-and-errors.md` 同段）；`recovery_test.go` 夹具改用 `applied==true` + 恢复补齐释放 |
| 25 个既有测试文件适配新签名 / 拆分（部分补 `require.True(applied)` 与显式 `ReleaseRunRefs`） | `git diff --stat -- '*_test.go'`（303 增 / 115 删） | **只适配已批准接口**；逐文件 diff 复核无断言删除、无阈值放宽（见 run-state §2 与 §4.2） |
| schema 版本与表数钉 | `internal/app/ports_test.go`（82→87）、`internal/app/supported_version_injection_test.go`（10→11）、`internal/storage/postgres/migrate_integration_test.go`（82→87）、`internal/storage/postgres/observability_migration_test.go`（10→11）、`internal/app/ports_test.go` 旧表桶 58→63 | 迁移 0011 只追加五表；`TestPostgres_FreshSchemaIsV1` 表数 87、版本 11 |
| `source` 闭集追加 `auto_extract`；`purpose` 闭集追加 `memory_extract`（迁移 CHECK） | `docs/contracts/openapi.yaml`、`0011_memory_learning.sql`、Go 闭集 | Q5 用户裁决（2026-11-05）；**无新增动作字面量** |
| `memory.confirm` 可选 body、400 / 503 语义、`superseded_id` 回执 | `docs/contracts/openapi.yaml`、`internal/web/handlers_admin_agents.go`（`decodeAdminWriteBodyOptional`）、`internal/app/memory_admin.go` | Q13 裁决；自动候选空 body 也走全门禁；未装配 resolver → 503，不降级 |
| stage `required_tests` 追加 10 条（207→217；唯一名 200→210） | `harness/stages/us22-base.json` | 只追加、不改 argv、不删项；F / R 修复轮补入 7 个具名用例 + 3 条既有用例补登 |
| 扫描器下沉共享 | `internal/core/secret_scan.go` 新增；`internal/app/admission_secret.go` 删除；`admission_secret_test.go` 改调 `core.*` | US-21 凭证规则保持一份；`TestUS22_SecretScanner_SharedWithAdmission` 断言两实现逐样本一致 |
| 迁移只追加 | `0011_memory_learning.sql`；`docs/contracts/schema.pg.sql` 零改动；0001–0010 零改写 | 宪法 IX |
| 夹具余量加长批次 ①（**只改时限，不改断言**） | `internal/provider/client_test.go`（`readPhaseTrigger` 500 ms → 5 s）；`internal/app/flow_http_integration_test.go`（HTTP 夹具 `Client.Timeout` 3 s → 15 s） | 用户 2026-11-06 裁决：「余量不够可以增加多一些，不然每次都余量不够导致 make check 一直重跑浪费时间」。实测依据：第十轮 stage `cover` 的 `TestProviderService_CancelDuringResponseReadTimeoutKind`（500 ms 余量被并行争用吃掉，取消落到传输阶段）、第七轮 `TestFlow_HTTPResumeIdempotentFactBeforeCurrentVersion`（`Client.Timeout exceeded while awaiting headers`）。两处断言的判别字面量（「未取完响应」/ Status==200 与各 HTTP 断言）**逐字未动**；只降低争用下的误触发，不改变验收语义 |
| 夹具余量加长批次 ②（同一裁决，**只改等待上限**） | `internal/app/flow_runtime_integration_test.go`（3 s→15 s ×4：`:56` `:74` `:227` `:239`；7 s→20 s：`:92`；6 s→30 s：`:113`；并发 Close 3 s→15 s：`:256`）、`internal/app/flow_evaluation_integration_test.go`（3 s→15 s：`:201`）、`internal/app/flow_http_integration_test.go`（收尾 join 5 s→15 s：`:83`） | 同一条用户裁决；触发证据是余量批次 ① 后 `make check` 的 `cover` 失败（§1.2 第 10 行：`flow_runtime_integration_test.go:110` 6 s join 看门狗）。**保留未动**：32 s 取消容忍看门狗、`require.GreaterOrEqual(closed.elapsed, 30*time.Second)` / `require.Less(closed.elapsed, 32*time.Second)` / `time.Since(drainStarted) >= 35*time.Second` 三条行为断言、并发 Close 的 50 ms 负向断言。三个文件均未被本 US 的产品改动触及（`git status --porcelain` 在本次编辑前对其零命中） |
| 取消容忍上界 32 s → 40 s（**用户 2026-11-06 裁决**，唯一一处改动**阈值本身**的调整） | `internal/app/flow_runtime_integration_test.go:89`（`require.Less(t, closed.elapsed, 40*time.Second)`）、`:83` 看门狗同步 32 s → 45 s | 依据：第 11 轮 `make check` 实测 `32.5669914s`，超出 2 s 松弛 567 ms；30 s 容忍期满后还要落库与回填审计，20 核并行 + cover 插桩下抖动超过 2 s。**保持不动**：`require.GreaterOrEqual(t, closed.elapsed, 30*time.Second)`（必须等满容忍期，防提前返回）与 `time.Since(drainStarted) >= 35*time.Second`；上界仍能抓住「卡死不返回」（分钟级）。裁决原文由用户在本轮问答中给出（「放宽到 40s」） |
| 夹具写回上限 1 s → 15 s（同一裁决，**只改夹具上限**） | `internal/flow/coordinator_integration_test.go`（`FinishRun` 上限 1 s→15 s）；`internal/storage/postgres/flow_advance_integration_test.go:43`（同族同名上限）；`coordinator_integration_test.go` 状态断言补 `reason=%q` 失败信息 | 触发证据：第 12 轮 `make check` 的 `cover`（§1.2 第 12 行）。机制：夹具写回上限被打穿 → 叶子返回错误 → 产品按 fail-closed 停为 `leaf_execution_unconfirmed`；已用 1 ns 上限**诱导红态**复现逐字相同症状（`回填运行终态… 开启事务: context deadline exceeded`）。状态期望与断言**逐字未动**；`reason=%q` 只增失败信息，不改判据 |
| 判据面影响自评 | 夹具余量批次合计 8 个测试文件、16 处时限 / 1 处阈值 | **不改变验收语义**：其余 13 处都是「等多久算超时」的夹具余量，断言行为（阶段文案、Status、真正 join、35 s 排水超时、`succeeded` / `failed` / `blocked` 状态）逐字保留；唯一一处阈值调整（32 s → 40 s）经用户裁决并保住下界，已列入 review ② 待确认项。无断言删除、无 `t.Skip` |
| 管理台文案修正（复核三轮 F-03）：五处「自动抽取属于后续设计」的陈旧表述改为已交付能力（默认关闭、候选需确认）；重建 console dist | `assets/console/src/components/memory-entries.ts`、`assets/console/src/views/agent-detail.ts`、`assets/console/src/views/workspace.ts`、`assets/console/tests/agent-operations.test.ts`、`assets/console/dist/*` | F-03：自动抽取已交付，旧文案却称「不会自动从对话里抽取」，与行为矛盾。改后 console `npm test` 41 文件 / 485 用例 PASS；`npm run build` 重建 4 个 dist 文件（integrity 同步）。**只改文案与构建产物**：被测试断言的句子逐字保留，唯一改动的断言同步到新文案（`不会自动从对话里抽取内容` → `确认后才进入这一区`），不删除其它断言、不放宽阈值 |
| 仓储重试用例改为生产真实顺序（复核三轮 F-02）+ CLI 显式 `--limit 0` 拒绝（F-04） | `internal/storage/postgres/learning_store_test.go`、`internal/cli/learning.go`、`internal/cli/learning_test.go` | F-02：原用例先 `BeginExtraction` 翻回 `pending` 再 `window_gone`，与生产顺序相反（生产在 `BeginExtraction` **之前**重读冻结窗口），因此掩盖 F-01；改为「第 1 次失败 → 第 2 次重试再失败 → 第 3 次读窗口缺失」，**冻结边界 / attempts / 游标断言全部保留**，只增加真实 `failed → skipped` 转移。F-04：`--limit` 用 `Flag.Changed` 区分「省略（缺省 50）」与「显式 0（非法）」，与帮助和 HTTP 面 `1–200` 一致，只增两条拒绝断言 |
| 夹具余量加长批次 ③（同一裁决，**只改等待上限**） | `internal/app/flow_observation_integration_test.go`（状态 `Eventually` 5 s → 20 s）；`internal/storage/postgres/pgtest/pgtest.go`（隔离 schema 回收 `DROP` 10 s → 30 s） | 触发证据：第 14 轮 `make check` 的 `test-integration`（§1.2 第 14 行）。隔离复跑 10/10、5/5 PASS；仓库外 `-overlay` 逆改 5 s→50 ms 与 10 s→30 ms 复现逐字相同失败文案。状态断言、清理语义与 3 分钟取槽位上限逐字未动；两个文件均未被本 US 的产品改动触及 |

**判据面零静默弱化**：上表每条要么是已审卡 / 已批准裁决的明文要求，要么是随已批准接口的等价适配；改变验收语义的条目均在模块卡 §2.4.6、`run-and-errors.md` 与 Q13 / Q5 裁决中有记录。新增测试全部为追加。

### 4.2 前序模块回归

`make check` 的 `test` / `test-integration` / `test-pg` / `cover` 覆盖全仓（含 US-1～US-21 全部包）；stage 的 `us22-base-pg` 另跑既有迁移 / 可观测 / Flow / 通知回归。第 3 轮失败的两条版本钉是本 US 迁移带来的**真实判据面差**，已按新版本更新；第 6、7 轮两条失败经隔离复跑判定为环境 / 时序，未改任何断言或超时。详见 §1.2。

## 5. 全局不变量自查（机器可判，2026-11-06 当场读数）

| # | 不变量 | 命令（节选） | 读数 | 四态 |
| --- | --- | --- | --- | --- |
| 1 | 坐标零 Agent 框架 | `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` | 零命中 | 通过 |
| 2 | `context.WithValue` 只允许追踪标识 | `grep -rn "context.WithValue" --include='*.go' cmd internal` | 4 处：`middleware.go:41/79`（request-ID）、两个测试夹具；另 1 条注释引用；无业务状态 | 通过 |
| 3 | 审计写路径唯一 | 新增写 SQL 全在 `internal/storage`（`learning_store.go`、`memory_confirm.go`、`llm_calls.go`）；`internal/core` 无 `.BeginCall(` | 零命中（core 与 storage 之外） | 通过 |
| 4 | 凭证零明文 | `grep -rnE "sk-[A-Za-z0-9]{16,}" --include='*.go' --include='*.yaml' .` | 0 命中 | 通过 |
| 5 | 无 ORM / 无自动迁移 | `grep -nE "gorm\|entgo\|ent\.io\|xorm" go.mod`；迁移只在 `schema.sql` + `0011` | 零命中；0011 只追加 | 通过 |
| 6 | 单二进制 `CGO_ENABLED=0` | `make build`（见 §1.1） | 通过（`-race` 为独立命令 / job） | 通过 |
| 7 | 时区禁用词 | `grep -rnE "time\.Local\|[.]Local[(][)]\|time\.Now[(][)]\.Location[(][)]" --include='*.go' .` | 2 处均为测试文件注释（逆改点说明）；生产代码零命中 | 通过 |
| 8 | 随机源红线 | `grep -rn '"math/rand' --include='*.go' cmd internal` | 零命中（抽取标识用 `crypto/rand`，见 `core/learning.go`） | 通过 |
| 9 | 点导入零命中 | `grep -rnE '^import[[:space:]]+\.\|^[[:space:]]+\.[[:space:]]*"' --include='*.go' cmd internal` | 零命中 | 通过 |
| 10 | `core` 只依赖标准库 | `GOPROXY=… go list -f '{{.Imports}}' ./internal/core` | 全部标准库（含 `crypto/sha256` / `crypto/rand`），零第三方、零内部包 | 通过 |
| 11 | LLM 审计入口唯一 | `grep -rn '\.BeginCall(' --include='*.go' internal/core` | 零命中 | 通过 |
| 12 | 前端构建不进 Go 门禁 | `grep -nE 'node\|npm' Makefile scripts/check-acceptance.sh scripts/check-acceptance.ps1` | 零命中 | 通过 |
| 13 | SQLite 不是运行期存储 | `grep -rn 'modernc.org/sqlite' --include='*.go' internal/app internal/web` | 6 命中，**全部**为既有 `_test.go` 的空白导入（`author_test.go` / `baseline_test.go` / `migrate_v3_test.go` / `providers_test.go` / `realcall_integration_test.go` / `wire_test.go`，均未被本 US 修改）；生产代码零命中 | 通过（附说明） |

## 6. 复核结论与剩余人工项

### 6.1 独立 AI 复核（独立上下文，三轮）

- **首轮**：F-01（HIGH，生产 CLI 学习端口不可达）→ 修复并补 `learning_port.go` 生产适配 + 真实入口回归；F-02（MEDIUM，释放缺终态守卫）→ 两后端加守卫并改契约与用例；F-03～F-06（LOW，Chmod 吞错 / 注释失实 / 草案 ID 遍历 / 目录 chmod）→ 全部 fail-closed 并补 `TestUS22_LoadDraft_RejectsDotIDs`（台账该行写的拟名 `TestUS22_Draft_RejectsTraversal` 未采用，以实际测试名为准；另追加台账更正行）。红态逐字见 §2。
- **二轮**：R-01（MEDIUM，确认未核对来源闭包）→ 递归闭包复核（深度 8，fail-closed）+ 契约 §7.2 / §7.3 更正；R-02（MEDIUM，提示词既有条目绕过凭证过滤）→ 命中凭证不进提示词；R-03（LOW，keep_both 携带替代目标返回 400）→ 改走 409。均补具名测试与红态。
- **三轮（收口前）**：F-01（HIGH，`FailExtraction` 对 `failed` 行的 `window_gone` 空操作 → 学习通道永久阻断，C-33 症状逐字命中）→ 允许 `failed + window_gone` 直接终结为 `skipped` + `finished_at` + 推进游标，**不递增 attempts**；红态逐字 `expected: "skipped" / actual: "failed"`（`learning_store_test.go:585`），修复后仓储 30 用例 PASS（`ok 16.734s`）。F-02（MEDIUM，仓储用例顺序相反是 F-01 逃过门禁与 stage 的直接原因）→ 用例改生产真实顺序。F-03（LOW，管理台五处陈旧文案）→ 改为已交付能力并重建 dist。F-04（LOW，CLI `--limit 0` 与帮助 1–200 矛盾）→ `Flag.Changed` 区分缺省 / 显式 0。F-05（复核者标「待确认」的夹具余量）→ 已在 run-state §2 与 §4.1 登记用户裁决（复核者按约束未读 run-state）。F-06（LOW，卡片 `:1355`「字节上限照常应用」与实现只裁剪前文的措辞差，效果等价）→ **不改代码**，卡片勘误建议随 review ② 裁决（卡已冻结，不静默改写）。
- **三轮未覆盖面（复核者声明，逐条登记）**：未独立执行 `go test` / `make check` / stage（以控制侧读数核对）、未在真实 PostgreSQL 上跑迁移、未重建前端或校验 dist 哈希、未跑 openapi 校验与 markdownlint、未做浏览器 UI 走查、按约束未读取 run-state 私有路径、并发只做静态分析、真实模型按裁决不验证。其中真实 PG 迁移、前端构建与 dist 由最终轮 stage 覆盖；openapi 契约由 `TestUS22_OpenAPIContract` 覆盖；markdownlint 与 UI 走查保留为人工项（见 §6.3）。
- **已知限制（登记，不改代码）**：F-07 —— stage 的 `console-test` / `console-build` 无法承载 `required_tests`（校验器要求 `go test -json`），前端断言由 Vitest 7 组 `TestUS22_Console_*` 承载；F-08 —— 模块卡 §2.3.5 与 §3.1 的锁序表述存在内部张力，实现按 §3.1（预算判定与 pending 写入同事务取 Agent 锁）；卡已冻结，不改写；F-09 —— plan / 卡的文件清单与实际落点存在差异（`learning_port.go` 等后续修复文件不在卡列、`admission_secret.go` 按任务删除），逐条记录在 §4 与本报告。
- **未覆盖面**：真实模型抽取质量、固定评测集、隔离 A/B、技能绑定收益、多节点、企业治理——全部 **`未测量`**，不写成已具备。

### 6.2 静态门禁妥协点（需 review ② 确认）

6 处 `#nosec`，均为「已审设计 + 语义不可替代」并逐条写明理由：

| 位置 | 规则 | 理由 |
| --- | --- | --- |
| `internal/memory/learning.go:738` | G302 | 已审设计：草案文件固定 0444 |
| `internal/memory/learning.go:742` | G302 | 已审设计：证据文件固定 0444 |
| `internal/memory/learning.go:746` | G302 | 已审设计：草案目录固定 0555 |
| `internal/storage/postgres/learning_store.go:827` | G202 | 拼接片段全部可追到本函数固定字面量，过滤值走位置参数 |
| `internal/storage/postgres/learning_store.go:968` | G202 | 同 `ListExtractions`：固定模板 + 位置参数 |
| `internal/app/learning_admin.go:283` | G304 | 目录名已拒绝分隔符与上跳（单层路径元素），文件名为固定字面量 |

### 6.3 人工验证项（M-01～M-13）

模块卡 §6.1 的 13 项人工验证，自动化判据均有具名测试覆盖（逐项对应见下表）。**用户 2026-11-06 明确不做管理台人工走查，按自动化判据接受 M-01～M-13**（决定原文见 `run-state.md` §5）；表内「人工状态」列保留「按自动化判据接受」口径，不冒充逐项人眼走查。

| # | 验证项 | 自动化判据（具名测试） | 人工状态 |
| --- | --- | --- | --- |
| M-01 | 默认关闭 | `TestUS22_Config_DefaultsOff`、`TestUS22_Disabled_NoWritesNoCalls`、`TestUS22_E2E_DisabledNoSideEffects` | 按自动化判据接受（2026-11-06 用户决定） |
| M-02 | 启用后生成候选 | `TestUS22_E2E_ExtractConfirmRecall`、`TestUS22_Candidate_InsertedAsCandidate` | 按自动化判据接受（2026-11-06 用户决定） |
| M-03 | 确认后新 Run 召回 | `TestUS22_E2E_ExtractConfirmRecall`、`TestUS22_Confirm_ResolutionReceipt` | 按自动化判据接受（2026-11-06 用户决定） |
| M-04a | 拒绝错误候选 | `TestUS22_E2E_RejectWrongCandidate`、`TestUS22_Candidate_NotRecalledBeforeConfirm` | 按自动化判据接受（2026-11-06 用户决定） |
| M-04b | 保留双方 | `TestUS22_Confirm_BodyOptional`、`TestUS22_Confirm_KeepBothDefault` | 按自动化判据接受（2026-11-06 用户决定） |
| M-05 | 证据可复核 | `TestUS22_E2E_ExtractConfirmRecall`（证据字段）、`TestUS22_Console_PendingUsesMemoryActions` | 按自动化判据接受（2026-11-06 用户决定） |
| M-06 | 失效后停止召回 | `TestUS22_E2E_DeleteStopsRecall`、`TestUS22_E2E_DeleteSourceStopsDerived`、`TestUS22_Recall_Derived*` | 按自动化判据接受（2026-11-06 用户决定） |
| M-07 | 恢复 | `TestUS22_E2E_RecoveryAfterCrash`、`TestUS22_Recovery_*`、`TestUS22_Unknown_NoRestartNextRunNoReplay` | 按自动化判据接受（2026-11-06 用户决定） |
| M-08 | 草案只读 | `TestUS22_Draft_FixedLayout` / `_ReadOnly` / `_NotAgentReadable` / `_NoScripts`、`TestUS22_CLI_Handoff*` | 按自动化判据接受（2026-11-06 用户决定） |
| M-09 | 无新增副作用 | `git diff go.mod go.sum package.json` 为空；无新端口 / 进程 | 按自动化判据接受（2026-11-06 用户决定） |
| M-10 | 契约一致 | `TestUS22_OpenAPIContract`、`TestUS22_NoWriteEndpoints`、`TestUS22_Contracts_NoNewActionLiterals` | 按自动化判据接受（2026-11-06 用户决定） |
| M-11 | 生命周期顺序 | `TestUS22_Hook_OnlyAfterTerminalApplied`、`TestUS22_Hook_OncePerRun`、`TestUS22_FinishRunKeepsTerminalWhenReleaseFails` | 按自动化判据接受（2026-11-06 用户决定） |
| M-12 | 确认门禁 | `TestUS22_Confirm_ExpiredRejected` / `_OriginRevokedRejected` / `_KeyExpiredRejected` / `_CallerDisabledRejected` / `_ClosureInvalidatedRejected` | 按自动化判据接受（2026-11-06 用户决定） |
| M-13 | 导航与双向定位 | `TestUS22_Console_MemoryTabs` / `_LearningRecordsReadOnly` / `_DeepLinksPreserveAgent` / `_SupersedeNeedsSuggestion` / `_SharedScopeWarning` | 按自动化判据接受（2026-11-06 用户决定） |

### 6.4 结论

- 七项证据（§1.1 门禁与 stage、§2 harness、§3 交付物、§4 前序回归与判据面、§5 不变量、§6 复核与人工项、§7 复跑）**逐项在本报告落点**。
- 效果类结论 `未测量`；真实模型、外部网络、脚本执行均未使用。
- **人工 review ②：通过（用户 2026-11-06 决定）**——用户明确不做管理台人工走查，按自动化判据接受 M-01～M-13；§6.2 的 6 处抑制、§4.1 的 32 s→40 s 阈值调整与 F-06 勘误（不修订、卡维持冻结）随收口包一并接受。决定原文登记在 `run-state.md` §5，非 AI 代签；提交、推送与合回 `main` 由该决定授权执行。

### 6.5 收口复跑 `/speckit-analyze`（2026-11-06，只读）

按模块卡 R6（`289b5e70…` / 2039 行）与当前 `spec.md` / `plan.md` / `tasks.md` 复跑实施后一致性分析（T033 第一件事）。**CRITICAL = 0**，可进入收口。

**需求覆盖**：FR-22-01～FR-22-13 各 ≥1 个实现任务；SC-001～SC-010 各 ≥1 个任务或验收证据。

| 需求 | 任务 | 需求 | 任务 |
| --- | --- | --- | --- |
| FR-22-01 | T011 / T012 / T013 / T023 | FR-22-08 | T021 / T022 / T023 |
| FR-22-02 | T005 / T010 / T012 | FR-22-09 | T024 / T025 |
| FR-22-03 | T011 / T016 / T017 | FR-22-10 | T016 / T018 |
| FR-22-04 | T009 / T010 / T011 | FR-22-11 | T026 / T027 / T028 |
| FR-22-05 | T008 | FR-22-12 | T003 / T029 |
| FR-22-06 | T009 / T011 / T013 | FR-22-13 | T019 / T020 |
| FR-22-07 | T014 / T015 | — | — |

**未映射任务**：T001（基线与流程证据）、T002（stage 派生）、T030（文档修订）、T032 / T033（验收与收口）——均为 `module-dev` 流程任务，不承载 FR；无「有任务无需求」的范围外实现。

**宪法对齐（相关原则逐条机器判）**：原则 1 `go.mod` 框架坐标零命中、`internal/core` 非标准库依赖零命中；原则 2 `ChatMemoryExtract` 不接受 `Tools` / `Flow`，生产装饰器必须实现、路由器不得实现（`TestUS22_Extractor_*`）；原则 3 显式映射 + `Pinned` 冻结携带预写面；原则 5 两级预写 fail-closed；原则 7 无 goroutine、无后台 Worker、同步有界（≤20 s）；原则 8 `CGO_ENABLED=0` 构建通过、零新依赖；原则 9 迁移只追加（`git status` 仅 `?? 0011_…`）、`schema.pg.sql` 未改。原则 4 / 6 / 10 未被本 US 的产品改动触碰（无 Agent 目录 / 技能绑定变更、无新执行接口、范围按卡）；无原则冲突 → 无 CRITICAL。

**一致性发现**：F-06（LOW，卡片 `:1355` 措辞差，效果等价）与 F-07 / F-08 / F-09 已在 §6.1 登记为已知限制，卡已冻结、不在本分析内改写；其余术语（自动抽取 / 候选 / 草案 / 交棒）在 spec / plan / tasks / contracts 一致，无重复需求、无不可测验收项、无占位符残留。

**结论**：CRITICAL 0 / HIGH 0 / MEDIUM 0 / LOW 4（均已登记——3 项不改代码、F-06 随 review ② 裁决）。

## 7. 复跑与交接

```sh
export GOPROXY=https://goproxy.cn
export BASALT_TEST_PG_DSN='postgres://postgres@127.0.0.1:<port>/basalt_test?sslmode=disable'  # 一次性 PG，勿用开发库

# 1) 宪法口径七目标（make check = fmt lint sec test test-integration cover build）
make check

# 2) 全量 stage（12 check；Windows 需先装配便携 Node 与 DSN）
sh scripts/check-acceptance.sh --stage harness/stages/us22-base.json

# 3) 本 US 定向回归
go test -count=1 ./internal/core ./internal/memory ./internal/provider
go test -tags=integration -count=1 -run '^TestUS22_' ./internal/app ./internal/storage/postgres
go test -tags=e2e -count=1 ./internal/e2e
go test -race -count=1 -run 'TestUS22_(Hook|Extract|Confirm)' ./internal/app ./internal/memory

# 4) 不变量与依赖
grep -rn '\.BeginCall(' --include='*.go' internal/core
grep -nE "eino|langchaingo|adk-go|trpc-agent-go" go.mod
git diff --stat go.mod go.sum assets/console/package.json
```

预期：1) exit 0；2) `status=passed`、12/12 check、217 必跑条目全 PASS；3) 全 PASS；4) 全部零命中 / 空 diff。

### 7.1 遗留：门禁结构与测试稳定性治理（交 US-23，不属本 US 交付）

本 US 计入预算的 15 次门禁 / stage 失败暴露两条**结构性**问题，处置属于工程流程，不改 US-22 验收面：

1. **一轮 `make check` 跑三遍全仓**：`test`（`-race -cover`，其中 `-cover` 无消费者）、`test-integration`、`cover`（重新全仓跑一遍带 `-coverprofile`）。一轮约 45 min，其中约 2/3 是重复执行。建议（待 US-23 立项时评审）：`test` 去掉无消费者的 `-cover`；`cover` 改为消费 `test-integration` 产出的 profile（`scripts/check-coverage.sh` 已支持注入 profile），并统一 `-timeout`（现为 20 m / 22 m 两处不一致）。预期降到约 28 min。**本 US 不改 Makefile**——中途改门禁结构会让已跑轮次的读数失去可比性。
2. **固定秒级等待上限是主要抖动源**：15 次失败中，第 9～12 与第 14 轮共 7 个用例源于测试夹具的墙钟预算（3 s / 6 s / 1 s / 50 ms 级）在 20 核并行、全仓并行集成与 cover 插桩下被吃掉，而不是产品缺陷；第 15 轮是主机临时端口瞬时耗尽（同样不是产品缺陷），stage 第 11 / 12 次是 WSL VM 停机与 C: 磁盘耗尽两类纯环境故障。本 US 已把已观测到的 16 处上限加到 15–45 s（§4.1，断言逐字未动）；用户同时给出四步治理：① 开发期只做定向验证；② 全量失败先缩小复现、验证根因再修，不得以「单跑一次绿」结案；③ 已知问题收敛后才跑全量；④ 全量通过后停止无依据的重复验证。该治理应写进 `docs/DevelopmentLoop.md` 的 Loop 协议与陷阱表，并由 US-23 承担门禁结构与夹具同步机制（事件同步替代固定 sleep）的改造。

原始备忘：`.loop/t032/followup-gate-optimization.md`（临时物，收口删除；内容已并入本节）。

## 附录 A：逐 check 必跑读数

`report.json` 的 `checks[].required_tests` 与 `passed_tests` 逐 check 读数（run `accept-20261106T105018.044921100`，`status=passed`）：

| check | 必跑条目 | 本轮通过 | 耗时 / exit | 日志 |
| --- | --- | --- | --- | --- |
| us22-base-core | 8 | 15 条通过，必跑 8/8 命中，0 FAIL / 0 SKIP | 1.1 s / 0 | us22-base-core.log |
| us22-base-memory | 69 | 144 条通过，必跑 69/69 命中，0 FAIL / 0 SKIP | 2.5 s / 0 | us22-base-memory.log |
| us22-base-provider | 11 | 12 条通过，必跑 11/11 命中，0 FAIL / 0 SKIP | 1.1 s / 0 | us22-base-provider.log |
| us22-base-app | 60 | 75 条通过，必跑 60/60 命中，0 FAIL / 0 SKIP | 13.6 s / 0 | us22-base-app.log |
| us22-base-pg | 39 | 66 条通过，必跑 39/39 命中，0 FAIL / 0 SKIP | 14.7 s / 0 | us22-base-pg.log |
| us22-base-web | 11 | 55 条通过，必跑 11/11 命中，0 FAIL / 0 SKIP | 1.3 s / 0 | us22-base-web.log |
| us22-base-cli | 6 | 8 条通过，必跑 6/6 命中，0 FAIL / 0 SKIP | 1.4 s / 0 | us22-base-cli.log |
| us22-base-e2e | 6 | 6 条通过，必跑 6/6 命中，0 FAIL / 0 SKIP | 11.2 s / 0 | us22-base-e2e.log |
| us22-base-console-test | —（Vitest 不受 `required_tests` 约束，见 §6.1 F-07） | 41 个测试文件 / 485 个测试全过（含 §6.3 所列 `TestUS22_Console_*`） | 14.2 s / 0 | us22-base-console-test.log |
| us22-base-console-build | — | 构建通过，`integrity.json` 4 个文件 | 8.0 s / 0 | us22-base-console-build.log |
| us22-base-race | 6 | 6 条通过，必跑 6/6 命中，0 FAIL / 0 SKIP | 6.6 s / 0 | us22-base-race.log |
| us22-base-memory-race | 1 | 1 条通过，必跑 1/1 命中，0 FAIL / 0 SKIP | 3.0 s / 0 | us22-base-memory-race.log |

必跑条目合计 **217**（8 + 69 + 11 + 60 + 39 + 11 + 6 + 6 + 6 + 1），全部命中且零 FAIL / 零 SKIP；`passed_tests` 另含同包其它 `^TestUS22_` 与既有回归用例（core 15、memory 144、provider 12、app 75、pg 66、web 55、cli 8、e2e 6、race 6、memory-race 1，合计 388 条去重前记录）。

## 附录 B：工件指纹（SHA-256）

| 工件 | SHA-256 | 行数 / 说明 |
| --- | --- | --- |
| `docs/us/us22-controlled-evolution.md`（R6 已审字节） | `289b5e70712974002d9addd9ab63893144ff7c6de332de73d6a1dd95c41ddfe9` | 2039 行（审阅基线） |
| `docs/us/us22-controlled-evolution.md`（当前文件，含批准横幅） | `449d58f9b1348966a34186c0c6727b7a0956d77c4cbf33b0e807e4fcadb9d14a` | 2042 行，冻结未改 |
| `specs/022-controlled-evolution/spec.md` | `06c5e121dfd2936af1ecc6a8724f49479964fe25be77543661dc8ee280f857d0` | 193 行 |
| `specs/022-controlled-evolution/plan.md` | `3415e3c9cb6902620a31673117b71769d9c60221f29a8a739fb9c6024911a677` | 172 行 |
| `specs/022-controlled-evolution/tasks.md` | `b5048172de359c6457ca88633364d53dcd163847c345b8b3d20dcb610ba44ed4` | 130 行，34/34 勾选（stage 指纹按 `scripts/acceptance/fingerprint.go` 归一化勾选位，冻结值 `87539164…` 不受翻转影响） |
| `harness/stages/us22-base.json` | `092e38c9b2a8dbaea60a123550569b70edf97f6cd1bb90918a2f0b10a7263d0d` | 707 行，12 check / 217 必跑 / 70 protect |
| `assets/console/dist/integrity.json` | `1ff134b06f2a3011d9e1a566c4d77e2d91f1bf777a4880c39d02285ee1987f19` | 4 文件完整性清单 |
| `bin/basalt.exe` | `b379ec95a570a895c3d85635be47fd79686275b090cd277dbafedb358d691e51` | 第 17 轮 `CGO_ENABLED=0` 构建产物 |
| stage 源码指纹（run `accept-20261106T105018.044921100`） | `1203e134ba64f181b9f7fed927863466cbaea0b86a0e4f3dcdb45cdd54815a49` | 由 `report.json` 记录；stage 被测二进制 `c250e3c6…` |

> stage 指纹在报告写出前计算（`scripts/acceptance/execute.go`），本附录、run-state 与 tasks 勾选位属过程状态 / 验收记录，不代表被测源码变化——被测代码、测试、stage 与契约在 stage 运行时已是最终态。
