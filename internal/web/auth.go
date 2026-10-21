package web

// 本文件是管理面认证闸门（模块卡 §3.1.2、约束 2–9）：Host / Origin 守卫先于凭据判定，
// 认证主体（actor）以**显式参数**交给处理函数（宪法 VII）——**不得**写入 context。
// 公开面是闭集（约束 8），只有公开路由绕过本闸门；其余路由一律经 Wrap 包装。
// This file is the management authentication guard: host and origin checks run before
// credential checks, and the actor reaches the handler only as an explicit parameter.
// Only the closed public set bypasses Wrap; every other route goes through it.

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ymedword/basalt/internal/core"
)

// AdminAuthConfig 是认证闸门的装配输入（颗粒度文档 §3.1.2）：取值由 app 层从
// `http.admin_auth` 解析后传入；token 是**已解析**的非空值，不落盘、不回显。
// 缺省白名单（allowed_hosts / allowed_origins）由 app 层按实际监听地址派生后填入。
// AdminAuthConfig carries resolved values; defaults are derived by the app layer from
// the real listeners and filled in before the guard is built.
type AdminAuthConfig struct {
	Mode           string   // token / local_trust
	Token          string   // 已解析的非空值；不落盘、不回显
	AllowedHosts   []string // 缺省由监听地址派生
	AllowedOrigins []string // scheme://host[:port] 全等
	TrustedProxies []string // CIDR
}

// adminHandler 是 server.go 导出类型 AdminHandler 的包内别名：模块卡 §3.1.2 的签名
// 逐字保留，同时让 *authGuard 满足 Options.AdminAuth 的 Wrap 签名（两者必须同一类型）。
// An in-package alias so the card's signature works verbatim and the guard satisfies
// the AdminAuth interface declared in server.go.
type adminHandler = AdminHandler

// 认证模式与信任头的固定字面量（run-and-errors §9.2）。
// Fixed literals of the auth modes and the local-trust header.
const (
	adminAuthModeToken      = "token"
	adminAuthModeLocalTrust = "local_trust"
	localTrustHeader        = "X-Basalt-Local-Trust"

	// 固定公开文案（约束 45）：401 不区分"缺失"与"无效"，403 不解释命中哪条规则；
	// 429 只给固定文案，等待秒数经 Retry-After 头表达（契约 §8 行 413）。
	// Fixed public messages: 401 never distinguishes missing from invalid, 403 never
	// explains which rule matched, and 429 carries the wait in Retry-After only.
	messageUnauthorized    = "管理凭据缺失或无效"
	messageForbidden       = "管理面请求被拒绝"
	messageAuthRateLimited = "认证失败次数过多，请稍后重试"
)

// forwardedHeaderNames 是"任一转发头都不得让 local_trust 放行"的完整清单（约束 3）。
// The complete set of forwarded headers that veto local_trust.
var forwardedHeaderNames = []string{
	"Forwarded",
	"X-Forwarded-For",
	"X-Real-IP",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
}

// authGuard 是认证闸门的运行时状态。构造后只读：白名单与代理网段在装配期一次性
// 归一化，请求路径上不做解析——配置错误必须在启动期暴露，而不是变成每请求 500。
// The guard is read-only after construction: allowlists and proxy networks are
// normalized once at assembly time so configuration mistakes fail at startup.
type authGuard struct {
	cfg       AdminAuthConfig
	tokenHash [sha256.Size]byte
	hosts     map[string]struct{}
	origins   map[string]struct{}
	proxies   []*net.IPNet
	limiter   *authLimiter
	logger    *slog.Logger
	now       func() time.Time
}

// authFailure 是一次认证拒绝的内部描述：rule 只进日志（约束 45 允许日志详细），
// status / code / message 才是响应面——401 与 403 的文案不得随规则变化。
// counted 标记这次拒绝是否计入来源失败限速：只有凭据失败与 local_trust 的 403
// 计入；Host / Origin 的 403 发生在认证之前，不计入（注 14——否则攻击者可用错误
// Host 把正常来源打进 429）。429 自带 retryAfter 供 Retry-After 头使用。
// authFailure separates the log-only rule label from the fixed response surface.
// Only credential failures and local-trust 403s are counted; host and origin rejects
// happen before authentication and never consume the failure budget.
type authFailure struct {
	rule       string
	status     int
	code       string
	message    string
	counted    bool
	retryAfter time.Duration
}

