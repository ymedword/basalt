package cli

// 本文件是 `chat` 命令（颗粒度文档 §3.1 cli 表 + §6.2 固定口径 1 / 3 / 4 + §2.4 注 7）。
// 交互循环本身在渠道层（`channel.CliChannel.Loop`：读一行 → 跑一轮 → 写回复 → 再看中断），
// 本层承担另三件事——**会话标识派生**（准入之前算好）、**每次运行的 60s 时限**、
// **运行结果 → 文本**的渲染。
// The chat command: the loop lives in the channel layer; this file derives the session id
// ahead of admission, gives every run its 60-second deadline, and renders run results.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/channel"
	"github.com/ymedword/basalt/internal/core"
)

const (
	// chatRunTimeout 是每次运行的时限（§6.2 固定口径 3 / §2.4 注 7）。调用方已有更早截止时
	// `context.WithTimeout` 取更早者——Ctrl-C 是取消、不是资源时限，两者互不替代。
	// chatRunTimeout is the per-run deadline; an earlier caller deadline wins.
	chatRunTimeout = 60 * time.Second

	// sessionIDDomain 是派生哈希的域前缀（§6.2 固定口径 1 逐字）。
	sessionIDDomain = "basalt/session/v1"
	// sessionIDSep 是规范元组编码的字段分隔字节（0x1F，US——域内不会自然出现的控制字符）。
	sessionIDSep = 0x1F
	// sessionIDHashHexLen 是取十六进制**前 16 位**（§6.2 固定口径 1 逐字）。
	sessionIDHashHexLen = 16

	// toolLineFormat 是工具调用提示行（§6.2 固定口径 4 逐字：`⚙ 调用工具 <name>…`）——
	// **只打工具名**：入参是模型给的内容、结果是工具产物（可能是整篇文件），两者都不落屏
	// （需求文档 §5.2 的脱敏纪律，约束 26）。
	toolLineFormat = "⚙ 调用工具 %s…"
	// failLineFormat 是失败行（§6.2 固定口径 4 逐字：`✗ <四段式/错误文本>`）。
	failLineFormat = "✗ %s"
	// cancelledLine 是取消的醒目提示：文本取自固定口径 3 的 `已取消`，前面加醒目标记——
	// 口径 4 要求"醒目提示"，口径 3 给出文本，两者都要在。
	cancelledLine = "⚠ 已取消"
	// limitLineFormat 是达上限的醒目提示（§6.2 固定口径 4；契约 §1.3：达上限不是错误，
	// 提示之后要跟已得的部分结果）。
	limitLineFormat = "⚠ 已达上限：%s"
)

// newChatCmd 组装 chat 命令。
// newChatCmd builds the chat command.
//
// 两个旗标是命令面的**全部开关**：`--agent` 选画像（它同时是会话标识派生的第三元），
// `--message` 交一条即退。标准流与中断源不在旗标上——它们走 Deps 注入（可判卷）。
// Two flags are the whole surface: which agent, and the optional one-shot message.
func newChatCmd(deps Deps) *cobra.Command {
	var (
		agentName string
		message   string
	)
	cmd := &cobra.Command{
		Use:   "chat",
		Short: "与一个 Agent 对话（交互多轮；--message 单条即退）",
		Long: "与一个 Agent 多轮对话：每行输入触发一次运行，回复打在标准输出上。\n" +
			"标准输入结束（Ctrl-D / 管道读完）正常退出；--message 给出一条消息即跑即退；\n" +
			"Ctrl-C 取消**当前这一次运行**（历史已落库、不受影响）并以退出码 0 收场。\n" +
			"Chat with one agent: one run per input line, Ctrl-C cancels the run in flight.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runChat(cmd.Context(), deps, agentName, message)
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "Agent 名（必填：它同时是会话标识派生的画像名）")
	cmd.Flags().StringVar(&message, "message", "", "单条消息：给出后不读标准输入，跑完即退")
	return cmd
}

