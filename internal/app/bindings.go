package app

// 本文件是绑定投影的重建面与准入校验面（颗粒度文档 R4.6 / T120）。
//
// 权威永远是 `AGENT.md` 的 `resource_bindings` 块；`agent_resource_bindings` 只是
// **可重建投影**——它服务于删除闸门、范围收缩与准入的反查，被手工篡改不改变实际
// 引用。因此这里的每个写动作都是"以文件为准整只重建"，没有第二套追加路径。
//
// 投影不可信时（定义不可解析、身份无法证明）记 `projection_error` 并清空该 Agent
// 的投影行：读投影的闸门看到失败记录会回退定义文件扫描，扫描也失败则按"可能引用"
// 处理（fail-closed）。宁可挡住一次删除，也不放过一个仍被引用的资源。
// This file rebuilds the binding projection and validates bindings at admission.
// AGENT.md stays authoritative; a failed projection is recorded and its rows are
// cleared so every reader can fall back to scanning the file, failing closed.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/web"
	"github.com/ymedword/basalt/internal/workspace"
)

// 投影失败的固定短语（进日志与 `projection_error` 集合；页面与运维按同一份短语读）。
// The fixed projection-failure reasons.
const (
	// projectionReasonInvalidBindings：定义文件的 `resource_bindings` 结构非法。
	projectionReasonInvalidBindings = "invalid_bindings"
	// projectionReasonNoIdentity：活动 Agent 没有可证明的稳定身份（索引缺失 / 未回填）。
	// 目录存在但**没有可读的 AGENT.md** 的情形不进投影失败集合：扫描阶段就看不到这份
	// 文档，归属由迁移侧的 `definition_missing` 短语记录。
	// A directory without a readable AGENT.md never reaches the projection.
	projectionReasonNoIdentity = "identity_unrecorded"
	// projectionReasonArchiveLegacy：v3 之前的归档没有记录身份，无法归属投影。
	projectionReasonArchiveLegacy = "archive_legacy_unrecorded"
)

// AgentIdentitySource 提供 Agent 名 → 稳定身份（生命周期状态文件的可重建镜像）。
// `*storage.ResourceStore` 满足本接口。
// AgentIdentitySource answers "which stable identity belongs to this agent name".
type AgentIdentitySource interface {
	AgentIdentity(ctx context.Context, agent string) (core.AgentUID, string, bool, error)
}

// ArchiveLister 列出归档记录（归档 Agent 也参与投影重建）。
// ArchiveLister lists archive rows; archived agents take part in the rebuild too.
type ArchiveLister interface {
	ListArchives(ctx context.Context) ([]core.AgentArchiveRecord, error)
}

// ProjectionFailure 是一条投影失败记录：哪个 Agent、固定原因。
// ProjectionFailure records one agent whose projection could not be trusted.
type ProjectionFailure struct {
	Agent  string
	Reason string
}

// ProjectionReport 是一次全量重建的计数与失败清单。
// ProjectionReport is the outcome of one full rebuild.
type ProjectionReport struct {
	Rebuilt  int
	Cleared  int
	Failures []ProjectionFailure
}

// BindingProjection 是绑定投影的重建面：写路径重建单个 Agent，启动时全量重建。
// 并发安全（闸门可能在服务开放后并发读取失败集合）。
// BindingProjection rebuilds the projection for one agent or for the whole workspace;
// its failure set is safe for concurrent readers.
type BindingProjection struct {
	Bindings *storage.BindingStore
	Identity AgentIdentitySource
	Archives ArchiveLister
	Root     string
	Logger   *slog.Logger
	Now      func() time.Time

	mu     sync.RWMutex
	failed map[string]string
}

// NewBindingProjection 装配投影重建面；失败集合初始为空。
// NewBindingProjection assembles the projector with an empty failure set.
func NewBindingProjection(bindings *storage.BindingStore, identity AgentIdentitySource,
	archives ArchiveLister, root string, logger *slog.Logger, now func() time.Time,
) *BindingProjection {
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &BindingProjection{
		Bindings: bindings,
		Identity: identity,
		Archives: archives,
		Root:     root,
		Logger:   logger,
		Now:      now,
		failed:   make(map[string]string, 4),
	}
}

// RebuildAgent 用一个 Agent 的**已校验**定义重建投影（写面提交成功后调用）：
// uid 为空直接拒绝——没有身份的投影无法归属，宁可留空也不写假行。
// RebuildAgent rebuilds one agent's projection from validated bindings.
func (p *BindingProjection) RebuildAgent(ctx context.Context, agent string, uid core.AgentUID,
	contentHash string, bindings core.ResourceBindings,
) error {
	if uid == "" {
		return fmt.Errorf("%w: Agent %s 没有稳定身份，无法重建绑定投影",
			core.ErrAgentResourceBindingInvalid, agent)
	}
	if err := p.Bindings.Rebuild(ctx, uid, agent, contentHash, bindings, p.Now()); err != nil {
		return err
	}
	p.recordSuccess(agent)
	return nil
}

