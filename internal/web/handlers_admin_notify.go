package web

// 本文件是通知渠道的写面（模块卡 §3.1.1 行 22 / §3.7.1）：`POST / PUT / DELETE`
// 三条写端点；`GET /notify-channels` 与 `/notify-channels/types` 仍由 US-11 的
// `handlers_notify_read.go` 提供，本文件**不**重复注册。请求体只经
// `decodeAdminWriteBody` 解码（未知字段拒绝）；名称 / 类型 / 占位 / 白名单校验
// 全部在适配器预写前完成（约束 38），本层只做形状投影与错误分类映射。
// 成功响应一律经 `notifyChannelView` 出站——地址只以 scheme + host（`url_host`）
// 出现，原始 URL（`${VAR}` 引用或历史行里的真实地址）**没有任何响应载体**。
// This file is the notify-channel write surface. The two GET endpoints stay in the
// US-11 read file and are not re-registered here; every success response leaves
// through notifyChannelView, so the address appears only as scheme+host.

import (
	"context"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// messageNotifyChannelBody 是请求体不是合法 JSON / 含未知字段时的固定文案。
	messageNotifyChannelBody = "请求体不是合法的通知渠道请求"
	// messageNotifyChannelNameMismatch 点明路径渠道名与请求体 name 冲突（更新）。
	messageNotifyChannelNameMismatch = "路径中的渠道名与请求体 name 不一致"
	// messageNotifyForceValue 是 `force` 取值的固定拒绝文案：契约枚举只有字面 true。
	messageNotifyForceValue = "force 只接受字面 true"
)

// handleAdminCreateNotifyChannel 处理 `POST /notify-channels`：端口未装配按 500
// 固定文案失败关闭；请求体原样交给适配器（默认停用语义、同名 400、占位与白名单
// 预检都在适配器内完成），成功回 `NotifyChannelView`。
// handleAdminCreateNotifyChannel forwards the body verbatim; the duplicate-name 400,
// placeholder checks and the domain pre-check all stay in the adapter.
func (s *Server) handleAdminCreateNotifyChannel(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	if s.opts.NotifyAdminService == nil {
		s.writeAdminPortMissing(w, "NotifyAdminService", "POST /notify-channels")
		return
	}
	var body NotifyChannelWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageNotifyChannelBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if s.rejectCredentialRef(w, body.CredentialRef) {
		return
	}
	channel, err := s.opts.NotifyAdminService.Create(r.Context(), actor, body)
	if err != nil {
		s.writeResourceAdminError(w, err, body.Name, "创建通知渠道")
		return
	}
	s.writeOK(w, s.notifyChannelWriteView(r.Context(), channel))
}

// notifyChannelWriteView 是写成功后的返回投影：写已经落盘，引用扫描失败只降级展示
// 并记 WARN——不能回 500 让客户端以为写失败而重试（重试会撞同名 400 / 覆盖更新）。
// A post-write reference-scan failure only degrades the view; the write already landed.
func (s *Server) notifyChannelWriteView(ctx context.Context, channel core.NotifyChannel) NotifyChannelView {
	references, err := s.notifyReferences(ctx, []string{channel.Name})
	if err != nil {
		s.logger().Warn("通知渠道写成功但引用扫描失败", "channel", channel.Name, "error", err)
		references = map[string][]string{}
	}
	return notifyChannelView(channel, references[channel.Name])
}

