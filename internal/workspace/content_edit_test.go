package workspace

// 本文件是内容条件读写的验收测试（颗粒度文档 §3.2.1：workspace/content_edit_test.go，
// 22 例：读面 + ETag 字节定义 4 例、行级补丁 11 例、内容写 7 例）。
// 采用**同包白盒**：ETag 期望值由测试自己用 crypto/sha256 拼出，不调用被测实现；
// 序列化与回滚用本文件的假仓储 / 假调度器，失败接缝由 `rename` 注入。
// This file covers the conditional read/write surface (22 cases). White-box on
// purpose: expected ETags are built from crypto/sha256, and the failing seams are
// injected through the rename hook instead of platform-dependent disk behaviour.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// writeAgentFile 在 agents/<name>/AGENT.md 落一份原文（自动创建父目录）。
func writeAgentFile(t *testing.T, root, name string, raw []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(agentDefinitionPath(name)))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), dirPerm))
	require.NoError(t, os.WriteFile(full, raw, filePerm))
}

// TestContentETag_StrongQuotedVector：ETag = `"sha256:<hex>"`（含双引号），对原始
// 字节计算——空内容、BOM、CRLF 都各得其值，不做归一化。
func TestContentETag_StrongQuotedVector(t *testing.T) {
	cases := [][]byte{
		{},
		[]byte("---\nname: writer\n---\n\n正文\n"),
		[]byte("\ufeff---\r\nname: writer\r\n---\r\n\r\n正文\r\n"),
	}
	for _, raw := range cases {
		sum := sha256.Sum256(raw)
		expected := `"sha256:` + hex.EncodeToString(sum[:]) + `"`
		require.Equal(t, expected, ContentETag(raw))
	}

	require.NotEqual(t, ContentETag([]byte("正文\n")), ContentETag([]byte("正文\r\n")),
		"换行风格必须进哈希")
}

// TestAgentEditor_ReadContentReturnsRawBytesAndETag：原文逐字节返回（含 BOM 与 CRLF），
// ETag 与返回字节同源；非法 Agent 名在读取之前即拒绝。
func TestAgentEditor_ReadContentReturnsRawBytesAndETag(t *testing.T) {
	root := t.TempDir()
	raw := []byte("\ufeff---\r\nname: writer\r\n---\r\n\r\n正文\r\n")
	writeAgentFile(t, root, "writer", raw)

	editor := NewAgentEditor(AgentEditorDeps{Root: root})
	content, err := editor.ReadContent(context.Background(), "writer")
	require.NoError(t, err)
	require.Equal(t, string(raw), content.Content)
	require.Equal(t, ContentETag(raw), content.ETag)

	_, err = editor.ReadContent(context.Background(), "../agents/writer")
	require.ErrorContains(t, err, "路径分隔符")
}

// TestAgentEditor_ReadContentMissingAgentWrapsNotExist：Agent 目录或 AGENT.md 缺失时
// 必须包装 fs.ErrNotExist（HTTP 层据此映射 404），不得返回空内容冒充成功。
func TestAgentEditor_ReadContentMissingAgentWrapsNotExist(t *testing.T) {
	root := t.TempDir()
	editor := NewAgentEditor(AgentEditorDeps{Root: root})

	_, err := editor.ReadContent(context.Background(), "ghost")
	require.Error(t, err)
	require.True(t, errors.Is(err, fs.ErrNotExist), "缺失 Agent 必须包装 fs.ErrNotExist：%v", err)
}

