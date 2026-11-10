//go:build integration

package e2e

// 本文件是 US-25C 双实例 e2e 的**唯一**夹具与 TestMain（`//go:build integration`）。
// `internal/e2e` 里其余文件全部是 `//go:build e2e`，两种 tag 互不装载，因此这里
// **不得**引用任何 e2e 侧符号：凡是需要的助手都在本文件按 `us25c` 前缀复制一份。
// 这样 `-tags=integration` 的编译集是自洽的，也不会因为将来修改 e2e 夹具而静默
// 改变本套判据。
// This file is the sole fixture and TestMain of the US-25C two-instance e2e suite. Every
// file in this package is tagged `e2e`; the two tag sets never compile together, so no
// `e2e` symbol may be referenced here and every helper is cloned with a `us25c` prefix.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtemp"
)

const (
	// us25cBuildVersion 是测试二进制的构建版本戳（`-X main.version=`）。
	// The build-time version stamp of the candidate binary.
	us25cBuildVersion = "us15-e2e"

	// us25cDSNEnv / us25cAdminTokenEnv / us25cKeyEnv 是三个只经**进程环境**传递的
	// 输入：测试库 DSN、管理面口令、模型凭据占位符。DSN 与口令都**不得**落盘——
	// 配置文件里只写 `${VAR}` 占位。
	// Three inputs travel only through the process environment; the config file carries
	// `${VAR}` placeholders and never the values.
	us25cDSNEnv        = "BASALT_E2E_DSN"
	us25cAdminTokenEnv = "BASALT_E2E_ADMIN_TOKEN"
	us25cKeyEnv        = "BASALT_E2E_KEY"

	// us25cAdminToken 是管理面口令；与 e2e 侧同一字面量，但独立定义以免跨 tag 依赖。
	// The admin token, defined here so no e2e-tagged symbol is referenced.
	us25cAdminToken = "e2e-admin-token-not-a-secret"

	// us25cKeyValue 是模型凭据的占位值（值本身不是秘密）。
	// The placeholder credential value.
	us25cKeyValue = "us25c-not-a-secret"

	// us25cPoll 是轮询基础间隔。
	// The base polling interval.
	us25cPoll = 100 * time.Millisecond

	// us25cProviderName / us25cModelID 是配置里的静态 provider 三元组：启动基线按它
	// 发布模型连接，Agent 的 frontmatter 引用同一组名字。
	// The static provider triple the startup baseline publishes and agents reference.
	us25cProviderName = "us25c-provider"
	us25cModelID      = "us25c-model"
)

// us25cBinaryPath 由 TestMain 写入：一次构建，全部用例共用。
// The candidate binary path, written once by TestMain.
var us25cBinaryPath string

// TestMain 是 `internal/e2e` 在 `integration` tag 下的唯一入口：`-test.list` 时短路
// （不构建二进制），其余情况先构建候选二进制并打印**一行** `binary_sha256=` 证据，
// 再跑用例。
// TestMain short-circuits `-test.list`, otherwise builds the candidate binary once and
// prints the `binary_sha256=` evidence line before running the tests.
func TestMain(m *testing.M) {
	if us25cListMode(os.Args[1:]) {
		os.Exit(m.Run())
	}
	path, source, cleanup, err := us25cBuildCandidate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "构建 US-25C 测试二进制失败: %v\n", err)
		os.Exit(1)
	}
	us25cBinaryPath = path
	sum, err := us25cFileSHA256(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "计算测试二进制哈希失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("binary_source=%s\nbinary_path=%s\nbinary_sha256=%s\n", source, path, sum)
	code := m.Run()
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}

// us25cListMode 判断本次进程是否只是列举用例（`-test.list` / `--test.list`）。
// us25cListMode reports whether the process only lists tests.
func us25cListMode(args []string) bool {
	for _, arg := range args {
		if arg == "-test.list" || arg == "--test.list" ||
			strings.HasPrefix(arg, "-test.list=") || strings.HasPrefix(arg, "--test.list=") {
			return true
		}
	}
	return false
}

// us25cBuildCandidate 返回候选二进制：优先采信 `BASALT_E2E_BINARY`（外部候选构建），
// 否则在临时目录里用 `CGO_ENABLED=0` 构建一次。
// us25cBuildCandidate honours BASALT_E2E_BINARY or builds one candidate binary.
func us25cBuildCandidate() (path, source string, cleanup func(), err error) {
	if candidate := os.Getenv("BASALT_E2E_BINARY"); candidate != "" {
		if _, statErr := os.Stat(candidate); statErr != nil {
			return "", "", nil, fmt.Errorf("BASALT_E2E_BINARY 指向的候选不存在: %w", statErr)
		}
		return candidate, "candidate", nil, nil
	}
	root, err := us25cRepoRoot()
	if err != nil {
		return "", "", nil, err
	}
	dir, err := os.MkdirTemp("", "basalt-us25c-e2e-")
	if err != nil {
		return "", "", nil, fmt.Errorf("创建临时构建目录: %w", err)
	}
	// Windows 的 exec 只认 PATHEXT 内的扩展名：无 `.exe` 的候选二进制会被判
	// "executable file not found"，构建成功也执行不了。
	// Windows exec requires an executable extension; a bare `basalt` cannot run.
	name := "basalt"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	target := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-trimpath",
		"-ldflags", "-X main.version="+us25cBuildVersion,
		"-o", target, "./cmd/basalt")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=https://goproxy.cn")
	if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, fmt.Errorf("go build 失败: %w（输出 %s）", buildErr, strings.TrimSpace(string(out)))
	}
	return target, "built", func() { _ = os.RemoveAll(dir) }, nil
}

// us25cRepoRoot 定位仓库根：`go env GOMOD` 指向的 go.mod 所在目录。
// us25cRepoRoot locates the module root through `go env GOMOD`.
func us25cRepoRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	cmd.Env = append(os.Environ(), "GOPROXY=https://goproxy.cn")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD 失败: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("当前目录不在 Go module 内（go env GOMOD = %q）", gomod)
	}
	return filepath.Dir(gomod), nil
}

// us25cFileSHA256 计算文件哈希（证据行用）。
// us25cFileSHA256 hashes one file for the evidence line.
func us25cFileSHA256(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// us25cSchema 建一个隔离 schema，并在用例结束（LIFO）回收。
// us25cSchema provisions one isolated schema and drops it at test end.
func us25cSchema(t *testing.T) pgtemp.Schema {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	schema, err := pgtemp.New(ctx, "basalt_us25c_")
	if err != nil {
		t.Fatalf("建隔离 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer dropCancel()
		if err := schema.Drop(dropCtx); err != nil {
			t.Logf("回收 schema %s 失败: %v", schema.Name, err)
		}
	})
	return schema
}

// us25cEnv 组装子进程环境：去掉继承来的三个输入名，再显式写入本项目取值。
// 覆盖而不追加，避免宿主机上残留的同名变量改变判据。
// us25cEnv overwrites the three input names instead of appending to inherited ones.
func us25cEnv(t *testing.T, dsn string, extra map[string]string) []string {
	t.Helper()
	env := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case us25cDSNEnv, us25cAdminTokenEnv, us25cKeyEnv:
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		us25cDSNEnv+"="+dsn,
		us25cAdminTokenEnv+"="+us25cAdminToken,
		us25cKeyEnv+"="+us25cKeyValue,
	)
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	return env
}

