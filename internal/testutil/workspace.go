// Package testutil 收拢测试专用基础设施：临时工作区与不触网的本地服务。
// Package testutil collects test-only infrastructure: temporary workspaces and
// loopback HTTP servers that never touch the real network.
//
// 本包只被 _test.go 引用，不进生产装配路径；后续 US 以同口径在此补假 Provider
// （US-2）与 MCP 测试服务（US-7）。
// Only _test.go files import this package; later stories extend it in the same
// idiom.
package testutil

import (
	"testing"
)

// TempWorkspace 返回一个全新空目录，用作进程工作目录（工作区 = <它>/.basalt）。
// TempWorkspace returns a fresh empty directory to be used as the process
// working directory; the workspace proper is <dir>/.basalt.
//
// 返回的是「工作目录」而非 `.basalt` 本身——与 cli.Deps.WorkDir 同口径，可直接
// 注入；每个测试各得一目录，因此互不干扰，也无需 chdir。
// It returns the working directory, matching cli.Deps.WorkDir, so tests need no
// chdir and cannot collide with each other.
func TempWorkspace(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}
