//go:build e2e

package e2e

// 本文件回填 US-6（长期记忆）的十个人工项（模块卡 §2.6.6 映射表）。十个子测试名与
// `backfillCatalog` 的 `Parent/sub` 逐字对应（`demo_markdown` / `demo_sqlite` 是历史
// 目录名，保留原名是因为 `current.json` 按名字钉判据——US-16 D-18 之后运行期只有
// PostgreSQL 一个载体，名字里的档位不再对应可选的载体）。
// 场景一律驱动真实二进制 + 回环假模型，断言只读四处：进程输出、退出码、发往模型的
// 请求体与 PostgreSQL 隔离 schema。旧 `MEMORY.md` 在这些用例里只作为**一次性只读导入
// 来源**出现（D-22：运行期不回读、不改写，导入走 `db switch`）——凡是"手写文件 → 运行期
// 读到"的旧判据都已按 US-16 的载体退役改写为"造库内状态 → 运行期读到"。
// This file backfills the ten manual items of US-6. The subtest names match the catalog
// verbatim (demo_markdown / demo_sqlite are historic names pinned by current.json; since
// US-16 D-18 PostgreSQL is the only runtime carrier). Legacy MEMORY.md files appear only
// as one-shot import sources (D-22): never read or written at runtime.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymedword/basalt/internal/testutil"
)

const (
	// memoryGlobalTitle / memoryAgentTitleFormat 是两档层文件标题的逐字形态——
	// 全局层与 `templateMemoryMD` 同源，Agent 层取同构形态（US-6 口径 3）。
	// The verbatim layer titles: the global one mirrors the workspace template.
	memoryGlobalTitle     = "# MEMORY.md —— 长期记忆 · 全局层"
	memoryAgentTitleFmt   = "# MEMORY.md —— 长期记忆 · Agent 层（%s）"
	memoryCoreHeading     = "## 核心记忆"
	memoryArchiveHeading  = "## 归档记忆"
	memoryCoreEntryLimit  = 4096
	memoryCoreTotalLimit  = 32768
	quotaEntryRefusalCopy = "核心记忆单条超出上限（4096 字节），请压缩内容或改写归档区"
	quotaTotalRefusalCopy = "核心记忆超出总量上限（32768 字节），请改写归档区"
)

// TestBackfill_Memory 覆盖长期记忆的十个人工项。
// TestBackfill_Memory covers the ten manual items of long-term memory.
func TestBackfill_Memory(t *testing.T) {
	t.Run("demo_core", testMemoryDemoCore)
	t.Run("demo_markdown", testMemoryDemoMarkdown)
	t.Run("demo_sqlite", testMemoryDemoSQLite)
	t.Run("edit_timing", testMemoryEditTiming)
	t.Run("quota", testMemoryQuota)
	t.Run("concurrency", testMemoryConcurrency)
	t.Run("redaction", testMemoryRedaction)
	t.Run("archive_size", testMemoryArchiveSize)
	t.Run("scopes", testMemoryScopes)
	t.Run("corrupt", testMemoryCorrupt)
}

