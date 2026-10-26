package storage

// 本文件是 `network_domain_policy`（schema v5）的唯一 SQL 入口：单行表承载出网域名
// 白名单的**唯一权威源**。`basalt.yaml` 的同名键只在表内无行时作为首次种子；此后
// 运行期与重启都读这张表（US-12 R8 裁决 2）。
// This file is the only SQL entry point of network_domain_policy. The table is the
// single source of truth; the YAML key seeds it once and is never read again.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// NetworkPolicyStore 是 core.NetworkDomainPolicyStore 的 SQLite 实现。
// NetworkPolicyStore is the SQLite implementation of the consumer-declared port.
type NetworkPolicyStore struct{ db *DB }

// 编译期断言：实现满足消费方（core）声明的端口。
// Compile-time assertion that the store satisfies the consumer-declared port.
var _ core.NetworkDomainPolicyStore = (*NetworkPolicyStore)(nil)

// NewNetworkPolicyStore 绑定一个已打开的写模式库句柄。
// NewNetworkPolicyStore binds the store to an opened writable database handle.
func NewNetworkPolicyStore(db *DB) *NetworkPolicyStore { return &NetworkPolicyStore{db: db} }

// policyColumns 是读取策略行（含脱敏操作者）的固定投影：操作者只在审计里，本表
// 只存 `operation_id`——回填走 LEFT JOIN，种子行（空 operation_id）取空串。
// policyColumns is the fixed projection; the actor is joined from the audit row.
const policyColumns = `p.policy_scope, p.domains, p.revision, p.updated_at, p.operation_id,
	COALESCE(op.actor_subject, '')`

// validatePolicyScope 拒绝空作用域：作用域是行主键，空串会把"忘了传 scope"
// 变成一行名为空串的幽灵数据。
// validatePolicyScope refuses an empty scope so a missing argument cannot create a ghost row.
func validatePolicyScope(scope string) error {
	if scope == "" {
		return errors.New("network domain policy: scope 不得为空")
	}
	return nil
}

// Load 读取指定作用域的当前策略行；无行时 exists=false（调用方据此决定是否种子）。
// Load reads the row of one scope; exists=false when that scope still has no row.
func (s *NetworkPolicyStore) Load(ctx context.Context, scope string) (core.NetworkDomainPolicy, bool, error) {
	if err := validatePolicyScope(scope); err != nil {
		return core.NetworkDomainPolicy{}, false, err
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+policyColumns+`
		   FROM network_domain_policy p
		   LEFT JOIN management_operations op ON op.operation_id = p.operation_id
		  WHERE p.policy_scope = ?`, scope)
	policy, err := scanPolicy(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return core.NetworkDomainPolicy{}, false, nil
	}
	if err != nil {
		return core.NetworkDomainPolicy{}, false, fmt.Errorf("load network domain policy %q: %w", scope, err)
	}
	return policy, true, nil
}

// Seed 在目标作用域无行时写入种子；已有行时原样返回既有行（幂等，**不**覆盖管理台
// 写入的值）。两个作用域各自独立：给禁止域名表种子不会动白名单那行。
// Seed inserts the config seed only when that scope is empty; an existing row wins.
func (s *NetworkPolicyStore) Seed(ctx context.Context, scope string, domains []string, now time.Time,
) (core.NetworkDomainPolicy, error) {
	if err := validatePolicyScope(scope); err != nil {
		return core.NetworkDomainPolicy{}, err
	}
	if _, err := s.db.write.ExecContext(ctx,
		`INSERT INTO network_domain_policy
		   (policy_scope, domains, revision, updated_at, operation_id)
		 VALUES (?, ?, 1, ?, '')
		 ON CONFLICT(policy_scope) DO NOTHING`,
		scope, joinPolicyDomains(domains), now.UnixMilli()); err != nil {
		return core.NetworkDomainPolicy{}, fmt.Errorf("seed network domain policy %q: %w", scope, err)
	}
	policy, exists, err := s.Load(ctx, scope)
	if err != nil {
		return core.NetworkDomainPolicy{}, err
	}
	if !exists {
		// 插入成功却读不到：读池与写池指向同一文件，正常装配下不可达；仍如实报错。
		return core.NetworkDomainPolicy{}, fmt.Errorf("network domain policy row %q missing after seeding", scope)
	}
	return policy, nil
}

