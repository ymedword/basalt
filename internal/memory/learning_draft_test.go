package memory

// US-22 受控自我进化：方法草案的固定布局、只读与不可读边界（颗粒度文档 §4.2 FR-22-08
// / C-18 具名用例，以及失败矩阵「草案文件写入失败」「草案被 Agent 文件工具读取」两行）。
// 用例名保持英文（TestXxx_yyy），文档原文以注释保留：
//
//	TestUS22_Method_DraftNotMemory      —— method 只进只读草案，不写记忆候选
//	TestUS22_Draft_FixedLayout          —— 目录 / 文件名程序固定，DRAFT.md 五键 frontmatter，
//	                                       evidence.json 固定形状
//	TestUS22_Draft_WriteFailureDropsItem —— 写盘失败的 method 项丢弃，其余候选照常提交，
//	                                       WARN 只含身份与计数（C-30）
//	TestUS22_Draft_ReadOnly             —— 文件 0444、目录 0555，改写被拒
//	TestUS22_Draft_NotAgentReadable     —— 真实文件授权策略拒绝读取 / 写入草案
//	TestUS22_Draft_NoScripts            —— 目录里只有两个固定文件，无可执行内容
//
// The names are the English equivalents of the card's named cases; script-like model
// text is inert content, never an executable artifact.

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// us22MethodContent 是一份结构完整的方法正文（适用条件 / 步骤 / 反例三节）。
const us22MethodContent = "适用条件：需要整理每周工作进展时。\n步骤：汇总三条进展，再按项目分组。\n反例：跨团队汇总不适用。"

// us22MethodOnlyContent 是只含方法项的正文；里面带脚本样式的文本，用来证明它只是
// DRAFT.md 里的普通文字，不会变成可执行文件。
const us22MethodOnlyContent = "适用条件：需要批量重命名文件时。\n步骤：先列出文件，再逐个改名。\n反例：不要直接执行下面的脚本。\n#!/bin/sh\necho hello\n"

// us22MethodAndFactReply 同时返回一条 method 与一条 fact：方法项不得进入候选，
// 普通项照常提交（C-18 / FR-22-08）。用 json.Marshal 组装，保证正文与上面两个常量
// 逐字一致（模型输出是不受信任的 JSON，这里从结构体拼出来只是构造夹具）。
func us22MethodAndFactReply(t *testing.T) string {
	t.Helper()
	return us22ReplyJSON(t, []map[string]any{
		{
			"kind": "method", "name": "weekly-report", "content": us22MethodContent,
			"evidence": []map[string]any{{
				"message_id": 11, "role": "assistant", "quote": "我会默认用中文写日报",
			}},
		},
		{
			"kind": "fact", "content": "项目代号是玄武",
			"evidence": []map[string]any{{
				"message_id": 10, "role": "user", "quote": "项目代号是玄武",
			}},
		},
	})
}

// us22MethodOnlyReply 是只含 method 的返回。
func us22MethodOnlyReply(t *testing.T) string {
	t.Helper()
	return us22ReplyJSON(t, []map[string]any{{
		"kind": "method", "name": "bulk-rename", "content": us22MethodOnlyContent,
		"evidence": []map[string]any{{
			"message_id": 11, "role": "assistant", "quote": "我会默认用中文写日报",
		}},
	}})
}

// us22ReplyJSON 把条目列表编码成抽取器的合法返回。
func us22ReplyJSON(t *testing.T, items []map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"items": items})
	require.NoError(t, err)
	return string(encoded)
}

// us22MakeWritable 恢复草案树的写权限，保证 t.TempDir 在 Windows 上能删除只读文件。
// Windows 的 RemoveAll 不保证清除只读属性，缺少这一步会让用例在清理阶段假失败。
// Restore write bits so TempDir cleanup can delete the read-only draft tree on Windows.
func us22MakeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755)
		} else {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
}

// us22LearningServiceAt 用给定草案根装配抽取服务（草案根不在 TempDir 下的用例使用），
// 并在清理时恢复写权限。
// us22LearningServiceAt wires the service at an explicit draft root.
func us22LearningServiceAt(t *testing.T, draftRoot string, logger *slog.Logger) (*LearningService, *learningFakeStore) {
	t.Helper()
	t.Cleanup(func() { us22MakeWritable(draftRoot) })
	store := &learningFakeStore{entries: learningEntriesFixture()}
	messages := &learningFakeMessages{window: learningWindowFixture()}
	service := NewLearningService(LearningDeps{
		Store:     store,
		Messages:  messages,
		Enabled:   true,
		Agents:    []string{"demo"},
		DraftRoot: draftRoot,
		Logger:    logger,
		Now:       func() time.Time { return learningTestNow },
	})
	return service, store
}