// NewAuthGuard 归一化白名单并构造闸门。配置残缺时返回错误、由启动流程拒绝启动：
// 任何"缺省放行"的解读都会让管理面在没有凭据的情况下裸奔（约束 1）。
// NewAuthGuard normalizes the allowlists; an incomplete configuration is an error,
// never a permissive default.
func NewAuthGuard(cfg AdminAuthConfig, logger *slog.Logger, now func() time.Time) (*authGuard, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	g := &authGuard{
		cfg:     cfg,
		hosts:   make(map[string]struct{}, len(cfg.AllowedHosts)),
		origins: make(map[string]struct{}, len(cfg.AllowedOrigins)),
		limiter: newAuthLimiter(authFailureLimit, authFailureWindow, now),
		logger:  logger,
		now:     now,
	}
	switch cfg.Mode {
	case adminAuthModeToken:
		// 空 token 与未解析占位一律拒绝：前者等于"无认证"，后者说明环境变量没注入。
		// Empty or unresolved tokens are refused: one means no auth at all, the other
		// means the environment variable was never injected.
		if cfg.Token == "" {
			return nil, errors.New("管理认证配置：token 模式缺少已解析的 token")
		}
		if strings.Contains(cfg.Token, "${") {
			return nil, errors.New("管理认证配置：token 仍是未解析的 ${...} 占位")
		}
		g.tokenHash = sha256.Sum256([]byte(cfg.Token))
	case adminAuthModeLocalTrust:
		// token 在本模式下不参与判定；是否存在由 cli 的凭证路径统一把关。
		// The token is unused here; its shape is policed by the loader.
	default:
		return nil, fmt.Errorf(
			"管理认证配置：mode 必须是 %s 或 %s（当前为 %q）",
			adminAuthModeToken, adminAuthModeLocalTrust, cfg.Mode)
	}

	for i, entry := range cfg.AllowedHosts {
		if strings.Contains(entry, "*") {
			return nil, fmt.Errorf("管理认证配置：allowed_hosts 第 %d 项不得使用通配符 *", i+1)
		}
		normalized := normalizeHost(entry)
		if normalized == "" {
			return nil, fmt.Errorf("管理认证配置：allowed_hosts 第 %d 项为空", i+1)
		}
		if _, _, err := net.SplitHostPort(normalized); err != nil {
			return nil, fmt.Errorf(
				"管理认证配置：allowed_hosts 第 %d 项必须写成 host:port（IPv6 写成 [::1]:8080）", i+1)
		}
		g.hosts[normalized] = struct{}{}
	}
	for i, entry := range cfg.AllowedOrigins {
		normalized, ok := normalizeOrigin(entry)
		if !ok {
			return nil, fmt.Errorf(
				"管理认证配置：allowed_origins 第 %d 项非法——必须写成 scheme://host[:port]（无路径、查询、userinfo 或通配）", i+1)
		}
		g.origins[normalized] = struct{}{}
	}
	for i, entry := range cfg.TrustedProxies {
		_, network, err := net.ParseCIDR(strings.TrimSpace(entry))
		if err != nil {
			return nil, fmt.Errorf(
				"管理认证配置：trusted_proxies 第 %d 项不是合法 CIDR——写成 10.0.0.0/8 或 127.0.0.1/32", i+1)
		}
		g.proxies = append(g.proxies, network)
	}
	return g, nil
}

// Wrap 把受保护处理函数包装成标准 http.Handler：Host → Origin → 来源归因 →
// 认证，全部通过后以显式 actor 调用 next。拒绝时写固定信封，绝不调用 next。
// Wrap runs host, origin, source attribution, and authentication in that order and
// calls next only with an authenticated actor.
func (g *authGuard) Wrap(next adminHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := requestIDFrom(r.Context())
		source := sourceOf(r, g.proxies)
		if !g.hostAllowed(r.Host) {
			g.reject(w, requestID, r, source, authFailure{
				rule: "host", status: http.StatusForbidden,
				code: codeForbidden, message: messageForbidden,
			})
			return
		}
		if !g.originAllowed(r.Header.Get("Origin")) {
			g.reject(w, requestID, r, source, authFailure{
				rule: "origin", status: http.StatusForbidden,
				code: codeForbidden, message: messageForbidden,
			})
			return
		}
		limitKey := authLimitKey(source)
		// 429 先于凭据校验（约束 7）：被限速的来源即使带正确 token 也不再进入
		// token 比较，避免"试探一次、看一次响应"的暴力通道。
		// The 429 gate precedes the credential check, so a limited source never
		// reaches the token comparison even with a valid credential.
		if retryAfter, limited := g.limiter.blocked(limitKey); limited {
			g.reject(w, requestID, r, source, authFailure{
				rule: "rate_limited", status: http.StatusTooManyRequests,
				code: codeAuthRateLimited, message: messageAuthRateLimited,
				retryAfter: retryAfter,
			})
			return
		}
		actor, failure := g.authenticate(r, source)
		if failure != nil {
			g.recordFailure(requestID, source, limitKey, *failure)
			g.reject(w, requestID, r, source, *failure)
			return
		}
		// 成功认证**不**调用 limiter.Reset：窗口只在到期时清零（约束 7）。
		// A successful authentication never resets the window.
		next(w, r, actor)
	})
}

