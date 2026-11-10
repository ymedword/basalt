package app

// 本文件是**钉住运行的装载面**（US-25C §2.3.5.1 T2）：调度领取把 bundle 哈希钉进
// 执行行之后，Run 准入必须从**同一份**已安装目录构建 Entry——不读注册表的当前快照，
// 也不读发布指针（领取之后同步可能已推进到新版本）。取值路径与同步轮的
// `EntryFromBundle` 完全同一条：定义取 bundle 内的 `agent/` 段，技能判定取本机已
// 物化的发布版本目录，资源快照取**最近一轮**准备的同一份 `PreparedResources`。
//
// 为什么需要独立类型而不是挂在注册表或同步器上：注册表只持工作区快照、不知道 bundle
// 目录；同步器在控制面没有 Entry 构建器（其 loader 为 nil），两类角色都需要这条读
// 路径。因此装载面按依赖（资产根 / 工作区根 / 分发读面 / Entry 构建器 / 快照盒）
// 单独构造，由装配层绑定到注册表。
// This file is the pinned entry loader: admission rebuilds the entry from the claimed
// bundle through the very same path the sync round uses, never from the live registry.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// preparedResourcesBox 在同步轮次与钉住装载之间传递**最近一轮**的资源准备快照
// （§3.1.4 C14）：同步器每轮 `prepare()` 之后发布，准入按它构建 Entry——两处读到的
// 是同一份 Providers / MCP / 工具名字集，Entry 判定因此不会与准备结果分叉。
//
// 为什么是"最近一轮"而不是准入时现算：准备闭包持 `ResourceGate` 写锁，而准入的
// `LookupPinned` 与 `ResolvePinned` 在读锁内/外有固定顺序，现算会引入新的锁等待与
// 第二套准备语义。同步轮是唯一准备方，最近一轮就是本进程当前的资源事实。
// 零值盒（未接线 / 首轮尚未跑完）读作空快照：Entry 判定按缺项失败关闭，不会当成
// "资源都齐"。
// The box hands the planner's most recent PreparedResources to pinned admission.
type preparedResourcesBox struct {
	mu  sync.RWMutex
	val workspace.PreparedResources
}

// store 发布一轮的新快照。切片由 `prepare()` 每轮新克隆，读侧只读共享。
// store publishes one round's snapshot; the slices are never mutated afterwards.
func (b *preparedResourcesBox) store(val workspace.PreparedResources) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.val = val
	b.mu.Unlock()
}

