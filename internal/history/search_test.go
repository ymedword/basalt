package history

// 本文件覆盖 T053 的检索面：命中只是指针（§4 的
// `TestHistorySearch_ResultsArePointersNotCitations`）、查询形状校验、端口失败与
// 违约的 fail-closed 行为。
//
// 端口用进程内假实现 `fakeMessageStore`：它按 `Scope` 在"SQL 里"过滤归属——这一步
// 与生产实现同构，因此越权结算一旦放宽，跨调用方案例会直接看到他人的行。
//
// This file covers the search surface: hits are pointers, malformed queries are
// rejected, and a store failure or contract violation fails closed.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

// tailCanary 是种子正文的最末一段：它必须在检索片段里**不出现**、只在精确读取的
// 原文里出现——这是"命中只回片段"的可判定判据。
// tailCanary appears only in the full text, never in a snippet.
const tailCanary = "TAIL-CANARY-77"

// fakeMessage 是假存储里的一行消息（含归属与 Agent，便于按范围与条件过滤）。
// fakeMessage is one seeded row of the fake store.
type fakeMessage struct {
	id        string
	sessionID string
	runID     string
	callerID  string
	agent     string
	role      string
	content   string
	evidence  []core.EvidenceRef
	createdAt time.Time
}

// fakeMessageStore 是 `MessageQueryStore` 的进程内实现：归属过滤、条件过滤、行数
// 上限都在这里完成，顺序为时间倒序（同刻按 message_id 倒序）。
//
// 它**不**做 1..200 的夹取（端口契约如此），因此 `Search` 的 `limit+1` 探测可被
// 观测：`lastQuery.Limit` 断言的就是传下去的行数上限。
// fakeMessageStore filters ownership and conditions in-process and never clamps Limit.
type fakeMessageStore struct {
	messages  []fakeMessage
	snippet   int
	err       error
	calls     int
	lastScope Scope
	lastQ     HistoryQuery
	// readCalls / sessionCalls 记录精确读取两个端口的调用次数：
	// "拒绝发生在访问存储之前"只能靠计数器证明，不能靠返回值推断。
	// readCalls / sessionCalls prove that rejections happen before any store call.
	readCalls    int
	sessionCalls int
	// readErr / sessionErr 让精确读取的两个端口各自失败，互不牵连。
	// readErr / sessionErr fail the two exact-read ports independently.
	readErr    error
	sessionErr error
}

// newFakeMessageStore 按时间倒序排好种子行。
// newFakeMessageStore seeds the rows in the port's documented order.
func newFakeMessageStore(snippetRunes int, messages ...fakeMessage) *fakeMessageStore {
	sorted := append([]fakeMessage(nil), messages...)
	// 时间倒序；同刻按 message_id 倒序，与端口契约一致。
	// Newest first; ties break on message_id descending, as the port promises.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0; j-- {
			left, right := sorted[j-1], sorted[j]
			swap := right.createdAt.After(left.createdAt) ||
				(right.createdAt.Equal(left.createdAt) && right.id > left.id)
			if !swap {
				break
			}
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	return &fakeMessageStore{messages: sorted, snippet: snippetRunes}
}

// visible 复刻存储端口的归属过滤：管理员不限（含迁移遗留的空归属行），调用方只看
// 自己；空 `CallerID` 的调用方范围一律不返回任何行（不得退化成"全部"）。
// visible mirrors the SQL ownership filter; an empty caller scope sees nothing.
func (f *fakeMessageStore) visible(m fakeMessage, scope Scope) bool {
	if scope.Admin {
		return scope.CallerID == "" || m.callerID == scope.CallerID
	}
	return scope.CallerID != "" && m.callerID == scope.CallerID
}

