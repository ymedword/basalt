package memory_test

// 本文件是记忆门面（`memory.Service`）的验收面（颗粒度文档 §3.2，共 10 条）：US2 落
// 4 条——作用域解析（T017）、生效层 = 核心区（T017）、fail-closed 构造（T017）、两层回退
// 不叠加的 R3（T018，另见 carrier_contract_test.go）；US4 再落 6 条——T038 整串检索与
// 配额类型化、T041 的 R9（读取面三档）、T042 的 R4a（注入侧不截断 + WARN）、T043 的并发
// 一条不丢与锁分档。
// This file is the facade's acceptance surface: four cases from US2, six more from US4.

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// testLogger 是"只求不响"的日志面（沿 `storage/recovery_test.go` 同名约定：丢弃级
// handler）。多数用例不判日志——注入期超配额 WARN 的判卷面在 US4（T046）另建。
// testLogger is the silent log surface, matching the storage package's convention.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captureWriter 是 R4a 的日志判卷面：captureLogger 写入、logs 取回。全包共一份——
// 只有判 WARN 的那条用例需要留痕（其余用例走 testLogger 丢弃）；本包用例不并行，
// 加锁防未来并行化时的数据竞争。每次构造清空：logs 只反映本次用例。
// captureWriter is the log-capture surface for R4a; the package shares one buffer
// because only the WARN case needs the trail.
var captureWriter = &logCapture{}

// logCapture 是内存日志缓冲（`slog` handler 的 `io.Writer` 面）。
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

// logs 取回目前捕获的日志文本。
func (c *logCapture) logs(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// captureLogger 返回"写进内存"的日志面（判 WARN 半轨用；其余用例走 testLogger）。
func captureLogger(t *testing.T) *slog.Logger {
	t.Helper()
	captureWriter.reset()
	return slog.New(slog.NewTextHandler(captureWriter, nil))
}

// TestService_AppendResolvesScopeFromAgentName 守「作用域解析只此一处」（口径 15 / 21）：
// 门面把 `core.Scope` 原样交给载体，路径由载体按 Scope 解析——Agent 名非空 →
// `memory/agents/<name>/MEMORY.md`，空（= `GlobalScope`）→ `memory/MEMORY.md`；
// 两层各自成文、互不串门。
// 验收点：口径 15 / 21。
func TestService_AppendResolvesScopeFromAgentName(t *testing.T) {
	root := t.TempDir()
	svc, err := memory.NewService(memory.Options{
		Backend: memory.NewMarkdownStore(root),
		Logger:  testLogger(t),
	})
	require.NoError(t, err)

	require.NoError(t, svc.Append(ctx, core.Scope{Agent: "alice"}, core.SectionCore, "- alice 的专有偏好"))
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "- 全局共享偏好"))

	agentRaw, err := os.ReadFile(filepath.Join(root, "memory", "agents", "alice", "MEMORY.md"))
	require.NoError(t, err, "Agent 层路径 = memory/agents/<name>/MEMORY.md")
	require.Contains(t, string(agentRaw), "alice 的专有偏好")
	require.NotContains(t, string(agentRaw), "全局共享偏好", "两层不串门")

	globalRaw, err := os.ReadFile(filepath.Join(root, "memory", "MEMORY.md"))
	require.NoError(t, err, "GlobalScope 路径 = memory/MEMORY.md")
	require.Contains(t, string(globalRaw), "全局共享偏好")
	require.NotContains(t, string(globalRaw), "alice 的专有偏好")
}

// TestService_LoadEffective_ReturnsCoreSectionOnly 守「注入面只拿核心区」（口径 2 / 3）：
// 生效层的返回值含核心区条目、**不含**归档区条目——归档只能经 `RecallByKeyword` 取回。
// 断言取「含 / 不含」而非全文相等：核心区文本的精确形态（是否带分区标题）不是本条的
// 判据面，层次与分区的归属才是。
// 验收点：口径 2 / 3。
func TestService_LoadEffective_ReturnsCoreSectionOnly(t *testing.T) {
	root := t.TempDir()
	svc, err := memory.NewService(memory.Options{
		Backend: memory.NewMarkdownStore(root),
		Logger:  testLogger(t),
	})
	require.NoError(t, err)

	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "- 每轮都要看的偏好"))
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive, "- 只在检索时露面的细节"))

	got, err := svc.LoadEffective(ctx, "")
	require.NoError(t, err)
	require.Contains(t, got, "每轮都要看的偏好", "生效层 = 核心区")
	require.NotContains(t, got, "只在检索时露面的细节", "归档区不进注入面")
}

