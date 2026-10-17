package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// baseConfig 是一份最小可用配置：凭证走 ${DEMO_KEY} 占位（需求 §5.13）。
// baseConfig is a minimal valid configuration whose credential is a placeholder.
const baseConfig = `http:
  addr: "127.0.0.1:8080"
log:
  level: info
  format: json
providers:
  - name: demo
    base_url: http://${DEMO_HOST}:8080/v1
    api_key: ${DEMO_KEY}
    default_model: demo-model
budget:
  max_iterations: 8
`

// writeConfig 把配置内容落到临时工作区，返回文件路径（测试隔离纪律：不碰真实工作区）。
// writeConfig drops the configuration into a temp directory and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "basalt.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// envLookup 把固定表做成 lookupEnv 桩——测试不读真实环境变量（取值来源可断言）。
// envLookup turns a fixed map into a lookupEnv stub, so tests never read the real
// environment and can assert exactly where a value came from.
func envLookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// TestConfig_ResolvesEnvPlaceholders：正常值全部解析——取值来自注入的 lookupEnv；
// 凭证与非凭证键（部分占位）都解析。
func TestConfig_ResolvesEnvPlaceholders(t *testing.T) {
	path := writeConfig(t, baseConfig)

	cfg, err := cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}))
	require.NoError(t, err)

	require.Len(t, cfg.Providers, 1)
	require.Equal(t, "resolved-secret-value", cfg.Providers[0].APIKey)
	require.Equal(t, "http://api.internal:8080/v1", cfg.Providers[0].BaseURL)
	require.Equal(t, "demo", cfg.Providers[0].Name)
	require.Equal(t, "json", cfg.Log.Format)
	require.Equal(t, 8, cfg.Budget.MaxIterations)
}

// TestConfig_RejectsMissingEnvVar：变量不存在 → 点名配置键路径与变量名；lookupEnv
// 返回 (\"\", false) 的"空串替换"形态不能绕过拒绝。
func TestConfig_RejectsMissingEnvVar(t *testing.T) {
	path := writeConfig(t, baseConfig)

	// 只缺 DEMO_KEY（DEMO_HOST 在位）——断言点唯一，与多缺失时的报错顺序无关。
	_, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_HOST": "api.internal"}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "providers[0]")
	require.Contains(t, err.Error(), "DEMO_KEY")
}

// TestConfig_RejectsEmptyCredential：凭证键的变量**存在但为空** → 拒绝（Operations §6）。
func TestConfig_RejectsEmptyCredential(t *testing.T) {
	path := writeConfig(t, baseConfig)

	_, err := cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":  "",
		"DEMO_HOST": "api.internal",
	}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "providers[0].api_key")
	require.Contains(t, err.Error(), "不得为空")
}

// TestConfig_RejectsMalformedPlaceholder：模板畸形（未闭合的 `${`）→ 拒绝并点名键路径。
func TestConfig_RejectsMalformedPlaceholder(t *testing.T) {
	path := writeConfig(t, `providers:
  - name: demo
    api_key: ${DEMO_KEY
`)

	_, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": "resolved-secret-value"}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "providers[0].api_key")
	require.NotContains(t, err.Error(), "resolved-secret-value")
}

// TestConfig_RejectsPlaintextCredential：明文凭证 → 拒；**错误文案不得回显明文值**。
func TestConfig_RejectsPlaintextCredential(t *testing.T) {
	const plaintext = "plaintext-credential-value"
	path := writeConfig(t, `providers:
  - name: demo
    api_key: `+plaintext+`
`)

	_, err := cli.LoadConfig(path, envLookup(nil))
	require.Error(t, err)
	require.Contains(t, err.Error(), "providers[0].api_key")
	require.NotContains(t, err.Error(), plaintext, "错误文案不得回显明文凭证")
}

