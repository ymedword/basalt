package memory_test

// 本文件是 markdown 载体的验收面（颗粒度文档 §3.2，共 8 条）：US2 / T016 落 4 条基础
// ——按需创建、分区落位、Load 原文、配额求和口径；US4 / T039 再落 3 条——条目级整串检索、
// 原子替换无残留、解析器容错与损坏三形态；US4 / T041（R9）落末条——损坏文件**写拒 +
// 原文件逐字节不变**。
// This file is the markdown carrier's acceptance surface: four basics from US2, three more
// with T039, and the damaged-file write refusal with T041.

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// ctx 是本包测试共用的背景上下文（**包级**：`carrier_contract_test.go` 的 R3 代码按
// §4.3 原样落地，其中直接引用 `ctx` 而不定义——三文件共享这一份）。
// ctx is shared package-wide: the R3 snippet in §4.3 references it without defining it.
var ctx = context.Background()

// 全局层文件标题的逐字字面量（与 `workspace.templates.go` 的 `templateMemoryMD` 同源）。
// The verbatim global-layer file title, shared with the workspace template.
const globalMemoryTitle = "# MEMORY.md —— 长期记忆 · 全局层"

// TestMarkdownStore_AppendCreatesAgentLayerOnDemand 守「Agent 层文件与目录按需创建」
// （约束 15）：`memory/agents` 有意不预建（`templates.go` 注释逐字），首次写入时建目录、
// 写出骨架 = 与 `templateMemoryMD` 同形（文件标题 + 两分区标题）。
// 验收点：约束 15 / 口径 14。
func TestMarkdownStore_AppendCreatesAgentLayerOnDemand(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)

	// 前置事实：Agent 层目录在首次写入前**不存在**——按需创建的前提是真的"按需"。
	_, statErr := os.Stat(filepath.Join(root, "memory", "agents", "alice"))
	require.ErrorIs(t, statErr, os.ErrNotExist, "memory/agents/<name> 有意不预建")

	require.NoError(t, store.Append(ctx, core.Scope{Agent: "alice"}, core.SectionCore, "- alice 偏好"))

	raw, err := os.ReadFile(filepath.Join(root, "memory", "agents", "alice", "MEMORY.md"))
	require.NoError(t, err, "首次写入即创建：目录与文件都按需建")

	text := string(raw)
	lines := strings.Split(text, "\n")
	require.True(t, strings.HasPrefix(lines[0], "# "),
		"第 1 行恒为文件标题（形态判据；Agent 层标题文本未在事实源里冻结），实际 %q", lines[0])
	require.Contains(t, text, "## 核心记忆", "骨架与 templateMemoryMD 同形：两分区标题齐备")
	require.Contains(t, text, "## 归档记忆")
}

// TestMarkdownStore_AppendWritesToRequestedSection 守「写入分区由调用方显式指定」（约束 8）：
// 同一层的两次写入分别落核心区与归档区，条目**不得**跨区。
// 验收点：约束 8 / 口径 22。
func TestMarkdownStore_AppendWritesToRequestedSection(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	scope := core.Scope{Agent: "alice"}

	require.NoError(t, store.Append(ctx, scope, core.SectionCore, "- 核心条目"))
	require.NoError(t, store.Append(ctx, scope, core.SectionArchive, "- 归档条目"))

	raw, err := os.ReadFile(filepath.Join(root, "memory", "agents", "alice", "MEMORY.md"))
	require.NoError(t, err)
	text := string(raw)

	coreIdx := strings.Index(text, "## 核心记忆")
	archiveIdx := strings.Index(text, "## 归档记忆")
	require.GreaterOrEqual(t, coreIdx, 0, "核心区标题必须在场")
	require.Greater(t, archiveIdx, coreIdx, "次序：核心区在前、归档区在后（约束 27 第三条结构事实）")

	coreBlock, archiveBlock := text[coreIdx:archiveIdx], text[archiveIdx:]
	require.Contains(t, coreBlock, "- 核心条目")
	require.NotContains(t, coreBlock, "- 归档条目", "分区写入落位对号：条目不得跨区")
	require.Contains(t, archiveBlock, "- 归档条目")
	require.NotContains(t, archiveBlock, "- 核心条目")
}

