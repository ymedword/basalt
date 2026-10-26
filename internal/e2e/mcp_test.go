//go:build e2e

package e2e

// 本文件回填 US-7（MCP）的八个人工项（模块卡 §2.6.7 映射表）。八个子测试名与
// `backfillCatalog` 的 `Parent/sub` 逐字对应。MCP server 由**测试二进制自举**：
// `mcpHelperCommand()` 把本包测试二进制以 `-test.run=^TestMCPHelperProcess$` 重新拉起，
// 设 `BASALT_MCP_HELPER=1` 后以 stdio 服务 `echo` / `boom` / `blob` 三件工具——全程不装
// 任何外部 server，也不访问网络。
// This file backfills US-7's eight manual items. The MCP server is the test binary itself
// re-executed as a stdio helper, so no external server or network is involved.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/testutil"
)

const (
	// mcpHelperServer 是 mcp_servers.yaml 里登记的唯一 server 名。
	// mcpHelperServer is the only server name registered in the fixtures.
	mcpHelperServer = "helper"
	// mcpEchoTool 是 helper 的标准回显工具。
	// mcpEchoTool is the helper's standard echo tool.
	mcpEchoTool = "echo"
	// mcpHelperMarker 出现在 helper 子进程的命令行里，供停机检查按命令行辨识。
	// mcpHelperMarker identifies helper subprocesses by their command line.
	mcpHelperMarker = "-test.run=^TestMCPHelperProcess$"
)

// TestBackfill_MCP 覆盖 MCP 的八个人工项。
// TestBackfill_MCP covers the eight manual items of the MCP module.
func TestBackfill_MCP(t *testing.T) {
	t.Run("zero_code", testMCPZeroCode)
	t.Run("bad_command", testMCPBadCommand)
	t.Run("two_paths", testMCPTwoPaths)
	t.Run("tool_list", testMCPToolList)
	t.Run("env_negative", testMCPEnvNegative)
	t.Run("shutdown", testMCPShutdown)
	t.Run("redaction", testMCPRedaction)
	t.Run("concurrency", testMCPConcurrency)
}

