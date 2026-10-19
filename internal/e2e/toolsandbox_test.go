//go:build e2e

package e2e

// 本文件回填 US-4 §6.1 第 6 项与 US-5 §6.1 第 3 / 5 / 6 / 12 项（模块卡 §2.6.4 / §2.6.5）：
// 沙箱四档真实文案、`shell` 真命令读数、超时整树回收、端口档与脱敏抽验。四个子测试名与
// `backfillCatalog` 的 `Parent/sub` 逐字对应。
// This file backfills the sandbox and shell items of US-4/US-5. The four subtests match
// the catalog names verbatim; every scenario drives the real binary.

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 四段式拒绝的固定文案（生产常量在 internal/tool/policy.go 与 core/errors.go；此处逐字
// 冻结，改动任一侧即红）。第三段由 `core.SandboxViolationError` 统一拼装。
// The frozen refusal copy, pinned verbatim on this side as well.
const (
	deniedRuleBash    = "bash / sh 不得放行"
	deniedGuideBash   = "需要跑脚本请改用受控解释器通道（配置键 shell.interpreters，脚本须落在该 Agent 的 scripts/ 或技能库内）并重启服务。"
	deniedRuleWrite   = "写入目标超出当前 Agent 的可写范围"
	deniedGuideWrite  = "写范围固定为当前 Agent 自己的目录（代码常量），没有配置键可以加。"
	deniedRulePort    = "出网目标端口不在默认档"
	deniedGuidePort   = "当前版本不支持非默认端口（http 用 80、https 用 443，或省略端口）；没有配置键可以加。"
	deniedRuleDomain  = "域名不在白名单"
	deniedGuideDomain = "如需访问，请联系运维把该域名加入配置键 http.allowed_domains 并重启服务。"
	deniedFixedLine   = "这是安全策略，请勿反复重试；"
)

// shellHelperEnv 置位时本测试二进制扮演 shell 用例的替身命令（TestMain 在构建被测
// 二进制之前短路）。
// shellHelperEnv turns this test binary into the stand-in command; TestMain
// short-circuits before building the binary under test.
const shellHelperEnv = "BASALT_E2E_HELPER"

// TestBackfill_ToolSandbox 覆盖沙箱与受控执行的四个进程级人工项。
// TestBackfill_ToolSandbox covers the four process-level sandbox items.
func TestBackfill_ToolSandbox(t *testing.T) {
	t.Run("sandbox_denied", testToolSandboxDenied)
	t.Run("shell", testToolSandboxShell)
	t.Run("process_tree", testToolSandboxProcessTree)
	t.Run("port_redaction", testToolSandboxPortRedaction)
}

// testToolSandboxDenied：四档拒绝各留真实四段式文案 + 审计 failed / sandbox_denied
// （US-5 §6.1 第 3 项）：`bash -c`、工作区外路径、非默认端口、非白名单域名。
func testToolSandboxDenied(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-bash", "shell", mustJSON(map[string]any{
			"executable": "bash",
			"arguments":  []string{"-c", "echo hi"},
		})),
		toolCallResponse("call-write", "write_file", mustJSON(map[string]any{
			"path":    "../outside-marker.txt",
			"content": "越界写入",
		})),
		toolCallResponse("call-port", "http_get", mustJSON(map[string]any{
			"url": "https://example.com:8443/x",
		})),
		toolCallResponse("call-domain", "http_get", mustJSON(map[string]any{
			"url": "https://not-allowed.example/x",
		})),
		textResponse("四档拒绝均已收到"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo",
		agentFrontmatter("demo", []string{"shell", "write_file", "http_get"}), "沙箱四档演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "逐档试一遍")
	requireExit(t, res, 0, "四档拒绝后运行继续")
	requireContains(t, res.Stdout, "四档拒绝均已收到", "四档拒绝")

	reqs := fake.Requests()
	if len(reqs) != 5 {
		t.Fatalf("假模型应收到 5 次请求（4 次工具 + 1 次收尾），实际 %d 次", len(reqs))
	}
	results := toolResults(t, reqs[len(reqs)-1])
	if len(results) != 4 {
		t.Fatalf("最后一轮应带 4 条工具结果，实际 %d 条：%s", len(results), requestText(t, reqs[len(reqs)-1]))
	}
	expectations := []struct {
		rule, guidance, target string
	}{
		{deniedRuleBash, deniedGuideBash, "bash"},
		{deniedRuleWrite, deniedGuideWrite, "outside-marker.txt"},
		{deniedRulePort, deniedGuidePort, "example.com:8443"},
		{deniedRuleDomain, deniedGuideDomain, "not-allowed.example"},
	}
	for i, want := range expectations {
		for _, fragment := range []string{want.rule, deniedFixedLine, want.guidance, want.target} {
			requireContains(t, results[i], fragment, fmt.Sprintf("第 %d 档拒绝文案", i+1))
		}
	}

	// 拒绝发生在写入之前：越界目标不得落盘。
	outside := filepath.Join(h.Root, ".basalt", "agents", "outside-marker.txt")
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("越界写入目标不应存在（err=%v）", err)
	}

	db := openDB(t, h.dbPath())
	requireAllToolAudits(t, db,
		[]string{"shell", "write_file", "http_get", "http_get"}, "sandbox_denied")
}

