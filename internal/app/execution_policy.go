package app

// 本文件是执行档位的管理面服务（14D；设计 §2.2.1 / §2.2.3 / §3.2）：权威源是单行表
// `execution_policy`，`basalt.yaml` 的 `execution.profile` 只在表内无行时作首次种子。
// 保存走「校验 → ETag 与方向确认 → 预写审计 → 修订号 CAS → 原子热替换」，全程不重启。
//
// 三档语义各自独立，**不得**互相顶替：
//   - `If-Match` 防"覆盖了别人刚写的值"（陈旧客户端）；
//   - `expected_profile` 防"执行了一个用户没确认过的方向"（陈旧对话框）——两者都满足才写；
//   - 修订号 CAS 防进程外的并发写入。
//
// This file owns the managed execution profile: one authoritative row, config seeding only
// on first run, and a conditional write whose direction is confirmed by `expected_profile`
// in addition to `If-Match` and the revision CAS.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// ExecutionPolicyDeps 是档位服务的装配参数。`Policy` 必须与运行期消费者共用同一实例，
// 否则"保存即生效"只改了副本。`ShellCommands` 是命令白名单的部署配置值——升级待确认项
// 的命令轴扫它（策略实例只持有集合，不区分来源）。
// The policy instance must be the very one every consumer uses, or the swap changes
// nothing; the shell command list feeds the shell-name axis of the pending scan.
type ExecutionPolicyDeps struct {
	Store          core.ExecutionPolicyStore
	Operations     core.OperationStore
	Policy         *tool.WhitelistPolicy
	ShellCommands  []string
	Resolver       tool.AddressResolver
	ResolveTimeout time.Duration
	Logger         *slog.Logger
	Now            func() time.Time
}

// ExecutionPolicyService 持有当前生效的档位并与数据库保持同步；`mu` 把「读当前值 →
// 比较条件 → 预写 → 写入 → 应用」串成一段，数据库修订号 CAS 是第二道闸。
// ExecutionPolicyService serializes read-compare-write-apply; the revision CAS is the
// second gate, and readers of Current stay lock-free.
type ExecutionPolicyService struct {
	deps    ExecutionPolicyDeps
	mu      sync.Mutex
	current atomic.Pointer[core.ExecutionPolicy]
}

// 编译期断言：服务满足 web 依赖的管理端口。
// Compile-time assertion that the service satisfies the web-facing port.
var _ core.ExecutionPolicyAdmin = (*ExecutionPolicyService)(nil)

// defaultResolveTimeout 是待确认项扫描的单条解析时限：扫描是只读页面请求，宁可少列
// 一条（解析失败不计入、不猜测），也不让一次页面打开挂在 DNS 上。
// The per-entry resolve budget for the pending scan; a timeout drops the entry rather
// than guessing.
const defaultResolveTimeout = 2 * time.Second

// NewExecutionPolicyService 装配服务：缺省 logger 丢弃、缺省时钟取系统时间、
// 缺省解析器是标准库。
// NewExecutionPolicyService wires the service with the standard resolver by default.
func NewExecutionPolicyService(deps ExecutionPolicyDeps) *ExecutionPolicyService {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Resolver == nil {
		deps.Resolver = net.DefaultResolver
	}
	if deps.ResolveTimeout <= 0 {
		deps.ResolveTimeout = defaultResolveTimeout
	}
	return &ExecutionPolicyService{deps: deps}
}

// EnsureSeeded 在启动时装载权威档位：表内已有行 → 直接应用（**不再读** YAML）；
// 无行 → 说明这是新建库（迁移总会写行），按配置种子或 `standard` 写入并记
// `system.instance_init`。
// EnsureSeeded loads the authoritative row; only a row-less database (a brand-new one —
// the migration always writes the row) gets a seed, and that seed is recorded as
// instance initialization.
func (a *ExecutionPolicyService) EnsureSeeded(ctx context.Context, configProfile string) error {
	policy, exists, err := a.deps.Store.Load(ctx)
	if err != nil {
		return err
	}
	if exists {
		a.apply(policy)
		a.deps.Logger.Info("execution profile loaded from database; config key ignored",
			"profile", string(policy.Profile), "source", policy.Source(), "revision", policy.Revision)
		return nil
	}

	seed := core.ProfileStandard
	if configured := core.ExecutionProfile(strings.TrimSpace(configProfile)); configured.Valid() {
		// 配置只作首次种子：写了就按它建库，没写的新库按 standard（开箱可用）。
		// A written profile seeds the fresh instance; otherwise new databases start standard.
		seed = configured
	}
	seeded, err := a.deps.Store.Seed(ctx, seed, core.SystemOperationInstanceInit, a.deps.Now())
	if err != nil {
		return err
	}
	a.apply(seeded)
	a.deps.Logger.Info("execution profile seeded for a new instance; database is authoritative from now on",
		"profile", string(seeded.Profile), "source", seeded.Source(), "revision", seeded.Revision)
	return nil
}