// TestMarkdownStore_LoadReturnsFileVerbatim 守「`Load` 返回文件原文」（口径 12 / 22）：
// 注入面读的是**重建结果**，`Load` 面读的是**原文**——文件本身不被改写，说明段与裸段落
// 一并原样返回（两档 `Load` 文本不承诺逐字节相同，正源于此）。
// 验收点：口径 12。
func TestMarkdownStore_LoadReturnsFileVerbatim(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)

	manual := globalMemoryTitle + "\n\n" +
		"全局层长期记忆：所有 Agent 共享。\n\n" +
		"## 核心记忆\n\n- 手写的偏好\n\n" +
		"## 归档记忆\n\n- 手写的归档\n"
	require.NoError(t, os.MkdirAll(filepath.Join(root, "memory"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "memory", "MEMORY.md"), []byte(manual), 0o644))

	got, err := store.Load(ctx, core.GlobalScope)
	require.NoError(t, err)
	require.Equal(t, manual, got, "Load 逐字节返回文件原文（含说明段与裸段落，零改写）")

	onDisk, err := os.ReadFile(filepath.Join(root, "memory", "MEMORY.md"))
	require.NoError(t, err)
	require.Equal(t, manual, string(onDisk), "读取面不得改写文件本身（口径 22）")
}

// TestMarkdownStore_AcceptsCRLFFromManualEdit 守「Windows 记事本手工保存的 CRLF 文件
// 与 LF 文件同语义」（FR-015 / FR-018 的跨平台边界）：按 `\n` 切行后必须先去掉行尾
// `\r` 再判结构——否则 `## 核心记忆\r` 不等于 `## 核心记忆`，合法文件被误判成
// 「缺少分区标题」而拒绝启动。`Load` 仍逐字节返回原文（CRLF 保留）；注入面读到的是
// 重建结果（LF 行尾），正文里的空行不得被误当成条目分界。
// 验收点：FR-015 / FR-018；Windows 手工编辑边界（第二轮复核 F-02）。
func TestMarkdownStore_AcceptsCRLFFromManualEdit(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	file := filepath.Join(root, "memory", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))

	// 多行条目中间有一个**空行**（CRLF 下是 "\r\n\r\n"）：解析器若把 "\r" 当零缩进行，
	// 会在空行处提前切块，把一条拆成两条。归档条目用于验证写入面重建同样逐条保留。
	manual := globalMemoryTitle + "\r\n\r\n" +
		"## 核心记忆\r\n\r\n" +
		"- 第一行\r\n  第二行\r\n\r\n  空行后的续行\r\n\r\n" +
		"- 第二条\r\n\r\n" +
		"## 归档记忆\r\n\r\n" +
		"- 归档条目 SQLite 方案\r\n"
	require.NoError(t, os.WriteFile(file, []byte(manual), 0o644))

	// ① Load 逐字节返回原文：兼容 CRLF 不等于改写文件。
	got, err := store.Load(ctx, core.GlobalScope)
	require.NoError(t, err, "CRLF 是合法的行尾形态，不得判成结构损坏")
	require.Equal(t, manual, got, "Load 原文逐字节返回（CRLF 保留）")

	// ② 注入面：核心区按条目重建为 LF 形态，空行留在条目正文内（不被当成条目分界）。
	svc, err := memory.NewService(memory.Options{Backend: store, Logger: testLogger(t)})
	require.NoError(t, err)
	effective, err := svc.LoadEffective(ctx, "")
	require.NoError(t, err)
	require.Equal(t,
		"## 核心记忆\n\n- 第一行\n  第二行\n\n  空行后的续行\n\n- 第二条",
		effective, "CRLF 文件的核心区按同一解析器重建（正文内的空行保留）")

	// ③ 写入面：在 CRLF 文件上追加一笔 → 重建为文档化的 LF 形态，原条目一条不丢。
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "新归档条目"))
	rebuilt, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t,
		globalMemoryTitle+"\n\n## 核心记忆\n\n- 第一行\n  第二行\n\n  空行后的续行\n\n- 第二条"+
			"\n\n## 归档记忆\n\n- 归档条目 SQLite 方案\n\n- 新归档条目\n",
		string(rebuilt), "写入重建使用文档化行尾（LF），原条目逐条保留")
}

