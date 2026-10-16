package tool

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是命令面工具（`shell`）的假 runner 主面（颗粒度文档 §3.2 的 shelltools_test.go，
// 17 条）：R5（`timeout_seconds` 只缩短四态）与 R6（非零退出码不是错误）在 §4.3 写出代码、
// 原样落地。判卷纪律（§4.1）：① 逐层用例两侧记账——假 runner 记「调了几次、收到什么」，
// 假策略记「判了哪些目标」，只断言最终结果会把「先执行后判定」判成通过；② 共享记账带锁
// （`-race` 下也是判卷依据）；③ 链路用例经**真实** `core.ToolExecutor` 与真实结果包装，
// 假审计仓储只做 I/O 边界（再造就等于把被测对象换成夹具）。受控子进程的**真实**面
//（起进程、整树终止、读取中限流）归 runner_test.go——本文件零进程、零外部命令。
//
// Fake-runner home of the shell tool: R5/R6 land verbatim, both sides of every judgement
// are accounted for, and chain cases run through the real ToolExecutor.

// —— 夹具 ——

// fakeRunner 是进程内假 runner（实现 `core.CommandRunner`）：带锁记录收到的 ctx 与命令，
// 产出由用例预置（零值 = 退出码 0、空输出、无错误）。R5 判的是**收到的截止时刻**、
// R6 判的是**收到的整条 Command**——两处都不比对文本。
// The in-process fake runner: the received context and command are recorded under a lock.
type fakeRunner struct {
	mu     sync.Mutex
	ctx    context.Context
	cmd    core.Command
	calls  int
	result core.CommandResult
	err    error
}

// Run 记账后交回预置产出；非零退出码与错误都由用例显式构造，夹具不自行判断。
// Run records the call and hands back whatever the case prepared.
func (r *fakeRunner) Run(ctx context.Context, cmd core.Command) (core.CommandResult, error) {
	r.mu.Lock()
	r.ctx = ctx
	r.cmd = cmd
	r.calls++
	r.mu.Unlock()
	return r.result, r.err
}

// deadline 返回 runner 收到的 ctx 的截止时刻（生效超时的唯一判据形态）。
func (r *fakeRunner) deadline() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil {
		return time.Time{}, false
	}
	return r.ctx.Deadline()
}

// command 返回 runner 收到的命令。
func (r *fakeRunner) command() core.Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cmd
}

// callCount 返回 Run 被调用的次数——「拒绝发生在执行之前」的判据是**零调用**。
func (r *fakeRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// shellToolEnv 造一次命令执行的显式环境（Agent 上下文 / 超时 / 预算都由执行侧给出，
// 工具不回读任何全局状态）。
func shellToolEnv() core.ToolEnv {
	return core.ToolEnv{
		RunID:          "run-shell-1",
		SessionID:      "sess-shell-1",
		AgentName:      "demo",
		Timeout:        30 * time.Second,
		MaxOutputBytes: 256 * 1024,
	}
}

// shellPolicyFor 造一个只放行给定可执行名的**真策略**（命令档的整串逐字比对）。
func shellPolicyFor(t *testing.T, commands ...string) *WhitelistPolicy {
	t.Helper()
	return NewWhitelistPolicy(PolicyOptions{Root: t.TempDir(), AllowedCommands: commands})
}

// runShellChain 跑一次链路用例：真工具 → **真实 `core.ToolExecutor`** → 假审计仓储
// （Resolve / 入参校验 / 两阶段审计 / 截断 / 结果包装全走生产路径）。
func runShellChain(t *testing.T, shell core.Tool, budget int, arguments string) (core.Result, *fakeInvocationStore) {
	t.Helper()
	registry := NewRegistry(slog.New(slog.DiscardHandler))
	require.NoError(t, registry.Register(shell))
	audit := &fakeInvocationStore{}
	res := core.NewToolExecutor(registry, audit, nil).Execute(context.Background(),
		core.ToolEnv{RunID: "shell-chain-run", AgentName: "demo", MaxOutputBytes: budget},
		[]string{"shell"},
		core.ToolCall{ID: "call_1", Name: "shell", Arguments: arguments})
	return res, audit
}

// shellChainResult 解出审计行里的 `result_json`（字段面：content / retryable / truncated）。
func shellChainResult(t *testing.T, audit *fakeInvocationStore) (string, bool, bool) {
	t.Helper()
	require.Len(t, audit.rows, 1, "一条审计行（预写 + 终态回填各一次）")
	var out struct {
		Content   string `json:"content"`
		Retryable bool   `json:"retryable"`
		Truncated bool   `json:"truncated"`
	}
	require.NoError(t, json.Unmarshal(audit.rows[0].result.ResultJSON, &out))
	return out.Content, out.Retryable, out.Truncated
}

// —— 用例（17 条）——

// TestShellTool_SchemaAndMetadata：契约 §5.2 的参数表与元数据——工具名 `shell`、
// 描述非空、必填只有 `executable`（必填判据由执行器按这份 schema 执行），
// 参数集合恰为 executable / arguments / timeout_seconds 三项（不多不少）。
func TestShellTool_SchemaAndMetadata(t *testing.T) {
	shell := NewShellTool(&spyPolicy{}, &fakeRunner{})

	require.Equal(t, "shell", shell.Name())
	require.NotEmpty(t, shell.Description())
	require.Equal(t, []string{"executable"}, requiredFieldsOf(t, shell))

	var shape struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(shell.InputSchema(), &shape))
	require.Equal(t, []string{"arguments", "executable", "timeout_seconds"},
		slices.Sorted(maps.Keys(shape.Properties)), "参数集合逐字对齐契约 §5.2：无多余键")
}

