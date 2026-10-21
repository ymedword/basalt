package app

// 本文件是 `core.AgentAdmin` 组合适配器的验收面（颗粒度文档 §3.2.1：
// internal/app/agent_console_test.go，8 例）。口径来自 §3.1.30 与图 C：
// 内容读写委托、启停先留痕再动状态文件、期望不符不写操作行、缺失 / 损坏状态
// 交给 409 分类、索引失败不回滚、actor 逐列透传。
// This file covers the AgentAdmin adapter: content delegation, lifecycle pre-write
// ordering, conflict semantics, corrupt-state classification and actor propagation.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// consoleAgentContent 是一份可解析的 AGENT.md 原文；内容写用例只在 body 上做替换。
// consoleAgentContent is a parseable AGENT.md fixture.
const consoleAgentContent = `---
name: writer
description: 写手
identity:
  agent_name: writer
provider:
  name: fake
  model: fake-model
---
你是写手。
`

// agentConsoleEnv 是一套真实依赖的夹具：临时工作区 + 临时库 + 共享闸门 + 固定时钟。
// 操作行与状态文件都落在真实实现上，断言的是"磁盘上发生了什么"，不是 fake 的调用记录。
// The fixture wires the real editor, lifecycle store, operation store and index so the
// assertions inspect actual rows and bytes rather than fake call logs.
type agentConsoleEnv struct {
	console   core.AgentAdmin
	root      string
	ops       *storage.ManagementStore
	versions  *storage.ResourceStore
	lifecycle *workspace.LifecycleStore
	gate      *core.AgentGate
	logs      *bytes.Buffer
}

// newAgentConsoleEnv 建夹具；Agent `writer` 的 AGENT.md 已落盘，状态文件尚未建立。
// newAgentConsoleEnv builds the fixture; the agent file exists, the state file does not.
func newAgentConsoleEnv(t *testing.T) *agentConsoleEnv {
	t.Helper()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)
	writeWorkspaceFile(t, root, "agents/writer/AGENT.md", consoleAgentContent)

	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(root, databaseFileName), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	ops := storage.NewManagementStore(db)
	versions := storage.NewResourceStore(db)
	gate := core.NewAgentGate()
	lifecycle := workspace.NewLifecycleStore(root, baselineClock)
	logger, logs := newCapturedLogger()
	editor := workspace.NewAgentEditor(workspace.AgentEditorDeps{
		Root:      root,
		Gate:      gate,
		Ops:       ops,
		Snapshots: storage.NewSnapshotStore(db),
		Registry:  &consoleRegistry{entries: map[string]core.AgentEntry{}},
		Scheduler: consoleSchedules{},
		Validate:  consoleValidator,
		Now:       baselineClock,
	})
	console := NewAgentConsole(AgentConsoleDeps{
		Editor:     editor,
		Lifecycle:  lifecycle,
		Index:      versions,
		Gate:       gate,
		Operations: ops,
		Logger:     logger,
		Now:        baselineClock,
	})
	return &agentConsoleEnv{
		console:   console,
		root:      root,
		ops:       ops,
		versions:  versions,
		lifecycle: lifecycle,
		gate:      gate,
		logs:      logs,
	}
}

// consoleRegistry 是注册表替换面的最小实现；Lookup 让编辑器在回滚路径上取旧条目。
// consoleRegistry is the minimal registry surface with an optional lookup.
type consoleRegistry struct {
	entries map[string]core.AgentEntry
}

func (r *consoleRegistry) Replace(name string, entry core.AgentEntry) {
	r.entries[name] = entry
}

func (r *consoleRegistry) Lookup(name string) (core.AgentEntry, bool) {
	entry, ok := r.entries[name]
	return entry, ok
}

// consoleSchedules 是调度同步面的空实现：本文件不判调度行为。
// consoleSchedules is a no-op reconciler; scheduling is covered elsewhere.
type consoleSchedules struct{}

func (consoleSchedules) ReconcileAgent(context.Context, string, []core.Schedule) error { return nil }

