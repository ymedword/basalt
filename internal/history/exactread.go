package history

// 本文件实现历史**精确读取**（已知位置）（US-16 §3.1.10 / C-26 / T054）：结算归属 →
// 分类标识 → 复核会话归属 → 取原文。
//
// 四条硬约束：
//  1. **跨调用方一律 404**（C-28）：归属过滤由 `Scope` 在存储层完成，读不到的一律
//     `ErrNotFound`；**不得**报 403——403 会泄露"这个会话 / 这条消息存在"。
//  2. **无状态临时标识不是主键**：`msg/<run>/<pos>`（`core.StatelessMessageID`）与
//     任何非十进制串在 `messages` 表都没有对应行，**必须**在访问存储之前直接
//     `ErrNotFound`，**不得**拿它去查库（"查过但没有"与"这根本不是主键"是两件事）。
//  3. **不冒充旧引用的新版本**（C-26）：`MessageView.EvidenceRefs` 原样透传，本包
//     **不**读取知识库、**不**把引用解析成最新版本；有效性标注归能读知识库的调用方。
//  4. **端口违约 fail-closed**：端口没给回请求的那一条（0 条 / 多条 / 标识不符 /
//     会话不符）都是实现没按契约工作，**不得**把可疑结果当原文返回。
//
// This file implements exact reads: resolve the scope, classify the identifier,
// re-check the session ownership, then load the row. Cross-caller reads are 404,
// stateless ids never reach the store, and port violations fail closed.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ymedword/basalt/internal/core"
)

// ExactReader 是历史精确读取器：把身份与消息标识结算成一次存储调用。
// ExactReader reads one message by primary key on behalf of one principal.
type ExactReader struct {
	store  MessageQueryStore
	logger *slog.Logger
}

// NewExactReader 装配精确读取器（签名冻结于设计 §3.1.10）。缺端口返回一个读取即
// 失败的实例（`Read` 报 `ErrHistoryUnavailable`），**不**在构造时 panic——装配层
// 需要能启动后 fail-closed。
// NewExactReader assembles the reader; a missing port fails at Read time.
func NewExactReader(store MessageQueryStore) *ExactReader {
	return &ExactReader{store: store, logger: slog.Default()}
}

// Read 精确读取一条消息。
//
// 流程：
//  1. 结算归属范围（`resolveScope`）：零值 → 401、缺 `sessions:read` → 403；
//  2. 分类标识：非十进制串（含无状态字面量 `msg/<run>/<pos>`）→ `ErrNotFound`，
//     **不**访问存储；
//  3. 给了会话时先 `ResolveSession` 复核归属：会话不存在或不属于本调用方 →
//     `ErrNotFound`（跨调用方不得 403）；
//  4. `ReadMessages` 取原文，并逐条核对端口契约。
//
// Read resolves the scope, classifies the id, re-checks the session and loads the row.
func (r *ExactReader) Read(ctx context.Context, p core.Principal, ref MessageRef) (MessageView, error) {
	if r == nil || r.store == nil {
		// 装配不可用：**不得**返回零值视图冒充"读到了空消息"。
		// A missing port must never look like an empty message.
		return MessageView{}, ErrHistoryUnavailable
	}
	scope, err := resolveScope(p, "")
	if err != nil {
		return MessageView{}, err
	}
	if !isMessagePrimaryKey(ref.MessageID) {
		// 无状态标识 / 空串 / 任意非十进制串都不是 `messages.id`：
		// 这是"没有这条消息"，不是"形状非法"——契约对该端点没有 400。
		// A stateless or non-decimal id simply has no row; the endpoint has no 400.
		return MessageView{}, ErrNotFound
	}
	if ref.SessionID != "" {
		// 先复核会话：会话不属于本调用方时统一 404，且不必再查消息表。
		// Resolve the session first so an alien session is a clean 404.
		session, err := r.store.ResolveSession(ctx, scope, ref.SessionID)
		if err != nil {
			return MessageView{}, r.wrap(ctx, "resolve session", ref, err)
		}
		if session.SessionID != ref.SessionID {
			err := fmt.Errorf("%w: resolved session %q for requested %q", errStoreContract, session.SessionID, ref.SessionID)
			r.logWarn(ctx, "history exact read contract violation", ref, err)
			return MessageView{}, err
		}
	}
	views, err := r.store.ReadMessages(ctx, scope, []MessageRef{ref})
	if err != nil {
		return MessageView{}, r.wrap(ctx, "read messages", ref, err)
	}
	// 端口契约：一条请求恰好一条视图，且标识与（给了会话时的）会话逐字相符。
	// The port must return exactly the row that was asked for.
	if len(views) != 1 {
		err := fmt.Errorf("%w: got %d views for one ref", errStoreContract, len(views))
		r.logWarn(ctx, "history exact read contract violation", ref, err)
		return MessageView{}, err
	}
	view := views[0]
	if view.MessageID != ref.MessageID {
		err := fmt.Errorf("%w: view carries message id %q for requested %q", errStoreContract, view.MessageID, ref.MessageID)
		r.logWarn(ctx, "history exact read contract violation", ref, err)
		return MessageView{}, err
	}
	if ref.SessionID != "" && view.SessionID != ref.SessionID {
		err := fmt.Errorf("%w: view carries session id %q for requested %q", errStoreContract, view.SessionID, ref.SessionID)
		r.logWarn(ctx, "history exact read contract violation", ref, err)
		return MessageView{}, err
	}
	return view, nil
}

// wrap 记录失败日志并原样包装错误：`errors.Is` 仍可判定 `ErrNotFound` / `ErrDeleted`
// 等哨兵；可观测性双轨要求失败路径既有日志也有错误返回。
// wrap logs and wraps the store error without hiding its sentinel.
func (r *ExactReader) wrap(ctx context.Context, op string, ref MessageRef, err error) error {
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDeleted) || errors.Is(err, ErrForbidden) {
		// 这三种是**读取结论**，不是故障：不写 WARN（否则正常 404 会淹掉真故障）。
		// Verdicts are not failures; only unexpected store errors are logged.
		return err
	}
	wrapped := fmt.Errorf("history: %s: %w", op, err)
	r.logWarn(ctx, "history exact read failed: "+op, ref, wrapped)
	return wrapped
}

// logWarn 输出失败路径的 WARN 日志（只带消息标识与错误，不带正文）。
// logWarn emits the failure-path log line; content never appears here.
func (r *ExactReader) logWarn(ctx context.Context, msg string, ref MessageRef, err error) {
	logger := r.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.WarnContext(ctx, msg,
		slog.String("session_id", ref.SessionID),
		slog.String("message_id", ref.MessageID),
		slog.String("error", err.Error()))
}

// isMessagePrimaryKey 判定标识是否是 `messages.id` 的十进制串。
//
// 只接受纯十进制：`messages.id` 是 bigint 自增，无状态 Run 的临时字面量
// `msg/<run>/<pos>` 与任何其它形状都**不可能**是主键。空串同样不算——空标识既
// 不能定位也不该被当成"查全部"。
// isMessagePrimaryKey accepts only decimal ids; the stateless `msg/<run>/<pos>`
// literal can never be a primary key.
func isMessagePrimaryKey(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
