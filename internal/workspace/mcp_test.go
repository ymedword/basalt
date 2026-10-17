package workspace_test

// 本文件是 US-7 配置解析面的验收用例（颗粒度文档 §3.2 的 workspace/mcp_test.go 十八例）。
// 全部经**导出**入口 `workspace.ParseMCPServers` 断言：`package workspace_test` 看不到
// 非导出实现（`parseMCPServers` / `mcpServerNames` / `isEnvPlaceholder`），因此两例以
// 行为等价的方式落地——名字序列与占位判据都从解析产出反推，**不新增导出测试 API**。
// Acceptance tests for the US-7 configuration parser. Everything goes through the
// exported ParseMCPServers entry point; the two cases that name unexported helpers
// assert the equivalent behaviour through the parser instead of adding test seams.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/workspace"
)

// TestParseMCPServers_MissingOrEmptyYieldsZero：文件缺失 / 空文件 / 纯注释 / `{}` /
// `servers: null` 五态一律零 server 零错误（FR-002；整段跳过由装配层负责）。
// Five zero-server shapes: missing, empty, comments-only, empty mapping, null servers.
func TestParseMCPServers_MissingOrEmptyYieldsZero(t *testing.T) {
	cases := []struct {
		name    string
		content string
		present bool
	}{
		{name: "缺失", present: false},
		{name: "空文件", present: true},
		{name: "纯注释", content: "# 只有注释\n# 没有 servers\n", present: true},
		{name: "空映射", content: "{}\n", present: true},
		{name: "servers 为 null", content: "servers: null\n", present: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.present {
				writeFileInRoot(t, root, "mcp_servers.yaml", tc.content)
			}
			specs, err := workspace.ParseMCPServers(root)
			require.NoError(t, err)
			require.Empty(t, specs)
		})
	}
}

// TestParseMCPServers_ListOrderPreserved：列表顺序原样保留（连接序 = 注册序的判据；
// 不排序、不建映射）。
// The servers list order is preserved verbatim — never sorted.
func TestParseMCPServers_ListOrderPreserved(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: zeta
    command: first-mcp
  - name: alpha
    command: second-mcp
  - name: mid
    command: third-mcp
`)
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 3)
	require.Equal(t, "zeta", specs[0].Name)
	require.Equal(t, "alpha", specs[1].Name)
	require.Equal(t, "mid", specs[2].Name)
}

// TestParseMCPServers_RejectsNonMappingRoot：顶层是列表等非映射形态 → 拒载并点名文件与
// 实际类型（FR-003）。
// A non-mapping document root is rejected with the file name and the actual type.
func TestParseMCPServers_RejectsNonMappingRoot(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", "- alpha\n- beta\n")
	specs, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Empty(t, specs)
	require.Contains(t, err.Error(), "mcp_servers.yaml 的顶层必须是映射")
	require.Contains(t, err.Error(), "实际是")
}

// TestParseMCPServers_RejectsServersNotList：`servers` 存在但不是列表 → 拒载。
// A non-list `servers` value is rejected.
func TestParseMCPServers_RejectsServersNotList(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", "servers: nope\n")
	specs, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Empty(t, specs)
	require.Contains(t, err.Error(), "servers 必须是列表")
	require.Contains(t, err.Error(), "实际是")
}

// TestParseMCPServers_RejectsEntryNotMapping：列表元素不是映射 → 拒载并点名下标。
// A non-mapping entry is rejected with its index.
func TestParseMCPServers_RejectsEntryNotMapping(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", "servers:\n  - just-a-string\n")
	specs, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Empty(t, specs)
	require.Contains(t, err.Error(), "servers[0] 必须是映射")
}

// TestParseMCPServers_RequiresNameAndCommand：缺 name / 缺 command / command 纯空白
// 三态一律拒载（FR-003）。
// Missing name, missing command, and a whitespace-only command are all rejected.
func TestParseMCPServers_RequiresNameAndCommand(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "缺 name",
			content: "servers:\n  - command: echo\n",
			want:    "servers[0] 缺少 name",
		},
		{
			name:    "缺 command",
			content: "servers:\n  - name: a\n",
			want:    "server a 缺少 command",
		},
		{
			name:    "command 纯空白",
			content: "servers:\n  - name: a\n    command: \"   \"\n",
			want:    "server a 缺少 command",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFileInRoot(t, root, "mcp_servers.yaml", tc.content)
			specs, err := workspace.ParseMCPServers(root)
			require.Error(t, err)
			require.Empty(t, specs)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestParseMCPServers_RejectsDuplicateNames：server 名重复 → 拒载并点名重复值。
// Duplicate server names are rejected with the duplicated name.
func TestParseMCPServers_RejectsDuplicateNames(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: dup
    command: first-mcp
  - name: dup
    command: second-mcp
`)
	specs, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Empty(t, specs)
	require.Contains(t, err.Error(), "server 名重复: dup")
}

