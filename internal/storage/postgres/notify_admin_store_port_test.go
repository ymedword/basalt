//go:build integration

package postgres

// 本文件是 `notify_channels` 管理写仓储（`NotifyAdminStore`）与发送只读仓储
// （`NotifyChannelRepo`）的判卷（颗粒度文档 §3.1.6 / 约束 38，check
// `012-storage-admin`）：CRUD 往返、空 URL 照存、同名创建不覆盖、更新留空保留原值、
// 行不存在映射 not-found；写面与发送读面（US-8 的 NotifyChannelRepo）读同一行。
// US-12 R4 追加身份与墓碑判据：主键是 `resource_uid`，名称只是当前标签，删除写墓碑
// 而不删行，同名重建是新实例。
// 发送只读面另判：读取按名称升序、空表返回空切片、ctx 取消原样上抛、零缓存、
// 墓碑行不参与发送。
//
// 由 SQLite 谱系 `internal/storage/notify_admin_store_test.go` 与
// `internal/storage/notify_store_test.go` 逐条移植：测试函数名、断言、中文验收点注释
// 与逆改点注释原样保留；只替换夹具（`openTempDB` → `portTestDB`）、读写句柄
// （`db.read` / `db.write` → `db.pool`）与方言（`?` → `$n`、时间列按 BIGINT Unix 毫秒）。
// 辅助函数加本文件前缀（移植规则第 5 条），避免与同包其他移植文件的同名构造冲突。
// Ported from the SQLite lineage verbatim; only the fixture, the handle and the dialect
// differ. Helper names carry this file's prefix so sibling port files keep compiling.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// notifyAdminSampleChannel 造一条形状完整的渠道记录；`uidSuffix` 决定身份（Crockford
// Base32 的合法字符，避免 I / L / O / U）。调用方按用例改字段。
// notifyAdminSampleChannel builds a complete channel record; the uid suffix must be a
// valid Crockford Base32 character.
func notifyAdminSampleChannel(name string, uidSuffix byte) core.NotifyChannel {
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

// notifyAdminRawChannel 是绕过领域映射读回的一行（保留可空列），判 "NULL vs 空串" 用。
// notifyAdminRawChannel reads one row without the domain mapping.
type notifyAdminRawChannel struct {
	ResourceUID string
	Name        string
	Type        string
	URL         string
	URLHost     string
	Mode        string
	Scope       string
	Owner       *string
	Credential  *string
	Revision    int
	Description string
	CreatedAt   int64
	UpdatedAt   int64
	DeletedAt   *int64
	OperationID string
}

func notifyAdminReadRawChannel(t *testing.T, db *DB, uid core.ResourceUID) notifyAdminRawChannel {
	t.Helper()
	var row notifyAdminRawChannel
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT resource_uid, name, type, url, url_host, credential_mode, scope,
		        owner_agent_uid, credential_id, revision, description,
		        created_at, updated_at, deleted_at, operation_id
		   FROM notify_channels WHERE resource_uid = $1`, string(uid)).
		Scan(&row.ResourceUID, &row.Name, &row.Type, &row.URL, &row.URLHost, &row.Mode,
			&row.Scope, &row.Owner, &row.Credential, &row.Revision, &row.Description,
			&row.CreatedAt, &row.UpdatedAt, &row.DeletedAt, &row.OperationID))
	return row
}

// notifyAdminCountChannels 数表里的全部行（含墓碑），判"删除是墓碑不是物理删除"。
// notifyAdminCountChannels counts every row, tombstones included.
func notifyAdminCountChannels(t *testing.T, db *DB) int {
	t.Helper()
	var count int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT count(*) FROM notify_channels`).Scan(&count))
	return count
}

