//go:build integration

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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
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
	db        *postgres.DB
	ops       *postgres.ManagementStore
	versions  *postgres.ResourceStore
	lifecycle *workspace.LifecycleStore
	gate      *core.AgentGate
	logs      *bytes.Buffer
}

// consoleEnvParts 把夹具的两组依赖一起交给变参：发布回调归编辑器（写锁内发布），
// 适配器只保留投影 / 生命周期等。
// consoleEnvParts exposes both dependency sets to the variadic option.
type consoleEnvParts struct {
	console *AgentConsoleDeps
	editor  *workspace.AgentEditorDeps
}

// newAgentConsoleEnv 建夹具；Agent `writer` 的 AGENT.md 已落盘，状态文件尚未建立。
// 变参 opts 在构造编辑器与适配器之前改写依赖：需要真实发布链的用例因此不用另装一套
// 夹具；既有调用点不传参，行为不变。
// newAgentConsoleEnv builds the fixture; the agent file exists, the state file does not.
// Variadic opts adjust the deps before construction, so tests that need the real publish
// chain reuse the same fixture; existing call sites pass nothing and are unchanged.
func newAgentConsoleEnv(t *testing.T, opts ...func(*agentConsoleEnv, *consoleEnvParts)) *agentConsoleEnv {
	t.Helper()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)
	writeWorkspaceFile(t, root, "agents/writer/AGENT.md", consoleAgentContent)

	db := pgtest.NewTestDB(t)

	ops := postgres.NewManagementStore(db)
	versions := postgres.NewResourceStore(db)
	gate := core.NewAgentGate()
	lifecycle := workspace.NewLifecycleStore(root, baselineClock)
	logger, logs := newCapturedLogger()
	env := &agentConsoleEnv{
		root:      root,
		db:        db,
		ops:       ops,
		versions:  versions,
		lifecycle: lifecycle,
		gate:      gate,
		logs:      logs,
	}
	editorDeps := workspace.AgentEditorDeps{
		Root:      root,
		Gate:      gate,
		Ops:       ops,
		Snapshots: postgres.NewSnapshotStore(db),
		Registry:  &consoleRegistry{entries: map[string]core.AgentEntry{}},
		Scheduler: consoleSchedules{},
		Validate:  consoleValidator,
		Now:       baselineClock,
	}
	deps := AgentConsoleDeps{
		// 内容写完之后按落盘文件重建绑定投影：夹具给真实现，判据才看得见"投影跟文件走"。
		Root: root,
		Projection: NewBindingProjection(postgres.NewBindingStore(db), versions,
			postgres.NewAgentAdminStore(db), postgres.NewKnowledgeCatalogStore(db), root, logger, baselineClock),
		Lifecycle:  lifecycle,
		Index:      versions,
		Gate:       gate,
		Operations: ops,
		Logger:     logger,
		Now:        baselineClock,
	}
	parts := &consoleEnvParts{console: &deps, editor: &editorDeps}
	for _, opt := range opts {
		opt(env, parts)
	}
	deps.Editor = workspace.NewAgentEditor(editorDeps)
	env.console = NewAgentConsole(deps)
	return env
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
		require.Equal(t, lifecycleMissingNotice, files.Lifecycle.LifecycleError,
			"原因只给自产分类，不拼底层路径")
	})
}

