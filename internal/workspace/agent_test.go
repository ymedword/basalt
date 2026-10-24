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

// writeMCPServers 在 root/mcp_servers.yaml 落一份 **servers: 列表**形态的最小配置
// （US-7 起解析器只认这一形态；名字集 = 列表里的 name，顺序即文件序）。
// writeMCPServers writes a servers-list shaped configuration; the name set is the
// list's names in file order.
func writeMCPServers(t *testing.T, root string, names []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("servers:\n")
	for _, name := range names {
		fmt.Fprintf(&b, "  - name: %s\n    command: echo\n", name)
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
// MCP 两个名字集随 US-7 的装载器接口扩入：`mcpServers == nil` = 未注入（装载器自读
// 配置，轻路径形态）、`mcpConnected == nil` = 连接结果未知（第三态 unverified）。
// newTestLoader builds a loader with the provider set injected explicitly and an
// empty tool pool; nil MCP name sets mean "read config here" and "connection unknown".
func newTestLoader(t *testing.T, mcpServers, mcpConnected []string, providers ...string) *workspace.AgentLoader {
	t.Helper()
	return newTestLoaderWithTools(t, nil, mcpServers, mcpConnected, providers...)
}

// newTestLoaderWithTools 在 newTestLoader 之上注入注册池名字集（§2.4 注 4：
// 名字集由装配期从工具注册表导出后给，workspace 不反向依赖 tool 包）——只影响
// "声明了 tools: 的 Agent 能否注册"，未声明 tools: 的 Agent 不受影响。
// 重路径的两条 MCP 名字集（已配置 / 已连接）由调用方显式给出；nil 的语义见上。
// newTestLoaderWithTools injects the registered-tool and MCP name sets on top of
// newTestLoader; it only affects agents that declare those names.
func newTestLoaderWithTools(t *testing.T, tools, mcpServers, mcpConnected []string, providers ...string) *workspace.AgentLoader {
	t.Helper()
	return workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers:    providers,
		Tools:        tools,
		MCPServers:   mcpServers,
		MCPConnected: mcpConnected,
		Logger:       bufferedLogger(t),
	})
}

// loadOnlyAgent 装载 root 并返回**唯一**的一条结果；要求该条已注册
// （兼容样例夹具的既定事实，段一用例据此取条目——样例声明了 10 个工具名，
// 故装载器须注入对应的注册池；§2.4 注 4）。
// loadOnlyAgent loads a single-agent workspace and returns the registered entry;
// the compat sample declares ten tools, so the loader injects that name set.
func loadOnlyAgent(t *testing.T, root string) core.AgentEntry {
	t.Helper()
	entries, err := newTestLoaderWithTools(t, compatSampleToolNames(), nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root) // 工具池缺省 = 空池
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root) // 工具池缺省 = 空池
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered, "空池下声明的名字一律视作未注册")
	require.Equal(t, "tool not registered: read_file", entries[0].RejectReason)
}

// 中文验收点：**模板声明 ↔ 注册池成对**（US-6：颗粒度文档 §6.2 口径 26 / spec FR-026 /
// 宪法 IV 连带）——`CreateAgent` 造出的 Agent 声明两件记忆工具：池含这两件时**正常注册**
// 且 `Profile.Tools` 逐字，池为空时**整只不注册**且 reason 点名首个缺名。
//
// 声明面刻意取自**真模板**而非本文件的手写 frontmatter 夹具：本条判的是"模板默认工具与
// 注册池两边同时演进"，手写声明会让两条断言在池与模板脱钩时照常绿——那正是 US-6 真正的
// 装配风险（新造的 Agent 一到池外就整只不注册，`profile create` 出来即死）。
// Acceptance point: the created template's declared memory tools and the assembly
// pool must agree — declared + in pool registers; declared + empty pool does not.
func TestAgentLoader_MemoryToolsInPoolRegisterAgent(t *testing.T) {
	memoryTools := []string{"save_memory", "recall_memory"}

	root := initWorkspaceRoot(t)
	_, err := workspace.CreateAgentStaged(root, core.AgentSpec{Name: "remembering-agent"}, []string{"deepseek"})
	require.NoError(t, err)

	t.Run("池含两件", func(t *testing.T) {
		entries, err := newTestLoaderWithTools(t, memoryTools, nil, nil, "deepseek").LoadAll(root)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.True(t, entries[0].Registered, "池含两件 → 应当注册：%s", entries[0].RejectReason)
		require.Equal(t, memoryTools, entries[0].Profile.Tools,
			"模板声明的工具名逐字进 Profile（声明序不变）")
	})

	t.Run("空池", func(t *testing.T) {
		entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root) // 工具池缺省 = 空池
		require.NoError(t, err)
		require.Len(t, entries, 1)
		require.False(t, entries[0].Registered, "空池下声明的记忆工具名一律视作未注册")
		require.Equal(t, "tool not registered: save_memory", entries[0].RejectReason,
			"reason 逐字点名声明序里的首个缺名")
	})
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.Contains(t, entries[0].RejectReason, "provider not registered: ghost")
	require.Contains(t, logText(t), "provider not registered: ghost")
}

