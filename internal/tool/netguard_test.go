package tool

// 本文件覆盖 14D D12「受控拨号」：解析结果与 `DialContext` 实拨地址一致（打桩记录实拨
// IP）、双栈任一结果受保护即拒、`Host` 与 TLS SNI 仍是原域名（证书按域名校验）、连接
// 不跨目标复用、3xx 不跟随、解析失败即拒，以及**不经环境变量代理**。
//
// 判据纪律：解析器与拨号器都是注入的替身——不打真实 DNS、不连外网；唯一接到的真实
// 套接字是 `httptest` 在本机起的服务，而本机地址恰好是受保护段（127.0.0.0/8），所以
// 每个放行路径都显式登记一条 `ip_port` 例外：这既让服务可达，又顺带判了"例外按 IP +
// 端口逐字匹配"。
// Both the resolver and the dialer are injected stubs; the only real sockets are local
// httptest servers, which sit in a protected range and therefore go through the
// exception path.
//
// The production guard is installed into a package-level pointer: every subtest that
// touches it resets it in cleanup so other tests see the bare-test-double fallback.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// stubResolver 是确定性的地址表替身：没登记的域名按 NXDOMAIN 失败，绝不回落到真实
// 解析器（回落会让判据依赖本机 DNS）。
// stubResolver answers from a fixed table and fails NXDOMAIN otherwise.
type stubResolver struct {
	answers map[string][]string
	err     error

	mu    sync.Mutex
	asked []string
}

func (s *stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	s.mu.Lock()
	s.asked = append(s.asked, host)
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	raw, ok := s.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	addrs := make([]net.IPAddr, 0, len(raw))
	for _, one := range raw {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(one)})
	}
	return addrs, nil
}

func (s *stubResolver) askedHosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// recordingDial 记录守门器**实际拨出**的地址；`fail` 非空时只记录不建连（判"没有
// 拨出任何连接"的用例不需要真套接字）。
// recordingDial records the address the guard actually dials.
type recordingDial struct {
	fail error

	mu    sync.Mutex
	addrs []string
}

func (d *recordingDial) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, address)
	d.mu.Unlock()
	if d.fail != nil {
		return nil, d.fail
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
}

func (d *recordingDial) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// ipPortException 造一条 `ip:port` 例外（守门器按 IP + 端口逐字匹配）。
// ipPortException builds one ip_port exception for the guard.
func ipPortException(value string) []core.NetworkException {
	return []core.NetworkException{{Kind: core.NetworkExceptionKindIPPort, Value: value}}
}

