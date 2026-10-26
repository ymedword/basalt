package app

// 本文件判内置安装器的**存量实例回填**与**凭据就绪补写**两段（14D review ② 返工的
// 回归面，用户 2026-10-26 裁决）：
//   - 存量实例（库在、标记不在）：不装 Agent、不写样例库，只在组件真实存在时**追加**
//     缺的免凭据条目——写一条注定起不来的条目，会让正常的老工作区每次启动都多一条连接
//     失败；装五个 Agent 则是升级的越权副作用。
//   - 凭据就绪：`GITHUB_TOKEN` 这类显式登记的别名只影响占位符引用哪一支变量，子进程的
//     环境键恒为 `GITHUB_PERSONAL_ACCESS_TOKEN`。
//   - 标记：`agents_skipped` 落盘后，下一次启动不会把 Agent 补装进老工作区。
//
// These tests pin the legacy backfill (agents untouched, entries gated on disk presence),
// the explicit GitHub token alias, and the agents_skipped marker's persistence.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/builtin"
)

// builtinTestDeps 构造一次安装的显式输入：无凭据、固定时钟、静默日志。
func builtinTestDeps(wsRoot, runtimeRoot string, fresh bool, env map[string]string) builtinOpenBoxDeps {
	return builtinOpenBoxDeps{
		WorkspaceRoot: wsRoot,
		RuntimeRoot:   runtimeRoot,
		Fresh:         fresh,
		LookupEnv: func(name string) (string, bool) {
			value, ok := env[name]
			return value, ok
		},
		Now:    func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) },
		Logger: slog.New(slog.DiscardHandler),
	}
}

// writeBuiltinTestFile 在指定路径放一个占位文件（组件就位只判存在性）。
func writeBuiltinTestFile(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
}

// readBuiltinTestMarker 读出安装标记并断言版本号。
func readBuiltinTestMarker(t *testing.T, wsRoot string) builtinOpenBoxState {
	t.Helper()
	raw, err := os.ReadFile(builtinOpenBoxStatePath(wsRoot))
	require.NoError(t, err)
	var state builtinOpenBoxState
	require.NoError(t, json.Unmarshal(raw, &state))
	require.Equal(t, builtinOpenBoxSchema, state.SchemaVersion)
	return state
}

// TestPrepareBuiltinOpenBox_LegacyBackfillsReadyEntriesOnly 判存量实例的回填边界：
// 组件就位的条目（docs-readonly / fetch）追加进配置，组件缺失的（dbhub）不写，
// 五个 Agent 与样例库一概不碰，标记声明 `agents_skipped`。
func TestPrepareBuiltinOpenBox_LegacyBackfillsReadyEntriesOnly(t *testing.T) {
	wsRoot := t.TempDir()
	runtimeRoot := t.TempDir()
	paths := builtinRuntimePaths(runtimeRoot, wsRoot)
	writeBuiltinTestFile(t, paths.DocsReadOnly)
	writeBuiltinTestFile(t, paths.Python)
	writeBuiltinTestFile(t, paths.Fetch)
	// Node / DBHub 故意缺席：lts 发行包缺失时不允许写出必然起不来的 dbhub 条目。

	report, err := prepareBuiltinOpenBox(context.Background(),
		builtinTestDeps(wsRoot, runtimeRoot, false, nil))
	require.NoError(t, err)

	require.True(t, report.Backfilled, "无标记且非新实例应判为存量回填")
	require.Empty(t, report.AgentsInstalled, "存量实例不得安装 Agent")
	require.Empty(t, report.AgentsKept)
	require.Equal(t, []string{"docs-readonly", "fetch"}, report.MCPWritten)
	require.Empty(t, report.SampleDB.State, "DBHub 组件缺失时不该准备样例库")
	require.Empty(t, report.DBHubConfig)
	require.Contains(t, report.Missing, "node")
	require.Contains(t, report.Missing, "dbhub")

	_, statErr := os.Stat(filepath.Join(wsRoot, agentsDirName))
	require.ErrorIs(t, statErr, os.ErrNotExist, "存量实例的 agents/ 不应被创建")

	raw, err := os.ReadFile(filepath.Join(wsRoot, "mcp_servers.yaml"))
	require.NoError(t, err)
	text := string(raw)
	require.Contains(t, text, "- name: docs-readonly")
	require.Contains(t, text, "- name: fetch")
	require.NotContains(t, text, "- name: dbhub", "组件缺失时不得写 dbhub 条目")
	require.NotContains(t, text, "- name: github", "凭据未就绪时不得写 github 条目")

	state := readBuiltinTestMarker(t, wsRoot)
	require.False(t, state.AgentsInstalled)
	require.True(t, state.AgentsSkipped, "回填标记必须声明 Agent 不归安装器管")
	require.Equal(t, []string{"docs-readonly", "fetch"}, state.MCPEntries)
}

