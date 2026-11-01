//go:build e2e

package e2e

// 本文件是 V-5（语义记忆写入）的**端到端**判据（`TestUS16MemoryIndex`）：真实二进制 +
// 回环假模型 + 回环假 embedding + 隔离 PostgreSQL schema，四条判据串成一条用户链路。
//
//	[1] save_memory 成功 → 向量**同步**落库，嵌入调用携带刚写入的正文；
//	[2] 同一 Agent 的语义召回命中（写后补索引在**读侧**可见），带名次与来源；
//	[3] 有生效归档记忆却没有向量的层 → 召回显式报「索引未就绪」，**不得**落成
//	    `(无匹配条目)`，也不得与「未装配」共用文案；
//	[4] 重启 → 启动期回填补齐缺口 → 同一查询命中。
//
// 为什么用**两个 Agent**：`RecallEntries` 没有相似度阈值，同一层只要有一条向量就必然
// 有命中；缺口场景必须落在**零向量的独立 Agent 层**才走得到缺口判定。
//
// 为什么缺口行在 serve **就绪之后**才种：启动期回填在装配阶段同步执行（`wire.go` 的
// `indexer.Backfill`），先种会被当场索引，[3] 的缺口与 [4] 的回填两个判据同时失效。
//
// This file is the end-to-end verdict for V-5: a synchronous vector write on save, a
// visible recall hit, an explicit index-gap error instead of a silent "no match", and
// the startup backfill closing that gap after a restart.

import (
	"strings"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/testutil"
)

// 本用例的 Agent 与正文：两个 Agent 分别承担「保存 → 命中」与「缺口 → 回填」两段。
// The two agents and the two bodies: one carries save-then-hit, the other the gap.
const (
	us16MemoryAgent = "memory-agent"
	us16GapAgent    = "gap-agent"

	// us16SavedContent 经工具写入；us16SeededContent 直接入库（代表库里已有的存量状态）。
	us16SavedContent  = "成本评估每周五进行"
	us16SeededContent = "巡检每周三执行"
)

// us16LastChatRequest 返回假模型收到的最后一次请求（判据读它的消息序列）。
// us16LastChatRequest returns the last request the fake model received.
func us16LastChatRequest(t *testing.T, chat *testutil.FakeOpenAIServer) map[string]any {
	t.Helper()
	requests := chat.Requests()
	if len(requests) == 0 {
		t.Fatalf("假模型一次请求都没收到")
	}
	return requests[len(requests)-1]
}

// us16SawEmbeddingInput 判假 embedding 是否收到过包含该片段的入参：向量来自一次真实
// 的嵌入调用，而不是库里凭空多出来的一行。
// us16SawEmbeddingInput reports whether any embedding call carried the fragment.
func us16SawEmbeddingInput(embed *us16EmbeddingServer, fragment string) bool {
	embed.mu.Lock()
	defer embed.mu.Unlock()
	for _, call := range embed.calls {
		for _, input := range call.Input {
			if strings.Contains(input, fragment) {
				return true
			}
		}
	}
	return false
}

