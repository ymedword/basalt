package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 runs 的**读侧唯一 SQL 入口**（§3.1.18；约束 21 / 22 / 24）：列表的固定排序与
// 固定窗口、详情按 run_id 取行、`final_text` 从消息派生、资源清单读取并校验哈希。
// 三条纪律：① 排序是常量 SQL，请求只能给三个值谓词（会话 / Agent / 状态），**不得**派生
// `ORDER BY`；② `final_text` 不是账本列（`runs` 表冻结），一律按该 Run 最近一条 assistant
// 消息派生，**不得**新增列、不得反写消息；③ 历史 Run 没有清单行就是"未记录"，读路径
// 返回 nil 且**零写入**——不得用当前发布版本补造（约束 24）。
// This file is the read-side SQL entry point for runs: fixed ordering and window, detail by
// id, final text derived from the latest assistant message, and manifest verification.

// 列表窗口的缺省值与上限（约束 21）。handler 会先归一并对越界给 400；仓储这里再做
// 一次防御性收敛——仓储是最后一道，**不**信任调用方已经归一过。
// Default and maximum list windows; the repository clamps defensively.
const (
	defaultRunListLimit = 50
	maxRunListLimit     = 200
)

const (
	// runBaseColumns 是读侧**唯一**的列清单与投影（详情与列表共用，避免第二份列定义漂移）。
	// `manifest_hash` 走 LEFT JOIN：历史 Run 没有清单行 → 空串（约束 24），不是错误。
	// The single read-side column projection shared by detail and list; a missing manifest
	// row yields an empty hash rather than an error.
	runBaseColumns = `r.run_id, r.session_id, r.agent, r.channel, r.user_id, r.trigger,
		       r.status, r.reason, r.iterations, r.tool_calls, r.config_hash,
		       r.started_at, r.finished_at, COALESCE(m.manifest_hash, '')`

	// runFinalTextColumn 是**列表**的 final_text 派生列：该 Run 最近一条 assistant 消息
	// （按 `seq` 倒序取一条），没有则空串。详情走 finalText 单查（注 16 允许两种形态）。
	// The list's derived final_text column; the detail path uses the separate finalText query.
	runFinalTextColumn = `,
		       COALESCE((SELECT content FROM messages
		                  WHERE messages.run_id = r.run_id AND messages.role = 'assistant'
		                  ORDER BY messages.seq DESC LIMIT 1), '')`

	// runFromClause 是读侧唯一的 FROM 基座（清单 LEFT JOIN 只此一处）。
	// The single FROM base of the read side.
	runFromClause = `
		  FROM runs r
		  LEFT JOIN run_resource_manifests m ON m.run_id = r.run_id`

	// runListSelect / runDetailSelect 是常量拼接：可变的只有三个值谓词，排序与列面不动。
	// Constant concatenation: only value predicates vary; ordering and columns never do.
	runListSelect   = "SELECT " + runBaseColumns + runFinalTextColumn + runFromClause
	runDetailSelect = "SELECT " + runBaseColumns + runFromClause
)

// rowScanner 是 `*sql.Row` 与 `*sql.Rows` 的共同扫描面（两种读法共用同一行投影）。
// rowScanner is the shared scan surface of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// GetRun 取单条运行详情：账本行 + 终态列 + 最近一条 assistant 消息 + 准入清单。
// 行不存在返回 `core.ErrRunNotFound`（web 映射 404）；清单行缺失是历史 Run 的常态，
// 返回 nil 清单而**不是**错误（约束 24）。
// GetRun returns one run detail, or ErrRunNotFound when the row is absent. A missing
// manifest row is the normal shape of a legacy run, not an error.
func (s *RunStore) GetRun(ctx context.Context, runID string) (core.RunDetail, error) {
	row := s.db.read.QueryRowContext(ctx, runDetailSelect+` WHERE r.run_id = ?`, runID)
	detail, err := scanRunDetail(row, nil)
	if errors.Is(err, sql.ErrNoRows) {
		return core.RunDetail{}, core.ErrRunNotFound
	}
	if err != nil {
		return core.RunDetail{}, fmt.Errorf("读取运行详情（run_id=%s）: %w", runID, err)
	}

	text, err := finalText(ctx, s.db.read, runID)
	if err != nil {
		return core.RunDetail{}, err
	}
	detail.FinalText = text

	manifest, err := loadRunManifest(ctx, s.db.read, runID)
	if err != nil {
		return core.RunDetail{}, err
	}
	detail.ResourceManifest = manifest
	return detail, nil
}

