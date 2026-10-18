# Tasks: US-8 通知工具（notify）

**Input**: Design documents from `/specs/008-notify/`

**Prerequisites**: [plan.md](./plan.md)、[spec.md](./spec.md)（US1~US4）、[research.md](./research.md)（D1~D12）、[data-model.md](./data-model.md)、[quickstart.md](./quickstart.md)；唯一事实源 = 颗粒度文档 [docs/us/us8-notify.md](../../docs/us/us8-notify.md) §2.2（约束 24 条）/ §2.3（图 A~C + 伪代码，签名逐字）/ §3（交付物清单）/ §3.5（前序改造点 6 项）/ §4.3（R1~R12 + A4 / A10 关键回归）/ §6.2（固定口径 25 条）；固定字面量 = [docs/contracts/](../../docs/contracts/)（`agent-md.md` §5.5 `notify` 契约、`run-and-errors.md` §7.2 / §7.3、`schema.sql` `notify_channels`）。

**Tests**: **必须包含**——本仓「harness 先行」是硬纪律：**新增 5 文件 41 条 + 修改 1 文件 1 条 = 42 条**（`core/envref_test.go` 1 / `storage/notify_store_test.go` 4 / `tool/notify_test.go` 21 / `tool/notify_adapters_test.go` 13 / `app/notify_test.go` 2 / `app/ports_test.go` 修改 1）。**关键回归独立落点**：R1~R12 + A4 / A10 逐条对应到测试任务并注明**逆改点**；实现与测试同批推进，**不得**先实现后补测。

**Organization**: 任务按 spec 的四个用户故事分组（US1~US4），每个故事可独立验证。**标签口径**：`[US1]`~`[US4]` 是**本 feature 内部**故事编号，**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）的 US-1~US-8；四个故事同属项目 **US-8** 一个模块。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属用户故事（US1 / US2 / US3 / US4）；Setup / Foundational / Polish 阶段不带标签
- 每条任务给出精确文件路径与可执行 DoD

## 任务阶段口径：测试准备 / 实现 / 验证（**不得**混记）

每个任务**只承担一个阶段**，DoD 只登记本阶段读数——**不得**把「准备好测试」记成「测试已通过」：

| 阶段 | 承担者 | 本阶段交付与 DoD 读数 |
| --- | --- | --- |
| **测试准备** | T003 / T006 / T009 / T012 / T015 / T018 / T019 / T020 / T021 | 测试**落地** + 登记**红态读数**（逐字构建错误或逐字失败文案）+ 写明**转绿前提 = T0xx**；**不得**写「转绿 / PASS」 |
| **实现** | T004 / T005 / T007 / T008 / T010 / T013 / T016 / T022 / T023 | **由红转绿**——绿态读数登记在实现任务 DoD，并点名它让哪几条测试任务由红转绿 |
| **验证** | T011 / T014 / T017 / T024 / T025 / T026 | 本故事**全绿** + 关键回归逐条绿态读数 + **逆改点红态复现**（`go test -overlay` 逆改，探针建在**仓库外**、用毕即删） |

**红态口径**：测试先于实现落地 → 登记自然红态；行为已在更早故事落地（跨故事复验的用例）→ 用 `go test -overlay` 逆改实现取得红态逐字读数，**不得只看退出码**。两种读数都必须能指出**逆改点**（实现怎么改它会红）。

## Path Conventions

单 Go module：`cmd/basalt/`、`internal/`。测试与被测包**同目录**；测试包归属（动手前逐文件核）：

| 测试文件 | 包名 | 说明 |
| --- | --- | --- |
| `internal/core/envref_test.go` | **`core_test`** | 外部包——只用导出 `core.EnvPlaceholderName` |
| `internal/storage/notify_store_test.go` | **`storage`**（内部包） | 与 `memory_store_test.go` 同包先例，直接开库 |
| `internal/tool/notify_test.go` | **`tool`**（内部包） | 白盒：直接调非导出构造缝 / 判专用错误类型 / 复用 `recordingAudit`（`mcp_test.go`） |
| `internal/tool/notify_adapters_test.go` | **`tool`**（内部包） | 白盒：非导出构造器注入 `fakeDoer` / 断言客户端 `Proxy == nil` |
| `internal/app/notify_test.go` | **`app`**（内部包） | 直接调非导出 `newNotifyAdapters` |
| `internal/app/ports_test.go` | **`app`**（内部包） | 既有文件，改 1 条 |

