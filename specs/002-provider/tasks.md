# Tasks: US-2 Provider 对接（能力一）

**Input**: Design documents from `/specs/002-provider/`

**Prerequisites**: [plan.md](./plan.md)（必需）、[spec.md](./spec.md)（用户故事与优先级）、[research.md](./research.md)、[data-model.md](./data-model.md)、[quickstart.md](./quickstart.md)

**Tests**: **必须包含**——本项目「harness 先行」是硬纪律：颗粒度文档 [docs/us/us2-provider.md](../../docs/us/us2-provider.md) §3.2 列出全部用例（默认层 54 + 集成层 2），实现与测试一起落地（implement 阶段不允许先实现后补测）。§4.3 写出代码的关键回归用例**原样落地**（断言逐条保真；函数名英文、文档原文以注释保留）。

**Organization**: 任务按用户故事分组，每个故事可独立实现与独立验证。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 所属用户故事（US1 / US2 / US3）——Spec-Kit 对**本 feature 内部**用户故事的编号（对应 spec.md 的三个 User Story），**不是**项目台账（`docs/us/REGISTRY.md`）里的 US-1 / US-2 / US-3；本 feature 的三个故事同属项目 **US-2（Provider 对接）**
- 每条任务都给出精确文件路径

## Path Conventions

单 Go module：`cmd/basalt/`、`internal/`、`scripts/`、仓库根。测试与被测包同目录（Go 惯例）；集成测试用 `//go:build integration` 标签隔离。

---

## Phase 1: Setup（共享前置）

**Purpose**: 新增坐标落地与写前核实——不涉任何用户故事

- [x] T001 依赖与 API 核实：`GOPROXY=https://goproxy.cn go get github.com/sashabaranov/go-openai@v1.43.0` 落地唯一新增坐标（`go.mod` / `go.sum`）；对照 [research.md](./research.md)「写前必须实测核实清单」——#1/#2/#3 复核落地形态（本地模块缓存读源码），#4（`CheckRedirect` 返回 `http.ErrUseLastResponse` 的返回语义）与 #5（`url.Parse` 对异常输入的宽容度）用**仓库外最小探针**逐例读回、用后即删；读数回填 research.md——核实不到即软门禁停下报告

---

## Phase 2: Foundational（阻塞全部用户故事）

**Purpose**: 契约层声明、审计仓储与测试基建——三个故事都建立在其上

**⚠️ CRITICAL**: 本阶段完成前，任何用户故事都不得开工

- [x] T002 `internal/core/provider.go`（新增）：`Message` / `ToolCall` / `Usage` / `ChatRequest` / `ChatResponse` / `ProviderClient` 接口 + `LLMCallStatus` 与四常量 + `ErrorKind` 与七常量 + `LLMCall` / `LLMCallResult` + `LLMCallStore` 接口——**纯声明、零可执行语句**（覆盖率判卷跳过）、只依赖标准库（[docs/us/us2-provider.md](../../docs/us/us2-provider.md) §3.1 逐字）
- [x] T003 [P] `internal/core/errors.go`（修改）：新增哨兵 `ErrProviderNotConfigured`（前序改造点 2，§3.5；既有哨兵不动、`errors.Is` 语义不变）
- [x] T004 [P] `internal/testutil/provider.go` + `provider_test.go`（新增）：`FakeStep` / `NewFakeProvider` / `Requests` / `CallCount` / `Started(n)` 信号基元 + `FakeOpenAIResponse` / `NewFakeOpenAIServer` / `URL` / `Requests`；自检 3 条用例（回放 / `Started(n)` 信号语义 / 假服务记录与回放）——基础设施自己先被判卷（编程指南 §8.3 第 2 项）
- [x] T005 `internal/storage/llm_calls_test.go`（新增）：6 条用例（`TestLLMCallRepo_BeginCallInsertsPendingRow` / `_FinishCallUpdatesTerminalFields` / `_FinishCallTruncatesErrorMessageAt4KBWithoutBreakingRunes`（§4.3 写出代码原样落地）/ `_FinishCallRejectsNonPendingRow` / `_CrashWindowLeavesPendingForRecovery` / `_ConcurrentBeginCall`）
- [x] T006 `internal/storage/llm_calls.go`（新增）：`LLMCallRepo` + `NewLLMCallRepo` + `BeginCall`（SQL **固定写** `pending`、`duration_ms=0`、`config_hash` NULL、`created_at` 毫秒 UTC）+ `FinishCall`（`UPDATE ... WHERE id = ? AND status = 'pending'`——终态只写一次；落库前 4 KB UTF-8 安全截断）+ 编译期断言 `var _ core.LLMCallStore = (*LLMCallRepo)(nil)`——审计 SQL **只**出现在本文件（全局不变量 3）

