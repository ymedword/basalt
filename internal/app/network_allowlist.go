package app

// 本文件是网络域名白名单的管理面服务（US-12 R8 裁决 2）：SQLite 单行表是唯一权威源，
// `basalt.yaml` 的同名键只在表内无行时作首次种子；保存走「校验 → 条件写（ETag / 修订号
// CAS）→ 预写审计 → 落库 → 原子热替换 → 撤销不再允许的 MCP http 连接」，全程不重启。
// This file owns the managed outbound allowlist: one authoritative SQLite row, config
// seeding only on first run, and a conditional write that swaps live rules atomically.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// NetworkAllowlistDeps 是白名单服务的装配参数。Supervisor 可选：未装配时跳过撤销
// （只读装配面）；Policy 必须与运行期消费者共用同一实例，否则"热生效"只改了副本。
// The policy must be the very instance every consumer uses, or the swap changes nothing.
type NetworkAllowlistDeps struct {
	Store      core.NetworkDomainPolicyStore
	Operations core.OperationStore
	Policy     *tool.WhitelistPolicy
	Supervisor *MCPSupervisor
	Logger     *slog.Logger
	Now        func() time.Time
}

// NetworkAllowlist 持有当前生效的策略快照并与数据库保持同步；`mu` 把「读当前值 →
// 比较 ETag → 预写 → 写入 → 应用」串成一段，数据库修订号 CAS 是第二道闸。
// NetworkAllowlist serializes read-compare-write-apply; the revision CAS is the second
// gate. Readers of Current are lock-free.
type NetworkAllowlist struct {
	deps NetworkAllowlistDeps
	mu   sync.Mutex
	// current 是**实际生效**的快照：页面读它，因此不会出现"库里已改、界面显示新值、
	// 但判定还用旧规则"的窗口。零值表示尚未装载（尚未种子）。
	current atomic.Pointer[core.NetworkDomainPolicy]
}

// 编译期断言：服务满足 web 依赖的管理端口。
// Compile-time assertion that the service satisfies the web-facing port.
var _ core.NetworkAllowlistAdmin = (*NetworkAllowlist)(nil)

// NewNetworkAllowlist 装配服务：缺省 logger 丢弃、缺省时钟取系统时间。
// NewNetworkAllowlist wires the service with a discard logger and the wall clock.
func NewNetworkAllowlist(deps NetworkAllowlistDeps) *NetworkAllowlist {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &NetworkAllowlist{deps: deps}
}

// EnsureSeeded 在启动时装载权威策略：表内已有行 → 直接应用（**不再读** YAML）；
// 无行 → 把 `basalt.yaml` 的 `http.allowed_domains` 规整后写入种子并记 INFO。
// 改 YAML 不会覆盖管理台写入的值，重启也不会回退（R8.2.1 第 2 条）。
// EnsureSeeded loads the authoritative row, seeding it from config exactly once.
func (a *NetworkAllowlist) EnsureSeeded(ctx context.Context, configDomains []string) error {
	policy, exists, err := a.deps.Store.Load(ctx)
	if err != nil {
		return err
	}
	if exists {
		a.apply(policy)
		a.deps.Logger.Info("network domain allowlist loaded from database; config key ignored",
			"source", "database", "revision", policy.Revision, "domains", len(policy.Domains))
		return nil
	}

	seed, err := a.normalizeSeed(configDomains)
	if err != nil {
		return err
	}
	seeded, err := a.deps.Store.Seed(ctx, seed, a.deps.Now())
	if err != nil {
		return err
	}
	a.apply(seeded)
	a.deps.Logger.Info("network domain allowlist seeded from config; database is authoritative from now on",
		"source", "config_seed", "revision", seeded.Revision, "domains", len(seeded.Domains))
	return nil
}

// normalizeSeed 逐项规整配置种子：非法条目**忽略并记 WARN**——它们在旧实现里同样
// 从不命中，认作配置笔误比让整个进程起不来更符合"首次种子"的语义；但条目总数超过
// 上限属于配置错误，拒绝启动并点名人先缩减。
// normalizeSeed drops invalid seed entries with a WARN (they never matched before either)
// but refuses to start when the list exceeds the documented bound.
func (a *NetworkAllowlist) normalizeSeed(configDomains []string) ([]string, error) {
	cleaned := make([]string, 0, len(configDomains))
	for _, raw := range configDomains {
		normalized, normErr := core.NormalizeDomainList([]string{raw})
		if normErr != nil {
			a.deps.Logger.Warn("dropping invalid network domain allowlist seed entry",
				"entry", raw, "error", normErr)
			continue
		}
		cleaned = append(cleaned, normalized...)
	}
	seed, err := core.NormalizeDomainList(cleaned)
	if err != nil {
		return nil, fmt.Errorf("invalid network domain allowlist seed: %w", err)
	}
	return seed, nil
}

