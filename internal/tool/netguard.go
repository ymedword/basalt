package tool

// 本文件是受控拨号（设计 §2.2.2「网络·解析 / 连接固定 / 代理」与 §2.2.7 的落点）：
// 出站连接在建连**之前**解析全部 A / AAAA，**任一**结果落在受保护地址段即拒；放行时
// 只拨那个已校验的 IP，`Host` 与 TLS SNI 仍用原域名（证书校验因此仍覆盖域名）。
//
// 三条不能省的约束：
//   - **不**读 `HTTP_PROXY` / `HTTPS_PROXY`：环境变量代理会让"校验的地址"与"实际连的
//     地址"分离。需经代理出网时只能显式配置 `http.proxy`——此时固定的是代理地址、目标
//     由代理解析，页面在同一处标注"目标地址不由本进程固定"。
//   - **不**设"连接后校验对端 IP"的降级：连接后的检查发生在请求与认证材料发出之后，
//     挡不住已经发出的东西。
//   - 解析失败即拒（DNS 不可用与被禁止不区分）：宁可拒一次，也不猜一个地址。
// The guarded dialer resolves every A/AAAA record before connecting, refuses if any
// result is protected, and dials only the validated IP with the original Host/SNI.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 受控拨号的拒绝文案固定片段（四段式的段 1 / 段 4；段 2 由调用方给安全形态的目标）。
// Frozen refusal copy fragments for the guarded dialer.
const (
	ruleProtectedAddress = "目标解析落在受保护地址段"
	// guidanceProtectedAddress 指向例外表：受保护段**可以**经例外放行，指引必须给出那条路。
	guidanceProtectedAddress = "内网与保留地址默认拒绝；确需访问请由管理员在管理台「执行环境」页登记访问例外（理由必填），保存后立即生效。"
	ruleCloudMetadata        = "目标是云元数据地址"
	guidanceCloudMetadata    = "云元数据地址任何档位与例外都不放行，没有配置键可以加。"
	ruleResolveFailed        = "目标域名无法解析"
	guidanceResolveFailed    = "解析失败即拒（DNS 不可用与被禁止不作区分）；请确认域名拼写与网络配置后重试。"
	ruleDialRefused          = "连接未能建立"
	guidanceDialRefused      = "已校验的地址仍无法建连；请确认目标可达后重试。"
)

// AddressResolver 解析一个主机名的全部地址；生产实现是 `net.DefaultResolver`。
// AddressResolver resolves every address of a host; the production value is the
// standard resolver, and tests inject a deterministic table.
type AddressResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// NetGuardOptions 是受控拨号的装配参数。`Exceptions` 是**活快照**函数（每次拨号读
// 当前例外集合）：例外删除后下一次判定即失效，不需要重建客户端。
// Exceptions is a live snapshot function so a deleted exception stops applying at the
// very next dial without rebuilding clients.
type NetGuardOptions struct {
	Resolver    AddressResolver
	Exceptions  func() []core.NetworkException
	ProxyURL    *url.URL
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
	// PreferIPv6 只影响测试口径（双栈时优先拨哪一族）；生产保持 false = 按解析顺序。
	PreferIPv6 bool
}

// NetGuard 是有状态的受控拨号器：无锁读例外快照，代理地址在装配期固定。
// NetGuard holds no per-request state; the proxy address is fixed at assembly time.
type NetGuard struct {
	resolver   AddressResolver
	exceptions func() []core.NetworkException
	proxy      *url.URL
	proxyAddr  string
	dial       func(ctx context.Context, network, address string) (net.Conn, error)
	preferIPv6 bool
}

// 编译期断言：拨号签名与 `net.Dialer.DialContext` 一致，可直接装进 `http.Transport`。
// Compile-time assertion that the signature plugs into http.Transport.
var _ func(ctx context.Context, network, address string) (net.Conn, error) = (*NetGuard)(nil).DialContext

// NewNetGuard 装配受控拨号；缺省解析器是标准库，缺省拨号器是 `net.Dialer`。
// NewNetGuard wires the guard with the standard resolver and dialer by default.
func NewNetGuard(opts NetGuardOptions) *NetGuard {
	guard := &NetGuard{
		resolver:   opts.Resolver,
		exceptions: opts.Exceptions,
		proxy:      opts.ProxyURL,
		dial:       opts.DialContext,
		preferIPv6: opts.PreferIPv6,
	}
	if guard.resolver == nil {
		guard.resolver = net.DefaultResolver
	}
	if guard.dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		guard.dial = dialer.DialContext
	}
	if opts.ProxyURL != nil {
		// 显式代理：固定的是代理地址；目标域名交给代理解析（页面同处标注）。
		guard.proxyAddr = opts.ProxyURL.Host
	}
	return guard
}

