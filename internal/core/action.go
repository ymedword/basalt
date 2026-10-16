package core

// 本文件是授权面与执行面的输入 / 产出声明（技术方案 §6.5 逐字）。
// 授权与执行分离（宪法 VI）：`Policy` 看 Action、`CommandRunner` 看 Command——
// 一个答"允许吗"，一个答"怎么做"，两个接口各自演进。
// This file declares the inputs and outputs of the authorization and execution faces
// (Technical Solution §6.5). Policy reads Action; CommandRunner reads Command.

// Action 是 Policy 的输入：授权只看它——**谁、要碰什么**，不看"打算怎么做"。
// 故**没有** argv 字段：命令行 / URL 的形态校验属于执行侧（LocalRunner）。
// Action is the input of Policy: who wants to touch what. Execution details carry no
// authority, which is why there is no argv field here.
type Action struct {
	Type   ActionType // 判据维度
	Target string     // 判据主体：文件路径 / 可执行名 / URL（按 Type 解释）
	Agent  string     // 发起动作的 Agent 名；空 = 无 Agent 上下文（CLI 顶层调用，不是异常）
}

// ActionType 是四类动作的字面量集合（**动作类型不含命令行参数**）。
// ActionType is the literal set of the four action kinds.
type ActionType string

const (
	// ActionFileRead 是文件读取判定。
	ActionFileRead ActionType = "FILE_READ"
	// ActionFileWrite 是文件写入判定（保护规则先行：AGENT.md / skills / scripts / memory 拒写）。
	ActionFileWrite ActionType = "FILE_WRITE"
	// ActionShellCommand 是命令执行判定——Target 是 executable 的**整串**，不看 argv。
	ActionShellCommand ActionType = "SHELL_COMMAND"
	// ActionHTTPRequest 是出网判定——逐跳校验由调用方在每一跳发送前再次 Check（US-5 驱动）。
	ActionHTTPRequest ActionType = "HTTP_REQUEST"
)

// Command 是一次受控子进程执行请求，经 CommandRunner.Run 消费。
// 可执行名与参数**分开显式承载**：授权面按 Executable 整串逐字比对，argv 形态校验
// （`-c` 拒、脚本真实路径越界拒）属执行侧——两个面各自只碰自己该碰的字段。
// Command is one controlled subprocess request. The executable and its arguments are
// carried separately: authorization compares the executable string verbatim while the
// runner validates argv shape.
type Command struct {
	Executable     string   // 与 shell.allowed_commands / interpreters 比对的对象（多词串必拒）
	Args           []string // argv 原样传递，不经任何 shell 解释
	Dir            string   // 工作目录：有 Agent 上下文 = 该 Agent 的目录；空 = 进程默认 CWD（不引入全局 chdir）
	Env            []string // 工具显式声明的附加变量（KEY=VALUE）；透传白名单在 Runner 侧统一执行
	MaxOutputBytes int      // 本次执行的读取侧上限；<=0 = 用 runner 装配值；只许比装配值更短
}

// CommandResult 是一次受控执行的产出：stdout 与 stderr **合并**为单串（顺序即进程写出顺序）。
// `Truncated` 只表达"达到生效输出上限"——**不承诺**原始长度（读取中限流意味着从未读完整输出）。
// CommandResult is the output of one controlled execution: stdout and stderr merged into
// one string. Truncated says the effective cap was hit; the original length is unknown.
type CommandResult struct {
	Output    string
	ExitCode  int
	Truncated bool
}
