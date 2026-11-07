package memory

// US-22 受控自我进化：抽取输出的严格解析与逐项校验（颗粒度文档 §4.2 FR-22-05 具名用例，
// 以及 C-10 / C-11 / C-25 的边界）。用例名保持英文（TestXxx_yyy），文档原文以注释保留：
//
//	Parse_StrictRejectsUnknownAndTrailing —— 未知键 / 尾随内容 → 整次失败
//	Parse_RejectsOversize                 —— 超 64 KiB → 整次失败
//	Parse_RejectsMalformedUTF8            —— 非法 UTF-8 → 整次失败
//	Validate_*                            —— 单项丢弃 / 降级，其余条目照常保留
//	Time_*                                —— 绝对时刻、未知不猜、valid_to 必须未来、
//	                                         evidence_at 是记录时刻
//
// The test names are the English equivalents of the card's named cases.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// learningTestNow 是全部时间用例的固定"当前时刻"。
var learningTestNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// learningWindowFixture 构造一个冻结窗口：只有 10–12 三条同来源消息；8 是前文（永不作为证据）。
// learningWindowFixture builds the frozen window used by every validation case; message 8
// lives in the prior context and must never be accepted as evidence.
func learningWindowFixture() core.LearningWindow {
	base := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	return core.LearningWindow{
		FromMessageID: 10,
		ToMessageID:   12,
		New: []core.LearningMessage{
			{MessageID: 10, RunID: "run_1", Role: "user", Content: "我习惯用中文写日报，项目代号是玄武。", CreatedAt: base},
			{MessageID: 11, RunID: "run_1", Role: "assistant", Content: "好的，我会默认用中文写日报。", CreatedAt: base.Add(time.Minute)},
			{MessageID: 12, RunID: "run_1", Role: "user", Content: "临时约束：本周五前只用灰度发布。", CreatedAt: base.Add(2 * time.Minute)},
		},
		Prior: []core.LearningMessage{
			{MessageID: 8, RunID: "run_0", Role: "user", Content: "更早的消息，不在窗口内。", CreatedAt: base.Add(-time.Hour)},
		},
	}
}

// learningEntriesFixture 构造既有条目快照：41 / 42 在归档区（候选 / 生效），43 在核心区。
func learningEntriesFixture() []LearningEntrySnapshot {
	return []LearningEntrySnapshot{
		{EntryID: 41, Section: core.SectionArchive, State: "candidate", Content: "项目代号是朱雀"},
		{EntryID: 42, Section: core.SectionArchive, State: "active", Content: "日报模版固定为三段"},
		{EntryID: 43, Section: core.SectionCore, State: "active", Content: "项目代号是玄武"},
	}
}

// evidenceFor 构造一条证据引用。
func evidenceFor(messageID int64, role, quote string) []extractEvidence {
	return []extractEvidence{{MessageID: messageID, Role: role, Quote: quote}}
}

// factItem 构造一条以窗口消息 10 为用户证据的普通事实条目。
func factItem(content string) extractItem {
	return extractItem{Kind: "fact", Content: content, Evidence: evidenceFor(10, "user", "项目代号是玄武")}
}

// temporaryItem 构造一条以窗口消息 12 为用户证据的临时约束条目。
func temporaryItem(validTo string) extractItem {
	return extractItem{
		Kind: "temporary", Content: "本周五前只用灰度发布", ValidTo: validTo,
		Evidence: evidenceFor(12, "user", "本周五前只用灰度发布"),
	}
}

// validateWith 用固定窗口与快照执行逐项校验。
func validateWith(items []extractItem) ([]validatedItem, int) {
	return validateExtractItems(extractOutput{Items: items}, learningWindowFixture(),
		learningEntriesFixture(), learningTestNow, defaultExtractRules())
}

// TestUS22_Parse_StrictRejectsUnknownAndTrailing 守「严格 JSON：无未知键、无尾随内容」。
// Unknown keys (including state/source fields a model must not control) and any trailing
// content fail the whole extraction.
func TestUS22_Parse_StrictRejectsUnknownAndTrailing(t *testing.T) {
	t.Parallel()
	rules := defaultExtractRules()
	t.Run("empty-items-valid", func(t *testing.T) {
		out, err := parseExtractOutput(`{"items":[]}`, rules)
		require.NoError(t, err)
		require.Empty(t, out.Items)
	})
	t.Run("unknown-key", func(t *testing.T) {
		_, err := parseExtractOutput(`{"items":[{"kind":"fact","content":"x","state":"active"}]}`, rules)
		require.ErrorIs(t, err, errExtractStrictJSON)
	})
	t.Run("top-level-array", func(t *testing.T) {
		_, err := parseExtractOutput(`[]`, rules)
		require.ErrorIs(t, err, errExtractNotJSONObject)
	})
	t.Run("trailing-object", func(t *testing.T) {
		_, err := parseExtractOutput(`{"items":[]}{"items":[]}`, rules)
		require.ErrorIs(t, err, errExtractStrictJSON)
	})
	t.Run("trailing-junk", func(t *testing.T) {
		_, err := parseExtractOutput(`{"items":[]} trailing`, rules)
		require.ErrorIs(t, err, errExtractStrictJSON)
	})
}