// TestAgentEditor_ReadContentTakesSharedReadLock：读面取的是注入的同一把 AgentGate，
// 写锁持有期间读取必须等待——这正是"比较与写入在同一把锁内"的前提（约束 25）。
func TestAgentEditor_ReadContentTakesSharedReadLock(t *testing.T) {
	root := t.TempDir()
	writeAgentFile(t, root, "writer", []byte("---\nname: writer\n---\n\n正文\n"))

	gate := core.NewAgentGate()
	editor := NewAgentEditor(AgentEditorDeps{Root: root, Gate: gate})

	release := gate.Lock("writer")
	done := make(chan error, 1)
	go func() {
		_, err := editor.ReadContent(context.Background(), "writer")
		done <- err
	}()

	select {
	case err := <-done:
		release()
		t.Fatalf("写锁持有期间读取不得返回（err=%v）", err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("释放写锁后读取必须返回")
	}
}

// ---------------------------------------------------------------------------
// 测试夹具与假依赖
// ---------------------------------------------------------------------------

// editorContent 是内容写的主夹具：带注释、映射块、序列与一条定时，足以覆盖
// "只改目标行 / 回滚恢复旧调度 / 受管清单" 三类断言。
// editorContent is the main fixture: a comment, map and sequence blocks, one schedule.
const editorContent = `---
name: writer
description: "旧描述"
# provider 段前的注释必须保留
identity:
  agent_name: "周报助手"
  prompt: "你是周报助手。"
provider:
  name: "mock"
  model: "mock-model"
tools:
  - "file_read"
  - "file_write"
schedules:
  - id: "writer-1"
    cron: "0 0 9 * * 1"
    zone: "UTC"
    message: "生成本周周报"
---

你是周报助手。
`

// editorContentReplaced 是整份替换用例的目标内容：只改描述、模型、工具与任务文案。
// editorContentReplaced is the whole-file replacement target.
const editorContentReplaced = `---
name: writer
description: "新描述"
# provider 段前的注释必须保留
identity:
  agent_name: "周报助手"
  prompt: "你是周报助手。"
provider:
  name: "mock"
  model: "mock-pro"
tools:
  - "file_read"
schedules:
  - id: "writer-1"
    cron: "0 0 9 * * 1"
    zone: "UTC"
    message: "生成周报"
---

你是周报助手。
`

// testValidator 是装载期校验的测试替身：与生产注入点同签名，走同一份
// `parseFrontmatter`，并强制"条目已注册"。
// testValidator mirrors the injected load-time gate through parseFrontmatter.
func testValidator() func(context.Context, string, []byte) (core.AgentEntry, string, []core.ManifestEntry, error) {
	return func(_ context.Context, agent string, candidate []byte) (core.AgentEntry, string, []core.ManifestEntry, error) {
		profile, _, err := parseFrontmatter(candidate)
		if err != nil {
			return core.AgentEntry{}, "", nil, err
		}
		if profile.Name != agent {
			return core.AgentEntry{}, "", nil, fmt.Errorf("name %q 与目录 %q 不一致", profile.Name, agent)
		}
		_, body, err := splitFrontmatter(candidate)
		if err != nil {
			return core.AgentEntry{}, "", nil, err
		}
		return core.AgentEntry{Name: agent, Registered: true, Profile: profile}, string(body),
			[]core.ManifestEntry{{Path: agentDefinitionPath(agent), SHA256: sha256Hex(candidate)}}, nil
	}
}

// fakeOps 是管理操作仓储的测试替身：记录预写与终态，可注入预写失败。
// fakeOps records pre-writes and finishes; the pre-write can be made to fail.
type fakeOps struct {
	mu       sync.Mutex
	begun    []core.OperationRecord
	finished []fakeFinish
	beginErr error
}

// fakeFinish 是一次终态回填的入参快照。
type fakeFinish struct {
	operationID string
	status      core.OperationStatus
	resultCode  string
	message     string
	detail      []byte
}

func (f *fakeOps) BeginOperation(_ context.Context, rec core.OperationRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beginErr != nil {
		return f.beginErr
	}
	f.begun = append(f.begun, rec)
	return nil
}

func (f *fakeOps) FinishOperation(_ context.Context, operationID string, status core.OperationStatus, resultCode, errorMessage string, detailJSON []byte, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, fakeFinish{
		operationID: operationID, status: status, resultCode: resultCode,
		message: errorMessage, detail: append([]byte(nil), detailJSON...),
	})
	return nil
}

func (f *fakeOps) GetOperation(context.Context, string) (core.OperationRecord, error) {
	return core.OperationRecord{}, core.ErrOperationNotFound
}

func (f *fakeOps) ListOperations(context.Context, core.OperationFilter) ([]core.OperationRecord, error) {
	return nil, nil
}

func (f *fakeOps) PendingOperations(context.Context) ([]core.OperationRecord, error) {
	return nil, nil
}

func (f *fakeOps) RecoverOperation(context.Context, string, core.OperationStatus, string, []byte, time.Time) error {
	return errors.New("恢复路径不属内容写测试")
}

// fakeSnapshots 记录落库的配置快照，可注入保存失败。
// fakeSnapshots records saved snapshots and can fail on demand.
type fakeSnapshots struct {
	mu    sync.Mutex
	saved []core.ConfigSnapshot
	err   error
}

func (f *fakeSnapshots) SaveSnapshot(_ context.Context, snap *core.ConfigSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, *snap)
	return nil
}

