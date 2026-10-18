package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// maxNotifyContentBytes 是 content 的上限（20 KiB，UTF-8 字节计）。上限在**查表之前**
// 判定——超限请求不触达存储层，也不产生任何外呼。
// maxNotifyContentBytes caps content at 20 KiB of UTF-8 bytes; the check runs before the
// registry read so an over-limit request never reaches storage or the network.
const maxNotifyContentBytes = 20 * 1024

// notifyInputSchema 是工具入参的固定字面量（契约 §5.5）：`required` 恰好只有 `content`；
// `channel` 可选——缺失留给运行期给出带可用渠道名的可读失败，而不是被 schema 拦下。
// The input schema literal from the contract: only content is required.
const notifyInputSchema = `{"type":"object","properties":{"channel":{"type":"string","description":"通知渠道名（notify_channels 注册表里的 name）；缺省即失败，不猜默认渠道"},"content":{"type":"string","description":"推送内容；按 UTF-8 字节计最多 20 KB"}},"required":["content"]}`

// NotifyTool 是通知推送工具：纯判定与分派，不落审计、不写消息、不记日志。
// NotifyTool pushes text to a registered channel; it writes no audit, message or log.
type NotifyTool struct {
	channels  core.NotifyChannelReader
	policy    core.Policy
	adapters  map[string]NotifyAdapter
	lookupEnv func(string) (string, bool)
}

// 编译期断言：本工具就是循环消费的统一 Tool 接口。
// Compile-time assertion that the tool satisfies the single Tool interface.
var _ core.Tool = (*NotifyTool)(nil)

// NewNotifyTool 克隆适配器映射（调用方事后改原映射不影响工具）；lookupEnv 缺省
// `os.LookupEnv`——环境变量是 `${VAR}` 引用的唯一真值来源。
// NewNotifyTool clones the adapter map and defaults the env lookup to os.LookupEnv.
func NewNotifyTool(channels core.NotifyChannelReader, policy core.Policy,
	adapters map[string]NotifyAdapter,
) *NotifyTool {
	cloned := make(map[string]NotifyAdapter, len(adapters))
	maps.Copy(cloned, adapters)
	return &NotifyTool{
		channels:  channels,
		policy:    policy,
		adapters:  cloned,
		lookupEnv: os.LookupEnv,
	}
}

// Name 是工具名（注册名 = 调用名，逐字一致）。
// Name returns the tool name.
func (t *NotifyTool) Name() string { return "notify" }

// Description 是模型可见的一句话说明。
// Description is the one-line model-facing description.
func (t *NotifyTool) Description() string {
	return "把一段文本推送到管理员登记过的通知渠道；channel 必须是注册表里的渠道名，没有默认渠道"
}

// InputSchema 返回入参 JSON Schema（固定字面量，测试逐字钉住）。
// InputSchema returns the fixed JSON schema of the input.
func (t *NotifyTool) InputSchema() json.RawMessage {
	return json.RawMessage(notifyInputSchema)
}