// 中文验收点：provider 名集取动态来源时，规则 2 判的是**每次**读到的当前名集——
// 启动后发布的模型连接无需重启即可被 Agent 绑定；名字撤下后下一次装载立即拒载。
// 动态来源优先于构造期快照（静态名单里的名字不再被采信）。
// Acceptance point: a runtime-published provider name binds without a restart and
// the dynamic source overrides the construction-time snapshot.
func TestAgentLoader_DynamicProviderSet(t *testing.T) {
	root := writeCompatSampleWorkspace(t)
	published := []string{"deepseek"}
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers:     []string{"only-at-startup"},
		ProvidersFunc: func() []string { return published },
		Tools:         compatSampleToolNames(),
		Logger:        bufferedLogger(t),
	})

	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "动态名集里的 provider 必须放行：%s", entries[0].RejectReason)

	published = nil
	entries, err = loader.LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.Equal(t, "provider not registered: deepseek", entries[0].RejectReason)

	// 动态来源非 nil 时构造期快照不再是判据：静态名单里的名字不被采信。
	published = []string{"ghost"}
	entries, err = loader.LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.Equal(t, "provider not registered: deepseek", entries[0].RejectReason)
}

// 中文验收点：model 空白拒载（规则 3）——空白含纯空格。
// Acceptance point: a blank model rejects the agent.
func TestAgentLoader_RejectsBlankModel(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "blanky", `
name: blanky
provider: {name: deepseek, model: "   "}
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

			entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.False(t, entries[0].Registered, "越界技能名必须整只不注册")
			require.Contains(t, entries[0].RejectReason, "skill not found: "+tc.skill)
		})
	}
}

// 中文验收点：mcp_servers 只判存在性半条（规则 6）——名字不在 mcp_servers.yaml
// 的 servers 列表内即拒载；「连接成功」半条随 US-7 的装配/装载器接线补齐。
// Acceptance point: an MCP server missing from the registry rejects the agent.
func TestAgentLoader_RejectsUnconfiguredMCPServer(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "mcpy", `
name: mcpy
provider: {name: deepseek, model: deepseek-chat}
mcp_servers: [ghost-mcp]
`, "正文")
	writeMCPServers(t, root, []string{"search"}) // 配置里只有 search

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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
notify_channels: [ops-alerts]
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
	// 规则 6 全条（US-7）：`search` 已配置**且已连接**才放行——两名单显式注入，
	// 旧断言"文件里存在"由此升级为"已配置且已连接"的判据面。
	entries, err := newTestLoaderWithTools(t, []string{"read_file"}, []string{"search"}, []string{"search"}, "deepseek").LoadAll(root)
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
	require.Equal(t, []string{"ops-alerts"}, p.NotifyChannels, "notify_channels（snake_case）")
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "%s", entries[0].RejectReason)
	require.Empty(t, entries[0].Profile.Channels, "显式 [] 保持为空，不回落 [{name: cli}]")
}

// 中文验收点：notify_channels 的空白条目无法匹配任何渠道 → 整只不注册
// （WARN invalid notify_channels）；渠道是否存在属于运行期注册表，装载器不判。
// Acceptance point: blank allowlist entries reject the agent; the loader does not
// own channel existence (that is a runtime registry concern).
func TestAgentLoader_NotifyChannelsBlankRejected(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "blank-notify", `
name: blank-notify
provider: {name: deepseek, model: deepseek-chat}
notify_channels: ["  "]
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered, "空白渠道名必须整只不注册")
	require.Contains(t, entries[0].RejectReason, "invalid notify_channels")
	require.Contains(t, entries[0].RejectReason, "不得为空白")
}

