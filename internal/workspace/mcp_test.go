package workspace_test

// 本文件是 US-7 配置解析面的验收用例（颗粒度文档 §3.2 的 workspace/mcp_test.go 十八例）。
// 全部经**导出**入口 `workspace.ParseMCPServers` 断言：`package workspace_test` 看不到
// 非导出实现（`parseMCPServers` / `mcpServerNames` / `isEnvPlaceholder`），因此两例以
// 行为等价的方式落地——名字序列与占位判据都从解析产出反推，**不新增导出测试 API**。
// Acceptance tests for the US-7 configuration parser. Everything goes through the
// exported ParseMCPServers entry point; the two cases that name unexported helpers
// assert the equivalent behaviour through the parser instead of adding test seams.

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
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

// TestParseMCPServers_RejectsUnsupportedTransport：stdio / http 之外的 transport 一律
// 拒载，点名 server 与取值（已决 #1 方案 A；US-12 约束 8 起 http 受支持，判据从
// 「只支持 stdio」改为「只支持 stdio / http」）。
// Any transport outside stdio / http is rejected fail-closed, naming the server and
// the value. Since US-12 (constraint 8) http is supported too, so the fixed phrase
// names both transports.
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
	require.Contains(t, err.Error(), "只支持 stdio / http")
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

// TestParseMCPServers_CandidateMatrix：严格候选的传输分支矩阵（约束 8）——stdio 必须
// 有单可执行名 command、不得有 url；http 必须有 http(s) 且无 userinfo 的 url、不得有
// command / args；transport 必须显式；合法候选返回规范化配置。
// Strict candidate matrix: transport must be explicit, stdio requires a command and
// forbids a url, http requires a url and forbids command/args.
func TestParseMCPServers_CandidateMatrix(t *testing.T) {
	cases := []struct {
		name    string
		cfg     core.MCPServerConfig
		wantErr string
		want    core.MCPServerConfig
	}{
		{
			name: "stdio 合法（显式 transport + 单可执行名 + args）",
			cfg: core.MCPServerConfig{
				Name: "local", Transport: "stdio", Command: "npx", Args: []string{"-y", "@scope/server"},
			},
			want: core.MCPServerConfig{
				Name: "local", Transport: "stdio", Command: "npx", Args: []string{"-y", "@scope/server"},
			},
		},
		{
			name: "http 合法（url 无 userinfo）",
			cfg: core.MCPServerConfig{
				Name: "remote", Transport: "http", URL: "https://mcp.example.com/v1",
				Env: map[string]string{"MCP_TOKEN": "${MCP_TOKEN}"},
			},
			want: core.MCPServerConfig{
				Name: "remote", Transport: "http", URL: "https://mcp.example.com/v1",
				Env: map[string]string{"MCP_TOKEN": "${MCP_TOKEN}"},
			},
		},
		{name: "缺 transport", cfg: core.MCPServerConfig{Name: "x", Command: "npx"}, wantErr: "必须显式给出 transport"},
		{name: "缺 name", cfg: core.MCPServerConfig{Transport: "stdio", Command: "npx"}, wantErr: "name 必填"},
		{name: "未知 transport", cfg: core.MCPServerConfig{Name: "x", Transport: "sse", URL: "https://a.example"}, wantErr: "transport 不受支持"},
		{name: "stdio 缺 command", cfg: core.MCPServerConfig{Name: "x", Transport: "stdio"}, wantErr: "缺少 command"},
		{name: "stdio 带 url", cfg: core.MCPServerConfig{Name: "x", Transport: "stdio", Command: "npx", URL: "https://a.example"}, wantErr: "不得给出 url"},
		{name: "http 缺 url", cfg: core.MCPServerConfig{Name: "x", Transport: "http"}, wantErr: "缺少 url"},
		{name: "http 带 command", cfg: core.MCPServerConfig{Name: "x", Transport: "http", URL: "https://a.example", Command: "npx"}, wantErr: "不得给出 command"},
		{name: "http 带 args", cfg: core.MCPServerConfig{Name: "x", Transport: "http", URL: "https://a.example", Args: []string{"-y"}}, wantErr: "不得给出 args"},
		{name: "http 非 http(s) scheme", cfg: core.MCPServerConfig{Name: "x", Transport: "http", URL: "ftp://a.example"}, wantErr: "http / https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := workspace.ValidateMCPServerCandidate(tc.cfg)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestParseMCPServers_RejectsUserinfoAndPlaintextTokenArgs：http url 带 userinfo、
// args 以明文承载凭证（`--flag=value` 与 `--flag value` 两种形态）一律拒绝；
// 错误文案不回显取值；`${VAR}` 占位形式通过（约束 8 / 23）。
// Userinfo URLs and plaintext credential args are rejected without echoing values;
// exact ${VAR} placeholders pass.
func TestParseMCPServers_RejectsUserinfoAndPlaintextTokenArgs(t *testing.T) {
	secret := "sk-live-must-not-leak"

	_, err := workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "u", Transport: "http", URL: "https://user:" + secret + "@mcp.example.com",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "url")
	require.NotContains(t, err.Error(), secret, "错误文案不得回显 URL 里的凭证")

	_, err = workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "t", Transport: "stdio", Command: "mcp", Args: []string{"--token=" + secret},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "args[0]")
	require.NotContains(t, err.Error(), secret, "错误文案不得回显参数取值")

	_, err = workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "t2", Transport: "stdio", Command: "mcp", Args: []string{"--token", secret},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "args[0]")
	require.NotContains(t, err.Error(), secret)

	// 14D 判据修订（设计 §2.3.2 args 口径表）：参数里的 `${VAR}` 不再解析——argv 对
	// 同机进程可读，托管值进 argv 等于泄露。新增 / 修改条目一律拒绝并点名参数序号；
	// 存量条目仍可加载（兼容路径），由诊断报 `args_placeholders_not_resolved`。
	// 期望集合是**扩大**：非凭证参数照旧通过，两条占位参数从"通过"改为"拒绝"。
	for _, args := range [][]string{
		{"--token=${MCP_TOKEN}"},
		{"--token", "${MCP_TOKEN}"},
	} {
		_, err := workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
			Name: "no", Transport: "stdio", Command: "mcp", Args: args,
		})
		require.Error(t, err, "14D 起新增条目的 args 不得含 ${VAR} 占位：%v", args)
		require.Contains(t, err.Error(), "args[")
		require.NotContains(t, err.Error(), "MCP_TOKEN", "错误文案只点名序号，不回显占位符内容")
	}
	_, err = workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "ok", Transport: "stdio", Command: "mcp", Args: []string{"--config=plain-value", "--verbose"},
	})
	require.NoError(t, err, "非凭证参数必须照旧通过")
}