// TestUS16MemoryIndex 是 V-5 的四条端到端判据（详见文件头部）。
// TestUS16MemoryIndex is the four-part end-to-end verdict for V-5.
func TestUS16MemoryIndex(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)

	addr := freeLoopbackAddr(t)
	// 八步剧本 = 四次运行、每次一次工具调用 + 一次终答；脚本用尽即判红（错位立刻暴露）。
	chat := newFakeModel(t,
		// 运行 1：memory-agent 写入归档记忆（缺省分区）。
		toolCallResponse("call-us16-save", "save_memory",
			mustJSON(map[string]string{"content": us16SavedContent})),
		textResponse("已记住成本评估的节奏"),
		// 运行 2：同一 Agent 语义召回，必须命中运行 1 写入的那条。
		toolCallResponse("call-us16-recall", "recall_memory",
			mustJSON(map[string]string{"query": "成本评估"})),
		textResponse("成本评估每周五进行"),
		// 运行 3：零向量层（gap-agent）召回，必须显式报索引缺口。
		toolCallResponse("call-us16-gap", "recall_memory",
			mustJSON(map[string]string{"query": "巡检"})),
		textResponse("索引尚未就绪"),
		// 运行 4：重启回填补齐之后，同一查询必须命中。
		toolCallResponse("call-us16-after", "recall_memory",
			mustJSON(map[string]string{"query": "巡检"})),
		textResponse("巡检每周三执行"),
	)
	embed := newUS16EmbeddingServer(t)

	providers := "providers:\n" +
		v3ProviderBlock(fixtureProvider, chat.URL(), fixtureModel) +
		v3ProviderBlock(us16EmbedProvider, embed.URL(), us16EmbedModel)
	h.writeConfig(t, v3ServeConfig(addr, providers, us16EmbeddingSection()+us16CallersSection()))
	writeWebAgent(t, h, us16MemoryAgent, []string{"save_memory", "recall_memory"})
	writeWebAgent(t, h, us16GapAgent, []string{"recall_memory"})

	p := startGrouped(t, h, fixtureEnv(), "serve")
	defer p.stop(t)
	waitHealthHTTP(t, p, addr, 30*time.Second)

	db := h.openDB(t)

	// [1] 保存 → 写后补索引同步完成：条目落库、向量落库，嵌入确实送出了刚写入的正文。
	// Save indexes the new entry synchronously: row, vector, and a real embedding call.
	saved := invokeAgent(t, addr, us16MemoryAgent, "请记住："+us16SavedContent)
	if saved.Status != "completed" {
		t.Fatalf("保存运行的 status = %q，期望 completed（reason=%q）", saved.Status, saved.Reason)
	}
	if n := scalarInt(t, db, `
		SELECT COUNT(*) FROM memory_entries
		 WHERE agent_name = $1 AND section = 'archive' AND state = 'active' AND content = $2`,
		us16MemoryAgent, us16SavedContent); n != 1 {
		t.Fatalf("保存后归档条目数 = %d，期望 1（save_memory 必须真的落库）", n)
	}
	if n := scalarInt(t, db, `
		SELECT COUNT(*) FROM memory_embeddings m
		  JOIN memory_entries e ON e.id = m.entry_id
		 WHERE e.agent_name = $1 AND e.content = $2`, us16MemoryAgent, us16SavedContent); n != 1 {
		t.Fatalf("保存后的记忆向量数 = %d，期望 1（写后补索引必须同步完成，V-5）", n)
	}
	if !us16SawEmbeddingInput(embed, us16SavedContent) {
		t.Fatalf("假 embedding 没有收到保存正文 %q：向量不是嵌入调用的产物", us16SavedContent)
	}
	toolResultContaining(t, us16LastChatRequest(t, chat), "已记住", "保存确认文案")

	// [2] 语义召回命中：写后补索引在**读侧**可见，命中带名次与来源，且不是占位文案。
	// The recall hit proves the write-side index is visible on the read side.
	recalled := invokeAgent(t, addr, us16MemoryAgent, "成本评估什么时候做？")
	if recalled.Status != "completed" {
		t.Fatalf("召回的 status = %q，期望 completed（reason=%q）", recalled.Status, recalled.Reason)
	}
	hit := toolResultContaining(t, us16LastChatRequest(t, chat), us16SavedContent, "语义召回命中")
	if !strings.Contains(hit, "1. [save_memory]") {
		t.Fatalf("召回结果缺少名次与来源前缀 `1. [save_memory]`：%q", hit)
	}
	if strings.Contains(hit, "(无匹配条目)") {
		t.Fatalf("召回结果同时命中与无匹配占位：%q", hit)
	}

	// [3] 缺口：种一条**没有向量**的生效归档行到零向量层，召回必须显式报缺口。
	// 种在 serve 就绪之后：启动期回填已经跑完，这一行不会被顺手索引。
	// The gap row is seeded after readiness so the startup backfill cannot have seen it.
	seedMemoryRows(t, h, seedMemoryRow{
		Scope: "agent", Agent: us16GapAgent, Section: "archive", Content: us16SeededContent,
	})
	if n := scalarInt(t, db, `
		SELECT COUNT(*) FROM memory_embeddings m
		  JOIN memory_entries e ON e.id = m.entry_id
		 WHERE e.agent_name = $1 AND e.content = $2`, us16GapAgent, us16SeededContent); n != 0 {
		t.Fatalf("种子行已有 %d 条向量，期望 0（缺口场景必须从零向量起步）", n)
	}

	gapRun := invokeAgent(t, addr, us16GapAgent, "巡检什么时候做？")
	if gapRun.Status != "completed" {
		t.Fatalf("缺口运行的 status = %q，期望 completed（工具失败不改变运行结局，reason=%q）",
			gapRun.Status, gapRun.Reason)
	}
	gap := toolResultContaining(t, us16LastChatRequest(t, chat), "记忆向量索引未就绪", "缺口必须显式提示")
	if strings.Contains(gap, "(无匹配条目)") {
		t.Fatalf("索引缺口被伪装成无匹配（V-5 明令禁止）：%q", gap)
	}
	if strings.Contains(gap, "语义召回未装配") {
		t.Fatalf("索引缺口与「未装配」共用文案（修复动作不同，不得混）：%q", gap)
	}
	if !strings.Contains(gap, "mode=keyword") {
		t.Fatalf("缺口文案必须给出可执行的下一步（改用 mode=keyword）：%q", gap)
	}
	if n := scalarInt(t, db, `
		SELECT COUNT(*) FROM memory_embeddings m
		  JOIN memory_entries e ON e.id = m.entry_id
		 WHERE e.agent_name = $1 AND e.content = $2`, us16GapAgent, us16SeededContent); n != 0 {
		t.Fatalf("缺口路召回后向量数 = %d，期望 0（读路径不得顺手补索引）", n)
	}
	// 审计面：这次召回是**真的执行过**的失败调用，不是工具层短路。
	// The audit rows prove the gap recall was a real failed invocation, not a shortcut.
	gapRows := queryRows(t, db, `
		SELECT status, error_kind FROM tool_invocations
		 WHERE tool_name = 'recall_memory' ORDER BY id`)
	if len(gapRows) != 2 {
		t.Fatalf("recall_memory 审计行数 = %d，期望 2（一次命中 + 一次缺口）", len(gapRows))
	}
	if got := stringValue(t, gapRows[len(gapRows)-1], "status"); got != "failed" {
		t.Fatalf("缺口召回审计 status = %q，期望 failed", got)
	}
	if got := stringValue(t, gapRows[len(gapRows)-1], "error_kind"); got != "invalid_input" {
		t.Fatalf("缺口召回审计 error_kind = %q，期望 invalid_input", got)
	}

	// [4] 重启 → 启动期回填把缺口补上 → 同一查询命中同一条正文。
	// Restart; the startup backfill fills the gap and the same query now hits.
	p.stop(t)
	addr2 := freeLoopbackAddr(t)
	h.writeConfig(t, v3ServeConfig(addr2, providers, us16EmbeddingSection()+us16CallersSection()))
	p2 := startGrouped(t, h, fixtureEnv(), "serve")
	defer p2.stop(t)
	waitHealthHTTP(t, p2, addr2, 30*time.Second)

	if n := scalarInt(t, db, `
		SELECT COUNT(*) FROM memory_embeddings m
		  JOIN memory_entries e ON e.id = m.entry_id
		 WHERE e.agent_name = $1 AND e.content = $2`, us16GapAgent, us16SeededContent); n != 1 {
		t.Fatalf("重启后种子行的向量数 = %d，期望 1（启动期回填必须补齐缺口）", n)
	}
	if !us16SawEmbeddingInput(embed, us16SeededContent) {
		t.Fatalf("假 embedding 没有收到种子正文 %q：回填的向量不是嵌入调用的产物", us16SeededContent)
	}

	after := invokeAgent(t, addr2, us16GapAgent, "巡检什么时候做？")
	if after.Status != "completed" {
		t.Fatalf("回填后召回的 status = %q，期望 completed（reason=%q）", after.Status, after.Reason)
	}
	backfilled := toolResultContaining(t, us16LastChatRequest(t, chat), us16SeededContent, "回填后召回命中")
	if strings.Contains(backfilled, "(无匹配条目)") {
		t.Fatalf("回填后仍落无匹配占位：%q", backfilled)
	}
}
