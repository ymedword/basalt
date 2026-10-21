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

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
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
}

// newMCPRuntime 复制两份输入后持有——装配期的切片后续即使被改写也不影响这句话柄。
// newMCPRuntime copies both inputs so later assembly-side edits cannot leak in.
func newMCPRuntime(specs []core.MCPServerConfig, conns []tool.MCPConnection) *mcpRuntime {
	return &mcpRuntime{specs: slices.Clone(specs), conns: slices.Clone(conns)}
}

// ConfiguredNames 返回已配置 server 名（文件序）——装载器据此判定"声明了不存在的 server"。
// ConfiguredNames lists the configured servers in file order.
func (m *mcpRuntime) ConfiguredNames() []string {
	names := make([]string, 0, len(m.specs))
	for _, spec := range m.specs {
		names = append(names, spec.Name)
	}
	return names
}

// ConnectedNames 返回实际连上的 server 名（文件序）——非 nil 的"已连接"集是重路径
// "知道连接结果"的判据，装载器据此直接拒载失败 server 的 Agent。
// ConnectedNames lists the servers that actually connected, in file order.
func (m *mcpRuntime) ConnectedNames() []string {
	names := make([]string, 0, len(m.conns))
	for _, conn := range m.conns {
		names = append(names, conn.Server)
	}
	return names
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

// resolveMCPEnv 在装配期把 `${VAR}` 占位换成环境值（技术方案 §3.7：所有占位在装配前统一
// 解析）。**未设置与空值同档拒绝**——空凭证送给 server 只会换来一个 401，排障方向会被引到
// server 身上；错误点名 server / 键 / 变量名，**不回显值**（值可能正是凭证）。lookup 注入：
// 生产传 `os.LookupEnv`，用例传替身（测试缝纪律：不为测试新增导出 API）。
// resolveMCPEnv resolves ${VAR} placeholders at assembly time. Missing and empty values
// are refused alike; the error names the server, key and variable but never the value.
func resolveMCPEnv(specs []core.MCPServerConfig, lookup func(string) (string, bool)) ([]core.MCPServerConfig, error) {
	for i := range specs {
		resolved := make(map[string]string, len(specs[i].Env))
		for key, ref := range specs[i].Env {
			varName := strings.TrimSuffix(strings.TrimPrefix(ref, "${"), "}")
			value, ok := lookup(varName)
			if !ok || value == "" {
				return nil, fmt.Errorf("mcp_servers.yaml 的 server %s 的 env.%s 引用的 %s 未设置或为空",
					specs[i].Name, key, varName)
			}
			resolved[key] = value // 值不进错误、不进日志
		}
		specs[i].Env = resolved
	}
	return specs, nil
}
