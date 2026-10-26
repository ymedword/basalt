//go:build e2e

package e2e

// 本文件是进程级回填的编排层（模块卡 §3.1.1）：构建一次真实二进制，然后用临时工作
// 目录、显式环境变量与标准流驱动它。断言只读三处——进程输出、退出码、工作区里的
// SQLite 库——不调用任何包内函数。
// This file is the orchestration layer for the process-level backfill: it builds the
// real binary once and drives it through a temporary work directory, explicit
// environment variables and the standard streams. Assertions read three surfaces
// only: process output, exit code, and the SQLite database inside the workspace.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	// 判卷侧走裸连接读库（与 internal/app 的既有先例同一手法）。
	_ "modernc.org/sqlite"
)

const (
	// buildVersion 是构建期注入 main.version 的固定值：`basalt --version` 必须回显它，
	// 证明被测二进制确实是本次构建的产物（而不是 PATH 上碰巧同名的另一个）。
	// buildVersion is the build-time stamp; `basalt --version` must echo it.
	buildVersion = "us15-e2e"

	// defaultRunTimeout 是短命命令的缺省时限；常驻进程用 start + waitExit 的显式时限。
	// defaultRunTimeout bounds one-shot commands; long-lived processes carry their own.
	defaultRunTimeout = 120 * time.Second

	// candidateBinaryEnv / candidateSHA256Env 是发布档与被测二进制的两个接缝（模块卡
	// §4.3 `e2e` / §6.11 第 7 条）：前者给出候选二进制的绝对路径，后者可选、用于交叉
	// 核对重算出的 SHA-256。两者都只由 `make e2e` 导出，测试代码不另造变量。
	// The two seams between the release build and the binary under test: the absolute
	// candidate path and an optional hash for cross-checking.
	candidateBinaryEnv = "BASALT_E2E_BINARY"
	candidateSHA256Env = "BASALT_E2E_SHA256"

	// binarySourceCandidate 表示被测二进制是外部候选产物（发布证据的唯一合法来源）；
	// binarySourceTemp 表示本包临时构建（开发手跑），不得作为发布证据。
	// candidate marks an external release candidate; temp marks the package's own build.
	binarySourceCandidate = "candidate"
	binarySourceTemp      = "temp"

	// readinessPoll 是 waitHTTP 的轮询间隔——回环上起停，25ms 足够细。
	// readinessPoll is the readiness polling interval.
	readinessPoll = 25 * time.Millisecond
)

// binaryPath / binarySHA256 / binarySource / expectedVersion 由 TestMain 决定一次后写入；
// 测试进程内只读。被测二进制的哈希是回填记录 code.tested_binary_sha256 的唯一来源；
// expectedVersion 是本次二进制 `--version` 必须回显的构建标记。
// binaryPath, binarySHA256, binarySource and expectedVersion are settled once by TestMain
// and read-only afterwards; the hash is the sole source of tested_binary_sha256.
var (
	binaryPath      string
	binarySHA256    string
	binarySource    string
	expectedVersion string
)

// TestMain 构建被测二进制并打印唯一一条 `binary_sha256=` 标记。
// TestMain builds the binary under test and prints exactly one `binary_sha256=` marker.
//
// 三类短路必须先于构建：MCP helper 子进程是**同一个测试二进制**自举（它若也去构建，
// 会把 30 秒构建时间塞进每一次 stdio 会话，且 helper 的标准输出是 MCP 协议流——
// 任何杂散输出都会破坏协议）；shell / process_tree 用例的替身命令同样是本二进制自举
// （构建一次已是浪费，它的输出还要被逐字断言）；`-test.list` 是 stage lint 的只读预检，
// 只需列名。
// Three short-circuits run before the build: the MCP helper and the shell stand-in are
// this very test binary re-executed (their stdout is data under assertion), and
// `-test.list` only needs test names.
func TestMain(m *testing.M) {
	if mode, args, ok := shellHelperMode(); ok {
		os.Exit(runShellHelper(mode, args))
	}
	if os.Getenv("BASALT_MCP_HELPER") != "" || listMode() {
		os.Exit(m.Run())
	}

	root, err := repoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: 定位仓库根失败: %v\n", err)
		os.Exit(1)
	}
	path, source, sum, cleanup, err := resolveBinaryUnderTest(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: 解析被测二进制失败: %v\n", err)
		os.Exit(1)
	}
	stamp, err := expectedVersionFor(root, source)
	if err != nil {
		cleanup()
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	binaryPath, binarySource, binarySHA256, expectedVersion = path, source, sum, stamp
	// 标记打在测试框架输出之前：一次运行各恰好一条，供回填记录与 stage 判卷引用。
	// The markers precede all test output: exactly one of each per run.
	fmt.Printf("binary_source=%s\n", source)
	fmt.Printf("binary_path=%s\n", path)
	fmt.Printf("binary_sha256=%s\n", sum)

	code := m.Run()
	cleanup()
	os.Exit(code)
}

// resolveBinaryUnderTest 决定被测二进制：`BASALT_E2E_BINARY` 存在时**直接使用该文件、
// 不另行构建**；缺失时回退到本包临时构建。返回值里的 cleanup 只回收临时构建目录，
// 候选分支为空操作（候选文件归调用方，测试不得删它）。
// resolveBinaryUnderTest prefers the candidate named by BASALT_E2E_BINARY and never
// rebuilds it; without the variable it falls back to the package's own temporary build.
// The cleanup only reclaims the temporary build directory.
func resolveBinaryUnderTest(root string) (path, source, sum string, cleanup func(), err error) {
	if candidate := os.Getenv(candidateBinaryEnv); candidate != "" {
		path, sum, err = checkCandidateBinary(candidate)
		if err != nil {
			return "", "", "", nil, err
		}
		return path, binarySourceCandidate, sum, func() {}, nil
	}

	dir, err := os.MkdirTemp("", "basalt-e2e-build-*")
	if err != nil {
		return "", "", "", nil, fmt.Errorf("创建构建目录: %w", err)
	}
	path, err = buildBinary(root, dir)
	if err != nil {
		os.RemoveAll(dir)
		return "", "", "", nil, fmt.Errorf("构建被测二进制: %w", err)
	}
	sum, err = fileSHA256(path)
	if err != nil {
		os.RemoveAll(dir)
		return "", "", "", nil, fmt.Errorf("计算被测二进制哈希: %w", err)
	}
	return path, binarySourceTemp, sum, func() { os.RemoveAll(dir) }, nil
}

