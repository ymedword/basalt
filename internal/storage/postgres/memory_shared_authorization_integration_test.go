//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

func TestSharedMemory_AuthorizationChecksLiveKey(t *testing.T) {
	ctx := context.Background()
	db := keyStoreTestDB(t)
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	scope := core.AgentInvokeScopePrefix + "alice"
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-a", []string{scope}, nil, nil)
	principal := core.Principal{Kind: core.PrincipalCaller, CallerID: keyStoreCallerA, KeyID: "key-a", Actor: core.ActorCaller, Scopes: []string{scope}}
	store := NewMemoryStore(db, func() time.Time { return keyStoreNow })
	require.NoError(t, store.AuthorizeMemory(ctx, principal, "alice"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "共享信息"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "归档信息"))
	reader, err := memory.NewService(memory.Options{Backend: store})
	require.NoError(t, err)
	text, err := reader.LoadLayers(ctx, "alice", principal, true)
	require.NoError(t, err)
	require.Contains(t, text, "共享信息")
	semantic := memory.NewSemanticStore(memory.SemanticDeps{Store: store})
	query := memory.RecallQuery{Principal: principal, AuthorizationAgent: "alice", Scope: core.GlobalScope, Query: "归档", Mode: memory.RecallModeKeyword}
	hits, err := semantic.Recall(ctx, query)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	_, err = db.pool.ExecContext(ctx, `UPDATE caller_keys SET revoked_at = $1 WHERE key_id = $2`, keyStoreNow.UnixMilli(), "key-a")
	require.NoError(t, err)
	require.ErrorIs(t, store.AuthorizeMemory(ctx, principal, "alice"), core.ErrForbidden)
	_, err = reader.LoadLayers(ctx, "alice", principal, true)
	require.ErrorIs(t, err, core.ErrForbidden)
	_, err = semantic.Recall(ctx, query)
	require.ErrorIs(t, err, core.ErrForbidden)
	_, err = db.pool.ExecContext(ctx, `UPDATE caller_keys SET revoked_at = NULL, scopes = ARRAY[]::text[] WHERE key_id = $1`, "key-a")
	require.NoError(t, err)
	require.ErrorIs(t, store.AuthorizeMemory(ctx, principal, "alice"), core.ErrForbidden)
}

// TestSharedMemory_RevokedKeyBlocksEveryReadMode 守「配置不能绕过撤销」：三条读取路径
// （未声明的兼容回退 / 显式 false / 显式 true）在 Key 撤销后一律拒绝——撤销是**当前**
// 状态，不是准入时的历史结论；`include_global_memory` 只决定读哪几层，不决定能不能读。
// 逆改点：把 `LoadEffective` 回退分支的 `authorizeRead` 删掉 → 撤销后仍返回"共享信息"
// → 本用例红；把 `LoadLayers` 的复核挪进 `includeGlobal` 为真的分支 → false 分支红。
// 验收点：C-52 / FR-015；review ② D-1。
func TestSharedMemory_RevokedKeyBlocksEveryReadMode(t *testing.T) {
	ctx := context.Background()
	db := keyStoreTestDB(t)
	keyStoreSeedCaller(t, db, keyStoreCallerA, "service", "alpha", "active")
	scope := core.AgentInvokeScopePrefix + "alice"
	keyStoreSeedKeyRow(t, db, keyStoreCallerA, "key-a", []string{scope}, nil, nil)
	principal := core.Principal{Kind: core.PrincipalCaller, CallerID: keyStoreCallerA, KeyID: "key-a", Actor: core.ActorCaller, Scopes: []string{scope}}
	store := NewMemoryStore(db, func() time.Time { return keyStoreNow })
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "共享信息"))
	reader, err := memory.NewService(memory.Options{Backend: store})
	require.NoError(t, err)

	// 撤销前：Agent 层为空，未声明的兼容回退读到全局层。
	text, err := reader.LoadEffective(ctx, principal, "alice")
	require.NoError(t, err)
	require.Contains(t, text, "共享信息")

	_, err = db.pool.ExecContext(ctx, `UPDATE caller_keys SET revoked_at = $1 WHERE key_id = $2`, keyStoreNow.UnixMilli(), "key-a")
	require.NoError(t, err)

	_, err = reader.LoadEffective(ctx, principal, "alice")
	require.ErrorIs(t, err, core.ErrForbidden, "撤销的 Key 不得经兼容回退读全局层")
	for _, includeGlobal := range []bool{false, true} {
		_, err = reader.LoadLayers(ctx, "alice", principal, includeGlobal)
		require.ErrorIs(t, err, core.ErrForbidden,
			"include_global_memory=%v 不是撤销旁路", includeGlobal)
	}
}
