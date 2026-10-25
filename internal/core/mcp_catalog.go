package core

// 本文件是只读 MCP 目录的领域类型（颗粒度文档 §3.1，契约 D-01）：目录数据随二进制
// 嵌入，启动装配时解析一次成不可变快照，请求期只做只读投影——不读工作区、不启动进程、
// 不写配置。零可执行语句、零内部包依赖（core 只依赖标准库）。
// This file declares the read-only MCP catalog types (module card §3.1, contract D-01).
// The catalog is embedded in the binary and parsed once at assembly time into an
// immutable snapshot; requests only project it. Declarations only.

// MCPCatalog 是整份目录快照。Version 是目录 schema 版本（当前为 1），Items 按 `id`
// 升序——排序由解析层保证，视图层不再排序。
// MCPCatalog is the whole catalog snapshot; items are already sorted by id.
type MCPCatalog struct {
	Version int               `json:"version"`
	Items   []MCPCatalogEntry `json:"items"`
}

// MCPCatalogEntry 是一条可复制的 server 配方。它是"怎么配"的建议，不是运行配置：
// Args 里的 `<...>` 只是编辑标记（本 US 不解析 args 中的 `${VAR}`），Command / URL
// 按 Transport 二选一，另一侧留空串。Verified 四件套区分"已实测"与"待实测"，
// Verified=false 时 VerificationNote 必须写清待实测项，VerifiedAt 为空、平台为空数组。
// MCPCatalogEntry is one copyable server recipe. It is advice, not live
// configuration: `<...>` in args are edit markers only, command/url are exclusive per
// transport, and the verified quartet keeps "measured" apart from "not yet measured".
type MCPCatalogEntry struct {
	ID                string          `json:"id"`
	DisplayName       string          `json:"display_name"`
	Summary           string          `json:"summary"`
	Category          string          `json:"category"`
	Transport         string          `json:"transport"`
	Command           string          `json:"command"`
	Args              []string        `json:"args"`
	URL               string          `json:"url"`
	Env               []MCPCatalogEnv `json:"env"`
	Requires          []string        `json:"requires"`
	SourceURL         string          `json:"source_url"`
	Verified          bool            `json:"verified"`
	VerificationNote  string          `json:"verification_note"`
	VerifiedAt        string          `json:"verified_at"`
	VerifiedPlatforms []string        `json:"verified_platforms"`
	Warnings          []string        `json:"warnings"`
}

// MCPCatalogEnv 是一条 env 占位声明：Placeholder 恰为 `${Key}`，页面据此预填表单；
// 管理台保存前由 env 闸门按真实进程环境核对，缺失即 fail-closed。只声明键名与用途，
// **不携带任何值**。
// MCPCatalogEnv declares one env placeholder; it never carries a value.
type MCPCatalogEnv struct {
	Key         string `json:"key"`
	Placeholder string `json:"placeholder"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}
