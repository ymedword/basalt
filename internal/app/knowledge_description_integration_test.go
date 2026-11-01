//go:build integration

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

func TestKnowledgeDescriptionAdmin_AuditsAndRejectsStale(t *testing.T) {
	ctx := context.Background()
	db := pgtest.NewTestDB(t)
	store := postgres.NewKnowledgeCatalogStore(db)
	ops := postgres.NewManagementStore(db)
	logger, _ := newCapturedLogger()
	admin := &knowledgeDescriptionAdmin{store: store, operations: ops, now: time.Now, logger: logger}
	collection, err := store.CreateCollection(ctx, core.LocalAdminPrincipal(), "手册", "")
	require.NoError(t, err)
	actor := core.Actor{Subject: "local_trust", Method: core.AuthMethodLocalTrust, Source: "127.0.0.1"}
	revision, err := admin.UpdateDescription(ctx, actor, collection.CollectionID, "内部应急流程", 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, revision)
	_, err = admin.UpdateDescription(ctx, actor, collection.CollectionID, "过时内容", 0)
	require.ErrorIs(t, err, core.ErrPreconditionFailed)
	rows, err := ops.ListOperations(ctx, core.OperationFilter{Action: "knowledge_collection.description_update"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	statuses := map[core.OperationStatus]bool{}
	for _, row := range rows {
		statuses[row.Status] = true
		require.NotContains(t, string(row.DetailJSON), "内部应急流程")
		require.NotContains(t, string(row.DetailJSON), "过时内容")
	}
	require.True(t, statuses[core.OperationSuccess])
	require.True(t, statuses[core.OperationFailed])
	admin.operations = failingBeginOperations{OperationStore: ops, err: errors.New("audit unavailable")}
	_, err = admin.UpdateDescription(ctx, actor, collection.CollectionID, "不应写入", 1)
	require.Error(t, err)
	collections, err := store.ListCollections(ctx, "", false)
	require.NoError(t, err)
	require.Equal(t, "内部应急流程", collections[0].Description)
	require.EqualValues(t, 1, collections[0].DescriptionRevision)

	// 审计身份不全 → **预写之前**就拒绝：空 `ActorSubject` 的管理操作行是一条无人可查
	// 的变更记录，写下去比不写更坏（宪法第 5 条 fail-closed）。既不留行、也不改简介。
	// 逆改点：删掉 UpdateDescription 的身份判定 → 本段出现第三条操作行、简介被改写。
	admin.operations = ops
	for _, bad := range []core.Actor{
		{Method: core.AuthMethodLocalTrust, Source: "127.0.0.1"},
		{Subject: "local_trust", Method: core.AuthMethodLocalTrust},
		{Subject: "local_trust", Method: core.AuthMethodMigration, Source: "127.0.0.1"},
	} {
		_, err = admin.UpdateDescription(ctx, bad, collection.CollectionID, "未认证写入", 1)
		require.Error(t, err, "审计身份不全必须拒绝（%+v）", bad)
	}
	rows, err = ops.ListOperations(ctx, core.OperationFilter{Action: "knowledge_collection.description_update"})
	require.NoError(t, err)
	require.Len(t, rows, 2, "被拒的请求不得留下操作行")
	collections, err = store.ListCollections(ctx, "", false)
	require.NoError(t, err)
	require.Equal(t, "内部应急流程", collections[0].Description)
	require.EqualValues(t, 1, collections[0].DescriptionRevision)
}
