package workspace_test

// 本文件是 US-3 schedules 加载期校验的关键回归用例（颗粒度文档 §3.2 的
// schedule_test.go 十六例，§4.3 写出代码逐条落地）。夹具家族见 agent_test.go。
// Regression tests for the schedule validation rules of the US-3 loader.

import (
	"strings"
	"testing"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// 中文验收点：5 段 cron 拒载，文案**必须**含固定句 "Cron expression must consist of 6
// fields"（Compatibility §5.1 判据 4）；robfig 原文（expected exactly 6 fields, found 5: …）
// 另附 detail，不替代固定句（见 §2.4 注 6）。
// Acceptance point: a five-field cron is rejected with the promised wording.
func TestAgentLoader_RejectsFiveFieldCron(t *testing.T) {
	root := writeWorkspaceWithSchedule(t, "0 8 * * *", "") // 5 段、无 zone
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	entry := entries[0]
	require.False(t, entry.Registered, "5 段 cron 必须整只不注册")
	require.Contains(t, entry.RejectReason, "Cron expression must consist of 6 fields")
}

// 中文验收点：语法非法（robfig 拒）的 detail 带 robfig 错误原文——本仓只做分类
// （reason 恒为 invalid schedule），触发时间计算的同一份解析原文照实附上。
// Acceptance point: a syntax error carries the parser's own message.
func TestAgentLoader_CronSyntaxErrorCarriesParserText(t *testing.T) {
	root := writeWorkspaceWithSchedule(t, "0 0 8 * * nope", "UTC") // 周字段不是合法名
	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	entry := entries[0]
	require.False(t, entry.Registered, "非法语法必须整只不注册")
	require.Contains(t, entry.RejectReason, "invalid schedule")
	require.Contains(t, entry.RejectReason, "failed to parse int from nope", "robfig 原文进 detail")
}

// 中文验收点：删 zone → 按 UTC（Compatibility §5.1 判据 5）——**不得**回落本机时区；
// 判据落在规范化结果上：spec 必须显式带 CRON_TZ=UTC（本机时区若被告状进 spec 即判红）。
// Acceptance point: a missing zone defaults to UTC, never the host time zone.
func TestAgentLoader_DefaultZoneIsUTC(t *testing.T) {
	root := writeWorkspaceWithSchedule(t, "0 0 8 * * *", "") // 6 段、无 zone
	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)
	require.True(t, entries[0].Registered, "缺 zone 不得导致拒载：%s", entries[0].RejectReason)
	require.Equal(t, "UTC", entries[0].Profile.Schedules[0].Zone)

	spec, err := workspace.CronSpec(entries[0].Profile.Schedules[0])
	require.NoError(t, err)
	require.Equal(t, "CRON_TZ=UTC 0 0 8 * * *", spec)
}

// 中文验收点：zone 名非法（非 IANA / 不可加载）拒载，detail 附 time.LoadLocation
// 的失败原文（zone 名不是秘密，可入文案——§6 固定口径 5）。
// Acceptance point: an unloadable zone name is rejected with the loader's message.
func TestAgentLoader_InvalidZoneRejected(t *testing.T) {
	root := writeWorkspaceWithSchedule(t, "0 0 8 * * *", "Mars/Olympus")
	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	entry := entries[0]
	require.False(t, entry.Registered, "非法 zone 必须整只不注册")
	require.Contains(t, entry.RejectReason, "invalid schedule")
	require.Contains(t, entry.RejectReason, "unknown time zone Mars/Olympus",
		"detail 取 time.LoadLocation 失败原文")
}

// 中文验收点：zone: Local 显式黑名单——time.LoadLocation("Local") 会成功，
// 只靠它校验拦不住"回落进程本地时区"（§2.2 约束 5）。
// Acceptance point: zone Local is blacklisted explicitly.
func TestAgentLoader_RejectsLocalZone(t *testing.T) {
	root := writeWorkspaceWithSchedule(t, "0 0 8 * * *", "Local")
	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	entry := entries[0]
	require.False(t, entry.Registered, "zone: Local 必须拒载")
	require.Contains(t, entry.RejectReason, "zone Local 被禁用（需地方时间请显式指定 IANA 时区）")
}

