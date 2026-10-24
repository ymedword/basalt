package workspace

// 本文件是模型连接定义文件读写的验收测试（颗粒度文档 §3.2.1：
// workspace/provider_file_test.go，计划 12 例；实际 14 例——在计划之外补「资源块字节
// 形态」（已发布 definition_yaml 的校验入口）与「非法兄弟条目」两组防御用例，均属新增
// 测试，不改动文档既有验收语义）。
// This file covers provider definition parsing, validation, rendering and atomic
// writes. The two extra cases guard the published block-excerpt form and the
// fail-closed behaviour when a sibling entry is invalid.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// providerDoc 是测试用的最小配置：两个 provider 条目 + 块前后注释与无关段，
// 用于验证块外字节逐字保留（与 basalt.yaml.example 同形）。
// providerDoc is the minimal fixture: two entries plus surrounding comments.
const providerDoc = `# Basalt 全局配置
providers:
  - name: deepseek
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
    default_model: deepseek-chat
  - name: qwen
    base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
    api_key: ${DASHSCOPE_API_KEY}
    default_model: qwen-plus

# 预算：运维定的上限
budget:
  max_iterations: 10
`

// deepseekEntry 是 providerDoc 中第一条目的原文字节。
const deepseekEntry = `  - name: deepseek
    base_url: https://api.deepseek.com
    api_key: ${DEEPSEEK_API_KEY}
    default_model: deepseek-chat
`

// localEntry 是追加 / 新建块用例使用的条目。
var localEntry = ProviderEntry{
	Name:         "local",
	BaseURL:      "http://127.0.0.1:8000/v1",
	APIKey:       "${LOCAL_LLM_KEY}",
	DefaultModel: "qwen2.5-32b-instruct",
}

// localEntryYAML 是 localEntry 的规范渲染结果（键序固定）。
const localEntryYAML = `  - name: local
    base_url: http://127.0.0.1:8000/v1
    api_key: ${LOCAL_LLM_KEY}
    default_model: qwen2.5-32b-instruct
`

// writeProviderDoc 在临时目录写一份定义文件并返回路径。
func writeProviderDoc(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "basalt.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// readFileString 读取文件内容（测试断言用）。
func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(raw)
}

