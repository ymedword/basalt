package web

// 本文件是执行档位端点（14D；契约 openapi `/execution-policy` 的 GET / PUT）：读当前
// 生效档位与升级待确认项，以及带 `If-Match` 的条件写。权威源是单行表
// `execution_policy`，`basalt.yaml` 的 `execution.profile` 只在表内无行时作首次种子；
// 保存成功即对**下一次**授权判定生效，不重启、不撤回在途 Run 与已建立连接。
//
// 本文件只做三件事：解码请求、把校验失败折成固定状态码、投影服务层算好的视图。
// 条件比较（If-Match、`expected_profile`、修订号 CAS）、预写与终态回填都在服务层，
// web 层**不**自己比较 ETag，也不把一次档位切换顺带写成白名单或例外变更。
// This file serves the execution-profile endpoints; the service owns every conditional
// comparison, the pre-write and the terminal recording, while web only decodes,
// maps failures onto the fixed status table and projects the view.

import (
	"context"
	"errors"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// 固定公开文案：档位条件写与只读读取各一条。中文是管理台与用户可见语言；日志模板
// 仍为英文（R8 裁决 1）。
// Fixed public Chinese texts for the profile endpoints; logs stay English.
const (
	executionPolicyStaleMessage    = "执行档位已被其他操作修改，请刷新后重试"
	executionPolicyMismatchMessage = "库内档位与页面确认的方向不一致，请刷新后重新确认"
	executionPolicyBodyMessage     = "请求体必须是 {\"profile\": \"standard|strict\", \"expected_profile\": \"standard|strict\"}"
	executionPolicyProfileMissing  = "请求体缺少 profile 字段"
	executionPolicyExpectedMissing = "请求体缺少 expected_profile 字段"
	executionPolicyNotLoaded       = "执行档位尚未装载，无法读写（装配错误）"
)

// executionPolicyWrite 是 PUT 的请求体：两个字段都用指针，缺字段与空串分开报——
// 空串会被服务层按"非法档位"拒绝，缺字段必须在本层点名（契约两者都 400）。
// Both fields are pointers so a missing key is reported apart from an empty literal.
type executionPolicyWrite struct {
	Profile         *string `json:"profile"`
	ExpectedProfile *string `json:"expected_profile"`
}

// policyConfirmationWrite 是 `POST /execution-policy/confirmations` 的请求体
// （契约 PolicyConfirmationWrite）：`decision` 当前唯一取值 `keep_denied`。
// The confirmation body; keep_denied is the only decision today.
type policyConfirmationWrite struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
}

// handleExecutionPolicy 处理 `GET /api/v1/execution-policy`（契约 D-03）：档位、生效
// 元数据与两组待确认项。**只读**——扫描不启动进程、不建连接、不写操作行。
// handleExecutionPolicy serves the read-only profile view; the scan starts no process
// and writes no operation row.
func (s *Server) handleExecutionPolicy(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	view, err := s.executionPolicyView(r.Context())
	if err != nil {
		s.writeExecutionPolicyReadError(w, err)
		return
	}
	s.writeOK(w, view)
}

// handleAdminUpdateExecutionPolicy 处理 `PUT /api/v1/execution-policy`：纯请求校验
// （请求体、If-Match 形状）在本层完成且零写入；`expected_profile` 的方向确认与修订号
// CAS 都在服务层同一把锁内完成。成功响应是**重新读出**的生效视图，不是回显。
// The handler validates the body and the If-Match shape before any write; the success
// payload is re-read from the live state rather than echoed.
func (s *Server) handleAdminUpdateExecutionPolicy(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.ExecutionPolicy == nil {
		s.writeAdminPortMissing(w, "ExecutionPolicy", "PUT /execution-policy")
		return
	}
	var body executionPolicyWrite
	if err := s.decodeAdminWriteBody(w, r, &body, executionPolicyBodyMessage); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.Profile == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, executionPolicyProfileMissing)
		return
	}
	if body.ExpectedProfile == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, executionPolicyExpectedMissing)
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, ifMatchRequiredMessage)
		return
	}

	_, operationID, err := s.opts.ExecutionPolicy.Update(r.Context(),
		core.ExecutionProfile(*body.Profile), core.ExecutionProfile(*body.ExpectedProfile), ifMatch, actor)
	if err != nil {
		s.writeExecutionPolicyWriteError(w, err, operationID)
		return
	}
	view, viewErr := s.executionPolicyView(r.Context())
	if viewErr != nil {
		// 写入已经成功落库：此时返回 500 会误导页面以为没生效。回一份只含档位与
		// 元数据、待确认项为空的视图，并把扫描失败记 ERROR——如实区分"存了"与
		// "列表没扫出来"。
		s.logger().Error("execution profile updated but the pending scan failed",
			"operation_id", operationID, "error", viewErr)
		policy := s.opts.ExecutionPolicy.Current()
		s.writeOK(w, executionPolicyView(policy, s.opts.ExecutionPolicy.ExceptionsCount(), nil))
		return
	}
	s.writeOK(w, view)
}

