package app

// 本文件把"已发布的资源版本"翻译成两个消费方视图（模块卡 §3.1.32）：
//   - `core.RunResources`：准入时在资源闸门读锁内固定 Provider / MCP / 技能版本与
//     运行时，任一绑定资源缺失或不一致 → `*core.ResourceUnavailableError`（失败关闭）；
//   - `web.ResourceReader`：管理台只读面，只回发布状态、可执行名与环境变量键名。
//
// 两者读同一份 `storage.ResourceStore`，版本与运行时来自同一快照；**不得**退回工作
// 副本、**不得**补造空版本号（约束 35 / 36 / 37）。
// This file adapts published resource versions to the run-admission port and the
// read-only console reader. Both read the same store, pin versions and runtime from a
// single snapshot, and fail closed instead of falling back to work copies.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/web"
	"github.com/ymedword/basalt/internal/workspace"
)

// 编译期断言：两个适配器分别满足消费方端口（§3.1.32）。
// Compile-time assertions that both adapters satisfy their consumer-side ports.
var (
	_ core.RunResources  = (*runResources)(nil)
	_ web.ResourceReader = (*resourceViews)(nil)
)

// 准入失败的固定原因（约束 37）：只给分类与修复入口；底层错误原文只经 `Unwrap`
// 暴露（可能带绝对路径），**不得**拼进 Reason。
// Fixed admission reasons; the underlying error stays reachable only through Unwrap.
const (
	resourceReasonProfile         = "agent profile unavailable; fix its AGENT.md and reload"
	resourceReasonProvider        = "published model connection version missing or unusable; re-run the baseline or republish"
	resourceReasonProviderRuntime = "model connection client missing from the published provider map; fix basalt.yaml and restart"
	resourceReasonMCP             = "applied mcp server version missing or unusable; publish it and restart the service"
	resourceReasonMCPRuntime      = "mcp server is not connected in this process; fix its definition and restart"
	resourceReasonSkill           = "published skill version missing or unusable; re-run the baseline or republish"
	// v3 准入新增的三类固定原因（R4.4 / R4.6 / R4.7）：身份未记录、绑定不成立、
	// 通知授权无法冻结、托管凭据不可解析。四者都给修复入口，底层原文只经 Unwrap。
	// The v3 admission reasons; each names the repair, never the raw cause.
	resourceReasonIdentity   = "agent identity is not recorded; re-run the baseline so the lifecycle index carries agent_uid"
	resourceReasonBinding    = "agent resource bindings are missing, invalid or ineligible; fix resource_bindings in AGENT.md and republish the referenced resources"
	resourceReasonNotify     = "notification grant cannot be frozen from the declared notify_mode; fix notify_mode / notify_channels in AGENT.md"
	resourceReasonCredential = "model connection credential is unusable at admission; fix its credential_mode / api_key and republish"
)

// mcpTransportStdio 是 v1 唯一接受的传输方式（与 workspace 的解析门禁同口径）。
// mcpTransportStdio is the only transport v1 accepts.
const mcpTransportStdio = "stdio"

// skillWorkCopyPrefix 是技能工作副本的逻辑路径前缀：发布版本行只记 storage_path
// （`.basalt/versions/skills/<name>/<no>`），WORK COPY 路径由名字派生。同一字面量
// 在 `internal/web` 的清单扁平化里也使用——两处都指向 `.basalt/skills/`。
// The logical work-copy prefix; the same literal backs the run manifest's flattened
// paths in internal/web.
const skillWorkCopyPrefix = ".basalt/skills/"

// pinnedProviderService 是准入消费的模型路由面：`Pinned` 交出的视图必须仍然带两阶段
// 审计（宪法 V），实现可以是冻结快照（`*provider.ProviderService`，测试与轻路径）或
// 运行期目录持有者的活路由（`*catalogProviderRouter`，生产装配）。`Pinned` 在 ResourceGate
// 读锁内被调用，生产实现读的是同一道闸保护下的当前快照（约束 17）。
// pinnedProviderService is the routing face admission consumes; the pinned view keeps
// the two-phase audit, whatever the underlying implementation.
type pinnedProviderService interface {
	Pinned(name string) (*provider.ProviderService, bool)
}

// runResources 是 `core.RunResources` 的唯一实现（§3.1.32 的固定形状）：字段逐字
// 保留——`Skills` 与 `Gate` 在本 US 由构造方注入，技能目录解析只认 `storage_path`。
// runResources is the single implementation of core.RunResources; the frozen field set
// is kept verbatim even where a field is consumed by the next US.
type runResources struct {
	Versions *storage.ResourceStore
	// Providers 是装配期首次构建的目录快照，随运行期保留以便装配诊断；**准入与模型调用
	// 不读它**——路由经 `LLM` 在准入读锁内取当前发布目录（T032），发布换表因此对新 Run
	// 立即生效（约束 16 / 17）。
	// Providers keeps the assembly-time snapshot for diagnostics only; admission routes
	// through LLM against the live catalog.
	Providers *providerCatalog
	// LLM 是唯一的审计路由面（人工 review ② 缺陷 2）：运行时里的 Provider 客户端取它的
	// `Pinned` 固定视图，**不得**直接发放目录里的裸协议客户端——那样两阶段审计被绕过，
	// 且运行内路由不再受准入固定表约束。
	// LLM is the audited routing surface; the runtime hands out its pinned views so a
	// run can neither bypass the audit nor fall back to another provider.
	LLM    pinnedProviderService
	MCP    *mcpRuntime // 本 US 在 mcp.go 引入（发布定义 → 运行时句柄）
	Skills *workspace.SkillLibrary
	Gate   *core.ResourceGate // 与 US-12 资源发布共用的同一实例

	// v3 准入冻结的四件只读依赖（R4.4 / R4.6 / R4.7）：身份镜像、绑定校验、通知渠道与
	// 绑定投影、凭据检查点。全部由装配面注入，**不得**在准入里按名称猜或缺省放行。
	// The read-only dependencies of the v3 admission freeze; every one is injected by the
	// assembly and nothing is inferred from a name.
	Identities  AgentIdentitySource
	NotifyAdmin *storage.NotifyAdminStore
	Channels    NotifyChannelCatalog
	Bindings    NotifyBindingSource
	Credentials BindingCredentialCheck

	// CredentialPins 是托管凭据的准入固定面（T134）；nil 时 managed 定义在
	// credentialPinOf 处失败关闭，不会按 env 运行。
	// CredentialPins pins managed credentials at admission; nil keeps managed failing closed.
	CredentialPins CredentialPinSource

	// Checkpoint 是 Run 内每次模型调用前的凭据检查点（FR-078 / T134）：撤销在下一发
	// 即生效。nil 等价于"本进程没有托管凭据来源"——没有固定版本就产生不了托管 Run。
	// Checkpoint re-checks the pinned credential version before every model call; nil is
	// only legal when no managed credential can exist in this process.
	Checkpoint RunCredentialCheckpoint
}

// AdmissionDeps 是 v3 准入冻结所需的只读依赖集合（与 runResources 的字段一一对应）。
// 单独成结构，避免构造函数长成一串同类型参数而让调用方把两件读面接反。
// AdmissionDeps groups the read-only dependencies of the v3 admission freeze.
type AdmissionDeps struct {
	Identities     AgentIdentitySource
	NotifyAdmin    *storage.NotifyAdminStore
	Channels       NotifyChannelCatalog
	Bindings       NotifyBindingSource
	Credentials    BindingCredentialCheck
	CredentialPins CredentialPinSource
	Checkpoint     RunCredentialCheckpoint
}

// NewRunResources 是准入解析端口的唯一构造点（图 A ⑥）。
// NewRunResources is the only construction point of the admission port.
func NewRunResources(versions *storage.ResourceStore, providers *providerCatalog,
	llm pinnedProviderService, mcp *mcpRuntime, skills *workspace.SkillLibrary,
	gate *core.ResourceGate, deps AdmissionDeps,
) core.RunResources {
	return &runResources{
		Versions:    versions,
		Providers:   providers,
		LLM:         llm,
		MCP:         mcp,
		Skills:      skills,
		Gate:        gate,
		Identities:  deps.Identities,
		NotifyAdmin: deps.NotifyAdmin,
		Channels:    deps.Channels,
		Bindings:    deps.Bindings,
		Credentials: deps.Credentials,

		CredentialPins: deps.CredentialPins,
		Checkpoint:     deps.Checkpoint,
	}
}

