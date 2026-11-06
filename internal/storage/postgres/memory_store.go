package postgres

// 本文件是 memory_entries 的唯一 SQL 入口，也是 memory_embeddings 的唯一**读**入口
// （向量写入与缺口判定在 `memory_embedding_index.go`：写后补索引与启动期回填，V-5）。
// PostgreSQL 谱系（`schema.pg.sql` v1）的十二列、三个索引与 `memory_embeddings` 由 US-16 建齐。
// 纪律五条：
//   - ① **生效层判据唯一**：`state = 'active' AND (valid_to IS NULL OR valid_to > now)`
//     是注入（Load）、关键词召回、语义召回与核心区配额共用的同一句话。漏一处就会让
//     候选 / 已删 / 到期条目进入模型上下文——这正是 T052 要修的缺陷面。
//   - ② 写入入口与 markdown 档同一规范化（`strings.TrimSpace`，约束 29 / P1-b）：
//     检查与存储不得用两份表示。
//   - ③ 配额检查与追加同一事务（约束 12）：判在 `withTx` 内，拒绝时连"先插一半"都
//     不留。
//   - ④ 时间显式传入（约束 25）：`created_at` 取自构造器时钟，方法体不出现
//     `time.Now()`。
//   - ⑤ 生命周期六态只由本文件写：`InsertEntry` 不改调用方给定的 state；`ReplaceEntry`
//     在单事务内先退役旧条再插新条——**不得**出现两条 active 头。
// This file is the only SQL entry point for memory_entries and the only read path for
// memory_embeddings: one effective-layer predicate shared by every read path, the same
// write normalization as the markdown carrier, quota and insert inside one transaction,
// an injected clock, and single-transaction lifecycle writes.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// 记忆条目的生命周期字面量（与 `schema.pg.sql` 的 CHECK 闭集逐字同源）；本包**不得**
// 扩集——加一个状态要同时改契约与 `internal/memory` 的生命周期，属软门禁。
// The lifecycle literals mirror the CHECK constraint verbatim.
const (
	memoryStateCandidate  = "candidate"
	memoryStateActive     = "active"
	memoryStateSuperseded = "superseded"
	memoryStateCorrected  = "corrected"
	memoryStateDeleted    = "deleted"
	memoryStateConflicted = "conflicted"
)

// 来源字面量（与 `memory_entries.source` 的 CHECK 闭集逐字同源）。前两个由本文件按状态
// 推导，后两个只接受调用方经 `memory.WithSource` 显式带入的受信任来源（旧记忆导入 /
// 迁移作业）；本文件**不**自行编造这两种值。
// The source literals mirror the CHECK constraint; the two import values are only ever
// honoured when a trusted caller supplies them explicitly.
const (
	memorySourceSaveMemory      = "save_memory"
	memorySourceExplicitPropose = "explicit_propose"
	memorySourceLegacyImport    = "legacy_import"
	memorySourceMigration       = "migration"
	// memorySourceAutoExtract 是受控自我进化提交事务写入的候选来源（US-22）；它只在
	// CommitExtraction 的受信任路径与本文件回读还原时出现，普通 Append / Propose 没有
	// 来源入参，无法伪装。
	// memorySourceAutoExtract is the trusted commit path's source literal from US-22.
	memorySourceAutoExtract = "auto_extract"
)

// memoryEntrySource 结算入库来源：条目显式带了来源（旧记忆导入 / 迁移作业 / 回读还原）
// 就照用，空串才按状态推导。**不得**反过来把显式来源覆盖成 `save_memory`——那会把导入
// 行降级成普通写入，D-18 的"导入 / 跳过 / 不可读逐条对账"随之失去载体。
// 闭集外的来源直接拒绝：库里 CHECK 也会挡，但在 Go 侧先报出可读错误。
// memoryEntrySource honours an explicit source and only derives one when absent.
func memoryEntrySource(entry memory.MemoryEntry) (string, error) {
	source := entry.Source()
	if source == "" {
		source = memorySourceForState(entry.State)
	}
	switch source {
	case memorySourceSaveMemory, memorySourceExplicitPropose,
		memorySourceLegacyImport, memorySourceMigration, memorySourceAutoExtract:
		return source, nil
	}
	return "", fmt.Errorf("记忆条目来源 %q 不在闭集内", source)
}

// 召回 / 生命周期路径的固定错误。Generation 类错误一律 fail-closed：缺代际、模型不符
// 或维度不符时**不得**退化成空结果或关键词路径——那会让"向量检索坏了"表现成"没有
// 相关记忆"，把装配故障藏进一次看起来正常的运行里。
// Recall fails closed on a missing active generation or a vector/model mismatch; an empty
// result must never stand in for a broken vector path.
var (
	errMemoryGenerationMissing = errors.New("postgres: 记忆召回缺少 active 索引代际")
	errMemoryModelMismatch     = errors.New("postgres: 记忆召回向量模型与 active 代际不符")
	errMemoryDimensionMismatch = errors.New("postgres: 记忆召回向量维度与 active 代际不符")
	errMemoryEntryNotFound     = errors.New("postgres: 记忆条目不存在")
)

// MemoryStore 是 memory_entries 的唯一 SQL 入口（`memory.MemoryStore` 的 PostgreSQL
// 实现）：兼容三方法（Append / Load / RecallByKeyword）+ 语义召回 + 五个生命周期动作。
// MemoryStore is the PostgreSQL implementation of memory.MemoryStore.
type MemoryStore struct {
	db  *DB
	now func() time.Time
}