// TestMarkdownStore_QuotaCountsEntryBytesExcludingScaffold 守「条目正文的规范表示与配额
// 求和口径」（约束 29 / 口径 23）：求和单位 = 规范化后条目正文自身的 UTF-8 字节，
// **只排除载体添加物**（文件标题 / 分区标题 / `"- "` 前缀 / 续行缩进 / 条目间空行）。
// 五段子断言——① 恰 4096 放行（`"- "` 前缀不计入）② 中文按 3 字节/字符 ③ 续行缩进不计入
// （内部换行计入）④ 首尾空白不计入（写入入口 `normalizeEntry` 先 `TrimSpace`）⑤ 总量恰
// 32768 恰好到顶放行、超 1 字节拒（文件标题与分区标题不计入）。
// 验收点：约束 29 / 口径 23；P1-b 子断言。
func TestMarkdownStore_QuotaCountsEntryBytesExcludingScaffold(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	scope := core.Scope{Agent: "quota"}

	// ① 单条恰 4096 字节 == 放行（恰在上限放行）。若把 "- " 前缀计入，这一条会被误拒。
	require.NoError(t, store.Append(ctx, scope, core.SectionCore, strings.Repeat("x", 4096)),
		"恰在上限 == 放行：只数条目正文自身的字节")

	// ② 中文按 UTF-8 字节（3 字节/字符）：1365 字 = 4095 B 放行。
	require.NoError(t, store.Append(ctx, scope, core.SectionCore, strings.Repeat("中", 1365)),
		"1365 个汉字 = 4095 B ≤ 4096")

	// ③ 续行缩进不计入、内部换行计入：2048 行 "x" 正文 = 2048 + 2047 换行 = 4095 B 放行。
	//    若把载体添加的续行缩进（每行两空格）计入，这一条会是 8000+ 字节而误拒。
	require.NoError(t, store.Append(ctx, scope, core.SectionCore, strings.Repeat("x\n", 2047)+"x"),
		"多行条目：缩进不计入、内部换行计入")

	// ④ 首尾空白不计入：入参原长 4100 字节、规范化后 4096 字节 → 放行。
	require.NoError(t, store.Append(ctx, scope, core.SectionCore, "  "+strings.Repeat("x", 4096)+"\n\n"),
		"首尾空白由 normalizeEntry 去掉，不计入配额")

	// ⑤ 单条越限：1366 字 = 4098 B > 4096 → 拒，且类型化判据是单条档。
	var quota *core.MemoryQuotaError
	err := store.Append(ctx, scope, core.SectionCore, strings.Repeat("中", 1366))
	require.ErrorAs(t, err, &quota, "1366 个汉字 = 4098 B > 4096")
	require.Equal(t, core.QuotaKindEntry, quota.Kind)

	// ⑥ 总量边界：前四段已用 4096+4095+4095+4096 = 16382 字节，补四条 4096 → 32766，
	//    再补 2 字节恰 32768 恰好到顶放行、1 字节即拒——文件标题与分区标题若被计入，
	//    这几笔会提前撞线。
	for i := 0; i < 4; i++ {
		require.NoError(t, store.Append(ctx, scope, core.SectionCore, strings.Repeat("y", 4096)))
	}
	require.NoError(t, store.Append(ctx, scope, core.SectionCore, "ab"), "恰 32768 == 放行")
	err = store.Append(ctx, scope, core.SectionCore, "c")
	require.ErrorAs(t, err, &quota, "32769 > 32768 → 总量拒")
	require.Equal(t, core.QuotaKindSection, quota.Kind)
}

// requireNoTempResidue 扫全树判「零临时文件残留」（约束 12）：原子替换的临时文件形如
// `<名字>.tmp-<16 位十六进制>`，成功与失败两条路都必须清干净——残留会让"文件树即记忆"
// 的承诺失真（巡检看到多余文件），也让下一次写入的目录快照对不上。stage 进失败文案：
// 同一函数在多处调用，红态必须说清是**哪一步**留下的（三处调用点各取一个阶段名）。
// requireNoTempResidue fails on any leftover temp file anywhere under the root, naming
// the stage that left it.
func requireNoTempResidue(t *testing.T, root, stage string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(d.Name(), ".tmp-") {
			t.Errorf("临时文件残留（%s）：%s", stage, path)
		}
		return nil
	})
	require.NoError(t, err)
}

