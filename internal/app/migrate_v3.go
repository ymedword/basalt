package app

// 本文件是 v3 迁移的编排（颗粒度文档 R4.10 / T119）：把「旧库 2 → 3」拆成**固定顺序**
// 的六步，每一步失败时停在自己那一步、报出该怎么恢复。
//
// 六步与失败分离：
//  1. 准入门（fail-closed）：先写 `system.baseline_v3` 的 pending 行；预写失败 → 不写
//     任何文件、不迁移、拒绝启动（宪法 V）。
//  2. 状态文件 v1 → v2：以**权威状态文件**为准补发 `ag_` UID（已有 UID 原样保留）。
//     失败 → 文件可能已部分升级（幂等可重跑），数据库未变，启动中止。
//  3. SQL 迁移（单事务）：失败 → 整事务回滚、`schema_version` 保持旧值；此时状态文件
//     已是 v2，**不自动降级**，启动中止并提示按备份恢复。
//  4. 数据库身份回填（契约 3g → 2c：**必须**在第 3 步加列之后）：两侧不一致即中止，
//     不覆盖任一侧。
//  5. 绑定回填写入 `AGENT.md`：只在能证明资源身份连续时写；证明不了就不写（该引用在
//     准入时报 `AGENT_RESOURCE_BINDING_MISSING`），**不得**按同名猜身份。
//  6. 完成守卫：全量重建绑定投影 → 操作置 `success` 并写迁移报告；任一步失败置 `failed`。
//
// 两条不可违背的约束：迁移**不读主密钥**（只处理结构与非托管数据），**不按同名猜 UID**。
// This file orchestrates the v2 -> v3 upgrade in a fixed six-step order. Each failure
// stops at its own step and names the recovery; the migration never reads the master
// key and never infers a uid from a name.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// actionBaselineV3 是 v3 迁移的准入 / 完成守卫（契约 §9.3 的固定动作字面量）。
	// actionBaselineV3 is the fixed management action of the v3 migration.
	actionBaselineV3 = "system.baseline_v3"

	// migrateV3ResultOK / migrateV3ResultInternal 是 result_code 的两个固定取值。
	// The two fixed result codes of the migration operation.
	migrateV3ResultOK       = "OK"
	migrateV3ResultInternal = "INTERNAL"

	// migrateV3ReportVersion 是 detail_json 的报告形态版本：读报告的人靠它分辨字段。
	// migrateV3ReportVersion versions the detail_json report shape.
	migrateV3ReportVersion = 1

	// migrateV3OperationWindow 是删除证据查询的窗口上限（ListOperations 的上限是 200）：
	// 只需要回答"最后保存之后有没有删除"，窗口取满即可。
	// migrateV3OperationWindow is the delete-evidence query window.
	migrateV3OperationWindow = 200

	// migrateV3DataVersion 是六步数据编排的所属版本：只有尚未到 v3 的库（v2 起）才需要
	// 状态文件升级 / 身份回填 / 绑定回填；库已到 v3 时数据面不再改动，只把 SQL 前向
	// 推进到目标版本（v4 的 0004 只加表 / 列 / 索引）。
	// migrateV3DataVersion is the version the six-step data orchestration belongs to.
	migrateV3DataVersion = 3
)

// 绑定回填的跳过原因（固定短语，进 detail_json；页面按同一份短语展示）。
// The fixed skip reasons of the binding backfill.
const (
	// migrateSkipDefinitionMissing：Agent 没有定义文件（只剩状态文件），无从读引用。
	migrateSkipDefinitionMissing = "definition_missing"
	// migrateSkipDefinitionInvalid：定义文件解析失败，读不出引用。
	migrateSkipDefinitionInvalid = "definition_invalid"
	// migrateSkipBindingsInvalid：已有绑定块结构非法——保持原样，等修复后重跑。
	migrateSkipBindingsInvalid = "bindings_invalid"
	// migrateSkipNeverPublished：资源从未发布，没有可证明的 UID。
	migrateSkipNeverPublished = "never_published"
	// migrateSkipDeleted：当前指针已删除（同名重建前的那一代）。
	migrateSkipDeleted = "deleted"
	// migrateSkipDeleteAfterSave：最后保存之后出现过 `*.delete` 操作——可能是删除后
	// 同名重建，身份连续性无法证明。
	migrateSkipDeleteAfterSave = "delete_after_save"
	// migrateSkipBindingConflict：文件里已有**不同**绑定——不覆盖，记冲突。
	migrateSkipBindingConflict = "binding_conflict"
	// migrateSkipEditUnsupported：定义文件结构无法安全块编辑，磁盘逐字节不变。
	migrateSkipEditUnsupported = "edit_unsupported"
)

