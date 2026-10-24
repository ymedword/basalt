package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeNotifyChannels 记调用次数（判"发送前读一次"与"不缓存"）。
// fakeNotifyChannels counts reads and returns the configured rows or error.
type fakeNotifyChannels struct {
	mu    sync.Mutex
	rows  []core.NotifyChannel
	err   error
	calls int
}

// ListNotifyChannels 返回预置行的克隆（调用方改不了夹具本体）。
// ListNotifyChannels returns a clone of the configured rows.
func (f *fakeNotifyChannels) ListNotifyChannels(context.Context) ([]core.NotifyChannel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return slices.Clone(f.rows), f.err
}

// readCount 是注册表读取次数。
// readCount returns how many times the registry was read.
func (f *fakeNotifyChannels) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// notifySend 是一次发送的判卷形态：解析后的真实地址 + 原文内容。
// notifySend is the assertion shape of one send: the resolved URL and verbatim content.
type notifySend struct {
	URL     string
	Content string
}

// recordingNotifyAdapter 记录发送；err 非 nil 时按失败返回。
// onSend 在记录前调用（R2 用它捕获"发送当刻 policy 已被判定过"）。
// recordingNotifyAdapter records sends; onSend runs before the record so R2 can observe
// the policy judgement at the very moment of sending.
type recordingNotifyAdapter struct {
	mu     sync.Mutex
	sent   []notifySend
	err    error
	onSend func()
}

// Send 记录一次发送（先跑 onSend 钩子）。
// Send records one send after running the onSend hook.
func (a *recordingNotifyAdapter) Send(_ context.Context, rawURL, content string) error {
	if a.onSend != nil {
		a.onSend()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, notifySend{URL: rawURL, Content: content})
	return a.err
}

// count 是发送次数（"判定失败即零外呼"的判据）。
// count returns the number of sends.
func (a *recordingNotifyAdapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sent)
}

// sends 返回发送记录的克隆（顺序即调用顺序）。
// sends returns a clone of the send records in call order.
func (a *recordingNotifyAdapter) sends() []notifySend {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sent)
}

// blockingNotifyAdapter 在 Send 里 close(entered) 后阻塞，直到 release 关闭或 ctx 结束
// （R12 用它在"发送进行中"观察审计行；所有等待都有界）。
// blockingNotifyAdapter blocks inside Send until released or canceled; every wait is
// bounded.
type blockingNotifyAdapter struct {
	entered chan struct{}
	release chan struct{}
}

// Send 宣告已进入发送并等待放行。
// Send signals entry and waits for release or cancellation.
func (a *blockingNotifyAdapter) Send(ctx context.Context, _, _ string) error {
	close(a.entered)
	select {
	case <-a.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// notifyPolicySpy 在拒绝时把完整 target 嵌进段 2（与真实风险同形），并记 Agent。
// 既有 spyPolicy 只记 Target、不记 Agent，故 R2 另用本夹具。
// notifyPolicySpy embeds the full target in its refusal (the same worst case as a real
// policy) and records the whole action, including the agent.
type notifyPolicySpy struct {
	mu      sync.Mutex
	allowed []string
	judged  []core.Action
	denied  int
}

// Check 判一次并记账（口径与既有 spyPolicy 一致：出网目标按 hostname 匹配）。
// Check judges one action and records it; HTTP targets are matched by hostname.
func (p *notifyPolicySpy) Check(_ context.Context, action core.Action) error {
	subject := action.Target
	if action.Type == core.ActionHTTPRequest {
		if parsed, err := url.Parse(action.Target); err == nil {
			subject = parsed.Hostname()
		}
	}
	p.mu.Lock()
	p.judged = append(p.judged, action)
	refused := !slices.Contains(p.allowed, subject)
	if refused {
		p.denied++
	}
	p.mu.Unlock()
	if refused {
		return &core.SandboxViolationError{
			Rule:     "测试夹具只放行名单内目标",
			Target:   action.Target,
			Guidance: "测试夹具不提供配置键。",
		}
	}
	return nil
}

// targets 返回判定过的目标串（顺序即判定顺序）。
// targets returns the judged targets in order.
func (p *notifyPolicySpy) targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.judged))
	for _, action := range p.judged {
		out = append(out, action.Target)
	}
	return out
}

// types 返回判定过的动作类型。
// types returns the judged action kinds.
func (p *notifyPolicySpy) types() []core.ActionType {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.ActionType, 0, len(p.judged))
	for _, action := range p.judged {
		out = append(out, action.Type)
	}
	return out
}

// agents 返回判定过的 Agent 名。
// agents returns the judged agent names.
func (p *notifyPolicySpy) agents() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.judged))
	for _, action := range p.judged {
		out = append(out, action.Agent)
	}
	return out
}

// denials 是拒绝次数。
// denials returns the refusal count.
func (p *notifyPolicySpy) denials() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.denied
}

// judgmentCount 是判定次数（R2 在发送当刻断言 == 1，钉住"判定先于发送"）。
// judgmentCount returns the number of judgements.
func (p *notifyPolicySpy) judgmentCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.judged)
}

// newTestNotifyTool 注入固定的环境查找函数（生产缺省 os.LookupEnv）；托管凭据解析面
// 留空——需要它的用例走 `newTestNotifyToolWithCredentials`。
// newTestNotifyTool injects a fixed env lookup and leaves the managed credential
// resolver nil; tests that need one use newTestNotifyToolWithCredentials.
func newTestNotifyTool(channels core.NotifyChannelReader, policy core.Policy,
	adapters map[string]NotifyAdapter, env map[string]string,
) *NotifyTool {
	return newTestNotifyToolWithCredentials(channels, policy, adapters, env, nil)
}

// newTestNotifyToolWithCredentials 同 `newTestNotifyTool`，但带上托管凭据解析面。
// newTestNotifyToolWithCredentials also wires the managed credential resolver.
func newTestNotifyToolWithCredentials(channels core.NotifyChannelReader, policy core.Policy,
	adapters map[string]NotifyAdapter, env map[string]string,
	credentials NotifyCredentialResolver,
) *NotifyTool {
	t := NewNotifyTool(channels, policy, adapters, credentials, nil)
	t.lookupEnv = func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
	return t
}

// TestNotifyTool_InputSchemaRequiresContentOnly —— N1（约束 1）：工具名固定 `notify`；
// schema 的必填集**恰好** `["content"]`——`channel` 可选是契约 §5.5 的既有口径，
// 运行期缺失归 `not_found`（带可用渠道名的可读失败），不在 schema 层拦截。
// N1: the name is fixed and the schema requires exactly content; channel stays optional.
func TestNotifyTool_InputSchemaRequiresContentOnly(t *testing.T) {
	tool := newTestNotifyTool(&fakeNotifyChannels{}, &notifyPolicySpy{}, nil, nil)

	require.Equal(t, "notify", tool.Name())
	require.NotEmpty(t, tool.Description())

	var schema struct {
		Type       string   `json:"type"`
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tool.InputSchema(), &schema))
	require.Equal(t, "object", schema.Type)
	require.Equal(t, []string{"content"}, schema.Required, "必填集恰好 content")
	require.Equal(t, "string", schema.Properties["content"].Type)
	require.Equal(t, "string", schema.Properties["channel"].Type, "channel 可选但必须声明")
}