// notifyAdminIdentityState 读身份行的状态（创建写 active、删除写 deleted）。
// notifyAdminIdentityState reads the identity row state.
func notifyAdminIdentityState(t *testing.T, db *DB, uid core.ResourceUID) string {
	t.Helper()
	var state string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT state FROM resource_identities WHERE resource_uid = $1`, string(uid)).Scan(&state))
	return state
}

// 中文验收点：创建 → 读回（身份 / 默认范围 / 凭据模式 / 版本号逐列保真）→ 身份行
// active → 发送读面同一条行 → 更新 → 删除；`created_at` 由仓储写入（非零毫秒）。
// Acceptance point: create, read back, the identity row, the send path seeing the same
// row, update and delete.
func TestNotifyAdminStore_CRUD(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	ch := notifyAdminSampleChannel("ops-alert", 'A')
	require.NoError(t, store.Create(ctx, ch))

	got, err := store.Get(ctx, ch.Name)
	require.NoError(t, err)
	require.Equal(t, ch.ResourceUID, got.ResourceUID, "读回必须是同一个资源实例")
	require.Equal(t, ch.Type, got.Type)
	require.Equal(t, ch.URL, got.URL)
	require.Equal(t, ch.Description, got.Description)
	require.Equal(t, core.CredentialModeEnv, got.CredentialMode)
	require.Equal(t, core.ScopeShared, got.Scope)
	require.Equal(t, 1, got.Revision)
	require.Nil(t, got.DeletedAt)
	require.Equal(t, ch.OperationID, got.OperationID)
	require.Positive(t, got.CreatedAt, "created_at 必须由仓储写入")
	require.Equal(t, got.CreatedAt, got.UpdatedAt, "创建行的 updated_at 与 created_at 同一时刻")
	require.Equal(t, "active", notifyAdminIdentityState(t, db, ch.ResourceUID))

	// 发送路径的只读仓储读同一张表：写后下一发即可见。
	sent, err := NewNotifyChannelRepo(db).ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Len(t, sent, 1)
	require.Equal(t, ch.ResourceUID, sent[0].ResourceUID, "发送读面必须看到写面刚落的同一行")

	updated := core.NotifyChannel{
		Name: ch.Name, Type: "feishu", URL: "${BASALT_TEST_HOOK}", Description: "改走飞书",
		OperationID: "op-notify-write-000002",
	}
	require.NoError(t, store.Update(ctx, updated, false))
	got, err = store.Get(ctx, ch.Name)
	require.NoError(t, err)
	require.Equal(t, "feishu", got.Type)
	require.Equal(t, "改走飞书", got.Description)
	require.Equal(t, 2, got.Revision, "地址变更必须使 revision +1")
	require.Equal(t, "op-notify-write-000002", got.OperationID, "写入行记录最后一次管理操作")

	require.NoError(t, store.Delete(ctx, ch.Name, "op-notify-write-000003"))
	_, err = store.Get(ctx, ch.Name)
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	sent, err = NewNotifyChannelRepo(db).ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, sent)

	// 删除是墓碑：行仍在，身份行同步置 deleted，操作号如实记录。
	raw := notifyAdminReadRawChannel(t, db, ch.ResourceUID)
	require.NotNil(t, raw.DeletedAt)
	require.Equal(t, "op-notify-write-000003", raw.OperationID)
	require.Equal(t, "deleted", notifyAdminIdentityState(t, db, ch.ResourceUID))
}

// 中文验收点：`url` 省略 / 空串照存（列 NOT NULL，空串满足）；读回是空串而非
// NULL——"暂不可发送"是合法状态（约束 38）。
// Acceptance point: an empty URL is stored as the empty string, never NULL.
func TestNotifyAdminStore_CreateEmptyURL(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	ch := notifyAdminSampleChannel("no-url", 'B')
	ch.URL = ""
	require.NoError(t, store.Create(ctx, ch))

	got, err := store.Get(ctx, ch.Name)
	require.NoError(t, err)
	require.Empty(t, got.URL)

	raw := notifyAdminReadRawChannel(t, db, ch.ResourceUID)
	require.Equal(t, "", raw.URL, "空 URL 必须是空串（NULL 会让读面走另一条路径）")
	require.Empty(t, raw.URLHost)
}

// 中文验收点：同名创建 → `core.ErrConflict`，既有行原样保留、不被覆盖；
// 表里仍只有一行（活动名称唯一）。
// Acceptance point: a duplicate name is a conflict and never overwrites the row.
func TestNotifyAdminStore_DuplicateNameIsConflict(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	first := notifyAdminSampleChannel("dup-channel", 'C')
	require.NoError(t, store.Create(ctx, first))

	second := notifyAdminSampleChannel("dup-channel", 'D')
	second.Type = "feishu"
	second.URL = "${OTHER_HOOK}"
	second.Description = "覆盖尝试"
	err := store.Create(ctx, second)
	require.ErrorIs(t, err, core.ErrConflict)
	require.ErrorContains(t, err, "dup-channel")

	got, err := store.Get(ctx, first.Name)
	require.NoError(t, err)
	require.Equal(t, first.ResourceUID, got.ResourceUID, "冲突创建不得改写既有行")
	require.Equal(t, "${BASALT_TEST_HOOK}", got.URL)
	require.Equal(t, 1, notifyAdminCountChannels(t, db))
}

// 中文验收点：更新留空 = 保留原值——`keepURL=true` 时地址相关列一动不动（含
// `url_host` 与凭据列），类型与说明照改，`revision` 不因非地址字段递增。
// Acceptance point: keepURL leaves every address column untouched while type and
// description change, and the revision stays put.
func TestNotifyAdminStore_UpdateKeepsURLWhenRequested(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	original := notifyAdminSampleChannel("keep-url", 'E')
	require.NoError(t, store.Create(ctx, original))
	before := notifyAdminReadRawChannel(t, db, original.ResourceUID)

	update := core.NotifyChannel{
		Name: original.Name, Type: "feishu", URL: "", Description: "只改类型与说明",
		OperationID: "op-notify-write-000004",
	}
	require.NoError(t, store.Update(ctx, update, true))

	got, err := store.Get(ctx, update.Name)
	require.NoError(t, err)
	require.Equal(t, "${BASALT_TEST_HOOK}", got.URL, "留空更新必须保留原 URL")
	require.Equal(t, "feishu", got.Type)
	require.Equal(t, "只改类型与说明", got.Description)

	after := notifyAdminReadRawChannel(t, db, original.ResourceUID)
	require.Equal(t, before.URLHost, after.URLHost, "地址列必须一动不动")
	require.Equal(t, before.Mode, after.Mode)
	require.Equal(t, before.Credential, after.Credential)
	require.Equal(t, before.Revision, after.Revision, "只改类型与说明不改 revision")
	require.Equal(t, "op-notify-write-000004", after.OperationID)
}

// 中文验收点：显式给新占位符（`keepURL=false`）→ URL 列被覆盖为候选值，
// `revision` +1（地址属于参与 ETag 的内容）。
// Acceptance point: keepURL=false replaces the url column and bumps the revision.
func TestNotifyAdminStore_UpdateReplacesURL(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	original := notifyAdminSampleChannel("replace-url", 'F')
	require.NoError(t, store.Create(ctx, original))

	update := core.NotifyChannel{
		Name: original.Name, Type: "webhook", URL: "${REPLACED_HOOK}", Description: "换地址",
		URLHost: "https://hooks.example.test", OperationID: "op-notify-write-000005",
	}
	require.NoError(t, store.Update(ctx, update, false))

	got, err := store.Get(ctx, update.Name)
	require.NoError(t, err)
	require.Equal(t, "${REPLACED_HOOK}", got.URL)
	require.Equal(t, "https://hooks.example.test", got.URLHost)
	require.Equal(t, 2, got.Revision)
}

// 中文验收点：删除不存在的渠道 → `core.ErrResourceNotFound`（重试的 404 语义）；
// 删除成功后从读面消失。
// Acceptance point: deleting a missing channel is not-found; deleting an existing
// one removes it from the send path.
func TestNotifyAdminStore_DeleteMissingIsNotFound(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	err := store.Delete(ctx, "never-existed", "op-notify-write-000006")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.ErrorContains(t, err, "never-existed")

	ch := notifyAdminSampleChannel("delete-me", 'G')
	require.NoError(t, store.Create(ctx, ch))
	require.NoError(t, store.Delete(ctx, ch.Name, "op-notify-write-000007"))
	_, err = store.Get(ctx, ch.Name)
	require.ErrorIs(t, err, core.ErrResourceNotFound)
}

// 中文验收点：同一渠道重复删除是幂等的 not-found（活动行已不在），墓碑原样保留、
// 不产生第二行——重试不受"已经删过了"的影响。
// Acceptance point: a repeated delete is an idempotent not-found and leaves the single
// tombstone untouched.
func TestNotifyAdminStore_DeleteIsIdempotentTombstone(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	ch := notifyAdminSampleChannel("twice", 'H')
	require.NoError(t, store.Create(ctx, ch))
	require.NoError(t, store.Delete(ctx, ch.Name, "op-notify-write-000008"))
	first := notifyAdminReadRawChannel(t, db, ch.ResourceUID)

	err := store.Delete(ctx, ch.Name, "op-notify-write-000009")
	require.ErrorIs(t, err, core.ErrResourceNotFound, "活动行已删除，重试按 404 语义")

	second := notifyAdminReadRawChannel(t, db, ch.ResourceUID)
	require.Equal(t, first.DeletedAt, second.DeletedAt, "墓碑时间不得被重试改写")
	require.Equal(t, "op-notify-write-000008", second.OperationID)
	require.Equal(t, 1, notifyAdminCountChannels(t, db))
}

// 中文验收点（US-12 R4 核心）：删除后按名称更新 → not-found 且墓碑零变化。
// 主键换成 UID 之后，`WHERE name = $1` 会命中同名墓碑行；写入必须解析活动行。
// Acceptance point: updating a deleted name must never rewrite its tombstone — the
// write path resolves the active row rather than matching the name directly.
func TestNotifyAdminStore_UpdateNeverHitsTombstone(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	ch := notifyAdminSampleChannel("gone", 'J')
	require.NoError(t, store.Create(ctx, ch))
	require.NoError(t, store.Delete(ctx, ch.Name, "op-notify-write-000010"))
	before := notifyAdminReadRawChannel(t, db, ch.ResourceUID)

	err := store.Update(ctx, core.NotifyChannel{
		Name: ch.Name, Type: "feishu", Description: "复活尝试",
		OperationID: "op-notify-write-000011",
	}, false)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	after := notifyAdminReadRawChannel(t, db, ch.ResourceUID)
	require.Equal(t, before, after, "墓碑行必须逐列不变")
}

// 中文验收点（US-12 R4）：同名重建是新实例——删除后同名创建成功、UID 不同、
// 旧墓碑保留、读面只看到新行。
// Acceptance point: a same-name rebuild is a new instance; the old tombstone stays.
func TestNotifyAdminStore_SameNameRebuildIsNewInstance(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	first := notifyAdminSampleChannel("rebuilt", 'K')
	require.NoError(t, store.Create(ctx, first))
	require.NoError(t, store.Delete(ctx, first.Name, "op-notify-write-000012"))

	second := notifyAdminSampleChannel("rebuilt", 'M')
	second.Type = "feishu"
	require.NoError(t, store.Create(ctx, second), "同名重建必须允许（旧行已是墓碑）")

	got, err := store.Get(ctx, second.Name)
	require.NoError(t, err)
	require.Equal(t, second.ResourceUID, got.ResourceUID, "名称必须指向新实例")
	require.Equal(t, 1, got.Revision, "新实例的 revision 从 1 重新计")
	require.Equal(t, "deleted", notifyAdminIdentityState(t, db, first.ResourceUID))
	require.Equal(t, "active", notifyAdminIdentityState(t, db, second.ResourceUID))
	require.Equal(t, 2, notifyAdminCountChannels(t, db), "旧墓碑必须保留供历史查询")
}

// 中文验收点（US-12 R4）：更新带 `ResourceUID` 时它是条件——与活动行不一致即拒绝
// 且零写入（两人同时改、其中一人手里的渠道已被同名重建时，后写不得落到新实例上）。
// Acceptance point: a caller-supplied uid is a precondition; a mismatch writes nothing.
func TestNotifyAdminStore_UpdateRejectsUIDMismatch(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	ch := notifyAdminSampleChannel("guarded", 'N')
	require.NoError(t, store.Create(ctx, ch))
	before := notifyAdminReadRawChannel(t, db, ch.ResourceUID)

	stale := notifyAdminSampleChannel("guarded", 'P')
	stale.Type = "feishu"
	stale.Description = "旧标签下的写入"
	err := store.Update(ctx, stale, false)
	require.ErrorIs(t, err, core.ErrResourceUIDMismatch)

	after := notifyAdminReadRawChannel(t, db, ch.ResourceUID)
	require.Equal(t, before, after, "UID 不匹配必须零写入")
}

// 中文验收点：名称 / 类型 / 操作号 / UID 缺失的写请求在任何 SQL 之前被拒（零行落库）
// ——管理写先留痕（`operation_id`）是存储侧的硬前提；更新不存在的渠道映射 not-found；
// 空名称的读 / 删同样拒绝。
// Acceptance point: a missing operation id or uid is refused before any SQL; an update
// of a missing channel is not-found.
func TestNotifyAdminStore_RejectsInvalidWrites(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	require.ErrorContains(t, store.Create(ctx,
		core.NotifyChannel{Name: "", Type: "webhook", OperationID: "op-x"}), "名称")
	require.ErrorContains(t, store.Create(ctx,
		core.NotifyChannel{Name: "x", Type: "", OperationID: "op-x"}), "类型")

	noOperation := notifyAdminSampleChannel("no-op", 'Q')
	noOperation.OperationID = ""
	require.ErrorContains(t, store.Create(ctx, noOperation), "operation_id")

	noUID := notifyAdminSampleChannel("no-uid", 'R')
	noUID.ResourceUID = ""
	require.ErrorContains(t, store.Create(ctx, noUID), "资源 UID")

	require.Zero(t, notifyAdminCountChannels(t, db), "形状不合格的写请求必须零落库")

	require.ErrorContains(t, store.Update(ctx,
		core.NotifyChannel{Type: "", OperationID: "op-x"}, true), "名称")
	require.ErrorContains(t, store.Update(ctx,
		core.NotifyChannel{Name: "x", Type: "", OperationID: "op-x"}, true), "类型")
	require.ErrorContains(t, store.Update(ctx,
		core.NotifyChannel{Name: "x", Type: "webhook"}, true), "operation_id")
	err := store.Update(ctx, notifyAdminSampleChannel("missing", 'S'), true)
	require.ErrorIs(t, err, core.ErrResourceNotFound)

	_, err = store.Get(ctx, "")
	require.ErrorContains(t, err, "名称")
	require.ErrorContains(t, store.Delete(ctx, "", "op-x"), "名称")
	require.ErrorContains(t, store.Delete(ctx, "x", ""), "operation_id")
}

// 中文验收点（US-12 R4 范围与凭据列）：范围与属主必须自洽（shared 无属主、exclusive
// 必有合法属主），env 模式不得带凭据身份、managed 模式不得带明文地址——形状不合格
// 的写请求零落库。
// Acceptance point: scope/owner and mode/credential shapes must be self-consistent.
func TestNotifyAdminStore_RejectsIncoherentScopeAndCredential(t *testing.T) {
	db := portTestDB(t)
	store := NewNotifyAdminStore(db)
	ctx := context.Background()

	sharedWithOwner := notifyAdminSampleChannel("bad-scope", 'T')
	sharedWithOwner.OwnerAgentUID = "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	require.ErrorContains(t, store.Create(ctx, sharedWithOwner), "共享范围")

	exclusiveNoOwner := notifyAdminSampleChannel("bad-scope", 'T')
	exclusiveNoOwner.Scope = core.ScopeExclusive
	require.ErrorContains(t, store.Create(ctx, exclusiveNoOwner), "属主")

	envWithCredential := notifyAdminSampleChannel("bad-mode", 'V')
	envWithCredential.CredentialID = "cred-1"
	require.ErrorContains(t, store.Create(ctx, envWithCredential), "env 模式")

	managedWithURL := notifyAdminSampleChannel("bad-mode", 'V')
	managedWithURL.CredentialMode = core.CredentialModeManaged
	require.ErrorContains(t, store.Create(ctx, managedWithURL), "managed 模式")

	require.Zero(t, notifyAdminCountChannels(t, db), "形状不合格的写请求必须零落库")
}

// notifyRepoInsertChannel 直接向 notify_channels 落一行夹具（v3 形状：主键是资源 UID，
// 地址列与凭据模式列齐全）。返回 UID 供用例断言；时刻显式传入（仓储不自取业务时间）。
// notifyRepoInsertChannel writes one v3-shaped fixture row and returns its uid.
func notifyRepoInsertChannel(t *testing.T, db *DB, name, typ, url string) core.ResourceUID {
	t.Helper()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	createdAt := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	_, err = db.pool.ExecContext(context.Background(),
		`INSERT INTO notify_channels
		   (resource_uid, name, type, url, url_host, credential_mode, scope, revision,
		    description, created_at, updated_at, operation_id)
		 VALUES ($1, $2, $3, $4, '', 'env', 'shared', 1, '', $5, $5, 'op-fixture-notify')`,
		string(uid), name, typ, url, createdAt.UnixMilli())
	require.NoError(t, err)
	return uid
}

// notifyRepoChannelNames 把读取结果投影成「名称 / 类型 / 地址」三元组：发送读面的契约只
// 消费这三列，其余身份 / 版本列由管理写面的用例单独判。
// notifyRepoChannelNames projects the read result onto the three consumed columns.
func notifyRepoChannelNames(channels []core.NotifyChannel) []core.NotifyChannel {
	out := make([]core.NotifyChannel, 0, len(channels))
	for _, ch := range channels {
		out = append(out, core.NotifyChannel{Name: ch.Name, Type: ch.Type, URL: ch.URL})
	}
	return out
}

// TestNotifyChannelRepo_ListsRowsOrderedByName —— S1（约束 4）：读取按 `name` 升序，
// 三列原样返回；插入顺序与之无关，排序由 SQL 固定（可用渠道名文案与测试可复现）。
// S1: rows come back ordered by name regardless of insertion order, with the three
// consumed columns returned verbatim.
func TestNotifyChannelRepo_ListsRowsOrderedByName(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	notifyRepoInsertChannel(t, db, "weekly", "webhook", "${WEEKLY_HOOK}")
	notifyRepoInsertChannel(t, db, "alerts", "feishu", "${ALERTS_HOOK}")
	notifyRepoInsertChannel(t, db, "ops", "feishu", "${OPS_HOOK}")

	repo := NewNotifyChannelRepo(db)
	got, err := repo.ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []core.NotifyChannel{
		{Name: "alerts", Type: "feishu", URL: "${ALERTS_HOOK}"},
		{Name: "ops", Type: "feishu", URL: "${OPS_HOOK}"},
		{Name: "weekly", Type: "webhook", URL: "${WEEKLY_HOOK}"},
	}, notifyRepoChannelNames(got))
}

// TestNotifyChannelRepo_EmptyRegistryReturnsEmptySlice —— S2（约束 2 / 4）：空表返回
// 空切片且不是错误——「没有渠道可发」与「读取失败」是两条不同的诊断路径。
// S2: an empty registry yields an empty slice, never an error.
func TestNotifyChannelRepo_EmptyRegistryReturnsEmptySlice(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)

	repo := NewNotifyChannelRepo(db)
	got, err := repo.ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.NotNil(t, got, "空表返回空切片（不是 nil）")
	require.Empty(t, got)
}

// TestNotifyChannelRepo_ContextErrorPropagates —— S3：ctx 已取消时读取直接失败，
// 错误链保留 `context.Canceled`（不吞、不换文案——分类由调用方按身份判定）。
// S3: a canceled context propagates with its identity intact.
func TestNotifyChannelRepo_ContextErrorPropagates(t *testing.T) {
	db := portTestDB(t)
	repo := NewNotifyChannelRepo(db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := repo.ListNotifyChannels(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

// TestNotifyChannelRepo_SeesRowsInsertedAfterConstruction —— S4（约束 4 / 注 4）：
// 仓储构造后新增的行下一发即可见——零缓存，US-12 / 手工 SQL 的改动不要求重建仓储。
// S4: rows inserted after construction are visible on the next read — zero caching.
func TestNotifyChannelRepo_SeesRowsInsertedAfterConstruction(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	repo := NewNotifyChannelRepo(db)

	before, err := repo.ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, before)

	notifyRepoInsertChannel(t, db, "ops", "feishu", "${OPS_HOOK}")
	after, err := repo.ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []core.NotifyChannel{{Name: "ops", Type: "feishu", URL: "${OPS_HOOK}"}},
		notifyRepoChannelNames(after))
}

// TestNotifyChannelRepo_ExcludesTombstones —— S5（US-12 R4）：墓碑行（`deleted_at`
// 非空）不出现在发送读面——删除过的渠道**不得**再收到投递；同名重建的新行照常出现。
// S5: tombstoned rows never reach the send path, while a same-name rebuild does.
func TestNotifyChannelRepo_ExcludesTombstones(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	oldUID := notifyRepoInsertChannel(t, db, "ops", "feishu", "${OPS_HOOK}")

	_, err := db.pool.ExecContext(ctx,
		`UPDATE notify_channels SET deleted_at = $1, updated_at = $2, operation_id = $3
		  WHERE resource_uid = $4`, int64(1_760_000_000_000), int64(1_760_000_000_000),
		"op-fixture-delete", string(oldUID))
	require.NoError(t, err)

	got, err := NewNotifyChannelRepo(db).ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, got, "墓碑行不得出现在发送读面")

	notifyRepoInsertChannel(t, db, "ops", "feishu", "${NEW_OPS_HOOK}")
	got, err = NewNotifyChannelRepo(db).ListNotifyChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []core.NotifyChannel{{Name: "ops", Type: "feishu", URL: "${NEW_OPS_HOOK}"}},
		notifyRepoChannelNames(got), "同名重建的新行必须出现在发送读面")
}