// checkCandidateBinary 校验候选二进制并**重算**它的 SHA-256：路径必须是绝对路径、
// 目标是普通文件；设置了 `BASALT_E2E_SHA256` 时还必须与重算值一致。缺文件、非绝对
// 路径、哈希不符一律失败——发布证据不允许"大致对得上"。
// checkCandidateBinary validates the candidate and recomputes its SHA-256; a missing
// file, a relative path or a hash mismatch is a hard failure, never a warning.
func checkCandidateBinary(candidate string) (path, sum string, err error) {
	if !filepath.IsAbs(candidate) {
		return "", "", fmt.Errorf("%s 必须是绝对路径（实际 %q）", candidateBinaryEnv, candidate)
	}
	info, err := os.Stat(candidate)
	if err != nil {
		return "", "", fmt.Errorf("候选二进制不可用（%s）: %w", candidate, err)
	}
	if !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("候选二进制不是普通文件（%s 的模式 %s）", candidate, info.Mode())
	}
	sum, err = fileSHA256(candidate)
	if err != nil {
		return "", "", fmt.Errorf("计算候选二进制哈希: %w", err)
	}
	if want := os.Getenv(candidateSHA256Env); want != "" && !strings.EqualFold(want, sum) {
		return "", "", fmt.Errorf("候选二进制哈希不符：%s 声明 %s，实际 %s", candidateSHA256Env, want, sum)
	}
	return candidate, sum, nil
}

// expectedVersionFor 决定 `--version` 必须回显的构建标记。临时构建是本包注入的
// `buildVersion`；候选二进制由 `make build` 按 `git describe --tags --always` 注入，
// 因此期望值取同一份 git 描述——解析不出来即失败（不跳过断言，也不放宽为"非空即可"）。
// expectedVersionFor derives the version stamp `--version` must echo: the injected
// constant for a temporary build, the checkout's `git describe` for a candidate. When
// the description cannot be resolved the run fails closed instead of skipping.
func expectedVersionFor(root, source string) (string, error) {
	if source != binarySourceCandidate {
		return buildVersion, nil
	}
	cmd := exec.Command("git", "describe", "--tags", "--always")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("候选二进制的版本标记无法核对（git describe --tags --always）: %w（%s）",
			err, strings.TrimSpace(stderr.String()))
	}
	stamp := strings.TrimSpace(string(out))
	if stamp == "" {
		return "", errors.New("候选二进制的版本标记为空：git describe --tags --always 没有输出")
	}
	return stamp, nil
}

// listMode 报告当前是否是 `go test -list` 的只读列名模式。
// listMode reports whether the binary runs in `go test -list` mode.
func listMode() bool {
	for _, arg := range os.Args[1:] {
		if arg == "-test.list" || strings.HasPrefix(arg, "-test.list=") {
			return true
		}
	}
	return false
}

// repoRoot 经 `go env GOMOD` 定位仓库根。
// repoRoot locates the repository root through `go env GOMOD`.
//
// 测试进程的工作目录是**包目录**（internal/e2e），不能直接 `./cmd/basalt`；
// GOMOD 指向当前模块的 go.mod，取父目录即是根。
// The test process runs in the package directory, so `./cmd/basalt` alone would
// not resolve; GOMOD names the module's go.mod and its parent is the root.
func repoRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Dir = ""
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull || filepath.Base(gomod) != "go.mod" {
		return "", fmt.Errorf("go env GOMOD 未指向模块文件（输出 %q）", gomod)
	}
	return filepath.Dir(gomod), nil
}

// buildBinary 以单二进制纪律构建被测程序：CGO 关闭、路径修剪、版本注入。
// buildBinary builds the binary under test under the single-binary discipline:
// CGO off, trimmed paths, injected version.
func buildBinary(root, dir string) (string, error) {
	name := "basalt"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-X main.version="+buildVersion,
		"-o", target, "./cmd/basalt")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("CGO_ENABLED=0 go build ./cmd/basalt: %w\n%s", err, out.String())
	}
	return target, nil
}

// fileSHA256 返回文件内容的十六进制摘要。
// fileSHA256 returns the hex digest of the file's content.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// harness 是一次回填场景的进程夹具。
// harness is the process fixture of one backfill scenario.
type harness struct {
	Root         string // 临时工作目录；二进制按 cwd 解析 basalt.yaml 与 .basalt/
	Binary       string // 被测二进制绝对路径
	BinarySHA256 string // 构建后计算，TestMain 打印 `binary_sha256=` 供回填记录引用

	runTimeout time.Duration
	mu         sync.Mutex
	secrets    []string

	// notifyChannels 是夹具登记的通知渠道（`insertNotifyChannel` 写入）：重命令启动前
	// 补给声明 `notify` 工具的 Agent——R4 起 Run 只冻结定义里声明过的渠道。
	// notifyChannels are the fixture-registered channels seeded into notify-capable agents.
	notifyChannels []fixtureChannel
}

// runResult 是一次短命命令的完整读数。
// runResult is the complete reading of one one-shot command.
type runResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// newHarness 建一个独立工作目录的夹具；二进制全包共享（TestMain 构建一次）。
// newHarness creates a fixture with its own work directory; the binary is shared.
func newHarness(t *testing.T) *harness {
	t.Helper()
	if binaryPath == "" {
		t.Fatal("被测二进制未构建——TestMain 未运行或构建失败")
	}
	return &harness{
		Root:         t.TempDir(),
		Binary:       binaryPath,
		BinarySHA256: binarySHA256,
		runTimeout:   defaultRunTimeout,
	}
}

// timeout 返回短命命令的时限；用例可经 setRunTimeout 放大（慢速场景）。
// timeout returns the one-shot command deadline; slow cases raise it explicitly.
func (h *harness) timeout() time.Duration {
	if h.runTimeout <= 0 {
		return defaultRunTimeout
	}
	return h.runTimeout
}

// setRunTimeout 调整短命命令的时限（只放大，不缩小——缩小会让慢机器上的正常命令误红）。
// setRunTimeout raises the one-shot deadline; it never shortens it.
func (h *harness) setRunTimeout(d time.Duration) {
	if d > h.runTimeout {
		h.runTimeout = d
	}
}

