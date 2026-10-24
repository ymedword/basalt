package tool

// 本文件覆盖系统代投发送面（US-12 R7）：按 UID 选渠道并复核资格、发送前重新解析
// 凭据、策略判定、三分结果与脱敏。适配器内部载荷 / 回执判定已有独立用例，这里只验
// 发送器如何组合它们。
// This file covers the system-delivery sender: uid lookup, eligibility, per-send
// credential resolution, policy judgement, three-valued outcomes, and redaction.

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// deliveryResultAdapter 记录发送并返回预置的三分结果；同时实现 Send（兜底面）与
// SendResult（详细面），代投路径只应调用后者。
// deliveryResultAdapter records sends and returns the configured three-valued outcome.
type deliveryResultAdapter struct {
	mu      sync.Mutex
	outcome core.DeliveryOutcome
	sends   []notifySend
}

func (a *deliveryResultAdapter) Send(_ context.Context, rawURL, content string) error {
	a.record(rawURL, content)
	return nil
}

func (a *deliveryResultAdapter) SendResult(_ context.Context,
	rawURL, content string,
) core.DeliveryOutcome {
	a.record(rawURL, content)
	return a.outcome
}

func (a *deliveryResultAdapter) record(rawURL, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sends = append(a.sends, notifySend{URL: rawURL, Content: content})
}

func (a *deliveryResultAdapter) sent() []notifySend {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]notifySend, len(a.sends))
	copy(out, a.sends)
	return out
}

// fakeDeliveryCredentials 是托管凭据解析器的替身。
// fakeDeliveryCredentials is the managed-credential resolver double.
type fakeDeliveryCredentials struct {
	resolution core.NotifyCredentialResolution
	err        error
	calls      int
}

func (f *fakeDeliveryCredentials) ResolveNotifyCredential(_ context.Context,
	_ core.NotifyChannel,
) (core.NotifyCredentialResolution, error) {
	f.calls++
	return f.resolution, f.err
}

// deliverySenderChannel 构造一个 env 模式共享飞书渠道；UID 由用例给出。
func deliverySenderChannel(uid core.ResourceUID) core.NotifyChannel {
	return core.NotifyChannel{
		ResourceUID:    uid,
		Name:           "feishu-ops",
		Type:           "feishu",
		URL:            "${BASALT_TEST_DELIVERY_HOOK}",
		URLHost:        "https://hooks.example.test",
		CredentialMode: core.CredentialModeEnv,
		Scope:          core.ScopeShared,
	}
}

// deliverySenderAllowedPolicy 放行测试目标主机；其余目标按沙箱拒绝返回。
func deliverySenderAllowedPolicy() *notifyPolicySpy {
	return &notifyPolicySpy{allowed: []string{"hooks.example.test"}}
}

// TestDeliverySender_LookupResolvesByUID —— 渠道按资源 UID 命中，身份与名称随结果返回。
// Lookup resolves the channel by resource uid.
func TestDeliverySender_LookupResolvesByUID(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		deliverySenderChannel("rs_ops"),
	}}
	sender := NewDeliverySender(channels, deliverySenderAllowedPolicy(),
		map[string]NotifyAdapter{"feishu": &deliveryResultAdapter{}}, nil)

	channel, err := sender.Lookup(context.Background(), "rs_ops", "au_writer")
	require.NoError(t, err)
	require.Equal(t, "feishu-ops", channel.Name)
	require.Equal(t, core.ResourceUID("rs_ops"), channel.ResourceUID)
	require.Equal(t, 1, channels.readCount(), "每次查找都重新读注册表，不缓存")
}

