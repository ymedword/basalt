package app

// 本文件是 v3 迁移编排的验收面（颗粒度文档 R4.10 / T119，3 例）：步骤顺序与完成守卫、
// 准入门 fail-closed（预写失败零写入），以及绑定回填的身份连续性判据。
// 夹具用**真实** v2 → v3 通道（合成 v1 建表 + 真实 0002 / 0003 迁移文件），不手搓 v3
// 库——编排要验的是"旧库被正确接上"，绕过迁移就证明不了。
// This file covers the v3 migration orchestration: step order, the fail-closed gate,
// and the identity-continuity proof rule for binding backfill. The fixture builds a
// genuine v2 database and lets the real 0003 migration carry it forward.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// migrateV3SyntheticV1 是合成 v1 建表脚本：只需版本表，其余表由
// migrateV3V1Tables 按 v3 迁移真实触及的 v1 形状补齐。
// migrateV3SyntheticV1 is the minimal v1 script; the touched tables follow.
const migrateV3SyntheticV1 = "CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL, description TEXT NOT NULL);\n"

// migrateV3V1Tables 是 0003 / 0004 会读 / 重建 / 加列的 v1 期表（与 storage 侧夹具
// 同一形状）：合成 v1 只带版本表，前向脚本必须看到真实列才能证明重建与加列本身。
// migrateV3V1Tables recreates the v1-era tables the 0003 / 0004 migrations touch.
const migrateV3V1Tables = `
CREATE TABLE runs (
    run_id TEXT PRIMARY KEY, session_id TEXT, agent TEXT NOT NULL, channel TEXT NOT NULL,
    user_id TEXT NOT NULL, trigger TEXT NOT NULL, status TEXT NOT NULL, reason TEXT,
    iterations INTEGER NOT NULL DEFAULT 0, tool_calls INTEGER NOT NULL DEFAULT 0,
    config_hash TEXT, started_at INTEGER NOT NULL, finished_at INTEGER
);
CREATE TABLE tool_invocations (
    id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, session_id TEXT,
    tool_call_id TEXT, tool_name TEXT NOT NULL, input_json TEXT, result_json TEXT,
    status TEXT NOT NULL, error_message TEXT, error_kind TEXT, duration_ms INTEGER,
    started_at INTEGER NOT NULL, finished_at INTEGER
);
CREATE TABLE notify_channels (
    name TEXT PRIMARY KEY, type TEXT NOT NULL, url TEXT NOT NULL, description TEXT,
    created_at INTEGER NOT NULL
);
CREATE TABLE scheduled_tasks (
    task_id TEXT PRIMARY KEY, agent_name TEXT NOT NULL, cron TEXT NOT NULL, zone TEXT NOT NULL,
    message TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, definition_hash TEXT NOT NULL,
    last_run_at INTEGER, last_status TEXT, retired_at INTEGER,
    created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);`

// migrateV3V2FS 组装「合成 v1 + 真实 0002」的迁移集：到 v2 为止全靠它，v3 由编排层
// 自己调 (*DB).Migrate 走真实 0003。
// migrateV3V2FS builds the synthetic v1 plus the real 0002 migration.
func migrateV3V2FS(t *testing.T) fstest.MapFS {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "storage", "migrations", "0002_us11_us12_resources.sql"))
	require.NoError(t, err)
	return fstest.MapFS{
		"schema.sql": &fstest.MapFile{Data: []byte(migrateV3SyntheticV1 + migrateV3V1Tables)},
		"migrations/0002_us11_us12_resources.sql": &fstest.MapFile{Data: script},
	}
}

// migrateV3StateFile 是 v1 状态文件的落盘形态：字段声明顺序即契约顺序，`agent_uid`
// 在 v1 里**不存在**（v2 起才有）。用例自己算校验和——期望值不调被测实现。
// migrateV3StateFile is the v1 on-disk shape; the test computes the checksum itself.
type migrateV3StateFile struct {
	SchemaVersion int    `json:"schema_version"`
	Agent         string `json:"agent"`
	State         string `json:"state"`
	Generation    int64  `json:"generation"`
	UpdatedAt     int64  `json:"updated_at"`
	OperationID   string `json:"operation_id"`
	Checksum      string `json:"checksum"`
}

