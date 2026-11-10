package app

// 本文件实现 US-25C FR-25C-17 的「技能发布 → 联动重发直接依赖的 Agent」（模块卡
// §2.3.7）。三条硬约束决定了这里的写法：
//
//  1. 重建来源只有**已安装的发布 bundle**与本技能的**已发布版本目录**；不得读工作区
//     工作副本，也不得复用 `buildWorkspaceBundle`（它直接读工作区 AGENT.md，会把
//     未批准的改动顺带发出去）；
//  2. 「是否还要发」的唯一依据是**锁内重读**：Agent 指针走 `PointerByUID`，技能当前
//     版本走 `PointerVersion`（单条 JOIN 一次快照）。调用方入参只用来选 Agent，不用来
//     定版本；`resolvePublishedSkill` 走 `Pointer`，在脚本化快照下会多读一次，这里不用；
//  3. 发布前再读一次 `PointerVersion`：晚到的旧版本联动看到的值已变 → `superseded` 且
//     零写入。没有这一步，V2 的联动会覆盖已引用 V3 的 bundle，版本倒退且下一轮"发现"
//     也不会把它修回来。
//
// Republish rebuilds from installed/published bytes only and re-reads both the agent
// pointer and the live skill version inside the lock; a changed live version before
// publication yields `superseded` with zero writes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// bundleRootDirName 是资产根下的内容寻址目录段（`assets.bundlesDirName` 的镜像）。
	// 发布器只在这里建"载荷还原"临时目录，真正的安装路径仍由 `assets.Install` 决定。
	// bundleRootDirName mirrors assets' hash-addressed root segment for temp dirs only.
	bundleRootDirName = "bundles"
)

// skillRepublishStore 是联动重发需要的共享记录读取面：在 `bundleRecordStore` 之上增加
// 两类只读查询——全量指针列表（选依赖 Agent）与技能当前版本号（锁内复核）。
// skillRepublishStore adds the pointer listing and the single-snapshot live version read
// to the publication store seam.
type skillRepublishStore interface {
	bundleRecordStore
	ListPointers(ctx context.Context, kind core.ResourceKind) ([]core.ResourcePointer, error)
	PointerVersion(ctx context.Context, kind core.ResourceKind, resourceID string) (int64, error)
}

// skillRepublishDeps 是联动重发器的依赖。`AgentGate` 必须与内容编辑共用同一个实例
// （模块卡 §2.3.7 锁序）；测试可传新实例，但生产**不得**在联动路径里另造一把。
// skillRepublishDeps carries the republisher's dependencies; AgentGate must be the same
// instance content editing uses.
type skillRepublishDeps struct {
	AssetRoot     string
	WorkspaceRoot string
	Versions      skillRepublishStore
	Operations    core.OperationStore
	Payloads      bundlePayloadReader
	PayloadStore  core.BundlePayloadStore
	AgentGate     *core.AgentGate
	MaxBytes      int64
	Logger        *slog.Logger
	Now           func() time.Time
}

// skillRepublisher 是联动重发的实现；构造后字段不再变化。
// skillRepublisher is the linkage republisher; its fields are fixed at construction.
type skillRepublisher struct {
	deps   skillRepublishDeps
	logger *slog.Logger
	now    func() time.Time
}

// newSkillRepublisher 装配一个联动重发器；缺省 logger / now 用安全默认值，依赖是否
// 齐全由每次调用的 `wired` 判定——半截装配必须逐次报失败，而不是 panic。
// newSkillRepublisher assembles the republisher; a half-wired assembly reports failures
// per call instead of panicking.
func newSkillRepublisher(deps skillRepublishDeps) *skillRepublisher {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &skillRepublisher{deps: deps, logger: logger, now: now}
}

// wired 判断联动重发所需的依赖是否齐全：发布链、技能版本读、Agent 锁与两条路径。
// 缺任何一项都按"未接线"失败关闭——不猜路径、不造第二把锁。
// wired reports whether every dependency the republish path needs is present.
func (r *skillRepublisher) wired() bool {
	return r != nil && r.deps.Versions != nil && r.deps.Operations != nil &&
		r.deps.PayloadStore != nil && r.deps.AgentGate != nil &&
		r.deps.AssetRoot != "" && r.deps.WorkspaceRoot != ""
}

