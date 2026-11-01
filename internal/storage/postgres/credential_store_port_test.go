//go:build integration

package postgres

// 本文件是托管凭据仓储的判卷（颗粒度文档 R4.8 / T115，check `012-storage-admin`）：
// 版本只追加、撤销清空密文并升纪元、撤销后任何路径都不得复活。
// 由 SQLite 谱系 `internal/storage/credential_store_test.go` 逐条移植：断言、中文
// 验收点与逆改点注释原样保留，只把夹具换成隔离 schema（`portTestDB`）与读写合一的
// `db.pool`；辅助函数加 `cred` 前缀，避免与同包其他移植文件的同名构造冲突。
// These tests pin the credential repository: append-only versions, revoke blanking the
// ciphertext while bumping the epoch, and the terminal nature of revocation. Ported from
// the SQLite lineage verbatim; only the fixture and the handle differ.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// credResourceUIDFor 按 `(kind, name)` 派生一个稳定的测试 UID：同名同种永远是同一实例，
// 不同名称 / 不同种类是不同实例——测试要判的正是"身份跟着实例走、不跟着名称走"。
// 与 SQLite 侧 `internal/storage/resource_store_test.go` 的同名构造逐字一致；加 `cred`
// 前缀是移植规则第 5 条的防重名约定。
// credResourceUIDFor derives a stable test uid from kind and name, identical to the
// SQLite lineage; the cred prefix keeps package-level helper names unique.
func credResourceUIDFor(kind core.ResourceKind, name string) core.ResourceUID {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + name))
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	payload := make([]byte, 26)
	for i := range payload {
		payload[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return core.ResourceUID(core.ResourceUIDPrefix + string(payload))
}

// credModelVersion 造一条模型连接版本；版本号 0 表示让仓储自动取号。
// credModelVersion builds a model_connection version; zero means auto-numbering.
func credModelVersion(resourceID, contentHash, operationID string, versionNo int) core.ResourceVersion {
	return core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    credResourceUIDFor(core.ResourceModelConnection, resourceID),
		ResourceID:     resourceID,
		VersionNo:      versionNo,
		ContentHash:    contentHash,
		DefinitionYAML: "name: " + resourceID + "\nmodel: gpt-test\n",
		CreatedAt:      opNow.UnixMilli(),
		OperationID:    operationID,
	}
}

// credSampleChannel 造一条形状完整的渠道记录；`uidSuffix` 决定身份（Crockford Base32 的
// 合法字符，避免 I / L / O / U）。调用方按用例改字段。
// credSampleChannel builds a complete channel record; the uid suffix must be a valid
// Crockford Base32 character.
func credSampleChannel(name string, uidSuffix byte) core.NotifyChannel {
	return core.NotifyChannel{
		ResourceUID:    testResourceUID(uidSuffix),
		Name:           name,
		Type:           "webhook",
		URL:            "${BASALT_TEST_HOOK}",
		Description:    "值班告警",
		CredentialMode: core.CredentialModeEnv,
		Scope:          core.ScopeShared,
		OperationID:    "op-notify-write-000001",
	}
}

// credentialFixture 造一对形状完整的身份 + 第 1 版（密文与 nonce 按 schema 约束）。
// credentialFixture builds a complete identity/version pair.
func credentialFixture(id string, opID string) (core.CredentialRecord, core.CredentialVersionRecord) {
	now := opNow.UnixMilli()
	identity := core.CredentialRecord{
		ID:             id,
		OwnerKind:      core.BindingModelConnection,
		OwnerUID:       testResourceUID('A'),
		OwnerName:      "openai",
		State:          core.CredentialIdentityActive,
		CurrentVersion: 1,
		KeyID:          "k1",
		CreatedAt:      now,
		UpdatedAt:      now,
		OperationID:    opID,
	}
	version := core.CredentialVersionRecord{
		CredentialID:  id,
		VersionNo:     1,
		State:         core.CredentialVersionActive,
		KeyID:         "k1",
		TargetBinding: "target-hash-1",
		Last4:         "abcd",
		Nonce:         bytes.Repeat([]byte{0x01}, 12),
		Ciphertext:    []byte("cipher-v1"),
		CreatedAt:     now,
		UpdatedAt:     now,
		OperationID:   opID,
	}
	return identity, version
}

