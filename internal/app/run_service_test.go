package app

// 本文件是运行端口适配器的验收面（颗粒度文档 §3.2.1：internal/app/run_service_test.go，
// 8 例）：Run / Archive / Cancel 委托 core.AgentService，ListRuns / GetRun 委托
// storage.RunStore，错误原样透传，接口方法集冻结为五项。适配器**只委托、不复制语义**，
// 因此这里的断言全部落在"读到的就是被委托方给的"——二次排序、补造字段或改写错误都会红。
// This file covers the run-service adapter: pure delegation, error pass-through, and the
// frozen five-method port. Every case is offline and uses a temp database.

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/web"
)

// newRunServiceFixture 建一个临时库 + 同一份账本仓储 + 适配器：读面与写面共用一个
// 实例，正是生产装配的纪律（wire.go 只建一次 runs）。
// newRunServiceFixture wires one store instance to both faces, mirroring production.
func newRunServiceFixture(t *testing.T) (*storage.DB, *storage.RunStore, *runService) {
	t.Helper()
	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(t.TempDir(), "basalt.db"), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	runs := storage.NewRunStore(db)
	svc := core.NewAgentService(core.ServiceOptions{
		Runs:   runs,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	adapter, ok := NewRunService(svc, runs).(*runService)
	require.True(t, ok, "NewRunService 必须返回 *runService（装配层唯一生产实现）")
	return db, runs, adapter
}

// seedRunServiceRow 预写一行运行账本；非 running 的目标状态在同一事务语义下经
// FinishRun 写入（与生产路径同一个写入口）。
// seedRunServiceRow pre-writes one ledger row and finishes it through the same entry.
func seedRunServiceRow(t *testing.T, runs *storage.RunStore, runID, agent string,
	startedAt time.Time, status core.RunStatus,
) {
	t.Helper()
	ctx := context.Background()
	// 零清单 = 装配未接线：只写运行行，读取面如实呈现"未记录"（约束 24）。
	require.NoError(t, runs.StartRun(ctx, core.RunRecord{
		RunID: runID, Agent: agent, Channel: "web", UserID: "u-1", StartedAt: startedAt,
	}, core.RunResourceManifest{}))
	if status != core.StatusRunning {
		require.NoError(t, runs.FinishRun(ctx, runID, status, "", 1, 0, startedAt.Add(time.Minute)))
	}
}

// TestRunService_SatisfiesWebRunService 对应颗粒度文档 §3.1.5 / §3.1.31：接口方法集
// **冻结为五项**——多一项就要同步适配器与全部测试替身，少一项会让装配层在别处转嫁；
// 构造器返回的具体类型也必须是 *runService（不得把 AgentService 直接塞进接口）。
// The port is frozen at five methods; the concrete type is the adapter.
func TestRunService_SatisfiesWebRunService(t *testing.T) {
	port := NewRunService(nil, nil)
	adapter, ok := port.(*runService)
	require.True(t, ok, "NewRunService 必须返回 *runService")
	require.Nil(t, adapter.Service)
	require.Nil(t, adapter.Runs)

	typ := reflect.TypeOf((*web.RunService)(nil)).Elem()
	names := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		names = append(names, typ.Method(i).Name)
	}
	require.Equal(t, []string{"Archive", "Cancel", "GetRun", "ListRuns", "Run"}, names,
		"web.RunService 方法集必须与模块卡 §3.1.5 逐字一致")
}

// TestRunService_RunDelegatesToAgentService 对应 §3.1.31：Run 必须走唯一运行入口。
// 零值服务对未知 Agent 返回 *AgentNotFoundError 且不落痕迹——适配器若自建入口，
// 这里看到的就不是该错误的形态。
// Run must delegate: the zero-value service reports an unknown agent through core's error.
func TestRunService_RunDelegatesToAgentService(t *testing.T) {
	adapter := NewRunService(core.NewAgentService(core.ServiceOptions{}), nil)
	res, err := adapter.Run(context.Background(), core.RunRequest{Agent: "ghost"})
	var notFound *core.AgentNotFoundError
	require.ErrorAs(t, err, &notFound, "Run 必须原样返回 core 的准入错误")
	require.Equal(t, "ghost", notFound.Name)
	require.Equal(t, core.RunResult{}, res, "调用方问题不返回半截结果")
}

// TestRunService_ArchiveDelegatesToAgentService 对应 §3.1.31：Archive 保持既有"按会话"
// 归档语义，装配缺失会话门禁时返回 core 自产的 *SessionGateUnavailableError（不是 404）。
// Archive keeps the session-scoped semantics and surfaces the core-side gate error.
func TestRunService_ArchiveDelegatesToAgentService(t *testing.T) {
	adapter := NewRunService(core.NewAgentService(core.ServiceOptions{}), nil)
	err := adapter.Archive(context.Background(), "sess-1")
	var gate *core.SessionGateUnavailableError
	require.ErrorAs(t, err, &gate, "归档必须委托 core，不得在本层另起语义")
}

// TestRunService_ListRunsDelegatesToRunStore 对应 §3.1.31 / 约束 21：列表排序、窗口与
// 三过滤由仓储固定——适配器**不得**二次排序、补造字段或裁剪清单哈希。
// ListRuns must return the repository projection untouched, including fixed ordering.
func TestRunService_ListRunsDelegatesToRunStore(t *testing.T) {
	_, runs, adapter := newRunServiceFixture(t)
	base := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	seedRunServiceRow(t, runs, "run-a", "alpha", base, core.StatusCompleted)
	seedRunServiceRow(t, runs, "run-b", "beta", base.Add(time.Hour), core.StatusRunning)
	seedRunServiceRow(t, runs, "run-c", "alpha", base.Add(2*time.Hour), core.StatusFailed)

	filter := core.RunFilter{Agent: "alpha", Limit: 10}
	got, err := adapter.ListRuns(context.Background(), filter)
	require.NoError(t, err)
	want, err := runs.ListRuns(context.Background(), filter)
	require.NoError(t, err)
	require.Equal(t, want, got, "适配器不得二次排序、补造或裁剪")
	require.Len(t, got, 2, "Agent 过滤必须是值谓词绑定")
	require.Equal(t, "run-c", got[0].RunID, "固定排序 started_at DESC")
	require.Equal(t, "run-a", got[1].RunID)
}

// TestRunService_GetRunDelegatesToRunStore 对应 §3.1.31 / 约束 24：详情与仓储直读逐字段
// 相等；零清单运行返回 nil 清单（历史运行标为"未记录"），不补造。
// GetRun equals the repository read verbatim; a zero-manifest run stays unrecorded.
func TestRunService_GetRunDelegatesToRunStore(t *testing.T) {
	_, runs, adapter := newRunServiceFixture(t)
	started := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	seedRunServiceRow(t, runs, "run-detail", "alpha", started, core.StatusCompleted)

	got, err := adapter.GetRun(context.Background(), "run-detail")
	require.NoError(t, err)
	want, err := runs.GetRun(context.Background(), "run-detail")
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Nil(t, got.ResourceManifest, "零清单运行必须读作未记录，不得补造清单")
	require.Equal(t, core.StatusCompleted, got.Status)
}

// TestRunService_CancelDelegatesToAgentService 对应 §3.1.31 / 约束 16：Cancel 只发信号
// 并返回当前状态，终态由循环写入；装配缺失、已结束与未知运行三种形态都原样透传。
// Cancel delegates signal-only cancellation; terminal state stays with the loop.
func TestRunService_CancelDelegatesToAgentService(t *testing.T) {
	ctx := context.Background()

	// ① 账本没接上：core 自产的装配错误原样上抛（不得改写成本层文案或 404）。
	bare := NewRunService(core.NewAgentService(core.ServiceOptions{}), nil)
	_, err := bare.Cancel(ctx, "run-x")
	require.EqualError(t, err, "agent service: run store is not configured")

	// ② 已结束的行：返回当前终态，且不因取消改写状态。
	_, runs, adapter := newRunServiceFixture(t)
	seedRunServiceRow(t, runs, "run-done", "alpha",
		time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), core.StatusCompleted)
	detail, err := adapter.Cancel(ctx, "run-done")
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, detail.Status, "取消不得覆盖已写终态")

	// ③ 未知运行：仓储的 ErrRunNotFound 原样上抛，由 web 映射 404。
	_, err = adapter.Cancel(ctx, "run-missing")
	require.ErrorIs(t, err, core.ErrRunNotFound)
}

