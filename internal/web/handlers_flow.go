package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func (s *Server) flowBound(w http.ResponseWriter) bool {
	if s.opts.Flows == nil {
		s.writeError(w, 500, codeInternal, messageInternal)
		return false
	}
	return true
}

func (s *Server) flowPage(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	limit := 50
	var err error
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
	}
	if err != nil || limit < 1 || limit > 100 {
		s.writeError(w, 400, "FLOW_DEFINITION_INVALID", "分页条数须为1到100")
		return "", 0, false
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 512 {
		s.writeError(w, 400, "FLOW_DEFINITION_INVALID", "分页游标无效")
		return "", 0, false
	}
	return cursor, limit, true
}

// Strict canonical decode rejects duplicate keys, trailing documents, deep or
// imprecise JSON before projecting the closed write DTO. Errors never echo data.
func (s *Server) decodeFlow(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, min(s.opts.MaxRequestBytes, 1<<20))
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeError(w, 400, "FLOW_DEFINITION_INVALID", "流程请求超过大小上限或无法读取")
		return false
	}
	canonical, err := core.CanonicalFlowJSON(r.Context(), raw)
	if err != nil {
		s.writeError(w, 400, "FLOW_DEFINITION_INVALID", "流程请求必须是合法的有界JSON")
		return false
	}
	if len(canonical) == 0 || canonical[0] != '{' {
		s.writeError(w, 400, "FLOW_DEFINITION_INVALID", "流程请求必须是JSON对象")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		s.writeError(w, 400, "FLOW_DEFINITION_INVALID", "流程请求字段或类型不符合协议")
		return false
	}
	return true
}

