package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 本文件是分布式执行的**消费方端口**：执行权类型、归属凭证、哨兵与四个接口。
// `core` 只声明自己需要的形状，实现分别落在 `internal/storage/postgres`（租约仓储、
// Worker 登记、回收领取、子树判据）与 `internal/app`（回收器组合）。
// This file declares the execution-ownership ports of the distributed runtime: lease
// kinds, the ownership token, sentinels and four consumer interfaces. Implementations
// live in internal/storage/postgres and internal/app.

// LeaseKind 是执行权类型。新增值必须同时改 CHECK 与解析器，未知值 fail-closed。
// LeaseKind enumerates execution-ownership domains; unknown values fail closed.
type LeaseKind string

const (
	// LeaseRun 覆盖交互 / 调度 / Flow 节点的**每一次 Run**：一条 Run 行一个租约。
	LeaseRun LeaseKind = "run"
	// LeaseScheduleExecution 覆盖**一条定时执行行**：它管的是执行行的收尾，
	// 不是它派生的 Run（那是 LeaseRun）。
	LeaseScheduleExecution LeaseKind = "schedule_execution"
	// LeaseFlowRoot 覆盖**一个 Flow 根任务**：子任务推进走父根的租约。
	LeaseFlowRoot LeaseKind = "flow_root"
)

// ParseLeaseKind 逐字解析 `execution_leases.kind` 闭集；空串、未知值、大小写变体
// 一律拒绝。闭集是 fail-closed 的：库里出现读不懂的 kind 必须先升级程序，
// 不得把它当成某个已知类型继续处理。
// ParseLeaseKind accepts exactly the three stored lease kinds; anything else is
// refused (fail-closed).
func ParseLeaseKind(raw string) (LeaseKind, error) {
	switch kind := LeaseKind(raw); kind {
	case LeaseRun, LeaseScheduleExecution, LeaseFlowRoot:
		return kind, nil
	default:
		return "", fmt.Errorf("core: 未知的租约类型 %q / unknown lease kind", raw)
	}
}

var (
	// ErrLeaseHeld 表示资源由另一未过期租约持有：不等待、不排队。
	ErrLeaseHeld = errors.New("core: lease is held by another owner")
	// ErrLeaseLost 表示负责人 / 代次不匹配或租约已过期：调用方必须停止新动作。
	ErrLeaseLost = errors.New("core: lease ownership lost")
	// ErrLeaseRenewDeadline 是 keepLease 的取消原因：连续续约失败已达
	// ttl - renewInterval，keeper 必须退出、执行侧不得再发起新动作。
	ErrLeaseRenewDeadline = errors.New("core: lease renew deadline exceeded")
	// ErrCancelRequested 是 keepLease 把"他处取消请求"传给执行侧的取消原因；
	// 它**不**由 Renew 以 error 返回——取消请求随成功续约的 Lease.CancelRequested
	// 字段带回（与 flow.Task.CancelRequested 同一形态）。
	ErrCancelRequested = errors.New("core: cancel requested by another worker")
	// ErrCallerConcurrencyLimited 表示调用方并发名额已满（HTTP 429）。它是新的权威
	// 哨兵；callers.ErrConcurrencyLimited 改为别名指向本值，既有 errors.Is 测试不变。
	ErrCallerConcurrencyLimited = errors.New("core: caller concurrency limit reached")
	// ErrTriggerAlreadyClaimed / ErrStaleNextFire 表示另一 Worker 已领取或推进。
	ErrTriggerAlreadyClaimed = errors.New("core: trigger already claimed")
	ErrStaleNextFire         = errors.New("core: scheduled next_fire_at changed")
	// ErrTaskDisabled 表示到期扫描之后任务已被停用：领取必须在任务行锁内重检最新
	// `enabled`，停用即拒绝，不得落下执行行或租约。它与 ErrTriggerAlreadyClaimed
	// 同属"本轮无声跳过"，不是故障；手动补跑（StartExecution）不受它影响。
	// ErrTaskDisabled marks a cron claim rejected by a committed disable; the claim
	// must recheck the latest enabled flag under the task row lock.
	ErrTaskDisabled = errors.New("core: task disabled after scan")
)