// MigrateV3Deps 是 v3 迁移编排的全部依赖。`Agents` 为空时按工作区目录清单自取——
// 迁移在装载器之前运行，不能等装载结果。
// MigrateV3Deps carries every dependency; an empty Agents list is derived from the
// workspace because the migration runs before the agent loader.
type MigrateV3Deps struct {
	DB         *storage.DB
	Operations core.OperationStore
	Lifecycle  core.IdentityUpgradePort
	Resources  *storage.ResourceStore
	Notify     *storage.NotifyAdminStore
	Bindings   *storage.BindingStore
	Root       string
	Agents     []string
	Logger     *slog.Logger
	Now        func() time.Time
	Target     int
}

// MigrateV3Skip 是一条"没有写绑定"的记录：Agent、资源类别、引用名与固定原因。
// MigrateV3Skip records one reference the migration refused to bind, with its reason.
type MigrateV3Skip struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// MigrateV3Limit 是一条迁移限制（契约 3h / R4.10 第 7 条）：同名删除 / 重建后，旧版本
// 无法可靠拆分归属，全部归入当前指针 UID；这里逐条列出资源名、版本数与相关操作 ID，
// 供运维核对。
// MigrateV3Limit reports one delete/rebuild cycle whose old versions were all attributed
// to the current pointer uid.
type MigrateV3Limit struct {
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	Versions     int      `json:"versions"`
	OperationIDs []string `json:"operation_ids"`
}

// MigrateV3Report 是迁移完成时写进 `detail_json` 的报告：计数、跳过清单与限制清单。
// MigrateV3Report is the detail_json payload written on success.
type MigrateV3Report struct {
	Version            int              `json:"version"`
	FromVersion        int              `json:"from_version"`
	ToVersion          int              `json:"to_version"`
	StateFilesUpgraded int              `json:"state_files_upgraded"`
	StateFilesMissing  int              `json:"state_files_missing"`
	AgentUIDsMirrored  int              `json:"agent_uids_mirrored"`
	BindingsWritten    int              `json:"bindings_written"`
	BindingsUnchanged  int              `json:"bindings_unchanged"`
	ProjectionsRebuilt int              `json:"projections_rebuilt"`
	Skips              []MigrateV3Skip  `json:"binding_skips"`
	Limits             []MigrateV3Limit `json:"migration_limits"`
	StartedAt          int64            `json:"started_at"`
	FinishedAt         int64            `json:"finished_at"`
}

