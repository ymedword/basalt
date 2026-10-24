package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// baseConfig 是一份最小可用配置：凭证走 ${DEMO_KEY} 占位（需求文档 §5.2）。
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

// TestConfig_RejectsNegativeMaxConcurrentRuns 验收并发上限配置键落位（约束 31 /
// W14）：负数在装载期拒绝并点名 `server.max_concurrent_runs`；0 与缺省原样通过
// ——缺省 32 是 core 的唯一决定，装载层不预填、不改写。
// 逆改点：删掉负数分支 → 第一个子用例拿不到错误（红）；把 0 也拒掉 → 后两个子用例红。
func TestConfig_RejectsNegativeMaxConcurrentRuns(t *testing.T) {
	env := envLookup(map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	})

	t.Run("负值拒载并点名配置键", func(t *testing.T) {
		path := writeConfig(t, baseConfig+"server:\n  max_concurrent_runs: -1\n")
		_, err := cli.LoadConfig(path, env)
		require.Error(t, err, "负数必须拒绝启动（0 才是缺省）")
		require.Contains(t, err.Error(), "server.max_concurrent_runs", "拒绝文案必须点名配置键")
	})

	t.Run("零表示缺省放行", func(t *testing.T) {
		path := writeConfig(t, baseConfig+"server:\n  max_concurrent_runs: 0\n")
		cfg, err := cli.LoadConfig(path, env)
		require.NoError(t, err, "0 = 缺省 32，是合法取值")
		require.Zero(t, cfg.Server.MaxConcurrentRuns, "装载层不预填缺省——32 由 core 决定")
	})

	t.Run("不写 server 段放行", func(t *testing.T) {
		path := writeConfig(t, baseConfig)
		cfg, err := cli.LoadConfig(path, env)
		require.NoError(t, err, "server 段是可选段")
		require.Zero(t, cfg.Server.MaxConcurrentRuns)
	})
}

// configWithAdminAuth 在 baseConfig 的 `http` 段内插入 `admin_auth` 子段；adminAuth
// 的每行按 4 空格缩进写好。YAML 里同一映射键只能出现一次——追加第二个 `http:` 块
// 会被判重复映射键，夹具必须替换而不是拼接。
// configWithAdminAuth inserts the admin_auth block into the http section; a second
// http block would be a duplicate mapping key.
func configWithAdminAuth(adminAuth string) string {
	return strings.Replace(baseConfig,
		"http:\n  addr: \"127.0.0.1:8080\"\n",
		"http:\n  addr: \"127.0.0.1:8080\"\n  admin_auth:\n"+adminAuth+"\n", 1)
}

// adminAuthEnv 是 admin_auth 用例共用的环境桩：provider 两键必须在位，管理 token
// 由各子例自行决定放不放。测试不读真实环境。
// adminAuthEnv is the shared environment stub for the admin-auth cases.
func adminAuthEnv(extra map[string]string) func(string) (string, bool) {
	values := map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}
	for name, value := range extra {
		values[name] = value
	}
	return envLookup(values)
}

// TestConfig_AdminAuthTokenModeAccepted token 模式四象限的**合法**一象限（约束 2）：
// `${ADMIN_TOKEN}` 经环境变量解析后非空，五个键全部原样落地——加载层不派生、不补值。
// The valid quadrant: the placeholder resolves to a non-empty value and all five keys
// land verbatim.
func TestConfig_AdminAuthTokenModeAccepted(t *testing.T) {
	path := writeConfig(t, configWithAdminAuth(`
    mode: token
    token: ${ADMIN_TOKEN}
    allowed_hosts:
      - 127.0.0.1:8080
      - localhost:8080
      - "[::1]:8080"
    allowed_origins:
      - http://127.0.0.1:8080
      - https://console.example.com
    trusted_proxies:
      - 10.0.0.0/8
      - 127.0.0.1/32`))

	cfg, err := cli.LoadConfig(path, adminAuthEnv(map[string]string{"ADMIN_TOKEN": "resolved-admin-token"}))
	require.NoError(t, err, "合法 token 模式必须通过装载")

	require.Equal(t, "token", cfg.HTTP.AdminAuth.Mode)
	require.Equal(t, "resolved-admin-token", cfg.HTTP.AdminAuth.Token)
	require.Equal(t, []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"},
		cfg.HTTP.AdminAuth.AllowedHosts, "Host 白名单原样保留（大小写与端口不加工）")
	require.Equal(t, []string{"http://127.0.0.1:8080", "https://console.example.com"},
		cfg.HTTP.AdminAuth.AllowedOrigins, "Origin 白名单原样保留")
	require.Equal(t, []string{"10.0.0.0/8", "127.0.0.1/32"},
		cfg.HTTP.AdminAuth.TrustedProxies, "可信代理 CIDR 原样保留")
}

