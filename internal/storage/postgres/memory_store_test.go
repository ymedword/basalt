//go:build integration

package postgres

// 本文件是**记忆载体（PostgreSQL 档）**的有库用例（T052）。三条口径：
//
//  1. **真库判约束**：`memory_entries` 的 NOT NULL / CHECK 闭集、`memory_embeddings`
//     的代际外键与 `vector(512)`、`ux_kb_index_generations_active` 的部分唯一索引——
//     这些只有真库才判；夹具不绕过被测方法去"预演"它。
//  2. **生效层判据逐路径对号**：Load / RecallByKeyword / RecallEntries / 核心区配额
//     四条路径各有一条"失效条目不得出现"的断言——判据漏写在哪一路，那一路上就红。
//  3. **逆改点逐条写明**：每个用例注释里的"逆改点"是这条用例的判别力来源；写不出
//     逆改点的断言等于没判。
//
// These are the database-backed cases for the PostgreSQL memory carrier: real schema
// constraints, one effective-layer assertion per read path, and an explicit mutation
// point for every case.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// memNow 是夹具的固定写库时刻（时间列一律 Unix 毫秒 UTC，固定值让断言可复算）。
// memNow is the fixed instant used by the memory fixtures.
var memNow = time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)

// memTestStore 建隔离 schema 并返回句柄与固定时钟的载体。
func memTestStore(t *testing.T) (*DB, *MemoryStore) {
	t.Helper()
	db := knTestDB(t)
	return db, NewMemoryStore(db, func() time.Time { return memNow })
}

// memSeedRow 是绕过被测方法直接落的行——用来构造候选 / 已删 / 到期这些**写入口不该
// 产生**的状态（写入口只产 active 与候选），从而把"读路径不看状态"的缺陷暴露出来。
// memSeedRow seeds states the write paths never produce, to expose read-path leaks.
type memSeedRow struct {
	scope        string
	agentName    any
	section      string
	content      string
	state        string
	source       string
	createdAt    int64
	validTo      any
	supersedesID any
}

// memSeed 插一行并返回主键；缺省值按"生效条目"补齐（与写入口同形）。
func memSeed(t *testing.T, db *DB, row memSeedRow) int64 {
	t.Helper()
	if row.createdAt == 0 {
		row.createdAt = memNow.UnixMilli()
	}
	if row.state == "" {
		row.state = memoryStateActive
	}
	if row.source == "" {
		row.source = memorySourceSaveMemory
	}

	var id int64
	err := db.pool.QueryRowContext(context.Background(), `
		INSERT INTO memory_entries
			(scope, agent_name, section, content, created_at, state, source,
			 valid_from, valid_to, supersedes_id, content_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $5, $8, $9, $10)
		RETURNING id`,
		row.scope, row.agentName, row.section, row.content, row.createdAt,
		row.state, row.source, row.validTo, row.supersedesID,
		memoryContentHash(row.content)).Scan(&id)
	require.NoError(t, err)
	return id
}

// memSeedGeneration 建一条索引代际：active 必须有 activated_at，retired 必须有
// retired_at（schema 的成对 CHECK），同一时刻至多一条 active（部分唯一索引）。
func memSeedGeneration(t *testing.T, db *DB, generation int, status, model string, dimensions int) {
	t.Helper()
	// 成对 CHECK 是**等号**：active 行必须且只能有 activated_at，retired 行必须且只能有
	// retired_at（退役行带 activated_at 同样违约）。
	var activatedAt, retiredAt any
	if status == "active" {
		activatedAt = memNow.UnixMilli()
	}
	if status == "retired" {
		retiredAt = memNow.UnixMilli()
	}
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO kb_index_generations
			(generation, embedding_model, dimensions, distance, status, activated_at, retired_at, created_at)
		VALUES ($1, $2, $3, 'cosine', $4, $5, $6, $7)`,
		generation, model, dimensions, status, activatedAt, retiredAt, memNow.UnixMilli())
	require.NoError(t, err)
}

// memSeedEmbedding 给条目写一条代际向量（向量写入的生产路径归 T062+ 的回填作业）。
func memSeedEmbedding(t *testing.T, db *DB, entryID int64, generation int, vector []float32) {
	t.Helper()
	literal, err := vectorLiteral(vector)
	require.NoError(t, err)
	_, err = db.pool.ExecContext(context.Background(), `
		INSERT INTO memory_embeddings (entry_id, generation, embedding, model, dimensions, created_at)
		VALUES ($1, $2, $3::vector, 'text-embedding-v4', $4, $5)`,
		entryID, generation, literal, len(vector), memNow.UnixMilli())
	require.NoError(t, err)
}

// memCount 数某分层下的行数：`section` / `state` 传空串表示"不限"。
// memCount counts rows in a layer; empty section/state means "any".
func memCount(t *testing.T, db *DB, scopeCol string, agentName any, section, state string) int {
	t.Helper()
	var n int
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM memory_entries
		  WHERE scope = $1 AND agent_name IS NOT DISTINCT FROM $2
		    AND ($3 = '' OR section = $3) AND ($4 = '' OR state = $4)`,
		scopeCol, agentName, section, state).Scan(&n))
	return n
}

// memCaller 构造一个持有指定 scope 的调用方身份（外部请求形态）。
func memCaller(scopes ...string) core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller-memory",
		KeyID:    "key-memory",
		Scopes:   scopes,
		Actor:    core.ActorCaller,
	}
}

