package app

// 本文件覆盖系统代投服务（US-12 R7）：预写 pending 行、运行前资格与**声明边界**复核、
// 三分结果回填、跳过、崩溃恢复。用真实 `tool.DeliverySender` + 假渠道目录 / 假适配器，
// 只替换存储与生命周期状态——断言的是服务的行为，不是发送器的内部实现。
// This file covers the system-delivery service: prewritten pending row, run-time
// eligibility and declared-channel checks, three-valued completion, skip, and recovery.

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// fakeDeliveryStore 是 `schedule_deliveries` 的内存替身：行只增不删，Finish 只改终态。
// fakeDeliveryStore is the in-memory delivery store double.
type fakeDeliveryStore struct {
	mu           sync.Mutex
	rows         map[string]core.ScheduleDeliveryRecord
	prepareErr   error
	getErr       error
	finishErr    error
	recoverCount int
	recoverErr   error
}

func newFakeDeliveryStore() *fakeDeliveryStore {
	return &fakeDeliveryStore{rows: map[string]core.ScheduleDeliveryRecord{}}
}

func (s *fakeDeliveryStore) Prepare(_ context.Context, record core.ScheduleDeliveryRecord) error {
	if s.prepareErr != nil {
		return s.prepareErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[record.RunID]; ok {
		return nil // 契约：同一 run_id 重复预写是幂等空操作
	}
	s.rows[record.RunID] = record
	return nil
}

func (s *fakeDeliveryStore) Finish(_ context.Context, runID string, status core.DeliveryStatus,
	httpStatus int, receiptCode *int, detail string, attemptedAt, finishedAt time.Time,
) error {
	if s.finishErr != nil {
		return s.finishErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[runID]
	if !ok {
		return fs.ErrNotExist
	}
	row.Status = status
	row.HTTPStatus = httpStatus
	row.ReceiptCode = receiptCode
	row.Detail = detail
	row.AttemptedAt = attemptedAt
	row.FinishedAt = finishedAt
	s.rows[runID] = row
	return nil
}

func (s *fakeDeliveryStore) Get(_ context.Context, runID string) (core.ScheduleDeliveryRecord, error) {
	if s.getErr != nil {
		return core.ScheduleDeliveryRecord{}, s.getErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[runID]
	if !ok {
		return core.ScheduleDeliveryRecord{}, fs.ErrNotExist
	}
	return row, nil
}

func (s *fakeDeliveryStore) ListByTask(_ context.Context, _ string,
	_ int,
) ([]core.ScheduleDeliveryRecord, error) {
	return nil, nil
}

func (s *fakeDeliveryStore) Recover(_ context.Context, _ time.Time) (int, error) {
	return s.recoverCount, s.recoverErr
}

// fakeDeliveryLifecycle 只实现 Read；其余方法不应被投递服务调用。
type fakeDeliveryLifecycle struct {
	record core.LifecycleRecord
	exists bool
	err    error
}

func (f *fakeDeliveryLifecycle) Read(_ context.Context,
	_ string,
) (core.LifecycleRecord, bool, error) {
	return f.record, f.exists, f.err
}

func (f *fakeDeliveryLifecycle) Transition(_ context.Context, _ string, _ core.AgentState,
	_ int64, _ core.AgentState, _ string,
) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errors.New("not used by delivery service")
}

func (f *fakeDeliveryLifecycle) Bootstrap(_ context.Context, _, _ string,
) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errors.New("not used by delivery service")
}

func (f *fakeDeliveryLifecycle) BootstrapIdentity(_ context.Context, _ string,
	_ core.AgentUID, _ string,
) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errors.New("not used by delivery service")
}

func (f *fakeDeliveryLifecycle) Discard(_ context.Context, _, _ string) error {
	return errors.New("not used by delivery service")
}

// fakeDeliveryProfiles 是注册表只读替身：按名给出当前装载条目。
type fakeDeliveryProfiles map[string]core.AgentEntry

func (f fakeDeliveryProfiles) Lookup(name string) (core.AgentEntry, bool) {
	entry, ok := f[name]
	return entry, ok
}

// fakeDeliveryAdapter 同时实现 Send 与 SendResult：代投路径只应走详细结果面。
type fakeDeliveryAdapter struct {
	outcome  core.DeliveryOutcome
	calls    int
	targets  []string
	contents []string
}

func (a *fakeDeliveryAdapter) Send(_ context.Context, rawURL, content string) error {
	a.calls++
	a.targets = append(a.targets, rawURL)
	a.contents = append(a.contents, content)
	return nil
}

