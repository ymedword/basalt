package tool

// 本文件是执行档位（14D）D1 与 D8 的判据：标准档基线七条，以及两档在**基础防护**上
// 同一结论、只在授权范围上分叉。
//
// 判据口径（模块卡 §4.4 的 D 表）：
//   - D1 标准档：公网域名允许；IP 字面量拒；私有段拒；元数据地址拒；解析到私有地址的
//     公网域名拒；重定向到私有地址拒；非 80/443 拒。
//   - D8 两档**基础防护一致**（解析到私有地址的域名、重定向到元数据地址、HTTP 代理环境
//     变量存在三种输入得到同一结论）；**授权范围不同**（标准档放行未命中禁止域名的公网
//     域名，严格档拒绝未加白名单的域名）。
//
// 判据纪律：解析器与拨号器都是注入的替身，不打真实 DNS、不连外网；唯一真实套接字是
// `httptest` 在本机起的服务，本机地址落在受保护段，因此每个放行路径都显式登记一条
// `ip_port` 例外——这既让服务可达，又顺带判了"例外按 IP + 端口逐字匹配"。
// Both the resolver and the dialer are stubs; the only real sockets are loopback httptest
// servers, which reach the exception path because loopback is a protected range.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// requirePolicyAllowed 断言放行：档位类判据只判结论，不比对文案。
// requirePolicyAllowed asserts the action is admitted.
func requirePolicyAllowed(t *testing.T, policy *WhitelistPolicy, action core.Action) {
	t.Helper()
	require.NoError(t, policy.Check(context.Background(), action), "该动作应当放行：%s", action.Target)
}

// requirePolicyRefused 断言拒绝，并确认拒绝**出自哪条规则**（分类只认类型）。
// requirePolicyRefused asserts a refusal and pins the rule it came from.
func requirePolicyRefused(t *testing.T, policy *WhitelistPolicy, action core.Action, ruleFragment string) {
	t.Helper()
	err := policy.Check(context.Background(), action)
	require.Error(t, err, "该动作必须被拒：%s", action.Target)
	require.True(t, core.IsSandboxViolation(err), "拒绝必须是 *core.SandboxViolationError（按类型分类）")
	require.Contains(t, err.Error(), ruleFragment)
}

// requireGuardRefused 断言拨号面拒绝：拒绝原因与规则片段都要对得上，且**没有任何**
// 连接被拨出（替身的 fail 非空时只记录不建连）。
// requireGuardRefused asserts the dial refusal reason and rule, with nothing dialled.
func requireGuardRefused(t *testing.T, guard *NetGuard, address string,
	reason core.PolicyDenialReason, ruleFragment string,
) {
	t.Helper()
	_, err := guard.DialContext(context.Background(), "tcp", address)
	var violation *core.SandboxViolationError
	require.ErrorAs(t, err, &violation, "拨号必须被拒：%s", address)
	require.Equal(t, reason, violation.Reason, "拒绝原因：%s", address)
	require.Contains(t, err.Error(), ruleFragment)
}

// newExecutionProfilePolicy 造一份最小策略实例：不建工作区骨架（出网判据不读文件），
// 只按档位与白名单判定。
// newExecutionProfilePolicy builds a policy instance for outbound judgement only.
func newExecutionProfilePolicy(t *testing.T, profile core.ExecutionProfile, allowed ...string) *WhitelistPolicy {
	t.Helper()
	policy := NewWhitelistPolicy(PolicyOptions{Root: t.TempDir(), AllowedDomains: allowed})
	require.True(t, policy.SetExecutionProfile(profile), "档位字面量必须合法：%s", profile)
	return policy
}

