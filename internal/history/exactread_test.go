package history

// 本文件覆盖 T054 的**精确读取**面：按主键取原文、跨调用方一律未找到（设计 §4 的
// `TestHistory_ExactReadCrossCallerNotFound`）、无状态临时标识不是主键、端口违约
// fail-closed。
//
// 假存储在 `ReadMessages` / `ResolveSession` 里按 `Scope` 过滤归属——与生产实现
// 同构。因此一旦 `ExactReader.Read` 丢掉范围（或把它放宽成管理员），跨调用方案例
// 会直接读到他人的原文；而"拒绝发生在访问存储之前"由端口计数器 `readCalls` /
// `sessionCalls` 证明，不靠返回值推断。
//
// This file covers exact reads: primary-key loads, cross-caller not-found, the
// stateless id class, and fail-closed store contract violations.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// seedExactRead 在 `seedCrossCaller` 之上给 cal_1 的消息挂一条来源引用：
// 精确读取必须**原样**带回，便于断言"不得解析成最新版本、不得改写"。
// seedExactRead adds one evidence ref to the first row so pass-through is assertable.
func seedExactRead() []fakeMessage {
	rows := seedCrossCaller()
	rows[0].evidence = []core.EvidenceRef{{
		Kind: "knowledge", ID: "doc-1", VersionID: "v1", ContentHash: "hash-1",
	}}
	return rows
}

// newExactStore 造一份种子存储。
// newExactStore seeds a fake store for the exact-read cases.
func newExactStore() *fakeMessageStore {
	return newFakeMessageStore(40, seedExactRead()...)
}

// exactReaderFor 用一个**静默**日志器装配读取器：断言的判据是错误与返回值，
// 不读日志文本（默认 `slog.Default()` 会往测试输出里写 WARN）。
// exactReaderFor builds a reader with a quiet logger.
func exactReaderFor(store MessageQueryStore) *ExactReader {
	return &ExactReader{store: store, logger: quietLogger()}
}

// TestHistory_ExactReadCrossCallerNotFound 保护 C-28 在**精确读取**面的边界：
// 读不到的一律 404（不是 403），管理员可跨调用方，迁移遗留的空归属只对管理员可见。
//
// 逆改点（实现怎么改它会红）：
//   - `Read` 把范围结算成空 / 管理员，或干脆不传 `Scope` → 跨调用方子用例读到原文；
//   - 跨调用方改报 `ErrForbidden` → 第一条子用例的 `NotErrorIs(core.ErrForbidden)`
//     与 `ErrNotFound` 断言同时失败（403 会泄露"这条消息存在"）；
//   - 给了会话却不复核 → "会话不属于自己"子用例读到原文；
//   - 把无归属的遗留消息匹配给任意调用方 → 遗留子用例不再 404。
//
// This test pins the C-28 boundary on exact reads: cross-caller reads are 404, never
// 403; admins span callers; legacy ownerless rows stay admin-only.
func TestHistory_ExactReadCrossCallerNotFound(t *testing.T) {
	ctx := context.Background()
	owner := callerPrincipal("cal_1", core.ScopeSessionsRead)

	t.Run("caller reads its own message", func(t *testing.T) {
		store := newExactStore()
		view, err := exactReaderFor(store).Read(ctx, owner, MessageRef{
			MessageID: "11", SessionID: "sess-cal-1",
		})
		require.NoError(t, err)
		require.Equal(t, "11", view.MessageID)
		require.Equal(t, "sess-cal-1", view.SessionID)
		require.Equal(t, "备份 audit.db 的记录", view.Content, "精确读取返回完整原文")
		require.Equal(t, Scope{CallerID: "cal_1"}, store.lastScope, "调用方范围必须结算成自己的归属")
		require.Equal(t, 1, store.sessionCalls, "给了会话就先复核会话归属")
	})

	t.Run("caller reading another caller's message is not found", func(t *testing.T) {
		store := newExactStore()
		_, err := exactReaderFor(store).Read(ctx, owner, MessageRef{MessageID: "22"})
		require.ErrorIs(t, err, ErrNotFound, "跨调用方的消息一律未找到")
		require.NotErrorIs(t, err, core.ErrForbidden, "403 会泄露他人消息的存在")
	})

	t.Run("caller asking under another caller's session is not found", func(t *testing.T) {
		store := newExactStore()
		_, err := exactReaderFor(store).Read(ctx, owner, MessageRef{
			MessageID: "11", SessionID: "sess-cal-2",
		})
		require.ErrorIs(t, err, ErrNotFound, "会话归属不符按未找到处理")
		require.Zero(t, store.readCalls, "会话复核失败后不得再读消息")
	})

	t.Run("legacy message is not found for a caller", func(t *testing.T) {
		store := newExactStore()
		_, err := exactReaderFor(store).Read(ctx, owner, MessageRef{MessageID: "33"})
		require.ErrorIs(t, err, ErrNotFound, "空归属的遗留消息只对管理员可见")
	})

	t.Run("admin reads across callers", func(t *testing.T) {
		store := newExactStore()
		view, err := exactReaderFor(store).Read(ctx, core.LocalAdminPrincipal(), MessageRef{MessageID: "22"})
		require.NoError(t, err)
		require.Equal(t, "22", view.MessageID)
		require.True(t, store.lastScope.Admin)
	})

	t.Run("admin reads a legacy message", func(t *testing.T) {
		store := newExactStore()
		view, err := exactReaderFor(store).Read(ctx, core.LocalAdminPrincipal(), MessageRef{MessageID: "33"})
		require.NoError(t, err)
		require.Equal(t, "33", view.MessageID)
	})

	t.Run("evidence refs are passed through untouched", func(t *testing.T) {
		store := newExactStore()
		view, err := exactReaderFor(store).Read(ctx, owner, MessageRef{MessageID: "11"})
		require.NoError(t, err)
		require.Equal(t, []core.EvidenceRef{{
			Kind: "knowledge", ID: "doc-1", VersionID: "v1", ContentHash: "hash-1",
		}}, view.EvidenceRefs, "引用原样透传：不解析成最新版本、不改写已发送消息")
	})
}

