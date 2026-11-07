package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是回收器的**领取面**：把"已经失去合法执行者"的 A 类租约领取过来，
// 交给 `internal/app` 的回收器按既有规则收尾（模块卡 §2.3.5 图 5）。
//
// 三条纪律：
//
//  1. **只领取已过期或已释放的租约**，绝不按心跳 / 启动时间推断死亡。心跳只作诊断
//     （FR-24-01），存活判定只有 `execution_leases.expires_at` 一个来源。
//  2. **领取必须原子**：`SELECT … FOR UPDATE OF l SKIP LOCKED` + 逐行条件 UPDATE
//     （owner + epoch CAS），并发回收器不会领到同一行；代次在领取时 +1，
//     旧持有者随后的任何受保护写入都会以 `ErrLeaseLost` 被拒（FR-24-15）。
//  3. **业务行必须仍未终态**：否则租约释放后还会被反复领取，收尾动作变成空转。
//     终态行的**残留**（引用 / 投递 / 审计）归 B 类幂等收尾域，不领租约。
//
// This file is the claim surface of the reclaimer: it only ever claims leases that are
// already expired or released, performs the claim atomically with SKIP LOCKED plus an
// owner/epoch CAS, and requires the business row to still be non-terminal.

// workerLostReason 是接管收尾写进 `runs.reason` 与执行行 `error_message` 的固定文案
// （模块卡 §2.3.5 A1 / A2）。它与 `owner_unauthorized` 同属机器可读的稳定字面量，
// 运维不必从中文文案里猜"这条是谁收的尾"。
// workerLostReason is the stable literal stamped by takeover recovery.
const workerLostReason = "worker_lost"

// ReclaimerStore 是回收器的领取与读取面。它与 `LeaseStore` 共用同一张
// `execution_leases`，但职责分开：`LeaseStore` 服务正常执行路径，
// 本类型只服务"执行者已经不在了"的收尾路径。
// ReclaimerStore owns the reclamation claim path; LeaseStore keeps serving normal runs.
type ReclaimerStore struct{ db *DB }

// NewReclaimerStore 绑定库句柄。
// NewReclaimerStore binds the database handle.
func NewReclaimerStore(db *DB) *ReclaimerStore { return &ReclaimerStore{db: db} }

// claimCandidates 是三类资源各自的领取语句（$1 = limit，$2 = 数据库当前毫秒）。
//
// 三域谓词逐字对应模块卡 §2.3.5：
//   - run：`runs.status='running'`；
//   - schedule_execution：`task_executions.status='started'`。资源标识是 text，
//     执行行主键是 identity bigint，比较时把主键转 text——反方向的
//     `resource_id::bigint` 一旦遇到非数字标识会让整条扫描直接报错；
//   - flow_root：根行、非终态、**未进入关闭态**。关闭态（cancelling /
//     close_intent<>'none' / 已过 deadline）由 `sweepClosures` 推进，
//     A3 收进来只会每轮"领取 → 被 Service.Recover 拒绝 → 放弃"。
//
// A1 / A2 / A3 的顺序由调用方保证（先 run、再 execution、最后 flow_root）：
// 先收尾 Run，执行行才能读到真实终态；最后处理根时子树输入已经稳定。
var claimCandidates = map[core.LeaseKind]string{
	core.LeaseRun: `
SELECT l.resource_id, l.owner_worker_id, l.ownership_epoch, l.acquired_at, l.renewed_at,
       l.expires_at, l.released_at
  FROM execution_leases l
  JOIN runs r ON r.run_id = l.resource_id
 WHERE l.kind = 'run'
   AND r.status = 'running'
   AND (l.released_at IS NOT NULL OR l.expires_at <= $2)
 ORDER BY l.expires_at, l.resource_id
 LIMIT $1
   FOR UPDATE OF l SKIP LOCKED`,

	core.LeaseScheduleExecution: `
SELECT l.resource_id, l.owner_worker_id, l.ownership_epoch, l.acquired_at, l.renewed_at,
       l.expires_at, l.released_at
  FROM execution_leases l
  JOIN task_executions e ON e.id::text = l.resource_id
 WHERE l.kind = 'schedule_execution'
   AND e.status = 'started'
   AND (l.released_at IS NOT NULL OR l.expires_at <= $2)
 ORDER BY l.expires_at, l.resource_id
 LIMIT $1
   FOR UPDATE OF l SKIP LOCKED`,

	core.LeaseFlowRoot: `
SELECT l.resource_id, l.owner_worker_id, l.ownership_epoch, l.acquired_at, l.renewed_at,
       l.expires_at, l.released_at
  FROM execution_leases l
  JOIN flow_tasks t ON t.task_id = l.resource_id
 WHERE l.kind = 'flow_root'
   AND t.status IN ('queued','running','blocked')
   AND t.parent_task_id IS NULL
   AND t.root_task_id = t.task_id
   AND t.depth = 0
   AND t.cancel_requested = false
   AND t.close_intent = 'none'
   AND t.deadline > $2
   AND (l.released_at IS NOT NULL OR l.expires_at <= $2)
 ORDER BY l.expires_at, l.resource_id
 LIMIT $1
   FOR UPDATE OF l SKIP LOCKED`,
}

