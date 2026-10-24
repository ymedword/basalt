package web

// 本文件判卷「用户可见文案中文化、日志与审计保留英文」的分界（US-12 R8 裁决 1 /
// 模块卡 R8.3.2–R8.3.3）：生命周期拒绝与装载拒因都按**类型化分类**出中文
// （不匹配英文 `Reason` / 英文拒因字面量）、未知分类走兜底中文、执行历史的
// `reason` 只经安全投影出站。
// The Chinese projection boundary: typed categories in, English diagnostics out.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestAgentUnavailableMessage_MapsByKindNotReason —— 五种分类各出对应中文，且文案
// **只**取决于 `Kind`：把英文 `Reason` 换掉（甚至换成另一类别的措辞）不改变结果。
// 逆改点：改回按 `Reason` 字符串匹配 → 换 Reason 的子例红。
func TestAgentUnavailableMessage_MapsByKindNotReason(t *testing.T) {
	cases := []struct {
		kind core.AgentUnavailableKind
		want string
	}{
		{core.AgentUnavailableDisabled, "Agent「reporter」已停用，请先启用后再执行任务。"},
		{core.AgentUnavailableArchived, "Agent「reporter」已归档，请先从归档恢复后再执行任务。"},
		{
			core.AgentUnavailableMissing,
			"Agent「reporter」的生命周期状态文件缺失，请联系管理员恢复状态文件后再执行。",
		},
		{
			core.AgentUnavailableCorrupt,
			"Agent「reporter」的生命周期状态文件损坏，请联系管理员用可信副本恢复。",
		},
		{core.AgentUnavailableKind("other"), "Agent「reporter」当前不可执行，请先确认其生命周期状态。"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			err := &core.AgentUnavailableError{
				Name:   "reporter",
				Kind:   tc.kind,
				Reason: "arbitrary english diagnostic that must not be matched",
			}
			require.Equal(t, tc.want, agentUnavailableMessage(err))
			require.NotContains(t, agentUnavailableMessage(err), "english diagnostic",
				"英文诊断不得出现在界面文案里")
		})
	}

	// 零值 Kind（未分类）走兜底文案，同样不读 Reason。
	err := &core.AgentUnavailableError{Name: "reporter", Reason: "Agent is disabled"}
	require.Equal(t, "Agent「reporter」当前不可执行，请先确认其生命周期状态。",
		agentUnavailableMessage(err))
}

// TestLifecycleConflictMessage —— 409 冲突固定中文并带回 Agent 名；英文哨兵不外泄。
// 逆改点：改成拼接底层 error 文本 → 断言红。
func TestLifecycleConflictMessage(t *testing.T) {
	message := lifecycleConflictMessage("reporter")
	require.Equal(t, "Agent「reporter」的状态已被其他操作改变，请刷新后重试。", message)
	require.NotContains(t, message, "generation")
	require.NotContains(t, message, "expected_state")
}

