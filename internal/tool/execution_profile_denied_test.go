package tool_test

// 本文件是禁止域名表（14D / D-10）的判定判据：与白名单**同一套**通配语法
// （`*.example.com` 只覆盖子域，**不**覆盖裸域名 `example.com`），两档都先于放行
// 判定；保存后**下一次**判定即生效（活快照，删除同样立即失效），不需要重建策略或
// 重启进程。授权面的"禁止 > 允许"由本文件钉住；拨号面由 `TestNetGuard_*` 单列。
// The denylist shares the allowlist's wildcard syntax (a `*.suffix` entry never
// covers the bare suffix), binds both profiles, and takes effect on the next
// judgement because the policy reads a live snapshot.

import (
	"testing"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// TestExecutionProfile_DeniedDomain 覆盖 D-10：`*.example.com` 不匹配裸域名；命中
// 禁止表的域名在两档都被拒，即便它同时命中白名单；保存与删除都对**下一次**判定
// 生效。
// Acceptance point (D10): the wildcard never covers the bare domain, both profiles
// refuse a denied host before any allow, and a save/delete binds the next judgement.
func TestExecutionProfile_DeniedDomain(t *testing.T) {
	for _, profile := range []core.ExecutionProfile{core.ProfileStandard, core.ProfileStrict} {
		t.Run(string(profile)+"_wildcard_skips_bare_domain", func(t *testing.T) {
			f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
				// 白名单把裸域名登记为放行，确保"被拒"只能来自禁止表而不是缺省拒绝。
				opts.AllowedDomains = []string{"example.com"}
			})
			f.policy.SetExecutionProfile(profile)
			f.policy.SetDeniedDomains([]string{"*.example.com"})
			// 子域命中禁止表：两档都拒，且与白名单是否命中无关。
			requireRefused(t, f.policy, httpAction("https://api.example.com/v1/items"), "禁止域名表")
			// 裸域名不被 `*.` 条目覆盖：设置类判定必须放行（白名单命中）。
			requireAllowed(t, f.policy, httpAction("https://example.com/"))
		})

		t.Run(string(profile)+"_save_binds_next_judgement", func(t *testing.T) {
			f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
				opts.AllowedDomains = []string{"api.example.com"}
			})
			f.policy.SetExecutionProfile(profile)
			action := httpAction("https://api.example.com/v1/items")
			// 保存前：白名单命中即放行（严格档同样如此）。
			requireAllowed(t, f.policy, action)
			// 保存后立即生效：同一条动作的下一次判定改为拒绝。
			f.policy.SetDeniedDomains([]string{"api.example.com"})
			requireRefused(t, f.policy, action, "禁止域名表")
			// 删除同样立即失效：下一次判定回到白名单结论。
			f.policy.SetDeniedDomains(nil)
			requireAllowed(t, f.policy, action)
		})
	}
}
