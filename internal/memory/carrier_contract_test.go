package memory_test

// 本文件是**两档载体契约面**（颗粒度文档 §3.2，共 7 条）：markdown 与 sqlite 跑
// **同一张场景表**，同一组断言在两档上都要成立——载体可换、语义不变（约束 17）。
// T018 落夹具位，T026 落其中 6 条（场景表 / 配额边界 / 条目边界往返 / 检索语义 /
// 结构中立 / 分层隔离），US4 / T043 落第 7 条（R5 并发写不丢条目）。
// This file is the carrier contract surface: fixtures landed with T018, six cases with
// T026, and the concurrency case (R5) with T043.

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/storage"
)

// carrierCase 是载体矩阵的一行：名字进子测试名，store 是两档共用的接口面。
// carrierCase is one row of the carrier matrix: a name for the subtest and the store.
type carrierCase struct {
	name  string
	store core.LongTermMemoryStore
}

// carriersForTest 返回两档载体——两档跑同一断言的**唯一入口**（R3 / R5 都经它取）。
// 本文件是 `package memory_test`（外部测试包），拿不到 `internal/storage` 的内测夹具，
// 故经**导出面**自建真库：`OpenWritable` 建库并迁移到当前 schema 版本，`t.Cleanup`
// 按后进先出关闭连接（早于 `t.TempDir()` 的目录清理注册，故先执行）。
// carriersForTest returns both carriers through exported APIs only; the sqlite side gets
// a real migrated database in a temp dir.
func carriersForTest(t *testing.T) []carrierCase {
	t.Helper()

	db, err := storage.OpenWritable(ctx, filepath.Join(t.TempDir(), "basalt.db"), storage.SupportedSchemaVersion)
	require.NoError(t, err, "两档载体须自建真库（外部测试包用不了内测夹具）")
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	// 时钟固定：本条矩阵的判据面是语义，不是时间；`created_at` 的可判面在 storage 侧
	// 单测（T019）里用可控的递增时钟单独钉。
	now := func() time.Time { return time.Unix(0, 0) }

	return []carrierCase{
		{name: "markdown", store: memory.NewMarkdownStore(t.TempDir())},
		{name: "sqlite", store: storage.NewMemoryStore(db, now)},
	}
}

// archiveEntriesForTest 按约束 28 的**文档化形态**从载体文本里取归档区条目：结构行只有
// 零缩进的文件标题与分区标题；条目 = 零缩进 `"- "` 起行（本用例的条目都是单行，缩进续行
// 归入上一条的正文、不单独成条）。这三类之外的任何行 → 直接失败（形态漂移必须可见）。
// The documented shape only; anything else fails rather than being swallowed silently.
//
// 为什么不用实现里的解析器：`package memory_test` 拿不到未导出的 `parseSections`，
// 且**不得**引用实现常量——形态漂移时它先红，而不是跟着实现一起漂（约束 28 / R5）。
func archiveEntriesForTest(t *testing.T, text string) []string {
	t.Helper()
	var entries []string
	inArchive := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "## "): // 分区标题（含其后的正文归属切换）
			inArchive = line == "## 归档记忆"
		case strings.HasPrefix(line, "# "): // 文件标题
			inArchive = false
		case line == "" || strings.HasPrefix(line, "  "): // 条目间空行 / 缩进续行
		case inArchive && strings.HasPrefix(line, "- "):
			entries = append(entries, strings.TrimPrefix(line, "- "))
		default:
			require.Failf(t, "载体形态漂移", "行 %q 既不是结构行也不是条目行", line)
		}
	}
	return entries
}

// 两分区标题的**独立抄写**（口径 22 冻结的字面量）：与 `archiveEntriesForTest` 里那份同理
// ——引用实现常量会让两边一起漂，钉不住形态。
const (
	coreHeading    = "## 核心记忆"
	archiveHeading = "## 归档记忆"
)