// TestConfig_AdminAuthSectionOptional 段整体缺失**不是**装载错误（注 12）：chat /
// tool / session 等命令不开放 HTTP 管理面；"serve 必须有 mode"由监听器创建后的
// web.ValidateAdminAuth 判定，装载层不替它拒绝整份配置文件。
// An entirely absent section is not a load error; serve enforces presence later.
func TestConfig_AdminAuthSectionOptional(t *testing.T) {
	path := writeConfig(t, baseConfig)

	cfg, err := cli.LoadConfig(path, adminAuthEnv(nil))
	require.NoError(t, err, "轻命令的配置不要求 admin_auth 段")

	require.Empty(t, cfg.HTTP.AdminAuth.Mode)
	require.Empty(t, cfg.HTTP.AdminAuth.Token)
	require.Empty(t, cfg.HTTP.AdminAuth.AllowedHosts)
	require.Empty(t, cfg.HTTP.AdminAuth.AllowedOrigins)
	require.Empty(t, cfg.HTTP.AdminAuth.TrustedProxies)
}

// TestConfig_AdminAuthRejectsMissingMode 半截认证配置：写了段内任何一键但 mode 为空
// → 装载期拒绝。文件里"看着配过"而实际没有模式，任何缺省解读都会让管理面裸奔。
// A partial section without a mode is refused: there is no safe default mode.
func TestConfig_AdminAuthRejectsMissingMode(t *testing.T) {
	path := writeConfig(t, configWithAdminAuth(`
    allowed_hosts:
      - 127.0.0.1:8080`))

	_, err := cli.LoadConfig(path, adminAuthEnv(nil))
	require.Error(t, err, "段内已配置其它键时 mode 不得留空")
	require.Contains(t, err.Error(), "http.admin_auth.mode", "拒绝文案必须点名配置键")
	require.Contains(t, err.Error(), "必填", "必须说明是必填项缺失")
}

// TestConfig_AdminAuthRejectsInvalidMode 取值域只有 token / local_trust（§9.1）：
// 其余取值（含大小写变体）拒绝启动并点名键与取值——不静默猜成某一档。
// Any mode outside the two literals fails closed, case variants included.
func TestConfig_AdminAuthRejectsInvalidMode(t *testing.T) {
	for _, mode := range []string{"basic", "TOKEN", "none"} {
		t.Run(mode, func(t *testing.T) {
			path := writeConfig(t, configWithAdminAuth("\n    mode: "+mode))

			_, err := cli.LoadConfig(path, adminAuthEnv(nil))
			require.Error(t, err, "取值域外必须拒绝：%s", mode)
			require.Contains(t, err.Error(), "http.admin_auth.mode", "拒绝文案必须点名配置键")
			require.Contains(t, err.Error(), mode, "拒绝文案必须点名取值")
			require.Contains(t, err.Error(), "token / local_trust", "文案给出取值域")
		})
	}
}