// TestConfig_RejectsSyntaxErrorWithoutEchoingSource：语法错误 → 拒绝，且错误文案
// 只含行列与类别。YAML 库的默认错误文本带源码摘录——凭证所在行的原文会被原样
// 带进 stdout / stderr / 日志（P1 回归：实测泄露）。
// A syntax error must not carry the source excerpt: the credential line would be
// echoed verbatim.
func TestConfig_RejectsSyntaxErrorWithoutEchoingSource(t *testing.T) {
	const secret = "sk-syntax-leak-probe-3f9a2b"
	path := writeConfig(t, `providers:
  - name: demo
    api_key: "`+secret+`
`)

	_, err := cli.LoadConfig(path, envLookup(nil))
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret, "语法错误文案不得带出配置文件原文")
	require.NotContains(t, err.Error(), "api_key", "不得回显源码行")
	require.Regexp(t, `\[\d+:\d+\]`, err.Error(), "行列信息必须保留（脱敏不等于丢诊断）")
}

// TestConfig_RejectsTypeErrorWithoutEchoingResolvedCredential：类型错误发生在
// ${VAR} 解析**之后**——库的源码摘录里是凭证的真实值；文案不得回显（P1 回归）。
// The type error happens after resolution, so the library would echo the real
// credential value.
func TestConfig_RejectsTypeErrorWithoutEchoingResolvedCredential(t *testing.T) {
	const secret = "sk-type-leak-probe-0987654321"
	path := writeConfig(t, `providers:
  - name: demo
    api_key: ${DEMO_KEY}
    default_model: [oops]
`)

	_, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": secret}))
	require.Error(t, err)
	require.Contains(t, err.Error(), "配置项类型非法")
	require.NotContains(t, err.Error(), secret, "解析后的凭证值不得出现在错误文案里")
	require.Regexp(t, `\[\d+:\d+\]`, err.Error(), "行列信息必须保留")
}

// TestConfig_UndefinedAliasDoesNotLeakSource：未定义别名的错误**首行**就带源文
// 本——实测 GetMessage() = `could not find alias "SENTINEL"`，且 err.Error() 还
// 附整段源码摘录；位置取自结构化 token、类别自产，库文案一个字不外传。
// P1 回归：旧实现把库文案首行拼进摘要，`api_key: *SENTINEL` 的 SENTINEL 在
// stdout、stderr 均命中。
func TestConfig_UndefinedAliasDoesNotLeakSource(t *testing.T) {
	const alias = "SENTINEL"
	path := writeConfig(t, `providers:
  - name: demo
    api_key: *`+alias+`
`)

	_, err := cli.LoadConfig(path, envLookup(nil))
	require.Error(t, err)
	require.NotContains(t, err.Error(), alias, "未定义别名的名字不得外传")
	require.NotContains(t, err.Error(), "could not find alias", "库文案不得外传")
	require.NotContains(t, err.Error(), "api_key", "不得回显源码行")
	require.Contains(t, err.Error(), "YAML 语法错误")
	require.Regexp(t, `\[\d+:\d+\]`, err.Error(), "行列信息必须保留（脱敏不等于丢诊断）")
}

// TestConfig_DuplicateMappingKeyDoesNotLeakSource：重复映射键走**同一条**库错误
// 形态——实测 Unmarshal 到 any 时是 SyntaxError 而**不是** DuplicateKeyError
// （yamlErrorCategory 里的 DuplicateKeyError 分支只对将来的严格解码路径生效），
// 库文案带所重复的键名与首次出现位置，同样不许外传。
// A duplicate mapping key surfaces as SyntaxError in this path; its message quotes
// the duplicated key and must not leave the package either.
func TestConfig_DuplicateMappingKeyDoesNotLeakSource(t *testing.T) {
	path := writeConfig(t, `providers:
  - name: demo
    api_key: ${A}
    api_key: ${B}
`)

	_, err := cli.LoadConfig(path, envLookup(map[string]string{"A": "a", "B": "b"}))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "api_key", "库文案带所重复的键名，不得外传")
	require.NotContains(t, err.Error(), "already defined", "库文案不得外传")
	require.Contains(t, err.Error(), "YAML 语法错误")
	require.Regexp(t, `\[\d+:\d+\]`, err.Error())
}