// 编译期断言：门面（`internal/memory`）只依赖端口，不依赖本包；接口面一旦漂移，
// 这里先红，而不是等到装配层。
var (
	_ core.LongTermMemoryStore = (*MemoryStore)(nil)
	_ memory.MemoryStore       = (*MemoryStore)(nil)
)

// NewMemoryStore 绑定库句柄与**显式传入的时钟**（约束 25）：方法签名冻结无时间参数，
// 故时间走构造器——不得在方法体里散落 `time.Now()`，否则 `created_at` 与 `valid_to`
// 判据都不可复现。
// NewMemoryStore binds the handle and an injected clock; time never comes from inside.
func NewMemoryStore(db *DB, now func() time.Time) *MemoryStore {
	return &MemoryStore{db: db, now: now}
}

// memoryRow 是一条待写入的记忆行：Append / InsertEntry / ReplaceEntry 共用同一段
// INSERT，列与参数的对应关系只写一次（少写一列就是 NOT NULL 违约，T052 之前
// `Append` 缺 `valid_from` / `content_hash` 正是这个形态）。
// memoryRow feeds the single INSERT shared by append, insert and replace.
type memoryRow struct {
	scopeCol     string
	agentName    any
	section      core.Section
	content      string
	state        string
	source       string
	createdAt    int64
	supersedesID int64
	// validTo 是条目到期时刻（Unix 毫秒）；0 = 无到期，落库写 NULL。既有 Append /
	// InsertEntry / ReplaceEntry 传零值，行为与 US-22 之前完全一致。
	// validTo lands as NULL when zero, so existing write paths are unchanged.
	validTo int64
	// confirmedBy 是确认 / 纠正时记录的确认人；空串落 NULL（候选、导入行与旧数据
	// 本来就没有确认人，用空串顶替 NULL 会让"从未确认"与"确认人是空串"无法区分）。
	// confirmedBy lands as NULL when empty, keeping "never recorded" distinct.
	confirmedBy string
}

// Append 向 scope 层的 section 分区追加一条条目。单条配额在事务外先判（判据不依赖库内
// 状态），总量配额在事务内与 INSERT 同域判定（约束 12）；两条都用**规范化后**的文本。
// Append 写入的是生效条目：`state='active'`、`source='save_memory'`、`valid_from` 等于
// `created_at`——兼容面没有状态参数，能走到这里的就是"保存即生效"。
// Append checks the per-entry bound before the transaction and the section total inside it.
func (s *MemoryStore) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error {
	c := strings.TrimSpace(content) // 与 markdown 档 normalizeEntry 同一个标准库函数（约束 29）
	scopeCol, agentName := scopeColumns(scope)

	if section == core.SectionCore && len(c) > core.MaxCoreEntryBytes {
		return &core.MemoryQuotaError{Kind: core.QuotaKindEntry, Limit: core.MaxCoreEntryBytes}
	}
	if err := validateMemorySection(section); err != nil {
		return err
	}

	now := s.now().UnixMilli()
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if section == core.SectionCore {
			total, err := coreSectionBytes(ctx, tx, scopeCol, agentName, now)
			if err != nil {
				return err
			}
			if total+len(c) > core.MaxCoreSectionBytes {
				return &core.MemoryQuotaError{Kind: core.QuotaKindSection, Limit: core.MaxCoreSectionBytes}
			}
		}

		_, err := insertMemoryRow(ctx, tx, memoryRow{
			scopeCol:  scopeCol,
			agentName: agentName,
			section:   section,
			content:   c,
			state:     memoryStateActive,
			source:    memorySourceSaveMemory,
			createdAt: now,
		})
		return err
	})
}