// sectionBodiesForTest 按约束 28 的**文档化形态**取出 title 分区下的**条目正文**：条目 =
// 零缩进 `"- "` 首行 + 它后面的空行与两空格缩进续行（多行正文原样粘回）；结构行只认零缩进
// 的文件标题与分区标题；目标分区之外的条目**跳过**（与 `archiveEntriesForTest` 的区别：
// 后者只服务单行条目的 R5，条目跨行或另一分区有内容时会长红）。
// The documented-shape extractor returning whole entry bodies, for both sections.
func sectionBodiesForTest(t *testing.T, text, title string) []string {
	t.Helper()
	var (
		bodies []string
		lines  []string
		in     bool
	)
	flush := func() {
		if len(lines) == 0 {
			return
		}
		body := strings.TrimSpace(strings.Join(lines, "\n"))
		lines = nil
		if body != "" {
			bodies = append(bodies, body)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "## "): // 分区标题：切换归属（含目标分区之外的）
			flush()
			in = line == title
		case strings.HasPrefix(line, "# "): // 文件标题：其后的正文归属回到"未分区"
			flush()
			in = false
		case line == "" || strings.HasPrefix(line, "  "): // 空行 / 缩进续行（空行不切块）
			if in {
				lines = append(lines, strings.TrimPrefix(line, "  "))
			}
		case strings.HasPrefix(line, "- "): // 条目首行
			flush()
			if in {
				lines = append(lines, strings.TrimPrefix(line, "- "))
			}
		default:
			require.Failf(t, "载体形态漂移", "行 %q 既不是结构行也不是条目行", line)
		}
	}
	flush()
	return bodies
}

// totalBytes 求和条目正文的字节数——配额判据面（约束 29：只算正文，不含载体添加物）。
func totalBytes(bodies []string) int {
	total := 0
	for _, body := range bodies {
		total += len(body)
	}
	return total
}

// TestCarriers_ScenarioTable 是两档的**主表**：同一张场景表在两档上跑（约束 17——载体可换、
// 语义不变）。每条场景自带新载体（场景之间不共享状态），断言一律经文档化形态的提取器读回。
// The main table: one scenario list, both carriers, one set of assertions.
func TestCarriers_ScenarioTable(t *testing.T) {
	scenarios := []struct {
		name string
		run  func(t *testing.T, store core.LongTermMemoryStore)
	}{
		{
			name: "空层：加载与召回都是空结果",
			run: func(t *testing.T, store core.LongTermMemoryStore) {
				text, err := store.Load(ctx, core.GlobalScope)
				require.NoError(t, err)
				require.Empty(t, text, "合法空层归一成空文本（两档同形）")

				hits, err := store.RecallByKeyword(ctx, core.GlobalScope, "任何词")
				require.NoError(t, err)
				require.Empty(t, hits, "未命中 = 空结果 + nil error（口径 10）")
			},
		},
		{
			name: "核心区写入：加载可见、不参与检索",
			run: func(t *testing.T, store core.LongTermMemoryStore) {
				require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionCore, "偏好：回答简洁"))

				text, err := store.Load(ctx, core.GlobalScope)
				require.NoError(t, err)
				require.Equal(t, []string{"偏好：回答简洁"}, sectionBodiesForTest(t, text, coreHeading))
				require.Empty(t, sectionBodiesForTest(t, text, archiveHeading), "归档区不得被顺手写进东西")

				hits, err := store.RecallByKeyword(ctx, core.GlobalScope, "回答简洁")
				require.NoError(t, err)
				require.Empty(t, hits, "核心区每轮已完整注入，不参与检索（约束 9）")
			},
		},
		{
			name: "归档区写入：核心区零条、召回按写入顺序",
			run: func(t *testing.T, store core.LongTermMemoryStore) {
				require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "第一条：成本评估"))
				require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, "第二条：成本复评"))

				text, err := store.Load(ctx, core.GlobalScope)
				require.NoError(t, err)
				require.Empty(t, sectionBodiesForTest(t, text, coreHeading))
				require.Equal(t, []string{"第一条：成本评估", "第二条：成本复评"},
					sectionBodiesForTest(t, text, archiveHeading))

				hits, err := store.RecallByKeyword(ctx, core.GlobalScope, "成本")
				require.NoError(t, err)
				require.Equal(t, []string{"第一条：成本评估", "第二条：成本复评"}, hits,
					"两条都命中且**按写入顺序**返回（约束 9 / 口径 12）")
			},
		},
		{
			name: "超限写被拒且不留痕：单条上限只压核心区",
			run: func(t *testing.T, store core.LongTermMemoryStore) {
				oversized := strings.Repeat("a", 4096+1) // 恰超 1 字节（口径 8 的单条上限）

				err := store.Append(ctx, core.GlobalScope, core.SectionCore, oversized)
				var quota *core.MemoryQuotaError
				require.ErrorAs(t, err, &quota, "超单条上限必须拒")
				require.Equal(t, core.QuotaKindEntry, quota.Kind, "档位 = 单条（不是总量）")
				require.Equal(t, 4096, quota.Limit)

				require.NoError(t, store.Append(ctx, core.GlobalScope, core.SectionArchive, oversized),
					"归档区无单条上限（约束 7）")

				text, err := store.Load(ctx, core.GlobalScope)
				require.NoError(t, err)
				require.Empty(t, sectionBodiesForTest(t, text, coreHeading), "被拒的核心区写不得留痕")
				require.Len(t, sectionBodiesForTest(t, text, archiveHeading), 1)
			},
		},
	}

	for _, sc := range scenarios {
		for _, c := range carriersForTest(t) {
			t.Run(c.name+"/"+sc.name, func(t *testing.T) {
				sc.run(t, c.store)
			})
		}
	}
}