// selfSignedCertFor 为指定域名签发一张自签服务器证书：证书只对**域名**有效，因此
// 客户端能验过证书，就证明 TLS 握手里用的是域名（SNI）而不是解析出的 IP。
// selfSignedCertFor issues a certificate valid only for the given DNS name.
func selfSignedCertFor(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// TestNetGuard_DialMatchesResolution 覆盖 D12 的判定面：实拨地址 = 已校验地址、
// 双栈任一受保护即拒、元数据地址例外也拒、例外按 IP + 端口放行、解析失败即拒、
// `Host` / SNI 保持域名、3xx 不跟随、每个消费者一份传输（不跨目标复用）。
// TestNetGuard_DialMatchesResolution covers the D12 judging face.
func TestNetGuard_DialMatchesResolution(t *testing.T) {
	t.Run("dials_the_validated_address_only", func(t *testing.T) {
		resolver := &stubResolver{answers: map[string][]string{
			"public.example": {"203.0.113.10", "203.0.113.11"},
		}}
		stop := errors.New("test dial stop")
		recorder := &recordingDial{fail: stop}
		guard := NewNetGuard(NetGuardOptions{Resolver: resolver, DialContext: recorder.DialContext})

		_, err := guard.DialContext(context.Background(), "tcp", "public.example:443")
		require.ErrorIs(t, err, stop, "拨号必须真的发生（由替身拒绝收尾）")
		require.Equal(t, []string{"public.example"}, resolver.askedHosts(), "解析的是原域名")
		require.Equal(t, []string{"203.0.113.10:443"}, recorder.dialed(),
			"实拨地址必须是解析结果里的那一个 IP，不是域名（域名在拨号层无法解析自定义端口/替身）")
	})

	t.Run("any_protected_address_refuses_the_host", func(t *testing.T) {
		resolver := &stubResolver{answers: map[string][]string{
			"mixed.example": {"198.51.100.5", "10.1.2.3"},
		}}
		recorder := &recordingDial{}

		guard := NewNetGuard(NetGuardOptions{Resolver: resolver, DialContext: recorder.DialContext})
		_, err := guard.DialContext(context.Background(), "tcp", "mixed.example:80")

		var violation *core.SandboxViolationError
		require.ErrorAs(t, err, &violation)
		require.Equal(t, core.DenialExceptionMissing, violation.Reason,
			"受保护地址但没有匹配例外 → exception_missing")
		require.Empty(t, recorder.dialed(), "双栈里只要有一个受保护地址，整只域名都不拨")
	})

	t.Run("cloud_metadata_is_refused_even_with_an_exception", func(t *testing.T) {
		resolver := &stubResolver{answers: map[string][]string{
			"metadata.example": {"169.254.169.254"},
		}}
		recorder := &recordingDial{}
		guard := NewNetGuard(NetGuardOptions{
			Resolver:    resolver,
			DialContext: recorder.DialContext,
			Exceptions: func() []core.NetworkException {
				return ipPortException("169.254.169.254:80")
			},
		})

		_, err := guard.DialContext(context.Background(), "tcp", "metadata.example:80")
		var violation *core.SandboxViolationError
		require.ErrorAs(t, err, &violation)
		require.Equal(t, core.DenialProtectedAddress, violation.Reason,
			"元数据地址在任何档位与例外下都不放行")
		require.Empty(t, recorder.dialed())
	})

	t.Run("exception_allows_the_matching_ip_and_port", func(t *testing.T) {
		resolver := &stubResolver{answers: map[string][]string{
			"loopback.example": {"127.0.0.1"},
		}}
		stop := errors.New("test dial stop")
		recorder := &recordingDial{fail: stop}
		guard := NewNetGuard(NetGuardOptions{
			Resolver:    resolver,
			DialContext: recorder.DialContext,
			Exceptions: func() []core.NetworkException {
				return ipPortException("127.0.0.1:8080")
			},
		})

		_, err := guard.DialContext(context.Background(), "tcp", "loopback.example:8080")
		require.ErrorIs(t, err, stop)
		require.Equal(t, []string{"127.0.0.1:8080"}, recorder.dialed())

		// 端口不匹配的例外不放行：例外是 `ip:port` 逐字匹配，不是"这个 IP 随便连"。
		_, err = guard.DialContext(context.Background(), "tcp", "loopback.example:8081")
		var violation *core.SandboxViolationError
		require.ErrorAs(t, err, &violation)
		require.Equal(t, core.DenialExceptionMissing, violation.Reason)
		require.Equal(t, []string{"127.0.0.1:8080"}, recorder.dialed())
	})

	t.Run("resolve_failure_is_refused", func(t *testing.T) {
		resolver := &stubResolver{err: &net.DNSError{Err: "server misbehaving", Name: "broken.example"}}
		recorder := &recordingDial{}
		guard := NewNetGuard(NetGuardOptions{Resolver: resolver, DialContext: recorder.DialContext})

		_, err := guard.DialContext(context.Background(), "tcp", "broken.example:443")
		var violation *core.SandboxViolationError
		require.ErrorAs(t, err, &violation)
		require.Equal(t, core.DenialProtectedAddress, violation.Reason,
			"解析失败按受保护地址拒——不猜一个地址")
		require.Empty(t, recorder.dialed())
	})

	t.Run("host_and_sni_stay_the_domain", func(t *testing.T) {
		const domain = "public.example"
		cert, pool := selfSignedCertFor(t, domain)

		var (
			mu      sync.Mutex
			gotHost string
			gotSNI  string
		)
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gotHost = r.Host
			if r.TLS != nil {
				gotSNI = r.TLS.ServerName
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		server.StartTLS()
		t.Cleanup(server.Close)

		_, port, err := net.SplitHostPort(server.Listener.Addr().String())
		require.NoError(t, err)
		resolver := &stubResolver{answers: map[string][]string{domain: {"127.0.0.1"}}}
		guard := NewNetGuard(NetGuardOptions{
			Resolver: resolver,
			// 本机地址在受保护段：登记一条 ip_port 例外才可能连上（真实放行路径）。
			Exceptions: func() []core.NetworkException {
				return ipPortException(net.JoinHostPort("127.0.0.1", port))
			},
		})
		transport := guard.Transport()
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
		client := &http.Client{Transport: transport}
		t.Cleanup(client.CloseIdleConnections)

		resp, err := client.Get("https://" + net.JoinHostPort(domain, port) + "/health")
		require.NoError(t, err, "证书只对域名有效：验过 → 握手用的是域名（SNI），不是 IP")
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, net.JoinHostPort(domain, port), gotHost, "Host 头保持原域名（含端口）")
		require.Equal(t, domain, gotSNI, "TLS SNI 保持原域名")
	})

	t.Run("redirects_are_not_followed", func(t *testing.T) {
		var targetHits int32
		var mu sync.Mutex
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			mu.Lock()
			targetHits++
			mu.Unlock()
		}))
		t.Cleanup(target.Close)
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://metadata.example/latest/meta-data/", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redirector.Close)

		_, port, err := net.SplitHostPort(redirector.Listener.Addr().String())
		require.NoError(t, err)
		resolver := &stubResolver{answers: map[string][]string{
			"redirecting.example": {"127.0.0.1"},
			// 重定向目标解析到云元数据地址：跟过去必然被拒，判据是"根本不去解析/拨它"。
			"metadata.example": {"169.254.169.254"},
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
		resp, err := client.Get("http://" + net.JoinHostPort("redirecting.example", port) + "/")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode, "3xx 原样返回，由逐跳循环重判")
		require.Equal(t, []string{net.JoinHostPort("127.0.0.1", port)}, recorder.dialed(),
			"只有第一跳被拨出：重定向目标一次解析、一次拨号都没有")
		mu.Lock()
		require.Equal(t, int32(0), targetHits)
		mu.Unlock()
	})

	t.Run("each_consumer_gets_its_own_transport", func(t *testing.T) {
		guard := NewNetGuard(NetGuardOptions{Resolver: &stubResolver{}})
		first := guard.Transport()
		second := guard.Transport()
		require.NotSame(t, first, second, "每次构造一份传输：连接池不跨目标、也不跨消费者复用")
		require.Nil(t, first.Proxy)
		require.Nil(t, second.Proxy)
	})
}