// TestRunService_GetRunPropagatesErrRunNotFound 对应 §3.1.31：查不到行时返回哨兵错误，
// 适配器不得把它变成空详情 + nil（那会让 web 渲染出一行不存在的运行）。
// GetRun surfaces the sentinel so the web layer can map 404.
func TestRunService_GetRunPropagatesErrRunNotFound(t *testing.T) {
	_, _, adapter := newRunServiceFixture(t)
	_, err := adapter.GetRun(context.Background(), "run-absent")
	require.ErrorIs(t, err, core.ErrRunNotFound)
}

// TestRunService_ListRunsPropagatesStoreError 对应 §3.1.31 / 约束 24：仓储错误必须上抛
// 并保留"查询运行列表"上下文；适配器不得吞错返回空列表（空列表是合法读数，混淆两者
// 会让排障把故障看成"没有运行"）。
// Store errors propagate with their context; an empty list must mean an empty window.
func TestRunService_ListRunsPropagatesStoreError(t *testing.T) {
	db, _, adapter := newRunServiceFixture(t)
	require.NoError(t, db.Close())

	_, err := adapter.ListRuns(context.Background(), core.RunFilter{})
	require.Error(t, err, "库不可读时必须报错，不得返回空列表")
	require.Contains(t, err.Error(), "查询运行列表")
}