// RebuildAgentDefinition 用**磁盘上的定义文件字节**重建单个 Agent 的投影（写路径在
// 创建 / 复制 / 恢复 / 归档 / 多文件提交成功之后调用）：失败分类与 `RebuildAll` 的
// 单 Agent 分支逐字一致——定义不可解析 → 清空该 Agent 的投影并记 `invalid_bindings`
// （闸门随之按"可能引用"处理），可解析 → 以文件里的绑定整块替换投影行。
// RebuildAgentDefinition rebuilds one agent from its definition bytes with the same
// failure classification the full rebuild uses.
func (p *BindingProjection) RebuildAgentDefinition(ctx context.Context, agent string, uid core.AgentUID,
	definition []byte,
) error {
	bindings, parseErr := workspace.ParseResourceBindings(definition)
	if parseErr != nil {
		if uid == "" {
			// 没有身份就没有可清的空行：与 RebuildAll 同口径，记失败但不动投影。
			p.recordFailure(agent, projectionReasonInvalidBindings)
			return nil
		}
		if clearErr := p.Bindings.Clear(ctx, uid); clearErr != nil {
			return fmt.Errorf("清空 Agent %s 的绑定投影: %w", agent, clearErr)
		}
		p.recordFailure(agent, projectionReasonInvalidBindings)
		return nil
	}
	return p.RebuildAgent(ctx, agent, uid, migrateV3ContentHash(definition), bindings)
}

// RebuildAgentFromDisk 按活动区的定义文件重建一个 Agent 的绑定投影（写操作成功之后的
// 余量）：定义读不到或重建失败只记 ERROR——投影可重建，写操作本身已经生效，**不得**
// 把投影失败改写成写失败（与索引同步同一纪律）。投影未装配时是 no-op。
// RebuildAgentFromDisk rebuilds one agent's projection from the on-disk definition after
// a successful write; failures are logged only and never turned into a write failure.
func RebuildAgentFromDisk(ctx context.Context, projection *BindingProjection, root, agent string,
	uid core.AgentUID, logger *slog.Logger,
) {
	if projection == nil || root == "" {
		return
	}
	definition, err := workspace.ReadAgentDefinition(root, agent)
	if err != nil {
		logger.Error("重建绑定投影失败：读取定义文件失败（写操作已生效，投影待下次重建）",
			"agent", agent, "error", err)
		return
	}
	rebuild(ctx, projection, agent, uid, definition, logger)
}

// rebuild 在**脱离取消**的上下文里重建投影并只记失败：调用点的 ctx 可能已随客户端
// 断开，被取消打断只会留下半截投影。
// rebuild runs detached from cancellation and only logs a failure.
func rebuild(ctx context.Context, projection *BindingProjection, agent string, uid core.AgentUID,
	definition []byte, logger *slog.Logger,
) {
	if err := projection.RebuildAgentDefinition(context.WithoutCancel(ctx), agent, uid, definition); err != nil {
		logger.Error("重建绑定投影失败（写操作已生效，投影待下次重建）",
			"agent", agent, "error", err)
	}
}

// RebuildAll 扫描工作区里的每份 `AGENT.md`（活动 + 归档）全量重建：
//   - 结构非法 → 清空该 Agent 投影并记 `invalid_bindings`；
//   - 没有可证明身份 → 记 `identity_unrecorded` / `archive_legacy_unrecorded`，不动投影
//     （本来就不会有行，清空也无意义）；
//   - 其余按文件重建，UID 以文件为准。
//
// 扫描本身失败（目录不可读、定义超出上限）返回错误——那是工作区级问题，启动应拒绝。
// RebuildAll rebuilds every active and archived agent from its definition file.
func (p *BindingProjection) RebuildAll(ctx context.Context) (ProjectionReport, error) {
	docs, err := workspace.ScanAgentDocuments(p.Root)
	if err != nil {
		return ProjectionReport{}, fmt.Errorf("重建绑定投影: %w", err)
	}
	archiveUIDs, err := p.archiveUIDs(ctx)
	if err != nil {
		return ProjectionReport{}, err
	}

	// 先清空失败集合：全量重建的结果就是新的真值，不叠加上一轮的残留。
	p.mu.Lock()
	p.failed = make(map[string]string, 4)
	p.mu.Unlock()

	report := ProjectionReport{}
	for _, doc := range docs {
		uid, reason := p.docIdentity(ctx, doc, archiveUIDs)
		if reason != "" {
			p.recordFailure(doc.Name, reason)
			report.Failures = append(report.Failures, ProjectionFailure{Agent: doc.Name, Reason: reason})
			continue
		}

		bindings, parseErr := workspace.ParseResourceBindings([]byte(doc.Text))
		if parseErr != nil {
			// 定义不可解析：投影清空 + 记错误，闸门按"可能引用"处理（fail-closed）。
			if clearErr := p.Bindings.Clear(ctx, uid); clearErr != nil {
				return report, fmt.Errorf("清空 Agent %s 的绑定投影: %w", doc.Name, clearErr)
			}
			p.recordFailure(doc.Name, projectionReasonInvalidBindings)
			report.Failures = append(report.Failures, ProjectionFailure{
				Agent: doc.Name, Reason: projectionReasonInvalidBindings,
			})
			report.Cleared++
			continue
		}

		if err := p.Bindings.Rebuild(ctx, uid, doc.Name, migrateV3ContentHash([]byte(doc.Text)),
			bindings, p.Now()); err != nil {
			return report, fmt.Errorf("重建 Agent %s 的绑定投影: %w", doc.Name, err)
		}
		report.Rebuilt++
	}
	return report, nil
}

// ProjectionError 报告某 Agent 的投影是否不可信（真 = 调用方应回退定义文件扫描）。
// ProjectionError reports whether the agent's projection must not be trusted.
func (p *BindingProjection) ProjectionError(agent string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	reason, ok := p.failed[agent]
	return reason, ok
}

