package tool

// 本文件是执行档位（14D）D4 的判据：文件写面的禁区在**两档**都不放宽，读面也不随档位
// 扩大。
//
// 判据口径（模块卡 §4.4 的 D4）：程序目录、主密钥路径、`.basalt/` 定义文件（`AGENT.md`
// 与受保护控制目录）写入均拒绝；读面不随档位扩大（工作区根文件仍不可读）。
//
// 为什么主密钥与程序目录落在**工作区之外**：主密钥不得放在工作区内（纪律见
// `InstanceSecretsService`），程序目录是安装清单里的只读运行时——两者本就不在任何
// Agent 的可写范围里，因此由同一条"写入目标超出可写范围"的规则挡下；本文件把这条
// 结论钉住，避免哪天给工作区开个旁路把它们顺带放进去。
// The master key must live outside the workspace and the program directory is read-only
// runtime content, so both are refused by the generic "outside the writable root" rule.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// d14Workspace 是 D4 判据用的工作区骨架：`root` 是工作区根（即 `.basalt`），`outside`
// 是它的上级目录（主密钥与程序目录住在这里）。
// d14Workspace lays out the workspace skeleton plus the two out-of-tree protected paths.
type d14Workspace struct {
	root    string
	outside string
}

// newD14Workspace 造骨架并返回：agents/demo-agent/{scripts,notes}、agents/other-agent、
// skills/、memory/、control/，以及一个工作区根文件与工作区外的两点。
// newD14Workspace builds the skeleton used by the D4 judgement.
func newD14Workspace(t *testing.T) d14Workspace {
	t.Helper()
	outside := t.TempDir()
	root := filepath.Join(outside, ".basalt")
	for _, rel := range []string{
		filepath.Join("agents", "demo-agent", "scripts"),
		filepath.Join("agents", "demo-agent", "notes"),
		filepath.Join("agents", "other-agent"),
		"skills",
		"memory",
		controlDirName,
		filepath.Join("runtime", "prog"),
	} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, rel), 0o750))
	}
	for _, rel := range []string{
		filepath.Join("agents", "demo-agent", "AGENT.md"),
		filepath.Join("agents", "other-agent", "AGENT.md"),
		filepath.Join("skills", "SKILL.md"),
		filepath.Join("memory", "MEMORY.md"),
		filepath.Join(controlDirName, "state.json"),
		"README.md",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte("占位内容\n"), 0o644))
	}
	// 工作区外的两条受保护路径：主密钥（同级文件）与程序目录（同级目录）。
	require.NoError(t, os.WriteFile(filepath.Join(outside, "master.key"), []byte("key\n"), 0o600))
	return d14Workspace{root: root, outside: outside}
}

// writeTarget 把绝对路径回填为策略可判的**绝对**目标（主密钥与程序目录在工作区外，
// 只能用绝对路径表达）。
// writeTarget builds an absolute write action.
func writeTarget(path string) core.Action {
	return core.Action{Type: core.ActionFileWrite, Target: path, Agent: "demo-agent"}
}

// readTarget 造一条读动作。
// readTarget builds a read action.
func readTarget(path string) core.Action {
	return core.Action{Type: core.ActionFileRead, Target: path, Agent: "demo-agent"}
}

// TestExecutionProfile_FileWriteDenied 覆盖 D4：两档的文件写禁区逐条拒绝、读面不随档位
// 扩大；自有目录内的普通写入仍放行（证明禁区不是"什么都拒"）。
// Acceptance point (D4): every protected write is refused in both profiles and the read
// face stays the same width; an ordinary write inside the agent's own directory is admitted.
func TestExecutionProfile_FileWriteDenied(t *testing.T) {
	workspace := newD14Workspace(t)
	for _, profile := range []core.ExecutionProfile{core.ProfileStandard, core.ProfileStrict} {
		t.Run(string(profile), func(t *testing.T) {
			policy := NewWhitelistPolicy(PolicyOptions{Root: workspace.root})
			require.True(t, policy.SetExecutionProfile(profile))

			t.Run("own_dir_write_allowed", func(t *testing.T) {
				requirePolicyAllowed(t, policy, writeTarget(
					filepath.Join("agents", "demo-agent", "notes", "out.txt")))
			})

			// `.basalt/` 定义文件：两个 Agent 的 AGENT.md 都拒（保护面在自有目录之前判）。
			for _, rel := range []string{
				filepath.Join("agents", "demo-agent", "AGENT.md"),
				filepath.Join("agents", "other-agent", "AGENT.md"),
			} {
				requirePolicyRefused(t, policy, writeTarget(rel), "AGENT.md 是受保护文件，写入被拒绝")
			}

			// 控制目录：读写**同一句**规则（禁止区的说法不随工具换）。
			requirePolicyRefused(t, policy, writeTarget(filepath.Join(controlDirName, "state.json")),
				"受保护控制目录不在文件工具的可访问范围")
			requirePolicyRefused(t, policy, readTarget(filepath.Join(controlDirName, "state.json")),
				"受保护控制目录不在文件工具的可访问范围")

			// 脚本、技能库、记忆目录：各自的禁区规则。
			requirePolicyRefused(t, policy,
				writeTarget(filepath.Join("agents", "demo-agent", "scripts", "run.sh")),
				"scripts/ 是只读区，写入被拒绝")
			requirePolicyRefused(t, policy, writeTarget(filepath.Join("skills", "SKILL.md")),
				"技能库不可写入")
			requirePolicyRefused(t, policy, writeTarget(filepath.Join("memory", "MEMORY.md")),
				"记忆目录不在可写范围")

			// 程序目录与主密钥：都在工作区之外，由"超出可写范围"统一挡下。
			requirePolicyRefused(t, policy,
				writeTarget(filepath.Join(workspace.outside, "runtime", "prog", "agent-tool")),
				"写入目标超出当前 Agent 的可写范围")
			requirePolicyRefused(t, policy, writeTarget(filepath.Join(workspace.outside, "master.key")),
				"写入目标超出当前 Agent 的可写范围")

			// 读面不随档位扩大：工作区根文件仍不可读（两档同一结论）。
			requirePolicyRefused(t, policy, readTarget("README.md"),
				"读取目标不在可读根之内")
			// 自有目录内的读不受影响（否则上面那条可能只是"读什么都拒"）。
			requirePolicyAllowed(t, policy,
				readTarget(filepath.Join("agents", "demo-agent", "AGENT.md")))
		})
	}
}
