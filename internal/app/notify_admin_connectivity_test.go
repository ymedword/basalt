package app

// 本文件是通知渠道「测试连接」的判卷（US-12 R7 范围修订，check
// `012-app-notify-author`）：先写操作行再发送、固定文案、三分结论（success / failed /
// unknown）、测试失败不得看起来像已验证、未配置地址 503、结果未知不自动重发、
// 装配缺发送面时不伪造一次测试。
// These tests pin the notify connectivity test: pre-written operation row, the fixed
// text, three-valued outcome, failed-is-not-verified, and no blind resend.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// withConnectivity 在基础夹具上补发送面与最后测试投影，并重建编排实例。
// withConnectivity adds the send face and the projection store, then rebuilds the admin.
func withConnectivity(t *testing.T, sender NotifyTestSender,
	mutate ...func(*NotifyAdminDeps),
) *notifyEnv {
	t.Helper()
	env := newNotifyEnv(t)
	env.deps.TestSender = sender
	env.deps.ConnectionTests = storage.NewConnectionTestStore(env.db)
	for _, m := range mutate {
		m(&env.deps)
	}
	env.admin = NewNotifyAdmin(env.deps)
	return env
}

// fakeNotifyTestSender 记录发送调用并按预设返回终态；block=true 时挂到超时，模拟
// "请求可能已发出、但等不到结论"。
// fakeNotifyTestSender records each send and returns a preset outcome; block waits for
// the deadline to simulate an unknown result.
type fakeNotifyTestSender struct {
	outcome core.DeliveryOutcome
	block   bool

	calls   int
	content string
	agent   string
	channel core.NotifyChannel
}

// Deliver 实现 NotifyTestSender：第一次调用只记录、不重试。
// Deliver implements NotifyTestSender and never retries on its own.
func (f *fakeNotifyTestSender) Deliver(ctx context.Context, channel core.NotifyChannel,
	agentName, content string,
) core.DeliveryOutcome {
	f.calls++
	f.channel = channel
	f.agent = agentName
	f.content = content
	if f.block {
		<-ctx.Done()
	}
	return f.outcome
}

// failingConnectionTestStore 让最后结果的投影写入失败：权威记录仍是操作台账，测试
// 结论本身不得因投影失败被改写。
// failingConnectionTestStore fails the last-test projection only.
type failingConnectionTestStore struct{}

func (failingConnectionTestStore) RecordConnectionTest(context.Context, core.ResourceConnectionTest) error {
	return errors.New("projection unavailable")
}

func (failingConnectionTestStore) ListConnectionTests(context.Context,
	core.ResourceKind,
) ([]core.ResourceConnectionTest, error) {
	return nil, errors.New("projection unavailable")
}

// lastTest 读一条渠道的最后测试投影（不存在时返回零值与 false）。
// lastTest returns the stored projection row for one channel uid.
func lastTest(t *testing.T, db *storage.DB, uid core.ResourceUID) (core.ResourceConnectionTest, bool) {
	t.Helper()
	rows, err := storage.NewConnectionTestStore(db).ListConnectionTests(
		context.Background(), core.ResourceNotifyChannel)
	require.NoError(t, err)
	for _, row := range rows {
		if row.ResourceUID == uid {
			return row, true
		}
	}
	return core.ResourceConnectionTest{}, false
}

// TestNotifyAdminTestChannel_Success：发送面确认受理 → 操作 success、结论 success、
// 投影写 success；发送面收到固定文案 `Basalt 连通性测试`，且不冒充任何 Agent 的名义。
// 中文验收点：测试成功有据可查，文案固定。
func TestNotifyAdminTestChannel_Success(t *testing.T) {
	sender := &fakeNotifyTestSender{outcome: core.DeliveryOutcome{
		Status: core.DeliverySent, HTTPStatus: 200, Detail: "通知渠道测试消息已发送",
	}}
	env := withConnectivity(t, sender)
	channel, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)

	result, err := env.admin.TestChannel(context.Background(), resourceActor(), "ops")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Equal(t, core.ConnectionTestSuccess, result.Status)
	require.Equal(t, "通知渠道测试消息已发送", result.Detail)

	require.Equal(t, 1, sender.calls, "一次测试只发一次")
	require.Equal(t, core.NotifyConnectivityTestText, sender.content)
	require.Equal(t, "Basalt 连通性测试", sender.content)
	require.Empty(t, sender.agent, "测试不是任何 Agent 的运行，不得带 Agent 名义")
	require.Equal(t, channel.ResourceUID, sender.channel.ResourceUID)

	records := env.rows(core.ActionNotifyChannelTest)
	require.Len(t, records, 1)
	require.Equal(t, core.OperationSuccess, records[0].Status)
	require.Equal(t, core.TargetKindNotifyChannel, records[0].TargetKind)
	require.Equal(t, "ops", records[0].TargetID)

	projected, ok := lastTest(t, env.db, channel.ResourceUID)
	require.True(t, ok)
	require.Equal(t, core.ConnectionTestSuccess, projected.Status)
	require.Equal(t, records[0].OperationID, projected.OperationID)
	require.Equal(t, baselineClock().UTC(), projected.TestedAt)
}