**测试缝纪律**：适配器经非导出 `newWebhookNotifyAdapter(doer)` / `newFeishuNotifyAdapter(doer)` 注入 `fakeDoer`；工具经 `lookupEnv` 注入环境查询——**不得**新增导出测试 API。`contracts/` **不新建**。每个 `go` 命令显式带 `GOPROXY=https://goproxy.cn`。**记账口径**：代码 新增 **6** + 修改 **3**；测试 新增 **5 文件 41 条** + 修改 **1 文件 1 条**；`go.mod` / `go.sum` / `schema.sql` / `basalt.yaml*` **零 diff**。

**前序改造点（§3.5 的 6 项）**：① T023、② T023、③ T023、④ T004 + T008、⑤ T020、⑥ T002/T026 核对。性质：**⑤ = 判据面变更**（八件 → 九件，人工 review ① 已批准）、**④ = 等价重构**（判据面零变更）、①②③ = 生产装配变更（由 R11 与 ports 用例覆盖）、⑥ = 零 diff 核对。均属已审范围，执行阶段不再逐条停车。

---

## Phase 1: Setup（共享前置）

**Purpose**: 环境与事实核实、零变更面基线——不涉任何用户故事

- [x] T001 写前核实与规范前置就位：`go version` 复核 `go1.27.2`；逐条读 [docs/EngineeringStandards.md](../../docs/EngineeringStandards.md) §1 Go / §2 测试 / §3 数据库 / §4 日志与错误；对照 [research.md](./research.md)「写前核实清单」复核 10 项读数（`EnvPlaceholderName` 两处既有实现、`newHTTPClient` 配置、分类判序、`fakeDoer` 夹具、`ports_test.go` 现态、飞书回执形态）。DoD：`go version` 退出码 0 且输出 `go1.27.2 windows/amd64`；规范前置四节全读；10 项复核逐条登记；探针（如有）建在仓库外、用毕即删（`git status --short` 零残留）
- [x] T002 [P] 零变更面基线取证：`git diff -- go.mod go.sum`（预期空）；`git diff -- internal/storage/schema.sql docs/contracts/schema.sql`（**零迁移**）；`git diff -- basalt.yaml basalt.yaml.example`（**零新增配置键**）；`git diff -- Makefile .github/ scripts/`（覆盖率阈值与七目标已在）；`ls -A internal/storage/migrations/`（仅 `.gitkeep`）。DoD：逐条读数粘贴；本 US 期间这些面**零改动**是收口硬判据

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: `core` 声明面（`${VAR}` 唯一判定 + 渠道纯类型/只读接口）、`storage` 只读仓储、两处既有调用点收敛——四个故事都建立在它之上

**⚠️ CRITICAL**: 本阶段完成前，任何用户故事都不得开工

