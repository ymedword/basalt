package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是受控子进程执行器的**平台无关**半边：环境白名单、输出读取中限流、取消与超时
// 语义、解释器 argv 形态校验都在这里；起进程与整树终止两个动作按平台下沉到
// runner_windows.go / runner_unix.go。取消语义的两条要点：① 起进程之前的取消直接判掉
//（起完再杀是白干一场），② 起进程之后的取消必须**整树**终止——单杀子进程会留下它派生的
// 后代，那些后代照样在跑用户代码。
// Platform-neutral half of the guarded runner: env allowlisting, read-time throttling,
// cancellation semantics, and interpreter shape checks. Spawn and tree termination are
// delegated to the per-platform files.

// 受控执行器的常量（取值与颗粒度文档 §6.2 对齐；**不得**在别处复制这些数字）。
// Runner constants; the values are frozen by the granularity document §6.2.
const (
	// defaultMaxOutputBytes 是输出上限的缺省值（256 KB）：配置给非正值时按缺省算——
	// 0 不能解释成"不限量"，那等于把这条防线整个关掉。
	defaultMaxOutputBytes = 256 * 1024
	// pipeDrainGrace 是"子进程已回收、读者仍未收口"时的兜底时限。正常路径读者随管道
	// EOF 立即收口；只有后代进程攥着写端（逃出了进程组 / Job）才会走到这里——到点关读端
	// 解阻并如实失败，**不得**挂等：工具执行不能出现无上限的等待。
	pipeDrainGrace = 250 * time.Millisecond
)

// 解释器受控通道的拒绝文案固定片段（四段式的段 1 规则 / 段 4 放行指引；段 2 具体目标、
// 段 3 出处由 `core.SandboxViolationError` 拼——与 policy.go 同一套契约）。
// Frozen rule and guidance fragments for the controlled-interpreter channel refusals.
const (
	ruleInterpreterShape     = "解释器 argv 形态不合法"
	guidanceInterpreterShape = "受控解释器通道只接受 [脚本路径, 参数…] 形态：argv 首项不得以 `-` 开头（`-c` / `-e` 这类内联代码一律拒）；没有配置键可以加。"
	ruleScriptOutside        = "解释器脚本不在允许区"
	guidanceScriptOutside    = "受控脚本必须落在当前 Agent 的 scripts/ 目录或工作区技能库 skills/ 内（代码常量），没有配置键可以加。"
)

// RunnerOptions 是 LocalRunner 的构造参数——键名逐字沿用 `basalt.yaml` 的既有配置键
// （§2.4 注 6：沿用已有契约键、不另造；输出预算的出口是 `budget.max_output_bytes`）。
// RunnerOptions mirrors the existing configuration keys verbatim.
type RunnerOptions struct {
	Root           string   // 工作区根（`.basalt`）：解释器脚本允许区 skills/ 相对它定位
	EnvPassthrough []string // shell.env_passthrough：透传给子进程的父进程变量名
	Interpreters   []string // shell.interpreters：受控解释器通道（形态与脚本范围由本包校验）
	MaxOutputBytes int      // budget.max_output_bytes：装配期上限；非正值按缺省 256 KB
	// ProgramDir 是 `<程序目录>` 根（真实路径）。程序目录可执行件的三项条件（14D
	// §2.2.2）都以它为基准：真实路径落在 `<ProgramDir>/runtime/**`、清单登记、保护生效。
	// 空值表示本实例没有程序目录（`go run` 的临时二进制）——三项条件恒不成立，这些
	// 可执行件只能走显式登记。
	// ProgramDir anchors the three-condition admission for program-directory executables.
	ProgramDir string
	// ProgramManifest 是安装清单的登记项（仅在保护状态生效时装载）。空清单意味着
	// "没有自动放行面"：程序目录里的任何东西都要显式登记才能跑（fail-closed）。
	// ProgramManifest is the installation manifest; an empty manifest admits nothing.
	ProgramManifest []ProgramDirEntry
}