// 中文验收点：cron 公共子集硬规则——**日与周不得同时被限定**（至少一个是裸 *；
// */2、1-15、1,5、单值都算限定）——契约 §3.1。robfig v3.0.1 对 dom / dow 同时被限定的
// 表达式是**接受**的（它不做公共子集约束——本轮源码实测），故本仓预检必须自己拦。
// Acceptance point: dom and dow may not both be restricted.
func TestAgentLoader_RejectsBothDomAndDowRestricted(t *testing.T) {
	cases := []struct {
		name           string
		cron           string
		wantRegistered bool
	}{
		{"both_restricted", "0 0 8 1-15 * mon", false},
		{"dom_step_and_dow_list", "0 0 8 */2 * 1,5", false}, // */2 计为限定
		{"only_dom_restricted", "0 0 8 1-15 * *", true},
		{"only_dow_restricted", "0 0 8 * * mon", true},
		{"dom_step_only", "0 0 8 */2 * *", true},
		{"both_star", "0 0 8 * * *", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeWorkspaceWithSchedule(t, tc.cron, "UTC")
			entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
				Providers: []string{"deepseek"},
				Logger:    bufferedLogger(t),
			}).LoadAll(root)
			require.NoError(t, err)
			require.Len(t, entries, 1)

			entry := entries[0]
			require.Equal(t, tc.wantRegistered, entry.Registered, "cron %q：%s", tc.cron, entry.RejectReason)
			if !tc.wantRegistered {
				require.Contains(t, entry.RejectReason, "日与周不得同时受限")
			}
		})
	}
}

// 中文验收点：周字段 7 拒（用 0 表示周日——公共子集钉死的替代写法），
// detail 是固定句；同表带一条周字段 0 的正向对照（证明拒的是 7 这个取值，不是整个字段）。
// Acceptance point: day-of-week 7 is rejected, 0 is the accepted spelling of Sunday.
func TestAgentLoader_RejectsSevenInDayOfWeek(t *testing.T) {
	cases := []struct {
		name           string
		cron           string
		wantRegistered bool
	}{
		{"sunday_as_zero", "0 0 8 * * 0", true},
		{"sunday_as_seven", "0 0 8 * * 7", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeWorkspaceWithSchedule(t, tc.cron, "UTC")
			entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
			require.NoError(t, err)

			entry := entries[0]
			require.Equal(t, tc.wantRegistered, entry.Registered, "cron %q：%s", tc.cron, entry.RejectReason)
			if !tc.wantRegistered {
				require.Contains(t, entry.RejectReason, "周字段 7 超出范围（0–6）")
			}
		})
	}
}

// 中文验收点：L / W / # 三种写法拒（Spring 支持、robfig 不支持——公共子集外），
// detail 固定句附字段原文；按月 / 周缩写含同名字母（JUL 含 L、WED 含 W）的**正向对照**
// ——拒的是语法记号，不是字母本身。
// Acceptance point: L, W and # are outside the public subset, while names that merely
// contain those letters stay valid.
func TestAgentLoader_RejectsUnsupportedTokens(t *testing.T) {
	cases := []struct {
		name           string
		cron           string
		wantRegistered bool
		wantDetail     string
	}{
		{"last_day_of_month", "0 0 8 L * *", false, "字段不支持 L/W/# 语法：L"},
		{"nearest_weekday", "0 0 8 15W * *", false, "字段不支持 L/W/# 语法：15W"},
		{"nth_weekday_of_month", "0 0 8 * * 1#2", false, "字段不支持 L/W/# 语法：1#2"},
		{"month_name_with_l", "0 0 8 * JUL *", true, ""},
		{"weekday_name_with_w", "0 0 8 * * WED", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeWorkspaceWithSchedule(t, tc.cron, "UTC")
			entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
			require.NoError(t, err)

			entry := entries[0]
			require.Equal(t, tc.wantRegistered, entry.Registered, "cron %q：%s", tc.cron, entry.RejectReason)
			if tc.wantDetail != "" {
				require.Contains(t, entry.RejectReason, tc.wantDetail)
			}
		})
	}
}

// 中文验收点：message 必填（空白拒）——detail 固定句（§6 固定口径 5）。
// Acceptance point: a blank schedule message rejects the agent.
func TestAgentLoader_RejectsBlankScheduleMessage(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "mutely", `
name: mutely
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - cron: "0 0 8 * * *"
    zone: UTC
    message: "   "
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	entry := entries[0]
	require.False(t, entry.Registered, "空白 message 必须整只不注册")
	require.Contains(t, entry.RejectReason, "message 为必填")
}

// 中文验收点：恰一个任务时 id 可缺省——派生 <agent>-1 并**写回**装载结果
// （调度注册（US-10）据此拿到的 id 就是运行时 id，不再二次派生）。
// Acceptance point: a single task without an id derives <agent>-1.
func TestAgentLoader_ScheduleIDSingleTaskDerivesAgentSuffix(t *testing.T) {
	root := writeWorkspaceWithSchedule(t, "0 0 8 * * *", "UTC")
	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	entry := entries[0]
	require.True(t, entry.Registered, "单任务缺 id 不得拒载：%s", entry.RejectReason)
	require.Len(t, entry.Profile.Schedules, 1)
	require.Equal(t, "sched-agent-1", entry.Profile.Schedules[0].ID)
}

// 中文验收点：两个及以上任务，每项 id 必须显式——缺省派生只在"恰一个任务"时发生。
// Acceptance point: with two or more tasks every id must be explicit.
func TestAgentLoader_ScheduleIDRequiredFromTwoTasks(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "twofold", `
name: twofold
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - cron: "0 0 8 * * *"
    zone: UTC
    message: 早报
  - cron: "0 0 9 * * *"
    zone: UTC
    message: 午报
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	entry := entries[0]
	require.False(t, entry.Registered, "≥2 项未显式 id 必须整只不注册")
	require.Contains(t, entry.RejectReason, "id 仅可在恰一个任务时缺省（派生 twofold-1）")
}

