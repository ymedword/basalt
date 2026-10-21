package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 读侧用例的夹具时刻：毫秒对齐（列存 Unix 毫秒，断言与读取都按毫秒比较）。
// The frozen fixture clock of the read-side cases, aligned to milliseconds.
var queryBase = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// queryRun 是一行账本夹具：只列读侧关心的列，其余取固定值。
// queryRun is one ledger fixture row covering the read-side columns.
type queryRun struct {
	runID      string
	sessionID  string
	agent      string
	channel    string
	trigger    string
	status     string
	reason     string
	iterations int
	toolCalls  int
	configHash string
	startedAt  time.Time
	finishedAt time.Time // 零值 = NULL（运行中）
}

// seedQueryRun 直接落一行账本（读侧用例不经过写路径——写路径的单事务形态由 T019 守）。
// seedQueryRow inserts one ledger row directly; the write path is exercised elsewhere.
func seedQueryRun(t *testing.T, db *DB, r queryRun) {
	t.Helper()
	if r.channel == "" {
		r.channel = "cli"
	}
	if r.trigger == "" {
		r.trigger = "user"
	}
	finishedAt := any(sql.NullInt64{})
	if !r.finishedAt.IsZero() {
		finishedAt = r.finishedAt.UnixMilli()
	}
	_, err := db.write.ExecContext(context.Background(),
		`INSERT INTO runs
		   (run_id, session_id, agent, channel, user_id, trigger, status,
		    reason, iterations, tool_calls, config_hash, started_at, finished_at)
		 VALUES (?, ?, ?, ?, 'u1', ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.runID, nullIfEmpty(r.sessionID), r.agent, r.channel, r.trigger, r.status,
		nullIfEmpty(r.reason), r.iterations, r.toolCalls, nullIfEmpty(r.configHash),
		r.startedAt.UnixMilli(), finishedAt)
	require.NoError(t, err)
}

// sampleManifest 造一份完整清单（列表名刻意不排序——读路径不得重排，越序读回即证）。
// sampleManifest builds a complete manifest; names stay unsorted on purpose.
func sampleManifest() core.RunResourceManifest {
	return core.RunResourceManifest{
		FormatVersion:  1,
		AgentLifecycle: "active",
		Provider: core.ResourceVersionRef{
			Name: "deepseek", VersionID: 11, VersionNo: 2, ContentHash: "aaaaaaaa",
		},
		MCPServers: []core.ResourceVersionRef{
			{Name: "zeta", VersionID: 12, VersionNo: 1, ContentHash: "bbbbbbbb"},
			{Name: "alpha", VersionID: 13, VersionNo: 1, ContentHash: "cccccccc"},
		},
		Skills: []core.SkillVersionRef{{
			ResourceVersionRef: core.ResourceVersionRef{
				Name: "summarize", VersionID: 14, VersionNo: 1, ContentHash: "dddddddd",
			},
			Files: []core.FileHash{{Path: "SKILL.md", SHA256: "eeeeeeee", Size: 42}},
		}},
	}
}

// seedManifestRow 落一行清单（`hashOverride` 非空时故意写入错误哈希，供校验用例用）。
// seedManifestRow inserts one manifest row; a non-empty override plants a wrong hash.
func seedManifestRow(t *testing.T, db *DB, runID string, m core.RunResourceManifest, hashOverride string) {
	t.Helper()
	hash, err := core.HashManifest(m)
	require.NoError(t, err)
	m.Hash = hash
	raw, err := core.MarshalManifest(m)
	require.NoError(t, err)
	stored := hash
	if hashOverride != "" {
		stored = hashOverride
	}
	_, err = db.write.ExecContext(context.Background(),
		`INSERT INTO run_resource_manifests
		   (run_id, format_version, manifest_hash, agent_lifecycle_state, snapshot_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		runID, m.FormatVersion, stored, m.AgentLifecycle, string(raw), queryBase.UnixMilli())
	require.NoError(t, err)
}

// countRowsIn 数某张表的行数（读侧"零写入"判据——只对测试里写死的表名调用）。
// countRowsIn counts rows of a table named by a compile-time literal in the test.
func countRowsIn(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.read.QueryRowContext(context.Background(),
		fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&n))
	return n
}

