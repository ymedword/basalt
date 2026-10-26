package web

// 本文件是禁止域名表与内网访问例外的管理端点（14D / D-10 / D-13；契约
// `/sandbox/denied-domains` 与 `/network-exceptions`）：
//   - `PUT /sandbox/denied-domains` 与出网白名单同构的条件写（同一张
//     `network_domain_policy` 的第二行），保存后**下一次**判定即生效；
//   - `GET /network-exceptions` 列表与集合 ETag、`POST` 新增（理由必填）、
//     `DELETE /network-exceptions/{id}` 条件删除（`If-Match` 是集合 ETag）。
//
// 本文件只做四件事：解码请求、把校验失败折成固定状态码、投影服务层算好的视图、
// 在装配缺失时 fail-closed。比较（If-Match、修订号 CAS、重复登记）、预写与终态
// 回填都在服务层；web 层**不**自己比较 ETag，也**不**把一次写入顺带改写另一张表。
// This file serves the denylist and network-exception endpoints; every comparison,
// the pre-write and the terminal recording live in the service layer.

import (
	"errors"
	"net/http"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 固定公开文案：禁止表条件写与例外写入各两条。中文是管理台与用户可见语言；
// 日志模板仍为英文（R8 裁决 1）。
// Fixed public Chinese texts for both endpoints; logs stay English.
const (
	networkDenylistStaleMessage    = "网络禁止域名表已被其他操作修改，请刷新后重试"
	networkDenylistDomainsMissing  = "请求体缺少 domains 字段（清空禁止表请显式传空数组）"
	networkExceptionStaleMessage   = "网络访问例外已被其他操作修改，请刷新后重试"
	networkExceptionConflictText   = "该网络访问例外已存在（不覆盖既有理由，删除后可重建）"
	networkExceptionBodyMessage    = "请求体必须是 {\"kind\": \"host|ip_port\", \"value\": \"…\", \"note\": \"…\"}"
	networkExceptionKindMissing    = "请求体缺少 kind 字段"
	networkExceptionValueMissing   = "请求体缺少 value 字段"
	networkExceptionNoteMissing    = "请求体缺少 note 字段（放行必须带理由）"
	networkExceptionNotFoundPrefix = "未找到网络访问例外："
)

// networkExceptionWrite 是 `POST /network-exceptions` 的请求体：三个字段都用指针，
// 缺字段在本层点名；非空校验（kind 闭集、value 语法、note 非空白与长度）在服务层，
// 保证 400 的判据只有一处。
// All three fields are pointers so a missing key is named here; value validation
// lives in the service so there is exactly one 400 judgement.
type networkExceptionWrite struct {
	Kind  *string `json:"kind"`
	Value *string `json:"value"`
	Note  *string `json:"note"`
}

// NetworkExceptionView 是例外行的对外投影（契约 `NetworkExceptionView`）。
// NetworkExceptionView is the fixed projection of one exception row.
type NetworkExceptionView struct {
	ExceptionID string `json:"exception_id"`
	Kind        string `json:"kind"`
	Value       string `json:"value"`
	Note        string `json:"note"`
	Revision    int64  `json:"revision"`
	CreatedAt   string `json:"created_at"`
	CreatedBy   string `json:"created_by"`
	OperationID string `json:"operation_id"`
}

// NetworkExceptionListView 是 `GET /network-exceptions` 的 data（契约同名字段）：
// `etag` 是整份集合的强 ETag，也是删除的唯一 `If-Match` 取值来源。
// NetworkExceptionListView carries the collection and the ETag used for delete.
type NetworkExceptionListView struct {
	ETag       string                 `json:"etag"`
	Exceptions []NetworkExceptionView `json:"exceptions"`
}

// NetworkExceptionCreateResult 是 `POST /network-exceptions` 的 data：新建行与
// 写入后整份集合的强 ETag。
// NetworkExceptionCreateResult pairs the new row with the post-write ETag.
type NetworkExceptionCreateResult struct {
	ETag      string               `json:"etag"`
	Exception NetworkExceptionView `json:"exception"`
}

// NetworkExceptionDeleteResult 是 `DELETE /network-exceptions/{id}` 的 data。
// NetworkExceptionDeleteResult is the delete response payload.
type NetworkExceptionDeleteResult struct {
	ExceptionID string `json:"exception_id"`
	ETag        string `json:"etag"`
	OperationID string `json:"operation_id"`
}

// networkExceptionView 把一条例外投影为契约视图：时间固定 RFC3339 UTC，零值渲染
// 为空串（没有来源时间就不编造一个看起来合法的时间）。
// networkExceptionView renders one row; a zero time renders as an empty string.
func networkExceptionView(exception core.NetworkException) NetworkExceptionView {
	view := NetworkExceptionView{
		ExceptionID: exception.ExceptionID,
		Kind:        exception.Kind,
		Value:       exception.Value,
		Note:        exception.Note,
		Revision:    exception.Revision,
		CreatedBy:   exception.CreatedBy,
		OperationID: exception.OperationID,
	}
	if !exception.CreatedAt.IsZero() {
		view.CreatedAt = exception.CreatedAt.UTC().Format(time.RFC3339)
	}
	return view
}

// handleAdminUpdateDeniedDomains 处理 `PUT /api/v1/sandbox/denied-domains`：请求形状
// 校验（请求体、If-Match 形状）在本层完成且零写入；成功响应是**重新读出的**生效
// 视图（`GET /sandbox` 同形），不做乐观回显。
// The handler validates the body and the If-Match shape before any write; the
// success payload is the live sandbox view rather than an optimistic echo.
func (s *Server) handleAdminUpdateDeniedDomains(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.NetworkDenylist == nil {
		s.writeAdminPortMissing(w, "NetworkDenylist", "PUT /sandbox/denied-domains")
		return
	}
	var body networkAllowlistWrite
	if err := s.decodeAdminWriteBody(w, r, &body, networkAllowlistBodyMessage); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.Domains == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, networkDenylistDomainsMissing)
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, ifMatchRequiredMessage)
		return
	}

	_, operationID, err := s.opts.NetworkDenylist.Update(r.Context(), *body.Domains, ifMatch, actor)
	if err != nil {
		s.writeNetworkDenylistError(w, err, operationID)
		return
	}
	s.writeOK(w, s.sandboxView())
}

