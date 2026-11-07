package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 runs（及其资源清单 / 引用）的**唯一 SQL 入口**。表 / 列 / 状态字面量以
// docs/contracts/schema.sql 为准。四条纪律。① **起始态不由调用方给**：`RunRecord` 没有状态
// 字段，`running` 由本文件的 SQL 固定写——与审计预写同一手法，调用方也就没有传错的可能。
// ② **不接收 finalText**：`runs` 表没有该列（§3.4 列级口径），最终回答只存在 `RunResult`
// 与会话的 assistant 消息里。③ **一次准入一笔账**：运行行、资源清单与全部引用在**同一
// 事务**里写入（约束 18）——任一失败整笔回滚，不会出现"Run 已开跑但没有资源账"。
// ④ **终态只覆盖仍未结束的行**：`FinishRun` 带 `WHERE status = 'running'` 并在**同一事务**
// 释放引用（约束 19 / 注 8）；已判定的终态不得被迟到的写入改口。
// This file is the only SQL entry point for runs, their manifests and references: the
// start state is SQL-fixed, no final text is persisted, admission writes run + manifest +
// every reference in one transaction, and terminal writes are guarded and release
// references in the same transaction.

// 四个 `trigger` 字面量（契约 §5 / §6.2 固定口径 8）。列名与表名一律内联在 SQL 字面量里
// （与 llm_calls.go 同风格）——不另立列清单常量，免得出现"第二份列定义"与 DDL 漂移。
// The four trigger literals; column names stay inline in the SQL for the same reason the
// rest of the package does: no second column list to drift from the DDL.
const (
	triggerUser   = "user"
	triggerAPI    = "api"
	triggerCron   = "cron"
	triggerManual = "manual"
)

// kindAgentBundle 是 Agent 配置包在**共享发布记录**里的资源种类（FR-24-25 / §2.3.9）：
// 0012 把 `resource_pointers` / `resource_versions` 的 CHECK 扩到这一项，它是
// "哪份 AGENT.md + 哪些技能文件"的持久权威。`resource_id` 存**规范化的 AgentUID**
// （`ag_` 前缀，与 `resource_uid` 同值），不存目录名。
// kindAgentBundle is the shared publication kind of an agent's content-addressed bundle;
// its resource_id carries the normalized AgentUID rather than the directory name.
const kindAgentBundle = "agent_bundle"

// defaultRunLeaseTTL 是 run 租约的缺省有效期（模块卡 §3.3 的 `worker.lease_ttl_seconds: 30`）。
// 续约间隔取 ttl/3，连续失败到 `ttl - renewInterval` 即有界停止（FR-24-05）。
// defaultRunLeaseTTL is the default run-lease lifetime (worker.lease_ttl_seconds: 30).
const defaultRunLeaseTTL = 30 * time.Second

// RunStore 是 runs 的唯一 SQL 入口（core.RunStore 的实现）。`ttl` 只影响
// `StartRun` 创建的 run 租约有效期；续约仍由 `LeaseStore.Renew` 按同一 TTL 推进。
// RunStore is the only SQL entry point for runs; ttl only sets the lifetime of the
// run lease created by StartRun.
type RunStore struct {
	db  *DB
	ttl time.Duration
}

// holdAdmissionLock 把"已持生命周期行锁"的暂停点转交给仅集成构建可见的钩子；生产构建里
// `db.faults` 为 nil，落到 `commitFaults` 空壳的同名方法（不解引用，安全）。
// holdAdmissionLock forwards to the integration-only hold point; a nil harness is a no-op.
func (s *RunStore) holdAdmissionLock(ctx context.Context) error {
	if err := s.db.faults.holdAdmissionLock(ctx); err != nil {
		return fmt.Errorf("准入暂停点: %w", err)
	}
	return nil
}

// 编译期断言：RunStore 满足消费方（core）声明的运行账本接口。
// Compile-time assertion that RunStore satisfies the consumer-declared interface.
var _ core.RunStore = (*RunStore)(nil)

// NewRunStore 绑定一个已打开的写模式库句柄（租约有效期取缺省 30s）。
// NewRunStore binds the store to an opened writable database handle.
func NewRunStore(db *DB) *RunStore {
	return &RunStore{db: db, ttl: defaultRunLeaseTTL}
}

// NewRunStoreWithLeaseTTL 用装配给出的 TTL 绑定存储。非正值按缺省值处理而不是
// 建一条"立即过期"的租约——那会让每次准入都在提交后立刻失权。
// NewRunStoreWithLeaseTTL takes the configured TTL; a non-positive value falls back
// to the default instead of minting an already-expired lease.
func NewRunStoreWithLeaseTTL(db *DB, ttl time.Duration) *RunStore {
	if ttl <= 0 {
		ttl = defaultRunLeaseTTL
	}
	return &RunStore{db: db, ttl: ttl}
}

// StartRun 建 `status = 'running'` 的账本行，并在**同一事务**内写入准入清单与全部资源
// 引用（约束 18）：任一失败整笔回滚（零 Run、零引用、零模型调用），失败 = 调用方**拒绝
// 启动**该次运行。
// StartRun inserts the running ledger row and, in one transaction, the admission manifest
// and its references; any failure rolls back all three.
//
// `trigger` 按渠道映射（§6 固定口径 8：cli → user、web → api、scheduler → cron、
// admin → manual）。映射是**显式表**而非推断：不认识的渠道返回错误，不落一行猜出来的
// 记账——`trigger` 列承担"区分定时与手动补跑"的审计职责（契约 §5），猜错比拒绝启动更糟。
// The trigger mapping is an explicit table; an unknown channel is refused rather than
// guessed, because the column carries audit meaning.
//
// **零清单 = 装配未接线**（`FormatVersion == 0`，见 core.RunStore 的文档）：只写运行行、
// **不伪造**清单行与引用——这类运行在读取面如实呈现为"未记录"（约束 24），假账会被当成
// 事实，后者更糟。
// A zero manifest means the wiring has not landed yet: only the run row is written, and
// the run reads back as unrecorded — a fabricated manifest would be read as fact.
//
// 清单哈希是**派生值**：写前重算 `HashManifest` 并核对，调用方给了不一致的清单直接拒绝
// ——落一行会被读侧判为损坏的账，不如在写侧失败关闭。`snapshot_json` 存完整清单（含
// `hash`），读侧重算的是"清空 hash 后的规整字节"，两者因此自洽（run_query.go）。
// The hash is re-derived and verified before any write; storing a self-inconsistent row
// would only move the failure to the read side.
func (s *RunStore) StartRun(ctx context.Context, rec core.RunRecord, manifest core.RunResourceManifest,
	owner core.WorkerIdentity, admission core.CallerAdmission, runID string,
) (core.LeaseToken, error) {
	trigger, err := triggerFor(rec.Channel, rec.Trigger)
	if err != nil {
		return core.LeaseToken{}, err
	}
	// 清单的静态校验在事务外完成：它只依赖入参（哈希重算 / 引用形状），不读库，
	// 放进事务只会白白拉长锁窗口。
	var (
		hash     string
		snapshot []byte
		refs     []manifestRef
	)
	if manifest.FormatVersion != 0 {
		hash, snapshot, refs, err = validateRunManifest(manifest)
		if err != nil {
			return core.LeaseToken{}, fmt.Errorf("预写运行账本（run_id=%s）: %w", rec.RunID, err)
		}
	}

	var token core.LeaseToken
	outcome, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 事务内的判定顺序**冻结**（模块卡 §2.3.2 / §3.1.2）：调用方 advisory →
		// 并发计数 → 会话 advisory → 会话活跃租约 → 发布 / 生命周期拦障 →
		// 任务次数记账 → runs / 清单 / 引用 / 租约。锁序只有这一种，fenced 写面
		// （advisory(会话) → 租约行）与学习写面（advisory(agent) → 租约行 → 业务行）
		// 都排在本方法之后，不会成环。
		// The in-transaction order is frozen by the module card; see the comment above.
		if admission.Enabled {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`,
				callerAdvisoryKey(rec.Principal.CallerID)); err != nil {
				return fmt.Errorf("获取调用方准入锁（caller=%s）: %w", rec.Principal.CallerID, err)
			}
			if err := checkCallerConcurrency(ctx, tx, rec.Principal.CallerID, admission); err != nil {
				return err
			}
		}
		if rec.SessionID != "" {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`,
				sessionAdvisoryKey(rec.SessionID)); err != nil {
				return fmt.Errorf("获取会话准入锁（session=%s）: %w", rec.SessionID, err)
			}
			busy, err := sessionHasLiveRunLease(ctx, tx, rec.SessionID)
			if err != nil {
				return err
			}
			if busy {
				return &core.SessionBusyError{SessionID: rec.SessionID}
			}
		}
		// 发布 / 生命周期最终复查（§2.3.9）。**当前只在清单携带 bundle 期望时启用**：
		// `AssetBundleHash` 非空是"资产准入管线产出了这份清单"的标记，而共享发布记录
		// （`agent_bundle` 指针）与生命周期索引由同一条管线维护；资产管线接线（T019 /
		// T029）之前没有任何生产者填它，此时强行复查只会把"尚未接线"误报成资产漂移。
		// T029（US7）负责把期望值接上并补齐六个准入拒绝用例——见 run-state 裁决记录。
		// The barrier is armed by a non-empty bundle expectation; T029 wires the producer.
		if manifest.FormatVersion != 0 && manifest.AssetBundleHash != "" {
			if err := admitPublishedResources(ctx, tx, rec, manifest); err != nil {
				return err
			}
			// 仅集成构建可见的暂停点：此刻本事务已持 `agent_lifecycle` 行锁，用例可在此
			// 证实"归档屏障与准入复查是同一行 FOR UPDATE 互斥"的两种真实获锁顺序
			// （FR-24-41）。生产构建的同名方法是无操作。
			// Integration-only hold point: the lifecycle row lock is already taken here.
			if err := s.holdAdmissionLock(ctx); err != nil {
				return err
			}
		}
		// 任务次数记账与准入同事务（Q21 / FR-24-33）：`ChargeRun` 不再参与本路径，
		// 预留与结算都由本事务内的私有 helper 完成；`admission.Enabled == false`
		// 是"内置管理员不限量"（与 `AdmitCall` 的 NoReservation 同一口径），不记账。
		if admission.Enabled {
			reservationID, err := chargeRunTaskQuotaTx(ctx, tx, rec.Principal.CallerID, runID,
				s.db.clock().UTC())
			if err != nil {
				return err
			}
			rec.ReservationID = reservationID
		}
		if err := insertRunRow(ctx, tx, rec, trigger); err != nil {
			return err
		}
		// 准入预留落库时 `run_id` 是 NULL（预留行先于 Run 行插入，FK 才能成立），
		// 账本行插入后在同一事务里回填——§3.4.2 的写入顺序逐字落地。
		// The reservation starts with run_id NULL and is linked after the run row.
		if err := backfillReservationRun(ctx, tx, rec); err != nil {
			return err
		}
		if manifest.FormatVersion != 0 {
			if err := insertRunManifestTx(ctx, tx, rec, manifest, hash, snapshot, refs); err != nil {
				return err
			}
		}
		if !owner.IsZero() {
			token, err = insertRunLeaseTx(ctx, tx, rec.RunID, owner, s.ttl)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// 提交结果未知：**不得**判未落、不得重试（FR-24-33）。映射成领域哨兵，
		// 由调用方按 run_id 回查（`LookupAdmittedRun`）。
		if errors.Is(err, ErrTxOutcomeUnknown) {
			return core.LeaseToken{}, fmt.Errorf("%w: %w", core.ErrAdmissionOutcomeUnknown, err)
		}
		return core.LeaseToken{}, err
	}
	if outcome != TxCommitted {
		// 回调未报错却不是已提交：装配缺陷（唯一写入口不得吞结局）。
		return core.LeaseToken{}, fmt.Errorf("预写运行账本（run_id=%s）: 事务结局 %q", rec.RunID, outcome)
	}
	return token, nil
}