// LocalRunner 是 v1 唯一的受控执行实现（技术方案 §4.3 授权矩阵的**执行侧**）：
// 授权已由 `WhitelistPolicy` 判过，本层只管"怎么跑"——环境白名单、整树终止、读取中
// 限流、解释器形态校验、程序目录三项条件复核。它不做失败分类：非零退出码与输出原样
// 交给调用方。
// The only guarded execution implementation in v1; authorization has already been decided
// by the policy layer, so this layer owns only *how* the command runs.
type LocalRunner struct {
	root           string
	envPassthrough []string
	interpreters   map[string]struct{}
	maxOutputBytes int                    // 装配期上限（非正值按缺省）；逐 Run 收窄经 effectiveMaxOutputBytes
	programRuntime string                 // `<ProgramDir>/runtime`（空 = 未装配程序目录）
	programPaths   map[string]string      // 相对 runtime 的折叠路径（`/` 分隔）→ SHA-256
	programNames   map[string]struct{}    // 清单声明的可执行基名（折叠）——用于识别"按名进来的"
	programHashes  map[string]programHash // 内容哈希缓存（按真实路径）
	programMu      sync.Mutex             // 保护哈希缓存的并发读
}

// 编译期断言：调用方只依赖 `core.CommandRunner`，换实现只动装配处（宪法 VI）。
// Compile-time assertion that callers depend on the interface, not this type.
var _ core.CommandRunner = (*LocalRunner)(nil)

// NewLocalRunner 装配受控执行器；列表逐条建集合表（显式面，不做前缀 / 通配推断）。
// 上限按**装配值原样**存下（含非正值），生效值逐次执行时由 effectiveMaxOutputBytes 算。
// NewLocalRunner wires the runner; the lists become explicit lookup tables.
func NewLocalRunner(opts RunnerOptions) *LocalRunner {
	interpreters := make(map[string]struct{}, len(opts.Interpreters))
	for _, name := range opts.Interpreters {
		interpreters[name] = struct{}{}
	}
	runner := &LocalRunner{
		root:           filepath.Clean(opts.Root),
		envPassthrough: slices.Clone(opts.EnvPassthrough),
		interpreters:   interpreters,
		maxOutputBytes: opts.MaxOutputBytes,
		programPaths:   make(map[string]string, len(opts.ProgramManifest)),
		programNames:   make(map[string]struct{}, len(opts.ProgramManifest)),
		programHashes:  make(map[string]programHash),
	}
	if opts.ProgramDir != "" {
		runner.programRuntime = filepath.Join(filepath.Clean(opts.ProgramDir), programRuntimeDirName)
	}
	for _, entry := range opts.ProgramManifest {
		key := foldCase(toSlashPath(entry.Path))
		runner.programPaths[key] = strings.ToLower(entry.SHA256)
		base := filepath.Base(filepath.FromSlash(entry.Path))
		runner.programNames[foldCase(strings.TrimSuffix(base, filepath.Ext(base)))] = struct{}{}
	}
	return runner
}

// effectiveMaxOutputBytes 算**本次执行**的读取侧上限：min(装配值、逐命令正值、256 KB 硬上限)。
// 逐命令值（`Command.MaxOutputBytes`）来自逐 Run 预算，**只许收窄**——它比装配值更长时装配值
// 说话（"更短才生效"是预算下传的固定口径）；两者都非正时回落缺省 256 KB（0 不得解释成"不限量"，
// 那等于把这条防线整个关掉）；装配值自身超硬上限同样夹回 256 KB。
// The per-command budget may only narrow the assembly value; the 256 KB ceiling binds either way.
func (r *LocalRunner) effectiveMaxOutputBytes(cmd core.Command) int {
	maxBytes := r.maxOutputBytes
	if maxBytes <= 0 || maxBytes > defaultMaxOutputBytes {
		maxBytes = defaultMaxOutputBytes
	}
	if cmd.MaxOutputBytes > 0 && cmd.MaxOutputBytes < maxBytes {
		maxBytes = cmd.MaxOutputBytes
	}
	return maxBytes
}