// testMCPZeroCode：Demo 三（甲档）——只写 `AGENT.md` 与 `mcp_servers.yaml`，零代码接入。
// 判据：工具出现在 `tool list`；模型确实调用了它；`tool_invocations` 有一行成功；启动
// 摘要 `mcp_connected=1`。
func testMCPZeroCode(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
	fake := newFakeModel(t,
		toolCallResponse("call-zero-code", mcpEchoTool, `{"text":"零代码接入-回显"}`),
		textResponse("工具调用完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+mcpShellAllowed())
	h.writeAgent(t, "demo", mcpAgentFrontmatter("demo", []string{mcpEchoTool}, []string{mcpHelperServer}),
		"零代码接入演示")

	res := h.run(t, mcpHelperEnv(), "chat", "--agent", "demo", "--message", "调用 echo")
	requireExit(t, res, 0, "零代码接入")
	requireContains(t, res.Stderr, "MCP server 已连接", "连接 INFO")
	requireContains(t, res.Stderr, "server=helper", "连接 INFO 点名 server")
	requireContains(t, res.Stderr, "mcp_connected=1", "启动摘要按实际连接数")

	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "零代码接入-回显", "模型收到的 echo 结果")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT tool_name, status FROM tool_invocations ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if got, _ := rows[0]["tool_name"].(string); got != mcpEchoTool {
		t.Fatalf("审计 tool_name = %q，期望 %q", got, mcpEchoTool)
	}
	if got, _ := rows[0]["status"].(string); got != "success" {
		t.Fatalf("审计 status = %q，期望 success", got)
	}

	list := h.run(t, mcpHelperEnv(), "tool", "list")
	requireExit(t, list, 0, "tool list 冒烟")
	requireContains(t, list.Stdout, mcpEchoTool+"：回显 text 参数", "tool list 看到 MCP 工具")
}

// testMCPBadCommand：错误 `command` 负向（U-2 第 3 项）。判据：启动不中断；日志出现
// `MCP server 启动失败——继续其余 server` 并点名 server；依赖它的 Agent 未注册
// （`mcp server skipped`）；其余 Agent 正常可聊。命令在 `shell.allowed_commands` 名单内
// （US-12 起 supervisor 连接前先判可执行名），因此走的仍是**连接失败**而不是策略拒绝。
func testMCPBadCommand(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeMCPServers(t, "servers:\n  - name: broken\n    command: basalt-e2e-missing-mcp-server\n")
	fake := newFakeModel(t, textResponse("正常回答"))
	h.writeConfig(t, configWithProvider(fake.URL())+mcpShellAllow("basalt-e2e-missing-mcp-server"))
	h.writeAgent(t, "demo", mcpAgentFrontmatter("demo", nil, []string{"broken"}), "依赖坏 server 的 Agent")
	h.writeAgent(t, "safe", agentFrontmatter("safe", nil), "不依赖 MCP 的 Agent")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "safe", "--message", "你好")
	requireExit(t, res, 0, "坏 command 不中断启动")
	requireContains(t, res.Stdout, "正常回答", "其余 Agent 正常可聊")
	requireContains(t, res.Stderr, "MCP server 启动失败——继续其余 server", "单 server 失败不中断其余")
	requireContains(t, res.Stderr, "server=broken", "WARN 点名 server")
	requireContains(t, res.Stderr, "detail=连接失败", "WARN 的 detail 只给固定短语")
	requireAbsent(t, res.Stderr, "basalt-e2e-missing-mcp-server", "WARN 不得回显原始 command")
	requireContains(t, res.Stderr, "mcp_connected=0", "启动摘要按实际连接数计 0")
	requireContains(t, res.Stderr, "Agent 未注册", "依赖失败 server 的 Agent 未注册")
	requireContains(t, res.Stderr, `reason="mcp server skipped"`, "拒载原因取契约字面量")
	requireContains(t, res.Stderr, "agent=demo", "拒载 WARN 点名 Agent")

	if got := len(fake.Requests()); got != 1 {
		t.Fatalf("假模型应收到 1 次请求（只跑 safe），实际 %d 次", got)
	}
}

// testMCPTwoPaths：两档展示面一致性（U-2 第 4 项）。轻路径（`profile list`）对含 MCP
// 声明的 Agent 标第三态 `registered=unverified`；重路径（`chat`）连接成功后摘要
// `mcp_connected=1`；零 MCP 配置时轻路径读数与 US-3 基线一致（无 `/ 未验证` 后缀）。
func testMCPTwoPaths(t *testing.T) {
	t.Run("with_mcp", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		h.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
		fake := newFakeModel(t, textResponse("重路径回答"))
		h.writeConfig(t, configWithProvider(fake.URL())+mcpShellAllowed())
		h.writeAgent(t, "demo", mcpAgentFrontmatter("demo", []string{mcpEchoTool}, []string{mcpHelperServer}),
			"两档展示演示")

		light := h.run(t, fixtureEnv(), "profile", "list")
		requireExit(t, light, 0, "profile list")
		requireContains(t, light.Stdout,
			"demo：registered=unverified——MCP 工具与连接需重路径确认", "轻路径第三态")
		requireContains(t, light.Stdout, "/ 未验证 1", "汇总行带未验证计数")

		heavy := h.run(t, mcpHelperEnv(), "chat", "--agent", "demo", "--message", "你好")
		requireExit(t, heavy, 0, "重路径 chat")
		requireContains(t, heavy.Stderr, "mcp_connected=1", "重路径确认连接")
		requireContains(t, heavy.Stdout, "重路径回答", "重路径正常作答")
	})

	t.Run("zero_mcp_baseline", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		h.writeConfig(t, configWithProvider("http://127.0.0.1:1"))
		h.writeAgent(t, "demo", mcpAgentFrontmatter("demo", nil, nil), "零 MCP 基线")

		light := h.run(t, fixtureEnv(), "profile", "list")
		requireExit(t, light, 0, "零 MCP profile list")
		requireContains(t, light.Stdout, "demo：registered=true", "零配置按 US-3 基线注册")
		requireAbsent(t, light.Stdout, "未验证", "零计数时汇总行与 US-3 逐字节相同")
	})
}

