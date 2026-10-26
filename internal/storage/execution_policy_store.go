package storage

// 本文件是 `execution_policy`（schema v6）的唯一 SQL 入口：单行表承载执行档位的
// **唯一权威源**。`basalt.yaml` 只在表内无行时作首次种子；新建实例由初始化流程写
// `standard`，v5 → v6 迁移写 `strict`（迁移脚本内固定字面量，不读 YAML、不反推白名单）。
// This file is the only SQL entry point of execution_policy. The row is authoritative;
// config seeds it once, the migration writes strict, and instance init writes standard.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// executionPolicyScope 是单行表的固定主键（schema CHECK 也是这个字面量）。
// executionPolicyScope is the fixed primary key of the single row.
const executionPolicyScope = "global"

// ExecutionPolicyStore 是 core.ExecutionPolicyStore 的 SQLite 实现。
// ExecutionPolicyStore is the SQLite implementation of the consumer-declared port.
type ExecutionPolicyStore struct{ db *DB }

// 编译期断言：实现满足消费方（core）声明的端口。
// Compile-time assertion that the store satisfies the consumer-declared port.
var _ core.ExecutionPolicyStore = (*ExecutionPolicyStore)(nil)

// NewExecutionPolicyStore 绑定一个已打开的写模式库句柄。
// NewExecutionPolicyStore binds the store to an opened writable database handle.
func NewExecutionPolicyStore(db *DB) *ExecutionPolicyStore { return &ExecutionPolicyStore{db: db} }

// executionPolicyColumns 是读取档位行的固定投影：写入者只在审计行里（本表只存
// operation_id），回填走 LEFT JOIN；迁移 / 初始化行没有审计行，取空串。
// The fixed projection; the actor is joined from the audit row.
const executionPolicyColumns = `p.profile, p.revision, p.updated_at, p.operation_id,
	COALESCE(op.actor_subject, '')`

// Load 读取档位行；无行时 exists=false（调用方据此决定是否写入种子）。
// Load reads the row; exists=false when the table is still empty.
func (s *ExecutionPolicyStore) Load(ctx context.Context) (core.ExecutionPolicy, bool, error) {
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+executionPolicyColumns+`
		   FROM execution_policy p
		   LEFT JOIN management_operations op ON op.operation_id = p.operation_id
		  WHERE p.policy_scope = ?`, executionPolicyScope)
	policy, err := scanExecutionPolicy(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ExecutionPolicy{}, false, nil
	}
	if err != nil {
		return core.ExecutionPolicy{}, false, fmt.Errorf("load execution policy: %w", err)
	}
	return policy, true, nil
}

// Seed 只在表内无行时写入种子并返回落库行；已有行时原样返回（幂等，**不**覆盖管理台
// 写入的值）。`operationID` 是系统流程标识（迁移 / 实例初始化），不是管理台写。
// Seed inserts the system-flow row only when the table is empty; an existing row wins.
func (s *ExecutionPolicyStore) Seed(ctx context.Context, profile core.ExecutionProfile,
	operationID string, now time.Time,
) (core.ExecutionPolicy, error) {
	if !profile.Valid() {
		return core.ExecutionPolicy{}, fmt.Errorf("seed execution policy: invalid profile %q", profile)
	}
	if operationID == "" {
		return core.ExecutionPolicy{}, errors.New("seed execution policy: operation id 不得为空")
	}
	if _, err := s.db.write.ExecContext(ctx,
		`INSERT INTO execution_policy
		   (policy_scope, profile, revision, updated_at, operation_id)
		 VALUES (?, ?, 1, ?, ?)
		 ON CONFLICT(policy_scope) DO NOTHING`,
		executionPolicyScope, string(profile), now.UnixMilli(), operationID); err != nil {
		return core.ExecutionPolicy{}, fmt.Errorf("seed execution policy: %w", err)
	}
	policy, exists, err := s.Load(ctx)
	if err != nil {
		return core.ExecutionPolicy{}, err
	}
	if !exists {
		// 插入成功却读不到：读池与写池指向同一文件，正常装配下不可达；仍如实报错。
		return core.ExecutionPolicy{}, errors.New("execution policy row missing after seeding")
	}
	return policy, nil
}

// Update 以期望修订号做 CAS：落后 / 超前的期望值一律返回 `ErrPolicyRevisionConflict`
// 且零写入。更新与回读在同一事务里完成，返回的行必然就是这次写入的结果。
// Update applies a revision CAS and re-reads inside the same transaction.
func (s *ExecutionPolicyStore) Update(ctx context.Context, profile core.ExecutionProfile,
	expectedRevision int64, operationID, actor string, now time.Time,
) (core.ExecutionPolicy, error) {
	if !profile.Valid() {
		return core.ExecutionPolicy{}, fmt.Errorf("update execution policy: invalid profile %q", profile)
	}
	if expectedRevision < 1 {
		return core.ExecutionPolicy{}, fmt.Errorf("%w: expected revision %d is invalid",
			core.ErrPolicyRevisionConflict, expectedRevision)
	}
	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return core.ExecutionPolicy{}, fmt.Errorf("begin execution policy write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE execution_policy
		    SET profile = ?, revision = revision + 1, updated_at = ?, operation_id = ?
		  WHERE policy_scope = ? AND revision = ?`,
		string(profile), now.UnixMilli(), operationID, executionPolicyScope, expectedRevision)
	if err != nil {
		return core.ExecutionPolicy{}, fmt.Errorf("update execution policy: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return core.ExecutionPolicy{}, fmt.Errorf("inspect execution policy write result: %w", err)
	}
	if affected == 0 {
		return core.ExecutionPolicy{}, fmt.Errorf("%w: expected revision %d does not match the current row",
			core.ErrPolicyRevisionConflict, expectedRevision)
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+executionPolicyColumns+`
		   FROM execution_policy p
		   LEFT JOIN management_operations op ON op.operation_id = p.operation_id
		  WHERE p.policy_scope = ?`, executionPolicyScope)
	policy, err := scanExecutionPolicy(row.Scan)
	if err != nil {
		return core.ExecutionPolicy{}, fmt.Errorf("re-read execution policy: %w", err)
	}
	// 操作者是本次调用的入参：刚写入的审计行可能与读池的可见性有先后，以入参为准。
	policy.UpdatedBy = actor
	if err := tx.Commit(); err != nil {
		return core.ExecutionPolicy{}, fmt.Errorf("commit execution policy write: %w", err)
	}
	return policy, nil
}

// scanExecutionPolicy 解出一行档位。
// scanExecutionPolicy decodes one row.
func scanExecutionPolicy(scan func(...any) error) (core.ExecutionPolicy, error) {
	var (
		policy    core.ExecutionPolicy
		profile   string
		updatedAt int64
	)
	if err := scan(&profile, &policy.Revision, &updatedAt, &policy.OperationID, &policy.UpdatedBy); err != nil {
		return core.ExecutionPolicy{}, err
	}
	policy.Profile = core.ExecutionProfile(profile)
	policy.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return policy, nil
}
