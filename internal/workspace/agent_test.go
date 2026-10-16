package workspace_test

// 本文件是 US-3 装载面的关键回归用例（颗粒度文档 §3.2 的 agent_test.go 十八例，
// §4.3 写出代码逐条落地）。夹具家族（非导出，一律写 t.TempDir()）落在本文件，
// 供同目录其余测试文件复用——§2.4 注 8：不新增 testutil 公开 API。
// Regression tests for the US-3 loader. The inline fixture family lives here and
// is shared with the other test files of this package.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// compatSampleAgentMD 是 Compatibility §5.1 的共享验收样例 AGENT.md（逐字转录）。
// The shared acceptance sample, transcribed verbatim from Compatibility §5.1.
const compatSampleAgentMD = `---
name: compat-check
description: 兼容性对照样例 Agent
identity:
  agent_name: 对照小助手
  prompt: 你是一个用于兼容性验证的助手。
provider:
  name: deepseek
  model: deepseek-chat
tools:
  - read_file
  - write_file
  - list_dir
  - shell
  - http_get
  - http_post
  - save_memory
  - recall_memory
  - notify
  - manage_schedule
skills:
  - digest-format
mcp_servers: []
bootstrap:
  - AGENTS.md
  - SOUL.md
  - USER.md
settings:
  max_iterations: 10
  max_history_turns: 20
schedules:
  - id: compat-daily
    cron: "0 0 8 * * *"
    zone: Asia/Shanghai
    message: "输出一句当前时间"
---

你是一个用于兼容性验证的助手。收到任何消息时，回复"compat ok"。
`

// compatSampleToolNames 是共享样例声明的 10 个工具名（与上文逐字一致），作为
// **注册池**注入装载器使用——判据 2 的落点是"加载器对已注册名字集不误拒"，不是
// "这 10 件工具都实现了"（§2.4 注 4）。
// compatSampleToolNames is the sample's ten declared tool names, injected as the
// registered pool: the acceptance point is that the loader never misrejects a
// registered name set.
func compatSampleToolNames() []string {
	return []string{
		"read_file", "write_file", "list_dir", "shell", "http_get", "http_post",
		"save_memory", "recall_memory", "notify", "manage_schedule",
	}
}

// digestFormatSkillMD 是技能库实体 digest-format：description 供段一卡片断言，
// 正文「技能说明正文占位」是 L2 负向探针——不得预载进段一。
// The digest-format skill entity; its body is the L2 negative probe.
const digestFormatSkillMD = `---
name: digest-format
description: 摘要格式统一
---

技能说明正文占位
`

// writeCompatSampleWorkspace 砌出 Compatibility §5.1 共享样例的完整工作区：
// .basalt/agents/compat-check/AGENT.md（样例逐字）+ Bootstrap 三件 + 技能库实体。
// 返回工作区根（= .basalt 目录本体，与 LoadAll 的 root 口径一致）。
// writeCompatSampleWorkspace materializes the shared sample workspace.
func writeCompatSampleWorkspace(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".basalt")
	writeFileInRoot(t, root, filepath.Join("agents", "compat-check", "AGENT.md"), compatSampleAgentMD)
	writeFileInRoot(t, root, "AGENTS.md", "# AGENTS.md\n\n项目级行为说明占位。\n")
	writeFileInRoot(t, root, "SOUL.md", "# SOUL.md\n\n人格定义占位。\n")
	writeFileInRoot(t, root, "USER.md", "# USER.md\n\n用户偏好占位。\n")
	writeFileInRoot(t, root, filepath.Join("skills", "digest-format", "SKILL.md"), digestFormatSkillMD)
	return root
}

// writeWorkspaceWithSchedule 砌一只带单条 schedule 的 Agent 工作区（cron / zone 逐例给定）。
// zone 传空串即不写该字段——走「缺省 UTC」路径。
// writeWorkspaceWithSchedule builds a one-agent workspace with a single schedule.
func writeWorkspaceWithSchedule(t *testing.T, cronExpr, zone string) string {
	t.Helper()
	root := t.TempDir()
	fm := "\nname: sched-agent\n" +
		"provider: {name: deepseek, model: deepseek-chat}\n" +
		"schedules:\n" +
		"  - cron: \"" + cronExpr + "\"\n"
	if zone != "" {
		fm += "    zone: " + zone + "\n"
	}
	fm += "    message: 定时播报\n"
	writeAgentFrontmatter(t, root, "sched-agent", fm, "调度夹具正文")
	return root
}