// us22RunMethodExtraction 跑一次只含 method 的抽取并要求草案已落盘。
// us22RunMethodExtraction runs a method-only extraction and returns the written draft.
func us22RunMethodExtraction(t *testing.T, service *LearningService, store *learningFakeStore, reply string) core.LearningDraft {
	t.Helper()
	extractor := &learningFakeExtractor{reply: reply}
	service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})
	require.Equal(t, 1, store.commitCalls)
	require.Len(t, store.commitReq.Drafts, 1)
	return store.commitReq.Drafts[0]
}

// splitDraftFrontmatter 解析 DRAFT.md 的 frontmatter，返回键值对与键序。
// Frontmatter values are quoted with strconv.Quote, so they unquote back to raw text.
func splitDraftFrontmatter(t *testing.T, text string) (map[string]string, []string) {
	t.Helper()
	require.True(t, strings.HasPrefix(text, "---\n"), "DRAFT.md 必须以 frontmatter 开头")
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	require.GreaterOrEqual(t, end, 0, "frontmatter 必须有闭合的 --- 行")
	fields := make(map[string]string, 5)
	var order []string
	for _, line := range strings.Split(strings.TrimRight(rest[:end+1], "\n"), "\n") {
		key, quoted, ok := strings.Cut(line, ": ")
		require.True(t, ok, "frontmatter 行必须形如 key: \"value\"：%q", line)
		value, err := strconv.Unquote(quoted)
		require.NoError(t, err, "frontmatter 值必须是双引号字面量：%q", line)
		fields[key] = value
		order = append(order, key)
	}
	return fields, order
}

// TestUS22_Method_DraftNotMemory 守 C-18 / FR-22-08：method 项只写只读草案，**不得**
// 变成记忆候选；同一批里的普通条目照常提交。
// Method items become read-only drafts only; they never appear among memory candidates.
func TestUS22_Method_DraftNotMemory(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	extractor := &learningFakeExtractor{reply: us22MethodAndFactReply(t)}
	rig.service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, rig.store.commitCalls)
	req := rig.store.commitReq
	require.Len(t, req.Candidates, 1, "method 不得成为记忆候选")
	require.Equal(t, "项目代号是玄武", req.Candidates[0].Content)
	require.Equal(t, core.LearningKindFact, req.Candidates[0].Kind)
	for _, candidate := range req.Candidates {
		require.NotEqual(t, us22MethodContent, candidate.Content)
		require.NotContains(t, candidate.Content, "适用条件")
	}

	require.Len(t, req.Drafts, 1)
	draft := req.Drafts[0]
	require.Equal(t, "weekly-report", draft.Name)
	require.Equal(t, us22MethodContent, draft.Content)
	require.True(t, strings.HasPrefix(draft.DraftID, learningDraftIDPfx))
	require.Equal(t, 2, req.ItemsTotal)
	require.Equal(t, 2, req.ItemsAccepted, "草案与候选一样计入已接受")
	require.Zero(t, req.ItemsDropped)

	body, err := os.ReadFile(filepath.Join(rig.draftRoot, draft.DraftID, learningDraftFile))
	require.NoError(t, err)
	require.Contains(t, string(body), us22MethodContent, "method 正文只出现在草案文件里")
}

