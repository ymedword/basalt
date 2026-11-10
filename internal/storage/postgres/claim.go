package postgres

// 本文件是**钉住版本的领取**（US-25C §2.3.5 ⑤ / §2.3.5.1 T1）：在既有领取事务里
// 多一步「锁发布指针行 `FOR SHARE` 并核对期望哈希」，成功时把固定哈希写进 started
// 执行行（`pinned_bundle_hash`）。指针缺失 / 已撤销 / 哈希不符分别是
// `core.ErrAssetNotReady` / `core.ErrAssetMoved`，**零写入**——不推进 `next_fire_at`、
// 不写执行行、不写租约：本次触发原样留在到期集合里，下一圈从就绪门重新求值。
//
// 为什么必须绑进同一事务（C20）：单独先读一次指针，在「读 → 发布 → 领取」之间仍留
// 窗口，而领取一旦推进 `next_fire_at` 就把这次触发白白消耗掉。发布路径在
// `resource_pointers` 上取 `FOR UPDATE`（resource_store.go:413-418，持到提交），
// 本文件的 `FOR SHARE` 与它**真实互斥**：要么领取先提交、发布等下一拍再推进指针，
// 要么发布先提交、领取读到新哈希并返回 `ErrAssetMoved`。锁序固定为「任务行 → 指针行」，
// 发布事务不碰 `scheduled_tasks`，无反向锁序。
//
// 名称到指针的解析：指针按**Agent UID** 寻址（`resource_pointers.resource_id` 与
// `resource_uid` 对 `agent_bundle` 同值，见 `publishedBundleFromManifest` 的身份断言），
// 而领取只拿得到 `task_id`。事务内的映射源是 `agent_lifecycle` 身份镜像
// （`agent_name → agent_uid`）——与 Run 准入的 `recheckAgentLifecycle`、`AgentIdentity`
// 同一份权威；行缺失 / UID 为空一律按 `ErrAssetNotReady` 失败关闭，不猜、不回落。
// This file owns the pinned claim: one extra pointer-row FOR SHARE and hash check inside
// the existing claim transaction. Both refusals write nothing at all. The agent name is
// mapped to its bundle pointer through the agent_lifecycle identity mirror, the same
// authority admission already uses.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 编译期断言：满足 core 声明的钉住领取与准入回读两个消费方端口。
// Compile-time assertions for the two consumer-side ports.
var (
	_ core.AssetPinnedClaimer = (*TaskStore)(nil)
	_ core.PinnedClaimReader  = (*TaskStore)(nil)
)

// pinnedPointerQuery 取任务所属 Agent 的发布指针：经 `agent_lifecycle` 把
// `agent_name` 映射到 `agent_uid`，再按 `resource_id` 命中指针行。
//
// `FOR SHARE OF p` 只锁指针行本身（不锁生命周期行）：与发布的 `FOR UPDATE` 真实
// 互斥，同时不把只读的身份镜像卷进锁竞争。生命周期行缺失 / `agent_uid` 为空时
// 连接结果为零行（NULL 参与等值比较恒为 NULL），由调用方按失败关闭处理。
// pinnedPointerQuery resolves the bundle pointer through the lifecycle identity mirror
// and locks only the pointer row.
const pinnedPointerQuery = `
SELECT v.content_hash, p.deleted_at
  FROM agent_lifecycle l
  JOIN resource_pointers p
    ON p.resource_kind = $2 AND p.resource_id = l.agent_uid
  JOIN resource_versions v ON v.id = p.published_version_id
 WHERE l.agent_name = $1
 FOR SHARE OF p`

// readClaimedPinQuery 回读一次领取的执行行与领取时的租约副本。`task_executions`
// **没有** agent 列（卡 §2.3.5.1 T2）：Agent 只能经 `task_id` 关联
// `scheduled_tasks.agent_name` 读出。历史行与非调度通道的行 `pinned_bundle_hash`
// 为 NULL，`run_id` 对 skipped 行同样可空，二者都用 Null 类型承接。
// readClaimedPinQuery reads the claim row; the agent comes from scheduled_tasks.
const readClaimedPinQuery = `
SELECT e.id, e.run_id, t.agent_name, e.status, e.pinned_bundle_hash,
       e.owner_worker_id, e.ownership_epoch
  FROM task_executions e
  JOIN scheduled_tasks t ON t.task_id = e.task_id
 WHERE e.id = $1`

