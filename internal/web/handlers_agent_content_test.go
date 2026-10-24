package web

// 本文件覆盖 Agent 配置内容端点（模块卡 §3.2.1 handlers_agent_content_test.go 的
// 20 例口径）：读面与 ETag、条件写的成功路径（整份替换 / 补丁）、前置条件矩阵
// （缺失 / 弱 / 通配 / 形状非法 / 不匹配）、请求体矩阵、失败分类（EDIT_UNSUPPORTED /
// 校验失败 / 快照失败 / 原子写失败 / 预写失败）、调度同步失败后的回滚、并发保存、
// 认证与文案脱敏。
//
// 夹具选择：内容比较、补丁、快照组装、原子写与回滚都走**真实的** workspace 编辑器与
// 生命周期仓储（`fileAgentConsole` 只在测试侧重放 internal/app 的组装与生命周期
// 投影——`package web` 不能导入 app：app 依赖 web，导入即环）；仓储侧只替换可注入
// 的操作 / 快照 / 注册表 / 调度四个端口，以便逐个注入失败。
// The fixture runs the real workspace editor and lifecycle store; only the injectable
// ports are faked. The adapter is replayed test-side because package web cannot import
// package app (app depends on web).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// contentAgentName 是用例里的 Agent 目录名。
	contentAgentName = "reporter"
	// contentAgentDoc 是内容端点的原文夹具：带注释、映射块、序列与正文，足以断言
	// "补丁只改目标行、其余字节不动"。
	// contentAgentDoc carries a comment, a map block, a sequence and a body, which is
	// what the patch cases need to prove byte preservation.
	contentAgentDoc = `---
name: reporter
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
---

你是周报助手。
`
)

// contentNow 是内容写用例的固定时钟（操作标识与状态文件时间戳都取它）。
// contentNow is the fixed clock for operation ids and state-file timestamps.
var contentNow = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)

// contentClock 供 workspace 的注入点使用。
// contentClock adapts the fixed instant to the injectable clock shape.
func contentClock() time.Time { return contentNow }

// contentValidator 是装载期校验的测试替身：签名与生产注入点一致，恒返回已注册条目
// （内容校验本身由 workspace 的 22 例白盒用例判卷，这里只判 HTTP 分类与写入编排）。
// contentValidator always returns a registered entry; content validation itself is
// covered by the workspace white-box suite, so these tests judge the HTTP mapping.
func contentValidator(_ context.Context, agent string, _ []byte) (core.AgentEntry, string, []core.ManifestEntry, error) {
	return core.AgentEntry{
			Name:       agent,
			Registered: true,
			Profile:    &core.Profile{Name: agent},
		}, "你是" + agent + "。",
		[]core.ManifestEntry{{Path: "AGENT.md", SHA256: strings.Repeat("0", 64)}}, nil
}

// contentRejectingValidator 模拟"候选未通过装载期校验"：与生产装载器同形返回
// 类型化拒绝（英文合并文本只在 Error() 里），由 web 层映射为 400 BAD_REQUEST。
// contentRejectingValidator mimics a candidate refused by the load-time gate with the
// same typed rejection the production loader returns.
func contentRejectingValidator(_ context.Context, agent string, _ []byte) (core.AgentEntry, string, []core.ManifestEntry, error) {
	return core.AgentEntry{Name: agent}, "", nil, &core.AgentDefinitionRejectedError{
		Kind:   core.AgentRejectProviderNotRegistered,
		Detail: "main",
	}
}

// fileAgentConsole 是 `core.AgentAdmin` 在测试侧的组装：内容读写委托给真实的
// workspace 编辑器，成功之后补上生命周期投影（与 internal/app 的同名适配器同构；
// web 不能导入 app，故在测试侧重放，见文件头说明）。
// fileAgentConsole replays the app-side adapter test-side: the real editor plus the
// lifecycle projection, which the editor itself does not fill.
type fileAgentConsole struct {
	editor    *workspace.AgentEditor
	lifecycle *workspace.LifecycleStore
}

var _ core.AgentAdmin = (*fileAgentConsole)(nil)

// ReadContent 原样委托：原文与 ETag 出自编辑器同一次读取。
// ReadContent delegates so content and ETag come from the same read.
func (c *fileAgentConsole) ReadContent(ctx context.Context, agent string) (core.AgentContent, error) {
	return c.editor.ReadContent(ctx, agent)
}

// UpdateContent 委托条件写后补生命周期投影。
// UpdateContent delegates the conditional write and fills the lifecycle projection.
func (c *fileAgentConsole) UpdateContent(ctx context.Context, agent string,
	update core.AgentContentUpdate, ifMatch string, actor core.Actor,
) (core.AgentFiles, error) {
	files, err := c.editor.UpdateContent(ctx, agent, update, ifMatch, actor)
	if err != nil {
		return core.AgentFiles{}, err
	}
	return c.withLifecycle(ctx, files), nil
}

// Transition 属启停端点；内容用例触达它即为实现走错分支。
// Transition belongs to the lifecycle endpoints; a content test reaching it is a bug.
func (c *fileAgentConsole) Transition(context.Context, string, core.AgentState,
	core.AgentLifecycleTransition, core.Actor,
) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errors.New("内容端点不得调用 Transition")
}

// withLifecycle 与 internal/app 的投影同一口径：缺失 / 损坏记 valid=false 并给固定
// 原因，读取失败也**不**把已生效的写入改写成错误。
// withLifecycle mirrors the app-side projection: a missing or corrupt state file is
// reported as valid=false, and a read failure never turns a successful write into an
// error.
func (c *fileAgentConsole) withLifecycle(ctx context.Context, files core.AgentFiles) core.AgentFiles {
	if c.lifecycle == nil {
		return files
	}
	rec, exists, err := c.lifecycle.Read(context.WithoutCancel(ctx), files.Agent)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		files.Lifecycle = core.AgentLifecycleView{LifecycleError: "lifecycle state file missing; restore it before toggling"}
	case err != nil:
		files.Lifecycle = core.AgentLifecycleView{LifecycleError: "lifecycle state file corrupt; restore it from a trusted copy"}
	case !exists:
		files.Lifecycle = core.AgentLifecycleView{LifecycleError: "lifecycle state file missing; restore it before toggling"}
	default:
		state := string(rec.State)
		files.Lifecycle = core.AgentLifecycleView{State: &state, Valid: true}
	}
	return files
}

// contentOpsFake 记录管理操作的预写与终态；beginErr 让预写在动盘之前失败。
// contentOpsFake records pre-writes and finishes; beginErr fails the pre-write.
type contentOpsFake struct {
	mu       sync.Mutex
	begun    []core.OperationRecord
	finished []contentFinishRecord
	beginErr error
	// onBegin 在预写成功返回前被调用一次，供用例在"留痕已落、目标未动"的瞬间
	// 观察磁盘与快照（约束 10 的顺序判据）；调用发生在锁外，hook 内可安全读盘。
	// onBegin runs once right after a successful pre-write, outside the lock, so the
	// case can observe the disk in the window constraint 10 defines.
	onBegin func()
}

// contentFinishRecord 是一次终态回填的入参快照。
// contentFinishRecord snapshots one FinishOperation call.
type contentFinishRecord struct {
	operationID string
	status      core.OperationStatus
	resultCode  string
	message     string
	detail      []byte
}

var _ core.OperationStore = (*contentOpsFake)(nil)

// BeginOperation 记录预写；注入失败时命令行与磁盘都不该被触达。
// BeginOperation records the pre-write; the injected failure must stop the change.
func (f *contentOpsFake) BeginOperation(_ context.Context, rec core.OperationRecord) error {
	f.mu.Lock()
	if f.beginErr != nil {
		err := f.beginErr
		f.mu.Unlock()
		return err
	}
	f.begun = append(f.begun, rec)
	hook := f.onBegin
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// FinishOperation 记录终态回填（含 detail_json，供脱敏断言）。
// FinishOperation records the terminal state, detail_json included for redaction checks.
func (f *contentOpsFake) FinishOperation(_ context.Context, operationID string,
	status core.OperationStatus, resultCode, errorMessage string, detail []byte, _ time.Time,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, contentFinishRecord{
		operationID: operationID, status: status,
		resultCode: resultCode, message: errorMessage,
		detail: append([]byte(nil), detail...),
	})
	return nil
}