// DialContext 是 `http.Transport.DialContext` 的实现：解析 → 分类 → 拨已校验的 IP。
// DialContext resolves, classifies, and dials the validated address only.
func (g *NetGuard) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if g == nil {
		// 未装配守门器的裸客户端（只可能出现在包内直构的测试替身里）：按无额外
		// 规则直拨，**不**在此处放宽任何生产路径——生产装配必装守门器。
		return (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, network, address)
	}
	if g.proxy != nil {
		// 代理档：只校验并固定代理主机；目标由代理解析（不再本地解析目标）。
		target, err := g.resolveProxy(address)
		if err != nil {
			return nil, err
		}
		return g.dial(ctx, network, target)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, &core.SandboxViolationError{
			Rule: ruleResolveFailed, Target: targetUnresolvable, Guidance: guidanceResolveFailed,
			Reason: core.DenialProtectedAddress,
		}
	}
	ips, err := g.lookup(ctx, host)
	if err != nil {
		// 解析失败即拒：**不**回落系统解析器、**不**用缓存猜一个地址。
		return nil, &core.SandboxViolationError{
			Rule: ruleResolveFailed, Target: host, Guidance: guidanceResolveFailed,
			Reason: core.DenialProtectedAddress,
		}
	}
	chosen, refusal := g.classify(host, port, ips)
	if refusal != nil {
		return nil, refusal
	}
	conn, dialErr := g.dial(ctx, network, net.JoinHostPort(chosen.String(), port))
	if dialErr != nil {
		// 建连失败只回安全类别与域名：底层错误可能带地址细节，但不含凭证，仍按
		// 外部错误面上抛，由调用方决定分类。
		return nil, dialErr
	}
	return conn, nil
}

// lookup 解析并规整地址列表（`net.IPAddr` → `net.IP`，去掉 IPv6 作用域）。
// lookup normalizes the resolver output to plain IPs.
func (g *NetGuard) lookup(ctx context.Context, host string) ([]net.IP, error) {
	// IP 字面量目标不解析：白名单按域名匹配，IP 字面量在 `Policy.Check` 已拒；这里
	// 再兜一次（MCP 配置面可能绕过 Policy 直接建连）。
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if ip := addr.IP; ip != nil {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("resolve: empty answer")
	}
	if g.preferIPv6 {
		// 测试口径：优先 IPv6 只改排列顺序，不改"任一受保护即拒"的判定。
		sortIPsIPv6First(ips)
	}
	return ips, nil
}

// classify 判一遍解析结果：任一地址受保护且**不能**被例外放行即拒；放行时返回第一个
// 可拨地址。云元数据地址例外也不能放行。
// classify refuses when any resolved address is protected without a matching exception.
func (g *NetGuard) classify(host, port string, ips []net.IP) (net.IP, error) {
	var chosen net.IP
	for _, ip := range ips {
		protected, metadata := core.ClassifyAddress(ip)
		if !protected {
			if chosen == nil {
				chosen = ip
			}
			continue
		}
		if metadata {
			// 云元数据地址：任何档位与例外都不放行（判定在例外之**前**）。
			return nil, &core.SandboxViolationError{
				Rule: ruleCloudMetadata, Target: host, Guidance: guidanceCloudMetadata,
				Reason: core.DenialProtectedAddress,
			}
		}
		if !g.excepted(host, ip, port) {
			return nil, &core.SandboxViolationError{
				Rule: ruleProtectedAddress, Target: host, Guidance: guidanceProtectedAddress,
				Reason: core.DenialExceptionMissing,
			}
		}
		// 有例外的受保护地址：可用；只有没有任何公网地址时才选它。
		if chosen == nil {
			chosen = ip
		}
	}
	if chosen == nil {
		return nil, &core.SandboxViolationError{
			Rule: ruleDialRefused, Target: host, Guidance: guidanceDialRefused,
			Reason: core.DenialProtectedAddress,
		}
	}
	return chosen, nil
}

