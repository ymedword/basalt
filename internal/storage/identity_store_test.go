package storage

// 本文件是 `resource_identities` 仓储的判卷（颗粒度文档 R4.5 / T113，check
// `012-storage-admin`）：身份一次写入、删除只翻状态留墓碑、版本号按实例（UID）计。
// 三条用例各守一条纪律——墓碑不删行、取号不跨身份、同名重建从 1 重新计。
// These tests pin the identity repository: tombstone-only deletion, per-uid version
// numbering, and same-name rebuilds restarting at one.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// newIdentity 造一条形状完整的身份行（默认 active、无删除时间）。
// newIdentity builds a complete identity row.
func newIdentity(uid core.ResourceUID, kind core.ResourceKind, name, operationID string) core.ResourceIdentity {
	return core.ResourceIdentity{
		UID:         uid,
		Kind:        kind,
		Name:        name,
		State:       core.IdentityActive,
		CreatedAt:   opNow.UnixMilli(),
		OperationID: operationID,
	}
}

// TestIdentityStore_Tombstone：删除是**状态翻转**而不是删行——墓碑保留名称快照与删除
// 时间；重复删除幂等（保留第一次时间）；不存在 → `core.ErrResourceNotFound`；
// 新身份不允许带删除态或复用已有 UID。
// 中文验收点：墓碑不删行、重复删除幂等、UID 不复用。
func TestIdentityStore_Tombstone(t *testing.T) {
	db := openTempDB(t)
	store := NewIdentityStore(db)
	ctx := context.Background()
	uid := testResourceUID('A')

	require.NoError(t, store.Create(ctx, newIdentity(uid, core.ResourceModelConnection, "openai", "op-id-1")))

	rec, err := store.IdentityByUID(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, uid, rec.UID)
	require.Equal(t, core.ResourceModelConnection, rec.Kind)
	require.Equal(t, "openai", rec.Name)
	require.Equal(t, core.IdentityActive, rec.State)
	require.Nil(t, rec.DeletedAt)

	first := opNow
	require.NoError(t, store.Tombstone(ctx, uid, "op-id-2", first))

	rec, err = store.IdentityByUID(ctx, uid)
	require.NoError(t, err, "墓碑仍可读——身份行不物理删除")
	require.Equal(t, core.IdentityDeleted, rec.State)
	require.NotNil(t, rec.DeletedAt)
	require.Equal(t, first.UnixMilli(), *rec.DeletedAt)
	require.Equal(t, "openai", rec.Name, "名称快照保留，历史仍能回答它是谁")

	// 重复删除：幂等，第一次的删除时间不被覆盖。
	require.NoError(t, store.Tombstone(ctx, uid, "op-id-3", first.Add(time.Hour)))
	rec, err = store.IdentityByUID(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, first.UnixMilli(), *rec.DeletedAt)

	// 不存在的 UID → 404 语义。
	err = store.Tombstone(ctx, testResourceUID('B'), "op-id-4", first)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	// UID 永不复用：再建同一 UID 被拒；删除态也不能作为"新身份"写入。
	require.Error(t, store.Create(ctx, newIdentity(uid, core.ResourceModelConnection, "openai", "op-id-5")))
	deletedAt := first.UnixMilli()
	revived := newIdentity(testResourceUID('C'), core.ResourceModelConnection, "other", "op-id-6")
	revived.State = core.IdentityDeleted
	revived.DeletedAt = &deletedAt
	require.Error(t, store.Create(ctx, revived))
}

