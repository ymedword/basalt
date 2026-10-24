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
	"slices"
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

	_, err = editor.ReadContent(context.Background(), "agents/writer")
	require.ErrorContains(t, err, "路径分隔符")

	// 越界形态（`..` 前缀）同样在读取之前被拒；US-12 起点前缀规则先于分隔符规则命中，
	// 故这里只判"被拒"，不锁定具体文案（判据面随 M9 的名称规则变更，见模块卡 §2.2）。
	_, err = editor.ReadContent(context.Background(), "../agents/writer")
	require.Error(t, err)
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
	return newEditorFixtureWith(t, raw, nil)
}

// newEditorFixtureWith 与 newEditorFixture 同构，额外注入受控工具装配钩子。
// newEditorFixtureWith is newEditorFixture plus an injected tool-assembly hook.
func newEditorFixtureWith(t *testing.T, raw string,
	assemble func(context.Context, string, *core.Profile) ([]string, core.ToolChange, error),
) *editorFixture {
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
		Root:          root,
		Gate:          core.NewAgentGate(),
		Ops:           fx.ops,
		Snapshots:     fx.snapshots,
		Registry:      fx.registry,
		Scheduler:     fx.scheduler,
		Validate:      testValidator(),
		AssembleTools: assemble,
		Now:           func() time.Time { return time.UnixMilli(1729500000123).UTC() },
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

// TestApplyChanges_ClearTemperatureRemovesOnlyThatLine：`temperature: null` 只移除
// 目标行，其余字节逐字保留；字段本来不存在时是幂等 no-op（不是错误）。
// Clearing removes exactly the temperature line and is a no-op when absent.
func TestApplyChanges_ClearTemperatureRemovesOnlyThatLine(t *testing.T) {
	withTemperature := strings.Replace(editorContent,
		"  model: \"mock-model\"\n", "  model: \"mock-model\"\n  temperature: 0.7\n", 1)
	require.Contains(t, withTemperature, "temperature: 0.7")

	patched, err := ApplyChanges([]byte(withTemperature), core.AgentContentChanges{
		Provider: &core.AgentProviderChanges{ClearTemperature: true},
	})
	require.NoError(t, err)
	require.Equal(t, editorContent, string(patched), "只移除 temperature 行")

	patched, err = ApplyChanges([]byte(editorContent), core.AgentContentChanges{
		Provider: &core.AgentProviderChanges{ClearTemperature: true},
	})
	require.NoError(t, err)
	require.Equal(t, editorContent, string(patched), "字段不存在时清除是 no-op")
}

// TestApplyChanges_RejectsTemperatureSetAndClear：同一补丁同时写值与清除是请求形状
// 错误——不得让后写覆盖先写而静默选一个。
// Setting and clearing the same field in one patch is refused.
func TestApplyChanges_RejectsTemperatureSetAndClear(t *testing.T) {
	value := 0.0
	_, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{
		Provider: &core.AgentProviderChanges{Temperature: &value, ClearTemperature: true},
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrEditUnsupported)
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

// TestApplyChanges_NotifyChannelsPatchOnly：notify_channels 整块替换、块外字节不动；
// 空列表写成 `notify_channels: []`（显式清空 = 不限制，与未声明同义）。
// The allowlist is patched as one block; an empty list serialises as [].
func TestApplyChanges_NotifyChannelsPatchOnly(t *testing.T) {
	channels := []string{"ops-alerts", "night-ops"}
	patched, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{
		NotifyChannels: &channels,
	})
	require.NoError(t, err)
	expected := strings.Replace(editorContent,
		"message: \"生成本周周报\"\n---\n",
		"message: \"生成本周周报\"\nnotify_channels:\n  - \"ops-alerts\"\n  - \"night-ops\"\n---\n", 1)
	require.Equal(t, expected, string(patched))

	empty := []string{}
	patched, err = ApplyChanges([]byte(editorContent), core.AgentContentChanges{
		NotifyChannels: &empty,
	})
	require.NoError(t, err)
	require.Contains(t, string(patched), "notify_channels: []")
	// 其余块逐字节不变：只动目标块是块编辑的硬前提。
	require.Contains(t, string(patched), "tools:\n  - \"file_read\"\n  - \"file_write\"\n")
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

// bindingsEditorContent 是带 resource_bindings 块的原文夹具：块前块后都有字节，
// 用于断言整块替换只动目标块。
// bindingsEditorContent carries a bindings block with material on both sides.
const bindingsEditorContent = `---
name: writer
description: "旧描述"
provider:
  name: "deepseek"
  model: "mock-model"
resource_bindings:
  model_connection:
    - name: old-conn
      uid: "rs_01J8Z7A1B2C3D4E5F6G7H8J9K0"
  notify_channel: []
schedules: []
---

你是周报助手。
`

// TestApplyChanges_BindingsReplaceWholeBlock：`changes.bindings` 是整块替换——
// nil = 不改（逐字节相同）；非 nil（含空切片）以数组为唯一内容重写，服务端不按
// 名称重新解析 UID、不按名合并；块外字节逐字保留。
// Acceptance point: bindings patch replaces the whole block and never merges by name.
func TestApplyChanges_BindingsReplaceWholeBlock(t *testing.T) {
	want := core.ResourceBindings{
		ModelConnections: []core.ResourceBinding{
			{Name: "deepseek", UID: testModelUID},
			{Name: "backup", UID: testNotifyUID},
		},
		NotifyChannels: []core.ResourceBinding{},
	}
	patched, err := ApplyChanges([]byte(bindingsEditorContent), core.AgentContentChanges{
		Bindings: []core.BindingChange{
			{ResourceKind: core.BindingModelConnection, RefName: "deepseek", ResourceUID: testModelUID},
			{ResourceKind: core.BindingModelConnection, RefName: "backup", ResourceUID: testNotifyUID},
		},
	})
	require.NoError(t, err)
	expected := strings.Replace(bindingsEditorContent,
		"resource_bindings:\n  model_connection:\n    - name: old-conn\n      uid: \"rs_01J8Z7A1B2C3D4E5F6G7H8J9K0\"\n  notify_channel: []\n",
		"resource_bindings:\n  model_connection:\n    - name: \"deepseek\"\n      uid: \""+testModelUID+"\"\n    - name: \"backup\"\n      uid: \""+testNotifyUID+"\"\n  notify_channel: []\n", 1)
	require.Equal(t, expected, string(patched))

	got, err := ParseResourceBindings(patched)
	require.NoError(t, err)
	require.Equal(t, want.ModelConnections, got.ModelConnections)
	require.Empty(t, got.NotifyChannels)

	// nil = 不改：逐字节相同。
	same, err := ApplyChanges([]byte(bindingsEditorContent), core.AgentContentChanges{})
	require.NoError(t, err)
	require.Equal(t, bindingsEditorContent, string(same))

	// 空切片 = 清空两组（显式替换，不是"不改"）。
	cleared, err := ApplyChanges([]byte(bindingsEditorContent),
		core.AgentContentChanges{Bindings: []core.BindingChange{}})
	require.NoError(t, err)
	require.Contains(t, string(cleared),
		"resource_bindings:\n  model_connection: []\n  notify_channel: []\n")
	require.Contains(t, string(cleared), "schedules: []\n---\n\n你是周报助手。\n",
		"块外字节必须逐字保留")
}

// TestApplyChanges_RejectsInvalidBindingsPatch：类别字面量、UID 格式、重复名 / 重复
// UID 任一非法 → 包装 ErrAgentResourceBindingInvalid 拒绝，不返回候选字节（零写入）。
// Acceptance point: every structurally invalid patch refuses before any write.
func TestApplyChanges_RejectsInvalidBindingsPatch(t *testing.T) {
	cases := map[string][]core.BindingChange{
		"未知类别": {{ResourceKind: "skill", RefName: "a", ResourceUID: testModelUID}},
		"UID 非法": {
			{ResourceKind: core.BindingModelConnection, RefName: "a", ResourceUID: "01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"},
		},
		"重复名称": {
			{ResourceKind: core.BindingModelConnection, RefName: "a", ResourceUID: testModelUID},
			{ResourceKind: core.BindingModelConnection, RefName: "a", ResourceUID: testNotifyUID},
		},
		"重复 UID": {
			{ResourceKind: core.BindingNotifyChannel, RefName: "a", ResourceUID: testModelUID},
			{ResourceKind: core.BindingNotifyChannel, RefName: "b", ResourceUID: testModelUID},
		},
	}
	for name, changes := range cases {
		t.Run(name, func(t *testing.T) {
			patched, err := ApplyChanges([]byte(bindingsEditorContent),
				core.AgentContentChanges{Bindings: changes})
			require.ErrorIs(t, err, core.ErrAgentResourceBindingInvalid)
			require.Nil(t, patched, "拒绝时不得返回候选字节")
		})
	}
}

// TestApplyChanges_NotifyModePatch：`notify_mode` 是顶层标量补丁——缺失时插在收尾
// 围栏之前，已存在时原位替换；其余字节逐字保留。
// Acceptance point: notify_mode is patched in place and inserted before the fence.
func TestApplyChanges_NotifyModePatch(t *testing.T) {
	mode := core.NotifyModeAllEligible
	patched, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{NotifyMode: &mode})
	require.NoError(t, err)
	expected := strings.Replace(editorContent, "message: \"生成本周周报\"\n---\n",
		"message: \"生成本周周报\"\nnotify_mode: \"all_eligible\"\n---\n", 1)
	require.Equal(t, expected, string(patched))

	raw := "---\nname: writer\nnotify_mode: none\n---\n\n正文。\n"
	allow := core.NotifyModeAllowlist
	channels := []string{"ops"}
	patched, err = ApplyChanges([]byte(raw), core.AgentContentChanges{
		NotifyMode: &allow, NotifyChannels: &channels,
	})
	require.NoError(t, err)
	require.Equal(t, "---\nname: writer\nnotify_mode: \"allowlist\"\nnotify_channels:\n  - \"ops\"\n---\n\n正文。\n",
		string(patched))
}

// TestApplyChanges_BodyReplaceAndClear：`changes.body` 整段替换围栏外正文——
// frontmatter 与闭合围栏逐字节保留；空串清空正文；与 frontmatter 补丁同批提交时
// 互不干扰；缺围栏仍按 ErrEditUnsupported 拒绝（不猜结构）。
// Acceptance point: body patches replace or clear only the body, never the frontmatter.
func TestApplyChanges_BodyReplaceAndClear(t *testing.T) {
	body := "新的正文。\n第二行。"
	patched, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{Body: &body})
	require.NoError(t, err)
	require.Equal(t, strings.Replace(editorContent, "---\n\n你是周报助手。\n",
		"---\n\n新的正文。\n第二行。\n", 1), string(patched))

	// 空串 = 清空正文：文件在闭合围栏行结束后截止，frontmatter 一个字节不动。
	empty := ""
	patched, err = ApplyChanges([]byte(editorContent), core.AgentContentChanges{Body: &empty})
	require.NoError(t, err)
	cut := strings.Index(editorContent, "---\n\n你是周报助手。\n") + len("---\n")
	require.Equal(t, editorContent[:cut], string(patched), "空串必须清空正文且保留 frontmatter")

	// 与 frontmatter 补丁同批提交：两段各自生效，互不吞噬。
	desc := "新描述"
	combined, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{
		Description: &desc, Body: &body,
	})
	require.NoError(t, err)
	require.Contains(t, string(combined), `description: "新描述"`)
	_, bodyAfter, splitErr := splitFrontmatter(combined)
	require.NoError(t, splitErr)
	require.Equal(t, body, string(bodyAfter))

	// 缺围栏：补丁路径不补围栏，直接拒绝。
	_, err = ApplyChanges([]byte("没有围栏的正文"), core.AgentContentChanges{Body: &body})
	require.ErrorIs(t, err, ErrEditUnsupported)
}