// insertRunManifestTx 在 StartRun 的事务里写资源清单、三类资源引用、凭据固定与通知
// 冻结（约束 18 / 24）：四者与 Run 行同生共死，任一失败整笔回滚。
// insertRunManifestTx writes the manifest, refs, credential pin and notify freeze in
// the same transaction as the run row.
func insertRunManifestTx(ctx context.Context, tx *sql.Tx, rec core.RunRecord,
	manifest core.RunResourceManifest, hash string, snapshot []byte, refs []manifestRef,
) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run_resource_manifests
		   (run_id, format_version, manifest_hash, agent_lifecycle_state, snapshot_json, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.RunID, manifest.FormatVersion, hash, manifest.AgentLifecycle, string(snapshot),
		rec.StartedAt.UnixMilli()); err != nil {
		return fmt.Errorf("预写运行资源清单（run_id=%s）: %w", rec.RunID, err)
	}
	for _, ref := range refs {
		// `resource_id` 是名称快照，`resource_uid` 是身份：引用闸门与"同名重建后旧
		// 引用不误伤新实例"都只认后者，两者一起落库才能事后核对当时的名称。
		// The name is a snapshot; the uid is the identity the delete gate trusts.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO run_resource_refs
			   (run_id, resource_kind, resource_id, resource_uid, resource_name,
			    version_id, content_hash, released_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, NULL)`,
			rec.RunID, string(ref.kind), ref.ref.Name, string(ref.ref.ResourceUID),
			ref.ref.Name, ref.ref.VersionID, ref.ref.ContentHash); err != nil {
			return fmt.Errorf("预写运行资源引用（run_id=%s, %s/%s）: %w",
				rec.RunID, ref.kind, ref.ref.Name, err)
		}
	}
	// 凭据版本与通知渠道集合在**同一事务**里冻结：Run 行、版本引用与"用哪版 Key"
	// 若分两次提交，崩在中间就会出现"有 Run 没固定凭据"的账。
	// Credential pins and the notify channel set are frozen in the same transaction.
	if pin, ok := manifestCredentialPin(rec.RunID, manifest); ok {
		if err := insertRunCredentialRefTx(ctx, tx, pin); err != nil {
			return fmt.Errorf("预写运行凭据固定（run_id=%s）: %w", rec.RunID, err)
		}
	}
	for _, ref := range manifestNotifyRefs(rec.RunID, manifest) {
		if err := insertRunNotifyRefTx(ctx, tx, ref); err != nil {
			return fmt.Errorf("预写运行通知冻结（run_id=%s）: %w", rec.RunID, err)
		}
	}
	return nil
}

// insertRunLeaseTx 在 StartRun 的同一事务里创建 run 租约（epoch = 1，记 `claimed`）。
// 与 `LeaseStore.Acquire` 的首次领取同形，但**不带 ON CONFLICT**：Run 行刚插入，
// 同一 `(kind, resource_id)` 不可能已有租约；冲突说明 run_id 复用或装配缺陷，
// 必须整笔回滚，而不是悄悄换成"接管"语义。
// insertRunLeaseTx creates the run lease in the admission transaction; a conflict is
// an assembly defect, never a silent takeover.
func insertRunLeaseTx(ctx context.Context, tx *sql.Tx, runID string, owner core.WorkerIdentity,
	ttl time.Duration,
) (core.LeaseToken, error) {
	ttlMS, err := validateLeaseTTL(ttl)
	if err != nil {
		return core.LeaseToken{}, err
	}
	// 时间列取同一条语句里的同一个数据库时钟读数（`clock_timestamp()` 是 volatile，
	// 一条语句里多次调用可能跨毫秒），避免 acquired_at 与 expires_at 落在两个时刻。
	var nowMS int64
	if err := tx.QueryRowContext(ctx, `
INSERT INTO execution_leases
  (kind, resource_id, owner_worker_id, ownership_epoch, acquired_at, renewed_at, expires_at)
SELECT $1, $2, $3, 1, ms, ms, ms + $4
  FROM (SELECT `+sqlNowMillis+` AS ms) AS now
RETURNING acquired_at`,
		string(core.LeaseRun), runID, owner.InstanceID, ttlMS,
	).Scan(&nowMS); err != nil {
		return core.LeaseToken{}, fmt.Errorf("创建运行租约（run_id=%s）: %w", runID, err)
	}
	if err := appendLeaseEvent(ctx, tx, core.LeaseRun, runID, "claimed", owner.InstanceID, 1,
		"", sql.NullInt64{}, "", nowMS); err != nil {
		return core.LeaseToken{}, err
	}
	return core.LeaseToken{
		Kind:           core.LeaseRun,
		ResourceID:     runID,
		OwnerWorkerID:  owner.InstanceID,
		OwnershipEpoch: 1,
	}, nil
}

// callerAdvisoryKey / sessionAdvisoryKey 是准入与 fenced 写面共用的 advisory 锁键：
// 同一把锁必须由同一个种子派生，否则"会话串行"只是两个进程各锁各的。
// The caller/session advisory keys are derived from one seed so every writer locks
// the very same key.
func callerAdvisoryKey(callerID string) int64 { return advisoryKey("basalt/caller/" + callerID) }

func sessionAdvisoryKey(sessionID string) int64 { return advisoryKey("basalt/session/" + sessionID) }

// admitPublishedResources 是 StartRun 事务内的发布 / 生命周期最终复查（§2.3.9）：
// 事务内**只读数据库**（不做文件 I/O），任一项不符即 fail-closed。
// admitPublishedResources re-reads the shared publication records and the lifecycle
// index inside the admission transaction; every arm fails closed.
func admitPublishedResources(ctx context.Context, tx *sql.Tx, rec core.RunRecord,
	manifest core.RunResourceManifest,
) error {
	if err := recheckAgentBundle(ctx, tx, manifest); err != nil {
		return err
	}
	if err := recheckResourceVersions(ctx, tx, manifest); err != nil {
		return err
	}
	if err := recheckProviderCredentialPin(ctx, tx, manifest); err != nil {
		return err
	}
	return recheckAgentLifecycle(ctx, tx, rec)
}

// recheckAgentBundle 比对 `agent_bundle` 发布指针与清单固定的 bundle 哈希：
// 指针缺失或已软删除 → `ErrAssetUnavailable`（503）；哈希不等 → `ErrAssetVersionMismatch`
// （409）。这是"哪个 AgentUID 对应哪份已发布配置"的唯一持久权威（R4）。
// recheckAgentBundle compares the agent_bundle pointer with the manifest's hash.
func recheckAgentBundle(ctx context.Context, tx *sql.Tx, manifest core.RunResourceManifest) error {
	var hash string
	err := tx.QueryRowContext(ctx, `
		SELECT v.content_hash
		  FROM resource_pointers p
		  JOIN resource_versions v ON v.id = p.published_version_id
		 WHERE p.resource_kind = $1 AND p.resource_id = $2 AND p.deleted_at IS NULL`,
		kindAgentBundle, string(manifest.AgentUID)).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: Agent %s 的共享发布记录缺失或已删除",
			core.ErrAssetUnavailable, manifest.AgentUID)
	}
	if err != nil {
		return fmt.Errorf("读取 Agent bundle 发布记录（uid=%s）: %w", manifest.AgentUID, err)
	}
	if hash != manifest.AssetBundleHash {
		return fmt.Errorf("%w: 共享发布记录 %s 与准入清单 %s 不一致",
			core.ErrAssetVersionMismatch, hash, manifest.AssetBundleHash)
	}
	return nil
}

// recheckResourceVersions 重读三类资源指针并比对清单冻结的版本号（§2.3.9 的
// "事务内再复查版本未漂移"）：Provider / 技能比 `published_version_id`，MCP 比
// `applied_version_id`（运行必须用进程里真正连接着的那个版本，D-12）。
// 指针行在预检之后被删除或版本被推进 → `ErrResourceVersionMismatch`（409，可重试）：
// 预检刚刚读过它，此刻消失是竞态而不是永久缺失，**不得**"以最新为准"静默换版本。
// recheckResourceVersions re-reads the three pointer kinds and compares the frozen
// version ids; drift is a retryable 409, never a silent upgrade.
func recheckResourceVersions(ctx context.Context, tx *sql.Tx, manifest core.RunResourceManifest) error {
	for _, ref := range manifestRefs(manifest) {
		var (
			versionID int64
			err       error
		)
		switch ref.kind {
		case core.ResourceMCPServer:
			err = tx.QueryRowContext(ctx, `
				SELECT COALESCE(applied_version_id, 0)
				  FROM resource_pointers
				 WHERE resource_kind = $1 AND resource_id = $2 AND deleted_at IS NULL`,
				string(ref.kind), ref.ref.Name).Scan(&versionID)
		default:
			err = tx.QueryRowContext(ctx, `
				SELECT published_version_id
				  FROM resource_pointers
				 WHERE resource_kind = $1 AND resource_id = $2 AND deleted_at IS NULL`,
				string(ref.kind), ref.ref.Name).Scan(&versionID)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 资源 %s/%s 的发布指针在预检之后消失或已删除",
				core.ErrResourceVersionMismatch, ref.kind, ref.ref.Name)
		}
		if err != nil {
			return fmt.Errorf("重读资源指针（%s/%s）: %w", ref.kind, ref.ref.Name, err)
		}
		if versionID != ref.ref.VersionID {
			return fmt.Errorf("%w: 资源 %s/%s 的指针版本 %d 与准入清单 %d 不一致",
				core.ErrResourceVersionMismatch, ref.kind, ref.ref.Name, versionID, ref.ref.VersionID)
		}
	}
	return nil
}

// recheckProviderCredentialPin 在事务内重读托管凭据的**当前版本**并与清单冻结的 pin
// 逐项比对（P-21 第三条纪律 / FR-24-25）：凭据轮换走的是"同一发布版本重发布"
// （`bumpRevision` 只推进 revision 与 operation，版本行与内容哈希一个字不改），
// `recheckResourceVersions` 的版本号比对**看不见**这次变化——只有本项能挡住
// "清单记 C2、手里还是 C1"。env 模式没有凭据身份（`CredentialID` 为空），本项空过。
//
// 归属、双向状态与目标绑定一并复查：只比版本号会让"凭据被换绑到另一个模型连接、
// 或被撤销 / 停用"仍算 pin 未漂移。任一不符 → `ErrResourceVersionMismatch`：
// 调用方重试即可，**不得**静默换凭据继续跑。
// recheckProviderCredentialPin re-reads the managed credential's current version and
// compares it field by field with the pin frozen in the manifest; rotating a credential
// republishes the same version, so the version-id recheck alone cannot see it.
func recheckProviderCredentialPin(ctx context.Context, tx *sql.Tx,
	manifest core.RunResourceManifest,
) error {
	pin := manifest.Provider
	if pin.CredentialID == "" {
		return nil
	}
	var (
		ownerKind    string
		ownerUID     string
		credState    string
		current      int64
		versionState string
		target       string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT c.owner_kind, c.owner_uid, c.state, c.current_version, v.state, v.target_binding
		  FROM credentials c
		  JOIN credential_versions v
		    ON v.credential_id = c.credential_id AND v.version_no = c.current_version
		 WHERE c.credential_id = $1`, pin.CredentialID).
		Scan(&ownerKind, &ownerUID, &credState, &current, &versionState, &target)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 凭据 %s 在预检之后消失或已无当前版本行",
			core.ErrResourceVersionMismatch, pin.CredentialID)
	}
	if err != nil {
		return fmt.Errorf("重读凭据 %s: %w", pin.CredentialID, err)
	}
	if ownerKind != string(core.BindingModelConnection) || ownerUID != string(pin.ResourceUID) {
		return fmt.Errorf("%w: 凭据 %s 的归属 %s/%s 与模型连接 %s/%s 不符",
			core.ErrResourceVersionMismatch, pin.CredentialID,
			ownerKind, ownerUID, core.BindingModelConnection, pin.ResourceUID)
	}
	if credState != "active" || versionState != "active" {
		return fmt.Errorf("%w: 凭据 %s 在准入窗口内状态变为 %s/%s",
			core.ErrResourceVersionMismatch, pin.CredentialID, credState, versionState)
	}
	if current != int64(pin.CredentialVersion) || target != pin.TargetBinding {
		return fmt.Errorf("%w: 凭据 %s 的当前版本 %d(@%s) 与准入固定的 %d(@%s) 不一致",
			core.ErrResourceVersionMismatch, pin.CredentialID,
			current, target, pin.CredentialVersion, pin.TargetBinding)
	}
	return nil
}

