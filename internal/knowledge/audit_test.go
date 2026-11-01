package knowledge

// 本文件覆盖 T036a 的操作审计：谁 / 何时 / 对哪个来源 / 结果如何四个事实，
// 双轨（权威行 + WARN 日志）、终态写失败升级 ERROR、取消不阻断终态回填，
// 以及"零值身份不产生假身份字段"。日志面用 JSON handler 捕获后按字段断言，
// 不耦合文本格式。
//
// This file covers the operation audit: the four facts, the two-track logging,
// ERROR on terminal-write failure, cancellation detachment and the rule that a
// rejected zero-value principal never looks like a real caller.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// captureLogger 返回写进缓冲区的 JSON 日志面；字段断言按解析后的对象做。
// captureLogger returns a JSON log surface for field-level assertions.
func captureLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

// logEntries 解析缓冲区里指定消息的全部日志行（一条记录一行）。
// logEntries parses every line whose message matches.
func logEntries(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var matched []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), "log line is not JSON: %s", line)
		if record["msg"] == msg {
			matched = append(matched, record)
		}
	}
	return matched
}

// requireLogEntry 断言某条消息恰好出现一次并返回其字段。
// requireLogEntry asserts exactly one line with the message and returns its fields.
func requireLogEntry(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	found := logEntries(t, buf, msg)
	require.Len(t, found, 1, "message %q should appear exactly once; log was:\n%s", msg, buf.String())
	return found[0]
}

// fixedClock 返回固定的测试时钟。
// fixedClock returns one fixed instant.
func fixedClock() func() time.Time {
	return func() time.Time { return time.UnixMilli(1_700_000_000_000).UTC() }
}

// TestOperationAudit_RejectsZeroPrincipal 守"身份先于动作"：零值身份开不了轨迹，
// 错误是 `ErrUnauthorized`，调用方据此拒绝且不写任何东西。
// TestOperationAudit_RejectsZeroPrincipal refuses to open a trace for zero identity.
func TestOperationAudit_RejectsZeroPrincipal(t *testing.T) {
	audit := newOperationAudit(testLogger(t), fixedClock())

	trace, err := audit.begin(core.Principal{}, opIngest, fixedClock()())
	require.ErrorIs(t, err, core.ErrUnauthorized)
	assert.Nil(t, trace)
}

// TestOperationAudit_RejectsUnknownOperation 守操作名闭集：未登记的名称是编程错误，
// 当场拒绝而不是落一条聚合不了的日志。
// TestOperationAudit_RejectsUnknownOperation refuses unregistered operation names.
func TestOperationAudit_RejectsUnknownOperation(t *testing.T) {
	audit := newOperationAudit(testLogger(t), fixedClock())

	trace, err := audit.begin(testPrincipal(), operationKind("purge"), fixedClock()())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "purge")
	assert.Nil(t, trace)
}

// TestOperationAudit_ReasonCodes 守 `reason_code` 的稳定性：日志聚合与排障按码分组，
// 未知错误一律 `operation_failed`（**不得**透出错误文本）。
// TestOperationAudit_ReasonCodes pins the stable reason codes.
func TestOperationAudit_ReasonCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "core unauthorized", err: core.ErrUnauthorized, want: "unauthorized"},
		{name: "core forbidden", err: core.ErrForbidden, want: "forbidden"},
		{name: "quota", err: ErrQuotaExhausted, want: "quota_exhausted"},
		{name: "rate limited", err: ErrRateLimited, want: "rate_limited"},
		{name: "concurrency", err: ErrConcurrencyLimited, want: "concurrency_limited"},
		{name: "unavailable", err: ErrIngestUnavailable, want: "unavailable"},
		{name: "path rejected", err: ErrPathRejected, want: "path_rejected"},
		{name: "not found", err: ErrNotFound, want: "not_found"},
		{name: "wrapped not found", err: wrapNotFound(), want: "not_found"},
		{name: "invalid request", err: ErrInvalidRequest, want: "invalid_request"},
		{name: "unknown", err: errors.New("boom"), want: "operation_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, reasonCode(tc.err))
		})
	}
}

