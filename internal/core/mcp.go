package core

// 本文件是 MCP server 配置的契约面：mcp_servers.yaml 的 servers[] 条目的解析结果。
// 纯声明——零可执行语句、零内部包依赖（core 只依赖标准库）；解析与校验进 internal/workspace。
// This file is the contract surface for MCP server configuration: the parsed shape of
// one mcp_servers.yaml servers[] entry. Declarations only; parsing and validation
// live in internal/workspace, and the contract layer stays on the standard library.

// MCPServerConfig 是一条 MCP server 配置。
// 同一类型承载**两个阶段**：`workspace.ParseMCPServers` 产出时 `Env` 的值是 `${VAR}`
// 占位形态；经 `app.resolveMCPEnv` 之后才是最终值——只有装配层允许把占位换成值（已决 #2）。
// MCPServerConfig is one MCP server entry. The same type carries two phases: when
// produced by ParseMCPServers the Env values are ${VAR} placeholders; only the
// assembly layer (resolveMCPEnv) replaces them with the resolved values.
type MCPServerConfig struct {
	Name    string            // name（必填；server 的唯一标识，也是日志与错误里点名的对象）
	Command string            // command（必填；按空白切分、无 shell 引号语义）
	Env     map[string]string // env（键 = 子进程变量名；值两阶段：占位 → 终值）
}