// writeAgentFrontmatter 在 root/agents/<name>/AGENT.md 落一份围栏 frontmatter + 正文；
// frontmatter 片段的前导换行会被裁掉（调用点保留文档里的裸换行写法）。
// writeAgentFrontmatter writes one fenced AGENT.md.
func writeAgentFrontmatter(t *testing.T, root, name, frontmatter, body string) {
	t.Helper()
	front := strings.TrimLeft(frontmatter, "\n")
	if !strings.HasSuffix(front, "\n") {
		front += "\n"
	}
	writeFileInRoot(t, root, filepath.Join("agents", name, "AGENT.md"),
		"---\n"+front+"---\n\n"+body+"\n")
}

// writeFileInRoot 在 root/rel 落一个文件（自动建父目录；权限沿用工作区口径）。
// writeFileInRoot writes one file under the workspace root.
func writeFileInRoot(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

// writeMCPServers 在 root/mcp_servers.yaml 落一份顶层键名集（值为最小启动配置；
// 本 US 只读顶层键名）。
// writeMCPServers writes a top-level key set into mcp_servers.yaml.
func writeMCPServers(t *testing.T, root string, names []string) {
	t.Helper()
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "%s:\n  command: echo\n", name)
	}
	writeFileInRoot(t, root, "mcp_servers.yaml", b.String())
}

// logSinks 以 *testing.T 为键寄存每只测试的日志缓冲——bufferedLogger 保持 §4.3 的
// 单返回值调用形态（Logger: bufferedLogger(t)），取文经 logText(t)。纯测试设施。
// logSinks keys per-test log buffers so bufferedLogger can keep its single-value
// shape as written in the harness code.
var logSinks sync.Map // map[*testing.T]*logBuffer

type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// bufferedLogger 返回把日志写进本测试专属缓冲的 Logger——加载失败的 WARN 断言
// （固定键 agent / reason / detail，A13）由此取文。
// bufferedLogger returns a logger draining into this test's private buffer.
func bufferedLogger(t *testing.T) *slog.Logger {
	t.Helper()
	buf := &logBuffer{}
	logSinks.Store(t, buf)
	t.Cleanup(func() { logSinks.Delete(t) })
	return slog.New(slog.NewTextHandler(buf, nil))
}

// logText 取回本测试截至目前写进 bufferedLogger 的日志文本。
// logText returns everything bufferedLogger captured for this test so far.
func logText(t *testing.T) string {
	t.Helper()
	raw, ok := logSinks.Load(t)
	require.True(t, ok, "本用例未创建 bufferedLogger")
	return raw.(*logBuffer).String()
}

// newTestLoader 造一只 provider 名集显式注入、WARN 进测试缓冲的装载器
// （§2.4 注 2：名集由调用方注入，装载器不自己读配置；工具池缺省 = 空池）。
// newTestLoader builds a loader with the provider set injected explicitly and an
// empty tool pool.
func newTestLoader(t *testing.T, providers ...string) *workspace.AgentLoader {
	t.Helper()
	return newTestLoaderWithTools(t, nil, providers...)
}

// newTestLoaderWithTools 在 newTestLoader 之上注入注册池名字集（§2.4 注 4：
// 名字集由装配期从工具注册表导出后给，workspace 不反向依赖 tool 包）——只影响
// "声明了 tools: 的 Agent 能否注册"，未声明 tools: 的 Agent 不受影响。
// newTestLoaderWithTools injects the registered-tool name set on top of
// newTestLoader; it only affects agents that declare tools.
func newTestLoaderWithTools(t *testing.T, tools []string, providers ...string) *workspace.AgentLoader {
	t.Helper()
	return workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: providers,
		Tools:     tools,
		Logger:    bufferedLogger(t),
	})
}