// appendVersionInput 造一个待追加版本的公共字段（版本号与状态由仓储填）。
// appendVersionInput builds the caller-supplied fields of a new version.
func appendVersionInput(target string, nonceByte byte) core.CredentialVersionRecord {
	return core.CredentialVersionRecord{
		KeyID:         "k1",
		TargetBinding: target,
		Last4:         "efgh",
		Nonce:         bytes.Repeat([]byte{nonceByte}, 12),
		Ciphertext:    []byte("cipher-" + target),
	}
}

// TestCredentialStore_VersionAppendOnly：每次换 Key 都是新版本——旧版本转 superseded 但
// 内容原样保留、版本号单调递增、身份的 current_version 随之前进；审计快照首次记录后不重写。
// 中文验收点：版本只追加，撤销以外的路径不改旧版本内容。
func TestCredentialStore_VersionAppendOnly(t *testing.T) {
	db := portTestDB(t)
	store := NewCredentialStore(db)
	ctx := context.Background()
	for _, op := range []string{"op-cred-1", "op-cred-2", "op-cred-3"} {
		seedOperation(t, db, op)
	}

	id := testCredentialID('A')
	identity, version := credentialFixture(id, "op-cred-1")
	require.NoError(t, store.Create(ctx, identity, version))

	require.NoError(t, store.AppendVersion(ctx, id, "k1", "op-cred-2",
		appendVersionInput("target-hash-2", 0x02), opNow.Add(time.Second)))

	got, err := store.Credential(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 2, got.CurrentVersion)
	require.Equal(t, core.CredentialIdentityActive, got.State)

	first, err := store.Version(ctx, id, 1)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionSuperseded, first.State)
	require.Equal(t, []byte("cipher-v1"), first.Ciphertext, "旧版本密文原样保留，供在途使用")
	require.Equal(t, 12, len(first.Nonce))

	second, err := store.Version(ctx, id, 2)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionActive, second.State)
	require.Equal(t, "target-hash-2", second.TargetBinding)

	versions, err := store.Versions(ctx, id)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	require.Equal(t, 1, versions[0].VersionNo)
	require.Equal(t, 2, versions[1].VersionNo)

	// 审计快照：首次记录的是当时活动版本，重复记录不改写首次结论。
	published, _, err := NewResourceStore(db).EnsureVersion(ctx, credModelVersion("openai", "sha256:cred", "op-cred-3", 0))
	require.NoError(t, err)
	require.NoError(t, store.RecordVersionCredential(ctx, published.ID, id, 1, opNow))
	require.NoError(t, store.RecordVersionCredential(ctx, published.ID, id, 2, opNow.Add(time.Minute)))
	resolved, ok, err := store.VersionCredential(ctx, published.ID, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, resolved, "快照只记首次发布时的活动版本")

	// 属主反查：一个资源实例至多一条凭据身份。
	byOwner, err := store.CredentialByOwner(ctx, core.BindingModelConnection, testResourceUID('A'))
	require.NoError(t, err)
	require.Equal(t, id, byOwner.ID)

	// 同属主再建 → 冲突（不覆盖既有身份）。
	other := identity
	other.ID = testCredentialID('B')
	_, dupVersion := credentialFixture(other.ID, "op-cred-3")
	require.ErrorIs(t, store.Create(ctx, other, dupVersion), core.ErrConflict)
}