// FailedAgents 返回失败集合的副本（运维视图与测试用；调用方不得修改返回值）。
// FailedAgents returns a copy of the failure set.
func (p *BindingProjection) FailedAgents() map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make(map[string]string, len(p.failed))
	for agent, reason := range p.failed {
		out[agent] = reason
	}
	return out
}

// archiveUIDs 把归档标识映射到已记录的 Agent 身份（未记录的不进表）。
// archiveUIDs maps archive ids to recorded identities.
func (p *BindingProjection) archiveUIDs(ctx context.Context) (map[string]core.AgentUID, error) {
	if p.Archives == nil {
		return nil, nil
	}
	records, err := p.Archives.ListArchives(ctx)
	if err != nil {
		return nil, fmt.Errorf("重建绑定投影：读取归档记录: %w", err)
	}
	out := make(map[string]core.AgentUID, len(records))
	for _, rec := range records {
		if rec.AgentUID != "" {
			out[rec.ArchiveID] = rec.AgentUID
		}
	}
	return out, nil
}

// docIdentity 取一份定义对应的稳定身份；取不到时返回固定原因。
// docIdentity resolves the identity of one definition, or its failure reason.
func (p *BindingProjection) docIdentity(ctx context.Context, doc workspace.AgentDocument,
	archiveUIDs map[string]core.AgentUID,
) (core.AgentUID, string) {
	if doc.Archived {
		uid := archiveUIDs[doc.ArchiveID]
		if uid == "" {
			return "", projectionReasonArchiveLegacy
		}
		return uid, ""
	}
	uid, _, found, err := p.Identity.AgentIdentity(ctx, doc.Name)
	if err != nil {
		p.Logger.Warn("重建绑定投影：读取 Agent 身份失败", "agent", doc.Name, "error", err)
		return "", projectionReasonNoIdentity
	}
	if !found || uid == "" {
		return "", projectionReasonNoIdentity
	}
	return uid, ""
}

// recordFailure / recordSuccess 维护失败集合（成功即摘掉旧记录）。
// recordFailure and recordSuccess maintain the failure set.
func (p *BindingProjection) recordFailure(agent, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed[agent] = reason
}

func (p *BindingProjection) recordSuccess(agent string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.failed, agent)
}

// —— 准入校验 ——

// BindingCredentialCheck 是凭据状态的只读检查（B5 的守卫实现它）。nil 表示本进程
// 没有托管凭据来源，按"无需检查"处理——托管模式本身在缺少解析器时已拒绝加载。
// BindingCredentialCheck is the read-only credential-state check; nil means none.
type BindingCredentialCheck interface {
	CheckBinding(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) error
}

// CredentialPinSource 是准入期固定托管凭据版本的只读面（T134）：按资源 UID 取当前
// active 身份与版本，产出清单里的 `CredentialPin`。**不读主密钥、不解密**；撤销、
// 版本状态非法或目标地址漂移一律失败关闭——**不得**换成另一个版本或降级为 env。
// nil 表示本进程没有托管凭据来源：此时 managed 定义在准入处失败关闭（见
// credentialPinOf），不会静默按 env 运行。
// CredentialPinSource pins the current managed credential version at admission without
// decrypting; a nil source keeps managed definitions failing closed.
type CredentialPinSource interface {
	PinModelCredential(ctx context.Context, uid core.ResourceUID, targetBinding string) (core.CredentialPin, error)
}

// BindingCheckDeps 是准入绑定校验的只读依赖。
// `NotifyChannels` 是**活跃**渠道目录（与发送路径同一个零缓存仓储，墓碑行不出现）：
// 保存期校验定时任务的 `deliver_to` 用它按 UID 查目标；为 nil 时带目标的保存失败关闭。
// BindingCheckDeps carries the read-only dependencies of the admission check; the
// channel catalog lists active rows only and a nil catalog keeps targets failing closed.
type BindingCheckDeps struct {
	Resources      *storage.ResourceStore
	Notify         *storage.NotifyAdminStore
	NotifyChannels core.NotifyChannelReader
	Credentials    BindingCredentialCheck
}

// BindingProblem 是一条可点名的绑定问题：类别、引用名、固定原因与哨兵。
// BindingProblem names one binding that fails admission.
type BindingProblem struct {
	Kind   core.BindingKind
	Name   string
	Reason string
	Cause  error
}

// Error 把问题渲染成"类别 / 名称 / 原因"，并保留哨兵以便 ErrorIs 判定。
// Error renders the problem and keeps the sentinel reachable through errors.Is.
func (p BindingProblem) Error() string {
	return fmt.Sprintf("%s %q: %s: %v", p.Kind, p.Name, p.Reason, p.Cause)
}

// Unwrap 暴露哨兵（准入期由 HTTP 层映射 409 与错误码）。
// Unwrap exposes the sentinel for the admission-time status mapping.
func (p BindingProblem) Unwrap() error { return p.Cause }

