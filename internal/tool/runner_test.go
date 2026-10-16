package tool_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// 本文件是受控子进程执行器的关键回归用例（颗粒度文档 §3.2 的 tool/runner_test.go 十条，
// G1~G10；第 10 条由实现方补：cwd 生效 + argv 直执不经 shell）。两条"跑一次单请求看不出来"
// 的保证在这里判卷：**内存不随输出膨胀**（读取中限流）与**取消即整树回收**（父子一并死）。
// 用例全部平台中立——辅以测试二进制自身当被测命令（见 TestMain），不依赖任何外部可执行文件。
// Regression cases for the guarded runner: G1-G10, platform-neutral by construction.

// 辅助进程的开关与各模式的参数名（都经工具声明的环境变量传递，正好一并复核白名单形态）。
// Helper-process switches; they travel as tool-declared environment variables.
const (
	helperModeVar      = "BASALT_RUNNER_HELPER_MODE"
	helperProbeVar     = "BASALT_RUNNER_HELPER_PROBE"
	helperExitCodeVar  = "BASALT_RUNNER_HELPER_EXIT"
	helperFloodVar     = "BASALT_RUNNER_HELPER_FLOOD"
	helperHeartbeatVar = "BASALT_RUNNER_HELPER_HEARTBEAT"

	helperModeReport   = "report"
	helperModeEnvProbe = "env-probe"
	helperModeEnvDump  = "env-dump"
	helperModeExit     = "exit"
	helperModeFlood    = "flood"
	helperModeTree     = "tree"
	helperModeBeat     = "beat"
)

// TestMain 兼作辅助进程的入口：带辅助标记被拉起时走辅助分支、**不经测试框架**，否则正常跑
// 本包测试。真起进程的用例必须有一个可控的命令，用测试二进制自身当那个命令最省事——不依赖
// 外部可执行文件，CI 上也不挑平台。
// TestMain doubles as the helper-process entry point: the runner tests use the test
// binary itself as the command under test.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperModeVar); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

// runHelper 是测试替身命令的主干：按模式干活并返回退出码。它**不是**被测代码，只为用例提供
// 可控的子进程行为（写多少字节、退出码多少、是否派生子进程）。
// runHelper is the stand-in command body; it only provides controllable child behaviour.
func runHelper(mode string) int {
	switch mode {
	case helperModeReport:
		workDir, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Println("cwd=" + workDir)
		for index, arg := range os.Args[1:] {
			fmt.Printf("arg%d=%s\n", index, arg)
		}
	case helperModeEnvProbe:
		for _, name := range strings.Split(os.Getenv(helperProbeVar), ",") {
			value, ok := os.LookupEnv(name)
			if !ok {
				value = "<unset>"
			}
			fmt.Printf("%s=%s\n", name, value)
		}
	case helperModeEnvDump:
		// 每条都带 `env:` 前缀：输出流是 stdout 与 stderr 的**合流**，子进程（就是本测试
		// 二进制）退出时运行时还可能往上写自己的话（例如 `-cover` 构建会写一行 GOCOVERDIR
		// 警告），整份输出直接当环境清单解析会被那种噪声骗到——只认带前缀的行。
		for _, entry := range os.Environ() {
			fmt.Println("env:" + entry)
		}
	case helperModeExit:
		fmt.Println("stdout-line")
		fmt.Fprintln(os.Stderr, "stderr-line")
		code, err := strconv.Atoi(os.Getenv(helperExitCodeVar))
		if err != nil {
			return 2
		}
		return code
	case helperModeFlood:
		total, err := strconv.Atoi(os.Getenv(helperFloodVar))
		if err != nil {
			return 2
		}
		chunk := bytes.Repeat([]byte("a"), 4096)
		for written := 0; written < total; {
			size := len(chunk)
			if remaining := total - written; remaining < size {
				size = remaining
			}
			if _, err := os.Stdout.Write(chunk[:size]); err != nil {
				return 2
			}
			written += size
		}
	case helperModeTree:
		// 孙进程：再拉一个自己（beat 模式）写心跳，然后本进程常驻不退——这样"整树回收"
		// 才有可观察对象，只杀子进程一个的话孙进程会继续写。
		self, err := os.Executable()
		if err != nil {
			return 2
		}
		beat := exec.Command(self)
		beat.Env = []string{helperModeVar + "=" + helperModeBeat, helperHeartbeatVar + "=" + os.Getenv(helperHeartbeatVar)}
		if err := beat.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "spawn grandchild:", err)
			return 2
		}
		fmt.Printf("grandchild=%d\n", beat.Process.Pid)
		for {
			time.Sleep(time.Hour)
		}
	case helperModeBeat:
		path := os.Getenv(helperHeartbeatVar)
		for {
			file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return 2
			}
			if _, err := file.WriteString("beat\n"); err != nil {
				return 2
			}
			if err := file.Close(); err != nil {
				return 2
			}
			time.Sleep(50 * time.Millisecond)
		}
	default:
		return 2
	}
	return 0
}

