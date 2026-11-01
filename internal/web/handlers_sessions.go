package web

// 本文件是会话与运行入口的 handler（模块卡 §3.1）：HTTP 层只做解码、边界判定与
// 结果映射——**不写消息、不执行工具、不另起循环、不持有准入状态**；会话准入（忙集、
// 实例上限、归档互斥）与 Run 账本全部由 RunService 背后的 AgentService 完成
// （宪法 II / V，约束 40）。
// Session and run handlers; the HTTP layer decodes, validates, and maps results only.
// Session admission and the run ledger belong to the RunService behind it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/callers"
	"github.com/ymedword/basalt/internal/core"
)

// 固定字面量（模块卡 §3.1；契约以 docs/contracts/openapi.yaml 为准）。
const (
	maxMessageBytes = 32 * 1024
	historyLimit    = 100
	channelWeb      = "web"
	defaultUserID   = "anonymous"
	statusActive    = "active"
	statusArchived  = "archived"
	triggerAPI      = "api"
)

// handleCreateSession 派生 id 后按 (channel,user_id,agent) 建行；新建时 rec.Status
// 显式置 active（契约 SessionSummary.status 枚举），不依赖 SQLite DEFAULT 的回读。
// 参数校验失败（agent 缺失 / null）在创建之前返回 400：零创建、零修改。
// `caller_id` 写入 `principal.CallerID`（C-28）；命中**他人**已存在的会话统一 404
// ——既不能把自己的会话名让给别人，也不泄露该会话是否存在。
// handleCreateSession derives the session id, writes the owner, and answers 404 for a
// session owned by another caller.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	var body struct {
		Agent   *string `json:"agent"`
		Channel *string `json:"channel"`
		UserID  *string `json:"user_id"`
	}
	if !s.decodeBody(w, r, &body) {
		return
	}
	if body.Agent == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "agent 必填")
		return
	}
	if _, ok := s.registeredAgent(*body.Agent); !ok {
		s.writeError(w, http.StatusNotFound, codeNotFound,
			"Agent 请求名不存在或未注册："+*body.Agent)
		return
	}

	channel := channelWeb
	if body.Channel != nil && *body.Channel != "" {
		channel = *body.Channel
	}
	userID := defaultUserID
	if body.UserID != nil && *body.UserID != "" {
		userID = *body.UserID
	}

	now := s.now().UTC()
	rec := core.SessionRecord{
		ID:           core.DeriveSessionID(channel, userID, *body.Agent),
		ProfileName:  *body.Agent,
		Channel:      channel,
		UserID:       userID,
		CallerID:     principal.CallerID,
		Status:       statusActive,
		CreatedAt:    now,
		LastActiveAt: now,
	}
	got, _, err := s.opts.Sessions.GetOrCreateSession(r.Context(), rec)
	if err != nil {
		s.writeStoreError(w, err, rec.ID)
		return
	}
	if !principalOwnsSession(principal, got) {
		// 同一 (channel,user_id,agent) 已属于别的调用方：按"不存在"回答，**不得**
		// 返回该行摘要（那样等于把他人会话的元数据借给新调用方）。
		// A row owned by someone else is reported as missing, never summarised.
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到会话："+rec.ID)
		return
	}
	s.writeOK(w, sessionSummaryView(got))
}

// principalOwnsSession 判定身份能否访问该会话：内置管理员恒真；调用方必须与
// `sessions.caller_id` 逐字相等。空归属（迁移遗留 / 管理面创建）**不**对调用方
// 放行——缺失归属按"不是你的"处理，而不是按"无主"处理（C-28）。
// principalOwnsSession admits the admin and the exact owner only; a missing owner is
// treated as someone else's, never as unowned.
func principalOwnsSession(principal core.Principal, rec core.SessionRecord) bool {
	if principal.Kind == core.PrincipalLocalAdmin {
		return true
	}
	return rec.CallerID != "" && rec.CallerID == principal.CallerID
}