// testMemoryDemoCore：Demo 二（甲档）——假模型调 save_memory 写 CORE，重启后新会话
// 读注入（US-6 §6.1 第 2 项）。新会话经 `serve` 的无状态 invoke 证明：请求只带 system +
// 当前消息，响应不带会话标识。
func testMemoryDemoCore(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-core", "save_memory", `{"content":"核心偏好-演示：回答保持简短","scope":"CORE"}`),
		textResponse("已记住核心偏好"),
		textResponse("重启后的回答"),
	)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"save_memory"}), "核心记忆回填演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "请记住我的偏好")
	requireExit(t, res, 0, "save_memory 写核心区")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "已记住（作用域：Agent 层 demo；分区：核心记忆）", "确认文案")

	// 写入落点是**唯一运行载体**（D-18）：核心区那条必须逐字进库，工作区的旧载体文件
	// 不参与运行期读写（D-22）。
	// The write lands in the single runtime carrier; the legacy file is inert.
	db := h.openDB(t)
	rows := queryRows(t, db,
		`SELECT scope, agent_name, section, content FROM memory_entries WHERE state = 'active' ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("memory_entries 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	for column, want := range map[string]string{
		"scope": "agent", "agent_name": "demo", "section": "core",
		"content": "核心偏好-演示：回答保持简短",
	} {
		if got, _ := rows[0][column].(string); got != want {
			t.Fatalf("memory_entries.%s = %q，期望 %q", column, got, want)
		}
	}

	startServe(t, h, addr)
	reading := invokeAgent(t, addr, "demo", "请继续")
	if reading.Status != "completed" {
		t.Fatalf("重启后运行状态 = %q，期望 completed", reading.Status)
	}
	if reading.SessionID != "" {
		t.Fatalf("无状态调用的会话标识 = %q，期望空", reading.SessionID)
	}

	reqs = fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("假模型应收到 3 次请求（两轮 + 重启后一轮），实际 %d 次", len(reqs))
	}
	system := systemPrompt(t, reqs[2])
	requireContains(t, system, memoryCoreHeading, "重启后必须注入核心区")
	requireContains(t, system, "核心偏好-演示：回答保持简短", "重启后必须注入条目正文")
	if msgs := requestMessages(t, reqs[2]); len(msgs) != 2 {
		t.Fatalf("无状态调用应只带 system + 当前消息，实际 %d 条：%s", len(msgs), requestText(t, reqs[2]))
	}
}

// testMemoryDemoMarkdown：旧 markdown 载体退役后的两分区语义（US-6 §6.1 第 3 项；
// 名字是历史目录名——D-18 之后运行期没有 markdown 载体）。判据：两条写入分别落进
// PostgreSQL 的核心区与归档区；**预先手写的 `MEMORY.md` 不进注入面、一个字节都不被
// 改写**（D-22：旧文件只剩一次性只读导入这一个用途）；重启后只注入核心区条目，归档
// 区不进系统提示词。
// 逆改点：把运行期接回文件读写 → 库行断言或字节比较红；把旧文件读进注入面 →
// `旧文件核心-markdown` 的 absent 断言红。
// The two write partitions land in the single carrier; the hand-written legacy file is
// never read or rewritten at runtime (D-22).
func testMemoryDemoMarkdown(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	// 升级实例里常见的手写残留：运行期必须当它不存在。
	writeMemoryLayer(t, h, "", []string{"旧文件核心-markdown"}, []string{"旧文件归档-markdown"})
	legacyPath := memoryPath(h, "")
	before := fileDigest(t, legacyPath)

	fake := newFakeModel(t,
		toolCallResponse("call-md-core", "save_memory", `{"content":"核心偏好-markdown","scope":"CORE"}`),
		toolCallResponse("call-md-archive", "save_memory", `{"content":"归档偏好-markdown","scope":"ARCHIVAL"}`),
		textResponse("两条都已记住"),
		textResponse("重启后的回答"),
	)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"save_memory"}), "两分区演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "记住两条偏好")
	requireExit(t, res, 0, "两分区写入")
	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("假模型应收到 3 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "已记住（作用域：Agent 层 demo；分区：核心记忆）", "核心区确认文案")
	requireToolResultContains(t, reqs[2], "已记住（作用域：Agent 层 demo；分区：归档记忆）", "归档区确认文案")
	requireAbsent(t, systemPrompt(t, reqs[0]), "旧文件核心-markdown",
		"运行期不得回读旧载体文件（D-22）")

	db := h.openDB(t)
	rows := queryRows(t, db,
		`SELECT section, content FROM memory_entries
		  WHERE scope = 'agent' AND agent_name = 'demo' AND state = 'active' ORDER BY id`)
	if len(rows) != 2 {
		t.Fatalf("memory_entries 应有 2 行，实际 %d 行：%v", len(rows), rows)
	}
	if got := stringValue(t, rows[0], "section"); got != "core" {
		t.Fatalf("第一条分区 = %q，期望 core", got)
	}
	if got := stringValue(t, rows[0], "content"); got != "核心偏好-markdown" {
		t.Fatalf("第一条正文 = %q，期望 核心偏好-markdown", got)
	}
	if got := stringValue(t, rows[1], "section"); got != "archive" {
		t.Fatalf("第二条分区 = %q，期望 archive", got)
	}
	if got := stringValue(t, rows[1], "content"); got != "归档偏好-markdown" {
		t.Fatalf("第二条正文 = %q，期望 归档偏好-markdown", got)
	}
	if got := fileDigest(t, legacyPath); got != before {
		t.Fatalf("旧载体文件不得被运行期改写（哈希 %s → %s）", before, got)
	}

	startServe(t, h, addr)
	reading := invokeAgent(t, addr, "demo", "请继续")
	if reading.Status != "completed" {
		t.Fatalf("重启后运行状态 = %q，期望 completed", reading.Status)
	}
	reqs = fake.Requests()
	if len(reqs) != 4 {
		t.Fatalf("假模型应收到 4 次请求（三条 + 重启后一条），实际 %d 次", len(reqs))
	}
	system := systemPrompt(t, reqs[3])
	requireContains(t, system, "核心偏好-markdown", "重启后注入核心区条目")
	requireAbsent(t, system, "归档偏好-markdown", "归档区不注入系统提示词")
	requireAbsent(t, system, "旧文件核心-markdown", "旧载体文件不得进注入面（D-22）")
}

// testMemoryDemoSQLite：旧 sqlite 档位退役后的现场（US-6 §6.1 第 3 项；名字是历史
// 目录名）。判据两段：① 旧配置 `memory.backend: sqlite` **被拒绝启动**——文案点名
// 配置键与 D-18，零模型调用（D-18：存在该键即拒，不静默忽略）；② 去掉该键后同一
// 工作区照常运行：记忆只进库，工作区 `MEMORY.md` 逐字节不变、Agent 层 markdown
// 文件不产生；重启后注入照常。
// 逆改点：把 `memory.backend` 当成可忽略键 → ① 的退出码断言红；把运行期写回文件 →
// ② 的文件哈希 / 缺文件断言红。
// The retired sqlite knob is refused outright; without it the write still lands only in
// PostgreSQL and no markdown file is produced or rewritten.
func testMemoryDemoSQLite(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	globalPath := memoryPath(h, "")
	before := fileDigest(t, globalPath)

	fake := newFakeModel(t,
		toolCallResponse("call-sql-core", "save_memory", `{"content":"核心偏好-sqlite","scope":"CORE"}`),
		textResponse("已记住"),
		textResponse("重启后的回答"),
	)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr)+"memory:\n  backend: sqlite\n")
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"save_memory"}), "sqlite 档演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "记住这条偏好")
	if res.ExitCode == 0 {
		t.Fatalf("memory.backend 存在即必须拒绝启动\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	output := res.Stdout + res.Stderr
	requireContains(t, output, "memory.backend", "拒绝文案点名配置键")
	requireContains(t, output, "D-18", "拒绝文案点名裁决")
	if got := len(fake.Requests()); got != 0 {
		t.Fatalf("配置被拒不得发起模型调用，实际 %d 次", got)
	}

	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	res = h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "记住这条偏好")
	requireExit(t, res, 0, "退役配置去除后的写入")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "已记住（作用域：Agent 层 demo；分区：核心记忆）", "确认文案")

	if got := fileDigest(t, globalPath); got != before {
		t.Fatalf("运行期不得改写工作区 MEMORY.md（哈希 %s → %s）", before, got)
	}
	if _, err := os.Stat(memoryPath(h, "demo")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("运行期不得产生 Agent 层 markdown 文件（stat 错误 %v）", err)
	}
	db := h.openDB(t)
	rows := queryRows(t, db, `SELECT scope, agent_name, section, content FROM memory_entries ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("memory_entries 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	for column, want := range map[string]string{
		"scope": "agent", "agent_name": "demo", "section": "core", "content": "核心偏好-sqlite",
	} {
		if got, _ := rows[0][column].(string); got != want {
			t.Fatalf("memory_entries.%s = %q，期望 %q", column, got, want)
		}
	}

	startServe(t, h, addr)
	reading := invokeAgent(t, addr, "demo", "请继续")
	if reading.Status != "completed" {
		t.Fatalf("重启后运行状态 = %q，期望 completed", reading.Status)
	}
	reqs = fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("假模型应收到 3 次请求，实际 %d 次", len(reqs))
	}
	system := systemPrompt(t, reqs[2])
	requireContains(t, system, "核心偏好-sqlite", "重启后注入核心区条目")
}