// TestShellTool_ArgvVerbatimAndTargetIsWholeExecutable：契约 §5.2 的 argv 契约——授权面只判
// executable **整串**（`Action` 没有 argv 字段），参数逐项原样交给执行侧，工具一个字节都不改。
func TestShellTool_ArgvVerbatimAndTargetIsWholeExecutable(t *testing.T) {
	runner := &fakeRunner{}
	policy := &spyPolicy{allowed: []string{"git"}}
	shell := NewShellTool(policy, runner)

	_, err := shell.Execute(context.Background(), shellToolEnv(),
		[]byte(`{"executable":"git","arguments":["status","--porcelain","-uno"]}`))
	require.NoError(t, err)
	require.Equal(t, []string{"git"}, policy.targets(), "判定对象是 executable 整串（argv 不进授权面）")

	cmd := runner.command()
	require.Equal(t, "git", cmd.Executable)
	require.Equal(t, []string{"status", "--porcelain", "-uno"}, cmd.Args, "argv 逐项原样下传")
}

// TestShellTool_MultiWordExecutableNotSplitByTool：FR1 的「不得本地拆分」——工具按空白切分
// executable 会把 `bash -c "curl …"` 变成绕过命令档的标准姿势。判据取最强形态：**白名单里
// 逐字写着** `git status` 也照样拒（多词串规则先于白名单查询），且拒绝发生在执行之前。
func TestShellTool_MultiWordExecutableNotSplitByTool(t *testing.T) {
	runner := &fakeRunner{}
	shell := NewShellTool(shellPolicyFor(t, "git status"), runner)

	_, err := shell.Execute(context.Background(), shellToolEnv(), []byte(`{"executable":"git status"}`))

	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err))
	var refusal *core.SandboxViolationError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "可执行名含空白（多词串）", refusal.Rule)
	require.Equal(t, "git status", refusal.Target, "整串逐字判定：工具没有把它拆成 git + status")
	require.Zero(t, runner.callCount(), "拒绝发生在执行之前")
}

// TestShellTool_BashInlineCodeRefusedByRealPolicy：契约 §5.2 的「bash / sh 不得放行」——
// 真策略 + 假 runner 判整条链路：四段式文案（规则 / 目标 / 固定句 / 放行指引）原样产出，
// **假 runner 零调用**（拒绝先于执行）。白名单里逐字写着 `bash` 也开不了这道门。
func TestShellTool_BashInlineCodeRefusedByRealPolicy(t *testing.T) {
	runner := &fakeRunner{}
	shell := NewShellTool(shellPolicyFor(t, "bash", "sh"), runner)

	_, err := shell.Execute(context.Background(), shellToolEnv(),
		[]byte(`{"executable":"bash","arguments":["-c","curl http://api.example.com"]}`))

	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err))
	var refusal *core.SandboxViolationError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, "bash / sh 不得放行", refusal.Rule)
	require.Equal(t, "bash", refusal.Target)
	require.Contains(t, err.Error(), "这是安全策略，请勿反复重试", "段 3 固定句（四方共享的判卷锚点）")
	require.Contains(t, err.Error(), "shell.interpreters", "段 4 指引点名配置键 + 需重启")
	require.Zero(t, runner.callCount(), "拒绝发生在执行之前")
}