- [x] T003 [P] `internal/core/envref_test.go`（新增 **1 条**，`package core_test`）——**测试准备**：`TestEnvPlaceholderName_ExactFormTable`（表驱动：接受 `${A}` / `${_A}` / `${A_1}` / `${aBc}`；拒绝 `$A` / `${}` / `${1A}` / `${A}${B}` / `${A}x` / `x${A}` / `${A-B}` / ` ${A}` / 空串）。DoD：落地 1 条；登记逐字构建错误（`undefined: core.EnvPlaceholderName`）；转绿前提 = T004；**不得**写「通过」
- [x] T004 `internal/core/envref.go`（新增——**纯标准库，零 import 内部包**）：`func EnvPlaceholderName(s string) (name string, ok bool)`，逐字节复刻 `workspace/mcp.go` 既有算法（长度 ≥ 4、前缀 `${`、后缀 `}`、首字符字母/下划线、其余字母/数字/下划线），关键注释中英并列。DoD：**由红转绿**——T003 **1 条 PASS**；`GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/core` 全绿；`go list -f '{{.Imports}}' ./internal/core` 只出标准库；`gofumpt -l` 空输出
- [x] T005 [P] `internal/core/notify.go`（新增——**纯类型，零 import 内部包**）：`type NotifyChannel struct { Name, Type, URL string }` + `type NotifyChannelReader interface { ListNotifyChannels(ctx context.Context) ([]NotifyChannel, error) }`；无方法、无测试（`[no statements]`）。DoD：`go build ./internal/core` 退出码 0；`notify.go` 可执行语句为零（`make cover` 既有规则跳过）；`gofumpt -l` 空输出
- [x] T006 `internal/storage/notify_store_test.go`（新增 **4 条**，`package storage`）——**测试准备**：S1 `TestNotifyChannelRepo_ListsRowsOrderedByName` · S2 `TestNotifyChannelRepo_EmptyRegistryReturnsEmptySlice` · S3 `TestNotifyChannelRepo_ContextErrorPropagates` · S4 `TestNotifyChannelRepo_SeesRowsInsertedAfterConstruction`（构造后新增行可见，无缓存）。DoD：落地 4 条；登记逐字构建错误（`undefined: NewNotifyChannelRepo`）；转绿前提 = T007
- [x] T007 `internal/storage/notify_store.go`（新增）：`NotifyChannelRepo{db *DB}` + `NewNotifyChannelRepo(db *DB) *NotifyChannelRepo` + `ListNotifyChannels`——SQL 固定 `SELECT name, type, url FROM notify_channels ORDER BY name`，走 `db.read`；空表返回空切片；读取失败原样上抛（包装在工具层）。DoD：**由红转绿**——T006 **4 条 PASS**；`go test -race -count=1 ./internal/storage` 全绿；`grep -n 'db.read' internal/storage/notify_store.go` 命中；`gofumpt -l` 空输出
- [x] T008 `internal/workspace/mcp.go` + `internal/cli/config.go`（修改——§3.5 改造点 ④，**等价重构**）：删除本地 `isEnvPlaceholder` 与 `wholePlaceholder` 正则变量，两处整串判定改调 `core.EnvPlaceholderName`；`cli/config.go` 的 `placeholder`（非锚定，用于串内替换）**保留**。DoD：`GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/workspace ./internal/cli` **零测试改动全绿**（`TestIsEnvPlaceholder_Table` / 凭证键用例逐字节不变）；`git diff --stat internal/workspace/mcp_test.go internal/cli/config_test.go` 为空；`grep -rn "func isEnvPlaceholder\|wholePlaceholder" internal` 零命中（注释除外）；`gofumpt -l` 空输出

**Checkpoint**: `${VAR}` 唯一判定、渠道类型/接口、只读仓储、两处收敛就位——US1 可开工

---

## Phase 3: User Story 1 - 登记渠道后一次推送闭环（Priority: P1）🎯 MVP

**Goal**: 给定已登记渠道与 `channel` 命中、`content` 非空的调用：发送端收到一次投递、工具返回固定成功文案、执行器审计经历 `pending → 成功` 两阶段；零外呼面不访问真实网络。

**Independent Test**: 假注册表 + 假发送端 + 假策略驱动工具与执行器：①发送端恰好收到一次调用（解析后的真实地址 + 原文内容）；②结果 = `已推送到通知渠道：ops` 且不可重试；③审计行先 `pending` 后成功。

### Tests for User Story 1

- [x] T009 [US1] `internal/tool/notify_test.go`（新增首批 **3 条**，`package tool`）——**测试准备**：N1 `TestNotifyTool_InputSchemaRequiresContentOnly` · N17 `TestNotifyTool_DispatchesResolvedContentToAdapter`（R6 的调用面：解析后 URL + 原文 content）· N19 `TestNotifyTool_ExecutorAuditsBeforeAdapterSend`（**R12**）；同批落辅助件 `fakeNotifyChannels` / `recordingNotifyAdapter` / `notifyPolicySpy` / `newTestNotifyTool`（§4.3 定义）。DoD：落地 3 条 + 辅助件；登记逐字构建错误（`undefined: NewNotifyTool` 等）；转绿前提 = T010；**不得**写「通过」

### Implementation for User Story 1