// writeMigrateV3StateFile 在临时工作区落一份**逐字节规范**的 v1 状态文件。
// writeMigrateV3StateFile writes a canonical v1 state file.
func writeMigrateV3StateFile(t *testing.T, root, agent string) {
	t.Helper()
	rec := migrateV3StateFile{
		SchemaVersion: 1,
		Agent:         agent,
		State:         string(core.AgentActive),
		Generation:    2,
		UpdatedAt:     1760000000000,
		OperationID:   "op-1760000000000-aaaaaa",
	}
	covered, err := json.Marshal(rec) // checksum 为空串时即被哈希的字节
	require.NoError(t, err)
	sum := sha256.Sum256(covered)
	rec.Checksum = "sha256:" + hex.EncodeToString(sum[:])
	final, err := json.Marshal(rec)
	require.NoError(t, err)
	writeWorkspaceFile(t, root, "control/agents/"+agent+".json", string(final))
}

// migrateV3AgentMD 是待迁移 Agent 的定义文件：引用了 provider `openai` 与通知渠道
// `alerts`，但**没有** `resource_bindings` 块（迁移前形态）。
// migrateV3AgentMD is the pre-migration definition: two references, no bindings block.
func migrateV3AgentMD(name string) string {
	return "---\nname: " + name +
		"\ndescription: 迁移用例\nprovider:\n  name: openai\n  model: gpt-test\n" +
		"notify_channels:\n  - alerts\n---\n\n你是 " + name + "。\n"
}

// migrateV3Env 是迁移编排夹具：真实 v2 库 + 临时工作区 + 各仓储。
// migrateV3Env is the orchestration fixture.
type migrateV3Env struct {
	deps   MigrateV3Deps
	root   string
	db     *storage.DB
	ops    *storage.ManagementStore
	notify *storage.NotifyAdminStore
	saved  int64 // AGENT.md 的修改时间（毫秒），删除证据比较的基准
}

