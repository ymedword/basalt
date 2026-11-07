package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// 本文件实现 `core.SubtreeGate`：Flow 根子树的**三个互相独立**的只读判据
// （模块卡 §2.3.6 / FR-24-20 / FR-24-38）。
//
// 为什么必须拆成三个而不是一两个复合查询：Isolated（还有没有人能提交）与
// Joined（还有没有在途 attempt）**不能互相推导**。若用后者推导前者，接管方在
// 看到 interrupted attempt 时会把它当作"还在跑"，从而既不能 Recover（要求
// Isolated）也不能 Advance（要求 Joined），形成死锁；反过来用 TTL 推断子 Run
// 已退出则会把"租约仍有效的在跑 Run"误判成可恢复，直接覆盖别人正在写的结果。
//
// 三个查询都**只读**：不写租约、不写业务行、不改状态。回收器拿到判据后自行决定
// 释放 / 恢复 / 放弃，判据本身不含任何结论。
// This file implements core.SubtreeGate: three independent read-only predicates over a
// Flow root's subtree. Isolated and Joined cannot be derived from each other, and TTL
// must never be used to infer that a child Run has exited.

// SubtreeGate 是 `core.SubtreeGate` 的 PostgreSQL 实现。
// SubtreeGate implements core.SubtreeGate against PostgreSQL.
type SubtreeGate struct{ db *DB }

// NewSubtreeGate 装配判据查询。db 为空时三个方法一律报错，不静默返回 false——
// "读不到"被当成"没有"会让回收器对着不存在的结论推进。
// NewSubtreeGate wires the gate. A nil handle is reported, never silently treated as
// "no live lease".
func NewSubtreeGate(db *DB) *SubtreeGate { return &SubtreeGate{db: db} }

// subtreeQueryer 抽掉 `*sql.DB` 与 `*sql.Tx` 的差异：三个判据既能独立跑，也能在
// 回收器已开启的事务里与其它读共用一个快照。
type subtreeQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// existsInSubtree 执行一条 `SELECT EXISTS(...)` 查询并返回布尔结果。
// existsInSubtree runs one SELECT EXISTS and returns its boolean.
func (g *SubtreeGate) existsInSubtree(ctx context.Context, query, rootTaskID, what string) (bool, error) {
	if g == nil || g.db == nil || g.db.pool == nil {
		return false, fmt.Errorf("postgres: 子树判据（%s）缺少数据库句柄", what)
	}
	if rootTaskID == "" {
		return false, fmt.Errorf("postgres: 子树判据（%s）缺少根任务标识", what)
	}
	return querySubtreeExists(ctx, g.db.pool, query, rootTaskID, what)
}

// querySubtreeExists 是 existsInSubtree 的自由函数形态，供需要传入事务的调用方复用。
func querySubtreeExists(ctx context.Context, q subtreeQueryer, query, rootTaskID, what string) (bool, error) {
	var found bool
	if err := q.QueryRowContext(ctx, query, rootTaskID).Scan(&found); err != nil {
		return false, fmt.Errorf("子树判据（%s）: %w", what, err)
	}
	return found, nil
}

// subtreeLiveRunLeaseQuery / subtreeInFlightAttemptQuery / subtreeUnresolvedActionQuery
// 是三个判据的**唯一实现**：`SubtreeGate` 与 `RecoverFlowRoot`（A3 取证）都读这一份。
// 两处各写一份同义 SQL 就会有第二个漂移点——一边改了判据、另一边还在用旧口径，
// 而这种不一致只在跨 Worker 接管时才暴露。
// The three predicates have exactly one implementation, shared by the gate and A3.

// subtreeLiveRunLeaseQuery 判 Isolated：子树里是否还有**仍有效**的 run 租约。
//
// 只看租约、**不看** `runs.status`：正常收尾在写终态之后才释放租约，因此
// "终态行 + 未释放租约"只可能出现在崩溃残留里，而那正是回收器要处理的对象；
// 反过来按 status 过滤会把"已终态但租约还在"的残留排除在 Isolated 之外，让
// Recover 永远等不到放行。时间取 `statement_timestamp()`：整条语句内恒定。
const subtreeLiveRunLeaseQuery = `
SELECT EXISTS (
    SELECT 1
      FROM flow_tasks t
      JOIN runs r ON r.flow_task_id = t.task_id
      JOIN execution_leases l ON l.kind = 'run' AND l.resource_id = r.run_id
     WHERE t.root_task_id = $1
       AND l.released_at IS NULL
       AND l.expires_at > (extract(epoch from statement_timestamp()) * 1000)::bigint
)`

// subtreeInFlightAttemptQuery 判 Joined：子树里是否还有 prepared / running 的尝试。
// interrupted / unknown **不计入**：它们只能由 Recover 清除，若把它们算作在途，
// 接管方将永远等不到 Joined，循环依赖就此闭合。
const subtreeInFlightAttemptQuery = `
SELECT EXISTS (
    SELECT 1
      FROM flow_tasks t
      JOIN flow_node_attempts n ON n.task_id = t.task_id
     WHERE t.root_task_id = $1
       AND n.state IN ('prepared','running')
)`

