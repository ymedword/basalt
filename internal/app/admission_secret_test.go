package app

// 本文件是 US-21 凭证扫描的验收面（颗粒度文档 §3.1.7、§4.3 具名用例）：
//
//	S1 `TestUS21_Secrets_NotEchoed`——已知前缀 / PEM / Authorization / 通用赋值逐类命中；
//	   `${ENV_VAR}` 占位与普通文本不命中；扫描只返回去重后的文件路径，不返回命中文本
//	S2 `TestUS21_Errors_DoNotEchoSecrets`——分类错误的对外文案与包装链只含固定哨兵文本，
//	   不含凭证样本；`errors.Is` 穿透包装（T011 起由 Propose 路径沿用同一断言）
//
// 这两个用例是 FR-21-14 的负向判据：实现若把命中文本拼进返回值或错误文案，用例即红。

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// admissionSecretSamples 是每类模式一条命中样本；样本值本身仅用于断言"未回显"。
// AWS 与 Slack 两类在**运行时拼出**（前缀与填充分段书写）：字面量会被
// `TestCredentialLeak_GoldenFixtures` 判成"示例长成可用凭据"，与本测试要造命中样本的
// 目的相反——两处纪律互斥，只有运行时拼接能同时满足。
// admissionSecretSamples carries one hit per pattern family; the values are only used
// to assert they never appear in returned paths or error text. The AWS and Slack samples
// are joined at run time so the source itself never matches a live credential shape.
var admissionSecretSamples = []string{
	`sk-live-abcdefghijklmnopqrstuvwx`,
	"AKIA" + strings.Repeat("Q7", 8),
	`ghp_abcdefghijklmnopqrstuvwxyz01`,
	`github_pat_abcdefghijklmnopqrstuv`,
	"xoxb-" + strings.Repeat("1234567890", 3),
	"-----BEGIN RSA PRIVATE KEY-----",
	"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9abcdefghij",
	`api_key = "abcdefghijklmnopqrstuvwxyz"`,
	`secret: "abcdefghijklmnop1234"`,
	`token=abcdefghijklmnopqrstuvwx`,
	`password: "supersecretpassword123"`,
}

// admissionSecretPlaceholders 是项目约定的占位写法，必须全部放行。
// admissionSecretPlaceholders are the documented placeholders and must never hit.
var admissionSecretPlaceholders = []string{
	`api_key = "${OPENAI_API_KEY}"`,
	`password: ${DB_PASSWORD}`,
	`token=${GITHUB_TOKEN}`,
	`secret: "${APP_SECRET}"`,
}