// newMigrateV3Env 建夹具：v2 库 + 一个已发布的模型连接 + 一个通知渠道 + 一个 Agent。
// providerPublished=false 时只有定义、没有指针（"从未发布"分支）。
// newMigrateV3Env builds a genuine v2 database with one agent and its two references.
func newMigrateV3Env(t *testing.T, opts ...migrateV3Option) *migrateV3Env {
	t.Helper()
	ctx := context.Background()

	cfg := migrateV3Config{
		providerPublished: true,
		channelPresent:    true,
		stateFile:         true,
		agentMD:           migrateV3AgentMD("reporter"),
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	dbPath := filepath.Join(root, databaseFileName)
	bare, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	// 新库先落到 v1（整份 schema.sql 建 v1 形态），再逐版前向到 v2——一次 target=2
	// 会走"新库直接建到目标版本"，绕过 0002。
	require.NoError(t, storage.Migrate(ctx, bare, migrateV3V2FS(t), 1))
	require.NoError(t, storage.Migrate(ctx, bare, migrateV3V2FS(t), 2))

	// 迁移前的 v2 形状：已发布的模型连接 openai（两版）、一个通知渠道、一条基线操作。
	const insertOp = `INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES (?, ?, ?, ?, 'admin', 'bearer', 'api', 'success', ?)`
	_, err = bare.Exec(insertOp, "op-1760000000000-aaaa", "system.baseline_v2", "system", "system", 100)
	require.NoError(t, err)
	_, err = bare.Exec(insertOp, "op-1760000100000-pub000", "model_connection.publish", "model_connection", "openai", 200)
	require.NoError(t, err)

	const insertVersion = `INSERT INTO resource_versions
		(id, resource_kind, resource_id, version_no, content_hash, definition_yaml, manifest_json, storage_path, created_at, operation_id)
		VALUES (?, 'model_connection', 'openai', ?, ?, 'name: openai', NULL, NULL, ?, 'op-1760000100000-pub000')`

	if cfg.providerPublished {
		// 版本只随发布产生：没有指针就没有版本行（否则 0003 取不到身份，建表即失败）。
		_, err = bare.Exec(insertVersion, 1, 1, "hash-v1", 200)
		require.NoError(t, err)
		_, err = bare.Exec(insertVersion, 2, 2, "hash-v2", 300)
		require.NoError(t, err)
		_, err = bare.Exec(`INSERT INTO resource_pointers
			(resource_kind, resource_id, published_version_id, applied_version_id, deleted_at, updated_at, operation_id)
			VALUES ('model_connection', 'openai', 2, 2, NULL, 300, 'op-1760000100000-pub000')`)
		require.NoError(t, err)
	}
	if cfg.channelPresent {
		_, err = bare.Exec(`INSERT INTO notify_channels (name, type, url, description, created_at)
			VALUES ('alerts', 'webhook', '${BASALT_ALERT_URL}', '值班渠', 400)`)
		require.NoError(t, err)
	}
	require.NoError(t, bare.Close())

	writeWorkspaceFile(t, root, "agents/reporter/AGENT.md", cfg.agentMD)

	// 旧库不迁移：OpenWritable 只装配句柄，前向迁移是编排层的显式动作。
	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	version, err := db.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, version, "夹具必须停在 v2")

	env := &migrateV3Env{
		root:   root,
		db:     db,
		ops:    storage.NewManagementStore(db),
		notify: storage.NewNotifyAdminStore(db),
		deps: MigrateV3Deps{
			DB:         db,
			Operations: storage.NewManagementStore(db),
			Lifecycle:  workspace.NewLifecycleStore(root, baselineClock),
			Resources:  storage.NewResourceStore(db),
			Notify:     storage.NewNotifyAdminStore(db),
			Bindings:   storage.NewBindingStore(db),
			Root:       root,
			Agents:     []string{"reporter"},
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			Now:        baselineClock,
		},
	}

	if cfg.stateFile {
		writeMigrateV3StateFile(t, root, "reporter")
	}
	// 定义文件的修改时间固定下来：删除证据的比较基准必须是可控读数。
	env.saved = baselineClock().Add(-time.Hour).UnixMilli()
	require.NoError(t, os.Chtimes(
		filepath.Join(root, "agents", "reporter", "AGENT.md"),
		time.UnixMilli(env.saved), time.UnixMilli(env.saved)))
	return env
}

// migrateV3Config 是夹具开关：覆盖"从未发布 / 渠道缺失 / 无状态文件"三条边界。
// migrateV3Config toggles the fixture's boundary conditions.
type migrateV3Config struct {
	providerPublished bool
	channelPresent    bool
	stateFile         bool
	agentMD           string
}

// migrateV3Option 是夹具开关的函数式写法。
// migrateV3Option applies one toggle.
type migrateV3Option func(*migrateV3Config)

// withoutProvider 关掉"资源已发布"这条前置（从未发布分支）。
func withoutProvider() migrateV3Option {
	return func(c *migrateV3Config) { c.providerPublished = false }
}

func withAgentMD(content string) migrateV3Option {
	return func(c *migrateV3Config) { c.agentMD = content }
}

// requireMigrateV3Operation 取指定动作的唯一操作行（多条直接失败，免得断言挑花眼）。
// requireMigrateV3Operation returns the single row of an action.
func requireMigrateV3Operation(t *testing.T, env *migrateV3Env, action string) core.OperationRecord {
	t.Helper()
	rows, err := env.ops.ListOperations(context.Background(), core.OperationFilter{Action: action, Limit: 50})
	require.NoError(t, err)
	require.Len(t, rows, 1, "动作 %s 应当恰好一条操作行", action)
	return rows[0]
}

