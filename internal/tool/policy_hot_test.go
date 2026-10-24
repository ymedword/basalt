package tool_test

// 本文件判出网域名白名单的**热替换**语义（US-12 R8 裁决 2）：管理台保存后下一次判定
// 即按新规则、不重启；空列表 = 全部拒绝；快照读取是副本；并发替换与判定无竞态；
// 撤销 MCP 连接用的 `AllowsHost` 与逐跳判定共用同一份通配语义。
// The outbound allowlist is hot-swappable: the next check sees the new rules, an empty
// list denies everything, and the snapshot copy cannot be mutated from outside.

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// TestWhitelistPolicy_HotSwapsDomains —— 构造期规则放行 → 保存新规则后下一次判定立刻
// 改判（旧域名被拒、新域名放行），**不**重建策略实例。
// 逆改点：把域名表退回普通字段（非原子快照）→ 本用例的改判断言与 -race 同时红。
func TestWhitelistPolicy_HotSwapsDomains(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{"api.example.com"}
	})
	requireAllowed(t, f.policy, httpAction("https://api.example.com/v1/chat"))

	f.policy.SetAllowedDomains([]string{"hooks.example.net"})
	requireRefused(t, f.policy, httpAction("https://api.example.com/v1/chat"), "域名不在白名单")
	requireAllowed(t, f.policy, httpAction("https://hooks.example.net/notify"))
}

// TestWhitelistPolicy_EmptyDomainsDenyAll —— 空规则是**合法值**：受此策略约束的出站
// 请求全部拒绝（不是"未配置即放行"）；传入 nil 同样得到空规则。
// 逆改点：空切片被当成"未设置 → 允许全部" → 红。
func TestWhitelistPolicy_EmptyDomainsDenyAll(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{"api.example.com"}
	})
	f.policy.SetAllowedDomains([]string{})
	requireRefused(t, f.policy, httpAction("https://api.example.com/v1/chat"), "域名不在白名单")

	f.policy.SetAllowedDomains(nil)
	requireRefused(t, f.policy, httpAction("https://api.example.com/v1/chat"), "域名不在白名单")
	require.NotNil(t, f.policy.AllowedDomains(), "空规则必须投影为 `[]`，不是 nil")
	require.Empty(t, f.policy.AllowedDomains())
}

// TestWhitelistPolicy_DomainGuidanceNamesConsole —— 模型可见的拒绝指引改为"管理台
// 保存即生效、无需重启"（R8 裁决 2 的文案要求）；不得再让模型等"改配置并重启"。
// 逆改点：指引改回「配置键 http.allowed_domains 并重启服务」→ 红。
func TestWhitelistPolicy_DomainGuidanceNamesConsole(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{}
	})
	body := requireRefused(t, f.policy, httpAction("https://api.example.com/v1/chat"), "域名不在白名单")
	require.Contains(t, body, "管理台「执行环境」页")
	require.Contains(t, body, "保存后立即生效，无需重启")
	require.NotContains(t, body, "并重启服务", "出网白名单已可在管理台热生效，指引不得再要求重启")
}

// TestWhitelistPolicy_AllowedDomainsIsCopy —— 读取快照返回副本：调用方改动返回值
// 不影响生效规则，也不会与并发判定共享底层数组。
// 逆改点：直接返回内部切片 → 改动后判定变化，本用例红。
func TestWhitelistPolicy_AllowedDomainsIsCopy(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{"b.example.com", "a.example.com", "a.example.com", " A.example.com "}
	})
	snapshot := f.policy.AllowedDomains()
	require.Equal(t, []string{"a.example.com", "b.example.com"}, snapshot,
		"投影必须小写、去重、升序")

	snapshot[0] = "evil.example.org"
	require.Equal(t, []string{"a.example.com", "b.example.com"}, f.policy.AllowedDomains(),
		"外部改动不得穿透内部快照")
	requireAllowed(t, f.policy, httpAction("https://a.example.com/x"))
	requireRefused(t, f.policy, httpAction("https://evil.example.org/x"), "域名不在白名单")
}

// TestWhitelistPolicy_AllowsHostSharesDomainRule —— `AllowsHost`（MCP 长连接撤销复用）
// 与逐跳判定同一份通配语义：`*.suffix` 只覆盖子域，不覆盖裸后缀；空规则全部拒绝。
// 逆改点：撤销侧另写一份匹配（如把裸后缀也当成命中）→ 对应子例红。
func TestWhitelistPolicy_AllowsHostSharesDomainRule(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{"*.example.com", "hooks.example.net"}
	})
	require.True(t, f.policy.AllowsHost("api.example.com"), "通配条目覆盖子域")
	require.False(t, f.policy.AllowsHost("example.com"), "通配条目不覆盖裸后缀")
	require.True(t, f.policy.AllowsHost("hooks.example.net"))
	require.False(t, f.policy.AllowsHost("other.example.net"))

	f.policy.SetAllowedDomains(nil)
	require.False(t, f.policy.AllowsHost("api.example.com"), "空规则下没有任何主机被允许")

	var nilPolicy *tool.WhitelistPolicy
	require.False(t, nilPolicy.AllowsHost("api.example.com"), "nil 策略按拒绝处理，不 panic")
}

// TestWhitelistPolicy_ConcurrentSwapAndCheck —— 替换与判定并发进行：每个读者要么看到
// 旧的完整规则、要么看到新的完整规则，绝不看到半份（`-race` 下同时判数据竞争）。
// 逆改点：改用 map 字段原地改写 → 竞态检测器报数据竞争。
func TestWhitelistPolicy_ConcurrentSwapAndCheck(t *testing.T) {
	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedDomains = []string{"a.example.com"}
	})
	action := httpAction("https://a.example.com/v1")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				f.policy.SetAllowedDomains([]string{"a.example.com", "b.example.com"})
				f.policy.SetAllowedDomains([]string{"a.example.com"})
			}
		}()
	}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				// a.example.com 在两份规则里都在，任何时刻都必须被放行。
				if err := f.policy.Check(context.Background(), action); err != nil {
					t.Errorf("并发替换期间仍放行的域名被拒：%v", err)
					return
				}
				_ = f.policy.AllowedDomains()
			}
		}()
	}
	wg.Wait()

	f.policy.SetAllowedDomains([]string{"a.example.com"})
	require.NoError(t, f.policy.Check(context.Background(), action))
	require.Equal(t, core.ActionHTTPRequest, action.Type)
}