// environ 合并继承环境与显式覆盖项，并登记需要脱敏的值。
// environ merges the inherited environment with the explicit overrides and records
// the values that must be redacted from diagnostics.
//
// 未写进 extra 的变量原样继承：本机代理 / 临时目录等设置不该被夹具改写；凭证类覆盖
// 项的值只存在于子进程环境与内存，**不落盘、不进输出**。
// Variables not overridden are inherited unchanged; credential overrides live only
// in the child environment and memory.
func (h *harness) environ(extra []string) []string {
	merged := make(map[string]string, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		merged[key] = value
	}
	for _, kv := range extra {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			merged[kv] = ""
			continue
		}
		merged[key] = value
		h.registerSecret(key, value)
	}
	out := make([]string, 0, len(merged))
	for key, value := range merged {
		out = append(out, key+"="+value)
	}
	return out
}

// secretEnvKey 认定需要脱敏的变量名：凭证键与 URL（webhook 地址按凭证对待，
// 证据里只允许出现 scheme://host）。
// secretEnvKey names the variables whose values must never reach diagnostics:
// credential keys and URLs (webhook addresses are treated as credentials).
var secretEnvKey = regexp.MustCompile(`(?i)(_KEY|_TOKEN|_SECRET|_PASSWORD|_URL)$`)

func (h *harness) registerSecret(key, value string) {
	if value == "" || !secretEnvKey.MatchString(key) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, existing := range h.secrets {
		if existing == value {
			return
		}
	}
	h.secrets = append(h.secrets, value)
}

// redact 把已登记的敏感值替换为占位，供失败文案使用。
// redact scrubs registered secrets from diagnostics.
func (h *harness) redact(text string) string {
	h.mu.Lock()
	secrets := append([]string(nil), h.secrets...)
	h.mu.Unlock()
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "<已隐去>")
	}
	return text
}

// command 组装一条对被测二进制的调用：cwd = 工作目录，环境 = 继承 + 覆盖。
// 重命令启动前先补齐夹具的模型连接绑定（v3 准入要求 AGENT.md 自带 `resource_bindings`）。
// command assembles one invocation of the binary under test; heavy commands seed the
// model-connection bindings their hand-written definitions need first.
func (h *harness) command(t *testing.T, ctx context.Context, env []string, args ...string) *exec.Cmd {
	t.Helper()
	h.prepareHeavy(t, env, args)
	cmd := exec.CommandContext(ctx, h.Binary, args...)
	cmd.Dir = h.Root
	cmd.Env = h.environ(env)
	return cmd
}

// run 跑一条短命命令并返回输出与退出码；非退出码类错误（启动失败 / 超时）直接判红。
// run executes a one-shot command and returns its streams and exit code; start
// failures and timeouts fail the test immediately.
func (h *harness) run(t *testing.T, env []string, args ...string) runResult {
	t.Helper()
	return h.runInput(t, env, "", args...)
}

// runInput 同 run，但可向标准输入喂一段文本（空串 = 不喂，进程立即读到 EOF）。
// runInput is run with an optional stdin payload; an empty string means no input.
func (h *harness) runInput(t *testing.T, env []string, stdin string, args ...string) runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout())
	defer cancel()
	cmd := h.command(t, ctx, env, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := runResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		res.ExitCode = -1
	}
	if ctx.Err() != nil {
		t.Fatalf("命令 %q 超时（%s）\n%s", strings.Join(args, " "), h.timeout(), h.redact(res.Stdout+res.Stderr))
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("命令 %q 启动失败: %v\n%s", strings.Join(args, " "), err, h.redact(res.Stdout+res.Stderr))
	}
	return res
}

// process 是一次常驻进程调用的句柄。
// process is the handle of one long-lived invocation.
type process struct {
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	done   chan struct{}

	mu       sync.Mutex
	waitErr  error
	exitCode int
}

// start 拉起一条常驻进程；用例结束（含失败）时兜底杀掉，不留后台进程。
// start launches a long-lived process and registers a kill-on-cleanup fallback.
func (h *harness) start(t *testing.T, env []string, args ...string) *process {
	t.Helper()
	cmd := h.command(t, context.Background(), env, args...)
	p := &process{cmd: cmd, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.stdout, p.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 %q 失败: %v", strings.Join(args, " "), err)
	}
	go p.wait()
	t.Cleanup(p.kill)
	return p
}

// wait 在后台收尸——Wait 只能调用一次，所有读数在关闭 done 前落定。
// wait reaps the process once; every reading settles before done closes.
func (p *process) wait() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.waitErr = err
	if p.cmd.ProcessState != nil {
		p.exitCode = p.cmd.ProcessState.ExitCode()
	} else {
		p.exitCode = -1
	}
	p.mu.Unlock()
	close(p.done)
}

// output 返回进程到目前为止的标准输出与标准错误（仅在进程已退出后读取是安全的）。
// output returns the captured streams; read it only after the process has exited.
func (p *process) output() string {
	return "stdout:\n" + p.stdout.String() + "\nstderr:\n" + p.stderr.String()
}

// waitExit 等进程退出并返回退出码；超时先杀进程再判红。
// waitExit waits for the process and returns its exit code; a timeout kills it
// first and then fails the test.
func (p *process) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(timeout):
		p.kill()
		<-p.done
		t.Fatalf("进程在 %s 内未退出\n%s", timeout, p.output())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

// signal 向进程发送一个信号（Windows 只支持 Kill；信号场景本身是 linux tag）。
// signal sends sig to the process; Windows only supports Kill, and the signal
// scenarios themselves carry the linux tag.
func (p *process) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("进程已退出（%d），无法发送信号 %v", p.exitCodeValue(), sig)
	default:
	}
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("发送信号 %v 失败: %v", sig, err)
	}
}

// kill 杀掉进程；已退出时是幂等空操作（清理路径会重复调用）。
// kill terminates the process; it is a no-op once the process has exited.
func (p *process) kill() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
}

func (p *process) exitCodeValue() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

// waitHTTP 轮询一个回环地址直到它接受连接；进程提前退出或超时都判红。
// waitHTTP polls a loopback address until it answers; an early exit or a timeout
// fails the test.
func (p *process) waitHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			t.Fatalf("进程在 %s 就绪前退出（%d）\n%s", url, p.exitCodeValue(), p.output())
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		lastErr = err
		time.Sleep(readinessPoll)
	}
	p.kill()
	<-p.done
	t.Fatalf("等待 %s 就绪超时（%s，最后一次错误 %v）\n%s", url, timeout, lastErr, p.output())
}

