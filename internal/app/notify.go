package app

import (
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
