package app

// 本文件是「最后一次连接测试」投影与「本地就绪」两个读数的判卷（US-12 R7 范围修订，
// check `012-app-resource-admin` / `012-app-notify-author`）：测试结果三分落投影、
// 模型测试三条分支都写投影、availability 只回答本地配置就绪（不得冒充实测）、
// last_test 与 availability 分开显示、从未测过时 last_test 为 nil。
// These tests pin the last-test projection and the local-readiness split on both read
// faces: availability is configuration, last_test is a recorded test, never conflated.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
	"github.com/ymedword/basalt/internal/web"
)

// recordingTestStore 是投影仓储的内存形态：记录每次 upsert 的最后一条。
// recordingTestStore keeps only the last upsert, mirroring the table's semantics.
type recordingTestStore struct {
	mu   sync.Mutex
	rows []core.ResourceConnectionTest
	err  error
}

func (r *recordingTestStore) RecordConnectionTest(_ context.Context, rec core.ResourceConnectionTest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	for i := range r.rows {
		if r.rows[i].Kind == rec.Kind && r.rows[i].ResourceUID == rec.ResourceUID {
			r.rows[i] = rec
			return nil
		}
	}
	r.rows = append(r.rows, rec)
	return nil
}

func (r *recordingTestStore) ListConnectionTests(_ context.Context,
	kind core.ResourceKind,
) ([]core.ResourceConnectionTest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	var out []core.ResourceConnectionTest
	for _, row := range r.rows {
		if row.Kind == kind {
			out = append(out, row)
		}
	}
	return out, nil
}

// lastOf 取一条投影（不存在时 false）。
// lastOf returns the recorded projection for one kind/uid.
func (r *recordingTestStore) lastOf(kind core.ResourceKind) (core.ResourceConnectionTest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range r.rows {
		if row.Kind == kind {
			return row, true
		}
	}
	return core.ResourceConnectionTest{}, false
}

// TestModelConnection_TestWritesLastTestProjection：模型连接的三个结论分支都覆盖
// 「最后一次测试」投影——成功 success、外呼失败 failed、超时 unknown；操作标识与
// 台账同一行（可回溯）。
// 中文验收点：测过就有记录，三档不混写。
func TestModelConnection_TestWritesLastTestProjection(t *testing.T) {
	t.Parallel()

	t.Run("成功", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
			Body: `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`,
		})
		recorder := &recordingTestStore{}
		env := newResourceEnv(t, func(d *ResourceAdminDeps) { d.ConnectionTests = recorder })
		env.publishModelConnection(t, "main", srv.URL(), "m1")

		result, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
		require.NoError(t, err)
		require.True(t, result.OK)

		projected, ok := recorder.lastOf(core.ResourceModelConnection)
		require.True(t, ok)
		require.Equal(t, env.pointer("main").ResourceUID, projected.ResourceUID)
		require.Equal(t, core.ConnectionTestSuccess, projected.Status)
		require.Equal(t, baselineClock().UTC(), projected.TestedAt)
		rows := env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionTest})
		require.Len(t, rows, 1)
		require.Equal(t, rows[0].OperationID, projected.OperationID, "投影必须指回可查证的操作行")
		require.Contains(t, projected.Detail, "ms")
	})

	t.Run("外部失败", func(t *testing.T) {
		t.Parallel()
		srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
			StatusCode: http.StatusInternalServerError,
			Body:       `{"error":{"message":"boom"}}`,
		})
		recorder := &recordingTestStore{}
		env := newResourceEnv(t, func(d *ResourceAdminDeps) { d.ConnectionTests = recorder })
		env.publishModelConnection(t, "main", srv.URL(), "m1")

		result, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
		require.NoError(t, err)
		require.False(t, result.OK)

		projected, ok := recorder.lastOf(core.ResourceModelConnection)
		require.True(t, ok)
		require.Equal(t, core.ConnectionTestFailed, projected.Status)
		require.NotContains(t, projected.Detail, resourceTestKeySentinel, "投影同样脱敏")
	})

	t.Run("超时", func(t *testing.T) {
		t.Parallel()
		// 处理函数挂起到客户端超时；release 兜底让 httptest.Server.Close 能返回。
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}))
		t.Cleanup(srv.Close)
		t.Cleanup(func() { close(release) })
		recorder := &recordingTestStore{}
		env := newResourceEnv(t, func(d *ResourceAdminDeps) {
			d.ConnectionTests = recorder
			d.ModelTestTimeout = 50 * time.Millisecond
		})
		env.publishModelConnection(t, "main", srv.URL, "m1")

		_, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
		require.ErrorIs(t, err, context.DeadlineExceeded)

		projected, ok := recorder.lastOf(core.ResourceModelConnection)
		require.True(t, ok)
		require.Equal(t, core.ConnectionTestUnknown, projected.Status,
			"超时是未知，不是失败——更不得显示成已验证")
	})
}