// RunMigrateV3 执行六步编排。库已是目标版本（新装或已完成）→ 直接返回零报告，
// 不写操作行（R4.10 第 6 条：空库由 `schema.sql` 直接建到目标版本）；库已在 v3 而
// 目标更高 → 只做 SQL 前向迁移（数据面已冻结，不重跑数据编排）。
// RunMigrateV3 runs the six steps when crossing v3, and only the SQL migration after it.
func RunMigrateV3(ctx context.Context, deps MigrateV3Deps) (MigrateV3Report, error) {
	agents, err := deps.validate()
	if err != nil {
		return MigrateV3Report{}, err
	}
	logger := deps.logger()
	now := deps.now()
	target := deps.target()

	current, err := deps.DB.SchemaVersion(ctx)
	if err != nil {
		return MigrateV3Report{}, fmt.Errorf("v3 迁移：读取 schema 版本: %w", err)
	}
	if current == target {
		logger.Info("v3 迁移：库已是目标版本，跳过", "schema_version", current)
		return MigrateV3Report{Version: migrateV3ReportVersion, FromVersion: current, ToVersion: current}, nil
	}
	if current > target {
		return MigrateV3Report{}, fmt.Errorf("v3 迁移：库版本 v%d 高于本二进制支持的 v%d，拒绝启动", current, target)
	}
	if current < migrateV3DataVersion-1 {
		return MigrateV3Report{}, fmt.Errorf(
			"v3 迁移：数据编排从 v%d 起，当前 v%d 没有定义的升级路径，拒绝启动",
			migrateV3DataVersion-1, current)
	}
	if current >= migrateV3DataVersion {
		// 数据编排只属于「跨到 v3」的那一次：v3 及以上的库不重跑状态文件 / 身份 / 绑定
		// 回填，也不写 `system.baseline_v3` 操作行，只把 SQL 前向推进到目标版本。
		// The data steps belong to crossing v3; an already-v3 database only moves forward.
		if migrateErr := deps.DB.Migrate(ctx, target); migrateErr != nil {
			return MigrateV3Report{}, fmt.Errorf(
				"v3 迁移：SQL 前向迁移 v%d → v%d 失败，库保持在 v%d: %w", current, target, current, migrateErr)
		}
		logger.Info("v3 迁移：数据编排已完成，仅前向迁移 schema", "from", current, "to", target)
		return MigrateV3Report{
			Version:     migrateV3ReportVersion,
			FromVersion: current,
			ToVersion:   target,
			StartedAt:   now.UnixMilli(),
			FinishedAt:  now.UnixMilli(),
		}, nil
	}

	report := MigrateV3Report{
		Version:     migrateV3ReportVersion,
		FromVersion: current,
		ToVersion:   target,
		StartedAt:   now.UnixMilli(),
	}

	// ① 准入门（fail-closed）：预写失败 = 一行文件都不写、库不动、拒绝启动。
	opID, pending, err := findPendingAction(ctx, deps.Operations, actionBaselineV3)
	if err != nil {
		return report, fmt.Errorf("v3 迁移：读取未完成操作: %w", err)
	}
	if pending {
		logger.Info("v3 迁移：复用未完成的迁移操作", "operation_id", opID)
	} else {
		opID = core.NewOperationID(now)
		record := core.OperationRecord{
			OperationID:  opID,
			Action:       actionBaselineV3,
			TargetKind:   targetKindSystem,
			TargetID:     targetKindSystem,
			ActorSubject: core.ActorSubjectMigration,
			AuthMethod:   core.AuthMethodMigration,
			Source:       baselineSource,
			Status:       core.OperationPending,
			CreatedAt:    now,
		}
		if beginErr := deps.Operations.BeginOperation(ctx, record); beginErr != nil {
			return report, fmt.Errorf("v3 迁移：预写操作记录失败，未写任何文件、未迁移，拒绝启动: %w", beginErr)
		}
		logger.Info("v3 迁移：开始", "operation_id", opID, "from", current, "to", target)
	}

	// fail 收口：待处理操作置 failed 并把失败原因带回装配层（装配层据此拒绝启动）。
	fail := func(cause error) error {
		if finishErr := deps.Operations.FinishOperation(ctx, opID, core.OperationFailed,
			migrateV3ResultInternal, cause.Error(), nil, deps.now()); finishErr != nil {
			cause = errors.Join(cause, fmt.Errorf("回填 v3 迁移失败终态: %w", finishErr))
		}
		return cause
	}

	// ② 状态文件 v1 → v2：身份以权威状态文件为准，已有 UID 原样保留。
	uids, upgradeErr := upgradeAgentStateFiles(ctx, deps.Lifecycle, agents, opID)
	if upgradeErr != nil {
		report.StateFilesUpgraded = uids.upgraded
		report.StateFilesMissing = uids.missing
		return report, fail(fmt.Errorf(
			"v3 迁移：状态文件升级失败（数据库未变；已升级的文件为 v2、可原样重跑），启动中止并点名恢复: %w", upgradeErr))
	}
	report.StateFilesUpgraded = uids.upgraded
	report.StateFilesMissing = uids.missing

	// ③ SQL 迁移（单事务）：失败即整事务回滚，schema_version 保持旧值。
	if migrateErr := deps.DB.Migrate(ctx, target); migrateErr != nil {
		return report, fail(fmt.Errorf(
			"v3 迁移：SQL 迁移失败，数据库已回滚到 v%d；状态文件已是 v2，不自动降级——启动中止，按备份恢复: %w",
			current, migrateErr))
	}
	logger.Info("v3 迁移：SQL 迁移完成", "schema_version", target)

	// ④ 数据库身份回填：**必须**在第 3 步加列之后（契约 3g → 2c）。
	for _, agent := range agents {
		if backfillErr := deps.Resources.BackfillAgentUID(ctx, agent, uids.byAgent[agent], opID, now); backfillErr != nil {
			return report, fail(fmt.Errorf("v3 迁移：数据库身份回填失败（Agent %s）: %w", agent, backfillErr))
		}
		report.AgentUIDsMirrored++
	}

	// ⑤ 绑定回填写入 `AGENT.md`：只在能证明身份连续时写。
	for _, agent := range agents {
		written, skips, backfillErr := backfillAgentBindings(ctx, deps, agent)
		report.Skips = append(report.Skips, skips...)
		switch {
		case backfillErr != nil:
			return report, fail(fmt.Errorf(
				"v3 迁移：绑定回填失败（Agent %s）；已回填的文件保持原样、重跑按幂等处理，启动中止: %w", agent, backfillErr))
		case written:
			report.BindingsWritten++
		default:
			report.BindingsUnchanged++
		}
	}

	// ⑥ 完成守卫：全量重建绑定投影（以文件为准），再写报告与成功终态。
	rebuilt, projectionErr := rebuildBindingProjection(ctx, deps, agents, uids.byAgent, now)
	if projectionErr != nil {
		return report, fail(fmt.Errorf("v3 迁移：绑定投影重建失败: %w", projectionErr))
	}
	report.ProjectionsRebuilt = rebuilt

	limits, limitErr := migrationLimits(ctx, deps)
	if limitErr != nil {
		return report, fail(fmt.Errorf("v3 迁移：迁移限制清单生成失败: %w", limitErr))
	}
	report.Limits = limits
	report.FinishedAt = deps.now().UnixMilli()

	detail, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		return report, fail(fmt.Errorf("v3 迁移：生成迁移报告: %w", marshalErr))
	}
	if finishErr := deps.Operations.FinishOperation(ctx, opID, core.OperationSuccess,
		migrateV3ResultOK, "", detail, deps.now()); finishErr != nil {
		return report, fmt.Errorf("v3 迁移：回填成功终态失败: %w", finishErr)
	}
	logger.Info("v3 迁移：完成", "operation_id", opID,
		"state_files_upgraded", report.StateFilesUpgraded,
		"state_files_missing", report.StateFilesMissing,
		"bindings_written", report.BindingsWritten,
		"migration_limits", len(report.Limits))
	return report, nil
}

