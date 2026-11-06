package app

// 本文件是 US-22 装配面的**无库**用例（T012）：判的是生产链路的三件事——
//
//  1. **装饰器可达**：生产运行入口交出的是 `guardedProviderClient` 外面再套
//     `diagnosticProviderClient`；两者都必须实现 `core.MemoryExtractor` 并把调用转发给
//     内层冻结视图，否则 `env.ProviderClient()` 的类型断言恒失败，抽取在生产环境永不可达；
//  2. **路由器禁区**：`catalogProviderRouter` 按调用发生时的快照路由，**不得**实现抽取
//     面；只允许 `Pinned` 交出的准入冻结视图被断言成 `MemoryExtractor`；
//  3. **凭据守点不因抽取放宽**：抽取调用与对话调用走同一条 `CheckRun`——撤销立即拒绝、
//     已释放的固定引用同样拒绝，且拒绝发生在审计预写与模型调用之前。
//
// 有库的撤权 / 释放分支由既有 `credential_guard_test.go` 判卷；本文件用桩把"检查点是否
// 被调用、拒绝是否阻断发出"钉在装配层。
//
// Unit-level cases for the US-22 assembly: the production decorators are reachable as
// extractors, the live router deliberately is not, and the credential checkpoint gates
// extraction exactly like chat.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/diagnostics"
	"github.com/ymedword/basalt/internal/provider"
)

// us22DiscardLogger 是本文件所有装配共用的静默日志器。
// us22DiscardLogger keeps the assembly tests quiet.
func us22DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// us22StubClient 是协议桩：记录调用次数并返回固定响应。
// us22StubClient is a protocol stub recording call count.
type us22StubClient struct {
	calls int
	resp  core.ChatResponse
	err   error
}

func (c *us22StubClient) Chat(context.Context, core.ChatRequest) (core.ChatResponse, error) {
	c.calls++
	return c.resp, c.err
}

// us22AuditStore 同时实现聊天审计与抽取预写面，分别记录两条入口被调用几次。
// us22AuditStore implements both audit surfaces and counts each entry separately.
type us22AuditStore struct {
	chatBegins    int
	extractBegins int
	finishCalls   int
	lastExtract   core.LLMCall
}

func (s *us22AuditStore) BeginCall(context.Context, core.LLMCall) (int64, error) {
	s.chatBegins++
	return 1, nil
}

func (s *us22AuditStore) BeginMemoryExtractCall(_ context.Context, call core.LLMCall) (int64, error) {
	s.extractBegins++
	s.lastExtract = call
	return 2, nil
}

func (s *us22AuditStore) FinishCall(context.Context, int64, core.LLMCallResult) error {
	s.finishCalls++
	return nil
}

// us22Checkpoint 是凭据检查点桩：记录被检查的 Run 并返回预设错误。
// us22Checkpoint records the checked run ids and returns a preset error.
type us22Checkpoint struct {
	calls []string
	err   error
}

func (c *us22Checkpoint) CheckRun(_ context.Context, runID string) error {
	c.calls = append(c.calls, runID)
	return c.err
}

// us22StubExtractor 直接实现抽取面，用来观察内层是否被调用；`Chat` 单列计数，
// 任何"退回聊天路径"都会在断言里现形。
// us22StubExtractor counts extraction calls and counts chat calls separately, so any
// fallback to the chat path shows up in the assertions.
type us22StubExtractor struct {
	calls     int
	chatCalls int
}

func (e *us22StubExtractor) Chat(context.Context, core.ChatRequest) (core.ChatResponse, error) {
	e.chatCalls++
	return core.ChatResponse{Content: "chat"}, nil
}

func (e *us22StubExtractor) ChatMemoryExtract(context.Context, core.ChatRequest) (core.ChatResponse, error) {
	e.calls++
	return core.ChatResponse{Content: "inner"}, nil
}

// us22PinStore 是固定版本读面桩，按 Run 返回预定引用。
// us22PinStore returns preset credential pins per run.
type us22PinStore struct {
	pins []core.RunCredentialRef
	err  error
}

func (s *us22PinStore) Pins(context.Context, string) ([]core.RunCredentialRef, error) {
	return s.pins, s.err
}

// us22ReleasedGuard 复刻生产守卫的"已释放即拒绝"分支（DB 版由
// TestCredentialGuard_RejectsRevoked 判卷）：本文件只用它确认检查点的拒绝会阻断发出。
// us22ReleasedGuard mirrors the released branch of the production guard so the test can
// prove a released pin blocks extraction before anything is emitted.
type us22ReleasedGuard struct {
	seen []core.RunCredentialRef
}