// TestService_RequiresBackend 守「没有载体就没有记忆」（口径 18 同族）：`Options.Backend`
// 零值 → `NewService` 拒绝构造（fail-closed），且**不得**交出可用实例。
// 验收点：口径 18。
func TestService_RequiresBackend(t *testing.T) {
	svc, err := memory.NewService(memory.Options{Logger: testLogger(t)})
	require.Error(t, err, "Backend == nil 必须拒绝构造")
	require.Nil(t, svc, "拒绝构造时不得交出可用实例")
}

// R3 —— 两层回退且不叠加（memory 层，两档同一断言）。
// 逆改点：把两层内容拼接（叠加）→ ② 的"全局偏好"与"alice 偏好"同时出现，断言红；
// 生效层判据写成"**核心区**非空"（而非契约 §5.2 字面的"层非空"）→ ③ 里 bob 的层**只写了归档**
// 时判据会去回退全局层核心区、返回非空文本，断言红——这一步就是口径 2 的逆改点
// （原 R3 缺它：只有"层非空"与"核心区非空"**分叉**的场景才判得出这个差别）。
//
// 落地适配（唯一一处，**断言逻辑一字未动**）：§4.3 原样代码写的是 `svc := NewService(...)`
// 单返回值；T020 定的签名是 `(*Service, error)`（fail-closed），故此处补一行 error 判。
func TestService_LoadEffective_TwoLayerFallbackWithoutStacking(t *testing.T) {
	for _, c := range carriersForTest(t) { // markdown（t.TempDir 根）/ sqlite（storage.MemoryStore + 真实库文件）
		t.Run(c.name, func(t *testing.T) {
			svc, err := memory.NewService(memory.Options{Backend: c.store, Logger: testLogger(t)})
			require.NoError(t, err)
			agentScope := core.Scope{Agent: "alice"}

			// ① 全局层有内容、Agent 层空 → 生效层 = 全局层。
			require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "- 全局偏好"))
			got, err := svc.LoadEffective(ctx, "alice")
			require.NoError(t, err)
			require.Contains(t, got, "- 全局偏好")

			// ② Agent 层有核心条目 → 只注 Agent 层，全局层不叠加。
			require.NoError(t, svc.Append(ctx, agentScope, core.SectionCore, "- alice 偏好"))
			got, err = svc.LoadEffective(ctx, "alice")
			require.NoError(t, err)
			require.Contains(t, got, "- alice 偏好")
			require.NotContains(t, got, "- 全局偏好", "两层不叠加")

			// ③ 另一个 Agent 的层里**只有归档条目**：层非空 → 生效层仍是它自己这一层；
			//    核心区切出为空（段 2 不产生文本），**不得**因此回退去注全局层核心区。
			require.NoError(t, svc.Append(ctx, core.Scope{Agent: "bob"}, core.SectionArchive, "- bob 归档条目"))
			got, err = svc.LoadEffective(ctx, "bob")
			require.NoError(t, err)
			require.Empty(t, got, "Agent 层只有归档区 → 段 2 空（层非空即生效层，不回退全局层核心区）")

			// ④ 归档区不进注入面（口径 1）：alice 的归档条目同样不出现。
			require.NoError(t, svc.Append(ctx, agentScope, core.SectionArchive, "- alice 归档条目"))
			got, err = svc.LoadEffective(ctx, "alice")
			require.NoError(t, err)
			require.NotContains(t, got, "- alice 归档条目", "归档区不进系统提示词")

			// ⑤ 无 Agent 上下文（空串）= 全局层，是正常取值不是异常。
			got, err = svc.LoadEffective(ctx, "")
			require.NoError(t, err)
			require.Contains(t, got, "- 全局偏好")
		})
	}
}

// errorStore 是「读写恒失败」的载体桩（**I/O 失败档**专用，R9/T041 取用）：真文件的权限
// 错误无法跨平台复现（Windows 的访问位与 POSIX 不同形），故这一档由桩注入。**与结构损坏
// 档分开**——后者用**真 markdown 文件**造（`parseSections` 的三条结构事实靠它判）。
// errorStore is the always-failing carrier stub, used for the I/O tier only; the damaged
// tier is built from a real markdown file instead.
type errorStore struct {
	err error
}

