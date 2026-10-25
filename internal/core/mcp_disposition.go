package core

// 本文件是 MCP 工具来源的领域类型（颗粒度文档 §3.1，契约 D-06）：回答"这条工具最终是谁
// 注册成功的、被跳过的为什么被跳过"。它只投影装配层的注册结果，不参与执行，也不改变
// 注册池的重名语义——`McpServerView.tools` 仍是"该 server 提供的工具名"。
// This file declares the MCP tool-disposition types (module card §3.1, contract D-06).
// It only projects the assembly-time registration outcome; the registry's duplicate
// semantics stay untouched. Declarations only.

// MCPToolDisposition 是一个 server 的注册投影：Offered 是它对注册池提出的工具数，
// Accepted 是实际注册成功的数，Skipped 逐条给出被跳过者与赢家来源。恒有
// Offered == Accepted + len(Skipped)。
// MCPToolDisposition projects one server's registration outcome; it always holds
// Offered == Accepted + len(Skipped).
type MCPToolDisposition struct {
	Offered  int           `json:"offered"`
	Accepted int           `json:"accepted"`
	Skipped  []MCPToolSkip `json:"skipped"`
}

// MCPToolSkip 是一条被跳过的工具。WinnerSource 是 `builtin` 或具体 MCP server 名
// （无法确定时为 `unknown`，不得猜）；Reason 只允许 duplicate_name / invalid_tool。
// MCPToolSkip names the winning source and one fixed skip reason.
type MCPToolSkip struct {
	Name         string `json:"name"`
	WinnerSource string `json:"winner_source"`
	Reason       string `json:"reason"`
}