// fakeRegistry 同时满足 EntryReplacer 与可选的 entryLookup（回滚取旧条目）。
// fakeRegistry satisfies both EntryReplacer and the optional entryLookup.
type fakeRegistry struct {
	mu      sync.Mutex
	entries map[string]core.AgentEntry
}

func newFakeRegistry() *fakeRegistry {
	return &fakeRegistry{entries: make(map[string]core.AgentEntry)}
}

func (r *fakeRegistry) Replace(name string, entry core.AgentEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[name] = entry
}

func (r *fakeRegistry) Lookup(name string) (core.AgentEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[name]
	return entry, ok
}

func (r *fakeRegistry) get(name string) (core.AgentEntry, bool) {
	return r.Lookup(name)
}

// fakeScheduler 记录每次按 Agent 同步的任务集，前 failures 次调用返回 err。
// fakeScheduler records reconciliations; the first `failures` calls return err.
type fakeScheduler struct {
	mu       sync.Mutex
	calls    [][]core.Schedule
	failures int
	err      error
}

func (s *fakeScheduler) ReconcileAgent(_ context.Context, _ string, schedules []core.Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, append([]core.Schedule(nil), schedules...))
	if s.failures > 0 {
		s.failures--
		return s.err
	}
	return nil
}

// editorFixture 组装一份落在临时工作区里的内容编辑器与全部假依赖。
// editorFixture wires an editor on a temp workspace with every fake dependency.
type editorFixture struct {
	root      string
	editor    *AgentEditor
	ops       *fakeOps
	snapshots *fakeSnapshots
	registry  *fakeRegistry
	scheduler *fakeScheduler
}

func newEditorFixture(t *testing.T, raw string) *editorFixture {
	t.Helper()
	root := t.TempDir()
	writeAgentFile(t, root, "writer", []byte(raw))
	fx := &editorFixture{
		root:      root,
		ops:       &fakeOps{},
		snapshots: &fakeSnapshots{},
		registry:  newFakeRegistry(),
		scheduler: &fakeScheduler{},
	}
	fx.editor = NewAgentEditor(AgentEditorDeps{
		Root:      root,
		Gate:      core.NewAgentGate(),
		Ops:       fx.ops,
		Snapshots: fx.snapshots,
		Registry:  fx.registry,
		Scheduler: fx.scheduler,
		Validate:  testValidator(),
		Now:       func() time.Time { return time.UnixMilli(1729500000123).UTC() },
	})
	return fx
}

// disk 读回磁盘上的 AGENT.md 原文。
func (fx *editorFixture) disk(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fx.root, filepath.FromSlash(agentDefinitionPath("writer"))))
	require.NoError(t, err)
	return raw
}

// dirEntries 列出 Agent 目录内的文件名（用于断言无 `.bak` / `.tmp-` 残留）。
func (fx *editorFixture) dirEntries(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(fx.root, filepath.FromSlash(agentsDirName), "writer")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// ---------------------------------------------------------------------------
// 行级补丁（约束 26）
// ---------------------------------------------------------------------------

// TestApplyChanges_ScalarEditsPreserveFormatting：标量只换值、映射叶子原位改写或
// 追加到块尾；注释、未改行与行尾风格逐字节保留，CRLF 文件插入的行也用 CRLF。
// Scalar edits rewrite only their own bytes and keep the comment and line endings.
func TestApplyChanges_ScalarEditsPreserveFormatting(t *testing.T) {
	model := "mock-pro"
	description := "新描述"
	changes := core.AgentContentChanges{
		Description: &description,
		Provider:    &core.AgentProviderChanges{Model: &model},
	}

	expected := strings.Replace(editorContent, `description: "旧描述"`, `description: "新描述"`, 1)
	expected = strings.Replace(expected, `  model: "mock-model"`, `  model: "mock-pro"`, 1)

	patched, err := ApplyChanges([]byte(editorContent), changes)
	require.NoError(t, err)
	require.Equal(t, expected, string(patched))

	// CRLF：未改行原样保留，改写的行沿用 CRLF（不得混入 LF）。
	crlf := strings.ReplaceAll(editorContent, "\n", "\r\n")
	patched, err = ApplyChanges([]byte(crlf), changes)
	require.NoError(t, err)
	require.Equal(t, strings.ReplaceAll(expected, "\n", "\r\n"), string(patched))
}

// TestApplyChanges_SequenceEditsReplaceTargetBlocksOnly：序列整块替换；空列表渲染为
// 流式 `[]`；块外的一切字节不动。
// Sequence edits replace exactly the target block; an empty list renders as [].
func TestApplyChanges_SequenceEditsReplaceTargetBlocksOnly(t *testing.T) {
	tools := []string{"file_read", "shell"}
	skills := []string{}
	patched, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{
		Tools: &tools, Skills: &skills,
	})
	require.NoError(t, err)

	expected := strings.Replace(editorContent, "tools:\n  - \"file_read\"\n  - \"file_write\"\n",
		"tools:\n  - \"file_read\"\n  - \"shell\"\n", 1)
	// 缺失的键统一插在收尾围栏之前（已存在的 schedules 块不动）。
	expected = strings.Replace(expected, "message: \"生成本周周报\"\n---\n",
		"message: \"生成本周周报\"\nskills: []\n---\n", 1)
	require.Equal(t, expected, string(patched))
}