// writeNetworkDenylistError 复用白名单条件写的映射（400 / 412 / 409 / 500），只替换
// 公开文案与读取当前 ETag 的端口——两张表的写入语义逐条一致，映射不得分叉。
// The mapping is shared with the allowlist writer; only the public text and the
// ETag source differ, so the two rows cannot drift apart.
func (s *Server) writeNetworkDenylistError(w http.ResponseWriter, err error, operationID string) {
	s.writeNetworkDomainPolicyError(w, err, operationID, s.opts.NetworkDenylist, networkDenylistStaleMessage)
}

// handleNetworkExceptions 处理 `GET /api/v1/network-exceptions`：只读内存快照，不触
// 库、不写操作行。装配缺失回 500——**不**返回空清单冒充"没有例外"（那会让页面
// 与拨号判定分叉）。
// The read handler serves the live snapshot; a missing port fails closed rather
// than answering with an empty list.
func (s *Server) handleNetworkExceptions(w http.ResponseWriter, _ *http.Request, _ core.Actor) {
	if s.opts.NetworkExceptions == nil {
		s.writeAdminPortMissing(w, "NetworkExceptions", "GET /network-exceptions")
		return
	}
	s.writeOK(w, s.networkExceptionListView())
}

// networkExceptionListView 投影当前例外集合与集合 ETag。
// networkExceptionListView projects the live set and its collection ETag.
func (s *Server) networkExceptionListView() NetworkExceptionListView {
	list := s.opts.NetworkExceptions.List()
	views := make([]NetworkExceptionView, 0, len(list))
	for _, exception := range list {
		views = append(views, networkExceptionView(exception))
	}
	return NetworkExceptionListView{ETag: s.opts.NetworkExceptions.ETag(), Exceptions: views}
}

// handleAdminCreateNetworkException 处理 `POST /api/v1/network-exceptions`：缺字段
// 在本层点名（400，零写入）；值 / 理由校验与重复登记都在服务层，返回 400 / 409。
// 成功响应带新建行与写入后的集合 ETag（页面可直接用作后续删除的 If-Match）。
// Missing keys are named here; the service owns value validation, duplicate
// detection, the pre-write and the terminal recording.
func (s *Server) handleAdminCreateNetworkException(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.NetworkExceptions == nil {
		s.writeAdminPortMissing(w, "NetworkExceptions", "POST /network-exceptions")
		return
	}
	var body networkExceptionWrite
	if err := s.decodeAdminWriteBody(w, r, &body, networkExceptionBodyMessage); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.Kind == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, networkExceptionKindMissing)
		return
	}
	if body.Value == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, networkExceptionValueMissing)
		return
	}
	if body.Note == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, networkExceptionNoteMissing)
		return
	}

	exception, operationID, err := s.opts.NetworkExceptions.Create(r.Context(),
		*body.Kind, *body.Value, *body.Note, actor)
	if err != nil {
		s.writeNetworkExceptionCreateError(w, err, operationID)
		return
	}
	s.writeOK(w, NetworkExceptionCreateResult{
		ETag:      s.opts.NetworkExceptions.ETag(),
		Exception: networkExceptionView(exception),
	})
}