// agentUIDs 是第 2 步的结果：逐 Agent 的 UID、升级计数与缺失计数。
// agentUIDs is the outcome of step 2.
type agentUIDs struct {
	byAgent  map[string]core.AgentUID
	upgraded int
	missing  int
}

// upgradeAgentStateFiles 逐个升级状态文件：缺 UID 的补发、已有 UID 原样保留、
// 文件缺失按 `legacy_unrecorded` 记（**不**按同名猜）。
// upgradeAgentStateFiles upgrades each state file, minting an identity only when absent.
func upgradeAgentStateFiles(ctx context.Context, lifecycle core.IdentityUpgradePort, agents []string, opID string) (agentUIDs, error) {
	out := agentUIDs{byAgent: make(map[string]core.AgentUID, len(agents))}
	for _, agent := range agents {
		rec, upgraded, err := lifecycle.UpgradeIdentity(ctx, agent, opID)
		if err != nil {
			return out, fmt.Errorf("升级 Agent %s 的身份: %w", agent, err)
		}
		if rec.AgentUID == "" {
			// 状态文件缺失：没有可证明的身份，交给数据库镜像记 legacy_unrecorded。
			out.missing++
			continue
		}
		if upgraded {
			out.upgraded++
		}
		out.byAgent[agent] = rec.AgentUID
	}
	return out, nil
}