// TestNotifyTool_DispatchesResolvedContentToAdapter —— N17（约束 7 / 22）：成功路径把
// **解析后的真实地址**与**原文内容**交给适配器，返回固定成功文案且不可重试。
// N17: the resolved URL and verbatim content reach the adapter; the fixed success text
// comes back with Retryable false.
func TestNotifyTool_DispatchesResolvedContentToAdapter(t *testing.T) {
	const hook = "https://hooks.example.com/open-apis/bot/v2/hook/secret-token"
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": hook})

	res, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"每日摘要\n第二行"}`))
	require.NoError(t, err)
	require.Equal(t, "已推送到通知渠道：ops", res.Content)
	require.False(t, res.Retryable, "推送不承诺可重试")

	sent := adapter.sends()
	require.Len(t, sent, 1)
	require.Equal(t, hook, sent[0].URL, "适配器拿到的是解析后的真实地址")
	require.Equal(t, "每日摘要\n第二行", sent[0].Content, "内容原样透传")
}

// TestNotifyTool_ExecutorAuditsBeforeAdapterSend —— N19（**R12**，约束 20 / 21）：
// 适配器阻塞在发送中时，审计行已存在且为 `pending`——先写审计再外呼；返回后同一行
// 回填成功。逆改点：适配器绕过执行器自己发送 → 阻塞期间审计行数为 0；执行后再补
// 第二行 → 终态行数 > 1。
// N19 (R12): while the adapter is blocked mid-send, the audit row already exists and is
// pending; the same row is finished afterwards.
func TestNotifyTool_ExecutorAuditsBeforeAdapterSend(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	adapter := &blockingNotifyAdapter{entered: entered, release: release}
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	audit := &recordingAudit{}
	reg := NewRegistry(slog.New(slog.DiscardHandler))
	require.NoError(t, reg.Register(tool))
	exec := core.NewToolExecutor(reg, audit, slog.New(slog.DiscardHandler))
	done := make(chan core.Result, 1)
	go func() {
		done <- exec.Execute(context.Background(),
			core.ToolEnv{RunID: "run-1", SessionID: "ses-1", Timeout: 5 * time.Second},
			[]string{"notify"},
			core.ToolCall{Name: "notify", Arguments: `{"channel":"ops","content":"每日摘要"}`})
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("适配器未在预算内收到发送")
	}
	require.Equal(t, 1, audit.count(), "执行期间审计行已存在")
	require.Equal(t, core.LLMCallPending, audit.lastStatus(), "必须是 pending（fail-closed 的可见形态）")

	unblock()
	select {
	case res := <-done:
		require.Equal(t, "已推送到通知渠道：ops", res.Content)
	case <-time.After(5 * time.Second):
		t.Fatal("工具调用未在预算内返回")
	}
	require.Equal(t, 1, audit.count(), "一次调用一行")
	require.Equal(t, core.LLMCallSuccess, audit.lastStatus())
}

// TestNotifyTool_MissingChannelIsNotFoundWithAvailableNames —— N2（约束 2 / R3）：
// 渠道缺失一律 `not_found`（`Unwrap → fs.ErrNotExist`），文案含「未指定」与可用渠道名；
// 空注册表明确写「注册表为空」。无默认渠道——判定失败即零发送。
// N2: a missing channel is not_found with the available names; an empty registry says so.
func TestNotifyTool_MissingChannelIsNotFoundWithAvailableNames(t *testing.T) {
	t.Run("非空注册表列出可用渠道名", func(t *testing.T) {
		channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
			{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
			{Name: "team", Type: "feishu", URL: "${TEAM_HOOK}"},
		}}
		adapter := &recordingNotifyAdapter{}
		policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
		tool := newTestNotifyTool(channels, policy,
			map[string]NotifyAdapter{"webhook": adapter, "feishu": adapter}, map[string]string{})

		_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
			json.RawMessage(`{"content":"每日摘要"}`))
		require.ErrorIs(t, err, fs.ErrNotExist, "渠道类失败归 not_found")
		require.Contains(t, err.Error(), "未指定")
		require.Contains(t, err.Error(), "ops")
		require.Contains(t, err.Error(), "team")
		require.Equal(t, 1, channels.readCount(), "列可用名需要读一次注册表")
		require.Equal(t, 0, policy.judgmentCount(), "未走到判定")
		require.Equal(t, 0, adapter.count(), "无默认渠道——判定失败即零发送")
	})
	t.Run("空注册表说明注册表为空", func(t *testing.T) {
		channels := &fakeNotifyChannels{}
		adapter := &recordingNotifyAdapter{}
		tool := newTestNotifyTool(channels, &notifyPolicySpy{},
			map[string]NotifyAdapter{"webhook": adapter}, map[string]string{})

		_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
			json.RawMessage(`{"content":"每日摘要"}`))
		require.ErrorIs(t, err, fs.ErrNotExist)
		require.Contains(t, err.Error(), "未指定")
		require.Contains(t, err.Error(), "注册表为空")
		require.Equal(t, 0, adapter.count())
	})
}

// TestNotifyTool_UnknownChannelIsNotFoundWithAvailableNames —— N3（约束 2 / R3）：
// 渠道名查无同样是 `not_found`，文案同时点名请求名与可用名，便于模型自我纠正。
// N3: an unknown channel is not_found and names both the request and the available set.
func TestNotifyTool_UnknownChannelIsNotFoundWithAvailableNames(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
		{Name: "team", Type: "feishu", URL: "${TEAM_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy,
		map[string]NotifyAdapter{"webhook": adapter, "feishu": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"prod","content":"每日摘要"}`))
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), "prod", "点名请求的渠道名")
	require.Contains(t, err.Error(), "ops")
	require.Contains(t, err.Error(), "team")
	require.Equal(t, 0, policy.judgmentCount())
	require.Equal(t, 0, adapter.count())
}