// SkillRepublisher 是联动重发的消费方接口：发布请求路径与运行期修复器只依赖它，
// 不依赖具体实现（`*ResourceAdmin` / `*skillRepublisher` 都满足）。
// SkillRepublisher is the consumer-side seam shared by the publish path and the repairer.
type SkillRepublisher interface {
	RepublishOne(ctx context.Context, agent, skillName string, actor core.Actor) core.BundlePublishResult
}

// republishDependentBundles 依次重发直接依赖该技能的 Agent：候选来自共享发布记录里
// 的已发布 bundle 清单（不看工作区），按 Agent 名排序、串行执行；每项自己持一把
// Agent 锁。结果与调用顺序一致，调用方原样返回给发布请求。
// republishDependentBundles republishes every published bundle that directly references
// the skill, in ascending agent-name order and one agent lock at a time.
func (r *skillRepublisher) republishDependentBundles(ctx context.Context, skillName string,
	actor core.Actor,
) ([]core.BundlePublishResult, error) {
	if !r.wired() {
		return nil, errors.New("技能联动重发未接线：缺版本仓储 / 操作仓储 / 载荷仓储 / Agent 锁 / 资产根 / 工作区根")
	}
	agents, err := r.dependentAgents(ctx, skillName)
	if err != nil {
		return nil, err
	}
	results := make([]core.BundlePublishResult, 0, len(agents))
	for _, agent := range agents {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		results = append(results, r.RepublishOne(ctx, agent, skillName, actor))
	}
	return results, nil
}

// dependentAgents 从共享记录里挑出「已发布的 bundle 清单引用了该技能」的 Agent：
// 指针行按 `DeletedAt` / `PublishedVersionID` 过滤，清单解析失败只点名跳过（不猜）。
// 排序在返回前完成——映射遍历顺序随机会让逐 Agent 结果不可复现。
// dependentAgents selects the agents whose published manifest references the skill and
// sorts them so per-agent results are reproducible.
func (r *skillRepublisher) dependentAgents(ctx context.Context, skillName string) ([]string, error) {
	pointers, err := r.deps.Versions.ListPointers(ctx, core.ResourceAgentBundle)
	if err != nil {
		return nil, fmt.Errorf("列出 Agent 资产发布指针: %w", err)
	}
	seen := make(map[string]struct{}, len(pointers))
	agents := make([]string, 0, len(pointers))
	for _, pointer := range pointers {
		if pointer.DeletedAt != nil || pointer.PublishedVersionID <= 0 {
			continue
		}
		version, err := r.deps.Versions.VersionByID(ctx, pointer.PublishedVersionID)
		if errors.Is(err, core.ErrResourceNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取 Agent 资产版本 %d: %w", pointer.PublishedVersionID, err)
		}
		var manifest assets.Manifest
		if err := json.Unmarshal([]byte(version.ManifestJSON), &manifest); err != nil {
			r.logger.Warn("技能联动：已发布 Agent 资产的清单无法解析，跳过",
				"version_id", version.ID, "error", err)
			continue
		}
		if manifest.Agent == "" {
			continue
		}
		if _, ok := seen[manifest.Agent]; ok {
			continue
		}
		if !manifestReferencesSkill(manifest, skillName) {
			continue
		}
		seen[manifest.Agent] = struct{}{}
		agents = append(agents, manifest.Agent)
	}
	slices.Sort(agents)
	return agents, nil
}

// skillRefIsLive 是"这条技能引用是否已经是当前发布版本"的**唯一**判据：联动重发的
// `skipped` 与运行期修复器的扫描都以它为准，两处各写一套迟早给出两种结论。
// skillRefIsLive is the single staleness criterion shared by the republisher's skipped
// shortcut and the runtime repairer's scan.
func skillRefIsLive(ref core.SkillVersionRef, liveID int64) bool {
	return ref.VersionID == liveID
}

