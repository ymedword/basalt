package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是命令面内置工具 `shell`（技术方案 §4.2、契约 §5.2）：把模型给的「可执行名 + 参数
// 数组」交给受控执行器跑一次。两条纪律贯穿全篇：① 授权只经 **core.Policy 接口**判定、
// 执行只经 **core.CommandRunner 接口**（授权与执行分离，宪法 VI）——本文件不出现任何具体
// 策略 / 执行实现，argv 形态与脚本范围校验归执行侧；② executable 与 arguments **分开传**，
// 进程 argv 直执、不经任何 shell 解释——工具**不得**按空白拆分 executable（拆开会把
// `bash -c "curl …"` 变成绕过命令档的标准姿势），也不得把参数拼进可执行名。
// The builtin shell tool: authorization and execution travel through the two core
// interfaces only, the executable is compared verbatim, and argv is never re-interpreted.
// No process-execution package appears in this file's import list — spawning belongs to
// the runner (the module-level grep for it stays at zero hits).

// shellToolName 是工具名——与 AGENT.md 的 `tools:` 声明逐字比对（调用名同字）。
// The tool name, matched verbatim against the tools declaration of AGENT.md.
const shellToolName = "shell"

// shellInputSchema 是 shell 的入参 Schema（内联常量，测试逐字钉住）：required 数组是执行侧
// 必填校验的**唯一依据**（core 按它判 invalid_input 并仍写审计行），工具**不**在解码点重复
// 判必填——分层不重叠（契约 §5.2 的参数表）。
// Inline JSON Schema; core reads the required array for the entry-level check.
const shellInputSchema = `{"type":"object","properties":{"executable":{"type":"string","description":"可执行文件名：与 shell.allowed_commands 整串逐字比对（不得把参数拼进来，多词串必拒）"},"arguments":{"type":"array","items":{"type":"string"},"description":"参数数组，逐项原样传递（不经任何 shell 解释）"},"timeout_seconds":{"type":"integer","description":"本次执行的超时秒数：只能比执行侧给的上限更短，非正值按上限算"}},"required":["executable"]}`

// ShellTool 是 `shell`：受控命令执行。判定（能不能跑）与执行（怎么跑）分属两个接口，
// 本类型只做编排：解码 → 判定 → 收窄超时 → 组装命令 → 执行 → 呈现结果。
// ShellTool orchestrates one guarded command run; the two faces stay in their interfaces.
type ShellTool struct {
	policy core.Policy
	runner core.CommandRunner
}

// 编译期断言：调用方只依赖 `core.Tool`，装配处换实现不触及本类型（宪法 VI）。
// Compile-time assertion that callers depend on the interface.
var _ core.Tool = (*ShellTool)(nil)

// NewShellTool 装配 `shell`：policy 做授权判定，runner 做受控执行——**只引用两个接口**。
// NewShellTool wires the tool against the two interfaces, never a concrete implementation.
func NewShellTool(policy core.Policy, runner core.CommandRunner) *ShellTool {
	return &ShellTool{policy: policy, runner: runner}
}

// Name 是工具名。
// Name is the registered tool name.
func (t *ShellTool) Name() string { return shellToolName }

// Description 是给模型看的能力说明（沙箱放行范围由配置决定，此处不承诺具体清单）。
// Description is the model-facing summary.
func (t *ShellTool) Description() string {
	return "执行白名单内的可执行文件（可执行名与参数分开传、argv 直执不经任何 shell；工作目录为当前 Agent 目录；可缩短超时，超时杀整棵进程树）"
}

// InputSchema 返回内联 Schema（测试逐字钉住）。
// InputSchema returns the inline schema, pinned verbatim by tests.
func (t *ShellTool) InputSchema() json.RawMessage { return json.RawMessage(shellInputSchema) }

// Execute 跑一次受控命令（流程 A）：解码 → 判定 → 收窄超时 → 组装 Command → runner.Run →
// 呈现结果。非零退出码**不是** error——它是子进程的正常产出形态，退出码与合流输出一并进
// 结果首行与正文（把非零码判成失败会让 grep 类"没匹配到"全变失败）。
// Execute runs one guarded command; a non-zero exit code is a result, not a failure.
func (t *ShellTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	in, err := decodeShellToolInput(input)
	if err != nil {
		return core.Result{}, err
	}
	// 授权面只看「谁、要碰什么」：Target 是 executable 的**整串**（Action 没有 argv 字段），
	// 拒绝文案（四段式）原样上抛——执行器分类 sandbox_denied 并回填给模型，Run 照常继续。
	err = t.policy.Check(ctx, core.Action{
		Type:   core.ActionShellCommand,
		Target: in.Executable,
		Agent:  env.AgentName,
	})
	if err != nil {
		return core.Result{}, err
	}
	narrowed, cancel := narrowTimeout(ctx, env.Timeout, in.TimeoutSeconds)
	defer cancel()
	res, err := t.runner.Run(narrowed, t.command(env, in))
	if err != nil {
		// 执行侧的错误原样上抛：启动失败 / 解释器形态 / 脚本越界 / 取消与截止的**身份**
		// 都由那一边定，分类（sandbox_denied / runtime_error / timeout）归执行器。
		return core.Result{}, err
	}
	return core.Result{Content: shellContent(res), Retryable: false}, nil
}