// TestMemoryStore_AppendWritesLifecycleColumns 守"兼容面追加也写全生命周期列"：
// `Append` 只收 scope / section / content，其余十二列必须由载体补齐——缺
// `valid_from` / `content_hash` 时真库直接 NOT NULL 违约（T052 修复前的形态）。
// 逆改点：从 INSERT 去掉 state / source / valid_from / content_hash 任一列 → 本用例红。
// 验收点：六态契约的写入侧 / 约束 29（写入与 markdown 档同一规范化）。
func TestMemoryStore_AppendWritesLifecycleColumns(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)

	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "  核心甲  "))
	require.NoError(t, store.Append(ctx, core.Scope{Agent: "demo"}, core.SectionArchive, "归档乙"))

	var (
		scope, agentName, section, content, state, source, contentHash string
		createdAt, validFrom                                           int64
		validTo, supersedesID                                          sql.NullInt64
	)
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT scope, COALESCE(agent_name, ''), section, content, state, source,
		       created_at, valid_from, valid_to, supersedes_id, content_hash
		  FROM memory_entries WHERE scope = 'global'`).
		Scan(&scope, &agentName, &section, &content, &state, &source,
			&createdAt, &validFrom, &validTo, &supersedesID, &contentHash))
	require.Equal(t, "global", scope)
	require.Empty(t, agentName, "全局层的 agent_name 必须是 NULL（空串会让取数静默零行）")
	require.Equal(t, "core", section)
	require.Equal(t, "核心甲", content, "写入前必须与 markdown 档同一规范化（TrimSpace）")
	require.Equal(t, memoryStateActive, state)
	require.Equal(t, memorySourceSaveMemory, source)
	require.Equal(t, memNow.UnixMilli(), createdAt)
	require.Equal(t, createdAt, validFrom, "valid_from 恒等于 created_at（写入即生效的起点）")
	require.False(t, validTo.Valid, "生效条目没有到期时刻")
	require.False(t, supersedesID.Valid)
	require.Equal(t, memoryContentHash("核心甲"), contentHash, "content_hash 按规范化后的正文求")

	var agentScope, agentName2 string
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		SELECT scope, agent_name FROM memory_entries WHERE scope = 'agent'`).
		Scan(&agentScope, &agentName2))
	require.Equal(t, "agent", agentScope)
	require.Equal(t, "demo", agentName2)
}

// TestMemoryStore_AppendEnforcesCoreQuota 守核心区配额在 PG 上照旧成立：单条上限、
// 总量上限、拒绝路径零副作用，且**只数生效条目**——候选与到期条目不进注入面，也就
// 不该占注入面的配额。
// 逆改点：① 把总量检查挪出事务（`withTx` 之前）或删掉；② 配额求和漏掉生效层判据 →
// 预先埋的候选行会把第 8 笔挤掉 → 断言红。
// 验收点：约束 12 / 约束 29 / 口径 6（恰在上限 == 放行）。
func TestMemoryStore_AppendEnforcesCoreQuota(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	scope := core.Scope{Agent: "quota"}

	// 候选条目先占 4096 字节：它不进注入面，因此不得占用配额。
	memSeed(t, db, memSeedRow{
		scope: "agent", agentName: "quota", section: "core",
		content: strings.Repeat("c", core.MaxCoreEntryBytes),
		state:   memoryStateCandidate, source: memorySourceExplicitPropose,
	})

	// 单条上限先于总量判据：MaxCoreEntryBytes+1 → entry 类拒绝。
	var quota *core.MemoryQuotaError
	err := store.Append(ctx, scope, core.SectionCore, strings.Repeat("a", core.MaxCoreEntryBytes+1))
	require.ErrorAs(t, err, &quota)
	require.Equal(t, core.QuotaKindEntry, quota.Kind)

	// 边界：上限 / 单条 = 恰 N 笔放行（候选不占额，所以第 N 笔仍在内）。
	fullRows := core.MaxCoreSectionBytes / core.MaxCoreEntryBytes
	for i := 0; i < fullRows; i++ {
		require.NoError(t, store.Append(ctx, scope, core.SectionCore,
			strings.Repeat("b", core.MaxCoreEntryBytes)), "第 %d 笔恰在上限内", i+1)
	}
	require.Equal(t, fullRows, memCount(t, db, "agent", "quota", "core", "active"))

	// 第 N+1 笔越限：section 类拒绝，且零副作用（不得留下超额那条生效行）。
	err = store.Append(ctx, scope, core.SectionCore, "c")
	require.ErrorAs(t, err, &quota)
	require.Equal(t, core.QuotaKindSection, quota.Kind)
	require.Equal(t, fullRows, memCount(t, db, "agent", "quota", "core", "active"),
		"总量越限的拒绝路径零副作用：不得留下超额行")
}