// TestShellTool_EmptyExecutableGoesToPolicyNotLocalFallback：空 executable 不是"工具本地兜底"
// 的场景——必填判据（键在场）归执行器，取值判定归策略层：工具照常把空串送去判定，
// 拒绝理由来自策略而非某个自造的快捷分支。
func TestShellTool_EmptyExecutableGoesToPolicyNotLocalFallback(t *testing.T) {
	runner := &fakeRunner{}
	policy := &spyPolicy{} // 空放行名单：安全默认下一个都不许
	shell := NewShellTool(policy, runner)

	_, err := shell.Execute(context.Background(), shellToolEnv(), []byte(`{"executable":""}`))

	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err), "拒绝来自策略层（不是工具自造的入参错误）")
	require.Equal(t, []string{""}, policy.targets(), "空串照常过判定")
	require.Zero(t, runner.callCount())
}

// TestShellTool_DirFollowsAgentContext：契约 §5.2 的 cwd 约定——有 Agent 上下文**且目录
// 存在**时子进程工作目录 = 该 Agent 目录；无上下文或目录不存在时留空（进程默认），
// 不用一个不存在的 cwd 去起进程。**不得**引入全局 chdir。
func TestShellTool_DirFollowsAgentContext(t *testing.T) {
	existing := t.TempDir()
	missing := filepath.Join(t.TempDir(), "absent")
	cases := []struct {
		name     string
		agentDir string
		wantDir  string
	}{
		{"有 Agent 上下文且目录存在", existing, existing},
		{"无 Agent 上下文", "", ""},
		{"目录不存在时留空", missing, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			shell := NewShellTool(&spyPolicy{allowed: []string{"git"}}, runner)
			env := shellToolEnv()
			env.AgentDir = tc.agentDir

			_, err := shell.Execute(context.Background(), env, []byte(`{"executable":"git"}`))

			require.NoError(t, err)
			require.Equal(t, tc.wantDir, runner.command().Dir)
		})
	}
}

// TestShellTool_EnvIsAlwaysEmpty：契约 §5.2 没有 env 参数——工具**不**自行透传任何变量
// （不读进程环境、不读代理变量），子进程的环境白名单整条归执行侧（`shell.env_passthrough`）。
func TestShellTool_EnvIsAlwaysEmpty(t *testing.T) {
	runner := &fakeRunner{}
	shell := NewShellTool(&spyPolicy{allowed: []string{"git"}}, runner)

	_, err := shell.Execute(context.Background(), shellToolEnv(), []byte(`{"executable":"git"}`))

	require.NoError(t, err)
	require.Nil(t, runner.command().Env, "工具不声明任何环境变量（白名单归执行侧统一执行）")
}

// TestShellTool_OutputPassedThroughVerbatim：FR7 的截断分工——上游限流点（runner）追加的
// 标记连同输出**原样透传**，工具不自行截断、不再添标记（会出现两个标记）；非零退出码
// 照进首行（它是结果的一部分）。
func TestShellTool_OutputPassedThroughVerbatim(t *testing.T) {
	const maxBytes = 32
	marker := core.TruncationMarker(maxBytes) // runner 已追加的那一个
	runner := &fakeRunner{result: core.CommandResult{
		Output: "partial" + marker, ExitCode: 1, Truncated: true,
	}}
	shell := NewShellTool(&spyPolicy{allowed: []string{"git"}}, runner)

	res, err := shell.Execute(context.Background(), shellToolEnv(), []byte(`{"executable":"git"}`))

	require.NoError(t, err)
	require.Equal(t, "退出码：1\npartial"+marker, res.Content, "输出与标记原样透传")
	require.Equal(t, 1, strings.Count(res.Content, marker), "标记恰一次：工具不得再加一个")
}