// CheckAgentBindings 在准入期校验一个 Agent 的绑定与引用（颗粒度文档 R4.4 / R4.6）：
//  1. 引用与绑定严格一一对应——缺失 `AGENT_RESOURCE_BINDING_MISSING`、
//     孤儿 / 重复 / 结构非法 `AGENT_RESOURCE_BINDING_INVALID`；
//  2. 每个 UID 存在、是该名称的**当前指针**、已发布——否则 `AGENT_RESOURCE_UNAVAILABLE`；
//  3. 对当前 Agent 有资格（共享 ∪ 本人专属）——否则 `RESOURCE_NOT_ELIGIBLE`；
//  4. 凭据状态可用（有检查器时）——否则由检查器返回 `CREDENTIAL_REVOKED` 等。
//
// 任何一条不成立即拒绝（不按名称猜、不自动补绑、不做部分放行）。
// CheckAgentBindings validates bindings and references at admission; every failure
// is named and nothing is inferred from a name.
func CheckAgentBindings(ctx context.Context, deps BindingCheckDeps, agent string,
	agentUID core.AgentUID, profile *core.Profile,
) error {
	channels := profile.NotifyChannels
	bindings := profile.ResourceBindings
	if err := bindings.ValidateCorrespondence(profileProviderNames(profile), channels); err != nil {
		return err
	}

	for _, entry := range bindings.ModelConnections {
		if err := checkModelConnectionBinding(ctx, deps, agentUID, entry); err != nil {
			return err
		}
	}
	for _, entry := range bindings.NotifyChannels {
		if err := checkNotifyChannelBinding(ctx, deps, agentUID, entry); err != nil {
			return err
		}
	}
	_ = agent
	return nil
}

// profileProviderNames 返回定义里的模型连接引用（空 provider 名 = 不引用，返回空集）。
// 与 wire.go 的 providerNames（配置面清单）区分开：这里读的是单个 Agent 的定义。
// profileProviderNames returns the provider references declared by one agent profile.
func profileProviderNames(profile *core.Profile) []string {
	if profile == nil || profile.Provider.Name == "" {
		return nil
	}
	return []string{profile.Provider.Name}
}

// checkModelConnectionBinding 校验一条模型连接绑定。
// checkModelConnectionBinding validates one model-connection binding.
func checkModelConnectionBinding(ctx context.Context, deps BindingCheckDeps,
	agentUID core.AgentUID, entry core.ResourceBinding,
) error {
	ptr, err := deps.Resources.Pointer(ctx, core.ResourceModelConnection, entry.Name)
	if err != nil || ptr.DeletedAt != nil || ptr.PublishedVersionID == 0 {
		return bindingProblem(core.BindingModelConnection, entry.Name,
			"资源不存在、已删除或从未发布", core.ErrAgentResourceUnavailable)
	}
	if ptr.ResourceUID != entry.UID {
		return bindingProblem(core.BindingModelConnection, entry.Name,
			"绑定的 UID 不是该名称的当前资源（同名重建不自动接管）", core.ErrAgentResourceBindingInvalid)
	}
	if !core.Eligible(ptr.Scope, ptr.OwnerAgentUID, agentUID) {
		return bindingProblem(core.BindingModelConnection, entry.Name,
			"资源不在此 Agent 的可用范围内", core.ErrResourceNotEligible)
	}
	if deps.Credentials != nil {
		if credErr := deps.Credentials.CheckBinding(ctx, core.ResourceModelConnection, entry.UID); credErr != nil {
			return bindingProblem(core.BindingModelConnection, entry.Name, "凭据不可用", credErr)
		}
	}
	return nil
}

// checkNotifyChannelBinding 校验一条通知渠道绑定。
// checkNotifyChannelBinding validates one notify-channel binding.
func checkNotifyChannelBinding(ctx context.Context, deps BindingCheckDeps,
	agentUID core.AgentUID, entry core.ResourceBinding,
) error {
	channel, err := deps.Notify.Get(ctx, entry.Name)
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return bindingProblem(core.BindingNotifyChannel, entry.Name,
				"渠道不存在或已删除", core.ErrAgentResourceUnavailable)
		}
		return fmt.Errorf("读取通知渠道 %q: %w", entry.Name, err)
	}
	if channel.ResourceUID != entry.UID {
		return bindingProblem(core.BindingNotifyChannel, entry.Name,
			"绑定的 UID 不是该名称的当前资源（同名重建不自动接管）", core.ErrAgentResourceBindingInvalid)
	}
	if !core.Eligible(channel.Scope, channel.OwnerAgentUID, agentUID) {
		return bindingProblem(core.BindingNotifyChannel, entry.Name,
			"渠道不在此 Agent 的可用范围内", core.ErrResourceNotEligible)
	}
	if deps.Credentials != nil {
		if credErr := deps.Credentials.CheckBinding(ctx, core.ResourceNotifyChannel, entry.UID); credErr != nil {
			return bindingProblem(core.BindingNotifyChannel, entry.Name, "凭据不可用", credErr)
		}
	}
	return nil
}

// bindingProblem 组装一条绑定问题。
// bindingProblem builds one binding problem.
func bindingProblem(kind core.BindingKind, name, reason string, cause error) error {
	return BindingProblem{Kind: kind, Name: name, Reason: reason, Cause: cause}
}

