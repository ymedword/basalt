# Implementation Plan: US-2 Provider 对接（能力一）

**Branch**: `002-provider` | **Date**: 2026-10-16 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/002-provider/spec.md`

**Note**: 本 plan 由 `/speckit-plan`（经 `/module-dev 2` 步骤 4）生成；模块级细节以颗粒度文档 [docs/us/us2-provider.md](../../docs/us/us2-provider.md) §2 / §3 为准，冲突时以 [docs/TechnicalSolution.md](../../docs/TechnicalSolution.md) 为准。

## Summary

把 Basalt 与 LLM 供应商之间的**一次真实调用**打通：配置装载（US-1 已交付）→ 装配处**显式映射表**一次构建 → 一次 Chat 调用（协议翻译 + 用量解析）→ 调用**成败都写** `llm_calls` 审计（fail-closed 两阶段写）；并交付 `basalt provider list`（列 Provider 及配置状态，轻路径只读配置）。

技术路线的三个要点：**① SDK 角色收窄**——新增唯一坐标 `go-openai v1.43.0` 只提供协议类型与已知字段编解码，HTTP 往返由 `provider` 自建薄传输（`net/http` + `encoding/json`，一次 POST、不重试、不跟随重定向），依据是源码核实：该库的消息编解码只覆盖已知键，未知字段**两个方向都会被丢**（`ExtraFields` 全仓零命中），协议透传承诺只能自建；**② 留痕先于发出**——`BeginCall` 插入失败即**拒绝发出**，终态回填失败只记 ERROR 并把行**留在进行中形态**（不伪造终态），两处写入共用同一把解耦上下文（`context.WithoutCancel` + 独立 5 秒）；**③ 地址面装载期拒绝**——`base_url` 只支持 scheme + host + path，含 userinfo / query / fragment 的取值在**装载期**拒绝（不静默裁剪），构造期与展示层按同一口径各再防一次。

## Technical Context

**Language/Version**: Go 1.27.2 构建版本（`go.mod` 的 `go` 行写最低版本要求 `go 1.27`；准确构建版本由 `toolchain go1.27.2` 与 CI 的 `actions/setup-go` 共同固定——**不得**表述为「锁定 1.27.2」）

**Primary Dependencies**: 既有四个坐标（`spf13/cobra` v1.10.2 / `goccy/go-yaml` v1.19.2 / `modernc.org/sqlite` v1.60.1 / `stretchr/testify` v1.12.1）不动；**新增唯一坐标** `github.com/sashabaranov/go-openai` v1.43.0——角色收窄为**协议类型与已知字段编解码**（请求信封基座 / 消息 / `ToolCall` / `Usage` / 错误体），HTTP 往返自建；**无** `import _` 副作用、无 `init()`、不启动后台 goroutine。标准库：`net/http`（自建传输，`CheckRedirect` 拒绝跟随）、`encoding/json`、`context`、`log/slog`、`time`。

**Storage**: SQLite 单文件库（`.basalt/basalt.db`，US-1 已交付打开 / 迁移 / PRAGMA 面）；`llm_calls` 表 US-1 已建齐，**本 US 首次写入**（`BeginCall` 插 `pending` / `FinishCall` 写终态两条手写 SQL，唯一入口 `internal/storage/llm_calls.go`）。列 / 状态字面量的唯一事实源是 [docs/contracts/schema.sql](../../docs/contracts/schema.sql)；本 US 写 `config_hash` 列恒为 NULL（快照机制归 US-4 / US-11）。

**Testing**: `go test -race -cover ./...`（默认层 **54** 条：service 20 + client 12 + storage 6 + app 3 + cli 10 + testutil 3，**无一例访问真实网络**；含实现评审回合新增 8 条）+ 独立 `go test -tags=integration ./...`（本 US 新增 2 条：真实调用正例 / 不可达端点反例，**显式执行开关 + 凭证 env 双门控**，缺一即 `t.Skip` 写明理由，CI 上跳过属预期）；`internal/testutil/provider.go` 交付假 Provider（含 `Started(n)` 同步基元——取消用例**不用** `time.Sleep`）与回环假 OpenAI 服务（记录收到的请求体，双向断言）。

**Target Platform**: 开发机 Windows 11 + Git Bash；发布产物四平台交叉编译（`CGO_ENABLED=0`）；`-race` 与发布构建分属不同命令 / job。

**Project Type**: CLI + 服务单二进制（本 US 交付内部调用面 + 一条命令组 `provider list`；HTTP 面归 US-9）

**Performance Goals**: 无显式吞吐 / 延迟指标（底座零件类 feature）；判据是**行为顺序**与**失败可解释**——留痕先于发出、审计不可用零发出、取消路径错误链保留、失败 100% 带类别

**Constraints**: 三条包纪律（依赖方向 / 接口定义在消费方 / 第三方实现放消费方包内）+ `core` 只依赖标准库且**只放声明（零可执行语句）**；审计写入路径唯一（SQL 只在 `internal/storage`）；凭证零明文（`${ENV_VAR}` 占位，客户端边界脱敏：抹 key 回显 + 4 KB UTF-8 安全截断 + 不带请求 URL 与响应正文全文）；`base_url` 两层拒绝（装载期 + 构造期）且文案不回显原 URL 与其中的值；覆盖率自本 feature 起 `internal/provider` 纳入 ≥ 50% 判卷（`internal/core` 按「只放声明」设计保持零语句）

**Scale/Scope**: 约 8~10 个实现文件 + 对应测试；新增坐标 1 个；命令面 2 → 3 条；测试 54 条默认层 + 2 条集成层；前序改造点 4 处

**环境实测读数（2026-10-16，本轮当场命令）**: `go version` → `go1.27.2 windows/amd64`（与 `toolchain go1.27.2` 一致）；`GOPROXY=https://goproxy.cn go list -m -versions github.com/sashabaranov/go-openai` → 末位 `v1.43.0`、退出码 0；`go test -cover ./internal/core/ ./internal/provider/` → 两包均 `[no test files]`（当前无可执行语句，覆盖率判卷跳过——本 US 给 `provider` 加实现后它纳入判卷）；工具链与四个静态检查工具就绪（读数与固定版本的一致性见 US-1 plan；本轮未复测的项不在此声称）。本地模块缓存已含 `github.com/sashabaranov/go-openai@v1.43.0`（H3 门禁可就地核实）。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

