//go:build integration

package app

// 本文件是 V-7「上下文装配记录接线」的**落库级**判据（2026-10-30 用户裁决）：
// 真实 `AgentService.Run` → 生产形状的循环（含 `WithAssemblyRecorder`）→ 真实
// `postgres.NewAssemblyStore` → 直接读 `context_assembly_records` 逐字段对照。
//
// 与 `internal/core` 的循环侧用例分工：那边判"每轮一次、先于模型调用、压缩结论已并入"
// （替身录制器，不需要库）；这里判"生产装配形状下记录真的落到库里，且与检索装配共享
// 同一个 run_id 可对齐"。两处都绿才说明链路闭合。
//
// 逆改点：
//   - 循环忘记 `.WithAssemblyRecorder(...)`（构造链回退）→ 行数断言红；
//   - 循环少填 `step_id` / `agent` / `model_version` → 对应列断言红；
//   - `llm_call_id` 落成 0 而不是 NULL → `sql.NullInt64.Valid` 断言红；
//   - `final_segments` 丢了知识段 → 知识用例的段断言红。
//
// This file is the persistence-level case for V-7: the production-shaped run graph writes
// a chat assembly record through the real store, and the knowledge case proves the chat
// row and the retrieval row share one run_id.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// asmBudgetView 是 `budget` 列的解码口径（§3.4.1 四个字段）。
type asmBudgetView struct {
	ContextTokens    int    `json:"context_tokens"`
	ProtocolReserve  int    `json:"protocol_reserve"`
	OutputReserve    int    `json:"output_reserve"`
	EstimatorVersion string `json:"estimator_version"`
}

// asmCompressionView 是 `compression` 列的解码口径（只取本文件断言到的字段）。
type asmCompressionView struct {
	Enabled      bool                     `json:"enabled"`
	Decision     core.CompressionDecision `json:"decision"`
	CompactionID string                   `json:"compaction_id"`
	ReasonCode   string                   `json:"reason_code"`
}

// asmTrimView 是 `history_trim` 列的解码口径（只取本文件断言到的字段）。
type asmTrimView struct {
	DroppedGroups int    `json:"dropped_groups"`
	TokensBefore  int    `json:"tokens_before"`
	ReasonCode    string `json:"reason_code"`
}

// asmRecordView 是一行装配记录的读面（jsonb 列已解码）。
// asmRecordView is one row as read back from the database.
type asmRecordView struct {
	Purpose     string
	StepID      string
	Agent       string
	PromptVer   string
	ModelVer    string
	LLMCallID   sql.NullInt64
	Budget      asmBudgetView
	Compression asmCompressionView
	HistoryTrim asmTrimView
	Segments    []core.FinalSegment
}