// Run 执行一次受控命令：形态校验 → 组装白名单环境 → 起进程 → 等退出（或按 ctx 取消
// 整树终止）→ 收口输出与句柄。**非零退出码不是错误**——它是子进程的正常产出形态，
// 退出码连同合并后的输出一并交给调用方判读（失败分类归工具层）。
// Run executes one guarded command; a non-zero exit code is a result, not an error.
func (r *LocalRunner) Run(ctx context.Context, cmd core.Command) (core.CommandResult, error) {
	if err := r.checkShellName(cmd); err != nil {
		return core.CommandResult{}, err
	}
	if err := r.checkProgramDir(cmd); err != nil {
		return core.CommandResult{}, err
	}
	if err := r.checkInterpreter(cmd); err != nil {
		return core.CommandResult{}, err
	}
	// 起进程之前先判 ctx：父上下文已取消 / 已超时就不起子进程，取消在这里零副作用。
	if err := ctx.Err(); err != nil {
		return core.CommandResult{}, err
	}
	env, err := r.childEnv(cmd)
	if err != nil {
		return core.CommandResult{}, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return core.CommandResult{}, fmt.Errorf("创建输出管道：%w", err)
	}
	proc, spawnErr := spawnGuarded(spawnRequest{
		executable: cmd.Executable,
		args:       cmd.Args,
		dir:        cmd.Dir,
		env:        env,
		output:     writer,
	})
	if spawnErr != nil {
		// 起进程失败时写端仍在我们手上（spawnGuarded 失败路径不接管它）：两端都关掉。
		return core.CommandResult{}, errors.Join(
			fmt.Errorf("启动子进程 %s：%w", cmd.Executable, spawnErr),
			closePipe("输出管道（读端）", reader),
			closePipe("输出管道（写端）", writer),
		)
	}
	// 自家这份写端副本立刻关：EOF 的判据是"所有写端都关了"，留着它读者永远等不到 EOF。
	if err := closePipe("输出管道（写端）", writer); err != nil {
		// 关不掉自家写端 = 读端可能永远收不到 EOF：整树终止后收口，如实上抛。
		return core.CommandResult{}, errors.Join(err, proc.killTree(), proc.close())
	}

	maxBytes := r.effectiveMaxOutputBytes(cmd)
	readCh := make(chan readOutcome, 1)
	go func() { readCh <- readOutput(reader, maxBytes) }()
	waitCh := make(chan waitOutcome, 1)
	go func() {
		code, waitErr := proc.wait()
		waitCh <- waitOutcome{code: code, err: waitErr}
	}()

	var waited waitOutcome
	select {
	case waited = <-waitCh:
	case <-ctx.Done():
		// 取消与"进程恰好跑完"可能同时成立：再探一次 waitCh，让"真的跑完了"优先——
		// 已回收的进程没必要再杀，也不该把一次正常完成记成取消。
		select {
		case waited = <-waitCh:
		default:
			// 超时 / 取消 → **整树**终止（不是 TerminateProcess / 单杀进程）。
			killErr := proc.killTree()
			if killErr != nil {
				// 连整树终止都失败：不能再等 wait（可能永挂），把已在手上的错误一起上抛。
				return core.CommandResult{}, errors.Join(
					ctx.Err(), killErr,
					proc.close(), closePipe("输出管道（读端）", reader),
				)
			}
			// 被杀死的进程必然以非零退出码 / 被信号结束——那个错误由**我们**制造，按设计
			// 折进返回值而不是丢弃：调用方用 errors.Is 仍能判出取消 / 截止（分类口径不变），
			// 同时保留"进程是怎么结束的"这条诊断信息。这一等是必要的：回收完成前关句柄
			// 会让平台侧的等待落在已关闭的句柄上。
			killed := <-waitCh
			return core.CommandResult{}, errors.Join(
				ctx.Err(), killed.err,
				proc.close(), closePipe("输出管道（读端）", reader),
			)
		}
	}

	// 读者收口：正常路径（子进程与全部后代都放开了写端）此时已收到 EOF 并交卷；
	// 兜底分支见 pipeDrainGrace 的说明。
	var outcome readOutcome
	select {
	case outcome = <-readCh:
	case <-time.After(pipeDrainGrace):
		return core.CommandResult{}, errors.Join(
			fmt.Errorf("子进程 %s 的输出管道在收尾时限内未收口：仍有后代进程持有写端", cmd.Executable),
			proc.close(), closePipe("输出管道（读端）", reader),
		)
	}

	// 收尾顺序固定：先关进程 / 线程 / Job 句柄（Windows 侧关 Job 会把仍挂在 Job 里的
	// 残留后代一并清掉），再关读端——顺序反了会在"读干净之前"把管道拆掉。
	closeErr := proc.close()
	pipeErr := closePipe("输出管道（读端）", reader)
	if waited.err != nil {
		return core.CommandResult{}, errors.Join(
			fmt.Errorf("回收子进程 %s：%w", cmd.Executable, waited.err), closeErr, pipeErr,
		)
	}
	if err := errors.Join(closeErr, pipeErr, outcome.err); err != nil {
		return core.CommandResult{}, err
	}
	text := string(outcome.data)
	if outcome.truncated {
		text += core.TruncationMarker(maxBytes)
	}
	return core.CommandResult{Output: text, ExitCode: waited.code, Truncated: outcome.truncated}, nil
}