// CheckScheduleDeliveryTargets 在保存期校验每条定时任务的系统代投目标（US-12 R7，
// 裁决 D-1）：`deliver_to` 只接受通知渠道**资源 UID**，必须命中一个活跃渠道行
// （墓碑行按不存在处理），该渠道在当前 Agent 的可用范围内（共享 ∪ 本人专属），且
// 落在 Agent 声明的通知渠道内（声明非空时；声明为空 = 不限制）。
// 按名称查找一律不做——同名重建会生成新 UID，按名字投递会把旧任务送往新机器人。
// 运行开始时 `deliveryService.Prepare` 用同一口径再复核一次并把结果写进投递记录；
// 两侧都只是"检查"，运行中的目标固定来自预写行，**不得**中途改投。
// CheckScheduleDeliveryTargets validates every schedule's delivery target at save time;
// the run path re-checks with the same rule and freezes the target per run.
func CheckScheduleDeliveryTargets(ctx context.Context, deps BindingCheckDeps,
	agentUID core.AgentUID, profile *core.Profile,
) error {
	if profile == nil {
		return nil
	}
	var channels []core.NotifyChannel
	for _, entry := range profile.Schedules {
		uid := core.ResourceUID(strings.TrimSpace(entry.DeliverTo))
		if uid == "" {
			continue
		}
		label := entry.ID
		if label == "" {
			label = "（唯一任务）"
		}
		if _, err := core.ParseResourceUID(string(uid)); err != nil {
			return scheduleTargetProblem(label, "",
				"投递目标必须是资源 UID（rs_ 前缀），不能写渠道名：请重新选择",
				core.ErrAgentResourceBindingInvalid)
		}
		if deps.NotifyChannels == nil {
			return fmt.Errorf("保存期投递目标校验缺少通知渠道目录装配（Agent %s 的任务 %s）",
				agentUID, label)
		}
		if channels == nil {
			list, err := deps.NotifyChannels.ListNotifyChannels(ctx)
			if err != nil {
				return fmt.Errorf("读取通知渠道目录: %w", err)
			}
			channels = list
		}
		channel, ok := findActiveChannel(channels, uid)
		if !ok {
			return scheduleTargetProblem(label, uid,
				"投递渠道不存在或已删除：请重新选择", core.ErrAgentResourceUnavailable)
		}
		if !core.Eligible(channel.Scope, channel.OwnerAgentUID, agentUID) {
			return scheduleTargetProblem(label, uid,
				"投递渠道不在本 Agent 的可用范围内：请改用共享渠道或本人专属渠道",
				core.ErrResourceNotEligible)
		}
		// 声明边界（R7："Agent 的渠道仍是权限边界"）：Agent 声明了通知渠道时，代投只能
		// 落在声明并绑定的那些 UID 内；声明为空 = 不限制，与 notify 工具的授权口径一致。
		if !deliveryWithinDeclaredChannels(profile, uid) {
			return scheduleTargetProblem(label, uid,
				"投递渠道不在该 Agent 声明的通知渠道内：请先声明并绑定该渠道，或改选已声明渠道",
				core.ErrResourceNotEligible)
		}
	}
	return nil
}

// deliveryWithinDeclaredChannels 判定代投目标是否落在 Agent 声明的通知渠道内：
// 声明为空 = 不限制；非空时身份以 `resource_bindings.notify_channels` 的 **UID** 为准
// （声明名 ↔ 绑定 UID 的一一对应由保存期绑定校验保证）。声明非空却没有对应绑定时
// 返回 false——无法证明目标在边界内就不放行。
// deliveryWithinDeclaredChannels enforces the declared channel boundary by uid; an empty
// declaration leaves delivery unrestricted.
func deliveryWithinDeclaredChannels(profile *core.Profile, uid core.ResourceUID) bool {
	if profile == nil || len(profile.NotifyChannels) == 0 {
		return true
	}
	for _, binding := range profile.ResourceBindings.NotifyChannels {
		if binding.UID == uid {
			return true
		}
	}
	return false
}

// scheduleTargetProblem 把一条定时任务投递目标问题包装成可执行响应（契约
// run-and-errors §8：`AGENT_RESOURCE_UNAVAILABLE` 必须点名引用名与 UID 前 8 位）。
// 2026-10-24 在 8080 真实配置的人工验收中发现：HTTP 层按固定短语出口只认
// `AdminError.Message`，裸 `BindingProblem` 的任务名与 UID 会丢，页面只剩
// "绑定的资源不存在"。这里保留 `BindingProblem` 在错误链上（`errors.As` 仍可取到
// 类别 / 名称 / 原因），并把定位信息写进对外文案；底层 cause 只留链上、不进文案。
// scheduleTargetProblem keeps the named BindingProblem in the chain and puts the task
// label and uid prefix into the public message the error contract requires.
func scheduleTargetProblem(label string, uid core.ResourceUID, guidance string, cause error) error {
	locator := fmt.Sprintf("定时任务 %q", label)
	if uid != "" {
		locator += "（目标 " + shortResourceUID(uid) + "）"
	}
	return &core.AdminError{
		Kind:    BindingProblem{Kind: core.BindingNotifyChannel, Name: label, Reason: guidance, Cause: cause},
		Message: locator + "：" + guidance,
	}
}

// shortResourceUID 取资源 UID 的前 8 个字符（契约 §8 的点名口径）；短于 8 位原样返回。
// shortResourceUID returns the first eight characters of a resource uid.
func shortResourceUID(uid core.ResourceUID) string {
	if len(uid) <= 8 {
		return string(uid)
	}
	return string(uid[:8])
}

// findActiveChannel 按资源 UID 在活跃渠道目录里找目标；二次过滤墓碑行，仓储实现
// 变化时也不会把已删除渠道当可用。
// findActiveChannel matches by uid and re-filters tombstones defensively.
func findActiveChannel(channels []core.NotifyChannel, uid core.ResourceUID) (core.NotifyChannel, bool) {
	for _, channel := range channels {
		if channel.ResourceUID == uid && channel.DeletedAt == nil {
			return channel, true
		}
	}
	return core.NotifyChannel{}, false
}