**Checkpoint**: 契约面与审计仓储就位，三个用户故事可按优先级推进

---

## Phase 3: User Story 1 - 一次调用，成败都留痕（Priority: P1）🎯 MVP

**Goal**: 显式映射路由 + 一次协议调用 + fail-closed 两阶段审计写：发出的调用 100% 先有 `pending` 行；审计不可用 → 零发出；取消路径错误链保留、行落 `failed` + `timeout`；未知字段与思考内容两跳不丢

**Independent Test**: 进程内假供应商 + 真审计库覆盖成功 / 供应商报错 / 取消 / 审计不可写四条路径，查库行与返回错误即可判卷；协议面用回环假 OpenAI 服务判出站请求体与入站解析，全程不访问真实网络

### Tests for User Story 1

- [x] T007 [P] [US1] `internal/provider/client_test.go`（新增 7 条）：`TestOpenAIClient_TranslatesRequestToOpenAIProtocol`（`stream` 不得为 true）/ `_RequestEnvelopeKeepsSingleMessagesKey` / `_HTTPErrorMapsToExternalError`（4xx / 5xx / 3xx 不跟随 + 畸形 JSON）/ `_ContextCancelStopsCall` / `_NoCredentialInErrorMessages` / `_RejectsBaseURLWithUnsupportedParts`（构造期防御）/ `_RoundTripsUnknownFieldsTwoHops`（两跳，§4.3 判据）
- [x] T008 [P] [US1] `internal/provider/service_test.go`（新增 18 条）：路由与三道拒绝 4 条 + 模型解析与成功/用量 3 条 + 留痕顺序与审计写入失败 4 条（含 `_AuditPendingBeforeCall` / `_RefusesCallWhenAuditPreWriteFails`——§4.3 写出代码原样落地）+ 取消 3 条（`_CancelDuringCallRecordsTimeoutKind` 经 `Started(1)` 同步、无 `time.Sleep`；`_PreCanceledContextStillAuditsFailedCall`；`_CanceledCallTerminalWriteFailurePreservesCancelChain`）+ 并发 1 条 + 透传与凭证三路 2 条 + `_UnclassifiedErrorRecordedAsUnknownKind` 1 条（FR-007 第三类「未知」兜底——裸错误 → `failed` + `unknown`，module-dev 步骤 5 裁决新增，见颗粒度文档「已决事项」#10）（函数名清单见 §3.2）；其中路由拒绝与供应商错误两类用例内**同时断言日志轨 WARN**（`Deps.Logger` 缓冲收到记录——FR-010 双轨；不改用例名与计数）
- [x] T009 [P] [US1] `internal/app/providers_test.go`（新增 3 条）：`TestNewProviderClients_ExplicitMapMatchesConfig`（键集逐字 = 配置名集）/ `_InvalidBaseURLRejected`（构造期拒绝并点名 `providers[i]`）/ `TestNewProviderService_ConfigToAuditRowChain`（配置 → 映射 → 调用 → `llm_calls` 行，假客户端 + 真库）

### Implementation for User Story 1

