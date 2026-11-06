package cli

// 本文件是受控自我进化的命令面（颗粒度文档 §3.1.10、T023 / T027）：
//
//	handoff —— 薄转交：把只读方法草案交给既有技能准入的提议入口
//	status / extractions / drafts —— 只读查询（T027 落地，不取实例锁）
//
// handoff 的四条纪律：
//  1. **身份先于一切副作用**：操作主体只来自 `Deps.CLIWriteActor`，`cli:unknown`
//     在调用端口与读取任何文件之前被拒绝（`admissionWriteActor` 与 US-21 同一判据）；
//  2. **文件读取走端口**：目录与来源声明经 `AdmissionPort` 的有界读取器读取——
//     cli 不 import workspace，边界检查只有一份实现；
//  3. **来源声明原样转交**：严格解码、`user_confirmed` 强制与 `kind` / `ref` 覆写
//     都在 app 的薄适配里做（单一份实现）；命令面不重新编码、不"顺手修正"；
//  4. **命令面不取实例锁、不开数据库**：锁与库句柄归 `LearningPort` 的生产适配器
//     （与 `agentsPort.withCLIAdmin` 同型）；独占锁不可重入，命令面再取一次会把
//     一次正常转交变成「锁被占用」。转交只调用提议入口，评估 / 批准 / 发布仍全部
//     在 `basalt admission` 命令里进行。
//
// The learning command surface. Handoff is a thin adapter: identity first, bounded
// file reads through the admission port, the strict source declaration passed through
// verbatim, and no lock or database handling in the command itself. The lock belongs
// to the learning port's production adapter, exactly like the agents port.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/core"
)

// newLearningCommand 组装 `learning` 命令组。
// newLearningCommand groups the learning subcommands.
func newLearningCommand(deps Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "learning",
		Short: "受控自我进化：学习状态、抽取账本、方法草案与受控转交",
		Long: "受控自我进化只做两件事：把已完成运行里有长期价值的陈述提炼成**待确认**\n" +
			"候选（永不由模型直接生效），以及把可复用方法写成只读草案。\n" +
			"查询命令只读、可在服务运行时执行；handoff 需要停止服务（实例锁归端口适配器\n" +
			"持有），且只调用既有技能准入的提议入口——评估 / 批准 / 发布仍走 admission。\n" +
			"The controlled-evolution surface: read-only queries while the service runs, and a\n" +
			"thin handoff into the existing admission propose entry point.",
		Args: cobra.NoArgs,
	}
	group.AddCommand(
		newLearningStatusCommand(deps),
		newLearningExtractionsCommand(deps),
		newLearningDraftsCommand(deps),
		newLearningHandoffCommand(deps),
	)
	return group
}

// requireLearningPort 取学习端口；缺失即报缺件——命令面**不得**自己拼装存储或
// 另起一条抽取 / 审计路径。
// requireLearningPort reports a wiring gap instead of assembling a second path.
func requireLearningPort(deps Deps, command string) (LearningPort, error) {
	if deps.Learning == nil {
		e := fmt.Errorf("学习端口未注入——%s 需要装配层注入的 LearningPort（internal/app 提供）", command)
		deps.Logger.Error(command + " 未通过：学习端口未注入")
		return nil, e
	}
	return deps.Learning, nil
}

// ─────────────────────────── 只读查询 ───────────────────────────
//
// 五个查询命令（status / extractions list|show / drafts list|show）**不取实例锁、
// 不校验身份**：服务运行中也能读（§3.1.10）。它们只经 `core.LearningAdmin`，
// 写方法与 handoff 都在另一条路径上——本组命令连 `Propose` 都摸不到。

// learningStates 是抽取状态闭集（与 memory_extractions.status 的 CHECK 同源）。
// learningStates mirrors the extraction status CHECK set.
var learningStates = []string{"pending", "success", "failed", "skipped", "unknown"}

