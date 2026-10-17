package cli

// 本文件对应颗粒度文档 §3.2 的 cli/chat 八条（R1~R8）。全部经**注入的假端口**判卷
// （假 `RunService` 记录收到的请求、假 `SessionLister` 持有会话历史）——cli 因此不需要
// import workspace / storage，也就能在毫秒级跑完一轮"真运行"。
// The eight chat cases run against injected fakes: the fake run service records what it
// received and the fake lister holds the history, so cli imports neither workspace nor
// storage and a full turn costs milliseconds.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeRunService 是 RunService 端口的判卷替身：按序交出预置结果 / 预置错误，并记下每一次
// 收到的请求（**收到什么就是命令面交出了什么**，因此"派生在准入之前"这类判据直接读它）。
// fakeRunService hands out canned outcomes in order and records every request it
// received, which is exactly what the command surface handed over.
type fakeRunService struct {
	replies []core.RunResult
	errs    []error
	onCall  func(ctx context.Context, req core.RunRequest)

	requests []core.RunRequest
}

func (f *fakeRunService) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	i := len(f.requests)
	f.requests = append(f.requests, req)

	if f.onCall != nil {
		f.onCall(ctx, req)
	}
	if i < len(f.errs) && f.errs[i] != nil {
		return core.RunResult{}, f.errs[i]
	}
	if i < len(f.replies) {
		return f.replies[i], nil
	}
	return core.RunResult{Status: core.StatusCompleted}, nil
}

// chatFixture 是一整套注入依赖：假运行面端口 + 一条可控中断通道 + 缓冲日志。
// chatFixture wires the injected run-face ports, an interrupt channel and a buffered
// logger.
type chatFixture struct {
	deps    Deps
	runs    *fakeRunService
	lister  *fakeSessionLister
	signals chan os.Signal
	logs    bytes.Buffer
}

// newChatFixture 装配 fixture；其余端口（Store / Workspace / Agents）一律留 nil——chat
// 只该碰运行面两个端口，碰到别的就是缺陷（nil 接口调用当场 panic，比悄悄通过更早也更响）。
// Every other port stays nil on purpose: chat must touch the two run-face ports only.
func newChatFixture(t *testing.T, workDir string) *chatFixture {
	t.Helper()

	f := &chatFixture{
		runs:    &fakeRunService{},
		lister:  &fakeSessionLister{messages: map[string][]core.Message{}},
		signals: make(chan os.Signal, 1),
	}
	f.deps = Deps{
		WorkDir:                workDir,
		SupportedSchemaVersion: 7,
		Logger:                 slog.New(slog.NewTextHandler(&f.logs, nil)),
		RunService:             f.runs,
		SessionLister:          f.lister,
		Signals:                f.signals,
	}
	return f
}

// chatRun 是一次 chat 运行的返回错误与三路输出。
// chatRun captures the error and the three output tracks of one run.
type chatRun struct {
	err    error
	stdout string
	stderr string
	logs   string
}

// executeChat 跑一次 chat（背景上下文）。
// executeChat runs one chat with a background context.
func executeChat(t *testing.T, f *chatFixture, stdin string, args ...string) chatRun {
	t.Helper()
	return executeChatContext(t, context.Background(), f, stdin, args...)
}

// executeChatContext 在注入的依赖上跑一次**真根命令**：标准流走一对管道（渠道层要求
// `*os.File`——真控制台与管道在那个签名上就是同一种介质，分流由它按句柄判定），标准输入
// 预置后立即关掉写端，交互模式因此看得到"输入结束"。
// The real root command runs against pipe pairs, since the channel layer takes *os.File;
// stdin is preloaded and closed so the interactive path sees its end of input.
func executeChatContext(t *testing.T, ctx context.Context, f *chatFixture, stdin string, args ...string) chatRun {
	t.Helper()

	inR, inW, err := os.Pipe()
	require.NoError(t, err, "建输入管道")
	outR, outW, err := os.Pipe()
	require.NoError(t, err, "建输出管道")
	t.Cleanup(func() {
		require.NoError(t, inR.Close())
		require.NoError(t, outR.Close())
	})

	if stdin != "" {
		_, err = io.WriteString(inW, stdin)
		require.NoError(t, err, "预置标准输入")
	}
	require.NoError(t, inW.Close(), "关掉标准输入写端——EOF 是三种正常收场里的一种")

	deps := f.deps
	deps.In, deps.Out = inR, outW

	root := NewRootCommand(deps)
	root.SetArgs(args)
	var stderr bytes.Buffer
	root.SetErr(&stderr)

	err = root.ExecuteContext(ctx)

	require.NoError(t, outW.Close(), "命令结束后关掉输出写端，读取侧才看得到 EOF")
	out, readErr := io.ReadAll(outR)
	require.NoError(t, readErr, "读回标准输出")

	return chatRun{err: err, stdout: string(out), stderr: stderr.String(), logs: f.logs.String()}
}

