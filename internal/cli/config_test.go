package cli_test

import (
	"os"
	"path/filepath"
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
			name: "必填缺失：api_key 空缺",
			content: `providers:
  - name: demo
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