// Execute 执行一次推送：解码 → 上限 → 读表 → 精确匹配渠道 → 查类型 → 校验占位 →
// 解析环境 → 策略判定 → 投递。任一前置失败都在外呼之前返回。
// Execute performs one push: decode, cap, registry read, exact channel match, type
// lookup, placeholder check, env resolution, policy judgement, then delivery. Every
// precondition fails before any outbound call.
func (t *NotifyTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	in, err := decodeNotifyInput(input)
	if err != nil {
		return core.Result{}, err
	}
	if len(*in.Content) > maxNotifyContentBytes {
		return core.Result{}, &core.InvalidInputError{
			Tool: "notify",
			Reason: fmt.Sprintf("content 超过 %d 字节上限（实际 %d 字节）",
				maxNotifyContentBytes, len(*in.Content)),
		}
	}

	channels, err := t.channels.ListNotifyChannels(ctx)
	if err != nil {
		if ctxErr := ctxPreferred(ctx, err); ctxErr != nil {
			return core.Result{}, ctxErr
		}
		return core.Result{}, fmt.Errorf("读取通知渠道注册表失败: %w", err)
	}

	name := strings.TrimSpace(in.Channel)
	if name == "" {
		return core.Result{}, missingChannelError(channels)
	}
	channel, ok := findNotifyChannel(channels, name)
	if !ok {
		return core.Result{}, unknownChannelError(name, channels)
	}

	adapter := t.adapters[channel.Type]
	if adapter == nil {
		return core.Result{}, &notifyTypeUnregisteredError{
			Channel:     channel.Name,
			ChannelType: channel.Type,
			Available:   availableNotifyTypes(t.adapters),
		}
	}

	varName, ok := core.EnvPlaceholderName(channel.URL)
	if !ok {
		return core.Result{}, fmt.Errorf("通知渠道 %s 的地址必须是 ${VAR} 占位（凭证不得明文入库）", channel.Name)
	}
	target, ok := t.lookupEnv(varName)
	if !ok || target == "" {
		return core.Result{}, fmt.Errorf("通知渠道 %s 引用的环境变量 ${%s} 未设置或为空", channel.Name, varName)
	}

	if err := t.policy.Check(ctx, core.Action{
		Type:   core.ActionHTTPRequest,
		Target: target,
		Agent:  env.AgentName,
	}); err != nil {
		return core.Result{}, redactNotifyPolicyError(err, target)
	}

	if err := adapter.Send(ctx, target, *in.Content); err != nil {
		return core.Result{}, err
	}
	return core.Result{Content: "已推送到通知渠道：" + channel.Name, Retryable: false}, nil
}

// notifyInput 用指针区分 content 缺失 / null 与空串（空串放行，见已决 #21）。
// notifyInput uses a pointer to tell a missing/null content from the empty string.
type notifyInput struct {
	Channel string  `json:"channel"`
	Content *string `json:"content"`
}

// decodeNotifyInput 解出入参并区分「整段不是 JSON 对象」与「content 缺失 / null」。
// 字段类型由 json.Unmarshal 判（数字 / 布尔 / 对象等非字符串 `channel` 落在这里）；
// 例外：JSON `null` 解到 string 字段不报错、保持空串，因此 `channel:null` 按「缺失
// 渠道」走 not_found，而不是解码点类型错误（N18 补充用例钉住该行为）。
// decodeNotifyInput splits "not an object" from "content missing/null"; field types are
// judged by json.Unmarshal, so a non-string channel lands here too. Exception: JSON null
// into a string field is a no-op, so a null channel means "missing" (not_found).
func decodeNotifyInput(input json.RawMessage) (notifyInput, error) {
	var in notifyInput
	if err := json.Unmarshal(input, &in); err != nil {
		return notifyInput{}, &core.InvalidInputError{
			Tool:   "notify",
			Reason: "notify 入参必须是 JSON 对象（channel 为字符串、content 为字符串）",
		}
	}
	if in.Content == nil {
		return notifyInput{}, &core.InvalidInputError{
			Tool:   "notify",
			Reason: "notify 缺少必填字段 content（字符串，按 UTF-8 字节最多 20 KB）",
		}
	}
	return in, nil
}

// findNotifyChannel 按渠道名做大小写敏感的精确匹配（大小写折叠会掩盖登记错误）。
// findNotifyChannel does an exact, case-sensitive match.
func findNotifyChannel(channels []core.NotifyChannel, name string) (core.NotifyChannel, bool) {
	for _, channel := range channels {
		if channel.Name == name {
			return channel, true
		}
	}
	return core.NotifyChannel{}, false
}

// availableChannelNames 渲染可用渠道名（`、` 连接）；空表明确写「注册表为空」——
// 「没有渠道可发」与「渠道名写错」是两条不同的自我纠正路径。
// availableChannelNames joins the names with a Chinese enumeration comma; an empty
// registry says so explicitly.
func availableChannelNames(channels []core.NotifyChannel) string {
	if len(channels) == 0 {
		return "（注册表为空）"
	}
	names := make([]string, 0, len(channels))
	for _, channel := range channels {
		names = append(names, channel.Name)
	}
	return strings.Join(names, "、")
}