// shellToolInput 是 shell 的入参形态：字段**是否出现**由 Schema 的 required 判（执行侧据此
// 回 invalid_input 并写审计行），本结构体只承载解出的值。
// The decoded input shape; presence is judged by the schema, not here.
type shellToolInput struct {
	Executable     string   `json:"executable"`
	Arguments      []string `json:"arguments"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// decodeShellToolInput 解出入参：字段**类型**不符是调用方的入参问题（给错类型不能落成
// "本仓缺陷"），失败载体 `*core.InvalidInputError` ⇒ 分类 `invalid_input`（判据只认类型）。
// 必填缺失**不在此判**——那是执行器入口更早的一道（checkToolInput）。
// A decode failure is an invalid-input failure, never a defect of this repository.
func decodeShellToolInput(input json.RawMessage) (shellToolInput, error) {
	var in shellToolInput
	if err := json.Unmarshal(input, &in); err != nil {
		return shellToolInput{}, &core.InvalidInputError{
			Tool:   shellToolName,
			Reason: fmt.Sprintf("解析 %s 入参：%s", shellToolName, err.Error()),
		}
	}
	return in, nil
}

// command 把入参组装成受控执行请求：argv 逐项原样、环境**不自行声明**（透传白名单整条归
// 执行侧）、工作目录按 Agent 上下文取、逐 Run 读取上限随行走（漏传 = 读取端恒用装配值，
// 配置收紧的预算到不了子进程读取口）。
// command builds the execution request: argv verbatim, no declared environment, the
// per-run read budget riding along on the command.
func (t *ShellTool) command(env core.ToolEnv, in shellToolInput) core.Command {
	return core.Command{
		Executable:     in.Executable,
		Args:           in.Arguments,
		Dir:            toolWorkDir(env.AgentDir), // 空 = 进程默认 CWD（契约 §5.2 的 cwd 约定）
		Env:            nil,                       // 契约无 env 参数：透传白名单整条归执行侧
		MaxOutputBytes: env.MaxOutputBytes,        // 逐 Run 读取上限随行走
	}
}

// toolWorkDir 判定子进程的工作目录（契约 §5.2 的 cwd 约定）：有 Agent 上下文**且目录存在**
// 时用该 Agent 目录，否则留空 = 进程默认 CWD。目录不存在时不起"注定失败"的进程——非法 cwd
// 会让每一次调用都死在启动上。**不得**引入全局 chdir（进程级状态不属于一次调用）。
// The child's working directory is the agent's own directory when it exists; an
// unusable path degrades to the process default instead of failing every spawn.
func toolWorkDir(agentDir string) string {
	if agentDir == "" {
		return ""
	}
	info, err := os.Stat(agentDir)
	if err != nil || !info.IsDir() {
		return ""
	}
	return agentDir
}

// narrowTimeout 把本次执行包进一个**只可能更短**的截止时间（契约 §6「模型只能缩短」）：
// 请求值非正 → 按执行侧上限算（父 ctx 已承载它，工具不另建超时路径）；请求值 ≥ 上限 →
// 不包（执行侧的已更短，再包一层毫无意义）；其余按请求值收窄。返回的 cancel 在不包时是
// no-op——调用方统一 defer cancel()，两条路都有得收。
// narrowTimeout only ever shortens: a non-positive or longer request stays with the
// parent context, whose deadline the executor already established.
func narrowTimeout(ctx context.Context, base time.Duration, requested int) (context.Context, context.CancelFunc) {
	requestedTimeout := time.Duration(requested) * time.Second
	if requestedTimeout <= 0 || requestedTimeout >= base {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, requestedTimeout)
}

// shellContent 是命令结果的呈现形态（契约 §5.2 的返回）：首行「退出码：<N>」，换行后接
// **原样**输出——runner 已按生效上限追加的截断标记随行而过，工具**不**再截断、不再添标记
// （自行截断会出现两个标记）；stdout 与 stderr 的合流已由执行侧完成。
// shellContent renders the exit code line followed by the output verbatim.
func shellContent(res core.CommandResult) string {
	return fmt.Sprintf("退出码：%d\n%s", res.ExitCode, res.Output)
}