// TestParseMCPServers_LegacyCommandSplitWarns：既有文件的旧格式多词 command 按
// strings.Fields 拆分（与旧 tool 层行为逐字等价），并记一次 WARN 提示迁移到 args；
// 兼容拆分不得泄漏完整 command（可能夹带凭证）。
// Legacy multi-word commands are split exactly like strings.Fields and warn once
// about migrating to args; the full command is never logged.
func TestParseMCPServers_LegacyCommandSplitWarns(t *testing.T) {
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	legacy := "npx  -y   @scope/server"
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: legacy
    command: "`+legacy+`"
`)
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, "stdio", specs[0].Transport)
	require.Equal(t, strings.Fields(legacy), append([]string{specs[0].Command}, specs[0].Args...),
		"拆分结果必须与旧 strings.Fields 逐字等价")

	logged := logBuf.String()
	require.Contains(t, logged, "WARN")
	require.Contains(t, logged, "拆分")
	require.Contains(t, logged, "args")
	require.Contains(t, logged, "legacy", "WARN 必须点名 server")
	require.NotContains(t, logged, legacy, "不得整体回显旧 command")
}

// TestParseMCPServers_LegacyAmbiguousRejected：既有文件同时给出 args 与多词 command
// → 拒绝（只允许一种写法，约束 8）；严格候选同样拒绝。
// A file entry carrying both args and a multi-word command is rejected; the strict
// candidate path rejects it too.
func TestParseMCPServers_LegacyAmbiguousRejected(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: ambiguous
    command: "npx -y server"
    args: ["--fast"]
`)
	_, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Contains(t, err.Error(), "同时给出 args 与多词 command")

	_, err = workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "ambiguous", Transport: "stdio", Command: "npx -y server", Args: []string{"--fast"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "同时给出 args 与多词 command")
}