// TestMemoryStore_LoadAndRecallKeywordHideInactiveEntries 守"注入面与关键词召回只看
// 生效条目"：候选 / 已删 / 已替代 / 已纠正 / 冲突 / 已到期六种行都不得出现在 Load
// 的渲染文本与 RecallByKeyword 的结果里。
// 逆改点：删掉 Load 或 RecallByKeyword 的 `state = 'active' AND (valid_to ...)` 判据
// → 对应的失效正文出现在结果里 → 断言红（两条路径分别可逆改，判别力各算各的）。
// 验收点：六态契约的读取侧 / 生效层判据（T052 核心）。
func TestMemoryStore_LoadAndRecallKeywordHideInactiveEntries(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	scope := core.Scope{Agent: "hide"}

	liveID := memSeed(t, db, memSeedRow{
		scope: "agent", agentName: "hide", section: "archive", content: "生效归档-记忆",
	})
	memSeed(t, db, memSeedRow{ // 候选
		scope: "agent", agentName: "hide", section: "archive", content: "候选归档-记忆",
		state: memoryStateCandidate, source: memorySourceExplicitPropose,
	})
	memSeed(t, db, memSeedRow{ // 已删
		scope: "agent", agentName: "hide", section: "archive", content: "已删归档-记忆",
		state: memoryStateDeleted,
	})
	memSeed(t, db, memSeedRow{ // 已被替代
		scope: "agent", agentName: "hide", section: "archive", content: "替代归档-记忆",
		state: memoryStateSuperseded,
	})
	memSeed(t, db, memSeedRow{ // 已被纠正
		scope: "agent", agentName: "hide", section: "archive", content: "纠正归档-记忆",
		state: memoryStateCorrected,
	})
	memSeed(t, db, memSeedRow{ // 冲突待裁决
		scope: "agent", agentName: "hide", section: "archive", content: "冲突归档-记忆",
		state: memoryStateConflicted,
	})
	memSeed(t, db, memSeedRow{ // 已到期：valid_to 早于夹具时刻，valid_from 更早
		scope: "agent", agentName: "hide", section: "archive", content: "到期归档-记忆",
		createdAt: memNow.Add(-time.Hour).UnixMilli(),
		validTo:   memNow.Add(-time.Minute).UnixMilli(),
	})
	memSeed(t, db, memSeedRow{ // 核心区的候选同样不得进注入面
		scope: "agent", agentName: "hide", section: "core", content: "候选核心-记忆",
		state: memoryStateCandidate, source: memorySourceExplicitPropose,
	})
	memSeed(t, db, memSeedRow{
		scope: "agent", agentName: "hide", section: "core", content: "生效核心-记忆",
	})

	text, err := store.Load(ctx, scope)
	require.NoError(t, err)
	require.Contains(t, text, "生效归档-记忆")
	require.Contains(t, text, "生效核心-记忆")
	for _, hidden := range []string{"候选归档-记忆", "已删归档-记忆", "替代归档-记忆", "纠正归档-记忆", "冲突归档-记忆", "到期归档-记忆", "候选核心-记忆"} {
		require.NotContains(t, text, hidden, "失效条目不进注入面：%s", hidden)
	}

	hits, err := store.RecallByKeyword(ctx, scope, "记忆")
	require.NoError(t, err)
	require.Equal(t, []string{"生效归档-记忆"}, hits, "关键词召回只返回生效归档条目")

	entries, err := store.RecallEntries(ctx, memory.RecallQuery{
		Principal: core.LocalAdminPrincipal(),
		Scope:     scope,
		Query:     "记忆",
		Mode:      memory.RecallModeKeyword,
	}, nil, "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, liveID, entries[0].ID)
	require.Equal(t, memoryStateActive, entries[0].State)
	require.Zero(t, entries[0].Score, "关键词路径的名次分恒 0")
}

// TestMemoryStore_RecallEntriesPinsActiveGeneration 守"召回只读当前 active 代际"（C-09）：
// 旧代际的向量再相似也不得入选，同一条目在新代际上的分数必须来自新向量。
// 逆改点：把召回 SQL 的 `m.generation = $2` 换成"任意代际"或改成读 latest → 只在旧代际
// 有向量的那条会挤进结果、或分数变成旧向量的 → 断言红。
// 验收点：C-09 / 单代际每查询。
func TestMemoryStore_RecallEntriesPinsActiveGeneration(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)

	memSeedGeneration(t, db, 1, "retired", "text-embedding-v4", 512)
	memSeedGeneration(t, db, 2, "active", "text-embedding-v4", 512)

	query := knVector(0.25)

	// 只在旧代际有向量：无论向量多接近查询，都不得入选。
	oldOnly := memSeed(t, db, memSeedRow{
		scope: "agent", agentName: "gen", section: "archive", content: "旧代际条目",
	})
	memSeedEmbedding(t, db, oldOnly, 1, query)

	// 两个代际都有向量，新代际向量与查询完全相同（cosine 分应约为 1.0）。
	pinned := memSeed(t, db, memSeedRow{
		scope: "agent", agentName: "gen", section: "archive", content: "新代际条目",
	})
	memSeedEmbedding(t, db, pinned, 1, knVector(0.75))
	memSeedEmbedding(t, db, pinned, 2, query)

	// 没有向量的生效条目不参与语义召回（宁可少召回，也不给不存在的相似度）。
	memSeed(t, db, memSeedRow{
		scope: "agent", agentName: "gen", section: "archive", content: "无向量条目",
	})

	entries, err := store.RecallEntries(ctx, memory.RecallQuery{
		Principal: core.LocalAdminPrincipal(),
		Scope:     core.Scope{Agent: "gen"},
		Query:     "任意",
		Mode:      memory.RecallModeSemantic,
	}, query, "text-embedding-v4")
	require.NoError(t, err)
	require.Len(t, entries, 1, "只在 active 代际有向量的条目入选")
	require.Equal(t, pinned, entries[0].ID)
	require.InDelta(t, 1.0, entries[0].Score, 1e-5, "分数必须来自 active 代际的向量")
}