// testMCPToolList：`tool list` 冒烟（U-2 第 5 项）。判据：MCP 三件工具与内置工具同列；
// 总数 = 无配置基线 + 3；MCP 工具的描述与参数 schema 与 server 声明一致（抽 echo 比对）。
func testMCPToolList(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeConfig(t, configWithProvider("http://127.0.0.1:1")+mcpShellAllowed())

	baseline := h.run(t, fixtureEnv(), "tool", "list")
	requireExit(t, baseline, 0, "tool list 基线")
	baseTotal := toolListTotal(t, baseline.Stdout)

	h.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
	list := h.run(t, mcpHelperEnv(), "tool", "list")
	requireExit(t, list, 0, "tool list 冒烟")
	requireContains(t, list.Stdout, "echo：回显 text 参数", "echo 描述原样")
	requireContains(t, list.Stdout, "boom：", "boom 在列")
	requireContains(t, list.Stdout, "blob：", "blob 在列")
	if got := toolListTotal(t, list.Stdout); got != baseTotal+3 {
		t.Fatalf("有 MCP 时工具总数 = %d，期望基线 %d + 3", got, baseTotal)
	}

	fake := newFakeModel(t,
		toolCallResponse("call-schema", mcpEchoTool, `{"text":"schema 抽验"}`),
		textResponse("schema 抽验完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+mcpShellAllowed())
	h.writeAgent(t, "demo", mcpAgentFrontmatter("demo", []string{mcpEchoTool}, []string{mcpHelperServer}),
		"schema 抽验")
	res := h.run(t, mcpHelperEnv(), "chat", "--agent", "demo", "--message", "抽验 schema")
	requireExit(t, res, 0, "schema 抽验")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	def := requestToolDefinition(t, reqs[0], mcpEchoTool)
	if got, _ := def["description"].(string); got != "回显 text 参数" {
		t.Fatalf("模型侧 echo 描述 = %q，期望与 server 声明一致", got)
	}
	parameters, _ := def["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	text, _ := properties["text"].(map[string]any)
	if got, _ := text["type"].(string); got != "string" {
		t.Fatalf("模型侧 echo 参数 schema 的 text.type = %v，期望 string", text["type"])
	}
}

// testMCPEnvNegative：`env` 占位的启动隔离与明文负向（U-2 第 6 项；14D §2.5.4 E3 修订）。
// 引用未设置的变量、引用空值不再中止整个进程：只跳过该 server——不建连接、不注册工具、
// 不拿占位符字面量起进程——其余 server 与内置工具照常，WARN 点名 server / 槽位 / 变量且
// **不回显值**。明文 `env` 仍是失败关闭：那是配置面错误，不是"这条 server 自己的凭据
// 当前取不到值"，不在启动隔离的放行范围内。
func testMCPEnvNegative(t *testing.T) {
	const missingVar = "BASALT_E2E_MCP_UNSET"
	yamlBody := "servers:\n  - name: helper\n    command: " + mcpHelperCommand() +
		"\n    env:\n      BASALT_MCP_HELPER: ${" + missingVar + "}\n"

	cases := []struct {
		name string
		env  []string
	}{
		{name: "unset", env: fixtureEnv()},
		{name: "empty", env: append(fixtureEnv(), missingVar+"=")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			initWorkspace(t, h)
			h.writeMCPServers(t, yamlBody)
			h.writeConfig(t, configWithProvider("http://127.0.0.1:1"))

			res := h.run(t, tc.env, "tool", "list")
			requireExit(t, res, 0, tc.name+"：单个 server 凭据缺失只跳过该 server，不中止启动")
			output := res.Stdout + res.Stderr
			requireContains(t, output, "MCP server 凭据不可用，本次启动跳过该 server",
				"启动隔离必须留下点明跳过事实的 WARN")
			requireContains(t, output, "server=helper", "WARN 点名被跳过的 server")
			requireContains(t, output, "槽位 BASALT_MCP_HELPER 引用的 "+missingVar+" 未设置或为空",
				"WARN 点名槽位与变量名")
			requireAbsent(t, output, "回显 text 参数",
				"被跳过的 server 不得注册任何工具")
			requireContains(t, output, "共 11 件工具",
				"跳过一个 server 不得影响其余内置工具的装配")
		})
	}

	t.Run("plaintext", func(t *testing.T) {
		const plaintext = "plaintext-token-value"
		h := newHarness(t)
		initWorkspace(t, h)
		h.writeMCPServers(t, "servers:\n  - name: helper\n    command: "+mcpHelperCommand()+
			"\n    env:\n      BASALT_MCP_HELPER: "+plaintext+"\n")
		h.writeConfig(t, configWithProvider("http://127.0.0.1:1"))

		res := h.run(t, fixtureEnv(), "tool", "list")
		if res.ExitCode == 0 {
			t.Fatalf("明文 env 必须拒绝启动\n%s", res.Stdout+res.Stderr)
		}
		output := res.Stdout + res.Stderr
		requireContains(t, output, "env.BASALT_MCP_HELPER 必须是 ${VAR} 占位（明文凭证不得入库）",
			"明文负向的逐字文案")
		requireAbsent(t, output, plaintext, "拒绝文案不得回显明文值")
	})
}

// testMCPShutdown：停机子进程不残留（U-2 第 7 项）。判据：`chat` 正常退出后，按镜像名
// 枚举的 helper 进程数回到 0（Windows 用 `tasklist`，其余平台用 `ps` 读命令行）。
func testMCPShutdown(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
	fake := newFakeModel(t, textResponse("结束"))
	h.writeConfig(t, configWithProvider(fake.URL())+mcpShellAllowed())
	h.writeAgent(t, "demo", mcpAgentFrontmatter("demo", []string{mcpEchoTool}, []string{mcpHelperServer}),
		"停机检查演示")

	// 前序用例的 helper 在父进程退出后才异步感知 EOF 并退出；先等基线归零再判本用例，
	// 否则会把"正在退出"误判成"残留"。真正的停机缺陷由等待超时判红。
	// Helper processes notice EOF asynchronously after their parent exits; wait for the
	// baseline to settle before asserting, so this case only fails on a real leak.
	waitForHelperCount(t, 0, 10*time.Second)
	res := h.run(t, mcpHelperEnv(), "chat", "--agent", "demo", "--message", "你好")
	requireExit(t, res, 0, "停机检查")
	requireContains(t, res.Stderr, "mcp_connected=1", "确认本进程确实起过 MCP 子进程")
	waitForHelperCount(t, 0, 10*time.Second)
}

// testMCPRedaction：凭证与敏感面（U-2 第 10 项）。三档判据：① `${VAR}` 值只在子进程
// 环境里，进程输出与工作区文件零回显；② 原始 `command` 不出现在 WARN 里（只给固定短语）；
// ③ 协议层原始错误文本不进模型上下文 / 日志 / 审计（审计 `error_message` = 自产 Reason）。
func testMCPRedaction(t *testing.T) {
	const sentinel = "mcp-sentinel-4c1e"

	t.Run("env_value", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		h.writeMCPServers(t, mcpServersYAMLWithEnv(mcpHelperServer,
			[2]string{"BASALT_MCP_HELPER", "${" + mcpHelperEnvRef + "}"},
			[2]string{"EXTRA_CREDENTIAL", "${BASALT_E2E_MCP_EXTRA}"}))
		h.writeConfig(t, configWithProvider("http://127.0.0.1:1")+mcpShellAllowed())
		env := append(mcpHelperEnv(), "BASALT_E2E_MCP_EXTRA="+sentinel)

		res := h.run(t, env, "tool", "list")
		requireExit(t, res, 0, "env 值不回显")
		requireContains(t, res.Stdout, "echo：回显 text 参数", "连接成功才有这一行")
		requireAbsent(t, res.Stdout+res.Stderr, sentinel, "进程输出不得回显 env 值")
		requireAbsentInWorkspace(t, h.Root, sentinel, "env 值不得落盘到工作区")
	})

	t.Run("command_phrase", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		h.writeMCPServers(t, "servers:\n  - name: broken\n    command: basalt-missing-"+sentinel+"-server\n")
		fake := newFakeModel(t, textResponse("正常回答"))
		h.writeConfig(t, configWithProvider(fake.URL())+mcpShellAllow("basalt-missing-"+sentinel+"-server"))
		h.writeAgent(t, "safe", agentFrontmatter("safe", nil), "原始命令不回显")

		res := h.run(t, fixtureEnv(), "chat", "--agent", "safe", "--message", "你好")
		requireExit(t, res, 0, "原始命令不回显")
		requireContains(t, res.Stderr, "detail=连接失败", "detail 只给固定短语")
		requireAbsent(t, res.Stdout+res.Stderr, sentinel, "WARN 不得回显原始 command")
	})

	t.Run("protocol_error", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		h.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
		fake := newFakeModel(t,
			// text 传数字：server 侧 handler 解码失败 → 协议层错误（原始文本不得外泄）。
			toolCallResponse("call-bad-arg", mcpEchoTool, `{"text": 1}`),
			textResponse("失败已回填"),
		)
		h.writeConfig(t, configWithProvider(fake.URL())+mcpShellAllowed())
		h.writeAgent(t, "demo", mcpAgentFrontmatter("demo", []string{mcpEchoTool}, []string{mcpHelperServer}),
			"协议错误脱敏")

		res := h.run(t, mcpHelperEnv(), "chat", "--agent", "demo", "--message", "触发协议错误")
		requireExit(t, res, 0, "工具失败不终止 Run")
		reqs := fake.Requests()
		if len(reqs) != 2 {
			t.Fatalf("假模型应收到 2 次请求（调用 → 失败回填后收尾），实际 %d 次", len(reqs))
		}
		reason := "MCP 调用失败：" + mcpHelperServer + "/" + mcpEchoTool
		requireToolResultContains(t, reqs[1], reason, "模型只看到自产 Reason")
		requireAbsent(t, lastToolContent(reqs[1]), "cannot unmarshal", "工具结果不得回填原始协议文本")
		requireAbsent(t, res.Stdout+res.Stderr, "cannot unmarshal", "原始协议错误不得进日志")

		db := openDB(t, h.dbPath())
		rows := queryRows(t, db, `SELECT status, error_kind, error_message FROM tool_invocations ORDER BY id`)
		if len(rows) != 1 {
			t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(rows), rows)
		}
		if got, _ := rows[0]["status"].(string); got != "failed" {
			t.Fatalf("审计 status = %q，期望 failed", got)
		}
		if got, _ := rows[0]["error_kind"].(string); got != "external_error" {
			t.Fatalf("审计 error_kind = %q，期望 external_error", got)
		}
		message, _ := rows[0]["error_message"].(string)
		if message != reason {
			t.Fatalf("审计 error_message = %q，期望逐字为自产 Reason", message)
		}
	})
}

