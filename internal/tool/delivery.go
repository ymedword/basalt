package tool

// 本文件是定时任务**系统代投**的发送面（US-12 R7）：按渠道资源 UID 选定渠道、
// 复核使用范围、每次发送重新解析凭据、经策略判定后调用同一套适配器，并把结果
// 三分（sent / failed / unknown）。它与 `notify` 工具共用适配器与 env 模式解析，
// **不**共用工具授权面——系统代投不经过模型，也不伪造一次工具调用。
// This file is the system-delivery sender for scheduled tasks: channel selection by
// resource uid, eligibility re-check, per-send credential resolution, policy verdict,
// and a three-valued outcome. It shares adapters and the env-mode resolver with the
// notify tool but never impersonates a model tool call.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"syscall"

	"github.com/ymedword/basalt/internal/core"
)

// DeliverySender 是系统代投的发送实现：装配层只构造一次，每次发送重新读渠道表、
// 重新解析凭据——撤销 / 删除 / 改范围立即生效。
// DeliverySender resolves and sends one system delivery; the registry and the credential
// are re-read per send, so a revoked or re-scoped channel takes effect immediately.
type DeliverySender struct {
	channels    core.NotifyChannelReader
	policy      core.Policy
	adapters    map[string]NotifyAdapter
	credentials NotifyCredentialResolver
	lookupEnv   func(string) (string, bool)
}

// NewDeliverySender 克隆适配器映射；lookupEnv 缺省 `os.LookupEnv`。`credentials` 可为
// nil：此时任何 `managed` 渠道都失败关闭（与 NotifyTool 同一口径）。
// NewDeliverySender clones the adapter map and defaults the env lookup to os.LookupEnv.
func NewDeliverySender(channels core.NotifyChannelReader, policy core.Policy,
	adapters map[string]NotifyAdapter, credentials NotifyCredentialResolver,
) *DeliverySender {
	cloned := make(map[string]NotifyAdapter, len(adapters))
	maps.Copy(cloned, adapters)
	return &DeliverySender{
		channels:    channels,
		policy:      policy,
		adapters:    cloned,
		credentials: credentials,
		lookupEnv:   os.LookupEnv,
	}
}

// Lookup 按**资源 UID** 定位渠道并复核资格（存在、未删除、本 Agent 可用、类型已
// 装配）。结果在运行开始前固定，运行内不按名称重新解析。
// Lookup resolves a channel by uid and re-checks eligibility; the target is frozen before
// the run starts.
func (s *DeliverySender) Lookup(ctx context.Context, channelUID core.ResourceUID,
	agentUID core.AgentUID,
) (core.NotifyChannel, error) {
	channels, err := s.channels.ListNotifyChannels(ctx)
	if err != nil {
		return core.NotifyChannel{}, fmt.Errorf("读取通知渠道注册表失败: %w", err)
	}
	for _, channel := range channels {
		if channel.ResourceUID != channelUID {
			continue
		}
		if channel.DeletedAt != nil {
			return core.NotifyChannel{}, fmt.Errorf("通知渠道 %s 已删除", channel.Name)
		}
		if !core.Eligible(channel.Scope, channel.OwnerAgentUID, agentUID) {
			return core.NotifyChannel{}, fmt.Errorf(
				"通知渠道 %s 已不在本 Agent 的可用范围内（使用范围已变更）", channel.Name)
		}
		if _, ok := s.adapters[channel.Type]; !ok {
			return core.NotifyChannel{}, fmt.Errorf(
				"通知渠道 %s 的类型 %s 未装配", channel.Name, channel.Type)
		}
		return channel, nil
	}
	return core.NotifyChannel{}, errors.New("代投目标渠道不存在（可能已被删除）")
}

// Deliver 执行一次发送并返回三分结果。任何前置失败（渠道类型、凭据、策略）都是**确定
// 失败**——请求没有发出；传输与回执层的歧义才记 unknown，**不得**盲目重发。
// Deliver returns the three-valued outcome; preflight failures are definite failures
// because no request was sent, and ambiguous transport outcomes are unknown.
func (s *DeliverySender) Deliver(ctx context.Context, channel core.NotifyChannel,
	agentName string, content string,
) core.DeliveryOutcome {
	display := notifyDisplayURL(channelDisplayTarget(channel))

	adapter := s.adapters[channel.Type]
	if adapter == nil {
		return failedOutcome(0, fmt.Sprintf("渠道类型 %s 未装配：%s", channel.Type, display))
	}
	resolution, err := s.resolveTarget(ctx, channel)
	if err != nil {
		return failedOutcome(0, redactedDeliveryError(err, display))
	}
	if policyErr := s.policy.Check(ctx, core.Action{
		Type:   core.ActionHTTPRequest,
		Target: resolution.Target,
		Agent:  agentName,
	}); policyErr != nil {
		return failedOutcome(0,
			redactedDeliveryError(redactNotifyPolicyError(policyErr, resolution.Target), display))
	}
	if sender, ok := adapter.(NotifyResultSender); ok {
		return sender.SendResult(ctx, resolution.Target, content)
	}
	// 兜底：只实现 Send 的适配器（测试夹具）按二值结果映射——成功即 sent，错误按 ctx
	// 身份判 unknown，其余按确定失败。
	// Fallback for send-only adapters: success is sent, a context error is unknown, and
	// anything else is a definite failure.
	if err := adapter.Send(ctx, resolution.Target, content); err != nil {
		if ctx.Err() != nil {
			return core.DeliveryOutcome{
				Status: core.DeliveryUnknown,
				Detail: "投递请求超时或已取消，结果未知：" + display,
			}
		}
		return failedOutcome(0, "投递失败："+display)
	}
	return core.DeliveryOutcome{Status: core.DeliverySent, Detail: "已发送：" + display}
}