// wrapNotFound 返回一个包装过的 `ErrNotFound`，守 errors.Is 而非字面比较。
// wrapNotFound exercises errors.Is instead of literal comparison.
func wrapNotFound() error {
	return errors.Join(ErrNotFound, errors.New("detail"))
}

// TestOperationAudit_SuccessCarriesFourFacts 守 FR-007 的四个事实在成功路径齐全：
// 谁（caller / key / actor）、何时（duration_ms）、哪个来源（集合 / 路径 / 文档 /
// 版本 / 作业）、结果如何（succeeded + 权威行引用）。
// TestOperationAudit_SuccessCarriesFourFacts pins all four facts on success.
func TestOperationAudit_SuccessCarriesFourFacts(t *testing.T) {
	h := newHarness(t)
	logger, buf := captureLogger(t)
	h.logger = logger

	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)
	require.Equal(t, VersionActive, result.Status)

	record := requireLogEntry(t, buf, "knowledge operation succeeded")
	assert.Equal(t, "INFO", record["level"])
	assert.Equal(t, "ingest", record["operation"])
	assert.Equal(t, "succeeded", record["result"])
	assert.Equal(t, "caller", record["principal_kind"])
	assert.Equal(t, "caller_a", record["caller_id"])
	assert.Equal(t, "key_a1", record["key_id"])
	assert.Equal(t, "caller", record["actor"])
	assert.Equal(t, "col_1", record["collection_id"])
	assert.Equal(t, "docs/a.md", record["source_path"])
	assert.Equal(t, "doc_1", record["document_id"])
	assert.Equal(t, "ver_1", record["version_id"])
	assert.Equal(t, "job_1", record["ingest_job_id"])
	assert.Equal(t, "kb_ingest_jobs", record["audit_table"])
	assert.Equal(t, "job_1", record["audit_id"])
	duration, ok := record["duration_ms"].(float64)
	require.True(t, ok, "duration_ms should be a number, got %T", record["duration_ms"])
	assert.GreaterOrEqual(t, duration, 0.0)
}

// TestOperationAudit_FailureWritesAuditAndLog 守双轨：作业期内失败既写 WARN 日志
// （带阶段与 error_kind），也把版本行与作业行写成失败终态。
// TestOperationAudit_FailureWritesAuditAndLog requires both tracks on failure.
func TestOperationAudit_FailureWritesAuditAndLog(t *testing.T) {
	h := newHarness(t)
	logger, buf := captureLogger(t)
	h.logger = logger
	h.embedder.err = errors.New("provider timeout")

	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)
	require.Equal(t, VersionFailed, result.Status)

	record := requireLogEntry(t, buf, "knowledge operation failed")
	assert.Equal(t, "WARN", record["level"])
	assert.Equal(t, "ingest", record["operation"])
	assert.Equal(t, "failed", record["result"])
	assert.Equal(t, "embed", record["stage"])
	assert.Equal(t, "embed_failed", record["error_kind"])
	assert.Equal(t, "caller_a", record["caller_id"])
	assert.Equal(t, "docs/a.md", record["source_path"])
	assert.Equal(t, "kb_ingest_jobs", record["audit_table"])
	assert.Equal(t, "job_1", record["audit_id"])
	assert.Contains(t, record["error"], "provider timeout")

	// 另一轨：版本与作业的失败终态确实落库。
	assert.Equal(t, []string{ErrorKindEmbedFailed}, h.store.failReasons)
	assert.Equal(t, "embed/failed/embed_failed", h.store.jobOutcomes[0])
}