// handleAdminDeleteNetworkException 处理 `DELETE /api/v1/network-exceptions/{id}`：
// `If-Match` 形状已由路由中间件把住，这里取出原值与路径标识交给服务层比较集合
// ETag；失败按 404 / 412 / 409 / 500 映射。
// The If-Match shape is already enforced by the routing middleware; the service
// compares the literal value with the live collection ETag.
func (s *Server) handleAdminDeleteNetworkException(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.NetworkExceptions == nil {
		s.writeAdminPortMissing(w, "NetworkExceptions", "DELETE /network-exceptions/{id}")
		return
	}
	exceptionID := r.PathValue("id")
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		// 路由已挡住非法形状；此处只防御直接调用处理函数的测试接缝。
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, ifMatchRequiredMessage)
		return
	}
	operationID, err := s.opts.NetworkExceptions.Delete(r.Context(), exceptionID, ifMatch, actor)
	if err != nil {
		s.writeNetworkExceptionDeleteError(w, err, exceptionID, operationID)
		return
	}
	s.writeOK(w, NetworkExceptionDeleteResult{
		ExceptionID: exceptionID,
		ETag:        s.opts.NetworkExceptions.ETag(),
		OperationID: operationID,
	})
}

// writeNetworkExceptionCreateError 把新增失败映射为契约固定状态码：请求校验 400、
// 重复登记 409 `CONFLICT`（预写之前就发现的重复没有操作行，先写失败后的唯一键
// 冲突带操作标识）、其余 500（带操作标识）。401 / 403 由认证中间件给出。
// writeNetworkExceptionCreateError maps create failures onto the fixed table; only
// post-pre-write failures carry an operation id.
func (s *Server) writeNetworkExceptionCreateError(w http.ResponseWriter, err error, operationID string) {
	var invalid *core.InvalidInputError
	switch {
	case errors.As(err, &invalid):
		// 值语法 / 云元数据地址 / 理由缺失或超长的 Reason 已点名具体原因（不含值）。
		s.writeError(w, http.StatusBadRequest, codeBadRequest, invalid.Reason)
	case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrPolicyRevisionConflict):
		s.writeAdminDataError(w, http.StatusConflict, codeConflict, networkExceptionConflictText,
			s.networkExceptionWriteData(operationID))
	default:
		s.logger().Error("network exception create failed", "error", err, "operation_id", operationID)
		s.writeNetworkExceptionInternal(w, operationID)
	}
}

// writeNetworkExceptionDeleteError 把删除失败映射为固定状态码：集合 ETag 不匹配 412
// （带当前集合 ETag，页面据此重取）、行不存在 404（点名标识）、并发删除撞车 409
// （带操作标识）、其余 500（带操作标识）。
// writeNetworkExceptionDeleteError maps delete failures onto the fixed table.
func (s *Server) writeNetworkExceptionDeleteError(w http.ResponseWriter, err error, exceptionID, operationID string) {
	switch {
	case errors.Is(err, core.ErrPreconditionFailed):
		s.writeAdminDataError(w, http.StatusPreconditionFailed, codePreconditionFailed,
			networkExceptionStaleMessage, s.staleNetworkExceptionError(""))
	case errors.Is(err, core.ErrResourceNotFound):
		s.writeError(w, http.StatusNotFound, codeNotFound, networkExceptionNotFoundPrefix+exceptionID)
	case errors.Is(err, core.ErrPolicyRevisionConflict), errors.Is(err, core.ErrConflict):
		s.writeAdminDataError(w, http.StatusConflict, codeConflict, networkExceptionStaleMessage,
			s.networkExceptionWriteData(operationID))
	default:
		s.logger().Error("network exception delete failed", "exception_id", exceptionID,
			"error", err, "operation_id", operationID)
		s.writeNetworkExceptionInternal(w, operationID)
	}
}

// writeNetworkExceptionInternal 是两条写入路径共用的 500 出口：预写之后失败**必须**
// 带操作标识供 `GET /operations/{id}` 查证（约束 1a）；预写之前（没有操作行）只给
// 固定文案，不泄露底层错误。
// The shared 500 exit; a post-pre-write failure always carries its operation id.
func (s *Server) writeNetworkExceptionInternal(w http.ResponseWriter, operationID string) {
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

// networkExceptionWriteData 组装 409 的失败数据：只带操作标识（无操作行时为空，
// 经 errorDataView 白名单自然退回不带 data 的信封）。
// The 409 payload carries the operation id only.
func (s *Server) networkExceptionWriteData(operationID string) error {
	data := map[string]any{}
	if operationID != "" {
		data["operation_id"] = operationID
	}
	return &core.AdminError{Kind: core.ErrConflict, Message: networkExceptionConflictText, Data: data}
}

// staleNetworkExceptionError 组装 412 的失败数据：当前集合 ETag（页面按它重排
// `If-Match` 后重试，**不**自动重发）与可选操作标识。
// staleNetworkExceptionError builds the stale payload from the live collection ETag.
func (s *Server) staleNetworkExceptionError(operationID string) error {
	data := map[string]any{}
	if s.opts.NetworkExceptions != nil {
		data["current_etag"] = s.opts.NetworkExceptions.ETag()
	}
	if operationID != "" {
		data["operation_id"] = operationID
	}
	return &core.AdminError{Kind: core.ErrPreconditionFailed, Message: networkExceptionStaleMessage, Data: data}
}