// TestDeliverySender_LookupRejects —— 删除 / 越出使用范围 / 类型未装配 / 不存在 /
// 注册表读失败都拒绝。
// Lookup rejects deleted, out-of-scope, unregistered-type, missing channels and a
// registry read failure.
func TestDeliverySender_LookupRejects(t *testing.T) {
	deletedAt := core.NotifyChannel{ResourceUID: "rs_ops", Name: "feishu-ops", Type: "feishu"}
	now := core.NotifyChannel{}
	_ = now
	deletedAt.DeletedAt = new(int64)
	*deletedAt.DeletedAt = 1

	exclusive := deliverySenderChannel("rs_ops")
	exclusive.Scope = core.ScopeExclusive
	exclusive.OwnerAgentUID = "au_other"

	untyped := deliverySenderChannel("rs_ops")
	untyped.Type = "sms"

	cases := []struct {
		name     string
		channels *fakeNotifyChannels
		uid      core.ResourceUID
		want     string
	}{
		{
			name:     "deleted",
			channels: &fakeNotifyChannels{rows: []core.NotifyChannel{deletedAt}},
			uid:      "rs_ops",
			want:     "已删除",
		},
		{
			name:     "out of scope",
			channels: &fakeNotifyChannels{rows: []core.NotifyChannel{exclusive}},
			uid:      "rs_ops",
			want:     "可用范围",
		},
		{
			name:     "adapter not assembled",
			channels: &fakeNotifyChannels{rows: []core.NotifyChannel{untyped}},
			uid:      "rs_ops",
			want:     "未装配",
		},
		{
			name:     "missing uid",
			channels: &fakeNotifyChannels{rows: []core.NotifyChannel{deliverySenderChannel("rs_ops")}},
			uid:      "rs_gone",
			want:     "不存在",
		},
		{
			name:     "registry error",
			channels: &fakeNotifyChannels{err: errors.New("registry down")},
			uid:      "rs_ops",
			want:     "读取通知渠道注册表失败",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := NewDeliverySender(tc.channels, deliverySenderAllowedPolicy(),
				map[string]NotifyAdapter{"feishu": &deliveryResultAdapter{}}, nil)
			_, err := sender.Lookup(context.Background(), tc.uid, "au_writer")
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestDeliverySender_DeliverUsesResultSender —— 代投走详细结果面：解析后的真实地址、
// 原文内容、策略判定带 Agent 名，返回值原样带回。
// Deliver uses the detailed-result face and judges the action with the agent name.
func TestDeliverySender_DeliverUsesResultSender(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK",
		"https://hooks.example.test/open-apis/bot/v2/hook/secret-token")
	receipt := 0
	adapter := &deliveryResultAdapter{outcome: core.DeliveryOutcome{
		Status: core.DeliverySent, HTTPStatus: 200, ReceiptCode: &receipt, Detail: "飞书已受理",
	}}
	policy := deliverySenderAllowedPolicy()
	sender := NewDeliverySender(&fakeNotifyChannels{rows: []core.NotifyChannel{
		deliverySenderChannel("rs_ops"),
	}}, policy, map[string]NotifyAdapter{"feishu": adapter}, nil)

	channel, err := sender.Lookup(context.Background(), "rs_ops", "au_writer")
	require.NoError(t, err)
	outcome := sender.Deliver(context.Background(), channel, "writer", "日报正文")

	require.Equal(t, core.DeliverySent, outcome.Status)
	require.Equal(t, 200, outcome.HTTPStatus)
	require.NotNil(t, outcome.ReceiptCode)
	require.Equal(t, []notifySend{{
		URL:     "https://hooks.example.test/open-apis/bot/v2/hook/secret-token",
		Content: "日报正文",
	}}, adapter.sent())
	require.Equal(t, []string{
		"https://hooks.example.test/open-apis/bot/v2/hook/secret-token",
	}, policy.targets())
	require.Equal(t, []core.ActionType{core.ActionHTTPRequest}, policy.types())
	require.Equal(t, []string{"writer"}, policy.agents())
}

// TestDeliverySender_DeliverEnvPlaceholderUnsetFailsClosed —— 引用的环境变量未设置时
// 零外呼、确定失败；失败文案只带变量名，不带任何值。
// An unset env placeholder fails before any outbound request and never echoes a value.
func TestDeliverySender_DeliverEnvPlaceholderUnsetFailsClosed(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "")
	adapter := &deliveryResultAdapter{}
	policy := deliverySenderAllowedPolicy()
	sender := NewDeliverySender(&fakeNotifyChannels{rows: []core.NotifyChannel{
		deliverySenderChannel("rs_ops"),
	}}, policy, map[string]NotifyAdapter{"feishu": adapter}, nil)

	channel, err := sender.Lookup(context.Background(), "rs_ops", "au_writer")
	require.NoError(t, err)
	outcome := sender.Deliver(context.Background(), channel, "writer", "正文")

	require.Equal(t, core.DeliveryFailed, outcome.Status)
	require.Empty(t, adapter.sent(), "前置失败不得发出请求")
	require.Empty(t, policy.targets(), "前置失败不做策略判定")
	require.Contains(t, outcome.Detail, "BASALT_TEST_DELIVERY_HOOK")
}

// TestDeliverySender_DeliverRedactsPolicyTarget —— 策略拒绝时文案压到 scheme://host，
// 路径 / 查询串 / 令牌不外泄；请求零发送。
// A policy refusal keeps only scheme://host in the detail and sends nothing.
func TestDeliverySender_DeliverRedactsPolicyTarget(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK",
		"https://hooks.example.test/open-apis/bot/v2/hook/secret-token?key=abc")
	adapter := &deliveryResultAdapter{}
	policy := &notifyPolicySpy{allowed: nil}
	sender := NewDeliverySender(&fakeNotifyChannels{rows: []core.NotifyChannel{
		deliverySenderChannel("rs_ops"),
	}}, policy, map[string]NotifyAdapter{"feishu": adapter}, nil)

	channel, err := sender.Lookup(context.Background(), "rs_ops", "au_writer")
	require.NoError(t, err)
	outcome := sender.Deliver(context.Background(), channel, "writer", "正文")

	require.Equal(t, core.DeliveryFailed, outcome.Status)
	require.Contains(t, outcome.Detail, "策略拒绝")
	require.Contains(t, outcome.Detail, "https://hooks.example.test")
	require.NotContains(t, outcome.Detail, "secret-token")
	require.NotContains(t, outcome.Detail, "/open-apis/")
	require.NotContains(t, outcome.Detail, "key=abc")
	require.Empty(t, adapter.sent())
}