// Resolve 在 `Gate.RLock()` 内查发布指针、构造 `ResolvedResources` 并固定运行时
// （卡片行 1631）：Provider 与技能取**发布**指针；MCP 取**已应用**指针（D-12 下
// 未重启前运行仍用旧版本，**不得**降级到新发布版本）；技能真路径取 `storage_path`。
// 版本与运行时必须来自同一快照，任一缺失 / 不一致 → 返回错误，由 core 统一包装为
// `*ResourceUnavailableError`（约束 37）。
// Resolve pins published versions under the gate's read lock: providers and skills use
// the published pointer, MCP uses the applied one. A missing or inconsistent row is an
// error; nothing ever falls back to a work copy.
func (r *runResources) Resolve(ctx context.Context, entry core.AgentEntry) (core.ResolvedResources, error) {
	if entry.Profile == nil {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonProfile, errors.New("agent entry has no profile"))
	}
	profile := entry.Profile

	r.Gate.RLock()
	// 读锁租约随运行时**返回给调用方**（人工 review ② US-12 交接裁决）：成功路径由
	// ReleaseAdmission 在 StartRun 成功后归还，解析中间的任何失败在本方法内归还——
	// 既不让发布插进"版本已固定、账本未落库"的窗口，也不把读锁漏给失败调用方。
	// The admission read lock is handed off with the runtime on success and released
	// here on every failure path, so publish can never slip into the start-run window.
	handedOff := false
	defer func() {
		if !handedOff {
			r.Gate.RUnlock()
		}
	}()

	runtime := &runRuntime{
		providers: make(map[string]core.ProviderClient, 1),
		mcp:       make(map[string]core.MCPHandle, len(profile.MCPServers)),
		gate:      r.Gate,
	}
	resolved := core.ResolvedResources{
		SkillDirs: make(map[string]string, len(profile.Skills)),
		Runtime:   runtime,
	}

	// ① Agent 身份（v3）：准入必须先把名字固定成 `agent_uid`——绑定资格校验与通知授权
	// 冻结都以它为输入，清单也会带上它。索引缺失 / 未回填一律失败关闭，不按名称猜。
	// The agent identity is frozen first: binding eligibility and notify grants both take
	// it as input, and a missing index row fails closed.
	agentUID, err := r.resolveAgentUID(ctx, entry.Name)
	if err != nil {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonIdentity, err)
	}

	providerName := profile.Provider.Name
	if providerName == "" {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonProvider,
			errors.New("agent profile declares no provider"))
	}
	providerVersion, err := r.publishedVersion(ctx, core.ResourceModelConnection, providerName)
	if err != nil {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonProvider, err)
	}
	client, ok := r.providerClient(providerName)
	if !ok {
		// 指针在、目录里没有：发布映射与资源账来自同一次装配，缺条目说明装配被绕过。
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonProviderRuntime,
			fmt.Errorf("provider %s 不在已发布映射表中", providerName))
	}
	resolved.Provider = versionRef(providerName, providerVersion)
	runtime.providers[providerName] = client

	// ② 模型名与凭据固定（契约 ProviderVersionRef）：模型取 AGENT.md 冻结的那一个
	// （运行实际发出的就是它），凭据按发布定义的模式固定；env 模式的目标绑定哈希由
	// 规范化 base_url 派生，托管模式在缺解析器时失败关闭（T130 接线后走凭据仓储）。
	// The model is the one frozen in AGENT.md; the credential pin follows the published
	// definition's mode, and managed mode fails closed until the resolver is assembled.
	model := strings.TrimSpace(profile.Provider.Model)
	if model == "" {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonProvider,
			errors.New("agent profile declares no provider model"))
	}
	resolved.ProviderModel = model
	definition, err := parseProviderDefinition(providerName, providerVersion.DefinitionYAML)
	if err != nil {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonProvider, err)
	}
	pin, err := credentialPinOf(ctx, providerName, providerVersion.ResourceUID, definition, r.CredentialPins)
	if err != nil {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonCredential, err)
	}
	resolved.ProviderCredential = pin

	for _, name := range profile.MCPServers {
		ref, err := r.resolveMCP(ctx, name)
		if err != nil {
			return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonMCP, err)
		}
		resolved.MCPServers = append(resolved.MCPServers, ref)
		runtime.mcp[name] = mcpHandle(name)
	}

	for _, name := range profile.Skills {
		ref, dir, err := r.resolveSkill(ctx, name)
		if err != nil {
			return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonSkill, err)
		}
		resolved.Skills = append(resolved.Skills, ref)
		resolved.SkillDirs[name] = dir
	}

	// ③ 绑定校验（R4.4）：引用与绑定严格一一对应，UID 是当前指针、有资格、凭据可用。
	// 放在版本固定之后：资源本身缺失时先报资源原因，绑定层不掩盖更具体的修复入口。
	// The binding check runs after the version pins so a missing resource keeps its own
	// repair hint instead of being masked by the binding layer.
	if err := CheckAgentBindings(ctx, BindingCheckDeps{
		Resources:   r.Versions,
		Notify:      r.NotifyAdmin,
		Credentials: r.Credentials,
	}, entry.Name, agentUID, profile); err != nil {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonBinding, err)
	}

	// ④ 通知授权冻结（R4.7）：三态按兼容规则归一后枚举成**具体渠道 UID**，
	// 集合里永远没有通配；`none` 不读目录。冻结结果随清单落库，运行中改文件不影响它。
	// The notification grant is frozen into concrete channel uids; none carries no lookup.
	notify, err := FreezeNotifyGrant(ctx, NotifyGrantDeps{Channels: r.Channels, Bindings: r.Bindings},
		agentUID, profile)
	if err != nil {
		return core.ResolvedResources{}, unavailable(entry.Name, resourceReasonNotify, err)
	}
	resolved.Notify = notify

	handedOff = true
	return resolved, nil
}

// resolveAgentUID 读生命周期索引里的 Agent 身份并校验形态：索引缺失与未回填是两回事，
// 但都拒绝准入——清单的 `agent_uid` 不得为空，也不得按名称临时生成一个新身份。
// resolveAgentUID reads the lifecycle index and validates the uid; a missing row and an
// un-recorded uid both refuse admission instead of minting an identity from the name.
func (r *runResources) resolveAgentUID(ctx context.Context, agent string) (core.AgentUID, error) {
	if r.Identities == nil {
		return "", errors.New("准入解析未装配 Agent 身份来源")
	}
	uid, _, found, err := r.Identities.AgentIdentity(ctx, agent)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("生命周期索引没有 Agent %s 的行", agent)
	}
	if _, err := core.ParseAgentUID(string(uid)); err != nil {
		return "", fmt.Errorf("身份未记录（Agent %s）: %w", agent, err)
	}
	return uid, nil
}

// credentialPinOf 按发布定义固定凭据引用：未声明 `credential_mode` 的旧定义按 env
// 读取（与字段清单「创建未声明 mode 只按 env 规则」同口径）；托管模式必须经
// `CredentialPinSource` 取**当前** active 版本——装配缺失即失败关闭，**不得**把
// managed 静默降级成 env 继续跑，也不得改钉另一个版本。
// credentialPinOf defaults an undeclared mode to env and pins managed definitions
// through the credential source; a missing source keeps managed failing closed.
func credentialPinOf(ctx context.Context, name string, uid core.ResourceUID,
	definition resourceProviderDefinition, pins CredentialPinSource,
) (core.CredentialPin, error) {
	raw := strings.TrimSpace(definition.CredentialMode)
	if raw == "" {
		raw = string(core.CredentialModeEnv)
	}
	mode, err := core.ParseCredentialMode(raw)
	if err != nil {
		return core.CredentialPin{}, fmt.Errorf("模型连接 %s 的凭据模式非法: %w", name, err)
	}
	if mode == core.CredentialModeManaged {
		if pins == nil {
			return core.CredentialPin{}, fmt.Errorf("模型连接 %s 使用托管凭据，但本进程尚未装配凭据解析器", name)
		}
		target, err := CredentialTargetBinding(core.BindingModelConnection, definition.BaseURL, "")
		if err != nil {
			return core.CredentialPin{}, err
		}
		pin, err := pins.PinModelCredential(ctx, uid, target)
		if err != nil {
			return core.CredentialPin{}, fmt.Errorf("模型连接 %s 的托管凭据不可固定: %w", name, err)
		}
		return pin, nil
	}
	return core.CredentialPin{
		Mode: core.CredentialModeEnv,
		TargetBinding: core.TargetBinding{
			Kind:    core.BindingModelConnection,
			BaseURL: definition.BaseURL,
		}.Hash(),
	}, nil
}

// providerClient 取该 Provider 的**审计固定视图**（人工 review ② 缺陷 2）：目录里的
// 裸协议客户端只用于构造路由服务，运行时**不得**直接发放它——那样两阶段审计被绕过，
// 且运行内路由不再被准入固定表约束。未接审计服务（nil）一律失败关闭，不退回裸客户端。
// providerClient hands out the audited pinned view; a missing audit service fails closed
// instead of falling back to a raw protocol client.
func (r *runResources) providerClient(name string) (core.ProviderClient, bool) {
	// 接口判空之外还要判"交出的视图为空"：typed-nil 的实现在这里等价于没接路由，
	// 两类缺失都按失败关闭处理（准入不得退回裸客户端）。
	if r.LLM == nil {
		return nil, false
	}
	pinned, ok := r.LLM.Pinned(name)
	if !ok || pinned == nil {
		return nil, false
	}
	if r.Checkpoint == nil {
		return pinned, true
	}
	return &guardedProviderClient{inner: pinned, checkpoint: r.Checkpoint}, true
}