// TestAgentConsole_UpdateContentRebuildsBindingProjection —— 内容补丁可以整块替换
// `resource_bindings`，写入成功之后投影必须跟着**落盘的文件**走（T128 / R4.6）：
// 不重建的话删除闸门与准入会按旧投影反查引用——旧绑定继续挡删除，新绑定不被看见。
// 身份取状态文件；状态文件缺失时只记 ERROR，写入本身仍然成功。
func TestAgentConsole_UpdateContentRebuildsBindingProjection(t *testing.T) {
	t.Parallel()

	t.Run("binding_patch_projects_to_identity", func(t *testing.T) {
		t.Parallel()
		env := newAgentConsoleEnv(t)
		bootstrapConsoleAgent(t, env)
		rec, exists, readErr := env.lifecycle.Read(context.Background(), "writer")
		require.NoError(t, readErr)
		require.True(t, exists)
		store := postgres.NewBindingStore(env.db)
		require.Empty(t, mustBindings(t, store, rec.AgentUID), "基线定义不带绑定块")

		current, err := env.console.ReadContent(context.Background(), "writer")
		require.NoError(t, err)
		_, err = env.console.UpdateContent(context.Background(), "writer", core.AgentContentUpdate{
			Changes: &core.AgentContentChanges{Bindings: []core.BindingChange{{
				ResourceKind: core.BindingModelConnection,
				RefName:      "fake",
				ResourceUID:  bindingModelUID,
			}}},
		}, current.ETag, consoleActor())
		require.NoError(t, err)

		rows := mustBindings(t, store, rec.AgentUID)
		require.Len(t, rows, 1, "投影必须按落盘文件重建")
		require.Equal(t, core.BindingModelConnection, rows[0].Kind)
		require.Equal(t, "fake", rows[0].RefName)
		require.Equal(t, bindingModelUID, rows[0].ResourceUID)
		require.Equal(t, "writer", rows[0].AgentName)

		// 再清空绑定块：投影行随文件消失（否则被摘掉的引用会永远挡住删除）。
		content, err := env.console.ReadContent(context.Background(), "writer")
		require.NoError(t, err)
		_, err = env.console.UpdateContent(context.Background(), "writer", core.AgentContentUpdate{
			Changes: &core.AgentContentChanges{Bindings: []core.BindingChange{}},
		}, content.ETag, consoleActor())
		require.NoError(t, err)
		require.Empty(t, mustBindings(t, store, rec.AgentUID))
	})

	t.Run("missing_state_file_skips_projection_without_failing", func(t *testing.T) {
		t.Parallel()
		env := newAgentConsoleEnv(t)

		current, err := env.console.ReadContent(context.Background(), "writer")
		require.NoError(t, err)
		_, err = env.console.UpdateContent(context.Background(), "writer", core.AgentContentUpdate{
			Changes: &core.AgentContentChanges{Bindings: []core.BindingChange{{
				ResourceKind: core.BindingModelConnection,
				RefName:      "fake",
				ResourceUID:  bindingModelUID,
			}}},
		}, current.ETag, consoleActor())
		require.NoError(t, err, "身份缺失不得把已生效的写入改写成失败")
		require.Contains(t, env.logs.String(), "重建绑定投影跳过")
	})
}