// consoleValidator 是装载期校验的替身：登记成功，段一与清单固定。
// consoleValidator stands in for the load-time validator.
func consoleValidator(_ context.Context, agent string, _ []byte) (core.AgentEntry, string, []core.ManifestEntry, error) {
	return core.AgentEntry{
			Name:       agent,
			Registered: true,
			Profile:    &core.Profile{Name: agent},
		},
		"## 角色",
		[]core.ManifestEntry{{Path: "AGENT.md", SHA256: strings.Repeat("0", 64)}},
		nil
}

// consoleActor 返回本机信任默认 actor。
// consoleActor returns the default local-trust actor.
func consoleActor() core.Actor {
	return core.Actor{
		Subject: core.ActorSubjectLocalTrust,
		Method:  core.AuthMethodLocalTrust,
		Source:  "127.0.0.1:41001",
	}
}

// bootstrapConsoleAgent 走生产路径建立状态文件（disabled / generation=1）。
// bootstrapConsoleAgent creates the state file the way the baseline does.
func bootstrapConsoleAgent(t *testing.T, env *agentConsoleEnv) {
	t.Helper()
	_, err := env.lifecycle.Bootstrap(context.Background(), "writer",
		core.NewOperationID(baselineClock()))
	require.NoError(t, err)
}

// readConsoleState 返回状态文件的当前字节（零写入断言用它逐字节比较）。
// readConsoleState returns the current state-file bytes for zero-write assertions.
func readConsoleState(t *testing.T, root string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "control", "agents", "writer.json"))
	require.NoError(t, err)
	return raw
}

// listConsoleOperations 返回全部管理操作行。
// listConsoleOperations returns every management operation row.
func listConsoleOperations(t *testing.T, store core.OperationStore) []core.OperationRecord {
	t.Helper()
	rows, err := store.ListOperations(context.Background(), core.OperationFilter{})
	require.NoError(t, err)
	return rows
}

// findConsoleOperation 按动作取唯一一行；缺失或重复都直接失败。
// findConsoleOperation returns the single row of one action.
func findConsoleOperation(t *testing.T, store core.OperationStore, action string) core.OperationRecord {
	t.Helper()
	var found []core.OperationRecord
	for _, row := range listConsoleOperations(t, store) {
		if row.Action == action {
			found = append(found, row)
		}
	}
	require.Len(t, found, 1, "action %s 应恰有一行", action)
	return found[0]
}

// decodeConsoleDetail 解析启停操作的 detail_json（键名与恢复侧逐字一致）。
// decodeConsoleDetail decodes the lifecycle detail projection.
func decodeConsoleDetail(t *testing.T, raw []byte) lifecycleOperationDetail {
	t.Helper()
	var detail lifecycleOperationDetail
	require.NoError(t, json.Unmarshal(raw, &detail))
	return detail
}

