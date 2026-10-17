package memory_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDependencyDirection_MemoryImportsOnlyCoreWorkspaceStdlib 守「记忆包生产 import 面」
// （FR-038 / SC-008 / 颗粒度文档 §6.2 口径 26）：只允许**标准库 + core + workspace**。
// 正向白名单比禁清单更强——清单之外的任何越界依赖（storage / tool / cli / app …）也会红，
// 不只挡住已知的几个包名。T023 里的 `go list` 是验证任务的一次性读数，**不是**持久判据；
// 本测试补足这个承载缺口（analyze C1 处置）。
// The memory package's production import face is stdlib + core + workspace only; a
// positive allowlist also catches dependencies nobody thought to blacklist.
//
// 判据读的是 `go list` 的**非测试**导入集（`{{.Imports}}`）：`carrier_contract_test.go`
// 里 test-only 的 `internal/storage` 导入不受影响——「test-only 可引 storage」与
// 「生产面不得引」两条并存，正是约束 17 的两档载体场景表所需的形态。
// `go list .Imports` is the non-test set, so the test-only storage import stays legal.
//
// 逆改点：给任一生产文件（如 `markdown.go`）加
// `_ "github.com/ymedword/basalt/internal/storage"` → 本条红，失败文案点名该路径。
// Inverse probe: adding a storage import to any production file turns this test red
// with the offending path in the message.
func TestDependencyDirection_MemoryImportsOnlyCoreWorkspaceStdlib(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", `{{join .Imports " "}}`,
		"github.com/ymedword/basalt/internal/memory").Output()
	require.NoError(t, err, "go list 是判据的一部分，必须可用: %s", out)

	allowed := map[string]bool{
		"github.com/ymedword/basalt/internal/core":      true,
		"github.com/ymedword/basalt/internal/workspace": true,
	}
	for _, imp := range strings.Fields(string(out)) {
		if allowed[imp] {
			continue
		}
		// 标准库判据 = 路径首段不含 "."（第三方域名必带点；标准库无域名前缀）。
		first := strings.SplitN(imp, "/", 2)[0]
		require.NotContains(t, first, ".",
			"记忆包生产导入面只允许标准库与 core / workspace——越界依赖：%s", imp)
	}
}