// TestCarriers_QuotaBoundaryExactLimit 判两档在配额边界上的**读数与判定一致**（约束 29 末条）：
// ① 8 条恰 4096 字节的条目 → 两档都恰好到 32768 并**放行**（恰在上限 == 放行），第 9 笔拒；
// ② P1-b 首尾空白档：8 条首尾各带一个空格、原长 4096 字节（规范化后 4094）的条目 → 两档
// **都**合计 32752 且**都**放行紧接的 `"x"`——不规范化时两档会分叉成 32752 / 32768
// （"换档只改一行"在边界上失效）。
// 逆改点：任一档把首尾空白计入求和（或规范化挪到检查之后）→ ② 的合计与放行档红；
// 把载体添加物（标题 / `"- "` 前缀 / 缩进）算进求和 → ① 的 8 条填不满 32768、第 9 笔放行。
func TestCarriers_QuotaBoundaryExactLimit(t *testing.T) {
	// entryOf 造一条**恰 n 字节**的正文：前缀保证条目可辨，尾部补 "a" 到精确长度。
	entryOf := func(t *testing.T, prefix string, n int) string {
		t.Helper()
		pad := n - len(prefix)
		require.Positive(t, pad, "前缀自身不得长过目标长度")
		return prefix + strings.Repeat("a", pad)
	}

	t.Run("八条恰 4096 到顶放行、第 9 笔拒", func(t *testing.T) {
		for _, c := range carriersForTest(t) {
			t.Run(c.name, func(t *testing.T) {
				for i := 0; i < 8; i++ {
					entry := entryOf(t, fmt.Sprintf("条目%d：", i), 4096)
					require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionCore, entry),
						"第 %d 条恰在单条上限之内，必须放行", i+1)
				}

				text, err := c.store.Load(ctx, core.GlobalScope)
				require.NoError(t, err)
				bodies := sectionBodiesForTest(t, text, coreHeading)
				require.Len(t, bodies, 8, "八条一条不丢")
				require.Equal(t, 32768, totalBytes(bodies), "恰在上限：8 × 4096")

				err = c.store.Append(ctx, core.GlobalScope, core.SectionCore, "x")
				var quota *core.MemoryQuotaError
				require.ErrorAs(t, err, &quota, "第 9 笔（32769）必须拒")
				require.Equal(t, core.QuotaKindSection, quota.Kind, "档位 = 层内总量")
				require.Equal(t, 32768, quota.Limit)
			})
		}
	})

	t.Run("首尾空白不计入：两档都 32752 且都放行后续零头", func(t *testing.T) {
		for _, c := range carriersForTest(t) {
			t.Run(c.name, func(t *testing.T) {
				for i := 0; i < 8; i++ {
					body := entryOf(t, fmt.Sprintf("白%d：", i), 4094)
					padded := " " + body + " " // 原长 4096、规范化后 4094
					require.Len(t, padded, 4096)
					require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionCore, padded))
				}

				text, err := c.store.Load(ctx, core.GlobalScope)
				require.NoError(t, err)
				bodies := sectionBodiesForTest(t, text, coreHeading)
				require.Len(t, bodies, 8)
				require.Equal(t, 32752, totalBytes(bodies),
					"首尾空白不计入：8 × 4094——**读回**的正文长度才是判据，只判「写成功」的话，"+
						"把原长（4096）当判据、却存了未规范化正文的实现照样绿")

				require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionCore, "x"),
					"32753 ≤ 32768——两档都放行（不规范化时 sqlite 侧会按 32769 拒）")
			})
		}
	})
}

