package web

// 本文件是**管理面记忆**的读写端点（US-16 裁决 A / 颗粒度文档 C-21）：结构化读模型
// 两条（全局层 / Agent 层）与四个写动作（显式提议 / 确认 / 纠正 / 软删）。
//
// 与既有两个只读端点（`GET /memory`、`GET /agents/{name}/memory`）的关系：那两个返回
// **渲染文本**（注入面看到的两段），本文件返回**带主键与状态的条目**——没有主键页面
// 就没有可纠正 / 可删除的对象，没有状态就看不见候选与已删条目。两套读面并存，各自
// 服务不同用途，**不互相替代**。
//
// 权限：七条路由全部是管理面（admin token / local_trust）。调用方 Key **不得**读写
// 他人记忆层——Agent 自己的记忆只经 `save_memory` / `recall_memory` 在运行中访问。
// 写动作的 actor 由认证闸门注入，本层只做形状投影与错误分类映射；条目正文**不**进
// 日志（只记条目标识与状态）。
// The management-side memory surface: two structured read rows and the four explicit
// write actions. It coexists with the two rendered-text rows rather than replacing them,
// and every row is management-only — a caller key never reaches another layer's memory.

import (
	"errors"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// messageMemoryProposalBody 是提议请求体不是合法 JSON / 含未知字段时的固定文案。
	messageMemoryProposalBody = "请求体不是合法的记忆提议"
	// messageMemoryCorrectionBody 是纠正请求体的同口径固定文案。
	messageMemoryCorrectionBody = "请求体不是合法的记忆纠正"
	// messageMemoryEntryID 是路径里条目标识非法时的固定文案：用 404 而不是 400，
	// 免得"这个标识格式对不对"成为存在性探针。
	messageMemoryEntryID = "记忆条目标识不存在"
	// messageMemoryConfirmBody 是确认请求体不是合法 JSON / 含未知字段时的固定文案。
	messageMemoryConfirmBody = "请求体不是合法的记忆确认"
	// messageMemoryConfirmUnavailable 是未装配候选确认端口时的 503 文案：点名缺的是
	// **装配**，不是"服务器内部错误"——否则运维只会翻日志，不会去看装配。
	messageMemoryConfirmUnavailable = "记忆候选确认端口未装配：自动候选无法确认，请检查部署装配"
)

// handleAdminListGlobalMemoryEntries 处理 `GET /memory/entries`：全局层的结构化读模型。
// 与 `GET /memory` 的差别是**带主键与状态**——它是管理面的读，不是注入面的投影。
// handleAdminListGlobalMemoryEntries serves the global layer's structured read model.
func (s *Server) handleAdminListGlobalMemoryEntries(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	s.writeMemoryEntryList(w, r, core.GlobalScope)
}

// handleAdminListAgentMemoryEntries 处理 `GET /agents/{name}/memory/entries`：Agent 层
// 的结构化读模型。Agent 不存在或未注册 → 404（与查询页面同一处判定），**不**回退
// 全局层——回退只发生在提示词组装。
// handleAdminListAgentMemoryEntries serves one agent's structured read model; it never
// falls back to the global layer.
func (s *Server) handleAdminListAgentMemoryEntries(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	name := r.PathValue("name")
	if _, ok := s.registeredAgent(name); !ok {
		s.writeError(w, http.StatusNotFound, codeNotFound,
			"Agent 请求名不存在或未注册："+name)
		return
	}
	s.writeMemoryEntryList(w, r, core.Scope{Agent: name})
}

// writeMemoryEntryList 是两个列表端点的共同实现：状态过滤来自查询串，非法取值由
// 适配器按 `InvalidInputError` 报 400——**不**静默忽略（忽略会让页面以为"没有候选"）。
// writeMemoryEntryList is the shared list path; an invalid state filter is a 400 rather
// than a silently empty page.
func (s *Server) writeMemoryEntryList(w http.ResponseWriter, r *http.Request, scope core.Scope) {
	if s.opts.MemoryAdmin == nil {
		s.writeAdminPortMissing(w, "MemoryAdmin", r.Method+" "+r.URL.Path)
		return
	}
	filter := core.MemoryEntryFilter{Scope: scope, State: r.URL.Query().Get("state")}
	records, err := s.opts.MemoryAdmin.ListEntries(r.Context(), filter)
	if err != nil {
		s.writeMemoryAdminError(w, err, "列出记忆条目")
		return
	}
	entries := make([]MemoryEntryView, 0, len(records))
	for _, record := range records {
		entries = append(entries, memoryEntryView(record))
	}
	s.writeOK(w, MemoryEntryListView{
		Scope:   memoryScopeOf(scope),
		Agent:   scope.Agent,
		Entries: entries,
	})
}

