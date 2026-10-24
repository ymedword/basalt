package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是管理操作仓储的判卷（颗粒度文档 §3.1.16，check `011-storage-management`）。
// 覆盖：预写、终态回填、幂等、过滤与窗口、pending 查询、恢复三态写入、并发回填，
// 以及 FR-012 / 约束 44 的"管理审计不写运行账本、不挂 run_id"。
// These tests pin the management operation store: pre-write, terminal write, idempotence,
// filtering and window, pending scan, recovery, concurrent finish, and the isolation of
// management audit from the run ledger (FR-012 / constraint 44).

var opNow = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// rawOperation 是**绕过领域映射**读回的一行（可空列保留 sql.Null*），专门用来判 NULL
// 到底写成了什么——经 scanOperation 读回只能验"值"，验不了"NULL vs 空串"。
// rawOperation reads a row without the domain mapping so NULL vs empty string stays visible.
type rawOperation struct {
	RequestHash        sql.NullString
	RequestedVersionID sql.NullInt64
	ExpectedETag       sql.NullString
	Status             string
	ResultCode         sql.NullString
	ErrorMessage       sql.NullString
	DetailJSON         []byte
	CreatedAt          int64
	FinishedAt         sql.NullInt64
	RecoveredAt        sql.NullInt64
	Verification       sql.NullString
}

func readRawOperation(t *testing.T, db *DB, operationID string) rawOperation {
	t.Helper()
	var row rawOperation
	err := db.read.QueryRowContext(context.Background(),
		`SELECT request_hash, requested_version_id, expected_etag, status, result_code,
		        error_message, detail_json, created_at, finished_at, recovered_at, verification
		   FROM management_operations WHERE operation_id = ?`, operationID).
		Scan(&row.RequestHash, &row.RequestedVersionID, &row.ExpectedETag, &row.Status,
			&row.ResultCode, &row.ErrorMessage, &row.DetailJSON, &row.CreatedAt,
			&row.FinishedAt, &row.RecoveredAt, &row.Verification)
	require.NoError(t, err)
	return row
}

// beginTestOperation 写一条最简预写行；用例只在真正关心的列上另行赋值。
func beginTestOperation(t *testing.T, store *ManagementStore, operationID string, createdAt time.Time) {
	t.Helper()
	require.NoError(t, store.BeginOperation(context.Background(), core.OperationRecord{
		OperationID:  operationID,
		Action:       "agent.content.update",
		TargetKind:   "agent",
		TargetID:     "writer",
		ActorSubject: core.ActorSubjectLocalTrust,
		AuthMethod:   core.AuthMethodLocalTrust,
		Source:       "127.0.0.1:50001",
		CreatedAt:    createdAt,
	}))
}

// TestManagementStore_BeginOperationWritesPending：预写行的状态由仓储固定写 pending，
// `detail_json` 空值存 `{}`，可空列的零值写 NULL（不是空串 / 0），created_at 存毫秒。
// 中文用例名：预写只落 pending、空 detail 存 {}、可空列写 NULL。
func TestManagementStore_BeginOperationWritesPending(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)

	// 情形一：可空列全空。
	beginTestOperation(t, store, "op-1-aaaaaa", opNow)
	row := readRawOperation(t, db, "op-1-aaaaaa")
	require.Equal(t, string(core.OperationPending), row.Status)
	require.False(t, row.RequestHash.Valid)
	require.False(t, row.RequestedVersionID.Valid)
	require.False(t, row.ExpectedETag.Valid)
	require.False(t, row.ResultCode.Valid)
	require.False(t, row.ErrorMessage.Valid)
	require.False(t, row.FinishedAt.Valid)
	require.False(t, row.RecoveredAt.Valid)
	require.False(t, row.Verification.Valid)
	require.JSONEq(t, `{}`, string(row.DetailJSON), "空 detail 必须存 {}，不存 NULL")
	require.Equal(t, opNow.UnixMilli(), row.CreatedAt)

	// 情形二：可空列给值——原样落列，不丢。
	require.NoError(t, store.BeginOperation(context.Background(), core.OperationRecord{
		OperationID:        "op-2-bbbbbb",
		Action:             "agent.content.update",
		TargetKind:         "agent",
		TargetID:           "writer",
		ActorSubject:       core.ActorSubjectTokenPrefix + "0123456789abcdef",
		AuthMethod:         core.AuthMethodBearer,
		Source:             "10.0.0.8:41000",
		RequestHash:        "sha256:abc",
		RequestedVersionID: 7,
		ExpectedETag:       `"sha256:def"`,
		DetailJSON:         []byte(`{"chars":12}`),
		CreatedAt:          opNow.Add(time.Second),
	}))
	row = readRawOperation(t, db, "op-2-bbbbbb")
	require.Equal(t, "sha256:abc", row.RequestHash.String)
	require.Equal(t, int64(7), row.RequestedVersionID.Int64)
	require.Equal(t, `"sha256:def"`, row.ExpectedETag.String)
	require.JSONEq(t, `{"chars":12}`, string(row.DetailJSON))
}

