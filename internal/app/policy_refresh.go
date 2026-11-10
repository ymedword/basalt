package app

// 本文件实现 US-25C §2.3.6 的**运行期策略刷新**（G1）：四类策略（允许域、拒绝域、网络
// 例外、执行档位）按数据库权威定期重新装载，刷新整轮单飞，摘要取自**本轮实际应用的那份
// 快照**。三条不变量贯穿全文件：
//
//  1. **应用与摘要同源**（C5）：每个服务在自己的 `mu` 内读表 → 应用 → 用这份行算子摘要。
//     **不得**先做一次"发现读"再用它的摘要代表应用结果——数据库一旦回到与发现读相同的
//     内容，比较就会相等并永久跳过，被撤掉的策略再也不会从内存里消失；
//  2. **过期与内容版本分开**（C6）：`lastVerifiedAt` 只描述「最近一轮四个服务全部成功」。
//     内容长期不变 + 刷新持续成功必须始终放行；只有读失败让时间戳停在原地，随实际时间
//     推移越过 `policy.max_stale` 后准入才返回 `POLICY_STALE`；
//  3. **构造不启动**：`NewPolicyRefresher` 只装配；启动期同步一次 `RefreshOnce` 与后台
//     `Run` 的时机归装配层（§2.3.1），停机时 `Run` 随 ctx 取消返回、join 在协程内完成，
//     协调器随后才关库。
//
// This file implements the runtime-policy refresh: four services reload under their own
// locks, digests come from the exact rows applied in this round, the whole round is
// single-flight, and freshness follows the last fully successful round only.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// defaultPolicyMaxStale 是准入新鲜度门的兜底阈值：装配未给 `policy.max_stale` 时按
// `cli.DefaultPolicyConfig()` 的 300s 判过期。配置装载期已校验为正数，走到这个兜底
// 只说明调用方是轻装配（测试 / 直接构造），不是生产配置。
// The fallback staleness bound mirrors cli.DefaultPolicyConfig; a real deployment always
// injects the configured value.
const defaultPolicyMaxStale = 300 * time.Second

// PolicyFreshness 是准入路径读取策略新鲜度的**只读**面（FR-25C-16 / §2.3.6）：
// `LastVerified` 返回最近一次四个策略服务全部成功核对的时刻，零值表示从未成功过。
// 准入只读它、不触发刷新——刷新归后台 `PolicyRefresher.Run` 与启动期同步一轮。
// `nil` 接口 = 不设门：`chat` / `tool` 两条轻路径是单进程、启动时装载策略，不存在跨
// 进程漂移，**不得**让一条长时间运行的 `chat` 因几分钟没刷新而被判过期。
// PolicyFreshness is the read-only freshness face admission consults; a nil interface
// disables the gate, which is what the single-process chat/tool paths require.
type PolicyFreshness interface {
	LastVerified() time.Time
}

// PolicyRefresherDeps 是策略刷新器的装配输入（§3.1.4）：四个数据源各是某类策略服务的
// 一致快照刷新端口（`core.PolicyDigestSource`），`RevokeHTTP` 在任一步内容变化时收紧
// 已建立的 MCP http 连接。`Interval` 取 `policy.sync_interval`、`MaxStale` 取
// `policy.max_stale`；两者都由配置装载期校验为正。
// PolicyRefresherDeps wires the four digest sources, the connection tightening hook and
// the two policy timing knobs.
type PolicyRefresherDeps struct {
	Allowlist  core.PolicyDigestSource
	Denylist   core.PolicyDigestSource
	Exceptions core.PolicyDigestSource
	Execution  core.PolicyDigestSource
	// RevokeHTTP 让 supervisor 按新规则重判已建立的 http 连接，返回被撤销的连接数。
	// 为 nil 表示本装配没有 MCP http 连接可收紧（只读面 / 轻装配）。
	RevokeHTTP func() int
	Logger     *slog.Logger
	Now        func() time.Time
	Interval   time.Duration
	MaxStale   time.Duration
}