// GetOperation 在内容用例里不该被调用。
// GetOperation is not exercised by the content endpoint.
func (f *contentOpsFake) GetOperation(context.Context, string) (core.OperationRecord, error) {
	return core.OperationRecord{}, core.ErrOperationNotFound
}

// ListOperations 在内容用例里不该被调用。
// ListOperations is not exercised by the content endpoint.
func (f *contentOpsFake) ListOperations(context.Context, core.OperationFilter) ([]core.OperationRecord, error) {
	return nil, nil
}

// PendingOperations 在内容用例里不该被调用。
// PendingOperations belongs to startup recovery.
func (f *contentOpsFake) PendingOperations(context.Context) ([]core.OperationRecord, error) {
	return nil, nil
}

// RecoverOperation 属启动恢复，内容用例触达它即为实现走错分支。
// RecoverOperation belongs to startup recovery.
func (f *contentOpsFake) RecoverOperation(context.Context, string, core.OperationStatus, string, []byte, time.Time) error {
	return errors.New("恢复路径不属内容写测试")
}

// watchBegin 注册预写成功的观察钩子（在锁外执行）。
// watchBegin registers the post-pre-write observation hook, run outside the lock.
func (f *contentOpsFake) watchBegin(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onBegin = hook
}

// failBegin 注入预写失败。
// failBegin makes the next pre-write fail.
func (f *contentOpsFake) failBegin(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginErr = err
}

// begunRecords 返回预写快照。
// begunRecords returns a snapshot of the pre-writes.
func (f *contentOpsFake) begunRecords() []core.OperationRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.OperationRecord(nil), f.begun...)
}

// finishedRecords 返回终态回填快照。
// finishedRecords returns a snapshot of the terminal writes.
func (f *contentOpsFake) finishedRecords() []contentFinishRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]contentFinishRecord(nil), f.finished...)
}

// contentSnapshotsFake 记录快照落库，可注入保存失败。
// contentSnapshotsFake records snapshot saves and can fail on demand.
type contentSnapshotsFake struct {
	mu    sync.Mutex
	saved []core.ConfigSnapshot
	err   error
	// onSave 在快照记录之后、返回之前被调用一次（锁外）：此刻磁盘必须还是写前字节。
	// onSave runs once after the snapshot is recorded and before the write, outside the
	// lock, when the disk must still hold the pre-write bytes.
	onSave func()
}

var _ core.SnapshotStore = (*contentSnapshotsFake)(nil)

// SaveSnapshot 记录快照；注入失败时返回错误（写盘之前必须停下）。
// SaveSnapshot records the snapshot; the injected failure stops the write.
func (f *contentSnapshotsFake) SaveSnapshot(_ context.Context, snap *core.ConfigSnapshot) error {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return err
	}
	f.saved = append(f.saved, *snap)
	hook := f.onSave
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// watchSave 注册快照保存后的观察钩子（锁外执行）。
// watchSave registers the post-save observation hook, run outside the lock.
func (f *contentSnapshotsFake) watchSave(hook func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onSave = hook
}

// failWith 注入快照保存失败。
// failWith makes every snapshot save fail.
func (f *contentSnapshotsFake) failWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// savedCount 返回成功落库的快照数。
// savedCount returns how many snapshots were persisted.
func (f *contentSnapshotsFake) savedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.saved)
}

// contentRegistryFake 同时满足 EntryReplacer 与 entryLookup（回滚取旧条目）。
// contentRegistryFake satisfies both EntryReplacer and entryLookup.
type contentRegistryFake struct {
	mu      sync.Mutex
	entries map[string]core.AgentEntry
}

var _ workspace.EntryReplacer = (*contentRegistryFake)(nil)

// Replace 原子替换一条注册表条目。
// Replace swaps one entry atomically.
func (f *contentRegistryFake) Replace(name string, entry core.AgentEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[name] = entry
}

// Lookup 读取当前条目（回滚时用于恢复写前快照）。
// Lookup returns the current entry so rollback can restore the pre-write snapshot.
func (f *contentRegistryFake) Lookup(name string) (core.AgentEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.entries[name]
	return entry, ok
}

// contentSchedulesFake 记录按 Agent 的调度同步；failures 次调用返回注入错误。
// contentSchedulesFake records reconciliations; the injected failure fires once so the
// rollback-side reconcile can succeed.
type contentSchedulesFake struct {
	mu       sync.Mutex
	calls    int
	failures int
	err      error
}

var _ workspace.ScheduleReconciler = (*contentSchedulesFake)(nil)

// ReconcileAgent 记录一次同步并按注入次数失败。
// ReconcileAgent records one reconciliation and fails while failures remain.
func (f *contentSchedulesFake) ReconcileAgent(context.Context, string, []core.Schedule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures > 0 {
		f.failures--
		return f.err
	}
	return nil
}

// failOnce 注入"下一次同步失败"：回滚时的第二次调用必须能成功，否则操作会落 unknown。
// failOnce fails exactly the next reconcile so the rollback reconcile can succeed.
func (f *contentSchedulesFake) failOnce(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = 1
	f.err = err
}

// callCount 返回同步调用次数。
// callCount returns the number of reconciliations.
func (f *contentSchedulesFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// contentAdminSpy 是形状校验与文案脱敏用例的记录型端口：一个请求打到适配器时，
// 说明形状校验（缺 If-Match / 弱 / 通配 / 畸形）**没有**在入口拦下。
// contentAdminSpy records adapter calls so shape failures can prove zero reach it.
type contentAdminSpy struct {
	mu        sync.Mutex
	reads     int
	writes    int
	err       error
	files     core.AgentFiles
	lastActor core.Actor
}

var _ core.AgentAdmin = (*contentAdminSpy)(nil)

// ReadContent 计数并返回固定原文。
// ReadContent counts the call and returns a fixed content.
func (s *contentAdminSpy) ReadContent(context.Context, string) (core.AgentContent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return core.AgentContent{}, s.err
	}
	return core.AgentContent{Content: "x", ETag: workspace.ContentETag([]byte("x"))}, nil
}

// UpdateContent 计数并返回预置结果。
// UpdateContent counts the call and returns the preset result.
func (s *contentAdminSpy) UpdateContent(_ context.Context, _ string,
	_ core.AgentContentUpdate, _ string, actor core.Actor,
) (core.AgentFiles, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	s.lastActor = actor
	if s.err != nil {
		return core.AgentFiles{}, s.err
	}
	return s.files, nil
}

// Transition 属启停端点；内容用例触达它即为实现走错分支。
// Transition belongs to the lifecycle endpoints.
func (s *contentAdminSpy) Transition(context.Context, string, core.AgentState,
	core.AgentLifecycleTransition, core.Actor,
) (core.LifecycleRecord, error) {
	return core.LifecycleRecord{}, errors.New("内容端点不得调用 Transition")
}

// readCount / writeCount 返回调用次数。
// readCount and writeCount expose the call counters.
func (s *contentAdminSpy) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// writeCount 返回写调用次数。
// writeCount exposes the write counter.
func (s *contentAdminSpy) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// actorOf 返回最近一次写的 actor。
// actorOf returns the actor of the last write.
func (s *contentAdminSpy) actorOf() core.Actor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastActor
}

// contentEnv 是一套落在临时工作区里的内容端点夹具：真实编辑器 + 真实状态文件仓储，
// 其余端口为可注入失败的假实现。
// contentEnv wires the real editor and lifecycle store on a temp workspace.
type contentEnv struct {
	root      string
	gate      *core.AgentGate
	ops       *contentOpsFake
	snapshots *contentSnapshotsFake
	registry  *contentRegistryFake
	scheduler *contentSchedulesFake
	lifecycle *workspace.LifecycleStore
	runs      *fakeRunService
	// admin 是服务端使用的同一个适配器实例：结构化配置用例要拿它重建一台带
	// AgentConfigService 的服务端，同时保持 /content 与 /config 读的是同一份装配。
	// admin is the same adapter the server holds, so the config cases can rebuild a
	// server with an extra port while keeping the real editor.
	admin  *fileAgentConsole
	server *Server
}

// newContentEnv 组装默认夹具（内容校验恒通过）。
// newContentEnv assembles the default fixture.
func newContentEnv(t *testing.T) *contentEnv {
	t.Helper()
	return newContentEnvWith(t, nil)
}

