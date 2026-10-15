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
			return nil, nil, fmt.Errorf("providers[%d]: %w", i, err)
		}
		clients[entry.Name] = client
		models[entry.Name] = entry.DefaultModel
	}
	return clients, models, nil
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