// TestCredentialStore_RevokeClearsCiphertext：撤销把身份与**全部**版本置 revoked、
// 密文清空、纪元 +1；版本行保留（历史仍能回答哪一版被撤销）；重复撤销幂等。
// 中文验收点：撤销后库中无可解密材料，纪元单调前进。
func TestCredentialStore_RevokeClearsCiphertext(t *testing.T) {
	db := portTestDB(t)
	store := NewCredentialStore(db)
	ctx := context.Background()
	for _, op := range []string{"op-rev-1", "op-rev-2", "op-rev-3"} {
		seedOperation(t, db, op)
	}

	id := testCredentialID('A')
	identity, version := credentialFixture(id, "op-rev-1")
	require.NoError(t, store.Create(ctx, identity, version))
	require.NoError(t, store.AppendVersion(ctx, id, "k1", "op-rev-2",
		appendVersionInput("target-hash-2", 0x02), opNow.Add(time.Second)))

	require.NoError(t, store.Revoke(ctx, id, "op-rev-3", opNow.Add(time.Minute)))

	got, err := store.Credential(ctx, id)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRevoked, got.State)
	require.Equal(t, 1, got.RevocationEpoch)

	versions, err := store.Versions(ctx, id)
	require.NoError(t, err)
	require.Len(t, versions, 2, "版本行保留为历史证据")
	for _, rec := range versions {
		require.Equal(t, core.CredentialVersionRevoked, rec.State)
		require.Nil(t, rec.Ciphertext, "撤销后密文必须清空")
	}

	// 重复撤销幂等：纪元不再前进，不报错。
	require.NoError(t, store.Revoke(ctx, id, "op-rev-3", opNow.Add(time.Hour)))
	got, err = store.Credential(ctx, id)
	require.NoError(t, err)
	require.Equal(t, 1, got.RevocationEpoch)
}

