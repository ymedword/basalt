# run-state — 015-acceptance-infra

> 本文件是**过程状态，非设计工件**——speckit 命令**不得**把它当设计输入。
>
> 失败台账**只追加**：一行一次尝试，不改旧行（可 diff 审计）。
>
> 唯一允许原地翻转的是未验证清单的「未验证 → 已验证」，翻转必须注日期与证据。

## 1. 基线

| 项 | 值 |
| --- | --- |
| 起始 commit | `1b074a400cf35a9bce0ae2981303d11430744515`（`main`，US-10 收口后） |
| 分支 | `015-acceptance-infra`（自 `main` 建，未推送） |
| 开跑时未提交改动 | 无（`git status --porcelain` 0 行） |
| worktree | `D:\code\basalt`（单工作区，未建并行 worktree） |
| 工具链 | `go1.27.2 windows/amd64`（≥ `toolchain` 声明） |

**模块卡人工 review ①（真实决定）**：2026-10-19，用户审阅 review ① 提交包（含两轮退回修订后的最终版）后决定「可以了，开始us15的注册和开发吧」，批准 US-15 登记并进入开发。所审路径 `docs/us/us15-acceptance-infra.md`，sha256 `19b70290a83380a6ee65c8671a3348df07efe88d216b35ae7fc9b883a1c2aae8`，1322 行。该版本已由 `/us-register 15` 登记入 `docs/us/REGISTRY.md`（第 37 行，未入库状态）。

**本 US 允许触碰的文件范围**（取自模块卡 §3.1 / §3.2）：

- `scripts/acceptance/`：`main.go`、`execute.go`、`config.go`、`fingerprint.go`（修改）；`record.go`、`stagelint.go`、`record_test.go`、`stagelint_test.go`、`stages_test.go`、`records_test.go`、`invariants_test.go`（新增）；`acceptance_test.go`（仅签名适配 + 新增两个用例，既有断言零删除）
- `internal/e2e/`：16 个文件（`doc.go`、`harness_test.go`、`fixtures_test.go`、`bootstrap_test.go`、`agentloop_test.go`、`toolsandbox_test.go`、`memory_test.go`、`mcp_test.go`、`notify_test.go`、`web_test.go`、`lock_test.go`、`recovery_test.go`、`scheduler_test.go`、`console_test.go`、`signal_linux_test.go`、`slow_test.go`、`real_test.go` —— 其中 `doc.go` + 15 个 `_test.go`）
- `harness/`：`stages/current.json`、`stages/us15-linux.json`、`stages/us15-slow.json`、`stages/us15-real.json`（新增）；`acceptance/ledger.md`、`acceptance/records/README.md`、`acceptance/records/reports/`（新增）；`README.md`、`project.md`（修改）
- `.github/workflows/ci.yml`（修改：插入三步 + 运行目录唯一化）
- `docs/DevelopmentLoop.md`（修改：§5.1 第 6 项整项替换、§9.3 引用句）
- `docs/us/REGISTRY.md`（已由 `/us-register 15` 追加一行，不再改动）
- `specs/015-acceptance-infra/**`（spec / plan / tasks / run-state）

**不改动**：`harness/acceptance.json`（profile 不变）、`Makefile`（七目标不变）、`go.mod` / `go.sum`（零新增坐标）、`schema.sql`（零迁移）、`docs/contracts/`、宪法、四份权威正文、历史 stage 三个文件、US-1~10 颗粒度文档。

**三件输入 sha256 基线**：

| 输入 | sha256 | 状态 |
| --- | --- | --- |
| `docs/us/us15-acceptance-infra.md` | `19b70290a83380a6ee65c8671a3348df07efe88d216b35ae7fc9b883a1c2aae8` | 已冻结（人审 ① 所审版本） |
| `specs/015-acceptance-infra/spec.md` | `c2ee61578a5e38ab0259ebc29c1ed0e87dbe106ab797d136e499e6f4b1b6529f` | 已冻结（clarify 后版本，206 行；5 条 AI 澄清均注模块卡来源） |
| `specs/015-acceptance-infra/plan.md` | `d1c76acca7daeaeac297be554582770aae8516921c2c86535b76db9a707bcd28` | 已冻结（原文 `2d6414ef…`，143 行；2026-10-19 analyze 阶段按模块卡 §3.1.1 表把 `internal/e2e` 文件数由 16 修正为 17，见 §6） |

**tasks 正文指纹**：`3d9c188422cf5329eb864ce1c70ac17e8eb9d26a82bca8a24cd2f1844022198a`（372 行，61 个任务；勾选位归一后冻结，2026-10-19）。原文 `bb0c0323…` 经实施前 analyze 修正 T038 / T039 / T042 / T053 / T057 / T061 与两处依赖注记后形成本基线，修正清单见 §6。

### 1.1 AI 任务比对（步骤 5 结论）

| 核对项 | 结论 | 依据 |
| --- | --- | --- |
| 任务 ↔ 模块卡 §3 交付物 | **齐**：12 个 `scripts/acceptance` 文件、17 个 `internal/e2e` 文件、4 个 stage、发布面 3 项、README / project、CI、DevelopmentLoop 均有任务；无表外交付物 | `tasks.md`「任务 ↔ 交付物比对」表逐项映射 |
| 33 个现行 check 可追溯 | **是**：US-8 7（含替代）、US-9 9、US-10 13、US-15 4，均映射到 T016 / T017 / T051 / T053 与四个 `015-*` 的实现任务 | `tasks.md`「33 个现行 check 的可追溯任务」 |
| 4 个独立 stage check 可追溯 | **是**：`015-signal-linux` / `015-slow-timeout` / `015-real-model` / `015-real-feishu` 均映射到测试、stage 与收口执行任务 | `tasks.md`「4 个独立 stage check」 |
| 关键回归与逆改点 | **是**：模块卡 §4.2 列出的具名测试均落到任务，并附逆改点 | `tasks.md`「关键回归与逆改点落点」 |
| 判据面变更 | **仅 §3.4 所列条目**；`acceptance_test.go` 只做签名适配 + 两个新增用例；零删除 / 零跳过 / 零放宽 | `tasks.md`「判据面变更核对」 |
| 历史冻结输入 | 未把历史 stage 或历史 US 文档列为修改项 | 任务清单无对应文件 |
| 测试先行 | 记录 / lint / 指纹测试先于或伴随实现；e2e 夹具先于目录与记录测试；回填执行与记录发布为独立收口任务 | T006→T007、T011→T012、T014→T015、T020–T038→T039–T043 |
| 计数差异 | 模块卡 §3.1.1 表实为 **17** 个文件（`doc.go` + 16 个 `_test.go`）；输入摘要与 plan 原稿的“16 个文件（`doc.go` + 15 个 `_test.go`）”少计 1 个测试文件。任务按表逐行覆盖 17 个，不增不减 | `tasks.md` 已登记同一差异；plan 同项已在 analyze 阶段修正（见 §6） |

**任务数**：Setup 2、Foundational 3、US1 8、US2 6、US3 24、US4 7、US5 3、Polish 8，合计 61。

**历史 stage 钉值基线（T001 实测，2026-10-19）**：三文件均为 LF 换行，原始哈希 = LF 归一哈希。

| 文件 | sha256（LF 归一） |
| --- | --- |
| `harness/stages/us8-notify-r1.json` | `85fcb3ebaa1b6615d977ac4457ceaadfdb473d07e1a0b9d1d3d1e127162c2f91` |
| `harness/stages/us9-web.json` | `1ff4c036e178f30e01aa5d307c32b6d21ab7b5a3979b8972f025edcc8202a412` |
| `harness/stages/us10-scheduler.json` | `42670059a5f0ba1839fd37888f44ad32996ab4bc70233485dec16ded6bab444d` |

## 2. 失败台账