// writeConfig 落一份 basalt.yaml（与 .basalt/ 同级）。
// writeConfig writes basalt.yaml next to the workspace directory.
func (h *harness) writeConfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(h.Root, "basalt.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写配置 %s 失败: %v", path, err)
	}
}

// writeAgent 落一份 AGENT.md：frontmatter 原样放进两条 `---` 之间，正文追加在后。
// 同时写一份 active 的生命周期状态文件——US-11（D-01 A）之后既有 Agent 基线一律为
// disabled，夹具必须先给出"运维已启用"的初始态，本套件才能继续判它原本要判的进程行为。
// writeAgent writes one AGENT.md: the frontmatter verbatim between the `---` fences,
// then the prompt body. It also seeds an active lifecycle state file, since US-11 baselines
// every pre-existing agent to disabled.
func (h *harness) writeAgent(t *testing.T, name, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(h.Root, ".basalt", "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 Agent 目录 %s 失败: %v", dir, err)
	}
	content := "---\n" + strings.TrimRight(frontmatter, "\n") + "\n---\n\n" + strings.TrimRight(body, "\n") + "\n"
	path := filepath.Join(dir, "AGENT.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 AGENT.md %s 失败: %v", name, err)
	}
	h.writeActiveLifecycleState(t, name)
	// 写盘即补绑定（当前指针已存在时）：用例常在启动前快照 AGENT.md，"启动失败不得改写
	// 定义"一类断言若等到重命令启动才由夹具补绑，会把夹具自己的写入误判成产品改写。
	// Seed at write time when the pointer already exists, so pre-start snapshots do not
	// mistake the fixture's own seeding for a rewrite by the process under test.
	h.seedBindings(t, path)
}

// seedBindings 给一份**夹具自有**的 AGENT.md 补 model_connection / notify_channel 绑定；
// 读不到当前发布指针（库未建、Provider 尚未发布）时返回 false，由 prepareHeavy 在重命令
// 启动前触发基线后重试。用例显式写出的绑定块与 `notify_mode:` 一律不动。
// seedBindings fills fixture-owned bindings from the current published pointers; when no
// pointer is readable yet it returns false and prepareHeavy retries after the warm-up.
func (h *harness) seedBindings(t *testing.T, path string) bool {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	doc, ok := parseBindingDoc(string(raw))
	if !ok || doc.Explicit {
		return false
	}
	var bindings []fixtureBinding
	if doc.Provider != "" {
		uid, found := h.modelConnectionUID(t, doc.Provider)
		if !found {
			return false
		}
		bindings = append(bindings, fixtureBinding{Kind: "model_connection", Name: doc.Provider, UID: uid})
	}
	var declared []string
	if doc.Notify {
		for _, channel := range h.fixtureChannels() {
			bindings = append(bindings, fixtureBinding{Kind: "notify_channel", Name: channel.name, UID: channel.uid})
			declared = append(declared, channel.name)
		}
	}
	if len(bindings) == 0 {
		return false
	}
	rewriteFixtureBindings(t, path, declared, bindings)
	return true
}

// lifecycleStateFile 是状态文件的**契约形态**（agents 契约 §2.2 的 v2）：字段顺序即
// 编码顺序——`agent_uid` 在 `agent` 之后、`state` 之前，checksum 覆盖整份字节。
// The on-disk state file shape in contract order; v2 carries agent_uid after agent.
type lifecycleStateFile struct {
	SchemaVersion int    `json:"schema_version"`
	Agent         string `json:"agent"`
	AgentUID      string `json:"agent_uid"`
	State         string `json:"state"`
	Generation    int64  `json:"generation"`
	UpdatedAt     int64  `json:"updated_at"`
	OperationID   string `json:"operation_id"`
	Checksum      string `json:"checksum"`
}

// fixtureAgentUID 生成一个契约合法的 Agent 身份（`ag_` + 26 位 Crockford Base32）：
// 夹具不 import internal 包，字符集在这里按契约字面量复制一份。
// fixtureAgentUID mints a contract-legal agent uid without importing internal packages.
func fixtureAgentUID(t *testing.T) string {
	t.Helper()
	return fixtureUID(t, "ag_")
}

// fixtureUID 生成 `<prefix>` + 26 位 Crockford Base32 的契约身份：Agent 用 `ag_`，
// 资源用 `rs_`（v3 起通知渠道以资源 UID 为主键）。
// fixtureUID mints a contract-legal identity with the given prefix.
func fixtureUID(t *testing.T, prefix string) string {
	t.Helper()
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var buf [26]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("生成身份随机源失败: %v", err)
	}
	out := make([]byte, 0, len(prefix)+len(buf))
	out = append(out, prefix...)
	for _, b := range buf {
		out = append(out, alphabet[int(b)%len(alphabet)])
	}
	return string(out)
}

// writeActiveLifecycleState 写一份 enabled 形态的状态文件（active / 代数 2）。
// writeActiveLifecycleState writes the active/generation-2 shape used by most fixtures.
func (h *harness) writeActiveLifecycleState(t *testing.T, name string) {
	t.Helper()
	// 代数 2 = "基线写入（1）之后的一次启用"，与真实启停路径留下的形态一致。
	h.writeLifecycleState(t, name, "active", 2)
}

// writeLifecycleState 按契约字面量写 `.basalt/control/agents/<name>.json`：紧凑
// JSON、固定字段顺序、checksum = "sha256:" + 十六进制（对 checksum 置空后的整份字节取
// sha256）。夹具直接按契约生成字节——本套件只测真实二进制，不 import internal 包来造状态。
// 状态与代数由调用方给定：14D / E5 需要"新建即停用"（disabled / 代数 1）的初始形态。
// The fixture speaks the contract's on-disk bytes literally: compact JSON in fixed field
// order with the documented checksum, generated without importing internal packages.
func (h *harness) writeLifecycleState(t *testing.T, name, state string, generation int64) {
	t.Helper()

	now := time.Now()
	record := lifecycleStateFile{
		// v3 库的准入要求状态文件给出稳定身份（缺 UID 的 v1 文件仍可读，但没有身份
		// 的 Agent 不能使用任何资源）；夹具因此直接写契约 v2 的形态。
		SchemaVersion: 2,
		Agent:         name,
		AgentUID:      fixtureAgentUID(t),
		State:         state,
		Generation:    generation,
		UpdatedAt:     now.UnixMilli(),
		OperationID:   fmt.Sprintf("op-e2e-%s-%d", name, now.UnixNano()),
	}
	unsigned, err := json.Marshal(record) // checksum 为空串，正与被哈希的形态一致
	if err != nil {
		t.Fatalf("编码生命周期状态 %s 失败: %v", name, err)
	}
	sum := sha256.Sum256(unsigned)
	record.Checksum = "sha256:" + hex.EncodeToString(sum[:])
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("编码生命周期状态 %s 失败: %v", name, err)
	}

	dir := filepath.Join(h.Root, ".basalt", "control", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建生命周期控制目录 %s 失败: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o644); err != nil {
		t.Fatalf("写生命周期状态 %s 失败: %v", name, err)
	}
	h.seedLifecycleOperation(t, record.OperationID, name, now.UnixMilli())
}

