package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 config_snapshots 的**唯一 SQL 入口**。表 / 列 / 唯一键以 docs/contracts/schema.sql
// 为准（此处不另抄列清单）。落库纪律两条：① **insert-if-absent**——`UNIQUE(agent_name, hash)`
// 冲突即 no-op，内容寻址意味着同一内容只有一个版本，重复落库不得改写既有行；
// ② **清单与哈希同源**——`manifest_json` 列写的就是 `hash` 所派生的那份字节，
// 落库侧重序列化（哪怕只差字段名大小写）都会让两列脱钩（DDL 注释："二者永不脱钩"）。
// This file is the only SQL entry point for config_snapshots: insert-if-absent on the
// content-addressed unique key, and the manifest JSON written byte-for-byte as the bytes
// the hash was derived from.

// SnapshotStore 是 config_snapshots 的唯一 SQL 入口（core.SnapshotStore 的实现）。
// SnapshotStore is the only SQL entry point for config_snapshots.
type SnapshotStore struct{ db *DB }

// 编译期断言：SnapshotStore 满足消费方（core）声明的快照落库接口。
// Compile-time assertion that SnapshotStore satisfies the consumer-declared interface.
var _ core.SnapshotStore = (*SnapshotStore)(nil)

// NewSnapshotStore 绑定一个已打开的写模式库句柄。
// NewSnapshotStore binds the store to an opened writable database handle.
func NewSnapshotStore(db *DB) *SnapshotStore { return &SnapshotStore{db: db} }

// SaveSnapshot 保存一份运行级配置快照；失败 = 调用方**拒绝启动**该次运行（零模型调用）。
// SaveSnapshot persists one run-level config snapshot; a failure means the caller refuses
// to start the run.
//
// **单条 INSERT 即一个短事务**（SQLite 隐式事务）——事务不得跨外部调用的纪律在这里体现为
// "根本不显式开事务"：本函数没有任何跨语句的一致性需求，begin/commit 只会拉长写锁持有时间。
// 冲突用 `ON CONFLICT (agent_name, hash) DO NOTHING` 而非 `INSERT OR IGNORE`：后者会连
// NOT NULL 之类的错误一并吞掉，把"表结构对不上"伪装成"重复快照"。
// One implicit-transaction INSERT; the conflict clause names the unique key exactly so that
// unrelated constraint failures still surface.
func (s *SnapshotStore) SaveSnapshot(ctx context.Context, snap *core.ConfigSnapshot) error {
	if snap == nil {
		return errors.New("保存配置快照：快照为空")
	}

	manifest, err := json.Marshal(snap.Manifest)
	if err != nil {
		return fmt.Errorf("序列化冻结清单（agent=%s hash=%s）: %w", snap.AgentName, snap.Hash, err)
	}

	// created_at 取落库侧时钟：冻结的 `ConfigSnapshot` 字段集与 `SaveSnapshot` 签名
	// （§3.1）都没有时间位，而该列只是"这行什么时候写的"——它**不进**哈希，
	// 同内容不同时刻仍是同一版本（§2.2 约束 12）。
	// created_at comes from the store's own clock: the frozen snapshot value and signature
	// carry no time, and the column never feeds the hash.
	_, err = s.db.write.ExecContext(ctx,
		`INSERT INTO config_snapshots (agent_name, hash, content, manifest_json, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (agent_name, hash) DO NOTHING`,
		snap.AgentName, snap.Hash, snap.Content, string(manifest), time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("保存配置快照（agent=%s hash=%s）: %w", snap.AgentName, snap.Hash, err)
	}
	return nil
}