// TestConfig_RejectsNonStringCredential：凭证键的原始值不是字符串（数字 / 布尔 /
// 空值）→ 在**解码器上游**的类型关拒绝。数字与布尔能被解码器无损转成字符串
// （实测：`api_key: 123456789` 与 `api_key: true` 解析无错，树里是 int / bool），
// 若只在字符串分支做形态检查，二者会连占位检查一起绕过。
// P1 回归：旧实现在完整工作区下对这两份配置判 status exit 0（配置被接受）。
func TestConfig_RejectsNonStringCredential(t *testing.T) {
	cases := []struct {
		name     string
		value    string // YAML 字面量（空串 = 空标量 → null）
		noEcho   string // 错误文案里不得出现的原文；空 = 无可回显值
		wantType string
	}{
		{name: "数字", value: `123456789`, noEcho: "123456789", wantType: "int"},
		{name: "布尔", value: `true`, noEcho: "true", wantType: "bool"},
		{name: "空值（裸键）", value: ``, wantType: "空值"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "providers:\n  - name: demo\n    api_key: "+tc.value+"\n")

			_, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": "resolved-secret-value"}))
			require.Error(t, err, "非字符串凭证必须拒绝（解码器会把数字 / 布尔转成字符串）")
			require.Contains(t, err.Error(), "providers[0].api_key")
			require.Contains(t, err.Error(), "必须是 ${VAR} 形态的字符串")
			require.Contains(t, err.Error(), tc.wantType)
			if tc.noEcho != "" {
				require.NotContains(t, err.Error(), tc.noEcho, "不得回显原始值")
			}
		})
	}
}

// TestConfig_RejectsInvalidValues：必填缺失与类型非法一律拒绝——不静默取默认值。
func TestConfig_RejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "必填缺失：providers[0].name 空缺",
			content: `providers:
  - api_key: ${DEMO_KEY}
`,
			want: "providers[0].name",
		},
		{
			// 夹具随 US-2 补上 base_url：该字段自 US-2 起同为必填（前序改造点 1），
			// 不补则本子例命中的是 base_url 分支而不是它要判的 api_key 分支——
			// 判据不变，缺的只是"其余字段齐备"这个前提。
			name: "必填缺失：api_key 空缺",
			content: `providers:
  - name: demo
    base_url: https://api.example.com/v1
`,
			want: "providers[0].api_key",
		},
		{
			name: "取值域非法：log.level 不存在的档位",
			content: `log:
  level: verbose
`,
			want: "log.level",
		},
		{
			name: "类型非法：budget.max_iterations 写成字符串",
			content: `budget:
  max_iterations: "很多"
`,
			want: "配置项类型非法",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.content)

			_, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": "resolved-secret-value"}))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestConfig_RejectsDuplicateProviderName 前序改造点 1（第 7 类 fail-closed）：
// `providers[].name` 重名 → 装载期拒绝并**点名两条目下标**。
//
// 为什么这条必须是装载期错误：映射表的键就是条目名——重名会让 map 静默保留最后一条，
// 前面的条目从此不可达而**无人知晓**（症状是"配了却路由不过去"，而不是一条报错）。
// Duplicate names must fail at load time: the map key is the entry name, so a
// duplicate silently drops an entry and the operator never learns about it.
func TestConfig_RejectsDuplicateProviderName(t *testing.T) {
	path := writeConfig(t, `providers:
  - name: demo
    base_url: https://alpha.example.com/v1
    api_key: ${DEMO_KEY}
  - name: other
    base_url: https://other.example.com/v1
    api_key: ${DEMO_KEY}
  - name: demo
    base_url: https://beta.example.com/v1
    api_key: ${DEMO_KEY}
`)

	_, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": "resolved-secret-value"}))

	require.Error(t, err, "重名必须拒绝（否则映射表静默缺条目）")
	require.Contains(t, err.Error(), "providers[0]", "必须点名先出现的那条下标")
	require.Contains(t, err.Error(), "providers[2]", "必须点名后出现的那条下标")
	require.Contains(t, err.Error(), "demo", "必须点名重复的名字")
	require.NotContains(t, err.Error(), "resolved-secret-value", "不得回显凭证值")
}