// TestExecutionProfile_StandardBaseline 覆盖 D1：标准档不靠白名单放行公网域名，但**不**
// 放宽任何一条基础防护——IP 字面量、私有段、元数据地址、解析到私有地址的公网域名、
// 重定向到私有地址、非默认端口全部照拒。
// Acceptance point (D1): the standard profile admits public domains without an allowlist
// while every baseline protection still refuses.
func TestExecutionProfile_StandardBaseline(t *testing.T) {
	policy := newExecutionProfilePolicy(t, core.ProfileStandard, "allowed.example.com")
	ctx := context.Background()

	// ① 公网域名：不在禁止域名表内即放行（不需要白名单）。
	requirePolicyAllowed(t, policy, core.Action{
		Type: core.ActionHTTPRequest, Target: "https://public.example.com/api", Agent: "demo-agent",
	})

	// ② IP 字面量：公网 IP 与私有段 IP 都在域名面上直接拒——放行 IP 等于绕过域名表。
	for _, target := range []string{
		"http://93.184.216.34/",
		"http://10.20.30.40/",
		"http://169.254.169.254/latest/meta-data/",
	} {
		requirePolicyRefused(t, policy, core.Action{
			Type: core.ActionHTTPRequest, Target: target, Agent: "demo-agent",
		}, "出网目标不是域名")
	}

	// ③ 非默认端口：http 只认 80、https 只认 443（交叉档与显式非默认端口同拒）。
	for _, target := range []string{
		"http://public.example.com:8080/",
		"http://public.example.com:443/",
		"https://public.example.com:80/",
	} {
		requirePolicyRefused(t, policy, core.Action{
			Type: core.ActionHTTPRequest, Target: target, Agent: "demo-agent",
		}, "出网目标端口不在默认档")
	}

	stop := errors.New("test dial stop")
	resolver := &stubResolver{answers: map[string][]string{
		"intranet.example.com":   {"10.20.30.40"},     // 私有段
		"metadata.example":       {"169.254.169.254"}, // 云元数据
		"api.public.example.com": {"10.0.0.7"},        // 公网域名解析到私有
		"public.example.com":     {"93.184.216.34"},   // 真公网
	}}
	dialer := &recordingDial{fail: stop}
	guard := NewNetGuard(NetGuardOptions{Resolver: resolver, DialContext: dialer.DialContext})

	// ④ 私有段：域名面放行（标准档不查白名单），拨号面按解析结果拒。
	requirePolicyAllowed(t, policy, core.Action{
		Type: core.ActionHTTPRequest, Target: "https://intranet.example.com/", Agent: "demo-agent",
	})
	requireGuardRefused(t, guard, "intranet.example.com:443",
		core.DenialExceptionMissing, "目标解析落在受保护地址段")

	// ⑤ 云元数据地址：即便登记了例外也不放行（判定在例外之**前**）。
	metadataGuard := NewNetGuard(NetGuardOptions{
		Resolver:    resolver,
		DialContext: dialer.DialContext,
		Exceptions:  func() []core.NetworkException { return ipPortException("169.254.169.254:443") },
	})
	requireGuardRefused(t, metadataGuard, "metadata.example:443",
		core.DenialProtectedAddress, "目标是云元数据地址")

	// ⑥ 解析到私有地址的公网域名：域名面看不出差别，拨号面同拒。
	requirePolicyAllowed(t, policy, core.Action{
		Type: core.ActionHTTPRequest, Target: "https://api.public.example.com/", Agent: "demo-agent",
	})
	requireGuardRefused(t, guard, "api.public.example.com:443",
		core.DenialExceptionMissing, "目标解析落在受保护地址段")

	// ⑦ 公网域名 + 公网地址：正常拨出已校验的 IP。
	_, err := guard.DialContext(ctx, "tcp", "public.example.com:443")
	require.ErrorIs(t, err, stop, "公网目标必须真的拨出")
	require.Equal(t, []string{"93.184.216.34:443"}, dialer.dialed(),
		"拨的是已校验的地址，域名留给 Host / SNI")

	// ⑧ 重定向到私有地址：3xx **不跟随**——第二跳一次解析、一次拨号都没有。
	requirePrivateRedirectNotFollowed(t)
}

