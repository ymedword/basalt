# Implementation Plan: 受控自我进化（记忆候选、证据链与人工确认）

**Branch**: `022-controlled-evolution` | **Date**: 2026-11-06 | **Spec**: [spec.md](spec.md)

**Input**: US-22 模块执行颗粒度文档 R6（含两处定稿勘误）经人工 review ① 通过（2026-11-05），所审内容 SHA-256 `289b5e70…`、2039 行；设计范围冻结。本批准**不代表实现或测试已通过**。

## Summary

在已交付的「显式保存即生效」与「US-21 技能 / 方法准入」之间补上**记忆与经验发现的受控入口**：
只为管理员显式启用的 Agent、只在 Web / CLI Run 终态真正落库之后，用一次同步、有界（端到端 ≤20s）
的模型调用把有长期价值的用户陈述提炼成**带逐字证据的候选**。候选固定 `candidate / auto_extract /
archive`，确认前不召回、不注入；管理员在既有「记忆」页以「保留双方 / 替代旧条」确认，确认事务执行
全部门禁（候选 CAS、候选到期、来源闭包、来源 Key、来源调用方、替代目标一致性）。来源失效后，
召回面沿来源关系递归复核派生闭包（深度 8，fail-closed）。方法类输出写成只读草案文件，只能经
`learning handoff` 调既有技能提议入口。审计两级 `pending` 预写、提交结果未知只查证不重放、迁移
`0011` 只追加、学习查询面只读。真实模型效果 / 评测集 / 隔离对照归 D-2（拟），本 US 一律 `未测量`。

## Technical Context

**Language/Version**: Go 构建版本由 go.mod toolchain 指令与 CI setup-go 固定（当前 go1.27.3；go 行 go 1.27 仅为最低要求，本机基础安装 1.27.2 不是构建版本，2026-10-27 实测）；单 module；发布 `CGO_ENABLED=0`。

**Primary Dependencies**: 技术栈固定句（逐字照抄，本 US 零新增坐标）：`Go 构建版本由 go.mod toolchain 指令与 CI setup-go 固定（当前 go1.27.3；go 行 go 1.27 仅为最低要求，本机基础安装 1.27.2 不是构建版本，2026-10-27 实测）+ 单 Go module（cmd/basalt + internal/ 分包）+ spf13/cobra v1.10.2 + goccy/go-yaml v1.19.2 + sashabaranov/go-openai v1.43.0（只做协议调用，禁自动工具循环）+ modernc.org/sqlite v1.60.1 + robfig/cron/v3 v3.0.1（6 段：cron.NewParser(Second|Minute|Hour|Dom|Month|Dow)，每任务时区经 CRON_TZ= 前缀）+ modelcontextprotocol/go-sdk v1.8.0 + fsnotify v1.10.1 + 标准库 net/http + ServeMux + log/slog + testify v1.12.1 + [测试专用] github.com/santhosh-tekuri/jsonschema/v6 v6.0.3（仅 *_test.go 导入；必要传递依赖如 golang.org/x/text 以 go mod tidy 实测为准，运行时不进发布二进制）；凭证 ${ENV_VAR} 占位；不用 ORM（手写 SQL + schema.sql 幂等）；CGO_ENABLED=0；本技术栈句与已批准计划内的依赖直接落地、表外新增坐标或版本变更先问用户；GOPROXY=https://goproxy.cn；写坐标前先 go list -m -versions 核实`。本 US 追加：US-16 的 pgx/v5 v5.11.0、US-11 的 Vue 3 + Vite + TypeScript + Vitest（版本以 `assets/console/package-lock.json` 为准）沿用，**不新增** Go / npm 坐标——严格 JSON 用 `encoding/json`，哈希用 `crypto/sha256`，不引 diff / schema / JSON-schema 库。

**Storage**: PostgreSQL v10 → v11，只追加 `migrations/0011_memory_learning.sql`（五张新表 + `memory_entries_source_check` / `llm_calls_purpose_check` 两个 CHECK 扩展），时间列 bigint 毫秒；SQLite 仅随 `core.RunStore` 签名与 `FinishRun` / `ReleaseRunRefs` 拆分适配，不新增学习路径；草案文件落 `.basalt/learning/drafts/<draft_id>/{DRAFT.md,evidence.json}`（只读、固定布局、不在任何 Agent 白名单）。

