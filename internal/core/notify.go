package core

import "context"

// NotifyChannel 是一条通知渠道的只读视图（`notify_channels` 表的三个消费列）。
// `URL` 列**必须**是精确 `${VAR}` 引用——真实地址只在发送时经进程环境解析，
// 本类型与它的任何消费者都**不得**把原值写进对外文案。
// NotifyChannel is the read-only view of one notification channel. URL must be an exact
// ${VAR} reference; the resolved address never leaves the send path.
type NotifyChannel struct {
	Name string // 渠道名（主键；大小写敏感精确匹配）
	Type string // 适配器类型（webhook / feishu，由装配层映射裁决）
	URL  string // `${VAR}` 引用；本 US 零明文
	// Description 是渠道说明（`notify_channels.description`，可空）；空列读成空串——
	// 契约 NotifyChannelView.description 是非空余类型字符串，不是 null。
	// Description is the optional channel note; a NULL column reads as an empty string.
	Description string
}

// NotifyChannelReader 是渠道注册表的只读面（消费方声明，实现在 `storage`）：
// 每次发送读取一次、不缓存——增删改在下一发即生效。
// NotifyChannelReader is the read-only registry face declared by the consumer; every send
// reads it once, with zero caching.
type NotifyChannelReader interface {
	ListNotifyChannels(ctx context.Context) ([]NotifyChannel, error)
}
