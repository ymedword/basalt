package memory

// 本文件承载 US-22 抽取候选的**逐项程序校验**（颗粒度文档 §2.3.4 / §3.1.5，FR-22-05 /
// C-10 / C-11 / C-25）：模型输出只在这里被赋予可信属性——类型闭集、内容上限、证据在冻结
// 窗口内且逐字、角色、临时约束的未来绝对 `valid_to`、去重、来源子集、建议目标；凭证命中
// 即丢弃该条；`certainty` 与 `evidence_at` 由程序派生，模型自报的 `confidence` 永不作为
// 状态、召回或确认的门槛。单项不合格只丢该条（计入 dropped），不失败整次——整次失败只发生
// 在解析层（extract_output.go）。
//
// 凭证规则本体只有一份：`core.TextHasSecret`（US-21 技能准入的扫描器下沉到 `internal/core`
// 之后由两侧共用，C-12）；本包**不得**复制模式表。`internal/app` 已依赖 `internal/memory`，
// 反向 import app 会成环；共用因此必须走 core。
//
// Per-item validation of extractor output. A bad item is dropped, never fatal; certainty
// and evidence_at are program-derived, and the model's confidence is never a gate. The
// credential rule set lives once, in core (shared with US-21 admission).

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// LearningEntrySnapshot 是抽取提示词与逐项校验共用的既有条目快照（候选 + 有效）。只暴露
// 展示所需字段；状态过滤由端口实现完成。来源子集与建议目标都只能落在快照内——快照之外的
// ID 一律丢弃或清空，模型不得凭空引用既有记忆。
// LearningEntrySnapshot is the snapshot of existing entries shared by the prompt and the
// validator; ids outside it are never trusted as sources or supersede targets.
type LearningEntrySnapshot struct {
	EntryID int64
	Section core.Section
	State   string
	Content string
}

// validatedItem 是校验通过的一条条目（尚未落库）。method 只写只读草案，其余写候选。
// validatedItem is one accepted item; method items become drafts, the rest candidates.
type validatedItem struct {
	Kind                  core.LearningItemKind
	Content               string
	Name                  string
	Confidence            float64
	Evidence              []core.LearningEvidence
	SourceEntryIDs        []int64
	SuggestedSupersedesID int64
	// EvidenceAt 是证据消息的最大 created_at：**记录时刻，不是事实发生时刻**（C-25）。
	EvidenceAt     time.Time
	ValidTo        time.Time // 零值 = 无到期；只有 temporary 会携带
	TimeConfidence string    // exact / unknown（relative_unknown 只留在 DB 闭集里，不产生）
	Certainty      string    // asserted / uncertain（程序派生）
}

// validateExtractItems 逐项校验 out.Items，返回（合法条目，丢弃数）。丢弃数是本次调用的
// 局部值，调用方不得存进服务字段——同一服务实例会被多个 Agent / 会话的 Run 并发调用。
// validateExtractItems validates each item and returns (accepted, dropped). The dropped
// count is call-local and must never live on the service struct.
func validateExtractItems(out extractOutput, window core.LearningWindow, entries []LearningEntrySnapshot, now time.Time, rules extractRules) ([]validatedItem, int) {
	messages := make(map[int64]core.LearningMessage, len(window.New))
	for _, message := range window.New {
		messages[message.MessageID] = message
	}
	known := make(map[int64]struct{}, len(entries))
	// dedupe 同时承担「与既有归档区 candidate/active 同文丢弃」与「同一批内重复丢弃」两件事：
	// 快照里只收集归档区正文（§2.3.4「同 Agent + archive」），候选落点即归档区。
	dedupe := make(map[string]struct{}, len(entries)+len(out.Items))
	for _, entry := range entries {
		known[entry.EntryID] = struct{}{}
		if entry.Section == core.SectionArchive {
			dedupe[entry.Content] = struct{}{}
		}
	}
	accepted := make([]validatedItem, 0, len(out.Items))
	dropped := 0
	for _, item := range out.Items {
		validated, ok := validateExtractItem(item, messages, known, dedupe, now, rules)
		if !ok {
			dropped++
			continue
		}
		dedupe[validated.Content] = struct{}{}
		accepted = append(accepted, validated)
	}
	return accepted, dropped
}