// subtreeUnresolvedActionQuery 判门 ③：子树里是否还有 outcome ∈ ('dispatching','unknown')
// 的动作。命中即**不得** Advance（那条外部动作可能已经发出），也**不得** Release 租约，
// 必须 Abandon 让下一轮重新 TookOver=true 走 Recover 查证。
const subtreeUnresolvedActionQuery = `
SELECT EXISTS (
    SELECT 1
      FROM flow_tasks t
      JOIN flow_actions a ON a.task_id = t.task_id
     WHERE t.root_task_id = $1
       AND a.outcome IN ('dispatching','unknown')
)`

// HasLiveRunLease 报告根子树里是否还有**仍有效**的 run 租约（未释放且未过期）。
// 为 false 即 Isolated：没有任何 Worker 还能往这棵子树提交，只放行 Recover。
// HasLiveRunLease reports whether any run lease in the subtree is still live
// (not released and not expired). False means Isolated.
func (g *SubtreeGate) HasLiveRunLease(ctx context.Context, rootTaskID string) (bool, error) {
	return g.existsInSubtree(ctx, subtreeLiveRunLeaseQuery, rootTaskID, "HasLiveRunLease")
}

// HasInFlightNodeAttempt 报告根子树里是否还有 prepared / running 的节点尝试。
// 为 false 即 Joined：可以放心 Advance / 复用已完成节点。
// HasInFlightNodeAttempt reports whether any prepared/running attempt remains in the
// subtree; interrupted/unknown deliberately do not count.
func (g *SubtreeGate) HasInFlightNodeAttempt(ctx context.Context, rootTaskID string) (bool, error) {
	return g.existsInSubtree(ctx, subtreeInFlightAttemptQuery, rootTaskID, "HasInFlightNodeAttempt")
}

// HasUnresolvedAction 报告根子树里是否还有 outcome ∈ ('dispatching','unknown') 的动作
// （门 ③：R7 追加）。
// HasUnresolvedAction reports unresolved (dispatching/unknown) actions in the subtree;
// a hit forbids both Advance and Release.
func (g *SubtreeGate) HasUnresolvedAction(ctx context.Context, rootTaskID string) (bool, error) {
	return g.existsInSubtree(ctx, subtreeUnresolvedActionQuery, rootTaskID, "HasUnresolvedAction")
}

// taskInFlightAttemptQuery 是 `subtreeInFlightAttemptQuery` 的**任务作用域**对应物：
// 锚点是"本次推进的目标任务"连同它的后代，而不是整棵根子树。
//
// 为什么必须有第二个作用域：`advanceSubflow` 在进入子任务循环**之前**就给父 subflow
// 节点 `Claim` 了一条 running attempt（subflow 节点没有 Run，attempt 就是它的执行
// 凭证）。子任务里复用已完成叶子时若仍扫整棵根子树，挡住复用的正是**调用方自己刚
// 建的那条 attempt**：Joined=false → ErrCapacity → `flow.Worker.handleJobError` 把
// 它当竞争错误吞掉 → 下一轮同样先 Claim 再复用，于是永远推进不了。锚定到目标任务后，
// 祖先链上的 attempt（本轮的父 / 祖父 subflow 节点）不参与判定；后代的在途 attempt
// 仍然计入。
//
// 与根口径一致的两点：interrupted / unknown **不计入**（它们是复用 / 查证本身的
// 输入），且只看持久事实——TTL 不参与任何推断。
// Task-scoped counterpart of subtreeInFlightAttemptQuery: the anchor and its
// descendants only, so the caller's own parent subflow attempt cannot self-block reuse.
const taskInFlightAttemptQuery = `
WITH RECURSIVE scope AS (
    SELECT task_id
      FROM flow_tasks
     WHERE task_id = $1
    UNION ALL
    SELECT child.task_id
      FROM flow_tasks child
      JOIN scope ON child.parent_task_id = scope.task_id
)
SELECT EXISTS (
    SELECT 1
      FROM flow_node_attempts n
      JOIN scope ON scope.task_id = n.task_id
     WHERE n.state IN ('prepared','running')
)`

// HasInFlightNodeAttemptUnder 报告锚点任务**自身子树**里是否还有 prepared / running
// 的节点尝试。子任务复用已完成节点时的 Joined 用它；根任务仍走
// `HasInFlightNodeAttempt`（整棵根子树）。两者口径不同，**不得**互相替代。
// HasInFlightNodeAttemptUnder reports in-flight attempts under the anchor task only.
func (s *FlowStore) HasInFlightNodeAttemptUnder(ctx context.Context, taskID string) (bool, error) {
	if s == nil || s.db == nil || s.db.pool == nil {
		return false, fmt.Errorf("postgres: 任务子树判据（HasInFlightNodeAttemptUnder）缺少数据库句柄")
	}
	if taskID == "" {
		return false, fmt.Errorf("postgres: 任务子树判据（HasInFlightNodeAttemptUnder）缺少任务标识")
	}
	return querySubtreeExists(ctx, s.db.pool, taskInFlightAttemptQuery, taskID, "HasInFlightNodeAttemptUnder")
}