// resetMigrateV3DB 让夹具退回 v2：用例要在同一进程里验"失败后库不动"，而迁移一旦
// 成功库就前进到 v3——重建一份同样的 v2 库比回滚真实迁移更诚实。
// resetMigrateV3DB rebuilds the fixture's database at v2.
func resetMigrateV3DB(t *testing.T, env *migrateV3Env) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, env.db.Close())

	dbPath := filepath.Join(env.root, databaseFileName)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		removeErr := os.Remove(dbPath + suffix)
		if removeErr != nil {
			require.ErrorIs(t, removeErr, os.ErrNotExist, "清理旧库文件 %s", dbPath+suffix)
		}
	}
	bare, err := sql.Open("sqlite", "file:"+dbPath)
	require.NoError(t, err)
	require.NoError(t, storage.Migrate(ctx, bare, migrateV3V2FS(t), 1))
	require.NoError(t, storage.Migrate(ctx, bare, migrateV3V2FS(t), 2))
	_, err = bare.Exec(`INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES ('op-1760000000000-aaaaaa', 'system.baseline_v2', 'system', 'system', 'admin', 'bearer', 'api', 'success', 100)`)
	require.NoError(t, err)
	_, err = bare.Exec(`INSERT INTO management_operations
		(operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		VALUES ('op-1760000100000-pub000', 'model_connection.publish', 'model_connection', 'openai', 'admin', 'bearer', 'api', 'success', 200)`)
	require.NoError(t, err)
	_, err = bare.Exec(`INSERT INTO resource_versions
		(id, resource_kind, resource_id, version_no, content_hash, definition_yaml, manifest_json, storage_path, created_at, operation_id)
		VALUES (1, 'model_connection', 'openai', 1, 'hash-v1', 'name: openai', NULL, NULL, 200, 'op-1760000100000-pub000')`)
	require.NoError(t, err)
	_, err = bare.Exec(`INSERT INTO resource_versions
		(id, resource_kind, resource_id, version_no, content_hash, definition_yaml, manifest_json, storage_path, created_at, operation_id)
		VALUES (2, 'model_connection', 'openai', 2, 'hash-v2', 'name: openai', NULL, NULL, 300, 'op-1760000100000-pub000')`)
	require.NoError(t, err)
	_, err = bare.Exec(`INSERT INTO resource_pointers
		(resource_kind, resource_id, published_version_id, applied_version_id, deleted_at, updated_at, operation_id)
		VALUES ('model_connection', 'openai', 2, 2, NULL, 300, 'op-1760000100000-pub000')`)
	require.NoError(t, err)
	_, err = bare.Exec(`INSERT INTO notify_channels (name, type, url, description, created_at)
		VALUES ('alerts', 'webhook', '${BASALT_ALERT_URL}', '值班渠', 400)`)
	require.NoError(t, err)
	require.NoError(t, bare.Close())

	db, err := storage.OpenWritable(ctx, dbPath, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	env.db = db
	env.ops = storage.NewManagementStore(db)
	env.notify = storage.NewNotifyAdminStore(db)
	env.deps.DB = db
	env.deps.Operations = env.ops
	env.deps.Resources = storage.NewResourceStore(db)
	env.deps.Notify = env.notify
	env.deps.Bindings = storage.NewBindingStore(db)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
}