// newContentEnvWith 允许用例替换编辑器依赖（例如注入拒绝型校验器）。
// newContentEnvWith lets a case replace editor dependencies before assembly.
func newContentEnvWith(t *testing.T, tune func(*workspace.AgentEditorDeps)) *contentEnv {
	t.Helper()
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	writeAgentDoc(t, root, contentAgentName, contentAgentDoc)

	gate := core.NewAgentGate()
	env := &contentEnv{
		root:      root,
		gate:      gate,
		ops:       &contentOpsFake{},
		snapshots: &contentSnapshotsFake{},
		registry:  &contentRegistryFake{entries: map[string]core.AgentEntry{}},
		scheduler: &contentSchedulesFake{},
		runs:      &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}},
	}
	env.lifecycle = workspace.NewLifecycleStore(root, contentClock)

	deps := workspace.AgentEditorDeps{
		Root:      root,
		Gate:      gate,
		Ops:       env.ops,
		Snapshots: env.snapshots,
		Registry:  env.registry,
		Scheduler: env.scheduler,
		Validate:  contentValidator,
		Now:       contentClock,
	}
	if tune != nil {
		tune(&deps)
	}
	admin := &fileAgentConsole{
		editor:    workspace.NewAgentEditor(deps),
		lifecycle: env.lifecycle,
	}
	env.admin = admin
	env.server = newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = admin
		o.Runs = env.runs
	})
	return env
}

// newFileAgentConsole 按模块卡 §4.3.2 的写法构造一个只服务内容端点的适配器：
// 真实编辑器 + 真实状态文件仓储，不带任何可注入失败的假端口。
// newFileAgentConsole builds the minimal adapter the module card's frozen case asks for.
func newFileAgentConsole(t *testing.T, root string) core.AgentAdmin {
	t.Helper()
	gate := core.NewAgentGate()
	return &fileAgentConsole{
		editor: workspace.NewAgentEditor(workspace.AgentEditorDeps{
			Root:      root,
			Gate:      gate,
			Ops:       &contentOpsFake{},
			Snapshots: &contentSnapshotsFake{},
			Registry:  &contentRegistryFake{entries: map[string]core.AgentEntry{}},
			Scheduler: &contentSchedulesFake{},
			Validate:  contentValidator,
			Now:       contentClock,
		}),
		lifecycle: workspace.NewLifecycleStore(root, contentClock),
	}
}

// writeAgentDoc 在 agents/<name>/AGENT.md 落一份原文（自动创建父目录）。
// writeAgentDoc writes the raw AGENT.md under the agent directory.
func writeAgentDoc(t *testing.T, root, name, doc string) {
	t.Helper()
	dir := filepath.Join(root, "agents", name)
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(doc), 0o600))
}

// writeAgent 按模块卡 §4.3.2 的夹具写一份最小配置（模型名由用例给出）。
// writeAgent writes the minimal document the frozen case uses.
func writeAgent(t *testing.T, root, name, model string) {
	t.Helper()
	writeAgentDoc(t, root, name,
		"---\nname: "+name+"\nprovider:\n  name: \"mock\"\n  model: \""+model+"\"\n---\n\n你是 "+name+"。\n")
}

// agentDocPath 给出 AGENT.md 的绝对路径（用于读盘与残留检查）。
// agentDocPath returns the absolute AGENT.md path.
func agentDocPath(root, name string) string {
	return filepath.Join(root, "agents", name, "AGENT.md")
}

// readAgentDoc 读取磁盘上的 AGENT.md 原文。
// readAgentDoc reads the raw AGENT.md bytes from disk.
func readAgentDoc(t *testing.T, root, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(agentDocPath(root, name))
	require.NoError(t, err)
	return raw
}

// bootstrapContentAgent 建立生命周期基线：内容写成功后的视图要求状态文件的真值。
// bootstrapContentAgent creates the lifecycle baseline the success view projects.
func bootstrapContentAgent(t *testing.T, env *contentEnv) {
	t.Helper()
	_, err := env.lifecycle.Bootstrap(context.Background(), contentAgentName, core.NewOperationID(contentNow))
	require.NoError(t, err)
}

// contentPath 给出内容端点路径。
// contentPath builds the content endpoint path.
func contentPath(name string) string {
	return "/api/v1/agents/" + name + "/content"
}

// contentGETFor 构造带凭据的读请求。
// contentGETFor builds an authenticated GET.
func contentGETFor(name string) *http.Request {
	return withBearer(adminRequest(http.MethodGet, contentPath(name), ""), testToken)
}

// contentGET 构造读当前夹具 Agent 的请求。
// contentGET reads the fixture agent.
func contentGET() *http.Request {
	return contentGETFor(contentAgentName)
}

// contentPUT 构造写请求；ifMatch 为空串表示**不带** If-Match 头。
// contentPUT builds a write request; an empty ifMatch leaves the header absent.
func contentPUT(name, body, ifMatch string) *http.Request {
	req := adminRequest(http.MethodPut, contentPath(name), body)
	withBearer(req, testToken)
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	return req
}

// contentBody 把整份原文封成线上请求体。
// contentBody wraps the whole document into the wire shape.
func contentBody(t *testing.T, content string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"content": content})
	require.NoError(t, err)
	return string(raw)
}

// currentETag 通过真实读端点取当前 ETag（与页面同一来源）。
// currentETag reads the current ETag through the real endpoint.
func currentETag(t *testing.T, env *contentEnv) string {
	t.Helper()
	rec := serveAdmin(env.server, contentGET())
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Header().Get("ETag")
}

// strongETagOf 拼一个形状合法的强 ETag（不保证与磁盘一致）。
// strongETagOf builds a well-formed strong ETag that need not match the disk.
func strongETagOf(seed byte) string {
	return `"sha256:` + strings.Repeat(string([]byte{seed}), 64) + `"`
}

// tokenActorSubject 复算 token 身份指纹（与认证闸门同一算法）。
// tokenActorSubject recomputes the token identity fingerprint.
func tokenActorSubject(token string) string {
	sum := sha256.Sum256([]byte(token))
	return core.ActorSubjectTokenPrefix + hex.EncodeToString(sum[:])[:16]
}

// decodeAgentContentView 解析读面成功信封。
// decodeAgentContentView decodes the read success envelope.
func decodeAgentContentView(t *testing.T, rec *httptest.ResponseRecorder) AgentContentView {
	t.Helper()
	var envelope struct {
		Code string           `json:"code"`
		Data AgentContentView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, codeOK, envelope.Code)
	return envelope.Data
}

// decodeAgentFilesView 解析写面成功信封。
// decodeAgentFilesView decodes the write success envelope.
func decodeAgentFilesView(t *testing.T, rec *httptest.ResponseRecorder) AgentFilesView {
	t.Helper()
	var envelope struct {
		Code string         `json:"code"`
		Data AgentFilesView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, codeOK, envelope.Code)
	return envelope.Data
}

// TestAgentContent_GetReturnsContentAndETag：读面返回逐字节原文与强 ETag，ETag 同时
// 出现在响应头与 `data.etag`；只读观测不写管理操作行（FR6）。
// 逆改点：ETag 按归一化内容计算 → 原文含 CRLF / BOM 变体时红；读路径写操作行 → 末断言红。
func TestAgentContent_GetReturnsContentAndETag(t *testing.T) {
	env := newContentEnv(t)

	rec := serveAdmin(env.server, contentGET())
	require.Equal(t, http.StatusOK, rec.Code)

	view := decodeAgentContentView(t, rec)
	require.Equal(t, contentAgentDoc, view.Content, "原文必须逐字节返回")
	require.Equal(t, workspace.ContentETag([]byte(contentAgentDoc)), view.ETag)
	require.Equal(t, view.ETag, rec.Header().Get("ETag"), "响应头与 data.etag 必须同值")
	require.Empty(t, env.ops.begunRecords(), "只读观测不写管理操作记录")
}

// TestAgentContent_ETagFormat：ETag 是带双引号的强 ETag 形状
// `"sha256:<64 位小写十六进制>"`，读面与写面同一口径。
// 逆改点：去掉引号 / 用大写十六进制 / 截断摘要 → 正则断言红。
func TestAgentContent_ETagFormat(t *testing.T) {
	env := newContentEnv(t)
	pattern := regexp.MustCompile(`^"sha256:[0-9a-f]{64}"$`)

	rec := serveAdmin(env.server, contentGET())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Regexp(t, pattern, rec.Header().Get("ETag"))
	require.Regexp(t, pattern, decodeAgentContentView(t, rec).ETag)

	newDoc := contentAgentDoc + "补充一行。\n"
	rec = serveAdmin(env.server, contentPUT(contentAgentName, contentBody(t, newDoc), currentETag(t, env)))
	require.Equal(t, http.StatusOK, rec.Code)
	header := rec.Header().Get("ETag")
	require.Regexp(t, pattern, header)
	require.Equal(t, workspace.ContentETag([]byte(newDoc)), header, "新 ETag 必须按新字节计算")
	require.Equal(t, header, decodeAgentFilesView(t, rec).ETag, "写面头部与 data.etag 必须同值")
}