// Append / RecallByKeyword 对本档无意义：R9 判的是读取面，两者同样回注入错误，让"误把桩
// 用在写面"的用例立刻可见，而不是静默通过。
func (s errorStore) Append(context.Context, core.Scope, core.Section, string) error {
	return s.err
}

// Load 恒回注入错误——三档里"读取失败必须上抛、不得当空层回退"的那一档。
func (s errorStore) Load(context.Context, core.Scope) (string, error) {
	return "", s.err
}

func (s errorStore) RecallByKeyword(context.Context, core.Scope, string) ([]string, error) {
	return nil, s.err
}

// 编译期断言：桩必须与门面同接口，否则 R9 的 I/O 档会退化成"编译不过"而不是"判据成立"。
var _ core.LongTermMemoryStore = errorStore{}

// R9 —— 读取面三档不得塌成一档（memory 层，无 Agent 上下文侧）。
// 逆改点：把 `text, _ := root.ReadFile(rel)`（或 `if ok {…}`）写回来 → ② 拿到 nil error
// 并回退全局层、③ 拿到 nil error 且把损坏文件当空层覆盖，三条断言皆红。
// 判据面分档：损坏形态用**真 markdown 文件**造（`parseSections` 的三条结构事实靠它判）；
// I/O 失败用**桩载体**注入（真文件造权限错误无法跨平台复现）。
//
// 落地适配（唯一一处，**断言逻辑一字未动**）：§4.3 原样代码写的是 `svc := NewService(...)`
// 单返回值；T020 定的签名是 `(*Service, error)`（fail-closed），故补 error 判（与 R3 同款）。
func TestService_LoadEffective_ErrorTiersDoNotCollapse(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	svc, err := memory.NewService(memory.Options{Backend: store, Logger: testLogger(t)})
	require.NoError(t, err)
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, "- 全局偏好"))

	// ① 文件不存在 = 空层（**不是**失败）：生效层回退到全局层核心区。
	got, err := svc.LoadEffective(ctx, "alice") // memory/agents/alice/MEMORY.md 尚不存在
	require.NoError(t, err, "文件不存在是正常取值，不是读取失败")
	require.Contains(t, got, "- 全局偏好", "缺失档照常回退全局层")

	// ② I/O 失败 = 读取失败：上抛，**不得**回退（注入面据此拒绝启动，约束 23）。
	failing, err := memory.NewService(memory.Options{Backend: errorStore{err: fs.ErrPermission}, Logger: testLogger(t)})
	require.NoError(t, err)
	got, err = failing.LoadEffective(ctx, "alice")
	require.Error(t, err, "I/O 失败必须上抛（不得当空层回退继续跑）")
	require.Empty(t, got, "失败时不得返回任何可注入文本")

	// ③ 非空但结构损坏（缺 `## 归档记忆`）：拒绝使用、拒绝覆盖、原文件逐字节不变。
	manual := "# MEMORY.md —— 长期记忆 · 全局层\n\n## 核心记忆\n\n- 手写的偏好\n"
	file := filepath.Join(root, "memory", "agents", "alice", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o750))
	require.NoError(t, os.WriteFile(file, []byte(manual), 0o600))

	got, err = svc.LoadEffective(ctx, "alice")
	require.Error(t, err, "结构损坏拒绝使用（不得当空层回退去注全局层）")
	require.Empty(t, got)
	require.Contains(t, err.Error(), "归档记忆", "错误文案点名声称与症状（缺哪个标题）")

	require.Error(t, svc.Append(ctx, core.Scope{Agent: "alice"}, core.SectionCore, "- 新条目"),
		"损坏文件拒绝覆盖：重建文件会静默删掉人手写的内容")
	after, readErr := os.ReadFile(file)
	require.NoError(t, readErr)
	require.Equal(t, manual, string(after), "被拒之后原文件逐字节不变")
}