- [x] T010 [US1] `internal/provider/client.go`（新增）：`Options` / `OpenAIClient` / `NewOpenAIClient`（含 `parseBaseURL` 构造期防御校验）/ `Chat`（自建薄传输：一次 POST、不重试、不 fallback、`CheckRedirect` 返回 `http.ErrUseLastResponse`、`http.Client` **不设自有超时**）/ `buildRequestBody`（map 覆盖、`messages` 单键）/ `encodeMessages`（已知字段 + `ProviderExtra` 平铺）/ `parseChatResponse`（未知键收进透传位）/ `sanitizeMessage`（抹 key 回显 + 4 KB UTF-8 安全截断）/ `CallError{Provider, Status, Kind, Message, err}` + `Error` / `Unwrap`；四条返回路径全部经 `CallError` 出口；编译期断言 `var _ core.ProviderClient = (*OpenAIClient)(nil)`
- [x] T011 [US1] `internal/provider/service.go`（新增）：`ProviderService` / `NewProviderService`（装配时一次构建、之后只读）+ `Chat`（查表路由 → 三道拒绝（**在预写之前**）→ `BeginCall`（失败即拒发）→ 调用 → `FinishCall`）+ `resolveModel` + `finish` + `classifyError` + `auditContext`（`context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)`，缺省 5 秒，**单点定义、两处共用**）；路由只查表（`s.clients[req.Provider]`）；编译期断言 `var _ core.ProviderClient = (*ProviderService)(nil)`；失败路径审计与 WARN / ERROR 日志**双轨**（固定键：provider / model / run_id / error / error_kind）
- [x] T012 [P] [US1] `internal/provider/doc.go`（修改）：占位句改实装说明——约束原文保留（无自动工具循环 / 工具调度归 `ToolExecutor` / 显式映射）（前序改造点 4，§3.5）
- [x] T013 [US1] `internal/app/providers.go`（新增）：`newProviderClients(cfg *cli.Config)`（**逐条**由配置构造 `name → 客户端` 与 `name → 缺省模型` 两张表——遍历的是配置**数组**，非映射）/ `newProviderService(cfg, store, logger)`
- [x] T014 [US1] 端到端实测：假客户端 + 真库走通「配置 → 映射 → 调用 → `llm_calls` 行」；`go test -race -count=1 ./internal/provider/ ./internal/storage/ ./internal/app/` 全绿；关键回归对号读数记录（留痕顺序 / 零发出 / 两跳 / 取消）

**Checkpoint**: US1 独立可验收——审计留痕零例外、失败可解释、协议两跳保真（MVP 到此可演示）

---

## Phase 4: User Story 2 - 一眼看清配了哪些供应商（Priority: P2）

**Goal**: `basalt provider list` 轻路径（只读配置、不开库、不取实例锁、不做恢复、不构造客户端）四形态：正常列出（脱敏）/ 缺文件判红点名路径 / 空段判红点名段名 / 无效配置原错误上抛

**Independent Test**: 临时目录写各形态配置（正常两家 / 缺文件 / 空段 / 无效 / 直接注入含 userinfo / query / fragment 的条目），跑命令比对输出、退出码与三路（标准输出 / 标准错误 / 日志缓冲）凭证断言

### Tests for User Story 2

- [x] T015 [P] [US2] `internal/cli/provider_test.go`（新增 6 条）：`TestProviderList_ListsConfiguredProviders` / `_RedactsBaseURLUserinfoAndQuery`（展示层防御：直接注入三成分条目 → 渲染为 `scheme://host/path`）/ `_NoCredentialLeakInOutput`（三路）/ `_MissingConfigFailsWithPath` / `_EmptyProvidersSectionFails` / `_InvalidConfigFailsAndNamesKeyPath`

### Implementation for User Story 2

- [x] T016 [US2] `internal/cli/provider.go`（新增）：`newProviderCommand(deps)` + `runProviderList(out, deps)`（轻路径：只读配置；缺文件 / 空段点名路径 / 段名；正常逐条列 name / 收窄地址 / default_model / 凭证状态文案 + 汇总行；exit 0 / 1 口径沿用 US-1）+ `redactedBaseURL(raw)`（**自实现**——`cli` **不** import `provider`，依赖方向红线）；同时扩展 `internal/cli/ports_test.go` 的禁止清单加入 `internal/provider`（既有判据只拦 `storage` / `workspace`——analysis 阶段发现 plan 原表述失实，此为修复落点）
- [x] T017 [P] [US2] `internal/cli/root.go`（修改）：`root.AddCommand(...)` 注册 `newProviderCommand(deps)`（前序改造点 3，§3.5；命令面 2 → 3 条）
- [x] T018 [US2] 端到端实测：临时工作区四形态逐例跑命令 + 退出码 + 三路凭证断言读数记录（含正常形态的汇总行与收窄地址比对）

