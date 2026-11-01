//go:build integration

package postgres

// 本文件是调度两表（scheduled_tasks / task_executions）仓储的专属判卷面：
// 14 个函数 / 38 例（docs/us/us10-scheduler.md §3.2.2）。夹具与读数由本文件自持，
// 生产写入路径仍唯一经仓储；直插行只用于造出"仓储 API 造不出来"的既成状态
// （运行痕迹、退休行、崩溃遗留的 started 行）。
//
// 由 SQLite 谱系 internal/storage/task_store_test.go 逐条移植：测试名、断言、中文验收点
// 与逆改点注释原样保留；只换"库怎么来"（portTestDB）、读写句柄合一（db.pool）、占位符
// $1..$n、时间列 BIGINT 毫秒与 PG 布尔 false/true。直插行另补 PG 新增的归属三列
// （caller_id / authorizing_kind）——与调度器定义对齐路径同值：local-admin + admin-builtin。
// This file owns the assertion surface of the scheduling tables, ported verbatim from the
// SQLite lineage; only the fixture, the placeholder dialect and the ownership columns differ.

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// ---- 夹具（§4.3 助手清单）----------------------------------------------------

// taskPortSeedTask 直插一条 active 任务行（时间取 opNow——判据不依赖实现自取时钟）。
// PG 的 scheduled_tasks 带归属三列（NOT NULL + CHECK），直插行与调度器定义对齐路径同值。
// taskPortSeedTask inserts one active task row at the frozen clock.
func taskPortSeedTask(t *testing.T, db *DB, id, agent, cron, message string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`INSERT INTO scheduled_tasks
		   (task_id, agent_name, cron, zone, message, enabled, definition_hash, created_at, updated_at,
		    caller_id, authorizing_kind)
		 VALUES ($1, $2, $3, 'UTC', $4, true, 'h1', $5, $5, $6, $7)`,
		id, agent, cron, message, opNow.UnixMilli(),
		core.LocalAdminCallerID, string(core.AuthorizingKindAdminBuiltin))
	require.NoError(t, err)
}

// taskPortSeedRetiredTask 直插一条已退休任务行（retired_at 取更早的时刻，便于与 now 区分）。
// taskPortSeedRetiredTask inserts one retired task row with an earlier retirement stamp.
func taskPortSeedRetiredTask(t *testing.T, w *sql.DB, id, agent string) {
	t.Helper()
	retiredAt := opNow.Add(-time.Hour).UnixMilli()
	_, err := w.ExecContext(context.Background(),
		`INSERT INTO scheduled_tasks
		   (task_id, agent_name, cron, zone, message, enabled, definition_hash,
		    retired_at, created_at, updated_at, caller_id, authorizing_kind)
		 VALUES ($1, $2, '0 8 * * * *', 'UTC', '已退休任务', true, 'h1', $3, $3, $3, $4, $5)`,
		id, agent, retiredAt, core.LocalAdminCallerID, string(core.AuthorizingKindAdminBuiltin))
	require.NoError(t, err)
}

// taskPortSeedRun 直插一条 runs 行（恢复 / 终态判据要读 Run 状态）。
// PG 的 runs 带归属与授权来源 NOT NULL 列：与调度路径同值（local-admin + admin-builtin
// + actor=scheduler）。
// taskPortSeedRun inserts one runs row with the scheduler's ownership columns.
func taskPortSeedRun(t *testing.T, db *DB, runID, status string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(),
		`INSERT INTO runs
		   (run_id, agent, channel, user_id, trigger, status, started_at,
		    caller_id, authorizing_kind, actor)
		 VALUES ($1, 'demo', 'scheduler', 'scheduler', 'cron', $2, $3, $4, $5, 'scheduler')`,
		runID, status, opNow.UnixMilli(),
		core.LocalAdminCallerID, string(core.AuthorizingKindAdminBuiltin))
	require.NoError(t, err)
}

// taskPortSeedExecution 直插一条执行行（空 runID → NULL；trigger 固定 cron，起始时刻 opNow）。
// taskPortSeedExecution inserts one execution row; an empty runID becomes NULL.
func taskPortSeedExecution(t *testing.T, w *sql.DB, id, runID string, status core.ExecutionStatus) int64 {
	t.Helper()
	return taskPortSeedExecutionAt(t, w, id, runID, status, opNow)
}

// taskPortSeedExecutionAt 是 taskPortSeedExecution 的显式时刻版本（恢复排序用例需要不同的 started_at）。
// PG 的自增 id 只能经 RETURNING 取回（SQLite 的 LastInsertId 不存在）。
// taskPortSeedExecutionAt takes an explicit started_at for the recovery-ordering case.
func taskPortSeedExecutionAt(t *testing.T, w *sql.DB, taskID, runID string, status core.ExecutionStatus, at time.Time) int64 {
	t.Helper()
	var id int64
	err := w.QueryRowContext(context.Background(),
		`INSERT INTO task_executions (task_id, run_id, status, trigger, started_at)
		 VALUES ($1, $2, $3, 'cron', $4)
		 RETURNING id`,
		taskID, nullIfEmpty(runID), string(status), at.UnixMilli()).Scan(&id)
	require.NoError(t, err)
	return id
}