// TestHistory_ExactReadRejectsStatelessIdentifier 保护"无状态临时标识不是主键"：
// `msg/<run>/<pos>`（`core.StatelessMessageID`）与任何非十进制串在访问存储**之前**
// 一律未找到；身份校验同样先于存储。
//
// 逆改点：把分类挪到存储调用之后、或让非十进制串直接进 `ReadMessages` →
// `readCalls` 不再为 0；把 401 / 403 换成未找到 → 错误断言失败。
//
// This test pins that stateless and non-decimal ids never reach the store, and that
// identity checks run first.
func TestHistory_ExactReadRejectsStatelessIdentifier(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		messageID string
	}{
		{name: "stateless literal", messageID: core.StatelessMessageID("run-1", 3)},
		{name: "empty id", messageID: ""},
		{name: "non-decimal id", messageID: "abc"},
		{name: "stateless prefix without position", messageID: "msg/run-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newExactStore()
			_, err := exactReaderFor(store).Read(ctx, callerPrincipal("cal_1", core.ScopeSessionsRead),
				MessageRef{MessageID: tc.messageID, SessionID: "sess-cal-1"})
			require.ErrorIs(t, err, ErrNotFound, "不是主键的标识即「没有这条消息」")
			require.Zero(t, store.readCalls, "分类必须在访问存储之前完成")
			require.Zero(t, store.sessionCalls, "分类必须在访问存储之前完成")
		})
	}

	t.Run("zero principal is unauthorized", func(t *testing.T) {
		store := newExactStore()
		_, err := exactReaderFor(store).Read(ctx, core.Principal{}, MessageRef{MessageID: "11"})
		require.ErrorIs(t, err, core.ErrUnauthorized, "零值身份不得被补成内置管理员")
		require.Zero(t, store.readCalls)
		require.Zero(t, store.sessionCalls)
	})

	t.Run("caller without sessions read is forbidden", func(t *testing.T) {
		store := newExactStore()
		_, err := exactReaderFor(store).Read(ctx,
			callerPrincipal("cal_1", core.ScopeSessionsWrite), MessageRef{MessageID: "11"})
		require.ErrorIs(t, err, ErrForbidden)
		require.Zero(t, store.readCalls)
		require.Zero(t, store.sessionCalls)
	})
}

