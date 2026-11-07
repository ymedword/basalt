//go:build integration

package postgres

// 本文件是 runs 账本仓储（起始行 / 终态 / 准入清单与资源引用）的专属判卷面：
// core.RunStore 的两条写路径与 trigger 映射。夹具与读数由本文件自持，生产写入路径
// 仍唯一经仓储；直读只用于判"库里到底写了什么"。
//
// 由 SQLite 谱系 internal/storage/run_store_test.go 逐条移植：测试名、断言、中文验收点
// 与逆改点注释原样保留；只换「库怎么来」（portTestDB）、读写句柄合一（db.pool）、
// 占位符 $1..$n 与 PG 的触发器方言。夹具另补 PG 的 runs 归属四列
// （caller_id / authorizing_kind / key_id / actor）——统一取内置管理员形态
// （local-admin / admin-builtin / 无 Key / actor=caller），迁移已种好 local-admin 行。
// 旧用例 K4 用 PRAGMA 自省列清单（SQLite 专有），按移植口径不移植，见下方跳过说明。
// This file owns the assertion surface of the runs ledger, ported verbatim from the
// SQLite lineage; only the fixture, the placeholder dialect, the trigger syntax and the
// PG ownership columns differ.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// requireFinishRun 断言终态写回成功并返回 `applied`（US-22 C-05：终态写回与引用释放
// 已拆成两个显式调用，释放由 `ReleaseRunRefs` 承担）。零值 token = 未接执行权的装配，
// 走无 fencing 的条件更新；US-24 的租约 fencing 用例直接调 `FinishRun` 并显式传 token。
// requireFinishRun asserts a successful terminal write and returns applied; release is a
// separate explicit call since US-22. A zero token marks an unleased assembly; the US-24
// fencing cases call FinishRun directly with a real token.
func requireFinishRun(t *testing.T, store core.RunStore, ctx context.Context, runID string,
	status core.RunStatus, reason string, iterations, toolCalls int, now time.Time,
) bool {
	t.Helper()
	applied, err := store.FinishRun(ctx, runID, core.LeaseToken{}, status, reason, iterations, toolCalls, now)
	require.NoError(t, err)
	return applied
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

// readRunRow 按 run_id 读回账本行（in-package 判卷走 db.pool——生产写入路径仍唯一经仓储）。
// readRunRow reads one ledger row back by run_id through the pool.
func readRunRow(t *testing.T, db *DB, runID string) runRow {
	t.Helper()
	var row runRow
	err := db.pool.QueryRowContext(context.Background(),
		`SELECT run_id, session_id, agent, channel, user_id, trigger, status,
		        reason, iterations, tool_calls, config_hash, started_at, finished_at
		   FROM runs WHERE run_id = $1`, runID).
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
// PG 的 runs 归属四列 NOT NULL 且带 CHECK，这里补内置管理员 Principal
// （SQLite 谱系无归属列，故此处是夹具适配、不是断言变化）。
// startRow builds a starting-row fixture for the given channel; PG requires the
// ownership columns, so the fixture carries the built-in admin principal.
func startRow(channel string) core.RunRecord {
	return core.RunRecord{
		RunID:      channel + "-1776000000123-abc123",
		SessionID:  "cli-3f9a1c07b2d84e55",
		Agent:      "demo",
		Channel:    channel,
		UserID:     "alice",
		Principal:  core.LocalAdminPrincipal(),
		ConfigHash: "3f9a1c07b2d84e55",
		StartedAt:  fixedStart,
	}
}

// TestRunStore_StartRunInsertsRunningRow —— K1：起始行 = `status` 写 `running`（由 SQL 固定，
// 不由调用方给）、`reason` / `finished_at` 为 NULL、计数列取缺省 0、时间戳落毫秒。
// TestRunStore_StartRunInsertsRunningRow pins the starting row.
func TestRunStore_StartRunInsertsRunningRow(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)

	rec := startRow("cli")
	require.NoError(t, startRunTest(store, ctx, rec, core.RunResourceManifest{}))

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
	db := portTestDB(t)
	store := NewRunStore(db)

	rec := startRow("web")
	require.NoError(t, startRunTest(store, ctx, rec, core.RunResourceManifest{}))

	finished := fixedStart.Add(90 * time.Second)
	requireFinishRun(t, store, ctx, rec.RunID, core.StatusCompleted, "", 2, 1, finished)

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
	require.NoError(t, startRunTest(store, ctx, limited, core.RunResourceManifest{}))
	requireFinishRun(t, store, ctx, limited.RunID, core.StatusLimitReached, "工具调用次数超限", 3, 50, finished)
	again := readRunRow(t, db, limited.RunID)
	require.Equal(t, string(core.StatusLimitReached), again.Status)
	require.Equal(t, "工具调用次数超限", again.Reason.String)
	require.Equal(t, 50, again.ToolCalls)
}

// TestRunStore_CloseRunningLeavesTerminalRowsUntouched —— K3：崩溃收尾**只把仍为 `running`
// 的**改 `failed`（业务行谓词是这条路径的全部意义）；已终态的行一个字都不动。
//
// US-24 把收尾面从"全局扫描"换成"领取过期租约"（FR-24-02 / FR-24-15），于是本用例
// 的守点升级成两条谓词的联合：**过期租约**决定"谁失去属主"，**业务行仍非终态**决定
// "谁还需要收尾"。夹具把四行的租约全部推到过期，已终态的两行仍不得被领取——这说明
// 保护它们的是状态谓词，而不是"租约恰好还活着"这种偶然；逆改点即删掉领取语句里的
// `r.status = 'running'`，那时四行都会被领走、终态行被改口，下列断言全红。
// TestRunStore_CloseRunningLeavesTerminalRowsUntouched: only rows still running are
// claimed and closed; terminal states are never re-judged.
func TestRunStore_CloseRunningLeavesTerminalRowsUntouched(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)

	// US-24 起"可被收尾"的前提是**有租约**：生产里每个非遗留 Run 都由持身份的 Worker
	// 领取，而 StartRun 只在 owner 非零时写 run 租约（零身份是测试与遗留兼容的"无租约"
	// 分支）。本用例要证明收尾面是"过期租约 ∧ 业务行仍 running"两条谓词的联合，四行
	// 因此必须真带租约；会话各不相同，否则第二行起会被会话准入拒成 SESSION_BUSY。
	// Under US-24 a reclaimable row must carry a lease, and production always passes a
	// non-zero owner — so the fixture does too, one distinct session per row.
	owner := core.WorkerIdentity{InstanceID: "run-store-test", Name: "run-store-test"}
	seed := func(rec core.RunRecord) {
		t.Helper()
		_, err := store.StartRun(ctx, rec, core.RunResourceManifest{}, owner, core.CallerAdmission{}, rec.RunID)
		require.NoError(t, err)
	}

	// 两种终态各一行 + 一行仍 running + 一行无状态（空 session）仍 running。
	completed := startRow("cli")
	completed.RunID = "cli-1776000000123-comple"
	completed.SessionID = "session-completed"
	seed(completed)
	requireFinishRun(t, store, ctx, completed.RunID, core.StatusCompleted, "", 1, 0, fixedStart)

	cancelled := startRow("cli")
	cancelled.RunID = "cli-1776000000123-cancel"
	cancelled.SessionID = "session-cancelled"
	seed(cancelled)
	requireFinishRun(t, store, ctx, cancelled.RunID, core.StatusCancelled, "context canceled", 1, 0, fixedStart)

	stale := startRow("scheduler")
	stale.RunID = "scheduler-1776000000123-stale0"
	stale.SessionID = "session-stale"
	seed(stale)

	stateless := startRow("web")
	stateless.RunID = "web-1776000000123-statle"
	stateless.SessionID = ""
	seed(stateless)

	closedAt := fixedStart.Add(5 * time.Minute)
	// 四行租约一律推到过期：已终态两行若出现"租约还在所以没被领"的假绿，本步会把它
	// 变成真红——它们**只**能靠业务行状态躲开领取。
	_, err := db.pool.ExecContext(ctx,
		`UPDATE execution_leases SET expires_at = $1 WHERE kind = 'run'`,
		closedAt.Add(-time.Minute).UnixMilli())
	require.NoError(t, err)

	claim := func() []core.Lease {
		t.Helper()
		leases, err := NewReclaimerStore(db).ClaimExpired(ctx, core.LeaseRun,
			core.WorkerIdentity{InstanceID: "run-store-test", Name: "run-store-test"},
			64, time.Minute)
		require.NoError(t, err)
		return leases
	}
	leases := claim()
	claimed := make([]string, 0, len(leases))
	for _, lease := range leases {
		claimed = append(claimed, lease.ResourceID)
	}
	require.ElementsMatch(t, []string{stale.RunID, stateless.RunID}, claimed,
		"领取面只出仍为 running 的行（空会话的无状态运行也算），已终态的行不得被领取")

	for _, lease := range leases {
		report, err := RecoverRun(ctx, db, recTestLogger(t), lease.LeaseToken, closedAt)
		require.NoError(t, err)
		require.Equal(t, 1, report.ClosedRuns, "run_id=%s", lease.ResourceID)
	}

	// 已结束的两行**原样保留**（状态、原因、起始时刻都不动）。
	done := readRunRow(t, db, completed.RunID)
	require.Equal(t, string(core.StatusCompleted), done.Status, "已结束的状态不得被覆盖")
	require.False(t, done.Reason.Valid)
	require.Equal(t, fixedStart.UnixMilli(), done.FinishedAt.Int64, "已写下的终态时刻不得被改写")
	cancel := readRunRow(t, db, cancelled.RunID)
	require.Equal(t, string(core.StatusCancelled), cancel.Status)
	require.Equal(t, "context canceled", cancel.Reason.String, "已写下的原因不得被改写")

	// 仍 running 的两行被收尾：failed + 原因 + finished_at。
	for _, id := range []string{stale.RunID, stateless.RunID} {
		row := readRunRow(t, db, id)
		require.Equal(t, string(core.StatusFailed), row.Status, "run_id=%s", id)
		require.Equal(t, workerLostReason, row.Reason.String)
		require.Equal(t, closedAt.UnixMilli(), row.FinishedAt.Int64)
		require.Equal(t, fixedStart.UnixMilli(), row.StartedAt, "收尾不改起始时刻")
		require.Zero(t, row.Iterations, "收尾不伪造计数（跑到哪算到哪）")
	}

	// 幂等：连跑第二次零变化——收尾后的行已是终态，不再进入领取面。
	again := claim()
	require.Empty(t, again, "第二次收尾没有可领的行")
}