// TestManagementStore_BeginOperationRejectsTerminalStatus：预写不接受终态——不存在
// "预写即成功"的捷径；空标识同样拒绝，且失败时零写入。
// 中文用例名：预写拒绝终态与空 operation_id，失败零写入。
func TestManagementStore_BeginOperationRejectsTerminalStatus(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)

	err := store.BeginOperation(context.Background(), core.OperationRecord{
		OperationID: "op-3-cccccc",
		Status:      core.OperationSuccess,
	})
	require.ErrorContains(t, err, "pending")

	err = store.BeginOperation(context.Background(), core.OperationRecord{Action: "agent.disable"})
	require.ErrorContains(t, err, "operation_id")

	var count int
	require.NoError(t, db.read.QueryRowContext(context.Background(),
		`SELECT count(*) FROM management_operations`).Scan(&count))
	require.Zero(t, count)
	_, err = store.GetOperation(context.Background(), "op-3-cccccc")
	require.ErrorIs(t, err, core.ErrOperationNotFound)
}

// TestManagementStore_FinishOperationWritesTerminalState：终态回填写 status / result_code /
// detail / finished_at；`recovered_at` 与 `verification` 留给恢复路径，正常完成保持 NULL。
// 中文用例名：终态回填只写终态列，恢复列保持 NULL。
func TestManagementStore_FinishOperationWritesTerminalState(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)
	beginTestOperation(t, store, "op-4-dddddd", opNow)

	finishedAt := opNow.Add(2 * time.Minute)
	require.NoError(t, store.FinishOperation(context.Background(), "op-4-dddddd",
		core.OperationSuccess, "OK", "",
		[]byte(`{"changed":["AGENT.md"]}`), finishedAt))

	row := readRawOperation(t, db, "op-4-dddddd")
	require.Equal(t, string(core.OperationSuccess), row.Status)
	require.Equal(t, "OK", row.ResultCode.String)
	require.False(t, row.ErrorMessage.Valid)
	require.JSONEq(t, `{"changed":["AGENT.md"]}`, string(row.DetailJSON))
	require.Equal(t, finishedAt.UnixMilli(), row.FinishedAt.Int64)
	require.False(t, row.RecoveredAt.Valid)
	require.False(t, row.Verification.Valid)

	rec, err := store.GetOperation(context.Background(), "op-4-dddddd")
	require.NoError(t, err)
	require.Equal(t, core.OperationSuccess, rec.Status)
	require.Equal(t, "OK", rec.ResultCode)
	require.True(t, rec.FinishedAt.Equal(finishedAt))
	require.Zero(t, rec.RecoveredAt)
	require.Empty(t, rec.Verification)
}

// TestManagementStore_FinishOperationIsIdempotent：已终态的行**不得**被第二次回填覆盖
// （并发收口与正常收口撞车时，先落的那次是事实）。
// 中文用例名：二次回填幂等、不覆盖已写终态。
func TestManagementStore_FinishOperationIsIdempotent(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)
	beginTestOperation(t, store, "op-5-eeeeee", opNow)

	firstAt := opNow.Add(time.Minute)
	require.NoError(t, store.FinishOperation(context.Background(), "op-5-eeeeee",
		core.OperationSuccess, "OK", "", []byte(`{"n":1}`), firstAt))
	require.NoError(t, store.FinishOperation(context.Background(), "op-5-eeeeee",
		core.OperationFailed, "BAD_REQUEST", "第二次回填不得生效", []byte(`{"n":2}`), opNow.Add(2*time.Minute)))

	row := readRawOperation(t, db, "op-5-eeeeee")
	require.Equal(t, string(core.OperationSuccess), row.Status)
	require.Equal(t, "OK", row.ResultCode.String)
	require.False(t, row.ErrorMessage.Valid)
	require.JSONEq(t, `{"n":1}`, string(row.DetailJSON))
	require.Equal(t, firstAt.UnixMilli(), row.FinishedAt.Int64)
}