// testMemoryEditTiming：记忆变更的生效时机（US-6 §6.1 第 4 项）。载体切换后"编辑"
// 的等价动作是**往库里写一条生效行**（D-18 之后运行期只读 PostgreSQL）。判据：运行
// 开始后写入的条目 → 本次运行后续模型调用的系统提示词不变（运行级冻结）；两次运行
// 之间的写入 → 下次运行生效。
// 逆改点：把记忆加载挪进每次模型调用（不再按运行冻结）→ 第二次调用的 absent 断言红；
// 运行之间不重读 → 第三调的 contains 断言红。
func testMemoryEditTiming(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	h.ensureStore(t)
	db := h.openDB(t)
	inserted := seedMemoryRow{Scope: "global", Section: "core", Content: "运行中新增偏好-edit-timing"}

	var (
		mu       sync.Mutex
		requests []map[string]any
	)
	server := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("假服务：读请求体失败: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("假服务：请求体不是 JSON 对象: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		requests = append(requests, req)
		n := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// 运行已开始（记忆已冻结）之后再写库：本次运行不得看见这条。
			if err := insertMemoryRowDirect(context.Background(), db, inserted, time.Now()); err != nil {
				t.Errorf("运行中写入记忆行失败: %v", err)
			}
			_, _ = io.WriteString(w, toolCallResponse("call-timing", "read_file", `{"path":"notes.txt"}`).Body)
			return
		}
		if n == 2 {
			_, _ = io.WriteString(w, textResponse("第一轮完成").Body)
			return
		}
		_, _ = io.WriteString(w, textResponse("第二轮完成").Body)
	}))
	h.writeConfig(t, configWithProvider(server.URL))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"read_file"}), "编辑时机演示")
	h.writeAgentFile(t, "demo", "notes.txt", "笔记内容\n")

	res := h.runInput(t, fixtureEnv(), "第一行\n第二行\n", "chat", "--agent", "demo")
	requireExit(t, res, 0, "编辑时机")
	requireContains(t, res.Stdout, "第一轮完成", "编辑时机")
	requireContains(t, res.Stdout, "第二轮完成", "编辑时机")

	mu.Lock()
	reqs := append([]map[string]any(nil), requests...)
	mu.Unlock()
	if len(reqs) != 3 {
		t.Fatalf("应恰好 3 次模型请求（首轮两次 + 次轮一次），实际 %d 次", len(reqs))
	}
	requireAbsent(t, systemPrompt(t, reqs[0]), "运行中新增偏好-edit-timing", "运行开始时库里还没有该条目")
	requireAbsent(t, systemPrompt(t, reqs[1]), "运行中新增偏好-edit-timing", "运行中写入不改变本次运行（运行级冻结）")
	requireContains(t, systemPrompt(t, reqs[2]), "运行中新增偏好-edit-timing", "两次运行之间的写入在下次运行生效")
}