// TestProviderDef_ParseAndRender 覆盖「读取 + 渲染」：字段逐项解析、渲染键序固定、
// 渲染结果与源文件逐字节同形（替换回文档后文档不变）。
func TestProviderDef_ParseAndRender(t *testing.T) {
	entries, err := ValidateProviderEntries([]byte(providerDoc))
	require.NoError(t, err)
	require.Equal(t, []ProviderEntry{
		{Name: "deepseek", BaseURL: "https://api.deepseek.com", APIKey: "${DEEPSEEK_API_KEY}", DefaultModel: "deepseek-chat"},
		{Name: "qwen", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1", APIKey: "${DASHSCOPE_API_KEY}", DefaultModel: "qwen-plus"},
	}, entries)

	rendered, err := RenderProviderEntry(entries[0])
	require.NoError(t, err)
	require.Equal(t, deepseekEntry, string(rendered))

	out, lost, err := ReplaceEntry([]byte(providerDoc), providersKey, "deepseek", rendered)
	require.NoError(t, err)
	require.False(t, lost)
	require.Equal(t, providerDoc, string(out))

	// 需要引号的值必须转义后仍可回读（渲染 → 校验往返一致）。
	p := ProviderEntry{Name: "weird", BaseURL: "https://example.com", APIKey: "${WEIRD_KEY}", DefaultModel: "model: v1 # x"}
	rendered, err = RenderProviderEntry(p)
	require.NoError(t, err)
	require.Contains(t, string(rendered), `default_model: "model: v1 # x"`)
	round, err := ValidateProviderEntries(rendered)
	require.NoError(t, err)
	require.Equal(t, []ProviderEntry{p}, round)
}

// TestProviderDef_ValidationMatrix 覆盖约束 7 的候选校验矩阵：必填、协议、userinfo、
// 占位形状；错误点名键且不回显凭证值。
func TestProviderDef_ValidationMatrix(t *testing.T) {
	valid := ProviderEntry{Name: "main", BaseURL: "https://api.example.com", APIKey: "${MAIN_KEY}", DefaultModel: "m1"}
	cases := []struct {
		name     string
		mutate   func(p *ProviderEntry)
		wantKeys string
	}{
		{"name 为空", func(p *ProviderEntry) { p.Name = "" }, "name"},
		{"name 带首尾空白", func(p *ProviderEntry) { p.Name = " main " }, "name"},
		{"base_url 为空", func(p *ProviderEntry) { p.BaseURL = "" }, "base_url"},
		{"base_url 非 http/https", func(p *ProviderEntry) { p.BaseURL = "ftp://api.example.com" }, "base_url"},
		{"base_url 缺主机", func(p *ProviderEntry) { p.BaseURL = "https:///v1" }, "base_url"},
		{"base_url 含 userinfo", func(p *ProviderEntry) { p.BaseURL = "https://user:pass@api.example.com" }, "base_url"},
		{"default_model 为空", func(p *ProviderEntry) { p.DefaultModel = "" }, "default_model"},
		{"api_key 为空", func(p *ProviderEntry) { p.APIKey = "" }, "api_key"},
		{"api_key 明文", func(p *ProviderEntry) { p.APIKey = "sk-live-plaintext" }, "api_key"},
		{"api_key 空占位", func(p *ProviderEntry) { p.APIKey = "${}" }, "api_key"},
		{"api_key 嵌套占位", func(p *ProviderEntry) { p.APIKey = "${${INNER}}" }, "api_key"},
		{"api_key 带前缀", func(p *ProviderEntry) { p.APIKey = "Bearer ${MAIN_KEY}" }, "api_key"},
		{"api_key 带后缀", func(p *ProviderEntry) { p.APIKey = "${MAIN_KEY}-v1" }, "api_key"},
		{"api_key 两个占位", func(p *ProviderEntry) { p.APIKey = "${A}${B}" }, "api_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.mutate(&p)
			_, err := RenderProviderEntry(p)
			require.ErrorIs(t, err, ErrInvalidDefinition)
			require.Contains(t, err.Error(), tc.wantKeys)
			require.NotContains(t, err.Error(), "sk-live-plaintext")
		})
	}

	// 文档形态同样走字段校验：非法条目被拒，错误定位到第几项。
	badDoc := "providers:\n  - name: ok\n    base_url: https://api.example.com\n    api_key: ${OK_KEY}\n    default_model: m1\n" +
		"  - name: bad\n    base_url: https://api.example.com\n    api_key: ${BAD_KEY}\n    default_model: \"\"\n"
	_, err := ValidateProviderEntries([]byte(badDoc))
	require.ErrorIs(t, err, ErrInvalidDefinition)
	require.Contains(t, err.Error(), "第 2 项")
	require.Contains(t, err.Error(), "default_model")
}

