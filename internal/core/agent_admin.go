package core

// 本文件是 Agent 写面的纯领域类型（颗粒度文档 §3.1.2）：创建请求的规格、受管文件
// 信息与归档记录。零行为——校验在 workspace、编排在 app、投影在 web；时间字段一律
// 用 int64 Unix 毫秒（与表列一致），不得换成 time.Time。
// This file declares the Agent write-surface domain types: the create spec, managed
// file info and the archive record. It carries no behavior; time fields stay int64
// Unix milliseconds to match the table columns.

// AgentSpec 是创建 Agent 的规格：名称、描述与默认模型。
// Name 是目录名（也是注册身份），由 workspace 的名称规则校验；Description 只用于
// 模板正文；Model 为空表示写入可见占位符「请在此填写模型名」，不是"使用默认模型"。
// AgentSpec is the Agent creation spec. An empty Model means the visible placeholder
// is written, not "use the default model".
type AgentSpec struct {
	Name        string
	Description string
	Model       string // 空 = 可见占位符「请在此填写模型名」
}

// AgentFileInfo 是受管文件的一行视图：相对路径与字节数（不携带正文）。
// AgentFileInfo is one managed-file row: relative path and byte size, without content.
type AgentFileInfo struct {
	Path string
	Size int64
}

// AgentArchiveRecord 是 `agent_archives` 表的一行：归档身份、内容哈希、清单与恢复事实。
// ArchiveID = `<agent>-YYYYMMDD-HHMMSS`（UTC），与工作区 `archive/` 目录同名；
// ArchivedAt 是 Unix 毫秒 UTC；RestoredAt 为 nil 表示未恢复，恢复后记目标 Agent 名。
// AgentArchiveRecord is one agent_archives row. ArchiveID equals the archive directory
// name, ArchivedAt is Unix milliseconds UTC, and a nil RestoredAt means not restored.
type AgentArchiveRecord struct {
	ArchiveID         string   // `<agent>-YYYYMMDD-HHMMSS`（UTC）
	AgentName         string   // 归档前的 Agent 名
	AgentUID          AgentUID // 归档前的稳定身份；空 = v3 之前的归档，无法证明身份
	ArchivedAt        int64    // Unix 毫秒 UTC（表列 archived_at INTEGER）
	ContentHash       string   // 归档目录内容清单的规整哈希
	ManifestJSON      string   // 归档目录清单（路径 / 大小 / 文件哈希）
	OperationID       string   // 归档管理操作
	RestoredAt        *int64   // nil = 未恢复
	RestoredAgentName string   // 恢复后的 Agent 名（原名或异名）
}