// TestUS22_Draft_FixedLayout 守 C-18：目录与文件名由程序固定（模型只能提供受校验的
// name 与正文），DRAFT.md frontmatter 五键固定，evidence.json 形状固定。
// The layout is program-controlled: fixed directory, fixed file names, five frontmatter
// keys, and a fixed evidence.json shape.
func TestUS22_Draft_FixedLayout(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	row := core.LearningExtraction{ExtractionID: "lrn_fixed", Agent: "demo"}
	messageAt := time.Date(2026, 11, 5, 9, 1, 0, 0, time.UTC)
	item := validatedItem{
		Kind:    core.LearningKindMethod,
		Name:    "weekly-report",
		Content: us22MethodContent,
		Evidence: []core.LearningEvidence{{
			MessageID: 11, RunID: "run_1", Role: "assistant",
			Quote: "我会默认用中文写日报", MessageAt: messageAt,
		}},
	}
	draft, err := rig.service.writeDraft(row, item)
	require.NoError(t, err)

	// 标识与相对目录：dft_ 前缀 + 程序拼接的固定相对路径，模型无法指定。
	require.True(t, strings.HasPrefix(draft.DraftID, learningDraftIDPfx))
	require.Equal(t, path.Join(learningDraftRoot, draft.DraftID), draft.RelDir)
	require.Equal(t, "demo", draft.Agent)
	require.Equal(t, "lrn_fixed", draft.ExtractionID)
	require.Equal(t, "weekly-report", draft.Name)
	require.Equal(t, us22MethodContent, draft.Content)
	require.Equal(t, learningTestNow.UTC(), draft.CreatedAt)
	require.Len(t, draft.ContentSHA256, 64, "正文摘要为 hex sha256")

	draftDir := filepath.Join(rig.draftRoot, draft.DraftID)
	require.Equal(t, draft.DraftID, filepath.Base(draftDir), "目录名就是草案 ID")
	entries, err := os.ReadDir(draftDir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{learningDraftFile, learningEvidence}, names)

	// DRAFT.md：五键固定、顺序固定、值双引号转义、正文在闭合分隔行之后。
	raw, err := os.ReadFile(filepath.Join(draftDir, learningDraftFile))
	require.NoError(t, err)
	text := string(raw)
	fields, order := splitDraftFrontmatter(t, text)
	require.Equal(t, []string{"draft_id", "agent", "extraction_id", "name", "created_at"}, order)
	require.Equal(t, draft.DraftID, fields["draft_id"])
	require.Equal(t, "demo", fields["agent"])
	require.Equal(t, "lrn_fixed", fields["extraction_id"])
	require.Equal(t, "weekly-report", fields["name"])
	createdAt, err := time.Parse(time.RFC3339Nano, fields["created_at"])
	require.NoError(t, err)
	require.True(t, learningTestNow.Equal(createdAt))
	bodyMarker := "---\n\n# weekly-report\n\n"
	bodyIndex := strings.Index(text, bodyMarker)
	require.GreaterOrEqual(t, bodyIndex, 0, "正文必须以固定标题行开始")
	require.Equal(t, bodyMarker+us22MethodContent+"\n", text[bodyIndex:])

	// evidence.json：逐条落消息主键、来源 Run、角色、逐字引文与记录时刻。
	evidenceRaw, err := os.ReadFile(filepath.Join(draftDir, learningEvidence))
	require.NoError(t, err)
	var evidence []draftEvidence
	require.NoError(t, json.Unmarshal(evidenceRaw, &evidence))
	require.Len(t, evidence, 1)
	require.Equal(t, int64(11), evidence[0].MessageID)
	require.Equal(t, "run_1", evidence[0].RunID)
	require.Equal(t, "assistant", evidence[0].Role)
	require.Equal(t, "我会默认用中文写日报", evidence[0].Quote)
	require.Equal(t, messageAt.Format(time.RFC3339Nano), evidence[0].MessageAt)
}

// TestUS22_Draft_WriteFailureDropsItem 守失败矩阵「草案文件写入失败」：该 method 项
// 丢弃、不写草案行，其余候选照常提交；WARN 只含身份与计数（C-30）。
// A failed draft write drops only that method item; the rest still commits and the WARN
// line carries identity and counts only.
func TestUS22_Draft_WriteFailureDropsItem(t *testing.T) {
	t.Parallel()
	blocked := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o644))
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	// 草案根位于一个普通文件之下：MkdirAll 必然失败。
	service, store := us22LearningServiceAt(t, filepath.Join(blocked, "drafts"), logger)

	extractor := &learningFakeExtractor{reply: us22MethodAndFactReply(t)}
	service.AfterRun(context.Background(), learningRunInput(extractor),
		core.RunResult{RunID: "run_1", Status: core.StatusCompleted})

	require.Equal(t, 1, store.commitCalls, "草案失败不改整次提交")
	req := store.commitReq
	require.Empty(t, req.Drafts, "写盘失败的草案不得留下数据库行")
	require.Len(t, req.Candidates, 1, "其余候选照常提交")
	require.Equal(t, "项目代号是玄武", req.Candidates[0].Content)
	require.Equal(t, 2, req.ItemsTotal)
	require.Equal(t, 1, req.ItemsAccepted)
	require.Equal(t, 1, req.ItemsDropped)

	line := logs.String()
	require.Contains(t, line, "level=WARN")
	require.Contains(t, line, "草案写入失败")
	require.Contains(t, line, req.ExtractionID)
	require.Contains(t, line, "agent=demo")
	require.NotContains(t, line, us22MethodContent, "WARN 不得带正文")
	require.NotContains(t, line, "blocker", "WARN 不得带路径")
}