// TestManagementStore_FinishOperationTruncatesErrorMessage：失败原文按 4 KB 截断且不切坏
// UTF-8（契约 §7.4）；截断后的内容是原文前缀。
// 中文用例名：失败原文 4 KB 截断、UTF-8 安全。
func TestManagementStore_FinishOperationTruncatesErrorMessage(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)
	beginTestOperation(t, store, "op-6-ffffff", opNow)

	long := strings.Repeat("错", 2000) // 6000 字节，超过 4096
	require.NoError(t, store.FinishOperation(context.Background(), "op-6-ffffff",
		core.OperationFailed, "INTERNAL", long, nil, opNow.Add(time.Minute)))

	row := readRawOperation(t, db, "op-6-ffffff")
	require.LessOrEqual(t, len(row.ErrorMessage.String), maxErrorMessageBytes)
	require.True(t, utf8.ValidString(row.ErrorMessage.String))
	require.True(t, strings.HasPrefix(long, row.ErrorMessage.String))
	require.Equal(t, "INTERNAL", row.ResultCode.String)
}

// TestManagementStore_TerminalWriteKeepsPendingDetail：终态回填与恢复在**不带** detail
// 时必须保留预写投影（凭据与范围动作的四证就写在预写行里），带了才覆盖。
// 逆改点：把空入参也规整成 `{}` → `COALESCE` 拿到非 NULL，预写投影被清空，两条断言红。
// 中文用例名：终态回填 / 恢复保留预写 detail，带 detail 时覆盖。
func TestManagementStore_TerminalWriteKeepsPendingDetail(t *testing.T) {
	seed := func(t *testing.T) (*ManagementStore, *DB) {
		t.Helper()
		db := openTempDB(t)
		store := NewManagementStore(db)
		require.NoError(t, store.BeginOperation(context.Background(), core.OperationRecord{
			OperationID:  "op-8-hhhhhh",
			Action:       "model_connection.credential.write",
			TargetKind:   "model_connection",
			TargetID:     "model_connection_uid",
			ActorSubject: core.ActorSubjectLocalTrust,
			AuthMethod:   core.AuthMethodLocalTrust,
			Source:       "127.0.0.1:50001",
			DetailJSON:   []byte(`{"credential_id":"cred_seeded","version_no":1}`),
			CreatedAt:    opNow,
		}))
		return store, db
	}

	t.Run("终态回填不带 detail 保留预写投影", func(t *testing.T) {
		store, db := seed(t)
		require.NoError(t, store.FinishOperation(context.Background(), "op-8-hhhhhh",
			core.OperationSuccess, "", "", nil, opNow.Add(time.Minute)))

		row := readRawOperation(t, db, "op-8-hhhhhh")
		require.JSONEq(t, `{"credential_id":"cred_seeded","version_no":1}`, string(row.DetailJSON))
	})

	t.Run("终态回填带 detail 覆盖预写投影", func(t *testing.T) {
		store, db := seed(t)
		require.NoError(t, store.FinishOperation(context.Background(), "op-8-hhhhhh",
			core.OperationSuccess, "", "", []byte(`{"result":"ok"}`), opNow.Add(time.Minute)))

		row := readRawOperation(t, db, "op-8-hhhhhh")
		require.JSONEq(t, `{"result":"ok"}`, string(row.DetailJSON))
	})

	t.Run("恢复不带 detail 保留预写投影", func(t *testing.T) {
		store, db := seed(t)
		require.NoError(t, store.RecoverOperation(context.Background(), "op-8-hhhhhh",
			core.OperationUnknown, core.VerificationIndeterminate, nil, opNow.Add(time.Minute)))

		row := readRawOperation(t, db, "op-8-hhhhhh")
		require.JSONEq(t, `{"credential_id":"cred_seeded","version_no":1}`, string(row.DetailJSON))
	})
}