// TestMemoryStore_RecallEntriesFailsClosedOnGenerationMismatch 守"向量链路坏掉时明确失败"：
// 缺 active 代际、模型不符、维度不符三种都不许退化成空结果或关键词路径——否则装配故障
// 会伪装成"没有相关记忆"。
// 逆改点：把任一 fail-closed 分支改成 `return nil, nil`（空结果）→ 本用例红。
// 验收点：fail-closed / D-01 口径。
func TestMemoryStore_RecallEntriesFailsClosedOnGenerationMismatch(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	query := memory.RecallQuery{
		Principal: core.LocalAdminPrincipal(),
		Scope:     core.Scope{Agent: "gen"},
		Query:     "任意",
		Mode:      memory.RecallModeSemantic,
	}

	t.Run("没有 active 代际", func(t *testing.T) {
		memSeedGeneration(t, db, 1, "retired", "text-embedding-v4", 512)
		_, err := store.RecallEntries(ctx, query, knVector(0.1), "text-embedding-v4")
		require.ErrorIs(t, err, errMemoryGenerationMissing)
	})

	memSeedGeneration(t, db, 2, "active", "text-embedding-v4", 512)

	t.Run("模型不符", func(t *testing.T) {
		_, err := store.RecallEntries(ctx, query, knVector(0.1), "other-embedding-model")
		require.ErrorIs(t, err, errMemoryModelMismatch)
	})

	t.Run("未声明模型", func(t *testing.T) {
		_, err := store.RecallEntries(ctx, query, knVector(0.1), "")
		require.ErrorIs(t, err, errMemoryModelMismatch)
	})

	t.Run("维度不符", func(t *testing.T) {
		_, err := store.RecallEntries(ctx, query, knVector(0.1)[:511], "text-embedding-v4")
		require.ErrorIs(t, err, errMemoryDimensionMismatch)
	})
}

// TestMemoryStore_RecallEntriesRequiresAgentScope 守"载体是权限的最后一道闸"：
// 零值身份 → ErrUnauthorized；缺 `agents:invoke:<name>` → ErrForbidden；持有该 scope
// 或内置管理员 → 放行；全局层是部署共享层，有效身份即可读。
// 逆改点：去掉 HasAgentScope 判定（任何有效身份都能读 Agent 层）→ 第二条断言红；
// 或改成"全局层也要 Agent scope" → 最后一条红。
// 验收点：FR-015 / C-28 / C-32（身份不得默认成管理员）。
func TestMemoryStore_RecallEntriesRequiresAgentScope(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)

	memSeed(t, db, memSeedRow{scope: "agent", agentName: "demo", section: "archive", content: "demo-记忆"})
	memSeed(t, db, memSeedRow{scope: "global", section: "archive", content: "全局-记忆"})

	recallAgent := memory.RecallQuery{Scope: core.Scope{Agent: "demo"}, Query: "记忆"}

	// 零值身份：在任何库访问之前就拒绝。
	_, err := store.RecallEntries(ctx, recallAgent, nil, "")
	require.ErrorIs(t, err, core.ErrUnauthorized)

	recallAgent.Principal = memCaller("sessions:read") // 有效身份，但没有该 Agent 的调用权
	_, err = store.RecallEntries(ctx, recallAgent, nil, "")
	require.ErrorIs(t, err, core.ErrForbidden)

	recallAgent.Principal = memCaller("agents:invoke:demo")
	entries, err := store.RecallEntries(ctx, recallAgent, nil, "")
	require.NoError(t, err)
	require.Len(t, entries, 1)

	recallAgent.Principal = core.LocalAdminPrincipal()
	entries, err = store.RecallEntries(ctx, recallAgent, nil, "")
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// 全局层：有效身份即可读（设计没有对应的 Agent scope）。
	globalRecall := memory.RecallQuery{
		Principal: memCaller("sessions:read"),
		Query:     "记忆",
	}
	entries, err = store.RecallEntries(ctx, globalRecall, nil, "")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, core.GlobalScope, entries[0].Scope)
}

// TestMemoryStore_RecallEntriesWithoutVectorFallsBackToKeyword 守"没走 embedding 时
// 载体自行决定"（端口注释原文）：空向量退回整串子串、score 恒 0，判据与
// RecallByKeyword 逐字一致。
// 逆改点：把空向量分支改成报错（或返回空集）→ 本用例红。
// 验收点：端口语义 / 兼容路径不得依赖向量。
func TestMemoryStore_RecallEntriesWithoutVectorFallsBackToKeyword(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	scope := core.Scope{Agent: "fallback"}

	memSeed(t, db, memSeedRow{scope: "agent", agentName: "fallback", section: "archive", content: "甲-记忆"})
	memSeed(t, db, memSeedRow{scope: "agent", agentName: "fallback", section: "archive", content: "乙-记忆"})
	memSeed(t, db, memSeedRow{
		scope: "agent", agentName: "fallback", section: "archive", content: "丙-记忆",
		state: memoryStateDeleted,
	})

	entries, err := store.RecallEntries(ctx, memory.RecallQuery{
		Principal: core.LocalAdminPrincipal(),
		Scope:     scope,
		Query:     "记忆",
	}, nil, "")
	require.NoError(t, err)
	require.Len(t, entries, 2, "只返回生效条目，按写入顺序")
	require.Equal(t, []string{"甲-记忆", "乙-记忆"}, []string{entries[0].Content, entries[1].Content})
	for _, entry := range entries {
		require.Zero(t, entry.Score)
		require.Equal(t, core.SectionArchive, entry.Section)
	}
}