// TestNetGuard_NoEnvProxy 覆盖 D12 的代理面：`HTTP_PROXY` / `HTTPS_PROXY` / `ALL_PROXY`
// 存在时，沙箱出站客户端（`newHTTPClient` / `newMCPHTTPClient`）**不**读它们——请求
// 直连到解析并分类过的地址；未装守门器的裸替身同样退回"无代理的默认传输"。
// TestNetGuard_NoEnvProxy proves the outbound clients never read environment proxies.
func TestNetGuard_NoEnvProxy(t *testing.T) {
	// 环境代理指向一个必定连不上的端口：只要有人读了它，请求就会失败。
	// The env proxies point at a closed port: any read of them fails the request.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	const domain = "direct.example"
	var (
		mu        sync.Mutex
		gotHosts  []string
		requestsN int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHosts = append(gotHosts, r.Host)
		requestsN++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)

	resolver := &stubResolver{answers: map[string][]string{domain: {"127.0.0.1"}}}
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

	target := "http://" + net.JoinHostPort(domain, port) + "/probe"
	for name, client := range map[string]*http.Client{
		"http_get": newHTTPClient(),
		"mcp_http": newMCPHTTPClient(nil),
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := client.Get(target)
			require.NoError(t, err, "%s 不得读环境变量代理", name)
			resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
		})
		client.CloseIdleConnections()
	}

	mu.Lock()
	require.Equal(t, []string{net.JoinHostPort(domain, port), net.JoinHostPort(domain, port)}, gotHosts,
		"两个客户端都把原域名带进了 Host")
	require.Equal(t, 2, requestsN)
	mu.Unlock()
	require.Equal(t, []string{
		net.JoinHostPort("127.0.0.1", port),
		net.JoinHostPort("127.0.0.1", port),
	}, recorder.dialed(), "实拨的是校验过的 IP；环境代理（127.0.0.1:1）一次都没被拨")

	// 未装守门器的裸替身：`guardedTransport` 退回**无代理**的默认传输——环境变量
	// 存在时也不改出口（生产装配必装守门器，这条只判退化路径本身）。
	// The bare test double still ignores environment proxies.
	SetNetGuard(nil)
	t.Cleanup(func() { SetNetGuard(nil) })
	require.Nil(t, guardedTransport().Proxy)
	fallback := newHTTPClient()
	t.Cleanup(fallback.CloseIdleConnections)
	resp, err := fallback.Get(server.URL)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