// TestParseMCPServers_RejectsEnvNotMapping：`env` 存在但不是映射 → 拒载。
// A non-mapping `env` value is rejected.
func TestParseMCPServers_RejectsEnvNotMapping(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: a
    command: echo
    env: nope
`)
	specs, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Empty(t, specs)
	require.Contains(t, err.Error(), "server a 的 env 必须是映射")
}

// R1a —— env 值只允许精确 ${VAR}；明文 / 半截占位拒载；错误文案不回显配置值。
// 逆改点：把检查放宽成 HasPrefix("${") && HasSuffix("}")
//
//	 → 第二组（${A}${B}）由拒转收，红；
//	把 val 拼进错误文案 → 第三组 NotContains 红（明文凭证进日志即泄密）。
//
// R1a: plaintext env values and partial placeholders are rejected without echoing
// the configured value (the value may be the credential itself).
func TestParseMCPServers_RejectsPlaintextEnvWithoutEchoingValue(t *testing.T) {
	root := t.TempDir()
	secret := "ghp_plaintext_must_not_leak"
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: gh
    command: gh-mcp
    env:
      GITHUB_TOKEN: `+secret+`
`)
	_, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Contains(t, err.Error(), "gh", "错误必须点名 server")
	require.Contains(t, err.Error(), "env.GITHUB_TOKEN", "错误必须点名字段")
	require.NotContains(t, err.Error(), secret, "错误文案不得回显配置值")

	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: gh
    command: gh-mcp
    env:
      TOKEN: ${A}${B}
`)
	_, err = workspace.ParseMCPServers(root)
	require.Error(t, err, "非精确 ${VAR} 形态必须拒（无 HasPrefix/HasSuffix 放行口）")
	require.NotContains(t, err.Error(), "${A}${B}", "占位原文同样不回显")
}

// TestParseMCPServers_AcceptsExactPlaceholderOnly：精确 `${VAR}` 通过；半截 / 拼接 /
// 数字开头 / 空名一律拒（FR-004）。占位判据经解析产出反推（非导出 isEnvPlaceholder
// 不新增导出测试缝）。
// Only the exact ${VAR} placeholder form is accepted; every near-miss is rejected.
func TestParseMCPServers_AcceptsExactPlaceholderOnly(t *testing.T) {
	rejected := []string{"${A}${B}", "${A}x", "$A", "${}", "${1A}"}
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: ok
    command: echo
    env:
      TOKEN: ${A}
`)
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, "${A}", specs[0].Env["TOKEN"])

	for _, value := range rejected {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: bad
    command: echo
    env:
      TOKEN: "`+value+`"
`)
			specs, err := workspace.ParseMCPServers(root)
			require.Error(t, err)
			require.Empty(t, specs)
			require.Contains(t, err.Error(), "env.TOKEN")
			require.NotContains(t, err.Error(), value, "占位原文不得回显")
		})
	}
}

// TestParseMCPServers_TransportDefaultsToStdio：省略 `transport` 等价于 `stdio`；
// 显式 `stdio` 同样通过（已决 #1 的默认侧）。
// An absent transport defaults to stdio, and an explicit stdio passes too.
func TestParseMCPServers_TransportDefaultsToStdio(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: defaults
    command: first-mcp
  - name: explicit
    command: second-mcp
    transport: stdio
`)
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 2)
	require.Equal(t, "defaults", specs[0].Name)
	require.Equal(t, "explicit", specs[1].Name)

	// `transport: null` 与 env 同口径：字段无值视同缺省，不触发类型拒载。
	rootNull := t.TempDir()
	writeFileInRoot(t, rootNull, "mcp_servers.yaml", `
servers:
  - name: nulled
    command: null-mcp
    transport: null
`)
	nullSpecs, err := workspace.ParseMCPServers(rootNull)
	require.NoError(t, err)
	require.Len(t, nullSpecs, 1)
	require.Equal(t, "nulled", nullSpecs[0].Name)
}