// us25cWriteConfig 写 `<root>/basalt.yaml`：数据库段固定走 `${BASALT_E2E_DSN}` 占位，
// 其余段由调用方给出；未自带 `callers:` 时补一份服务入口限流预算（serve 必填）。
// us25cWriteConfig writes the instance config; the DSN stays a placeholder.
func us25cWriteConfig(t *testing.T, root, body string) {
	t.Helper()
	content := "database:\n  dsn: ${" + us25cDSNEnv + "}\n" + body
	if !strings.Contains(body, "callers:") {
		content += "\ncallers:\n  ingress:\n    run: {qps: 50, burst: 50}\n" +
			"    ingest: {qps: 50, burst: 50}\n    read: {qps: 50, burst: 50}\n"
	}
	path := filepath.Join(root, "basalt.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写配置 %s 失败: %v", path, err)
	}
}

// us25cConfigBody 渲染一份双角色共用的配置主体：一家指向回环桩的静态 provider（启动
// 基线据它发布模型连接）、worker 租约 6s / 续约 2s（故障用例的停止阈值 4s）、调度
// 100ms 轮询、资产同步 1s、策略刷新 1s。`addr` 为空时整段 `http:` 缺席——gateway
// 不启 HTTP。凭据只写 `${VAR}` 占位，真值由进程环境注入。
// us25cConfigBody renders the shared config body; an empty addr omits the http section.
func us25cConfigBody(addr, workerName, modelBaseURL string) string {
	var sb strings.Builder
	if addr != "" {
		sb.WriteString("http:\n  addr: " + addr + "\n  admin_auth:\n    mode: token\n    token: ${" + us25cAdminTokenEnv + "}\n")
	}
	sb.WriteString("providers:\n")
	sb.WriteString("  - name: " + us25cProviderName + "\n")
	sb.WriteString("    base_url: " + modelBaseURL + "\n")
	sb.WriteString("    api_key: ${" + us25cKeyEnv + "}\n")
	sb.WriteString("    default_model: " + us25cModelID + "\n")
	sb.WriteString("worker:\n")
	sb.WriteString("  name: " + workerName + "\n")
	sb.WriteString("  lease_ttl_seconds: 6\n")
	sb.WriteString("  renew_interval_seconds: 2\n")
	sb.WriteString("  reclaim_interval_seconds: 1\n")
	sb.WriteString("  reclaim_batch: 32\n")
	sb.WriteString("  heartbeat_interval_seconds: 2\n")
	sb.WriteString("  max_inflight: 8\n")
	sb.WriteString("  shutdown_grace_seconds: 45\n")
	sb.WriteString("scheduler:\n")
	sb.WriteString("  poll_interval_ms: 100\n")
	sb.WriteString("  dispatch_batch: 2\n")
	sb.WriteString("  missed_grace_seconds: 3600\n")
	sb.WriteString("assets:\n")
	sb.WriteString("  bundle_root: .basalt/assets\n")
	sb.WriteString("  sync_interval: 1s\n")
	sb.WriteString("  sync_timeout: 20s\n")
	sb.WriteString("  sync_batch: 50\n")
	sb.WriteString("  max_bundle_bytes: 16777216\n")
	sb.WriteString("  full_scan_every: 1\n")
	sb.WriteString("policy:\n")
	sb.WriteString("  sync_interval: 1s\n")
	sb.WriteString("  max_stale: 60s\n")
	return sb.String()
}

// us25cNewRoot 建一个工作区根：目录 + 配置 + `init` 骨架。gateway 必须显式 init
// ——它不跑 serve 的兜底初始化，缺 `.basalt/` 会在取实例锁时失败。
// us25cNewRoot provisions one workspace root; gateway needs the explicit init.
func us25cNewRoot(t *testing.T, env []string, addr, workerName, modelBaseURL string) string {
	t.Helper()
	root := t.TempDir()
	us25cWriteConfig(t, root, us25cConfigBody(addr, workerName, modelBaseURL))
	stdout, stderr, code := us25cRun(t, root, env, "init")
	if code != 0 {
		t.Fatalf("basalt init（%s）退出码 = %d，期望 0\nstdout:\n%s\nstderr:\n%s", root, code, stdout, stderr)
	}
	return root
}

// us25cMigrate 把共享测试库迁到当前版本：PostgreSQL 谱系的建表是独立一步，夹具在
// 直写任何运行期行（管理操作补行、判卷侧查询）之前必须补跑。
// us25cMigrate brings the shared test schema up to date before any fixture-side row write.
func us25cMigrate(t *testing.T, root string, env []string) {
	t.Helper()
	stdout, stderr, code := us25cRun(t, root, env, "db", "migrate")
	if code != 0 {
		t.Fatalf("basalt db migrate 退出码 = %d，期望 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
}

// us25cWaitModelConnection 轮询直到静态 provider 被启动基线发布出指针行，返回其
// 资源 UID（Agent 的绑定块要写它）。
// us25cWaitModelConnection waits for the baseline-published model connection.
func us25cWaitModelConnection(t *testing.T, db *sql.DB, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		uid, _ := us25cScalarString(t, db,
			`SELECT resource_uid FROM resource_pointers
			  WHERE resource_kind = 'model_connection' AND resource_id = $1 AND deleted_at IS NULL`, name)
		if uid != "" {
			return uid
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待模型连接 %s 发布超时（%s）", name, timeout)
	return ""
}

// us25cCopyAgentDir 把控制面工作区里的 Agent 目录逐文件复制到另一个工作区：它就是
// Compose 部署里打进 Worker 镜像的种子形态。Worker 侧的注册集来自本地目录，但运行
// 定义与技能一律取自已安装的发布包——本套用例要判的正是这两者分叉之后的行为。
// us25cCopyAgentDir copies the agent directory across roots, mirroring the seed archive.
func us25cCopyAgentDir(t *testing.T, fromRoot, toRoot, name string) {
	t.Helper()
	src := filepath.Join(fromRoot, ".basalt", "agents", name)
	dst := filepath.Join(toRoot, ".basalt", "agents", name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("建 Worker 侧 Agent 目录失败: %v", err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("读控制面 Agent 目录 %s 失败: %v", src, err)
	}
	copied := 0
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("Agent 目录 %s 含子目录 %s：本夹具只支持平铺定义文件", src, entry.Name())
		}
		raw, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), raw, 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", entry.Name(), err)
		}
		copied++
	}
	if copied == 0 {
		t.Fatalf("控制面 Agent 目录 %s 为空，没有可复制的定义", src)
	}
}