// TestApplyChanges_InsertsMissingKeysAndChildren：缺失的顶层键与映射子键按契约顺序
// 插在收尾围栏之前；已有内容不受影响。
// Missing keys are inserted before the closing fence in canonical order.
func TestApplyChanges_InsertsMissingKeysAndChildren(t *testing.T) {
	raw := "---\nname: writer\n---\n\n正文。\n"
	description := "新描述"
	agentName := "周报助手"
	tools := []string{"file_read"}

	patched, err := ApplyChanges([]byte(raw), core.AgentContentChanges{
		Description: &description,
		Identity:    &core.AgentIdentityChanges{AgentName: &agentName},
		Tools:       &tools,
	})
	require.NoError(t, err)
	require.Equal(t, `---
name: writer
description: "新描述"
identity:
  agent_name: "周报助手"
tools:
  - "file_read"
---

正文。
`, string(patched))
}

// TestApplyChanges_SchedulesRewritePreservesOtherBytes：schedules 复用唯一写入器，
// 只重建该块；块前的注释与块外字节逐字节保留。
// Schedules reuse the single writer and leave every byte outside the block alone.
func TestApplyChanges_SchedulesRewritePreservesOtherBytes(t *testing.T) {
	schedules := []core.Schedule{{
		ID: "writer-1", Cron: "0 0 9 * * 1", Zone: "UTC", Message: "生成周报",
	}}
	patched, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{Schedules: &schedules})
	require.NoError(t, err)
	require.Equal(t, strings.Replace(editorContent, `message: "生成本周周报"`, `message: "生成周报"`, 1), string(patched))
}

// TestApplyChanges_RejectsBlockAndInlineComments：目标块内或目标值上的注释无法在
// 行级重写中保留 → EDIT_UNSUPPORTED（不得静默丢注释）。
// Comments in a target block or value refuse the patch instead of being dropped.
func TestApplyChanges_RejectsBlockAndInlineComments(t *testing.T) {
	agentName := "x"
	description := "x"
	cases := []struct {
		name string
		raw  string
		ch   core.AgentContentChanges
	}{
		{
			name: "identity 块内注释",
			raw:  "---\nname: writer\nidentity:\n  # 说明\n  agent_name: \"旧\"\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Identity: &core.AgentIdentityChanges{AgentName: &agentName}},
		},
		{
			name: "description 行内注释",
			raw:  "---\nname: writer\ndescription: \"旧\" # 备注\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Description: &description},
		},
		{
			name: "schedules 块内注释",
			raw:  "---\nname: writer\nschedules:\n  # 旧任务\n  - id: \"w-1\"\n    cron: \"0 0 9 * * 1\"\n    message: \"m\"\n---\n\n正文\n",
			ch: core.AgentContentChanges{Schedules: &[]core.Schedule{{
				ID: "w-1", Cron: "0 0 9 * * 1", Zone: "UTC", Message: "m2",
			}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyChanges([]byte(tc.raw), tc.ch)
			require.ErrorIs(t, err, ErrEditUnsupported)
		})
	}
}

// TestApplyChanges_RejectsAnchorsAliasesAndDuplicates：锚点 / 别名与重复键无法安全
// 行级重写（会改变语义或指向）→ EDIT_UNSUPPORTED，包含"锚点不唯一"的重复键形态。
// Anchors, aliases and duplicate keys refuse the patch.
func TestApplyChanges_RejectsAnchorsAliasesAndDuplicates(t *testing.T) {
	model := "x"
	description := "x"
	agentName := "x"
	cases := []struct {
		name string
		raw  string
		ch   core.AgentContentChanges
	}{
		{
			name: "映射值锚点",
			raw:  "---\nname: writer\nprovider:\n  model: &base \"旧\"\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Provider: &core.AgentProviderChanges{Model: &model}},
		},
		{
			name: "映射值别名",
			raw:  "---\nname: writer\nprovider:\n  model: *base\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Provider: &core.AgentProviderChanges{Model: &model}},
		},
		{
			name: "顶层键重复（锚点不唯一）",
			raw:  "---\nname: writer\ndescription: \"a\"\ndescription: \"b\"\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Description: &description},
		},
		{
			name: "映射子键重复",
			raw:  "---\nname: writer\nidentity:\n  agent_name: \"a\"\n  agent_name: \"b\"\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Identity: &core.AgentIdentityChanges{AgentName: &agentName}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyChanges([]byte(tc.raw), tc.ch)
			require.ErrorIs(t, err, ErrEditUnsupported)
		})
	}
}