// testMCPConcurrency：并发抽验（U-2 第 13 项）。判据：8 个并发会话同调一件 MCP 工具，
// 全部成功回显、8 行成功审计、无失败 WARN；`-race` 由非 e2e 的 `go test -race` 覆盖。
func testMCPConcurrency(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
	addr := freeLoopbackAddr(t)

	var (
		mu       sync.Mutex
		requests int
	)
	server := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("假服务：请求体不是 JSON 对象: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		requests++
		mu.Unlock()
		k := invokeOrdinal(userMessageOf(req), "并发MCP-")
		if k == "" {
			t.Errorf("假服务：用户消息缺序号：%s", mustJSON(req))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if lastToolContent(req) == "" {
			_, _ = fmt.Fprintf(w, "%s", toolCallResponse("call-"+k, mcpEchoTool,
				mustJSON(map[string]string{"text": "回显-并发MCP-" + k})).Body)
			return
		}
		_, _ = fmt.Fprintf(w, "%s", textResponse("完成-并发MCP-"+k).Body)
	}))
	h.writeConfig(t, configWithAddr(server.URL, addr)+mcpShellAllowed())
	h.writeAgent(t, "demo", mcpAgentFrontmatter("demo", []string{mcpEchoTool}, []string{mcpHelperServer}),
		"并发调用演示")

	serve := startServeMCP(t, h, addr)

	const workers = 8
	readings := make([]invokeReading, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := fmt.Sprintf("%02d", i+1)
			readings[i], errs[i] = postInvoke(addr, "demo", "并发MCP-"+k)
		}()
	}
	wg.Wait()

	for i, reading := range readings {
		if errs[i] != nil {
			t.Fatalf("第 %d 笔 invoke 失败: %v", i+1, errs[i])
		}
		if reading.Status != "completed" {
			t.Fatalf("第 %d 笔运行状态 = %q，期望 completed", i+1, reading.Status)
		}
		want := fmt.Sprintf("完成-并发MCP-%02d", i+1)
		if reading.FinalText != want {
			t.Fatalf("第 %d 笔最终文本 = %q，期望 %q", i+1, reading.FinalText, want)
		}
	}

	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != workers*2 {
		t.Fatalf("假服务应收到 %d 次请求（每笔两轮），实际 %d 次", workers*2, gotRequests)
	}

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db,
		`SELECT tool_name, status, result_json FROM tool_invocations ORDER BY id`)
	if len(rows) != workers {
		t.Fatalf("tool_invocations 应有 %d 行，实际 %d 行", workers, len(rows))
	}
	seen := make(map[string]bool, workers)
	for _, row := range rows {
		if got, _ := row["tool_name"].(string); got != mcpEchoTool {
			t.Fatalf("审计 tool_name = %q，期望 %q", got, mcpEchoTool)
		}
		if got, _ := row["status"].(string); got != "success" {
			t.Fatalf("审计 status = %q，期望 success", got)
		}
		result, _ := row["result_json"].(string)
		requireContains(t, result, "回显-并发MCP-", "结果正文含 echo 回显")
		seen[result] = true
	}
	if len(seen) != workers {
		t.Fatalf("8 笔并发的结果应两两不同，实际只有 %d 种", len(seen))
	}
	requireAbsent(t, serve.output(), "工具执行失败", "并发调用不得出现失败 WARN")
}