// runnerFixture 是一份最小工作区夹具 + 受控执行器：脚本允许区（本 Agent 的 scripts/、
// 工作区技能库 skills/）与一个"工作区之外"的目录都在 t.TempDir() 下现造。
// A minimal workspace fixture plus the runner under test.
type runnerFixture struct {
	root       string // 工作区根（已投影真实路径，见 newRunnerFixture）
	runner     *tool.LocalRunner
	self       string // 测试二进制自身：当被测命令用
	agentDir   string // agents/demo：命令的缺省工作目录
	scriptPath string // agents/demo/scripts/run.sh：合法脚本（形态校验放行的那一个）
	otherAgent string // agents/other/scripts/run.sh：别的 Agent 的脚本
	outside    string // 工作区之外的脚本
}

// newRunnerFixture 造夹具；mutate 非 nil 时用来改装配参数（白名单 / 上限 / 解释器名单）。
// 工作区根取 t.TempDir() 的**投影真实路径**：脚本允许区判定比较的是投影后的路径，夹具必须
// 站在同一个坐标系里（临时目录在某些机器上本身经过软链接），否则合法脚本会被误判越界。
// The workspace root is the projected path so that both sides compare in one basis.
func newRunnerFixture(t *testing.T, mutate func(*tool.RunnerOptions)) *runnerFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	self, err := os.Executable()
	require.NoError(t, err)
	fixture := &runnerFixture{
		root:       root,
		self:       self,
		agentDir:   filepath.Join(root, "agents", "demo"),
		scriptPath: filepath.Join(root, "agents", "demo", "scripts", "run.sh"),
		otherAgent: filepath.Join(root, "agents", "other", "scripts", "run.sh"),
	}
	scripts := []string{
		fixture.scriptPath,
		fixture.otherAgent,
		filepath.Join(root, "skills", "tool.sh"),
	}
	for _, script := range scripts {
		require.NoError(t, os.MkdirAll(filepath.Dir(script), 0o755))
		require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755))
	}
	// 工作区**之外**的脚本：另一个临时目录（t.TempDir() 每次给新目录）。
	fixture.outside = filepath.Join(t.TempDir(), "outside.sh")
	require.NoError(t, os.WriteFile(fixture.outside, []byte("#!/bin/sh\n"), 0o755))

	opts := tool.RunnerOptions{Root: root}
	if mutate != nil {
		mutate(&opts)
	}
	fixture.runner = tool.NewLocalRunner(opts)
	return fixture
}

// command 组一个"跑测试二进制自身"的辅助命令：模式与参数走工具声明的环境变量，
// 工作目录缺省落在 Agent 目录（与真实调用形态一致）。
func (f *runnerFixture) command(mode string, extraEnv ...string) core.Command {
	env := append([]string{helperModeVar + "=" + mode}, extraEnv...)
	return core.Command{Executable: f.self, Dir: f.agentDir, Env: env}
}

// TestLocalRunner_ChildEnvIsAllowlisted 覆盖 G1：环境变量**白名单**——只透传
// shell.env_passthrough 点名的父进程变量，工具显式声明的项照传，同名时以声明为准；
// 其余一个都不给。白名单点名但父进程没有的变量不凭空造。
// Acceptance point (G1): the child environment is exactly allowlist ∩ parent + declared.
func TestLocalRunner_ChildEnvIsAllowlisted(t *testing.T) {
	t.Setenv("BASALT_RUNNER_KEEP", "kept-value")
	t.Setenv("BASALT_RUNNER_DROP", "must-not-leak")
	fixture := newRunnerFixture(t, func(opts *tool.RunnerOptions) {
		opts.EnvPassthrough = []string{"BASALT_RUNNER_KEEP", "BASALT_RUNNER_ABSENT"}
	})
	res, err := fixture.runner.Run(context.Background(), fixture.command(helperModeEnvProbe,
		helperProbeVar+"=BASALT_RUNNER_KEEP,BASALT_RUNNER_DROP,BASALT_RUNNER_ABSENT,BASALT_RUNNER_DECLARED",
		"BASALT_RUNNER_DECLARED=from-tool",
		"BASALT_RUNNER_KEEP=tool-wins",
	))
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
	require.Contains(t, res.Output, "BASALT_RUNNER_KEEP=tool-wins\n", "同名时以工具声明为准")
	require.Contains(t, res.Output, "BASALT_RUNNER_DECLARED=from-tool\n")
	require.Contains(t, res.Output, "BASALT_RUNNER_DROP=<unset>\n", "不在白名单里的父进程变量一个都不许透传")
	require.Contains(t, res.Output, "BASALT_RUNNER_ABSENT=<unset>\n", "白名单点名但父进程没有的变量不凭空造")
}