// insertMemoryRow 是唯一的条目 INSERT：`valid_from` 恒等于 `created_at`（写入即生效的
// 起点），`content_hash` 恒等于正文的 sha256（去重与来源判定按它比），`supersedes_id`
// 以 NULL 表示"没有前驱"（0 ≠ NULL，交给 `NULLIF` 会多一次类型推断，故在 Go 侧转）。
// insertMemoryRow is the only entry INSERT; supersedes_id is carried as NULL when absent.
func insertMemoryRow(ctx context.Context, tx Queryer, row memoryRow) (int64, error) {
	var supersedes any
	if row.supersedesID > 0 {
		supersedes = row.supersedesID
	}
	var confirmedBy any
	if row.confirmedBy != "" {
		confirmedBy = row.confirmedBy
	}
	var validTo any
	if row.validTo > 0 {
		validTo = row.validTo
	}

	var id int64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO memory_entries
		   (scope, agent_name, section, content, created_at,
		    state, source, valid_from, valid_to, supersedes_id, content_hash, confirmed_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 RETURNING id`,
		row.scopeCol, row.agentName, string(row.section), row.content, row.createdAt,
		row.state, row.source, row.createdAt, validTo, supersedes, memoryContentHash(row.content),
		confirmedBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("写入记忆条目（%s/%s）: %w", row.scopeCol, row.section, err)
	}
	return id, nil
}

// Load 渲染该层的规范文本（同形两分区，约束 17 / 口径 12）：与 markdown 档**不承诺**
// 逐字节相同，只承诺同一解析器可切出同一分区结构。零行 = 空层 → 空文本（与 markdown 档
// 的合法空层归一一致）。**只读生效条目**：候选 / 已删 / 已被替代 / 已到期都不进注入面
// （T052 前后判据相同——markdown 档物理上就没有这些行，PG 档必须靠判据补齐）。
// 渲染形态漂移由 `TestMemoryStore_RenderedTextIsParseableBySharedParser` 用文档化形态的
// 解析规则守住（两档各写一份渲染器，形态一致靠判据钉）。
// Load renders the effective layer only: candidates, deletions and expired rows never
// reach the prompt.
func (s *MemoryStore) Load(ctx context.Context, scope core.Scope) (string, error) {
	scopeCol, agentName := scopeColumns(scope)

	rows, err := s.db.pool.QueryContext(ctx,
		`SELECT section, content FROM memory_entries
		  WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2
		    AND state = 'active' AND (valid_to IS NULL OR valid_to > $3)
		  ORDER BY id`, scopeCol, agentName, s.now().UnixMilli())
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

// derivedClosureCTE 与 derivedLiveFilterSQL 是召回面**派生复核**的同一段判据（US-22 /
// C-31）：三个 SQL 入口（RecallByKeyword、无向量关键词回退、向量路径）逐字共用，不各写一份。
//
//   - derivedClosureCTE 沿 `memory_candidate_sources` 递归取「被召回条目 → 来源 → 来源的
//     来源…」的闭包。种子只从本次召回的分层（$1 / $2 / $3）里的派生条目起步，递归本身
//     跨分层（来源没有同层要求）。闭包每次查询只物化一次，行级判据按 entry_id 关联回外层行。
//     深度到 8 就停：链还没走完的节点由判据按失效处理（fail-closed），绝不返回无法证明
//     来源有效的条目。
//   - derivedLiveFilterSQL 是行级判据：闭包内任一来源缺失（物理删除，正常不会发生）、非
//     `active` 或已过期（`valid_to <= $4`，与生效层判据同一时间参数）→ 排除该派生条目；
//     无来源行的显式条目闭包为空，判据恒真，不受影响。它写在 WHERE 里，故先于 ORDER BY /
//     LIMIT / top-k 生效——被排除的派生条目不占返回名额，条数不足时**不回填**。
//
// 占位符约定：三个入口的 $1 = scope、$2 = agent_name、$3 = section、$4 = now 逐字一致，
// 共享片段才能原样拼接（SQL 文本是编译期常量，不做运行时拼接）。
// The shared derived-entry re-check: one recursive closure over memory_candidate_sources
// and one row-level predicate, applied in SQL before ordering and top-k, depth-limited at 8
// and fail-closed.
const derivedClosureCTE = `WITH RECURSIVE derived_closure(entry_id, source_id, depth) AS (
	SELECT s.entry_id, s.source_entry_id, 1
	  FROM memory_candidate_sources s
	  JOIN memory_entries e ON e.id = s.entry_id
	 WHERE e.scope = $1 AND e.agent_name IS NOT DISTINCT FROM $2 AND e.section = $3
	UNION ALL
	SELECT c.entry_id, s.source_entry_id, c.depth + 1
	  FROM derived_closure c
	  JOIN memory_candidate_sources s ON s.entry_id = c.source_id
	 WHERE c.depth < 8
)
`

const derivedLiveFilterSQL = `NOT EXISTS (
	SELECT 1
	  FROM derived_closure c
	  LEFT JOIN memory_entries src ON src.id = c.source_id
	 WHERE c.entry_id = memory_entries.id
	   AND (src.id IS NULL
	        OR src.state <> 'active'
	        OR (src.valid_to IS NOT NULL AND src.valid_to <= $4)
	        OR (c.depth >= 8 AND EXISTS (
	            SELECT 1 FROM memory_candidate_sources deeper WHERE deeper.entry_id = c.source_id)))
)`

// recallByKeywordSQL 是 RecallByKeyword 的完整语句：既有生效层判据 + 派生复核。
// The keyword statement = effective-layer predicate + derived re-check.
const recallByKeywordSQL = derivedClosureCTE + `
SELECT content FROM memory_entries
 WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2 AND section = $3
   AND state = 'active' AND (valid_to IS NULL OR valid_to > $4)
   AND strpos(content, $5) > 0
   AND ` + derivedLiveFilterSQL + `
 ORDER BY id`

// RecallByKeyword 在**归档区**做整串子串检索：`strpos` 是字节级匹配——不分词、不扩词、
// 不做大小写折叠；按 `id` 升序即写入顺序。未命中 = 空结果 + `nil` error（口径 10）。
// 与 Load 同一生效层判据：失效条目即便正文命中也不得返回；来源已失效的派生条目同样
// 不得返回（C-31）。
// RecallByKeyword matches whole substrings over the effective archive in write order.
func (s *MemoryStore) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error) {
	scopeCol, agentName := scopeColumns(scope)

	rows, err := s.db.pool.QueryContext(ctx, recallByKeywordSQL,
		scopeCol, agentName, string(core.SectionArchive), s.now().UnixMilli(), query)
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

// RecallEntries 在归档区召回生效条目：有向量走语义路径（固定 active 代际、距离算子按
// 代际行的 `distance` 三选一），没向量退回整串子串（score 恒 0）。两条路径共用同一段
// 生效层判据与同一条列序，返回形状因此完全一致。
//
// 权限每次重查（FR-015）：零值 / 无效 Principal → `core.ErrUnauthorized`；Agent 层要求
// `agents:invoke:<name>`（缺失 → `core.ErrForbidden`）；全局层是部署共享层，设计没有对应
// scope，任何有效 Principal 可读。这里**不得**复用门面的判定结果——存储层是最后一道闸。
//
// 语义路径 fail-closed 三种：没有 active 代际 / 向量模型与代际不符 / 向量维度与代际不符。
// 三者都直接报错而不是退回关键词或空结果——否则"向量链路坏了"会伪装成"没有相关记忆"。
// RecallEntries re-checks permissions on every call and fails closed when the vector path
// cannot be trusted.
func (s *MemoryStore) RecallEntries(ctx context.Context, query memory.RecallQuery, vector []float32, model string) ([]memory.MemoryEntry, error) {
	if !query.Principal.Valid() {
		return nil, core.ErrUnauthorized
	}
	if query.Scope.Agent != "" && !query.Principal.HasAgentScope(query.Scope.Agent) {
		return nil, fmt.Errorf("%w: 无权召回 Agent %q 的记忆", core.ErrForbidden, query.Scope.Agent)
	}

	scopeCol, agentName := scopeColumns(query.Scope)
	now := s.now().UnixMilli()
	if len(vector) == 0 {
		return s.recallEntriesByKeyword(ctx, scopeCol, agentName, query.Query, now, query.Limit)
	}
	return s.recallEntriesByVector(ctx, scopeCol, agentName, vector, model, now, query.Limit)
}

// recallEntriesByKeywordSQL 是关键词回退的完整语句：列序与向量路径一致，判据与
// `RecallByKeyword` 逐字共用（含派生复核与 `strpos` 空 query 匹配全部的既有语义）。
// The keyword fallback shares the derived re-check verbatim with RecallByKeyword.
const recallEntriesByKeywordSQL = derivedClosureCTE + `
SELECT id, scope, agent_name, section, content, state, source, supersedes_id, created_at,
       0::float8 AS score
  FROM memory_entries
 WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2 AND section = $3
   AND state = 'active' AND (valid_to IS NULL OR valid_to > $4)
   AND strpos(content, $5) > 0
   AND ` + derivedLiveFilterSQL + `
 ORDER BY id
 LIMIT $6::bigint`

// recallEntriesByKeyword 是语义路径的兼容回退：调用方没走 embedding（empty vector）时，
// 按整串子串召回、score 恒 0，判据与 `RecallByKeyword` 逐字一致（含空 query 匹配全部
// 的 `strpos` 既有语义——不在 T052 改判据）。
// recallEntriesByKeyword matches whole substrings with a constant zero score.
func (s *MemoryStore) recallEntriesByKeyword(ctx context.Context, scopeCol string, agentName any, query string, now int64, limit int) ([]memory.MemoryEntry, error) {
	rows, err := s.db.pool.QueryContext(ctx, recallEntriesByKeywordSQL,
		scopeCol, agentName, string(core.SectionArchive), now, query, limitArg(limit))
	if err != nil {
		return nil, fmt.Errorf("召回记忆归档区（%s）: %w", scopeCol, err)
	}
	entries, err := scanMemoryEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("读取召回结果（%s）: %w", scopeCol, err)
	}
	return entries, nil
}

// recallEntriesByVectorSQL 是向量召回的完整语句：生效层判据、派生复核与距离表达式同一
// 形态；占位符按共享片段约定重排为 $1 scope / $2 agent / $3 section / $4 now，
// $5 查询向量 / $6 代际 / $7 LIMIT。距离 CASE 三选一保持静态（gosec G202 与可读性两头
// 都站得住）。
// The vector statement keeps the shared $1-$4 prefix so the derived re-check is reused
// verbatim; $5 is the query vector, $6 the generation and $7 the limit.
const recallEntriesByVectorSQL = derivedClosureCTE + `
SELECT id, scope, agent_name, section, content, state, source, supersedes_id, created_at,
       CASE v.distance
         WHEN 'cosine' THEN 1.0 - (v.embedding <=> $5::vector)
         WHEN 'l2' THEN 1.0 / (1.0 + (v.embedding <-> $5::vector))
         ELSE -(v.embedding <#> $5::vector)
       END AS score
  FROM memory_entries
  JOIN (
      SELECT m.entry_id, m.embedding, g.distance
        FROM memory_embeddings m
        JOIN kb_index_generations g ON g.generation = m.generation
       WHERE m.generation = $6
  ) v ON v.entry_id = memory_entries.id
 WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2 AND section = $3
   AND state = 'active' AND (valid_to IS NULL OR valid_to > $4)
   AND ` + derivedLiveFilterSQL + `
 ORDER BY CASE v.distance
            WHEN 'cosine' THEN v.embedding <=> $5::vector
            WHEN 'l2' THEN v.embedding <-> $5::vector
            ELSE v.embedding <#> $5::vector
          END ASC, id DESC
 LIMIT $7::bigint`

// recallEntriesByVector 在**单个 active 代际**上做向量召回（C-09）：代际、模型与维度
// 三项一起核对，任一不符即拒绝。距离表达式与 `vectorSearcher.Search` 同一形态——静态
// CASE 三选一，不拼动态 SQL（gosec G202 与可读性两头都站得住）。派生复核在排序与 LIMIT
// 之前生效（C-31）：来源已失效的派生条目不占名额，也不挤掉有效条目。
//
// 向量**零命中**时再判一次索引缺口（V-5）：同层同代际下存在生效归档条目却没有向量，
// 就说明"没有可召回记忆"这个结论不成立——返回 `memory.ErrIndexNotReady`，由工具层
// 渲染成可自我纠正的失败，**不得**伪装成 `(无匹配条目)`。缺口计数放在召回之后而不是
// 之前：有命中的调用不为一次诊断查询多付一次往返。
// On a zero-hit vector query, a remaining gap turns into ErrIndexNotReady instead of a
// "no match" result.
// recallEntriesByVector pins exactly one active generation and checks model and dimension.
func (s *MemoryStore) recallEntriesByVector(ctx context.Context, scopeCol string, agentName any, vector []float32, model string, now int64, limit int) ([]memory.MemoryEntry, error) {
	if model == "" {
		// 没有模型标识 = 无法核对"这条向量是不是这个代际产的"——按不满足处理。
		return nil, fmt.Errorf("%w: 查询向量未声明模型", errMemoryModelMismatch)
	}

	var (
		generation int
		genModel   string
		dimensions int
	)
	err := s.db.pool.QueryRowContext(ctx,
		`SELECT generation, embedding_model, dimensions
		   FROM kb_index_generations WHERE status = 'active'`).
		Scan(&generation, &genModel, &dimensions)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errMemoryGenerationMissing
	}
	if err != nil {
		return nil, fmt.Errorf("读取记忆索引代际: %w", err)
	}
	if model != genModel {
		return nil, fmt.Errorf("%w: 查询模型 %q，代际 %d 模型 %q", errMemoryModelMismatch, model, generation, genModel)
	}
	if len(vector) != dimensions {
		return nil, fmt.Errorf("%w: 查询维度 %d，代际 %d 维度 %d", errMemoryDimensionMismatch, len(vector), generation, dimensions)
	}

	literal, err := vectorLiteral(vector)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.pool.QueryContext(ctx, recallEntriesByVectorSQL,
		scopeCol, agentName, string(core.SectionArchive), now, literal, generation, limitArg(limit))
	if err != nil {
		return nil, fmt.Errorf("向量召回记忆归档区（%s）: %w", scopeCol, err)
	}
	entries, err := scanMemoryEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("读取向量召回结果（%s）: %w", scopeCol, err)
	}
	if len(entries) == 0 {
		missing, err := s.countUnindexedArchiveEntries(ctx, scopeCol, agentName, generation, now)
		if err != nil {
			return nil, err
		}
		if missing > 0 {
			return nil, fmt.Errorf("%w: 代际 %d 在 %s 层有 %d 条生效归档记忆尚无向量（未回填或回填失败）",
				memory.ErrIndexNotReady, generation, scopeCol, missing)
		}
	}
	return entries, nil
}

// limitArg 把 `Limit` 映射成 SQL 参数：<= 0 表示不截断，传 NULL（`LIMIT NULL` 在
// PostgreSQL 里等价于无上限）。
// limitArg maps "no truncation" to a NULL LIMIT.
func limitArg(limit int) any {
	if limit <= 0 {
		return nil
	}
	return limit
}

// LoadEntry 按主键读一条（任意状态，生命周期动作的前置检查）。found=false 表示不存在；
// 库层错误与"不存在"必须分开——前者要上报，后者由调用方按业务语义处理。
// LoadEntry reads one row in any state; found separates "absent" from "broken".
func (s *MemoryStore) LoadEntry(ctx context.Context, id int64) (memory.MemoryEntry, bool, error) {
	if id <= 0 {
		return memory.MemoryEntry{}, false, nil
	}
	entry, err := scanMemoryEntry(s.db.pool.QueryRowContext(ctx,
		`SELECT id, scope, agent_name, section, content, state, source, supersedes_id, created_at,
		        0::float8 AS score
		   FROM memory_entries WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.MemoryEntry{}, false, nil
	}
	if err != nil {
		return memory.MemoryEntry{}, false, fmt.Errorf("读取记忆条目 %d: %w", id, err)
	}
	return entry, true, nil
}

// ListEntries 按分层列出条目（管理面读模型）：**六态全集**、主键倒序、可选状态过滤。
// 与 `Load` 的生效层判据**故意不同**——`Load` 只出 active 且未到期的行（注入面），本方法
// 必须把 candidate / deleted / corrected 等一并返回，否则页面既看不到待确认的候选，
// 也看不到自己刚删掉的那一行。它同样**不**做权限过滤：调用方身份在领域层先判过
// （`memory.SemanticStore.ListEntries`），这里只按分层取行。
// ListEntries returns every state for one layer in id-descending order; it deliberately
// does not apply the effective-layer predicate, so candidates and deleted rows stay visible.
func (s *MemoryStore) ListEntries(ctx context.Context, query memory.ListQuery) ([]memory.MemoryEntry, error) {
	if query.State != "" && !memory.KnownState(query.State) {
		return nil, fmt.Errorf("列出记忆条目：状态取值域外：%q", query.State)
	}
	scopeCol, agentName := scopeColumns(query.Scope)

	// 过滤与窗口都用**定位置占位符**，不拼 SQL 文本：`$3` 空串表示"六态全列"（与
	// `ListQuery.State` 的既有语义一致），`$4` 走 `limitArg` 把 `<= 0` 映射成 NULL
	// （`LIMIT NULL` 等价于不截断）。两处原先用 `Sprintf` 拼占位符编号，被 gosec 判
	// G202（SQL 字符串拼接）；改成静态语句后行为逐字不变，且计划缓存可复用同一份文本。
	// Both the filter and the window use fixed positional placeholders, so the statement
	// text is constant: `$3` empty means "all six states", `$4` goes through limitArg.
	const listEntriesSQL = `SELECT id, scope, agent_name, section, content, state, source, supersedes_id,
	                              created_at, 0::float8 AS score
	                         FROM memory_entries
	                        WHERE scope = $1
	                          AND agent_name IS NOT DISTINCT FROM $2
	                          AND ($3 = '' OR state = $3)
	                        ORDER BY id DESC
	                        LIMIT $4::bigint`

	rows, err := s.db.pool.QueryContext(ctx, listEntriesSQL,
		scopeCol, agentName, query.State, limitArg(query.Limit))
	if err != nil {
		return nil, fmt.Errorf("列出记忆条目（%s）: %w", scopeCol, err)
	}
	entries, err := scanMemoryEntries(rows)
	if err != nil {
		return nil, fmt.Errorf("读取记忆条目列表（%s）: %w", scopeCol, err)
	}
	return entries, nil
}

// FindEntryByContent 按（分层, 分区, 正文哈希）查同源条目（提议去重）。正文以入库时同一
// 套规范化求哈希，调用方传已规范化正文即可。命中多行时按"活着优先"取一条：active 先于
// candidate，其余状态最后——`ORDER BY` 与 `id DESC` 一起保证结果确定（同内容的多代行里
// 最新的一行胜出）。
// FindEntryByContent prefers the live row and breaks ties by newest id.
func (s *MemoryStore) FindEntryByContent(ctx context.Context, scope core.Scope, section core.Section, content string) (memory.MemoryEntry, bool, error) {
	if err := validateMemorySection(section); err != nil {
		return memory.MemoryEntry{}, false, err
	}
	scopeCol, agentName := scopeColumns(scope)

	entry, err := scanMemoryEntry(s.db.pool.QueryRowContext(ctx,
		`SELECT id, scope, agent_name, section, content, state, source, supersedes_id, created_at,
		        0::float8 AS score
		   FROM memory_entries
		  WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2 AND section = $3
		    AND content_hash = $4
		  ORDER BY CASE state WHEN 'active' THEN 0 WHEN 'candidate' THEN 1 ELSE 2 END, id DESC
		  LIMIT 1`, scopeCol, agentName, string(section), memoryContentHash(strings.TrimSpace(content))))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.MemoryEntry{}, false, nil
	}
	if err != nil {
		return memory.MemoryEntry{}, false, fmt.Errorf("按内容查记忆条目（%s/%s）: %w", scopeCol, section, err)
	}
	return entry, true, nil
}