// testMemoryQuota：超配额（US-6 §6.1 第 5 项）。两档判据：① 库内已有的 36 KB 核心区
// 只记 WARN 且**完整注入**（不截断、不丢条）；② 超预算时**拒绝启动**（零 LLM 调用）。
func testMemoryQuota(t *testing.T) {
	t.Run("injection_over_quota", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		rows := make([]seedMemoryRow, 0, 9)
		for i := 0; i < 9; i++ {
			content := ""
			switch i {
			case 0:
				content = fixedWidthEntry("头部哨兵-quota-", 4000)
			case 8:
				tail := "-尾部结束"
				content = fixedWidthEntry("尾部哨兵-quota-", 4000-len(tail)) + tail
			default:
				content = fixedWidthEntry(fmt.Sprintf("配额条目-%02d-", i+1), 4000)
			}
			rows = append(rows, seedMemoryRow{Scope: "global", Section: "core", Content: content})
		}
		seedMemoryRows(t, h, rows...)

		fake := newFakeModel(t, textResponse("配额读数完成"))
		h.writeConfig(t, configWithProvider(fake.URL()))
		h.writeAgent(t, "demo", agentFrontmatter("demo", nil), "注入期配额读数")

		res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "读一下记忆")
		requireExit(t, res, 0, "超配额仍完整注入")
		requireContains(t, res.Stderr, "核心记忆区超出写配额，仍完整注入", "注入期 WARN")
		requireContains(t, res.Stderr, "bytes=36000", "WARN 必须带实际字节数")
		requireContains(t, res.Stderr, "limit=32768", "WARN 必须带上限")

		reqs := fake.Requests()
		if len(reqs) != 1 {
			t.Fatalf("假模型应收到 1 次请求，实际 %d 次", len(reqs))
		}
		system := systemPrompt(t, reqs[0])
		requireContains(t, system, "头部哨兵-quota-", "超配额不截断：首条仍在")
		requireContains(t, system, "-尾部结束", "超配额不丢条：末条仍在")
	})

	t.Run("budget_refusal", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		seedMemoryRows(t, h, seedMemoryRow{
			Scope: "global", Section: "core", Content: strings.Repeat("配额超限内容", 15000),
		})

		fake := newFakeModel(t) // 零步剧本：任何一次模型调用都会判红。
		h.writeConfig(t, configWithProvider(fake.URL())+"budget:\n  context_tokens: 8192\n")
		h.writeAgent(t, "demo", agentFrontmatter("demo", nil), "预算拒绝演示")

		res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "你好")
		if res.ExitCode == 0 {
			t.Fatalf("超预算必须拒绝启动\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
		}
		output := res.Stdout + res.Stderr
		requireContains(t, output, "上下文超限", "拒绝文案点名超限")
		requireContains(t, output, "budget.context_tokens=8192", "拒绝文案点名配置键与取值")
		if got := len(fake.Requests()); got != 0 {
			t.Fatalf("准入失败不得发起模型调用，实际 %d 次", got)
		}
		db := h.openDB(t)
		if n := scalarInt(t, db, `SELECT COUNT(*) FROM llm_calls`); n != 0 {
			t.Fatalf("llm_calls 应有 0 行，实际 %d 行", n)
		}
	})
}

// testMemoryConcurrency：并发抽验（US-6 §6.1 第 7 项）。判据：12 笔并发 save_memory 到
// 同一作用域，恰好 8 笔落盘（8 × 4096 = 32768 恰为上限）、4 笔被拒；无条目丢失、总量
// 不穿透上限。`-race` 由非 e2e 的 `go test -race` 覆盖，本进程级用例不复跑 `-race`。
func testMemoryConcurrency(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)

	var (
		mu       sync.Mutex
		success  int
		refused  int
		other    int
		requests int
	)
	server := testutil.NewHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("假服务：读请求体失败: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("假服务：请求体不是 JSON 对象: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		k := invokeOrdinal(userMessageOf(req), "并发保存-")
		if k == "" {
			t.Errorf("假服务：用户消息缺序号：%s", mustJSON(req))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if lastToolContent(req) == "" {
			arguments := mustJSON(map[string]string{"content": concurrencyEntry(k), "scope": "CORE"})
			_, _ = io.WriteString(w, toolCallResponse("call-"+k, "save_memory", arguments).Body)
			return
		}
		mu.Lock()
		switch tool := lastToolContent(req); {
		case strings.Contains(tool, "已记住（作用域：Agent 层 demo；分区：核心记忆）"):
			success++
		case strings.Contains(tool, quotaTotalRefusalCopy):
			refused++
		default:
			other++
			t.Errorf("假服务：未预期的工具结果：%s", tool)
		}
		mu.Unlock()
		_, _ = io.WriteString(w, textResponse("并发完成").Body)
	}))
	h.writeConfig(t, configWithAddr(server.URL, addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"save_memory"}), "并发保存演示")
	startServe(t, h, addr)

	const workers = 12
	type outcome struct {
		k       int
		reading invokeReading
		err     error
	}
	results := make([]outcome, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := fmt.Sprintf("%02d", i+1)
			reading, err := postInvoke(addr, "demo", "并发保存-"+k)
			results[i] = outcome{k: i + 1, reading: reading, err: err}
		}()
	}
	wg.Wait()

	for _, out := range results {
		if out.err != nil {
			t.Fatalf("第 %d 笔 invoke 失败: %v", out.k, out.err)
		}
		if out.reading.Status != "completed" {
			t.Fatalf("第 %d 笔运行状态 = %q，期望 completed", out.k, out.reading.Status)
		}
		if out.reading.FinalText != "并发完成" {
			t.Fatalf("第 %d 笔最终文本 = %q，期望 并发完成", out.k, out.reading.FinalText)
		}
	}

	mu.Lock()
	gotSuccess, gotRefused, gotOther, gotRequests := success, refused, other, requests
	mu.Unlock()
	if gotSuccess != 8 {
		t.Fatalf("并发 12 笔里应恰好 8 笔落盘，实际 %d 笔", gotSuccess)
	}
	if gotRefused != 4 {
		t.Fatalf("并发 12 笔里应恰好 4 笔被配额拒绝，实际 %d 笔", gotRefused)
	}
	if gotOther != 0 {
		t.Fatalf("出现 %d 笔非预期工具结果", gotOther)
	}
	if gotRequests != workers*2 {
		t.Fatalf("假服务应收到 %d 次请求（每笔两轮），实际 %d 次", workers*2, gotRequests)
	}

	// 落盘判据读**唯一运行载体**（D-18）：8 条生效核心行、每条 4096 字节、总量恰为上限。
	db := h.openDB(t)
	rows := queryRows(t, db,
		`SELECT content FROM memory_entries
		  WHERE scope = 'agent' AND agent_name = 'demo' AND section = 'core' AND state = 'active'
		  ORDER BY id`)
	if len(rows) != 8 {
		t.Fatalf("核心区应有 8 行，实际 %d 行", len(rows))
	}
	total := 0
	for _, row := range rows {
		entry := stringValue(t, row, "content")
		if len(entry) != memoryCoreEntryLimit {
			t.Fatalf("并发条目长度 = %d 字节，期望 %d 字节", len(entry), memoryCoreEntryLimit)
		}
		total += len(entry)
	}
	if total != memoryCoreTotalLimit {
		t.Fatalf("核心区总字节 = %d，期望恰为上限 %d", total, memoryCoreTotalLimit)
	}
}

