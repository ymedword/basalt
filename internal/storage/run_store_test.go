package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// US-12 R4 起清单 v2 必填：Agent / 资源稳定 UID 与目标绑定哈希。UID 字面量按契约取
// `ag_` / `rs_` + 26 位 Crockford Base32（无 I/L/O/U），末位按资源序号变化便于辨认。
// Manifest v2 requires stable uids and the target-binding hash in fixtures.
const (
	testAgentUID      core.AgentUID = "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	testTargetBinding string        = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// testResourceUID 造一个合法的资源 UID（末位区分同一测试里的多个资源）。
// testResourceUID builds a valid resource uid varying only in its last byte.
func testResourceUID(last byte) core.ResourceUID {
	return core.ResourceUID("rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3" + string(last))
}

// runRow 是 runs 一行的判卷形态（列全部显式——可空列用 Null 形态，判 NULL 与判空串不同）。
// runRow is the assertion shape of one runs row; nullable columns stay nullable.
type runRow struct {
	RunID      string
	SessionID  sql.NullString
	Agent      string
	Channel    string
	UserID     string
	Trigger    string
	Status     string
	Reason     sql.NullString
	Iterations int
	ToolCalls  int
	ConfigHash sql.NullString
	StartedAt  int64
	FinishedAt sql.NullInt64
}

// readRunRow 按 run_id 读回账本行（in-package 判卷走 db.read——生产写入路径仍唯一经仓储）。
// readRunRow reads one ledger row back by run_id through the read pool.
func readRunRow(t *testing.T, db *DB, runID string) runRow {
	t.Helper()
	var row runRow
	err := db.read.QueryRowContext(context.Background(),
		`SELECT run_id, session_id, agent, channel, user_id, trigger, status,
		        reason, iterations, tool_calls, config_hash, started_at, finished_at
		   FROM runs WHERE run_id = ?`, runID).
		Scan(&row.RunID, &row.SessionID, &row.Agent, &row.Channel, &row.UserID, &row.Trigger,
			&row.Status, &row.Reason, &row.Iterations, &row.ToolCalls, &row.ConfigHash,
			&row.StartedAt, &row.FinishedAt)
	require.NoError(t, err)
	return row
}

// fixedStart 是账本用例的冻结时刻（时间显式传入——仓储不自取业务时间）。
// fixedStart is the frozen clock of the ledger cases.
var fixedStart = time.Date(2026, 10, 16, 8, 30, 0, 123*int(time.Millisecond), time.UTC)

// startRow 造一份起始行夹具（渠道参数化——trigger 断言要用四个渠道各来一次）。
// startRow builds a starting-row fixture for the given channel.
func startRow(channel string) core.RunRecord {
	return core.RunRecord{
		RunID:      channel + "-1776000000123-abc123",
		SessionID:  "cli-3f9a1c07b2d84e55",
		Agent:      "demo",
		Channel:    channel,
		UserID:     "alice",
		ConfigHash: "3f9a1c07b2d84e55",
		StartedAt:  fixedStart,
	}
}

// TestRunStore_StartRunInsertsRunningRow —— K1：起始行 = `status` 写 `running`（由 SQL 固定，
// 不由调用方给）、`reason` / `finished_at` 为 NULL、计数列取缺省 0、时间戳落毫秒。
// TestRunStore_StartRunInsertsRunningRow pins the starting row.
func TestRunStore_StartRunInsertsRunningRow(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	rec := startRow("cli")
	require.NoError(t, store.StartRun(ctx, rec, core.RunResourceManifest{}))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusRunning), row.Status, "起始态由仓储固定写 running")
	require.Equal(t, rec.Agent, row.Agent)
	require.Equal(t, rec.Channel, row.Channel)
	require.Equal(t, rec.UserID, row.UserID)
	require.Equal(t, rec.SessionID, row.SessionID.String)
	require.Equal(t, rec.ConfigHash, row.ConfigHash.String)
	require.False(t, row.Reason.Valid, "起始行没有原因 → NULL")
	require.False(t, row.FinishedAt.Valid, "起始行未结束 → NULL")
	require.Zero(t, row.Iterations)
	require.Zero(t, row.ToolCalls)
	require.Equal(t, fixedStart.UnixMilli(), row.StartedAt)
}