// TestNotifyTool_DeclaredAllowlistGatesChannels —— notify_channels 非空 = 白名单：
// 声明内的渠道可发送；声明外的渠道即使已登记也按 not_found 拒绝，文案点明是**声明
// 限制**而不是渠道不存在，且零判定零外呼。
// A non-empty declaration is an allowlist: declared channels send, everything else is
// refused as a declaration limit (not_found) before policy judgement.
func TestNotifyTool_DeclaredAllowlistGatesChannels(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
		{Name: "team", Type: "webhook", URL: "${TEAM_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{
			"OPS_HOOK":  "https://hooks.example.com/T",
			"TEAM_HOOK": "https://hooks.example.com/U",
		})
	env := core.ToolEnv{AgentName: "digest", NotifyChannels: []string{"ops"}}

	res, err := tool.Execute(context.Background(), env,
		json.RawMessage(`{"channel":"ops","content":"日报"}`))
	require.NoError(t, err)
	require.Equal(t, "已推送到通知渠道：ops", res.Content)
	require.Equal(t, 1, adapter.count(), "声明内的渠道正常发送")

	_, err = tool.Execute(context.Background(), env,
		json.RawMessage(`{"channel":"team","content":"日报"}`))
	require.ErrorIs(t, err, fs.ErrNotExist, "声明外的渠道按 not_found 拒绝")
	require.Contains(t, err.Error(), "notify_channels", "文案点明声明限制")
	require.NotContains(t, err.Error(), "不存在", "不得伪装成渠道不存在")
	require.Contains(t, err.Error(), "ops", "可用名只列声明内的渠道")
	require.Equal(t, 1, adapter.count(), "拒绝时零外呼")
	require.Equal(t, 1, policy.judgmentCount(), "声明拒绝发生在策略判定之前")
}

// TestNotifyTool_EmptyDeclarationKeepsBackwardCompatibility —— 未声明或显式空列表
// = 不限制：既有 Agent 不声明也能使用任意已登记渠道（行为与引入白名单前一致）。
// An absent or empty declaration stays unrestricted for existing agents.
func TestNotifyTool_EmptyDeclarationKeepsBackwardCompatibility(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "team", Type: "webhook", URL: "${TEAM_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"TEAM_HOOK": "https://hooks.example.com/U"})

	for _, declared := range [][]string{nil, {}} {
		res, err := tool.Execute(context.Background(),
			core.ToolEnv{AgentName: "digest", NotifyChannels: declared},
			json.RawMessage(`{"channel":"team","content":"日报"}`))
		require.NoError(t, err, "空声明不限制：%v", declared)
		require.Equal(t, "已推送到通知渠道：team", res.Content)
	}
	require.Equal(t, 2, adapter.count())
}

// TestNotifyTool_DeclaredButUnregisteredExplainsEmptyIntersection —— 声明非空但一个
// 渠道都没登记时，失败文案说「声明的渠道均未登记」，而不是误导性的「注册表为空」。
// When no declared channel is registered, the failure names the empty intersection.
func TestNotifyTool_DeclaredButUnregisteredExplainsEmptyIntersection(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{},
		map[string]NotifyAdapter{"webhook": adapter}, nil)

	_, err := tool.Execute(context.Background(),
		core.ToolEnv{AgentName: "digest", NotifyChannels: []string{"ghost"}},
		json.RawMessage(`{"content":"日报"}`))
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), "notify_channels 声明的渠道均未登记")
	require.Equal(t, 0, adapter.count())
}

// TestNotifyTool_BlankChannelTreatedAsMissing —— N4（约束 2 / R3）：纯空白视同缺失，
// 走「未指定」路径而不是把空白当一个渠道名去查。
// N4: a whitespace-only channel is treated as missing, not as a channel name.
func TestNotifyTool_BlankChannelTreatedAsMissing(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"   ","content":"每日摘要"}`))
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), "未指定", "空白视同缺失")
	require.Contains(t, err.Error(), "ops")
	require.Equal(t, 0, adapter.count())
}

// TestNotifyTool_ContentOverLimitFailsBeforeLookup —— N5（**R5**，约束 3）：
// 超限是 `invalid_input`，且在**读注册表之前**判定——注册表读取次数为 0。
// N5 (R5): over-limit content is invalid_input before the registry is read.
func TestNotifyTool_ContentOverLimitFailsBeforeLookup(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	// 冻结字面量断言：上限写死 20 × 1024，测试不得随实现常量一起漂移。
	require.Equal(t, 20*1024, maxNotifyContentBytes)
	over := strings.Repeat("a", maxNotifyContentBytes+1)
	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"`+over+`"}`))
	require.True(t, core.IsInvalidInput(err))
	require.Equal(t, 0, channels.readCount(), "超限在查表之前判")
	require.Empty(t, policy.targets())
	require.Equal(t, 0, adapter.count())
}

// TestNotifyTool_ContentAtLimitIsAccepted —— N6（**R5**，约束 3）：恰好 20 KiB 放行，
// 发送一次、内容原样（边界值是「不得超过」，不是「必须小于」）。
// N6 (R5): content of exactly the cap is accepted and sent verbatim.
func TestNotifyTool_ContentAtLimitIsAccepted(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	atLimit := strings.Repeat("a", maxNotifyContentBytes)
	res, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"`+atLimit+`"}`))
	require.NoError(t, err)
	require.Equal(t, "已推送到通知渠道：ops", res.Content)
	sent := adapter.sends()
	require.Len(t, sent, 1)
	require.Equal(t, atLimit, sent[0].Content)
}

// TestNotifyTool_ContentLimitCountsUTF8Bytes —— N7（**R5**，约束 3）：上限按 UTF-8
// 字节计而非字符数——20481 字节的中文串超限。
// N7 (R5): the cap counts UTF-8 bytes, not characters.
func TestNotifyTool_ContentLimitCountsUTF8Bytes(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": &recordingNotifyAdapter{}},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	over := strings.Repeat("中", maxNotifyContentBytes/3+1)
	require.Greater(t, len(over), maxNotifyContentBytes, "中文字符串按字节计超限")
	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"`+over+`"}`))
	require.True(t, core.IsInvalidInput(err), "按字节计而不是按字符数")
	require.Equal(t, 0, channels.readCount())
}

// TestNotifyTool_RegistryReadFailureIsNotChannelMissing —— N15（约束 4 / 注 2）：
// 注册表读失败是实例故障（`runtime_error`），不得被当成「渠道不存在」，且零外呼。
// N15: a registry read failure is an instance fault, never a channel-not-found.
func TestNotifyTool_RegistryReadFailureIsNotChannelMissing(t *testing.T) {
	channels := &fakeNotifyChannels{err: errors.New("database is locked")}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"每日摘要"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "读取通知渠道注册表失败")
	require.Contains(t, err.Error(), "database is locked", "原因链经 %w 保留")
	require.NotErrorIs(t, err, fs.ErrNotExist, "不是渠道缺失")
	require.False(t, core.IsInvalidInput(err))
	require.False(t, core.IsSandboxViolation(err))
	require.False(t, core.IsExternalError(err))
	require.Equal(t, 1, channels.readCount())
	require.Equal(t, 0, policy.judgmentCount())
	require.Equal(t, 0, adapter.count())
}

// TestNotifyTool_ReadsRegistryPerSend —— N16（约束 4）：每次发送读一次注册表、不缓存；
// 两次发送 → 两次读取。逆改点：构造时读一次并缓存 → 第二次读数停留在 1。
// N16: every send reads the registry once; two sends read twice.
func TestNotifyTool_ReadsRegistryPerSend(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	for range 2 {
		_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
			json.RawMessage(`{"channel":"ops","content":"每日摘要"}`))
		require.NoError(t, err)
	}
	require.Equal(t, 2, channels.readCount(), "每次发送读一次、不缓存")
	require.Equal(t, 2, adapter.count())
}

