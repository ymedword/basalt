package workspace_test

// 本文件是 US-4 运行级配置快照冻结面的关键回归用例（颗粒度文档 §3.2 的
// workspace/snapshot_test.go 八例 O1~O8，§4.3 写出代码逐条落地）。夹具沿用
// agent_test.go / context_loader_test.go 的家族（§2.4 注 8）。
// Regression tests for the frozen run-level config snapshot. Fixtures are shared
// with agent_test.go and context_loader_test.go.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// digestHex 是**测试侧独立**重算的 sha256 小写十六进制（不调用被测代码——哈希口径
// 自查：清单条的 sha256 必须等于文件内容的哈希，快照的 hash 必须等于清单序列化的哈希）。
// digestHex recomputes a digest independently of the code under test.
func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// manifestPaths 取清单里的路径列（断言排序与覆盖面时只看路径）。
// manifestPaths lists the manifest's path column.
func manifestPaths(manifest []core.ManifestEntry) []string {
	paths := make([]string, 0, len(manifest))
	for _, e := range manifest {
		paths = append(paths, e.Path)
	}
	return paths
}

// 中文验收点：Profile 与段一出自**同一次读取的同一份原始 AGENT.md**——装载之后改盘，
// 快照必须用新盘内容，**不得**与旧的 `entry.Profile` 混用（否则出现"旧 Profile 配新正文"）。
// Acceptance point: profile and section one come from one read of the current file,
// never from the profile cached at load time.
func TestSnapshot_UsesSingleReadOfAgentMD(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root) // 装载期的 Profile：agent_name「对照小助手」、正文「compat ok」
	require.Equal(t, "对照小助手", entry.Profile.Identity.AgentName)

	// 改盘：frontmatter 与正文同属一份文件——两处一起改，才看得出新读取覆盖了两处。
	changed := strings.Replace(compatSampleAgentMD, "对照小助手", "改盘后的名字", 1)
	changed = strings.Replace(changed, "compat ok", "compat ok v2", 1)
	require.NotEqual(t, compatSampleAgentMD, changed)
	writeFileInRoot(t, root, filepath.Join("agents", "compat-check", "AGENT.md"), changed)

	snap, err := workspace.NewContextLoader(bufferedLogger(t)).Snapshot(root, entry, sectionOneNow())
	require.NoError(t, err)

	// Profile = 这一次读取的产物（与装载期缓存不是同一份）。
	require.Equal(t, "改盘后的名字", snap.Profile.Identity.AgentName)
	require.NotEqual(t, entry.Profile.Identity.AgentName, snap.Profile.Identity.AgentName)
	require.Equal(t, "你是一个用于兼容性验证的助手。", snap.Profile.Identity.Prompt, "同一次读取的其余字段也来自新盘")
	// 段一与 Profile 同源：新名字 / 新正文进段一，旧名字一个都不留。
	require.Contains(t, snap.SectionOne, "你的名字是「改盘后的名字」。")
	require.Contains(t, snap.SectionOne, "compat ok v2")
	require.NotContains(t, snap.SectionOne, "对照小助手")
	require.NotContains(t, snap.SectionOne, `回复"compat ok"。`)
	// 原文也是这一次读取的（三件产物同源，缺一件就说明读了第二遍）。
	require.Equal(t, changed, snap.Content)
}

// 中文验收点：`content` = **AGENT.md 全文**（含 frontmatter 围栏）——不是正文片段、
// 不是哈希；"人工可读的便捷副本"只有一个来源：原文。
// Acceptance point: content holds the raw AGENT.md verbatim, fence included.
func TestSnapshot_ContentKeepsRawAgentMD(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)

	snap, err := workspace.NewContextLoader(bufferedLogger(t)).Snapshot(root, entry, sectionOneNow())
	require.NoError(t, err)

	require.Equal(t, compatSampleAgentMD, snap.Content, "content = 原文逐字（夹具常量即盘上内容）")
	require.Contains(t, snap.Content, "---\nname: compat-check", "frontmatter 也算全文的一部分")
	require.Contains(t, snap.Content, "skills:\n  - digest-format")
	require.NotEqual(t, snap.SectionOne, snap.Content, "段一是注入面，原文是存档面——两回事")
	require.NotContains(t, snap.Content, "你的名字是「", "段一的拼装痕迹不得混进原文")
}