// TestRunStore_FinishRunUpdatesAllTerminalColumns —— K2：终态更新**全部列**；空 reason 写回
// NULL（不是空串）；`FinishRun` 的签名里**没有** finalText 可言（最终文本不进账本）。
// TestRunStore_FinishRunUpdatesAllTerminalColumns pins the terminal columns.
func TestRunStore_FinishRunUpdatesAllTerminalColumns(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	rec := startRow("web")
	require.NoError(t, store.StartRun(ctx, rec, core.RunResourceManifest{}))

	finished := fixedStart.Add(90 * time.Second)
	require.NoError(t, store.FinishRun(ctx, rec.RunID, core.StatusCompleted, "", 2, 1, finished))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusCompleted), row.Status)
	require.False(t, row.Reason.Valid, "正常终止没有原因 → NULL（空串与没值是两回事）")
	require.Equal(t, 2, row.Iterations)
	require.Equal(t, 1, row.ToolCalls)
	require.Equal(t, finished.UnixMilli(), row.FinishedAt.Int64)
	require.Equal(t, fixedStart.UnixMilli(), row.StartedAt, "起始时刻不被终态改写")

	// 非正常终止：reason 落文本（另一行——同一行的第二次 Finish 归幂等用例，见 K7）。
	limited := startRow("web")
	limited.RunID = "web-1776000000123-limit1"
	require.NoError(t, store.StartRun(ctx, limited, core.RunResourceManifest{}))
	require.NoError(t, store.FinishRun(ctx, limited.RunID, core.StatusLimitReached, "工具调用次数超限", 3, 50, finished))
	again := readRunRow(t, db, limited.RunID)
	require.Equal(t, string(core.StatusLimitReached), again.Status)
	require.Equal(t, "工具调用次数超限", again.Reason.String)
	require.Equal(t, 50, again.ToolCalls)
}

// TestRunStore_CloseRunningLeavesTerminalRowsUntouched —— K3：崩溃收尾**只把仍为 `running`
// 的**改 `failed`（`WHERE status = 'running'` 是这条语句的全部意义）；扫描面与收尾面配套。
// TestRunStore_CloseRunningLeavesTerminalRowsUntouched: only rows still running are closed;
// terminal states are never re-judged.
func TestRunStore_CloseRunningLeavesTerminalRowsUntouched(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	// 两种终态各一行 + 一行仍 running + 一行无状态（空 session）仍 running。
	completed := startRow("cli")
	completed.RunID = "cli-1776000000123-comple"
	require.NoError(t, store.StartRun(ctx, completed, core.RunResourceManifest{}))
	require.NoError(t, store.FinishRun(ctx, completed.RunID, core.StatusCompleted, "", 1, 0, fixedStart))

	cancelled := startRow("cli")
	cancelled.RunID = "cli-1776000000123-cancel"
	require.NoError(t, store.StartRun(ctx, cancelled, core.RunResourceManifest{}))
	require.NoError(t, store.FinishRun(ctx, cancelled.RunID, core.StatusCancelled, "context canceled", 1, 0, fixedStart))

	stale := startRow("scheduler")
	stale.RunID = "scheduler-1776000000123-stale0"
	require.NoError(t, store.StartRun(ctx, stale, core.RunResourceManifest{}))

	stateless := startRow("web")
	stateless.RunID = "web-1776000000123-statle"
	stateless.SessionID = ""
	require.NoError(t, store.StartRun(ctx, stateless, core.RunResourceManifest{}))

	running, err := listRunningRunIDs(ctx, db.write)
	require.NoError(t, err)
	require.Len(t, running, 2, "扫描面只出仍为 running 的行（空会话的无状态运行也算）")

	closedAt := fixedStart.Add(5 * time.Minute)
	const reason = "进程中断（启动时发现未完成的运行）"
	n := 0
	for _, runID := range running {
		closed, err := closeRunInTransaction(ctx, db.write, runID, closedAt)
		require.NoError(t, err)
		if closed {
			n++
		}
	}
	require.Equal(t, 2, n, "恰收尾两行仍 running 的运行")

	// 已结束的两行**原样保留**（状态、原因、起始时刻都不动）。
	done := readRunRow(t, db, completed.RunID)
	require.Equal(t, string(core.StatusCompleted), done.Status, "已结束的状态不得被覆盖")
	require.False(t, done.Reason.Valid)
	cancel := readRunRow(t, db, cancelled.RunID)
	require.Equal(t, string(core.StatusCancelled), cancel.Status)
	require.Equal(t, "context canceled", cancel.Reason.String, "已写下的原因不得被改写")

	// 仍 running 的两行被收尾：failed + 原因 + finished_at。
	for _, id := range []string{stale.RunID, stateless.RunID} {
		row := readRunRow(t, db, id)
		require.Equal(t, string(core.StatusFailed), row.Status, "run_id=%s", id)
		require.Equal(t, reason, row.Reason.String)
		require.Equal(t, closedAt.UnixMilli(), row.FinishedAt.Int64)
		require.Equal(t, fixedStart.UnixMilli(), row.StartedAt, "收尾不改起始时刻")
		require.Zero(t, row.Iterations, "收尾不伪造计数（跑到哪算到哪）")
	}

	// 幂等：连跑第二次零变化（恢复扫描可随时重跑）。
	again := 0
	for _, runID := range running {
		closed, err := closeRunInTransaction(ctx, db.write, runID, closedAt.Add(time.Minute))
		require.NoError(t, err)
		if closed {
			again++
		}
	}
	require.Zero(t, again, "第二次收尾没有可收的行")
	left, err := listRunningRunIDs(ctx, db.write)
	require.NoError(t, err)
	require.Empty(t, left)
}