// TestMigrateV3_StepOrder 验六步顺序：状态文件先升 v2、库再前进到 v3、身份回填在加列
// 之后、绑定回填写进文件并以文件为准重建投影、完成守卫写 success 与报告。
// TestMigrateV3_StepOrder walks the six steps end to end.
func TestMigrateV3_StepOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMigrateV3Env(t)

	report, err := RunMigrateV3(ctx, env.deps)
	require.NoError(t, err)

	// ③ 库前进到目标版本（0003 → 0004 一并落地），操作行以 success + OK 收口。
	version, err := env.db.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, storage.SupportedSchemaVersion, version)
	op := requireMigrateV3Operation(t, env, actionBaselineV3)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, migrateV3ResultOK, op.ResultCode)
	require.Equal(t, core.ActorSubjectMigration, op.ActorSubject)
	require.Equal(t, core.AuthMethodMigration, op.AuthMethod)

	// ① 预写行在库还是 v2 时就得存在——0003 的身份行引用它；这里靠"迁移成功"反证，
	// 并在报告里核对步骤计数。
	require.Equal(t, 2, report.FromVersion)
	require.Equal(t, storage.SupportedSchemaVersion, report.ToVersion)
	require.Equal(t, 1, report.StateFilesUpgraded, "v1 状态文件必须被升级")
	require.Equal(t, 1, report.AgentUIDsMirrored)
	require.Equal(t, 1, report.BindingsWritten)
	require.Equal(t, 1, report.ProjectionsRebuilt)

	// ② 状态文件成为 v2 并带上稳定身份；数据库镜像与文件一致。
	lifecycle := workspace.NewLifecycleStore(env.root, baselineClock)
	rec, exists, err := lifecycle.Read(ctx, "reporter")
	require.NoError(t, err)
	require.True(t, exists)
	require.True(t, rec.AgentUID.Valid(), "升级后的状态文件必须携带 agent_uid")
	require.Equal(t, string(core.AgentActive), string(rec.State))
	require.EqualValues(t, 2, rec.Generation, "升级只刷新身份与归属，不改状态与代数")
	require.Equal(t, op.OperationID, rec.OperationID, "升级必须把本次迁移操作记进状态文件")

	stored, source, found, err := env.deps.Resources.AgentIdentity(ctx, "reporter")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, rec.AgentUID, stored, "数据库镜像必须与状态文件同源")
	require.Equal(t, "state_file", source)

	// ⑤ 绑定块的 UID 就是迁移后指针里的 UID（不按名猜，也不复制别的读数）。
	ptr, err := env.deps.Resources.Pointer(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	channel, err := env.notify.Get(ctx, "alerts")
	require.NoError(t, err)

	view, err := workspace.InspectAgentForMigration(env.root, "reporter")
	require.NoError(t, err)
	require.Equal(t, []core.ResourceBinding{{Name: "openai", UID: ptr.ResourceUID}}, view.Bindings.ModelConnections)
	require.Equal(t, []core.ResourceBinding{{Name: "alerts", UID: channel.ResourceUID}}, view.Bindings.NotifyChannels)

	// ⑥ 投影以文件为准重建：查得到的就是文件里那一组。
	projected, err := env.deps.Bindings.Bindings(ctx, rec.AgentUID)
	require.NoError(t, err)
	require.Len(t, projected, 2)

	require.Empty(t, report.Skips, "可证明的引用不得记跳过")
}

// TestMigrateV3_AlreadyV3OnlyMigratesSchema 验 v3 → v4 的升级只动 schema：数据面在
// v3 已完成，不重跑状态文件 / 身份 / 绑定回填，也不写 `system.baseline_v3` 操作行。
// An already-v3 database only moves its schema forward; the data steps do not rerun.
func TestMigrateV3_AlreadyV3OnlyMigratesSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMigrateV3Env(t)

	// 先以 Target=3 真实跑一遍六步编排，把夹具带到「数据编排已完成」的 v3 库。
	v3Deps := env.deps
	v3Deps.Target = migrateV3DataVersion
	_, err := RunMigrateV3(ctx, v3Deps)
	require.NoError(t, err)

	definitionBefore, err := os.ReadFile(filepath.Join(env.root, "agents", "reporter", "AGENT.md"))
	require.NoError(t, err)
	stateBefore, err := os.ReadFile(filepath.Join(env.root, "control", "agents", "reporter.json"))
	require.NoError(t, err)

	report, err := RunMigrateV3(ctx, env.deps)
	require.NoError(t, err)
	require.Equal(t, migrateV3DataVersion, report.FromVersion)
	require.Equal(t, storage.SupportedSchemaVersion, report.ToVersion)

	version, err := env.db.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, storage.SupportedSchemaVersion, version, "schema 必须前向到目标版本")

	definitionAfter, err := os.ReadFile(filepath.Join(env.root, "agents", "reporter", "AGENT.md"))
	require.NoError(t, err)
	require.Equal(t, definitionBefore, definitionAfter, "已在 v3 的库不得重写定义文件")
	stateAfter, err := os.ReadFile(filepath.Join(env.root, "control", "agents", "reporter.json"))
	require.NoError(t, err)
	require.Equal(t, stateBefore, stateAfter, "已在 v3 的库不得重写状态文件")

	// 到达 v3 的那次编排各写一条；v3 → v4 不得再写第二条。
	requireMigrateV3Operation(t, env, actionBaselineV3)
}