// TestCarriers_EntryBoundariesSurviveRoundTrip 判**同一张边界场景表**在两档的一条进一条出
// （约束 28）：条目正文里的空行 / 代码围栏 / 分区标题 / `"- "` 开头行都只当字符看——归档
// 正文里的标题不得变成核心区结构，重建后**逐字节等于原输入**（规范化后）。
// 逆改点：把解析器改成"按行收条目"（不粘缩进续行）→ 多行条目被拆成两条，条数与逐字相等
// 断言红；把 `## ` 前缀的行一律当结构行 → 正文里的标题被吞掉、条目少一条。
func TestCarriers_EntryBoundariesSurviveRoundTrip(t *testing.T) {
	boundaries := []struct {
		name    string
		content string
	}{
		{name: "含空行", content: "首段\n\n末段"},
		{name: "代码围栏", content: "示例：\n```go\nfmt.Println(\"hi\")\n```"},
		{name: "正文含核心区标题", content: "首行\n## 核心记忆\n末行"},
		{name: "首行是归档区标题", content: "## 归档记忆\n第二行"},
		{name: "首行是列表项", content: "- 已经带列表前缀"},
	}

	for _, b := range boundaries {
		for _, c := range carriersForTest(t) {
			t.Run(c.name+"/"+b.name, func(t *testing.T) {
				require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionArchive, b.content))

				text, err := c.store.Load(ctx, core.GlobalScope)
				require.NoError(t, err)
				require.Equal(t, []string{strings.TrimSpace(b.content)},
					sectionBodiesForTest(t, text, archiveHeading), "一条进一条出（多行正文不得拆条）")
				require.Empty(t, sectionBodiesForTest(t, text, coreHeading),
					"归档正文里的标题不变成核心区结构")

				// 再写一条哨兵：上一条若被拆开或吞掉，这里的整表比较立刻红。
				require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionArchive, "哨兵"))
				text, err = c.store.Load(ctx, core.GlobalScope)
				require.NoError(t, err)
				require.Equal(t, []string{strings.TrimSpace(b.content), "哨兵"},
					sectionBodiesForTest(t, text, archiveHeading))
			})
		}
	}
}

