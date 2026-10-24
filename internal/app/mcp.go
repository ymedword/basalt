package app

// 本文件是装配侧的 MCP 辅助（US-7）：连接器接口（`App` 的测试夹层，沿 `providerOverride`
// 先例——生产实现是 `*tool.MCPPool`，测试注入假连接器）与连接结果 → 工具名集。
// 装配层是唯一知道"接口背后是哪个实例"的地方；连接池本身归 `internal/tool`。
// This file holds the assembly-side MCP helpers: the connector seam (mirroring
// providerOverride) and the connected tool-name set. The pool itself belongs to
// internal/tool.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// mcpRuntime 是已发布 MCP 定义在运行期的**只读句柄**（颗粒度文档 §3.1.32 的消费方视图）：
// 本 US 承载装配期算出的两份有序名单——已配置（文件序）与已连接（实际连上，文件序）。
// 名单顺序就是注册序判据（server 文件序 → 工具列表序），故不换成 map 保存。
// US-12 的 supervisor 接管启动 / 停止后沿用同一形状，并在句柄上带出版本号快照。
// mcpRuntime is the read-only runtime view of MCP definitions: the configured and
// connected name lists, both kept in file order because order is the registration
// criterion.
type mcpRuntime struct {
	specs []core.MCPServerConfig
	conns []tool.MCPConnection
	// supervisor 非 nil 时，连接与工具名单实时取自 supervisor 快照——US-12 起连接
	// 生命周期归 supervisor，句柄不再持有连接池结果（`conns` 只服务无 supervisor
	// 的测试夹具与旧装配路径）。
	// When set, connection state is read live from the supervisor snapshot; conns only
	// serves fixtures without a supervisor.
	supervisor *MCPSupervisor
}

// newMCPRuntime 复制两份输入后持有——装配期的切片后续即使被改写也不影响这句话柄。
// newMCPRuntime copies both inputs so later assembly-side edits cannot leak in.
func newMCPRuntime(specs []core.MCPServerConfig, conns []tool.MCPConnection) *mcpRuntime {
	return &mcpRuntime{specs: slices.Clone(specs), conns: slices.Clone(conns)}
}

// newMCPRuntimeFromSupervisor 让句柄以 supervisor 状态为准：配置名单仍是启动时的文件序
// 快照，连接 / 工具 / 应用版本每次读取都取 supervisor 的当前状态（启停后立即生效）。
// newMCPRuntimeFromSupervisor reads connection state live from the supervisor while the
// configured list stays the file-order snapshot captured at startup.
func newMCPRuntimeFromSupervisor(supervisor *MCPSupervisor, specs []core.MCPServerConfig) *mcpRuntime {
	return &mcpRuntime{specs: slices.Clone(specs), supervisor: supervisor}
}

// ConfiguredNames 返回已配置 server 名（文件序）——装载器据此判定"声明了不存在的 server"。
// ConfiguredNames lists the configured servers in file order.
func (m *mcpRuntime) ConfiguredNames() []string {
	return mcpConfigNames(m.specs)
}

// mcpConfigNames 按给定顺序列出 server 名；启动早期的第一遍装载在发布版本尚不存在时
// 用它保持与文件同序（序即注册序判据）。
// mcpConfigNames lists server names in the given order; order is the registration criterion.
func mcpConfigNames(specs []core.MCPServerConfig) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// ConnectedNames 返回实际连上的 server 名（文件序）——非 nil 的"已连接"集是重路径
// "知道连接结果"的判据，装载器据此直接拒载失败 server 的 Agent。
// ConnectedNames lists the servers that actually connected, in file order.
func (m *mcpRuntime) ConnectedNames() []string {
	if m.supervisor != nil {
		snapshot := m.supervisor.Snapshot()
		names := make([]string, 0, len(m.specs))
		for _, spec := range m.specs {
			if status, ok := snapshot[spec.Name]; ok && status.Connected {
				names = append(names, spec.Name)
			}
		}
		return names
	}
	names := make([]string, 0, len(m.conns))
	for _, conn := range m.conns {
		names = append(names, conn.Server)
	}
	return names
}

// Tools 返回该 server 当前连接上发现的工具名（连接序）；未连接 / 未知 server 为空列表。
// Tools lists the tools discovered on the live connection, empty when not connected.
func (m *mcpRuntime) Tools(name string) []string {
	tools := []string{}
	if m.supervisor != nil {
		status, ok := m.supervisor.Status(name)
		if !ok || !status.Connected {
			return tools
		}
		return append(tools, status.Tools...)
	}
	for _, conn := range m.conns {
		if conn.Server != name {
			continue
		}
		for _, t := range conn.Tools {
			tools = append(tools, t.Name())
		}
	}
	return tools
}