// SearchMessages 实现检索端口。
// SearchMessages implements the search port.
func (f *fakeMessageStore) SearchMessages(_ context.Context, scope Scope, q HistoryQuery) ([]HistoryHit, error) {
	f.calls++
	f.lastScope = scope
	f.lastQ = q
	if f.err != nil {
		return nil, f.err
	}
	if q.Limit <= 0 {
		// 端口契约要求调用方给出正数上限；零值视为违约，直接报错而不是返回全部。
		// A non-positive cap is a contract violation, not "return everything".
		return nil, errors.New("fake store: non-positive limit")
	}
	var hits []HistoryHit
	for _, m := range f.messages {
		if !f.visible(m, scope) {
			continue
		}
		if q.CallerID != "" && m.callerID != q.CallerID {
			continue
		}
		if q.SessionID != "" && m.sessionID != q.SessionID {
			continue
		}
		if q.Agent != "" && m.agent != q.Agent {
			continue
		}
		if !q.From.IsZero() && m.createdAt.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && m.createdAt.After(q.To) {
			continue
		}
		if !strings.Contains(m.content, q.Keywords) {
			continue
		}
		hits = append(hits, HistoryHit{
			MessageID: m.id,
			SessionID: m.sessionID,
			RunID:     m.runID,
			Role:      m.role,
			Snippet:   snippetOf(m.content, f.snippet),
			Score:     float64(len(m.content)),
			CreatedAt: m.createdAt,
		})
		if len(hits) == q.Limit {
			break
		}
	}
	return hits, nil
}

// ReadMessages 实现精确读取端口：按主键取行、按归属过滤，缺失即未找到。
// ReadMessages implements the exact-read port.
func (f *fakeMessageStore) ReadMessages(_ context.Context, scope Scope, refs []MessageRef) ([]MessageView, error) {
	f.readCalls++
	f.lastScope = scope
	if f.readErr != nil {
		return nil, f.readErr
	}
	out := make([]MessageView, 0, len(refs))
	for _, ref := range refs {
		found := false
		for _, m := range f.messages {
			if m.id != ref.MessageID || !f.visible(m, scope) {
				continue
			}
			if ref.SessionID != "" && m.sessionID != ref.SessionID {
				continue
			}
			out = append(out, MessageView{
				MessageID:    m.id,
				SessionID:    m.sessionID,
				RunID:        m.runID,
				Seq:          1,
				Role:         m.role,
				Content:      m.content,
				EvidenceRefs: append([]core.EvidenceRef(nil), m.evidence...),
				CreatedAt:    m.createdAt,
			})
			found = true
			break
		}
		if !found {
			return nil, ErrNotFound
		}
	}
	return out, nil
}

// ResolveSession 实现会话归属解析。
// ResolveSession implements session ownership resolution.
func (f *fakeMessageStore) ResolveSession(_ context.Context, scope Scope, sessionID string) (SessionRef, error) {
	f.sessionCalls++
	f.lastScope = scope
	if f.sessionErr != nil {
		return SessionRef{}, f.sessionErr
	}
	for _, m := range f.messages {
		if m.sessionID != sessionID || !f.visible(m, scope) {
			continue
		}
		return SessionRef{
			SessionID: m.sessionID,
			CallerID:  m.callerID,
			Agent:     m.agent,
			CreatedAt: m.createdAt,
		}, nil
	}
	return SessionRef{}, ErrNotFound
}

// snippetOf 按上限截取片段：超过上限时截断并加省略号，正文尾部**不得**出现。
// snippetOf clips the snippet; the tail of the content never appears.
func snippetOf(content string, limit int) string {
	if limit <= 0 {
		return content
	}
	runes := []rune(content)
	if len(runes) <= limit {
		return content
	}
	return string(runes[:limit]) + "…"
}

// quietLogger 丢弃日志输出：断言只针对返回错误，不看日志文本。
// quietLogger discards log output; assertions target returned errors only.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// callerPrincipal 构造一个持有所需 scope 的调用方身份。
// callerPrincipal builds a caller principal with the given scopes.
func callerPrincipal(callerID string, scopes ...string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: callerID,
		KeyID:    "key-" + callerID,
		Scopes:   scopes,
		Actor:    core.ActorCaller,
	}
}