// TestMigrateV3_BaselinePendingFailureWritesNothing 验准入门 fail-closed：预写失败 →
// 状态文件仍是 v1、定义文件逐字节不变、库仍是 v2、没有任何迁移操作行。
// TestMigrateV3_BaselinePendingFailureWritesNothing proves the gate writes nothing.
func TestMigrateV3_BaselinePendingFailureWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newMigrateV3Env(t)

	stateBefore, err := os.ReadFile(filepath.Join(env.root, "control", "agents", "reporter.json"))
	require.NoError(t, err)
	agentBefore, err := os.ReadFile(filepath.Join(env.root, "agents", "reporter", "AGENT.md"))
	require.NoError(t, err)

	env.deps.Operations = &failingOperationStore{OperationStore: env.ops}

	_, err = RunMigrateV3(ctx, env.deps)
	require.Error(t, err)
	require.Contains(t, err.Error(), "预写操作记录失败")

	// 零写入：文件逐字节一致，库停在 v2，操作台账里没有成功的迁移行。
	stateAfter, err := os.ReadFile(filepath.Join(env.root, "control", "agents", "reporter.json"))
	require.NoError(t, err)
	require.Equal(t, stateBefore, stateAfter, "预写失败不得改动状态文件")
	agentAfter, err := os.ReadFile(filepath.Join(env.root, "agents", "reporter", "AGENT.md"))
	require.NoError(t, err)
	require.Equal(t, agentBefore, agentAfter, "预写失败不得改动定义文件")

	version, err := env.db.SchemaVersion(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, version, "预写失败不得迁移")

	rows, err := env.ops.ListOperations(ctx, core.OperationFilter{Action: actionBaselineV3, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, rows, "预写失败不得留下操作行")
}