// us25cRun 跑一次 CLI 并保留双流与退出码。
// us25cRun runs one CLI invocation and keeps both streams and the exit code.
func us25cRun(t *testing.T, root string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(us25cBinaryPath, args...)
	cmd.Dir = root
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("执行 basalt %v 失败: %v", args, err)
		}
		code = exitErr.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// us25cLogSink 是**并发安全**的进程输出收集器：`bytes.Buffer` 直接当 Stdout 时，
// 进程仍在写、测试同时读会触发数据竞争甚至读出错乱内容。
// us25cLogSink is a mutex-guarded process output sink.
type us25cLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write 实现 io.Writer。
// Write implements io.Writer.
func (s *us25cLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String 返回当前累计输出。
// String returns everything written so far.
func (s *us25cLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// us25cProcess 是一个被拉起的实例进程。
// us25cProcess is one launched instance process.
type us25cProcess struct {
	cmd  *exec.Cmd
	log  *us25cLogSink
	done chan error
	once sync.Once
}

// us25cStart 在 root 下启动 `serve` / `gateway` 并登记清理。
// us25cStart launches serve or gateway in root and registers cleanup.
func us25cStart(t *testing.T, root string, env []string, args ...string) *us25cProcess {
	t.Helper()
	sink := &us25cLogSink{}
	cmd := exec.Command(us25cBinaryPath, args...)
	cmd.Dir = root
	cmd.Env = env
	cmd.Stdout = sink
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 basalt %v 失败: %v", args, err)
	}
	p := &us25cProcess{cmd: cmd, log: sink, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

// stop 先请进程优雅停机（SIGINT / os.Interrupt），有界等待后强杀。
// stop asks for a graceful shutdown first and kills after a bounded wait.
func (p *us25cProcess) stop(t *testing.T) {
	t.Helper()
	p.once.Do(func() {
		if p.cmd.Process == nil {
			return
		}
		_ = p.cmd.Process.Signal(os.Interrupt)
		select {
		case <-p.done:
			return
		case <-time.After(20 * time.Second):
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
}

// us25cLogs 返回当前输出快照。
// us25cLogs snapshots the process output.
func (p *us25cProcess) us25cLogs() string { return p.log.String() }

// us25cWaitLog 轮询进程输出直到包含 needle。
// us25cWaitLog polls the process output until it contains needle.
func us25cWaitLog(t *testing.T, p *us25cProcess, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.us25cLogs(), needle) {
			return
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待日志 %q 超时（%s）：\n%s", needle, timeout, p.us25cLogs())
}

// us25cWaitLogLine 轮询进程输出直到**某一行**同时包含全部 needles。整段匹配对
// `agent=<名字>` 这类碎片太弱——不同行各出现一次也会判过，所以要点名同行的用例
// 必须走这个变体。
// us25cWaitLogLine waits until one single line carries every needle.
func us25cWaitLogLine(t *testing.T, p *us25cProcess, timeout time.Duration, needles ...string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if us25cHasLineWith(p.us25cLogs(), needles) {
			return
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待同一行同时包含 %q 超时（%s）：\n%s", needles, timeout, p.us25cLogs())
}

// us25cHasLineWith 判断整段输出里是否存在一行同时包含全部 needles。
// us25cHasLineWith reports whether one line carries every needle.
func us25cHasLineWith(logs string, needles []string) bool {
	for _, line := range strings.Split(logs, "\n") {
		ok := true
		for _, needle := range needles {
			if !strings.Contains(line, needle) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// us25cWaitHealth 轮询 `/api/v1/health` 直到 200（管理面认证不拦健康检查）。
// us25cWaitHealth polls the health endpoint until it answers 200.
func us25cWaitHealth(t *testing.T, addr string, p *us25cProcess) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/api/v1/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			last = fmt.Sprintf("HTTP %d：%s", resp.StatusCode, string(body))
		} else {
			last = err.Error()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待健康检查超时：最后一次 %s\n进程输出：\n%s", last, p.us25cLogs())
}

// us25cFreeAddr 取一个回环空闲端口。
// us25cFreeAddr reserves then releases one loopback port.
func us25cFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取回环端口失败: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("释放回环端口失败: %v", err)
	}
	return addr
}

// us25cEnvelope 是统一信封的判卷侧形状。
// us25cEnvelope is the verdict-side envelope.
type us25cEnvelope struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// us25cResponse 是一次 HTTP 读数。
// us25cResponse records one HTTP reading.
type us25cResponse struct {
	Status  int
	Header  http.Header
	Raw     string
	Env     us25cEnvelope
	HasData bool
}

// us25cDo 发一次带管理面口令的请求。
// us25cDo issues one admin-authenticated request.
func us25cDo(t *testing.T, addr, method, path string, payload any, header map[string]string) *us25cResponse {
	t.Helper()
	body := ""
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("%s %s 编码请求体失败: %v", method, path, err)
		}
		body = string(raw)
	}
	req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("%s %s 构造请求失败: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+us25cAdminToken)
	for key, value := range header {
		req.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 90 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s 读响应失败: %v", method, path, err)
	}
	out := &us25cResponse{Status: resp.StatusCode, Header: resp.Header.Clone(), Raw: string(raw)}
	if err := json.Unmarshal(raw, &out.Env); err == nil {
		out.HasData = len(out.Env.Data) > 0 && string(out.Env.Data) != "null"
	}
	return out
}

// us25cRequireOK 断言 200 + OK，并按需解析 data。
// us25cRequireOK asserts a 200 OK envelope and optionally decodes data.
func us25cRequireOK(t *testing.T, resp *us25cResponse, dst any, label string) {
	t.Helper()
	if resp.Status != http.StatusOK || resp.Env.Code != "OK" {
		t.Fatalf("%s：HTTP %d / code %q，期望 200 / OK（信封 %s）", label, resp.Status, resp.Env.Code, resp.Raw)
	}
	if dst == nil {
		return
	}
	if !resp.HasData {
		t.Fatalf("%s：响应缺少 data：%s", label, resp.Raw)
	}
	if err := json.Unmarshal(resp.Env.Data, dst); err != nil {
		t.Fatalf("%s：data 不是预期 JSON：%v（原文 %s）", label, err, resp.Raw)
	}
}

// us25cFilesView 是 Agent 受管文件集合的判卷副本。
// us25cFilesView mirrors the managed-file view.
type us25cFilesView struct {
	Agent     string `json:"agent"`
	ETag      string `json:"etag"`
	Lifecycle struct {
		State      *string `json:"state"`
		Generation int64   `json:"generation"`
	} `json:"lifecycle"`
	Files []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	} `json:"files"`
	Publish *us25cPublishView `json:"publish,omitempty"`
}

// us25cPublishView 是内容写入接口的发布结果（US-25C 起随写入返回）。
// us25cPublishView mirrors the publish outcome carried by a content write.
type us25cPublishView struct {
	Status             string `json:"status"`
	PublishedVersionID int64  `json:"published_version_id"`
	Error              string `json:"error"`
}

// us25cCreateAgent 建一个 Agent，返回创建响应里的受管文件集合（含 ETag）。
// us25cCreateAgent creates one agent and returns its managed-file view.
func us25cCreateAgent(t *testing.T, addr, name, model string) us25cFilesView {
	t.Helper()
	var view us25cFilesView
	resp := us25cDo(t, addr, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":        name,
		"description": "US-25C e2e",
		"model":       model,
	}, nil)
	us25cRequireOK(t, resp, &view, "创建 Agent "+name)
	if view.ETag == "" {
		t.Fatalf("创建 Agent %s：etag 为空", name)
	}
	return view
}

// us25cAgentFiles 读受管文件集合，并核对响应头 ETag 与 data.etag 逐字相等。
// us25cAgentFiles pins header ETag == data.etag.
func us25cAgentFiles(t *testing.T, addr, name string) us25cFilesView {
	t.Helper()
	resp := us25cDo(t, addr, http.MethodGet, "/api/v1/agents/"+name+"/files", nil, nil)
	var view us25cFilesView
	us25cRequireOK(t, resp, &view, "读取 Agent "+name+" 的文件集合")
	if got := resp.Header.Get("ETag"); got != view.ETag {
		t.Fatalf("读取 Agent %s 文件集合：响应头 ETag = %q，data.etag = %q", name, got, view.ETag)
	}
	return view
}

// us25cSubmitDoc 提交 AGENT.md（条件写），并在服务端触发一次发布。
// us25cSubmitDoc submits AGENT.md and triggers the server-side publish.
// us25cSubmitDoc 提交 AGENT.md **并断言发布成功**：本套的每个用例都依赖
// "写入即发布"，发布失败会让后续断言（指针、载荷、同步）在错误的起点上展开。
// us25cSubmitDoc submits AGENT.md and asserts the publish succeeded.
func us25cSubmitDoc(t *testing.T, addr, name, doc string) us25cFilesView {
	t.Helper()
	before := us25cAgentFiles(t, addr, name)
	var view us25cFilesView
	resp := us25cDo(t, addr, http.MethodPost, "/api/v1/agents/"+name+"/files",
		map[string]any{"files": []map[string]string{{"path": "AGENT.md", "content": doc}}},
		map[string]string{"If-Match": before.ETag})
	us25cRequireOK(t, resp, &view, "提交 Agent "+name+" 的 AGENT.md")
	if view.Publish == nil {
		t.Fatalf("提交 Agent %s 的 AGENT.md：响应缺少 publish 结果：%s", name, resp.Raw)
	}
	if view.Publish.Status != "published" {
		t.Fatalf("提交 Agent %s 的 AGENT.md：publish.status=%q（期望 published），error=%q",
			name, view.Publish.Status, view.Publish.Error)
	}
	if view.Publish.PublishedVersionID == 0 {
		t.Fatalf("提交 Agent %s 的 AGENT.md：published_version_id 为 0", name)
	}
	return view
}

// us25cEnableAgent 启用 Agent（dis/1 → active/2），跨监听器去抖窗口重试。
// us25cEnableAgent enables one agent across the watcher debounce window.
func us25cEnableAgent(t *testing.T, addr, name string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		resp := us25cDo(t, addr, http.MethodPost, "/api/v1/agents/"+name+"/enable", map[string]any{
			"expected_state":      "disabled",
			"expected_generation": 1,
		}, nil)
		last = fmt.Sprintf("HTTP %d：%s", resp.Status, resp.Raw)
		switch {
		case resp.Status == http.StatusOK:
			return
		case resp.Status == http.StatusNotFound:
		case resp.Status == http.StatusConflict && resp.Env.Code == "AGENT_UNAVAILABLE":
		default:
			t.Fatalf("启用 Agent %s：%s", name, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待启用 Agent %s 超时：最后一次响应 %s", name, last)
}

// us25cWaitRegistered 轮询 `/api/v1/profiles` 直到该 Agent 被注册表看见。
// us25cWaitRegistered polls the profile list until the agent is registered.
func us25cWaitRegistered(t *testing.T, addr, name string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		resp := us25cDo(t, addr, http.MethodGet, "/api/v1/profiles", nil, nil)
		if resp.Status == http.StatusOK && resp.HasData {
			var rows []struct {
				Name       string `json:"name"`
				Registered bool   `json:"registered"`
			}
			if err := json.Unmarshal(resp.Env.Data, &rows); err == nil {
				for _, row := range rows {
					if row.Name == name && row.Registered {
						return
					}
				}
				last = "未列出或未注册"
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 Agent %s 注册超时：最后一次读数 %s", name, last)
}

// us25cAgentDoc 渲染一份最小 AGENT.md：一个 6 段 cron 任务、无工具、无 MCP，
// 正文携带版本标记（发布后模型请求里能读到的**实际配置值**）。
// us25cAgentDoc renders a minimal AGENT.md whose body carries the version marker.
func us25cAgentDoc(name, provider, model, modelUID, taskID, cron, marker string) string {
	return fmt.Sprintf(`---
name: %s
description: US-25C e2e
provider:
  name: %s
  model: %s
tools: []
mcp_servers: []
notify_mode: none
resource_bindings:
  model_connection:
    - name: %s
      uid: %s
  notify_channel: []
schedules:
  - id: %s
    cron: "%s"
    zone: UTC
    message: US-25C e2e 定时触发
---

%s
`, name, provider, model, provider, modelUID, taskID, cron, marker)
}

// us25cOpenPG 开一条判卷侧连接（直连测试库，不经被测进程）。DSN 只经内存传递。
// us25cOpenPG opens the verdict-side connection; the DSN stays in memory.
func us25cOpenPG(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开判卷侧数据库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// us25cScalarString 读一个字符串标量；无行时 ok=false。
// us25cScalarString reads one string scalar.
func us25cScalarString(t *testing.T, db *sql.DB, query string, args ...any) (string, bool) {
	t.Helper()
	var out sql.NullString
	err := db.QueryRow(query, args...).Scan(&out)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("查询标量失败: %v", err)
	}
	if !out.Valid {
		return "", true
	}
	return out.String, true
}

// us25cScalarInt 读一个整数标量；无行时 ok=false。
// us25cScalarInt reads one integer scalar.
func us25cScalarInt(t *testing.T, db *sql.DB, query string, args ...any) (int64, bool) {
	t.Helper()
	var out sql.NullInt64
	err := db.QueryRow(query, args...).Scan(&out)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		t.Fatalf("查询计数失败: %v", err)
	}
	if !out.Valid {
		return 0, true
	}
	return out.Int64, true
}

// us25cPointerHashQuery 取某个 Agent 已发布 bundle 指针上的哈希。
// us25cPointerHashQuery reads the published bundle hash of one agent.
const us25cPointerHashQuery = `
SELECT COALESCE(v.manifest_json::json->>'bundle_hash', '')
  FROM resource_pointers p
  JOIN resource_versions v ON v.id = p.published_version_id
 WHERE p.resource_kind = 'agent_bundle'
   AND p.deleted_at IS NULL
   AND v.manifest_json::json->>'agent' = $1`

// us25cPointerHash 读当前指针哈希；空串表示没有指针行。
// us25cPointerHash reads the current pointer hash; empty means no pointer row.
func us25cPointerHash(t *testing.T, db *sql.DB, agent string) string {
	t.Helper()
	out, _ := us25cScalarString(t, db, us25cPointerHashQuery, agent)
	return out
}

// us25cWaitPointer 轮询直到指针哈希满足 pred。
// us25cWaitPointer polls until the pointer hash satisfies pred.
func us25cWaitPointer(t *testing.T, db *sql.DB, agent, label string, timeout time.Duration,
	pred func(string) bool,
) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = us25cPointerHash(t, db, agent)
		if pred(last) {
			return last
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待 %s 超时（%s）：最后一次指针哈希 %q", label, timeout, last)
	return ""
}

// us25cTaskExists 判断任务的调度行是否存在。
// us25cTaskExists reports whether one scheduled task row exists.
func us25cTaskExists(t *testing.T, db *sql.DB, taskID string) bool {
	t.Helper()
	count, _ := us25cScalarInt(t, db,
		`SELECT count(*) FROM scheduled_tasks WHERE task_id = $1 AND retired_at IS NULL`, taskID)
	return count > 0
}

// us25cWaitTask 轮询直到任务的调度行出现（启用后由定义协调写入）。
// us25cWaitTask waits for the scheduled task row written by definition reconciliation.
func us25cWaitTask(t *testing.T, db *sql.DB, taskID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if us25cTaskExists(t, db, taskID) {
			return
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待任务 %s 的调度行超时（%s）", taskID, timeout)
}

// us25cArmTask 把任务的 next_fire_at 拨到数据库当前时间 + delay，并断言恰好命中
// 一行——命中零行通常意味着任务行还没写入，静默继续会让用例等到超时才报错。
// us25cArmTask arms one task and requires exactly one updated row.
func us25cArmTask(t *testing.T, db *sql.DB, taskID string, delay time.Duration) {
	t.Helper()
	res, err := db.Exec(
		`UPDATE scheduled_tasks
		    SET next_fire_at = (extract(epoch from clock_timestamp()) * 1000)::bigint + $2
		  WHERE task_id = $1 AND retired_at IS NULL`,
		taskID, delay.Milliseconds())
	if err != nil {
		t.Fatalf("上弦任务 %s 失败: %v", taskID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("读取上弦影响行数失败: %v", err)
	}
	if affected != 1 {
		t.Fatalf("上弦任务 %s 影响 %d 行，期望 1", taskID, affected)
	}
}

// us25cNextFireAt 读任务的 next_fire_at（毫秒）。
// us25cNextFireAt reads one task's next_fire_at.
func us25cNextFireAt(t *testing.T, db *sql.DB, taskID string) (int64, bool) {
	t.Helper()
	return us25cScalarInt(t, db,
		`SELECT next_fire_at FROM scheduled_tasks WHERE task_id = $1`, taskID)
}

// us25cExecution 是最近一条执行行的判卷读数。`ID` 是执行行主键（identity，永不复用），
// 租约表按它的十进制串寻址（`execution_leases.resource_id`），故障用例还要按它定点
// 复查同一行。
// us25cExecution is the execution row reading; its ID addresses the execution lease.
type us25cExecution struct {
	ID         int64
	PinnedHash string
	RunID      string
	Status     string
}

// us25cScanExecution 把一行 `(id, pinned_hash, run_id, status)` 读进结构体。
// us25cScanExecution scans one execution row projection.
func us25cScanExecution(t *testing.T, row *sql.Row) (us25cExecution, bool) {
	t.Helper()
	var (
		id     int64
		pinned sql.NullString
		runID  sql.NullString
		status sql.NullString
	)
	err := row.Scan(&id, &pinned, &runID, &status)
	if err == sql.ErrNoRows {
		return us25cExecution{}, false
	}
	if err != nil {
		t.Fatalf("读执行行失败: %v", err)
	}
	return us25cExecution{ID: id, PinnedHash: pinned.String, RunID: runID.String, Status: status.String}, true
}

// us25cLatestExecution 读某任务最近一条执行行。
// us25cLatestExecution reads the latest execution row of one task.
func us25cLatestExecution(t *testing.T, db *sql.DB, taskID string) (us25cExecution, bool) {
	t.Helper()
	return us25cScanExecution(t, db.QueryRow(
		`SELECT id, COALESCE(pinned_bundle_hash, ''), COALESCE(run_id, ''), status
		   FROM task_executions WHERE task_id = $1 ORDER BY id DESC LIMIT 1`, taskID))
}

// us25cExecutionByID 按主键读一条执行行（收尾后复查同一条）。
// us25cExecutionByID reads one execution row by primary key.
func us25cExecutionByID(t *testing.T, db *sql.DB, id int64) (us25cExecution, bool) {
	t.Helper()
	return us25cScanExecution(t, db.QueryRow(
		`SELECT id, COALESCE(pinned_bundle_hash, ''), COALESCE(run_id, ''), status
		   FROM task_executions WHERE id = $1`, id))
}

// us25cExecutionCount 统计任务的执行行数。
// us25cExecutionCount counts the execution rows of one task.
func us25cExecutionCount(t *testing.T, db *sql.DB, taskID string) int64 {
	t.Helper()
	count, _ := us25cScalarInt(t, db,
		`SELECT count(*) FROM task_executions WHERE task_id = $1`, taskID)
	return count
}

// us25cRunStatus 读一次 Run 的状态与原因。
// us25cRunStatus reads one run's status and reason.
func us25cRunStatus(t *testing.T, db *sql.DB, runID string) (status, reason string, ok bool) {
	t.Helper()
	var (
		s sql.NullString
		r sql.NullString
	)
	err := db.QueryRow(`SELECT status, COALESCE(reason, '') FROM runs WHERE run_id = $1`, runID).
		Scan(&s, &r)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("读 Run 状态失败: %v", err)
	}
	return s.String, r.String, true
}

// us25cWaitRunTerminal 轮询直到 Run 离开 running。
// us25cWaitRunTerminal waits until the run leaves the running state.
func us25cWaitRunTerminal(t *testing.T, db *sql.DB, runID string, timeout time.Duration) (string, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var status, reason string
	for time.Now().Before(deadline) {
		if s, r, ok := us25cRunStatus(t, db, runID); ok {
			status, reason = s, r
			if s != "running" {
				return status, reason
			}
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待 Run %s 进入终态超时（%s）：最后一次 %s / %s", runID, timeout, status, reason)
	return "", ""
}

// us25cStub 是一个**不受脚本步数限制**的 OpenAI 协议桩：记录每次请求的消息正文与
// 命中次数，可选地把响应挂起在一道闸门上（故障用例需要"调用在途"这个窗口）。
// `internal/testutil` 的 FakeOpenAIServer 在脚本用尽后判失败，本套要判的是调度与
// 恢复，不是脚本消费，所以自带一个最小桩。
// us25cStub is an unlimited recording OpenAI stub with an optional response gate.
type us25cStub struct {
	mu       sync.Mutex
	requests int
	hits     int
	contents []string
	gate     chan struct{}
	server   *httptest.Server
}

// us25cNewStub 起一个回环桩。
// us25cNewStub starts one loopback stub.
func us25cNewStub(t *testing.T) *us25cStub {
	t.Helper()
	stub := &us25cStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(stub.server.Close)
	return stub
}

// URL 返回桩的基地址（调用方再拼 `/v1`）。
// URL returns the stub base URL.
func (s *us25cStub) URL() string { return s.server.URL }

// Block 装上一道闸门：此后的响应会挂起，直到 release 或客户端断开。
// Block installs the response gate.
func (s *us25cStub) Block() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gate == nil {
		s.gate = make(chan struct{})
	}
}

// Release 放行挂起的响应（幂等）。
// Release opens the gate, idempotently.
func (s *us25cStub) Release() {
	s.mu.Lock()
	gate := s.gate
	s.gate = nil
	s.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// Hits 返回命中次数。
// Hits returns the number of completed requests.
func (s *us25cStub) Hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// Requests 返回到达次数（请求体读完即计数，闸门之前）——在途调用被取消时 Hits
// 不增长，判"没有重放"必须看到达次数而不是完成次数。
// Requests counts arrivals (incremented before the gate), so in-flight cancellations
// are visible; Hits alone cannot prove that absence.
func (s *us25cStub) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Contents 返回所有请求消息正文的拼接。
// Contents returns every request's message text concatenated.
func (s *us25cStub) Contents() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.contents, "\n")
}

// ContentsFrom 返回第 start 个请求起的正文拼接（start 由调用方在"上弦"之前用
// Requests() 取值）。判"这一次 Run 读到的是哪个版本的正文"必须按请求切片：整段
// 拼接里同时躺着上一轮的标记，包含判断会把旧版本误判成新版本。
// ContentsFrom returns the concatenation from request index start onwards.
func (s *us25cStub) ContentsFrom(start int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if start < 0 {
		start = 0
	}
	if start > len(s.contents) {
		start = len(s.contents)
	}
	return strings.Join(s.contents[start:], "\n")
}

// serve 处理一次 `/chat/completions`。
// serve answers one chat-completions request.
func (s *us25cStub) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "读请求体失败", http.StatusBadRequest)
		return
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &body)
	text := make([]string, 0, len(body.Messages))
	for _, msg := range body.Messages {
		text = append(text, msg.Content)
	}
	s.mu.Lock()
	s.requests++
	s.contents = append(s.contents, strings.Join(text, "\n"))
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	s.hits++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
}

// us25cProxy 是盖在测试库前面的一台**可切断** TCP 代理：切断时关闭已跟踪的连接并
// 拒绝新连接，恢复后照常转发。地址不变——重绑端口会让进程重连到别人的连接池状态，
// 判据就不再是"同一实例从数据库中断里恢复"。
// us25cProxy is a cuttable TCP proxy in front of the test database; the address is stable.
type us25cProxy struct {
	target string
	ln     net.Listener
	mu     sync.Mutex
	cut    bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

// us25cNewProxy 起一台指向 target 的代理。
// us25cNewProxy starts one proxy towards target.
func us25cNewProxy(t *testing.T, target string) *us25cProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("起数据库代理失败: %v", err)
	}
	proxy := &us25cProxy{target: target, ln: ln, conns: map[net.Conn]struct{}{}}
	proxy.wg.Add(1)
	go proxy.acceptLoop()
	t.Cleanup(func() {
		_ = ln.Close()
		proxy.Cut(true)
		proxy.wg.Wait()
	})
	return proxy
}

// Addr 返回代理监听地址。
// Addr returns the proxy listen address.
func (p *us25cProxy) Addr() string { return p.ln.Addr().String() }

// acceptLoop 接受连接并按切断标志决定转发或立即断开。
// acceptLoop forwards or drops connections depending on the cut flag.
func (p *us25cProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		cut := p.cut
		p.mu.Unlock()
		if cut {
			_ = client.Close()
			continue
		}
		upstream, err := net.DialTimeout("tcp", p.target, 10*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.track(client)
		p.track(upstream)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
			go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
			<-done
			p.untrack(client)
			p.untrack(upstream)
			_ = client.Close()
			_ = upstream.Close()
		}()
	}
}

// track 登记一条需要被切断的连接。
// track records one connection that a cut must close.
func (p *us25cProxy) track(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conns[conn] = struct{}{}
}

// untrack 移除一条已结束的连接。
// untrack forgets one finished connection.
func (p *us25cProxy) untrack(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, conn)
}

// Cut 设置切断标志；置真时立刻关闭全部在跟踪的连接。
// Cut flips the flag and closes tracked connections when cutting.
func (p *us25cProxy) Cut(on bool) {
	p.mu.Lock()
	p.cut = on
	var victims []net.Conn
	if on {
		for conn := range p.conns {
			victims = append(victims, conn)
		}
	}
	p.mu.Unlock()
	for _, conn := range victims {
		_ = conn.Close()
	}
}

// us25cProxyDSN 把测试库 DSN 的宿主改成代理地址；其余成分（用户、口令、库名、
// search_path）逐字保留。DSN 与其中的口令**不打印**，解析失败也不回显原文。
// us25cProxyDSN rewrites the DSN host to the proxy without ever echoing the DSN.
func us25cProxyDSN(t *testing.T, dsn, proxyAddr string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析测试库 DSN 失败（内容不打印）: %v", err)
	}
	parsed.Host = proxyAddr
	return parsed.String()
}

// us25cDSNHost 取 DSN 的 host:port（代理的上游目标），不返回其余成分，也从不打印。
// us25cDSNHost returns only the DSN host:port; nothing else about the DSN leaks.
func us25cDSNHost(t *testing.T, dsn string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析测试库 DSN 失败（内容不打印）: %v", err)
	}
	if parsed.Host == "" {
		t.Fatalf("测试库 DSN 没有 host:port（内容不打印）")
	}
	return parsed.Host
}

