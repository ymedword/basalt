package app

// 本文件锁死 V-8 补齐的生产端（US-16 颗粒度文档 §2.9）：运行路径的 `llm_call`、
// 执行器的 `tool_call` / `permission_decision`、以及配额判定的 `quota_decision`。
// 每个用例都写成"实现改了就红"的形态，逐条给出逆改点。
//
// 三条共同口径：
//   - 事件经**真实** `diagnostics.Recorder` 落库前必须过 `Validate()`——形状非法的事件
//     会在这里红，而不是在生产里被静默丢弃；
//   - 锚点缺失**不伪造**（§2.9 逐字）：没有 `run_id` 的调用一律零事件；
//   - 诊断不改变业务结论：错误链、配额判定与返回值逐字保持。
//
// This file pins the V-8 producers: the run-path llm_call, the executor's tool_call and
// permission_decision, and quota_decision. Each case names its mutation point.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// producerRecorder 用真实写入器组一个诊断端口，返回捕获面。
// producerRecorder wires the real recorder and returns the capture surface.
func producerRecorder(t *testing.T) (*diagnostics.Recorder, *adapterEventStore) {
	t.Helper()
	cfg, err := diagnostics.NewConfig(0, 1)
	require.NoError(t, err)
	store := &adapterEventStore{}
	recorder := diagnostics.NewRecorder(diagnostics.RecorderOptions{
		Store:    store,
		Sampler:  diagnostics.NewRateSampler(cfg),
		Redactor: diagnostics.NewWhitelistRedactor(),
		Now:      func() time.Time { return adapterNow },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return recorder, store
}

// stubProviderClient 固定返回一条响应或一条错误。
// stubProviderClient returns one fixed reply or one fixed error.
type stubProviderClient struct {
	resp core.ChatResponse
	err  error
}

func (c *stubProviderClient) Chat(context.Context, core.ChatRequest) (core.ChatResponse, error) {
	return c.resp, c.err
}

// TestDiagnosticProviderClient_LLMCallFailureCarriesReason 守失败事件（V-8 的
// 用户裁定补齐项之一）：调用发出后失败带 `llm_call_failed`，`decision` 留空。
//
// 逆改点：失败分支不发事件 / 不发原因码 → 前两条断言红；把失败写成 `deny` →
// "决定留空"断言红（模型调用成败不是策略判定）。
func TestDiagnosticProviderClient_LLMCallFailureCarriesReason(t *testing.T) {
	recorder, store := producerRecorder(t)
	boom := errors.New("供应商超时")
	client := newDiagnosticProviderClient(&stubProviderClient{err: boom}, recorder)

	_, err := client.Chat(t.Context(), core.ChatRequest{RunID: "run_1", Round: 2})
	require.ErrorIs(t, err, boom, "诊断不得改写错误链")

	require.Len(t, store.events, 1)
	ev := store.events[0]
	require.Equal(t, diagnostics.KindLLMCall, ev.Kind)
	require.Equal(t, "run_1", ev.Correlation.RunID)
	require.Equal(t, "2", ev.StepID)
	require.Equal(t, chatAttemptID, ev.AttemptID)
	require.Equal(t, int64(2), ev.Seq)
	require.Equal(t, diagnostics.ReasonLLMCallFailed, ev.ReasonCode)
	require.Empty(t, ev.Decision, "模型调用成败不是一次策略判定")
}

// TestDiagnosticProviderClient_LLMCallSuccessHasNoReason 守成功事件：同一锚点、不带
// 原因码（原因码回答"为什么失败"，成功没有可回答的）。
//
// 逆改点：成功不发事件 → 长度断言红；给成功也盖 `llm_call_failed` → 原因码断言红。
func TestDiagnosticProviderClient_LLMCallSuccessHasNoReason(t *testing.T) {
	recorder, store := producerRecorder(t)
	client := newDiagnosticProviderClient(&stubProviderClient{
		resp: core.ChatResponse{Content: "好"},
	}, recorder)

	resp, err := client.Chat(t.Context(), core.ChatRequest{RunID: "run_1", Round: 1})
	require.NoError(t, err)
	require.Equal(t, "好", resp.Content, "响应必须原样透传")

	require.Len(t, store.events, 1)
	require.Equal(t, diagnostics.KindLLMCall, store.events[0].Kind)
	require.Empty(t, store.events[0].ReasonCode, "成功事件不带原因码")
}

// TestDiagnosticProviderClient_NeverSentFailuresEmitNoEvent 守 `llm_call_failed`
// 的语义边界（§2.9.1）：审计预写失败、路由查表不中、凭据撤销 / 不可读这四类
// **确定未发出**的失败不发事件——该码只回答"发出后没拿到可用响应"，把它们记成
// 模型失败会把装配 / 凭据 / 审计故障误导成供应商故障。轮号 < 1 同样不发。
//
// 逆改点：去掉 `callNeverIssued` 分支 → 四个子用例各自多出一条事件，长度断言红；
// 只认前两个哨兵 → 凭据两个子用例红。
func TestDiagnosticProviderClient_NeverSentFailuresEmitNoEvent(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "audit_write", err: core.ErrAuditWrite},
		{name: "route_miss", err: core.ErrProviderNotConfigured},
		{name: "credential_revoked", err: core.ErrCredentialRevoked},
		{name: "credential_unavailable", err: core.ErrCredentialUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder, store := producerRecorder(t)
			client := newDiagnosticProviderClient(&stubProviderClient{err: tc.err}, recorder)

			_, err := client.Chat(t.Context(), core.ChatRequest{RunID: "run_1", Round: 1})
			require.ErrorIs(t, err, tc.err, "诊断不得改写错误链")
			require.Empty(t, store.events, "确定未发出的失败不记 llm_call 事件")
		})
	}

	recorder, store := producerRecorder(t)
	client := newDiagnosticProviderClient(&stubProviderClient{err: errors.New("轮号非法")}, recorder)
	_, _ = client.Chat(t.Context(), core.ChatRequest{RunID: "run_1", Round: 0})
	require.Empty(t, store.events, "轮号 < 1 的调用在预写前就被拒绝，同样不记")
}