// TestApplyChanges_DisplayNameThreeState：`display_name` 三态（R6.2）——非空写入
// 去空白后的值（字段缺失时插在闭合围栏前），空串移除该行（回到回退技术名），
// 字段不存在时移除是字节级空操作（ETag 不变）；其余字节逐字保留。
// TestApplyChanges_DisplayNameThreeState pins set / clear / no-op and byte preservation.
func TestApplyChanges_DisplayNameThreeState(t *testing.T) {
	display := "  值班助手  "
	patched, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{DisplayName: &display})
	require.NoError(t, err)
	expected := strings.Replace(editorContent,
		"    message: \"生成本周周报\"\n---\n",
		"    message: \"生成本周周报\"\ndisplay_name: \"值班助手\"\n---\n", 1)
	require.Equal(t, expected, string(patched), "只追加展示名行，块外字节逐字保留")

	// 已有值改为新值：只改该行。
	next := "新的展示名"
	replaced, err := ApplyChanges([]byte(expected), core.AgentContentChanges{DisplayName: &next})
	require.NoError(t, err)
	require.Equal(t, strings.Replace(expected,
		`display_name: "值班助手"`, `display_name: "新的展示名"`, 1), string(replaced))

	// 空串 = 移除该行；字段不存在时是字节级空操作（ETag 因而不变）。
	clear := ""
	cleared, err := ApplyChanges([]byte(expected), core.AgentContentChanges{DisplayName: &clear})
	require.NoError(t, err)
	require.Equal(t, editorContent, string(cleared), "移除后必须逐字节回到原文件")

	noop, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{DisplayName: &clear})
	require.NoError(t, err)
	require.Equal(t, editorContent, string(noop), "字段不存在时移除为空操作")
	require.Equal(t, ContentETag([]byte(editorContent)), ContentETag(noop), "空操作不得改变 ETag")
}

