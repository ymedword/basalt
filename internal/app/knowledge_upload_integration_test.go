//go:build integration

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

func TestKnowledgeUpload_NoOverwriteAndAudit(t *testing.T) {
	ctx := context.Background()
	db := pgtest.NewTestDB(t)
	ops := postgres.NewManagementStore(db)
	logger, _ := newCapturedLogger()
	root := t.TempDir()
	s := &knowledgeUploadAdmin{root: root, maxBytes: 128, operations: ops, now: time.Now, logger: logger}
	actor := core.Actor{Subject: "local_trust", Method: core.AuthMethodLocalTrust, Source: "127.0.0.1"}
	path, err := s.Upload(ctx, actor, "文档.md", "原始正文")
	require.NoError(t, err)
	require.Equal(t, "uploads/文档.md", path)
	_, err = s.Upload(ctx, actor, "文档.md", "覆盖")
	require.ErrorIs(t, err, core.ErrConflict)
	raw, err := os.ReadFile(filepath.Join(root, "uploads", "文档.md"))
	require.NoError(t, err)
	require.Equal(t, "原始正文", string(raw))
	for _, name := range []string{"../escape.md", "x/y.md", `x\y.md`, "code.exe"} {
		_, err := s.Upload(ctx, actor, name, "data")
		require.Error(t, err)
	}
	_, err = s.Upload(ctx, actor, "too-large.txt", string(make([]byte, 129)))
	require.Error(t, err)
	rows, err := ops.ListOperations(ctx, core.OperationFilter{Action: "knowledge_document.upload"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// 只数行数不够：预写之后**没有**收尾（`FinishOperation` 漏调）同样留下两行，而两行
	// 都是 `pending`——那正是崩溃窗口的形态，不能当成功证据。逐行判终态 + 结果码。
	// 逆改点：删掉任一 FinishOperation → 该行停在 pending，本段红。
	statuses := make([]core.OperationStatus, 0, len(rows))
	for _, row := range rows {
		require.NotEqual(t, core.OperationPending, row.Status, "操作行不得停在 pending：%s", row.TargetID)
		require.NotEmpty(t, row.FinishedAt, "终态必须带结束时间：%s", row.TargetID)
		require.NotContains(t, string(row.DetailJSON), "原始正文")
		statuses = append(statuses, row.Status)
	}
	// 两次同名上传：一次成功、一次被 409 拒绝，两种终态都要出现——全成功说明"不覆盖"
	// 没生效，全失败说明首次也没成，两者都不该过。
	require.ElementsMatch(t, []core.OperationStatus{core.OperationSuccess, core.OperationFailed}, statuses)
	s.operations = failingBeginOperations{OperationStore: ops, err: errors.New("audit unavailable")}
	_, err = s.Upload(ctx, actor, "blocked.txt", "data")
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(root, "uploads", "blocked.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