// TestConfig_AdminAuthRejectsInvalidTokens token 模式的其余三象限 + 键缺失：明文、
// 解析为空、变量不存在一律拒绝；明文与非字符串值的错误文案**不得回显原值**。
// credentialKey 覆盖本键，四条拒绝路径与 providers[i].api_key 同源。
// The remaining token quadrants: plaintext, empty expansion and an unresolved
// placeholder all fail closed, and no rejected value is echoed.
func TestConfig_AdminAuthRejectsInvalidTokens(t *testing.T) {
	const plaintext = "plaintext-admin-token-7c"
	cases := []struct {
		name   string
		token  string // `token:` 行（空 = 整键不写）
		env    map[string]string
		want   []string
		noEcho string
	}{
		{
			name:   "明文 token",
			token:  "    token: " + plaintext,
			want:   []string{"http.admin_auth.token", "明文凭证"},
			noEcho: plaintext,
		},
		{
			name:  "解析为空",
			token: "    token: ${ADMIN_TOKEN}",
			env:   map[string]string{"ADMIN_TOKEN": ""},
			want:  []string{"http.admin_auth.token", "不得为空"},
		},
		{
			name:  "变量不存在",
			token: "    token: ${ADMIN_TOKEN}",
			want:  []string{"http.admin_auth.token", "ADMIN_TOKEN"},
		},
		{
			name:   "非字符串值",
			token:  "    token: 123456",
			want:   []string{"http.admin_auth.token", "必须是 ${VAR} 形态的字符串", "int"},
			noEcho: "123456",
		},
		{
			name: "整键缺失",
			want: []string{"http.admin_auth.token", "必填"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snippet := "\n    mode: token"
			if tc.token != "" {
				snippet += "\n" + tc.token
			}
			path := writeConfig(t, configWithAdminAuth(snippet))

			_, err := cli.LoadConfig(path, adminAuthEnv(tc.env))
			require.Error(t, err, "非法 token 必须拒绝启动：%s", tc.name)
			for _, want := range tc.want {
				require.Contains(t, err.Error(), want)
			}
			if tc.noEcho != "" {
				require.NotContains(t, err.Error(), tc.noEcho, "错误文案不得回显被拒的 token 原值")
			}
		})
	}
}

// TestConfig_AdminAuthValidatesAllowedHosts Host 白名单形状（约束 4）：禁通配、禁
// userinfo、禁 scheme / 路径 / 查询、端口必填且为 1–65535 的整数——省略端口的条目
// 永远匹配不上带端口的 Host，必须拒绝。
// Host entries must carry a numeric port; wildcards and userinfo are refused.
func TestConfig_AdminAuthValidatesAllowedHosts(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  string
	}{
		{"通配符", "*", "通配符"},
		{"省略端口", "localhost", "缺端口"},
		{"IPv6 缺方括号", "::1:8080", "缺端口"},
		{"带 scheme", "http://127.0.0.1:8080", "只写 host:port"},
		{"带路径", "127.0.0.1:8080/path", "只写 host:port"},
		{"带 userinfo", "user@host:8080", "userinfo"},
		{"端口非数字", "127.0.0.1:http", "端口非法"},
		{"端口为零", "127.0.0.1:0", "端口非法"},
		{"端口越界", "127.0.0.1:70000", "端口非法"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, configWithAdminAuth(`
    mode: local_trust
    allowed_hosts:
      - "`+tc.entry+`"`))

			_, err := cli.LoadConfig(path, adminAuthEnv(nil))
			require.Error(t, err, "非法 Host 条目必须拒绝启动：%s", tc.name)
			require.Contains(t, err.Error(), "http.admin_auth.allowed_hosts", "拒绝文案必须点名配置键")
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestConfig_AdminAuthValidatesAllowedOrigins Origin 白名单形状（约束 5）：每项必须
// 是 `scheme://host[:port]`，`null`、路径、查询、fragment、userinfo、通配符一律拒绝
// ——带路径的条目永远匹配不上浏览器发来的 Origin。
// Origins must be exactly scheme://host[:port]; null, paths, queries and wildcards
// are all refused.
func TestConfig_AdminAuthValidatesAllowedOrigins(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		want   string
		noEcho string
	}{
		{"null", "null", "非法", ""},
		{"通配符", "http://*.example.com", "通配符", ""},
		{"裸星号", "*", "通配符", ""},
		{"尾斜杠", "http://127.0.0.1:8080/", "非法", ""},
		{"带路径", "http://127.0.0.1:8080/path", "非法", ""},
		{"带查询", "http://127.0.0.1:8080?x=1", "非法", ""},
		{"带 fragment", "http://127.0.0.1:8080#frag", "非法", ""},
		{"缺 scheme", "127.0.0.1:8080", "非法", ""},
		{"带 userinfo", "http://user:pw@127.0.0.1:8080", "userinfo", "pw"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, configWithAdminAuth(`
    mode: local_trust
    allowed_origins:
      - "`+tc.origin+`"`))

			_, err := cli.LoadConfig(path, adminAuthEnv(nil))
			require.Error(t, err, "非法 Origin 条目必须拒绝启动：%s", tc.name)
			require.Contains(t, err.Error(), "http.admin_auth.allowed_origins", "拒绝文案必须点名配置键")
			require.Contains(t, err.Error(), tc.want)
			if tc.noEcho != "" {
				require.NotContains(t, err.Error(), tc.noEcho, "userinfo 里的值不得回显")
			}
		})
	}
}

