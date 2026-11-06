# US22 实施研究

- Decision：抽取只在 `finish` 的固定顺序「终态落库（`FinishRun` 返回 `applied`）→ 条件钩子 → 无条件 `defer ReleaseRunRefs`」上触发，不挂 web / CLI 回调、不订阅事件。Rationale：唯一触发点才能保证「终态已持久化才抽取」与「释放排在抽取之后」；Alternative：各渠道各挂一处拒绝（第二套触发路径，且释放先于抽取会让凭据守点拒绝）。
- Decision：`FinishRun` 只写终态并返回 `applied`，同事务的三类引用释放拆成 `ReleaseRunRefs`（内部按 `status <> 'running'` 守卫）。Rationale：钩子运行期间凭据 `released_at` 必须仍为空，否则 `credential_guard` 必然拒绝；Alternative：保留同事务释放并让钩子自带凭据拒绝（等于绕过守点）。
- Decision：抽取严格同步、端到端 20s（工作 13s + 审计回填 5s + 失败回填 2s），不起 goroutine。Rationale：宪法 VII 只批准 US-19 顺序 Flow 的有界后台例外，本 US 不申请新例外；Alternative：后台 Worker / 持久任务拒绝（需原则修订且会派生第二套执行路径）。
- Decision：回填截止在**开始回填时**计算 `min(now+5s, HookDeadline)`，预写受 `workCtx`（13s）约束、不 `WithoutCancel` 外扩。Rationale：预先算死或用 `workCtx` 截止都会在慢调用 / 超时后无法落终态；独立 5s 预写会叠在 20s 预算之外；Alternative：预写单独 5s 或回填沿用工作段拒绝。
- Decision：窗口在首次尝试后按 `window_from_message_id` / `window_to_message_id` 冻结，重试只重读这段并按实际条数复核完整性；游标按（Agent、会话、来源调用方、来源 Key）推进。Rationale：重试换窗口会产生与证据不一致的抽取，跨来源重贴会把 A 的消息记成 B 的来源；Alternative：重试重算窗口拒绝。
- Decision：来源身份只由 `BeginExtraction` 从 `runs.(caller_id, key_id)` 复制，请求路径没有写入入口；`key_id IS NULL` 落 `''`。Rationale：来源是权限与召回复核的根，任何请求可控来源都是提权面；Alternative：让调用方传 origin 拒绝。
- Decision：模型输出用 `encoding/json` + `io.LimitReader`（64 KiB）+ `DisallowUnknownFields` + 解码后无尾随内容，不引 JSON-schema 库。Rationale：本 US 零新增依赖是已批约束，且逐项校验本身必须用 Go 代码表达；Alternative：引 schema 库拒绝（表外坐标）。
- Decision：`certainty` 由程序按证据与类型派生，模型自报 `confidence` 只落元数据、不作门槛。Rationale：模型自报置信度是不可信输入的一部分，不能决定是否落候选；Alternative：设阈值过滤拒绝（阈值无测量依据）。
- Decision：候选正文复用 `memory_entries`（`state='candidate'`、`source='auto_extract'`、`section='archive'`），证据 / 来源 / 抽取状态放四张侧表。Rationale：召回、删除、备份、审计全部复用既有路径，无需第二套条目模型；Alternative：新建 candidate 表拒绝（召回面要再写一套并集查询）。
- Decision：确认复用既有 `POST /api/v1/memory/entries/{id}/confirm` 的可选 body，自动候选一律进统一事务（空 body = `keep_both`），显式提议保持旧 `Confirm`。Rationale：不新增写端点与动作字面量，且「人工确认」必须一次执行全部门禁；Alternative：新增 `/memory/candidates/{id}/confirm` 拒绝（新写面 + 两套门禁）。
- Decision：派生复核写成 SQL 片段 `derivedLiveFilterSQL`（`NOT EXISTS (WITH RECURSIVE …)`），在三个 SQL 入口与排序 / top-k 之前执行，深度上限 8 fail-closed。Rationale：只在确认时复核会被召回面绕过，只在 Go 里后过滤会让失效条目挤掉有效名额；Alternative：Go 内存过滤拒绝。
- Decision：US-21 的凭证扫描器下沉到 `internal/core/secret_scan.go`，app 与 memory 共用一份规则。Rationale：app 已依赖 memory，memory 不能反向 import app；复制第二份规则必然漂移；Alternative：memory import app 拒绝（依赖倒置成环）。
- Decision：方法草案写 `.basalt/learning/drafts/<draft_id>/{DRAFT.md,evidence.json}` + `memory_method_drafts` 索引行，只读、固定布局、不进任何 Agent 白名单；`learning handoff` 是薄适配，只调 `AdmissionAdmin.Propose`。Rationale：草案不是技能也不是记忆条目，评估 / 批准 / 发布仍归 US-21；Alternative：handoff 直接评估或发布拒绝（绕过准入状态机）。
- Decision：管理台并入既有「记忆」页三页签（`?tab=library|pending|records`），不新增左侧入口、不新增 `/console/learning*` 路由、不新增写按钮。Rationale：学习面只读，确认 / 删除复用既有记忆动作；Alternative：独立学习导航拒绝（已批卡片明确不做）。
- Decision：迁移 `0011` 只追加：两个 CHECK 用 `DROP CONSTRAINT IF EXISTS` + `ADD CONSTRAINT` 按定义替换，五张新表 `CREATE TABLE IF NOT EXISTS`；`docs/contracts/schema.pg.sql` 冻结基线不改。Rationale：旧约束会继续拒绝新取值，只 ADD 会留下双重约束；Alternative：改写已发布迁移拒绝。
- Decision：每 Agent 每滚动 24h 的 200 次尝试是代码常量，预算判定与行写入 / 重试递增同事务并取 Agent 级 advisory lock。Rationale：配置键会扩大管理面写面，无锁并发会突破上限；Alternative：配置键或进程内计数拒绝。
- Decision：启动恢复只查证不重放——有候选元数据或草案行判 `success`，否则 `unknown`；两者都在同一事务推进游标并写 `verified_at`；未查证 `unknown` 与 `pending` 阻断同来源新窗口。Rationale：提交结果未知不等于未发生，重放会重复计费并可能重复落候选；Alternative：按最后一步重做拒绝。

实际 PostgreSQL 迁移前向、并发确认单赢家、冻结窗口清理与恢复三态在实现阶段以集成用例取证，不把设计写成已实测。预期失效与重建成本已在卡 §2.6 矩阵逐条给出。
