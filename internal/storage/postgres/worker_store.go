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
type WorkerStore struct {
	db *DB
	// defaultHeartbeatSeconds 是迁移前旧行（心跳间隔列为 NULL）的兜底阈值来源：
	// 概览按行计算 `max(3 × 间隔, 30s)`，缺值行必须显式标注「取部署默认」，而不是
	// 拿管理台进程自己的间隔套给所有实例。
	// defaultHeartbeatSeconds backs legacy rows that never reported their cadence.
	defaultHeartbeatSeconds int
}

// 编译期断言：实现满足 core 声明的登记端口与概览读取端口。
var (
	_ core.WorkerRegistry       = (*WorkerStore)(nil)
	_ core.WorkerOverviewReader = (*WorkerStore)(nil)
)

// defaultWorkerHeartbeatSeconds 是构造期默认值的缺省（与管理配置缺省同值 10s）。
// It matches the configuration default; assembly overrides it with the live value.
const defaultWorkerHeartbeatSeconds = 10

// overviewNodeLimit 是概览明细的兜底上限（调用方缺省传 200；非正数按它处理）。
// The fallback detail bound when the caller passes a non-positive limit.
const overviewNodeLimit = 200

// WorkerStoreOption 是构造期的可选注入点（当前只有旧行兜底间隔）。
// WorkerStoreOption configures a WorkerStore at construction time.
type WorkerStoreOption func(*WorkerStore)

// WithDefaultHeartbeatInterval 注入部署默认心跳间隔（秒）：只用于迁移前旧行的
// 诊断阈值兜底，不参与任何归属判定。非正数被忽略、保持缺省值——错误配置不得把
// 诊断阈值算成 0。
// WithDefaultHeartbeatInterval injects the deployment default used only for legacy
// rows; non-positive values keep the built-in default.
func WithDefaultHeartbeatInterval(seconds int) WorkerStoreOption {
	return func(s *WorkerStore) {
		if seconds > 0 {
			s.defaultHeartbeatSeconds = seconds
		}
	}
}