// recordFailure 记录计入配额的认证失败；来源刚进入限速时补一条 WARN——下一次
// 请求起将直接 429，排障时需要这一行把"401 风暴"与"开始限速"分开。
// recordFailure counts the failure and logs the transition into the limited state.
func (g *authGuard) recordFailure(requestID, source, limitKey string, failure authFailure) {
	if !failure.counted {
		return
	}
	if retryAfter, limited := g.limiter.Fail(limitKey); limited {
		g.logger.Warn("来源进入认证限速",
			"request_id", requestID,
			"source", safeLogValue(source),
			"retry_after_seconds", retryAfterSeconds(retryAfter))
	}
}

// authenticate 按配置模式判定凭据。未知模式按拒绝处理（构造函数已拦截，这里是
// 手工拼装 authGuard 的兜底）：宁可全拒，也不把零值 tokenHash 当成有效凭据。
// Unknown modes deny by default so a hand-built guard can never accept an empty hash.
func (g *authGuard) authenticate(r *http.Request, source string) (core.Actor, *authFailure) {
	switch g.cfg.Mode {
	case adminAuthModeToken:
		return g.authenticateToken(r, source)
	case adminAuthModeLocalTrust:
		return g.authenticateLocalTrust(r, source)
	default:
		return core.Actor{}, &authFailure{
			rule: "unknown_mode", status: http.StatusUnauthorized,
			code: codeUnauthorized, message: messageUnauthorized, counted: true,
		}
	}
}