// R1 `--message` 单条即退：跑一轮就收场，不读标准输入；请求形状（Agent / 渠道 / 用户 /
// 会话标识 / 消息）逐项对得上。附带两问：预算从哪来、配置读不出来时怎么办。
// R1 one-shot mode runs exactly one turn without touching stdin, plus the budget
// source question.
func TestChat_MessageFlagRunsOnceAndExits(t *testing.T) {
	t.Run("单条即退：不读标准输入，请求形状完整", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{
			RunID: "cli-1-a1b2c3", Status: core.StatusCompleted, FinalText: "收到。",
		}}

		run := executeChat(t, f, "这行不该被读走\n", "chat", "--agent", "notes-agent", "--message", "你好")

		require.NoError(t, run.err, "正常收场（退出码 0）；stderr=%s", run.stderr)
		assert.Contains(t, run.stdout, "收到。", "单轮回复直接打正文")
		require.Len(t, f.runs.requests, 1, "恰好一次运行")

		req := f.runs.requests[0]
		assert.Equal(t, "notes-agent", req.Agent)
		assert.Equal(t, "你好", req.Message, "输入取自 --message 而不是标准输入")
		assert.Equal(t, "cli", req.Channel, "触发源由渠道映射")
		assert.NotEmpty(t, req.UserID, "用户标识由渠道层解析（OS 用户名或回落 unknown）")
		assert.Equal(t, deriveSessionID("cli", req.UserID, "notes-agent"), req.SessionID,
			"会话标识在命令面派生后随请求交给运行入口")
		assert.Empty(t, req.ConfigHash, "配置哈希由运行时填充，命令面不填")
		// 两次读：首轮前预读一次（把游标对齐到既有历史之后，2026-10-16 用户裁决的渲染修复），
		// 运行后再读一次取本轮新增——两处都只为取工具行与部分结果，不改任何状态。
		assert.Equal(t, []string{"Messages:" + req.SessionID, "Messages:" + req.SessionID}, f.lister.calls,
			"读回历史只为取本轮新增的工具行与部分结果")
	})

	t.Run("预算取自 basalt.yaml 的 budget 段", func(t *testing.T) {
		t.Setenv("BASALT_CHAT_KEY", "resolved-9f2a")
		dir := t.TempDir()
		writeProviderConfig(t, dir, `log:
  level: info
budget:
  max_iterations: 3
  max_tool_calls: 7
  max_output_bytes: 4096
  context_tokens: 2048
providers:
  - name: alpha
    base_url: https://alpha.example.com/v1
    api_key: ${BASALT_CHAT_KEY}
`)

		f := newChatFixture(t, dir)
		f.runs.replies = []core.RunResult{{Status: core.StatusCompleted, FinalText: "好。"}}

		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

		require.NoError(t, run.err, "stderr=%s", run.stderr)
		require.Len(t, f.runs.requests, 1)
		assert.Equal(t, core.Budget{
			MaxIterations:  3,
			MaxToolCalls:   7,
			MaxOutputBytes: 4096,
			ContextTokens:  2048,
		}, f.runs.requests[0].Budget, "命令行运行读的就是运维写在配置里的预算")
	})

	t.Run("无配置：零预算 + WARN，运行照跑", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{Status: core.StatusCompleted, FinalText: "好。"}}

		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

		require.NoError(t, run.err, "无配置是宽容的（与 status / profile 同一种）；stderr=%s", run.stderr)
		require.Len(t, f.runs.requests, 1)
		assert.Equal(t, core.Budget{}, f.runs.requests[0].Budget,
			"缺省值由运行入口统一补齐——本层不补，避免同一项两个缺省来源")
		assert.Contains(t, run.logs, "未发现配置，预算按缺省值执行", "宽容要留痕")
	})

	t.Run("配置无效：fail-closed，模型一次都没调", func(t *testing.T) {
		dir := t.TempDir()
		writeProviderConfig(t, dir, "log:\n  level: 超级详细\n")

		f := newChatFixture(t, dir)
		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

		require.Error(t, run.err, "预算读不出来时不能默认套一套替配置者定的上限")
		assert.Empty(t, f.runs.requests, "零次运行")
		assert.Contains(t, run.logs, "chat 未通过：配置无效", "失败路径留痕")
	})
}