// TestAgentContent_GetUnknownAgent_NotFound：目录或 AGENT.md 缺失映射 404，文案点名
// 查不到的名字，**不得**把缺失当成空内容。
// 逆改点：缺失回落成 200 + 空串 → 红；文案泄露绝对路径 → 脱敏断言红（见 19 例）。
func TestAgentContent_GetUnknownAgent_NotFound(t *testing.T) {
	env := newContentEnv(t)

	rec := serveAdmin(env.server, contentGETFor("ghost"))
	require.Equal(t, http.StatusNotFound, rec.Code)

	envelope := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeNotFound, envelope.Code)
	require.Contains(t, envelope.Message, "ghost", "404 文案必须点名查不到的 Agent")
	require.Nil(t, envelope.Data, "失败信封不带 data")
}

// TestAgentContent_StaleETagLeavesFileUntouched 按模块卡 §4.3.2 **原样落地**：
// 旧 ETag 保存 → 412 且磁盘逐字节不变。
// 逆改点：比较挪到写盘之后 / 缺失时不比较 → 412 变 200 且前后字节不等。
func TestAgentContent_StaleETagLeavesFileUntouched(t *testing.T) {
	root := t.TempDir()
	writeAgent(t, root, "reporter", "请在此填写模型名")

	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = newFileAgentConsole(t, root)
	})

	oldETag := `"sha256:` + strings.Repeat("0", 64) + `"`
	before, err := os.ReadFile(filepath.Join(root, "agents", "reporter", "AGENT.md"))
	require.NoError(t, err)

	req := adminRequest(http.MethodPut, "/api/v1/agents/reporter/content",
		`{"content":"---\nname: reporter\n---\n"}`)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("If-Match", oldETag)
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	after, err := os.ReadFile(filepath.Join(root, "agents", "reporter", "AGENT.md"))
	require.NoError(t, err)
	require.Equal(t, before, after, "412 必须磁盘零写入")
}

// TestAgentContent_PutWholeFile_Success：整份替换成功返回文件集合视图、新 ETag 与
// 生命周期投影；操作行先 pending 后 success，actor 只落 token 指纹；内容写不触发运行。
// 逆改点：成功路径不写操作行 / actor 落明文 / 触碰 Runs → 对应断言红。
func TestAgentContent_PutWholeFile_Success(t *testing.T) {
	env := newContentEnv(t)
	bootstrapContentAgent(t, env)
	oldETag := currentETag(t, env)

	newDoc := contentAgentDoc + "新增一行。\n"
	rec := serveAdmin(env.server, contentPUT(contentAgentName, contentBody(t, newDoc), oldETag))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	view := decodeAgentFilesView(t, rec)
	require.Equal(t, contentAgentName, view.Agent)
	require.Equal(t, workspace.ContentETag([]byte(newDoc)), view.ETag)
	require.Equal(t, view.ETag, rec.Header().Get("ETag"))
	require.Equal(t, contentAgentName, view.Lifecycle.Agent)
	require.True(t, view.Lifecycle.Valid, "基线已建立，视图必须给出有效状态")
	require.NotNil(t, view.Lifecycle.State)
	require.Equal(t, string(core.AgentDisabled), *view.Lifecycle.State)
	require.Empty(t, view.Lifecycle.LifecycleError)
	require.Len(t, view.Files, 1, "受管清单只含 Agent 目录根下的普通文件")
	require.Equal(t, "AGENT.md", view.Files[0].Path)
	require.Equal(t, int64(len(newDoc)), view.Files[0].Size)

	require.Equal(t, newDoc, string(readAgentDoc(t, env.root, contentAgentName)))

	begun := env.ops.begunRecords()
	require.Len(t, begun, 1, "写入必须先落一条 pending 操作行")
	require.Equal(t, "agent.content.update", begun[0].Action)
	require.Equal(t, "agent", begun[0].TargetKind)
	require.Equal(t, contentAgentName, begun[0].TargetID)
	require.Equal(t, core.OperationPending, begun[0].Status)
	require.Equal(t, oldETag, begun[0].ExpectedETag, "操作行记录写前 ETag，供恢复查证")
	require.Equal(t, tokenActorSubject(testToken), begun[0].ActorSubject)
	require.Equal(t, core.AuthMethodBearer, begun[0].AuthMethod)
	require.NotEmpty(t, begun[0].Source)
	require.NotContains(t, begun[0].ActorSubject, testToken, "actor 只落指纹，不落明文")

	finished := env.ops.finishedRecords()
	require.Len(t, finished, 1)
	require.Equal(t, core.OperationSuccess, finished[0].status)
	require.Equal(t, "OK", finished[0].resultCode)
	require.Equal(t, env.ops.begunRecords()[0].OperationID, finished[0].operationID)

	require.Empty(t, env.runs.requests(), "内容写不得触发运行")
	require.Equal(t, 1, env.snapshots.savedCount(), "写盘之前必须先落配置快照")
}

// TestAgentContent_PutPatch_Success：结构化补丁只改目标行，注释、未改字段与正文
// 逐字节保留；成功后的 ETag 按打过补丁的字节计算。
// 逆改点：补丁整份重新生成 → 注释行 / 序列块断言红。
func TestAgentContent_PutPatch_Success(t *testing.T) {
	env := newContentEnv(t)
	oldETag := currentETag(t, env)

	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		`{"changes":{"provider":{"model":"mock-pro"}}}`, oldETag))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	expected := strings.Replace(contentAgentDoc, `  model: "mock-model"`, `  model: "mock-pro"`, 1)
	require.Equal(t, expected, string(readAgentDoc(t, env.root, contentAgentName)))
	require.Contains(t, expected, "# provider 段前的注释必须保留", "补丁不得丢注释")
	require.Contains(t, expected, "你是周报助手。", "补丁不得动正文")

	view := decodeAgentFilesView(t, rec)
	require.Equal(t, workspace.ContentETag([]byte(expected)), view.ETag)
	require.Equal(t, "AGENT.md", view.Files[0].Path)
}

// TestAgentContentChanges_TemperatureTriState：温度补丁三态——省略 = 不修改，
// 数字（含 0）= 写入，`null` = 清除（回到供应商缺省）。省略**不得**被读成清除。
// 对应契约：agent-md §2.3 `changes.provider.temperature`。
func TestAgentContentChanges_TemperatureTriState(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantClear bool
		wantValue *float64
	}{
		{
			name: "omitted leaves unchanged",
			body: `{"changes":{"provider":{"model":"m"}}}`,
		},
		{
			name:      "explicit null clears",
			body:      `{"changes":{"provider":{"temperature":null}}}`,
			wantClear: true,
		},
		{
			name:      "explicit zero is a value",
			body:      `{"changes":{"provider":{"temperature":0}}}`,
			wantValue: ptrFloat(0),
		},
		{
			name:      "number sets value",
			body:      `{"changes":{"provider":{"temperature":0.7}}}`,
			wantValue: ptrFloat(0.7),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body contentUpdateBody
			require.NoError(t, json.Unmarshal([]byte(tc.body), &body))
			require.NotNil(t, body.Changes)
			changes, err := body.Changes.toCore()
			require.NoError(t, err)
			require.NotNil(t, changes.Provider)
			require.Equal(t, tc.wantClear, changes.Provider.ClearTemperature)
			if tc.wantValue == nil {
				require.Nil(t, changes.Provider.Temperature)
				return
			}
			require.NotNil(t, changes.Provider.Temperature)
			require.InDelta(t, *tc.wantValue, *changes.Provider.Temperature, 1e-9)
		})
	}
}

// ptrFloat 取浮点指针，仅供上表写字面量期望值。
func ptrFloat(v float64) *float64 { return &v }

