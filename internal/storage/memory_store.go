package storage

// 本文件是 memory_entries 的**唯一 SQL 入口**（sqlite 档记忆载体）：六列 + idx_memory_scope
// 由 US-1 的 schema.sql 建齐——**零迁移**（§3.4）。纪律三条：① **写入入口与 markdown 档
// 同一规范化**（`strings.TrimSpace`，同一个标准库函数——约束 29 / P1-b），检查与存储不得
// 用两份表示；② **配额检查与追加同一事务**（约束 12）——判在 `withWriteLock` 内，拒绝时
// 连"先插一半"的可能都不留；③ **时间显式传入**（约束 25）——`created_at` 取自构造器时钟。
// This file is the only SQL entry point for memory_entries: the same normalization as the
// markdown carrier, quota and insert inside one transaction, and an injected clock.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// MemoryStore 是 memory_entries 的唯一 SQL 入口（`core.LongTermMemoryStore` 的 sqlite 档）。
// MemoryStore is the sqlite carrier: the only SQL entry point for memory_entries.
type MemoryStore struct {
	db  *DB
	now func() time.Time
}

// 编译期断言：sqlite 档与 markdown 档跑同一张场景表（约束 17）的前提是接口面一致。
var _ core.LongTermMemoryStore = (*MemoryStore)(nil)

// NewMemoryStore 绑定库句柄与**显式传入的时钟**（约束 25）：方法签名冻结无时间参数，
// 故时间走构造器——不得在方法体里散落 `time.Now()`，否则 `created_at` 不可判。
// NewMemoryStore binds the handle and an injected clock; time never comes from inside.
func NewMemoryStore(db *DB, now func() time.Time) *MemoryStore {
	return &MemoryStore{db: db, now: now}
}

