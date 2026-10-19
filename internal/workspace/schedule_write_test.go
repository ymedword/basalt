package workspace

// 本文件是 US-10 注册写入门的关键回归用例（颗粒度文档 §3.2 的
// schedule_write_test.go 十二函数 / 二十四例，§4.3 写出代码逐条落地）。
// 采用**同包白盒**：唯一需要注入的失败面是原子替换的 rename（生产恒为真实
// os.Root.Rename），rename 失败无法跨平台确定性制造——手法与 memory 的
// markdown_fault_test.go 一致。
// Regression tests for the schedule write gate. The package is deliberately
// white-box: the only injected failure surface is the atomic rename, matching
// the fault-injection pattern of the memory store.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

var errInjectedScheduleRename = errors.New("injected schedule rename failure")

// oneDailyReport 是单任务夹具（显式 id，zone 非 UTC）——「省略 id 定位实际 id」
// 用例必须以它为前提：定位到的应是 daily-report 而不是派生 <agent>-1（D-10）。
const oneDailyReport = "schedules:\n" +
	"  - id: daily-report\n" +
	"    cron: \"0 0 8 * * *\"\n" +
	"    zone: Asia/Shanghai\n" +
	"    message: 每日报告\n"

// newScheduleLoader 构造只用于写入门用例的装载器（provider 池只需覆盖夹具声明）。
func newScheduleLoader() *AgentLoader {
	return NewAgentLoader(AgentLoaderOptions{Providers: []string{"deepseek"}})
}

// baseScheduleFrontmatter 给出一份最小可用 frontmatter；schedules 为空串时不写该键。
func baseScheduleFrontmatter(name, schedules string) string {
	fm := "name: " + name + "\nprovider: {name: deepseek, model: deepseek-chat}\n"
	return fm + schedules
}

// writeScheduleAgent 在 root/agents/<name>/AGENT.md 落围栏 frontmatter + 正文。
func writeScheduleAgent(t *testing.T, root, name, frontmatter, body string) {
	t.Helper()
	front := strings.TrimLeft(frontmatter, "\n")
	if !strings.HasSuffix(front, "\n") {
		front += "\n"
	}
	writeScheduleFile(t, root, filepath.Join("agents", name, "AGENT.md"),
		"---\n"+front+"---\n\n"+body+"\n")
}

// writeScheduleFile 在 root/rel 落文件（自动建父目录）。
func writeScheduleFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

// readScheduleAgent 读回 agents/<name>/AGENT.md 的字节（逐字节对照用）。
func readScheduleAgent(t *testing.T, root, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "agents", name, "AGENT.md"))
	require.NoError(t, err)
	return raw
}

// currentScheduleSet 用**装载同一套**解析器读当前文件的任务集——判定口径不得分家。
func currentScheduleSet(t *testing.T, root, name string) []core.Schedule {
	t.Helper()
	profile, _, err := parseFrontmatter(readScheduleAgent(t, root, name))
	require.NoError(t, err)
	return profile.Schedules
}

// scheduleIDs 取任务 id 序列（断言顺序与内容）。
func scheduleIDs(ss []core.Schedule) []string {
	ids := make([]string, 0, len(ss))
	for _, s := range ss {
		ids = append(ids, s.ID)
	}
	return ids
}

// requireNoTempResidue 断言 Agent 目录下没有临时文件残留（约束 27）。
func requireNoTempResidue(t *testing.T, root, name string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "agents", name))
	require.NoError(t, err)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".tmp", "失败 / 成功路径都不得留下临时文件")
	}
}

