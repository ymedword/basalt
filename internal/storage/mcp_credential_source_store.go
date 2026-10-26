package storage

// 本文件是 `mcp_credential_sources` 的唯一 SQL 入口（14D / 设计 §2.3.2）：每个 MCP
// 占位符一行，声明它的凭据来源是 `managed` 还是 `env`。
//
// 为什么单列一张表而不是"托管行存在即 managed"：撤销会把那行变成不可用，若靠存在性
// 推断来源，撤销就会**隐式回落**进程环境——撤了还在用。来源进表之后，撤销只是把
// 托管值变成不可用，来源声明仍是 `managed`，解析必然 miss。
// This file is the only SQL entry point for per-placeholder MCP credential sources.
// Keeping the source in its own table is what stops a revoke from silently falling
// back to the process environment.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// mcpCredentialSourceColumns 是 `mcp_credential_sources` 的读取列序（scan 按同一顺序）。
// mcpCredentialSourceColumns is the read order of the source table.
// #nosec G101 -- SQL 列序常量，值不是凭据（变量名命中）。
const mcpCredentialSourceColumns = `server_uid, binding_key, mode, revision, updated_at, operation_id`

// MCPCredentialSourceStore 是 MCP 占位符凭据来源仓储。
// MCPCredentialSourceStore is the repository of per-placeholder credential sources.
type MCPCredentialSourceStore struct{ db *DB }

// NewMCPCredentialSourceStore 绑定一个已打开的写模式库句柄。
// NewMCPCredentialSourceStore binds the store to an opened database handle.
func NewMCPCredentialSourceStore(db *DB) *MCPCredentialSourceStore {
	return &MCPCredentialSourceStore{db: db}
}

// Upsert 写入一条来源声明：首写 `revision = 1`，其后每次成功写入 +1（与
// `execution_policy` / `network_exceptions` 的修订号同口径）。来源变更必须留下是哪次
// 管理操作改的，所以 `operation_id` 必填。
// Upsert records one declaration; revision starts at 1 and grows on every successful
// write, like the other revisioned tables. The producing operation is mandatory.
func (s *MCPCredentialSourceStore) Upsert(ctx context.Context, source core.MCPCredentialSource, at time.Time) error {
	normalized, err := normalizeMCPCredentialSource(source)
	if err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now()
	}
	if _, err := s.db.write.ExecContext(ctx,
		`INSERT INTO mcp_credential_sources
		   (server_uid, binding_key, mode, revision, updated_at, operation_id)
		 VALUES (?, ?, ?, 1, ?, ?)
		 ON CONFLICT(server_uid, binding_key) DO UPDATE SET
		   mode = excluded.mode,
		   revision = mcp_credential_sources.revision + 1,
		   updated_at = excluded.updated_at,
		   operation_id = excluded.operation_id`,
		string(normalized.ServerUID), normalized.BindingKey, string(normalized.Mode),
		at.UnixMilli(), normalized.OperationID); err != nil {
		return fmt.Errorf("写入 MCP %s 的凭据来源 %s: %w",
			normalized.ServerUID, normalized.BindingKey, err)
	}
	return nil
}

// BackfillEnvSources 给**没有声明**的占位符补记 `mode = env`，已有声明一律不动
// （`ON CONFLICT DO NOTHING`）。这是存量配置的一次性迁移：v5 的 MCP 凭据只有进程
// 环境一条路，补记的是"当前真实来源"，不是运行时回落——补记之后改 managed 再撤销，
// 仍然不会退回 env。返回实际补记的条数，供启动日志留证。
// BackfillEnvSources records mode=env for placeholders that have no declaration yet,
// never touching an explicit one. It records the current fact rather than allowing a
// runtime fallback, and returns how many rows it added for the startup log.
func (s *MCPCredentialSourceStore) BackfillEnvSources(ctx context.Context,
	serverUID core.ResourceUID, keys []string, operationID string, at time.Time,
) (int, error) {
	if _, err := core.ParseResourceUID(string(serverUID)); err != nil {
		return 0, err
	}
	if operationID == "" {
		return 0, errors.New("补记 MCP 凭据来源: operation_id 不能为空")
	}
	if at.IsZero() {
		at = time.Now()
	}
	added := 0
	for _, raw := range keys {
		key, ok := core.MCPBindingKey(raw)
		if !ok {
			return added, fmt.Errorf("补记 MCP %s 的凭据来源: 非法槽位名（env 键名或头名）", serverUID)
		}
		result, err := s.db.write.ExecContext(ctx,
			`INSERT INTO mcp_credential_sources
			   (server_uid, binding_key, mode, revision, updated_at, operation_id)
			 VALUES (?, ?, 'env', 1, ?, ?)
			 ON CONFLICT(server_uid, binding_key) DO NOTHING`,
			string(serverUID), key, at.UnixMilli(), operationID)
		if err != nil {
			return added, fmt.Errorf("补记 MCP %s 的凭据来源 %s: %w", serverUID, key, err)
		}
		if affected, err := result.RowsAffected(); err == nil && affected > 0 {
			added++
		}
	}
	return added, nil
}