// SaveBindingCheck 构造保存期绑定校验闭包（内容条件写与多文件提交共用这一份）：
// 身份取生命周期状态文件，缺失或损坏时按空身份校验——共享资源仍可用，专属资源会被
// `RESOURCE_NOT_ELIGIBLE` 拒绝（与准入同一口径：没有可信身份的 Agent 不能使用专属资源）。
// 绑定之后紧接着校验定时任务投递目标（裁决 D：保存与运行两侧都校验资格）。
// SaveBindingCheck is the one save-time binding check; an agent without a trusted
// identity keeps shared resources and loses exclusive ones.
func SaveBindingCheck(lifecycle *workspace.LifecycleStore, deps BindingCheckDeps) func(ctx context.Context, agent string, profile *core.Profile) error {
	return func(ctx context.Context, agent string, profile *core.Profile) error {
		var agentUID core.AgentUID
		if lifecycle != nil {
			if rec, exists, err := lifecycle.Read(ctx, agent); err == nil && exists {
				agentUID = rec.AgentUID
			}
		}
		if err := CheckAgentBindings(ctx, deps, agent, agentUID, profile); err != nil {
			return err
		}
		return CheckScheduleDeliveryTargets(ctx, deps, agentUID, profile)
	}
}

// AgentConfigDeps 是结构化配置读面的只读依赖（颗粒度文档 R4.3）：定义文件取工作区，
// 身份墓碑 / 发布指针 / 渠道行 / 凭据身份各取一份仓储，生命周期状态文件给稳定身份。
// **不**注入操作仓储：这个面只读，不写操作行、不改投影。
// AgentConfigDeps carries the read-only dependencies of the config reader; there is
// no operation store here because the surface never writes.
type AgentConfigDeps struct {
	Root        string
	Resources   *storage.ResourceStore
	Identities  *storage.IdentityStore
	Notify      *storage.NotifyAdminStore
	Credentials *storage.CredentialStore
	Lifecycle   *workspace.LifecycleStore
}

// AgentConfigReader 实现 `web.AgentConfigService`：一次读文件给出字段与强 ETag，
// 再按绑定逐个查身份、指针、渠道与凭据，输出每个引用的解析状态。**不补绑、不按名
// 解析**——绑定 UID 原样校验，名称只用于显示与"同名重建后指向旧资源"的提示。
// AgentConfigReader reads the definition once and resolves each binding against the
// stores; it never re-binds or resolves a uid from a name.
type AgentConfigReader struct {
	deps AgentConfigDeps
}

// 编译期断言：读面实现 web 的消费方端口（与写面端口同一纪律）。
// Compile-time assertion that the reader satisfies the consumer-side port.
var _ web.AgentConfigService = (*AgentConfigReader)(nil)

// NewAgentConfigReader 组装只读配置读面。
// NewAgentConfigReader assembles the read-only config reader.
func NewAgentConfigReader(deps AgentConfigDeps) *AgentConfigReader {
	return &AgentConfigReader{deps: deps}
}

// agentConfigUnreadable 是"配置读不出来"的固定日志文案（原因只进日志，不出站）。
// agentConfigUnreadable is the fixed log message; the cause never leaves the process.
const agentConfigUnreadable = "读取 Agent 结构化配置失败"

// Config 读取一个 Agent 的结构化配置与引用解析状态。
// Config returns the structured config and the per-reference resolution.
func (r *AgentConfigReader) Config(ctx context.Context, name string) (web.AgentConfigView, error) {
	if err := workspace.ValidateAgentName(name); err != nil {
		// 非法目录名不可能对应注册中的 Agent：按"未找到"回答（契约此端点只列
		// 401 / 403 / 404，**不**为坏名字另造 400）。
		// A malformed directory name cannot name a registered agent, so it answers 404.
		return web.AgentConfigView{}, &core.AgentNotFoundError{Name: name}
	}
	migration, err := workspace.InspectAgentForMigration(r.deps.Root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return web.AgentConfigView{}, &core.AgentNotFoundError{Name: name}
		}
		// 定义无法解析时结构化视图不成立：空字段会把"读不出来"写成"配置为空"，
		// 页面回落到原文编辑器修复。契约此端点未列 400，故按内部错误上抛（固定文案，
		// 原因只进日志）。
		// An unparsable definition cannot produce a truthful view; the raw editor stays
		// the repair path, and the contract lists no 400 for this endpoint.
		return web.AgentConfigView{}, fmt.Errorf("%s: %w", agentConfigUnreadable, err)
	}
	resolution, err := ResolveNotifyMode(migration.Profile)
	if err != nil {
		return web.AgentConfigView{}, fmt.Errorf("%s: %w", agentConfigUnreadable, err)
	}

	// 稳定身份取状态文件；缺失或损坏时留空——视图照常可读，专属资源会因"无资格"
	// 被点名（与保存期校验同一口径：没有可信身份的 Agent 不能使用专属资源）。
	// The stable identity comes from the state file; without one the view still
	// renders, and exclusive resources fail eligibility exactly like a save would.
	var agentUID core.AgentUID
	if r.deps.Lifecycle != nil {
		if rec, exists, readErr := r.deps.Lifecycle.Read(ctx, name); readErr == nil && exists {
			agentUID = rec.AgentUID
		}
	}

	profile := migration.Profile
	references := make([]web.AgentConfigReferenceView, 0,
		len(profileProviderNames(profile))+len(profile.NotifyChannels)+
			len(profile.ResourceBindings.ModelConnections)+len(profile.ResourceBindings.NotifyChannels))
	for _, row := range configReferenceRows(
		profileProviderNames(profile), profile.ResourceBindings.ModelConnections) {
		ref, refErr := r.reference(ctx, core.BindingModelConnection, row, agentUID)
		if refErr != nil {
			return web.AgentConfigView{}, refErr
		}
		references = append(references, ref)
	}
	for _, row := range configReferenceRows(
		profile.NotifyChannels, profile.ResourceBindings.NotifyChannels) {
		ref, refErr := r.reference(ctx, core.BindingNotifyChannel, row, agentUID)
		if refErr != nil {
			return web.AgentConfigView{}, refErr
		}
		references = append(references, ref)
	}

	return web.AgentConfigView{
		Agent:            name,
		AgentUID:         string(agentUID),
		ContentETag:      workspace.ContentETag(migration.Raw),
		Fields:           agentConfigFields(profile),
		NotifyMode:       string(resolution.Mode),
		NotifyModeSource: notifyModeSource(resolution.Legacy),
		References:       references,
		BindingComplete:  bindingComplete(references),
	}, nil
}