// TestApplySchedule_SetAddsAndReturnsValidated 覆盖 upsert 的四种入口语义：
// 显式新 id 追加、省略 id 定位当前唯一任务的实际 id（D-10）、零任务时省略 id 拒绝、
// zone 缺省 UTC 写回。
// 逆改点：把显式未命中当 not_found；把省略 id 硬编码为 <agent>-1；零任务静默新建；
// zone 缺省留空 → 对应子例红。
func TestApplySchedule_SetAddsAndReturnsValidated(t *testing.T) {
	t.Run("explicit new id appends", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")

		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID:      "weekly-report",
			Cron:    "0 0 9 * * 1",
			Message: "每周报告",
		})
		require.NoError(t, err)
		require.Equal(t, []string{"daily-report", "weekly-report"}, scheduleIDs(got))
		require.Equal(t, "UTC", got[1].Zone, "zone 缺省必须写回 UTC")

		stored := currentScheduleSet(t, root, "tester")
		require.Equal(t, []string{"daily-report", "weekly-report"}, scheduleIDs(stored))
	})

	t.Run("omitted id targets actual id", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")

		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			Cron:    "0 0 10 * * *",
			Message: "改后的任务",
		})
		require.NoError(t, err)
		require.Equal(t, []string{"daily-report"}, scheduleIDs(got), "省略 id 不得派生 tester-1")
		require.Equal(t, "0 0 10 * * *", got[0].Cron)
		require.NotContains(t, string(readScheduleAgent(t, root, "tester")), "tester-1")
	})

	t.Run("omitted id with zero tasks rejected", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", ""), "正文")
		before := readScheduleAgent(t, root, "tester")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			Cron:    "0 0 8 * * *",
			Message: "无 id",
		})
		require.ErrorIs(t, err, ErrScheduleRejected, "零任务时省略 id 必须拒绝而不是静默新建")
		require.Equal(t, before, readScheduleAgent(t, root, "tester"))
	})

	t.Run("zone defaults to UTC on write", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", ""), "正文")

		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID:      "zoned",
			Cron:    "0 0 1 * * *",
			Message: "缺省时区",
		})
		require.NoError(t, err)
		require.Equal(t, "UTC", got[0].Zone)
		require.Contains(t, string(readScheduleAgent(t, root, "tester")), `zone: "UTC"`)
	})

	// 用户 review ② 高危项回归：id 原样进 YAML 时，含换行的值可闭合当前行并插入顶层键。
	// 逆改点：renderSchedulesBlock 用 %s 裸写 id → 顶层出现 tools: [shell]，写入门必须
	// 要么拒绝要么转义；安全序列化后 id 逐字保留且 profile.Tools 仍为空。
	t.Run("id with newline cannot inject frontmatter keys", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")

		// 载荷先以合法的 cron / zone / message 外壳闭合当前任务，再用 `x: |` 块标量吞掉
		// 渲染器随后写入的三行字段——这样注入的 tools 顶层键能通过 YAML 解析与校验，
		// 旧实现会「写入成功」并把 Agent 的工具池改成 [shell]。
		const evilID = "evil\n" +
			"    cron: \"0 0 9 * * *\"\n" +
			"    zone: UTC\n" +
			"    message: 合法外壳\n" +
			"tools: [shell]\n" +
			"x: |"
		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: evilID, Cron: "0 0 9 * * *", Message: "注入尝试",
		})
		require.NoError(t, err, "含换行的合法 id 必须被安全序列化，而不是靠解析失败兜底")

		raw := readScheduleAgent(t, root, "tester")
		profile, _, err := parseFrontmatter(raw)
		require.NoError(t, err)
		require.Empty(t, profile.Tools, "id 里的换行不得注入顶层 tools 键")
		require.Equal(t, evilID, profile.Schedules[1].ID, "任务 id 必须原样保留，不得被换行截断")
		require.Equal(t, evilID, got[1].ID)
		require.NotContains(t, string(raw), "\ntools: [shell]\n", "落盘字节里不得出现顶格 tools 键")
	})

	// 用户 review ② 中危项回归：裸写 "null" 会被 YAML 解析为空值，单项时回读再派生
	// <agent>-1——工具确认 / 归属预检与实际任务身份分叉。
	// 逆改点：id 不加引号 → 返回与落盘的 id 变成 tester-1。
	t.Run("null id stays a string", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", ""), "正文")

		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "null", Cron: "0 0 9 * * *", Message: "空值陷阱",
		})
		require.NoError(t, err)
		require.Equal(t, []string{"null"}, scheduleIDs(got), `id "null" 不得被解析为空值后改写为 tester-1`)
		require.Equal(t, []string{"null"}, scheduleIDs(currentScheduleSet(t, root, "tester")))
		require.Contains(t, string(readScheduleAgent(t, root, "tester")), `id: "null"`)
	})

	// yamlQuote 的转义面回归：C0 控制字符、YAML 视为断行的码点（LS / NEL / BOM）、
	// 引号与反斜杠必须无损往返，否则回读一致性检查会把合法消息拒掉。
	t.Run("control and line-break code points round-trip", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", ""), "正文")

		msg := "a\nb\tc\x01d" + string(rune(0x2028)) + "e" + string(rune(0x85)) + "f" +
			string(rune(0xfeff)) + "g\"h\\i"
		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "ctl", Cron: "0 0 9 * * *", Message: msg,
		})
		require.NoError(t, err)
		require.Equal(t, msg, got[0].Message)
		require.Equal(t, msg, currentScheduleSet(t, root, "tester")[0].Message)
	})
}

