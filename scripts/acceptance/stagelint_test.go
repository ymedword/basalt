package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStageLint_TransformListArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		required []string
		want     []string
		wantErr  bool
	}{
		{
			name:     "drops-execution-flags",
			args:     []string{"go", "test", "-json", "-count=1", "-tags=e2e", "-run", "^TestA$", "./internal/e2e"},
			required: []string{"TestA", "TestB/sub"},
			want:     []string{"go", "test", "-tags=e2e", "./internal/e2e", "-list", "^(?:TestA|TestB)$"},
		},
		{
			name:     "keeps-chdir-and-packages",
			args:     []string{"go", "-C", ".", "test", "-v", "./...", "-run=^TestX$", "-list=old"},
			required: []string{"TestX"},
			want:     []string{"go", "-C", ".", "test", "./...", "-list", "^TestX$"},
		},
		{
			name:     "subtests-truncate-to-top-level",
			args:     []string{"go", "test", "-json", "-count=1", "./x"},
			required: []string{"TestParent/case", "TestParent/other"},
			want:     []string{"go", "test", "./x", "-list", "^TestParent$"},
		},
		{
			name: "non-go-command",
			args: []string{"make", "test"}, wantErr: true,
		},
		{
			name: "non-test-go-command",
			args: []string{"go", "vet", "./..."}, wantErr: true,
		},
		{
			name:     "rejects-exec",
			args:     []string{"go", "test", "-exec", "/bin/false", "./internal/e2e"},
			required: []string{"TestA"}, wantErr: true,
		},
		{
			name:     "rejects-toolexec",
			args:     []string{"go", "test", "-toolexec=/bin/false", "./internal/e2e"},
			required: []string{"TestA"}, wantErr: true,
		},
		{
			name:     "rejects-overlay",
			args:     []string{"go", "test", "-overlay=overlay.json", "./internal/e2e"},
			required: []string{"TestA"}, wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := transformListArgs(tc.args, tc.required)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestStageLint_DetectsMissingTest 现行 stage 失配为错误、历史 stage 失配仅告警，且 lint 只读。
// TestStageLint_DetectsMissingTest fails closed on the live stage and only warns on history.
func TestStageLint_DetectsMissingTest(t *testing.T) {
	root := t.TempDir()
	stages := filepath.Join(root, "harness", "stages")
	require.NoError(t, os.MkdirAll(stages, 0o700))
	writeStage := func(name, checkID string, required ...string) []byte {
		quoted := make([]string, len(required))
		for i, test := range required {
			quoted[i] = `"` + test + `"`
		}
		raw := []byte(`{"version":1,"id":"` + checkID + `","approval":"test","inputs":["harness/acceptance.json"],"checks":[{"id":"` + checkID + `","args":["go","test","-json","-count=1","./x","-run","^TestPresent$"],"timeout_seconds":10,"required_tests":[` + strings.Join(quoted, ",") + `]}],"tasks":[{"id":"t1","checks":["` + checkID + `"]}]}`)
		require.NoError(t, os.WriteFile(filepath.Join(stages, name), raw, 0o600))
		return raw
	}
	current := writeStage("current.json", "live", "TestPresent", "TestMissing")
	writeStage("history.json", "old", "TestGone")

	list := func(context.Context, string, []string) (string, error) {
		return "TestPresent\nok example/pkg\n", nil
	}
	findings, err := lintStages(context.Background(), root, "harness/stages/current.json", list)
	require.NoError(t, err)
	require.Len(t, findings, 2)
	byTest := map[string]lintFinding{}
	for _, finding := range findings {
		byTest[finding.Test] = finding
	}
	require.Equal(t, "error", byTest["TestMissing"].Severity)
	require.Equal(t, "harness/stages/current.json", byTest["TestMissing"].Stage)
	require.Equal(t, "live", byTest["TestMissing"].Check)
	require.Equal(t, "warn", byTest["TestGone"].Severity)
	require.Equal(t, "harness/stages/history.json", byTest["TestGone"].Stage)

	after, err := os.ReadFile(filepath.Join(stages, "current.json"))
	require.NoError(t, err)
	require.Equal(t, current, after, "stage lint is read-only")

	_, err = lintStages(context.Background(), root, "", list)
	require.ErrorContains(t, err, "requires the current stage")
}