// missingChannelError 构造「未指定渠道」的 not_found 失败（无默认渠道）。
// missingChannelError builds the not_found failure for a missing channel.
func missingChannelError(channels []core.NotifyChannel) error {
	return &notifyNotFoundError{
		Reason: fmt.Sprintf("未指定通知渠道（可用渠道：%s）", availableChannelNames(channels)),
	}
}

// unknownChannelError 构造「渠道名查无」的 not_found 失败，文案点名请求名与可用名。
// unknownChannelError builds the not_found failure for an unknown channel name.
func unknownChannelError(name string, channels []core.NotifyChannel) error {
	return &notifyNotFoundError{
		Reason: fmt.Sprintf("通知渠道 %s 不存在（可用渠道：%s）", name, availableChannelNames(channels)),
	}
}

// availableNotifyTypes 渲染已装配的类型键（排序后连接）——诊断文案不依赖 map 遍历顺序。
// availableNotifyTypes renders the assembled type keys in a stable order.
func availableNotifyTypes(adapters map[string]NotifyAdapter) string {
	types := make([]string, 0, len(adapters))
	for name := range adapters {
		types = append(types, name)
	}
	slices.Sort(types)
	if len(types) == 0 {
		return "（无）"
	}
	return strings.Join(types, "、")
}

// notifyNotFoundError 是渠道缺失 / 查无的专用错误：Error 只回可读文案，Unwrap 回
// fs.ErrNotExist——执行器据错误链把这类失败归 not_found，不比对文本。
// notifyNotFoundError carries a channel-not-found failure; Unwrap returns
// fs.ErrNotExist so the executor classifies by type, never by message text.
type notifyNotFoundError struct {
	Reason string
}

// Error 返回可读文案（含可用渠道名，供模型自我纠正）。
// Error returns the readable reason with the available channel names.
func (e *notifyNotFoundError) Error() string { return e.Reason }

// Unwrap 把渠道类失败锚到 fs.ErrNotExist（执行器既有分类判据）。
// Unwrap anchors the channel failure to fs.ErrNotExist.
func (e *notifyNotFoundError) Unwrap() error { return fs.ErrNotExist }

// notifyTypeUnregisteredError 是类型认领失败的专用错误（归 runtime_error）：装配层
// 没有该类型的适配器，属本实例装配问题，不是渠道数据问题。
// notifyTypeUnregisteredError reports a channel type the assembly never registered;
// this is an instance wiring problem, not a registry-data problem.
type notifyTypeUnregisteredError struct {
	Channel     string
	ChannelType string
	Available   string
}

// Error 点名渠道、类型与已装配类型集合。
// Error names the channel, its type, and the assembled type set.
func (e *notifyTypeUnregisteredError) Error() string {
	return fmt.Sprintf("通知渠道 %s 的类型 %s 未装配（已装配类型：%s）",
		e.Channel, e.ChannelType, e.Available)
}

// notifyDisplayURL 返回 scheme://host（不含端口 / userinfo / path / query）；
// 解析不出返回既有 targetUnresolvable（"（目标无法解析）"），不返回空串——空串会让
// 四段式段 2 变成两个标点之间的空位，模型看不出"有目标但解析不了"。
// notifyDisplayURL returns scheme://host only; an unparsable URL falls back to the
// existing placeholder rather than an empty segment.
func notifyDisplayURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return targetUnresolvable
	}
	return parsed.Scheme + "://" + parsed.Hostname()
}

// redactNotifyPolicyError 只重写沙箱拒绝的 Target（Rule / Guidance 原样保留）；非沙箱
// 错误原样返回。重写按错误链判定，对任何 Policy 实现生效——夹具模拟的最坏实现（段 2
// 嵌完整 URL）也逃不过。
// redactNotifyPolicyError rewrites only the target of a sandbox refusal, preserving the
// rule and guidance verbatim; any other error passes through unchanged.
func redactNotifyPolicyError(err error, rawURL string) error {
	var violation *core.SandboxViolationError
	if !errors.As(err, &violation) {
		return err
	}
	redacted := *violation
	redacted.Target = notifyDisplayURL(rawURL)
	return &redacted
}
