package observability

import "github.com/ymedword/basalt/internal/diagnostics"

// redactTimelineEntry 在消费侧再收窄原因与允许属性，不信任适配器或历史数据。
// redactTimelineEntry rechecks reasons and allowed attributes without trusting adapters or history.
func redactTimelineEntry(e TimelineEntry) TimelineEntry {
	if e.Source == SourceDiagnostic {
		if !diagnostics.Kind(e.Kind).Valid() {
			e.Kind = "unknown"
			e.RedactedFields++
		}
		if e.ReasonCode != "" && !diagnostics.ReasonCode(e.ReasonCode).Valid() {
			e.ReasonCode = "unclassified"
			e.RedactedFields++
		}
		if !diagnostics.Decision(e.Decision).Valid() {
			e.Decision = ""
			e.RedactedFields++
		}
	} else if e.Source == SourceIngest {
		e.ReasonCode = exportIngestReason(e.ReasonCode)
	} else {
		e.ReasonCode = safeContextReason(e.ReasonCode)
	}
	attrs := make(map[string]any, len(e.Attrs))
	for k, v := range e.Attrs {
		if k == "error_kind" {
			text, ok := v.(string)
			if !ok || safeContextReason(text) != text {
				e.RedactedFields++
				continue
			}
		}
		attrs[k] = v
	}
	e.Attrs = exportAttributes(attrs)
	if n, ok := e.Attrs["redacted_fields"].(int); ok {
		e.RedactedFields += n
		delete(e.Attrs, "redacted_fields")
	}
	return e
}