// TestCarriers_RecallPhraseHitsOnlyWhenContiguous 判检索语义的两态（注 6 / 口径 12）：
// ① 连续短语命中——条目原文里含「SQLite 方案 评估」这一**连续子串** → 命中；
// ② 分散词不自动组合——查询的词分散在不同条目上 → 不命中。**不得**钉「多词短语必不命中」
// 的旧表述（2026-10-17 已废止并同步上游四处）。
// 逆改点：把匹配换成 `LIKE '%a%b%'` 式分词匹配 → ② 命中，断言红；把 `instr` 换成大小写
// 不敏感匹配 → 大小写档红。
func TestCarriers_RecallPhraseHitsOnlyWhenContiguous(t *testing.T) {
	const phrase = "SQLite 方案 评估"

	for _, c := range carriersForTest(t) {
		t.Run(c.name, func(t *testing.T) {
			require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionArchive, "结论："+phrase+"已完成"))
			require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionArchive, "SQLite 是本项目的存储选型"))
			require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionArchive, "评估完成，结论另见附录"))
			require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionCore, "核心区也提到 "+phrase))

			hits, err := c.store.RecallByKeyword(ctx, core.GlobalScope, phrase)
			require.NoError(t, err)
			require.Equal(t, []string{"结论：" + phrase + "已完成"}, hits,
				"连续短语命中且**只**返回归档区那一条（核心区不参与检索）")

			hits, err = c.store.RecallByKeyword(ctx, core.GlobalScope, "SQLite 评估")
			require.NoError(t, err)
			require.Empty(t, hits, "分散词不自动组合命中（注 6）")

			hits, err = c.store.RecallByKeyword(ctx, core.GlobalScope, "sqlite")
			require.NoError(t, err)
			require.Empty(t, hits, "区分大小写（口径 12）")
		})
	}
}

// TestCarriers_LoadShapeIsCarrierNeutral 判口径 12 的"同一解析器切出同一分区结构"：同一串写入
// 在两档 `Load` 出来的文本上，结构行齐备且次序固定、两分区切出的条目正文**逐字相同**。
// 文本本身**不承诺逐字节相同**（两档各有一份渲染器），故断言只落在结构与条目正文上。
// 逆改点：某档渲染器少写一个分区标题 / 调换分区次序 → 结构断言或提取器直接失败。
func TestCarriers_LoadShapeIsCarrierNeutral(t *testing.T) {
	type shape struct {
		core    []string
		archive []string
	}
	shapes := map[string]shape{}

	for _, c := range carriersForTest(t) {
		require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionCore, "偏好：回答简洁"))
		require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionCore, "语言：中文"))
		require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionArchive, "结论：SQLite 方案"))
		require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionArchive, "多行：\n\n第二段"))

		text, err := c.store.Load(ctx, core.GlobalScope)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(text, "# "), "第 1 行恒为文件标题（结构行只认零缩进）")

		var headings []string
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "## ") {
				headings = append(headings, line)
			}
		}
		require.Equal(t, []string{coreHeading, archiveHeading}, headings,
			"两分区标题齐备且次序固定（%s）", c.name)

		shapes[c.name] = shape{
			core:    sectionBodiesForTest(t, text, coreHeading),
			archive: sectionBodiesForTest(t, text, archiveHeading),
		}
	}

	require.Len(t, shapes, 2, "两档都在矩阵里（缺一档 = 判据面漏了一半）")
	require.Equal(t, []string{"偏好：回答简洁", "语言：中文"}, shapes["markdown"].core)
	require.Equal(t, shapes["markdown"], shapes["sqlite"],
		"两档切出的两分区条目正文逐字相同（结构面同形，文本不承诺逐字节相同）")
}