// TestNotifyTool_RejectsNonStringFields —— N18（约束 1）：字段类型不符与 content 为
// null 都是 `invalid_input`，在解码点即失败：零注册表读取、零发送。
// N18: non-string fields and null content are invalid_input at the decode point.
func TestNotifyTool_RejectsNonStringFields(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"channel 非字符串", `{"channel":123,"content":"每日摘要"}`},
		{"content 非字符串", `{"channel":"ops","content":123}`},
		{"content 为 null", `{"channel":"ops","content":null}`},
		{"入参不是对象", `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
				{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
			}}
			adapter := &recordingNotifyAdapter{}
			tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
				map[string]NotifyAdapter{"webhook": adapter},
				map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

			_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
				json.RawMessage(tc.input))
			require.True(t, core.IsInvalidInput(err), "字段类型问题归 invalid_input")
			require.Equal(t, 0, channels.readCount(), "解码失败不读注册表")
			require.Equal(t, 0, adapter.count())
		})
	}
}

// TestNotifyTool_NullChannelTreatedAsMissing —— N18 补充（人工 review ②，2026-10-18）：
// JSON `null` 解到 string 字段不报错、保持空串，因此 `channel:null` **不是**「非字符串
// 字段在解码点拒绝」，而是按缺失渠道走 not_found（列出可用名），零发送。N18 的
// 「channel 非字符串」子例只覆盖数字；本用例把 null 的既有行为钉住，防止被误述成
// 解码期类型错误。零外呼与零判定同时成立。
// A null channel survives decoding as "" (encoding/json ignores null for string fields),
// so it is treated as a missing channel (not_found), not a decode-time type error.
func TestNotifyTool_NullChannelTreatedAsMissing(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy,
		map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":null,"content":"每日摘要"}`))
	require.ErrorIs(t, err, fs.ErrNotExist, "null 渠道按缺失处理，归 not_found")
	require.Contains(t, err.Error(), "未指定")
	require.Contains(t, err.Error(), "ops", "错误文本列出可用渠道名")
	require.Equal(t, 1, channels.readCount(), "缺失判定要读注册表列可用名")
	require.Equal(t, 0, policy.judgmentCount(), "未走到判定")
	require.Equal(t, 0, adapter.count(), "零发送")
}

// TestNotifyTool_EmptyContentIsSent —— N21（约束 1 / §6.2 第 2 条，人工 review ①
// 裁决）：空串 `""` 放行——工具层不设内容下限，接收端可自行拒绝。逆改点：加空串
// 拒绝 → 本用例红。
// N21: the empty string is sent; the tool imposes no lower bound on content.
func TestNotifyTool_EmptyContentIsSent(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	res, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":""}`))
	require.NoError(t, err)
	require.Equal(t, "已推送到通知渠道：ops", res.Content)
	sent := adapter.sends()
	require.Len(t, sent, 1, "空串也发送一次")
	require.Equal(t, "", sent[0].Content)
}

// notifyAudit 记录执行器回填的终态结果（N20 判 error_kind 与失败文本回填）。
// notifyAudit records terminal results so N20 can assert the error kind and the
// failure text fed back to the model.
type notifyAudit struct {
	mu   sync.Mutex
	rows []core.ToolInvocationResult
}

// BeginInvocation 预写一行 pending。
// BeginInvocation appends a pending row.
func (a *notifyAudit) BeginInvocation(_ context.Context, _ core.ToolInvocation) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, core.ToolInvocationResult{Status: core.LLMCallPending})
	return int64(len(a.rows)), nil
}

// FinishInvocation 把同一行回填为终态。
// FinishInvocation fills the same row with its terminal result.
func (a *notifyAudit) FinishInvocation(_ context.Context, id int64, res core.ToolInvocationResult) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if id < 1 || int(id) > len(a.rows) {
		return fmt.Errorf("tool invocation row %d not found", id)
	}
	a.rows[id-1] = res
	return nil
}

// last 返回最后一行的终态。
// last returns the terminal result of the last row.
func (a *notifyAudit) last() core.ToolInvocationResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.rows) == 0 {
		return core.ToolInvocationResult{}
	}
	return a.rows[len(a.rows)-1]
}

