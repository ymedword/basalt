package workspace_test

// 本文件是 AGENT.md `knowledge:` 声明面的关键回归用例（设计 D-19 / A-6、契约
// agent-md §2.7、加载期规则 12）：声明即绑定——结构非法整只不注册，缺省权限为
// `read`，合法声明原样带进 Profile 供注册闸门与检索授权读取。
//
// 判别力：把 `parseKnowledgeFront` 的 DisallowUnknownField 去掉 → 未知元素键用例
// 红；把 `core.ValidateKnowledgeBindings` 删掉 → 重名 / 缺名用例红；把 access 的
// 闭集判定换成"非空即认" → `access: write` 用例红；把 profile.Knowledge 赋值删掉
// → 装载器正向用例红。
//
// Regression tests for the `knowledge:` declaration: malformed blocks keep the agent
// unregistered, the default access is read, and a valid block reaches the profile.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// knowledgeFixture 拼一份带 `knowledge:` 块的 AGENT.md（块文本逐字插入）。
// knowledgeFixture wraps the given block text into a loadable AGENT.md.
func knowledgeFixture(block string) string {
	return "---\n" +
		"name: bot\n" +
		"description: 知识声明样例\n" +
		"provider:\n" +
		"  name: deepseek\n" +
		"  model: deepseek-chat\n" +
		block +
		"---\n" +
		"\n" +
		"你是知识声明样例。\n"
}

// TestKnowledgeBindings_AbsentOrEmptyBlockYieldsNoDeclaration：未声明、空块、`null`
// 三种写法都等于"不检索任何集合"，不构成绑定也不拒载。
func TestKnowledgeBindings_AbsentOrEmptyBlockYieldsNoDeclaration(t *testing.T) {
	for name, block := range map[string]string{
		"absent": "",
		"empty":  "knowledge: []\n",
		"null":   "knowledge:\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := workspace.ParseKnowledgeBindings([]byte(knowledgeFixture(block)))
			require.NoError(t, err)
			require.Empty(t, got)
		})
	}
}

// TestKnowledgeBindings_ParsesDeclarationsAndDefaultsAccess：两项声明按文件序带出，
// 缺省 access 归一为 read，显式 read 原样保留。
func TestKnowledgeBindings_ParsesDeclarationsAndDefaultsAccess(t *testing.T) {
	block := "knowledge:\n" +
		"  - name: runbooks\n" +
		"  - name: \"faq\"\n" +
		"    access: read\n"
	got, err := workspace.ParseKnowledgeBindings([]byte(knowledgeFixture(block)))
	require.NoError(t, err)
	require.Equal(t, []core.KnowledgeBinding{
		{Name: "runbooks", Access: core.KnowledgeAccessRead},
		{Name: "faq", Access: core.KnowledgeAccessRead},
	}, got)
}

// TestKnowledgeBindings_RefusesMalformedDeclarations：结构非法一律包装
// `core.ErrAgentKnowledgeBindingInvalid`——加载期据此整只不注册（规则 12）。
func TestKnowledgeBindings_RefusesMalformedDeclarations(t *testing.T) {
	cases := map[string]string{
		"not a list": "knowledge: read\n",
		"mapping":    "knowledge:\n  name: runbooks\n",
		"unknown element key": "knowledge:\n" +
			"  - name: runbooks\n" +
			"    write: true\n",
		"missing name": "knowledge:\n" +
			"  - access: read\n",
		"duplicate name": "knowledge:\n" +
			"  - name: runbooks\n" +
			"  - name: runbooks\n",
		"unknown access": "knowledge:\n" +
			"  - name: runbooks\n" +
			"    access: write\n",
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := workspace.ParseKnowledgeBindings([]byte(knowledgeFixture(block)))
			require.ErrorIs(t, err, core.ErrAgentKnowledgeBindingInvalid)
		})
	}
}

// TestLoadAgent_KnowledgeDeclarationReachesProfile：合法声明经装载器带进
// `Profile.Knowledge`，条目正常注册；注册闸门与检索授权读的就是这一份。
func TestLoadAgent_KnowledgeDeclarationReachesProfile(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "bot", "\n"+
		"name: bot\n"+
		"provider: {name: deepseek, model: deepseek-chat}\n"+
		"knowledge:\n"+
		"  - name: runbooks\n"+
		"  - name: faq\n"+
		"    access: read\n", "知识声明夹具正文")
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered)
	require.Equal(t, []core.KnowledgeBinding{
		{Name: "runbooks", Access: core.KnowledgeAccessRead},
		{Name: "faq", Access: core.KnowledgeAccessRead},
	}, entries[0].Profile.Knowledge)
}

// TestLoadAgent_MalformedKnowledgeKeepsAgentUnregistered：结构非法 → 整只不注册，
// 拒因是独立类别 `invalid knowledge`，detail 是固定短语（库文案不外传）。
func TestLoadAgent_MalformedKnowledgeKeepsAgentUnregistered(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "bot", "\n"+
		"name: bot\n"+
		"provider: {name: deepseek, model: deepseek-chat}\n"+
		"knowledge:\n"+
		"  - name: runbooks\n"+
		"    access: admin\n", "知识声明夹具正文")
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered, "声明结构非法 → 整只不注册")
	require.Equal(t, core.AgentRejectInvalidKnowledge, entries[0].RejectKind)
	require.NotContains(t, entries[0].RejectReason, "admin",
		"拒载文案不得回显声明的取值")
}

// TestKnowledgeBindings_UnknownTopLevelKeyStillWarnsOnly：`knowledge` 是字段表内
// 的已知键——带声明的 AGENT.md 不得被判成"含未知键"（否则每条合法声明都会多一条
// 误导性 WARN，契约 §2.1 与实现就此分叉）。
func TestKnowledgeBindings_UnknownTopLevelKeyStillWarnsOnly(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "bot", "\n"+
		"name: bot\n"+
		"provider: {name: deepseek, model: deepseek-chat}\n"+
		"knowledge:\n"+
		"  - name: runbooks\n", "知识声明夹具正文")
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered)
	require.NotContains(t, logText(t), "knowledge", "字段表内的键不得进未知键 WARN")
}