func (a *fakeDeliveryAdapter) SendResult(_ context.Context, rawURL, content string) core.DeliveryOutcome {
	a.calls++
	a.targets = append(a.targets, rawURL)
	a.contents = append(a.contents, content)
	return a.outcome
}

// deliveryTestChannel 构造一个 env 模式的共享飞书渠道。
func deliveryTestChannel(uid core.ResourceUID, name string) core.NotifyChannel {
	return core.NotifyChannel{
		ResourceUID:    uid,
		Name:           name,
		Type:           "feishu",
		URL:            "${BASALT_TEST_DELIVERY_HOOK}",
		URLHost:        "https://example.test",
		CredentialMode: core.CredentialModeEnv,
		Scope:          core.ScopeShared,
	}
}

// newDeliveryTestService 装配一个可用的投递服务；profiles 为 nil 时模拟装配缺陷。
func newDeliveryTestService(t *testing.T, channels []core.NotifyChannel,
	adapter *fakeDeliveryAdapter, profiles deliveryProfileSource,
) (*deliveryService, *fakeDeliveryStore) {
	t.Helper()
	store := newFakeDeliveryStore()
	sender := tool.NewDeliverySender(
		fakeAssemblyChannels{channels: channels},
		allowAllNotifyPolicy{},
		map[string]tool.NotifyAdapter{"feishu": adapter},
		nil,
	)
	lifecycle := &fakeDeliveryLifecycle{
		record: core.LifecycleRecord{Agent: "writer", AgentUID: "au_writer"},
		exists: true,
	}
	service := newDeliveryService(store, sender, lifecycle, profiles,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return service, store
}

// writerEntry 构造注册表里的 writer 条目：声明渠道与绑定 UID 由用例给出。
func writerEntry(declared []string, bindings ...core.ResourceBinding) core.AgentEntry {
	return core.AgentEntry{
		Name:       "writer",
		Registered: true,
		Profile: &core.Profile{
			Name:             "writer",
			NotifyChannels:   declared,
			ResourceBindings: core.ResourceBindings{NotifyChannels: bindings},
		},
	}
}

// TestDeliveryService_PrepareWritesPendingRow —— 运行前先落 pending 行，行里带
// 生命周期取出的 Agent 身份、渠道 UID 与名称；声明为空 = 不限制。
// Prepare writes the pending row with the lifecycle identity before any run starts.
func TestDeliveryService_PrepareWritesPendingRow(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, &fakeDeliveryAdapter{},
		fakeDeliveryProfiles{"writer": writerEntry(nil)})

	intent := core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	}
	require.NoError(t, service.Prepare(context.Background(), intent))

	row, err := store.Get(context.Background(), "run_1")
	require.NoError(t, err)
	require.Equal(t, core.DeliveryPending, row.Status)
	require.Equal(t, core.AgentUID("au_writer"), row.AgentUID)
	require.Equal(t, "feishu-ops", row.ChannelName)
	require.Equal(t, core.ResourceUID("rs_ops"), row.ChannelUID)
	require.Equal(t, "daily", row.TaskID)
}

// TestDeliveryService_PrepareRejectsOutsideDeclaredChannels —— R7 声明边界：Agent 声明
// 了通知渠道时，代投目标必须是声明并绑定的那些 UID；越界即失败关闭且零写入。
// Prepare fails closed when the target is outside the agent's declared channels.
func TestDeliveryService_PrepareRejectsOutsideDeclaredChannels(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	channels := []core.NotifyChannel{
		deliveryTestChannel("rs_ops", "feishu-ops"),
		deliveryTestChannel("rs_other", "feishu-other"),
	}

	cases := []struct {
		name     string
		declared []string
		bindings []core.ResourceBinding
	}{
		{
			name:     "target not in bound set",
			declared: []string{"feishu-ops"},
			bindings: []core.ResourceBinding{{Name: "feishu-ops", UID: "rs_ops"}},
		},
		{
			name:     "declared but unbound",
			declared: []string{"feishu-other"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, store := newDeliveryTestService(t, channels, &fakeDeliveryAdapter{},
				fakeDeliveryProfiles{"writer": writerEntry(tc.declared, tc.bindings...)})
			err := service.Prepare(context.Background(), core.DeliveryIntent{
				RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_other",
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), "不在 Agent")
			require.Empty(t, store.rows, "边界拒绝时不得写投递行")
		})
	}
}