// 中文验收点：声明了尚未登记的渠道名不拒载——存在性由运行期注册表判定，
// notify 工具在调用时返回 not_found（与删除闸门/引用视图同口径）。
// Acceptance point: unknown channel names stay loadable; the runtime registry decides.
func TestAgentLoader_NotifyChannelsUnknownNameStaysLoadable(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "ghost-notify", `
name: ghost-notify
provider: {name: deepseek, model: deepseek-chat}
notify_channels: [ghost-channel]
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "渠道存在性不由装载器判定：%s", entries[0].RejectReason)
	require.Equal(t, []string{"ghost-channel"}, entries[0].Profile.NotifyChannels)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

			entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

			entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
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

// TestAgentLoader_DeclaredServerConnectedRegistersAgent：规则 6 全条——已配置且已连接 → 注册，
// 第三态不触发（对照组；判别力在 R4 的拒载方向）。
// Acceptance point: a configured and connected server registers the agent and leaves
// Unverified false.
func TestAgentLoader_DeclaredServerConnectedRegistersAgent(t *testing.T) {
	root := t.TempDir()
	writeMCPServers(t, root, []string{"search"})
	writeAgentFrontmatter(t, root, "scout", `
name: scout
provider: {name: probe, model: probe-model}
mcp_servers: [search]
`, "")

	entries, err := newTestLoader(t, []string{"search"}, []string{"search"}, "probe").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "已配置且已连接必须注册：%s", entries[0].RejectReason)
	require.False(t, entries[0].Unverified)
	require.Empty(t, entries[0].RejectReason)
}

// R4 —— 规则 6 的「连接成功」半条：存在但未连接的 server 与不存在的 server 同判。
// 逆改点：只查 mcpNames 不查 mcpConnected → 第一组由拒转收，红；
//
//	守卫"接线忘了传连接结果"：传 nil 会走 unverified 分支，第二组红。
//
// R4: a configured but unconnected server is rejected exactly like a missing one.
func TestAgentLoader_ConfiguredButNotConnectedRejects(t *testing.T) {
	root := t.TempDir()
	writeFileInRoot(t, root, "mcp_servers.yaml", `
servers:
  - name: search
    command: search-mcp
`)
	writeAgentFrontmatter(t, root, "scout", `
name: scout
provider: {name: probe, model: probe-model}
mcp_servers: [search]
`, "")

	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers:    []string{"probe"},
		MCPServers:   []string{"search"},
		MCPConnected: []string{}, // 已配置、未连上
	}).LoadAll(root)
	require.NoError(t, err)
	require.False(t, entries[0].Registered)
	require.False(t, entries[0].Unverified)
	require.Contains(t, entries[0].RejectReason, "mcp server skipped")
	require.Contains(t, entries[0].RejectReason, "search", "detail 点名 server")
}

// TestAgentLoader_LightPathMarksMCPAgentsUnverified（待决 #2）：轻路径不连接 MCP——
// MCPConnected == nil 且配置非空时，可能依赖 MCP 的 Agent 走第三态；确定性规则仍先拒。
// 逆改点：第三态未实现 → Unverified 恒 false；把待确认当放行 → Registered 红。
// Acceptance point: the light path reports unverified without rejecting, while
// deterministic rules still reject first.
func TestAgentLoader_LightPathMarksMCPAgentsUnverified(t *testing.T) {
	root := t.TempDir()
	writeMCPServers(t, root, []string{"search"})
	writeAgentFrontmatter(t, root, "mcp-only", `
name: mcp-only
provider: {name: probe, model: probe-model}
mcp_servers: [search]
`, "")
	writeAgentFrontmatter(t, root, "bad-provider", `
name: bad-provider
provider: {name: ghost, model: probe-model}
mcp_servers: [search]
`, "")

	entries, err := newTestLoader(t, nil, nil, "probe").LoadAll(root) // 两名单皆 nil = 轻路径
	require.NoError(t, err)
	require.Len(t, entries, 2)

	byName := make(map[string]core.AgentEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	require.False(t, byName["mcp-only"].Registered)
	require.True(t, byName["mcp-only"].Unverified, "轻路径必须给第三态")
	require.Empty(t, byName["mcp-only"].RejectReason, "待确认不是拒载")
	require.Contains(t, logText(t), "Agent 注册待确认")

	require.False(t, byName["bad-provider"].Registered, "确定性规则仍先拒")
	require.False(t, byName["bad-provider"].Unverified)
	require.Contains(t, byName["bad-provider"].RejectReason, "provider not registered: ghost")
}

// TestAgentLoader_NoMCPConfigKeepsTwoStateBehavior：零 MCP 配置（无文件）时与 US-6 逐字节
// 同判——普通 Agent 照常注册；声明了 server 的仍按规则 6 拒载；第三态不出现。
// 逆改点：把「无配置」也当待确认 → 两处 Unverified 断言红；把 nil connected 一律放行
// → 拒载断言红。
// Acceptance point: without MCP configuration the two-state behavior is unchanged.
func TestAgentLoader_NoMCPConfigKeepsTwoStateBehavior(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "plain", `
name: plain
provider: {name: probe, model: probe-model}
`, "")
	writeAgentFrontmatter(t, root, "declarer", `
name: declarer
provider: {name: probe, model: probe-model}
mcp_servers: [search]
`, "")

	entries, err := newTestLoader(t, nil, nil, "probe").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 2)

	byName := make(map[string]core.AgentEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	require.True(t, byName["plain"].Registered, "零配置下普通 Agent 照常注册：%s", byName["plain"].RejectReason)
	require.False(t, byName["plain"].Unverified)

	require.False(t, byName["declarer"].Registered)
	require.False(t, byName["declarer"].Unverified, "零配置下不存在第三态")
	require.Contains(t, byName["declarer"].RejectReason, "mcp server skipped")
	require.Contains(t, byName["declarer"].RejectReason, "search")
}

// 中文验收点（T057 / 模块卡 §3.1.13 图 D）：内容条件写的装载期闸门——候选经与装载
// 同一道规则 1–8 判定后返回已注册条目、段一正文与冻结清单；校验只读内存字节，磁盘
// 原文逐字节不变。逆改点：跳过规则 / 返回未注册条目 / 校验时写盘 → 断言红。
func TestAgentLoader_ValidateCandidate_AcceptsCandidate(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "alpha", "name: alpha\nprovider: {name: deepseek, model: deepseek-chat}\n", "原始正文")
	loader := newTestLoader(t, nil, nil, "deepseek")
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "夹具必须已注册：%s", entries[0].RejectReason)

	candidate := []byte("---\nname: alpha\nprovider: {name: deepseek, model: deepseek-chat}\n---\n\n新正文\n")
	entry, section, manifest, err := loader.ValidateCandidate(root, entries, "alpha", candidate,
		time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.True(t, entry.Registered, "合法候选返回已注册条目")
	require.Equal(t, "alpha", entry.Name)
	require.Contains(t, section, "新正文", "段一取候选字节，不取磁盘旧文")
	require.NotEmpty(t, manifest, "清单至少含 AGENT.md 一条")

	onDisk, readErr := os.ReadFile(filepath.Join(root, "agents", "alpha", "AGENT.md"))
	require.NoError(t, readErr)
	require.Contains(t, string(onDisk), "原始正文", "候选校验不得落盘")
	require.NotContains(t, string(onDisk), "新正文")
}

// 中文验收点（T057）：候选违反装载规则时按装载口径拒载，错误文案取 reject 摘要——
// 方向名不一致与空 model 各有固定 reason。逆改点：放行坏候选 → 断言红。
func TestAgentLoader_ValidateCandidate_RejectsInvalidCandidates(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "alpha", "name: alpha\nprovider: {name: deepseek, model: deepseek-chat}\n", "正文")
	loader := newTestLoader(t, nil, nil, "deepseek")
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		candidate string
		wantPart  string
	}{
		{
			name:      "frontmatter name 与目录名不一致",
			candidate: "---\nname: other\nprovider: {name: deepseek, model: deepseek-chat}\n---\n\n正文\n",
			wantPart:  "name mismatch",
		},
		{
			name:      "空 model",
			candidate: "---\nname: alpha\nprovider: {name: deepseek, model: \"\"}\n---\n\n正文\n",
			wantPart:  "blank model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := loader.ValidateCandidate(root, entries, "alpha", []byte(tc.candidate), now)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantPart)
		})
	}
}

// 中文验收点（T057 / 图 D）：候选的调度 id 占用以**其他**已注册 Agent 为基线——
// 抢占别人的 id 拒载；保留自己的旧 id 放行（被编辑 Agent 的旧占用不参与）。并发保存
// 只有一个成功的判定由调度占用基线 + AgentGate 写锁共同保证。
// 逆改点：占用基线包含被编辑 Agent 自身（任何保留旧 id 的保存都被拒）/ 不含其他
// Agent（冲突放行）→ 断言红。
func TestAgentLoader_ValidateCandidate_ScheduleClaims(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "alpha", `
name: alpha
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - id: daily-alpha
    cron: "0 0 8 * * *"
    message: alpha 播报
`, "alpha 正文")
	writeAgentFrontmatter(t, root, "beta", `
name: beta
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - id: daily-beta
    cron: "0 0 9 * * *"
    message: beta 播报
`, "beta 正文")
	loader := newTestLoader(t, nil, nil, "deepseek")
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, entry := range entries {
		require.True(t, entry.Registered, "夹具必须全部注册：%s %s", entry.Name, entry.RejectReason)
	}

	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	conflict := []byte("---\nname: beta\nprovider: {name: deepseek, model: deepseek-chat}\n" +
		"schedules:\n  - id: daily-alpha\n    cron: \"0 0 9 * * *\"\n    message: beta 播报\n---\n\nbeta 正文\n")
	_, _, _, err = loader.ValidateCandidate(root, entries, "beta", conflict, now)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid schedule")
	require.Contains(t, err.Error(), "id 已被 alpha 占用：daily-alpha")

	own := []byte("---\nname: beta\nprovider: {name: deepseek, model: deepseek-chat}\n" +
		"schedules:\n  - id: daily-beta\n    cron: \"0 0 9 * * *\"\n    message: beta 播报（改）\n---\n\nbeta 新正文\n")
	entry, _, _, err := loader.ValidateCandidate(root, entries, "beta", own, now)
	require.NoError(t, err, "被编辑 Agent 保留自己的调度 id 不得与自身旧占用冲突")
	require.True(t, entry.Registered)
}

// writeRewrittenSampleWorkspace 砌出「共享样例改名后」的工作区：把 Compatibility §5.1
// 样例的 name / identity.agent_name 改写成 newName，落到 agents/<newName>/，bootstrap
// 三件与技能库照抄共享样例——复制 / 恢复路径在磁盘上的产物形态。
// writeRewrittenSampleWorkspace materializes the shared sample under a new name.
func writeRewrittenSampleWorkspace(t *testing.T, newName string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".basalt")
	rewritten, err := workspace.RewriteAgentName([]byte(compatSampleAgentMD), newName)
	require.NoError(t, err)
	writeFileInRoot(t, root, filepath.Join("agents", newName, "AGENT.md"), string(rewritten))
	writeFileInRoot(t, root, "AGENTS.md", "# AGENTS.md\n\n项目级行为说明占位。\n")
	writeFileInRoot(t, root, "SOUL.md", "# SOUL.md\n\n人格定义占位。\n")
	writeFileInRoot(t, root, "USER.md", "# USER.md\n\n用户偏好占位。\n")
	writeFileInRoot(t, root, filepath.Join("skills", "digest-format", "SKILL.md"), digestFormatSkillMD)
	return root
}

// 中文验收点（约束 31）：改名后的 Agent 仍按**新名字**整只注册——除目录名与两处名字
// 字段外，provider / tools / skills / bootstrap / settings / schedules 逐项不变。
// Acceptance point: a renamed copy registers under the new name with every other
// profile field unchanged.
func TestAgentLoader_RewrittenNameKeepsProfile(t *testing.T) {
	root := writeRewrittenSampleWorkspace(t, "compat-copy")
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Tools:     compatSampleToolNames(),
		Logger:    bufferedLogger(t),
	})

	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	entry := entries[0]
	require.True(t, entry.Registered, "改名副本必须可装载：%s", entry.RejectReason)
	require.Equal(t, "compat-copy", entry.Name)
	require.Equal(t, "compat-copy", entry.Profile.Name)
	require.Equal(t, "compat-copy", entry.Profile.Identity.AgentName)
	require.Equal(t, "deepseek", entry.Profile.Provider.Name)
	require.Equal(t, "deepseek-chat", entry.Profile.Provider.Model)
	require.Equal(t, compatSampleToolNames(), entry.Profile.Tools)
	require.Equal(t, []string{"digest-format"}, entry.Profile.Skills)
	require.Equal(t, []string{"AGENTS.md", "SOUL.md", "USER.md"}, entry.Profile.Bootstrap)
	require.Equal(t, "你是一个用于兼容性验证的助手。", entry.Profile.Identity.Prompt)
}

// 中文验收点（约束 31）：改名不动 schedules——cron 表达式 / 时区 / 正文原样解析
// （复制路径另有"清空定时任务"的编排，本层只保证文件字节不被改写破坏）。
// Acceptance point: rewriting the name leaves the schedules block byte-identical.
func TestAgentLoader_RewrittenNameKeepsSchedules(t *testing.T) {
	root := writeRewrittenSampleWorkspace(t, "compat-copy")
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Tools:     compatSampleToolNames(),
		Logger:    bufferedLogger(t),
	})

	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "%s", entries[0].RejectReason)
	require.Len(t, entries[0].Profile.Schedules, 1)

	spec, err := workspace.CronSpec(entries[0].Profile.Schedules[0])
	require.NoError(t, err)
	require.Equal(t, "CRON_TZ=Asia/Shanghai 0 0 8 * * *", spec)
}

// 中文验收点（反例，说明改写是**必需**的）：把样例原样拷到新目录、不改写名字 →
// 装载器按"目录名 ≠ frontmatter name"整只不注册（只复制不改名的实现会静默失效）。
// Acceptance point: a copy without the rewrite is rejected by the loader — which is
// why the rewrite is mandatory.
func TestAgentLoader_CopyWithoutRewriteIsRejected(t *testing.T) {
	root := writeRewrittenSampleWorkspace(t, "compat-copy")
	// 把未改写的样例放回同一新目录（覆盖改写后的文件），模拟"只拷贝不改名"。
	writeFileInRoot(t, root, filepath.Join("agents", "compat-copy", "AGENT.md"), compatSampleAgentMD)

	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Tools:     compatSampleToolNames(),
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered, "目录名与 frontmatter name 不一致时必须整只不注册")
	require.Contains(t, entries[0].RejectReason, "不一致")
}

// 绑定夹具用 UID：与 bindings_test.go 的同名常量值一致；test 包读取的是装配面
// 暴露出来的值，不依赖白盒实现。
const (
	bindingModelUID  = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	bindingNotifyUID = "rs_01J8Z7A1B2C3D4E5F6G7H8J9K0"
)

// bindingsFrontmatter 拼一只声明了通知模式与完整绑定的 Agent。
// bindingsFrontmatter builds an agent declaring a notify mode and full bindings.
func bindingsFrontmatter(name string) string {
	return "name: " + name + "\n" +
		"provider: {name: deepseek, model: deepseek-chat}\n" +
		"notify_mode: allowlist\n" +
		"notify_channels: [ops]\n" +
		"resource_bindings:\n" +
		"  model_connection:\n" +
		"    - name: deepseek\n" +
		"      uid: " + bindingModelUID + "\n" +
		"  notify_channel:\n" +
		"    - name: ops\n" +
		"      uid: " + bindingNotifyUID + "\n"
}

// 中文验收点：`resource_bindings` 与 `notify_mode` 经装载面暴露到 Profile——解析只做
// 结构校验（规则 11），绑定与引用的一一对应属于保存期 / 准入判据，不在加载期判。
// Acceptance point: both fields surface on the parsed profile; correspondence is not
// a load-time concern.
func TestAgentLoader_ExposesResourceBindingsAndNotifyMode(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "bound", bindingsFrontmatter("bound"), "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "%s", entries[0].RejectReason)
	require.NotNil(t, entries[0].Profile)
	require.Equal(t, core.NotifyModeAllowlist, entries[0].Profile.NotifyMode)
	require.Equal(t, []core.ResourceBinding{
		{Name: "deepseek", UID: core.ResourceUID(bindingModelUID)},
	}, entries[0].Profile.ResourceBindings.ModelConnections)
	require.Equal(t, []core.ResourceBinding{
		{Name: "ops", UID: core.ResourceUID(bindingNotifyUID)},
	}, entries[0].Profile.ResourceBindings.NotifyChannels)
}

// 中文验收点：模板占位符与旧 Agent 仍可加载——未声明 `resource_bindings` 或只声明
// 部分引用都不影响装载（契约 agent-md §2.1「加载期不要求完整」）。
// Acceptance point: absent or incomplete bindings never block loading.
func TestAgentLoader_MissingBindingsStayLoadable(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "unbound", `
name: unbound
provider: {name: deepseek, model: deepseek-chat}
notify_mode: all_eligible
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "%s", entries[0].RejectReason)
	require.Equal(t, core.NotifyModeAllEligible, entries[0].Profile.NotifyMode)
	require.Empty(t, entries[0].Profile.ResourceBindings.ModelConnections)
	require.Empty(t, entries[0].Profile.ResourceBindings.NotifyChannels)
}

