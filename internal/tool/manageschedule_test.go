package tool

// 本文件覆盖第 10 个内置工具 manage_schedule（颗粒度文档 §3.2 的 10 函数 / 15 例）：
// 身份只取 env.AgentName、动作 / 必填校验、成功确认含 id、失败载体（invalid_input /
// not_found）与未绑定槽位的显式失败。
// Tests for the tenth builtin tool manage_schedule: env-only identity, required fields,
// id-bearing confirmations, failure carriers, and the unbound-slot failure.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// scheduleWriteCall 是一次编辑调用的判卷形态（身份 / 动作 / 任务的逐字段对照）。
// scheduleWriteCall is the assertion shape of one editor call.
type scheduleWriteCall struct {
	Agent  string
	Action core.ScheduleAction
	Task   core.Schedule
}

// fakeScheduleEditor 记录调用并返回预置结果；读写错误可分别注入。
// fakeScheduleEditor records calls and returns configured results; read and write errors
// are injectable independently.
type fakeScheduleEditor struct {
	mu       sync.Mutex
	reads    []string
	writes   []scheduleWriteCall
	tasks    []core.Schedule
	readErr  error
	writeErr error
}

// ReadSchedules 记录身份并返回预置任务集的克隆。
// ReadSchedules records the agent and returns a clone of the configured tasks.
func (f *fakeScheduleEditor) ReadSchedules(_ context.Context, agent string) ([]core.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, agent)
	if f.readErr != nil {
		return nil, f.readErr
	}
	return append([]core.Schedule(nil), f.tasks...), nil
}

// ApplySchedule 记录身份 / 动作 / 任务并返回落盘后的任务集。
// ApplySchedule records the call and returns the post-write task set.
func (f *fakeScheduleEditor) ApplySchedule(_ context.Context, agent string, action core.ScheduleAction, task core.Schedule) ([]core.Schedule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, scheduleWriteCall{Agent: agent, Action: action, Task: task})
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return append([]core.Schedule(nil), f.tasks...), nil
}

// readCalls 返回读调用的身份序列。
func (f *fakeScheduleEditor) readCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reads...)
}

// writeCalls 返回写调用的记录序列。
func (f *fakeScheduleEditor) writeCalls() []scheduleWriteCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]scheduleWriteCall(nil), f.writes...)
}

// TestManageSchedule_NoAgentContextRejected 锁定约束 25：无 Agent 上下文一律拒绝，
// 且不触达编辑器（零副作用）。
// 逆改点：缺省身份回落成空串继续执行 → 错误载体断言与零调用断言红。
func TestManageSchedule_NoAgentContextRejected(t *testing.T) {
	editor := &fakeScheduleEditor{}
	tool := NewManageScheduleTool(editor)

	_, err := tool.Execute(context.Background(), core.ToolEnv{}, json.RawMessage(`{"action":"list"}`))
	require.True(t, core.IsInvalidInput(err), "无 Agent 上下文必须归 invalid_input")
	require.Empty(t, editor.readCalls())
	require.Empty(t, editor.writeCalls())
}

// TestManageSchedule_List 覆盖读面的两态：用 env 身份读、结果文本含全部任务 id。
// 逆改点：用入参里的身份读 → 身份断言红；结果不带 id → 文本断言红。
func TestManageSchedule_List(t *testing.T) {
	t.Run("uses env agent", func(t *testing.T) {
		editor := &fakeScheduleEditor{tasks: []core.Schedule{
			{ID: "daily-report", Cron: "0 0 8 * * *", Zone: "Asia/Shanghai", Message: "每日报告"},
		}}
		tool := NewManageScheduleTool(editor)

		_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
			json.RawMessage(`{"action":"list"}`))
		require.NoError(t, err)
		require.Equal(t, []string{"tester"}, editor.readCalls())
	})

	t.Run("result text contains ids", func(t *testing.T) {
		editor := &fakeScheduleEditor{tasks: []core.Schedule{
			{ID: "daily-report", Cron: "0 0 8 * * *", Zone: "UTC", Message: "每日报告"},
			{ID: "weekly-report", Cron: "0 0 9 * * 1", Zone: "UTC", Message: "每周报告"},
		}}
		tool := NewManageScheduleTool(editor)

		got, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
			json.RawMessage(`{"action":"list"}`))
		require.NoError(t, err)
		require.Contains(t, got.Content, "daily-report")
		require.Contains(t, got.Content, "weekly-report")
	})
}