// TestRunStore_RunsTableHasNoFinalTextColumn（K4）**未移植**：旧用例用 `PRAGMA table_info(runs)`
// 自省列清单，属 SQLite 专有行为（移植口径第 3 条）。PG 侧的列清单由
// docs/contracts/schema.pg.sql 与 schema_contract_test.go（TestPostgresSchemaFile_MatchesContract）
// 覆盖；"runs 不含 final_text" 的契约面因此仍有判据，只是不在本文件重复。
// K4 is not ported: it introspects columns through PRAGMA, which is SQLite-specific; the
// PG column list is pinned by the schema contract test instead.

// TestRunStore_TimestampsAreUnixMilli —— K5：两处时间列都存 **Unix 毫秒**
// （迁移纪律 4——不是秒、不是纳秒）。
// TestRunStore_TimestampsAreUnixMilli pins both timestamp columns to Unix milliseconds.
func TestRunStore_TimestampsAreUnixMilli(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)

	start := time.Date(2026, 10, 16, 8, 30, 0, 987*int(time.Millisecond), time.UTC)
	rec := startRow("admin")
	rec.StartedAt = start
	require.NoError(t, startRunTest(store, ctx, rec, core.RunResourceManifest{}))

	finish := start.Add(1500 * time.Millisecond)
	requireFinishRun(t, store, ctx, rec.RunID, core.StatusFailed, "模型调用失败", 1, 0, finish)

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
	db := portTestDB(t)
	store := NewRunStore(db)

	for channel, want := range map[string]string{
		"cli":       triggerUser,
		"web":       triggerAPI,
		"scheduler": triggerCron,
		"admin":     triggerManual,
	} {
		rec := startRow(channel)
		require.NoError(t, startRunTest(store, ctx, rec, core.RunResourceManifest{}), "渠道 %s", channel)
		require.Equal(t, want, readRunRow(t, db, rec.RunID).Trigger, "渠道 %s 的 trigger", channel)
	}

	err := startRunTest(store, ctx, startRow("email"), core.RunResourceManifest{})
	require.Error(t, err, "未映射的渠道不得建行")
	require.Contains(t, err.Error(), "email", "错误必须点名渠道")

	var count int
	require.NoError(t, db.pool.QueryRowContext(ctx, `SELECT count(*) FROM runs`).Scan(&count))
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

