//go:build integration

package postgres

// 本文件判"业务历史"的四表清单与排除项（US-25 §3.1.2 / FR-25-03 / FR-25-04）。
//
// 为什么必须用真库：自动建钥的资格判定里，"零业务历史"是唯一一项只能由存储回答的事实。
// 假件回放一个预置布尔值，四张表写错一张、或者把 `management_operations` 的 pending
// 审计也算进来，都永远不会被发现——而后者恰恰会让首次准备自己把自己判成存量实例，
// "可证明的新实例"从此不成立。
//
// 逆改点：从 `runtimeHistoryTables` 删掉任一张表 → 对应子用例红；把清单换成
// `management_operations` → 排除项用例红；把任一查询失败吞成 false →
// `TestRuntimeHistory_Tables` 的取消上下文断言红；把 EXISTS 改成整表读再判空 →
// 语义仍绿，但 `HasRuntimeHistory` 的注释与 §3.1.2 的"不整表读内存"要求不符（审查项）。
//
// These are the database-backed cases for the business-history allow-list: every listed
// table flips the verdict, the audit table never does, and a read failure is an error.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// clearRuntimeHistory 把四张业务历史表与它们的最小父行清空；顺序按外键依赖排，
// 子表在前。只用于子用例之间的隔离，不碰任何其它表。
// clearRuntimeHistory empties the four tables (and their minimal parents) in FK order.
func clearRuntimeHistory(t *testing.T, db *DB) {
	t.Helper()
	for _, statement := range []string{
		`DELETE FROM task_executions`,
		`DELETE FROM runs`,
		`DELETE FROM agent_lifecycle`,
		`DELETE FROM kb_documents`,
		`DELETE FROM scheduled_tasks`,
		`DELETE FROM kb_collections`,
		`DELETE FROM management_operations`,
	} {
		_, err := db.pool.ExecContext(context.Background(), statement)
		require.NoError(t, err)
	}
}

// TestRuntimeHistory_Tables 逐表判：清单中的每一张表只要有行就必须返回 true，空库返回
// false，取消的上下文必须返回错误（读失败不得被吞成"没有历史"）。
// TestRuntimeHistory_Tables checks every allow-listed table one by one.
func TestRuntimeHistory_Tables(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	store := NewInstanceSecretsStore(db)

	cases := []struct {
		table string
		seed  []string
	}{
		{"runs", []string{
			`INSERT INTO runs
			 (run_id, session_id, agent, channel, user_id, trigger, status, started_at,
			  caller_id, authorizing_kind, key_id, actor)
			 VALUES ('hist-run', NULL, 'demo', 'web', 'u1', 'user', 'completed', 1,
			         'c1', 'caller-key', 'key-hist', 'caller')`,
		}},
		{"task_executions", []string{
			`INSERT INTO scheduled_tasks
			 (task_id, agent_name, cron, zone, message, definition_hash, created_at, updated_at,
			  caller_id, authorizing_kind, authorizing_key_id)
			 VALUES ('hist-task', 'demo', '0 * * * * *', 'UTC', 'hello', 'hash', 1, 1,
			         'c1', 'caller-key', 'key-hist')`,
			`INSERT INTO task_executions (task_id, status, trigger, started_at)
			 VALUES ('hist-task', 'completed', 'cron', 1)`,
		}},
		{"agent_lifecycle", []string{
			`INSERT INTO agent_lifecycle (agent_name, updated_at) VALUES ('demo', 1)`,
		}},
		{"kb_documents", []string{
			`INSERT INTO kb_collections (collection_id, name, created_at) VALUES ('hist-col', 'hist', 1)`,
			`INSERT INTO kb_documents (document_id, collection_id, source_path, content_hash, created_at, updated_at)
			 VALUES ('hist-doc', 'hist-col', 'sources/a.md', 'hash', 1, 1)`,
		}},
	}

	// 用例与包内清单一一对应：清单增删表时必须同步扩用例，否则这里先红。
	require.Len(t, cases, len(runtimeHistoryTables))
	for i, table := range runtimeHistoryTables {
		require.Equal(t, table, cases[i].table, "用例顺序必须与 runtimeHistoryTables 一致")
	}

	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			clearRuntimeHistory(t, db)
			has, err := store.HasRuntimeHistory(ctx)
			require.NoError(t, err)
			require.False(t, has, "空库不得判为有业务历史")

			for _, statement := range tc.seed {
				_, seedErr := db.pool.ExecContext(ctx, statement)
				require.NoError(t, seedErr)
			}
			has, err = store.HasRuntimeHistory(ctx)
			require.NoError(t, err)
			require.True(t, has, "%s 有行必须计为业务历史", tc.table)
		})
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := store.HasRuntimeHistory(cancelled)
	require.Error(t, err, "读失败必须上抛，不得吞成 false")
}

// TestRuntimeHistory_ManagementOperationsExcluded 判排除项：首次准备自己写的 pending
// 审计（`management_operations`）不属于业务历史。同夹具里补一条 run 证明 false 不是
// "什么都没查"——查询本身是有效的。
// TestRuntimeHistory_ManagementOperationsExcluded keeps the audit table out of the verdict.
func TestRuntimeHistory_ManagementOperationsExcluded(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t)
	store := NewInstanceSecretsStore(db)

	_, err := db.pool.ExecContext(ctx,
		`INSERT INTO management_operations
		 (operation_id, action, target_kind, target_id, actor_subject, auth_method, source, status, created_at)
		 VALUES ('hist-op', 'system.secrets_init', 'system', 'master_key', 'cli:test', 'cli', 'cli', 'pending', 1)`)
	require.NoError(t, err)

	has, err := store.HasRuntimeHistory(ctx)
	require.NoError(t, err)
	require.False(t, has, "management_operations 不得计入业务历史——否则首次准备会把自己判成存量实例")

	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO runs
		 (run_id, session_id, agent, channel, user_id, trigger, status, started_at,
		  caller_id, authorizing_kind, key_id, actor)
		 VALUES ('hist-run-2', NULL, 'demo', 'web', 'u1', 'user', 'completed', 1,
		         'c1', 'caller-key', 'key-hist', 'caller')`)
	require.NoError(t, err)
	has, err = store.HasRuntimeHistory(ctx)
	require.NoError(t, err)
	require.True(t, has, "同夹具里的真实业务行必须仍被识别")
}