// seedTime 是种子消息的基准时间（UTC），避免用当前时钟导致断言随机器时间漂移。
// seedTime is a fixed UTC base so assertions never drift with the wall clock.
var seedTime = time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)

// TestHistorySearch_ResultsArePointersNotCitations 保护"检索只是指针"这条不变量
// （设计 §4 同名用例）：命中不得携带可直接引用的正文，也不得被判为 Supported；
// 原文只能经精确读取取得。
//
// 逆改点（实现怎么改它会红）：
//   - 让 `SearchMessages` 把整段正文塞进 `Snippet` → 片段断言见到尾部标记；
//   - 让 `locateSources` 接受没有版本与内容哈希的来源（即允许命充当引用）→
//     `Supported` 非空；
//   - 让精确读取返回截断视图 → 原文断言见不到尾部标记。
//
// This test pins the pointers-only invariant: a hit carries no citable content, cannot
// become Supported, and the full text requires an exact read.
func TestHistorySearch_ResultsArePointersNotCitations(t *testing.T) {
	content := "发布前必须先备份 audit.db 并记录校验和" + strings.Repeat("·", 200) + tailCanary
	store := newFakeMessageStore(40, fakeMessage{
		id:        "42",
		sessionID: "sess-1",
		runID:     "run-1",
		callerID:  "cal_1",
		agent:     "ops",
		role:      "assistant",
		content:   content,
		createdAt: seedTime,
	})
	searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})
	ctx := context.Background()
	principal := callerPrincipal("cal_1", core.ScopeSessionsRead)

	result, err := searcher.Search(ctx, principal, HistoryQuery{Keywords: "备份"})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)

	hit := result.Hits[0]
	require.Equal(t, "42", hit.MessageID, "命中必须带回精确读取所需的主键")
	require.Contains(t, hit.Snippet, "备份")
	require.NotContains(t, hit.Snippet, tailCanary, "检索命中不得携带可直接引用的正文")

	// 用命中自己的字段拼一个"来源"：它没有版本、没有内容哈希，因此连 `located`
	// 都不成立——检索命中在任何口径下都进不了 Supported。
	// Fabricating a source out of the hit still cannot locate it: no version, no hash.
	answer := "依据（来源 42），发布前必须先备份 audit.db。"
	fabricated := knowledge.ResolveCitations(answer, []knowledge.CitationSource{{
		ChunkID: hit.MessageID,
		Text:    hit.Snippet,
	}})
	require.Empty(t, fabricated.Supported, "检索命中不得判为 Supported")
	require.NotEmpty(t, fabricated.Unsupported, "缺版本与内容哈希的来源连 located 都不成立")

	// 即便完全不提供来源，答案里出现的命中标识也只能是 Unresolved（没读过）。
	// Without any source the cited id stays unresolved: it was never read.
	unread := knowledge.ResolveCitations(answer, nil)
	require.Empty(t, unread.Supported)
	require.NotEmpty(t, unread.Unresolved)

	// 原文只能经精确读取取得，且必须是完整正文。
	views, err := store.ReadMessages(ctx, resolveMust(t, principal, ""), []MessageRef{{MessageID: hit.MessageID}})
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, content, views[0].Content, "精确读取必须返回完整原文")
	require.False(t, views[0].Truncated)
}