// recheckAgentLifecycle 在事务内锁定生命周期索引行（与归档屏障是同一行 FOR UPDATE
// 互斥），逐项比对代次与状态文件哈希（P-16 / FR-24-41 / FR-24-42）：
//   - 索引代次高于期望 → `ErrAgentLifecycleStale`（另一个 Worker 已推进过）；
//   - 索引代次低于期望 → `ErrAgentLifecycleMismatch`（**不**自动放行，需管理操作
//     单调修复）；
//   - 同代次但 state 非 active / 哈希不符 / 无行 → `ErrAgentLifecycleMismatch`。
//
// 零值期望直接拒绝：P-16 要求准入比对期望值，缺期望不是"跳过复查"。
// recheckAgentLifecycle locks the lifecycle index row and compares generation and
// state-file hash; a zero expectation is refused rather than skipped.
func recheckAgentLifecycle(ctx context.Context, tx *sql.Tx, rec core.RunRecord) error {
	if rec.Lifecycle == (core.LifecycleExpectation{}) {
		return fmt.Errorf("%w: Run %s 未携带生命周期期望值（P-16）",
			core.ErrAgentLifecycleMismatch, rec.RunID)
	}
	var (
		generation int64
		state      sql.NullString
		stateHash  sql.NullString
	)
	err := tx.QueryRowContext(ctx, `
		SELECT generation, state, state_file_hash
		  FROM agent_lifecycle
		 WHERE agent_name = $1
		   FOR UPDATE`, rec.Agent).Scan(&generation, &state, &stateHash)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: Agent %q 未登记在共享生命周期索引中",
			core.ErrAgentLifecycleMismatch, rec.Agent)
	}
	if err != nil {
		return fmt.Errorf("锁定生命周期索引行（agent=%s）: %w", rec.Agent, err)
	}
	if generation > rec.Lifecycle.Generation {
		return fmt.Errorf("%w: 共享索引代次 %d 高于本地期望 %d",
			core.ErrAgentLifecycleStale, generation, rec.Lifecycle.Generation)
	}
	if generation < rec.Lifecycle.Generation {
		return fmt.Errorf("%w: 本地代次 %d 高于共享索引 %d——需管理操作单调修复",
			core.ErrAgentLifecycleMismatch, rec.Lifecycle.Generation, generation)
	}
	if state.String != string(core.AgentActive) || stateHash.String != rec.Lifecycle.StateFileHash {
		return fmt.Errorf("%w: 同代次 %d 的状态不一致（state=%q）",
			core.ErrAgentLifecycleMismatch, generation, state.String)
	}
	return nil
}

