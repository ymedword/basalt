package diagnostics

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiagnosticsEnum_MigrationMatchesCodeAndOpenAPI(t *testing.T) {
	// 判据：迁移、代码与公开枚举一一对应，缺值/多值都失败。
	raw, err := os.ReadFile("../storage/postgres/migrations/0006_observability.sql")
	require.NoError(t, err)
	for _, c := range []struct {
		constraint string
		want       []string
	}{
		{"diagnostic_events_kind_check", kindStrings()},
		{"diagnostic_events_reason_code_check", append([]string{""}, reasonCodeStrings()...)},
	} {
		t.Run(c.constraint, func(t *testing.T) {
			section := strings.SplitN(string(raw), "ADD CONSTRAINT "+c.constraint, 2)
			require.Len(t, section, 2)
			check := strings.SplitN(section[1], "NOT VALID", 2)
			require.Len(t, check, 2, "historical unknown values must remain readable")
			var got []string
			for _, match := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(check[0], -1) {
				got = append(got, match[1])
			}
			require.ElementsMatch(t, c.want, got)
		})
	}
	doc := openAPIDoc(t)
	require.ElementsMatch(t, enumStrings(t, dig(t, doc, "components", "schemas", "DiagnosticKind", "enum")), kindStrings())
	require.ElementsMatch(t, enumStrings(t, dig(t, doc, "components", "schemas", "DiagnosticReasonCode", "enum")), reasonCodeStrings())
}

func TestObservabilityProducer_ToolCallIDRedaction(t *testing.T) {
	redactor := NewWhitelistRedactor()
	for _, kind := range []Kind{KindToolCall, KindPermissionDecision} {
		got := redactor.Redact(kind, map[string]any{"tool_call_id": "call_real", "password": "CANARY_OBS_SECRET", "unknown": "CANARY_OBS_UNKNOWN"})
		require.Equal(t, "call_real", got["tool_call_id"], "actual tool correlation must survive redaction")
		require.NotContains(t, got, "password")
		require.NotContains(t, got, "unknown")
	}
	require.Empty(t, redactor.Redact(KindRetrieval, map[string]any{"tool_call_id": "not-a-tool"}))
}
