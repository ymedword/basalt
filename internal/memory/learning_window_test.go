package memory

// T016 / T017 的窗口、游标、预算、重试与提交用例（颗粒度文档 §3.1.2 / §3.1.3 / §3.1.5 /
// §3.1.7，FR-22-03 / FR-22-06 / FR-22-07 / FR-22-10）。断言面是**传给下游的入参**与
// **下游收到的终态**：窗口在前两次尝试之间是否逐字复用、游标左界是否原样进消息端口、
// 预算耗尽时是否零模型调用、冻结窗口缺消息时是否转 window_gone 而不是拿残缺集合抽取、
// 提交失败时是否留 pending 而不是写 failed。
//
// The window/cursor/budget/retry/commit cases: they assert the arguments handed to the
// message port and the terminal state the store receives.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errTestCursor 模拟游标读取失败（存储不可用 / 事务失败）。
var errTestCursor = errors.New("learning: cursor unavailable")

// errTestWindow 模拟消息窗口读取失败。
var errTestWindow = errors.New("learning: window unavailable")

// errTestEntries 模拟既有条目快照读取失败。
var errTestEntries = errors.New("learning: entries unavailable")

// failedRow 构造一行可重试的 failed 抽取账本（窗口 10..12，三条消息）。
// failedRow builds one retryable failed ledger row frozen on window 10..12.
func failedRow(attempts int) *core.LearningExtraction {
	return &core.LearningExtraction{
		ExtractionID:        "lrn_retry",
		Agent:               "demo",
		SessionID:           "sess_1",
		RunID:               "run_0",
		LastAttemptRunID:    "run_0",
		OriginCallerID:      "caller-a",
		OriginKeyID:         "key-1",
		Status:              core.LearningExtractionFailed,
		WindowFromMessageID: 10,
		WindowToMessageID:   12,
		WindowMessages:      3,
		PriorMessages:       1,
		Attempts:            attempts,
		CreatedAt:           learningTestNow.Add(-time.Hour),
	}
}

// newLearningLimitsRig 用显式上限装配服务；未给出的字段由 normaliseLimits 补缺省值。
// newLearningLimitsRig assembles the service with explicit limits over the defaults.
func newLearningLimitsRig(t *testing.T, limits LearningLimits, logger *slog.Logger) (*LearningService, *learningFakeStore, *learningFakeMessages) {
	t.Helper()
	store := &learningFakeStore{entries: learningEntriesFixture()}
	messages := &learningFakeMessages{window: learningWindowFixture()}
	service := NewLearningService(LearningDeps{
		Store:     store,
		Messages:  messages,
		Enabled:   true,
		Agents:    []string{"demo"},
		DraftRoot: filepath.Join(t.TempDir(), "drafts"),
		Limits:    limits,
		Logger:    logger,
		Now:       func() time.Time { return learningTestNow },
	})
	return service, store, messages
}

// learningCapturedLogger 返回写进内存的日志面（DEBUG 级起），供 WARN 断言用。
// learningCapturedLogger returns an in-memory handler for WARN assertions.
func learningCapturedLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// TestUS22_Window_BoundsAndDrop 守 FR-22-03：首次尝试把窗口边界、条数、前文条数与丢弃数
// 写进抽取行；消息端口的游标左界与双字节上限逐项取 limits（不是手写默认值）。
// The first attempt freezes the window on the ledger row and passes the exact limits.
func TestUS22_Window_BoundsAndDrop(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.cursor = core.LearningCursor{
		Agent: "demo", SessionID: "sess_1", CommittedMessageID: 77,
	}
	window := learningWindowFixture()
	window.Dropped = 4
	rig.messages.window = window
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.messages.extractionCalls)
	require.Equal(t, "sess_1", rig.messages.extractionSession, "新窗口按触发 Run 的会话读取")
	require.Equal(t, int64(77), rig.messages.afterMessageID, "游标左界必须原样进消息端口")
	require.Equal(t, 6, rig.messages.priorLimit)
	require.Equal(t, 40, rig.messages.newLimit)
	require.Equal(t, 2048, rig.messages.priorBytes)
	require.Equal(t, 8192, rig.messages.newBytes)

	require.Equal(t, 1, rig.store.beginCalls)
	row := rig.store.beginRow
	require.Equal(t, int64(10), row.WindowFromMessageID)
	require.Equal(t, int64(12), row.WindowToMessageID)
	require.Equal(t, 3, row.WindowMessages)
	require.Equal(t, 1, row.PriorMessages)
	require.Equal(t, 4, row.DroppedWindowMessages, "丢弃条数如实落行，供状态页核对")
	require.Equal(t, "run_1", row.RunID)
	require.Equal(t, "run_1", row.LastAttemptRunID)
	require.Equal(t, "local", row.Provider)
	require.Equal(t, "model-a", row.Model)
	require.Equal(t, "cfg-1", row.ConfigHash)
	require.Equal(t, learningPromptVersion, row.PromptVersion)
	require.Equal(t, learningRuleVersion, row.RuleVersion)
}