// TestManageSchedule_Set 覆盖 set 的三态：必填校验（cron / message）、动作与任务
// 逐字段透传、成功确认含 id。
// 逆改点：缺 cron 仍透传 → 零写调用断言红；透传时丢字段 → 逐字段断言红。
func TestManageSchedule_Set(t *testing.T) {
	t.Run("requires cron and message", func(t *testing.T) {
		editor := &fakeScheduleEditor{}
		tool := NewManageScheduleTool(editor)

		for _, input := range []string{
			`{"action":"set","id":"a","message":"有消息没 cron"}`,
			`{"action":"set","id":"a","cron":"0 0 8 * * *"}`,
		} {
			_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"}, json.RawMessage(input))
			require.True(t, core.IsInvalidInput(err), "set 缺必填必须归 invalid_input：%s", input)
		}
		require.Empty(t, editor.writeCalls())
	})

	t.Run("passes action and task through", func(t *testing.T) {
		editor := &fakeScheduleEditor{}
		tool := NewManageScheduleTool(editor)

		_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
			json.RawMessage(`{"action":"set","id":"daily-report","cron":"0 0 8 * * *","zone":"Asia/Shanghai","message":"每日报告"}`))
		require.NoError(t, err)
		require.Equal(t, []scheduleWriteCall{{
			Agent:  "tester",
			Action: core.ScheduleSet,
			Task: core.Schedule{
				ID: "daily-report", Cron: "0 0 8 * * *", Zone: "Asia/Shanghai", Message: "每日报告",
			},
		}}, editor.writeCalls())
	})

	t.Run("confirmation contains id", func(t *testing.T) {
		editor := &fakeScheduleEditor{tasks: []core.Schedule{
			{ID: "daily-report", Cron: "0 0 10 * * *", Zone: "UTC", Message: "改后的任务"},
		}}
		tool := NewManageScheduleTool(editor)

		got, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
			json.RawMessage(`{"action":"set","cron":"0 0 10 * * *","message":"改后的任务"}`))
		require.NoError(t, err)
		require.Contains(t, got.Content, "daily-report", "省略 id 时确认必须含实际 id")
	})
}

// TestManageSchedule_Remove 覆盖 remove 的两态：id 必填、成功确认含 id。
// 逆改点：缺 id 仍调用编辑器 → 零写调用断言红。
func TestManageSchedule_Remove(t *testing.T) {
	t.Run("requires id", func(t *testing.T) {
		editor := &fakeScheduleEditor{}
		tool := NewManageScheduleTool(editor)

		_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
			json.RawMessage(`{"action":"remove"}`))
		require.True(t, core.IsInvalidInput(err))
		require.Empty(t, editor.writeCalls())
	})

	t.Run("confirmation contains id", func(t *testing.T) {
		editor := &fakeScheduleEditor{}
		tool := NewManageScheduleTool(editor)

		got, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
			json.RawMessage(`{"action":"remove","id":"daily-report"}`))
		require.NoError(t, err)
		require.Contains(t, got.Content, "daily-report")
		require.Equal(t, core.ScheduleRemove, editor.writeCalls()[0].Action)
	})
}

// TestManageSchedule_InvalidAction 覆盖未知动作：invalid_input 且零调用。
// 逆改点：未知动作按 list 兜底 → 错误载体与零调用断言红。
func TestManageSchedule_InvalidAction(t *testing.T) {
	editor := &fakeScheduleEditor{}
	tool := NewManageScheduleTool(editor)

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
		json.RawMessage(`{"action":"delete"}`))
	require.True(t, core.IsInvalidInput(err))
	require.Empty(t, editor.readCalls())
	require.Empty(t, editor.writeCalls())
}

