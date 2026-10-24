package storage

// 本文件是 `agent_resource_bindings` 的唯一 SQL 入口（颗粒度文档 R4.6 / T114）。
//
// 这张表是 AGENT.md `resource_bindings` 的**投影**，不是写入口：
//   - 权威是定义文件；文件改了就整份重投影（同事务删旧增新），不做逐条补丁。
//   - 每行记 `content_hash`，回答"这份投影基于哪一版定义"；重建时可自证一致。
//   - 反查（资源 → 引用它的 Agent）是删除闸门的判据，按 `resource_uid` 走索引，
//     **不得**用"当前没有运行"之类的间接证据推断无引用。
//
// This file is the only SQL entry point for the binding projection: AGENT.md stays
// authoritative, projection is a full replace inside one transaction, and the reverse
// lookup by resource uid is what the delete gate reads.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// bindingColumns 是 agent_resource_bindings 的读取列序（scanBinding 按同一顺序扫描）。
// bindingColumns is the read order of the binding table.
const bindingColumns = `agent_uid, agent_name, resource_kind, ref_name, resource_uid, content_hash, projected_at`

// BindingStore 是 Agent 资源绑定投影仓储。
// BindingStore is the repository of the agent resource-binding projection.
type BindingStore struct{ db *DB }

// NewBindingStore 绑定一个已打开的写模式库句柄。
// NewBindingStore binds the store to an opened writable database handle.
func NewBindingStore(db *DB) *BindingStore { return &BindingStore{db: db} }

// Rebuild 用给定绑定**全量替换**某 Agent 的投影行：删旧与增新在同一事务，中途失败
// 回滚到旧投影（不会留下"删了一半"的中间态）。同一输入重复调用结果不变（幂等）。
// 定义里删掉的绑定在这里随之消失——否则反查会挡住一个已经没人引用的资源。
// Rebuild replaces one agent's projection rows in a single transaction; repeating the
// same input is a no-op and dropped bindings disappear.
func (s *BindingStore) Rebuild(ctx context.Context, agentUID core.AgentUID, agentName, contentHash string, bindings core.ResourceBindings, at time.Time) error {
	uid, entries, err := normalizeBindingProjection(agentUID, agentName, contentHash, bindings, at)
	if err != nil {
		return err
	}

	tx, err := s.db.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("重建资源绑定 %s: 开启事务: %w", uid, err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := deleteBindingsTx(ctx, tx, uid); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := insertBindingTx(ctx, tx, entry); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("重建资源绑定 %s: 提交: %w", uid, err)
	}
	return nil
}

// Clear 删除某 Agent 的全部投影行（Agent 删除 / 归档时用——没有定义文件就没有绑定）。
// Clear drops every projection row of one agent.
func (s *BindingStore) Clear(ctx context.Context, agentUID core.AgentUID) error {
	uid, err := core.ParseAgentUID(string(agentUID))
	if err != nil {
		return err
	}
	return deleteBindingsTx(ctx, s.db.write, uid)
}

// Bindings 按 Agent 读全部投影行，按 `(kind, ref_name)` 排序——顺序稳定，便于比对。
// Bindings reads every projection row of one agent in a stable order.
func (s *BindingStore) Bindings(ctx context.Context, agentUID core.AgentUID) ([]core.AgentResourceBinding, error) {
	uid, err := core.ParseAgentUID(string(agentUID))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+bindingColumns+` FROM agent_resource_bindings
		  WHERE agent_uid = ? ORDER BY resource_kind ASC, ref_name ASC`, string(uid))
	if err != nil {
		return nil, fmt.Errorf("列出资源绑定 %s: %w", uid, err)
	}
	defer func() { _ = rows.Close() }()
	return collectBindings(rows, uid)
}

// ReferencingAgents 按资源 UID 反查引用它的 Agent（删除闸门的判据）：
// 跨两个绑定类别一并返回，调用方按 `Kind` 自己筛。没有行 = 没有定义文件引用它。
// ReferencingAgents reverse-looks-up the agents that bind one resource uid.
func (s *BindingStore) ReferencingAgents(ctx context.Context, uid core.ResourceUID) ([]core.AgentResourceBinding, error) {
	canonical, err := core.ParseResourceUID(string(uid))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+bindingColumns+` FROM agent_resource_bindings
		  WHERE resource_uid = ? ORDER BY agent_name ASC, resource_kind ASC, ref_name ASC`,
		string(canonical))
	if err != nil {
		return nil, fmt.Errorf("反查资源绑定 %s: %w", canonical, err)
	}
	defer func() { _ = rows.Close() }()
	return collectBindings(rows, core.AgentUID(""))
}