// ClaimExpired 领取至多 limit 条 `kind` 域的过期 / 已释放租约，返回领取后的视图
// （`TookOver` 表示"旧持有者已过期且未交还"——即真正的接管）。
//
// reclaimTTL 是接管方拿到的有效期；必须为正，否则领取后立即过期，下一轮又被别人领走，
// 收尾永远做不完。
// ClaimExpired claims at most limit expired-or-released leases of one kind and returns
// the claimed views; TookOver marks a real takeover.
func (s *ReclaimerStore) ClaimExpired(ctx context.Context, kind core.LeaseKind,
	owner core.WorkerIdentity, limit int, reclaimTTL time.Duration,
) ([]core.Lease, error) {
	if _, err := core.ParseLeaseKind(string(kind)); err != nil {
		// 未知 kind 一律拒绝（FR-24-30）：库里出现读不懂的类型必须先升级程序。
		return nil, err
	}
	query, ok := claimCandidates[kind]
	if !ok {
		return nil, fmt.Errorf("postgres: 回收器没有 %q 域的领取语句", kind)
	}
	if owner.InstanceID == "" {
		return nil, fmt.Errorf("postgres: 回收领取缺少 Worker 实例标识")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("postgres: 回收领取的条数上限必须为正（收到 %d）", limit)
	}
	ttlMS, err := validateLeaseTTL(reclaimTTL)
	if err != nil {
		return nil, err
	}
	if s == nil || s.db == nil || s.db.pool == nil {
		return nil, fmt.Errorf("postgres: 回收领取缺少数据库句柄")
	}

	var claimed []core.Lease
	_, err = s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		claimed = nil
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, query, limit, nowMS)
		if err != nil {
			return fmt.Errorf("扫描可回收租约（%s）: %w", kind, err)
		}
		candidates, err := scanClaimCandidates(rows)
		if err != nil {
			return err
		}
		for _, cand := range candidates {
			lease, err := s.claimOne(ctx, tx, kind, owner.InstanceID, cand, ttlMS, nowMS)
			if err != nil {
				return err
			}
			claimed = append(claimed, lease)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// claimRow 是加锁读到的一条候选。
type claimRow struct {
	resourceID string
	owner      string
	epoch      int64
	acquiredAt int64
	renewedAt  int64
	expiresAt  int64
	releasedAt sql.NullInt64
}

// scanClaimCandidates 读完候选游标。**先读完再逐行写**：同一事务里对同一连接先开
// 一个游标再做 UPDATE 是允许的，但把"读"与"写"交错会让锁等待与游标状态互相纠缠，
// 分两步更好读、也更好判失败点。
func scanClaimCandidates(rows *sql.Rows) ([]claimRow, error) {
	defer func() { _ = rows.Close() }()
	var out []claimRow
	for rows.Next() {
		var cand claimRow
		if err := rows.Scan(&cand.resourceID, &cand.owner, &cand.epoch, &cand.acquiredAt,
			&cand.renewedAt, &cand.expiresAt, &cand.releasedAt); err != nil {
			return nil, fmt.Errorf("读取可回收租约: %w", err)
		}
		out = append(out, cand)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描可回收租约: %w", err)
	}
	return out, nil
}

// claimOne 把一条候选改写成"我持有、代次 +1、未释放、到期时间推后"。
// owner + epoch 是 CAS 条件：行锁已保证并发回收器不会同时进来，CAS 只是把"读到的
// 确实是这一行"落成可判定的断言——0 行即说明状态被别的路径改写，整笔回滚，
// 不带着错误认知去收尾。
func (s *ReclaimerStore) claimOne(ctx context.Context, tx *sql.Tx, kind core.LeaseKind,
	ownerID string, cand claimRow, ttlMS, nowMS int64,
) (core.Lease, error) {
	res, err := tx.ExecContext(ctx, `
UPDATE execution_leases
   SET owner_worker_id = $3,
       ownership_epoch = ownership_epoch + 1,
       acquired_at = $4,
       renewed_at = $4,
       expires_at = $4::bigint + $5::bigint,
       released_at = NULL
 WHERE kind = $1 AND resource_id = $2
   AND owner_worker_id = $6 AND ownership_epoch = $7`,
		string(kind), cand.resourceID, ownerID, nowMS, ttlMS, cand.owner, cand.epoch)
	if err != nil {
		return core.Lease{}, fmt.Errorf("领取过期租约（%s/%s）: %w", kind, cand.resourceID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return core.Lease{}, fmt.Errorf("确认租约领取（%s/%s）: %w", kind, cand.resourceID, err)
	}
	if affected != 1 {
		return core.Lease{}, fmt.Errorf("postgres: 租约领取未命中（%s/%s，期望代次 %d）",
			kind, cand.resourceID, cand.epoch)
	}
	epoch := cand.epoch + 1
	detail := "回收器接管过期租约"
	if cand.releasedAt.Valid {
		detail = "回收器接管已释放但业务未终态的租约"
	}
	if err := appendLeaseEvent(ctx, tx, kind, cand.resourceID, "reclaimed", ownerID, epoch,
		cand.owner, sql.NullInt64{Int64: cand.epoch, Valid: true}, detail, nowMS); err != nil {
		return core.Lease{}, err
	}
	// TookOver 只在"过期未释放"时为真：已释放的旧持有者是正常交棒后残留，
	// 不是"旧属主可能还活着"，两条路径的后续处置因此不同（A3 走 Release vs Recover）。
	return leaseView(kind, cand.resourceID, ownerID, epoch, nowMS, nowMS, nowMS+ttlMS,
		!cand.releasedAt.Valid, false), nil
}

// TerminalResidueReport 是 B1（终态残留对账）一轮的读数。
// 正常路径（无崩溃、无迁移遗留）下四个计数都应为 0；>0 说明上一次收尾在
// "终态已写、引用 / 投递未结算"之间断过——这是**预期可发生**的，不是缺陷。
// TerminalResidueReport counts what one B1 round actually repaired.
type TerminalResidueReport struct {
	// Runs 是入选并至少修复了一行的 Run 数。
	Runs int

	// Unknowns 是归一化为 `unknown` 的审计行数（两张表合计）。
	Unknowns int

	// RefsReleased 是三张引用表合计释放的行数。
	RefsReleased int

	// DeliveriesSettled 是结算掉的 pending 投递行数。
	DeliveriesSettled int
}

// terminalResidueCandidates 选出**已失去合法执行者**的终态 Run 及其残留。
//
// 谓词逐条对应模块卡 §2.3.5 B1（$1 = limit，$2 = 数据库当前毫秒）：
//   - 四个终态全覆盖。漏掉 `limit_reached` 会让这类 Run 的残留永久存在——它是最容易
//     被"只有失败才需要收拾"的直觉漏掉的一支。
//   - 用 `NOT EXISTS(仍有效的租约)` 而**不是** `EXISTS(已释放 / 已过期的租约)`：
//     迁移前遗留的终态行根本没有租约行（0012 只回填非终态行），按后者判会永久漏扫。
//   - 残留判定必须覆盖三张引用表，任何一张漏掉都会让对应种类的引用永远挂着。
//   - 已经干净的行不入选：否则每轮都会把同一条终态 Run 数一遍，"命中数为 0"这个
//     正常路径判据就失去意义。
//
// Picks terminal runs whose pending residue nobody can still be working on.
const terminalResidueCandidates = `
SELECT r.run_id, r.status
  FROM runs r
 WHERE r.status IN ('completed','limit_reached','failed','cancelled')
   AND NOT EXISTS (
         SELECT 1
           FROM execution_leases l
          WHERE l.kind = 'run' AND l.resource_id = r.run_id
            AND l.released_at IS NULL AND l.expires_at > $2)
   AND (EXISTS (SELECT 1 FROM llm_calls a
                 WHERE a.run_id = r.run_id AND a.status = 'pending')
     OR EXISTS (SELECT 1 FROM tool_invocations a
                 WHERE a.run_id = r.run_id AND a.status = 'pending')
     OR EXISTS (SELECT 1 FROM run_resource_refs x
                 WHERE x.run_id = r.run_id AND x.released_at IS NULL)
     OR EXISTS (SELECT 1 FROM run_credential_refs x
                 WHERE x.run_id = r.run_id AND x.released_at IS NULL)
     OR EXISTS (SELECT 1 FROM run_notify_refs x
                 WHERE x.run_id = r.run_id AND x.released_at IS NULL)
     OR EXISTS (SELECT 1 FROM schedule_deliveries d
                 WHERE d.run_id = r.run_id AND d.status = 'pending'))
 ORDER BY r.finished_at, r.run_id
 LIMIT $1`

// ReclaimTerminalResidue 执行一轮 B1：对至多 limit 个"已失去合法执行者的终态 Run"
// 逐项做幂等修复（模块卡 §2.3.5 B1 / FR-24-39）。
//
// **不领租约**：B 类是"没有执行者"的收尾域，条件更新本身就是互斥——两个回收器同时
// 跑同一行也只会得到同一个结果，不会产生第二次副作用。但这**不等于**可以跳过所有权
// 判定：入选谓词要求"该 Run 不存在仍有效的租约"，否则会把一个正在正常收尾的 Run
// （`FinishRun` 已提交、学习钩子还在跑）的引用与投递提前收掉。扫描谓词只是快照判断，
// 逐行处理前**必须**再在租约行上加锁重判（`lockLeaseRecheck`）——一条未提交的
// 合法续约在快照里不可见，不重判就会把它覆盖的引用提前收掉。
//
// 顺序与 A1 一致：审计归一化 → 三张引用表 → 投递结算。审计先归一是因为投递结论与
// 引用判定都要基于"已经不存在 pending 审计"这个事实。
// ReclaimTerminalResidue repairs terminal-run residue without claiming any lease.
func (s *ReclaimerStore) ReclaimTerminalResidue(ctx context.Context, limit int,
	now time.Time,
) (TerminalResidueReport, error) {
	var report TerminalResidueReport
	if s == nil || s.db == nil || s.db.pool == nil {
		return report, fmt.Errorf("postgres: 终态残留对账缺少数据库句柄")
	}
	if limit <= 0 {
		return report, fmt.Errorf("postgres: 终态残留对账的条数上限必须为正（收到 %d）", limit)
	}
	_, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		report = TerminalResidueReport{}
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, terminalResidueCandidates, limit, nowMS)
		if err != nil {
			return fmt.Errorf("扫描终态残留: %w", err)
		}
		candidates, err := scanTerminalResidue(rows)
		if err != nil {
			return err
		}
		for _, cand := range candidates {
			part, err := reclaimOneTerminalRun(ctx, tx, cand, now)
			if err != nil {
				return err
			}
			report.Runs += part.Runs
			report.Unknowns += part.Unknowns
			report.RefsReleased += part.RefsReleased
			report.DeliveriesSettled += part.DeliveriesSettled
		}
		return nil
	})
	if err != nil {
		return TerminalResidueReport{}, err
	}
	return report, nil
}

