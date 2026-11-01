//go:build integration

package postgres

// 本文件是 `embedding_calls` 两阶段审计的有库用例（T128 面的审计证据）：真 schema、
// 真约束、真外键，只有调用结果的世界被构造出来。
//
// 判别力（逆改点）：
//   - 把 `FinishEmbedding` 的 `WHERE status='pending'` 去掉 → `TestEmbeddingCallRepo_WriteOnce`
//     的第二次回填成功，用例红；
//   - 把"未观察到响应"映射成 `failed` → `TestEmbeddingCallRepo_OutcomeDoesNotFabricateFailure`
//     的 transport 子例红；
//   - 把 `prompt_tokens` 固定写 0（忽略实参）→ 成功子例的用量断言红；
//   - 去掉 `input_count` 的前置拒绝 → 非正输入子例以驱动错误通过（断言文案，故仍红）。
//
// The cases run against the real schema and constraints; only the call outcome is
// constructed. The header lists the mutation each assertion is designed to catch.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/knowledge"
)

// embFixedNow 是本文件的固定时钟：时间列按 Unix 毫秒落库，固定值让断言可复算。
var embFixedNow = time.Date(2026, 10, 30, 9, 30, 0, 0, time.UTC)

// embTestDB 在隔离 schema 上建已迁移句柄并注入固定时钟。
func embTestDB(t *testing.T) *DB {
	t.Helper()
	db := knTestDB(t)
	db.now = func() time.Time { return embFixedNow }
	return db
}

// embSeedJob 建一条完整的摄取作业链（集合 → 文档 → 版本 → 作业），返回作业标识。
// 外键要求作业行真实存在；这条链同时证明 `ingest_job_id` 的引用是真的。
func embSeedJob(t *testing.T, db *DB, jobID string) string {
	t.Helper()
	ctx := context.Background()
	at := knNow.UnixMilli()
	const (
		collectionID = "col_emb"
		documentID   = "doc_emb"
		versionID    = "ver_emb"
	)
	knSeedCollection(t, db, collectionID, "emb")
	_, err := db.pool.ExecContext(ctx,
		`INSERT INTO kb_documents (document_id, collection_id, source_path, content_hash, created_at, updated_at)
		 VALUES ($1, $2, 'docs/a.md', 'h1', $3, $3)`, documentID, collectionID, at)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO kb_document_versions
		   (version_id, document_id, content_hash, status, parser_version, chunker_version, created_at, updated_at)
		 VALUES ($1, $2, 'h1', 'pending', 'text-v1', 'chunk-v1', $3, $3)`, versionID, documentID, at)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO kb_ingest_jobs
		   (ingest_job_id, document_id, version_id, stage, attempt, status, started_at)
		 VALUES ($1, $2, $3, 'embed', 1, 'running', $4)`, jobID, documentID, versionID, at)
	require.NoError(t, err)
	return jobID
}

// embRow 是 `embedding_calls` 行的断言视图。
type embRow struct {
	RunID        sql.NullString
	JobID        sql.NullString
	Model        string
	Dimensions   int
	InputCount   int
	PromptTokens int64
	Status       string
	ErrorKind    sql.NullString
	CreatedAt    int64
	FinishedAt   sql.NullInt64
}

// embLoad 按行 id 读回整行。
func embLoad(t *testing.T, db *DB, id string) embRow {
	t.Helper()
	var row embRow
	err := db.pool.QueryRowContext(context.Background(),
		`SELECT run_id, ingest_job_id, model, dimensions, input_count, prompt_tokens,
		        status, error_kind, created_at, finished_at
		   FROM embedding_calls WHERE id = $1`, id).Scan(
		&row.RunID, &row.JobID, &row.Model, &row.Dimensions, &row.InputCount,
		&row.PromptTokens, &row.Status, &row.ErrorKind, &row.CreatedAt, &row.FinishedAt)
	require.NoError(t, err)
	return row
}