// taskPortListExecutions 按 id 升序读回某个任务的执行行（in-package 判卷走同一连接池；可空列用 Null 形态）。
// taskPortListExecutions reads the execution rows of one task in ascending id order.
func taskPortListExecutions(t *testing.T, r *sql.DB, taskID string) []core.ExecutionRecord {
	t.Helper()
	rows, err := r.QueryContext(context.Background(),
		`SELECT id, task_id, run_id, status, trigger, duration_ms, error_message, started_at, finished_at
		   FROM task_executions WHERE task_id = $1 ORDER BY id ASC`, taskID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()

	out := make([]core.ExecutionRecord, 0, 2)
	for rows.Next() {
		var (
			rec        core.ExecutionRecord
			runID      sql.NullString
			status     string
			trigger    string
			durationMS sql.NullInt64
			errMsg     sql.NullString
			startedAt  int64
			finishedAt sql.NullInt64
		)
		require.NoError(t, rows.Scan(&rec.ID, &rec.TaskID, &runID, &status, &trigger,
			&durationMS, &errMsg, &startedAt, &finishedAt))
		rec.RunID = runID.String
		rec.Status = core.ExecutionStatus(status)
		rec.Trigger = core.Trigger(trigger)
		rec.DurationMS = durationMS.Int64
		rec.ErrorMessage = errMsg.String
		rec.StartedAt = time.UnixMilli(startedAt).UTC()
		if finishedAt.Valid {
			rec.FinishedAt = time.UnixMilli(finishedAt.Int64).UTC()
		}
		out = append(out, rec)
	}
	require.NoError(t, rows.Err())
	return out
}

// taskPortLastStatusOf 读回 scheduled_tasks.last_status（NULL → 空串）。
// taskPortLastStatusOf reads the last_status column; NULL maps to the empty string.
func taskPortLastStatusOf(t *testing.T, r *sql.DB, taskID string) string {
	t.Helper()
	var status sql.NullString
	require.NoError(t, r.QueryRowContext(context.Background(),
		`SELECT last_status FROM scheduled_tasks WHERE task_id = $1`, taskID).Scan(&status))
	return status.String
}

// taskPortCronOf 读回单列 cron（其余单列读数同理）。
// taskPortCronOf reads one task column back through the pool.
func taskPortCronOf(t *testing.T, db *DB, id string) string {
	t.Helper()
	var v string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT cron FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&v))
	return v
}

// taskPortMessageOf 读回 message。
func taskPortMessageOf(t *testing.T, db *DB, id string) string {
	t.Helper()
	var v string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT message FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&v))
	return v
}

// taskPortAgentOf 读回 agent_name。
func taskPortAgentOf(t *testing.T, db *DB, id string) string {
	t.Helper()
	var v string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT agent_name FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&v))
	return v
}

// taskPortRetiredAtOf 读回 retired_at（NULL → 零值）。
func taskPortRetiredAtOf(t *testing.T, db *DB, id string) time.Time {
	t.Helper()
	var ms sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT retired_at FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&ms))
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64).UTC()
}

// ---- 附加读数助手 ------------------------------------------------------------

// taskPortEnabledOf 读回 enabled（PG boolean → bool）。
func taskPortEnabledOf(t *testing.T, db *DB, id string) bool {
	t.Helper()
	var v bool
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT enabled FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&v))
	return v
}

// taskPortLastRunAtOf 读回 last_run_at（NULL → 零值）。
func taskPortLastRunAtOf(t *testing.T, db *DB, id string) time.Time {
	t.Helper()
	var ms sql.NullInt64
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT last_run_at FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&ms))
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64).UTC()
}

// taskPortUpdatedAtOf 读回 updated_at。
func taskPortUpdatedAtOf(t *testing.T, db *DB, id string) time.Time {
	t.Helper()
	var ms int64
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT updated_at FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&ms))
	return time.UnixMilli(ms).UTC()
}

// taskPortCreatedAtOf 读回 created_at。
func taskPortCreatedAtOf(t *testing.T, db *DB, id string) time.Time {
	t.Helper()
	var ms int64
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT created_at FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&ms))
	return time.UnixMilli(ms).UTC()
}