// TestRunStore_RunsTableHasNoFinalTextColumn —— K4（§4.3 判据）：`runs` 表的列清单与契约
// 逐字一致，且**不含** `final_text`——最终回答只存在返回结果与会话的 assistant 消息里。
// TestRunStore_RunsTableHasNoFinalTextColumn asserts the column list verbatim, and that no
// column carries the final answer.
func TestRunStore_RunsTableHasNoFinalTextColumn(t *testing.T) {
	db := openTempDB(t)

	rows, err := db.read.QueryContext(context.Background(), `PRAGMA table_info(runs)`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	var columns []string
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())

	require.Equal(t, []string{
		"run_id", "session_id", "agent", "channel", "user_id", "trigger", "status",
		"reason", "iterations", "tool_calls", "config_hash", "started_at", "finished_at",
	}, columns, "列清单以 docs/contracts/schema.sql 为准（逐字、含顺序）")
	require.NotContains(t, columns, "final_text", "运行表不得有最终文本列")
}

// TestRunStore_TimestampsAreUnixMilli —— K5：两处时间列都存 **Unix 毫秒**
// （迁移纪律 4——不是秒、不是纳秒）。
// TestRunStore_TimestampsAreUnixMilli pins both timestamp columns to Unix milliseconds.
func TestRunStore_TimestampsAreUnixMilli(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	start := time.Date(2026, 10, 16, 8, 30, 0, 987*int(time.Millisecond), time.UTC)
	rec := startRow("admin")
	rec.StartedAt = start
	require.NoError(t, store.StartRun(ctx, rec, core.RunResourceManifest{}))

	finish := start.Add(1500 * time.Millisecond)
	require.NoError(t, store.FinishRun(ctx, rec.RunID, core.StatusFailed, "模型调用失败", 1, 0, finish))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, start.UnixMilli(), row.StartedAt)
	require.Equal(t, finish.UnixMilli(), row.FinishedAt.Int64)
	require.EqualValues(t, 1500, row.FinishedAt.Int64-row.StartedAt, "差值也按毫秒记账")
	require.Len(t, strconv.FormatInt(row.StartedAt, 10), 13,
		"Unix 毫秒是 13 位十进制——秒（10 位）与纳秒（19 位）都落在这里之外")
}

// TestRunStore_TriggerForScheduler —— 约束 15：显式 trigger 只允许 `scheduler` 渠道下的
// `cron` / `manual`；其余组合一律拒绝落账（显式值不得绕过渠道约束）。
// 逆改点：只校验显式值、不校验渠道 → web+manual / cli+cron 两例红。
// TestRunStore_TriggerForScheduler pins the explicit-trigger channel constraint.
func TestRunStore_TriggerForScheduler(t *testing.T) {
	t.Run("scheduler_cron_accepted", func(t *testing.T) {
		got, err := triggerFor("scheduler", core.TriggerCron)
		require.NoError(t, err)
		require.Equal(t, triggerCron, got)
	})

	t.Run("scheduler_manual_accepted", func(t *testing.T) {
		got, err := triggerFor("scheduler", core.TriggerManual)
		require.NoError(t, err)
		require.Equal(t, triggerManual, got)
	})

	t.Run("scheduler_other_explicit_rejected", func(t *testing.T) {
		_, err := triggerFor("scheduler", core.Trigger("weekly"))
		require.Error(t, err, "scheduler 只接受 cron / manual")
	})

	t.Run("web_manual_rejected", func(t *testing.T) {
		_, err := triggerFor("web", core.TriggerManual)
		require.Error(t, err, "非 scheduler 渠道不得自称 manual")
	})

	t.Run("cli_cron_rejected", func(t *testing.T) {
		_, err := triggerFor("cli", core.TriggerCron)
		require.Error(t, err, "非 scheduler 渠道不得自称 cron")
	})
}