// TestHistory_ExactReadStoreFailureAndContractViolationFailClosed 保护存储侧失败路径：
// 端口报错原样上抛（可 `errors.Is` 判定），端口违约（条数不符、标识不符、会话不符）
// 一律 fail-closed，**不得**把可疑结果当原文返回。
//
// 逆改点：吞掉端口错误返回零值视图 → 第一个子用例拿到 nil error；不核对条数与标识
// → 违约子用例拿到"原文"而不是错误。
//
// This test pins that store failures propagate and contract violations fail closed.
func TestHistory_ExactReadStoreFailureAndContractViolationFailClosed(t *testing.T) {
	ctx := context.Background()
	owner := callerPrincipal("cal_1", core.ScopeSessionsRead)
	boom := errors.New("store down")

	t.Run("missing port fails closed", func(t *testing.T) {
		reader := NewExactReader(nil)
		_, err := reader.Read(ctx, owner, MessageRef{MessageID: "11"})
		require.ErrorIs(t, err, ErrHistoryUnavailable, "缺端口不得冒充空消息")
	})

	t.Run("store error propagates", func(t *testing.T) {
		store := newExactStore()
		store.readErr = boom
		_, err := exactReaderFor(store).Read(ctx, owner, MessageRef{MessageID: "11"})
		require.ErrorIs(t, err, boom, "端口错误必须可被 errors.Is 判定")
	})

	t.Run("session resolve error propagates", func(t *testing.T) {
		store := newExactStore()
		store.sessionErr = boom
		_, err := exactReaderFor(store).Read(ctx, owner, MessageRef{MessageID: "11", SessionID: "sess-cal-1"})
		require.ErrorIs(t, err, boom)
		require.Zero(t, store.readCalls)
	})

	t.Run("no row is a contract violation", func(t *testing.T) {
		_, err := exactReaderFor(&looseStore{views: nil}).Read(ctx, owner, MessageRef{MessageID: "11"})
		require.ErrorIs(t, err, errStoreContract, "缺行必须报违约，不得回零值视图")
	})

	t.Run("row with another message id is a contract violation", func(t *testing.T) {
		loose := &looseStore{views: []MessageView{{MessageID: "99", SessionID: "sess-cal-1"}}}
		_, err := exactReaderFor(loose).Read(ctx, owner, MessageRef{MessageID: "11"})
		require.ErrorIs(t, err, errStoreContract, "端口不得用别的消息顶替")
	})

	t.Run("row with another session id is a contract violation", func(t *testing.T) {
		loose := &looseStore{views: []MessageView{{MessageID: "11", SessionID: "sess-other"}}}
		_, err := exactReaderFor(loose).Read(ctx, owner, MessageRef{MessageID: "11", SessionID: "sess-cal-1"})
		require.ErrorIs(t, err, errStoreContract, "会话不符不得当原文返回")
	})
}

// looseStore 是只用来触发端口违约的精确读取端口实现：`ReadMessages` 原样返回预置
// 视图，`ResolveSession` 总是成功返回请求的会话。
// looseStore returns whatever the case preloaded, so contract checks can be driven.
type looseStore struct {
	views []MessageView
}

// SearchMessages 不参与精确读取用例。
// SearchMessages is unused here.
func (l *looseStore) SearchMessages(context.Context, Scope, HistoryQuery) ([]HistoryHit, error) {
	return nil, nil
}

// ReadMessages 原样返回预置视图，不做任何核对。
// ReadMessages returns the preloaded views verbatim.
func (l *looseStore) ReadMessages(context.Context, Scope, []MessageRef) ([]MessageView, error) {
	return l.views, nil
}

// ResolveSession 总是解析成功，返回请求的会话标识。
// ResolveSession always resolves to the requested session.
func (l *looseStore) ResolveSession(_ context.Context, _ Scope, sessionID string) (SessionRef, error) {
	return SessionRef{SessionID: sessionID}, nil
}