// testToolSandboxShell：`shell` 真命令读数（US-5 §6.1 第 5 项）——cwd 落在 Agent 目录、
// 参数逐项原样、非零退出码不是失败、stdout 与 stderr 合流。
func testToolSandboxShell(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	exe := helperBinary(t)
	fake := newFakeModel(t,
		toolCallResponse("call-exec", "shell", mustJSON(map[string]any{
			"executable": exe,
			"arguments":  []string{"report", "alpha", "beta"},
		})),
		textResponse("命令已执行"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+shellSection(exe, []string{shellHelperEnv}))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"shell"}), "受控命令演示")

	res := h.run(t, append(fixtureEnv(), shellHelperEnv+"=1"),
		"chat", "--agent", "demo", "--message", "跑一条命令")
	requireExit(t, res, 0, "shell 真命令")
	requireContains(t, res.Stdout, "⚙ 调用工具 shell…", "shell 真命令")
	requireContains(t, res.Stdout, "命令已执行", "shell 真命令")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	results := toolResults(t, reqs[1])
	if len(results) != 1 {
		t.Fatalf("第二轮应带 1 条工具结果，实际 %d 条", len(results))
	}
	content := results[0]
	requireContains(t, content, "退出码：3\n", "非零退出码原样进首行")
	requireContains(t, content, "arg0=alpha\n", "参数逐项原样传递")
	requireContains(t, content, "arg1=beta\n", "参数逐项原样传递")
	requireContains(t, content, "stdout-line", "stdout 合流")
	requireContains(t, content, "stderr-line", "stderr 合流")
	wantDir := filepath.Join(h.Root, ".basalt", "agents", "demo")
	if got := lineValue(t, content, "cwd="); !strings.EqualFold(filepath.Clean(got), filepath.Clean(wantDir)) {
		t.Fatalf("子进程 cwd = %q，期望 Agent 目录 %q", got, wantDir)
	}
	if strings.Index(content, "stdout-line") > strings.Index(content, "stderr-line") {
		t.Fatalf("stdout 与 stderr 未保持写出的先后顺序：\n%s", content)
	}

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "shell", "success", "", 1)
	rows := queryRows(t, db, `SELECT result_json FROM tool_invocations`)
	if result, _ := rows[0]["result_json"].(string); !strings.Contains(result, "退出码：3") {
		t.Fatalf("审计 result_json 未保留命令结果：%q", result)
	}
}

// testToolSandboxProcessTree：经**工具路径**触发超时，父子进程一并消失（US-4 §6.1 第 6 项 +
// US-5 §6.1 第 6 项）。基线对照 = 既有的 `TestLocalRunner_KillsProcessTreeOnCancel`（runner
// 级，随 `make check` 每轮执行）；本项判的是工具 → 执行器 → runner 整条链。
func testToolSandboxProcessTree(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	exe := helperBinary(t)
	heartbeat := filepath.Join(t.TempDir(), "heartbeat.log")
	fake := newFakeModel(t,
		toolCallResponse("call-tree", "shell", mustJSON(map[string]any{
			"executable":      exe,
			"arguments":       []string{"tree", heartbeat},
			"timeout_seconds": 2,
		})),
		textResponse("超时后运行继续"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+shellSection(exe, []string{shellHelperEnv}))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"shell"}), "进程树回收演示")

	res := h.run(t, append(fixtureEnv(), shellHelperEnv+"=1"),
		"chat", "--agent", "demo", "--message", "跑一棵会超时的进程树")
	requireExit(t, res, 0, "超时后运行继续")
	requireContains(t, res.Stdout, "⚙ 调用工具 shell…", "进程树")
	requireContains(t, res.Stdout, "超时后运行继续", "进程树")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "context deadline exceeded", "截止错误必须回填给模型")

	db := openDB(t, h.dbPath())
	requireToolAudit(t, db, "shell", "failed", "timeout", 1)

	// 孙进程在超时前必须真的写过心跳（否则用例退化成"什么都没起"）；整树回收后
	// 心跳文件**不得**再增长。
	size := fileSize(t, heartbeat)
	if size == 0 {
		t.Fatal("心跳文件为空：孙进程未在超时前启动，用例未测到整树回收")
	}
	time.Sleep(500 * time.Millisecond)
	if again := fileSize(t, heartbeat); again != size {
		t.Fatalf("超时后孙进程仍在写心跳（%d → %d 字节）：整棵树都要回收", size, again)
	}
}