// TestApplyChanges_DisplayNameRejectsInvalid：结构化写面拒绝超过 64 字符与控制字符
// 的展示名（400 口径的 InvalidInputError）；装载期不判这个字段，只有写面拦。
// TestApplyChanges_DisplayNameRejectsInvalid refuses oversized and control-character
// values on the structured write surface only.
func TestApplyChanges_DisplayNameRejectsInvalid(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{name: "too-long", value: strings.Repeat("名", displayNameMaxRunes+1)},
		{name: "control-char", value: "值班\n助手"},
		{name: "tab", value: "值班\t助手"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{DisplayName: &tc.value})
			var invalid *core.InvalidInputError
			require.ErrorAs(t, err, &invalid)
			require.False(t, errors.Is(err, ErrEditUnsupported), "非法值按 400 口径归类")
		})
	}

	// 边界：恰好 64 个字符必须接受。
	atCap := strings.Repeat("名", displayNameMaxRunes)
	_, err := ApplyChanges([]byte(editorContent), core.AgentContentChanges{DisplayName: &atCap})
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// 受控工具装配（R7 / 宪法约束 11）
// ---------------------------------------------------------------------------

// assemblyContent 是受控工具装配用例的基础内容：`notify_mode` 已显式声明、
// `tools` 是普通块序列（可被行级补丁安全改写）。
// assemblyContent is the assembly fixture: an explicit notify_mode and a block tools list.
const assemblyContent = `---
name: writer
notify_mode: all_eligible
tools:
  - "save_memory"
provider:
  name: "mock"
  model: "mock-pro"
---

正文。
`