// Append 向 scope 层的 section 分区追加一条条目。单条配额在事务外先判（判据不依赖库内
// 状态），总量配额在事务内与 INSERT 同域判定（约束 12）；两条都用**规范化后**的文本。
// Append checks the per-entry bound before the transaction and the section total inside it.
func (s *MemoryStore) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error {
	c := strings.TrimSpace(content) // 与 markdown 档 normalizeEntry 同一个标准库函数（约束 29）
	scopeCol, agentName := scopeColumns(scope)

	if section == core.SectionCore && len(c) > core.MaxCoreEntryBytes {
		return &core.MemoryQuotaError{Kind: core.QuotaKindEntry, Limit: core.MaxCoreEntryBytes}
	}

	return withWriteLock(ctx, s.db.write, func(ctx context.Context, conn *sql.Conn) error {
		if section == core.SectionCore {
			total, err := coreSectionBytes(ctx, conn, scopeCol, agentName)
			if err != nil {
				return err
			}
			if total+len(c) > core.MaxCoreSectionBytes {
				return &core.MemoryQuotaError{Kind: core.QuotaKindSection, Limit: core.MaxCoreSectionBytes}
			}
		}

		if _, err := conn.ExecContext(ctx,
			`INSERT INTO memory_entries (scope, agent_name, section, content, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			scopeCol, agentName, string(section), c, s.now().UnixMilli()); err != nil {
			return fmt.Errorf("追加记忆条目（%s/%s）: %w", scopeCol, section, err)
		}
		return nil
	})
}

// Load 渲染该层的规范文本（同形两分区，约束 17 / 口径 12）：与 markdown 档**不承诺**
// 逐字节相同，只承诺同一解析器可切出同一分区结构。零行 = 空层 → 空文本（与 markdown 档
// 的合法空层归一一致）。渲染形态漂移由 `TestMemoryStore_RenderedTextIsParseableBySharedParser`
// 用文档化形态的解析规则守住（README 口吻的原因：两档各写一份渲染器，形态一致靠判据钉）。
// Load renders the layer in the shared shape; zero rows render as an empty layer.
func (s *MemoryStore) Load(ctx context.Context, scope core.Scope) (string, error) {
	scopeCol, agentName := scopeColumns(scope)

	rows, err := s.db.read.QueryContext(ctx,
		`SELECT section, content FROM memory_entries
		  WHERE scope = ? AND agent_name IS ?
		  ORDER BY id`, scopeCol, agentName)
	if err != nil {
		return "", fmt.Errorf("读取记忆层（%s）: %w", scopeCol, err)
	}
	defer func() { _ = rows.Close() }() // 关闭错误不是判据：迭代错误由 rows.Err() 报（包内既有形态）

	var coreEntries, archiveEntries []string
	for rows.Next() {
		var (
			section string
			content string
		)
		if err := rows.Scan(&section, &content); err != nil {
			return "", fmt.Errorf("扫描记忆条目: %w", err)
		}
		switch core.Section(section) {
		case core.SectionCore:
			coreEntries = append(coreEntries, content)
		case core.SectionArchive:
			archiveEntries = append(archiveEntries, content)
		default:
			// 列取值域由写入面唯一决定（本文件是唯一 SQL 入口）；出现域外值说明库被外部
			// 改动过——按 fail-closed 上抛，不得静默丢条。
			return "", fmt.Errorf("读取记忆层（%s）: 分区取值域外：%q", scopeCol, section)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("遍历记忆条目（%s）: %w", scopeCol, err)
	}

	if len(coreEntries) == 0 && len(archiveEntries) == 0 {
		return "", nil // 空层
	}
	return renderMemoryText(scope, coreEntries, archiveEntries), nil
}

// RecallByKeyword 在**归档区**做整串子串检索：`instr` 是字节级匹配——不分词、不扩词、
// 不做大小写折叠；按 `id` 升序即写入顺序。未命中 = 空结果 + `nil` error（口径 10）。
// RecallByKeyword matches whole substrings over the archive in write order; a miss is a
// success rather than an error.
func (s *MemoryStore) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error) {
	scopeCol, agentName := scopeColumns(scope)

	rows, err := s.db.read.QueryContext(ctx,
		`SELECT content FROM memory_entries
		  WHERE scope = ? AND agent_name IS ? AND section = ? AND instr(content, ?) > 0
		  ORDER BY id`, scopeCol, agentName, string(core.SectionArchive), query)
	if err != nil {
		return nil, fmt.Errorf("检索记忆归档区（%s）: %w", scopeCol, err)
	}
	defer func() { _ = rows.Close() }()

	var hits []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return nil, fmt.Errorf("扫描召回条目: %w", err)
		}
		hits = append(hits, content)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历召回条目（%s）: %w", scopeCol, err)
	}
	return hits, nil
}

// scopeColumns 把分层映射成表的两列（§3.4）：全局层 `scope='global'` + `agent_name IS NULL`
// ——**不得**用空串顶替 NULL（查询恒用 `IS NULL`，空串会让取数静默返回零行）。nil 经
// `agent_name IS ?` 传下去正好落 NULL：SQLite 的 `IS` 对 NULL 与值都成立。
// scopeColumns maps the layer onto the two columns; the global layer is SQL NULL.
func scopeColumns(scope core.Scope) (scopeCol string, agentName any) {
	if scope.Agent == "" {
		return "global", nil
	}
	return "agent", scope.Agent
}

// coreSectionBytes 求该层核心区**存量**的字节总量（`octet_length` 按 UTF-8 字节，
// 与 markdown 档的 `len(正文)` 同一口径——约束 29）。必须在调用方的写事务内执行：
// 检查与 INSERT 同域才不会被并发写插队。
// coreSectionBytes sums the core region's bytes; it must run inside the caller's write
// transaction so the check and the insert share one atomic region.
func coreSectionBytes(ctx context.Context, conn *sql.Conn, scopeCol string, agentName any) (int, error) {
	var total int
	err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(octet_length(content)), 0) FROM memory_entries
		  WHERE scope = ? AND agent_name IS ? AND section = ?`,
		scopeCol, agentName, string(core.SectionCore)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("统计核心区配额（%s）: %w", scopeCol, err)
	}
	return total, nil
}

// renderMemoryText 渲染整层规范文本：文件标题 + 核心区 + 归档区，条目 = 零缩进 `"- "` 首行
// + 两空格缩进续行（约束 28 / 口径 22）。**两档各有一份渲染器**（本包不得 import
// `internal/memory`——载体是门面的下游端口实现，方向不得倒置），形态一致由
// `TestMemoryStore_RenderedTextIsParseableBySharedParser` 钉住。
// renderMemoryText renders the layer text in the documented shape.
func renderMemoryText(scope core.Scope, coreEntries, archiveEntries []string) string {
	var b strings.Builder
	b.WriteString(memoryFileTitle(scope))
	b.WriteString("\n\n")
	renderMemorySection(&b, "## 核心记忆", coreEntries)
	b.WriteString("\n\n")
	renderMemorySection(&b, "## 归档记忆", archiveEntries)
	b.WriteString("\n")
	return b.String()
}

// memoryFileTitle 是渲染用的文件标题：全局层取与 `templateMemoryMD` 同源的字面量，
// Agent 层取同构形态（与 `internal/memory` 的文件标题形态一致；判据只钳形态）。
func memoryFileTitle(scope core.Scope) string {
	if scope.Agent == "" {
		return "# MEMORY.md —— 长期记忆 · 全局层"
	}
	return "# MEMORY.md —— 长期记忆 · Agent 层（" + scope.Agent + "）"
}

// renderMemorySection 渲染一个分区：无条目时只剩标题行（空分区不放空行——重建幂等）。
func renderMemorySection(b *strings.Builder, heading string, entries []string) {
	b.WriteString(heading)
	for _, entry := range entries {
		b.WriteString("\n\n") // 标题与首条之间、条目之间都是空行（口径 22 的条目分隔）
		renderMemoryEntry(b, entry)
	}
}

// renderMemoryEntry 渲染一条条目：首行加 `"- "`，其余各行（含空行）加两空格缩进。
func renderMemoryEntry(b *strings.Builder, body string) {
	lines := strings.Split(body, "\n")
	b.WriteString("- ")
	b.WriteString(lines[0])
	for _, line := range lines[1:] {
		b.WriteString("\n")
		if line != "" {
			b.WriteString("  ")
			b.WriteString(line)
		}
	}
}