// mcpHelperEnv 在夹具环境上追加 helper 占位变量（取值 1）。
// mcpHelperEnv appends the helper placeholder variable on top of the fixture env.
func mcpHelperEnv() []string {
	return append(fixtureEnv(), mcpHelperEnvRef+"=1")
}

// startServeMCP 拉起 `basalt serve`（带上 helper 占位变量）并等健康检查就绪。
// startServeMCP launches `basalt serve` with the helper placeholder and waits for health.
func startServeMCP(t *testing.T, h *harness, addr string) *process {
	t.Helper()
	p := h.start(t, mcpHelperEnv(), "serve")
	p.waitHTTP(t, "http://"+addr+"/api/v1/health", 30*time.Second)
	return p
}

// mcpAgentFrontmatter 组装声明 MCP server 的 frontmatter（复用夹具 provider 常量）。
// mcpAgentFrontmatter assembles frontmatter declaring tools and MCP servers.
func mcpAgentFrontmatter(name string, tools, servers []string) string {
	out := "name: " + name + "\n" +
		"description: e2e 回填夹具\n" +
		"provider:\n  name: " + fixtureProvider + "\n  model: " + fixtureModel + "\n"
	if len(tools) == 0 {
		out += "tools: []\n"
	} else {
		out += "tools:\n"
		for _, tool := range tools {
			out += "  - " + tool + "\n"
		}
	}
	if len(servers) == 0 {
		return out + "mcp_servers: []\n"
	}
	out += "mcp_servers:\n"
	for _, server := range servers {
		out += "  - " + server + "\n"
	}
	return out
}

