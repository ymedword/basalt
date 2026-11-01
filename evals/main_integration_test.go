//go:build integration

package main

// 运行器端到端面：`mainRun` 会走 `app.OpenRuntime` 的生产装配，US-16 起它只连
// PostgreSQL（D-01：缺 `BASALT_TEST_PG_DSN` 即失败，不跳过）。单元面（main_test.go）
// 只放纯函数与端口替身；需要真实库的这条因此单独留在构建标签后。
// The end-to-end runner case needs the real runtime and therefore a real database;
// it lives behind the integration tag while main_test.go keeps the pure logic.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMainRun_IsolatesResults exercises the CLI through the real runtime with a fake model.
func TestMainRun_IsolatesResults(t *testing.T) {
	root := t.TempDir()
	cases := filepath.Join(root, "cases")
	require.NoError(t, os.Mkdir(cases, 0o700))
	writeCaseFile(t, cases, "case.yaml", fakeCaseYAML("isolated-cli", nil))
	t.Chdir(root)
	out := filepath.Join(root, "separate-results")
	var stderr strings.Builder
	require.Zero(t, mainRun([]string{"--cases", cases, "--results", out}, io.Discard, &stderr),
		"运行器失败；stderr=%s", stderr.String())
	files, err := filepath.Glob(filepath.Join(out, "*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	var rpt report
	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &rpt))
	require.Equal(t, 1, rpt.Totals.Passed)
	require.Zero(t, rpt.Totals.Skipped)
	_, err = os.Stat(resultsDirName)
	require.ErrorIs(t, err, os.ErrNotExist)
}