// ClaimDueTriggerPinned 是 `core.AssetPinnedClaimer` 的 PostgreSQL 实现：与
// `ClaimDueTrigger` 同一个领取事务，另加「锁指针行 + 核对期望哈希」一步。
//
// `expectHash` 必须是本进程就绪快照里的已安装哈希（不含空白以外的语义归一）；
// 空值不是「跳过核对」，而是调用方的装配错误——没有期望哈希的钉住领取无法证明
// 领取与就绪一致，直接拒绝。
// ClaimDueTriggerPinned adds the pointer lock and hash check to the claim transaction;
// an empty expectation is refused rather than treated as "skip the check".
func (s *TaskStore) ClaimDueTriggerPinned(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time,
	owner core.WorkerIdentity, ttl time.Duration, expectHash string,
) (core.DueExecution, error) {
	if expectHash == "" {
		return core.DueExecution{}, fmt.Errorf("postgres: 钉住领取缺少期望哈希（task_id=%s）", taskID)
	}
	return s.claimDueTrigger(ctx, taskID, plannedAt, nextFireAt, owner, ttl, expectHash)
}

// claimDueTrigger 是两条领取入口共用的唯一事务体（US-24 建立，US-25C 追加钉住分支）：
//
//	任务行 FOR UPDATE（退休即视为不存在）
//	→ 同一 planned_at 已有 cron 执行行 → ErrTriggerAlreadyClaimed
//	→ 同任务已有 started 执行行        → ErrTaskBusy
//	→ [仅钉住分支] 指针行 FOR SHARE + 哈希核对 → ErrAssetNotReady / ErrAssetMoved
//	→ CAS 推进 next_fire_at（0 行）    → ErrStaleNextFire
//	→ INSERT started 执行行（planned_at / owner_worker_id / epoch=1 / pinned_bundle_hash）
//	→ INSERT schedule_execution 租约（epoch=1，立即生效）
//
// `expectHash == ""` 表示非钉住领取（手动补跑与历史调用方），执行行的
// `pinned_bundle_hash` 写 NULL，准入走既有当前指针语义。钉住分支的两种拒绝都发生在
// **任何写入之前**：事务回滚后库里没有执行行、没有推进、没有租约。
// claimDueTrigger is the single transaction body of both claim entries; refusals happen
// before any write.
func (s *TaskStore) claimDueTrigger(ctx context.Context, taskID string, plannedAt, nextFireAt time.Time,
	owner core.WorkerIdentity, ttl time.Duration, expectHash string,
) (core.DueExecution, error) {
	if owner.InstanceID == "" {
		return core.DueExecution{}, fmt.Errorf("postgres: 领取计划触发缺少领取者身份（task_id=%s）", taskID)
	}
	ttlMS, err := validateLeaseTTL(ttl)
	if err != nil {
		return core.DueExecution{}, err
	}
	if plannedAt.IsZero() || nextFireAt.IsZero() {
		return core.DueExecution{}, fmt.Errorf("postgres: 计划触发时刻为空（task_id=%s）", taskID)
	}

	var claimed core.DueExecution
	outcome, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockTaskRow(ctx, tx, taskID)
		if err != nil {
			return err
		}
		// 停用可能提交在到期扫描之后：任务行锁保证这里读到的是最新已提交的 `enabled`。
		// 停用即拒绝领取——不得留下 started 执行行与租约（FR-24-10 的启停语义）。
		// The disable may have committed after the due scan; under the task row lock
		// this recheck sees the latest enabled flag and rejects the claim outright.
		if !task.Enabled {
			return core.ErrTaskDisabled
		}
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		taken, err := executionPlannedExists(ctx, tx, taskID, plannedAt)
		if err != nil {
			return err
		}
		if taken {
			return core.ErrTriggerAlreadyClaimed
		}
		busy, err := executionStartedExists(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if busy {
			return core.ErrTaskBusy
		}
		// 钉住分支：锁指针行并核对期望哈希。必须在 CAS 推进与执行行 INSERT 之前——
		// 拒绝时整个事务回滚，库里什么都没发生。
		if expectHash != "" {
			if err := lockPinnedPointer(ctx, tx, taskID, task.Agent, expectHash); err != nil {
				return err
			}
		}

		res, err := tx.ExecContext(ctx, `
UPDATE scheduled_tasks
   SET next_fire_at = $1, updated_at = $2
 WHERE task_id = $3 AND next_fire_at = $4`,
			nextFireAt.UnixMilli(), nowMS, taskID, plannedAt.UnixMilli())
		if err != nil {
			return fmt.Errorf("推进下一次触发（task_id=%s）: %w", taskID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认下一次触发推进（task_id=%s）: %w", taskID, err)
		}
		if affected == 0 {
			return core.ErrStaleNextFire
		}

		// Run ID 在领取事务内分配：`task_executions.run_id` 必须与随后 Run 行同一个值，
		// 而冻结签名没有 RunID 入参（T014 裁决 ①）。时间段用计划时刻——同一次计划
		// 触发的多条线索在日志里可归并，随机段保证唯一。
		runID := core.NewRunID("scheduler", plannedAt.UTC(), core.RunIDSuffix())
		var pinned any
		if expectHash != "" {
			pinned = expectHash
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `
INSERT INTO task_executions(task_id, run_id, status, trigger, started_at,
                           planned_at, owner_worker_id, ownership_epoch,
                           pinned_bundle_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, 1, $8)
RETURNING id`,
			taskID, runID, string(core.ExecutionStarted), string(core.TriggerCron),
			nowMS, plannedAt.UnixMilli(), owner.InstanceID, pinned).Scan(&id); err != nil {
			return fmt.Errorf("预写计划触发执行行（task_id=%s）: %w", taskID, err)
		}
		token, err := insertExecutionLease(ctx, tx, id, owner.InstanceID, nowMS, ttlMS)
		if err != nil {
			return err
		}
		claimed = core.DueExecution{
			Task:             task,
			ExecutionID:      id,
			PlannedAt:        plannedAt.UTC(),
			Lease:            token,
			RunID:            runID,
			PinnedBundleHash: expectHash,
		}
		return nil
	})
	if err != nil {
		return core.DueExecution{}, err
	}
	if outcome != TxCommitted {
		// 领取是否落库未知：**不得**重试、**不得**当未领取。调用方记 ERROR 后由下一轮
		// 到期扫描按真实行状态收敛。
		return core.DueExecution{}, fmt.Errorf("%w: 计划触发领取结局为 %q（task_id=%s）",
			ErrTxOutcomeUnknown, outcome, taskID)
	}
	return claimed, nil
}

