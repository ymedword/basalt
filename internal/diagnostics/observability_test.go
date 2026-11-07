package diagnostics

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiagnosticsEnum_MigrationMatchesCodeAndOpenAPI(t *testing.T) {
	// 判据：**生效**的迁移约束、代码与公开枚举一一对应，缺值/多值都失败。
	// 约束可以被后续迁移加宽（0012 把原因码闭集扩到 recovery_unknown，因为 0006
	// 已发布、不得改写），所以判卷对象是该约束名的**最后一次** ADD CONSTRAINT，
	// 而不是固定读 0006——否则"库里的生效值域"与"判卷读到的值域"会悄悄分叉。
	// The judged DDL is the last ADD CONSTRAINT for each name across migrations, so a
	// later forward migration that widens a closed set is still covered by this check.
	for _, c := range []struct {
		constraint string
		want       []string
	}{
		{"diagnostic_events_kind_check", kindStrings()},
		{"diagnostic_events_reason_code_check", append([]string{""}, reasonCodeStrings()...)},
	} {
		t.Run(c.constraint, func(t *testing.T) {
			section := strings.SplitN(effectiveConstraintDDL(t, c.constraint), "ADD CONSTRAINT "+c.constraint, 2)
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

// effectiveConstraintDDL 返回迁移目录里该约束名最后一次 `ADD CONSTRAINT ... NOT VALID`
// 起到文件尾的片段：后追加的迁移可以重定义闭集，判卷必须看最终生效的那一份。
// effectiveConstraintDDL returns the tail of the last migration that adds the named
// constraint, so a later forward migration redefining the domain is what gets judged.
func effectiveConstraintDDL(t *testing.T, constraint string) string {
	t.Helper()
	const dir = "../storage/postgres/migrations"
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	var last string
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		if idx := strings.LastIndex(string(raw), "ADD CONSTRAINT "+constraint); idx >= 0 {
			last = string(raw)[idx:]
		}
	}
	require.NotEmpty(t, last, "迁移目录里没有任何 ADD CONSTRAINT %s", constraint)
	return last
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
