package core

// 本文件是 MCP server 配置的契约面：mcp_servers.yaml 的 servers[] 条目的解析结果。
// 纯声明——零可执行语句、零内部包依赖（core 只依赖标准库）；解析与校验进 internal/workspace。
// This file is the contract surface for MCP server configuration: the parsed shape of
// one mcp_servers.yaml servers[] entry. Declarations only; parsing and validation
// live in internal/workspace, and the contract layer stays on the standard library.

// MCPServerConfig 是一条 MCP server 配置。
// 同一类型承载**两个阶段**：`workspace.ParseMCPServers` 产出时 `Env` 的值是 `${VAR}`
// 占位形态；经 `app.resolveMCPEnv` 之后才是最终值——只有装配层允许把占位换成值（已决 #2）。
// US-12 起 `Transport` / `Args` / `URL` 参与严格候选与按传输分支的连接：空 Transport
// 只对既有文件按 stdio 读取，管理面写入必须显式给 `stdio` 或 `http`。
// MCPServerConfig is one MCP server entry. The same type carries two phases: when
// produced by ParseMCPServers the Env values are ${VAR} placeholders; only the
// assembly layer (resolveMCPEnv) replaces them with the resolved values. Since
// US-12 the transport, args and URL take part in strict validation and in the
// transport-branched connection path; an empty transport means legacy stdio.
type MCPServerConfig struct {
	Name      string            // name（必填；server 的唯一标识，也是日志与错误里点名的对象）
	Transport string            // transport（`stdio` / `http`；空 = 既有文件按 stdio 读取）
	Command   string            // stdio：单个可执行名（不再按空白切分）
	Args      []string          // stdio：参数分开保存（与 command 分离）
	URL       string            // http：无 userinfo 的绝对 URL
	Env       map[string]string // env（键 = 子进程变量名；值两阶段：占位 → 终值）
}

// PublishedMCPServer 把解析后的配置与明确的已发布版本绑定；supervisor 只消费该
// 结构，不再自行拆分 command。
// PublishedMCPServer binds a parsed configuration to its published version; the
// supervisor consumes this shape instead of splitting command itself.
type PublishedMCPServer struct {
	VersionID int64
	Config    MCPServerConfig
}
