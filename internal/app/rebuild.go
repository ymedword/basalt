package app

// 本文件是本地实例的**重建接缝**（US-24 P-21 / FR-24-25）：准入发现本地实例自报的构建
// 证据与目标版本不一致时，按**同一目标版本**重建实例并原子换入，再比一次。
//
// 两条纪律：
//
//   - **按同一目标版本**重建，不跟随发布指针：换到"最新版本"会让本次准入清单里记的版本
//     与实际实例分叉；目标版本由准入给出，仓库行按 ID 读回并逐项核对归属。
//   - **重建只换本地实例**：Provider 走目录快照的写时复制换入（旧快照继续服务在途 Run），
//     MCP 走 supervisor 的连接重建（`RebuildAtApplied`）——两者都**不**重写共享发布记录；
//     准入**不得**隐式执行 MCP apply（D-12）。
//
// This file is the local-instance rebuild seam. It never follows the publish pointer, never
// rewrites a shared publication record, and never performs an implicit MCP apply.

import (
	"context"
	"errors"
	"fmt"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// ResourceRebuildPort 是准入消费的重建接缝（接口定义在消费方）：`target` 是准入已固化
// 的版本引用（Provider 取发布版本、MCP 取已应用版本），实现必须按**它**重建。nil 实现
// 只在轻装配里出现；此时发生不一致按 503 拒绝，不静默放行。
// ResourceRebuildPort rebuilds one local instance at the already-pinned target version.
type ResourceRebuildPort interface {
	// RebuildProvider 按目标版本重建模型连接并换入目录；`pin` 是准入冻结的凭据引用，
	// 重建必须把同一份材料绑定进新客户端。
	// RebuildProvider rebuilds the provider client at the target version and pin.
	RebuildProvider(ctx context.Context, target core.ResourceVersionRef, pin core.CredentialPin) error
	// RebuildMCP 按目标（已应用）版本重建 MCP 连接，**不**改写 applied 指针。
	// RebuildMCP rebuilds the MCP connection at the applied version without moving it.
	RebuildMCP(ctx context.Context, target core.ResourceVersionRef) error
}

// localRebuilder 是 `ResourceRebuildPort` 的生产实现：只持有重建所需的最少依赖，
// 所有权重的库内事实都按版本 ID 重读。
// localRebuilder holds the minimum dependencies; every fact is re-read by version id.
type localRebuilder struct {
	versions    *postgres.ResourceStore
	catalog     *providerCatalogStore
	credentials catalogCredentialSource
	mcp         *mcpRuntime
	mcpCreds    mcpCredentialResolver
}

// 编译期断言：生产实现满足消费方端口。
// Compile-time assertion that the production rebuilder satisfies its consumer port.
var _ ResourceRebuildPort = (*localRebuilder)(nil)

// newLocalRebuilder 构造重建接缝。依赖均可为 nil——缺失在具体重建路径处失败关闭，
// 装配期不因"这个实例用不到 MCP"而拒绝启动。
// newLocalRebuilder tolerates nil dependencies; a missing one fails that path closed.
func newLocalRebuilder(versions *postgres.ResourceStore, catalog *providerCatalogStore,
	credentials catalogCredentialSource, mcp *mcpRuntime, mcpCreds mcpCredentialResolver,
) ResourceRebuildPort {
	return &localRebuilder{
		versions:    versions,
		catalog:     catalog,
		credentials: credentials,
		mcp:         mcp,
		mcpCreds:    mcpCreds,
	}
}

// rebuildVersion 按 ID 读回版本行并逐项核对它仍是准入固化的那一版：种类、资源名、资源
// UID、内容哈希任一不符即拒绝重建——用别的行重建出来的实例不能声称自己是指定版本。
// rebuildVersion re-reads the row and refuses to rebuild from a version that is not the
// very one admission pinned.
func (b *localRebuilder) rebuildVersion(ctx context.Context, kind core.ResourceKind,
	target core.ResourceVersionRef,
) (core.ResourceVersion, error) {
	if b == nil || b.versions == nil {
		return core.ResourceVersion{}, errors.New("重建接缝未接资源仓储")
	}
	version, err := b.versions.VersionByID(ctx, target.VersionID)
	if err != nil {
		return core.ResourceVersion{}, err
	}
	if version.Kind != kind || version.ResourceID != target.Name ||
		version.ResourceUID != target.ResourceUID || version.ContentHash != target.ContentHash {
		return core.ResourceVersion{}, fmt.Errorf(
			"%s/%s 的目标版本 %d 归属不符（got %s/%s uid=%s hash=%s）",
			kind, target.Name, target.VersionID, version.Kind, version.ResourceID,
			version.ResourceUID, version.ContentHash)
	}
	return version, nil
}

// RebuildProvider 按目标版本重建一个 Provider 客户端并换入目录快照。重建路径**不取
// `ResourceGate` 写锁**：调用方（准入）正持有读锁，`RWMutex` 不可重入；目录换入本身是
// 原子指针写，与发布路径的写锁序列化关系不受影响——发布拿不到写锁就说明它在等本次准入
// 结束，而本次准入结束后它会更新的就是它自己那份新表。
// RebuildProvider swaps a rebuilt client into the catalog without taking the gate write
// lock, which the admission read lock would deadlock against.
func (b *localRebuilder) RebuildProvider(ctx context.Context, target core.ResourceVersionRef,
	pin core.CredentialPin,
) error {
	if b == nil || b.catalog == nil {
		return errors.New("重建接缝未接目录持有者")
	}
	version, err := b.rebuildVersion(ctx, core.ResourceModelConnection, target)
	if err != nil {
		return err
	}
	entries, err := publishedProviderEntries(version.DefinitionYAML)
	if err != nil {
		return fmt.Errorf("模型连接 %s 的目标版本 %d 定义非法: %w", target.Name, version.ID, err)
	}
	if len(entries) != 1 || entries[0].Name != target.Name {
		return fmt.Errorf("模型连接 %s 的目标版本 %d 必须恰含一个同名条目（实际 %d 条）",
			target.Name, version.ID, len(entries))
	}
	entry := entries[0]
	// 指针只作**身份载体**传给取 Key 的函数：四个字段全部来自刚核对过的版本行，
	// 不重读当前发布指针——重建的目标是准入固化的那一版，不是"现在发布指向谁"。
	// The pointer is an identity carrier built from the verified row, never a re-read of
	// the current publish pointer.
	pointer := core.ResourcePointer{
		Kind:               version.Kind,
		ResourceID:         version.ResourceID,
		ResourceUID:        version.ResourceUID,
		PublishedVersionID: version.ID,
	}
	apiKey, err := catalogAPIKey(ctx, pointer, entry, version, b.credentials)
	if err != nil {
		return err
	}
	client, err := provider.NewOpenAIClient(provider.Options{
		Name:    entry.Name,
		BaseURL: entry.BaseURL,
		APIKey:  apiKey,
	})
	if err != nil {
		return fmt.Errorf("重建模型连接 %s: %w", entry.Name, err)
	}
	// 构建证据与客户端同一份输入（版本行 + 同一份定义算出的 pin），换入时**一起**换：
	// 只换客户端会让旧证据继续描述新实例，准入拿到的就是"错误的证据"。证据必须按
	// **实际构建材料**如实记录——若它与准入冻结的 pin 已不同代，准入的再比会失败并
	// 返回 503，那是正确结果（本轮 Run 的期望已过期），不能靠改写证据掩盖。
	// The proof records what the client was actually built from; an admission whose pin
	// has moved on is refused by the re-compare, never talked around by rewriting it.
	built, err := builtVersionFrom(ctx, entry.Name, version, version.DefinitionYAML, b.credentials)
	if err != nil {
		return err
	}
	// 重建材料必须就是准入冻结的那一份：不等说明凭据在窗口内又换了一代，此时**不换入**
	// ——换进去的是一个与本次清单不符的实例。拒绝后由下一次准入按新 pin 重新修复。
	// The rebuilt material must be the one admission froze; otherwise refuse to swap.
	if built.Credential != pin {
		return fmt.Errorf("模型连接 %s 的凭据在准入窗口内换代（准入 %s@%d，重建 %s@%d）",
			entry.Name, pin.ID, pin.Version, built.Credential.ID, built.Credential.Version)
	}
	b.catalog.Store(b.catalog.Load().with(entry.Name, client, entry.DefaultModel, built))
	return nil
}

// RebuildMCP 按目标（已应用）版本重建一个 MCP 连接：解析目标版本的已发布定义 → 按声明
// 来源解析凭据 → 走 supervisor 的连接重建。**不**改 applied 指针（`RebuildAtApplied`）。
// RebuildMCP reconnects one server at the applied version and leaves the pointer alone.
func (b *localRebuilder) RebuildMCP(ctx context.Context, target core.ResourceVersionRef) error {
	if b == nil || b.mcp == nil || b.mcp.supervisor == nil {
		return errors.New("重建接缝未接 MCP supervisor")
	}
	if b.mcpCreds == nil {
		return errors.New("重建接缝未接 MCP 凭据解析面")
	}
	version, err := b.rebuildVersion(ctx, core.ResourceMCPServer, target)
	if err != nil {
		return err
	}
	cfg, err := workspace.ParseMCPServerBlock([]byte(version.DefinitionYAML))
	if err != nil {
		return fmt.Errorf("mcp_server %s 的目标版本 %d 定义非法: %w", target.Name, version.ID, err)
	}
	if cfg.Name != version.ResourceID {
		return fmt.Errorf("mcp_server %s 的目标版本 %d 条目名 %q 与资源标识不一致",
			target.Name, version.ID, cfg.Name)
	}
	// 凭据只按**声明来源**取：managed 取不到即失败，**不**回落进程环境（与启动解析同一
	// 口径）；解析出的值用完立刻归零，不留在重建路径的局部变量之外。代次冻结先于解析
	// （与 `publishedMCPServers` 同一理由，US-25C §2.3.8 P6）：窗口内换代最多多重建一次，
	// 反序则可能把新代次配旧明文记进连接。
	// Values are resolved by the declared source only and zeroed right after use; the
	// generation is frozen first so a race errs toward one extra rebuild.
	frozen, err := b.mcpCreds.Freeze(ctx, target.Name, version.ResourceUID, cfg)
	if err != nil {
		return err
	}
	values, err := b.mcpCreds.Resolve(ctx, target.Name, version.ResourceUID, cfg)
	if err != nil {
		return err
	}
	resolved := ApplyResolvedMCP(cfg, values)
	ZeroMCPCredentialValues(values)
	published := core.PublishedMCPServer{
		VersionID:            version.ID,
		CredentialGeneration: mcpCredentialGeneration(frozen),
		Config:               resolved,
	}
	if err := b.mcp.supervisor.RebuildAtApplied(context.WithoutCancel(ctx), published); err != nil {
		return err
	}
	return nil
}