// TestLocalRunner_KillsProcessTreeOnCancel 覆盖 G2（中文验收点：**超时杀整棵进程树，子进程
// 一并死**）：取消 / 超时共用 ctx.Done() 这一条分支，本用例用**取消**做确定性触发——先等
// 孙进程真的在写心跳再取消，比"定时到点"可靠：慢机器上先取消会退化成"什么都没起就先杀了"，
// 用例照样绿却什么都没测到（负例控制）。心跳在取消后**不得**再增长。
// Acceptance point (G2): cancelling reclaims the whole tree, grandchild included.
func TestLocalRunner_KillsProcessTreeOnCancel(t *testing.T) {
	heartbeat := filepath.Join(t.TempDir(), "heartbeat.log")
	fixture := newRunnerFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() {
		_, err := fixture.runner.Run(ctx, fixture.command(helperModeTree, helperHeartbeatVar+"="+heartbeat))
		runErr <- err
	}()
	waitForHeartbeats(t, heartbeat, 3)
	cancel()
	select {
	case err := <-runErr:
		require.ErrorIs(t, err, context.Canceled, "取消必须体现在返回错误里")
	case <-time.After(10 * time.Second):
		t.Fatal("取消之后 Run 未在 10 秒内返回（整树回收可能挂住了）")
	}
	beats := heartbeatCount(t, heartbeat)
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, beats, heartbeatCount(t, heartbeat), "取消后孙进程不得继续写心跳（整棵树都要回收）")
}

// TestLocalRunner_ThrottlesOutputWhileReading 覆盖 G3：输出**读取中**限流——8 MB 输出、缺省
// 256 KB 上限。主判据是内容长度恰为"生效上限 + 截断标记"；累计分配只作辅助观察（阈值取
// 输出规模的一半——"先读全再截断"的实现累计分配必然 ≥ 输出规模）。余量必须读干：不读干的
// 话还在写的子进程会阻塞在写端，要到超时才被杀，本用例也会因超时而失败。
// Acceptance point (G3): throttling happens during the read, not after it.
func TestLocalRunner_ThrottlesOutputWhileReading(t *testing.T) {
	const floodBytes = 8 << 20
	fixture := newRunnerFixture(t, nil)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res, err := fixture.runner.Run(context.Background(),
		fixture.command(helperModeFlood, helperFloodVar+"="+strconv.Itoa(floodBytes)))
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode, "洪泛的子进程必须正常收场（读干余量才不会被写阻塞拖死）")
	require.True(t, res.Truncated)
	require.Len(t, res.Output, 256*1024+len(core.TruncationMarker(256*1024)),
		"输出长度 = 缺省生效上限 256 KB + 截断标记")
	require.Equal(t, strings.Repeat("a", 256*1024), res.Output[:256*1024], "保留的必须是**前** 256 KB")
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(floodBytes/2),
		"8 MB 输出下累计分配不得接近输出规模（读取中限流才是限流）")
}

// TestLocalRunner_RespectsSmallerOutputCap 覆盖 G4：配置的**更小**上限被尊重（不是"至少
// 256 KB"），标记里的数字是生效上限而不是缺省值。
// Acceptance point (G4): a smaller configured cap wins over the default.
func TestLocalRunner_RespectsSmallerOutputCap(t *testing.T) {
	const maxBytes = 32
	fixture := newRunnerFixture(t, func(opts *tool.RunnerOptions) { opts.MaxOutputBytes = maxBytes })
	res, err := fixture.runner.Run(context.Background(),
		fixture.command(helperModeFlood, helperFloodVar+"=4096"))
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
	require.True(t, res.Truncated)
	require.Equal(t, strings.Repeat("a", maxBytes)+core.TruncationMarker(maxBytes), res.Output,
		"生效上限是 32 字节：标记里的数字也是 32，而不是缺省 262144")
}