// Current 返回实际生效的档位（零值 = 尚未装载）。
// Current returns the live snapshot; the zero value means "not loaded yet".
func (a *ExecutionPolicyService) Current() core.ExecutionPolicy {
	if policy := a.current.Load(); policy != nil {
		return *policy
	}
	return core.ExecutionPolicy{}
}

// ExceptionsCount 返回运行期策略实例里的例外条数：与拨号判定读同一份快照，页面
// 显示的数字就是此刻生效的集合——不另读库，避免"库里 3 条、判定看到 2 条"。
// The count is read from the live policy snapshot the dialer judges against.
func (a *ExecutionPolicyService) ExceptionsCount() int {
	if a.deps.Policy == nil {
		return 0
	}
	return len(a.deps.Policy.NetworkExceptions())
}

// Update 执行一次档位切换：校验 → ETag → 方向确认 → 预写 `pending` → 修订号 CAS
// → 原子热替换 → 回填终态。成功返回重新读出的档位与操作标识；预写之后的失败一律置
// failed 并把操作标识带回给调用方（页面据此查证）。
// Update performs the whole conditional write and returns the resulting policy plus the
// operation id; any failure after the pre-write is recorded as failed.
func (a *ExecutionPolicyService) Update(ctx context.Context, profile, expectedProfile core.ExecutionProfile,
	ifMatch string, actor core.Actor,
) (core.ExecutionPolicy, string, error) {
	if !profile.Valid() {
		// 纯请求校验：非法档位零写入、零操作行。
		return core.ExecutionPolicy{}, "", &core.InvalidInputError{
			Tool: "execution_policy", Reason: "档位只能是 standard 或 strict",
		}
	}
	if !expectedProfile.Valid() {
		return core.ExecutionPolicy{}, "", &core.InvalidInputError{
			Tool: "execution_policy", Reason: "expected_profile 必填，且只能是 standard 或 strict",
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	current := a.Current()
	if ifMatch != core.ExecutionPolicyETag(current) {
		return core.ExecutionPolicy{}, "", fmt.Errorf(
			"%w: execution policy If-Match does not match the current revision",
			core.ErrPreconditionFailed)
	}
	if expectedProfile != current.Profile {
		// 方向确认落空：页面渲染时看到 X、库里已经是 Y，就**不**执行任何写入——不能
		// 用陈旧对话框静默执行一个用户没确认过的方向。
		// A stale confirmation dialog never gets to execute a direction nobody confirmed.
		return core.ExecutionPolicy{}, "", fmt.Errorf(
			"%w: 页面确认的方向是 %s，库内当前是 %s",
			core.ErrExecutionProfileMismatch, expectedProfile, current.Profile)
	}

	now := a.deps.Now()
	detail := executionPolicyOperationDetail{
		BeforeProfile:  string(current.Profile),
		AfterProfile:   string(profile),
		BeforeRevision: current.Revision,
		AfterRevision:  current.Revision + 1,
	}
	detailJSON, marshalErr := json.Marshal(detail)
	if marshalErr != nil {
		// 结构只有字符串与整数，此分支不可达；仍不吞错误，按预写失败处理。
		return core.ExecutionPolicy{}, "", fmt.Errorf(
			"assemble execution policy operation detail: %w", marshalErr)
	}

	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionExecutionPolicyUpdate,
		TargetKind:   core.TargetKindExecutionPolicy,
		TargetID:     executionPolicyScope,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  agentRequestHash(string(profile)),
		ExpectedETag: ifMatch,
		DetailJSON:   detailJSON,
		Status:       core.OperationPending,
		CreatedAt:    now,
	}
	if beginErr := a.deps.Operations.BeginOperation(ctx, record); beginErr != nil {
		// 预写失败 = 拒绝执行：档位行一个字节都没动（宪法 V）。
		a.deps.Logger.Error("refusing execution policy update: pre-write of operation record failed",
			"error", beginErr)
		return core.ExecutionPolicy{}, "", fmt.Errorf(
			"pre-write operation record failed; refusing to update execution policy: %w", beginErr)
	}

	policy, updateErr := a.deps.Store.Update(ctx, profile, current.Revision, operationID,
		actor.Subject, now)
	if updateErr != nil {
		a.finish(ctx, operationID, core.OperationFailed, resultCodeFor(updateErr), updateErr, detailJSON)
		if errors.Is(updateErr, core.ErrPolicyRevisionConflict) {
			// CAS 落败：数据库已由别处推进，内存快照可能落后。重读并应用权威行，
			// 使随后的 GET 与授权判定与数据库一致；重读失败不臆造当前值。
			a.reloadAfterConflict(ctx)
		}
		return core.ExecutionPolicy{}, operationID, updateErr
	}

	// 落库成功后立刻生效：**下一次**授权判定即按新档位（在途 Run 与已建立连接不追溯）。
	a.apply(policy)
	a.finish(ctx, operationID, core.OperationSuccess, "OK", nil, detailJSON)
	a.deps.Logger.Info("execution profile updated", "profile", string(policy.Profile),
		"revision", policy.Revision, "operation_id", operationID)
	return policy, operationID, nil
}

// PendingConfirmations 只读扫描两组待确认项（设计 §2.2.6）：出网白名单中解析到受保护
// 地址段的条目、命令白名单中命中交互式 shell 名称的条目。**不**启动进程、**不**写操作行、
// **不**改策略；解析失败（NODATA / 超时）的条目不计入，也不猜测。
// The scan is read-only; an entry whose resolution fails is dropped rather than guessed.
func (a *ExecutionPolicyService) PendingConfirmations(ctx context.Context) ([]core.PendingPolicyConfirmation, error) {
	items := make([]core.PendingPolicyConfirmation, 0, 4)
	networkItems, err := a.pendingNetworkEntries(ctx)
	if err != nil {
		return nil, err
	}
	items = append(items, networkItems...)
	items = append(items, a.pendingShellEntries()...)

	// 「保持拒绝」的处置记录让该项不再列出：一条 `success` 的同目标确认行即视为已看过。
	// A recorded keep-denied decision drops the item from the list.
	kept := make([]core.PendingPolicyConfirmation, 0, len(items))
	for _, item := range items {
		recorded, lookupErr := a.confirmationRecorded(ctx, item.ID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if recorded {
			continue
		}
		kept = append(kept, item)
	}
	return kept, nil
}

// Confirm 记录一条「保持拒绝」：**不改授权结论**（受保护地址段与 shell 名称本来就默认
// 拒绝），只让该项不再出现在待确认列表里。已记录过 → already=true 且不新增审计行。
// Confirm records a keep-denied decision without changing any authorization verdict.
func (a *ExecutionPolicyService) Confirm(ctx context.Context, id string, actor core.Actor) (string, bool, error) {
	pending, err := a.PendingConfirmations(ctx)
	if err != nil {
		return "", false, err
	}
	item, found := findPending(pending, id)
	if !found {
		// 不在当前待确认集合内（已加例外 / 已记录 / 扫描条件不再成立）：点名标识，
		// **不**按 id 猜条目、**不**为未知标识留记录。
		return "", false, &core.InvalidInputError{
			Tool: "execution_policy", Reason: fmt.Sprintf("待确认项 %q 不在当前待确认集合内", id),
		}
	}

	now := a.deps.Now()
	detailJSON, marshalErr := json.Marshal(policyConfirmationDetail{ID: item.ID, Axis: string(item.Axis)})
	if marshalErr != nil {
		return "", false, fmt.Errorf("assemble policy confirmation detail: %w", marshalErr)
	}
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionExecutionPolicyConfirm,
		TargetKind:   core.TargetKindExecutionPolicy,
		TargetID:     item.ID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  agentRequestHash("keep_denied"),
		DetailJSON:   detailJSON,
		Status:       core.OperationPending,
		CreatedAt:    now,
	}
	if beginErr := a.deps.Operations.BeginOperation(ctx, record); beginErr != nil {
		a.deps.Logger.Error("refusing policy confirmation: pre-write of operation record failed",
			"error", beginErr, "id", item.ID)
		return "", false, fmt.Errorf(
			"pre-write operation record failed; refusing to record policy confirmation: %w", beginErr)
	}
	a.finish(ctx, operationID, core.OperationSuccess, "OK", nil, detailJSON)
	return operationID, false, nil
}

// pendingNetworkEntries 扫出网白名单轴：逐条解析（通配条目不可解析、跳过），任一地址
// 落在受保护段且没有覆盖它的例外即列为待确认项。
// Wildcard entries cannot be resolved and are skipped; an entry is pending while some
// protected address it resolves to has no covering exception.
func (a *ExecutionPolicyService) pendingNetworkEntries(ctx context.Context) ([]core.PendingPolicyConfirmation, error) {
	if a.deps.Policy == nil {
		return nil, nil
	}
	exceptions := a.deps.Policy.NetworkExceptions()
	items := make([]core.PendingPolicyConfirmation, 0, 4)
	for _, entry := range a.deps.Policy.AllowedDomains() {
		if strings.HasPrefix(entry, "*.") {
			// 通配条目没有可解析的具体域名：不猜测、不列举（契约只列"解析到"的条目）。
			continue
		}
		resolved, err := a.resolve(ctx, entry)
		if err != nil || len(resolved) == 0 {
			// 解析失败（NODATA / 超时 / 解析器错误）→ 不计入待确认项，也不猜测。
			continue
		}
		protected := make([]string, 0, len(resolved))
		covered := true
		for _, ip := range resolved {
			isProtected, metadata := core.ClassifyAddress(ip)
			if !isProtected {
				continue
			}
			protected = append(protected, ip.String())
			if metadata || !exceptionCovers(exceptions, entry, ip) {
				covered = false
			}
		}
		if len(protected) == 0 || covered {
			continue
		}
		items = append(items, core.PendingPolicyConfirmation{
			ID:                pendingConfirmationID(core.PendingAxisNetworkAllowlist, entry),
			Axis:              core.PendingAxisNetworkAllowlist,
			Value:             entry,
			Reason:            core.PendingReasonProtectedAddress,
			ResolvedProtected: protected,
		})
	}
	return items, nil
}

// pendingShellEntries 扫命令白名单轴：命中交互式 shell 名称集合的条目逐条列出（升级后
// 它们已被拒绝，待管理员显式选择「加例外」不适用于命令——这里只给「保持拒绝」）。
// Entries whose names hit the shell ceiling are listed; the only explicit action left is
// keep-denied, because the shell ceiling has no configuration key.
func (a *ExecutionPolicyService) pendingShellEntries() []core.PendingPolicyConfirmation {
	items := make([]core.PendingPolicyConfirmation, 0, 2)
	for _, command := range a.deps.ShellCommands {
		name := strings.TrimSpace(command)
		if name == "" || !core.IsShellName(name) {
			continue
		}
		items = append(items, core.PendingPolicyConfirmation{
			ID:                pendingConfirmationID(core.PendingAxisShellCommand, name),
			Axis:              core.PendingAxisShellCommand,
			Value:             name,
			Reason:            core.PendingReasonShellName,
			ResolvedProtected: []string{},
		})
	}
	return items
}

// confirmationRecorded 判定是否已有该条目的「保持拒绝」成功记录（目标标识即待确认项
// 标识）。查询失败**不**当作"未记录"——那会把已处置项又列出来。
// A failed lookup is never read as "not recorded": that would resurrect a handled item.
func (a *ExecutionPolicyService) confirmationRecorded(ctx context.Context, id string) (bool, error) {
	records, err := a.deps.Operations.ListOperations(ctx, core.OperationFilter{
		Action:     core.ActionExecutionPolicyConfirm,
		TargetKind: core.TargetKindExecutionPolicy,
		TargetID:   id,
		Status:     string(core.OperationSuccess),
		Limit:      1,
	})
	if err != nil {
		return false, fmt.Errorf("查证待确认项 %q 的处置记录: %w", id, err)
	}
	return len(records) > 0, nil
}

// resolve 解析一条域名（带单条时限）。IP 字面量按原值返回——白名单不接受 IP 字面量，
// 这里只是不让扫描路径凭空造一个新错误。
// resolve looks up one host under the per-entry budget; an IP literal passes through.
func (a *ExecutionPolicyService) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, a.deps.ResolveTimeout)
	defer cancel()
	addrs, err := a.deps.Resolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IP != nil {
			ips = append(ips, addr.IP)
		}
	}
	return ips, nil
}

