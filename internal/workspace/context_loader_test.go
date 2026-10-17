package workspace_test

// 本文件是 US-3 段一装配面的关键回归用例（颗粒度文档 §3.2 的 context_loader_test.go
// 七例，§4.3 写出代码逐条落地）。夹具沿用 agent_test.go 的家族（§2.4 注 8）。
// Regression tests for section-one assembly. Fixtures are shared with agent_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// sectionOneNow 是段一用例的统一固定时刻（UTC）——时间由调用方显式传入，
// 函数内不得取真实当前时间（§2.2 约束 8）。
// sectionOneNow is the fixed instant every section-one test passes in.
func sectionOneNow() time.Time {
	return time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
}

// tail 取字符串末尾 n 个**字符**（断言失败消息里的现场快照；按 rune 切，避免
// 截出半个汉字）。
// tail returns the last n runes of s for failure messages.
func tail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

// 中文验收点：段一四要素齐备且**顺序正确**——比较各标记的位置（只 Contains 的话，
// 正文 / 卡片 / 时间顺序颠倒也能通过）、时间**在末尾**、技能正文（L2）**不得**预载。
// Acceptance point: section one keeps its part order and ends with the timestamp.
func TestContextLoader_SectionOneCarriesAllFourParts(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)
	now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC) // 缺省 UTC——输出含偏移（§2.4 注 10）

	section, err := workspace.NewContextLoader(bufferedLogger(t)).SectionOne(root, entry, now)
	require.NoError(t, err)

	iPrompt := strings.Index(section, "你是一个用于兼容性验证的助手。")                     // identity.prompt（样例原文）
	iName := strings.Index(section, "你的名字是「对照小助手」。")                         // 自我称谓（agent_name 样例原文）
	iBody := strings.Index(section, "compat ok")                             // AGENT.md 正文（样例原文）
	iBoot := strings.Index(section, "## AGENTS.md")                          // bootstrap 小节（样例声明）
	iCard := strings.Index(section, "- digest-format：摘要格式统一")                // 卡片 L1（description 夹具自定）
	iPath := strings.Index(section, ".basalt/skills/digest-format/SKILL.md") // 技能库真路径（§11.3 字面量）
	iTime := strings.Index(section, "当前时间：2026-10-16 09:30:00 +00:00")       // 末尾日期时间（now 显式传入）

	marks := []struct {
		label string
		idx   int
	}{
		{"人格", iPrompt},
		{"称谓", iName},
		{"正文", iBody},
		{"bootstrap", iBoot},
		{"卡片", iCard},
		{"真路径", iPath},
		{"时间", iTime},
	}
	for _, m := range marks {
		require.GreaterOrEqual(t, m.idx, 0, "段一缺少%s", m.label)
	}

	// 次序：人格 → 称谓 → 正文 → bootstrap → 卡片（name/description）→ 真路径 → 时间
	require.Less(t, iPrompt, iName)
	require.Less(t, iName, iBody)
	require.Less(t, iBody, iBoot)
	require.Less(t, iBoot, iCard)
	require.Less(t, iCard, iPath)
	require.Less(t, iPath, iTime)

	// 时间**收尾**（不是"存在即可"）：段一以时间行结束。
	require.True(t, strings.HasSuffix(section, "当前时间：2026-10-16 09:30:00 +00:00"),
		"段一必须以当前时间收尾，实际尾部：%q", tail(section, 40))
	require.NotContains(t, section, "技能说明正文占位") // L2 正文不得预载（夹具负向探针）
}

// 中文验收点：时间在末尾且**含时区偏移**（非 UTC 时刻渲染 +08:00，不得回落进程本地
// 时区）；未配置 identity.prompt 时段一不以提示词开头、无声明技能时不出现技能小节。
// Acceptance point: the timestamp carries the offset, and unconfigured parts are absent.
func TestContextLoader_SectionOneEndsWithTimestamp(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "plain-agent",
		"\nname: plain-agent\nprovider: {name: deepseek, model: deepseek-chat}\n",
		"无提示词无技能的正文")
	entry := loadOnlyAgent(t, root)

	// 固定 +08:00 时区：输出的偏移由 now 决定（§2.4 注 10——缺省 UTC、显式 IANA、含偏移）。
	now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	section, err := workspace.NewContextLoader(bufferedLogger(t)).SectionOne(root, entry, now)
	require.NoError(t, err)

	require.True(t, strings.HasSuffix(section, "当前时间：2026-10-16 09:30:00 +08:00"),
		"段一必须以带偏移的当前时间收尾，实际尾部：%q", tail(section, 40))
	require.True(t, strings.HasPrefix(section, "你的名字是「plain-agent」。"),
		"未配置 identity.prompt 时段一以自我称谓开头（缺省 = name），实际开头：%q", head(section, 40))
	require.NotContains(t, section, "## 技能") // 技能声明为空 → 不出现技能小节
}