// newAssemblyStoreFactory 是夹具的写入器构造回调：真库 + 丢弃日志器。
// 日志只承载"记录被丢弃"的 WARN，用例不判日志文本——那归写入器自己的用例。
// newAssemblyStoreFactory builds the real store over the fixture database.
func newAssemblyStoreFactory(t *testing.T, db *postgres.DB) core.AssemblyRecorder {
	t.Helper()
	return postgres.NewAssemblyStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// chatAssemblyRows 读回该 Run 的 `purpose='chat'` 记录（按写入顺序，逐字段解码）。
// chatAssemblyRows reads back the chat-purpose rows for one run.
func chatAssemblyRows(t *testing.T, db *postgres.DB, runID string) []asmRecordView {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
		SELECT purpose, step_id, agent, prompt_version, model_version, llm_call_id,
		       budget::text, compression::text, history_trim::text, final_segments::text
		FROM context_assembly_records
		WHERE run_id = $1 AND purpose = 'chat'
		ORDER BY created_at, record_id`, runID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := make([]asmRecordView, 0, 2)
	for rows.Next() {
		var item asmRecordView
		var budgetJSON, compressionJSON, trimJSON, segmentsJSON string
		require.NoError(t, rows.Scan(&item.Purpose, &item.StepID, &item.Agent, &item.PromptVer,
			&item.ModelVer, &item.LLMCallID, &budgetJSON, &compressionJSON, &trimJSON, &segmentsJSON))
		require.NoError(t, json.Unmarshal([]byte(budgetJSON), &item.Budget), "budget 必须是 JSON 对象")
		require.NoError(t, json.Unmarshal([]byte(compressionJSON), &item.Compression), "compression 必须是 JSON 对象")
		require.NoError(t, json.Unmarshal([]byte(trimJSON), &item.HistoryTrim), "history_trim 必须是 JSON 对象")
		require.NoError(t, json.Unmarshal([]byte(segmentsJSON), &item.Segments), "final_segments 必须是 JSON 数组")
		out = append(out, item)
	}
	require.NoError(t, rows.Err())
	return out
}

// segmentIDs 把段落记录压成标识切片（顺序即 `Order`）。
func segmentIDs(segments []core.FinalSegment) []string {
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		out = append(out, segment.ID)
	}
	return out
}

// TestAssemblyRecordE2E_ChatRoundPersists 判"一次对话 Run 在库里留下恰一条 chat 记录"，
// 逐字段对照循环与轮次给出的关联值。
//
// 注意：`compression.reason_code` 目前为空串——设计口径写"未发生压缩时 `decision=none`
// 并给原因"，而实现没有为"未装配 / 未触发"定义原因码字面量。本用例只判 `decision=none`
// 这一事实，不把空原因码写成"正确"；缺口登记在 run-state 未验证清单（V-7 附录）。
func TestAssemblyRecordE2E_ChatRoundPersists(t *testing.T) {
	h := newRunHarnessWith(t, runHarnessOptions{Assembly: newAssemblyStoreFactory})
	h.provider.respondWithText("收到")

	res, err := h.invoker.Run(context.Background(), h.requestFor(runHarnessCaller))
	require.NoError(t, err, "运行期失败必须体现在 Status 上")
	require.Equal(t, core.StatusCompleted, res.Status, "前提：Run 正常完成")

	rows := chatAssemblyRows(t, h.db, res.RunID)
	require.Len(t, rows, 1, "一轮 = 恰一条 chat 装配记录（本用例没有检索）")
	row := rows[0]
	require.Equal(t, "chat", row.Purpose)
	require.Equal(t, "1", row.StepID, "step_id 取轮次（首轮 = 1）")
	require.Equal(t, runHarnessAgentName, row.Agent, "Agent 名取自冻结快照")
	require.Equal(t, runHarnessModel, row.ModelVer, "模型取快照里的 Provider 绑定")
	require.Empty(t, row.PromptVer, "chat 装配没有模板版本：留空串，不编假值")
	require.False(t, row.LLMCallID.Valid, "装配发生在模型调用之前：llm_call_id 落 NULL")

	require.Equal(t, harnessBudget().ContextTokens, row.Budget.ContextTokens, "预算取本 Run 的冻结值")
	require.Equal(t, core.ReserveTokens, row.Budget.ProtocolReserve)
	require.Zero(t, row.Budget.OutputReserve, "输出预留未接线时为 0（不是默认预留，C-31）")
	require.Equal(t, core.EstimateVersion, row.Budget.EstimatorVersion)

	require.Equal(t, []string{"system", "recent"}, segmentIDs(row.Segments),
		"无记忆 / 无知识 / 无摘要时只有系统段与近期历史段——记录只写真发生的段落")
	for i, segment := range row.Segments {
		require.Equal(t, i+1, segment.Order, "段落顺序从 1 起连续")
	}

	require.Equal(t, core.DecisionNone, row.Compression.Decision, "压缩未接线：decision=none 是真实结论")
	require.False(t, row.Compression.Enabled)
	require.Greater(t, row.HistoryTrim.TokensBefore, 0, "裁剪前的真实估算必须落库，不得写占位零值")
	require.Zero(t, row.HistoryTrim.DroppedGroups, "本用例历史很短：没有整组丢弃")
	require.Empty(t, row.HistoryTrim.ReasonCode, "未裁剪时原因为空（契约如此）")
}

// TestAssemblyRecordE2E_ChatLinkedToKnowledgeRetrieval 判"对话装配与检索装配同 Run 可对齐"：
// 同一个 `run_id` 下有一条 `retrieval` 行与一条 `chat` 行；chat 行如实记录知识段进了
// 本轮装配，而 `hits` 只由检索行承载（用途语义不混写）。
//
// 逆改点：chat 记录不写 `knowledge` 段（`finalSegments` 漏接注入面）→ 段断言红；
// 检索把 `hits` 也写进 chat 行 / 或 chat 行完全缺失 → 行数与用途断言红。
func TestAssemblyRecordE2E_ChatLinkedToKnowledgeRetrieval(t *testing.T) {
	e := newKnRunE2E(t, knRunE2EOptions{WithReadScope: true, WithAssembly: true})
	e.ingest(t)
	e.provider.respondWithText(knRunE2EAnswerCited)

	res := e.runOnce(t)
	require.Equal(t, core.StatusCompleted, res.Status, "前提：Run 正常完成")

	rows := chatAssemblyRows(t, e.db, res.RunID)
	require.Len(t, rows, 1, "一轮对话 = 一条 chat 记录；检索记录另算")
	row := rows[0]
	require.Equal(t, runHarnessAgentName, row.Agent)
	require.Equal(t, "1", row.StepID)
	require.False(t, row.LLMCallID.Valid)
	require.Equal(t, []string{"system", "knowledge", "recent"}, segmentIDs(row.Segments),
		"知识注入面确实进了本轮装配：knowledge 段必须出现")
	require.Empty(t, row.Compression.CompactionID, "本用例未压缩")
	require.Equal(t, core.DecisionNone, row.Compression.Decision)

	// 同一 Run 的检索行仍在（V-6 的既有判据），且两行按 run_id 直接对齐。
	all := e.assemblies(t, res.RunID)
	require.Len(t, all, 2, "同一 Run：检索装配一条 + 对话装配一条")
	byPurpose := map[string]knRunE2EAssembly{}
	for _, item := range all {
		byPurpose[item.Purpose] = item
	}
	retrieval, ok := byPurpose["retrieval"]
	require.True(t, ok, "检索装配行必须仍在（V-6 判据不因 V-7 接线改变）")
	chat, ok := byPurpose["chat"]
	require.True(t, ok, "对话装配行必须落库")
	require.Len(t, retrieval.Hits, 1, "hits 只由检索行承载")
	require.Empty(t, chat.Hits, "对话行不重复写检索命中")
	require.Empty(t, retrieval.StepID, "运行级检索发生在第 1 轮之前，没有轮次可填")
	require.Equal(t, "1", chat.StepID, "对话行的轮次从 1 起")
}