// taskPortDefinitionHashOf 读回 definition_hash。
func taskPortDefinitionHashOf(t *testing.T, db *DB, id string) string {
	t.Helper()
	var v string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT definition_hash FROM scheduled_tasks WHERE task_id = $1`, id).Scan(&v))
	return v
}

// ---- ApplyAgentTasks ----------------------------------------------------------

// TestApplyAgentTasks_UpsertPreservesState：插入补缺省、更新保留运行状态、定义指纹更新。
// 逆改点：DO UPDATE 覆盖 enabled / last_* / created_at → 对应用例断言红。
func TestApplyAgentTasks_UpsertPreservesState(t *testing.T) {
	ctx := context.Background()

	t.Run("insert_defaults", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		now := opNow

		require.NoError(t, store.ApplyAgentTasks(ctx, "demo", []core.TaskRecord{{
			TaskID: "demo-1", Agent: "demo", Cron: "0 8 * * * *", Zone: "UTC",
			Message: "每日巡检", DefinitionHash: "h1",
			CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		}}, nil, now))

		tasks, err := store.ListTasks(ctx)
		require.NoError(t, err)
		require.Len(t, tasks, 1)
		got := tasks[0]
		require.True(t, got.Enabled, "新任务缺省启用")
		require.True(t, got.LastRunAt.IsZero(), "last_run_at 缺省 NULL → 零值")
		require.Empty(t, got.LastStatus, "last_status 缺省 NULL → 空串")
		require.True(t, got.RetiredAt.IsZero(), "retired_at 缺省 NULL → 零值")
		require.Equal(t, now.UnixMilli(), got.CreatedAt.UnixMilli())
		require.Equal(t, now.UnixMilli(), got.UpdatedAt.UnixMilli())
		require.Equal(t, "h1", got.DefinitionHash)
	})

	t.Run("update_preserves_state", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "旧巡检")

		// 造出"运行状态已经存在"的旧行：enabled 关闭、last_* 有值、created_at 更早。
		older := opNow.Add(-time.Hour)
		_, err := db.pool.ExecContext(ctx,
			`UPDATE scheduled_tasks
			    SET enabled = false, last_run_at = $1, last_status = 'failed', created_at = $2
			  WHERE task_id = 'demo-1'`,
			older.UnixMilli(), older.UnixMilli())
		require.NoError(t, err)

		now := opNow
		require.NoError(t, store.ApplyAgentTasks(ctx, "demo", []core.TaskRecord{{
			TaskID: "demo-1", Agent: "demo", Cron: "0 9 * * * *", Zone: "UTC",
			Message: "新巡检", DefinitionHash: "h2",
			CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		}}, nil, now))

		require.Equal(t, "0 9 * * * *", taskPortCronOf(t, db, "demo-1"))
		require.Equal(t, "新巡检", taskPortMessageOf(t, db, "demo-1"))
		require.False(t, taskPortEnabledOf(t, db, "demo-1"), "upsert 不得覆盖 enabled")
		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"), "upsert 不得覆盖 last_status")
		require.Equal(t, older.UnixMilli(), taskPortLastRunAtOf(t, db, "demo-1").UnixMilli(),
			"upsert 不得覆盖 last_run_at")
		require.Equal(t, older.UnixMilli(), taskPortCreatedAtOf(t, db, "demo-1").UnixMilli(),
			"upsert 不得覆盖 created_at")
		require.Equal(t, now.UnixMilli(), taskPortUpdatedAtOf(t, db, "demo-1").UnixMilli(),
			"updated_at 随本次写入前进")
	})

	t.Run("definition_hash_updated", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		require.Equal(t, "h1", taskPortDefinitionHashOf(t, db, "demo-1"))

		require.NoError(t, store.ApplyAgentTasks(ctx, "demo", []core.TaskRecord{{
			TaskID: "demo-1", Agent: "demo", Cron: "0 8 * * * *", Zone: "UTC",
			Message: "巡检", DefinitionHash: "h2",
			CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		}}, nil, opNow))

		require.Equal(t, "h2", taskPortDefinitionHashOf(t, db, "demo-1"), "定义指纹必须随文件更新")
	})
}

// TestTaskStore_RetireAndRevive：退休只置时间、不物理删；同 id 再现复活且 enabled 保持库值。
// 逆改点：物理删除 → 执行历史行随之消失；复活重置 enabled=1 → 断言红。
func TestTaskStore_RetireAndRevive(t *testing.T) {
	ctx := context.Background()

	t.Run("retire_marks_without_delete", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedExecution(t, db.pool, "demo-1", "run-a", core.ExecutionStarted)

		require.NoError(t, store.ApplyAgentTasks(ctx, "demo", nil, []string{"demo-1"}, opNow))

		require.False(t, taskPortRetiredAtOf(t, db, "demo-1").IsZero(), "退休只置 retired_at")
		require.Len(t, taskPortListExecutions(t, db.pool, "demo-1"), 1, "退休不物理删，历史仍可查")
		tasks, err := store.ListTasks(ctx)
		require.NoError(t, err)
		require.Len(t, tasks, 1, "retired 行原样返回，由 core 过滤")
		require.False(t, tasks[0].RetiredAt.IsZero())
	})

	t.Run("revive_clears_retired_keeps_enabled", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		_, err := db.pool.ExecContext(ctx,
			`UPDATE scheduled_tasks SET enabled = false, last_status = 'failed' WHERE task_id = 'demo-1'`)
		require.NoError(t, err)

		require.NoError(t, store.ApplyAgentTasks(ctx, "demo", nil, []string{"demo-1"}, opNow))
		require.False(t, taskPortRetiredAtOf(t, db, "demo-1").IsZero())

		// 同 Agent 同 id 再现 = 复活：清 retired_at，但运行状态保持库里的值。
		require.NoError(t, store.ApplyAgentTasks(ctx, "demo", []core.TaskRecord{{
			TaskID: "demo-1", Agent: "demo", Cron: "0 9 * * * *", Zone: "UTC",
			Message: "巡检", DefinitionHash: "h2",
			CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		}}, nil, opNow.Add(time.Minute)))

		require.True(t, taskPortRetiredAtOf(t, db, "demo-1").IsZero(), "复活清 retired_at")
		require.False(t, taskPortEnabledOf(t, db, "demo-1"), "复活保持库里的 enabled=0")
		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"))
		require.Equal(t, "0 9 * * * *", taskPortCronOf(t, db, "demo-1"))
	})
}

// TestApplyAgentTasks_SameOwnerReviveAllowed：约束 42——同 owner 对 retired 行的 upsert
// 在归属谓词下命中（RowsAffected()==1）并提交；逆改点：谓词漏掉 owner 或漏清 retired_at。
func TestApplyAgentTasks_SameOwnerReviveAllowed(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)
	taskPortSeedRetiredTask(t, db.pool, "demo-1", "demo")

	require.NoError(t, store.ApplyAgentTasks(ctx, "demo", []core.TaskRecord{{
		TaskID: "demo-1", Agent: "demo", Cron: "0 9 * * * *", Zone: "UTC",
		Message: "新巡检", DefinitionHash: "h2",
		CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin,
	}}, nil, opNow))

	require.True(t, taskPortRetiredAtOf(t, db, "demo-1").IsZero(), "同 owner 复活清 retired_at")
	require.Equal(t, "0 9 * * * *", taskPortCronOf(t, db, "demo-1"))
	require.Equal(t, "demo", taskPortAgentOf(t, db, "demo-1"))
}

// TestApplyAgentTasks_CrossOwnerRejected：约束 42——其他 Agent 的 active id 整事务拒绝，
// 原行 agent_name / enabled / cron 全不变。逆改点：DO UPDATE 覆盖 agent_name → 接管成功。
func TestApplyAgentTasks_CrossOwnerRejected(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)
	taskPortSeedTask(t, db, "shared-1", "agent-a", "0 8 * * * *", "A 的任务")
	_, err := db.pool.ExecContext(ctx,
		`UPDATE scheduled_tasks SET enabled = false WHERE task_id = 'shared-1'`)
	require.NoError(t, err)

	err = store.ApplyAgentTasks(ctx, "agent-b", []core.TaskRecord{{
		TaskID: "shared-1", Agent: "agent-b", Cron: "0 9 * * * *", Zone: "UTC",
		Message: "B 接管", DefinitionHash: "hb",
		CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin,
	}}, nil, opNow)

	require.ErrorIs(t, err, core.ErrTaskOwnerConflict)
	require.Equal(t, "agent-a", taskPortAgentOf(t, db, "shared-1"), "归属不变")
	require.False(t, taskPortEnabledOf(t, db, "shared-1"), "enabled 不变")
	require.Equal(t, "0 8 * * * *", taskPortCronOf(t, db, "shared-1"), "整事务回滚，原定义不变")
	require.Equal(t, "A 的任务", taskPortMessageOf(t, db, "shared-1"))
}

// TestApplyAgentTasks_CrossOwnerRetiredRejected：约束 17 / 42——下线不释放归属，
// retired 行同样拒绝其他 Agent 的接管。逆改点：把 retired 当"归属已释放" → 接管成功。
func TestApplyAgentTasks_CrossOwnerRetiredRejected(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)
	taskPortSeedRetiredTask(t, db.pool, "shared-1", "agent-a")

	err := store.ApplyAgentTasks(ctx, "agent-b", []core.TaskRecord{{
		TaskID: "shared-1", Agent: "agent-b", Cron: "0 9 * * * *", Zone: "UTC",
		Message: "B 接管", DefinitionHash: "hb",
		CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin,
	}}, nil, opNow)

	require.ErrorIs(t, err, core.ErrTaskOwnerConflict)
	require.Equal(t, "agent-a", taskPortAgentOf(t, db, "shared-1"))
	require.False(t, taskPortRetiredAtOf(t, db, "shared-1").IsZero(), "retired 状态不变")
}

// TestApplyAgentTasks_RollbackOnSecondWrite：约束 41——同一事务内任一语句失败即整体回滚，
// 库保持完整旧定义（无部分更新、无部分退休）。SQLite 触发器做故障注入；PG 侧用等价的
// BEFORE UPDATE 触发器（plpgsql RAISE EXCEPTION）注入同一故障。
// 逆改点：逐条自动提交或忽略中间错误继续 → 部分生效，断言红。
func TestApplyAgentTasks_RollbackOnSecondWrite(t *testing.T) {
	ctx := context.Background()

	newUpsert := func(id, cron, message string) core.TaskRecord {
		return core.TaskRecord{
			TaskID: id, Agent: "demo", Cron: cron, Zone: "UTC",
			Message: message, DefinitionHash: "h2",
			CallerID: core.LocalAdminCallerID, AuthorizingKind: core.AuthorizingKindAdminBuiltin,
		}
	}

	// taskPortInjectUpdateFailure 在隔离 schema 内建一个"更新 demo-2 即抛错"的触发器，
	// 等价于 SQLite 侧的 `CREATE TRIGGER ... BEGIN SELECT RAISE(ABORT, ...); END`。
	// taskPortInjectUpdateFailure installs the PG counterpart of the SQLite fault trigger.
	taskPortInjectUpdateFailure := func(t *testing.T, db *DB) {
		t.Helper()
		_, err := db.pool.ExecContext(ctx,
			`CREATE FUNCTION task_port_fail_demo_2() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN
			   RAISE EXCEPTION 'injected failure';
			 END
			 $$;
			 CREATE TRIGGER fail_demo_2 BEFORE UPDATE ON scheduled_tasks
			   FOR EACH ROW WHEN (NEW.task_id = 'demo-2')
			   EXECUTE FUNCTION task_port_fail_demo_2()`)
		require.NoError(t, err)
	}

	t.Run("second_upsert_fails", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "旧巡检")
		taskPortSeedTask(t, db, "demo-2", "demo", "0 9 * * * *", "旧日报")
		taskPortInjectUpdateFailure(t, db)

		err := store.ApplyAgentTasks(ctx, "demo", []core.TaskRecord{
			newUpsert("demo-1", "*/10 * * * * *", "新巡检"),
			newUpsert("demo-2", "*/20 * * * * *", "新日报"),
		}, nil, opNow)
		require.Error(t, err, "第二条 upsert 被触发器拒绝")

		require.Equal(t, "0 8 * * * *", taskPortCronOf(t, db, "demo-1"), "第一条 upsert 必须随事务回滚")
		require.Equal(t, "旧巡检", taskPortMessageOf(t, db, "demo-1"))
		require.Equal(t, "0 9 * * * *", taskPortCronOf(t, db, "demo-2"))
		require.Equal(t, "旧日报", taskPortMessageOf(t, db, "demo-2"))
	})

	t.Run("second_retire_fails", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "旧巡检")
		taskPortSeedTask(t, db, "demo-2", "demo", "0 9 * * * *", "旧日报")
		taskPortInjectUpdateFailure(t, db)

		err := store.ApplyAgentTasks(ctx, "demo",
			[]core.TaskRecord{newUpsert("demo-1", "*/10 * * * * *", "新巡检")},
			[]string{"demo-2"}, opNow)
		require.Error(t, err, "retire 被触发器拒绝")

		require.Equal(t, "0 8 * * * *", taskPortCronOf(t, db, "demo-1"), "已成功的 upsert 必须回滚")
		require.Equal(t, "旧巡检", taskPortMessageOf(t, db, "demo-1"))
		require.True(t, taskPortRetiredAtOf(t, db, "demo-2").IsZero(), "retire 不得部分生效")
	})
}

// ---- 启停与运行痕迹 ----------------------------------------------------------

// TestTaskStore_SetTaskEnabled：翻转启停；未知 id → fs.ErrNotExist（并发下线兜底）。
func TestTaskStore_SetTaskEnabled(t *testing.T) {
	ctx := context.Background()

	t.Run("toggles", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")

		require.NoError(t, store.SetTaskEnabled(ctx, "demo-1", false, opNow))
		require.False(t, taskPortEnabledOf(t, db, "demo-1"))
		require.Equal(t, opNow.UnixMilli(), taskPortUpdatedAtOf(t, db, "demo-1").UnixMilli())

		require.NoError(t, store.SetTaskEnabled(ctx, "demo-1", true, opNow.Add(time.Minute)))
		require.True(t, taskPortEnabledOf(t, db, "demo-1"))
	})

	t.Run("unknown_id_not_found", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)

		err := store.SetTaskEnabled(ctx, "ghost", true, opNow)
		require.ErrorIs(t, err, fs.ErrNotExist, "未知 id 必须包装 fs.ErrNotExist")
	})
}

// TestTaskStore_MarkTaskRun：last_run_at / last_status / updated_at 三元组一次更新。
func TestTaskStore_MarkTaskRun(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewTaskStore(db)
	taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")

	require.NoError(t, store.MarkTaskRun(ctx, "demo-1", opNow, core.ExecutionFailed))

	require.Equal(t, opNow.UnixMilli(), taskPortLastRunAtOf(t, db, "demo-1").UnixMilli())
	require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"))
	require.Equal(t, opNow.UnixMilli(), taskPortUpdatedAtOf(t, db, "demo-1").UnixMilli())
}

// TestTaskStore_StartExecutionAndFinish：起始行恒为 started；收尾填时长与错误消息。
func TestTaskStore_StartExecutionAndFinish(t *testing.T) {
	ctx := context.Background()

	t.Run("start", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")

		id, err := store.StartExecution(ctx, core.ExecutionRecord{
			TaskID: "demo-1", RunID: "scheduler-1-a", Status: core.ExecutionStarted,
			Trigger: core.TriggerCron, StartedAt: opNow,
		})
		require.NoError(t, err)
		require.NotZero(t, id)

		rows := taskPortListExecutions(t, db.pool, "demo-1")
		require.Len(t, rows, 1)
		got := rows[0]
		require.Equal(t, id, got.ID)
		require.Equal(t, core.ExecutionStarted, got.Status, "起始态由仓储固定写 started")
		require.Equal(t, core.TriggerCron, got.Trigger)
		require.Equal(t, "scheduler-1-a", got.RunID)
		require.Zero(t, got.DurationMS)
		require.Empty(t, got.ErrorMessage)
		require.True(t, got.FinishedAt.IsZero(), "起始行未结束 → finished_at 为 NULL")
		require.Equal(t, opNow.UnixMilli(), got.StartedAt.UnixMilli())
	})

	t.Run("finish_fills_duration_and_error", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedRun(t, db, "run-a", "failed")

		id, err := store.StartExecution(ctx, core.ExecutionRecord{
			TaskID: "demo-1", RunID: "run-a", Status: core.ExecutionStarted,
			Trigger: core.TriggerCron, StartedAt: opNow,
		})
		require.NoError(t, err)

		finished := opNow.Add(1500 * time.Millisecond)
		require.NoError(t, store.FinishExecution(ctx, id, core.ExecutionFailed,
			1500*time.Millisecond, "模型调用失败", finished))

		got := taskPortListExecutions(t, db.pool, "demo-1")[0]
		require.Equal(t, core.ExecutionFailed, got.Status)
		require.Equal(t, int64(1500), got.DurationMS)
		require.Equal(t, "模型调用失败", got.ErrorMessage)
		require.Equal(t, finished.UnixMilli(), got.FinishedAt.UnixMilli())
		require.Equal(t, "run-a", got.RunID, "Run 行存在 → run_id 保留")
		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"))
	})
}

// TestTaskStore_FinishExecution：终态写在同一事务同步 last_status（含 skipped）；
// run_id 的判据是"该行是否真的产生过 Run"，不是 status 取值（P-04）。
// 逆改点：只写执行行 → last_status 保持旧值；按 status == 'skipped' 判清除 →
// 准入失败行留悬空引用；未知行号静默成功 → 错误断言红。
func TestTaskStore_FinishExecution(t *testing.T) {
	ctx := context.Background()

	start := func(t *testing.T, store *TaskStore, runID string) int64 {
		t.Helper()
		id, err := store.StartExecution(ctx, core.ExecutionRecord{
			TaskID: "demo-1", RunID: runID, Status: core.ExecutionStarted,
			Trigger: core.TriggerCron, StartedAt: opNow,
		})
		require.NoError(t, err)
		return id
	}

	t.Run("completed_syncs_last_status", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedRun(t, db, "run-a", "completed")
		id := start(t, store, "run-a")

		require.NoError(t, store.FinishExecution(ctx, id, core.ExecutionCompleted, time.Second, "", opNow))

		require.Equal(t, core.ExecutionCompleted, taskPortListExecutions(t, db.pool, "demo-1")[0].Status)
		require.Equal(t, "completed", taskPortLastStatusOf(t, db.pool, "demo-1"))
		require.Equal(t, "run-a", taskPortListExecutions(t, db.pool, "demo-1")[0].RunID)
	})

	t.Run("failed_syncs_last_status", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedRun(t, db, "run-b", "failed")
		id := start(t, store, "run-b")

		require.NoError(t, store.FinishExecution(ctx, id, core.ExecutionFailed, 2*time.Second, "运行失败", opNow))

		got := taskPortListExecutions(t, db.pool, "demo-1")[0]
		require.Equal(t, core.ExecutionFailed, got.Status)
		require.Equal(t, "run-b", got.RunID)
		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"))
	})

	t.Run("skipped_nulls_run_id", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		// run-x 不落 runs 表：跳过行没有产生过 Run。
		id := start(t, store, "run-x")

		require.NoError(t, store.FinishExecution(ctx, id, core.ExecutionSkipped, 0, "", opNow))

		got := taskPortListExecutions(t, db.pool, "demo-1")[0]
		require.Equal(t, core.ExecutionSkipped, got.Status)
		require.Empty(t, got.RunID, "未产生 Run → run_id 置 NULL")
		require.Equal(t, "skipped", taskPortLastStatusOf(t, db.pool, "demo-1"),
			"任何终态都同步 last_status，含 skipped")
	})

	t.Run("failed_without_run_row_nulls_run_id", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		// 准入失败（未知 Agent / 实例满）：失败终态但从未进 runs 表。
		id := start(t, store, "run-ghost")

		require.NoError(t, store.FinishExecution(ctx, id, core.ExecutionFailed,
			0, "Agent 不存在", opNow))

		got := taskPortListExecutions(t, db.pool, "demo-1")[0]
		require.Equal(t, core.ExecutionFailed, got.Status)
		require.Empty(t, got.RunID, "未进 runs 表的失败行不得留悬空引用")
		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"))
	})

	t.Run("unknown_id_not_found", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")

		err := store.FinishExecution(ctx, 9999, core.ExecutionCompleted, 0, "", opNow)
		require.ErrorIs(t, err, fs.ErrNotExist, "未知行号必须如实报错，不得静默成功")
		require.Empty(t, taskPortLastStatusOf(t, db.pool, "demo-1"), "未知行号不得误写 last_status")
		require.Empty(t, taskPortListExecutions(t, db.pool, "demo-1"))
	})

	t.Run("unknown_id_leaves_previous_last_status", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedRun(t, db, "run-a", "completed")
		id := start(t, store, "run-a")
		require.NoError(t, store.FinishExecution(ctx, id, core.ExecutionCompleted, time.Second, "", opNow))
		require.Equal(t, "completed", taskPortLastStatusOf(t, db.pool, "demo-1"))

		err := store.FinishExecution(ctx, 9999, core.ExecutionFailed, 0, "幽灵行", opNow)
		require.ErrorIs(t, err, fs.ErrNotExist)
		require.Equal(t, "completed", taskPortLastStatusOf(t, db.pool, "demo-1"),
			"未知行号不得覆盖已有 last_status")
	})
}

// TestTaskStore_RecordSkipped：直接插入零时长终态行；不产生 Run、不触碰 last_*。
// 逆改点：误写 last_status → 断言红；把 run_id 写成空串而非 NULL → run_id IS NULL 断言红。
func TestTaskStore_RecordSkipped(t *testing.T) {
	ctx := context.Background()

	t.Run("zero_duration_terminal_row", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")

		require.NoError(t, store.RecordSkipped(ctx, core.ExecutionRecord{
			TaskID: "demo-1", Status: core.ExecutionSkipped,
			Trigger: core.TriggerCron, StartedAt: opNow,
		}))

		rows := taskPortListExecutions(t, db.pool, "demo-1")
		require.Len(t, rows, 1)
		got := rows[0]
		require.Equal(t, core.ExecutionSkipped, got.Status)
		require.Equal(t, core.TriggerCron, got.Trigger)
		require.Empty(t, got.RunID)
		require.Zero(t, got.DurationMS)
		require.Empty(t, got.ErrorMessage)
		require.Equal(t, opNow.UnixMilli(), got.StartedAt.UnixMilli())
		require.Equal(t, opNow.UnixMilli(), got.FinishedAt.UnixMilli(),
			"跳过行 started_at = finished_at = now")

		// duration_ms 必须写 0 而不是 NULL（契约：零时长终态行）。
		var durationIsNull bool
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT duration_ms IS NULL FROM task_executions WHERE id = $1`, got.ID).Scan(&durationIsNull))
		require.False(t, durationIsNull, "duration_ms 写 0，不写 NULL")

		var runIsNull bool
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT run_id IS NULL FROM task_executions WHERE id = $1`, got.ID).Scan(&runIsNull))
		require.True(t, runIsNull, "跳过行 run_id 必须为 NULL")
	})

	t.Run("does_not_touch_last_status", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		older := opNow.Add(-time.Hour)
		_, err := db.pool.ExecContext(ctx,
			`UPDATE scheduled_tasks SET last_run_at = $1, last_status = 'failed' WHERE task_id = 'demo-1'`,
			older.UnixMilli())
		require.NoError(t, err)

		require.NoError(t, store.RecordSkipped(ctx, core.ExecutionRecord{
			TaskID: "demo-1", Status: core.ExecutionSkipped,
			Trigger: core.TriggerCron, StartedAt: opNow,
		}))

		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"),
			"无 started 前驱的跳过行不触碰 last_*")
		require.Equal(t, older.UnixMilli(), taskPortLastRunAtOf(t, db, "demo-1").UnixMilli())
	})
}

// TestTaskStore_RecoverStartedExecutions：启动恢复把遗留 started 行按 Run 终态收尾
// （completed / limit_reached → completed；failed / cancelled / 仍 running / 无 Run → failed），
// 无 Run 行的 run_id 置 NULL，同任务多行按 started_at 正序处理。
// 逆改点：用 string 直扫 run_id → NULL 行 Scan 报错；乱序处理 → last_status 来自旧行。
func TestTaskStore_RecoverStartedExecutions(t *testing.T) {
	ctx := context.Background()

	// 四终态映射：Run 终态 → 执行行终态。
	for _, tc := range []struct {
		runStatus string
		want      core.ExecutionStatus
	}{
		{"completed", core.ExecutionCompleted},
		{"limit_reached", core.ExecutionCompleted},
		{"failed", core.ExecutionFailed},
		{"cancelled", core.ExecutionFailed},
	} {
		t.Run("run_"+tc.runStatus, func(t *testing.T) {
			db := portTestDB(t)
			store := NewTaskStore(db)
			taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
			taskPortSeedRun(t, db, "run-a", tc.runStatus)
			taskPortSeedExecution(t, db.pool, "demo-1", "run-a", core.ExecutionStarted)

			n, err := store.RecoverStartedExecutions(ctx, opNow)
			require.NoError(t, err)
			require.Equal(t, 1, n)

			got := taskPortListExecutions(t, db.pool, "demo-1")[0]
			require.Equal(t, tc.want, got.Status, "runs.status=%s", tc.runStatus)
			require.Equal(t, "run-a", got.RunID, "Run 行存在 → run_id 保留")
			require.Equal(t, opNow.UnixMilli(), got.FinishedAt.UnixMilli())
			require.Equal(t, string(tc.want), taskPortLastStatusOf(t, db.pool, "demo-1"))
		})
	}

	t.Run("no_run_row_maps_failed_and_nulls_run_id", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		// run-ghost 不在 runs 表：崩溃遗留行没有任何 Run 可依。
		taskPortSeedExecution(t, db.pool, "demo-1", "run-ghost", core.ExecutionStarted)

		n, err := store.RecoverStartedExecutions(ctx, opNow)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		got := taskPortListExecutions(t, db.pool, "demo-1")[0]
		require.Equal(t, core.ExecutionFailed, got.Status)
		require.Empty(t, got.RunID, "无 Run 行同时把 run_id 置 NULL（不留下悬空引用）")
		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"))
	})

	t.Run("run_still_running_maps_failed", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedRun(t, db, "run-live", "running")
		taskPortSeedExecution(t, db.pool, "demo-1", "run-live", core.ExecutionStarted)

		n, err := store.RecoverStartedExecutions(ctx, opNow)
		require.NoError(t, err)
		require.Equal(t, 1, n)

		got := taskPortListExecutions(t, db.pool, "demo-1")[0]
		require.Equal(t, core.ExecutionFailed, got.Status, "Run 未收尾 = 崩溃遗留 → failed")
		require.Equal(t, "run-live", got.RunID, "Run 行存在 → run_id 保留")
		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"))
	})

	t.Run("null_run_id_scannable", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedExecution(t, db.pool, "demo-1", "", core.ExecutionStarted)

		// 夹具自检：确实造出了 run_id IS NULL 的遗留行（sql.NullString 的判别力来源）。
		var nulls int
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT count(*) FROM task_executions WHERE run_id IS NULL`).Scan(&nulls))
		require.Equal(t, 1, nulls)

		n, err := store.RecoverStartedExecutions(ctx, opNow)
		require.NoError(t, err)
		require.Equal(t, 1, n, "run_id 为 NULL 的遗留行同样可被扫描收尾")

		got := taskPortListExecutions(t, db.pool, "demo-1")[0]
		require.Equal(t, core.ExecutionFailed, got.Status)
		require.Empty(t, got.RunID)
		require.Equal(t, "failed", taskPortLastStatusOf(t, db.pool, "demo-1"))
	})

	t.Run("same_task_multiple_started_uses_newest", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedRun(t, db, "run-old", "failed")
		taskPortSeedRun(t, db, "run-new", "completed")
		taskPortSeedExecutionAt(t, db.pool, "demo-1", "run-old", core.ExecutionStarted, opNow.Add(-time.Minute))
		taskPortSeedExecutionAt(t, db.pool, "demo-1", "run-new", core.ExecutionStarted, opNow)

		n, err := store.RecoverStartedExecutions(ctx, opNow)
		require.NoError(t, err)
		require.Equal(t, 2, n)

		rows := taskPortListExecutions(t, db.pool, "demo-1")
		require.Len(t, rows, 2)
		require.Equal(t, core.ExecutionFailed, rows[0].Status)
		require.Equal(t, core.ExecutionCompleted, rows[1].Status)
		require.Equal(t, "completed", taskPortLastStatusOf(t, db.pool, "demo-1"),
			"last_status 必须来自最新一行（按 started_at 正序收尾）")
	})
}