// TestDeliveryService_PrepareAcceptsBoundDeclaredChannel —— 声明且绑定同一 UID 的渠道放行。
// A declared and bound channel passes the boundary check.
func TestDeliveryService_PrepareAcceptsBoundDeclaredChannel(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, &fakeDeliveryAdapter{},
		fakeDeliveryProfiles{"writer": writerEntry([]string{"feishu-ops"},
			core.ResourceBinding{Name: "feishu-ops", UID: "rs_ops"})})

	require.NoError(t, service.Prepare(context.Background(), core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	}))
	require.Len(t, store.rows, 1)
}

// TestDeliveryService_PrepareFailsClosedWithoutProfiles —— 没有注册表就无法复核声明边界，
// 装配缺陷必须让本轮不发起运行。
// A missing profile source keeps Prepare failing closed.
func TestDeliveryService_PrepareFailsClosedWithoutProfiles(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, &fakeDeliveryAdapter{}, nil)

	err := service.Prepare(context.Background(), core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "注册表")
	require.Empty(t, store.rows)
}

// TestDeliveryService_PrepareRejectsUnknownAgentInRegistry —— 注册表里没有该 Agent
// （例如已归档）时同样失败关闭。
// An agent missing from the registry fails closed too.
func TestDeliveryService_PrepareRejectsUnknownAgentInRegistry(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, &fakeDeliveryAdapter{},
		fakeDeliveryProfiles{})

	err := service.Prepare(context.Background(), core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "不在注册表")
	require.Empty(t, store.rows)
}

// TestDeliveryService_PrepareRejectsOutOfScopeChannel —— 使用范围在运行前复核：
// 专享渠道不属于该 Agent 时不发起运行。
// Prepare re-checks scope eligibility before the run starts.
func TestDeliveryService_PrepareRejectsOutOfScopeChannel(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	channel := deliveryTestChannel("rs_ops", "feishu-ops")
	channel.Scope = core.ScopeExclusive
	channel.OwnerAgentUID = "au_someone_else"
	service, store := newDeliveryTestService(t, []core.NotifyChannel{channel},
		&fakeDeliveryAdapter{}, fakeDeliveryProfiles{"writer": writerEntry(nil)})

	err := service.Prepare(context.Background(), core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "可用范围")
	require.Empty(t, store.rows)
}

// TestDeliveryService_PrepareRequiresLifecycle —— 生命周期状态缺失时拿不到稳定身份，
// 本轮不发起运行。
// Prepare requires the lifecycle record for the stable agent identity.
func TestDeliveryService_PrepareRequiresLifecycle(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, &fakeDeliveryAdapter{},
		fakeDeliveryProfiles{"writer": writerEntry(nil)})
	service.lifecycle = &fakeDeliveryLifecycle{exists: false}

	err := service.Prepare(context.Background(), core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "生命周期")
	require.Empty(t, store.rows)
}

// TestDeliveryService_DeliverWritesTerminalState —— 运行成功后按预写行里的目标发送，
// 并回填 sent 与尝试时间。
// Deliver sends after the run and writes the terminal state.
func TestDeliveryService_DeliverWritesTerminalState(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	adapter := &fakeDeliveryAdapter{outcome: core.DeliveryOutcome{
		Status: core.DeliverySent, HTTPStatus: 200, Detail: "已发送：https://example.test",
	}}
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, adapter,
		fakeDeliveryProfiles{"writer": writerEntry(nil)})

	intent := core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	}
	require.NoError(t, service.Prepare(context.Background(), intent))
	outcome := service.Deliver(context.Background(), intent, "日报正文")

	require.Equal(t, core.DeliverySent, outcome.Status)
	require.Equal(t, 1, adapter.calls)
	require.Equal(t, []string{"https://example.test/hook"}, adapter.targets)
	require.Equal(t, []string{"日报正文"}, adapter.contents)

	row, err := store.Get(context.Background(), "run_1")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySent, row.Status)
	require.Equal(t, 200, row.HTTPStatus)
	require.False(t, row.AttemptedAt.IsZero())
}

// TestDeliveryService_DeliverMissingRecordFailsWithoutSend —— 没有预写行就**不发送**，
// 返回确定失败；不允许"先发送再补记录"。
// Deliver without a prewritten row never sends.
func TestDeliveryService_DeliverMissingRecordFailsWithoutSend(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	adapter := &fakeDeliveryAdapter{outcome: core.DeliveryOutcome{Status: core.DeliverySent}}
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, adapter,
		fakeDeliveryProfiles{"writer": writerEntry(nil)})

	outcome := service.Deliver(context.Background(), core.DeliveryIntent{
		RunID: "run_missing", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	}, "正文")

	require.Equal(t, core.DeliveryFailed, outcome.Status)
	require.Contains(t, outcome.Detail, "预写记录缺失")
	require.Zero(t, adapter.calls)
	require.Empty(t, store.rows)
}

