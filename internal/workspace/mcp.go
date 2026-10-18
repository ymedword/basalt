package workspace

// 本文件是 mcp_servers.yaml 的解析与形态校验（US-7）：顶层映射 + `servers:` 列表，
// 条目字段 name / transport / command / env；列表顺序原样保留（= 连接序 = 注册序）。
// 本层**不读环境变量**——`env` 值只校验是否为精确 `${VAR}` 占位，解析（占位 → 终值）
// 归装配层 `app.resolveMCPEnv`（已决 #2；技术方案 §3.7）。
// This file parses and validates mcp_servers.yaml. It never reads the environment:
// env values are only checked for the exact ${VAR} placeholder form; resolution
// belongs to the assembly layer.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/core"
)

// ParseMCPServers 读取工作区根部的 mcp_servers.yaml 并产出 server 配置列表。
// 文件缺失 / 空文件 / 纯注释 / `{}` / 无 servers 键 / `servers: null` → 零 server、
// 零错误（整段跳过由装配层负责，FR-002）；其余形态问题一律 fail-closed 拒载。
// ParseMCPServers reads the workspace-root mcp_servers.yaml. The six zero-server
// shapes yield an empty slice and a nil error; every other shape failure is fatal.
func ParseMCPServers(root string) ([]core.MCPServerConfig, error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() { _ = base.Close() }() // 根句柄只读；关闭失败无后续动作可做
	return parseMCPServers(base)
}

// parseMCPServers 是解析主体（持有根句柄，读取在根上被挡软连接越界）。
// parseMCPServers is the parsing body working on the workspace root handle.
func parseMCPServers(base *os.Root) ([]core.MCPServerConfig, error) {
	raw, err := base.ReadFile(mcpServersFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // 缺失 = 零 server
		}
		return nil, fmt.Errorf("读取 %s: %w", mcpServersFile, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil // 空文件
	}

	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("解析 %s: %s", mcpServersFile, frontmatterErrorSummary(err))
	}
	if tree == nil {
		return nil, nil // `null` 文档 / 纯注释（注释不产生节点）
	}
	mapping, ok := tree.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 的顶层必须是映射（servers: 列表），实际是 %T", mcpServersFile, tree)
	}
	rawList, present := mapping["servers"]
	if !present || rawList == nil {
		return nil, nil // 兼容 US-1 模板的 `{}` 与省略 servers 的文件
	}
	items, ok := rawList.([]any)
	if !ok {
		return nil, fmt.Errorf("%s 的 servers 必须是列表，实际是 %T", mcpServersFile, rawList)
	}

	out := make([]core.MCPServerConfig, 0, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s 的 servers[%d] 必须是映射，实际是 %T", mcpServersFile, i, item)
		}
		name := stringField(entry, "name")
		if name == "" {
			return nil, fmt.Errorf("%s 的 servers[%d] 缺少 name", mcpServersFile, i)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s 的 server 名重复: %s", mcpServersFile, name)
		}
		seen[name] = true
		// transport 字段一旦出现必须先过类型检查：非字符串经字符串强转会变成空串、
		// 再按缺省 stdio 放行——等于让非法配置悄悄获得执行含义。`null` 视同缺省（与 env 同口径）。
		// A present transport must be a string before its value is examined; coercing a
		// non-string to "" would silently grant the stdio default execution meaning.
		transport := ""
		if rawTransport, hasTransport := entry["transport"]; hasTransport && rawTransport != nil {
			typedTransport, ok := rawTransport.(string)
			if !ok {
				return nil, fmt.Errorf("%s 的 server %s 的 transport 必须是字符串，实际是 %T", mcpServersFile, name, rawTransport)
			}
			transport = typedTransport
		}
		if transport != "" && transport != "stdio" {
			return nil, fmt.Errorf("%s 的 server %s 的 transport 不受支持（%s）——v1 只支持 stdio", mcpServersFile, name, transport)
		}
		command := stringField(entry, "command")
		if strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("%s 的 server %s 缺少 command", mcpServersFile, name)
		}
		env := map[string]string{}
		if value, hasEnv := entry["env"]; hasEnv && value != nil {
			envMap, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s 的 server %s 的 env 必须是映射", mcpServersFile, name)
			}
			for key, rawValue := range envMap {
				placeholder, ok := rawValue.(string)
				if _, isRef := core.EnvPlaceholderName(placeholder); !ok || !isRef {
					// 错误只点名字段，**永不回显**配置值——值可能正是凭证（FR-004 / R1a）
					return nil, fmt.Errorf("%s 的 server %s 的 env.%s 必须是 ${VAR} 占位（明文凭证不得入库）", mcpServersFile, name, key)
				}
				env[key] = placeholder
			}
		}
		out = append(out, core.MCPServerConfig{Name: name, Command: command, Env: env})
	}
	return out, nil
}

// mcpServerNames 取配置列表的 server 名序列（保持配置序——装载器规则 4/6 按声明序
// 报首个失败名，顺序是判据的一部分，**不得**排序）。
// mcpServerNames returns the server names in configuration order; sorting would
// change the first-failure reporting of loader rules 4 and 6.
func mcpServerNames(specs []core.MCPServerConfig) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

// stringField 取映射里的字符串字段；缺失或非字符串一律按空串处理（空串在调用点
// 各自对应一条点名错误）。
// stringField returns a string field or "" when absent or of another type.
func stringField(entry map[string]any, key string) string {
	value, _ := entry[key].(string)
	return value
}