// runStoreResourceUIDFor 按 `(kind, name)` 派生一个稳定的测试 UID：同名同种永远是同一实例。
// 本包内 resource_store_port_test.go 已有同义助手 `resourceUIDFor`（不同临时构建标签），
// 按移植口径加本文件前缀避免潜在重名。
// runStoreResourceUIDFor derives a stable test uid from kind and name; the prefix keeps it
// from colliding with the resource port file's identically-shaped helper.
func runStoreResourceUIDFor(kind core.ResourceKind, name string) core.ResourceUID {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + name))
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	payload := make([]byte, 26)
	for i := range payload {
		payload[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return core.ResourceUID(core.ResourceUIDPrefix + string(payload))
}

// runStoreModelVersion 造一条模型连接版本；版本号 0 表示让仓储自动取号。
// runStoreModelVersion builds a model_connection version; zero means auto-numbering.
func runStoreModelVersion(resourceID, contentHash, operationID string, versionNo int) core.ResourceVersion {
	return core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    runStoreResourceUIDFor(core.ResourceModelConnection, resourceID),
		ResourceID:     resourceID,
		VersionNo:      versionNo,
		ContentHash:    contentHash,
		DefinitionYAML: "name: " + resourceID + "\nmodel: gpt-test\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    operationID,
	}
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
		runStoreModelVersion("openai", "sha256:provider", "op-run-manifest", 0))
	require.NoError(t, err)
	browser, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceUID:    runStoreResourceUIDFor(core.ResourceMCPServer, "browser"),
		ResourceID:     "browser",
		ContentHash:    "sha256:mcp-browser",
		DefinitionYAML: "name: browser\ncommand: npx\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    "op-run-manifest",
	})
	require.NoError(t, err)
	filesystem, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceUID:    runStoreResourceUIDFor(core.ResourceMCPServer, "filesystem"),
		ResourceID:     "filesystem",
		ContentHash:    "sha256:mcp-filesystem",
		DefinitionYAML: "name: filesystem\ncommand: npx\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    "op-run-manifest",
	})
	require.NoError(t, err)
	skill, _, err := store.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  runStoreResourceUIDFor(core.ResourceSkill, "summarize"),
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
	err := db.pool.QueryRowContext(context.Background(),
		`SELECT format_version, manifest_hash, agent_lifecycle_state, snapshot_json, created_at
		   FROM run_resource_manifests WHERE run_id = $1`, runID).
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
	rows, err := db.pool.QueryContext(context.Background(),
		`SELECT resource_kind, resource_id, resource_uid, resource_name,
		        version_id, content_hash, released_at
		   FROM run_resource_refs WHERE run_id = $1
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
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM runs WHERE run_id = $1`, runID).Scan(&runs))
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = $1`, runID).Scan(&manifests))
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_refs WHERE run_id = $1`, runID).Scan(&refs))
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
	db := portTestDB(t)
	store := NewRunStore(db)

	rec := startRow("cli")
	require.NoError(t, startRunTest(store, ctx, rec, core.RunResourceManifest{}))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusRunning), row.Status)

	var manifests, refs int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = $1`, rec.RunID).Scan(&manifests))
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_refs WHERE run_id = $1`, rec.RunID).Scan(&refs))
	require.Zero(t, manifests, "零清单不得写出清单行（假账会被读成事实）")
	require.Zero(t, refs, "零清单不得写出资源引用")
}

