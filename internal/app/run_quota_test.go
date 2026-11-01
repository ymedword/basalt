package app

// 本文件锁死 Run 级任务次数适配器的口径（US-16 N-01）：类别恒 `ClassRun`、每次 1 次
// 任务、幂等键 `run:<run_id>:admit`、预留后**立即**结算、失败不释放。
//
// 逆改点（实现改坏时哪条断言红）：
//   - 类别写成 `ClassChat` → `Cost{Tasks:1}` 直接被 `callers` 的 validate 拒绝，
//     第一个用例红；
//   - 幂等键少了 `:admit` 或用了 run 内序号 → 键断言红（重放去重的派生源就变了）；
//   - 把结算留到 Run 结束后 → `commits` 为空断言红（任务次数是准入成本，不是结果）；
//   - 结算失败时调用 `Release` → "零归还"断言红（无法证明未消耗就不得归还）；
//   - 已收口的预留当成功放行 → 失败关闭用例红（等于绕过限额）。
//
// The verdict: one ClassRun task per logical run, keyed on run:<run_id>:admit, settled
// immediately, never released on an unknown outcome.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
)

// errInjectedSettle 是一次"提交结果未知"的存储故障（不是任何契约哨兵）。
// errInjectedSettle stands for an unknown commit outcome, not a contract sentinel.
var errInjectedSettle = errors.New("injected: commit outcome unknown")

// runQuotaAdapter 用假存储组一个真实限制器，再套上被测适配器。
// runQuotaAdapter builds a real limiter over the fake store, then the adapter.
func runQuotaAdapter(store *adapterQuotaStore, diag diagnosticRecorder) *runQuota {
	return newRunQuota(callers.NewLimiter(callers.LimiterDeps{Quotas: store}), diag, nil)
}

// TestRunQuota_ChargesOneTaskUnderRunAdmitKey 守记账口径：类别 / 数量 / 幂等键三件。
// TestRunQuota_ChargesOneTaskUnderRunAdmitKey pins class, amount and idempotency key.
func TestRunQuota_ChargesOneTaskUnderRunAdmitKey(t *testing.T) {
	store := &adapterQuotaStore{}
	res, err := runQuotaAdapter(store, nil).ChargeRun(t.Context(), core.RunQuotaRequest{
		Principal: adapterCallerPrincipal(),
		RunID:     "run_42",
	})
	require.NoError(t, err)

	require.Equal(t, callers.ClassRun, store.class, "Run 级任务次数必须以 ClassRun 记账")
	require.Equal(t, callers.Cost{Tasks: 1}, store.cost, "一次逻辑 Run 恒记 1 个任务")
	require.Equal(t, "run:run_42:admit", store.idem, "幂等键由 run_id 派生，末位是逻辑准入")
	require.Equal(t, []callers.Usage{{Tasks: 1}}, store.commits, "预留后立即结算，不留在 pending")
	require.Equal(t, "res_1", res.ReservationID, "预留标识原样返回（写进账本行）")
	require.Empty(t, store.releases, "成功路径不得归还")
}

// TestRunQuota_ReserveFailurePassesSentinelThrough 守拒绝原样上抛：入口据
// `callers.ErrQuotaExhausted` 映射 429，包装一层就映射不出来了。
// The reserve error must reach the entry point unwrapped.
func TestRunQuota_ReserveFailurePassesSentinelThrough(t *testing.T) {
	store := &adapterQuotaStore{reserveErr: callers.ErrQuotaExhausted}
	_, err := runQuotaAdapter(store, nil).ChargeRun(t.Context(), core.RunQuotaRequest{
		Principal: adapterCallerPrincipal(),
		RunID:     "run_43",
	})
	require.ErrorIs(t, err, callers.ErrQuotaExhausted)
	require.Empty(t, store.commits, "预留失败不得结算")
}

// TestRunQuota_UnlimitedAdminLeavesNoReservation 守内置管理员不限量：零预留行、
// 账本列留空——与 `TestQuota_LocalAdminUnlimitedNoReservation` 同一口径。
// The unlimited built-in admin reserves nothing and returns an empty id.
func TestRunQuota_UnlimitedAdminLeavesNoReservation(t *testing.T) {
	store := &adapterQuotaStore{}
	limiter := callers.NewLimiter(callers.LimiterDeps{Quotas: store, LocalAdminUnlimited: true})

	res, err := newRunQuota(limiter, nil, nil).ChargeRun(t.Context(), core.RunQuotaRequest{
		Principal: core.LocalAdminCLIPrincipal(),
		RunID:     "run_44",
	})
	require.NoError(t, err)
	require.Empty(t, res.ReservationID)
	require.Empty(t, store.commits, "没有预留就没有结算")
}