// ---- 列表读数 ----------------------------------------------------------------

// TestTaskStore_ListTasks：可空列映射零值、含 retired 行、task_id 升序。
// 逆改点：用 time.Time / string 直扫可空列 → 新任务行 Scan 报错。
func TestTaskStore_ListTasks(t *testing.T) {
	ctx := context.Background()

	t.Run("fresh_row_zero_values", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")

		tasks, err := store.ListTasks(ctx)
		require.NoError(t, err)
		require.Len(t, tasks, 1)
		got := tasks[0]
		require.Equal(t, "demo-1", got.TaskID)
		require.Equal(t, "demo", got.Agent)
		require.Equal(t, "0 8 * * * *", got.Cron)
		require.Equal(t, "UTC", got.Zone)
		require.Equal(t, "巡检", got.Message)
		require.True(t, got.Enabled)
		require.Equal(t, "h1", got.DefinitionHash)
		require.True(t, got.LastRunAt.IsZero(), "last_run_at NULL → 零值")
		require.Empty(t, got.LastStatus, "last_status NULL → 空串")
		require.True(t, got.RetiredAt.IsZero(), "retired_at NULL → 零值")
		require.Equal(t, opNow.UnixMilli(), got.CreatedAt.UnixMilli())
		require.Equal(t, opNow.UnixMilli(), got.UpdatedAt.UnixMilli())
	})

	t.Run("partial_columns", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		older := opNow.Add(-time.Hour)
		_, err := db.pool.ExecContext(ctx,
			`UPDATE scheduled_tasks SET last_run_at = $1, last_status = 'failed' WHERE task_id = 'demo-1'`,
			older.UnixMilli())
		require.NoError(t, err)

		tasks, err := store.ListTasks(ctx)
		require.NoError(t, err)
		got := tasks[0]
		require.Equal(t, older.UnixMilli(), got.LastRunAt.UnixMilli())
		require.Equal(t, core.ExecutionFailed, got.LastStatus)
		require.True(t, got.RetiredAt.IsZero(), "未退休的列仍为零值")
	})

	t.Run("retired_rows_included", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedRetiredTask(t, db.pool, "demo-1", "demo")

		tasks, err := store.ListTasks(ctx)
		require.NoError(t, err)
		require.Len(t, tasks, 1, "retired 行原样返回（过滤责任在 core）")
		require.False(t, tasks[0].RetiredAt.IsZero())
	})

	t.Run("ordered_by_task_id", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "task-b", "demo", "0 8 * * * *", "B")
		taskPortSeedTask(t, db, "task-c", "demo", "0 8 * * * *", "C")
		taskPortSeedTask(t, db, "task-a", "demo", "0 8 * * * *", "A")

		tasks, err := store.ListTasks(ctx)
		require.NoError(t, err)
		require.Len(t, tasks, 3)
		require.Equal(t, []string{"task-a", "task-b", "task-c"},
			[]string{tasks[0].TaskID, tasks[1].TaskID, tasks[2].TaskID}, "task_id 升序")
	})
}