// lockPinnedPointer 锁住该 Agent 的发布指针行并核对期望哈希：
//
//   - 生命周期行缺失 / UID 为空 / 指针行缺失 → `ErrAssetNotReady`
//   - 指针已撤销（`deleted_at` 非空）      → `ErrAssetNotReady`
//   - 指针内容哈希 ≠ 期望（扫描后、领取前被发布抢先）→ `ErrAssetMoved`
//
// 两种结局都由调用方回滚整笔事务；本函数只读，**不**写任何行。
// lockPinnedPointer locks the pointer row and compares hashes; it only reads.
func lockPinnedPointer(ctx context.Context, tx *sql.Tx, taskID, agent, expectHash string) error {
	var (
		contentHash string
		deletedAt   sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, pinnedPointerQuery, agent, string(core.ResourceAgentBundle)).
		Scan(&contentHash, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("核对领取指针（task_id=%s, agent=%s）: %w", taskID, agent, core.ErrAssetNotReady)
	}
	if err != nil {
		return fmt.Errorf("锁定领取指针（task_id=%s, agent=%s）: %w", taskID, agent, err)
	}
	if deletedAt.Valid {
		return fmt.Errorf("核对领取指针（task_id=%s, agent=%s）: 指针已撤销: %w",
			taskID, agent, core.ErrAssetNotReady)
	}
	if contentHash != expectHash {
		return fmt.Errorf("核对领取指针（task_id=%s, agent=%s）: 已发布哈希 %s 与期望 %s 不符: %w",
			taskID, agent, contentHash, expectHash, core.ErrAssetMoved)
	}
	return nil
}

// ReadClaimedPin 是 `core.PinnedClaimReader` 的 PostgreSQL 实现：按执行行主键回读
// 领取记录（`ClaimedPinRecord`）。行不存在返回 `(零值, false, nil)`——调用方据此
// 映射为 `ErrClaimPinInvalid`，而不是把「查不到」当成读失败重试。
//
// 本方法只读、不加锁：它是准入的**廉价前置**，权威判定在 `StartRun` 的
// `recheckClaimPinInTx`（同事务、固定锁序）。
// ReadClaimedPin reads the claim record without locks; it is the cheap precheck only.
func (s *TaskStore) ReadClaimedPin(ctx context.Context, executionID int64) (core.ClaimedPinRecord, bool, error) {
	var (
		rec    core.ClaimedPinRecord
		runID  sql.NullString
		hash   sql.NullString
		owner  sql.NullString
		epoch  sql.NullInt64
		agent  string
		status string
	)
	err := s.db.pool.QueryRowContext(ctx, readClaimedPinQuery, executionID).Scan(
		&rec.ExecutionID, &runID, &agent, &status, &hash, &owner, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ClaimedPinRecord{}, false, nil
	}
	if err != nil {
		return core.ClaimedPinRecord{}, false, fmt.Errorf("读取领取记录（execution_id=%d）: %w", executionID, err)
	}
	rec.RunID = runID.String
	rec.Agent = agent
	rec.Status = status
	rec.BundleHash = hash.String
	rec.OwnerWorkerID = owner.String
	rec.OwnershipEpoch = epoch.Int64
	return rec, true, nil
}
