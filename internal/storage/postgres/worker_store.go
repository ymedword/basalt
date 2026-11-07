package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `worker_nodes` 的唯一 SQL 入口（US-24 T006 / FR-24-01）。
//
// 三条边界：
//
//  1. **心跳不是占有权证明**。`last_heartbeat_at` 只服务运维诊断；任何归属判定都走
//     `execution_leases.expires_at`（回收器与租约仓储），不得按心跳或启动时间推断
//     某个 Worker "已经死了"。
//  2. **身份是 InstanceID**。`worker_name` 允许重复（两台机器同名是运维常态），
//     主键只有 instance_id；同名不等于同进程。
//  3. **时间只取数据库时钟**，与本包租约仓储同一口径——多宿主的本机时钟偏差不得
//     混进诊断时间线。
//
// This file is the only SQL entry point for worker_nodes. Heartbeats are diagnostics
// only: ownership is decided exclusively by execution_leases.expires_at. The identity
// is the instance id; names may collide. Timestamps come from the database clock.

// WorkerStore 是 core.WorkerRegistry 的 PostgreSQL 实现。
// WorkerStore is the PostgreSQL implementation of the worker registry port.
type WorkerStore struct{ db *DB }

// 编译期断言：实现满足 core 声明的登记端口。
var _ core.WorkerRegistry = (*WorkerStore)(nil)

// NewWorkerStore 绑定一个已打开的写模式库句柄。
func NewWorkerStore(db *DB) *WorkerStore { return &WorkerStore{db: db} }

// sqlNowMillis 是写入用的数据库时钟表达式（单个 SQL 内联，避免多一次往返）。
// 名字刻意不与测试助手的 `dbNowMillis` 撞车——同包测试在同一编译集里。
// sqlNowMillis inlines the database clock so a write needs no extra round trip.
const sqlNowMillis = `(extract(epoch from clock_timestamp()) * 1000)::bigint`

// Register 登记一个 Worker 实例。同一 instance_id 重复登记是幂等覆盖：更新诊断字段
// 与心跳、清掉 stopped_at（进程重启复用同一标识时视为重新上线）。schema_version 只
// 在登记时写入——运行中进程实际装载的版本不会变，心跳不重复上报。
//
// 身份必填 InstanceID 与 SchemaVersion：空标识会让两台机器落成同一行，非正版本号
// 说明装配面没把实际 schema 版本传下来，两者都必须在入口拒绝而不是留脏行。
// Register upserts the diagnostic row for one instance; schema_version is written once.
func (s *WorkerStore) Register(ctx context.Context, worker core.WorkerIdentity) error {
	if worker.InstanceID == "" {
		return errors.New("postgres: Worker 实例标识为空")
	}
	if worker.SchemaVersion <= 0 {
		return fmt.Errorf("postgres: Worker %s 的 schema 版本非法（%d）", worker.InstanceID, worker.SchemaVersion)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
                         started_at, last_heartbeat_at, stopped_at)
VALUES ($1, $2, $3, $4, $5, `+sqlNowMillis+`, `+sqlNowMillis+`, NULL)
ON CONFLICT (instance_id) DO UPDATE
   SET worker_name       = EXCLUDED.worker_name,
       hostname          = EXCLUDED.hostname,
       build_version     = EXCLUDED.build_version,
       schema_version    = EXCLUDED.schema_version,
       started_at        = EXCLUDED.started_at,
       last_heartbeat_at = EXCLUDED.last_heartbeat_at,
       stopped_at        = NULL`,
		worker.InstanceID, worker.Name, worker.Hostname, worker.Version, worker.SchemaVersion)
	if err != nil {
		return fmt.Errorf("登记 Worker %s: %w", worker.InstanceID, err)
	}
	return nil
}

// Heartbeat 刷新诊断心跳。已登记但已停止的实例拒绝心跳——那说明停机流程之后还有循环
// 在跑，属于生命周期缺陷，必须让调用方看见而不是静默续命。
// Heartbeat refreshes the diagnostic timestamp; a stopped instance refuses further beats.
func (s *WorkerStore) Heartbeat(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return errors.New("postgres: Worker 实例标识为空")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE worker_nodes
   SET last_heartbeat_at = `+sqlNowMillis+`
 WHERE instance_id = $1 AND stopped_at IS NULL`, instanceID)
	if err != nil {
		return fmt.Errorf("postgres: Worker %s 心跳失败: %w", instanceID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: Worker %s 心跳结果不可读: %w", instanceID, err)
	}
	if affected == 1 {
		return nil
	}
	var stopped sql.NullInt64
	err = s.db.QueryRowContext(ctx,
		`SELECT stopped_at FROM worker_nodes WHERE instance_id = $1`, instanceID).Scan(&stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("postgres: Worker %s 未登记", instanceID)
	}
	if err != nil {
		return fmt.Errorf("读取 Worker %s 登记行: %w", instanceID, err)
	}
	return fmt.Errorf("postgres: Worker %s 已停止，不得继续心跳", instanceID)
}