// seedLifecycleOperation 在库里补一条**真实存在**的启用操作行：v3 的
// `agent_lifecycle.operation_id` 外键指向 `management_operations`，而夹具伪造的状态
// 文件携带的 `op-...` 在真实工作区里本来就对应一条历史操作行（US-11 的 `agent.enable`）。
// 不补行时启动基线写索引会以 FOREIGN KEY 失败（只 WARN），索引缺身份后准入全部拒绝。
// 库尚未由 `init` 建立时跳过——调用方没有可运行的工作区，先建库再写 Agent。
// seedLifecycleOperation mirrors the historical enable row a pre-v3 state file points
// at; without it the v3 index write fails its foreign key and admission refuses.
func (h *harness) seedLifecycleOperation(t *testing.T, operationID, agent string, atMillis int64) {
	t.Helper()
	if _, err := os.Stat(h.dbPath()); errors.Is(err, os.ErrNotExist) {
		return
	} else if err != nil {
		t.Fatalf("检查库文件 %s 失败: %v", h.dbPath(), err)
	}
	db := openDB(t, h.dbPath())
	if _, err := db.Exec(
		`INSERT OR IGNORE INTO management_operations
		   (operation_id, action, target_kind, target_id, actor_subject, auth_method,
		    source, status, result_code, created_at, finished_at)
		 VALUES (?, 'agent.enable', 'agent', ?, 'cli:e2e-fixture', 'cli', 'cli',
		         'success', 'OK', ?, ?)`,
		operationID, agent, atMillis, atMillis); err != nil {
		t.Fatalf("补启用操作行 %s（Agent %s）失败: %v", operationID, agent, err)
	}
}

// dbPath 返回工作区里的库文件路径。
// dbPath returns the database file inside the workspace.
func (h *harness) dbPath() string {
	return filepath.Join(h.Root, ".basalt", "basalt.db")
}

// openDB 打开一条判卷侧连接；关闭失败也要判红（吞错会让临时目录清理掩盖问题）。
// openDB opens a verdict-side connection; a close failure fails the test too.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("打开库 %s 失败: %v", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("连接库 %s 失败: %v", path, err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭库 %s 失败: %v", path, err)
		}
	})
	return db
}

// queryRows 把查询结果读成按列名索引的行切片；BLOB 归一为字符串便于断言。
// queryRows reads a query into column-keyed rows; BLOBs normalize to strings.
func queryRows(t *testing.T, db *sql.DB, query string, args ...any) []map[string]any {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("查询失败 %q: %v", query, err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("关闭查询结果失败: %v", err)
		}
	}()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("读取列名失败: %v", err)
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("扫描行失败: %v", err)
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = normalizeValue(values[i])
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历查询结果失败: %v", err)
	}
	return out
}

// normalizeValue 把驱动返回值归一到断言友好的形态。
// normalizeValue folds driver values into assertion-friendly shapes.
func normalizeValue(value any) any {
	switch v := value.(type) {
	case []byte:
		return string(v)
	default:
		return v
	}
}

// —— US-12 管理写面夹具（模块卡 §3.1.9 / §4.4，T065）——
// US-12 admin write-surface fixtures. Every struct below mirrors exactly one
// published view from internal/web/views*.go; fields not asserted are omitted
// and ignored by encoding/json, so a view addition never breaks this file.

// adminFileView 是 AgentFileView 的判卷副本。
type adminFileView struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// adminLifecycleView 是 AgentLifecycleView 的判卷副本。
type adminLifecycleView struct {
	Agent          string  `json:"agent"`
	State          *string `json:"state"`
	Generation     int64   `json:"generation"`
	UpdatedAt      string  `json:"updated_at"`
	OperationID    string  `json:"operation_id"`
	Valid          bool    `json:"valid"`
	LifecycleError string  `json:"lifecycle_error"`
}

// adminFilesView 是 AgentFilesView 的判卷副本（写结果与读取同一形状）。
type adminFilesView struct {
	Agent     string             `json:"agent"`
	Lifecycle adminLifecycleView `json:"lifecycle"`
	ETag      string             `json:"etag"`
	Files     []adminFileView    `json:"files"`
}

// adminArchiveRow 是归档记录 AgentArchiveView 的判卷副本。
type adminArchiveRow struct {
	ArchiveID         string `json:"archive_id"`
	Agent             string `json:"agent"`
	ArchivedAt        string `json:"archived_at"`
	ContentHash       string `json:"content_hash"`
	RestoredAt        string `json:"restored_at"`
	RestoredAgentName string `json:"restored_agent_name"`
}

// adminVersionRow 是 ResourceVersionView 的判卷副本。
type adminVersionRow struct {
	VersionID             int64  `json:"version_id"`
	ResourceKind          string `json:"resource_kind"`
	ResourceID            string `json:"resource_id"`
	VersionNo             int    `json:"version_no"`
	ContentHash           string `json:"content_hash"`
	CreatedAt             string `json:"created_at"`
	OperationID           string `json:"operation_id"`
	Published             bool   `json:"published"`
	Applied               bool   `json:"applied"`
	RevertedFromVersionID *int64 `json:"reverted_from_version_id"`
}

// adminPublishResult 是 ResourcePublishResultView 的判卷副本。
type adminPublishResult struct {
	Version        adminVersionRow `json:"version"`
	PendingRestart bool            `json:"pending_restart"`
}