// TestCredentialStore_NeverRevives 固定 2026-10-23 判据裁决后的矩阵：
//   - **版本**撤销是终态：旧的 revoked 版本永不回到 active，密文保持 NULL；
//   - **身份**可经重新录入迁回 active：追加的是**新编号**版本，撤销纪元不重置、不回退；
//   - 重新录入失败（形状非法 / 写库被拒）整体回滚，身份保持撤销、不留版本行；
//   - 已撤销的凭据仍不得被"退役"改写；已退役的凭据仍可撤销。
//
// 中文验收点：被撤销的版本永远不能恢复；重新录入只追加新版本。
func TestCredentialStore_NeverRevives(t *testing.T) {
	db := portTestDB(t)
	store := NewCredentialStore(db)
	ctx := context.Background()
	for _, op := range []string{"op-live-1", "op-live-2", "op-live-3", "op-live-4", "op-live-5"} {
		seedOperation(t, db, op)
	}

	id := testCredentialID('A')
	identity, version := credentialFixture(id, "op-live-1")
	require.NoError(t, store.Create(ctx, identity, version))

	// 先退役再撤销：合法（资源删除后才发现泄露）。
	require.NoError(t, store.Retire(ctx, id, "op-live-2", opNow))
	got, err := store.Credential(ctx, id)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRetired, got.State)
	retiredVersion, err := store.Version(ctx, id, 1)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionRetired, retiredVersion.State)
	require.Equal(t, []byte("cipher-v1"), retiredVersion.Ciphertext, "退役保留密文供在途使用")

	require.NoError(t, store.Revoke(ctx, id, "op-live-3", opNow.Add(time.Second)))
	got, err = store.Credential(ctx, id)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRevoked, got.State)
	require.Equal(t, 1, got.RevocationEpoch)

	// 重新录入失败（形状非法）：直接拒绝，零写入。
	err = store.AppendVersion(ctx, id, "k2", "op-live-4",
		core.CredentialVersionRecord{KeyID: "k2", Nonce: bytes.Repeat([]byte{0x02}, 12), Ciphertext: []byte("cipher-v2")},
		opNow.Add(2*time.Second))
	require.Error(t, err, "缺 target_binding 的版本必须被拒")

	// 重新录入失败（写库被拒：操作号不存在 → 外键失败）：事务整体回滚。
	badOp := appendVersionInput("target-hash-2", 0x02)
	badOp.KeyID = "k2"
	err = store.AppendVersion(ctx, id, "k2", "op-missing", badOp, opNow.Add(2*time.Second))
	require.Error(t, err, "写库失败必须整体回滚")
	got, err = store.Credential(ctx, id)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRevoked, got.State, "失败的重新录入不得改变身份状态")
	require.Equal(t, 1, got.RevocationEpoch, "失败的重新录入不得动纪元")
	require.Equal(t, 1, got.CurrentVersion)
	versions, err := store.Versions(ctx, id)
	require.NoError(t, err)
	require.Len(t, versions, 1, "被拒的追加不留版本行")

	// 重新录入成功：身份迁回 active，追加第 2 版；"版本只可撤销、不可复活"的断言不变。
	reentry := appendVersionInput("target-hash-2", 0x02)
	reentry.KeyID = "k2"
	require.NoError(t, store.AppendVersion(ctx, id, "k2", "op-live-4", reentry, opNow.Add(3*time.Second)))
	got, err = store.Credential(ctx, id)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityActive, got.State, "重新录入把身份迁回 active")
	require.Equal(t, 2, got.CurrentVersion)
	require.Equal(t, "k2", got.KeyID)
	require.Equal(t, 1, got.RevocationEpoch, "重新录入不重置、不回退撤销纪元")

	oldVersion, err := store.Version(ctx, id, 1)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionRevoked, oldVersion.State, "被撤销的版本永远不能恢复")
	require.Nil(t, oldVersion.Ciphertext, "被撤销的版本永远不带回密文")
	newVersion, err := store.Version(ctx, id, 2)
	require.NoError(t, err)
	require.Equal(t, core.CredentialVersionActive, newVersion.State)
	require.NotEmpty(t, newVersion.Ciphertext)

	// 再次撤销：纪元继续前进（2），两个版本都是 revoked。
	require.NoError(t, store.Revoke(ctx, id, "op-live-5", opNow.Add(4*time.Second)))
	got, err = store.Credential(ctx, id)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityRevoked, got.State)
	require.Equal(t, 2, got.RevocationEpoch)
	versions, err = store.Versions(ctx, id)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	for _, rec := range versions {
		require.Equal(t, core.CredentialVersionRevoked, rec.State)
		require.Nil(t, rec.Ciphertext)
	}

	// 已撤销的凭据不得被"退役"改写（revoked → retired 非法）。
	require.Error(t, store.Retire(ctx, id, "op-live-5", opNow), "已撤销的凭据不得被退役改写")

	_, err = store.Credential(ctx, testCredentialID('Z'))
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	_, err = store.Version(ctx, id, 9)
	require.ErrorIs(t, err, core.ErrResourceNotFound)
}