// testMemoryRedaction：凭证与敏感面（US-6 §6.1 第 9 项）。判据：确认文案与配额拒绝文案
// 都不回显记忆正文；进程输出与审计 error_message 里不出现敏感内容；注入期 WARN 只给
// 路径与字节数，不打印正文。
func testMemoryRedaction(t *testing.T) {
	const sentinel = "敏感内容-sentinel-2b7f"

	t.Run("tool_copies", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		oversize := fixedWidthEntry("超限敏感-"+sentinel+"-", 4200)
		fake := newFakeModel(t,
			toolCallResponse("call-secret", "save_memory", mustJSON(map[string]string{
				"content": "敏感偏好-" + sentinel,
				"scope":   "CORE",
			})),
			toolCallResponse("call-oversize", "save_memory", mustJSON(map[string]string{
				"content": oversize,
				"scope":   "CORE",
			})),
			textResponse("本轮完成"),
		)
		h.writeConfig(t, configWithProvider(fake.URL()))
		h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"save_memory"}), "脱敏演示")

		res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "记住这些")
		requireExit(t, res, 0, "脱敏用例")
		reqs := fake.Requests()
		if len(reqs) != 3 {
			t.Fatalf("假模型应收到 3 次请求，实际 %d 次", len(reqs))
		}

		confirmation := toolResultContaining(t, reqs[1], "已记住（作用域：Agent 层 demo；分区：核心记忆）", "确认文案")
		requireAbsent(t, confirmation, sentinel, "确认文案不得回显记忆正文")

		refusal := toolResultContaining(t, reqs[2], quotaEntryRefusalCopy, "配额拒绝文案")
		requireAbsent(t, refusal, sentinel, "拒绝文案不得回显记忆正文")
		requireAbsent(t, res.Stdout+res.Stderr, sentinel, "进程输出不得回显记忆正文")

		db := h.openDB(t)
		rows := queryRows(t, db, `SELECT status, error_kind, error_message FROM tool_invocations ORDER BY id`)
		if len(rows) != 2 {
			t.Fatalf("tool_invocations 应有 2 行，实际 %d 行：%v", len(rows), rows)
		}
		if got, _ := rows[0]["status"].(string); got != "success" {
			t.Fatalf("第一行 status = %q，期望 success", got)
		}
		if got, _ := rows[1]["status"].(string); got != "failed" {
			t.Fatalf("第二行 status = %q，期望 failed", got)
		}
		if got, _ := rows[1]["error_kind"].(string); got != "invalid_input" {
			t.Fatalf("第二行 error_kind = %q，期望 invalid_input", got)
		}
		message, _ := rows[1]["error_message"].(string)
		if message != quotaEntryRefusalCopy {
			t.Fatalf("审计 error_message = %q，期望逐字为配额拒绝文案", message)
		}
		requireAbsent(t, message, sentinel, "审计错误信息不得回显记忆正文")
	})

	t.Run("injection_warn", func(t *testing.T) {
		h := newHarness(t)
		initWorkspace(t, h)
		entry := fixedWidthEntry("敏感注入-"+sentinel+"-", 5000)
		seedMemoryRows(t, h, seedMemoryRow{Scope: "agent", Agent: "demo", Section: "core", Content: entry})

		fake := newFakeModel(t, textResponse("已读取记忆"))
		h.writeConfig(t, configWithProvider(fake.URL()))
		h.writeAgent(t, "demo", agentFrontmatter("demo", nil), "超限注入演示")

		res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "你好")
		requireExit(t, res, 0, "超限注入")
		requireContains(t, res.Stderr, "核心记忆单条超出写配额，仍完整注入", "注入期单条 WARN")
		requireContains(t, res.Stderr, "bytes=5000", "WARN 必须带实际字节数")
		requireAbsent(t, res.Stdout+res.Stderr, sentinel, "注入期 WARN 不得打印记忆正文")

		reqs := fake.Requests()
		if len(reqs) != 1 {
			t.Fatalf("假模型应收到 1 次请求，实际 %d 次", len(reqs))
		}
		requireContains(t, systemPrompt(t, reqs[0]), sentinel, "超限条目仍完整注入（不截断）")
	})
}