// TestMemoryStore_ReplaceEntryIsAtomic 守"纠正 / 替代不得出现两条 active 头"：
// 一次事务内退役旧条并插入新条；目标已不在 active 时返回 core.ErrConflict 且**不插入**。
// 逆改点：把 UPDATE 与 INSERT 拆到两个事务、或把 `WHERE state = 'active'` 去掉 →
// 第二次替代会造出第二条 active（或静默改掉已退役行）→ 断言红。
// 验收点：端口注释的原子性要求 / 六态迁移。
func TestMemoryStore_ReplaceEntryIsAtomic(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	scope := core.Scope{Agent: "replace"}

	oldID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope:   scope,
		Section: core.SectionArchive,
		Content: "旧事实",
		State:   memoryStateActive,
	})
	require.NoError(t, err)

	newID, err := store.ReplaceEntry(ctx, oldID, memoryStateCorrected, memory.WithConfirmedBy(memory.MemoryEntry{
		Scope:        scope,
		Section:      core.SectionArchive,
		Content:      "新事实",
		State:        memoryStateActive,
		SupersedesID: oldID,
	}, "admin-1"))
	require.NoError(t, err)
	require.NotEqual(t, oldID, newID)

	// 逆改点：去掉 insertMemoryRow 的 confirmed_by 列 → 这里读到 NULL → 断言红。
	require.Equal(t, "admin-1", memConfirmedBy(t, db, newID),
		"替代条必须把确认人写进 confirmed_by（纠正的数据级证据）")
	require.Empty(t, memConfirmedBy(t, db, oldID), "旧条没有确认人，不得顺手补")

	oldEntry, found, err := store.LoadEntry(ctx, oldID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memoryStateCorrected, oldEntry.State, "旧条已退役")

	newEntry, found, err := store.LoadEntry(ctx, newID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memoryStateActive, newEntry.State)
	require.Equal(t, oldID, newEntry.SupersedesID)
	require.Equal(t, 1, memCount(t, db, "agent", "replace", "", "active"),
		"同一分层的 active 头唯一")

	// 旧条已不在 active：再替代必须冲突，且不得插入新行（总数保持 2）。
	_, err = store.ReplaceEntry(ctx, oldID, memoryStateCorrected, memory.MemoryEntry{
		Scope:   scope,
		Section: core.SectionArchive,
		Content: "第三条",
		State:   memoryStateActive,
	})
	require.ErrorIs(t, err, core.ErrConflict)
	require.Equal(t, 2, memCount(t, db, "agent", "replace", "", ""),
		"冲突路径零副作用：不得插入第三条")

	// 退役状态与替代条状态都是闭集：越界在写库前就被拒绝。
	_, err = store.ReplaceEntry(ctx, newID, memoryStateDeleted, memory.MemoryEntry{
		Scope: scope, Section: core.SectionArchive, Content: "第四条", State: memoryStateActive,
	})
	require.Error(t, err, "deleted 只能走软删，不得借替代路径完成")

	_, err = store.ReplaceEntry(ctx, newID, memoryStateSuperseded, memory.MemoryEntry{
		Scope: scope, Section: core.SectionArchive, Content: "第五条", State: memoryStateCandidate,
	})
	require.Error(t, err, "替代条必须是 active")
}