对照 [.specify/memory/constitution.md](../../.specify/memory/constitution.md) 十原则 + 三条架构约束 + 开发工作流（GATE 判定：通过 / 不适用 / 需说明）：

| # | 宪法条目 | 本 feature 关系 | GATE |
| --- | --- | --- | --- |
| I | 自实现 ReAct 循环 | **部分适用**：新增坐标是**协议 SDK**（非 Agent 框架）；客户端只做一次协议调用，响应中的工具调用**只透传**（执行权归 US-4）；循环体归 US-4，本 US 不实现 | 通过 |
| II | LLM 客户端只做协议调用 | **适用（首个落地面）**：调用面只有 Chat 请求 / 响应结构；**不启用**任何自动工具循环、不让库接管会话历史或工具调度；本 US **无**消息仓储写入（会话历史归 US-4）——「不得出现第二套追加路径」在写入面自然成立（追加点为零） | 通过 |
| III | Provider 必须显式映射 | **适用（本 US 的核心交付）**：`app.newProviderClients` **逐条**由配置构造 `name → 客户端`；路由 `s.clients[req.Provider]` **只查表**；装配处遍历的是**配置数组**（有序显式）而非映射；装载期拒绝重名（否则映射静默缺条目） | 通过 |
| IV | 一个目录 = 一个 Agent；技能声明式绑定 | 不适用——Agent 加载与技能绑定归 US-3；本 US 不触碰 `workspace` 面 | 通过 |
| V | 审计 day-one 且 fail-closed | **适用（`llm_calls` 首次写入）**：调用前先插 `pending`、**插入失败即拒绝发出**（ERROR）；返回后写终态；终态写失败 → ERROR + 行留 `pending`（残窗如实登记，归一化归 US-4）；**不得**以「日志够了」跳过落库 | 通过 |
| VI | 授权与执行分离（两接口） | 不适用——沙箱 / 工具归 US-4 / US-5；本 US 无 `Policy` / `CommandRunner` 消费面 | 通过 |
| VII | 同步执行 + context 只承载取消 / 截止 / 追踪 | **适用**：全程同步一次调用，无异步编排；映射表 / 模型表 / 仓储 / 日志器 / 关联字段一律**显式类型化参数**；`context.WithValue` **零使用**（收尾 grep）。审计两处写入经 `auditContext`（`WithoutCancel` + `WithTimeout`）——两者作用在**取消与截止面**（剥除取消信号、加独立截止），**不承载业务态**，属本条允许面 | 通过 |
| VIII | 单二进制纪律 + 依赖最小化 | **适用**：新增坐标仅一个且在技术栈表内（直接落地，不重复询问）；无 `import _` 副作用；`CGO_ENABLED=0 go build` 不受影响；`-race` 与发布构建分属不同命令 / job | 通过 |
| IX | 不用 ORM，手写 SQL | **适用**：`go.mod` 零 ORM 坐标；本 US 两条手写 SQL（`INSERT` 预写 / `UPDATE` 终态）只出现在 `internal/storage`；无新表、无新迁移（表 US-1 已建齐，已发布迁移不动） | 通过 |
| X | 五大能力优先，范围卡紧 | 通过——本 US 即五大能力之一的 Provider 面；Out of Scope 十三项归属明确（spec.md Out of Scope / 颗粒度文档 §5） | 通过 |