// authenticateToken 用 sha256 + 常量时间比较判定 Bearer 凭据：长度与内容都不以
// 提前返回泄露信息；指纹只留前 16 位十六进制，明文 token 不进入 actor。
// Token credentials are compared through sha256 in constant time; the actor carries
// only the first 16 hex characters of the fingerprint.
func (g *authGuard) authenticateToken(r *http.Request, source string) (core.Actor, *authFailure) {
	token, ok := bearerToken(r)
	if !ok {
		return core.Actor{}, &authFailure{
			rule: "credential_missing", status: http.StatusUnauthorized,
			code: codeUnauthorized, message: messageUnauthorized, counted: true,
		}
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(sum[:], g.tokenHash[:]) != 1 {
		return core.Actor{}, &authFailure{
			rule: "credential_invalid", status: http.StatusUnauthorized,
			code: codeUnauthorized, message: messageUnauthorized, counted: true,
		}
	}
	return core.Actor{
		Subject: core.ActorSubjectTokenPrefix + hex.EncodeToString(sum[:])[:16],
		Method:  core.AuthMethodBearer,
		Source:  source,
	}, nil
}

// authenticateLocalTrust 要求三重条件同时成立（约束 3）：直接对端是回环、请求**不带
// 任何**转发头、显式信任头逐字为 "1"。转发头一律否决——反向代理会把公网请求变成
// 回环连接，若只看 peer 就能被同机代理绕过。
// Local trust requires a loopback peer, zero forwarded headers, and the literal trust
// header; any forwarded header vetoes the request because a local reverse proxy can
// otherwise turn a public request into a loopback connection.
func (g *authGuard) authenticateLocalTrust(r *http.Request, source string) (core.Actor, *authFailure) {
	deny := func(rule string) (core.Actor, *authFailure) {
		return core.Actor{}, &authFailure{
			rule: rule, status: http.StatusForbidden,
			code: codeForbidden, message: messageForbidden, counted: true,
		}
	}
	peer, ok := peerIP(r)
	if !ok || !peer.IsLoopback() {
		return deny("peer_not_loopback")
	}
	if hasForwardedHeader(r.Header) {
		return deny("forwarded_header")
	}
	if r.Header.Get(localTrustHeader) != "1" {
		return deny("trust_header_missing")
	}
	return core.Actor{
		Subject: core.ActorSubjectLocalTrust,
		Method:  core.AuthMethodLocalTrust,
		Source:  source,
	}, nil
}

// hostAllowed 按归一化后的 host:port 逐字匹配（大小写不敏感、端口必须一致）。
// hostAllowed matches the normalized host:port exactly, port included.
func (g *authGuard) hostAllowed(raw string) bool {
	_, ok := g.hosts[normalizeHost(raw)]
	return ok
}

// originAllowed 只在请求**带** Origin 时判定：缺失不单独构成放行依据，也不单独
// 构成拒绝理由（非浏览器客户端本就不带）——凭据判定仍在后面（约束 5）。
// An absent Origin neither grants nor denies; credentials are still required.
func (g *authGuard) originAllowed(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	normalized, ok := normalizeOrigin(raw)
	if !ok {
		return false
	}
	_, allowed := g.origins[normalized]
	return allowed
}

// reject 记一条 WARN（规则名、来源与追踪标识只进日志）并写固定失败信封。
// 日志字段经 safeLogValue 投影：转发头是外部输入，不得伪造日志行。
// One WARN line carries the rule, source, and tracing id; the response body stays
// fixed. External inputs are projected before logging.
func (g *authGuard) reject(w http.ResponseWriter, requestID string, r *http.Request, source string, failure authFailure) {
	g.logger.Warn("管理面请求被拒绝",
		"request_id", requestID,
		"method", r.Method,
		"path", safeLogValue(r.URL.Path),
		"rule", failure.rule,
		"status", failure.status,
		"source", safeLogValue(source))
	// 429 必须在写信封前设置 Retry-After：写头只能在 WriteHeader 之前（契约 §8）。
	// Retry-After must be set before the envelope writes the status line.
	if failure.status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(failure.retryAfter), 10))
	}
	writeEnvelope(w, failure.status, Envelope{
		Code: failure.code, Message: failure.message,
		Timestamp: g.now().UTC().Format(time.RFC3339),
	}, g.logger)
}