// mcpServersYAMLWithEnv 组装带 env 条目的单 server 配置（键值按给定顺序输出）。
// mcpServersYAMLWithEnv assembles a single-server configuration with explicit env rows.
func mcpServersYAMLWithEnv(name string, pairs ...[2]string) string {
	out := "servers:\n  - name: " + name + "\n    command: " + mcpHelperCommand() + "\n"
	if len(pairs) == 0 {
		return out
	}
	out += "    env:\n"
	for _, pair := range pairs {
		out += "      " + pair[0] + ": " + pair[1] + "\n"
	}
	return out
}

// toolListTotal 解析 `tool list` 汇总行的工具总数。
// toolListTotal parses the total from `tool list`'s summary line.
func toolListTotal(t *testing.T, out string) int {
	t.Helper()
	idx := strings.LastIndex(out, "共 ")
	if idx < 0 {
		t.Fatalf("tool list 输出缺少汇总行：\n%s", out)
	}
	fields := strings.Fields(out[idx+len("共 "):])
	if len(fields) == 0 {
		t.Fatalf("tool list 汇总行缺少数字：\n%s", out)
	}
	total, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("tool list 汇总行数字无法解析（%q）：%v", fields[0], err)
	}
	return total
}

// requestToolDefinition 取模型请求里某件工具的定义（OpenAI `tools[].function`）。
// requestToolDefinition returns the named tool definition from a model request.
func requestToolDefinition(t *testing.T, req map[string]any, name string) map[string]any {
	t.Helper()
	raw, _ := req["tools"].([]any)
	for _, item := range raw {
		tool, _ := item.(map[string]any)
		if tool == nil {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		if got, _ := fn["name"].(string); got == name {
			return fn
		}
	}
	t.Fatalf("请求里没有工具 %q 的定义：%s", name, requestText(t, req))
	return nil
}

// helperProcessCount 数当前机器上的 helper 子进程（本测试进程自身不计）。Windows 按
// 镜像名（同一个测试二进制）查 `tasklist`；其余平台按命令行里的 `-test.run` 标记查 `ps`。
// helperProcessCount counts helper subprocesses; the test process itself is excluded.
func helperProcessCount(t *testing.T) int {
	t.Helper()
	self := strconv.Itoa(os.Getpid())
	if runtime.GOOS == "windows" {
		image := filepath.Base(os.Args[0])
		out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+image, "/FO", "CSV", "/NH").Output()
		if err != nil {
			t.Fatalf("tasklist 枚举进程失败: %v", err)
		}
		count := 0
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, `"`) {
				continue // 本地化的"没有匹配任务"提示行
			}
			fields := strings.Split(line, `","`)
			if len(fields) < 2 {
				continue
			}
			if pid := strings.Trim(fields[1], `" `); pid != self {
				count++
			}
		}
		return count
	}

	out, err := exec.Command("ps", "-ww", "-eo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("ps 枚举进程失败: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] == self {
			continue
		}
		if strings.Contains(line, mcpHelperMarker) {
			count++
		}
	}
	return count
}

// waitForHelperCount 轮询等待 helper 进程数到达期望值（超时判红并给出读数）。
// waitForHelperCount polls until the helper count settles on want.
func waitForHelperCount(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := helperProcessCount(t)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper 子进程数 = %d，期望 %d（已等 %s）", got, want, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
