package workspace

// 本文件是 `resource_bindings` 块编辑面的关键回归用例（颗粒度文档 R4.6 / T108 /
// 专属 check `012-workspace-bindings`）：解析与序列化往返逐字节稳定；块编辑只动
// 目标块、块外字节逐字保留；结构非法与不可安全替换的构造一律拒绝。
// Regression tests for the resource_bindings block: byte-stable round trips,
// verbatim preservation outside the block, and fail-closed malformed input.

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ymedword/basalt/internal/core"
)

const (
	testModelUID  = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	testNotifyUID = "rs_01J8Z7A1B2C3D4E5F6G7H8J9K0"
)

// bindingsFixture 返回一份带完整 frontmatter 与正文的 AGENT.md。
// bindingsFixture returns an AGENT.md with a full frontmatter and a body.
func bindingsFixture(bindingsBlock string) string {
	return "---\n" +
		"name: bot\n" +
		"description: 绑定样例\n" +
		"provider:\n" +
		"  name: deepseek\n" +
		"  model: deepseek-chat\n" +
		"notify_mode: allowlist\n" +
		bindingsBlock +
		"schedules: []\n" +
		"---\n" +
		"\n" +
		"你是绑定样例。\n"
}

// wantBindings 是样例期望值。
// wantBindings is the expected domain value of the fixture block.
func wantBindings() core.ResourceBindings {
	return core.ResourceBindings{
		ModelConnections: []core.ResourceBinding{{Name: "deepseek", UID: testModelUID}},
		NotifyChannels:   []core.ResourceBinding{{Name: "ops", UID: testNotifyUID}},
	}
}

// TestBindings_ParseSerializeRoundTripBytes：解析与序列化往返逐字节稳定——
// 契约已审样例（引号写法）与仓内规范渲染（双引号标量）都必须回到同一份规范字节。
func TestBindings_ParseSerializeRoundTripBytes(t *testing.T) {
	contractBlock := "resource_bindings:\n" +
		"  model_connection:\n" +
		"    - name: \"deepseek\"\n" +
		"      uid: \"" + testModelUID + "\"\n" +
		"  notify_channel:\n" +
		"    - name: \"ops\"\n" +
		"      uid: \"" + testNotifyUID + "\"\n"

	got, err := ParseResourceBindings([]byte(bindingsFixture(contractBlock)))
	if err != nil {
		t.Fatalf("解析契约样例失败: %v", err)
	}
	if !slices.Equal(got.ModelConnections, wantBindings().ModelConnections) ||
		!slices.Equal(got.NotifyChannels, wantBindings().NotifyChannels) {
		t.Fatalf("解析结果与期望不一致: %+v", got)
	}

	canonical := renderResourceBindingsBlock(got, "\n")
	again, err := ParseResourceBindings([]byte(bindingsFixture(canonical)))
	if err != nil {
		t.Fatalf("回读规范渲染失败: %v", err)
	}
	if !slices.Equal(again.ModelConnections, got.ModelConnections) ||
		!slices.Equal(again.NotifyChannels, got.NotifyChannels) {
		t.Fatalf("规范渲染回读不一致: %+v", again)
	}
	if rendered := renderResourceBindingsBlock(again, "\n"); rendered != canonical {
		t.Fatalf("二次渲染字节不稳定:\n%s\n---\n%s", canonical, rendered)
	}

	// 无块与空组都回到空结构，且渲染稳定为 `[]`。
	absent, err := ParseResourceBindings([]byte(bindingsFixture("")))
	if err != nil {
		t.Fatalf("无绑定块应返回空结构: %v", err)
	}
	if len(absent.ModelConnections) != 0 || len(absent.NotifyChannels) != 0 {
		t.Fatalf("无绑定块应返回空结构，实际 %+v", absent)
	}
	empty := renderResourceBindingsBlock(absent, "\n")
	const wantEmpty = "resource_bindings:\n  model_connection: []\n  notify_channel: []\n"
	if empty != wantEmpty {
		t.Fatalf("空绑定渲染不规范:\n%s", empty)
	}
}