// TestAgentConsole_ReadContentDelegatesToEditor：读面原样转交编辑器——原文逐字节、
// ETag 与同一份字节对应，且只读不写管理操作行。
func TestAgentConsole_ReadContentDelegatesToEditor(t *testing.T) {
	t.Parallel()
	env := newAgentConsoleEnv(t)

	content, err := env.console.ReadContent(context.Background(), "writer")
	require.NoError(t, err)
	require.Equal(t, consoleAgentContent, content.Content)
	require.Equal(t, workspace.ContentETag([]byte(consoleAgentContent)), content.ETag)
	require.Empty(t, listConsoleOperations(t, env.ops), "只读面不得写管理操作行")

	_, err = env.console.ReadContent(context.Background(), "missing")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

// TestAgentConsole_UpdateContentDelegatesToEditor：写面原样转交编辑器——条件写生效、
// 文件更新、操作行由编辑器按图 D 写下且为 success；成功响应同时投影状态文件真值
// （valid=true / state=disabled），状态文件缺失时写入仍然成功、视图标为无效。
func TestAgentConsole_UpdateContentDelegatesToEditor(t *testing.T) {
	t.Parallel()
	env := newAgentConsoleEnv(t)
	bootstrapConsoleAgent(t, env)

	current, err := env.console.ReadContent(context.Background(), "writer")
	require.NoError(t, err)
	replacement := strings.Replace(consoleAgentContent, "写手", "记者", 1)

	files, err := env.console.UpdateContent(context.Background(), "writer",
		core.AgentContentUpdate{Content: replacement}, current.ETag, consoleActor())
	require.NoError(t, err)
	require.Equal(t, "writer", files.Agent)
	require.NotEmpty(t, files.Files)
	require.True(t, files.Lifecycle.Valid, "状态文件已由基线建立，视图必须标为有效")
	require.NotNil(t, files.Lifecycle.State)
	require.Equal(t, string(core.AgentDisabled), *files.Lifecycle.State)
	require.Empty(t, files.Lifecycle.LifecycleError)

	onDisk, err := os.ReadFile(filepath.Join(env.root, "agents", "writer", "AGENT.md"))
	require.NoError(t, err)
	require.Equal(t, replacement, string(onDisk))

	row := findConsoleOperation(t, env.ops, actionContentUpdate)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, consoleActor().Subject, row.ActorSubject)

	t.Run("missing state file never fails the write", func(t *testing.T) {
		env := newAgentConsoleEnv(t)

		current, err := env.console.ReadContent(context.Background(), "writer")
		require.NoError(t, err)
		files, err := env.console.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: replacement}, current.ETag, consoleActor())
		require.NoError(t, err, "状态文件缺失不得把已生效的写入说成失败")
		require.False(t, files.Lifecycle.Valid)
		require.Nil(t, files.Lifecycle.State)
		require.Equal(t, lifecycleMissingReason, files.Lifecycle.LifecycleError,
			"原因只给自产分类，不拼底层路径")
	})
}

// TestAgentConsole_TransitionEnableWritesOperationAndIndex：启用先写 pending 再动状态
// 文件；成功后回填 success，状态文件、操作行与索引三处的 operation_id 一致。
func TestAgentConsole_TransitionEnableWritesOperationAndIndex(t *testing.T) {
	t.Parallel()
	env := newAgentConsoleEnv(t)
	bootstrapConsoleAgent(t, env)

	rec, err := env.console.Transition(context.Background(), "writer", core.AgentActive,
		core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 1},
		consoleActor())
	require.NoError(t, err)
	require.Equal(t, core.AgentActive, rec.State)
	require.Equal(t, int64(2), rec.Generation)
	require.True(t, strings.HasPrefix(rec.OperationID, "op-"), "操作标识格式固定：%s", rec.OperationID)

	stored, exists, err := env.lifecycle.Read(context.Background(), "writer")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, rec, stored)

	row := findConsoleOperation(t, env.ops, actionEnable)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, lifecycleResultOK, row.ResultCode)
	require.Equal(t, targetKindAgent, row.TargetKind)
	require.Equal(t, "writer", row.TargetID)
	require.Equal(t, rec.OperationID, row.OperationID)
	detail := decodeConsoleDetail(t, row.DetailJSON)
	require.Equal(t, string(core.AgentDisabled), detail.ExpectedState)
	require.Equal(t, int64(1), detail.ExpectedGeneration)
	require.True(t, strings.HasPrefix(row.RequestHash, "sha256:"))
	require.Len(t, row.RequestHash, len("sha256:")+64)

	indexed, valid, errMsg, err := env.versions.LifecycleIndex(context.Background(), "writer")
	require.NoError(t, err)
	require.True(t, valid)
	require.Empty(t, errMsg)
	require.Equal(t, core.AgentActive, indexed.State)
	require.Equal(t, int64(2), indexed.Generation)
	require.Equal(t, rec.OperationID, indexed.OperationID)
}