// TestCarriers_ScopeIsolationAgentVsGlobal 判分层的**存储面隔离**：Agent 层与全局层各存各的
// ——核心区互不可见、同一关键词在另一层的归档区**取不到**（约束 4 的载体面；"回退但不叠加"
// 是门面面的判据，在 `memory_test.go` 的 R3 里）。
// 逆改点：分层的两列/两路径写错（sqlite 侧恒落 global、markdown 侧拼错 Agent 层路径）→
// 某个方向的可见性或召回串门，断言红。
func TestCarriers_ScopeIsolationAgentVsGlobal(t *testing.T) {
	alice := core.Scope{Agent: "alice"}
	bob := core.Scope{Agent: "bob"}

	for _, c := range carriersForTest(t) {
		t.Run(c.name, func(t *testing.T) {
			require.NoError(t, c.store.Append(ctx, core.GlobalScope, core.SectionCore, "全局偏好：回答简洁"))
			require.NoError(t, c.store.Append(ctx, alice, core.SectionCore, "alice 偏好：用中文"))
			require.NoError(t, c.store.Append(ctx, alice, core.SectionArchive, "alice 归档：成本评估"))
			require.NoError(t, c.store.Append(ctx, bob, core.SectionArchive, "bob 归档：成本评估"))

			globalText, err := c.store.Load(ctx, core.GlobalScope)
			require.NoError(t, err)
			require.Equal(t, []string{"全局偏好：回答简洁"},
				sectionBodiesForTest(t, globalText, coreHeading), "全局层核心区不得混入 Agent 层条目")
			require.Empty(t, sectionBodiesForTest(t, globalText, archiveHeading),
				"全局层归档区不得混入 Agent 层条目")

			aliceText, err := c.store.Load(ctx, alice)
			require.NoError(t, err)
			require.Equal(t, []string{"alice 偏好：用中文"},
				sectionBodiesForTest(t, aliceText, coreHeading), "Agent 层只见自己那层")
			require.Equal(t, []string{"alice 归档：成本评估"},
				sectionBodiesForTest(t, aliceText, archiveHeading))

			hits, err := c.store.RecallByKeyword(ctx, alice, "成本评估")
			require.NoError(t, err)
			require.Equal(t, []string{"alice 归档：成本评估"}, hits, "alice 的归档区只取到自己的")

			hits, err = c.store.RecallByKeyword(ctx, bob, "成本评估")
			require.NoError(t, err)
			require.Equal(t, []string{"bob 归档：成本评估"}, hits, "bob 同样只见自己的（同名关键词不串门）")

			hits, err = c.store.RecallByKeyword(ctx, core.GlobalScope, "成本评估")
			require.NoError(t, err)
			require.Empty(t, hits, "全局层检索取不到任何 Agent 层条目")
		})
	}
}

// R5 —— 并发写同一作用域：一条不丢、不互相覆盖（memory 层，两档，-race）。
// 逆改点：去掉门面 per-scope 锁 → markdown 档"读-改-写"交错丢条目，条数断言红。
// 判据形态（P1-3 修正）：**不得**用 `Contains(loaded, "条目 1")`——"条目 10"能替它过；
// 探针实测：只落 15 条时，16 条 Contains 断言仍全绿。改为按约束 28 的形态提取条目、
// 逐条比对**多重集**（ElementsMatch）：丢条、重复、条目串被截半都会红。
//
// 落地适配（唯一一处，**断言逻辑一字未动**）：§4.3 原样代码写 `svc := NewService(...)`
// 单返回值；T020 定的签名是 `(*Service, error)`（fail-closed），故补 error 判。
func TestCarriers_ConcurrentAppendKeepsEveryEntry(t *testing.T) {
	for _, c := range carriersForTest(t) {
		t.Run(c.name, func(t *testing.T) {
			svc, err := memory.NewService(memory.Options{Backend: c.store, Logger: testLogger(t)})
			require.NoError(t, err)
			const n = 16
			errs := make([]error, n)
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					errs[i] = svc.Append(ctx, core.GlobalScope, core.SectionArchive,
						fmt.Sprintf("条目 %d", i))
				}(i)
			}
			wg.Wait()
			for i, err := range errs {
				require.NoError(t, err, "第 %d 笔并发追加", i) // 断言一律回主 goroutine
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