// 中文验收点：同一 Agent 内两条显式 id 相同 → 拒，detail 点名重复的 id
// （同 Agent 内不存在"显式 vs 派生"撞车——派生只在恰一个任务时发生）。
// Acceptance point: duplicate explicit ids inside one agent are rejected.
func TestAgentLoader_ScheduleIDDuplicateWithinAgentRejects(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "dupe", `
name: dupe
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - id: dup
    cron: "0 0 8 * * *"
    zone: UTC
    message: 早报
  - id: dup
    cron: "0 0 9 * * *"
    zone: UTC
    message: 午报
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	entry := entries[0]
	require.False(t, entry.Registered, "本只内 id 重复必须整只不注册")
	require.Contains(t, entry.RejectReason, "id 重复：dup")
}

// 中文验收点：调度 id 全局唯一、先到先得——先注册的 Agent 占住 id 后，
// 后来的同 id Agent 整只不注册，且 detail 点名占用者（§6 固定口径 5）。
// Acceptance point: ids are globally unique, first come first served.
func TestAgentLoader_ScheduleIDCollisionRejectsLaterAgent(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "early", `
name: early
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - id: shared
    cron: "0 0 8 * * *"
    zone: UTC
    message: 早报
`, "正文")
	writeAgentFrontmatter(t, root, "late", `
name: late
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - id: shared
    cron: "0 0 9 * * *"
    zone: UTC
    message: 晚报
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	byName := make(map[string]core.AgentEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	require.True(t, byName["early"].Registered, "先到者占用成功：%s", byName["early"].RejectReason)
	require.False(t, byName["late"].Registered, "后到者同 id 必须整只不注册")
	require.Contains(t, byName["late"].RejectReason, "id 已被 early 占用：shared")
}

// 中文验收点：占用按**整只 Agent** 提交——alpha 的第一条任务先声明 id "x"、第二条非法
// （5 段）；alpha 整只不注册，且 "x" **未被占用**：随后字典序靠后的 beta 显式声明 id "x"
// 必须注册成功（占用的唯一提交点是 LoadAll 的注册成功分支——§2.3 流程 A / §2.4 注 6）。
// Acceptance point: a rejected agent must not hold any schedule id.
func TestAgentLoader_RejectedAgentClaimsNoScheduleIDs(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "alpha", `
name: alpha
provider: {name: deepseek, model: deepseek-chat}
tools: []
schedules:
  - id: x
    cron: "0 0 8 * * *"
    zone: UTC
    message: 早报
  - id: y
    cron: "0 8 * * *"        # 5 段——第二条非法
    zone: UTC
    message: 晚报
`, "")
	writeAgentFrontmatter(t, root, "beta", `
name: beta
provider: {name: deepseek, model: deepseek-chat}
tools: []
schedules:
  - id: x
    cron: "0 0 9 * * *"
    zone: UTC
    message: 午报
`, "")

	entries, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"deepseek"},
		Logger:    bufferedLogger(t),
	}).LoadAll(root)
	require.NoError(t, err)

	byName := make(map[string]core.AgentEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	require.False(t, byName["alpha"].Registered, "第二条非法 → 整只不注册")
	require.True(t, byName["beta"].Registered,
		"被拒的 Agent 不得占用 id：beta 应能注册（%s）", byName["beta"].RejectReason)
}