// TestApplyChanges_RejectsFlowCollections：流式集合整块重建会丢掉原排版，拒绝。
// Flow collections refuse the patch.
func TestApplyChanges_RejectsFlowCollections(t *testing.T) {
	tools := []string{"x"}
	agentName := "x"
	description := "x"
	cases := []struct {
		name string
		raw  string
		ch   core.AgentContentChanges
	}{
		{
			name: "流式序列",
			raw:  "---\nname: writer\ntools: [\"a\", \"b\"]\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Tools: &tools},
		},
		{
			name: "流式映射",
			raw:  "---\nname: writer\nidentity: {agent_name: \"旧\"}\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Identity: &core.AgentIdentityChanges{AgentName: &agentName}},
		},
		{
			name: "标量值流式集合",
			raw:  "---\nname: writer\ndescription: [a]\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Description: &description},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyChanges([]byte(tc.raw), tc.ch)
			require.ErrorIs(t, err, ErrEditUnsupported)
		})
	}
}

// TestApplyChanges_RejectsMultilineScalars：块标量 / 多行值不在单行重写能力内，拒绝。
// Block scalars and multi-line values refuse the patch.
func TestApplyChanges_RejectsMultilineScalars(t *testing.T) {
	description := "x"
	model := "x"
	cases := []struct {
		name string
		raw  string
		ch   core.AgentContentChanges
	}{
		{
			name: "description 块标量",
			raw:  "---\nname: writer\ndescription: |\n  第一行\n  第二行\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Description: &description},
		},
		{
			name: "provider.model 块标量",
			raw:  "---\nname: writer\nprovider:\n  model: |\n    x\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Provider: &core.AgentProviderChanges{Model: &model}},
		},
		{
			name: "provider.model 空值续行",
			raw:  "---\nname: writer\nprovider:\n  model:\n    x\n---\n\n正文\n",
			ch:   core.AgentContentChanges{Provider: &core.AgentProviderChanges{Model: &model}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyChanges([]byte(tc.raw), tc.ch)
			require.ErrorIs(t, err, ErrEditUnsupported)
		})
	}
}

// TestApplyChanges_RejectsUnknownFieldsInTargetBlock：目标块出现字段表之外的键时，
// 无法判断它该不该被保留 → 拒绝（不得静默丢弃）。
// Unknown keys inside a target block refuse the patch.
func TestApplyChanges_RejectsUnknownFieldsInTargetBlock(t *testing.T) {
	agentName := "x"
	raw := "---\nname: writer\nidentity:\n  agent_name: \"旧\"\n  extra: \"y\"\n---\n\n正文\n"
	_, err := ApplyChanges([]byte(raw), core.AgentContentChanges{
		Identity: &core.AgentIdentityChanges{AgentName: &agentName},
	})
	require.ErrorIs(t, err, ErrEditUnsupported)
}

// TestApplyChanges_RejectsBrokenFence：围栏缺失或不闭合时不做"帮用户补围栏"的猜测。
// A missing or unclosed fence refuses the patch.
func TestApplyChanges_RejectsBrokenFence(t *testing.T) {
	description := "x"
	for name, raw := range map[string]string{
		"缺少起始围栏": "name: writer\n",
		"围栏未闭合":  "---\nname: writer\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ApplyChanges([]byte(raw), core.AgentContentChanges{Description: &description})
			require.ErrorIs(t, err, ErrEditUnsupported)
		})
	}
}

// TestApplyChanges_NoChangesIsByteIdentical：空补丁返回逐字节相同的原文（含 BOM 原文）。
// An empty patch returns the original bytes unchanged, BOM included.
func TestApplyChanges_NoChangesIsByteIdentical(t *testing.T) {
	for _, raw := range []string{editorContent, string(rune(0xFEFF)) + editorContent} {
		patched, err := ApplyChanges([]byte(raw), core.AgentContentChanges{})
		require.NoError(t, err)
		require.Equal(t, raw, string(patched))
	}
}