// handleAdminProposeGlobalMemory 处理 `POST /memory/proposals`：全局层的显式提议。
// 新落库的行恒为 candidate——**确认**才是晋升路径，提议本身不进注入面；命中同源
// active 条目时回执如实给该行的状态（`merged=true`、`state=active`），不伪造成候选。
// handleAdminProposeGlobalMemory records an explicit proposal in the global layer.
func (s *Server) handleAdminProposeGlobalMemory(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	s.handleAdminProposeMemory(w, r, actor, core.GlobalScope)
}

// handleAdminProposeAgentMemory 处理 `POST /agents/{name}/memory/proposals`。
// handleAdminProposeAgentMemory records an explicit proposal in one agent's layer.
func (s *Server) handleAdminProposeAgentMemory(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if _, ok := s.registeredAgent(name); !ok {
		s.writeError(w, http.StatusNotFound, codeNotFound,
			"Agent 请求名不存在或未注册："+name)
		return
	}
	s.handleAdminProposeMemory(w, r, actor, core.Scope{Agent: name})
}

// handleAdminProposeMemory 是两条提议端点的共同实现：请求体只经
// `decodeAdminWriteBody` 解码（未知字段拒绝），分区缺省由适配器按归档区落。
// handleAdminProposeMemory is the shared proposal path; an absent section means the
// archive, decided by the adapter.
func (s *Server) handleAdminProposeMemory(w http.ResponseWriter, r *http.Request, actor core.Actor, scope core.Scope) {
	if s.opts.MemoryAdmin == nil {
		s.writeAdminPortMissing(w, "MemoryAdmin", "POST "+r.URL.Path)
		return
	}
	var body MemoryProposalWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageMemoryProposalBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	result, err := s.opts.MemoryAdmin.Propose(r.Context(), actor, core.MemoryProposal{
		Scope:   scope,
		Section: core.Section(body.Section),
		Content: body.Content,
	})
	if err != nil {
		s.writeMemoryAdminError(w, err, "提议记忆")
		return
	}
	s.writeOK(w, memoryEntryReceipt(result))
}