// TestMemoryStore_InsertEntryWritesConfirmer 守"纠正的 conflicted 分支也记录确认人"：
// InsertEntry 把条目自带的确认人写进 confirmed_by；没带确认人时落 **NULL** 而不是空串
// ——"从未记录"与"确认人是空串"必须可区分。
// 逆改点：去掉 insertMemoryRow 的 confirmed_by 列 → 读数空串 → 红；空值写成空串 →
// `IS NULL` 断言红。
// 验收点：CorrectRequest.ConfirmedBy 的落库侧。
func TestMemoryStore_InsertEntryWritesConfirmer(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	scope := core.Scope{Agent: "conflict"}

	withWho, err := store.InsertEntry(ctx, memory.WithConfirmedBy(memory.MemoryEntry{
		Scope:   scope,
		Section: core.SectionArchive,
		Content: "两个来源矛盾的正文",
		State:   memoryStateConflicted,
	}, "admin-2"))
	require.NoError(t, err)
	require.Equal(t, "admin-2", memConfirmedBy(t, db, withWho))

	withoutWho, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope:   scope,
		Section: core.SectionArchive,
		Content: "候选正文",
		State:   memoryStateCandidate,
	})
	require.NoError(t, err)
	var isNull bool
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT confirmed_by IS NULL FROM memory_entries WHERE id = $1`, withoutWho).Scan(&isNull))
	require.True(t, isNull, "没带确认人必须落 NULL，不得写成空串")
}

// TestMemoryStore_ListEntriesSixStatesAndWindow 守管理面读模型的存储侧：六态全集、分层
// 隔离、主键倒序、窗口截断；域外状态在触达 SQL 之前被拒绝（不是静默空页）。
// 逆改点：套上生效层判据（`state = 'active'`）→ 候选 / 已删行消失，计数断言红；去掉
// `ORDER BY id DESC` → 倒序断言红；忽略 LIMIT → 窗口断言红。
// 验收点：管理面读模型（US-16 裁决 A）/ `ListEntries` 端口注释。
func TestMemoryStore_ListEntriesSixStatesAndWindow(t *testing.T) {
	ctx := context.Background()
	_, store := memTestStore(t)
	agentScope := core.Scope{Agent: "list"}

	candidateID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope: core.GlobalScope, Section: core.SectionArchive, Content: "全局候选", State: memoryStateCandidate,
	})
	require.NoError(t, err)
	activeID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope: core.GlobalScope, Section: core.SectionArchive, Content: "全局有效", State: memoryStateActive,
	})
	require.NoError(t, err)
	deletedID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope: core.GlobalScope, Section: core.SectionArchive, Content: "全局已删", State: memoryStateDeleted,
	})
	require.NoError(t, err)
	agentID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope: agentScope, Section: core.SectionArchive, Content: "Agent 候选", State: memoryStateCandidate,
	})
	require.NoError(t, err)

	// 六态全集 + 主键倒序：读模型不套召回面的有效层判据。
	all, err := store.ListEntries(ctx, memory.ListQuery{Scope: core.GlobalScope})
	require.NoError(t, err)
	require.Equal(t, []int64{deletedID, activeID, candidateID}, entryIDs(all),
		"全局层三条按主键倒序，候选与已删都在")

	filtered, err := store.ListEntries(ctx, memory.ListQuery{
		Scope: core.GlobalScope, State: memoryStateCandidate,
	})
	require.NoError(t, err)
	require.Equal(t, []int64{candidateID}, entryIDs(filtered), "状态过滤")

	window, err := store.ListEntries(ctx, memory.ListQuery{Scope: core.GlobalScope, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []int64{deletedID, activeID}, entryIDs(window), "窗口截断取最新的两条")

	// 分层隔离：Agent 层只看得到自己的行（同正文的全局行不得混进来）。
	agentRows, err := store.ListEntries(ctx, memory.ListQuery{Scope: agentScope})
	require.NoError(t, err)
	require.Equal(t, []int64{agentID}, entryIDs(agentRows), "分层隔离")

	// 域外状态是错误而不是空列表（空页会把"取值写错"伪装成"没有记忆"）。
	_, err = store.ListEntries(ctx, memory.ListQuery{Scope: core.GlobalScope, State: "expired"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "状态取值域外")
}

// entryIDs 取出一批条目的主键，供顺序断言使用。
func entryIDs(entries []memory.MemoryEntry) []int64 {
	ids := make([]int64, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

// TestMemoryStore_UpdateEntryStateOnlyMigratesState 守"状态迁移不动正文"：确认候选只改
// state / confirmed_by；空 confirmedBy 不抹掉已记录的人；0 行受影响按"不存在"上报。
// 逆改点：把 UPDATE 改成写回 content、或用 `confirmed_by = $3` 直接覆盖 → 第二条或第三条
// 断言红；把 0 行当成功 → 第 4 条红。
// 验收点：端口注释"/不得/改动正文" / 软删幂等的写入侧。
func TestMemoryStore_UpdateEntryStateOnlyMigratesState(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)

	id, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope:   core.Scope{Agent: "state"},
		Section: core.SectionArchive,
		Content: "候选事实",
		State:   memoryStateCandidate,
	})
	require.NoError(t, err)

	var before, after memory.MemoryEntry
	before, found, err := store.LoadEntry(ctx, id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memorySourceExplicitPropose,
		memSourceOf(t, db, id), "候选条目的来源是显式提议")

	require.NoError(t, store.UpdateEntryState(ctx, id, memoryStateActive, "alice"))
	after, found, err = store.LoadEntry(ctx, id)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memoryStateActive, after.State)
	require.Equal(t, before.Content, after.Content, "状态迁移不得改动正文")
	require.Equal(t, before.CreatedAt, after.CreatedAt)
	require.Equal(t, "alice", memConfirmedBy(t, db, id))

	// 空 confirmedBy 不覆盖已有记录（删除不该抹掉"谁确认的"）。
	require.NoError(t, store.UpdateEntryState(ctx, id, memoryStateDeleted, ""))
	require.Equal(t, "alice", memConfirmedBy(t, db, id))

	// 不存在的条目：0 行受影响按不存在上报，不得静默成功。
	err = store.UpdateEntryState(ctx, id+999, memoryStateDeleted, "")
	require.ErrorIs(t, err, errMemoryEntryNotFound)

	// 六态闭集：`expired` 不是契约状态（到期由 valid_to 表达）。
	require.ErrorContains(t, store.UpdateEntryState(ctx, id, "expired", ""), "取值域外")
}

// memSourceOf 读一行的 source 列。
func memSourceOf(t *testing.T, db *DB, id int64) string {
	t.Helper()
	var source string
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT source FROM memory_entries WHERE id = $1`, id).Scan(&source))
	return source
}

// memConfirmedBy 读一行的 confirmed_by 列。
func memConfirmedBy(t *testing.T, db *DB, id int64) string {
	t.Helper()
	var confirmed sql.NullString
	require.NoError(t, db.pool.QueryRowContext(context.Background(),
		`SELECT confirmed_by FROM memory_entries WHERE id = $1`, id).Scan(&confirmed))
	return confirmed.String
}