// TestUS22_Window_FrozenSameOriginOnly 守 C-13：重试只按**冻结行自己的**会话与来源重读
// 同一段窗口——当前 Run 的会话即使不同也不得据此改读，否则跨来源消息会被贴成新身份。
// A retry re-reads the frozen row's own session and origin, never the current run's.
func TestUS22_Window_FrozenSameOriginOnly(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.unresolved = failedRow(1) // SessionID sess_1，来源 caller-a / key-1
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	input := learningRunInput(extractor)
	input.SessionID = "sess-other" // 当前 Run 的会话与冻结行不同
	rig.service.AfterRun(context.Background(), input,
		core.RunResult{RunID: "run_2", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.messages.frozenCalls)
	require.Zero(t, rig.messages.extractionCalls, "重试不得改读新窗口")
	require.Equal(t, "sess_1", rig.messages.frozenSession, "冻结窗口按冻结行自己的会话重读")
	require.Equal(t, [2]string{"caller-a", "key-1"}, rig.messages.frozenOrigin,
		"冻结窗口按冻结行的来源过滤，不取当前 Run 的身份")
}

// TestUS22_Window_ExcludesToolAndSystem 守 FR-22-03 的角色过滤：工具 / 系统消息即使
// 出现在窗口里也不得充当证据（窗口读取面已剔除它们；此处是第二道程序校验）——引用它们
// 的条目被丢弃，同批合法条目照常保留。
// Tool/system messages never back evidence even if a window ever exposes them.
func TestUS22_Window_ExcludesToolAndSystem(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	base := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	window := learningWindowFixture()
	window.New = append(window.New,
		core.LearningMessage{MessageID: 13, RunID: "run_1", Role: "tool", Content: "工具输出：部署命令成功", CreatedAt: base},
		core.LearningMessage{MessageID: 14, RunID: "run_1", Role: "system", Content: "系统提示：你是助手", CreatedAt: base},
	)
	rig.messages.window = window

	extractor := &learningFakeExtractor{reply: `{"items":[
		{"kind":"fact","content":"部署命令成功","evidence":[{"message_id":13,"role":"tool","quote":"部署命令成功"}]},
		{"kind":"fact","content":"助手身份设定","evidence":[{"message_id":14,"role":"system","quote":"系统提示"}]},
		{"kind":"fact","content":"默认语言为中文","evidence":[{"message_id":10,"role":"user","quote":"我习惯用中文写日报"}]}
	]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.store.commitCalls)
	require.Len(t, rig.store.commitReq.Candidates, 1, "工具 / 系统消息不得作证据")
	require.Equal(t, "默认语言为中文", rig.store.commitReq.Candidates[0].Content,
		"同批合法条目不受牵连")
	require.Equal(t, 2, rig.store.commitReq.ItemsDropped)
}

// TestUS22_Prior_NotEvidence 守 C-10：前文只补足指代，永不作证据——引用前文消息的条目
// 被丢弃；同批引用窗口消息的条目照常保留。
// Prior messages never back evidence; a same-batch in-window item survives.
func TestUS22_Prior_NotEvidence(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[
		{"kind":"fact","content":"更早的部署约定","evidence":[{"message_id":8,"role":"user","quote":"更早的消息"}]},
		{"kind":"fact","content":"项目使用中文日报","evidence":[{"message_id":10,"role":"user","quote":"我习惯用中文写日报"}]}
	]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.store.commitCalls)
	require.Len(t, rig.store.commitReq.Candidates, 1)
	require.Equal(t, "项目使用中文日报", rig.store.commitReq.Candidates[0].Content)
	require.Equal(t, 1, rig.store.commitReq.ItemsDropped)
}

// TestUS22_Window_PriorBytesCapped 守 FR-22-03：窗口与前文的两个上限逐项来自 limits
// （不是写死 40 / 8192），换一组上限必须原样出现在消息端口入参上。
// The window and prior limits are passed verbatim from the configured limits.
func TestUS22_Window_PriorBytesCapped(t *testing.T) {
	t.Parallel()
	service, _, messages := newLearningLimitsRig(t, LearningLimits{
		WindowMessages: 7, WindowBytes: 1024, PriorMessages: 3, PriorBytes: 128,
	}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, messages.extractionCalls)
	require.Equal(t, 3, messages.priorLimit)
	require.Equal(t, 128, messages.priorBytes)
	require.Equal(t, 7, messages.newLimit)
	require.Equal(t, 1024, messages.newBytes)
}

// TestUS22_EntrySnapshot_BytesCapped 守 FR-22-03：条目快照的条数 / 字节上限来自 limits；
// 快照读取失败时放弃本次抽取，不建行、不调用。
// The entry snapshot limits come from the configured limits; a read failure aborts.
func TestUS22_EntrySnapshot_BytesCapped(t *testing.T) {
	t.Parallel()
	t.Run("limits-passed", func(t *testing.T) {
		service, store, _ := newLearningLimitsRig(t, LearningLimits{EntrySnapshot: 4, EntrySnapshotBytes: 512}, nil)
		extractor := &learningFakeExtractor{reply: `{"items":[]}`}
		service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
		require.Equal(t, 4, store.entriesLimit)
		require.Equal(t, 512, store.entriesBytes)
		require.Equal(t, 1, store.beginCalls)
	})
	t.Run("read-error-noop", func(t *testing.T) {
		logger, buf := learningCapturedLogger()
		rig := newLearningTestRig(t, true, []string{"demo"}, logger)
		rig.store.entriesErr = errTestEntries
		extractor := &learningFakeExtractor{reply: `{"items":[]}`}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
		require.Zero(t, rig.store.beginCalls, "快照读不到不建抽取行")
		require.Zero(t, extractor.calls)
		require.Zero(t, rig.store.commitCalls)
		require.Zero(t, rig.store.failCalls)
		require.Contains(t, buf.String(), "读取既有条目失败")
	})
}

// TestUS22_Window_EmptyNoop 守 FR-22-03：窗口为空时不建账本行、不调用模型——"没有新
// 消息"不是一次抽取尝试，不应消耗预算，游标也不动。
// An empty window creates no ledger row and calls no model.
func TestUS22_Window_EmptyNoop(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.messages.window = core.LearningWindow{}
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.messages.extractionCalls)
	require.Zero(t, rig.store.beginCalls)
	require.Zero(t, extractor.calls)
	require.Zero(t, rig.store.commitCalls, "游标只由成功提交 / 跳过 / 恢复推进")
	require.Zero(t, rig.store.failCalls)
}

// TestUS22_Window_ReadErrorNoop 守失败矩阵：消息窗口读取失败记 WARN，不建行、不调用、
// 不写失败（读不到不等于抽取失败，重试条件尚未成立）。
// A window read failure is a WARN no-op: no ledger row, no call, no failure row.
func TestUS22_Window_ReadErrorNoop(t *testing.T) {
	t.Parallel()
	logger, buf := learningCapturedLogger()
	rig := newLearningTestRig(t, true, []string{"demo"}, logger)
	rig.messages.err = errTestWindow
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.messages.extractionCalls)
	require.Zero(t, rig.store.beginCalls)
	require.Zero(t, extractor.calls)
	require.Zero(t, rig.store.commitCalls)
	require.Zero(t, rig.store.failCalls)
	require.Contains(t, buf.String(), "读取消息窗口失败")
	require.Contains(t, buf.String(), "level=WARN")
}

// TestUS22_Window_PartialGoneSkips 守 C-33：冻结窗口重读后条数少于冻结条数（消息被
// 保留策略清理）→ 记 `window_gone`，不拿残缺集合抽取，也不提交。
// A partially cleaned frozen window becomes window_gone instead of a partial extraction.
func TestUS22_Window_PartialGoneSkips(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.unresolved = failedRow(1)
	partial := learningWindowFixture()
	partial.New = partial.New[:2] // 冻结时 3 条，现在只剩 2 条
	rig.messages.window = partial
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_2", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.messages.frozenCalls)
	require.Zero(t, rig.messages.extractionCalls, "不得退回新窗口读取")
	require.Zero(t, extractor.calls, "残缺窗口不得发出模型调用")
	require.Zero(t, rig.store.beginCalls)
	require.Zero(t, rig.store.commitCalls)
	require.Equal(t, 1, rig.store.failCalls)
	require.Equal(t, core.LearningFailureWindowGone, rig.store.failClass)
	require.Contains(t, rig.store.failMessage, "frozen window incomplete")
}

// TestUS22_Window_FullGoneSkipsAndAdvances 守 C-33：窗口全部被清理同样转
// `window_gone`（终态由存储层按类别置 skipped 并推进游标）。
// A fully cleaned window takes the same window_gone path; the store advances the cursor.
func TestUS22_Window_FullGoneSkipsAndAdvances(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.unresolved = failedRow(2)
	rig.messages.window = core.LearningWindow{}
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_3", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.messages.frozenCalls)
	require.Zero(t, extractor.calls)
	require.Zero(t, rig.store.commitCalls)
	require.Equal(t, 1, rig.store.failCalls)
	require.Equal(t, core.LearningFailureWindowGone, rig.store.failClass)
}

// TestUS22_Retry_FrozenWindowOnly 守 FR-22-10 / §2.3.3：同来源失败行的下一次尝试只重读
// 冻结窗口（边界与条数逐字复用、沿用原抽取行），不重新计算窗口。
// A retry re-reads the frozen window and reuses the same ledger row.
func TestUS22_Retry_FrozenWindowOnly(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.unresolved = failedRow(1)
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_2", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.messages.frozenCalls)
	require.Zero(t, rig.messages.extractionCalls, "重试不得改读新窗口")
	require.Equal(t, int64(10), rig.messages.frozenFrom, "冻结左界逐字复用")
	require.Equal(t, int64(12), rig.messages.frozenTo, "冻结右界逐字复用")

	require.Equal(t, 1, rig.store.beginCalls)
	row := rig.store.beginRow
	require.Equal(t, "lrn_retry", row.ExtractionID, "同一失败行继续用，不新建主身份")
	require.Equal(t, int64(10), row.WindowFromMessageID, "冻结边界不变")
	require.Equal(t, int64(12), row.WindowToMessageID)
	require.Equal(t, 3, row.WindowMessages)
	require.Equal(t, 1, row.PriorMessages)
}

// TestUS22_Retry_AttemptBindsRunModel 守 FR-22-10：重试的尝试身份绑定**本次**触发 Run
// （last_attempt_run_id / provider / model / config_hash 全部更新），调用参数取本次身份。
// A retry rebinds the attempt to the current run's provider / model / config hash.
func TestUS22_Retry_AttemptBindsRunModel(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.unresolved = failedRow(1)
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	input := core.LearningRunInput{
		RunID:       "run_2",
		Agent:       "demo",
		SessionID:   "sess_1",
		Channel:     "cli",
		Provider:    "local-2",
		Model:       "model-b",
		ConfigHash:  "cfg-2",
		CompletedAt: learningTestNow.Add(time.Minute),
		Extractor:   extractor,
	}
	rig.service.AfterRun(context.Background(), input,
		core.RunResult{RunID: "run_2", Status: core.StatusCompleted})

	row := rig.store.beginRow
	require.Equal(t, "run_2", row.LastAttemptRunID, "尝试身份绑定本次 Run")
	require.Equal(t, learningTestNow.Add(time.Minute).UnixMilli(), row.LastAttemptAt.UnixMilli())
	require.Equal(t, "local-2", row.Provider)
	require.Equal(t, "model-b", row.Model)
	require.Equal(t, "cfg-2", row.ConfigHash)

	require.Equal(t, 1, extractor.calls, "重试照常发一次调用")
	call := extractor.reqs[0]
	require.Equal(t, "run_2", call.RunID)
	require.Equal(t, "local-2", call.Provider, "调用参数取本次尝试身份")
	require.Equal(t, "model-b", call.Model)
	require.Equal(t, "cfg-2", call.ConfigHash)
}

// TestUS22_Retry_NonRetryableBlocks 守 FR-22-10：失败满 3 次（或 pending / 未查证
// unknown）不再重试——同来源新窗口被阻断，零模型调用、零新行。
// A failed row at the retry ceiling no longer retries; the next run is blocked.
func TestUS22_Retry_NonRetryableBlocks(t *testing.T) {
	t.Parallel()
	cases := map[string]*core.LearningExtraction{
		"failed-at-ceiling": failedRow(3),
		"pending": {
			ExtractionID: "lrn_pending", Agent: "demo", SessionID: "sess_1",
			Status: core.LearningExtractionPending, Attempts: 1,
		},
		"unverified-unknown": {
			ExtractionID: "lrn_unknown", Agent: "demo", SessionID: "sess_1",
			Status: core.LearningExtractionUnknown, Attempts: 1,
		},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newLearningTestRig(t, true, []string{"demo"}, nil)
			rig.store.unresolved = row
			extractor := &learningFakeExtractor{reply: `{"items":[]}`}

			rig.service.AfterRun(context.Background(), learningRunInput(extractor),
				core.RunResult{RunID: "run_2", Status: core.StatusCompleted})

			require.Zero(t, rig.messages.extractionCalls)
			require.Zero(t, rig.messages.frozenCalls)
			require.Zero(t, extractor.calls)
			require.Zero(t, rig.store.beginCalls)
			require.Zero(t, rig.store.failCalls, "阻断不是失败终态，不写 failed 行")
		})
	}
}

// TestUS22_Unresolved_BlocksSameOrigin 守 C-13：同来源 pending 与**未查证** unknown
// 阻断新窗口（零写入零调用）；已查证 unknown（verified_at 非空）不再阻断，改走新窗口。
// Pending and unverified-unknown block; a verified unknown no longer blocks.
func TestUS22_Unresolved_BlocksSameOrigin(t *testing.T) {
	t.Parallel()
	t.Run("pending-blocks", func(t *testing.T) {
		rig := newLearningTestRig(t, true, []string{"demo"}, nil)
		rig.store.unresolved = &core.LearningExtraction{
			ExtractionID: "lrn_pending", Agent: "demo", SessionID: "sess_1",
			Status: core.LearningExtractionPending, Attempts: 1,
		}
		extractor := &learningFakeExtractor{reply: `{"items":[]}`}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_2", Status: core.StatusCompleted})
		require.Zero(t, rig.messages.extractionCalls)
		require.Zero(t, rig.messages.frozenCalls)
		require.Zero(t, rig.store.beginCalls)
		require.Zero(t, extractor.calls)
	})
	t.Run("unverified-unknown-blocks", func(t *testing.T) {
		rig := newLearningTestRig(t, true, []string{"demo"}, nil)
		rig.store.unresolved = &core.LearningExtraction{
			ExtractionID: "lrn_unknown", Agent: "demo", SessionID: "sess_1",
			Status: core.LearningExtractionUnknown, Attempts: 1,
		}
		extractor := &learningFakeExtractor{reply: `{"items":[]}`}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_2", Status: core.StatusCompleted})
		require.Zero(t, rig.messages.frozenCalls, "未查证 unknown 不得被当作 failed 重试")
		require.Zero(t, rig.store.beginCalls)
		require.Zero(t, extractor.calls)
	})
	t.Run("verified-unknown-unblocks", func(t *testing.T) {
		rig := newLearningTestRig(t, true, []string{"demo"}, nil)
		rig.store.unresolved = &core.LearningExtraction{
			ExtractionID: "lrn_verified", Agent: "demo", SessionID: "sess_1",
			Status: core.LearningExtractionUnknown, Attempts: 1,
			VerifiedAt: learningTestNow.Add(-time.Minute),
		}
		extractor := &learningFakeExtractor{reply: `{"items":[]}`}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_2", Status: core.StatusCompleted})
		require.Equal(t, 1, rig.messages.extractionCalls, "查证后改用新窗口")
		require.Zero(t, rig.messages.frozenCalls)
		require.Equal(t, 1, rig.store.beginCalls)
		require.Equal(t, 1, extractor.calls)
	})
}

// TestUS22_Budget_ExhaustedNoCallNoRow 守 C-20：预算耗尽时存储层回
// `ErrLearningBudgetExhausted`——不建行、不调用、不写失败，记带 limit / window 的 WARN。
// 预算口径是代码常量：每 Agent 滚动 24h 200 次。
// An exhausted budget stops before any model call and writes no row.
func TestUS22_Budget_ExhaustedNoCallNoRow(t *testing.T) {
	t.Parallel()
	logger, buf := learningCapturedLogger()
	rig := newLearningTestRig(t, true, []string{"demo"}, logger)
	rig.store.beginErr = core.ErrLearningBudgetExhausted
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.store.beginCalls, "预算判定在 BeginExtraction 事务内")
	require.Zero(t, extractor.calls)
	require.Zero(t, rig.store.commitCalls)
	require.Zero(t, rig.store.failCalls, "耗尽不是失败终态：不写行")

	logs := buf.String()
	require.Contains(t, logs, "尝试次数上限已耗尽")
	require.Contains(t, logs, "limit=200", "WARN 带尝试上限")
	require.Contains(t, logs, "window=24h0m0s", "WARN 带滚动窗口长度")
}

// TestUS22_Budget_RollingWindow 守 C-20：预算常量是每 Agent 滚动 24h 200 次；判定对
// **重试**同样生效——重试也要再进一次预算事务，耗尽同样零调用。
// The rolling 24h / 200-attempt budget also gates retries.
func TestUS22_Budget_RollingWindow(t *testing.T) {
	t.Parallel()
	limits := DefaultLearningLimits()
	require.Equal(t, 200, limits.AttemptsPerAgent)
	require.Equal(t, 24*time.Hour, limits.BudgetWindow)

	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.unresolved = failedRow(1)
	rig.store.beginErr = core.ErrLearningBudgetExhausted
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_2", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.messages.frozenCalls)
	require.Equal(t, 1, rig.store.beginCalls, "重试同样要过预算事务")
	require.Zero(t, extractor.calls, "重试遇预算耗尽同样不发调用")
	require.Zero(t, rig.store.failCalls, "不写失败行（预算耗尽不是这次尝试的失败）")
	require.Equal(t, "demo", rig.store.beginRow.Agent, "预算按 Agent 计")
}

// TestUS22_Budget_CountsAttempts 守 C-20：重试把已记录的尝试数带进事务，由存储在同一
// 事务内递增——服务不得把计数重置为 0，否则上限永远达不到。
// A retry carries the recorded attempt count into the budget transaction.
func TestUS22_Budget_CountsAttempts(t *testing.T) {
	t.Parallel()
	t.Run("retry-carries-attempts", func(t *testing.T) {
		rig := newLearningTestRig(t, true, []string{"demo"}, nil)
		rig.store.unresolved = failedRow(2)
		extractor := &learningFakeExtractor{reply: `{"items":[]}`}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_2", Status: core.StatusCompleted})
		require.Equal(t, 2, rig.store.beginRow.Attempts)
	})
	t.Run("fresh-run-starts-at-zero", func(t *testing.T) {
		rig := newLearningTestRig(t, true, []string{"demo"}, nil)
		extractor := &learningFakeExtractor{reply: `{"items":[]}`}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
		require.Zero(t, rig.store.beginRow.Attempts)
	})
}

// TestUS22_Cursor_LeftBoundPassed 守 FR-22-03：首次尝试的游标左界原样进消息端口。
// The cursor's committed message id is passed verbatim.
func TestUS22_Cursor_LeftBoundPassed(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.cursor = core.LearningCursor{CommittedMessageID: 31}
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	require.Equal(t, int64(31), rig.messages.afterMessageID)
}

// TestUS22_Cursor_ReadErrorNoop 守 FR-22-03：游标读取失败时不猜左界，直接放弃本次抽取，
// 记 WARN，不建行、不调用。
// A cursor read failure aborts the extraction instead of guessing the left bound.
func TestUS22_Cursor_ReadErrorNoop(t *testing.T) {
	t.Parallel()
	logger, buf := learningCapturedLogger()
	rig := newLearningTestRig(t, true, []string{"demo"}, logger)
	rig.store.cursorErr = errTestCursor
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Zero(t, rig.messages.extractionCalls, "读不到游标不得退回全量窗口")
	require.Zero(t, extractor.calls)
	require.Zero(t, rig.store.beginCalls)
	require.Zero(t, rig.store.failCalls)
	require.Contains(t, buf.String(), "读取游标失败")
}

// TestUS22_Cursor_AdvanceOnSuccess 守 C-13：只有成功提交推进游标——服务在成功路径上
// 只调一次 CommitExtraction，不写失败行；提交回执带上本次抽取与完成时刻。
// Success advances the cursor through exactly one commit and no failure write.
func TestUS22_Cursor_AdvanceOnSuccess(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.store.commitCalls)
	require.Zero(t, rig.store.failCalls, "成功路径不得写失败终态")
	require.Equal(t, rig.store.beginRow.ExtractionID, rig.store.commitReq.ExtractionID)
	require.True(t, rig.store.commitReq.CompletedAt.Equal(learningTestNow))
}

// TestUS22_Cursor_NoAdvanceOnFailure 守 C-13：失败不推进游标——模型错误后只写一次失败
// 终态，不提交、不在钩内重试。
// A failure writes one terminal failure row and never commits.
func TestUS22_Cursor_NoAdvanceOnFailure(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{err: errors.New("upstream 500")}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, extractor.calls, "每次尝试只调用一次")
	require.Equal(t, 1, rig.store.failCalls)
	require.Zero(t, rig.store.commitCalls, "失败不提交、不推进游标")
}

// TestUS22_Cursor_ThreeFailuresSkip 守 C-13：第 3 次失败由存储置 skipped 并推进游标；
// 服务侧只写到一次失败终态，仍用冻结窗口，不提交。
// The third failure is written once; the store turns it into skipped and advances the cursor.
func TestUS22_Cursor_ThreeFailuresSkip(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.unresolved = failedRow(2) // 本次是第 3 次尝试
	extractor := &learningFakeExtractor{err: errors.New("upstream 500")}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_2", Status: core.StatusCompleted})

	require.Equal(t, 2, rig.store.beginRow.Attempts, "已有 2 次尝试原样带进事务")
	require.Equal(t, 1, rig.messages.frozenCalls)
	require.Zero(t, rig.messages.extractionCalls)
	require.Equal(t, 1, rig.store.failCalls)
	require.Zero(t, rig.store.commitCalls)
}

// TestUS22_Model_ErrorCountsTowardSkip 守失败矩阵：模型网络 / 供应商错误归
// `model_error` 并计入尝试数；服务不在钩内自行重试。
// A provider error is classified model_error and counted once.
func TestUS22_Model_ErrorCountsTowardSkip(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{err: errors.New("provider: connection reset")}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, core.LearningFailureModel, rig.store.failClass)
	require.Equal(t, 1, extractor.calls, "每次尝试只调用一次，不在钩内重试")
	require.Equal(t, 1, rig.store.failCalls)
	require.Zero(t, rig.store.commitCalls)
}

// TestUS22_Failure_AuditUnavailable 守失败矩阵 / B-14：审计预写失败归
// `audit_unavailable`，装配缺陷归 `extractor_unavailable`——不得一律归 `model_error`。
// Audit failures and assembly defects have their own failure classes.
func TestUS22_Failure_AuditUnavailable(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err   error
		class core.LearningFailureClass
	}{
		"audit": {
			err:   fmt.Errorf("memory_extract audit prewrite: %w", core.ErrAuditWrite),
			class: core.LearningFailureAudit,
		},
		"unavailable": {
			err:   fmt.Errorf("wrapper: %w", core.ErrLearningExtractorUnavailable),
			class: core.LearningFailureUnavailable,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newLearningTestRig(t, true, []string{"demo"}, nil)
			extractor := &learningFakeExtractor{err: tc.err}
			rig.service.AfterRun(context.Background(), learningRunInput(extractor),
				core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
			require.Equal(t, 1, rig.store.failCalls)
			require.Equal(t, tc.class, rig.store.failClass)
			require.Zero(t, rig.store.commitCalls)
		})
	}
}

// TestUS22_Commit_AtomicOnFailure 守 C-14：提交事务失败时结果未知——服务**不得**写
// failed 覆盖可能已提交的 success，只记 ERROR 留 pending 交恢复查证，也不在钩内重试。
// A failed commit keeps the row pending: no failure write, no replay.
func TestUS22_Commit_AtomicOnFailure(t *testing.T) {
	t.Parallel()
	logger, buf := learningCapturedLogger()
	rig := newLearningTestRig(t, true, []string{"demo"}, logger)
	rig.store.commitErr = errors.New("commit failed")
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.store.commitCalls)
	require.Equal(t, 1, extractor.calls, "提交失败不得重发模型调用")
	require.Equal(t, 1, rig.store.beginCalls)
	require.Zero(t, rig.store.failCalls, "结果未知不得写 failed 覆盖可能已提交的 success")
	require.Contains(t, buf.String(), "提交结果未知")
}

// TestUS22_Commit_UnknownKeepsPending 守 C-14 / B-08：提交结果未知后不重启，同来源
// 下一 Run 被 pending 行阻断——零写入、零调用、无第二条抽取行。
// After an unknown commit, a pending row blocks the next same-origin run without a replay.
func TestUS22_Commit_UnknownKeepsPending(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	rig.store.commitErr = errors.New("commit failed")
	first := &learningFakeExtractor{reply: `{"items":[]}`}
	rig.service.AfterRun(context.Background(), learningRunInput(first),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	require.Zero(t, rig.store.failCalls, "未知结果保持 pending")

	// 存储层随后把该行如实报告为 pending（提交可能已成功也可能没有）。
	rig.store.unresolved = &core.LearningExtraction{
		ExtractionID: "lrn_unknown", Agent: "demo", SessionID: "sess_1",
		Status: core.LearningExtractionPending, Attempts: 1,
	}
	second := &learningFakeExtractor{reply: `{"items":[]}`}
	blind := learningRunInput(second)
	blind.RunID = "run_2"
	rig.service.AfterRun(context.Background(), blind,
		core.RunResult{RunID: "run_2", Status: core.StatusCompleted})

	require.Zero(t, second.calls, "未查证前不得重放")
	require.Equal(t, 1, rig.store.beginCalls, "无第二条抽取行")
	require.Equal(t, 1, rig.store.commitCalls, "第二次不得提交")
	require.Zero(t, rig.messages.frozenCalls, "pending 阻断发生在读窗口之前")
}

// TestUS22_Commit_DedupeUsesDatabase 守 C-14 / §2.3.4：去重以提交事务内的数据库判定
// 为准——快照可能被 8 KiB 截断，服务不得据快照自行丢弃，也不得自己写记忆条目；重复项
// 交给 CommitExtraction，数据库丢弃后服务仍按成功处理（不写失败行）。
// Dedupe is decided inside the commit transaction, never by the truncated snapshot.
func TestUS22_Commit_DedupeUsesDatabase(t *testing.T) {
	t.Parallel()
	const duplicateText = "季度报表在每月 5 号前提交"
	store := &dedupeLearningStore{dbContents: map[string]struct{}{duplicateText: {}}}
	service := NewLearningService(LearningDeps{
		Store:     store,
		Messages:  &learningFakeMessages{window: learningWindowFixture()},
		Enabled:   true,
		Agents:    []string{"demo"},
		DraftRoot: filepath.Join(t.TempDir(), "drafts"),
		Now:       func() time.Time { return learningTestNow },
	})
	extractor := &learningFakeExtractor{reply: `{"items":[
		{"kind":"fact","content":"季度报表在每月 5 号前提交","evidence":[{"message_id":10,"role":"user","quote":"我习惯用中文写日报"}]}
	]}`}

	service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, store.commitCalls)
	require.Len(t, store.commitReq.Candidates, 1,
		"快照里没有这条：是否重复必须交给提交事务判定")
	require.Empty(t, store.kept, "数据库按 content_hash 丢弃重复项")
	require.Zero(t, store.failCalls, "数据库去重不是本次抽取失败")
}

// dedupeLearningStore 在记账桩之上模拟提交事务的 DB 去重：与库里已有正文相同的候选
// 不进入 kept，其余照常返回。
// dedupeLearningStore simulates the transaction-side content_hash dedupe.
type dedupeLearningStore struct {
	learningFakeStore
	dbContents map[string]struct{}
	kept       []core.LearningCandidate
}

func (s *dedupeLearningStore) CommitExtraction(ctx context.Context,
	req core.CommitLearningExtraction,
) (core.LearningCommitResult, error) {
	result, err := s.learningFakeStore.CommitExtraction(ctx, req)
	for _, candidate := range req.Candidates {
		if _, duplicate := s.dbContents[candidate.Content]; duplicate {
			continue
		}
		s.kept = append(s.kept, candidate)
	}
	return result, err
}

// TestUS22_Extraction_RunUnique 守 FR-22-06：`memory_extractions.run_id` 唯一——同一 Run
// 的第二次钩子调用在预写处撞唯一约束，服务不得发出模型调用，也不得写失败行。
// A duplicate run_id at the prewrite aborts before any model call or failure row.
func TestUS22_Extraction_RunUnique(t *testing.T) {
	t.Parallel()
	logger, buf := learningCapturedLogger()
	rig := newLearningTestRig(t, true, []string{"demo"}, logger)
	rig.store.beginErr = errors.New(`duplicate key value violates unique constraint "memory_extractions_run_id_key"`)
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.store.beginCalls)
	require.Zero(t, extractor.calls, "预写失败不得发出调用")
	require.Zero(t, rig.store.failCalls, "唯一冲突不是本次抽取的失败终态")
	require.Zero(t, rig.store.commitCalls)
	require.Contains(t, buf.String(), "预写抽取行失败")
}

// TestUS22_Commit_OriginFromRunRow 守 FR-22-07 / C-13：来源身份由 BeginExtraction 从
// `runs` 行复制——服务构造的行不得自带来源，也不得从当前 Principal 取值。
// The service leaves the origin empty so the store copies it from the runs row.
func TestUS22_Commit_OriginFromRunRow(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: `{"items":[]}`}

	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	row := rig.store.beginRow
	require.Empty(t, row.OriginCallerID, "来源由存储从事务外的 runs 行复制")
	require.Empty(t, row.OriginKeyID, "空串在存储层表示本地管理员，服务不得自行填值")
	require.Equal(t, "run_1", row.RunID, "触发 Run 是来源复制的锚点")
}

// TestUS22_Logs_FailureWarn 守 C-30：失败既落库也记 WARN，日志带抽取身份与分类 /
// 状态，window_gone 的日志状态与落库终态一致（skipped）。
// Failure paths emit WARN with identity, class and terminal status.
func TestUS22_Logs_FailureWarn(t *testing.T) {
	t.Parallel()
	t.Run("model-failure-warn", func(t *testing.T) {
		logger, buf := learningCapturedLogger()
		rig := newLearningTestRig(t, true, []string{"demo"}, logger)
		extractor := &learningFakeExtractor{err: errors.New("upstream 500")}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
		logs := buf.String()
		require.Contains(t, logs, "level=WARN")
		require.Contains(t, logs, "学习抽取失败")
		require.Contains(t, logs, "extraction_id=")
		require.Contains(t, logs, "agent=demo")
		require.Contains(t, logs, "failure_class=model_error")
		require.Contains(t, logs, "status=failed")
	})
	t.Run("window-gone-logs-skipped", func(t *testing.T) {
		logger, buf := learningCapturedLogger()
		rig := newLearningTestRig(t, true, []string{"demo"}, logger)
		rig.store.unresolved = failedRow(1)
		rig.messages.window = core.LearningWindow{}
		extractor := &learningFakeExtractor{reply: `{"items":[]}`}
		rig.service.AfterRun(context.Background(), learningRunInput(extractor),
			core.RunResult{RunID: "run_2", Status: core.StatusCompleted})
		logs := buf.String()
		require.Contains(t, logs, "failure_class=window_gone")
		require.Contains(t, logs, "status=skipped", "日志状态与落库终态一致")
	})
}

// TestUS22_Concurrent_SameAgentSerialized 守 C-20 的并发面（memory-race）：同一 Agent
// 的多个 Run 并发调用钩子时，服务不得持有跨 Run 的可变状态——每条账本行、每次提交都
// 只属于自己那次 Run（`-race` 另行判卷数据竞争）；同 Agent 的事务串行由存储层的
// advisory lock 负责（PG 用例 TestUS22_Store_CommitAdvisoryLock）。
// Concurrent same-agent hooks keep per-run identity intact; -race guards shared state.
func TestUS22_Concurrent_SameAgentSerialized(t *testing.T) {
	t.Parallel()
	const workers = 8
	store := &serialLearningStore{}
	service := NewLearningService(LearningDeps{
		Store:     store,
		Messages:  perRunMessages{},
		Enabled:   true,
		Agents:    []string{"demo"},
		DraftRoot: filepath.Join(t.TempDir(), "drafts"),
		Now:       func() time.Time { return learningTestNow },
	})

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := fmt.Sprintf("run_%d", i)
			messageID := int64(10 + i)
			extractor := &learningFakeExtractor{reply: fmt.Sprintf(
				`{"items":[{"kind":"fact","content":"事实%d","evidence":[{"message_id":%d,"role":"user","quote":"证据%d"}]}]}`,
				i, messageID, i)}
			service.AfterRun(context.Background(), core.LearningRunInput{
				RunID: runID, Agent: "demo", SessionID: fmt.Sprintf("sess_%d", i),
				Channel: "web", Provider: "local", Model: "model-a", ConfigHash: "cfg-1",
				CompletedAt: learningTestNow, Extractor: extractor,
			}, core.RunResult{RunID: runID, Status: core.StatusCompleted})
		}(i)
	}
	wg.Wait()

	require.Len(t, store.rows, workers)
	require.Len(t, store.commits, workers)
	require.Empty(t, store.fails)
	runByExtraction := make(map[string]string, workers)
	for _, row := range store.rows {
		require.Equal(t, "demo", row.Agent)
		require.NotEmpty(t, row.ExtractionID)
		_, duplicate := runByExtraction[row.ExtractionID]
		require.False(t, duplicate, "并发调用不得共用抽取 ID")
		runByExtraction[row.ExtractionID] = row.RunID
	}
	for _, commit := range store.commits {
		runID, ok := runByExtraction[commit.ExtractionID]
		require.True(t, ok)
		require.Len(t, commit.Candidates, 1)
		require.Equal(t, "事实"+strings.TrimPrefix(runID, "run_"), commit.Candidates[0].Content,
			"提交内容必须来自本 Run 的模型响应，不得串线")
	}
}

// serialLearningStore 是并发用例的线程安全记账桩。
// serialLearningStore is the mutex-guarded recorder for the concurrency case.
type serialLearningStore struct {
	mu      sync.Mutex
	rows    []core.LearningExtraction
	fails   []core.LearningFailureClass
	commits []core.CommitLearningExtraction
}

func (s *serialLearningStore) UnresolvedForOrigin(context.Context, string, string, string) (*core.LearningExtraction, error) {
	return nil, nil
}

func (s *serialLearningStore) LoadCursor(context.Context, string, string, string) (core.LearningCursor, bool, error) {
	return core.LearningCursor{}, false, nil
}

func (s *serialLearningStore) EntriesForExtraction(context.Context, core.Scope, int, int) ([]LearningEntrySnapshot, error) {
	return nil, nil
}

func (s *serialLearningStore) BeginExtraction(_ context.Context, row core.LearningExtraction) (core.LearningExtraction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, row)
	row.Attempts++
	return row, nil
}

func (s *serialLearningStore) FailExtraction(_ context.Context, _ string,
	class core.LearningFailureClass, _ string, _ time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails = append(s.fails, class)
	return nil
}

func (s *serialLearningStore) CommitExtraction(_ context.Context,
	req core.CommitLearningExtraction,
) (core.LearningCommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits = append(s.commits, req)
	return core.LearningCommitResult{}, nil
}

// perRunMessages 按触发 Run 生成各自的一条消息窗口（无共享可变状态）。
// perRunMessages derives a per-run one-message window without shared state.
type perRunMessages struct{}

func (perRunMessages) MessagesForExtraction(_ context.Context, _, runID string, _ int64,
	_, _, _, _ int,
) (core.LearningWindow, error) {
	index := strings.TrimPrefix(runID, "run_")
	var i int
	_, _ = fmt.Sscanf(index, "%d", &i)
	messageID := int64(10 + i)
	return core.LearningWindow{
		FromMessageID: messageID,
		ToMessageID:   messageID,
		New: []core.LearningMessage{{
			MessageID: messageID, RunID: runID, Role: "user",
			Content: fmt.Sprintf("证据%d", i), CreatedAt: learningTestNow,
		}},
	}, nil
}

func (perRunMessages) MessagesForFrozenWindow(context.Context, string, string, string, int64, int64, int, int) (core.LearningWindow, error) {
	return core.LearningWindow{}, nil
}