// spawnRequest 是 spawnGuarded 的平台无关入参（内部契约面）：可执行名 / argv / 工作目录 /
// 已过滤的环境 / 标准输出与标准错误合流的写端。写端**不归** spawnGuarded 关——它只是
// 拿来让子进程继承一份副本，释放责任留在调用方。
// spawnRequest is the platform-neutral input of spawnGuarded; the output writer stays
// owned by the caller.
type spawnRequest struct {
	executable string
	args       []string
	dir        string
	env        []string
	output     *os.File
}

// guardedProcess 是"可整树终止"的受控子进程契约面：两个平台文件各给一份实现，主流程
// 只经这三个方法操作——它不感知 Job 对象 / 进程组的存在。
// guardedProcess is the platform-neutral view of a spawnable child; the main flow never
// sees Job objects or process groups.
type guardedProcess interface {
	// wait 阻塞至子进程退出并回收之，返回退出码；**只有回收本身失败**才返回错误
	//（非零退出码是正常产出，不是错误）。
	wait() (int, error)
	// killTree 终止**整棵**进程树，不是单杀子进程。
	killTree() error
	// close 释放进程 / 线程 / 平台句柄；Windows 侧关 Job 会连带清掉残留后代。
	close() error
}

// waitOutcome 是一次子进程回收的产出（等待 goroutine 经 channel 交给主流程）。
// waitOutcome is what the reaping goroutine hands back to the main flow.
type waitOutcome struct {
	code int
	err  error
}

// readOutcome 是一次输出读取的产出（读者 goroutine 经 channel 交给主流程）。
// readOutcome is what the reader goroutine hands back to the main flow.
type readOutcome struct {
	data      []byte
	truncated bool
	err       error
}