**架构约束**：依赖方向 `channel` / `web` / `cli` → `core` ← `provider` / `memory` / `tool` / `storage` / `workspace`；`app` 唯一装配层；`core` 只依赖标准库——本 US 新增第二个依赖倒置样例（`core.ProviderClient` 由 `provider` 实现、`core.LLMCallStore` 由 `storage` 实现，两者均由 `app` 注入；`go-openai` 只在 `provider`、SQL 只在 `storage`）；接口定义在消费方；无环由 CI 用 `go list` 复核；凭证一律 `${ENV_VAR}` 占位、错误与输出面零回显。

**开发工作流**：门禁一条命令 `make check`（七目标，**Makefile 无改动**——新增测试随既有 `go test` 目标执行，覆盖率判卷自动纳入 `provider` / `core`）；覆盖率可执行判据沿用 US-1 的 `scripts/check-coverage.sh`；三条实测纪律（格式判卷先退出码后输出 / `-race` 与 `CGO_ENABLED=0` 分离 / build tag 单列）适用于本 US 新增的集成层；提交纪律（AI 不自动 commit / push）。

**plan review 必查（宪法 §开发工作流）逐条**：

| 必查项 | 本 plan 的答复 |
| --- | --- |
| 依赖方向无环 | `core` 新增 `provider.go` 为**纯声明**（零语句）、`errors.go` 加一个哨兵——仍只依赖标准库；`provider` 依赖 `core` + `go-openai`；`storage` 依赖 `core`；`cli` 依赖 `core`（**不得**为复用校验而 import `provider`——既有 `internal/cli/ports_test.go` 的禁止清单**只拦 `storage` / `workspace`**，本 US **扩展**该清单加入 `internal/provider`，把这条从人工复核升为 CI 判卷）；`app` 装配全部；`cmd/basalt` 只调 `app.Run`。无反向依赖（收尾以 `go list -deps` 复核） |
| 审计写入路径唯一 | `llm_calls` 的 SQL **只**出现在 `internal/storage/llm_calls.go`；`provider` 经 `core.LLMCallStore` 接口消费、不知 SQL |
| 无框架坐标 | `go.mod` 新增唯一坐标 `go-openai`（协议 SDK）；收尾 `grep -nE "eino\|langchaingo\|adk-go\|trpc-agent-go" go.mod` 零命中 |
| 无 `context.WithValue` 业务态 | 本 US `context.WithValue` 零使用（收尾 grep 零命中）；`auditContext` 只用 `WithoutCancel` / `WithTimeout`（取消与截止面） |
| 单二进制构建通过 | 新增坐标纯 Go（无 CGO）；`build` 目标 `CGO_ENABLED=0 go build -trimpath ...`；`-race` 只在 `test` 目标 |