// backfillAgentBindings 为一个 Agent 计算并写入绑定块：返回是否真的写了文件。
// 证明不了连续性的引用逐条记 skip、不写；文件已有**不同**绑定 → 不覆盖并记冲突。
// backfillAgentBindings writes the provable bindings of one agent into AGENT.md.
func backfillAgentBindings(ctx context.Context, deps MigrateV3Deps, agent string) (bool, []MigrateV3Skip, error) {
	view, err := workspace.InspectAgentForMigration(deps.Root, agent)
	if err != nil {
		return false, []MigrateV3Skip{{
			Agent: agent, Kind: "agent", Name: agent, Reason: migrateSkipReason(err),
		}}, nil
	}

	planned := core.ResourceBindings{}
	var skips []MigrateV3Skip
	if name := view.Profile.Provider.Name; name != "" {
		uid, reason, proveErr := proveModelConnection(ctx, deps, name, view.ModTime)
		if proveErr != nil {
			return false, nil, proveErr
		}
		if reason != "" {
			skips = append(skips, MigrateV3Skip{Agent: agent, Kind: string(core.BindingModelConnection), Name: name, Reason: reason})
		} else {
			planned.ModelConnections = append(planned.ModelConnections, core.ResourceBinding{Name: name, UID: uid})
		}
	}
	for _, name := range view.Profile.NotifyChannels {
		uid, reason, proveErr := proveNotifyChannel(ctx, deps, name, view.ModTime)
		if proveErr != nil {
			return false, nil, proveErr
		}
		if reason != "" {
			skips = append(skips, MigrateV3Skip{Agent: agent, Kind: string(core.BindingNotifyChannel), Name: name, Reason: reason})
		} else {
			planned.NotifyChannels = append(planned.NotifyChannels, core.ResourceBinding{Name: name, UID: uid})
		}
	}

	if len(planned.ModelConnections) == 0 && len(planned.NotifyChannels) == 0 {
		return false, skips, nil
	}
	if !bindingsEmpty(view.Bindings) && !bindingsEqual(view.Bindings, planned) {
		// 重跑保护：文件里已有的绑定与本次计划不同 → 不覆盖，记冲突交人核对。
		skips = append(skips, MigrateV3Skip{Agent: agent, Kind: "agent", Name: agent, Reason: migrateSkipBindingConflict})
		return false, skips, nil
	}

	backfill, err := workspace.BackfillAgentBindings(deps.Root, agent, planned)
	if err != nil {
		if errors.Is(err, workspace.ErrEditUnsupported) {
			skips = append(skips, MigrateV3Skip{Agent: agent, Kind: "agent", Name: agent, Reason: migrateSkipEditUnsupported})
			return false, skips, nil
		}
		return false, nil, err
	}
	return backfill.Changed, skips, nil
}

// proveModelConnection 判定模型连接引用的身份连续性：当前指针 active、已发布，且
// 最后保存之后没有 `model_connection.delete`。
// proveModelConnection decides whether a provider reference proves identity continuity.
func proveModelConnection(ctx context.Context, deps MigrateV3Deps, name string, savedAt time.Time) (core.ResourceUID, string, error) {
	ptr, err := deps.Resources.Pointer(ctx, core.ResourceModelConnection, name)
	if errors.Is(err, core.ErrResourceNotFound) {
		return "", migrateSkipNeverPublished, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("读取模型连接指针 %s: %w", name, err)
	}
	if ptr.DeletedAt != nil {
		return "", migrateSkipDeleted, nil
	}
	if ptr.PublishedVersionID == 0 {
		return "", migrateSkipNeverPublished, nil
	}
	blocked, err := deleteAfter(ctx, deps.Operations, core.ActionModelConnectionDelete,
		core.TargetKindModelConnection, name, savedAt)
	if err != nil {
		return "", "", err
	}
	if blocked {
		return "", migrateSkipDeleteAfterSave, nil
	}
	return ptr.ResourceUID, "", nil
}

// proveNotifyChannel 判定通知渠道引用的身份连续性；渠道没有版本表，判据是"活动行存在"
// 加"最后保存之后没有 `notify_channel.delete`"。
// proveNotifyChannel decides whether a notify-channel reference proves continuity.
func proveNotifyChannel(ctx context.Context, deps MigrateV3Deps, name string, savedAt time.Time) (core.ResourceUID, string, error) {
	channel, err := deps.Notify.Get(ctx, name)
	if errors.Is(err, core.ErrResourceNotFound) {
		return "", migrateSkipNeverPublished, nil
	}
	if err != nil {
		return "", "", fmt.Errorf("读取通知渠道 %s: %w", name, err)
	}
	blocked, err := deleteAfter(ctx, deps.Operations, core.ActionNotifyChannelDelete,
		core.TargetKindNotifyChannel, name, savedAt)
	if err != nil {
		return "", "", err
	}
	if blocked {
		return "", migrateSkipDeleteAfterSave, nil
	}
	return channel.ResourceUID, "", nil
}