// TestPrepareBuiltinOpenBox_LegacyWithoutComponentsWritesNothing 判组件全缺时的存量
// 实例：一条都不写、不创建配置文件，标记仍然落盘（下一次启动保持同一判定）。
func TestPrepareBuiltinOpenBox_LegacyWithoutComponentsWritesNothing(t *testing.T) {
	wsRoot := t.TempDir()
	runtimeRoot := t.TempDir()

	report, err := prepareBuiltinOpenBox(context.Background(),
		builtinTestDeps(wsRoot, runtimeRoot, false, nil))
	require.NoError(t, err)

	require.True(t, report.Backfilled)
	require.Empty(t, report.MCPWritten)
	_, statErr := os.Stat(filepath.Join(wsRoot, "mcp_servers.yaml"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "没有可写条目时不得创建配置文件")

	state := readBuiltinTestMarker(t, wsRoot)
	require.True(t, state.AgentsSkipped)
	require.True(t, state.InstalledAt != "")
}

// TestPrepareBuiltinOpenBox_LegacySecondRunKeepsAgentsSkipped 判第二次启动：`agents_skipped`
// 让安装器仍然不装 Agent，已写条目零写入、文件逐字节不变。
func TestPrepareBuiltinOpenBox_LegacySecondRunKeepsAgentsSkipped(t *testing.T) {
	wsRoot := t.TempDir()
	runtimeRoot := t.TempDir()
	paths := builtinRuntimePaths(runtimeRoot, wsRoot)
	writeBuiltinTestFile(t, paths.DocsReadOnly)

	deps := builtinTestDeps(wsRoot, runtimeRoot, false, nil)
	first, err := prepareBuiltinOpenBox(context.Background(), deps)
	require.NoError(t, err)
	require.Equal(t, []string{"docs-readonly"}, first.MCPWritten)
	before, err := os.ReadFile(filepath.Join(wsRoot, "mcp_servers.yaml"))
	require.NoError(t, err)

	second, err := prepareBuiltinOpenBox(context.Background(), deps)
	require.NoError(t, err)
	require.False(t, second.Backfilled, "标记已存在，本次不是回填")
	require.Empty(t, second.AgentsInstalled, "agents_skipped 落盘后不得补装 Agent")
	require.Empty(t, second.MCPWritten)

	after, err := os.ReadFile(filepath.Join(wsRoot, "mcp_servers.yaml"))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "零写入路径不得改动配置字节")
	require.True(t, readBuiltinTestMarker(t, wsRoot).AgentsSkipped)
}

