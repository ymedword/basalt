package tool

// 本文件是执行档位（14D）D11 在**判定面**的判据：白名单里解析到受保护地址段的条目，
// 升级后默认在拨号面拒绝；只有管理员逐条登记访问例外才放行，删除例外后下一次判定即
// 失效。（升级后的待确认项列表、"保持拒绝"与"加例外"各自的审计行由 app 层的
// `TestExecutionProfile_MigrationStrict` 判；本文件只判运行期授权结论。）
//
// 为什么域名面**放行**、拨号面才拒：白名单表达的是管理员的意图（这个域名要访问），
// 而"它解析到哪里"只有在拨号时才知道——域名面判不到地址，判定必须落在解析之后。
// The allowlist expresses intent; the resolved address is only knowable at dial time, so
// the refusal — and therefore the exception — lives on the dial face.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestExecutionProfile_WhitelistResolvedProtected 覆盖 D11：受保护地址段的存量白名单
// 条目**默认拒绝**，逐条例外只放行该域名，例外删除后立即回到拒绝。
// Acceptance point (D11): an allowlisted host resolving into a protected range is denied
// by default, admitted only by a per-host exception, and denied again once it is removed.
func TestExecutionProfile_WhitelistResolvedProtected(t *testing.T) {
	ctx := context.Background()
	resolver := &stubResolver{answers: map[string][]string{
		"intranet.example.com":        {"10.20.30.40"},
		"legacy-intranet.example.com": {"10.20.30.41"},
	}}
	stop := errors.New("test dial stop")
	dialer := &recordingDial{fail: stop}

	// 例外集合是**活快照**：闭包每次拨号读当前值，删除后下一次判定即失效。
	// The exception list is a live snapshot read at every dial.
	var exceptions []core.NetworkException
	guard := NewNetGuard(NetGuardOptions{
		Resolver:    resolver,
		DialContext: dialer.DialContext,
		Exceptions:  func() []core.NetworkException { return exceptions },
	})

	// 存量条目的常见形态是严格档 + 白名单命中：域名面放行，拨号面默认拒绝。
	policy := newExecutionProfilePolicy(t, core.ProfileStrict, "intranet.example.com")
	requirePolicyAllowed(t, policy, core.Action{
		Type: core.ActionHTTPRequest, Target: "https://intranet.example.com/", Agent: "demo-agent",
	})
	requireGuardRefused(t, guard, "intranet.example.com:443",
		core.DenialExceptionMissing, "目标解析落在受保护地址段")
	require.Empty(t, dialer.dialed(), "默认拒绝时一个连接都不许拨出")

	// ① 「加例外」：只放行被登记的那一条，拨的是已校验的地址。
	exceptions = []core.NetworkException{{
		Kind: core.NetworkExceptionKindHost, Value: "intranet.example.com",
	}}
	_, err := guard.DialContext(ctx, "tcp", "intranet.example.com:443")
	require.ErrorIs(t, err, stop, "有例外的受保护地址必须真的拨出")
	require.Equal(t, []string{"10.20.30.40:443"}, dialer.dialed(),
		"拨的是已校验的地址，域名留给 Host / SNI")

	// ② 例外只覆盖它点名的那一条：同段地址的另一个域名仍然拒（host 例外按域名匹配，
	// 不做地址段级放行）。
	requireGuardRefused(t, guard, "legacy-intranet.example.com:443",
		core.DenialExceptionMissing, "目标解析落在受保护地址段")

	// ③ 「删除例外」：下一次判定即失效，不需要重建客户端。
	exceptions = nil
	requireGuardRefused(t, guard, "intranet.example.com:443",
		core.DenialExceptionMissing, "目标解析落在受保护地址段")

	// ④ 标准档同样只靠例外放行：档位不改变受保护地址段的结论。
	standard := newExecutionProfilePolicy(t, core.ProfileStandard, "intranet.example.com")
	requirePolicyAllowed(t, standard, core.Action{
		Type: core.ActionHTTPRequest, Target: "https://intranet.example.com/", Agent: "demo-agent",
	})
	requireGuardRefused(t, guard, "intranet.example.com:443",
		core.DenialExceptionMissing, "目标解析落在受保护地址段")
}