// TestProviderDef_ManagedCredentialMode 覆盖 credential_mode 的两个分支：托管定义
// 不带 api_key、渲染省略该键、解析与渲染往返一致；显式 env 字段原样保留；未知模式
// 拒绝并点名字段。
// TestProviderDef_ManagedCredentialMode covers the managed credential block form:
// no api_key, round-trip stable, and strict rejection of unknown modes.
func TestProviderDef_ManagedCredentialMode(t *testing.T) {
	const managedDoc = `  - name: alpha
    base_url: https://api.example.com/v1
    credential_mode: managed
    default_model: alpha-model
`
	entries, err := ValidateProviderEntries([]byte(managedDoc))
	require.NoError(t, err)
	require.Equal(t, []ProviderEntry{{
		Name:           "alpha",
		BaseURL:        "https://api.example.com/v1",
		DefaultModel:   "alpha-model",
		CredentialMode: "managed",
	}}, entries)

	rendered, err := RenderProviderEntry(entries[0])
	require.NoError(t, err)
	require.Equal(t, managedDoc, string(rendered))
	require.NotContains(t, string(rendered), "api_key")

	// 显式 env 字段照常解析与渲染（缺省省略，显式写出保留）。
	envEntry := ProviderEntry{
		Name:           "beta",
		BaseURL:        "https://api.example.com",
		APIKey:         "${BETA_KEY}",
		DefaultModel:   "m1",
		CredentialMode: "env",
	}
	rendered, err = RenderProviderEntry(envEntry)
	require.NoError(t, err)
	require.Contains(t, string(rendered), "credential_mode: env")
	round, err := ValidateProviderEntries(rendered)
	require.NoError(t, err)
	require.Equal(t, []ProviderEntry{envEntry}, round)

	// 托管模式下不得带 api_key；未知模式拒绝且不回显占位值。
	_, err = ValidateProviderEntries([]byte("  - name: alpha\n    base_url: https://api.example.com\n" +
		"    api_key: ${A_KEY}\n    credential_mode: managed\n    default_model: m1\n"))
	require.ErrorIs(t, err, ErrInvalidDefinition)
	require.Contains(t, err.Error(), "api_key")
	require.NotContains(t, err.Error(), "A_KEY")

	_, err = ValidateProviderEntries([]byte("  - name: alpha\n    base_url: https://api.example.com\n" +
		"    credential_mode: private\n    default_model: m1\n"))
	require.ErrorIs(t, err, ErrInvalidDefinition)
	require.Contains(t, err.Error(), "credential_mode")
}

// TestProviderDef_RejectsUnsupportedShapes 覆盖结构拒绝：未知字段、非字符串值、
// 流式集合与空标量定义。
func TestProviderDef_RejectsUnsupportedShapes(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			"未知字段",
			"providers:\n  - name: main\n    base_url: https://api.example.com\n    api_key: ${MAIN_KEY}\n    default_model: m1\n    headers: x\n",
			"未知字段 headers",
		},
		{
			"非字符串值",
			"providers:\n  - name: main\n    base_url: https://api.example.com\n    api_key: ${MAIN_KEY}\n    default_model: 42\n",
			"字段 default_model 必须是字符串",
		},
		{
			"流式条目集合",
			"providers: [{name: main, base_url: https://api.example.com, api_key: \"${MAIN_KEY}\", default_model: m1}]\n",
			"流式集合",
		},
		{
			"流式资源块",
			"[{name: main, base_url: https://api.example.com, api_key: \"${MAIN_KEY}\", default_model: m1}]\n",
			"块式列表",
		},
		{
			"顶层标量",
			"just-a-string\n",
			"不可解析",
		},
		{
			"多文档",
			"providers: []\n---\nproviders: []\n",
			"恰为一个 YAML 文档",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateProviderEntries([]byte(tc.doc))
			require.ErrorIs(t, err, ErrInvalidDefinition)
			require.Contains(t, err.Error(), tc.want)
			require.NotContains(t, err.Error(), "sk-live")
		})
	}
}

// TestProviderDef_RejectsDuplicateNames 覆盖重名拒绝（文档形态与资源块形态）。
func TestProviderDef_RejectsDuplicateNames(t *testing.T) {
	entry := "  - name: main\n    base_url: https://api.example.com\n    api_key: ${MAIN_KEY}\n    default_model: m1\n"
	doc := "providers:\n" + entry + entry
	_, err := ValidateProviderEntries([]byte(doc))
	require.ErrorIs(t, err, ErrInvalidDefinition)
	require.Contains(t, err.Error(), "条目名 main 重复")

	_, err = ValidateProviderEntries([]byte(entry + entry))
	require.ErrorIs(t, err, ErrInvalidDefinition)
	require.Contains(t, err.Error(), "条目名 main 重复")
}