// TestConfig_RejectsMissingProviderBaseURL 前序改造点 1：`base_url` 提为**必填**
// （无 base_url 无法构造客户端）。独立用例——不复用其他必填判据的断言。
func TestConfig_RejectsMissingProviderBaseURL(t *testing.T) {
	path := writeConfig(t, `providers:
  - name: demo
    api_key: ${DEMO_KEY}
`)

	_, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": "resolved-secret-value"}))

	require.Error(t, err, "缺 base_url 必须拒绝（客户端构造不出没有端点的供应商）")
	require.Contains(t, err.Error(), "providers[0].base_url", "必须点名键路径")
	require.Contains(t, err.Error(), "必填", "必须说明是必填项缺失")
	require.NotContains(t, err.Error(), "resolved-secret-value", "不得回显凭证值")
}

// TestConfig_RejectsProviderBaseURLWithUserinfoQueryFragment —— 第 8 类 fail-closed（§2.4 注 9）。
// 判决两个对象：① **拒绝**——含这些成分的地址不得生效（静默裁剪会让配置在运行时"变味"，
// 认证 / 路由错误由此变成难定位的故障）；② **文案纪律**——只点名键路径与成分名，不回显原 URL、
// 也不回显其中的值（userinfo 里可能正是一份凭证）。子用例各带一个"敏感片段"，逐例断言它
// 不出现在错误文案里（与 US-1 的"明文不回显"同口径）。
func TestConfig_RejectsProviderBaseURLWithUserinfoQueryFragment(t *testing.T) {
	// component 与用例名分开：后两个子例的**成分名**同为 `scheme`（缺 scheme 与
	// 非 http(s) 方案走同一条判定），但拒因不同、子例名要各说各的。
	// 子例覆盖面以颗粒度文档 §3.2 的行为准（userinfo / query / fragment / 缺 scheme；
	// 非 http(s) 方案同判；**空 fragment / 空 query 分隔符同拒**）——§4.3 的写出代码
	// 是摘录，只列了前三类。合法 `%23` 路径的正例是独立用例
	// （TestConfig_AcceptsEncodedHashInPath）。
	// The component is asserted separately from the subtest name because the last
	// two cases share the component `scheme` while being rejected for different
	// reasons; the empty-delimiter cases name the literal trailing `#` / `?`.
	cases := []struct {
		name      string
		baseURL   string
		component string
		sensitive string
	}{
		{"userinfo", "https://user-x9:secret-7f@api.example.com/v1", "userinfo", "secret-7f"},
		{"query", "https://api.example.com/v1?route=9f3a", "query", "route=9f3a"},
		{"fragment", "https://api.example.com/v1#alt-2b", "fragment", "alt-2b"},
		{"空 fragment（尾随 #）", "https://api.example.com/marker-a1f#", "fragment", "marker-a1f"},
		{"空 query（尾随 ?）", "https://api.example.com/marker-b2e?", "query", "marker-b2e"},
		{"缺 scheme", "api.example.com/v1", "scheme", "api.example.com"},
		{"非 http(s) 方案", "ftp://api.example.com/v1", "scheme", "ftp://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, providerConfigWithBaseURL(tc.baseURL)) // US-1 既有夹具
			_, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": "resolved-secret-value"}))

			require.Error(t, err, "含 %s 的 base_url 必须被拒绝", tc.name)
			require.Contains(t, err.Error(), "providers[0].base_url", "必须点名键路径")
			require.Contains(t, err.Error(), tc.component, "必须点出不支持的成分")
			require.NotContains(t, err.Error(), tc.baseURL, "不得回显原 URL")
			require.NotContains(t, err.Error(), tc.sensitive, "不得回显其中的值")
		})
	}
}

// TestConfig_AcceptsEncodedHashInPath —— 空 fragment 拒绝的**正例守卫**：被拒的是字面 `#`
// 分隔符（尾随空 `#` 会让拼接的端点整段落进 fragment）；路径里编码过的 `%23` 是合法成分，
// 不得连坐——且取值**原样保留**（不裁剪、不重编码，`%23` 不是 fragment 分隔符）。
// TestConfig_AcceptsEncodedHashInPath: the encoded `%23` stays legal and is preserved
// verbatim, so the empty-fragment rejection does not overshoot.
func TestConfig_AcceptsEncodedHashInPath(t *testing.T) {
	const baseURL = "https://api.example.com/v1%23suffix"
	path := writeConfig(t, providerConfigWithBaseURL(baseURL))

	cfg, err := cli.LoadConfig(path, envLookup(map[string]string{"DEMO_KEY": "resolved-secret-value"}))

	require.NoError(t, err, "%23 是合法编码 path，不得被 fragment 判定连坐")
	require.Equal(t, baseURL, cfg.Providers[0].BaseURL, "取值原样保留")
}