// runChat 装载预算、构造渠道、把每一轮交给 chatRunner。
// runChat loads the budget, builds the channel and hands every turn to the runner.
func runChat(ctx context.Context, deps Deps, agentName, message string) error {
	if agentName == "" {
		return fmt.Errorf("必须指定 --agent <name>：会话标识由「渠道 + 用户 + 画像」派生，画像名取该 Agent 的 AgentName——用 profile list 查看已有 Agent")
	}
	if deps.RunService == nil || deps.SessionLister == nil {
		// 端口缺一就跑不动（一个跑运行、一个读历史）；这属装配缺陷，明说而不是跑出个空回复。
		return fmt.Errorf("运行面端口未注入——chat 需要 RunService 与 SessionLister（装配层 internal/app 提供）")
	}

	budget, err := chatBudget(deps)
	if err != nil {
		return err
	}

	ch := channel.NewCliChannel(channel.CliOptions{
		In:      deps.In,
		Out:     deps.Out,
		Message: message,
		Signals: deps.Signals,
		Logger:  deps.Logger,
	})
	runner := &chatRunner{
		deps:    deps,
		agent:   agentName,
		channel: ch.Name(),
		budget:  budget,
	}

	if err := ch.Loop(ctx, runner.handle); err != nil {
		return err
	}
	// 退出码：可预期的终止（标准输入结束 / 单条 / Ctrl-C）一律 0——取消是预期动作而非错误
	// （§6.2 固定口径 3），达上限不是错误（契约 §1.3）。**运行期失败**（`failed`）则非零：
	// 失败行已经渲染在标准输出上（口径 4），这里补上脚本与监控看得见的那个信号。
	// Every expected termination exits zero; a run-time failure does not — the rendered
	// line says what happened, the exit code says that something did.
	return runner.failure
}

// chatRunner 承载一轮运行的全部命令面状态。
// chatRunner holds the command surface's per-turn state.
type chatRunner struct {
	deps    Deps
	agent   string
	channel string
	budget  core.Budget

	// rendered 是"已经渲染过的消息条数"：每次运行返回后按**位置**增量确认本轮新增的消息，
	// 工具调用提示行与部分结果都从那里读（见 ports.go 里 SessionLister 的说明）。
	// rendered is the count of messages already shown; each turn diffs by position.
	rendered int

	// seeded 标记游标是否已对齐既有历史（见 seedCursor）。为零值即"还没对齐"。
	// seeded marks whether the cursor has been aligned with the pre-existing history.
	seeded bool

	// failure 是会话中出现过的**运行期失败**——循环收场后据此定退出码。
	// failure remembers a run-time failure so the process can exit non-zero.
	failure error
}

// handle 跑一轮：派生会话标识 → 建 60s 时限 → 调运行入口 → 读回消息 → 渲染。
// handle runs one turn: derive, deadline, run, read back, render.
func (r *chatRunner) handle(ctx context.Context, msg channel.Message) (channel.Reply, error) {
	// ① 会话标识在**准入之前**算好：运行入口不派生（空串 = 无状态），命令面是唯一派生点
	// （§6.2 固定口径 1 / FR-023）。
	sessionID := deriveSessionID(r.channel, msg.UserID, r.agent)

	// ①′ 首轮先把游标对齐到既有历史之后：渲染口径是"本轮新增"（§6.2 固定口径 4），而新进程
	// 对**既有会话**开跑时历史里可能躺着上一进程写的工具调用——不预读就会把那几行当本轮重放
	// （T066 端到端实测发现，2026-10-16 用户裁决修）。预读失败只影响工具行取数，记 WARN、
	// 游标留在 0（退回"整段历史渲染"），不拦住这一轮。
	// The cursor is aligned before the first run so a resumed session never replays old tool
	// lines; a failed pre-read degrades to rendering the whole history and never blocks.
	if !r.seeded {
		r.seeded = true
		if history, seedErr := r.deps.SessionLister.Messages(ctx, sessionID); seedErr != nil {
			r.deps.Logger.Warn("chat 预读会话历史失败，首轮按整段历史渲染",
				"session_id", sessionID, "error", seedErr)
		} else {
			r.rendered = len(history)
		}
	}

	// ② 每次运行单独建时限；父上下文更早截止时按更早的那个（WithTimeout 天然取小）。
	runCtx, cancel := context.WithTimeout(ctx, chatRunTimeout)
	defer cancel()

	// ③ 唯一运行入口。调用方问题（请求非法 / Agent 不存在 / 同会话忙 / 实例数超限）走 error
	// ——如实上抛、循环收场、进程非零退出；运行期失败在 res.Status 里（契约 §1.2）。
	res, err := r.deps.RunService.Run(runCtx, core.RunRequest{
		Agent:     r.agent,
		SessionID: sessionID,
		Message:   msg.Text,
		Channel:   r.channel,
		UserID:    msg.UserID,
		Budget:    r.budget,
	})
	if err != nil {
		return channel.Reply{}, err
	}

	// ④ 读回历史取工具行与部分结果。**用循环的 ctx 而不是 runCtx**：这一轮可能刚被取消或
	// 已到截止，runCtx 已经死了，而历史照读不误——回填的消息正是运行自己写的。
	msgs, readErr := r.deps.SessionLister.Messages(ctx, sessionID)
	if readErr != nil {
		// 读不回来只影响工具行与部分结果的取数（状态行不受影响）：记 WARN、按空历史渲染，
		// 且**游标不动**——本轮消息若在下一轮被读到，工具行补在真实顺序里的"本轮之后"，
		// 既不丢、也不错位到更早的位置。
		r.deps.Logger.Warn("chat 读回会话消息失败，本轮只渲染状态行",
			"run_id", res.RunID, "session_id", sessionID, "error", readErr)
		msgs = nil
	}

	text, next := renderTurn(res, msgs, r.rendered)
	if readErr == nil {
		r.rendered = next
	}

	if res.Status == core.StatusFailed && r.failure == nil {
		r.failure = fmt.Errorf("运行 %s 失败：%s", res.RunID, res.Reason)
	}
	return channel.Reply{Text: text}, nil
}