// TestProviderDef_DoesNotResolveEnvironment 覆盖「候选校验不解析环境变量」：
// 即便同名环境变量已设置，返回值仍是占位原文；无关段的占位也不解析。
func TestProviderDef_DoesNotResolveEnvironment(t *testing.T) {
	const envName = "BASALT_TEST_PROVIDER_KEY_NO_RESOLVE"
	t.Setenv(envName, "resolved-secret")
	doc := "smtp:\n  token: ${BASALT_TEST_SMTP_NO_RESOLVE}\n" +
		"providers:\n  - name: main\n    base_url: https://api.example.com\n    api_key: ${" + envName + "}\n    default_model: m1\n"
	entries, err := ValidateProviderEntries([]byte(doc))
	require.NoError(t, err)
	require.Equal(t, "${"+envName+"}", entries[0].APIKey)
}

// TestProviderFile_ReadProviderEntries 覆盖读取：有块、无该键（空列表）、文件缺失
// （错误）、内容非法（错误）。
func TestProviderFile_ReadProviderEntries(t *testing.T) {
	entries, err := ReadProviderEntries(writeProviderDoc(t, providerDoc))
	require.NoError(t, err)
	require.Len(t, entries, 2)

	entries, err = ReadProviderEntries(writeProviderDoc(t, "budget:\n  max_iterations: 10\n"))
	require.NoError(t, err)
	require.Empty(t, entries)

	_, err = ReadProviderEntries(filepath.Join(t.TempDir(), "absent.yaml"))
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, err = ReadProviderEntries(writeProviderDoc(t, "providers:\n  - name: main\n    base_url: ftp://api.example.com\n    api_key: ${MAIN_KEY}\n    default_model: m1\n"))
	require.ErrorIs(t, err, ErrInvalidDefinition)
}

// TestProviderFile_UpdateAppendsNewEntry 覆盖追加：新条目紧跟块尾、块外字节逐字
// 保留、注释不动。
func TestProviderFile_UpdateAppendsNewEntry(t *testing.T) {
	path := writeProviderDoc(t, providerDoc)
	lost, err := UpdateProviderEntry(path, localEntry)
	require.NoError(t, err)
	require.False(t, lost)

	want := strings.Replace(providerDoc, "    default_model: qwen-plus\n", "    default_model: qwen-plus\n"+localEntryYAML, 1)
	require.Equal(t, want, readFileString(t, path))

	entries, err := ReadProviderEntries(path)
	require.NoError(t, err)
	require.Equal(t, []string{"deepseek", "qwen", "local"}, []string{entries[0].Name, entries[1].Name, entries[2].Name})
	require.Equal(t, localEntry, entries[2])
}

// TestProviderFile_UpdateReplacesExistingEntry 覆盖替换：只改目标条目行区间，
// 兄弟条目与块外注释逐字不变。
func TestProviderFile_UpdateReplacesExistingEntry(t *testing.T) {
	path := writeProviderDoc(t, providerDoc)
	updated := ProviderEntry{
		Name:         "qwen",
		BaseURL:      "https://dashscope.aliyuncs.com/compatible-mode/v1",
		APIKey:       "${DASHSCOPE_API_KEY}",
		DefaultModel: "qwen-max",
	}
	lost, err := UpdateProviderEntry(path, updated)
	require.NoError(t, err)
	require.False(t, lost)

	want := strings.Replace(providerDoc, "    default_model: qwen-plus\n", "    default_model: qwen-max\n", 1)
	require.Equal(t, want, readFileString(t, path))
	entries, err := ReadProviderEntries(path)
	require.NoError(t, err)
	require.Equal(t, updated, entries[1])
	require.Contains(t, readFileString(t, path), deepseekEntry)
}