// PolicyRefresher 串行执行策略刷新（整轮单飞）：`running` 是单飞闸，`appliedDigest`
// 与 `lastVerified` 是准入与刷新器共享的读数（读侧原子，写侧只在单飞临界区内）。
// 并发 tick 只跳过本次触发、不排队——下一拍会重新尝试，失败不需要跨轮重试。
// PolicyRefresher runs one round at a time; concurrent ticks skip rather than queue,
// because the next tick retries anyway.
type PolicyRefresher struct {
	deps PolicyRefresherDeps

	running       atomic.Bool
	appliedDigest atomic.Pointer[string]
	lastVerified  atomic.Pointer[time.Time]
}

// 编译期断言：刷新器就是准入读取的新鲜度面。
// Compile-time assertion that the refresher is the freshness face admission reads.
var _ PolicyFreshness = (*PolicyRefresher)(nil)

// NewPolicyRefresher 装配刷新器：缺省 logger 丢弃、缺省时钟取系统时间。**不**启动任何
// 循环——构造不启动是 §2.3.1 的装配纪律。
// NewPolicyRefresher wires the refresher and starts nothing.
func NewPolicyRefresher(deps PolicyRefresherDeps) *PolicyRefresher {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &PolicyRefresher{deps: deps}
}

// LastVerified 返回最近一次四个策略服务**全部成功**的核对时刻；零值 = 从未成功过。
// 失败轮不推进它（否则一次读失败会被洗成「刚核对过」），因此它同时是准入的过期判据
// 与运维判断「该等刷新还是该查数据库」的唯一线索。
// LastVerified never advances on a failed round; zero means the policy was never
// verified in this process.
func (r *PolicyRefresher) LastVerified() time.Time {
	if r == nil {
		return time.Time{}
	}
	if verified := r.lastVerified.Load(); verified != nil {
		return *verified
	}
	return time.Time{}
}

// policySource 把数据源与它的固定名字绑在一起：失败时按名字点名服务，组合摘要按
// **固定顺序**取子摘要（顺序是摘要协议的一部分，不得按 map 遍历决定）。
// policySource binds a digest source to its fixed name and position.
type policySource struct {
	name   string
	source core.PolicyDigestSource
}

// policySources 返回四个数据源的固定顺序（§2.3.6：allowlist / denylist /
// execution / exceptions）。顺序变化等于摘要协议变化，必须与模块卡同批修订。
// The order is part of the digest protocol and must match the module card.
func (r *PolicyRefresher) policySources() [4]policySource {
	return [4]policySource{
		{name: "allowlist", source: r.deps.Allowlist},
		{name: "denylist", source: r.deps.Denylist},
		{name: "execution", source: r.deps.Execution},
		{name: "exceptions", source: r.deps.Exceptions},
	}
}

