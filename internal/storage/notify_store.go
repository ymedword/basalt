package storage

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
)

// NotifyChannelRepo 是 `notify_channels` 的**只读**仓储（本 US 零写入；增删改查归
// US-12）。每次调用都执行同一条查询、走只读连接池：零缓存意味着管理端或手工 SQL
// 的改动在**下一发**即生效，不需要失效策略，也不引入新的配置键。
// NotifyChannelRepo is the read-only registry repository: every call runs the one query
// against the read pool, so changes made elsewhere take effect on the next send.
type NotifyChannelRepo struct {
	db *DB
}

// 编译期断言：仓储满足消费方声明的只读面（依赖倒置——接口在 core，实现在本包）。
// Compile-time assertion that the repo satisfies the consumer-side read face.
var _ core.NotifyChannelReader = (*NotifyChannelRepo)(nil)

// NewNotifyChannelRepo 绑定一个已经打开的库句柄；仓储不持有状态，可在一次装配里
// 与工具共享同一句柄。
// NewNotifyChannelRepo binds an opened handle; the repo holds no state of its own.
func NewNotifyChannelRepo(db *DB) *NotifyChannelRepo {
	return &NotifyChannelRepo{db: db}
}

// ListNotifyChannels 按 `name` 升序返回全部渠道（只取消费的三列）。空表返回**空切片**
// 而非错误——"没有渠道可发"与"读取失败"是两条不同的诊断路径。读取失败**原样上抛**：
// 文案包装归工具层（本包不决定对外措辞）。
// ListNotifyChannels returns all channels ordered by name; the empty registry is an empty
// slice, not an error. Read failures propagate verbatim — wording belongs to the tool.
func (r *NotifyChannelRepo) ListNotifyChannels(ctx context.Context) ([]core.NotifyChannel, error) {
	rows, err := r.db.read.QueryContext(ctx,
		`SELECT name, type, url FROM notify_channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]core.NotifyChannel, 0)
	for rows.Next() {
		var ch core.NotifyChannel
		if err := rows.Scan(&ch.Name, &ch.Type, &ch.URL); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