// adminProfileRow 是 /profiles 一行的判卷子集：写后要等的是"注册表是否已看见"。
type adminProfileRow struct {
	Name       string             `json:"name"`
	Registered bool               `json:"registered"`
	Lifecycle  adminLifecycleView `json:"lifecycle"`
	// Tools / NotifyChannels 是定义文件声明的投影：定义重载后它们随注册表一起变，
	// 可当作「新定义已生效」的可观测信号（引用视图不覆盖通知渠道）。
	// Tools / NotifyChannels project the definition file and follow registry reloads,
	// so they serve as an observable "new definition in effect" signal.
	Tools          []string `json:"tools"`
	NotifyChannels []string `json:"notify_channels"`
}

// adminWrite 发一次管理写请求；payload 为 nil 表示空请求体（与 webRequest 同一口径）。
// adminWrite issues one admin write; a nil payload means an empty body.
func adminWrite(t *testing.T, addr, method, path string, payload any, header map[string]string) *webResponse {
	t.Helper()
	body := ""
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("%s %s 编码请求体失败: %v", method, path, err)
		}
		body = string(raw)
	}
	return webRequest(t, addr, method, path, body, header)
}

// requireWriteOK 断言 200 + OK 信封；dst 非 nil 时继续解析 data。
// requireWriteOK asserts a 200 OK envelope and optionally decodes data.
func requireWriteOK(t *testing.T, resp *webResponse, dst any, label string) {
	t.Helper()
	requireWebStatus(t, resp, http.StatusOK, "OK", "", label)
	if dst != nil {
		decodeWebData(t, resp, dst, label)
	}
}

// requireLifecycle 断言一次生命周期读数的状态与代数。
// requireLifecycle asserts the state and generation of one lifecycle reading.
func requireLifecycle(t *testing.T, view adminLifecycleView, state string, generation int64, label string) {
	t.Helper()
	if view.State == nil || *view.State != state {
		t.Fatalf("%s：生命周期状态 = %v，期望 %q", label, view.State, state)
	}
	if view.Generation != generation {
		t.Fatalf("%s：generation = %d，期望 %d", label, view.Generation, generation)
	}
}

// readProfile 读一次 /profiles 并返回指定名字的行；不存在时 ok=false。
// readProfile reads /profiles once and returns the row for name, if present.
func readProfile(t *testing.T, addr, name string) (row adminProfileRow, ok bool) {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/profiles", "", nil)
	requireWebStatus(t, resp, http.StatusOK, "OK", "", "读取 profiles")
	var rows []adminProfileRow
	decodeWebData(t, resp, &rows, "读取 profiles")
	for _, item := range rows {
		if item.Name == name {
			return item, true
		}
	}
	return adminProfileRow{}, false
}