// 运行详情往返：账本起始行 + 终态列 + final_text + 清单哈希逐字段对号。
// 对应文档用例：「详情」「参数矩阵（详情）」。

func TestRunQuery_GetRunRoundTripsAllColumns(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	seedQueryRun(t, db, queryRun{
		runID: "web-1776000000001-aaa111", sessionID: "web-sess-1", agent: "assistant",
		channel: "web", trigger: "api", status: "completed", reason: "",
		iterations: 3, toolCalls: 2, configHash: "cfg-hash-1",
		startedAt: queryBase, finishedAt: queryBase.Add(4 * time.Second),
	})

	detail, err := store.GetRun(ctx, "web-1776000000001-aaa111")
	require.NoError(t, err)
	require.Equal(t, "web-1776000000001-aaa111", detail.RunID)
	require.Equal(t, "web-sess-1", detail.SessionID)
	require.Equal(t, "assistant", detail.Agent)
	require.Equal(t, "web", detail.Channel)
	require.Equal(t, core.Trigger("api"), detail.Trigger)
	require.Equal(t, core.StatusCompleted, detail.Status)
	require.Empty(t, detail.Reason)
	require.Equal(t, 3, detail.Iterations)
	require.Equal(t, 2, detail.ToolCalls)
	require.Equal(t, "cfg-hash-1", detail.ConfigHash)
	require.True(t, queryBase.Equal(detail.StartedAt))
	require.True(t, queryBase.Add(4*time.Second).Equal(detail.FinishedAt))
	require.Empty(t, detail.ManifestHash, "历史 Run 无清单 → 空哈希")
	require.Nil(t, detail.ResourceManifest, "历史 Run 不回填清单")
}

// 无状态调用的 session_id 写 NULL——读回空串而不是 "null" 或错误。
// 对应文档用例：「NULL 往返」。

func TestRunQuery_GetRunStatelessRunHasEmptySession(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	seedQueryRun(t, db, queryRun{
		runID: "cli-1776000000002-bbb222", agent: "assistant", status: "running",
		startedAt: queryBase,
	})

	detail, err := store.GetRun(ctx, "cli-1776000000002-bbb222")
	require.NoError(t, err)
	require.Empty(t, detail.SessionID, "NULL 会话读回空串")
	require.True(t, detail.FinishedAt.IsZero(), "未收口的行没有 finished_at")
	require.Equal(t, core.StatusRunning, detail.Status)
}

// 不存在的 Run → 固定哨兵（web 映射 404），不是通用错误、也不是空行。
// 对应文档用例：「未知运行 404」。

func TestRunQuery_GetRunMissingReturnsErrRunNotFound(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	_, err := store.GetRun(ctx, "cli-1776000000099-zzz999")
	require.ErrorIs(t, err, core.ErrRunNotFound)
}

// final_text 取最近一条 assistant 消息（按 seq），不取工具结果、不取更早草稿、
// 也不取其后出现的 user 消息。对应文档用例：「取最近助手消息」。

func TestRunQuery_FinalTextTakesLastAssistantMessage(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	const runID = "cli-1776000000003-ccc333"
	seedQueryRun(t, db, queryRun{
		runID: runID, sessionID: "cli-sess-1", agent: "assistant", status: "completed",
		iterations: 2, toolCalls: 1, startedAt: queryBase, finishedAt: queryBase.Add(time.Second),
	})
	seedMessage(t, db.write, "cli-sess-1", 1, "user", "帮我读文件", runID)
	seedMessage(t, db.write, "cli-sess-1", 2, "assistant", "草稿回答", runID)
	seedMessage(t, db.write, "cli-sess-1", 3, "assistant", "", runID,
		core.ToolCall{ID: "c1", Name: "read_file"})
	seedMessage(t, db.write, "cli-sess-1", 4, "tool", "文件内容", runID)
	seedMessage(t, db.write, "cli-sess-1", 5, "assistant", "最终回答", runID)
	seedMessage(t, db.write, "cli-sess-1", 6, "user", "追问", runID)

	detail, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, "最终回答", detail.FinalText)
}