// TestAgentRejectMessage_TypedKindsBecomeChinese —— 装载拒因按**分类常量**映射为
// 中文：带 detail 的模板填入 detail，不带 detail 的模板不留下悬空冒号；可能含
// 英文诊断或绝对路径的分类（读取失败 / frontmatter / 调度）吞掉 detail。
// 逆改点：改回按英文前缀字符串匹配、或把 detail 原样拼进界面 → 对应子例红。
func TestAgentRejectMessage_TypedKindsBecomeChinese(t *testing.T) {
	cases := []struct {
		kind   core.AgentRejectKind
		detail string
		want   string
	}{
		{core.AgentRejectNameMismatch, "other-agent", "目录名与 AGENT.md 的 name 不一致：other-agent"},
		{core.AgentRejectNameMismatch, "", "目录名与 AGENT.md 的 name 不一致"},
		{core.AgentRejectProviderNotRegistered, "main", "引用的模型连接未发布或不存在：main"},
		{core.AgentRejectBlankModel, "", "未在 AGENT.md 中声明默认模型"},
		{core.AgentRejectToolNotRegistered, "shell", "工具未注册：shell"},
		{core.AgentRejectSkillNotFound, "reporter-help", "技能未找到：reporter-help"},
		{core.AgentRejectMCPServerSkipped, "fs", "MCP 服务不可用（未配置或未连接）：fs"},
		{core.AgentRejectBootstrapMissing, "MEMORY.md", "上下文文件缺失：MEMORY.md"},
		{core.AgentRejectInvalidNotifyChannels, "未知渠道名", "通知渠道声明非法：未知渠道名"},
		{core.AgentRejectInvalidNotifyMode, "strict", "通知模式声明非法：strict"},
		{core.AgentRejectInvalidNotifyMode, "", "通知模式声明非法"},
		{core.AgentRejectInvalidTemperature, "4.2", "输出随机性（temperature）取值非法：4.2"},
		{core.AgentRejectSkillVersionMissing, "reporter-help", "绑定的技能没有已发布版本：reporter-help"},
		{
			core.AgentRejectReadFailed,
			"open /srv/basalt/agents/reporter/AGENT.md: no such file or directory",
			"读取 AGENT.md 失败（详情见服务端日志）",
		},
		{
			core.AgentRejectInvalidFrontmatter,
			"yaml: line 3: did not find expected key",
			"AGENT.md 的 frontmatter 无法解析（详情见服务端日志）",
		},
		{
			core.AgentRejectInvalidResourceBindings,
			"块结构非法",
			"资源绑定块（resource_bindings）结构非法",
		},
		{
			core.AgentRejectInvalidSchedule,
			"秒数为 61",
			"定时任务声明非法：请检查 cron 表达式、时区、消息与投递目标（详情见服务端日志）",
		},
		{
			core.AgentRejectKind("some future loader reason"),
			"detail",
			"装载校验未通过（详情见服务端日志）",
		},
	}
	for _, tc := range cases {
		got := agentRejectMessage(tc.kind, tc.detail)
		require.Equal(t, tc.want, got, "kind=%q detail=%q", tc.kind, tc.detail)
		require.NotContains(t, got, "%!"+"s(MISSING)",
			"模板不得漏 %s 处理")
	}
	readFailure := agentRejectMessage(core.AgentRejectReadFailed,
		"open /srv/basalt/agents/reporter/AGENT.md: no such file or directory")
	require.NotContains(t, readFailure, "/srv/", "绝对路径只进日志")
	require.NotContains(t, readFailure, "open ", "英文诊断只进日志")
}

// TestAgentRejectMessage_CoversEveryDefinedKind —— 每个已定义的分类都有专用中文
// 文案（不是兜底）：新增分类却忘了映射时本用例红，避免悄悄退化成通用短语。
// 逆改点：删掉任一分类的 switch 分支 → 该子例得到兜底文案，断言红。
func TestAgentRejectMessage_CoversEveryDefinedKind(t *testing.T) {
	kinds := []core.AgentRejectKind{
		core.AgentRejectReadFailed,
		core.AgentRejectInvalidFrontmatter,
		core.AgentRejectInvalidResourceBindings,
		core.AgentRejectNameMismatch,
		core.AgentRejectProviderNotRegistered,
		core.AgentRejectBlankModel,
		core.AgentRejectInvalidTemperature,
		core.AgentRejectToolNotRegistered,
		core.AgentRejectSkillNotFound,
		core.AgentRejectMCPServerSkipped,
		core.AgentRejectBootstrapMissing,
		core.AgentRejectInvalidSchedule,
		core.AgentRejectInvalidNotifyChannels,
		core.AgentRejectInvalidNotifyMode,
		core.AgentRejectSkillVersionMissing,
	}
	const fallback = "装载校验未通过（详情见服务端日志）"
	seen := make(map[string]string, len(kinds))
	for _, kind := range kinds {
		require.NotEmpty(t, string(kind), "分类常量不得为空")
		message := agentRejectMessage(kind, "")
		require.NotEqual(t, fallback, message, "分类 %q 缺少专用文案", kind)
		if prior, exists := seen[message]; exists {
			t.Fatalf("分类 %q 与 %q 共用文案 %q，用户无法区分原因", kind, prior, message)
		}
		seen[message] = string(kind)
	}
}