// TestDiagnosticProviderClient_SkipsWithoutRunID 守"锚点不伪造"：没有 `run_id`
// 的调用（轻装配 / 单测直调）零事件。
//
// 逆改点：去掉 `RunID == ""` 的分支 → 空锚点事件在 `Validate()` 处红。
func TestDiagnosticProviderClient_SkipsWithoutRunID(t *testing.T) {
	recorder, store := producerRecorder(t)
	client := newDiagnosticProviderClient(&stubProviderClient{err: errors.New("x")}, recorder)

	_, _ = client.Chat(t.Context(), core.ChatRequest{Round: 1})
	require.Empty(t, store.events)
}

// TestDiagnosticProviderClient_NilRecorderKeepsInnerClient 守零成本旁路：诊断未启用
// 时装配层原样交回内层客户端，不做任何包装。
//
// 逆改点：无 recorder 也包一层 → 类型断言红。
func TestDiagnosticProviderClient_NilRecorderKeepsInnerClient(t *testing.T) {
	inner := &stubProviderClient{}
	require.Same(t, inner, newDiagnosticProviderClient(inner, nil))
}

// TestToolDiagAdapter_CallCarriesAuditReference 守 `tool_call` 的关联面：审计行存在
// 时带 `audit_ref`（表封闭集 `tool_invocations`）、工具名与失败分类进 attrs。
//
// 逆改点：不写 `audit_ref` → 引用断言红；把 error_kind 写进共用集键 → 目前仍
// 放行（白名单按 kind 收窄的判卷在 `internal/diagnostics` 的脱敏用例里）。
func TestToolDiagAdapter_CallCarriesAuditReference(t *testing.T) {
	recorder, store := producerRecorder(t)
	adapter := newToolDiagAdapter(recorder, nil)

	adapter.RecordToolDiagnostic(t.Context(), core.ToolDiagnostic{
		Kind:      core.ToolDiagnosticCall,
		RunID:     "run_9",
		StepID:    "3",
		Seq:       3,
		Tool:      "fs.read",
		ErrorKind: core.ErrorKindInvalidInput,
		AuditID:   42,
	})

	require.Len(t, store.events, 1)
	ev := store.events[0]
	require.Equal(t, diagnostics.KindToolCall, ev.Kind)
	require.Equal(t, "run_9", ev.Correlation.RunID)
	require.Equal(t, "3", ev.StepID)
	require.Equal(t, int64(3), ev.Seq)
	require.Equal(t, "fs.read", ev.Attrs["tool"])
	require.Equal(t, string(core.ErrorKindInvalidInput), ev.Attrs["error_kind"])
	require.NotNil(t, ev.AuditRef, "有审计行的调用必须带引用")
	require.Equal(t, "tool_invocations", ev.AuditRef.Table)
	require.Equal(t, "42", ev.AuditRef.ID)
	require.Empty(t, ev.Decision, "工具调用不是策略判定")
}

