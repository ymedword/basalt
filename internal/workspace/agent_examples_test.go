package workspace

// 本文件守颗粒度文档 §4.3 C-40 的 `TestBuiltinAgents_FiveDefinitionsParse`：
// 五个内置 Agent 的 AGENT.md 必须真的能过**装载期同一个**解析器（`parseFrontmatter`），
// 且解析结果里的身份、供应商与技能声明与目录名 / 设计清单一致。
//
// 为什么放在本包：装载器对空白模型整只拒绝注册（`blank model`），内置安装又把模型
// 清空（builtin.BlankModel）——因此"解析成功"只能在这里直接判，不能借装载结果反推。
// 读仓库里的 `examples/daily/**/AGENT.md`（安装器的源），不是安装后的副本。
//
// The five built-in definitions must parse through the same parser the loader uses.
// The installer blanks provider.model, so the loader rejects them at registration; parsing
// is therefore asserted here against the repository examples the installer copies from.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// builtinExampleAgents 是 `examples/daily/` 下的五个目录名（与 builtin.Agents() 同源；
// 这里单独抄一份，实现把某只 Agent 改名时本用例会红，而不是跟着实现自证）。
// builtinExampleAgents mirrors builtin.Agents() by hand so a rename shows up here.
var builtinExampleAgents = []string{
	"biz-daily",
	"docs-diff",
	"github-radar",
	"industry-brief",
	"repo-digest",
}

// TestBuiltinAgents_FiveDefinitionsParse 中文验收点：五份定义逐份解析成功、无未知键、
// `name` 等于目录名、`provider` 名字与模型非空；`repo-digest` 的 `skills: [repo-digest]`
// 必须被解析出来（声明即绑定——解析期漏掉技能等于整只 Agent 少一项能力）。
//
// 逆改点：解析器漏读 `provider` 块 → Provider.Name 为空；把 `skills` 当未知键 → warns
// 非空且 Skills 为空。两处都会让本用例点名具体 Agent。
func TestBuiltinAgents_FiveDefinitionsParse(t *testing.T) {
	for _, name := range builtinExampleAgents {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "daily", name, "AGENT.md"))
			require.NoError(t, err, "内置定义必须存在于 examples/daily/%s", name)

			profile, warns, err := parseFrontmatter(raw)
			require.NoError(t, err, "%s 的 AGENT.md 必须能被装载期解析器接受", name)
			require.Empty(t, warns, "%s 的定义不得带未知键", name)
			require.Equal(t, name, profile.Name, "name 必须等于目录名")
			require.NotEmpty(t, profile.Provider.Name, "%s 必须声明 provider.name", name)
			require.NotEmpty(t, profile.Provider.Model, "%s 必须声明 provider.model（占位文案也算声明）", name)

			if name == "repo-digest" {
				require.Equal(t, []string{"repo-digest"}, profile.Skills,
					"repo-digest 的技能声明必须在解析期原样带出")
			}
		})
	}
}