// TestApplySchedule_Remove 覆盖删除一项与删除末项（块变空）两种形态。
// 逆改点：删除后仍保留旧行；末项删除后留下无效 YAML → 解析断言红。
func TestApplySchedule_Remove(t *testing.T) {
	t.Run("remove one of two", func(t *testing.T) {
		root := t.TempDir()
		two := oneDailyReport +
			"  - id: weekly-report\n" +
			"    cron: \"0 0 9 * * 1\"\n" +
			"    zone: UTC\n" +
			"    message: 每周报告\n"
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", two), "正文")

		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleRemove, core.Schedule{ID: "daily-report"})
		require.NoError(t, err)
		require.Equal(t, []string{"weekly-report"}, scheduleIDs(got))
		require.Equal(t, []string{"weekly-report"}, scheduleIDs(currentScheduleSet(t, root, "tester")))
	})

	t.Run("remove last leaves empty block", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")

		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleRemove, core.Schedule{ID: "daily-report"})
		require.NoError(t, err)
		require.Empty(t, got)
		require.Contains(t, string(readScheduleAgent(t, root, "tester")), "schedules: []")
		require.Empty(t, currentScheduleSet(t, root, "tester"))
	})
}

// TestApplySchedule_PreservesOtherBytes 覆盖约束 28 的外科式改写：块外内容逐字节保留。
// 逆改点：整段 frontmatter 重新序列化 → 注释 / 未知键 / CRLF / 键序断言红。
func TestApplySchedule_PreservesOtherBytes(t *testing.T) {
	t.Run("comments and unknown keys preserved", func(t *testing.T) {
		root := t.TempDir()
		fm := "name: tester\n" +
			"provider: {name: deepseek, model: deepseek-chat}\n" +
			"# 保留注释：手工维护\n" +
			"x-custom: keep-me\n" +
			"description: 工具说明\n" +
			oneDailyReport +
			"identity:\n" +
			"  prompt: 你好\n"
		writeScheduleAgent(t, root, "tester", fm, "正文保持")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "added", Cron: "0 0 9 * * *", Message: "新增任务",
		})
		require.NoError(t, err)

		out := string(readScheduleAgent(t, root, "tester"))
		require.Contains(t, out, "# 保留注释：手工维护")
		require.Contains(t, out, "x-custom: keep-me")
		require.Contains(t, out, "description: 工具说明")
		require.Contains(t, out, "identity:\n  prompt: 你好")
		require.Contains(t, out, "正文保持")
	})

	t.Run("CRLF preserved", func(t *testing.T) {
		root := t.TempDir()
		crlf := "---\r\n" +
			"name: tester\r\n" +
			"provider: {name: deepseek, model: deepseek-chat}\r\n" +
			"# CRLF 注释\r\n" +
			"schedules:\r\n" +
			"  - id: daily-report\r\n" +
			"    cron: \"0 0 8 * * *\"\r\n" +
			"    zone: Asia/Shanghai\r\n" +
			"    message: 每日报告\r\n" +
			"---\r\n\r\n" +
			"正文保持\r\n"
		writeScheduleFile(t, root, filepath.Join("agents", "tester", "AGENT.md"), crlf)

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "weekly-report", Cron: "0 0 9 * * 1", Message: "每周报告",
		})
		require.NoError(t, err)

		out := string(readScheduleAgent(t, root, "tester"))
		require.Contains(t, out, "# CRLF 注释\r\n", "块外注释与换行风格逐字节保留")
		require.Contains(t, out, "---\r\n\r\n正文保持\r\n", "正文与围栏保留 CRLF")
		require.NotContains(t, out, "\n\n---\r\n", "换行风格不得被改写为混合形态")
	})

	t.Run("zero-indent sequence replaced completely", func(t *testing.T) {
		root := t.TempDir()
		fm := "name: tester\n" +
			"provider: {name: deepseek, model: deepseek-chat}\n" +
			"schedules:\n" +
			"- id: daily-report\n" +
			"  cron: \"0 0 8 * * *\"\n" +
			"  zone: Asia/Shanghai\n" +
			"  message: 每日报告\n" +
			"identity:\n" +
			"  prompt: 你好\n"
		writeScheduleAgent(t, root, "tester", fm, "正文保持")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "added", Cron: "0 0 9 * * *", Message: "新增任务",
		})
		require.NoError(t, err, "零缩进序列（合法 YAML）不得导致改写失败")

		out := string(readScheduleAgent(t, root, "tester"))
		require.Equal(t, 1, strings.Count(out, "daily-report"), "零缩进旧块必须整体被替换，不得残留")
		require.Contains(t, out, "identity:\n  prompt: 你好")
		require.Equal(t, []string{"daily-report", "added"}, scheduleIDs(currentScheduleSet(t, root, "tester")))
	})

	// 用户 review ② 中危项回归：块内顶格注释是**合法 YAML**（注释可出现在任意缩进），
	// 旧扫描把它当作块结束 → 旧任务留在文件里 → 已存在 id 与新候选相撞报「id 重复」。
	// 逆改点：扫描遇 # 即 break → 本用例 ApplySchedule 返回 ErrScheduleRejected。
	t.Run("column-zero comment inside block does not split it", func(t *testing.T) {
		root := t.TempDir()
		fm := "schedules:\n" +
			"  - id: daily-report\n" +
			"    cron: \"0 0 8 * * *\"\n" +
			"    zone: Asia/Shanghai\n" +
			"    message: 每日报告\n" +
			"# 周任务：周一早九点\n" +
			"  - id: weekly-report\n" +
			"    cron: \"0 0 9 * * 1\"\n" +
			"    zone: UTC\n" +
			"    message: 每周报告\n"
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", fm), "正文")

		// 先证明合法输入可读（装载同一套解析器）。
		require.Equal(t, []string{"daily-report", "weekly-report"}, scheduleIDs(currentScheduleSet(t, root, "tester")))

		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "daily-report", Cron: "0 0 7 * * *", Message: "改后的每日报告",
		})
		require.NoError(t, err, "块内顶格注释不得导致旧块残留 / id 重复")
		require.Equal(t, []string{"daily-report", "weekly-report"}, scheduleIDs(got))
		require.Equal(t, "0 0 7 * * *", got[0].Cron)
		require.Equal(t, []string{"daily-report", "weekly-report"}, scheduleIDs(currentScheduleSet(t, root, "tester")))
	})

	// 注释归属的 lookahead：注释串之后是顶层键（而非块内内容）时，它属于下一个键，
	// 必须随块外字节一起保留——不能因为"块内注释合法"而把下一个键的注释一并吞掉。
	// 逆改点：把任何顶格注释都当块内内容 → 本用例注释被删。
	t.Run("comment belonging to next key preserved", func(t *testing.T) {
		root := t.TempDir()
		fm := oneDailyReport +
			"# identity 的说明（属于下一个键）\n" +
			"identity:\n" +
			"  prompt: 你好\n"
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", fm), "正文")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "added", Cron: "0 0 9 * * *", Message: "新增",
		})
		require.NoError(t, err)

		out := string(readScheduleAgent(t, root, "tester"))
		require.Contains(t, out, "# identity 的说明（属于下一个键）\nidentity:\n  prompt: 你好")
		require.Equal(t, []string{"daily-report", "added"}, scheduleIDs(currentScheduleSet(t, root, "tester")))
	})

	t.Run("missing schedules key inserts before closing fence", func(t *testing.T) {
		root := t.TempDir()
		fm := "name: tester\n" +
			"provider: {name: deepseek, model: deepseek-chat}\n" +
			"identity:\n" +
			"  prompt: 你好\n"
		writeScheduleAgent(t, root, "tester", fm, "正文")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "first", Cron: "0 0 8 * * *", Message: "首个任务",
		})
		require.NoError(t, err)

		out := string(readScheduleAgent(t, root, "tester"))
		require.Less(t, strings.Index(out, "schedules:"), strings.LastIndex(out, "\n---"), "必须插在收尾围栏之前")
		require.Equal(t, []string{"first"}, scheduleIDs(currentScheduleSet(t, root, "tester")))
	})
}

