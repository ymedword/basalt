package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// callRow 是 llm_calls 一行的判卷形态（列全部显式——可空列用 Null 形态，判 NULL 与判空串不同）。
// callRow is the assertion shape of one llm_calls row; nullable columns stay nullable so
// NULL and the empty string cannot be confused.
type callRow struct {
	ID               int64
	RunID            string
	SessionID        sql.NullString
	Round            int
	Provider         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	DurationMS       int64
	Status           string
	ErrorMessage     sql.NullString
	ErrorKind        sql.NullString
	ConfigHash       sql.NullString
	CreatedAt        int64
}

// openTempDB 给每个用例一个独立的临时真库（测试隔离纪律：不共享样例库）。
// openTempDB hands every test its own temporary real database.
func openTempDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenWritable(context.Background(), filepath.Join(t.TempDir(), "basalt.db"), SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// readCallRow 按 id 读回审计行（in-package 判卷走 db.read——生产写入路径仍唯一经仓储）。
// readCallRow reads one audit row back by id through the read pool.
func readCallRow(t *testing.T, db *DB, id int64) callRow {
	t.Helper()
	var row callRow
	err := db.read.QueryRowContext(context.Background(),
		`SELECT id, run_id, session_id, round, provider, model,
		        prompt_tokens, completion_tokens, total_tokens, duration_ms,
		        status, error_message, error_kind, config_hash, created_at
		   FROM llm_calls WHERE id = ?`, id).
		Scan(&row.ID, &row.RunID, &row.SessionID, &row.Round, &row.Provider, &row.Model,
			&row.PromptTokens, &row.CompletionTokens, &row.TotalTokens, &row.DurationMS,
			&row.Status, &row.ErrorMessage, &row.ErrorKind, &row.ConfigHash, &row.CreatedAt)
	require.NoError(t, err)
	return row
}

// TestLLMCallRepo_BeginCallInsertsPendingRow：预写行的列值——status 由仓储**固定写**
// pending（不由调用方给）、duration_ms=0、可空列写 NULL、created_at 存 Unix 毫秒。
// TestLLMCallRepo_BeginCallInsertsPendingRow pins the pre-written row: pending is written
// by the repo itself, nullable columns stay NULL, created_at is stored in milliseconds.
func TestLLMCallRepo_BeginCallInsertsPendingRow(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewLLMCallRepo(db)

	created := time.Date(2026, 10, 16, 3, 4, 5, 678*int(time.Millisecond), time.UTC)
	id, err := repo.BeginCall(ctx, core.LLMCall{
		RunID: "run-1", Round: 2, Provider: "fake", Model: "m", CreatedAt: created,
	})
	require.NoError(t, err)
	require.Positive(t, id)

	row := readCallRow(t, db, id)
	require.Equal(t, core.LLMCallPending, core.LLMCallStatus(row.Status), "预写态恒为 pending")
	require.Equal(t, "run-1", row.RunID)
	require.False(t, row.SessionID.Valid, "空 session_id 写 NULL（列可空）")
	require.Equal(t, 2, row.Round)
	require.Equal(t, "fake", row.Provider)
	require.Equal(t, "m", row.Model)
	require.EqualValues(t, 0, row.DurationMS, "预写时耗时未定，写 0")
	require.False(t, row.ErrorMessage.Valid, "预写行无失败原文")
	require.False(t, row.ErrorKind.Valid, "预写行无失败类别")
	require.False(t, row.ConfigHash.Valid, "config_hash 空 → 写 NULL（§2.4 裁决注 3）")
	require.Equal(t, created.UnixMilli(), row.CreatedAt, "created_at 存 Unix 毫秒 UTC")
}

// TestLLMCallRepo_FinishCallUpdatesTerminalFields：终态回填的列值（成败、三 token 列、耗时、
// 失败类别）——成功时 error_kind 与 error_message 写 NULL。
// TestLLMCallRepo_FinishCallUpdatesTerminalFields pins the terminal columns.
func TestLLMCallRepo_FinishCallUpdatesTerminalFields(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewLLMCallRepo(db)

	id, err := repo.BeginCall(ctx, core.LLMCall{RunID: "run-1", Round: 1, Provider: "fake", Model: "m"})
	require.NoError(t, err)

	require.NoError(t, repo.FinishCall(ctx, id, core.LLMCallResult{
		Status: core.LLMCallSuccess, PromptTokens: 11, CompletionTokens: 22, TotalTokens: 33,
		Duration: 250 * time.Millisecond,
	}))

	row := readCallRow(t, db, id)
	require.Equal(t, core.LLMCallSuccess, core.LLMCallStatus(row.Status))
	require.Equal(t, 11, row.PromptTokens)
	require.Equal(t, 22, row.CompletionTokens)
	require.Equal(t, 33, row.TotalTokens)
	require.EqualValues(t, 250, row.DurationMS, "耗时落毫秒列")
	require.False(t, row.ErrorKind.Valid, "成功行 error_kind 为 NULL")
	require.False(t, row.ErrorMessage.Valid, "成功行 error_message 为 NULL")

	failedID, err := repo.BeginCall(ctx, core.LLMCall{RunID: "run-1", Round: 2, Provider: "fake", Model: "m"})
	require.NoError(t, err)
	require.NoError(t, repo.FinishCall(ctx, failedID, core.LLMCallResult{
		Status: core.LLMCallFailed, ErrorMessage: "供应商 500", ErrorKind: core.ErrorKindExternalError,
		Duration: 3 * time.Millisecond,
	}))

	failedRow := readCallRow(t, db, failedID)
	require.Equal(t, core.LLMCallFailed, core.LLMCallStatus(failedRow.Status))
	require.Equal(t, "供应商 500", failedRow.ErrorMessage.String)
	require.Equal(t, core.ErrorKindExternalError, core.ErrorKind(failedRow.ErrorKind.String))
	require.EqualValues(t, 3, failedRow.DurationMS)
}

// TestLLMCallRepo_FinishCallTruncatesErrorMessageAt4KBWithoutBreakingRunes —— 契约 §7.4。
// 断言：长度上界是字节 4 KB；且末尾是合法 UTF-8（按 rune 边界回退，不切断半个汉字）。
func TestLLMCallRepo_FinishCallTruncatesErrorMessageAt4KBWithoutBreakingRunes(t *testing.T) {
	ctx := context.Background()
	repo := NewLLMCallRepo(openTempDB(t))
	id, err := repo.BeginCall(ctx, core.LLMCall{RunID: "run-test", Round: 1, Provider: "fake", Model: "m"})
	require.NoError(t, err)

	long := strings.Repeat("超长错误", 4000) // 远超 4 KB，且是多字节字符
	require.NoError(t, repo.FinishCall(ctx, id, core.LLMCallResult{
		Status: core.LLMCallFailed, ErrorMessage: long, ErrorKind: core.ErrorKindExternalError,
	}))

	got := readErrorMessage(t, repo, id)
	require.LessOrEqual(t, len(got), 4*1024, "截断上界 = 4 KB")
	require.True(t, utf8.ValidString(got), "截断不得切断 UTF-8 字符")
	require.NotEqual(t, long, got, "超长原文必须被截断")
	require.True(t, strings.HasPrefix(long, got), "截断保留头部（不是掐头去尾）")
	require.NotEmpty(t, got, "截断不得把文案抹成空串")
}

// readErrorMessage 读回失败原文——截断用例的判卷出口。
// readErrorMessage reads the stored failure message back.
func readErrorMessage(t *testing.T, repo *LLMCallRepo, id int64) string {
	t.Helper()
	var msg string
	err := repo.db.read.QueryRowContext(context.Background(),
		`SELECT error_message FROM llm_calls WHERE id = ?`, id).Scan(&msg)
	require.NoError(t, err)
	return msg
}

// TestLLMCallRepo_FinishCallRejectsNonPendingRow：终态只写一次——非 pending 行拒绝并点名 id，
// 原值不被覆盖（"终态恒等于第一次判定"是审计可解释性的前提）。
// TestLLMCallRepo_FinishCallRejectsNonPendingRow: the terminal state is written once.
func TestLLMCallRepo_FinishCallRejectsNonPendingRow(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewLLMCallRepo(db)

	id, err := repo.BeginCall(ctx, core.LLMCall{RunID: "run-1", Round: 1, Provider: "fake", Model: "m"})
	require.NoError(t, err)
	require.NoError(t, repo.FinishCall(ctx, id, core.LLMCallResult{
		Status: core.LLMCallSuccess, PromptTokens: 5, Duration: time.Millisecond,
	}))

	err = repo.FinishCall(ctx, id, core.LLMCallResult{
		Status: core.LLMCallFailed, ErrorMessage: "改判", ErrorKind: core.ErrorKindUnknown,
	})
	require.Error(t, err, "非 pending 行不得二次回填")
	require.Contains(t, err.Error(), "1", "错误必须点名行 id")

	row := readCallRow(t, db, id)
	require.Equal(t, core.LLMCallSuccess, core.LLMCallStatus(row.Status), "先写的终态不被覆盖")
	require.Equal(t, 5, row.PromptTokens)
	require.False(t, row.ErrorMessage.Valid)
}

// TestLLMCallRepo_CrashWindowLeavesPendingForRecovery：崩溃恢复窗口——进程消失后留下的
// 持久化形态就是 pending 行（恢复归一化 pending → unknown 归 US-4，本用例只判窗口形态：
// 无半写、无终态伪造）。
// TestLLMCallRepo_CrashWindowLeavesPendingForRecovery judges only the persisted shape of a
// crash: a pending row with nothing half-written. Normalization belongs to US-4.
func TestLLMCallRepo_CrashWindowLeavesPendingForRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")

	crashing, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	repo := NewLLMCallRepo(crashing)
	id, err := repo.BeginCall(ctx, core.LLMCall{RunID: "run-crash", Round: 1, Provider: "fake", Model: "m"})
	require.NoError(t, err)
	require.NoError(t, crashing.Close()) // 进程消失：调用已发出、终态未回填

	reopened, err := OpenWritable(ctx, path, SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	row := readCallRow(t, reopened, id)
	require.Equal(t, core.LLMCallPending, core.LLMCallStatus(row.Status), "崩溃留下的是 pending 行")
	require.EqualValues(t, 0, row.DurationMS, "无半写：耗时不落")
	require.False(t, row.ErrorMessage.Valid, "无半写：不伪造失败原文")
	require.False(t, row.ErrorKind.Valid, "无半写：不伪造失败类别")
}

// TestLLMCallRepo_ConcurrentBeginCall：并发预写（写池 1 连接串行）→ -race 无告警、id 唯一。
// TestLLMCallRepo_ConcurrentBeginCall exercises concurrent pre-writes under -race.
func TestLLMCallRepo_ConcurrentBeginCall(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	repo := NewLLMCallRepo(db)

	const n = 8
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := repo.BeginCall(ctx, core.LLMCall{RunID: "run-parallel", Round: i + 1, Provider: "fake", Model: "m"})
			require.NoError(t, err)
			ids[i] = id
		}()
	}
	wg.Wait()

	seen := map[int64]bool{}
	for _, id := range ids {
		require.False(t, seen[id], "行 id 必须唯一，重复 %d", id)
		seen[id] = true
	}

	var count int
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM llm_calls WHERE status = 'pending'`).Scan(&count))
	require.Equal(t, n, count, "并发预写全部落库且均为 pending")
}