// newLearningStatusCommand 组装 `learning status`。
// newLearningStatusCommand builds the read-only status command.
func newLearningStatusCommand(deps Deps) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "status",
		Short: "学习抽取状态（启用、名单、预算、待决计数）",
		Long: "只读展示受控自我进化的生效状态：是否启用、Agent 名单与未注册名单、\n" +
			"滚动窗口预算与耗尽标记、pending / unknown / 孤儿草案计数、规则与提示词版本。\n" +
			"Show the effective learning state: enable flag, allow-lists, budget, pending\n" +
			"counts and the fixed rule / prompt versions.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			port, err := requireLearningPort(deps, "learning status")
			if err != nil {
				return err
			}
			status, err := port.Status(cmd.Context())
			if err != nil {
				deps.Logger.Error("learning status 未通过", "error", err)
				return err
			}
			if asJSON {
				return writeAdmissionJSON(cmd, learningStatusJSONView(status))
			}
			out := cmd.OutOrStdout()
			if status.Enabled {
				if _, err := fmt.Fprintf(out, "学习抽取：已启用（Agent 名单：%s）\n",
					strings.Join(status.Agents, ", ")); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprintln(out,
					"学习抽取：未启用（出厂默认；未启用时零写入、零模型调用）"); err != nil {
					return err
				}
			}
			if len(status.UnknownAgents) > 0 {
				if _, err := fmt.Fprintf(out, "名单内未注册（不抽取）：%s\n",
					strings.Join(status.UnknownAgents, ", ")); err != nil {
					return err
				}
			}
			exhausted := "未耗尽"
			if status.Exhausted {
				exhausted = "已耗尽"
			}
			if _, err := fmt.Fprintf(out, "提取额度：%d/%d（滚动窗口 %s，%s）\n",
				status.AttemptsUsed, status.AttemptLimit, status.BudgetWindow, exhausted); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "待决：pending=%d unknown=%d 孤儿草案=%d\n",
				status.PendingExtractions, status.UnknownExtractions, status.OrphanDrafts); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "版本：rule=%s prompt=%s\n",
				status.RuleVersion, status.PromptVersion); err != nil {
				return err
			}
			if status.LastExtractionAt.IsZero() {
				_, err = fmt.Fprintln(out, "最近抽取：无")
			} else {
				_, err = fmt.Fprintf(out, "最近抽取：%s\n", admissionTime(status.LastExtractionAt))
			}
			return err
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// newLearningExtractionsCommand 组装 `learning extractions` 命令组（list / show）。
// newLearningExtractionsCommand groups the extraction list and detail commands.
func newLearningExtractionsCommand(deps Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "extractions",
		Short: "抽取账本（只读）：列表与详情",
		Args:  cobra.NoArgs,
	}
	group.AddCommand(newLearningExtractionsListCommand(deps), newLearningExtractionsShowCommand(deps))
	return group
}