// R2 渲染模板：工具调用提示行只给名字（入参与结果全文都不上屏）、单轮回复直接打正文、
// 失败行、达上限与取消的醒目提示 + 已得部分结果；多轮之间只渲染新增部分。
// R2 the rendering templates, including the "one ⚙ line, never the arguments or the
// tool output" rule and the per-turn diff.
func TestChat_RendersEveryTurnWithoutLeakingToolIO(t *testing.T) {
	// 两个哨兵串分别藏在"模型给的入参"与"工具产物"里：只要有一个上了屏，用例就红。
	const (
		argsMarker = "ARGSECRET-8c1d"
		bodyMarker = "TOOLBODY-4e7f"
	)

	t.Run("工具行只给名字：入参与结果全文都不上屏", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{
			RunID: "cli-1-a1b2c3", Status: core.StatusCompleted, FinalText: "文件里写的是 42。",
		}}
		f.runs.onCall = func(_ context.Context, req core.RunRequest) {
			hist := f.lister.messages[req.SessionID]
			hist = append(hist,
				core.Message{Role: core.RoleUser, Content: req.Message},
				core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
					ID: "call-1", Name: "read_file", Arguments: `{"path":"` + argsMarker + `.txt"}`,
				}}},
				core.Message{Role: core.RoleTool, ToolCallID: "call-1", Content: bodyMarker + ": 42"},
			)
			f.lister.messages[req.SessionID] = hist
		}

		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "读一下 notes.txt 并总结")

		require.NoError(t, run.err, "stderr=%s", run.stderr)
		assert.Contains(t, run.stdout, "⚙ 调用工具 read_file…", "工具调用一行提示，名字逐字")
		assert.Contains(t, run.stdout, "文件里写的是 42。", "单轮回复直接打正文")
		assert.NotContains(t, run.stdout, argsMarker, "入参不得上屏（需求文档 §5.2 脱敏纪律）")
		assert.NotContains(t, run.stdout, bodyMarker, "工具结果全文不得上屏")
	})

	t.Run("多轮：每轮只渲染本轮新增，工具行不重印", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{
			{RunID: "cli-1-a1b2c3", Status: core.StatusCompleted, FinalText: "一答"},
			{RunID: "cli-2-d4e5f6", Status: core.StatusCompleted, FinalText: "二答"},
		}
		f.runs.onCall = func(_ context.Context, req core.RunRequest) {
			hist := f.lister.messages[req.SessionID]
			switch req.Message {
			case "第一问":
				hist = append(hist,
					core.Message{Role: core.RoleUser, Content: req.Message},
					core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
						ID: "call-1", Name: "read_file", Arguments: `{"path":"` + argsMarker + `.txt"}`,
					}}},
					core.Message{Role: core.RoleTool, ToolCallID: "call-1", Content: bodyMarker},
				)
			case "第二问":
				hist = append(hist,
					core.Message{Role: core.RoleUser, Content: req.Message},
					core.Message{Role: core.RoleAssistant, Content: "二答"},
				)
			}
			f.lister.messages[req.SessionID] = hist
		}

		run := executeChat(t, f, "第一问\n第二问\n", "chat", "--agent", "notes-agent")

		require.NoError(t, run.err, "EOF 收场（退出码 0）；stderr=%s", run.stderr)
		require.Len(t, f.runs.requests, 2, "两行输入跑两轮")
		assert.Equal(t, "第一问", f.runs.requests[0].Message)
		assert.Equal(t, "第二问", f.runs.requests[1].Message)
		assert.Contains(t, run.stdout, "一答")
		assert.Contains(t, run.stdout, "二答")
		assert.Equal(t, 1, strings.Count(run.stdout, "⚙"), "工具行属于第一轮——第二轮不得重印")
		assert.NotContains(t, run.stdout, bodyMarker)
	})

	t.Run("新进程对既有会话：首轮不重放历史工具行", func(t *testing.T) {
		// 两次 executeChat = 两个进程（两个 chatRunner）先后对**同一会话**开跑。游标从零起又
		// 不预读时，第二个进程会把上一进程写的工具行当"本轮新增"重放（T066 端到端实测发现，
		// 2026-10-16 用户裁决修）——本子例钉住预读对齐后的行为。
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{
			{RunID: "cli-1-a1b2c3", Status: core.StatusCompleted, FinalText: "一答"},
			{RunID: "cli-2-d4e5f6", Status: core.StatusCompleted, FinalText: "二答"},
		}
		f.runs.onCall = func(_ context.Context, req core.RunRequest) {
			hist := f.lister.messages[req.SessionID]
			switch req.Message {
			case "第一问":
				hist = append(hist,
					core.Message{Role: core.RoleUser, Content: req.Message},
					core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
						ID: "call-1", Name: "read_file", Arguments: `{"path":"` + argsMarker + `.txt"}`,
					}}},
					core.Message{Role: core.RoleTool, ToolCallID: "call-1", Content: bodyMarker},
				)
			case "第二问":
				// 第二个进程这一轮**没有**工具调用：屏幕上出现的任何工具行都只可能是历史重放。
				hist = append(hist,
					core.Message{Role: core.RoleUser, Content: req.Message},
					core.Message{Role: core.RoleAssistant, Content: "二答"},
				)
			}
			f.lister.messages[req.SessionID] = hist
		}

		first := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "第一问")
		require.NoError(t, first.err, "stderr=%s", first.stderr)
		require.Equal(t, 1, strings.Count(first.stdout, "⚙"), "第一轮：本轮自己的调用照常给一行")

		second := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "第二问")
		require.NoError(t, second.err, "stderr=%s", second.stderr)
		assert.Contains(t, second.stdout, "二答")
		assert.NotContains(t, second.stdout, "⚙",
			"新进程首轮只渲染本轮新增：预读对齐游标后，历史里的工具行不得重放")
	})

	t.Run("失败行：✗ + 原因", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{
			RunID: "cli-1-a1b2c3", Status: core.StatusFailed, Reason: "运行内部错误",
		}}

		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

		require.Error(t, run.err, "运行期失败给非零退出码（见 R3）")
		assert.Contains(t, run.stdout, "✗ 运行内部错误", "失败行逐字")
	})

	t.Run("达上限：醒目提示 + 已得部分结果", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{
			RunID: "cli-1-a1b2c3", Status: core.StatusLimitReached, Reason: "超过最大迭代次数",
		}}
		f.runs.onCall = partialTurn(f)

		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "写一篇长文")

		require.NoError(t, run.err, "达上限是预期终止、不是错误（契约 §1.3）；stderr=%s", run.stderr)
		assert.Contains(t, run.stdout, "⚠ 已达上限：超过最大迭代次数")
		assert.Contains(t, run.stdout, "先给到这里的部分结果。", "提示之后要跟已得的部分结果")
	})

	t.Run("取消：醒目提示 + 已得部分结果", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{
			RunID: "cli-1-a1b2c3", Status: core.StatusCancelled, Reason: "运行被取消",
		}}
		f.runs.onCall = partialTurn(f)

		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "写一篇长文")

		require.NoError(t, run.err, "取消是预期动作、不是错误；stderr=%s", run.stderr)
		assert.Contains(t, run.stdout, "已取消")
		assert.Contains(t, run.stdout, "⚠", "口径要求醒目")
		assert.Contains(t, run.stdout, "先给到这里的部分结果。")
	})

	t.Run("未知运行状态：如实打出状态名", func(t *testing.T) {
		// 运行状态是五值集合（契约 §2）：出现第六个值说明契约变了而渲染层没跟上——
		// 如实打出状态名，比默默当成功、或用某个分支冒充事实都更可查。
		text, next := renderTurn(core.RunResult{Status: core.RunStatus("paused")}, nil, 0)
		assert.Equal(t, "✗ 未知运行状态 paused", text)
		assert.Equal(t, 0, next)
	})
}

