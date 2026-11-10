package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是到期扫描的**两段键集分页**（US-25C §2.3.5 扫描公平 / 卡 §3.1.3）：
// 开圈时以**一条**只读语句冻结边界（数据库时间 + 本圈 NULL 段上界），此后每拍只取
// 一页；NULL 段按 `task_id` 前进、非 NULL 段按 `(next_fire_at, task_id)` 前进，
// 非 NULL 段首屏**不**拼元组下界（NULL 参与 `>` 恒为 NULL，会把首屏取成零行）。
// 扫描本身仍不加锁：互斥在 `ClaimDueTrigger*` 的任务行锁 + CAS 上。
// This file owns the two-segment keyset paging of the due scan: one read freezes the
// sweep bounds, one page per pass, NULL segment by task_id and the rest by
// (next_fire_at, task_id) with no tuple lower bound on its first page.
const (
	// sweepBoundsQuery 用**单条语句**（同一个快照）同时取本圈数据库时间与 NULL 段
	// 最大 `task_id`；谓词与 NULL 段扫描完全一致，空串表示本圈没有 NULL 行。
	// One statement, one snapshot: the sweep cutoff and the NULL-segment upper bound.
	sweepBoundsQuery = `
SELECT (extract(epoch from clock_timestamp()) * 1000)::bigint,
       COALESCE(MAX(task_id), '')
  FROM scheduled_tasks
 WHERE enabled AND retired_at IS NULL
   AND agent_name = ANY($1::text[])
   AND next_fire_at IS NULL`

	// sweepNullSegmentQuery 取 NULL 段的一页（`$1` 游标，`$2` Agent 集合，
	// `$3` 本圈上界，`$4` limit）。整页 NULL 也**前进**：调用方据末行推进游标。
	// The NULL segment pages by task_id and never resets on a full page.
	sweepNullSegmentQuery = `
SELECT ` + taskRecordColumns + `, next_fire_at
  FROM scheduled_tasks
 WHERE enabled AND retired_at IS NULL
   AND agent_name = ANY($2::text[])
   AND next_fire_at IS NULL
   AND task_id > $1::text
   AND task_id <= $3::text
 ORDER BY task_id ASC
 LIMIT $4::bigint`

	// sweepDueFirstPageQuery 取非 NULL 段**首屏**（`$1` 圈界毫秒，`$2` Agent 集合，
	// `$3` limit）：**不带**元组下界——首屏没有可用的上一行。
	// The first non-NULL page carries no tuple lower bound.
	sweepDueFirstPageQuery = `
SELECT ` + taskRecordColumns + `, next_fire_at
  FROM scheduled_tasks
 WHERE enabled AND retired_at IS NULL
   AND agent_name = ANY($2::text[])
   AND next_fire_at IS NOT NULL
   AND next_fire_at <= $1::bigint
 ORDER BY next_fire_at ASC, task_id ASC
 LIMIT $3::bigint`

	// sweepDueAfterQuery 取非 NULL 段的后续页（`$1` 圈界毫秒，`$2`/`$3` 元组下界，
	// `$4` Agent 集合，`$5` limit）。
	// Subsequent non-NULL pages advance by the (next_fire_at, task_id) tuple.
	sweepDueAfterQuery = `
SELECT ` + taskRecordColumns + `, next_fire_at
  FROM scheduled_tasks
 WHERE enabled AND retired_at IS NULL
   AND agent_name = ANY($4::text[])
   AND next_fire_at IS NOT NULL
   AND next_fire_at <= $1::bigint
   AND (next_fire_at, task_id) > ($2::bigint, $3::text)
 ORDER BY next_fire_at ASC, task_id ASC
 LIMIT $5::bigint`
)