// guardedProviderClient 是交给运行时的模型调用面：每次调用先过 Run 凭据检查点
// （FR-078），再进两阶段审计的固定视图。检查在审计预写之前——撤销即"拒绝发出"，
// 不留下一次从未发生的调用行，也不回落到 env 或其他版本。
// guardedProviderClient runs the run's credential checkpoint before every call, ahead of
// the audit pre-write, so a revoked credential is refused before anything is emitted.
type guardedProviderClient struct {
	inner      core.ProviderClient
	checkpoint RunCredentialCheckpoint
}

// 编译期断言：带检查点的客户端仍是模型客户端。
// Compile-time assertion that the guarded client stays a model client.
var _ core.ProviderClient = (*guardedProviderClient)(nil)

// Chat 先复核 Run 固定的凭据版本，再交给固定视图发出。
// Chat checks the pinned credential and then delegates to the pinned view.
func (c *guardedProviderClient) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	if err := c.checkpoint.CheckRun(ctx, req.RunID); err != nil {
		return core.ChatResponse{}, err
	}
	return c.inner.Chat(ctx, req)
}

// resolveMCP 取 MCP 的**已应用**版本：发布指针可以指向尚未生效的新版本（D-12），
// 运行必须用进程里真正连接着的那个版本，否则清单会记一个从未建立的连接。
// resolveMCP pins the applied version, never the freshly published one.
func (r *runResources) resolveMCP(ctx context.Context, name string) (core.ResourceVersionRef, error) {
	ptr, err := r.visiblePointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		return core.ResourceVersionRef{}, err
	}
	if ptr.AppliedVersionID == nil {
		return core.ResourceVersionRef{}, fmt.Errorf("%w: mcp_server/%s 尚无已应用版本", core.ErrResourceNotFound, name)
	}
	version, err := r.versionOf(ctx, core.ResourceMCPServer, name, *ptr.AppliedVersionID)
	if err != nil {
		return core.ResourceVersionRef{}, err
	}
	if err := expectSameResourceUID(core.ResourceMCPServer, name, ptr, version); err != nil {
		return core.ResourceVersionRef{}, err
	}
	if !r.mcpConnected(name) {
		return core.ResourceVersionRef{}, fmt.Errorf("mcp_server/%s 未在本进程连接", name)
	}
	return versionRef(name, version), nil
}

// resolveSkill 取技能的发布版本、完整文件清单与不可变目录：目录取 `storage_path`
// （约束 31 的契约字面量），文件清单来自版本行的 `manifest_json`；返回的切片非 nil，
// 空清单序列化为 `[]`。
// resolveSkill returns the published version, its non-nil file list and the immutable
// directory taken verbatim from storage_path.
func (r *runResources) resolveSkill(ctx context.Context, name string) (core.SkillVersionRef, string, error) {
	version, err := r.publishedVersion(ctx, core.ResourceSkill, name)
	if err != nil {
		return core.SkillVersionRef{}, "", err
	}
	if strings.TrimSpace(version.StoragePath) == "" {
		return core.SkillVersionRef{}, "", fmt.Errorf("技能 %s 的发布版本缺少存储路径", name)
	}
	files, err := parseSkillManifest(name, version.ManifestJSON)
	if err != nil {
		return core.SkillVersionRef{}, "", err
	}
	return core.SkillVersionRef{
		ResourceVersionRef: versionRef(name, version),
		Files:              files,
	}, version.StoragePath, nil
}

// runSnapshotBuilder 是 `core.RunSnapshotBuilder` 的生产实现（约束 31 / 图 F）：把准入
// 解析结果里的 `SkillDirs`（技能名 → 发布目录 `storage_path`）原样交给配置装配器，卡片
// 与哈希因此读自**发布版本目录**，不读工作副本。缺失映射在装配器一侧失败关闭。
// runSnapshotBuilder folds the admission result into the run's frozen config snapshot:
// cards and hashes come from the pinned published skill directories, never the work copy.
type runSnapshotBuilder struct {
	loader *workspace.ContextLoader
	root   string
	now    func() time.Time
}

// 编译期断言：装配器满足运行入口消费的冻结端口。
// Compile-time assertion that the builder satisfies the consumer-side port.
var _ core.RunSnapshotBuilder = (*runSnapshotBuilder)(nil)

// newRunSnapshotBuilder 构造发布版本快照构建器；nil 时钟回落 time.Now（与装配面同一口径）。
// newRunSnapshotBuilder builds the published-version snapshot builder.
func newRunSnapshotBuilder(loader *workspace.ContextLoader, root string, now func() time.Time) *runSnapshotBuilder {
	if now == nil {
		now = time.Now
	}
	return &runSnapshotBuilder{loader: loader, root: root, now: now}
}

// Build 冻结一次运行的配置：解析结果只消费 `SkillDirs`（版本号已在准入时固定，本处不再
// 查库）；工作副本路径不会出现在卡片行与清单里。
// Build freezes one run's config; only SkillDirs is consumed, so no version can drift here.
func (b *runSnapshotBuilder) Build(_ context.Context, entry core.AgentEntry, resolved core.ResolvedResources) (core.ConfigSnapshot, error) {
	snapshot, err := b.loader.SnapshotPublished(b.root, entry, workspace.PublishedSkills(resolved.SkillDirs), b.now())
	if err != nil {
		return core.ConfigSnapshot{}, err
	}
	return *snapshot, nil
}

// publishedVersion 读发布指针指向的版本并核对归属（kind + resource_id 必须逐字相符，
// 资源 UID 也必须与当前指针一致——版本行属于另一代实例是数据事故，**不得**继续准入）。
// publishedVersion verifies that the pointer's version belongs to the same resource and
// to the very identity the name currently points at.
func (r *runResources) publishedVersion(ctx context.Context, kind core.ResourceKind, id string) (core.ResourceVersion, error) {
	ptr, err := r.visiblePointer(ctx, kind, id)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	version, err := r.versionOf(ctx, kind, id, ptr.PublishedVersionID)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	if err := expectSameResourceUID(kind, id, ptr, version); err != nil {
		return core.ResourceVersion{}, err
	}
	return version, nil
}

// expectSameResourceUID 核对版本行与当前指针属于同一个资源实例。指针尚未回填 UID 的
// 旧库不在这里另判：清单构造会因 UID 非法失败关闭，错误入口更准确。
// expectSameResourceUID checks that the version row and the pointer share one identity;
// an un-backfilled pointer is left to the manifest builder's own fail-closed check.
func expectSameResourceUID(kind core.ResourceKind, id string, ptr core.ResourcePointer,
	version core.ResourceVersion,
) error {
	if ptr.ResourceUID == "" {
		return nil
	}
	if version.ResourceUID != ptr.ResourceUID {
		return fmt.Errorf("资源 %s/%s 的版本 %d 属于 %s，当前指针指向 %s",
			kind, id, version.ID, version.ResourceUID, ptr.ResourceUID)
	}
	return nil
}

// visiblePointer 读一个未删除的发布指针；缺失与已删除同样按"资源不存在"报告。
// visiblePointer treats a deleted pointer as missing.
func (r *runResources) visiblePointer(ctx context.Context, kind core.ResourceKind, id string) (core.ResourcePointer, error) {
	if id == "" {
		return core.ResourcePointer{}, fmt.Errorf("%w: %s 名为空", core.ErrResourceNotFound, kind)
	}
	ptr, err := r.Versions.Pointer(ctx, kind, id)
	if err != nil {
		return core.ResourcePointer{}, err
	}
	if ptr.DeletedAt != nil {
		return core.ResourcePointer{}, fmt.Errorf("%w: %s/%s 已删除", core.ErrResourceNotFound, kind, id)
	}
	return ptr, nil
}

// versionOf 读版本行并核对它属于 `(kind, id)`。
// versionOf loads the version row and checks its owner.
func (r *runResources) versionOf(ctx context.Context, kind core.ResourceKind, id string, versionID int64) (core.ResourceVersion, error) {
	version, err := r.Versions.VersionByID(ctx, versionID)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	if version.Kind != kind || version.ResourceID != id {
		return core.ResourceVersion{}, fmt.Errorf("资源 %s/%s 的版本 %d 属于 %s/%s",
			kind, id, version.ID, version.Kind, version.ResourceID)
	}
	return version, nil
}

// mcpConnected 判断 MCP server 在本进程是否真的连上：没有连接就没有可固定的句柄。
// mcpConnected reports whether the runtime actually holds a connection.
func (r *runResources) mcpConnected(name string) bool {
	if r.MCP == nil {
		return false
	}
	return slices.Contains(r.MCP.ConnectedNames(), name)
}

// versionRef 把版本行投影成清单引用（名字取资源名，UID 是版本行自身的身份，版本号
// 逐资源从 1 起）。清单不接受空 UID——由 `BuildRunResourceManifest` 失败关闭。
// versionRef projects a version row onto the manifest reference, carrying the row's own
// resource uid; an empty uid is rejected later by the manifest builder.
func versionRef(name string, version core.ResourceVersion) core.ResourceVersionRef {
	return core.ResourceVersionRef{
		Name:        name,
		ResourceUID: version.ResourceUID,
		VersionID:   version.ID,
		VersionNo:   version.VersionNo,
		ContentHash: version.ContentHash,
	}
}