// 中文验收点：清单覆盖**注入面读到的每一个文件**（AGENT.md + bootstrap 文件 + 绑定
// 技能的 SKILL.md——技术方案 §3.6），逐条 `{path, sha256}` 且 sha256 等于该文件内容的
// 哈希；路径按序排列（规整序列化的确定性）。
// Acceptance point: the manifest covers every injected file, each hashed from the
// very bytes that were injected, ordered by path.
func TestSnapshot_ManifestCoversEveryInjectedFile(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)

	snap, err := workspace.NewContextLoader(bufferedLogger(t)).Snapshot(root, entry, sectionOneNow())
	require.NoError(t, err)

	// 兼容样例工作区的注入面 = 5 个文件（AGENT.md + bootstrap 三件 + 技能定义）。
	want := []string{
		"AGENTS.md",
		"SOUL.md",
		"USER.md",
		"agents/compat-check/AGENT.md",
		"skills/digest-format/SKILL.md",
	}
	require.Equal(t, want, manifestPaths(snap.Manifest), "清单 = 注入面的每一份文件，按路径排序")

	for _, e := range snap.Manifest {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(e.Path)))
		require.NoError(t, err, "清单条必须点得住盘上的文件：%s", e.Path)
		require.Equal(t, digestHex(raw), e.SHA256, "清单条的 sha256 = 该文件内容的哈希：%s", e.Path)
	}

	// 注入的内容确实包含这些文件（清单与注入面对得上，不是两份各说各话）。
	require.Contains(t, snap.SectionOne, "## AGENTS.md")
	require.Contains(t, snap.SectionOne, "## SOUL.md")
	require.Contains(t, snap.SectionOne, "## USER.md")
}

// 中文验收点：**同内容不同时刻 → 同 hash**（当前时间不得混入哈希——§2.2 约束 12 与
// data-model 实体 4 判据）；而段一随时刻变，两条断言一起才说明"只把时钟挡在哈希之外"。
// Acceptance point: the clock never enters the hash, even though it does enter the
// injected section.
func TestSnapshot_HashIgnoresCurrentTime(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)
	loader := workspace.NewContextLoader(bufferedLogger(t))

	first, err := loader.Snapshot(root, entry, sectionOneNow())
	require.NoError(t, err)
	second, err := loader.Snapshot(root, entry, sectionOneNow().Add(48*time.Hour))
	require.NoError(t, err)

	require.Equal(t, first.Hash, second.Hash, "同内容不同时刻必须同哈希（时间不入哈希）")
	require.Equal(t, first.Manifest, second.Manifest, "清单逐条相同（时间也不进清单）")
	require.Len(t, first.Hash, 64, "sha256 十六进制小写 64 字符")
	require.Equal(t, strings.ToLower(first.Hash), first.Hash)

	require.NotEqual(t, first.SectionOne, second.SectionOne, "段一末尾是当前时间——它随时刻变")
	require.Contains(t, second.SectionOne, "当前时间：2026-10-18 09:30:00 +00:00")
}

// 中文验收点：清单里的任一文件读不到 → **拒绝启动**（返回错误并点名路径、零注入、
// 不产出半份快照）——"注入内容与哈希必然对应"靠这条守着（§2.2 约束 6 / §11.5）。
// Acceptance point: any unreadable manifest source is a loud refusal, never a
// partial snapshot.
func TestSnapshot_FailsWhenSourceFileDisappears(t *testing.T) {
	loader := workspace.NewContextLoader(bufferedLogger(t))
	cases := []struct {
		label string
		gone  string
		want  string
	}{
		{"agent definition", filepath.Join("agents", "compat-check", "AGENT.md"), "agents/compat-check/AGENT.md"},
		{"bootstrap", "SOUL.md", "SOUL.md"},
		{"skill definition", filepath.Join("skills", "digest-format", "SKILL.md"), "skills/digest-format/SKILL.md"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			root := writeCompatSampleWorkspace(t)
			entry := loadOnlyAgent(t, root)
			require.NoError(t, os.Remove(filepath.Join(root, tc.gone)))

			snap, err := loader.Snapshot(root, entry, sectionOneNow())
			require.Error(t, err, "注入面缺文件必须拒绝启动")
			require.Nil(t, snap, "不得产出半份快照")
			require.Contains(t, err.Error(), tc.want, "错误必须点名缺失的路径")
		})
	}
}