// assemblyNoneContent 与 assemblyContent 同形，但通知意图为 none 且已声明 notify——
// 保存时系统必须把它移除并作为可见变更返回。
// assemblyNoneContent declares notify_mode none together with the notify tool.
const assemblyNoneContent = `---
name: writer
notify_mode: none
tools:
  - "save_memory"
  - "notify"
provider:
  name: "mock"
  model: "mock-pro"
---

正文。
`

// TestAgentEditor_UpdateContentAssemblesNotify：装配钩子的增删落到磁盘，并随保存
// 结果返回；结构化补丁与整份原文两条写面走同一逻辑。
// The assembly delta reaches the disk and is returned with the save result on both
// the patch and the whole-file write faces.
func TestAgentEditor_UpdateContentAssemblesNotify(t *testing.T) {
	addNotify := func(_ context.Context, _ string, profile *core.Profile) ([]string, core.ToolChange, error) {
		if slices.Contains(profile.Tools, core.NotifyToolName) {
			return profile.Tools, core.ToolChange{}, nil
		}
		return append(slices.Clone(profile.Tools), core.NotifyToolName),
			core.ToolChange{Added: []string{core.NotifyToolName}}, nil
	}
	dropNotify := func(_ context.Context, _ string, profile *core.Profile) ([]string, core.ToolChange, error) {
		if !slices.Contains(profile.Tools, core.NotifyToolName) {
			return profile.Tools, core.ToolChange{}, nil
		}
		return slices.DeleteFunc(slices.Clone(profile.Tools), func(name string) bool {
				return name == core.NotifyToolName
			}),
			core.ToolChange{Removed: []string{core.NotifyToolName}}, nil
	}

	t.Run("结构化补丁加入 notify", func(t *testing.T) {
		fx := newEditorFixtureWith(t, assemblyContent, addNotify)
		description := "新描述"
		files, err := fx.editor.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Changes: &core.AgentContentChanges{Description: &description}},
			ContentETag([]byte(assemblyContent)), core.Actor{Subject: "token:test"})
		require.NoError(t, err)

		require.Equal(t, []string{core.NotifyToolName}, files.Changes.Added)
		require.Empty(t, files.Changes.Removed)
		disk := string(fx.disk(t))
		require.Contains(t, disk, "  - \"notify\"\n", "装配结果必须落到磁盘")
		require.Contains(t, disk, "description: \"新描述\"\n")
		require.Len(t, fx.ops.begun, 1)
		require.Equal(t, core.OperationSuccess, fx.ops.finished[0].status)
	})

	t.Run("整份原文移除 notify", func(t *testing.T) {
		fx := newEditorFixtureWith(t, assemblyNoneContent, dropNotify)
		files, err := fx.editor.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: assemblyNoneContent},
			ContentETag([]byte(assemblyNoneContent)), core.Actor{Subject: "token:test"})
		require.NoError(t, err)

		require.Equal(t, []string{core.NotifyToolName}, files.Changes.Removed)
		require.Empty(t, files.Changes.Added)
		require.NotContains(t, string(fx.disk(t)), "  - \"notify\"\n", "none 下必须不装配 notify")
	})

	t.Run("无变更时不改写 tools", func(t *testing.T) {
		// 内容里已声明 notify 时钩子返回零变更；tools 行逐字节保留。
		raw := strings.Replace(assemblyContent, "  - \"save_memory\"\n", "  - \"save_memory\"\n  - \"notify\"\n", 1)
		fx2 := newEditorFixtureWith(t, raw, addNotify)
		description := "新描述"
		files, err := fx2.editor.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Changes: &core.AgentContentChanges{Description: &description}},
			ContentETag([]byte(raw)), core.Actor{Subject: "token:test"})
		require.NoError(t, err)
		require.True(t, files.Changes.Empty())
		require.Contains(t, string(fx2.disk(t)), "tools:\n  - \"save_memory\"\n  - \"notify\"\n")
	})
}

