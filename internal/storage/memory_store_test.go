package storage

// 本文件是 sqlite 记忆载体的验收面（颗粒度文档 §3.2，共 6 条）：本批次（US2）落 5 条
// ——六列往返与规范化、整串召回、全局层 NULL 形状、拒绝路径零副作用、渲染可解析；
// US4 / T044 落第 6 条——R6（配额检查与追加同事务，并发不穿透上限）。
// The sqlite carrier's acceptance surface; R6 lands with US4.

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestMemoryStore_AppendAndLoadRoundTrip 守「六列逐列可读 + 时钟显式注入 + 存的是规范化
// 文本」（§3.4 / 约束 25 / 29）：`created_at` 来自构造器传入的时钟而非 `time.Now`，且随
// 写入递增；`content` 列 = `strings.TrimSpace(原文)`（P1-b 子断言——与 markdown 档同一
// 标准库函数、同一表示）。
// 验收点：§3.4 · 约束 25 / 29。
func TestMemoryStore_AppendAndLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)

	// 时钟每次调用前进一秒：`created_at` 因此**可判**——既证明它取自注入时钟（首笔落在
	// 1700000001 而非"大约现在"），又证明逐笔递增（约束 25：时间显式传入）。
	tick := time.Unix(1700000000, 0)
	store := NewMemoryStore(db, func() time.Time {
		tick = tick.Add(time.Second)
		return tick
	})
	scope := core.Scope{Agent: "alice"}

	require.NoError(t, store.Append(ctx, scope, core.SectionCore, "- alice 的偏好"))
	require.NoError(t, store.Append(ctx, scope, core.SectionArchive, "  带首尾空白的条目  "))

	var (
		id        int64
		scopeCol  string
		agentName sql.NullString
		section   string
		content   string
		createdAt int64
	)
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT id, scope, agent_name, section, content, created_at
		   FROM memory_entries ORDER BY id LIMIT 1`).
		Scan(&id, &scopeCol, &agentName, &section, &content, &createdAt))
	require.Equal(t, "agent", scopeCol, "scope 列记分层（≠ 工具参数 scope——那是分区）")
	require.True(t, agentName.Valid, "Agent 层落 Agent 名而非 NULL")
	require.Equal(t, "alice", agentName.String)
	require.Equal(t, "core", section, "section 列记分区")
	require.Equal(t, "- alice 的偏好", content)
	require.Equal(t, time.Unix(1700000001, 0).UnixMilli(), createdAt, "created_at 由构造器时钟显式写入，存 Unix 毫秒 UTC（契约 schema.sql 迁移纪律 4）")

	firstID, firstCreated := id, createdAt
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT id, scope, agent_name, section, content, created_at
		   FROM memory_entries ORDER BY id LIMIT 1 OFFSET 1`).
		Scan(&id, &scopeCol, &agentName, &section, &content, &createdAt))
	require.Equal(t, "archive", section)
	require.Equal(t, "带首尾空白的条目", content, "存的是规范化文本：读回无首尾空白（P1-b）")
	require.Greater(t, id, firstID, "id 是写入顺序的事实源")
	require.Greater(t, createdAt, firstCreated, "created_at 随写入递增")

	loaded, err := store.Load(ctx, scope)
	require.NoError(t, err)
	require.Contains(t, loaded, "- alice 的偏好")
	require.Contains(t, loaded, "- 带首尾空白的条目")
}