- [x] T010 [US1] `internal/tool/notify.go`（新增：成功路径）+ `internal/tool/notify_adapters.go`（新增：仅 `NotifyAdapter` 接口与载荷类型骨架）：`maxNotifyContentBytes = 20*1024`；`NotifyTool{channels, policy, adapters, lookupEnv}` + `NewNotifyTool`（克隆映射；`lookupEnv` 缺省 `os.LookupEnv`）+ 四方法、内联 schema（`required:["content"]`）；`notifyInput{Channel string; Content *string}` + `decodeNotifyInput`；`findNotifyChannel`（TrimSpace + 大小写敏感精确）；`EnvPlaceholderName` → `lookupEnv` → `policy.Check(ctx, Action{HTTP, 真实 URL, env.AgentName})` → `adapters[ch.Type].Send` → `Result{"已推送到通知渠道：" + ch.Name, Retryable:false}`。**本任务只做成功路径**：失败分支返回临时普通错误，文档化的错误文案 / 类型 / 分类在 T013（US2）与 T016（US3）落地。DoD：**由红转绿**——T009 **3 条 PASS**；`go test -race -count=1 ./internal/tool ./internal/core ./internal/storage` 全绿；`gofumpt -l` 空输出
- [x] T011 [US1] **US1 验证**：复跑 `GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/tool ./internal/core ./internal/storage`；**必跑**既有 `TestToolExecutor_PreWriteFailureRefusesExecution` 与 `TestToolExecutor_TerminalWriteFailureKeepsPendingRow`（`go test -count=1 ./internal/core -run '^(TestToolExecutor_PreWriteFailureRefusesExecution|TestToolExecutor_TerminalWriteFailureKeepsPendingRow)$'`，**不重造**）；对 N19 做逆改点红态复现（`-overlay`：适配器 Send 改到审计预写之前 → N19 红），探针建在仓库外、用毕即删。DoD：三包全绿；两既有执行器用例 PASS；N19 红→绿读数各一行；`git status --short` 无探针残留

**Checkpoint**: US1 独立可验——正向闭环走通，审计两阶段成立

---

## Phase 4: User Story 2 - 无默认渠道、失败可诊断（Priority: P2）

**Goal**: `channel` 缺失 / 纯空白 / 查无 → `not_found` + 可用渠道名（空表说明注册表为空），零发送；`content` 20 KiB 边界在查表之前判定、空串放行；每次发送读一次注册表、无缓存。

**Independent Test**: 可计数的假注册表与假发送端：①三种渠道失败形态 → `not_found` + 可用名单 + 发送零次；②20 KiB 恰好放行 / 超 1 字节失败且注册表读取 0 次；③空串发送一次；④连续两次发送 → 读取两次；构造后新增行可见。

### Tests for User Story 2

- [x] T012 [US2] `internal/tool/notify_test.go`（追加 **10 条**）——**测试准备**：N2 `TestNotifyTool_MissingChannelIsNotFoundWithAvailableNames`（两子例：非空表列名 / 空表说明注册表为空）· N3 `TestNotifyTool_UnknownChannelIsNotFoundWithAvailableNames` · N4 `TestNotifyTool_BlankChannelTreatedAsMissing` · N5 `TestNotifyTool_ContentOverLimitFailsBeforeLookup`（**R5**）· N6 `TestNotifyTool_ContentAtLimitIsAccepted`（**R5**）· N7 `TestNotifyTool_ContentLimitCountsUTF8Bytes`（**R5**）· N15 `TestNotifyTool_RegistryReadFailureIsNotChannelMissing` · N16 `TestNotifyTool_ReadsRegistryPerSend` · N18 `TestNotifyTool_RejectsNonStringFields` · N21 `TestNotifyTool_EmptyContentIsSent`。DoD：落地 10 条；登记红态——新行为（N2~N5、N15、N18）逐字失败文案；已由 T010 覆盖的行为（N6 / N16 / N21）用 `-overlay` 逆改取得逐字红态（删上限判定 → N6 红；加缓存 → N16 红；拒绝空串 → N21 红）；转绿前提 = T013
- [x] T013 [US2] `internal/tool/notify.go`（改动）：补全失败面——解码错误 → `*InvalidInputError`；`content` 超 20 KiB 在**查表之前**判 `invalid_input`；`channel` 缺失 / 空白 / 查无 → `*notifyNotFoundError`（`Unwrap() → fs.ErrNotExist`，文案列出可用渠道名 `、` 连接，空表写明注册表为空）；注册表读取失败包装 `fmt.Errorf("读取通知渠道注册表失败: %w", err)`；未知类型 → `*notifyTypeUnregisteredError`（零 env / 零 policy / 零发送）。DoD：**由红转绿**——T012 **10 条全 PASS**；`go test -race -count=1 ./internal/tool` 全绿；N5 注册表读取次数 0、N21 发送一次逐字断言
- [x] T014 [US2] **US2 验证**：对 R3 / R5 做逆改点红态复现（`-overlay`：渠道未命中改回首条默认 → R3 红；上限检查移到查表之后 → R5 红且读取次数 > 0）。DoD：`./internal/tool ./internal/storage` 全绿；R3 / R5 红→绿读数各一行；探针用毕即删