// TestDeliveryService_DeliverUnknownIsRecordedNotRetried —— 结果未知按 unknown 回填，
// 只调用一次适配器（不盲目重发）。
// An ambiguous outcome is recorded as unknown and never resent.
func TestDeliveryService_DeliverUnknownIsRecordedNotRetried(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	adapter := &fakeDeliveryAdapter{outcome: core.DeliveryOutcome{
		Status: core.DeliveryUnknown, Detail: "投递请求超时，结果未知：https://example.test",
	}}
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, adapter,
		fakeDeliveryProfiles{"writer": writerEntry(nil)})

	intent := core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	}
	require.NoError(t, service.Prepare(context.Background(), intent))
	outcome := service.Deliver(context.Background(), intent, "正文")

	require.Equal(t, core.DeliveryUnknown, outcome.Status)
	require.Equal(t, 1, adapter.calls)
	row, err := store.Get(context.Background(), "run_1")
	require.NoError(t, err)
	require.Equal(t, core.DeliveryUnknown, row.Status)
}

// TestDeliveryService_DeliverRecordLookupFailureFailsWithoutSend —— 预写行读取失败时
// 同样不发送，并返回确定失败。
// A store read failure prevents the send.
func TestDeliveryService_DeliverRecordLookupFailureFailsWithoutSend(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	adapter := &fakeDeliveryAdapter{}
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, adapter,
		fakeDeliveryProfiles{"writer": writerEntry(nil)})
	store.getErr = errors.New("disk unavailable")

	outcome := service.Deliver(context.Background(), core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	}, "正文")

	require.Equal(t, core.DeliveryFailed, outcome.Status)
	require.Zero(t, adapter.calls)
}

// TestDeliveryService_SkipKeepsRow —— 运行未成功时标 skipped，记录保留。
// Skip keeps the prewritten row with a skipped state.
func TestDeliveryService_SkipKeepsRow(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, &fakeDeliveryAdapter{},
		fakeDeliveryProfiles{"writer": writerEntry(nil)})

	intent := core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	}
	require.NoError(t, service.Prepare(context.Background(), intent))
	require.NoError(t, service.Skip(context.Background(), intent, "运行失败，未发送"))

	row, err := store.Get(context.Background(), "run_1")
	require.NoError(t, err)
	require.Equal(t, core.DeliverySkipped, row.Status)
	require.Equal(t, "运行失败，未发送", row.Detail)
}

// TestDeliveryService_FinishWriteFailureKeepsPending —— 终态写失败不改判，行保持 pending，
// 由下次启动恢复标为未知（不当作已送达）。
// A failed terminal write leaves the row pending for startup recovery.
func TestDeliveryService_FinishWriteFailureKeepsPending(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	adapter := &fakeDeliveryAdapter{outcome: core.DeliveryOutcome{Status: core.DeliverySent}}
	channels := []core.NotifyChannel{deliveryTestChannel("rs_ops", "feishu-ops")}
	service, store := newDeliveryTestService(t, channels, adapter,
		fakeDeliveryProfiles{"writer": writerEntry(nil)})

	intent := core.DeliveryIntent{
		RunID: "run_1", TaskID: "daily", Agent: "writer", ChannelUID: "rs_ops",
	}
	require.NoError(t, service.Prepare(context.Background(), intent))
	store.finishErr = errors.New("database is locked")

	outcome := service.Deliver(context.Background(), intent, "正文")

	require.Equal(t, core.DeliverySent, outcome.Status, "发送结果照实返回")
	row, err := store.Get(context.Background(), "run_1")
	require.NoError(t, err)
	require.Equal(t, core.DeliveryPending, row.Status, "写失败时行保持 pending")
}

// TestDeliveryService_RecoverDelegates —— 启动恢复直接交给存储，不在这里做二次判断。
// Recover delegates to the store.
func TestDeliveryService_RecoverDelegates(t *testing.T) {
	t.Setenv("BASALT_TEST_DELIVERY_HOOK", "https://example.test/hook")
	service, store := newDeliveryTestService(t, nil, &fakeDeliveryAdapter{},
		fakeDeliveryProfiles{})
	store.recoverCount = 2

	count, err := service.Recover(context.Background(), time.Now())
	require.NoError(t, err)
	require.Equal(t, 2, count)
}