// 没有 assistant 消息（只有 user / tool）→ 空串，不拿工具结果冒充最终回答。
// 对应文档用例：「无助手消息 → 空串」。

func TestRunQuery_FinalTextEmptyWithoutAssistantMessage(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	const runID = "cli-1776000000004-ddd444"
	seedQueryRun(t, db, queryRun{
		runID: runID, sessionID: "cli-sess-2", agent: "assistant", status: "failed",
		reason: "模型调用失败", startedAt: queryBase, finishedAt: queryBase.Add(time.Second),
	})
	seedMessage(t, db.write, "cli-sess-2", 1, "user", "你好", runID)
	seedMessage(t, db.write, "cli-sess-2", 2, "tool", "工具输出", runID)

	detail, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	require.Empty(t, detail.FinalText)
	require.Equal(t, "模型调用失败", detail.Reason, "reason 取账本行，不取消息")
}

// final_text 有 run_id 边界：同一会话里另一 Run 的助手消息不得混入。
// 对应文档用例：「跨 Run 不串话」。

func TestRunQuery_FinalTextScopedToRun(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	const (
		runOne = "cli-1776000000005-eee555"
		runTwo = "cli-1776000000006-fff666"
	)
	seedQueryRun(t, db, queryRun{
		runID: runOne, sessionID: "shared-sess", agent: "assistant", status: "completed",
		startedAt: queryBase, finishedAt: queryBase.Add(time.Second),
	})
	seedQueryRun(t, db, queryRun{
		runID: runTwo, sessionID: "shared-sess", agent: "assistant", status: "completed",
		startedAt: queryBase.Add(time.Minute), finishedAt: queryBase.Add(time.Minute + time.Second),
	})
	seedMessage(t, db.write, "shared-sess", 1, "user", "第一问", runOne)
	seedMessage(t, db.write, "shared-sess", 2, "assistant", "第一答", runOne)
	seedMessage(t, db.write, "shared-sess", 3, "user", "第二问", runTwo)
	seedMessage(t, db.write, "shared-sess", 4, "assistant", "第二答", runTwo)

	one, err := store.GetRun(ctx, runOne)
	require.NoError(t, err)
	require.Equal(t, "第一答", one.FinalText)

	two, err := store.GetRun(ctx, runTwo)
	require.NoError(t, err)
	require.Equal(t, "第二答", two.FinalText)
}

// 列表固定排序：started_at 降序；同一时刻按 run_id 降序（固定次序而非随机序）。
// 对应文档用例：「固定排序」。

func TestRunQuery_ListRunsFixedOrder(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	seedQueryRun(t, db, queryRun{
		runID: "run-a", agent: "assistant", status: "completed",
		startedAt: queryBase.Add(2 * time.Minute),
	})
	seedQueryRun(t, db, queryRun{
		runID: "run-b", agent: "assistant", status: "completed",
		startedAt: queryBase.Add(2 * time.Minute), // 与 run-a 同时刻 → 比 run_id
	})
	seedQueryRun(t, db, queryRun{
		runID: "run-c", agent: "assistant", status: "completed",
		startedAt: queryBase.Add(time.Minute),
	})

	runs, err := store.ListRuns(ctx, core.RunFilter{})
	require.NoError(t, err)
	require.Len(t, runs, 3)
	require.Equal(t, []string{"run-b", "run-a", "run-c"},
		[]string{runs[0].RunID, runs[1].RunID, runs[2].RunID})
}

// 三个过滤参数的值谓词矩阵：单条件各自命中、三条件联合取交集、空过滤返回全部。
// 对应文档用例：「参数矩阵」。

