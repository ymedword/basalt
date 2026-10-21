package app

import (
	"fmt"
	"log/slog"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
)

// newProviderClients 由配置**逐条**构造显式映射表（宪法 III 从条文变成结构）：遍历的是
// 配置数组（有序、显式），键取条目自己的名字。**不得**遍历映射表推断、**不得**取"唯一的
// 那个"当默认——Go 的 map 遍历顺序随机，任何"从表里挑一个"的写法都会让同一进程的不同请求
// 指到不同供应商（宪法第三条的违反症状）。
// newProviderClients builds the explicit map entry by entry, over the ordered
// configuration slice. Nothing is ever inferred from the map itself.
//
// 重名条目已在装载期被拒（config.go 第 7 类 fail-closed），故此处不会静默丢条目。
// Duplicate names are rejected at load time, so no entry can silently vanish here.
func newProviderClients(cfg *cli.Config) (map[string]core.ProviderClient, map[string]string, error) {
	catalog, err := newProviderCatalog(cfg)
	if err != nil {
		return nil, nil, err
	}
	return catalog.clients, catalog.models, nil
}

// providerCatalog 是装配期**一次**构建的显式 name → client 映射快照（宪法 III 从条文变成
// 结构）。US-12 改为按已发布版本重建同一形状的目录，调用方只认这份快照、不认底下的客户端
// 构造；本 US 用它在注册与运行装配之间共享同一份"哪些 Provider 生效"的读数。
// providerCatalog is the explicit name-to-client snapshot built once at assembly;
// US-12 will rebuild the same shape from published versions.
type providerCatalog struct {
	clients map[string]core.ProviderClient
	models  map[string]string
}

// newProviderCatalog 按配置数组的**有序**条目逐条建表，名字取条目自己的 name——不遍历
// 映射表推断、不取"唯一那个"当默认（宪法 III 的违反症状见 CLAUDE.md 原则 3）。
// newProviderCatalog builds the table entry by entry over the ordered configuration.
func newProviderCatalog(cfg *cli.Config) (*providerCatalog, error) {
	clients := make(map[string]core.ProviderClient, len(cfg.Providers))
	models := make(map[string]string, len(cfg.Providers))

	for i, entry := range cfg.Providers {
		client, err := provider.NewOpenAIClient(provider.Options{
			Name:    entry.Name,
			BaseURL: entry.BaseURL,
			APIKey:  entry.APIKey,
		})
		if err != nil {
			// 下标只有装配层给得出——构造层看得见单个条目，不知道自己在数组里的位置。
			// Only the assembly layer knows the entry's index.
			return nil, fmt.Errorf("providers[%d]: %w", i, err)
		}
		clients[entry.Name] = client
		models[entry.Name] = entry.DefaultModel
	}
	return &providerCatalog{clients: clients, models: models}, nil
}

// newProviderCatalogWithOverride 是目录构造的覆盖感知入口（评测基座的默认后端）：override
// 非 nil 时**照建全表**——每个配置名各一条、都指向同一个替身；表里只放一个条目等于绕开
// 路由查表，评测就失去对"配置名 → 客户端"对应关系的检验（宪法 III）。nil 时等价于按配置
// 逐条构造真实客户端。
// newProviderCatalogWithOverride builds the explicit table with an optional override
// client; every configured name still gets its own entry.
func newProviderCatalogWithOverride(cfg *cli.Config, override core.ProviderClient) (*providerCatalog, error) {
	if override == nil {
		return newProviderCatalog(cfg)
	}
	clients := make(map[string]core.ProviderClient, len(cfg.Providers))
	models := make(map[string]string, len(cfg.Providers))
	for _, entry := range cfg.Providers {
		clients[entry.Name] = override
		models[entry.Name] = entry.DefaultModel
	}
	return &providerCatalog{clients: clients, models: models}, nil
}

// newProviderService 装配路由 + 审计服务：先建映射表，再把它与模型表、仓储、日志器一并
// 显式传给构造函数（依赖全部走类型化参数——业务状态不进 context，宪法 VII）。
// newProviderService wires routing and audit: the map is built first and then passed
// explicitly along with the model table, the store and the logger.
//
// 本 US 无生产调用方（占位点见颗粒度文档 §3.5）：第一个消费者是 US-4 的 serve / chat 装配；
// 本 US 经集成用例走同一条装配路径，「交付即可用」而不是「到 US-4 才第一次被编译进来」。
// US-4 is the first production consumer; the integration case exercises this very
// path today so the deliverable is usable on arrival.
func newProviderService(cfg *cli.Config, store core.LLMCallStore,
	logger *slog.Logger,
) (*provider.ProviderService, error) {
	clients, models, err := newProviderClients(cfg)
	if err != nil {
		return nil, err
	}
	return provider.NewProviderService(clients, models, store, logger), nil
}

// modelClient 返回运行入口消费的模型客户端：路由查表与两阶段审计一步不少（宪法 III + V）。
// 映射表由调用方**一次构建**后传入，运行准入（`runResources.Providers`）与模型调用因此
// 共享同一份目录——"准入查到的表"与"实际调用的表"不可能分叉。
// modelClient wraps the one catalog built by the assembly, so admission and the actual
// model call share the very same explicit name-to-client map.
func modelClient(catalog *providerCatalog, store core.LLMCallStore, logger *slog.Logger) *provider.ProviderService {
	return provider.NewProviderService(catalog.clients, catalog.models, store, logger)
}