// TestProviderFile_UpdateCreatesBlockWhenAbsent 覆盖键缺失时创建块（普通文档末尾与
// `basalt init` 的 `{}` 模板两种形态）。
func TestProviderFile_UpdateCreatesBlockWhenAbsent(t *testing.T) {
	t.Run("文档末尾建块", func(t *testing.T) {
		path := writeProviderDoc(t, "# 仅预算\nbudget:\n  max_iterations: 10\n")
		lost, err := UpdateProviderEntry(path, localEntry)
		require.NoError(t, err)
		require.False(t, lost)
		require.Equal(t, "# 仅预算\nbudget:\n  max_iterations: 10\nproviders:\n"+localEntryYAML, readFileString(t, path))
	})

	t.Run("空表模板换成块式列表", func(t *testing.T) {
		path := writeProviderDoc(t, "{}\n")
		lost, err := UpdateProviderEntry(path, localEntry)
		require.NoError(t, err)
		require.False(t, lost)
		require.Equal(t, "providers:\n"+localEntryYAML, readFileString(t, path))
	})
}

// TestProviderFile_RemoveKeepsSiblings 覆盖删除：兄弟条目保留、目标缺失报
// fs.ErrNotExist 且不动文件、删到空块后仍可再次追加。
func TestProviderFile_RemoveKeepsSiblings(t *testing.T) {
	t.Run("删除一条", func(t *testing.T) {
		path := writeProviderDoc(t, providerDoc)
		lost, err := RemoveProviderEntry(path, "deepseek")
		require.NoError(t, err)
		require.False(t, lost)
		require.Equal(t, strings.Replace(providerDoc, deepseekEntry, "", 1), readFileString(t, path))
		entries, err := ReadProviderEntries(path)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.Equal(t, "qwen", entries[0].Name)
	})

	t.Run("目标不存在", func(t *testing.T) {
		path := writeProviderDoc(t, providerDoc)
		_, err := RemoveProviderEntry(path, "absent")
		require.ErrorIs(t, err, fs.ErrNotExist)
		require.Equal(t, providerDoc, readFileString(t, path))
	})

	t.Run("删空后可再追加", func(t *testing.T) {
		path := writeProviderDoc(t, providerDoc)
		_, err := RemoveProviderEntry(path, "deepseek")
		require.NoError(t, err)
		_, err = RemoveProviderEntry(path, "qwen")
		require.NoError(t, err)
		entries, err := ReadProviderEntries(path)
		require.NoError(t, err)
		require.Empty(t, entries)

		lost, err := UpdateProviderEntry(path, localEntry)
		require.NoError(t, err)
		require.False(t, lost)
		entries, err = ReadProviderEntries(path)
		require.NoError(t, err)
		require.Equal(t, []ProviderEntry{localEntry}, entries)
	})
}

// TestProviderFile_CommentsLostNoWrite 覆盖注释损失：目标条目内含注释时
// 返回 commentsLost=true 且**不写文件**（调用方据此映射 400 EDIT_UNSUPPORTED，
// 文件必须逐字节不变）。
func TestProviderFile_CommentsLostNoWrite(t *testing.T) {
	inline := "providers:\n  - name: main  # 主连接\n    base_url: https://api.example.com\n    api_key: ${MAIN_KEY}\n    default_model: m1\n"
	fullLine := "providers:\n  - name: main\n    # 迁移说明\n    base_url: https://api.example.com\n    api_key: ${MAIN_KEY}\n    default_model: m1\n"
	updated := ProviderEntry{Name: "main", BaseURL: "https://api.example.com/v1", APIKey: "${MAIN_KEY}", DefaultModel: "m1"}

	for name, doc := range map[string]string{"行内注释": inline, "整行注释": fullLine} {
		t.Run(name, func(t *testing.T) {
			path := writeProviderDoc(t, doc)
			lost, err := UpdateProviderEntry(path, updated)
			require.NoError(t, err)
			require.True(t, lost)
			require.Equal(t, doc, readFileString(t, path))

			lost, err = RemoveProviderEntry(path, "main")
			require.NoError(t, err)
			require.True(t, lost)
			require.Equal(t, doc, readFileString(t, path))
		})
	}
}