// CheckAdmission 是准入的**只读预检**（图 2 的 [4]）：调用方并发计数在前、同会话
// 活跃 run 租约在后——顺序与 openapi 的错误优先级一致（429 CALLER_CONCURRENCY_LIMITED
// → 409 SESSION_BUSY）。本方法**不写任何行**、不产生任何占用；结论可能被 `StartRun`
// 事务内的权威复查推翻（那是唯一定论，这里只负责"早失败"）。
// CheckAdmission is the read-only precheck; StartRun's transaction is the only authority.
//
// 两段判据与既有 `callers.Limiter` 同口径（模块卡 §2.3.2 的并发语义）：计的是
// `runs.status='running'` 的行，**不**把 running Run 与 Flow 任务相加；第二条限制
// **只对 Flow 请求**生效（`IsFlow`），非 Flow 请求判了它就会让交互预留反过来卡死交互。
// The two limits mirror callers.Limiter: total running runs, and the flow-only
// reserved-slot check gated on IsFlow.
//
// 实例容量（`TOO_MANY_RUNS`）**不**在这里判：它按每个 Worker 的进程内计数，数据库里
// 没有"本 Worker 在跑几条"的可读事实（runs 表没有 worker 列），预检由装配层的内存
// 名额表承担（T013）。
// The per-worker instance cap stays in the assembly's in-memory table: runs carry no
// worker column, so this query cannot see it.
func (s *RunStore) CheckAdmission(ctx context.Context, sessionID string, p core.Principal,
	admission core.CallerAdmission,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// 身份不合法时**拒绝**，不回落成"不限量"——与 limiter 的 `!p.Valid()` 同一口径。
	// An invalid principal is refused rather than treated as unlimited.
	if !p.Valid() {
		return core.ErrUnauthorized
	}
	if admission.Enabled {
		if err := checkCallerConcurrency(ctx, s.db.pool, p.CallerID, admission); err != nil {
			return err
		}
	}
	busy, err := sessionHasLiveRunLease(ctx, s.db.pool, sessionID)
	if err != nil {
		return err
	}
	if busy {
		return &core.SessionBusyError{SessionID: sessionID}
	}
	return nil
}

// checkCallerConcurrency 数在跑的 Run 并与调用方额度比较；查询失败原样上抛（fail-closed，
// **不**降级成放行）。额度 <= 0 一律拒绝：既有内存名额表对"缺失 / 零限额"就是拒绝全部，
// 预检不得比权威判定更宽松。
//
// 接收 `Queryer` 而不是绑定连接池：同一个判据既是只读预检，也是 `StartRun` 事务内的
// 权威复查（`*sql.Tx`）——两份同义 SQL 就是第二处漂移点。
// checkCallerConcurrency takes a Queryer so the precheck and the authoritative in-tx
// recheck run the very same statement.
func checkCallerConcurrency(ctx context.Context, q Queryer, callerID string,
	admission core.CallerAdmission,
) error {
	var total, flow int
	if err := q.QueryRowContext(ctx,
		`SELECT count(*), count(*) FILTER (WHERE flow_task_id IS NOT NULL)
		   FROM runs WHERE caller_id = $1 AND status = $2`,
		callerID, string(core.StatusRunning)).Scan(&total, &flow); err != nil {
		return fmt.Errorf("读取调用方并发计数（caller=%s）: %w", callerID, err)
	}
	if admission.MaxConcurrent <= 0 {
		return core.ErrCallerConcurrencyLimited
	}
	// 判定顺序与 `callers.Limiter.acquire` 的 `flow && (…)` 门控逐字同形：
	// 非 Flow 请求只判总数；交互预留为 0 时第二条恒等于第一条，不写特例。
	if total >= admission.MaxConcurrent {
		return core.ErrCallerConcurrencyLimited
	}
	if admission.IsFlow {
		reserved := admission.ReservedForInteractive
		if reserved >= admission.MaxConcurrent || flow >= admission.MaxConcurrent-reserved {
			return core.ErrCallerConcurrencyLimited
		}
	}
	return nil
}

// sessionHasLiveRunLease 判定同会话是否已有**未释放**的 run，且该 Run 仍在进行：
// 租约未过期（`expires_at > now`）**或**行仍是 `running`。
//
// 两侧判据各挡一种窗口（FR-24-07 / FR-24-43，§4.4.2 三段断言）：
//   - 行已终态、租约尚未释放：终态提交不等于收尾，引用与租约留在
//     `CompleteRunCloseout` 之前（FR-24-06），此时新 Run 必须仍被拒绝；
//   - 行仍 running、租约已过期：崩溃 Worker 留下的行由**接管恢复**收尾，恢复事务持
//     同一把会话 advisory 锁；在它提交之前新 Run 不得插队（FR-24-43）。
//
// 预检与事务内复查共用本判据（同一 `Queryer` 形态），结论可能被事务内的那次推翻——
// 事务内那次才是权威。
// The session is busy while an unreleased run lease exists AND either the lease is
// still live or the row is still running: terminal-but-not-closed and
// running-with-expired-lease both block until recovery/closeout completes.
func sessionHasLiveRunLease(ctx context.Context, q Queryer, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, nil
	}
	var busy bool
	if err := q.QueryRowContext(ctx,
		`SELECT EXISTS (
		   SELECT 1
		     FROM runs r
		     JOIN execution_leases l
		       ON l.kind = $1 AND l.resource_id = r.run_id
		    WHERE r.session_id = $2
		      AND l.released_at IS NULL
		      AND (r.status = $3 OR l.expires_at > `+sqlNowMillis+`))`,
		string(core.LeaseRun), sessionID, string(core.StatusRunning)).Scan(&busy); err != nil {
		return false, fmt.Errorf("读取会话活跃租约（session=%s）: %w", sessionID, err)
	}
	return busy, nil
}

// LookupAdmittedRun 是"提交结果未知"之后的唯一回查面：按 runID 读 **Run 行与其
// run 租约**。查得到就返回该租约的 token（调用方按成功继续派生 keepLease），查不到
// 返回 `core.ErrRunNotFound`——**不**退款、**不**重试（FR-24-33）。
//
// 判据取 **Run 行**而不是只看租约（模块卡 §2.3.2 [6]）：Run 行、已结算预留与租约
// 同在 `StartRun` 一个事务里落库，因此"Run 行可见"就是"准入已提交"的权威证据；
// 只看租约会把零 owner 装配（不建租约）误判成未准入。租约在这里只承担"交还执行权"
// 的职责——没有租约（零 owner 装配）时返回零 token，行为与未接线租约时逐字一致。
//
// 反向不成立：**查无行不是"未提交"的证据**——原 COMMIT 可能仍在服务端执行（C-34），
// 调用方只能按失败关闭处理（不退款、不重试），不得据此判定未落。
// The visible run row is the authoritative admission proof; the lease only carries the
// execution right. A missing row never proves a rollback (C-34).
func (s *RunStore) LookupAdmittedRun(ctx context.Context, runID string) (core.LeaseToken, error) {
	var owner sql.NullString
	var epoch sql.NullInt64
	err := s.db.pool.QueryRowContext(ctx, `
SELECT l.owner_worker_id, l.ownership_epoch
  FROM runs r
  LEFT JOIN execution_leases l
         ON l.kind = $1 AND l.resource_id = r.run_id
 WHERE r.run_id = $2`,
		string(core.LeaseRun), runID).Scan(&owner, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return core.LeaseToken{}, core.ErrRunNotFound
	}
	if err != nil {
		return core.LeaseToken{}, fmt.Errorf("回查准入结果（run_id=%s）: %w", runID, err)
	}
	if !owner.Valid {
		// 零 owner 装配不建租约：Run 行可见即已准入，但没有执行权可交还。
		// A zero-owner assembly creates no lease; admission is still proven by the row.
		return core.LeaseToken{}, nil
	}
	return core.LeaseToken{
		Kind:           core.LeaseRun,
		ResourceID:     runID,
		OwnerWorkerID:  owner.String,
		OwnershipEpoch: epoch.Int64,
	}, nil
}

// RequestCancel 写 `runs.cancel_requested`：非属主只写标志、**不**代替属主提交终态，
// 返回写后的真实状态（可能仍是 running——**不得**假装取消成功，FR-24-22）。
// 行不存在 → `core.ErrRunNotFound`（web 映射 404）；已终态 → 原样返回（幂等：只更新
// 仍在 running 的行，已判定的终态不被这次写入改口）。
// RequestCancel only raises the flag and reports the real state; a terminal run is
// returned untouched, and a missing row is ErrRunNotFound.
func (s *RunStore) RequestCancel(ctx context.Context, runID string) (core.RunDetail, error) {
	if _, err := s.db.pool.ExecContext(ctx,
		`UPDATE runs SET cancel_requested = true
		  WHERE run_id = $1 AND status = $2 AND NOT cancel_requested`,
		runID, string(core.StatusRunning)); err != nil {
		return core.RunDetail{}, fmt.Errorf("请求取消运行（run_id=%s）: %w", runID, err)
	}
	// 读回真实状态：UPDATE 命中 0 行既可能是"已终态"也可能是"行不存在"，只有读一次
	// 才能把两者分开——前者是幂等成功，后者是 404。
	// Zero updated rows is either "already terminal" (idempotent) or "missing" (404);
	// only the read-back tells them apart.
	return s.GetRun(ctx, runID)
}