| 日期 | 问题（测试名 / 首错位置 + 症状） | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-10-19 | T018 `sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/current.json` → exit 2，`# ./internal/e2e` `stat ...: directory not found` | US3 的 e2e 包尚未落地（T020），lint 无法执行 `go test -list` | **预期红（非缺陷）**：按 T018 记录；T020 建包后复跑应转 exit 1（缺 `015-*` 具名测试）；完整 lint 由 T053 收口。历史 check 与替代项的定义等价性本轮由 T019 的 `TestStages_CurrentCoversHistorical` / `TestStages_CurrentMatchesHistorical` 已判 | 1 |
| 2026-10-20 | T018 复跑（T020 建包后）`--lint-stages --stage harness/stages/current.json` → `EXIT=1`：`ERROR ... missing TestBackfill_*` / `TestStages_Backfill*` / `TestRecords_*` / `TestRepoInvariants_*`；历史 `us8-notify-r1.json` 出 1 条 `WARN ... missing TestToolListerPort_HeavyAssemblyRegistersNineTools` | 建包后 `go test -list` 可执行，缺失从"目录不存在"下沉为"具名测试缺失" | **按预期转档（非缺陷）**：exit 2 → exit 1 转换完成；`WARN` 属历史 stage 的既有失配（历史 stage 失配只 WARN 的口径）；`015-backfill` 的具名缺失由 T022–T034 转绿，完整 lint 由 T053 收口 | 1 |
| 2026-10-20 | T022 首跑 `TestBackfill_Bootstrap/assembly` → FAIL：`internal/core 依赖了非标准库包：github.com/ymedword/basalt/internal/core`（其余 12 个子测试已 PASS） | `go list -deps ./internal/core` 的结果含**被查包自身**，模板只滤 `.Standard`，自身被误判成非标准依赖 | 修测试（不放宽判据）：先取 `go list -f '{{.ImportPath}}'` 的 self 再在汇总时跳过；修复后 `-v` 复跑 13/13 PASS，`binary_sha256=5a98546b…`，`ok … 5.957s` | 1 |
| 2026-10-20 | T026 首跑 `TestBackfill_MCP/bad_command` → FAIL：`WARN 带固定短语：输出缺少 "reason=启动失败"`（其余 7 个子测试 PASS） | 缺可执行文件的失败点在 initialize 握手（`exec.Command` 不校验存在性），WARN 的 reason 取 `初始化失败`；模块卡只固定 WARN 文案与 `detail=可执行文件不存在`，未固定 reason 字面量 | 修测试断言（对齐实测读数，不放宽判据）：改判 `reason=初始化失败` 并注明握手期暴露；复跑 8/8 PASS，`binary_sha256=5a98546b…`，`ok … 7.107s` | 1 |
| 2026-10-20 | T028 首跑 `TestBackfill_Web/redaction` → FAIL：`日志脱敏：文本不应包含 "INTERNAL-MARKER-7f3a"`（其余 17 子测试 PASS） | 整体进程输出断言超出 US-9 约束 23 的对象：脱敏对象是 HTTP 层 failed WARN；提供方适配器的 `msg=模型调用失败` WARN 按设计保留原始 error（约束 23 原文「原始原因只留在 runs.reason 与 500 的 ERROR 日志」） | 收紧断言到诊断行（`diagnostic=` / `<path>` / 无哨兵与原始路径 / `run_id`+`request_id`+`session_id`+`agent` 四键），不加新判据、不放宽既有判据；复跑该子测试 PASS | 1 |
| 2026-10-20 | T028 复跑 `TestBackfill_Web/redaction` → FAIL：`取消的运行应有 1 行 cancelled，实际 0 行`（日志有 `运行终态落库失败 store=runs op=finish`） | 客户端断开使运行上下文取消，`FinishRun` 用已取消上下文落库失败；`internal/core/service.go:226` 明文设计是「行留 running 待恢复扫描收尾」 | 按设计改断言：接受 `cancelled`（已落库）/ `running`（待恢复）两种时序，并要求重启后恢复扫描把行收尾（`finished_at` 非空）；复跑 PASS | 1 |
| 2026-10-20 | T029 红态探针 `go test -overlay` 首跑：TestMain 嵌套构建 FAIL `Open d:\code\basalt\internal\workspace\lock_windows.go: deleted in overlay`，而同参数直接 `go build -overlay` 退出 0 | overlay 键为大写盘符 `D:/…`，外层 shell cwd 为小写 `d:\code\basalt` 时 `go env GOMOD` 返回小写、嵌套构建用 `cmd.Dir` 小写，fsys 按字节比较路径大小写 → 键失配（错误文案误导为 deleted） | 在盘符大写 cwd（`D:\code\basalt`）下复跑生效：逆改 `lockHandle` / `unlockHandle` 为 no-op 后四个分支全红——`instance_lock`：第二实例照常取锁、装配完成、仅 bind 报 `Only one usage of each socket address`；`write_mode`：`profile create` 退出 0 并写出 `agents/lock-probe/AGENT.md`；`platform_lock`：暂停窗口内 `profile create` 退出 0 并创建 `suspended-probe`；`heavy_commands`：`chat --message` 走到 provider 传输失败而非装配拒绝。红态二进制 `binary_sha256=203940a1…`，绿态 `5a98546b…`；探针目录仓库外，用毕已删 | 1 |
| 2026-10-20 | 尝试用「双大小写键」overlay 规避 cwd 大小写问题 → 两个嵌套 `go build` 进程各占满一核、150s 不结束（无限循环） | 同文件两个仅大小写不同的 overlay 键触发 cmd/go fsys 的查找 / 遍历退化 | 放弃双键方案，恢复单键 overlay；`taskkill` 清掉死循环进程后复跑正常，红态读数不受影响 | 2 |
| 2026-10-20 | T030 红态探针首次 overlay 替换未命中：`task_store.go` 副本 grep 仍为 `ExecutionStarted`，测试照常 PASS 且 `binary_sha256` 与绿态相同 | perl 锚点误加前导反引号（实际源码该行 `ORDER BY` 前无标记），替换零命中且不报错 | 修正锚点为 `e.id ASC`, string(core.ExecutionStarted)) 后生效。三条逆改全红：① 关闭半截组识别（`len(missing) > 0 && false`）→ `等待输出 "崩溃恢复扫描完成 unknowns=1 repaired_groups=1 closed_runs=1" 超时`，日志实测 `repaired_groups=0`，红态二进制 `10cf3650…`；② `RecoverStartedExecutions` 查询状态改为 `ExecutionCompleted` → `等待输出 "启动恢复完成 recovered=1" 超时`，日志 `recovered=0`，红态二进制 `82fff493…`；③ `warnMissedTriggersLocked` 对错过点插 `StartExecution` 行（模拟补跑）→ `停机错过不得补跑，实际产生 1 条执行行`，红态二进制 `7f3ead15…`。绿态 `5a98546b…`，五子测试 5/5 PASS（8.610s）；apply 路径哈希 `recovery.go 1a92e6ae…` / `task_store.go 947f7d68…` / `scheduler.go cf55bfa6…`。探针目录仓库外，用毕已删 | 1 |
| 2026-10-20 | T031 红态探针（模块卡 §4.2 逆改点三条；overlay 副本仓库外，盘符大写 cwd 下生效） | 逆改点 = cron 不触发 / 归属守卫失效 / `CRON_TZ` 被忽略 | 三条全红：① `internal/core/scheduler.go` 的 `s.engine.Start()` 改为不调用的方法值 → `scheduler_test.go:78 等待输出 "调度触发" 超时（1m0s）`，红态二进制 `a5d6a373…`；② `internal/storage/task_store.go` 归属谓词 `WHERE scheduled_tasks.agent_name = excluded.agent_name` 改为 `WHERE 1 = 1` → `scheduler_test.go:814 进程在 30s 内未退出`，日志 `agents=2` 且任务注册成功（守卫失效后进程不 fail closed）；③ `internal/workspace/agent.go` 的 `CronSpec` 忽略 zone 恒发 `CRON_TZ=UTC` → `scheduler_test.go:484 等待输出 "调度触发" 超时（1m0s）`，日志 `next=…09:43:58.000+08:00` 比墙钟晚 8 小时。绿态 `binary_sha256=5a98546b…`，十子测试 10/10 PASS（53.57s）；apply 路径哈希 `scheduler.go cf55bfa6…` / `task_store.go 947f7d68…` / `agent.go ee3e45f0…`。同任务对 `recovery_test.go` 的 `scheduleSummary` 只做字段增补（三个新字段供 T031 读视图），既有断言零改动。探针目录仓库外，用毕已删 | 1 |
| 2026-10-20 | T032 红态探针（模块卡 §4.2 逆改点「管道输出被按 ANSI 代码页截断」；overlay 副本仓库外，盘符大写 cwd 下生效） | 逆改点 = 控制台管道输出按 ANSI 代码页截断 | 红态：`internal/cli/chat.go` 的 `renderTurn` 最终回答追加改为 `strings.Map` 把所有 `r > 0x7F` 的字符替换为 `?`（模拟代码页截断）→ `console_test.go:73 最终回答逐字到达 stdout：输出缺少 "管道往返成功：中文 🚀 全角，。！？"`，`--- FAIL: TestBackfill_ConsolePipe (0.95s)`。绿态：`binary_sha256=5a98546b…`，`--- PASS: TestBackfill_ConsolePipe (2.02s)`（单顶层测试、不拆子测试；三段各自起真实 `chat` 进程，shell 与 MCP 分段跑以避开共用 `shell.env_passthrough`）。apply 路径哈希 `internal/cli/chat.go 4c52fdc1…` / `internal/e2e/console_test.go 0cae98ff…`。探针目录仓库外，用毕已删 | 1 |
| 2026-10-20 | T033 红态探针（模块卡 §4.2 逆改点「信号不触发优雅停机或事件序错乱」；overlay 副本仓库外，WSL Ubuntu + go1.27.2 下生效） | 逆改点 = 信号不触发优雅停机或事件序错乱 | 红态 A：`internal/app/run.go` 的 `signalContext` 丢弃 `signal.NotifyContext` 返回的 ctx、改返 `context.WithCancel(ctx)`（信号被吞、停机不启动）→ `signal_linux_test.go:94 监听 127.0.0.1:35529 未在 10s 内关闭`，`--- FAIL: TestBackfill_SignalLinux/grace_exhausted (25.02s)`，红态二进制 `binary_sha256=5acbbd75…`。红态 B：`internal/app/wire.go` 的 `heavyRuntime.Close` 把 `mcp.Close` 移到 `scheduler.Stop` 之前（停机事件序错乱）→ `signal_linux_test.go:230 SIGTERM 之后的 MCP 工具结果：工具结果不含 "信号后 MCP 回显"`（工具结果为「MCP 调用失败：helper/echo」），`--- FAIL: TestBackfill_SignalLinux/sigterm_order (16.80s)`，红态二进制 `binary_sha256=4e2a4007…`。绿态（WSL 实测）：`--- PASS: TestBackfill_SignalLinux (118.29s)`（grace_exhausted 84.37s / sigterm_order 33.92s，240s 预算内），`binary_sha256=06cc0abd…`；Windows 分支记未验证（平台限制：无法向子进程投递 SIGTERM）。apply 路径哈希 `internal/e2e/signal_linux_test.go d037f166…`。探针目录仓库外，用毕已删 | 1 |
| 2026-10-20 | T034 红态探针首轮（`go test -overlay=…` 外层测试 + 内层 `go build`）→ **未变红**：`TestBackfill_SlowTimeout/run_timeout` PASS（316.10s），绿态同二进制 `5a98546b…` | 外层 `-overlay` 只作用于被编译的测试包；e2e 测试内部的 `go build ./cmd/basalt`（`harness_test.go:153`）是独立进程，未继承 overlay，二进制仍是真实实现 | 改用 `GOFLAGS=-overlay=<绝对路径>` 运行外层测试：内层 `go build` 经 `cmd.Env = append(os.Environ(), "CGO_ENABLED=0")` 继承 GOFLAGS 后生效；本轮先记为探针方法错误（非测试缺陷） | 1 |
| 2026-10-20 | T001–T015 勾选补记：tasks.md 中 T001–T015 仍为未勾选，而其工件与测试已在早前轮次落地 | 复选框未随验收回写（非实现缺失） | **非缺陷，勾选补齐**：T001 见本文件 §1 的 stage 钉值基线；T002 三件发布面骨架存在；T003–T012 由 `grep` 核对符号与字段（`superseded_checks` / `level` / `acceptanceReport` 新字段 / `buildRecord` 等 / `--stage-only` 等 / 指纹忽略面）并复跑 `TestRecord_*`、`TestAccept_ReportPathsRelative`、`TestAccept_ExternalRequiresAuthorizationFlags`、`TestFingerprint_IgnoresAcceptanceEvidence` 全 PASS（`ok … scripts/acceptance 2.229s` / `2.050s`）；T014–T015 的 `TestStageLint_*` 与 `transformListArgs` 等符号存在且 T018 复跑已覆盖 lint 行为。T013 的绿读数即本轮两次 test 输出 | 1 |
| 2026-10-20 | T035 stage lint 预检：`GOPROXY=https://goproxy.cn go run ./scripts/acceptance --lint-stages --stage harness/stages/current.json` → `stage lint: ok`、exit 0；us15-slow 无告警；us15-linux 在 Windows 出 2 条 `WARN … missing TestBackfill_SignalLinux/*` | linux tag 测试在 Windows 不参与 `go test -list`；历史 / 非 current stage 失配只 WARN 的口径（当前 stage 之外不 fail-closed） | **按预期**：非 current stage 的 WARN 记录在案，Linux 通道读数由 T040 在 WSL 给出；两 stage 文件 `inputs` / `protect` / `approval` 与模块卡 §3.2 逐字核对 | 1 |
| 2026-10-20 | T036 目录与冻结集落地：`backfillCatalog` 129 项（process 114 + external 15）、`requiredBackfillItems` 114 项；`go test -count=1 ./scripts/acceptance -run 'TestStages_'` → `TestStages_BackfillCatalogCoversRequiredSet` PASS（含同时删项仍红反例），`TestStages_BackfillCatalogMatchesRequiredTests` / `TestStages_PinsUS15Checks` FAIL，原因均为 `open harness/stages/us15-real.json: The system cannot find the file specified` | 两测试按 tasks.md 设计依赖四个 stage 齐全；`us15-real.json` 由 T047 交付，属登记在案的预期 interim 红（tasks.md「Key Dependencies」） | 编号正则（US-1~3 无子节、US-4~9 必含子节、US-10 `6.2`、run-state `rsU{1-4}`）与字母按 §2.6 / §2.5.2 拆分顺序（未进目录的丙类 / 未验证 / 不适用拆分同样占字母）逐项转写；反例覆盖删 required_test、linux 项挪入 015-backfill、重号、非法编号 | 1 |
| 2026-10-20 | T034 红态探针复跑（`GOFLAGS=-overlay=<绝对路径>`，盘符大写 cwd；overlay 逆改 `internal/core` 的 run 超时执行路径为不生效）→ **红态成立**：`slow_test.go:71 等待任务 demo-1 的 last_status = "failed" 超时（5m30s），最后一次读数 "started"`，`--- FAIL: TestBackfill_SlowTimeout/run_timeout (345.38s)`，红态二进制 `binary_sha256=a9f8ac73…`，exit 1 | 逆改点 = 300s 慢速超时不生效（任务停在 `started` 不落 `failed`） | GOFLAGS 继承生效（对比首轮同参数 PASS 316.10s / 二进制 `5a98546b…`）；红态逐字文案已取到，绿态同测试 PASS（316.23s，`binary_sha256=5a98546b…`）。探针目录 `%TEMP%\us15-probe-slow` 仓库外，用毕已删 | 2 |
| 2026-10-20 | T037 记录面测试落地：`gofumpt -w` 后 `go vet ./scripts/acceptance` 通过；跑 `TestFingerprint_` / `TestRecords_` / `TestLedger_` 三类（`-v` 读数）→ `TestFingerprint_IgnoresAcceptanceEvidence` PASS、`TestRecords_BackfillBindsToExecution` PASS（7/7 反例子测试）、`TestRecords_RealBindsToExecution` PASS（3/3）、`TestRecords_StatusDoesNotMaskConflicts` PASS（4/4）、`TestRecords_PublishedConform` PASS、`TestRecords_NoSecretsOrAbsolutePaths` PASS、`TestLedger_IndexesAllRecords` PASS（4/4）；唯一红为 `TestRecords_BackfillCoversCatalog/published_set_is_covered`，报「Should be empty, but was [us1-6-2b … us10-6.2-16c]」全 129 项列出 | 发布面尚无记录（首批由 T042 生成、T043 台账登记），属 tasks.md 第 175 行登记在案的 bootstrap 预期红 | **预期红保留**：断言为视图 1 全目录覆盖，不做空集豁免；T042 / T043 发布后转绿。首轮实现曾把「同键报告被改写」与后续反例共用夹具，导致 report_sha256 遮蔽后续预期错误，已改为每个反例独立 `t.TempDir` 夹具 | 1 |
| 2026-10-20 | T038 e2e 全量首跑 `go test -tags=e2e -count=1 -json ./internal/e2e` → exit 1：`TestBackfill_MCP/shutdown` FAIL，`mcp_test.go:281 用例开始前已有 1 个 helper 子进程残留——先修补前序用例的停机路径`（其余 11 个顶层测试全 PASS，115 个 PASS 动作） | 前序 `tool_list` / `two_paths` 拉起的 helper 在父进程退出后才异步感知 EOF；`shutdown` 的前置基线检查是零等待瞬时读数，把"正在退出"误判为"残留"（同测试单独复跑 8/8 PASS，4.25s） | 修测试的**前置基线等待**（不是验收判据）：`waitForHelperCount(t, 0, 10s)` 后再起本用例；真正的停机缺陷仍由用例末尾的 `waitForHelperCount(t, 0, 10s)` 与等待超时判红。复跑 `-run '^TestBackfill_MCP$'` 8/8 PASS；全量复跑 exit 0：**11 个顶层测试 / 116 个 PASS 动作 / 0 FAIL**，`ok … 98.434s` | 1 |
| 2026-10-20 | T038 读数：`go vet -tags=e2e,slow,real ./internal/e2e` → exit 0（VET_FULL_OK）；`go test -tags=e2e -list '.*' ./internal/e2e` 列出 11 个 `TestBackfill_*` 与 helper 入口 `TestMCPHelperProcess`，`ok … 1.818s`；全量 e2e `exit=0`、`pass` 动作 116、`fail` 动作 0 | T038 只判 e2e 具名 PASS 与 vet；`015-invariants` 测试文件到 T051 才存在，完整 stage lint 归 T053 | 三项读数齐备，无未验证项 | 1 |
| 2026-10-20 | T039 首次执行 → exit 2：`input/protected path scripts/acceptance/invariants_test.go: GetFileAttributesEx D:\code\basalt\scripts\acceptance\invariants_test.go: The system cannot find the file specified.` | current stage 的 `protect` 清单把 T051 交付物列为受保护路径，而 T051 尚未实施；目录前置校验直接失败 | **依赖顺序修正（非缺陷）**：T051 本就标 [P]，先实施 T051（以及其 `CIRunDirsUnique` 判据所需的 T052），再以**新目录**重跑 T039；不修改 stage、不放宽判据 | 1 |
| 2026-10-20 | T051 落地：六测试 `go test -count=1 ./scripts/acceptance -run '^TestRepoInvariants_'` → 6/6 PASS，`ok … 1.358s`；`go vet ./scripts/acceptance` exit 0；逆改点按模块卡 §4.2 逐条取红态（探针模块与备份均仓库外，脚本退出时恢复并核对哈希） | 六条逆改应分别命中对应断言 | 六条红态全部取得逐字文案：① go.mod 加 `require github.com/probe/eino`（replace 仓库外空模块）→ `go.mod 出现 Agent 框架坐标 "eino"（宪法 I：ReAct 循环必须自实现，协议 SDK 不得接管循环）`；② `internal/core/errors.go` 加 `time.Local` → `生产代码使用了进程本地时区（时区必须显式：调度 zone 缺省 UTC、Agent 配置显式 zone）` + `internal/core/errors.go:214`；③ `internal/app/providers.go` 加合成 `sk-…` → `发布面出现明文凭证形态（一律 ${ENV_VAR} 占位，运行时注入；扫描面见模块卡 §4.2）` + `providers.go:86`；④ go.mod 加 `github.com/probe/gorm` → `go.mod 出现 ORM 坐标 "gorm"（宪法 IX：手写 SQL，迁移只经 schema.sql + schema_version）`；⑤ providers.go 导入测试专用坐标 → `生产代码导入了测试专用依赖 santhosh-tekuri/jsonschema（发布二进制只带必要依赖）` + `providers.go:4`；⑥ ci.yml 把 `--out` 退回 `.loop/runs/ci` → `.github/workflows/ci.yml:60 的 --out ".loop/runs/ci" 缺少 GITHUB_RUN_ID（跨运行会复用目录，证据互相覆盖）`。恢复后 go.mod / errors.go / providers.go 哈希与探针前逐字节一致 | 1 |
| 2026-10-20 | T051 首次实现按整行判 `--out`：同一行 `tee "...ci-console-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.log"` 仍含两变量，把 `--out` 退回固定名不会红（假绿） | 判据必须截取 `--out` 后的参数，而不是整行文本 | 改为正则捕获 `--out` 后的参数（双引号 / 单引号 / 裸值三形态）后只判参数值；解析不到参数时 fail-closed 判红；复跑 6/6 PASS；红态 ⑥ 在改后实现上取得 | 1 |
| 2026-10-20 | T052 落地：`.github/workflows/ci.yml` 在 Unified acceptance 与 Upload 之间插入 lint-stages / current / us15-linux 三步；既有 `--out` 与 console 日志、三步 `--out` 全部带 `${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}` 后缀；不新增 job、不改 job 名、不接入 us15-real | CI 命令原文照抄模块卡 §4.2，运行目录唯一性由 `TestRepoInvariants_CIRunDirsUnique` 机器判 | 逐行核对三步与四个运行目录（`ci-…` / `current-…` / `us15-linux-…` 及 `ci-console-…`）；不变量测试绿；`ci.yml` 当前交付态哈希 `a9fcea17…` | 1 |
| 2026-10-20 | T039 r2 记录生成：`record.json` 校验失败 `record contains a webhook path, query string or userinfo`（同轮报告与日志已产出） | `validateRecord` 对整条记录做 URL 形态扫描，把已批准的**子测试名**（如 `TestHttpGet_PortFrozenCombinations/放行_http://api.example.com/a`）误判为 webhook 残留 | **修实现（判据面变更，须在 review ② 披露）**：URL 形态扫描改为只扫 `urlScanRecord(rec)`（清空各 check 的 `passed_tests` / `skipped_tests` 后再扫），凭证形态仍扫全记录；新增回归 `TestRecord_ValidateSkipsTestNameURLs`（子测试名 URL 形态通过、`Evidence.Excerpt` 中的 webhook 仍判红）并复跑 PASS。依据：模块卡 §4.2 校验规则 9 的扫描对象是证据文本，测试名不是证据文本；不删除任何既有断言 | 1 |
| 2026-10-20 | T039 r3 回填运行（新目录 `us15-t039-backfill-r3`）：`sh scripts/check-acceptance.sh --stage harness/stages/current.json --stage-only --run-kind backfill --executor ai --out .loop/runs/us15-t039-backfill-r3` → 整体 exit 1，`failed 015-acceptance-infra (exit=1)`，其余 32 个 check 全 passed（含 `015-backfill` exit=0、`015-invariants` exit=0、`015-e2e-vet` exit=0） | 已登记在案的 bootstrap 预期红：覆盖用例（T042 / T043）与真实 stage（T047）未发布前 `015-acceptance-infra` 必红 | **预期红保留**：`015-backfill` 的 11 个顶层测试全 PASS（`TestBackfill_AgentLoop` / `_Bootstrap` / `_ConsolePipe` / `_CrossProcessLock` / `_MCP` / `_Memory` / `_Notify` / `_Recovery` / `_Scheduler` / `_ToolSandbox` / `_Web`），`binary_sha256=5a98546b…`；`record.json` 已产出：`run_kind=backfill`、`executor=ai`、`verdict=failed`、`base_commit=1b074a4…`、`source_fingerprint=14b8e88d…`、`report_sha256=eb0b233b…`、`go_version=go1.27.2`、`windows/amd64`、`dirty=true`；`report.json` 与 32 份日志留存于运行目录，T042 取 `015-backfill` 段 | 1 |
| 2026-10-20 | T044 `internal/e2e/real_test.go` 落地：`go test -c -tags='e2e real' -o %TEMP%/basalt-e2e-real.test.exe ./internal/e2e` → 编译通过；`gofumpt -l internal/e2e/real_test.go` 无输出（退出码 0） | 真实项测试依赖尚未存在的 `us15-real` stage 与 `requiredRealItems`（T045 / T047） | 文件按模块卡 §4.5 实现 `TestBackfill_Real` 11 子测试 + `TestBackfill_RealFeishu` 2 子测试；缺环境 `t.Fatalf`（不 SKIP）、包级原子封顶 30 次调用 / 5 次外发、证据只出 `scheme://host`；真实运行读数待 T048 / T049 授权执行后补 | 1 |
| 2026-10-20 | T040 Linux 信号 stage（新目录 `us15-t040-linux-r1`，WSL Ubuntu）：`sh scripts/check-acceptance.sh --stage harness/stages/us15-linux.json --stage-only --out .loop/runs/us15-t040-linux-r1` → exit 0，`015-signal-linux` passed | 平台中立回填在 Windows 执行、Linux 信号语义须在本机构建环境执行 | `TestBackfill_SignalLinux` PASS（118.09s；子测试 `grace_exhausted` 84.10s、`sigterm_order` 34.00s）；`binary_sha256=06cc0abd…`、`environment=linux/amd64`、`go1.27.2`、`fingerprint=dbf3fb30…`；`report.json` 与日志留存，T042 取 `015-signal-linux` 段 | 1 |
| 2026-10-20 | T041 慢速 stage（新目录 `us15-t041-slow-r1`）：`sh scripts/check-acceptance.sh --stage harness/stages/us15-slow.json --stage-only --out .loop/runs/us15-t041-slow-r1` → exit 0，`015-slow-timeout` passed | 300s 超时语义须实际跑满慢速路径，不能以单元测试替代 | `TestBackfill_SlowTimeout` PASS（320.73s；子测试 `run_timeout`）；`binary_sha256=5a98546b…`（Windows 构建）、`environment=windows/amd64`、`go1.27.2`、`fingerprint=1d0ea555…`；`report.json` 与日志留存，T042 取 `015-slow-timeout` 段 | 1 |
| 2026-10-20 | T042 发布：生成器（仓库外，用毕待收口删除）从目录导出（临时探针 `zz_t042_probe_test.go`，用毕即删）取 `backfillCatalog` 129 项，再从 `us15-t039-backfill-r3` / `us15-t040-linux-r1` / `us15-t041-slow-r1` 的报告**只读承载 check 段**（`015-backfill` / `015-signal-linux` / `015-slow-timeout`）程序化生成；发布 117 条记录（3 条 run + 114 条 manual_item）与 114 份同次报告副本；探针复核 `validateRecord` + 目录映射 117/117 通过；15 个 external 项因无对应运行跳过 | 值必须取自报告、断言必须能落在具名 `passed_tests`；发布面只追加 | 首轮复跑红：`TestRecords_NoSecretsOrAbsolutePaths` → `evidence contains an absolute windows path`，Messages `us15-t039-backfill-r3.json`；根因已定位：`absoluteWindowsPath` 的盘符正则无前置边界，命中已批准子测试名里的 `https:/`（`s:/`，另有 `http:/` 的 `t:/`），非盘符路径。**修判据（误判收窄，须在 review ② 披露）**：正则加「行首或非字母数字」边界（`[^a-z0-9]` 作前置分支），并新增回归断言（测试标识内 `http://` / `https://` 不判红；行首 `D:\logs\run.log` 与 `D:/logs/run.log` 仍判红）；复跑 `TestRecords_NoSecretsOrAbsolutePaths` PASS。既有断言零删除、零放宽（`D:/logs/run.log` 反例保留） | 1 |
| 2026-10-20 | T043 台账与复验：生成器按固定 7 列向 `harness/acceptance/ledger.md` 追加 117 行（链接 `records/<id>.json` 均存在）；复跑 `go test -json -count=1 ./scripts/acceptance`（`-run` 取 `TestRecords_` / `TestLedger_` / `TestStages_Backfill` 三组）→ exit 1，9 个顶层测试中 8 PASS，唯一红为 `TestRecords_BackfillCoversCatalog/published_set_is_covered`：`Should be empty, but was [us2-6-3a us4-6.1-2b us4-6.1-2d us4-6.1-2j us5-6.1-2a us6-6.1-2b us7-6.1-2b us7-rsU3-1b us8-6.1-5a us8-6.1-7b us9-6.2-5a us10-6.2-1c us10-6.2-4c us10-6.2-5c us10-6.2-16c]`（36 个 pass 动作） | 剩余红恰为 15 个 external 目录项，其记录只能由 T048 / T049 在用户授权后跑 `us15-real` 产生 | **按预期保留红**：process 侧 114 项已全量覆盖；本任务「至全绿」在 T049 前不可达，T043 复选框保持未勾选，待 T049 发布 15 条 external 记录后复跑转绿再勾；tasks.md 第 175 行的旧表述同步修正 | 1 |
| 2026-10-20 | T053 本地部分收口：`go test -json -count=1 ./scripts/acceptance -run '^TestRepoInvariants_'` → exit 0，六条不变量全 PASS；`sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/current.json` → `stage lint: ok`、exit 0，3 条 WARN（`us15-linux` 两条 `TestBackfill_SignalLinux/*` 为 Windows 无法 `-list` linux tag 测试、`us8-notify-r1` 一条为历史 stage 既有失配）；`ci.yml` 三步与目录后缀已在 T052 逐行核对 | 真实 CI 运行须用户推送后才有结论，任务原文把它归入 review ② 证据 | **本地通过**：lint 已能看到四个 `015-*` 测试文件；真实 CI 读数待推送后补记，不预写为通过 | 1 |
| 2026-10-20 | T054 / T055 / T056 文档接线：`harness/README.md` 151 行（阶段清单与 lint、旗标表、记录与发布、路径投影、指纹排除）；`harness/project.md` 66 行（5 行新增映射 + 机器结果行扩写）；`docs/DevelopmentLoop.md` 1328 行（§5.1 第 6 项整项替换为模块卡 §2.4 注 C 最终文本、§9.3 新增「现行累计清单与记录台账」段），§6.2 第 3 项与 §6.3 凭证纪律原文未动 | 文档改动不在 git 跟踪面，证据取文件行数与 IDE markdown 诊断 | **通过**：三文件 IDE 诊断均 0 问题；§5.1 第 6 项逐字比对模块卡 §2.4 注 C；三项复选框已勾选 | 1 |
| 2026-10-20 | T049 首跑 `us15-real`（run `20261019T193703-real`）：两个 check 均 failed，exit 1。`015-real-model` 11 个子测试全红，根因同一：`provider "real": HTTP 400: The supported API model names are deepseek-flash, deepseek-v4-pro, but you passed v4flash.`；`015-real-feishu/send` PASS（真实发送 1 次），`redaction` FAIL：`拒绝场景不得产生任何真实发送，实际累计 1 次` | ① 模型名 `v4flash` 不是服务端合法 ID；② `redaction` 断言的是包级累计计数，而 `send` 先运行已把累计推到 1——该断言在正常顺序下不可满足，已丧失判别力 | 已修：YAML 模型名改 `deepseek-flash`（服务端返回的合法名）；`internal/e2e/real_test.go` 拒绝场景改断言「本子测试前后计数差为 0」（拒绝语义与累计封顶不变）。判据面修正，review ② 披露。失败运行保留在 `.loop/runs/20261019T193703-real` 作证据；该次未发布记录与台账行 | 1 |
| 2026-10-20 | T049 第二跑 `us15-real`（run `20261019T193942-real`）：`015-real-feishu` passed（delta 断言修正生效，真实发送 1 次）；`015-real-model` 10/11，唯一红 `TestBackfill_Real/scheduler_manage_self`：`real_test.go:481: scheduled_tasks 应有 1 行，实际 0 行：[]`（其前 AGENT.md 写回断言已过） | 按 US-10 注 13 / 约束 21，`chat` / `tool` 下 `ReconcileAgent` 是 no-op（只落盘、不落库）；人工项 US-10 §6.2 第 4 条要求「端点列表随之变化」，只有 `serve` / `gateway` 可观察——测试用 chat 却断言库行，判据与运行模式不符 | 修测试（不放宽）：改 `serve` + `webInvoke`，断言 AGENT.md 写回、`GET /schedules` 出现 `self-added`、库内 `scheduled_tasks` 一行、审计 success；`gofumpt -w` 与 `go vet -tags=e2e,real` 通过。判据面修正（更完整），review ② 披露；失败运行保留 `.loop/runs/20261019T193942-real`，未发布该次记录 | 2 |
| 2026-10-20 | T049 第三跑 `us15-real`（run `20261019T194329-real`）：`015-real-model` passed（49.1s）、`015-real-feishu` passed（7.8s），整体 exit 0 | 验证性运行，无新假设 | **通过**：`real_model_calls=21`（≤30）、`real_model_tokens=10701`、`real_feishu_sends=1`（≤5）、`feishu_target=https://open.feishu.cn`（仅 `scheme://host`）、两 check `binary_sha256=5a98546b…` 与报告一致；运行目录对 API key / 完整 webhook / `open-apis` 路径扫描 **0 命中**。以生成器发布 15 条 external `manual_item` 记录 + 1 条 run 记录 + 15 份同次报告 + 16 行台账（ledger 144 行、records 133、reports 129）；`go test -count=1 ./scripts/acceptance` 全绿（129/129 目录覆盖） | 1 |
| 2026-10-20 | T057 首跑 `make check` → `sec` 目标失败（`make_check_exit=2`）：gosec 报 3 条——`scripts/acceptance/stagelint.go:108` G204（`exec.CommandContext` 变量实参）、`scripts/acceptance/record.go:344` 与 `record.go:171` G304（变量路径 `os.ReadFile`） | G304 可用 `os.Root` 做机制级根限定（Go 1.24+），属标准修复面；G204 的实参（`-tags` / 包路径）数据驱动，gosec 只接受全常量实参，静态无法消除——仓库外探针实测：变量命令名、常量命令 + 变量实参均报，仅全常量干净 | **修复后全绿**：① G304 新增 `readUnderRoot` / `fileSHA256(root, rel)` 经 `os.OpenRoot` 读取，全部调用点（含 `reportForRecord`）改为仓库相对路径；② G204 经用户 2026-10-20 裁决加 `#nosec G204` 并写明四条理由（命令名已固定、参数仅来自受保护 stage、只读 `go test -list`、替代写法只削弱审计面），与 `internal/tool` runner / mcp 的既有豁免同口径，review ② 披露；复跑 `gosec -quiet ./scripts/acceptance` 零命中。第二次 `make check` → `make_check_exit=0`，七目标：fmt 干净；lint（go vet + staticcheck 指定集）通过；sec（gosec + govulncheck）零命中；test 与 test-integration 通过；`check-coverage: 全部包达标`（`internal/core 86.67%`、`internal/tool 85.79%`、`internal/memory 91.32%`、`internal/storage 79.84%` 均 ≥70%，`scripts/acceptance 79.80%` ≥50%，`internal/e2e` 与 `scripts` 以「无可执行语句」跳过、有可执行语句包无一缺失）；build `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w …"` 成功。附带 `git diff --exit-code go.mod go.sum internal/storage/schema.sql docs/contracts/schema.sql Makefile harness/acceptance.json` exit 0（零新增坐标 / 迁移 / 配置键） | 1 |