// handleGetSession 返回会话摘要 + 最近 100 条消息（按 seq 升序）；归档会话照读。
// 窗口大小固定，请求不得派生排序 / 分页参数（约束 15）。
// handleGetSession returns the summary plus the latest 100 messages; archived
// sessions stay readable.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.opts.Sessions.GetSession(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err, id)
		return
	}
	msgs, err := s.opts.Sessions.MessagesTail(r.Context(), id, historyLimit)
	if err != nil {
		s.writeStoreError(w, err, id)
		return
	}
	s.writeOK(w, sessionDetailView(rec, msgs))
}

// decodeBody 用 MaxBytesReader 包体，解码一次并要求读到 EOF（尾随内容 → 400）。
// 不强制 Content-Type；未知字段忽略；必填字段用指针区分缺失 / null 与空串（约束 33）。
// 请求体超限返回 400（注 1）——不把 MaxBytesReader 的 413 泄漏给客户端。
// decodeBody bounds the body, decodes once, and requires EOF; oversized bodies are
// 400, never 413.
func (s *Server) decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.opts.MaxRequestBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.writeError(w, http.StatusBadRequest, codeBadRequest,
				fmt.Sprintf("请求体超过 %d 字节上限", s.opts.MaxRequestBytes))
			return false
		}
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "请求体不是合法 JSON 对象")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		// 对象之后还有内容（第二份 JSON 或垃圾）→ 400；只有空白到尾才算读完。
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "请求体不是合法 JSON 对象")
		return false
	}
	return true
}

// writeStoreError 把 fs.ErrNotExist 映射为 404（回显会话标识），其余 500。
// 底层错误只进 ERROR 日志，不进响应（约束 23）。
// writeStoreError maps a store miss to 404 and everything else to 500.
func (s *Server) writeStoreError(w http.ResponseWriter, err error, sessionID string) {
	if errors.Is(err, fs.ErrNotExist) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到会话："+sessionID)
		return
	}
	s.logger().Error("会话仓储操作失败", "session_id", sessionID, "error", err)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// handlePostMessage 是发消息闭环：校验（含 32 KiB 运行前判定）→ 读会话 → 运行 → 映射。
// 归档与在途的互斥由 AgentService 判定（RequireExistingSession；归档会话返回
// *SessionArchivedError → 400）。客户端在运行期间断开时不写响应（写已断开的连接
// 只会得到 broken pipe）；HTTP 层不写消息、不写审计——Run 行与消息追加都在
// AgentService 内（宪法 II / V）。
// handlePostMessage validates, reads the session, runs, and maps; admission and
// archiving are decided by the service, and a disconnected client gets no response.
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	id := r.PathValue("id")
	var body struct {
		Message *string `json:"message"`
	}
	if !s.decodeBody(w, r, &body) {
		return
	}
	if body.Message == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "message 必填")
		return
	}
	// 按 UTF-8 字节计数（`len(string)` 即字节数）；判定必须先于运行（约束 16）。
	if len(*body.Message) > maxMessageBytes {
		s.writeError(w, http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("消息超过 %d 字节上限", maxMessageBytes))
		return
	}

	// 读面只取 ProfileName / UserID；未命中 → 既有 404 文案。归属校验**先于**任何
	// 运行副作用：跨调用方与"不存在"共用同一 404，不泄露会话存在（C-28）。
	// The ownership check runs before any run side effect and shares the 404 shape with
	// a missing session.
	rec, err := s.opts.Sessions.GetSession(r.Context(), id)
	if err != nil {
		s.writeStoreError(w, err, id)
		return
	}
	if !principalOwnsSession(principal, rec) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到会话："+id)
		return
	}

	runCtx, cancel := context.WithTimeout(r.Context(), s.opts.RunTimeout)
	defer cancel()
	started := s.now()
	req := core.RunRequest{
		// 身份由业务认证中间件显式传入（T025）：**不得**在这里补默认值，也不得
		// 放进 context（宪法 VII / C-28）。
		// The principal is the middleware's explicit parameter, never a default here.
		Principal:              principal,
		Agent:                  rec.ProfileName,
		SessionID:              id,
		Message:                *body.Message,
		Channel:                channelWeb,
		UserID:                 rec.UserID,
		Budget:                 s.opts.RunBudget,
		RequireExistingSession: true,
		// 服务端生成的追踪标识（A-F2 / C-F02）：Run 准入在账本创建之前拒绝时，
		// 管理端只能靠它定位事件。来源是 recoverMiddleware 注入的 context 值，
		// **不读**客户端请求头（否则调用方可伪造关联）。
		// The server-generated tracing id, never taken from a client header.
		RequestID: requestIDFrom(r.Context()),
	}
	res, err := s.opts.Runs.Run(runCtx, req)
	if r.Context().Err() != nil {
		// 客户端已断开也必须留下 failed 诊断：否则「运行失败但原因未入库」的
		// 唯一线索随连接一起丢失（独立复核 R5R-06）。
		s.logFailedRun(r, res, req)
		s.logClientGone(r)
		return
	}
	if err != nil {
		s.writeRunError(w, err, runCtx)
		return
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		s.writeError(w, http.StatusGatewayTimeout, codeTimeout, "运行超过 60 秒时限")
		return
	}
	s.writeRunResult(r, w, res, req, started, s.now())
}