**Testing**: 12 个 stage check（`us22-base-memory / core / provider / app / pg / web / cli / e2e / console-test / console-build / race / memory-race`）+ 公共 Harness `harness/acceptance.json`；全部用本地协议桩与冻结夹具，**不调用真实模型、不读真实凭据、不连外网**；`make check` 七目标仍是完成的定义，stage 是超集入口。

**Target Platform**: Windows 11 + Git Bash 开发机；Linux/amd64 发布目标；单实例部署，不要求容器或外部网络。

**Performance Goals**: 钩子端到端 ≤20s（工作段 13s + 审计回填 5s + 失败回填 2s），**不得起 goroutine**；请求级最坏 5 + 20 + 5 = 30s（终态落库与引用释放各保留 5s 上界，分段独立计时，不叠成 30s 的钩子）；窗口 ≤40 条 / 8 KiB、前文 ≤6 条 / 2 KiB、输出 ≤64 KiB、单次 ≤5 条候选；钩子失败不改变 Run 的终态 / 返回 / 错误。

**Constraints**: 出厂默认关闭；不新增后台 Worker / 持久任务 / 定时扫描；不新增写端点、动作字面量、左侧导航或管理台开关；不自动晋升 / 自动纠正 / 自动删除；不新增第六类配额；不做全局层 / 个人层 / 跨 Agent 学习；不改写冻结基线或已发布迁移；效果类结论一律 `未测量`。

**Scale/Scope**: 每 Agent 每滚动 24h ≤200 次抽取尝试（代码常量）；抽取行按预算增长；列表沿用仓储 `Limit` 语义；单实例内同 Agent 抽取由 advisory lock 串行，跨节点协调不在本 US。

## Constitution Check

*GATE: 实施前与设计后各核对一次。*

| 原则 | 本次落实 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct | 不触碰循环体与工具调度；抽取不带 `Tools`，不执行任何工具 / 脚本 | 通过 |
| II 客户端只做协议调用；唯一消息仓储 | `ChatMemoryExtract` 只做一次协议调用；消息**只读**（`MessagesForExtraction` / `MessagesForFrozenWindow`）；不新增任何消息追加路径，用户输入与工具结果仍由原路径写入 | 通过 |
| III Provider 显式映射 | 抽取取**触发 Run 冻结**的 provider / model / config，经该 Run 的固定映射查表；`catalogProviderRouter` **不实现** `MemoryExtractor`，生产装饰器（`guardedProviderClient` / `diagnosticProviderClient`）**必须实现** | 通过 |
| IV 一目录一 Agent；技能声明式绑定 | 不改变 `AGENT.md` frontmatter 与渐进披露；草案只在 `.basalt/learning/drafts`，不进任何 Agent 读写白名单，不是技能 | 通过 |
| V 审计 day-one fail-closed | 抽取行 `pending` → `llm_calls` `pending`（`memory_extract` / 轮次 0）→ 才发调用；任一预写失败不调用；确认先写 `memory.confirm` `pending`；明细不含正文 | 通过 |
| VI 授权与执行分离 | 不触碰 `Policy` / `CommandRunner`；不调用任何 runner | 通过 |
| VII 同步 + 有界后台例外 | 严格同步、单钩子、无 goroutine、无 Worker、无持久任务类型；业务状态显式传参，不用 `context.WithValue` 承载 | 通过 |
| VIII 单二进制 | 零新增 Go / npm 坐标；`CGO_ENABLED=0`；`-race` 与发布构建分属不同命令 / job；console 只新增只读页签 | 通过 |
| IX 手写 SQL、迁移只追加 | 新增 `0011_memory_learning.sql`；`schema.pg.sql` 冻结基线不改；已发布迁移不改写 | 通过 |
| X 五大能力优先、范围卡紧 | 只交付已批 US-22 范围；D-2 效果实验、自动晋升、管理台开关、企业治理全部明确不做 | 通过 |