// WorkerIdentity 是一次进程的身份。InstanceID 是协调身份，Name 只作诊断。
// SchemaVersion 是运行中进程实际装载的 schema 版本，只在 Register 时写入
// worker_nodes.schema_version，进程生命周期内不变（心跳不重复上报）。
// 回收器 / 管理面用它与库的 SupportedSchemaVersion 比对：不一致即存在混跑，
// 管理面拒绝"在线混跑"式升级（Q15 的停机迁移口径）。
// WorkerIdentity is the identity of one process. InstanceID coordinates; Name is
// diagnostic only. SchemaVersion is written once at Register and never refreshed.
type WorkerIdentity struct {
	InstanceID    string
	Name          string
	Hostname      string
	Version       string
	SchemaVersion int
}

// IsZero 判定"这次装配没有 Worker 身份"——未接执行权的轻装配与 SQLite 源谱系为真。
// 带租约的准入（`StartRun`）收到非零 owner 才创建租约行；反过来，不带租约的后端
// 收到非零 owner 是装配缺陷，必须显式拒绝而不是悄悄丢掉身份。
// IsZero reports an assembly without a worker identity; a lease-bearing backend must
// refuse a zero owner and a lease-less backend must refuse a non-zero one.
func (w WorkerIdentity) IsZero() bool { return w == WorkerIdentity{} }

// LeaseToken 是提交时携带的归属凭证；业务状态显式传参，不放进 context。
// LeaseToken is the ownership credential carried by every fenced commit. Business
// state travels as an explicit argument, never through context.WithValue.
type LeaseToken struct {
	Kind           LeaseKind
	ResourceID     string
	OwnerWorkerID  string
	OwnershipEpoch int64
}

// IsZero 判定"这次写入没有执行权凭证可带"——零值 token 只出现在未接执行权的装配
// （轻装配、单进程 SQLite 谱系与零值单测）；一旦 Run 经 `StartRun` 领取租约，token
// 恒为非零，受保护写入面据此选择 fenced 入口。用**四字段全零**判，不单看 ResourceID：
// 只填了一半的 token 不是"没有凭证"，而是装配缺陷，必须继续走 fenced 分支由存储拒绝。
// IsZero reports the zero credential used by unleased assemblies; a half-filled token is
// an assembly defect, not an absent credential, so it still takes the fenced branch.
func (t LeaseToken) IsZero() bool { return t == LeaseToken{} }

// Lease 是租约的当前视图。CancelRequested 只对 kind='run' 有意义：它把
// runs.cancel_requested 随续约响应带回属主，避免另开轮询通道。
//
// TookOver 只在 Acquire 的返回值上有意义，且**刻意不放进 LeaseToken**：
// 它是"这一次领取发生了什么"的结论，不是提交时携带的归属凭证。放进 token
// 会让 Renew / Release / FencedTx 的调用方误以为它参与归属校验；实际上
// 归属只看 OwnerWorkerID + OwnershipEpoch。
// Lease is the current view of a lease. TookOver is deliberately kept out of
// LeaseToken: it is a conclusion about one acquisition, not a credential.
type Lease struct {
	LeaseToken
	AcquiredAt      time.Time
	RenewedAt       time.Time
	ExpiresAt       time.Time
	CancelRequested bool
	TookOver        bool
}

// ReclaimReport 是一轮回收的计数；错误只计数，详细错误进日志。
// ReclaimReport counts one reclamation pass; individual errors are logged.
type ReclaimReport struct {
	Runs         int
	Executions   int
	FlowRoots    int
	PendingAudit int
	Errors       int
}

// Tx 是受归属保护事务的句柄。`FencedTx` 的签名（模块卡 §3.1.1）把事务交给
// 调用方的写入闭包，但 `core` 不能 import 任何驱动包，因此这里只声明
// `database/sql` 的**标准三方法**子集：`*sql.Tx` 天然满足，postgres 实现可原样
// 传入，无需断言或包装。`core` 自身不调用这些方法。
// Tx is the consumer-side handle of a fenced transaction. It declares the plain
// database/sql subset so *sql.Tx satisfies it directly; core never calls it.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LeaseStore 是执行权归属的唯一读写端口。所有时间判定由实现使用数据库时间。
// LeaseStore is the single read/write port for ownership; time comes from the
// database clock inside the implementation.
type LeaseStore interface {
	// owner 是领取者身份：租约行的 owner_worker_id 由此写入，续约 / 释放 / fencing
	// 都拿它比对；没有 owner 就无法建立归属。
	// 返回的 Lease.TookOver 表示本次领取来自"已过期"的旧持有者（需先 Recover）。
	Acquire(ctx context.Context, kind LeaseKind, resourceID string,
		owner WorkerIdentity, ttl time.Duration) (Lease, error)
	Renew(ctx context.Context, token LeaseToken, ttl time.Duration) (Lease, error)
	Release(ctx context.Context, token LeaseToken) error

	// FencedTx 是受归属保护的写入唯一入口：同一事务内先对租约行 SELECT … FOR UPDATE，
	// 校验 owner + epoch + 未过期后执行 fn；不符返回 ErrLeaseLost。
	// 互斥由行锁提供，不得退化为 EXISTS 快照。
	FencedTx(ctx context.Context, token LeaseToken, fn func(ctx context.Context, tx Tx) error) error
}