// loadOnlyAgent 装载 root 并返回**唯一**的一条结果；要求该条已注册
// （兼容样例夹具的既定事实，段一用例据此取条目——样例声明了 10 个工具名，
// 故装载器须注入对应的注册池；§2.4 注 4）。
// loadOnlyAgent loads a single-agent workspace and returns the registered entry;
// the compat sample declares ten tools, so the loader injects that name set.
func loadOnlyAgent(t *testing.T, root string) core.AgentEntry {
	t.Helper()
	entries, err := newTestLoaderWithTools(t, compatSampleToolNames(), "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "夹具应当注册：%s", entries[0].RejectReason)
	return entries[0]
}

// 中文验收点：Compatibility §5.1 共享样例加载——注册成功、10 个工具名逐字解析、
// 技能声明即绑定、schedules 派生命中；判据 3：从**固定起点** 2026-10-01T00:00:00Z
// 起未来 10 次触发逐条相同（Next 严格晚于传入时刻——robfig v3.0.1 源码实测）。
// 改造落点（§2.4 注 4）：装载器注入 10 个名字作注册池——注册成功 + `tool not
// registered` **零命中**；样例 AGENT.md 一个字不动。
// Acceptance point: the shared compatibility sample loads with its ten names
// injected as the registered pool, and its cron fires on the same ten instants on
// both platforms from a fixed start.
func TestAgentLoader_LoadsCompatSampleAgent(t *testing.T) {
	root := writeCompatSampleWorkspace(t) // Compatibility §5.1 样例逐字：.basalt/agents/compat-check/AGENT.md
	// + .basalt/{AGENTS.md, SOUL.md, USER.md}（bootstrap 三件）+ .basalt/skills/digest-format/SKILL.md
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Tools:     compatSampleToolNames(), // 判据 2 前半：10 个名字全部在注册池内
		Logger:    bufferedLogger(t),
	})

	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	for _, e := range entries {
		require.NotContains(t, e.RejectReason, "tool not registered",
			"判据 2 反例面：已注册的 10 个名字一个都不得被误判为未注册")
	}

	entry := entries[0]
	require.True(t, entry.Registered, "样例必须注册：%s", entry.RejectReason)
	require.NotNil(t, entry.Profile)
	require.Equal(t, "compat-check", entry.Profile.Name)
	require.Equal(t, []string{
		"read_file", "write_file", "list_dir", "shell", "http_get", "http_post",
		"save_memory", "recall_memory", "notify", "manage_schedule",
	}, entry.Profile.Tools) // 判据 2：10 个名字全部命中
	require.Equal(t, []string{"digest-format"}, entry.Profile.Skills)
	require.Len(t, entry.Profile.Schedules, 1)

	sched := entry.Profile.Schedules[0]
	require.Equal(t, "compat-daily", sched.ID)
	require.Equal(t, "Asia/Shanghai", sched.Zone)

	spec, err := workspace.CronSpec(sched) // "CRON_TZ=Asia/Shanghai 0 0 8 * * *"
	require.NoError(t, err)
	require.Equal(t, "CRON_TZ=Asia/Shanghai 0 0 8 * * *", spec)

	parsed, err := cron.NewParser(
		cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(spec)
	require.NoError(t, err)

	// 固定起点：2026-10-01T00:00:00Z 恰是当日 08:00 +08:00——Next 严格晚于起点，
	// 故第一次落在次日；daily 08:00 +08:00 == 00:00:00 UTC。
	// Fixed start: the first firing is the NEXT day, because Next() is strictly after t.
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := start
	for i := 1; i <= 10; i++ {
		want := time.Date(2026, 10, 1+i, 0, 0, 0, 0, time.UTC) // 08:00 +08:00
		at = parsed.Next(at)
		require.True(t, want.Equal(at), "第 %d 次触发：want %s，got %s", i, want, at)
	}
}

