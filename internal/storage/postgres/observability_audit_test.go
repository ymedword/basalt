//go:build integration

package postgres

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

func TestObservabilityStore_AuditFailureMetrics(t *testing.T) {
	db := portTestDB(t)
	var count atomic.Int64
	db.ObserveAuditWriteFailures(func() { count.Add(1) })
	_, err := db.pool.ExecContext(t.Context(), `CREATE FUNCTION obs_fail_audit_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit write failure'; END $$`)
	require.NoError(t, err)
	for _, table := range []string{"llm_calls", "tool_invocations", "embedding_calls", "management_operations"} {
		// 表名来自固定测试集合，故障只作用于本例的隔离schema。
		_, err := db.pool.ExecContext(t.Context(), `CREATE TRIGGER obs_fail_audit BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION obs_fail_audit_insert()`)
		require.NoError(t, err)
	}
	_, err = NewLLMCallRepo(db).BeginCall(t.Context(), core.LLMCall{RunID: "synthetic", Provider: "synthetic", Model: "synthetic", CreatedAt: time.Now()})
	require.Error(t, err)
	_, err = NewToolInvocationRepo(db).BeginInvocation(t.Context(), core.ToolInvocation{RunID: "synthetic", ToolName: "synthetic", CreatedAt: time.Now()})
	require.Error(t, err)
	_, err = NewEmbeddingCallRepo(db, 512).BeginEmbedding(t.Context(), "", "synthetic", 1)
	require.Error(t, err)
	operation := core.OperationRecord{OperationID: "obs-failed", Action: "system.baseline_v2", TargetKind: "system", TargetID: "system", ActorSubject: core.ActorSubjectMigration, AuthMethod: core.AuthMethodMigration, Source: "migration", CreatedAt: time.Now()}
	err = NewManagementStore(db).BeginOperation(t.Context(), operation)
	require.Error(t, err)
	require.Equal(t, int64(4), count.Load(), "all four authoritative audit pre-write failures must be observed")
	operation.DetailJSON = []byte("invalid-json")
	err = NewManagementStore(db).BeginOperation(t.Context(), operation)
	require.Error(t, err)
	require.Equal(t, int64(4), count.Load(), "input validation is not a failed database write")
	db.ObserveAuditWriteFailures(func() { panic("CANARY_METRIC_CALLBACK") })
	require.NotPanics(t, func() {
		_, err := NewLLMCallRepo(db).BeginCall(t.Context(), core.LLMCall{RunID: "synthetic", Provider: "synthetic", Model: "synthetic"})
		require.ErrorContains(t, err, "synthetic audit write failure", "callback must preserve the original database error")
	})
}

func TestObservabilityStore_AtomicManagementAuditFailureObserved(t *testing.T) {
	db := portTestDB(t)
	seedOperation(t, db, "obs-atomic-op")
	var count atomic.Int64
	db.ObserveAuditWriteFailures(func() { count.Add(1) })
	_, err := db.pool.ExecContext(t.Context(), `CREATE FUNCTION obs_fail_success_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.status='success' THEN RAISE EXCEPTION 'synthetic success audit failure'; END IF; RETURN NEW; END $$`)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(t.Context(), `CREATE TRIGGER obs_fail_success_audit BEFORE UPDATE ON management_operations FOR EACH ROW EXECUTE FUNCTION obs_fail_success_audit()`)
	require.NoError(t, err)
	rec := sampleArchive("obs-atomic-archive", "synthetic", "obs-atomic-op")
	err = NewAgentAdminStore(db).InsertArchive(t.Context(), rec, "obs-atomic-op")
	require.Error(t, err)
	require.Equal(t, int64(1), count.Load(), "the shared audit SQL failure must be counted from atomic archive transactions")
	err = NewManagementStore(db).FinishOperation(t.Context(), "obs-atomic-op", core.OperationFailed, "ARCHIVE_FAILED", "", nil, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(1), count.Load(), "successful compensation must not erase or double-count the failure")
	archives, err := NewAgentAdminStore(db).ListArchives(t.Context())
	require.NoError(t, err)
	require.Empty(t, archives)
}