// ---------------------------------------------------------------------------
// 内容写（图 D / 约束 25–27）
// ---------------------------------------------------------------------------

// TestAgentEditor_UpdateContentRequiresStrongIfMatch：缺失、弱 ETag、`*`、裸哈希与
// 不匹配一律 412；磁盘逐字节不变，且**不写管理操作行**（约束 25）。
// Every non-matching or malformed If-Match answers 412 with zero disk writes and
// no operation row.
func TestAgentEditor_UpdateContentRequiresStrongIfMatch(t *testing.T) {
	fx := newEditorFixture(t, editorContent)
	current := ContentETag([]byte(editorContent))
	attempts := []string{
		"",
		"W/" + current,
		"*",
		strings.Trim(current, `"`),
		`"sha256:` + strings.Repeat("0", 64) + `"`,
	}
	for _, ifMatch := range attempts {
		_, err := fx.editor.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: editorContentReplaced}, ifMatch, core.Actor{Subject: "token:test"})
		require.ErrorIs(t, err, core.ErrPreconditionFailed, "If-Match=%q", ifMatch)
		require.Equal(t, editorContent, string(fx.disk(t)), "412 必须零写入（If-Match=%q）", ifMatch)
	}
	require.Empty(t, fx.ops.begun, "412 不得写管理操作行")
	require.Empty(t, fx.snapshots.saved)
	require.Empty(t, fx.registry.entries)
}

// TestAgentEditor_UpdateContentPatchAndReplacementSucceed：整份替换与结构化补丁都
// 走完"校验 → 快照 → pending → 原子写 → 注册表 → 调度 → 终态"，返回受管清单。
// Both the whole-file and the patch paths complete the full Figure D sequence.
func TestAgentEditor_UpdateContentPatchAndReplacementSucceed(t *testing.T) {
	description := "新描述"
	model := "mock-pro"
	tools := []string{"file_read"}
	schedules := []core.Schedule{{ID: "writer-1", Cron: "0 0 9 * * 1", Zone: "UTC", Message: "生成周报"}}

	cases := []struct {
		name     string
		update   core.AgentContentUpdate
		expected string
	}{
		{
			name:     "整份替换",
			update:   core.AgentContentUpdate{Content: editorContentReplaced},
			expected: editorContentReplaced,
		},
		{
			name: "结构化补丁",
			update: core.AgentContentUpdate{Changes: &core.AgentContentChanges{
				Description: &description,
				Provider:    &core.AgentProviderChanges{Model: &model},
				Tools:       &tools,
				Schedules:   &schedules,
			}},
			expected: editorContentReplaced,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEditorFixture(t, editorContent)
			files, err := fx.editor.UpdateContent(context.Background(), "writer", tc.update,
				ContentETag([]byte(editorContent)), core.Actor{Subject: "token:test", Method: core.AuthMethodBearer, Source: "127.0.0.1:5555"})
			require.NoError(t, err)

			require.Equal(t, tc.expected, string(fx.disk(t)))
			require.Equal(t, "writer", files.Agent)
			require.Len(t, files.Files, 1)
			require.Equal(t, agentDefinitionFile, files.Files[0].Path)
			require.Equal(t, int64(len(tc.expected)), files.Files[0].Size)
			require.NotContains(t, fx.dirEntries(t), agentDefinitionFile+".bak", "成功路径不得残留备份")

			require.Len(t, fx.ops.begun, 1)
			record := fx.ops.begun[0]
			require.Equal(t, "agent.content.update", record.Action)
			require.Equal(t, "agent", record.TargetKind)
			require.Equal(t, "writer", record.TargetID)
			require.Equal(t, "token:test", record.ActorSubject)
			require.Equal(t, core.AuthMethodBearer, record.AuthMethod)
			require.Equal(t, "127.0.0.1:5555", record.Source)
			require.Equal(t, ContentETag([]byte(editorContent)), record.ExpectedETag)
			require.Equal(t, core.OperationPending, record.Status)
			require.Equal(t, checksumPrefix+sha256Hex([]byte(tc.expected)), record.RequestHash)

			require.Len(t, fx.ops.finished, 1)
			require.Equal(t, core.OperationSuccess, fx.ops.finished[0].status)
			require.Equal(t, resultCodeOK, fx.ops.finished[0].resultCode)
			var detail map[string]string
			require.NoError(t, json.Unmarshal(fx.ops.finished[0].detail, &detail))
			require.Equal(t, sha256Hex([]byte(tc.expected)), detail["candidate_sha256"])
			require.Equal(t, sha256Hex([]byte(editorContent)), detail["previous_sha256"])

			require.Len(t, fx.snapshots.saved, 1)
			require.Equal(t, tc.expected, fx.snapshots.saved[0].Content)
			require.NotEmpty(t, fx.snapshots.saved[0].Hash)

			entry, ok := fx.registry.get("writer")
			require.True(t, ok)
			require.True(t, entry.Registered)
			require.Len(t, fx.scheduler.calls, 1)
			require.Len(t, fx.scheduler.calls[0], 1)
			require.Equal(t, "生成周报", fx.scheduler.calls[0][0].Message)
		})
	}
}

