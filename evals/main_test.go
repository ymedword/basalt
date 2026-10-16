package main

// 本文件是评测运行器**判卷逻辑**的单元测试：装载校验 / 可执行断言判定 / 计量完整性 /
// 读数提取 / 脚本回放 / 门控判定——都是纯函数或只依赖端口接口的部分，不需要真运行期。
// 端到端那半（装配 + 跑 + 落结果文件）由颗粒度文档 §6.1 的人工项与 T072 的实测承担。
// Unit tests for the judging logic; the assemble-and-run half is covered by the manual
// acceptance items and the T072 end-to-end readings.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
)

// ---------------------------------------------------------------------------
// 装载与校验
// ---------------------------------------------------------------------------

// TestLoadCases_LoadsInFilenameOrder 中文验收点：用例执行序**确定**（按文件名排序装载），
// 与目录读取序无关；非 *.yaml 的文件不参与装载。
func TestLoadCases_LoadsInFilenameOrder(t *testing.T) {
	dir := t.TempDir()
	writeCaseFile(t, dir, "b-second.yaml", fakeCaseYAML("second", nil))
	writeCaseFile(t, dir, "a-first.yaml", fakeCaseYAML("first", nil))
	writeCaseFile(t, dir, "notes.txt", "不是用例，不该被读")

	cases, err := loadCases(dir)
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second"}, caseIDs(cases),
		"装载序必须是文件名序（a-first 先于 b-second）")
}

// TestLoadCases_RejectsInvalidCases 中文验收点：一条用例不合法 → **整批拒绝**（半个批次的
// 读数比没有读数更危险）；逐类别点名，不让坏用例半路炸在运行时。
func TestLoadCases_RejectsInvalidCases(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(blocks map[string]string)
		wantSub string
	}{
		{
			name:    "id 形态非法",
			mutate:  func(b map[string]string) { b["id"] = "id: Bad_ID\n" },
			wantSub: `id "Bad_ID" 不合法`,
		},
		{
			name:    "kind 不在四类读数内",
			mutate:  func(b map[string]string) { b["kind"] = "kind: 性能\n" },
			wantSub: "不在四类读数内",
		},
		{
			name:    "message 为空",
			mutate:  func(b map[string]string) { b["message"] = "message: \"\"\n" },
			wantSub: "message 必填",
		},
		{
			name: "fixture.agent 形态非法",
			mutate: func(b map[string]string) {
				b["fixture"] = strings.Replace(b["fixture"], "  agent: good", "  agent: ../escape", 1)
			},
			wantSub: "fixture.agent",
		},
		{
			name:    "缺 fixture（连带 agent 与 agent_md）",
			mutate:  func(b map[string]string) { delete(b, "fixture") },
			wantSub: "fixture.agent",
		},
		{
			name: "夹具路径是绝对路径",
			mutate: func(b map[string]string) {
				b["fixture"] = strings.Replace(b["fixture"], "    notes.txt: |", "    \"/etc/passwd\": |", 1)
			},
			wantSub: "是绝对路径",
		},
		{
			name: "夹具路径逃出工作区",
			mutate: func(b map[string]string) {
				b["fixture"] = strings.Replace(b["fixture"], "    notes.txt: |", "    \"../../outside.txt\": |", 1)
			},
			wantSub: "逃出工作区",
		},
		{
			name:    "预算为负",
			mutate:  func(b map[string]string) { b["budget"] = "budget:\n  max_iterations: -3\n" },
			wantSub: "不得为负",
		},
		{
			name:    "fake 档缺 script",
			mutate:  func(b map[string]string) { delete(b, "script") },
			wantSub: "fake 档必须给 script",
		},
		{
			name:    "expect.status 写 running",
			mutate:  func(b map[string]string) { b["expect"] = expectBlock("running", "call-1", "success") },
			wantSub: "不得写 running",
		},
		{
			name:    "expect.status 不是五值之一",
			mutate:  func(b map[string]string) { b["expect"] = expectBlock("done", "call-1", "success") },
			wantSub: "不是运行状态五值之一",
		},
		{
			name:    "审计断言引用不存在的 tool_call_id",
			mutate:  func(b map[string]string) { b["expect"] = expectBlock("completed", "call-9", "success") },
			wantSub: "引用了脚本里不存在的 tool_call_id",
		},
		{
			name:    "审计断言状态不是四值之一",
			mutate:  func(b map[string]string) { b["expect"] = expectBlock("completed", "call-1", "cancelled") },
			wantSub: "不是审计状态四值之一",
		},
		{
			name:    "未知键（写错键名的用例必须装载期拒绝）",
			mutate:  func(b map[string]string) { b["budget"] = "budget:\n  max_iteration: 3\n" },
			wantSub: "解析失败",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeCaseFile(t, dir, "case.yaml", fakeCaseYAML("good", tt.mutate))

			_, err := loadCases(dir)
			require.Error(t, err, "这条用例必须被装载期拒绝")
			require.Contains(t, err.Error(), tt.wantSub)
		})
	}
}

// TestLoadCases_RejectsRealModelScript 中文验收点：真模型档**不得**写 script——留着脚本会
// 让读数看起来是脚本控制的（那是假的确定性）。
func TestLoadCases_RejectsRealModelScript(t *testing.T) {
	dir := t.TempDir()
	writeCaseFile(t, dir, "case.yaml", fakeCaseYAML("gated", func(b map[string]string) {
		b["provider"] = "provider: deepseek\n"
	}))

	_, err := loadCases(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "不得写 script")
}

// TestLoadCases_RejectsDuplicateIDs 中文验收点：id 重复 → 整批拒绝，且**两条路径都点名**
// （只说后一条会让排障的人不知道该跟谁撞了）。
func TestLoadCases_RejectsDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	writeCaseFile(t, dir, "a.yaml", fakeCaseYAML("dup", nil))
	writeCaseFile(t, dir, "b.yaml", fakeCaseYAML("dup", nil))

	_, err := loadCases(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "用例 id 重复")
	require.Contains(t, err.Error(), "a.yaml")
	require.Contains(t, err.Error(), "b.yaml")
}