// ListNodes 返回全部 Worker 实例的只读运维投影（`basalt workers list` 与系统概览）。
//
// 活跃租约数按 `owner_worker_id` 聚合 `execution_leases`：未释放且**未过期**（数据库
// 时钟）才算有效——已过期但尚未被回收的租约不再代表任何仍在提交的执行权，把它算成
// 活跃会让诊断页显示一个已经不存在的执行者（FR-24-15 的同一判据：不按心跳或启动
// 时间推断生死，只看 expires_at）。
//
// 排序固定 `started_at DESC, instance_id`：同名实例按标识稳定排序，两次读取不会
// 因为 map 顺序互换行。
// ListNodes returns every worker node with its active (unreleased, unexpired) lease
// count; expired leases stop counting because they no longer represent a submitter.
func (s *WorkerStore) ListNodes(ctx context.Context) ([]core.WorkerNodeView, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT w.instance_id, w.worker_name, w.hostname, w.build_version, w.schema_version,
       w.started_at, w.last_heartbeat_at, w.stopped_at,
       COALESCE(l.active_leases, 0)
  FROM worker_nodes w
  LEFT JOIN (SELECT owner_worker_id, count(*) AS active_leases
               FROM execution_leases
              WHERE released_at IS NULL
                AND expires_at > `+sqlNowMillis+`
              GROUP BY owner_worker_id) l
         ON l.owner_worker_id = w.instance_id
 ORDER BY w.started_at DESC, w.instance_id`)
	if err != nil {
		return nil, fmt.Errorf("读取 Worker 列表: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 非 nil 空切片：调用方把"没有 Worker"编码为 `[]` 而不是 `null`。
	out := make([]core.WorkerNodeView, 0)
	for rows.Next() {
		var (
			node      core.WorkerNodeView
			started   int64
			heartbeat int64
			stopped   sql.NullInt64
		)
		if err := rows.Scan(&node.InstanceID, &node.Name, &node.Hostname, &node.BuildVersion,
			&node.SchemaVersion, &started, &heartbeat, &stopped, &node.ActiveLeases); err != nil {
			return nil, fmt.Errorf("读取 Worker 列表行: %w", err)
		}
		node.StartedAt = time.UnixMilli(started).UTC()
		node.LastHeartbeatAt = time.UnixMilli(heartbeat).UTC()
		if stopped.Valid {
			at := time.UnixMilli(stopped.Int64).UTC()
			node.StoppedAt = &at
		}
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描 Worker 列表: %w", err)
	}
	return out, nil
}

// CountActiveLeases 返回全库仍有效的租约总数（未释放且未过期，数据库时钟）。
// 它统计的是**全部负责人**——包括已不在 worker_nodes 里的 `legacy` 回填租约；
// 概览要的是"还有多少执行权在生效"，按节点求和会把没有登记行的持有者漏掉。
// CountActiveLeases counts every live lease, including owners without a node row.
func (s *WorkerStore) CountActiveLeases(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `
SELECT count(*) FROM execution_leases
 WHERE released_at IS NULL
   AND expires_at > `+sqlNowMillis).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("统计活跃租约: %w", err)
	}
	return count, nil
}

// Stop 标记实例下线。幂等：重复 Stop 保留首次 stopped_at，不把停机时间往后推。
// 未登记的实例返回错误——静默成功会让"登记失败但停机继续"的装配缺陷无法被发现。
// Stop is idempotent and keeps the first timestamp; an unknown instance is an error.
func (s *WorkerStore) Stop(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return errors.New("postgres: Worker 实例标识为空")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE worker_nodes
   SET stopped_at = COALESCE(stopped_at, `+sqlNowMillis+`)
 WHERE instance_id = $1`, instanceID)
	if err != nil {
		return fmt.Errorf("停止 Worker %s: %w", instanceID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("postgres: Worker %s 停止结果不可读: %w", instanceID, err)
	}
	if affected == 0 {
		return fmt.Errorf("postgres: Worker %s 未登记", instanceID)
	}
	return nil
}
