package tool_test

// 本文件是执行档位（14D）D2 判据：**授权范围等价**——严格档的域名 / 命令 / 解释器授权
// 结论与升级前（只有一档时的矩阵）逐条一致；标准档只在两处**有意**放宽（未命中禁止域名
// 的公网域名、安装清单登记的程序目录可执行件），其余结论与严格档相同。基础防护四项
// （DNS 解析校验、连接固定、忽略代理环境变量、shell 名称集合）是**有意偏离**，由
// `TestNetGuard_*` / `TestExecutionProfile_ShellNameSet` 单列，不混进这里的"等价"。
// D2: strict keeps the pre-upgrade authorization verdicts line by line; standard only
// widens the two documented faces. The four baseline upgrades are tested separately.

import (
	"testing"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// TestExecutionProfile_AuthorizationParity 覆盖 D2：同一批动作在两档下的判定，严格档与
// 升级前矩阵逐条一致，标准档的差值只有"未命中禁止域名的公网域名"与"清单登记的程序目录
// 可执行件"两项放开；端口档、IP 字面量与命令白名单在**两档都**不放松。
// Acceptance point (D2): the profile delta is exactly the two documented widenings.
func TestExecutionProfile_AuthorizationParity(t *testing.T) {
	newFixture := func() *policyFixture {
		return newPolicyFixture(t, func(opts *tool.PolicyOptions) {
			opts.AllowedDomains = []string{"api.example.com", "*.data.example.com"}
			opts.AllowedCommands = []string{"git"}
			opts.Interpreters = []string{"python3"}
			opts.ProgramDirNames = []string{"tool"}
		})
	}

	t.Run("strict_matches_pre_upgrade_matrix", func(t *testing.T) {
		f := newFixture()
		f.policy.SetExecutionProfile(core.ProfileStrict)
		// 域名：白名单命中放行；通配只覆盖子域；未登记的公网域名拒（升级前同结论）。
		requireAllowed(t, f.policy, httpAction("https://api.example.com/v1/items"))
		requireAllowed(t, f.policy, httpAction("https://a.data.example.com/x"))
		requireRefused(t, f.policy, httpAction("https://data.example.com/x"), "域名不在白名单")
		requireRefused(t, f.policy, httpAction("https://public.example.org/"), "域名不在白名单")
		// 命令：白名单整串命中放行；程序目录清单在严格档**不**放行（要显式登记）。
		requireAllowed(t, f.policy, shellAction("git"))
		requireRefused(t, f.policy, shellAction("tool"), "可执行名不在白名单")
		// 解释器：命中即放行到执行侧（argv 形态由执行侧判）。
		requireAllowed(t, f.policy, shellAction("python3"))
	})

	t.Run("standard_widens_exactly_two_faces", func(t *testing.T) {
		f := newFixture()
		f.policy.SetExecutionProfile(core.ProfileStandard)
		// 放宽 ①：未命中禁止域名的公网域名放行。
		requireAllowed(t, f.policy, httpAction("https://public.example.org/"))
		// 放宽 ②：清单登记的程序目录可执行件按名放行到执行侧。
		requireAllowed(t, f.policy, shellAction("tool"))
		// 其余结论不变：白名单 / 解释器照旧，未登记的第三方命令仍拒。
		requireAllowed(t, f.policy, httpAction("https://api.example.com/v1/items"))
		requireAllowed(t, f.policy, shellAction("git"))
		requireAllowed(t, f.policy, shellAction("python3"))
		requireRefused(t, f.policy, shellAction("curl"), "可执行名不在白名单")
	})

	t.Run("neither_profile_relaxes_shape_rules", func(t *testing.T) {
		for _, profile := range []core.ExecutionProfile{core.ProfileStandard, core.ProfileStrict} {
			f := newFixture()
			f.policy.SetExecutionProfile(profile)
			// IP 字面量目标：两档都拒（放行 IP 等于绕过域名表）。
			requireRefused(t, f.policy, httpAction("https://93.184.216.34/x"), "出网目标不是域名")
			// 非缺省端口：两档都拒（端口档与档位无关）。
			requireRefused(t, f.policy, httpAction("https://api.example.com:8443/x"), "端口不在默认档")
			// 多词命令：两档都拒（命令形态与档位无关）。
			requireRefused(t, f.policy, shellAction("git status"), "可执行名含空白")
		}
	})

	// 禁止域名先于一切放行分支：两档同判、与白名单是否命中无关（D10 在 T107 另测）。
	t.Run("denied_domain_binds_both_profiles", func(t *testing.T) {
		for _, profile := range []core.ExecutionProfile{core.ProfileStandard, core.ProfileStrict} {
			f := newFixture()
			f.policy.SetExecutionProfile(profile)
			f.policy.SetDeniedDomains([]string{"*.example.com"})
			requireRefused(t, f.policy, httpAction("https://api.example.com/v1/items"), "禁止域名表")
			requireRefused(t, f.policy, httpAction("https://a.other.example.com/"), "禁止域名表")
		}
	})
}
