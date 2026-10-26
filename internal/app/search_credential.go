package app

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
)

// 本文件是 `web_search` 的**搜索凭据解析面**（14D T126）：工具层只声明消费方端口
// `tool.WebSearchCredentialResolver`，生产实现落在这里——沿用既有凭据解析服务的
// **同一条**取键路径（`catalogAPIKey`：env 走 `${VAR}` 精确解析、managed 走发布版本的
// 当前 active 版本），不新造第二条读凭证的入口。
// This file is the production credential face behind web_search: it resolves the agent's
// bound model connection and reuses the same key path as the provider catalog.

// webSearchCredentialSlot 是两段绑定的槽位：装配期先建（`versions` 与凭据解析器在
// 工具注册之前就绪），Agent 注册表构建之后再 `Bind`。调用发生在 Run 内，那一刻注册表
// 必然已绑定；未绑定即**失败关闭**（返回明确错误），不猜默认连接、不回退 env。
// The two-stage slot: constructed before tool registration and bound once the agent
// registry exists; an unbound slot fails closed instead of guessing a connection.
type webSearchCredentialSlot struct {
	mu          sync.RWMutex
	agents      core.AgentSource
	versions    *storage.ResourceStore
	credentials managedCredentialSource
}

// 编译期断言：槽位可直接作为工具层的凭据解析面使用。
// Compile-time assertion that the slot satisfies the tool-side port.
var _ tool.WebSearchCredentialResolver = (*webSearchCredentialSlot)(nil)

// newWebSearchCredentialSlot 建未绑定槽位（`versions` / `credentials` 装配期确定、之后只读）。
// newWebSearchCredentialSlot builds the unbound slot.
func newWebSearchCredentialSlot(versions *storage.ResourceStore, credentials managedCredentialSource) *webSearchCredentialSlot {
	return &webSearchCredentialSlot{versions: versions, credentials: credentials}
}

// Bind 绑定 Agent 注册表（装配期一次；注册表本身以快照整体替换承载热加载，引用稳定）。
// Bind publishes the agent registry once during assembly.
func (s *webSearchCredentialSlot) Bind(agents core.AgentSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents = agents
}

// ResolveSearchCredential 按 Agent 名取搜索键：Agent → `provider.name` → 已发布模型连接
// 指针 → 发布版本条目 → 既有凭据解析。全部失败都只点名 Agent / 连接，**不回显任何材料**；
// 不校验连接是否 DeepSeek——兼容代理是合法部署，连接配错的症状是官方 401，如实上报即可
// （14D T126-R1）。
// ResolveSearchCredential maps agent to model connection to the same key path the provider
// catalog uses; it never echoes material and never guesses a connection.
func (s *webSearchCredentialSlot) ResolveSearchCredential(ctx context.Context, agentName string) (string, error) {
	s.mu.RLock()
	agents := s.agents
	s.mu.RUnlock()
	if agents == nil {
		// 装配顺序错误：宁可整次调用失败，也不拿别的连接顶上。
		return "", fmt.Errorf("搜索凭据解析面尚未绑定 Agent 注册表（装配顺序错误）")
	}
	if s.versions == nil || s.credentials == nil {
		return "", fmt.Errorf("搜索凭据解析面缺少资源存储或凭据解析器（装配错误）")
	}
	agent, ok := agents.Lookup(agentName)
	if !ok || agent.Profile == nil {
		return "", fmt.Errorf("无法定位 Agent %s 的模型连接：不在注册表中", agentName)
	}
	providerName := strings.TrimSpace(agent.Profile.Provider.Name)
	if providerName == "" {
		return "", fmt.Errorf("无法确定 Agent %s 的模型连接：未声明 provider.name", agentName)
	}
	pointer, err := s.versions.Pointer(ctx, core.ResourceModelConnection, providerName)
	if err != nil {
		return "", fmt.Errorf("取不到 Agent %s 的模型连接 %s 的已发布版本: %w", agentName, providerName, err)
	}
	entry, version, err := publishedProviderEntry(ctx, s.versions, pointer)
	if err != nil {
		return "", err
	}
	// 与模型调用同一条取键路径（env / managed 两种模式都在这里分派，不按值形态推断）。
	// The very same key path as the model catalog: both credential modes branch here.
	return catalogAPIKey(ctx, pointer, entry, version, s.credentials)
}