**Phase 1 后复评（post-design）**：设计产物（[research.md](./research.md) / [data-model.md](./data-model.md) / [quickstart.md](./quickstart.md)）完成后重跑本表——结论同前，无新增违规、无 Complexity Tracking 条目（见文末）。

## Project Structure

### Documentation (this feature)

```text
specs/002-provider/
├── spec.md              # /speckit-specify 输出（需求规格）
├── plan.md              # 本文件（/speckit-plan 输出）
├── research.md          # Phase 0 输出（决策 + 写前必须实测核实清单）
├── data-model.md        # Phase 1 输出（实体、状态机与校验规则）
├── quickstart.md        # Phase 1 输出（可复制执行的验证步骤）
├── checklists/
│   └── requirements.md  # 需求质量清单（/speckit-specify 输出）
└── tasks.md             # /speckit-tasks 输出（下一步生成）
```

**`contracts/` 本 feature 不新建**（与 US-1 的差异，理由如下）：US-1 的 `contracts/` 记录的是**当时不存在于任何契约文件里的**新导出签名与命令面语义（`storage-api.md` / `cli-contract.md`）；本 US 的固定字面量**已有唯一事实源**——表 / 列 / 状态字面量在 [docs/contracts/schema.sql](../../docs/contracts/schema.sql)、两阶段写与失败分类在 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md) §4.2 / §7.2 / §7.3 / §7.4、导出签名逐字来源在颗粒度文档 §3.1。再抄一份到 `specs/` 就是**第二份字面量**——抄一份必漂移一处（颗粒度文档 §3.4 的同一理由）。plan 的契约节只**引用**，不复制。

### Source Code (repository root)

```text
internal/
├── core/
│   ├── provider.go          # 新增：ProviderClient 接口 + Message/ToolCall/Usage +
│   │                        #       ChatRequest/ChatResponse + LLMCallStatus/ErrorKind 常量 +
│   │                        #       LLMCall/LLMCallResult + LLMCallStore 接口（**纯声明，零语句**）
│   ├── errors.go            # 修改：新增哨兵 ErrProviderNotConfigured（既有哨兵不动）
│   └── doc.go               # 不动
├── provider/
│   ├── doc.go               # 修改：占位句改实装说明（约束原文保留：无自动工具循环 / 工具调度归 ToolExecutor）
│   ├── client.go            # 新增：Options + OpenAIClient + NewOpenAIClient + Chat + CallError{Error,Unwrap}
│   │                        #       + buildRequestBody / encodeMessages / parseChatResponse /
│   │                        #         sanitizeMessage / parseBaseURL（内部函数，同为交付面）
│   ├── client_test.go       # 新增：7 条（协议翻译 / 信封单键 / HTTP 错误映射 / 取消 / 无凭证 /
│   │                        #         构造期地址拒绝 / 未知字段两跳）
│   ├── service.go           # 新增：ProviderService + NewProviderService + Chat +
│   │                        #       resolveModel / finish / classifyError + auditContext / auditWriteTimeout
│   └── service_test.go      # 新增：18 条（路由 / 三道拒绝 / 留痕顺序 / 成功与用量 / 失败与类别 /
│   │                        #         凭证三路 / 预写失败 / 终态写失败 ×2 / 取消 ×2 / 并发 / 透传 / 「未知」兜底）
├── storage/
│   ├── llm_calls.go         # 新增：LLMCallRepo + NewLLMCallRepo + BeginCall + FinishCall
│   │                        #       （`llm_calls` 两阶段写的**唯一 SQL 入口**）
│   └── llm_calls_test.go    # 新增：6 条（预写列值 / 终态回填 / 4 KB 截断 / 终态只写一次 /
│   │                        #         崩溃窗口形态 / 并发预写）
├── app/
│   ├── providers.go         # 新增：newProviderClients（逐条显式映射）+ newProviderService
│   ├── providers_test.go    # 新增：3 条（映射键集 = 配置名集 / 非法地址点名 / 端到端链路）
│   └── realcall_integration_test.go  # 新增：//go:build integration 2 条（双门控正例 / 不可达反例）
├── cli/
│   ├── provider.go          # 新增：newProviderCommand + runProviderList + redactedBaseURL
│   ├── provider_test.go     # 新增：6 条（列配置 / 展示层脱敏 / 无凭证泄露 / 缺配置 / 空段 / 无效配置）
│   ├── config.go            # 修改：validateConfig 增三条规则（重名拒绝 / base_url 必填 / 成分拒绝）
│   ├── config_test.go       # 修改：追加 3 条（三条规则各一例，第 8 类含不回显断言）
│   ├── ports_test.go        # 修改：禁止清单加入 internal/provider（依赖方向判据——见 Structure Decision）
│   └── root.go              # 修改：注册 provider 命令组
├── testutil/
│   ├── provider.go          # 新增：FakeProvider（步骤回放 / Requests / CallCount / Started(n)）
│   │                        #       + FakeOpenAIServer（回放状态与响应体 / 记录请求体）
│   └── provider_test.go     # 新增：3 条（回放 / Started 信号语义 / 假服务记录与回放）
└── （其余包不动：channel / memory / tool / web / workspace）

go.mod / go.sum              # 新增唯一坐标 github.com/sashabaranov/go-openai v1.43.0
Makefile                     # 无改动（七目标不变；provider / core 覆盖率判卷自动纳入）
.github/workflows/ci.yml     # 无改动（新增用例随既有 test job 执行）
```