// TestMarkdownStore_RecallMatchesWholeStringWithinEntries 守归档区检索的四条口径
// （约束 9 / 口径 12）：**条目级**（匹配域是"某一条的**完整正文**"——多行条目的续行
// 也是正文，不跨条目拼）的**整串**子串匹配、**区分大小写**、**按写入顺序**返回；
// 未命中 = 空 + nil（口径 10）。
// 逆改点：把匹配换成分词（查询按空白切词、逐词 `Contains`）→ 「分散词不命中」那一条红
// ——**同一条正文里**分开出现的两个词会被拼成一次命中（口径 12 的判别力）。
// 验收点：约束 9 / 10（注 6 两态）。
func TestMarkdownStore_RecallMatchesWholeStringWithinEntries(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)

	// 第一条是**多行条目**：连续短语 `SQLite 方案 评估` 在首行、`细节另见附录` 在续行
	// ——匹配域须覆盖完整正文（只在首行里找的实现会把 ② 判成未命中）。
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "结论：SQLite 方案 评估\n已完成，细节另见附录"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "SQLite 是本项目的存储选型"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "评估完成，结论另见附录"))
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "核心区也提到 SQLite 方案 评估"))

	// ① 连续短语命中：查询串是第一条正文里的**连续子串** → 命中，返回的是**完整正文**
	//    （含换行）——不是被截断的首行。
	hits, err := store.RecallByKeyword(ctx, core.GlobalScope, "SQLite 方案 评估")
	require.NoError(t, err)
	require.Equal(t, []string{"结论：SQLite 方案 评估\n已完成，细节另见附录"}, hits,
		"核心区那条含同一短语，但核心区不参与检索")

	// ② 匹配域含续行：查询只出现在第一条的**第二行**上 → 仍命中（条目级 = 整条正文级）。
	hits, err = store.RecallByKeyword(ctx, core.GlobalScope, "细节另见附录")
	require.NoError(t, err)
	require.Equal(t, []string{"结论：SQLite 方案 评估\n已完成，细节另见附录"}, hits,
		"续行也是条目正文的一部分（匹配域不是首行）")

	// ③ 单关键词命中两条 → 按**写入顺序**返回（文件次序即顺序）。
	hits, err = store.RecallByKeyword(ctx, core.GlobalScope, "SQLite")
	require.NoError(t, err)
	require.Equal(t, []string{"结论：SQLite 方案 评估\n已完成，细节另见附录", "SQLite 是本项目的存储选型"}, hits,
		"核心区条目不得出现（核心区每轮已完整注入，不参与检索）")

	// ④ 分散词不自动组合：`SQLite` 与 `评估` **同在第一条正文里但不相邻**，整串
	//    `SQLite 评估` 不是任何一条的子串 → 不命中。分词式实现会在这里误报第一条。
	hits, err = store.RecallByKeyword(ctx, core.GlobalScope, "SQLite 评估")
	require.NoError(t, err)
	require.Empty(t, hits, "分散词不自动组合——匹配是条目正文里的连续子串")

	// ⑤ 区分大小写：归档区里是 `SQLite`，小写查询不命中。
	hits, err = store.RecallByKeyword(ctx, core.GlobalScope, "sqlite")
	require.NoError(t, err)
	require.Empty(t, hits, "检索区分大小写，不得做大小写归一")

	// ⑥ 未命中 = 空 + nil（口径 10：不是错误）。
	hits, err = store.RecallByKeyword(ctx, core.GlobalScope, "没有这条")
	require.NoError(t, err, "未命中是成功取值，不是失败")
	require.Empty(t, hits)
}

// TestMarkdownStore_AppendReplacesAtomicallyWithoutResidue 守「唯一名临时文件 + rename
// 原子替换」（约束 12）：替换走**同一目录**的临时文件，成功与失败两条路都**零残留**；
// 被拒的写入**不触碰原文件一个字节**（拒绝发生在重建与替换之前）。
// 逆改点：把失败路径的清理删掉（在配额拒绝前先建临时文件且不删）→ 残留断言红。
// 边界（如实登记）：**写入过程中的 I/O 失败**无法在导出面跨平台制造（Windows 的访问位
// 与 POSIX 不同形），故本条的"失败态"取**确定可复现**的拒绝档（配额）；`tempFailure` 的
// 清理分支因此不在本用例的判据面内。
// 验收点：约束 12 / 口径 14。
func TestMarkdownStore_AppendReplacesAtomicallyWithoutResidue(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	scope := core.Scope{Agent: "atomic"}
	file := filepath.Join(root, "memory", "agents", "atomic", "MEMORY.md")

	// ① 首次写入（含按需建目录）→ 零残留。
	require.NoError(t, store.Append(ctx, scope, core.SectionCore, "第一条"))
	requireNoTempResidue(t, root, "首次写入后")

	// ② 第二笔 = 走"读-改-写"的替换路径：两分区各就各位，替换后仍零残留。
	require.NoError(t, store.Append(ctx, scope, core.SectionArchive, "第二条"))
	requireNoTempResidue(t, root, "替换后")
	replaced, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, []string{"第一条"}, sectionBodiesForTest(t, string(replaced), coreHeading),
		"替换 = 旧内容 + 新条目都还在（不是覆盖成只有新内容）")
	require.Equal(t, []string{"第二条"}, sectionBodiesForTest(t, string(replaced), archiveHeading))

	// ③ 失败态：单条越限被拒 → 原文件**逐字节不变**，且不得留下临时文件。
	before, err := os.ReadFile(file)
	require.NoError(t, err)
	var quota *core.MemoryQuotaError
	err = store.Append(ctx, scope, core.SectionCore, strings.Repeat("x", core.MaxCoreEntryBytes+1))
	require.ErrorAs(t, err, &quota, "越限单条必须被拒")
	requireNoTempResidue(t, root, "被拒后")

	after, err := os.ReadFile(file)
	require.NoError(t, err, "被拒之后文件仍然可读（拒绝不是删文件）")
	require.Equal(t, string(before), string(after), "被拒的写入不得改动原文件一个字节")
}

