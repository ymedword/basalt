package core

// 本文件判终态 reason 的**来源**（评审 IDX-26）：keeper 用 `context.WithCancelCause`
// 登记的租约原因必须落成契约固定字面量，不能被 `ctx.Err()` 抹平成 "context canceled"
// ——否则 Run 行再也分不出"租约丢了"与"用户取消"。
// These cases pin the cancelled-outcome reason derivation from the context cause.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRun_CancelledReasonKeepsLeaseLossCause —— 四种原因各自落对文案：
// 失权 / 续约截止 → `renew_failed`（契约「租约丢失的 reason 字面量」固定值）；
// 他处取消请求与普通取消仍按原样文案（不是租约故障，不借机改写）；
// 时限到 → 契约 §6 的时限字面量；其他错误原样带出；nil 为空串。
//
// 逆改点：`cancelled` 退回只看 `cause.Error()` / 调用方退回传 `ctx.Err()` →
// 前两条变成 "core: lease ownership lost" / "context canceled"，用例红。
func TestRun_CancelledReasonKeepsLeaseLossCause(t *testing.T) {
	env := &RunEnv{}
	cases := []struct {
		name  string
		cause error
		want  string
	}{
		{"renew deadline", ErrLeaseRenewDeadline, "renew_failed"},
		{"lease lost", ErrLeaseLost, "renew_failed"},
		{"remote cancel request", ErrCancelRequested, context.Canceled.Error()},
		{"plain cancel", context.Canceled, "context canceled"},
		{"deadline", context.DeadlineExceeded, reasonRunTimeout},
		{"other", errors.New("boom"), "boom"},
		{"none", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := env.cancelled(tc.cause, 0, 0)
			require.Equal(t, StatusCancelled, result.Status)
			require.Equal(t, tc.want, result.Reason)
		})
	}
}