// testToolSandboxPortRedaction：端口档与脱敏抽验（US-5 §6.1 第 12 项）——`:8443` 拒绝、
// 默认端口放行到域名档、交叉档同拒、指引不指向域名白名单、拒绝文案与日志零回显。
func testToolSandboxPortRedaction(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	const secretURL = "https://user:secret@example.com:8443/x?token=abc123"
	fake := newFakeModel(t,
		toolCallResponse("call-port-redact", "http_get", mustJSON(map[string]any{"url": secretURL})),
		toolCallResponse("call-default-port", "http_get", mustJSON(map[string]any{
			"url": "https://example.com:443/x",
		})),
		toolCallResponse("call-cross-http", "http_get", mustJSON(map[string]any{
			"url": "http://example.com:443/x",
		})),
		toolCallResponse("call-cross-https", "http_get", mustJSON(map[string]any{
			"url": "https://example.com:80/x",
		})),
		textResponse("端口与脱敏抽验完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"http_get"}), "端口档演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "逐档试端口")
	requireExit(t, res, 0, "端口档")
	requireContains(t, res.Stdout, "端口与脱敏抽验完成", "端口档")

	reqs := fake.Requests()
	if len(reqs) != 5 {
		t.Fatalf("假模型应收到 5 次请求，实际 %d 次", len(reqs))
	}
	results := toolResults(t, reqs[len(reqs)-1])
	if len(results) != 4 {
		t.Fatalf("最后一轮应带 4 条工具结果，实际 %d 条", len(results))
	}

	// ① `:8443` 被拒，段 2 点名 host 与端口。
	requireContains(t, results[0], deniedRulePort, "非默认端口档")
	requireContains(t, results[0], "example.com:8443", "段 2 点名 host 与端口")
	// ② 第 4 段指引冻结：只说"不支持非默认端口"，不指向域名白名单。
	requireContains(t, results[0], deniedGuidePort, "非默认端口指引")
	requireAbsent(t, results[0], "http.allowed_domains", "端口档指引不得指向域名白名单")
	// ③ 拒绝文案零回显 userinfo / query。
	for _, secret := range []string{"secret", "abc123"} {
		requireAbsent(t, results[0], secret, "拒绝文案不得回显 userinfo / query")
	}
	// ④ 显式默认端口 `https:443` 放行到域名档（证明端口档先于域名档且默认档通过）。
	requireContains(t, results[1], deniedRuleDomain, "https:443 应放行到域名档")
	requireContains(t, results[1], deniedGuideDomain, "域名档指引")
	// ⑤ 交叉档同拒。
	requireContains(t, results[2], deniedRulePort, "http:443 交叉档")
	requireContains(t, results[3], deniedRulePort, "https:80 交叉档")

	db := openDB(t, h.dbPath())
	requireAllToolAudits(t, db, []string{"http_get", "http_get", "http_get", "http_get"}, "sandbox_denied")
	rows := queryRows(t, db, `SELECT error_message FROM tool_invocations ORDER BY id`)
	message, _ := rows[0]["error_message"].(string)
	requireContains(t, message, "example.com:8443", "审计 error_message 段 2")
	for _, secret := range []string{"secret", "abc123"} {
		requireAbsent(t, message, secret, "审计 error_message 不得回显 userinfo / query")
	}

	// 日志面零回显（stdout 与 stderr 一起扫）。
	for _, secret := range []string{"secret", "abc123"} {
		requireAbsent(t, res.Stdout+res.Stderr, secret, "进程输出不得回显 userinfo / query")
	}
}

// helperBinary 返回本测试二进制的绝对路径（`/` 分隔，可逐字进配置与工具入参）。
// helperBinary returns this test binary's absolute path with forward slashes.
func helperBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("定位测试二进制失败: %v", err)
	}
	return filepath.ToSlash(exe)
}

// shellSection 组装 `shell` 配置段：命令白名单放替身命令，可选环境透传白名单。
// shellSection assembles the shell section with the helper allowlisted.
func shellSection(exe string, passthrough []string) string {
	out := "shell:\n  allowed_commands:\n    - '" + exe + "'\n"
	if len(passthrough) > 0 {
		out += "  env_passthrough:\n"
		for _, name := range passthrough {
			out += "    - " + name + "\n"
		}
	}
	return out
}

