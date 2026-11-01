package web

// 本文件是 `GET /api/v1/compactions` 的 HTTP 层（US-16 / T089）：压缩链的**只读
// 派生视图**。判据逐条取自冻结契约 `docs/contracts/openapi.yaml`：
//
//  1. **管理面只读**：根 security 只有 `bearerAuth` / `localTrust`，没有 `callerKey`
//     ——本行是管理行（`adminHandler`），不声明 scope、不映射 ingress。读取面**不写**
//     管理操作审计（run-and-errors §9.3）。
//  2. **`session_id` 与 `run_id` 恰给一个**：都给或都不给 → 400，且在触达端口之前
//     返回——形状非法时不去读库。
//  3. **`limit` 边界**：minimum 1 / maximum 200 / default 50；越界或非整数 → 400。
//  4. **装配缺失按失败关闭**：端口 nil 固定 500 + ERROR 日志，**不得**回 200 +
//     空数组——那会把装配故障显示成"从未压缩"。
//  5. **取消不算失败**：`context.Canceled` 只记 WARN、不写响应（连接已断）。
//
// This file is the HTTP layer of the read-only compaction view: management surface,
// exactly one scope, contract limit bounds checked before the port, and a nil port
// failing closed rather than faking "never compacted".

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/ymedword/basalt/internal/compaction"
	"github.com/ymedword/basalt/internal/core"
)

// 契约 `GET /compactions` 的 `limit` 边界（minimum 1 / maximum 200 / default 50）。
// web 层复刻这三项只为在触达端口之前 400 / 补默认；存储侧再判一次下界（失败关闭），
// 两处口径以契约为准。
// The contract's limit bounds; the store re-checks the lower bound.
const (
	defaultCompactionChainLimit = 50
	maxCompactionChainLimit     = 200
)

// 压缩视图固定文案（契约 §8：400 只说明"请求形状非法"，不回显内部规则细节）。
// Fixed compaction copy; a shape denial never explains internal rules.
const (
	messageCompactionScopeRequired = "session_id 与 run_id 必须恰给一个"
	messageCompactionLimitRange    = "limit 必须是 1 到 200 之间的整数"
)

// handleListCompactions 处理 `GET /api/v1/compactions`：按 scope 返回**当前
// `chain_epoch`** 的链（含 failed / stale 与原因码），次序与截断由存储层固定，
// 本层不重排、不筛状态、不合并两个引用集合。
// handleListCompactions reads the current generation; ordering and truncation are the
// store's job, and this layer never re-sorts or drops statuses.
func (s *Server) handleListCompactions(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.CompactionChain == nil {
		s.writeCompactionPortMissing(w, "GET /compactions")
		return
	}
	scope, ok := s.parseCompactionScope(w, r)
	if !ok {
		return
	}
	limit := defaultCompactionChainLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxCompactionChainLimit {
			s.writeError(w, http.StatusBadRequest, codeBadRequest, messageCompactionLimitRange)
			return
		}
		limit = parsed
	}
	entries, err := s.opts.CompactionChain.ListCurrentChain(r.Context(), scope, limit)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			s.logger().Warn("读取压缩链已取消", "scope", scope.Canonical(), "actor", actor.Subject)
			return
		}
		s.logger().Error("读取压缩链失败", "scope", scope.Canonical(), "actor", actor.Subject, "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	s.writeOK(w, compactionViews(entries))
}

// parseCompactionScope 解析"恰给一个"的 scope 参数：两个都给或都不给都是 400。
// 契约没有"都为空 = 列全部"的读法——压缩条目按会话 / 无状态 Run 分范围存储，
// 跨范围的合并列表既不满足排序口径（每个范围各有自己的当前代次），也超出本端点
// 要回答的问题（"这段对话被压缩过哪些次"）。
// parseCompactionScope enforces exactly one scope id; there is no cross-scope list.
func (s *Server) parseCompactionScope(w http.ResponseWriter, r *http.Request) (compaction.Scope, bool) {
	q := r.URL.Query()
	sessionID, runID := q.Get("session_id"), q.Get("run_id")
	if (sessionID == "") == (runID == "") {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageCompactionScopeRequired)
		return compaction.Scope{}, false
	}
	return compaction.Scope{SessionID: sessionID, RunID: runID}, true
}

// writeCompactionPortMissing 是装配缺失的失败关闭出口：ERROR 日志 + 固定 500。
// 与 `writeHistoryPortMissing` 同一口径，端口名进日志、不进响应体。
// writeCompactionPortMissing fails closed with a fixed 500 and names the port in the log.
func (s *Server) writeCompactionPortMissing(w http.ResponseWriter, endpoint string) {
	s.logger().Error("装配缺少压缩链端口", "port", "CompactionChain", "endpoint", endpoint)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}