// validateManifestCredentialPin 校验清单里的凭据固定形状：托管模式三项齐备且合法，
// env 模式三项全空。混合形态（有 id 没版本、有版本没 id）一律拒绝——它会让冻结行
// 与清单各说一半。
// validateManifestCredentialPin checks that the pin is coherent before it is frozen.
func validateManifestCredentialPin(provider core.ProviderVersionRef) error {
	// 目标绑定两种模式**都要**：它记的是规范化目标地址的哈希，是运行期复核的锚点，
	// 不是凭据材料——env 模式同样有地址。
	// The target binding is required in both modes; it hashes the target address, it is
	// not credential material.
	if !isSHA256Hex(provider.TargetBinding) {
		return fmt.Errorf("资源清单的 %s 目标绑定哈希非法：必须是 64 位小写十六进制", provider.Name)
	}
	if provider.CredentialID == "" {
		if provider.CredentialVersion != 0 {
			return fmt.Errorf("资源清单的 %s 凭据固定不完整：env 模式不得带凭据版本", provider.Name)
		}
		return nil
	}
	if err := core.ValidateCredentialID(provider.CredentialID); err != nil {
		return fmt.Errorf("资源清单的 %s 凭据引用非法: %w", provider.Name, err)
	}
	if provider.CredentialVersion < 1 {
		return fmt.Errorf("资源清单的 %s 凭据版本非法（%d）", provider.Name, provider.CredentialVersion)
	}
	return nil
}

// sha256HexSize 是小写十六进制 sha256 的长度（与 core 的 `target_binding` 同形）。
// sha256HexSize is the length of a lowercase hex sha256, matching core's target binding.
const sha256HexSize = 64

// isSHA256Hex 判定 64 位小写十六进制（sha256 的十六进制形态）。
// isSHA256Hex reports whether the value is a 64-char lowercase hex digest.
func isSHA256Hex(value string) bool {
	if len(value) != sha256HexSize {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// manifestCredentialPin 把清单里的托管凭据固定摊平成 `run_credential_refs` 行；
// env 模式（`CredentialID` 为空）**不写行**——env 没有凭据身份，凭空造一个 id 只会
// 让审计里出现查不到的引用。
// manifestCredentialPin yields the pin row, or ok=false for env mode.
func manifestCredentialPin(runID string, manifest core.RunResourceManifest) (core.RunCredentialRef, bool) {
	provider := manifest.Provider
	if provider.CredentialID == "" {
		return core.RunCredentialRef{}, false
	}
	return core.RunCredentialRef{
		RunID:             runID,
		CredentialID:      provider.CredentialID,
		CredentialVersion: provider.CredentialVersion,
		ResourceKind:      core.BindingModelConnection,
		ResourceUID:       provider.ResourceUID,
		ResourceName:      provider.Name,
	}, true
}

// manifestNotifyRefs 把清单里冻结的渠道集合摊平成 `run_notify_refs` 行。
// manifestNotifyRefs flattens the frozen channel set into notify-ref rows.
func manifestNotifyRefs(runID string, manifest core.RunResourceManifest) []core.RunNotifyRef {
	refs := make([]core.RunNotifyRef, 0, len(manifest.Notify.Channels))
	for _, channel := range manifest.Notify.Channels {
		refs = append(refs, core.RunNotifyRef{
			RunID:       runID,
			AgentUID:    manifest.AgentUID,
			ChannelUID:  channel.ResourceUID,
			ChannelName: channel.Name,
		})
	}
	return refs
}

// FinishRun 把终态写回同一行（status / reason / iterations / tool_calls / finished_at），
// 并报告**本次调用是否真正完成了状态转换**（US-22：抽取钩子的唯一触发判据）。
// 引用释放**不在**本方法内：终态 → 抽取 → 释放的顺序由 `core.AgentService.finish` 编排，
// 钩子要读的运行期客户端依赖引用仍然有效（C-05）。
// FinishRun writes the terminal state and reports whether this call applied it. Reference
// release moved out: the core service orders terminal → hook → release so the hook still
// sees live refs.
//
// 两条守卫。① `WHERE status = 'running'`：取消路径只发信号、崩溃恢复与超时兜底都可能
// 并发调用本方法；已判定的终态**不得**被迟到的写入改口（注 8）。② 影响 0 行时读当前行：
// 行存在 = 幂等未转换（返回 false，不覆盖）；行不存在 = **如实报错**，账本行缺失说明
// 启动纪律被绕过，不该被"更新成功"掩盖。
//
// **归属 fencing（US-24 FR-24-06 / FR-24-36）**：token 非零时整笔经 `fencedTx` 提交
// ——先锁租约行再校验 owner + epoch + 未释放 + 未过期；不符返回 `core.ErrLeaseLost`，
// 且**一行都不写**。只写终态列：引用、投递与租约留给 `CompleteRunCloseout`。
// 零值 token 走无 fencing 的纯条件更新——SQLite 谱系、轻装配与零值单测没有租约，
// 这不是"可以用零值绕过 fence"：装配一旦接线，调用方拿到的 token 就非零。
// The status guard keeps a late terminal write from re-judging a finished run; a zero-row
// update is an idempotent no-op when the row exists, and a hard error when it does not.
// A non-zero token fences the whole write on owner + epoch + not-expired; only terminal
// columns are written.
func (s *RunStore) FinishRun(ctx context.Context, runID string, token core.LeaseToken,
	status core.RunStatus, reason string, iterations, toolCalls int, now time.Time,
) (bool, error) {
	if token.IsZero() {
		var applied bool
		_, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			applied, err = applyTerminalWrite(ctx, tx, runID, status, reason, iterations, toolCalls, now)
			return err
		})
		if err != nil {
			return false, err
		}
		return applied, nil
	}

	var applied bool
	err := fencedTx(ctx, s.db, token, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		applied, err = applyTerminalWrite(ctx, tx, runID, status, reason, iterations, toolCalls, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return applied, nil
}

// applyTerminalWrite 是两条路径共用的**唯一终态写入**：条件更新 + 0 行分类。
// 事务边界由调用方保证（无 fencing / fencedTx 二选一），写入内容两路径逐字相同。
// applyTerminalWrite is the single terminal-column write both paths share, so the
// fenced and unfenced branches cannot drift.
func applyTerminalWrite(ctx context.Context, ex sqlExecutor, runID string, status core.RunStatus,
	reason string, iterations, toolCalls int, now time.Time,
) (bool, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE runs
		    SET status = $1, reason = $2, iterations = $3, tool_calls = $4, finished_at = $5
		  WHERE run_id = $6 AND status = $7`,
		string(status), nullIfEmpty(reason), iterations, toolCalls, now.UnixMilli(),
		runID, string(core.StatusRunning))
	if err != nil {
		return false, fmt.Errorf("回填运行终态（run_id=%s）: %w", runID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("确认运行终态写入（run_id=%s）: %w", runID, err)
	}
	if affected == 0 {
		// 0 行有两种含义：已终态（幂等，applied=false）与账本行缺失（装配缺陷，
		// 如实报错）。失权已经在 fence 里判掉，走不到这里。
		if err := requireRunRow(ctx, ex, runID); err != nil {
			return false, err
		}
		return false, nil
	}
	return true, nil
}

// CompleteRunCloseout 是 Run 收尾的**最后一笔**（US-24 五步收尾的第 5 步）：释放三类
// 引用（幂等）并**最后**释放 run 租约，一个事务提交。调用时机固定在学习钩子与投递
// 都结束之后——顺序由 `core.AgentService.finish` / 调度器保证，本方法不猜。
// CompleteRunCloseout releases refs and last the run lease in one transaction; the
// caller owns the five-step order.
//
// token 非零时经 `fencedTx` 提交（owner + epoch + 未释放 + 未过期）；失权返回
// `core.ErrLeaseLost`。**终态未持久化**（`FinishRun` 失败或未提交）时不释放任何东西
// 并返回 nil：引用是钩子与恢复的输入，租约留给回收器按"已过期"接管——与既有
// `ReleaseRunRefs` 的守卫同一口径。
// 重放幂等：租约已由同一 owner + epoch 释放（上次提交结果未知的重试）→ nil，
// 与 `LeaseStore.Release` 同口径。
// A non-zero token fences the closeout; an unpersisted terminal state releases nothing
// and leaves the lease to the reclaimer. A same-owner replay is a no-op.
func (s *RunStore) CompleteRunCloseout(ctx context.Context, runID string, token core.LeaseToken,
	now time.Time,
) error {
	if token.IsZero() {
		return s.releaseRunRefsTx(ctx, runID, now)
	}

	err := fencedTx(ctx, s.db, token, func(ctx context.Context, tx *sql.Tx) error {
		terminal, err := runTerminal(ctx, tx, runID)
		if err != nil {
			return err
		}
		if !terminal {
			return nil
		}
		if err := releaseAllRunRefs(ctx, tx, runID, now); err != nil {
			return err
		}
		// 租约释放与引用释放同一事务：要么"引用已释放且租约已交还"，要么都不是。
		// 释放时间取数据库时钟，与续约 / 回收的判据同源。
		nowMS, err := readDBMillis(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
UPDATE execution_leases
   SET released_at = $5
 WHERE kind = $1 AND resource_id = $2 AND owner_worker_id = $3 AND ownership_epoch = $4
   AND released_at IS NULL`,
			string(token.Kind), token.ResourceID, token.OwnerWorkerID, token.OwnershipEpoch, nowMS)
		if err != nil {
			return fmt.Errorf("释放运行租约（run_id=%s）: %w", runID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("确认运行租约释放（run_id=%s）: %w", runID, err)
		}
		if affected != 1 {
			// fence 已确认行属于本人且未释放；走到这里说明并发写坏了行锁前提，
			// 按失权处理（整笔回滚，不提交半截收尾）。
			return core.ErrLeaseLost
		}
		return appendLeaseEvent(ctx, tx, token.Kind, token.ResourceID, "released",
			token.OwnerWorkerID, token.OwnershipEpoch, "", sql.NullInt64{}, "", nowMS)
	})
	if errors.Is(err, core.ErrLeaseLost) && s.closeoutAlreadyDone(ctx, token) {
		// 重放：上一次提交已落库（引用与租约都已处理），本次无写入可做。
		return nil
	}
	return err
}