// InsertEntry 插入一条新条目并返回主键。state 由调用方给定（candidate / active /
// conflicted），本方法**不改写**它。来源按 `memoryEntrySource` 结算：显式带来了
// `legacy_import` / `migration` 就照用（旧记忆导入与迁移作业是仅有的两条受信任路径），
// 没带才按状态推导（active → `save_memory`，其余 → `explicit_propose`）。确认人随
// `entry.ConfirmedBy()` 落库（纠正的 conflicted 分支走这里），空值落 NULL。
// InsertEntry persists the caller's state verbatim, honours an explicit trusted source,
// and writes the confirmer carried by the entry itself.
func (s *MemoryStore) InsertEntry(ctx context.Context, entry memory.MemoryEntry) (int64, error) {
	if err := validateMemoryEntry(entry); err != nil {
		return 0, err
	}
	source, err := memoryEntrySource(entry)
	if err != nil {
		return 0, err
	}
	scopeCol, agentName := scopeColumns(entry.Scope)

	var newID int64
	err = s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		id, err := insertMemoryRow(ctx, tx, memoryRow{
			scopeCol:     scopeCol,
			agentName:    agentName,
			section:      entry.Section,
			content:      strings.TrimSpace(entry.Content),
			state:        entry.State,
			source:       source,
			createdAt:    s.now().UnixMilli(),
			supersedesID: entry.SupersedesID,
			confirmedBy:  entry.ConfirmedBy(),
		})
		if err != nil {
			return err
		}
		newID = id
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// UpdateEntryState 只迁移状态并可选记录 `confirmed_by`；正文、分层、分区与 `valid_from`
// 一律不动。空 `confirmedBy` 不覆盖已有值（`COALESCE(NULLIF(...))`）——删除不该抹掉
// 历史上是谁确认的。0 行受影响 = 条目在判定与写入之间被删了，按"不存在"上报而不是
// 静默成功。
// UpdateEntryState only migrates state; an empty confirmedBy keeps the recorded value.
func (s *MemoryStore) UpdateEntryState(ctx context.Context, id int64, state string, confirmedBy string) error {
	if !memoryStateAllowed(state) {
		return fmt.Errorf("记忆条目 %d: 状态取值域外：%q", id, state)
	}
	if id <= 0 {
		return fmt.Errorf("%w: %d", errMemoryEntryNotFound, id)
	}

	res, err := s.db.pool.ExecContext(ctx,
		`UPDATE memory_entries
		    SET state = $2,
		        confirmed_by = COALESCE(NULLIF($3, ''), confirmed_by)
		  WHERE id = $1`, id, state, confirmedBy)
	if err != nil {
		return fmt.Errorf("迁移记忆条目状态（%d → %s）: %w", id, state, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取状态迁移影响行数（%d）: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %d", errMemoryEntryNotFound, id)
	}
	return nil
}

// ReplaceEntry 在一个事务内插入 next 并把 oldID 从 active 迁移为 retireState：纠正 /
// 替代必须原子完成，**不得**出现两条 active 头。守卫条件写在 UPDATE 的 `WHERE state =
// 'active'` 里——并发下目标已被别的动作改过时影响行数为 0，此时返回 `core.ErrConflict`
// 且**不插入** next（先判后插会把竞态窗口原样搬进来）。
// ReplaceEntry retires the still-active oldID and inserts next in one transaction; a
// target that is no longer active yields core.ErrConflict without an insert. The new
// row records the confirmer carried by next (`confirmed_by`), so a corrected head keeps
// the data-level evidence of who vouched for the new body.
func (s *MemoryStore) ReplaceEntry(ctx context.Context, oldID int64, retireState string, next memory.MemoryEntry) (int64, error) {
	if oldID <= 0 {
		return 0, fmt.Errorf("%w: %d", errMemoryEntryNotFound, oldID)
	}
	if !memoryRetireStateAllowed(retireState) {
		return 0, fmt.Errorf("记忆条目 %d: 退役状态取值域外：%q（只允许 superseded / corrected）", oldID, retireState)
	}
	if err := validateMemoryEntry(next); err != nil {
		return 0, err
	}
	if next.State != memoryStateActive {
		return 0, fmt.Errorf("记忆条目 %d: 替代条必须是 active，实际 %q", oldID, next.State)
	}
	if next.SupersedesID != 0 && next.SupersedesID != oldID {
		return 0, fmt.Errorf("记忆条目 %d: 替代条的前驱 %d 与被替代条目不符", oldID, next.SupersedesID)
	}
	source, err := memoryEntrySource(next)
	if err != nil {
		return 0, err
	}
	scopeCol, agentName := scopeColumns(next.Scope)

	var newID int64
	err = s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE memory_entries SET state = $2 WHERE id = $1 AND state = 'active'`,
			oldID, retireState)
		if err != nil {
			return fmt.Errorf("退役记忆条目（%d → %s）: %w", oldID, retireState, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("读取退役影响行数（%d）: %w", oldID, err)
		}
		if affected == 0 {
			// 目标已不在 active（并发下被确认 / 纠正 / 删除）——不插入，交给上层裁决。
			return core.ErrConflict
		}

		id, err := insertMemoryRow(ctx, tx, memoryRow{
			scopeCol:     scopeCol,
			agentName:    agentName,
			section:      next.Section,
			content:      strings.TrimSpace(next.Content),
			state:        next.State,
			source:       source,
			createdAt:    s.now().UnixMilli(),
			supersedesID: oldID, // 前驱以参数为准：next 自带的同值已在上方校验过
			confirmedBy:  next.ConfirmedBy(),
		})
		if err != nil {
			return err
		}
		newID = id
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newID, nil
}

// scanMemoryEntries 遍历召回结果并逐行读成 `memory.MemoryEntry`；迭代错误在最后统一报。
// scanMemoryEntries drains a result set into entries.
func scanMemoryEntries(rows *sql.Rows) ([]memory.MemoryEntry, error) {
	defer func() { _ = rows.Close() }()

	var entries []memory.MemoryEntry
	for rows.Next() {
		entry, err := scanMemoryEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// memoryScanner 抽象 `*sql.Row` 与 `*sql.Rows` 的公共扫描面（单行与多行共用一份列序解析）。
// memoryScanner is the scan surface shared by *sql.Row and *sql.Rows.
type memoryScanner interface {
	Scan(dest ...any) error
}

// scanMemoryEntry 按固定的十列读一行：`id, scope, agent_name, section, content, state,
// source, supersedes_id, created_at, score`。分层两列要还原成 `core.Scope`，取值域外一律
// 报错（本文件是唯一写入口，出现域外值说明库被外部改过）。`source` 经 `memory.WithSource`
// 还原——回读是受信任路径，读到的值与库里的值逐字相同（导入 / 迁移行不会因回读被降级）。
// scanMemoryEntry reads the fixed ten-column shape and restores the source verbatim.
func scanMemoryEntry(row memoryScanner) (memory.MemoryEntry, error) {
	var (
		id           int64
		scope        string
		agentName    sql.NullString
		section      string
		content      string
		state        string
		source       string
		supersedesID sql.NullInt64
		createdAt    int64
		score        float64
	)
	if err := row.Scan(&id, &scope, &agentName, &section, &content, &state, &source,
		&supersedesID, &createdAt, &score); err != nil {
		return memory.MemoryEntry{}, err
	}
	layer, err := memoryScope(scope, agentName)
	if err != nil {
		return memory.MemoryEntry{}, err
	}
	if err := validateMemorySection(core.Section(section)); err != nil {
		return memory.MemoryEntry{}, err
	}
	return memory.WithSource(memory.MemoryEntry{
		ID:           id,
		Scope:        layer,
		Section:      core.Section(section),
		Content:      content,
		State:        state,
		Score:        score,
		SupersedesID: supersedesID.Int64,
		CreatedAt:    time.UnixMilli(createdAt).UTC(),
	}, source), nil
}

// memoryScope 把两列还原成分层：`global` 必须没有 agent_name，`agent` 必须有非空
// agent_name——空串顶替 NULL 会让取数静默返回零行（见 scopeColumns）。
// memoryScope rebuilds the layer and rejects out-of-domain column pairs.
func memoryScope(scope string, agentName sql.NullString) (core.Scope, error) {
	switch scope {
	case "global":
		if agentName.Valid {
			return core.Scope{}, fmt.Errorf("记忆条目: 全局层不得带 agent_name（%q）", agentName.String)
		}
		return core.Scope{}, nil
	case "agent":
		if !agentName.Valid || agentName.String == "" {
			return core.Scope{}, errors.New("记忆条目: Agent 层缺少 agent_name")
		}
		return core.Scope{Agent: agentName.String}, nil
	default:
		return core.Scope{}, fmt.Errorf("记忆条目: 分层取值域外：%q", scope)
	}
}

// memoryContentHash 是条目正文的 sha256（64 位小写十六进制），与 `memory_entries.
// content_hash` 及 `kb_chunks.content_hash` 同形——去重与来源判定都按它比。
// memoryContentHash is the lower-case hex sha256 of an entry body.
func memoryContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// memorySourceForState 由状态反推来源（**只在条目没带显式来源时**用，见 memoryEntrySource）：
// 生效条目来自直接保存，候选 / 冲突条目来自显式提议与纠正。导入与迁移行永远走显式来源，
// 不经本函数推导。
// memorySourceForState derives the source from the state, only when none was supplied.
func memorySourceForState(state string) string {
	if state == memoryStateActive {
		return memorySourceSaveMemory
	}
	return memorySourceExplicitPropose
}

// memoryStateAllowed 判定六态闭集（与 `schema.pg.sql` 的 CHECK 同源）。
// memoryStateAllowed reports whether the state is one of the six contract literals.
func memoryStateAllowed(state string) bool {
	switch state {
	case memoryStateCandidate, memoryStateActive, memoryStateSuperseded,
		memoryStateCorrected, memoryStateDeleted, memoryStateConflicted:
		return true
	default:
		return false
	}
}

// memoryRetireStateAllowed 判定"退役状态"闭集：只有被替代与被纠正两种，`deleted` 走
// 软删（UpdateEntryState），不得借替代路径完成。
// memoryRetireStateAllowed limits retirement to superseded and corrected.
func memoryRetireStateAllowed(state string) bool {
	return state == memoryStateSuperseded || state == memoryStateCorrected
}

// validateMemoryEntry 校验一条待写条目：分层由 scopeColumns 保证，这里管状态与分区。
// validateMemoryEntry rejects out-of-domain state and section values.
func validateMemoryEntry(entry memory.MemoryEntry) error {
	if !memoryStateAllowed(entry.State) {
		return fmt.Errorf("记忆条目: 状态取值域外：%q", entry.State)
	}
	return validateMemorySection(entry.Section)
}

// validateMemorySection 判定分区闭集（core / archive）。
// validateMemorySection rejects sections outside core/archive.
func validateMemorySection(section core.Section) error {
	switch section {
	case core.SectionCore, core.SectionArchive:
		return nil
	default:
		return fmt.Errorf("记忆条目: 分区取值域外：%q", section)
	}
}

// scopeColumns 把分层映射成表的两列（§3.4）：全局层 `scope='global'` + `agent_name IS NULL`
// ——**不得**用空串顶替 NULL（空串会让取数静默返回零行）。nil 参数要靠
// `IS NOT DISTINCT FROM` 才同时覆盖「NULL 对 NULL 成立」与「值对值相等」两句语义：
// SQLite 的 `IS` 本来就是这个行为，PostgreSQL 的 `=` 对 NULL 恒为 UNKNOWN——迁移时必须
// 显式改写，否则全局层的每次读取都会静默返回零行（2026-10-28 移植时发现）。
// scopeColumns maps the layer onto the two columns. The comparison must be
// IS NOT DISTINCT FROM: SQLite's IS treats NULL as a value, PostgreSQL's = does not,
// so the global layer would silently read zero rows otherwise.
func scopeColumns(scope core.Scope) (scopeCol string, agentName any) {
	if scope.Agent == "" {
		return "global", nil
	}
	return "agent", scope.Agent
}

// coreSectionBytes 求该层核心区**生效存量**的字节总量（`octet_length` 按 UTF-8 字节，
// 与 markdown 档的 `len(正文)` 同一口径——约束 29）。必须在调用方的写事务内执行：
// 检查与 INSERT 同域才不会被并发写插队。只数生效条目：候选与到期条目不进注入面，
// 也就不该占注入面的配额（T052 判据；markdown 档物理上没有这些行，两档由此对齐）。
// coreSectionBytes sums the effective core region; it must run inside the caller's write
// transaction so the check and the insert share one atomic region.
func coreSectionBytes(ctx context.Context, tx Queryer, scopeCol string, agentName any, now int64) (int, error) {
	var total int
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(octet_length(content)), 0) FROM memory_entries
		  WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2 AND section = $3
		    AND state = 'active' AND (valid_to IS NULL OR valid_to > $4)`,
		scopeCol, agentName, string(core.SectionCore), now).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("统计核心区配额（%s）: %w", scopeCol, err)
	}
	return total, nil
}

// renderMemoryText 渲染整层规范文本：文件标题 + 核心区 + 归档区，条目 = 零缩进 `"- "` 首行
// + 两空格缩进续行（约束 28 / 口径 22）。**两档各有一份渲染器**：本包只 import
// `internal/memory` 的**端口类型**（实现方必须满足接口），不复用它的渲染器——形态一致
// 由 `TestMemoryStore_RenderedTextIsParseableBySharedParser` 钉住。
// renderMemoryText renders the layer text in the documented shape; the renderer is
// independent of the facade's copy and kept in sync by test.
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
