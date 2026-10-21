package assets

// 本文件是 assets 包自身的最小烟测：覆盖率判据（宪法·开发工作流，
// scripts/check-coverage.sh 第 4 类）要求有可执行语句的包必须有测试文件，本包
// 只有 Console() 一条入口。产物完整性 / 逐文件 sha256 断言在
// internal/web/console_test.go，那里判的是要发布的同一份 go:embed 产物；本文件
// 只确认入口返回可用的产物根，不重复那份判据。
// A minimal smoke test for the embed entry point so this package is judged on its
// own coverage; the integrity assertions live in internal/web/console_test.go.

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConsole_ExposesDistRoot 验收点：Console() 返回的 fs.FS 以 dist 为根——
// 根下有 index.html，assets/ 目录非空。嵌入路径写错时静态壳整包失效，这里先红。
func TestConsole_ExposesDistRoot(t *testing.T) {
	t.Parallel()

	fsys := Console()
	require.NotNil(t, fsys)

	_, err := fs.Stat(fsys, "index.html")
	require.NoError(t, err, "产物根缺少 index.html")

	entries, err := fs.ReadDir(fsys, "assets")
	require.NoError(t, err, "产物根缺少 assets/ 目录")
	require.NotEmpty(t, entries, "dist/assets 为空：前端构建产物缺失")
}