// executionPolicyView 读出当前档位与只读扫描结果并投影为契约视图。档位行尚未装载
// （修订号 0）按装配错误拒绝：`execution_policy` 在迁移 / 实例初始化时必写一行，
// 没有行意味着服务没被种子化，**不**用 `strict` 之类的默认值冒充当前值。
// A revision of zero means the service was never seeded, which is an assembly error
// rather than a value that could be projected.
func (s *Server) executionPolicyView(ctx context.Context) (ExecutionPolicyView, error) {
	if s.opts.ExecutionPolicy == nil {
		return ExecutionPolicyView{}, errExecutionPolicyPortMissing
	}
	policy := s.opts.ExecutionPolicy.Current()
	if policy.Revision == 0 {
		return ExecutionPolicyView{}, errExecutionPolicyNotLoaded
	}
	pending, err := s.opts.ExecutionPolicy.PendingConfirmations(ctx)
	if err != nil {
		return ExecutionPolicyView{}, err
	}
	return executionPolicyView(policy, s.opts.ExecutionPolicy.ExceptionsCount(), pending), nil
}

// executionPolicyView 把档位行与待确认项投影为契约视图：ETag 规则与
// `network_allowlist` 同口径（修订号 0 留空），受保护地址段与 shell 名称集合取
// core 的**同一份**字面量——展示与判定不可能漂移。
// executionPolicyView projects the row; the protected ranges and shell names come from
// the very literals the judgement reads.
func executionPolicyView(policy core.ExecutionPolicy, exceptionsCount int,
	pending []core.PendingPolicyConfirmation,
) ExecutionPolicyView {
	view := ExecutionPolicyView{
		NetworkAllowlistView: executionPolicyMetadata(policy),
		Profile:              string(policy.Profile),
		ExceptionsCount:      exceptionsCount,
		ProtectedAddresses:   core.ProtectedAddresses(),
		ShellNames:           core.ShellNames(),
		PendingConfirmations: make([]PendingPolicyConfirmationView, 0, len(pending)),
	}
	for _, item := range pending {
		view.PendingConfirmations = append(view.PendingConfirmations, PendingPolicyConfirmationView{
			ID:                item.ID,
			Axis:              string(item.Axis),
			Value:             item.Value,
			Reason:            string(item.Reason),
			ResolvedProtected: nonNilStrings(item.ResolvedProtected),
		})
	}
	return view
}

// 装配 / 种子化的固定错误：读取端点据此给出同一条 500 文案，不把内部状态泄进响应。
// Fixed internal errors for the read path.
var (
	errExecutionPolicyPortMissing = errors.New("execution policy port is not assembled")
	errExecutionPolicyNotLoaded   = errors.New("execution policy has not been seeded")
)

// writeExecutionPolicyReadError 把只读路径的失败统一映射为 500 INTERNAL：读不到当前
// 档位与"档位是 standard / strict"不是一回事，**不得**用默认值填响应。
// A failed read is never answered with a default profile.
func (s *Server) writeExecutionPolicyReadError(w http.ResponseWriter, err error) {
	s.logger().Error("execution policy read failed", "error", err)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}

// writeExecutionPolicyWriteError 把条件写失败映射为契约固定状态码：请求校验 400、
// 方向确认落空 409 `EXECUTION_PROFILE_MISMATCH`（零写入、零操作行，带当前档位与
// ETag 供重新确认）、条件不满足 412（带当前 ETag）、修订号冲突 409（只带操作标识）、
// 其余 500（带操作标识）。400 / 409 方向 / 412 都发生在预写之前，**没有**操作行。
// writeExecutionPolicyWriteError maps the conditional write's failures onto the fixed
// status table; only post-pre-write failures carry an operation id.
func (s *Server) writeExecutionPolicyWriteError(w http.ResponseWriter, err error, operationID string) {
	var invalid *core.InvalidInputError
	switch {
	case errors.As(err, &invalid):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, invalid.Reason)
	case errors.Is(err, core.ErrExecutionProfileMismatch):
		s.writeAdminDataError(w, http.StatusConflict, codeExecutionProfileMismatch,
			executionPolicyMismatchMessage, s.executionPolicyMismatchError())
	case errors.Is(err, core.ErrPreconditionFailed):
		s.writeAdminDataError(w, http.StatusPreconditionFailed, codePreconditionFailed,
			executionPolicyStaleMessage, s.executionPolicyStaleError(""))
	case errors.Is(err, core.ErrPolicyRevisionConflict):
		// 进程内互斥之后仍撞修订号：另一进程已写入。`pending` 已写，必须带操作标识；
		// 落败方**不**承诺当前 ETag（服务层已在失败路径重读权威行）。
		s.writeAdminDataError(w, http.StatusConflict, codeConflict,
			executionPolicyStaleMessage, s.executionPolicyStaleError(operationID))
	default:
		s.logger().Error("execution policy conditional write failed",
			"error", err, "operation_id", operationID)
		if operationID == "" {
			s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
		s.writeEnvelope(w, http.StatusInternalServerError, Envelope{
			Code: codeInternal, Message: messageInternal,
			Data:      ErrorDataView{OperationID: &operationID},
			Timestamp: s.timestamp(),
		})
	}
}

