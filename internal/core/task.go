package core

import (
	"context"
	"errors"
	"time"
)

// 本文件是调度面的领域类型与消费方接口（签名取自 docs/us/us24-state-distributed.md
// §3.1.3 / §3.1.4；US-24 把触发从进程内定时器改为数据库到期扫描）。
// TaskStore / RunInvoker / CronEngine 的实现在 storage / *core.AgentService / adapters/cron；
// core 只依赖标准库（含 context / errors / log/slog / time），不 import 任何实现包。
// This file holds the scheduling domain types and consumer-side interfaces. Implementations
// live in storage, *core.AgentService and adapters/cron; core depends on the standard
// library only. US-24 replaced in-process timers with database due-scanning.

// ExecutionStatus 是执行行的状态四值（契约 schema.sql：started / completed / failed / skipped）。
// ExecutionStatus is the four-value state literal of an execution row.
type ExecutionStatus string

const (
	// ExecutionStarted 是 `StartExecution` 写入的起始态（Run 之前）。
	ExecutionStarted ExecutionStatus = "started"
	// ExecutionCompleted 对应 Run 终态 completed / limit_reached。
	ExecutionCompleted ExecutionStatus = "completed"
	// ExecutionFailed 对应 Run 终态 failed / cancelled，或无 Run 行的崩溃遗留收尾。
	ExecutionFailed ExecutionStatus = "failed"
	// ExecutionSkipped 表示本轮未执行（锁失 / 会话已归档 / 兄弟任务占会话），**无 Run 行**。
	ExecutionSkipped ExecutionStatus = "skipped"
)

// Trigger 是执行行的触发来源两值（与 runs 表的 trigger 列同字面量）。
// Trigger is the two-value origin literal of an execution row.
type Trigger string

const (
	// TriggerCron 表示定时到点触发。
	TriggerCron Trigger = "cron"
	// TriggerManual 表示手动补跑触发。
	TriggerManual Trigger = "manual"
)

// ScheduleAction 是 manage_schedule 工具的动作三值。
// ScheduleAction is the action literal set of the manage_schedule tool.
type ScheduleAction string

const (
	// ScheduleSet 表示新增或改写一条定时任务定义。
	ScheduleSet ScheduleAction = "set"
	// ScheduleRemove 表示按 id 删除一条定时任务定义。
	ScheduleRemove ScheduleAction = "remove"
	// ScheduleList 表示列出本 Agent 的定时任务。
	ScheduleList ScheduleAction = "list"
)

// ErrTaskBusy 表示手动补跑撞上该任务的在途执行（HTTP 409 的唯一判据）。
var ErrTaskBusy = errors.New("scheduler: task busy")

