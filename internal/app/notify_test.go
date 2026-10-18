package app

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/testutil"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// TestNewNotifyAdapters_ExactTypeSet —— P1（约束 7 / R11）：类型 → 适配器映射只有
// 装配层一处定义，且**恰好** `webhook` / `feishu` 两键；多一键（擅自承诺原生协议）
// 少一键都当场落红——工具内核按类型查表，装配层漏键即该类型渠道全部不可用。
// P1 (R11): the type→adapter map is defined only here and holds exactly two keys.
func TestNewNotifyAdapters_ExactTypeSet(t *testing.T) {
	adapters := newNotifyAdapters()
	require.Len(t, adapters, 2, "类型 → 适配器映射恰好两键")

	keys := make([]string, 0, len(adapters))
	for name := range adapters {
		keys = append(keys, name)
	}
	slices.Sort(keys)
	require.Equal(t, []string{"feishu", "webhook"}, keys)
	require.IsType(t, &tool.WebhookNotifyAdapter{}, adapters["webhook"], "两键的值类型不得互换")
	require.IsType(t, &tool.FeishuNotifyAdapter{}, adapters["feishu"], "两键的值类型不得互换")
}

// TestAgentsPort_LightPathAcceptsNotifyDeclaration —— P2（约束 23 / R11）：轻路径
// （`profile list` / `profile show`）不建注册池，装载判定拿装配层的**声明名集**；
// 声明了 `tools: [notify]` 的 Agent 必须与重路径得到同一结论——注册。声明名集漏掉
// `notify` 时同一份 AGENT.md 在轻路径被判未注册注册，两条路开始分叉。
// P2 (R11): the light path judges registration against the assembly's declared name set;
// an agent declaring notify must register exactly as it would on the heavy path.
func TestAgentsPort_LightPathAcceptsNotifyDeclaration(t *testing.T) {
	workDir := testutil.TempWorkspace(t)
	root := filepath.Join(workDir, ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	writeAgentDir(t, root, "notifier", `---
name: notifier
provider:
  name: deepseek
  model: deepseek-chat
tools: [notify]
---

巡检结束后把摘要推到 ops 渠道。
`)

	port := agentsPort{WorkDir: workDir}
	entries, err := port.List(root, []string{"deepseek"})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered,
		"轻路径与重路径共用同一份声明名集：notify 必须可注册：%s", entries[0].RejectReason)
}