func (g *us22ReleasedGuard) Check(_ context.Context, ref core.RunCredentialRef) error {
	g.seen = append(g.seen, ref)
	if ref.ReleasedAt != nil {
		return core.ErrCredentialUnavailable
	}
	return nil
}

// us22Recorder 记录诊断事件条数。
// us22Recorder counts diagnostic events.
type us22Recorder struct {
	events int
}

func (r *us22Recorder) Record(context.Context, diagnostics.DiagnosticEvent) { r.events++ }

// TestUS22_Extractor_DecoratedReachable：生产装饰器链（诊断 + 凭据检查点）必须满足抽取
// 接口，调用经冻结视图落到 `purpose='memory_extract'` 的专用预写，且不记 `llm_call`
// 诊断事件。
// 逆改点：删掉任一层装饰器的 `ChatMemoryExtract` → 类型断言失败而红；装饰器改调
// `Chat` → `chatBegins` 非零而红；诊断装饰器照抄 Chat 的事件记录 → 事件数非零而红。
// TestUS22_Extractor_DecoratedReachable pins decorator reachability and the dedicated
// pre-write path.
func TestUS22_Extractor_DecoratedReachable(t *testing.T) {
	ctx := context.Background()
	wire := &us22StubClient{resp: core.ChatResponse{Content: "ok"}}
	audit := &us22AuditStore{}
	service := provider.NewProviderService(
		map[string]core.ProviderClient{"prov": wire}, map[string]string{"prov": "model"},
		audit, us22DiscardLogger()).WithMemoryExtractStore(audit)

	checkpoint := &us22Checkpoint{}
	guarded := &guardedProviderClient{inner: service, checkpoint: checkpoint}
	recorder := &us22Recorder{}
	decorated := newDiagnosticProviderClient(guarded, recorder)

	extractor, ok := decorated.(core.MemoryExtractor)
	require.True(t, ok, "生产装饰器链必须实现抽取面，否则运行入口的类型断言恒失败")

	resp, err := extractor.ChatMemoryExtract(ctx, core.ChatRequest{
		Provider: "prov", RunID: "run-1", Round: 0,
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Content)
	require.Equal(t, 1, wire.calls, "装饰器必须把抽取转发到内层冻结视图")
	require.Equal(t, []string{"run-1"}, checkpoint.calls, "抽取调用必须先过 Run 凭据检查点")
	require.Equal(t, 1, audit.extractBegins, "抽取必须走 memory_extract 专用预写")
	require.Equal(t, 0, audit.chatBegins, "抽取不得退回聊天预写")
	require.Equal(t, 1, audit.finishCalls)
	require.Equal(t, 0, recorder.events, "抽取 round=0 低于诊断步号下限，不得记 llm_call 事件")
	require.Equal(t, 0, audit.lastExtract.Round)
	require.Equal(t, "run-1", audit.lastExtract.RunID)
}

// TestUS22_Extractor_GuardedChecksCredential：凭据检查点拒绝（撤销）时，抽取调用
// **不得发出**——内层调用次数为零，错误原样上抛。
// 逆改点：把 `ChatMemoryExtract` 写成直接转发（不过 `CheckRun`）→ 内层被调用而红。
// TestUS22_Extractor_GuardedChecksCredential pins that a revoked credential blocks the
// extraction call before it is emitted.
func TestUS22_Extractor_GuardedChecksCredential(t *testing.T) {
	inner := &us22StubExtractor{}
	checkpoint := &us22Checkpoint{err: core.ErrCredentialRevoked}
	guarded := &guardedProviderClient{inner: inner, checkpoint: checkpoint}

	_, err := guarded.ChatMemoryExtract(context.Background(), core.ChatRequest{
		Provider: "prov", RunID: "run-revoked", Round: 0,
	})
	require.ErrorIs(t, err, core.ErrCredentialRevoked)
	require.Equal(t, []string{"run-revoked"}, checkpoint.calls)
	require.Zero(t, inner.calls, "撤权后抽取调用不得发出")
	require.Zero(t, inner.chatCalls, "撤权后不得退回聊天路径")
}

// TestUS22_Extractor_GuardedRejectsReleasedRef：Run 的固定凭据引用已释放时，检查点
// 仍把该引用交给守卫并由守卫拒绝——抽取**不得**越过已释放的引用发出。
// 逆改点：检查点按 `ReleasedAt` 静默跳过该引用 → 守卫看不到它、内层被调用而红；
// 装饰器绕过检查点 → `inner.calls` 非零而红。
// TestUS22_Extractor_GuardedRejectsReleasedRef pins that released pins reach the guard
// and their refusal blocks the extraction.
func TestUS22_Extractor_GuardedRejectsReleasedRef(t *testing.T) {
	released := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC).UnixMilli()
	pins := &us22PinStore{pins: []core.RunCredentialRef{{
		RunID:             "run-released",
		CredentialID:      "cred-1",
		CredentialVersion: 1,
		ResourceKind:      core.BindingModelConnection,
		ResourceName:      "prov",
		ReleasedAt:        &released,
	}}}
	guard := &us22ReleasedGuard{}
	checkpoint, err := NewRunCredentialCheckpoint(RunCredentialCheckpointDeps{Pins: pins, Guard: guard})
	require.NoError(t, err)

	inner := &us22StubExtractor{}
	guarded := &guardedProviderClient{inner: inner, checkpoint: checkpoint}
	_, err = guarded.ChatMemoryExtract(context.Background(), core.ChatRequest{
		Provider: "prov", RunID: "run-released", Round: 0,
	})
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)
	require.Len(t, guard.seen, 1, "已释放的固定引用必须交给守卫判定，不得被静默跳过")
	require.NotNil(t, guard.seen[0].ReleasedAt)
	require.Zero(t, inner.calls, "已释放引用的 Run 不得发出抽取调用")
	require.Zero(t, inner.chatCalls, "已释放引用的 Run 不得退回聊天路径")
}