**Checkpoint**: US2 独立可验收——`provider list` 四形态与脱敏纵深防御成立

---

## Phase 5: User Story 3 - 配置防线与真实供应商的一次调用（Priority: P3）

**Goal**: 装载期三条新规则（重名拒绝 / 地址必填 / 地址成分拒绝，文案不回显原 URL 与其中的值）+ 真实调用验收正例与不可达反例（双门控；跳过不算通过）

**Independent Test**: 三条新规则用默认层用例逐条判卷（**不需要凭证**）；真实调用正例与不可达反例走集成层（显式执行开关 + 凭证环境变量双门控，缺一即跳过并写明理由）

### Tests for User Story 3

- [x] T019 [P] [US3] `internal/cli/config_test.go`（追加 3 条）：`TestConfig_RejectsDuplicateProviderName`（点名两条目下标）/ `_RejectsMissingProviderBaseURL`（独立用例，不复用其他必填判据）/ `_RejectsProviderBaseURLWithUserinfoQueryFragment`（子例覆盖 userinfo / query / fragment / **缺 scheme**——缺 scheme 对号 spec.md US3 验收场景 2；非 `http(s)` 方案同判；文案含键路径与成分名、**不含原 URL 与其中的值**）
- [x] T020 [P] [US3] `internal/app/realcall_integration_test.go`（新增，`//go:build integration` 2 条）：`TestProviderService_RealCallWritesAudit`（双门控 `BASALT_REAL_CALL=1` + 凭证 env，缺一即 `t.Skip` 写明理由；入口 `context.WithTimeout` **显式设截止**；token 判据按供应商**是否返回 usage 分支**）/ `TestProviderService_UnreachableEndpointRecordsFailedAudit`（**不需要凭证**，故不因缺凭证跳过）；独立临时库（`BASALT_DEMO_DIR`，不复用开发库）

### Implementation for User Story 3

- [x] T021 [US3] `internal/cli/config.go`（修改）：`validateConfig` 新增三条规则——① `providers[].name` 重名拒绝（点名两条目下标）；② `providers[].base_url` 必填；③ `base_url` 拒绝 userinfo / query / fragment（只支持 scheme + host + path，缺 scheme **不代补**）——文案只点名键路径与成分名、不回显原 URL 与其中的值（`url.Parse` 原始错误**不得**直传）；**不**改动既有六类校验的语义与文案风格（前序改造点 1，§3.5）
- [ ] T022 [US3] 真实调用验收（**人执行**）：按 [quickstart.md](./quickstart.md) 场景 4 跑正例 + 不可达反例，读数记录；凭证经环境变量注入（不落盘 / 不落日志 / 不进对话）；**`t.Skip` 不算通过**——收口报告必须区分 `--- PASS` 与 `--- SKIP`

**Checkpoint**: 三个用户故事全部独立可验收

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: 收口证据与跨故事一致性

