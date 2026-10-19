package tool

// 第 10 个内置工具 manage_schedule（颗粒度文档 §3.1.6 / 约束 25、26、31）：
// 纯判定与分派——身份只取 ToolEnv.AgentName，入参不承载 Agent 身份；写操作一律经
// ScheduleEditor（生产装配为 app 的 scheduleEditorAdapter → 工作区唯一写入门），
// 工具自身不落盘、不解析 AGENT.md、不扫描其他 Agent 目录。
// The tenth builtin tool manage_schedule: judgement and dispatch only. Identity comes
// solely from ToolEnv.AgentName; every write goes through the ScheduleEditor backend.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/ymedword/basalt/internal/core"
)

// manageScheduleInputSchema 是工具入参的固定字面量（契约 agent-md §5.6）：`required`
// 恰好只有 `action`；`set` / `remove` 的条件必填在运行期判定（模型拿到可读失败，
// 而不是被 schema 拦成笼统错误）。测试逐字钉住。
// The fixed input schema literal from the contract: only action is unconditionally
// required; conditional requirements are judged at run time.
const manageScheduleInputSchema = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["set","remove","list"],"description":"动作：set 新增或改写、remove 按 id 删除、list 列出本 Agent 的全部任务"},` +
	`"id":{"type":"string","description":"任务 id；remove 必填；set 省略时仅当该 Agent 当前恰有一个任务（定位其实际 id）"},` +
	`"cron":{"type":"string","description":"6 段 cron（秒 分 时 日 月 周）；set 必填"},` +
	`"zone":{"type":"string","description":"时区名；缺省 UTC"},` +
	`"message":{"type":"string","description":"到点发给自己的消息；set 必填"}},` +
	`"required":["action"]}`

// ScheduleEditor 是 manage_schedule 的编辑后端（消费方接口定义在 tool；生产实现是
// app 的 scheduleEditorAdapter，测试可用 fake 替换）。
// ScheduleEditor is the editing backend consumed by the tool; the app adapter is the
// production implementation.
type ScheduleEditor interface {
	ReadSchedules(ctx context.Context, agent string) ([]core.Schedule, error)
	ApplySchedule(ctx context.Context, agent string, action core.ScheduleAction, task core.Schedule) ([]core.Schedule, error)
}

// ScheduleEditorSlot 解决装配环：注册表先于 runtime 构建，编辑器在 runtime 之后才有。
// Bind 之前调用返回明确错误（不得静默降级）。
// ScheduleEditorSlot resolves the assembly cycle: the registry is built before the
// runtime, so the editor is bound later. Calls before Bind fail explicitly.
type ScheduleEditorSlot struct {
	mu     sync.RWMutex
	editor ScheduleEditor
}

// NewScheduleEditorSlot 返回未绑定的槽位。
// NewScheduleEditorSlot returns an unbound slot.
func NewScheduleEditorSlot() *ScheduleEditorSlot { return &ScheduleEditorSlot{} }

// Bind 绑定编辑器（装配期一次；并发安全的发布）。
// Bind publishes the editor once during assembly.
func (s *ScheduleEditorSlot) Bind(editor ScheduleEditor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.editor = editor
}

// ReadSchedules 透传到已绑定的编辑器；未绑定即显式报错。
// ReadSchedules delegates to the bound editor; an unbound slot fails explicitly.
func (s *ScheduleEditorSlot) ReadSchedules(ctx context.Context, agent string) ([]core.Schedule, error) {
	editor, err := s.current()
	if err != nil {
		return nil, err
	}
	return editor.ReadSchedules(ctx, agent)
}

// ApplySchedule 透传到已绑定的编辑器；未绑定即显式报错。
// ApplySchedule delegates to the bound editor; an unbound slot fails explicitly.
func (s *ScheduleEditorSlot) ApplySchedule(ctx context.Context, agent string, action core.ScheduleAction, task core.Schedule) ([]core.Schedule, error) {
	editor, err := s.current()
	if err != nil {
		return nil, err
	}
	return editor.ApplySchedule(ctx, agent, action, task)
}

// current 取已绑定编辑器（读锁内取引用）。
func (s *ScheduleEditorSlot) current() (ScheduleEditor, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.editor == nil {
		return nil, errors.New("调度编辑器尚未绑定：装配顺序错误（slot.Bind 必须在工具被调用前完成）")
	}
	return s.editor, nil
}

// 编译期断言：槽位可作为工具后端使用。
var _ ScheduleEditor = (*ScheduleEditorSlot)(nil)

// ManageScheduleTool 是任务管理工具（第 10 件内置）。
// ManageScheduleTool is the tenth builtin tool.
type ManageScheduleTool struct {
	editor ScheduleEditor
}

// 编译期断言：本工具就是循环消费的统一 Tool 接口。
var _ core.Tool = (*ManageScheduleTool)(nil)

// NewManageScheduleTool 绑定编辑后端。
// NewManageScheduleTool binds the editing backend.
func NewManageScheduleTool(editor ScheduleEditor) *ManageScheduleTool {
	return &ManageScheduleTool{editor: editor}
}

// Name 是工具名（注册名 = 调用名，逐字一致）。
// Name returns the tool name.
func (t *ManageScheduleTool) Name() string { return "manage_schedule" }

// Description 是模型可见的一句话说明。
// Description is the one-line model-facing description.
func (t *ManageScheduleTool) Description() string {
	return "管理当前 Agent 自己的定时任务：set 新增或改写、remove 按 id 删除、list 列出全部任务；身份取自运行上下文，入参不能指定别的 Agent"
}

// InputSchema 返回入参 JSON Schema（固定字面量，测试逐字钉住）。
// InputSchema returns the fixed input schema literal.
func (t *ManageScheduleTool) InputSchema() json.RawMessage {
	return json.RawMessage(manageScheduleInputSchema)
}

// manageScheduleInput 是入参解码形态；message 用指针区分缺失 / null 与空串。
// manageScheduleInput is the decoded input; the message pointer tells missing/null from
// the empty string.
type manageScheduleInput struct {
	Action  string  `json:"action"`
	ID      string  `json:"id"`
	Cron    string  `json:"cron"`
	Zone    string  `json:"zone"`
	Message *string `json:"message"`
}

// Execute 执行一次动作：解码 → 身份 → （list 读 / set 与 remove 写）→ 确认。
// 身份只取 env.AgentName（约束 25）；入参里的 agent 字段被解码器忽略。
// Execute performs one action: decode, identity, then read or write through the editor.
// Identity comes solely from env.AgentName; any agent field in the input is ignored.
func (t *ManageScheduleTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	in, err := decodeManageScheduleInput(input)
	if err != nil {
		return core.Result{}, err
	}
	agent := strings.TrimSpace(env.AgentName)
	if agent == "" {
		return core.Result{}, &core.InvalidInputError{
			Tool:   "manage_schedule",
			Reason: "缺少 Agent 上下文：manage_schedule 只能管理当前 Agent 自己的定时任务",
		}
	}

	switch core.ScheduleAction(strings.TrimSpace(in.Action)) {
	case core.ScheduleList:
		tasks, err := t.editor.ReadSchedules(ctx, agent)
		if err != nil {
			return core.Result{}, err
		}
		return core.Result{Content: formatScheduleList(tasks)}, nil

	case core.ScheduleSet:
		if strings.TrimSpace(in.Cron) == "" || in.Message == nil || strings.TrimSpace(*in.Message) == "" {
			return core.Result{}, &core.InvalidInputError{
				Tool:   "manage_schedule",
				Reason: "set 必须提供 cron 与 message（zone 缺省 UTC；id 省略仅在当前恰有一个任务时生效）",
			}
		}
		task := core.Schedule{
			ID:      strings.TrimSpace(in.ID),
			Cron:    strings.TrimSpace(in.Cron),
			Zone:    strings.TrimSpace(in.Zone),
			Message: *in.Message,
		}
		after, err := t.editor.ApplySchedule(ctx, agent, core.ScheduleSet, task)
		if err != nil {
			return core.Result{}, err
		}
		return core.Result{Content: "已保存定时任务：" + confirmScheduleID(task.ID, after)}, nil

	case core.ScheduleRemove:
		id := strings.TrimSpace(in.ID)
		if id == "" {
			return core.Result{}, &core.InvalidInputError{
				Tool:   "manage_schedule",
				Reason: "remove 必须提供 id",
			}
		}
		if _, err := t.editor.ApplySchedule(ctx, agent, core.ScheduleRemove, core.Schedule{ID: id}); err != nil {
			return core.Result{}, err
		}
		return core.Result{Content: "已删除定时任务：" + id}, nil

	default:
		return core.Result{}, &core.InvalidInputError{
			Tool:   "manage_schedule",
			Reason: "action 必须是 set / remove / list 之一",
		}
	}
}

// decodeManageScheduleInput 解出入参并区分「整段不是 JSON 对象」与字段类型错误。
// decodeManageScheduleInput splits "not an object" from field-type failures.
func decodeManageScheduleInput(input json.RawMessage) (manageScheduleInput, error) {
	var in manageScheduleInput
	if err := json.Unmarshal(input, &in); err != nil {
		return manageScheduleInput{}, &core.InvalidInputError{
			Tool:   "manage_schedule",
			Reason: "manage_schedule 入参必须是 JSON 对象（action / id / cron / zone / message 均为字符串）",
		}
	}
	return in, nil
}

// formatScheduleList 渲染任务清单（每行 id 在前，便于模型与用户对号）。
// formatScheduleList renders one line per task with the id first.
func formatScheduleList(tasks []core.Schedule) string {
	if len(tasks) == 0 {
		return "当前没有定时任务"
	}
	lines := make([]string, 0, len(tasks)+1)
	lines = append(lines, fmt.Sprintf("当前共 %d 个定时任务：", len(tasks)))
	for _, s := range tasks {
		lines = append(lines, fmt.Sprintf("- %s | %s | %s | %s", s.ID, s.Cron, s.Zone, s.Message))
	}
	return strings.Join(lines, "\n")
}

// confirmScheduleID 取确认文本里的 id：显式 id 用入参；省略 id 时用落盘后的唯一任务
// 的实际 id（D-10——不派生 <agent>-1）。
// confirmScheduleID prefers the explicit id and falls back to the single persisted task's
// actual id when the input omitted one.
func confirmScheduleID(given string, after []core.Schedule) string {
	if given != "" {
		return given
	}
	if len(after) == 1 {
		return after[0].ID
	}
	return "(未返回 id)"
}
