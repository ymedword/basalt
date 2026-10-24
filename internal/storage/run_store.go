package storage

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

// RunStore 是 runs 的唯一 SQL 入口（core.RunStore 的实现）。
// RunStore is the only SQL entry point for runs.
type RunStore struct{ db *DB }

// 编译期断言：RunStore 满足消费方（core）声明的运行账本接口。
// Compile-time assertion that RunStore satisfies the consumer-declared interface.
var _ core.RunStore = (*RunStore)(nil)

// NewRunStore 绑定一个已打开的写模式库句柄。
// NewRunStore binds the store to an opened writable database handle.
func NewRunStore(db *DB) *RunStore { return &RunStore{db: db} }

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
func (s *RunStore) StartRun(ctx context.Context, rec core.RunRecord, manifest core.RunResourceManifest) error {
	trigger, err := triggerFor(rec.Channel, rec.Trigger)
	if err != nil {
		return err
	}
	if manifest.FormatVersion == 0 {
		return insertRunRow(ctx, s.db.write, rec, trigger)
	}
	hash, snapshot, refs, err := validateRunManifest(manifest)
	if err != nil {
		return fmt.Errorf("预写运行账本（run_id=%s）: %w", rec.RunID, err)
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("预写运行账本（run_id=%s）: 开启事务: %w", rec.RunID, err)
	}
	// 未走到 Commit 的每条返回路径都在这里回滚；已提交后 Rollback 返回 ErrTxDone，
	// 不可补救也无事可做（与 EnsureVersion 同一收尾手法）。
	defer func() { _ = tx.Rollback() }()

	if err := insertRunRow(ctx, tx, rec, trigger); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run_resource_manifests
		   (run_id, format_version, manifest_hash, agent_lifecycle_state, snapshot_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
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
			 VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("预写运行账本（run_id=%s）: 提交: %w", rec.RunID, err)
	}
	return nil
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
// 并在**同一事务**释放该 Run 的资源引用（约束 19）。
// FinishRun writes the terminal state and releases the run's resource references in the
// same transaction.
//
// 两条守卫。① `WHERE status = 'running'`：取消路径只发信号、崩溃恢复与超时兜底都可能
// 并发调用本方法；已判定的终态**不得**被迟到的写入改口（注 8）。② 影响 0 行时读当前行：
// 行存在 = 幂等成功（不覆盖、不释放——释放只属于本次实际转换的行）；行不存在 = **如实
// 报错**，账本行缺失说明启动纪律被绕过，不该被"更新成功"掩盖。
// The status guard keeps a late terminal write from re-judging a finished run; a zero-row
// update is idempotent success when the row exists, and a hard error when it does not.
func (s *RunStore) FinishRun(ctx context.Context, runID string, status core.RunStatus, reason string,
	iterations, toolCalls int, now time.Time,
) error {
	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("回填运行终态（run_id=%s）: 开启事务: %w", runID, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE runs
		    SET status = ?, reason = ?, iterations = ?, tool_calls = ?, finished_at = ?
		  WHERE run_id = ? AND status = ?`,
		string(status), nullIfEmpty(reason), iterations, toolCalls, now.UnixMilli(),
		runID, string(core.StatusRunning))
	if err != nil {
		return fmt.Errorf("回填运行终态（run_id=%s）: %w", runID, err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认运行终态写入（run_id=%s）: %w", runID, err)
	}
	if affected == 0 {
		if err := requireRunRow(ctx, tx, runID); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("回填运行终态（run_id=%s）: 提交: %w", runID, err)
		}
		return nil
	}

	if _, err := releaseRunRefs(ctx, tx, runID, now); err != nil {
		return err
	}
	if _, err := releaseRunCredentialRefs(ctx, tx, runID, now); err != nil {
		return err
	}
	if _, err := releaseRunNotifyRefs(ctx, tx, runID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("回填运行终态（run_id=%s）: 提交: %w", runID, err)
	}
	return nil
}

// releaseRunRefs 释放某个 Run 的全部未释放引用（约束 19 / 注 8）。
// **事务边界由调用方保证**：终态转换与引用释放必须一起提交——正是"版本永远显示被引用"
// 或"Run 已收尾却仍占着版本"两种中间态的防线。
// releaseRunRefs stamps released_at on the run's unreleased references; the caller owns
// the transaction so the status flip and the release always commit together.
func releaseRunRefs(ctx context.Context, ex sqlExecutor, runID string, now time.Time) (int, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE run_resource_refs
		    SET released_at = ?
		  WHERE run_id = ? AND released_at IS NULL`,
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
func requireRunRow(ctx context.Context, q queryer, runID string) error {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM runs WHERE run_id = ?`, runID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("回填运行终态（run_id=%s）: 账本行不存在", runID)
	}
	if err != nil {
		return fmt.Errorf("读取运行账本行（run_id=%s）: %w", runID, err)
	}
	return nil
}

// insertRunRow 写起始行——零清单路径与「运行行 + 清单 + 引用」事务共用的**唯一** INSERT
// 语句（两份同义 SQL 就是第二处漂移点）。
// insertRunRow is the single starting-row INSERT shared by both StartRun paths.
func insertRunRow(ctx context.Context, ex sqlExecutor, rec core.RunRecord, trigger string) error {
	// 单条 INSERT 即一个短事务；reason / finished_at 留 NULL（终态才写），
	// iterations / tool_calls 取列缺省 0（还没跑，不是"跑了零次"）。
	_, err := ex.ExecContext(ctx,
		`INSERT INTO runs
		   (run_id, session_id, agent, channel, user_id, trigger, status,
		    reason, iterations, tool_calls, config_hash, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, 0, 0, ?, ?, NULL)`,
		rec.RunID, nullIfEmpty(rec.SessionID), rec.Agent, rec.Channel, rec.UserID,
		trigger, string(core.StatusRunning), nullIfEmpty(rec.ConfigHash), rec.StartedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("预写运行账本行（run_id=%s）: %w", rec.RunID, err)
	}
	return nil
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
