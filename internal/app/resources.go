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
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// runResources 是 `core.RunResources` 的唯一实现（§3.1.32 的固定形状）：字段逐字
// 保留——`Skills` 与 `Gate` 在本 US 由构造方注入，技能目录解析只认 `storage_path`。
// runResources is the single implementation of core.RunResources; the frozen field set
// is kept verbatim even where a field is consumed by the next US.
type runResources struct {
	Versions  *storage.ResourceStore
	Providers *providerCatalog // 本 US 在 providers.go 引入（显式 name→client 映射）
	// LLM 是装配期唯一的审计路由服务（人工 review ② 缺陷 2）：运行时里的 Provider
	// 客户端取它的 `Pinned` 固定视图，**不得**直接发放目录里的裸协议客户端——那样
	// 两阶段审计被绕过，且运行内路由不再受准入固定表约束。
	// LLM is the audited routing service; the runtime hands out its pinned views so a
	// run can neither bypass the audit nor fall back to another provider.
	LLM    *provider.ProviderService
	MCP    *mcpRuntime // 本 US 在 mcp.go 引入（发布定义 → 运行时句柄）
	Skills *workspace.SkillLibrary
	Gate   *core.ResourceGate // 与 US-12 资源发布共用的同一实例
}

// NewRunResources 是准入解析端口的唯一构造点（图 A ⑥）。
// NewRunResources is the only construction point of the admission port.
func NewRunResources(versions *storage.ResourceStore, providers *providerCatalog, llm *provider.ProviderService, mcp *mcpRuntime, skills *workspace.SkillLibrary, gate *core.ResourceGate) core.RunResources {
	return &runResources{Versions: versions, Providers: providers, LLM: llm, MCP: mcp, Skills: skills, Gate: gate}
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
	handedOff = true
	return resolved, nil
}