// terminalResidueRow 是一条入选的终态 Run。
type terminalResidueRow struct {
	runID  string
	status string
}

// scanTerminalResidue 读完候选游标（先读完再逐行写，理由见 scanClaimCandidates）。
func scanTerminalResidue(rows *sql.Rows) ([]terminalResidueRow, error) {
	defer func() { _ = rows.Close() }()
	var out []terminalResidueRow
	for rows.Next() {
		var row terminalResidueRow
		if err := rows.Scan(&row.runID, &row.status); err != nil {
			return nil, fmt.Errorf("读取终态残留: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描终态残留: %w", err)
	}
	return out, nil
}

// lockLeaseRecheck 在**加锁后**重判一条租约是否仍由活着的执行者持有。
//
// 为什么扫描之后还要重判：回收器的候选扫描（B1 / B2）都是事务快照上的
// `NOT EXISTS(有效租约)` 判断，一条**尚未提交**的合法续约在快照里不可见——扫描会
// 选中该资源，随后直接收尾就会抢在续约提交前把活租约的引用 / 投递收掉。这里先对该
// 租约行 `FOR UPDATE`：
//   - 续约先持锁：本语句等它提交，再按**获锁后**的数据库时钟重读 `expires_at`，
//     看到续约后的值 → 判为活租约，调用方跳过；
//   - 本语句先持锁：续约语句随后等本事务结束，重新求值时看到的是已过期的
//     `expires_at`（它的时钟不早于本次重读），续约失败——与"过期即失权"一致。
//
// 租约行不存在（0012 之前的遗留行）表示没有执行者，按可回收处理。
// lockLeaseRecheck decides, under the lease row lock and a fresh database clock,
// whether a live owner still holds the lease; a scan's NOT EXISTS cannot see an
// uncommitted renewal, so the decision must be re-made after the lock is acquired.
func lockLeaseRecheck(ctx context.Context, tx *sql.Tx, kind core.LeaseKind, resourceID string) (bool, error) {
	var (
		expiresAt  int64
		releasedAt sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `
SELECT expires_at, released_at
  FROM execution_leases
 WHERE kind = $1 AND resource_id = $2
   FOR UPDATE`, string(kind), resourceID).Scan(&expiresAt, &releasedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("锁定租约行（%s/%s）: %w", kind, resourceID, err)
	}
	nowMS, err := readDBMillis(ctx, tx)
	if err != nil {
		return false, err
	}
	return !releasedAt.Valid && expiresAt > nowMS, nil
}

// reclaimOneTerminalRun 修复一个终态 Run 的残留，并返回这一行真落盘的计数。
//
// 先锁租约行再重判、最后才碰业务行：锁序与正常路径的"租约行 → 业务行"一致，
// 不会与续约 / 接管形成环。三步业务修复都是幂等条件更新：重复执行不会产生第二次
// 副作用。全部计数为 0 表示"扫描之后别人已经收拾干净了"——不是错误，也不计入
// Runs（否则"命中数为 0"的正常路径判据就测不出来了）。
func reclaimOneTerminalRun(ctx context.Context, tx *sql.Tx,
	cand terminalResidueRow, now time.Time,
) (TerminalResidueReport, error) {
	var part TerminalResidueReport
	live, err := lockLeaseRecheck(ctx, tx, core.LeaseRun, cand.runID)
	if err != nil {
		return part, err
	}
	if live {
		// 锁内看到活租约：扫描快照没看见的（未提交）续约已经落库，本轮不动业务行。
		return part, nil
	}
	normalized, err := normalizePendingAuditRows(ctx, tx, []string{cand.runID}, now)
	if err != nil {
		return part, err
	}
	unknowns := len(normalized)
	refs := 0
	for _, release := range []func(context.Context, sqlExecutor, string, time.Time) (int, error){
		releaseRunRefs, releaseRunCredentialRefs, releaseRunNotifyRefs,
	} {
		n, err := release(ctx, tx, cand.runID, now)
		if err != nil {
			return part, err
		}
		refs += n
	}
	status, detail := terminalDeliveryOutcome(cand.status)
	deliveries, err := settleDeliveriesInTx(ctx, tx, cand.runID, status, detail, now)
	if err != nil {
		return part, err
	}
	if unknowns == 0 && refs == 0 && deliveries == 0 {
		return part, nil
	}
	return TerminalResidueReport{
		Runs:              1,
		Unknowns:          unknowns,
		RefsReleased:      refs,
		DeliveriesSettled: deliveries,
	}, nil
}

// ExecutionLeaseToken 把回收器领到的 `core.Lease` 投影成 `TaskStore.FinishExecution`
// 要求的 token —— 两者本来就是同一条租约的两种视图，这里只做一次显式转换，避免
// 调用方各自拼字段。
// ExecutionLeaseToken projects a claimed lease into the credential FinishExecution wants.
func ExecutionLeaseToken(lease core.Lease) (core.LeaseToken, int64, error) {
	if lease.Kind != core.LeaseScheduleExecution {
		return core.LeaseToken{}, 0, fmt.Errorf("postgres: 执行权类型 %q 不是 schedule_execution", lease.Kind)
	}
	id, err := parseExecutionResourceID(lease.ResourceID)
	if err != nil {
		return core.LeaseToken{}, 0, err
	}
	return lease.LeaseToken, id, nil
}

// parseExecutionResourceID 把 `schedule_execution` 的 text 资源标识解成执行行主键。
// 租约表的 resource_id 是 text（三域共用一张表），执行行的主键是 identity bigint——
// 两处形态不同，转换只在这里做一次。
func parseExecutionResourceID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("postgres: 执行行标识 %q 不是整数: %w", raw, err)
	}
	return id, nil
}