## 3. 停止预算与计数

| 规则 | 预算 | 当前计数 |
| --- | --- | --- |
| 同一问题修复不收敛 → `/debug` | 3 次 | 0 |
| `/debug` 内同一问题假设不成立 → 停下讨论设计 | 3 个 | 0 |
| 本 US `/debug` 轮次 | ≤ 2 | 0 |

**无进展**（相邻两次尝试无新读数 / 新假设 / 新定位）→ 立即停，不等计数。预算到期 = 停下报告请裁，不自动放弃。

## 4. 未验证清单

收口时按模块卡 §4.5 / §6.2 逐项登记（含四态与依据）。当前：实施未开始，清单待收口填写。

## 5. 收口结论

待人工 review ②。当前状态：spec / clarify / plan / tasks 已完成并冻结；tasks 指纹 `3d9c1884…`；AI 任务比对通过；实施前 `/speckit-analyze` 已完成（CRITICAL 0，8 条发现全部在已审范围内处置，见 §6）。实现未开始，下一步从 T001 进入 implement。

## 6. 实施前 /speckit-analyze（2026-10-19）

**结论**：CRITICAL **0**；HIGH 1、MEDIUM 4、LOW 3，共 8 条发现。按本仓自动推进模式，findings 在已审范围内由 AI 控制侧修正派生工件并复跑；无范围、权限、公共契约或验收语义变更。宪法对齐 13 项逐条通过（plan.md Constitution Check 无违宪项）。

