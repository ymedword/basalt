package core

import (
	"context"
	"errors"
	"time"
)

// 本文件是调度面的领域类型与消费方接口（签名逐字取自 docs/us/us10-scheduler.md §3.1.1）。
// TaskStore / RunInvoker / CronEngine 的实现在 storage / *core.AgentService / adapters/cron；
// core 只依赖标准库（含 context / errors / log/slog / time），不 import 任何实现包。
// This file holds the scheduling domain types and consumer-side interfaces. Implementations
// live in storage, *core.AgentService and adapters/cron; core depends on the standard
// library only.

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
	TaskID         string
	Agent          string
	Cron           string
	Zone           string
	Message        string
	DeliverTo      string
	Enabled        bool
	DefinitionHash string
	LastRunAt      time.Time
	LastStatus     ExecutionStatus
	RetiredAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
	StartExecution(ctx context.Context, rec ExecutionRecord) (int64, error)
	FinishExecution(ctx context.Context, id int64, status ExecutionStatus, duration time.Duration, errMsg string, at time.Time) error
	RecordSkipped(ctx context.Context, rec ExecutionRecord) error
	RecoverStartedExecutions(ctx context.Context, now time.Time) (int, error)
	ListExecutions(ctx context.Context, taskID string, limit int) ([]ExecutionRecord, error)
}

// RunInvoker 是调度器调用运行面的最小端口（实现是 *core.AgentService）——调度与手动补跑
// 共用同一 `Run` 入口，不另开执行链。
// RunInvoker is the minimal port the scheduler uses to invoke runs; scheduled and manual
// runs share the same Run entry point.
type RunInvoker interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// CronHandle 是一次任务注册的停止句柄。
// CronHandle stops one registered job.
type CronHandle interface{ Stop() }

// CronEngine 是 cron 引擎的消费方接口（实现在 internal/adapters/cron）。
// CronEngine is the consumer-side interface of the cron engine.
type CronEngine interface {
	Add(spec string, job func()) (CronHandle, error)
	Start()
	Stop() context.Context
	Occurrences(spec string, from, to time.Time, limit int) []time.Time
}