// TestRunStore_StartRunWritesRunManifestAndRefs —— 约束 18 的正向判据：
// 运行行、清单行与全部引用一次落库；清单哈希可被读侧重算复现；引用一律未释放。
// TestRunStore_StartRunWritesRunManifestAndRefs pins the single-transaction
// admission write: run row, manifest row and every reference commit together.
func TestRunStore_StartRunWritesRunManifestAndRefs(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	rec := startRow("web")
	require.NoError(t, startRunTest(store, ctx, rec, fx.manifest))

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
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	broken := fx.manifest
	broken.Provider.VersionID = 999999 // 不存在的版本行
	rehashManifest(t, &broken)         // 内容合法、哈希自洽——失败只能来自外键

	rec := startRow("cli")
	err := startRunTest(store, ctx, rec, broken)
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
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	broken := fx.manifest
	broken.MCPServers[0].ResourceUID = ""
	rehashManifest(t, &broken)

	rec := startRow("cli")
	err := startRunTest(store, ctx, rec, broken)
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
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	broken := fx.manifest
	broken.AgentUID = ""
	rehashManifest(t, &broken)

	rec := startRow("cli")
	err := startRunTest(store, ctx, rec, broken)
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
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	tampered := fx.manifest
	tampered.Skills[0].Files[0].SHA256 = "sha256:tampered" // 故意不重算哈希

	rec := startRow("cli")
	err := startRunTest(store, ctx, rec, tampered)
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
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	disabled, err := core.BuildRunResourceManifest(testAgentUID, core.AgentDisabled, fx.resolved)
	require.NoError(t, err, "构造器允许冻结非启用态——拒绝发生在写侧")

	rec := startRow("cli")
	err = startRunTest(store, ctx, rec, disabled)
	require.Error(t, err)
	require.Contains(t, err.Error(), "生命周期状态必须是 active")
	requireNoRunTrace(t, db, rec.RunID)
}

