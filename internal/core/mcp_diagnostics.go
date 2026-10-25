package core

// 本文件是 MCP server 只读诊断的领域类型（颗粒度文档 §3.1，契约 D-02）：回答"当前这条
// 已发布配置按真实启动方式能否定位、环境变量是否就绪、是否待重启、有哪些已知边界"。
// 诊断不启动进程、不连接、不写操作行，也不预测测试结果——诊断与测试是两条独立轴，
// 由页面分开展示。零可执行语句、零内部包依赖。
// This file declares the read-only MCP diagnosis types (module card §3.1, contract
// D-02). Diagnosis never starts a process, never connects, never writes an operation
// row, and never predicts a test result. Declarations only.

import "time"

// MCPServerDiagnosis 是一次诊断的完整回答。Command 只对 stdio 有值（http 为 nil），
// Env 只回"键是否就绪"的布尔，不回值；Warnings 只回诊断固定 code 集合，不返回自由
// 文本。PendingRestart 只由发布指针与已应用版本决定，与连接状态无关。
// MCPServerDiagnosis is one diagnosis answer. Command is nil for http; Env only
// reports booleans; warnings carry fixed codes only.
type MCPServerDiagnosis struct {
	Name               string               `json:"name"`
	Transport          string               `json:"transport"`
	PublishedVersionID int64                `json:"published_version_id"`
	AppliedVersionID   int64                `json:"applied_version_id"`
	PendingRestart     bool                 `json:"pending_restart"`
	Command            *MCPCommandDiagnosis `json:"command"`
	Env                []MCPEnvDiagnosis    `json:"env"`
	Warnings           []string             `json:"warnings"`
	CheckedAt          time.Time            `json:"checked_at"`
}

// MCPCommandDiagnosis 是"命令能否定位"的三态结论。Path 只在 State=located 时非空；
// State=unconfirmed 表示按现行启动构造无法确定（如 `.cmd` 包装、含分隔符的相对入口），
// **不是**失败——诊断不确定与时测试仍然真实尝试，分类由真实错误给出。
// MCPCommandDiagnosis is the three-state lookup verdict; "unconfirmed" is not a
// failure and must not predetermine the test outcome.
type MCPCommandDiagnosis struct {
	State  string `json:"state"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// MCPEnvDiagnosis 只回布尔：变量存在且非空为 true，未设置或空串为 false。
// MCPEnvDiagnosis reports presence only, never the value.
type MCPEnvDiagnosis struct {
	Key string `json:"key"`
	Set bool   `json:"set"`
}