**Checkpoint**: US2 独立可验——无默认渠道、诊断可用、边界顺序正确

---

## Phase 5: User Story 3 - 凭证红线与判定边界（Priority: P3）

**Goal**: `url` 只允许精确 `${VAR}`、发送时解析、未设置 / 空串 fail-closed；判定对象是解析后真实 URL 且先于发送；沙箱拒绝与传输错误文案只出 `scheme://host`，规则与指引原文保留；工具与适配器不持有 logger。

**Independent Test**: 明文 / 半截占位 → 失败且不回显；变量未设置 / 空值 → 点名 `${VAR}`；策略拒绝文案嵌完整 URL → 输出只含 `scheme://host`、Rule / Guidance 原文保留；策略一次、发送零次；传输错误含哨兵 token → 零命中。

### Tests for User Story 3

- [x] T015 [US3] `internal/tool/notify_test.go`（追加 **7 条**）——**测试准备**：N8 `TestNotifyTool_PlaintextURLFailsWithoutEcho`（**R4**）· N9 `TestNotifyTool_NearMissPlaceholderFailsWithoutEcho`（**R4**）· N10 `TestNotifyTool_UnsetEnvFailsClosed`（**R4**）· N11 `TestNotifyTool_EmptyEnvFailsClosed`（**R4**）· N12 `TestNotifyTool_PolicyDenialRedactsCredentialURL`（**R1**；断言 `spyPolicy` 两段原文保留）· N13 `TestNotifyTool_PolicySeesResolvedURLAndAgentBeforeAnySend`（**R2**；`recordingNotifyAdapter.onSend` 当刻断言 `judgmentCount() == 1`）· N20 `TestNotifyTool_ExecutorRecordsRuntimeErrorForUnsetEnv`。DoD：落地 7 条；登记红态——N12 / N13 新行为逐字失败文案；N8~N11 / N20 已由 T010 / T013 部分覆盖，用 `-overlay` 逆改取得逐字红态（占位判定改成前缀匹配 → N8/N9 红；空值当未设置 → N10/N11 红；判定移到发送之后 → N13 红）；转绿前提 = T016
- [x] T016 [US3] `internal/tool/notify.go`（改动）：补全凭证面——`notifyDisplayURL(rawURL)`（`scheme://host`；解析失败返回既有 `targetUnresolvable` 占位）；`redactNotifyPolicyError`（沙箱拒绝保留 `Rule` / `Guidance` 原文、只重写 `Target`；非沙箱错误原样上抛）；`url` 非精确 `${VAR}` → `runtime_error` 且不回显地址；环境未设置 / 空串 → 点名 `${VAR}`、不回显值；`env.AgentName` 显式传导；工具零 logger。DoD：**由红转绿**——T015 **7 条全 PASS**；`go test -race -count=1 ./internal/tool ./internal/core` 全绿；N12 / N13 的逐字断言（段 2 只有 `scheme://host`；判定次数 == 1）PASS
- [x] T017 [US3] **US3 验证**：对 R1 / R2 / R4 做逆改点红态复现（`-overlay`：脱敏改回透传完整 URL → R1 红；策略判定移到 Send 之后 → R2 红；占位判定改前缀匹配 → R4 红）。DoD：两包全绿；R1 / R2 / R4 红→绿读数各一行；探针用毕即删