func TestRunQuery_ListRunsFilterMatrix(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	seedQueryRun(t, db, queryRun{
		runID: "r1", sessionID: "s1", agent: "alpha", status: "completed",
		startedAt: queryBase.Add(4 * time.Minute),
	})
	seedQueryRun(t, db, queryRun{
		runID: "r2", sessionID: "s1", agent: "beta", status: "failed",
		startedAt: queryBase.Add(3 * time.Minute),
	})
	seedQueryRun(t, db, queryRun{
		runID: "r3", sessionID: "s2", agent: "alpha", status: "failed",
		startedAt: queryBase.Add(2 * time.Minute),
	})
	seedQueryRun(t, db, queryRun{
		runID: "r4", agent: "alpha", status: "running", startedAt: queryBase.Add(time.Minute),
	})

	cases := []struct {
		name   string
		filter core.RunFilter
		want   []string
	}{
		{"无过滤取全部", core.RunFilter{}, []string{"r1", "r2", "r3", "r4"}},
		{"按会话", core.RunFilter{SessionID: "s1"}, []string{"r1", "r2"}},
		{"按 Agent", core.RunFilter{Agent: "alpha"}, []string{"r1", "r3", "r4"}},
		{"按状态", core.RunFilter{Status: "failed"}, []string{"r2", "r3"}},
		{"会话 + 状态交集", core.RunFilter{SessionID: "s1", Status: "failed"}, []string{"r2"}},
		{"三条件交集", core.RunFilter{SessionID: "s2", Agent: "alpha", Status: "failed"}, []string{"r3"}},
		{"无命中", core.RunFilter{Agent: "ghost"}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs, err := store.ListRuns(ctx, tc.filter)
			require.NoError(t, err)
			got := make([]string, 0, len(runs))
			for _, run := range runs {
				got = append(got, run.RunID)
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// 窗口收敛：缺省 50、上限 200、非正数按缺省；显式小数原样生效。
// 对应文档用例：「limit 矩阵」。

func TestRunQuery_ListRunsClampsLimit(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	const total = 205
	for i := range total {
		seedQueryRun(t, db, queryRun{
			runID:     fmt.Sprintf("run-%03d", i),
			agent:     "assistant",
			status:    "completed",
			startedAt: queryBase.Add(time.Duration(i) * time.Second),
		})
	}

	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"零值按缺省 50", 0, defaultRunListLimit},
		{"负数按缺省 50", -7, defaultRunListLimit},
		{"显式 3", 3, 3},
		{"上限 200", maxRunListLimit, maxRunListLimit},
		{"超上限收敛到 200", total, maxRunListLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs, err := store.ListRuns(ctx, core.RunFilter{Limit: tc.limit})
			require.NoError(t, err)
			require.Len(t, runs, tc.want)
		})
	}
}

// 详情加载准入清单：解析 snapshot_json、重算哈希与库内 manifest_hash 相等，
// 列表路径只带哈希、不嵌完整清单。
// 对应文档用例：「清单解析」「列表只带哈希」。

func TestRunQuery_GetRunLoadsManifestAndVerifiesHash(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	const runID = "web-1776000000007-ggg777"
	seedQueryRun(t, db, queryRun{
		runID: runID, sessionID: "web-sess-2", agent: "assistant", status: "completed",
		startedAt: queryBase, finishedAt: queryBase.Add(time.Second),
	})
	want := sampleManifest()
	seedManifestRow(t, db, runID, want, "")
	wantHash, err := core.HashManifest(want)
	require.NoError(t, err)

	detail, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, wantHash, detail.ManifestHash)
	require.NotNil(t, detail.ResourceManifest)
	require.Equal(t, want.Provider, detail.ResourceManifest.Provider)
	require.Equal(t, want.MCPServers, detail.ResourceManifest.MCPServers,
		"读路径不得重排清单顺序")
	require.Equal(t, want.Skills, detail.ResourceManifest.Skills)

	runs, err := store.ListRuns(ctx, core.RunFilter{})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, wantHash, runs[0].ManifestHash, "列表带哈希")
	require.Nil(t, runs[0].ResourceManifest, "列表不嵌完整清单")
}

// 历史 Run（无清单行）：详情返回 nil 清单、空哈希，且读操作对清单 / 引用表**零写入**。
// 对应文档用例：「legacy 零写入」。