// releaseRunRefsTx 是零值 token 路径：只释放三类引用，没有租约可交还。
// 没有租约的装配里"收尾"就是释放引用本身。
// releaseRunRefsTx is the zero-token path: refs only, no lease to hand back.
func (s *RunStore) releaseRunRefsTx(ctx context.Context, runID string, now time.Time) error {
	_, err := s.db.Tx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		terminal, err := runTerminal(ctx, tx, runID)
		if err != nil {
			return err
		}
		if !terminal {
			return nil
		}
		return releaseAllRunRefs(ctx, tx, runID, now)
	})
	return err
}

// closeoutAlreadyDone 判定 `ErrLeaseLost` 是否只是"同一 owner + epoch 已释放"的重放。
// 只读一次：fence 已证明归属不成立时才走这里，不构成正常路径的第二条判据。
// closeoutAlreadyDone recognizes a replay of an already-released same-owner lease.
func (s *RunStore) closeoutAlreadyDone(ctx context.Context, token core.LeaseToken) bool {
	var row leaseRow
	err := s.db.pool.QueryRowContext(ctx, `
SELECT owner_worker_id, ownership_epoch, expires_at, released_at
  FROM execution_leases
 WHERE kind = $1 AND resource_id = $2`,
		string(token.Kind), token.ResourceID).
		Scan(&row.Owner, &row.Epoch, &row.Expires, &row.Released)
	if err != nil {
		return false
	}
	return row.Owner == token.OwnerWorkerID && row.Epoch == token.OwnershipEpoch && row.Released.Valid
}

// releaseAllRunRefs 把三类引用释放放进调用方的事务（幂等：只更新未释放的行）。
// releaseAllRunRefs releases the three reference kinds inside the caller's transaction.
func releaseAllRunRefs(ctx context.Context, ex sqlExecutor, runID string, now time.Time) error {
	if _, err := releaseRunRefs(ctx, ex, runID, now); err != nil {
		return err
	}
	if _, err := releaseRunCredentialRefs(ctx, ex, runID, now); err != nil {
		return err
	}
	if _, err := releaseRunNotifyRefs(ctx, ex, runID, now); err != nil {
		return err
	}
	return nil
}

// ReleaseRunRefs 释放该 Run 的资源 / 凭据 / 通知三类引用（幂等：只更新未释放的行）。
// 在终态落库与抽取钩子之后由 `core.AgentService.finish` **无条件**调用；失败只上抛，
// 由调用方记 ERROR，已终态未释放的行由下次启动的 `releaseTerminalRefs` 补齐。
// **释放前先确认终态已持久化**（`status <> 'running'`）：`FinishRun` 写失败或尚未提交时
// 引用不释放，留给启动恢复——否则钩子还没跑凭据就被回收，抽取必失败（C-05）。
// ReleaseRunRefs releases the three reference kinds; idempotent, called unconditionally
// after the terminal write and the extraction hook. It refuses to release while the run
// is still `running` (FinishRun failed or not yet committed): those refs are left to
// startup recovery. Startup recovery closes any leftovers.
func (s *RunStore) ReleaseRunRefs(ctx context.Context, runID string, now time.Time) error {
	tx, err := s.db.pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("释放运行引用（run_id=%s）: 开启事务: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()

	terminal, err := runTerminal(ctx, tx, runID)
	if err != nil {
		return err
	}
	if !terminal {
		return nil
	}

	if err := releaseAllRunRefs(ctx, tx, runID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("释放运行引用（run_id=%s）: 提交: %w", runID, err)
	}
	return nil
}

// releaseRunRefs 释放某个 Run 的全部未释放资源引用（约束 19 / 注 8）。
// **事务边界由调用方保证**：`ReleaseRunRefs` 把三类释放放进同一事务提交。
// releaseRunRefs stamps released_at on the run's unreleased resource refs; the caller
// owns the transaction that commits the three kinds together.
func releaseRunRefs(ctx context.Context, ex sqlExecutor, runID string, now time.Time) (int, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE run_resource_refs
		    SET released_at = $1
		  WHERE run_id = $2 AND released_at IS NULL`,
		now.UnixMilli(), runID)
	if err != nil {
		return 0, fmt.Errorf("释放运行资源引用（run_id=%s）: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("确认运行资源引用释放（run_id=%s）: %w", runID, err)
	}
	return int(affected), nil
}

// requireRunRow 确认账本行存在（FinishRun 的 0 行分支：区分"已终态"与"没有这一行"）。
// requireRunRow distinguishes "already terminal" (idempotent success) from a missing row.
func requireRunRow(ctx context.Context, q Queryer, runID string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE run_id = $1`, runID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("回填运行终态（run_id=%s）: 账本行不存在", runID)
	}
	if err != nil {
		return fmt.Errorf("读取运行账本行（run_id=%s）: %w", runID, err)
	}
	return nil
}

// runTerminal 报告该 Run 的终态是否已持久化（`status <> 'running'`）。
// `ReleaseRunRefs` 据此守卫：行仍 running（FinishRun 失败 / 未提交）或行不存在时不释放，
// 引用留给启动恢复；状态只从 running 走向终态、不会回退，读后无需再锁。
// runTerminal reports whether the terminal state is persisted (status <> 'running').
// ReleaseRunRefs uses it as the release guard: a running or missing row keeps its refs
// for startup recovery. Statuses never move back to running, so no extra locking needed.
func runTerminal(ctx context.Context, q Queryer, runID string) (bool, error) {
	var status string
	err := q.QueryRowContext(ctx, `SELECT status FROM runs WHERE run_id = $1`, runID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("读取运行终态（run_id=%s）: %w", runID, err)
	}
	return status != string(core.StatusRunning), nil
}

// runOwnership 把 `RunRecord.Principal` 派生为 runs 的五个归属列：`caller_id` /
// `authorizing_kind` / `key_id` / `reservation_id` / `actor`（设计 §3.4.2、C-28 / C-32 / C-42）。
//
// 派生是**确定性映射**，写前与 schema 的三条 CHECK 同口径校验：身份不合法、caller-key
// 缺 `key_id`、admin-builtin 带了 Key 一律拒绝——写一行归属自相矛盾的账，比拒绝启动更糟
// （读侧会把它当事实）。`reservation_id` 可空：`local-admin` 无限额度或无需预留时为 NULL。
//
// runOwnership derives the five ownership columns from the run principal and rejects any
// combination the schema CHECKs would reject, instead of letting the row contradict itself.
func runOwnership(rec core.RunRecord) (callerID, authorizingKind, keyID, reservationID, actor string, err error) {
	if !rec.Principal.Valid() {
		return "", "", "", "", "", fmt.Errorf(
			"运行身份非法（run_id=%s）：Run 必须携带有效 Principal", rec.RunID)
	}
	kind := core.AuthorizingKindAdminBuiltin
	key := ""
	if rec.Principal.Kind == core.PrincipalCaller {
		kind = core.AuthorizingKindCallerKey
		key = rec.Principal.KeyID
		if key == "" {
			return "", "", "", "", "", fmt.Errorf(
				"运行身份缺少授权 Key（run_id=%s, caller_id=%s）", rec.RunID, rec.Principal.CallerID)
		}
	}
	return rec.Principal.CallerID, string(kind), key, rec.ReservationID, string(rec.Principal.Actor), nil
}