// mergeChildEnv 组装子进程的环境变量块：**只**透传白名单命中的父进程变量，加上调用方
// 显式声明的变量；同名时以**声明**为准（显式给的值优先于环境里的老值）。返回值**必须**是
// 非 nil 切片——平台侧在 env 为空时都会回落到"继承整个父进程环境"，空切片才表达"一个都不
// 透传"。声明项按键名排序输出：`cmd.Env` 的 map 形态丢掉了输入顺序，排序让结果仍可复现；
// 被声明覆盖的透传名整条跳过，因此输出里不存在同名重复项（Windows 的"先出现为准"不受影响）。
// mergeChildEnv builds the child environment: allowlisted parent variables plus
// explicitly declared ones, with declarations winning on collisions. The result is
// never nil, because an empty slice is what expresses "inherit nothing".
func mergeChildEnv(passthrough []string, declared map[string]string) []string {
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	slices.Sort(names)
	env := make([]string, 0, len(passthrough)+len(names))
	for _, name := range passthrough {
		if _, overridden := declared[name]; overridden {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	for _, name := range names {
		env = append(env, name+"="+declared[name])
	}
	return env
}

// childEnv 把工具声明的 KEY=VALUE 条目解析成声明表后交给 mergeChildEnv——本函数只做
// 形态校验（缺 `=` / 空键即拒），合并语义只有 mergeChildEnv 一处实现（shell 与 MCP 共用）。
// childEnv validates the tool-declared KEY=VALUE entries and delegates the merge.
func (r *LocalRunner) childEnv(cmd core.Command) ([]string, error) {
	declared := make(map[string]string, len(cmd.Env))
	for _, entry := range cmd.Env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("工具声明的环境变量 %q 不是 KEY=VALUE 形态", entry)
		}
		declared[name] = value
	}
	return mergeChildEnv(r.envPassthrough, declared), nil
}

// checkInterpreter 是解释器受控通道的**执行侧**校验（§2.2 约束 17 / 流程 F ⑤）：授权面
// 只看 executable 整串（`Action` 没有 argv 字段），argv 形态与脚本真实路径都在这一道判。
// 命中解释器名单才判；一般白名单命令不走这里。
// Interpreter argv shape and script-path validation live here, on the execution side:
// the authorization face sees only the executable string, never the argv.
func (r *LocalRunner) checkInterpreter(cmd core.Command) error {
	if _, ok := r.interpreters[cmd.Executable]; !ok {
		return nil
	}
	if len(cmd.Args) == 0 {
		return deny(ruleInterpreterShape, "（无参数）", guidanceInterpreterShape)
	}
	script := cmd.Args[0]
	// argv 首项必须是**脚本路径**：`-c` / `-e` 这类内联代码形态一律拒（放行它们等于
	// 把解释器变成不受 scripts/ 只读区约束的任意代码通道）。
	if strings.HasPrefix(script, "-") {
		return deny(ruleInterpreterShape, script, guidanceInterpreterShape)
	}
	real, err := resolveRealPath(cmd.Dir, script)
	if err != nil {
		return deny(ruleScriptOutside, script, guidanceScriptOutside)
	}
	for _, dir := range r.scriptRoots(cmd) {
		if withinDir(real, dir) {
			return nil
		}
	}
	return deny(ruleScriptOutside, script, guidanceScriptOutside)
}

// checkShellName 是**执行侧**的同一道能力上限（14D §2.2.5 / D7）：授权面按命令名字面判过
// 之后，这里再按**解析后的真实路径**判一次。两层都判不是重复——授权面防的是配置错误
// （`allowed_commands: [bash]`），执行面防的是"名字换了马甲"：`helper -> /bin/zsh` 这种软链接
// 在字面比对下是完全不同的名字，只有投影到真实路径才看得见 basename 仍是 shell。
// The execution-side half of the same ceiling: after literal authorization, judge the
// resolved real path, because a symlink named `helper` is a different name literally.
func (r *LocalRunner) checkShellName(cmd core.Command) error {
	if core.IsShellName(cmd.Executable) {
		return deny(ruleShellName, cmd.Executable, guidanceShellName)
	}
	// 只用**解析成功**的结果判：解析不出来时 spawn 同样会失败，让执行错误如实归因，
	// 不把"没找到"改写成"策略拒绝"（契约 C-06 的同一口径）。
	// Judge only a resolved path; an unresolvable one fails at spawn and is attributed there.
	if real, err := r.resolveExecutable(cmd); err == nil && core.IsShellName(real) {
		return deny(ruleShellName, cmd.Executable, guidanceShellName)
	}
	return nil
}

