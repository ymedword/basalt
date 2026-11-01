package history

// 本文件实现历史**检索**（未知位置）（US-16 §3.1.10 / §2.10.4 图 9 的检索行 /
// T053）：结算归属 → 校验查询形状 → 取 `limit+1` 条探测 → 裁剪并标注截断。
//
// 三条硬约束：
//  1. **命中只是指针**：`HistoryHit` 只有 message_id / session_id / run_id / role /
//     片段 / 分数，**没有正文位**——正文只经 `ExactReader.Read` 取得。检索命中
//     **不得**参与 `CitationReport`（连 `located` 都不成立，见 §2.10.4 引用判定）。
//  2. **截断必须显式标注**：`SearchResult.Truncated` 为真表示"还有更多，本页不是
//     全部"。静默裁剪会让调用方以为已穷尽（§4 的
//     `TestHistory_SearchBudgetTruncationMarked` 就是这个逆改点）。
//  3. **归属由 Scope 强制**：关键词、Agent、会话、时间、`caller_id` 都只是**收窄**
//     条件，永远不能放宽 `resolveScope` 结算出的归属过滤。
//
// 与设计卡 §3.1.10 的一处**签名差异**（记入 tasks.md 附录 R64，交人工 review ②
// 裁决）：交付物表写的是 `Search(...) ([]HistoryHit, error)`，但该签名没有任何位置
// 承载"预算截断"标注，而 §2.10.4（截断必须显式标注）与 §4 的
// `TestHistory_SearchBudgetTruncationMarked`（缺 `truncated` 标记即红）都要求它。
// 因此返回类型改为 `SearchResult{Hits, Truncated}`，并同步给冻结契约的
// `HistorySearchResult` 追加**可选** `truncated` 字段（默认 false，向后兼容）。
//
// This file implements history search: resolve the scope, validate the query,
// probe limit+1 rows, then trim and mark truncation. Hits are pointers only —
// there is deliberately no content field.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// defaultSearchLimit 是契约里 `limit` 的默认值（1..200，缺省 50）。
	// defaultSearchLimit is the contract default for limit.
	defaultSearchLimit = 50
	// maxSearchLimit 是契约里 `limit` 的上限；超过即请求形状非法，**不得**静默夹取。
	// maxSearchLimit is the contract ceiling; exceeding it is a malformed request.
	maxSearchLimit = 200
)

// errStoreContract 表示存储端口违约：返回了缺主键的命中，或条数超过请求上限。
// 两种都说明实现没有按端口契约工作，必须 fail-closed，**不得**照常返回可疑结果。
// errStoreContract marks a store that broke its port contract.
var errStoreContract = errors.New("history: message query store violated its contract")

// HistoryQuery 是检索的过滤条件与分页参数。
//
// `Limit` 对**存储端口**而言是原始行数上限（可能被 `Search` 加一用于探测是否还有
// 下一页），端口实现**不得**再对它做 1..200 的夹取——夹取会吞掉探测行，让截断
// 标注在边界值上失真。HTTP 面的 1..200 校验在领域层 `Search` 里完成。
//
// HistoryQuery carries the filters and the raw row cap; the store must not clamp it.
type HistoryQuery struct {
	// Keywords 是关键词（必填，去空白后非空）。
	Keywords string
	// Agent 是可选的 Agent 画像过滤（空 = 不过滤）。
	Agent string
	// SessionID 是可选的会话过滤（空 = 不过滤）。
	SessionID string
	// CallerID 是可选的调用方过滤；调用方身份只能填自己，管理员可跨调用方。
	CallerID string
	// From 是时间窗起点（含）；零值 = 不限。
	From time.Time
	// To 是时间窗终点（含）；零值 = 不限。
	To time.Time
	// Limit 是页大小；0 = 默认 50，上限 200。
	Limit int
}

// HistoryHit 是检索命中的**指针**：标识、元数据、片段与分数，**没有正文**。
// 它不能当引用用——要引用必须先经 `ExactReader.Read` 读原文并复核当前状态。
// HistoryHit is a pointer: ids, metadata, snippet and score, never full content.
type HistoryHit struct {
	// MessageID 是 `messages.id` 的十进制串（精确读取的主键）。
	MessageID string
	// SessionID 是命中所属会话。
	SessionID string
	// RunID 是产生该消息的 Run。
	RunID string
	// Role 取 user / assistant / tool / system。
	Role string
	// Snippet 是命中片段，**不是**引用依据。
	Snippet string
	// Score 是相关性分数（首批关键词 / trigram 路径的分数口径由实现方给出）。
	Score float64
	// CreatedAt 是消息创建时间（UTC）。
	CreatedAt time.Time
}

// SearchResult 是检索的一页结果与**预算标注**。
//
// `Truncated` 为真表示命中数超过本页上限、本页不是全部；调用方**必须**能看见它
// （HTTP 面映射成 `HistorySearchResult.truncated`），否则会以为已穷尽。
// SearchResult is one page plus the explicit truncation mark.
type SearchResult struct {
	// Hits 是本页命中（可能为空，非 nil）。
	Hits []HistoryHit
	// Truncated 表示还有更多命中未返回。
	Truncated bool
}

// SearcherDeps 是检索器的装配依赖。
type SearcherDeps struct {
	// Store 是历史回查存储端口；缺省即装配不可用（fail-closed）。
	Store MessageQueryStore
	// Logger 是运行日志出口；缺省用 `slog.Default()`。失败路径同时写 WARN
	// 日志与错误返回（可观测性双轨——读取不落审计，但不得静默）。
	Logger *slog.Logger
}