// 中文验收点：注入 3 个名字（read_file / write_file / list_dir）→ 同一份兼容样例
// 被拒，reason 逐字 `tool not registered: shell`——报的是**声明序**里的第一个缺名
// （多缺名也只报一条；报哪条不以池的遍历顺序为转移）。
// Acceptance point: a declared-but-unregistered tool rejects the whole agent and
// the reason names the first missing name in declaration order.
func TestAgentLoader_RejectsAgentDeclaringUnregisteredTool(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Tools:     compatSampleToolNames()[:3], // 前三个：read_file / write_file / list_dir
		Logger:    bufferedLogger(t),
	})

	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0]
	require.False(t, entry.Registered, "声明的工具名不在注册池 → 整只不注册")
	require.Equal(t, "tool not registered: shell", entry.RejectReason,
		"reason 逐字点名声明序里的第一个缺名（shell）")
	require.Equal(t, compatSampleToolNames(), entry.Profile.Tools,
		"声明面照常解析进 Profile（未注册也可供 list / show 展示）")
	require.Contains(t, logText(t), "tool not registered: shell",
		"未注册同时经 WARN 点名（reason 取契约字面量原样）")
}

// 中文验收点：**空池语义**前半（§2.4 注 4）——装载器未注入任何工具名（`Tools` 零值）
// 时，未声明 `tools:` 的 Agent 照常注册，行为与改造前的占位一致。
// Acceptance point: an empty tool pool still registers an agent that declares no
// tools — the behavior before the gate landed.
func TestAgentLoader_EmptyToolPoolKeepsAgentWithoutToolsDeclaration(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "pool-less", `
name: pool-less
provider: {name: deepseek, model: deepseek-chat}
`, "正文占位")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root) // 工具池缺省 = 空池
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered,
		"空池不得拦下未声明 tools: 的 Agent：%s", entries[0].RejectReason)
	require.Empty(t, entries[0].Profile.Tools)
}

// 中文验收点：**空池语义**后半——`Tools` 零值且 Agent **声明了** `tools:` →
// 整只不注册，reason 逐字点名首个声明名（空池不是"全部放行"）。
// Acceptance point: the other half — a declared tool can never pass while nothing
// is registered, so the empty pool is not a blanket pass.
func TestAgentLoader_EmptyToolPoolRejectsAgentDeclaringTools(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "declaring", `
name: declaring
provider: {name: deepseek, model: deepseek-chat}
tools: [read_file]
`, "正文占位")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root) // 工具池缺省 = 空池
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered, "空池下声明的名字一律视作未注册")
	require.Equal(t, "tool not registered: read_file", entries[0].RejectReason)
}

// 中文验收点：目录名与 frontmatter name 逐字一致——不一致整只不注册且点名；
// 解析成功即有 Profile（未注册也可非 nil，供 list / show 展示声明面——§2.4 注 3）。
// Acceptance point: a name that differs from the directory name rejects the agent.
func TestAgentLoader_RejectsNameMismatch(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "alpha", `
name: beta
provider: {name: deepseek, model: deepseek-chat}
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0]
	require.False(t, entry.Registered, "name 与目录名不符必须整只不注册")
	require.Contains(t, entry.RejectReason, "name mismatch")
	require.NotNil(t, entry.Profile, "解析成功即有 Profile——未注册也可非 nil")
	require.Equal(t, "beta", entry.Profile.Name)
	require.Equal(t, "alpha", entry.Name)

	logs := logText(t)
	require.Contains(t, logs, "agent=alpha")
	require.Contains(t, logs, "name mismatch", "点名 WARN 的 reason 取契约字面量原样")
}

// 中文验收点：未注册 provider 拒载（规则 2）——名集由调用方显式注入。
// Acceptance point: a provider outside the injected name set rejects the agent.
func TestAgentLoader_RejectsUnregisteredProvider(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "ghostly", `
name: ghostly
provider: {name: ghost, model: deepseek-chat}
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.Contains(t, entries[0].RejectReason, "provider not registered: ghost")
	require.Contains(t, logText(t), "provider not registered: ghost")
}