// TestModelConnection_ProjectionFailureKeepsResult：投影写失败只记 ERROR，测试结论
// 与操作台账不受影响（不得把已完成的测试说成失败）。
// 中文验收点：投影坏了不改结论。
func TestModelConnection_ProjectionFailureKeepsResult(t *testing.T) {
	t.Parallel()
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`,
	})
	env := newResourceEnv(t, func(d *ResourceAdminDeps) {
		d.ConnectionTests = &recordingTestStore{err: context.DeadlineExceeded}
	})
	env.publishModelConnection(t, "main", srv.URL(), "m1")

	result, err := env.admin.TestModelConnection(context.Background(), resourceActor(), "main")
	require.NoError(t, err)
	require.True(t, result.OK)
	require.Contains(t, env.logs.String(), "写入模型连接最后测试结果失败")
	require.Len(t, env.listOperations(core.OperationFilter{Action: core.ActionModelConnectionTest}), 1)
}

// TestResourceViews_AvailabilityIsLocalReadinessOnly：模型连接的 availability 只看
// 本地配置（env 变量是否存在），last_test 只来自真实测试记录；两者分开，从未测过时
// last_test 为 null（不是"成功"也不是"失败"）。
// 中文验收点：配置就绪 ≠ 已验证。
func TestResourceViews_AvailabilityIsLocalReadinessOnly(t *testing.T) {
	t.Parallel()
	env := newRunResourcesEnv(t)
	ctx := context.Background()

	version := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "http://127.0.0.1:1", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", version.ID, true)
	uid := fixtureResourceUID(t, core.ResourceModelConnection, "alpha")

	newViews := func(lookup func(string) (string, bool), tests core.ResourceConnectionTestStore) web.ResourceReader {
		return NewResourceViews(env.versions, env.lifecycle, env.mcp,
			storage.NewCredentialStore(env.db), ResourceViewDeps{
				ConnectionTests: tests,
				LookupEnv:       lookup,
			})
	}

	// 环境变量就绪、从未测试：ready + nil。
	views := newViews(func(name string) (string, bool) {
		if name == "BASALT_ALPHA_KEY" {
			return "sk-present", true
		}
		return "", false
	}, nil)
	connections, err := views.ModelConnections(ctx)
	require.NoError(t, err)
	require.Len(t, connections, 1)
	require.Equal(t, core.AvailabilityReady, connections[0].Availability)
	require.Nil(t, connections[0].LastTest, "从未测过必须是 null")

	// 环境变量缺失：unavailable——不得因为"曾测过"而显示可用。
	missing := newViews(func(string) (string, bool) { return "", false }, nil)
	detail, err := missing.ModelConnection(ctx, "alpha")
	require.NoError(t, err)
	require.Equal(t, core.AvailabilityUnavailable, detail.Availability)
	require.Nil(t, detail.LastTest)

	// 写入一条失败投影：availability 仍是本地就绪，last_test 单独给出失败结论。
	recorder := &recordingTestStore{}
	require.NoError(t, recorder.RecordConnectionTest(ctx, core.ResourceConnectionTest{
		Kind:        core.ResourceModelConnection,
		ResourceUID: uid,
		TestedAt:    baselineClock(),
		Status:      core.ConnectionTestFailed,
		Detail:      "对端返回 500",
		OperationID: "op-1760000000000-abcdef",
	}))
	withTest := newViews(func(name string) (string, bool) {
		if name == "BASALT_ALPHA_KEY" {
			return "sk-present", true
		}
		return "", false
	}, recorder)
	connections, err = withTest.ModelConnections(ctx)
	require.NoError(t, err)
	require.Equal(t, core.AvailabilityReady, connections[0].Availability)
	require.NotNil(t, connections[0].LastTest)
	require.Equal(t, core.ConnectionTestFailed, connections[0].LastTest.Status)
	require.Equal(t, "2026-10-21T09:00:00Z", connections[0].LastTest.TestedAt)
	require.Equal(t, "对端返回 500", connections[0].LastTest.Detail)
}

// TestNotifyChannelRead_AvailabilityAndLastTest：同一组读数在通知读面上成立——
// 未配置地址是 unconfigured（不是 unavailable）、env 变量缺失是 unavailable、
// last_test 只来自投影且指回操作行。
// 中文验收点：渠道的"能不能用"分本地配置与实测两栏。
func TestNotifyChannelRead_AvailabilityAndLastTest(t *testing.T) {
	t.Parallel()
	env := newResourceStateEnv(t)
	ctx := context.Background()

	env.seedChannel(t, "ops", core.ScopeShared, "")
	// 创建可省略地址 = 渠道暂不可发送（empty URL 行）。
	blankUID, err := core.NewResourceUID()
	require.NoError(t, err)
	require.NoError(t, storage.NewNotifyAdminStore(env.db).Create(ctx, core.NotifyChannel{
		ResourceUID: blankUID, Name: "blank", Type: "webhook", OperationID: env.opID,
	}))

	recorder := &recordingTestStore{}
	read := newNotifyChannelReadPort(storage.NewNotifyChannelRepo(env.db), env.credentials,
		env.versions, notifyReadDeps{
			ConnectionTests: recorder,
			LookupEnv: func(name string) (string, bool) {
				if name == "BASALT_TEST_OPS_HOOK" {
					return "https://hooks.example.com/services/ops", true
				}
				return "", false
			},
		})

	channel, err := read.Channel(ctx, "ops")
	require.NoError(t, err)
	require.Equal(t, core.AvailabilityReady, channel.Availability)
	require.Nil(t, channel.LastTest)

	blank, err := read.Channel(ctx, "blank")
	require.NoError(t, err)
	require.Equal(t, core.AvailabilityUnconfigured, blank.Availability,
		"没有地址是「未配置」，不是「不可用」")

	record := core.ResourceConnectionTest{
		Kind:        core.ResourceNotifyChannel,
		ResourceUID: channel.ResourceUID,
		TestedAt:    baselineClock(),
		Status:      core.ConnectionTestSuccess,
		Detail:      "通知渠道测试消息已发送",
		OperationID: "op-1760000000000-abcdef",
	}
	require.NoError(t, recorder.RecordConnectionTest(ctx, record))

	channel, err = read.Channel(ctx, "ops")
	require.NoError(t, err)
	require.Equal(t, core.AvailabilityReady, channel.Availability)
	require.NotNil(t, channel.LastTest)
	require.Equal(t, core.ConnectionTestSuccess, channel.LastTest.Status)
	require.Equal(t, baselineClock().UTC(), channel.LastTest.TestedAt)

	// 环境变量缺失：本地配置不可用，但实测记录仍在（两条事实分开显示）。
	missing := newNotifyChannelReadPort(storage.NewNotifyChannelRepo(env.db), env.credentials,
		env.versions, notifyReadDeps{
			ConnectionTests: recorder,
			LookupEnv:       func(string) (string, bool) { return "", false },
		})
	channel, err = missing.Channel(ctx, "ops")
	require.NoError(t, err)
	require.Equal(t, core.AvailabilityUnavailable, channel.Availability)
	require.NotNil(t, channel.LastTest, "配置缺失不得抹掉上次实测记录")
}