// us25cExecutionOwnerByID 按执行行主键读归属：领取实例 id 与经 `worker_nodes` 反查的
// 配置名。按主键定点复查（而不是"最近一条"）能让上弦循环在并发上弦时也不会张冠李戴。
// us25cExecutionOwnerByID resolves the owner of one execution row by primary key.
func us25cExecutionOwnerByID(t *testing.T, db *sql.DB, executionID int64) (workerName string, instanceID string, ok bool) {
	t.Helper()
	var (
		instance sql.NullString
		name     sql.NullString
	)
	err := db.QueryRow(
		`SELECT e.owner_worker_id, n.worker_name
		   FROM task_executions e
		   LEFT JOIN worker_nodes n ON n.instance_id = e.owner_worker_id
		  WHERE e.id = $1`, executionID).Scan(&instance, &name)
	if err == sql.ErrNoRows {
		return "", "", false
	}
	if err != nil {
		t.Fatalf("读执行行 %d 归属失败: %v", executionID, err)
	}
	return name.String, instance.String, true
}

// us25cNewExecutionCount 轮询直到执行行数超过 baseline，返回最新一条。布尔返回值
// 让上弦循环自己决定"继续等"还是"换一次尝试"，不把超时直接判成失败。
// us25cNewExecutionCount waits for a fresh execution row and returns the latest one.
func us25cNewExecutionCount(t *testing.T, db *sql.DB, taskID string, baseline int64, timeout time.Duration) (us25cExecution, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if count := us25cExecutionCount(t, db, taskID); count > baseline {
			if exec, ok := us25cLatestExecution(t, db, taskID); ok {
				return exec, true
			}
		}
		time.Sleep(us25cPoll)
	}
	return us25cExecution{}, false
}

