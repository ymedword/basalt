package app

// 本文件锁死 `knowledge_adapters.go` 的契约面（US-16 C-06）：配额端口的类别绑定与
// 哨兵翻译、诊断字面量到冻结契约的逐值翻译。改任何一侧的名字或取值，这里先红——
// `internal/knowledge` 不 import `callers` / `diagnostics` 之后，字段与字面量的
// 一致性只能靠这一组用例证明。
//
// 断言口径：
//   - 配额错误**同时**命中领域哨兵与实现哨兵（双 `%w`）：知识面按前者分类，
//     HTTP 面按后者映射 429；
//   - 诊断事件经真实 `diagnostics.Recorder` 落库前必须通过 `Validate()`，原因码与
//     判定逐字等于契约冻结值，事件种类固定 `retrieval`。
//
// This file pins the adapter contract surface: class binding, sentinel translation and
// the value-by-value diagnostic literal mapping. It is the only place proving the two
// sides stay aligned now that knowledge imports neither helpers nor the contract.

import (
	"bytes"
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
	"github.com/ymedword/basalt/internal/knowledge"
)

// adapterPeriodStart 是假预留的周期起点（Unix 毫秒），用来证明票据字段往返无损。
const adapterPeriodStart int64 = 1_700_000_000_000

// adapterQuotaStore 是进程内的 `callers.QuotaStore`：记录入参并允许注入错误。
// adapterQuotaStore records inputs and lets each path inject its own error.
type adapterQuotaStore struct {
	class      callers.RequestClass
	cost       callers.Cost
	idem       string
	reserveErr error
	// commitErr 注入结算失败（N-01 的"结果未知"与"已收口"两条路径共用它）。
	// commitErr injects a settle failure, shared by the unknown and terminal paths.
	commitErr error
	commits   []callers.Usage
	releases  []callers.ReleaseReason
}

func (s *adapterQuotaStore) Reserve(_ context.Context, callerID string, class callers.RequestClass,
	cost callers.Cost, idem string,
) (callers.Reservation, error) {
	s.class, s.cost, s.idem = class, cost, idem
	if s.reserveErr != nil {
		return callers.Reservation{}, s.reserveErr
	}
	return callers.Reservation{
		ID:          "res_1",
		CallerID:    callerID,
		Class:       class,
		PeriodStart: adapterPeriodStart,
		Amount:      cost.Bytes,
		Status:      callers.ReservationPending,
	}, nil
}

func (s *adapterQuotaStore) Commit(_ context.Context, _, _ string, usage callers.Usage) (callers.Settlement, error) {
	s.commits = append(s.commits, usage)
	return callers.Settlement{}, s.commitErr
}

func (s *adapterQuotaStore) Release(_ context.Context, _, _ string, reason callers.ReleaseReason) error {
	s.releases = append(s.releases, reason)
	return nil
}

func (s *adapterQuotaStore) Reconcile(context.Context, string, callers.ReconcileOutcome, int64, string) (callers.Settlement, error) {
	return callers.Settlement{}, nil
}

func (s *adapterQuotaStore) ClearOverage(context.Context, string, int64, string) error { return nil }

func (s *adapterQuotaStore) SetLimit(context.Context, string, callers.Limit) error { return nil }

func (s *adapterQuotaStore) ListPending(context.Context, string) ([]callers.Reservation, error) {
	return nil, nil
}

func (s *adapterQuotaStore) Get(context.Context, string, string) (callers.Reservation, error) {
	return callers.Reservation{}, nil
}

// adapterCallerPrincipal 是过 `Principal.Valid` 的外部调用方身份。
// adapterCallerPrincipal is a caller identity that passes Principal.Valid.
func adapterCallerPrincipal() core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller_1",
		KeyID:    "key_1",
		Scopes:   []string{core.ScopeKnowledgeRead},
		Actor:    core.ActorCaller,
	}
}

// adapterQuota 用假存储组一个真实限制器，再套上被测适配器。
// adapterQuota builds a real limiter over the fake store, then wraps the adapter.
func adapterQuota(store *adapterQuotaStore) knowledge.Quota {
	return newKnowledgeQuotaAdapter(callers.NewLimiter(callers.LimiterDeps{Quotas: store}))
}