// TestAgentContent_PutPatchEditUnsupported：目标值带行内注释时补丁拒绝执行，
// 返回 400 `EDIT_UNSUPPORTED` 并提示改用整份原文；磁盘零写入、零操作行。
// 逆改点：静默丢注释 → 200 且磁盘字节变化，红。
func TestAgentContent_PutPatchEditUnsupported(t *testing.T) {
	env := newContentEnv(t)
	doc := "---\nname: reporter\ndescription: \"旧\" # 备注\n---\n\n正文\n"
	writeAgentDoc(t, env.root, contentAgentName, doc)
	oldETag := currentETag(t, env)
	require.Equal(t, workspace.ContentETag([]byte(doc)), oldETag)

	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		`{"changes":{"description":"新描述"}}`, oldETag))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	envelope := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeEditUnsupported, envelope.Code)
	require.Contains(t, envelope.Message, "description", "文案必须点名无法保留的字段")
	require.Contains(t, envelope.Message, "请改用整份原文保存", "契约 §8 要求给出替代路径")
	require.Equal(t, doc, string(readAgentDoc(t, env.root, contentAgentName)), "拒绝时磁盘零写入")
	require.Empty(t, env.ops.begunRecords(), "补丁无法生成候选时还没有要留痕的变更")
	require.Zero(t, env.snapshots.savedCount())
}

// TestAgentContent_PutValidationFailure：候选未通过装载期校验 → 400 并回装载器的
// 拒绝摘要；磁盘与快照都不动，操作行不落。
// 逆改点：校验晚于预写 / 忽略未注册候选 → 操作行或磁盘变化，红。
func TestAgentContent_PutValidationFailure(t *testing.T) {
	env := newContentEnvWith(t, func(deps *workspace.AgentEditorDeps) {
		deps.Validate = contentRejectingValidator
	})
	oldETag := currentETag(t, env)
	before := readAgentDoc(t, env.root, contentAgentName)

	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		contentBody(t, contentAgentDoc+"被拒绝的候选\n"), oldETag))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	envelope := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeBadRequest, envelope.Code)
	require.Contains(t, envelope.Message, "引用的模型连接未发布或不存在：main")
	require.NotContains(t, envelope.Message, "provider not registered", "英文拒因不得进入用户文案")
	require.Equal(t, before, readAgentDoc(t, env.root, contentAgentName), "校验失败必须磁盘零写入")
	require.Empty(t, env.ops.begunRecords())
	require.Zero(t, env.snapshots.savedCount())
}

// TestAgentContent_PutSnapshotFailure_Internal：快照落库失败 → 500 固定文案，磁盘
// 零写入；操作行已 pending 并回填 failed。
// 逆改点：快照失败仍写盘 → 磁盘变化；500 文案带底层原因 → 脱敏断言红。
func TestAgentContent_PutSnapshotFailure_Internal(t *testing.T) {
	env := newContentEnv(t)
	env.snapshots.failWith(errors.New("磁盘已满"))
	oldETag := currentETag(t, env)
	before := readAgentDoc(t, env.root, contentAgentName)

	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		contentBody(t, contentAgentDoc+"快照失败\n"), oldETag))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	envelope := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeInternal, envelope.Code)
	require.Equal(t, messageInternal, envelope.Message)
	require.NotContains(t, rec.Body.String(), "磁盘已满", "底层原因只进日志")

	require.Equal(t, before, readAgentDoc(t, env.root, contentAgentName), "快照失败不得写盘")
	finished := env.ops.finishedRecords()
	require.Len(t, finished, 1)
	require.Equal(t, core.OperationFailed, finished[0].status)
	require.Equal(t, "INTERNAL", finished[0].resultCode)
}

// TestAgentContent_PutRollbackRestoresBytes：写盘后调度同步失败 → 从备份回滚，
// 磁盘恢复写前字节、`.bak` 不残留；操作行回填 failed（不是 success / unknown）。
// 逆改点：调度失败不回滚 → 磁盘保持新字节；回滚后不回填终态 → 操作行停在 pending。
func TestAgentContent_PutRollbackRestoresBytes(t *testing.T) {
	env := newContentEnv(t)
	env.scheduler.failOnce(errors.New("cron 表达式非法"))
	oldETag := currentETag(t, env)
	before := readAgentDoc(t, env.root, contentAgentName)

	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		contentBody(t, contentAgentDoc+"调度会失败\n"), oldETag))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, messageInternal, decodeAuthEnvelope(t, rec).Message)

	require.Equal(t, before, readAgentDoc(t, env.root, contentAgentName), "回滚必须恢复写前字节")
	_, statErr := os.Stat(agentDocPath(env.root, contentAgentName) + ".bak")
	require.ErrorIs(t, statErr, fs.ErrNotExist, "回滚后备份不得残留")
	require.Equal(t, 2, env.scheduler.callCount(), "一次失败 + 一次回滚同步")

	finished := env.ops.finishedRecords()
	require.Len(t, finished, 1)
	require.Equal(t, core.OperationFailed, finished[0].status)
	require.Contains(t, finished[0].message, "已回滚")
}

// TestAgentContent_PutOperationPreWriteFailure_Internal：预写操作行失败 → 拒绝执行，
// 磁盘与快照都不动（宪法 V 的先写后做）。
// 逆改点：预写失败仍继续写盘 → 磁盘变化 / 快照落库，红。
func TestAgentContent_PutOperationPreWriteFailure_Internal(t *testing.T) {
	env := newContentEnv(t)
	env.ops.failBegin(errors.New("审计库不可写"))
	oldETag := currentETag(t, env)
	before := readAgentDoc(t, env.root, contentAgentName)

	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		contentBody(t, contentAgentDoc+"预写失败\n"), oldETag))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, messageInternal, decodeAuthEnvelope(t, rec).Message)

	require.Equal(t, before, readAgentDoc(t, env.root, contentAgentName))
	require.Empty(t, env.ops.begunRecords())
	require.Zero(t, env.snapshots.savedCount(), "预写失败必须在快照落库之前停下")
}

// TestAgentContent_AtomicWriteFailure_Internal：备份写不下去（同名 `.bak` 被目录占用）
// 时写入失败，原文件逐字节不变、无 `.tmp-` 残留；操作行回填 failed。
// 逆改点：备份失败后仍替换目标 → 目标字节变化；半截临时文件不清理 → 残留断言红。
func TestAgentContent_AtomicWriteFailure_Internal(t *testing.T) {
	env := newContentEnv(t)
	require.NoError(t, os.MkdirAll(agentDocPath(env.root, contentAgentName)+".bak", 0o750))
	oldETag := currentETag(t, env)
	before := readAgentDoc(t, env.root, contentAgentName)

	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		contentBody(t, contentAgentDoc+"原子写失败\n"), oldETag))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, messageInternal, decodeAuthEnvelope(t, rec).Message)

	require.Equal(t, before, readAgentDoc(t, env.root, contentAgentName), "写盘失败必须保留原字节")
	dirEntries, err := os.ReadDir(filepath.Join(env.root, "agents", contentAgentName))
	require.NoError(t, err)
	for _, entry := range dirEntries {
		require.NotContains(t, entry.Name(), ".tmp-", "失败路径不得留下临时文件")
	}

	finished := env.ops.finishedRecords()
	require.Len(t, finished, 1)
	require.Equal(t, core.OperationFailed, finished[0].status)
}

