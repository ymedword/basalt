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

// templateDSNEnv 是生成模板 `database.dsn` 引用的环境变量名；下面的用例断言模板正文
// 与它逐字一致——改名时两边一起改，不会留下"模板引用了没人注入的变量"的断线。
// templateDSNEnv is the variable the generated template references by name.
const templateDSNEnv = "BASALT_DATABASE_DSN"

// TestInstanceConfigTemplate_RegistersBundledCommands 判模板登记的就是安装器可能写入的
// 全部命令：随包条目（免凭据两条 + 凭据就绪后才写的 github）的 command 一律在内，且都是
// 绝对路径——白名单整串逐字比对，相对路径永远不命中。
func TestInstanceConfigTemplate_RegistersBundledCommands(t *testing.T) {
	// 模板把 DSN 写成 `${BASALT_DATABASE_DSN}`：装载期要求该变量存在，值只经环境提供。
	t.Setenv(templateDSNEnv, "postgres://template-fixture/basalt")
	runtimeRoot := t.TempDir()
	wsRoot := t.TempDir()
	text := instanceConfigTemplate(runtimeRoot, wsRoot)

	// ① 生成的正文必须能过配置装载：YAML 层面的故障（转义、缩进）在这里暴露，
	// 而不是等到首个进程启动失败。
	path := filepath.Join(t.TempDir(), configFileName)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	cfg, err := cli.LoadConfig(path, os.LookupEnv)
	require.NoError(t, err, "生成的配置必须能装载：\n%s", text)
	// 占位确实接到了变量上：运行期唯一存储是 PostgreSQL，这里断的是"接线"，不是值。
	require.Equal(t, "postgres://template-fixture/basalt", cfg.Database.DSN)

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
	t.Setenv(templateDSNEnv, "postgres://template-fixture/basalt")
	text := instanceConfigTemplate("", t.TempDir())
	require.NotContains(t, text, "allowed_commands",
		"取不到程序目录时不得写任何白名单：猜出来的路径会把别的目录里的组件登记进来")

	path := filepath.Join(t.TempDir(), configFileName)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	cfg, err := cli.LoadConfig(path, os.LookupEnv)
	require.NoError(t, err)
	require.Empty(t, cfg.Shell.AllowedCommands)
}

// TestInstanceConfigTemplate_RequiresDatabaseDSN 判占位不等于"无需准备数据库"：模板里
// `database.dsn` 只写 `${VAR}`，变量缺失时装载层当场拒绝并点名配置键与变量名——生成配置
// 的那个进程自己也不会带着一套缺省连接串偷偷启动，更不会回退 SQLite。
func TestInstanceConfigTemplate_RequiresDatabaseDSN(t *testing.T) {
	// t.Setenv 负责登记"测试结束后恢复原值"，紧接着删掉才是"变量缺失"这一读数：
	// 变量存在但为空是另一种读数（由要求 DSN 的子命令拒绝），两者不混判。
	t.Setenv(templateDSNEnv, "")
	require.NoError(t, os.Unsetenv(templateDSNEnv))
	text := instanceConfigTemplate("", t.TempDir())
	require.Contains(t, text, "${"+templateDSNEnv+"}",
		"模板必须逐字引用环境变量 %s：\n%s", templateDSNEnv, text)

	path := filepath.Join(t.TempDir(), configFileName)
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	_, err := cli.LoadConfig(path, os.LookupEnv)
	require.Error(t, err, "DSN 变量缺失时生成的配置不得被放行")
	require.Contains(t, err.Error(), "database.dsn")
	require.Contains(t, err.Error(), templateDSNEnv)
}
