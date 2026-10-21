package web

// 本文件实现 Agent 启停端点（颗粒度文档 §3.1.7）：`POST /agents/{name}/disable` 与
// `POST /agents/{name}/enable`。handler 只做「静态解码 → 存在性 → 委托
// core.AgentAdmin.Transition → 投影」；写锁、预写、状态文件原子写与索引同步全部在
// 组合适配器（internal/app/agent_console.go）内完成（图 C）。
// This file implements the enable/disable endpoints. The handler decodes, checks
// existence, delegates to core.AgentAdmin.Transition and projects the record; the
// write lock, the pending pre-write, the state file and the index live in the adapter.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// messageLifecycleBody 是启停请求体不是合法 JSON 时的固定文案：解析器细节不外传
// （约束 45）。字段级校验失败另有点名文案。
// The fixed text for an unparsable body; field-level failures carry their own names.
const messageLifecycleBody = "请求体不是合法的生命周期迁移请求"

// enableRejectedReason 是启用被拒、而装载结果没有点名原因时的固定兜底（未验证态）。
// Fallback reason when the loader entry carries no reject detail.
const enableRejectedReason = "加载期校验或资源引用未通过"

// lifecycleTransitionBody 用指针区分「字段缺失 / null」与合法零值：两个字段都必填，
// expected_generation 为 0 也必须显式给出。
// Pointer fields distinguish a missing field from a legitimate zero value.
type lifecycleTransitionBody struct {
	ExpectedState      *string `json:"expected_state"`
	ExpectedGeneration *int64  `json:"expected_generation"`
}

// handleDisableAgent 停用一个 Agent：停用只拒绝新准入，**不取消**在途运行——本层
// 不触达任何运行端口，取消语义不在这里（约束 16）。
// handleDisableAgent stops new admissions only; it never touches a run port.
func (s *Server) handleDisableAgent(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	s.transitionAgent(w, r, actor, core.AgentDisabled)
}

// handleEnableAgent 启用一个 Agent：除状态文件前置条件外，加载期校验与资源引用
// 必须当前仍通过（约束 16）——未注册的装载条目在这里返回 409，不让"永远跑不起来"
// 的 Agent 被打开。
// handleEnableAgent additionally requires the loader entry to be registered, so an
// agent that could never run is never switched on.
func (s *Server) handleEnableAgent(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	s.transitionAgent(w, r, actor, core.AgentActive)
}

// transitionAgent 是两个启停端点的共同骨架：解码（400，零副作用）→ 存在性（404）
// → 启用前置校验（409）→ 委托适配器 → 视图或错误映射。
// transitionAgent is the shared skeleton: decode, existence, enable precondition,
// delegation, then the view or the mapped failure.
func (s *Server) transitionAgent(w http.ResponseWriter, r *http.Request, actor core.Actor, target core.AgentState) {
	if s.opts.AgentAdmin == nil {
		s.logger().Error("装配缺少 AgentAdmin 端口", "agent", r.PathValue("name"))
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}

	name := r.PathValue("name")
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxRequestBytes)
	expected, err := decodeLifecycleTransition(r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusBadRequest, codeBadRequest,
				fmt.Sprintf("请求体超过 %d 字节上限", s.opts.MaxRequestBytes))
			return
		}
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	// 存在性按装载结果判定（含未注册项）：契约要求「Agent 不存在 → 404」。状态文件
	// 缺失是另一回事——那只说明文件坏了，由适配器映射成 409 AGENT_UNAVAILABLE。
	// Existence comes from the loader result (rejects included); a missing state file
	// stays a 409 raised by the adapter.
	entry, exists := s.lifecycleEntry(name)
	if !exists {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到 Agent："+name)
		return
	}
	if target == core.AgentActive && !entry.Registered {
		reason := entry.RejectReason
		if reason == "" {
			reason = enableRejectedReason
		}
		s.logger().Warn("启用被拒：加载期校验或资源引用未通过", "agent", name, "reason", reason)
		s.writeError(w, http.StatusConflict, codeAgentUnavailable,
			fmt.Sprintf("Agent %s 当前不可启用（%s）", name, reason))
		return
	}
	if target == core.AgentActive {
		// 约束 16：启用要求「加载期校验与资源引用当前仍通过」。注册只证明工作副本
		// 可解析（约束 36），不证明引用的 Provider / MCP / 技能仍有已发布版本——
		// 发布指针可能已被删除，或技能刚被编辑成未发布状态。未确认即拒绝，避免
		// 打开一个准入必然失败的 Agent。资源清单读不到同样失败关闭。
		// Requirement 16: enable re-checks that every referenced resource still has a
		// published version; an unreadable index fails closed the same way.
		if reason := s.enableResourceRejection(r.Context(), entry); reason != "" {
			s.logger().Warn("启用被拒：资源引用无已发布版本", "agent", name, "reason", reason)
			s.writeError(w, http.StatusConflict, codeAgentUnavailable,
				fmt.Sprintf("Agent %s 当前不可启用（%s）", name, reason))
			return
		}
	}

	rec, err := s.opts.AgentAdmin.Transition(r.Context(), name, target, expected, actor)
	if err != nil {
		s.writeLifecycleError(w, err, name, target)
		return
	}
	s.writeOK(w, agentLifecycleView(rec))
}