// TestKnowledgeQuotaAdapter_AdmitBindsIngestClass 守类别绑定与票据往返：摄取只有
// 一本账（`ClassIngest`），票据的预留标识 / 归属 / 周期必须原样过桥。
// 逆改点：类别写成 ClassChat 或票据丢字段 → 对应用例红。
func TestKnowledgeQuotaAdapter_AdmitBindsIngestClass(t *testing.T) {
	store := &adapterQuotaStore{}

	ticket, err := adapterQuota(store).AdmitCall(t.Context(), adapterCallerPrincipal(),
		knowledge.QuotaCost{Bytes: 96}, "ingest:job_1:bytes")
	require.NoError(t, err)

	require.Equal(t, callers.ClassIngest, store.class, "摄取必须以 ClassIngest 记账")
	require.Equal(t, callers.Cost{Bytes: 96}, store.cost)
	require.Equal(t, "ingest:job_1:bytes", store.idem, "幂等键原样透传")
	require.Equal(t, knowledge.QuotaTicket{
		ReservationID: "res_1",
		CallerID:      "caller_1",
		PeriodStart:   adapterPeriodStart,
	}, ticket)
}

// TestKnowledgeQuotaAdapter_SentinelTranslation 守双向哨兵：领域侧可分类
// （reason_code），实现侧可映射（HTTP 429）；非哨兵错误原样上抛。
// 逆改点：只保留一侧 → 对应 ErrorIs 红。
func TestKnowledgeQuotaAdapter_SentinelTranslation(t *testing.T) {
	cases := []struct {
		name   string
		caller error
		want   error
	}{
		{name: "quota exhausted", caller: callers.ErrQuotaExhausted, want: knowledge.ErrQuotaExhausted},
		{name: "rate limited", caller: callers.ErrRateLimited, want: knowledge.ErrRateLimited},
		{name: "concurrency limited", caller: callers.ErrConcurrencyLimited, want: knowledge.ErrConcurrencyLimited},
		{name: "caller unauthorized", caller: callers.ErrUnauthorized, want: core.ErrUnauthorized},
		{name: "caller forbidden", caller: callers.ErrForbidden, want: core.ErrForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &adapterQuotaStore{reserveErr: tc.caller}
			_, err := adapterQuota(store).AdmitCall(t.Context(), adapterCallerPrincipal(),
				knowledge.QuotaCost{Tokens: 7}, "ingest:job_1:embed:att_1:1")
			require.ErrorIs(t, err, tc.want, "领域侧哨兵必须可判")
			require.ErrorIs(t, err, tc.caller, "实现侧标识必须保留：知识面 HTTP 429 映射只认它")
		})
	}

	t.Run("passthrough", func(t *testing.T) {
		boom := errors.New("reserve backend offline")
		store := &adapterQuotaStore{reserveErr: boom}
		_, err := adapterQuota(store).AdmitCall(t.Context(), adapterCallerPrincipal(),
			knowledge.QuotaCost{Tokens: 7}, "ingest:job_1:embed:att_1:1")
		require.ErrorIs(t, err, boom, "非哨兵错误必须原样上抛")
		require.NotErrorIs(t, err, knowledge.ErrQuotaExhausted, "不得把普通故障冒充配额拒绝")
	})
}

// TestKnowledgeQuotaAdapter_CommitAndRelease 守结算与归还：用量维度过桥、归还原因
// 逐值翻译、未知原因不落到实现；`NoReservation` 票据空操作。
// 逆改点：原因字面量写反或未知值放行 → 对应断言红。
func TestKnowledgeQuotaAdapter_CommitAndRelease(t *testing.T) {
	store := &adapterQuotaStore{}
	quota := adapterQuota(store)
	ticket := knowledge.QuotaTicket{
		ReservationID: "res_1",
		CallerID:      "caller_1",
		PeriodStart:   adapterPeriodStart,
	}

	require.NoError(t, quota.Commit(t.Context(), ticket, knowledge.QuotaUsage{Tokens: 7}))
	require.Equal(t, []callers.Usage{{Tokens: 7}}, store.commits)

	require.NoError(t, quota.Commit(t.Context(), knowledge.QuotaTicket{NoReservation: true},
		knowledge.QuotaUsage{Bytes: 96}))
	require.Len(t, store.commits, 1, "不限量票据不落账")

	require.NoError(t, quota.Release(t.Context(), ticket, knowledge.QuotaReleaseNotSent))
	require.NoError(t, quota.Release(t.Context(), ticket, knowledge.QuotaReleaseAuditFailed))
	require.Equal(t, []callers.ReleaseReason{
		callers.ReleaseNotSent, callers.ReleaseAuditFailed,
	}, store.releases)

	err := quota.Release(t.Context(), ticket, knowledge.QuotaReleaseReason("zero_usage"))
	require.Error(t, err, "知识面只允许「调用证明未发出」的两个原因")
	require.Len(t, store.releases, 2, "未知原因不得落到实现")
}