// TestManagementStore_GetOperationRoundTripsNullColumns：领域形态与 NULL 的往返——
// 空值读回零值、非空值逐字保留；不存在的标识映射 core.ErrOperationNotFound。
// 中文用例名：Get 的 NULL 往返与 not-found。
func TestManagementStore_GetOperationRoundTripsNullColumns(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)
	beginTestOperation(t, store, "op-7-gggggg", opNow)
	require.NoError(t, store.FinishOperation(context.Background(), "op-7-gggggg",
		core.OperationSuccess, "", "", nil, opNow.Add(time.Minute)))

	rec, err := store.GetOperation(context.Background(), "op-7-gggggg")
	require.NoError(t, err)
	require.Equal(t, "op-7-gggggg", rec.OperationID)
	require.Equal(t, "agent.content.update", rec.Action)
	require.Equal(t, "agent", rec.TargetKind)
	require.Equal(t, "writer", rec.TargetID)
	require.Equal(t, core.ActorSubjectLocalTrust, rec.ActorSubject)
	require.Equal(t, core.AuthMethodLocalTrust, rec.AuthMethod)
	require.Equal(t, "127.0.0.1:50001", rec.Source)
	require.Empty(t, rec.RequestHash)
	require.Zero(t, rec.RequestedVersionID)
	require.Empty(t, rec.ExpectedETag)
	require.Equal(t, core.OperationSuccess, rec.Status)
	require.Empty(t, rec.ResultCode)
	require.Empty(t, rec.ErrorMessage)
	require.JSONEq(t, `{}`, string(rec.DetailJSON))
	require.True(t, rec.CreatedAt.Equal(opNow))
	require.False(t, rec.FinishedAt.IsZero())
	require.True(t, rec.RecoveredAt.IsZero())
	require.Empty(t, rec.Verification)

	_, err = store.GetOperation(context.Background(), "op-missing")
	require.ErrorIs(t, err, core.ErrOperationNotFound)
}

// TestManagementStore_ListOperationsFiltersAndFixedOrder：四个过滤值可单独或组合使用；
// 排序固定 `created_at DESC, operation_id DESC`（同毫秒用标识倒序兜底，顺序不漂）。
// 中文用例名：列表过滤矩阵与固定排序。
func TestManagementStore_ListOperationsFiltersAndFixedOrder(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)

	mk := func(id, action, targetKind, targetID string, createdAt time.Time) {
		t.Helper()
		require.NoError(t, store.BeginOperation(context.Background(), core.OperationRecord{
			OperationID: id, Action: action, TargetKind: targetKind, TargetID: targetID,
			ActorSubject: core.ActorSubjectMigration, AuthMethod: core.AuthMethodMigration,
			Source: "migration", CreatedAt: createdAt,
		}))
	}
	mk("op-1000-aaaaaa", "agent.disable", "agent", "writer", time.UnixMilli(1000))
	mk("op-1000-bbbbbb", "agent.enable", "agent", "writer", time.UnixMilli(1000))
	mk("op-2000-cccccc", "agent.disable", "agent", "reader", time.UnixMilli(2000))
	require.NoError(t, store.FinishOperation(context.Background(), "op-1000-bbbbbb",
		core.OperationSuccess, "OK", "", nil, opNow))

	ids := func(records []core.OperationRecord) []string {
		out := make([]string, 0, len(records))
		for _, rec := range records {
			out = append(out, rec.OperationID)
		}
		return out
	}

	all, err := store.ListOperations(context.Background(), core.OperationFilter{})
	require.NoError(t, err)
	require.Equal(t, []string{"op-2000-cccccc", "op-1000-bbbbbb", "op-1000-aaaaaa"}, ids(all))

	byAction, err := store.ListOperations(context.Background(), core.OperationFilter{Action: "agent.disable"})
	require.NoError(t, err)
	require.Equal(t, []string{"op-2000-cccccc", "op-1000-aaaaaa"}, ids(byAction))

	byKind, err := store.ListOperations(context.Background(), core.OperationFilter{TargetKind: "agent", TargetID: "reader"})
	require.NoError(t, err)
	require.Equal(t, []string{"op-2000-cccccc"}, ids(byKind))

	pending, err := store.ListOperations(context.Background(), core.OperationFilter{Status: string(core.OperationPending)})
	require.NoError(t, err)
	require.Equal(t, []string{"op-2000-cccccc", "op-1000-aaaaaa"}, ids(pending))
}

