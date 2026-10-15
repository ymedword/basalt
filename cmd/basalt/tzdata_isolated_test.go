//go:build tzisolated

// 隔离组（构建 tag `tzisolated`，**不入** make check；CI 必跑步骤在「已删系统时区数据与
// GOROOT zoneinfo.zip」的容器内执行，见颗粒度文档 §3.3）。
//
// 为什么隔离组要另立构建标签：默认层说不清「时区解析成功」是内嵌数据生效，还是这台
// 机器上本来就有系统 tzdata。只有在系统数据被真正删除的环境里，正反对照才有判别力。
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// tzProbeHelperEnv 是 helper 子进程的开关：本测试二进制重入时按此分支走探针逻辑。
const tzProbeHelperEnv = "TZ_PROBE_HELPER"

// tzProbeSource 是对照探针的源码：**不**导入 time/tzdata，故隔离环境里必须先失败。
//
// 探针契约（判据固定，构建细节可调）：
//
//	加载成功 → exit 0；加载失败 → stderr 打印 `TZ_LOAD_FAILED: <err>` 且 exit 3。
//
// 「必须以时区加载失败这一种失败」失败——启动失败 / 崩溃 / 任意非 3 退出码都判红，
// 否则隔离步骤会因为探针本身写坏了而假绿。
const tzProbeSource = `package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
		fmt.Fprintf(os.Stderr, "TZ_LOAD_FAILED: %v\n", err)
		os.Exit(3)
	}
	os.Exit(0)
}
`

// TestTZData_IsolatedEmbeddedFallback（主组）：helper 子进程 = 本测试二进制
// （携带 time/tzdata），隔离环境内必须成功。
func TestTZData_IsolatedEmbeddedFallback(t *testing.T) {
	if os.Getenv(tzProbeHelperEnv) == "1" {
		if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
			t.Fatalf("内嵌 tzdata 未生效: %v", err)
		}
		return
	}
	out, err := runIsolated(t, os.Args[0], "-test.run=TestTZData_IsolatedEmbeddedFallback")
	require.NoError(t, err, string(out)) // 主组：隔离环境内必须成功
}

// TestTZData_IsolatedControlWithoutImport（对照）：无 time/tzdata 导入的探针在隔离环境内
// **必须失败**，且必须是「时区加载失败」这一种失败——探针契约：LoadLocation 失败 →
// stderr 打印 TZ_LOAD_FAILED: <err> 且 exit 3；成功 → exit 0。
// 启动失败 / 崩溃 / 任意非 3 退出码 = 对照无效 → 本测试判红（不得把任意错误当成隔离成功）。
func TestTZData_IsolatedControlWithoutImport(t *testing.T) {
	probe := buildProbeWithoutTZData(t) // 现场 go build；构建细节以实现期实测收敛，判据固定
	out, err := runIsolated(t, probe)
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee, "对照探针未失败或非进程退出错误——隔离无效或探针构造有误: %s", out)
	require.Equal(t, 3, ee.ExitCode(), "对照失败原因不是时区加载失败: %s", out)
	require.Contains(t, string(out), "TZ_LOAD_FAILED")
}

// runIsolated 在隔离环境里跑一个子进程：继承环境 + TZ="" + ZONEINFO / GOROOT 指向
// 不存在的路径（容器步骤已实际删除系统 tzdata 与 GOROOT zip，环境变量是双保险）。
// 返回合并输出与错误——判据看退出码与标记，两者都要能取到。
func runIsolated(t *testing.T, name string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(),
		"TZ=",
		"ZONEINFO="+filepath.Join(t.TempDir(), "no-such-zoneinfo.zip"),
		"GOROOT="+filepath.Join(t.TempDir(), "no-such-goroot"),
		tzProbeHelperEnv+"=1",
	)
	return cmd.CombinedOutput()
}

// buildProbeWithoutTZData 现场构建对照探针并返回其路径。
// 单独起一个最小 module——探针必须「干净」（不 import time/tzdata），
// 不能沾本包的依赖面。
func buildProbeWithoutTZData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/tzprobe\n\ngo 1.27\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte(tzProbeSource), 0o644))

	bin := filepath.Join(dir, "tzprobe"+exeSuffix())
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "探针构建失败: %s", out)
	return bin
}

// exeSuffix 给出本平台可执行文件的后缀——显式写死比依赖 go build 的补全行为稳。
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