// 中文验收点：model 空白拒载（规则 3）——空白含纯空格。
// Acceptance point: a blank model rejects the agent.
func TestAgentLoader_RejectsBlankModel(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "blanky", `
name: blanky
provider: {name: deepseek, model: "   "}
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.Contains(t, entries[0].RejectReason, "blank model")
}

// 中文验收点：脚手架占位文本「请在此填写模型名」是普通文本、天然放行
// （契约 §2 字段表：model 只判非空白）。
// Acceptance point: the scaffold placeholder for the model passes the blank check.
func TestAgentLoader_AcceptsPlaceholderModel(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "placeholder", `
name: placeholder
provider: {name: deepseek, model: 请在此填写模型名}
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "占位文本不得被当作空白拒载：%s", entries[0].RejectReason)
	require.Equal(t, "请在此填写模型名", entries[0].Profile.Provider.Model)
}

// 中文验收点：技能库无实体 → 整只不注册并点名（规则 5；声明即绑定的另一半）。
// Acceptance point: an undeclared skill entity rejects the agent.
func TestAgentLoader_RejectsMissingSkill(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "skilled", `
name: skilled
provider: {name: deepseek, model: deepseek-chat}
skills: [ghost-skill]
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.Contains(t, entries[0].RejectReason, "skill not found: ghost-skill")
}

// 中文验收点（R2 评审修复）：技能名**越界**必须判「无实体」——`../…` 这类声明经
// path.Join 清洁后会落到技能库之外（agents/<name>/skills/secret/SKILL.md 正好被
// 命中），它的 description 会随卡片进段一。判据：①越界名一律拒载且 reason 取契约
// 字面量 skill not found（越界名不得被解析成任何路径）；②名字必须是**单个目录名**
// （`..` / 路径分隔符都算越界），已在库内的回环形态（../skills/<name>）同样拒载。
// Acceptance point: a skill name that escapes the skill library is rejected as
// "no entity" on every platform, without ever building a path out of it.
func TestAgentLoader_RejectsSkillNameOutsideLibrary(t *testing.T) {
	cases := []struct {
		label string
		skill string
	}{
		{"escape_to_sibling_subtree", "../agents/esc-agent/skills/secret"},
		{"escape_back_into_library", "../skills/digest-format"},
		{"windows_separator", `..\agents\private`},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			root := t.TempDir()
			// 越界**命中物**：①Agent 本地 skills/ 目录内的实体（清洁后正好被读到）；
			// ②技能库里的正常实体（回环声明经清洁后也正好被读到）。
			writeFileInRoot(t, root, filepath.Join("agents", "esc-agent", "skills", "secret", "SKILL.md"),
				"---\nname: secret\ndescription: 越界命中的说明\n---\n\n越界正文\n")
			writeFileInRoot(t, root, filepath.Join("skills", "digest-format", "SKILL.md"), digestFormatSkillMD)
			writeAgentFrontmatter(t, root, "esc-agent",
				"\nname: esc-agent\nprovider: {name: deepseek, model: deepseek-chat}\nskills:\n  - "+tc.skill+"\n",
				"正文")

			entries, err := newTestLoader(t, "deepseek").LoadAll(root)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.False(t, entries[0].Registered, "越界技能名必须整只不注册")
			require.Contains(t, entries[0].RejectReason, "skill not found: "+tc.skill)
		})
	}
}

// 中文验收点：mcp_servers 只判存在性半条（规则 6）——名字不在 mcp_servers.yaml
// 顶层键内即拒载；「连接成功」半条归 US-7（§2.4 注 7）。
// Acceptance point: an MCP server missing from the registry rejects the agent.
func TestAgentLoader_RejectsUnconfiguredMCPServer(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "mcpy", `
name: mcpy
provider: {name: deepseek, model: deepseek-chat}
mcp_servers: [ghost-mcp]
`, "正文")
	writeMCPServers(t, root, []string{"search"}) // 配置里只有 search

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.Contains(t, entries[0].RejectReason, "mcp server skipped")
	require.Contains(t, entries[0].RejectReason, "ghost-mcp", "点名的 server 进 detail")
}

// 中文验收点：bootstrap 声明的文件必须存在（规则 7）——缺失点名文件路径。
// Acceptance point: a missing bootstrap file rejects the agent.
func TestAgentLoader_RejectsMissingBootstrapFile(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "booty", `
name: booty
provider: {name: deepseek, model: deepseek-chat}
bootstrap: [MISSING.md]
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.Contains(t, entries[0].RejectReason, "bootstrap file missing")
	require.Contains(t, entries[0].RejectReason, "MISSING.md")
}