// TestPrepareBuiltinOpenBox_GitHubAliasWritesPlaceholder 判 `GITHUB_TOKEN` 这一显式别名：
// 条目只在组件与凭据都就绪时写入，占位符引用别名，子进程的环境键仍是规范名。
func TestPrepareBuiltinOpenBox_GitHubAliasWritesPlaceholder(t *testing.T) {
	wsRoot := t.TempDir()
	runtimeRoot := t.TempDir()
	paths := builtinRuntimePaths(runtimeRoot, wsRoot)
	writeBuiltinTestFile(t, paths.GitHub)

	deps := builtinTestDeps(wsRoot, runtimeRoot, false, map[string]string{
		"GITHUB_TOKEN": "token-in-test",
	})
	report, err := prepareBuiltinOpenBox(context.Background(), deps)
	require.NoError(t, err)
	require.Contains(t, report.MCPWritten, "github")

	raw, err := os.ReadFile(filepath.Join(wsRoot, "mcp_servers.yaml"))
	require.NoError(t, err)
	text := string(raw)
	require.Contains(t, text, "GITHUB_PERSONAL_ACCESS_TOKEN: ${GITHUB_TOKEN}",
		"子进程环境键必须是规范名，占位符引用显式别名")
	require.NotContains(t, text, "token-in-test", "凭据值不得进入配置文件")

	// 第二次启动：github 已记入标记且仍在文件里，零写入。
	before := text
	second, err := prepareBuiltinOpenBox(context.Background(), deps)
	require.NoError(t, err)
	require.Empty(t, second.MCPWritten)
	after, err := os.ReadFile(filepath.Join(wsRoot, "mcp_servers.yaml"))
	require.NoError(t, err)
	require.Equal(t, before, string(after))
	require.Contains(t, readBuiltinTestMarker(t, wsRoot).MCPEntries, "github")
}

// TestPrepareBuiltinOpenBox_GitHubRequiresCredential 判凭据缺席：即使组件就位也不写
// github 条目——写出来就是一条必然发布失败、或发布后连不上的配置。
func TestPrepareBuiltinOpenBox_GitHubRequiresCredential(t *testing.T) {
	wsRoot := t.TempDir()
	runtimeRoot := t.TempDir()
	writeBuiltinTestFile(t, builtinRuntimePaths(runtimeRoot, wsRoot).GitHub)

	report, err := prepareBuiltinOpenBox(context.Background(),
		builtinTestDeps(wsRoot, runtimeRoot, false, map[string]string{
			"GITHUB_PAT": "not-a-registered-alias",
		}))
	require.NoError(t, err)
	require.NotContains(t, report.MCPWritten, "github")
	_, statErr := os.Stat(filepath.Join(wsRoot, "mcp_servers.yaml"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

// TestPrepareBuiltinOpenBox_FreshInstallsAgentsAndAllEntries 判新实例不受回填收紧影响：
// 五个 Agent 照装、三条免凭据条目照写（哪怕运行时目录还是空的，配置也不随本机缺件变形）。
func TestPrepareBuiltinOpenBox_FreshInstallsAgentsAndAllEntries(t *testing.T) {
	wsRoot := t.TempDir()
	runtimeRoot := t.TempDir()

	report, err := prepareBuiltinOpenBox(context.Background(),
		builtinTestDeps(wsRoot, runtimeRoot, true, nil))
	require.NoError(t, err)

	require.False(t, report.Backfilled)
	wantAgents := make([]string, 0, len(builtin.Agents()))
	for _, agent := range builtin.Agents() {
		wantAgents = append(wantAgents, agent.Name)
	}
	require.ElementsMatch(t, wantAgents, report.AgentsInstalled)
	require.Equal(t, []string{"docs-readonly", "dbhub", "fetch"}, report.MCPWritten)
	require.NotEmpty(t, report.DBHubConfig, "新实例不因样例库 pending 而跳过 DBHub 配置")

	state := readBuiltinTestMarker(t, wsRoot)
	require.True(t, state.AgentsInstalled)
	require.False(t, state.AgentsSkipped)

	raw, err := os.ReadFile(filepath.Join(wsRoot, "mcp_servers.yaml"))
	require.NoError(t, err)
	for _, name := range []string{"docs-readonly", "dbhub", "fetch"} {
		require.Contains(t, string(raw), "- name: "+name)
	}
	require.False(t, strings.Contains(string(raw), "- name: github"),
		"新实例没有凭据时同样不写 github 条目")
}
