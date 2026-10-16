//go:build unix

package workspace_test

// 本文件是 R2 评审修复的补充回归：bootstrap 声明的**特殊文件**必须在装载期拒载
// （颗粒度文档 §3.2 六文件之外的新增用例，登记见 US-3 验收报告 R2 段）。
// 特殊文件里唯一能程序化造出的形态是 FIFO（syscall.Mkfifo，POSIX 专有），故本文件带
// unix 构建标签——Windows 上不参与编译；该平台上非普通文件的**目录**形态由
// agent_test.go 的 TestAgentLoader_RejectsBootstrapDirectory 覆盖，两条走的是同一处
// 判定（info.Mode().IsRegular()）。
// Special-file coverage for the bootstrap type check; POSIX-only, hence the build tag.

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// 中文验收点：bootstrap 指向 FIFO（特殊文件）→ 装载期拒载，reason 取契约字面量
// bootstrap file missing，detail 点名路径与类型；不得拖到段一读取时才失败。
// Acceptance point: a FIFO named by bootstrap rejects the agent at load time.
func TestAgentLoader_RejectsBootstrapSpecialFile(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, syscall.Mkfifo(filepath.Join(root, "PIPE"), 0o600))
	writeAgentFrontmatter(t, root, "boot-agent", `
name: boot-agent
provider: {name: deepseek, model: deepseek-chat}
bootstrap: [PIPE]
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered, "bootstrap 指向特殊文件必须整只不注册")
	require.Contains(t, entries[0].RejectReason, "bootstrap file missing")
	require.Contains(t, entries[0].RejectReason, "PIPE")
	require.Contains(t, entries[0].RejectReason, "不是普通文件")
}