// parseSkillManifest 解析版本行的 `manifest_json`：必须是文件数组。空清单写成 `[]`
// 是合法的空列表，`null` / 空原文则说明发布行已损坏——按失败关闭报错，不当成空技能
// 继续（否则会冻结一个没有任何文件、运行期必然找不到 SKILL.md 的版本）。
// parseSkillManifest requires a file array: `[]` is a valid empty list, while null or
// an empty document is a corrupt row and fails closed.
func parseSkillManifest(name, raw string) ([]core.FileHash, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("技能 %s 的发布版本缺少文件清单", name)
	}
	var files []core.FileHash
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return nil, fmt.Errorf("技能 %s 的文件清单无法解析: %w", name, err)
	}
	if files == nil {
		return nil, fmt.Errorf("技能 %s 的文件清单不是数组", name)
	}
	for _, file := range files {
		if file.Path == "" {
			return nil, fmt.Errorf("技能 %s 的文件清单含空路径", name)
		}
	}
	return files, nil
}

// unavailable 构造准入失败分类（约束 37）：Reason 固定给修复入口，Cause 只经
// `Unwrap` 暴露。
// unavailable builds the fixed admission failure with the cause reachable only via Unwrap.
func unavailable(agent, reason string, cause error) error {
	return &core.ResourceUnavailableError{Name: agent, Reason: reason, Cause: cause}
}

// runRuntime 是准入时固定的不可变运行时视图：Provider 客户端映射与 MCP 句柄都在
// 构造后只读。`Release` 幂等——重复调用不再产生副作用；查找在释放后一律返回 false
// （不再发放已经被归还的引用）。引用计数与回收归 US-12 的 supervisor。
//
// 另持有准入读锁的租约（`gate`）：`ReleaseAdmission` 归还读锁而保留运行时（账本
// 落库成功后运行仍要用固定视图），`Release` 先归还读锁再作废运行时。两者共用同一个
// `sync.Once`——先 ReleaseAdmission 后 Release 不会重复 RUnlock（会 panic）。
// The runtime also owns the admission read-lock lease: ReleaseAdmission returns the
// lock but keeps the view alive for the run; Release returns both, exactly once.
type runRuntime struct {
	providers map[string]core.ProviderClient
	mcp       map[string]core.MCPHandle
	gate      *core.ResourceGate
	released  atomic.Bool
	once      sync.Once
	// admissionOnce 保证读锁只归还一次；nil gate（测试构造的零值）时为空操作。
	// admissionOnce makes the single RUnlock idempotent; a nil gate is a no-op.
	admissionOnce sync.Once
}

// Provider 返回本次运行固定的客户端；释放后不再发放。
// Provider returns the pinned client, never after Release.
func (r *runRuntime) Provider(name string) (core.ProviderClient, bool) {
	if r == nil || r.released.Load() {
		return nil, false
	}
	client, ok := r.providers[name]
	return client, ok
}

// MCP 返回本次运行固定的 MCP 句柄；释放后不再发放。
// MCP returns the pinned handle, never after Release.
func (r *runRuntime) MCP(name string) (core.MCPHandle, bool) {
	if r == nil || r.released.Load() {
		return nil, false
	}
	handle, ok := r.mcp[name]
	return handle, ok
}

// ReleaseAdmission 归还准入读锁，运行时本身保持可用（幂等）。StartRun 成功后由
// 运行入口调用——读锁覆盖"版本已固定、账本与清单已落库"的整个准入窗口。
// ReleaseAdmission returns the admission read lock and keeps the view alive.
func (r *runRuntime) ReleaseAdmission() {
	if r == nil || r.gate == nil {
		return
	}
	r.admissionOnce.Do(r.gate.RUnlock)
}

// Release 归还本次运行的运行时引用（幂等）。映射本身不清空：并发查找与释放之间
// 不能对同一张 map 读写；释放后由原子标志把后续查找全部挡掉。读锁若尚未归还
// （解析后的失败路径）在此一并归还——失败路径只需要一个收尾调用。
// Release flips the released flag once and also returns an unreturned admission lock.
func (r *runRuntime) Release() {
	if r == nil {
		return
	}
	r.ReleaseAdmission()
	r.once.Do(func() { r.released.Store(true) })
}

// mcpHandle 只是 server 名的只读包装：本 US 的 `core.MCPHandle` 只要求报名字，
// 句柄生命周期归 US-12。
// mcpHandle wraps the server name; the handle lifecycle belongs to US-12.
type mcpHandle string

// ServerName 返回该句柄对应的 MCP server 名。
// ServerName reports the server this handle pins.
func (h mcpHandle) ServerName() string { return string(h) }

// resourceViews 是 `web.ResourceReader` 的唯一实现（图 A ⑨）：只读投影发布指针、
// 版本行与运行期连接视图。`lifecycle` 按卡片固定签名注入，留给 US-12 的资源写面
// 与生命周期联动——六个只读方法不读它；`credentials` 只供条件 ETag 的凭据段
// （FR-081）读取，nil 表示没有托管凭据（测试夹具 / 只读投影的半截运行期）。
// resourceViews projects pointers, version rows and the live connection view; the
// credential store is read only for the conditional ETag, and nil means no managed
// credentials in this assembly.
type resourceViews struct {
	versions       *storage.ResourceStore
	lifecycle      *workspace.LifecycleStore
	mcp            *mcpRuntime
	credentials    *storage.CredentialStore
	mcpCredentials *MCPCredentialService
	tests          core.ResourceConnectionTestStore
	lookupEnv      func(string) (string, bool)
}

// ResourceViewDeps 是管理读面回答「本地就绪 / 上次实测」所需的两个可选来源（R7 范围
// 修订）：`ConnectionTests` 为 nil 表示本次装配不保留投影（历史仍在管理操作台账）；
// `LookupEnv` 为 nil 时回落 `os.LookupEnv`——与发送路径、准入解析同一真值来源。
// ResourceViewDeps supplies the optional readiness sources; a nil lookup falls back to
// os.LookupEnv, the same source admission and the send path resolve.
type ResourceViewDeps struct {
	ConnectionTests core.ResourceConnectionTestStore
	LookupEnv       func(string) (string, bool)
	// MCPCredentials 提供逐槽位的凭据来源与状态投影（契约 `McpCredentialKeyView`）。nil
	// 表示本次装配没有凭据面：`credential_keys` 如实为空集合，**不**伪造状态。
	// MCPCredentials supplies the per-slot credential projection; nil leaves it empty.
	MCPCredentials *MCPCredentialService
}

// NewResourceViews 是图 A 中 `resourceViews` 的唯一构造点（卡片行 1632）。
// NewResourceViews is the only construction point of the console-side reader.
func NewResourceViews(versions *storage.ResourceStore, lifecycle *workspace.LifecycleStore,
	mcp *mcpRuntime, credentials *storage.CredentialStore, deps ResourceViewDeps,
) web.ResourceReader {
	lookupEnv := deps.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	return &resourceViews{
		versions: versions, lifecycle: lifecycle, mcp: mcp, credentials: credentials,
		mcpCredentials: deps.MCPCredentials,
		tests:          deps.ConnectionTests, lookupEnv: lookupEnv,
	}
}

