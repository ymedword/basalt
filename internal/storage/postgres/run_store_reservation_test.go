//go:build integration

package postgres

// 本文件判 US-16 §3.4.2「写入顺序」的第三句：准入预留先以 `run_id = NULL` 落库，
// 账本行插入后**在同一事务**里回填 `quota_reservations.run_id`。
//
// 判据面（正向 + 三条反向）：
//
//   - 有清单路径 / 零清单路径都要回填，且 `runs.reservation_id` 指向同一条预留；
//   - 预留行不存在 → 失败关闭，账本行与清单零落痕（不能留一条查不到消耗的 Run）；
//   - 预留属于别的调用方 → 失败关闭（调用方之间不得互相挂账）；
//   - 预留已挂到别的 Run → 失败关闭（账目矛盾必须报出来，不改口）；
//   - 同一 Run 的重放 → 幂等通过（`run_id IS NULL` 谓词让重试不翻车）。
//
// 由 US-16 T025 / T090 的服务级接线落地后补齐；此前 tasks.md:1124 记录的"保持
// NULL"延期与设计正文不一致，本文件是该项的设计侧判据。
// These cases pin the same-transaction reservation backfill, including its
// fail-closed branches and the idempotent replay.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// seedAdmissionReservation 种一条尚未挂 Run 的准入预留（`run_id = NULL`），
// 模拟 `QuotaStore.Reserve` 的写入形态。
// seedAdmissionReservation inserts one unlinked admission reservation.
func seedAdmissionReservation(t *testing.T, db *DB, reservationID, callerID string) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO quota_reservations
		   (reservation_id, caller_id, idempotency_key, class, run_id,
		    period_start, amount, actual, overage, state, created_at, settled_at)
		VALUES ($1, $2, $1, 'run', NULL, $3, 1, NULL, 0, 'pending', $3, NULL)`,
		reservationID, callerID, fixedStart.UnixMilli())
	require.NoError(t, err)
}

// readReservationRunID 读预留行上的 run_id（NULL 读作空串）。
// readReservationRunID reads the linked run id (NULL reads as empty).
func readReservationRunID(t *testing.T, db *DB, reservationID string) string {
	t.Helper()
	var linked sql.NullString
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT run_id FROM quota_reservations WHERE reservation_id = $1`,
		reservationID).Scan(&linked))
	return linked.String
}

// readRunReservationID 读账本行记下的准入预留标识（NULL 读作空串）。
// readRunReservationID reads the admission reservation recorded on the run row.
func readRunReservationID(t *testing.T, db *DB, runID string) string {
	t.Helper()
	var linked sql.NullString
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT reservation_id FROM runs WHERE run_id = $1`, runID).Scan(&linked))
	return linked.String
}

// TestRunStore_StartRunLinksAdmissionReservation —— 有清单路径：账本行与预留归属
// 一次提交，`runs.reservation_id` 与 `quota_reservations.run_id` 互指同一条预留。
// 逆改点：删掉同事务里的回填 UPDATE → 预留 run_id 保持 NULL，本用例红。
// TestRunStore_StartRunLinksAdmissionReservation pins the backfill on the manifest path.
func TestRunStore_StartRunLinksAdmissionReservation(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	reservation := "rsv-admit-manifest"
	seedAdmissionReservation(t, db, reservation, core.LocalAdminCallerID)

	rec := startRow("web")
	rec.ReservationID = reservation
	require.NoError(t, store.StartRun(ctx, rec, fx.manifest))

	require.Equal(t, reservation, readRunReservationID(t, db, rec.RunID),
		"账本行必须记下准入预留")
	require.Equal(t, rec.RunID, readReservationRunID(t, db, reservation),
		"预留必须在同一事务里回填 run_id（§3.4.2 写入顺序）")
	require.Equal(t, string(core.StatusRunning), readRunRow(t, db, rec.RunID).Status)
}

// TestRunStore_StartRunLinksReservationWithoutManifest —— 零清单路径同样回填：
// 轻装配不写清单，但预留归属仍要与账本行同事务落下。
// TestRunStore_StartRunLinksReservationWithoutManifest pins the zero-manifest path.
func TestRunStore_StartRunLinksReservationWithoutManifest(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)

	reservation := "rsv-admit-zeromanifest"
	seedAdmissionReservation(t, db, reservation, core.LocalAdminCallerID)

	rec := startRow("cli")
	rec.ReservationID = reservation
	require.NoError(t, store.StartRun(ctx, rec, core.RunResourceManifest{}))

	require.Equal(t, reservation, readRunReservationID(t, db, rec.RunID))
	require.Equal(t, rec.RunID, readReservationRunID(t, db, reservation),
		"零清单路径也必须回填 run_id")

	var manifests int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM run_resource_manifests WHERE run_id = $1`, rec.RunID).Scan(&manifests))
	require.Zero(t, manifests, "回填不改变零清单的语义：仍然不伪造清单行")
}