// TestNotifyTool_PlaintextURLFailsWithoutEcho —— N8（**R4**，约束 5 / 12）：
// 明文地址既不是模型输入问题、也还没到外呼——按实例配置问题失败，且**不回显地址**。
// N8 (R4): a plaintext URL fails closed without echoing the address.
func TestNotifyTool_PlaintextURLFailsWithoutEcho(t *testing.T) {
	const secret = "https://hooks.example.com/T-secret"
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{Name: "ops", Type: "webhook", URL: secret}}}
	policy := &spyPolicy{}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter}, nil)

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"每日摘要"}`))
	require.Error(t, err)
	require.False(t, core.IsInvalidInput(err), "不是模型输入问题")
	require.False(t, core.IsSandboxViolation(err))
	require.False(t, core.IsExternalError(err))
	require.NotContains(t, err.Error(), "T-secret", "明文地址不得回显")
	require.Contains(t, err.Error(), "ops")
	require.Contains(t, err.Error(), "${VAR}")
	require.Empty(t, policy.targets(), "形态非法：连 policy 都不调")
	require.Equal(t, 0, adapter.count())
}

// TestNotifyTool_NearMissPlaceholderFailsWithoutEcho —— N9（**R4**，约束 5）：
// 半截占位形态一律拒绝且不回显原值——前缀匹配是把凭证放进库的口子。
// N9 (R4): every near-miss placeholder form is rejected without echoing the value.
func TestNotifyTool_NearMissPlaceholderFailsWithoutEcho(t *testing.T) {
	for _, raw := range []string{"${A}${B}", "${A}x", "$A", "${}", "${1A}"} {
		t.Run(raw, func(t *testing.T) {
			channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{Name: "ops", Type: "webhook", URL: raw}}}
			policy := &spyPolicy{}
			adapter := &recordingNotifyAdapter{}
			tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter}, nil)

			_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
				json.RawMessage(`{"channel":"ops","content":"每日摘要"}`))
			require.Error(t, err)
			require.False(t, core.IsInvalidInput(err), "不是模型输入问题")
			require.False(t, core.IsSandboxViolation(err))
			require.False(t, core.IsExternalError(err))
			require.NotContains(t, err.Error(), raw, "不回显地址原值")
			require.Contains(t, err.Error(), "ops")
			require.Contains(t, err.Error(), "${VAR}")
			require.Empty(t, policy.targets(), "形态非法：连 policy 都不调")
			require.Equal(t, 0, adapter.count())
		})
	}
}

// TestNotifyTool_UnsetEnvFailsClosed —— N10（**R4**，约束 6）：环境变量未设置时
// 点名变量名并失败关闭——不得把 `${VAR}` 字面量当 URL 发出去。
// N10 (R4): an unset variable fails closed and names the variable.
func TestNotifyTool_UnsetEnvFailsClosed(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"}}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{})
	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"每日摘要"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "${OPS_HOOK}", "点名环境变量")
	require.Equal(t, 0, policy.judgmentCount())
	require.Equal(t, 0, adapter.count(), "失败关闭：零外呼")
}

// TestNotifyTool_EmptyEnvFailsClosed —— N11（**R4**，约束 6）：变量已设置但值为空串
// 与未设置同判——空值不得静默变成一次无目标外呼。
// N11 (R4): an empty value is treated like an unset variable.
func TestNotifyTool_EmptyEnvFailsClosed(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"}}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": ""})
	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"每日摘要"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "${OPS_HOOK}", "点名环境变量")
	require.Equal(t, 0, policy.judgmentCount())
	require.Equal(t, 0, adapter.count(), "失败关闭：零外呼")
}

// TestNotifyTool_PolicyDenialRedactsCredentialURL —— N12（**R1**，约束 11 / 12）：
// 沙箱拒绝保留 Rule / Guidance 原文，段 2 只留 `scheme://host`——端口、userinfo、
// path、query 全部不得进入模型可见文本。
// N12 (R1): policy refusal keeps rule and guidance verbatim but redacts the target.
func TestNotifyTool_PolicyDenialRedactsCredentialURL(t *testing.T) {
	const token = "hook-token-must-not-leak"
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	// spyPolicy 的拒绝把完整 action.Target 嵌进段 2——模拟"未来某个 Policy 实现"的
	// 最坏形态，钉住的是本工具的重写逻辑，而不是现行 WhitelistPolicy 的文案。
	// 地址带端口与 userinfo：钉住"只保留 scheme + hostname"的完整口径。
	policy := &spyPolicy{} // 空放行名单
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://user:pass@hooks.example.com:8443/" + token + "?sign=" + token})

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"每日摘要"}`))
	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err), "拒绝身份必须保留")
	require.NotContains(t, err.Error(), token, "凭证不得出现在模型可见文本")
	require.NotContains(t, err.Error(), "/"+token)
	require.NotContains(t, err.Error(), "user:pass", "userinfo 不进可观测面")
	require.NotContains(t, err.Error(), ":8443", "端口不进可观测面")
	require.Contains(t, err.Error(), "https://hooks.example.com", "段 2 = scheme + host")
	require.Contains(t, err.Error(), "测试夹具只放行名单内目标", "Rule 原文保留")
	require.Contains(t, err.Error(), "测试夹具不提供配置键。", "Guidance 原文保留")
	require.Equal(t, 0, adapter.count(), "判定拒绝后零外呼")
}

// TestNotifyTool_PolicySeesResolvedURLAndAgentBeforeAnySend —— N13（**R2**，约束 9 / 10）：
// 判定对象是解析后的终值、动作类型固定 HTTP、Agent 名显式传导；且发送当刻判定已发生
// 且只发生一次。
// N13 (R2): the policy sees the resolved URL and agent, and judgement precedes the send.
func TestNotifyTool_PolicySeesResolvedURLAndAgentBeforeAnySend(t *testing.T) {
	const resolved = "https://hooks.example.com/services/T1"
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	adapter := &recordingNotifyAdapter{}
	var judgedAtSend int
	adapter.onSend = func() { judgedAtSend = policy.judgmentCount() }
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": resolved})

	res, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"每日摘要"}`))
	require.NoError(t, err)
	require.Equal(t, []string{resolved}, policy.targets(), "判定对象是解析后的终值")
	require.Equal(t, []core.ActionType{core.ActionHTTPRequest}, policy.types(), "动作类型固定为 HTTP 请求")
	require.Equal(t, []string{"digest"}, policy.agents(), "Agent 名显式传导")
	require.Equal(t, 0, policy.denials())
	require.Equal(t, 1, judgedAtSend, "发送当刻判定已发生且只判一次——判定先于发送")
	require.Len(t, adapter.sent, 1)
	require.Equal(t, "已推送到通知渠道：ops", res.Content)
	require.False(t, res.Retryable)
}