// TestNotifyAdminTestChannel_FailedIsNotVerified：对端明确拒绝 → 操作 success（尝试
// 完成）但结论 failed、`OK=false`；投影同样记 failed——失败不得看起来像已验证。
// 中文验收点：失败就是失败，不给"已验证"的样子。
func TestNotifyAdminTestChannel_FailedIsNotVerified(t *testing.T) {
	receipt := 19001
	sender := &fakeNotifyTestSender{outcome: core.DeliveryOutcome{
		Status: core.DeliveryFailed, HTTPStatus: 200, ReceiptCode: &receipt,
		Detail: "飞书回执 code=19001",
	}}
	env := withConnectivity(t, sender)
	channel, err := env.create("feishu", "feishu", "${BASALT_TEST_FEISHU_HOOK}", "")
	require.NoError(t, err)

	result, err := env.admin.TestChannel(context.Background(), resourceActor(), "feishu")
	require.NoError(t, err, "确定的失败不是请求错误——端点按 200 返回结论")
	require.False(t, result.OK)
	require.Equal(t, core.ConnectionTestFailed, result.Status)
	require.Equal(t, "飞书回执 code=19001", result.Detail)

	records := env.rows(core.ActionNotifyChannelTest)
	require.Len(t, records, 1)
	require.Equal(t, core.OperationSuccess, records[0].Status,
		"尝试确实完成，操作终态是 success；失败结论在 detail_json 里")
	require.NotEmpty(t, records[0].DetailJSON)
	require.Contains(t, string(records[0].DetailJSON), core.ConnectionTestFailed)

	projected, ok := lastTest(t, env.db, channel.ResourceUID)
	require.True(t, ok)
	require.Equal(t, core.ConnectionTestFailed, projected.Status)
	require.Equal(t, "飞书回执 code=19001", projected.Detail)
}

// TestNotifyAdminTestChannel_UnknownNeverResent：等待到截止仍无结论 → 结论 unknown、
// 操作 unknown（TIMEOUT），发送面**只被调用一次**，投影记 unknown。
// 中文验收点：结果不明时不重发，也不写成失败。
func TestNotifyAdminTestChannel_UnknownNeverResent(t *testing.T) {
	sender := &fakeNotifyTestSender{
		block:   true,
		outcome: core.DeliveryOutcome{Status: core.DeliveryUnknown, Detail: "等待回执超时"},
	}
	env := withConnectivity(t, sender, func(deps *NotifyAdminDeps) {
		deps.TestTimeout = 20 * time.Millisecond
	})
	channel, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)

	result, err := env.admin.TestChannel(context.Background(), resourceActor(), "ops")
	require.NoError(t, err)
	require.False(t, result.OK)
	require.Equal(t, core.ConnectionTestUnknown, result.Status)
	require.Equal(t, 1, sender.calls, "结果未知时不得自动重发")

	records := env.rows(core.ActionNotifyChannelTest)
	require.Len(t, records, 1)
	require.Equal(t, core.OperationUnknown, records[0].Status)
	require.Equal(t, resourceResultTimeout, records[0].ResultCode)

	projected, ok := lastTest(t, env.db, channel.ResourceUID)
	require.True(t, ok)
	require.Equal(t, core.ConnectionTestUnknown, projected.Status)
}

// TestNotifyAdminTestChannel_NoAddress：从未配置地址 → 503 分类的
// `ErrProviderNotConfigured`，操作 failed + NOT_CONFIGURED，投影记 failed，**零发送**；
// 错误文案点名补录入口，不把"没配"说成"测过但失败"。
// 中文验收点：没地址时明确提示补录。
func TestNotifyAdminTestChannel_NoAddress(t *testing.T) {
	sender := &fakeNotifyTestSender{outcome: core.DeliveryOutcome{Status: core.DeliverySent}}
	env := withConnectivity(t, sender)
	channel, err := env.create("blank", "webhook", "", "")
	require.NoError(t, err)

	result, err := env.admin.TestChannel(context.Background(), resourceActor(), "blank")
	require.ErrorIs(t, err, core.ErrProviderNotConfigured)
	require.Equal(t, core.ConnectionTestFailed, result.Status)
	require.Contains(t, err.Error(), "补录地址")
	require.Zero(t, sender.calls, "没有地址就不发")

	records := env.rows(core.ActionNotifyChannelTest)
	require.Len(t, records, 1)
	require.Equal(t, core.OperationFailed, records[0].Status)
	require.Equal(t, resourceResultNotConfigured, records[0].ResultCode)

	projected, ok := lastTest(t, env.db, channel.ResourceUID)
	require.True(t, ok)
	require.Equal(t, core.ConnectionTestFailed, projected.Status)
	require.Equal(t, "渠道未配置地址", projected.Detail)
}