// TestApplySchedule_RejectsInvalidCandidate 覆盖四类候选拒绝：坏 cron、5 段、重复 id、
// 跨 Agent claim 冲突——原文一律不变（先验后写，约束 27）。
// 逆改点：跳过同源校验直接落盘 / 把冲突写成成功 → 错误类型与字节断言红。
func TestApplySchedule_RejectsInvalidCandidate(t *testing.T) {
	t.Run("bad cron", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")
		before := readScheduleAgent(t, root, "tester")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "bad", Cron: "not a cron", Message: "坏表达式",
		})
		require.ErrorIs(t, err, ErrScheduleRejected)
		require.Equal(t, before, readScheduleAgent(t, root, "tester"))
	})

	t.Run("five-field cron", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")
		before := readScheduleAgent(t, root, "tester")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "five", Cron: "0 8 * * *", Message: "五段",
		})
		require.ErrorIs(t, err, ErrScheduleRejected)
		require.Contains(t, err.Error(), "Cron expression must consist of 6 fields")
		require.Equal(t, before, readScheduleAgent(t, root, "tester"))
	})

	t.Run("duplicate id", func(t *testing.T) {
		root := t.TempDir()
		dup := "schedules:\n" +
			"  - id: dup\n" +
			"    cron: \"0 0 8 * * *\"\n" +
			"    zone: UTC\n" +
			"    message: 一\n" +
			"  - id: dup\n" +
			"    cron: \"0 0 9 * * *\"\n" +
			"    zone: UTC\n" +
			"    message: 二\n"
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", dup), "正文")
		before := readScheduleAgent(t, root, "tester")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "other", Cron: "0 0 10 * * *", Message: "新增",
		})
		require.ErrorIs(t, err, ErrScheduleRejected)
		require.Contains(t, err.Error(), "id 重复")
		require.Equal(t, before, readScheduleAgent(t, root, "tester"))
	})

	t.Run("cross-agent claim conflict", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", ""), "正文")
		before := readScheduleAgent(t, root, "tester")
		claims := map[string][]core.Schedule{
			"other": {{ID: "shared", Cron: "0 0 8 * * *", Zone: "UTC", Message: "别人的任务"}},
		}

		_, err := newScheduleLoader().ApplySchedule(root, claims, "tester", core.ScheduleSet, core.Schedule{
			ID: "shared", Cron: "0 0 9 * * *", Message: "抢占",
		})
		require.ErrorIs(t, err, ErrScheduleRejected)
		require.Contains(t, err.Error(), "other")
		require.Equal(t, before, readScheduleAgent(t, root, "tester"))
	})
}