// ReadSweepBounds 冻结一圈到期扫描的边界（`core.AgentDueLister`）：数据库时间充当
// 本圈 `cutoff`（同时是开圈这一拍的 `dbNow`）与 NULL 段最大 `task_id`。两者必须来自
// **同一条语句的同一个快照**——分两次读会出现"上界取自更晚快照、cutoff 取自更早"，
// 本圈就可能收进不该考察的行。读失败由调用方放弃本拍，**不得**用进程时钟顶替。
// ReadSweepBounds freezes one sweep from a single snapshot; a failure abandons the pass.
func (s *TaskStore) ReadSweepBounds(ctx context.Context, agents []string) (core.SweepBounds, error) {
	if len(agents) == 0 {
		// 零集合不是"没有边界"，而是调用方装配错误：给零值 cutoff 会让整圈都没有
		// 到期行，静默吞掉这类错误比报错更糟。调度器已在此之前返回。
		// An empty agent set is an assembly error, not a bound of zero.
		return core.SweepBounds{}, errors.New("读取到期扫描边界: Agent 集合为空")
	}
	var (
		ms      int64
		nullMax string
	)
	if err := s.db.pool.QueryRowContext(ctx, sweepBoundsQuery, agents).Scan(&ms, &nullMax); err != nil {
		return core.SweepBounds{}, fmt.Errorf("读取到期扫描边界: %w", err)
	}
	return core.SweepBounds{
		Cutoff:          time.UnixMilli(ms).UTC(),
		NullUpperTaskID: nullMax,
	}, nil
}

// ListDispatchableForAgents 是 `ListDispatchable` 的按 Agent 收窄 + 两段键集分页版本
// （`core.AgentDueLister`）：单拍**至多一页**，按本圈冻结边界截断。
//
// 多 Worker 共用一张 `scheduled_tasks` 时，全局批次会被其他工作区的到期行占满（它们
// 既不归本进程领取，也不会被本进程推进），本地 Agent 因此永远进不了批次——按注册集合
// 先收窄就消除了这种饿死（F6）；两段轮转则消除"前 N 条长期不可领取占满每一批"的饥饿。
// `after == nil`（或未携带圈界）只在轻装配 / 直接调用时出现：按旧全局语义取首屏，
// 存储层**不**自行取边界。
// ListDispatchableForAgents narrows and pages the due scan; storage never freezes bounds
// itself.
func (s *TaskStore) ListDispatchableForAgents(ctx context.Context, agents []string,
	after *core.DueScanCursor, limit int,
) ([]core.DueTask, error) {
	if len(agents) == 0 || limit <= 0 {
		return nil, nil
	}
	if after == nil || after.Sweep == nil {
		return s.listDue(ctx, dueTasksLocalQuery, []any{limit, agents}, limit)
	}
	var out []core.DueTask
	if after.InNullSegment && after.Sweep.NullUpperTaskID != "" {
		nullPage, err := s.listDue(ctx, sweepNullSegmentQuery,
			[]any{after.NullAfterTaskID, agents, after.Sweep.NullUpperTaskID, limit}, limit)
		if err != nil {
			return nil, err
		}
		out = append(out, nullPage...)
		if len(out) >= limit {
			return out, nil
		}
		// NULL 段不足一页：本圈 NULL 段到界，同拍用剩余容量取非 NULL 段。此时游标
		// 里的元组下界属于上一圈的非 NULL 段之外的位置——开圈后首次进入非 NULL 段
		// 时 `AfterNextFireAt` 为零，正是"首屏不拼元组下界"的分支。
		// A short NULL page falls through to the non-NULL segment in the same pass.
	}
	rest, err := s.listSweepDueSegment(ctx, agents, after, limit-len(out))
	if err != nil {
		return nil, err
	}
	return append(out, rest...), nil
}

// listSweepDueSegment 取非 NULL 段的一页：`AfterNextFireAt == nil` 是首屏，**不得**
// 拿 NULL 拼元组下界。
// listSweepDueSegment pages the non-NULL segment; a nil tuple cursor means first page.
func (s *TaskStore) listSweepDueSegment(ctx context.Context, agents []string,
	after *core.DueScanCursor, limit int,
) ([]core.DueTask, error) {
	cutoffMS := after.Sweep.Cutoff.UnixMilli()
	if after.AfterNextFireAt == nil {
		return s.listDue(ctx, sweepDueFirstPageQuery, []any{cutoffMS, agents, limit}, limit)
	}
	return s.listDue(ctx, sweepDueAfterQuery,
		[]any{cutoffMS, after.AfterNextFireAt.UnixMilli(), after.AfterTaskID, agents, limit}, limit)
}