func flowETag(value, id string) (flow.TaskVersion, error) {
	var version flow.TaskVersion
	if len(value) < 3 || len(value) > 512 || value[0] != '"' || value[len(value)-1] != '"' {
		return version, core.ErrPreconditionFailed
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[1 : len(value)-1])
	if err != nil {
		return version, core.ErrPreconditionFailed
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 4 || parts[0] != id {
		return version, core.ErrPreconditionFailed
	}
	version.Revision, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || version.Revision < 0 {
		return version, core.ErrPreconditionFailed
	}
	version.StateVersion, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil || version.StateVersion < 1 {
		return version, core.ErrPreconditionFailed
	}
	version.ExecutionEpoch, err = strconv.ParseInt(parts[3], 10, 64)
	if err != nil || version.ExecutionEpoch < 1 {
		return version, core.ErrPreconditionFailed
	}
	if version.ETag(id) != value {
		return version, core.ErrPreconditionFailed
	}
	return version, nil
}

func (s *Server) flowWriteView(w http.ResponseWriter, v FlowTaskView, accepted bool) {
	w.Header().Set("ETag", v.ETag)
	status := http.StatusOK
	if accepted {
		status = http.StatusAccepted
	}
	s.writeEnvelope(w, status, Envelope{Code: codeOK, Message: messageSuccess, Data: v, Timestamp: s.timestamp()})
}

func (s *Server) writeFlowError(w http.ResponseWriter, err error) {
	status, code, message := 500, codeInternal, messageInternal
	var violation *core.SchemaViolationError
	switch {
	case errors.Is(err, core.ErrUnauthorized), errors.Is(err, core.ErrForbidden):
		status, code, message = 403, codeForbidden, "当前身份没有该流程操作权限"
	case errors.Is(err, core.ErrPreconditionFailed):
		status, code, message = 412, "PRECONDITION_FAILED", "任务版本已变化，请刷新后重试"
	case errors.As(err, &violation):
		status, code, message = 400, "FLOW_DEFINITION_INVALID", "输入字段不符合流程要求，请检查标记的位置"
	case errors.Is(err, flow.ErrNotFound):
		status, code, message = 404, "FLOW_NOT_FOUND", "没有找到该流程任务"
	case errors.Is(err, flow.ErrDefinitionNotFound):
		status, code, message = 404, "FLOW_DEFINITION_NOT_FOUND", "没有找到该流程定义"
	case errors.Is(err, flow.ErrDefinitionVersionNotFound):
		status, code, message = 404, "FLOW_DEFINITION_VERSION_NOT_FOUND", "没有找到该流程版本"
	case errors.Is(err, flow.ErrSchemaUnsupported):
		status, code, message = 400, "FLOW_SCHEMA_UNSUPPORTED", "流程使用了无效或当前不支持的 Schema，请使用受支持的严格子集"
	case errors.Is(err, flow.ErrDefinitionInvalid):
		status, code, message = 400, "FLOW_DEFINITION_INVALID", "流程定义或输入不符合协议"
	case errors.Is(err, flow.ErrDefinitionRetired):
		status, code, message = 409, "FLOW_DEFINITION_RETIRED", "流程已退役，不能接受新任务"
	case errors.Is(err, flow.ErrDefinitionNotCurrent):
		status, code, message = 409, "FLOW_DEFINITION_NOT_CURRENT", "新任务只能使用当前发布版本"
	case errors.Is(err, flow.ErrIdempotencyConflict):
		status, code, message = 409, "FLOW_IDEMPOTENCY_CONFLICT", "同一操作标识已用于不同请求"
	case errors.Is(err, flow.ErrActionUnknown):
		status, code, message = 409, "FLOW_ACTION_UNKNOWN", "存在未查证的调用结果，不会自动重放"
	case errors.Is(err, flow.ErrCheckpointUnsupported):
		status, code, message = 409, "FLOW_CHECKPOINT_UNSUPPORTED", "检查点格式或依据无法核对"
	case errors.Is(err, flow.ErrTaskClosed):
		status, code, message = 409, "FLOW_CLOSED", "任务已关闭，不能继续推进"
	case errors.Is(err, flow.ErrDeadline):
		status, code, message = 409, "FLOW_DEADLINE_EXPIRED", "任务已超过固定截止时间"
	case errors.Is(err, flow.ErrCapacity):
		status, code, message = 429, "FLOW_CAPACITY_EXHAUSTED", "流程任务容量已满，请稍后重试"
	case errors.Is(err, flow.ErrInteractiveReserve):
		status, code, message = 429, "FLOW_INTERACTIVE_RESERVE", "当前调用方并发额度已全部预留给交互运行；请调整额度或交互预留后再新建任务"
	case errors.Is(err, flow.ErrBudget):
		status, code, message = 409, "FLOW_STATE_CONFLICT", "任务预算已耗尽，不能继续分配"
	case errors.Is(err, flow.ErrStateConflict), errors.Is(err, flow.ErrCommitUnknown):
		status, code, message = 409, "FLOW_STATE_CONFLICT", "任务状态或操作结果尚未核对，请读取当前状态"
	}
	if status == 500 {
		s.logger().Error("Flow request failed", "reason", "flow_storage_unavailable")
	}
	if status == 400 && violation != nil {
		path := violation.Path
		if len(path) > 512 || strings.ContainsAny(path, "\r\n\x00") {
			path = "$"
		}
		s.writeEnvelope(w, status, Envelope{Code: code, Message: message, Data: map[string]string{"field_path": path, "rule": "input_schema"}, Timestamp: s.timestamp()})
		return
	}
	s.writeError(w, status, code, message)
}

func (s *Server) handleFlowDefinitions(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Definitions(r.Context(), p, false, cursor, limit)
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

func (s *Server) handleFlowDefinition(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	v, err := s.opts.Flows.Definition(r.Context(), p, r.PathValue("name"))
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(v.UID+":"+strconv.FormatInt(v.Version, 10)+":"+v.Hash))
	s.writeOK(w, v)
}

func (s *Server) handleFlowTasks(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Tasks(r.Context(), p, cursor, limit, r.URL.Query().Get("definition_name"))
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

func (s *Server) handleFlowTask(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Task(r.Context(), p, r.PathValue("id"), cursor, limit)
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	w.Header().Set("ETag", v.ETag)
	s.writeOK(w, v)
}

func (s *Server) handleFlowArtifacts(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Artifacts(r.Context(), p, r.PathValue("id"), cursor, limit)
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

func (s *Server) handleFlowVerifications(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Verifications(r.Context(), p, r.PathValue("id"), cursor, limit)
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

type flowStartWrite struct {
	DefinitionUID     string          `json:"definition_uid"`
	DefinitionVersion int64           `json:"definition_version"`
	Input             json.RawMessage `json:"input"`
	Budget            *flow.BudgetCap `json:"budget_cap,omitempty"`
}

func (s *Server) handleFlowStart(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	var body flowStartWrite
	if !s.decodeFlow(w, r, &body) {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if strings.TrimSpace(key) == "" || len(key) > 256 {
		s.writeFlowError(w, flow.ErrDefinitionInvalid)
		return
	}
	v, accepted, err := s.opts.Flows.Start(r.Context(), flow.StartRequest{Principal: p, RequestID: requestIDFrom(r.Context()), IdempotencyKey: key, DefinitionUID: body.DefinitionUID, DefinitionVersion: body.DefinitionVersion, Input: body.Input, BudgetCap: body.Budget})
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.flowWriteView(w, v, accepted)
}

type flowControlWrite struct {
	OperationID string `json:"operation_id"`
	Reason      string `json:"reason"`
}

func (s *Server) flowControl(w http.ResponseWriter, r *http.Request) (flow.TaskVersion, flowControlWrite, bool) {
	version, err := flowETag(r.Header.Get("If-Match"), r.PathValue("id"))
	if err != nil {
		s.writeFlowError(w, err)
		return version, flowControlWrite{}, false
	}
	var body flowControlWrite
	if !s.decodeFlow(w, r, &body) {
		return version, body, false
	}
	if strings.TrimSpace(body.OperationID) == "" || len(body.OperationID) > 256 || strings.TrimSpace(body.Reason) == "" || len(body.Reason) > 4096 {
		s.writeFlowError(w, flow.ErrDefinitionInvalid)
		return version, body, false
	}
	return version, body, true
}

func (s *Server) handleFlowCancel(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	version, body, ok := s.flowControl(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Cancel(r.Context(), flow.CancelRequest{Principal: p, TaskID: r.PathValue("id"), Expected: version, OperationID: body.OperationID, Reason: body.Reason, RequestID: requestIDFrom(r.Context())})
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.flowWriteView(w, v, !v.Status.Terminal() && v.CloseIntent != flow.CloseNone)
}

func (s *Server) handleFlowResume(w http.ResponseWriter, r *http.Request, p core.Principal) {
	if !s.flowBound(w) {
		return
	}
	version, body, ok := s.flowControl(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Resume(r.Context(), flow.ResumeRequest{Principal: p, TaskID: r.PathValue("id"), Expected: version, OperationID: body.OperationID, Reason: body.Reason, RequestID: requestIDFrom(r.Context())})
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.flowWriteView(w, v, false)
}

func (s *Server) handleAdminFlowDefinitions(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if !s.flowBound(w) {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Definitions(r.Context(), core.LocalAdminPrincipal(), true, cursor, limit)
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

func (s *Server) handleAdminFlowTasks(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.flowBound(w) {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.AdminTasks(r.Context(), actor, cursor, limit, r.URL.Query().Get("definition_name"))
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

func (s *Server) handleAdminFlowTask(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.flowBound(w) {
		return
	}
	cursor, limit, ok := s.flowPage(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.AdminTask(r.Context(), actor, r.PathValue("id"), cursor, limit)
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.flowWriteView(w, v, false)
}

type flowDefinitionWrite struct {
	Definition      json.RawMessage `json:"definition"`
	UID             string          `json:"definition_uid,omitempty"`
	ExpectedVersion *int64          `json:"expected_version,omitempty"`
	OperationID     string          `json:"operation_id,omitempty"`
}

func (s *Server) flowDefinitionBody(w http.ResponseWriter, r *http.Request) (flowDefinitionWrite, flow.Definition, bool) {
	var body flowDefinitionWrite
	if !s.decodeFlow(w, r, &body) {
		return body, flow.Definition{}, false
	}
	// HTTP publication carries the complete immutable bundle, including the
	// separately loaded TASK.json contract. The file-only decoder deliberately
	// rejects embedded contracts and must not be reused for this envelope.
	var definition flow.Definition
	decoder := json.NewDecoder(bytes.NewReader(body.Definition))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&definition); err != nil {
		s.writeFlowError(w, flow.ErrDefinitionInvalid)
		return body, definition, false
	}
	return body, definition, true
}

func (s *Server) handleAdminFlowValidate(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.flowBound(w) {
		return
	}
	_, def, ok := s.flowDefinitionBody(w, r)
	if !ok {
		return
	}
	if err := s.opts.Flows.Validate(r.Context(), core.LocalAdminPrincipal(), actor, def); err != nil {
		s.writeFlowError(w, errors.Join(flow.ErrDefinitionInvalid, err))
		return
	}
	s.writeOK(w, map[string]bool{"valid": true})
}

func (s *Server) handleAdminFlowPublish(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.flowBound(w) {
		return
	}
	body, def, ok := s.flowDefinitionBody(w, r)
	if !ok {
		return
	}
	if body.ExpectedVersion == nil || body.OperationID == "" {
		s.writeFlowError(w, flow.ErrDefinitionInvalid)
		return
	}
	v, err := s.opts.Flows.Publish(r.Context(), flow.PublishRequest{Principal: core.LocalAdminPrincipal(), Actor: actor, DefinitionUID: body.UID, ExpectedVersion: *body.ExpectedVersion, OperationID: body.OperationID, RequestID: requestIDFrom(r.Context()), Definition: def})
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

type flowRetireWrite struct {
	ExpectedVersion int64  `json:"expected_version"`
	OperationID     string `json:"operation_id"`
	Reason          string `json:"reason"`
}

func (s *Server) handleAdminFlowRetire(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.flowBound(w) {
		return
	}
	var body flowRetireWrite
	if !s.decodeFlow(w, r, &body) {
		return
	}
	v, err := s.opts.Flows.Retire(r.Context(), flow.RetireRequest{Principal: core.LocalAdminPrincipal(), Actor: actor, DefinitionUID: r.PathValue("uid"), Version: body.ExpectedVersion, ExpectedVersion: body.ExpectedVersion, OperationID: body.OperationID, Reason: body.Reason, RequestID: requestIDFrom(r.Context())})
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.writeOK(w, v)
}

func (s *Server) handleAdminFlowAbandon(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if !s.flowBound(w) {
		return
	}
	version, body, ok := s.flowControl(w, r)
	if !ok {
		return
	}
	v, err := s.opts.Flows.Abandon(r.Context(), flow.AbandonRequest{Principal: core.LocalAdminPrincipal(), Actor: actor, TaskID: r.PathValue("id"), Expected: version, OperationID: body.OperationID, Reason: body.Reason, RequestID: requestIDFrom(r.Context())})
	if err != nil {
		s.writeFlowError(w, err)
		return
	}
	s.flowWriteView(w, v, !v.Status.Terminal() && v.CloseIntent != flow.CloseNone)
}