// TestRunStore_StartRunRejectsUnknownReservation —— 预留行不存在时失败关闭：
// 宁可拒绝启动，也不留一条管理面查不到消耗的 Run。整笔回滚，账本零落痕。
// TestRunStore_StartRunRejectsUnknownReservation fails closed on a missing reservation.
func TestRunStore_StartRunRejectsUnknownReservation(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)
	fx := seedRunManifest(t, db)

	rec := startRow("web")
	rec.ReservationID = "rsv-does-not-exist"
	err := store.StartRun(ctx, rec, fx.manifest)
	require.Error(t, err)
	require.Contains(t, err.Error(), "回填预留归属", "失败点必须是预留回填")
	require.Contains(t, err.Error(), "不存在", "错误必须说清是预留不存在")
	requireNoRunTrace(t, db, rec.RunID)
}

// TestRunStore_StartRunRejectsReservationOwnedByAnotherCaller —— 预留属于别的
// 调用方：调用方之间不得互相挂账，失败关闭。
// TestRunStore_StartRunRejectsReservationOwnedByAnotherCaller refuses a foreign charge.
func TestRunStore_StartRunRejectsReservationOwnedByAnotherCaller(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)

	quotaSeedCaller(t, db, "caller-other", "service")
	reservation := "rsv-owned-by-other"
	seedAdmissionReservation(t, db, reservation, "caller-other")

	rec := startRow("cli")
	rec.ReservationID = reservation
	err := store.StartRun(ctx, rec, core.RunResourceManifest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "回填预留归属")
	require.Contains(t, err.Error(), "不属于调用方", "错误必须点名归属不符")
	requireNoRunTrace(t, db, rec.RunID)
	require.Empty(t, readReservationRunID(t, db, reservation), "别人的预留不得被挂上")
}

// TestRunStore_StartRunRejectsReservationOfAnotherRun —— 预留已挂到别的 Run：
// 账目矛盾必须报出来，不能改口静默重挂。
// TestRunStore_StartRunRejectsReservationOfAnotherRun refuses a reservation already
// linked to a different run.
func TestRunStore_StartRunRejectsReservationOfAnotherRun(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)

	reservation := "rsv-already-linked"
	seedAdmissionReservation(t, db, reservation, core.LocalAdminCallerID)

	first := startRow("cli")
	first.ReservationID = reservation
	require.NoError(t, store.StartRun(ctx, first, core.RunResourceManifest{}))

	second := startRow("web")
	second.ReservationID = reservation
	err := store.StartRun(ctx, second, core.RunResourceManifest{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "已属于 Run", "错误必须点名该预留的实际归属")
	requireNoRunTrace(t, db, second.RunID)
	require.Equal(t, first.RunID, readReservationRunID(t, db, reservation),
		"失败不得改动既有挂接")
}

// TestBackfillReservationRun_IsIdempotentForSameRun —— 同一 Run 的重放幂等：
// 回填谓词带 `run_id IS NULL`，已挂同一 Run 时返回成功而不是"已属于"。
// TestBackfillReservationRun_IsIdempotentForSameRun pins the replay branch.
func TestBackfillReservationRun_IsIdempotentForSameRun(t *testing.T) {
	ctx := context.Background()
	db := portTestDB(t)
	store := NewRunStore(db)

	reservation := "rsv-replay"
	seedAdmissionReservation(t, db, reservation, core.LocalAdminCallerID)

	rec := startRow("cli")
	rec.ReservationID = reservation
	require.NoError(t, store.StartRun(ctx, rec, core.RunResourceManifest{}))

	require.NoError(t, backfillReservationRun(ctx, db.pool, rec),
		"同一 Run 的重放必须幂等通过")
	require.Equal(t, rec.RunID, readReservationRunID(t, db, reservation))
}