// TestToolDiagAdapter_CallWithoutAuditRowHasNoReference 守"不伪造引用"：解析失败与
// 未注册工具没有审计行（AuditID 为 0），事件照发但不带 `audit_ref`。
//
// 逆改点：无条件写引用 → 引用断言从 nil 变非 nil 红。
func TestToolDiagAdapter_CallWithoutAuditRowHasNoReference(t *testing.T) {
	recorder, store := producerRecorder(t)
	adapter := newToolDiagAdapter(recorder, nil)

	adapter.RecordToolDiagnostic(t.Context(), core.ToolDiagnostic{
		Kind:      core.ToolDiagnosticCall,
		RunID:     "run_9",
		Tool:      "missing",
		ErrorKind: core.ErrorKindNotFound,
	})

	require.Len(t, store.events, 1)
	require.Nil(t, store.events[0].AuditRef, "没有审计行就没有引用")
}

// TestToolDiagAdapter_DeniedRecordsPermissionDecision 守拒绝路径：工具集外的调用写
// `permission_decision` + `deny` + `permission_denied`，且**不**带审计引用（执行从未
// 启动，没有行可引用）。
//
// 逆改点：拒绝写成 `tool_call` → kind 断言红；补一个假的审计引用 → nil 断言红。
func TestToolDiagAdapter_DeniedRecordsPermissionDecision(t *testing.T) {
	recorder, store := producerRecorder(t)
	adapter := newToolDiagAdapter(recorder, nil)

	adapter.RecordToolDiagnostic(t.Context(), core.ToolDiagnostic{
		Kind:  core.ToolDiagnosticDenied,
		RunID: "run_9",
		Tool:  "shell.exec",
	})

	require.Len(t, store.events, 1)
	ev := store.events[0]
	require.Equal(t, diagnostics.KindPermissionDecision, ev.Kind)
	require.Equal(t, diagnostics.DecisionDeny, ev.Decision)
	require.Equal(t, diagnostics.ReasonPermissionDenied, ev.ReasonCode)
	require.Equal(t, "shell.exec", ev.Attrs["tool"])
	require.Nil(t, ev.AuditRef)
}

// TestToolDiagAdapter_DropsUnknownShapeAndMissingAnchor 守两条丢弃路径：未知形态
// （编程错误）与缺锚点都不落库。
//
// 逆改点：未知形态兜底成 `tool_call` → 第一条断言红。
func TestToolDiagAdapter_DropsUnknownShapeAndMissingAnchor(t *testing.T) {
	recorder, store := producerRecorder(t)
	adapter := newToolDiagAdapter(recorder, nil)

	adapter.RecordToolDiagnostic(t.Context(), core.ToolDiagnostic{
		Kind: core.ToolDiagnosticKind("bogus"), RunID: "run_9",
	})
	adapter.RecordToolDiagnostic(t.Context(), core.ToolDiagnostic{
		Kind: core.ToolDiagnosticCall, Tool: "fs.read",
	})
	require.Empty(t, store.events)
}