// deleteAfter 回答"最后保存之后有没有对该名称的删除操作"：有 → 身份连续性无法证明。
// 查询只按名称与目标类型走固定窗口（ListOperations 上限 200，按时间倒序）。
// deleteAfter reports whether a delete of that name happened after the last save.
func deleteAfter(ctx context.Context, operations core.OperationStore, action, targetKind, name string, savedAt time.Time) (bool, error) {
	ops, err := operations.ListOperations(ctx, core.OperationFilter{
		Action:     action,
		TargetKind: targetKind,
		TargetID:   name,
		Limit:      migrateV3OperationWindow,
	})
	if err != nil {
		return false, fmt.Errorf("读取 %s 的删除操作（%s）: %w", name, action, err)
	}
	for _, op := range ops {
		if op.CreatedAt.After(savedAt) {
			return true, nil
		}
	}
	return false, nil
}

// rebuildBindingProjection 以**文件**为准全量重建投影：读不出定义的 Agent 清空投影
// （宁可空，也不留与文件不符的旧行）。
// rebuildBindingProjection rebuilds every projection from the definition files.
func rebuildBindingProjection(ctx context.Context, deps MigrateV3Deps, agents []string, uids map[string]core.AgentUID, at time.Time) (int, error) {
	rebuilt := 0
	for _, agent := range agents {
		uid := uids[agent]
		if uid == "" {
			continue // 无可信身份：投影无法归属，保持为空（准入按未绑定处理）
		}
		view, err := workspace.InspectAgentForMigration(deps.Root, agent)
		if err != nil {
			if clearErr := deps.Bindings.Clear(ctx, uid); clearErr != nil {
				return rebuilt, fmt.Errorf("清空 Agent %s 的绑定投影: %w", agent, clearErr)
			}
			deps.logger().Warn("v3 迁移：定义文件不可读，绑定投影清空",
				"agent", agent, "reason", migrateSkipReason(err))
			continue
		}
		if err := deps.Bindings.Rebuild(ctx, uid, agent,
			migrateV3ContentHash(view.Raw), view.Bindings, at); err != nil {
			return rebuilt, fmt.Errorf("重建 Agent %s 的绑定投影: %w", agent, err)
		}
		rebuilt++
	}
	return rebuilt, nil
}

// migrationLimits 逐条列出迁移前的删除 / 重建循环（契约 3h）：同名历史版本无法可靠
// 拆分归属，全部归入当前指针 UID——报告只陈述事实，不猜测拆分、不改写历史内容。
// migrationLimits lists every delete/rebuild cycle for operator review.
func migrationLimits(ctx context.Context, deps MigrateV3Deps) ([]MigrateV3Limit, error) {
	kinds := []core.ResourceKind{
		core.ResourceModelConnection, core.ResourceMCPServer, core.ResourceSkill,
	}
	var limits []MigrateV3Limit
	for _, kind := range kinds {
		pointers, err := deps.Resources.ListPointers(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, ptr := range pointers {
			ops, opErr := deps.Operations.ListOperations(ctx, core.OperationFilter{
				Action:     deleteAction(kind),
				TargetKind: resourceTargetKind(kind),
				TargetID:   ptr.ResourceID,
				Limit:      migrateV3OperationWindow,
			})
			if opErr != nil {
				return nil, opErr
			}
			if len(ops) == 0 {
				continue
			}
			versions, verErr := deps.Resources.VersionsByUID(ctx, kind, ptr.ResourceUID)
			if verErr != nil {
				return nil, verErr
			}
			ids := make([]string, 0, len(ops))
			for _, op := range ops {
				ids = append(ids, op.OperationID)
			}
			slices.Reverse(ids) // 报告按时间正序读（查询是倒序）
			limits = append(limits, MigrateV3Limit{
				Kind:         string(kind),
				Name:         ptr.ResourceID,
				Versions:     len(versions),
				OperationIDs: ids,
			})
		}
	}
	return limits, nil
}

// migrateSkipReason 把读定义文件的错误映射为固定原因短语。
// migrateSkipReason maps a definition-read error to its fixed skip reason.
func migrateSkipReason(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return migrateSkipDefinitionMissing
	case errors.Is(err, core.ErrAgentResourceBindingInvalid):
		return migrateSkipBindingsInvalid
	case errors.Is(err, workspace.ErrEditUnsupported):
		return migrateSkipEditUnsupported
	default:
		return migrateSkipDefinitionInvalid
	}
}