// handleArchiveSession 只改状态与首次归档时间：不删消息 / 会话 / 审计；重复归档幂等；
// 在途运行期间由 AgentService 拒绝（409 冻结文案，归档不生效）。成功响应无 data（注 4）。
// handleArchiveSession delegates archiving to the service; success carries no data.
func (s *Server) handleArchiveSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.opts.Runs.Archive(r.Context(), id)
	var busyErr *core.SessionBusyError
	if errors.As(err, &busyErr) {
		s.writeError(w, http.StatusConflict, codeSessionBusy, messageBusy)
		return
	}
	var notFoundErr *core.SessionNotFoundError
	if errors.As(err, &notFoundErr) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到会话："+id)
		return
	}
	if err != nil {
		s.writeStoreError(w, err, id)
		return
	}
	s.writeOK(w, nil)
}

// handleInvoke 是无状态调用：不建会话、不追加消息、无 409（无会话可锁，只有 429）；
// 响应 session_id 为空串，运行记录与审计照写（约束 10）。
// handleInvoke is the stateless run: no session row, no message writes, no 409.
func (s *Server) handleInvoke(w http.ResponseWriter, r *http.Request, principal core.Principal) {
	name := r.PathValue("name")
	var body struct {
		Message *string `json:"message"`
	}
	if !s.decodeBody(w, r, &body) {
		return
	}
	if body.Message == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "message 必填")
		return
	}
	if len(*body.Message) > maxMessageBytes {
		s.writeError(w, http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("消息超过 %d 字节上限", maxMessageBytes))
		return
	}
	if _, ok := s.registeredAgent(name); !ok {
		s.writeError(w, http.StatusNotFound, codeNotFound,
			"Agent 请求名不存在或未注册："+name)
		return
	}

	runCtx, cancel := context.WithTimeout(r.Context(), s.opts.RunTimeout)
	defer cancel()
	started := s.now()
	req := core.RunRequest{
		Principal: principal, // 业务认证中间件显式传入（T025），不得补默认值
		Agent:     name,
		SessionID: "", // 无状态调用（契约 §1.1）
		Message:   *body.Message,
		Channel:   channelWeb,
		UserID:    defaultUserID,
		Budget:    s.opts.RunBudget,
		// 同 handlePostMessage：服务端生成，不读客户端头（A-F2 / C-F02）。
		// Same server-generated id as handlePostMessage.
		RequestID: requestIDFrom(r.Context()),
	}
	res, err := s.opts.Runs.Run(runCtx, req)
	if r.Context().Err() != nil {
		s.logFailedRun(r, res, req) // 同 handlePostMessage：断开也留 failed 诊断
		s.logClientGone(r)
		return
	}
	if err != nil {
		s.writeRunError(w, err, runCtx)
		return
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		s.writeError(w, http.StatusGatewayTimeout, codeTimeout, "运行超过 60 秒时限")
		return
	}
	s.writeRunResult(r, w, res, req, started, s.now())
}