// TestUS21_Secrets_NotEchoed 覆盖逐类命中、占位放行与"只回路径"三条判据。
// TestUS21_Secrets_NotEchoed covers each pattern family, placeholder exemption and the
// paths-only contract.
func TestUS21_Secrets_NotEchoed(t *testing.T) {
	t.Parallel()
	for _, sample := range admissionSecretSamples {
		require.Truef(t, core.TextHasSecret(sample), "样本必须命中：%q", sample)
	}
	for _, placeholder := range admissionSecretPlaceholders {
		require.Falsef(t, core.TextHasSecret(placeholder), "占位不得命中：%q", placeholder)
	}
	for _, benign := range []string{
		"技能说明：本技能把日报汇总为 Markdown。",
		"password rotation is documented in the operations guide.",
		"token budget: 4096",
		"",
	} {
		require.Falsef(t, core.TextHasSecret(benign), "普通文本不得命中：%q", benign)
	}

	files := []core.AuthorFile{
		{Path: "SKILL.md", Content: "普通技能正文。"},
		{Path: "notes/usage.md", Content: admissionSecretSamples[0]},
		{Path: "SKILL.md", Content: "同名文件第二次出现，不应重复上报。"},
		{Path: "notes/usage.md", Content: admissionSecretSamples[6]},
		{Path: "config/example.yaml", Content: admissionSecretPlaceholders[0]},
		{Path: "scripts/run.sh", Content: admissionSecretSamples[7]},
	}
	hits := core.ScanFilesForSecrets(files)
	require.Equal(t, []string{"notes/usage.md", "scripts/run.sh"}, hits)
	for _, hit := range hits {
		for _, sample := range admissionSecretSamples {
			require.NotContains(t, hit, sample, "返回值只能是路径，不得回显命中文本")
		}
	}

	clean := core.AdmissionSource{
		Kind: "user", Ref: "本地手写", Notes: "从运维手册整理",
		Evidence: []string{"tests/demo.md"}, Counterexamples: []string{"不适用"},
		Limitations: []string{"覆盖率有限"}, RetentionUntil: "2027-01-01T00:00:00Z",
	}
	require.False(t, core.SourceHasSecret(clean))
	for name, mutate := range map[string]func(*core.AdmissionSource){
		"ref":             func(s *core.AdmissionSource) { s.Ref = admissionSecretSamples[1] },
		"notes":           func(s *core.AdmissionSource) { s.Notes = admissionSecretSamples[2] },
		"evidence":        func(s *core.AdmissionSource) { s.Evidence = []string{admissionSecretSamples[3]} },
		"counterexamples": func(s *core.AdmissionSource) { s.Counterexamples = []string{admissionSecretSamples[4]} },
		"limitations":     func(s *core.AdmissionSource) { s.Limitations = []string{admissionSecretSamples[8]} },
		"placeholder":     func(s *core.AdmissionSource) { s.Notes = admissionSecretPlaceholders[0] },
	} {
		declared := clean
		mutate(&declared)
		require.Equal(t, name != "placeholder", core.SourceHasSecret(declared), "来源字段 %s 判定不符", name)
	}
}

// TestUS21_Errors_DoNotEchoSecrets 确认分类错误对外只呈现固定哨兵文本，且包装不携带样本。
// TestUS21_Errors_DoNotEchoSecrets pins the fixed sentinel text and its wrapping chain.
func TestUS21_Errors_DoNotEchoSecrets(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("提议失败：%w", core.ErrAdmissionSecretDetected)
	require.ErrorIs(t, wrapped, core.ErrAdmissionSecretDetected)
	for _, sample := range admissionSecretSamples {
		require.NotContains(t, wrapped.Error(), sample, "错误文案不得回显凭证")
	}
	require.False(t, errors.Is(wrapped, core.ErrAdmissionInvalidInput))
	require.False(t, strings.Contains(wrapped.Error(), "PRIVATE KEY"))
}

// TestUS22_SecretScanner_SharedWithAdmission 守 C-12：抽取侧的凭证判定与 US-21 准入
// 扫描器是**同一份实现**——对同一组命中 / 占位 / 普通文本样本必须逐字同判；任何一侧
// 复制规则并各自演化，都会在边界样本（占位放行、通用赋值长度）上先漂移。
// Extraction reuses the admission scanner: both sides must classify every sample the
// same way. A second rule copy drifts first on the placeholder and length-boundary cases.
func TestUS22_SecretScanner_SharedWithAdmission(t *testing.T) {
	t.Parallel()
	samples := append([]string{}, admissionSecretSamples...)
	samples = append(samples, admissionSecretPlaceholders...)
	samples = append(samples,
		"技能说明：本技能把日报汇总为 Markdown。",
		"password rotation is documented in the operations guide.",
		"token budget: 4096",
		"",
		// 抽取面的典型载体：证据引文里夹带凭证、正文里只有占位。
		"用户说：把 key 记成 "+admissionSecretSamples[0]+" 就行",
		"偏好：凭据用 ${LLM_API_KEY} 占位，不写明文",
	)
	for _, sample := range samples {
		require.Equal(t, core.TextHasSecret(sample), memory.TextHasCredential(sample),
			"准入与抽取必须共用同一份扫描规则：%q", sample)
	}
}