// Skills 列出未删除的发布指针：顺序取 `ListPointers` 的 resource_id 升序，本层不重排。
// Skills lists every live skill pointer in storage order.
func (v *resourceViews) Skills(ctx context.Context) ([]web.SkillView, error) {
	pointers, err := v.versions.ListPointers(ctx, core.ResourceSkill)
	if err != nil {
		return nil, err
	}
	views := make([]web.SkillView, 0, len(pointers))
	for _, ptr := range pointers {
		view, err := v.skillView(ctx, ptr)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// Skill 返回单个技能；缺失或已删除 → `core.ErrResourceNotFound`。
// Skill returns one skill or ErrResourceNotFound.
func (v *resourceViews) Skill(ctx context.Context, name string) (web.SkillView, error) {
	ptr, err := v.livePointer(ctx, core.ResourceSkill, name)
	if err != nil {
		return web.SkillView{}, err
	}
	return v.skillView(ctx, ptr)
}

// skillView 投影一个技能指针：path 是工作副本逻辑路径，etag 由发布版本
// content_hash 生成（格式与内容 ETag 相同），file_count 取清单长度。description 在
// 本 US 没有可达来源（只读端口读不到 SKILL.md 正文），保持空串——见 run-state I-60。
// skillView projects one skill pointer; description stays empty because the frozen
// port has no reachable source for it.
func (v *resourceViews) skillView(ctx context.Context, ptr core.ResourcePointer) (web.SkillView, error) {
	version, err := v.versionOf(ctx, core.ResourceSkill, ptr.ResourceID, ptr.PublishedVersionID)
	if err != nil {
		return web.SkillView{}, err
	}
	files, err := parseSkillManifest(ptr.ResourceID, version.ManifestJSON)
	if err != nil {
		return web.SkillView{}, err
	}
	referenced, err := v.versions.ReferencedBy(ctx, core.ResourceSkill, ptr.ResourceID)
	if err != nil {
		return web.SkillView{}, err
	}
	etag, err := publishedResourceETag(ctx, v.credentials, ptr, version)
	if err != nil {
		return web.SkillView{}, err
	}
	return web.SkillView{
		Name:         ptr.ResourceID,
		Path:         skillWorkCopyPrefix + ptr.ResourceID,
		VersionID:    version.ID,
		VersionNo:    version.VersionNo,
		ContentHash:  version.ContentHash,
		ETag:         etag,
		FileCount:    len(files),
		ReferencedBy: nonNilStrings(referenced),
	}, nil
}

// MCPServers 列出 MCP server：全集 = 未删除的发布指针 ∪ 运行期已配置名单（US-12 新
// 登记的 server 在重启前只存在于指针里，**不得**因未进运行时而从列表消失），按名升序。
// MCPServers lists the union of live pointers and configured names, sorted by name.
func (v *resourceViews) MCPServers(ctx context.Context) ([]web.McpServerView, error) {
	pointers, err := v.versions.ListPointers(ctx, core.ResourceMCPServer)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pointers)+len(v.configuredMCPNames()))
	seen := make(map[string]bool, len(pointers))
	for _, ptr := range pointers {
		if seen[ptr.ResourceID] {
			continue
		}
		seen[ptr.ResourceID] = true
		names = append(names, ptr.ResourceID)
	}
	for _, name := range v.configuredMCPNames() {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	slices.Sort(names)
	views := make([]web.McpServerView, 0, len(names))
	for _, name := range names {
		projection, err := v.projectMCPServer(ctx, name)
		if err != nil {
			return nil, err
		}
		views = append(views, projection.view)
	}
	return views, nil
}

// McpServer 返回单个 MCP server：既无指针也不在配置名单 → `core.ErrResourceNotFound`。
// ETag 取**发布版本** content_hash 的内容 ETag（人工 review ② F3），只经响应头出站；
// `args` / `url` 取自同一发布定义，按契约 `McpServerDetail` 进详情响应体，供编辑
// 表单预填（2026-10-25 US-14 走查裁决）。无指针（未入账的配置项）时 ETag 为空串、
// 两个字段取零值，**不伪造**。
// McpServer returns one server: the published definition supplies args/url for the
// detail body, while the ETag leaves the process only as a response header.
func (v *resourceViews) McpServer(ctx context.Context, name string) (web.McpServerDetail, error) {
	_, found, err := v.optionalPointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		return web.McpServerDetail{}, err
	}
	if !found && !slices.Contains(v.configuredMCPNames(), name) {
		return web.McpServerDetail{}, fmt.Errorf("%w: mcp_server/%s", core.ErrResourceNotFound, name)
	}
	projection, err := v.projectMCPServer(ctx, name)
	if err != nil {
		return web.McpServerDetail{}, err
	}
	detail := web.McpServerDetail{View: projection.view, ETag: projection.etag}
	if projection.definition != nil {
		detail.Args = projection.definition.Args
		detail.URL = projection.definition.URL
	}
	return detail, nil
}

// mcpServerProjection 汇总 MCP server 的一次投影：公开视图、详情专用 ETag 与发布
// 定义。`args` / `url` 只进详情响应体（契约 McpServerDetail），**不**进列表视图。
// mcpServerProjection bundles the view, the detail-only ETag and the published
// definition; args/url reach the detail body only.
type mcpServerProjection struct {
	view       web.McpServerView
	etag       string
	definition *resourceMCPDefinition
}

// projectMCPServer 投影一个 MCP server：定义字段取**发布**版本（页面展示"当前定义"，
// 未生效由 pending_restart 表达）；连接与工具名取运行期；无指针时版本字段全零、
// definition 为 nil，**不伪造**版本号。env 只出键名（约束 35）；ETag 由发布版本
// 内容生成，无指针时为空串。详情端点据此补齐 `args` / `url`（2026-10-25 走查裁决）。
// projectMCPServer projects one server: definition fields come from the published
// version, connectivity from the runtime; a missing pointer leaves zero fields and a
// nil definition. The ETag is derived from the published version.
func (v *resourceViews) projectMCPServer(ctx context.Context, name string) (mcpServerProjection, error) {
	view := web.McpServerView{
		Name:      name,
		Transport: mcpTransportStdio,
		EnvKeys:   []string{},
		// 请求头只出名字、不出值；逐占位符的凭据来源与状态在凭据投影接入前如实为空集合
		// （空集合输出 `[]`，不是 `null`）。
		// Header names only; credential-key rows stay an empty array until the
		// per-placeholder projection is wired.
		HeaderKeys:     []string{},
		CredentialKeys: []web.McpCredentialKeyView{},
		// `Tools` 保持 offered 语义（注册前 server 声明的全量名单，D-06）；被谁抢走、
		// 为什么跳过由 `ToolDisposition` 单独表达，不在这里过滤——否则页面无法区分
		// "server 没提供"与"提供了但没赢"。
		// Tools stays offered-only; the registration outcome lives in ToolDisposition.
		Tools:           v.mcpTools(name),
		Connected:       slices.Contains(v.connectedMCPNames(), name),
		ToolDisposition: web.MCPToolDispositionViewOf(v.mcp.ToolDisposition(name)),
	}
	ptr, found, err := v.optionalPointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		return mcpServerProjection{}, err
	}
	if !found {
		return mcpServerProjection{view: view}, nil
	}
	version, err := v.versionOf(ctx, core.ResourceMCPServer, name, ptr.PublishedVersionID)
	if err != nil {
		return mcpServerProjection{}, err
	}
	definition, err := parseMCPDefinition(name, version.DefinitionYAML)
	if err != nil {
		return mcpServerProjection{}, err
	}
	view.Transport = mcpTransport(definition.Transport)
	view.Command = mcpExecutable(definition.Command)
	view.EnvKeys = sortedEnvKeys(definition.Env)
	// 请求头同样只出名字；逐槽位的来源与状态取**声明的来源**，不按"有没有托管行"推断。
	view.HeaderKeys = sortedEnvKeys(definition.Headers)
	if v.mcpCredentials != nil {
		statuses, statusErr := v.mcpCredentials.Statuses(ctx, ptr.ResourceUID, core.MCPServerConfig{
			Name:      name,
			Transport: definition.Transport,
			Command:   definition.Command,
			Args:      definition.Args,
			URL:       definition.URL,
			Env:       definition.Env,
			Headers:   definition.Headers,
		})
		if statusErr != nil {
			return mcpServerProjection{}, statusErr
		}
		view.CredentialKeys = mcpCredentialKeyViews(statuses)
	}
	view.PublishedVersionID = version.ID
	view.PublishedVersionNo = version.VersionNo
	view.AppliedVersionID = derefVersionID(ptr.AppliedVersionID)
	if ptr.AppliedVersionID != nil {
		applied, err := v.versionOf(ctx, core.ResourceMCPServer, name, *ptr.AppliedVersionID)
		if err != nil {
			return mcpServerProjection{}, err
		}
		view.AppliedVersionNo = applied.VersionNo
	}
	view.PendingRestart = mcpRestartPending(ptr)
	etag, err := publishedResourceETag(ctx, v.credentials, ptr, version)
	if err != nil {
		return mcpServerProjection{}, err
	}
	return mcpServerProjection{view: view, etag: etag, definition: &definition}, nil
}