// resolveExecutable 取子进程**实际会执行**的路径：裸名走 PATH 查找（Unix `exec.Command`
// 与 Windows `CreateProcess` 都按 PATH 找），带分隔符的相对入口按**子进程**工作目录解析，
// 最后投影到真实路径。投影失败原样上抛，由调用方决定是否据此拒绝。
// resolveExecutable returns the path the child will actually run, resolved to its real
// target; relative entries resolve against the child's working directory, not ours.
func (r *LocalRunner) resolveExecutable(cmd core.Command) (string, error) {
	exe := cmd.Executable
	if !strings.ContainsAny(exe, `/\`) {
		found, err := exec.LookPath(exe)
		if err != nil {
			return "", err
		}
		exe = found
	}
	return resolveRealPath(cmd.Dir, exe)
}

// scriptRoots 是受控脚本的两处允许区：当前 Agent 的 scripts/（有 Agent 上下文时）与
// 工作区技能库 skills/——与授权矩阵的读范围同一批目录，不另开口子。
// scriptRoots are the only two places a controlled script may live.
func (r *LocalRunner) scriptRoots(cmd core.Command) []string {
	roots := make([]string, 0, 2)
	if cmd.Dir != "" {
		roots = append(roots, filepath.Join(cmd.Dir, scriptsDirName))
	}
	return append(roots, filepath.Join(r.root, skillsDirName))
}

// readOutput 在读端上读取输出，**读取中限流**：先按上限读"上限 + 1"字节判超限，超限即
// 只留前 maxBytes 字节、余量读掉丢弃。丢弃不是可选动作——不读干会让还在写的子进程
// 阻塞在写端（限流不等于断流），内存也不该随输出规模膨胀。
// readOutput throttles while reading: only the first maxBytes are kept, and the rest is
// drained so a still-writing child never blocks on a full pipe.
func readOutput(reader io.Reader, maxBytes int) readOutcome {
	data, err := io.ReadAll(io.LimitReader(reader, int64(maxBytes)+1))
	if err != nil && !isBenignReadError(err) {
		return readOutcome{data: data, err: fmt.Errorf("读取子进程输出：%w", err)}
	}
	if len(data) <= maxBytes {
		return readOutcome{data: data}
	}
	outcome := readOutcome{data: data[:maxBytes], truncated: true}
	if _, drainErr := io.Copy(io.Discard, reader); drainErr != nil && !isBenignReadError(drainErr) {
		outcome.err = fmt.Errorf("丢弃超限输出：%w", drainErr)
	}
	return outcome
}

// isBenignReadError 判定"读取被打断"的两种**已知结束形态**：读到文件尾（io.EOF）与我方
// 关掉读端 / 整树终止（fs.ErrClosed）。H9 复核读数：Windows 侧 ERROR_BROKEN_PIPE 由运行时
// 映射为 io.EOF（internal/poll/fd_windows.go），关读端打断阻塞中的 Read 由 os.File 映射为
// fs.ErrClosed——故两个平台共用同一套判据，不需要平台专属的错误文件。
// isBenignReadError recognises the two known end-of-read shapes; both platforms map
// onto the same portable pair.
func isBenignReadError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, fs.ErrClosed)
}

// closePipe 关闭管道的一端。重复关闭（fs.ErrClosed）按预期形态放行而不是报错：收尾路径
// 会先"关读端解阻读者"再统一收口，同一端被关两次属于正常流程而非异常。
// closePipe tolerates a repeated close, which the shutdown paths legitimately produce.
func closePipe(name string, file *os.File) error {
	if err := file.Close(); err != nil && !errors.Is(err, fs.ErrClosed) {
		return fmt.Errorf("关闭%s：%w", name, err)
	}
	return nil
}