// TestRunStore_ReleaseRunRefsScopedToRun —— 约束 19 的正向判据（US-22 C-05 拆分后）：
// `FinishRun` 只写终态、**不**释放引用；`ReleaseRunRefs` 按 run_id 收窄释放，共用同一批
// 版本的另一个 Run 不受波及。
// TestRunStore_ReleaseRunRefsScopedToRun pins the split: FinishRun writes only the
// terminal state, and ReleaseRunRefs releases exactly the finishing run's refs.
func TestRunStore_ReleaseRunRefsScopedToRun(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	rec := startRow("web")
	require.NoError(t, startRunTest(store, ctx, rec, fx.manifest))
	other := startRow("web")
	other.RunID = "web-1776000000123-other1"
	require.NoError(t, startRunTest(store, ctx, other, fx.manifest),
		"同一批版本可被多个 Run 同时引用（引用表主键是 run_id + 资源）")

	finished := fixedStart.Add(2 * time.Minute)
	require.True(t, requireFinishRun(t, store, ctx, rec.RunID, core.StatusCompleted, "", 2, 1, finished))

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusCompleted), row.Status)
	require.Equal(t, finished.UnixMilli(), row.FinishedAt.Int64)
	for _, ref := range readRunRefRows(t, db, rec.RunID) {
		require.False(t, ref.ReleasedAt.Valid, "终态写回不再连带释放：%s/%s", ref.Kind, ref.ResourceID)
	}

	require.NoError(t, store.ReleaseRunRefs(ctx, rec.RunID, finished))

	released := readRunRefRows(t, db, rec.RunID)
	require.Len(t, released, 4)
	for _, ref := range released {
		require.True(t, ref.ReleasedAt.Valid, "终态的 Run 不得留着未释放引用（%s/%s）",
			ref.Kind, ref.ResourceID)
		require.Equal(t, finished.UnixMilli(), ref.ReleasedAt.Int64, "释放时刻取调用方给的时刻")
	}

	for _, ref := range readRunRefRows(t, db, other.RunID) {
		require.False(t, ref.ReleasedAt.Valid,
			"释放只属于本次收尾的 Run——%s/%s 不得被连带释放", ref.Kind, ref.ResourceID)
	}
}

// TestUS22_ReleaseRunRefsSkipsWhileRunning —— C-05 的守卫判据：终态未持久化（`FinishRun`
// 写失败 / 未提交）时 `ReleaseRunRefs` 不释放引用，留给启动恢复；行不存在同样是无操作。
// TestUS22_ReleaseRunRefsSkipsWhileRunning pins the status guard: while the run is still
// running (or absent) the release is a no-op, leaving refs to startup recovery.
func TestUS22_ReleaseRunRefsSkipsWhileRunning(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	rec := startRow("cli")
	require.NoError(t, startRunTest(store, ctx, rec, fx.manifest))

	// ① 仍在运行：释放是空操作，引用原样保留（钩子还要用凭据）。
	require.NoError(t, store.ReleaseRunRefs(ctx, rec.RunID, fixedStart.Add(time.Minute)))
	for _, ref := range readRunRefRows(t, db, rec.RunID) {
		require.False(t, ref.ReleasedAt.Valid,
			"running 的 Run 不得释放引用：%s/%s", ref.Kind, ref.ResourceID)
	}

	// ② 行不存在：无引用可释放，返回 nil（收尾 defer 在 FinishRun 失败后照常调用）。
	require.NoError(t, store.ReleaseRunRefs(ctx, "missing-run-id", fixedStart.Add(time.Minute)))

	// ③ 终态落库后才释放。
	finished := fixedStart.Add(2 * time.Minute)
	require.True(t, requireFinishRun(t, store, ctx, rec.RunID, core.StatusCompleted, "", 1, 0, finished))
	require.NoError(t, store.ReleaseRunRefs(ctx, rec.RunID, finished))
	for _, ref := range readRunRefRows(t, db, rec.RunID) {
		require.True(t, ref.ReleasedAt.Valid,
			"终态后的释放必须生效：%s/%s", ref.Kind, ref.ResourceID)
	}
}

