package app

// 本文件是 US-21 技能引用扫描的验收面（颗粒度文档 §3.1.8、T009 DoD）：
//
//	R1 `TestUS21_Refs_ScansActiveAndArchive`——活动 / 归档两侧按 `skills` 声明判定；
//	   归档点名 `archive:<id>`；解析失败的 Agent 按可能引用计入且带固定后缀
//	R2 `TestUS21_Refs_MissingDirsAreEmpty`——缺 `agents/` 或 `archive/` 是正常状态
//	R3 `TestUS21_Refs_Deterministic`——同一工作区两次调用得到逐字相同的升序名单
//
// 判别力：实现若删掉 fail-closed 分支（把解析失败当"未引用"），R1 直接红；若改用
// 归档路径而不是 `archive:<id>` 点名，R1 / R2 的名字断言红。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// admissionRefAgent 是一份最小可解析的 Agent 定义；skills 为空时写 `skills: []`。
// admissionRefAgent renders a minimal parseable AGENT.md with the given skills.
func admissionRefAgent(name string, skills ...string) string {
	body := "---\nname: " + name + "\nprovider:\n  name: p\n  model: m\nskills:"
	if len(skills) == 0 {
		return body + " []\n---\n正文\n"
	}
	body += "\n"
	for _, skill := range skills {
		body += "  - " + skill + "\n"
	}
	return body + "---\n正文\n"
}

// writeAdmissionRefFile 在 root 下写入一份定义文件（自动建目录）。
// writeAdmissionRefFile writes one definition file below root.
func writeAdmissionRefFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
}

// TestUS21_Refs_ScansActiveAndArchive 覆盖两侧判定与 fail-closed 口径。
// TestUS21_Refs_ScansActiveAndArchive covers both trees and the fail-closed rule.
func TestUS21_Refs_ScansActiveAndArchive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeAdmissionRefFile(t, root, "agents/daily-report-owner/AGENT.md", admissionRefAgent("daily-report-owner", "daily-report", "other"))
	writeAdmissionRefFile(t, root, "agents/unrelated/AGENT.md", admissionRefAgent("unrelated", "other"))
	writeAdmissionRefFile(t, root, "agents/broken/AGENT.md", "---\nskills: [daily-report\n")
	writeAdmissionRefFile(t, root, "agents/no-definition/keep.txt", "不是定义文件")
	writeAdmissionRefFile(t, root, "archive/arch_01/AGENT.md", admissionRefAgent("arch_01", "daily-report"))
	writeAdmissionRefFile(t, root, "archive/arch_02/AGENT.md", "---\nskills: [daily-report\n")
	writeAdmissionRefFile(t, root, "archive/arch_03/keep.txt", "归档目录缺 AGENT.md")

	refs, err := scanSkillReferences(root, "daily-report")
	require.NoError(t, err)
	require.Equal(t, []string{
		"archive:arch_01",
		"archive:arch_02",
		"broken" + unparseableAgentMarker,
		"daily-report-owner",
		"no-definition" + unparseableAgentMarker,
	}, refs)
	for _, ref := range refs {
		if strings.HasPrefix(ref, "archive:") {
			require.NotContains(t, strings.TrimPrefix(ref, "archive:"), "/", "归档点名只带标识，不带路径")
			require.NotContains(t, strings.TrimPrefix(ref, "archive:"), `\`, "归档点名只带标识，不带路径")
		}
	}
}

// TestUS21_Refs_MissingDirsAreEmpty 确认未建 Agent / 未归档是正常状态。
// TestUS21_Refs_MissingDirsAreEmpty confirms missing directories yield an empty list.
func TestUS21_Refs_MissingDirsAreEmpty(t *testing.T) {
	t.Parallel()
	refs, err := scanSkillReferences(t.TempDir(), "daily-report")
	require.NoError(t, err)
	require.Empty(t, refs)
}

// TestUS21_Refs_Deterministic 确认名单排序稳定（基线 JSON 直接进哈希）。
// TestUS21_Refs_Deterministic pins the sorted order because baseline JSON is hashed.
func TestUS21_Refs_Deterministic(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeAdmissionRefFile(t, root, "agents/zeta/AGENT.md", admissionRefAgent("zeta", "daily-report"))
	writeAdmissionRefFile(t, root, "agents/alpha/AGENT.md", admissionRefAgent("alpha", "daily-report"))
	first, err := scanSkillReferences(root, "daily-report")
	require.NoError(t, err)
	second, err := scanSkillReferences(root, "daily-report")
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, []string{"alpha", "zeta"}, first)
}