// TestNotifyTool_ExecutorRecordsRuntimeErrorForUnsetEnv —— N20（注 2 / 约束 21）：
// 环境缺失是实例配置问题，执行器归 `runtime_error`；失败文本回填为工具结果，
// 模型据此知道该找运维补哪个变量。
// N20: an unset variable is runtime_error in the audit row and the text is fed back.
func TestNotifyTool_ExecutorRecordsRuntimeErrorForUnsetEnv(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	tool := newTestNotifyTool(channels, &notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": adapter}, map[string]string{})

	audit := &notifyAudit{}
	reg := NewRegistry(slog.New(slog.DiscardHandler))
	require.NoError(t, reg.Register(tool))
	exec := core.NewToolExecutor(reg, audit, slog.New(slog.DiscardHandler))
	res := exec.Execute(context.Background(),
		core.ToolEnv{RunID: "run-1", SessionID: "ses-1", Timeout: 5 * time.Second},
		[]string{"notify"},
		core.ToolCall{Name: "notify", Arguments: `{"channel":"ops","content":"每日摘要"}`})

	require.Contains(t, res.Content, "${OPS_HOOK}", "失败文本回填为工具结果")
	require.False(t, res.Retryable)
	last := audit.last()
	require.Equal(t, core.LLMCallFailed, last.Status)
	require.Equal(t, core.ErrorKindRuntimeError, last.ErrorKind, "环境缺失是实例配置问题")
	require.Contains(t, last.ErrorMessage, "${OPS_HOOK}")
	require.Equal(t, 0, adapter.count(), "失败关闭：零外呼")
}

// TestNotifyTool_UnknownTypeFailsWithoutPolicyOrSend —— N14（约束 8 / R10）：渠道类型
// 未装配时返回专用错误（执行器据类型归 runtime_error），且**不查环境、不调策略判定、
// 不发送**——类型查表在占位校验与环境解析之前；顺序颠倒会让一个本实例根本发不出的
// 渠道类型先触达环境 / 策略 / 网络。
// N14 (R10): an unregistered channel type fails with the dedicated error before any env
// lookup, policy judgement or send.
func TestNotifyTool_UnknownTypeFailsWithoutPolicyOrSend(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "dingtalk", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy,
		map[string]NotifyAdapter{"webhook": adapter}, nil)

	lookups := 0
	tool.lookupEnv = func(string) (string, bool) {
		lookups++
		return "https://hooks.example.com/hook", true
	}

	_, err := tool.Execute(context.Background(), core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"巡检摘要"}`))

	var typeErr *notifyTypeUnregisteredError
	require.ErrorAs(t, err, &typeErr, "未知类型走专用错误（执行器据类型归 runtime_error）")
	require.Contains(t, err.Error(), "dingtalk", "错误文案点名未装配的类型")
	require.Contains(t, err.Error(), "ops", "错误文案点名渠道")
	require.Equal(t, 0, lookups, "类型未装配不得触达环境")
	require.Equal(t, 0, policy.judgmentCount(), "类型未装配不得调策略判定")
	require.Equal(t, 0, adapter.count(), "类型未装配不得发送")
	require.Equal(t, 1, channels.readCount(), "查表本身仍读一次注册表")
}

// mustChannelUID 造一个合法的资源 UID 供冻结集合使用（生成失败即测试环境故障）。
// mustChannelUID mints a valid resource uid for the frozen set.
func mustChannelUID(t *testing.T) core.ResourceUID {
	t.Helper()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	return uid
}

// fakeNotifyCredentials 记录托管凭据解析次数并按预置结果返回；地址与错误二选一。
// fakeNotifyCredentials records resolutions and returns the configured address or error.
type fakeNotifyCredentials struct {
	mu      sync.Mutex
	addr    string
	credID  string
	version int
	err     error
	calls   int
}

// ResolveNotifyCredential 返回预置地址或错误（每次发送都应被调用一次），并带上预置的
// 凭据身份与实际使用版本号。
// ResolveNotifyCredential returns the configured address or error per call, with the
// configured credential identity and version.
func (f *fakeNotifyCredentials) ResolveNotifyCredential(context.Context, core.NotifyChannel) (core.NotifyCredentialResolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return core.NotifyCredentialResolution{}, f.err
	}
	return core.NotifyCredentialResolution{
		Target:            f.addr,
		CredentialID:      f.credID,
		CredentialVersion: f.version,
	}, nil
}

// callCount 是解析次数（钉住"每次发送都重新解析、不缓存"）。
// callCount returns how many sends resolved the credential.
func (f *fakeNotifyCredentials) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeNotifyDeliveries 记录工具写出的投递记录，并可注入写入错误。
// fakeNotifyDeliveries records delivery writes and can inject a failure.
type fakeNotifyDeliveries struct {
	mu      sync.Mutex
	records []core.NotifyDeliveryRecord
	err     error
}

// Record 追加一条投递记录（注入错误时一次都不追加）。
// Record appends one delivery record unless an error is injected.
func (f *fakeNotifyDeliveries) Record(_ context.Context, record core.NotifyDeliveryRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.records = append(f.records, record)
	return nil
}

// rows 返回记录克隆（顺序即调用顺序）。
// rows returns a clone of the recorded rows.
func (f *fakeNotifyDeliveries) rows() []core.NotifyDeliveryRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.records)
}

// TestNotifyTool_RecordsDeliveryInsideRun —— R4.8：Run 内成功投递后写一条
// `notify_deliveries` 派生记录，记实际使用的凭据身份 / 版本与目标 host；env 模式的凭据
// 身份与版本保持零值。地址明文**不得**进入记录。
// Inside a run a successful send writes one delivery row with the credential actually used
// and the bare target host; env mode leaves the credential identity and version zero.
func TestNotifyTool_RecordsDeliveryInsideRun(t *testing.T) {
	agentUID, err := core.NewAgentUID()
	require.NoError(t, err)

	t.Run("managed_records_credential_version_and_host", func(t *testing.T) {
		const hook = "https://hooks.example.com/managed/secret?token=abc"
		channelUID := mustChannelUID(t)
		channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{
			ResourceUID:    channelUID,
			Name:           "ops",
			Type:           "webhook",
			CredentialMode: core.CredentialModeManaged,
			Scope:          core.ScopeShared,
		}}}
		credentials := &fakeNotifyCredentials{addr: hook, credID: "cred_managed01", version: 3}
		deliveries := &fakeNotifyDeliveries{}
		tool := newTestNotifyToolWithCredentials(channels,
			&notifyPolicySpy{allowed: []string{"hooks.example.com"}},
			map[string]NotifyAdapter{"webhook": &recordingNotifyAdapter{}}, nil, credentials)
		tool.deliveries = deliveries

		res, err := tool.Execute(context.Background(), core.ToolEnv{
			RunID:            "webhook-1700000000000-abc123",
			AgentUID:         agentUID,
			AgentName:        "digest",
			RunBound:         true,
			NotifyMode:       core.NotifyModeAllEligible,
			NotifyChannels:   []string{string(channelUID)},
			ToolInvocationID: 42,
		}, json.RawMessage(`{"channel":"ops","content":"日报"}`))
		require.NoError(t, err)
		require.Equal(t, "已推送到通知渠道：ops", res.Content)

		rows := deliveries.rows()
		require.Len(t, rows, 1, "一次调用至多一条投递记录")
		require.Equal(t, int64(42), rows[0].ToolInvocationID)
		require.Equal(t, "webhook-1700000000000-abc123", rows[0].RunID)
		require.Equal(t, agentUID, rows[0].AgentUID)
		require.Equal(t, channelUID, rows[0].ChannelUID)
		require.Equal(t, "ops", rows[0].ChannelName)
		require.Equal(t, "cred_managed01", rows[0].CredentialID)
		require.Equal(t, 3, rows[0].CredentialVersion)
		require.Equal(t, "hooks.example.com", rows[0].TargetHost, "只记 host，不记路径与查询串")
		require.NotContains(t, rows[0].TargetHost, "secret", "记录不得含地址明文")
	})

	t.Run("env_mode_records_empty_credential", func(t *testing.T) {
		channelUID := mustChannelUID(t)
		channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{
			ResourceUID: channelUID, Name: "ops", Type: "webhook", URL: "${OPS_HOOK}",
			Scope: core.ScopeShared,
		}}}
		deliveries := &fakeNotifyDeliveries{}
		tool := newTestNotifyTool(channels,
			&notifyPolicySpy{allowed: []string{"hooks.example.com"}},
			map[string]NotifyAdapter{"webhook": &recordingNotifyAdapter{}},
			map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})
		tool.deliveries = deliveries

		_, err := tool.Execute(context.Background(), core.ToolEnv{
			RunID:            "webhook-1700000000001-def456",
			AgentUID:         agentUID,
			AgentName:        "digest",
			RunBound:         true,
			NotifyMode:       core.NotifyModeAllEligible,
			NotifyChannels:   []string{string(channelUID)},
			ToolInvocationID: 7,
		}, json.RawMessage(`{"channel":"ops","content":"摘要"}`))
		require.NoError(t, err)

		rows := deliveries.rows()
		require.Len(t, rows, 1)
		require.Empty(t, rows[0].CredentialID, "env 模式没有凭据身份")
		require.Zero(t, rows[0].CredentialVersion, "env 模式没有凭据版本")
		require.Equal(t, "hooks.example.com", rows[0].TargetHost)
	})
}

// TestNotifyTool_NoDeliveryRecordWithoutInvocationOrAfterFailure —— 派生记录只在
// 「Run 内 + 执行器已注入调用 id + 发送成功」时写；记录写入失败也不得把已完成的发送
// 改判为失败（模型重试会重复推送）。
// The delivery row is written only for run-bound calls with an audit id after a successful
// send; a recorder failure never turns a completed send into a failure.
func TestNotifyTool_NoDeliveryRecordWithoutInvocationOrAfterFailure(t *testing.T) {
	agentUID, err := core.NewAgentUID()
	require.NoError(t, err)
	channelUID := mustChannelUID(t)
	baseEnv := core.ToolEnv{
		RunID:            "webhook-1700000000002-ghi789",
		AgentUID:         agentUID,
		AgentName:        "digest",
		RunBound:         true,
		NotifyMode:       core.NotifyModeAllEligible,
		NotifyChannels:   []string{string(channelUID)},
		ToolInvocationID: 9,
	}
	newTool := func(t *testing.T, adapter *recordingNotifyAdapter,
		deliveries *fakeNotifyDeliveries,
	) *NotifyTool {
		t.Helper()
		channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{
			ResourceUID: channelUID, Name: "ops", Type: "webhook", URL: "${OPS_HOOK}",
			Scope: core.ScopeShared,
		}}}
		tool := newTestNotifyTool(channels,
			&notifyPolicySpy{allowed: []string{"hooks.example.com"}},
			map[string]NotifyAdapter{"webhook": adapter},
			map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})
		tool.deliveries = deliveries
		return tool
	}

	t.Run("no_audit_id_means_no_record", func(t *testing.T) {
		deliveries := &fakeNotifyDeliveries{}
		env := baseEnv
		env.ToolInvocationID = 0
		_, err := newTool(t, &recordingNotifyAdapter{}, deliveries).Execute(
			context.Background(), env, json.RawMessage(`{"channel":"ops","content":"摘要"}`))
		require.NoError(t, err)
		require.Empty(t, deliveries.rows())
	})

	t.Run("non_run_call_means_no_record", func(t *testing.T) {
		deliveries := &fakeNotifyDeliveries{}
		env := baseEnv
		env.RunBound = false
		env.NotifyChannels = nil // 非 Run 调用按声明名判定；空 = 未声明（不限制）
		_, err := newTool(t, &recordingNotifyAdapter{}, deliveries).Execute(
			context.Background(), env, json.RawMessage(`{"channel":"ops","content":"摘要"}`))
		require.NoError(t, err)
		require.Empty(t, deliveries.rows(), "非 Run 调用没有审计行，不写派生记录")
	})

	t.Run("failed_send_means_no_record", func(t *testing.T) {
		deliveries := &fakeNotifyDeliveries{}
		adapter := &recordingNotifyAdapter{err: errors.New("推送失败")}
		_, err := newTool(t, adapter, deliveries).Execute(
			context.Background(), baseEnv, json.RawMessage(`{"channel":"ops","content":"摘要"}`))
		require.Error(t, err)
		require.Empty(t, deliveries.rows(), "发送失败没有投递")
	})

	t.Run("recorder_failure_keeps_send_success", func(t *testing.T) {
		deliveries := &fakeNotifyDeliveries{err: errors.New("写库失败")}
		adapter := &recordingNotifyAdapter{}
		res, err := newTool(t, adapter, deliveries).Execute(
			context.Background(), baseEnv, json.RawMessage(`{"channel":"ops","content":"摘要"}`))
		require.NoError(t, err, "派生账写失败不得改判已完成的发送")
		require.Equal(t, "已推送到通知渠道：ops", res.Content)
		require.Equal(t, 1, adapter.count())
	})
}

// TestNotifyTool_RunBoundNoneReportsNotifyDisabled —— R4.7 ①：`notify_mode: none`
// 在任何查表与外呼之前按固定文案失败，错误链可被判成 `notify_disabled`。
// R4.7 step one: none mode fails with the fixed text before the registry read and any
// outbound call, and the error chain classifies as notify_disabled.
func TestNotifyTool_RunBoundNoneReportsNotifyDisabled(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})

	_, err := tool.Execute(context.Background(), core.ToolEnv{
		AgentName:  "digest",
		RunBound:   true,
		NotifyMode: core.NotifyModeNone,
	}, json.RawMessage(`{"channel":"ops","content":"日报"}`))

	require.Error(t, err)
	require.True(t, core.IsNotifyDisabled(err), "none 模式归 notify_disabled")
	require.Equal(t, "该 Agent 未启用通知（notify_mode: none）", err.Error(), "固定文案逐字")
	require.Equal(t, 0, channels.readCount(), "none 模式不读注册表")
	require.Equal(t, 0, policy.judgmentCount(), "none 模式不调策略判定")
	require.Equal(t, 0, adapter.count(), "none 模式不投递")
}

// TestNotifyTool_RunBoundFreezesChannelUIDs —— R4.7 ②：Run 内只认准入时冻结的**具体
// UID**；集合外的渠道即使已登记也按 not_found 拒绝，文案点明是冻结集合限制并只列可用名。
// Inside a run only the frozen uids are usable; an out-of-set channel is not_found
// naming the frozen-set limit, before any policy judgement or send.
func TestNotifyTool_RunBoundFreezesChannelUIDs(t *testing.T) {
	opsUID := mustChannelUID(t)
	teamUID := mustChannelUID(t)
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{ResourceUID: opsUID, Name: "ops", Type: "webhook", URL: "${OPS_HOOK}", Scope: core.ScopeShared},
		{ResourceUID: teamUID, Name: "team", Type: "webhook", URL: "${TEAM_HOOK}", Scope: core.ScopeShared},
	}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{
			"OPS_HOOK":  "https://hooks.example.com/T",
			"TEAM_HOOK": "https://hooks.example.com/U",
		})
	env := core.ToolEnv{
		AgentName:      "digest",
		RunBound:       true,
		NotifyMode:     core.NotifyModeAllEligible,
		NotifyChannels: []string{string(opsUID)},
	}

	res, err := tool.Execute(context.Background(), env,
		json.RawMessage(`{"channel":"ops","content":"日报"}`))
	require.NoError(t, err, "冻结集合内的渠道正常发送")
	require.Equal(t, "已推送到通知渠道：ops", res.Content)

	_, err = tool.Execute(context.Background(), env,
		json.RawMessage(`{"channel":"team","content":"日报"}`))
	require.ErrorIs(t, err, fs.ErrNotExist, "集合外的渠道按 not_found 拒绝")
	require.Contains(t, err.Error(), "不在本 Run 冻结的通知渠道集合内", "文案点明冻结集合限制")
	require.NotContains(t, err.Error(), "不存在", "不得伪装成渠道不存在")
	require.Contains(t, err.Error(), "（可用渠道：ops）", "可用名只列集合内的渠道")
	require.Equal(t, 1, adapter.count(), "拒绝时零外呼")
	require.Equal(t, 1, policy.judgmentCount(), "拒绝发生在策略判定之前")
}

// TestNotifyTool_RunBoundEmptyFrozenSetAuthorizesNothing —— R4.7：Run 内空集合**不是**
// 「不限制」，它表示这次准入没有拿到任何渠道；任何发送请求都按 not_found 失败。
// An empty frozen set inside a run authorizes nothing; it never means unrestricted.
func TestNotifyTool_RunBoundEmptyFrozenSetAuthorizesNothing(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{
		{ResourceUID: mustChannelUID(t), Name: "ops", Type: "webhook", URL: "${OPS_HOOK}"},
	}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})
	env := core.ToolEnv{
		AgentName:      "digest",
		RunBound:       true,
		NotifyMode:     core.NotifyModeAllEligible,
		NotifyChannels: nil,
	}

	_, err := tool.Execute(context.Background(), env,
		json.RawMessage(`{"channel":"ops","content":"日报"}`))

	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), "本 Run 冻结的通知渠道集合为空或均不可用")
	require.Equal(t, 0, adapter.count(), "空集合不得放行任何发送")
	require.Equal(t, 0, policy.judgmentCount())
}

// TestNotifyTool_RunBoundExclusiveChannelOfAnotherAgentIsNotFound —— R4.7 ③：冻结之后
// 渠道若变成他人专属，当前 Agent 失去资格（fail-closed，不当成"渠道不存在"以外的话术）。
// A channel that became another agent's exclusive after admission stops working.
func TestNotifyTool_RunBoundExclusiveChannelOfAnotherAgentIsNotFound(t *testing.T) {
	channelUID := mustChannelUID(t)
	otherOwner, err := core.NewAgentUID()
	require.NoError(t, err)
	callerUID, err := core.NewAgentUID()
	require.NoError(t, err)
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{
		ResourceUID:   channelUID,
		Name:          "ops",
		Type:          "webhook",
		URL:           "${OPS_HOOK}",
		Scope:         core.ScopeExclusive,
		OwnerAgentUID: otherOwner,
	}}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter},
		map[string]string{"OPS_HOOK": "https://hooks.example.com/T"})
	env := core.ToolEnv{
		AgentName:      "digest",
		AgentUID:       callerUID,
		RunBound:       true,
		NotifyMode:     core.NotifyModeAllEligible,
		NotifyChannels: []string{string(channelUID)},
	}

	_, err = tool.Execute(context.Background(), env,
		json.RawMessage(`{"channel":"ops","content":"日报"}`))

	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), "已不在本 Agent 的可用范围内")
	require.Equal(t, 0, adapter.count(), "失去资格不得发送")
	require.Equal(t, 0, policy.judgmentCount())
}

// TestNotifyTool_ManagedCredentialResolvedPerSend —— R4.8：managed 渠道每次发送都重新
// 解析当前 active 凭据（不缓存、不回退明文），地址只进适配器不进成功文案。
// Managed channels resolve their current credential on every send; the address reaches
// the adapter only and never the success text.
func TestNotifyTool_ManagedCredentialResolvedPerSend(t *testing.T) {
	const hook = "https://hooks.example.com/managed/secret"
	channelUID := mustChannelUID(t)
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{
		ResourceUID:    channelUID,
		Name:           "ops",
		Type:           "webhook",
		CredentialMode: core.CredentialModeManaged,
		CredentialID:   "cred_test",
		Scope:          core.ScopeShared,
	}}}
	adapter := &recordingNotifyAdapter{}
	credentials := &fakeNotifyCredentials{addr: hook}
	tool := newTestNotifyToolWithCredentials(channels,
		&notifyPolicySpy{allowed: []string{"hooks.example.com"}},
		map[string]NotifyAdapter{"webhook": adapter}, nil, credentials)
	env := core.ToolEnv{
		AgentName:      "digest",
		RunBound:       true,
		NotifyMode:     core.NotifyModeAllEligible,
		NotifyChannels: []string{string(channelUID)},
	}

	for range 2 {
		res, err := tool.Execute(context.Background(), env,
			json.RawMessage(`{"channel":"ops","content":"日报"}`))
		require.NoError(t, err)
		require.Equal(t, "已推送到通知渠道：ops", res.Content)
		require.NotContains(t, res.Content, "hooks.example.com", "成功文案不含地址")
	}

	require.Equal(t, 2, credentials.callCount(), "每次发送都重新解析凭据")
	sent := adapter.sends()
	require.Len(t, sent, 2)
	require.Equal(t, hook, sent[0].URL)
}

// TestNotifyTool_RevokedManagedCredentialIsNotifyDisabled —— R4.8：托管凭据撤销是终态，
// 按 `notify_disabled` 固定文案失败，零外呼、零策略判定。
// A revoked managed credential fails as notify_disabled with the fixed text, before any
// policy judgement or send.
func TestNotifyTool_RevokedManagedCredentialIsNotifyDisabled(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{
		ResourceUID:    mustChannelUID(t),
		Name:           "ops",
		Type:           "webhook",
		CredentialMode: core.CredentialModeManaged,
	}}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	credentials := &fakeNotifyCredentials{
		err: fmt.Errorf("凭据已撤销: %w", core.ErrCredentialRevoked),
	}
	tool := newTestNotifyToolWithCredentials(channels, policy,
		map[string]NotifyAdapter{"webhook": adapter}, nil, credentials)

	_, err := tool.Execute(context.Background(),
		core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"日报"}`))

	require.Error(t, err)
	require.True(t, core.IsNotifyDisabled(err), "撤销归 notify_disabled")
	require.Contains(t, err.Error(), "已撤销")
	require.Contains(t, err.Error(), "notify_disabled")
	require.Equal(t, 0, adapter.count(), "撤销后不得发送")
	require.Equal(t, 0, policy.judgmentCount())
}

// TestNotifyTool_ManagedWithoutResolverFailsClosed —— 装配缺凭据解析器时 managed 渠道
// 失败关闭（不回退明文、不猜测地址），env 渠道不受影响。
// Without a resolver every managed channel fails closed; env channels are unaffected.
func TestNotifyTool_ManagedWithoutResolverFailsClosed(t *testing.T) {
	channels := &fakeNotifyChannels{rows: []core.NotifyChannel{{
		ResourceUID:    mustChannelUID(t),
		Name:           "ops",
		Type:           "webhook",
		CredentialMode: core.CredentialModeManaged,
	}}}
	adapter := &recordingNotifyAdapter{}
	policy := &notifyPolicySpy{allowed: []string{"hooks.example.com"}}
	tool := newTestNotifyTool(channels, policy, map[string]NotifyAdapter{"webhook": adapter}, nil)

	_, err := tool.Execute(context.Background(),
		core.ToolEnv{AgentName: "digest"},
		json.RawMessage(`{"channel":"ops","content":"日报"}`))

	require.Error(t, err)
	require.Contains(t, err.Error(), "未装配凭据解析器")
	require.Equal(t, 0, adapter.count())
	require.Equal(t, 0, policy.judgmentCount())
}