// lineValue 取以 prefix 开头的那一行余下部分。
// lineValue returns the remainder of the first line carrying the prefix.
func lineValue(t *testing.T, text, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return rest
		}
	}
	t.Fatalf("输出里没有 %q 开头的行：\n%s", prefix, text)
	return ""
}

// fileSize 返回文件字节数；文件不存在直接判红（心跳文件本就该存在）。
// fileSize returns the file size; a missing file fails the test.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return info.Size()
}

// requireAllToolAudits 逐行断言审计表：工具名按顺序、状态全为 failed、错误类别全为期望值。
// requireAllToolAudits asserts every audit row's tool name, failed status and error kind.
func requireAllToolAudits(t *testing.T, db *sql.DB, tools []string, errorKind string) {
	t.Helper()
	rows := queryRows(t, db,
		`SELECT tool_name, status, error_kind FROM tool_invocations ORDER BY id`)
	if len(rows) != len(tools) {
		t.Fatalf("tool_invocations 应有 %d 行，实际 %d 行：%v", len(tools), len(rows), rows)
	}
	for i, tool := range tools {
		if got, _ := rows[i]["tool_name"].(string); got != tool {
			t.Fatalf("tool_invocations[%d].tool_name = %v，期望 %s", i, rows[i]["tool_name"], tool)
		}
		if got, _ := rows[i]["status"].(string); got != "failed" {
			t.Fatalf("tool_invocations[%d].status = %v，期望 failed", i, rows[i]["status"])
		}
		if got, _ := rows[i]["error_kind"].(string); got != errorKind {
			t.Fatalf("tool_invocations[%d].error_kind = %v，期望 %q", i, rows[i]["error_kind"], errorKind)
		}
	}
}

// shellHelperMode 解析替身命令的模式：环境置位才算替身（正常 `go test` 返回 false）。
// shellHelperMode reads the stand-in mode; the environment gates the behaviour.
func shellHelperMode() (mode string, args []string, ok bool) {
	if os.Getenv(shellHelperEnv) == "" || len(os.Args) < 2 {
		return "", nil, false
	}
	return os.Args[1], os.Args[2:], true
}

// runShellHelper 是替身命令主干（不是被测代码）：
//   - `report`：打印 cwd 与参数，写一行 stdout 与一行 stderr，退出码 3；
//   - `tree`：派生孙进程（`beat` 模式写心跳）后常驻不退，供整树回收观察；
//   - `beat`：每 50ms 追加一行心跳；
//   - `block`：写 `<放行文件>.started` 标记后轮询放行文件，供崩溃恢复用例把运行
//     稳定地卡在工具执行中途（进程被杀时它在 Windows 随 Job 回收、在 Linux 靠放行
//     文件自行退出；120 秒硬上限防孤儿进程）。
//
// runShellHelper is the stand-in command body: `report` prints readings and exits 3,
// `tree` spawns a heartbeat grandchild and stays resident, `beat` appends heartbeats,
// and `block` parks a run mid-tool with a started marker until the release file appears.
func runShellHelper(mode string, args []string) int {
	switch mode {
	case "report":
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		fmt.Println("cwd=" + wd)
		for index, arg := range args {
			fmt.Printf("arg%d=%s\n", index, arg)
		}
		fmt.Println("stdout-line")
		fmt.Fprintln(os.Stderr, "stderr-line")
		return 3
	case "tree":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "tree 模式需要一个心跳文件路径")
			return 2
		}
		self, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "定位自身失败:", err)
			return 2
		}
		beat := exec.Command(self, "beat", args[0])
		if err := beat.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "spawn grandchild:", err)
			return 2
		}
		fmt.Printf("grandchild=%d\n", beat.Process.Pid)
		for {
			time.Sleep(time.Hour)
		}
	case "beat":
		if len(args) != 1 {
			return 2
		}
		for {
			file, err := os.OpenFile(args[0], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return 2
			}
			if _, err := file.WriteString("beat\n"); err != nil {
				_ = file.Close()
				return 2
			}
			if err := file.Close(); err != nil {
				return 2
			}
			time.Sleep(50 * time.Millisecond)
		}
	case "block":
		if len(args) != 1 {
			fmt.Fprintln(os.Stderr, "block 模式需要一个放行文件路径")
			return 2
		}
		release := args[0]
		if err := os.WriteFile(release+".started", []byte("started\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "写启动标记:", err)
			return 2
		}
		deadline := time.Now().Add(120 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(release); err == nil {
				return 0
			}
			time.Sleep(50 * time.Millisecond)
		}
		return 2
	default:
		return 2
	}
}