// 以下四个接口是**受归属保护的写入面**（模块卡 §2.3.3 图 3、FR-24-36）：消息追加、
// 工具审计两阶段写、模型调用 pending 预写——按 §2.3.3 的保护清单，它们是必须在
// `FencedTx` 内完成的写。它们与对应的普通写接口**并存而不替换**：
//
//   - 普通接口（`MessageStore` / `ToolInvocationStore` / `LLMCallStore` …）继续由未接
//     执行权的装配实现（SQLite 谱系、轻装配、单测替身），签名一字不改——没有 P 项
//     授权改它们，改签名会波及前序模块的公共接口（软门禁 4）；
//   - fenced 接口只由 `internal/storage/postgres` 实现；写入方按 `LeaseToken.IsZero()`
//     选入口：零值走普通写，非零值**必须**走 fenced 写，实现缺失即 fail-closed
//     （装配缺陷不得静默降级成无保护写——那正是"旧持有者覆盖新结果"的入口）。
//
// 为什么 fenced 方法自带业务参数而不是复用 `FencedTx` + 闭包：`core` 不能 import
// 任何驱动包，闭包里写不了 SQL；把"先锁租约行、再校验、再写"整体下沉到实现，
// 才能保证校验与业务写**处在同一事务**（§2.3.3 的硬性要求）。
// These four interfaces are the ownership-protected write surfaces (§2.3.3 figure 3,
// FR-24-36). They coexist with the plain interfaces rather than replacing them: the
// plain signatures are untouched, the fenced ones exist only in postgres, and callers
// pick by LeaseToken.IsZero(). A non-zero token with no fenced implementation is an
// assembly defect and fails closed instead of degrading to an unprotected write.

// FencedMessageStore 是消息追加的受保护入口（`messages` 写入面）。
// FencedMessageStore is the fenced entry point for message appends.
type FencedMessageStore interface {
	// FencedAppendMessage 在同一事务内先锁租约行、校验归属，再追加消息并返回行 id；
	// 失权返回 `ErrLeaseLost`，**不写入任何行**。
	// FencedAppendMessage locks the lease row, verifies ownership, then appends — one
	// transaction. Lost ownership returns ErrLeaseLost and writes nothing.
	FencedAppendMessage(ctx context.Context, token LeaseToken, sessionID, runID string, m Message) (string, error)
}

// FencedToolAuditStore 是工具审计两阶段写的受保护入口。
// FencedToolAuditStore is the fenced entry point of the two-phase tool audit.
type FencedToolAuditStore interface {
	// FencedBeginInvocation 在 fenced 事务内写 pending 行；失权返回 `ErrLeaseLost`，
	// 调用方据此**拒绝执行**该次工具调用（fail-closed 与失权叠加时同样不执行）。
	FencedBeginInvocation(ctx context.Context, token LeaseToken, inv ToolInvocation) (int64, error)

	// FencedFinishInvocation 在 fenced 事务内回填终态。失权时终态**不落库**、行留
	// pending（与写失败的既有处置一致：不覆盖真实结果，交回收器按作用域归一化）。
	FencedFinishInvocation(ctx context.Context, token LeaseToken, id int64, res ToolInvocationResult) error
}

// FencedChatCallStore 是对话模型调用 pending 预写的受保护入口。
// FencedChatCallStore is the fenced pre-write of a chat llm_calls row.
type FencedChatCallStore interface {
	// FencedBeginCall 在 fenced 事务内插入 pending 行；失权返回 `ErrLeaseLost`，
	// 调用方**拒绝发出**该次模型调用。
	FencedBeginCall(ctx context.Context, token LeaseToken, call LLMCall) (int64, error)
}