// TestConfig_AdminAuthValidatesTrustedProxies 可信代理只接受可解析 CIDR（约束 6）：
// 裸 IP、越界前缀与任意字符串拒绝；空数组合法（没有反向代理的直连部署）。
// Trusted proxies accept CIDRs only; a bare IP is refused rather than widened.
func TestConfig_AdminAuthValidatesTrustedProxies(t *testing.T) {
	t.Run("合法 CIDR 与空清单", func(t *testing.T) {
		path := writeConfig(t, configWithAdminAuth(`
    mode: local_trust
    trusted_proxies:
      - 10.0.0.0/8
      - 127.0.0.1/32
      - "::1/128"`))
		cfg, err := cli.LoadConfig(path, adminAuthEnv(nil))
		require.NoError(t, err)
		require.Equal(t, []string{"10.0.0.0/8", "127.0.0.1/32", "::1/128"},
			cfg.HTTP.AdminAuth.TrustedProxies)

		path = writeConfig(t, configWithAdminAuth("\n    mode: local_trust\n    trusted_proxies: []"))
		cfg, err = cli.LoadConfig(path, adminAuthEnv(nil))
		require.NoError(t, err, "空数组合法（直连部署）")
		require.Empty(t, cfg.HTTP.AdminAuth.TrustedProxies)
	})

	for _, entry := range []string{"10.0.0.1", "not-a-cidr", "300.0.0.0/8", "10.0.0.0/33"} {
		t.Run("拒绝 "+entry, func(t *testing.T) {
			path := writeConfig(t, configWithAdminAuth(`
    mode: local_trust
    trusted_proxies:
      - "`+entry+`"`))

			_, err := cli.LoadConfig(path, adminAuthEnv(nil))
			require.Error(t, err, "非法 CIDR 必须拒绝启动：%s", entry)
			require.Contains(t, err.Error(), "http.admin_auth.trusted_proxies", "拒绝文案必须点名配置键")
			require.Contains(t, err.Error(), "CIDR")
		})
	}
}

// authorConfig 在最小可用配置上追加 author 段——其余段与 baseConfig 一致，
// 子用例之间的唯一变量是 author 段内容。
// authorConfig appends an author section to the minimal valid configuration.
func authorConfig(section string) string {
	return baseConfig + "author:\n" + section
}

// TestConfig_ParsesAuthorSection —— T006 的装载集成：`author:` 段经
// ParseAuthorSection 落进类型化字段；省略 temperature 时取缺省 0.2。
// TestConfig_ParsesAuthorSection proves LoadConfig lands the typed section and applies
// the 0.2 default when temperature is omitted.
func TestConfig_ParsesAuthorSection(t *testing.T) {
	path := writeConfig(t, authorConfig("  provider: main\n  model: author-model\n  temperature: 1.5\n"))
	cfg, err := cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}))
	require.NoError(t, err)
	require.Equal(t, "main", cfg.Author.Provider)
	require.Equal(t, "author-model", cfg.Author.Model)
	require.Equal(t, 1.5, cfg.Author.Temperature)

	path = writeConfig(t, authorConfig("  provider: main\n  model: author-model\n"))
	cfg, err = cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}))
	require.NoError(t, err)
	require.Equal(t, 0.2, cfg.Author.Temperature, "省略 temperature 时取缺省 0.2")
}