// TestService_RecallByKeyword_WholeStringCaseSensitiveArchiveOnly 守归档区检索的四条口径
// （约束 9 / 10）：**整串子串**命中（连续短语命中——不分词、不同义词扩展、分散词不自动
// 组合）、**区分大小写**（不做大小写归一）、**只查归档区**（核心区每轮已完整注入，不参与
// 检索）、**按写入顺序**返回；未命中不是错误（空结果 + `nil`——不得映射成 not_found /
// runtime_error）。作用域仍是调用方给的：检索只在自己那一层里做。
// 验收点：约束 9 / 10（注 6 的两态判据在 markdown 档另有条目）。
func TestService_RecallByKeyword_WholeStringCaseSensitiveArchiveOnly(t *testing.T) {
	root := t.TempDir()
	svc, err := memory.NewService(memory.Options{
		Backend: memory.NewMarkdownStore(root),
		Logger:  testLogger(t),
	})
	require.NoError(t, err)
	aliceScope := core.Scope{Agent: "alice"}

	// 归档区三条（写入顺序即返回顺序）；另埋两条**干扰项**：同层核心区一条、全局层归档一条
	// ——"只查归档区"与"只查本层"两个边界都要判得出。写入的是**条目正文**（不带 `"- "`
	// 前缀——那是载体添加物，约束 28/29；召回取回的也是正文）。
	require.NoError(t, svc.Append(ctx, aliceScope, core.SectionArchive, "部署流程见 RUNBOOK"))
	require.NoError(t, svc.Append(ctx, aliceScope, core.SectionArchive, "回滚演练每月一次"))
	require.NoError(t, svc.Append(ctx, aliceScope, core.SectionArchive, "部署流程的补充说明"))
	require.NoError(t, svc.Append(ctx, aliceScope, core.SectionCore, "部署流程的核心记忆"))
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive, "部署流程的全局条目"))

	hits, err := svc.RecallByKeyword(ctx, aliceScope, "部署流程")
	require.NoError(t, err)
	require.Equal(t, []string{"部署流程见 RUNBOOK", "部署流程的补充说明"}, hits,
		"整串命中 + 按写入顺序；核心区条目与全局层条目都不得出现（核心区不参与检索，层不串门）")

	// 区分大小写：归档区里确有 `RUNBOOK`，小写查询**不**命中。
	hits, err = svc.RecallByKeyword(ctx, aliceScope, "runbook")
	require.NoError(t, err)
	require.Empty(t, hits, "检索区分大小写，不得做大小写归一")

	// 分散词不自动组合（注 6）：`部署` 与 `RUNBOOK` 都在归档区，整串 `部署 RUNBOOK` 不命中。
	hits, err = svc.RecallByKeyword(ctx, aliceScope, "部署 RUNBOOK")
	require.NoError(t, err)
	require.Empty(t, hits, "分散词不自动组合——检索是连续子串匹配")

	// 未命中 = 空结果 + nil error（口径 10：不是错误，也不是空指针/异常形状）。
	hits, err = svc.RecallByKeyword(ctx, aliceScope, "没有这条")
	require.NoError(t, err, "未命中是成功取值，不是失败")
	require.Empty(t, hits)
}

// TestService_AppendQuotaRejectionIsTyped 守配额拒绝的**类型化**判据（约束 7 / 16）：越限
// 返回 `*core.MemoryQuotaError`，调用方按 `Kind` 分档判——**不比文案**（文案是给模型读的
// 指引，不是判据）。两档各一条：① 单条越限（`entry`）；② 层内核心区总量越限（`section`）。
// ① 的内容**同时**越过两条上限，因此它还钉住**判定次序**：单条判据先于总量判据——把两条
// 检查互换，① 会拿到 `section`，断言红（本条判别力的来源）。
// 验收点：约束 7 / 16。
func TestService_AppendQuotaRejectionIsTyped(t *testing.T) {
	root := t.TempDir()
	svc, err := memory.NewService(memory.Options{
		Backend: memory.NewMarkdownStore(root),
		Logger:  testLogger(t),
	})
	require.NoError(t, err)

	var quota *core.MemoryQuotaError

	// ① 单条越限：32769 字节同时越过单条（4096）与总量（32768）两条上限 → 必须先报 entry。
	err = svc.Append(ctx, core.GlobalScope, core.SectionCore, strings.Repeat("a", core.MaxCoreSectionBytes+1))
	require.ErrorAs(t, err, &quota, "越限必须是类型化拒绝（errors.As 可判）")
	require.Equal(t, core.QuotaKindEntry, quota.Kind, "单条判据先于总量判据")
	require.Equal(t, core.MaxCoreEntryBytes, quota.Limit)

	// ② 总量越限：8 条恰 4096 字节的条目恰好到顶（口径 6：恰在上限 == 放行），再 1 字节即拒。
	entry := strings.Repeat("b", core.MaxCoreEntryBytes)
	for i := 0; i < core.MaxCoreSectionBytes/core.MaxCoreEntryBytes; i++ {
		require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionCore, entry), "第 %d 条恰在单条上限内", i)
	}
	err = svc.Append(ctx, core.GlobalScope, core.SectionCore, "x")
	require.ErrorAs(t, err, &quota, "到顶后 1 字节都进不去")
	require.Equal(t, core.QuotaKindSection, quota.Kind, "总量判据报的是层内求和上限")
	require.Equal(t, core.MaxCoreSectionBytes, quota.Limit)

	// 归档区**无配额**（约束 7）：同一份超限内容落归档区照常成功——超限处置只针对核心区。
	require.NoError(t, svc.Append(ctx, core.GlobalScope, core.SectionArchive, strings.Repeat("c", core.MaxCoreSectionBytes*2)),
		"归档区无配额，只追加不拒绝")
}

