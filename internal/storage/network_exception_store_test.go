package storage

// 本文件判卷 `network_exceptions`（schema v6）的仓储面：稳定排序、重复登记不覆盖理由、
// 删除未命中报 `ErrResourceNotFound`、操作者经审计行回读、底层故障不被伪装成业务结论。
// These tests pin the network exception store: stable order, no silent overwrite of the
// recorded reason, and honest failure classification.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// newNetworkExceptionStore 给每个用例一个独立的真库。
func newNetworkExceptionStore(t *testing.T) (*NetworkExceptionStore, *DB) {
	t.Helper()
	db := openTempDB(t)
	return NewNetworkExceptionStore(db), db
}

// exceptionFixture 造一条完整例外行（时间取固定时钟，判据不依赖实现自取时钟）。
func exceptionFixture(id, kind, value, note string) core.NetworkException {
	return core.NetworkException{
		ExceptionID: id,
		Kind:        kind,
		Value:       value,
		Note:        note,
		CreatedAt:   policyNow,
		OperationID: "op-exc",
	}
}

// TestNetworkExceptionStore_ListOrderAndLookup —— List 按 `kind` 升序、同 kind 内按
// `value` 升序（与写入顺序无关）；Lookup 命中返回与写入一致的行、未命中 ok=false 且
// 不是错误。
// 逆改点：ORDER BY 改成插入顺序 / Lookup 未命中报错 → 本用例红。
func TestNetworkExceptionStore_ListOrderAndLookup(t *testing.T) {
	store, _ := newNetworkExceptionStore(t)
	ctx := context.Background()

	for _, exc := range []core.NetworkException{
		exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z30", "ip_port", "10.0.0.2:443", "内网直连"),
		exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z31", "host", "b.example.com", "业务回调"),
		exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z32", "host", "a.example.com", "行情接口"),
	} {
		require.NoError(t, store.Create(ctx, exc))
	}

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 3)
	require.Equal(t, []string{"a.example.com", "b.example.com", "10.0.0.2:443"},
		[]string{listed[0].Value, listed[1].Value, listed[2].Value})
	require.Equal(t, "host", listed[0].Kind)
	require.Equal(t, int64(1), listed[0].Revision, "首写修订号固定为 1")
	require.Equal(t, "op-exc", listed[0].OperationID)
	require.True(t, listed[0].CreatedAt.Equal(policyNow), "时间以 UTC 毫秒读回")
	require.Empty(t, listed[0].CreatedBy, "没有审计行时操作者投影为空串")

	found, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z31")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "b.example.com", found.Value)
	require.Equal(t, "业务回调", found.Note)

	_, ok, err = store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z39")
	require.NoError(t, err)
	require.False(t, ok, "未命中是合法答案，不是错误")
}

// TestNetworkExceptionStore_DuplicatePairNeverOverwrites —— `(kind, value)` 重复登记
// 映射为 `ErrPolicyRevisionConflict`，既有行的理由、标识与时间一字不动。
// 逆改点：改成 upsert / INSERT OR REPLACE → 原有审计理由被悄悄替换，本用例红。
func TestNetworkExceptionStore_DuplicatePairNeverOverwrites(t *testing.T) {
	store, _ := newNetworkExceptionStore(t)
	ctx := context.Background()
	original := exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z33", "host", "hooks.example.net", "原始理由")
	require.NoError(t, store.Create(ctx, original))

	dup := exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z34", "host", "hooks.example.net", "篡改理由")
	err := store.Create(ctx, dup)
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict)
	require.Contains(t, err.Error(), "hooks.example.net")

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1, "重复登记不得留下第二行")
	require.Equal(t, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z33", listed[0].ExceptionID)
	require.Equal(t, "原始理由", listed[0].Note)
}

// TestNetworkExceptionStore_CreateRejectsInvalidShape —— 空理由 / 非法 kind 被 schema
// 检出来，且**不得**被错认成重复登记（管理台会据此显示"已存在"）。
// 逆改点：所有写错误一律包成 ErrPolicyRevisionConflict → 本用例红。
func TestNetworkExceptionStore_CreateRejectsInvalidShape(t *testing.T) {
	store, _ := newNetworkExceptionStore(t)
	ctx := context.Background()

	err := store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z35", "host", "a.example.com", ""))
	require.Error(t, err, "放行必须带理由")
	require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict))
	require.Contains(t, err.Error(), "create network exception")

	err = store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z36", "domain", "a.example.com", "非法类别"))
	require.Error(t, err)
	require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict))
}