// TestRunStore_TriggerDerivesFromChannel —— K6：`trigger` 由渠道映射取值（§6 固定口径 8），
// 未映射的渠道**拒绝建行**（不落一行猜出来的记账）。
// TestRunStore_TriggerDerivesFromChannel pins the channel-to-trigger mapping; an unmapped
// channel writes no row at all.
func TestRunStore_TriggerDerivesFromChannel(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	for channel, want := range map[string]string{
		"cli":       triggerUser,
		"web":       triggerAPI,
		"scheduler": triggerCron,
		"admin":     triggerManual,
	} {
		rec := startRow(channel)
		require.NoError(t, store.StartRun(ctx, rec, core.RunResourceManifest{}), "渠道 %s", channel)
		require.Equal(t, want, readRunRow(t, db, rec.RunID).Trigger, "渠道 %s 的 trigger", channel)
	}

	err := store.StartRun(ctx, startRow("email"), core.RunResourceManifest{})
	require.Error(t, err, "未映射的渠道不得建行")
	require.Contains(t, err.Error(), "email", "错误必须点名渠道")

	var count int
	require.NoError(t, db.read.QueryRowContext(ctx, `SELECT count(*) FROM runs`).Scan(&count))
	require.Equal(t, 4, count, "拒绝的渠道零落痕")
}

// ---- 运行资源账（约束 18 / 19，K7 / K8） ------------------------------------

// runManifestFixture 是资源账用例的夹具：一 Provider、两 MCP、一技能，版本都已落库。
// 清单由准入构造器产出（带哈希），与真实准入路径同形。
// runManifestFixture seeds one provider, two MCP servers and one skill version, then
// builds the admission manifest the same way the real admission path does.
type runManifestFixture struct {
	manifest core.RunResourceManifest
	resolved core.ResolvedResources
	versions []core.ResourceVersion
}

// seedRunManifest 落库四条资源版本并返回可准入的清单（生命周期 active）。
// 版本行的外键要求管理操作先存在——真实路径由基线 / US-12 的 BeginOperation 提供。
// seedRunManifest inserts the version rows (their FK requires an operation row first)
// and returns a manifest that admission would accept.
func seedRunManifest(t *testing.T, db *DB) runManifestFixture {
	t.Helper()
	ctx := context.Background()
	seedOperation(t, db, "op-run-manifest")
	store := NewResourceStore(db)

	provider, _, err := store.EnsureVersion(ctx,
		modelVersion("openai", "sha256:provider", "op-run-manifest", 0))
	require.NoError(t, err)
	browser, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceUID:    resourceUIDFor(core.ResourceMCPServer, "browser"),
		ResourceID:     "browser",
		ContentHash:    "sha256:mcp-browser",
		DefinitionYAML: "name: browser\ncommand: npx\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    "op-run-manifest",
	})
	require.NoError(t, err)
	filesystem, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceUID:    resourceUIDFor(core.ResourceMCPServer, "filesystem"),
		ResourceID:     "filesystem",
		ContentHash:    "sha256:mcp-filesystem",
		DefinitionYAML: "name: filesystem\ncommand: npx\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    "op-run-manifest",
	})
	require.NoError(t, err)
	skill, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  resourceUIDFor(core.ResourceSkill, "summarize"),
		ResourceID:   "summarize",
		ContentHash:  "sha256:skill-summarize",
		ManifestJSON: `[{"path":"SKILL.md","sha256":"sha256:skill-file","size":12}]`,
		StoragePath:  ".basalt/versions/skills/summarize/1",
		CreatedAt:    opNow.UnixMilli(),
		OperationID:  "op-run-manifest",
	})
	require.NoError(t, err)

	// 引用行的 UID 取自版本行自身：清单固定的身份与落库的版本必须是同一个实例。
	// The reference takes the version's own uid so the manifest and the row agree.
	refOf := func(v core.ResourceVersion) core.ResourceVersionRef {
		return core.ResourceVersionRef{
			Name:        v.ResourceID,
			ResourceUID: v.ResourceUID,
			VersionID:   v.ID,
			VersionNo:   v.VersionNo,
			ContentHash: v.ContentHash,
		}
	}
	// MCP 故意乱序给出：清单必须在构造时规范化，引用行落库顺序不得依赖调用方顺序。
	resolved := core.ResolvedResources{
		Provider:   refOf(provider),
		MCPServers: []core.ResourceVersionRef{refOf(filesystem), refOf(browser)},
		Skills: []core.SkillVersionRef{{
			ResourceVersionRef: refOf(skill),
			Files: []core.FileHash{{
				Path:   "SKILL.md",
				SHA256: "sha256:skill-file",
				Size:   12,
			}},
		}},
		ProviderModel:      "gpt-test",
		ProviderCredential: core.CredentialPin{Mode: core.CredentialModeEnv, TargetBinding: testTargetBinding},
		Notify:             core.NotifyManifest{Mode: core.NotifyModeAllEligible, Channels: []core.NotifyChannelRef{}},
	}
	manifest, err := core.BuildRunResourceManifest(testAgentUID, core.AgentActive, resolved)
	require.NoError(t, err)
	return runManifestFixture{
		manifest: manifest,
		resolved: resolved,
		versions: []core.ResourceVersion{provider, browser, filesystem, skill},
	}
}