// handleAdminUpdateNotifyChannel 处理 `PUT /notify-channels/{name}`：路径是渠道的
// 唯一身份；请求体 `name` 留空或缺省时按路径补齐，非空且不一致时 400（纯请求
// 校验，早于任何写动作）。`url` 留空 = 保留原值（契约 NotifyChannelUpsert）——
// 本层**不**读存储、**不**回填地址，是否保留由适配器在写锁内解释。
// handleAdminUpdateNotifyChannel takes the path as the channel identity and forwards
// a blank url verbatim; whether it keeps the stored reference is the adapter's call.
func (s *Server) handleAdminUpdateNotifyChannel(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.NotifyAdminService == nil {
		s.writeAdminPortMissing(w, "NotifyAdminService", "PUT /notify-channels/{name}")
		return
	}
	var body NotifyChannelWrite
	if err := s.decodeAdminWriteBody(w, r, &body, messageNotifyChannelBody); err != nil {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if s.rejectCredentialRef(w, body.CredentialRef) {
		return
	}
	if body.Name != "" && body.Name != name {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageNotifyChannelNameMismatch)
		return
	}
	body.Name = name
	channel, err := s.opts.NotifyAdminService.Update(r.Context(), actor, name, body)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "修改通知渠道")
		return
	}
	s.writeOK(w, s.notifyChannelWriteView(r.Context(), channel))
}

// handleAdminDeleteNotifyChannel 处理 `DELETE /notify-channels/{name}`：整词引用
// 扫描、被引用 400 点名与 `detail_json.forced=true` 都在适配器内；本层只解析
// `?force=true` 字面量——契约枚举只有 `true`，其余取值（含 `false` / `1` /
// `TRUE`）按 400 拒绝且**不**触达端口，成功回 200 + 空 data。`resource_uid` 是契约
// 的必需查询参数（涉及资源身份的动作），本层原样下传，由适配器在预写前与当前指针
// 比较——缺失或不一致 → 409 `RESOURCE_UID_MISMATCH`，不按名称猜测目标。
// handleAdminDeleteNotifyChannel also forwards resource_uid verbatim; the adapter
// compares it against the current pointer before any operation row exists.
func (s *Server) handleAdminDeleteNotifyChannel(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.NotifyAdminService == nil {
		s.writeAdminPortMissing(w, "NotifyAdminService", "DELETE /notify-channels/{name}")
		return
	}
	force, ok := parseNotifyForce(r.URL.Query().Get("force"))
	if !ok {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageNotifyForceValue)
		return
	}
	resourceUID := r.URL.Query().Get("resource_uid")
	if err := s.opts.NotifyAdminService.Delete(r.Context(), actor, name, resourceUID, force); err != nil {
		s.writeResourceAdminError(w, err, name, "删除通知渠道")
		return
	}
	s.writeOK(w, nil)
}

// handleAdminTestNotifyChannel 处理 `POST /notify-channels/{name}/test`：先写管理
// 操作行，再经发送面发出一条固定的「Basalt 连通性测试」消息；结果三值投影——成功、
// 确定失败与结果未知（超时 / 回执无法解析）都是 200：未知是**测试结论**而不是接口
// 错误，页面据 `status=unknown` 提示"可能已送达、请查证后手动重试"，本层**不**重试。
// 只有请求本身不成立（渠道不存在 / 未配置地址 / 无发送面 / 预写失败）才走错误信封。
// handleAdminTestNotifyChannel sends one fixed connectivity message after the operation
// pre-write; a completed send with a definite failure stays 200 with ok=false.
func (s *Server) handleAdminTestNotifyChannel(w http.ResponseWriter, r *http.Request, actor core.Actor) {
	name := r.PathValue("name")
	if s.opts.NotifyAdminService == nil {
		s.writeAdminPortMissing(w, "NotifyAdminService", "POST /notify-channels/{name}/test")
		return
	}
	result, err := s.opts.NotifyAdminService.TestChannel(r.Context(), actor, name)
	if err != nil {
		s.writeResourceAdminError(w, err, name, "测试通知渠道")
		return
	}
	s.writeOK(w, notifyChannelTestResultView(result))
}

// parseNotifyForce 把 `force` 查询参数解释为强删开关：缺省或空串 = false，
// 字面 `true` = true，其余一律不合法（调用方按 400 拒绝）。
// parseNotifyForce accepts only an absent value or the literal "true".
func parseNotifyForce(raw string) (bool, bool) {
	switch raw {
	case "":
		return false, true
	case "true":
		return true, true
	default:
		return false, false
	}
}
