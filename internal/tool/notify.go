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

// notifyDisabledNoneReason 是 `notify_mode: none` 的固定文案（契约 §5.5 / R4.7 逐字：
// 「该 Agent 未启用通知（notify_mode: none）」）——不查渠道、不投递。
// notifyDisabledNoneReason is the fixed none-mode text from the contract.
const notifyDisabledNoneReason = "该 Agent 未启用通知（notify_mode: none）"

// NotifyCredentialResolver 按渠道解析**当前** active 托管凭据对应的投递地址（R4.8）：
// 只在 `credential_mode: managed` 时使用，env 模式仍走 `${VAR}` 环境解析。每次发送都
// 重新解析、**不缓存**——撤销的凭据立刻停发，而不是继续用准入时的旧版本。返回的地址
// 只用于本次投递，**不得**进入任何对外文案；凭据已撤销须返回包着
// `core.ErrCredentialRevoked` 的错误，工具把它转成固定文案的 `notify_disabled`。
// 返回结构同时带上实际使用的凭据身份与版本号，供投递记录追溯（R4.8）。
// NotifyCredentialResolver resolves the current active credential of one managed channel
// on every send; a revoked credential must surface core.ErrCredentialRevoked.
type NotifyCredentialResolver interface {
	ResolveNotifyCredential(ctx context.Context, channel core.NotifyChannel) (core.NotifyCredentialResolution, error)
}

// NotifyDeliveryRecorder 是 `notify_deliveries` 的写入端口（实现见 storage）：一次工具
// 调用至多一条记录，主键是工具调用 id。这是**派生账**——工具调用审计（`tool_invocations`）
// 才是权威，所以写失败只降级为日志，不把已完成的发送改判为失败。
// NotifyDeliveryRecorder writes the one-per-invocation delivery record; a write failure is
// logged but never turns a completed send into a failure.
type NotifyDeliveryRecorder interface {
	Record(ctx context.Context, record core.NotifyDeliveryRecord) error
}

// NotifyTool 是通知推送工具：纯判定与分派，不写消息、不记日志；唯一的落库是成功投递后
// 的 `notify_deliveries` 派生行（Run 内、且执行器已注入工具调用 id 时）。
// NotifyTool pushes text to a registered channel. Its only write is the derived
// notify_deliveries row after a successful delivery inside a run.
type NotifyTool struct {
	channels    core.NotifyChannelReader
	policy      core.Policy
	adapters    map[string]NotifyAdapter
	credentials NotifyCredentialResolver
	deliveries  NotifyDeliveryRecorder
	lookupEnv   func(string) (string, bool)
}

// 编译期断言：本工具就是循环消费的统一 Tool 接口。
// Compile-time assertion that the tool satisfies the single Tool interface.
var _ core.Tool = (*NotifyTool)(nil)