// bindingsEmpty 报告绑定结构是否为空。
// bindingsEmpty reports whether the binding structure has no entries.
func bindingsEmpty(bindings core.ResourceBindings) bool {
	return len(bindings.ModelConnections) == 0 && len(bindings.NotifyChannels) == 0
}

// bindingsEqual 按**集合**比较两组绑定：顺序不是语义，重排不算冲突。
// bindingsEqual compares two binding sets; ordering carries no meaning.
func bindingsEqual(a, b core.ResourceBindings) bool {
	return slices.Equal(sortedBindings(a.ModelConnections), sortedBindings(b.ModelConnections)) &&
		slices.Equal(sortedBindings(a.NotifyChannels), sortedBindings(b.NotifyChannels))
}

// sortedBindings 返回按名称排序的副本（不改调用方切片）。
// sortedBindings returns a name-sorted copy without mutating the caller's slice.
func sortedBindings(entries []core.ResourceBinding) []core.ResourceBinding {
	out := slices.Clone(entries)
	slices.SortFunc(out, func(x, y core.ResourceBinding) int {
		if x.Name != y.Name {
			return compareStrings(x.Name, y.Name)
		}
		return compareStrings(string(x.UID), string(y.UID))
	})
	return out
}

// compareStrings 是字符串的三值比较（slices.SortFunc 的返回值）。
// compareStrings is the three-way string comparison SortFunc expects.
func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// migrateV3ContentHash 给出内容哈希（裸 sha256 十六进制，与资源版本内容哈希同一形态）。
// migrateV3ContentHash renders the bare sha256 hex digest of definition bytes.
func migrateV3ContentHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// 编译期断言：装配层的迁移端口就是本文件的六步编排。
// Compile-time assertion that the assembly's migration port is this orchestration.
var _ Migrator = migrator{}

// migrator 是 `Migrator` 端口的生产实现：把装配期的依赖固化成一次调用。
// migrator is the production Migrator implementation.
type migrator struct {
	deps MigrateV3Deps
}

// NewMigrator 构造迁移编排端口。依赖不在这里校验——真正的校验落在 `RunMigrateV3`
// 的 validate，保证"库已是目标版本就直接返回"之前不漏掉任何装配缺口。
// NewMigrator builds the port; dependency validation stays inside RunMigrateV3.
func NewMigrator(deps MigrateV3Deps) Migrator {
	return migrator{deps: deps}
}

// MigrateV3 执行六步编排（库已是目标版本 → 零报告、不写操作行）。
// MigrateV3 runs the six-step orchestration; an up-to-date database is left untouched.
func (m migrator) MigrateV3(ctx context.Context) (MigrateV3Report, error) {
	return RunMigrateV3(ctx, m.deps)
}

// validate 校验依赖并把 Agent 清单补全；返回按名字排序的清单。
// validate checks the dependencies and completes the sorted agent list.
func (d MigrateV3Deps) validate() ([]string, error) {
	switch {
	case d.DB == nil:
		return nil, errors.New("v3 迁移：缺少数据库句柄")
	case d.Operations == nil:
		return nil, errors.New("v3 迁移：缺少管理操作仓储")
	case d.Lifecycle == nil:
		return nil, errors.New("v3 迁移：缺少状态文件升级端口")
	case d.Resources == nil:
		return nil, errors.New("v3 迁移：缺少资源仓储")
	case d.Notify == nil:
		return nil, errors.New("v3 迁移：缺少通知渠道仓储")
	case d.Bindings == nil:
		return nil, errors.New("v3 迁移：缺少绑定投影仓储")
	case d.Root == "":
		return nil, errors.New("v3 迁移：缺少工作区根")
	}
	agents := slices.Clone(d.Agents)
	if len(agents) == 0 {
		names, err := workspace.AgentNames(d.Root)
		if err != nil {
			return nil, fmt.Errorf("v3 迁移：扫描 Agent 目录: %w", err)
		}
		agents = names
	}
	slices.Sort(agents)
	return agents, nil
}

// logger 返回生效的日志器（nil 回落默认）。
// logger returns the effective logger.
func (d MigrateV3Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// now 返回注入时钟（nil 回落 time.Now）。
// now returns the injected clock.
func (d MigrateV3Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// target 返回目标 schema 版本（nil 语义：0 表示按契约取值）。
// target returns the target schema version.
func (d MigrateV3Deps) target() int {
	if d.Target > 0 {
		return d.Target
	}
	return storage.SupportedSchemaVersion
}