// TestMemoryStore_RecallUsesInstrOnArchiveOrderedByID 守「召回 = 归档区 + 整串 + 区分
// 大小写 + 按写入顺序」（约束 9）：`instr` 是字节级子串匹配——不做分词、不做多词组合、
// 不做大小写折叠；核心区不参与检索；未命中是空结果 + nil error（口径 10）。
// 验收点：约束 9 / 10。
func TestMemoryStore_RecallUsesInstrOnArchiveOrderedByID(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewMemoryStore(db, func() time.Time { return time.Unix(1700000000, 0) })

	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "部署窗口在周二"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "回滚流程见 runbook"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "部署前置检查清单"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "Runbook 首字母大写版"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "部署偏好放核心区"))

	// ① 整串命中 + 按 id 升序 + 不越区：核心区那条同词条目不得出现。
	got, err := store.RecallByKeyword(ctx, core.GlobalScope, "部署")
	require.NoError(t, err)
	require.Equal(t, []string{"部署窗口在周二", "部署前置检查清单"}, got,
		"instr 整串命中：按写入顺序返回、核心区不参与检索")

	// ② 分散词不自动组合：query 作为一个整串匹配，"部署"与"清单"分开出现不算命中。
	got, err = store.RecallByKeyword(ctx, core.GlobalScope, "部署 清单")
	require.NoError(t, err)
	require.Empty(t, got, "不做分词、不做多词组合")

	// ③ 区分大小写：小写 query 不命中大写版。
	got, err = store.RecallByKeyword(ctx, core.GlobalScope, "runbook")
	require.NoError(t, err)
	require.Equal(t, []string{"回滚流程见 runbook"}, got, "大小写敏感")

	// ④ 未命中不是错误（口径 10）：空结果 + nil error，不是 not_found / runtime_error。
	got, err = store.RecallByKeyword(ctx, core.GlobalScope, "查无此词")
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestMemoryStore_GlobalScopeUsesNullAgentName 守全局层的存储形状（§3.4）：`scope='global'`
// 的行 `agent_name` **IS NULL**——不得用空串顶替（查询恒用 `IS NULL`，空串会让取数静默
// 返回零行）。写入与读取两侧都要对上。
// 验收点：§3.4。
func TestMemoryStore_GlobalScopeUsesNullAgentName(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewMemoryStore(db, func() time.Time { return time.Unix(1700000000, 0) })

	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "全局偏好"))
	require.NoError(t, store.Append(ctx, core.Scope{Agent: "alice"}, core.SectionCore, "alice 的偏好"))

	var nullCount, emptyCount, agentCount int
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_entries WHERE scope = 'global' AND agent_name IS NULL`).
		Scan(&nullCount))
	require.Equal(t, 1, nullCount, "全局层落 NULL")
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_entries WHERE scope = 'global' AND agent_name = ''`).
		Scan(&emptyCount))
	require.Zero(t, emptyCount, "不得用空串顶替 NULL")
	require.NoError(t, db.read.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_entries WHERE scope = 'agent' AND agent_name = 'alice'`).
		Scan(&agentCount))
	require.Equal(t, 1, agentCount, "Agent 层按 Agent 名取数")

	// 读取面用同一查询形状：Load 取得到全局层（若写入侧落了空串，这里是零行）。
	loaded, err := store.Load(ctx, core.GlobalScope)
	require.NoError(t, err)
	require.Contains(t, loaded, "全局偏好")
}

// TestMemoryStore_InsertFailureLeavesNoRow 守「失败不留半行」（约束 12）：配额判定与
// 追加在同一事务里——被拒的追加不得留下任何行，也不得改动既有行。
// 验收点：约束 12。
func TestMemoryStore_InsertFailureLeavesNoRow(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewMemoryStore(db, func() time.Time { return time.Unix(1700000000, 0) })
	scope := core.Scope{Agent: "alice"}

	countRows := func() int {
		var n int
		require.NoError(t, db.read.QueryRowContext(ctx, `SELECT count(*) FROM memory_entries`).Scan(&n))
		return n
	}

	// 单条越限：拒绝 + 零行（连"先插一半再判"的可能都不留）。
	var quota *core.MemoryQuotaError
	err := store.Append(ctx, scope, core.SectionCore, strings.Repeat("a", core.MaxCoreEntryBytes+1))
	require.ErrorAs(t, err, &quota)
	require.Equal(t, core.QuotaKindEntry, quota.Kind)
	require.Zero(t, countRows(), "单条越限的拒绝路径零行")

	// 填到恰满（8 × 4096 = 32768，恰在上限 == 放行），再来 1 字节 → 总量拒 → 行数不变。
	for i := 0; i < 8; i++ {
		require.NoError(t, store.Append(ctx, scope, core.SectionCore, strings.Repeat("b", core.MaxCoreEntryBytes)),
			"第 %d 笔恰在上限内", i+1)
	}
	err = store.Append(ctx, scope, core.SectionCore, "c")
	require.ErrorAs(t, err, &quota)
	require.Equal(t, core.QuotaKindSection, quota.Kind)
	require.Equal(t, 8, countRows(), "总量越限的拒绝路径零副作用：不得留下第 9 行")
}

// TestMemoryStore_RenderedTextIsParseableBySharedParser 守「渲染文本与 markdown 档同形」
// （口径 12）：sqlite 档 `Load` 渲染出的规范文本，按**约束 28 的文档化形态**（同一套解析
// 规则——结构行只认零缩进的文件标题与分区标题，条目 = 零缩进 `"- "` 起行）能切出同一分区
// 结构；规则之外的任何行直接失败，渲染形态一漂它就红。
// 验收点：口径 12 / 约束 28。
func TestMemoryStore_RenderedTextIsParseableBySharedParser(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)
	store := NewMemoryStore(db, func() time.Time { return time.Unix(1700000000, 0) })
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

// R6 —— sqlite 档：配额检查与追加同事务，并发也不得穿透上限（storage 层）。
// 逆改点：把 SUM(octet_length) 查询挪到 withWriteLock 事务之外 → 两笔同时通过检查 → 总量超限，断言红。
func TestMemoryStore_ConcurrentAppendNeverExceedsCoreQuota(t *testing.T) {
	ctx := context.Background()
	db := openTempDB(t)                                                      // 既有夹具（llm_calls_test.go:41）：真库 + 全量迁移 + Cleanup 关闭
	store := NewMemoryStore(db, func() time.Time { return time.Unix(0, 0) }) // 内测包：同包直呼

	// 单条 20000 字节 > MaxCoreEntryBytes（4096）：单条判据先于总量判据，Kind = entry。
	var quota *core.MemoryQuotaError
	err := store.Append(ctx, core.GlobalScope, core.SectionCore, strings.Repeat("a", 20000))
	require.ErrorAs(t, err, &quota)
	require.Equal(t, core.QuotaKindEntry, quota.Kind)

	// 边界用 4096 字节条目：上限 32768 / 4096 = 恰 8 笔可成功；并发 12 笔里必须恰好 8 笔通过。
	// （若"求和"在事务之外，两笔并发会各查各的都在限内 → 双双落盘 → 穿透上限。）
	entry := strings.Repeat("b", 4096) // 恰在上限内（口径 6：恰在上限 == 放行）
	errs := make([]error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = store.Append(ctx, core.GlobalScope, core.SectionCore, entry)
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		require.ErrorAs(t, err, &quota, "第 %d 笔应被总量判据拒绝", i)
		require.Equal(t, core.QuotaKindSection, quota.Kind)
	}
	require.Equal(t, 8, succeeded, "并发 12 笔里只允许恰好 8 笔落盘，多一笔即穿透上限")
	// 到顶之后 1 字节都进不去——总额恰为 32768，未被任何并发穿透。
	require.ErrorAs(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "x"), &quota)
}