// 中文验收点：规则 10——`notify_mode` 取值合法且组合成立（none / all_eligible 时
// 白名单为空；allowlist 至少一个），否则整只不注册并点名 `invalid notify_mode`。
// Acceptance point: rule 10 rejects illegal literals and illegal combinations.
func TestAgentLoader_InvalidNotifyModeRejected(t *testing.T) {
	cases := map[string]string{
		"非法字面量":            "notify_mode: sometimes\n",
		"none 带渠道":         "notify_mode: none\nnotify_channels: [ops]\n",
		"all_eligible 带渠道": "notify_mode: all_eligible\nnotify_channels: [ops]\n",
		"allowlist 无渠道":    "notify_mode: allowlist\n",
	}
	for name, block := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeAgentFrontmatter(t, root, "mode-agent",
				"\nname: mode-agent\nprovider: {name: deepseek, model: deepseek-chat}\n"+block, "正文")

			entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.False(t, entries[0].Registered, "规则 10 不通过必须整只不注册")
			require.Contains(t, entries[0].RejectReason, "invalid notify_mode")
			require.Contains(t, logText(t), "invalid notify_mode")
		})
	}
}

// 中文验收点：绑定块结构非法（规则 11）→ WARN 类别是 `invalid resource_bindings`
// （与 generic frontmatter 失败分开），整只不注册。
// Acceptance point: a structurally invalid block reports its own WARN category.
func TestAgentLoader_InvalidResourceBindingsRejected(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "broken-bindings", `
name: broken-bindings
provider: {name: deepseek, model: deepseek-chat}
resource_bindings:
  model_connection:
    - name: deepseek
  notify_channel: []
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Registered, "绑定块结构非法必须整只不注册")
	require.Contains(t, entries[0].RejectReason, "invalid resource_bindings")
	require.Contains(t, logText(t), `reason="invalid resource_bindings"`)
	require.NotContains(t, logText(t), `reason="invalid frontmatter"`)
}

// 中文验收点：候选校验（保存期闸门）保留绑定错误的哨兵——web 层据此回 400
// `AGENT_RESOURCE_BINDING_INVALID`，而不是折叠成无类别的 500。
// Acceptance point: ValidateCandidate keeps the binding sentinel for the save path.
func TestAgentLoader_ValidateCandidateKeepsBindingSentinel(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "bound", bindingsFrontmatter("bound"), "正文")

	candidate := []byte("---\n" +
		"name: bound\n" +
		"provider: {name: deepseek, model: deepseek-chat}\n" +
		"resource_bindings:\n" +
		"  model_connection:\n" +
		"    - uid: " + bindingModelUID + "\n" + // 缺 name：结构非法
		"  notify_channel: []\n" +
		"---\n\n正文\n")

	loader := newTestLoader(t, nil, nil, "deepseek")
	_, _, _, err := loader.ValidateCandidate(root, nil, "bound", candidate, time.Now())
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
}

// TestAgentLoader_DisplayName：`display_name` 是已知键（R6.2）——声明时去空白后写入
// Profile，缺省或纯空白都读成空串（展示层回退技术名），不因它拒载；未知键仍只 WARN。
// TestAgentLoader_DisplayName parses the known display_name key, reads absent/blank
// values as empty, and keeps the unknown-key WARN for everything else.
func TestAgentLoader_DisplayName(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{name: "declared", line: "display_name: 运维小助手\n", want: "运维小助手"},
		{name: "declared-trimmed", line: "display_name: \"  值班助手  \"\n", want: "值班助手"},
		{name: "blank", line: "display_name: \"   \"\n", want: ""},
		{name: "absent", line: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeAgentFrontmatter(t, root, "display", `
name: display
description: 展示名用例
provider: {name: deepseek, model: deepseek-chat}
`+tc.line, "正文")

			entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.True(t, entries[0].Registered, "%s", entries[0].RejectReason)
			require.Equal(t, tc.want, entries[0].Profile.DisplayName)
			require.NotContains(t, logText(t), "display_name", "已知键不得进未知键 WARN")
		})
	}
}

// TestAgentLoader_DisplayNameKeepsUnknownKeyWarn：display_name 之外仍走未知键 WARN
// （键路径可见、值不进日志），且两者可同时出现——登记新键不压缩前向兼容。
// TestAgentLoader_DisplayNameKeepsUnknownKeyWarn keeps the unknown-key WARN intact
// alongside a parsed display_name.
func TestAgentLoader_DisplayNameKeepsUnknownKeyWarn(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "display", `
name: display
provider: {name: deepseek, model: deepseek-chat}
display_name: 值班助手
future_option: s3cr3t-value
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "%s", entries[0].RejectReason)
	require.Equal(t, "值班助手", entries[0].Profile.DisplayName)

	logs := logText(t)
	require.Contains(t, logs, "unknown frontmatter key")
	require.Contains(t, logs, "future_option")
	require.NotContains(t, logs, "display_name", "已知键不得进未知键清单")
	require.NotContains(t, logs, "s3cr3t-value")
}