**Checkpoint**: US3 独立可验——凭证红线三面（注册、解析、判定）全部成立

---

## Phase 6: User Story 4 - 投递真值与同形装配（Priority: P4）

**Goal**: webhook 2xx / feishu `code==0` 两套成功判据；回执不可解析 / 超 64 KiB 一律失败；不跟随重定向、不读环境代理、ctx 身份优先；`notify` 以第 9 件内置工具装配，映射恰好两键，轻 / 重路径一致。

**Independent Test**: `fakeDoer` 驱动两适配器：载荷 / 状态码 / 回执矩阵 / 超限 / 重定向 / 代理 / ctx / Content-Type；装配后工具清单九件、映射两键、轻路径接受 `notify` 声明。

### Tests for User Story 4

- [x] T018 [P] [US4] `internal/tool/notify_adapters_test.go`（新增 **13 条**，`package tool`）——**测试准备**：A1 `TestWebhookNotifyAdapter_PostsContentJSON` · A2 `TestWebhookNotifyAdapter_Non2xxIsExternalError` · A3 `TestWebhookNotifyAdapter_DoesNotFollowRedirects`（**R8**）· A4 `TestNotifyAdapters_ClientIgnoresEnvProxy`（**A4**；断言两个适配器持有客户端 `Transport.(*http.Transport).Proxy == nil`）· A5 `TestWebhookNotifyAdapter_TransportErrorRedactsURL`（**R1**）· A6 `TestWebhookNotifyAdapter_ContextIdentityWins`（**R9**）· A7 `TestFeishuNotifyAdapter_PostsTextPayload` · A8 `TestFeishuNotifyAdapter_RequiresCodeZero` · A9 `TestFeishuNotifyAdapter_UnparsableReceiptFails`（空体 / 非 JSON / 缺 code / `code:null` / 小数 / 尾随垃圾）· A10 `TestFeishuNotifyAdapter_OversizedReceiptFails`（**A10**；有效 `{"code":0}` + 超限空白）· A11 `TestFeishuNotifyAdapter_Non2xxFailsBeforeReceipt` · A12 `TestFeishuNotifyAdapter_ReceiptReadFailureRespectsContext`（**R9**）· A13 `TestNotifyAdapters_FixedContentType`。DoD：落地 13 条；登记逐字构建错误（`undefined: NewWebhookNotifyAdapter` 等）；转绿前提 = T022
- [x] T019 [P] [US4] `internal/app/notify_test.go`（新增 **2 条**，`package app`）——**测试准备**：P1 `TestNewNotifyAdapters_ExactTypeSet`（**R11**；恰好 `webhook` / `feishu` 两键）· P2 `TestAgentsPort_LightPathAcceptsNotifyDeclaration`（**R11**）。DoD：落地 2 条；登记逐字构建错误（`undefined: newNotifyAdapters` / 轻路径拒绝 `notify`）；转绿前提 = T023
- [x] T020 [P] [US4] `internal/app/ports_test.go`（修改 **1 条**——§3.5 改造点 ⑤，**判据面变更，人工 review ① 已批准**）：`TestToolListerPort_HeavyAssemblyRegistersEightTools` 改名 `TestToolListerPort_HeavyAssemblyRegistersNineTools`；期望名集加入 `"notify"`（排序位于 `list_dir` 与 `read_file` 之间）；保留**精确集合比较**与实体可解析循环（`notify` 加入可解析项）。DoD：落地改名 + 名集更新；登记逐字红态（`registry.Names()` 缺 `notify` → 集合不等）；转绿前提 = T023；**不得**放宽为子集比较
- [x] T021 [US4] `internal/tool/notify_test.go`（追加 **1 条**）——**测试准备**：N14 `TestNotifyTool_UnknownTypeFailsWithoutPolicyOrSend`（**R10**；`require.ErrorAs` 判 `*notifyTypeUnregisteredError`；断言零 env 查询 / `judgmentCount()==0` / 发送 0 次）。DoD：落地 1 条；登记红态（未知类型落到 nil 适配器 / 仍查 env → 逐字失败）；转绿前提 = T013 + T016（类型分派在 T010，专用错误在 T013；若已绿则用 `-overlay` 逆改「先解析 env 再查类型」取得红态）