// manifestReferencesSkill 判断清单是否直接引用了该技能。
func manifestReferencesSkill(manifest assets.Manifest, skillName string) bool {
	return slices.ContainsFunc(manifest.Skills, func(skill core.SkillVersionRef) bool {
		return skill.Name == skillName
	})
}

// RepublishOne 重发一个 Agent 的 bundle（模块卡 §2.3.7 的 `republishOne`）：
//
//	Agent 写锁 → 锁内重读指针 → 锁内读技能当前版本 → 已引用则 skipped →
//	解析 / 还原重建源 → 逐项核对配置清单 → 重建并安装 → 发布前复查 → 发布
//
// 状态不进 error：所有可预期结局（尤其在 `skipped` / `superseded` / `missing`）都从
// 返回值表达；error 只保留给上下文取消这类"没跑完"。错误文本是固定中文原因，不含
// 底层原文与路径（契约 openapi.yaml 的 error 字段口径）。
// RepublishOne runs the full single-agent republish under the agent write lock and
// reports every expected outcome through Status rather than error.
func (r *skillRepublisher) RepublishOne(ctx context.Context, agent, skillName string,
	actor core.Actor,
) core.BundlePublishResult {
	if !r.wired() {
		return bundleFailed(agent, "技能联动未接线")
	}
	if agent == "" || skillName == "" {
		return bundleMissing(agent, "Agent 或技能名为空")
	}
	unlock := r.deps.AgentGate.Lock(agent)
	defer unlock()
	return r.republishLocked(ctx, agent, skillName, actor)
}