// TestAgentConsole_TransitionDisableRecordsActor：停用动作写入 `agent.disable`，
// actor 的 subject / method / source 三列逐字来自请求，状态文件代数 +1。
func TestAgentConsole_TransitionDisableRecordsActor(t *testing.T) {
	t.Parallel()
	env := newAgentConsoleEnv(t)
	bootstrapConsoleAgent(t, env)
	_, err := env.console.Transition(context.Background(), "writer", core.AgentActive,
		core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 1},
		consoleActor())
	require.NoError(t, err)

	actor := core.Actor{
		Subject: core.ActorSubjectTokenPrefix + strings.Repeat("a", 16),
		Method:  core.AuthMethodBearer,
		Source:  "10.0.0.8:41234",
	}
	rec, err := env.console.Transition(context.Background(), "writer", core.AgentDisabled,
		core.AgentLifecycleTransition{ExpectedState: core.AgentActive, ExpectedGeneration: 2}, actor)
	require.NoError(t, err)
	require.Equal(t, core.AgentDisabled, rec.State)
	require.Equal(t, int64(3), rec.Generation)

	row := findConsoleOperation(t, env.ops, actionDisable)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, actor.Subject, row.ActorSubject)
	require.Equal(t, actor.Method, row.AuthMethod)
	require.Equal(t, actor.Source, row.Source)
	require.Equal(t, rec.OperationID, row.OperationID)
}

// TestAgentConsole_TransitionPreconditionMismatchWritesNoOperation：期望状态 / 代数不符
// 是"还没有要留痕的变更"——返回冲突、零操作行、状态文件逐字节不变。
func TestAgentConsole_TransitionPreconditionMismatchWritesNoOperation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		target   core.AgentState
		expected core.AgentLifecycleTransition
	}{
		{
			name:     "generation_mismatch",
			target:   core.AgentActive,
			expected: core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 7},
		},
		{
			name:     "state_mismatch",
			target:   core.AgentActive,
			expected: core.AgentLifecycleTransition{ExpectedState: core.AgentActive, ExpectedGeneration: 1},
		},
		{
			// 期望与当前一致、但目标就是当前态：没有可执行的迁移，同样按冲突处理。
			name:     "already_at_target",
			target:   core.AgentDisabled,
			expected: core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newAgentConsoleEnv(t)
			bootstrapConsoleAgent(t, env)
			before := readConsoleState(t, env.root)

			_, err := env.console.Transition(context.Background(), "writer", tc.target, tc.expected, consoleActor())
			require.ErrorIs(t, err, core.ErrLifecycleConflict)
			require.Empty(t, listConsoleOperations(t, env.ops), "期望不符不得写操作行")
			require.Equal(t, before, readConsoleState(t, env.root), "期望不符不得改状态文件")
		})
	}
}

// TestAgentConsole_TransitionMissingOrCorruptStateIsConflict：状态文件缺失 / 损坏是
// 失败关闭——返回 *core.AgentUnavailableError（web 映射 409 AGENT_UNAVAILABLE），
// 零操作行；损坏文件原样保留，不被"修复"覆盖。
func TestAgentConsole_TransitionMissingOrCorruptStateIsConflict(t *testing.T) {
	t.Parallel()

	t.Run("missing", func(t *testing.T) {
		env := newAgentConsoleEnv(t)
		_, err := env.console.Transition(context.Background(), "writer", core.AgentActive,
			core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 1},
			consoleActor())
		var unavailable *core.AgentUnavailableError
		require.ErrorAs(t, err, &unavailable)
		require.Contains(t, unavailable.Error(), "missing")
		require.Empty(t, listConsoleOperations(t, env.ops))
		_, statErr := os.Stat(filepath.Join(env.root, "control", "agents", "writer.json"))
		require.ErrorIs(t, statErr, fs.ErrNotExist)
	})

	t.Run("corrupt", func(t *testing.T) {
		env := newAgentConsoleEnv(t)
		statePath := filepath.Join(env.root, "control", "agents", "writer.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
		corrupt := []byte(`{"schema_version":1,"agent":"writer"`)
		require.NoError(t, os.WriteFile(statePath, corrupt, 0o600))

		_, err := env.console.Transition(context.Background(), "writer", core.AgentActive,
			core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 1},
			consoleActor())
		var unavailable *core.AgentUnavailableError
		require.ErrorAs(t, err, &unavailable)
		require.Contains(t, unavailable.Error(), "corrupt")
		require.Empty(t, listConsoleOperations(t, env.ops))

		raw, readErr := os.ReadFile(statePath)
		require.NoError(t, readErr)
		require.Equal(t, corrupt, raw, "损坏文件不得被覆盖")
	})
}