// TestLocalRunner_TruncationMarkerOmitsOriginalLength 覆盖 G5：截断标记**不承诺**原始长度
// ——读取中限流从来没有把完整输出读进来，标记里写原始长度就是编数字。判据取最严的等值
// 形态：内容 = 前 64 字节 + 标记，且整个输出里不出现真实规模 1000。
// Acceptance point (G5): the marker carries the effective cap, never the original size.
func TestLocalRunner_TruncationMarkerOmitsOriginalLength(t *testing.T) {
	const maxBytes = 64
	const floodBytes = 1000
	fixture := newRunnerFixture(t, func(opts *tool.RunnerOptions) { opts.MaxOutputBytes = maxBytes })
	res, err := fixture.runner.Run(context.Background(),
		fixture.command(helperModeFlood, helperFloodVar+"="+strconv.Itoa(floodBytes)))
	require.NoError(t, err)
	require.True(t, res.Truncated)
	require.Equal(t, strings.Repeat("a", maxBytes)+core.TruncationMarker(maxBytes), res.Output)
	require.NotContains(t, res.Output, strconv.Itoa(floodBytes), "标记不得写原始输出规模")
}

// TestLocalRunner_CapturesExitCodeAndStderr 覆盖 G6：捕获退出码与 stderr——**非零退出码不是
// 运行期错误**（它是子进程的正常产出形态，失败分类归工具层），stdout 与 stderr 合流且保持
// 进程写出的顺序。
// Acceptance point (G6): exit code and stderr are captured; a non-zero exit is a result.
func TestLocalRunner_CapturesExitCodeAndStderr(t *testing.T) {
	fixture := newRunnerFixture(t, nil)
	res, err := fixture.runner.Run(context.Background(),
		fixture.command(helperModeExit, helperExitCodeVar+"=3"))
	require.NoError(t, err, "非零退出码不得让 Run 报错")
	require.Equal(t, 3, res.ExitCode)
	require.False(t, res.Truncated)
	require.Contains(t, res.Output, "stdout-line")
	require.Contains(t, res.Output, "stderr-line")
	require.Less(t, strings.Index(res.Output, "stdout-line"), strings.Index(res.Output, "stderr-line"),
		"两股输出合流且保持进程写出的顺序")
}

// TestLocalRunner_DoesNotInheritProxyOrCredentialVars 覆盖 G7：白名单为空时子进程的环境
// **一个父进程变量都不继承**——代理与凭证类变量在真实部署环境里遍地都是，这一条判的是
// "运行时不会把企业代理或密钥悄悄递给被调用的命令"。判据取整份环境枚举（比逐个点名探测强：
// 没点名的变量也逃不出去）。
// Acceptance point (G7): with an empty allowlist the child inherits nothing.
func TestLocalRunner_DoesNotInheritProxyOrCredentialVars(t *testing.T) {
	for name, value := range map[string]string{
		"HTTPS_PROXY":           "http://probe.invalid:8080",
		"HTTP_PROXY":            "http://probe.invalid:8080",
		"NO_PROXY":              "localhost",
		"AWS_SECRET_ACCESS_KEY": "probe-placeholder-not-a-credential",
		"OPENAI_API_KEY":        "probe-placeholder-not-a-credential",
	} {
		t.Setenv(name, value)
	}
	fixture := newRunnerFixture(t, nil)
	res, err := fixture.runner.Run(context.Background(), fixture.command(helperModeEnvDump))
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
	require.Equal(t, []string{helperModeVar + "=" + helperModeEnvDump}, envLines(res.Output),
		"子进程的环境应当只剩工具自己声明的那一项（父进程的变量一项都没跟过去）")
}