// TestNotifyAdminTestChannel_AssemblyGaps：缺发送面 → 503 且不写操作行、不写投影
// （不得伪造一次"已测试"）；缺投影仓储 → 测试照常执行并返回结论。
// 中文验收点：装配缺口不伪装成测试结论。
func TestNotifyAdminTestChannel_AssemblyGaps(t *testing.T) {
	t.Run("缺发送面", func(t *testing.T) {
		env := newNotifyEnv(t)
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)

		_, err = env.admin.TestChannel(context.Background(), resourceActor(), "ops")
		require.ErrorIs(t, err, core.ErrProviderNotConfigured)
		require.Empty(t, env.rows(core.ActionNotifyChannelTest),
			"缺发送面不得留下操作行，也不得伪造测试")
	})

	t.Run("缺投影仓储", func(t *testing.T) {
		sender := &fakeNotifyTestSender{outcome: core.DeliveryOutcome{Status: core.DeliverySent}}
		env := withConnectivity(t, sender, func(deps *NotifyAdminDeps) {
			deps.ConnectionTests = nil
		})
		_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
		require.NoError(t, err)

		result, err := env.admin.TestChannel(context.Background(), resourceActor(), "ops")
		require.NoError(t, err)
		require.True(t, result.OK)
		require.Len(t, env.rows(core.ActionNotifyChannelTest), 1, "历史仍在操作台账")
	})
}

// TestNotifyAdminTestChannel_MissingChannel：不存在的渠道 → 404 语义，零操作行、零发送。
// 中文验收点：测试不存在的渠道不留痕。
func TestNotifyAdminTestChannel_MissingChannel(t *testing.T) {
	sender := &fakeNotifyTestSender{outcome: core.DeliveryOutcome{Status: core.DeliverySent}}
	env := withConnectivity(t, sender)

	_, err := env.admin.TestChannel(context.Background(), resourceActor(), "ghost")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.Zero(t, sender.calls)
	require.Empty(t, env.rows(core.ActionNotifyChannelTest))
}

// TestNotifyAdminTestChannel_ProjectionFailureKeepsResult：投影写不进去只记 ERROR，
// 不把已完成的测试改写成失败（否则管理员会重发）。
// 中文验收点：投影坏了不改变结论。
func TestNotifyAdminTestChannel_ProjectionFailureKeepsResult(t *testing.T) {
	sender := &fakeNotifyTestSender{outcome: core.DeliveryOutcome{Status: core.DeliverySent}}
	env := withConnectivity(t, sender, func(deps *NotifyAdminDeps) {
		deps.ConnectionTests = failingConnectionTestStore{}
	})
	_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)

	result, err := env.admin.TestChannel(context.Background(), resourceActor(), "ops")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Len(t, env.rows(core.ActionNotifyChannelTest), 1)
	require.Contains(t, env.logs.String(), "写入通知渠道最后测试结果失败")
}

// TestNotifyAdminTestChannel_DetailIsRedacted：操作详情只含状态、耗时、回执码与固定
// 短语；**不得**出现地址占位或解析后的完整地址。
// 中文验收点：台账不泄地址。
func TestNotifyAdminTestChannel_DetailIsRedacted(t *testing.T) {
	sender := &fakeNotifyTestSender{outcome: core.DeliveryOutcome{
		Status: core.DeliverySent, HTTPStatus: 200, Detail: "https://open.feishu.cn 已受理",
	}}
	env := withConnectivity(t, sender)
	_, err := env.create("ops", "webhook", "${BASALT_TEST_OPS_HOOK}", "")
	require.NoError(t, err)

	_, err = env.admin.TestChannel(context.Background(), resourceActor(), "ops")
	require.NoError(t, err)

	records := env.rows(core.ActionNotifyChannelTest)
	require.Len(t, records, 1)
	detail := string(records[0].DetailJSON)
	require.NotContains(t, detail, "hooks.example.com/services")
	require.NotContains(t, detail, "${BASALT_TEST_OPS_HOOK}")
	require.NotContains(t, strings.ToLower(detail), "super-secret")
	require.Contains(t, detail, "latency_ms")
}
