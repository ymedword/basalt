//go:build integration

package postgres

// 本文件是 `TestAssembly_HistoryTrimRecordsDroppedGroups`（US-16 §4 / C-31 / T129）的
// 真库判据：真 `core.PromptBuilder` 触发**整组丢弃** → `AssemblyRecorder` 落库 → 直接读
// `context_assembly_records` 逐字段对照。
//
// 为什么必须经真 PromptBuilder：`history_trim` 的失败形态是"写成空占位"——手搓一个
// 记录对象只能证明编码正确，证明不了"裁剪真的发生之后，记录里有真实的 message_id"。
// 预算也不硬编码估算器读数（估算器只承诺版本固定，不承诺保守上界）：先用大预算读出
// "丢掉两组之后"的估算，再把上限压到该读数，于是"再丢一组就超限、不丢就超限"同时成立，
// 丢弃组数必然是 2。
//
// 逆改点：把 `history_trim` 编码成 `{}` 或把 `DroppedGroups` 写死 0 → 组数与两条
// `message_id` 断言红；`LLMCallID=0` 落成 0 而不是 NULL → 扫描到 0 而非 NULL，断言红；
// 去掉 purpose 词表预挡 → 第二段的非法 purpose 用例改由数据库 CHECK 拒绝，行数断言仍绿
// 但 WARN 路径不再被覆盖（见用例内注释）。
//
// The real prompt builder drives the trim so the record is evidence of what happened, not
// of what the test typed in; the budget is calibrated from the builder's own readings.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// assemblyResolver 是段 4 的空解析器：本用例没有工具池，规格查询恒为空。
// assemblyResolver serves an empty tool pool; every lookup misses.
type assemblyResolver struct{}

// Resolve 恒未命中。
// Resolve always misses.
func (assemblyResolver) Resolve(string) (core.Tool, bool) { return nil, false }

// Specs 恒为空。
// Specs always yields nothing.
func (assemblyResolver) Specs([]string) []core.ToolSpec { return nil }

// assemblyHistory 造 `rounds` 轮"提问 + 工具调用 + 工具结果"，末尾再附一条当前提问，
// 消息按生成次序带标识 `m1..mN`（与 `internal/core/prompt_test.go` 的夹具同形；
// 装配记录要求被丢弃的消息带真实标识，空标识会让 `Build` 直接拒绝）。
// assemblyHistory mirrors the core fixture: n rounds plus the current question, ids m1..mN.
func assemblyHistory(rounds int) []core.Message {
	out := make([]core.Message, 0, rounds*3+1)
	seq := 0
	next := func(m core.Message) core.Message {
		seq++
		m.ID = fmt.Sprintf("m%d", seq)
		return m
	}
	for i := 1; i <= rounds; i++ {
		out = append(out,
			next(core.Message{Role: core.RoleUser, Content: fmt.Sprintf("u%d", i)}),
			next(core.Message{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{
				ID: fmt.Sprintf("c%d", i), Name: "read_file",
				Arguments: fmt.Sprintf(`{"path":"f%d.txt"}`, i),
			}}}),
			next(core.Message{
				Role: core.RoleTool, Content: fmt.Sprintf("t%d", i),
				ToolCallID: fmt.Sprintf("c%d", i),
			}),
		)
	}
	return append(out, next(core.Message{Role: core.RoleUser, Content: fmt.Sprintf("u%d", rounds+1)}))
}

// assemblyEnv 造一个只含系统提示与预算的运行环境：历史由 `ContextInputs.Recent` 显式
// 传入，不依赖会话（无状态范围正是装配记录的典型场景）。
// assemblyEnv builds an environment whose history comes in explicitly via Recent.
func assemblyEnv(contextTokens int) *core.RunEnv {
	return &core.RunEnv{
		Req: core.RunRequest{Message: "当前消息"},
		Snapshot: &core.ConfigSnapshot{
			AgentName:  "demo",
			SectionOne: "段一：你是 demo。",
			Profile:    &core.Profile{Settings: core.Settings{MaxHistoryTurns: 20}},
		},
		Budget: core.Budget{ContextTokens: contextTokens},
	}
}