// TestHistorySearch_RejectsMalformedQuery 保护查询形状校验：空关键词、倒置时间窗、
// 越界 limit 都在访问存储**之前**被拒（`calls` 保持 0）。
//
// 逆改点：把校验放到存储调用之后，或把越界 limit 静默夹到 200 → `calls` 不再为 0 /
// 错误类型不再是 `ErrInvalidQuery`。
//
// This test pins the query-shape checks and that they run before any store call.
func TestHistorySearch_RejectsMalformedQuery(t *testing.T) {
	principal := callerPrincipal("cal_1", core.ScopeSessionsRead)
	cases := []struct {
		name  string
		query HistoryQuery
	}{
		{name: "empty keywords", query: HistoryQuery{Keywords: "   "}},
		{name: "inverted window", query: HistoryQuery{Keywords: "备份", From: seedTime, To: seedTime.Add(-time.Hour)}},
		{name: "negative limit", query: HistoryQuery{Keywords: "备份", Limit: -1}},
		{name: "limit above ceiling", query: HistoryQuery{Keywords: "备份", Limit: maxSearchLimit + 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeMessageStore(40)
			searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

			_, err := searcher.Search(context.Background(), principal, tc.query)
			require.ErrorIs(t, err, ErrInvalidQuery)
			require.Zero(t, store.calls, "形状非法必须在访问存储之前拒绝")
		})
	}
}

// TestHistorySearch_StoreFailureAndContractViolationFailClosed 保护存储侧的两条
// 失败路径：端口报错原样上抛（可 errors.Is 判定），端口违约（无主键命中）则
// fail-closed，**不得**把可疑结果当成命中返回。
//
// 逆改点：吞掉端口错误返回空结果 → 第一个子用例拿到 nil error；对缺主键的命中
// 照常返回 → 第二个子用例拿到结果而不是错误。
//
// This test pins that store failures propagate and contract violations fail closed.
func TestHistorySearch_StoreFailureAndContractViolationFailClosed(t *testing.T) {
	principal := callerPrincipal("cal_1", core.ScopeSessionsRead)
	boom := errors.New("store down")

	t.Run("store error propagates", func(t *testing.T) {
		store := newFakeMessageStore(40)
		store.err = boom
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		_, err := searcher.Search(context.Background(), principal, HistoryQuery{Keywords: "备份"})
		require.ErrorIs(t, err, boom, "端口错误必须可被 errors.Is 判定")
	})

	t.Run("hit without a message id fails closed", func(t *testing.T) {
		store := &badHitStore{}
		searcher := NewSearcher(SearcherDeps{Store: store, Logger: quietLogger()})

		_, err := searcher.Search(context.Background(), principal, HistoryQuery{Keywords: "备份"})
		require.ErrorIs(t, err, errStoreContract)
	})

	t.Run("missing port fails closed", func(t *testing.T) {
		searcher := NewSearcher(SearcherDeps{Logger: quietLogger()})

		_, err := searcher.Search(context.Background(), principal, HistoryQuery{Keywords: "备份"})
		require.ErrorIs(t, err, ErrHistoryUnavailable, "缺端口不得冒充空结果")
	})
}

// badHitStore 是只用来触发"命中缺主键"违约的端口实现。
// badHitStore violates the port by returning a hit without a primary key.
type badHitStore struct{}

// SearchMessages 返回一条没有 message_id 的命中。
// SearchMessages returns one hit that carries no message id.
func (b *badHitStore) SearchMessages(context.Context, Scope, HistoryQuery) ([]HistoryHit, error) {
	return []HistoryHit{{SessionID: "sess-1", Role: "assistant"}}, nil
}

// ReadMessages 不参与本用例。
// ReadMessages is unused here.
func (b *badHitStore) ReadMessages(context.Context, Scope, []MessageRef) ([]MessageView, error) {
	return nil, ErrNotFound
}

// ResolveSession 不参与本用例。
// ResolveSession is unused here.
func (b *badHitStore) ResolveSession(context.Context, Scope, string) (SessionRef, error) {
	return SessionRef{}, ErrNotFound
}

// resolveMust 是测试助手：结算范围失败即终止用例。
// resolveMust resolves the scope or fails the test.
func resolveMust(t *testing.T, p core.Principal, requested string) Scope {
	t.Helper()
	scope, err := resolveScope(p, requested)
	require.NoError(t, err)
	return scope
}