// TestApplySchedule_CrossAgentIDRejected 锁定 remove 的越界错误类型（约束 31 / F4）：
// 命中其他已注册 Agent 的 id → ErrScheduleRejected（invalid_input），不是 not_found。
// 逆改点：先按本 Agent 集合查不到就返回 fs.ErrNotExist → 错误类型断言红。
func TestApplySchedule_CrossAgentIDRejected(t *testing.T) {
	root := t.TempDir()
	writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", ""), "正文")
	before := readScheduleAgent(t, root, "tester")
	claims := map[string][]core.Schedule{
		"other": {{ID: "shared", Cron: "0 0 8 * * *", Zone: "UTC", Message: "别人的任务"}},
	}

	_, err := newScheduleLoader().ApplySchedule(root, claims, "tester", core.ScheduleRemove, core.Schedule{ID: "shared"})
	require.ErrorIs(t, err, ErrScheduleRejected)
	require.False(t, errors.Is(err, fs.ErrNotExist), "跨 Agent 越界不得归类为 not_found")
	require.Contains(t, err.Error(), "other")
	require.Equal(t, before, readScheduleAgent(t, root, "tester"))
}

// TestApplySchedule_RemoveUnknownIDNotFound 覆盖普通未知 id：fs.ErrNotExist 链
// （工具侧据此归类 not_found）。
func TestApplySchedule_RemoveUnknownIDNotFound(t *testing.T) {
	root := t.TempDir()
	writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")

	_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleRemove, core.Schedule{ID: "ghost"})
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestApplySchedule_NoTempResidue 覆盖成功与失败两条路径都不留临时文件（约束 27）。
// 逆改点：失败路径不 Remove 临时文件 → 目录扫描断言红。
func TestApplySchedule_NoTempResidue(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "added", Cron: "0 0 9 * * *", Message: "新增",
		})
		require.NoError(t, err)
		requireNoTempResidue(t, root, "tester")
	})

	t.Run("failure", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")

		_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "bad", Cron: "not a cron", Message: "坏表达式",
		})
		require.ErrorIs(t, err, ErrScheduleRejected)
		requireNoTempResidue(t, root, "tester")
	})
}