// providerClient 取该 Provider 的**审计固定视图**（人工 review ② 缺陷 2）：目录里的
// 裸协议客户端只用于构造路由服务，运行时**不得**直接发放它——那样两阶段审计被绕过，
// 且运行内路由不再被准入固定表约束。未接审计服务（nil）一律失败关闭，不退回裸客户端。
// providerClient hands out the audited pinned view; a missing audit service fails closed
// instead of falling back to a raw protocol client.
func (r *runResources) providerClient(name string) (core.ProviderClient, bool) {
	if r.LLM == nil {
		return nil, false
	}
	return r.LLM.Pinned(name)
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

// publishedVersion 读发布指针指向的版本并核对归属（kind + resource_id 必须逐字相符）。
// 指针指向别人的版本属于数据事故，**不得**拿它继续准入。
// publishedVersion verifies that the pointer's version belongs to the same resource.
func (r *runResources) publishedVersion(ctx context.Context, kind core.ResourceKind, id string) (core.ResourceVersion, error) {
	ptr, err := r.visiblePointer(ctx, kind, id)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	return r.versionOf(ctx, kind, id, ptr.PublishedVersionID)
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

// versionRef 把版本行投影成清单引用（名字取资源名，版本号逐资源从 1 起）。
// versionRef projects a version row onto the manifest reference.
func versionRef(name string, version core.ResourceVersion) core.ResourceVersionRef {
	return core.ResourceVersionRef{
		Name:        name,
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
// 与生命周期联动——本 US 的六个只读方法不读它。
// resourceViews projects pointers, version rows and the live connection view; the
// lifecycle field is kept per the frozen constructor signature for US-12.
type resourceViews struct {
	versions  *storage.ResourceStore
	lifecycle *workspace.LifecycleStore
	mcp       *mcpRuntime
}

// NewResourceViews 是图 A 中 `resourceViews` 的唯一构造点（卡片行 1632）。
// NewResourceViews is the only construction point of the console-side reader.
func NewResourceViews(versions *storage.ResourceStore, lifecycle *workspace.LifecycleStore, mcp *mcpRuntime) web.ResourceReader {
	return &resourceViews{versions: versions, lifecycle: lifecycle, mcp: mcp}
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
	return web.SkillView{
		Name:         ptr.ResourceID,
		Path:         skillWorkCopyPrefix + ptr.ResourceID,
		VersionID:    version.ID,
		VersionNo:    version.VersionNo,
		ContentHash:  version.ContentHash,
		ETag:         publishedETag(version.ContentHash),
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
		view, _, err := v.mcpServerView(ctx, name)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// McpServer 返回单个 MCP server：既无指针也不在配置名单 → `core.ErrResourceNotFound`。
// ETag 取**发布版本** content_hash 的内容 ETag（人工 review ② F3）：数据以页面展示的
// 当前定义为准，且只经响应头出站——公开视图里没有该字段；无指针（未入账的配置项）
// 时为空串。响应体因此与列表项完全同形。
// McpServer returns one server; the internal ETag is derived from the published
// version's content hash and leaves the process only as a response header.
func (v *resourceViews) McpServer(ctx context.Context, name string) (web.McpServerDetail, error) {
	_, found, err := v.optionalPointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		return web.McpServerDetail{}, err
	}
	if !found && !slices.Contains(v.configuredMCPNames(), name) {
		return web.McpServerDetail{}, fmt.Errorf("%w: mcp_server/%s", core.ErrResourceNotFound, name)
	}
	view, etag, err := v.mcpServerView(ctx, name)
	if err != nil {
		return web.McpServerDetail{}, err
	}
	return web.McpServerDetail{View: view, ETag: etag}, nil
}

// mcpServerView 投影一个 MCP server：定义字段取**发布**版本（页面展示"当前定义"，
// 未生效由 pending_restart 表达）；连接与工具名取运行期；无指针时版本字段全零，
// **不伪造**版本号。env 只出键名（约束 35）。第二个返回值是详情端点专用 ETag：
// 由发布版本 content_hash 生成；无指针时为空串。
// mcpServerView projects one server: definition fields come from the published version,
// connectivity from the runtime, and a missing pointer yields zero version fields. The
// second result is the detail-only ETag; it is empty when no version is recorded.
func (v *resourceViews) mcpServerView(ctx context.Context, name string) (web.McpServerView, string, error) {
	view := web.McpServerView{
		Name:      name,
		Transport: mcpTransportStdio,
		EnvKeys:   []string{},
		Tools:     v.mcpTools(name),
		Connected: slices.Contains(v.connectedMCPNames(), name),
	}
	ptr, found, err := v.optionalPointer(ctx, core.ResourceMCPServer, name)
	if err != nil {
		return web.McpServerView{}, "", err
	}
	if !found {
		return view, "", nil
	}
	version, err := v.versionOf(ctx, core.ResourceMCPServer, name, ptr.PublishedVersionID)
	if err != nil {
		return web.McpServerView{}, "", err
	}
	definition, err := parseMCPDefinition(name, version.DefinitionYAML)
	if err != nil {
		return web.McpServerView{}, "", err
	}
	view.Transport = mcpTransport(definition.Transport)
	view.Command = mcpExecutable(definition.Command)
	view.EnvKeys = sortedEnvKeys(definition.Env)
	view.PublishedVersionID = version.ID
	view.PublishedVersionNo = version.VersionNo
	view.AppliedVersionID = derefVersionID(ptr.AppliedVersionID)
	if ptr.AppliedVersionID != nil {
		applied, err := v.versionOf(ctx, core.ResourceMCPServer, name, *ptr.AppliedVersionID)
		if err != nil {
			return web.McpServerView{}, "", err
		}
		view.AppliedVersionNo = applied.VersionNo
	}
	view.PendingRestart = mcpRestartPending(ptr)
	return view, publishedETag(version.ContentHash), nil
}

// ModelConnections 列出未删除的模型连接指针；顺序取 `ListPointers` 升序。
// ModelConnections lists every live model connection pointer in storage order.
func (v *resourceViews) ModelConnections(ctx context.Context) ([]web.ModelConnectionView, error) {
	pointers, err := v.versions.ListPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return nil, err
	}
	views := make([]web.ModelConnectionView, 0, len(pointers))
	for _, ptr := range pointers {
		view, err := v.modelConnectionView(ctx, ptr)
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
	return v.modelConnectionView(ctx, ptr)
}

// modelConnectionView 投影一个模型连接：api_key_env 只出 `${ENV_VAR}` 引用名，
// 非占位形态（或未配置）为空串——解析后的凭证值**永不**进入视图（约束 35）。
// modelConnectionView exposes the env reference name only.
func (v *resourceViews) modelConnectionView(ctx context.Context, ptr core.ResourcePointer) (web.ModelConnectionView, error) {
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
	view := web.ModelConnectionView{
		Name:               ptr.ResourceID,
		BaseURL:            definition.BaseURL,
		APIKeyEnv:          envReferenceName(definition.APIKey),
		DefaultModel:       definition.DefaultModel,
		PublishedVersionID: version.ID,
		PublishedVersionNo: version.VersionNo,
		AppliedVersionID:   derefVersionID(ptr.AppliedVersionID),
		ETag:               publishedETag(version.ContentHash),
		ReferencedBy:       nonNilStrings(referenced),
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

// livePointer 读一个未删除的发布指针；缺失与已删除同样按"资源不存在"报告。
// livePointer treats a deleted pointer as missing.
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
	tools := []string{}
	if v.mcp == nil {
		return tools
	}
	for _, conn := range v.mcp.conns {
		if conn.Server != name {
			continue
		}
		for _, tool := range conn.Tools {
			tools = append(tools, tool.Name())
		}
	}
	return tools
}

// resourceProviderDefinition 是 provider 发布版本定义原文的只读投影（字段与
// basalt.yaml 的 providers[] 段同形，见 `internal/cli.ProviderConfig`）。
// resourceProviderDefinition mirrors the provider definition block.
type resourceProviderDefinition struct {
	Name         string `yaml:"name"`
	BaseURL      string `yaml:"base_url"`
	APIKey       string `yaml:"api_key"`
	DefaultModel string `yaml:"default_model"`
}

// resourceMCPDefinition 是 mcp_servers.yaml servers[] 段的只读投影。
// resourceMCPDefinition mirrors the MCP server definition block.
type resourceMCPDefinition struct {
	Name      string            `yaml:"name"`
	Transport string            `yaml:"transport"`
	Command   string            `yaml:"command"`
	Env       map[string]string `yaml:"env"`
}

// parseProviderDefinition 解析模型连接的发布定义原文；空原文或解析失败都报错
// （发布行损坏，**不得**当作空连接继续）。
// parseProviderDefinition refuses an empty or unparsable definition.
func parseProviderDefinition(id, raw string) (resourceProviderDefinition, error) {
	var definition resourceProviderDefinition
	if strings.TrimSpace(raw) == "" {
		return definition, fmt.Errorf("模型连接 %s 的发布版本缺少定义原文", id)
	}
	if err := yaml.Unmarshal([]byte(raw), &definition); err != nil {
		return definition, fmt.Errorf("模型连接 %s 的定义原文无法解析: %w", id, err)
	}
	return definition, nil
}

// parseMCPDefinition 解析 MCP server 的发布定义原文；口径同上。
// parseMCPDefinition refuses an empty or unparsable definition.
func parseMCPDefinition(id, raw string) (resourceMCPDefinition, error) {
	var definition resourceMCPDefinition
	if strings.TrimSpace(raw) == "" {
		return definition, fmt.Errorf("mcp_server/%s 的发布版本缺少定义原文", id)
	}
	if err := yaml.Unmarshal([]byte(raw), &definition); err != nil {
		return definition, fmt.Errorf("mcp_server/%s 的定义原文无法解析: %w", id, err)
	}
	return definition, nil
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

// publishedETag 按内容 ETag 的同一格式生成强 ETag（含双引号）。
// publishedETag renders a strong ETag in the same format as the content ETag.
func publishedETag(contentHash string) string {
	return `"sha256:` + contentHash + `"`
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