// handleNotFound 是方法无关 catch-all：未知路径与方法都返回统一 404 信封（注 3），
// 不暴露 ServeMux 的纯文本 404 与自动 405。
// handleNotFound is the method-agnostic catch-all.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, http.StatusNotFound, codeNotFound,
		fmt.Sprintf("未找到该接口（%s %s）", r.Method, r.URL.Path))
}

// writeRunError 按错误类型映射 400 / 404 / 409 / 429 / 504 / 500（约束 22 / 37 / 43），
// **不比对错误文本**；运行截止优先于错误类型判定（超时那一刻 Run 可能带回任意
// wrapped error）。归档会话固定 400 + 冻结文案；生命周期不可用固定 409
// `AGENT_UNAVAILABLE`；500 只出固定文案，原始错误进 ERROR 日志（约束 23）。
// writeRunError maps typed errors to status codes and never matches on text.
// writeRunAdmissionError 是运行准入专用码的出口（契约 §8 准入面）：绑定缺失 / 非法 /
// 无资格 / 凭据已撤销 → 409，主密钥不可用 → 503。它与保存面共用同一批 sentinel，
// 但状态码不同——保存期 400 是「改 `AGENT.md` 再存」，准入 409 是「资源状态已变，
// 换绑或恢复资源后再跑」。`AGENT_RESOURCE_UNAVAILABLE` 没有独立准入分支：准入把同一
// 条件并入 `AGENT_RESOURCE_BINDING_INVALID`（模块卡错误码表）。
// writeRunAdmissionError maps the admission-time binding and credential sentinels to
// their documented 409 / 503 codes, which differ from the save-time 400 split.
func (s *Server) writeRunAdmissionError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, core.ErrAgentResourceBindingMissing):
		s.writeError(w, http.StatusConflict, codeAgentResourceBindingMissing,
			adminMessageOr(err, messageBindingMissing))
	case errors.Is(err, core.ErrAgentResourceBindingInvalid):
		s.writeError(w, http.StatusConflict, codeAgentResourceBindingInvalid,
			adminMessageOr(err, messageBindingInvalid))
	case errors.Is(err, core.ErrResourceNotEligible):
		s.writeError(w, http.StatusConflict, codeResourceNotEligible,
			adminMessageOr(err, messageNotEligible))
	case errors.Is(err, core.ErrCredentialRevoked):
		s.writeError(w, http.StatusConflict, codeCredentialRevoked,
			adminMessageOr(err, messageCredentialRevoked))
	case errors.Is(err, core.ErrCredentialUnavailable):
		s.writeError(w, http.StatusServiceUnavailable, codeCredentialUnavailable,
			adminMessageOr(err, messageCredentialUnavailable))
	default:
		return false
	}
	return true
}

// writeQuotaExhausted 写 429 `QUOTA_EXHAUSTED`，并在错误带恢复时间时给 `Retry-After`
// （契约 §8：周期额度不足给到下一周期起点）。限额为零、限额行缺失与 `overage > 0`
// 未核对都没有日历上的恢复时间——等一个周期也不会恢复，靠管理员调额或核对，因此
// **不发**头：给错方向的等待比不给更误导。
// writeQuotaExhausted adds Retry-After only when the calendar holds a recovery instant.
func (s *Server) writeQuotaExhausted(w http.ResponseWriter, err error) {
	var quotaErr *callers.QuotaExhaustedError
	if errors.As(err, &quotaErr) && !quotaErr.RetryAt.IsZero() {
		if remaining := time.Until(quotaErr.RetryAt); remaining > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(remaining), 10))
		}
	}
	s.writeError(w, http.StatusTooManyRequests, codeQuotaExhausted,
		errorCodeMessage(codeQuotaExhausted, ""))
}

