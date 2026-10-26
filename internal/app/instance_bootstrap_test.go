package app

// 本文件判新实例配置模板的**命令白名单**（14D T117 现场发现的缺陷的回归面）：
// 全新安装时生成的配置若不登记随包运行时组件，内置 MCP 条目的绝对路径 command 会在
// spawn 之前被策略拒绝（`decision=deny reason=policy_denied`），开箱即用走不到
// "依赖就位"。两条断言各自对应一种真实故障：
//   - 模板登记的命令与安装器写入的条目**不同源** → 少一条即拒；多一条即越权放行；
//   - 生成正文不可装载（单引号里的 Windows 反斜杠被当成转义）→ 首个进程起不来。
//
// These tests pin the allowlist the fresh-instance template writes: same source as the
// installer's entries, and the generated body must load as-is.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/builtin"
	"github.com/ymedword/basalt/internal/cli"
)

// TestInstanceConfigTemplate_RegistersBundledCommands 判模板登记的就是安装器可能写入的
// 全部命令：随包条目（免凭据两条 + 凭据就绪后才写的 github）的 command 一律在内，且都是
// 绝对路径——白名单整串逐字比对，相对路径永远不命中。
func TestInstanceConfigTemplate_RegistersBundledCommands(t *testing.T) {
	runtimeRoot := t.TempDir()
	wsRoot := t.TempDir()
	text := instanceConfigTemplate(runtimeRoot, wsRoot)

	// ① 生成的正文必须能过配置装载：YAML 层面的故障（转义、缩进）在这里暴露，
	// 而不是等到首个进程启动失败。
	path := filepath.Join(t.TempDir(), configFileName)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	cfg, err := cli.LoadConfig(path, os.LookupEnv)
	require.NoError(t, err, "生成的配置必须能装载：\n%s", text)

	// ② 白名单与条目同源：安装器写什么命令，这里就登记什么。
	paths := builtinRuntimePaths(runtimeRoot, wsRoot)
	want := builtin.AllowlistedCommands(paths)
	require.Equal(t, want, cfg.Shell.AllowedCommands)
	require.NotEmpty(t, want)
	for _, entry := range builtin.CredentialFreeEntries(paths) {
		require.Contains(t, want, entry.Command,
			"免凭据条目 %s 的命令必须在白名单里，否则它会在 spawn 前被策略拒绝", entry.Name)
	}
	github := builtin.GitHubEntry(paths)
	require.Contains(t, want, github.Command,
		"github 条目凭据就绪后才写，但命令必须在首启就登记——否则补上凭据的那次启动仍然被拒")
	for _, command := range want {
		require.True(t, filepath.IsAbs(command), "白名单里的命令必须是绝对路径：%q", command)
	}
}

// TestInstanceConfigTemplate_NoRuntimeRootSkipsAllowlist 判"取不到程序目录"时的回落：
// 不猜路径、整节省略。没有白名单等于显式登记口径（fail-closed），不得退化成放行。
func TestInstanceConfigTemplate_NoRuntimeRootSkipsAllowlist(t *testing.T) {
	text := instanceConfigTemplate("", t.TempDir())
	require.NotContains(t, text, "allowed_commands",
		"取不到程序目录时不得写任何白名单：猜出来的路径会把别的目录里的组件登记进来")

	path := filepath.Join(t.TempDir(), configFileName)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	cfg, err := cli.LoadConfig(path, os.LookupEnv)
	require.NoError(t, err)
	require.Empty(t, cfg.Shell.AllowedCommands)
}