// load 读最近一轮快照（未发布时为零值，判定按缺项失败关闭）。
// load returns the latest snapshot, or the zero value before the first round.
func (b *preparedResourcesBox) load() workspace.PreparedResources {
	if b == nil {
		return workspace.PreparedResources{}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.val
}

// pinnedEntries 是 `core.PinnedAgentSource` 的生产实现。`LookupPinned` 只按
// `(agent, hash)` 定位已安装目录，目录缺失 / 清单与哈希或身份不符一律
// `core.ErrAssetUnavailable`（本实例现在给不出领取钉住的那一份），**不**退回当前
// 发布指针目录。
// pinnedEntries loads an entry from the claimed bundle; it never falls back.
type pinnedEntries struct {
	assetRoot     string
	workspaceRoot string
	distribution  core.AssetDistributionPort
	loader        BundleEntryBuilder
	prepared      *preparedResourcesBox
}

// 编译期断言：装载面满足 core 声明的钉住消费方端口。
// Compile-time assertion that the loader satisfies the pinned consumer port.
var _ core.PinnedAgentSource = (*pinnedEntries)(nil)

// newPinnedEntries 构造钉住装载面。依赖缺一不可由装配层保证（生产装配全部非空）；
// 这里不做隐藏降级——缺依赖的运行期只会以 `ErrAssetUnavailable` 失败关闭。
// newPinnedEntries builds the pinned loader from its fixed dependencies.
func newPinnedEntries(assetRoot, workspaceRoot string,
	distribution core.AssetDistributionPort, loader BundleEntryBuilder, prepared *preparedResourcesBox,
) *pinnedEntries {
	return &pinnedEntries{
		assetRoot:     assetRoot,
		workspaceRoot: workspaceRoot,
		distribution:  distribution,
		loader:        loader,
		prepared:      prepared,
	}
}

// LookupPinned 从 `<assetRoot>/bundles/<agent>/<hash>/agent` 构建注册条目：
//
//  1. `assets.Resolve` 只按哈希定位（不解析"最新"），`assets.Verify` 逐文件复核；
//  2. 清单身份必须与请求的 agent 与哈希一致（防目录错位冒充）；
//  3. 技能目录按清单记录经版本行投影（与物化同一规则），Entry 判定只读已物化目录；
//  4. Entry 构建走同步轮同一条 `EntryFromBundle`，资源快照取最近一轮准备结果。
//
// 任一失败都包装为 `core.ErrAssetUnavailable`：领取钉住的版本在本实例不可用（尚未
// 安装 / 目录损坏 / Entry 非法），准入据此 503，不重试、不换版本。
// LookupPinned rebuilds the entry from the claimed bundle; every failure is
// ErrAssetUnavailable because the claimed version cannot be served right now.
func (p *pinnedEntries) LookupPinned(ctx context.Context, agent, hash string) (core.AgentEntry, error) {
	agent = strings.TrimSpace(agent)
	hash = strings.TrimSpace(hash)
	if agent == "" || hash == "" {
		return core.AgentEntry{}, fmt.Errorf("%w: 钉住装载缺少 agent 或 bundle 哈希", core.ErrAssetUnavailable)
	}
	if p.loader == nil || p.distribution == nil || strings.TrimSpace(p.assetRoot) == "" ||
		strings.TrimSpace(p.workspaceRoot) == "" {
		return core.AgentEntry{}, fmt.Errorf("%w: 钉住装载面未接线（agent=%s）", core.ErrAssetUnavailable, agent)
	}
	dir, err := assets.Resolve(ctx, p.assetRoot, agent, hash)
	if err != nil {
		return core.AgentEntry{}, pinnedUnavailable(agent, hash, err)
	}
	manifest, err := assets.Verify(ctx, dir)
	if err != nil {
		return core.AgentEntry{}, pinnedUnavailable(agent, hash, err)
	}
	if manifest.Agent != agent || manifest.BundleHash != hash {
		return core.AgentEntry{}, fmt.Errorf("%w: 已安装目录 %s 的清单属于 agent=%s hash=%s（请求 %s / %s）",
			core.ErrAssetUnavailable, dir, manifest.Agent, manifest.BundleHash, agent, hash)
	}
	// 技能目录按清单记录投影（与物化同一规则）：目录必须已经由同步轮物化到位；
	// 投影失败说明版本行或路径与 bundle 记录不符，同样按不可用失败关闭。
	published := make(map[string]string, len(manifest.Skills))
	for _, ref := range manifest.Skills {
		relDir, _, err := resolveSkillMaterialization(ctx, p.distribution, p.workspaceRoot, ref)
		if err != nil {
			return core.AgentEntry{}, pinnedUnavailable(agent, hash, err)
		}
		published[ref.Name] = relDir
	}
	entry, err := p.loader.EntryFromBundle(p.workspaceRoot,
		assets.Installed{Manifest: manifest, Dir: dir}, p.prepared.load(), published)
	if err != nil {
		return core.AgentEntry{}, pinnedUnavailable(agent, hash, err)
	}
	return entry, nil
}

// pinnedUnavailable 统一钉住装载的错误分类：保留原始原因文本，错误链指向
// `core.ErrAssetUnavailable`，web 因此映射为 503 `ASSET_UNAVAILABLE`。
// pinnedUnavailable wraps any lookup failure as ErrAssetUnavailable.
func pinnedUnavailable(agent, hash string, cause error) error {
	return fmt.Errorf("%w: 钉住 bundle agent=%s hash=%s: %v", core.ErrAssetUnavailable, agent, hash, cause)
}