// TestConfig_AuthorSectionAbsentDisablesFeature：不带 author 段的既有配置照常装载，
// 解析结果是零值——作者功能关闭，不是"缺省温度 0.2 已配置"。
// TestConfig_AuthorSectionAbsentDisablesFeature keeps existing configs loading and
// yields the zero value, which downstream reads as "feature off".
func TestConfig_AuthorSectionAbsentDisablesFeature(t *testing.T) {
	path := writeConfig(t, baseConfig)
	cfg, err := cli.LoadConfig(path, envLookup(map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}))
	require.NoError(t, err, "没有 author 段不得阻断启动")
	require.Empty(t, cfg.Author.Provider)
	require.Empty(t, cfg.Author.Model)
	require.Zero(t, cfg.Author.Temperature, "缺段必须返回零值（功能关闭），不填缺省温度")
}

// TestConfig_RejectsInvalidAuthorSection —— 段内错误在装载期拒绝：未知键、越界
// temperature、非映射段与类型错误都点名 author 键路径。
// TestConfig_RejectsInvalidAuthorSection surfaces section errors through LoadConfig.
func TestConfig_RejectsInvalidAuthorSection(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "未知键",
			content: "author:\n  modle: typo\n",
			want:    "未知键",
		},
		{
			name:    "temperature 越界",
			content: "author:\n  temperature: 2.5\n",
			want:    "author.temperature",
		},
		{
			name:    "非映射段",
			content: "author: oops\n",
			want:    "author",
		},
		{
			name:    "provider 类型错误",
			content: "author:\n  provider: 123\n",
			want:    "author.provider",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.content)
			_, err := cli.LoadConfig(path, envLookup(nil))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// demoEnv 是最小可用配置所需的两个环境变量（其余子用例共用）。
// demoEnv supplies the two variables the minimal valid configuration references.
func demoEnv() map[string]string {
	return map[string]string{
		"DEMO_KEY":  "resolved-secret-value",
		"DEMO_HOST": "api.internal",
	}
}

// secretsConfig 在最小可用配置上追加 secrets 段——子用例之间的唯一变量是段内容。
// secretsConfig appends a secrets section to the minimal valid configuration.
func secretsConfig(section string) string {
	return baseConfig + "secrets:\n" + section
}

// TestConfig_MasterKeyFileRequired —— T136：整段省略合法（纯 env 实例不需要主密钥）；
// 只要写了 `secrets:`（含空小节），就必须给出 `master_key_file`——写了段等于声明
// "这台实例托管凭据"，空路径会让部署者以为已配好，故障推到每次托管写入才暴露。
// TestConfig_MasterKeyFileRequired keeps an absent section legal while requiring the
// path as soon as the section is written at all.
func TestConfig_MasterKeyFileRequired(t *testing.T) {
	t.Run("整段省略", func(t *testing.T) {
		path := writeConfig(t, baseConfig)
		cfg, err := cli.LoadConfig(path, envLookup(demoEnv()))
		require.NoError(t, err, "纯 env 实例不因缺 secrets 段而失败")
		require.Empty(t, cfg.Secrets.MasterKeyFile, "缺段必须是零值，不是某个缺省路径")
	})

	t.Run("写了段但缺路径", func(t *testing.T) {
		for name, section := range map[string]string{
			"空小节":  "",
			"仅注释":  "  # 暂未配置\n",
			"空映射":  "  {}\n",
			"路径空串": "  master_key_file: \"\"\n",
		} {
			t.Run(name, func(t *testing.T) {
				path := writeConfig(t, secretsConfig(section))
				_, err := cli.LoadConfig(path, envLookup(demoEnv()))
				require.Error(t, err, "写了 secrets 段就必须给出路径")
				require.Contains(t, err.Error(), "secrets.master_key_file")
			})
		}
	})

	t.Run("给出工作区外绝对路径", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "keys", "master.key")
		path := writeConfig(t, secretsConfig("  master_key_file: '"+outside+"'\n"))
		cfg, err := cli.LoadConfig(path, envLookup(demoEnv()))
		require.NoError(t, err)
		require.Equal(t, outside, cfg.Secrets.MasterKeyFile)
	})
}