// AppliedVersion 返回 supervisor 侧记录的已应用版本；没有 supervisor 或尚未接管时
// 返回 false（调用方把"无 applied"当作未生效）。
// AppliedVersion reports the supervisor-side applied version; false means none yet.
func (m *mcpRuntime) AppliedVersion(name string) (int64, bool) {
	if m.supervisor == nil {
		return 0, false
	}
	status, ok := m.supervisor.Status(name)
	if !ok || status.AppliedVersionID == 0 {
		return 0, false
	}
	return status.AppliedVersionID, true
}

// PendingRestart 派生 pending_restart：发布版本非零且与已应用版本不一致（含尚未应用）。
// 注 13 真值表——待重启的 server 仍可用旧版本工具，但新发布的内容要重启进程才生效。
// PendingRestart derives published != applied, including the never-applied case.
func (m *mcpRuntime) PendingRestart(name string, publishedVersionID int64) bool {
	if publishedVersionID == 0 {
		return false
	}
	applied, ok := m.AppliedVersion(name)
	return !ok || applied != publishedVersionID
}

// mcpConnector 是装配层对 MCP 连接池的最小依赖面：生产实现 = `*tool.MCPPool`；
// 测试直接设置包内字段 `App.mcpConnect` 注入替身——**不新增导出测试 API**。
// `Connect` 只在整体取消 / 截止时返回错误；其余失败由实现吸收为跳过（US2）。
// mcpConnector is the assembly layer's minimal view of the pool; tests inject a fake
// through the package-internal App field instead of an exported seam.
type mcpConnector interface {
	Connect(ctx context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, error)
	Close() error
}

// mcpToolNames 按连接顺序收集工具名（server 文件序 → 工具列表序）：顺序本身是注册序的
// 判据，重名由注册池按先到先得判定（内置优先）。
// mcpToolNames lists the connected tool names in connection order.
func mcpToolNames(conns []tool.MCPConnection) []string {
	var names []string
	for _, conn := range conns {
		for _, t := range conn.Tools {
			names = append(names, t.Name())
		}
	}
	return names
}

// publishedMCPServers 把**已发布版本**解析成 supervisor 的启动输入（图 A ⑧ / FR17）：
// 先按配置文件序排（序即注册序判据），文件里已删但指针保留的 server 排在其后、按
// resource_id 升序。任一条解析 / 校验 / 占位解析失败都返回错误（fail-closed）——运行
// 对象只由已发布版本构建，**不**回退到配置文件内容。返回顺序即连接与注册顺序。
// publishedMCPServers resolves published MCP versions into supervisor inputs, in
// configuration order followed by pointer-only servers; any invalid entry fails closed.
func publishedMCPServers(ctx context.Context, store *storage.ResourceStore, fileOrder []string,
	lookup func(string) (string, bool),
) ([]core.PublishedMCPServer, error) {
	pointers, err := store.ListPointers(ctx, core.ResourceMCPServer)
	if err != nil {
		return nil, fmt.Errorf("读取已发布 MCP server 指针: %w", err)
	}
	byName := make(map[string]core.ResourcePointer, len(pointers))
	for _, pointer := range pointers {
		byName[pointer.ResourceID] = pointer
	}
	ordered := make([]core.ResourcePointer, 0, len(pointers))
	seen := make(map[string]struct{}, len(pointers))
	for _, name := range fileOrder {
		if pointer, ok := byName[name]; ok {
			ordered = append(ordered, pointer)
			seen[name] = struct{}{}
		}
	}
	for _, pointer := range pointers { // ListPointers 已按 resource_id 升序
		if _, ok := seen[pointer.ResourceID]; ok {
			continue
		}
		ordered = append(ordered, pointer)
	}

	servers := make([]core.PublishedMCPServer, 0, len(ordered))
	for _, pointer := range ordered {
		version, err := store.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return nil, fmt.Errorf("读取 MCP server %s 的发布版本 %d: %w",
				pointer.ResourceID, pointer.PublishedVersionID, err)
		}
		if version.Kind != core.ResourceMCPServer || version.ResourceID != pointer.ResourceID {
			return nil, fmt.Errorf("MCP server %s 的发布版本 %d 归属不符（got %s/%s）",
				pointer.ResourceID, version.ID, version.Kind, version.ResourceID)
		}
		cfg, err := workspace.ParseMCPServerBlock([]byte(version.DefinitionYAML))
		if err != nil {
			return nil, fmt.Errorf("MCP server %s 的发布版本 %d 定义非法: %w",
				pointer.ResourceID, version.ID, err)
		}
		if cfg.Name != pointer.ResourceID {
			return nil, fmt.Errorf("MCP server %s 的发布版本 %d 条目名 %q 与资源标识不一致",
				pointer.ResourceID, version.ID, cfg.Name)
		}
		// 占位解析发生在**任何连接之前**（技术方案 §3.7）：解析不出来就不连接，错误里
		// 只有 server / 键 / 变量名，没有值。
		// Placeholder resolution happens before any connection; errors never carry values.
		resolved, err := resolveMCPEnv([]core.MCPServerConfig{cfg}, lookup)
		if err != nil {
			return nil, err
		}
		servers = append(servers, core.PublishedMCPServer{VersionID: version.ID, Config: resolved[0]})
	}
	return servers, nil
}