// Get 读一条来源声明；没有声明 → `ok = false`（调用方按"要求显式选择"处理）。
// Get reads one declaration; ok=false means undeclared.
func (s *MCPCredentialSourceStore) Get(ctx context.Context,
	serverUID core.ResourceUID, bindingKey string,
) (core.MCPCredentialSource, bool, error) {
	uid, err := core.ParseResourceUID(string(serverUID))
	if err != nil {
		return core.MCPCredentialSource{}, false, err
	}
	key, ok := core.MCPBindingKey(bindingKey)
	if !ok {
		return core.MCPCredentialSource{}, false, errors.New("读取 MCP 凭据来源: 非法槽位名（env 键名或头名）")
	}
	row := s.db.read.QueryRowContext(ctx,
		`SELECT `+mcpCredentialSourceColumns+` FROM mcp_credential_sources
		  WHERE server_uid = ? AND binding_key = ?`, string(uid), key)
	rec, err := scanMCPCredentialSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.MCPCredentialSource{}, false, nil
	}
	if err != nil {
		return core.MCPCredentialSource{}, false,
			fmt.Errorf("读取 MCP %s 的凭据来源 %s: %w", uid, key, err)
	}
	return rec, true, nil
}

// List 按绑定键升序列出一个 server 的全部来源声明；没有声明返回空切片（不是 nil——
// 调用方可以无差别 range）。
// List returns every declaration of one server in binding-key order.
func (s *MCPCredentialSourceStore) List(ctx context.Context, serverUID core.ResourceUID) ([]core.MCPCredentialSource, error) {
	uid, err := core.ParseResourceUID(string(serverUID))
	if err != nil {
		return nil, err
	}
	rows, err := s.db.read.QueryContext(ctx,
		`SELECT `+mcpCredentialSourceColumns+` FROM mcp_credential_sources
		  WHERE server_uid = ? ORDER BY binding_key ASC`, string(uid))
	if err != nil {
		return nil, fmt.Errorf("列出 MCP %s 的凭据来源: %w", uid, err)
	}
	defer func() { _ = rows.Close() }()

	out := []core.MCPCredentialSource{}
	for rows.Next() {
		rec, scanErr := scanMCPCredentialSource(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("扫描 MCP %s 的凭据来源: %w", uid, scanErr)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 MCP %s 的凭据来源: %w", uid, err)
	}
	return out, nil
}

// scanMCPCredentialSource 按 mcpCredentialSourceColumns 的列序扫描一行。
// scanMCPCredentialSource scans one source row.
func scanMCPCredentialSource(row interface{ Scan(...any) error }) (core.MCPCredentialSource, error) {
	var (
		rec    core.MCPCredentialSource
		server string
		mode   string
	)
	if err := row.Scan(&server, &rec.BindingKey, &mode, &rec.Revision, &rec.UpdatedAt,
		&rec.OperationID); err != nil {
		return core.MCPCredentialSource{}, err
	}
	rec.ServerUID = core.ResourceUID(server)
	rec.Mode = core.CredentialMode(mode)
	return rec, nil
}

// normalizeMCPCredentialSource 校验写入形状：UID / 占位符名 / 两值模式 / 操作号齐备。
// normalizeMCPCredentialSource validates the shape before the schema CHECK sees it.
func normalizeMCPCredentialSource(source core.MCPCredentialSource) (core.MCPCredentialSource, error) {
	uid, err := core.ParseResourceUID(string(source.ServerUID))
	if err != nil {
		return core.MCPCredentialSource{}, err
	}
	source.ServerUID = uid
	key, ok := core.MCPBindingKey(source.BindingKey)
	if !ok {
		return core.MCPCredentialSource{}, errors.New("MCP 凭据来源: binding_key 必须是槽位名（env 键名或头名）")
	}
	source.BindingKey = key
	mode, err := core.ParseCredentialMode(string(source.Mode))
	if err != nil {
		return core.MCPCredentialSource{}, err
	}
	source.Mode = mode
	if strings.TrimSpace(source.OperationID) == "" {
		return core.MCPCredentialSource{}, errors.New("MCP 凭据来源: operation_id 不能为空")
	}
	return source, nil
}