// adapterEventStore 收集写入器已脱敏的事件。
// adapterEventStore collects the redacted events the recorder persisted.
type adapterEventStore struct {
	events []diagnostics.DiagnosticEvent
}

func (s *adapterEventStore) InsertEvent(_ context.Context, ev diagnostics.DiagnosticEvent) error {
	s.events = append(s.events, ev)
	return nil
}

// adapterNow 是诊断用例的固定时钟。
var adapterNow = time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC)

// adapterDiag 用真实写入器组一个诊断适配器，返回捕获面与日志缓冲。
// adapterDiag wires the real recorder and returns the capture surface and log buffer.
func adapterDiag(t *testing.T) (knowledge.DiagnosticRecorder, *adapterEventStore, *bytes.Buffer) {
	t.Helper()
	cfg, err := diagnostics.NewConfig(0, 1)
	require.NoError(t, err)
	store := &adapterEventStore{}
	var logs bytes.Buffer
	recorder := diagnostics.NewRecorder(diagnostics.RecorderOptions{
		Store:    store,
		Sampler:  diagnostics.NewRateSampler(cfg),
		Redactor: diagnostics.NewWhitelistRedactor(),
		Now:      func() time.Time { return adapterNow },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	return newKnowledgeDiagAdapter(recorder, logger), store, &logs
}

// TestKnowledgeDiagAdapter_ReasonLiteralsMatchContract 守原因码逐值对齐：知识面包内
// 字面量与冻结契约**同值**（装配记录直接带这些字符串，值不同就是下游判据漂移）。
// 逆改点：任一侧改名 → string 比较或 ReasonCode 断言红。
func TestKnowledgeDiagAdapter_ReasonLiteralsMatchContract(t *testing.T) {
	cases := []struct {
		reason knowledge.DiagnosticReason
		want   diagnostics.ReasonCode
	}{
		{knowledge.DiagnosticReasonVectorUnavailable, diagnostics.ReasonVectorUnavailable},
		{knowledge.DiagnosticReasonKeywordUnavailable, diagnostics.ReasonKeywordUnavailable},
		{knowledge.DiagnosticReasonPermissionDenied, diagnostics.ReasonPermissionDenied},
		{knowledge.DiagnosticReasonBudgetTrimmed, diagnostics.ReasonBudgetTrimmed},
		{knowledge.DiagnosticReasonCitationUnresolved, diagnostics.ReasonCitationUnresolved},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			require.Equal(t, string(tc.want), string(tc.reason), "字面量必须与冻结契约同值")
			adapter, store, _ := adapterDiag(t)
			adapter.Record(t.Context(), knowledge.DiagnosticEvent{
				OccurredAt: adapterNow,
				RunID:      "run_1",
				StepID:     "step_1",
				Actor:      core.ActorCaller,
				Reason:     tc.reason,
				Decision:   knowledge.DiagnosticDecisionDegrade,
				Attrs:      map[string]any{"count": 1},
			})
			require.Len(t, store.events, 1)
			recorded := store.events[0]
			require.NoError(t, recorded.Validate(), "适配器产出的事件必须满足诊断契约")
			require.Equal(t, diagnostics.KindRetrieval, recorded.Kind, "知识面只有检索事件")
			require.Equal(t, tc.want, recorded.ReasonCode)
			require.Equal(t, diagnostics.DecisionDegrade, recorded.Decision)
			require.Equal(t, diagnostics.Correlation{RunID: "run_1"}, recorded.Correlation)
			require.Equal(t, "step_1", recorded.StepID)
			require.Equal(t, diagnostics.ActorCaller, recorded.Actor)
			require.Equal(t, adapterNow.UTC().UnixMilli(), recorded.OccurredAt)
			require.Equal(t, 1, recorded.Attrs["count"], "白名单键保留")
		})
	}
}