// R4a —— 手工编辑超配额：**完整**注入 + WARN（memory 层，可观测性双轨的日志半轨）。
// 逆改点：注入期静默截断（"只取前 N 字节"）→ 与全量期望文本的相等断言红——**末尾哨兵**
// 最先消失，断言失败信息直接指到截断点；删掉超限判定 → 日志里没有 WARN，断言红。
// 判据形态（P1-3 修正）：不比 Contains（"条目在不在"在截断实现下照样能过），而是**逐字节
// 等于完整期望文本** + 尾部哨兵——截断、丢条、多拼接都会红。
//
// 落地适配（两处，**断言逻辑一字未动**）：① §4.3 原样代码写 `svc := NewService(...)` 单
// 返回值；T020 定的签名是 `(*Service, error)`，故补 error 判。② `t.TempDir()` 是空目录，
// 写手工文件前先建 `memory/`（与 `TestMarkdownStore_LoadReturnsFileVerbatim` 同款）。
// 转绿前提 = T046（注入期超配额 WARN 的行为在该任务落地；本任务登记的是**红态先行**）。
func TestOversizedManualMemory_WarnsAndInjectsFully(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	svc, err := memory.NewService(memory.Options{Backend: store, Logger: captureLogger(t)})
	require.NoError(t, err)

	// 3000 条 24 字节条目 + 末尾哨兵 ≈ 72 KB > 32768：手工编辑不归工具配额管。
	entries := make([]string, 0, 3001)
	for i := 0; i < 3000; i++ {
		entries = append(entries, "- 很长的一条偏好")
	}
	entries = append(entries, "- 末尾哨兵：本条必须在注入结果里出现")

	// 人手写的文件 = 载体重建形态（零缩进列表项 + 条目间空行，约束 28）。
	manual := "# MEMORY.md —— 长期记忆 · 全局层\n\n## 核心记忆\n\n" +
		strings.Join(entries, "\n\n") + "\n\n## 归档记忆\n"
	require.NoError(t, os.MkdirAll(filepath.Join(root, "memory"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "memory", "MEMORY.md"), []byte(manual), 0o600))

	got, err := svc.LoadEffective(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "## 核心记忆\n\n"+strings.Join(entries, "\n\n"), got,
		"注入面 = 核心区全文（不截断、不含文件标题、不含归档区）")

	logs := captureWriter.logs(t)
	require.Contains(t, logs, "WARN")
	require.Contains(t, logs, "超") // 日志点名超配额事实；**不得**打印记忆正文
	require.NotContains(t, logs, "- 很长的一条偏好", "日志不打记忆正文")
	require.NotContains(t, logs, "末尾哨兵", "日志不打记忆正文")
}