// TestQuotaDecision_SplitsVerdictFromReason 守 `quota_decision` 的公共翻译：放行不带
// 原因码，三种拒绝各自映射契约码，未知错误留空，缺锚点不写。
//
// 逆改点：放行也盖原因码 → 首个子用例红；把并发名额并进 quota_exhausted → 第三条
// 子用例红（两者的恢复条件不同，排障方向不同）。
func TestQuotaDecision_SplitsVerdictFromReason(t *testing.T) {
	recorder, store := producerRecorder(t)
	cases := []struct {
		name   string
		err    error
		want   diagnostics.ReasonCode
		denied bool
	}{
		{name: "allow", err: nil, want: "", denied: false},
		{name: "quota", err: callers.ErrQuotaExhausted, want: diagnostics.ReasonQuotaExhausted, denied: true},
		{name: "concurrency", err: callers.ErrConcurrencyLimited, want: diagnostics.ReasonConcurrencyLimited, denied: true},
		{name: "rate", err: callers.ErrRateLimited, want: diagnostics.ReasonRateLimited, denied: true},
		{name: "unknown", err: errors.New("存储抖动"), want: "", denied: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(store.events)
			recordQuotaDecision(t.Context(), recorder, quotaDecision{
				RunID: "run_7", StepID: "1", Seq: 1, Class: "chat", Err: tc.err,
			})
			require.Len(t, store.events, before+1)
			ev := store.events[len(store.events)-1]
			require.Equal(t, diagnostics.KindQuotaDecision, ev.Kind)
			require.Equal(t, "run_7", ev.Correlation.RunID)
			require.Equal(t, "chat", ev.Attrs["class"])
			require.Equal(t, tc.want, ev.ReasonCode)
			if tc.denied {
				require.Equal(t, diagnostics.DecisionDeny, ev.Decision)
			} else {
				require.Equal(t, diagnostics.DecisionAllow, ev.Decision)
			}
		})
	}

	before := len(store.events)
	recordQuotaDecision(t.Context(), recorder, quotaDecision{Class: "chat"})
	recordQuotaDecision(t.Context(), nil, quotaDecision{RunID: "run_7"})
	require.Len(t, store.events, before, "缺锚点或缺端口一律零事件")
}

// TestQuotaDecision_RequestAnchorFallback 守 `request_id` 退路锚点（A-F2 / C-F02）：
// Run 准入在账本创建之前被拒时还没有 `run_id`，改用服务端生成的 `request_id` 关联；
// 该锚点**只记拒绝**（放行由 Run 自己的时间线承载），`run_id` 存在时优先级更高。
//
// 逆改点：把退路锚点去掉 → 首条长度断言红（事件为 0）；让放行也按 request_id 记 →
// 第二条"零新增"断言红；锚点优先级写反 → 第三条 `RunID` 断言红。
func TestQuotaDecision_RequestAnchorFallback(t *testing.T) {
	recorder, store := producerRecorder(t)

	recordQuotaDecision(t.Context(), recorder, quotaDecision{
		RequestID: "0f0e0d0c0b0a0908", Class: "run", Err: callers.ErrConcurrencyLimited,
	})
	require.Len(t, store.events, 1, "没有 run_id 时按 request_id 记拒绝")
	require.Equal(t, "0f0e0d0c0b0a0908", store.events[0].Correlation.RequestID)
	require.Empty(t, store.events[0].Correlation.RunID, "不得伪造 run_id")
	require.Equal(t, diagnostics.DecisionDeny, store.events[0].Decision)
	require.Equal(t, diagnostics.ReasonConcurrencyLimited, store.events[0].ReasonCode)

	recordQuotaDecision(t.Context(), recorder, quotaDecision{RequestID: "req-allow", Class: "run"})
	require.Len(t, store.events, 1, "request 锚点只记拒绝：放行零事件")

	recordQuotaDecision(t.Context(), recorder, quotaDecision{
		RunID: "run_9", RequestID: "req-both", Class: "run", Err: callers.ErrQuotaExhausted,
	})
	require.Len(t, store.events, 2)
	require.Equal(t, "run_9", store.events[1].Correlation.RunID, "run_id 优先")
	require.Empty(t, store.events[1].Correlation.RequestID)

	recordQuotaDecision(t.Context(), recorder, quotaDecision{Class: "run", Err: callers.ErrConcurrencyLimited})
	require.Len(t, store.events, 2, "两个锚点都缺席时不落库")
}