// executionPolicyMismatchError 组装 409 的方向不一致数据：带上库内当前档位与 ETag，
// 页面据此重画确认框；**不**带操作标识（此时还没有操作行）。
// The mismatch payload carries the live profile and ETag so the dialog can be redrawn.
func (s *Server) executionPolicyMismatchError() error {
	data := map[string]any{}
	if s.opts.ExecutionPolicy != nil {
		policy := s.opts.ExecutionPolicy.Current()
		data["current_profile"] = string(policy.Profile)
		if policy.Revision > 0 {
			data["current_etag"] = core.ExecutionPolicyETag(policy)
		}
	}
	return &core.AdminError{
		Kind: core.ErrExecutionProfileMismatch, Message: executionPolicyMismatchMessage, Data: data,
	}
}

// executionPolicyStaleError 组装 412 / 修订号冲突的数据：能读到当前修订时给
// `current_etag`，CAS 落败时另带操作标识（可能为空）。
// executionPolicyStaleError builds the stale-condition payload from the live snapshot.
func (s *Server) executionPolicyStaleError(operationID string) error {
	data := map[string]any{}
	if s.opts.ExecutionPolicy != nil {
		if policy := s.opts.ExecutionPolicy.Current(); policy.Revision > 0 {
			data["current_etag"] = core.ExecutionPolicyETag(policy)
		}
	}
	if operationID != "" {
		data["operation_id"] = operationID
	}
	return &core.AdminError{Kind: core.ErrPreconditionFailed, Message: executionPolicyStaleMessage, Data: data}
}

// handleAdminConfirmPolicy 处理 `POST /api/v1/execution-policy/confirmations`：
// 记录一条待确认项的「保持拒绝」。它**不改授权结论**、不改档位行、不推进修订号，
// 因而不需要 `If-Match`；重复记录返回 200 且 `already_recorded=true`，不新增审计行。
// The confirmation records a keep-denied decision without touching any authorization
// verdict or the policy row, so it takes no If-Match.
func (s *Server) handleAdminConfirmPolicy(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.ExecutionPolicy == nil {
		s.writeAdminPortMissing(w, "ExecutionPolicy", "POST /execution-policy/confirmations")
		return
	}
	var body policyConfirmationWrite
	if err := s.decodeAdminWriteBody(w, r, &body, policyConfirmationBodyMessage); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.ID == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, policyConfirmationIDMissing)
		return
	}
	if body.Decision != policyDecisionKeepDenied {
		// 「加例外」走 `POST /network-exceptions`：一次写入只表达一个目的，本端点**不**
		// 顺带建例外，也不接受空串以外的未知取值。
		s.writeError(w, http.StatusBadRequest, codeBadRequest, policyConfirmationDecisionMessage)
		return
	}
	operationID, already, err := s.opts.ExecutionPolicy.Confirm(r.Context(), body.ID, actor)
	if err != nil {
		s.writePolicyConfirmationError(w, err, operationID)
		return
	}
	s.writeOK(w, PolicyConfirmationResultView{
		ID:              body.ID,
		Decision:        policyDecisionKeepDenied,
		OperationID:     operationID,
		AlreadyRecorded: already,
	})
}

// policyDecisionKeepDenied 是「保持拒绝」的契约字面量（当前唯一取值）。
// The only confirmation decision literal today.
const policyDecisionKeepDenied = "keep_denied"

// 确认端点的固定公开文案。
// Fixed public texts for the confirmation endpoint.
const (
	policyConfirmationBodyMessage     = "请求体必须是 {\"id\": \"<axis>:<value>\", \"decision\": \"keep_denied\"}"
	policyConfirmationIDMissing       = "请求体缺少 id 字段"
	policyConfirmationDecisionMessage = "decision 只接受 keep_denied；放行请走 POST /network-exceptions"
)

// writePolicyConfirmationError 映射处置失败：不在待确认集合内 400（点名标识）、
// 预写之后失败 500（带操作标识）。
// Unknown ids are a 400 naming the id; post-pre-write failures carry the operation id.
func (s *Server) writePolicyConfirmationError(w http.ResponseWriter, err error, operationID string) {
	var invalid *core.InvalidInputError
	switch {
	case errors.As(err, &invalid):
		s.writeError(w, http.StatusBadRequest, codeBadRequest, invalid.Reason)
	default:
		s.logger().Error("policy confirmation failed", "error", err, "operation_id", operationID)
		if operationID == "" {
			s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
		s.writeEnvelope(w, http.StatusInternalServerError, Envelope{
			Code: codeInternal, Message: messageInternal,
			Data:      ErrorDataView{OperationID: &operationID},
			Timestamp: s.timestamp(),
		})
	}
}