// collectBindings 扫描结果集；`agentUID` 只用于错误文案。
// collectBindings scans a row set; the agent uid only labels errors.
func collectBindings(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, agentUID core.AgentUID,
) ([]core.AgentResourceBinding, error) {
	var out []core.AgentResourceBinding
	for rows.Next() {
		entry, err := scanBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描资源绑定 %s: %w", agentUID, err)
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历资源绑定 %s: %w", agentUID, err)
	}
	return out, nil
}

// deleteBindingsTx 是投影行删除的唯一 SQL（全量重建与 Clear 共用）。
// deleteBindingsTx is the single DELETE used by rebuild and clear.
func deleteBindingsTx(ctx context.Context, ex sqlExecutor, agentUID core.AgentUID) error {
	if _, err := ex.ExecContext(ctx,
		`DELETE FROM agent_resource_bindings WHERE agent_uid = ?`, string(agentUID)); err != nil {
		return fmt.Errorf("删除资源绑定 %s: %w", agentUID, err)
	}
	return nil
}

// insertBindingTx 是投影行插入的唯一 SQL。
// insertBindingTx is the single INSERT of the projection table.
func insertBindingTx(ctx context.Context, ex sqlExecutor, entry core.AgentResourceBinding) error {
	if _, err := ex.ExecContext(ctx,
		`INSERT INTO agent_resource_bindings
		   (agent_uid, agent_name, resource_kind, ref_name, resource_uid, content_hash, projected_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(entry.AgentUID), entry.AgentName, string(entry.Kind), entry.RefName,
		string(entry.ResourceUID), entry.ContentHash, entry.ProjectedAt); err != nil {
		return fmt.Errorf("写入资源绑定 %s/%s/%s: %w", entry.AgentUID, entry.Kind, entry.RefName, err)
	}
	return nil
}

// scanBinding 按 bindingColumns 的列序扫描一行。
// scanBinding scans one binding row.
func scanBinding(row interface{ Scan(...any) error }) (core.AgentResourceBinding, error) {
	var (
		entry core.AgentResourceBinding
		uid   string
		kind  string
		ref   string
	)
	err := row.Scan(&uid, &entry.AgentName, &kind, &ref, &entry.ResourceUID,
		&entry.ContentHash, &entry.ProjectedAt)
	if err != nil {
		return core.AgentResourceBinding{}, err
	}
	entry.AgentUID = core.AgentUID(uid)
	entry.Kind = core.BindingKind(kind)
	entry.RefName = ref
	return entry, nil
}

// normalizeBindingProjection 校验重建输入并摊平成行：Agent 身份合法、名称与内容哈希
// 非空、绑定结构合法（名称非空 / UID 合法 / 同类别不重复）。投影时间缺省为当下。
// 绑定与定义引用的**一一对应**由调用方在解析 AGENT.md 后校验——本函数只判行结构。
// normalizeBindingProjection validates the rebuild input; reference correspondence is
// the caller's job because it needs the parsed definition.
func normalizeBindingProjection(agentUID core.AgentUID, agentName, contentHash string, bindings core.ResourceBindings, at time.Time) (core.AgentUID, []core.AgentResourceBinding, error) {
	uid, err := core.ParseAgentUID(string(agentUID))
	if err != nil {
		return "", nil, err
	}
	if agentName == "" {
		return "", nil, errors.New("资源绑定: agent_name 不得为空")
	}
	if contentHash == "" {
		return "", nil, errors.New("资源绑定: content_hash 不得为空")
	}
	if err := bindings.Validate(); err != nil {
		return "", nil, err
	}
	if at.IsZero() {
		at = time.Now()
	}
	projectedAt := at.UnixMilli()

	entries := make([]core.AgentResourceBinding, 0,
		len(bindings.ModelConnections)+len(bindings.NotifyChannels))
	appendGroup := func(kind core.BindingKind, group []core.ResourceBinding) {
		for _, entry := range group {
			entries = append(entries, core.AgentResourceBinding{
				AgentUID:    uid,
				AgentName:   agentName,
				Kind:        kind,
				RefName:     entry.Name,
				ResourceUID: entry.UID,
				ContentHash: contentHash,
				ProjectedAt: projectedAt,
			})
		}
	}
	appendGroup(core.BindingModelConnection, bindings.ModelConnections)
	appendGroup(core.BindingNotifyChannel, bindings.NotifyChannels)
	return uid, entries, nil
}