// 中文验收点（R2 评审修复）：bootstrap 声明的条目必须是**普通文件**——目录在装载期
// 就拒载（此前要拖到段一读取时才炸）；reason 仍是契约 §2.1 的字面量 bootstrap file
// missing（本 US 不新增 reason 字面量），类型不符由 detail 点名。
// Acceptance point: a bootstrap entry that is not a regular file rejects the agent
// at load time, with the contract literal as the reason.
func TestAgentLoader_RejectsBootstrapDirectory(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, filepath.Join("BOOT_DIR", "inner.md"), "目录里的文件\n")
	writeAgentFrontmatter(t, root, "boot-agent", `
name: boot-agent
provider: {name: deepseek, model: deepseek-chat}
bootstrap: [BOOT_DIR]
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered, "bootstrap 指向目录必须整只不注册")
	require.Contains(t, entries[0].RejectReason, "bootstrap file missing")
	require.Contains(t, entries[0].RejectReason, "BOOT_DIR", "detail 点名条目")
	require.Contains(t, entries[0].RejectReason, "不是普通文件")
}

// 中文验收点：Agent 本地 skills/ 目录是**遗留形态**——被无视并记一条 WARN，
// 绑定结果不变（契约 §2.1 遗留形态备案；本地目录不得成为第二入口）。
// Acceptance point: a local skills/ directory is ignored with one WARN and does
// not change the binding result.
func TestAgentLoader_IgnoresLocalSkillsDirectory(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "legacy", `
name: legacy
provider: {name: deepseek, model: deepseek-chat}
`, "正文")
	writeFileInRoot(t, root, filepath.Join("agents", "legacy", "skills", "relic", "SKILL.md"),
		"旧形态残留——不得被识别为绑定")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	entry := entries[0]
	require.True(t, entry.Registered, "本地 skills/ 不得影响注册判定：%s", entry.RejectReason)
	require.Empty(t, entry.Profile.Skills, "绑定一律以 frontmatter skills: 声明为准")

	logs := logText(t)
	require.Contains(t, logs, "local skills directory ignored")
	require.Contains(t, logs, "legacy/skills", "WARN 点名本地目录路径（detail）")
}

// 中文验收点：单只失败不拖垮整批（规则 3 的另一半）——坏的那只不注册并点名，
// 好的那只照常注册。
// Acceptance point: one rejected agent does not block the others.
func TestAgentLoader_OneRejectedAgentDoesNotBlockOthers(t *testing.T) {
	root := t.TempDir()
	// 目录名字节序：aaa 在前——坏的那只先被装载，后来者仍须照常装载。
	writeAgentFrontmatter(t, root, "aaa", `
name: aaa
provider: {name: ghost, model: deepseek-chat}
`, "正文")
	writeAgentFrontmatter(t, root, "bbb", `
name: bbb
provider: {name: deepseek, model: deepseek-chat}
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 2, "失败不静默消失——每只目录恰一条结果")

	byName := make(map[string]core.AgentEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	require.False(t, byName["aaa"].Registered)
	require.Contains(t, byName["aaa"].RejectReason, "provider not registered: ghost")
	require.True(t, byName["bbb"].Registered, "其余 Agent 照常装载：%s", byName["bbb"].RejectReason)
}

