# US21 验收报告

日期：2026-11-05～06（review ② 修复轮第二轮）。状态：全量 stage `accept-20261105T134838.179213700`（21/21 通过）与 `make check`（exit 0）均已在修复轮第二轮的源码上完成，读数见 §1.1；两者绑定范围不同，D-24 的证据边界见 §1.1 与 §4 第 19 条。修复轮第一轮的全量 stage `accept-20261105T024043.645871300` 绑定的是**上一轮**源码，已降为历史读数（§1.2），不能证明本轮四项修复。**人工 review ② 已于 2026-11-06 通过**（用户明确回复「review2通过，可以提交代码合并到main了」），据此授权提交与快进合并 `main`；U3 / F-05 仍为已登记的未验证项，不改写为已验证。review ① 依据与 review ② 退回 / 裁决见 [run-state](run-state.md) §1。未 commit、push、merge、tag、发布或部署。

## 1. 门禁与取证基线

### 1.1 本轮读数（review ② 修复轮第二轮，2026-11-05 / 11-06）

本轮在**修复轮第二轮的源码**上重跑全量 stage 与七目标。两份证据的**绑定范围不同**，必须分开读：

- **全量 stage** `accept-20261105T134838.179213700` 绑定源码指纹 `635080e5f03f2ac53e5f347c9f166f0f3d00ac43174be6d6f26506bf879a7d81`，覆盖 F-01～F-04 四项修复与 R-03。
- **`make check`**（2026-11-06 00:07:36 起跑）绑定**当前工作树**：起跑前后 `git diff | sha256sum` 均为 `5aee733ba570c6d4…`、`git status --porcelain | sha256sum` 均为 `2479c838d1ed7547…`，两次一致，说明运行期间源码未变。该树**包含 D-24**（`specs/016` 的 `keep_recent_groups` 默认值改动）。
- D-24 按用户 2026-11-05 裁决**不单独重跑全量 stage**，因此 stage 证据不含 D-24；D-24 的判据由 `TestLoadConfig_CompactionKeepRecentGroupsDefaultsToFour`（4 子例）与本次 `make check` 承载，事实源见 `specs/016-knowledge-memory-history/run-state.md` R175。

本轮修复四点：F-01 工作副本交换 + 回滚双失败记 `unknown`（`workspace.ErrWorkCopyUncertain`）；F-02 冻结候选目录已就位但写行报错记 `unknown`；F-03 路径段首尾空白拒绝 + 脚本分类前剥末尾空白；F-04 `policy_hash` 输入补 snake_case `json` tag。契约同步（`skill-admission.md` `05b68e9a…`），模块卡 §7 追加一行（`751cb573…`）。

**全量 stage**（[report.json](../../.loop/runs/accept-20261105T134838.179213700/report.json)；终端日志 `.loop/us21/final-acceptance-us21-review2i-20261105.log`）：

| 命令/检查 | 四态 | 本轮读数 | 证据 |
| --- | --- | --- | --- |
| stage 总体 | 通过 | **21 / 21 check，`status=passed exit=0`**，`scope=full/us21-base`，`dirty=true`，`go_version=go1.27.3`，总用时 3661.9 s（逐 check 合计 3659.1 s） | report.json |
| fmt / lint / sec | 通过 | 3.0 / 9.4 / 22.1 s，均 exit 0；`No vulnerabilities found` | fmt.log / lint.log / sec.log |
| test（race，全仓） | 通过 | 117.7 s；`internal/app` 89.929 s | test.log |
| test-integration | 通过 | 1174.7 s；`internal/app` **1166.275 s** / 1200 s（余量 33.7 s） | test-integration.log |
| test-pg（race） | 通过 | 872.0 s；`storage/postgres` **866.953 s** / 900 s（余量 33.0 s） | test-pg.log |
| cover | 通过 | 1293.6 s；`internal/app` **1275.078 s** / 1320 s（余量 44.9 s）；全部有语句包达标 | cover.log |
| build（`CGO_ENABLED=0`） | 通过 | 0.7 s，exit 0 | build.log |
| eval-build / behavior / injection | 通过 | 1.7 / 12.3 / 5.9 s；behavior 为 5 PASS / 1 SKIP | 同名 log |
| behavior：outcome-real-model-answer | 跳过 | 未给 `--provider`；遵守 2026-11-04 裁决，属 `harness/acceptance.json` 允许的 skip | behavior.log |
| US21 base stage | 通过 | 10 checks、**125** 个必跑条目全部 PASS，必跑项零 FAIL / 零 SKIP | report.json / 附录 A |
| stage：console test / build | 通过 | 40 文件 / 478 测试；构建 exit 0（integrity 4 文件）；11.0 s / 6.5 s | 同名 log |
| `tested_binary_sha256` | 通过 | `a3d23d5814ce6ce474e2821039e3a6f9593163d23af43a11c48270fbb3891d76`（e2e 自建候选二进制，`binary_source=temp`，不是 `bin/basalt.exe`） | report.json |

**`make check`**（`.loop/us21/make-check-review2j-20261106.log`，UTF-8，非 PowerShell 重定向）：