// RefreshOnce 执行一轮刷新，返回报「内容有变化」的服务数（> 0 即已要求收紧 MCP http
// 连接）。整轮单飞：另一轮尚未结束时本次触发直接跳过并返回 (0, nil)——上一轮会把它的
// 结果落定，下一拍再试。
//
// 逐服务调 `RefreshFromStore`（读表、应用、子摘要三者同源于该服务锁内的**一次**读）：
//   - 任一步 changed 或组合摘要与上一轮不同 → `RevokeHTTP()`；
//   - 任一服务失败 → 点名服务的错误返回，**不**推进 `lastVerifiedAt`，该服务保持上一轮
//     内容（其余服务本轮已应用的内容保留——应用是幂等的，下一拍重读重应用即可）；
//   - 四个服务都成功 → `lastVerifiedAt = now`，保存 `appliedDigest`。
//
// A failed round never advances the verified stamp; a changed round tightens live MCP
// http links before the round returns.
func (r *PolicyRefresher) RefreshOnce(ctx context.Context) (int64, error) {
	if r == nil {
		return 0, nil
	}
	if !r.running.CompareAndSwap(false, true) {
		// 上一轮未完：跳过本次触发，不排队、不叠加（§2.3.6「单飞」）。
		// A tick that lands mid-round is skipped, never queued or overlapped.
		return 0, nil
	}
	defer r.running.Store(false)

	sources := r.policySources()
	digests := [4]string{}
	changedCount := int64(0)
	failures := make([]error, 0, len(sources))
	for index, item := range sources {
		if item.source == nil {
			// 缺装配不得演变成 panic，也不得静默当作"无变化"：按失败关闭处理。
			failures = append(failures, fmt.Errorf("%s 未接线", item.name))
			continue
		}
		changed, digest, err := item.source.RefreshFromStore(ctx)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s：%w", item.name, err))
			continue
		}
		digests[index] = digest
		if changed {
			changedCount++
		}
	}

	if len(failures) > 0 {
		err := fmt.Errorf("刷新运行期策略失败：%w", errors.Join(failures...))
		r.logStaleAfterFailure(err)
		// 失败轮里成功的那几个服务若发现了变化，照样收紧已建立的连接：撤销是安全方向，
		// 宁可多断一条也不让已撤销的放行继续生效。
		if changedCount > 0 {
			r.revokeHTTP(nil)
		}
		return changedCount, err
	}

	composed := composePolicyDigest(digests)
	if previous := r.appliedDigest.Load(); changedCount > 0 || previous == nil || *previous != composed {
		r.revokeHTTP(&composed)
	}
	r.appliedDigest.Store(&composed)
	verified := r.deps.Now()
	r.lastVerified.Store(&verified)
	return changedCount, nil
}

// Run 每 `policy.sync_interval` 触发一轮刷新，直到 ctx 取消；首次同步由装配层在启动期
// 显式执行（构造不启动）。`Interval <= 0` 表示本进程不做周期刷新，直接阻塞到停机。
// 停机时在飞的一轮随 ctx 取消返回，join 在本协程内完成。
// Run ticks RefreshOnce until shutdown; the first round belongs to startup.
func (r *PolicyRefresher) Run(ctx context.Context) error {
	if r == nil {
		return nil
	}
	interval := r.deps.Interval
	if interval <= 0 {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := r.RefreshOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				r.deps.Logger.Error("runtime policy refresh round failed",
					"error", err, "stale", r.isStaleAt(r.deps.Now()))
			}
		}
	}
}

// logStaleAfterFailure 在失败轮上判断「此刻准入是否已拒绝新 Run」并记 ERROR：这是把
// 「刷新在失败」与「服务已开始拒绝业务」串起来的唯一日志锚点。新鲜度判定与准入共用
// 同一口径（零值 = 从未成功 = 过期）。
// The log line ties "refresh is failing" to "new runs are being rejected".
func (r *PolicyRefresher) logStaleAfterFailure(err error) {
	r.deps.Logger.Error("runtime policy refresh failed; new runs may be rejected while stale",
		"error", err, "stale", r.isStaleAt(r.deps.Now()), "last_verified", r.LastVerified())
}

// isStaleAt 是过期判定的**刷新侧**副本：与准入门的判据同口径（零值或无有效成功记录
// 即过期），只用于日志读数，不参与准入结论——准入自己有 `PolicyFreshness` +
// `policy.max_stale`，两处读的是同一份 `LastVerified`。
// isStaleAt mirrors the admission verdict for logging only.
func (r *PolicyRefresher) isStaleAt(now time.Time) bool {
	verified := r.LastVerified()
	if verified.IsZero() {
		return true
	}
	maxStale := r.deps.MaxStale
	if maxStale <= 0 {
		maxStale = defaultPolicyMaxStale
	}
	return now.Sub(verified) > maxStale
}