// requirePrivateRedirectNotFollowed 用一个真实回环服务造"重定向到内网域名"，判 3xx
// 原样返回且重定向目标零拨号：跟随 3xx 等于把校验过的地址换成了模型选的地址。
// requirePrivateRedirectNotFollowed asserts a redirect to a private host is never followed.
func requirePrivateRedirectNotFollowed(t *testing.T) {
	t.Helper()
	var targetHits int
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHits++
	}))
	t.Cleanup(redirectTarget.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://intranet.example.com/", http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	_, port, err := net.SplitHostPort(redirector.Listener.Addr().String())
	require.NoError(t, err)
	resolver := &stubResolver{answers: map[string][]string{
		"redirecting.example":  {"127.0.0.1"},
		"intranet.example.com": {"10.20.30.40"},
	}}
	recorder := &recordingDial{}
	guard := NewNetGuard(NetGuardOptions{
		Resolver:    resolver,
		DialContext: recorder.DialContext,
		Exceptions: func() []core.NetworkException {
			return ipPortException(net.JoinHostPort("127.0.0.1", port))
		},
	})
	SetNetGuard(guard)
	t.Cleanup(func() { SetNetGuard(nil) })

	client := newHTTPClient()
	t.Cleanup(client.CloseIdleConnections)
	url := "http://" + net.JoinHostPort("redirecting.example", port) + "/"
	// 这里不判 `Policy.Check`：回环服务用的是随机端口，策略面按端口档会先拒——本子判据
	// 判的是"客户端拿到 3xx 后不跟过去"，与域名 / 端口授权是两件事。
	resp, err := client.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode, "3xx 原样返回，由逐跳循环重判")
	require.Equal(t, []string{net.JoinHostPort("127.0.0.1", port)}, recorder.dialed(),
		"只有第一跳被拨出：重定向目标一次解析、一次拨号都没有")
	require.Equal(t, 0, targetHits, "私有地址上的服务一次都没被碰到")
}