// deriveSessionID 按**规范元组编码 + 稳定哈希**派生会话标识（§6.2 固定口径 1，逐字）：
// `sha256("basalt/session/v1" ‖ 0x1F ‖ <len(channel)>:channel ‖ 0x1F ‖ <len(user)>:user ‖
// 0x1F ‖ <len(profile)>:profile)` 取十六进制前 16 位，前缀渠道名（如 `cli-3f9a1c07b2d84e55`）。
// deriveSessionID derives the session id from the canonical tuple encoding.
//
// **长度前缀**是这一步的全部要点：`user=a-b, profile=c` 与 `user=a, profile=b-c` 直接拼接
// 出的字节完全一样，只有把每段的长度也编进元组，两种组合才会落到不同摘要上（§4.3 判据）。
// 结果只含 `[a-z0-9-]`：渠道名是本地常量、其余部分是十六进制——哈希把任意用户输入（含
// 路径分隔符与控制字符）折成定长十六进制，标识因此永远可以安全地当文件名与日志字段用。
// The length prefix is the point: the same bytes concatenate from different tuples
// otherwise. The hex digest keeps arbitrary user input out of the resulting id.
func deriveSessionID(channel, user, profile string) string {
	digest := sha256.New()
	digest.Write([]byte(sessionIDDomain))
	for _, part := range []string{channel, user, profile} {
		digest.Write([]byte{sessionIDSep})
		fmt.Fprintf(digest, "%d:%s", len(part), part)
	}
	return channel + "-" + hex.EncodeToString(digest.Sum(nil))[:sessionIDHashHexLen]
}

// renderTurn 把一轮运行渲染成文本（§6.2 固定口径 4）。返回文本与新的消息游标。
// renderTurn renders one run's outcome, returning the text and the new cursor.
//
// 三件事按序发生：工具调用提示行（本轮新增的助手消息逐条给出一行）、状态行、部分结果。
// 文本**不带**尾随换行——行尾由渠道层加（`channel.Reply.Text` 的约定）。
// Tool lines first, then the status line, then whatever partial text exists; the
// trailing newline belongs to the channel layer.
func renderTurn(res core.RunResult, msgs []core.Message, from int) (string, int) {
	if from < 0 || from > len(msgs) {
		// 游标只可能因为"上一次读失败"而落后或越界；越界时收到队尾——重复打一行旧工具行
		// 比把游标停在一个不存在的下标上安全。
		from = len(msgs)
	}

	fresh := msgs[from:]
	lines := make([]string, 0, 2+len(fresh))
	for _, name := range toolCallNames(fresh) {
		lines = append(lines, fmt.Sprintf(toolLineFormat, name))
	}

	switch res.Status {
	case core.StatusCompleted:
		// 完成的回复直接打正文（口径 4 第一句）——正文为空时什么都不打：那一轮本就
		// 没有可展示的文本，替它编一句"（空回复）"是把事实改成装饰。
		if res.FinalText != "" {
			lines = append(lines, res.FinalText)
		}
	case core.StatusLimitReached:
		lines = append(lines, fmt.Sprintf(limitLineFormat, res.Reason))
		lines = appendPartial(lines, res, fresh)
	case core.StatusCancelled:
		lines = append(lines, cancelledLine)
		lines = appendPartial(lines, res, fresh)
	case core.StatusFailed:
		lines = append(lines, fmt.Sprintf(failLineFormat, res.Reason))
	default:
		// 运行状态是五值集合（契约 §2）：出现第六个值说明契约变了而本层没跟上——如实打出
		// 状态名，比默默当成功、或用某个分支冒充事实都更可查。
		lines = append(lines, fmt.Sprintf(failLineFormat, "未知运行状态 "+string(res.Status)))
	}
	return strings.Join(lines, "\n"), len(msgs)
}