// TestMigrateV3_BindingBackfillProofRule 逐条验"证明不了就不写"：从未发布、最后保存
// 之后删除、文件里已有不同绑定、已有相同绑定（重跑幂等）四种情形。
// TestMigrateV3_BindingBackfillProofRule walks the four unprovable/duplicate cases.
func TestMigrateV3_BindingBackfillProofRule(t *testing.T) {
	t.Parallel()

	t.Run("never_published", func(t *testing.T) {
		t.Parallel()
		env := newMigrateV3Env(t, withoutProvider())
		report, err := RunMigrateV3(context.Background(), env.deps)
		require.NoError(t, err)

		require.Contains(t, skipReasons(report), migrateSkipNeverPublished)

		view, err := workspace.InspectAgentForMigration(env.root, "reporter")
		require.NoError(t, err)
		require.Empty(t, view.Bindings.ModelConnections, "不可证明的引用不得写入")
		// 通知渠道照样能证明 → 只写它。
		require.Len(t, view.Bindings.NotifyChannels, 1)
	})

	t.Run("delete_after_save", func(t *testing.T) {
		t.Parallel()
		env := newMigrateV3Env(t)
		// 定义文件最后保存之后发生过一次删除：同名重建与改配置无法区分。
		ctx := context.Background()
		deletedAt := time.UnixMilli(env.saved + 1000)
		err := env.ops.BeginOperation(ctx, core.OperationRecord{
			OperationID:  "op-1760000300000-del000",
			Action:       core.ActionModelConnectionDelete,
			TargetKind:   core.TargetKindModelConnection,
			TargetID:     "openai",
			ActorSubject: "admin",
			AuthMethod:   core.AuthMethodBearer,
			Source:       "api",
			Status:       core.OperationPending,
			CreatedAt:    deletedAt,
		})
		require.NoError(t, err)
		require.NoError(t, env.ops.FinishOperation(ctx, "op-1760000300000-del000",
			core.OperationSuccess, "OK", "", nil, deletedAt))

		report, err := RunMigrateV3(context.Background(), env.deps)
		require.NoError(t, err)
		require.Contains(t, skipReasons(report), migrateSkipDeleteAfterSave)

		view, err := workspace.InspectAgentForMigration(env.root, "reporter")
		require.NoError(t, err)
		require.Empty(t, view.Bindings.ModelConnections)
	})

	t.Run("binding_conflict", func(t *testing.T) {
		t.Parallel()
		// 文件里已有一组**不同**的绑定：重跑时不得覆盖，记冲突。
		env := newMigrateV3Env(t, withAgentMD(
			"---\nname: reporter\ndescription: 迁移用例\nprovider:\n  name: openai\n  model: gpt-test\n"+
				"resource_bindings:\n  model_connection:\n    - name: openai\n      uid: rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A\n"+
				"  notify_channel: []\n---\n\n你是 reporter。\n"))

		report, err := RunMigrateV3(context.Background(), env.deps)
		require.NoError(t, err)
		require.Contains(t, skipReasons(report), migrateSkipBindingConflict)

		view, err := workspace.InspectAgentForMigration(env.root, "reporter")
		require.NoError(t, err)
		require.Equal(t, "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", view.Bindings.ModelConnections[0].UID.String(),
			"已有绑定必须原样保留")
	})

	t.Run("idempotent_rerun", func(t *testing.T) {
		t.Parallel()
		env := newMigrateV3Env(t)

		first, err := RunMigrateV3(context.Background(), env.deps)
		require.NoError(t, err)
		require.Equal(t, 1, first.BindingsWritten)

		afterFirst, err := os.ReadFile(filepath.Join(env.root, "agents", "reporter", "AGENT.md"))
		require.NoError(t, err)

		// 第二次跑在新库上不叫重跑（库已是 v3 → 直接跳过），所以退回 v2 再走一遍：
		// 文件里已是同一组绑定 → 不写（Changed=false）。
		resetMigrateV3DB(t, env)
		second, err := RunMigrateV3(context.Background(), env.deps)
		require.NoError(t, err)
		require.Zero(t, second.BindingsWritten, "同一组绑定不得二次落盘")
		require.Equal(t, 1, second.BindingsUnchanged)

		afterSecond, err := os.ReadFile(filepath.Join(env.root, "agents", "reporter", "AGENT.md"))
		require.NoError(t, err)
		require.Equal(t, afterFirst, afterSecond, "重跑必须逐字节不变")
	})
}

// failingOperationStore 让预写失败：只有 BeginOperation 被替换，读路径仍是真实现。
// failingOperationStore fails only the pre-write.
type failingOperationStore struct {
	core.OperationStore
}

// BeginOperation 恒失败——准入门要验的正是这一条。
func (s *failingOperationStore) BeginOperation(context.Context, core.OperationRecord) error {
	return errors.New("注入：台账不可写")
}

// skipReasons 收集报告里的跳过原因（断言只看原因集合，不依赖顺序）。
// skipReasons collects the skip reasons of a report.
func skipReasons(report MigrateV3Report) []string {
	out := make([]string, 0, len(report.Skips))
	for _, skip := range report.Skips {
		out = append(out, skip.Reason)
	}
	return out
}