// TestAgentEditor_UpdateContentRejectsForeignToolChange：装配层返回 notify 之外的
// 工具名即实现漂移——直接拒绝，磁盘零写入、零操作行（系统不得自动改其他工具）。
// Any tool name other than notify from the assembly hook is refused outright.
func TestAgentEditor_UpdateContentRejectsForeignToolChange(t *testing.T) {
	hook := func(_ context.Context, _ string, profile *core.Profile) ([]string, core.ToolChange, error) {
		return append(slices.Clone(profile.Tools), "shell"), core.ToolChange{Added: []string{"shell"}}, nil
	}
	fx := newEditorFixtureWith(t, assemblyContent, hook)
	description := "新描述"
	_, err := fx.editor.UpdateContent(context.Background(), "writer",
		core.AgentContentUpdate{Changes: &core.AgentContentChanges{Description: &description}},
		ContentETag([]byte(assemblyContent)), core.Actor{Subject: "token:test"})
	require.ErrorContains(t, err, "不允许的工具名")
	require.Equal(t, assemblyContent, string(fx.disk(t)))
	require.Empty(t, fx.ops.begun, "拒绝必须发生在预写之前")
}

// TestAgentEditor_UpdateContentRefusesUnpatchableTools：通知意图要求装配但 tools
// 段无法安全改写（流式集合）时拒绝并点名下一步，磁盘零写入——不静默保留不一致。
// When the assembly needs a change but the tools segment cannot be rewritten safely,
// the save refuses with an actionable message instead of silently keeping the mismatch.
func TestAgentEditor_UpdateContentRefusesUnpatchableTools(t *testing.T) {
	raw := `---
name: writer
notify_mode: all_eligible
tools: [save_memory]
provider:
  name: "mock"
  model: "mock-pro"
---

正文。
`
	hook := func(_ context.Context, _ string, profile *core.Profile) ([]string, core.ToolChange, error) {
		return append(slices.Clone(profile.Tools), core.NotifyToolName),
			core.ToolChange{Added: []string{core.NotifyToolName}}, nil
	}
	fx := newEditorFixtureWith(t, raw, hook)
	description := "新描述"
	_, err := fx.editor.UpdateContent(context.Background(), "writer",
		core.AgentContentUpdate{Changes: &core.AgentContentChanges{Description: &description}},
		ContentETag([]byte(raw)), core.Actor{Subject: "token:test"})
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Reason, "手动")
	require.Equal(t, raw, string(fx.disk(t)))
	require.Empty(t, fx.ops.begun)
}
