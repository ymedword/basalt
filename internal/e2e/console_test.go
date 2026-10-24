//go:build e2e

package e2e

// 本文件回填 US-4 / US-5 / US-6 / US-7 的「Windows 真控制台中文」甲档（模块卡 §2.6 的
// 4b / 6b / 10b / 11b）：真控制台观感属丙类，管道 UTF-8 往返属甲类。三个真实 `chat`
// 进程分别把中文送过 argv、文件、子进程 stdout、MCP stdio 与记忆落盘，最终回答再从
// stdout 管道读回；任一环被按 ANSI 代码页截断都会在逐字比较处变红。三段分跑是因为
// shell 与 MCP 共用 `shell.env_passthrough` 白名单——替身变量不能同时喂给两个子进程。
// This file backfills the pipe-UTF-8 half of the console items for US-4/5/6/7. Three
// real `chat` processes round-trip Chinese through argv, a file, a child process, MCP
// stdio and memory. They run separately because shell and MCP share the env passthrough
// whitelist, so the shell stand-in variable must not leak into the MCP helper child.

import (
	"strings"
	"testing"
)

const (
	// consolePrompt 是命令行参数里的中文（含四字节表情与全角标点）——编解码一旦不按
	// UTF-8 走，参数边界最先破相。
	// consolePrompt is the argv-side Chinese payload (4-byte rune + full-width marks).
	consolePrompt = "管道往返：启动中文消息 🚀，请原样回显"

	// consoleReply 是模型回答里的中文；它要经 HTTP、控制台与消息表三处原样落地。
	// consoleReply is the model reply that must survive HTTP, console and the message table.
	consoleReply = "管道往返成功：中文 🚀 全角，。！？"

	// consoleNote 是工作区文件里的中文（read_file 回填给模型）。
	// consoleNote travels through read_file back into the model request.
	consoleNote = "笔记-中文🚀：文件内容必须原样回填"

	// consoleShellArg 是子进程参数里的中文（shell 替身按 `arg0=` 原样打印）。
	// consoleShellArg travels through a child process argument and its stdout.
	consoleShellArg = "子进程参数-中文🚀"

	// consoleEchoArg 是 MCP 回显工具的入参（子进程 stdio 上的一轮 JSON-RPC）。
	// consoleEchoArg travels over the MCP stdio channel and back.
	consoleEchoArg = "回声-中文🚀"

	// consoleMemory 是记忆条目的正文；落盘后再读文件逐字核对。
	// consoleMemory is the memory entry body, verified byte-for-byte on disk.
	consoleMemory = "记忆-中文🚀：管道往返"
)

