package cli

import (
	"log/slog"
	"os"
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

	// RunService 与 SessionLister 是运行面端口（见 ports.go）：`chat` 走前者跑一次运行、
	// 经后者读回本轮新增的消息，`session list` 只用后者。
	// The run-face ports: chat runs through RunService and reads history through
	// SessionLister; session list consumes SessionLister only.
	RunService    RunService
	SessionLister SessionLister

	// Tools 是工具注册池端口（见 ports.go）：`tool list` 经它取「注册了什么」这一份数据，
	// 清单的顺序由实现侧给出、命令面零重排。只有重路径装配才构建过注册池，因此该端口
	// **只在重路径上非空**——命令面拿到 nil 即报缺件，不自造第二份清单。
	// The tool-registry port: only the heavy path ever builds the pool, so a nil here
	// means a wiring gap and is reported as such.
	Tools ToolLister

	// In / Out 是命令面应当读写的**介质**（真控制台或管道——分流由渠道层按句柄判定，
	// 不看环境变量）；Signals 是中断来源。三者为空即用进程标准流与渠道自建的中断源。
	// 注入是为了可判卷：真控制台与真 Ctrl-C 只能在人工项里按，自动化这一侧拿得住的
	// 只有一对管道和一条信号通道（颗粒度文档 §6.1 第 4 项）。
	// In/Out/Signals are the media and the interrupt source; nil falls back to the
	// process standard streams and a channel-owned signal source. Injecting them is
	// what makes the interactive path testable at all.
	In      *os.File
	Out     *os.File
	Signals <-chan os.Signal
}