// resolveTarget 解析本次发送的真实地址：env 模式与工具共用解析原语；managed 模式走
// 凭据解析器取当前 active 版本。撤销在代投侧只回固定短语——这里没有模型可提示，
// 记录与页面需要的是可核对的原因。
// resolveTarget resolves the outbound address for this send only.
func (s *DeliverySender) resolveTarget(ctx context.Context,
	channel core.NotifyChannel,
) (core.NotifyCredentialResolution, error) {
	if channel.CredentialMode != core.CredentialModeManaged {
		return resolveEnvNotifyTarget(channel, s.lookupEnv)
	}
	if s.credentials == nil {
		return core.NotifyCredentialResolution{}, fmt.Errorf(
			"通知渠道 %s 使用托管凭据，但本实例未装配凭据解析器（装配错误）", channel.Name)
	}
	resolution, err := s.credentials.ResolveNotifyCredential(ctx, channel)
	if err != nil {
		if errors.Is(err, core.ErrCredentialRevoked) {
			return core.NotifyCredentialResolution{}, fmt.Errorf(
				"通知渠道 %s 的托管凭据已撤销（notify_disabled），请在管理台重新写入后重试",
				channel.Name)
		}
		return core.NotifyCredentialResolution{}, fmt.Errorf(
			"解析通知渠道 %s 的托管凭据失败: %w", channel.Name, err)
	}
	if resolution.Target == "" {
		return core.NotifyCredentialResolution{}, fmt.Errorf(
			"通知渠道 %s 的托管凭据未给出可用地址（凭据不可用）", channel.Name)
	}
	return resolution, nil
}

// NotifyResultSender 是适配器可选的**详细结果**面：系统代投需要区分 sent / failed /
// unknown 与 HTTP 状态 / 飞书回执码。`Send` 的既有语义不变（错误即失败），因此工具
// 路径与代投路径共享同一套发送实现而不互相改义。
// NotifyResultSender is the optional detailed-result face used by system delivery; the
// existing Send contract is unchanged.
type NotifyResultSender interface {
	SendResult(ctx context.Context, rawURL, content string) core.DeliveryOutcome
}

// channelDisplayTarget 给出错误文案里的脱敏占位：managed 渠道的 URL 为空（返回空串，
// 由 notifyDisplayURL 落到「目标无法解析」占位），env 渠道给它的 `${VAR}` 形态。
// channelDisplayTarget renders the redaction-safe placeholder of one channel.
func channelDisplayTarget(channel core.NotifyChannel) string {
	if channel.CredentialMode == core.CredentialModeManaged {
		return ""
	}
	return channel.URL
}

// redactedDeliveryError 把解析 / 策略错误压成固定短语：只保留占位后的目标，绝不带
// 路径、查询串、环境变量值或凭据。
// redactedDeliveryError compresses a preflight error into fixed wording.
func redactedDeliveryError(err error, display string) string {
	var violation *core.SandboxViolationError
	if errors.As(err, &violation) {
		return "策略拒绝：" + notifyDisplayURL(violation.Target)
	}
	if display == targetUnresolvable {
		return "投递前置检查失败：" + err.Error()
	}
	return fmt.Sprintf("投递前置检查失败：%s（%s）", err.Error(), display)
}

// failedOutcome 构造一次确定失败（请求没有发出，或服务端明确拒绝）。
// failedOutcome builds a definite failure.
func failedOutcome(httpStatus int, detail string) core.DeliveryOutcome {
	return core.DeliveryOutcome{Status: core.DeliveryFailed, HTTPStatus: httpStatus, Detail: detail}
}

// sentOutcome 构造一次成功（服务端已确认受理）。
// sentOutcome builds a confirmed success.
func sentOutcome(httpStatus int, detail string) core.DeliveryOutcome {
	return core.DeliveryOutcome{Status: core.DeliverySent, HTTPStatus: httpStatus, Detail: detail}
}

// unknownOutcome 构造一次结果未知：可能已送达，**不得**据它自动重发。
// unknownOutcome builds an ambiguous result that must never trigger a resend.
func unknownOutcome(detail string) core.DeliveryOutcome {
	return core.DeliveryOutcome{Status: core.DeliveryUnknown, Detail: detail}
}

// classifyDeliveryTransport 把传输层错误分成确定失败与结果未知：连接被拒 / 域名解析
// 失败说明请求没有送达（确定失败）；超时、取消与其它中断可能已送达，一律 unknown。
// classifyDeliveryTransport separates definite failures from ambiguous transport errors.
func classifyDeliveryTransport(ctx context.Context, err error, display string) core.DeliveryOutcome {
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) {
		return unknownOutcome("投递请求超时或已取消，结果未知：" + display)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return unknownOutcome("投递请求超时，结果未知：" + display)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return failedOutcome(0, "投递目标拒绝连接："+display)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return failedOutcome(0, "投递目标域名解析失败："+display)
	}
	return unknownOutcome("投递连接中断，结果未知：" + display)
}