// waitProfile 轮询 /profiles 直到 ready 成立；超时报告最后一次读数。
// waitProfile polls /profiles until ready holds, reporting the last reading on timeout.
func waitProfile(t *testing.T, addr, name, what string, ready func(adminProfileRow, bool) bool) adminProfileRow {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var (
		last adminProfileRow
		ok   bool
	)
	for time.Now().Before(deadline) {
		last, ok = readProfile(t, addr, name)
		if ready(last, ok) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时：最后一次读数 %+v（存在=%v）", what, last, ok)
	return adminProfileRow{}
}

// waitAgentRegistered 等注册表看见并注册成功该 Agent（创建 / 复制 / 恢复后的
// 监听器去抖窗口）。
// waitAgentRegistered waits until the registry lists the agent as registered.
func waitAgentRegistered(t *testing.T, addr, name string) adminProfileRow {
	t.Helper()
	return waitProfile(t, addr, name, "Agent "+name+" 注册", func(row adminProfileRow, ok bool) bool {
		return ok && row.Registered
	})
}

// waitAgentGone 等该 Agent 从注册表消失（归档后目录移出、监听器重新装载）。
// waitAgentGone waits until the registry no longer lists the agent.
func waitAgentGone(t *testing.T, addr, name string) {
	t.Helper()
	waitProfile(t, addr, name, "Agent "+name+" 从注册表消失", func(_ adminProfileRow, ok bool) bool {
		return !ok
	})
}

// enableAgentRequest 发一次启用请求（两个期望字段都是必填指针）。
// enableAgentRequest sends one enable request; both expectation fields are required.
func enableAgentRequest(t *testing.T, addr, name string, generation int64) *webResponse {
	t.Helper()
	return adminWrite(t, addr, http.MethodPost, "/api/v1/agents/"+name+"/enable", map[string]any{
		"expected_state":      "disabled",
		"expected_generation": generation,
	}, nil)
}

// waitAgentEnabled 轮询启用：加载器尚未看见（404）与条目暂不可启用
// （409 AGENT_UNAVAILABLE）都是创建后的正常中间态；其它响应立即判红。
// waitAgentEnabled retries enable across the post-create watcher window; any
// other status fails immediately.
func waitAgentEnabled(t *testing.T, addr, name string, generation int64) adminLifecycleView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		resp := enableAgentRequest(t, addr, name, generation)
		last = fmt.Sprintf("HTTP %d：%s", resp.Status, resp.Raw)
		switch {
		case resp.Status == http.StatusOK:
			var view adminLifecycleView
			decodeWebData(t, resp, &view, "启用 Agent")
			requireLifecycle(t, view, "active", generation+1, "启用 Agent")
			return view
		case resp.Status == http.StatusNotFound:
			// 监听器去抖窗口内装载器还没有该条目。
		case resp.Status == http.StatusConflict && resp.Env.Code == "AGENT_UNAVAILABLE":
			// 已注册但引用资源尚未就绪。
		default:
			t.Fatalf("启用 Agent %s：%s", name, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待启用 Agent %s 超时：最后一次响应 %s", name, last)
	return adminLifecycleView{}
}

// adminAgentFiles 读受管文件集合，并断言响应头 ETag 与 data.etag 逐字一致（契约 D-16）。
// adminAgentFiles reads the managed file set and pins header ETag == data.etag.
func adminAgentFiles(t *testing.T, addr, name string) adminFilesView {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/agents/"+name+"/files", "", nil)
	var view adminFilesView
	requireWriteOK(t, resp, &view, "读取 Agent 文件集合")
	if got := resp.Header.Get("ETag"); got != view.ETag {
		t.Fatalf("读取 Agent 文件集合：响应头 ETag = %q，data.etag = %q", got, view.ETag)
	}
	if view.ETag == "" {
		t.Fatalf("读取 Agent 文件集合：data.etag 为空")
	}
	return view
}

// adminModelConnection 读一个模型连接：模板创建与提交正文都要用到发布指针的
// `resource_uid`（绑定必须携带 UID，按名解析是被禁止的）。
// adminModelConnection reads one connection so the fixtures can carry its uid.
func adminModelConnection(t *testing.T, addr, name string) adminModelConnectionView {
	t.Helper()
	resp := webRequest(t, addr, http.MethodGet, "/api/v1/model-connections/"+name, "", nil)
	var view adminModelConnectionView
	requireWriteOK(t, resp, &view, "读取模型连接 "+name)
	if view.ResourceUID == "" {
		t.Fatalf("读取模型连接 %s：resource_uid 为空（v3 起必须记录身份）", name)
	}
	return view
}

// bindingsBlock 渲染一段 `resource_bindings` 文本（四条空格缩进、与契约示例同形），
// 供手工拼 AGENT.md 的用例显式写出绑定——服务端不按名补绑。
// bindingsBlock renders one binding block for hand-written AGENT.md fixtures.
func bindingsBlock(kind, refName, uid string) string {
	return "resource_bindings:\n  " + kind + ":\n    - name: " + refName + "\n      uid: " + uid + "\n"
}

// heavyCommands 与 internal/app 的重命令清单同口径（组装层不 import internal 包，
// 这里按命令名复制一份：多一条只会多一次无害装配，少一条会让绑定漏补）。
// heavyCommands mirrors the assembly layer's heavy command list.
var heavyCommands = map[string]bool{
	"chat": true, "tool": true, "serve": true, "gateway": true, "secrets": true,
}

// isHeavyCommand 报告这条调用是否走完整运行期（跳过前导旗标取子命令名）。
// isHeavyCommand reports whether this invocation takes the full-runtime path.
func isHeavyCommand(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return heavyCommands[arg]
	}
	return false
}

// bindingMarker 标记夹具自动补写的绑定块：只有带这一行的块会被下一轮补绑整体替换；
// 用例显式写出的 `resource_bindings`（v3 与 US-12 用例的写法）一律不动。
// bindingMarker tags the fixture-owned bindings block; explicit blocks are left alone.
const bindingMarker = "  # e2e-fixture-bindings"

// fixtureBinding 是待写入 frontmatter 的一条资源绑定。
// fixtureBinding is one resource binding to render into a definition.
type fixtureBinding struct {
	Kind string
	Name string
	UID  string
}

// bindingDoc 是一个活动区 Agent 的绑定现状（只读扫描结果）。
// bindingDoc is the bindings state of one active agent definition.
type bindingDoc struct {
	Agent    string
	Path     string
	Provider string
	Notify   bool // frontmatter 的 tools 列声明了 notify
	Explicit bool // 用例自己写了 `resource_bindings`（没有夹具标记）
}

// prepareHeavy 在重命令启动前给手写的 AGENT.md 补齐 `resource_bindings`。v3 准入要求
// 定义显式声明绑定、且 UID 等于该名称的**当前指针**（agent-md 契约 §2.5「不自动补绑」）；
// 夹具在首次基线发布之前无从得知 `rs_` 身份，因此按运维的真实顺序分两段：先读指针，
// 读不到就跑一次 `tool list` 触发启动基线发布，再读指针并写回绑定块。通知渠道的绑定
// 取自 `insertNotifyChannel` 登记的渠道——它代替"运维在 AGENT.md 里声明渠道"这一步。
// prepareHeavy seeds the bindings v3 admission requires; hand-written fixtures cannot
// know the published `rs_` identity before the first baseline run.
func (h *harness) prepareHeavy(t *testing.T, env []string, args []string) {
	t.Helper()
	if !isHeavyCommand(args) {
		return
	}
	docs := h.bindingDocs(t)
	if len(docs) == 0 {
		return
	}

	seen := make(map[string]struct{}, len(docs))
	missing := false
	for _, doc := range docs {
		if doc.Provider == "" {
			continue
		}
		if _, done := seen[doc.Provider]; done {
			continue
		}
		seen[doc.Provider] = struct{}{}
		if _, ok := h.modelConnectionUID(t, doc.Provider); !ok {
			missing = true
		}
	}
	if missing {
		h.runBaselineWarmup(t, env)
	}

	// provider 仍未发布（配置里没有或基线失败）时 seedBindings 自动跳过：交给被测命令
	// 按原判据报错，夹具不替它掩盖。
	// A provider that is still unpublished is left alone so the command under test reports it.
	for _, doc := range docs {
		h.seedBindings(t, doc.Path)
	}
}

// publishBeforeSnapshot 在用例**快照定义文件之前**完成一次启动基线发布并补齐夹具绑定，
// 等价于运维的真实顺序（先发布 Provider，再在 AGENT.md 里写出当前指针身份）。没有它，
// "启动 / 调用不得改写定义"一类字节快照会把夹具自己的补绑误判成产品改写。
// publishBeforeSnapshot publishes the baseline and seeds fixture bindings before a test
// snapshots definition bytes, mirroring the operator's real order.
func (h *harness) publishBeforeSnapshot(t *testing.T, env []string) {
	t.Helper()
	h.runBaselineWarmup(t, env)
	for _, doc := range h.bindingDocs(t) {
		h.seedBindings(t, doc.Path)
	}
}

// bindingDocs 扫描活动区的 AGENT.md，读出 provider / notify 工具声明与绑定现状。
// 夹具 frontmatter 由本包按固定形态生成，这里只做逐行判定，不引入 YAML 依赖。
// bindingDocs scans the active definitions for provider refs, the notify tool and the
// bindings state; fixture frontmatter is line-oriented, so no YAML dependency is used.
func (h *harness) bindingDocs(t *testing.T) []bindingDoc {
	t.Helper()
	dir := filepath.Join(h.Root, ".basalt", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []bindingDoc
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name(), "AGENT.md")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		doc, ok := parseBindingDoc(string(raw))
		if !ok {
			continue
		}
		doc.Agent, doc.Path = entry.Name(), path
		out = append(out, doc)
	}
	return out
}