// validateExtractItem 按 §2.3.4 的丢弃 / 降级顺序校验一条条目；第二返回值为 false 表示丢弃。
// validateExtractItem applies the §2.3.4 drop/downgrade rules; false means dropped.
func validateExtractItem(item extractItem, messages map[int64]core.LearningMessage, known map[int64]struct{}, dedupe map[string]struct{}, now time.Time, rules extractRules) (validatedItem, bool) {
	kind := core.LearningItemKind(item.Kind)
	if !validLearningKind(kind) {
		return validatedItem{}, false
	}
	if strings.TrimSpace(item.Content) == "" || len(item.Content) > rules.MaxContentBytes {
		return validatedItem{}, false
	}
	if _, duplicate := dedupe[item.Content]; duplicate {
		return validatedItem{}, false
	}
	if TextHasCredential(item.Content) {
		return validatedItem{}, false
	}
	name := ""
	if kind == core.LearningKindMethod {
		name = strings.TrimSpace(item.Name)
		if !validDraftName(name) {
			return validatedItem{}, false
		}
	}
	if len(item.Evidence) == 0 || len(item.Evidence) > rules.MaxEvidence {
		return validatedItem{}, false
	}

	evidence := make([]core.LearningEvidence, 0, len(item.Evidence))
	seenMessages := make(map[int64]struct{}, len(item.Evidence))
	hasUserEvidence := false
	var evidenceAt time.Time
	for _, claimed := range item.Evidence {
		message, ok := messages[claimed.MessageID]
		if !ok {
			// 窗口外（含前文消息与跨来源消息）：前文只进提示词，永不作证据（C-10）。
			return validatedItem{}, false
		}
		if message.Role != roleUser && message.Role != roleAssistant {
			return validatedItem{}, false
		}
		if claimed.Role != message.Role {
			return validatedItem{}, false
		}
		if claimed.Quote == "" || !strings.Contains(message.Content, claimed.Quote) {
			return validatedItem{}, false
		}
		if TextHasCredential(claimed.Quote) {
			return validatedItem{}, false
		}
		if _, duplicate := seenMessages[claimed.MessageID]; duplicate {
			continue
		}
		seenMessages[claimed.MessageID] = struct{}{}
		if message.Role == roleUser {
			hasUserEvidence = true
		}
		if message.CreatedAt.After(evidenceAt) {
			evidenceAt = message.CreatedAt
		}
		evidence = append(evidence, core.LearningEvidence{
			MessageID: message.MessageID,
			RunID:     message.RunID,
			Role:      message.Role,
			Quote:     claimed.Quote,
			MessageAt: message.CreatedAt,
		})
	}
	if len(evidence) == 0 {
		return validatedItem{}, false
	}
	if evidenceAt.UnixMilli() < 1 {
		// 记录时刻不可求证时不写零值 / 负数（C-25；DB CHECK evidence_at >= 1 兜底）。
		return validatedItem{}, false
	}

	// fact / preference / constraint / temporary 必须有用户证据，否则降级为 inference；
	// method 不在降级清单里（它只进草案，证据用于 evidence.json 复核）。
	switch kind {
	case core.LearningKindFact, core.LearningKindPreference, core.LearningKindConstraint, core.LearningKindTemporary:
		if !hasUserEvidence {
			kind = core.LearningKindInference
		}
	}

	validTo := time.Time{}
	timeConfidence := timeConfidenceUnknown
	if kind == core.LearningKindTemporary {
		// 只接受 RFC3339 绝对时刻，且必须晚于当前；相对日期不解析、不猜（C-25）。
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(item.ValidTo))
		if err != nil || !parsed.After(now) {
			return validatedItem{}, false
		}
		validTo, timeConfidence = parsed, timeConfidenceExact
	}

	certainty := certaintyUncertain
	switch kind {
	case core.LearningKindFact, core.LearningKindPreference, core.LearningKindConstraint, core.LearningKindTemporary:
		// 走到这里说明未被降级，即至少有一条 user 证据。
		certainty = certaintyAsserted
	}

	if len(item.SourceEntryIDs) > rules.MaxSourceEntries {
		return validatedItem{}, false
	}
	sources := make([]int64, 0, len(item.SourceEntryIDs))
	seenSources := make(map[int64]struct{}, len(item.SourceEntryIDs))
	for _, entryID := range item.SourceEntryIDs {
		if _, ok := known[entryID]; !ok {
			return validatedItem{}, false
		}
		if _, duplicate := seenSources[entryID]; duplicate {
			continue
		}
		seenSources[entryID] = struct{}{}
		sources = append(sources, entryID)
	}

	suggestion := item.SuggestedSupersedesID
	if suggestion != 0 {
		if _, ok := known[suggestion]; !ok {
			// 快照之外的建议目标只清空建议，条目保留（§2.3.4）。
			suggestion = 0
		}
	}

	// 置信度只作展示与排序提示：夹到 [0,1] 防止异常值，不作为任何门槛。
	confidence := item.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}

	return validatedItem{
		Kind:                  kind,
		Content:               item.Content,
		Name:                  name,
		Confidence:            confidence,
		Evidence:              evidence,
		SourceEntryIDs:        sources,
		SuggestedSupersedesID: suggestion,
		EvidenceAt:            evidenceAt,
		ValidTo:               validTo,
		TimeConfidence:        timeConfidence,
		Certainty:             certainty,
	}, true
}