// TestService_LoadEffective_SingleOversizedManualEntryWarns 守「手工编辑导致的**单条**
// 超配额同样记 WARN 且完整注入」（FR-021 的两条配额 / FR-025）：注入侧不得只判核心区
// 总量（32 KB）——单条 4097 字节、总量远未到顶时，合法文件仍是超配额文件，排障者必须
// 在日志里看到痕迹。同时守边界：恰 4096 字节不告警（恰等上限 == 放行）。
// 逆改点：删掉 coreSectionOf 的单条检查 → 4097 那条日志无 WARN，断言红。
// 验收点：FR-021 / FR-025；第二轮复核 F-03。
func TestService_LoadEffective_SingleOversizedManualEntryWarns(t *testing.T) {
	root := t.TempDir()
	store := memory.NewMarkdownStore(root)
	svc, err := memory.NewService(memory.Options{Backend: store, Logger: captureLogger(t)})
	require.NoError(t, err)
	file := filepath.Join(root, "memory", "MEMORY.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))

	oversized := strings.Repeat("x", core.MaxCoreEntryBytes+1)
	manual := globalMemoryTitle + "\n\n## 核心记忆\n\n- " + oversized + "\n\n## 归档记忆\n"
	require.NoError(t, os.WriteFile(file, []byte(manual), 0o600))

	got, err := svc.LoadEffective(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "## 核心记忆\n\n- "+oversized, got, "单条超配额仍完整注入（不截断）")

	logs := captureWriter.logs(t)
	require.Contains(t, logs, "WARN")
	require.Contains(t, logs, "单条")
	require.Contains(t, logs, "4096", "日志点名触发的上限")
	require.NotContains(t, logs, oversized, "日志不得打印记忆正文")

	// 边界：恰 4096 字节（总量也远低于 32 KB）→ 不告警。
	exact := strings.Repeat("y", core.MaxCoreEntryBytes)
	manual = globalMemoryTitle + "\n\n## 核心记忆\n\n- " + exact + "\n\n## 归档记忆\n"
	require.NoError(t, os.WriteFile(file, []byte(manual), 0o600))
	captureWriter.reset()

	got, err = svc.LoadEffective(ctx, "")
	require.NoError(t, err)
	require.Equal(t, "## 核心记忆\n\n- "+exact, got)
	require.NotContains(t, captureWriter.logs(t), "WARN", "恰等上限 == 放行，不告警")
}

// TestService_ConcurrentAppendSameScopeSerialized 守「同作用域写入串行」的行为面
// （约束 13 / 口径 19）：16 笔并发追加同一作用域，逐笔成功、**一条不丢、也不重复**。
// 判据是**多重集比较**（`ElementsMatch`）而不是 `Contains`——后者「条目 10」能替
// 「条目 1」过（探针实测：只落 15 条时 16 条 `Contains` 断言仍全绿）。
// 逆改点：去掉门面的 per-scope 锁 → markdown 档「读-改-写」交错、后写覆盖先写，
// 条数 / 多重集断言红。并发纪律：子 goroutine 只收集 error，断言一律回主 goroutine。
func TestService_ConcurrentAppendSameScopeSerialized(t *testing.T) {
	const n = 16

	for _, c := range carriersForTest(t) {
		t.Run(c.name, func(t *testing.T) {
			svc, err := memory.NewService(memory.Options{Backend: c.store, Logger: testLogger(t)})
			require.NoError(t, err)

			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					errs[i] = svc.Append(ctx, core.GlobalScope, core.SectionArchive, fmt.Sprintf("条目 %d", i))
				}(i)
			}
			wg.Wait()
			for i, err := range errs {
				require.NoError(t, err, "第 %d 笔并发追加", i)
			}

			loaded, err := svc.Load(ctx, core.GlobalScope)
			require.NoError(t, err)
			want := make([]string, 0, n)
			for i := 0; i < n; i++ {
				want = append(want, fmt.Sprintf("条目 %d", i))
			}
			got := archiveEntriesForTest(t, loaded)
			require.Len(t, got, n, "并发追加不得丢条目（也不得多出条目）")
			require.ElementsMatch(t, want, got, "16 条逐字齐全且两两不同（多重集比较：重复即红）")
		})
	}
}

// blockingStore 是**闸门式**载体替身（R3 裁决 P2-b 的判别力来源）：`Append` 一进入就把
// 本次 `core.Scope` 发到 `entered`，然后**阻塞**到 `release` 被关闭才返回；`Load` /
// `RecallByKeyword` 恒空。它把「谁在什么时候进入载体」变成可观测事件——锁不存在 / 锁粒度
// 错时，进入次序立刻与期望不符。
// blockingStore is a gate-style carrier double: Append announces the scope on entered,
// then blocks until release is closed.
type blockingStore struct {
	entered chan core.Scope // 有缓冲：主 goroutine 先断言"没进来"也不会把子 goroutine 卡死
	release chan struct{}   // 关闭 = 放行全部被闸住的 Append
}