// providerConfigWithBaseURL 以 US-1 的 baseConfig 为底，只替换 base_url 一行——
// 子用例之间的唯一变量就是被拒的那个成分。
func providerConfigWithBaseURL(baseURL string) string {
	return strings.Replace(baseConfig, "base_url: http://${DEMO_HOST}:8080/v1", "base_url: "+baseURL, 1)
}

// allowlistSections 是 `sandbox` / `shell` / `http.allowed_domains` 三段的白名单形态。
// 五个键名逐字取自颗粒度文档 §3.5 第 4 项（US-4 的 T058 把它们从原始 map 搬进类型化
// 字段，**不改键名、不删键**）——写成字面量而不是引用常量：键名本身就是被测面，
// 引用同一份声明会让"两边一起改"也判成通过。
const allowlistSections = `http:
  addr: "127.0.0.1:8080"
  allowed_domains:
    - api.internal.example
    - Mirror.Example.COM
log:
  level: info
providers:
  - name: demo
    base_url: http://${DEMO_HOST}:8080/v1
    api_key: ${DEMO_KEY}
sandbox:
  cross_agent_read:
    - notes-agent
    - archive-agent
shell:
  allowed_commands:
    - git
    - go
  interpreters:
    - python
    - node
  env_passthrough:
    - PATH
    - HOME
`

// TestLoadConfig_ParsesAllowlistSections 三段白名单落进类型化字段：五个键名逐字沿用、
// 取值与顺序原样保留（`allowed_commands` 是整串逐字比对、域名大小写不敏感——重排或规范化
// 都会改变策略语义，装载层因此只搬运、不加工）。
// TestLoadConfig_ParsesAllowlistSections: the five allowlist keys land in typed fields
// verbatim, order included.
func TestLoadConfig_ParsesAllowlistSections(t *testing.T) {
	path := writeConfig(t, allowlistSections)

	cfg, err := cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}))
	require.NoError(t, err, "三段都是可选段，配了就该原样落地")

	require.Equal(t, []string{"api.internal.example", "Mirror.Example.COM"}, cfg.HTTP.AllowedDomains,
		"http.allowed_domains：大小写原样保留（不敏感判定归策略侧）")
	require.Equal(t, []string{"notes-agent", "archive-agent"}, cfg.Sandbox.CrossAgentRead,
		"sandbox.cross_agent_read：允许被跨 Agent 读取的 Agent 名清单")
	require.Equal(t, []string{"git", "go"}, cfg.Shell.AllowedCommands, "shell.allowed_commands")
	require.Equal(t, []string{"python", "node"}, cfg.Shell.Interpreters, "shell.interpreters")
	require.Equal(t, []string{"PATH", "HOME"}, cfg.Shell.EnvPassthrough, "shell.env_passthrough")
}

// TestLoadConfig_AllowlistSectionsDefaultToEmpty 缺省形态：三段一个键都不写 → 零值**空清单**
// （而不是报错）。空清单在策略侧的语义是"一个都不放行"——这在授权面是安全默认，在装载面
// 是"没配就不拦"，两处口径由消费者各自落实，装载层不得替它们补白名单。
// TestLoadConfig_AllowlistSectionsDefaultToEmpty: an absent section is an empty
// allowlist, which the policy side reads as deny-all.
func TestLoadConfig_AllowlistSectionsDefaultToEmpty(t *testing.T) {
	path := writeConfig(t, baseConfig)

	cfg, err := cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}))
	require.NoError(t, err, "可选段缺失不是错误")

	require.Empty(t, cfg.HTTP.AllowedDomains)
	require.Empty(t, cfg.Sandbox.CrossAgentRead)
	require.Empty(t, cfg.Shell.AllowedCommands)
	require.Empty(t, cfg.Shell.Interpreters)
	require.Empty(t, cfg.Shell.EnvPassthrough)
}

