package web

// 本文件是通知渠道的只读面（模块卡 §3.1.29）：列表只回 `NotifyChannelView`，地址
// 只以 scheme + host（`url_host`）出现——原始 URL（可能是 `${VAR}` 引用，也可能是
// 历史行里误存的真实地址）与查询串**没有任何响应载体**（凭证红线，约束 25 同口径）。
// 类型列表来自装配层显式注册表（`notify_adapters` 的键集排序），本层不维护第二份。
// Read-only notify channels; the address leaves only as scheme+host, and the type
// list comes from the assembly layer's single explicit registry.
// 本 US **不**提供创建 / 更新 / 删除（US-12 落地写面）。

import (
	"context"
	"net/http"
	"net/url"

	"github.com/ymedword/basalt/internal/core"
)

// handleListNotifyChannels 处理 `GET /api/v1/notify-channels`：端口未装配按 500
// 固定文案拒绝（装配缺失是错误，不是空列表——空列表会让运维以为"配了但没有渠道"）。
// handleListNotifyChannels serves the channel list; a missing port is a fixed 500,
// never an empty list.
func (s *Server) handleListNotifyChannels(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.Notify == nil {
		s.logger().Error("通知渠道端口未装配，/notify-channels 无法读取")
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	channels, err := s.opts.Notify.ListNotifyChannels(r.Context())
	if err != nil {
		s.logger().Error("列出通知渠道失败", "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	names := make([]string, 0, len(channels))
	for _, ch := range channels {
		names = append(names, ch.Name)
	}
	references, err := s.notifyReferences(r.Context(), names)
	if err != nil {
		// 失败关闭：空引用集合会被页面读成"没有任何 Agent 引用"。
		// Fail closed: an empty map would read as "no agent references this channel".
		s.logger().Error("扫描通知渠道引用失败", "error", err)
		s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
		return
	}
	views := make([]NotifyChannelView, 0, len(channels))
	for _, ch := range channels {
		views = append(views, notifyChannelView(ch, references[ch.Name]))
	}
	s.writeOK(w, views)
}

// notifyReferences 查询一组渠道的文档引用者。端口未装配（测试 / 无工作区嵌入）时
// 返回空映射；读面**不得**把查询失败降级成空映射（见 handleListNotifyChannels）。
// notifyReferences resolves document references; a missing port is an empty map, but
// a failed scan is never downgraded to one.
func (s *Server) notifyReferences(ctx context.Context, names []string) (map[string][]string, error) {
	if s.opts.NotifyReferences == nil || len(names) == 0 {
		return map[string][]string{}, nil
	}
	return s.opts.NotifyReferences.NotifyReferences(ctx, names)
}

// handleNotifyTypes 处理 `GET /api/v1/notify-channels/types`：返回装配层登记的类型名，
// 顺序由装配层固定（有序表）；空注册表输出 `[]`。
// handleNotifyTypes serves the assembled type names in their fixed order.
func (s *Server) handleNotifyTypes(w http.ResponseWriter, _ *http.Request, _ core.Actor) {
	s.writeOK(w, nonNilStrings(s.opts.NotifyTypes))
}

// notifyURLHost 把已存地址投影成 `scheme://host`；不是带 scheme + host 的绝对地址
// （含 `${VAR}` 引用、相对路径、空串）一律回空串——解析失败**不得**退回原文案。
// notifyURLHost projects an absolute address to scheme+host; anything unparsable,
// including a `${VAR}` reference, yields an empty string and never the raw value.
func notifyURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
