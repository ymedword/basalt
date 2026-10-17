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
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

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