// TestDeliverySender_DeliverManagedFailsClosed —— 托管渠道：解析器缺失 / 凭据已撤销 /
// 解析不出地址，一律在发送前失败关闭。
// Managed channels fail closed before sending when the resolver is missing, the
// credential is revoked, or no target is resolved.
func TestDeliverySender_DeliverManagedFailsClosed(t *testing.T) {
	managed := deliverySenderChannel("rs_ops")
	managed.CredentialMode = core.CredentialModeManaged
	managed.URL = ""
	managed.CredentialID = "cred_1"
	adapter := &deliveryResultAdapter{}

	cases := []struct {
		name    string
		creds   NotifyCredentialResolver
		want    string
		adapter bool
	}{
		{name: "no resolver", creds: nil, want: "未装配凭据解析器"},
		{
			name:  "revoked",
			creds: &fakeDeliveryCredentials{err: core.ErrCredentialRevoked},
			want:  "已撤销",
		},
		{
			name:  "empty target",
			creds: &fakeDeliveryCredentials{},
			want:  "未给出可用地址",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sender := NewDeliverySender(&fakeNotifyChannels{rows: []core.NotifyChannel{managed}},
				deliverySenderAllowedPolicy(), map[string]NotifyAdapter{"feishu": adapter}, tc.creds)
			outcome := sender.Deliver(context.Background(), managed, "writer", "正文")
			require.Equal(t, core.DeliveryFailed, outcome.Status)
			require.Contains(t, outcome.Detail, tc.want)
			require.Empty(t, adapter.sent())
		})
	}
}