// testMemoryArchiveSize：归档区体量边界读数（US-6 §6.1 第 13 项）。200 条 × 1000
// 字节；只记录运行耗时读数，**不设达标线**。判据：运行成功、归档内容不进系统提示词。
// 载体两档已合一（D-18 起运行期只有 PostgreSQL），保留一次读数。
func testMemoryArchiveSize(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	rows := make([]seedMemoryRow, 0, 200)
	for _, entry := range archiveEntries(200, 1000) {
		rows = append(rows, seedMemoryRow{Scope: "global", Section: "archive", Content: entry})
	}
	seedMemoryRows(t, h, rows...)

	fake := newFakeModel(t, textResponse("归档读数完成"))
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil), "归档体量读数")

	start := time.Now()
	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "你好")
	elapsed := time.Since(start)
	requireExit(t, res, 0, "归档体量")
	t.Logf("archive_size：200 条 × 1000 字节，运行耗时 %s（读数，不设达标线）", elapsed)

	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("假模型应收到 1 次请求，实际 %d 次", len(reqs))
	}
	requireAbsent(t, systemPrompt(t, reqs[0]), "归档条目-001-", "归档区不注入系统提示词")
}

// testMemoryScopes：两层作用域现场（run-state U-1）。判据：Agent 层非空 → 只用 Agent 层
// 核心区；Agent 层只有归档条目 → 生效层仍是它自己、核心区为空且**不回退**全局层；
// 无 Agent 层 → 注入全局层核心区。
func testMemoryScopes(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	seedMemoryRows(t, h,
		seedMemoryRow{Scope: "global", Section: "core", Content: "全局层偏好-GLOBAL"},
		seedMemoryRow{Scope: "agent", Agent: "scoped", Section: "core", Content: "Agent层偏好-ALPHA"},
		seedMemoryRow{Scope: "agent", Agent: "scoped", Section: "archive", Content: "Agent归档-BETA"},
		seedMemoryRow{Scope: "agent", Agent: "archonly", Section: "archive", Content: "仅归档-ONLY"},
	)

	fake := newFakeModel(t,
		textResponse("scoped 回答"),
		textResponse("archonly 回答"),
		textResponse("plain 回答"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	for _, name := range []string{"scoped", "archonly", "plain"} {
		h.writeAgent(t, name, agentFrontmatter(name, nil), name+" 作用域演示")
	}

	for _, name := range []string{"scoped", "archonly", "plain"} {
		res := h.run(t, fixtureEnv(), "chat", "--agent", name, "--message", "你是谁")
		requireExit(t, res, 0, name+" 作用域运行")
	}
	reqs := fake.Requests()
	if len(reqs) != 3 {
		t.Fatalf("假模型应收到 3 次请求，实际 %d 次", len(reqs))
	}

	scopedSystem := systemPrompt(t, reqs[0])
	requireContains(t, scopedSystem, "Agent层偏好-ALPHA", "Agent 层核心区注入")
	requireAbsent(t, scopedSystem, "全局层偏好-GLOBAL", "两层不叠加：全局层不注入")
	requireAbsent(t, scopedSystem, "Agent归档-BETA", "归档区不注入")

	archOnlySystem := systemPrompt(t, reqs[1])
	requireAbsent(t, archOnlySystem, "Agent层偏好-ALPHA", "archonly 没有核心条目")
	requireAbsent(t, archOnlySystem, "全局层偏好-GLOBAL", "Agent 层有归档条目时生效层是它自己，不回退全局")
	requireAbsent(t, archOnlySystem, memoryCoreHeading, "核心区为空时段 2 不产生任何字符")

	plainSystem := systemPrompt(t, reqs[2])
	requireContains(t, plainSystem, "全局层偏好-GLOBAL", "无 Agent 层时注入全局层核心区")
	requireAbsent(t, plainSystem, "Agent层偏好-ALPHA", "不得串层注入")
}

// testMemoryCorrupt：损坏的旧载体文件（run-state U-1；D-22 改判）。旧 `MEMORY.md` 只在
// `db switch` 的一次性导入路径被解析，运行期不读——运行期看到结构损坏的文件**不是
// 故障**：运行照常成功、文件逐字节不变、损坏条目不得混进注入面。导入路径对损坏层的
// 显式拒绝另由 `TestSwitch_RefusesWhenLegacyMemoryUnreadable`（integration）判卷。
// 逆改点：把旧文件重新接回运行期读取 → 解析失败会拒绝运行、读成功则损坏条目进提示词，
// 两条断言各自红。
func testMemoryCorrupt(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	path := memoryPath(h, "")
	broken := memoryGlobalTitle + "\n\n" + memoryCoreHeading + "\n\n- 损坏会话条目\n"
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatalf("写损坏文件失败: %v", err)
	}
	before := fileDigest(t, path)

	fake := newFakeModel(t, textResponse("损坏旧文件不影响运行"))
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", nil), "损坏旧文件演示")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "你好")
	requireExit(t, res, 0, "损坏的旧载体文件不参与运行期读写")
	reqs := fake.Requests()
	if len(reqs) != 1 {
		t.Fatalf("假模型应收到 1 次请求，实际 %d 次", len(reqs))
	}
	requireAbsent(t, systemPrompt(t, reqs[0]), "损坏会话条目", "损坏旧文件不得进注入面（D-22）")
	if got := fileDigest(t, path); got != before {
		t.Fatalf("运行期不得改写旧载体文件（哈希 %s → %s）", before, got)
	}
}

// memoryPath 返回某层记忆文件的路径；agent 为空串即全局层。
// memoryPath returns the layer file's path; an empty agent means the global layer.
func memoryPath(h *harness, agent string) string {
	if agent == "" {
		return filepath.Join(h.Root, ".basalt", "memory", "MEMORY.md")
	}
	return filepath.Join(h.Root, ".basalt", "memory", "agents", agent, "MEMORY.md")
}

// writeMemoryLayer 按文档化形态手写一层记忆文件（模拟人手编辑）。
// writeMemoryLayer hand-writes one layer in the documented shape.
func writeMemoryLayer(t *testing.T, h *harness, agent string, core, archive []string) {
	t.Helper()
	title := memoryGlobalTitle
	if agent != "" {
		title = fmt.Sprintf(memoryAgentTitleFmt, agent)
	}
	path := memoryPath(h, agent)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建记忆目录 %s 失败: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(renderMemoryLayer(title, core, archive)), 0o644); err != nil {
		t.Fatalf("写记忆文件 %s 失败: %v", path, err)
	}
}