// TestManageSchedule_UsesEnvAgentOnly 锁定约束 25：入参里的 agent 字段一律忽略，
// 身份只取 ToolEnv.AgentName。
// 逆改点：优先读入参 agent → 身份断言红（会读到 other）。
func TestManageSchedule_UsesEnvAgentOnly(t *testing.T) {
	editor := &fakeScheduleEditor{}
	tool := NewManageScheduleTool(editor)

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
		json.RawMessage(`{"action":"list","agent":"other"}`))
	require.NoError(t, err)
	require.Equal(t, []string{"tester"}, editor.readCalls(), "入参 agent 不得承载身份")
}

// TestManageSchedule_NotFoundClassified 锁定 remove 未知 id / Agent 未注册的载体：
// 错误链保留 fs.ErrNotExist，由 core.classifyToolError 归 not_found。
// 逆改点：把 not_found 吞成成功或改写成 internal → errors.Is 断言红。
func TestManageSchedule_NotFoundClassified(t *testing.T) {
	editor := &fakeScheduleEditor{writeErr: fmt.Errorf("任务 id ghost 不存在: %w", fs.ErrNotExist)}
	tool := NewManageScheduleTool(editor)

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
		json.RawMessage(`{"action":"remove","id":"ghost"}`))
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestManageSchedule_InvalidInputClassified 覆盖两类：编辑器拒绝（ErrScheduleRejected
// 已被适配器转为 *core.InvalidInputError）原样透传；入参 schema 字面量逐字钉住。
// 逆改点：把 InvalidInputError 包成普通 error → errors.As 断言红；schema 改动 → 字面对照红。
func TestManageSchedule_InvalidInputClassified(t *testing.T) {
	t.Run("rejection passes through", func(t *testing.T) {
		editor := &fakeScheduleEditor{writeErr: &core.InvalidInputError{
			Tool:   "manage_schedule",
			Reason: "任务 id shared 属于 Agent other，不得跨 Agent 修改",
		}}
		tool := NewManageScheduleTool(editor)

		_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
			json.RawMessage(`{"action":"remove","id":"shared"}`))
		require.True(t, core.IsInvalidInput(err))
		require.Contains(t, err.Error(), "other")
	})

	t.Run("schema literal", func(t *testing.T) {
		want := `{"type":"object","properties":{` +
			`"action":{"type":"string","enum":["set","remove","list"],"description":"动作：set 新增或改写、remove 按 id 删除、list 列出本 Agent 的全部任务"},` +
			`"id":{"type":"string","description":"任务 id；remove 必填；set 省略时仅当该 Agent 当前恰有一个任务（定位其实际 id）"},` +
			`"cron":{"type":"string","description":"6 段 cron（秒 分 时 日 月 周）；set 必填"},` +
			`"zone":{"type":"string","description":"时区名；缺省 UTC"},` +
			`"message":{"type":"string","description":"到点发给自己的消息；set 必填"}},` +
			`"required":["action"]}`
		tool := NewManageScheduleTool(&fakeScheduleEditor{})
		require.Equal(t, want, string(tool.InputSchema()))
	})
}

// TestManageSchedule_UnboundSlotFails 覆盖装配期两段绑定：Bind 前调用必须显式报错，
// 不得静默降级（装配环见颗粒度文档 §3.1.6）。
func TestManageSchedule_UnboundSlotFails(t *testing.T) {
	slot := NewScheduleEditorSlot()
	_, err := slot.ReadSchedules(context.Background(), "tester")
	require.Error(t, err)
	require.Contains(t, err.Error(), "绑定")
	_, err = slot.ApplySchedule(context.Background(), "tester", core.ScheduleList, core.Schedule{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "绑定")
}

// TestManageSchedule_ToolSpec 锁定工具名与描述，并解析 schema 的关键形状
// （required 恰好 action，properties 恰好五键）。
func TestManageSchedule_ToolSpec(t *testing.T) {
	tool := NewManageScheduleTool(&fakeScheduleEditor{})
	require.Equal(t, "manage_schedule", tool.Name())
	require.NotEmpty(t, tool.Description())
	require.Contains(t, tool.Description(), "任务")

	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema(), &schema))
	require.Equal(t, []string{"action"}, schema.Required)
	require.Len(t, schema.Properties, 5)
	for _, key := range []string{"action", "id", "cron", "zone", "message"} {
		require.Contains(t, schema.Properties, key)
	}
	require.True(t, strings.Contains(string(tool.InputSchema()), `"set"`))
}