// ValidateAdminAuth 在监听器创建后校验**依赖真实监听地址**的规则（约束 1 / 3 / 4）：
// mode 必须显式给出；token 模式必须有已解析凭据；非回环监听必须在 allowed_hosts
// 显式列出；local_trust 要求全部监听回环；白名单不得含通配符或非法 CIDR。
// 监听前拒绝启动，避免"默认监听 127.0.0.1 就当已经安全"的部署。
// ValidateAdminAuth checks the listener-dependent rules after the listeners exist:
// explicit mode, resolved token, explicit allowlisting of non-loopback listeners,
// all-loopback local_trust, and well-formed allowlists.
func ValidateAdminAuth(cfg AdminAuthConfig, listeners []string) error {
	switch cfg.Mode {
	case adminAuthModeToken:
		if cfg.Token == "" {
			return errors.New("必填配置项 http.admin_auth.token 缺失——token 模式必须提供 ${VAR} 占位并经环境变量注入")
		}
		if strings.Contains(cfg.Token, "${") {
			return errors.New("配置项 http.admin_auth.token 仍是未解析的 ${...} 占位——先导出对应环境变量")
		}
	case adminAuthModeLocalTrust:
	default:
		return fmt.Errorf(
			"必填配置项 http.admin_auth.mode 缺失或非法（当前为 %q）——只允许 token / local_trust", cfg.Mode)
	}

	allowed := make(map[string]struct{}, len(cfg.AllowedHosts))
	allowedPorts := make(map[string]struct{}, len(cfg.AllowedHosts))
	for i, entry := range cfg.AllowedHosts {
		if strings.Contains(entry, "*") {
			return fmt.Errorf("配置项 http.admin_auth.allowed_hosts 第 %d 项不得使用通配符 *——必须逐项列出 host:port", i+1)
		}
		normalized := normalizeHost(entry)
		host, port, err := net.SplitHostPort(normalized)
		if err != nil || host == "" || port == "" {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_hosts 第 %d 项缺端口或非法——必须写成 host:port（IPv6 写成 [::1]:8080）", i+1)
		}
		allowed[normalized] = struct{}{}
		allowedPorts[port] = struct{}{}
	}
	for i, entry := range cfg.AllowedOrigins {
		if _, ok := normalizeOrigin(entry); !ok {
			return fmt.Errorf(
				"配置项 http.admin_auth.allowed_origins 第 %d 项非法——必须写成 scheme://host[:port]（无路径、查询、userinfo 或通配）", i+1)
		}
	}
	for i, entry := range cfg.TrustedProxies {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(entry)); err != nil {
			return fmt.Errorf(
				"配置项 http.admin_auth.trusted_proxies 第 %d 项不是合法 CIDR——写成 10.0.0.0/8 或 127.0.0.1/32 这样的网段", i+1)
		}
	}

	for i, listener := range listeners {
		host, port, err := net.SplitHostPort(listener)
		if err != nil {
			return fmt.Errorf("监听地址第 %d 项无法解析为 host:port（%q）", i+1, listener)
		}
		loopback := isLoopbackHost(host)
		if cfg.Mode == adminAuthModeLocalTrust && !loopback {
			return fmt.Errorf(
				"http.admin_auth.mode=local_trust 要求全部监听地址回环——监听地址第 %d 项 %q 不是回环", i+1, listener)
		}
		if loopback {
			continue
		}
		if isUnspecifiedHost(host) {
			// 通配监听（0.0.0.0 / ::）本身不是客户端能写进 Host 的值：要求同端口至少
			// 有一条显式白名单，真正的可达主机名 / IP 必须逐条列出。
			// A wildcard bind is not a Host a client can send: require an explicit
			// allowlist entry on the same port instead.
			if _, ok := allowedPorts[port]; !ok {
				return fmt.Errorf(
					"非回环监听 %q 的端口未在 http.admin_auth.allowed_hosts 显式列出", listener)
			}
			continue
		}
		if _, ok := allowed[normalizeHost(listener)]; !ok {
			return fmt.Errorf(
				"非回环监听地址 %q 必须在 http.admin_auth.allowed_hosts 显式列出", listener)
		}
	}
	return nil
}

// normalizeHost 归一 Host：去空白、小写、去 FQDN 尾点，IPv6 取规范形式，端口原样保留。
// 端口不补缺省——"端口必须一致"要求配置里写全，缺端口的条目永远匹配不上。
// normalizeHost lowercases, strips a trailing dot, canonicalizes IPv6, and keeps the
// port exactly as written; a port-less entry therefore never matches.
func normalizeHost(raw string) string {
	host := strings.TrimSpace(raw)
	if host == "" {
		return ""
	}
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return strings.TrimSuffix(strings.ToLower(host), ".")
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	if ip := net.ParseIP(name); ip != nil {
		name = ip.String()
	}
	if strings.Contains(name, ":") {
		return "[" + name + "]:" + port
	}
	return name + ":" + port
}