// ModelConnections 列出未删除的模型连接指针；顺序取 `ListPointers` 升序。
// ModelConnections lists every live model connection pointer in storage order.
func (v *resourceViews) ModelConnections(ctx context.Context) ([]web.ModelConnectionView, error) {
	pointers, err := v.versions.ListPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return nil, err
	}
	tests, err := v.connectionTestIndex(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]web.ModelConnectionView, 0, len(pointers))
	for _, ptr := range pointers {
		view, err := v.modelConnectionView(ctx, ptr, tests)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// ModelConnection 返回单个模型连接；缺失或已删除 → `core.ErrResourceNotFound`。
// ModelConnection returns one connection or ErrResourceNotFound.
func (v *resourceViews) ModelConnection(ctx context.Context, name string) (web.ModelConnectionView, error) {
	ptr, err := v.livePointer(ctx, core.ResourceModelConnection, name)
	if err != nil {
		return web.ModelConnectionView{}, err
	}
	tests, err := v.connectionTestIndex(ctx)
	if err != nil {
		return web.ModelConnectionView{}, err
	}
	return v.modelConnectionView(ctx, ptr, tests)
}

// connectionTestIndex 一次读齐全部模型连接的最后测试结果（UID → 记录）；未装配投影
// 仓储时返回 nil，页面据 `last_test = null` 显示「从未测试」。
// connectionTestIndex loads every model connection's last test at once.
func (v *resourceViews) connectionTestIndex(ctx context.Context) (map[core.ResourceUID]core.ResourceConnectionTest, error) {
	if v.tests == nil {
		return nil, nil
	}
	rows, err := v.tests.ListConnectionTests(ctx, core.ResourceModelConnection)
	if err != nil {
		return nil, fmt.Errorf("读取模型连接的最后连接测试: %w", err)
	}
	index := make(map[core.ResourceUID]core.ResourceConnectionTest, len(rows))
	for _, row := range rows {
		index[row.ResourceUID] = row
	}
	return index, nil
}

// modelConnectionAvailability 只回答「本地配置看起来能不能用」：托管模式要求 active
// 的凭据身份，env 模式要求 `${VAR}` 引用存在且环境变量非空。它**不**代表实测可用——
// 那由 last_test 单独给出；凭证缺失时页面因此显示"不可用"而不是"已验证"。
// modelConnectionAvailability answers local readiness only; live availability is the
// separate last_test record.
func (v *resourceViews) modelConnectionAvailability(definition resourceProviderDefinition,
	digest *credentialDigest,
) string {
	if digest != nil {
		if digest.IdentityState == core.CredentialIdentityActive {
			return core.AvailabilityReady
		}
		return core.AvailabilityUnavailable
	}
	name, ok := core.EnvPlaceholderName(definition.APIKey)
	if !ok {
		return core.AvailabilityUnavailable
	}
	if value, found := v.lookupEnv(name); !found || strings.TrimSpace(value) == "" {
		return core.AvailabilityUnavailable
	}
	return core.AvailabilityReady
}

// modelConnectionView 投影一个模型连接：api_key_env 只出 `${ENV_VAR}` 引用名，
// 非占位形态（或未配置）为空串——解析后的凭证值**永不**进入视图（约束 35）。
// modelConnectionView exposes the env reference name only.
func (v *resourceViews) modelConnectionView(ctx context.Context, ptr core.ResourcePointer,
	tests map[core.ResourceUID]core.ResourceConnectionTest,
) (web.ModelConnectionView, error) {
	version, err := v.versionOf(ctx, core.ResourceModelConnection, ptr.ResourceID, ptr.PublishedVersionID)
	if err != nil {
		return web.ModelConnectionView{}, err
	}
	definition, err := parseProviderDefinition(ptr.ResourceID, version.DefinitionYAML)
	if err != nil {
		return web.ModelConnectionView{}, err
	}
	referenced, err := v.versions.ReferencedBy(ctx, core.ResourceModelConnection, ptr.ResourceID)
	if err != nil {
		return web.ModelConnectionView{}, err
	}
	digest, err := loadCredentialDigest(ctx, v.credentials, core.ResourceModelConnection, ptr.ResourceUID)
	if err != nil {
		return web.ModelConnectionView{}, err
	}
	owner, err := v.ownerAgentName(ctx, ptr)
	if err != nil {
		return web.ModelConnectionView{}, err
	}
	view := web.ModelConnectionView{
		Name:               ptr.ResourceID,
		ResourceUID:        string(ptr.ResourceUID),
		BaseURL:            definition.BaseURL,
		APIKeyEnv:          envReferenceName(definition.APIKey),
		DefaultModel:       definition.DefaultModel,
		CredentialMode:     string(viewCredentialMode(digest)),
		Scope:              string(scopeOfPointer(ptr)),
		OwnerAgent:         owner,
		Revision:           ptr.Revision,
		PublishedVersionID: version.ID,
		PublishedVersionNo: version.VersionNo,
		AppliedVersionID:   derefVersionID(ptr.AppliedVersionID),
		ETag:               resourceETag(ptr, version, digest),
		ReferencedBy:       nonNilStrings(referenced),
		Availability:       v.modelConnectionAvailability(definition, digest),
	}
	if rec, ok := tests[ptr.ResourceUID]; ok {
		view.LastTest = web.ConnectionTestViewOf(&rec)
	}
	if digest != nil {
		view.CredentialRef = digest.ID
		view.CredentialState = string(digest.IdentityState)
		view.CredentialVersion = digest.Version
		view.Last4 = digest.Last4
	}
	if ptr.AppliedVersionID != nil {
		applied, err := v.versionOf(ctx, core.ResourceModelConnection, ptr.ResourceID, *ptr.AppliedVersionID)
		if err != nil {
			return web.ModelConnectionView{}, err
		}
		view.AppliedVersionNo = applied.VersionNo
	}
	return view, nil
}

// ownerAgentName 取专属资源的属主 Agent **名**（共享资源为空串）。名字查不到
// **必须**上报：`owner_agent` 为空会被页面读成"共享"，那是把"证明不了"写成
// "没有属主"。活动 Agent 取生命周期索引、归档 Agent 取归档行，两者都读不到才是错误。
// ownerAgentName resolves the owner name of an exclusive resource and refuses to
// report an empty owner when the name cannot be proven.
func (v *resourceViews) ownerAgentName(ctx context.Context, ptr core.ResourcePointer) (string, error) {
	return resolveOwnerAgentName(ctx, v.versions, ptr.Scope, ptr.OwnerAgentUID,
		fmt.Sprintf("资源 %s/%s", ptr.Kind, ptr.ResourceID))
}

// resolveOwnerAgentName 是属主名反查的唯一实现（资源指针与通知渠道共用）：共享范围
// 返回空串，专属范围必须解析出名字。`what` 只用于错误文案，指出是哪一行。
// resolveOwnerAgentName is the one owner-name lookup shared by resource pointers and
// notification channels; an exclusive owner that cannot be named is an error.
func resolveOwnerAgentName(ctx context.Context, versions *storage.ResourceStore,
	scope core.ResourceScope, uid core.AgentUID, what string,
) (string, error) {
	if scope != core.ScopeExclusive {
		return "", nil
	}
	if _, err := core.ParseAgentUID(string(uid)); err != nil {
		return "", fmt.Errorf("%s 的专属属主身份非法: %w", what, err)
	}
	if versions == nil {
		return "", fmt.Errorf("%s 的专属属主 %s 无法反查：缺少生命周期索引", what, uid)
	}
	name, found, err := versions.AgentNameByUID(ctx, uid)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: %s 的专属属主 %s", core.ErrResourceNotFound, what, uid)
	}
	return name, nil
}

// livePointer 读一个未删除的发布指针；缺失与已删除同样按"资源不存在"报告。// livePointer treats a deleted pointer as missing.
func (v *resourceViews) livePointer(ctx context.Context, kind core.ResourceKind, id string) (core.ResourcePointer, error) {
	ptr, found, err := v.optionalPointer(ctx, kind, id)
	if err != nil {
		return core.ResourcePointer{}, err
	}
	if !found {
		return core.ResourcePointer{}, fmt.Errorf("%w: %s/%s", core.ErrResourceNotFound, kind, id)
	}
	return ptr, nil
}

// optionalPointer 区分"没有指针"与"读失败"：前者是合法状态（配置了但尚未入账），
// 后者必须上报——把读失败说成"没有"会静默显示零版本。
// optionalPointer distinguishes an absent pointer from a read failure.
func (v *resourceViews) optionalPointer(ctx context.Context, kind core.ResourceKind, id string) (core.ResourcePointer, bool, error) {
	if id == "" {
		return core.ResourcePointer{}, false, nil
	}
	ptr, err := v.versions.Pointer(ctx, kind, id)
	if errors.Is(err, core.ErrResourceNotFound) {
		return core.ResourcePointer{}, false, nil
	}
	if err != nil {
		return core.ResourcePointer{}, false, err
	}
	if ptr.DeletedAt != nil {
		return core.ResourcePointer{}, false, nil
	}
	return ptr, true, nil
}

// versionOf 读版本行并核对归属（与准入路径同一口径）。
// versionOf loads the version row and checks its owner.
func (v *resourceViews) versionOf(ctx context.Context, kind core.ResourceKind, id string, versionID int64) (core.ResourceVersion, error) {
	version, err := v.versions.VersionByID(ctx, versionID)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	if version.Kind != kind || version.ResourceID != id {
		return core.ResourceVersion{}, fmt.Errorf("资源 %s/%s 的版本 %d 属于 %s/%s",
			kind, id, version.ID, version.Kind, version.ResourceID)
	}
	return version, nil
}

// configuredMCPNames / connectedMCPNames 取运行期两份名单的副本；未接运行时为空。
// The two name lists are copied from the runtime; a nil runtime yields nothing.
func (v *resourceViews) configuredMCPNames() []string {
	if v.mcp == nil {
		return nil
	}
	return v.mcp.ConfiguredNames()
}

// connectedMCPNames 取已连接名单（文件序）。
// connectedMCPNames lists the connected servers in file order.
func (v *resourceViews) connectedMCPNames() []string {
	if v.mcp == nil {
		return nil
	}
	return v.mcp.ConnectedNames()
}

// mcpTools 按连接顺序收集该 server 的工具名；非 nil，空集序列化为 `[]`。
// mcpTools collects the server's tool names in connection order.
func (v *resourceViews) mcpTools(name string) []string {
	if v.mcp == nil {
		return []string{}
	}
	return v.mcp.Tools(name)
}