// TestMarkdownStore_ParseToleratesExtraHeadingsAndRejectsBrokenShape 守解析器的**容错边界**
// （约束 28 / 口径 21）：合法扩展（多余 `### 小节` 标题、裸段落）**不算损坏**——按条目收集、
// 重建时补上载体形态（`"- "` 前缀与续行缩进）**原样保留**，人手加个小节标题不该让整个 Agent
// 起不来；结构损坏只有**三条事实**（缺分区标题 / 同一标题出现两次 / 归档区早于核心区），
// 每一条都必须报错并**点名症状与文件**，不得当空层。
// 逆改点：把任何 `#` 开头的行都当结构行（吞掉 `### 小节`）→ 重建后的条目表少一条，断言红；
// 把三条结构事实的判定删掉（`!inCore || !inArchive` 不再报错）→ 三种畸形文件全部静默通过，
// 三条 `require.Error` 皆红。
// 验收点：口径 21 / 约束 27 / 28。
func TestMarkdownStore_ParseToleratesExtraHeadingsAndRejectsBrokenShape(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	file := filepath.Join(root, "memory", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))

	// ① 合法扩展：多余的 `### 小节` 与裸段落都在核心区里 → Load 不报损坏、逐字节返回原文。
	extended := globalMemoryTitle + "\n\n## 核心记忆\n\n- 已有条目\n\n### 小节标题\n\n裸段落正文\n\n## 归档记忆\n"
	require.NoError(t, os.WriteFile(file, []byte(extended), 0o600))

	got, err := store.Load(ctx, core.GlobalScope)
	require.NoError(t, err, "多余的 ### 小节与裸段落是合法扩展，不得判损坏")
	require.Equal(t, extended, got, "Load 面逐字节返回文件原文——扩展内容不被改写")

	// ② 写入一笔触发重建：合法扩展**按条目保留**（补 `"- "` 前缀进核心区），一条不丢。
	require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "新条目"))
	rebuilt, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, []string{"已有条目", "### 小节标题", "裸段落正文", "新条目"},
		sectionBodiesForTest(t, string(rebuilt), coreHeading), "合法扩展按条目原样重建（顺序不变、不丢失）")
	require.Equal(t, []string{globalMemoryTitle, coreHeading, archiveHeading},
		structureLinesForTest(t, string(rebuilt)), "重建后仍恰是一个文件标题 + 两个分区标题（多余标题不成为第三分区）")

	// ③ 结构损坏三形态：各自报错并点名症状（缺哪个 / 重复哪个 / 次序错）；`Load` 与
	//    `RecallByKeyword` 两条读取面**都**不得当空层（写入面的拒绝见 T041 的 R9）。
	broken := []struct {
		name    string
		content string
		symptom string
	}{
		{
			name:    "缺归档区标题",
			content: globalMemoryTitle + "\n\n## 核心记忆\n\n- 只有核心区\n",
			symptom: archiveHeading,
		},
		{
			name:    "核心区标题重复",
			content: globalMemoryTitle + "\n\n## 核心记忆\n\n- 一条\n\n## 核心记忆\n\n- 又一条\n\n## 归档记忆\n",
			symptom: coreHeading,
		},
		{
			name:    "归档区早于核心区",
			content: globalMemoryTitle + "\n\n## 归档记忆\n\n- 归档条目\n\n## 核心记忆\n\n- 核心条目\n",
			symptom: "次序",
		},
	}

	for _, b := range broken {
		t.Run(b.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(file, []byte(b.content), 0o600))

			_, loadErr := store.Load(ctx, core.GlobalScope)
			require.Error(t, loadErr, "结构损坏必须报错，不得当空层回退")
			require.Contains(t, loadErr.Error(), b.symptom, "错误必须点名症状（缺哪个 / 重复哪个 / 次序错）")
			require.Contains(t, loadErr.Error(), "memory/MEMORY.md", "错误必须点名文件")

			_, recallErr := store.RecallByKeyword(ctx, core.GlobalScope, "条目")
			require.Error(t, recallErr, "检索面同样不得把损坏文件当空层")

			onDisk, readErr := os.ReadFile(file)
			require.NoError(t, readErr)
			require.Equal(t, b.content, string(onDisk), "读取面不得改写损坏文件（修复不是读取面的事）")
		})
	}
}