| ID | 严重度 | 位置 | 发现 | 处置 |
| --- | --- | --- | --- | --- |
| A1 | HIGH | `tasks.md` T038 | 要求"完整 stage lint 退出 0"，但 `015-invariants` 的测试文件到 T051 才存在，该 check 必红 | 已改：T038 只要求 e2e 具名 PASS 与 vet 退出 0；完整 stage lint 移入 T053 |
| A2 | MEDIUM | `tasks.md` T039 | 回填 bootstrap 运行会被尚未发布记录的用例拖成整体退出 1，未写明预期 | 已改：T039 明示整体非 0 属预期 bootstrap，必须确认 `015-backfill` check 为 passed 并留读数 |
| A3 | MEDIUM | `tasks.md` T042 | 人工项记录生成未区分整体 `run_status` 与承载该目录项的 check 段 | 已改：按 T039 / T040 / T041 各自对应 check 段取结论，整体 `run_status` 不参与判断 |
| A4 | MEDIUM | `tasks.md` T057 / FR-024 | FR-024（零新增坐标 / 迁移 / 配置键）缺可执行核对 | 已改：T057 增加 `git diff --exit-code` 对 `go.mod` / `go.sum` / 两份 `schema.sql` / `Makefile` / `harness/acceptance.json` 的零 diff 核对 |
| A5 | MEDIUM | `plan.md:39` | `internal/e2e` 文件数写 16，与模块卡 §3.1.1 表 17 行不符 | 已改：plan 改为 17（`doc.go` + 16 个 `_test.go`） |
| A6 | LOW | `tasks.md` T042 | 绑定校验方法表述含糊（"如需包内校验"） | 已改：明确用临时 `_test.go` 探针调用 `validateBackfillBinding`，用毕即删 |
| A7 | LOW | `tasks.md` T061 | "判定面"与全仓"判据面"用词不一致 | 已改："判据面" |
| A8 | LOW | `tasks.md` Key Dependencies / 计数差异登记 | "T038 负责转绿"与 A1 修订后不一致；计数行仍称 plan 少计 | 已改：依赖注记改为 T038 覆盖 e2e / vet、T053 完整 lint 转绿；计数行注明 plan 已按表修正 |

