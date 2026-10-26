package tool_test

// 本文件是执行档位（14D）的 D 系列判据之一：D7 交互式 shell 名称集合。
// 判据面（颗粒度文档 §4.3 D7）：`zsh` / `dash` / `ksh` / `fish` / `csh` / `tcsh` /
// `busybox` / `powershell` / `pwsh` / `cmd` 的裸名、完整路径与带可执行后缀形式一律拒；
// `Policy`（字面）与 `Runner`（`EvalSymlinks` 后的真实 basename）两层都判；软链接改名
// 同样拒，且与执行档位无关（两档一致）。软链接子句单列在
// `TestExecutionProfile_ShellNameSymlink`——创建软链接需要平台能力，跳过时不得连带
// 拖红同族的必跑判据。
// D7: the interactive-shell name ceiling binds both profiles, both layers, and survives
// a symlink rename because the runner judges the resolved real basename.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// newShellNameList 是 D7 点名的新增十项（`bash` / `sh` 另由既有的 F13 用例覆盖）。
// The ten names introduced by 14D; bash and sh have their own older regression (F13).
var newShellNames = []string{
	"zsh", "dash", "ksh", "fish", "csh", "tcsh", "busybox", "powershell", "pwsh", "cmd",
}

// TestExecutionProfile_ShellNameSet 覆盖 D7：两档都拒、字面与真实路径两层都判、
// 软链接改名同样拒，且**任何允许分支都不能把它请回来**（白名单 / 解释器通道 /
// 程序目录三条口子逐个试）。
// Acceptance point (D7): both profiles refuse, both layers judge, and no allow branch
// can bring an interactive shell back.
func TestExecutionProfile_ShellNameSet(t *testing.T) {
	// ① 授权面（`WhitelistPolicy.Check`）：两档 × 三种写法，且把该名字**同时**放进
	// 白名单、解释器与程序目录清单——能力上限先于一切允许分支，配置错误不能变成放行。
	// Policy face: the ceiling precedes every allow branch, so a misconfigured allowlist
	// still cannot admit a shell name.
	for _, profile := range []core.ExecutionProfile{core.ProfileStandard, core.ProfileStrict} {
		t.Run("policy_"+string(profile), func(t *testing.T) {
			f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
				opts.AllowedCommands = append([]string{}, newShellNames...)
				opts.Interpreters = append([]string{}, newShellNames...)
				opts.ProgramDirNames = append([]string{}, newShellNames...)
			})
			f.policy.SetExecutionProfile(profile)
			for _, name := range newShellNames {
				for _, spelling := range []string{name, "/usr/bin/" + name, name + ".exe"} {
					requireRefused(t, f.policy, shellAction(spelling), "交互式 shell 不得放行")
				}
			}
		})
	}

	// ② 执行面（`LocalRunner.Run`）：即便授权面按字面放行（软链接在字面上是另一个
	// 名字），执行面也要在**起进程之前**拒。
	// Execution face: refuse before spawning, even when literal authorization passed.
	t.Run("runner_refuses_before_spawn", func(t *testing.T) {
		shellPath := hostShellPath(t)
		runner := tool.NewLocalRunner(tool.RunnerOptions{Root: t.TempDir()})
		for _, target := range []string{hostShellName(), shellPath} {
			_, err := runner.Run(context.Background(), core.Command{Executable: target, Dir: t.TempDir()})
			require.Error(t, err, "执行侧必须在起进程之前拒绝：%s", target)
			require.True(t, core.IsSandboxViolation(err), "拒绝必须是 *core.SandboxViolationError")
			require.Contains(t, err.Error(), "交互式 shell 不得放行")
		}
	})
}

// TestExecutionProfile_ShellNameSymlink 是 D7 的软链接改名子句：`helper -> <真实 shell
// 路径>`。授权面看 `helper` 是白名单里的名字（放行），执行面投影到真实路径后 basename
// 仍是 shell —— 必须拒。
//
// 为什么单列成顶层用例而不是 `TestExecutionProfile_ShellNameSet` 的子测试：创建软链接
// 需要平台能力（Windows 上是 SeCreateSymbolicLinkPrivilege，即开发者模式或管理员），
// 没有该能力的机器上本用例只能跳过。验收执行器把"必跑用例的子测试被跳过"判为失败
// （`scripts/acceptance/execute.go` 的 `checkTests`），子测试跳过会连带把同族的
// 两档 / 两层判据一起判红；顶层用例跳过则如实记在报告的 `skipped_tests` 里——判据
// 面不减，未验证一眼可见。
// A top-level test, not a subtest: the symlink clause is platform-gated, and the check
// runner fails any required test whose subtest skipped — which would take the siblings
// down with it. A top-level skip stays visible in the report's skipped_tests.
func TestExecutionProfile_ShellNameSymlink(t *testing.T) {
	dir := t.TempDir()
	realShell := filepath.Join(dir, hostShellName())
	require.NoError(t, os.WriteFile(realShell, []byte("占位内容\n"), 0o700))
	link := filepath.Join(dir, "helper")
	if err := os.Symlink(realShell, link); err != nil {
		// Windows 上创建软链接需要 SeCreateSymbolicLinkPrivilege（开发者模式或管理员）；
		// 没有权限时不能伪造结论，如实标跳过并写明理由。
		// Windows needs a privilege to create symlinks; without it report a skip.
		t.Skipf("平台拒绝创建软链接（Windows 需开发者模式或管理员权限）：%v", err)
	}

	f := newPolicyFixture(t, func(opts *tool.PolicyOptions) {
		opts.AllowedCommands = []string{"helper"}
	})
	f.policy.SetExecutionProfile(core.ProfileStandard)
	requireAllowed(t, f.policy, shellAction("helper"))

	runner := tool.NewLocalRunner(tool.RunnerOptions{Root: t.TempDir()})
	_, err := runner.Run(context.Background(), core.Command{Executable: link, Dir: dir})
	require.Error(t, err, "软链接改名不得绕过 shell 名称上限")
	require.True(t, core.IsSandboxViolation(err))
	require.Contains(t, err.Error(), "交互式 shell 不得放行")
}

// hostShellName 返回本机**确实存在**的 shell 名称：用真实存在的可执行件做用例，
// 拒绝才有意义（否则"未找到"与"被拒"分不开）。
// A shell name that really exists here, so a refusal cannot be confused with not-found.
func hostShellName() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}

// hostShellPath 把本机 shell 解析成完整路径（完整路径写法同样必须被拒）。
func hostShellPath(t *testing.T) string {
	t.Helper()
	name := hostShellName()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path, err := exec.LookPath(name)
	require.NoError(t, err, "本机必须能找到 %s，用例才有判别力", name)
	require.True(t, strings.HasPrefix(path, string(filepath.Separator)) || filepath.IsAbs(path),
		"解析结果必须是绝对路径：%s", path)
	return path
}
