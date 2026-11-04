//go:build integration

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 从真实v7迁移，逐字比较原会话、消息和两类审计；不以空库升级代替兼容证据。
// Upgrade populated v7 and compare the original records, not just an empty schema.
func TestFlow_MigrateV7PreservesOriginalExecutionAndAudit(t *testing.T) {
	conn := migrationConn(t)
	ctx := t.Context()
	require.NoError(t, ensureSchema(ctx, conn, 7))
	_, err := conn.ExecContext(ctx, `
INSERT INTO sessions(session_id,profile_name,channel,user_id,message_count,created_at,last_active_at,caller_id)
VALUES ('v7-session','reader','web','original-user',4,1000,2000,'local-admin');
INSERT INTO runs(run_id,session_id,agent,channel,user_id,trigger,status,reason,iterations,tool_calls,started_at,finished_at,caller_id,authorizing_kind,actor)
VALUES ('v7-run','v7-session','reader','web','original-user','user','completed','original reason',2,1,1000,2000,'local-admin','admin-builtin','admin-cli'),
       ('v7-active','v7-session','reader','web','original-user','user','running',NULL,1,1,2100,NULL,'local-admin','admin-builtin','admin-cli');
INSERT INTO messages(session_id,run_id,seq,role,content,tool_calls,tool_call_id,created_at)
VALUES ('v7-session','v7-run',1,'user','original question',NULL,NULL,1000),
       ('v7-session','v7-run',2,'assistant',NULL,'[{"id":"v7-call","name":"read_file","arguments":"{\"path\":\"original.txt\"}"}]',NULL,1100),
       ('v7-session','v7-run',3,'tool','original source bytes',NULL,'v7-call',1200),
       ('v7-session','v7-run',4,'assistant','original answer',NULL,NULL,2000);
INSERT INTO llm_calls(run_id,session_id,round,provider,model,prompt_tokens,completion_tokens,total_tokens,duration_ms,status,created_at)
VALUES ('v7-run','v7-session',1,'pinned-provider','pinned-model',5,3,8,50,'success',1000),
       ('v7-active','v7-session',1,'pinned-provider','pinned-model',0,0,0,0,'pending',2100);
INSERT INTO tool_invocations(run_id,session_id,tool_call_id,tool_name,input_json,result_json,status,duration_ms,started_at,finished_at)
VALUES ('v7-run','v7-session','v7-call','read_file','{"path":"original.txt"}','{"content":"original source bytes"}','success',20,1100,1200),
       ('v7-active','v7-session','v7-unknown','write_file','{"path":"out.txt","content":"sent once"}',NULL,'unknown',NULL,2100,NULL);`)
	require.NoError(t, err)
	queries := []string{
		`SELECT jsonb_agg(to_jsonb(s) ORDER BY session_id)::text FROM sessions s`,
		`SELECT jsonb_agg(to_jsonb(m) ORDER BY session_id,seq)::text FROM messages m`,
		`SELECT jsonb_agg(to_jsonb(r)-'flow_task_id'-'flow_node_attempt_id' ORDER BY run_id)::text FROM runs r`,
		`SELECT jsonb_agg(to_jsonb(l) ORDER BY id)::text FROM llm_calls l`,
		`SELECT jsonb_agg(to_jsonb(i) ORDER BY id)::text FROM tool_invocations i`,
	}
	before := make([]string, len(queries))
	for i, query := range queries {
		require.NoError(t, conn.QueryRowContext(ctx, query).Scan(&before[i]))
	}
	require.NoError(t, ensureSchema(ctx, conn, 8))
	for i, query := range queries {
		var after string
		require.NoError(t, conn.QueryRowContext(ctx, query).Scan(&after))
		require.JSONEq(t, before[i], after, "v7 record projection %d must remain exact", i)
	}
	var original, untouched, flowTasks int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE flow_task_id IS NULL AND flow_node_attempt_id IS NULL) FROM runs`).Scan(&original, &untouched))
	require.Equal(t, 2, original)
	require.Equal(t, original, untouched)
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT count(*) FROM flow_tasks`).Scan(&flowTasks))
	require.Zero(t, flowTasks, "migration must not fabricate tasks or execute recovery")
	version, err := schemaVersion(ctx, conn)
	require.NoError(t, err)
	require.Equal(t, 8, version)
	require.NoError(t, ensureSchema(ctx, conn, 8), "repeat startup is idempotent")
}