// TestEntryRejectMessage_IgnoresEnglishReason —— 视图只读 `RejectKind` /
// `RejectDetail`：同一条目上挂着英文 `RejectReason` 也不外泄；零值分类走兜底，
// 不回显 `RejectReason`。
// 逆改点：改回投影 `RejectReason` → 英文子例红。
func TestEntryRejectMessage_IgnoresEnglishReason(t *testing.T) {
	entry := core.AgentEntry{
		Name:         "reporter",
		RejectKind:   core.AgentRejectProviderNotRegistered,
		RejectDetail: "main",
		RejectReason: "provider not registered: main",
	}
	message := entryRejectMessage(entry)
	require.Equal(t, "引用的模型连接未发布或不存在：main", message)
	require.NotContains(t, message, "provider")

	unclassified := core.AgentEntry{Name: "reporter", RejectReason: "缺少 system prompt"}
	require.Equal(t, "装载校验未通过（详情见服务端日志）",
		entryRejectMessage(unclassified))
}

// TestProblemMessages_ProjectsEachEntryAndNeverNil —— 逐条目投影 `problems`：已注册或
// 尚待确认（无分类）的条目给空切片（**不是** nil），已分类的未注册条目恰给一条中文；
// 聚合结果等于逐条投影，英文原因不随条目进入视图。
// Every entry projects a non-nil problems slice; a classified unregistered entry
// contributes exactly one Chinese item.
func TestProblemMessages_ProjectsEachEntryAndNeverNil(t *testing.T) {
	entries := []core.AgentEntry{
		{Name: "healthy", Registered: true},
		{
			Name: "ghost", Registered: false,
			RejectKind:   core.AgentRejectNameMismatch,
			RejectDetail: "other",
			RejectReason: "name mismatch: other",
		},
		// 轻路径的「待确认」条目没有分类：空切片，不是一条空串问题。
		{Name: "pending", Registered: false},
	}
	var aggregated []string
	for _, entry := range entries {
		view := profileView(entry)
		require.NotNil(t, view.Problems, "%s: problems 必须是空切片而非 nil", entry.Name)
		if entry.Registered || entry.RejectKind == "" {
			require.Empty(t, view.Problems)
		} else {
			require.Len(t, view.Problems, 1)
			require.Equal(t, "目录名与 AGENT.md 的 name 不一致：other", view.Problems[0])
			require.NotContains(t, view.Problems[0], "name mismatch")
		}
		aggregated = append(aggregated, view.Problems...)
	}
	require.Equal(t, []string{"目录名与 AGENT.md 的 name 不一致：other"}, aggregated,
		"每个已分类的未注册条目恰投影一条")
}

// TestPublicRunReason_ChineseOnlyAndSafe —— 执行历史的 `reason` 只出四类固定中文：
// 取消 / 失败 / 达到上限；迭代与工具预算两个上限原因本身已是中文，原样透出；其余
// 状态为空串。任何底层英文原因（含路径与凭证片段）都不得出现在返回值里。
// 逆改点：把 `reason` 原样回显 → 下例会带出英文与路径，本用例红。
func TestPublicRunReason_ChineseOnlyAndSafe(t *testing.T) {
	cases := []struct {
		status core.RunStatus
		reason string
		want   string
	}{
		{core.StatusCancelled, "context canceled", "运行已取消"},
		{core.StatusFailed, "open /srv/basalt/agents/reporter/AGENT.md: no such file", "运行失败"},
		{core.StatusFailed, "api_key=sk-should-never-leak", "运行失败"},
		{core.StatusLimitReached, core.ReasonIterationLimit, core.ReasonIterationLimit},
		{core.StatusLimitReached, core.ReasonToolBudget, core.ReasonToolBudget},
		{core.StatusLimitReached, "some other limit", "运行达到上限"},
		{core.StatusCompleted, "", ""},
	}
	for _, tc := range cases {
		got := publicRunReason(tc.status, tc.reason)
		require.Equal(t, tc.want, got)
		require.NotContains(t, got, "/srv/")
		require.NotContains(t, got, "sk-should-never-leak")
	}
}