// TestValidateScheduleCandidate_AcceptsAndNormalizes：候选四字段去空白、空 zone 取
// UTC，随后走装载期同一套校验并原样返回规整副本。
// TestValidateScheduleCandidate_AcceptsAndNormalizes trims the candidate, defaults the
// zone to UTC, and returns the normalized copy through the load-time rules.
func TestValidateScheduleCandidate_AcceptsAndNormalizes(t *testing.T) {
	got, err := workspace.ValidateScheduleCandidate(core.Schedule{
		ID:      " daily-report ",
		Cron:    " 0 30 8 * * * ",
		Zone:    "  ",
		Message: " 生成每日运维日报 ",
	})
	require.NoError(t, err)
	require.Equal(t, core.Schedule{
		ID:      "daily-report",
		Cron:    "0 30 8 * * *",
		Zone:    "UTC",
		Message: "生成每日运维日报",
	}, got)

	shanghai, err := workspace.ValidateScheduleCandidate(core.Schedule{
		ID: "daily-report", Cron: "0 30 8 * * *", Zone: "Asia/Shanghai", Message: "生成日报",
	})
	require.NoError(t, err)
	require.Equal(t, "Asia/Shanghai", shanghai.Zone)
}

// TestValidateScheduleCandidate_RejectsInvalid：装载期的六条规则原样生效——5 段、
// CRON_TZ 前缀、公共子集外记号、越界值、Local / 未知时区与空 message 都拒绝。
// TestValidateScheduleCandidate_RejectsInvalid applies the six load-time rules verbatim.
func TestValidateScheduleCandidate_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name     string
		schedule core.Schedule
		wantPart string
	}{
		{name: "five-fields", schedule: core.Schedule{ID: "d", Cron: "30 8 * * *", Zone: "UTC", Message: "m"}, wantPart: "6 fields"},
		{name: "timezone-prefix", schedule: core.Schedule{ID: "d", Cron: "CRON_TZ=UTC 0 30 8 * * *", Zone: "UTC", Message: "m"}, wantPart: "CRON_TZ"},
		{name: "outside-subset", schedule: core.Schedule{ID: "d", Cron: "0 30 8 L * *", Zone: "UTC", Message: "m"}, wantPart: "L/W/#"},
		{name: "out-of-range", schedule: core.Schedule{ID: "d", Cron: "0 30 25 * * *", Zone: "UTC", Message: "m"}},
		{name: "zone-local", schedule: core.Schedule{ID: "d", Cron: "0 30 8 * * *", Zone: "Local", Message: "m"}, wantPart: "Local"},
		{name: "zone-unknown", schedule: core.Schedule{ID: "d", Cron: "0 30 8 * * *", Zone: "Mars/Olympus", Message: "m"}},
		{name: "blank-message", schedule: core.Schedule{ID: "d", Cron: "0 30 8 * * *", Zone: "UTC", Message: "  "}, wantPart: "message"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := workspace.ValidateScheduleCandidate(tc.schedule)
			require.Error(t, err)
			if tc.wantPart != "" {
				require.Contains(t, err.Error(), tc.wantPart)
			}
		})
	}
}