| 目标 | 四态 | 本轮读数 |
| --- | --- | --- |
| 总体 | 通过 | **exit 0**；00:07:36 起跑、`bin/basalt.exe` mtime 00:48，墙钟 ≈ 41 分钟；全文 `FAIL` / `panic:` / `out of shared memory` / `context deadline exceeded` 命中数 **0** |
| fmt | 通过 | `check-format: gofmt / gofumpt 均干净` |
| lint | 通过 | `go vet -tags=integration` + staticcheck 固定 `-checks`，exit 0 |
| sec | 通过 | gosec + govulncheck；`No vulnerabilities found.`；所导入包受影响漏洞 0 |
| test（`-race -cover`） | 通过 | 全仓 ok；`internal/app` 104.905 s |
| test-integration | 通过 | 全仓 ok；`internal/app` **1085.197 s** / 1200 s（余量 114.8 s）、`storage/postgres` 834.338 s；无 53200、无回收超时、无 FAIL |
| cover | 通过 | `internal/app` **1125.969 s** / 1320 s（余量 194.0 s）；`check-coverage: 全部包达标` |
| build | 通过 | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`；`bin/basalt.exe` sha256 `f7ae196aa0b4ca81635ceac1a079f1f2736024949e96d77bf4739130df453287`，`go version -m` → `go1.27.3` |

覆盖率（`scripts/check-coverage.sh` 判卷，底线 70%：core / tool / memory / storage；其余 50%）：本轮 `make check` 读数 core **79.45%**、tool **77.52%**、memory **85.60%**、storage **73.87%**、app **74.84%**；`internal/app` 在 cover 中标签为 74.8%。没有降低阈值，也没有跳过无可执行语句以外的包。

门禁后只更新本验收报告、run-state、analyze 与 plan 的收口说明；产品代码、测试、stage 与契约在本报告落盘前已定稿，stage 指纹对这些文件有效。

**失败与重试（如实记录，不覆盖）**：清理 396 个残留 `basalt_pgtest_*` schema 之后的第一轮 `make check`（`.loop/us21/make-check-review2i-20261105.log`）在 23:11–23:30 普发 `pgtest.go:149 … timeout: context deadline exceeded`，`internal/app`（1200.789 s）与 `internal/storage/postgres`（1200.295 s）撞 20m panic，随后 `go test` 空转 50 分钟被停止。清理后复测同一夹具 `3.972 s` 通过，判定为批量 DROP 后的瞬时 DDL 退化，非代码缺陷；重跑即上表读数。完整台账见 [run-state](run-state.md) §2。

### 1.2 修复轮第一轮读数（2026-11-05，历史，不作本轮证据）

分支 `021-learning-admission`，基线 commit `83678a06aa60363bfd63387e02f013c8b6f830f4`（US-20 收尾），工作树 `dirty=true`（US-21 改动未提交，属本 US 全部交付面）。全量 stage 报告 `run_id=accept-20261105T024043.645871300`，`status=passed exit_code=0`，源码指纹 `3a387558101f955455c89bd0d82b590c36a1f9308eda1dfa82d9abdcc8deb9c1`，`started_at=2026-11-05T02:40:44Z`（本机 2026-11-05 10:40），总用时 2144.1 s，`tested_binary_sha256=c9eb0d1482265d7492413477f13b680cd947a0ca935427a74a27623679ab89a4`。

报告与逐 check 日志均在 [.loop/runs/accept-20261105T024043.645871300](../../.loop/runs/accept-20261105T024043.645871300/)；stage 终端日志 `.loop/us21/final-acceptance-us21-review2d-20261105.log`（UTF-16LE）。`tested_binary_sha256=c9eb0d14…` 是 e2e 用例自建候选二进制的标记（`binary_source=temp`），不是 `bin/basalt.exe`；发布构建产物单独记录在附录 B。失败历史保留在 run-state §2，不用后续通过覆盖更早的失败。

| 命令/检查 | 四态 | 本轮读数 | 证据 |
| --- | --- | --- | --- |
| full：fmt | 通过 | 4.2 s / exit 0 | fmt.log |
| full：lint（`-tags=integration` vet + staticcheck 固定 `-checks`） | 通过 | 19.2 s / exit 0 | lint.log |
| full：sec（gosec + govulncheck） | 通过 | 31.9 s / exit 0；`No vulnerabilities found`；所导入包受影响漏洞 0、所需模块内 2 个当前未调用的提示保留 | sec.log |
| full：test（race，全仓） | 通过 | 149.1 s / exit 0 | test.log |
| full：test-integration | 通过 | 682.9 s / exit 0；`internal/app` 单包 669.1 s（旧 600 s 内层默认值下必 red，见 §4 第 8 条） | test-integration.log |
| full：test-pg（race） | 通过 | 380.6 s / exit 0 | test-pg.log |
| full：cover | 通过 | 707.4 s；`internal/app` 653.7 s / 74.83%，全部有语句包达标 | cover.log |
| full：build（`CGO_ENABLED=0`） | 通过 | 6.3 s / exit 0；产物 stamp `go1.27.3`（`go version -m bin/basalt.exe`） | build.log |
| full：eval-build / behavior / injection | 通过 | 4.2 / 4.4 / 3.1 s，均 exit 0；behavior 为 5 PASS / 1 SKIP | 同名 log |
| behavior：outcome-real-model-answer | 跳过 | 未给 `--provider`；遵守 2026-11-04「后续 US 自动验收不得调用真实模型」裁决，属 `harness/acceptance.json` 允许的 skip | behavior.log |
| US21 base stage | 通过 | 10 checks、**125** 个必跑条目全部 PASS，必跑项零 FAIL/SKIP | report.json |
| stage：console test / build | 通过 | 40 文件 / 478 测试；构建 exit 0（integrity 4 文件） | console 两条 log |
| make check（七目标：fmt / lint / sec / test / test-integration / cover / build） | 中断（无结果） | 该轮在 `test` 目标中途停止（日志 63 行，止于 `internal/adapters/cron`），未产生七目标结论，**不作任何证据**（`.loop/us21/make-check-review2-20261105.log`） | 同上 log |

覆盖率（`scripts/check-coverage.sh` 判卷，底线 70%：core / tool / memory / storage；其余 50%）：core 79.45%、tool 77.52%、memory 85.60%、storage 74.02%、storage/postgres 75.36%；app 74.83%、cli 82.21%、web 85.18%、workspace 75.86%、isolation 89.66%。没有降低阈值，也没有跳过无可执行语句以外的包。

门禁后只更新本验收报告、run-state、analyze 与 plan 的收口说明；产品代码、测试、stage 与契约在本报告落盘前已定稿，stage 指纹对这些文件有效。

### 1.3 首轮读数（保留，不作修复后证据）

首轮全量 stage 报告 `run_id=accept-20261104T232550.282100700`，`status=passed exit=0`，源码指纹 `1b1b77052c3d68c39fb476ba45b26a4b70c8dc388c2c23d4037e5c73c8472084`，`tested_binary_sha256=c11a38445a643f230b36a431bff1823a9d6a632271315e0c0bcb0f6cac175f09`，10 checks / 116 必跑条目全绿；`make check` exit 0（`.loop/us21/make-check-20261105.log`）；覆盖率 core 79.45% / tool 77.52% / memory 85.60% / storage 73.87% / postgres 75.36% / app 74.77% / workspace 75.82%。失败轮次（attempt 1~5）与各自原因见 run-state §2。

该轮绑定的是 **review ② 退回前**的源码，保留用于失败历史与前后对照；它**不能**证明 R-03、F-04 与四项必解发现（含修复轮三处残留）已解决——那些结论只由 §1.1 本轮读数承载。

## 2. Harness 与逆改判别力

`harness/stages/us21-base.json` 共 10 个 check；必跑条目合计 **125**（core 16 / workspace 13 / app 63 / pg 14 / web 5 / cli 5 / e2e 5 / race 4）。report.json 中 125 个具名条目均有 PASS 事件，必跑项零 FAIL / SKIP（逐条比对：缺失 0、SKIP 0）；各 check 同时以 `-run ^TestUS21_` 收进同包其它 US-21 用例，一并全绿。逐 check 读数见附录 A。

**修复轮第一批新增逆改探针 7 条（M5～M11）**，均为仓库外 Go `-overlay` 副本执行（原文件未改），要求目标断言逐字失败而不是只看退出码：

| ID | 逆改点 | 四态 | 红态读数（逐字） | log |
| --- | --- | --- | --- | --- |
| M5（R-03） | `BuildAdmissionPolicy` 不把 `isolation.profiles.*` 纳入哈希 | 通过 | `TestUS21_Policy_IsolationProfileChangesHash`：`Should not be: "941bb226a45dfad5814916fa55ce716aedf5b8bde380872dd0fbc7f46317e8cc"`（Messages：镜像变化必须改哈希） | red-policy-hash-20261105.log |
| M6（R-03） | 同上，落在发布闸门 | 通过 | `TestUS21_Policy_IsolationProfileDriftRejectsApproval`：`Should not be: "1afba8bdace83d2a6e2e1bc943c20386f6019fe10e240a28da4cb5753b250740"`（Messages：改档位必须改哈希），5.27 s | red-policy-drift-20261105.log |
| M7（发现 2） | `admissionScripts` 还原为只认扩展名 | 通过 | `TestUS21_Evaluate_ExtensionlessScript`：`expected: []string{"bin/runner", "scripts/collect"} actual: []string{}`；`TestUS21_Approve_ExtensionlessScriptRequiresReview`：`expected: []string{"scripts/collect"} actual: []string{}` | red-eval-ext / red-approve-ext-20261105.log |
| M8（残留 1） | `Propose` 对提交未知仍走删目录分支 | 通过 | `TestUS21_Propose_CommitUnknown_KeepsSnapshot`：`expected: "unknown" actual: "failed"` | red-propose-unknown-20261105.log |
| M9（残留 2） | 撤回「被超越」分支把提交未知记 `failed` | 通过 | `TestUS21_Withdraw_SupersededCommitUnknownRecorded`：`expected: "unknown" actual: "failed"` | red-withdraw-superseded-unknown-20261105.log |
| M10（残留 3） | `path.Ext` 末尾点漏判（`collect.` 不视为脚本） | 通过 | `TestUS21_Evaluate_TrailingDotScript`：`expected: []string{"scripts/collect."} actual: []string{}` | red-trailing-dot-20261105.log |
| M11（发现 4） | 撤回补偿失败记 `failed` 而非 `unknown` | 通过 | `TestUS21_Withdraw_CompensationFailureRecordedUnknown`：`expected: "unknown" actual: "failed"`（Messages：补偿失败必须记 unknown） | red-withdraw-unknown-20261105.log |

**修复轮第二轮新增逆改探针 7 条（M12～M18）**，同样为仓库外 `-overlay` 副本（探针目录 `%TEMP%\us21-redprobe-fix2`，用毕按字面路径删除；原文件未改）：

| ID | 逆改点 | 四态 | 红态读数（逐字） |
| --- | --- | --- | --- |
| M12（F-01） | `WriteSkillWorkCopy` 的 `errors.Join` 去掉 `ErrWorkCopyUncertain` 哨兵（仍保留说明文案） | 通过 | `TestUS21_WriteSkillWorkCopy_RollbackFailureIsUncertain`：`Target error should be in err chain: expected "技能工作副本处于不确定状态"`；chain 列出的两条 join 错误与 detail 文案都在，证明判别力靠哨兵而不是靠文案子串；Messages「回滚失败必须可识别为不确定状态」 |
| M13（F-01） | `Publish` 的哨兵分支改回 `finishFailed` | 通过 | `TestUS21_Publish_WorkCopyUncertainRecordedUnknown`：`expected: "unknown" actual: "failed"`（Messages「工作副本不确定必须记 unknown」） |
| M14（F-01） | `prepareWithdrawVersion` 的哨兵包装改回直接返回原错误 | 通过 | `TestUS21_Withdraw_WorkCopyUncertainRecordedUnknown`：`expected: "unknown" actual: "failed"`（Messages「工作副本不确定必须记 unknown」） |
| M15（F-02） | 删 `Propose` 的 `AdmissionCandidateDirExists` 分支（退回一律 `finishFailed`） | 通过 | `TestUS21_Propose_FreezeUncertainKeepsDir`：`expected: "unknown" actual: "failed"`（Messages「目录已就位必须记 unknown」） |
| M16（F-03） | 删 `normalizeSkillFilePath` 的段首尾空白判定 | 通过 | `TestSkillImport_EdgeWhitespace` 五个子例全部 `An error is expected but got nil.`（用例：给 `scripts/run.sh` 加尾随空格、给 `scripts/collect.` 加尾随空格、给 `notes.md` 加前导空格、目录名 `dir` 后接空格、`docs` 与 `guide.md` 之间留空格；定位 `skill_admin_test.go:70 → :223`） |
| M17（F-03） | `admissionScripts` 分类前不再 `TrimRightFunc(unicode.IsSpace)` | 通过 | `TestUS21_Evaluate_TrailingSpaceScript`：`expected: []string{"scripts/collect. ", "scripts/run.sh "} actual: []string{}` |
| M18（F-04） | 剥掉 `AdmissionIsolationProfile` / `AdmissionDomainPolicy` / `AdmissionNetworkException` 的 `json` tag | 通过 | `TestUS21_Policy_HashInputMatchesContractNames`：实际串含 `"isolation_profiles":[{"Name":"standard",…}`、`"domain_policies":[{"Scope":…`，`does not contain "\"isolation_profiles\":[{\"name\":\"standard\""` |

首轮新增探针 4 条（M1～M4，保留、不重跑），均在仓库外 Go `-overlay` 副本执行（探针目录 `%TEMP%\us21-w10-probe` 用毕按字面路径删除）：

| ID | 逆改点 | 四态 | 红态读数 |
| --- | --- | --- | --- |
| M1（D-F-04 A） | `prepareWithdrawVersion` 两处比对前加 `false &&`（跳过内容哈希与清单比对） | 通过 | `改写上一版正文` / `清单外多文件` 逐字红：`Expected error with "admission candidate mismatch" in chain but got nil`；`登记目录属于其他技能` 仍绿（证明归属分支独立） |
| M2（D-F-04 B） | `LoadPublished` 失败不再包成 mismatch（`return ..., err`） | 通过 | `登记目录属于其他技能` 逐字红：`in chain: "技能 weekly-report 的发布目录 \".basalt/versions/skills/other-skill/1\" 属于技能 other-skill，拒绝读取"`；另两子例仍绿 |
| M3（D-F-02 app） | 派生态谓词 `!now.Before(expires_at)` 改为 `now.After(expires_at)`（开区间） | 通过 | `TestUS21_State_ExpiredPredicateGuards/等值边界即_expired`：`expected "expired" / actual "approved"` |
| M4（D-F-02 web） | 同一逆改落在 `admissionDisplayState` | 通过 | `TestUS21_AdmissionList_DerivedExpired`：`expected: "expired" / actual: "approved"`，`Messages: now == expires_at 即过期` |

任务级红态探针（T007 / T008 / T010 / T011 / T013 / T014 / T018 / T019 / T020 / T021 / T022 / T023 / T025 / T025b / T025c / T025d / T025e）的逐条读数与探针目录已记在 run-state §1.3 对应任务行；本轮不重跑、不把历史读数冒充本轮读数。

## 3. 交付物核对

逐路径存在性与行数（`git ls-files --others --exclude-standard` + `wc -l`，本轮当场读数）：

| 交付组 | 落点（行数） | 核对结果 |
| --- | --- | --- |
| core | `admission.go` 565、`admission_json.go` 859、`admission_json_test.go` 584 | 通过；`core` 仍只依赖标准库；`AdmissionPolicy` 增 `IsolationProfiles`（R-03） |
| workspace | `admission_store.go` 684、`admission_store_test.go` 536；技能面 `skill_admin.go`（本轮 +`ErrWorkCopyUncertain` 与段首尾空白判定）、`skill_admin_uncertain_test.go` 65（新增）、`skill_admin_test.go` 597 | 通过；冻结树只读、拒绝路径有专例；工作副本双失败哨兵有专例 |
| postgres | `admission_store.go` 1207、`admission_store_test.go` 768、`migrations/0010_skill_admission.sql` 146 | 通过；0001–0009 零改写（`git diff -- migrations` 为空）；两处 `Commit` 失败统一包装 `ErrAdmissionCommitUnknown` |
| app（生产） | `admission_admin.go` 1798、`admission_eval.go` 572、`admission_port.go` 363、`admission_refs.go` 164、`admission_secret.go` 93 | 通过；`VerifyOperation` 在 `admission_admin.go`（F-06 登记）；`writeWorkCopy` / `writeCandidate` / `withdrawAdmission` 三个注入点均有生产默认值 |
| app（测试） | `admission_admin_test.go` 642、`admission_approve_test.go` 646、`admission_approve_unit_test.go` 98、`admission_audit_test.go` 414、`admission_cancel_test.go` 229、`admission_eval_test.go` 553、`admission_port_test.go` 181、`admission_publish_test.go` 695、`admission_recovery_test.go` 280、`admission_refs_test.go` 97、`admission_secret_test.go` 120、`admission_withdraw_test.go` 565 | 通过；修复轮两批共 14 条具名回归（§2 M5～M18） |
| cli | `admission.go` 1058、`admission_test.go` 639 | 通过；七个写命令 + 两个读命令，无 `--approver` 旁路 |
| web | `handlers_admissions.go` 356、`handlers_admissions_test.go` 668、`contracts_admissions_test.go` 263 | 通过；两条只读路由在 `server.go`（F-02 登记） |
| e2e | `us21_admission_test.go` 855 | 通过；五条全链路 / 并发 / 恢复 / 只读面 |
| console | `views/admissions.ts` 155、`views/admission-detail.ts` 291、`tests/admissions.test.ts` 326 + 重建 dist 4 文件 | 通过；无写控件、无 token 存储 |
| 契约 | `docs/contracts/skill-admission.md` 162；`openapi.yaml` / `run-and-errors.md` 追加 | 通过；`TestUS21_OpenAPIContract` 双向对账；`policy_hash` 与末尾点条款按 R-03 / 发现 2 同步 |
| Harness | `harness/stages/us21-base.json` 510 | 通过；114 → 116 → 125 均为追加，无删改条目 |
| 公共门禁时限（用户 2026-11-05 裁决） | `scripts/check-coverage.sh` +`-timeout=15m`；`harness/acceptance.json` test-integration `-timeout=12m`、外层 600→900 s；`Makefile` test-integration `-timeout=12m` | 通过；只放宽时限，断言 / 包范围 / 阈值零变更（§4 第 8 条） |

前序文件被触碰（非新增）：`internal/storage/postgres/connect.go`、`resource_store.go`、`migrate_integration_test.go`、`observability_migration_test.go`；`internal/app/resource_admin.go`、`baseline.go`、`wire.go`、`run.go`、`web.go`；`internal/web/server.go`、`views.go`、`handlers_operations.go`、`contract_test.go`；`internal/cli/ports.go`、`deps.go`、`root.go`；`assets/console/src/api.ts`、`app.ts`、`router.ts`、`views/operations.ts` 与 dist；`docs/contracts/openapi.yaml`、`run-and-errors.md`；`docs/Operations.md`（私有正文，按快照核对）；`Makefile`、`scripts/check-coverage.sh`、`harness/acceptance.json`（门禁时限，§4 第 8 条）。均为已批准接线、随增枚举或用户裁决的时限调整，不新增第二套写入 / 发布 / 审计路径。

实现与模块卡的差异（analyze 已登记，逐条不改写已勾选任务正文）：F-01 冻结就位为单次 `Rename`（可观察契约不变）；F-02 只读路由落在 `server.go`（卡允许二选一）；F-03 实际改动文件以 `plan.md` 按实测更正为准；F-06 `VerifyOperation` 落 `admission_admin.go`；F-07 T004 措辞「八类结构体」与实现（20 struct + 7 枚举）口径差；F-04 `AdmissionPort` 12 方法 vs 卡列 9（review ② 决策项）；F-05 软链接用例在 Windows 以目录联接回退实跑（见 §6）。

## 4. 前序回归与判据面

前序回归：full 的 `test`（全仓 race）、`test-integration`（全仓 `-tags=integration -count=1`）、`test-pg`（race）三项 exit 0，覆盖 US-1～US-20 全部既有用例；US-21 定向复跑 `go test -race -tags=integration -count=1 -run '^TestUS21_' ./internal/app` 于 2026-11-05 编辑后重取读数 `ok ... 15.344s`（无 data race）；e2e 五条在 stage 中同轮通过。

判据面变更（逐条给依据，无删除、无放宽）：

1. `harness/stages/us21-base.json` 的 `us21-base-app.required_tests` 114 → 116：只追加 `TestUS21_Withdraw_SnapshotFailureKeepsPublished` 与 `TestUS21_Withdraw_TamperedPublishedContentRejected`（两者本就存在并在宽 `-run ^TestUS21_` 下执行）。依据：delta 复核 D-F-03；只增不减。
2. `TestUS21_Withdraw_TamperedPublishedContentRejected` 增第三子例「登记目录属于其他技能」（数据库篡改 `storage_path`）：新增判据，原两子例不动。
3. `TestUS21_AdmissionList_DerivedExpired` 从 4 夹具扩到 7 夹具、新增等值边界 / superseded / reject / 零有效期断言与 approved 过滤剔除断言：只增断言。
4. 新增 `TestUS21_State_ExpiredPredicateGuards`（app 侧 8 分支纯函数用例）：新增判据。
5. `prepareWithdrawVersion` 把 `LoadPublished` 失败统一包成 `ErrAdmissionCandidateMismatch`（保留原错误链）：失败仍 fail-closed，错误分类更严格；契约同步写明。
6. `docs/contracts/skill-admission.md` 第 86 行重写：把存储层实际校验项、两层都不校验 `SupersededAt` 的状态机保证、基线方向写准；纯文档，不改运行行为。
7. 前序随增适配（已有失败台账）：`0010` 迁移使 `TestPostgres_FreshSchemaIsV1` 版本链 / 表数期望与 `TestObservability_MigrationForwardOnly` 常量同步到 v10 —— 常量随迁移推进，验收语义不变。
8. **公共门禁时限（2026-11-05 用户裁决，授权修改 cover 与 test-integration 两处时限并核对内层 / 外层）**：`scripts/check-coverage.sh` 的 cover 判卷补 `-timeout=15m`（外层 harness 预算 1200 s 不变）；`harness/acceptance.json` 的 test-integration 补 `-timeout=12m`、外层 `timeout_seconds` 600→900；`Makefile` 的 test-integration（`make check` 走的同一命令）补 `-timeout=12m`。依据读数：`internal/app` 单包 `-cover` 512.9 s 通过（74.8%），并行 `go test ./...` 下 `panic: test timed out after 10m0s` / FAIL 601.231 s（`.loop/us21/cover-retry2-20261105.log`、`.loop/us21/app-cover-timing-20261105.log`）；修复后本轮 test-integration 的 app 包 669.1 s、cover 的 app 包 653.7 s——按旧 600 s 默认值两项都会红。内层均小于外层（12m < 900 s；15m < 1200 s），不会被内层提前终止。断言、必跑清单、包范围与覆盖率阈值零变更，不新增跳过。
9. `TestUS21_Publish_UsesSharedPrimitives` 的源码钉字面量 `workspace.WriteSkillWorkCopy(` 改为 `workspace.WriteSkillWorkCopy`（去尾随括号）：F-01 的注入缝把该原语从直接调用改为函数值形参（`writeWorkCopy: workspace.WriteSkillWorkCopy`），判据仍是「编排面必须引用共享原语」，只有语法形态适配；其余五个钉子（`ValidateSkillDraft(` / `SnapshotSkill(` / `PublishAdmission` / `gate.Lock()` / `abortSkillCommit(`）与两条 `NotContains` 零变更。是随已批准修复的等价适配，不是放宽。
10. 契约 `docs/contracts/skill-admission.md`：脚本判定补「路径段首尾空白一律拒绝」「分类前剥末尾空白是纵深防御」；恢复补「工作副本交换 + 回滚双失败记 `unknown`，不自动复原 / 清理」。纯文档与实现同步（M12～M18 承载判别力），不改运行语义之外的判据。
11. **迁移 advisory 锁按目标 schema 派生（2026-11-05 用户裁决「批准第一项，并授权同步相关契约说明与受保护测试」）**：`migrate.go` 的锁键由全库固定键改为 `migrationLockSeed + "/" + 目标 schema`（`migrationAdvisoryKeyFor`）；`migrationTargetSchema` 解析目标 schema 并拒绝 `search_path` 回退——`current_schema()` 为 NULL 或 `schema_version` 落在别的 schema 一律拒绝，迁移对象与版本表必须属于同一 schema。同一 schema 仍严格互斥（事务内取 `pg_advisory_xact_lock`、锁内重检版本、失败整体回滚），不同 schema 可并行。兼容边界写进代码注释与契约：旧版固定键与新版按 schema 键**不互斥**，升级必须停旧启新；已发布迁移零改写（`git diff -- migrations` 为空）。判据面：`TestPostgres_MigrateTakesAdvisoryLock` 改为按目标 schema 判卷，新增 `TestPostgres_Migrate_DifferentSchemasRunInParallel` 与 `TestPostgres_Migrate_RejectsVersionTableOutsideTargetSchema`（同 schema 互斥 / 不同 schema 并行 / 回滚各有用例）。
12. **夹具准备按「对象计锁」限流（测试基础设施，不改产品语义）**：`internal/storage/postgres/pgtest/pgtest.go` 的跨进程槽位闸门定档为**迁移 3 路 + 回收 1 路**——会话级 advisory 锁 + 服务端阻塞排队，合计 3 × 1,136 + 1,597 = 5,005 条锁，占本机测试库 6,400 条预算的 78%；`OpenMigrated` 让「自建 schema 再自己 `postgres.Open`」的用例走同一闸门。依据是 2026-11-05 实测：按 schema 派生锁键后几十个 `t.Parallel()` 夹具真正并行，`pg_locks` 峰值 17,834 行 / 22 个会话，报 `ERROR: out of shared memory (SQLSTATE 53200)`（两轮全量 stage 的 cover 195 + 177 + 46 与 20 次，另有 16 条回收失败留残留）。同批同源码（1,724 条用例 = 1,086 顶层 + 638 子用例）整包跑 `internal/app` 的路数对照：共用 1 路 878.95 s；1 迁移 + 2 回收 1,057.3 s（最慢）；2 迁移 + 1 回收 829.5 s（另有一次回收撞 10 s 上限假红）；**3 迁移 + 1 回收 802.7 s**（`Elapsed=802.742`，命令总 808.3 s，零失败、零 53200、零回收超时，迁移最大持有 6.06 s、回收最大持有 3.85 s）。3 迁移 + 2 回收需 6,602 条锁 > 6,400，**已排除**。等待分布实测：前 11 分钟合计 < 1 s，全部 1,997.5 s 集中在最后 3 分钟（+11/+12/+13 为 724 / 916 / 356 s）——顶层 `t.Parallel()` 要等串行用例跑完才并发启动。断言、必跑清单、包范围与覆盖率阈值零变更。
13. **公共门禁时限第二次调整（2026-11-05 用户裁决「同意方案 1，授权修改这两处公共门禁时限，并记录调整理由及原始耗时证据」）**：`scripts/check-coverage.sh` 的 cover 内层 15m → **17m**（1020 s，外层 1200 s 不变）；`harness/acceptance.json` 与 `Makefile` 的 test-integration 内层 12m → **14m**（840 s，外层 900 s 不变）。依据读数：12m 轮 `internal/app` 在 720.973 s 撞内层超时（同轮 cover 的 app 包 900.147 s 贴边界通过，说明测试全绿只是慢）；14m 轮 app 直接实测 840.565 s 超时、同轮 cover app **881.749 s** 通过；随后整包单跑实测 802.7 s（定档 3 迁移 + 1 回收，见上条）。内层均小于外层（14m = 840 s < 900 s；17m = 1020 s < 1200 s），不会被内层提前终止。本轮未再上调任何时限。断言、必跑清单、包范围与覆盖率阈值零变更，不新增跳过。
14. **公共门禁时限第三次调整（2026-11-05 用户裁决「批准有界调整测试时间预算」）**：test-integration 内层 14m → **20m**（1200 s），harness 外层 `timeout_seconds` 900 → **1800 s**，`Makefile` 的 test-integration 同步为 `-timeout=20m`（与验收入口同一份预算）；cover **维持 17m／1200 s 不动**（同轮实测通过，按用户裁决等读数再判）。依据读数：定档 3 迁移 + 1 回收后整包单跑 `internal/app` 802.742 s；全量 stage `accept-20261105T113150` 里 `internal/app` **840.776 s** 撞 14m 内层，同轮 `internal/storage/postgres` 655.147 s、`internal/dbmigrate` 171.599 s 并行争用，且前者的测试助手完全绕过夹具闸门。本轮只调测试运行预算——不修改产品超时、行为时延断言、必跑清单与覆盖率阈值，不跳过测试；资源不足、回收失败与断言失败仍判红。内层 20m = 1200 s < 外层 1800 s，不会被内层提前终止。
15. **补齐遗漏的夹具限流（2026-11-05 用户裁决「继续补齐遗漏的夹具限流，保留真正需要验证并发的测试能力」）**：闸门从 `pgtest` 下沉到 `pgtemp`（新文件 [slots.go](../../internal/storage/postgres/pgtemp/slots.go)，导出 `AcquireMigrationSlot` / `AcquireDropSlot`），`pgtest` 改为委托；`internal/storage/postgres` 的测试助手补上闸门——`knTestDB`（`portTestDB` 的底座）、`openMigratedTestDB`（新建，供 `connect_integration_test.go` 与 `tx_integration_test.go` 的 `Open` 调用点）、`createSchema` 的 `DROP SCHEMA ... CASCADE` 回收。下沉而不是复制，是因为 `pgtest` 反向导入 `postgres`，`postgres` 的测试无法导入它（循环）；`pgtemp` 只依赖标准库与 pgx，可被双方共用。**保留并发判据**：`migrate_integration_test.go` 的迁移用例（`migrationConn`）**不**走闸门，仍直接调用迁移路径——「同 schema 互斥 / 不同 schema 并行」的验证能力不变。实测（`-race`，`.loop/us21/testpg-gated-20261105.log`）：`internal/storage/postgres` **665.691 s** 通过，`53200` = 0、`out of shared memory` = 0、闸门超时 = 0。断言零变更。
16. **test-pg 显式预算（2026-11-05 用户裁决「加 -timeout=15m，外层 1200 s 不动」）**：`harness/acceptance.json` 的 test-pg 补 `-timeout=15m`。该 check 此前**没有显式 `-timeout`**，吃的是 Go 默认 10m——历史读数 565.7 s 通过、本轮 606.1 s 判红、补闸门后 665.691 s，长期贴着默认值。15m = 900 s < 外层 1200 s，余量 35%。只补显式预算，不改断言、包范围与覆盖率阈值。
17. **cover 预算第二次调整（2026-11-05 用户裁决「22m / 外层 1500s（推荐）」）**：`scripts/check-coverage.sh` 的 cover 内层 17m → **22m（1320 s）**，`harness/acceptance.json` 的 cover `timeout_seconds` 1200 → **1500 s**。依据读数：`accept-20261105T125343.344426200` 轮 cover 的 `internal/app` 撞 17m 内层时限 **1020.637 s** 后 panic，而同轮不带 cover 的 test-integration app 为 949.793 s（`-cover` 插桩约 +7.4% 越线）。22m 相对超时点留约 29% 余量；本轮实测 cover app **1275.078 s** / 1320 s。内层 22m = 1320 s < 外层 1500 s，不会被内层提前终止。断言、必跑清单、包范围与覆盖率阈值零变更。
18. **夹具槽位起点改连接 pid（静态门禁修复，测试基础设施）**：`pgtemp/slots.go` 首版用 `math/rand/v2` 的 `rand.IntN` 分散槽位起点，被 gosec **G404**（Severity HIGH）判定；改为 `sync/atomic` 轮转后触发 **G115**（`int → uint64` 溢出）。最终改为 `SELECT pg_backend_pid()` 取后端 pid 再对容量取模——零随机、零类型转换，并发连接（含跨测试进程）自然从不同槽位起扫。`make sec` 23.5 s 通过、vet / staticcheck / gofumpt 干净。只改测试夹具的排队策略，产品并发语义与任何断言零变更；`migrate_integration_test.go` 的 `migrationConn` 仍不过闸门，「同 schema 互斥 / 不同 schema 并行」的验证能力保留。
19. **D-24：`compaction.keep_recent_groups` 默认值（用户裁决，跨 US 落在本树）**：`enabled: true` 且键缺失时装载器取默认 **4**（`internal/cli/config.go` 的 `defaultKeepRecentGroups = 4`；`validateUS16Sections` 的 required 列表改为七键；显式 0 / 负数仍拒绝）；`config_test.go` 增 `TestLoadConfig_CompactionKeepRecentGroupsDefaultsToFour` 四子例，`wire_compaction.go` 仅注释、`basalt.yaml.example`、`docs/TechnicalDesign.md` §2.5.3 与 `specs/016` R175 同步。**证据边界（用户 2026-11-05 明确）**：该改动落在本轮 `make check` 进行中，**不得**为它单独重跑全量 stage 与 make check，随 US-21 一并提交；因此 stage 指纹 `635080e5…` **不含**该改动，只有 §1.1 的 `make check`（树哈希 `5aee733b…`）覆盖它。`specs/016` R175 自述「make check 本轮未跑，须在合入前补」与本裁决冲突，以用户裁决为准，不以该自述另行扩大验证。判据面：新增四条子例断言，不删改既有断言。

## 5. 全局不变量自查（机器可判，本轮当场读数）

| 不变量 | 复核命令 | 读数 |
| --- | --- | --- |
| 无 Agent 框架 / ORM 坐标 | `grep -nE 'langchain\|langgraph\|eino\|agent-framework\|gorm\|ent \|sqlx\|sqlboiler' go.mod` | 无命中 |
| 非测试代码不用 `time.Local` | `grep -rn 'time.Local' --include='*.go' internal cmd \| grep -v '_test.go'` | 无命中 |
| `context.WithValue` 只装追踪标识 | `grep -rn 'context.WithValue' --include='*.go' internal cmd` | 仅 `internal/web/middleware.go:41`、`:79` 的 request ID（另有测试文件与一条禁止性注释） |
| 随机源红线 | `grep -rn 'math/rand' --include='*.go' internal cmd` | 三条均为「禁止 math/rand」的注释，无代码使用 |
| `.basalt/admissions` 不在 Agent 白名单 | stage `TestUS21_Policy_AdmissionsDirDenied` | 通过（读写均以 `SandboxViolationError` 拒绝） |
| 零新增依赖 | `git diff -- go.mod go.sum` | 0 行 |
| 单二进制 | `make build`（`CGO_ENABLED=0`） | 通过；`go version -m bin/basalt.exe` → `go1.27.3` |
| 迁移只追加 | `git diff --stat -- internal/storage/postgres/migrations` | 空（`0010` 为新增文件） |

## 6. 复核结论与剩余人工项

- analyze 复跑两轮：首轮（2026-11-05，CRITICAL 0，F-01～F-10 逐条处置）与 review ② 修复轮增量（[analyze.md](analyze.md) §6，CRITICAL 0 / HIGH 0）。
- 独立复核：首轮四轮（T025b / T025c / T025d / delta `ae8134a035a4b5c63`）；review ② 退回后的修复轮独立上下文复核在**同一批准范围**内再发现三处同类残留（F1.2 HIGH：`Propose` 提交未知仍删冻结目录；F1.3 LOW：撤回「被超越」分支提交未知记 `failed`；F2.2 LOW：末尾点文件名绕过脚本判定），均按红态先行修复并留红 / 绿读数（§2 M8～M10）。
- 修复轮第二轮独立上下文复核（同一批准范围）：F-01（阻断）工作副本交换 + 回滚双失败未记 `unknown`、F-02 冻结候选目录已就位仍按 `failed` 收尾、F-03 路径段首尾空白绕过脚本判定与身份分离、F-04 `policy_hash` 输入结构体缺 snake_case `json` tag——四项全部修复并各配红 / 绿读数（§2 M12～M18）；同轮把 `TestUS21_Publish_UsesSharedPrimitives` 的源码钉字面量随 F-01 注入缝做等价适配（§4 第 9 条）。未处置阻断：无。
- review ② 退回时的四项必解发现 + R-03 + F-04 处置状态：四项发现全部修复并各有红 / 绿读数（§2 M5～M11）；R-03 按用户裁决落地——`isolation.profiles.*` 十个执行边界字段进入 `policy_hash`，回归覆盖「评估 / 批准后修改同名档位，旧批准发布被拒绝」；F-04 用户明确「接受」，登记为**已接受**偏差。
- **AI 结论：全量 stage 21/21 通过（`accept-20261105T134838.179213700`）与 `make check` exit 0 均已落在修复轮第二轮的源码上（§1.1）；红 / 绿读数齐备；当前待人工 review ② 复验**。不写成人工已接受。**证据边界**：stage 指纹 `635080e5…` 早于 D-24，D-24 只由本次 `make check` 与定向子例覆盖（用户裁决不单独重跑，§4 第 19 条）；U1～U8 之外的 U3、F-05 仍未验证（§6 表）。

review ② 待裁决 / 待人工执行项：

| # | 项 | 状态 | 说明 |
| --- | --- | --- | --- |
| U3 | 高风险双人分离（两个可解析 OS 用户） | 未验证（保留） | 单用户主机缺第二 OS 用户；由 `TestUS21_HighRisk_SeparationPasses` 等覆盖，人工双用户复核保留 |
| U4 | 管理台只读页人工走查 | 已验证（2026-11-05） | 用户在本机实例完成人工走查并明确回复「人工走查pass」；页面为只读列表 / 详情投影、无写控件；自动化面由 `NoWriteEndpoints` / `ConsoleReadOnly` 覆盖 |
| U5 | 备份纳入 `.basalt/admissions` | 已验证（2026-11-05） | 实跑备份 / 恢复演练（隔离 schema `us21_u5`，PostgreSQL 18.6 + `pg_dump -Fc` / `pg_restore`）：灾变后 82 表与工作区 `.basalt` 均恢复，`status`「全部通过」、`admission list --state published` 与 `show --files` 读到正文（`.loop/us21/u5-backup-drill-20261105.log`）。诚实边界：演练库为共享测试库中的专用 schema（角色无 CREATEDB），不是独立生产实例；dump / restore 用 WSL 内 PG18.6 客户端 |
| U6 | 迁移 v9 → v10（旧库前向、重复启动幂等） | 已验证（2026-11-05） | `TestPostgres_FreshSchemaIsV1` 逐版应用 v1→v10 并对整链幂等重放；`TestUS21_Migration_PartialUniqueIndex` 钉住 0010 的部分唯一索引。诚实边界：链式用例从空库起逐版应用，不是「保留 v9 快照再升级」的独立夹具 |
| U7 | 单二进制 / 零新增依赖 | 已验证（2026-11-05） | §5 的 build 与 go.mod 读数；本轮 `bin/basalt.exe` stamp `go1.27.3` |
| U8 | 全量门禁 + 全量 stage | 已验证（2026-11-05 / 11-06） | §1.1：全量 stage `accept-20261105T134838.179213700`（21/21 check、125 必跑全绿，指纹 `635080e5…`）；`make check` exit 0（树哈希 `5aee733b…`，含 D-24）。**边界**：stage 指纹不含 D-24，见 §1.1 与 §4 第 19 条；旧轮读数保留在 §1.2 / §1.3 |
| R-03 | `policy_hash` 是否纳入 `isolation.profiles.*` 档位限额 | 已按裁决关闭（2026-11-05） | 用户裁决「要改」：`AdmissionPolicy` 增 `IsolationProfiles`（镜像 / 内存 / PID / 超时等十个执行边界），CLI 从 `cfg.Isolation.Profiles` 投影，`BuildAdmissionPolicy` 稳定排序后进入哈希；契约同步；M5 / M6 逆改红态证明判别力 |
| F-04 | `AdmissionPort` 12 方法 vs 模块卡 9 方法 | 已接受（2026-11-05） | 用户明确「F-04 接受」：保留九个业务方法 + 三个只读辅助方法（`ImportFiles` / `ReadSourceFile` / `ReadDecisionFile`），文件校验唯一实现；偏差登记在 analyze F-04 与本表 |
| F-05 | 真实符号链接分支的平台覆盖 | 部分覆盖 | Windows 以目录联接实跑「特殊文件」拒绝分支；真实符号链接（「符号链接」消息）需 Linux CI 或有权限环境复核，不写成已验证 |
| — | 真实模型 / 外部网络 / 候选脚本执行 | 未授权 | 按 2026-11-04 用户裁决不做；`effect` / `cost` 恒 `not_evaluable` |

## 7. 复跑与交接

```sh
# 1) 全量 stage（本机需 dev-env PG；Windows 经包装器注入 DSN）
.\scripts\check-acceptance.ps1 --stage harness/stages/us21-base.json
sh scripts/check-acceptance.sh --stage harness/stages/us21-base.json   # Git Bash / Linux

# 2) 宪法口径七目标
make check

# 3) 定向回归
go test -race -tags=integration -count=1 -run '^TestUS21_' ./internal/app
go test -tags=e2e -count=1 ./internal/e2e -run '^TestUS21_'
npm --prefix assets/console test && npm --prefix assets/console run build

# 4) 不变量与依赖
git diff -- go.mod go.sum; git diff --stat -- internal/storage/postgres/migrations
go version -m bin/basalt.exe        # 期望 go1.27.3
```

恢复位置：人工 review ②。先看本文 §6 待裁决项与 [run-state](run-state.md) §4 未验证清单；不自动提交、不推送、不把本报告当作用户收口决定。

## 附录 A：逐项必跑读数

读数取自 `accept-20261105T134838.179213700`（[report.json](../../.loop/runs/accept-20261105T134838.179213700/report.json) 的 `duration_ms`；`passed_tests` 为该 check 实际通过的用例数，含同包 `-run ^TestUS21_` 收进来的用例）。

| check | 必跑条目 | 具名条目命中 | 本轮读数 | log |
| --- | --- | --- | --- | --- |
| us21-base-core | 16 | 16/16 PASS（本 check 共 65 条用例通过） | 1.1 s / exit 0 | us21-base-core.log |
| us21-base-workspace | 13 | 13/13 PASS（含三条软链接 / 联接拒绝；共 33 条通过） | 2.7 s / exit 0 | us21-base-workspace.log |
| us21-base-app | 63 | 63/63 PASS（含 review ② 新增 9 条：策略哈希 2、脚本判定 3、提交未知 4；共 121 条通过） | 63.5 s / exit 0 | us21-base-app.log |
| us21-base-pg | 14 | 14/14 PASS（含迁移与部分唯一索引；共 24 条通过） | 31.2 s / exit 0 | us21-base-pg.log |
| us21-base-web | 5 | 5/5 PASS（含 `DerivedExpired`、`NoWriteEndpoints`、`OpenAPIContract`；共 30 条通过） | 1.1 s / exit 0 | us21-base-web.log |
| us21-base-cli | 5 | 5/5 PASS（共 11 条通过） | 1.8 s / exit 0 | us21-base-cli.log |
| us21-base-e2e | 5 | 5/5 PASS | 19.0 s / exit 0 | us21-base-e2e.log |
| us21-base-console-test | — | 40 文件 / 478 测试 | 11.0 s / exit 0 | us21-base-console-test.log |
| us21-base-console-build | — | 构建 + integrity 4 文件 | 6.5 s / exit 0 | us21-base-console-build.log |
| us21-base-race | 4 | 4/4 PASS | 8.2 s / exit 0 | us21-base-race.log |

必跑条目合计 125（16 + 13 + 63 + 14 + 5 + 5 + 5 + 4），零 FAIL / 零 SKIP、逐条命中；
stage 之外另有同包 `^TestUS21_` 用例同时通过（app 91 个顶层、web 8 个、cli 11 个、
workspace 13 个、core 16 个、pg 22 个）。app 的 63 条较上轮 54 条新增 9 条，全部
为本轮修复回归：`Policy_IsolationProfileChangesHash` /
`Policy_IsolationProfileDriftRejectsApproval`、`Evaluate_ExtensionlessScript` /
`Approve_ExtensionlessScriptRequiresReview` / `Evaluate_TrailingDotScript`、
`Propose_CommitUnknown_KeepsSnapshot` / `Publish_CommitUnknown_KeepsSnapshot` /
`Withdraw_SupersededCommitUnknownRecorded` /
`Withdraw_CompensationFailureRecordedUnknown`。

## 附录 B：工件指纹（SHA-256）

| 工件 | 值 |
| --- | --- |
| 模块卡 `docs/us/us21-learning-admission.md`（review ② 修复轮第二轮追加后，2715 行） | `751cb573e5dfd8230609726365083641ae3154ba31ef074b7f904dfb9a8ceb4e` |
| `spec.md`（本轮未改） | `6f76656a75354a312367c48c7f96e0db0ee9df4d433511002c01b792bc9c36ba` |
| `plan.md`（R-03 补正后） | `94fe63b994e7d3815ca521e675595cfd39488ff4ec69ebef72d10750c64dc9ed` |
| `tasks.md`（T026 / T027 已勾选） | `f1edf2848adeab2905d7546840c3d05e2e33dfe407c2d50689ac3c9c26130c11` |
| `analyze.md`（含 §6 修复轮、§7 修复轮第二轮复跑） | `79542d8066ccafbe803927437226a84e96a9edaa2e6465b81aa1b6bdb2b82663` |
| `docs/contracts/skill-admission.md`（R-03、脚本判定与恢复同步，162 行） | `05b68e9a3ee5932fee1c7968d8f299fe8cfbc29f4bb265d8ac71350834a9b2c7` |
| `harness/stages/us21-base.json`（116 → 125 后） | `4dd9673856464590fc0c9f65282faf314c32dd66cbafc1d423c286bd06d9c0c0` |
| `scripts/check-coverage.sh`（cover `-timeout=22m`，§4 第 17 条） | `0f2817eb785a26eaffb2e8a56fb4ab780b4eb4fa7301ec6a61807e85fb73b98e` |
| `harness/acceptance.json`（test-integration `-timeout=20m` / 1800 s；test-pg `-timeout=15m` / 1200 s；cover 1500 s） | `699e6804edb154d683fde850f9694ef0e72c5f2d02d68ce8a7ae8f311bb128c7` |
| `Makefile`（test-integration `-timeout=20m`） | `2af4409abd950b436fb2fb55f565add8d2ec810538d21585d110c297315fd5bd` |
| `internal/storage/postgres/pgtemp/slots.go`（夹具槽位闸门，§4 第 18 条） | `382615c5cefec4c769745c1a6cb0b9117d850c06479d3389b74ca72b29bd372a` |
| `internal/storage/postgres/pgtest/pgtest.go`（委托闸门） | `e8a2f9e3a1e2c428162f5fc78e2917b28754a016772d07db20f083226405bdb7` |
| `internal/storage/postgres/pgtemp/pgtemp.go` | `8f1ebe98c9e107473ed7d2c769c006ccf5b786ebb11bf888c289f4c77a3551a4` |
| `internal/storage/postgres/migrate.go`（按目标 schema 派生锁键，§4 第 11 条） | `5df5a57b9aadcc65f378fe6a340704f617e150528002a694e4aeabff003b2d7a` |
| stage e2e 自建候选二进制（本轮 `tested_binary_sha256`，`binary_source=temp`） | `a3d23d5814ce6ce474e2821039e3a6f9593163d23af43a11c48270fbb3891d76` |
| 发布构建产物 `bin/basalt.exe`（`make check` 的 build，`CGO_ENABLED=0`，stamp `go1.27.3`） | `f7ae196aa0b4ca81635ceac1a079f1f2736024949e96d77bf4739130df453287` |
