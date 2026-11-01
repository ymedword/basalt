package scripts

// 本文件是 D-01（US-16 / T125）的闸门自检：集成测试的判据现在依赖真实 PostgreSQL，
// 缺 BASALT_TEST_PG_DSN 必须在**每一层入口**失败，而不是让用例全部跳过冒充通过。
// The gate self-tests for D-01: every entry point must fail closed without a DSN.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIntegrationGate_IntegrationSuiteFailsWithoutDSN 验证第二道闸门：即便绕过
// Makefile 直接 `go test -tags=integration`，缺 DSN 也必须失败（测试助手 fail-closed），
// 不得出现「所有用例 SKIP、整包 ok」的假绿。选一个最早取 DSN 的有库用例做代表。
// Bypassing make still fails: the test helper refuses to skip when the DSN is absent.
func TestIntegrationGate_IntegrationSuiteFailsWithoutDSN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-tags=integration", "-count=1",
		"-run", "^TestPostgres_FreshSchemaIsV1$", "../internal/storage/postgres")
	// 显式清空环境：调用方恰好设置了 DSN 时也能复现"缺 DSN"。
	cmd.Env = withoutEnv(os.Environ(), "BASALT_TEST_PG_DSN")
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "缺 DSN 的集成运行必须失败，不得全部跳过: %s", out)
	require.Contains(t, string(out), "BASALT_TEST_PG_DSN")
	require.Contains(t, string(out), "D-01")
}

// TestIntegrationGate_MakefileTargetsDeclareGate 钉住 Makefile 的前置闸门。行为判据
// 由测试助手与 check-coverage.sh 各自成立；这里防的是守卫被静默移除——Makefile 不进
// Go 编译面，没有别的检查能看见它。
// The Makefile guard has no compiler behind it; removing it must turn this test red.
func TestIntegrationGate_MakefileTargetsDeclareGate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Makefile"))
	require.NoError(t, err)
	var recipe string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "test-integration:") {
			recipe = line
			break
		}
	}
	require.NotEmpty(t, recipe, "Makefile 必须保留 test-integration 目标")
	require.Contains(t, recipe, "BASALT_TEST_PG_DSN", "缺 DSN 的前置闸门不得移除")
	require.Contains(t, recipe, "-tags=integration")
	require.Contains(t, recipe, "-count=1")
}