// insertRunRow 写起始行——零清单路径与「运行行 + 清单 + 引用」事务共用的**唯一** INSERT
// 语句（两份同义 SQL 就是第二处漂移点）。
// insertRunRow is the single starting-row INSERT shared by both StartRun paths.
func insertRunRow(ctx context.Context, ex sqlExecutor, rec core.RunRecord, trigger string) error {
	callerID, authorizingKind, keyID, reservationID, actor, err := runOwnership(rec)
	if err != nil {
		return err
	}
	var taskID, nodeAttemptID, leaseOwnerID, leaseEpoch any
	var revision int64
	if rec.Flow != nil {
		if rec.Flow.TaskID == "" || rec.Flow.NodeAttemptID == "" {
			return core.ErrPreconditionFailed
		}
		// 子 Run 创建的父根前置判定（US-24 §2.3.9 / P-05）：父根租约、父根归属、
		// attempt 代次与 revision。下面的 INSERT EXISTS 仍在同一事务里兜底。
		// The parent-root pre-check runs in this same transaction; the INSERT re-checks.
		if err := createRunForNode(ctx, ex, *rec.Flow, rec.StartedAt.UnixMilli()); err != nil {
			return err
		}
		taskID, nodeAttemptID, revision = rec.Flow.TaskID, rec.Flow.NodeAttemptID, rec.Flow.TaskRevision
		leaseOwnerID, leaseEpoch = rec.Flow.Lease.OwnerWorkerID, rec.Flow.Lease.OwnershipEpoch
	}
	// 单条 INSERT 即一个短事务；reason / finished_at 留 NULL（终态才写），
	// iterations / tool_calls 取列缺省 0（还没跑，不是"跑了零次"）。
	result, err := ex.ExecContext(ctx,
		`INSERT INTO runs
		   (run_id, session_id, agent, channel, user_id, trigger, status,
		    reason, iterations, tool_calls, config_hash, started_at, finished_at,
		    caller_id, authorizing_kind, key_id, reservation_id, actor, request_id,flow_task_id,flow_node_attempt_id)
		 SELECT $1,$2,$3,$4,$5,$6,$7,NULL,0,0,$8,$9,NULL,$10,$11,$12,$13,$14,$15,$16::text,$17::text
		 WHERE ($16::text IS NULL AND $17::text IS NULL) OR EXISTS (
		   SELECT 1 FROM flow_tasks t JOIN flow_node_attempts a ON a.task_id=t.task_id
		   WHERE t.task_id=$16 AND a.node_attempt_id=$17 AND a.run_id=$1 AND a.session_id IS NOT DISTINCT FROM $2::text
		     AND a.state IN ('prepared','running') AND t.status='running' AND t.close_intent='none' AND NOT t.cancel_requested
		     AND t.caller_id=$10 AND t.authorizing_kind=$11 AND t.authorizing_key_id IS NOT DISTINCT FROM $12::text
		     AND t.current_revision=$18 AND a.task_revision=t.current_revision AND a.execution_epoch=t.execution_epoch AND t.deadline>$9
		     AND EXISTS (SELECT 1 FROM execution_leases l
		       WHERE l.kind='flow_root' AND l.resource_id=t.root_task_id
		         AND l.owner_worker_id=$19::text AND l.ownership_epoch=$20::bigint
		         AND l.released_at IS NULL AND l.expires_at>$9)
		 )`,
		rec.RunID, nullIfEmpty(rec.SessionID), rec.Agent, rec.Channel, rec.UserID,
		trigger, string(core.StatusRunning), nullIfEmpty(rec.ConfigHash), rec.StartedAt.UnixMilli(),
		callerID, authorizingKind, nullIfEmpty(keyID), nullIfEmpty(reservationID), actor, nullIfEmpty(rec.RequestID), taskID, nodeAttemptID, revision, leaseOwnerID, leaseEpoch)
	if err != nil {
		return fmt.Errorf("预写运行账本行（run_id=%s）: %w", rec.RunID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return core.ErrPreconditionFailed
	}
	return nil
}

// backfillReservationRun 把本次准入的预留挂到刚插入的 Run 上（§3.4.2「写入顺序」：
// 先 `INSERT quota_reservations (run_id = NULL)` → `INSERT runs (reservation_id = ?)`
// → **同事务** `UPDATE quota_reservations SET run_id = ?`）。回填只认本调用方的这
// 一条预留：行不存在、或已挂到别的 Run，一律失败关闭——静默留在 NULL 会让这次任务
// 消耗在管理面查不到对应 Run，账与人对不上。`run_id IS NULL` 让同事务重放（L-19
// 的"未验证清单"式重试）保持幂等。
// The admission reservation is linked to the run in the same transaction that inserts
// the ledger row; a missing or foreign reservation fails closed instead of leaving the
// charge unattributable.
func backfillReservationRun(ctx context.Context, ex sqlExecutor, rec core.RunRecord) error {
	if rec.ReservationID == "" {
		// 内置管理员不限量（NoReservation）或轻装配：没有预留行可挂。
		return nil
	}
	res, err := ex.ExecContext(ctx, `
		UPDATE quota_reservations
		   SET run_id = $1
		 WHERE reservation_id = $2 AND caller_id = $3 AND run_id IS NULL`,
		rec.RunID, rec.ReservationID, rec.Principal.CallerID)
	if err != nil {
		return fmt.Errorf("回填预留归属（run_id=%s）: %w", rec.RunID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认预留归属（run_id=%s）: %w", rec.RunID, err)
	}
	if affected == 1 {
		return nil
	}
	// 0 行有四种可能，读一次把「预留不存在 / 不属于本调用方」与「已挂同一 Run」
	// （幂等重放）分开；已挂**别的** Run 是账目矛盾，必须报错而不是改口。
	var linked sql.NullString
	err = ex.QueryRowContext(ctx,
		`SELECT run_id FROM quota_reservations
		  WHERE reservation_id = $1 AND caller_id = $2`,
		rec.ReservationID, rec.Principal.CallerID).Scan(&linked)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("回填预留归属（run_id=%s）: 预留 %s 不存在或不属于调用方 %s",
			rec.RunID, rec.ReservationID, rec.Principal.CallerID)
	}
	if err != nil {
		return fmt.Errorf("读取预留归属（run_id=%s）: %w", rec.RunID, err)
	}
	if linked.Valid && linked.String == rec.RunID {
		return nil // 幂等重放：已经是本 Run 的预留
	}
	return fmt.Errorf("回填预留归属（run_id=%s）: 预留 %s 已属于 Run %q",
		rec.RunID, rec.ReservationID, linked.String)
}

// manifestRef 是一条待落 `run_resource_refs` 的引用（种类 + 版本指向）。
// manifestRef pairs one reference with its resource kind.
type manifestRef struct {
	kind core.ResourceKind
	ref  core.ResourceVersionRef
}

// manifestRefs 把清单展开为引用行：Provider 一条、每个 MCP 一条、每个技能一条。
// 顺序与 BuildRunResourceManifest 的规范化结果一致（MCP / 技能按 name 升序）。
// manifestRefs flattens the manifest into the reference rows, in the builder's order.
func manifestRefs(m core.RunResourceManifest) []manifestRef {
	refs := make([]manifestRef, 0, 1+len(m.MCPServers)+len(m.Skills))
	refs = append(refs, manifestRef{kind: core.ResourceModelConnection, ref: m.Provider.ResourceVersionRef})
	for _, ref := range m.MCPServers {
		refs = append(refs, manifestRef{kind: core.ResourceMCPServer, ref: ref})
	}
	for _, skill := range m.Skills {
		refs = append(refs, manifestRef{kind: core.ResourceSkill, ref: skill.ResourceVersionRef})
	}
	return refs
}

// validateRunManifest 校验清单并产出待落库的三样:规整哈希、快照 JSON、引用行。
// 四道判据各自对应一条硬约束：
//   - 格式版本 > 0：只有准入构造器（core.BuildRunResourceManifest）产出的清单才带版本号，
//     零值说明调用方手拼了一份——不落库；
//   - 生命周期只存 `active`（schema 注释与 §3.4）：非启用状态根本不该准入；
//   - 哈希重算一致：`Hash` 是持久结构的派生值，不一致即内容被改过；
//   - 引用非空且不重复：主键 `(run_id, kind, id)` 会拒绝重复，但让它以 SQL 错误暴露
//     不如在这里点名是哪一条——同一 Run 的同一资源只能固定一个版本。
//
// validateRunManifest validates the manifest and yields the hash, snapshot JSON and refs.
func validateRunManifest(manifest core.RunResourceManifest) (string, []byte, []manifestRef, error) {
	if manifest.FormatVersion <= 0 {
		return "", nil, nil,
			fmt.Errorf("资源清单格式版本非法（%d）：清单必须由准入构造器产出", manifest.FormatVersion)
	}
	if manifest.AgentLifecycle != string(core.AgentActive) {
		return "", nil, nil,
			fmt.Errorf("资源清单的生命周期状态必须是 active（实际 %q）：非启用状态不得准入",
				manifest.AgentLifecycle)
	}
	// v2 起清单必须固定 Agent 身份：引用视图与审计按 UID 归属，空 UID 的清单会让
	// "这次运行是哪个 Agent"只能靠名称推断——同名重建后即失真。
	// From v2 the manifest must pin the agent identity rather than rely on the name.
	if manifest.FormatVersion >= 2 {
		if _, err := core.ParseAgentUID(string(manifest.AgentUID)); err != nil {
			return "", nil, nil, fmt.Errorf("资源清单的 Agent 身份非法: %w", err)
		}
		// 凭据固定：托管模式必须带合法 id、版本与目标绑定；env 模式三项全空。
		// 这条判据是 freeze 的前提——不合法就写不出 `run_credential_refs` 的账。
		// The credential pin must be coherent before it can be frozen.
		if err := validateManifestCredentialPin(manifest.Provider); err != nil {
			return "", nil, nil, err
		}
		// 通知渠道集合：每个条目都要能落到 `run_notify_refs`（名称非空、UID 合法）。
		for _, channel := range manifest.Notify.Channels {
			if channel.Name == "" {
				return "", nil, nil, errors.New("资源清单含空的通知渠道名称")
			}
			if _, err := core.ParseResourceUID(string(channel.ResourceUID)); err != nil {
				return "", nil, nil, fmt.Errorf("资源清单的通知渠道 %q 身份非法: %w", channel.Name, err)
			}
		}
	}
	hash, err := core.HashManifest(manifest)
	if err != nil {
		return "", nil, nil, fmt.Errorf("重算资源清单哈希: %w", err)
	}
	if manifest.Hash != hash {
		return "", nil, nil,
			fmt.Errorf("资源清单哈希与内容不一致：给定 %q，重算 %q", manifest.Hash, hash)
	}
	snapshot, err := core.MarshalManifest(manifest)
	if err != nil {
		return "", nil, nil, fmt.Errorf("序列化资源清单: %w", err)
	}

	refs := manifestRefs(manifest)
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.ref.Name == "" {
			return "", nil, nil, fmt.Errorf("资源清单含空的 %s 标识", ref.kind)
		}
		if ref.ref.VersionID <= 0 {
			return "", nil, nil,
				fmt.Errorf("资源清单的 %s/%s 版本标识非法（%d）", ref.kind, ref.ref.Name, ref.ref.VersionID)
		}
		// 引用行的身份列是 NOT NULL 的前置判据：清单缺 UID 时在这里拒绝，
		// 而不是让 SQL 约束在事务中途抛出看不出是哪条引用的错误。
		// The uid is the reference's identity column; refuse a missing one here.
		if _, err := core.ParseResourceUID(string(ref.ref.ResourceUID)); err != nil {
			return "", nil, nil,
				fmt.Errorf("资源清单的 %s/%s 资源身份非法: %w", ref.kind, ref.ref.Name, err)
		}
		key := string(ref.kind) + "\x00" + ref.ref.Name
		if _, dup := seen[key]; dup {
			return "", nil, nil,
				fmt.Errorf("资源清单重复引用 %s/%s：同一 Run 的同一资源只能固定一个版本",
					ref.kind, ref.ref.Name)
		}
		seen[key] = struct{}{}
	}
	return hash, snapshot, refs, nil
}