**覆盖与映射复核（复跑）**：FR-001~FR-025 + SC-001~SC-012 共 37 项，100% 有任务映射；61 个任务；33 个现行 check 与 4 个独立 stage check 均可追溯；判据面变更仅模块卡 §3.4；既有断言零删除 / 零跳过 / 零放宽；回填顺序为"e2e 与记录工具 → 回填发布 → `make check` 收口"，bootstrap 预期红已显式登记。

**修订自检**：全部 61 个任务修订时均未勾选（0 个 `[x]`），修订只改动未执行任务的描述与两处注释，不存在已执行证据被覆盖；修订后 tasks.md 372 行、61 个任务、sha256 `3d9c1884…`，plan.md sha256 `d1c76acc…`，两文件 markdownlint 0 问题。CRITICAL = 0，允许进入 implement。

## 7. 收口执行登记（T057–T059，2026-10-20）

### 7.1 T057 七目标门禁

`make check` → `make_check_exit=0`；七目标逐目标复跑读数：

| 目标 | 四态 | 本轮读数 |
| --- | --- | --- |
| fmt | 通过 | `check-format: gofmt / gofumpt 均干净`（`fmt_exit=0`） |
| lint | 通过 | `go vet ./...` + `staticcheck -checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022 ./...`（`lint_exit=0`） |
| sec | 通过（修复后） | 首跑 3 命中：G204 `scripts/acceptance/stagelint.go:108`、G304 `scripts/acceptance/record.go:344` / `:171`。G304 以 `os.OpenRoot` 机制级根限定修复（`readUnderRoot`），G204 经用户 2026-10-20 裁决加 `#nosec G204` 并写明四条理由（与 `internal/tool` runner / mcp 既有豁免同口径）；复跑 `gosec -quiet ./... && govulncheck ./...` → `No vulnerabilities found.`（`sec_exit=0`） |
| test | 通过 | `go test -race -cover ./...`（`test_exit=0`），18 包 ok；`internal/e2e` 仅 `e2e` tag 下有测试，无 tag 时 `[no test files]` |
| test-integration | 通过 | `go test -tags=integration ./...`（`test_integration_exit=0`） |
| cover | 通过 | `check-coverage: 全部包达标`；`internal/core 86.67%`、`internal/tool 85.79%`、`internal/memory 91.32%`、`internal/storage 79.84%`（≥70%），`scripts/acceptance 79.80%`（≥50%）；`internal/e2e` 与 `scripts` 以「无可执行语句」跳过 |
| build | 通过 | `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=…" -o bin/basalt ./cmd/basalt` 成功 |

**零 diff 复核**：`git diff --exit-code -- go.mod go.sum internal/storage/schema.sql docs/contracts/schema.sql Makefile harness/acceptance.json` → exit 0（零新增坐标 / 迁移 / 门禁 profile 变更）。

### 7.2 T058 quickstart 场景 1–10

| 场景 | 命令 / 通道 | 四态 | 本轮读数 |
| --- | --- | --- | --- |
| 1 lint | `sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/current.json` | 通过 | exit 0，`stage lint: ok`；3 条 WARN：`us15-linux` 两条（Linux tag 测试在 Windows 不参与 `-list`，由 WSL 通道覆盖）、`us8-notify-r1` 一条（历史 stage 既有失配，按只 WARN 口径） |
| 2 工具判据 | quickstart 两条 `go test -count=1`（同一 `-run` 集，`-v` 计数） | 通过 | exit 0；S2a 86 PASS / 0 SKIP / 0 FAIL，S2b 6 PASS / 0 SKIP / 0 FAIL |
| 3 现行累计 | `sh scripts/check-acceptance.sh --stage harness/stages/current.json --stage-only --out .loop/runs/local-20261019T195208-current` | 通过 | exit 0；33/33 check passed；`record_id=local-20261019T195208-current` 与运行目录同名；`evidence.report_sha256=b6c1929e…` 与同目录 `report.json` 实测哈希一致；33 条 `checks[].log` 全为仓库相对、`/` 分隔，绝对路径 0 |
| 4 e2e | `go test -tags=e2e -count=1 ./internal/e2e` | 通过 | exit 0，`ok … 97.611s`；11 个顶层 `TestBackfill_*`（T038 具名清单） |
| 5 vet | `go vet -tags=e2e,slow,real ./internal/e2e` | 通过 | exit 0 |
| 6 Linux 信号 | WSL Ubuntu：`sh scripts/check-acceptance.sh --stage harness/stages/us15-linux.json --stage-only --out .loop/runs/local-20261019T195208-linux` | 通过（WSL 通道） | exit 0，`015-signal-linux passed`；`duration_ms=122333`；`passed_tests` 含 `TestBackfill_SignalLinux/grace_exhausted` 与 `/sigterm_order`；`report_sha256=73f60422…` 与 record 一致。Windows 原生无法投递 SIGTERM / 真实 Ctrl+C，不把 WSL 读数记为 Windows 原生通过 |
| 7 slow | `sh scripts/check-acceptance.sh --stage harness/stages/us15-slow.json --stage-only --out .loop/runs/local-20261019T195208-slow` | 通过 | exit 0，`015-slow-timeout passed`；record / report 配对（`report_sha256=7fe08e4c…`）；1 条日志路径仓库相对、绝对 0 |
| 8 real | 引用 T049 第三跑 `run 20261019T194329-real`（本轮不重复外发） | 通过（引用） | `015-real-model` 49.1s、`015-real-feishu` 7.8s；21 次模型调用（≤30）、10701 tokens、1 次发送（≤5）；授权 `ref` / `scope` 与报告逐项相等；`feishu_target` 仅 `scheme://host`。重复真实外发不产生新证据且消耗授权额度，故不重跑 |
| 9 七目标 | `make check` | 通过 | exit 0（逐目标读数见 §7.1） |
| 10 发布面 | `go test -count=1 ./scripts/acceptance -run '^(TestRecords_…)'`（三组前缀：`TestRecords_`、`TestLedger_`、`TestStages_`） | 通过 | exit 0，`ok … 1.935s`；发布面 133 条记录 / 144 行台账 / 129 个目录项全覆盖（T049 读数，本轮判据复跑全绿） |

**产物**：汇总日志 `.loop/runs/t058-20261019T195208.log`；运行目录 `.loop/runs/local-20261019T195208-current`、`-slow`、`-linux`（均在仓库内 `.loop/`，不入库）。

### 7.3 T059 状态登记

**tasks 正文指纹（勾选位归一）**：

- 方法：`sed -E 's/^- \[[xX]\]/- [ ]/' specs/015-acceptance-infra/tasks.md | sha256sum`
- 冻结基线（2026-10-19，0 勾选，归一值 = 原文值）：`3d9c188422cf5329eb864ce1c70ac17e8eb9d26a82bca8a24cd2f1844022198a`（372 行 / 61 任务）
- 当前（2026-10-20）：`17ddb8c6d2e44a9a1dd3e6d052cd128ad2aa4eec34957ffaf537c563c6c6dc62`（372 行 / 61 任务 / **61 勾选**；T060 / T061 在复核与收口包完成后翻转，归一哈希不变）
- 差异说明：实施期 tasks.md 合法变更（勾选翻转经无感；正文改动已登记的有 T043 台账行的 `Key Dependencies` 第 175 行更新与 T053 描述扩充「运行完整 lint 至退出 0」）。任务集合未增删、编号 T001–T061 连续，不构成范围变更。**analyze 阶段的 `.pre-revision` 快照未保留**，故未做逐字节重放；本值作为收口后的漂移比对基线。

