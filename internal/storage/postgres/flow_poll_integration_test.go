//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/flow"
)

type flowPollQueries struct {
	Queryer
	count int
}

func (q *flowPollQueries) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.count++
	return q.Queryer.QueryContext(ctx, query, args...)
}

func (q *flowPollQueries) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.count++
	return q.Queryer.QueryRowContext(ctx, query, args...)
}

// 只统计真实PG读请求；32任务不能每轮各重读相同不可变定义。
// Count actual PG statements rather than substituting an in-memory repository.
func TestFlow_PollLoadsBoundedBatchWithoutPerTaskRoundTrips(t *testing.T) {
	store, version := newFlowStoreTest(t)
	service := flowServiceForStore(store)
	for i := range 32 {
		_, err := service.Start(t.Context(), startFlowRequest(version, fmt.Sprintf("poll-%02d", i)))
		require.NoError(t, err)
	}
	queries := &flowPollQueries{Queryer: store.db.pool}
	store.db.pool.SetMaxOpenConns(1)
	storedVersion, err := store.ReadDefinition(t.Context(), flow.DefinitionRef{UID: version.DefinitionUID, Version: version.Version})
	require.NoError(t, err)
	tasks, err := readFlowRoots(t.Context(), queries, true, time.Now().Add(time.Second).UnixMilli(), 32)
	require.NoError(t, err)
	require.Len(t, tasks, 32)
	require.Equal(t, 2, queries.count, "one root batch and one immutable definition read, not 65 sequential round trips")
	for _, task := range tasks {
		require.Equal(t, version.ContentHash, task.AcceptedVersion.ContentHash)
		require.Equal(t, storedVersion.Definition, task.AcceptedVersion.Definition)
		require.Equal(t, flow.Queued, task.Status)
		require.NotEmpty(t, task.Contract.Scope.CallerID)
		require.Equal(t, task.ID, task.Revision.TaskID)
	}
}