// TestExecutionProfile_BothProfilesSameBaseline 覆盖 D8：三种基础防护输入在两档得到
// **同一**结论，而授权范围按档位分叉。
// Acceptance point (D8): baseline protections are profile-invariant; authorization differs.
func TestExecutionProfile_BothProfilesSameBaseline(t *testing.T) {
	profiles := []core.ExecutionProfile{core.ProfileStandard, core.ProfileStrict}

	t.Run("private_resolution_refused_in_both", func(t *testing.T) {
		stop := errors.New("test dial stop")
		resolver := &stubResolver{answers: map[string][]string{
			"intranet.example.com": {"10.20.30.40"},
		}}
		dialer := &recordingDial{fail: stop}
		guard := NewNetGuard(NetGuardOptions{Resolver: resolver, DialContext: dialer.DialContext})
		for _, profile := range profiles {
			// 域名单加进白名单：让两档的**域名面**都放行，剩下的差别只可能来自基础防护。
			policy := newExecutionProfilePolicy(t, profile, "intranet.example.com")
			requirePolicyAllowed(t, policy, core.Action{
				Type: core.ActionHTTPRequest, Target: "https://intranet.example.com/", Agent: "demo-agent",
			})
			requireGuardRefused(t, guard, "intranet.example.com:443",
				core.DenialExceptionMissing, "目标解析落在受保护地址段")
		}
		require.Empty(t, dialer.dialed(), "两档都一个连接都没拨出")
	})

	t.Run("metadata_redirect_not_followed_in_both", func(t *testing.T) {
		var targetHits int
		redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			targetHits++
		}))
		t.Cleanup(redirectTarget.Close)
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 重定向目标解析到云元数据地址：跟过去必然被拒，判据是"根本不去解析 / 拨它"。
			http.Redirect(w, r, "http://metadata.example/latest/meta-data/", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redirector.Close)

		_, port, err := net.SplitHostPort(redirector.Listener.Addr().String())
		require.NoError(t, err)
		resolver := &stubResolver{answers: map[string][]string{
			"redirecting.example": {"127.0.0.1"},
			"metadata.example":    {"169.254.169.254"},
		}}
		recorder := &recordingDial{}
		guard := NewNetGuard(NetGuardOptions{
			Resolver:    resolver,
			DialContext: recorder.DialContext,
			Exceptions: func() []core.NetworkException {
				return ipPortException(net.JoinHostPort("127.0.0.1", port))
			},
		})
		SetNetGuard(guard)
		t.Cleanup(func() { SetNetGuard(nil) })

		for _, profile := range profiles {
			// 活档位切到本档（策略面不判这两条 URL：回环服务的随机端口会先被端口档拒，
			// 与"3xx 不跟随"是两件事）；结论只取决于客户端与守门器，因此两档必须一致。
			policy := newExecutionProfilePolicy(t, profile, "redirecting.example")
			require.Equal(t, profile, policy.ExecutionProfile())
			url := "http://" + net.JoinHostPort("redirecting.example", port) + "/"
			client := newHTTPClient()
			resp, err := client.Get(url)
			require.NoError(t, err)
			require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode,
				"档位 %s：3xx 原样返回", profile)
			resp.Body.Close()
			client.CloseIdleConnections()
		}
		require.Equal(t, []string{
			net.JoinHostPort("127.0.0.1", port),
			net.JoinHostPort("127.0.0.1", port),
		}, recorder.dialed(), "两档都只拨了第一跳")
		require.Equal(t, 0, targetHits, "元数据地址上的服务一次都没被碰到")
	})

	t.Run("env_proxy_ignored_in_both", func(t *testing.T) {
		// 环境变量代理会让"校验的地址"与"实际连的地址"分离：两档都**不得**读它。
		for _, key := range []string{
			"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy",
		} {
			t.Setenv(key, "http://127.0.0.1:1")
		}
		t.Setenv("NO_PROXY", "")
		t.Setenv("no_proxy", "")

		var hits int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(server.Close)

		_, port, err := net.SplitHostPort(server.Listener.Addr().String())
		require.NoError(t, err)
		loopback := net.JoinHostPort("127.0.0.1", port)
		resolver := &stubResolver{answers: map[string][]string{}}
		recorder := &recordingDial{}
		guard := NewNetGuard(NetGuardOptions{
			Resolver:    resolver,
			DialContext: recorder.DialContext,
			Exceptions:  func() []core.NetworkException { return ipPortException(loopback) },
		})
		SetNetGuard(guard)
		t.Cleanup(func() { SetNetGuard(nil) })

		for _, profile := range profiles {
			// 同上：URL 是 IP 字面量 + 随机端口，策略面必然拒；本子判据只判"代理变量不生效"。
			policy := newExecutionProfilePolicy(t, profile, "127.0.0.1")
			require.Equal(t, profile, policy.ExecutionProfile())
			url := "http://" + loopback + "/"
			client := newHTTPClient()
			resp, err := client.Get(url)
			require.NoError(t, err, "档位 %s：直连必须成功（代理变量不得生效）", profile)
			require.Equal(t, http.StatusNoContent, resp.StatusCode)
			resp.Body.Close()
			client.CloseIdleConnections()
		}
		require.Equal(t, 2, hits, "两档都直接打到回环服务")
		require.Equal(t, []string{loopback, loopback}, recorder.dialed(),
			"拨的是目标本身，不是环境变量里的代理")
	})

	t.Run("authorization_differs", func(t *testing.T) {
		// 授权分叉：标准档放行未命中禁止域名的公网域名；严格档要求白名单命中。
		standard := newExecutionProfilePolicy(t, core.ProfileStandard)
		requirePolicyAllowed(t, standard, core.Action{
			Type: core.ActionHTTPRequest, Target: "https://not-listed.example.com/", Agent: "demo-agent",
		})
		strict := newExecutionProfilePolicy(t, core.ProfileStrict)
		requirePolicyRefused(t, strict, core.Action{
			Type: core.ActionHTTPRequest, Target: "https://not-listed.example.com/", Agent: "demo-agent",
		}, "域名不在白名单")
		// 白名单命中后严格档同样放行：分叉只在"要不要白名单"。
		strictNamed := newExecutionProfilePolicy(t, core.ProfileStrict, "not-listed.example.com")
		requirePolicyAllowed(t, strictNamed, core.Action{
			Type: core.ActionHTTPRequest, Target: "https://not-listed.example.com/", Agent: "demo-agent",
		})
	})
}
