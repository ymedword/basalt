package diagnostics

import (
	"math"
	"time"
)

// MeasuredPhase 是阶段耗时证据的固定标量视图：名称、父阶段、状态、有限耗时与结束时刻。
// 2026-11-02 的阶段计时裁决只新增这些固定标量 attrs；读取入口同时被存储脱敏、
// 读侧投影与导出脱敏复用。
//
// MeasuredPhase is the fixed scalar view of phase-timing evidence. The 2026-11-02
// adjudication adds only these scalar attrs; storage redaction, read projection and
// export redaction all share the same reader.
type MeasuredPhase struct {
	Name, Parent, Status string
	DurationMS           float64
	End                  time.Time
}

func ReadPhase(kind Kind, attrs map[string]any) (MeasuredPhase, bool) {
	name, _ := attrs["phase"].(string)
	parent, _ := attrs["phase_parent"].(string)
	status, _ := attrs["phase_status"].(string)
	end, _ := attrs["phase_end"].(string)
	expectedKind, expectedParent := Kind(""), ""
	switch name {
	case "knowledge_retrieval":
		expectedKind = KindRetrieval
	case "query_embedding", "retrieval_scope", "lexical_search", "vector_search", "retrieval_fusion", "knowledge_source_read":
		expectedKind, expectedParent = KindRetrieval, "knowledge_retrieval"
	case "context_prepare":
		expectedKind = KindAssembly
	case "knowledge_revalidate", "context_compaction", "context_assembly":
		expectedKind, expectedParent = KindAssembly, "context_prepare"
	case "model_first_response", "model_response_read":
		expectedKind, expectedParent = KindLLMCall, "model_call"
	default:
		return MeasuredPhase{}, false
	}
	duration, ok := attrs["duration_ms"].(float64)
	if !ok {
		if value, valid := attrs["duration_ms"].(int64); valid {
			duration, ok = float64(value), true
		}
	}
	finished, err := time.Parse(time.RFC3339Nano, end)
	if kind != expectedKind || parent != expectedParent || (status != "completed" && status != "failed" && status != "cancelled" && status != "skipped") || !ok || math.IsNaN(duration) || math.IsInf(duration, 0) || duration < 0 || duration > 30*24*60*60*1000 || err != nil || finished.UnixMilli() < 0 {
		return MeasuredPhase{}, false
	}
	return MeasuredPhase{Name: name, Parent: parent, Status: status, DurationMS: duration, End: finished}, true
}
