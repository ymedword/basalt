package storage

// 本文件判卷 HTTP 会话读面的四条 SQL（颗粒度文档 §3.1 固定语句）：会话详情读取、
// 幂等创建、幂等归档、最近 N 条尾读。与 session_store_test.go 同一口径：既看仓储
// 返回面，也在需要时直读列面。
// The HTTP-facing session SQL: detail read, idempotent create, idempotent archive
// and the latest-N message tail.

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestSessionStore_GetSessionReadsStatus 判卷 active / archived + archived_at /
// 未知 id 三态：状态列与归档时间必须原样读回，未知 id 报不存在而不是零值。
// GetSession returns status and archived_at faithfully; a miss is fs.ErrNotExist.
func TestSessionStore_GetSessionReadsStatus(t *testing.T) {
	ctx := context.Background()
	store := NewSessionStore(openTempDB(t))

	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, store.UpsertSession(ctx, core.SessionRecord{
		ID: "s1", ProfileName: "ops-agent", Channel: "web", UserID: "u",
		CreatedAt: now, LastActiveAt: now,
	}))

	got, err := store.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "active", got.Status)
	require.Nil(t, got.ArchivedAt)
	require.Equal(t, "ops-agent", got.ProfileName)
	require.Equal(t, "web", got.Channel)
	require.Equal(t, "u", got.UserID)

	archivedAt := now.Add(time.Minute)
	require.NoError(t, store.ArchiveSession(ctx, "s1", archivedAt))
	got, err = store.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "archived", got.Status)
	require.NotNil(t, got.ArchivedAt)
	require.True(t, got.ArchivedAt.Equal(archivedAt))

	_, err = store.GetSession(ctx, "missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestSessionStore_GetOrCreateIsIdempotent 判卷幂等三例：首次新建 created=true；
// 重复元组返回既有行且 created_at 不被改写；已归档行原样返回、不复活。
// The first create reports created=true; a repeat returns the existing row with its
// original created_at, and an archived row stays archived.
func TestSessionStore_GetOrCreateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewSessionStore(openTempDB(t))

	now := time.Now().UTC().Truncate(time.Millisecond)
	first := core.SessionRecord{
		ID: "web-0123456789abcdef", ProfileName: "ops-agent", Channel: "web", UserID: "anonymous",
		CreatedAt: now, LastActiveAt: now,
	}
	created, isNew, err := store.GetOrCreateSession(ctx, first)
	require.NoError(t, err)
	require.True(t, isNew, "首次创建必须报告 created=true")
	require.Equal(t, "web-0123456789abcdef", created.ID)
	require.Equal(t, 0, created.MessageCount)
	require.True(t, created.CreatedAt.Equal(now))
	require.Equal(t, "active", created.Status)

	later := now.Add(time.Hour)
	repeat := first
	repeat.CreatedAt = later
	repeat.LastActiveAt = later
	existing, isNew, err := store.GetOrCreateSession(ctx, repeat)
	require.NoError(t, err)
	require.False(t, isNew, "同元组重复创建不得新建")
	require.True(t, existing.CreatedAt.Equal(now), "created_at 首写后不得被改写")

	require.NoError(t, store.ArchiveSession(ctx, first.ID, later))
	archived, isNew, err := store.GetOrCreateSession(ctx, first)
	require.NoError(t, err)
	require.False(t, isNew)
	require.Equal(t, "archived", archived.Status, "已归档行不得被创建请求复活")
}

// TestSessionStore_ArchiveIsIdempotent 判卷归档语义四例（R9）：置 archived 并首次
// 写 archived_at；重复归档时间不变；消息保留；未知 id → fs.ErrNotExist。
// Archive writes status plus the first archived_at, keeps messages, and reports a
// missing id instead of silently succeeding.
func TestSessionStore_ArchiveIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewSessionStore(openTempDB(t))

	// 存储按 UnixMilli 落 INTEGER（session_store.go 既有约定）；截断到毫秒，
	// 避免纳秒位在写读往返中丢失导致相等断言偶发红。
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, store.UpsertSession(ctx, core.SessionRecord{
		ID: "s1", ProfileName: "ops-agent", Channel: "web", UserID: "u",
		CreatedAt: now, LastActiveAt: now,
	}))
	require.NoError(t, store.AppendMessage(ctx, "s1", "run-1",
		core.Message{Role: "user", Content: "保留我"}))

	first := now.Add(time.Minute)
	require.NoError(t, store.ArchiveSession(ctx, "s1", first))
	got, err := store.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.Equal(t, "archived", got.Status)
	require.NotNil(t, got.ArchivedAt)
	require.True(t, got.ArchivedAt.Equal(first))

	second := now.Add(2 * time.Minute)
	require.NoError(t, store.ArchiveSession(ctx, "s1", second), "重复归档必须幂等")
	got, err = store.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.True(t, got.ArchivedAt.Equal(first), "archived_at 首次写入后不得变化")

	msgs, err := store.MessagesTail(ctx, "s1", 100)
	require.NoError(t, err)
	require.Len(t, msgs, 1, "归档不得删除消息")

	require.ErrorIs(t, store.ArchiveSession(ctx, "missing", second), fs.ErrNotExist,
		"未知 id 必须报不存在，而不是静默成功")
}

// TestSessionStore_MessagesTailKeepsLatestInOrder 判卷历史窗口（R8）：120 条只回
// 最近 100 条、按 seq 升序、created_at 回填。
// The tail keeps the latest 100 in ascending seq order with created_at filled.
func TestSessionStore_MessagesTailKeepsLatestInOrder(t *testing.T) {
	ctx := context.Background()
	store := NewSessionStore(openTempDB(t))

	now := time.Now().UTC()
	require.NoError(t, store.UpsertSession(ctx, core.SessionRecord{
		ID: "s1", ProfileName: "ops-agent", Channel: "web", UserID: "u",
		CreatedAt: now, LastActiveAt: now,
	}))
	for i := 0; i < 120; i++ {
		require.NoError(t, store.AppendMessage(ctx, "s1", "run-1",
			core.Message{Role: "user", Content: fmt.Sprintf("m%03d", i)}))
	}

	tail, err := store.MessagesTail(ctx, "s1", 100)
	require.NoError(t, err)
	require.Len(t, tail, 100)
	require.Equal(t, "m020", tail[0].Content, "窗口从第 21 条开始（最近的 100 条）")
	require.Equal(t, "m119", tail[99].Content)
	require.False(t, tail[0].CreatedAt.IsZero(), "created_at 必须回填（契约 MessageView）")
}