// handleAdminConfirmMemory 处理 `POST /memory/entries/{id}/confirm`：候选晋升的
// **唯一**用户可达路径。非候选目标由适配器按 `core.ErrConflict` 报 409，页面据此
// 提示"该条已不是候选"，**不**自动重放。
//
// 请求体**可空**（契约 MemoryConfirmWrite）：空 body 只表示 `keep_both`，**不**表示
// 跳过门禁——自动候选照样走统一确认事务。装配约束（§3.1.8）：自动候选必须经
// `core.MemoryConfirmResolver` 确认；未装配 resolver 时只有显式条目按旧 `Confirm`
// 执行，自动候选由适配器回 `core.ErrMemoryConfirmUnavailable`，本层映射 503
// `NOT_CONFIGURED`——**不得**退化成旧 Confirm 或 keep_both。请求带了处置字段而本
// 装配无法兑现时同样按 503 拒绝：静默忽略会让页面以为"替代已完成"。
// handleAdminConfirmMemory is the only user-reachable promotion path. The body is
// optional (keep_both), and an auto candidate without the optional resolver fails
// closed with 503 instead of degrading to the un-gated legacy Confirm.
func (s *Server) handleAdminConfirmMemory(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	entryID, ok := s.memoryEntryID(w, r)
	if !ok {
		return
	}
	if s.opts.MemoryAdmin == nil {
		s.writeAdminPortMissing(w, "MemoryAdmin", "POST "+r.URL.Path)
		return
	}
	var body MemoryConfirmWrite
	if err := s.decodeAdminWriteBodyOptional(w, r, &body, messageMemoryConfirmBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	request := core.MemoryConfirmRequest{
		EntryID:           entryID,
		Resolution:        body.Resolution,
		SupersedesEntryID: body.SupersedesEntryID,
	}
	resolver, ok := s.opts.MemoryAdmin.(core.MemoryConfirmResolver)
	if !ok {
		if request.Resolution != "" || request.SupersedesEntryID != 0 {
			s.writeMemoryAdminError(w, core.ErrMemoryConfirmUnavailable, "确认记忆")
			return
		}
		result, err := s.opts.MemoryAdmin.Confirm(r.Context(), actor, entryID)
		if err != nil {
			s.writeMemoryAdminError(w, err, "确认记忆")
			return
		}
		s.writeOK(w, memoryEntryReceipt(result))
		return
	}
	result, err := resolver.ConfirmWithResolution(r.Context(), actor, request)
	if err != nil {
		s.writeMemoryAdminError(w, err, "确认记忆")
		return
	}
	s.writeOK(w, memoryEntryReceipt(result))
}

// handleAdminCorrectMemory 处理 `POST /memory/entries/{id}/correct`：新条继承目标条的
// 分层与分区；目标已不在有效态时新条落 conflicted（领域层决定，回执的 `state` 如实
// 反映），页面据此提示"待人工裁决"而不是"已纠正"。
// handleAdminCorrectMemory reports the resulting state verbatim; a conflicted outcome
// is surfaced rather than rendered as a successful correction.
func (s *Server) handleAdminCorrectMemory(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	entryID, ok := s.memoryEntryID(w, r)
	if !ok {
		return
	}
	if s.opts.MemoryAdmin == nil {
		s.writeAdminPortMissing(w, "MemoryAdmin", "POST "+r.URL.Path)
		return
	}
	var body MemoryCorrectionWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageMemoryCorrectionBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	result, err := s.opts.MemoryAdmin.Correct(r.Context(), actor, core.MemoryCorrection{
		EntryID: entryID,
		Content: body.Content,
	})
	if err != nil {
		s.writeMemoryAdminError(w, err, "纠正记忆")
		return
	}
	s.writeOK(w, memoryEntryReceipt(result))
}

// handleAdminDeleteMemory 处理 `DELETE /memory/entries/{id}`：软删，行与来源保留，
// 立即不可召回。重复删除按幂等返回成功（回执如实给出 deleted 状态）。
// handleAdminDeleteMemory soft-deletes one entry; repeats stay idempotent.
func (s *Server) handleAdminDeleteMemory(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	entryID, ok := s.memoryEntryID(w, r)
	if !ok {
		return
	}
	if s.opts.MemoryAdmin == nil {
		s.writeAdminPortMissing(w, "MemoryAdmin", "DELETE "+r.URL.Path)
		return
	}
	result, err := s.opts.MemoryAdmin.Delete(r.Context(), actor, entryID)
	if err != nil {
		s.writeMemoryAdminError(w, err, "删除记忆")
		return
	}
	s.writeOK(w, memoryEntryReceipt(result))
}

// memoryEntryID 解析路径里的条目标识；非法按 404 处理（原因见 messageMemoryEntryID）。
// memoryEntryID parses the path id and answers 404 when it is not a positive integer.
func (s *Server) memoryEntryID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	entryID, ok := parseMemoryEntryID(r.PathValue("id"))
	if !ok {
		s.writeError(w, http.StatusNotFound, codeNotFound, messageMemoryEntryID)
		return 0, false
	}
	return entryID, true
}

// parseMemoryEntryID 复用写面既有的十进制正整数解析器（与版本号同形：ASCII 十进制、
// 正整数、允许前导零）。共用一份实现，**不**在此复制校验逻辑。
// parseMemoryEntryID reuses the existing positive-decimal parser rather than copying it.
func parseMemoryEntryID(raw string) (int64, bool) { return parseVersionID(raw) }

// writeMemoryAdminError 把记忆管理面的失败按类型映射为固定响应（只认 errors.Is /
// errors.As，**不**比对文本）：
//   - core.ErrMemoryEntryNotFound → 404 NOT_FOUND（条目不存在或已被清理）
//   - *core.InvalidInputError → 400 BAD_REQUEST（正文为空 / 状态取值域外 / 分区未知）
//   - core.ErrConflict → 409 CONFLICT（对非候选执行确认等状态冲突）
//   - core.ErrMemoryConfirmUnavailable → 503 NOT_CONFIGURED（自动候选缺候选确认端口）
//   - 其余 → 500 INTERNAL，底层原因只进日志
//
// 每条分支都经 `writeAdminDataError` 出站：`pending` 之后的失败带 `operation_id`，
// 页面据此去操作记录里查证这次写到底留下了什么。
// writeMemoryAdminError maps failures by sentinel and type only; post-pending failures
// carry the operation id so the page can verify what the write left behind.
func (s *Server) writeMemoryAdminError(w http.ResponseWriter, err error, action string) {
	switch {
	case errors.Is(err, core.ErrMemoryEntryNotFound):
		s.writeAdminDataError(w, http.StatusNotFound, codeNotFound, "记忆条目不存在", err)
	case core.IsInvalidInput(err):
		var invalidErr *core.InvalidInputError
		errors.As(err, &invalidErr)
		s.writeAdminDataError(w, http.StatusBadRequest, codeBadRequest, invalidErr.Reason, err)
	case errors.Is(err, core.ErrConflict):
		s.writeAdminDataError(w, http.StatusConflict, codeConflict,
			"记忆条目的当前状态不允许本次操作", err)
	case errors.Is(err, core.ErrMemoryConfirmUnavailable):
		// 装配缺口只在服务端日志里带底层原因；响应是固定文案，不泄露装配细节。
		s.logger().Error("记忆候选确认端口未装配，拒绝确认自动候选",
			"action", action, "error", err)
		s.writeAdminDataError(w, http.StatusServiceUnavailable, codeNotConfigured,
			messageMemoryConfirmUnavailable, err)
	default:
		s.logger().Error("记忆管理写失败", "action", action, "error", err)
		s.writeAdminDataError(w, http.StatusInternalServerError, codeInternal, messageInternal, err)
	}
}