// newLearningExtractionsListCommand 组装 `learning extractions list`。
// newLearningExtractionsListCommand builds the extraction list command.
func newLearningExtractionsListCommand(deps Deps) *cobra.Command {
	var agent, state string
	var limit int
	var asJSON bool
	command := &cobra.Command{
		Use:   "list",
		Short: "列出抽取账本（可按 Agent / 状态过滤）",
		Long: "只读列出抽取行；--state 取 pending / success / failed / skipped / unknown，\n" +
			"--limit 取 1–200（省略时缺省 50，由存储层落缺省）。\n" +
			"List extraction rows read-only; the limit defaults to 50 when omitted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state = strings.TrimSpace(state)
			if state != "" && !slices.Contains(learningStates, state) {
				return fmt.Errorf("--state 取值非法（%s）——只允许 %s",
					state, strings.Join(learningStates, " / "))
			}
			// 省略时默认 0 交给存储层取 50；显式给出的 0 属于非法参数——帮助与
			// HTTP 面（handlers_learning.go:303-315）都写 1–200，不接受 0。
			if (cmd.Flags().Changed("limit") && limit < 1) || limit > 200 {
				return fmt.Errorf("--limit 取值非法（%d）——只允许 1–200（省略时缺省 50）", limit)
			}
			port, err := requireLearningPort(deps, "learning extractions list")
			if err != nil {
				return err
			}
			views, err := port.ListExtractions(cmd.Context(), core.LearningExtractionFilter{
				Agent: strings.TrimSpace(agent),
				State: state,
				Limit: limit,
			})
			if err != nil {
				deps.Logger.Error("learning extractions list 未通过", "error", err)
				return err
			}
			if asJSON {
				return writeAdmissionJSON(cmd, learningExtractionListJSONView(views))
			}
			out := cmd.OutOrStdout()
			if len(views) == 0 {
				_, err := fmt.Fprintln(out, "（没有抽取记录）")
				return err
			}
			for _, view := range views {
				line := fmt.Sprintf("%s  %s  %s  attempts=%d",
					view.Extraction.ExtractionID, view.Extraction.Agent,
					view.Extraction.Status, view.Extraction.Attempts)
				if view.Extraction.FailureClass != "" {
					line += fmt.Sprintf("  failure=%s", view.Extraction.FailureClass)
				}
				if _, err := fmt.Fprintf(out, "%s  %s\n", admissionTime(view.Extraction.CreatedAt), line); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&agent, "agent", "", "只看该 Agent 的抽取")
	command.Flags().StringVar(&state, "state", "", "只看该状态的抽取（pending / success / failed / skipped / unknown）")
	command.Flags().IntVar(&limit, "limit", 0, "最多返回条数（1–200，缺省 50）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// newLearningExtractionsShowCommand 组装 `learning extractions show`。
// newLearningExtractionsShowCommand builds the extraction detail command.
func newLearningExtractionsShowCommand(deps Deps) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "show <extraction-id>",
		Short: "查看一次抽取的详情（含候选与草案摘要）",
		Long: "只读展示抽取行、冻结窗口、失败分类与证据、关联候选与草案摘要。\n" +
			"Show one extraction with its frozen window, failure detail, candidates and drafts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			extractionID := strings.TrimSpace(args[0])
			if extractionID == "" {
				return errors.New("extractions show 需要 <extraction-id> 参数")
			}
			port, err := requireLearningPort(deps, "learning extractions show")
			if err != nil {
				return err
			}
			view, err := port.LoadExtraction(cmd.Context(), extractionID)
			if err != nil {
				deps.Logger.Error("learning extractions show 未通过", "extraction", extractionID, "error", err)
				if errors.Is(err, core.ErrLearningNotFound) {
					return fmt.Errorf("抽取不存在：%s", extractionID)
				}
				return err
			}
			if asJSON {
				return writeAdmissionJSON(cmd, learningExtractionDetailJSONView(view))
			}
			out := cmd.OutOrStdout()
			extraction := view.Extraction
			if _, err := fmt.Fprintf(out,
				"抽取：%s\nAgent：%s\n状态：%s（尝试 %d 次）\n窗口：消息 %d–%d（%d 条，丢弃 %d）\n",
				extraction.ExtractionID, extraction.Agent, extraction.Status, extraction.Attempts,
				extraction.WindowFromMessageID, extraction.WindowToMessageID,
				extraction.WindowMessages, extraction.DroppedWindowMessages); err != nil {
				return err
			}
			if extraction.FailureClass != "" {
				if _, err := fmt.Fprintf(out, "失败分类：%s（%s）\n",
					extraction.FailureClass, extraction.ErrorMessage); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(out, "规则 / 提示词：%s / %s\n",
				extraction.RuleVersion, extraction.PromptVersion); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "候选 %d 条，草案 %d 份\n",
				len(view.Candidates), len(view.Drafts)); err != nil {
				return err
			}
			for _, candidate := range view.Candidates {
				if _, err := fmt.Fprintf(out, "  候选 #%d  %s  %s\n",
					candidate.EntryID, candidate.Kind, candidate.State); err != nil {
					return err
				}
			}
			for _, draft := range view.Drafts {
				if _, err := fmt.Fprintf(out, "  草案 %s  技能 %s\n", draft.DraftID, draft.Name); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// newLearningDraftsCommand 组装 `learning drafts` 命令组（list / show）。
// newLearningDraftsCommand groups the draft list and detail commands.
func newLearningDraftsCommand(deps Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "drafts",
		Short: "方法草案（只读）：列表与详情",
		Args:  cobra.NoArgs,
	}
	group.AddCommand(newLearningDraftsListCommand(deps), newLearningDraftsShowCommand(deps))
	return group
}

// newLearningDraftsListCommand 组装 `learning drafts list`。
// newLearningDraftsListCommand builds the draft list command.
func newLearningDraftsListCommand(deps Deps) *cobra.Command {
	var agent string
	var limit int
	var asJSON bool
	command := &cobra.Command{
		Use:   "list",
		Short: "列出方法草案（可按 Agent 过滤）",
		Long:  "只读列出草案元数据（不含正文）；正文经 `learning drafts show` 查看。",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// 省略时默认 0 交给存储层取 50；显式给出的 0 属于非法参数——帮助与
			// HTTP 面（handlers_learning.go:303-315）都写 1–200，不接受 0。
			if (cmd.Flags().Changed("limit") && limit < 1) || limit > 200 {
				return fmt.Errorf("--limit 取值非法（%d）——只允许 1–200（省略时缺省 50）", limit)
			}
			port, err := requireLearningPort(deps, "learning drafts list")
			if err != nil {
				return err
			}
			views, err := port.ListDrafts(cmd.Context(), core.LearningDraftFilter{
				Agent: strings.TrimSpace(agent),
				Limit: limit,
			})
			if err != nil {
				deps.Logger.Error("learning drafts list 未通过", "error", err)
				return err
			}
			if asJSON {
				metas := make([]learningDraftMetaJSON, 0, len(views))
				for _, view := range views {
					metas = append(metas, learningDraftMetaJSONView(view))
				}
				return writeAdmissionJSON(cmd, metas)
			}
			out := cmd.OutOrStdout()
			if len(views) == 0 {
				_, err := fmt.Fprintln(out, "（没有方法草案）")
				return err
			}
			for _, view := range views {
				if _, err := fmt.Fprintf(out, "%s  %s  技能 %s  %s\n",
					admissionTime(view.CreatedAt), view.DraftID, view.Name, view.Agent); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&agent, "agent", "", "只看该 Agent 的草案")
	command.Flags().IntVar(&limit, "limit", 0, "最多返回条数（1–200，缺省 50）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// newLearningDraftsShowCommand 组装 `learning drafts show`：草案正文的**唯一**出口。
// newLearningDraftsShowCommand is the one place the draft body is shown.
func newLearningDraftsShowCommand(deps Deps) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "show <draft-id>",
		Short: "查看方法草案的元数据、正文与证据",
		Long: "只读展示草案元数据、`DRAFT.md` 正文与 `evidence.json` 证据。\n" +
			"草案是只读文件，不会被任何 Agent 读取或执行。\n" +
			"Show the draft metadata, body and evidence; drafts are never executed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			draftID := strings.TrimSpace(args[0])
			if draftID == "" {
				return errors.New("drafts show 需要 <draft-id> 参数")
			}
			port, err := requireLearningPort(deps, "learning drafts show")
			if err != nil {
				return err
			}
			draft, err := port.LoadDraft(cmd.Context(), draftID)
			if err != nil {
				deps.Logger.Error("learning drafts show 未通过", "draft", draftID, "error", err)
				if errors.Is(err, core.ErrLearningDraftNotFound) {
					return fmt.Errorf("草案不存在：%s", draftID)
				}
				return err
			}
			if asJSON {
				return writeAdmissionJSON(cmd, learningDraftDetailJSONView(draft))
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out,
				"草案：%s\n技能：%s\nAgent：%s\n来源抽取：%s\n内容哈希：%s\n目录：%s\n证据 %d 条\n\n%s\n",
				draft.DraftID, draft.Name, draft.Agent, draft.ExtractionID,
				draft.ContentSHA256, draft.RelDir, len(draft.Evidence), draft.Content); err != nil {
				return err
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// ─────────────────────── 只读面的机器投影 ───────────────────────

// learningStatusJSON 与 §3.1.10 的 `LearningStatusView` 字段逐项同源。
// learningStatusJSON mirrors the LearningStatusView field names of §3.1.10.
type learningStatusJSON struct {
	Enabled             bool     `json:"enabled"`
	Agents              []string `json:"agents"`
	UnknownAgents       []string `json:"unknown_agents"`
	RuleVersion         string   `json:"rule_version"`
	PromptVersion       string   `json:"prompt_version"`
	AttemptsUsed        int      `json:"attempts_used"`
	AttemptLimit        int      `json:"attempt_limit"`
	Exhausted           bool     `json:"exhausted"`
	BudgetWindowSeconds int64    `json:"budget_window_seconds"`
	PendingExtractions  int      `json:"pending_extractions"`
	UnknownExtractions  int      `json:"unknown_extractions"`
	OrphanDrafts        int      `json:"orphan_drafts"`
	LastExtractionAt    string   `json:"last_extraction_at"`
}

// learningStatusJSONView 投影状态读模型（空名单保持 `[]`，不写成 null）。
// learningStatusJSONView projects the status read model; empty lists stay [].
func learningStatusJSONView(status core.LearningStatus) learningStatusJSON {
	agents := status.Agents
	if agents == nil {
		agents = []string{}
	}
	unknownAgents := status.UnknownAgents
	if unknownAgents == nil {
		unknownAgents = []string{}
	}
	return learningStatusJSON{
		Enabled:             status.Enabled,
		Agents:              agents,
		UnknownAgents:       unknownAgents,
		RuleVersion:         status.RuleVersion,
		PromptVersion:       status.PromptVersion,
		AttemptsUsed:        status.AttemptsUsed,
		AttemptLimit:        status.AttemptLimit,
		Exhausted:           status.Exhausted,
		BudgetWindowSeconds: int64(status.BudgetWindow / time.Second),
		PendingExtractions:  status.PendingExtractions,
		UnknownExtractions:  status.UnknownExtractions,
		OrphanDrafts:        status.OrphanDrafts,
		LastExtractionAt:    admissionTime(status.LastExtractionAt),
	}
}

// learningEvidenceJSON 是证据的机器投影（逐字引文与消息主键）。
// learningEvidenceJSON is the evidence projection with the verbatim quote.
type learningEvidenceJSON struct {
	MessageID int64  `json:"message_id"`
	RunID     string `json:"run_id"`
	Role      string `json:"role"`
	Quote     string `json:"quote"`
	MessageAt string `json:"message_at"`
}

// learningEvidenceJSONViews 投影证据列表（空切片保持为 `[]`，不写成 null）。
// learningEvidenceJSONViews keeps an empty list as [] instead of null.
func learningEvidenceJSONViews(evidence []core.LearningEvidence) []learningEvidenceJSON {
	views := make([]learningEvidenceJSON, 0, len(evidence))
	for _, item := range evidence {
		views = append(views, learningEvidenceJSON{
			MessageID: item.MessageID,
			RunID:     item.RunID,
			Role:      item.Role,
			Quote:     item.Quote,
			MessageAt: admissionTime(item.MessageAt),
		})
	}
	return views
}

// learningExtractionJSON 是抽取账本行的机器投影。
// learningExtractionJSON is the machine projection of one extraction row.
type learningExtractionJSON struct {
	ExtractionID          string `json:"extraction_id"`
	Agent                 string `json:"agent"`
	SessionID             string `json:"session_id"`
	RunID                 string `json:"run_id"`
	LastAttemptRunID      string `json:"last_attempt_run_id"`
	OriginCallerID        string `json:"origin_caller_id"`
	OriginKeyID           string `json:"origin_key_id"`
	Status                string `json:"status"`
	WindowFromMessageID   int64  `json:"window_from_message_id"`
	WindowToMessageID     int64  `json:"window_to_message_id"`
	WindowMessages        int    `json:"window_messages"`
	DroppedWindowMessages int    `json:"dropped_window_messages"`
	PriorMessages         int    `json:"prior_messages"`
	Attempts              int    `json:"attempts"`
	ItemsTotal            int    `json:"items_total"`
	ItemsAccepted         int    `json:"items_accepted"`
	ItemsDropped          int    `json:"items_dropped"`
	PromptVersion         string `json:"prompt_version"`
	RuleVersion           string `json:"rule_version"`
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	FailureClass          string `json:"failure_class"`
	ErrorMessage          string `json:"error_message"`
	CreatedAt             string `json:"created_at"`
	LastAttemptAt         string `json:"last_attempt_at"`
	FinishedAt            string `json:"finished_at"`
	VerifiedAt            string `json:"verified_at"`
}

// learningExtractionJSONView 投影抽取行。
// learningExtractionJSONView projects one extraction row.
func learningExtractionJSONView(extraction core.LearningExtraction) learningExtractionJSON {
	return learningExtractionJSON{
		ExtractionID:          extraction.ExtractionID,
		Agent:                 extraction.Agent,
		SessionID:             extraction.SessionID,
		RunID:                 extraction.RunID,
		LastAttemptRunID:      extraction.LastAttemptRunID,
		OriginCallerID:        extraction.OriginCallerID,
		OriginKeyID:           extraction.OriginKeyID,
		Status:                string(extraction.Status),
		WindowFromMessageID:   extraction.WindowFromMessageID,
		WindowToMessageID:     extraction.WindowToMessageID,
		WindowMessages:        extraction.WindowMessages,
		DroppedWindowMessages: extraction.DroppedWindowMessages,
		PriorMessages:         extraction.PriorMessages,
		Attempts:              extraction.Attempts,
		ItemsTotal:            extraction.ItemsTotal,
		ItemsAccepted:         extraction.ItemsAccepted,
		ItemsDropped:          extraction.ItemsDropped,
		PromptVersion:         extraction.PromptVersion,
		RuleVersion:           extraction.RuleVersion,
		Provider:              extraction.Provider,
		Model:                 extraction.Model,
		FailureClass:          string(extraction.FailureClass),
		ErrorMessage:          extraction.ErrorMessage,
		CreatedAt:             admissionTime(extraction.CreatedAt),
		LastAttemptAt:         admissionTime(extraction.LastAttemptAt),
		FinishedAt:            admissionTime(extraction.FinishedAt),
		VerifiedAt:            admissionTime(extraction.VerifiedAt),
	}
}

// learningCandidateJSON 是候选的机器投影（证据与来源关系可复核）。
// learningCandidateJSON is the candidate projection with reviewable evidence.
type learningCandidateJSON struct {
	EntryID               int64                  `json:"entry_id"`
	State                 string                 `json:"state"`
	Content               string                 `json:"content"`
	Kind                  string                 `json:"kind"`
	Certainty             string                 `json:"certainty"`
	Confidence            float64                `json:"confidence"`
	Evidence              []learningEvidenceJSON `json:"evidence"`
	SourceEntryIDs        []int64                `json:"source_entry_ids"`
	SuggestedSupersedesID int64                  `json:"suggested_supersedes_id"`
	EvidenceAt            string                 `json:"evidence_at"`
	ValidTo               string                 `json:"valid_to"`
	TimeConfidence        string                 `json:"time_confidence"`
	CreatedAt             string                 `json:"created_at"`
}

// learningCandidateJSONView 投影候选。
// learningCandidateJSONView projects one candidate.
func learningCandidateJSONView(candidate core.LearningCandidateView) learningCandidateJSON {
	return learningCandidateJSON{
		EntryID:               candidate.EntryID,
		State:                 candidate.State,
		Content:               candidate.Content,
		Kind:                  candidate.Kind,
		Certainty:             candidate.Certainty,
		Confidence:            candidate.Confidence,
		Evidence:              learningEvidenceJSONViews(candidate.Evidence),
		SourceEntryIDs:        candidate.SourceEntryIDs,
		SuggestedSupersedesID: candidate.SuggestedSupersedesID,
		EvidenceAt:            admissionTime(candidate.EvidenceAt),
		ValidTo:               admissionTime(candidate.ValidTo),
		TimeConfidence:        candidate.TimeConfidence,
		CreatedAt:             admissionTime(candidate.CreatedAt),
	}
}

// learningExtractionListJSON 是列表回执：与详情分开，列表不展开候选与草案。
// learningExtractionListJSON keeps the list receipt flat.
type learningExtractionListJSON struct {
	Extractions []learningExtractionJSON `json:"extractions"`
}

// learningExtractionListJSONView 投影抽取列表。
// learningExtractionListJSONView projects the extraction list.
func learningExtractionListJSONView(views []core.LearningExtractionView) learningExtractionListJSON {
	items := make([]learningExtractionJSON, 0, len(views))
	for _, view := range views {
		items = append(items, learningExtractionJSONView(view.Extraction))
	}
	return learningExtractionListJSON{Extractions: items}
}

// learningExtractionDetailJSON 是详情回执：账本行 + 候选 + 草案摘要。
// learningExtractionDetailJSON carries the row, candidates and draft summaries.
type learningExtractionDetailJSON struct {
	Extraction learningExtractionJSON  `json:"extraction"`
	Candidates []learningCandidateJSON `json:"candidates"`
	Drafts     []learningDraftMetaJSON `json:"drafts"`
}

// learningExtractionDetailJSONView 投影抽取详情。
// learningExtractionDetailJSONView projects one extraction detail.
func learningExtractionDetailJSONView(view core.LearningExtractionView) learningExtractionDetailJSON {
	candidates := make([]learningCandidateJSON, 0, len(view.Candidates))
	for _, candidate := range view.Candidates {
		candidates = append(candidates, learningCandidateJSONView(candidate))
	}
	drafts := make([]learningDraftMetaJSON, 0, len(view.Drafts))
	for _, draft := range view.Drafts {
		drafts = append(drafts, learningDraftMetaJSONView(draft))
	}
	return learningExtractionDetailJSON{
		Extraction: learningExtractionJSONView(view.Extraction),
		Candidates: candidates,
		Drafts:     drafts,
	}
}

// learningDraftDetailJSON 是草案详情的机器投影：元数据 + 正文 + 证据。
// learningDraftDetailJSON adds the body and evidence to the draft metadata.
type learningDraftDetailJSON struct {
	learningDraftMetaJSON
	Content  string                 `json:"content"`
	Evidence []learningEvidenceJSON `json:"evidence"`
}

// learningDraftDetailJSONView 投影草案详情。
// learningDraftDetailJSONView projects one draft detail.
func learningDraftDetailJSONView(draft core.LearningDraftView) learningDraftDetailJSON {
	return learningDraftDetailJSON{
		learningDraftMetaJSON: learningDraftMetaJSONView(draft),
		Content:               draft.Content,
		Evidence:              learningEvidenceJSONViews(draft.Evidence),
	}
}

// ─────────────────────────── handoff ───────────────────────────

// newLearningHandoffCommand 组装 `learning handoff`：把草案元数据交给技能准入的
// 提议入口。命令面只做参数、身份与有界文件读取；锁、库与字段强制都在适配器里。
// newLearningHandoffCommand hands a draft to the admission propose entry point; the
// lock, the database and the forced source fields all live behind the port.
func newLearningHandoffCommand(deps Deps) *cobra.Command {
	var name, fromDir, sourceFile string
	var asJSON bool
	command := &cobra.Command{
		Use:   "handoff <draft-id> --name <skill> --from-dir <dir> --source-file <json>",
		Short: "把只读方法草案转交给既有技能准入（只调用提议入口）",
		Long: "把 <draft-id> 的只读方法草案作为技能候选交给既有准入：草案必须存在，主体必须是\n" +
			"本机 CLI 用户，来源声明必须是严格 JSON 且 confirmation 为 user_confirmed。\n" +
			"程序强制来源字段（kind=model_proposal、ref=learning/draft/<draft-id>），\n" +
			"随后只调用提议入口；评估 / 批准 / 发布仍全部走 `basalt admission`。\n" +
			"转交需要停止服务（实例锁归端口适配器持有）；输出只含草案元数据与提议回执。\n" +
			"Hand off a read-only draft to the admission propose entry point; the source\n" +
			"declaration must be strict JSON with confirmation=user_confirmed, and nothing\n" +
			"is evaluated, approved or published here.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			const commandName = "learning handoff"
			draftID := strings.TrimSpace(args[0])
			if draftID == "" {
				return errors.New("handoff 需要 <draft-id> 参数")
			}
			if strings.TrimSpace(name) == "" {
				return errors.New("handoff 需要 --name 指定技能名")
			}
			if strings.TrimSpace(fromDir) == "" {
				return errors.New("handoff 需要 --from-dir 指定技能目录")
			}
			if strings.TrimSpace(sourceFile) == "" {
				return errors.New("handoff 需要 --source-file 指定来源声明 JSON")
			}
			port, err := requireLearningPort(deps, commandName)
			if err != nil {
				return err
			}
			// 身份先于文件读取与端口调用：不可信主体不碰磁盘、不触端口（端口即开库
			// 入口），也不取实例锁——`admissionWriteActor` 与 US-21 共用同一判据。
			actor, err := admissionWriteActor(deps, commandName)
			if err != nil {
				return err
			}
			// 目录与来源声明经准入端口的有界读取器读取：cli 不 import workspace，
			// 路径穿越 / 软链接 / 体积上限只有一份实现。
			reader, err := requireAdmissionPort(deps, commandName)
			if err != nil {
				return err
			}
			files, err := reader.ImportFiles(fromDir)
			if err != nil {
				return err
			}
			sourceJSON, err := reader.ReadSourceFile(sourceFile)
			if err != nil {
				return err
			}
			result, err := port.Handoff(cmd.Context(), core.LearningHandoffRequest{
				Actor:      actor,
				DraftID:    draftID,
				SkillName:  strings.TrimSpace(name),
				Files:      files,
				SourceJSON: sourceJSON,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return writeAdmissionJSON(cmd, struct {
					Draft     learningDraftMetaJSON  `json:"draft"`
					Candidate admissionCandidateJSON `json:"candidate"`
				}{
					Draft:     learningDraftMetaJSONView(result.Draft),
					Candidate: admissionCandidateJSONView(result.Candidate),
				})
			}
			// 人类输出只含草案元数据与提议回执：草案正文不在这里回显（只读入口是
			// `learning drafts show`），来源声明正文更不回显。
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out,
				"草案已转交：%s（技能 %s）\n草案来源：抽取 %s（Agent %s）\n内容哈希：%s\n候选已冻结：%s（状态 %s）\n下一步：basalt admission evaluate %s\n",
				result.Draft.DraftID, result.Draft.Name, result.Draft.ExtractionID,
				result.Draft.Agent, result.Draft.ContentSHA256,
				result.Candidate.CandidateID, result.Candidate.State,
				result.Candidate.CandidateID); err != nil {
				return err
			}
			return nil
		},
	}
	command.Flags().StringVar(&name, "name", "", "技能名（与 AGENT.md 的 skills 声明同名）")
	command.Flags().StringVar(&fromDir, "from-dir", "", "技能源目录（不跟随软链接）")
	command.Flags().StringVar(&sourceFile, "source-file", "", "来源声明严格 JSON 文件（≤64 KiB，confirmation 必须是 user_confirmed）")
	command.Flags().BoolVar(&asJSON, "json", false, "输出机器可读结果")
	return command
}

// learningDraftMetaJSON 是草案元数据的机器可读投影：**不含正文**（正文只有
// `learning drafts show` 一个出口），内容身份由 `content_sha256` 承担。
// The metadata projection carries no body; the content hash is the identity.
type learningDraftMetaJSON struct {
	DraftID       string `json:"draft_id"`
	ExtractionID  string `json:"extraction_id"`
	Agent         string `json:"agent"`
	Name          string `json:"name"`
	ContentSHA256 string `json:"content_sha256"`
	RelDir        string `json:"rel_dir"`
	CreatedAt     string `json:"created_at"`
}

// learningDraftMetaJSONView 投影草案元数据。
// learningDraftMetaJSONView projects the draft metadata without its body.
func learningDraftMetaJSONView(draft core.LearningDraftView) learningDraftMetaJSON {
	return learningDraftMetaJSON{
		DraftID:       draft.DraftID,
		ExtractionID:  draft.ExtractionID,
		Agent:         draft.Agent,
		Name:          draft.Name,
		ContentSHA256: draft.ContentSHA256,
		RelDir:        draft.RelDir,
		CreatedAt:     admissionTime(draft.CreatedAt),
	}
}