// 中文验收点：**声明即绑定**——技能库里存在但未被声明的实体不得进段一；只注入
// L1 卡片（name + description + 真路径），SKILL.md 正文（L2）不得预载。
// Acceptance point: only declared skills reach section one, cards only, no bodies.
func TestContextLoader_OnlyDeclaredSkillsReachSectionOne(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	// 技能库里多放一份**未被声明**的实体（负向探针）。
	writeFileInRoot(t, root, filepath.Join("skills", "extra-skill", "SKILL.md"),
		"---\nname: extra-skill\ndescription: 未声明的技能\n---\n\n未声明技能正文占位\n")
	entry := loadOnlyAgent(t, root)

	section, err := workspace.NewContextLoader(bufferedLogger(t)).SectionOne(root, entry, sectionOneNow())
	require.NoError(t, err)

	require.Contains(t, section,
		"- digest-format：摘要格式统一（.basalt/skills/digest-format/SKILL.md）")
	require.NotContains(t, section, "extra-skill") // 未声明 → 不进段一
	require.NotContains(t, section, "未声明的技能")      // 未声明的 description 同样不进
	require.NotContains(t, section, "未声明技能正文占位")   // 更不得预载其正文
	require.NotContains(t, section, "技能说明正文占位")    // 已声明技能的正文也不预载（L2 留给 read_file）
}

// 中文验收点：未注册的 Agent 不得组装系统提示词（契约 §2.1「整只不注册」的执行面）——
// 校验未过（Profile 非 nil）与解析失败（Profile 为 nil）两条路径都必须拒绝。
// Acceptance point: an unregistered entry is refused on both load outcomes.
func TestContextLoader_RefusesUnregisteredAgent(t *testing.T) {
	// 5 段 cron → 整只不注册，但 frontmatter 已解析成功（Profile 非 nil）。
	root := writeWorkspaceWithSchedule(t, "0 8 * * *", "")
	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered)
	require.NotNil(t, entries[0].Profile, "解析成功即有 Profile——拒绝发生在校验面（§2.4 注 3）")

	loader := workspace.NewContextLoader(bufferedLogger(t))

	section, err := loader.SectionOne(root, entries[0], sectionOneNow())
	require.Error(t, err)
	require.Empty(t, section)
	require.Contains(t, err.Error(), "sched-agent")
	require.Contains(t, err.Error(), "未注册")

	// frontmatter 解析失败 → Profile 为 nil（连声明面都没有）→ 同样拒绝。
	section, err = loader.SectionOne(root, core.AgentEntry{Name: "sched-agent"}, sectionOneNow())
	require.Error(t, err)
	require.Empty(t, section)
	require.Contains(t, err.Error(), "sched-agent")
}

// 中文验收点：**无状态原语**——每次调用重读文件、不缓存：装载后改 AGENT.md 正文，
// 第二次调用必须取到新内容（反向陷阱：不得缓存上一次的拼装结果）。
// Acceptance point: every call re-reads the sources; nothing is cached.
func TestContextLoader_RereadsBetweenCalls(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)
	loader := workspace.NewContextLoader(bufferedLogger(t))

	first, err := loader.SectionOne(root, entry, sectionOneNow())
	require.NoError(t, err)
	require.Contains(t, first, `回复"compat ok"。`)

	// 装载之后改正文：第二次调用必须反映新内容。
	writeFileInRoot(t, root, filepath.Join("agents", "compat-check", "AGENT.md"),
		strings.Replace(compatSampleAgentMD, "compat ok", "compat ok v2", 1))

	second, err := loader.SectionOne(root, entry, sectionOneNow())
	require.NoError(t, err)
	require.Contains(t, second, "compat ok v2")
	require.NotContains(t, second, `回复"compat ok"。`, "段一缓存了上一次的正文")
}

// 中文验收点：装载后 bootstrap 文件消失 → SectionOne **返回错误**并点名路径，
// 禁止静默省略（静默省略会让"注入内容"与"清单哈希"悄悄分叉——§2.2 约束 6）。
// Acceptance point: a vanished bootstrap file is a loud error, never a silent omission.
func TestContextLoader_FailsWhenBootstrapFileDisappears(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)
	require.NoError(t, os.Remove(filepath.Join(root, "SOUL.md"))) // 样例声明的三件之一

	section, err := workspace.NewContextLoader(bufferedLogger(t)).SectionOne(root, entry,
		time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC))
	require.Error(t, err)
	require.Empty(t, section)
	require.Contains(t, err.Error(), "SOUL.md") // 点名缺失路径
}

// 中文验收点：装载后 SKILL.md 消失 → SectionOne **返回错误**并点名技能库真路径，
// 不得回落空卡片（空卡片会让模型以为技能存在却读不到说明）。
// Acceptance point: a vanished SKILL.md is a loud error naming the library path.
func TestContextLoader_FailsWhenSkillFileDisappears(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	entry := loadOnlyAgent(t, root)
	require.NoError(t, os.Remove(filepath.Join(root, "skills", "digest-format", "SKILL.md")))

	section, err := workspace.NewContextLoader(bufferedLogger(t)).SectionOne(root, entry, sectionOneNow())
	require.Error(t, err)
	require.Empty(t, section)
	require.Contains(t, err.Error(), "skills/digest-format/SKILL.md") // 点名技能库真路径
}

// head 取字符串开头 n 个字符（与 tail 同口径，供断言失败消息报现场）。
// head returns the first n runes of s for failure messages.
func head(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
