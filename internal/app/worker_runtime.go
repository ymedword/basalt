package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// 本文件是 Worker 身份与诊断心跳的装配面（US-24 T006 / FR-24-01 / 模块卡图 1）。
//
// 三条边界：
//
//  1. **身份是 InstanceID，不是 Name**。实例标识每个进程随机生成一次；`worker_name`
//     允许重复（两台机器同名是运维常态），任何归属判定都只看租约，不看名字。
//  2. **心跳只作诊断**。心跳失败只记 WARN，不终止进程、不参与"某个 Worker 是否还活着"
//     的判定——存活与归属由 `execution_leases.expires_at` 决定（回收器范畴）。
//  3. **注册失败即启动失败**。没有身份就不参与协调；不接受"先跑起来再补登记"。
//
// This file carries the worker identity and its diagnostic heartbeat. Heartbeats never
// decide ownership; only execution_leases.expires_at does. A failed registration aborts
// startup: without an identity the process does not take part in coordination.

// workerStopTimeout 是停机时标记 Worker 下线的独立预算。停机信号已经取消了父 ctx，
// 这里用 WithoutCancel 保留值、另给一段短预算——与审计终态写同一口径
// （模块卡 §2.3.11 [7]）。
// workerStopTimeout is the independent budget for marking the worker stopped after the
// parent context has been cancelled.
const workerStopTimeout = 5 * time.Second

// workerRuntime 持有一个 Worker 的身份、登记端口、心跳循环与回收循环的句柄。
// workerRuntime holds the worker identity, the registry port, and both loops.
type workerRuntime struct {
	worker    core.WorkerIdentity
	registry  core.WorkerRegistry
	reclaimer core.Reclaimer
	logger    *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}

	// 回收循环的句柄与心跳分开：停机时**先停领取**（回收属于领取面），再停心跳——
	// 反过来的话，心跳先停而回收还在跑，运维台账会出现"已下线却仍在收尾"的窗口
	// （模块卡 §2.3.11 [1] 的停机次序）。
	// The reclaim loop stops before the heartbeat: stop claiming first, then diagnostics.
	reclaimCancel context.CancelFunc
	reclaimDone   chan struct{}
}

// newWorkerInstanceID 生成 128 位随机 Worker 实例标识（hex 编码、`wrk_` 前缀便于在
// 日志与运维视图里一眼认出这是**进程**身份而不是工作区名册的 `inst_` 标识）。随机源
// 不可用时返回错误而不是吞掉——拿不到唯一身份就无法参与协调，必须让启动失败。
//
// 与模块卡 §2.3.1 伪代码的**唯一偏差**：伪代码写 `newInstanceID()`；实现改名
// `newWorkerInstanceID`，因为 `instance_identity.go` 已有同名的**工作区名册**标识
// 生成器（`inst_` 前缀、落盘不可变），两者语义不同，共名会让调用方误配。签名另增
// error 返回：`crypto/rand` 失败必须上抛。两处偏差已在 run-state §1.3 登记。
// newWorkerInstanceID mints a random 128-bit process id. Renamed from the pseudocode's
// newInstanceID to avoid colliding with the workspace-roster generator, and widened
// with an error return so an entropy failure aborts startup.
func newWorkerInstanceID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成 Worker 实例标识: %w", err)
	}
	return "wrk_" + hex.EncodeToString(buf[:]), nil
}

// newWorkerRuntime 构造句柄；两个循环由 StartHeartbeat / StartReclaimer 显式启动
// （构造不启动）。reclaimer 允许为 nil：SQLite 谱系与不接执行权的轻装配没有租约面，
// 但那种装配本来就不会走到 `startWorkerRuntime`。
// newWorkerRuntime builds the handle; both loops start explicitly. A nil reclaimer is
// tolerated here because leased assemblies always supply one.
func newWorkerRuntime(worker core.WorkerIdentity, registry core.WorkerRegistry,
	reclaimer core.Reclaimer, logger *slog.Logger,
) *workerRuntime {
	if logger == nil {
		logger = slog.Default()
	}
	return &workerRuntime{worker: worker, registry: registry, reclaimer: reclaimer, logger: logger}
}

// StartHeartbeat 启动诊断心跳循环：每 interval 刷新一次 `worker_nodes.last_heartbeat_at`。
// 单次心跳用 `context.WithTimeout(interval)` 有界，避免一次卡住的写入把下一拍挤掉；
// 失败只记 WARN（含实例标识），不取消任何执行上下文——心跳不是占有权证明。
// StartHeartbeat runs the diagnostic loop; a failed beat is a warning, never fatal.
func (r *workerRuntime) StartHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = cli.DefaultWorkerConfig().HeartbeatInterval()
	}
	hbCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.done = make(chan struct{})

	go func() {
		defer close(r.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				beatCtx, beatCancel := context.WithTimeout(hbCtx, interval)
				err := r.registry.Heartbeat(beatCtx, r.worker.InstanceID)
				beatCancel()
				// 停机导致的取消不是心跳失败：父 ctx 已结束就不再刷 WARN。
				if err != nil && hbCtx.Err() == nil {
					r.logger.Warn("Worker 心跳失败（仅诊断，归属仍由租约判定）",
						"instance_id", r.worker.InstanceID, "error", err)
				}
			}
		}
	}()
}

