package cli

import (
	"log/slog"
)

// Deps 是命令面的显式类型化装配参数——工作目录、目标 schema 版本与日志器一律
// 经参数传递，不得塞进 context、不得用包级变量顶替（宪法第 7 条）。
// Deps carries the explicitly typed wiring parameters for the command surface:
// the working directory, the target schema version and the logger. None of them
// travel through the context or through package-level state.
//
// WorkDir 由装配层注入而非各命令自行 os.Getwd——测试因此免于 chdir，
// status 的三节也共享同一路径（一份报告只检查一个工作区）。
// WorkDir is injected by the assembly layer instead of each command calling
// os.Getwd, which keeps tests free of chdir and pins every status section to the
// same workspace.
type Deps struct {
	// WorkDir 是运行命令的进程工作目录；工作区 = <WorkDir>/.basalt。
	// WorkDir is the process working directory; the workspace is <WorkDir>/.basalt.
	WorkDir string

	// SupportedSchemaVersion 是二进制支持的目标 schema 版本；生产装配传
	// storage.SupportedSchemaVersion，测试传合成值以覆盖单版本期不可达的分支。
	// SupportedSchemaVersion is the schema version this binary targets.
	SupportedSchemaVersion int

	// Logger 是结构化日志器；测试注入带缓冲 handler 的实例以断言凭证不泄露与
	// 失败路径留痕。
	// Logger is the structured logger; tests inject a buffered handler to assert
	// that credentials never leak and that failure paths leave a trace.
	Logger *slog.Logger

	// Store 与 Workspace 是命令面的两个端口（接口定义在消费方，见 ports.go）：
	// 存储器与工作区的**实现**由装配层 internal/app 注入，命令面只认接口——
	// cli 因此不 import storage / workspace，依赖方向是 app → cli → core。
	// The two ports the commands consume; internal/app injects the adapters, so
	// cli never imports the implementation packages.
	Store     Store
	Workspace Workspace

	// Agents 是 Agent 目录端口：list / show 只读（轻路径——不取实例锁、不触数据库），
	// create / delete 是**写模式**（实现侧先取实例锁：被活实例持有时拒绝启动且零写入）。
	// Agents is the agent-directory port: list / show stay read-only, while create /
	// delete write through the instance lock taken inside the implementation.
	Agents Agents
}