// TestBindings_EditPreservesOutsideBytes：块编辑只替换目标块——块前、块后（含
// 正文）逐字保留；块缺失时安全插入；CRLF 文件保持 CRLF。
func TestBindings_EditPreservesOutsideBytes(t *testing.T) {
	oldBlock := "resource_bindings:\n" +
		"  model_connection:\n" +
		"    - name: old-conn\n" +
		"      uid: " + testModelUID + "\n" +
		"  notify_channel: []\n"
	original := bindingsFixture(oldBlock)

	patched, err := PatchResourceBindings([]byte(original), wantBindings())
	if err != nil {
		t.Fatalf("替换绑定块失败: %v", err)
	}
	prefix := original[:strings.Index(original, "resource_bindings:")]
	suffixAt := strings.Index(original, oldBlock) + len(oldBlock)
	suffix := original[suffixAt:]
	text := string(patched)
	if !strings.HasPrefix(text, prefix) {
		t.Fatalf("块前字节被改动:\n%s", text)
	}
	if !strings.HasSuffix(text, suffix) {
		t.Fatalf("块后字节被改动:\n%s", text)
	}
	got, err := ParseResourceBindings(patched)
	if err != nil {
		t.Fatalf("回读补丁结果失败: %v", err)
	}
	if !slices.Equal(got.ModelConnections, wantBindings().ModelConnections) ||
		!slices.Equal(got.NotifyChannels, wantBindings().NotifyChannels) {
		t.Fatalf("补丁结果与期望不一致: %+v", got)
	}

	// 块缺失：插在收尾围栏之前，其余字节逐字保留。
	absent := bindingsFixture("")
	inserted, err := PatchResourceBindings([]byte(absent), wantBindings())
	if err != nil {
		t.Fatalf("插入绑定块失败: %v", err)
	}
	wantInserted := strings.Replace(absent, "schedules: []\n---\n",
		"schedules: []\n"+renderResourceBindingsBlock(wantBindings(), "\n")+"---\n", 1)
	if string(inserted) != wantInserted {
		t.Fatalf("插入位置或其余字节不符:\n%s", inserted)
	}

	// CRLF 文件：渲染沿用文件换行风格。
	crlf := strings.ReplaceAll(bindingsFixture(oldBlock), "\n", "\r\n")
	patchedCRLF, err := PatchResourceBindings([]byte(crlf), wantBindings())
	if err != nil {
		t.Fatalf("CRLF 替换失败: %v", err)
	}
	if strings.Contains(strings.ReplaceAll(string(patchedCRLF), "\r\n", ""), "\n") {
		t.Fatalf("CRLF 文件被混入裸 LF:\n%q", patchedCRLF)
	}
	if !strings.HasSuffix(string(patchedCRLF), "\r\n你是绑定样例。\r\n") {
		t.Fatalf("CRLF 正文被改动: %q", patchedCRLF)
	}

	// BOM 前缀：块外字节（含 BOM）逐字保留。
	// 必须写转义：字面量 BOM 会触发 Go 扫描器的「illegal byte order mark」。
	bom := "\ufeff" + original
	patchedBOM, err := PatchResourceBindings([]byte(bom), wantBindings())
	if err != nil {
		t.Fatalf("BOM 文件替换失败: %v", err)
	}
	if !strings.HasPrefix(string(patchedBOM), "\ufeff---\nname: bot\n") {
		t.Fatalf("BOM 前缀被改动: %q", patchedBOM)
	}
}

// TestBindings_RejectsMalformedBlock：结构非法（解析期）与不可安全替换（编辑期）
// 的构造全部拒绝，且错误可被哨兵识别。
func TestBindings_RejectsMalformedBlock(t *testing.T) {
	parseCases := []struct {
		name  string
		block string
	}{
		{"未知子键", "resource_bindings:\n  skills:\n    - name: a\n      uid: " + testModelUID + "\n"},
		{"元素多余字段", "resource_bindings:\n  model_connection:\n    - name: a\n      uid: " + testModelUID + "\n      note: x\n  notify_channel: []\n"},
		{"元素缺名称", "resource_bindings:\n  model_connection:\n    - uid: " + testModelUID + "\n  notify_channel: []\n"},
		{"元素空名称", "resource_bindings:\n  model_connection:\n    - name: \"\"\n      uid: " + testModelUID + "\n  notify_channel: []\n"},
		{"UID 缺前缀", "resource_bindings:\n  model_connection:\n    - name: a\n      uid: 01J8Z6YQ4M2S3T7V9W0X1Y2Z3A\n  notify_channel: []\n"},
		{"UID 含易混字符", "resource_bindings:\n  model_connection:\n    - name: a\n      uid: rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3O\n  notify_channel: []\n"},
		{"重复名称", "resource_bindings:\n  model_connection:\n    - name: a\n      uid: " + testModelUID + "\n    - name: a\n      uid: " + testNotifyUID + "\n  notify_channel: []\n"},
		{"重复 UID", "resource_bindings:\n  model_connection:\n    - name: a\n      uid: " + testModelUID + "\n    - name: b\n      uid: " + testModelUID + "\n  notify_channel: []\n"},
		{"组不是列表", "resource_bindings:\n  model_connection: deepseek\n  notify_channel: []\n"},
		{"块不是映射", "resource_bindings:\n  - name: a\n"},
		{"顶层键重复", "resource_bindings: []\nresource_bindings:\n  model_connection: []\n  notify_channel: []\n"},
	}
	for _, tc := range parseCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseResourceBindings([]byte(bindingsFixture(tc.block)))
			if err == nil {
				t.Fatalf("应拒绝非法绑定块 %q", tc.block)
			}
			if !errors.Is(err, core.ErrAgentResourceBindingInvalid) {
				t.Fatalf("错误应包装 ErrAgentResourceBindingInvalid，实际 %v", err)
			}
		})
	}

	editCases := []struct {
		name  string
		block string
	}{
		{"块内注释", "resource_bindings:\n  model_connection: []  # 旧连接\n  notify_channel: []\n"},
		{"键行含注释", "resource_bindings:  # 绑定\n  model_connection: []\n  notify_channel: []\n"},
		{"块内含锚点", "resource_bindings:\n  model_connection: &conns\n    - name: a\n      uid: " + testModelUID + "\n  notify_channel: []\n"},
		{"块内含制表符", "resource_bindings:\n\tmodel_connection: []\n  notify_channel: []\n"},
		{"引号键无法定位", "\"resource_bindings\":\n  model_connection: []\n  notify_channel: []\n"},
	}
	for _, tc := range editCases {
		t.Run(tc.name, func(t *testing.T) {
			original := bindingsFixture(tc.block)
			patched, err := PatchResourceBindings([]byte(original), wantBindings())
			if err == nil {
				t.Fatalf("应拒绝编辑 %q，实际得到:\n%s", tc.block, patched)
			}
			if patched != nil {
				t.Fatalf("拒绝时不得返回候选字节: %q", patched)
			}
			if !errors.Is(err, ErrEditUnsupported) && !errors.Is(err, core.ErrAgentResourceBindingInvalid) {
				t.Fatalf("错误应可被哨兵识别，实际 %v", err)
			}
		})
	}
}