// revokeHTTP 要求 supervisor 按新规则重判已建立的 MCP http 连接；未装配时跳过。
// `digest` 非 nil 时只用于日志（记录是哪一轮摘要触发了收紧）。
// revokeHTTP tightens live MCP http links; the digest only annotates the log line.
func (r *PolicyRefresher) revokeHTTP(digest *string) {
	if r.deps.RevokeHTTP == nil {
		return
	}
	revoked := r.deps.RevokeHTTP()
	if revoked <= 0 {
		return
	}
	fields := []any{"revoked", revoked}
	if digest != nil {
		fields = append(fields, "digest", *digest)
	}
	r.deps.Logger.Info("runtime policy changed; revoked disallowed MCP http links", fields...)
}

// composePolicyDigest 把四个服务各自的子摘要按固定顺序组合成应用摘要（§2.3.6）：
// 每段先写长度再写内容，避免"两段边界移动、拼接串相同"的歧义。
// composePolicyDigest joins the four sub-digests in the fixed order with length
// prefixes, so a boundary shift can never produce the same composite string.
func composePolicyDigest(parts [4]string) string {
	builder := policyDigestBuilder{}
	for _, part := range parts {
		builder.add(part)
	}
	return builder.digest()
}

// policyDomainSubDigest 是域名策略行的子摘要，规范化口径（冻结，§2.3.6）：
// scope → (revision, 排序去重后的 domains)。**不**含 `updated_at` / `updated_by`：
// 内容与版本号才是判定面，时间戳会把同一毫秒的两次写洗成"没有变化"。
// policyDomainSubDigest normalizes a domain row as scope plus revision plus the sorted,
// deduped domain list; timestamps are deliberately excluded.
func policyDomainSubDigest(policy core.NetworkDomainPolicy) string {
	domains := append([]string(nil), policy.Domains...)
	slices.Sort(domains)
	domains = slices.Compact(domains)
	builder := policyDigestBuilder{}
	builder.add(policy.Scope)
	builder.add(strconv.FormatInt(policy.Revision, 10))
	for _, domain := range domains {
		builder.add(domain)
	}
	return builder.digest()
}

// policyExceptionSubDigest 是访问例外集合的子摘要，规范化口径（冻结，§2.3.6）：
// 按 (kind, value) 排序的身份对；**不**含 note 与任何时间戳——例外表只有 `created_at`、
// 更新走 `DELETE` + `INSERT`，删除对任何 `max()` 都不可见，只有内容摘要能看见。
// policyExceptionSubDigest normalizes the exception set to sorted (kind, value) identity
// pairs; notes and timestamps are excluded exactly as the frozen rule requires.
func policyExceptionSubDigest(list []core.NetworkException) string {
	pairs := make([]string, 0, len(list))
	for _, item := range list {
		pairs = append(pairs, item.Kind+"\x00"+item.Value)
	}
	slices.Sort(pairs)
	builder := policyDigestBuilder{}
	for _, pair := range pairs {
		builder.add(pair)
	}
	return builder.digest()
}

// policyExecutionSubDigest 是执行档位的子摘要，规范化口径（冻结，§2.3.6）：
// profile → revision。档位内容与修订号一一对应（CAS 保证同修订号不会被改写），
// 同内容不同修订号同样是变化。
// policyExecutionSubDigest normalizes the execution row to its revision.
func policyExecutionSubDigest(policy core.ExecutionPolicy) string {
	builder := policyDigestBuilder{}
	builder.add(strconv.FormatInt(policy.Revision, 10))
	return builder.digest()
}

// policyDigestBuilder 是带长度前缀的稳定编码器：`len:value` 逐段拼接，字段边界由长度
// 自身确定，不依赖分隔符不出现在内容里。
// policyDigestBuilder length-prefixes every field so content cannot shift boundaries.
type policyDigestBuilder struct {
	builder strings.Builder
}

// add 追加一段字段。
func (b *policyDigestBuilder) add(value string) {
	b.builder.WriteString(strconv.Itoa(len(value)))
	b.builder.WriteByte(':')
	b.builder.WriteString(value)
}

// digest 返回 SHA-256 的十六进制摘要。
func (b *policyDigestBuilder) digest() string {
	sum := sha256.Sum256([]byte(b.builder.String()))
	return hex.EncodeToString(sum[:])
}