func TestRunQuery_LegacyRunLeavesManifestAbsentAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	const runID = "cli-1776000000008-hhh888"
	seedQueryRun(t, db, queryRun{
		runID: runID, sessionID: "cli-sess-3", agent: "assistant", status: "completed",
		startedAt: queryBase, finishedAt: queryBase.Add(time.Second),
	})

	detail, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	require.Empty(t, detail.ManifestHash)
	require.Nil(t, detail.ResourceManifest)

	_, err = store.ListRuns(ctx, core.RunFilter{})
	require.NoError(t, err)

	require.Equal(t, 0, countRowsIn(t, db, "run_resource_manifests"), "历史 Run 不得回填清单")
	require.Equal(t, 0, countRowsIn(t, db, "run_resource_refs"), "历史 Run 不得补造引用")
}

// 清单哈希不符 → 明确错误（不静默降级成 nil——那会把损坏伪装成历史 Run）。
// 对应文档用例：「哈希校验失败」。

func TestRunQuery_ManifestHashMismatchIsRejected(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	const runID = "web-1776000000009-iii999"
	seedQueryRun(t, db, queryRun{
		runID: runID, sessionID: "web-sess-3", agent: "assistant", status: "completed",
		startedAt: queryBase, finishedAt: queryBase.Add(time.Second),
	})
	seedManifestRow(t, db, runID, sampleManifest(), "deadbeefdeadbeef")

	detail, err := store.GetRun(ctx, runID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "清单哈希不一致")
	require.False(t, errors.Is(err, core.ErrRunNotFound), "损坏不是不存在")
	require.Empty(t, detail.RunID, "失败不返回半截详情")
}

// 概览计数：运行中不受时间窗限制；近 24 小时含端点（now-24h 那一刻算在窗内）；
// 失败计数只覆盖窗口内的失败。对应模块卡 §4.4 的 011-storage-resources。
// Overview counters: running is unwindowed, the 24h cutoff is inclusive, and the
// failure counter only covers in-window failures.

func TestRunQuery_CountRuns_WindowsAndStatuses(t *testing.T) {
	db := openTempDB(t)
	seedQueryRun(t, db, queryRun{
		runID: "run-live", agent: "assistant", status: string(core.StatusRunning),
		startedAt: queryBase.Add(-time.Hour),
	})
	seedQueryRun(t, db, queryRun{
		runID: "run-ok", agent: "assistant", status: string(core.StatusCompleted),
		startedAt: queryBase.Add(-2 * time.Hour), finishedAt: queryBase.Add(-time.Hour),
	})
	seedQueryRun(t, db, queryRun{
		runID: "run-failed", agent: "assistant", status: string(core.StatusFailed),
		startedAt: queryBase.Add(-3 * time.Hour), finishedAt: queryBase.Add(-2 * time.Hour),
	})
	seedQueryRun(t, db, queryRun{
		runID: "run-failed-edge", agent: "assistant", status: string(core.StatusFailed),
		startedAt: queryBase.Add(-24 * time.Hour), finishedAt: queryBase.Add(-23 * time.Hour),
	})
	seedQueryRun(t, db, queryRun{
		runID: "run-failed-old", agent: "assistant", status: string(core.StatusFailed),
		startedAt:  queryBase.Add(-24*time.Hour - time.Millisecond),
		finishedAt: queryBase.Add(-23 * time.Hour),
	})
	seedQueryRun(t, db, queryRun{
		runID: "run-ok-old", agent: "assistant", status: string(core.StatusCompleted),
		startedAt: queryBase.Add(-48 * time.Hour), finishedAt: queryBase.Add(-47 * time.Hour),
	})

	running, last24h, failedLast24h, err := NewRunStore(db).CountRuns(context.Background(), queryBase)
	require.NoError(t, err)
	require.EqualValues(t, 1, running, "运行中不受时间窗限制")
	require.EqualValues(t, 4, last24h, "近 24 小时含 now-24h 端点")
	require.EqualValues(t, 2, failedLast24h, "只数窗口内的失败")
}

// 空账本：三条计数都是真实的 0，不是"读不到"——查询必须成功。
// An empty ledger yields three genuine zeros, not an error.

func TestRunQuery_CountRuns_EmptyLedgerIsZero(t *testing.T) {
	db := openTempDB(t)
	running, last24h, failedLast24h, err := NewRunStore(db).CountRuns(context.Background(), queryBase)
	require.NoError(t, err)
	require.Zero(t, running)
	require.Zero(t, last24h)
	require.Zero(t, failedLast24h)
}