// TestMarkdownStore_AppendRefusesCorruptedFileKeepsOriginalBytes 守「结构损坏拒绝**覆盖**」
// （约束 27 第三档 / 口径 21）：缺 `## 归档记忆` 的人手文件，读取面报错点名症状与文件，
// **写入面同样必须拒绝**——重建会把人手写的内容静默删掉（归档区标题被重新生成，原有
// 条目与说明段都消失）。被拒之后原文件逐字节不变：拒绝不是"先修好再说"，也不是删文件。
// 与 T039 的 `TestMarkdownStore_ParseToleratesExtraHeadingsAndRejectsBrokenShape` 分工：
// 那里判三种畸形形态的**读取面**（Load / Recall 的症状点名），本条判**写入面**的拒绝。
// 逆改点：把 `Append` 里 `loadLayer` 的错误吞掉（当成空层继续重建）→ 写入成功、损坏文件
// 被两分区骨架覆盖，写拒与逐字节不变两条断言皆红（见 T041 的 overlay 探针）。
// 验收点：约束 27 第三档 / 口径 21。
func TestMarkdownStore_AppendRefusesCorruptedFileKeepsOriginalBytes(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	file := filepath.Join(root, "memory", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))

	// 缺归档区标题的损坏文件：说明段与核心区都在，人手内容是真实的（不是空洞样例）。
	manual := globalMemoryTitle + "\n\n## 核心记忆\n\n- 手写的偏好\n"
	require.NoError(t, os.WriteFile(file, []byte(manual), 0o600))

	// ① 读取面先报错：点名缺的分区标题与文件（判据与 T039 同源，此处作前置取证）。
	_, loadErr := store.Load(ctx, core.GlobalScope)
	require.Error(t, loadErr, "结构损坏必须报错，不得当空层")
	require.Contains(t, loadErr.Error(), archiveHeading, "错误必须点名缺的分区标题")
	require.Contains(t, loadErr.Error(), "memory/MEMORY.md", "错误必须点名文件")

	// ② 写入面拒绝覆盖：核心区与归档区**都**不得绕过读取面的损坏判定。
	coreAppendErr := store.Append(ctx, core.GlobalScope, core.SectionCore, "新核心条目")
	archiveAppendErr := store.Append(ctx, core.GlobalScope, core.SectionArchive, "新归档条目")
	require.Error(t, coreAppendErr, "损坏文件拒绝覆盖：重建会静默删掉人手写的内容")
	require.Error(t, archiveAppendErr, "归档区写入同样不得绕过损坏判定")

	// ③ 被拒之后原文件逐字节不变。
	after, err := os.ReadFile(file)
	require.NoError(t, err, "被拒之后文件仍然可读（拒绝不是删文件）")
	require.Equal(t, manual, string(after), "被拒的写入不得改动原文件一个字节")
}

// structureLinesForTest 取重建文本里的**结构行**（零缩进的文件标题与分区标题）——用于判
// 「多余标题没有变成第三分区」。判定与 `archiveEntriesForTest` 同源：`## ` 开头是分区标题、
// `# ` 开头是文件标题（`### ` 两者都不是，故一旦它出现在结构化位置上就会在此显形）。
// The structural lines of a rebuilt file, for the "no third section" assertion.
func structureLinesForTest(t *testing.T, text string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "## "), strings.HasPrefix(line, "# "):
			lines = append(lines, line)
		}
	}
	return lines
}