// TestAgentContent_ConcurrentSave_OnlyOneWins：两个并发保存带同一旧 ETag，只有一个
// 成功；落败者 412 且不写操作行，磁盘等于胜者的字节。
// 逆改点：比较与写入不在同一把写锁内 → 两个都 200（或磁盘与胜者不符）。
func TestAgentContent_ConcurrentSave_OnlyOneWins(t *testing.T) {
	env := newContentEnv(t)
	oldETag := currentETag(t, env)

	docs := []string{contentAgentDoc + "并发 A\n", contentAgentDoc + "并发 B\n"}
	bodies := []string{contentBody(t, docs[0]), contentBody(t, docs[1])}

	type outcome struct {
		doc  string
		code int
	}
	outcomes := make(chan outcome, len(docs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, doc := range docs {
		wg.Add(1)
		go func(doc, body string) {
			defer wg.Done()
			<-start
			rec := serveAdmin(env.server, contentPUT(contentAgentName, body, oldETag))
			outcomes <- outcome{doc: doc, code: rec.Code}
		}(doc, bodies[i])
	}
	close(start)
	wg.Wait()
	close(outcomes)

	winner := ""
	wins, conflicts := 0, 0
	for result := range outcomes {
		switch result.code {
		case http.StatusOK:
			wins++
			winner = result.doc
		case http.StatusPreconditionFailed:
			conflicts++
		default:
			t.Fatalf("并发保存只允许 200 或 412，实得 %d", result.code)
		}
	}
	require.Equal(t, 1, wins, "并发保存只有一个成功")
	require.Equal(t, 1, conflicts, "另一个必须 412")
	require.Equal(t, winner, string(readAgentDoc(t, env.root, contentAgentName)), "磁盘必须是胜者的字节")

	require.Len(t, env.ops.begunRecords(), 1, "落败的保存不写操作行")
	finished := env.ops.finishedRecords()
	require.Len(t, finished, 1)
	require.Equal(t, core.OperationSuccess, finished[0].status)
}

// TestAgentContent_PutRequiresIfMatch：缺 If-Match → 412，且**不**触达适配器
// （形状校验在写锁之前，约束 25）。
// 逆改点：缺失按"无条件写"处理 → 200 且适配器被调用，红。
func TestAgentContent_PutRequiresIfMatch(t *testing.T) {
	spy := &contentAdminSpy{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = spy
	})

	rec := serveAdmin(srv, contentPUT(contentAgentName, contentBody(t, contentAgentDoc), ""))
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	envelope := decodeAuthEnvelope(t, rec)
	require.Equal(t, codePreconditionFailed, envelope.Code)
	require.Contains(t, envelope.Message, "If-Match")
	require.Contains(t, envelope.Message, "sha256:", "文案必须给出合法形状")
	require.Zero(t, spy.writeCount(), "缺 If-Match 不得触达适配器")
}

// TestAgentContent_PutRejectsWeakETag：弱 ETag（`W/"..."`）一律 412。
// 逆改点：剥掉 `W/` 前缀后比较 → 弱比较被当成强比较，红。
func TestAgentContent_PutRejectsWeakETag(t *testing.T) {
	spy := &contentAdminSpy{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = spy
	})

	weak := `W/` + strongETagOf('a')
	rec := serveAdmin(srv, contentPUT(contentAgentName, contentBody(t, contentAgentDoc), weak))
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	require.Equal(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code)
	require.Zero(t, spy.writeCount())
}

// TestAgentContent_PutRejectsWildcard：通配 `*` 不是强 ETag，一律 412（不得当成
// "任意版本均可写"）。
// 逆改点：`*` 直接放行 → 200 且适配器被调用，红。
func TestAgentContent_PutRejectsWildcard(t *testing.T) {
	spy := &contentAdminSpy{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = spy
	})

	rec := serveAdmin(srv, contentPUT(contentAgentName, contentBody(t, contentAgentDoc), "*"))
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	require.Equal(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code)
	require.Zero(t, spy.writeCount())
}

// TestAgentContent_PutRejectsMalformedETag：形状非法的 If-Match 一律 412，包括裸哈希、
// 大写十六进制、长度偏差、前后空白与错误前缀。
// 逆改点：只查前缀不查摘要字符集 / 长度 → 对应子例红。
func TestAgentContent_PutRejectsMalformedETag(t *testing.T) {
	spy := &contentAdminSpy{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = spy
	})

	cases := []struct {
		name    string
		ifMatch string
	}{
		{"裸哈希", strings.Repeat("a", 64)},
		{"大写十六进制", `"sha256:` + strings.Repeat("A", 64) + `"`},
		{"摘要少一位", `"sha256:` + strings.Repeat("a", 63) + `"`},
		{"摘要多一位", `"sha256:` + strings.Repeat("a", 65) + `"`},
		{"前导空白", ` "sha256:` + strings.Repeat("a", 64) + `"`},
		{"尾随空白", `"sha256:` + strings.Repeat("a", 64) + `" `},
		{"错误前缀", `"sha1:` + strings.Repeat("a", 64) + `"`},
		{"缺尾引号", `"sha256:` + strings.Repeat("a", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAdmin(srv, contentPUT(contentAgentName, contentBody(t, contentAgentDoc), tc.ifMatch))
			require.Equal(t, http.StatusPreconditionFailed, rec.Code)
			require.Equal(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code)
		})
	}
	require.Zero(t, spy.writeCount(), "形状非法不得触达适配器")
}

