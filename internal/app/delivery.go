package app

// 本文件把调度器的系统代投端口接到存储与发送实现上（US-12 R7）：预写 pending 记录、
// 运行时复核渠道资格、发送后回填三分结果、运行未成功时标跳过、启动时恢复遗留行。
// 调度器只认任务与渠道 UID；Agent 身份在这里从生命周期状态取出并随行落库。
// This file wires the scheduler's delivery port to the store and the sender: prewrite,
// run-time eligibility re-check, three-valued outcome, skip, and startup recovery.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// deliveryProfileSource 是代投声明边界复核需要的注册表最小面（进程内唯一的
// `*agentRegistry` 直接满足）：只读、按名取当前装载条目，不缓存。
// deliveryProfileSource is the minimal registry face the declaration check needs; the
// in-process *agentRegistry satisfies it directly.
type deliveryProfileSource interface {
	Lookup(name string) (core.AgentEntry, bool)
}

// deliveryService 是 core.ScheduleDelivery 的实现：一次运行至多一条投递记录，记录在
// 运行前写下，任何失败都不删行。
// deliveryService is the one-row-per-run implementation of core.ScheduleDelivery.
type deliveryService struct {
	store     core.DeliveryStore
	sender    *tool.DeliverySender
	lifecycle core.LifecyclePort
	profiles  deliveryProfileSource
	logger    *slog.Logger
	now       func() time.Time
}

// 编译期断言：这就是调度器消费的系统代投端口。
// Compile-time assertion that the scheduler's delivery port is satisfied.
var _ core.ScheduleDelivery = (*deliveryService)(nil)

// newDeliveryService 装配投递服务；now 缺省 `time.Now`（测试注入固定时钟）。
// `profiles` 缺失是装配错误：没有注册表就证明不了"目标在 Agent 声明的渠道内"，
// Prepare 因此失败关闭而不是跳过边界复核。
// newDeliveryService assembles the delivery service; a missing profile source keeps the
// declared-channel boundary failing closed.
func newDeliveryService(store core.DeliveryStore, sender *tool.DeliverySender,
	lifecycle core.LifecyclePort, profiles deliveryProfileSource, logger *slog.Logger,
) *deliveryService {
	if logger == nil {
		logger = slog.Default()
	}
	return &deliveryService{
		store:     store,
		sender:    sender,
		lifecycle: lifecycle,
		profiles:  profiles,
		logger:    logger,
		now:       time.Now,
	}
}

// Prepare 在运行**之前**写 pending 行：先按生命周期记录取 Agent 身份，再按渠道 UID
// 复核资格（存在、未删除、本 Agent 可用、类型已装配）。任一步失败即不发起运行——没有
// 记录就不投递，也不允许"先跑完再补记录"。
// Prepare writes the pending row before the run, resolving the agent identity and
// re-checking channel eligibility first; a failure here means the run never starts.
func (d *deliveryService) Prepare(ctx context.Context, intent core.DeliveryIntent) error {
	agentUID, err := d.agentUID(ctx, intent.Agent)
	if err != nil {
		return err
	}
	channel, err := d.sender.Lookup(ctx, intent.ChannelUID, agentUID)
	if err != nil {
		return err
	}
	if err := d.checkDeclaredChannel(intent.Agent, channel); err != nil {
		return err
	}
	record := core.ScheduleDeliveryRecord{
		RunID:       intent.RunID,
		TaskID:      intent.TaskID,
		Agent:       intent.Agent,
		AgentUID:    agentUID,
		ChannelUID:  channel.ResourceUID,
		ChannelName: channel.Name,
		Status:      core.DeliveryPending,
		CreatedAt:   d.now(),
	}
	if err := d.store.Prepare(ctx, record); err != nil {
		return fmt.Errorf("写入投递预写记录失败: %w", err)
	}
	return nil
}

// checkDeclaredChannel 复核预写前的声明边界（R7："Agent 的渠道仍是权限边界"）：
// 注册表条目缺失、profile 解析失败或目标不在声明并绑定的 UID 内都失败关闭。
// 运行开始后不再重复这道判定——目标已随预写行固定，Deliver 只按 UID 复核资格。
// checkDeclaredChannel fails closed when the target is outside the agent's declared
// channels; the boundary is enforced when the target is frozen, not mid-run.
func (d *deliveryService) checkDeclaredChannel(agent string, channel core.NotifyChannel) error {
	if d.profiles == nil {
		return fmt.Errorf("装配错误：Agent %s 未接入注册表，无法复核代投渠道声明", agent)
	}
	entry, ok := d.profiles.Lookup(agent)
	if !ok || entry.Profile == nil {
		return fmt.Errorf("代投渠道声明复核失败：Agent %s 不在注册表中", agent)
	}
	if !deliveryWithinDeclaredChannels(entry.Profile, channel.ResourceUID) {
		return fmt.Errorf(
			"通知渠道 %s 不在 Agent %s 声明的通知渠道内，本轮不发起运行", channel.Name, agent)
	}
	return nil
}