// writeRunError 把运行失败按类型映射为固定响应；未识别的失败只记日志、回固定 500。
func (s *Server) writeRunError(w http.ResponseWriter, err error, runCtx context.Context) {
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		s.writeError(w, http.StatusGatewayTimeout, codeTimeout, "运行超过 60 秒时限")
		return
	}
	// 认证失败的防御分支（C-F08）：正常路径由认证中间件与企业服务层拦截，能走到这里
	// 说明服务层在"已认证请求"上又判了一次身份。只认哨兵 `core.ErrUnauthorized`——
	// 按文案匹配会把内部错误误报成 401，也会让改一句文案就改变状态码（约束 22）。
	// 排在两个调用方 429 之前：同一条 error 链同时命中时，身份问题先暴露（先修身份
	// 才谈得上名额）。401 不带 `Retry-After`；日志只带 request_id，不带身份与凭据。
	// The defensive identity branch matches the sentinel only and precedes the two
	// caller 429s; the log carries request_id but never the identity or credentials.
	if errors.Is(err, core.ErrUnauthorized) {
		s.logger().Warn("运行入口认证失败", "request_id", requestIDFrom(runCtx))
		s.writeError(w, http.StatusUnauthorized, codeUnauthorized, messageUnauthorized)
		return
	}
	// 调用方准入的两个拒绝在运行入口**原样上抛**（[3] 见 `run_admission.go`，[5] 见
	// `call_quota.go` 的文件头），契约把它们的码冻结成互不合并的 429（§8）：这里不
	// 认领就只能落到末行的 500 INTERNAL，调用方分不清"该等名额"与"该等周期"。
	// `ErrRateLimited` 不在此列——[2] 的入口速率由认证后的 HTTP 中间件承担，运行入口
	// 的 error 链里不会出现它（C-35 的三段分工）。
	// The two caller refusals surface unchanged from the run entry; the contract freezes
	// them as separate 429 codes, so a 500 here would hide which one happened.
	if errors.Is(err, callers.ErrConcurrencyLimited) {
		s.writeError(w, http.StatusTooManyRequests, codeCallerConcurrencyLimited,
			errorCodeMessage(codeCallerConcurrencyLimited, ""))
		return
	}
	if errors.Is(err, callers.ErrQuotaExhausted) {
		s.writeQuotaExhausted(w, err)
		return
	}

	var archivedErr *core.SessionArchivedError
	if errors.As(err, &archivedErr) {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, "会话已归档，不可追加消息")
		return
	}
	var agentErr *core.AgentNotFoundError
	if errors.As(err, &agentErr) {
		s.writeError(w, http.StatusNotFound, codeNotFound,
			"Agent 请求名不存在或未注册："+agentErr.Name)
		return
	}
	// 生命周期不可用是 409（约束 37），**必须先于**下面的 fs.ErrNotExist 判定：
	// 状态文件缺失时 core 把 fs.ErrNotExist 挂在 Unwrap 链上，先判 fs 会把失败
	// 关闭的 409 错报成 404。文案取 core 自产文本（原因 + 修复入口），底层错误
	// 原文只经 Unwrap 隔离，不进入响应。
	// The lifecycle refusal is checked before fs.ErrNotExist: the missing-state-file
	// cause would otherwise turn a fail-closed 409 into a 404.
	var unavailableErr *core.AgentUnavailableError
	if errors.As(err, &unavailableErr) {
		// 中文按 Kind 投影（errors_zh.go）；英文 Reason 只进日志。
		// The Chinese copy comes from the Kind projection; English stays in the log.
		s.writeError(w, http.StatusConflict, codeAgentUnavailable, agentUnavailableMessage(unavailableErr))
		return
	}
	// 参与准入的绑定 / 凭据失败排在 fs.ErrNotExist 之前：主密钥不可用等错误可能把
	// 文件系统原因挂在链上，先判 fs 会把 503 错报成 404。
	// Admission refusals precede fs.ErrNotExist so a wrapped fs cause cannot turn a
	// fail-closed 409/503 into a 404.
	if s.writeRunAdmissionError(w, err) {
		return
	}
	if errors.Is(err, fs.ErrNotExist) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "未找到请求的资源")
		return
	}
	var busyErr *core.SessionBusyError
	if errors.As(err, &busyErr) {
		s.writeError(w, http.StatusConflict, codeSessionBusy, messageBusy)
		return
	}
	var fullErr *core.InstanceFullError
	if errors.As(err, &fullErr) {
		s.writeError(w, http.StatusTooManyRequests, codeTooManyRuns,
			fmt.Sprintf("实例并发上限 %d，请稍后重试", fullErr.Limit))
		return
	}
	s.logger().Error("运行失败", "error", err)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// writeRunResult 按 RunResult.Status 写 200 信封：limit_reached → LIMIT_REACHED