// TestAgentConsole_UpdateContentPublishesBundle：保存即发布（2026-11-09 用户裁决，
// 修订 D-25-10 的"内容写不发布"）——内容写成功后适配器必须用同一个 Agent 名与**真实
// 操作主体**调用发布端口，并把发布结果随响应返回；条件写失败（ETag 过期）时不得发布；
// 发布失败不回滚已生效的写入，但必须留 ERROR、响应带 failed 与固定提示。
// The console content write publishes with the request actor and reports the outcome;
// a failed conditional write publishes nothing, and a failed publish never rewrites a
// committed write.
func TestAgentConsole_UpdateContentPublishesBundle(t *testing.T) {
	t.Parallel()

	t.Run("publishes_after_successful_write", func(t *testing.T) {
		t.Parallel()
		var published []string
		var actors []core.Actor
		env := newAgentConsoleEnv(t, func(_ *agentConsoleEnv, parts *consoleEnvParts) {
			parts.editor.PublishBundle = contentPublishHook(parts.console.Logger,
				func(_ context.Context, agent string, actor core.Actor) (int64, error) {
					published = append(published, agent)
					actors = append(actors, actor)
					return 42, nil
				})
		})
		bootstrapConsoleAgent(t, env)

		actor := consoleActor()
		current, err := env.console.ReadContent(context.Background(), "writer")
		require.NoError(t, err)
		files, err := env.console.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: strings.Replace(consoleAgentContent, "写手", "记者", 1)},
			current.ETag, actor)
		require.NoError(t, err)
		require.Equal(t, []string{"writer"}, published, "保存成功必须发布一次")
		require.Equal(t, []core.Actor{actor}, actors, "发布必须透传本次请求的真实操作主体")
		require.Equal(t, core.AgentPublishStatusPublished, files.Publish.Status)
		require.EqualValues(t, 42, files.Publish.VersionID, "响应必须带回发布版本号")
		require.Empty(t, files.Publish.Error)

		_, err = env.console.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: consoleAgentContent}, "sha256:stale", actor)
		require.Error(t, err, "过期 ETag 必须按条件写失败")
		require.Equal(t, []string{"writer"}, published, "写入未生效时不得发布")
	})

	t.Run("publish_failure_keeps_write_and_logs", func(t *testing.T) {
		t.Parallel()
		env := newAgentConsoleEnv(t, func(_ *agentConsoleEnv, parts *consoleEnvParts) {
			parts.editor.PublishBundle = contentPublishHook(parts.console.Logger,
				func(context.Context, string, core.Actor) (int64, error) {
					return 0, errors.New("共享发布记录不可用")
				})
		})
		bootstrapConsoleAgent(t, env)

		current, err := env.console.ReadContent(context.Background(), "writer")
		require.NoError(t, err)
		replacement := strings.Replace(consoleAgentContent, "写手", "编辑", 1)
		files, err := env.console.UpdateContent(context.Background(), "writer",
			core.AgentContentUpdate{Content: replacement}, current.ETag, consoleActor())
		require.NoError(t, err, "发布失败不得把已生效的写入改写成失败")
		require.Equal(t, "writer", files.Agent)
		// 发布失败必须页面可见：状态 + 固定提示，且不得把底层错误原文（可能含路径）
		// 带进响应。
		require.Equal(t, core.AgentPublishStatusFailed, files.Publish.Status)
		require.NotEmpty(t, files.Publish.Error)
		require.NotContains(t, files.Publish.Error, "共享发布记录不可用",
			"底层错误只进日志，响应只给固定提示")

		onDisk, readErr := os.ReadFile(filepath.Join(env.root, "agents", "writer", "AGENT.md"))
		require.NoError(t, readErr)
		require.Equal(t, replacement, string(onDisk), "发布失败不得回滚工作副本")
		require.Contains(t, env.logs.String(), "publish agent bundle after content update failed")
		require.Contains(t, env.logs.String(), slog.LevelError.String())
	})

	t.Run("save_waits_for_inflight_publish", func(t *testing.T) {
		t.Parallel()
		// 并发顺序：第一次发布被挂起时，第二次保存必须等在门外，不能抢先写盘——
		// 否则"后完成的发布构建了较早写入的内容"会让共享指针倒退。
		// A save must not overtake an in-flight publication of the same agent.
		started := make(chan struct{})
		release := make(chan struct{})
		var calls int32
		env := newAgentConsoleEnv(t, func(_ *agentConsoleEnv, parts *consoleEnvParts) {
			parts.editor.PublishBundle = contentPublishHook(parts.console.Logger,
				func(ctx context.Context, _ string, _ core.Actor) (int64, error) {
					if atomic.AddInt32(&calls, 1) == 1 {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
							return 0, ctx.Err()
						}
					}
					return 1, nil
				})
		})
		bootstrapConsoleAgent(t, env)

		current, err := env.console.ReadContent(context.Background(), "writer")
		require.NoError(t, err)
		staleETag := current.ETag

		firstDone := make(chan error, 1)
		go func() {
			_, err := env.console.UpdateContent(context.Background(), "writer",
				core.AgentContentUpdate{Content: strings.Replace(consoleAgentContent, "写手", "甲", 1)},
				staleETag, consoleActor())
			firstDone <- err
		}()
		<-started // 第一次保存已写盘，发布进行中

		secondDone := make(chan error, 1)
		go func() {
			// 用同一份过期 ETag：串行化生效时它会先等发布结束，再按条件写失败。
			_, err := env.console.UpdateContent(context.Background(), "writer",
				core.AgentContentUpdate{Content: strings.Replace(consoleAgentContent, "写手", "乙", 1)},
				staleETag, consoleActor())
			secondDone <- err
		}()

		select {
		case <-secondDone:
			t.Fatal("第二次保存不得越过在途发布")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		require.NoError(t, <-firstDone)
		require.Error(t, <-secondDone, "发布结束后，过期 ETag 仍必须按条件写失败")
	})
}