// republishLocked 是持有 Agent 写锁的重发主体。
// republishLocked is the republish body holding the agent write lock.
func (r *skillRepublisher) republishLocked(ctx context.Context, agent, skillName string,
	actor core.Actor,
) core.BundlePublishResult {
	uid, _, ok, err := r.deps.Versions.AgentIdentity(ctx, agent)
	if err != nil {
		r.logger.Error("技能联动：读取 Agent 身份失败", "agent", agent, "error", err)
		return bundleFailed(agent, "读取 Agent 身份失败")
	}
	if !ok {
		return bundleMissing(agent, "Agent 不在生命周期索引")
	}
	pointer, err := r.deps.Versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(uid))
	if errors.Is(err, core.ErrResourceNotFound) {
		return bundleMissing(agent, "Agent 尚未发布资产")
	}
	if err != nil {
		r.logger.Error("技能联动：读取 Agent 发布指针失败", "agent", agent, "error", err)
		return bundleFailed(agent, "读取 Agent 发布指针失败")
	}
	if pointer.DeletedAt != nil {
		return bundleMissing(agent, "Agent 资产指针已删除")
	}
	version, err := r.deps.Versions.VersionByID(ctx, pointer.PublishedVersionID)
	if errors.Is(err, core.ErrResourceNotFound) {
		return bundleMissing(agent, "Agent 发布版本不存在")
	}
	if err != nil {
		r.logger.Error("技能联动：读取 Agent 发布版本失败",
			"agent", agent, "version_id", pointer.PublishedVersionID, "error", err)
		return bundleFailed(agent, "读取 Agent 发布版本失败")
	}
	var oldManifest assets.Manifest
	if err := json.Unmarshal([]byte(version.ManifestJSON), &oldManifest); err != nil {
		r.logger.Error("技能联动：已发布清单无法解析", "agent", agent, "version_id", version.ID, "error", err)
		return bundleFailed(agent, "已发布清单损坏")
	}
	if oldManifest.Agent != agent || oldManifest.AgentUID != core.AgentUID(uid) {
		return bundleFailed(agent, "已发布清单身份不符")
	}
	if oldManifest.BundleHash != version.ContentHash {
		return bundleFailed(agent, "已发布清单与版本记录不一致")
	}
	skillIndex := slices.IndexFunc(oldManifest.Skills, func(skill core.SkillVersionRef) bool {
		return skill.Name == skillName
	})
	if skillIndex < 0 {
		return bundleMissing(agent, "Agent 未引用该技能")
	}

	// 锁内读技能**当前**已发布版本：这是幂等判据与重建引用，不是调用方入参。
	liveID, err := r.deps.Versions.PointerVersion(ctx, core.ResourceSkill, skillName)
	if errors.Is(err, core.ErrResourceNotFound) {
		return bundleMissing(agent, "技能没有已发布版本")
	}
	if err != nil {
		r.logger.Error("技能联动：读取技能当前发布版本失败", "skill", skillName, "error", err)
		return bundleFailed(agent, "读取技能当前发布版本失败")
	}
	if skillRefIsLive(oldManifest.Skills[skillIndex], liveID) {
		return core.BundlePublishResult{Agent: agent, Status: core.BundlePublishSkipped}
	}

	oldDir, cleanup, failure := r.resolveRebuildSource(ctx, agent, version)
	if failure != nil {
		return *failure
	}
	defer cleanup()

	verified, err := assets.Verify(ctx, oldDir)
	if err != nil {
		r.logger.Error("技能联动：已安装资产不可复算", "agent", agent, "dir", oldDir, "error", err)
		return bundleFailed(agent, "已安装资产不可复算")
	}
	if verified.BundleHash != version.ContentHash {
		return bundleFailed(agent, "已安装资产与发布记录不一致")
	}

	ref, newRoot, err := r.liveSkillRef(ctx, skillName, liveID)
	if err != nil {
		r.logger.Error("技能联动：技能发布版本不可用", "skill", skillName, "version_id", liveID, "error", err)
		return bundleMissing(agent, "技能发布版本不可用")
	}
	rebuilt, err := rebuildBundleWithSkill(ctx, oldDir, agent, ref, newRoot)
	if err != nil {
		r.logger.Error("技能联动：重建 Agent 资产失败", "agent", agent, "skill", skillName, "error", err)
		return bundleFailed(agent, "重建 Agent 资产失败")
	}

	// 发布前最后一道：技能指针仍停在锁内读到的版本才写；否则这次联动是"晚到的旧版本"。
	liveAgain, err := r.deps.Versions.PointerVersion(ctx, core.ResourceSkill, skillName)
	if err != nil {
		r.logger.Error("技能联动：发布前复核技能版本失败", "skill", skillName, "error", err)
		return bundleFailed(agent, "发布前复核技能版本失败")
	}
	if liveAgain != liveID {
		return core.BundlePublishResult{Agent: agent, Status: core.BundlePublishSuperseded}
	}

	dir, err := assets.Resolve(ctx, r.deps.AssetRoot, agent, rebuilt.BundleHash)
	if err != nil {
		r.logger.Error("技能联动：重建资产目录不可解析", "agent", agent, "bundle", rebuilt.BundleHash, "error", err)
		return bundleFailed(agent, "重建资产目录不可解析")
	}
	publisher := newBundlePublisher(r.deps.Versions, r.deps.Operations, r.deps.PayloadStore,
		r.deps.MaxBytes, r.logger, r.now)
	if _, err := publisher.publishAs(ctx, assets.Installed{Manifest: rebuilt, Dir: dir}, actor); err != nil {
		r.logger.Error("技能联动：发布重建资产失败", "agent", agent, "skill", skillName, "error", err)
		return bundleFailed(agent, "发布重建资产失败")
	}
	r.logger.Info("技能联动：已重发 Agent 资产",
		"agent", agent, "skill", skillName, "bundle", rebuilt.BundleHash)
	return core.BundlePublishResult{Agent: agent, Status: core.BundlePublishOK}
}