**依赖方向**：`core` 只加纯类型 / 哨兵 / 严格 JSON 与凭证扫描（`internal/app/admission_secret.go` 的规则**下沉**到 `internal/core/secret_scan.go`，app 与 memory 共用）；`memory` 提供抽取服务与输出校验端口，只依赖 `core` 与标准库；`provider` 追加协议路径；`storage/postgres` 实现仓储、迁移与召回过滤；`app` 编排装配、管理面与恢复；`web` / `cli` 只依 `core` 接口。**无新增包**，`core` 不 import 其他 `internal` 包。

**设计后复核**：Phase 1 产物（`data-model.md` / `contracts/README.md` / `quickstart.md`）未引入新依赖、新写端点、新动作字面量或第二套审计 / 消息路径；结论维持「通过」，无 Complexity Tracking 项。

## Project Structure

### Documentation (this feature)

```text
specs/022-controlled-evolution/
├── plan.md              # 本文件
├── research.md          # Phase 0 输出
├── data-model.md        # Phase 1 输出
├── quickstart.md        # Phase 1 输出
├── contracts/README.md  # Phase 1 输出（指向权威契约四件）
├── spec.md              # 已审规格（review ① 派生）
├── checklists/requirements.md
├── run-state.md         # 过程状态（非设计工件）
└── tasks.md             # Phase 2 输出（/speckit-tasks 生成）
```

### Source Code (repository root)

```text
internal/core/
├── learning.go              # 新增：LearningRunInput / LearningHook / MemoryExtractor / 游标与读模型类型 / 哨兵
├── secret_scan.go           # 新增：凭证扫描器（自 internal/app/admission_secret.go 下沉）
├── provider.go              # 追加：LLMCallPurposeMemoryExtract / MemoryExtractCallStore / ChatRequest.HookDeadline
├── memory_admin.go          # 追加：MemoryConfirmRequest / MemoryConfirmResolver / SupersededID / MemoryCandidateMeta / MemoryEntryRecord.Candidate
├── service.go               # 改造：ServiceOptions.Learning；finish = FinishRun(applied) → 条件钩子 → defer ReleaseRunRefs
└── store.go                 # 改造：FinishRun 返回 (bool, error)；新增 ReleaseRunRefs

internal/memory/
├── learning.go              # 新增：LearningService.AfterRun、预算 / 窗口 / 游标 / 重试 / 提交编排
├── extract_output.go        # 新增：严格 JSON 解码（≤64 KiB / 无未知键 / 无尾随）
└── extract_validate.go      # 新增：逐项证据与类型闭集校验、去重、来源子集、建议目标、确定性派生

internal/provider/service.go # 改造：共享 chat 路径；ChatMemoryExtract（round=0、专用预写、回填截止 min(now+5s, HookDeadline)）

internal/storage/
├── run_store.go             # 改造（SQLite）：FinishRun 只留终态；ReleaseRunRefs 独立
└── postgres/
    ├── learning_store.go          # 新增：五表读写、预算 / 游标 / 重试 / 恢复 / 读模型
    ├── memory_confirm.go          # 新增：统一确认事务（全部门禁 + CAS + 审计）
    ├── migrations/0011_memory_learning.sql  # 新增
    ├── run_store.go               # 改造：FinishRun / ReleaseRunRefs 拆分
    ├── memory_store.go           # 改造：LoadCandidateMeta / ConfirmCandidate / valid_to / derivedLiveFilterSQL 三入口
    ├── session_store.go          # 改造：MessagesForExtraction / MessagesForFrozenWindow
    ├── llm_calls.go              # 改造：BeginMemoryExtractCall 委托 beginPendingCall
    └── connect.go                # 改造：SupportedSchemaVersion = 11

internal/app/
├── learning_admin.go        # 新增：LearningAdmin（状态 / 抽取 / 草案 / candidate 读模型）
├── learning_recovery.go     # 新增：recoverLearning（只查证不重放）
├── memory_admin.go          # 改造：ConfirmWithResolution；ListEntries 填 Candidate
├── providers.go             # 改造：catalogProviderRouter.WithMemoryExtractStore、Pinned 携带仓储、路由器不实现 MemoryExtractor
├── resources.go             # 改造：guardedProviderClient.ChatMemoryExtract
├── diagnostics_client.go    # 改造：diagnosticProviderClient.ChatMemoryExtract（不记诊断事件）
├── admission_secret.go      # 改造：委托 core 扫描器（规则单份）
├── wire.go                  # 改造：装配 LearningService / LearningAdmin / resolver / recoverLearning
└── resources.go / run.go / web.go 既有启动兜底接线

internal/web/
├── handlers_learning.go     # 新增：五个只读端点
├── handlers_admin_memory.go # 改造：确认 handler 解析可选 body
├── admin_routes.go          # 改造：learning 只读路由（不新增导航入口）
└── contract_test.go         # 改造：OpenAPI 契约覆盖

internal/cli/
├── learning.go              # 新增：status / extractions list|show / drafts list|show / handoff
└── config.go                # 改造：LearningConfig + 校验

internal/e2e/us22_learning_test.go   # 新增：六个 E2E
assets/console/                      # 记忆页三页签、learning-records、测试、dist 重建
harness/stages/us22-base.json        # 新增 stage
docs/contracts/memory-learning.md、openapi.yaml、run-and-errors.md、schema.pg.sql
docs/TechnicalDesign.md、docs/Requirements.md、docs/Operations.md
```