// TestNetworkExceptionStore_DeleteReportsMissing —— 删除既有行生效；再删同一标识返回
// `core.ErrResourceNotFound`（不是静默成功）。
// 逆改点：按 0 行影响当成功返回 → 页面把"已被别人删掉"显示成删除成功，本用例红。
func TestNetworkExceptionStore_DeleteReportsMissing(t *testing.T) {
	store, _ := newNetworkExceptionStore(t)
	ctx := context.Background()
	require.NoError(t, store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z37",
		"ip_port", "192.0.2.10:8443", "备用出口")))

	require.NoError(t, store.Delete(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z37"))
	_, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z37")
	require.NoError(t, err)
	require.False(t, ok)

	require.ErrorIs(t, store.Delete(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z37"), core.ErrResourceNotFound)
}

// TestNetworkExceptionStore_ActorFromAuditRow —— 写入者来自审计行（按 `operation_id`
// 关联），例外表本身不存操作者。
// 逆改点：改成在例外表里存操作者 → 审计与例外可能分叉，本用例红。
func TestNetworkExceptionStore_ActorFromAuditRow(t *testing.T) {
	store, db := newNetworkExceptionStore(t)
	ctx := context.Background()
	seedPolicyOperation(t, db, "op-exc", "token:abc123")
	require.NoError(t, store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z38",
		"host", "api.example.com", "对账接口")))

	listed, err := store.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "token:abc123", listed[0].CreatedBy)

	found, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z38")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "token:abc123", found.CreatedBy)
}

// TestNetworkExceptionStore_ErrorPathsKeepClassification —— 池故障带上下文前缀返回，
// 且不得被误判为"未找到"或"重复登记"。
// 逆改点：错误被吞成 nil / ok=false → 页面把存储故障显示成"例外不存在"，本用例红。
func TestNetworkExceptionStore_ErrorPathsKeepClassification(t *testing.T) {
	ctx := context.Background()

	t.Run("list", func(t *testing.T) {
		store, db := newNetworkExceptionStore(t)
		closeReadPool(t, db)
		_, err := store.List(ctx)
		require.Error(t, err)
		require.Contains(t, err.Error(), "list network exceptions")
	})

	t.Run("lookup", func(t *testing.T) {
		store, db := newNetworkExceptionStore(t)
		closeReadPool(t, db)
		_, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z30")
		require.Error(t, err, "存储故障不得伪装成未命中")
		require.False(t, ok)
		require.Contains(t, err.Error(), "lookup network exception")
	})

	t.Run("create_and_delete", func(t *testing.T) {
		store, db := newNetworkExceptionStore(t)
		closeWritePool(t, db)
		err := store.Create(ctx, exceptionFixture("exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z30",
			"host", "a.example.com", "理由"))
		require.Error(t, err)
		require.False(t, errors.Is(err, core.ErrPolicyRevisionConflict))
		require.Contains(t, err.Error(), "create network exception")

		err = store.Delete(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z30")
		require.Error(t, err)
		require.False(t, errors.Is(err, core.ErrResourceNotFound))
		require.Contains(t, err.Error(), "delete network exception")
	})
}

// TestNetworkExceptionStore_CreatedAtIsStoredAsMillis —— 创建时间按 Unix 毫秒落库，
// 读回仍是同一时刻（UTC）：页面按它排序 / 展示，不得随时区漂移。
func TestNetworkExceptionStore_CreatedAtIsStoredAsMillis(t *testing.T) {
	store, db := newNetworkExceptionStore(t)
	ctx := context.Background()
	moment := time.Date(2026, 3, 1, 23, 59, 30, 250_000_000, time.FixedZone("UTC+8", 8*3600))
	require.NoError(t, store.Create(ctx, core.NetworkException{
		ExceptionID: "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
		Kind:        "host",
		Value:       "tz.example.com",
		Note:        "时区往返",
		CreatedAt:   moment,
		OperationID: "op-tz",
	}))

	var stored int64
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT created_at FROM network_exceptions WHERE exception_id = ?`,
		"exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A").Scan(&stored))
	require.Equal(t, moment.UnixMilli(), stored)

	found, ok, err := store.Lookup(ctx, "exc_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, found.CreatedAt.Equal(moment))
	require.Equal(t, time.UTC, found.CreatedAt.Location())
}
