package observability

// EvidenceGrade 限定一项结论能被记录证明到什么程度。
// EvidenceGrade bounds what a recorded fact can prove.
type EvidenceGrade string

// 证据档位按项给出，装配证据不能证明模型实际使用。
// Evidence grades never equate assembly with actual model use.
const (
	EvidenceDirect           EvidenceGrade = "direct"
	EvidenceNeedsOutputCheck EvidenceGrade = "needs_output_check"
	EvidenceInsufficient     EvidenceGrade = "insufficient"
)

// GapReason 只陈述有依据的缺口原因。
// GapReason describes only supported evidence gaps.
type GapReason string

// 缺口与空结果分别表达，未知不得渲染为正常。
// Missing evidence is distinct from a successful empty result.
const (
	GapNotRecorded            GapReason = "not_recorded"
	GapSampledOut             GapReason = "sampled_out"
	GapQueryFailed            GapReason = "query_failed"
	GapRetentionWindowCleaned GapReason = "retention_window_cleaned"
	GapNotSupported           GapReason = "not_supported"
	GapUnknown                GapReason = "unknown"
)

// EvidenceGap 不携带原始错误、凭据或被过滤资源的身份。
// EvidenceGap excludes raw errors, secrets, and filtered identities.
type EvidenceGap struct {
	Source SourceKind `json:"source"`
	Reason GapReason  `json:"reason"`
	Detail string     `json:"detail"`
}