// TestCredentialStore_ListAndOwnerKeyReads 是移植时的**补充用例**：旧文件只判了写入
// 路径，本实现的只读面（`ListActive` / `ListAll` / `ListAllVersions` /
// `CredentialByOwnerKey`）在 SQLite 谱系里由 app 层用例覆盖，移植后需要本包自证。
// 判据：枚举只含 active 身份、排查面含已撤销身份、核验面按 `(凭据 ID, 版本号)` 升序
// 带出全部版本行；按属主键未命中返回 not-found（"没有托管值"不得被读成回落 env）。
// 中文验收点：排查面与核验面读到的行与写入事实一致。
// TestCredentialStore_ListAndOwnerKeyReads supplements the ported cases with the read
// paths of this implementation; a miss on the owner key is a not-found, never env fallback.
func TestCredentialStore_ListAndOwnerKeyReads(t *testing.T) {
	db := portTestDB(t)
	store := NewCredentialStore(db)
	ctx := context.Background()
	for _, op := range []string{"op-list-1", "op-list-2", "op-list-3"} {
		seedOperation(t, db, op)
	}

	first := testCredentialID('A')
	identityA, versionA := credentialFixture(first, "op-list-1")
	require.NoError(t, store.Create(ctx, identityA, versionA))

	second := testCredentialID('B')
	identityB, versionB := credentialFixture(second, "op-list-2")
	identityB.OwnerUID = testResourceUID('B')
	require.NoError(t, store.Create(ctx, identityB, versionB))

	require.NoError(t, store.AppendVersion(ctx, first, "k1", "op-list-3",
		appendVersionInput("target-hash-2", 0x02), opNow.Add(time.Second)))
	require.NoError(t, store.Revoke(ctx, second, "op-list-3", opNow.Add(time.Second)))

	active, err := store.ListActive(ctx)
	require.NoError(t, err)
	require.Len(t, active, 1, "已撤销身份不在枚举里")
	require.Equal(t, first, active[0].ID)
	require.Equal(t, core.CredentialIdentityActive, active[0].State)

	all, err := store.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2, "排查面含已撤销身份")
	require.Equal(t, first, all[0].ID)
	require.Equal(t, second, all[1].ID)
	require.Equal(t, core.CredentialIdentityActive, all[0].State)
	require.Equal(t, core.CredentialIdentityRevoked, all[1].State)

	versions, err := store.ListAllVersions(ctx)
	require.NoError(t, err)
	require.Len(t, versions, 3, "全部版本行按（凭据 ID, 版本号）升序")
	require.Equal(t, first, versions[0].CredentialID)
	require.Equal(t, core.CredentialVersionSuperseded, versions[0].State)
	require.NotEmpty(t, versions[0].Ciphertext, "在途版本保留密文")
	require.Equal(t, 2, versions[1].VersionNo)
	require.Equal(t, core.CredentialVersionActive, versions[1].State)
	require.Equal(t, second, versions[2].CredentialID)
	require.Equal(t, core.CredentialVersionRevoked, versions[2].State)
	require.Nil(t, versions[2].Ciphertext, "撤销版本在核验面上不得带密文")

	byKey, err := store.CredentialByOwnerKey(ctx, core.BindingModelConnection, testResourceUID('A'), "")
	require.NoError(t, err)
	require.Equal(t, first, byKey.ID)
	_, err = store.CredentialByOwnerKey(ctx, core.BindingModelConnection, testResourceUID('Z'), "")
	require.ErrorIs(t, err, core.ErrResourceNotFound, "未命中不得回落 env")
}