// TestParseMCPServers_RejectsUnsupportedTransport：非 stdio 的 transport 一律拒载，
// 点名 server 与取值（已决 #1 方案 A）。
// A non-stdio transport is rejected fail-closed, naming the server and the value.
func TestParseMCPServers_RejectsUnsupportedTransport(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: web
    command: web-mcp
    transport: sse
`)
	specs, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Empty(t, specs)
	require.Contains(t, err.Error(), "server web 的 transport 不受支持（sse）")
	require.Contains(t, err.Error(), "v1 只支持 stdio")
}

// TestParseMCPServers_RejectsNonStringTransport：`transport` 字段一旦出现就必须是字符串。
// 非字符串（数字 / 布尔 / 列表 / 映射）不得经字符串强转变成空串、再按缺省 stdio 放行——
// 那会让非法配置悄悄获得执行含义。`null` 与 env 同口径视同缺省（见默认值用例）。
// A present transport must be a string; coercing non-strings to "" would silently grant
// the stdio default execution meaning to an invalid entry.
func TestParseMCPServers_RejectsNonStringTransport(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{name: "整数", value: "1"},
		{name: "布尔", value: "true"},
		{name: "空列表", value: "[]"},
		{name: "空映射", value: "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: web
    command: web-mcp
    transport: `+tc.value+`
`)
			specs, err := workspace.ParseMCPServers(root)
			require.Error(t, err, "非字符串 transport 必须拒载，不得按缺省 stdio 放行")
			require.Empty(t, specs)
			require.Contains(t, err.Error(), "server web")
			require.Contains(t, err.Error(), "transport")
		})
	}
}

// TestParseMCPServers_IgnoresUnknownEntryKeys：条目里的未知字段忽略（前向兼容，
// FR-001；不因多一个键拒载）。
// Unknown entry keys are ignored for forward compatibility.
func TestParseMCPServers_IgnoresUnknownEntryKeys(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: future
    command: future-mcp
    cwd: /tmp/somewhere
    args: [--fast]
    priority: 3
`)
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, "future", specs[0].Name)
	require.Equal(t, "future-mcp", specs[0].Command)
}

// TestParseMCPServers_EnvKeysPreserved：env 全部键按名保存，值为占位原文（解析层
// 只校验形态、不做任何值替换——替换归装配层 resolveMCPEnv）。
// Every env key is preserved verbatim with its placeholder value; resolution is not
// this layer's job.
func TestParseMCPServers_EnvKeysPreserved(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: gh
    command: gh-mcp
    env:
      GITHUB_TOKEN: ${GH_TOKEN}
      LOG_LEVEL: ${LOG_LEVEL}
`)
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, map[string]string{
		"GITHUB_TOKEN": "${GH_TOKEN}",
		"LOG_LEVEL":    "${LOG_LEVEL}",
	}, specs[0].Env)
}

