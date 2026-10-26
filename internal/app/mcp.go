package app

// 本文件是装配侧的 MCP 辅助（US-7）：连接器接口（`App` 的测试夹层，沿 `providerOverride`
// 先例——生产实现是 `*tool.MCPPool`，测试注入假连接器）与连接结果 → 工具名集。
// 装配层是唯一知道"接口背后是哪个实例"的地方；连接池本身归 `internal/tool`。
// This file holds the assembly-side MCP helpers: the connector seam (mirroring
// providerOverride) and the connected tool-name set. The pool itself belongs to
// internal/tool.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// dispositions 是装配期写一次的工具来源表（D-06）；nil 表示本次装配没有来源表，
	// 视图按"未知"处理（`tool_disposition: null`），不伪造空对象。
	// dispositions is the assembly-time source table; nil means unknown, never an empty object.
	dispositions *mcpDispositionTable
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

// mcpRevokerOf 把句柄的 supervisor 投影成"按名回收连接"的回调；句柄或 supervisor 缺席
// 时返回 nil（调用方据此跳过回收，而不是在只读装配里 panic）。
// mcpRevokerOf projects the supervisor into a by-name teardown callback; nil means the
// process has no MCP runtime to tear down.
func mcpRevokerOf(runtime *mcpRuntime) func(ctx context.Context, name string) error {
	if runtime == nil || runtime.supervisor == nil {
		return nil
	}
	return runtime.supervisor.RevokeServer
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

// ToolDisposition 返回该 server 的工具来源投影（D-06）；未连接 / 未知 / 没有来源表
// 一律 nil——视图据此输出 `null`。已连接但一个工具都没提供时返回零计数对象：这是
// "已注册 0 个"的事实（D-06：连接后 offered = len(tools)），不是未知。
// ToolDisposition returns nil for unconnected or unknown servers. A connected server
// that offered no tools gets a zero-count object, not null: connected implies
// offered == len(tools), and null has no counts to compare.
func (m *mcpRuntime) ToolDisposition(name string) *core.MCPToolDisposition {
	if m == nil || m.dispositions == nil {
		return nil
	}
	if !slices.Contains(m.ConnectedNames(), name) {
		return nil
	}
	if disposition := m.dispositions.view(name); disposition != nil {
		return disposition
	}
	return &core.MCPToolDisposition{Skipped: []core.MCPToolSkip{}}
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

// mcpRegisterTools 把一个连接批次里的工具**按 server 分组整组登记**（14D）：先撤下该
// server 此前的名字与来源条目，再按连接序 → 工具列表序登记。用整组替换而不是逐条登记，
// 是因为重连 / 受控重建后的工具集可能变小：逐条登记会把上一次的名字留成孤儿，声明它们
// 的 Agent 仍能解析到一个已经没有连接的实例。
// 重名语义不变：注册池拒绝重复并保留原注册（内置赢家，约束 8 / D-06），来源表照实记
// `duplicate_name` 与赢家来源。
// mcpRegisterTools registers each connected server's tools as one owned set (replacing any
// earlier set) so a shrink cannot leave orphan names behind; duplicates are still refused.
func mcpRegisterTools(registry *tool.Registry, dispositions *mcpDispositionTable,
	conns []tool.MCPConnection, logger *slog.Logger,
) {
	for _, conn := range conns {
		dispositions.resetServer(conn.Server)
		errs := registry.ReplaceOwner(conn.Server, conn.Tools)
		for i, t := range conn.Tools {
			dispositions.recordMCP(conn.Server, t.Name(), errs[i])
		}
		if logger != nil {
			logger.Debug("MCP 工具组已登记", "server", conn.Server, "offered", len(conn.Tools))
		}
	}
}

// mcpReplaceTools 在受控重建的提交段里整组换掉一个 server 的注册池条目与来源条目：
// 撤下该归属方此前的名字，再按给定顺序登记，重名照旧拒绝并保留原注册（内置赢家）。
// 注册池与来源表必须成对更新——否则页面会看到"名字已换、来源还是旧的"。
// mcpReplaceTools swaps one owner's registry entries and source records together; the
// two must move as a pair or the view would show one generation's names with another's
// provenance.
func mcpReplaceTools(registry *tool.Registry, dispositions *mcpDispositionTable,
	server string, tools []core.Tool,
) {
	dispositions.resetServer(server)
	errs := registry.ReplaceOwner(server, tools)
	for i, t := range tools {
		dispositions.recordMCP(server, t.Name(), errs[i])
	}
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
// resource_id 升序。运行对象只由已发布版本构建，**不**回退到配置文件内容。返回顺序即
// 连接与注册顺序。
//
// **启动隔离**（设计 §2.5.1 / 验收 E3，T115）：某一条的凭据当前取不到值（env 变量缺失 /
// 为空、托管未录入 / 已撤销 / 版本非 active）只让**它自己**未就绪——跳过该 server、不建
// 连接，其余照常启动；页面经资源视图看到它的槽位状态，引用它的 Agent 按装载规则显示
// 未就绪。其余任何失败（读不到版本、版本归属 / 条目名不符、定义非法、槽位未声明来源、
// 主密钥不可用、存储读取失败、密文归属不符）仍**失败关闭**：那是配置或实例完整性问题，
// 跳过只会让坏数据伪装成"某条没配好"。
// publishedMCPServers resolves published MCP versions into supervisor inputs, in
// configuration order followed by pointer-only servers. A server whose own credential is
// currently unavailable is skipped (startup isolation, E3); every other failure still
// aborts the assembly.
func publishedMCPServers(ctx context.Context, store *storage.ResourceStore, fileOrder []string,
	credentials mcpCredentialResolver, now func() time.Time, logger *slog.Logger,
) ([]core.PublishedMCPServer, error) {
	if credentials == nil {
		return nil, errors.New("解析已发布 MCP server: 本次装配没有 MCP 凭据面——来源无法按声明解析")
	}
	// 整趟补记共用一个启动操作号：它记录的是"实例启动时看到的事实"，不是一次管理写。
	// One startup operation id covers the whole backfill pass.
	backfillOperation := core.NewOperationID(now())
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
		// 来源补记（v5 → v6 的事实记录）：文件能表达的只有 `${VAR}`，所以由基线播种 /
		// 升级而来的槽位在第一次启动时按事实记成 `env`；已有声明的槽位不动。这是**记录**
		// 而不是回落——之后改成 managed 再撤销，仍然不会退回 env。
		// The file can only express ${VAR}, so file-seeded and upgraded slots get their
		// factual env source recorded once (idempotent); declared slots are untouched.
		if _, err := credentials.BackfillEnvSources(ctx, pointer.ResourceID, pointer.ResourceUID,
			cfg, backfillOperation, now()); err != nil {
			return nil, err
		}
		// 占位解析发生在**任何连接之前**（技术方案 §3.7）：只按声明的来源取值，
		// `managed` 失败即失败、不回落 env；错误里只有 server / 槽位 / 变量名，没有值。
		// Resolution happens before any connection and follows the declared source only.
		values, err := credentials.Resolve(ctx, pointer.ResourceID, pointer.ResourceUID, cfg)
		if err != nil {
			// 启动隔离只放行"该 server 自己的凭据取不到值"这一类；其余仍失败关闭。
			// 跳过 = 不建连接、不注册工具：不用占位符字面量起进程，也不把缺值带进运行期。
			// Only this server's own missing credential is skipped; no process is started
			// with literal placeholders, and every other failure still aborts the start.
			if !mcpCredentialUnavailable(err) {
				return nil, err
			}
			if logger != nil {
				logger.Warn("MCP server 凭据不可用，本次启动跳过该 server（其余照常；页面显示未就绪）",
					"server", pointer.ResourceID, "error", err)
			}
			continue
		}
		resolved := ApplyResolvedMCP(cfg, values)
		ZeroMCPCredentialValues(values)
		servers = append(servers, core.PublishedMCPServer{VersionID: version.ID, Config: resolved})
	}
	return servers, nil
}

// mcpCredentialResolver 是装配期需要的两个凭据动作：按声明来源解析，与补记来源事实。
// `*MCPCredentialService` 是唯一实现。
// mcpCredentialResolver is the assembly-time credential face; the service is its only
// implementation.
type mcpCredentialResolver interface {
	Resolve(ctx context.Context, serverName string, serverUID core.ResourceUID,
		cfg core.MCPServerConfig) (map[string]string, error)
	BackfillEnvSources(ctx context.Context, serverName string, serverUID core.ResourceUID,
		cfg core.MCPServerConfig, operationID string, at time.Time) (int, error)
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