// ListRuns 按固定排序 `started_at DESC, run_id DESC` 与固定窗口返回运行列表。
// 三个过滤参数都是值谓词（会话 / Agent / 状态），空串 = 不过滤；`Limit` 缺省 50、
// 上限 200。非法状态字面量由 handler 先判 400，仓储只做等值绑定——拼进 SQL 的只有
// 常量片段，任何请求值都走占位符。
// ListRuns returns runs in the fixed order and window; every request value binds as a
// placeholder and only constant fragments are concatenated.
func (s *RunStore) ListRuns(ctx context.Context, filter core.RunFilter) ([]core.RunDetail, error) {
	query := runListSelect
	conds := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if filter.SessionID != "" {
		conds = append(conds, "r.session_id = ?")
		args = append(args, filter.SessionID)
	}
	if filter.Agent != "" {
		conds = append(conds, "r.agent = ?")
		args = append(args, filter.Agent)
	}
	if filter.Status != "" {
		conds = append(conds, "r.status = ?")
		args = append(args, filter.Status)
	}
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	// 排序与窗口是常量（约束 21）：请求**不得**影响 ORDER BY。
	query += " ORDER BY r.started_at DESC, r.run_id DESC LIMIT ?"
	args = append(args, normalizeRunListLimit(filter.Limit))

	rows, err := s.db.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询运行列表: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 非 nil 空切片：HTTP 层把空窗口编码为 `[]` 而不是 `null`。
	out := make([]core.RunDetail, 0)
	for rows.Next() {
		var lastText sql.NullString
		detail, err := scanRunDetail(rows, &lastText)
		if err != nil {
			return nil, fmt.Errorf("读运行列表行: %w", err)
		}
		detail.FinalText = lastText.String
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("扫描运行列表: %w", err)
	}
	return out, nil
}