// normalizeOrigin 把 Origin 归一为 `scheme://host:port`（http/https 补缺省端口）：
// 只接受 scheme + host + port 三部分，路径、查询、userinfo、片段与字面量 `null`
// 一律判非法（约束 5）。返回 ok=false 的条目在装配期即拒绝启动。
// normalizeOrigin accepts exactly scheme://host:port; everything else, including the
// literal "null", is invalid. Default http/https ports are filled in so a browser
// omitting the default port still matches.
func normalizeOrigin(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.EqualFold(value, "null") {
		return "", false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.Opaque != "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	port := parsed.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", false
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host + ":" + port, true
}

// bearerToken 只接受 `Bearer <token>` 形态（scheme 大小写不敏感），空凭据判缺失。
// bearerToken accepts only the Bearer scheme; an empty credential counts as missing.
func bearerToken(r *http.Request) (string, bool) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	const scheme = "bearer "
	if len(raw) <= len(scheme) || !strings.EqualFold(raw[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(raw[len(scheme):])
	if token == "" {
		return "", false
	}
	return token, true
}

// sourceOf 做来源归因（约束 6）：直接对端不在 trusted_proxies 内时**忽略一切转发头**，
// 只回对端地址；在可信网段内时，从右向左取第一个非可信地址（最右非可信 = 代理实际
// 追加的那一跳）。全部跳都可信或没有可用跳时回退到直接对端，不猜更远的地址。
// sourceOf attributes the source: an untrusted peer ignores every forwarded header,
// while a trusted peer takes the rightmost non-trusted hop and falls back to the peer.
func sourceOf(r *http.Request, trusted []*net.IPNet) string {
	peer := strings.TrimSpace(r.RemoteAddr)
	peerAddr := peer
	if host, _, err := net.SplitHostPort(peer); err == nil {
		peerAddr = host
	}
	peerIPAddr := net.ParseIP(strings.Trim(peerAddr, "[]"))
	if peerIPAddr == nil || !ipTrusted(peerIPAddr, trusted) {
		return peer
	}
	hops := forwardedHops(r)
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(hops[i])
		if ip == nil || ipTrusted(ip, trusted) {
			continue
		}
		return ip.String()
	}
	return peer
}

// forwardedHops 抽取转发链路候选地址：X-Forwarded-For 优先，其次 X-Real-IP，
// 最后按 RFC 7239 解析 Forwarded 的 `for=` 参数。
// forwardedHops prefers X-Forwarded-For, then X-Real-IP, then RFC 7239 Forwarded.
func forwardedHops(r *http.Request) []string {
	if raw := r.Header.Get("X-Forwarded-For"); strings.TrimSpace(raw) != "" {
		parts := strings.Split(raw, ",")
		hops := make([]string, 0, len(parts))
		for _, part := range parts {
			hops = append(hops, cleanHop(part))
		}
		return hops
	}
	if raw := r.Header.Get("X-Real-IP"); strings.TrimSpace(raw) != "" {
		return []string{cleanHop(raw)}
	}
	if raw := r.Header.Get("Forwarded"); strings.TrimSpace(raw) != "" {
		var hops []string
		for _, element := range strings.Split(raw, ",") {
			for _, param := range strings.Split(element, ";") {
				key, value, ok := strings.Cut(param, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "for") {
					continue
				}
				hops = append(hops, cleanHop(value))
			}
		}
		return hops
	}
	return nil
}

// cleanHop 去掉引号、方括号与端口，只留地址本身（IPv6 裸地址原样保留）。
// cleanHop strips quotes, brackets, and ports, leaving the bare address.
func cleanHop(raw string) string {
	hop := strings.Trim(strings.TrimSpace(raw), `"`)
	if host, _, err := net.SplitHostPort(hop); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(hop, "[]")
}

// hasForwardedHeader 判"请求是否携带任一转发头"：存在即否决 local_trust，
// 空值也算（头存在本身就是"经过了转发"的证据）。
// Header presence alone vetoes local trust, even with an empty value.
func hasForwardedHeader(header http.Header) bool {
	for _, name := range forwardedHeaderNames {
		if _, ok := header[textproto.CanonicalMIMEHeaderKey(name)]; ok {
			return true
		}
	}
	return false
}

// peerIP 取直接对端 IP：只解析 RemoteAddr，**不**看任何转发头。
// peerIP parses the direct peer address only.
func peerIP(r *http.Request) (net.IP, bool) {
	host := strings.TrimSpace(r.RemoteAddr)
	if split, _, err := net.SplitHostPort(host); err == nil {
		host = split
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return nil, false
	}
	return ip, true
}

// ipTrusted 判某地址是否落在可信代理网段内。
// ipTrusted reports whether the address belongs to a trusted proxy network.
func ipTrusted(ip net.IP, trusted []*net.IPNet) bool {
	for _, network := range trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// isLoopbackHost 判监听 host 是否回环：`localhost` 字面量与一切回环 IP。
// isLoopbackHost accepts the literal "localhost" and every loopback IP.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isUnspecifiedHost 判通配监听（空 host / 0.0.0.0 / ::）。
// isUnspecifiedHost reports a wildcard bind.
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// safeLogValue 把外部输入投影成单行日志值：控制字符（含 CR / LF）替换为 `?`，
// 超长截断——转发头与路径都可能被构造来伪造日志行。
// safeLogValue projects external input into a single log-safe line.
func safeLogValue(raw string) string {
	if raw == "" {
		return ""
	}
	const maxRunes = 128
	var out strings.Builder
	count := 0
	for _, r := range raw {
		if count >= maxRunes {
			out.WriteString("…")
			break
		}
		if unicode.IsControl(r) {
			out.WriteByte('?')
		} else {
			out.WriteRune(r)
		}
		count++
	}
	return out.String()
}