// us25cWaitExecutionDone 轮询直到执行行离开 `started`，返回收尾后的整行（含 Run ID）。
// `started` 是领取事务写入的初态（claim.go 的 `INSERT INTO task_executions`）；收尾行
// 一定带终态与 `finished_at`，等它比等 Run 行更贴近"这条领取已经结算"。
// us25cWaitExecutionDone waits until the execution row leaves the started state.
func us25cWaitExecutionDone(t *testing.T, db *sql.DB, executionID int64, timeout time.Duration) us25cExecution {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var status string
	for time.Now().Before(deadline) {
		if exec, ok := us25cExecutionByID(t, db, executionID); ok {
			status = exec.Status
			if exec.Status != "" && exec.Status != "started" {
				return exec
			}
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待执行行 %d 收尾超时（%s）：最后一次状态 %q", executionID, timeout, status)
	return us25cExecution{}
}

// us25cWaitScalar 轮询一条标量查询直到谓词成立，返回最后一次读数。
// us25cWaitScalar polls one scalar query until pred holds.
func us25cWaitScalar(t *testing.T, db *sql.DB, label string, timeout time.Duration,
	query string, pred func(string) bool, args ...any,
) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last, _ = us25cScalarString(t, db, query, args...)
		if pred(last) {
			return last
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待 %s 超时（%s）：最后一次读数 %q", label, timeout, last)
	return ""
}

// us25cWaitRequests 轮询直到桩的到达次数达到 want（到达先于闸门，在途调用也计数）。
// us25cWaitRequests waits until the stub has seen want arrivals.
func us25cWaitRequests(t *testing.T, stub *us25cStub, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		last = stub.Requests()
		if last >= want {
			return
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待桩的请求到达数达到 %d 超时（%s）：最后一次 %d", want, timeout, last)
}

// us25cLease 读一条执行租约：属主实例、持有轮次与释放时刻（未释放记 `-`）。
// `schedule_execution` 的 `resource_id` 是执行行 id 的十进制串（claim.go 写入）。
// us25cLease reads one schedule-execution lease.
func us25cLease(t *testing.T, db *sql.DB, executionID int64) (owner string, epoch int64, released string) {
	t.Helper()
	var (
		ownerCell  sql.NullString
		releasedAt sql.NullString
	)
	err := db.QueryRow(
		`SELECT owner_worker_id, ownership_epoch, COALESCE(released_at::text, '-')
		   FROM execution_leases
		  WHERE kind = 'schedule_execution' AND resource_id = $1`,
		strconv.FormatInt(executionID, 10)).Scan(&ownerCell, &epoch, &releasedAt)
	if err != nil {
		t.Fatalf("读执行行 %d 的租约失败: %v", executionID, err)
	}
	return ownerCell.String, epoch, releasedAt.String
}

// us25cReplayFingerprint 是"这条 Run 的副作用账本"的指纹：Agent 名与三张计数表。
// 恢复只允许改状态，不允许新增消息 / 调用 / 工具行；未知副作用的调用不得重放，
// 因此"恢复前后指纹相同"是 FR-25C-23 的可判定读法。
// us25cReplayFingerprint fingerprints one run's effect ledger (counts only).
func us25cReplayFingerprint(t *testing.T, db *sql.DB, runID string) string {
	t.Helper()
	fp, _ := us25cScalarString(t, db,
		`SELECT r.agent
		        || '|' || (SELECT count(*) FROM messages WHERE run_id = r.run_id)
		        || '|' || (SELECT count(*) FROM llm_calls WHERE run_id = r.run_id)
		        || '|' || (SELECT count(*) FROM tool_invocations WHERE run_id = r.run_id)
		   FROM runs r WHERE r.run_id = $1`, runID)
	if fp == "" {
		t.Fatalf("Run %s 不存在，无法取副作用指纹", runID)
	}
	return fp
}

// us25cRunManifestHash 读 Run 准入时固定的资源清单里的 `asset_bundle_hash`。
// 这是"本轮实际用了哪个已发布版本"的落库证据（v3 清单起有该字段；旧清单为空）。
// us25cRunManifestHash reads the bundle hash fixed in the run's resource manifest.
func us25cRunManifestHash(t *testing.T, db *sql.DB, runID string) (string, bool) {
	t.Helper()
	return us25cScalarString(t, db,
		`SELECT COALESCE(snapshot_json::json->>'asset_bundle_hash', '')
		   FROM run_resource_manifests WHERE run_id = $1`, runID)
}

// us25cWaitInstalled 轮询直到某实例的资产根装有目标哈希：`bundle.json` 出现即安装完成
// 的判据（`assets.Install` 先写目录再原子换入，清单最后落位）。它只证明"装上了"，
// 不证明"可领取"——可领取由 worker 的 `ready` 与控制的 `installed` 门槛各自决定。
// us25cWaitInstalled waits until the bundle directory with its manifest exists.
func us25cWaitInstalled(t *testing.T, root, agent, hash string, timeout time.Duration) {
	t.Helper()
	path := filepath.Join(root, ".basalt", "assets", "bundles", agent, hash, "bundle.json")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return
		}
		time.Sleep(us25cPoll)
	}
	t.Fatalf("等待 %s 安装 %s/%s 超时（%s）：%s 未出现", root, agent, hash, timeout, path)
}

// us25cPlantCorruptManifest 把已安装目录的 `bundle.json` 改成"清单自报哈希 ≠ 目录寻址
// 哈希"：`assets.Resolve` 判 `ErrAssetVersionMismatch`，同步项落到 `corrupt`，且
// 本地目录不再自洽——回填也不能拿它当来源重建载荷（backfill 先 `Resolve`，损坏即停）。
// us25cPlantCorruptManifest makes the installed manifest disagree with its address.
func us25cPlantCorruptManifest(t *testing.T, root, agent, hash string) {
	t.Helper()
	path := filepath.Join(root, ".basalt", "assets", "bundles", agent, hash, "bundle.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s/%s 的 bundle.json 失败: %v", agent, hash, err)
	}
	var manifest assets.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("解析 %s/%s 的 bundle.json 失败: %v", agent, hash, err)
	}
	sum := sha256.Sum256([]byte("corrupt-" + hash))
	manifest.BundleHash = hex.EncodeToString(sum[:])
	corrupt, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("编码损坏清单 %s/%s 失败: %v", agent, hash, err)
	}
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("写损坏清单 %s/%s 失败: %v", agent, hash, err)
	}
}