// TestLocalRunner_RejectsInterpreterArgvShapes 覆盖 G8：解释器受控通道的**执行侧**校验——
// `-c` / `-m` 这类内联代码形态拒、脚本真实路径越界拒（授权面只看 executable 整串，argv 形态
// 只能在执行侧判）；合法形态必须放行到启动（放行的读数 = 失败原因**不是**沙箱拒绝，而是
// 可执行名跑不起来）。解释器名字取不存在的字符串，让"放行"这一条在三平台上读数一致。
// Acceptance point (G8): inline-code argv and out-of-area scripts are refused on the
// execution side, while a legal shape reaches the actual spawn.
func TestLocalRunner_RejectsInterpreterArgvShapes(t *testing.T) {
	const interpreter = "basalt-runner-nonexistent-interpreter"
	fixture := newRunnerFixture(t, func(opts *tool.RunnerOptions) {
		opts.Interpreters = []string{interpreter}
	})
	cases := []struct {
		name string
		args []string
		rule string
	}{
		{"无参数", nil, "解释器 argv 形态不合法"},
		{"-c 内联代码", []string{"-c", "print(1)"}, "解释器 argv 形态不合法"},
		{"-m 模块形态", []string{"-m", "http.server"}, "解释器 argv 形态不合法"},
		{"脚本在别的 Agent 目录", []string{fixture.otherAgent}, "解释器脚本不在允许区"},
		{"脚本在工作区之外", []string{fixture.outside}, "解释器脚本不在允许区"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := fixture.runner.Run(context.Background(), core.Command{
				Executable: interpreter,
				Args:       testCase.args,
				Dir:        fixture.agentDir,
			})
			require.Error(t, err)
			require.True(t, core.IsSandboxViolation(err), "解释器形态拒绝也走四段式沙箱文案")
			require.Contains(t, err.Error(), testCase.rule)
		})
	}
	t.Run("本 Agent 的 scripts 内放行到启动", func(t *testing.T) {
		_, err := fixture.runner.Run(context.Background(), core.Command{
			Executable: interpreter,
			Args:       []string{fixture.scriptPath, "--flag"},
			Dir:        fixture.agentDir,
		})
		require.Error(t, err, "解释器不存在，启动必然失败——这正说明形态校验已经放行")
		require.False(t, core.IsSandboxViolation(err), "合法形态必须通过执行侧校验")
		require.Contains(t, err.Error(), "启动子进程")
	})
	t.Run("技能库内的脚本同样放行", func(t *testing.T) {
		skill := filepath.Join(fixture.root, "skills", "tool.sh")
		_, err := fixture.runner.Run(context.Background(), core.Command{
			Executable: interpreter,
			Args:       []string{skill},
			Dir:        fixture.agentDir,
		})
		require.Error(t, err)
		require.False(t, core.IsSandboxViolation(err), "技能库是受控脚本的第二处允许区")
	})
}

// TestLocalRunner_AppliesDirAndSkipsShell 覆盖 G10（第 10 条由实现方补）：cwd 真的生效
// （子进程看到的工作目录 = cmd.Dir；空值回落进程默认 CWD，**不**做全局 chdir），且 argv
// **直执不经任何 shell 解释**——带空格 / 引号 / `&&` 的单个参数必须逐字到达：任何一层
// shell 都会把 `&&` 当运算符、把引号当语法，参数就不可能原样出现。
// Acceptance point (G10): the working directory applies and argv is passed verbatim.
func TestLocalRunner_AppliesDirAndSkipsShell(t *testing.T) {
	fixture := newRunnerFixture(t, nil)
	args := []string{"plain", "with space", `quote"inside`, "&&", "--flag=value"}
	command := fixture.command(helperModeReport)
	command.Args = args
	res, err := fixture.runner.Run(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
	require.Contains(t, res.Output, "cwd="+fixture.agentDir+"\n")
	for index, arg := range args {
		require.Contains(t, res.Output, fmt.Sprintf("arg%d=%s\n", index, arg),
			"argv 必须逐字到达（不经任何 shell 解释）")
	}
	workDir, err := os.Getwd()
	require.NoError(t, err)
	noDir := fixture.command(helperModeReport)
	noDir.Dir = ""
	res, err = fixture.runner.Run(context.Background(), noDir)
	require.NoError(t, err)
	require.Contains(t, res.Output, "cwd="+workDir+"\n", "没有 Agent 上下文时回落进程默认工作目录")
}

// waitForHeartbeats 轮询心跳文件直到行数达标：供"先确认孙进程跑起来了、再取消"的顺序用。
func waitForHeartbeats(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if heartbeatCount(t, path) >= want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("心跳文件 %s 在 10 秒内未达到 %d 行：孙进程没有跑起来", path, want)
}

// heartbeatCount 数心跳文件的行数（文件尚未创建按 0 行算）。
func heartbeatCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(data), "\n")
}

// envLines 从环境枚举输出里挑出带 `env:` 前缀的行（去掉前缀），供"一个都不继承"做整份比对：
// 同一股输出里混着 stderr 的运行时噪声，按前缀挑才不受它影响。
func envLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if entry, ok := strings.CutPrefix(line, "env:"); ok {
			lines = append(lines, entry)
		}
	}
	return lines
}
