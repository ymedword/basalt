package tool_test

// 本文件是执行档位（14D）D3 / D9 两条判据：
//   D3 程序目录放行需三项同时满足——安装清单登记（含 SHA-256）、`EvalSymlinks` 后的真实路径
//      在 `<程序目录>/runtime/**`、该路径处于只读保护下；**只有路径前缀相同 → 拒**。
//   D9 `.cmd` / `.bat` 不按扩展名豁免：既不在安装清单、也不在显式白名单 → 拒；按四要素
//      显式登记后可用（判定只认登记，不看扩展名）。
// D3: the program-directory admission needs all three conditions at once.
// D9: batch-file extensions are neither exempted nor banned; only registration counts.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// programDirFixture 造一个最小程序目录：`<root>/runtime/tools/probe[.exe]` 是测试二进制
// 的副本（可执行、内容可哈希），清单登记同一份文件。
// The fixture copies the test binary into a protected runtime directory and registers it.
type programDirFixture struct {
	root      string   // `<程序目录>`
	runtime   string   // `<程序目录>/runtime`
	probe     string   // 清单登记的那份可执行件（真实路径）
	probeSum  string   // 它的 SHA-256
	probeName string   // 登记路径（相对 runtime，`/` 分隔）
	probeEnv  []string // 让插桩探针能正常退出的最小环境（见 probeCommandEnv）
}

func newProgramDirFixture(t *testing.T, writable bool) *programDirFixture {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	toolsDir := filepath.Join(runtimeDir, "tools")
	require.NoError(t, os.MkdirAll(toolsDir, 0o750))

	probeName := "probe"
	if os.PathSeparator == '\\' {
		probeName += ".exe"
	}
	probe := filepath.Join(toolsDir, probeName)
	self, err := os.ReadFile(os.Args[0])
	require.NoError(t, err, "测试二进制自身必须可读，才能造出真实的程序目录可执行件")
	require.NoError(t, os.WriteFile(probe, self, 0o755))
	perm := os.FileMode(0o555)
	if writable {
		perm = 0o755
	}
	require.NoError(t, os.Chmod(probe, perm))
	if !writable {
		// Windows 上只读属性会让 TempDir 清理失败，先登记还原再删。
		t.Cleanup(func() { _ = os.Chmod(probe, 0o755) })
	}

	sum := sha256.Sum256(self)
	return &programDirFixture{
		root:      root,
		runtime:   runtimeDir,
		probe:     probe,
		probeSum:  hex.EncodeToString(sum[:]),
		probeName: "tools/" + probeName,
		probeEnv:  probeCommandEnv(t),
	}
}

// probeCommandEnv 给探针一个可写临时目录。执行器只透传声明过的环境变量（`mergeChildEnv`），
// 于是子进程默认没有任何临时目录变量；而 `go test -cover` 插桩的测试二进制在退出时必须
// 落到一个临时目录写覆盖率数据（`testing/internal/testdeps.coverTearDown` 在
// `-test.gocoverdir` 缺省时调 `os.MkdirTemp("", "gocoverdir")`），Windows 上拿不到
// `%TEMP%` 就退而用 `C:\WINDOWS` 并因权限失败，整只探针以 2 退出——那是插桩形态的
// 副作用，与本用例要判的 D3 三项条件无关。三个键覆盖三个平台，未插桩时无副作用。
// The probe needs a writable temp dir: the executor passes only declared env vars, and a
// cover-instrumented test binary writes coverage data to os.MkdirTemp("") on exit — with
// no %TEMP% that lands in C:\WINDOWS and exits 2, a coverage artifact, not a D3 verdict.
func probeCommandEnv(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{"TEMP=" + dir, "TMP=" + dir, "TMPDIR=" + dir}
}

// newRunner 按给定清单装配执行器（清单为空同样合法：表示没有自动放行面）。
func (f *programDirFixture) newRunner(t *testing.T, manifest []tool.ProgramDirEntry) *tool.LocalRunner {
	t.Helper()
	return tool.NewLocalRunner(tool.RunnerOptions{
		Root:            t.TempDir(),
		ProgramDir:      f.root,
		ProgramManifest: manifest,
	})
}