// Current 返回实际生效的策略快照（零值 = 尚未装载）。
// Current returns the live snapshot; the zero value means "not loaded yet".
func (a *NetworkAllowlist) Current() core.NetworkDomainPolicy {
	if policy := a.current.Load(); policy != nil {
		return *policy
	}
	return core.NetworkDomainPolicy{}
}

// Update 执行一次条件写：校验 → ETag 比较（412，零操作行）→ 预写 pending（失败即零写入）
// → 修订号 CAS 落库 → 原子热替换 → 撤销不再允许的 MCP http 连接 → 回填终态。
// 返回落库后的策略与操作标识；预写之后的失败一律置 failed 并把操作标识带回给调用方。
// Update performs the whole conditional write and returns the resulting policy plus the
// operation id; any failure after the pre-write is recorded as failed.
func (a *NetworkAllowlist) Update(ctx context.Context, domains []string, ifMatch string,
	actor core.Actor,
) (core.NetworkDomainPolicy, string, error) {
	normalized, err := core.NormalizeDomainList(domains)
	if err != nil {
		// 纯请求校验：非法域名零写入、零操作行（R8.2.4 第 3 条）。
		return core.NetworkDomainPolicy{}, "", err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	current := a.Current()
	if ifMatch != core.NetworkDomainETag(current) {
		// 条件写不满足：412，零写入、零操作行（R8.2.4 第 2 条）。
		return core.NetworkDomainPolicy{}, "", fmt.Errorf(
			"%w: network domain allowlist If-Match does not match the current revision",
			core.ErrPreconditionFailed)
	}

	now := a.deps.Now()
	detail := networkAllowlistOperationDetail{
		BeforeRevision:  current.Revision,
		AfterRevision:   current.Revision + 1,
		CandidateSHA256: networkDomainsSHA256(normalized),
	}
	detailJSON, marshalErr := json.Marshal(detail)
	if marshalErr != nil {
		// 结构只有整数与字符串，此分支不可达；仍不吞错误，按预写失败处理。
		return core.NetworkDomainPolicy{}, "", fmt.Errorf(
			"assemble network domain allowlist operation detail: %w", marshalErr)
	}

	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionNetworkAllowlistUpdate,
		TargetKind:   core.TargetKindNetworkAllowlist,
		TargetID:     core.PolicyScopeHTTPDomains,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  agentRequestHash(strings.Join(normalized, "\n")),
		ExpectedETag: ifMatch,
		DetailJSON:   detailJSON,
		Status:       core.OperationPending,
		CreatedAt:    now,
	}
	if beginErr := a.deps.Operations.BeginOperation(ctx, record); beginErr != nil {
		// 预写失败 = 拒绝执行：策略行一个字节都没动（宪法 V）。
		a.deps.Logger.Error("refusing network domain allowlist update: pre-write of operation record failed",
			"error", beginErr)
		return core.NetworkDomainPolicy{}, "", fmt.Errorf(
			"pre-write operation record failed; refusing to update network domain allowlist: %w",
			beginErr)
	}

	policy, updateErr := a.deps.Store.Update(ctx, normalized, current.Revision,
		operationID, actor.Subject, now)
	if updateErr != nil {
		a.finish(ctx, operationID, core.OperationFailed, resultCodeFor(updateErr), updateErr,
			detailJSON)
		if errors.Is(updateErr, core.ErrPolicyRevisionConflict) {
			// CAS 落败：数据库已由别处推进，内存快照可能落后。重读并应用权威行，
			// 使随后的 GET 与逐跳判定与数据库一致；重读失败不臆造当前值。
			// On CAS loss the authoritative row may be ahead; reload before the client retries.
			a.reloadAfterConflict(ctx)
		}
		return core.NetworkDomainPolicy{}, operationID, updateErr
	}

	// 内存原子替换不可能失败：落库成功后立刻生效，下一跳判定即按新规则
	// （R8.2.3 第 1 条 / R8.2.4 第 7 条）。
	a.apply(policy)
	revoked := a.revokeMCPHTTP(a.deps.Policy)
	if len(revoked) > 0 {
		a.deps.Logger.Info("network domain allowlist tightened; revoked disallowed MCP http links",
			"servers", revoked, "revision", policy.Revision)
	}
	a.finish(ctx, operationID, core.OperationSuccess, "OK", nil, detailJSON)
	return policy, operationID, nil
}

