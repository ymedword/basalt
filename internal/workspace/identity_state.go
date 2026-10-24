package workspace

// 本文件承载状态文件 schema v2 的身份面（颗粒度文档 R4.4 / T109）：
// 给定稳定身份建初始状态、把 v1 文件原地升级为 v2、以及迁移用的批量升级入口。
// 所有写入仍只经 writeLifecycleStateAtomic（临时文件 + rename），本文件不建第二套
// 写路径；校验和与规范字节复用 lifecycle.go 的同一套函数。
// This file carries the identity side of the lifecycle state file schema v2: seeding
// a caller-supplied UID, upgrading v1 files in place, and the migration-facing batch
// entry point. Every write still goes through writeLifecycleStateAtomic.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/ymedword/basalt/internal/core"
)

// BootstrapIdentity 写「disabled / generation=1 / 指定 agent_uid」的初始状态文件：
// 归档恢复（原名与异名）都沿用原身份，**不得**另生成新 UID。文件已存在且身份一致 →
// 原样返回；身份不同 → core.ErrLifecycleConflict（不静默改身份）；损坏 → 报错不覆盖。
// **调用方必须已持有该 Agent 的写锁**（与 Bootstrap / Transition 同口径）。
// BootstrapIdentity seeds a disabled/generation=1 file with the caller's UID. A
// mismatching existing identity is a conflict, never a silent rewrite.
func (s *LifecycleStore) BootstrapIdentity(ctx context.Context, agent string, agentUID core.AgentUID, operationID string) (rec core.LifecycleRecord, err error) {
	if err := ctx.Err(); err != nil {
		return core.LifecycleRecord{}, err
	}
	if err := ValidateAgentName(agent); err != nil {
		return core.LifecycleRecord{}, err
	}
	if !agentUID.Valid() {
		return core.LifecycleRecord{}, fmt.Errorf("%w: Agent %s 的恢复身份格式非法：%q",
			core.ErrLifecycleConflict, agent, agentUID)
	}

	base, err := os.OpenRoot(s.root)
	if err != nil {
		return core.LifecycleRecord{}, fmt.Errorf("打开工作区目录 %s: %w", s.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", s.root, closeErr))
		}
	}()

	existing, exists, readErr := readLifecycleState(base, agent)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return core.LifecycleRecord{}, fmt.Errorf("恢复基线失败：Agent %s 的状态文件不可用，拒绝覆盖: %v", agent, readErr)
	}
	if exists {
		if existing.AgentUID != agentUID {
			return core.LifecycleRecord{}, fmt.Errorf("%w: Agent %s 的现存状态文件身份为 %q，与恢复身份 %q 不一致",
				core.ErrLifecycleConflict, agent, existing.AgentUID, agentUID)
		}
		return existing, nil
	}

	rec = core.LifecycleRecord{
		SchemaVersion: lifecycleSchemaVersion,
		Agent:         agent,
		AgentUID:      agentUID,
		State:         core.AgentDisabled,
		Generation:    1,
		UpdatedAt:     s.now().UnixMilli(),
		OperationID:   operationID,
	}
	checksum, _, err := lifecycleChecksum(rec)
	if err != nil {
		return core.LifecycleRecord{}, err
	}
	rec.Checksum = checksum
	data, err := encodeLifecycleState(rec)
	if err != nil {
		return core.LifecycleRecord{}, fmt.Errorf("生成 Agent %s 的状态文件字节: %w", agent, err)
	}
	if err := writeLifecycleStateAtomic(base, agent, data, s.renameFn()); err != nil {
		return core.LifecycleRecord{}, err
	}
	return rec, nil
}

// UpgradeIdentity 把 v1 状态文件原地升级为 v2：补发稳定身份，保留状态、代数与
// 管理操作归属，仅刷新 updated_at 与 operation_id（本次升级操作）。已是 v2 →
// 原样返回且 upgraded=false；文件缺失 → (zero, false, nil)（由调用方按
// `legacy_unrecorded` 记数据库镜像）；损坏 → 报错，不覆盖。
// UpgradeIdentity upgrades a v1 file to v2 by minting its identity and keeping the
// state and generation. A v2 file is returned unchanged; a missing file is reported
// as not-upgraded so callers can record legacy_unrecorded.
func (s *LifecycleStore) UpgradeIdentity(ctx context.Context, agent, operationID string) (rec core.LifecycleRecord, upgraded bool, err error) {
	if err := ctx.Err(); err != nil {
		return core.LifecycleRecord{}, false, err
	}
	if err := ValidateAgentName(agent); err != nil {
		return core.LifecycleRecord{}, false, err
	}

	base, err := os.OpenRoot(s.root)
	if err != nil {
		return core.LifecycleRecord{}, false, fmt.Errorf("打开工作区目录 %s: %w", s.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", s.root, closeErr))
		}
	}()

	current, exists, readErr := readLifecycleState(base, agent)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return core.LifecycleRecord{}, false, nil
		}
		return core.LifecycleRecord{}, false, readErr
	}
	if !exists {
		return core.LifecycleRecord{}, false, nil
	}
	if current.SchemaVersion == lifecycleSchemaVersion {
		return current, false, nil
	}

	agentUID, uidErr := core.NewAgentUID()
	if uidErr != nil {
		return core.LifecycleRecord{}, false, fmt.Errorf("生成 Agent %s 的稳定身份: %w", agent, uidErr)
	}
	next := current
	next.SchemaVersion = lifecycleSchemaVersion
	next.AgentUID = agentUID
	next.UpdatedAt = s.now().UnixMilli()
	next.OperationID = operationID

	checksum, _, err := lifecycleChecksum(next)
	if err != nil {
		return core.LifecycleRecord{}, false, err
	}
	next.Checksum = checksum
	data, err := encodeLifecycleState(next)
	if err != nil {
		return core.LifecycleRecord{}, false, fmt.Errorf("生成 Agent %s 的状态文件字节: %w", agent, err)
	}
	if err := writeLifecycleStateAtomic(base, agent, data, s.renameFn()); err != nil {
		return core.LifecycleRecord{}, false, err
	}
	return next, true, nil
}

// 编译期断言：身份升级入口与迁移编排的消费面一致。
// Compile-time assertion that the upgrade entry point matches the migration face.
var _ core.IdentityUpgradePort = (*LifecycleStore)(nil)