### Implementation for User Story 4

- [x] T022 [US4] `internal/tool/notify_adapters.go`（改动，补全）：`maxNotifyReceiptBytes = 64*1024`；`WebhookNotifyAdapter` / `FeishuNotifyAdapter`（导出无参构造器 + 非导出 `new…(httpDoer)` 测试缝）；`webhookNotifyPayload` / `feishuNotifyPayload` / `feishuNotifyText` / `feishuNotifyReceipt{Code *int}` / `notifyPost` / `feishuReceiptVerdict`；固定 `Content-Type: application/json; charset=utf-8`；`newHTTPClient()`（不跟随重定向、不读代理）；webhook 2xx 成功不读体、所有分支关闭 body；feishu `LimitReader(max+1)` 显式超限 + `json.Unmarshal` 判尾随垃圾；传输错误 `Reason = 固定类别 + ： + scheme://host`、原始错误只进 `Cause`；ctx 先判、返回裸哨兵。DoD：**由红转绿**——T018 **13 条全 PASS**；`go test -race -count=1 ./internal/tool` 全绿；`gofumpt -l` 空输出
- [x] T023 [US4] `internal/app/notify.go`（新增：`newNotifyAdapters()` 唯一映射，恰好 `webhook` / `feishu` 两键）+ `internal/app/wire.go`（修改——§3.5 改造点 ①②③）：在记忆工具之后、等值门禁**之前**注册 `tool.NewNotifyTool(storage.NewNotifyChannelRepo(db), policy, newNotifyAdapters())`；`declaredToolNames` 八名 → 九名（排序，`notify` 位于 `list_dir` 与 `read_file` 之间）；三处注释八件 → 九件（注明第 10 件 `manage_schedule` 归 US-10）。DoD：**由红转绿**——T019 **2 条**、T020 **1 条**、T021 **1 条**全 PASS；`go test -race -count=1 ./internal/app ./internal/workspace ./internal/cli` 全绿；`grep -n '"notify"' internal/app/wire.go` 命中注册与名集两处；`gofumpt -l` 空输出
- [x] T024 [US4] **US4 验证**：对 R6 / R7 / A10 / R8 / A4 / R9 / R10 / R11 做逆改点红态复现（`-overlay`：webhook 改认 3xx 成功 → R6 红；feishu 去掉 `code==0` 判定 → R7 红；回执读满截断不判超限 → A10 红；客户端改 `http.DefaultClient` → A4 + R8 红；ctx 判定移到包装之后 → R9 红；类型查表前先解析 env → R10 红；映射加第三键 → R11 红）。DoD：`./internal/tool ./internal/app` 全绿；八条红→绿读数各一行；探针用毕即删

**Checkpoint**: US4 独立可验——投递真值、重定向/代理由、装配同形全部成立

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: 全量门禁、零变更面、quickstart 可执行性