// partialTurn 让假运行写入"模型在中途说过的一段话"：`limit_reached` / `cancelled` 两种
// 终态的返回结构收不下部分文本（FinalText 恒空），可展示的部分结果只能从会话历史里取。
// partialTurn writes the partial answer into the history, which is the only place it
// survives the two statuses whose result struct cannot carry it.
func partialTurn(f *chatFixture) func(context.Context, core.RunRequest) {
	return func(_ context.Context, req core.RunRequest) {
		hist := f.lister.messages[req.SessionID]
		hist = append(hist,
			core.Message{Role: core.RoleUser, Content: req.Message},
			core.Message{Role: core.RoleAssistant, Content: "先给到这里的部分结果。"},
		)
		f.lister.messages[req.SessionID] = hist
	}
}

// R3 退出码：可预期的终止一律 0（完成 / 达上限 / 取消），运行期失败给非零；调用方问题
// 的 error 原样上抛（由 app.Run 映射成 1）。
// R3 every expected termination exits zero; a run-time failure does not.
func TestChat_ExitCodeTracksRunOutcome(t *testing.T) {
	cases := []struct {
		name    string
		reply   core.RunResult
		wantErr bool
	}{
		{"completed → 0", core.RunResult{Status: core.StatusCompleted, FinalText: "好。"}, false},
		{"limit_reached → 0（达上限不是错误）", core.RunResult{Status: core.StatusLimitReached, Reason: "工具调用次数超限"}, false},
		{"cancelled → 0（取消是预期动作）", core.RunResult{Status: core.StatusCancelled, Reason: "运行被取消"}, false},
		{"failed → 非零", core.RunResult{RunID: "cli-1-a1b2c3", Status: core.StatusFailed, Reason: "运行内部错误"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newChatFixture(t, t.TempDir())
			f.runs.replies = []core.RunResult{tc.reply}

			run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

			if !tc.wantErr {
				require.NoError(t, run.err, "stderr=%s", run.stderr)
				return
			}
			require.Error(t, run.err, "运行期失败要给出非零退出码（app.Run 把 error 映射为 1）")
			assert.Contains(t, run.err.Error(), tc.reply.Reason, "错误点名原因")
			assert.Contains(t, run.stdout, "✗ "+tc.reply.Reason, "退出码之外还留有可读的失败行")
		})
	}

	t.Run("调用方问题：error 原样上抛", func(t *testing.T) {
		sentinel := errors.New("同会话忙：请稍后再试")
		f := newChatFixture(t, t.TempDir())
		f.runs.errs = []error{sentinel}

		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

		require.ErrorIs(t, run.err, sentinel, "运行入口的调用方问题不包装、不吞、不排队")
		assert.Empty(t, run.stdout, "跑都没跑起来，不该有半截回复")
	})
}