// TestUS22_Parse_RejectsOversize 守 64 KiB 上限：超出即整次失败，上限之内照常解析。
// The 64 KiB cap rejects the whole output; anything within it still parses.
func TestUS22_Parse_RejectsOversize(t *testing.T) {
	t.Parallel()
	rules := defaultExtractRules()
	huge := `{"items":[{"kind":"fact","content":"` + strings.Repeat("a", rules.MaxOutputBytes) + `"}]}`
	_, err := parseExtractOutput(huge, rules)
	require.ErrorIs(t, err, errExtractOversize)

	within := `{"items":[{"kind":"fact","content":"` + strings.Repeat("a", 1024) + `"}]}`
	require.LessOrEqual(t, len(within), rules.MaxOutputBytes)
	_, err = parseExtractOutput(within, rules)
	require.NoError(t, err)
}

// TestUS22_Parse_RejectsMalformedUTF8 守「输出必须是合法 UTF-8」。
// Invalid UTF-8 is rejected before any field is trusted.
func TestUS22_Parse_RejectsMalformedUTF8(t *testing.T) {
	t.Parallel()
	raw := `{"items":[{"kind":"fact","content":"` + string([]byte{0xff, 0xfe}) + `"}]}`
	_, err := parseExtractOutput(raw, defaultExtractRules())
	require.ErrorIs(t, err, errExtractMalformedUTF8)
}

// TestUS22_Parse_RejectsTooManyItems 守 MaxItems=5：条数超限整次拒绝（严格 schema）。
// More than five items fails the whole extraction instead of being truncated.
func TestUS22_Parse_RejectsTooManyItems(t *testing.T) {
	t.Parallel()
	items := make([]string, 6)
	for i := range items {
		items[i] = `{"kind":"fact","content":"条目"}`
	}
	raw := `{"items":[` + strings.Join(items, ",") + `]}`
	_, err := parseExtractOutput(raw, defaultExtractRules())
	require.ErrorIs(t, err, errExtractTooManyItems)
}

// TestUS22_Validate_EvidenceWindowOnly 守 C-10：证据只能是冻结窗口内的消息；前文消息与
// 窗口外（含跨来源）的 message_id 一律丢弃该条。
// Evidence must belong to the frozen window; prior-context and out-of-window ids drop the
// item (cross-origin messages are absent from the window by construction).
func TestUS22_Validate_EvidenceWindowOnly(t *testing.T) {
	t.Parallel()
	t.Run("prior-message-not-evidence", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.Evidence = evidenceFor(8, "user", "更早的消息")
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("cross-origin-not-in-window", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.Evidence = evidenceFor(999, "user", "别的来源的消息")
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("window-message-accepted", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("项目代号是玄武")})
		require.Zero(t, dropped)
		require.Len(t, accepted, 1)
		require.Equal(t, int64(10), accepted[0].Evidence[0].MessageID)
		require.Equal(t, "run_1", accepted[0].Evidence[0].RunID)
	})
	t.Run("role-mismatch-dropped", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.Evidence = evidenceFor(10, "assistant", "项目代号是玄武")
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
}

// TestUS22_Validate_QuoteMustBeVerbatim 守 C-10：引文必须是该消息正文的逐字子串，
// 空引文不算证据。
// Quotes must be a verbatim, non-empty substring of the cited message.
func TestUS22_Validate_QuoteMustBeVerbatim(t *testing.T) {
	t.Parallel()
	t.Run("rewritten-quote-dropped", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.Evidence = evidenceFor(10, "user", "我喜欢用日语写日报")
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("empty-quote-dropped", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.Evidence = evidenceFor(10, "user", "")
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("exact-substring-accepted", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.Evidence = evidenceFor(10, "user", "习惯用中文写日报")
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Len(t, accepted, 1)
		require.Equal(t, "习惯用中文写日报", accepted[0].Evidence[0].Quote)
	})
}

// TestUS22_Validate_NoUserEvidenceDowngrades 守 C-10 的降级：fact / preference /
// constraint / temporary 没有用户证据时降级为 inference，确定性随之变为 uncertain。
// Missing user evidence downgrades the item to inference instead of dropping it.
func TestUS22_Validate_NoUserEvidenceDowngrades(t *testing.T) {
	t.Parallel()
	t.Run("assistant-only-downgrades", func(t *testing.T) {
		item := extractItem{
			Kind: "fact", Content: "日报默认用中文",
			Evidence: evidenceFor(11, "assistant", "默认用中文写日报"),
		}
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Len(t, accepted, 1)
		require.Equal(t, core.LearningKindInference, accepted[0].Kind)
		require.Equal(t, certaintyUncertain, accepted[0].Certainty)
	})
	t.Run("user-evidence-asserted", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("项目代号是玄武")})
		require.Zero(t, dropped)
		require.Equal(t, core.LearningKindFact, accepted[0].Kind)
		require.Equal(t, certaintyAsserted, accepted[0].Certainty)
	})
	t.Run("inference-stays-uncertain", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.Kind = "inference"
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Equal(t, core.LearningKindInference, accepted[0].Kind)
		require.Equal(t, certaintyUncertain, accepted[0].Certainty)
	})
	t.Run("method-not-downgraded", func(t *testing.T) {
		item := extractItem{
			Kind: "method", Content: "日报整理流程", Name: "daily-report",
			Evidence: evidenceFor(11, "assistant", "默认用中文写日报"),
		}
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Equal(t, core.LearningKindMethod, accepted[0].Kind)
	})
}