// renderMemoryLayer 重建一层记忆文件的规范形态（标题 + 两分区，单行条目）。
// renderMemoryLayer renders one layer in the canonical single-line-entry shape.
func renderMemoryLayer(title string, core, archive []string) string {
	return title + "\n\n" + renderMemorySection(memoryCoreHeading, core) +
		"\n\n" + renderMemorySection(memoryArchiveHeading, archive) + "\n"
}

func renderMemorySection(heading string, entries []string) string {
	if len(entries) == 0 {
		return heading
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, "- "+entry)
	}
	return heading + "\n\n" + strings.Join(lines, "\n\n")
}

// readText 读一个文本文件（失败即判红）。
// readText reads a text file and fails the test on error.
func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return string(raw)
}

// toolResultContaining 返回第一条包含指定片段的工具结果正文。
// toolResultContaining returns the first tool-result content carrying the fragment.
func toolResultContaining(t *testing.T, req map[string]any, want, label string) string {
	t.Helper()
	for _, content := range toolResults(t, req) {
		if strings.Contains(content, want) {
			return content
		}
	}
	t.Fatalf("%s：没有工具结果包含 %q\n%s", label, want, requestText(t, req))
	return ""
}

// fixedWidthEntry 生成恰好 size 字节的单行条目：前缀之后用 ASCII 填充。
// fixedWidthEntry builds a single-line entry of exactly size bytes.
func fixedWidthEntry(prefix string, size int) string {
	if len(prefix) > size {
		panic("e2e fixture: 前缀超过目标宽度")
	}
	return prefix + strings.Repeat("a", size-len(prefix))
}

// archiveEntries 生成 count 条各 size 字节的归档条目（体量读数用）。
// archiveEntries builds count archive entries of size bytes each.
func archiveEntries(count, size int) []string {
	entries := make([]string, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, fixedWidthEntry(fmt.Sprintf("归档条目-%03d-", i+1), size))
	}
	return entries
}

// seedMemoryRow 描述一条直接种进 PostgreSQL 的生效记忆行：绕过工具面，代表"库里已有
// 的存量状态"。D-18 / D-22 之后运行期只读 PostgreSQL，旧 markdown 文件不再参与运行期
// 读写——凡是"手写文件 → 运行期读到"的旧判据都改为"种库内状态 → 运行期读到"。
// seedMemoryRow describes one active row seeded straight into PostgreSQL.
type seedMemoryRow struct {
	Scope   string // global / agent
	Agent   string // scope=global 时留空
	Section string // core / archive
	Content string
}

// insertMemoryRowDirect 按写入路径同口径插一条生效行（`state='active'`、
// `source='save_memory'`、`valid_from=created_at`、正文 sha256）。列语义照抄
// `insertMemoryRow`：`valid_from` / `content_hash` 在 PostgreSQL 谱系里 NOT NULL 且
// 无默认值，`agent_name` 用 NULL 表示全局层（`Load` 按 IS NOT DISTINCT FROM 匹配）。
// 列名漂移时这里先红。
// insertMemoryRowDirect mirrors insertMemoryRow's column semantics.
func insertMemoryRowDirect(ctx context.Context, db *sql.DB, row seedMemoryRow, createdAt time.Time) error {
	var agent any
	if row.Agent != "" {
		agent = row.Agent
	}
	sum := sha256.Sum256([]byte(row.Content))
	if _, err := db.ExecContext(ctx,
		`INSERT INTO memory_entries
		   (scope, agent_name, section, content, created_at, state, source, valid_from, content_hash)
		 VALUES ($1, $2, $3, $4, $5, 'active', 'save_memory', $5, $6)`,
		row.Scope, agent, row.Section, row.Content, createdAt.UnixMilli(),
		hex.EncodeToString(sum[:])); err != nil {
		return fmt.Errorf("种入记忆行（%s/%s）: %w", row.Scope, row.Section, err)
	}
	return nil
}

// seedMemoryRows 先确保运行期 schema 已迁移，再按给定顺序逐条种行（时间戳递增；
// `Load` 按 id 排序，判据不依赖时钟精度）。
// seedMemoryRows migrates the runtime schema, then inserts the rows in order.
func seedMemoryRows(t *testing.T, h *harness, rows ...seedMemoryRow) {
	t.Helper()
	// 写行前先确保运行期 schema 已迁移（见 ensureStore）。
	// The runtime schema is migrated before the rows are written; see ensureStore.
	h.ensureStore(t)
	db := h.openDB(t)
	base := time.Now()
	for i, row := range rows {
		createdAt := base.Add(time.Duration(i) * time.Millisecond)
		if err := insertMemoryRowDirect(context.Background(), db, row, createdAt); err != nil {
			t.Fatalf("种入第 %d 条记忆行失败: %v", i+1, err)
		}
	}
}