// NewWorkerStore 绑定一个已打开的写模式库句柄。
func NewWorkerStore(db *DB, opts ...WorkerStoreOption) *WorkerStore {
	store := &WorkerStore{db: db, defaultHeartbeatSeconds: defaultWorkerHeartbeatSeconds}
	for _, opt := range opts {
		opt(store)
	}
	return store
}

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
//
// 心跳间隔按 `WorkerIdentity.HeartbeatIntervalSeconds` 落库：`> 0` 写正整数，`0` 写
// NULL（迁移前旧行语义），负数在应用层拒绝——数据库 CHECK 只是最后一道兜底，不能
// 让装配缺陷等到写库才暴露。
// Register upserts the diagnostic row for one instance; schema_version is written once.
// The heartbeat interval lands as a positive integer, 0 as NULL, and negatives are
// rejected at the entry before the database CHECK is ever reached.
func (s *WorkerStore) Register(ctx context.Context, worker core.WorkerIdentity) error {
	if worker.InstanceID == "" {
		return errors.New("postgres: Worker 实例标识为空")
	}
	if worker.SchemaVersion <= 0 {
		return fmt.Errorf("postgres: Worker %s 的 schema 版本非法（%d）", worker.InstanceID, worker.SchemaVersion)
	}
	if worker.HeartbeatIntervalSeconds < 0 {
		return fmt.Errorf("postgres: Worker %s 的心跳间隔非法（%d）", worker.InstanceID, worker.HeartbeatIntervalSeconds)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO worker_nodes(instance_id, worker_name, hostname, build_version, schema_version,
                         started_at, last_heartbeat_at, stopped_at, heartbeat_interval_seconds)
VALUES ($1, $2, $3, $4, $5, `+sqlNowMillis+`, `+sqlNowMillis+`, NULL, NULLIF($6::integer, 0))
ON CONFLICT (instance_id) DO UPDATE
   SET worker_name                = EXCLUDED.worker_name,
       hostname                   = EXCLUDED.hostname,
       build_version              = EXCLUDED.build_version,
       schema_version             = EXCLUDED.schema_version,
       started_at                 = EXCLUDED.started_at,
       last_heartbeat_at          = EXCLUDED.last_heartbeat_at,
       stopped_at                 = NULL,
       heartbeat_interval_seconds = EXCLUDED.heartbeat_interval_seconds`,
		worker.InstanceID, worker.Name, worker.Hostname, worker.Version, worker.SchemaVersion,
		worker.HeartbeatIntervalSeconds)
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

// DefaultHeartbeatIntervalSeconds 返回构造期注入的部署默认心跳间隔（秒）。概览投影
// 只在旧行（间隔列为 NULL）缺值时用它给出实际生效的诊断阈值，并在详情标注来源；
// 它不参与任何归属判定。
// DefaultHeartbeatIntervalSeconds exposes the deployment default for legacy rows; it
// never participates in ownership decisions.
func (s *WorkerStore) DefaultHeartbeatIntervalSeconds() int { return s.defaultHeartbeatSeconds }

// ReadWorkerOverview 用**一条 SQL 语句**返回概览面板的全部读数（模块卡 §2.3.9）：
// 三类全库计数、全库活跃租约数、有界明细与每行诊断读数。
//
// 快照语义：`bounds` 先冻结数据库 `now()`（事务开始时刻的常量），后续 CTE 一律引用
// `bounds.as_of`，**不出现** `clock_timestamp()`——后者在同一语句内会推进，混进来会让
// `as_of`、逐行判定与租约聚合落在不同瞬间。`counted` 与 `page` 都从同一个 `scored`
// 派生，外层把计数与明细**同时投影**，因此不存在"先查计数、再查明细"的两次读窗口；
// `total` 是三类计数之和（全库聚合），`truncated = total > len(nodes)`。
// 全库活跃租约数（`active_leases`）在**同一条语句**里直接对 `execution_leases` 聚合，
// 不按节点求和——没有 `worker_nodes` 行的 legacy 持有者同样计入（§2.3.9 L918）。
//
// 逐行阈值 `max(3 × 该行心跳间隔, 30s)`：间隔列在旧行上为 NULL（迁移 0013 之前登记），
// 此时回退构造期注入的部署默认值——**不得**拿管理台进程自己的间隔套给所有实例。
// 状态只作显示：`active` 只表示近期收到心跳，`stale` 只表示待确认（心跳超时不证明进程
// 已死），`stopped` 只由 `stopped_at` 判定。
//
// ReadWorkerOverview returns counts and bounded details from one statement's snapshot;
// per-row thresholds come from the row's own interval, falling back to the injected
// deployment default for pre-migration rows.
func (s *WorkerStore) ReadWorkerOverview(ctx context.Context, limit int) (core.WorkerOverview, error) {
	if limit <= 0 {
		limit = overviewNodeLimit
	}
	rows, err := s.db.QueryContext(ctx, `
WITH bounds AS MATERIALIZED (
    SELECT now() AS as_of
), scored AS MATERIALIZED (
    SELECT w.instance_id, w.worker_name, w.hostname, w.build_version, w.schema_version,
           w.started_at, w.last_heartbeat_at, w.stopped_at, w.heartbeat_interval_seconds,
           GREATEST(3 * COALESCE(w.heartbeat_interval_seconds, $1::integer), 30) AS effective_seconds,
           CASE
               WHEN w.stopped_at IS NOT NULL THEN 'stopped'
               WHEN b.as_of - make_interval(secs => GREATEST(3 * COALESCE(w.heartbeat_interval_seconds, $1::integer), 30))
                    > to_timestamp(w.last_heartbeat_at / 1000.0) THEN 'stale'
               ELSE 'active'
           END AS status,
           GREATEST((extract(epoch FROM b.as_of) * 1000)::bigint - w.last_heartbeat_at, 0) / 1000
               AS heartbeat_age
      FROM worker_nodes w CROSS JOIN bounds b
), counted AS MATERIALIZED (
    SELECT count(*) FILTER (WHERE status = 'active')  AS active,
           count(*) FILTER (WHERE status = 'stale')   AS stale,
           count(*) FILTER (WHERE status = 'stopped') AS stopped
      FROM scored
), page AS MATERIALIZED (
    SELECT instance_id, worker_name, hostname, build_version, schema_version,
           started_at, last_heartbeat_at, stopped_at, heartbeat_interval_seconds,
           status, heartbeat_age
      FROM scored
     ORDER BY CASE status WHEN 'active' THEN 0 WHEN 'stale' THEN 1 ELSE 2 END,
              last_heartbeat_at DESC, instance_id
     LIMIT $2::integer
)
SELECT c.active, c.stale, c.stopped, b.as_of, g.active_leases,
       p.instance_id, p.worker_name, p.hostname, p.build_version, p.schema_version,
       p.started_at, p.last_heartbeat_at, p.stopped_at, p.heartbeat_interval_seconds,
       p.status, p.heartbeat_age,
       COALESCE(l.leases, 0) AS node_active_leases
  FROM counted c CROSS JOIN bounds b
  CROSS JOIN (SELECT count(*) AS active_leases
                FROM execution_leases
               WHERE released_at IS NULL
                 AND expires_at > (SELECT (extract(epoch FROM as_of) * 1000)::bigint FROM bounds)) g
  LEFT JOIN page p ON TRUE
  LEFT JOIN (SELECT owner_worker_id, count(*) AS leases
               FROM execution_leases
              WHERE released_at IS NULL
                AND expires_at > (SELECT (extract(epoch FROM as_of) * 1000)::bigint FROM bounds)
              GROUP BY owner_worker_id) l
         ON l.owner_worker_id = p.instance_id
 ORDER BY (p.status IS NULL),
          CASE p.status WHEN 'active' THEN 0 WHEN 'stale' THEN 1 ELSE 2 END,
          p.last_heartbeat_at DESC, p.instance_id`,
		s.defaultHeartbeatSeconds, limit)
	if err != nil {
		return core.WorkerOverview{}, fmt.Errorf("读取 Worker 概览: %w", err)
	}
	defer func() { _ = rows.Close() }()

	overview := core.WorkerOverview{Nodes: make([]core.WorkerNodeStatus, 0)}
	for rows.Next() {
		var (
			active, stale, stopped int
			asOf                   time.Time
			globalLeases           int64
			instanceID             sql.NullString
			name, hostname         sql.NullString
			buildVersion           sql.NullString
			schemaVersion          sql.NullInt64
			started, heartbeat     sql.NullInt64
			stoppedAt              sql.NullInt64
			interval               sql.NullInt64
			status                 sql.NullString
			heartbeatAge           sql.NullInt64
			nodeLeases             sql.NullInt64
		)
		if err := rows.Scan(&active, &stale, &stopped, &asOf, &globalLeases,
			&instanceID, &name, &hostname, &buildVersion, &schemaVersion,
			&started, &heartbeat, &stoppedAt, &interval, &status, &heartbeatAge,
			&nodeLeases); err != nil {
			return core.WorkerOverview{}, fmt.Errorf("读取 Worker 概览行: %w", err)
		}
		overview.AsOf = asOf.UTC()
		overview.Counts = core.WorkerOverviewCounts{Active: active, Stale: stale, Stopped: stopped}
		overview.ActiveLeases = globalLeases
		// 明细页为空时 LEFT JOIN 仍返回一行（p.* 全 NULL）：此时只取计数与 as_of。
		if !instanceID.Valid {
			continue
		}
		node := core.WorkerNodeStatus{
			WorkerNodeView: core.WorkerNodeView{
				InstanceID:      instanceID.String,
				Name:            name.String,
				Hostname:        hostname.String,
				BuildVersion:    buildVersion.String,
				SchemaVersion:   int(schemaVersion.Int64),
				StartedAt:       time.UnixMilli(started.Int64).UTC(),
				LastHeartbeatAt: time.UnixMilli(heartbeat.Int64).UTC(),
				ActiveLeases:    nodeLeases.Int64,
			},
			HeartbeatAgeSeconds: heartbeatAge.Int64,
			Status:              status.String,
		}
		if stoppedAt.Valid {
			at := time.UnixMilli(stoppedAt.Int64).UTC()
			node.StoppedAt = &at
		}
		if interval.Valid {
			seconds := int(interval.Int64)
			node.HeartbeatIntervalSeconds = &seconds
		}
		overview.Nodes = append(overview.Nodes, node)
	}
	if err := rows.Err(); err != nil {
		return core.WorkerOverview{}, fmt.Errorf("扫描 Worker 概览: %w", err)
	}
	overview.Total = overview.Counts.Active + overview.Counts.Stale + overview.Counts.Stopped
	overview.Truncated = overview.Total > len(overview.Nodes)
	return overview, nil
}

// CountActiveLeases 返回全库仍有效的租约总数（未释放且未过期，数据库时钟）。
// 它统计的是**全部负责人**——包括已不在 worker_nodes 里的 `legacy` 回填租约；
// 概览要的是"还有多少执行权在生效"，按节点求和会把没有登记行的持有者漏掉。
// 概览面板**不再**调用本方法：`ReadWorkerOverview` 在自己的单条语句里用同一谓词读取
// `ActiveLeases`，保证与计数、明细同快照（§2.3.9 L862 / L918）；本方法保留给直接
// 调用的消费方。
// CountActiveLeases counts every live lease, including owners without a node row; the
// overview reads the same predicate inside its own single statement and no longer calls
// this method.
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
