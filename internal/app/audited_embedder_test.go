package app

// 本文件的判据面：查询侧 embedding **先预写 pending 再发调用**（原则 V 的
// fail-closed），调用返回后终态只写一次，且终态写失败不改变调用结果。
// The judged surface: pre-write before the call, one terminal write after it, and a
// failed terminal write that never rewrites the call outcome.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/knowledge"
)

// auditedEmbedderInnerStub 是可计数的内层客户端替身。
// auditedEmbedderInnerStub counts provider calls.
type auditedEmbedderInnerStub struct {
	calls int
	batch knowledge.EmbedBatch
	err   error
}

// Embed 记一次调用并返回预设结果。
func (s *auditedEmbedderInnerStub) Embed(_ context.Context, _ []string) (knowledge.EmbedBatch, error) {
	s.calls++
	return s.batch, s.err
}

// auditBegin 记录一次预写入参。
type auditBegin struct {
	jobID      string
	model      string
	inputCount int
}

// auditFinish 记录一次终态入参。
type auditFinish struct {
	callID       string
	promptTokens int64
	callErr      error
}

// auditedEmbedderAuditStub 是两阶段审计端口的替身，记录调用次序与入参。
// auditedEmbedderAuditStub records the two-phase audit calls.
type auditedEmbedderAuditStub struct {
	beginErr  error
	callID    string
	begins    []auditBegin
	finishErr error
	finishes  []auditFinish
	// finishDeadline / finishBudget 记录终态写收到的上下文是否带截止时间及其剩余
	// 时长——契约 §4.2 要求脱取消的终态写有独立短超时。
	finishDeadline bool
	finishBudget   time.Duration
	// order 记录 "begin" / "embed" / "finish" 的先后，用来判"预写先于调用"。
	order []string
	inner *auditedEmbedderInnerStub
}

// BeginEmbedding 实现预写。
func (s *auditedEmbedderAuditStub) BeginEmbedding(_ context.Context, jobID, model string, inputCount int) (string, error) {
	s.order = append(s.order, "begin")
	s.begins = append(s.begins, auditBegin{jobID: jobID, model: model, inputCount: inputCount})
	if s.beginErr != nil {
		return "", s.beginErr
	}
	return s.callID, nil
}

// FinishEmbedding 实现终态写，并记录收到的截止时间。
func (s *auditedEmbedderAuditStub) FinishEmbedding(ctx context.Context, callID string, promptTokens int64, callErr error) error {
	s.order = append(s.order, "finish")
	if deadline, ok := ctx.Deadline(); ok {
		s.finishDeadline = true
		s.finishBudget = time.Until(deadline)
	}
	s.finishes = append(s.finishes, auditFinish{callID: callID, promptTokens: promptTokens, callErr: callErr})
	return s.finishErr
}

// newAuditedEmbedderForTest 组装一个受测装饰器，并保留两侧替身供断言。
func newAuditedEmbedderForTest(t *testing.T, inner *auditedEmbedderInnerStub,
	audit *auditedEmbedderAuditStub, logs *bytes.Buffer,
) *auditedEmbedder {
	t.Helper()
	return &auditedEmbedder{
		inner:  inner,
		audit:  audit,
		model:  "embed-test",
		logger: slog.New(slog.NewTextHandler(logs, nil)),
	}
}

// TestAuditedEmbedder_PreWriteFailureSkipsProvider 判 fail-closed：预写失败时
// 内层客户端**一次都不被调用**，且错误链里能认出预写失败的原因。
// 逆改点：把 `BeginEmbedding` 的错误改成"只记日志、继续调用" → `calls` 断言红。
func TestAuditedEmbedder_PreWriteFailureSkipsProvider(t *testing.T) {
	writeErr := errors.New("审计库不可写")
	inner := &auditedEmbedderInnerStub{}
	audit := &auditedEmbedderAuditStub{beginErr: writeErr}
	var logs bytes.Buffer
	emb := newAuditedEmbedderForTest(t, inner, audit, &logs)

	_, err := emb.Embed(context.Background(), []string{"查询"})

	require.Error(t, err)
	require.ErrorIs(t, err, writeErr, "预写失败的原因必须留在错误链里")
	require.Zero(t, inner.calls, "预写失败后**不得**发出 embedding 调用（原则 V）")
	require.Empty(t, audit.finishes, "没有调用就不得写终态")
}

