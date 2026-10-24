package app

import (
	"context"
	"log/slog"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
)

// newNotifyAdapters 是类型支持集的唯一真值：新增类型只改这里——工具内核只按 channel.Type
// 查表，不出现类型判断分支；映射恰好 `webhook` / `feishu` 两键（R11 逐字判卷）。
// newNotifyAdapters is the single source of truth for supported channel types; the tool
// kernel only looks up this map and never branches on the type string.
func newNotifyAdapters() map[string]tool.NotifyAdapter {
	return map[string]tool.NotifyAdapter{
		"webhook": tool.NewWebhookNotifyAdapter(),
		"feishu":  tool.NewFeishuNotifyAdapter(),
	}
}

// newNotifyTool 装配发送面：渠道清单每次发送都读数据库（写后下一发即可见，零缓存），
// 类型 → 适配器映射与 `/notify-channels/types` 共用 `newNotifyAdapters`（约束 38 的
// 唯一真值），凭据在**每次发送时**解析（env 读 `${VAR}`、managed 读当前 active 版本，
// R4.8）——工具内核不回退明文，也不缓存已解析的地址。`credentials` 为 nil 时 managed
// 渠道一律失败关闭（本实例没有托管凭据解析面）；`deliveries` 为 nil 时不写投递记录。
// 本装配不带 logger：US-8 冻结的工具接口不含日志面，失败由执行器统一记 WARN
// （`core/tool_exec.go`），`detail` 里点名渠道与变量；级别差异见 run-state 的派生记录。
// newNotifyTool wires the send path: the registry is read from the database on every
// send, the adapter map is shared with the types endpoint, and credentials are resolved
// per send with no plaintext fallback.
func newNotifyTool(channels core.NotifyChannelReader, policy core.Policy,
	credentials tool.NotifyCredentialResolver, deliveries tool.NotifyDeliveryRecorder,
) *tool.NotifyTool {
	return tool.NewNotifyTool(channels, policy, newNotifyAdapters(), credentials, deliveries)
}

// notifyDeliveryRecorder 把 `notify_deliveries` 的写入降级为「记日志、不失败」：
// 这张表是派生账，权威审计是执行器预写的 `tool_invocations` 行（fail-closed）；发送
// 已经完成，派生行写失败**不得**把结果改判为失败——模型据此重试就是重复推送。
// notifyDeliveryRecorder logs a failed derived-record write instead of failing the send.
type notifyDeliveryRecorder struct {
	store  *storage.NotifyDeliveryStore
	logger *slog.Logger
}

// Record 写一条投递记录；失败记 WARN（固定键 run_id / tool_invocation_id / channel）。
// Record writes one delivery row and downgrades a failure to WARN.
func (r notifyDeliveryRecorder) Record(ctx context.Context, record core.NotifyDeliveryRecord) error {
	if err := r.store.Record(ctx, record); err != nil {
		if r.logger != nil {
			r.logger.WarnContext(ctx, "写入通知投递记录失败",
				"run_id", record.RunID,
				"tool_invocation_id", record.ToolInvocationID,
				"channel", record.ChannelName,
				"error", err)
		}
	}
	return nil
}