// TestOperationAudit_TerminalWriteFailureIsError 守工程规范 §4：权威行终态写失败
// 升级为 ERROR（需要人介入），业务结果**不**被反转。
// TestOperationAudit_TerminalWriteFailureIsError escalates a lost terminal write.
func TestOperationAudit_TerminalWriteFailureIsError(t *testing.T) {
	t.Run("success path keeps the active result", func(t *testing.T) {
		h := newHarness(t)
		logger, buf := captureLogger(t)
		h.logger = logger
		h.store.finishJob = func(context.Context, string, string, string, string, time.Time) error {
			return errors.New("job update failed")
		}

		result, err := h.build().Ingest(t.Context(), h.request())
		require.NoError(t, err)
		assert.Equal(t, VersionActive, result.Status)

		record := requireLogEntry(t, buf, "knowledge operation terminal write failed")
		assert.Equal(t, "ERROR", record["level"])
		assert.Equal(t, "job_1", record["audit_id"])
		assert.Contains(t, record["error"], "job update failed")
	})

	t.Run("failure path reports both lost rows", func(t *testing.T) {
		h := newHarness(t)
		logger, buf := captureLogger(t)
		h.logger = logger
		h.embedder.err = errors.New("provider timeout")
		h.store.failVersion = func(context.Context, string, string, time.Time) error {
			return errors.New("version update failed")
		}
		h.store.finishJob = func(context.Context, string, string, string, string, time.Time) error {
			return errors.New("job update failed")
		}

		result, err := h.build().Ingest(t.Context(), h.request())
		require.NoError(t, err)
		assert.Equal(t, VersionFailed, result.Status)

		record := requireLogEntry(t, buf, "knowledge operation terminal write failed")
		assert.Equal(t, "ERROR", record["level"])
		assert.Contains(t, record["error"], "fail version ver_1")
		assert.Contains(t, record["error"], "finish job job_1")
	})
}

// TestOperationAudit_DetachedFromCancellation 守"取消不得留下半截状态"：终态回填
// 拿到的上下文已经脱离取消，请求被取消也不影响写终态。
// TestOperationAudit_DetachedFromCancellation pins the detached terminal write.
func TestOperationAudit_DetachedFromCancellation(t *testing.T) {
	audit := newOperationAudit(testLogger(t), fixedClock())
	trace, err := audit.begin(testPrincipal(), opRetrieve, fixedClock()())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, ctx.Err())

	called := false
	var seen error
	trace.succeed(ctx, auditRef{table: "context_assembly_records", id: "asm_1"},
		func(persistCtx context.Context) error {
			called = true
			seen = persistCtx.Err()
			return nil
		})

	require.True(t, called, "terminal write must still run after cancellation")
	assert.NoError(t, seen, "terminal write must not inherit the cancelled context")
}

// TestOperationAudit_TerminalWriteIsBounded 守终态写的另一半（契约 §4.2）：脱取消
// 是为了不丢终态，独立短超时是为了不挂死——连接池卡死时没有它，一次已经发生的操作
// 会永久停在半截状态。
// 逆改点：把 `persistTerminal` 的 `context.WithTimeout` 改回裸 `WithoutCancel` →
// deadline 断言红；把上限改到分钟级 → 上界断言红。
func TestOperationAudit_TerminalWriteIsBounded(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	err := persistTerminal(t.Context(), func(ctx context.Context) error {
		deadline, hasDeadline = ctx.Deadline()
		return nil
	})
	require.NoError(t, err)
	require.True(t, hasDeadline, "脱取消的终态写必须带独立截止时间")
	assert.Positive(t, time.Until(deadline), "剩余时长必须为正")
	assert.LessOrEqual(t, time.Until(deadline), detachedWriteTimeout, "超时不得超过声明的上界")
}

// TestOperationAudit_SuccessWithoutAuditRecordIsFlagged 守"成功也要能对号"：没有
// 权威行引用的成功降级为 WARN，排障时不会以为证据齐全。
// TestOperationAudit_SuccessWithoutAuditRecordIsFlagged warns when no row exists.
func TestOperationAudit_SuccessWithoutAuditRecordIsFlagged(t *testing.T) {
	audit := newOperationAudit(testLogger(t), fixedClock())
	logger, buf := captureLogger(t)
	audit.logger = logger
	trace, err := audit.begin(testPrincipal(), opSourceView, fixedClock()())
	require.NoError(t, err)

	trace.succeed(t.Context(), auditRef{}, nil)

	record := requireLogEntry(t, buf, "knowledge operation succeeded without an audit record")
	assert.Equal(t, "WARN", record["level"])
	assert.Equal(t, "source_view", record["operation"])
	assert.NotContains(t, record, "audit_table")
}

