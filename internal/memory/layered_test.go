package memory_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

func TestLoadLayers_ExplicitChoiceAndAuthorization(t *testing.T) {
	service, err := memory.NewService(memory.Options{Backend: memory.NewMarkdownStore(t.TempDir()), Logger: testLogger(t)})
	require.NoError(t, err)
	for _, item := range []struct {
		scope   core.Scope
		content string
	}{
		{core.GlobalScope, "共享事实"},
		{core.GlobalScope, "相同事实"},
		{core.Scope{Agent: "alice"}, "Agent 偏好"},
		{core.Scope{Agent: "alice"}, "相同事实"},
	} {
		require.NoError(t, service.Append(ctx, item.scope, core.SectionCore, item.content))
	}
	combined, err := service.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), true)
	require.NoError(t, err)
	require.Contains(t, combined, "全局记忆")
	require.Contains(t, combined, "Agent 记忆")
	require.Equal(t, 1, strings.Count(combined, "相同事实"))
	local, err := service.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), false)
	require.NoError(t, err)
	require.NotContains(t, local, "共享事实")
	_, err = service.LoadLayers(ctx, "alice", core.Principal{}, true)
	require.ErrorIs(t, err, core.ErrUnauthorized)
	caller := core.Principal{
		Kind: core.PrincipalCaller, CallerID: "caller", KeyID: "key", Actor: core.ActorCaller,
		Scopes: []string{core.AgentInvokeScopePrefix + "bob"},
	}
	require.True(t, caller.Valid())
	_, err = service.LoadLayers(ctx, "alice", caller, true)
	require.ErrorIs(t, err, core.ErrForbidden)
}

// TestLoadLayers_MergePathStillWarnsOnOverQuota 守「合并注入不得吞掉超配额告警」：
// `include_global_memory = true` 走的是另一条渲染路径（不经 `coreSectionOf`），
// 手工编辑出的超配额核心区若只在那条路径告警，合并注入的用户就再也看不到任何痕迹
// ——这条注入路径**没有审计行**，日志是唯一留痕。
// 逆改点：删掉 LoadLayers 合并分支里的 warnCoreOverQuota 调用 → 本用例的 WARN 断言红。
// 验收点：FR-021 / FR-025；复核发现 GM-02。
func TestLoadLayers_MergePathStillWarnsOnOverQuota(t *testing.T) {
	root := t.TempDir()
	svc, err := memory.NewService(memory.Options{Backend: memory.NewMarkdownStore(root), Logger: captureLogger(t)})
	require.NoError(t, err)

	// 两条超配额分别落在**不同层**：全局层总量超限、Agent 层单条超限。合并注入必须
	// 两层都记——只判 Agent 层会漏掉全局层，反之亦然。
	globalFile := filepath.Join(root, "memory", "MEMORY.md")
	agentFile := filepath.Join(root, "memory", "agents", "alice", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(agentFile), 0o755))

	bulk := strings.Repeat("z", core.MaxCoreSectionBytes/2+1024)
	global := globalMemoryTitle + "\n\n## 核心记忆\n\n- " + bulk + "\n\n- " + bulk + "\n\n## 归档记忆\n"
	require.NoError(t, os.WriteFile(globalFile, []byte(global), 0o600))

	oversized := strings.Repeat("x", core.MaxCoreEntryBytes+1)
	agent := globalMemoryTitle + "\n\n## 核心记忆\n\n- " + oversized + "\n\n## 归档记忆\n"
	require.NoError(t, os.WriteFile(agentFile, []byte(agent), 0o600))

	got, err := svc.LoadLayers(ctx, "alice", core.LocalAdminPrincipal(), true)
	require.NoError(t, err)
	require.Contains(t, got, "全局记忆")
	require.Contains(t, got, "Agent 记忆")

	logs := captureWriter.logs(t)
	require.Contains(t, logs, "WARN")
	require.Contains(t, logs, "单条", "Agent 层的单条超配额必须在合并路径上告警")
	require.Contains(t, logs, "核心记忆区超出写配额", "全局层的总量超配额必须在合并路径上告警")
	require.NotContains(t, logs, bulk, "日志不得打印记忆正文")
	require.NotContains(t, logs, oversized, "日志不得打印记忆正文")
}
