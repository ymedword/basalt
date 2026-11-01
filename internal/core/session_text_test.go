package core_test

// 本文件是「消息正文可存储性」的判卷面（US-16 人工走查发现，2026-10-31）。
//
// 事实：外部工具结果（抓回的网页正文、MCP server 回传的文本）可能带 NUL（0x00）
// 或非法 UTF-8 字节；PostgreSQL 的 `text` 列两者都不能存——NUL 在任何编码下都非法，
// 非法 UTF-8 报 `SQLSTATE 22021`。修复前这条坏字节让 `AppendMessage` 失败，整轮
// 运行记 `failed`，连半截工具组的补齐也在同一处再失败一次（`半截工具组补齐失败`）。
//
// 判据：**追加路径必须先把坏字节换成 U+FFFD，再照常落库**，内存历史与落库正文是
// 同一份文本（单一追加路径），日志只记规模、绝不带正文。
// These tests pin the storage-safety wash on the single append path. The fake store
// below rejects exactly what PostgreSQL rejects, so its red state is the production
// failure mode (SQLSTATE 22021) rather than a made-up one.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// pgTextStore 按 PostgreSQL `text` 列的判卷复刻会话仓储的失败面：含 NUL 或非法
// UTF-8 的正文一律拒写（与真库同一条 `SQLSTATE 22021`），且**不落行**——这样
// 「正文含坏字节」的判据在没有数据库的单元测试里得到与真库一致的读数。
// pgTextStore mirrors the PostgreSQL text-column rejection; a rejected append writes
// no row, exactly like the real store.
type pgTextStore struct{ sessionStore }

// AppendMessage 先按真库规则判卷，通过后交给记录型替身。
// AppendMessage rejects unstorable text before delegating to the recording fake.
func (s *pgTextStore) AppendMessage(
	ctx context.Context, sessionID, runID string, m core.Message,
) (string, error) {
	if strings.Contains(m.Content, "\x00") {
		return "", errors.New(
			`ERROR: invalid byte sequence for encoding "UTF8": 0x00 (SQLSTATE 22021)`)
	}
	if !utf8.ValidString(m.Content) {
		return "", errors.New(
			`ERROR: invalid byte sequence for encoding "UTF8": 0xff (SQLSTATE 22021)`)
	}
	if !utf8.ValidString(m.ReasoningContent) || strings.Contains(m.ReasoningContent, "\x00") {
		return "", errors.New(
			`ERROR: invalid byte sequence for encoding "UTF8" (SQLSTATE 22021)`)
	}
	return s.sessionStore.AppendMessage(ctx, sessionID, runID, m)
}

// captureLogs 把默认 logger 换成字符串缓冲，返回读取句柄；用例结束恢复原值。
// captureLogs swaps the default logger for a buffer and restores it on cleanup.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var buf strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf.String
}

// 中文验收点：外部工具结果里的 NUL 与非法 UTF-8 **不得**让追加失败——真库会以
// `SQLSTATE 22021` 拒写，追加路径必须换成 U+FFFD 后照常落库；内存历史与落库正文
// 一致；推理正文同口径；WARN 只记替换规模与定位字段，不带正文。
//
// 逆改点：去掉 `AppendMessage` 里的清洗 → 本用例拿到 pgTextStore 的 22021 错误，
// `require.NoError` 逐字变红（与 2026-10-31 走查的失败文本同形）。
func TestSession_AppendMessageSanitizesUnstorableText(t *testing.T) {
	logs := captureLogs(t)
	store := &pgTextStore{}
	sess := core.NewSession("cli-3f9a1c07b2d84e55", store, nil)

	raw := "页面开头" + "\x00" + "中间" + "\xff\xfe" + "结尾"
	require.NoError(t, sess.AppendMessage(context.Background(), "run-1", core.Message{
		Role:             core.RoleTool,
		Content:          raw,
		ToolCallID:       "call-1",
		ReasoningContent: "思考\x00",
	}), "工具结果里的坏字节不得让追加失败（真库报 SQLSTATE 22021）")

	want := "页面开头�中间��结尾"
	require.Equal(t, want, store.lastContent(t), "落库正文：NUL 与每个非法字节各换一个 U+FFFD")
	require.Equal(t, want, sess.Messages()[0].Content, "内存历史与落库正文必须是同一份文本")
	require.Equal(t, "思考�", sess.Messages()[0].ReasoningContent, "推理正文同口径")

	logged := logs()
	require.Contains(t, logged, "消息正文含无法承载的字节")
	require.Contains(t, logged, "replaced_bytes=3", "正文换了 3 个字节（1 个 NUL + 2 个非法字节）")
	require.Contains(t, logged, "tool_call_id=call-1", "日志要能定位到是哪个工具调用")
	require.NotContains(t, logged, "页面开头", "日志只记规模，绝不带正文")
	require.NotContains(t, logged, "思考", "推理正文同样不得进日志")
}

// 中文验收点：无状态会话不落库、也不写审计，但内存历史要进模型提示词——坏字节
// 同样必须清洗（协议只接受 UTF-8 文本）。
// 逆改点：把清洗挂在 `persistent()` 分支内 → 本用例读到原始 NUL，断言红。
func TestSession_StatelessHistoryAlsoSanitized(t *testing.T) {
	_ = captureLogs(t) // WARN 已在上一用例判过，这里只吞掉输出
	sess := core.NewSession("", nil, nil)
	require.NoError(t, sess.AppendMessage(context.Background(), "run-2",
		core.Message{Role: core.RoleTool, Content: "前\x00后", ToolCallID: "c1"}))
	require.Equal(t, "前�后", sess.Messages()[0].Content)
}

// 中文验收点：崩溃 / 取消后的补齐路径同样走唯一追加出口——审计行里还原出来的
// 正文带 NUL 时，合成消息也必须清洗后落库（修复前这条路径以同一条 22021 失败，
// 日志留下 `半截工具组补齐失败`）。
// 逆改点：让 `AppendSynthesizedToolMessage` 绕过清洗（或清洗只挂在 AppendToolResult
// 上）→ 本用例拿到 22021，`require.NoError` 变红。
func TestSession_CompleteToolGroupSanitizesRestoredResult(t *testing.T) {
	_ = captureLogs(t) // 同上：本用例判行为，不判日志
	store := &pgTextStore{}
	runID := "cli-1776000000123-abc123"
	call := core.ToolCall{ID: "c1", Name: "fetch"}

	audit := &lookupAudit{rows: map[string]core.ToolInvocationView{
		runID + "|c1": {
			Status:     core.LLMCallSuccess,
			ResultJSON: json.RawMessage(`{"content":"正文\u0000尾部","retryable":false}`),
		},
	}}
	sess := core.NewSession("cli-3f9a1c07b2d84e55", store, audit)

	require.NoError(t, sess.CompleteToolGroup(context.Background(), runID,
		[]core.ToolCall{call}, core.ReasonCancelled), "补齐路径不得因正文坏字节失败")
	require.Equal(t, "正文�尾部", store.lastContent(t), "还原出的 NUL 同样换成 U+FFFD")
	require.Equal(t, 1, store.count())
}