**Structure Decision**: 沿用单 Go module + `internal/` 分包；本 US 不新增包，落位与颗粒度文档 §3.1 / §3.2 一致。`core` 只承载纯类型与接口（消费方声明），实现分属 `memory` / `provider` / `storage` / `app`；`app` 仍是唯一装配层。

## Implementation sequence

1. **存储与签名地基**：`0011_memory_learning.sql` + `SupportedSchemaVersion = 11`；`core.RunStore.FinishRun` 返回值与 `ReleaseRunRefs` 拆分（SQLite 与 PG 同步）；`core.LearningRunInput` / `LearningHook` / `MemoryExtractor` / 哨兵 / 读模型类型；`core/secret_scan.go` 下沉与 US-21 回归。
2. **provider 两阶段审计**：共享 `chat` 路径、`ChatMemoryExtract`（round 0）、`ChatRequest.HookDeadline`、回填截止在回填开始时计算；`llm_calls.go` 委托；装饰器实现、路由器不实现。
3. **memory 抽取服务**：窗口 / 前文 / 游标 / 预算 / 冻结重试 / 失败分类 / 严格 JSON / 逐项校验 / 草案写盘 / 提交；钩子总预算与 panic 保护。
4. **确认与召回复核**：`memory_confirm.go` 统一事务（空 body 也全门禁、CAS、409 分类）；`memory_store.go` 的 `valid_to` 与 `derivedLiveFilterSQL` 三入口接入。
5. **app 装配与恢复**：`LearningAdmin`、`ConfirmWithResolution`、`recoverLearning`、`wire.go` 单点装配；未启用时全端口关闭。
6. **只读面**：web 五个 GET 端点 + 可选确认 body；CLI 只读命令与 `handoff` 薄适配；console 记忆页三页签与 dist 重建。
7. **契约与文档**：`docs/contracts/memory-learning.md` 新增，`openapi.yaml` / `run-and-errors.md` 追加，Requirements / TechnicalDesign / Operations 增量修订（逐字按卡 §2.4）。
8. **Harness 与收尾**：`harness/stages/us22-base.json`（12 checks + protect）、`TestUS22_*` 落地；逐任务验收 → 完整 stage → `make check` 七目标 → analyze 复跑 → 独立复核 → 七项证据。

## Safety and lifecycle