// Stop 按"停止领取 → 停止诊断 → 标记下线"的次序收尾（模块卡 §2.3.11 [1]）：回收循环
// 属于**领取面**，先停它，进程才不会再领到新的过期租约；心跳随后停；最后在**独立短
// 预算**里写下线标记。预算耗尽只报错，不阻塞停机——下线标记是诊断，不是停机成败的判据。
// Stop ceases claiming first (the reclaim loop), then the heartbeat, then marks the
// instance stopped within an independent budget.
func (r *workerRuntime) Stop(ctx context.Context) error {
	if r.reclaimCancel != nil {
		r.reclaimCancel()
	}
	if r.reclaimDone != nil {
		select {
		case <-r.reclaimDone:
		case <-ctx.Done():
			return fmt.Errorf("等待 Worker %s 回收循环退出: %w", r.worker.InstanceID, ctx.Err())
		}
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.done != nil {
		select {
		case <-r.done:
		case <-ctx.Done():
			return fmt.Errorf("等待 Worker %s 心跳循环退出: %w", r.worker.InstanceID, ctx.Err())
		}
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerStopTimeout)
	defer cancel()
	if err := r.registry.Stop(stopCtx, r.worker.InstanceID); err != nil {
		return fmt.Errorf("标记 Worker %s 下线: %w", r.worker.InstanceID, err)
	}
	return nil
}

// workerSettings 取出 `worker` 段；配置文件整体缺失时（`cfg == nil` 或未经解码的零值）
// 退回 §3.3 的冻结缺省。段存在时解析入口已填入缺省并通过装载期校验，这里不再判。
// workerSettings falls back to the frozen defaults only when the config file itself is
// absent; a present section has already been defaulted and validated at load time.
func workerSettings(cfg *cli.Config) cli.WorkerConfig {
	if cfg == nil || cfg.Worker.LeaseTTLSeconds == 0 {
		return cli.DefaultWorkerConfig()
	}
	return cfg.Worker
}

// workerHostname 取主机名。取不到返回空串：`hostname` 是诊断列，采集失败不该阻止
// 一个本来能正常协调的进程启动。
// workerHostname is diagnostic; an unavailable hostname never blocks startup.
func workerHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// mintWorkerIdentity 生成一次进程身份。装配期（`newRuntime`）生成、运行入口与登记
// 复用同一个值：`StartRun` 写下的 `owner_worker_id` 与 `worker_nodes.instance_id` 若
// 各自生成，租约归属会指向一个不存在的实例，排障时无法把租约对回进程。
// mintWorkerIdentity mints the process identity once; admission-time leases and the
// serve-time registration must name the very same instance.
func mintWorkerIdentity(cfg *cli.Config) (core.WorkerIdentity, error) {
	instanceID, err := newWorkerInstanceID()
	if err != nil {
		return core.WorkerIdentity{}, err
	}
	name := workerSettings(cfg).Name
	if name == "" {
		name = workerHostname()
	}
	return core.WorkerIdentity{
		InstanceID:               instanceID,
		Name:                     name,
		Hostname:                 workerHostname(),
		Version:                  Version,
		SchemaVersion:            postgres.SupportedSchemaVersion,
		HeartbeatIntervalSeconds: workerSettings(cfg).HeartbeatIntervalSeconds,
	}, nil
}

// startWorkerRuntime 按模块卡图 1 装配 Worker：Register（失败即启动失败）→ **首轮
// `ReclaimOnce`（失败同样启动失败）** → 心跳与回收两个循环。身份由装配期生成后传入
// （见 `mintWorkerIdentity`）；chat / tool 不调用本函数。
//
// 首轮回收必须是**同步的启动前提**而不是后台第一拍：它负责把上一代进程遗留的过期租约
// 收尾（含迁移 0012 回填的 `legacy` 租约，FR-24-31），这一轮没跑成功就开服务等于让
// 残留的 running 行继续冒充在跑。回收器自身是固定的有界维护循环（FR-24-17），启动期
// 首轮沿用同一入口，不另造一条同步恢复路径。
// startWorkerRuntime registers the identity, runs the first ReclaimOnce as a startup
// prerequisite, then starts the heartbeat and reclaim loops.
func (a *App) startWorkerRuntime(ctx context.Context, db *postgres.DB,
	worker core.WorkerIdentity, reclaimer core.Reclaimer,
) (*workerRuntime, error) {
	cfg := workerSettings(a.cfg)
	// 登记表同时是概览读数的来源：注入本部署的心跳间隔，迁移前旧行（列为 NULL）
	// 的判阈用它兜底并在详情标注来源。
	registry := postgres.NewWorkerStore(db, postgres.WithDefaultHeartbeatInterval(cfg.HeartbeatIntervalSeconds))
	if err := registry.Register(ctx, worker); err != nil {
		return nil, fmt.Errorf("register worker: %w", err)
	}
	if reclaimer == nil {
		return nil, fmt.Errorf("worker %s 缺少回收器：租约面装配不完整", worker.InstanceID)
	}
	report, err := reclaimer.ReclaimOnce(ctx, cfg.ReclaimBatch)
	if err != nil {
		return nil, fmt.Errorf("首轮回收: %w", err)
	}
	a.loggerOr().Info("Worker 首轮回收完成",
		"instance_id", worker.InstanceID, "runs", report.Runs,
		"executions", report.Executions, "flow_roots", report.FlowRoots,
		"pending_audit", report.PendingAudit, "errors", report.Errors)
	rt := newWorkerRuntime(worker, registry, reclaimer, a.loggerOr())
	rt.StartHeartbeat(ctx, cfg.HeartbeatInterval())
	rt.StartReclaimer(ctx, cfg.ReclaimInterval(), cfg.ReclaimBatch)
	a.loggerOr().Info("Worker 已登记",
		"instance_id", worker.InstanceID, "name", worker.Name,
		"schema_version", worker.SchemaVersion, "heartbeat_interval", cfg.HeartbeatInterval().String(),
		"reclaim_interval", cfg.ReclaimInterval().String(), "reclaim_batch", cfg.ReclaimBatch)
	return rt, nil
}