// TaskRecord 是 `scheduled_tasks` 的一行（装载后的运行时快照 + 启停状态 + 运行痕迹）。
// TaskRecord is one scheduled_tasks row: the post-load runtime snapshot.
type TaskRecord struct {
	TaskID  string
	Agent   string
	Cron    string
	Zone    string
	Message string
	// CallerID / AuthorizingKind / AuthorizingKeyID 是任务归属与执行授权来源
	// （`scheduled_tasks` 三列，C-32）：调度执行只能按它们重建 `OwnedPrincipal`，
	// **不得**在执行期改写，也**不得**按有无 Key 反推 kind。
	// The task's owner and authorizing source; scheduling rebuilds the principal from
	// these three columns and never rewrites them at execution time.
	CallerID         string
	AuthorizingKind  AuthorizingKind
	AuthorizingKeyID string
	DeliverTo        string
	Enabled          bool
	DefinitionHash   string
	LastRunAt        time.Time
	LastStatus       ExecutionStatus
	RetiredAt        time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// DueTask 是一条到期扫描结果；NextFireAt 零值表示尚未初始化。
// DueTask is one due-scan result; a zero NextFireAt means "not initialized yet".
type DueTask struct {
	Task       TaskRecord
	NextFireAt time.Time
}

// DueExecution 是一次已领取的计划触发。
// DueExecution is one claimed planned trigger.
//
// RunID 是对冻结字面量（模块卡 §3.1.3）的唯一增量（T014 裁决 ①）：领取事务必须落
// `task_executions.run_id`，而 `ClaimDueTrigger` 的签名没有 RunID 入参。RunID 由存储层
// 在领取事务内生成（`core.NewRunID("scheduler", now, core.RunIDSuffix())`），形态与
// `docs/contracts/run-and-errors.md` 的 `<channel>-<unix_millis>-<6 位 base36>` 一致。
// RunID is the only extension to the frozen literal: the claim transaction must write
// task_executions.run_id, which the frozen signature has no parameter for.
type DueExecution struct {
	Task        TaskRecord
	ExecutionID int64
	PlannedAt   time.Time
	Lease       LeaseToken
	// RunID 由领取事务分配，与 `task_executions.run_id` 同一值。
	RunID string
	// PinnedBundleHash 是领取事务内与执行行同笔写入的固定 bundle 哈希
	// （`task_executions.pinned_bundle_hash`，US-25C FR-25C-08）：Run 准入只按它
	// 复核领取记录，**不得**在准入时重读当时的指针。
	// PinnedBundleHash is fixed by the claim transaction and is the only hash the
	// admission path may trust for this execution.
	PinnedBundleHash string
}

// SweepBounds 是一圈到期扫描的冻结边界（US-25C §2.3.5）：开圈时由**一次**只读查询
// 取数据库时间与本圈 NULL 段的最大 `task_id`。非 NULL 段只考察 `next_fire_at <= Cutoff`，
// NULL 段只考察 `task_id <= NullUpperTaskID`；两者都只出不进，因此一圈必然结束。
// `NullUpperTaskID` 为空串表示本圈没有 NULL 行。
// SweepBounds freezes one sweep: the database clock plus the largest NULL-segment task id.
type SweepBounds struct {
	Cutoff          time.Time
	NullUpperTaskID string
}

// DueScanCursor 是到期扫描的两段键集游标（US-25C FR-25C-11）：`Sweep == nil` 表示
// 下一拍开新圈；`InNullSegment` 为真时按 `task_id` 在 NULL 段前进，为假时按
// `(next_fire_at, task_id)` 前进。非 NULL 段首屏 `AfterNextFireAt == nil`——存储层
// **不得**把 NULL 当元组下界拼接。
//
// 游标由调度器持在 `stateMu` 内（锁内零 I/O），**只有 `Start` 清空**：
// `SetRegistered` / `ReplaceEntries` 只换内存集合，不得复位扫描进度。
// DueScanCursor is the two-segment keyset cursor; only Start ever clears it.
type DueScanCursor struct {
	Sweep           *SweepBounds
	InNullSegment   bool
	NullAfterTaskID string
	AfterNextFireAt *time.Time
	AfterTaskID     string
}

// AssetPinnedClaimer 是到期领取的钉住扩展（TaskStore 的可选扩展，US-25C FR-25C-08）：
// 在**同一领取事务**内锁住发布指针行并核对期望哈希，成功时把哈希写进 started 执行行。
// 指针缺失 / 已撤销与哈希不符分别返回 `ErrAssetNotReady` / `ErrAssetMoved`，两者都
// **零写入**（不写执行行、不推进 `next_fire_at`）。
// AssetPinnedClaimer is the optional pinned-claim extension of TaskStore; both refusal
// outcomes write nothing at all.
type AssetPinnedClaimer interface {
	ClaimDueTriggerPinned(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time,
		owner WorkerIdentity, ttl time.Duration, expectHash string) (DueExecution, error)
}

// ClaimedBundlePin 是调度通道带入 Run 的固定版本凭证（§2.3.5.1）：
// `RunRequest.Pinned` **只由调度通道填**，web / cli / admin 一律 nil。
// ClaimedBundlePin is filled only by the scheduling path; other entries leave it nil.
type ClaimedBundlePin struct {
	ExecutionID int64
	BundleHash  string
	Lease       LeaseToken
}

// ClaimedPinRecord 是准入回读的领取记录（执行行 + 领取时写下的租约副本）：
// `Agent` 经 `task_executions.task_id` 关联 `scheduled_tasks.agent_name` 读出
// （执行行没有 agent 列）；owner / epoch 来自领取时写下的副本，仅供准入的一致性前置。
// ClaimedPinRecord is the claimed execution row plus the lease copy written at claim time.
type ClaimedPinRecord struct {
	ExecutionID    int64
	RunID          string
	Agent          string
	Status         string
	BundleHash     string
	OwnerWorkerID  string
	OwnershipEpoch int64
}

// PinnedClaimReader 是准入路径回读领取记录的消费方端口（实现在 storage）：
// `(零值, false, nil)` 表示查无此行，调用方据此映射为 `ErrClaimPinInvalid`。
// PinnedClaimReader reads the claim record for admission; a miss is (zero, false, nil).
type PinnedClaimReader interface {
	ReadClaimedPin(ctx context.Context, executionID int64) (ClaimedPinRecord, bool, error)
}

// ExecutionRecord 是 `task_executions` 的一行（执行历史；无 Run 的终态行 RunID 为空）。
// ExecutionRecord is one task_executions row; terminal rows without a Run carry an empty RunID.
//
// Delivery / DeliveryChannel / DeliveryDetail 是从 `schedule_deliveries` 左连接来的
// **投递结果**，与 Status 的**执行结果**分开展示（R7）：`Delivery == ""` 表示本行没有
// 代投记录（任务未配置代投或本轮未创建投递行）。
// The delivery fields are projected from schedule_deliveries and stay separate from the
// execution status; an empty Delivery means this row has no delivery record.
type ExecutionRecord struct {
	ID              int64
	TaskID          string
	RunID           string
	Status          ExecutionStatus
	Trigger         Trigger
	DurationMS      int64
	ErrorMessage    string
	Delivery        DeliveryStatus
	DeliveryChannel string
	DeliveryDetail  string
	StartedAt       time.Time
	FinishedAt      time.Time
	// PlannedAt 是定时触发的计划时刻（`(task_id, planned_at)` 去重键）；manual 为 nil。
	// PlannedAt is the planned instant of a cron trigger; nil for manual runs.
	PlannedAt *time.Time
}

// TaskStore 是任务定义与执行历史的仓储接口（实现在 internal/storage，SQL 只在那里出现）。
// TaskStore is the repository interface for task definitions and execution history.
type TaskStore interface {
	ListTasks(ctx context.Context) ([]TaskRecord, error)
	// ApplyAgentTasks 在同一事务内应用该 Agent 的全部定义更新与退休：
	//   - upserts 的归属守卫失败（task_id 属于其他 Agent）→ ErrTaskOwnerConflict；
	//   - retireIDs 中不属于该 Agent 的 id 同样返回 ErrTaskOwnerConflict；
	//   - 任一语句失败 → 全部回滚（无部分更新、无部分退休）。
	ApplyAgentTasks(ctx context.Context, agent string, upserts []TaskRecord, retireIDs []string, now time.Time) error
	SetTaskEnabled(ctx context.Context, taskID string, enabled bool, now time.Time) error
	MarkTaskRun(ctx context.Context, taskID string, at time.Time, status ExecutionStatus) error

	// ListDispatchable 返回 enabled、未退休且 next_fire_at 为 NULL 或已到期的任务。
	// ListDispatchable returns enabled, unretired tasks whose next_fire_at is NULL or due.
	ListDispatchable(ctx context.Context, limit int) ([]DueTask, error)

	// SetNextFire 只初始化 / 推进 next_fire_at；expected 非 nil 时做 CAS。
	// SetNextFire only initializes / advances next_fire_at; expected non-nil makes it a CAS.
	SetNextFire(ctx context.Context, taskID string, expected *time.Time, next time.Time) error

	// ClaimDueTrigger 单事务：插入 started 执行行（唯一 planned_at）、CAS 推进
	// next_fire_at、写 schedule_execution 租约。冲突分别返回 ErrTriggerAlreadyClaimed /
	// ErrTaskBusy / ErrStaleNextFire。
	ClaimDueTrigger(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time,
		owner WorkerIdentity, ttl time.Duration) (DueExecution, error)

	// RecordMissedTrigger 单事务：写一条 skipped 行并 CAS 推进 next_fire_at。
	// RecordMissedTrigger writes one skipped row and CAS-advances next_fire_at in one tx.
	RecordMissedTrigger(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time,
		missed int, reason string) error

	// RecordBusyTrigger 单事务：写一条 skipped(task_busy) 行并 CAS 推进 next_fire_at。
	// 与 RecordMissedTrigger 同理：不推进会让该 planned_at 被 skipped 行占住，
	// 真正的领取永远撞 (task_id, planned_at) 唯一约束。
	// Like RecordMissedTrigger: without advancing, the skipped row would pin planned_at
	// forever and every real claim would hit the (task_id, planned_at) unique constraint.
	RecordBusyTrigger(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time) error

	// StartExecution 单事务写执行行与租约；同任务已有 started 执行 → ErrTaskBusy。
	// StartExecution writes the execution row and its lease in one tx; a started row
	// for the same task yields ErrTaskBusy.
	StartExecution(ctx context.Context, rec ExecutionRecord, owner WorkerIdentity,
		ttl time.Duration) (int64, LeaseToken, error)

	// FinishExecution 带 token 做 fencing，并同步 last_status；释放租约。
	// FinishExecution fences on the token, syncs last_status and releases the lease.
	FinishExecution(ctx context.Context, id int64, token LeaseToken, status ExecutionStatus,
		duration time.Duration, errMsg string, at time.Time) error

	// RecordSkipped 保持既有签名，只服务 manual / RunNow 的跳过（无 planned_at）。
	RecordSkipped(ctx context.Context, rec ExecutionRecord) error
	ListExecutions(ctx context.Context, taskID string, limit int) ([]ExecutionRecord, error)
}

// AgentDueLister 是到期扫描的**可选扩展**（不属于冻结的 TaskStore 面）：只返回属于
// 给定 Agent 集合的到期任务，并按两段键集游标分页（US-25C FR-25C-11）。
//
// 为什么需要它：`ListDispatchable` 是全局批次（所有 Worker 的 Agent 一起按 next_fire_at
// 排序取前 N 条）。多 Worker 下其他工作区的到期行既不归本进程领取、又不会被本进程推进，
// 当它们的条数达到单轮上限时，本地 Agent 的任务**永远**进不了批次（F6 饿死）。
// 存储实现本扩展时，调度器按本地注册集合过滤后再取；未实现（单进程 / 轻装配）时退回
// 全局批次 + 内存过滤，语义与从前一致。
//
// 扫描公平的扩展面（卡 §3.1.2）：`ReadSweepBounds` **每圈一次**取冻结边界（数据库时间
// 与 NULL 段上界），`ListDispatchableForAgents` 单拍只取一页；`after == nil` 表示调用方
// 尚未开圈，存储层**不得**自行取边界，只按本拍传入的游标取一页（`Sweep` 为空时按
// 等价于旧全局批次的语义取首屏）。
// AgentDueLister is an optional due-scan extension: it narrows the batch to the given
// agents and pages it through the two-segment keyset cursor.
type AgentDueLister interface {
	ListDispatchableForAgents(ctx context.Context, agents []string, after *DueScanCursor,
		limit int) ([]DueTask, error)
	ReadSweepBounds(ctx context.Context, agents []string) (SweepBounds, error)
}

// DBNowReader 是扫描时钟的可选扩展：有数据库时钟时，到期判定、错失宽限与下一次触发
// 一律以它为基准（模块卡 §2.3.4 的 `db_now`）。未实现时退回装配时钟（轻装配 / 单测）。
// DBNowReader is the optional database clock; the card requires due, grace and next-fire
// decisions to be based on db_now, not the process clock.
type DBNowReader interface {
	DBNow(ctx context.Context) (time.Time, error)
}

// RunInvoker 是调度器调用运行面的最小端口（实现是 *core.AgentService）——调度与手动补跑
// 共用同一 `Run` 入口，不另开执行链。
// RunInvoker is the minimal port the scheduler uses to invoke runs; scheduled and manual
// runs share the same Run entry point.
type RunInvoker interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// CronEngine 只保留“按表达式算下一次 / 枚举区间”；进程内定时器 API 由本 US 删除。
// CronEngine now only computes the next occurrence or enumerates a range; the
// in-process timer API was removed by US-24.
type CronEngine interface {
	Next(spec string, from time.Time) (time.Time, error)
	Occurrences(spec string, from, to time.Time, limit int) []time.Time
}