// TestAgentConsole_UpdateContentPublishesBundleRecord：接上**真实**发布链之后，保存
// 成功即意味着共享记录与工作副本逐字一致——准入的 `checkDefinitionMatchesBundle`
// 比对的正是这两处字节。2026-11-09 用户报告的症状（控制台改完内容，下一次 Run 报
// 「本地资产包与共享发布记录不一致」）在这里钉成回归用例：旧的"内容写不发布"行为
// 会让本用例在共享指针 / 清单哈希处失败。
// With the real publish chain installed, a committed console save leaves the shared
// record pinning the saved definition bytes — the exact pair admission compares.
func TestAgentConsole_UpdateContentPublishesBundleRecord(t *testing.T) {
	t.Parallel()
	bundleRoot := filepath.Join(t.TempDir(), "bundles")
	env := newAgentConsoleEnv(t, func(env *agentConsoleEnv, parts *consoleEnvParts) {
		parts.editor.PublishBundle = contentPublishHook(parts.console.Logger,
			func(ctx context.Context, agent string, actor core.Actor) (int64, error) {
				return publishWorkspaceAgentBundle(ctx, workspacePublishDeps{
					Root:         env.root,
					BundleRoot:   bundleRoot,
					Versions:     env.versions,
					Operations:   env.ops,
					Payloads:     postgres.NewAssetDistributionStore(env.db),
					PayloadStore: postgres.NewBundlePayloadStore(env.db),
					MaxBytes:     defaultPayloadMaxBytes,
					Actor:        actor,
					Logger:       parts.console.Logger,
					Now:          baselineClock,
				}, agent)
			})
	})
	bootstrapConsoleAgent(t, env)
	// 发布要求生命周期索引里有身份：启用一次，把状态文件的 AgentUID 写进索引
	// （生产装配里这一步由创建 / 启用路径完成）。
	_, err := env.console.Transition(context.Background(), "writer", core.AgentActive,
		core.AgentLifecycleTransition{ExpectedState: core.AgentDisabled, ExpectedGeneration: 1},
		consoleActor())
	require.NoError(t, err)

	current, err := env.console.ReadContent(context.Background(), "writer")
	require.NoError(t, err)
	replacement := strings.Replace(consoleAgentContent, "写手", "终审", 1)
	_, err = env.console.UpdateContent(context.Background(), "writer",
		core.AgentContentUpdate{Content: replacement}, current.ETag, consoleActor())
	require.NoError(t, err)

	uid, _, ok, err := env.versions.AgentIdentity(context.Background(), "writer")
	require.NoError(t, err)
	require.True(t, ok, "启用后索引里必须有身份")
	pointer, err := env.versions.PointerByUID(context.Background(), core.ResourceAgentBundle,
		core.ResourceUID(uid))
	require.NoError(t, err, "保存后必须已发布 agent_bundle 指针")
	require.Nil(t, pointer.DeletedAt)
	version, err := env.versions.VersionByID(context.Background(), pointer.PublishedVersionID)
	require.NoError(t, err)

	dir, err := assets.Resolve(context.Background(), bundleRoot, "writer", version.ContentHash)
	require.NoError(t, err)
	manifest, err := assets.Verify(context.Background(), dir)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(replacement))
	want := hex.EncodeToString(sum[:])
	var got string
	for _, file := range manifest.ConfigFiles {
		if file.Path == "agents/writer/AGENT.md" {
			got = file.SHA256
		}
	}
	require.Equal(t, want, got, "bundle 清单必须钉住保存后的定义字节")
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

	indexed, valid, errMsg, err := env.versions.ReadLifecycleIndex(context.Background(), "writer")
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

	_, valid, _, indexErr := env.versions.ReadLifecycleIndex(context.Background(), "writer")
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

// ReadLifecycleIndex 与写面同一个失败面：索引读不到时同样按失败上报，不回落到零值。
// ReadLifecycleIndex fails like the write surface; it never falls back to a zero value.
func (f failingLifecycleIndex) ReadLifecycleIndex(context.Context, string) (core.LifecycleRecord, bool, string, error) {
	return core.LifecycleRecord{}, false, "", f.err
}

// SetLifecycleBarrier 与 PutLifecycleIndex 同一个失败面：管理面归档在屏障写不进时同样
// 必须停手并留下 pending 行（FR-24-41 的 fail-closed）。
// SetLifecycleBarrier fails the same way, so the archive barrier is fail-closed too.
func (f failingLifecycleIndex) SetLifecycleBarrier(context.Context, string, string, time.Time) (bool, error) {
	return false, f.err
}

// RestoreLifecycleBarrier 同上。
// RestoreLifecycleBarrier mirrors the failing surface above.
func (f failingLifecycleIndex) RestoreLifecycleBarrier(context.Context, core.LifecycleRecord, string, int64) (bool, error) {
	return false, f.err
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

	_, valid, _, indexErr := env.versions.ReadLifecycleIndex(context.Background(), "writer")
	require.NoError(t, indexErr)
	require.False(t, valid, "索引写失败后不应出现索引行")
	require.Contains(t, logs.String(), "生命周期索引同步失败")
	require.Contains(t, logs.String(), slog.LevelError.String())
}