// TestDeliverySender_DeliverFallsBackToSend —— 只实现 Send 的适配器按二值映射：
// 成功 sent、错误 failed；上下文已取消时按 unknown，不盲目重发。
// A send-only adapter maps success to sent, an error to failed, and a canceled context
// to unknown.
func TestDeliverySender_DeliverFallsBackToSend(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://hooks.example.test/hook")
	channel := deliverySenderChannel("rs_ops")

	t.Run("success", func(t *testing.T) {
		adapter := &recordingNotifyAdapter{}
		sender := NewDeliverySender(&fakeNotifyChannels{rows: []core.NotifyChannel{channel}},
			deliverySenderAllowedPolicy(), map[string]NotifyAdapter{"feishu": adapter}, nil)
		outcome := sender.Deliver(context.Background(), channel, "writer", "正文")
		require.Equal(t, core.DeliverySent, outcome.Status)
		require.Equal(t, 1, adapter.count())
	})

	t.Run("failure", func(t *testing.T) {
		adapter := &recordingNotifyAdapter{err: errors.New("boom")}
		sender := NewDeliverySender(&fakeNotifyChannels{rows: []core.NotifyChannel{channel}},
			deliverySenderAllowedPolicy(), map[string]NotifyAdapter{"feishu": adapter}, nil)
		outcome := sender.Deliver(context.Background(), channel, "writer", "正文")
		require.Equal(t, core.DeliveryFailed, outcome.Status)
	})

	t.Run("canceled context stays unknown", func(t *testing.T) {
		adapter := &recordingNotifyAdapter{err: context.Canceled}
		sender := NewDeliverySender(&fakeNotifyChannels{rows: []core.NotifyChannel{channel}},
			deliverySenderAllowedPolicy(), map[string]NotifyAdapter{"feishu": adapter}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		outcome := sender.Deliver(ctx, channel, "writer", "正文")
		require.Equal(t, core.DeliveryUnknown, outcome.Status)
	})
}

// TestDeliverySender_ManagedResolverTargetUsed —— 托管解析出的地址只用于本次发送，
// 失败文案同样只保留 host。
// The managed target is used for this send only.
func TestDeliverySender_ManagedResolverTargetUsed(t *testing.T) {
	managed := deliverySenderChannel("rs_ops")
	managed.CredentialMode = core.CredentialModeManaged
	managed.URL = ""
	managed.CredentialID = "cred_1"
	adapter := &deliveryResultAdapter{outcome: core.DeliveryOutcome{Status: core.DeliverySent}}
	creds := &fakeDeliveryCredentials{resolution: core.NotifyCredentialResolution{
		Target: "https://hooks.example.test/hook/xyz", CredentialID: "cred_1", CredentialVersion: 3,
	}}
	sender := NewDeliverySender(&fakeNotifyChannels{rows: []core.NotifyChannel{managed}},
		deliverySenderAllowedPolicy(), map[string]NotifyAdapter{"feishu": adapter}, creds)

	outcome := sender.Deliver(context.Background(), managed, "writer", "正文")

	require.Equal(t, core.DeliverySent, outcome.Status)
	require.Equal(t, 1, creds.calls)
	require.Equal(t, []notifySend{{URL: "https://hooks.example.test/hook/xyz", Content: "正文"}},
		adapter.sent())
}

// timeoutNetError 只实现 net.Error 的超时面，用于分类表。
type timeoutNetError struct{}

func (timeoutNetError) Error() string   { return "i/o timeout" }
func (timeoutNetError) Timeout() bool   { return true }
func (timeoutNetError) Temporary() bool { return true }

// TestClassifyDeliveryTransport —— 传输错误三分：拒绝连接 / 域名解析失败是确定失败；
// 超时与其它中断是未知，**不得**据此重发。
// Transport errors are three-valued: refusal and DNS failure are definite; timeouts and
// other interruptions are unknown.
func TestClassifyDeliveryTransport(t *testing.T) {
	display := "https://hooks.example.test"
	cases := []struct {
		name string
		err  error
		want core.DeliveryStatus
		text string
	}{
		{
			name: "connection refused", err: syscall.ECONNREFUSED,
			want: core.DeliveryFailed, text: "拒绝连接",
		},
		{
			name: "dns failure", err: &net.DNSError{Err: "no such host", Name: "hooks.example.test"},
			want: core.DeliveryFailed, text: "域名解析失败",
		},
		{
			name: "timeout", err: timeoutNetError{},
			want: core.DeliveryUnknown, text: "结果未知",
		},
		{
			name: "other interruption", err: errors.New("connection reset"),
			want: core.DeliveryUnknown, text: "结果未知",
		},
		{
			name: "deadline", err: context.DeadlineExceeded,
			want: core.DeliveryUnknown, text: "结果未知",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome := classifyDeliveryTransport(context.Background(), tc.err, display)
			require.Equal(t, tc.want, outcome.Status)
			require.Contains(t, outcome.Detail, tc.text)
			require.True(t, strings.Contains(outcome.Detail, display), "只展示 scheme://host")
		})
	}

	t.Run("canceled ctx wins over transport shape", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		outcome := classifyDeliveryTransport(ctx, syscall.ECONNREFUSED, display)
		require.Equal(t, core.DeliveryUnknown, outcome.Status)
	})
}