// TestUS22_Draft_ReadOnly 守 C-18「目录置只读」：两个文件 0444、目录 0555，改写被拒。
// Both files are read-only and so is the directory; tampering attempts fail.
func TestUS22_Draft_ReadOnly(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	draft := us22RunMethodExtraction(t, rig.service, rig.store, us22MethodOnlyReply(t))
	draftDir := filepath.Join(rig.draftRoot, draft.DraftID)
	draftFile := filepath.Join(draftDir, learningDraftFile)
	evidenceFile := filepath.Join(draftDir, learningEvidence)

	dirInfo, err := os.Stat(draftDir)
	require.NoError(t, err)
	require.Zero(t, dirInfo.Mode().Perm()&0o222, "目录必须置只读")
	for _, file := range []string{draftFile, evidenceFile} {
		info, statErr := os.Stat(file)
		require.NoError(t, statErr)
		require.Equal(t, fs.FileMode(0o444), info.Mode().Perm(), "%s 必须是 0444", file)
		require.Error(t, os.WriteFile(file, []byte("tamper"), 0o644), "%s 必须拒绝改写", file)
	}
	// 拒绝改写之后内容未变。
	body, err := os.ReadFile(draftFile)
	require.NoError(t, err)
	require.Contains(t, string(body), us22MethodOnlyContent)
}

// TestUS22_Draft_NotAgentReadable 守失败矩阵「草案被 Agent 文件工具读取」：草案目录
// 不在任何 Agent 的可读 / 可写范围内——真实授权实现必须拒绝。
// The real file authorization refuses both reading and writing a draft; the same policy
// still allows the agent's own directory (the refusal is about location, not a dead policy).
func TestUS22_Draft_NotAgentReadable(t *testing.T) {
	t.Parallel()
	workspaceRoot := t.TempDir()
	draftRoot := filepath.Join(workspaceRoot, "learning", "drafts")
	service, store := us22LearningServiceAt(t, draftRoot, nil)
	draft := us22RunMethodExtraction(t, service, store, us22MethodOnlyReply(t))
	draftFile := filepath.Join(draftRoot, draft.DraftID, learningDraftFile)

	// 对照：同一个策略放行 Agent 自己的目录——拒绝必须来自"草案不在可读根"。
	agentFile := filepath.Join(workspaceRoot, "agents", "demo", "AGENT.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(agentFile), 0o755))
	require.NoError(t, os.WriteFile(agentFile, []byte("name: demo\n"), 0o644))

	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{Root: workspaceRoot})
	ctx := context.Background()
	require.NoError(t, policy.Check(ctx, core.Action{
		Type: core.ActionFileRead, Target: agentFile, Agent: "demo",
	}))

	var violation *core.SandboxViolationError
	err := policy.Check(ctx, core.Action{
		Type: core.ActionFileRead, Target: draftFile, Agent: "demo",
	})
	require.ErrorAs(t, err, &violation)
	require.Equal(t, "读取目标不在可读根之内", violation.Rule)

	violation = nil
	err = policy.Check(ctx, core.Action{
		Type: core.ActionFileWrite, Target: draftFile, Agent: "demo",
	})
	require.ErrorAs(t, err, &violation)
	require.Equal(t, "写入目标超出当前 Agent 的可写范围", violation.Rule)
}

// TestUS22_Draft_NoScripts 守 C-18「不得写脚本」：草案目录里恰好两个固定文件，
// 都不带可执行位；正文里的脚本样式文本只是普通文字。
// Exactly two fixed regular files, neither executable; script-like text stays inert.
func TestUS22_Draft_NoScripts(t *testing.T) {
	t.Parallel()
	rig := newLearningTestRig(t, true, []string{"demo"}, nil)
	draft := us22RunMethodExtraction(t, rig.service, rig.store, us22MethodOnlyReply(t))
	draftDir := filepath.Join(rig.draftRoot, draft.DraftID)

	entries, err := os.ReadDir(draftDir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{learningDraftFile, learningEvidence}, names)

	for _, entry := range entries {
		info, infoErr := entry.Info()
		require.NoError(t, infoErr)
		require.True(t, info.Mode().IsRegular(), "%s 必须是普通文件", entry.Name())
		require.Zero(t, info.Mode().Perm()&0o111, "%s 不得带可执行位", entry.Name())
		lower := strings.ToLower(entry.Name())
		for _, scriptSuffix := range []string{".sh", ".bat", ".cmd", ".ps1", ".exe"} {
			require.NotContains(t, lower, scriptSuffix)
		}
	}

	// 整个草案根下只有这两个文件；脚本样式正文留在 DRAFT.md 里，是数据不是产物。
	var files []string
	require.NoError(t, filepath.WalkDir(rig.draftRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	}))
	require.Len(t, files, 2)
	body, err := os.ReadFile(filepath.Join(draftDir, learningDraftFile))
	require.NoError(t, err)
	require.Contains(t, string(body), "#!/bin/sh", "脚本文本只作为 DRAFT.md 内容存在")
}