// TestLoadCases_RejectsEmptyAndMissingDirectory 中文验收点：空目录（没有 *.yaml）→ 报错退出，
// 而不是"零用例、零失败"地收场（静默跑零条等于把"没跑"报成"全过"）；目录不存在 → 错误
// 链里保留底层原因。
func TestLoadCases_RejectsEmptyAndMissingDirectory(t *testing.T) {
	dir := t.TempDir()

	_, err := loadCases(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "没有 *.yaml 用例")

	_, err = loadCases(filepath.Join(dir, "nope"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestSessionID_DerivedFromCaseID 中文验收点：评测会话标识由用例 id 派生并带 `eval-` 前缀，
// 与命令面的「渠道 + 用户 + 画像」派生形态明确分开——评测会话不得伪装成某个用户建的会话。
func TestSessionID_DerivedFromCaseID(t *testing.T) {
	require.Equal(t, "eval-tool-selection", sessionID("tool-selection"))
	require.True(t, strings.HasPrefix(sessionID("x"), "eval-"), "前缀是评测会话的判定位")
}

// TestBudgetSpec_CoreBudgetMapsThreeFields 中文验收点：用例预算的零值原样传下去（由核心的
// 合并逻辑填缺省）——运行器**不**在这里补默认值，否则缺省口径就有第二个来源。
func TestBudgetSpec_CoreBudgetMapsThreeFields(t *testing.T) {
	require.Equal(t, core.Budget{}, budgetSpec{}.coreBudget(), "零值照传，缺省归核心")

	got := budgetSpec{MaxIterations: 3, MaxToolCalls: 7, ContextTokens: 2048}.coreBudget()
	require.Equal(t, 3, got.MaxIterations)
	require.Equal(t, 7, got.MaxToolCalls)
	require.Equal(t, 2048, got.ContextTokens)
	require.Zero(t, got.ToolTimeout, "用例口径不含工具超时——它不在 YAML 面上")
}

// ---------------------------------------------------------------------------
// 计量完整性
// ---------------------------------------------------------------------------

// TestAssessUsage_CompletenessBranches 中文验收点：计量完整性三支判全——①无行 → 不完整；
// ②任何行 pending / unknown → 不完整（点名轮号）；③成功行三列全零 → 不完整（**不得**把
// 库中默认零读成零成本）。失败行三列为零**不**判不完整（调用没成功就没有用量，是事实）。
func TestAssessUsage_CompletenessBranches(t *testing.T) {
	tests := []struct {
		name     string
		views    []core.LLMCallView
		want     usageReading
		complete bool
		wantNote string
	}{
		{
			name:     "无审计行",
			views:    nil,
			complete: false,
			wantNote: "没有任何模型调用审计行",
		},
		{
			name: "成功行带用量",
			views: []core.LLMCallView{
				{Round: 1, Status: core.LLMCallSuccess, PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
			},
			want:     usageReading{Prompt: 100, Completion: 20, Total: 120},
			complete: true,
		},
		{
			name: "多行求和（含失败行的零）",
			views: []core.LLMCallView{
				{Round: 1, Status: core.LLMCallSuccess, PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
				{Round: 2, Status: core.LLMCallFailed},
				{Round: 3, Status: core.LLMCallSuccess, PromptTokens: 80, CompletionTokens: 10, TotalTokens: 90},
			},
			want:     usageReading{Prompt: 180, Completion: 30, Total: 210},
			complete: true,
		},
		{
			name: "行仍悬空 pending",
			views: []core.LLMCallView{
				{Round: 1, Status: core.LLMCallSuccess, PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11},
				{Round: 2, Status: core.LLMCallPending},
			},
			want:     usageReading{Prompt: 10, Completion: 1, Total: 11},
			complete: false,
			wantNote: "第 2 轮审计行仍未落终态",
		},
		{
			name: "行被恢复扫描归一为 unknown",
			views: []core.LLMCallView{
				{Round: 1, Status: core.LLMCallUnknown},
			},
			complete: false,
			wantNote: "第 1 轮审计行仍未落终态",
		},
		{
			name: "成功行三列全零（供应商未回用量）",
			views: []core.LLMCallView{
				{Round: 1, Status: core.LLMCallSuccess},
				{Round: 2, Status: core.LLMCallSuccess, PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10},
			},
			want:     usageReading{Prompt: 5, Completion: 5, Total: 10},
			complete: false,
			wantNote: "供应商未回用量",
		},
		{
			name: "失败行三列为零不算不完整",
			views: []core.LLMCallView{
				{Round: 1, Status: core.LLMCallFailed},
			},
			complete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage, complete, note := assessUsage(tt.views)
			require.Equal(t, tt.want, usage, "三列求和必须覆盖全部行")
			require.Equal(t, tt.complete, complete)
			if tt.complete {
				require.Empty(t, note, "完整时不该有任何说明")
				return
			}
			require.Contains(t, note, "计量不完整")
			require.Contains(t, note, tt.wantNote)
		})
	}
}

// TestAssessUsage_EmptyReadsAsIncompleteNeverZeroCost 中文验收点：零行时求和确实是三个零，
// 但完整性标记必须是 false——库中默认零**不得**被读成零成本。
func TestAssessUsage_EmptyReadsAsIncompleteNeverZeroCost(t *testing.T) {
	usage, complete, note := assessUsage(nil)
	require.Equal(t, usageReading{}, usage)
	require.False(t, complete)
	require.NotEmpty(t, note)
}

// ---------------------------------------------------------------------------
// 可执行断言判定
// ---------------------------------------------------------------------------

// TestJudge_PassesEveryExpectation 中文验收点：全部命中时逐条判过，且**不留 Detail**
// （结果文件不该被成功项的说明淹没）。
func TestJudge_PassesEveryExpectation(t *testing.T) {
	ec := fakeCase("good")
	obs := observation{
		result: core.RunResult{
			Status:    core.StatusCompleted,
			FinalText: "按文件内容总结：三条要点。",
		},
		toolSeq:     []string{"read_file"},
		toolResults: []string{`{"content":"工作区笔记正文"}`},
		llmCalls:    []core.LLMCallView{{Round: 1, Status: core.LLMCallSuccess, TotalTokens: 42}},
		audit:       []auditReading{{ToolCallID: "call-1", Found: true, Status: "success"}},
		usage:       usageReading{Total: 42},
		complete:    true,
	}

	checks := judge(ec, obs)
	require.Len(t, checks, 3, "status + text_contains + audit 三条")
	require.True(t, allChecksPassed(checks), "全部断言应判过：%+v", checks)
	for _, c := range checks {
		require.True(t, c.OK, "断言 %s", c.Name)
		require.Empty(t, c.Detail, "通过的断言不得带 Detail")
	}
}

// TestJudge_ReportsBothSidesOnFailure 中文验收点：任一断言不中即 FAIL，Detail 给出
// **期望 vs 实际**（读数要能自证，而不是只给一句"没通过"）。
func TestJudge_ReportsBothSidesOnFailure(t *testing.T) {
	ec := fakeCase("good")
	ec.Expect.TextContains = []string{"不存在的片段"}
	ec.Expect.ToolSequence = &[]string{"read_file"}
	ec.Expect.LLMCalls = ptr(9)

	obs := observation{
		result: core.RunResult{
			Status: core.StatusLimitReached,
			Reason: "达迭代上限（3 轮）",
		},
		toolSeq:  []string{"read_file", "list_dir"},
		llmCalls: []core.LLMCallView{{Round: 1, Status: core.LLMCallSuccess, TotalTokens: 5}},
		audit:    []auditReading{{ToolCallID: "call-1", Found: true, Status: "success"}},
		usage:    usageReading{Total: 5},
		complete: true,
	}

	checks := judge(ec, obs)
	require.False(t, allChecksPassed(checks))

	byName := map[string]checkResult{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	require.Contains(t, byName["status"].Detail, "期望状态 completed，实际 limit_reached")
	require.Contains(t, byName["status"].Detail, "运行给出的原因：达迭代上限",
		"非正常终止要带运行给的原因")
	require.Contains(t, byName["tool_sequence"].Detail, "期望序列 [read_file]，实际序列 [read_file list_dir]")
	require.Contains(t, byName["text_contains"].Detail, `期望命中 "不存在的片段"`)
	require.Contains(t, byName["llm_calls"].Detail, "期望 9，实际 1")
}

// TestJudge_TextContainsJudgesFinalTextOnly 中文验收点：`text_contains` 只判**最终回答**，
// 不把工具结果并进同一片文本域——否则"文件里有这段话"会被读成"模型答了这段话"。
func TestJudge_TextContainsJudgesFinalTextOnly(t *testing.T) {
	ec := fakeCase("good")
	ec.Expect.TextContains = []string{"工作区笔记正文"}

	obs := observation{
		result:      core.RunResult{Status: core.StatusCompleted, FinalText: "总结如上。"},
		toolResults: []string{"工作区笔记正文"},
		audit:       []auditReading{{ToolCallID: "call-1", Found: true, Status: "success"}},
	}

	checks := judge(ec, obs)
	require.False(t, allChecksPassed(checks), "片段只在工具结果里出现时不算命中")
	for _, c := range checks {
		if c.Name == "text_contains" {
			require.Contains(t, c.Detail, "观测到 1 段文本")
		}
	}
}

// TestJudge_ToolResultsContainJudgesToolMessages 中文验收点：`tool_results_contain` 判的是
// role=tool 的消息正文（四段式拒绝文案的判据落在这里——那是工具回填，不是模型回答）。
func TestJudge_ToolResultsContainJudgesToolMessages(t *testing.T) {
	ec := fakeCase("good")
	ec.Expect.TextContains = nil // 本用例只判工具结果，最终回答不参与
	ec.Expect.ToolResultsContain = []string{"读取目标不在可读根之内"}

	obs := observation{
		result:      core.RunResult{Status: core.StatusCompleted, FinalText: "换条路。"},
		toolResults: []string{"违规：读取目标不在可读根之内。规则出处：沙箱读白名单。"},
		audit:       []auditReading{{ToolCallID: "call-1", Found: true, Status: "success"}},
	}
	require.True(t, allChecksPassed(judge(ec, obs)))

	obs.toolResults = []string{"读取成功"}
	require.False(t, allChecksPassed(judge(ec, obs)))
}

// TestJudge_AuditAssertionsKeyedByToolCallID 中文验收点：审计断言按 tool_call_id 定位——
// 行**必须存在**（未命中即失败，不是"跳过"）；状态与失败分类逐项判；只写 id 的断言
// 只判"行存在"（不替用例发明状态期望）。
func TestJudge_AuditAssertionsKeyedByToolCallID(t *testing.T) {
	readings := []auditReading{
		{ToolCallID: "call-1", Found: true, Status: "failed", ErrorKind: "sandbox_denied"},
		{ToolCallID: "call-2", Found: false},
	}

	hit := checkAudit(auditSpec{ToolCallID: "call-1", Status: "failed", ErrorKind: "sandbox_denied"}, readings)
	require.True(t, hit.OK)
	require.Equal(t, "audit:call-1", hit.Name, "断言名必须带 tool_call_id——多行审计下才分得清")

	missing := checkAudit(auditSpec{ToolCallID: "call-2", Status: "failed"}, readings)
	require.False(t, missing.OK)
	require.Contains(t, missing.Detail, "没有审计行")

	wrongKind := checkAudit(auditSpec{ToolCallID: "call-1", ErrorKind: "timeout"}, readings)
	require.False(t, wrongKind.OK)
	require.Contains(t, wrongKind.Detail, "期望失败分类 timeout，实际 sandbox_denied")
	require.Contains(t, wrongKind.Detail, "状态 failed", "分类不符时把状态一并带上")

	wrongStatus := checkAudit(auditSpec{ToolCallID: "call-1", Status: "success"}, readings)
	require.False(t, wrongStatus.OK)
	require.Contains(t, wrongStatus.Detail, "期望审计状态 success，实际 failed")

	exists := checkAudit(auditSpec{ToolCallID: "call-1"}, readings)
	require.True(t, exists.OK, "只写 tool_call_id 的断言只判「行存在」")
}

// TestJudge_MinTotalTokensAndUsageComplete 中文验收点：token 下界按 ≥ 判且说明**不换算金额**；
// 计量完整性按期望值判（期望"不完整"且确实不完整 = 判过——这是"读数确实缺失"的断言）。
func TestJudge_MinTotalTokensAndUsageComplete(t *testing.T) {
	ec := fakeCase("good")
	ec.Expect.TextContains = nil // 本用例只判用量与计量完整性
	ec.Expect.MinTotalTokens = ptr(100)
	ec.Expect.UsageComplete = ptr(true)

	obs := observation{
		result:    core.RunResult{Status: core.StatusCompleted},
		audit:     []auditReading{{ToolCallID: "call-1", Found: true, Status: "success"}},
		usage:     usageReading{Total: 99},
		complete:  false,
		usageNote: "计量不完整：第 1 轮调用成功但三列 token 均为零（供应商未回用量）",
	}

	byName := map[string]checkResult{}
	for _, c := range judge(ec, obs) {
		byName[c.Name] = c
	}
	require.Contains(t, byName["min_total_tokens"].Detail, "期望总用量 ≥ 100，实际 99")
	require.Contains(t, byName["min_total_tokens"].Detail, "不换算金额")
	require.Contains(t, byName["usage_complete"].Detail, "期望计量完整性 true，实际 false")
	require.Contains(t, byName["usage_complete"].Detail, "供应商未回用量", "不完整的原因要留在读数里")

	ec.Expect.UsageComplete = ptr(false)
	require.True(t, allChecksPassed(judge(ec, observation{
		result:   core.RunResult{Status: core.StatusCompleted},
		audit:    []auditReading{{ToolCallID: "call-1", Found: true, Status: "success"}},
		usage:    usageReading{Total: 150},
		complete: false,
	})), "期望「计量不完整」且确实不完整时判过")
}

// TestJudge_EmptyCheckSetNeverPasses 中文验收点：零条断言**不算通过**——没有断言的用例
// 只会产生"看起来跑过了"的假读数。
func TestJudge_EmptyCheckSetNeverPasses(t *testing.T) {
	require.False(t, allChecksPassed(nil))
	require.False(t, allChecksPassed([]checkResult{}))
}

// TestCheckStatus_ReasonIsPreviewed 中文验收点：状态不符时带运行给出的原因，且原因**截断**
// 后回显（不得把整段正文回显进结果文件）。
func TestCheckStatus_ReasonIsPreviewed(t *testing.T) {
	long := strings.Repeat("原因", 300)
	check := checkStatus(string(core.StatusCompleted), core.RunResult{
		Status: core.StatusFailed,
		Reason: long,
	})
	require.False(t, check.OK)
	require.Contains(t, check.Detail, "未回显")
	require.Less(t, len([]rune(check.Detail)), len([]rune(long)), "详情必须短于原文")
}

// ---------------------------------------------------------------------------
// 读数提取
// ---------------------------------------------------------------------------

// TestToolSequence_FromAssistantMessages 中文验收点：工具序列取自**助手消息里的工具调用**
// （循环逐条执行并按同一顺序回填，消息序即执行序），跨多条助手消息按序合并；工具结果取自
// role=tool 的正文。
func TestToolSequence_FromAssistantMessages(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleUser, Content: "读一下 notes.txt"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{
			{ID: "call-1", Name: "read_file"}, {ID: "call-2", Name: "list_dir"},
		}},
		{Role: core.RoleTool, ToolCallID: "call-1", Content: `{"content":"正文"}`},
		{Role: core.RoleTool, ToolCallID: "call-2", Content: `{"content":"a.txt"}`},
		{Role: core.RoleAssistant, Content: "总结完成"},
	}
	require.Equal(t, []string{"read_file", "list_dir"}, toolSequence(msgs))
	require.Equal(t, []string{`{"content":"正文"}`, `{"content":"a.txt"}`}, toolResultTexts(msgs))
}

// TestToolSequence_EmptyWhenNoToolCalls 中文验收点：无工具调用的一跑 → 空序列（读数写 `[]`，
// 而不是 null 或缺项）。
func TestToolSequence_EmptyWhenNoToolCalls(t *testing.T) {
	msgs := []core.Message{
		{Role: core.RoleUser, Content: "你好"},
		{Role: core.RoleAssistant, Content: "你好，有什么可以帮你？"},
	}
	require.Empty(t, toolSequence(msgs))
	require.Empty(t, toolResultTexts(msgs))
}

// TestObserve_ReadsProductionSurfaces 中文验收点：读数全部来自**生产同一条读面**——
// 会话消息（工具序列 / 工具结果）+ 模型调用审计行（用量与计量完整性）+ 工具审计行
// （按脚本给出的 tool_call_id 逐条定位）；会话标识用派生值、运行标识用本次运行的。
func TestObserve_ReadsProductionSurfaces(t *testing.T) {
	ec := fakeCase("good")
	runID := "cli-1700000000000-abc123"

	sessions := &stubSessions{msgs: []core.Message{
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "call-1", Name: "read_file"}}},
		{Role: core.RoleTool, ToolCallID: "call-1", Content: `{"content":"正文"}`},
	}}
	calls := &stubCalls{views: []core.LLMCallView{
		{Round: 1, Status: core.LLMCallSuccess, PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	}}
	invocations := &stubInvocations{rows: map[string]core.ToolInvocationView{
		"call-1": {Status: core.LLMCallFailed, ErrorKind: core.ErrorKindSandboxDenied},
	}}

	obs, err := observe(context.Background(), caseReaders{
		sessions: sessions, invocations: invocations, calls: calls,
	}, ec, core.RunResult{RunID: runID, Status: core.StatusCompleted})

	require.NoError(t, err)
	require.Equal(t, []string{"read_file"}, obs.toolSeq)
	require.Equal(t, []string{`{"content":"正文"}`}, obs.toolResults)
	require.Len(t, obs.llmCalls, 1)
	require.Equal(t, usageReading{Prompt: 7, Completion: 3, Total: 10}, obs.usage)
	require.True(t, obs.complete)
	require.Equal(t, []auditReading{
		{ToolCallID: "call-1", Found: true, Status: "failed", ErrorKind: "sandbox_denied"},
	}, obs.audit)

	require.Equal(t, sessionID("good"), sessions.lastID, "会话标识必须是派生出来的那个")
	require.Equal(t, runID, calls.lastRunID, "用量读数按运行标识取")
	require.Equal(t, runID, invocations.lastRunID, "审计读面同样按运行标识取")
}

// TestObserve_AuditMissIsAReadingNotAnError 中文验收点：审计未命中记一条 Found=false 的
// 读数（不是错误）——"这次调用没有审计行"正是运行时归属校验要判的事实。
func TestObserve_AuditMissIsAReadingNotAnError(t *testing.T) {
	obs, err := observe(context.Background(), caseReaders{
		sessions:    &stubSessions{},
		invocations: &stubInvocations{rows: map[string]core.ToolInvocationView{}},
		calls:       &stubCalls{},
	}, fakeCase("good"), core.RunResult{RunID: "cli-1-abc123"})

	require.NoError(t, err)
	require.Equal(t, []auditReading{{ToolCallID: "call-1", Found: false}}, obs.audit)
	require.False(t, obs.complete, "零审计行 → 计量不完整")
}

// TestObserve_PropagatesReadErrors 中文验收点：读面报错即如实上抛（读数不完整时**不得**
// 拿零值接着判卷——那会把"读不出来"读成"用量为零"）。
func TestObserve_PropagatesReadErrors(t *testing.T) {
	ec := fakeCase("good")
	boom := errors.New("库被锁住了")

	_, err := observe(context.Background(), caseReaders{
		sessions:    &stubSessions{err: boom},
		invocations: &stubInvocations{},
		calls:       &stubCalls{},
	}, ec, core.RunResult{RunID: "r"})
	require.ErrorIs(t, err, boom)
	require.Contains(t, err.Error(), "读取会话消息")

	_, err = observe(context.Background(), caseReaders{
		sessions:    &stubSessions{},
		invocations: &stubInvocations{},
		calls:       &stubCalls{err: boom},
	}, ec, core.RunResult{RunID: "r"})
	require.ErrorIs(t, err, boom)
	require.Contains(t, err.Error(), "读取模型调用审计行")

	_, err = observe(context.Background(), caseReaders{
		sessions:    &stubSessions{},
		invocations: &stubInvocations{err: boom},
		calls:       &stubCalls{},
	}, ec, core.RunResult{RunID: "r"})
	require.ErrorIs(t, err, boom)
	require.Contains(t, err.Error(), "读取工具审计行")
}

// TestScriptToolCallIDs_FlattensInScriptOrder 中文验收点：审计读数的定位键按脚本顺序铺平
// ——多条回答里的调用一次性取全，顺序即执行序。
func TestScriptToolCallIDs_FlattensInScriptOrder(t *testing.T) {
	ec := fakeCase("good")
	ec.Script = []scriptReply{
		{ToolCalls: []toolCallSpec{{ID: "call-1", Name: "read_file"}, {ID: "call-2", Name: "list_dir"}}},
		{ToolCalls: []toolCallSpec{{ID: "call-3", Name: "read_file"}}},
	}
	require.Equal(t, []string{"call-1", "call-2", "call-3"}, scriptToolCallIDs(ec))

	plain := fakeCase("plain")
	plain.Script = []scriptReply{{Content: "直接给出回答"}}
	require.Empty(t, scriptToolCallIDs(plain), "一次工具都不调的用例没有审计定位键")
}

// ---------------------------------------------------------------------------
// 假模型与门控
// ---------------------------------------------------------------------------

// TestScriptedClient_ReplaysInOrderThenErrors 中文验收点：逐条按序回放；**脚本用尽即报错**
// （不静默重放最后一条——用例写漏回答就该红，而不是拿到一份看起来跑通了的读数）。
func TestScriptedClient_ReplaysInOrderThenErrors(t *testing.T) {
	client := newScriptedClient([]scriptReply{
		{Content: "先看看文件", ToolCalls: []toolCallSpec{
			{ID: "call-1", Name: "read_file", Arguments: `{"path":"notes.txt"}`},
		}, Usage: usageSpec{Prompt: 100, Completion: 20}},
		{Content: "总结完成", Usage: usageSpec{Prompt: 10, Completion: 4}},
	})

	first, err := client.Chat(context.Background(), core.ChatRequest{Round: 1})
	require.NoError(t, err)
	require.Equal(t, "先看看文件", first.Content)
	require.Equal(t, []core.ToolCall{{ID: "call-1", Name: "read_file", Arguments: `{"path":"notes.txt"}`}}, first.ToolCalls)
	require.Equal(t, core.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, first.Usage,
		"只写提示 / 补全两项时合成总量")

	second, err := client.Chat(context.Background(), core.ChatRequest{Round: 2})
	require.NoError(t, err)
	require.Equal(t, "总结完成", second.Content)
	require.Empty(t, second.ToolCalls)
	require.Equal(t, core.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}, second.Usage)

	_, err = client.Chat(context.Background(), core.ChatRequest{Round: 3})
	require.Error(t, err, "用例只写了两条回答，第三次调用必须报错")
	require.Contains(t, err.Error(), "脚本已用尽")
	require.Equal(t, 2, client.callCount())
}

