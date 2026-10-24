package core

// 本文件是定时任务系统代投的领域类型与端口（US-12 R7）：投递目标绑定资源 UID、
// 发送前先落权威记录、结果三分（成功 / 确定失败 / 未知）、与执行结果分开展示。
// This file defines the system-delivery domain for scheduled tasks: the target is a
// resource uid, the authoritative row is written before sending, and the outcome is
// three-valued (sent / failed / unknown) and shown separately from the run result.

import (
	"context"
	"time"
)

// DeliveryStatus 是系统投递记录的状态五值（契约 schema.sql）。
// DeliveryStatus is the five-value state of a schedule delivery row.
type DeliveryStatus string

const (
	// DeliveryPending 是发送前的预写态；进程在此态退出时由启动恢复改判。
	DeliveryPending DeliveryStatus = "pending"
	// DeliverySent 表示服务端已确认受理（飞书另要求回执 code=0）。
	DeliverySent DeliveryStatus = "sent"
	// DeliveryFailed 是确定失败：连接被拒、明确非 2xx、飞书明确回执错误。
	DeliveryFailed DeliveryStatus = "failed"
	// DeliveryUnknown 表示结果未知：超时、连接中段断开、回执不可解析。
	// **不得**把未知当作确定失败盲目重发。
	DeliveryUnknown DeliveryStatus = "unknown"
	// DeliverySkipped 表示本轮没有发送（运行未成功 / 进程退出时运行未完成）。
	DeliverySkipped DeliveryStatus = "skipped"
)

// ScheduleDeliveryRecord 是 `schedule_deliveries` 的一行。它是系统代投的权威记录，
// 与模型工具调用审计（tool_invocations）分开，**不**伪造一次工具调用。
// ScheduleDeliveryRecord is the authoritative system-delivery row; it never fakes a
// model tool invocation.
type ScheduleDeliveryRecord struct {
	RunID       string
	TaskID      string
	Agent       string
	AgentUID    AgentUID
	ChannelUID  ResourceUID
	ChannelName string
	Status      DeliveryStatus
	HTTPStatus  int
	ReceiptCode *int
	Detail      string
	AttemptedAt time.Time
	FinishedAt  time.Time
	CreatedAt   time.Time
}

// DeliveryIntent 是一次系统代投的运行期固定目标：任务、运行、Agent 名与渠道 **UID**
// 在运行开始前确定，运行内不再按名称解析。Agent 身份由投递服务在 Prepare 时从生命
// 周期记录取出并随行落库——调度器只认名称，不持有 Agent UID。
// DeliveryIntent freezes the delivery target for one run; the agent identity is resolved
// by the delivery service at prepare time and stored with the row.
type DeliveryIntent struct {
	RunID      string
	TaskID     string
	Agent      string
	ChannelUID ResourceUID
}

// DeliveryOutcome 是一次发送的终态。Status 只取 DeliverySent / DeliveryFailed /
// DeliveryUnknown；Detail 只含固定短语与 scheme://host，**不得**含地址路径、查询串
// 或凭证。
// DeliveryOutcome is the terminal outcome of one send, with a redacted detail.
type DeliveryOutcome struct {
	Status      DeliveryStatus
	HTTPStatus  int
	ReceiptCode *int
	Detail      string
}

// ScheduleDelivery 是调度器使用的系统代投端口。Prepare 在运行前写 pending 行（写不
// 进去即不发起运行）；Deliver 只在运行成功后调用；Skip 把未成功的运行标为跳过。
// 崩溃恢复由 Recover 完成：pending + 运行已完成 → unknown（不自动重发）。
// ScheduleDelivery prepares the authoritative row before the run, delivers only after
// a successful run, and recovers pending rows without blind resends.
type ScheduleDelivery interface {
	Prepare(ctx context.Context, intent DeliveryIntent) error
	Deliver(ctx context.Context, intent DeliveryIntent, content string) DeliveryOutcome
	Skip(ctx context.Context, intent DeliveryIntent, reason string) error
	Recover(ctx context.Context, now time.Time) (int, error)
}

// DeliveryStore 是 `schedule_deliveries` 的仓储端口（实现在 internal/storage）。
// DeliveryStore is the repository port of the delivery table.
type DeliveryStore interface {
	// Prepare 插入 pending 行；同一 run_id 重复调用是幂等空操作。
	Prepare(ctx context.Context, record ScheduleDeliveryRecord) error
	// Finish 回填终态；未知 run_id 返回 fs.ErrNotExist。
	Finish(ctx context.Context, runID string, status DeliveryStatus, httpStatus int,
		receiptCode *int, detail string, attemptedAt, finishedAt time.Time) error
	// Get 按 run_id 读取一条记录。
	Get(ctx context.Context, runID string) (ScheduleDeliveryRecord, error)
	// ListByTask 返回某任务最近的投递记录（新→旧）。
	ListByTask(ctx context.Context, taskID string, limit int) ([]ScheduleDeliveryRecord, error)
	// Recover 处理崩溃遗留的 pending 行：关联 Run 已完成 → unknown，否则 skipped。
	Recover(ctx context.Context, now time.Time) (int, error)
}