// TestConfig_MasterKeyInsideWorkspaceRejected —— T136：主密钥**不得**落在工作区内。
// 装载层按「配置文件同级 `.basalt/`」做词法判定（绝对与相对路径都换算到同一根）；
// 工作区外与跨盘路径放行。链接 / 权限与真实工作区根由 LoadMasterKey 运行期复核。
// TestConfig_MasterKeyInsideWorkspaceRejected walks the lexical boundary; runtime
// link and permission checks still belong to LoadMasterKey.
func TestConfig_MasterKeyInsideWorkspaceRejected(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		keyPath string
	}{
		{name: "绝对路径落在工作区内", keyPath: filepath.Join(dir, ".basalt", "master.key")},
		{name: "相对路径落在工作区内", keyPath: ".basalt/master.key"},
		{name: "路径就是工作区根", keyPath: filepath.Join(dir, ".basalt")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "basalt.yaml")
			require.NoError(t, os.WriteFile(path,
				[]byte(secretsConfig("  master_key_file: '"+tc.keyPath+"'\n")), 0o600))
			_, err := cli.LoadConfig(path, envLookup(demoEnv()))
			require.Error(t, err)
			require.Contains(t, err.Error(), "工作区内")
		})
	}

	t.Run("工作区外的同级目录", func(t *testing.T) {
		path := filepath.Join(dir, "basalt.yaml")
		outside := filepath.Join(dir, "keys", "master.key")
		require.NoError(t, os.WriteFile(path,
			[]byte(secretsConfig("  master_key_file: '"+outside+"'\n")), 0o600))
		cfg, err := cli.LoadConfig(path, envLookup(demoEnv()))
		require.NoError(t, err, "工作区外的路径不得被连坐拒绝")
		require.Equal(t, outside, cfg.Secrets.MasterKeyFile)
	})
}

// TestConfig_ManagedProviderNeedsNoAPIKey —— R4.8：服务端在定义文件里写
// `credential_mode: managed` 且**没有** `api_key` 行；装载层必须接受自己写出的形态，
// 否则下一次启动就拒绝装载本实例的定义文件（实例改由管理台托管凭据后无法重启）。
// TestConfig_ManagedProviderNeedsNoAPIKey accepts the exact shape the server writes.
func TestConfig_ManagedProviderNeedsNoAPIKey(t *testing.T) {
	path := writeConfig(t, `http:
  addr: "127.0.0.1:8080"
providers:
  - name: vault
    base_url: https://api.example.com/v1
    credential_mode: managed
    default_model: vault-model
`)

	cfg, err := cli.LoadConfig(path, envLookup(nil))
	require.NoError(t, err, "managed 条目没有 api_key 是契约形态，不是缺项")
	require.Equal(t, "managed", cfg.Providers[0].CredentialMode, "模式字段原样落地")
	require.Empty(t, cfg.Providers[0].APIKey, "managed 条目不得带 api_key")
}

// TestConfig_ProviderCredentialModeContradictions —— 两模式的矛盾形态逐条失败关闭：
// env（含省略）缺占位、managed 又写 api_key、未知模式字面量。
// TestConfig_ProviderCredentialModeContradictions fails closed on every contradictory
// shape across the two modes.
func TestConfig_ProviderCredentialModeContradictions(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  string
	}{
		{
			name:  "env 缺 api_key",
			entry: "  - name: demo\n    base_url: https://api.example.com/v1\n    default_model: m\n",
			want:  "providers[0].api_key 缺失",
		},
		{
			name: "managed 带 api_key",
			entry: "  - name: demo\n    base_url: https://api.example.com/v1\n" +
				"    credential_mode: managed\n    api_key: ${DEMO_KEY}\n    default_model: m\n",
			want: "providers[0].api_key 在 credential_mode: managed 下不得出现",
		},
		{
			name: "未知模式字面量",
			entry: "  - name: demo\n    base_url: https://api.example.com/v1\n" +
				"    credential_mode: plain\n    default_model: m\n",
			want: "providers[0].credential_mode 取值非法",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, "http:\n  addr: \"127.0.0.1:8080\"\nproviders:\n"+tc.entry)

			_, err := cli.LoadConfig(path, envLookup(demoEnv()))

			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want, "必须点名键路径与规则")
			require.NotContains(t, err.Error(), "resolved-secret-value", "不得回显解析后的凭证值")
		})
	}
}