// TestScriptedClient_CopiesScriptAndCancellationWins 中文验收点：脚本在构造时拷一份（用例
// 定义不被回放过程改写）；ctx 已取消时假模型也不作答（与真实客户端同序：先查取消）。
func TestScriptedClient_CopiesScriptAndCancellationWins(t *testing.T) {
	script := []scriptReply{{Content: "第一条"}}
	client := newScriptedClient(script)
	script[0].Content = "被外部改写"

	resp, err := client.Chat(context.Background(), core.ChatRequest{})
	require.NoError(t, err)
	require.Equal(t, "第一条", resp.Content, "脚本是拷贝，外部改动不影响回放")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Chat(ctx, core.ChatRequest{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, client.callCount(), "取消的运行不消耗脚本")
}

// TestGateSkip_MissingConditionsSkipRealModelCases 中文验收点：真模型档**缺一不发**——
// 未给 --provider / 给了别的名字 / 没有配置文件 / 配置里没有该条目，四种情形一律跳过；
// fake 档（provider 为空）恒执行。
func TestGateSkip_MissingConditionsSkipRealModelCases(t *testing.T) {
	t.Chdir(t.TempDir())

	fake := fakeCase("good")
	require.Empty(t, gateSkip(fake, ""), "fake 档不受门控影响")
	require.Empty(t, gateSkip(fake, "deepseek"))

	gated := fakeCase("gated")
	gated.Provider = "deepseek"
	require.Contains(t, gateSkip(gated, ""), "未门控")
	require.Contains(t, gateSkip(gated, ""), "本次未给 --provider")
	require.Contains(t, gateSkip(gated, "qwen"), "门控的是 qwen")

	require.Contains(t, gateSkip(gated, "deepseek"), "工作目录没有 "+configFileName)

	require.NoError(t, os.WriteFile(configFileName, []byte("providers:\n  - name: qwen\n"), 0o644))
	require.Contains(t, gateSkip(gated, "deepseek"), "没有名为 deepseek 的供应商条目")

	require.NoError(t, os.WriteFile(configFileName, []byte("providers:\n  - name: deepseek\n"), 0o644))
	require.Empty(t, gateSkip(gated, "deepseek"), "四样齐了才不跳过")
}

// TestConfigHasProvider 中文验收点：门控前的配置检查只回答"同名条目在不在"——键值级查找，
// 不解析凭证（明文凭证在这一步不接触）；非配置文本判为不成立（不是判成有）。
func TestConfigHasProvider(t *testing.T) {
	raw := []byte("providers:\n  - name: probe\n    base_url: http://127.0.0.1:1\n    api_key: ${EVAL_KEY}\n")
	require.True(t, configHasProvider(raw, "probe"))
	require.False(t, configHasProvider(raw, "deepseek"))
	require.False(t, configHasProvider([]byte("providers: 不是列表\n"), "probe"))
	require.False(t, configHasProvider(nil, "probe"))
}

// TestExecuteCase_SkipsUngatedRealModelCase 中文验收点：未门控的真模型用例**跳过且不计入
// 通过**（跳过不是通过——把"没跑"读成"过了"是评测最坏的失效形态）；跳过路径不建临时
// 工作区、不装配运行期，故不产生运行标识与读数。
func TestExecuteCase_SkipsUngatedRealModelCase(t *testing.T) {
	t.Chdir(t.TempDir())

	ec := fakeCase("gated")
	ec.Provider = "deepseek"

	cr := executeCase(context.Background(), ec, "", testLogger())
	require.True(t, cr.Skipped)
	require.False(t, cr.Passed)
	require.Empty(t, cr.Error)
	require.Equal(t, "provider:deepseek", cr.Backend)
	require.Equal(t, "gated", cr.ID)
	require.Equal(t, kindToolSelection, cr.Kind)
	require.Contains(t, cr.SkipReason, "缺一不发")
	require.Empty(t, cr.RunID, "跳过的用例没有运行标识——没有读数可言")
	require.Empty(t, cr.Checks, "跳过的用例不判卷")
}

// ---------------------------------------------------------------------------
// 夹具装配与结果文件
// ---------------------------------------------------------------------------

// TestBuildWorkspace_LaysOutFixtureAndProbeConfig 中文验收点：夹具落在**临时工作区**里
// （骨架由生产代码建、agents/ 下逐 Agent 目录、工作区文件相对 `.basalt/`）；fake 档配置
// 里的凭证是 `${VAR}` 占位形态——明文不落盘（凭证复核的人工项在这里有一份自动判据）。
func TestBuildWorkspace_LaysOutFixtureAndProbeConfig(t *testing.T) {
	workDir := t.TempDir()
	ec := fakeCase("good")
	ec.Fixture.WorkspaceFiles = map[string]string{
		"agents/coworker/AGENT.md":   "---\nname: coworker\n---\n",
		"agents/coworker/secret.txt": "别的 Agent 的正文\n",
	}

	require.NoError(t, buildWorkspace(workDir, ec))

	wsRoot := filepath.Join(workDir, workspaceDirName)
	agentDir := filepath.Join(wsRoot, agentsDirName, "good")
	require.FileExists(t, filepath.Join(agentDir, agentFileName))
	require.FileExists(t, filepath.Join(agentDir, "notes.txt"))
	require.FileExists(t, filepath.Join(wsRoot, agentsDirName, "coworker", "secret.txt"))
	require.DirExists(t, filepath.Join(wsRoot, "skills"), "骨架由生产代码建（六个目录齐）")

	raw, err := os.ReadFile(filepath.Join(workDir, configFileName))
	require.NoError(t, err)
	require.Contains(t, string(raw), "${"+probeAPIKeyEnv+"}", "配置里只许出现占位形态")
	require.NotContains(t, string(raw), probeAPIKeyValue, "哨兵值不得落盘")
	require.Contains(t, string(raw), "name: "+probeProviderName)
}

// TestBuildWorkspace_GatedCopiesConfigVerbatim 中文验收点：真模型档把仓库根的 basalt.yaml
// **原样复制**——真模型读的就是运维写的那份配置，评测不替它改编。
func TestBuildWorkspace_GatedCopiesConfigVerbatim(t *testing.T) {
	t.Chdir(t.TempDir())
	original := "# 运维写的配置：注释与键序都要原样保留\nproviders:\n  - name: deepseek\n    base_url: https://api.example.com/v1\n    api_key: ${DS_KEY}\n"
	require.NoError(t, os.WriteFile(configFileName, []byte(original), 0o644))

	workDir := t.TempDir()
	ec := fakeCase("gated")
	ec.Provider = "deepseek"
	require.NoError(t, buildWorkspace(workDir, ec))

	raw, err := os.ReadFile(filepath.Join(workDir, configFileName))
	require.NoError(t, err)
	require.Equal(t, original, string(raw), "逐字节相同")
}

// TestBuildWorkspace_MissingConfigIsAnError 中文验收点：真模型档拿不到配置文件即报错
// （不生成一份替代品——那会让门控在"没配置"时也看起来成立）。
func TestBuildWorkspace_MissingConfigIsAnError(t *testing.T) {
	t.Chdir(t.TempDir())

	ec := fakeCase("gated")
	ec.Provider = "deepseek"
	err := buildWorkspace(t.TempDir(), ec)
	require.Error(t, err)
	require.Contains(t, err.Error(), configFileName)
}

// TestBuildWorkspace_AgentMDUsesDeclaredProvider 中文验收点：夹具的 AGENT.md 声明的是用例
// 指定的 provider（fake 档即探针名）——路由查表因此仍是一次真实查表，且 Agent 名与目录名
// 一致（装载期的注册判据）。
func TestBuildWorkspace_AgentMDUsesDeclaredProvider(t *testing.T) {
	workDir := t.TempDir()
	ec := fakeCase("good")
	require.NoError(t, buildWorkspace(workDir, ec))

	raw, err := os.ReadFile(filepath.Join(workDir, workspaceDirName, agentsDirName, "good", agentFileName))
	require.NoError(t, err)
	require.Contains(t, string(raw), "name: good", "Agent 名必须与目录名一致")
	require.Contains(t, string(raw), "provider: {name: "+probeProviderName)
}

// TestNewResultPath_AvoidsOverwrite 中文验收点：同一秒内的第二次运行**不覆盖**第一次的
// 结果文件（"连跑两次读数一致"这条复核依赖两份文件都在）。
func TestNewResultPath_AvoidsOverwrite(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	first, err := newResultPath(dir, at)
	require.NoError(t, err)
	require.Equal(t, "20261016-120000.json", filepath.Base(first))
	require.NoError(t, os.WriteFile(first, []byte("{}"), 0o644))

	second, err := newResultPath(dir, at)
	require.NoError(t, err)
	require.Equal(t, "20261016-120000-2.json", filepath.Base(second))

	require.NoError(t, os.WriteFile(second, []byte("{}"), 0o644))
	third, err := newResultPath(dir, at)
	require.NoError(t, err)
	require.Equal(t, "20261016-120000-3.json", filepath.Base(third))
}

// TestWriteReport_NoMoneyFields 中文验收点：结果文件只有 token 用量与调用次数，**没有**任何
// 金额口径字段（资源用量不换算金额），且口径三项（估算器版本 / 余量 / 时间与用例目录）
// 落在顶层。
func TestWriteReport_NoMoneyFields(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	rpt := &report{
		GeneratedAt:     at.Format(time.RFC3339),
		CasesDir:        defaultCasesDir,
		EstimateVersion: core.EstimateVersion,
		ReserveTokens:   core.ReserveTokens,
		Totals:          totals{Total: 1, Passed: 1},
		Kinds:           map[string]int{kindToolSelection: 1},
	}

	path, err := writeReport(dir, rpt, at)
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(raw)
	for _, banned := range []string{"cost", "money", "price", "usd", "amount", "金额"} {
		require.NotContains(t, strings.ToLower(text), banned, "结果文件不得出现金额口径字段")
	}
	for _, want := range []string{
		`"estimate_version": "v1"`, `"reserve_tokens": 1024`,
		`"generated_at": "2026-10-16T12:00:00Z"`, `"cases_dir": "evals/cases"`,
	} {
		require.Contains(t, text, want, "结果文件缺字段 %s", want)
	}
}

// TestWriteReport_KeepsRunIDAndAuditLinkage 中文验收点：每例带运行标识与审计关联
// （tool_call_id + 状态 + 失败分类）与 token 用量——据此可回到审计行，结果文件不是第二套记账。
func TestWriteReport_KeepsRunIDAndAuditLinkage(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 16, 12, 0, 1, 0, time.UTC)
	rpt := &report{
		Cases: []caseResult{{
			ID: "inject", Kind: kindInjection, Backend: "fake", RunID: "cli-2-def456",
			SessionID: sessionID("inject"), Status: string(core.StatusCompleted),
			ToolSeq: []string{"read_file"}, LLMCalls: 2,
			Usage:         usageReading{Prompt: 10, Completion: 4, Total: 14},
			UsageComplete: true,
			Audit: []auditReading{{
				ToolCallID: "call-1", Found: true, Status: "failed", ErrorKind: "sandbox_denied",
			}},
			Checks: []checkResult{{Name: "status", OK: true}}, Passed: true,
		}},
	}

	path, err := writeReport(dir, rpt, at)
	require.NoError(t, err)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var decoded struct {
		Cases []struct {
			RunID         string `json:"run_id"`
			SessionID     string `json:"session_id"`
			UsageComplete bool   `json:"usage_complete"`
			Usage         struct {
				Prompt int `json:"prompt"`
				Total  int `json:"total"`
			} `json:"usage"`
			Audit []struct {
				ToolCallID string `json:"tool_call_id"`
				Status     string `json:"status"`
				ErrorKind  string `json:"error_kind"`
			} `json:"audit"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Len(t, decoded.Cases, 1)
	require.Equal(t, "cli-2-def456", decoded.Cases[0].RunID)
	require.Equal(t, "eval-inject", decoded.Cases[0].SessionID)
	require.Equal(t, 14, decoded.Cases[0].Usage.Total)
	require.True(t, decoded.Cases[0].UsageComplete)
	require.Equal(t, "sandbox_denied", decoded.Cases[0].Audit[0].ErrorKind)
}

// TestWriteReport_SecondRunInSameSecondKeepsBoth 中文验收点：同一秒连跑两次 → 两份文件都在
// （"连跑两次读数一致"靠两份文件对比）。
func TestWriteReport_SecondRunInSameSecondKeepsBoth(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 10, 16, 12, 0, 2, 0, time.UTC)

	first, err := writeReport(dir, &report{Cases: []caseResult{{ID: "a"}}}, at)
	require.NoError(t, err)
	second, err := writeReport(dir, &report{Cases: []caseResult{{ID: "b"}}}, at)
	require.NoError(t, err)

	require.NotEqual(t, first, second)
	require.FileExists(t, first)
	require.FileExists(t, second)
}

// ---------------------------------------------------------------------------
// 汇总渲染
// ---------------------------------------------------------------------------

// TestKindsLine_FixedOrderAndCounts 中文验收点：四类读数按固定次序渲染、缺项计 0
// （不得遍历映射表——顺序必须是确定的）。
func TestKindsLine_FixedOrderAndCounts(t *testing.T) {
	line := kindsLine(map[string]int{kindToolSelection: 2, kindInjection: 3})
	require.Equal(t, "工具选择 2 / 业务结果 0 / 越权尝试 3 / 资源用量 0", line)
	require.Equal(t, "工具选择 0 / 业务结果 0 / 越权尝试 0 / 资源用量 0", kindsLine(nil))
}

// TestSummaryCounters_SeparateSkippedFromPassed 中文验收点：跳过**单独计数**，不与通过混算；
// 计量不完整的用例被点名（读数是"不完整"时报告要看得见，而不是让读者自己发现零）。
func TestSummaryCounters_SeparateSkippedFromPassed(t *testing.T) {
	cases := []caseResult{
		{ID: "a", Passed: true, UsageComplete: true},
		{ID: "b", Passed: false, UsageComplete: false},
		{ID: "c", Skipped: true, SkipReason: "未门控"},
	}
	require.Equal(t, 1, countIncomplete(cases), "跳过的不算不完整——它根本没有读数")
	require.Equal(t, []string{"b"}, incompleteIDs(cases))
}

// TestCompletenessText 中文验收点：完整性渲染为中文两态（命令行与结果文件同一套词）。
func TestCompletenessText(t *testing.T) {
	require.Equal(t, "完整", completenessText(true))
	require.Equal(t, "不完整", completenessText(false))
}

// TestToolSeqText_EmptySequenceIsVisible 中文验收点：空序列渲染成 `[]`——"一次都没调"是要
// 能一眼看出的读数。
func TestToolSeqText_EmptySequenceIsVisible(t *testing.T) {
	require.Equal(t, "[]", toolSeqText(nil))
	require.Equal(t, "[read_file]", toolSeqText([]string{"read_file"}))
	require.Equal(t, "[read_file list_dir]", toolSeqText([]string{"read_file", "list_dir"}))
}

// TestPreview_CapsObservedText 中文验收点：Detail 里的观测文本**截断**后回显（结果文件不
// 该成为第二个文件副本）；短文本原样保留，且截断语**不是**核心的截断标记字面量。
func TestPreview_CapsObservedText(t *testing.T) {
	short := "短文本"
	require.Equal(t, short, preview(short))

	got := preview(strings.Repeat("长", 500))
	require.Less(t, len([]rune(got)), 500)
	require.Contains(t, got, "未回显")
	require.NotContains(t, got, "已截断：输出超过", "不得复用工具输出截断的固定标记")
}

// TestWriteCaseLines_ThreeForms 中文验收点：逐例收场行三态分明——PASS 带读数、FAIL 逐条点名
// 未过的断言、SKIP 带跳过原因（命令行上就能看出哪条断言没过）。
func TestWriteCaseLines_ThreeForms(t *testing.T) {
	rpt := &report{Cases: []caseResult{
		{
			ID: "ok", Kind: kindToolSelection, RunID: "cli-1-a", Status: "completed",
			ToolSeq: []string{"read_file"}, LLMCalls: 2,
			Usage: usageReading{Prompt: 1, Completion: 2, Total: 3}, UsageComplete: true, Passed: true,
		},
		{
			ID: "bad", Kind: kindOutcome, RunID: "cli-1-b", Status: "limit_reached",
			Error: "运行入口拒绝执行：同会话忙",
			Checks: []checkResult{
				{Name: "status", OK: false, Detail: "期望状态 completed，实际 limit_reached"},
				{Name: "audit:call-1", OK: true},
			},
		},
		{ID: "skip", Kind: kindUsage, Skipped: true, SkipReason: "未门控"},
	}}

	var out strings.Builder
	writeCaseLines(&out, rpt)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")

	require.Len(t, lines, 5, "三例各一行，FAIL 多两行（运行错误 + 未过的断言）")
	require.Contains(t, lines[0], "[PASS] ok")
	require.Contains(t, lines[0], "计量=完整")
	require.Contains(t, lines[1], "[FAIL] bad")
	require.Contains(t, lines[2], "运行错误：运行入口拒绝执行")
	require.Contains(t, lines[3], "断言未过 status")
	require.Contains(t, lines[4], "[SKIP] skip")
	require.Contains(t, lines[4], "未门控")
	require.NotContains(t, out.String(), "audit:call-1", "通过的断言不上报")
}

// ---------------------------------------------------------------------------
// 测试夹具与桩
// ---------------------------------------------------------------------------

// fakeCase 返回一条基准 fake 档用例（各用例按其需要改写字段）。
// fakeCase returns a baseline fake-backend case.
func fakeCase(id string) evalCase {
	return evalCase{
		ID:          id,
		Kind:        kindToolSelection,
		Description: "基准用例",
		Message:     "读一下 notes.txt 并总结",
		Fixture: fixture{
			Agent:   id,
			AgentMD: agentMD(id),
			Files:   map[string]string{"notes.txt": "工作区笔记正文\n"},
		},
		Budget: budgetSpec{MaxIterations: 3},
		Script: []scriptReply{
			{Content: "先看看文件", ToolCalls: []toolCallSpec{
				{ID: "call-1", Name: "read_file", Arguments: `{"path":"notes.txt"}`},
			}, Usage: usageSpec{Prompt: 100, Completion: 20}},
			{Content: "按文件内容总结：三条要点。", Usage: usageSpec{Prompt: 80, Completion: 22}},
		},
		Expect: expectations{
			Status:       string(core.StatusCompleted),
			TextContains: []string{"总结"},
			Audit:        []auditSpec{{ToolCallID: "call-1", Status: "success"}},
		},
	}
}

// agentMD 返回夹具 Agent 的 AGENT.md：身份正文 + 探针 provider 声明（与探针配置条目同名，
// 路由因此仍是一次真实查表）。名字用常量拼——两处各写一遍字面量迟早会漂。
// agentMD builds the fixture AGENT.md declaring the probe provider by constant.
func agentMD(id string) string {
	return "---\nname: " + id + "\nprovider: {name: " + probeProviderName + ", model: " +
		probeDefaultModel + "}\ntools: [read_file]\n---\n\n身份：评测夹具助手。\n"
}

// fakeCaseYAML 以 YAML 文本形态给出基准用例：按**块**拼装，测试要改哪一段就替换或删掉
// 哪一段（`delete` 即整块不出现）——行内文本替换会把 block scalar 的正文行留在原地，
// 让解析错误落到别的地方去。
// fakeCaseYAML assembles the case text block by block so a mutation replaces or drops a
// whole key; mid-line replacement would leave block-scalar bodies dangling.
func fakeCaseYAML(id string, mutate func(blocks map[string]string)) string {
	blocks := map[string]string{
		"id":          "id: " + id + "\n",
		"kind":        "kind: " + kindToolSelection + "\n",
		"provider":    "",
		"description": "description: 基准用例\n",
		"message":     "message: 读一下 notes.txt\n",
		"fixture": "fixture:\n" +
			"  agent: " + id + "\n" +
			"  agent_md: |\n" +
			"    ---\n" +
			"    name: " + id + "\n" +
			"    provider: {name: " + probeProviderName + ", model: " + probeDefaultModel + "}\n" +
			"    tools: [read_file]\n" +
			"    ---\n" +
			"\n" +
			"    身份：评测夹具助手。\n" +
			"  files:\n" +
			"    notes.txt: |\n" +
			"      工作区笔记正文\n",
		"budget": "budget:\n  max_iterations: 3\n",
		"script": "script:\n" +
			"  - content: 先看看文件\n" +
			"    tool_calls:\n" +
			"      - id: call-1\n" +
			"        name: read_file\n" +
			"        arguments: '{\"path\":\"notes.txt\"}'\n" +
			"    usage: {prompt: 100, completion: 20}\n" +
			"  - content: 按文件内容总结：三条要点。\n" +
			"    usage: {prompt: 80, completion: 22}\n",
		"expect": expectBlock("completed", "call-1", "success"),
	}
	if mutate != nil {
		mutate(blocks)
	}

	var b strings.Builder
	for _, key := range []string{
		"id", "kind", "provider", "description", "message", "fixture", "budget", "script", "expect",
	} {
		b.WriteString(blocks[key])
	}
	return b.String()
}

// expectBlock 拼一段 expect 块（终止状态与审计断言两项可改——这两处的字面量正是装载期判的）。
func expectBlock(status, toolCallID, auditStatus string) string {
	return "expect:\n" +
		"  status: " + status + "\n" +
		"  text_contains: [总结]\n" +
		"  audit:\n" +
		"    - tool_call_id: " + toolCallID + "\n" +
		"      status: " + auditStatus + "\n"
}

// writeCaseFile 落一条用例文件。
func writeCaseFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

// caseIDs 取用例 id 序列（用于判装载序）。
func caseIDs(cases []evalCase) []string {
	ids := make([]string, 0, len(cases))
	for _, ec := range cases {
		ids = append(ids, ec.ID)
	}
	return ids
}

// ptr 取一个指针（断言的"没写"与"写了零值"是两回事）。
func ptr[T any](v T) *T { return &v }

// testLogger 返回一个丢弃输出的日志器（用例判定不该往终端写东西）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubSessions 是 cli.SessionLister 的桩：Messages 记下被问的会话标识（判读数取对了会话）。
type stubSessions struct {
	msgs   []core.Message
	err    error
	lastID string
}

var _ cli.SessionLister = (*stubSessions)(nil)

func (s *stubSessions) ListSessions(context.Context) ([]core.SessionRecord, error) {
	return nil, errors.New("评测读数不使用会话清单")
}

func (s *stubSessions) Messages(_ context.Context, sessionID string) ([]core.Message, error) {
	s.lastID = sessionID
	if s.err != nil {
		return nil, s.err
	}
	return s.msgs, nil
}

// stubInvocations 是 core.ToolInvocationReader 的桩。
type stubInvocations struct {
	rows      map[string]core.ToolInvocationView
	err       error
	lastRunID string
}

var _ core.ToolInvocationReader = (*stubInvocations)(nil)

func (s *stubInvocations) Lookup(_ context.Context, runID, toolCallID string) (core.ToolInvocationView, bool, error) {
	s.lastRunID = runID
	if s.err != nil {
		return core.ToolInvocationView{}, false, s.err
	}
	view, ok := s.rows[toolCallID]
	return view, ok, nil
}

// stubCalls 是 core.LLMCallReader 的桩。
type stubCalls struct {
	views     []core.LLMCallView
	err       error
	lastRunID string
}

var _ core.LLMCallReader = (*stubCalls)(nil)

func (s *stubCalls) CallsByRun(_ context.Context, runID string) ([]core.LLMCallView, error) {
	s.lastRunID = runID
	if s.err != nil {
		return nil, s.err
	}
	return slices.Clone(s.views), nil
}