// TestExecutionProfile_ProgramDirRequiresThree 覆盖 D3：三项条件缺一即拒，全满足才放行；
// 只有路径前缀相同（清单里登记的是同名文件、解析到 runtime/ 下的另一份）同样拒。
// Acceptance point (D3): every missing condition refuses, all three admit, and a name
// resolving to a different file under runtime/ is refused.
func TestExecutionProfile_ProgramDirRequiresThree(t *testing.T) {
	// 三项同时满足：放行（用测试二进制自身作被测程序，`-test.run=^$` 让它立即以 0 退出）。
	t.Run("all_three_conditions_admit", func(t *testing.T) {
		f := newProgramDirFixture(t, false)
		runner := f.newRunner(t, []tool.ProgramDirEntry{{Path: f.probeName, SHA256: f.probeSum}})
		result, err := runner.Run(context.Background(), core.Command{
			Executable: f.probe,
			Args:       []string{"-test.run=^$"},
			Dir:        f.root,
			Env:        f.probeEnv,
		})
		require.NoError(t, err, "三项条件都满足必须放行：%s", result.Output)
		require.Equal(t, 0, result.ExitCode, "探针应正常退出：%s", result.Output)
	})

	// 条件 ②：名字在清单里，但解析到的文件不是登记的那个 → 拒（只有路径前缀相同不算可信）。
	t.Run("same_name_different_file_is_refused", func(t *testing.T) {
		f := newProgramDirFixture(t, false)
		otherDir := filepath.Join(f.runtime, "other")
		require.NoError(t, os.MkdirAll(otherDir, 0o750))
		other := filepath.Join(otherDir, filepath.Base(f.probe))
		self, err := os.ReadFile(os.Args[0])
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(other, self, 0o555))
		t.Cleanup(func() { _ = os.Chmod(other, 0o755) })

		runner := f.newRunner(t, []tool.ProgramDirEntry{{Path: f.probeName, SHA256: f.probeSum}})
		_, err = runner.Run(context.Background(), core.Command{
			Executable: other,
			Args:       []string{"-test.run=^$"},
			Dir:        f.root,
		})
		require.Error(t, err, "同名但不在清单登记的路径上，不得按清单自动放行")
		require.True(t, core.IsSandboxViolation(err))
		require.Contains(t, err.Error(), "程序目录可执行件的自动放行条件未满足")
	})

	// 条件 ③：保护未生效（文件有写位）→ 拒，降级为需显式登记。
	t.Run("writable_file_is_refused", func(t *testing.T) {
		f := newProgramDirFixture(t, true)
		runner := f.newRunner(t, []tool.ProgramDirEntry{{Path: f.probeName, SHA256: f.probeSum}})
		_, err := runner.Run(context.Background(), core.Command{
			Executable: f.probe,
			Args:       []string{"-test.run=^$"},
			Dir:        f.root,
		})
		require.Error(t, err, "只读保护未生效时不得按清单自动放行")
		require.True(t, core.IsSandboxViolation(err))
	})

	// 条件 ①：内容哈希与安装时不一致 → 拒（保护失败后被人改过的文件不得执行）。
	t.Run("hash_mismatch_is_refused", func(t *testing.T) {
		f := newProgramDirFixture(t, false)
		runner := f.newRunner(t, []tool.ProgramDirEntry{
			{Path: f.probeName, SHA256: "0000000000000000000000000000000000000000000000000000000000000000"},
		})
		_, err := runner.Run(context.Background(), core.Command{
			Executable: f.probe,
			Args:       []string{"-test.run=^$"},
			Dir:        f.root,
		})
		require.Error(t, err, "内容哈希不一致必须拒绝")
		require.True(t, core.IsSandboxViolation(err))
	})

	// 空清单 = 没有自动放行面，但**不整体拒绝**程序目录里的显式登记目标：本层不判
	// 清单之外的调用（授权面已按 `shell.allowed_commands` 决定过）。
	t.Run("empty_manifest_does_not_block_explicit_targets", func(t *testing.T) {
		f := newProgramDirFixture(t, false)
		runner := f.newRunner(t, nil)
		result, err := runner.Run(context.Background(), core.Command{
			Executable: f.probe,
			Args:       []string{"-test.run=^$"},
			Dir:        f.root,
			Env:        f.probeEnv,
		})
		require.NoError(t, err, "清单为空时本层不介入显式登记的目标：%s", result.Output)
	})
}

// TestExecutionProfile_CmdBatNotExempt 覆盖 D9：`.cmd` / `.bat` 是普通可执行件——不因扩展名
// 整体豁免，也不因扩展名整体拒绝；只认登记（白名单 / 安装清单）。
// Acceptance point (D9): batch files are admitted or refused by registration alone.
func TestExecutionProfile_CmdBatNotExempt(t *testing.T) {
	for _, profile := range []core.ExecutionProfile{core.ProfileStandard, core.ProfileStrict} {
		t.Run("unregistered_"+string(profile), func(t *testing.T) {
			f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
				// 同名的无扩展件登记了，带扩展名的没有——不得互相顶替。
				opts.AllowedCommands = []string{"tool"}
				opts.ProgramDirNames = []string{"tool"}
			})
			f.policy.SetExecutionProfile(profile)
			for _, target := range []string{"tool.cmd", "tool.bat", `C:\tools\tool.cmd`} {
				requireRefused(t, f.policy, shellAction(target), "可执行名不在白名单")
			}
			// 无扩展的同名件仍然可用：拒绝来自"没登记"，不是来自扩展名。
			requireAllowed(t, f.policy, shellAction("tool"))
		})
	}

	// 显式登记后可用（四要素登记）：扩展名不参与判定，登记整串才参与。
	t.Run("explicit_registration_admits", func(t *testing.T) {
		f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
			opts.AllowedCommands = []string{"tool.cmd"}
		})
		f.policy.SetExecutionProfile(core.ProfileStrict)
		requireAllowed(t, f.policy, shellAction("tool.cmd"))
		requireRefused(t, f.policy, shellAction("tool.bat"), "可执行名不在白名单")
	})
}
