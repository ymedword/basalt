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
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

	// readinessPoll 是 waitHTTP 的轮询间隔——回环上起停，25ms 足够细。
	// readinessPoll is the readiness polling interval.
	readinessPoll = 25 * time.Millisecond
)

// binaryPath 与 binarySHA256 由 TestMain 构建一次后写入；测试进程内只读。
// 被测二进制的哈希是回填记录 code.tested_binary_sha256 的唯一来源。
// binaryPath and binarySHA256 are written once by TestMain and read-only afterwards;
// the hash is the sole source of the record's tested_binary_sha256.
var (
	binaryPath   string
	binarySHA256 string
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
	dir, err := os.MkdirTemp("", "basalt-e2e-build-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: 创建构建目录失败: %v\n", err)
		os.Exit(1)
	}
	path, err := buildBinary(root, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: 构建被测二进制失败: %v\n", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: 计算被测二进制哈希失败: %v\n", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	binaryPath, binarySHA256 = path, sum
	// 标记打在测试框架输出之前：一次运行恰好一条，供回填记录引用。
	// The marker precedes all test output: exactly one per run, referenced by records.
	fmt.Printf("binary_sha256=%s\n", sum)

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
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
// command assembles one invocation of the binary under test.
func (h *harness) command(ctx context.Context, env []string, args ...string) *exec.Cmd {
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
	cmd := h.command(ctx, env, args...)
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
	cmd := h.command(context.Background(), env, args...)
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
// writeAgent writes one AGENT.md: the frontmatter verbatim between the `---` fences,
// then the prompt body.
func (h *harness) writeAgent(t *testing.T, name, frontmatter, body string) {
	t.Helper()
	dir := filepath.Join(h.Root, ".basalt", "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 Agent 目录 %s 失败: %v", dir, err)
	}
	content := "---\n" + strings.TrimRight(frontmatter, "\n") + "\n---\n\n" + strings.TrimRight(body, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("写 AGENT.md %s 失败: %v", name, err)
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