// R4 Ctrl-C → 该次运行 `cancelled` 且**进程退出码 0**：中断经注入的通道送达，取消的正是
// 正在跑的这一次运行，落库的历史不受影响。
// R4 an interrupt cancels the run in flight and the process still exits zero.
func TestChat_InterruptCancelsRunAndExitsZero(t *testing.T) {
	f := newChatFixture(t, t.TempDir())
	var cancelled bool
	f.runs.onCall = func(ctx context.Context, _ core.RunRequest) {
		// 中断必须落在**运行中**：等运行已经进了这一轮（渠道的取消槽位已就位）再送信号——
		// 停在提示符上的 Ctrl-C 是另一条路径，不在本判据内。
		f.signals <- os.Interrupt
		select {
		case <-ctx.Done():
			cancelled = true
		case <-time.After(5 * time.Second):
			// 不设兜底的话，中断没送达会以整仓超时的形式出现（难定位）；5 秒足够。
		}
	}
	f.runs.replies = []core.RunResult{{
		RunID: "cli-1-a1b2c3", Status: core.StatusCancelled, Reason: "运行被取消",
	}}

	run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "帮我跑个长任务")

	require.NoError(t, run.err, "取消是预期动作而非错误：进程退出码 0")
	assert.True(t, cancelled, "Ctrl-C 取消的正是这一次运行（运行内部的 ctx 被取消）")
	assert.Contains(t, run.stdout, "已取消", "该次运行打印取消提示")
}