// TestAuditedEmbedder_WritesTerminalStateAfterCall 判正常路径的次序与终态内容：
// 预写 → 调用 → 终态，终态带预写返回的行 id 与回执用量，关联字段为空串（查询侧
// 没有摄取作业号，仓储落成 NULL）。
// 逆改点：终态漏写 / 用量写 0 / 伪造 jobID → 对应断言红。
func TestAuditedEmbedder_WritesTerminalStateAfterCall(t *testing.T) {
	inner := &auditedEmbedderInnerStub{batch: knowledge.EmbedBatch{
		Vectors: [][]float32{{0.1, 0.2}}, PromptTokens: 11, Dimensions: 2, Model: "embed-test",
	}}
	audit := &auditedEmbedderAuditStub{callID: "42", inner: inner}
	var logs bytes.Buffer
	emb := newAuditedEmbedderForTest(t, inner, audit, &logs)

	batch, err := emb.Embed(context.Background(), []string{"查询"})

	require.NoError(t, err)
	require.Equal(t, int64(11), batch.PromptTokens, "回执必须原样透传")
	require.Equal(t, []string{"begin", "finish"}, audit.order, "预写必须先于终态")
	require.Len(t, audit.begins, 1)
	require.Equal(t, "", audit.begins[0].jobID, "查询侧没有摄取作业号，不得伪造")
	require.Equal(t, "embed-test", audit.begins[0].model, "模型取装配配置，不从回执反推")
	require.Equal(t, 1, audit.begins[0].inputCount)
	require.Len(t, audit.finishes, 1)
	require.Equal(t, "42", audit.finishes[0].callID)
	require.Equal(t, int64(11), audit.finishes[0].promptTokens)
	require.NoError(t, audit.finishes[0].callErr)
	require.Empty(t, logs.String(), "成功路径不应留下 WARN")
}

// TestAuditedEmbedder_CallErrorReachesTerminalState 判调用失败时终态照写、失败
// 原因进审计（行不会停在 pending），用量按"未知"记 0，且错误原样上抛。
// 逆改点：失败时直接 return 不写终态 → `finishes` 空断言红。
func TestAuditedEmbedder_CallErrorReachesTerminalState(t *testing.T) {
	callErr := errors.New("提供方超时")
	inner := &auditedEmbedderInnerStub{err: callErr}
	audit := &auditedEmbedderAuditStub{callID: "7"}
	var logs bytes.Buffer
	emb := newAuditedEmbedderForTest(t, inner, audit, &logs)

	_, err := emb.Embed(context.Background(), []string{"查询"})

	require.ErrorIs(t, err, callErr)
	require.Len(t, audit.finishes, 1, "调用已发出，终态必须写")
	require.ErrorIs(t, audit.finishes[0].callErr, callErr, "失败原因必须落审计")
	require.Zero(t, audit.finishes[0].promptTokens, "失败时用量未知，写 0 而不是编一个数")
}

// TestAuditedEmbedder_TerminalWriteHasIndependentDeadline 判终态写带独立短超时
// （契约 §4.2）：脱了取消的写也必须有害界——连接池卡死时没有它，一次已经完成的
// 调用会永久停在"在途"形态，调用方也永远回不来。
//
// 逆改点：把 `finish` 里的 `context.WithTimeout` 改回裸 `WithoutCancel` →
// `finishDeadline` 断言红；把上限改到分钟级 → 上界断言红。
func TestAuditedEmbedder_TerminalWriteHasIndependentDeadline(t *testing.T) {
	inner := &auditedEmbedderInnerStub{batch: knowledge.EmbedBatch{
		Vectors: [][]float32{{0.1, 0.2}}, PromptTokens: 5, Dimensions: 2, Model: "embed-test",
	}}
	audit := &auditedEmbedderAuditStub{callID: "13"}
	var logs bytes.Buffer
	emb := newAuditedEmbedderForTest(t, inner, audit, &logs)

	_, err := emb.Embed(context.Background(), []string{"查询"})
	require.NoError(t, err)

	require.Len(t, audit.finishes, 1)
	require.True(t, audit.finishDeadline, "终态写必须带独立截止时间")
	require.Positive(t, audit.finishBudget, "剩余时长必须为正")
	require.LessOrEqual(t, audit.finishBudget, auditFinishTimeout, "超时不得超过声明的上界")
}

// TestAuditedEmbedder_TerminalWriteFailureKeepsResult 判终态写失败只 WARN、不改写
// 调用结果：调用已经发生，把审计回填失败报成 embedding 失败会让上游误判"没花钱"。
// 逆改点：把 `finish` 的错误返回给调用方 → 本用例红（同时会误伤降级判定）。
func TestAuditedEmbedder_TerminalWriteFailureKeepsResult(t *testing.T) {
	inner := &auditedEmbedderInnerStub{batch: knowledge.EmbedBatch{
		Vectors: [][]float32{{0.1, 0.2}}, PromptTokens: 3, Dimensions: 2, Model: "embed-test",
	}}
	audit := &auditedEmbedderAuditStub{callID: "9", finishErr: errors.New("写库失败")}
	var logs bytes.Buffer
	emb := newAuditedEmbedderForTest(t, inner, audit, &logs)

	batch, err := emb.Embed(context.Background(), []string{"查询"})

	require.NoError(t, err, "审计回填失败不得改写调用结果")
	require.Equal(t, int64(3), batch.PromptTokens)
	require.Contains(t, logs.String(), "终态写入失败", "失败路径必须留下 WARN（可观测性双轨）")
}