// 窗口消息与证据引用的角色取值（与 messages.role / memory_extraction 的窗口过滤同源）。
// The role literals shared with the window query.
const (
	roleUser      = "user"
	roleAssistant = "assistant"
)

// 时间与确定性字面量：与 memory_candidate_meta 的 CHECK 闭集同源。`relative_unknown`
// 仍留在 DB 闭集里，但本规则版本不产生它——相对日期一律记 unknown（C-25）。
// Literals shared with the memory_candidate_meta CHECK sets; relative_unknown is a
// reservable DB value this rule version never produces.
const (
	timeConfidenceExact   = "exact"
	timeConfidenceUnknown = "unknown"
	certaintyAsserted     = "asserted"
	certaintyUncertain    = "uncertain"
)

// validLearningKind 判据与 memory_candidate_meta.kind 的 CHECK 同源（含 method——它只进
// 草案表，不进候选表）。
// validLearningKind mirrors the kind set; method only ever becomes a draft.
func validLearningKind(kind core.LearningItemKind) bool {
	switch kind {
	case core.LearningKindFact, core.LearningKindPreference, core.LearningKindConstraint,
		core.LearningKindInference, core.LearningKindTemporary, core.LearningKindMethod:
		return true
	}
	return false
}

// validDraftName 复用技能名的**同一份**判据（`workspace.ValidateAgentName`，技能名经
// validSkillName 走的也是它）并加上模块卡的 100 字符上限。草案名只是 DRAFT.md 的展示标签，
// 不参与路径拼接；但规则不得自成一版。
// validDraftName reuses the skill-name rule and adds the 100-character cap.
func validDraftName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > 100 {
		return false
	}
	return workspace.ValidateAgentName(name) == nil
}

// TextHasCredential 是抽取候选正文与证据引文的凭证判定：逐字委托共用扫描器。
// TextHasCredential delegates to the shared scanner; it never re-implements the rules.
func TextHasCredential(text string) bool { return core.TextHasSecret(text) }
