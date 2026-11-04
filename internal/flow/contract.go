package flow

import (
	"encoding/json"

	"github.com/ymedword/basalt/internal/core"
)

// BudgetCap 是不可放大的预算上限；零表示未配置，不表示无限。
// BudgetCap is a ceiling; zero means unconfigured rather than unlimited.
type BudgetCap struct {
	MaxTotalTokens     int64 `json:"max_total_tokens"`
	MaxToolCalls       int64 `json:"max_tool_calls"`
	MaxDurationSeconds int64 `json:"max_duration_seconds"`
	MaxDepth           int   `json:"max_depth"`
	MaxChildren        int   `json:"max_children"`
	MaxAttemptsPerNode int   `json:"max_attempts_per_node"`
	MaxRevisions       int   `json:"max_revisions"`
}

// RequiredSlot 绑定作者指定的适用规则；found 不等于 verified。
// RequiredSlot binds applicable author rules; finding evidence is not verification.
type RequiredSlot struct {
	ID         string   `json:"id"`
	Validators []string `json:"validators"`
	MinSources int      `json:"min_sources"`
}

// AcceptanceRule 固定规则类别、版本、目标和判据；模型不可覆盖。
// AcceptanceRule pins rule identity, version, target and acceptance criteria.
type AcceptanceRule struct {
	ID            string          `json:"id"`
	Version       int64           `json:"version"`
	Kind          string          `json:"kind"`
	TargetKind    string          `json:"target_kind"`
	TargetID      string          `json:"target_id"`
	Required      bool            `json:"required"`
	Schema        json.RawMessage `json:"schema,omitempty"`
	MinSources    int             `json:"min_sources,omitempty"`
	ValidatorID   string          `json:"validator_id,omitempty"`
	CalibrationID string          `json:"calibration_id,omitempty"`
	HumanRequired bool            `json:"human_required,omitempty"`
}

// ArtifactSpec 限定作者所需的最终产物，不接受调用方任意产物协议。
// ArtifactSpec fixes the author-required deliverable shape.
type ArtifactSpec struct {
	ID       string `json:"id"`
	Format   string `json:"format"`
	Required bool   `json:"required"`
}

// TaskContract 是不可变任务验收模板；输入只能填参数或收紧预算。
// TaskContract is the immutable acceptance template; callers cannot weaken controls.
type TaskContract struct {
	ContractVersion int64              `json:"contract_version"`
	Goal            string             `json:"goal"`
	Scope           core.FlowAuthority `json:"scope"`
	InputSchema     json.RawMessage    `json:"input_schema"`
	InputRefs       []EvidenceRef      `json:"input_refs,omitempty"`
	Constraints     []string           `json:"constraints"`
	RequiredSlots   []RequiredSlot     `json:"required_slots"`
	OutputSchema    json.RawMessage    `json:"output_schema"`
	ArtifactSpec    []ArtifactSpec     `json:"artifact_spec"`
	Acceptance      []AcceptanceRule   `json:"acceptance"`
	Budget          BudgetCap          `json:"budget"`
	StopConditions  []string           `json:"stop_conditions"`
	Escalation      string             `json:"escalation"`
}

// EvidenceRef 保存版本化来源定位；不会复制来源正文为第二权威。
// EvidenceRef identifies a versioned source without duplicating authoritative content.
type EvidenceRef struct {
	ID            string `json:"evidence_id"`
	TaskID        string `json:"task_id"`
	SourceKind    string `json:"source_kind"`
	SourceUID     string `json:"source_uid"`
	SourceVersion string `json:"source_version"`
	Location      string `json:"location"`
	ContentHash   string `json:"content_hash"`
	RunID         string `json:"run_id,omitempty"`
	NodeAttemptID string `json:"node_attempt_id,omitempty"`
}