// （message 点名上限与配置键），completed / failed / cancelled → OK（原因经
// publicRunReason 投影，原始原因不出站）。`running` 不是合法的终态——记 ERROR 并
// 按内部错误处理。
//
// failed 额外记一条 WARN 内部诊断：core 在 Run 账本创建之前就失败时（如快照读取
// 失败），原始原因只存在于内存，响应又只有固定文案——不记这一行，排障将失去唯一
// 线索。诊断经 sanitizeRunReason 脱敏，并带上 run_id / request_id / session_id /
// agent；**原始原因不落日志**（约束 23 的日志面）。
// writeRunResult maps the final status onto the two allowed 200 shapes and logs a
// sanitized WARN diagnostic for failed runs, which may never reach the run ledger.
func (s *Server) writeRunResult(r *http.Request, w http.ResponseWriter, res core.RunResult, req core.RunRequest, started, finished time.Time) {
	view := runView(req, res, started, finished)
	switch res.Status {
	case core.StatusLimitReached:
		s.writeResult(w, codeLimitReached, view)
	case core.StatusCompleted, core.StatusFailed, core.StatusCancelled:
		s.logFailedRun(r, res, req)
		s.writeOK(w, view)
	default:
		s.logger().Error("运行返回了非终态", "status", res.Status, "run_id", res.RunID)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
	}
}

// logFailedRun 记 failed 运行的内部诊断（约束 23 的日志面）：响应只给固定文案，
// 原始原因只经 sanitizeRunReason 脱敏后落 WARN。core 可能在 Run 账本创建之前就
// 失败，此时 run_id 为空——request_id / session_id / agent 仍是定位线索；因此
// 正常返回与客户端断开两条路径都必须调用本方法（独立复核 R5R-06）。
// logFailedRun records the sanitized diagnostic for failed runs; the run ledger row
// may not exist yet, so the request/session/agent fields stay the only correlation.
func (s *Server) logFailedRun(r *http.Request, res core.RunResult, req core.RunRequest) {
	if res.Status != core.StatusFailed {
		return
	}
	s.logger().Warn("运行以 failed 结束",
		"run_id", res.RunID,
		"request_id", requestIDFrom(r.Context()),
		"session_id", req.SessionID,
		"agent", req.Agent,
		"diagnostic", sanitizeRunReason(res.Reason))
}

// logClientGone 记客户端断开（不写响应体——写已断开的连接只会得到 broken pipe）。
// logClientGone logs a client disconnect without writing any response.
func (s *Server) logClientGone(r *http.Request) {
	s.logger().Info("客户端已断开，未写响应",
		"request_id", requestIDFrom(r.Context()),
		"method", r.Method,
		"path", r.URL.Path)
}