// TestAgentEditor_UpdateContentValidationFailureZeroWrites：装载期校验失败 → 400 分类、
// 磁盘零字节变化、无快照、无操作行（校验先于一切写入）。
// A validation failure is a 400-class error with zero writes and no operation row.
func TestAgentEditor_UpdateContentValidationFailureZeroWrites(t *testing.T) {
	fx := newEditorFixture(t, editorContent)
	invalid := strings.ReplaceAll(editorContentReplaced, "name: writer", "name: other")

	_, err := fx.editor.UpdateContent(context.Background(), "writer",
		core.AgentContentUpdate{Content: invalid},
		ContentETag([]byte(editorContent)), core.Actor{Subject: "token:test"})

	var invalidInput *core.InvalidInputError
	require.ErrorAs(t, err, &invalidInput)
	require.Equal(t, "agent.content", invalidInput.Tool)
	require.Equal(t, editorContent, string(fx.disk(t)), "校验失败必须零写入")
	require.Empty(t, fx.ops.begun, "校验失败不得写管理操作行")
	require.Empty(t, fx.snapshots.saved)
	require.Empty(t, fx.registry.entries)
}

// TestAgentEditor_UpdateContentPreRenameFailuresLeaveDiskUntouched：预写失败、快照失败
// 与 rename 失败都不改变磁盘；rename 失败同时不留备份 / 临时文件残留。
// Pre-rename failures leave the disk byte-identical and leave no residue.
func TestAgentEditor_UpdateContentPreRenameFailuresLeaveDiskUntouched(t *testing.T) {
	t.Run("预写失败", func(t *testing.T) {
		fx := newEditorFixture(t, editorContent)
		fx.ops.beginErr = errors.New("数据库不可用")
		_, err := fx.editor.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: editorContentReplaced},
			ContentETag([]byte(editorContent)), core.Actor{Subject: "token:test"})
		require.ErrorContains(t, err, "预写管理操作")
		require.Equal(t, editorContent, string(fx.disk(t)))
		require.Empty(t, fx.snapshots.saved)
		require.Empty(t, fx.ops.finished)
	})

	t.Run("快照失败", func(t *testing.T) {
		fx := newEditorFixture(t, editorContent)
		fx.snapshots.err = errors.New("磁盘已满")
		_, err := fx.editor.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: editorContentReplaced},
			ContentETag([]byte(editorContent)), core.Actor{Subject: "token:test"})
		require.ErrorContains(t, err, "保存配置快照")
		require.Equal(t, editorContent, string(fx.disk(t)))
		require.Len(t, fx.ops.begun, 1)
		require.Len(t, fx.ops.finished, 1)
		require.Equal(t, core.OperationFailed, fx.ops.finished[0].status)
		require.Empty(t, fx.registry.entries)
		require.Empty(t, fx.scheduler.calls)
	})

	t.Run("rename 失败", func(t *testing.T) {
		fx := newEditorFixture(t, editorContent)
		fx.editor.rename = func(*os.Root, string, string) error { return errors.New("rename 被拒绝") }
		_, err := fx.editor.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: editorContentReplaced},
			ContentETag([]byte(editorContent)), core.Actor{Subject: "token:test"})
		require.ErrorContains(t, err, "替换")
		require.Equal(t, editorContent, string(fx.disk(t)))
		require.Len(t, fx.ops.finished, 1)
		require.Equal(t, core.OperationFailed, fx.ops.finished[0].status)
		require.NotContains(t, fx.dirEntries(t), agentDefinitionFile+".bak")
		for _, name := range fx.dirEntries(t) {
			require.NotContains(t, name, ".tmp-")
		}
	})
}