// TestLoadConfig_MemoryBackendDefaultsToMarkdown 缺省档 = markdown（约束 18）：**不写
// memory 段**与写了段但省略 backend 两条路径的读数**必须相同**——默认值只有一个来源，
// 哪条路径给都判同一个值。
//
// 不写 memory 段时断言的是**缺省值**而不是「零值恰好等于 markdown」：留空 → 归一到
// markdown 正是本用例判的东西（只判"没报错"的写法，把「宁可写成空字符串」的实现也放过）。
// 逆改点：把缺省改成空串或 sqlite → 两条子例的分区断言红。
func TestLoadConfig_MemoryBackendDefaultsToMarkdown(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{name: "不写 memory 段", tail: ""},
		{name: "写了段但省略 backend", tail: "memory:\n  backend: \"\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, baseConfig+tc.tail)
			cfg, err := cli.LoadConfig(path, envLookup(map[string]string{
				"DEMO_KEY":  "resolved-secret-value",
				"DEMO_HOST": "api.internal",
			}))
			require.NoError(t, err, "记忆段是可选段：不配 = 用缺省档，不是错误")
			require.Equal(t, "markdown", cfg.Memory.Backend,
				"缺省档 = markdown（约束 18；零新增配置键——backend 是既有键）")
		})
	}
}

// TestLoadConfig_AcceptsSqliteBackend 取值域内的另一档：`sqlite` 原样落地（装载层只
// 搬运、不加工——去哪找库文件是装配层的事，见 T036）；`${VAR}` 占位在**类型化之后**
// 仍照常解析（配置面的占位纪律对 memory 段不因类型化而失效）。
// 逆改点：把取值域写成只有 markdown 一种 → 显式 sqlite 被拒；类型化时漏掉占位解析路径
// → 占位档读出字面 `${...}`。
func TestLoadConfig_AcceptsSqliteBackend(t *testing.T) {
	path := writeConfig(t, baseConfig+"memory:\n  backend: sqlite\n")
	cfg, err := cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}))
	require.NoError(t, err, "sqlite 在取值域内（约束 18）")
	require.Equal(t, "sqlite", cfg.Memory.Backend)

	path = writeConfig(t, baseConfig+"memory:\n  backend: ${MEMORY_BACKEND}\n")
	cfg, err = cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":       "resolved-secret-value",
		"DEMO_HOST":      "api.internal",
		"MEMORY_BACKEND": "sqlite",
	}))
	require.NoError(t, err, "占位解析在类型化之后照常生效")
	require.Equal(t, "sqlite", cfg.Memory.Backend)
}

// TestLoadConfig_RejectsUnknownMemoryBackend 取值域外 fail-closed（约束 18）：`mem0`
// （接口预留、实现延后——需求 U-8）与一切未知取值**拒绝启动**，文案**点名键与取值**且
// 给出可执行下一步；**不得**静默降级到 markdown——那会让「我配了 mem0」悄悄变成
// 「记忆写进了文件」，载体形态与运维预期分叉。
// 逆改点：把 default 分支改成回落 markdown → 三态全红（无错误）。
func TestLoadConfig_RejectsUnknownMemoryBackend(t *testing.T) {
	for _, backend := range []string{"mem0", "postgres", "MEMORY.DEFAULT"} {
		t.Run(backend, func(t *testing.T) {
			path := writeConfig(t, baseConfig+"memory:\n  backend: "+backend+"\n")
			_, err := cli.LoadConfig(path, envLookup(map[string]string{
				"DEMO_KEY":  "resolved-secret-value",
				"DEMO_HOST": "api.internal",
			}))
			require.Error(t, err, "取值域外必须拒绝启动（不静默降级）：%s", backend)
			require.Contains(t, err.Error(), "memory.backend", "拒绝文案必须点名配置键：%s", backend)
			require.Contains(t, err.Error(), backend, "拒绝文案必须点名取值：%s", backend)
			require.Contains(t, err.Error(), "markdown", "文案给出取值域（可执行下一步）：%s", backend)
		})
	}
}