// notifyModeSource 把解析结果投影成契约枚举：显式声明 vs 兼容推断。
// notifyModeSource projects the resolution into the contract enum.
func notifyModeSource(legacy bool) string {
	if legacy {
		return web.NotifyModeSourceLegacy
	}
	return web.NotifyModeSourceExplicit
}

// configReferenceRow 是一条待解析的引用（或缺引用的孤儿绑定）：结构判定只依赖
// 定义本身，资源状态在 `reference` 里查。
// configReferenceRow is one reference (or orphan binding) before resolution.
type configReferenceRow struct {
	refName    string
	uid        core.ResourceUID
	hasBinding bool
	duplicate  bool
	// orphan 表示这条绑定在定义里没有对应引用：它没有可解析的"引用状态"，
	// 直接回报 orphan，不拿 UID 去查仓储（否则一个未知 UID 的孤儿会被写成
	// stale_uid，页面就会去提示"改名或删除"，而正确动作是删掉这条多余绑定）。
	// An orphan has no reference to resolve: it reports orphan without touching the
	// stores, so an unknown uid is not mislabelled stale_uid.
	orphan bool
}

// configReferenceRows 合并"定义里的引用"与"绑定"两侧：以引用为序，缺绑定与重复
// 绑定各占一行；只在绑定侧出现的名称补在末尾并标孤儿（重复则标重复）。顺序稳定，
// 不依赖 map 遍历。
// configReferenceRows merges references and bindings in a stable order; bindings
// without a reference are appended as orphans.
func configReferenceRows(refs []string, bindings []core.ResourceBinding) []configReferenceRow {
	byName := make(map[string][]core.ResourceBinding, len(bindings))
	for _, binding := range bindings {
		byName[binding.Name] = append(byName[binding.Name], binding)
	}
	rows := make([]configReferenceRow, 0, len(refs)+len(bindings))
	declared := make(map[string]bool, len(refs))
	for _, ref := range refs {
		declared[ref] = true
		list := byName[ref]
		switch len(list) {
		case 0:
			rows = append(rows, configReferenceRow{refName: ref})
		case 1:
			rows = append(rows, configReferenceRow{refName: ref, uid: list[0].UID, hasBinding: true})
		default:
			rows = append(rows, configReferenceRow{refName: ref, uid: list[0].UID, hasBinding: true, duplicate: true})
		}
	}
	for _, binding := range bindings {
		if declared[binding.Name] {
			continue
		}
		rows = append(rows, configReferenceRow{
			refName:    binding.Name,
			uid:        binding.UID,
			hasBinding: true,
			duplicate:  len(byName[binding.Name]) > 1,
			orphan:     true,
		})
	}
	return rows
}

// reference 解析一行引用的状态：结构（缺 / 重 / 孤儿）→ 身份墓碑与同名性 →
// 发布与资格 → 凭据身份。任何仓储读失败都整体上抛，**不**把读失败写成"未发布"。
// reference resolves one row; a store failure is reported instead of being folded
// into "not published".
func (r *AgentConfigReader) reference(ctx context.Context, kind core.BindingKind,
	row configReferenceRow, agentUID core.AgentUID,
) (web.AgentConfigReferenceView, error) {
	view := web.AgentConfigReferenceView{
		ResourceKind: string(kind),
		RefName:      row.refName,
		ResourceUID:  string(row.uid),
	}
	switch {
	case !row.hasBinding:
		view.BindingStatus = web.BindingStatusMissing
		return view, nil
	case row.duplicate:
		view.BindingStatus = web.BindingStatusDuplicate
		return view, nil
	case row.orphan:
		// 孤儿绑定没有引用侧，**不**解析身份 / 发布 / 资格：这些字段保持零值，
		// 页面据此提示"删除这条多余绑定"，而不是"该资源已失效"。
		// An orphan has no reference side, so no store lookup happens for it.
		view.BindingStatus = web.BindingStatusOrphan
		return view, nil
	}

	identity, err := r.deps.Identities.IdentityByUID(ctx, row.uid)
	switch {
	case errors.Is(err, core.ErrResourceNotFound):
		view.BindingStatus = web.BindingStatusStaleUID
	case err != nil:
		return view, fmt.Errorf("读取资源身份 %s: %w", row.uid, err)
	default:
		view.ResourceName = identity.Name
		view.Deleted = identity.State == core.IdentityDeleted
		view.BindingStatus = web.BindingStatusBound
		// 同名重建后该 UID 仍是旧的资源实例：名称对不上即"绑定已过期"，
		// 页面提示改用新 UID，**不**自动改绑。
		if view.Deleted || string(identity.Kind) != string(kind) || identity.Name != row.refName {
			view.BindingStatus = web.BindingStatusStaleUID
		}
	}

	if err := r.fillResourceState(ctx, &view, kind, row.uid, agentUID); err != nil {
		return view, err
	}
	if r.deps.Credentials != nil {
		record, credErr := r.deps.Credentials.CredentialByOwner(ctx, kind, row.uid)
		switch {
		case errors.Is(credErr, core.ErrResourceNotFound):
		case credErr != nil:
			return view, fmt.Errorf("读取资源 %s 的凭据身份: %w", row.uid, credErr)
		default:
			// retired 也原样报：它表示"已切回 env"，页面据此提示而不是显示托管。
			view.CredentialState = string(record.State)
		}
	}
	return view, nil
}