// TestUS22_Router_NotExtractor：按当前快照路由的路由器**不得**满足抽取接口；只有
// `Pinned` 交出的准入冻结视图可以抽取。装配期装入预写面后，新视图带仓储、旧视图
// fail-closed，且旧视图的拒绝不写审计行、不发出调用。
// 逆改点：给路由器补 `ChatMemoryExtract` → 第一个类型断言为真而红；`Pinned` 不把
// 仓储带进一行视图 → 新视图在预写处 fail-closed 而红。
// TestUS22_Router_NotExtractor pins the router's forbidden extraction surface and the
// pre-write surface carried by pinned views built after assembly.
func TestUS22_Router_NotExtractor(t *testing.T) {
	ctx := context.Background()
	wire := &us22StubClient{resp: core.ChatResponse{Content: "ok"}}
	audit := &us22AuditStore{}
	holder, err := newProviderCatalogStore(&providerCatalog{
		clients: map[string]core.ProviderClient{"prov": wire},
		models:  map[string]string{"prov": "model"},
	})
	require.NoError(t, err)
	router := newCatalogProviderRouter(holder, audit, us22DiscardLogger())

	_, ok := any(router).(core.MemoryExtractor)
	require.False(t, ok, "路由器不得实现抽取面：它按调用发生时的快照路由，不是准入冻结视图")

	wired := router.WithMemoryExtractStore(audit)
	require.NotSame(t, router, wired, "装配必须返回新路由器，旧实例不得被原地修改")

	pinned, ok := wired.Pinned("prov")
	require.True(t, ok)
	extractor, ok := any(pinned).(core.MemoryExtractor)
	require.True(t, ok, "装入预写面后，Pinned 交出的冻结视图必须可抽取")
	resp, err := extractor.ChatMemoryExtract(ctx, core.ChatRequest{
		Provider: "prov", RunID: "run-router", Round: 0,
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Content)
	require.Equal(t, 1, audit.extractBegins)
	require.Equal(t, 0, audit.chatBegins)

	// 未装预写面的旧路由器：冻结视图在预写处 fail-closed，不写审计行、不发出调用。
	oldPinned, ok := router.Pinned("prov")
	require.True(t, ok)
	_, err = oldPinned.ChatMemoryExtract(ctx, core.ChatRequest{
		Provider: "prov", RunID: "run-router-2", Round: 0,
	})
	require.ErrorIs(t, err, core.ErrLearningExtractorUnavailable)
	require.Equal(t, 1, audit.extractBegins, "未装预写面的视图不得写出审计行")
	require.Equal(t, 1, wire.calls, "未装预写面的视图不得发出模型调用")
}