// memoryScopeOf 把分层投影成视图里的 `scope` 字面量（global | agent，与既有
// `MemoryView.scope` 枚举同一批取值）。
// memoryScopeOf projects the layer onto the shared scope literal.
func memoryScopeOf(scope core.Scope) string {
	if scope.Agent == "" {
		return memoryScopeGlobal
	}
	return memoryScopeAgent
}

// memoryEntryView 把一条领域读模型投影成出站视图。时间统一按 RFC3339 UTC 输出，
// 与既有视图同一写法。
// memoryEntryView projects one record into the outbound view.
func memoryEntryView(record core.MemoryEntryRecord) MemoryEntryView {
	return MemoryEntryView{
		EntryID:      record.EntryID,
		Scope:        memoryScopeOf(record.Scope),
		Agent:        record.Scope.Agent,
		Section:      string(record.Section),
		State:        record.State,
		Content:      record.Content,
		Source:       record.Source,
		SupersedesID: record.SupersedesID,
		CreatedAt:    formatTime(record.CreatedAt),
		Candidate:    memoryCandidateView(record.Candidate),
	}
}

// memoryCandidateView 把候选元数据投影成出站视图；nil 原样输出 `null`（非自动候选）。
// `evidence` 与 `source_entry_ids` **恒为数组**：空集合输出 `[]` 而不是 null，页面
// 不用区分两种"没有"。来源调用方 / Key 不投影——证据复核只需要消息与引文。
// memoryCandidateView projects the metadata; arrays stay non-nil and the origin caller
// and key never leave the process.
func memoryCandidateView(meta *core.MemoryCandidateMeta) *MemoryCandidateView {
	if meta == nil {
		return nil
	}
	evidence := make([]MemoryEvidenceView, 0, len(meta.Evidence))
	for _, item := range meta.Evidence {
		evidence = append(evidence, MemoryEvidenceView{
			MessageID: item.MessageID,
			RunID:     item.RunID,
			Role:      item.Role,
			Quote:     item.Quote,
			MessageAt: formatTime(item.MessageAt),
		})
	}
	sourceIDs := make([]int64, 0, len(meta.SourceEntryIDs))
	sourceIDs = append(sourceIDs, meta.SourceEntryIDs...)
	return &MemoryCandidateView{
		ExtractionID:          meta.ExtractionID,
		Kind:                  meta.Kind,
		Certainty:             meta.Certainty,
		Confidence:            meta.Confidence,
		Evidence:              evidence,
		SourceEntryIDs:        sourceIDs,
		SuggestedSupersedesID: meta.SuggestedSupersedesID,
		EvidenceAt:            formatTime(meta.EvidenceAt),
		// 空串 = 无到期：格式化的零值时间就是空串，不需要第二个哨兵。
		ValidTo:        formatTime(meta.ValidTo),
		TimeConfidence: meta.TimeConfidence,
	}
}

// memoryEntryReceipt 把写回执投影成视图：页面据此只刷新受影响的行。
// `superseded_id` 恒在：0 明确表示"本次没有替代任何条目"。
// memoryEntryReceipt projects a write receipt so the page can refresh one row.
func memoryEntryReceipt(result core.MemoryEntryResult) MemoryEntryReceipt {
	return MemoryEntryReceipt{
		EntryID:      result.EntryID,
		State:        result.State,
		ReplacedID:   result.ReplacedID,
		Merged:       result.Merged,
		SupersededID: result.SupersededID,
	}
}