// TestMemoryStore_FindEntryByContentPrefersLiveRow 守"同源去重优先取活着的那条"：
// 同正文多代行并存时 active 先于 candidate、候选先于其余状态，同级取最新 id。
// 逆改点：去掉 ORDER BY 的 CASE（或改成纯 id DESC）→ 已删 / 已退役行会被当成同源条目 →
// 断言红（去重把新提议并进死行，正文再也回不到召回）。
// 验收点：Propose 的去重前提（端口注释）/ 六态读取侧。
func TestMemoryStore_FindEntryByContentPrefersLiveRow(t *testing.T) {
	ctx := context.Background()
	store := memTestStoreDB(t)

	scope := core.Scope{Agent: "find"}
	liveID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope: scope, Section: core.SectionArchive, Content: "同一事实", State: memoryStateActive,
	})
	require.NoError(t, err)
	deadID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope: scope, Section: core.SectionArchive, Content: "同一事实", State: memoryStateCandidate,
	})
	require.NoError(t, err)
	require.NoError(t, store.UpdateEntryState(ctx, deadID, memoryStateDeleted, ""))

	entry, found, err := store.FindEntryByContent(ctx, scope, core.SectionArchive, "  同一事实  ")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, liveID, entry.ID, "active 行优先于已删行；正文按同一规范化求哈希")

	// 活着的那条也没了：退回候选 / 其余状态（仍要能查到，便于生命周期判断）。
	require.NoError(t, store.UpdateEntryState(ctx, liveID, memoryStateDeleted, ""))
	entry, found, err = store.FindEntryByContent(ctx, scope, core.SectionArchive, "同一事实")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, deadID, entry.ID)

	// 分层与分区都是键的一部分：换个分区查不到。
	_, found, err = store.FindEntryByContent(ctx, scope, core.SectionCore, "同一事实")
	require.NoError(t, err)
	require.False(t, found)

	// 不存在的正文：found=false 而不是报错。
	_, found, err = store.FindEntryByContent(ctx, scope, core.SectionArchive, "没有这条")
	require.NoError(t, err)
	require.False(t, found)
}

// memTestStoreDB 只要载体的简写（不需要库句柄的用例用）。
func memTestStoreDB(t *testing.T) *MemoryStore {
	t.Helper()
	_, store := memTestStore(t)
	return store
}

// TestMemoryStore_RenderedTextIsParseableBySharedParser 守"渲染文本与 markdown 档同形"
// （口径 12）：PG 档 `Load` 渲染出的规范文本，按约束 28 的文档化形态（结构行只认零缩进
// 的文件标题与分区标题，条目 = 零缩进 `"- "` 起行）能切出同一分区结构；规则之外的行
// 直接失败，渲染形态一漂它就红。
// 逆改点：改 renderMemorySection / renderMemoryEntry 的缩进或 `"- "` 前缀 → 默认分支红。
// 验收点：口径 12 / 约束 28。
func TestMemoryStore_RenderedTextIsParseableBySharedParser(t *testing.T) {
	ctx := context.Background()
	store := memTestStoreDB(t)
	scope := core.Scope{Agent: "alice"}

	require.NoError(t, store.Append(ctx, scope, core.SectionCore, "核心甲"))
	require.NoError(t, store.Append(ctx, scope, core.SectionArchive, "归档乙"))

	text, err := store.Load(ctx, scope)
	require.NoError(t, err)

	var cores, archives []string
	section := ""
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "# "): // 文件标题：正文归属回到"未分区"
			section = ""
		case line == "## 核心记忆":
			section = "core"
		case line == "## 归档记忆":
			section = "archive"
		case line == "" || strings.HasPrefix(line, "  "): // 条目间空行 / 缩进续行
		case strings.HasPrefix(line, "- ") && section == "core":
			cores = append(cores, strings.TrimPrefix(line, "- "))
		case strings.HasPrefix(line, "- ") && section == "archive":
			archives = append(archives, strings.TrimPrefix(line, "- "))
		default:
			require.Failf(t, "渲染形态漂移", "行 %q 既不是结构行也不是条目行", line)
		}
	}
	require.Equal(t, []string{"核心甲"}, cores, "核心区条目切在核心区块内")
	require.Equal(t, []string{"归档乙"}, archives, "归档区条目切在归档区块内")
}

// TestMemoryStore_ExplicitSourceRoundTrip 守"受信任来源写入 / 回读逐字一致"：旧记忆导入
// 与数据库迁移带进来的 `legacy_import` / `migration` 必须原样落库，`LoadEntry` 与
// `FindEntryByContent` 回读时还原同一个值——回读是第三条受信任路径，不得把导入行降级成
// 普通写入。
// 逆改点：① 写入口忽略显式来源、按状态推导 → 两条 source 断言红；② 回读不还原 source
// → `loaded.Source()` 断言红。
// 验收点：用户 2026-10-28 指令第 3 条 / D-18。
func TestMemoryStore_ExplicitSourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)

	legacyID, err := store.InsertEntry(ctx, memory.WithSource(memory.MemoryEntry{
		Scope:   core.GlobalScope,
		Section: core.SectionArchive,
		Content: "旧载体记忆",
		State:   memoryStateActive,
	}, memory.SourceLegacyImport))
	require.NoError(t, err)
	require.Equal(t, "legacy_import", memSourceOf(t, db, legacyID))

	loaded, found, err := store.LoadEntry(ctx, legacyID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memory.SourceLegacyImport, loaded.Source(), "回读必须还原库里的来源")

	byContent, found, err := store.FindEntryByContent(ctx, core.GlobalScope, core.SectionArchive, "旧载体记忆")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memory.SourceLegacyImport, byContent.Source())

	migrationID, err := store.InsertEntry(ctx, memory.WithSource(memory.MemoryEntry{
		Scope:   core.Scope{Agent: "migrated"},
		Section: core.SectionArchive,
		Content: "迁移来的记忆",
		State:   memoryStateActive,
	}, memory.SourceMigration))
	require.NoError(t, err)
	require.Equal(t, "migration", memSourceOf(t, db, migrationID))

	loaded, found, err = store.LoadEntry(ctx, migrationID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memory.SourceMigration, loaded.Source())
	require.Equal(t, memoryStateActive, loaded.State, "来源不得改写状态")
}