// 中文验收点：frontmatter → Profile 的**逐字段映射**用非缺省值判卷（snake_case 键名逐字
// 对齐契约 §2：agent_name / mcp_servers / max_iterations / max_history_turns），且
// "未声明"与"显式零值 / 空列表"可区分（temperature: 0 与未声明不同；channels: [] 不回落 cli）。
// Acceptance point: snake_case keys map through an explicit DTO, and explicit zero
// values stay distinguishable from absent ones.
func TestAgentLoader_ParsesAllFrontmatterFields(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "mapper", `
name: mapper
description: 映射用例
identity:
  agent_name: 小帮手
  prompt: 你是映射用例。
provider:
  name: deepseek
  model: deepseek-chat
  temperature: 0
tools: [read_file]
skills: []
mcp_servers: [search]
bootstrap: [AGENTS.md]
channels: []
settings:
  max_iterations: 42
  max_history_turns: 7
schedules: []
`, "正文占位")
	writeFileInRoot(t, root, "AGENTS.md", "引导")
	writeMCPServers(t, root, []string{"search"})

	// 规则 4 门禁：夹具声明了 tools: [read_file] → 注册池里必须真有这个名字
	// （§2.4 注 4；映射断言本身逐字不变）。
	entries, err := newTestLoaderWithTools(t, []string{"read_file"}, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "样例应注册：%s", entries[0].RejectReason)

	p := entries[0].Profile
	require.Equal(t, "映射用例", p.Description)
	require.Equal(t, "小帮手", p.Identity.AgentName) // identity.agent_name（snake_case）
	require.Equal(t, "deepseek", p.Provider.Name)
	require.Equal(t, []string{"read_file"}, p.Tools)
	require.Equal(t, []string{"search"}, p.MCPServers) // mcp_servers（snake_case）
	require.Equal(t, []string{"AGENTS.md"}, p.Bootstrap)
	require.Empty(t, p.Channels, "显式 channels: [] 不得回落缺省 [{name: cli}]")
	require.Equal(t, 42, p.Settings.MaxIterations)  // max_iterations（snake_case）
	require.Equal(t, 7, p.Settings.MaxHistoryTurns) // max_history_turns（snake_case）
	require.NotNil(t, p.Provider.Temperature, "显式 temperature: 0 与未声明不同")
	require.Equal(t, 0.0, *p.Provider.Temperature)
}

// 中文验收点：缺省填充只补"未声明"——agent_name 回填 name、settings 10 / 20、
// channels 回落 [{name: cli}]、temperature 保持 nil（供应商缺省）。
// Acceptance point: absent fields take the documented defaults, and only those.
func TestAgentLoader_FillsDocumentedDefaultsWhenAbsent(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "minimal", `
name: minimal
description: 极简声明面
provider: {name: deepseek, model: deepseek-chat}
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "极简声明面必须能注册：%s", entries[0].RejectReason)

	p := entries[0].Profile
	require.Equal(t, "minimal", p.Identity.AgentName, "identity.agent_name 未声明 → 回填 name")
	require.Empty(t, p.Identity.Prompt)
	require.Equal(t, 10, p.Settings.MaxIterations, "settings.max_iterations 未声明 → 10")
	require.Equal(t, 20, p.Settings.MaxHistoryTurns, "settings.max_history_turns 未声明 → 20")
	require.Equal(t, []core.Channel{{Name: "cli"}}, p.Channels, "channels 未声明 → [{name: cli}]")
	require.Nil(t, p.Provider.Temperature, "temperature 未声明 = nil（供应商缺省）")
	require.Empty(t, p.Tools, "tools 不声明 = 空池")
	require.Empty(t, p.Schedules)
}

// 中文验收点：显式空列表与未声明是两件事——channels: [] 不得回落 [{name: cli}]
// （契约 §2 字段表：缺省列与显式给出是两件事）。
// Acceptance point: an explicit empty channels list is not the default.
func TestAgentLoader_EmptyChannelsStaysEmpty(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "channel-less", `
name: channel-less
provider: {name: deepseek, model: deepseek-chat}
channels: []
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "%s", entries[0].RejectReason)
	require.Empty(t, entries[0].Profile.Channels, "显式 [] 保持为空，不回落 [{name: cli}]")
}