// TestShellTool_RunnerErrorPropagatesUnchanged：执行侧的错误**原样**上抛——启动失败 /
// 解释器形态 / 脚本越界的分类归执行器（`sandbox_denied` / `runtime_error` 的判定在那一侧），
// 工具不包装、不吞、不改文案；取消 / 截止的 ctx 身份同样穿透（执行器据此归 `timeout`，
// 口径 3 的"身份优先"在工具侧也要立得住）。
func TestShellTool_RunnerErrorPropagatesUnchanged(t *testing.T) {
	sentinel := errors.New("启动子进程 git：找不到可执行文件")
	runner := &fakeRunner{err: sentinel}
	shell := NewShellTool(&spyPolicy{allowed: []string{"git"}}, runner)

	res, err := shell.Execute(context.Background(), shellToolEnv(), []byte(`{"executable":"git"}`))

	require.ErrorIs(t, err, sentinel, "错误原样上抛")
	require.Equal(t, core.Result{}, res, "失败路径不产出结果")

	canceled := &fakeRunner{err: errors.Join(context.Canceled, errors.New("回收子进程：进程已终止"))}
	shell = NewShellTool(&spyPolicy{allowed: []string{"git"}}, canceled)

	_, err = shell.Execute(context.Background(), shellToolEnv(), []byte(`{"executable":"git"}`))

	require.ErrorIs(t, err, context.Canceled, "ctx 身份穿透包装（分类归 timeout）")
}