// TestApplySchedule_AtomicOnRenameFailure rename 失败时原文逐字节不变、无临时残留：
// 经生产代码里的未导出注入点（rename 生产恒为 nil → os.Root.Rename）确定性触发。
// 逆改点：先截断 / 覆盖原文件再 rename → 字节断言红；失败不清理 → 残留断言红。
func TestApplySchedule_AtomicOnRenameFailure(t *testing.T) {
	root := t.TempDir()
	writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")
	before := readScheduleAgent(t, root, "tester")

	loader := newScheduleLoader()
	loader.rename = func(*os.Root, string, string) error { return errInjectedScheduleRename }

	_, err := loader.ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
		ID: "added", Cron: "0 0 9 * * *", Message: "新增",
	})
	require.ErrorIs(t, err, errInjectedScheduleRename)
	require.Equal(t, before, readScheduleAgent(t, root, "tester"), "rename 失败时原文件必须逐字节不变")
	requireNoTempResidue(t, root, "tester")
}

// TestApplySchedule_ConcurrentSets 验证 writeMu 串行化「读-改-写」全过程：
// 并发 set 不得互相覆盖，最终文件仍是合法 frontmatter 且包含全部任务。
// 逆改点：去掉锁 / 锁范围只覆盖写 → 丢更新，任务数断言红（-race 归 010-race）。
func TestApplySchedule_ConcurrentSets(t *testing.T) {
	root := t.TempDir()
	writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文")
	loader := newScheduleLoader()

	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = loader.ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
				ID:      fmt.Sprintf("task-%d", i),
				Cron:    "0 0 9 * * *",
				Message: fmt.Sprintf("并发任务 %d", i),
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		require.NoErrorf(t, err, "writer %d", i)
	}

	stored := currentScheduleSet(t, root, "tester")
	require.Len(t, stored, writers+1, "并发写必须全部保留（不得丢更新）")
	ids := scheduleIDs(stored)
	for i := range writers {
		require.Contains(t, ids, fmt.Sprintf("task-%d", i))
	}
}