// TestProviderFile_AtomicWrite 覆盖原子写：同目录临时文件 → rename；rename 失败
// 保留原文件并清理临时文件。
func TestProviderFile_AtomicWrite(t *testing.T) {
	t.Run("成功写入不留临时文件", func(t *testing.T) {
		path := writeProviderDoc(t, providerDoc)
		_, err := UpdateProviderEntry(path, localEntry)
		require.NoError(t, err)
		temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), "basalt.yaml.tmp-*"))
		require.NoError(t, err)
		require.Empty(t, temps)
	})

	t.Run("rename 失败保留原文件", func(t *testing.T) {
		path := writeProviderDoc(t, providerDoc)
		var (
			renameDir  string
			renameBase string
			tempData   string
		)
		err := writeProviderFileWith(path, []byte("providers: []\n"), func(oldpath, newpath string) error {
			renameDir = filepath.Dir(oldpath)
			renameBase = filepath.Base(oldpath)
			raw, readErr := os.ReadFile(oldpath)
			require.NoError(t, readErr)
			tempData = string(raw)
			require.Equal(t, path, newpath)
			return errors.New("rename boom")
		})
		require.ErrorContains(t, err, "rename boom")
		// 临时文件必须与目标同目录（rename 才能原子生效），且数据已写入。
		require.Equal(t, filepath.Dir(path), renameDir)
		require.True(t, strings.HasPrefix(renameBase, "basalt.yaml.tmp-"), renameBase)
		require.Equal(t, "providers: []\n", tempData)
		require.Equal(t, providerDoc, readFileString(t, path))

		temps, globErr := filepath.Glob(filepath.Join(filepath.Dir(path), "basalt.yaml.tmp-*"))
		require.NoError(t, globErr)
		require.Empty(t, temps)
	})
}

// TestProviderFile_ETagBytes 覆盖 ETag 的字节定义（约束 4 / §6.1 第 9 行）：
// 写入文件的条目字节就是 definition_yaml 原文，哈希输入不得重新序列化。
func TestProviderFile_ETagBytes(t *testing.T) {
	path := writeProviderDoc(t, providerDoc)
	updated := ProviderEntry{
		Name:         "deepseek",
		BaseURL:      "https://api.deepseek.com/v1",
		APIKey:       "${DEEPSEEK_API_KEY}",
		DefaultModel: "deepseek-chat",
	}
	_, err := UpdateProviderEntry(path, updated)
	require.NoError(t, err)

	raw := []byte(readFileString(t, path))
	entries, err := BlockEntries(raw, providersKey)
	require.NoError(t, err)
	var stored []byte
	for _, entry := range entries {
		if entry.Name == "deepseek" {
			stored = entry.Raw
		}
	}
	rendered, err := RenderProviderEntry(updated)
	require.NoError(t, err)
	require.Equal(t, rendered, stored)

	sum := sha256.Sum256(stored)
	etag := `"sha256:` + hex.EncodeToString(sum[:]) + `"`
	require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, etag)
}

// TestProviderFile_RejectsInvalidSibling 覆盖候选整体校验：既有条目非法时，
// 修改**其它**条目失败且零写入；直接修正该条目则成功（存在恢复路径）。
func TestProviderFile_RejectsInvalidSibling(t *testing.T) {
	legacy := "providers:\n  - name: legacy\n    base_url: https://legacy.example.com\n    api_key: sk-plaintext\n    default_model: m0\n"
	path := writeProviderDoc(t, legacy)

	_, err := UpdateProviderEntry(path, localEntry)
	require.ErrorIs(t, err, ErrInvalidDefinition)
	require.NotContains(t, err.Error(), "sk-plaintext")
	require.Equal(t, legacy, readFileString(t, path))

	fixed := ProviderEntry{Name: "legacy", BaseURL: "https://legacy.example.com", APIKey: "${LEGACY_KEY}", DefaultModel: "m0"}
	lost, err := UpdateProviderEntry(path, fixed)
	require.NoError(t, err)
	require.False(t, lost)
	entries, err := ReadProviderEntries(path)
	require.NoError(t, err)
	require.Equal(t, []ProviderEntry{fixed}, entries)
}
