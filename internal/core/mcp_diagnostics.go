package core

// 本文件是 MCP server 只读诊断的领域类型（颗粒度文档 §3.1，契约 D-02）：回答"当前这条
// 已发布配置按真实启动方式能否定位、环境变量是否就绪、是否待重启、运行时依赖是否就位、
// 被哪些 Agent 引用、有哪些已知边界"。诊断不启动进程、不连接、不写操作行，也不预测测试
// 结果——诊断与测试是两条独立轴，由页面分开展示。零可执行语句、零内部包依赖。
// This file declares the read-only MCP diagnosis types (module card §3.1, contract
// D-02). Diagnosis never starts a process, never connects, never writes an operation
// row, and never predicts a test result. Declarations only.

import "time"

// MCPServerDiagnosis 是一次诊断的完整回答。Command 只对 stdio 有值（http 为 nil），
// Env 只回"键是否就绪"的布尔，不回值；Warnings 只回诊断固定 code 集合，不返回自由
// 文本。PendingRestart 只由发布指针与已应用版本决定，与连接状态无关。Dependency 只对
// 有运行时组件登记的条目有值（其余为 nil → 输出 `null`，**不是**"缺失"）；References
// 是恒在的引用轴，与 `/resources/{kind}/{id}/references` 同源。
// MCPServerDiagnosis is one diagnosis answer. Command is nil for http; Env only
// reports booleans; warnings carry fixed codes only; Dependency is null for
// unregistered components, never "missing".
type MCPServerDiagnosis struct {
	Name               string                  `json:"name"`
	Transport          string                  `json:"transport"`
	PublishedVersionID int64                   `json:"published_version_id"`
	AppliedVersionID   int64                   `json:"applied_version_id"`
	PendingRestart     bool                    `json:"pending_restart"`
	Command            *MCPCommandDiagnosis    `json:"command"`
	Env                []MCPEnvDiagnosis       `json:"env"`
	Dependency         *MCPDependencyDiagnosis `json:"dependency"`
	References         MCPReferencesDiagnosis  `json:"references"`
	Warnings           []string                `json:"warnings"`
	CheckedAt          time.Time               `json:"checked_at"`
}

// MCPDependencyDiagnosis 是运行时依赖轴（设计 §2.3.4）：条目依赖的组件是否就位、
// 版本是否符合清单。只由运行时目录与版本清单判定——不启动进程取版本、不安装、不
// 访问网络。ExpectedVersion 为空串表示没有登记期望版本；FoundVersion 在 missing 时
// 为空串、version_mismatch 时给实测（目录）版本。
// MCPDependencyDiagnosis is the runtime dependency axis, judged from the runtime
// directory and version manifest only; no process is started to read a version.
type MCPDependencyDiagnosis struct {
	Component       string             `json:"component"`
	State           MCPDependencyState `json:"state"`
	ExpectedVersion string             `json:"expected_version"`
	FoundVersion    string             `json:"found_version"`
}

// MCPReferencesDiagnosis 是引用该 server 的 Agent 名单：升序、含归档 Agent（点名为
// `archive:<id>`），与资源引用视图同一份来源——诊断不复算第二套引用规则。
// MCPReferencesDiagnosis mirrors the resource references view: sorted, archived agents
// included, never a second implementation of the reference rule.
type MCPReferencesDiagnosis struct {
	Count  int      `json:"count"`
	Agents []string `json:"agents"`
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