// Update 以期望修订号做 CAS：落后 / 超前的期望值一律返回 `ErrPolicyRevisionConflict`
// 且零写入。更新与回读在同一事务里完成，返回的行必然就是这次写入的结果。
// Update applies a revision CAS and re-reads inside the same transaction.
func (s *NetworkPolicyStore) Update(ctx context.Context, scope string, domains []string, expectedRevision int64,
	operationID string, actor string, now time.Time,
) (core.NetworkDomainPolicy, error) {
	if err := validatePolicyScope(scope); err != nil {
		return core.NetworkDomainPolicy{}, err
	}
	if expectedRevision < 1 {
		return core.NetworkDomainPolicy{}, fmt.Errorf("%w: expected revision %d is invalid",
			core.ErrPolicyRevisionConflict, expectedRevision)
	}
	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return core.NetworkDomainPolicy{}, fmt.Errorf("begin network domain policy write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`UPDATE network_domain_policy
		    SET domains = ?, revision = revision + 1, updated_at = ?, operation_id = ?
		  WHERE policy_scope = ? AND revision = ?`,
		joinPolicyDomains(domains), now.UnixMilli(), operationID,
		scope, expectedRevision)
	if err != nil {
		return core.NetworkDomainPolicy{}, fmt.Errorf("update network domain policy %q: %w", scope, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return core.NetworkDomainPolicy{}, fmt.Errorf("inspect network domain policy write result: %w", err)
	}
	if affected == 0 {
		return core.NetworkDomainPolicy{}, fmt.Errorf("%w: expected revision %d does not match the current row",
			core.ErrPolicyRevisionConflict, expectedRevision)
	}
	row := tx.QueryRowContext(ctx,
		`SELECT `+policyColumns+`
		   FROM network_domain_policy p
		   LEFT JOIN management_operations op ON op.operation_id = p.operation_id
		  WHERE p.policy_scope = ?`, scope)
	policy, err := scanPolicy(row.Scan)
	if err != nil {
		return core.NetworkDomainPolicy{}, fmt.Errorf("re-read network domain policy %q: %w", scope, err)
	}
	// 操作者是本次调用的入参（刚写入的 operation_id 对应的审计行可能尚未提交完成读写
	// 可见性差异），以入参为准，避免把空串投影给页面。
	policy.UpdatedBy = actor
	if err := tx.Commit(); err != nil {
		return core.NetworkDomainPolicy{}, fmt.Errorf("commit network domain policy write: %w", err)
	}
	return policy, nil
}

// scanPolicy 解出策略行：domains 是换行连接的规整域名（空串 = 空列表）。
// scanPolicy decodes one row; an empty domains string is an empty list.
func scanPolicy(scan func(...any) error) (core.NetworkDomainPolicy, error) {
	var (
		policy    core.NetworkDomainPolicy
		domains   string
		updatedAt int64
	)
	if err := scan(&policy.Scope, &domains, &policy.Revision, &updatedAt,
		&policy.OperationID, &policy.UpdatedBy); err != nil {
		return core.NetworkDomainPolicy{}, err
	}
	policy.Domains = splitPolicyDomains(domains)
	policy.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return policy, nil
}

// joinPolicyDomains 把规整域名按行连接；空列表写空串。
// joinPolicyDomains renders the normalized list as newline-joined text.
func joinPolicyDomains(domains []string) string { return strings.Join(domains, "\n") }

// splitPolicyDomains 还原域名列表；空串得到空的非 nil 切片（视图按 `[]` 输出）。
// splitPolicyDomains parses the stored text into a non-nil slice.
func splitPolicyDomains(text string) []string {
	out := []string{}
	for _, entry := range strings.Split(text, "\n") {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}