// assemblyTrimRow 是读回 `history_trim` 列的形状（键名与该列的字段口径一致）。
// assemblyTrimRow mirrors the history_trim column's field names.
type assemblyTrimRow struct {
	DroppedGroups         int    `json:"dropped_groups"`
	FirstDroppedMessageID string `json:"first_dropped_message_id"`
	CutMessageID          string `json:"cut_message_id"`
	ReasonCode            string `json:"reason_code"`
	TokensBefore          int    `json:"tokens_before"`
	TokensAfter           int    `json:"tokens_after"`
	StrategyVersion       string `json:"strategy_version"`
}

// TestAssembly_HistoryTrimRecordsDroppedGroups 走一遍"真裁剪 → 落库 → 读回"：
// 组数、首条与切点的 message_id、原因码、前后估算、压缩结论与调用关联全部逐字段对照。
// TestAssembly_HistoryTrimRecordsDroppedGroups drives a real trim into the database.
func TestAssembly_HistoryTrimRecordsDroppedGroups(t *testing.T) {
	ctx := context.Background()
	db := newTxTestDB(t) // 隔离 schema + 已种内置调用方 local-admin
	_, err := db.pool.ExecContext(ctx, `INSERT INTO runs
		(run_id, agent, channel, user_id, trigger, status, started_at,
		 caller_id, authorizing_kind, key_id, actor)
		VALUES ('r1', 'demo', 'web', 'u1', 'user', 'running', $1,
		        'local-admin', 'admin-builtin', NULL, 'admin-cli')`, time.Now().UTC().UnixMilli())
	require.NoError(t, err, "装配记录的 run_id 外键需要一行 runs")

	builder := core.NewPromptBuilder(assemblyResolver{})
	msgs := assemblyHistory(3) // 3 组 + 当前提问 = 10 条（m1..m10）

	// 估算读数一律取自同一个构建器：大预算下不裁剪，`TokensAfter` 就是本轮全量估算。
	estimate := func(recent []core.Message) int {
		t.Helper()
		_, rec, err := builder.Build(ctx, assemblyEnv(1<<20), core.ContextInputs{Recent: recent})
		require.NoError(t, err)
		return rec.HistoryTrim.TokensAfter
	}
	afterTwoGroups := estimate(msgs[6:]) // 丢掉前两组（m1..m6）之后的估算

	// 上限 = "丢两组之后"的读数：不丢超限、丢一组仍超限、丢两组刚好不超。
	env := assemblyEnv(afterTwoGroups + core.ReserveTokens)
	prompt, rec, err := builder.Build(ctx, env, core.ContextInputs{Recent: msgs})
	require.NoError(t, err, "标定后的上限必须可装配（非裁员由不得超限）")

	// ① 裁剪真的发生了：这是本用例不空转的前提，先钉死。
	require.Equal(t, 2, rec.HistoryTrim.DroppedGroups, "m1 组与 m2 组各计一个完整工具组")
	require.Equal(t, core.HistoryTrimReasonGroupDropped, rec.HistoryTrim.ReasonCode,
		"本 US 唯一裁剪原因码")
	require.Equal(t, msgs[0].ID, rec.HistoryTrim.FirstDroppedMessageID, "首条被丢弃消息的标识")
	require.Equal(t, msgs[5].ID, rec.HistoryTrim.CutMessageID, "切点 = 最后一条被丢弃消息的标识")
	require.Greater(t, rec.HistoryTrim.TokensBefore, rec.HistoryTrim.TokensAfter,
		"裁剪前后读数必须不同，否则记录不出信息")
	require.Equal(t, core.DecisionNone, rec.Compression.Decision,
		"未接线压缩：真实结论是 none，不是空串")
	require.Len(t, prompt.History, len(msgs)-6, "整组丢弃后剩下的正是保留段")

	// ② 与调用关联的字段由循环填（`Build` 拿不到 Run 标识与轮次）。
	rec.RunID = "r1"
	rec.StepID = "s1"
	rec.Purpose = core.PurposeChat
	rec.Agent = "demo"
	rec.PromptVersion = "prompt-v1"
	rec.ModelVersion = "test-model"
	require.Zero(t, rec.LLMCallID, "装配发生在模型调用之前，此时还没有调用标识")

	logger := slog.New(slog.DiscardHandler)
	NewAssemblyStore(db, logger).Record(ctx, rec)

	// ③ 读回逐字段对照：不经写入方的任何中间状态，直接看库里那一行。
	var (
		recordID   string
		purpose    string
		llmCallID  sql.NullInt64
		trimRaw    []byte
		compRaw    []byte
		segments   []byte
		budgetRaw  []byte
		createdAt  int64
		estimatorV string
	)
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT record_id, purpose, llm_call_id, history_trim, compression,
		       final_segments, budget, estimator_version, created_at
		FROM context_assembly_records WHERE run_id = 'r1'`).
		Scan(&recordID, &purpose, &llmCallID, &trimRaw, &compRaw,
			&segments, &budgetRaw, &estimatorV, &createdAt))

	require.Regexp(t, `^ctxa_[0-9A-Z]+$`, recordID, "记录标识由写入器分配，带 ctxa_ 前缀")
	require.Equal(t, string(core.PurposeChat), purpose)
	require.False(t, llmCallID.Valid, "尚未分配调用标识时落 NULL，**不得**落 0 指着不存在的行")
	require.Equal(t, core.EstimateVersion, estimatorV, "估算器版本如实入账")
	require.Positive(t, createdAt, "写入器补时刻")

	// ④ `history_trim` 的每一格都来自本轮真实装配。
	var trim assemblyTrimRow
	require.NoError(t, json.Unmarshal(trimRaw, &trim))
	require.Equal(t, 2, trim.DroppedGroups, "组数落库后仍是 2（写成空占位即在此红）")
	require.Equal(t, msgs[0].ID, trim.FirstDroppedMessageID)
	require.Equal(t, msgs[5].ID, trim.CutMessageID)
	require.Equal(t, core.HistoryTrimReasonGroupDropped, trim.ReasonCode,
		"`group_dropped` 必须在库里可查，长会话裁剪才复现得出")
	require.Equal(t, rec.HistoryTrim.StrategyVersion, trim.StrategyVersion)
	require.Equal(t, rec.HistoryTrim.TokensBefore, trim.TokensBefore)
	require.Equal(t, rec.HistoryTrim.TokensAfter, trim.TokensAfter)
	require.Greater(t, trim.TokensBefore, trim.TokensAfter)

	// ⑤ 压缩结论与末段落：没有压缩时写的是"确实没压"这一事实。
	var compression struct {
		Enabled  bool   `json:"enabled"`
		Decision string `json:"decision"`
	}
	require.NoError(t, json.Unmarshal(compRaw, &compression))
	require.False(t, compression.Enabled)
	require.Equal(t, string(core.DecisionNone), compression.Decision)

	var segmentsRow []struct {
		ID    string `json:"id"`
		Order int    `json:"order"`
	}
	require.NoError(t, json.Unmarshal(segments, &segmentsRow))
	require.NotEmpty(t, segmentsRow, "本轮实际注入的段落不得为空")
	require.Equal(t, "system", segmentsRow[0].ID, "段序第一条是系统 + 工具定义")

	var budget struct {
		ContextTokens   int `json:"context_tokens"`
		ProtocolReserve int `json:"protocol_reserve"`
	}
	require.NoError(t, json.Unmarshal(budgetRaw, &budget))
	require.Equal(t, env.Budget.ContextTokens, budget.ContextTokens)
	require.Equal(t, core.ReserveTokens, budget.ProtocolReserve,
		"协议余量来自常量，不是配置键")

	// ⑥ 形状非法的记录就地丢弃，不进数据库：词表外的 purpose 会在 CHECK 上失败，但那时
	// 已经躺在 `db` 的错误路径里，定位成本远高于这里挡下。
	bad := rec
	bad.Purpose = core.AssemblyPurpose("bogus")
	NewAssemblyStore(db, logger).Record(ctx, bad)
	var rows int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM context_assembly_records`).Scan(&rows))
	require.Equal(t, 1, rows, "词表外的 purpose 必须被写入器丢弃，而不是撞 CHECK 留痕")
}