// TestUS22_Validate_TemporaryNeedsValidTo 守 C-10：temporary 必须有未来绝对 valid_to，
// 缺失、过去或相对日期都丢弃。
// Temporary constraints require a future absolute valid_to; missing, past or relative
// values drop the item.
func TestUS22_Validate_TemporaryNeedsValidTo(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing":   "",
		"past":      "2026-11-01T00:00:00Z",
		"relative":  "下周五",
		"date-only": "2035-01-01",
	}
	for name, validTo := range cases {
		t.Run(name, func(t *testing.T) {
			accepted, dropped := validateWith([]extractItem{temporaryItem(validTo)})
			require.Empty(t, accepted)
			require.Equal(t, 1, dropped)
		})
	}
	t.Run("future-absolute-accepted", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{temporaryItem("2035-01-01T00:00:00Z")})
		require.Zero(t, dropped)
		require.Len(t, accepted, 1)
		require.Equal(t, timeConfidenceExact, accepted[0].TimeConfidence)
		require.Equal(t, time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC), accepted[0].ValidTo.UTC())
	})
}

// TestUS22_Validate_DedupeCandidateActive 守 C-11：与既有归档区 candidate / active 同文
// 丢弃；同一批内的重复也只保留第一条。核心区既有正文不参与归档候选去重（§2.3.4 的
// 「同 Agent + archive」范围——候选落点即归档区）。
// Duplicates of archive candidates/actives are dropped, including repeats inside one batch;
// core-section entries are outside the archive dedupe scope.
func TestUS22_Validate_DedupeCandidateActive(t *testing.T) {
	t.Parallel()
	t.Run("existing-candidate-dropped", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("项目代号是朱雀")})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("existing-active-dropped", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("日报模版固定为三段")})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("same-batch-repeat-dropped", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("灰度发布两周"), factItem("灰度发布两周")})
		require.Len(t, accepted, 1)
		require.Equal(t, 1, dropped)
	})
	t.Run("core-entry-not-in-archive-dedupe", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("项目代号是玄武")})
		require.Zero(t, dropped)
		require.Len(t, accepted, 1)
	})
}

// TestUS22_Validate_SourceIDsSubset 守 C-11：source_entry_ids 必须是快照子集，且不超过
// MaxSourceEntries=5；重复 ID 去重后保留。
// Source ids must be a snapshot subset and at most five; duplicates collapse.
func TestUS22_Validate_SourceIDsSubset(t *testing.T) {
	t.Parallel()
	t.Run("subset-accepted", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.SourceEntryIDs = []int64{41, 42}
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Equal(t, []int64{41, 42}, accepted[0].SourceEntryIDs)
	})
	t.Run("outside-snapshot-dropped", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.SourceEntryIDs = []int64{41, 999}
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("too-many-sources-dropped", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.SourceEntryIDs = []int64{41, 42, 43, 41, 42, 43}
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("duplicate-ids-collapse", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.SourceEntryIDs = []int64{41, 41, 42}
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Equal(t, []int64{41, 42}, accepted[0].SourceEntryIDs)
	})
}

// TestUS22_Validate_SuggestionSnapshotOnly 守 C-11：建议取代目标不在快照时只清空建议，
// 条目保留。
// A supersede suggestion outside the snapshot is cleared; the item is kept.
func TestUS22_Validate_SuggestionSnapshotOnly(t *testing.T) {
	t.Parallel()
	t.Run("snapshot-target-kept", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.SuggestedSupersedesID = 42
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Equal(t, int64(42), accepted[0].SuggestedSupersedesID)
	})
	t.Run("outside-target-cleared", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.SuggestedSupersedesID = 999
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Len(t, accepted, 1)
		require.Zero(t, accepted[0].SuggestedSupersedesID)
	})
}

// TestUS22_Validate_CredentialDropped 守 C-12：正文或证据引文命中凭证扫描即丢弃该条；
// ${ENV_VAR} 占位放行（与 US-21 共用同一份规则）。
// A credential hit in the content or in a quote drops the item; ${ENV_VAR} placeholders pass.
func TestUS22_Validate_CredentialDropped(t *testing.T) {
	t.Parallel()
	// 样本在运行时拼出：源码里不得出现完整凭据形态（TestCredentialLeak_GoldenFixtures 会扫本目录）。
	credential := "sk-" + strings.Repeat("a1B2", 6)
	t.Run("content-credential-dropped", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("凭据是 " + credential)})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("quote-credential-dropped", func(t *testing.T) {
		window := learningWindowFixture()
		window.New[0].Content = "记住 " + credential
		item := factItem("用户给过一个凭据")
		item.Evidence = evidenceFor(10, "user", credential)
		accepted, dropped := validateExtractItems(extractOutput{Items: []extractItem{item}},
			window, learningEntriesFixture(), learningTestNow, defaultExtractRules())
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("placeholder-accepted", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("凭据一律用 ${LLM_API_KEY} 占位")})
		require.Zero(t, dropped)
		require.Len(t, accepted, 1)
	})
}

// TestUS22_Validate_DropsInvalidItemKeepsOthers 守「丢弃单项不失败整次」。
// One invalid item never fails the batch; the others are kept.
func TestUS22_Validate_DropsInvalidItemKeepsOthers(t *testing.T) {
	t.Parallel()
	invalid := factItem("条目")
	invalid.Kind = "opinion"
	accepted, dropped := validateWith([]extractItem{factItem("灰度发布两周"), invalid, factItem("日报每周五发出")})
	require.Len(t, accepted, 2)
	require.Equal(t, 1, dropped)
	require.Equal(t, "灰度发布两周", accepted[0].Content)
	require.Equal(t, "日报每周五发出", accepted[1].Content)
}