- [x] T023 [P] 覆盖率判卷调平：`make cover` 全包达标（`internal/provider` 自本 feature 起纳入 ≥ 50% 判卷；`internal/core` 若保持零可执行语句则跳过——**不得**为凑覆盖率往 `core` 加空转测试或语句）；读数记录
- [x] T024 [P] 静态与安全门禁复核：`gofumpt -w` 只对本轮改动文件；`go vet` / staticcheck（显式判定集 `-checks=inherit,ST1000,ST1003,ST1016,ST1020,ST1021,ST1022`）/ `gosec -quiet ./...` / `govulncheck ./...` 零命中；抑制注释（`//nolint` / `#nosec`）零使用
- [x] T025 [P] 全局不变量逐条 grep 自查（`module-dev` 配置区 9 条）+ 依赖方向无环复核（`go list -deps`：`core` 不 import 其他 internal 包与第三方；`cli` **不** import `provider`——`ports_test.go` 既有判据回归）
- [x] T026 [P] [quickstart.md](./quickstart.md) 四场景逐条执行（场景 4 未由人执行时如实登记「未执行」，**不得**记通过）
- [x] T027 文档一致性收口：交付物清单 ↔ 实现逐项核对（[docs/us/us2-provider.md](../../docs/us/us2-provider.md) §3.1 / §3.2 / §3.3 / §3.4 / §3.5）+ harness 映射表 §4.2 逐条对号 + §4.2 治理关键词 grep 判据执行 + `/speckit-analyze` 收口复核；新发现陷阱按陷阱表惯例拟条（带「怎么发现」与「怎么复核」），经用户确认后入库
- [x] T028 `make check` 终局全绿 + 七项 DoD 证据留存（门禁输出 / 映射对号 / 存在性核对 / **前序模块 US-1 全部测试回归** / 不变量自查 / 剩余人工项清单 / 变更总结三段式）；`Makefile` 与 `.github/workflows/ci.yml` **无改动**核对（七目标不变、新增用例随既有 `test` / `test-integration` 目标执行）

---

## Phase 7: 实现评审批次（外部评审 2×P1 + 2×P2 + 2 优化；用户逐条探针复现，修完再终验）

**Purpose**: 修复评审发现（均已由用户以临时探针复现），补回归判据；对应颗粒度文档「已决事项」#11~#17

- [x] T029 [US1] `internal/provider/client.go` 修复四项：① 读取阶段补取消 / 截止分类（`timeout` + 错误链保留，罕见形态下 `errors.Join` 补 ctx 错误）；② 响应体读取上限（`io.LimitReader(maxResponseBodyBytes+1)`，成功体与错误体同判，超限安全失败）；③ 首条 choice 必须有 `message` 对象（缺失 / `null` / 非对象判死）；④ 未知字段解码改 `UseNumber()`（大整数两跳保真）
- [x] T030 [US2] 空 fragment 分隔符两层拒绝：`parseBaseURL` 与 `validateProviderBaseURL` 同判**字面 `#`**（`url.URL` 无 `ForceFragment`，空分隔符只能从原始串上找）；正例守卫保留路径里的合法 `%23`
- [x] T031 [US1] [US2] 回归用例落地（默认层 46 → **54**）：`client_test.go` 五条（读取阶段取消 / 截止、大整数两跳（**判据取原始出站 JSON**）、读取上限、缺 `message` 判死、编码 `%23` 正例）+ `service_test.go` 两条（读取阶段取消的审计终态、坏响应不得记 `success`）+ `config_test.go` 一条（编码 `%23` 正例）；表格子例扩展：两层各加空 fragment / 空 query 两例；`captureRawRequests` 扩为按请求回放响应体
- [x] T032 [US3] `internal/app/realcall_integration_test.go` token 断言撤除过强推断（只留三列与响应 **Equal**；不得由 `total_tokens > 0` 反推 usage 是否存在）
- [x] T033 文档与计数同步：颗粒度文档 §3.2 / §4.1 / §4.2 / §6 + 已决事项 #11~#17；`spec.md` / `plan.md` / `quickstart.md` / `tasks.md` 计数 46 → 54；报告口径「需求映射覆盖」与行为验证分开陈述
- [x] T034 `make check` 终局复跑（本轮改动后）+ 七项 DoD 证据以**本轮当场输出**为准；`Makefile` / `.github/workflows/ci.yml` 复认无改动

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup（Phase 1）**: 无依赖，立即开始
- **Foundational（Phase 2）**: 依赖 Setup —— **阻塞全部用户故事**
- **User Stories（Phase 3+）**: 均依赖 Foundational；按 P1 → P2 → P3 顺序推进（本项目单人 + 依赖链，不并行开工）
- **Polish（Phase 6）**: 依赖三个故事完成
- **实现评审批次（Phase 7）**: 依赖 Phase 6 收口后由外部评审触发；T029~T031 同批落地（实现与回归判据一起），T032~T033 独立，T034 收尾复跑门禁