// CountRuns 返回系统概览的三个运行计数：运行中（**不**受时间窗限制）、近 24 小时、
// 近 24 小时失败。单条聚合 SQL：状态字面量经占位符绑定（请求 / 状态值一律不进 SQL
// 文本），窗口下界 `now-24h` 含端点。计数是账本读面，**零写入**。
// CountRuns answers the overview's three run counters in one aggregate query; the
// status literals bind as parameters and the 24h cutoff is inclusive.
func (s *RunStore) CountRuns(ctx context.Context, now time.Time) (running, last24h, failedLast24h int64, err error) {
	cutoff := now.Add(-24 * time.Hour).UnixMilli()
	err = s.db.read.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN started_at >= ? THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN started_at >= ? AND status = ? THEN 1 ELSE 0 END), 0)
		   FROM runs`,
		string(core.StatusRunning), cutoff, cutoff, string(core.StatusFailed)).
		Scan(&running, &last24h, &failedLast24h)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("统计运行概览计数: %w", err)
	}
	return running, last24h, failedLast24h, nil
}

// scanRunDetail 扫描一行读侧投影。`lastText` 为 nil 时扫描面**不含** final_text 派生列
// （详情路径随后用 finalText 单查）；非 nil 时列表 SQL 的派生列落进它。
// scanRunDetail scans one projection row; a nil lastText means the final_text column is
// absent from the query and will be filled by the caller.
func scanRunDetail(row rowScanner, lastText *sql.NullString) (core.RunDetail, error) {
	var (
		detail    core.RunDetail
		sessionID sql.NullString
		trigger   string
		status    string
		reason    sql.NullString
		configHsh sql.NullString
		startedAt int64
		finished  sql.NullInt64
	)
	dests := []any{
		&detail.RunID, &sessionID, &detail.Agent, &detail.Channel, &detail.UserID, &trigger,
		&status, &reason, &detail.Iterations, &detail.ToolCalls, &configHsh,
		&startedAt, &finished, &detail.ManifestHash,
	}
	if lastText != nil {
		dests = append(dests, lastText)
	}
	if err := row.Scan(dests...); err != nil {
		return core.RunDetail{}, err
	}

	// NULL 映射固定（契约注 18）：空会话 = 无状态调用；未收口的行没有 finished_at。
	detail.SessionID = sessionID.String
	detail.Trigger = core.Trigger(trigger)
	detail.Status = core.RunStatus(status)
	detail.Reason = reason.String
	detail.ConfigHash = configHsh.String
	detail.StartedAt = time.UnixMilli(startedAt).UTC()
	if finished.Valid {
		detail.FinishedAt = time.UnixMilli(finished.Int64).UTC()
	}
	return detail, nil
}

// finalText 取该 Run 最近一条 assistant 消息的正文（按 `seq` 倒序）；没有则空串。
// **不取**工具结果、**不取** system / user 消息，也**不跨 Run**——`run_id` 谓词是边界。
// finalText returns the content of the run's latest assistant message, or "" when none.
func finalText(ctx context.Context, q queryer, runID string) (string, error) {
	var text string
	err := q.QueryRowContext(ctx,
		`SELECT COALESCE(content, '')
		   FROM messages
		  WHERE run_id = ? AND role = 'assistant'
		  ORDER BY seq DESC LIMIT 1`, runID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("读取运行最终回答（run_id=%s）: %w", runID, err)
	}
	return text, nil
}

// loadRunManifest 读取并校验 Run 的资源清单：没有行 → `(nil, nil)`（历史 Run，约束 24）；
// 有行 → 解析 `snapshot_json` 后**重算哈希**并与 `manifest_hash` 比对，不一致返回错误。
// 校验失败**不得**静默降级为 nil——那会把"清单损坏"伪装成"历史 Run"。
// loadRunManifest returns (nil, nil) for a legacy run and verifies the recomputed hash
// against the stored manifest_hash otherwise; a mismatch is an error, never a silent nil.
func loadRunManifest(ctx context.Context, q queryer, runID string) (*core.RunResourceManifest, error) {
	var (
		snapshotJSON string
		wantHash     string
	)
	err := q.QueryRowContext(ctx,
		`SELECT snapshot_json, manifest_hash FROM run_resource_manifests WHERE run_id = ?`, runID).
		Scan(&snapshotJSON, &wantHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取运行资源清单（run_id=%s）: %w", runID, err)
	}

	var manifest core.RunResourceManifest
	if err := json.Unmarshal([]byte(snapshotJSON), &manifest); err != nil {
		return nil, fmt.Errorf("解析运行资源清单（run_id=%s）: %w", runID, err)
	}
	if err := verifyManifestBytes([]byte(snapshotJSON), wantHash, runID); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// verifyManifestBytes 用**已存储的字节**校验清单：写入时 `manifest_hash` 是「同一份
// JSON 把顶层 hash 值置空」后的 sha256，因此校验也必须回放同一定义，而不是把
// snapshot_json 解析进**当前**结构体再重新序列化——结构体新增字段（v1 → v2 增加
// agent_uid / provider.model / 凭据固定 / notify）会改变重序列化字节，把合法的历史行
// 误判成损坏（实测：v1 清单的 Run 详情 500）。
//
// `hash` 是结构体声明序的第二个字段（v1 / v2 都是），所以首个 `"hash":"` 就是顶层
// 字段；值只含小写十六进制，不存在转义。截取它的值后，剩余字节必须逐字节等于写入时
// 的哈希输入。篡改任何其它字节都会改变重算结果。
// verifyManifestBytes replays the writer's hash definition over the stored bytes.
// Re-marshalling through the current struct would reject every pre-v2 row once the
// struct grows new fields; verifying the stored bytes stays format-independent.
func verifyManifestBytes(raw []byte, wantHash, runID string) error {
	const hashField = `"hash":"`
	idx := bytes.Index(raw, []byte(hashField))
	if idx < 0 {
		return fmt.Errorf("运行资源清单缺少 hash 字段（run_id=%s）", runID)
	}
	start := idx + len(hashField)
	end := start + len(wantHash)
	if end >= len(raw) || string(raw[start:end]) != wantHash || raw[end] != '"' {
		return fmt.Errorf("运行资源清单哈希不一致（run_id=%s）: 库里 %s", runID, wantHash)
	}
	input := make([]byte, 0, len(raw)-len(wantHash))
	input = append(input, raw[:start]...)
	input = append(input, raw[end:]...)
	sum := sha256.Sum256(input)
	got := hex.EncodeToString(sum[:])
	if got != wantHash {
		return fmt.Errorf("运行资源清单哈希不一致（run_id=%s）: 库里 %s，重算 %s",
			runID, wantHash, got)
	}
	return nil
}

// normalizeRunListLimit 收敛窗口：缺省 50、上限 200、非正数按缺省（约束 21）。
// normalizeRunListLimit clamps the window to [1, 200] with a default of 50.
func normalizeRunListLimit(n int) int {
	switch {
	case n <= 0:
		return defaultRunListLimit
	case n > maxRunListLimit:
		return maxRunListLimit
	default:
		return n
	}
}