// Deliver 在运行成功后发送并回填终态。目标与身份取自**预写行**（运行开始前固定），
// 只按 UID 重新读渠道做资格复核——不按名称解析，也不改用运行后的新配置。
// Deliver sends after a successful run; target and identity come from the prewritten row,
// and only eligibility is re-checked by uid.
func (d *deliveryService) Deliver(ctx context.Context, intent core.DeliveryIntent,
	content string,
) core.DeliveryOutcome {
	started := d.now()
	record, err := d.store.Get(ctx, intent.RunID)
	if err != nil {
		d.logger.Error("读取投递预写记录失败，本次不发送",
			"task_id", intent.TaskID, "run_id", intent.RunID, "error", err)
		return core.DeliveryOutcome{
			Status: core.DeliveryFailed,
			Detail: "投递预写记录缺失，未发送",
		}
	}

	var outcome core.DeliveryOutcome
	channel, lookupErr := d.sender.Lookup(ctx, record.ChannelUID, record.AgentUID)
	if lookupErr != nil {
		outcome = core.DeliveryOutcome{
			Status: core.DeliveryFailed,
			Detail: "投递前置检查失败：" + lookupErr.Error(),
		}
	} else {
		outcome = d.sender.Deliver(ctx, channel, intent.Agent, content)
	}
	d.finish(ctx, intent, outcome, started)
	return outcome
}

// Skip 把未成功的运行标为跳过：记录保留，页面能看出"跑了但没投递"与"没跑"的区别。
// Skip marks a run that did not succeed; the row stays so the page can tell "ran but not
// delivered" from "never ran".
func (d *deliveryService) Skip(ctx context.Context, intent core.DeliveryIntent, reason string) error {
	now := d.now()
	return d.store.Finish(ctx, intent.RunID, core.DeliverySkipped, 0, nil, reason, now, now)
}

// Recover 处理崩溃遗留的 pending 行：关联运行已完成 → unknown（**不自动重发**），
// 其余 → skipped。由调度器 Start 在恢复执行行之后调用。
// Recover resolves pending rows left by a crash; a completed run becomes unknown and is
// never resent.
func (d *deliveryService) Recover(ctx context.Context, now time.Time) (int, error) {
	return d.store.Recover(ctx, now)
}

// finish 回填终态；写失败只记 ERROR——行保持 pending，下次启动恢复会标为未知，不会
// 被静默当成已送达。
// finish writes the terminal state; a write failure leaves the row pending so the next
// startup recovery reports it as unknown instead of silently assuming success.
func (d *deliveryService) finish(ctx context.Context, intent core.DeliveryIntent,
	outcome core.DeliveryOutcome, startedAt time.Time,
) {
	if err := d.store.Finish(ctx, intent.RunID, outcome.Status, outcome.HTTPStatus,
		outcome.ReceiptCode, outcome.Detail, startedAt, d.now()); err != nil {
		d.logger.Error("投递结果写库失败（记录保持 pending，启动恢复标为未知）",
			"task_id", intent.TaskID, "run_id", intent.RunID,
			"status", string(outcome.Status), "error", err)
	}
}

// agentUID 从生命周期状态取稳定身份：资格判定只认 UID，不认名称。状态缺失 / 损坏
// 一律失败关闭——本轮不发起运行，由调度器把执行行收尾为失败。
// agentUID resolves the stable identity from the lifecycle record and fails closed.
func (d *deliveryService) agentUID(ctx context.Context, agent string) (core.AgentUID, error) {
	if d.lifecycle == nil {
		return "", fmt.Errorf("装配错误：Agent %s 未接入生命周期状态端口，无法复核代投资格", agent)
	}
	record, exists, err := d.lifecycle.Read(ctx, agent)
	if err != nil {
		return "", fmt.Errorf("读取 Agent %s 的生命周期状态失败: %w", agent, err)
	}
	if !exists {
		return "", fmt.Errorf("生命周期状态缺失：Agent %s 无法复核代投资格", agent)
	}
	return record.AgentUID, nil
}