// us25cPayload 是载荷表一行的**内存**副本。载荷字节只在本进程内存里：不写临时文件、
// 不进日志、不进任何测试产物；恢复时按原值写回同一哈希。
// us25cPayload is an in-memory copy of one payload row; the bytes never touch disk.
type us25cPayload struct {
	Agent         string
	AgentUID      string
	FormatVersion int64
	Payload       []byte
	PayloadSHA256 string
	SizeBytes     int64
	CreatedAt     int64
}

// us25cReadPayload 读某哈希的载荷行并整行留在内存。
// us25cReadPayload reads one payload row into memory.
func us25cReadPayload(t *testing.T, db *sql.DB, hash string) (us25cPayload, bool) {
	t.Helper()
	var row us25cPayload
	err := db.QueryRow(
		`SELECT agent, agent_uid, format_version, payload, payload_sha256, size_bytes, created_at
		   FROM agent_bundle_payloads WHERE bundle_hash = $1`, hash).
		Scan(&row.Agent, &row.AgentUID, &row.FormatVersion, &row.Payload,
			&row.PayloadSHA256, &row.SizeBytes, &row.CreatedAt)
	if err == sql.ErrNoRows {
		return us25cPayload{}, false
	}
	if err != nil {
		t.Fatalf("读载荷 %s 失败: %v", hash, err)
	}
	return row, true
}