// concurrencyEntry 生成恰好 4096 字节的并发条目：序号前缀 + ASCII 填充。
// concurrencyEntry builds exactly one 4096-byte entry for the concurrency case.
func concurrencyEntry(k string) string {
	return fixedWidthEntry("并发条目-"+k+"-", memoryCoreEntryLimit)
}

// invokeOrdinal 从用户消息里取前缀之后的序号（无匹配返回空串）。
// invokeOrdinal extracts the ordinal suffix after prefix from the user message.
func invokeOrdinal(message, prefix string) string {
	idx := strings.Index(message, prefix)
	if idx < 0 {
		return ""
	}
	rest := message[idx+len(prefix):]
	if len(rest) < 2 {
		return ""
	}
	return rest[:2]
}

// userMessageOf 返回请求里最后一条用户消息的正文（无则空串）。
// userMessageOf returns the last user message's content, or an empty string.
func userMessageOf(req map[string]any) string {
	raw, _ := req["messages"].([]any)
	last := ""
	for _, item := range raw {
		msg, _ := item.(map[string]any)
		if msg == nil {
			continue
		}
		if role, _ := msg["role"].(string); role != "user" {
			continue
		}
		if content, ok := msg["content"].(string); ok {
			last = content
		}
	}
	return last
}

// lastToolContent 返回请求里最后一条工具结果消息的正文（无则空串）。
// lastToolContent returns the last tool message's content, or an empty string.
func lastToolContent(req map[string]any) string {
	raw, _ := req["messages"].([]any)
	last := ""
	for _, item := range raw {
		msg, _ := item.(map[string]any)
		if msg == nil {
			continue
		}
		if role, _ := msg["role"].(string); role != "tool" {
			continue
		}
		if content, ok := msg["content"].(string); ok {
			last = content
		}
	}
	return last
}

// configWithAddr 在最小可装载配置上追加 http 段：监听回环端口 + 管理认证（T025 起
// serve 没有管理认证配置就拒绝启动）。
// configWithAddr appends the http section (listen address plus management auth);
// serve refuses to start without the auth block since T025.
func configWithAddr(baseURL, addr string) string {
	return configWithProvider(baseURL) + "http:\n  addr: " + addr + "\n" + adminAuthSection()
}

// freeLoopbackAddr 取一个当前空闲的回环地址（监听 0 端口后立刻释放）。
// freeLoopbackAddr reserves then releases a loopback port and returns its address.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请回环端口失败: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("释放回环端口失败: %v", err)
	}
	return addr
}

// startServe 拉起 `basalt serve` 并等健康检查就绪。
// startServe launches `basalt serve` and waits for the health endpoint.
func startServe(t *testing.T, h *harness, addr string) *process {
	t.Helper()
	p := h.start(t, fixtureEnv(), "serve")
	p.waitHTTP(t, "http://"+addr+"/api/v1/health", 30*time.Second)
	return p
}

// invokeReading 是无状态 invoke 响应的判卷读数。
// invokeReading is the verdict-side reading of one stateless invoke response.
type invokeReading struct {
	RunID     string
	SessionID string
	Agent     string
	Status    string
	Reason    string
	FinalText string
}

// postInvoke 发一次无状态调用；可在子 goroutine 里调用（不触碰 *testing.T）。
// postInvoke performs one stateless invoke; it is safe off the test goroutine.
func postInvoke(addr, agent, message string) (invokeReading, error) {
	payload, err := json.Marshal(map[string]string{"message": message})
	if err != nil {
		return invokeReading{}, err
	}
	client := &http.Client{Timeout: 90 * time.Second}
	req, err := http.NewRequest(
		http.MethodPost, "http://"+addr+"/api/v1/agents/"+agent+"/invoke",
		bytes.NewReader(payload))
	if err != nil {
		return invokeReading{}, fmt.Errorf("组装 invoke 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(applyAdminAuth(req))
	if err != nil {
		return invokeReading{}, fmt.Errorf("调用 invoke 失败: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return invokeReading{}, fmt.Errorf("读 invoke 响应失败: %w", err)
	}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			RunID     string `json:"run_id"`
			SessionID string `json:"session_id"`
			Agent     string `json:"agent"`
			Status    string `json:"status"`
			Reason    string `json:"reason"`
			FinalText string `json:"final_text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return invokeReading{}, fmt.Errorf("响应不是信封 JSON（HTTP %d）: %v（原文 %s）", resp.StatusCode, err, raw)
	}
	if envelope.Code != "OK" {
		return invokeReading{}, fmt.Errorf("invoke 失败：HTTP %d code=%s message=%s", resp.StatusCode, envelope.Code, envelope.Message)
	}
	if envelope.Data == nil {
		return invokeReading{}, fmt.Errorf("invoke 响应缺少 data：%s", raw)
	}
	return invokeReading{
		RunID:     envelope.Data.RunID,
		SessionID: envelope.Data.SessionID,
		Agent:     envelope.Data.Agent,
		Status:    envelope.Data.Status,
		Reason:    envelope.Data.Reason,
		FinalText: envelope.Data.FinalText,
	}, nil
}

// invokeAgent 在主 goroutine 上发一次无状态调用，失败即判红。
// invokeAgent performs one stateless invoke on the test goroutine.
func invokeAgent(t *testing.T, addr, agent, message string) invokeReading {
	t.Helper()
	reading, err := postInvoke(addr, agent, message)
	if err != nil {
		t.Fatalf("%s 的 invoke 失败: %v", agent, err)
	}
	return reading
}