// embCount 数当前 schema 里的审计行数（拒绝路径的"零写入"证据）。
func embCount(t *testing.T, db *DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM embedding_calls`).Scan(&count))
	return count
}

// TestEmbeddingCallRepo_BeginWritesPending 守预写形态：`pending` + 用量 0 + 未完成
// （finished_at NULL）+ 维度来自装配 + `run_id` 为空（摄取路径只带作业标识）。
func TestEmbeddingCallRepo_BeginWritesPending(t *testing.T) {
	db := embTestDB(t)
	jobID := embSeedJob(t, db, "job_1")
	repo := NewEmbeddingCallRepo(db, knowledge.VectorDimensions)

	callID, err := repo.BeginEmbedding(t.Context(), jobID, "text-embedding-v4", 3)
	require.NoError(t, err)

	row := embLoad(t, db, callID)
	require.Equal(t, "pending", row.Status)
	require.Equal(t, "text-embedding-v4", row.Model)
	require.Equal(t, knowledge.VectorDimensions, row.Dimensions)
	require.Equal(t, 3, row.InputCount)
	require.Equal(t, int64(0), row.PromptTokens)
	require.False(t, row.ErrorKind.Valid)
	require.False(t, row.FinishedAt.Valid)
	require.False(t, row.RunID.Valid)
	require.Equal(t, jobID, row.JobID.String)
	require.Equal(t, embFixedNow.UnixMilli(), row.CreatedAt)
}

// TestEmbeddingCallRepo_BeginFailsClosed 守预写失败即"不发出调用"：非正输入在
// 本层拒绝（CHECK 的更早失败，带点名文案），不存在的作业被外键拒绝，两者都零写入。
func TestEmbeddingCallRepo_BeginFailsClosed(t *testing.T) {
	db := embTestDB(t)
	embSeedJob(t, db, "job_1")
	repo := NewEmbeddingCallRepo(db, knowledge.VectorDimensions)

	for _, count := range []int{0, -1} {
		_, err := repo.BeginEmbedding(t.Context(), "job_1", "text-embedding-v4", count)
		require.Error(t, err)
		require.Contains(t, err.Error(), "input_count")
	}

	_, err := repo.BeginEmbedding(t.Context(), "job_missing", "text-embedding-v4", 1)
	require.Error(t, err, "不存在的作业必须被外键拒绝")
	require.Equal(t, 0, embCount(t, db))
}

// TestEmbeddingCallRepo_FinishSuccess 守成功终态：状态与用量一起落库，
// `error_kind` 留 NULL，`finished_at` 取回填时刻。
func TestEmbeddingCallRepo_FinishSuccess(t *testing.T) {
	db := embTestDB(t)
	jobID := embSeedJob(t, db, "job_1")
	repo := NewEmbeddingCallRepo(db, knowledge.VectorDimensions)

	callID, err := repo.BeginEmbedding(t.Context(), jobID, "text-embedding-v4", 2)
	require.NoError(t, err)
	require.NoError(t, repo.FinishEmbedding(t.Context(), callID, 42, nil))

	row := embLoad(t, db, callID)
	require.Equal(t, "success", row.Status)
	require.Equal(t, int64(42), row.PromptTokens)
	require.False(t, row.ErrorKind.Valid)
	require.True(t, row.FinishedAt.Valid)
	require.Equal(t, embFixedNow.UnixMilli(), row.FinishedAt.Int64)
}

// TestEmbeddingCallRepo_OutcomeDoesNotFabricateFailure 守终态判据：只有"观察到提供方
// 响应"才是 `failed`；没有响应的结局一律 `unknown`——**不得**把未知写成已知失败。
func TestEmbeddingCallRepo_OutcomeDoesNotFabricateFailure(t *testing.T) {
	cases := []struct {
		name    string
		callErr error
		status  string
		kind    string // 空串 = NULL
	}{
		{"失败响应", &knowledge.EmbeddingError{Provider: "p", Status: 400, Message: "bad"}, "failed", "external_error"},
		{"协议违约响应", &knowledge.EmbeddingError{Provider: "p", Status: 200, Message: "unparsable"}, "failed", "external_error"},
		{"传输失败无响应", &knowledge.EmbeddingError{Provider: "p", Message: "dial fail"}, "unknown", "external_error"},
		{"取消无响应", fmt.Errorf("aborted: %w", context.Canceled), "unknown", "timeout"},
		{"本地错误", errors.New("空输入"), "unknown", "unknown"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := embTestDB(t)
			jobID := embSeedJob(t, db, "job_1")
			repo := NewEmbeddingCallRepo(db, knowledge.VectorDimensions)

			callID, err := repo.BeginEmbedding(t.Context(), jobID, "text-embedding-v4", 1)
			require.NoError(t, err)
			require.NoError(t, repo.FinishEmbedding(t.Context(), callID, 0, tc.callErr))

			row := embLoad(t, db, callID)
			require.Equal(t, tc.status, row.Status)
			if tc.kind == "" {
				require.False(t, row.ErrorKind.Valid)
			} else {
				require.Equal(t, tc.kind, row.ErrorKind.String)
			}
			require.True(t, row.FinishedAt.Valid)
		})
	}
}

// TestEmbeddingCallRepo_WriteOnce 守"终态只写一次"：第二次回填必须报错且不改行。
// 逆改点：去掉 WHERE 的 pending 条件 → 第二次回填成功，本用例红。
func TestEmbeddingCallRepo_WriteOnce(t *testing.T) {
	db := embTestDB(t)
	jobID := embSeedJob(t, db, "job_1")
	repo := NewEmbeddingCallRepo(db, knowledge.VectorDimensions)

	callID, err := repo.BeginEmbedding(t.Context(), jobID, "text-embedding-v4", 1)
	require.NoError(t, err)
	require.NoError(t, repo.FinishEmbedding(t.Context(), callID, 7, nil))

	err = repo.FinishEmbedding(t.Context(), callID, 9,
		&knowledge.EmbeddingError{Provider: "p", Status: 500, Message: "late"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "终态只写一次")

	row := embLoad(t, db, callID)
	require.Equal(t, "success", row.Status)
	require.Equal(t, int64(7), row.PromptTokens)
}

// TestEmbeddingCallRepo_FinishRejectsBadInput 守入参闸门：非数字标识与负用量都拒绝，
// 且不改动任何行。
func TestEmbeddingCallRepo_FinishRejectsBadInput(t *testing.T) {
	db := embTestDB(t)
	jobID := embSeedJob(t, db, "job_1")
	repo := NewEmbeddingCallRepo(db, knowledge.VectorDimensions)

	callID, err := repo.BeginEmbedding(t.Context(), jobID, "text-embedding-v4", 1)
	require.NoError(t, err)

	err = repo.FinishEmbedding(t.Context(), "emb_"+jobID, 0, nil)
	require.Error(t, err, "非十进制 call_id 必须拒绝")

	require.Error(t, repo.FinishEmbedding(t.Context(), callID, -1, nil))

	row := embLoad(t, db, callID)
	require.Equal(t, "pending", row.Status)
	require.Equal(t, int64(0), row.PromptTokens)

	// 不存在的行同样按"终态只写一次"拒绝（影响行数为 0），不会静默成功。
	require.Error(t, repo.FinishEmbedding(t.Context(), "9223372036854775807", 0, nil))
}