// 中文验收点：派生 id 与显式 id 的撞车只在**跨 Agent** 出现——A 单任务派生 a-1 占住，
// B 显式 a-1 撞车被拒（§2.3 流程 A 末注）。
// Acceptance point: an explicit id colliding with another agent's derived id loses.
func TestAgentLoader_ExplicitIDCollidesWithDerivedID(t *testing.T) {
	root := t.TempDir()
	writeAgentFrontmatter(t, root, "a", `
name: a
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - cron: "0 0 8 * * *"
    zone: UTC
    message: 早报
`, "正文")
	writeAgentFrontmatter(t, root, "b", `
name: b
provider: {name: deepseek, model: deepseek-chat}
schedules:
  - id: a-1
    cron: "0 0 9 * * *"
    zone: UTC
    message: 晚报
`, "正文")

	entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
	require.NoError(t, err)

	byName := make(map[string]core.AgentEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	require.True(t, byName["a"].Registered, "单任务派生 a-1：%s", byName["a"].RejectReason)
	require.Equal(t, "a-1", byName["a"].Profile.Schedules[0].ID)
	require.False(t, byName["b"].Registered, "显式声明已被派生占用的 id 必须拒")
	require.Contains(t, byName["b"].RejectReason, "id 已被 a 占用：a-1")
}

// 中文验收点：每任务时区经 CRON_TZ= 前缀（契约 §3.1）——规范化对**每个** zone 都成立，
// 时区串逐字保留（不改写、不省略）；CronSpec 是纯规范化（校验归装载期，见 §2.3 流程 A）。
// Acceptance point: CronSpec prefixes every zone verbatim.
func TestCronSpec_PrefixesEveryZone(t *testing.T) {
	cases := []struct {
		zone string
	}{
		{"UTC"},
		{"Asia/Shanghai"},
		{"America/New_York"},
		{"Europe/Berlin"},
		{""}, // 缺省兜底：空 zone 同样按 UTC 前缀（装载已填，这里是函数自身的防御）
	}
	for _, tc := range cases {
		t.Run("zone_"+tc.zone, func(t *testing.T) {
			want := "CRON_TZ=" + tc.zone + " 0 0 8 * * *"
			if tc.zone == "" {
				want = "CRON_TZ=UTC 0 0 8 * * *"
			}
			spec, err := workspace.CronSpec(core.Schedule{Cron: "0 0 8 * * *", Zone: tc.zone})
			require.NoError(t, err)
			require.Equal(t, want, spec)
		})
	}
}

// 中文验收点（R2 评审修复）：cron **自带时区前缀**必须拒载——时区只有一个来源（任务的
// zone 字段）：装载期放行、出口（CronSpec）再叠一层前缀，会产出
// `CRON_TZ=UTC CRON_TZ=Asia/Shanghai 0 0 8 * * *`（robfig 随后解析失败）——"过了校验"
// 与"注册时炸"各说各话。判据：拒载、reason 取 invalid schedule、detail 是统一拒绝句。
// Acceptance point: an expression carrying its own timezone prefix is refused,
// because the zone field is the single source of the timezone.
func TestAgentLoader_RejectsCronWithTimezonePrefix(t *testing.T) {
	for _, expr := range []string{"CRON_TZ=Asia/Shanghai 0 0 8 * * *", "TZ=UTC 0 0 8 * * *"} {
		t.Run(strings.Fields(expr)[0], func(t *testing.T) {
			root := writeWorkspaceWithSchedule(t, expr, "Asia/Shanghai")
			entries, err := newTestLoader(t, nil, nil, "deepseek").LoadAll(root)
			require.NoError(t, err)
			require.Len(t, entries, 1)

			entry := entries[0]
			require.False(t, entry.Registered, "cron 自带时区前缀必须整只不注册")
			require.Contains(t, entry.RejectReason, "invalid schedule")
			require.Contains(t, entry.RejectReason,
				"cron 内不得自带时区前缀（TZ= / CRON_TZ=）——时区一律由 zone 字段声明")
		})
	}
}

// 中文验收点（R2 评审修复）：出口产出的表达式**必须**是 robfig 能解析的最终形态，且
// 前缀只有一层——校验与规范化必须落在同一份表达式上（此前校验用剥前缀的原始串、出口
// 再追加一次前缀，两条路径各说各话）。
// Acceptance point: the normalized expression parses and carries exactly one prefix.
func TestCronSpec_NormalizedResultCarriesOnePrefix(t *testing.T) {
	parser := cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	for _, zone := range []string{"UTC", "Asia/Shanghai", "America/New_York"} {
		t.Run("zone_"+zone, func(t *testing.T) {
			spec, err := workspace.CronSpec(core.Schedule{Cron: "0 0 8 * * *", Zone: zone})
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(spec, "CRON_TZ="), "前缀只许一层：%s", spec)
			require.Equal(t, 1, strings.Count(spec, "TZ="), "时区前缀只许出现一次：%s", spec)

			_, err = parser.Parse(spec)
			require.NoError(t, err, "规范化结果必须可解析：%s", spec)
		})
	}

	// 出口自身的防线：调用方绕过装载期直接给带前缀的 cron，CronSpec 不得叠出双前缀。
	spec, err := workspace.CronSpec(core.Schedule{Cron: "CRON_TZ=Asia/Shanghai 0 0 8 * * *", Zone: "UTC"})
	require.Error(t, err, "带前缀的输入必须报错，实际产出：%s", spec)
	require.Empty(t, spec)
	require.Contains(t, err.Error(),
		"cron 内不得自带时区前缀（TZ= / CRON_TZ=）——时区一律由 zone 字段声明")
}