// TestParseMCPServers_ReadsThroughRoot：读取经工作区根句柄（os.Root）——根内文件可读，
// 指向根外的软连接被挡下（FR-003 的越界判据）。
// Reads go through the workspace root handle: files inside are readable, while a
// symlink escaping the root is blocked.
func TestParseMCPServers_ReadsThroughRoot(t *testing.T) {
	t.Run("根内文件可读", func(t *testing.T) {
		root := t.TempDir()
		writeFileInRoot(t, root, "mcp_servers.yaml", "servers:\n  - name: local\n    command: echo\n")
		specs, err := workspace.ParseMCPServers(root)
		require.NoError(t, err)
		require.Len(t, specs, 1)
		require.Equal(t, "local", specs[0].Name)
	})

	t.Run("软连接越界被挡下", func(t *testing.T) {
		outside := t.TempDir()
		content := "servers:\n  - name: leaked\n    command: echo\n"
		require.NoError(t, os.WriteFile(filepath.Join(outside, "mcp_servers.yaml"), []byte(content), 0o644))
		root := t.TempDir()
		if err := os.Symlink(filepath.Join(outside, "mcp_servers.yaml"), filepath.Join(root, "mcp_servers.yaml")); err != nil {
			t.Skip("本机无法创建软连接（Windows 需开发者模式或管理员权限），越界判据待有权限环境复核")
		}
		specs, err := workspace.ParseMCPServers(root)
		require.Error(t, err)
		require.Empty(t, specs)
		require.Contains(t, err.Error(), "读取 mcp_servers.yaml")
	})
}

// TestParseMCPServers_ParseErrorIsSummarized：YAML 库文案不外传——错误里只有本仓的
// 定位摘要（`[行:列] 类别`），不含库的原始报文（§2.3 伪代码的 frontmatterErrorSummary）。
// Library wording stays internal: only the repository's own `[line:col] category`
// summary reaches the caller.
func TestParseMCPServers_ParseErrorIsSummarized(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", "servers: [unclosed\n")
	specs, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Empty(t, specs)
	require.Contains(t, err.Error(), "解析 mcp_servers.yaml: ")
	require.Contains(t, err.Error(), "YAML 语法错误")
	require.NotContains(t, err.Error(), "unexpected", "库的原始报文不得外传")
}

// TestMCPServerNames_PreservesConfigOrder：名字集与配置顺序一致（mcpServerNames 是
// 非导出实现，外部测试包经解析产出的 Name 序列做行为等价断言；装载器规则 4/6 依赖
// 该顺序报首个失败名）。
// The server-name sequence follows configuration order; asserted through the parsed
// configs because mcpServerNames is unexported.
func TestMCPServerNames_PreservesConfigOrder(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: third
    command: third-mcp
  - name: first
    command: first-mcp
  - name: second
    command: second-mcp
`)
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	require.Equal(t, []string{"third", "first", "second"}, names, "名字序列必须保持配置序")
}

// TestIsEnvPlaceholder_Table：占位判据的边界表（isEnvPlaceholder 非导出，经解析结果
// 反推：接受组解析成功、拒绝组报 env 字段错误）。
// Boundary table of the placeholder predicate, inferred from parser acceptance.
func TestIsEnvPlaceholder_Table(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{value: "${A}", want: true},
		{value: "${_A}", want: true},
		{value: "${A_1}", want: true},
		{value: "${aBc}", want: true},
		{value: "$A", want: false},
		{value: "${}", want: false},
		{value: "${1A}", want: false},
		{value: "${A}${B}", want: false},
		{value: "${A}x", want: false},
		{value: "x${A}", want: false},
		{value: "${A-B}", want: false},
		{value: " ${A}", want: false},
		{value: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			root := t.TempDir()
			writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: probe
    command: echo
    env:
      TOKEN: "`+tc.value+`"
`)
			specs, err := workspace.ParseMCPServers(root)
			if tc.want {
				require.NoError(t, err)
				require.Len(t, specs, 1)
				require.Equal(t, tc.value, specs[0].Env["TOKEN"])
				return
			}
			require.Error(t, err)
			require.Empty(t, specs)
			require.Contains(t, err.Error(), "env.TOKEN")
		})
	}
}