// excepted 判定是否有生效例外覆盖"这个域名解析到这个地址的这个端口"：`host` 例外按
// 白名单同一套通配语法匹配域名；`ip_port` 例外按 IP + 端口逐字匹配。
// excepted matches either a host exception or an ip_port exception for the resolved pair.
func (g *NetGuard) excepted(host string, ip net.IP, port string) bool {
	if g.exceptions == nil {
		return false
	}
	portNum, _ := strconv.Atoi(port)
	for _, exception := range g.exceptions() {
		switch exception.Kind {
		case core.NetworkExceptionKindHost:
			if core.DomainEntryMatches(exception.Value, host) {
				return true
			}
		case core.NetworkExceptionKindIPPort:
			otherHost, otherPort, err := net.SplitHostPort(exception.Value)
			if err != nil {
				continue
			}
			otherIP := net.ParseIP(otherHost)
			otherNum, convErr := strconv.Atoi(otherPort)
			if otherIP == nil || convErr != nil {
				continue
			}
			if otherIP.Equal(ip) && otherNum == portNum {
				return true
			}
		}
	}
	return false
}

// resolveProxy 解析代理主机（代理地址在装配期已固定，这里只做解析与固定）。
// resolveProxy pins the proxy address; the target is resolved by the proxy itself.
func (g *NetGuard) resolveProxy(address string) (string, error) {
	host, port, err := net.SplitHostPort(g.proxyAddr)
	if err != nil {
		return "", &core.SandboxViolationError{
			Rule: ruleResolveFailed, Target: targetUnresolvable, Guidance: guidanceResolveFailed,
			Reason: core.DenialProtectedAddress,
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		return net.JoinHostPort(ip.String(), port), nil
	}
	ips, lookupErr := g.lookup(context.Background(), host)
	if lookupErr != nil || len(ips) == 0 {
		return "", &core.SandboxViolationError{
			Rule: ruleResolveFailed, Target: host, Guidance: guidanceResolveFailed,
			Reason: core.DenialProtectedAddress,
		}
	}
	_ = address
	return net.JoinHostPort(ips[0].String(), port), nil
}

// Transport 造一份受控 `*http.Transport`：`Proxy` 只认显式配置（**不**读环境变量），
// `DialContext` 是受控拨号，其余沿用 DefaultTransport 的默认值。每个消费者拿一份
// 独立实例——连接池按目标分池，不跨目标复用。
// Transport builds a guarded transport; Proxy is explicit-only and never env-driven.
func (g *NetGuard) Transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = g.DialContext
	if g != nil && g.proxy != nil {
		transport.Proxy = http.ProxyURL(g.proxy)
	} else {
		transport.Proxy = nil
	}
	return transport
}

// 进程级守门器：装配层装一次，所有本包内造的出站客户端都经它拨号。
// `nil` 表示未装配（只可能出现在直构测试替身里）。
// The process-wide guard installed once at assembly; nil only in bare test doubles.
var processNetGuard atomic.Pointer[NetGuard]

// SetNetGuard 装配进程级受控拨号。**必须**在任何出站客户端被构造之前调用；重复调用
// 以最后一次为准（装配只有一次，重复只出现在测试里）。
// SetNetGuard installs the process-wide guard before any outbound client is built.
func SetNetGuard(guard *NetGuard) { processNetGuard.Store(guard) }

// currentNetGuard 读进程级守门器；未装配时为 nil。
// currentNetGuard returns the installed guard or nil.
func currentNetGuard() *NetGuard { return processNetGuard.Load() }

// guardedTransport 造一份受控传输：已装守门器时用它，否则退回"无代理的默认传输"
// （直构测试替身场景；生产装配必装守门器）。
// guardedTransport prefers the installed guard and otherwise falls back to a
// proxy-free default transport for bare test doubles.
func guardedTransport() *http.Transport {
	if guard := currentNetGuard(); guard != nil {
		return guard.Transport()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}

// sortIPsIPv6First 把 IPv6 地址排到前面（稳定：同族保持解析顺序）。
// sortIPsIPv6First stably moves IPv6 addresses to the front.
func sortIPsIPv6First(ips []net.IP) {
	v6 := make([]net.IP, 0, len(ips))
	v4 := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if ip.To4() == nil {
			v6 = append(v6, ip)
			continue
		}
		v4 = append(v4, ip)
	}
	copy(ips, append(v6, v4...))
}