// TestAgentContent_PutRejectsInvalidBody：请求体不合法一律 400，且**不**触达适配器
// （解码先于 If-Match 与写锁）。
// 逆改点：跳过 exactly-one 校验 / 接受未知字段 / 允许尾随文档 → 对应子例红。
func TestAgentContent_PutRejectsInvalidBody(t *testing.T) {
	spy := &contentAdminSpy{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = spy
	})

	cases := []struct {
		name string
		body string
	}{
		{"未知顶层字段", `{"content":"x","extra":1}`},
		{"未知嵌套字段", `{"changes":{"bogus":1}}`},
		{"content 与 changes 同时给", `{"content":"x","changes":{}}`},
		{"两者都不给", `{}`},
		{"JSON 截断", `{"content":"x"`},
		{"尾随第二个文档", `{"content":"x"}{"content":"y"}`},
		{"schedules 元素不是对象", `{"changes":{"schedules":["x"]}}`},
		{"schedules 字段类型非法", `{"changes":{"schedules":[{"cron":5}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAdmin(srv, contentPUT(contentAgentName, tc.body, strongETagOf('a')))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Equal(t, codeBadRequest, decodeAuthEnvelope(t, rec).Code)
		})
	}

	t.Run("请求体超过上限", func(t *testing.T) {
		limited := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.AgentAdmin = spy
			o.MaxRequestBytes = 32
		})
		body := `{"content":"` + strings.Repeat("a", 64) + `"}`
		rec := serveAdmin(limited, contentPUT(contentAgentName, body, strongETagOf('a')))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		envelope := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeBadRequest, envelope.Code)
		require.Contains(t, envelope.Message, "上限")
	})
	require.Zero(t, spy.writeCount(), "请求体不合法不得触达适配器")
}

// TestAgentContent_ErrorRedaction：500 只回固定文案，底层路径与原因不外传；
// 缺失装配端口同样 500；成功响应不含凭据。
// 逆改点：错误处理直接回 err.Error() → 路径 / 底层原因断言红。
func TestAgentContent_ErrorRedaction(t *testing.T) {
	t.Run("适配器错误只回固定文案", func(t *testing.T) {
		spy := &contentAdminSpy{err: errors.New(`open D:\secret\workspace\AGENT.md: Access is denied`)}
		srv := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.AgentAdmin = spy
		})

		rec := serveAdmin(srv, contentGET())
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		envelope := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, envelope.Code)
		require.Equal(t, messageInternal, envelope.Message)
		require.NotContains(t, rec.Body.String(), "secret")
		require.NotContains(t, rec.Body.String(), "Access is denied")

		rec = serveAdmin(srv, contentPUT(contentAgentName, contentBody(t, contentAgentDoc), strongETagOf('a')))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, messageInternal, decodeAuthEnvelope(t, rec).Message)
	})

	t.Run("缺失装配端口", func(t *testing.T) {
		srv := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
		})
		for _, req := range []*http.Request{
			contentGET(),
			contentPUT(contentAgentName, contentBody(t, contentAgentDoc), strongETagOf('a')),
		} {
			rec := serveAdmin(srv, req)
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			require.Equal(t, messageInternal, decodeAuthEnvelope(t, rec).Message)
			require.NotContains(t, rec.Body.String(), "AgentAdmin", "装配细节只进日志")
		}
	})

	t.Run("成功响应不含凭据", func(t *testing.T) {
		env := newContentEnv(t)
		rec := serveAdmin(env.server, contentPUT(contentAgentName,
			contentBody(t, contentAgentDoc+"脱敏\n"), currentETag(t, env)))
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotContains(t, rec.Body.String(), testToken)
	})
}

// TestAgentContent_RequiresAuth：内容端点一律要认证——缺凭据 401 且不触达适配器；
// 有效凭据通过闸门，actor 以 token 指纹进入写路径。
// 逆改点：内容端点误标公开 → 401 变 200；actor 缺省为空 → 指纹断言红。
func TestAgentContent_RequiresAuth(t *testing.T) {
	spy := &contentAdminSpy{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = spy
	})

	unauthenticated := []*http.Request{
		adminRequest(http.MethodGet, contentPath(contentAgentName), ""),
		adminRequest(http.MethodPut, contentPath(contentAgentName), contentBody(t, contentAgentDoc)),
	}
	for _, req := range unauthenticated {
		rec := serveAdmin(srv, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, rec).Code)
	}
	require.Zero(t, spy.readCount())
	require.Zero(t, spy.writeCount())
	require.Empty(t, spy.actorOf().Subject, "未认证请求不得进入写路径")

	rec := serveAdmin(srv, contentGET())
	require.Equal(t, http.StatusOK, rec.Code, "有效凭据必须通过闸门")

	rec = serveAdmin(srv, contentPUT(contentAgentName, contentBody(t, contentAgentDoc), strongETagOf('a')))
	require.Equal(t, http.StatusOK, rec.Code)
	actor := spy.actorOf()
	require.Equal(t, tokenActorSubject(testToken), actor.Subject)
	require.Equal(t, core.AuthMethodBearer, actor.Method)
	require.NotEmpty(t, actor.Source)
}

// contentLogSink 是并发安全的日志缓冲：被测编辑器写的是 `slog.Default()`
// （§3.1.13 的依赖集里没有 logger），所以用例只能从默认出口取文；换出与恢复由
// captureContentLogs 成对完成。本用例不并行，与其他顶层用例不重叠。
// contentLogSink captures the default logger because the editor has no logger
// dependency; captureContentLogs swaps it in and restores it on cleanup.
type contentLogSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

// Write 满足 io.Writer，供 TextHandler 落一行一条记录。
// Write satisfies io.Writer so the text handler emits one record per line.
func (s *contentLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String 返回已捕获的全部日志。
// String returns everything captured so far.
func (s *contentLogSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// captureContentLogs 把默认 logger 临时指向本用例的缓冲，并在用例结束时恢复。
// captureContentLogs points the default logger at the case's sink and restores it.
func captureContentLogs(t *testing.T) *contentLogSink {
	t.Helper()
	sink := &contentLogSink{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return sink
}

// requireContentLog 断言缓冲里有一条含 msg 的记录，且级别不低于 WARN。模块卡错误表
// 把内部失败定为 ERROR，任务描述写 WARN——两者都算"失败有日志"，断言不锁死单一级别，
// 但**不允许**静默（DEBUG / INFO 或完全没有日志都会红）。
// requireContentLog demands a record at WARN or ERROR; a missing or quieter log fails.
func requireContentLog(t *testing.T, sink *contentLogSink, msg string) {
	t.Helper()
	for _, line := range strings.Split(sink.String(), "\n") {
		if !strings.Contains(line, msg) {
			continue
		}
		require.True(t,
			strings.Contains(line, "level=WARN") || strings.Contains(line, "level=ERROR"),
			"失败路径必须以 WARN 或 ERROR 入日志，实得: %s", line)
		return
	}
	t.Fatalf("失败路径缺少日志 %q；实际日志:\n%s", msg, sink.String())
}

// TestAgentContent_OperationPreWriteOrdering：把约束 10 的顺序判据钉在磁盘读数上——
// 预写返回的瞬间磁盘仍是写前字节，快照落库的瞬间预写已存在且磁盘仍未动，之后才是
// 原子写；操作行从 pending 走到 success 是同一条标识。
// 逆改点：先写盘后留痕 → 预写钩子读到新字节；先写盘后落快照 → 快照钩子读到新字节。
func TestAgentContent_OperationPreWriteOrdering(t *testing.T) {
	env := newContentEnv(t)
	oldETag := currentETag(t, env)
	before := readAgentDoc(t, env.root, contentAgentName)
	newDoc := contentAgentDoc + "顺序用例\n"

	var atBegin, atSave []byte
	var beginReadErr, saveReadErr error
	beganAtSave := 0
	env.ops.watchBegin(func() {
		atBegin, beginReadErr = os.ReadFile(agentDocPath(env.root, contentAgentName))
	})
	env.snapshots.watchSave(func() {
		atSave, saveReadErr = os.ReadFile(agentDocPath(env.root, contentAgentName))
		beganAtSave = len(env.ops.begunRecords())
	})

	rec := serveAdmin(env.server, contentPUT(contentAgentName, contentBody(t, newDoc), oldETag))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, beginReadErr)
	require.NoError(t, saveReadErr)

	require.Equal(t, before, atBegin, "预写返回时目标文件必须一个字节都没动")
	require.Equal(t, before, atSave, "快照落库时目标文件必须仍是写前字节")
	require.Equal(t, 1, beganAtSave, "快照落库之前必须先有 pending 操作行")
	require.Equal(t, []byte(newDoc), readAgentDoc(t, env.root, contentAgentName))

	begun := env.ops.begunRecords()
	require.Len(t, begun, 1)
	require.Equal(t, oldETag, begun[0].ExpectedETag, "操作行固定写前 ETag，供恢复查证 applied / not_applied")
	require.Equal(t, strings.Trim(workspace.ContentETag([]byte(newDoc)), `"`), begun[0].RequestHash,
		"请求摘要按候选字节计算")

	finished := env.ops.finishedRecords()
	require.Len(t, finished, 1)
	require.Equal(t, begun[0].OperationID, finished[0].operationID, "同一条操作行从 pending 走到成功")
	require.Equal(t, core.OperationSuccess, finished[0].status)
	require.Equal(t, begun[0].DetailJSON, finished[0].detail, "终态回填沿用同一份详情投影")

	detail := string(begun[0].DetailJSON)
	require.Contains(t, detail, "candidate_sha256")
	require.Contains(t, detail, "previous_sha256")
	require.NotContains(t, detail, "顺序用例", "detail_json 只落摘要，不落正文")
	require.NotContains(t, detail, testToken, "detail_json 不得含凭据")
	require.NotContains(t, detail, env.root, "detail_json 不得含绝对路径")
}

// TestAgentContent_OperationFailureAuditTrail：逐个失败类别核对"审计 + 日志"双轨——
// 预写失败零操作行且磁盘不动；快照 / 原子写失败落一条 failed 操作行，磁盘逐字节
// 不变，且失败路径确实有一条 WARN/ERROR 日志（不是只落库不记日志）。
// 逆改点：失败只写日志不落审计 / 只落审计不写日志 / 失败仍动盘 → 对应断言红。
func TestAgentContent_OperationFailureAuditTrail(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, env *contentEnv)
		wantLog string
		// wantBegun 是预期落库的 pending 行数：预写失败为零，其余为一条。
		wantBegun int
		// wantSnapshots 是预期落库的快照数：快照本身失败为零，写盘阶段失败为一
		// （快照先于写盘成功落库，磁盘仍是写前字节）。
		wantSnapshots int
	}{
		{
			name: "预写失败拒绝执行",
			prepare: func(_ *testing.T, env *contentEnv) {
				env.ops.failBegin(errors.New("审计库不可写"))
			},
			wantLog:       "内容写预写失败",
			wantBegun:     0,
			wantSnapshots: 0,
		},
		{
			name: "快照落库失败",
			prepare: func(_ *testing.T, env *contentEnv) {
				env.snapshots.failWith(errors.New("磁盘已满"))
			},
			wantLog:       "内容写快照落库失败",
			wantBegun:     1,
			wantSnapshots: 0,
		},
		{
			name: "原子写失败",
			prepare: func(t *testing.T, env *contentEnv) {
				require.NoError(t, os.MkdirAll(agentDocPath(env.root, contentAgentName)+".bak", 0o750))
			},
			wantLog:       "内容写盘失败",
			wantBegun:     1,
			wantSnapshots: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := captureContentLogs(t)
			env := newContentEnv(t)
			tc.prepare(t, env)
			oldETag := currentETag(t, env)
			before := readAgentDoc(t, env.root, contentAgentName)

			rec := serveAdmin(env.server, contentPUT(contentAgentName,
				contentBody(t, contentAgentDoc+"失败双轨\n"), oldETag))
			require.Equal(t, http.StatusInternalServerError, rec.Code)
			require.Equal(t, messageInternal, decodeAuthEnvelope(t, rec).Message)

			require.Equal(t, before, readAgentDoc(t, env.root, contentAgentName), "失败路径必须磁盘零写入")
			require.Equal(t, tc.wantSnapshots, env.snapshots.savedCount(),
				"快照只在写盘之前成功落库，之后失败不回填快照")

			begun := env.ops.begunRecords()
			require.Len(t, begun, tc.wantBegun)
			finished := env.ops.finishedRecords()
			if tc.wantBegun == 0 {
				require.Empty(t, finished, "没有预写就没有可回填的终态")
			} else {
				require.Len(t, finished, 1, "已预写的失败必须回填终态")
				require.Equal(t, begun[0].OperationID, finished[0].operationID)
				require.Equal(t, core.OperationFailed, finished[0].status)
				require.Equal(t, "INTERNAL", finished[0].resultCode)
				require.NotContains(t, string(finished[0].detail), "失败双轨",
					"detail_json 只落摘要，不落正文")
				require.NotContains(t, string(finished[0].detail), testToken)
			}

			requireContentLog(t, sink, tc.wantLog)
			require.NotContains(t, rec.Body.String(), "磁盘已满", "底层原因只进日志")
		})
	}
}