// TestRunAdmission_DenialRecordsRequestAnchoredEvent 守 [3] 生产路径的接线：准入
// 拒绝（身份非法）在 Run 账本创建之前发生，事件靠 `RunRequest.RequestID` 定位；
// 调度器等没有 HTTP 请求的来源不带 request_id，同样零事件。
//
// 逆改点：`run_admission.go` 不传 `RequestID` → 首条长度断言红；把 request 锚点
// 当成 allow 也记 → 第二条红（调度器路径没有锚点，长度仍为 1 才说明只记了首条）。
func TestRunAdmission_DenialRecordsRequestAnchoredEvent(t *testing.T) {
	recorder, store := producerRecorder(t)
	admission := &runAdmission{limiter: callers.NewLimiter(callers.LimiterDeps{}), diag: recorder}

	_, err := admission.run(t.Context(), core.RunRequest{RequestID: "0f0e0d0c0b0a0908"},
		func(context.Context, core.RunRequest) (core.RunResult, error) {
			t.Fatal("准入被拒的请求不得进入执行体")
			return core.RunResult{}, nil
		})
	require.ErrorIs(t, err, core.ErrUnauthorized)
	require.Len(t, store.events, 1)
	require.Equal(t, "0f0e0d0c0b0a0908", store.events[0].Correlation.RequestID)
	require.Equal(t, diagnostics.DecisionDeny, store.events[0].Decision)

	// 调度器路径：没有 request_id，也没有 run_id（调度器的 Run 尚未创建）→ 零事件。
	_, err = admission.run(t.Context(), core.RunRequest{},
		func(context.Context, core.RunRequest) (core.RunResult, error) {
			t.Fatal("准入被拒的请求不得进入执行体")
			return core.RunResult{}, nil
		})
	require.ErrorIs(t, err, core.ErrUnauthorized)
	require.Len(t, store.events, 1, "没有锚点的事件不发（不伪造关联）")
}

// TestChatQuota_RecordsVerdictOnTheCallPath 守 `chatQuota.AdmitCall` 的接线：判定先
// 留痕再返回，放行 / 拒绝两态都记；拒绝时错误链原样上抛（诊断不改变业务结论）。
//
// 逆改点：删掉 `recordQuotaDecision` 调用 → 两条长度断言红；让诊断失败影响返回 →
// `ErrorIs` 断言红。
func TestChatQuota_RecordsVerdictOnTheCallPath(t *testing.T) {
	recorder, store := producerRecorder(t)

	// 放行：内置管理员不限量，不落预留行，但判定照样留痕。
	allowQuota := &chatQuota{
		limiter: callers.NewLimiter(callers.LimiterDeps{LocalAdminUnlimited: true}),
		diag:    recorder,
	}
	_, err := allowQuota.AdmitCall(t.Context(), core.CallQuotaRequest{
		Principal: core.LocalAdminPrincipal(), RunID: "run_5", Round: 1,
		IdempotencyKey: "run:run_5:chat:1:a1",
	})
	require.NoError(t, err)
	require.Len(t, store.events, 1)
	require.Equal(t, diagnostics.DecisionAllow, store.events[0].Decision)
	require.Empty(t, store.events[0].ReasonCode)

	// 拒绝：假存储给出配额耗尽，判定记 deny + quota_exhausted，错误链保持。
	denyQuota := &chatQuota{
		limiter: callers.NewLimiter(callers.LimiterDeps{
			Quotas: &adapterQuotaStore{reserveErr: callers.ErrQuotaExhausted},
		}),
		diag: recorder,
	}
	_, err = denyQuota.AdmitCall(t.Context(), core.CallQuotaRequest{
		Principal: adapterCallerPrincipal(), RunID: "run_5", Round: 2,
		IdempotencyKey: "run:run_5:chat:2:a1",
	})
	require.ErrorIs(t, err, callers.ErrQuotaExhausted, "诊断不得改写或吞掉判定错误")
	require.Len(t, store.events, 2)
	denied := store.events[1]
	require.Equal(t, diagnostics.DecisionDeny, denied.Decision)
	require.Equal(t, diagnostics.ReasonQuotaExhausted, denied.ReasonCode)
	require.Equal(t, "2", denied.StepID)
}