// TestBackfill_ConsolePipe：单顶层测试、不拆子测试（目录以 `Parent` 形式钉住）。
// 判据：每段 stdout 逐字包含该段中文回答；模型侧收到各处中文原文；助手与用户消息
// 在库里逐字一致；记忆文件落盘原文；全部输出无 U+FFFD 替换字符。
// TestBackfill_ConsolePipe is one top-level test (the catalog pins it as `Parent`).
func TestBackfill_ConsolePipe(t *testing.T) {
	// 甲段：argv → 模型请求；文件内容 → 模型请求；记忆落盘 → 文件；最终回答 → stdout。
	// US-4 的对话环与 US-6 的记忆链在这一段闭环。
	// Segment A closes US-4's loop and US-6's memory chain.
	h := newHarness(t)
	initWorkspace(t, h)
	h.writeAgentFile(t, "demo", "notes.txt", consoleNote+"\n")
	fake := newFakeModel(t,
		toolCallResponse("call-pipe-read", "read_file", `{"path":"notes.txt"}`),
		toolCallResponse("call-pipe-memory", "save_memory", mustJSON(map[string]string{
			"content": consoleMemory,
			"scope":   "CORE",
		})),
		textResponse(consoleReply),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"read_file", "save_memory"}),
		"管道 UTF-8 往返演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", consolePrompt)
	requireExit(t, res, 0, "管道 UTF-8 往返（对话与记忆）")
	requireContains(t, res.Stdout, "⚙ 调用工具 read_file…", "工具行 UTF-8")
	requireContains(t, res.Stdout, consoleReply, "最终回答逐字到达 stdout")
	requireNoReplacementRune(t, res, "对话与记忆")

	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("甲段假模型应收到 3 次请求（2 次工具 + 1 次收尾），实际 %d 次", len(reqs))
	}
	requireContains(t, requestText(t, reqs[0]), consolePrompt, "argv 中文原样进请求")
	results := toolResults(t, reqs[len(reqs)-1])
	if len(results) != 2 {
		t.Fatalf("甲段最后一轮应带 2 条工具结果，实际 %d 条：%s",
			len(results), requestText(t, reqs[len(reqs)-1]))
	}
	requireContains(t, results[0], consoleNote, "文件内容回填（US-4）")
	requireContains(t, results[1], "已记住（作用域：Agent 层 demo；分区：核心记忆）", "记忆确认（US-6）")

	// 消息表按 UTF-8 原样落库：用户输入与助手回答都逐字相等。
	db := openDB(t, h.dbPath())
	users := queryRows(t, db, `SELECT content FROM messages WHERE role = 'user'`)
	if len(users) != 1 {
		t.Fatalf("用户消息应有 1 条，实际 %d 条：%v", len(users), users)
	}
	if got := stringValue(t, users[0], "content"); got != consolePrompt {
		t.Fatalf("用户消息落库 = %q，期望逐字等于 %q", got, consolePrompt)
	}
	// 带工具调用的助手消息 content 为空（契约允许），只取承载最终回答的那一条。
	// Assistant messages carrying tool calls have empty content; only the final one counts.
	assistants := queryRows(t, db,
		`SELECT content FROM messages WHERE role = 'assistant' AND content IS NOT NULL AND content <> ''`)
	if len(assistants) != 1 {
		t.Fatalf("助手最终回答应有 1 条，实际 %d 条：%v", len(assistants), assistants)
	}
	if got := stringValue(t, assistants[0], "content"); got != consoleReply {
		t.Fatalf("助手消息落库 = %q，期望逐字等于 %q", got, consoleReply)
	}
	requireContains(t, readText(t, memoryPath(h, "demo")), consoleMemory, "记忆条目落盘原文")

	// 乙段：中文参数进子进程，子进程 stdout 原样回到模型请求（US-5）。
	// Segment B sends the Chinese argument through a child process and back.
	hShell := newHarness(t)
	initWorkspace(t, hShell)
	exe := helperBinary(t)
	fakeShell := newFakeModel(t,
		toolCallResponse("call-pipe-shell", "shell", mustJSON(map[string]any{
			"executable": exe,
			"arguments":  []string{"report", consoleShellArg},
		})),
		textResponse("子进程往返成功"),
	)
	hShell.writeConfig(t, configWithProvider(fakeShell.URL())+shellSection(exe, []string{shellHelperEnv}))
	hShell.writeAgent(t, "demo", agentFrontmatter("demo", []string{"shell"}), "shell 管道演示")

	resShell := hShell.run(t, append(fixtureEnv(), shellHelperEnv+"=1"),
		"chat", "--agent", "demo", "--message", consolePrompt)
	requireExit(t, resShell, 0, "管道 UTF-8 往返（子进程）")
	requireContains(t, resShell.Stdout, "⚙ 调用工具 shell…", "工具行 UTF-8")
	requireContains(t, resShell.Stdout, "子进程往返成功", "乙段最终回答")
	requireNoReplacementRune(t, resShell, "子进程")
	reqsShell := fakeShell.Requests()
	if len(reqsShell) != 2 {
		t.Fatalf("乙段假模型应收到 2 次请求，实际 %d 次", len(reqsShell))
	}
	requireToolResultContains(t, reqsShell[1], "arg0="+consoleShellArg, "子进程参数与 stdout（US-5）")

	// 丙段：中文入参经 MCP stdio 交给 helper 子进程再回显（US-7）。
	// Segment C round-trips Chinese through the MCP stdio channel.
	hMCP := newHarness(t)
	initWorkspace(t, hMCP)
	hMCP.writeMCPServers(t, mcpServersYAML(mcpHelperServer))
	fakeMCP := newFakeModel(t,
		toolCallResponse("call-pipe-echo", mcpEchoTool, mustJSON(map[string]string{"text": consoleEchoArg})),
		textResponse("MCP 往返成功"),
	)
	hMCP.writeConfig(t, configWithProvider(fakeMCP.URL())+mcpShellAllowed())
	hMCP.writeAgent(t, "demo", mcpAgentFrontmatter("demo", []string{mcpEchoTool}, []string{mcpHelperServer}),
		"MCP 管道演示")

	resMCP := hMCP.run(t, mcpHelperEnv(), "chat", "--agent", "demo", "--message", consolePrompt)
	requireExit(t, resMCP, 0, "管道 UTF-8 往返（MCP）")
	requireContains(t, resMCP.Stderr, "mcp_connected=1", "MCP 子进程确实连上")
	requireContains(t, resMCP.Stdout, "MCP 往返成功", "丙段最终回答")
	requireNoReplacementRune(t, resMCP, "MCP")
	reqsMCP := fakeMCP.Requests()
	if len(reqsMCP) != 2 {
		t.Fatalf("丙段假模型应收到 2 次请求，实际 %d 次", len(reqsMCP))
	}
	requireToolResultContains(t, reqsMCP[1], consoleEchoArg, "MCP 回显（US-7）")
}

// requireNoReplacementRune 断言两路输出里没有 U+FFFD：出现即说明某一跳不是 UTF-8。
// requireNoReplacementRune fails on U+FFFD, which marks a non-UTF-8 hop.
func requireNoReplacementRune(t *testing.T, res runResult, label string) {
	t.Helper()
	if combined := res.Stdout + "\n" + res.Stderr; strings.ContainsRune(combined, '�') {
		t.Fatalf("%s：输出出现 U+FFFD 替换字符，编码链路不是 UTF-8\n%s", label, combined)
	}
}