// resolveRebuildSource 解析重建来源：本机已安装的内容寻址目录优先；只有目录**缺失**
// （`ErrAssetUnavailable`）才从载荷表还原到 `<assetRoot>/bundles/<agent>/.rebuild-*`。
// 还原目录与真实安装目录同深度，`rebuildBundleWithSkill` 因此仍能推出同一个资产根；
// 哈希 / 内容不符（`ErrAssetVersionMismatch`）不退回载荷——那份目录已被篡改。
// resolveRebuildSource prefers the installed directory and falls back to the payload
// table only when the directory is missing, never when it mismatches.
func (r *skillRepublisher) resolveRebuildSource(ctx context.Context, agent string,
	version core.ResourceVersion,
) (string, func(), *core.BundlePublishResult) {
	dir, err := assets.Resolve(ctx, r.deps.AssetRoot, agent, version.ContentHash)
	if err == nil {
		return dir, func() {}, nil
	}
	if !errors.Is(err, core.ErrAssetUnavailable) {
		r.logger.Error("技能联动：已安装资产不可用", "agent", agent, "error", err)
		failure := bundleFailed(agent, "已安装资产不可用")
		return "", nil, &failure
	}
	if r.deps.Payloads == nil || r.deps.MaxBytes <= 0 {
		failure := bundleFailed(agent, "资产载荷读取未接线")
		return "", nil, &failure
	}
	record, ok, err := r.deps.Payloads.ReadBundlePayload(ctx, version.ContentHash)
	if err != nil {
		r.logger.Error("技能联动：读取资产载荷失败", "agent", agent, "error", err)
		failure := bundleFailed(agent, "读取资产载荷失败")
		return "", nil, &failure
	}
	if !ok || len(record.Payload) == 0 {
		return "", nil, resultPointer(bundleMissing(agent, "资产目录与载荷均缺失"))
	}
	parent := filepath.Join(r.deps.AssetRoot, bundleRootDirName, agent)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		r.logger.Error("技能联动：创建载荷还原目录失败", "agent", agent, "error", err)
		failure := bundleFailed(agent, "创建载荷还原目录失败")
		return "", nil, &failure
	}
	temp, err := os.MkdirTemp(parent, ".rebuild-")
	if err != nil {
		r.logger.Error("技能联动：创建载荷还原临时目录失败", "agent", agent, "error", err)
		failure := bundleFailed(agent, "创建载荷还原目录失败")
		return "", nil, &failure
	}
	cleanup := func() { _ = os.RemoveAll(temp) }
	if err := assets.DecodeBundle(ctx, record.Payload, temp, version.ContentHash, r.deps.MaxBytes); err != nil {
		cleanup()
		r.logger.Error("技能联动：资产载荷无法还原", "agent", agent, "error", err)
		failure := bundleFailed(agent, "资产载荷无法还原")
		return "", nil, &failure
	}
	return temp, cleanup, nil
}

// liveSkillRef 把锁内读到的技能版本号还原成重建引用与已发布版本目录：文件清单来自
// 版本行、目录来自登记的 `storage_path`（两种契约形态都认）。**不**读工作副本。
// liveSkillRef rebuilds the skill reference from the recorded version and its published
// version directory; the work copy is never read.
func (r *skillRepublisher) liveSkillRef(ctx context.Context, name string,
	liveID int64,
) (core.SkillVersionRef, string, error) {
	version, err := r.deps.Versions.VersionByID(ctx, liveID)
	if err != nil {
		return core.SkillVersionRef{}, "", fmt.Errorf("读取技能版本 %d: %w", liveID, err)
	}
	if version.Kind != core.ResourceSkill || version.ResourceID != name {
		return core.SkillVersionRef{}, "", fmt.Errorf("版本 %d 属于 %s/%s，不是技能 %s",
			version.ID, version.Kind, version.ResourceID, name)
	}
	if strings.TrimSpace(version.StoragePath) == "" {
		return core.SkillVersionRef{}, "", fmt.Errorf("技能 %s 的版本 %d 缺少存储路径", name, version.ID)
	}
	files, err := parseSkillManifest(name, version.ManifestJSON)
	if err != nil {
		return core.SkillVersionRef{}, "", err
	}
	relDir, err := workspace.PublishedVersionRelDir(version.StoragePath)
	if err != nil {
		return core.SkillVersionRef{}, "", fmt.Errorf("技能 %s 的版本目录不可用: %w", name, err)
	}
	root := filepath.Join(r.deps.WorkspaceRoot, filepath.FromSlash(relDir))
	return core.SkillVersionRef{
		ResourceVersionRef: versionRef(name, version),
		Files:              files,
	}, root, nil
}

