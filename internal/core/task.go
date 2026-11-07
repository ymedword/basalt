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
// 给定 Agent 集合的到期任务。
//
// 为什么需要它：`ListDispatchable` 是全局批次（所有 Worker 的 Agent 一起按 next_fire_at
// 排序取前 N 条）。多 Worker 下其他工作区的到期行既不归本进程领取、又不会被本进程推进，
// 当它们的条数达到单轮上限时，本地 Agent 的任务**永远**进不了批次（F6 饿死）。
// 存储实现本扩展时，调度器按本地注册集合过滤后再取；未实现（单进程 / 轻装配）时退回
// 全局批次 + 内存过滤，语义与从前一致。
// AgentDueLister is an optional due-scan extension: it narrows the batch to the given
// agents so rows belonging to other workers can never starve the local pass.
type AgentDueLister interface {
	ListDispatchableForAgents(ctx context.Context, agents []string, limit int) ([]DueTask, error)
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