**33 个现行 check → 任务映射**（check 全名见 `harness/stages/current.json`，逐字 argv 见模块卡 §4.2）：

| 组 | check | 任务 |
| --- | --- | --- |
| US-8 | `008-envref`、`008-executor-audit`、`008-storage-notify`、`008-tool-notify`、`008-tool-notify-adapters`、`008-app-notify`、`008-ports-ten`（`008-ports-nine` 的显式替代） | T016、T017 |
| US-9 | `009-web-contract`、`009-web-handlers`、`009-web-server`、`009-web-race`、`009-storage-session-api`、`009-core-sessionid`、`009-cli-serve`、`009-app-web`、`009-cli-config` | T016、T017、T019 |
| US-10 | `010-core-scheduler`、`010-core-sessiongate`、`010-core-runid`、`010-cron-adapter`、`010-storage-tasks`、`010-workspace-writegate`、`010-tool-manageschedule`、`010-web-schedules`、`010-web-contract-schema`、`010-app-schedule`、`010-app-tools`、`010-app-crossentry`、`010-race` | T016、T017、T019 |
| `015-acceptance-infra` | 记录 / lint / 指纹 / 报告路径 / 授权旗标判据 | T006–T013、T014–T016、T036–T037、T045–T046 |
| `015-invariants` | 六个仓库不变量 | T051、T053 |
| `015-backfill` | 129 项目录与进程级回填 | T020–T043 |
| `015-e2e-vet` | tagged 代码 vet | T038、T053 |

**4 个独立 stage check → 任务映射**：

| check | stage | 任务 |
| --- | --- | --- |
| `015-signal-linux` | `harness/stages/us15-linux.json` | T033、T035、T040 |
| `015-slow-timeout` | `harness/stages/us15-slow.json` | T034、T035、T041 |
| `015-real-model` | `harness/stages/us15-real.json` | T044、T047、T048–T050 |
| `015-real-feishu` | `harness/stages/us15-real.json` | T044、T047、T048–T050 |

**未验证清单**（四态 = 未验证；均已给原因与关闭条件）：

| 项 | 类别 | 原因 | 现有证据 / 关闭条件 |
| --- | --- | --- | --- |
| GitHub 分支保护 | 未授权乙类（平台授权） | 用户未提供平台授权 | 授权到位后按 §4.5 同口径执行并发布记录 |
| NivroOS 跨仓对拍 | 未授权乙类（跨仓） | 跨仓授权未给 | 同上 |
| 真实天气 API 外发 | 未授权乙类（第三方服务） | 第三方服务未授权 | 同上 |
| 真实第三方 MCP server | 缺环境 | 缺外部安装，不属乙类授权面 | 安装后按回填矩阵执行 |
| 第二主机场景 | 缺环境 | 本机无第二主机 | 提供主机后执行 |
| Windows 原生 SIGTERM / Ctrl+C 事件序 | 平台限制 | Windows 无法投递信号或真实 console 事件 | Linux 侧 WSL 已通过（run `local-20261019T195208-linux`）；Windows 原生不冒充 |
| US-10 15b | 平台限制 | 同上 | 同上 |
| US-8 飞书错误关键词 `code=19024` | 缺环境 | 关键词安全未确认 | 确认真实错误关键词后执行 |
| US-4 Demo③ / ④ / ⑦ 与 §6.1 第 3 项 | 缺环境 | 模型 / provider 行为不确定；假模型路径已覆盖确定性拒绝与协议部分 | 真实服务行为可复现时补执行 |
| US-4 token 估算 | 缺环境 | 缺分词器依赖 | 依赖获批后执行 |
| 真实 CI 运行（lint-stages / current / us15-linux 三步） | 未验证（待推送） | 用户推送前 CI 不运行；本地仅核对 `ci.yml` 与目录唯一性不变量 | 推送后读取 Actions 结论与运行目录后缀，回填本行 |
| 丙类人工判断项 | 人判 | 证据充分性 / 控制台观感 / 备份面口径 / 差异可接受性 / 文档真实性由用户裁决 | review ② 逐条列于 §8（T061 收口包） |

**停止预算与计数**：`/debug` 轮次 0（预算 ≤2）；单题最高尝试计数 2（T034 探针方法修正、T049 真实运行第二次），均收敛，未触发「3 次不收敛」；无「相邻两次无新证据」停点；预算未到期。

## 8. 人工 review ② 收口包（T061，2026-10-20）

> 本节是 T061 的交付物：七项证据、四段变更总结、判据面变更逐类、记录 / 台账索引、乙类两栏与未验证 / 不适用 / 丙类 / W3 清单。独立复核（T060）结果见 §8.1。**AI 侧收口完成不代表人工 review ② 已通过**；本节状态为「待用户决定」。

### 8.1 T060 独立复核

独立上下文（子代理）复核，输入为冻结规格（模块卡 §2.6 / §3 / §4 / §4.5 / §6 + spec FR/SC + tasks DoD）与判据面 diff，不读作者叙述。日期 2026-10-20；结论 **阻断（BLOCKING）**，8 条发现。以下逐条处置，修复后复跑证据（§8.4 / §8.5）。

| ID | 严重度 | 规格锚点 | 发现（证据 `file:line`） | 处置 |
| --- | --- | --- | --- | --- |
| F-01 | 阻断 | 模块卡 §2.6:450 / §2.6:461 ① / §4.2:992/996/997 | `current.json` 的 `015-backfill.required_tests` 只有 11 个顶层名；`us15-real.json` 两个 check 只有父名；`matchCatalogToChecks` 用 `topLevelTest` 投影自洽（`stages_test.go:668`），反例只删顶层名 | **已修复**：三段改为目录逐字集合（87 / 11 / 2），删除投影改逐字相等，新增 `deleted_subtest_is_red` 反例；反例判别力经 `-overlay` 逆改（恢复投影）实测红 |
| F-02 | 阻断 | §4.2:992 / §6.3 | 4 条运行记录无发布报告副本；`reportForRecord` 只读 `evidence.report_path`（`record.go:360-373`）；`needsReport` 只覆盖人工项（`record.go:488-490`）；无 `.loop` 的干净副本运行 `015-acceptance-infra` 在 `records_test.go:406` 失败 | **已修复**：4 份运行报告发布至 `records/reports/<record_id>.json`（哈希与 `report_sha256` 逐一相等）；解析改为发布副本优先、回退原始路径；`needsReport` 统一覆盖通过 / 失败记录；配对测试扩展到运行记录并校验哈希；无 `.loop` 干净副本三条关键测试 PASS |
| F-03 | 高 | §6.3:1286-1290 | 唯一已发布的 current 运行是 `us15-t039-backfill-r3`（failed，缺文件瞬态）；绿的本地 current 运行未发布、台账无行；CI 链接因未推送不存在；T061 未勾 | **已处理**：F-01 修复后重跑 current stage（`20261019T203742-current`，33/33 PASS，退出 0），记录 + 同次报告 + 台账行已发布并过发布校验（§8.4）；CI 链接保持**未验证**（推送时机归用户），T061 待人工 review ② |
| F-04 | 中 | §3.4:942-969（报告字段行）/ §3.4 结论「既有断言逐条保留」 | `acceptance_test.go:415-425` 把 `NotContains(raw,"environment")` 换成四项环境键精确断言，未登记 | **已登记**：旧断言与 §3.4 强制新增 `environment` 字段直接矛盾，属被迫替换；新断言以 `Len(env)==4` + 四键白名单保留「不采集任意环境变量」原意，严格强于旧断言。见 §8.3.3 判据面表 |
| F-05 | 低 | 用户 2026-10-20 的 `#nosec G204` 裁决 | 抑制注释称 `loadStage` 守卫路径、历史 stage 哈希钉住，实际 `lintStages` 用 `readJSON` 读全部 stage；`transformListArgs` 保留 `-exec` / `-toolexec` / `-overlay` | **已修复**：`transformListArgs` 拒绝三个可替换执行体旗标（新增 3 条反例，`-overlay` 逆改实测红）；`#nosec` 注释改写为真实信任边界（仓库受控输入、固定 `go test -list`、不经 shell）。抑制点仍为 1 处，用户已批准 |
| F-06 | 低 | §4.5 次数上限 | 模型调用 / 飞书发送的次数封顶是事后判红（`real_test.go:238-258`、`:651-653`），不能阻止超限调用本身 | **已记录边界**：常量注释写明事后执法与实测用量（21/30 次调用、1/5 次发送），§8.5 同步；事前逐次封顶需生产代码回调，§4.5 范围外 |
| F-07 | 低 | §4.5:1145-1231 | `harness/README.md` / `harness/project.md` 未写明 `us15-real` 仅收口授权后执行、不进 current / CI | **已修复**：两处补句（README「阶段清单与 lint」节、project.md「外部 stage」行） |
| F-08 | 低 | §4.2:1005-1013 | README 称 current 失配 fail-closed「退出 2」，实现为断言失败退出 1、基础设施错误退出 2（`stagelint.go:186-207`） | **已修复**：README 改为「缺失退出 1；读取 / 变换 / `-list` 失败退出 2」 |

**未覆盖面（复核者声明的 10 项，沿用原文摘要）**：未读 run-state（按纪律禁止）；未执行验收 / `make check` / CI / 真实 / Linux / 慢速项；干净检出模拟为部分副本；未重算全部记录指纹与报告哈希；私有文档不在 git、仅内容核对；未运行六条不变量 / Linux 分支 / lint 检出反例；未对 129 项做语义级复核；未运行格式 / 静态检查 / markdown；凭证扫描只覆盖已发布记录与台账；CI 唯一性只按代码与测试存在性核对。以上由控制侧在 §8.2 / §8.4 的证据中补齐，其中 Linux / CI / 真实项的可执行性边界见 §8.6。

**总结论**：8 条发现全部处置，其中 F-01 / F-02 为阻断项，修复后重新生成 current 运行证据并复跑判据测试；无静默丢弃。修复后的复核面变化（新增 3 条 lint 反例、1 条子测试反例、运行记录配对断言、发布报告哈希断言）均为**新增**，未删除或放宽既有断言。

### 8.2 七项证据