**Structure Decision**: 单 Go module + `internal/` 分包（无多模块、无 `src/` 层）；本 US 新增实现文件 5 个（`core/provider.go`、`provider/client.go`、`provider/service.go`、`storage/llm_calls.go`、`app/providers.go`）+ 命令与测试基础设施 2 个（`cli/provider.go`、`testutil/provider.go`）+ 测试文件 8 个；修改 4 处（`cli/config.go`、`cli/root.go`、`core/errors.go`、`provider/doc.go`——均为颗粒度文档 §3.5 点名的前序改造点，逐条为软门禁）+ 既有测试扩展 1 处（`cli/ports_test.go` 的禁止清单加入 `internal/provider`——「`cli` 不 import `provider`」由 CI 判卷；该文件不在 §3.2 新增用例清单内，用例计数不变）。与台账「模块落位（摘要）」列一致：`go.mod`（新增 `go-openai v1.43.0`）、`internal/{core,provider,storage,app,cli,testutil}`。

**分层落位（三条包纪律的逐条落地）**：

| 纪律 | 本 US 的落法 |
| --- | --- |
| 依赖方向无环 | `core` 只放声明（零语句）；实现进 `provider` / `storage`；`app` 装配；`cli` 消费 `core`（**不** import `provider`——展示层脱敏自实现 `redactedBaseURL`，不为复用把依赖方向破掉；该禁令由扩展禁止清单后的 `ports_test.go` 判卷，见上表） |
| 接口定义在消费方 | `core.ProviderClient` 由 `provider.ProviderService`（路由面）与 `provider.OpenAIClient`（单供应商面）实现；`core.LLMCallStore` 由 `provider.ProviderService` 消费、`storage.LLMCallRepo` 实现；两端编译期断言（`var _ core.X = (*Y)(nil)`） |
| 第三方实现放消费方包内 | `go-openai` 只在 `internal/provider`；SQL 只在 `internal/storage`；`core` 零第三方依赖（收尾 `go list -deps` 复核） |

**标签口径（`[US1]` 不混淆）**：`tasks.md` 中的 `[US1]` / `[US2]` / `[US3]` 是 Spec-Kit 对**本 feature 内部**用户故事的编号（对应 [spec.md](./spec.md) 的三个 User Story），**不是**项目台账（[docs/us/REGISTRY.md](../../docs/us/REGISTRY.md)）里的 US-1 / US-2 / US-3——本 feature 的三个故事全部落在项目 **US-2（Provider 对接）** 一个模块内。标签按工具产出保留，不另造编号。

## Complexity Tracking

> 仅当 Constitution Check 存在需辩护的违规时填写。

无违规——本 feature 无 Constitution Check 不通过项，故无复杂度辩护条目。