// TestMemoryStore_ExplicitSourceRejectsOutOfClosure 守来源闭集：闭集外的值在写库前就被
// 拒绝（库里的 CHECK 是最后一道，Go 侧先报可读错误），且拒绝路径零副作用。
// 逆改点：去掉 memoryEntrySource 的闭集校验 → 写入要么被库 CHECK 报成模糊错误、要么
// 落进闭集外的分支 → 错误文案断言红。
func TestMemoryStore_ExplicitSourceRejectsOutOfClosure(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)

	_, err := store.InsertEntry(ctx, memory.WithSource(memory.MemoryEntry{
		Scope:   core.GlobalScope,
		Section: core.SectionArchive,
		Content: "伪造来源",
		State:   memoryStateActive,
	}, "forged"))
	require.ErrorContains(t, err, "不在闭集内")
	require.Equal(t, 0, memCount(t, db, "global", nil, "", ""), "拒绝路径不得留下半行")
}

// postgresLegacySource 是 PG 集成用例里的只读导入源。
// postgresLegacySource feeds import fixtures into ImportLegacy.
type postgresLegacySource struct {
	entries []memory.LegacyEntry
}

// Read 返回预置条目。
func (s postgresLegacySource) Read(context.Context) ([]memory.LegacyEntry, error) {
	return s.entries, nil
}

// TestImportLegacy_PostgresReconcilesAndIsIdempotent 在真库上守 T130 / D-18：导入行写
// `legacy_import` 且回读可见，重复导入零新增，冲突行（已删除）显式报告且原状不动。
// 逆改点：① 导入不盖来源 → source 断言红；② 幂等键失效 → 第二次 imported 非零；
// ③ 冲突行被改写 → 状态断言红。
func TestImportLegacy_PostgresReconcilesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, store := memTestStore(t)
	svc := memory.NewSemanticStore(memory.SemanticDeps{Store: store})

	liveID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope:   core.GlobalScope,
		Section: core.SectionArchive,
		Content: "已在库中",
		State:   memoryStateActive,
	})
	require.NoError(t, err)
	deletedID, err := store.InsertEntry(ctx, memory.MemoryEntry{
		Scope:   core.GlobalScope,
		Section: core.SectionArchive,
		Content: "已删除的知识",
		State:   memoryStateActive,
	})
	require.NoError(t, err)
	require.NoError(t, store.UpdateEntryState(ctx, deletedID, memoryStateDeleted, ""))

	source := postgresLegacySource{entries: []memory.LegacyEntry{
		{Scope: core.GlobalScope, Section: core.SectionCore, Content: "偏好：先给结论"},
		{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "已在库中"},
		{Scope: core.GlobalScope, Section: core.SectionArchive, Content: "已删除的知识"},
	}}

	report, err := memory.ImportLegacy(ctx, source, svc)
	require.NoError(t, err)
	require.Equal(t, 3, report.Total())
	require.Len(t, report.Imported, 1)
	require.Len(t, report.Skipped, 1)
	require.Equal(t, liveID, report.Skipped[0].EntryID)
	require.Len(t, report.Conflicts, 1)
	require.Equal(t, deletedID, report.Conflicts[0].EntryID)
	require.Equal(t, memoryStateDeleted, report.Conflicts[0].State)

	importedID := report.Imported[0].EntryID
	require.Equal(t, "legacy_import", memSourceOf(t, db, importedID), "导入行在库里必须是 legacy_import")
	loaded, found, err := store.LoadEntry(ctx, importedID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, memory.SourceLegacyImport, loaded.Source())
	require.Equal(t, memoryStateActive, loaded.State, "旧载体条目导入后直接生效")
	require.Equal(t, 3, memCount(t, db, "global", nil, "", ""), "两行预置 + 一行导入")

	var conflictState string
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT state FROM memory_entries WHERE id = $1`, deletedID).Scan(&conflictState))
	require.Equal(t, memoryStateDeleted, conflictState, "冲突行不得被导入改写")

	second, err := memory.ImportLegacy(ctx, source, svc)
	require.NoError(t, err)
	require.Empty(t, second.Imported, "重复导入不得再写一行")
	require.Len(t, second.Skipped, 2, "已导入的那条第二次也必须按跳过记账")
	require.Len(t, second.Conflicts, 1)
	require.Equal(t, 3, memCount(t, db, "global", nil, "", ""), "权威库行数保持不变")
}