// failingBeginOperations 让预写失败：满足"预写失败 → 目标零写入"的判据。
// failingBeginOperations forces the pre-write to fail.
type failingBeginOperations struct {
	core.OperationStore
	err error
}

func (s failingBeginOperations) BeginOperation(context.Context, core.OperationRecord) error {
	return s.err
}

// TestAgentConsole_TransitionPreWriteFailureZeroWrites：预写失败 = 拒绝执行（约束 10）——
// 状态文件与索引原样，操作行一行也没有（连 pending 都没落下）。
func TestAgentConsole_TransitionPreWriteFailureZeroWrites(t *testing.T) {
	t.Parallel()
	env := newAgentConsoleEnv(t)
	bootstrapConsoleAgent(t, env)
	before := readConsoleState(t, env.root)

	logger, logs := newCapturedLogger()
	console := NewAgentConsole(AgentConsoleDeps{
		Editor:     nil, // 启停路径不使用编辑器
		Lifecycle:  env.lifecycle,
		Index:      env.versions,
		Gate:       env.gate,
		Operations: failingBeginOperations{OperationStore: env.ops, err: errors.New("数据库不可用")},
		Logger:     logger,
		Now:        baselineClock,
	})

	_, err := console.Transition(context.Background(), "writer", core.AgentActive,
		core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 1},
		consoleActor())
	require.Error(t, err)
	require.Contains(t, err.Error(), "拒绝执行")
	require.Equal(t, before, readConsoleState(t, env.root), "预写失败不得改状态文件")
	require.Empty(t, listConsoleOperations(t, env.ops), "预写失败不得留下任何操作行")

	_, valid, _, indexErr := env.versions.LifecycleIndex(context.Background(), "writer")
	require.NoError(t, indexErr)
	require.False(t, valid)
	require.Contains(t, logs.String(), "启停预写操作记录失败")
}

// failingLifecycleIndex 让索引写失败：状态文件已生效时，失败只记 ERROR，不回滚。
// failingLifecycleIndex makes the rebuildable index write fail.
type failingLifecycleIndex struct {
	err error
}

func (f failingLifecycleIndex) PutLifecycleIndex(context.Context, core.LifecycleRecord, bool, string) error {
	return f.err
}

// TestAgentConsole_TransitionIndexFailureKeepsStateAndLogsError：索引可重建、状态文件是
// 权威——索引失败不回滚状态文件，操作仍回填 success，并以 ERROR 留下可见痕迹。
func TestAgentConsole_TransitionIndexFailureKeepsStateAndLogsError(t *testing.T) {
	t.Parallel()
	env := newAgentConsoleEnv(t)
	bootstrapConsoleAgent(t, env)

	logger, logs := newCapturedLogger()
	console := NewAgentConsole(AgentConsoleDeps{
		Lifecycle:  env.lifecycle,
		Index:      failingLifecycleIndex{err: errors.New("索引库只读")},
		Gate:       env.gate,
		Operations: env.ops,
		Logger:     logger,
		Now:        baselineClock,
	})

	rec, err := console.Transition(context.Background(), "writer", core.AgentActive,
		core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 1},
		consoleActor())
	require.NoError(t, err, "索引失败不得把已生效的启停说成失败")
	require.Equal(t, core.AgentActive, rec.State)

	stored, exists, readErr := env.lifecycle.Read(context.Background(), "writer")
	require.NoError(t, readErr)
	require.True(t, exists)
	require.Equal(t, rec, stored, "索引失败不得回滚状态文件")

	row := findConsoleOperation(t, env.ops, actionEnable)
	require.Equal(t, core.OperationSuccess, row.Status)

	_, valid, _, indexErr := env.versions.LifecycleIndex(context.Background(), "writer")
	require.NoError(t, indexErr)
	require.False(t, valid, "索引写失败后不应出现索引行")
	require.Contains(t, logs.String(), "生命周期索引同步失败")
	require.Contains(t, logs.String(), slog.LevelError.String())
}