// manifestRow 是 run_resource_manifests 一行的判卷形态。
// manifestRow is the assertion shape of one manifest row.
type manifestRow struct {
	FormatVersion  int
	ManifestHash   string
	AgentLifecycle string
	SnapshotJSON   string
	CreatedAt      int64
}

// readManifestRow 按 run_id 读回清单行（读池判卷，与 run_query.go 同源）。
// readManifestRow reads the manifest row back through the read pool.
func readManifestRow(t *testing.T, db *DB, runID string) manifestRow {
	t.Helper()
	var row manifestRow
	err := db.read.QueryRowContext(context.Background(),
		`SELECT format_version, manifest_hash, agent_lifecycle_state, snapshot_json, created_at
		   FROM run_resource_manifests WHERE run_id = ?`, runID).
		Scan(&row.FormatVersion, &row.ManifestHash, &row.AgentLifecycle, &row.SnapshotJSON, &row.CreatedAt)
	require.NoError(t, err)
	return row
}

// runRefRow 是 run_resource_refs 一行的判卷形态（released_at 用 Null 形态判"未释放"）。
// runRefRow is the assertion shape of one reference row.
type runRefRow struct {
	Kind         string
	ResourceID   string
	ResourceUID  sql.NullString
	ResourceName sql.NullString
	VersionID    int64
	ContentHash  string
	ReleasedAt   sql.NullInt64
}