// toolCallNames 按顺序收集本轮新增消息里的工具调用名（**只有名字**——入参不进渲染面）。
// toolCallNames collects the tool-call names in order; arguments never reach the screen.
func toolCallNames(msgs []core.Message) []string {
	var names []string
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			names = append(names, call.Name)
		}
	}
	return names
}

// appendPartial 追加"已得部分结果"（可能没有——那就只留状态行）。
// appendPartial appends whatever partial text exists, if any.
func appendPartial(lines []string, res core.RunResult, fresh []core.Message) []string {
	if text := partialText(res, fresh); text != "" {
		lines = append(lines, text)
	}
	return lines
}

// partialText 取"已得部分结果"：先用返回文本，为空时回落到本轮最后一条**有正文的助手消息**
// ——运行结果在 `limit_reached` / `cancelled` 两种状态下收不下部分文本（`FinalText` 恒空，
// 冻结签名所致），而模型最后说出的那段话本身就在会话历史里。
// partialText falls back to the run's last non-empty assistant message, which is where
// the partial answer actually lives when the result struct cannot carry it.
func partialText(res core.RunResult, fresh []core.Message) string {
	if res.FinalText != "" {
		return res.FinalText
	}
	for i := len(fresh) - 1; i >= 0; i-- {
		if fresh[i].Role == core.RoleAssistant && fresh[i].Content != "" {
			return fresh[i].Content
		}
	}
	return ""
}

// chatBudget 读配置的 `budget` 段（契约 §6：数值预算的入口是 `basalt.yaml` 的 `budget:` 段）
// 并映射成运行预算。
// chatBudget maps the configured budget section onto the run budget.
//
// 缺省值**不在本层补**：契约 §6 的五项缺省由运行入口统一补齐，本层只搬运配置里显式写下的
// 取值——同一项两个缺省来源，将来改缺省就必然有一处漏改。配置文件缺失 ⇒ 全零预算 + WARN
// （与 status / profile 对"无配置"的同一种宽容）；配置存在但无效 ⇒ 拒绝启动（fail-closed：
// 预算读不出来时，静默跑一套缺省值等于替运维改了上限）。
// Defaults belong to the run entry point; a missing file falls back to the defaults with
// a warning, while an invalid one refuses to start.
func chatBudget(deps Deps) (core.Budget, error) {
	path := filepath.Join(deps.WorkDir, configFileName)
	cfg, err := LoadConfig(path, os.LookupEnv)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		deps.Logger.Warn("未发现配置，预算按缺省值执行", "path", path)
		return core.Budget{}, nil
	case err != nil:
		deps.Logger.Error("chat 未通过：配置无效", "path", path, "error", err)
		return core.Budget{}, err
	}

	budget := core.Budget{
		MaxIterations: cfg.Budget.MaxIterations,
		MaxToolCalls:  cfg.Budget.MaxToolCalls,
		ContextTokens: cfg.Budget.ContextTokens,
	}
	// MaxOutputBytes 在配置面是 int64（US-1 的键类型）、在运行面是 int：超出 int 表示范围的
	// 取值**不搬运**——留在零值由运行入口补缺省，比截断成一个无声改写的上限更接近配置者的意图。
	// An out-of-range value is dropped rather than truncated to something the operator
	// never wrote.
	if v := cfg.Budget.MaxOutputBytes; v > 0 && v <= math.MaxInt {
		budget.MaxOutputBytes = int(v)
	}
	return budget, nil
}