// R5 派生无碰撞：长度前缀消除拼接歧义——`user=a-b, profile=c` 与 `user=a, profile=b-c`
// 直接拼接出的字节完全一样（§4.3 判据），只有把每段长度也编进元组才会落到不同摘要上。
// R5 the length prefix is the whole point of the canonical tuple encoding.
func TestDeriveSessionID_AdjacentTuplesDoNotCollide(t *testing.T) {
	cases := []struct {
		name            string
		userA, profileA string
		userB, profileB string
	}{
		{"连字符相邻", "a-b", "c", "a", "b-c"},
		{"空段相邻", "a", "bc", "ab", "c"},
		{"前后缀互借", "ab", "c", "a", "bc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := deriveSessionID("cli", tc.userA, tc.profileA)
			b := deriveSessionID("cli", tc.userB, tc.profileB)
			assert.NotEqual(t, a, b, "(%q,%q) 与 (%q,%q) 必须落到不同标识",
				tc.userA, tc.profileA, tc.userB, tc.profileB)
		})
	}

	t.Run("同参数总是同标识（无随机、无时间）", func(t *testing.T) {
		assert.Equal(t, deriveSessionID("cli", "yunwei", "notes-agent"), deriveSessionID("cli", "yunwei", "notes-agent"),
			"派生是纯函数——否则重启续会话就续不上")
	})

	t.Run("黄金值：口径 1 的逐字节形态", func(t *testing.T) {
		// 期望值由口径 1 的字节串独立算出：sha256("basalt/session/v1" ‖ 0x1F ‖ "3:cli" ‖
		// 0x1F ‖ "6:yunwei" ‖ 0x1F ‖ "11:notes-agent") 的十六进制前 16 位。写死它，是为了
		// 让"改域串 / 改分隔符 / 改长度前缀形态"这类改动当场现形。
		assert.Equal(t, "cli-a2025c15b5cbc303", deriveSessionID("cli", "yunwei", "notes-agent"))
		assert.Len(t, deriveSessionID("cli", "yunwei", "notes-agent"), len("cli-")+16)
	})
}

// R6 字符集安全：用户标识是任意输入（可能含路径分隔符、控制字符、超长串），派生结果必须
// 永远只含 `[a-f0-9]` 与渠道前缀——它要当文件名与日志字段用。
// R6 the derived id stays safe to use as a file name and a log field.
func TestDeriveSessionID_ResultIsPathAndLogSafe(t *testing.T) {
	shape := regexp.MustCompile(`^cli-[a-f0-9]{16}$`)
	hostile := []string{
		"",
		"../../etc/passwd",
		`..\..\windows\system32`,
		"a b",
		"运维用户",
		"line\nbreak",
		"tab\tsep",
		"ctl\x1fsep",
		strings.Repeat("x", 4096),
	}
	for _, user := range hostile {
		assert.Regexp(t, shape, deriveSessionID("cli", user, "notes-agent"), "用户标识 %q 折成安全标识", user)
	}
}

