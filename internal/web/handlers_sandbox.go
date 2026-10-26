package web

// 本文件是沙箱白名单端点（模块卡 §3.1.28 与 R8.2.2 / R8.2.4）：GET 投影四类白名单，
// 其中出网域名是**当前生效**值并附 `network_allowlist` 元数据；PUT 以逐字强
// `If-Match` 条件写这一份列表，保存成功即热生效、不重启。其余三类仍是部署者配置，
// 页面不提供写入口，也不谎造"改了就生效"。
// 投影在 `sandboxView`：路径只出相对展示形态，宿主绝对路径与解析后的凭证一律不出站。
// The sandbox view plus the conditional write of the outbound allowlist; the other
// three lists stay deployment configuration, and the projection keeps relative paths.

import (
	"errors"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// 固定公开文案（R8.2.4）：条件写不满足、修订号冲突与请求体缺失各一条。中文是
// 管理台与用户可见语言；日志模板仍为英文（R8 裁决 1）。
// Fixed public Chinese texts for the conditional write; logs stay English.
const (
	networkAllowlistStaleMessage   = "网络域名白名单已被其他操作修改，请刷新后重试"
	networkAllowlistBodyMessage    = "请求体必须是 {\"domains\": [...]} 且 domains 为字符串数组"
	networkAllowlistDomainsMissing = "请求体缺少 domains 字段（清空白名单请显式传空数组）"
)

// networkAllowlistWrite 是 PUT 的请求体：`domains` 用指针区分"未提供"（400）与
// "显式空数组"（合法：清空白名单 = 全部拒绝，R8.2.1 第 3 条）。
// The pointer distinguishes an absent field from an explicit empty list.
type networkAllowlistWrite struct {
	Domains *[]string `json:"domains"`
}

// handleSandbox 处理 `GET /api/v1/sandbox`（契约 openapi.yaml）。
// handleSandbox serves the sandbox allowlist view.
func (s *Server) handleSandbox(w http.ResponseWriter, _ *http.Request, _ core.Actor) {
	s.writeOK(w, s.sandboxView())
}

// handleAdminUpdateNetworkDomains 处理 `PUT /api/v1/sandbox/network-domains`
// （R8.2.2）：纯请求校验（请求体、If-Match 形状）在本层完成且零写入；校验失败的
// 400 点名第一处非法项，412 带 `data.current_etag`，写后失败一律带 `operation_id`。
// 成功响应是**重新读出的**生效视图——与随后 GET 的字段一致，不做乐观回显。
// The handler validates the body and If-Match shape before any write; the success
// payload is the live view, not an optimistic echo.
func (s *Server) handleAdminUpdateNetworkDomains(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.NetworkAllowlist == nil {
		s.writeAdminPortMissing(w, "NetworkAllowlist", "PUT /sandbox/network-domains")
		return
	}
	var body networkAllowlistWrite
	if err := s.decodeAdminWriteBody(w, r, &body, networkAllowlistBodyMessage); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if body.Domains == nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, networkAllowlistDomainsMissing)
		return
	}
	ifMatch, ok := parseIfMatch(r.Header.Get("If-Match"))
	if !ok {
		s.writeError(w, http.StatusPreconditionFailed, codePreconditionFailed, ifMatchRequiredMessage)
		return
	}

	_, operationID, err := s.opts.NetworkAllowlist.Update(r.Context(), *body.Domains, ifMatch, actor)
	if err != nil {
		s.writeNetworkAllowlistError(w, err, operationID)
		return
	}
	s.writeOK(w, s.sandboxView())
}

// writeNetworkAllowlistError 把白名单条件写失败交给共用映射表，只提供本表的公开
// 文案与当前 ETag 端口（与禁止表同构，禁止另写一套状态码）。
// writeNetworkAllowlistError delegates to the shared mapping with this row's text.
func (s *Server) writeNetworkAllowlistError(w http.ResponseWriter, err error, operationID string) {
	s.writeNetworkDomainPolicyError(w, err, operationID, s.opts.NetworkAllowlist, networkAllowlistStaleMessage)
}

// writeNetworkDomainPolicyError 是两张网络域名表（出网白名单 / 禁止表）共用的失败
// 映射：请求校验 400、条件不满足 412（带当前 ETag）、修订号冲突 409（只带操作
// 标识）、其余 500。两张表的写入语义逐条一致，映射**不得**分叉；调用方只替换公开
// 文案与读取当前 ETag 的端口。
// The shared failure mapping for both policy rows; callers only swap the public text
// and the port that yields the current ETag.
func (s *Server) writeNetworkDomainPolicyError(w http.ResponseWriter, err error, operationID string,
	port core.NetworkAllowlistAdmin, staleMessage string,
) {
	var invalid *core.InvalidInputError
	switch {
	case errors.As(err, &invalid):
		// NormalizeDomainList 的 Reason 已点名第一处非法项（含序号）；Tool 名不外露。
		s.writeError(w, http.StatusBadRequest, codeBadRequest, invalid.Reason)
	case errors.Is(err, core.ErrPreconditionFailed):
		s.writeAdminDataError(w, http.StatusPreconditionFailed, codePreconditionFailed,
			staleMessage, staleAllowlistError(port.Current(), staleMessage))
	case errors.Is(err, core.ErrPolicyRevisionConflict):
		// 进程内互斥之后仍撞修订号：另一进程已写入。此时 `pending` 已写、必须带
		// 操作标识（约束 1a）；但落败进程的内存快照可能仍落后于数据库，**不**承诺
		// `current_etag`（契约只对 412 给出该字段）。服务端已在失败路径重读权威行，
		// 页面重新 GET 即可拿到当前值。
		// A CAS loser cannot vouch for the current ETag; only the operation id ships.
		s.writeAdminDataError(w, http.StatusConflict, codeConflict,
			staleMessage, staleAllowlistConflictError(staleMessage, operationID))
	default:
		s.logger().Error("network domain policy conditional write failed",
			"error", err, "operation_id", operationID)
		if operationID == "" {
			s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
		// `pending` 已写：失败必须可经 `GET /operations/{id}` 追查（约束 1a）。
		// data 经同一白名单投影（ErrorDataView）出站，不另开序列化出口。
		s.writeEnvelope(w, http.StatusInternalServerError, Envelope{
			Code: codeInternal, Message: messageInternal,
			Data:      ErrorDataView{OperationID: &operationID},
			Timestamp: s.timestamp(),
		})
	}
}

// staleAllowlistConflictError 组装 409 的失败数据：只带操作标识，不带
// `current_etag`——CAS 落败方无法保证自己的快照就是数据库当前值。
// The 409 payload carries the operation id only, never a possibly stale ETag.
func staleAllowlistConflictError(message, operationID string) error {
	data := map[string]any{}
	if operationID != "" {
		data["operation_id"] = operationID
	}
	return &core.AdminError{
		Kind: core.ErrPolicyRevisionConflict, Message: message, Data: data,
	}
}

// staleAllowlistError 组装"当前 ETag（+ 可选操作标识）"的失败数据：页面按
// `current_etag` 重排输入即可重试，**不**自动重发。
// staleAllowlistError builds the stale-condition payload from the live snapshot.
func staleAllowlistError(policy core.NetworkDomainPolicy, message string, operationID ...string) error {
	data := map[string]any{"current_etag": core.NetworkDomainETag(policy)}
	if len(operationID) > 0 && operationID[0] != "" {
		data["operation_id"] = operationID[0]
	}
	return &core.AdminError{Kind: core.ErrPreconditionFailed, Message: message, Data: data}
}