// readRunRefRows 按 run_id 读回引用行，按 (kind, id) 排序以得到稳定断言序。
// readRunRefRows reads the reference rows in a stable (kind, id) order.
func readRunRefRows(t *testing.T, db *DB, runID string) []runRefRow {
	t.Helper()
	rows, err := db.read.QueryContext(context.Background(),
		`SELECT resource_kind, resource_id, resource_uid, resource_name,
		        version_id, content_hash, released_at
		   FROM run_resource_refs WHERE run_id = ?
		  ORDER BY resource_kind, resource_id`, runID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []runRefRow
	for rows.Next() {
		var row runRefRow
		require.NoError(t, rows.Scan(&row.Kind, &row.ResourceID, &row.ResourceUID,
			&row.ResourceName, &row.VersionID, &row.ContentHash, &row.ReleasedAt))
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
}

// requireNoRunTrace 断言被拒绝的准入**零落痕**：runs / 清单 / 引用三表都没有该 run_id。
// requireNoRunTrace asserts a refused admission left no trace in any of the three tables.
func requireNoRunTrace(t *testing.T, db *DB, runID string) {
	t.Helper()
	ctx := context.Background()
	var runs, manifests, refs int
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM runs WHERE run_id = ?`, runID).Scan(&runs))
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = ?`, runID).Scan(&manifests))
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_refs WHERE run_id = ?`, runID).Scan(&refs))
	require.Zero(t, runs, "整笔回滚：runs 不得留下半行")
	require.Zero(t, manifests, "整笔回滚：清单不得留下半行")
	require.Zero(t, refs, "整笔回滚：引用不得留下半行")
}

// rehashManifest 重算清单哈希（改过内容的夹具必须重算才可能通过哈希校验）。
// rehashManifest re-derives the hash after a fixture is mutated on purpose.
func rehashManifest(t *testing.T, m *core.RunResourceManifest) {
	t.Helper()
	hash, err := core.HashManifest(*m)
	require.NoError(t, err)
	m.Hash = hash
}

// TestRunStore_StartRunWithoutManifestWritesOnlyTheRow —— 零清单（FormatVersion == 0）
// 表示装配未接线：只写运行行，**不伪造**清单行与引用（历史运行如实读作"未记录"）。
// TestRunStore_StartRunWithoutManifestWritesOnlyTheRow writes the run row alone for a zero
// manifest, so the run reads back as unrecorded rather than fabricated.
func TestRunStore_StartRunWithoutManifestWritesOnlyTheRow(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	rec := startRow("cli")
	require.NoError(t, store.StartRun(ctx, rec, core.RunResourceManifest{}))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusRunning), row.Status)

	var manifests, refs int
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = ?`, rec.RunID).Scan(&manifests))
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_refs WHERE run_id = ?`, rec.RunID).Scan(&refs))
	require.Zero(t, manifests, "零清单不得写出清单行（假账会被读成事实）")
	require.Zero(t, refs, "零清单不得写出资源引用")
}

// TestRunStore_StartRunWritesRunManifestAndRefs —— 约束 18 的正向判据：
// 运行行、清单行与全部引用一次落库；清单哈希可被读侧重算复现；引用一律未释放。
// TestRunStore_StartRunWritesRunManifestAndRefs pins the single-transaction
// admission write: run row, manifest row and every reference commit together.
func TestRunStore_StartRunWritesRunManifestAndRefs(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	rec := startRow("web")
	require.NoError(t, store.StartRun(ctx, rec, fx.manifest))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusRunning), row.Status, "准入写 running（由 SQL 固定）")
	require.Equal(t, rec.ConfigHash, row.ConfigHash.String, "config_hash 保持 ConfigSnapshot 原义")

	m := readManifestRow(t, db, rec.RunID)
	require.Equal(t, fx.manifest.FormatVersion, m.FormatVersion)
	require.Equal(t, fx.manifest.Hash, m.ManifestHash)
	require.Equal(t, string(core.AgentActive), m.AgentLifecycle)
	require.Equal(t, rec.StartedAt.UnixMilli(), m.CreatedAt)

	var parsed core.RunResourceManifest
	require.NoError(t, json.Unmarshal([]byte(m.SnapshotJSON), &parsed))
	require.Equal(t, fx.manifest, parsed, "快照 JSON 必须逐字段还原准入清单")
	gotHash, err := core.HashManifest(parsed)
	require.NoError(t, err)
	require.Equal(t, m.ManifestHash, gotHash, "读侧重算哈希必须与列值一致（run_query 同一口径）")

	refs := readRunRefRows(t, db, rec.RunID)
	require.Len(t, refs, 4, "Provider + 两个 MCP + 一个技能各一条引用")
	require.Equal(t, []string{
		"mcp_server/browser",
		"mcp_server/filesystem",
		"model_connection/openai",
		"skill/summarize",
	}, []string{
		refs[0].Kind + "/" + refs[0].ResourceID,
		refs[1].Kind + "/" + refs[1].ResourceID,
		refs[2].Kind + "/" + refs[2].ResourceID,
		refs[3].Kind + "/" + refs[3].ResourceID,
	}, "引用集合与清单一致（种类 × 名称）")

	byKey := make(map[string]core.ResourceVersion, len(fx.versions))
	for _, v := range fx.versions {
		byKey[string(v.Kind)+"/"+v.ResourceID] = v
	}
	for _, ref := range refs {
		version, ok := byKey[ref.Kind+"/"+ref.ResourceID]
		require.True(t, ok, "引用 %s/%s 必须来自夹具版本", ref.Kind, ref.ResourceID)
		require.Equal(t, version.ID, ref.VersionID, "引用固定的是版本行 id")
		require.Equal(t, version.ContentHash, ref.ContentHash)
		// 身份与名称快照同时落库：删除闸门按 UID 判定，历史核对按名称。
		require.Equal(t, string(version.ResourceUID), ref.ResourceUID.String,
			"引用行的 resource_uid 必须等于版本行的身份")
		require.Equal(t, version.ResourceID, ref.ResourceName.String,
			"resource_name 是准入时的名称快照")
		require.False(t, ref.ReleasedAt.Valid, "准入刚写下的引用一律未释放")
	}
}

// TestRunStore_StartRunRollsBackOnMissingVersion —— 约束 18 的反向判据：
// 任一引用写不进去（版本行不存在）→ 整笔回滚，三表零落痕（零 Run、零引用）。
// TestRunStore_StartRunRollsBackOnMissingVersion pins the all-or-nothing
// admission: a missing version row rolls back the run and the manifest too.
func TestRunStore_StartRunRollsBackOnMissingVersion(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	broken := fx.manifest
	broken.Provider.VersionID = 999999 // 不存在的版本行
	rehashManifest(t, &broken)         // 内容合法、哈希自洽——失败只能来自外键

	rec := startRow("cli")
	err := store.StartRun(ctx, rec, broken)
	require.Error(t, err)
	require.Contains(t, err.Error(), "预写运行资源引用", "失败点是被外键拒绝的引用行")
	require.Contains(t, err.Error(), "model_connection/openai", "错误必须点名是哪条引用")
	requireNoRunTrace(t, db, rec.RunID)
}

// TestRunStore_StartRunRejectsMissingIdentity —— US-12 R4：引用行的身份是删除闸门
// 与范围判定的依据。清单缺资源 UID 时在写侧拒绝并点名是哪条引用，而不是让 NOT NULL
// 在事务中途报一个分不清主语的约束错误；三表零落痕。
// TestRunStore_StartRunRejectsMissingIdentity refuses a manifest whose references
// carry no resource uid, naming the offending reference.
func TestRunStore_StartRunRejectsMissingIdentity(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	broken := fx.manifest
	broken.MCPServers[0].ResourceUID = ""
	rehashManifest(t, &broken)

	rec := startRow("cli")
	err := store.StartRun(ctx, rec, broken)
	require.Error(t, err)
	require.Contains(t, err.Error(), "资源身份")
	require.Contains(t, err.Error(), broken.MCPServers[0].Name, "错误必须点名是哪条引用")
	requireNoRunTrace(t, db, rec.RunID)
}

// TestRunStore_StartRunRejectsMissingAgentUID —— v2 清单必须固定 Agent 身份：
// 缺 UID 的清单会让"这次运行是哪个 Agent"只能靠名称推断，同名重建后即失真。
// TestRunStore_StartRunRejectsMissingAgentUID refuses a v2 manifest without an agent uid.
func TestRunStore_StartRunRejectsMissingAgentUID(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	broken := fx.manifest
	broken.AgentUID = ""
	rehashManifest(t, &broken)

	rec := startRow("cli")
	err := store.StartRun(ctx, rec, broken)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Agent 身份")
	requireNoRunTrace(t, db, rec.RunID)
}

// TestRunStore_StartRunRejectsTamperedHash —— 哈希是派生值：内容被改而
// 哈希未重算的清单在写侧就被拒绝，不落到读侧才被发现"账是坏的"。
// TestRunStore_StartRunRejectsTamperedHash refuses a manifest whose
// content no longer matches its recorded hash.
func TestRunStore_StartRunRejectsTamperedHash(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	tampered := fx.manifest
	tampered.Skills[0].Files[0].SHA256 = "sha256:tampered" // 故意不重算哈希

	rec := startRow("cli")
	err := store.StartRun(ctx, rec, tampered)
	require.Error(t, err)
	require.Contains(t, err.Error(), "哈希与内容不一致")
	requireNoRunTrace(t, db, rec.RunID)
}

// TestRunStore_StartRunRejectsNonActiveLifecycle —— 清单里只可能出现
// active（schema 注释与 §3.4）：停用 / 归档的 Agent 根本不该走到准入写库这一步。
// TestRunStore_StartRunRejectsNonActiveLifecycle refuses a manifest frozen
// under any state other than active.
func TestRunStore_StartRunRejectsNonActiveLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	disabled, err := core.BuildRunResourceManifest(testAgentUID, core.AgentDisabled, fx.resolved)
	require.NoError(t, err, "构造器允许冻结非启用态——拒绝发生在写侧")

	rec := startRow("cli")
	err = store.StartRun(ctx, rec, disabled)
	require.Error(t, err)
	require.Contains(t, err.Error(), "生命周期状态必须是 active")
	requireNoRunTrace(t, db, rec.RunID)
}

// TestRunStore_FinishRunReleasesRefsInSameTransaction —— 约束 19 的正向判据：终态与
// 引用释放一起提交，且释放按 run_id 收窄——共用同一批版本的另一个 Run 不受波及。
// TestRunStore_FinishRunReleasesRefsInSameTransaction pins the same-transaction release,
// scoped to the run that actually transitioned.
func TestRunStore_FinishRunReleasesRefsInSameTransaction(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	rec := startRow("web")
	require.NoError(t, store.StartRun(ctx, rec, fx.manifest))
	other := startRow("web")
	other.RunID = "web-1776000000123-other1"
	require.NoError(t, store.StartRun(ctx, other, fx.manifest),
		"同一批版本可被多个 Run 同时引用（引用表主键是 run_id + 资源）")

	finished := fixedStart.Add(2 * time.Minute)
	require.NoError(t, store.FinishRun(ctx, rec.RunID, core.StatusCompleted, "", 2, 1, finished))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusCompleted), row.Status)
	require.Equal(t, finished.UnixMilli(), row.FinishedAt.Int64)

	released := readRunRefRows(t, db, rec.RunID)
	require.Len(t, released, 4)
	for _, ref := range released {
		require.True(t, ref.ReleasedAt.Valid, "终态的 Run 不得留着未释放引用（%s/%s）",
			ref.Kind, ref.ResourceID)
		require.Equal(t, finished.UnixMilli(), ref.ReleasedAt.Int64, "释放时刻与终态同一时刻")
	}

	for _, ref := range readRunRefRows(t, db, other.RunID) {
		require.False(t, ref.ReleasedAt.Valid,
			"释放只属于本次收尾的 Run——%s/%s 不得被连带释放", ref.Kind, ref.ResourceID)
	}
}

// TestRunStore_FinishRunRollsBackWhenReleaseFails —— 约束 19 的反向判据：引用释放失败
// 时终态转换一起回滚（同一事务），运行保持 running 等下次收口，不会出现"已结束但仍占着版本"。
// TestRunStore_FinishRunRollsBackWhenReleaseFails proves the status flip and the release
// commit or roll back as one unit.
func TestRunStore_FinishRunRollsBackWhenReleaseFails(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	rec := startRow("cli")
	require.NoError(t, store.StartRun(ctx, rec, fx.manifest))

	_, err := db.write.ExecContext(ctx, `CREATE TRIGGER fail_run_ref_release
	    BEFORE UPDATE ON run_resource_refs
	    BEGIN SELECT RAISE(ABORT, '注入的引用释放失败'); END`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.write.ExecContext(context.Background(),
			`DROP TRIGGER IF EXISTS fail_run_ref_release`)
		require.NoError(t, err)
	})

	err = store.FinishRun(ctx, rec.RunID, core.StatusCompleted, "", 1, 0, fixedStart.Add(time.Minute))
	require.Error(t, err, "引用释放失败必须如实上抛")
	require.Contains(t, err.Error(), "释放运行资源引用")

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusRunning), row.Status,
		"引用释放失败 → 终态转换一起回滚（同一事务）")
	require.False(t, row.FinishedAt.Valid)
	for _, ref := range readRunRefRows(t, db, rec.RunID) {
		require.False(t, ref.ReleasedAt.Valid, "回滚后引用仍未释放：%s/%s", ref.Kind, ref.ResourceID)
	}
}

// TestRunStore_FinishRunDoesNotOverwriteTerminalState —— K7（约束 19）：同一行的第二次
// Finish 是幂等成功——不覆盖已判定的终态、不改原因与计数（注 8）。
// TestRunStore_FinishRunDoesNotOverwriteTerminalState: a second Finish is idempotent
// success and never re-judges the terminal row.
func TestRunStore_FinishRunDoesNotOverwriteTerminalState(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	rec := startRow("web")
	require.NoError(t, store.StartRun(ctx, rec, core.RunResourceManifest{}))
	first := fixedStart.Add(90 * time.Second)
	require.NoError(t, store.FinishRun(ctx, rec.RunID, core.StatusCompleted, "", 2, 1, first))

	// 迟到的失败写（并发收尾 / 重复取消）：必须有守卫，且按幂等成功返回。
	late := first.Add(30 * time.Second)
	require.NoError(t, store.FinishRun(ctx, rec.RunID, core.StatusCancelled, "迟到的取消", 9, 9, late),
		"已终态的行 → 幂等成功，不报错也不覆盖")

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusCompleted), row.Status, "已判定的终态不得被改口")
	require.False(t, row.Reason.Valid, "已写下的原因（此处为 NULL）不得被改")
	require.Equal(t, 2, row.Iterations, "终态计数不得被迟到写入覆盖")
	require.Equal(t, 1, row.ToolCalls)
	require.Equal(t, first.UnixMilli(), row.FinishedAt.Int64, "终态时刻保持首次判定")
}

// TestRunStore_FinishRunMissingRowIsError —— 0 行影响分两种：行存在 = 幂等成功；行不存在
// = **如实报错**——账本行缺失说明启动纪律被绕过，不得被"更新成功"掩盖。
// TestRunStore_FinishRunMissingRowIsError treats a missing ledger row as a hard error.
func TestRunStore_FinishRunMissingRowIsError(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewRunStore(db)

	err := store.FinishRun(ctx, "web-1776000000123-ghost", core.StatusFailed, "", 0, 0, fixedStart)
	require.Error(t, err)
	require.Contains(t, err.Error(), "账本行不存在")
}