| # | 证据项 | 读数 | 证据位置 |
| --- | --- | --- | --- |
| 1 | 门禁七目标全绿 | `make check` 退出 0；fmt / lint / sec / test / test-integration / cover / build 全通过（覆盖率 core 86.67、tool 85.79、memory 91.32、storage 79.84，均 ≥70；其余包见 §7.1）。T060 F-01 / F-05 修复后于**最终树复跑**同样退出 0（`check-coverage: 全部包达标`、`CGO_ENABLED=0 go build` 成功） | §7.1，2026-10-20 实测（两次） |
| 2 | 文档 harness 映射逐条 | current stage 33/33 PASS（run `20261019T203742-current`，退出 0，已发布）；`015-signal-linux` PASS（122.333 s）；`015-slow-timeout` PASS；`015-real-model` / `015-real-feishu` 具名 PASS；八条回填 / 绑定 / 三视图 / 冻结测试 2026-10-20 复跑 PASS（读数见 §8.4） | §7.2、§8.4 |
| 3 | 交付物存在性 | 逐项核对通过：`scripts/acceptance` 修改 5 个 Go 文件 + 新增 7 个；`internal/e2e` 17 个文件（`doc.go` + 16 个 `_test.go`）；4 个 stage 文件；`harness/acceptance/` 发布面；`ci.yml` 三步；`harness/README.md` 与 `docs/DevelopmentLoop.md` 已改（私有文档，文件哈希核对） | 本节「交付物核对」 |
| 4 | 前序模块回归 | `make check` 的 test / test-integration 退出 0（全包 `go test -race -cover ./...`）；current stage 中 US-8 / 9 / 10 共 29 个历史 check 全 PASS，历史 stage 文件零 diff（`git diff --stat` 空，退出 0） | §7.1、§7.2 S3 |
| 5 | 全局不变量 | `015-invariants` 六条 PASS：禁 Agent 框架坐标 / 无进程本地时区 / 发布面零明文凭证 / 无 ORM / 测试专用依赖不进发布二进制 / CI 运行目录唯一 | `harness/stages/current.json:802-820` |
| 6 | 剩余人工项 | 乙类已授权 15 项已执行并发布记录；未授权、缺环境、不适用、丙类逐条列于 §8.5 / §8.6，无静默遗漏 | 本节 |
| 7 | 变更总结四段 | 见 §8.3（改动点 / 重点 review 清单 / 判据面变更 / 如何验证） | 本节 |

**交付物核对**（2026-10-20 实测）：

| 组 | 核对结果 |
| --- | --- |
| `scripts/acceptance` 修改 | `main.go` / `execute.go` / `config.go` / `fingerprint.go` / `acceptance_test.go`（+352 / −70，含 `ci.yml`）；签名适配未删断言 |
| `scripts/acceptance` 新增 | `record.go` / `stagelint.go` / `record_test.go` / `stagelint_test.go` / `stages_test.go` / `records_test.go` / `invariants_test.go` |
| `internal/e2e` | 17 个：`doc.go` + 16 个 `_test.go`（含 `real_test.go`；build tag 分层 e2e / e2e+linux / e2e+slow / e2e+real） |
| stage 文件 | `harness/stages/current.json`（sha256 `6d871fef…`，T060 F-01 字面名修订后）、`us15-linux.json`（`d6e0bfa7…`）、`us15-slow.json`（`599dcbb2…`）、`us15-real.json`（`73311d6f…`，T060 F-01 修订后） |
| 发布面 | `harness/acceptance/ledger.md`（134 数据行）、`records/`（134 条）、`records/reports/`（134 份）、`records/README.md` |
| 私有文档 | `harness/README.md`（`--lint-stages` / 记录与发布节已就位）；`docs/DevelopmentLoop.md` §5.1 第 6 项已替换为「AI 默认执行可判定项并留证」口径（`docs/DevelopmentLoop.md:436`）；模块卡未改（sha256 `19b70290…`，与 review ① 所审一致） |

### 8.3 四段变更总结

#### 8.3.1 改动点

| 组 | 文件 | 动机 |
| --- | --- | --- |
| CI | `.github/workflows/ci.yml`（修改，+21 行） | 在既有 test job 内插入 lint-stages / current / linux 三步；所有 `--out`（含既有 ci）加 `${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}`；不新增 job、不改 job 名 |
| 执行器 | `scripts/acceptance/main.go` | 新增 `--stage-only` / `--lint-stages` / `--run-kind` / `--executor` / 授权双旗标；stage-only 与显式 profile 冲突退出 2；`--out` 必须在仓库根内 |
| 执行器 | `scripts/acceptance/execute.go` | `accept` 返回报告、`checkTests` 同时返回 PASS / SKIP；报告加 run_id / 指纹 / 基线 / 环境 / 二进制哈希；日志路径序列化投影为仓库相对 |
| 执行器 | `scripts/acceptance/config.go` | stage 增加 `superseded_checks`；check 增加 `level`（source / process / external） |
| 执行器 | `scripts/acceptance/fingerprint.go` | 记录与台账排除出源码指纹（发布记录不得改变自身指纹） |
| 执行器 | `scripts/acceptance/acceptance_test.go` | 签名适配 + `TestAccept_ReportPathsRelative` 等新增用例；既有断言保留 |
| 记录工具 | `scripts/acceptance/record.go`（新增） | 生成 run / manual_item 记录；层级不降级、授权绑定、绑定校验、脱敏与路径投影 |
| 记录工具 | `scripts/acceptance/stagelint.go`（新增） | required_tests 只读存在性预检；`-list` 变换、子测试名截断顶层；current 失配 fail-closed |
| 判据测试 | `record_test.go` / `stagelint_test.go` / `stages_test.go` / `records_test.go` / `invariants_test.go`（新增） | 记录规范、lint 判别力、冻结全集双向相等、执行绑定与三视图、六条仓库不变量 |
| 回填夹具 | `internal/e2e/`（新增 17 文件） | 真实二进制 + 临时工作区 + 回环假服务；平台中立 / Linux 信号 / 300s 慢速 / 真实外发分层隔离 |
| Harness | `harness/stages/current.json`（新增） | 33 个现行 check；US-8 失配项以 `superseded_checks` 映射到 `008-ports-ten` |
| Harness | `harness/stages/us15-{linux,slow,real}.json`（新增） | 平台 / 慢速 / 外部授权独立执行，均不进入 current 与 CI |
| 发布面 | `harness/acceptance/{ledger.md,records/,records/reports/,records/README.md}`（新增） | 记录与同次报告成对发布、台账追加、发布前绑定校验 |
| 私有文档 | `harness/README.md`、`docs/DevelopmentLoop.md` | 用法与记录规范；收口执行口径修订（用户 2026-10-19 裁决，见 §1 基线） |
| 规格 | `specs/015-acceptance-infra/`（新增） | spec / clarify / plan / tasks / research / data-model / quickstart / run-state |

#### 8.3.2 重点 review 清单（按风险排序）

1. **记录 ↔ 同次报告绑定（C-12）**：`scripts/acceptance/records_test.go` 的 `TestRecords_BackfillBindsToExecution` 必须从同次报告的具名 PASS 取证，而不是读记录自身结论；报告被改写、换运行、换 check、指纹 / 平台 / 二进制不符均须判红（今日复跑 PASS，七类反例全部变红）。
2. **层级不降级与外部授权（C-14 / C-15）**：`scripts/acceptance/record.go` 中 process / external 缺 `binary_sha256=` 即基础设施错误、不降级；`main.go` 的 external 缺授权、非 external 携带授权、报告与记录授权不一致均退出 2、不生成记录。
3. **stage lint 只读预检与 current fail-closed**：`scripts/acceptance/stagelint.go`；单点抑制 `#nosec G204`（`stagelint.go:113`）经用户 2026-10-20 批准，理由四条写在注释（常量 `go`、受控 stage 文件、只读 `-list`、无代码级消除方式），已进 §7.1 报告。T060 F-05 后 `transformListArgs` 拒绝 `-exec` / `-toolexec` / `-overlay`（3 条反例），注释改写为真实信任边界。
4. **冻结与零漂移**：`scripts/acceptance/stages_test.go` 的 `requiredBackfillItems`（114）与 `requiredRealItems`（15）独立于目录书写；`TestStages_HistoricalFilesPinned` 按 LF 归一哈希钉住历史 stage；`git diff` 对历史 stage 为空。
5. **路径投影与 `--out` 守卫（FR-008）**：`execute.go` 序列化投影为仓库相对、`/` 分隔；`main.go` 拒绝仓库根外输出；`TestAccept_ReportPathsRelative` 双平台验证。
6. **CI 运行目录唯一化**：`ci.yml` 的 `RUN_ID-ATTEMPT` 后缀 + `TestRepoInvariants_CIRunDirsUnique`；job 名与既有上传步骤未变。

#### 8.3.3 判据面变更（逐类，对应模块卡 §3.4）

| 类别 | 变更 | 判定 |
| --- | --- | --- |
| 接口签名适配 | `accept` 增加返回值；`checkTests` 返回 PASS / SKIP | 内部接口；既有断言逐条保留，唯一例外见下行 |
| 报告环境断言（T060 F-04） | `acceptance_test.go` 的 `NotContains(raw,"environment")` 替换为 `Len(env)==4` + `os/arch/go_version/ci` 四键精确断言 | §3.4 报告字段行强制新增 `environment`，旧断言与之直接矛盾、无法保留；新断言保留「不采集任意环境变量」原意且更严格（白名单而非缺席） |
| 报告结构 | 新增 `run_id` / `source_fingerprint` / `base_commit` / `dirty` / `environment` / `tested_binary_sha256` / `level` / `passed_tests` / `skipped_tests`；日志路径投影 | 字段追加；判卷逻辑不变；required SKIP 仍判失败 |
| 指纹面 | 排除记录与台账 | 证据目录不计入源码指纹；判据输入仍在指纹内 |
| stage 结构 | `superseded_checks`、`level` 可选键 | 缺省 `level=source`，既有 stage 判据不变 |
| 新增工具 | `record.go` / `stagelint.go` 与 CLI 旗标 | 判据面新增；既有调用行为不变 |
| external 授权面 | 层级增加 `external`；授权双旗标 | 判据面新增；既有 source / process 不变 |
| 新增判据测试 | 5 个测试文件 + `acceptance_test.go` 新用例 | 判据面新增；未删改既有断言 |
| 新增回填测试 | `internal/e2e` 17 文件 + build tag 选择面 | 判据面新增；`make check` 七目标不变 |
| 新增 stage | current 的 4 个 `015-*` check 与 3 个独立 stage | 判据面新增；历史 check 原样保留 |
| CI | 三步插入 + 运行目录唯一后缀 | 判据面新增；不删既有步骤、不改 job 名 |
| 白名单抑制 | `#nosec G204` 单点（`stagelint.go:113`） | 经用户 2026-10-20 批准并进验收报告；其余门禁抑制为零。T060 F-05 后参数面进一步收紧（拒绝 `-exec` / `-toolexec` / `-overlay`，新增 3 条反例），抑制理由改写为真实信任边界 |

**结论**：除 T060 F-04 一处**被迫替换**（旧断言与 §3.4 强制新增字段直接矛盾；新断言更严格，属替换而非放宽）外，判据面**零删除、零跳过、零放宽**；`make check` 七目标与 `harness/acceptance.json` profile 未变。

#### 8.3.4 如何验证（可直接复制执行）