// TestManagementStore_ListOperationsClampsLimit：缺省 50、上限 200（§3.1.16）；
// 负数按缺省处理——窗口永远在 [1, 200]，调用方无法借 limit 拉全表。
// 中文用例名：列表窗口缺省 50 / 上限 200。
func TestManagementStore_ListOperationsClampsLimit(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)

	for i := 0; i < 205; i++ {
		require.NoError(t, store.BeginOperation(context.Background(), core.OperationRecord{
			OperationID:  fmt.Sprintf("op-%04d-x", i),
			Action:       "agent.content.update",
			TargetKind:   "agent",
			TargetID:     "writer",
			ActorSubject: core.ActorSubjectMigration,
			AuthMethod:   core.AuthMethodMigration,
			Source:       "migration",
			CreatedAt:    time.UnixMilli(int64(i + 1)),
		}))
	}

	defaulted, err := store.ListOperations(context.Background(), core.OperationFilter{})
	require.NoError(t, err)
	require.Len(t, defaulted, defaultOperationLimit)

	capped, err := store.ListOperations(context.Background(), core.OperationFilter{Limit: 1000})
	require.NoError(t, err)
	require.Len(t, capped, maxOperationLimit)

	negative, err := store.ListOperations(context.Background(), core.OperationFilter{Limit: -3})
	require.NoError(t, err)
	require.Len(t, negative, defaultOperationLimit)
}

// TestManagementStore_PendingOperationsOnlyPending：恢复输入只含 pending 行，
// 已终态 / 已恢复的行不再参与后续启动扫描；顺序按时间与标识升序，可复核。
// 中文用例名：pending 查询只回 pending，升序确定。
func TestManagementStore_PendingOperationsOnlyPending(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)

	beginTestOperation(t, store, "op-3000-cccccc", time.UnixMilli(3000))
	beginTestOperation(t, store, "op-1000-aaaaaa", time.UnixMilli(1000))
	beginTestOperation(t, store, "op-2000-bbbbbb", time.UnixMilli(2000))
	require.NoError(t, store.FinishOperation(context.Background(), "op-2000-bbbbbb",
		core.OperationSuccess, "OK", "", nil, opNow))
	require.NoError(t, store.RecoverOperation(context.Background(), "op-3000-cccccc",
		core.OperationUnknown, core.VerificationIndeterminate, nil, opNow))

	pending, err := store.PendingOperations(context.Background())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "op-1000-aaaaaa", pending[0].OperationID)
}

// TestManagementStore_RecoverOperationVerifiesTerminalState：恢复三组结论与终态一一对应
// （applied→success、not_applied→failed、indeterminate→unknown），同写 recovered_at 与
// verification；错配拒绝；已终态幂等不覆盖；不存在的标识返回 not-found。
// 中文用例名：恢复三态写入、错配拒绝、幂等、not-found。
func TestManagementStore_RecoverOperationVerifiesTerminalState(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)
	beginTestOperation(t, store, "op-4000-aaaaaa", time.UnixMilli(4000))
	beginTestOperation(t, store, "op-4001-bbbbbb", time.UnixMilli(4001))

	recoveredAt := opNow.Add(5 * time.Minute)
	require.NoError(t, store.RecoverOperation(context.Background(), "op-4000-aaaaaa",
		core.OperationSuccess, core.VerificationApplied, []byte(`{"verified":true}`), recoveredAt))
	require.NoError(t, store.RecoverOperation(context.Background(), "op-4001-bbbbbb",
		core.OperationUnknown, core.VerificationIndeterminate, nil, recoveredAt))

	row := readRawOperation(t, db, "op-4000-aaaaaa")
	require.Equal(t, string(core.OperationSuccess), row.Status)
	require.Equal(t, recoveredAt.UnixMilli(), row.RecoveredAt.Int64)
	require.Equal(t, core.VerificationApplied, row.Verification.String)
	require.Equal(t, recoveredAt.UnixMilli(), row.FinishedAt.Int64)
	require.JSONEq(t, `{"verified":true}`, string(row.DetailJSON))

	unknown := readRawOperation(t, db, "op-4001-bbbbbb")
	require.Equal(t, string(core.OperationUnknown), unknown.Status)
	require.Equal(t, core.VerificationIndeterminate, unknown.Verification.String)

	// 错配：success 只能配 applied。
	err := store.RecoverOperation(context.Background(), "op-4000-aaaaaa",
		core.OperationSuccess, core.VerificationIndeterminate, nil, recoveredAt)
	require.ErrorContains(t, err, "不匹配")

	// 幂等：已恢复的行不被第二次查证覆盖。
	require.NoError(t, store.RecoverOperation(context.Background(), "op-4000-aaaaaa",
		core.OperationFailed, core.VerificationNotApplied, nil, recoveredAt.Add(time.Minute)))
	again := readRawOperation(t, db, "op-4000-aaaaaa")
	require.Equal(t, string(core.OperationSuccess), again.Status)
	require.Equal(t, core.VerificationApplied, again.Verification.String)

	err = store.RecoverOperation(context.Background(), "op-missing",
		core.OperationFailed, core.VerificationNotApplied, nil, recoveredAt)
	require.ErrorIs(t, err, core.ErrOperationNotFound)
}