// mcpPublishedConfigs 抽取配置（运行期句柄只要配置与文件序名集）。
// mcpPublishedConfigs extracts the configs from published servers.
func mcpPublishedConfigs(servers []core.PublishedMCPServer) []core.MCPServerConfig {
	specs := make([]core.MCPServerConfig, 0, len(servers))
	for _, server := range servers {
		specs = append(specs, server.Config)
	}
	return specs
}

// markMCPApplied 记录 supervisor 连接成功后的 applied 版本。只有发布指针仍指向该版本时
// 才写：期间若有更新的发布，把旧版本写回指针就是**倒退发布**——跳过即可，`pending_restart`
// 会如实为真（约束 21 的真值表）。server 已删除时无行可写，同样直接返回。
// markMCPApplied records applied only while the pointer still names that version; a
// newer publish must never be rolled back, and a deleted server has no row to update.
func markMCPApplied(ctx context.Context, store *storage.ResourceStore, name string, versionID int64,
	now func() time.Time,
) error {
	pointers, err := store.ListPointers(ctx, core.ResourceMCPServer)
	if err != nil {
		return fmt.Errorf("读取 MCP server %s 的发布指针: %w", name, err)
	}
	for _, pointer := range pointers {
		if pointer.ResourceID != name {
			continue
		}
		if pointer.PublishedVersionID != versionID {
			return nil
		}
		if err := store.Publish(ctx, core.ResourceMCPServer, name, versionID, true, pointer.OperationID, now()); err != nil {
			return fmt.Errorf("记录 MCP server %s 的应用版本 %d: %w", name, versionID, err)
		}
		return nil
	}
	return nil
}

// resolveMCPEnv 在装配期把 `${VAR}` 占位换成环境值（技术方案 §3.7：所有占位在装配前统一
// 解析）。**未设置与空值同档拒绝**——空凭证送给 server 只会换来一个 401，排障方向会被引到
// server 身上；错误点名 server / 键 / 变量名，**不回显值**（值可能正是凭证）。lookup 注入：
// 生产传 `os.LookupEnv`，用例传替身（测试缝纪律：不为测试新增导出 API）。
// resolveMCPEnv resolves ${VAR} placeholders at assembly time. Missing and empty values
// are refused alike; the error names the server, key and variable but never the value.
func resolveMCPEnv(specs []core.MCPServerConfig, lookup func(string) (string, bool)) ([]core.MCPServerConfig, error) {
	// 逐条构造新切片而不是回写入参下标：gosec G602 对「range 下标回写」判越界
	// （置信度高、属误报），但项目口径是**修实现不改判据**、抑制手段默认禁用——
	// 构造副本既消除判定，又让「入参不被就地改写」成为调用方可依赖的性质。
	// Build a fresh slice instead of writing back through a range index: G602 flags
	// that pattern, suppression is disabled here, and callers get a copy for free.
	resolved := make([]core.MCPServerConfig, 0, len(specs))
	for _, spec := range specs {
		env := make(map[string]string, len(spec.Env))
		for key, ref := range spec.Env {
			varName := strings.TrimSuffix(strings.TrimPrefix(ref, "${"), "}")
			value, ok := lookup(varName)
			if !ok || value == "" {
				return nil, fmt.Errorf("mcp_servers.yaml 的 server %s 的 env.%s 引用的 %s 未设置或为空",
					spec.Name, key, varName)
			}
			env[key] = value // 值不进错误、不进日志
		}
		spec.Env = env
		resolved = append(resolved, spec)
	}
	return resolved, nil
}