// us25cDeletePayload 删除某哈希的载荷行（只动分发介质，不动指针与本地目录）。
// us25cDeletePayload removes one payload row without touching the pointer.
func us25cDeletePayload(t *testing.T, db *sql.DB, hash string) {
	t.Helper()
	if _, err := db.Exec(
		`DELETE FROM agent_bundle_payloads WHERE bundle_hash = $1`, hash); err != nil {
		t.Fatalf("删除载荷 %s 失败: %v", hash, err)
	}
}

// us25cRestorePayload 把内存副本写回同一哈希（重复写是 no-op）。
// us25cRestorePayload writes the in-memory copy back under the same hash.
func us25cRestorePayload(t *testing.T, db *sql.DB, hash string, row us25cPayload) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO agent_bundle_payloads
		   (bundle_hash, agent, agent_uid, format_version, payload, payload_sha256, size_bytes, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (bundle_hash) DO NOTHING`,
		hash, row.Agent, row.AgentUID, row.FormatVersion, row.Payload,
		row.PayloadSHA256, row.SizeBytes, row.CreatedAt); err != nil {
		t.Fatalf("恢复载荷 %s 失败: %v", hash, err)
	}
}

// us25cClaim 是一次"被点名的进程真的领取并跑完"的读数：执行行、领取进程（配置名 +
// 实例 id）、该次 Run 在桩上的请求窗口与运行期清单哈希。
// us25cClaim records one claim that a named process actually executed.
type us25cClaim struct {
	Execution us25cExecution
	Worker    string
	Instance  string
	Window    string
	Manifest  string
}

// us25cClaimAs 反复上弦，直到 **owner** 指定的进程领取并跑完一条新执行。
//
// 为什么按进程点名：A、B 都在扫同一张表，谁先领取由 CAS 决定；只断言"有一端执行了"
// 无法证明"两端用的是同一版本"。每次尝试前记下桩的到达序号，窗口只取这一次 Run 的
// 请求——整段拼接里躺着上一轮的正文，包含判断会把旧版本误判成新版本。
//
// 为什么每次尝试都要等收尾：执行行仍在 `started` 时重新上弦会撞 `ErrTaskBusy`，
// 写出 skipped 行并推进 `next_fire_at`，把"执行行数"这条判据搅乱。
// us25cClaimAs re-arms until the named worker claims and finishes one fresh execution.
func us25cClaimAs(t *testing.T, db *sql.DB, stub *us25cStub, taskID, owner string, timeout time.Duration) us25cClaim {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var (
		attempts int
		last     string
	)
	for time.Now().Before(deadline) {
		attempts++
		baseline := us25cExecutionCount(t, db, taskID)
		start := stub.Requests()
		us25cArmTask(t, db, taskID, 0)
		exec, ok := us25cNewExecutionCount(t, db, taskID, baseline, 3*time.Second)
		if !ok {
			last = fmt.Sprintf("第 %d 次上弦后 3s 内没有新执行行", attempts)
			continue
		}
		worker, instance, ownerKnown := us25cExecutionOwnerByID(t, db, exec.ID)
		exec = us25cWaitExecutionDone(t, db, exec.ID, 120*time.Second)
		if exec.RunID != "" {
			if _, _, exists := us25cRunStatus(t, db, exec.RunID); exists {
				us25cWaitRunTerminal(t, db, exec.RunID, 120*time.Second)
			}
		}
		last = fmt.Sprintf("第 %d 次由 %s 领取（执行行 %d，状态 %s）", attempts, worker, exec.ID, exec.Status)
		if !ownerKnown || worker != owner {
			continue
		}
		manifest := ""
		if exec.RunID != "" {
			manifest, _ = us25cRunManifestHash(t, db, exec.RunID)
		}
		return us25cClaim{
			Execution: exec,
			Worker:    worker,
			Instance:  instance,
			Window:    stub.ContentsFrom(start),
			Manifest:  manifest,
		}
	}
	t.Fatalf("等待 %s 领取任务 %s 超时（%s，共上弦 %d 次）：最后一次 %s", owner, taskID, timeout, attempts, last)
	return us25cClaim{}
}