// Searcher 是历史检索器：把身份与查询结算成一次存储调用，并标注预算截断。
// Searcher is the history search entry point.
type Searcher struct {
	store  MessageQueryStore
	logger *slog.Logger
}

// NewSearcher 装配检索器。缺端口返回一个检索即失败的实例（`Search` 报
// `ErrHistoryUnavailable`），**不**在构造时 panic——装配层需要能启动后 fail-closed。
// NewSearcher assembles the searcher; a missing port fails at Search time.
func NewSearcher(deps SearcherDeps) *Searcher {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Searcher{store: deps.Store, logger: logger}
}

// Search 执行一次历史检索。
//
// 流程（与 §2.10.4 的检索行逐块对应）：
//  1. 结算归属范围（`resolveScope`）：零值 → 401、缺 scope 或越权指定 → 403；
//  2. 校验查询形状：关键词必填、时间窗不倒置、`Limit` 不越界 → `ErrInvalidQuery`；
//  3. 以 `limit+1` 条探测存储：多出来的那一条只用于判定"还有更多"；
//  4. 裁剪到 `limit` 条并置 `Truncated`——**不得**静默截断。
//
// Search resolves the scope, validates the query, probes one extra row and marks
// truncation explicitly.
func (s *Searcher) Search(ctx context.Context, p core.Principal, q HistoryQuery) (SearchResult, error) {
	if s == nil || s.store == nil {
		// 装配不可用：**不得**返回空结果冒充"查过了没有"。
		// A missing port must never look like an empty search.
		return SearchResult{}, ErrHistoryUnavailable
	}
	scope, err := resolveScope(p, q.CallerID)
	if err != nil {
		return SearchResult{}, err
	}
	limit, err := normalizeLimit(q.Limit)
	if err != nil {
		return SearchResult{}, err
	}
	probe, err := normalizeQuery(scope, q, limit)
	if err != nil {
		return SearchResult{}, err
	}
	hits, err := s.store.SearchMessages(ctx, scope, probe)
	if err != nil {
		// 失败路径：WARN 日志 + 错误上抛，两条轨都不省。
		// Log and return: the read path has no audit row, so silence is the only
		// thing that would make a failure invisible.
		s.logger.WarnContext(ctx, "history search failed",
			slog.String("session_id", probe.SessionID),
			slog.String("agent", probe.Agent),
			slog.Int("limit", limit),
			slog.String("error", err.Error()))
		return SearchResult{}, fmt.Errorf("history: search messages: %w", err)
	}
	if len(hits) > limit+1 {
		// 端口违约：请求了 limit+1 条却拿回更多，结果集不可信。
		// The store ignored the row cap, so the page cannot be trusted.
		err := fmt.Errorf("%w: got %d hits for limit %d", errStoreContract, len(hits), limit+1)
		s.logger.WarnContext(ctx, "history search contract violation", slog.String("error", err.Error()))
		return SearchResult{}, err
	}
	truncated := len(hits) > limit
	if truncated {
		hits = hits[:limit]
	}
	for i := range hits {
		if strings.TrimSpace(hits[i].MessageID) == "" {
			// 无主键的命中无法精确读取，照常返回会让调用方拿着死指针。
			// A hit without a primary key is a dead pointer.
			err := fmt.Errorf("%w: hit at position %d has no message id", errStoreContract, i)
			s.logger.WarnContext(ctx, "history search contract violation", slog.String("error", err.Error()))
			return SearchResult{}, err
		}
	}
	if hits == nil {
		// 契约要求 `hits` 永远是数组（JSON `[]`），nil 会被编码成 `null`。
		// The contract requires an array; nil would encode as null.
		hits = []HistoryHit{}
	}
	return SearchResult{Hits: hits, Truncated: truncated}, nil
}

// normalizeLimit 把页大小结算成契约取值：0 → 默认 50，负数与超上限 → 形状非法。
// normalizeLimit applies the contract default and rejects out-of-range values.
func normalizeLimit(limit int) (int, error) {
	switch {
	case limit == 0:
		return defaultSearchLimit, nil
	case limit < 0 || limit > maxSearchLimit:
		return 0, fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidQuery, maxSearchLimit)
	default:
		return limit, nil
	}
}

// normalizeQuery 校验查询并回填**生效**的调用方过滤与探测行数。
//
// 回填的 `CallerID` 取 `effectiveCallerFilter` 的结果：调用方身份恒为自己的归属，
// 管理员按请求给值——请求参数**不得**放大已经结算出的范围。
// normalizeQuery validates the query and fills the effective caller filter and the
// limit+1 probe, never widening the resolved scope.
func normalizeQuery(scope Scope, q HistoryQuery, limit int) (HistoryQuery, error) {
	if strings.TrimSpace(q.Keywords) == "" {
		// 关键词是契约必填项；空关键词等于"扫全表"，按形状非法拒绝。
		// Keywords are required by the contract; an empty one would scan everything.
		return HistoryQuery{}, fmt.Errorf("%w: keywords are required", ErrInvalidQuery)
	}
	if !q.From.IsZero() && !q.To.IsZero() && q.From.After(q.To) {
		// 倒置时间窗必然零命中，直接拒绝比返回空集更诚实。
		// An inverted window is a malformed request, not an empty result.
		return HistoryQuery{}, fmt.Errorf("%w: from must not be after to", ErrInvalidQuery)
	}
	q.CallerID = effectiveCallerFilter(scope, q.CallerID)
	q.Limit = limit + 1
	return q, nil
}