// reloadAfterConflict 在修订号 CAS 落败后重读权威行并应用：让页面随后的 GET 与
// 逐跳判定看到数据库真值，而不是落败进程的旧快照。重读失败只记 WARN——调用方
// 不得据此声称"当前值已知"（409 响应也不承诺 `current_etag`）。
// reloadAfterConflict converges the live snapshot after a CAS loss; a failed reload is
// logged and never presented as a known current value.
func (a *NetworkAllowlist) reloadAfterConflict(ctx context.Context) {
	policy, exists, err := a.deps.Store.Load(ctx)
	if err != nil || !exists {
		a.deps.Logger.Warn("failed to reload network domain allowlist after revision conflict",
			"error", err, "row_present", exists)
		return
	}
	a.apply(policy)
	revoked := a.revokeMCPHTTP(a.deps.Policy)
	a.deps.Logger.Warn("network domain allowlist reloaded after revision conflict",
		"revision", policy.Revision, "revoked_mcp_http", revoked)
}

// apply 同时替换策略实例与生效快照：两处必须来自同一次写入，否则页面显示与判定会分叉。
// apply swaps the policy instance and the live snapshot together.
func (a *NetworkAllowlist) apply(policy core.NetworkDomainPolicy) {
	a.deps.Policy.SetAllowedDomains(policy.Domains)
	snapshot := policy
	a.current.Store(&snapshot)
}

// revokeMCPHTTP 让 supervisor 按**新规则**重判已建立的 http 连接；未装配 supervisor
// 的只读面直接跳过。匹配器直接用策略实例的 `AllowsHost`——撤销与逐跳判定共用同一份
// 通配语义，**不**在装配层再写一份。放宽白名单不自动重连（如实边界见 R8.2.3）。
// revokeMCPHTTP re-judges live http links with the policy's own matcher; loosening
// never reconnects.
func (a *NetworkAllowlist) revokeMCPHTTP(policy *tool.WhitelistPolicy) []string {
	if a.deps.Supervisor == nil || policy == nil {
		return nil
	}
	return a.deps.Supervisor.RevokeDisallowedHTTP(policy.AllowsHost)
}

// finish 回填终态；回填不随请求取消而放弃，失败只记 ERROR（留待启动恢复查证）。
// finish writes the terminal state detached from cancellation; a failure is logged.
func (a *NetworkAllowlist) finish(ctx context.Context, operationID string,
	status core.OperationStatus, code string, cause error, detail []byte,
) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if err := a.deps.Operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, detail, a.deps.Now()); err != nil {
		a.deps.Logger.Error("failed to record network domain allowlist operation outcome; left for startup recovery",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// resultCodeFor 把更新失败映射为审计码：修订号冲突是 409 CONFLICT，其余按内部错误。
// resultCodeFor maps an update failure onto the shared result-code table.
func resultCodeFor(err error) string {
	if errors.Is(err, core.ErrPolicyRevisionConflict) {
		return "CONFLICT"
	}
	return "INTERNAL"
}

// networkAllowlistOperationDetail 是 `detail_json` 的固定投影：启动恢复按
// before/after 修订号与候选域名哈希三态判定（applied / not_applied / unknown）。
// The detail is the recovery verdict's material; keys must match the recovery struct.
type networkAllowlistOperationDetail struct {
	BeforeRevision  int64  `json:"before_revision"`
	AfterRevision   int64  `json:"after_revision"`
	CandidateSHA256 string `json:"candidate_sha256"`
}

// networkDomainsSHA256 是规整域名列表的摘要（按行连接）：恢复用它与库内当前值比对。
// networkDomainsSHA256 digests the normalized list for recovery comparison.
func networkDomainsSHA256(domains []string) string {
	sum := sha256.Sum256([]byte(strings.Join(domains, "\n")))
	return hex.EncodeToString(sum[:])
}
