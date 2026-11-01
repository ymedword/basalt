package callers

// 本文件直接判卷审计模块本身（US-16 / T028 / FR-039）：`pending` 行先于任何变更、
// 终态回填剥离请求取消、序列化与回填失败都不改业务结果且必须留日志。管理面的
// 服务级顺序判据在 `admin_test.go` / `key_test.go`，真实终态落库在
// `quota_pg_test.go` 的集成用例——这里只钉共享写法器，避免两处实现各自漂移。
// These tests judge the audit module itself: the pending row precedes any change,
// the terminal write is detached from request cancellation, and neither a marshal
// nor a store failure may change the business outcome without leaving a log line.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// auditRecorder 在 `stubOperations` 之上记录回填时观察到的 context 状态——
// 取消是否已被剥离只能在这一层看见。
// auditRecorder records the context state observed at the terminal write, which is
// the only place where cancellation detachment is observable.
type auditRecorder struct {
	stubOperations
	finishCtxErr []error
}

// FinishOperation 记录调用时的 `ctx.Err()` 再委托给内存实现。
// FinishOperation records ctx.Err() before delegating to the in-memory stub.
func (r *auditRecorder) FinishOperation(ctx context.Context, operationID string,
	status core.OperationStatus, resultCode, errorMessage string, detailJSON []byte, now time.Time,
) error {
	r.finishCtxErr = append(r.finishCtxErr, ctx.Err())
	return r.stubOperations.FinishOperation(ctx, operationID, status, resultCode, errorMessage, detailJSON, now)
}

// newAuditWriter 构造固定时钟、固定主语与可读日志缓冲的写法器。
// newAuditWriter builds the writer with a frozen clock and a readable log buffer.
func newAuditWriter(rec *auditRecorder) (operationWriter, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	writer := operationWriter{
		ops:     rec,
		logger:  slog.New(slog.NewTextHandler(buf, nil)),
		now:     func() time.Time { return time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC) },
		subject: subjectCallerOperation,
	}
	return writer, buf
}

// TestAuditWriter_BeginPreWritesPendingRecord 判预写行的形状：操作标识可排序、
// 目标固定为调用方、主体三字段原样落库、状态一律 `pending`、详情是脱敏投影。
// 逆改点：把状态写成 `success`，或把 actor 字段抄错 → 字段断言红。
func TestAuditWriter_BeginPreWritesPendingRecord(t *testing.T) {
	rec := &auditRecorder{}
	writer, buf := newAuditWriter(rec)

	operationID, err := writer.begin(context.Background(), testActor(), actionCallerCreate,
		testCallerID, "sha256:abc", map[string]any{"name": "ops"})
	require.NoError(t, err)
	require.Regexp(t, `^op-\d{13}-[a-z0-9]{6}$`, operationID)

	require.Len(t, rec.begun, 1)
	prewritten := rec.begun[0]
	require.Equal(t, operationID, prewritten.OperationID)
	require.Equal(t, actionCallerCreate, prewritten.Action)
	require.Equal(t, targetKindCaller, prewritten.TargetKind)
	require.Equal(t, testCallerID, prewritten.TargetID)
	require.Equal(t, testActor().Subject, prewritten.ActorSubject)
	require.Equal(t, testActor().Method, prewritten.AuthMethod)
	require.Equal(t, testActor().Source, prewritten.Source)
	require.Equal(t, "sha256:abc", prewritten.RequestHash)
	require.Equal(t, core.OperationPending, prewritten.Status, "预写行必须是 pending")
	require.Equal(t, time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC), prewritten.CreatedAt)

	var detail map[string]any
	require.NoError(t, json.Unmarshal(prewritten.DetailJSON, &detail))
	require.Equal(t, "ops", detail["name"])

	require.Empty(t, rec.finished, "预写不写终态")
	require.Empty(t, buf.String(), "成功路径不得打错误日志")
}