// sqlExecutor 是 `*sql.DB` 与 `*sql.Tx` 的公共执行面：恢复路径要在短事务内复用仓储的
// 单写语句（**不**给事务另写第二份 SQL——两份同义 SQL 就是第二处漂移点）。
// sqlExecutor is the shared execution surface of *sql.DB and *sql.Tx so recovery can reuse
// repository statements inside its transaction instead of duplicating them.
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// triggerFor 按渠道映射 `runs.trigger`（§6 固定口径 8）。未知渠道 → 错误（见 StartRun 注释）。
// 显式值只允许 `scheduler` 渠道下的 `cron` / `manual`（约束 15）：显式值**不得**绕过渠道
// 约束——否则任意渠道都能自称 manual，审计列随之失去意义。
// An explicit trigger is accepted only for the scheduler channel (constraint 15); every
// other combination is refused rather than silently rewritten.
func triggerFor(channel string, explicit core.Trigger) (string, error) {
	if explicit != "" {
		switch {
		case channel == "scheduler" && explicit == core.TriggerCron:
			return triggerCron, nil
		case channel == "scheduler" && explicit == core.TriggerManual:
			return triggerManual, nil
		default:
			return "", fmt.Errorf(
				"预写运行账本行: 渠道 %q 不接受显式 trigger %q（只有 scheduler 可用 cron / manual，约束 15）",
				channel, explicit)
		}
	}
	switch channel {
	case "cli":
		return triggerUser, nil
	case "web":
		return triggerAPI, nil
	case "scheduler":
		return triggerCron, nil
	case "admin":
		return triggerManual, nil
	default:
		return "", fmt.Errorf("预写运行账本行: 不认识的渠道 %q（无法映射 runs.trigger，§6 固定口径 8）", channel)
	}
}

// AgentResiduals 计算图 8 [4] 的四条归档完成判据（FR-24-41）。全部达成时 `Unmet` 为空。
//
// **只看持久结果，不看租约**：租约过期 / 已释放不构成"执行已退出"的证据——行可能仍未
// 终态、引用仍未释放，拿 TTL 当退出判据会把"已失联但收尾未完成"的 Agent 直接归档掉
// （图 8 的 R4 修正）。四条判据都以该 Agent 的 Run 为范围：
//
//	① no_running_run      `runs.status = 'running'`（同时带回 Run 标识，供 409 点名）
//	② no_pending_audit    `llm_calls` / `tool_invocations` 的 `status='pending'`
//	③ refs_released       run_resource_refs / run_credential_refs / run_notify_refs 全已释放
//	④ no_pending_delivery `schedule_deliveries.status='pending'`
//
// 四条分多条语句读而不是一个快照：归档屏障已经把新 Run 封住，方向只会朝"全部达成"
// 收敛（该 Agent 没有任何 Run 行时四条同时成立），因此不存在"读的时候成立、返回时又
// 变不成立"的窗口。查询失败如实上抛——读不到判据**不得**当成已达成（fail-closed）。
//
// AgentResiduals evaluates the four archive-completion predicates from persisted state
// only; lease expiry is never evidence that execution ended.
func (s *RunStore) AgentResiduals(ctx context.Context, agent string) (core.ArchiveResiduals, error) {
	var out core.ArchiveResiduals

	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT run_id FROM runs
		  WHERE agent = $1 AND status = 'running'
		  ORDER BY started_at, run_id`, agent)
	if err != nil {
		return core.ArchiveResiduals{}, fmt.Errorf("列出 Agent %s 的在途 Run: %w", agent, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return core.ArchiveResiduals{}, fmt.Errorf("扫描 Agent %s 的在途 Run: %w", agent, err)
		}
		out.RunIDs = append(out.RunIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return core.ArchiveResiduals{}, fmt.Errorf("遍历 Agent %s 的在途 Run: %w", agent, err)
	}
	if len(out.RunIDs) > 0 {
		out.Unmet = append(out.Unmet, core.ArchiveResidualRunningRun)
	}

	pendingAudit, err := s.agentAny(ctx, agent, `SELECT EXISTS (
		  SELECT 1 FROM llm_calls c JOIN runs r ON r.run_id = c.run_id
		   WHERE r.agent = $1 AND c.status = 'pending')
		OR EXISTS (
		  SELECT 1 FROM tool_invocations t JOIN runs r ON r.run_id = t.run_id
		   WHERE r.agent = $1 AND t.status = 'pending')`)
	if err != nil {
		return core.ArchiveResiduals{}, err
	}
	if pendingAudit {
		out.Unmet = append(out.Unmet, core.ArchiveResidualPendingAudit)
	}

	unreleased, err := s.agentAny(ctx, agent, `SELECT EXISTS (
		  SELECT 1 FROM run_resource_refs x JOIN runs r ON r.run_id = x.run_id
		   WHERE r.agent = $1 AND x.released_at IS NULL)
		OR EXISTS (
		  SELECT 1 FROM run_credential_refs x JOIN runs r ON r.run_id = x.run_id
		   WHERE r.agent = $1 AND x.released_at IS NULL)
		OR EXISTS (
		  SELECT 1 FROM run_notify_refs x JOIN runs r ON r.run_id = x.run_id
		   WHERE r.agent = $1 AND x.released_at IS NULL)`)
	if err != nil {
		return core.ArchiveResiduals{}, err
	}
	if unreleased {
		out.Unmet = append(out.Unmet, core.ArchiveResidualRefsReleased)
	}

	pendingDelivery, err := s.agentAny(ctx, agent,
		`SELECT EXISTS (SELECT 1 FROM schedule_deliveries
		                 WHERE agent_name = $1 AND status = 'pending')`)
	if err != nil {
		return core.ArchiveResiduals{}, err
	}
	if pendingDelivery {
		out.Unmet = append(out.Unmet, core.ArchiveResidualPendingDelivery)
	}

	return out, nil
}

// agentAny 执行一条"该 Agent 范围内是否存在残留"的布尔查询。
// agentAny runs one "does any residual exist for this agent" boolean query.
func (s *RunStore) agentAny(ctx context.Context, agent, query string) (bool, error) {
	var found bool
	if err := s.db.pool.QueryRowContext(ctx, query, agent).Scan(&found); err != nil {
		return false, fmt.Errorf("读取 Agent %s 的归档残量判据: %w", agent, err)
	}
	return found, nil
}