// TestCredentialStore_ResourceDeleteRetiresCredential —— R4.8 / M-1：删除模型连接或
// 通知渠道时，凭据身份与活动版本在**同一事务**里转 `retired`（密文保留给在途 Run），
// 不留"资源已删、凭据仍 active"的窗口。边界：
//   - env 模式（没有凭据行）删除照常；
//   - 已撤销的身份保持 `revoked`，不被删除改写成 `retired`（撤销是终态）。
//
// TestCredentialStore_ResourceDeleteRetiresCredential pins that a resource delete
// retires its credential in the tombstone transaction, without reviving revoked rows.
func TestCredentialStore_ResourceDeleteRetiresCredential(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	opIDs := []string{
		"op-del-mc-1", "op-del-mc-2", "op-del-ch-1", "op-del-ch-2",
		"op-del-rev-1", "op-del-rev-2", "op-del-env-1", "op-del-env-2",
	}
	for _, op := range opIDs {
		seedOperation(t, db, op)
	}
	creds := NewCredentialStore(db)
	resources := NewResourceStore(db)
	channels := NewNotifyAdminStore(db)

	published := func(name, opID string) core.ResourceVersion {
		t.Helper()
		v, _, err := resources.EnsureVersion(ctx, credModelVersion(name, "sha256:"+name, opID, 0))
		require.NoError(t, err)
		require.NoError(t, resources.Publish(ctx, core.ResourceModelConnection, name,
			v.ID, true, opID, opNow))
		return v
	}
	seal := func(id, opID string, kind core.BindingKind, owner core.ResourceUID) {
		t.Helper()
		now := opNow.UnixMilli()
		require.NoError(t, creds.Create(ctx, core.CredentialRecord{
			ID:             id,
			OwnerKind:      kind,
			OwnerUID:       owner,
			OwnerName:      "fixture",
			State:          core.CredentialIdentityActive,
			CurrentVersion: 1,
			KeyID:          "k1",
			CreatedAt:      now,
			UpdatedAt:      now,
			OperationID:    opID,
		}, core.CredentialVersionRecord{
			CredentialID:  id,
			VersionNo:     1,
			State:         core.CredentialVersionActive,
			KeyID:         "k1",
			TargetBinding: "target-hash",
			Nonce:         bytes.Repeat([]byte{0x01}, 12),
			Ciphertext:    []byte("cipher-v1"),
			CreatedAt:     now,
			UpdatedAt:     now,
			OperationID:   opID,
		}))
	}
	assertRetired := func(id string) {
		t.Helper()
		identity, err := creds.Credential(ctx, id)
		require.NoError(t, err)
		require.Equal(t, core.CredentialIdentityRetired, identity.State)
		version, err := creds.Version(ctx, id, 1)
		require.NoError(t, err)
		require.Equal(t, core.CredentialVersionRetired, version.State)
		require.Equal(t, []byte("cipher-v1"), version.Ciphertext, "密文保留给在途 Run 用完")
	}

	t.Run("模型连接删除", func(t *testing.T) {
		v := published("openai-del", "op-del-mc-1")
		id := testCredentialID('C')
		seal(id, "op-del-mc-1", core.BindingModelConnection, v.ResourceUID)
		require.NoError(t, resources.MarkDeletedByUID(ctx, core.ResourceModelConnection,
			"openai-del", v.ResourceUID, "op-del-mc-2", opNow))
		assertRetired(id)
	})

	t.Run("通知渠道删除", func(t *testing.T) {
		channel := credSampleChannel("ops-del", 'D')
		channel.OperationID = "op-del-ch-1"
		require.NoError(t, channels.Create(ctx, channel))
		id := testCredentialID('D')
		seal(id, "op-del-ch-1", core.BindingNotifyChannel, channel.ResourceUID)
		require.NoError(t, channels.Delete(ctx, "ops-del", "op-del-ch-2"))
		assertRetired(id)
	})

	t.Run("已撤销保持撤销", func(t *testing.T) {
		channel := credSampleChannel("ops-rev", 'E')
		channel.OperationID = "op-del-rev-1"
		require.NoError(t, channels.Create(ctx, channel))
		id := testCredentialID('E')
		seal(id, "op-del-rev-1", core.BindingNotifyChannel, channel.ResourceUID)
		require.NoError(t, creds.Revoke(ctx, id, "op-del-rev-1", opNow))
		require.NoError(t, channels.Delete(ctx, "ops-rev", "op-del-rev-2"))

		identity, err := creds.Credential(ctx, id)
		require.NoError(t, err)
		require.Equal(t, core.CredentialIdentityRevoked, identity.State,
			"撤销是终态：删除不得把身份改写成 retired")
		version, err := creds.Version(ctx, id, 1)
		require.NoError(t, err)
		require.Equal(t, core.CredentialVersionRevoked, version.State)
		require.Empty(t, version.Ciphertext, "撤销清空密文，删除不复活")
	})

	t.Run("env 模式没有凭据行", func(t *testing.T) {
		v := published("env-del", "op-del-env-1")
		require.NoError(t, resources.MarkDeletedByUID(ctx, core.ResourceModelConnection,
			"env-del", v.ResourceUID, "op-del-env-2", opNow))
	})
}