- **默认关闭**：未配置 `learning.auto_extract` 时 `ServiceOptions.Learning == nil`，零写入、零模型调用、零正文日志；启用需显式 `learning.agents`，空 / 重复 / 非法名启动失败，未知 Agent WARN + 状态页列出、不抽取。
- **唯一触发点**：`finish` 固定顺序「终态落库（`applied`）→ 条件钩子 → `defer ReleaseRunRefs`」；跳过条件 `!applied || Status != completed || Flow != nil || FixedTool != nil || channel ∉ {web,cli}`；释放无条件执行，启动 `releaseTerminalRefs` 兜底。
- **两级预写**：抽取行 `pending` → `llm_calls` `pending`，任一失败不调用；每次尝试一次调用、不重试不 fallback；重试绑定本次 Run 的 provider / model / config。
- **不可信输出**：严格 JSON（≤64 KiB、UTF-8、无未知键、无尾随）→ 逐项类型 / 内容 / 证据（窗口内、角色、逐字子串）/ 临时约束（未来绝对 `valid_to`）/ 去重 / 来源子集 / 建议目标；凭证命中丢弃该条；`certainty` 由程序派生。
- **候选不可见**：`state='candidate'`、`source='auto_extract'`、`section='archive'`，只有 `active` 才进召回 / 注入；管理台「待确认」只走既有确认 / 删除动作。
- **确认门禁**：自动候选（`source=auto_extract` 或存在 meta）即使空 body 也进入统一事务；候选 CAS、候选到期、来源闭包 active 且未过期、来源 Key 未撤销未到期、来源调用方未停用、替代目标与建议逐字一致且仍 active；任一不满足 409 且回滚；显式提议走旧路径；审计 `memory.confirm` 先 `pending`。
- **派生复核**：`derivedLiveFilterSQL` 在三个 SQL 入口（`RecallByKeyword`、`RecallEntries` 关键词回退、`RecallEntries` 向量）共用，递归深度 8 fail-closed，过滤先于排序与 top-k；显式条目不受影响。
- **草案只读**：固定布局、只读文件、不进白名单、不执行；`handoff` 取实例锁 + `cli:<OS user>` + `user_confirmed` + 强制 `kind=model_proposal` / `ref=learning/draft/<id>`，只调 `AdmissionAdmin.Propose`。
- **恢复只查证**：启动扫 `pending`；有候选元数据或草案行 → `success`，否则 `unknown`；同事务推进游标 + 写 `verified_at`；未查证 `unknown` 阻断同来源新窗口，查证后解除；不重放、不删草案、不写 `failed` 覆盖。
- **时延有界**：20s 总预算 = 13s 工作 + 5s 审计回填 + 2s 失败回填；预写受 `workCtx` 约束、不 `WithoutCancel` 外扩；钩子不起 goroutine，失败不影响 Run。

## Internal clarifications

以下取舍已由已审颗粒度文档 §6.4 的 Q1–Q15 与 §2.3 的伪代码确定，plan 不再另立决策：

1. **范围**（Q1）：本 US = D 的确定性第一切片（离线行为 / 权限 / 失败验收）；效果实验归 D-2（拟），需另行立项并解除真实模型禁令。
2. **同步而非后台**（Q2）：20s 是包含收尾的总预算；先定对生命周期顺序，不引入 Worker。
3. **固定 `archive`**（Q3）：不做「确认后迁核心区」。
4. **预算是尝试次数**（Q4 / Q6）：200 次 / 滚动 24h 为代码常量；不新增第六类配额，抽取是实例侧开销。
5. **两个新字面量**（Q5）：`source='auto_extract'`、`purpose='memory_extract'` 同步进 Go 闭集、契约与只追加迁移。
6. **未知 Agent**（Q7）：WARN + 状态页，不抽取、不阻止启动；名单为空 / 重复 / 非法名则启动报错。
7. **handoff**（Q8 / Q11）：提供 CLI 薄适配，只调 `Propose`；草案目录 `.basalt/learning/drafts`。
8. **窗口与证据**（Q9 / Q10）：只取 user / assistant；证据保留引文、来源身份与时刻，不承诺无限保留。
9. **共用扫描器**（Q12）：US-21 的凭证规则下沉 `core`，保留 US-21 回归。
10. **确认处置**（Q13 / Q14）：可选 body；所有自动候选走全门禁，空 body = `keep_both`；未装配 resolver 拒绝而不降级；Key 撤销、Key 到期、调用方停用均阻止确认。
11. **§9.6.6 限定自动保存**（Q15）：不纳入本 US，另行独立评审。