// TestIdentityStore_VersionScopeByUID：版本号的作用域是**实例**而不是名称——
// 两个同名但 UID 不同的资源各自从 1 计数，取号互不影响。
// 中文验收点：同名不同实例的版本号互不干扰。
func TestIdentityStore_VersionScopeByUID(t *testing.T) {
	db := openTempDB(t)
	store := NewIdentityStore(db)
	versions := NewResourceStore(db)
	ctx := context.Background()

	seedOperation(t, db, "op-scope-1")
	seedOperation(t, db, "op-scope-2")

	uidA := testResourceUID('A')
	uidB := testResourceUID('B')
	require.NoError(t, store.Create(ctx, newIdentity(uidA, core.ResourceModelConnection, "openai", "op-scope-1")))
	require.NoError(t, store.Create(ctx, newIdentity(uidB, core.ResourceModelConnection, "openai", "op-scope-2")))

	next, err := store.AllocateVersionNo(ctx, core.ResourceModelConnection, uidA)
	require.NoError(t, err)
	require.Equal(t, 1, next, "还没版本时取 1")

	versionOf := func(uid core.ResourceUID, hash, operationID string) core.ResourceVersion {
		return core.ResourceVersion{
			Kind:           core.ResourceModelConnection,
			ResourceUID:    uid,
			ResourceID:     "openai",
			ContentHash:    hash,
			DefinitionYAML: "name: openai\n",
			CreatedAt:      opNow.UnixMilli(),
			OperationID:    operationID,
		}
	}
	first, _, err := versions.EnsureVersion(ctx, versionOf(uidA, "sha256:a", "op-scope-1"))
	require.NoError(t, err)
	require.Equal(t, 1, first.VersionNo)

	next, err = store.AllocateVersionNo(ctx, core.ResourceModelConnection, uidA)
	require.NoError(t, err)
	require.Equal(t, 2, next)

	next, err = store.AllocateVersionNo(ctx, core.ResourceModelConnection, uidB)
	require.NoError(t, err)
	require.Equal(t, 1, next, "同名不同 UID 是另一实例，从 1 开始")

	second, _, err := versions.EnsureVersion(ctx, versionOf(uidA, "sha256:b", "op-scope-1"))
	require.NoError(t, err)
	require.Equal(t, 2, second.VersionNo)

	// 种类不匹配的取号在写库前拒绝（版本表只认三值）。
	_, err = store.AllocateVersionNo(ctx, core.ResourceNotifyChannel, uidA)
	require.Error(t, err)

	// 身份种类按四值解析：身份表里的通知渠道合法，版本表里仍是非法种类。
	require.NoError(t, store.Create(ctx, newIdentity(testResourceUID('C'), core.ResourceNotifyChannel, "值班", "op-scope-2")))
}

// TestIdentityStore_SameNameRebuildStartsAtOne：同名删除后重建是**新身份**——
// 旧墓碑仍在（两个代际都查得到），新实例的版本号从 1 重新计。
// 中文验收点：同名重建版本号从 1 起，旧代际保留。
func TestIdentityStore_SameNameRebuildStartsAtOne(t *testing.T) {
	db := openTempDB(t)
	store := NewIdentityStore(db)
	versions := NewResourceStore(db)
	ctx := context.Background()

	seedOperation(t, db, "op-rebuild-1")
	seedOperation(t, db, "op-rebuild-2")

	oldUID := testResourceUID('A')
	newUID := testResourceUID('B')
	require.NoError(t, store.Create(ctx, newIdentity(oldUID, core.ResourceModelConnection, "openai", "op-rebuild-1")))
	_, _, err := versions.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    oldUID,
		ResourceID:     "openai",
		ContentHash:    "sha256:a",
		DefinitionYAML: "name: openai\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    "op-rebuild-1",
	})
	require.NoError(t, err)
	require.NoError(t, store.Tombstone(ctx, oldUID, "op-rebuild-1", opNow))

	require.NoError(t, store.Create(ctx, newIdentity(newUID, core.ResourceModelConnection, "openai", "op-rebuild-2")))

	next, err := store.AllocateVersionNo(ctx, core.ResourceModelConnection, newUID)
	require.NoError(t, err)
	require.Equal(t, 1, next, "重建是新实例，版本号重新从 1 起")

	oldNext, err := store.AllocateVersionNo(ctx, core.ResourceModelConnection, oldUID)
	require.NoError(t, err)
	require.Equal(t, 2, oldNext, "旧墓碑的版本账不因重建清零")

	generations, err := store.IdentitiesByName(ctx, core.ResourceModelConnection, "openai")
	require.NoError(t, err)
	require.Len(t, generations, 2, "同名两个代际都保留")
	require.Equal(t, oldUID, generations[0].UID)
	require.Equal(t, core.IdentityDeleted, generations[0].State)
	require.Equal(t, newUID, generations[1].UID)
	require.Equal(t, core.IdentityActive, generations[1].State)

	// 未知种类与空名称在查询前拒绝。
	_, err = store.IdentitiesByName(ctx, core.ResourceKind("unknown"), "openai")
	require.Error(t, err)
	_, err = store.IdentitiesByName(ctx, core.ResourceModelConnection, "")
	require.Error(t, err)
}