// NewNotifyTool 克隆适配器映射（调用方事后改原映射不影响工具）；lookupEnv 缺省
// `os.LookupEnv`——环境变量是 `${VAR}` 引用的唯一真值来源。`credentials` 可为 nil：
// 此时任何 `managed` 渠道都失败关闭（本实例不能解析托管地址）。`deliveries` 可为 nil：
// 此时不写投递记录（非 Run 直调与没接仓储的测试实例）。
// NewNotifyTool clones the adapter map and defaults the env lookup to os.LookupEnv. A nil
// resolver makes every managed channel fail closed; a nil recorder writes no delivery.
func NewNotifyTool(channels core.NotifyChannelReader, policy core.Policy,
	adapters map[string]NotifyAdapter, credentials NotifyCredentialResolver,
	deliveries NotifyDeliveryRecorder,
) *NotifyTool {
	cloned := make(map[string]NotifyAdapter, len(adapters))
	maps.Copy(cloned, adapters)
	return &NotifyTool{
		channels:    channels,
		policy:      policy,
		adapters:    cloned,
		credentials: credentials,
		deliveries:  deliveries,
		lookupEnv:   os.LookupEnv,
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

// Execute 执行一次推送（契约 §5.5 的判定顺序）：① 判模式 → 解码 → 上限 → 读表 →
// ② 查冻结集合 → ③ 查当前范围 / 状态 → ④ 解析凭据 → 查类型 → 策略判定 → 投递。
// 任一前置失败都在外呼之前返回；`none` 连注册表都不读。
// Execute performs one push in the contract's fixed order: mode, decode, cap, registry,
// frozen set, current scope, credential, type, policy, delivery.
func (t *NotifyTool) Execute(ctx context.Context, env core.ToolEnv, input json.RawMessage) (core.Result, error) {
	scope := notifyScopeOf(env)
	// ① 先判模式：`none` 在任何查表与外呼之前失败（R4.7：不查渠道、不投递）。
	// The none mode fails before the registry read and before any outbound call.
	if scope.mode == core.NotifyModeNone {
		return core.Result{}, &core.NotifyDisabledError{Reason: notifyDisabledNoneReason}
	}

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

	// 可见范围来自 Run 准入时冻结的 ToolEnv，**不**在调用时回读 AGENT.md：
	// Run 内按**具体 UID** 判定（空集合不是「不限制」）；非 Run 调用沿用声明的名字。
	// The visible set is the run-frozen ToolEnv copy, never a live file re-read.
	visible := scope.restrict(channels)

	name := strings.TrimSpace(in.Channel)
	if name == "" {
		return core.Result{}, missingChannelError(visible, scope)
	}
	// ② 冻结集合 / 声明之外：`not_found`，文案说清是授权限制而不是"渠道不存在"。
	// Outside the frozen set or declaration: not_found that names the authorization limit.
	if scope.declared() && !containsChannelName(visible, name) {
		return core.Result{}, scope.deniedError(name, visible)
	}
	channel, ok := findNotifyChannel(visible, name)
	if !ok {
		return core.Result{}, unknownChannelError(name, visible, scope)
	}
	// ③ 当前范围 / 状态：冻结之后再转为他人专属的渠道不再可用（删除与撤销分别由
	// 注册表可见性与凭据解析兜住）。Run 内才有 Agent 身份，非 Run 调用无从判定。
	// A channel that became another agent's exclusive after admission stops working.
	if scope.runBound && !core.Eligible(channel.Scope, channel.OwnerAgentUID, env.AgentUID) {
		return core.Result{}, &notifyNotFoundError{
			Reason: fmt.Sprintf("通知渠道 %s 已不在本 Agent 的可用范围内（可用渠道：%s）",
				channel.Name, availableChannelNames(visible)),
		}
	}

	adapter := t.adapters[channel.Type]
	if adapter == nil {
		return core.Result{}, &notifyTypeUnregisteredError{
			Channel:     channel.Name,
			ChannelType: channel.Type,
			Available:   availableNotifyTypes(t.adapters),
		}
	}

	// ④ 每次发送解析凭据：env 模式读 `${VAR}`，managed 模式读当前 active 版本。
	// Resolve the credential on every send: env reads ${VAR}, managed reads the current
	// active version.
	resolution, err := t.resolveTarget(ctx, channel)
	if err != nil {
		return core.Result{}, err
	}

	if err := t.policy.Check(ctx, core.Action{
		Type:   core.ActionHTTPRequest,
		Target: resolution.Target,
		Agent:  env.AgentName,
	}); err != nil {
		return core.Result{}, redactNotifyPolicyError(err, resolution.Target)
	}

	if err := adapter.Send(ctx, resolution.Target, *in.Content); err != nil {
		return core.Result{}, err
	}
	// 成功投递后写派生记录（R4.8）：只记实际使用的凭据版本与目标 host，不记地址。
	// Record the delivery after a successful send; the address itself is never stored.
	t.recordDelivery(ctx, env, channel, resolution)
	return core.Result{Content: "已推送到通知渠道：" + channel.Name, Retryable: false}, nil
}

// recordDelivery 在 Run 内写 `notify_deliveries`（一次调用至多一条）：没有记录器、
// 非 Run 调用或执行器未注入工具调用 id 时跳过。写入用收尾上下文（发送已完成，取消
// 不该把这条派生账变成写不进去）；失败不改判发送结果——记录器实现负责记日志。
// recordDelivery writes the derived row inside a run; a write failure never changes the
// send result, because the authoritative audit is the tool_invocations row.
func (t *NotifyTool) recordDelivery(ctx context.Context, env core.ToolEnv, channel core.NotifyChannel,
	resolution core.NotifyCredentialResolution,
) {
	if t.deliveries == nil || !env.RunBound || env.ToolInvocationID <= 0 {
		return
	}
	record := core.NotifyDeliveryRecord{
		ToolInvocationID:  env.ToolInvocationID,
		RunID:             env.RunID,
		AgentUID:          env.AgentUID,
		ChannelUID:        channel.ResourceUID,
		ChannelName:       channel.Name,
		CredentialID:      resolution.CredentialID,
		CredentialVersion: resolution.CredentialVersion,
		TargetHost:        notifyTargetHost(resolution.Target),
	}
	detached := context.WithoutCancel(ctx)
	_ = t.deliveries.Record(detached, record)
}

// resolveTarget 解析本次投递的真实地址：env 模式要求精确 `${VAR}` 占位并从环境读出；
// managed 模式经凭据解析器取当前 active 版本（撤销 → 固定文案的 `notify_disabled`）。
// 解析结果只用于判定与投递，**不**进入成功文案与错误文案。
// resolveTarget resolves the outbound address of one channel for this send only.
func (t *NotifyTool) resolveTarget(ctx context.Context, channel core.NotifyChannel) (core.NotifyCredentialResolution, error) {
	if channel.CredentialMode == core.CredentialModeManaged {
		return t.resolveManagedTarget(ctx, channel)
	}
	return resolveEnvNotifyTarget(channel, t.lookupEnv)
}

// resolveEnvNotifyTarget 是 env 模式地址解析的唯一实现（工具与系统代投共用）：地址
// 必须是精确 `${VAR}` 占位，值只来自环境，**不**读配置文件、不猜明文地址。
// resolveEnvNotifyTarget is the one env-mode resolver shared by the tool and delivery.
func resolveEnvNotifyTarget(channel core.NotifyChannel, lookupEnv func(string) (string, bool),
) (core.NotifyCredentialResolution, error) {
	varName, ok := core.EnvPlaceholderName(channel.URL)
	if !ok {
		return core.NotifyCredentialResolution{}, fmt.Errorf(
			"通知渠道 %s 的地址必须是 ${VAR} 占位（凭证不得明文入库）", channel.Name)
	}
	target, ok := lookupEnv(varName)
	if !ok || target == "" {
		return core.NotifyCredentialResolution{}, fmt.Errorf(
			"通知渠道 %s 引用的环境变量 ${%s} 未设置或为空", channel.Name, varName)
	}
	return core.NotifyCredentialResolution{Target: target}, nil
}

// resolveManagedTarget 经凭据解析器取托管渠道的投递地址（R4.8）：撤销是终态，按
// `notify_disabled` 固定文案失败；其余解析失败属实例故障（runtime_error），一律失败关闭。
// resolveManagedTarget fails closed: a revoked credential is notify_disabled, and any
// other resolution failure stays an instance fault.
func (t *NotifyTool) resolveManagedTarget(ctx context.Context, channel core.NotifyChannel) (core.NotifyCredentialResolution, error) {
	if t.credentials == nil {
		return core.NotifyCredentialResolution{}, fmt.Errorf(
			"通知渠道 %s 使用托管凭据，但本实例未装配凭据解析器（装配错误）", channel.Name)
	}
	resolution, err := t.credentials.ResolveNotifyCredential(ctx, channel)
	if err != nil {
		if errors.Is(err, core.ErrCredentialRevoked) {
			return core.NotifyCredentialResolution{}, &core.NotifyDisabledError{
				Reason: fmt.Sprintf("通知渠道 %s 的托管凭据已撤销（notify_disabled）；请管理员重新写入凭据后重试",
					channel.Name),
			}
		}
		return core.NotifyCredentialResolution{}, fmt.Errorf(
			"解析通知渠道 %s 的托管凭据失败: %w", channel.Name, err)
	}
	if resolution.Target == "" {
		return core.NotifyCredentialResolution{}, fmt.Errorf(
			"通知渠道 %s 的托管凭据未给出可用地址（凭据不可用）", channel.Name)
	}
	return resolution, nil
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

// notifyScope 是本次调用可见的渠道范围（R4.7）：Run 内按准入冻结的**具体 UID** 判定，
// 空集合表示「这次准入没有拿到任何渠道」而不是「不限制」；非 Run 调用（CLI / 直调）
// 没有 Run 授权面，沿用 US-8 的声明名语义（空 = 未声明，不限制）。
// notifyScope is the visible channel range of one call: frozen uids inside a run (an
// empty set authorizes nothing), the US-8 name declaration outside one.
type notifyScope struct {
	runBound bool
	mode     core.NotifyMode
	uids     []string
	names    []string
}

// notifyScopeOf 按执行环境取可见范围；Run 内模式与集合来自 `RunEnv` 的冻结副本。
// notifyScopeOf derives the visible range from the execution environment.
func notifyScopeOf(env core.ToolEnv) notifyScope {
	if env.RunBound {
		return notifyScope{runBound: true, mode: env.NotifyMode, uids: env.NotifyChannels}
	}
	return notifyScope{names: env.NotifyChannels}
}

// declared 报告本次调用是否带明确的授权面：Run 内恒为真（空集合也是明确结论）；
// 非 Run 只有声明非空才算声明。
// declared reports whether an explicit authorization surface exists.
func (s notifyScope) declared() bool {
	if s.runBound {
		return true
	}
	return len(s.names) > 0
}

// restrict 按可见范围收紧注册表，顺序沿用注册表顺序（不引入第二套排序语义）。
// restrict narrows the registry to the visible range, keeping registry order.
func (s notifyScope) restrict(channels []core.NotifyChannel) []core.NotifyChannel {
	if !s.declared() {
		return channels
	}
	visible := make([]core.NotifyChannel, 0, len(channels))
	for _, channel := range channels {
		if s.runBound {
			if slices.Contains(s.uids, string(channel.ResourceUID)) {
				visible = append(visible, channel)
			}
			continue
		}
		if slices.Contains(s.names, channel.Name) {
			visible = append(visible, channel)
		}
	}
	return visible
}

// emptyNote 说明「可见集合为空」的原因：Run 内是冻结集合为空或均已不可用，
// 非 Run 时区分「声明了但一个都没登记」与「注册表为空」。
// emptyNote explains why the visible set is empty.
func (s notifyScope) emptyNote() string {
	if s.runBound {
		return "（本 Run 冻结的通知渠道集合为空或均不可用）"
	}
	if len(s.names) > 0 {
		return "（notify_channels 声明的渠道均未登记）"
	}
	return "（注册表为空）"
}

// deniedError 构造「渠道不在授权面内」的失败：锚到 fs.ErrNotExist（模型可据可用名
// 自我纠正），但文案必须说清是**授权限制**，不能伪装成"渠道不存在"。
// deniedError names the authorization limit as the reason, never pretending the channel
// does not exist.
func (s notifyScope) deniedError(name string, visible []core.NotifyChannel) error {
	available := availableChannelNames(visible)
	if len(visible) == 0 {
		available = s.emptyNote()
	}
	if s.runBound && s.mode == core.NotifyModeAllEligible {
		return &notifyNotFoundError{
			Reason: fmt.Sprintf("通知渠道 %s 不在本 Run 冻结的通知渠道集合内（可用渠道：%s）",
				name, available),
		}
	}
	return &notifyNotFoundError{
		Reason: fmt.Sprintf("通知渠道 %s 不在本 Agent 的可用通知渠道声明（notify_channels）中（可用渠道：%s）",
			name, available),
	}
}

// availableChannelNamesFor 渲染诊断用的可用名（空集合时用范围说明代替空表）。
// availableChannelNamesFor renders the available names, explaining an empty set.
func availableChannelNamesFor(channels []core.NotifyChannel, scope notifyScope) string {
	if len(channels) == 0 {
		return scope.emptyNote()
	}
	return availableChannelNames(channels)
}

// containsChannelName 报告可见集合里是否有这个渠道名。
// containsChannelName reports whether the visible set carries the name.
func containsChannelName(channels []core.NotifyChannel, name string) bool {
	_, ok := findNotifyChannel(channels, name)
	return ok
}

// missingChannelError 构造「未指定渠道」的 not_found 失败（无默认渠道）。
// missingChannelError builds the not_found failure for a missing channel.
func missingChannelError(channels []core.NotifyChannel, scope notifyScope) error {
	return &notifyNotFoundError{
		Reason: fmt.Sprintf("未指定通知渠道（可用渠道：%s）", availableChannelNamesFor(channels, scope)),
	}
}

// unknownChannelError 构造「渠道名查无」的 not_found 失败，文案点名请求名与可用名。
// unknownChannelError builds the not_found failure for an unknown channel name.
func unknownChannelError(name string, channels []core.NotifyChannel, scope notifyScope) error {
	return &notifyNotFoundError{
		Reason: fmt.Sprintf("通知渠道 %s 不存在（可用渠道：%s）", name, availableChannelNamesFor(channels, scope)),
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

// notifyTargetHost 取投递目标的 host（不含端口 / userinfo / path / query），写进
// `notify_deliveries.target_host`；解析不出返回空串（schema 口径：解析不出 host 时为
// 空串），**不**复用展示占位符。
// notifyTargetHost returns the bare host recorded in notify_deliveries; an unparsable
// target yields the empty string as the schema specifies.
func notifyTargetHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	return parsed.Hostname()
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