// TestAgentEditor_UpdateContentPostRenameFailureRollsBack：rename 之后调度同步失败 →
// 文件、注册表与调度全部回滚为写前状态，操作回填 failed（约束 27）。
// A post-rename schedule failure rolls file, registry and schedules back.
func TestAgentEditor_UpdateContentPostRenameFailureRollsBack(t *testing.T) {
	fx := newEditorFixture(t, editorContent)
	oldEntry, _, _, err := testValidator()(context.Background(), "writer", []byte(editorContent))
	require.NoError(t, err)
	fx.registry.Replace("writer", oldEntry)
	fx.scheduler.failures = 1
	fx.scheduler.err = errors.New("调度器不可用")

	_, err = fx.editor.UpdateContent(context.Background(), "writer",
		core.AgentContentUpdate{Content: editorContentReplaced},
		ContentETag([]byte(editorContent)), core.Actor{Subject: "token:test"})
	require.ErrorContains(t, err, "同步 Agent 定时")

	require.Equal(t, editorContent, string(fx.disk(t)), "回滚必须恢复逐字节原文")
	entry, ok := fx.registry.get("writer")
	require.True(t, ok)
	require.Equal(t, "旧描述", entry.Profile.Description)
	require.Len(t, fx.scheduler.calls, 2)
	require.Equal(t, "生成周报", fx.scheduler.calls[0][0].Message)
	require.Equal(t, "生成本周周报", fx.scheduler.calls[1][0].Message)
	require.Len(t, fx.ops.finished, 1)
	require.Equal(t, core.OperationFailed, fx.ops.finished[0].status)
	require.NotContains(t, fx.dirEntries(t), agentDefinitionFile+".bak", "回滚后备份已被消费")
}

// TestAgentEditor_UpdateContentRollbackFailureMarksUnknown：回滚本身失败时操作必须标为
// unknown（绝不 success）并留下可排障的错误，磁盘保持替换后的状态。
// A failed rollback marks the operation unknown and never success.
func TestAgentEditor_UpdateContentRollbackFailureMarksUnknown(t *testing.T) {
	fx := newEditorFixture(t, editorContent)
	fx.scheduler.failures = 2
	fx.scheduler.err = errors.New("调度器不可用")
	fx.editor.rename = func(root *os.Root, oldname, newname string) error {
		if strings.HasSuffix(oldname, ".bak") {
			return errors.New("备份已损坏")
		}
		return root.Rename(oldname, newname)
	}

	_, err := fx.editor.UpdateContent(context.Background(), "writer",
		core.AgentContentUpdate{Content: editorContentReplaced},
		ContentETag([]byte(editorContent)), core.Actor{Subject: "token:test"})
	require.ErrorContains(t, err, "unknown")
	require.Equal(t, editorContentReplaced, string(fx.disk(t)))
	require.Len(t, fx.ops.finished, 1)
	require.Equal(t, core.OperationUnknown, fx.ops.finished[0].status)
	require.Equal(t, resultCodeInternal, fx.ops.finished[0].resultCode)
}

// TestAgentEditor_ConcurrentSave：同一 ETag 的两个并发保存恰好一个成功，另一个
// 412；最终磁盘等于赢家的字节（约束 27）。函数名按模块卡 §4.4 的 011-race
// required_tests 定名（原名 UpdateContent... 不再保留）。
// Two concurrent saves under the same ETag: exactly one wins, the other gets 412.
func TestAgentEditor_ConcurrentSave(t *testing.T) {
	fx := newEditorFixture(t, editorContent)
	etag := ContentETag([]byte(editorContent))
	first := strings.Replace(editorContentReplaced, "新描述", "并发甲", 1)
	second := strings.Replace(editorContentReplaced, "新描述", "并发乙", 1)

	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, content := range []string{first, second} {
		wg.Add(1)
		go func(content string) {
			defer wg.Done()
			_, err := fx.editor.UpdateContent(context.Background(), "writer",
				core.AgentContentUpdate{Content: content}, etag, core.Actor{Subject: "token:test"})
			results <- err
		}(content)
	}
	wg.Wait()
	close(results)

	var succeeded, precondition int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, core.ErrPreconditionFailed):
			precondition++
		default:
			t.Fatalf("并发保存只允许成功或 412，实际：%v", err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, precondition)
	require.Contains(t, []string{first, second}, string(fx.disk(t)))
	require.Len(t, fx.ops.begun, 1)
	require.Len(t, fx.ops.finished, 1)
	require.Equal(t, core.OperationSuccess, fx.ops.finished[0].status)
}