// TestAuditWriter_BeginRefusesWhenStoreFails 判规则 1：预写失败即拒绝执行，
// 由调用方在触碰任何目标之前返回错误；同时必须留 ERROR 日志（审计 + 日志双轨）。
// 逆改点：吞掉 `BeginOperation` 的错误返回空标识 → 错误断言红。
func TestAuditWriter_BeginRefusesWhenStoreFails(t *testing.T) {
	rec := &auditRecorder{stubOperations: stubOperations{beginErr: errors.New("audit store down")}}
	writer, buf := newAuditWriter(rec)

	operationID, err := writer.begin(context.Background(), testActor(), actionCallerCreate,
		testCallerID, "sha256:abc", map[string]any{"name": "ops"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "预写操作记录失败，拒绝执行")
	require.Empty(t, operationID, "预写失败不得给出可回填的操作标识")
	require.Contains(t, buf.String(), "预写记录失败，拒绝执行")
}

// TestAuditWriter_BeginRejectsUnmarshalableDetail 判详情序列化在触碰存储之前完成：
// 组不出来的详情不能先落一行 pending 再报错，否则审计会停在不可能回填的行上。
// 逆改点：把 Marshal 挪到 `BeginOperation` 之后 → 存储收到一行，断言红。
func TestAuditWriter_BeginRejectsUnmarshalableDetail(t *testing.T) {
	rec := &auditRecorder{}
	writer, _ := newAuditWriter(rec)

	operationID, err := writer.begin(context.Background(), testActor(), actionCallerCreate,
		testCallerID, "sha256:abc", map[string]any{"bad": func() {}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "组装")
	require.Empty(t, operationID)
	require.Empty(t, rec.begun, "详情组装失败不得预写")
}

// TestAuditWriter_FinishSurvivesCanceledRequest 判规则 2：客户端断开后终态回填
// 照常落库——停在 `pending` 的审计行会把可解释的失败变成待人工查证的悬案。
// 逆改点：`context.WithoutCancel(ctx)` 换回 `ctx` → 观察到 `context canceled`。
func TestAuditWriter_FinishSurvivesCanceledRequest(t *testing.T) {
	rec := &auditRecorder{}
	writer, _ := newAuditWriter(rec)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer.finish(ctx, "op-1700000000000-abc123", core.OperationSuccess, resultOK, "",
		map[string]any{"name": "ops"})

	require.Len(t, rec.finished, 1, "取消路径仍须回填终态")
	require.Len(t, rec.finishCtxErr, 1)
	require.NoError(t, rec.finishCtxErr[0], "回填必须剥离请求取消")
	require.Equal(t, core.OperationSuccess, rec.finished[0].Status)
}

// TestAuditWriter_FinishKeepsTerminalRowOnUnmarshalableDetail 判详情序列化失败
// 只丢详情、不丢终态，且失败必须留 ERROR 日志。
// 逆改点：序列化失败直接 return → 终态行缺失，断言红。
func TestAuditWriter_FinishKeepsTerminalRowOnUnmarshalableDetail(t *testing.T) {
	rec := &auditRecorder{}
	writer, buf := newAuditWriter(rec)

	writer.finish(context.Background(), "op-1700000000000-abc123", core.OperationFailed,
		"INTERNAL", "store down", map[string]any{"bad": func() {}})

	require.Len(t, rec.finished, 1, "详情失败不得阻断终态回填")
	require.Equal(t, core.OperationFailed, rec.finished[0].Status)
	require.Nil(t, rec.finished[0].Detail)
	require.Contains(t, buf.String(), "详情序列化失败，终态照常回填")
}

// TestAuditWriter_FinishFailureDoesNotPropagate 判规则 3：回填失败不改业务结果
// （函数无返回值、不 panic），但必须留 ERROR 日志交给启动恢复查证。
// 逆改点：在回填失败时 panic 或把错误上抛 → 业务路径被审计故障污染。
func TestAuditWriter_FinishFailureDoesNotPropagate(t *testing.T) {
	rec := &auditRecorder{stubOperations: stubOperations{finishErr: errors.New("audit store down")}}
	writer, buf := newAuditWriter(rec)

	require.NotPanics(t, func() {
		writer.finish(context.Background(), "op-1700000000000-abc123", core.OperationSuccess,
			resultOK, "", map[string]any{"name": "ops"})
	})
	require.Contains(t, buf.String(), "回填终态失败，留待启动恢复查证")
}