// TestRunQuota_SettleUnknownKeepsReservationWithoutRelease 守"结果未知"：不报错、
// 不释放、返回预留标识——Run 照常开始，预留继续占额度等管理面核对。
// An unknown settle outcome keeps the reservation pending and never releases it.
func TestRunQuota_SettleUnknownKeepsReservationWithoutRelease(t *testing.T) {
	store := &adapterQuotaStore{commitErr: errInjectedSettle}
	res, err := runQuotaAdapter(store, nil).ChargeRun(t.Context(), core.RunQuotaRequest{
		Principal: adapterCallerPrincipal(),
		RunID:     "run_45",
	})
	require.NoError(t, err, "结算结果未知不等于准入失败")
	require.Len(t, store.commits, 1, "确实尝试过结算")
	require.Empty(t, store.releases, "无法证明未消耗，不得归还")
	require.Equal(t, "res_1", res.ReservationID, "账本仍要记下这次预留")
}

// TestRunQuota_TerminalReservationFailsClosed 守已收口预留：管理面已把它收敛为
// released / reconciled，放行等于让这次执行绕过限额——失败关闭，原因上抛。
// A terminal reservation for this run id must fail closed.
func TestRunQuota_TerminalReservationFailsClosed(t *testing.T) {
	store := &adapterQuotaStore{commitErr: callers.ErrReservationSettled}
	res, err := runQuotaAdapter(store, nil).ChargeRun(t.Context(), core.RunQuotaRequest{
		Principal: adapterCallerPrincipal(),
		RunID:     "run_46",
	})
	require.ErrorIs(t, err, callers.ErrReservationSettled)
	require.Empty(t, res.ReservationID, "失败关闭时不返回可用结果")
	require.Empty(t, store.releases, "不得借此释放（预留已在终态）")
}

// TestRunQuota_RecordsVerdict 守诊断：放行与拒绝都记一条 `quota_decision`，
// 类目 `run_task`、锚点 run_id、拒绝映射 quota_exhausted。
// Both verdicts are recorded under the run_task class, anchored on the run id.
func TestRunQuota_RecordsVerdict(t *testing.T) {
	recorder, events := producerRecorder(t)

	_, err := runQuotaAdapter(&adapterQuotaStore{}, recorder).ChargeRun(t.Context(), core.RunQuotaRequest{
		Principal: adapterCallerPrincipal(),
		RunID:     "run_47",
	})
	require.NoError(t, err)

	_, err = runQuotaAdapter(&adapterQuotaStore{reserveErr: callers.ErrQuotaExhausted}, recorder).
		ChargeRun(t.Context(), core.RunQuotaRequest{
			Principal: adapterCallerPrincipal(),
			RunID:     "run_48",
		})
	require.ErrorIs(t, err, callers.ErrQuotaExhausted)

	require.Len(t, events.events, 2)
	allow, deny := events.events[0], events.events[1]
	require.Equal(t, diagnostics.KindQuotaDecision, allow.Kind)
	require.Equal(t, "run_47", allow.Correlation.RunID)
	require.Equal(t, "run_task", allow.Attrs["class"])
	require.Equal(t, diagnostics.DecisionAllow, allow.Decision)
	require.Empty(t, allow.ReasonCode, "放行不带原因码")

	require.Equal(t, "run_48", deny.Correlation.RunID)
	require.Equal(t, "run_task", deny.Attrs["class"])
	require.Equal(t, diagnostics.DecisionDeny, deny.Decision)
	require.Equal(t, diagnostics.ReasonQuotaExhausted, deny.ReasonCode)
}

// TestRunQuota_NilLimiterIsNotWired 守未接线：nil 限制器不预留、不报错，账本列留空
// ——轻装配与既有零值单测的行为与接线前逐字一致。
// A nil limiter keeps the pre-wiring behaviour.
func TestRunQuota_NilLimiterIsNotWired(t *testing.T) {
	res, err := newRunQuota(nil, nil, nil).ChargeRun(context.Background(), core.RunQuotaRequest{
		Principal: adapterCallerPrincipal(),
		RunID:     "run_49",
	})
	require.NoError(t, err)
	require.Empty(t, res.ReservationID)
}