// FencedMemoryExtractCallStore 是记忆抽取 pending 预写的受保护入口。单列成接口
// （而不是并进 FencedChatCallStore）是为了让只做抽取的替身不必实现对话预写。
// FencedMemoryExtractCallStore is the fenced pre-write of a memory_extract row; kept
// separate so extract-only doubles need not implement the chat pre-write.
type FencedMemoryExtractCallStore interface {
	FencedBeginMemoryExtractCall(ctx context.Context, token LeaseToken, call LLMCall) (int64, error)
}

// ErrLeaseFenceUnavailable 表示装配给了非零执行权凭证，但该写入面的实现没有
// fenced 入口。它是**装配缺陷**而非业务状态：不得降级为普通写（那会绕开归属校验），
// 也不得重试——只能修改装配后重启。
// ErrLeaseFenceUnavailable flags an assembly defect: a non-zero credential reached a
// store with no fenced entry point. Never degrade to the plain write, never retry.
var ErrLeaseFenceUnavailable = errors.New("core: store has no fenced entry for a leased write")

// WorkerRegistry 只记录身份与心跳，不构成占有权证明。
// WorkerRegistry records identity and heartbeats; it is not proof of ownership.
type WorkerRegistry interface {
	Register(ctx context.Context, worker WorkerIdentity) error
	Heartbeat(ctx context.Context, instanceID string) error
	Stop(ctx context.Context, instanceID string) error
}

// WorkerNodeView 是 `worker_nodes` 的只读运维投影（模块卡 §3.1.12 / §3.1.13）：
// `basalt workers list` 与系统概览的 Worker 列表读同一份形状。
//
// 它是**新增的读模型**而不是 `WorkerRegistry` 的第四个方法：登记端口在模块卡
// §2.3.1 冻结为 Register / Heartbeat / Stop 三方法，给接口加列会改已定字面量。
// 身份是 InstanceID（Name 允许重复），时间全部来自数据库时钟；`ActiveLeases` 是
// 该实例名下**未释放且未过期**的租约数——心跳不参与这个计数。
// WorkerNodeView is the read-only projection of worker_nodes. It is a new read model
// rather than a fourth WorkerRegistry method: that interface is frozen at three
// methods, and extending it would change an approved literal. ActiveLeases counts the
// instance's unreleased, unexpired leases; heartbeats never feed into it.
type WorkerNodeView struct {
	InstanceID      string
	Name            string
	Hostname        string
	BuildVersion    string
	SchemaVersion   int
	StartedAt       time.Time
	LastHeartbeatAt time.Time
	// StoppedAt 非 nil 表示该实例已走完停机流程（再次登记会清空它）。
	// A non-nil StoppedAt means the instance finished its shutdown sequence.
	StoppedAt *time.Time
	// ActiveLeases 是该实例当前仍有效的执行权数量（只读诊断，不是占有权证明）。
	// ActiveLeases counts the instance's still-valid execution leases.
	ActiveLeases int64
}

// Reclaimer 每次只回收已过期或已释放的租约所覆盖的执行域。
// Reclaimer reclaims only domains covered by expired or released leases.
type Reclaimer interface {
	ReclaimOnce(ctx context.Context, limit int) (ReclaimReport, error)
}

// SubtreeGate 是 Flow 根子树的**三个独立判据**（R4 两判据 + R7 门 ③，见 §2.3.6）：
// 前两者合二为一会形成循环依赖——interrupted / unknown 只能由 Recover 清除，
// 而 Recover 又要求它们先消失。
//   - HasLiveRunLease：还有没有人能往这棵子树提交 → 反面即 Isolated，只放行 Recover
//   - HasInFlightNodeAttempt：还有没有 prepared / running attempt
//     → 反面即 Joined，只放行 Advance / reuseCompleted
//   - HasUnresolvedAction：子树下还有没有 outcome ∈ ('dispatching','unknown') 的
//     动作 → 为真即**不得** Advance，且**不得** Release（Abandon 逼出 TookOver=true）
//
// 实现：postgres.NewSubtreeGate；三个查询都只读，不写租约。
type SubtreeGate interface {
	HasLiveRunLease(ctx context.Context, rootTaskID string) (bool, error)
	HasInFlightNodeAttempt(ctx context.Context, rootTaskID string) (bool, error)
	HasUnresolvedAction(ctx context.Context, rootTaskID string) (bool, error)
}