// TestTaskStore_ListExecutions：降序 + LIMIT、started 行可空列映射零值、按任务过滤。
// 逆改点：直扫可空列 → started 行 Scan 报错。
func TestTaskStore_ListExecutions(t *testing.T) {
	ctx := context.Background()

	t.Run("descending_with_limit", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedExecutionAt(t, db.pool, "demo-1", "run-1", core.ExecutionCompleted, opNow)
		taskPortSeedExecutionAt(t, db.pool, "demo-1", "run-2", core.ExecutionCompleted, opNow.Add(time.Minute))
		taskPortSeedExecutionAt(t, db.pool, "demo-1", "run-3", core.ExecutionCompleted, opNow.Add(2*time.Minute))

		rows, err := store.ListExecutions(ctx, "demo-1", 2)
		require.NoError(t, err)
		require.Len(t, rows, 2, "LIMIT 生效")
		require.Equal(t, "run-3", rows[0].RunID, "最新的在前")
		require.Equal(t, "run-2", rows[1].RunID)
	})

	t.Run("started_row_zero_values", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedExecution(t, db.pool, "demo-1", "", core.ExecutionStarted)

		rows, err := store.ListExecutions(ctx, "demo-1", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		got := rows[0]
		require.Equal(t, core.ExecutionStarted, got.Status)
		require.Empty(t, got.RunID, "run_id NULL → 空串")
		require.Zero(t, got.DurationMS, "duration_ms NULL → 0")
		require.Empty(t, got.ErrorMessage, "error_message NULL → 空串")
		require.True(t, got.FinishedAt.IsZero(), "finished_at NULL → 零值")
		require.Equal(t, opNow.UnixMilli(), got.StartedAt.UnixMilli())
	})

	t.Run("delivery_join_reports_channel_uid", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedExecution(t, db.pool, "demo-1", "run-d", core.ExecutionCompleted)
		_, err := db.pool.ExecContext(ctx,
			`INSERT INTO schedule_deliveries
			   (run_id, task_id, agent_name, agent_uid, channel_uid, channel_name,
			    status, http_status, receipt_code, detail, created_at)
			 VALUES ('run-d', 'demo-1', 'demo', 'ag_test',
			         'rs_channeluid00000000000000000', 'feishu-ops',
			         'sent', 200, 0, '已发送（HTTP 200）', $1)`, opNow.UnixMilli())
		require.NoError(t, err)

		rows, err := store.ListExecutions(ctx, "demo-1", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, core.DeliverySent, rows[0].Delivery)
		require.Equal(t, "rs_channeluid00000000000000000", rows[0].DeliveryChannel,
			"按契约回渠道资源 UID；渠道名只是发送时快照，同名重建会换 UID")
		require.Equal(t, "已发送（HTTP 200）", rows[0].DeliveryDetail)
	})

	t.Run("only_target_task", func(t *testing.T) {
		db := portTestDB(t)
		store := NewTaskStore(db)
		taskPortSeedTask(t, db, "demo-1", "demo", "0 8 * * * *", "巡检")
		taskPortSeedTask(t, db, "demo-2", "demo", "0 9 * * * *", "日报")
		taskPortSeedExecution(t, db.pool, "demo-1", "run-a", core.ExecutionCompleted)
		taskPortSeedExecution(t, db.pool, "demo-2", "run-b", core.ExecutionCompleted)

		rows, err := store.ListExecutions(ctx, "demo-1", 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, "demo-1", rows[0].TaskID)
		require.Equal(t, "run-a", rows[0].RunID)
	})
}