### User Story Dependencies

- **US1（P1）**: Foundational 后即可开工——消费 `core` 声明、`LLMCallRepo`、`FakeProvider`（T002–T006）；`app/providers.go` 端到端链（T013 / T014）是本故事自有交付物
- **US2（P2）**: Foundational 后即可开工——只消费 US-1 已交付的配置装载面（`cli.Config` / `LoadConfig` / `Deps`）与命令骨架，**不**依赖 US1 的调用面；`redactedBaseURL` 自实现（不 import `provider`）
- **US3（P3）**: 三条装载期规则（T019 / T021）不依赖其他故事；真实调用正例（T020 / T022）消费 US1 的调用链与审计面，反例只消费装配面

### Within Each User Story

- 测试与实现**同批落地**（harness 先行：先写用例、确认其因缺实现而失败，再补实现）
- 类型 / 声明 → 客户端与仓储 → 服务 → 装配 / 命令接线 → 端到端实测
- 每个故事收尾必须「该故事的测试全绿」才进下一个故事
- §4.3 写出代码的关键回归用例（`_AuditPendingBeforeCall` / `_RefusesCallWhenAuditPreWriteFails` / `_FinishCallTruncatesErrorMessageAt4KBWithoutBreakingRunes` / `_CancelDuringCallRecordsTimeoutKind`）**原样落地**

### Parallel Opportunities

- Setup：仅 T001（其余任务都等它）
- Foundational：T003 与 T002 可并行（不同文件）；T004（testutil）与 T005 / T006（storage）可并行
- US1：T007 / T008 / T009 三个测试文件可并行；T012（doc.go）与 T010 / T011 可并行
- US2：T017（root.go 注册）与 T016（provider.go）可并行
- US3：T019（config_test.go）与 T020（集成测试文件）可并行
- Polish：T023–T026 可并行

---

## Parallel Example: User Story 1

```text
# 三个测试文件可同时写（互不同文件）：
T007 internal/provider/client_test.go（7 条）
T008 internal/provider/service_test.go（18 条）
T009 internal/app/providers_test.go（3 条）

# 文档修改与实现互相独立：
T012 internal/provider/doc.go（占位句改实装说明）
T010 internal/provider/client.go
T011 internal/provider/service.go
```

---

## Implementation Strategy

### MVP First（User Story 1）

1. Phase 1 Setup → Phase 2 Foundational
2. Phase 3（US1）：显式映射 + 一次调用 + 两阶段审计写
3. **停下验证**：`go test -race` 本模块全绿 + 关键回归对号（留痕顺序 / 零发出 / 两跳）
4. 本 feature 的 MVP 到此即可演示（一次调用的成败都留痕）

### Incremental Delivery

1. Setup + Foundational → 契约面 / 审计仓储 / 测试基建就绪
2. US1 → 调用与审计（MVP）
3. US2 → `provider list`（可演示配置一览）
4. US3 → 装载期防线 + 真实调用验收（由人执行）
5. Polish → 覆盖率 / 不变量 / quickstart / 收口证据

---

## Notes

- **[P]** = 不同文件、无未完成依赖
- 测试与实现同批交付；**不得**删断言 / 无理由 `t.Skip` / 放宽阈值让门禁变绿
- **不自动 commit / push / tag**——每个 US 的提交由用户执行；提交信息格式见 [docs/EngineeringStandards.md](../../docs/EngineeringStandards.md) §5
- 软门禁：交付物清单外的对外概念、已定字面量修改、第三方 API 核实不到 → **停下报告**
- 前序改造点四条（§3.5）分别落在 T003（core 哨兵）/ T012（provider doc.go）/ T017（root.go 注册）/ T021（cli 三规则），逐条为软门禁
- 核心逻辑必须与 §2.3 流程 A–D 的伪代码**逐块对照落地**（同构性核对，签名逐字一致）；内部函数名（`buildRequestBody` / `encodeMessages` / `parseChatResponse` / `sanitizeMessage` / `parseBaseURL` / `resolveModel` / `finish` / `classifyError` / `auditContext`）同为交付面，逐项可核