// 中文验收点：未知键宽容但可见（R1 裁决）——字段表之外的键不拒载（前向兼容：
// 基线新增可选项不得升级成"双底座不兼容"——§2.4 注 5）。
// Acceptance point: unknown frontmatter keys are tolerated, not fatal.
func TestAgentLoader_ToleratesUnknownFrontmatterKeys(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "forward", `
name: forward
provider: {name: deepseek, model: deepseek-chat}
future_option: yes
future_section:
  nested_option: 1
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "未知键不得拒载：%s", entries[0].RejectReason)
	require.NotNil(t, entries[0].Profile)
}

// 中文验收点：未知键 WARN 的 detail **只放键路径、不打印值**（路径集先排序后输出）
// ——未知键的值是"还没理解"的输入，落日志等于把未审计内容写进第二份记录（§2.2 约束 13）。
// Acceptance point: the unknown-key WARN names the key path and never the value.
func TestAgentLoader_UnknownKeyWarnNamesPathWithoutValue(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "extra", `
name: extra
provider: {name: deepseek, model: deepseek-chat}
secret_extra: s3cr3t-value
identity:
  agent_name: 别名
  legacy_field: s3cr3t-nested
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "%s", entries[0].RejectReason)

	logs := logText(t)
	require.Contains(t, logs, "unknown frontmatter key")
	require.Contains(t, logs, "secret_extra", "detail 放键路径（顶层）")
	require.Contains(t, logs, "identity.legacy_field", "detail 放键路径（嵌套）")
	require.NotContains(t, logs, "s3cr3t-value", "未知键的值不得进日志")
	require.NotContains(t, logs, "s3cr3t-nested", "嵌套未知键的值不得进日志")
}

// 中文验收点：temperature 已声明则必须落在 0–2（契约 §2 的字段约束在装载期执行
// ——越界值到 US-4 构造请求才炸就太晚了——§2.4 注 9）。
// Acceptance point: a declared temperature outside 0–2 rejects the agent.
func TestAgentLoader_RejectsOutOfRangeTemperature(t *testing.T) {
	cases := []struct {
		name        string
		temperature string
	}{
		{"above_range", "3"},
		{"negative", "-0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeAgentFrontmatter(t, root, "hot", `
name: hot
provider:
  name: deepseek
  model: deepseek-chat
  temperature: `+tc.temperature+`
`, "正文")

			entries, err := newTestLoader(t, "deepseek").LoadAll(root)
			require.NoError(t, err)
			require.False(t, entries[0].Registered, "越界温度必须拒载")
			require.Contains(t, entries[0].RejectReason, "invalid provider.temperature")
		})
	}
}

// 中文验收点：NaN / ±Inf 拒（有限性判定——只判 0–2 区间会放过 NaN：NaN 与任何数的
// 比较都是 false，区间检查拦不住它）。
// Acceptance point: non-finite temperatures are rejected.
func TestAgentLoader_RejectsNonFiniteTemperature(t *testing.T) {
	cases := []struct {
		name        string
		temperature string
	}{
		{"nan", ".nan"},
		{"positive_infinity", ".inf"},
		{"negative_infinity", "-.inf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeAgentFrontmatter(t, root, "infinite", `
name: infinite
provider:
  name: deepseek
  model: deepseek-chat
  temperature: `+tc.temperature+`
`, "正文")

			entries, err := newTestLoader(t, "deepseek").LoadAll(root)
			require.NoError(t, err)
			require.False(t, entries[0].Registered, "非有限温度必须拒载")
			require.Contains(t, entries[0].RejectReason, "invalid provider.temperature")
		})
	}
}

// 中文验收点：解析错误文案压缩为「位置 + 自产类别」——库文案一个字不外传，
// **不得**回显 YAML 原始片段（陷阱 40：库文案会带出错处源码；SENTINEL 探针即判据）。
// Acceptance point: a parse error never quotes the offending source.
func TestAgentLoader_ParseErrorOmitsSourceExcerpt(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "broken", `
name: broken
provider: {name: deepseek, model: *SENTINEL-ALIAS}
`, "正文")

	entries, err := newTestLoader(t, "deepseek").LoadAll(root)
	require.NoError(t, err)
	entry := entries[0]
	require.False(t, entry.Registered, "解析失败必须整只不注册")
	require.Nil(t, entry.Profile, "解析失败时 Profile 为 nil——声明面不可得")
	require.Contains(t, entry.RejectReason, "invalid frontmatter")
	require.NotContains(t, entry.RejectReason, "SENTINEL-ALIAS", "不得回显源码片段")

	logs := logText(t)
	require.Contains(t, logs, "invalid frontmatter")
	require.NotContains(t, logs, "SENTINEL-ALIAS", "日志同样不得回显源码片段")
}