// 中文验收点：`SectionOne` **签名与既有行为不变**——它就是快照的注入面（委托关系），
// 同一入参下两条路径给同一段文本、同样拒绝未注册的 Agent（守卫只在唯一的读取路径上）。
// Acceptance point: SectionOne keeps its signature and behavior; it is the snapshot's
// injection face and refuses unregistered agents exactly like Snapshot does.
func TestSnapshot_SectionOneDelegatesWithoutChangingBehavior(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)
	loader := workspace.NewContextLoader(bufferedLogger(t))
	now := sectionOneNow()

	snap, err := loader.Snapshot(root, entry, now)
	require.NoError(t, err)

	// 同签名同入参：段一（既有导出方法）逐字等于快照的注入面。
	section, err := loader.SectionOne(root, entry, now)
	require.NoError(t, err)
	require.Equal(t, snap.SectionOne, section)
	require.True(t, strings.HasSuffix(section, "当前时间：2026-10-16 09:30:00 +00:00"), "收尾口径不变")

	// 未注册（Profile 为 nil）→ 两条路径同样拒绝、同样点名。
	snap, err = loader.Snapshot(root, core.AgentEntry{Name: entry.Name}, now)
	require.Error(t, err)
	require.Nil(t, snap)
	require.Contains(t, err.Error(), "未注册")
	require.Contains(t, err.Error(), entry.Name)

	section, err = loader.SectionOne(root, core.AgentEntry{Name: entry.Name}, now)
	require.Error(t, err)
	require.Empty(t, section)
	require.Contains(t, err.Error(), "未注册")
}

// 中文验收点：L2/L3 技能正文与 `scripts/` **不进清单**（按需读取、读时为准），
// 未声明的技能实体同样不进——清单只记"这次注入了什么"（技术方案 §3.6）。
// Acceptance point: L2/L3 bodies, scripts and undeclared skills stay out of the
// manifest; it records what was injected, nothing else.
func TestSnapshot_ManifestExcludesBodiesAndScripts(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	// 渐进披露的下一层（L2 参考文档 / L3 脚本）与一份**未被声明**的技能实体。
	writeFileInRoot(t, root, filepath.Join("skills", "digest-format", "reference.md"), "L2 参考文档占位\n")
	writeFileInRoot(t, root, filepath.Join("skills", "digest-format", "scripts", "run.sh"), "echo L3\n")
	writeFileInRoot(t, root, filepath.Join("skills", "extra-skill", "SKILL.md"),
		"---\nname: extra-skill\ndescription: 未声明的技能\n---\n\n未声明技能正文占位\n")

	entry := loadOnlyAgent(t, root)
	snap, err := workspace.NewContextLoader(bufferedLogger(t)).Snapshot(root, entry, sectionOneNow())
	require.NoError(t, err)

	require.Len(t, snap.Manifest, 5, "清单只记注入面读到的文件（L2/L3 与未声明实体一件都不许进来）")
	for _, e := range snap.Manifest {
		require.NotContains(t, e.Path, "reference.md", "L2 正文按需读取，不在运行冻结范围")
		require.NotContains(t, e.Path, "scripts/", "L3 脚本按需读取，不在运行冻结范围")
		require.NotContains(t, e.Path, "extra-skill", "未声明的技能实体不进清单")
	}
}

// 中文验收点：`manifest_json` **规整序列化稳定**——同一份配置两次冻结的序列化字节逐字
// 相同、键名逐字 `path` / `sha256`、路径有序；且快照 `Hash` 可由测试侧对同一份清单重算
// 得出（= 清单规整序列化的 sha256，不是文件内容哈希——两者是不同的量）。
// Acceptance point: the manifest serializes canonically, and the snapshot hash is
// recomputable as the digest of that serialization.
func TestSnapshot_ManifestSerializationIsCanonical(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)
	loader := workspace.NewContextLoader(bufferedLogger(t))

	first, err := loader.Snapshot(root, entry, sectionOneNow())
	require.NoError(t, err)
	second, err := loader.Snapshot(root, entry, sectionOneNow().Add(time.Minute))
	require.NoError(t, err)

	firstJSON, err := json.Marshal(first.Manifest)
	require.NoError(t, err)
	secondJSON, err := json.Marshal(second.Manifest)
	require.NoError(t, err)
	require.Equal(t, string(firstJSON), string(secondJSON), "同一份配置的清单序列化逐字相同")
	require.Contains(t, string(firstJSON), `{"path":"AGENTS.md","sha256":"`, "键名逐字 path / sha256（契约列注释）")

	require.True(t, slices.IsSorted(manifestPaths(first.Manifest)), "清单按路径排序——规整序列化的确定性")
	require.Equal(t, digestHex(firstJSON), first.Hash, "Hash = 清单规整序列化的 sha256（测试侧可独立重算）")
	require.NotEqual(t, digestHex([]byte(first.Content)), first.Hash, "快照哈希不是 AGENT.md 内容的哈希——两个量不得混同")
}