```sh
# 1) 全量门禁（七目标）
make check

# 2) stage lint（预期：退出 0，末行 `stage lint: ok`，3 条 WARN——
#    us15-linux 两项 Linux-only 子测试在 Windows 不可见、us8 历史失配一项；2026-10-20 复跑实测）：
#    WARN harness/stages/us15-linux.json 015-signal-linux missing TestBackfill_SignalLinux/grace_exhausted
#    WARN harness/stages/us15-linux.json 015-signal-linux missing TestBackfill_SignalLinux/sigterm_order
#    WARN harness/stages/us8-notify-r1.json 008-ports-nine missing TestToolListerPort_HeavyAssemblyRegistersNineTools
sh scripts/check-acceptance.sh --lint-stages --stage harness/stages/current.json

# 3) 现行累计 stage（33 个 check 全 PASS）
sh scripts/check-acceptance.sh --stage harness/stages/current.json --stage-only --out .loop/runs/<new-dir>

# 4) 记录 / 冻结 / 绑定 / 三视图（2026-10-20 复跑读数见 §8.4）
GOPROXY=https://goproxy.cn go test ./scripts/acceptance -count=1 -v -run 'TestRecords_|TestStages_|TestRepoInvariants_'

# 5) Linux 信号（WSL；Windows 原生保持未验证）
wsl.exe -d Ubuntu -- bash -lc 'cd /mnt/d/code/basalt && sh scripts/check-acceptance.sh --stage harness/stages/us15-linux.json --stage-only --out .loop/runs/<new-dir>'

# 6) 300s 慢速超时
sh scripts/check-acceptance.sh --stage harness/stages/us15-slow.json --stage-only --out .loop/runs/<new-dir>

# 7) 真实项（仅授权后；凭证只经运行时环境注入，值不落盘）
#    BASALT_REAL_LLM_BASE_URL / BASALT_REAL_LLM_MODEL / BASALT_REAL_LLM_API_KEY / BASALT_REAL_FEISHU_URL
sh scripts/check-acceptance.sh --stage harness/stages/us15-real.json --stage-only \
  --authorization-ref user-2026-10-19-real-llm-webhook \
  --authorization-scope "real-llm:openai-compatible; real-webhook:feishu; synthetic-data-only; model-calls<=30; cny<=10; sends<=5" \
  --out .loop/runs/<new-dir>
```

### 8.4 记录 / 台账索引

**总量**：134 条记录 = 5 条 run + 129 条 manual_item（114 process + 15 external）；134 份报告（含 5 份运行报告）；`ledger.md` 134 数据行。记录发布后只追加：T039 原始运行 `failed` 行保留在台账（bootstrap 期整体运行失败属预期，见 §6 A2），其目录项由同次 `passed` 逐项记录承载。

| run_id | stage | 结论 | 目录项数 | 记录前缀 |
| --- | --- | --- | --- | --- |
| `us15-t039-backfill-r3` | current-tree（原始回填） | run 级 failed（留证） | 111 | `us15-t039-backfill-r3-*` |
| `us15-t040-linux-r1` | us15-linux | passed | 2（`us9-6.1-6a`、`us10-6.2-15a`） | `us15-t040-linux-r1-*` |
| `us15-t041-slow-r1` | us15-slow | passed | 1（`us10-6.2-12b`） | `us15-t041-slow-r1-*` |
| `20261019T194329-real` | us15-real | passed | 15 | `20261019T194329-real-*` |
| `20261019T203742-current` | current-tree（T060 F-03 复跑） | passed（33/33，退出 0） | 33 check | 无逐项记录（run 级） |

**未发布运行（过程说明）**：`20261019T203445-current` 在运行中途修改了 `internal/e2e/real_test.go`（F-06 注释更正），源码指纹与启动快照不符，按 `source_changed` 语义该轮不能作为绿色证据；已在完成前终止并清理锁与残留进程，未发布记录 / 报告 / 台账行。重跑 `20261019T203742-current` 使用的源码指纹 `837198ab…` 经外部探针按 `fingerprint.go` 同算法复算一致，运行期间无源码变化。

**目录项按 US 分布**（`backfillCatalog`，共 129）：US-1 8、US-2 7、US-3 7、US-4 14、US-5 8、US-6 13、US-7 16、US-8 9、US-9 26、US-10 21。逐项 ID 到测试名的映射以 `scripts/acceptance/stages_test.go` 的目录为准；每条记录的 `subject.items` 与 `subject.test` 逐字回链，台账按 `record_id` 可查。

**今日复跑读数**（2026-10-20，发布新运行记录后执行 `go test ./scripts/acceptance -count=1 -v -run 'TestRecords_|TestStages_|TestRepoInvariants_'`，19 条具名测试全部 PASS，整包 `ok … 2.490s`）：

| 测试 | 结果 | 说明 |
| --- | --- | --- |
| `TestRecords_BackfillCoversCatalog` | PASS | 目录覆盖 / 掉项变红 / 未登记记录变红 / 未验证不计入 |
| `TestRecords_BackfillBindsToExecution` | PASS | 七类绑定反例（无 PASS 事件 / 错 check / 错运行 / 改写报告 / 指纹 / 平台 / 二进制）全部变红 |
| `TestRecords_RealBindsToExecution` | PASS | 乙类 15 项记录与同次真实报告逐项绑定 |
| `TestRecords_StatusDoesNotMaskConflicts` | PASS | 跨平台失败不被通过掩盖 / 用户复跑入状态 / 同键冲突双保留 / 新未验证不替换通过 |
| `TestRecords_PublishedConform` | PASS | 全部 passed / failed 记录均有发布报告副本，副本哈希与 `report_sha256` 相等（含 5 条运行记录） |
| `TestRecords_NoSecretsOrAbsolutePaths` | PASS | 记录与报告零凭证形态、零盘符或绝对路径 |
| `TestStages_HistoricalFilesPinned` | PASS | 历史 stage LF 归一哈希钉住 |
| `TestStages_CurrentCoversHistorical` | PASS | 历史 check 全覆盖 + 替代项显式 |
| `TestStages_CurrentMatchesHistorical` | PASS | current 中历史 check 与历史 stage 逐字段一致 |
| `TestStages_PinsUS15Checks` | PASS | 新 check 与 US-8 替代项定义钉住 |
| `TestStages_BackfillCatalogMatchesRequiredTests` | PASS | required_tests 与目录逐字一致（四类反例变红，含 `deleted_subtest_is_red`） |
| `TestStages_BackfillCatalogCoversRequiredSet` | PASS | 目录 ↔ 冻结全集双向相等（同时删项的共谋反例仍红） |
| `TestStages_RealCatalogCoversAuthorizedSet` | PASS | 乙类目录 ↔ 冻结全集双向相等 |

**冲突项**：无。已发布记录中不存在同一目录项的相反结论；唯一 `failed` 是 T039 run 级记录（见上）。

### 8.5 乙类清单（两栏）

**已授权（§4.5，`ref = user-2026-10-19-real-llm-webhook`）**：全部 `passed`；`authorization` 与报告逐项相等；`tested_binary_sha256 = 5a98546b…`；同次报告 sha `20949b88…`。

| 承载 check | 目录项（记录 ID 后缀） | 数量 |
| --- | --- | --- |
| `015-real-model` | `us2-6-3a`、`us4-6.1-2b`、`us4-6.1-2d`、`us4-6.1-2j`、`us5-6.1-2a`、`us6-6.1-2b`、`us7-6.1-2b`、`us7-rsU3-1b`、`us9-6.2-5a`、`us10-6.2-1c`、`us10-6.2-4c`、`us10-6.2-5c`、`us10-6.2-16c` | 13 |
| `015-real-feishu` | `us8-6.1-5a`、`us8-6.1-7b` | 2 |

**用量**：模型调用 21 ≤ 30；飞书发送 1 ≤ 5；tokens 10701（读数见 §7.2 S8，run `20261019T194329-real`）。**费用未测量**——无计费读数，按调用次数与 token 用量远低于授权上限判断（CNY 10 为授权上限，非实测值）。输出 / 记录 / 报告只出现 `scheme://host`。

**次数封顶的执法边界（T060 F-06）**：模型调用与飞书发送的上限是**事后累计判红**——模型调用在每个子测试结束后按 `llm_calls` 行数累计，飞书发送在返回后累计；超限当轮失败，但已发生的调用 / 外发无法撤回。事前逐次封顶需要生产代码提供逐次回调（§4.5 范围外、且会改动生产路径），本 US 不做；实测用量 21/30、1/5 远低于上限。边界同样写在 `internal/e2e/real_test.go` 的上限常量注释中。

**未授权（保持未验证，缺失授权逐条）**：

| 项目 | 缺失授权 / 原因 |
| --- | --- |
| US-1 GitHub 分支保护 | 平台授权未给 |
| US-3 NivroOS 跨仓对拍 | 跨仓授权未给 |
| US-5 真实天气 API 外发 | 第三方服务未授权 |
| 其他未给凭证的真实外发 | 无明示授权；不得以已授权项替代或扩大解释 |

### 8.6 未验证 / 不适用 / 丙类 / W3

**未验证（缺环境 / 缺依赖 / 平台限制，不冒充通过）**：逐条见 §7.3 表——US-4 token 估算（缺分词器依赖）；US-7 真实第三方 MCP server（缺外部安装）；US-9 第二主机（缺第二台主机）；Windows 原生 SIGTERM / Ctrl+C 事件序（平台限制，Linux 侧经 WSL 已通过）；US-10 15b（平台限制）；US-8 飞书 `code=19024`（关键词安全未确认）；US-4 Demo③ / ④ / ⑦ 与 §6.1 第 3 项（模型 / provider 行为不确定）；真实 CI 运行（推送前不可执行，推送后回填 Actions 结论与运行目录后缀）。

**不适用（`na_basis`）**：

| 项 | `na_basis` |
| --- | --- |
| US-10 §6.2 第 6 项 6c（等换季日） | US-10 原文明写不要求 |
| US-9 §6.2 第 11 项反向代理 ACAO 路径 | 原文标注「（可选）」；默认拒绝半由 `TestBackfill_Web/cross_origin` 覆盖，未记为 N/A |
| US-1 / US-2 历史「`context.WithValue` 零使用」 | 已被宪法原则 7「只允许追踪标识」取代，现行两处为 request ID |
| US-1~10 历史一次性项（零迁移 / 零依赖 / 变动范围） | 历史项一次性判据，本 US 零迁移 / 零新增生产依赖，逐项不适用 |

**丙类（用户裁决）**：review ② 对证据充分性的判断；US-4 / US-5 / US-6 / US-7 的真控制台观感；US-6 部署侧备份面口径；NivroOS 差异的可接受性；文档真实性判断。

**W3（后续波次，非本 US 范围）**：超出回填需要的进程级场景；故障注入扩展（仅回填必需的已随本 US 完成）；fuzz / 属性测试。

### 8.7 收口状态

- AI 侧：T057 七目标、T058 quickstart 1–10、T059 登记、**T060 独立复核**（8 条 findings 全部处置，F-01 / F-02 阻断项修复后按逆改红态与干净副本复验）、**T061 收口包**（§8.1–§8.6）均已完成；绿色 current 运行 `20261019T203742-current` 已发布（134 条记录 / 134 份报告 / 134 行台账，lint 3 条 WARN 均为已知历史项）。AI 侧收口完成。
- 人工侧：**待用户 review ② 决定**。未收到决定前不得写成「人工接受」；用户可抽检任意记录（台账 → 记录 → 同次报告 → 原始日志）或亲自复跑 §8.3.4 的任一命令。
- commit / push / tag 仍归用户；真实 CI 结论在推送后回填。