// TestUS22_Validate_NoArbitraryTargets 守「模型输出不是权限边界」：模型不能自定状态 /
// 主键类字段（未知键 → 整次失败），也不能凭 ID 引用快照之外的条目。
// The model cannot dictate state or ids: such keys fail the parse, and out-of-snapshot ids
// are dropped or cleared.
func TestUS22_Validate_NoArbitraryTargets(t *testing.T) {
	t.Parallel()
	rules := defaultExtractRules()
	for name, raw := range map[string]string{
		"state-field":    `{"items":[{"kind":"fact","content":"x","state":"active"}]}`,
		"entry-id-field": `{"items":[{"kind":"fact","content":"x","entry_id":42}]}`,
		"source-field":   `{"items":[{"kind":"fact","content":"x","source":"explicit_propose"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseExtractOutput(raw, rules)
			require.ErrorIs(t, err, errExtractStrictJSON)
		})
	}
	t.Run("out-of-snapshot-ids-neutralised", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.SourceEntryIDs = []int64{999}
		item.SuggestedSupersedesID = 998
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted, "快照之外的来源 ID 丢弃该条")
		require.Equal(t, 1, dropped)
	})
}

// TestUS22_Time_AbsoluteOnly 守 C-25：valid_to 只接受 RFC3339 绝对时刻，日期串与
// 本地化格式都不解析。
// valid_to accepts RFC3339 only; date-only and localized strings are not parsed.
func TestUS22_Time_AbsoluteOnly(t *testing.T) {
	t.Parallel()
	accepted, dropped := validateWith([]extractItem{temporaryItem("2035-01-01T08:30:00+08:00")})
	require.Zero(t, dropped)
	require.Equal(t, timeConfidenceExact, accepted[0].TimeConfidence)
	require.True(t, accepted[0].ValidTo.Equal(time.Date(2035, 1, 1, 0, 30, 0, 0, time.UTC)))
	for _, raw := range []string{"2035/01/01", "01/01/2035", "2035年1月1日"} {
		t.Run(raw, func(t *testing.T) {
			accepted, dropped := validateWith([]extractItem{temporaryItem(raw)})
			require.Empty(t, accepted)
			require.Equal(t, 1, dropped)
		})
	}
}

// TestUS22_Time_RelativeStaysUnknown 守 C-25：相对日期不猜；非 temporary 条目即便带了
// 时间串也不落 valid_to，time_confidence 记 unknown。
// Relative dates are never guessed; non-temporary items carry no expiry and stay unknown.
func TestUS22_Time_RelativeStaysUnknown(t *testing.T) {
	t.Parallel()
	t.Run("temporary-relative-dropped", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{temporaryItem("下周五")})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("fact-relative-stays-unknown", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.ValidTo = "下周五"
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Equal(t, timeConfidenceUnknown, accepted[0].TimeConfidence)
		require.True(t, accepted[0].ValidTo.IsZero())
	})
	t.Run("fact-absolute-no-expiry", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.ValidTo = "2035-01-01T00:00:00Z"
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		require.Equal(t, timeConfidenceUnknown, accepted[0].TimeConfidence)
		require.True(t, accepted[0].ValidTo.IsZero())
	})
}

// TestUS22_Time_ValidToMustBeFuture 守 C-25：valid_to 必须晚于当前时刻，等于当前也不行。
// valid_to must be strictly after now.
func TestUS22_Time_ValidToMustBeFuture(t *testing.T) {
	t.Parallel()
	for name, validTo := range map[string]string{
		"equal-now":      learningTestNow.Format(time.RFC3339),
		"one-second-ago": learningTestNow.Add(-time.Second).Format(time.RFC3339),
	} {
		t.Run(name, func(t *testing.T) {
			accepted, dropped := validateWith([]extractItem{temporaryItem(validTo)})
			require.Empty(t, accepted)
			require.Equal(t, 1, dropped)
		})
	}
	t.Run("one-second-after", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{temporaryItem(learningTestNow.Add(time.Second).Format(time.RFC3339))})
		require.Zero(t, dropped)
		require.Len(t, accepted, 1)
	})
}

// TestUS22_Time_EvidenceAtIsRecordTime 守 C-25：evidence_at 取证据消息的最大 created_at，
// 是记录时刻而非事实发生时刻，也绝不是"当前时刻"；记录时刻不可求证（≤0）时丢弃该条，
// 不写零值。
// evidence_at is the max record time of the cited messages — never "now" — and an
// unverifiable record time drops the item instead of writing zero.
func TestUS22_Time_EvidenceAtIsRecordTime(t *testing.T) {
	t.Parallel()
	t.Run("max-record-time", func(t *testing.T) {
		item := factItem("项目代号是玄武")
		item.Evidence = append(evidenceFor(10, "user", "项目代号是玄武"),
			extractEvidence{MessageID: 12, Role: "user", Quote: "本周五前只用灰度发布"})
		accepted, dropped := validateWith([]extractItem{item})
		require.Zero(t, dropped)
		want := learningWindowFixture().New[2].CreatedAt
		require.Equal(t, want.UnixMilli(), accepted[0].EvidenceAt.UnixMilli())
		require.NotEqual(t, learningTestNow.UnixMilli(), accepted[0].EvidenceAt.UnixMilli())
		require.Positive(t, accepted[0].EvidenceAt.UnixMilli())
	})
	t.Run("zero-record-time-dropped", func(t *testing.T) {
		window := learningWindowFixture()
		window.New[0].CreatedAt = time.Time{}
		accepted, dropped := validateExtractItems(extractOutput{Items: []extractItem{factItem("项目代号是玄武")}},
			window, learningEntriesFixture(), learningTestNow, defaultExtractRules())
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
}

// TestUS22_Validate_ContentBounds 守内容上限与名称规则：空正文、超 4 KiB 与非法草案名丢弃。
// Empty or oversized content and invalid draft names are dropped.
func TestUS22_Validate_ContentBounds(t *testing.T) {
	t.Parallel()
	t.Run("empty-content-dropped", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem("   ")})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("oversized-content-dropped", func(t *testing.T) {
		accepted, dropped := validateWith([]extractItem{factItem(strings.Repeat("长", 1366))}) // 4098 字节
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("method-bad-name-dropped", func(t *testing.T) {
		item := extractItem{
			Kind: "method", Content: "日报整理流程", Name: "../escape",
			Evidence: evidenceFor(11, "assistant", "默认用中文写日报"),
		}
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
	t.Run("method-name-required", func(t *testing.T) {
		item := extractItem{
			Kind: "method", Content: "日报整理流程",
			Evidence: evidenceFor(11, "assistant", "默认用中文写日报"),
		}
		accepted, dropped := validateWith([]extractItem{item})
		require.Empty(t, accepted)
		require.Equal(t, 1, dropped)
	})
}

// ---------------------------------------------------------------------------
// T011：LearningService.AfterRun 主流程（颗粒度文档 §2.3.1）。
// 服务无返回值，因此这些用例只断言"下游发生了什么"——写没写行、调没调模型、
// 提交了什么；Run 的终态不可能被它改变。
// T011 service-flow cases: the hook has no return value, so every assertion is about
// downstream effects only.
// ---------------------------------------------------------------------------

// learningFakeStore 是 LearningStore 的记账桩：每个方法记调用次数，提交请求原样留档。
// learningFakeStore records calls and keeps the last commit request.
type learningFakeStore struct {
	unresolved    *core.LearningExtraction
	unresolvedErr error
	cursor        core.LearningCursor
	cursorErr     error
	entries       []LearningEntrySnapshot
	entriesErr    error
	beginErr      error
	beginRow      core.LearningExtraction
	commitErr     error

	unresolvedCalls int
	cursorCalls     int
	entriesCalls    int
	beginCalls      int
	failCalls       int
	commitCalls     int

	entriesLimit int
	entriesBytes int

	failClass   core.LearningFailureClass
	failMessage string
	commitReq   core.CommitLearningExtraction
}

func (f *learningFakeStore) UnresolvedForOrigin(_ context.Context, _, _, _ string) (*core.LearningExtraction, error) {
	f.unresolvedCalls++
	return f.unresolved, f.unresolvedErr
}

func (f *learningFakeStore) LoadCursor(_ context.Context, _, _, _ string) (core.LearningCursor, bool, error) {
	f.cursorCalls++
	return f.cursor, f.cursor != (core.LearningCursor{}), f.cursorErr
}

func (f *learningFakeStore) EntriesForExtraction(_ context.Context, _ core.Scope, limit, bytes int) ([]LearningEntrySnapshot, error) {
	f.entriesCalls++
	f.entriesLimit, f.entriesBytes = limit, bytes
	return f.entries, f.entriesErr
}

func (f *learningFakeStore) BeginExtraction(_ context.Context, row core.LearningExtraction, _ core.LeaseToken) (core.LearningExtraction, error) {
	f.beginCalls++
	f.beginRow = row
	if f.beginErr != nil {
		return core.LearningExtraction{}, f.beginErr
	}
	row.Attempts++
	return row, nil
}

func (f *learningFakeStore) FailExtraction(_ context.Context, _, _ string, _ core.LeaseToken, _ string, _ int,
	class core.LearningFailureClass, message string, _ time.Time,
) error {
	f.failCalls++
	f.failClass = class
	f.failMessage = message
	return nil
}

func (f *learningFakeStore) CommitExtraction(_ context.Context, req core.CommitLearningExtraction, _ core.LeaseToken) (core.LearningCommitResult, error) {
	f.commitCalls++
	f.commitReq = req
	return core.LearningCommitResult{}, f.commitErr
}

// learningFakeMessages 是消息读面的记账桩；默认返回包内共用的冻结窗口夹具。
// 入参逐项留档：窗口 / 前文上限与游标左界是 T016 的判据面，不能只数调用次数。
// learningFakeMessages returns the shared window fixture and records every argument.
type learningFakeMessages struct {
	window          core.LearningWindow
	err             error
	extractionCalls int
	frozenCalls     int

	afterMessageID    int64
	priorLimit        int
	newLimit          int
	priorBytes        int
	newBytes          int
	extractionSession string
	frozenSession     string
	frozenFrom        int64
	frozenTo          int64
	frozenOrigin      [2]string // caller_id, key_id
}

func (f *learningFakeMessages) MessagesForExtraction(_ context.Context, sessionID, _ string, afterMessageID int64,
	priorLimit, newLimit, priorBytes, newBytes int,
) (core.LearningWindow, error) {
	f.extractionCalls++
	f.extractionSession = sessionID
	f.afterMessageID = afterMessageID
	f.priorLimit, f.newLimit = priorLimit, newLimit
	f.priorBytes, f.newBytes = priorBytes, newBytes
	return f.window, f.err
}

func (f *learningFakeMessages) MessagesForFrozenWindow(_ context.Context, sessionID, originCallerID, originKeyID string,
	fromMessageID, toMessageID int64, priorLimit, priorBytes int,
) (core.LearningWindow, error) {
	f.frozenCalls++
	f.frozenSession = sessionID
	f.frozenFrom, f.frozenTo = fromMessageID, toMessageID
	f.frozenOrigin = [2]string{originCallerID, originKeyID}
	f.priorLimit, f.priorBytes = priorLimit, priorBytes
	return f.window, f.err
}

// learningFakeExtractor 是 MemoryExtractor 的记账桩：记录每次调用与请求。
// learningFakeExtractor records every extraction call and its request.
type learningFakeExtractor struct {
	reply string
	err   error
	calls int
	reqs  []core.ChatRequest
}

func (f *learningFakeExtractor) ChatMemoryExtract(_ context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	f.calls++
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return core.ChatResponse{}, f.err
	}
	return core.ChatResponse{Content: f.reply}, nil
}

// learningTestRig 是装配好的服务与两个记账桩；extractor 随每次 Run 的输入传入。
// draftRoot 供草案用例直接核对落盘位置；清理时先恢复写权限（Windows 的 RemoveAll
// 不保证清除只读属性，T021 起草案目录本身是只读的）。
// learningTestRig bundles the service with its recording fakes; draftRoot exposes the
// on-disk draft directory, and cleanup restores write bits before TempDir removal.
type learningTestRig struct {
	service   *LearningService
	store     *learningFakeStore
	messages  *learningFakeMessages
	draftRoot string
}

func newLearningTestRig(t *testing.T, enabled bool, agents []string, logger *slog.Logger) *learningTestRig {
	t.Helper()
	store := &learningFakeStore{entries: learningEntriesFixture()}
	messages := &learningFakeMessages{window: learningWindowFixture()}
	draftRoot := filepath.Join(t.TempDir(), "drafts")
	// 注册在 t.TempDir 之后 → 先于 TempDir 清理执行（t.Cleanup 后进先出）。
	t.Cleanup(func() { us22MakeWritable(draftRoot) })
	service := NewLearningService(LearningDeps{
		Store:     store,
		Messages:  messages,
		Enabled:   enabled,
		Agents:    agents,
		DraftRoot: draftRoot,
		Logger:    logger,
		Now:       func() time.Time { return learningTestNow },
	})
	return &learningTestRig{service: service, store: store, messages: messages, draftRoot: draftRoot}
}

// learningRunInput 构造一次正常的 web 渠道完成 Run。
func learningRunInput(extractor core.MemoryExtractor) core.LearningRunInput {
	return core.LearningRunInput{
		RunID:       "run_1",
		Agent:       "demo",
		SessionID:   "sess_1",
		Channel:     "web",
		Provider:    "local",
		Model:       "model-a",
		ConfigHash:  "cfg-1",
		CompletedAt: learningTestNow,
		Extractor:   extractor,
	}
}

// TestUS22_Disabled_NoWritesNoCalls 守 FR-22-01：默认关闭时零写入、零模型调用；非
// completed 的 Run 同样不触发。判断发生在任何存储读取之前。
// Disabled (default) and non-completed runs touch neither the store nor the model.
func TestUS22_Disabled_NoWritesNoCalls(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]core.RunStatus{
		"completed-but-disabled": core.StatusCompleted,
		"failed-run":             core.StatusFailed,
	} {
		t.Run(name, func(t *testing.T) {
			rig := newLearningTestRig(t, false, []string{"demo"}, nil)
			extractor := &learningFakeExtractor{reply: `{"items":[]}`}
			rig.service.AfterRun(context.Background(), learningRunInput(extractor),
				core.RunResult{RunID: "run_1", Status: status})
			require.Zero(t, extractor.calls)
			require.Zero(t, rig.store.unresolvedCalls)
			require.Zero(t, rig.store.cursorCalls)
			require.Zero(t, rig.store.entriesCalls)
			require.Zero(t, rig.store.beginCalls)
			require.Zero(t, rig.store.commitCalls)
			require.Zero(t, rig.messages.extractionCalls)
			require.Zero(t, rig.messages.frozenCalls)
		})
	}
}

// TestUS22_AgentNotListed_Noop 守 FR-22-01：启用但 Agent 不在显式名单内时只留 Debug
// 日志，不读库、不调用。
// An agent outside the explicit list is a no-op with a debug log only.
func TestUS22_AgentNotListed_Noop(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"other"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	require.Zero(t, extractor.calls)
	require.Zero(t, rig.store.unresolvedCalls)
	require.Zero(t, rig.store.beginCalls)
	require.Zero(t, rig.messages.extractionCalls)
}

// TestUS22_DeterministicPromptVersion 守 FR-22-04 / C-15：提示词由程序序列化，同一输入
// 逐字节相同，版本常量固定；凭证命中的消息不进提示词（但仍留在冻结窗口与证据校验面）。
// The prompt is deterministic, versioned, and never carries credential-bearing messages.
func TestUS22_DeterministicPromptVersion(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	row := core.LearningExtraction{
		ExtractionID: "lrn_test", Agent: "demo", RunID: "run_1",
		WindowFromMessageID: 10, WindowToMessageID: 12,
	}
	window := learningWindowFixture()
	entries := learningEntriesFixture()
	first := rig.service.buildPrompt(row, window, entries)
	second := rig.service.buildPrompt(row, window, entries)
	require.Equal(t, first, second, "同一输入必须产生逐字节相同的提示词")
	require.Len(t, first, 2)
	require.Equal(t, "system", first[0].Role)
	require.Contains(t, first[0].Content, "只输出一个 JSON 对象")
	require.Equal(t, "user", first[1].Role)
	require.Contains(t, first[1].Content, `"prompt_version":"memory-extract-v1"`)
	require.Contains(t, first[1].Content, `"rule_version":"memory-rules-v1"`)
	require.Contains(t, first[1].Content, `"message_id":10`)
	require.Contains(t, first[1].Content, `"entry_id":41`)

	// 凭据形态在运行时拼出，源码不落完整样本（TestCredentialLeak_GoldenFixtures 扫描本目录）。
	credential := "sk-" + strings.Repeat("a1B2", 6)
	window.New[0].Content = "记住 " + credential
	withSecret := rig.service.buildPrompt(row, window, learningEntriesFixture())
	require.NotContains(t, withSecret[0].Content, credential)
	require.NotContains(t, withSecret[1].Content, credential)
	// 提示词侧剔除不影响证据校验面：消息仍在冻结窗口里。
	require.Len(t, window.New, 3)
}

// TestUS22_Prompt_EntriesCredentialFiltered 守 §3.1.13「提示词里不放凭证」：既有条目
// 快照与窗口消息同一口径，命中凭证的条目不序列化进提示词；条目仍留在快照里供
// source_entry_ids 子集判定——过滤只影响模型可见面，不改变校验面。
//
// 逆改点：删掉 entries 循环里的 TextHasCredential → 凭证被序列化进用户消息，断言红。
// Credential-bearing entries are filtered from the prompt exactly like messages are.
func TestUS22_Prompt_EntriesCredentialFiltered(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	row := core.LearningExtraction{ExtractionID: "lrn_test", Agent: "demo", RunID: "run_1"}

	// 凭据形态在运行时拼出，源码不落完整样本（TestCredentialLeak_GoldenFixtures 扫描本目录）。
	credential := "sk-" + strings.Repeat("a1B2", 6)
	entries := []LearningEntrySnapshot{
		{EntryID: 41, Section: core.SectionArchive, State: "candidate", Content: "项目代号是朱雀"},
		{EntryID: 44, Section: core.SectionArchive, State: "active", Content: "部署令牌 " + credential},
	}

	prompt := rig.service.buildPrompt(row, learningWindowFixture(), entries)
	require.Len(t, prompt, 2)
	require.NotContains(t, prompt[0].Content, credential, "系统提示词不得出现凭证")
	require.NotContains(t, prompt[1].Content, credential, "条目快照不得把凭证送进提示词")
	require.Contains(t, prompt[1].Content, `"entry_id":41`)
	require.NotContains(t, prompt[1].Content, `"entry_id":44`, "命中凭证的条目整条剔除")
	require.Len(t, entries, 2, "过滤只影响提示词，不改变快照本身")
}

// TestUS22_Candidate_InsertedAsCandidate 守 FR-22-06 / C-13：校验通过的普通条目以
// 归档区候选进入提交事务；状态由提交路径写死，模型与调用方都无从指定。
// Accepted non-method items go to the commit as archive candidates with program-fixed state.
func TestUS22_Candidate_InsertedAsCandidate(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[{"kind":"fact","content":"项目代号是玄武",` +
		`"evidence":[{"message_id":10,"role":"user","quote":"项目代号是玄武"}]}]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, extractor.calls)
	require.Equal(t, 1, rig.store.beginCalls)
	require.Equal(t, 3, rig.store.beginRow.WindowMessages, "首次尝试冻结整段窗口条数")
	require.Equal(t, 1, rig.store.commitCalls)
	require.Zero(t, rig.store.failCalls)

	req := rig.store.commitReq
	require.Len(t, req.Candidates, 1)
	candidate := req.Candidates[0]
	require.Equal(t, core.SectionArchive, candidate.Section)
	require.Equal(t, "项目代号是玄武", candidate.Content)
	require.Equal(t, core.LearningKindFact, candidate.Kind)
	require.Equal(t, certaintyAsserted, candidate.Certainty)
	require.Len(t, candidate.Evidence, 1)
	require.Equal(t, int64(10), candidate.Evidence[0].MessageID)
	require.Equal(t, "run_1", candidate.Evidence[0].RunID)
	require.Equal(t, 1, req.ItemsTotal)
	require.Equal(t, 1, req.ItemsAccepted)
	require.Zero(t, req.ItemsDropped)
	require.Equal(t, learningTestNow.UnixMilli(), req.CompletedAt.UnixMilli())

	// 抽取调用取本次触发 Run 冻结的模型身份与轮次 0。
	call := extractor.reqs[0]
	require.Equal(t, "run_1", call.RunID)
	require.Equal(t, "sess_1", call.SessionID)
	require.Equal(t, 0, call.Round)
	require.Equal(t, "local", call.Provider)
	require.Equal(t, "model-a", call.Model)
	require.Equal(t, "cfg-1", call.ConfigHash)
	require.False(t, call.HookDeadline.IsZero())
	require.Len(t, call.Messages, 2)

	// 候选结构里没有 State 字段：状态只由提交事务写死为 candidate。
	_, hasState := reflect.TypeOf(core.LearningCandidate{}).FieldByName("State")
	require.False(t, hasState)
}

// TestUS22_Source_IsAutoExtract 守 C-14：自动抽取是闭集来源字面量，且候选结构不暴露
// 来源字段——写入只能由受信任的提交路径完成，调用方无法伪装成其它来源。
// The source literal is fixed and not settable through the candidate struct.
func TestUS22_Source_IsAutoExtract(t *testing.T) {
	t.Parallel()
	require.Equal(t, "auto_extract", SourceAutoExtract)
	_, hasSource := reflect.TypeOf(core.LearningCandidate{}).FieldByName("Source")
	require.False(t, hasSource)

	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[{"kind":"preference","content":"日报用中文写",` +
		`"evidence":[{"message_id":10,"role":"user","quote":"我习惯用中文写日报"}]}]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	require.Equal(t, 1, rig.store.commitCalls)
	require.Len(t, rig.store.commitReq.Candidates, 1)
	require.Equal(t, core.LearningKindPreference, rig.store.commitReq.Candidates[0].Kind)
}

// TestUS22_Commit_ZeroCandidatesAdvances 守 C-17 / 失败矩阵：模型正常返回但没有合法
// 候选时仍走提交（success + 游标推进），不得记失败、不得回滚。
// A well-formed empty result still commits (success) so the cursor advances.
func TestUS22_Commit_ZeroCandidatesAdvances(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	require.Equal(t, 1, rig.store.commitCalls)
	require.Zero(t, rig.store.failCalls)
	req := rig.store.commitReq
	require.Empty(t, req.Candidates)
	require.Empty(t, req.Drafts)
	require.Zero(t, req.ItemsTotal)
	require.Zero(t, req.ItemsAccepted)
	require.Zero(t, req.ItemsDropped)
}

// TestUS22_Scope_AgentOnly 守 FR-22-06：候选范围只写 Agent 层，绝不落全局层。
// Candidates are agent-scoped only; the global layer is never written.
func TestUS22_Scope_AgentOnly(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[{"kind":"fact","content":"项目代号是玄武",` +
		`"evidence":[{"message_id":10,"role":"user","quote":"项目代号是玄武"}]}]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	require.Len(t, rig.store.commitReq.Candidates, 1)
	require.Equal(t, core.Scope{Agent: "demo"}, rig.store.commitReq.Candidates[0].Scope)
	require.NotEqual(t, core.GlobalScope, rig.store.commitReq.Candidates[0].Scope)
}

// TestUS22_Scope_NoGlobalWrite 守 FR-22-08：method 条目只进草案，不写记忆条目；
// 提交里的每个候选都落在 Agent 层，全局层零写入。
// Method items become drafts only; no candidate ever targets the global layer.
func TestUS22_Scope_NoGlobalWrite(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[` +
		`{"kind":"method","name":"daily-report","content":"适用条件：写日报\n步骤：先列事实\n反例：不要编造",` +
		`"evidence":[{"message_id":11,"role":"assistant","quote":"默认用中文写日报"}]}]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	require.Equal(t, 1, rig.store.commitCalls)
	req := rig.store.commitReq
	require.Empty(t, req.Candidates, "method 不写记忆条目")
	require.Len(t, req.Drafts, 1)
	require.Equal(t, "demo", req.Drafts[0].Agent)
	for _, candidate := range req.Candidates {
		require.NotEqual(t, core.GlobalScope, candidate.Scope)
	}
}

// TestUS22_Logs_NoContentOrSecret 守 C-30：成功与失败日志只含身份与计数，不含候选
// 正文、证据引文或凭证。
// Logs carry ids and counts only — never content, quotes or credentials.
func TestUS22_Logs_NoContentOrSecret(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	rig := newLearningTestRig(t, true, []string{"demo"}, logger)

	extractor := &learningFakeExtractor{reply: `{"items":[{"kind":"fact","content":"秘密周报格式",` +
		`"evidence":[{"message_id":10,"role":"user","quote":"项目代号是玄武"}]}]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	logs := buf.String()
	require.Contains(t, logs, "学习抽取完成")
	require.NotContains(t, logs, "秘密周报格式")
	require.NotContains(t, logs, "项目代号是玄武")

	credential := "sk-" + strings.Repeat("a1B2", 6)
	failing := &learningFakeExtractor{err: fmt.Errorf("provider rejected token %s", credential)}
	rig.service.AfterRun(context.Background(), learningRunInput(failing),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	logs = buf.String()
	require.Contains(t, logs, "学习抽取失败")
	require.Contains(t, logs, "failure_class")
	require.NotContains(t, logs, credential)
}

// TestUS22_Errors_MessageSanitized 守 C-30：失败摘要 ≤1 KiB，凭证命中整体抑制；
// 截断按 UTF-8 边界，不写半截字符。
// The failure summary is ≤1 KiB, suppresses credentials, and truncates on a UTF-8 boundary.
func TestUS22_Errors_MessageSanitized(t *testing.T) {
	t.Parallel()
	t.Run("credential-suppressed", func(t *testing.T) {
		rig := newLearningTestRig(t, true, []string{"demo"}, nil)
		credential := "sk-" + strings.Repeat("a1B2", 6)
		extractor := &learningFakeExtractor{err: fmt.Errorf("provider rejected token %s", credential)}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
		require.Equal(t, 1, rig.store.failCalls)
		require.Equal(t, core.LearningFailureModel, rig.store.failClass)
		require.Contains(t, rig.store.failMessage, "details suppressed")
		require.NotContains(t, rig.store.failMessage, credential)
	})
	t.Run("oversized-truncated", func(t *testing.T) {
		rig := newLearningTestRig(t, true, []string{"demo"}, nil)
		extractor := &learningFakeExtractor{err: errors.New(strings.Repeat("错", 2000))}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
		require.Equal(t, 1, rig.store.failCalls)
		require.LessOrEqual(t, len(rig.store.failMessage), 1024)
		require.True(t, utf8.ValidString(rig.store.failMessage))
	})
}