// apply 同时替换策略实例的档位与生效快照：两处必须来自同一次写入，否则页面显示与
// 判定会分叉。
// apply swaps the policy's live profile and the snapshot together.
func (a *ExecutionPolicyService) apply(policy core.ExecutionPolicy) {
	if a.deps.Policy != nil {
		a.deps.Policy.SetExecutionProfile(policy.Profile)
	}
	snapshot := policy
	a.current.Store(&snapshot)
}

// reloadAfterConflict 在修订号 CAS 落败后重读权威行并应用：让页面随后的 GET 与授权
// 判定看到数据库真值。重读失败只记 WARN——调用方不得据此声称"当前值已知"。
// reloadAfterConflict converges the live snapshot after a CAS loss.
func (a *ExecutionPolicyService) reloadAfterConflict(ctx context.Context) {
	policy, exists, err := a.deps.Store.Load(ctx)
	if err != nil || !exists {
		a.deps.Logger.Warn("failed to reload execution policy after revision conflict",
			"error", err, "row_present", exists)
		return
	}
	a.apply(policy)
	a.deps.Logger.Warn("execution policy reloaded after revision conflict",
		"profile", string(policy.Profile), "revision", policy.Revision)
}

// finish 回填终态；回填不随请求取消而放弃，失败只记 ERROR（留待启动恢复查证）。
// finish writes the terminal state detached from cancellation; a failure is logged.
func (a *ExecutionPolicyService) finish(ctx context.Context, operationID string,
	status core.OperationStatus, code string, cause error, detail []byte,
) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if err := a.deps.Operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, detail, a.deps.Now()); err != nil {
		a.deps.Logger.Error("failed to record execution policy operation outcome; left for startup recovery",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// executionPolicyScope 是 `execution_policy` 单行表的主键（也是审计的 target_id）。
// The fixed primary key of the single-row table, reused as the audit target id.
const executionPolicyScope = "global"

// executionPolicyOperationDetail 是 `execution_policy.update` 的 `detail_json` 固定投影：
// 启动恢复按前后档位与修订号判三态；**不**放待确认项正文。
// The detail carries only the profile transition and revisions; recovery reads exactly
// these keys.
type executionPolicyOperationDetail struct {
	BeforeProfile  string `json:"before_profile"`
	AfterProfile   string `json:"after_profile"`
	BeforeRevision int64  `json:"before_revision"`
	AfterRevision  int64  `json:"after_revision"`
}

// policyConfirmationDetail 是 `execution_policy.confirm` 的 `detail_json` 固定投影：
// 轴 + 处置（当前唯一取值 keep_denied）。
// The confirmation detail records the axis and the decision (keep_denied today).
type policyConfirmationDetail struct {
	ID   string `json:"id"`
	Axis string `json:"axis"`
}

// pendingConfirmationID 是待确认项的稳定标识 `<axis>:<value>`（页面据此记住处置结果）。
// pendingConfirmationID renders the stable `<axis>:<value>` id.
func pendingConfirmationID(axis core.PendingConfirmationAxis, value string) string {
	return string(axis) + ":" + value
}

// findPending 在待确认集合里按标识找一项。
// findPending looks an item up by its stable id.
func findPending(items []core.PendingPolicyConfirmation, id string) (core.PendingPolicyConfirmation, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return core.PendingPolicyConfirmation{}, false
}

// exceptionCovers 判定一条已登记例外是否覆盖"这个白名单条目解析到这个地址"：
// `host` 例外按白名单同一套通配语法匹配条目；`ip_port` 例外按地址匹配（端口在扫描期
// 未知——登记过该地址的例外即视为管理员已看过这个地址，逐端口放行仍由拨号器判）。
// A host exception matches the entry; an ip_port exception matches the address (the port
// is unknown at scan time, and per-port admission still binds at dial time).
func exceptionCovers(exceptions []core.NetworkException, entry string, ip net.IP) bool {
	for _, exception := range exceptions {
		switch exception.Kind {
		case core.NetworkExceptionKindHost:
			if core.DomainEntryMatches(exception.Value, entry) || entry == exception.Value {
				return true
			}
		case core.NetworkExceptionKindIPPort:
			host, _, err := net.SplitHostPort(exception.Value)
			if err != nil {
				continue
			}
			other := net.ParseIP(host)
			if other != nil && other.Equal(ip) {
				return true
			}
		}
	}
	return false
}