// resourceProviderDefinition 是 provider 发布版本定义原文的只读投影（字段与
// basalt.yaml 的 providers[] 段同形，见 `internal/cli.ProviderConfig`）。
// resourceProviderDefinition mirrors the provider definition block.
type resourceProviderDefinition struct {
	Name         string `yaml:"name"`
	BaseURL      string `yaml:"base_url"`
	APIKey       string `yaml:"api_key"`
	DefaultModel string `yaml:"default_model"`
	// CredentialMode 是 v3 的凭据两模式（env / managed）；未声明 = env（兼容 v2 定义）。
	// CredentialMode is the v3 two-mode credential switch; absent means env.
	CredentialMode string `yaml:"credential_mode"`
}

// resourceMCPDefinition 是 mcp_servers.yaml servers[] 段的只读投影。
// resourceMCPDefinition mirrors the MCP server definition block.
type resourceMCPDefinition struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"`
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	URL       string            `yaml:"url"`
	Env       map[string]string `yaml:"env"`
	Headers   map[string]string `yaml:"headers"`
}

// parseProviderDefinition 解析模型连接的发布定义原文；空原文或解析失败都报错
// （发布行损坏，**不得**当作空连接继续）。两种已落库形态都接受：基线写入的条目
// 映射（序列指示符已剥离）与管理写路径写入的块式列表片段（顶层是单条目列表，
// 约束 4 要求 `definition_yaml` 存块原文）。形态差异只影响读取输入，**不**改写
// 存储字节或哈希。
// parseProviderDefinition accepts both stored shapes: the baseline's dash-stripped
// entry mapping and the management path's block-list excerpt.
func parseProviderDefinition(id, raw string) (resourceProviderDefinition, error) {
	var definition resourceProviderDefinition
	if strings.TrimSpace(raw) == "" {
		return definition, fmt.Errorf("模型连接 %s 的发布版本缺少定义原文", id)
	}
	if err := yaml.Unmarshal([]byte(raw), &definition); err != nil {
		var entries []resourceProviderDefinition
		if seqErr := yaml.Unmarshal([]byte(raw), &entries); seqErr != nil || len(entries) != 1 {
			return resourceProviderDefinition{}, fmt.Errorf("模型连接 %s 的定义原文无法解析: %w", id, err)
		}
		definition = entries[0]
	}
	return definition, nil
}

// parseMCPDefinition 解析 MCP server 的发布定义原文；口径同上（含两种已落库形态）。
// parseMCPDefinition refuses an empty or unparsable definition; both stored shapes
// are accepted exactly like the provider projection.
func parseMCPDefinition(id, raw string) (resourceMCPDefinition, error) {
	var definition resourceMCPDefinition
	if strings.TrimSpace(raw) == "" {
		return definition, fmt.Errorf("mcp_server/%s 的发布版本缺少定义原文", id)
	}
	if err := yaml.Unmarshal([]byte(raw), &definition); err != nil {
		var entries []resourceMCPDefinition
		if seqErr := yaml.Unmarshal([]byte(raw), &entries); seqErr != nil || len(entries) != 1 {
			return resourceMCPDefinition{}, fmt.Errorf("mcp_server/%s 的定义原文无法解析: %w", id, err)
		}
		definition = entries[0]
	}
	return definition, nil
}

// mcpCredentialKeyViews 把凭据状态投影成契约视图：顺序沿用 `MCPCredentialSlots` 的
// 字典序，`last4` 之外不回任何凭据片段。nil 结果统一成空切片（JSON `[]`，不是 null）。
// mcpCredentialKeyViews maps the statuses onto the contract view, never returning null.
func mcpCredentialKeyViews(statuses []MCPCredentialStatus) []web.McpCredentialKeyView {
	views := make([]web.McpCredentialKeyView, 0, len(statuses))
	for _, status := range statuses {
		views = append(views, web.McpCredentialKeyView{
			Key:   status.Key,
			Mode:  string(status.Mode),
			State: string(status.State),
			Last4: status.Last4,
		})
	}
	return views
}

// mcpTransport 归一化传输方式：缺省按 v1 的唯一取值 stdio；其它取值原样投影，
// 由 schema 的枚举判卷（解析门禁保证不会有第三个值）。
// mcpTransport defaults an omitted transport to stdio.
func mcpTransport(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return mcpTransportStdio
	}
	return raw
}