// TestKnowledgeDiagAdapter_DecisionLiteralsMatchContract 守判定逐值对齐。
// 逆改点：deny / trim / degrade 任一错位 → 对应用例红。
func TestKnowledgeDiagAdapter_DecisionLiteralsMatchContract(t *testing.T) {
	cases := []struct {
		decision knowledge.DiagnosticDecision
		want     diagnostics.Decision
	}{
		{knowledge.DiagnosticDecisionDeny, diagnostics.DecisionDeny},
		{knowledge.DiagnosticDecisionTrim, diagnostics.DecisionTrim},
		{knowledge.DiagnosticDecisionDegrade, diagnostics.DecisionDegrade},
	}
	for _, tc := range cases {
		t.Run(string(tc.decision), func(t *testing.T) {
			require.Equal(t, string(tc.want), string(tc.decision), "字面量必须与冻结契约同值")
			adapter, store, _ := adapterDiag(t)
			adapter.Record(t.Context(), knowledge.DiagnosticEvent{
				OccurredAt: adapterNow,
				RunID:      "run_1",
				Actor:      core.ActorScheduler,
				Reason:     knowledge.DiagnosticReasonBudgetTrimmed,
				Decision:   tc.decision,
			})
			require.Len(t, store.events, 1)
			require.Equal(t, tc.want, store.events[0].Decision)
			require.Equal(t, diagnostics.ActorScheduler, store.events[0].Actor)
		})
	}
}

// TestKnowledgeDiagAdapter_CitationUnresolvedKeepsEmptyDecision 守引用复核事件的形状
// （US-16 V-6）：`citation_unresolved` 是**结论性**事件，没有 allow / deny 可填——
// 空判定必须原样通过，不得被当成未知字面量丢弃，也不得被补成某个决策。
// 逆改点：`knowledgeDecision` 删掉空串分支 / 事件改带决策 → 本用例红（前者丢事件，
// 后者判定断言红）。
func TestKnowledgeDiagAdapter_CitationUnresolvedKeepsEmptyDecision(t *testing.T) {
	adapter, store, _ := adapterDiag(t)
	adapter.Record(t.Context(), knowledge.DiagnosticEvent{
		OccurredAt: adapterNow,
		RunID:      "run_1",
		Actor:      core.ActorCaller,
		Reason:     knowledge.DiagnosticReasonCitationUnresolved,
		Attrs:      map[string]any{"count": 2},
	})
	require.Len(t, store.events, 1, "空判定是契约允许的「未给出判定」，事件必须落库")
	recorded := store.events[0]
	require.NoError(t, recorded.Validate(), "适配器产出的事件必须满足诊断契约")
	require.Equal(t, diagnostics.KindRetrieval, recorded.Kind)
	require.Equal(t, diagnostics.ReasonCitationUnresolved, recorded.ReasonCode)
	require.Equal(t, diagnostics.Decision(""), recorded.Decision,
		"引用复核没有决策可填，不得凭空补一个")
	require.Equal(t, map[string]any{"count": 2}, recorded.Attrs)
}

// TestKnowledgeDiagAdapter_ZeroOccurredAtLeftToWriter 守「不编时间」：知识面没给时刻
// 时留零值，由写入器按自己的时钟补齐。
// 逆改点：把零值时间转成负毫秒 → 事件被 Validate 丢弃，断言红。
func TestKnowledgeDiagAdapter_ZeroOccurredAtLeftToWriter(t *testing.T) {
	adapter, store, _ := adapterDiag(t)
	adapter.Record(t.Context(), knowledge.DiagnosticEvent{
		RunID:    "run_1",
		Actor:    core.ActorCaller,
		Reason:   knowledge.DiagnosticReasonVectorUnavailable,
		Decision: knowledge.DiagnosticDecisionDegrade,
	})
	require.Len(t, store.events, 1)
	require.Equal(t, adapterNow.UTC().UnixMilli(), store.events[0].OccurredAt)
}

// TestKnowledgeDiagAdapter_UnknownLiteralsDroppedWithWarn 守未知字面量的丢弃路径：
// 编程错误不得写进冻结列，也不得静默——WARN 必须可见。
// 逆改点：未知值照写 → 事件落库（Validate 红）或日志断言红。
func TestKnowledgeDiagAdapter_UnknownLiteralsDroppedWithWarn(t *testing.T) {
	adapter, store, logs := adapterDiag(t)
	adapter.Record(t.Context(), knowledge.DiagnosticEvent{
		RunID:    "run_1",
		Actor:    core.ActorCaller,
		Reason:   knowledge.DiagnosticReason("not_a_contract_reason"),
		Decision: knowledge.DiagnosticDecisionDegrade,
	})
	adapter.Record(t.Context(), knowledge.DiagnosticEvent{
		RunID:    "run_1",
		Actor:    core.ActorCaller,
		Reason:   knowledge.DiagnosticReasonVectorUnavailable,
		Decision: knowledge.DiagnosticDecision("not_a_contract_decision"),
	})
	require.Empty(t, store.events, "未知字面量不得落库")
	require.Equal(t, 2, bytes.Count(logs.Bytes(), []byte("diagnostic event dropped")),
		"丢弃必须留 WARN（双轨可观测性）")
}