// enableResourceRejection 返回启用前置校验的固定拒绝原因；空串表示通过。资源视图
// 读不到 → 不可确认（resourceViewsProblem）；引用缺已发布版本 → 点名缺失项。
// 两种情况都映射为 409 AGENT_UNAVAILABLE，底层错误只进 WARN 日志（约束 45）。
// enableResourceRejection returns the fixed refusal reason ("" passes); the raw index
// error stays in the WARN log.
func (s *Server) enableResourceRejection(ctx context.Context, entry core.AgentEntry) string {
	if entry.Profile == nil {
		return enableRejectedReason
	}
	index, err := s.publishedResourceIndex(ctx)
	if err != nil {
		s.logger().Warn("读取已发布资源视图失败，启用按不可确认处理", "agent", entry.Name, "error", err)
		return resourceViewsProblem
	}
	missing := missingPublishedResources(entry, index)
	if len(missing) == 0 {
		return ""
	}
	return missingPublishedPrefix + strings.Join(missing, "、")
}

// decodeLifecycleTransition 解析启停请求体：未知字段拒绝（管理写不接受拼错的键），
// 两个字段必填，expected_state 只接受 active / disabled——archived 不是启停可达的
// 目标态。错误文案是本层自产的固定文本，解析器原文不外传。
// decodeLifecycleTransition rejects unknown or misspelled keys, requires both fields,
// and accepts only active/disabled; the decoder's raw text never leaves this layer.
func decodeLifecycleTransition(r *http.Request) (core.AgentLifecycleTransition, error) {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body lifecycleTransitionBody
	if err := dec.Decode(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return core.AgentLifecycleTransition{}, err
		}
		return core.AgentLifecycleTransition{}, errors.New(messageLifecycleBody)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return core.AgentLifecycleTransition{}, err
		}
		return core.AgentLifecycleTransition{}, errors.New(messageLifecycleBody)
	}
	if body.ExpectedState == nil {
		return core.AgentLifecycleTransition{}, errors.New("expected_state 必填")
	}
	if body.ExpectedGeneration == nil {
		return core.AgentLifecycleTransition{}, errors.New("expected_generation 必填")
	}
	state := core.AgentState(*body.ExpectedState)
	switch state {
	case core.AgentActive, core.AgentDisabled:
	default:
		return core.AgentLifecycleTransition{}, fmt.Errorf(
			"expected_state 只接受 active / disabled，实际 %q", *body.ExpectedState)
	}
	return core.AgentLifecycleTransition{
		ExpectedState:      state,
		ExpectedGeneration: *body.ExpectedGeneration,
	}, nil
}

// lifecycleEntry 按名字查装载结果：先查不可变注册表（已注册，热加载后的当前快照），
// 再退回装载结果里的未注册条目——启用校验需要后者点名拒绝原因。两者都没有即
// 「Agent 不存在」。
// lifecycleEntry prefers the live registry and falls back to the loader result so an
// unregistered entry can still name its rejection reason.
func (s *Server) lifecycleEntry(name string) (core.AgentEntry, bool) {
	if s.opts.Registry != nil {
		if entry, ok := s.opts.Registry.Lookup(name); ok {
			return entry, true
		}
	}
	for _, entry := range s.opts.Agents.Profiles() {
		if entry.Name == name {
			return entry, true
		}
	}
	return core.AgentEntry{}, false
}

// writeLifecycleError 把适配器的失败按类型映射为固定响应（错误表）：类型判定
// 只认 errors.Is / errors.As，**不**比对文本（EngineeringStandards §1.4）。
//   - *core.InvalidInputError → 400 BAD_REQUEST（点名哪个入参）
//   - *core.AgentNotFoundError → 404 NOT_FOUND
//   - *core.AgentUnavailableError → 409 AGENT_UNAVAILABLE（自产原因，不含路径）
//   - core.ErrLifecycleConflict → 409 CONFLICT（期望状态 / 代数不符，零操作行）
//   - 其余（含预写失败）→ 500 INTERNAL，固定文案，底层原因只进日志
//
// writeLifecycleError maps adapter failures by type, never by text; anything else is
// a fixed 500 whose cause stays in the log.
func (s *Server) writeLifecycleError(w http.ResponseWriter, err error, agent string, target core.AgentState) {
	var invalidErr *core.InvalidInputError
	if errors.As(err, &invalidErr) {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, invalidErr.Reason)
		return
	}
	var notFoundErr *core.AgentNotFoundError
	if errors.As(err, &notFoundErr) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到 Agent："+notFoundErr.Name)
		return
	}
	var unavailableErr *core.AgentUnavailableError
	if errors.As(err, &unavailableErr) {
		s.writeError(w, http.StatusConflict, codeAgentUnavailable, unavailableErr.Error())
		return
	}
	if errors.Is(err, core.ErrLifecycleConflict) {
		s.writeError(w, http.StatusConflict, codeConflict, err.Error())
		return
	}
	s.logger().Error("Agent 启停失败", "agent", agent, "target", string(target), "error", err)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}
