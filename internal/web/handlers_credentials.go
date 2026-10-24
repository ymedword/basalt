package web

// 本文件是两个**资源状态动作**端点（模块卡 §3.7.1 / T139）：
// `POST /resources/{kind}/{name}/credential/revoke` 与
// `POST /resources/{kind}/{name}/scope`。两条端点都是条件写：`If-Match` 形状已由
// `admin_routes.go` 的中间件判定，本层仍按请求头重读原值并原样下传——比较发生在
// 适配器的资源锁内（陈旧值 412 且零写入、零操作行，FR-003）。
// 纯请求校验在这里完成：请求体形状（未知字段拒绝）、撤销必须 `confirm: true`、
// 路径资源名非空。身份（`resource_uid`）、范围字面量与属主的语义裁决归适配器：
// 它按动作字面量给出可读的 400 / 409，并且**先校验再预写**（FR-001）。
// 成功响应写契约声明的 `ETag` 响应头，再回契约视图；失败一律经
// `writeResourceAdminError` 映射，`pending` 之后的失败带 `operation_id`。
// This file carries the two resource-state endpoints. If-Match is re-read verbatim
// and compared inside the adapter's resource lock; only body shape and the confirm
// literal are judged here. A non-empty ETag becomes a response header.

import (
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// messageCredentialRevokeBody 是请求体不是合法 JSON / 含未知字段时的固定文案。
	// #nosec G101 -- 固定用户文案，值不是凭据（变量名含 credential 命中的误报）。
	messageCredentialRevokeBody = "请求体不是合法的凭据撤销请求"
	// messageCredentialConfirm 是 `confirm` 不是字面 true 时的固定文案：契约枚举
	// 只有 `true`，缺省与 `false` 同样拒绝，且拒绝早于任何操作行。
	// #nosec G101 -- 固定用户文案，值不是凭据（变量名含 credential 命中的误报）。
	messageCredentialConfirm = "撤销必须显式确认：请求体须带 confirm: true"
	// messageResourceScopeBody 是范围切换请求体形状错误的固定文案。
	messageResourceScopeBody = "请求体不是合法的范围切换请求"
)

// handleAdminRevokeCredential 处理 `POST /resources/{kind}/{name}/credential/revoke`：
// 端口未装配按 500 固定文案失败关闭；`confirm` 校验先于端口调用，因此被拒请求不留
// 操作行。类别只接受 model_connection / notify_channel——由适配器按撤销动作字面量
// 判 400；env 模式资源没有托管凭据时同样由适配器给出 400。
// handleAdminRevokeCredential rejects a non-literal confirm before the port, so a
// refused request never leaves an operation row.
func (s *Server) handleAdminRevokeCredential(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.ResourceStateService == nil {
		s.writeAdminPortMissing(w, "ResourceStateService",
			"POST /resources/{kind}/{name}/credential/revoke")
		return
	}
	var body CredentialRevokeWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageCredentialRevokeBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if !body.Confirm {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageCredentialConfirm)
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}
	view, err := s.opts.ResourceStateService.RevokeCredential(r.Context(), actor,
		r.PathValue("kind"), name, body, ifMatch)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "撤销托管凭据")
		return
	}
	s.setETagHeader(w, view.ETag)
	s.writeOK(w, view)
}

// handleAdminConvertScope 处理 `POST /resources/{kind}/{name}/scope`：范围字面量、
// "exclusive 必带 owner_agent / shared 不得带"与属主存在性都在适配器内判定，
// 且属主解析排在预写之前——404 / 409 的请求同样不留操作行。成功响应同时给
// `data.etag` 与 `ETag` 响应头（契约在这一端点声明了两处）。
// handleAdminConvertScope forwards the body verbatim; the adapter owns the literal,
// the owner rules and the owner lookup, all before the pending row.
func (s *Server) handleAdminConvertScope(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.ResourceStateService == nil {
		s.writeAdminPortMissing(w, "ResourceStateService",
			"POST /resources/{kind}/{name}/scope")
		return
	}
	var body ResourceScopeWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageResourceScopeBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, messageIfMatchRequired)
		return
	}
	view, err := s.opts.ResourceStateService.ConvertScope(r.Context(), actor,
		r.PathValue("kind"), name, body, ifMatch)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "切换资源使用范围")
		return
	}
	s.setETagHeader(w, view.ETag)
	s.writeOK(w, view)
}

// setETagHeader 在条件值已解析时写 `ETag` 响应头；读回失败（空串）时省略——
// 空值的 `ETag:` 会让客户端把空串当成当前值去重试。
// setETagHeader omits an empty value so a failed read-back never becomes "current".
func (s *Server) setETagHeader(w http.ResponseWriter, etag string) {
	if etag == "" {
		return
	}
	w.Header().Set("ETag", etag)
}