// fillResourceState 补齐发布与资格：模型连接读发布指针，通知渠道读渠道行。
// 查不到行时 published / eligible 保持 false——"证明不了"不写成"可以"。
// fillResourceState fills published and eligibility; an absent row keeps both false.
func (r *AgentConfigReader) fillResourceState(ctx context.Context, view *web.AgentConfigReferenceView,
	kind core.BindingKind, uid core.ResourceUID, agentUID core.AgentUID,
) error {
	switch kind {
	case core.BindingModelConnection:
		ptr, err := r.deps.Resources.PointerByUID(ctx, core.ResourceModelConnection, uid)
		if errors.Is(err, core.ErrResourceNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取模型连接 %s 的发布指针: %w", uid, err)
		}
		if ptr.DeletedAt != nil {
			view.Deleted = true
			return nil
		}
		view.Published = ptr.PublishedVersionID != 0
		view.Eligible = core.Eligible(ptr.Scope, ptr.OwnerAgentUID, agentUID)
	case core.BindingNotifyChannel:
		channel, err := r.deps.Notify.GetByUID(ctx, uid)
		if errors.Is(err, core.ErrResourceNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取通知渠道 %s: %w", uid, err)
		}
		if channel.DeletedAt != nil {
			view.Deleted = true
			return nil
		}
		view.Published = true
		view.Eligible = core.Eligible(channel.Scope, channel.OwnerAgentUID, agentUID)
	}
	return nil
}

// agentConfigFields 把已解析的 profile 投影成表单字段；空集合输出 `[]`。
// agentConfigFields projects the parsed profile into the form fields.
func agentConfigFields(profile *core.Profile) web.AgentConfigFieldsView {
	schedules := make([]web.AgentConfigScheduleView, len(profile.Schedules))
	for i, schedule := range profile.Schedules {
		schedules[i] = web.AgentConfigScheduleView{
			ID:        schedule.ID,
			Cron:      schedule.Cron,
			Zone:      schedule.Zone,
			Message:   schedule.Message,
			DeliverTo: schedule.DeliverTo,
		}
	}
	return web.AgentConfigFieldsView{
		Name:        profile.Name,
		Description: profile.Description,
		DisplayName: profile.DisplayName,
		Identity: web.AgentConfigIdentityView{
			AgentName: profile.Identity.AgentName,
			Prompt:    profile.Identity.Prompt,
		},
		Provider: web.AgentConfigProviderView{
			Name:        profile.Provider.Name,
			Model:       profile.Provider.Model,
			Temperature: profile.Provider.Temperature,
		},
		Tools:          nonNilStrings(profile.Tools),
		Skills:         nonNilStrings(profile.Skills),
		MCPServers:     nonNilStrings(profile.MCPServers),
		Bootstrap:      nonNilStrings(profile.Bootstrap),
		NotifyMode:     string(profile.NotifyMode),
		NotifyChannels: nonNilStrings(profile.NotifyChannels),
		ResourceBindings: web.AgentConfigBindingsView{
			ModelConnections: nonNilBindings(profile.ResourceBindings.ModelConnections),
			NotifyChannels:   nonNilBindings(profile.ResourceBindings.NotifyChannels),
		},
		Settings: web.AgentConfigSettingsView{
			MaxIterations:   profile.Settings.MaxIterations,
			MaxHistoryTurns: profile.Settings.MaxHistoryTurns,
		},
		Schedules: schedules,
	}
}

// nonNilBindings 保证空绑定组序列化为 `[]`（与其它集合同一口径）。
// nonNilBindings guarantees an empty group marshals to [].
func nonNilBindings(bindings []core.ResourceBinding) []core.ResourceBinding {
	if bindings == nil {
		return []core.ResourceBinding{}
	}
	return bindings
}

// bindingComplete 判定全部引用是否都"恰有一条有效绑定"：任何一条缺失 / 重复 /
// 孤儿 / 未发布 / 无资格 / 凭据撤销都算不完整（retired = 已切回 env，不算撤销）。
// bindingComplete is true only when every reference pairs with one usable binding.
func bindingComplete(references []web.AgentConfigReferenceView) bool {
	for _, ref := range references {
		if ref.BindingStatus != web.BindingStatusBound || !ref.Published || !ref.Eligible {
			return false
		}
		if ref.CredentialState == string(core.CredentialIdentityRevoked) {
			return false
		}
	}
	return true
}