// parseBindingDoc 取 frontmatter 里的 provider.name、tools 是否含 notify，并区分
// 夹具写的绑定块（带标记，可重写）与用例写的绑定块（不动）。
// parseBindingDoc extracts provider.name and the notify tool, and tells fixture-owned
// bindings from explicit ones.
func parseBindingDoc(text string) (bindingDoc, bool) {
	if !strings.HasPrefix(text, "---\n") {
		return bindingDoc{}, false
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return bindingDoc{}, false
	}
	lines := strings.Split(rest[:end], "\n")
	var doc bindingDoc
	inProvider, inTools := false, false
	for i, line := range lines {
		if line == "resource_bindings:" {
			doc.Explicit = i+1 >= len(lines) || lines[i+1] != bindingMarker
			continue
		}
		if line == "notify_channels:" {
			// 带标记的是夹具上一轮写的块（可整体重写）；否则是用例自己的声明，不动。
			if i+1 >= len(lines) || lines[i+1] != bindingMarker {
				doc.Explicit = true
			}
			continue
		}
		if line == "notify_mode:" {
			doc.Explicit = true
			continue
		}
		switch {
		case line == "provider:":
			inProvider, inTools = true, false
			continue
		case line == "tools:":
			inProvider, inTools = false, true
			continue
		case !strings.HasPrefix(line, " "):
			inProvider, inTools = false, false
			continue
		}
		trimmed := strings.TrimSpace(line)
		if inProvider {
			if name, ok := strings.CutPrefix(trimmed, "name:"); ok {
				doc.Provider = strings.Trim(strings.TrimSpace(name), `"`)
				inProvider = false
			}
			continue
		}
		if inTools && trimmed == "- notify" {
			doc.Notify = true
		}
	}
	return doc, true
}

// fixtureChannel 是夹具登记的通知渠道（名字 + 资源身份）。
// fixtureChannel is one channel registered by the fixture.
type fixtureChannel struct{ name, uid string }

// recordNotifyChannel 登记夹具渠道：声明 `notify` 工具的 Agent 在下一次重命令前会
// 得到对应绑定（R4 起渠道必须显式声明，Run 只冻结声明过的渠道）。
// recordNotifyChannel registers a channel so notify-capable agents get its binding.
func (h *harness) recordNotifyChannel(name, uid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.notifyChannels {
		if h.notifyChannels[i].name == name {
			h.notifyChannels[i].uid = uid
			return
		}
	}
	h.notifyChannels = append(h.notifyChannels, fixtureChannel{name: name, uid: uid})
}

// fixtureChannels 返回已登记渠道的快照。
// fixtureChannels returns a snapshot of the registered channels.
func (h *harness) fixtureChannels() []fixtureChannel {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]fixtureChannel(nil), h.notifyChannels...)
}

// modelConnectionUID 读模型连接的当前发布指针身份；库未建、指针缺失或已删除都返回
// false（调用方先触发基线再重试）。
// modelConnectionUID reads the published pointer identity of one model connection.
func (h *harness) modelConnectionUID(t *testing.T, name string) (string, bool) {
	t.Helper()
	if _, err := os.Stat(h.dbPath()); err != nil {
		return "", false
	}
	db := openDB(t, h.dbPath())
	var uid string
	err := db.QueryRow(
		`SELECT resource_uid FROM resource_pointers
		  WHERE resource_kind = 'model_connection' AND resource_id = ? AND deleted_at IS NULL`,
		name).Scan(&uid)
	if err != nil || uid == "" {
		return "", false
	}
	return uid, true
}

// runBaselineWarmup 跑一次 `tool list` 触发启动基线：基线只补建发布版本与指针
// （内容未变则 no-op），不改 Agent 状态、不写管理操作行。输出与退出码刻意忽略——
// 这次调用只为了让指针存在，判卷交给随后的被测命令；失败（如锁被占用）同样不影响判卷。
// runBaselineWarmup publishes the baseline once; its output and exit code are ignored.
func (h *harness) runBaselineWarmup(t *testing.T, env []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), h.timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, h.Binary, "tool", "list")
	cmd.Dir = h.Root
	cmd.Env = h.environ(env)
	_ = cmd.Run()
}

// rewriteFixtureBindings 用一份完整块替换夹具上一轮写的块（闭合 `---` 之前）：
// 块外字节逐字保留，正文不受影响；内容未变则不落盘（避免无谓的 mtime 扰动）。
// 渠道名声明（`notify_channels`）与绑定名严格一一对应，两者必须同一轮写入。
// rewriteFixtureBindings replaces the fixture-owned block before the closing fence;
// the channel declaration and its bindings are always rewritten together.
func rewriteFixtureBindings(t *testing.T, path string, declared []string, bindings []fixtureBinding) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 补绑定失败: %v", path, err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---\n") {
		return
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return
	}
	front, tail := rest[:end], rest[end:]
	for _, key := range []string{"notify_channels:", "resource_bindings:"} {
		if marker := strings.Index(front, key+"\n"+bindingMarker+"\n"); marker >= 0 {
			front = strings.TrimRight(front[:marker], "\n")
		}
	}
	updated := "---\n" + front + "\n" + renderFixtureBindings(declared, bindings) + tail[1:]
	if updated == text {
		return
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("写回 %s 补绑定失败: %v", path, err)
	}
}

// renderFixtureBindings 按固定顺序渲染渠道声明与绑定块（model_connection 在前）。
// renderFixtureBindings renders the channel declaration and the bindings block.
func renderFixtureBindings(declared []string, bindings []fixtureBinding) string {
	var out strings.Builder
	if len(declared) > 0 {
		// 非空声明即 allowlist（契约 agent-md §2.1 规则 10 的兼容读取）。
		out.WriteString("notify_channels:\n" + bindingMarker + "\n")
		for _, name := range declared {
			out.WriteString("  - " + name + "\n")
		}
	}
	out.WriteString("resource_bindings:\n" + bindingMarker + "\n")
	for _, kind := range []string{"model_connection", "notify_channel"} {
		wrote := false
		for _, entry := range bindings {
			if entry.Kind != kind {
				continue
			}
			if !wrote {
				out.WriteString("  " + kind + ":\n")
				wrote = true
			}
			out.WriteString("    - name: " + entry.Name + "\n      uid: " + entry.UID + "\n")
		}
	}
	return out.String()
}

// readLifecycleStateFile 读 `.basalt/control/agents/<name>.json`（生命周期状态文件
// 是 generation 的唯一权威载体；写结果视图可能零值投影）。
// readLifecycleStateFile reads the on-disk lifecycle state, the generation authority.
func readLifecycleStateFile(t *testing.T, h *harness, name string) lifecycleStateFile {
	t.Helper()
	path := filepath.Join(h.Root, ".basalt", "control", "agents", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取生命周期状态文件 %s 失败: %v", path, err)
	}
	var state lifecycleStateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("解析生命周期状态文件 %s 失败: %v", path, err)
	}
	return state
}