- [x] T025 全量测试与门禁：`GOPROXY=https://goproxy.cn make check`（七目标）；`GOPROXY=https://goproxy.cn make cover`（`core` / `tool` / `storage` ≥ 70%，`app` ≥ 50%）；`GOPROXY=https://goproxy.cn go test -race -count=1 ./internal/core ./internal/storage ./internal/tool ./internal/app`。DoD：七目标逐条四态读数；42 条全部实际执行、无 `SKIP`；覆盖率逐包读数；`gofmt -l` / `gofumpt -l` 先查退出码再判输出（均为空）
- [x] T026 零变更面与依赖方向核对：`git diff -- go.mod go.sum`、`git diff -- internal/storage/schema.sql docs/contracts/schema.sql`、`git diff -- basalt.yaml basalt.yaml.example`、`git diff -- Makefile .github/ scripts/` 均空；`go list -f '{{.Imports}}' ./internal/core` 只出标准库；`go list -f '{{join .Imports "\n"}}' ./internal/tool | grep -E 'basalt/internal/(app|cli|workspace|storage)'` 零命中；`grep -rnE "//nolint|#nosec|//lint:ignore" --include='*.go' internal/core internal/storage internal/tool internal/app` 零命中；按 [quickstart.md](./quickstart.md) 逐条核对可执行性（人工项保持「未验证」，不冒充通过）。DoD：逐条空读数 / 零命中；`git status --short` 只含本 US 允许的 15 文件 + `specs/008-notify/**` + `harness/stages/us8-notify-r1.json`（stage 由步骤 6 生成）

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: 无依赖，可立即开始（T001 → T002）
- **Foundational (Phase 2)**: 依赖 Setup；T003 → T004 → T008；T005 → T006 → T007；T003 与 T005 可并行
- **US1 (Phase 3)**: 依赖 T007（仓储）与 T004（`EnvPlaceholderName`）；T009 → T010 → T011
- **US2 (Phase 4)**: 依赖 US1；T012 → T013 → T014（T012 / T013 触碰同一文件，串行）
- **US3 (Phase 5)**: 依赖 US2；T015 → T016 → T017（同一文件，串行）
- **US4 (Phase 6)**: T018 / T019 / T020 可并行（不同文件）；T022 依赖 T018；T023 依赖 T019 / T020 / T022；T021（`notify_test.go` 追加）排在 T015 之后、T024 之前；T024 依赖 T017 / T021 / T022 / T023
- **Polish (Phase 7)**: 依赖全部故事完成；T025 → T026

### Within Each User Story

- 测试准备任务先落测试并登记红态（逐字失败文案），实现任务由红转绿，验证任务做逆改点红态复现
- 同一文件的测试追加与实现改动**串行**（`notify_test.go` / `notify.go` / `notify_adapters.go`）
- 故事完成前不得进入下一故事；`notify.go` 的失败面按 US2 → US3 分两批补全

### Parallel Opportunities

- T001 与 T002 可并行（不同文件 / 只读取证）
- T003 与 T005 可并行（`core/envref_test.go` vs `core/notify.go`）
- T018 / T019 / T020 可并行（三个不同测试文件）
- 其余任务因同文件或前置依赖保持串行

---

## Parallel Example: User Story 4

```text
# 三个测试文件可同时落地（红态）：
Task: "T018 internal/tool/notify_adapters_test.go（A1~A13）"
Task: "T019 internal/app/notify_test.go（P1~P2）"
Task: "T020 internal/app/ports_test.go（八件 → 九件）"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. 完成 Phase 1（T001~T002）与 Phase 2（T003~T008）
2. 完成 Phase 3（T009~T011）
3. **STOP and VALIDATE**：`go test -race ./internal/tool ./internal/core ./internal/storage` + 两既有执行器用例
4. 此时正向闭环可演示（假发送端；真实 webhook 归人工项）

### Incremental Delivery

1. Foundational → 判定 / 类型 / 仓储 / 收敛就位
2. US1 → 正向闭环（MVP）
3. US2 → 失败可诊断 + 边界顺序
4. US3 → 凭证红线三面
5. US4 → 投递真值 + 装配九件
6. Polish → 全量门禁与零变更核对

---

## Notes

- [P] 任务 = 不同文件、无未完成依赖；同文件任务一律串行
- 关键回归 R1~R12 + A4 / A10 均已落到具名任务：R1 = T015 / T018（A5）；R2 = T015；R3 = T012；R4 = T015；R5 = T012；R6 = T018（A1/A2）+ T009（N17）；R7 = T018（A7~A11）；R8 = T018（A3）；R9 = T018（A6/A12）；R10 = T021；R11 = T019 + T020；R12 = T009（N19）；A4 = T018；A10 = T018
- 既有执行器两用例（`TestToolExecutor_PreWriteFailureRefusesExecution` / `TestToolExecutor_TerminalWriteFailureKeepsPendingRow`）在 T011 **必跑**、**不重造**
- 判据面变更仅 T020（八件 → 九件，人工 review ① 已批准）；T008 为等价重构、判据面零变更；其余既有测试零改动
- 不得发明颗粒度文档之外的交付物；不得改动 `spec.md` / `plan.md` / 颗粒度文档
- 每个 `go` 命令显式 `GOPROXY=https://goproxy.cn`；`-race` 与 `CGO_ENABLED=0` 分属不同命令；不自动 commit / push / tag