// rebuildBundleWithSkill 从**已安装 bundle** 重建一份只替换指定技能引用的清单，并把
// 结果装进同一个资产根；返回安装后的清单（调用方按 `BundleHash` 重新 `Resolve`）。
//
// 冻结签名没有 `Dest` / `assetRoot` 参数（模块卡 §3.1.4）：资产根从 `oldDir` 的
// `<assetRoot>/bundles/<agent>/<hash>` 形态推出，暂存目录建在同一父目录下（同卷、可
// rename），构建完立即由 `assets.Install` 装入内容寻址目录并从暂存目录清理。
//
// 配置文件不参与"换技能"，但 `assets.Build` 对配置文件不做来源哈希校验（`expected ==
// nil`），所以构建后必须用返回清单与旧清单**逐项**比对：任何一项哈希 / 大小变化都说明
// 重建源不是原来那份已发布内容，一律拒绝发布。
// rebuildBundleWithSkill rebuilds from the installed bundle only and refuses to publish
// when the recomputed config file list differs from the old manifest.
func rebuildBundleWithSkill(ctx context.Context, oldDir, agent string,
	ref core.SkillVersionRef, newRoot string,
) (assets.Manifest, error) {
	if agent == "" || ref.Name == "" || newRoot == "" {
		return assets.Manifest{}, fmt.Errorf("%w: 重建输入不完整（agent / 技能名 / 版本目录为空）",
			core.ErrAssetVersionMismatch)
	}
	oldManifest, err := assets.Verify(ctx, oldDir)
	if err != nil {
		return assets.Manifest{}, fmt.Errorf("校验重建来源: %w", err)
	}
	if oldManifest.Agent != agent {
		return assets.Manifest{}, fmt.Errorf("%w: 重建来源属于 Agent %s，不是 %s",
			core.ErrAssetVersionMismatch, oldManifest.Agent, agent)
	}
	assetRoot, err := assetRootOfBundleDir(oldDir, agent)
	if err != nil {
		return assets.Manifest{}, err
	}
	replaced := false
	sources := make([]assets.SkillSource, 0, len(oldManifest.Skills))
	for _, skill := range oldManifest.Skills {
		if skill.Name == ref.Name {
			replaced = true
			sources = append(sources, assets.SkillSource{Ref: ref, Root: newRoot})
			continue
		}
		sources = append(sources, assets.SkillSource{
			Ref:  skill,
			Root: filepath.Join(oldDir, syncBundleSkillsDir, skill.Name),
		})
	}
	if !replaced {
		return assets.Manifest{}, fmt.Errorf("%w: 已安装 bundle 未引用技能 %s",
			core.ErrAssetVersionMismatch, ref.Name)
	}
	parent := filepath.Dir(filepath.Clean(oldDir))
	staging, err := os.MkdirTemp(parent, ".rebuild-build-")
	if err != nil {
		return assets.Manifest{}, fmt.Errorf("创建重建暂存目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	configFiles := make([]string, 0, len(oldManifest.ConfigFiles))
	for _, file := range oldManifest.ConfigFiles {
		configFiles = append(configFiles, file.Path)
	}
	rebuilt, err := assets.Build(ctx, assets.BuildSpec{
		Agent:       oldManifest.Agent,
		AgentUID:    oldManifest.AgentUID,
		ConfigHash:  oldManifest.ConfigHash,
		ConfigRoot:  filepath.Join(oldDir, assets.ConfigBundleRel("")),
		ConfigFiles: configFiles,
		Skills:      sources,
		Dest:        staging,
	})
	if err != nil {
		return assets.Manifest{}, fmt.Errorf("构建重建 bundle: %w", err)
	}
	if !slices.Equal(rebuilt.ConfigFiles, oldManifest.ConfigFiles) {
		return assets.Manifest{}, fmt.Errorf("%w: 重建后的配置文件清单与已发布版本不一致",
			core.ErrAssetVersionMismatch)
	}
	installed, err := assets.Install(ctx, staging, assetRoot)
	if err != nil {
		return assets.Manifest{}, fmt.Errorf("安装重建 bundle: %w", err)
	}
	return installed.Manifest, nil
}

// assetRootOfBundleDir 从 `<assetRoot>/bundles/<agent>/<hash>` 形态推出资产根；形态不符
// （例如调用方传了工作区目录）一律拒绝——安装到错误根会让执行期读不到这份内容。
// assetRootOfBundleDir derives the asset root and refuses any other directory shape.
func assetRootOfBundleDir(dir, agent string) (string, error) {
	clean := filepath.Clean(dir)
	agentDir := filepath.Dir(clean)
	bundlesDir := filepath.Dir(agentDir)
	if filepath.Base(agentDir) != agent || filepath.Base(bundlesDir) != bundleRootDirName {
		return "", fmt.Errorf("%w: 目录 %s 不是 Agent %s 的内容寻址 bundle 目录",
			core.ErrAssetVersionMismatch, dir, agent)
	}
	return filepath.Dir(bundlesDir), nil
}

// bundleMissing / bundleFailed 构造带固定中文原因的结果（契约只在 missing / failed
// 上给 error）。
func bundleMissing(agent, reason string) core.BundlePublishResult {
	return core.BundlePublishResult{Agent: agent, Status: core.BundlePublishMissing, Error: reason}
}

// bundleFailed 构造失败结果：原因是固定短语，底层错误只进日志。
func bundleFailed(agent, reason string) core.BundlePublishResult {
	return core.BundlePublishResult{Agent: agent, Status: core.BundlePublishFailed, Error: reason}
}

// resultPointer 把结果取址，便于"解析重建源"用指针表达失败返回。
func resultPointer(result core.BundlePublishResult) *core.BundlePublishResult {
	return &result
}

// withSkillLinkage 执行技能发布后的联动重发，并把逐 Agent 结果装进发布结果。
// 联动失败**不**回滚技能发布（技能已落库，回滚会让"已发布但无人知道"变成"发布失败却
// 可能已生效"）：错误只落 ERROR 日志，未完成的项由运行期修复器在后续轮次有界重试。
// 没有依赖该技能的 Agent 时 `Publish` 保持 nil——契约只在技能发布触发联动时给该字段。
// withSkillLinkage attaches the per-agent outcomes to a successful skill publish; a
// linkage failure never rolls the publish back and is left to the runtime repairer.
func (s *ResourceAdmin) withSkillLinkage(ctx context.Context, actor core.Actor, skillName string,
	result core.ResourcePublishResult,
) core.ResourcePublishResult {
	results, err := s.republisher.republishDependentBundles(ctx, skillName, actor)
	if err != nil {
		s.logger.Error("技能发布后的联动重发未完成，留给运行期修复器重试",
			"skill", skillName, "error", err)
	}
	if len(results) > 0 {
		result.Publish = results
	}
	return result
}

// republishDependentBundles / RepublishOne 是 `*ResourceAdmin` 上的转发入口（模块卡
// §3.1.4 冻结签名）：发布链与运行期修复器都从同一个装配好的重发器走，不各造一份依赖。
// The ResourceAdmin methods forward to the one assembled republisher.
func (s *ResourceAdmin) republishDependentBundles(ctx context.Context, skillName string,
	actor core.Actor,
) ([]core.BundlePublishResult, error) {
	return s.republisher.republishDependentBundles(ctx, skillName, actor)
}

// RepublishOne 重发单个 Agent（`SkillRepublisher` 接口的生产实现）。
// RepublishOne is the production SkillRepublisher implementation.
func (s *ResourceAdmin) RepublishOne(ctx context.Context, agent, skillName string,
	actor core.Actor,
) core.BundlePublishResult {
	return s.republisher.RepublishOne(ctx, agent, skillName, actor)
}

// 编译期钉住：`*ResourceAdmin` 必须满足消费方接口（模块卡 §3.3 冻结）。
var _ SkillRepublisher = (*ResourceAdmin)(nil)