func (s *blockingStore) Append(_ context.Context, scope core.Scope, _ core.Section, _ string) error {
	s.entered <- scope
	<-s.release
	return nil
}

func (s *blockingStore) Load(context.Context, core.Scope) (string, error) { return "", nil }

func (s *blockingStore) RecallByKeyword(context.Context, core.Scope, string) ([]string, error) {
	return nil, nil
}

// 编译期断言：夹具必须与门面同接口，否则本用例会退化成"编译不过"而不是"判据成立"。
var _ core.LongTermMemoryStore = (*blockingStore)(nil)

// waitEntered 等一个载体进入信号；超时 = 用例失败（**不得**无限期裸等）。
// 2s 是给 P2-b 的逆改方向留的判别窗口：换成一把大锁时 bob 永远等不到进入信号。
func waitEntered(t *testing.T, entered <-chan core.Scope, msg string) core.Scope {
	t.Helper()
	select {
	case scope := <-entered:
		return scope
	case <-time.After(2 * time.Second):
		require.FailNow(t, msg, "等待载体进入信号超时（2s）——锁粒度错时这里就是红点")
		return core.Scope{}
	}
}

// TestService_AppendLocksAreKeyedByScope 守**锁按完整 Scope 分档**（R3 裁决 P2-b / 约束 13）：
// ① 同 Scope 的第二笔在首笔放行前**不得**进入载体（同作用域串行）；
// ② alice 阻塞期间 bob **必须**能进入载体（不同 Agent 不互相阻塞——按「层」只建两把或
// 换一把全仓大锁都会让 bob 等到超时）。
// 逆改点：去掉门面的 per-scope 锁 → ① 立刻红（第二笔直接进了载体）；把分档 map 换成
// 一把大锁 / 按层的两把 → ② 超时红（P2-b 的判别力就在这一档）。
// 并发纪律：require 一律回主 goroutine；所有等待都带超时。
func TestService_AppendLocksAreKeyedByScope(t *testing.T) {
	alice := core.Scope{Agent: "alice"}
	bob := core.Scope{Agent: "bob"}

	store := &blockingStore{entered: make(chan core.Scope, 8), release: make(chan struct{})}
	// 用例中途失败时也放行全部被闸住的 Append——失败的用例不该把 goroutine 永久挂住。
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(store.release) }) }
	t.Cleanup(releaseAll)

	svc, err := memory.NewService(memory.Options{Backend: store, Logger: testLogger(t)})
	require.NoError(t, err)

	// 首笔 alice：进入载体后即被闸住（持锁不放）。
	aliceFirst := make(chan error, 1)
	go func() { aliceFirst <- svc.Append(ctx, alice, core.SectionCore, "alice 第一笔") }()
	require.Equal(t, alice, waitEntered(t, store.entered, "alice 第一笔必须进入载体"))

	// ① 同 Scope 第二笔：首笔仍在闸内 → 第二笔不得进入载体。
	aliceSecond := make(chan error, 1)
	go func() { aliceSecond <- svc.Append(ctx, alice, core.SectionCore, "alice 第二笔") }()
	select {
	case scope := <-store.entered:
		t.Fatalf("同 Scope 的第二笔在首笔放行前进入了载体（scope = %+v）——per-scope 锁未生效", scope)
	case <-time.After(200 * time.Millisecond):
		// 预期：窗口内没有任何进入信号（正确实现下这不是竞态，锁让它永远不成立）。
	}

	// ② alice 仍被闸住 → bob 必须能进入（不同 Agent 不共用一把锁）。
	bobAppend := make(chan error, 1)
	go func() { bobAppend <- svc.Append(ctx, bob, core.SectionCore, "bob 的条目") }()
	require.Equal(t, bob, waitEntered(t, store.entered, "alice 阻塞期间 bob 必须能进入载体（不同 Agent 不互相阻塞）"))

	// 放行：三笔都必须以 nil 结束（同 Scope 的第二笔在锁放行后补进载体）。
	releaseAll()
	require.NoError(t, <-aliceFirst, "alice 第一笔")
	require.NoError(t, <-bobAppend, "bob 的追加")
	require.NoError(t, <-aliceSecond, "alice 第二笔（锁放行后补进载体）")
}
