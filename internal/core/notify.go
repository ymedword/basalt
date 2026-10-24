package core

import "context"

// NotifyChannel 是一条通知渠道的领域视图（`notify_channels` 行）。
// `URL` 列**必须**是精确 `${VAR}` 引用（env 模式）或空串（managed 模式，地址以密文存放在
// credential_versions）；真实地址只在发送时解析，本类型与它的任何消费者都**不得**把原值
// 写进对外文案。`ResourceUID` 是身份，`Name` 只是当前标签——同名重建必然是新 UID。
// NotifyChannel is the domain view of one notification channel. URL is an exact ${VAR}
// reference (env) or empty (managed, ciphertext lives in credential_versions); the uid is
// the identity and the name is only the current label.
type NotifyChannel struct {
	ResourceUID ResourceUID // 资源实例身份；同名重建生成新 UID
	Name        string      // 渠道名（当前标签；活动名称唯一）
	Type        string      // 适配器类型（webhook / feishu，由装配层映射裁决）
	URL         string      // env：`${VAR}` 引用；managed：空串；本 US 零明文
	// Description 是渠道说明（`notify_channels.description`，可空）；空列读成空串——
	// 契约 NotifyChannelView.description 是非空余类型字符串，不是 null。
	// Description is the optional channel note; a NULL column reads as an empty string.
	Description string
	// URLHost 是展示用 scheme + host（不含 path / query）：渠道地址的路径与查询串
	// 本身就是凭证，展示面只给主机部分。解析不出时为空串（空不等于未配置）。
	// URLHost is the display-only scheme + host; empty when unparsable.
	URLHost string
	// CredentialMode 是地址两模式：env（`${VAR}` 引用）或 managed（密文托管）。
	CredentialMode CredentialMode
	// Scope / OwnerAgentUID 是使用范围（shared / exclusive 与专属属主）。
	Scope         ResourceScope
	OwnerAgentUID AgentUID
	CredentialID  string // managed 模式的当前凭据身份；env 模式为空
	Revision      int    // 每次地址 / 凭据 / 范围变更 +1；参与资源 ETag

	// 以下四项**只由管理读面**（app 层）补齐，发送路径不读它们——零值即"没查"，
	// 不是"没有凭据"：`CredentialState` 为空时视图按模式判定，不臆造状态。
	// The four fields below are filled only by the admin read face (app); the send path
	// leaves them zero, and an empty state is never interpreted as "no credential".
	OwnerAgentName    string                  // exclusive 时的所有者 Agent **名**；shared 为空
	CredentialState   CredentialIdentityState // 凭据身份状态；env 或未写入为空
	CredentialVersion int                     // 当前活动凭据版本号；env 为 0
	Last4             string                  // 托管密钥尾号；长度不足 8 位为空
	// ETag 是这一行的**条件 ETag**（定义 + UID + revision + 范围 + 凭据状态），由 app 层
	// 用唯一一份配方计算；web 只投影，**不得**另算一份。
	// ETag is the row's conditional ETag, computed by the single recipe in app.
	ETag string
	// Availability / LastTest 同样只由管理读面补齐（R7 范围修订）：前者回答"本地配置
	// 看起来能不能用"（ready / unconfigured / unavailable），后者是最后一次**实测**
	// 结果；两者分开展示，**不得**用前者冒充后者，nil 表示从未测过。
	// Availability and LastTest are admin-read projections too; local readiness never
	// stands in for a live test, and a nil LastTest means "never tested".
	Availability string
	LastTest     *ResourceConnectionTest
	CreatedAt    int64  // Unix 毫秒
	UpdatedAt    int64  // Unix 毫秒
	DeletedAt    *int64 // Unix 毫秒；nil = 未删除（墓碑行不出现在活跃视图）
	OperationID  string // 最后一次写入的管理操作
}

// NotifyChannelReader 是渠道注册表的只读面（消费方声明，实现在 `storage`）：
// 每次发送读取一次、不缓存——增删改在下一发即生效。
// NotifyChannelReader is the read-only registry face declared by the consumer; every send
// reads it once, with zero caching.
type NotifyChannelReader interface {
	ListNotifyChannels(ctx context.Context) ([]NotifyChannel, error)
}