// R7 派生在**准入之前**：运行入口收到的标识已经是命令面算好的值（空串保留"无状态"语义，
// 运行入口不代为派生），且同一组 (渠道, 用户, 画像) 跨次不同消息仍落到同一会话。
// R7 the id is derived by the command surface ahead of admission, from the tuple alone.
func TestChat_DerivesSessionIDBeforeAdmission(t *testing.T) {
	f := newChatFixture(t, t.TempDir())
	f.runs.replies = []core.RunResult{
		{RunID: "cli-1-a1b2c3", Status: core.StatusCompleted, FinalText: "一。"},
		{RunID: "cli-2-d4e5f6", Status: core.StatusCompleted, FinalText: "二。"},
	}

	first := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "第一条")
	require.NoError(t, first.err, "stderr=%s", first.stderr)
	second := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "第二条")
	require.NoError(t, second.err, "stderr=%s", second.stderr)

	require.Len(t, f.runs.requests, 2)
	firstReq, secondReq := f.runs.requests[0], f.runs.requests[1]

	assert.NotEmpty(t, firstReq.SessionID, "运行入口收到的是算好的标识，不是空串")
	assert.Equal(t, deriveSessionID("cli", firstReq.UserID, "notes-agent"), firstReq.SessionID,
		"派生只用 (渠道, 用户, 画像) 三元组")
	assert.NotEqual(t, firstReq.Message, secondReq.Message, "两次的消息确实不同")
	assert.Equal(t, firstReq.SessionID, secondReq.SessionID, "消息不同、会话相同——两次落到同一会话")
	// 每个进程各两次读（首轮前预读对齐游标 + 运行后读回新增），读的都是同一个派生标识。
	assert.Equal(t,
		[]string{
			"Messages:" + firstReq.SessionID, "Messages:" + firstReq.SessionID,
			"Messages:" + secondReq.SessionID, "Messages:" + secondReq.SessionID,
		},
		f.lister.calls)
}

// R8 时限：每次运行都建 60s 截止（Ctrl-C 不能替代资源时限）；调用方已有更早截止时按更早的
// 那个（`context.WithTimeout` 天然取小），并且那个更早的截止真能送达运行内部。
// R8 every run gets its own deadline, and an earlier caller deadline wins.
func TestChat_GivesEveryRunItsOwnDeadline(t *testing.T) {
	t.Run("每次运行建 60s 时限", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{Status: core.StatusCompleted, FinalText: "好。"}}
		var (
			hasDeadline bool
			remaining   time.Duration
		)
		f.runs.onCall = func(ctx context.Context, _ core.RunRequest) {
			deadline, ok := ctx.Deadline()
			hasDeadline = ok
			if ok {
				remaining = time.Until(deadline)
			}
		}

		run := executeChat(t, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

		require.NoError(t, run.err, "stderr=%s", run.stderr)
		require.True(t, hasDeadline, "每次运行都要有截止时间")
		assert.Greater(t, remaining, 55*time.Second, "接近 60s")
		assert.LessOrEqual(t, remaining, 60*time.Second, "上限就是 60s，不该更长")
	})

	t.Run("调用方更早截止：取更早者", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{Status: core.StatusCompleted, FinalText: "好。"}}
		var remaining time.Duration
		f.runs.onCall = func(ctx context.Context, _ core.RunRequest) {
			if deadline, ok := ctx.Deadline(); ok {
				remaining = time.Until(deadline)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		run := executeChatContext(t, ctx, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

		require.NoError(t, run.err, "stderr=%s", run.stderr)
		require.Len(t, f.runs.requests, 1)
		assert.LessOrEqual(t, remaining, 2*time.Second, "调用方更早的截止胜出")
		assert.Greater(t, remaining, 1500*time.Millisecond, "取的是调用方那一个，不是 60s 缺省")
	})

	t.Run("更早的截止真能送达运行内部", func(t *testing.T) {
		f := newChatFixture(t, t.TempDir())
		f.runs.replies = []core.RunResult{{
			RunID: "cli-1-a1b2c3", Status: core.StatusCancelled, Reason: "超过运行时限",
		}}
		var sawDeadlineExceeded bool
		f.runs.onCall = func(ctx context.Context, _ core.RunRequest) {
			<-ctx.Done()
			sawDeadlineExceeded = errors.Is(ctx.Err(), context.DeadlineExceeded)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		run := executeChatContext(t, ctx, f, "", "chat", "--agent", "notes-agent", "--message", "在吗")

		assert.True(t, sawDeadlineExceeded, "运行内部看到的是截止时间到（不是显式取消）")
		// 调用方自己的 ctx 已经到点，循环层往外写回复也会随之失败（渠道的 Send 尊重 ctx）；
		// 真命令面踩不到这一条（root 的 ctx 是 Background），注入 ctx 的调用方才会——
		// 收口报告已登记该边界。
		require.ErrorIs(t, run.err, context.DeadlineExceeded, "调用方收摊后没人该继续往下走")
		assert.Empty(t, run.stdout)
	})
}

// 编译期桩：假端口只在本文件用；端口形状变化时在这里先红。
var _ RunService = (*fakeRunService)(nil)
