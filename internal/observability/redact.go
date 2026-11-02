package observability

import "github.com/ymedword/basalt/internal/diagnostics"

func diagnosticsReason(reason string) bool {
	return reason != "" && diagnostics.ReasonCode(reason).Valid()
}