// TestReadSchedules 覆盖读面两态：返回文件当前内容（含派生 id）、未知 Agent
// 保持 fs.ErrNotExist 链。
// 逆改点：读 DB 或做过期缓存 → 派生 id / 新写入不可见；未知 Agent 返回空列表 → 断言红。
func TestReadSchedules(t *testing.T) {
	t.Run("returns current file with derived id", func(t *testing.T) {
		root := t.TempDir()
		fm := "schedules:\n" +
			"  - cron: \"0 0 8 * * *\"\n" +
			"    message: 定时播报\n"
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", fm), "正文")

		got, err := newScheduleLoader().ReadSchedules(root, "tester")
		require.NoError(t, err)
		require.Equal(t, []string{"tester-1"}, scheduleIDs(got))
		require.Equal(t, "UTC", got[0].Zone)
	})

	t.Run("unknown agent", func(t *testing.T) {
		_, err := newScheduleLoader().ReadSchedules(t.TempDir(), "ghost")
		require.ErrorIs(t, err, fs.ErrNotExist)
	})
}

// TestApplySchedule_DoesNotTouchOtherAgents 只改目标目录：同工作区其他 Agent 的
// AGENT.md 逐字节不变。
// 逆改点：按目录扫描后整批重写 → 其他 Agent 的字节断言红。
func TestApplySchedule_DoesNotTouchOtherAgents(t *testing.T) {
	root := t.TempDir()
	writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", oneDailyReport), "正文 A")
	writeScheduleAgent(t, root, "other", baseScheduleFrontmatter("other", oneDailyReport), "正文 B")
	beforeOther := readScheduleAgent(t, root, "other")

	_, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
		ID: "added", Cron: "0 0 9 * * *", Message: "新增",
	})
	require.NoError(t, err)
	require.Equal(t, beforeOther, readScheduleAgent(t, root, "other"))
}

// TestApplySchedule_ClaimBaseline 锁定「候选校验用的占用集 = 传入的已注册 claims」：
// 已注册 Agent 的条目进入基线（跨 Agent 冲突被拒）；未注册目录不计（写入门不扫盘）。
// 逆改点：忽略 claims 只看本 Agent → 冲突用例红；反向从磁盘扫描所有目录 → 未注册用例红。
func TestApplySchedule_ClaimBaseline(t *testing.T) {
	t.Run("registered entries enter baseline", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", ""), "正文")
		claims := map[string][]core.Schedule{
			"registered": {{ID: "taken", Cron: "0 0 8 * * *", Zone: "UTC", Message: "已注册任务"}},
		}

		_, err := newScheduleLoader().ApplySchedule(root, claims, "tester", core.ScheduleSet, core.Schedule{
			ID: "taken", Cron: "0 0 9 * * *", Message: "抢占",
		})
		require.ErrorIs(t, err, ErrScheduleRejected)
	})

	t.Run("unregistered directories do not count", func(t *testing.T) {
		root := t.TempDir()
		writeScheduleAgent(t, root, "tester", baseScheduleFrontmatter("tester", ""), "正文")
		// other 目录在盘上但**未注册**（claims 里没有它）：不计入基线。
		writeScheduleAgent(t, root, "other", baseScheduleFrontmatter("other", oneDailyReport), "正文 B")

		got, err := newScheduleLoader().ApplySchedule(root, nil, "tester", core.ScheduleSet, core.Schedule{
			ID: "daily-report", Cron: "0 0 9 * * *", Message: "接管未注册目录的 id",
		})
		require.NoError(t, err)
		require.Equal(t, []string{"daily-report"}, scheduleIDs(got))
	})
}