// mcpExecutable 只取命令行的可执行名（argv[0]）：参数可能含路径或凭证，**不得**出站
// （约束 35 的"只回可执行名"）。
// mcpExecutable returns argv[0] only; arguments never leave the process.
func mcpExecutable(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// sortedEnvKeys 只回 env 的键名，按升序（值可能是凭证，**不得**出站）。
// sortedEnvKeys returns the sorted key names only.
func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// envReferenceName 只接受精确 `${VAR}` 形态并返回变量名；其余形态回空串
// （`core.EnvPlaceholderName` 是唯一判定实现，不另写一份）。
// envReferenceName returns the referenced variable name or "" for a non-placeholder.
func envReferenceName(raw string) string {
	name, ok := core.EnvPlaceholderName(raw)
	if !ok {
		return ""
	}
	return name
}

// ETag 组成（FR-081）：
//   - 哈希输入是「`键=值`」按固定顺序、NUL 分隔的规整串；值本身不含 NUL（哈希是
//     十六进制、UID / 枚举由上游判卷、序号是十进制），因此拼接无歧义；
//   - 内容哈希之外还带资源 UID、修订号、范围、属主与凭据状态——只换 Key 时定义字节
//     不变，纯内容哈希发现不了并发替换（两个管理员同时换 Key，后写会覆盖先写）；
//   - 技能与 MCP 没有凭据身份，凭据段整体省略；换 Key **不得**动 Agent 正文的 ETag
//     （Agent ETag 仍是原始字节哈希，含 `resource_bindings`）。
//
// The conditional ETag covers content plus identity, revision, scope and credential
// state; extra segments are omitted for kinds that have no credential identity.

// resourceETagVersion 是 ETag 规整串的版本前缀：字段集变化必须换版本，避免新旧
// 计算方式产出看起来可比、实际不可比的 ETag。
// resourceETagVersion prefixes the canonical string; a field-set change bumps it.
const resourceETagVersion = "basalt/resource-etag/v1"

// credentialDigest 是条件 ETag 的「凭据状态」段：凭据身份 ID、当前版本号、该版本
// 状态、撤销纪要与目标绑定哈希。env 模式没有凭据身份 → nil（整段省略）。
// credentialDigest is the credential-state segment of the conditional ETag.
type credentialDigest struct {
	ID            string
	Version       int
	State         core.CredentialVersionState
	Epoch         int
	TargetBinding string
	// IdentityState / Last4 只供**视图展示**（`credential_state`、`last4`），
	// **不参与** `resourceETag` 的哈希——ETag 的字段集与顺序是契约的一部分，
	// 两个只读展示字段不得混进去。前者是凭据**身份**状态，后者是当前活动版本的尾号。
	// IdentityState and Last4 are display-only and must never enter the ETag hash.
	IdentityState core.CredentialIdentityState
	Last4         string
}

// viewCredentialMode 由「有没有凭据身份」判定资源模式（契约 ModelConnectionView /
// NotifyChannelView 的 `credential_mode`）：没有身份 = env。清单侧同一规则见
// `ProviderVersionRef.CredentialMode`，模式不另存第二列。
// viewCredentialMode derives the mode from the presence of a credential identity.
func viewCredentialMode(digest *credentialDigest) core.CredentialMode {
	if digest == nil {
		return core.CredentialModeEnv
	}
	return core.CredentialModeManaged
}

// scopeOfPointer 归一化使用范围：空列读成 shared（列默认值与迁移回填口径）。
// scopeOfPointer reads an empty scope as shared, the documented column default.
func scopeOfPointer(ptr core.ResourcePointer) core.ResourceScope {
	if ptr.Scope == core.ScopeExclusive {
		return core.ScopeExclusive
	}
	return core.ScopeShared
}

// credentialBindingKind 把资源类别映射到凭据属主类别：只有模型连接与通知渠道有
// 托管凭据身份（`credentials.owner_kind` 的取值集）。
// credentialBindingKind maps a resource kind to its credential owner kind.
func credentialBindingKind(kind core.ResourceKind) (core.BindingKind, bool) {
	switch kind {
	case core.ResourceModelConnection:
		return core.BindingModelConnection, true
	case core.ResourceNotifyChannel:
		return core.BindingNotifyChannel, true
	default:
		return "", false
	}
}

// loadCredentialDigest 读资源当前凭据身份与活动版本。没有身份（env 模式 / 技能 /
// MCP / 未装配凭据仓储）返回 nil；读失败**必须**上报——把"读不到"当成"没有凭据"
// 会让 ETag 静默退化成内容哈希，条件写就再次发现不了换 Key。
// loadCredentialDigest returns nil when the resource has no credential identity and
// reports read failures instead of degrading to a pure content hash.
func loadCredentialDigest(ctx context.Context, creds *storage.CredentialStore,
	kind core.ResourceKind, uid core.ResourceUID,
) (*credentialDigest, error) {
	owner, ok := credentialBindingKind(kind)
	if !ok || creds == nil {
		return nil, nil
	}
	if _, err := core.ParseResourceUID(string(uid)); err != nil {
		// 没有资源身份的旧行没有凭据身份可查（v2 基线的遗留指针），ETag 照常可算。
		return nil, nil
	}
	identity, err := creds.CredentialByOwner(ctx, owner, uid)
	if errors.Is(err, core.ErrResourceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取资源 %s 的凭据身份: %w", uid, err)
	}
	// 已退役的身份是「模式已切回 env」的持久标记（R4.8：managed → env 把身份置
	// retired，版本仍可解密以便在途使用）。资源此时不再有凭据段：视图按 env 报，
	// ETag 也不含凭据字段；把 retired 当成 managed 会让切换后的资源永远显示托管。
	// A retired identity marks a switch back to env; the resource has no credential
	// segment any more, so the view and the ETag must not claim managed.
	if identity.State == core.CredentialIdentityRetired {
		return nil, nil
	}
	version, err := creds.Version(ctx, identity.ID, identity.CurrentVersion)
	if errors.Is(err, core.ErrResourceNotFound) {
		return nil, fmt.Errorf("凭据 %s 的活动版本 %d 缺失", identity.ID, identity.CurrentVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("读取凭据 %s 的版本 %d: %w", identity.ID, identity.CurrentVersion, err)
	}
	return &credentialDigest{
		ID:            identity.ID,
		Version:       identity.CurrentVersion,
		State:         version.State,
		Epoch:         identity.RevocationEpoch,
		TargetBinding: version.TargetBinding,
		IdentityState: identity.State,
		Last4:         version.Last4,
	}, nil
}

// publishedResourceETag 用一份已读的指针与版本行计算条件 ETag（按资源类别补凭据段）。
// publishedResourceETag composes the conditional ETag from an already-read pointer
// and version row.
func publishedResourceETag(ctx context.Context, creds *storage.CredentialStore,
	ptr core.ResourcePointer, version core.ResourceVersion,
) (string, error) {
	digest, err := loadCredentialDigest(ctx, creds, ptr.Kind, ptr.ResourceUID)
	if err != nil {
		return "", err
	}
	return resourceETag(ptr, version, digest), nil
}

// resourceETag 按固定字段顺序计算条件 ETag：字段集与顺序是契约的一部分（FR-081），
// 变动即换 `resourceETagVersion`。输出仍是 `"sha256:<64 位小写十六进制>"`——与内容
// ETag 同一语法，但**不是** `content_hash` 本身，读接口必须用本函数而不是直接回显。
// resourceETag renders the canonical conditional ETag; the field set and order are
// part of the contract.
func resourceETag(ptr core.ResourcePointer, version core.ResourceVersion,
	digest *credentialDigest,
) string {
	var b etagBuilder
	b.write(resourceETagVersion)
	scope := ptr.Scope
	if scope == "" {
		// 列默认值是 shared；空值只可能来自未走仓储的构造，按默认值归一化，
		// 避免同一个资源出现两种"共享"写法。
		scope = core.ScopeShared
	}
	b.field("content_hash", version.ContentHash)
	b.field("resource_uid", string(ptr.ResourceUID))
	b.field("revision", strconv.Itoa(ptr.Revision))
	b.field("scope", string(scope))
	b.field("owner_agent_uid", string(ptr.OwnerAgentUID))
	b.credential(digest)
	return b.sum()
}

// etagBuilder 是条件 ETag 规整串的唯一拼装器：`键=值` 按调用顺序、NUL 分隔。
// 值本身不含 NUL（哈希是十六进制、UID / 枚举由上游判卷、序号是十进制），拼接无歧义。
// etagBuilder is the single canonical-string composer shared by every conditional ETag.
type etagBuilder struct{ b strings.Builder }

func (e *etagBuilder) write(s string) { e.b.WriteString(s) }

func (e *etagBuilder) field(key, value string) {
	e.b.WriteByte(0)
	e.b.WriteString(key)
	e.b.WriteByte('=')
	e.b.WriteString(value)
}

// credential 追加凭据段；没有凭据身份（env 模式）整段省略。
// credential appends the credential segment; an env-mode resource omits it entirely.
func (e *etagBuilder) credential(digest *credentialDigest) {
	if digest == nil {
		return
	}
	e.field("credential_id", digest.ID)
	e.field("credential_version", strconv.Itoa(digest.Version))
	e.field("credential_version_state", string(digest.State))
	e.field("revocation_epoch", strconv.Itoa(digest.Epoch))
	e.field("target_binding", digest.TargetBinding)
}

// sum 输出强 ETag 形态 `"sha256:<64 位小写十六进制>"`。
func (e *etagBuilder) sum() string {
	sum := sha256.Sum256([]byte(e.b.String()))
	return `"sha256:` + hex.EncodeToString(sum[:]) + `"`
}

// newResourceUID 为一次资源创建分配稳定身份。随机源失败直接失败——不生成可预测的
// 回退值；同名重建同样走这里，旧 UID 永不复活。
// newResourceUID allocates the identity of a newly created resource; a same-name
// rebuild allocates a fresh uid and never revives the tombstoned one.
func newResourceUID(kind core.ResourceKind, name string) (core.ResourceUID, error) {
	uid, err := core.NewResourceUID()
	if err != nil {
		return "", fmt.Errorf("为 %s %s 分配资源身份: %w", kind, name, err)
	}
	return uid, nil
}

// resourceUIDOfPointer 取名称当前指向的实例身份（更新 / 发布 / 删除一律用它，**不得**
// 按名称另猜一个）。指针没有 UID 只可能来自未回填的旧库——fail-closed，不猜身份。
// resourceUIDOfPointer returns the uid the name currently points at; a pointer without
// a uid is a library-level anomaly and fails closed.
func resourceUIDOfPointer(kind core.ResourceKind, name string, ptr core.ResourcePointer) (core.ResourceUID, error) {
	if _, err := core.ParseResourceUID(string(ptr.ResourceUID)); err != nil {
		return "", fmt.Errorf("%w: %s/%s 的发布指针缺少资源 UID", core.ErrResourceUIDMismatch, kind, name)
	}
	return ptr.ResourceUID, nil
}

// expectResourceUID 校验身份动作显式携带的 `resource_uid`：缺失 / 非法 / 与当前指针
// 不一致都返回 409 分类的 `core.ErrResourceUIDMismatch`，并在 data 里给出当前 UID
// （契约 §9.5「涉及资源身份的动作」）。绝不按名称猜测目标。
// expectResourceUID enforces the explicit resource_uid carried by identity actions and
// reports the current uid in the error data; the target is never guessed by name.
func expectResourceUID(kind core.ResourceKind, name, raw string, ptr core.ResourcePointer) (core.ResourceUID, error) {
	current, err := resourceUIDOfPointer(kind, name, ptr)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(raw) == "" {
		return "", &core.AdminError{
			Kind:    core.ErrResourceUIDMismatch,
			Message: fmt.Sprintf("%s %s 的资源身份缺失：请求必须携带与当前指针一致的 resource_uid", kind, name),
			Data:    map[string]any{"resource_uid": string(current)},
		}
	}
	uid, err := core.ParseResourceUID(raw)
	if err != nil {
		return "", &core.AdminError{
			Kind:    core.ErrResourceUIDMismatch,
			Message: fmt.Sprintf("%s %s 的 resource_uid 不是合法的资源身份", kind, name),
			Data:    map[string]any{"resource_uid": string(current)},
		}
	}
	if uid != current {
		return "", &core.AdminError{
			Kind: core.ErrResourceUIDMismatch,
			Message: fmt.Sprintf("%s %s 当前指向 %s，请求给出的 resource_uid 不是当前身份",
				kind, name, current),
			Data: map[string]any{"resource_uid": string(current)},
		}
	}
	return current, nil
}

// mcpRestartPending 与 web 的投影同一口径（约束 29 / D-09）：已发布且与实际生效
// 版本不同 → 需重启；无发布指针（0）时不报"需重启"（页面另行标问题）。
// mcpRestartPending mirrors the web projection of the restart hint.
func mcpRestartPending(ptr core.ResourcePointer) bool {
	if ptr.PublishedVersionID == 0 {
		return false
	}
	return ptr.AppliedVersionID == nil || *ptr.AppliedVersionID != ptr.PublishedVersionID
}

// derefVersionID 把可空版本 id 投影为 0（契约字段是非空整数）。
// derefVersionID renders a nil version id as zero.
func derefVersionID(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

// nonNilStrings 保持列表非 nil（JSON 的 null 与"空列表"必须可区分）。
// nonNilStrings keeps the list serializing as [] rather than null.
func nonNilStrings(values []string) []string {
	return append(make([]string, 0, len(values)), values...)
}