// TestValidateMCPServerCandidate_AbsolutePathWithSpaces：绝对路径里的空格是**路径的
// 一部分**，不是参数分隔——开箱设计要求程序目录可以含空格（设计 §4.1 第 1 步），而内置
// 条目的命令恒为 `<程序目录>/runtime/...` 的绝对路径。拆分只认**正面证据**（整串是文件
// 或首词是文件），路径不存在时按整串保留：依赖缺失必须是 connected=false 的可见状态
// （启动隔离），不能在定义校验阶段变成启动失败。
// Spaces inside an absolute path belong to the path. Splitting needs positive evidence,
// so a missing component cannot turn into a validation error.
func TestValidateMCPServerCandidate_AbsolutePathWithSpaces(t *testing.T) {
	command := filepath.Join(t.TempDir(), "Program Files", "Basalt 0.1.0", "runtime",
		"mcp", "docs-readonly", "1.0.0", "docs-readonly")
	require.Contains(t, command, " ", "用例路径必须含空格")
	if _, err := os.Stat(command); err == nil {
		t.Fatalf("该用例要判「不依赖存在性」，路径不应存在：%s", command)
	}

	cfg, err := workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "docs-readonly", Transport: "stdio", Command: command,
	})
	require.NoError(t, err)
	require.Equal(t, command, cfg.Command, "绝对路径必须整串保留，不得按空白拆分")
	require.Empty(t, cfg.Args)

	// 带 args 同样成立：dbhub 就是「node 绝对路径 + 三个参数」的形态。
	withArgs, err := workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "dbhub", Transport: "stdio", Command: command,
		Args: []string{"--transport", "stdio"},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"--transport", "stdio"}, withArgs.Args)

	// 非绝对路径的多词 command 仍按原规则拒绝（约束 8 / 47）：放行的只是绝对路径。
	_, err = workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "glued", Transport: "stdio", Command: "runtime/my tools/npx -y server",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "必须是单个可执行名")
}

// TestParseMCPServers_LegacyAbsoluteMultiWordStillSplit：旧格式「整行命令写在 command
// 里」的**读取兼容**不因绝对路径豁免而丢（约束 8 / Operations §5.4）——首词是已存在的
// 可执行文件、整串不是文件时，读取路径按空白拆成 command + args。缺这条，存量绝对路径
// 条目会从"能连"变成"策略拒绝"，而那正是本缺陷要修的反面。
// The legacy command line still splits on the read path when the leading token is a
// real file and the whole string is not.
func TestParseMCPServers_LegacyAbsoluteMultiWordStillSplit(t *testing.T) {
	root := t.TempDir()
	// 旧格式表达不了"含空格的可执行路径 + 参数"（按空白一切两半就分不清），
	// 这种形态必须用 args 字段——因此这里的首词不带空格。
	helper := filepath.Join(root, "helper.exe")
	require.NoError(t, os.WriteFile(helper, []byte{}, 0o600))
	doc := "servers:\n  - name: helper\n    command: '" + helper + " -test.run=^TestMCPHelperProcess$'\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "mcp_servers.yaml"), []byte(doc), 0o600))

	servers, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, servers, 1)
	require.Equal(t, helper, servers[0].Command, "首词（含空格的绝对路径）是 command")
	require.Equal(t, []string{"-test.run=^TestMCPHelperProcess$"}, servers[0].Args)
}

// TestParseMCPServers_SpaceyAbsolutePathNotSplit：整串是文件时**不拆**——解压到
// `Program Files` 的程序目录就是这一形态；拆了会把 `C:\Program` 当可执行文件。
// A whole-string file is a path with spaces and must not be split.
func TestParseMCPServers_SpaceyAbsolutePathNotSplit(t *testing.T) {
	root := t.TempDir()
	programDir := filepath.Join(root, "Program Files", "Basalt 0.1.0", "runtime", "mcp")
	require.NoError(t, os.MkdirAll(programDir, 0o750))
	bundled := filepath.Join(programDir, "docs-readonly.exe")
	require.NoError(t, os.WriteFile(bundled, []byte{}, 0o600))
	doc := "servers:\n  - name: docs-readonly\n    command: '" + bundled + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "mcp_servers.yaml"), []byte(doc), 0o600))

	servers, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, servers, 1)
	require.Equal(t, bundled, servers[0].Command, "整串是文件时必须原样保留")
	require.Empty(t, servers[0].Args)
}