// TestIngest_AdmissionRejectionIsLogged 守准入期拒绝的留痕：路径被拒时零读取、
// 零写入，但有 WARN 日志（含来源路径与 reason_code）。
// TestIngest_AdmissionRejectionIsLogged keeps admission rejections observable.
func TestIngest_AdmissionRejectionIsLogged(t *testing.T) {
	h := newHarness(t)
	logger, buf := captureLogger(t)
	h.logger = logger
	req := h.request()
	req.Path = "docs/a.pdf"

	_, err := h.build().Ingest(t.Context(), req)
	require.ErrorIs(t, err, ErrPathRejected)

	record := requireLogEntry(t, buf, "knowledge operation rejected")
	assert.Equal(t, "WARN", record["level"])
	assert.Equal(t, "ingest", record["operation"])
	assert.Equal(t, "rejected", record["result"])
	assert.Equal(t, "path_rejected", record["reason_code"])
	assert.Equal(t, "caller_a", record["caller_id"])
	assert.Equal(t, "col_1", record["collection_id"])
	assert.Equal(t, "docs/a.pdf", record["source_path"])
	assert.NotContains(t, record, "audit_table")

	assert.Empty(t, h.reader.paths)
	assert.Empty(t, h.events)
	assert.Empty(t, h.quota.admits)
}

// TestIngest_ForbiddenIsRejectedAndLogged 守 scope 拒绝同样留痕，且身份字段齐全
// （这是合法身份，只是没有写权限）。
// TestIngest_ForbiddenIsRejectedAndLogged logs the scope rejection with identity.
func TestIngest_ForbiddenIsRejectedAndLogged(t *testing.T) {
	h := newHarness(t)
	logger, buf := captureLogger(t)
	h.logger = logger
	req := h.request()
	req.Principal.Scopes = []string{core.ScopeKnowledgeRead}

	_, err := h.build().Ingest(t.Context(), req)
	require.ErrorIs(t, err, core.ErrForbidden)

	record := requireLogEntry(t, buf, "knowledge operation rejected")
	assert.Equal(t, "forbidden", record["reason_code"])
	assert.Equal(t, "caller_a", record["caller_id"])
	assert.Equal(t, "key_a1", record["key_id"])
}

// TestIngest_ZeroPrincipalLogsNoIdentity 守"零值身份不产生假身份字段"：拒绝日志里
// 没有 caller_id / principal_kind，读日志的人不会把它当成某个真实调用方。
// TestIngest_ZeroPrincipalLogsNoIdentity never fabricates identity fields.
func TestIngest_ZeroPrincipalLogsNoIdentity(t *testing.T) {
	h := newHarness(t)
	logger, buf := captureLogger(t)
	h.logger = logger
	req := h.request()
	req.Principal = core.Principal{}

	_, err := h.build().Ingest(t.Context(), req)
	require.ErrorIs(t, err, core.ErrUnauthorized)

	record := requireLogEntry(t, buf, "knowledge operation rejected")
	assert.Equal(t, "unauthorized", record["reason_code"])
	assert.NotContains(t, record, "caller_id")
	assert.NotContains(t, record, "key_id")
	assert.NotContains(t, record, "principal_kind")
}

// TestIngest_DedupLogsVersionAsAuditRef 守去重命中的权威行：本次没有作业，引用指向
// 命中的版本行，而不是编一个作业号。
// TestIngest_DedupLogsVersionAsAuditRef points at the deduplicated version row.
func TestIngest_DedupLogsVersionAsAuditRef(t *testing.T) {
	h := newHarness(t)
	logger, buf := captureLogger(t)
	h.logger = logger
	h.store.findActive = func(context.Context, string, string) (VersionRecord, bool, error) {
		return VersionRecord{
			DocumentID: "doc_9", VersionID: "ver_9", Status: VersionActive,
			ChunkCount: 12, Generation: 1,
		}, true, nil
	}

	result, err := h.build().Ingest(t.Context(), h.request())
	require.NoError(t, err)
	require.True(t, result.Deduplicated)

	record := requireLogEntry(t, buf, "knowledge operation succeeded")
	assert.Equal(t, "kb_document_versions", record["audit_table"])
	assert.Equal(t, "ver_9", record["audit_id"])
	assert.Equal(t, "ver_9", record["version_id"])
	assert.NotContains(t, record, "ingest_job_id")
}