// TestUS22_FinishRunKeepsTerminalWhenReleaseFails —— US-22 C-05 的反向判据：终态与释放
// 已拆分，释放失败**不得**回滚已判定的终态；运行保持终态、引用留待启动恢复补齐。
// TestUS22_FinishRunKeepsTerminalWhenReleaseFails proves a failed release never rolls back
// the terminal state; startup recovery backfills the residual refs.
func TestUS22_FinishRunKeepsTerminalWhenReleaseFails(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	rec := startRow("cli")
	require.NoError(t, startRunTest(store, ctx, rec, fx.manifest))

	// SQLite 谱系用 `CREATE TRIGGER ... RAISE(ABORT, ...)`；PG 的等价物是触发器函数
	// 内 `RAISE EXCEPTION`。注入点相同：UPDATE run_resource_refs 时失败。
	// The SQLite lineage raises inside a trigger; PG does the same through a trigger
	// function that raises an exception on UPDATE of the reference table.
	_, err := db.pool.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION run_store_fail_ref_release() RETURNS trigger
		LANGUAGE plpgsql AS $fn$
		BEGIN
			RAISE EXCEPTION '注入的引用释放失败';
		END
		$fn$`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx, `
		CREATE TRIGGER fail_run_ref_release
			BEFORE UPDATE ON run_resource_refs
			FOR EACH ROW EXECUTE FUNCTION run_store_fail_ref_release()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.pool.ExecContext(context.Background(),
			`DROP TRIGGER IF EXISTS fail_run_ref_release ON run_resource_refs`)
		require.NoError(t, err)
		_, err = db.pool.ExecContext(context.Background(),
			`DROP FUNCTION IF EXISTS run_store_fail_ref_release()`)
		require.NoError(t, err)
	})

	finished := fixedStart.Add(time.Minute)
	applied, err := store.FinishRun(ctx, rec.RunID, core.LeaseToken{}, core.StatusCompleted, "", 1, 0, finished)
	require.NoError(t, err, "释放失败不得让终态写回失败")
	require.True(t, applied)

	err = store.ReleaseRunRefs(ctx, rec.RunID, finished)
	require.Error(t, err, "引用释放失败必须如实上抛")
	require.Contains(t, err.Error(), "释放运行资源引用")

	row := readRunRow(t, db, rec.RunID)
	require.Equal(t, string(core.StatusCompleted), row.Status, "已判定的终态不得因释放失败被回滚")
	require.Equal(t, finished.UnixMilli(), row.FinishedAt.Int64)
	for _, ref := range readRunRefRows(t, db, rec.RunID) {
		require.False(t, ref.ReleasedAt.Valid, "释放失败后引用仍未释放（待启动恢复）：%s/%s",
			ref.Kind, ref.ResourceID)
	}
}

// TestRunStore_FinishRunDoesNotOverwriteTerminalState —— K7（约束 19）：同一行的第二次
// Finish 是幂等成功——不覆盖已判定的终态、不改原因与计数（注 8）。
// TestRunStore_FinishRunDoesNotOverwriteTerminalState: a second Finish is idempotent
// success and never re-judges the terminal row.
func TestRunStore_FinishRunDoesNotOverwriteTerminalState(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)

	rec := startRow("web")
	require.NoError(t, startRunTest(store, ctx, rec, core.RunResourceManifest{}))
	first := fixedStart.Add(90 * time.Second)
	require.True(t, requireFinishRun(t, store, ctx, rec.RunID, core.StatusCompleted, "", 2, 1, first))

	// 迟到的失败写（并发收尾 / 重复取消）：必须有守卫，且按幂等**未转换**返回
	// （US-22：applied=false 是抽取钩子跳过的依据）。
	late := first.Add(30 * time.Second)
	require.False(t, requireFinishRun(t, store, ctx, rec.RunID, core.StatusCancelled, "迟到的取消", 9, 9, late),
		"已终态的行 → 幂等未转换，不报错也不覆盖")

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
	db := portTestDB(t)
	store := NewRunStore(db)

	_, err := store.FinishRun(ctx, "web-1776000000123-ghost", core.LeaseToken{}, core.StatusFailed, "", 0, 0, fixedStart)
	require.Error(t, err)
	require.Contains(t, err.Error(), "账本行不存在")
}