// TestParseMCPServers_StrictMultiWordCommandRejected：候选（严格路径）的多词 command
// 直接 400 类错误并指引用 args 字段，不做兼容拆分（约束 47 ②）。
// Strict candidates reject a multi-word command instead of splitting it and point
// the operator at the args field.
func TestParseMCPServers_StrictMultiWordCommandRejected(t *testing.T) {
	_, err := workspace.ValidateMCPServerCandidate(core.MCPServerConfig{
		Name: "legacy", Transport: "stdio", Command: "npx -y server",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "单个可执行名")
	require.Contains(t, err.Error(), "args 字段")
}

// TestParseMCPServers_TransportRequiredForCandidate：候选必须显式给 transport；
// 既有文件缺省仍按 stdio 读入（两条路径的唯一语义差异）。
// Candidates must state the transport explicitly; existing files still default to
// stdio — the only semantic difference between the two paths.
func TestParseMCPServers_TransportRequiredForCandidate(t *testing.T) {
	_, err := workspace.ValidateMCPServerCandidate(core.MCPServerConfig{Name: "x", Command: "npx"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "必须显式给出 transport")

	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", "servers:\n  - name: legacy\n    command: npx\n")
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, "stdio", specs[0].Transport, "既有文件缺省 transport 按 stdio 读入")
}

// TestParseMCPServers_ExtendedFieldsRoundTrip：扩展字段逐项保留——stdio 的 args 顺序、
// http 的 url、两种传输的 env 占位原文（解析层不做值替换）。
// The extended fields survive verbatim: args order, url, and env placeholders.
func TestParseMCPServers_ExtendedFieldsRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: local
    transport: stdio
    command: uvx
    args: ["--from", "pkg", "server"]
    env:
      MCP_TOKEN: ${MCP_TOKEN}
  - name: remote
    transport: http
    url: https://mcp.example.com/v1
    env:
      REMOTE_TOKEN: ${REMOTE_TOKEN}
`)
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 2)

	require.Equal(t, "local", specs[0].Name)
	require.Equal(t, "stdio", specs[0].Transport)
	require.Equal(t, "uvx", specs[0].Command)
	require.Equal(t, []string{"--from", "pkg", "server"}, specs[0].Args)
	require.Equal(t, map[string]string{"MCP_TOKEN": "${MCP_TOKEN}"}, specs[0].Env)

	require.Equal(t, "remote", specs[1].Name)
	require.Equal(t, "http", specs[1].Transport)
	require.Empty(t, specs[1].Command)
	require.Empty(t, specs[1].Args)
	require.Equal(t, "https://mcp.example.com/v1", specs[1].URL)
	require.Equal(t, map[string]string{"REMOTE_TOKEN": "${REMOTE_TOKEN}"}, specs[1].Env)
}

// TestParseMCPServerBlock_AcceptsBothStoredShapes：已发布版本的 `definition_yaml` 有
// 两种已落库形态——管理写路径的块式列表片段（带 `- ` 指示符）与基线 / 对账路径的
// 顶层条目映射；两者都必须解析回同一条配置（图 C 的"读取 published 版本"）。
// Both stored shapes — the block item written by the admin path and the mapping form
// written by baseline — parse back into the same configuration.
func TestParseMCPServerBlock_AcceptsBothStoredShapes(t *testing.T) {
	blockForm := []byte("  - name: local\n    transport: stdio\n    command: uvx\n    args:\n      - --from\n      - pkg\n")
	cfg, err := workspace.ParseMCPServerBlock(blockForm)
	require.NoError(t, err)
	require.Equal(t, core.MCPServerConfig{
		Name: "local", Transport: "stdio", Command: "uvx", Args: []string{"--from", "pkg"},
	}, cfg)

	mappingForm := []byte("name: remote\ntransport: http\nurl: https://mcp.example.com/v1\nenv:\n  REMOTE_TOKEN: ${REMOTE_TOKEN}\n")
	cfg, err = workspace.ParseMCPServerBlock(mappingForm)
	require.NoError(t, err)
	require.Equal(t, "remote", cfg.Name)
	require.Equal(t, "http", cfg.Transport)
	require.Equal(t, "https://mcp.example.com/v1", cfg.URL)
	require.Equal(t, map[string]string{"REMOTE_TOKEN": "${REMOTE_TOKEN}"}, cfg.Env)
}

// TestParseMCPServerBlock_RejectsNonSingleShape：多条目列表、标量与非法传输都被拒
// （定义行只承载**一条**配置；一条版本行里出现两个 server 是存储分叉，不猜选）。
// 缺省 transport 不在此列：既有文件的兼容路径按 stdio 读入（约束 8 / 47）。
// Multiple entries, scalars and invalid transports are refused; a missing transport
// stays accepted because the legacy reading path defaults it to stdio.
func TestParseMCPServerBlock_RejectsNonSingleShape(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "两个条目", raw: "  - name: a\n    transport: http\n    url: https://a.example\n  - name: b\n    transport: http\n    url: https://b.example\n"},
		{name: "标量", raw: "just-a-string\n"},
		{name: "空值", raw: "null\n"},
		{name: "非法传输", raw: "name: a\ntransport: grpc\ncommand: uvx\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := workspace.ParseMCPServerBlock([]byte(tc.raw))
			require.ErrorIs(t, err, workspace.ErrInvalidDefinition)
		})
	}

	legacy, err := workspace.ParseMCPServerBlock([]byte("name: a\ncommand: uvx\n"))
	require.NoError(t, err)
	require.Equal(t, "stdio", legacy.Transport)
}

// TestParseMCPServerBlock_LegacyMultiWordCommandSplits：存储字节可能来自旧格式文件，
// 多词 command 按兼容路径拆分（约束 47②：编辑前的既有条目在发布 / 重启路径照旧可用）。
// A stored legacy multi-word command still splits on the reading path.
func TestParseMCPServerBlock_LegacyMultiWordCommandSplits(t *testing.T) {
	cfg, err := workspace.ParseMCPServerBlock([]byte("name: legacy\ntransport: stdio\ncommand: \"npx -y server\"\n"))
	require.NoError(t, err)
	require.Equal(t, "npx", cfg.Command)
	require.Equal(t, []string{"-y", "server"}, cfg.Args)
}

// TestRenderMCPServerEntry_DeterministicAndRoundTrip：渲染键序固定（name → transport →
// command / url → args → env），env 键按升序，输出能被文件解析器原样读回同一条配置；
// 严格候选规则在渲染前生效（多词 command → 错误、零写入）。
// Rendering is deterministic, sorted, round-trips through the file parser, and applies
// the strict candidate rules before producing any bytes.
func TestRenderMCPServerEntry_DeterministicAndRoundTrip(t *testing.T) {
	stdio := core.MCPServerConfig{
		Name:      "local",
		Transport: "stdio",
		Command:   "uvx",
		Args:      []string{"--from", "pkg", "server"},
		Env:       map[string]string{"B_TOKEN": "${B_TOKEN}", "A_TOKEN": "${A_TOKEN}"},
	}
	rendered, err := workspace.RenderMCPServerEntry(stdio)
	require.NoError(t, err)
	want := "  - name: local\n    transport: stdio\n    command: uvx\n    args:\n" +
		"      - \"--from\"\n      - pkg\n      - server\n" +
		"    env:\n      A_TOKEN: ${A_TOKEN}\n      B_TOKEN: ${B_TOKEN}\n"
	require.Equal(t, want, string(rendered))

	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", "servers:\n"+string(rendered))
	specs, err := workspace.ParseMCPServers(root)
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, stdio, specs[0])

	httpCfg := core.MCPServerConfig{Name: "remote", Transport: "http", URL: "https://mcp.example.com/v1"}
	rendered, err = workspace.RenderMCPServerEntry(httpCfg)
	require.NoError(t, err)
	require.Equal(t, "  - name: remote\n    transport: http\n    url: https://mcp.example.com/v1\n", string(rendered))

	_, err = workspace.RenderMCPServerEntry(core.MCPServerConfig{
		Name: "legacy", Transport: "stdio", Command: "npx -y server",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "args")
}

// TestParseMCPServers_DuplicateNamesAcrossTransports：重名判定与传输无关——一个 stdio
// 与一个 http 同名同样拒载（名字是 server 的唯一标识）。
// Duplicate names are rejected regardless of transport.
func TestParseMCPServers_DuplicateNamesAcrossTransports(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: dup
    transport: stdio
    command: local-mcp
  - name: dup
    transport: http
    url: https://mcp.example.com
`)
	_, err := workspace.ParseMCPServers(root)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server 名重复: dup")
}