// TestDelete_OperationIsAudited 守删除的留痕：成功时权威行就是软删后的那一行，
// 失败时落 WARN（沿用原错误返回）。
// TestDelete_OperationIsAudited pins the delete audit on both outcomes.
func TestDelete_OperationIsAudited(t *testing.T) {
	t.Run("success names the soft-deleted row", func(t *testing.T) {
		h := newHarness(t)
		logger, buf := captureLogger(t)
		h.logger = logger

		require.NoError(t, h.build().Delete(t.Context(), DeleteRequest{
			Principal: testPrincipal(), DocumentID: "doc_1",
		}))

		record := requireLogEntry(t, buf, "knowledge operation succeeded")
		assert.Equal(t, "delete", record["operation"])
		assert.Equal(t, "kb_documents", record["audit_table"])
		assert.Equal(t, "doc_1", record["audit_id"])
		assert.Equal(t, "doc_1", record["document_id"])
	})

	t.Run("failure is logged with the cause", func(t *testing.T) {
		h := newHarness(t)
		logger, buf := captureLogger(t)
		h.logger = logger
		h.store.deleteColl = func(context.Context, string, time.Time) error {
			return errors.New("collection delete failed")
		}

		err := h.build().Delete(t.Context(), DeleteRequest{
			Principal: testPrincipal(), CollectionID: "col_1",
		})
		require.Error(t, err)

		record := requireLogEntry(t, buf, "knowledge operation failed")
		assert.Equal(t, "delete", record["operation"])
		assert.Equal(t, "operation_failed", record["reason_code"])
		assert.Equal(t, "col_1", record["collection_id"])
		assert.Contains(t, record["error"], "collection delete failed")
	})
}

// TestReindex_OperationIsAudited 守重建的轨迹：操作名保持 reindex（不被共用的摄取
// 链路改成 ingest），真实重建应关联新作业。
// TestReindex_OperationIsAudited keeps the reindex label through the shared path.
func TestReindex_OperationIsAudited(t *testing.T) {
	h := newHarness(t)
	logger, buf := captureLogger(t)
	h.logger = logger
	h.store.document = func(context.Context, string) (DocumentRecord, error) {
		return DocumentRecord{
			DocumentID: "doc_1", CollectionID: "col_7", SourcePath: "docs/b.txt",
			ContentHash: sha256Hex("原始字节"),
		}, nil
	}
	// 读取器回显它实际加载的路径（生产实现把请求路径投影成真实路径后返回）。
	h.reader.src.Path = "docs/b.txt"
	h.store.findActive = func(context.Context, string, string) (VersionRecord, bool, error) {
		return VersionRecord{
			DocumentID: "doc_1", VersionID: "ver_7", Status: VersionActive,
			ChunkCount: 3, Generation: 1,
		}, true, nil
	}

	result, err := h.build().Reindex(t.Context(), ReindexRequest{
		Principal: testPrincipal(), DocumentID: "doc_1",
	})
	require.NoError(t, err)
	require.False(t, result.Deduplicated)
	require.NotEmpty(t, result.IngestJobID)

	record := requireLogEntry(t, buf, "knowledge operation succeeded")
	assert.Equal(t, "reindex", record["operation"])
	assert.Equal(t, "kb_ingest_jobs", record["audit_table"])
	assert.Equal(t, result.IngestJobID, record["audit_id"])
	assert.Equal(t, "docs/b.txt", record["source_path"])
}

// TestOperationAudit_SanitizesExternalFields 守日志注入防线：外部字符串里的换行、
// 控制字符与超长值不得原样进日志。
// TestOperationAudit_SanitizesExternalFields strips control characters and caps length.
func TestOperationAudit_SanitizesExternalFields(t *testing.T) {
	assert.Equal(t, "docs/a.md", sanitizeField("docs/a\r\n.md\x00"))
	assert.Equal(t, "docs/bold.md", sanitizeField("docs/b\x1bold.md"))

	long := strings.Repeat("x", 4096)
	cleaned := sanitizeField(long)
	assert.Len(t, cleaned, 256+len("..."))
	assert.True(t, strings.HasSuffix(cleaned, "..."))
}