// TestShellTool_DecodeTypeErrorIsInvalidInput：注 11——字段**类型**不符（给错类型的参数）
// 是调用方的入参问题，不是本仓缺陷：解码失败一律 `*core.InvalidInputError`（⇒ 分类
// `invalid_input`），且**不执行**。必填缺失不在此判——那是执行器入口的活（更早一道）。
func TestShellTool_DecodeTypeErrorIsInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"arguments 类型错", `{"executable":"git","arguments":"oops"}`},
		{"arguments 元素类型错", `{"executable":"git","arguments":[1,2]}`},
		{"timeout_seconds 类型错", `{"executable":"git","timeout_seconds":"5"}`},
		{"executable 类型错", `{"executable":42}`},
		{"整体不是对象", `["git"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			shell := NewShellTool(&spyPolicy{allowed: []string{"git"}}, runner)

			_, err := shell.Execute(context.Background(), shellToolEnv(), []byte(tc.input))

			require.Error(t, err)
			require.True(t, core.IsInvalidInput(err), "解码失败走 invalid_input 载体")
			require.Zero(t, runner.callCount(), "解码失败不执行")
		})
	}
}

// TestShellTool_BudgetFlowsIntoCommand：约束 28——逐 Run 的读取上限经 `Command.MaxOutputBytes`
// 下传（漏传 = 读取端恒用装配值，配置收紧的预算到不了子进程读取口）；执行侧没给预算时
// 原样零值下传（零值语义 = 用 runner 装配值，工具**不得**自作主张填一个数字）。
func TestShellTool_BudgetFlowsIntoCommand(t *testing.T) {
	cases := []struct {
		name   string
		budget int
		want   int
	}{
		{"逐 Run 预算随行", 4096, 4096},
		{"未给预算时零值下传", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{}
			shell := NewShellTool(&spyPolicy{allowed: []string{"git"}}, runner)
			env := shellToolEnv()
			env.MaxOutputBytes = tc.budget

			_, err := shell.Execute(context.Background(), env, []byte(`{"executable":"git"}`))

			require.NoError(t, err)
			require.Equal(t, tc.want, runner.command().MaxOutputBytes)
		})
	}
}

// TestShellTool_ArgvShapeChecksStayInRunner：授权与执行分离（宪法 VI）在工具侧的形状——
// 解释器受控通道的 argv 形态校验（`-c` 拒、脚本真实路径越界拒）归**执行侧**，工具不本地
// 预判：命中解释器名单的调用照常下传，越界与否由 `CommandRunner` 拒（那一面归
// runner_test.go 的 G8）。工具多做一道本地判定 = 同一语义两份实现。
func TestShellTool_ArgvShapeChecksStayInRunner(t *testing.T) {
	policy := NewWhitelistPolicy(PolicyOptions{Root: t.TempDir(), Interpreters: []string{"python"}})
	runner := &fakeRunner{}
	shell := NewShellTool(policy, runner)

	_, err := shell.Execute(context.Background(), shellToolEnv(),
		[]byte(`{"executable":"python","arguments":["-c","print(1)"]}`))

	require.NoError(t, err, "工具不做形态判定：`-c` 形态由执行侧拒")
	require.Equal(t, 1, runner.callCount())
	cmd := runner.command()
	require.Equal(t, "python", cmd.Executable)
	require.Equal(t, []string{"-c", "print(1)"}, cmd.Args, "argv 逐项原样下传")
}

// TestShellTool_TimeoutSecondsOnlyNarrows：契约 §5.2 的扩展字段 + Compatibility §2.2
// 「只能是缩短」+ 契约 §6「模型只能缩短」。**父 deadline 先建立**——生产链路由执行器在
// env.Timeout 处建立（tool_exec.go:102-107，取值 <= 0 视为不设限）；本用例直接调用工具，
// 就自己把等价形态建出来（评审 P2-6 修正原稿：用 context.Background() 却断言"缺省"分支
// 有 deadline，是把执行器的职责错当工具的）。判据不比对文本：假 runner 记录**收到的 ctx
// 截止时刻**。
func TestShellTool_TimeoutSecondsOnlyNarrows(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		want      time.Duration
	}{
		{"缺省 → 用父 deadline", `{"executable":"git"}`, 30 * time.Second},
		{"更短 → 收窄一层", `{"executable":"git","timeout_seconds":5}`, 5 * time.Second},
		{"更长 → 不生效", `{"executable":"git","timeout_seconds":3600}`, 30 * time.Second},
		{"非正值 → 按缺省", `{"executable":"git","timeout_seconds":0}`, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{} // 记录 Run 收到的 ctx 与 core.Command
			shell := NewShellTool(&spyPolicy{allowed: []string{"git"}}, runner)

			parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, err := shell.Execute(parent,
				core.ToolEnv{Timeout: 30 * time.Second, AgentName: "demo"}, []byte(tc.requested))

			require.NoError(t, err)
			deadline, ok := runner.deadline()
			require.True(t, ok, "生效超时必须以 deadline 形态交给 runner（工具只做收窄、自身不建超时路径）")
			require.InDelta(t, tc.want.Seconds(), time.Until(deadline).Seconds(), 1.0,
				"生效超时 = min(env.Timeout, timeout_seconds)")
		})
	}
}

// TestShellTool_NonZeroExitIsNotAnError：core.CommandResult 的既有口径（runner_test.go:316
// 已判"runner 不报错"）——工具侧再判一次**呈现**：退出码进结果首行、输出原样保留（工具
// 不追加标记——约束 11）、逐 Run 预算随 Command 下传（约束 28）。把非零码判成失败会让
// grep 类"没匹配到"全变失败。
func TestShellTool_NonZeroExitIsNotAnError(t *testing.T) {
	runner := &fakeRunner{result: core.CommandResult{Output: "no match\n", ExitCode: 1}}
	shell := NewShellTool(&spyPolicy{allowed: []string{"grep"}}, runner)

	res, err := shell.Execute(context.Background(),
		core.ToolEnv{Timeout: 30 * time.Second, MaxOutputBytes: 256 * 1024},
		[]byte(`{"executable":"grep","arguments":["-r","needle","."]}`))

	require.NoError(t, err, "非零退出码不是 error")
	require.Equal(t, "退出码：1\nno match\n", res.Content)
	require.False(t, res.Retryable) // §6.2 固定口径 2 的默认值
	require.Equal(t, core.Command{
		Executable: "grep", Args: []string{"-r", "needle", "."},
		MaxOutputBytes: 256 * 1024, // 逐 Run 读取上限（约束 28）——漏传 = 读取端恒用装配值
	}, runner.command(), "argv 逐项原样、不经任何 shell；预算随行")
}

// TestShellTool_ChainCapsFinalContentWithExitLine：§6.2 固定口径 7 在命令面的完整判卷——
// 首行「退出码：<N>」的开销**计入**预算（预算口径 = 最终 `Content`）；执行器的
// `len(content) > maxBytes` 判据在"恰超一字节"处命中；标记恰一次、N = 生效上限、truncated
// 值正确；空输出只有退出码行。链路 = 真工具 → **真实 `core.ToolExecutor`** → 假审计仓储。
func TestShellTool_ChainCapsFinalContentWithExitLine(t *testing.T) {
	const capBytes = 64
	exitLine := "退出码：0\n" // 首行开销：14 字节（中文按字节计）
	cases := []struct {
		name          string
		output        string
		wantTruncated bool
	}{
		{"小于上限", strings.Repeat("a", 16), false},
		{"等于上限", strings.Repeat("a", capBytes-len(exitLine)), false}, // 退出码行计入 ⇒ 恰为上限
		{"超过上限一字节", strings.Repeat("a", capBytes-len(exitLine)+1), true},
		{"空输出只有退出码行", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shell := NewShellTool(&spyPolicy{allowed: []string{"git"}},
				&fakeRunner{result: core.CommandResult{Output: tc.output}})

			res, audit := runShellChain(t, shell, capBytes, `{"executable":"git","arguments":["status"]}`)

			content, retryable, truncated := shellChainResult(t, audit)
			require.Equal(t, res.Content, content, "返回给循环的内容与落库值同一份")
			require.False(t, retryable)
			require.Equal(t, tc.wantTruncated, truncated)
			require.True(t, strings.HasPrefix(content, exitLine), "首行是退出码")
			marker := core.TruncationMarker(capBytes)
			if tc.wantTruncated {
				require.Equal(t, 1, strings.Count(content, marker), "标记恰一次")
				require.LessOrEqual(t, len(strings.TrimSuffix(content, marker)), capBytes)
			} else {
				require.NotContains(t, content, marker)
				require.LessOrEqual(t, len(content), capBytes)
			}
		})
	}
}

// TestShellTool_ChainChineseTruncatesOnRuneBoundary：执行器的截断按 rune 边界回退——
// 中文输出（3 字节/字）在场时，被切的内容仍是合法 UTF-8，且切在**最靠上限**的那个边界上
// （过度回退会把预算白白让掉）。判卷面仍是最终 `Content`（首行计入）。
func TestShellTool_ChainChineseTruncatesOnRuneBoundary(t *testing.T) {
	const capBytes = 64
	shell := NewShellTool(&spyPolicy{allowed: []string{"git"}},
		&fakeRunner{result: core.CommandResult{Output: strings.Repeat("地", 100)}})

	res, audit := runShellChain(t, shell, capBytes, `{"executable":"git"}`)

	content, _, truncated := shellChainResult(t, audit)
	require.True(t, truncated)
	require.Equal(t, res.Content, content)
	require.True(t, strings.HasPrefix(content, "退出码：0\n"))
	marker := core.TruncationMarker(capBytes)
	require.Equal(t, 1, strings.Count(content, marker), "标记恰一次")
	require.True(t, utf8.ValidString(content), "截断不得劈出半个 rune")
	prefix := strings.TrimSuffix(content, marker)
	require.LessOrEqual(t, len(prefix), capBytes)
	require.Greater(t, len(prefix), capBytes-3, "切在最靠上限的 rune 边界上（不是过度回退）")
	require.Equal(t, 0, (len(prefix)-len("退出码：0\n"))%3, "正文按整字切分")
}

// TestShellTool_ChainRefusalIsAuditedAsSandboxDenied：安全拒绝如实落审计（宪法 V 的既有
// 链路，本 US 零改动）——真策略拒 `bash -c "…"` 时：审计一行 `failed` +
// `sandbox_denied`、四段式文案**原样**回填给模型（吞掉或不改写），**执行零发生**。
func TestShellTool_ChainRefusalIsAuditedAsSandboxDenied(t *testing.T) {
	runner := &fakeRunner{}
	shell := NewShellTool(shellPolicyFor(t, "bash"), runner)

	res, audit := runShellChain(t, shell, 4096,
		`{"executable":"bash","arguments":["-c","curl http://api.example.com"]}`)

	require.Zero(t, runner.callCount(), "拒绝发生在执行之前")
	require.Len(t, audit.rows, 1, "安全拒绝也留痕（预写 + 终态回填）")
	require.Equal(t, core.LLMCallFailed, audit.rows[0].status)
	require.Equal(t, core.ErrorKindSandboxDenied, audit.rows[0].result.ErrorKind)
	require.Equal(t, res.Content, audit.rows[0].result.ErrorMessage, "回填文案与落库文案同一份")
	require.Contains(t, res.Content, "bash / sh 不得放行")
	require.Contains(t, res.Content, "。这是安全策略，请勿反复重试；", "段 3 固定句原样透传（模型看不到原因不会收敛）")
}