// TestAgentContent_OperationSharesAgentGate：内容写与运行准入共用同一把 Agent 闸门
// （约束 25 / 图 D）——写锁被占住时保存请求停在临界区外，释放后才落盘；不得出现
// 编辑器私有的第二把锁。
// 逆改点：编辑器自建私有锁 / 不取锁 → 持锁期间请求就完成，红。
func TestAgentContent_OperationSharesAgentGate(t *testing.T) {
	env := newContentEnv(t)
	oldETag := currentETag(t, env)
	before := readAgentDoc(t, env.root, contentAgentName)
	body := contentBody(t, contentAgentDoc+"闸门用例\n")

	unlock := env.gate.Lock(contentAgentName)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- serveAdmin(env.server, contentPUT(contentAgentName, body, oldETag))
	}()

	select {
	case rec := <-done:
		unlock()
		t.Fatalf("写锁被占住期间保存不得完成，实得状态码 %d", rec.Code)
	case <-time.After(150 * time.Millisecond):
	}
	require.Equal(t, before, readAgentDoc(t, env.root, contentAgentName), "等待期间磁盘不得被写")

	unlock()
	select {
	case rec := <-done:
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("释放写锁后保存必须继续完成")
	}
	require.NotEqual(t, before, readAgentDoc(t, env.root, contentAgentName))
}

// TestAgentContent_PutPatch_BodyNotifyModeAndBindings：契约 agent-md §2.3 列出的
// `body` / `notify_mode` / `bindings` 三个补丁键必须在线上形状里可用并落到文件——
// changesBody 缺任一键时 DisallowUnknownFields 会把页面保存挡成 400。
// 逆改点：删掉任一线上字段 → 对应请求变 400；按名称补绑 → UID 断言红。
// Acceptance point: the three contract-declared patch keys reach the file verbatim.
func TestAgentContent_PutPatch_BodyNotifyModeAndBindings(t *testing.T) {
	env := newContentEnv(t)
	const channelUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z4C"
	const body = "全新的正文。"

	req := contentPUT(contentAgentName, `{"changes":{`+
		`"body":"全新的正文。",`+
		`"notify_mode":"allowlist",`+
		`"notify_channels":["alerts"],`+
		`"bindings":[{"resource_kind":"notify_channel","ref_name":"alerts","resource_uid":"`+channelUID+`"}]}}`,
		currentETag(t, env))
	rec := serveAdmin(env.server, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	raw := readAgentDoc(t, env.root, contentAgentName)
	require.Contains(t, string(raw), `notify_mode: "allowlist"`)
	require.Contains(t, string(raw), `  - "alerts"`)
	bindings, parseErr := workspace.ParseResourceBindings(raw)
	require.NoError(t, parseErr)
	require.Equal(t, []core.ResourceBinding{{Name: "alerts", UID: channelUID}},
		bindings.NotifyChannels, "绑定必须逐字录用请求体给的 UID")
	require.True(t, strings.HasSuffix(string(raw), "---\n\n"+body+"\n"),
		"正文必须整段替换为请求值，实得: %q", string(raw))
}

// TestAgentContent_PutPatch_RejectsInvalidNotifyModeAndBinding：非法的
// `notify_mode` 字面量与格式非法的绑定 UID 都在写锁之前 400，磁盘零写入——
// 前者按请求体校验拒绝，后者按绑定专用码拒绝（不按名猜 UID）。
// 逆改点：静默接受未知模式 / 把非法 UID 当缺省 → 200 且磁盘变化，红。
func TestAgentContent_PutPatch_RejectsInvalidNotifyModeAndBinding(t *testing.T) {
	spy := &contentAdminSpy{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = spy
	})

	rec := serveAdmin(srv, contentPUT(contentAgentName,
		`{"changes":{"notify_mode":"sometimes"}}`, strongETagOf('a')))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	envelope := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeBadRequest, envelope.Code)
	require.Contains(t, envelope.Message, "none / all_eligible / allowlist")
	require.Zero(t, spy.writeCount(), "非法 notify_mode 不得触达适配器")

	env := newContentEnv(t)
	rec = serveAdmin(env.server, contentPUT(contentAgentName,
		`{"changes":{"bindings":[{"resource_kind":"notify_channel","ref_name":"alerts","resource_uid":"not-a-uid"}]}}`,
		currentETag(t, env)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, codeAgentResourceBindingInvalid, decodeAuthEnvelope(t, rec).Code)
	require.Equal(t, contentAgentDoc, string(readAgentDoc(t, env.root, contentAgentName)),
		"绑定非法必须磁盘零写入")
	require.Empty(t, env.ops.begunRecords())
}

// TestAgentContentChanges_DisplayNameTriState：`changes.display_name` 三态（R6.2）——
// 省略 = 不修改（nil），空串 = 移除该行，非空 = 写入；省略不得被读成清除。
// 对应契约：agent-md §2 的 changes 表。
// TestAgentContentChanges_DisplayNameTriState pins the omitted / empty / value states.
func TestAgentContentChanges_DisplayNameTriState(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantNil bool
		want    string
	}{
		{name: "omitted leaves unchanged", body: `{"changes":{"description":"新描述"}}`, wantNil: true},
		{name: "empty string clears", body: `{"changes":{"display_name":""}}`, want: ""},
		{name: "value sets", body: `{"changes":{"display_name":" 值班助手 "}}`, want: " 值班助手 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body contentUpdateBody
			require.NoError(t, json.Unmarshal([]byte(tc.body), &body))
			require.NotNil(t, body.Changes)
			changes, err := body.Changes.toCore()
			require.NoError(t, err)
			if tc.wantNil {
				require.Nil(t, changes.DisplayName)
				return
			}
			require.NotNil(t, changes.DisplayName)
			require.Equal(t, tc.want, *changes.DisplayName)
		})
	}
}

// TestAgentContent_PutDisplayNameRoundTrip：写入展示名后磁盘只多一行，读回 ETag 与
// 响应一致；用新 ETag 清空后逐字节回到原文件。长度越界按 400 拒绝且零写入。
// TestAgentContent_PutDisplayNameRoundTrip sets and clears the display name through the
// real endpoint and refuses an oversized value with zero writes.
func TestAgentContent_PutDisplayNameRoundTrip(t *testing.T) {
	env := newContentEnv(t)
	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		`{"changes":{"display_name":"值班助手"}}`, currentETag(t, env)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	withDisplay := strings.Replace(contentAgentDoc,
		"  - \"file_read\"\n---\n",
		"  - \"file_read\"\ndisplay_name: \"值班助手\"\n---\n", 1)
	require.Equal(t, withDisplay, string(readAgentDoc(t, env.root, contentAgentName)))
	view := decodeAgentFilesView(t, rec)
	require.Equal(t, workspace.ContentETag([]byte(withDisplay)), view.ETag)

	rec = serveAdmin(env.server, contentPUT(contentAgentName,
		`{"changes":{"display_name":""}}`, view.ETag))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, contentAgentDoc, string(readAgentDoc(t, env.root, contentAgentName)),
		"空串必须移除该行并逐字节回到原文件")

	tooLong, err := json.Marshal(map[string]string{"display_name": strings.Repeat("名", 65)})
	require.NoError(t, err)
	rec = serveAdmin(env.server, contentPUT(contentAgentName,
		`{"changes":`+string(tooLong)+`}`, currentETag(t, env)))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	envelope := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeBadRequest, envelope.Code)
	require.Contains(t, envelope.Message, "display_name")
	require.Equal(t, contentAgentDoc, string(readAgentDoc(t, env.root, contentAgentName)),
		"非法展示名必须磁盘零写入")
}