// TestManagementStore_ConcurrentFinish：并发回填只有一个生效（先落者是事实），
// 其余幂等返回；终态、result_code / detail / finished_at 必须来自同一次回填。
// 中文用例名：并发终态回填仅一次生效（011-race 必跑）。
func TestManagementStore_ConcurrentFinish(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)
	beginTestOperation(t, store, "op-5000-aaaaaa", time.UnixMilli(5000))

	const workers = 8
	statuses := []core.OperationStatus{
		core.OperationSuccess, core.OperationFailed, core.OperationUnknown,
	}
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status := statuses[i%len(statuses)]
			errs[i] = store.FinishOperation(context.Background(), "op-5000-aaaaaa",
				status, fmt.Sprintf("R%d", i), "",
				[]byte(fmt.Sprintf(`{"winner":%d}`, i)), opNow.Add(time.Duration(i)*time.Second))
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "worker %d", i)
	}

	row := readRawOperation(t, db, "op-5000-aaaaaa")
	require.NotEqual(t, string(core.OperationPending), row.Status)
	require.True(t, row.ResultCode.Valid)
	var winner int
	_, err := fmt.Sscanf(row.ResultCode.String, "R%d", &winner)
	require.NoError(t, err)
	require.Equal(t, string(statuses[winner%len(statuses)]), row.Status)
	require.JSONEq(t, fmt.Sprintf(`{"winner":%d}`, winner), string(row.DetailJSON))
	require.Equal(t, opNow.Add(time.Duration(winner)*time.Second).UnixMilli(), row.FinishedAt.Int64)
}

// TestManagementStore_DoesNotWriteRunLedger：管理审计与运行审计**互不混写**（FR-012 /
// 约束 44）——写管理操作不落 runs / llm_calls / tool_invocations，表结构里没有 run_id，
// 行也不在终态后被删除。
// 中文用例名：管理操作不写运行账本、不挂 run_id、终态行保留。
func TestManagementStore_DoesNotWriteRunLedger(t *testing.T) {
	db := openTempDB(t)
	store := NewManagementStore(db)
	beginTestOperation(t, store, "op-6000-aaaaaa", time.UnixMilli(6000))
	beginTestOperation(t, store, "op-6001-bbbbbb", time.UnixMilli(6001))
	require.NoError(t, store.FinishOperation(context.Background(), "op-6000-aaaaaa",
		core.OperationSuccess, "OK", "", nil, opNow))
	require.NoError(t, store.RecoverOperation(context.Background(), "op-6001-bbbbbb",
		core.OperationUnknown, core.VerificationIndeterminate, nil, opNow))

	for _, table := range []string{"runs", "llm_calls", "tool_invocations"} {
		var count int
		require.NoError(t, db.read.QueryRowContext(context.Background(),
			`SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count, "%s 不得因管理操作而写入", table)
	}

	rows, err := db.read.QueryContext(context.Background(),
		`SELECT name FROM pragma_table_info('management_operations')`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())
	require.NotContains(t, columns, "run_id", "管理操作表不得挂 run_id")

	var kept int
	require.NoError(t, db.read.QueryRowContext(context.Background(),
		`SELECT count(*) FROM management_operations`).Scan(&kept))
	require.Equal(t, 2, kept, "终态行不得删除")
}
