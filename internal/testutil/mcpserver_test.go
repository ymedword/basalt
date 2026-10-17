package testutil

import (
	"context"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// TestMCPTestServer_StandardToolsAreListed 是测试基建的自证用例：标准工具集三件
// （echo / boom / blob）可被列出，且每件都带非空 input schema。
// TestMCPTestServer_StandardToolsAreListed proves the fixture: the standard tool set
// is listed with non-nil input schemas.
func TestMCPTestServer_StandardToolsAreListed(t *testing.T) {
	transport := NewInMemoryMCPPair(t, 0, StandardMCPTools()...)
	client := mcp.NewClient(&mcp.Implementation{Name: "basalt-test"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })

	page, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(page.Tools))
	for _, tool := range page.Tools {
		names = append(names, tool.Name)
		require.NotNil(t, tool.InputSchema, "工具 %s 的 schema 不得为空", tool.Name)
	}
	sort.Strings(names)
	require.Equal(t, []string{"blob", "boom", "echo"}, names)
}

// TestRunMCPHelper_SkipsWithoutEnv：未设 BASALT_MCP_HELPER 时子进程入口必须跳过——
// 默认层跑到它时是 SKIP 而不是失败；跳过理由必须可读。
// TestRunMCPHelper_SkipsWithoutEnv: the helper entry point skips when the helper
// environment variable is absent, with a human-readable reason.
func TestRunMCPHelper_SkipsWithoutEnv(t *testing.T) {
	t.Setenv("BASALT_MCP_HELPER", "")
	RunMCPHelper(t) // 未设环境变量 → 内部 t.Skip；能走到下一行说明没有跳过
	t.Fatal("未设 BASALT_MCP_HELPER 时 RunMCPHelper 必须跳过")
}
