package app

// 本文件是 US-25C §2.3.2 / §4.3 的发布链定向判据（单元层，无数据库）：
//   - 载荷必须先于指针 CAS 写入，载荷写失败时指针不变、操作行回填 failed（FR-25C-01）；
//   - 同哈希发布也核对载荷，缺失则补写且**不动**指针（FR-25C-02 / C15）；
//   - 载荷已存在且自洽时按真幂等返回，不重复写（C15）。
//
// 夹具用内存替身替换共享资源记录、操作仓储与载荷写面（模块卡 §4.1：单元层允许内存
// 夹具、无外部依赖）。判据只看**调用次序与指针状态**，不依赖 PostgreSQL。
// Unit judges for the publish chain: payload before the pointer CAS, a failed payload
// leaves the pointer untouched, and the same-hash path still proves the payload exists.

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

const (
	// publishPayloadAgent 是本文件夹具的 Agent 名（不依赖工作区里已有的定义）。
	publishPayloadAgent = "publish-agent"
	// publishPayloadUID 是合法形态的 Agent 身份（`ag_` + 26 位 Crockford Base32）。
	publishPayloadUID = core.AgentUID("ag_0123456789ABCDEFGHJKMNPQRS")
	// publishPayloadHash 是本文件夹具的 bundle 哈希（内容寻址目录名，逐字用作 basename）。
	publishPayloadHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// publishPayloadMaxBytes 是夹具载荷上限：够装下临时目录里的少量文件。
	publishPayloadMaxBytes = int64(1 << 20)
)

// publishPayloadClock 是固定时钟：操作标识与版本时间可复算。
func publishPayloadClock() time.Time {
	return time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
}

// fakeBundleRecords 是 `bundleRecordStore` 的内存替身：记录调用次序与指针状态，
// 让"先载荷后指针"成为可断言的面，而不是靠观察文件系统。
// fakeBundleRecords is the in-memory bundleRecordStore double: it records the call
// sequence and the pointer state so ordering is assertable.
type fakeBundleRecords struct {
	calls        []string
	uid          core.AgentUID
	pointer      core.ResourcePointer
	pointerSet   bool
	versions     map[int64]core.ResourceVersion
	nextVersion  int64
	ensureErr    error
	publishErr   error
	publishCalls int
	// ops 让替身复现生产的收尾方式：指针 CAS 成功时**在同一个事务里**回填 success
	// 终态（`PublishAgentBundle` 的 finishOperationTx），发布器本身不再写一次。
	// ops mirrors the production CAS: the success terminal row is written there.
	ops *fakeOperationLog
	// lifecycleIndex 是共享生命周期索引的内存替身：回填的重建判据读它。
	// lifecycleIndex doubles the shared lifecycle index for the backfill rebuild gate.
	lifecycleIndex map[string]core.LifecycleRecord
}

// newFakeBundleRecords 建一份空发布记录：还没有任何指针与版本行。
func newFakeBundleRecords() *fakeBundleRecords {
	return &fakeBundleRecords{
		uid:         publishPayloadUID,
		versions:    make(map[int64]core.ResourceVersion),
		nextVersion: 7,
	}
}

func (f *fakeBundleRecords) note(call string) { f.calls = append(f.calls, call) }

func (f *fakeBundleRecords) AgentIdentity(context.Context, string) (core.AgentUID, string, bool, error) {
	f.note("identity")
	return f.uid, publishPayloadAgent, true, nil
}

func (f *fakeBundleRecords) Pointer(context.Context, core.ResourceKind, string) (core.ResourcePointer, error) {
	f.note("pointer")
	if !f.pointerSet {
		return core.ResourcePointer{}, core.ErrResourceNotFound
	}
	return f.pointer, nil
}

func (f *fakeBundleRecords) PointerByUID(context.Context, core.ResourceKind, core.ResourceUID) (core.ResourcePointer, error) {
	f.note("pointer")
	if !f.pointerSet {
		return core.ResourcePointer{}, core.ErrResourceNotFound
	}
	return f.pointer, nil
}

func (f *fakeBundleRecords) ReadLifecycleIndex(_ context.Context, agent string) (core.LifecycleRecord, bool, string, error) {
	record, ok := f.lifecycleIndex[agent]
	return record, ok, "", nil
}

func (f *fakeBundleRecords) VersionByID(_ context.Context, id int64) (core.ResourceVersion, error) {
	f.note("version")
	version, ok := f.versions[id]
	if !ok {
		return core.ResourceVersion{}, core.ErrResourceNotFound
	}
	return version, nil
}

func (f *fakeBundleRecords) EnsureVersion(_ context.Context, v core.ResourceVersion) (core.ResourceVersion, bool, error) {
	f.note("ensure-version")
	if f.ensureErr != nil {
		return core.ResourceVersion{}, false, f.ensureErr
	}
	if v.ID == 0 {
		v.ID = f.nextVersion
	}
	f.versions[v.ID] = v
	return v, false, nil
}

func (f *fakeBundleRecords) PublishAgentBundle(_ context.Context, uid core.AgentUID,
	versionID, _ int64, operationID string, _ time.Time,
) (bool, error) {
	f.note("publish-pointer")
	f.publishCalls++
	if f.publishErr != nil {
		return false, f.publishErr
	}
	f.pointer = core.ResourcePointer{
		Kind:               core.ResourceAgentBundle,
		ResourceID:         string(uid),
		ResourceUID:        core.ResourceUID(uid),
		PublishedVersionID: versionID,
	}
	f.pointerSet = true
	if f.ops != nil {
		f.ops.finished = append(f.ops.finished, core.OperationRecord{
			OperationID: operationID,
			Status:      core.OperationSuccess,
		})
	}
	return true, nil
}

// fakeBundlePayloads 同时扮演读面（同哈希核对）与写面（writePayload 的插入点）：
// 生产是两个实现，替身合成一个，调用次序并进共享记录。
type fakeBundlePayloads struct {
	records  map[string]core.BundlePayloadRecord
	putErr   error
	putCalls int
	order    *[]string
}

// newFakeBundlePayloads 建空载荷表；`order` 与记录替身共用，用于跨替身断言次序。
func newFakeBundlePayloads(order *[]string) *fakeBundlePayloads {
	return &fakeBundlePayloads{records: make(map[string]core.BundlePayloadRecord), order: order}
}

func (f *fakeBundlePayloads) PutBundlePayload(_ context.Context, rec core.BundlePayloadRecord) error {
	f.putCalls++
	if f.order != nil {
		*f.order = append(*f.order, "payload")
	}
	if f.putErr != nil {
		return f.putErr
	}
	f.records[rec.BundleHash] = rec
	return nil
}

func (f *fakeBundlePayloads) ReadBundlePayload(_ context.Context, hash string) (core.BundlePayloadRecord, bool, error) {
	rec, ok := f.records[hash]
	return rec, ok, nil
}

// fakeOperationLog 是 `core.OperationStore` 的内存替身：只关心预写与终态两次调用。
// 恢复面方法不会被发布链调用，给零值实现即可。
type fakeOperationLog struct {
	begun    []core.OperationRecord
	finished []core.OperationRecord
	beginErr error
	order    *[]string
}

func (f *fakeOperationLog) BeginOperation(_ context.Context, rec core.OperationRecord) error {
	if f.beginErr != nil {
		return f.beginErr
	}
	if f.order != nil {
		*f.order = append(*f.order, "begin")
	}
	f.begun = append(f.begun, rec)
	return nil
}

func (f *fakeOperationLog) FinishOperation(_ context.Context, operationID string, status core.OperationStatus,
	resultCode, errorMessage string, detailJSON []byte, now time.Time,
) error {
	f.finished = append(f.finished, core.OperationRecord{
		OperationID:  operationID,
		Status:       status,
		ResultCode:   resultCode,
		ErrorMessage: errorMessage,
		DetailJSON:   detailJSON,
		FinishedAt:   now,
	})
	return nil
}

func (f *fakeOperationLog) GetOperation(context.Context, string) (core.OperationRecord, error) {
	return core.OperationRecord{}, core.ErrResourceNotFound
}

func (f *fakeOperationLog) ListOperations(context.Context, core.OperationFilter) ([]core.OperationRecord, error) {
	return nil, nil
}

func (f *fakeOperationLog) PendingOperations(context.Context) ([]core.OperationRecord, error) {
	return nil, nil
}

func (f *fakeOperationLog) RecoverOperation(context.Context, string, core.OperationStatus, string,
	[]byte, time.Time,
) error {
	return nil
}

// publishPayloadInstalled 砌一份"已安装"的 bundle 目录：目录名就是内容寻址哈希，
// 内含一个普通文件（确定性编码要求真实目录内容，空目录换不出有意义的载荷）。
func publishPayloadInstalled(t *testing.T, hash string) assets.Installed {
	t.Helper()
	dir := filepath.Join(t.TempDir(), hash)
	writeWorkspaceFile(t, dir, "agent/agents/"+publishPayloadAgent+"/AGENT.md", publishPayloadAgentMD)
	return assets.Installed{
		Manifest: assets.Manifest{
			FormatVersion: 1,
			Agent:         publishPayloadAgent,
			AgentUID:      publishPayloadUID,
			ConfigHash:    "cfg-hash-fixture",
			BundleHash:    hash,
		},
		Dir: dir,
	}
}

// publishPayloadAgentMD 是夹具定义正文：字段齐全，构建 / 解析都不需要额外分支。
const publishPayloadAgentMD = `---
name: publish-agent
description: 发布链载荷用例
provider:
  name: deepseek
  model: deepseek-chat
---

发布链载荷用例正文。
`

// requireCalledBefore 断言 `before` 出现在 `after` 之前（两者都必须被调用过）。
func requireCalledBefore(t *testing.T, calls []string, before, after string) {
	t.Helper()
	first := slices.Index(calls, before)
	second := slices.Index(calls, after)
	require.GreaterOrEqual(t, first, 0, "缺少调用 %q：%v", before, calls)
	require.GreaterOrEqual(t, second, 0, "缺少调用 %q：%v", after, calls)
	require.Less(t, first, second, "%q 必须早于 %q：%v", before, after, calls)
}

// 中文验收点：载荷必须先于指针 CAS 写入。逆改点（模块卡 §4.3）：把 `writePayload`
// 移到 CAS 之后 → 本用例的次序断言失败，载荷失败用例看到指针已变。
// Acceptance point: the payload write happens strictly before the pointer CAS.
func TestPublish_WritesPayloadBeforePointer(t *testing.T) {
	records := newFakeBundleRecords()
	payloads := newFakeBundlePayloads(&records.calls)
	ops := &fakeOperationLog{order: &records.calls}
	records.ops = ops
	logger, _ := newCapturedLogger()
	publisher := newBundlePublisher(records, ops, payloads, publishPayloadMaxBytes,
		logger, publishPayloadClock)

	installed := publishPayloadInstalled(t, publishPayloadHash)
	versionID, err := publisher.publishAs(context.Background(), installed, core.Actor{})
	require.NoError(t, err)
	require.Equal(t, int64(7), versionID, "发布必须返回版本行的 ID")

	require.Equal(t, 1, payloads.putCalls, "发布必须写一次载荷")
	requireCalledBefore(t, records.calls, "begin", "payload")
	requireCalledBefore(t, records.calls, "ensure-version", "payload")
	requireCalledBefore(t, records.calls, "payload", "publish-pointer")
	require.True(t, records.pointerSet, "载荷写成功后指针必须前进")
	require.Equal(t, int64(7), records.pointer.PublishedVersionID)
	require.Len(t, ops.finished, 1)
	require.Equal(t, core.OperationSuccess, ops.finished[0].Status)
}

// 中文验收点：载荷写失败时发布必须失败关闭——指针**不动**、操作行回填 failed。
// Acceptance point: a payload failure fails closed; the pointer never moves and the
// pending operation row is closed as failed.
func TestPublish_PayloadFailureLeavesPointer(t *testing.T) {
	records := newFakeBundleRecords()
	payloads := newFakeBundlePayloads(&records.calls)
	payloads.putErr = errors.New("载荷写入失败（夹具）")
	ops := &fakeOperationLog{order: &records.calls}
	logger, _ := newCapturedLogger()
	publisher := newBundlePublisher(records, ops, payloads, publishPayloadMaxBytes,
		logger, publishPayloadClock)

	installed := publishPayloadInstalled(t, publishPayloadHash)
	_, err := publisher.publishAs(context.Background(), installed, core.Actor{})
	require.Error(t, err, "载荷写失败必须拒绝发布，不得只装本地")

	require.Equal(t, 1, payloads.putCalls, "失败路径必须尝试过一次载荷写入")
	require.Zero(t, records.publishCalls, "载荷失败后不得再动指针")
	require.False(t, records.pointerSet, "指针必须保持原状")
	require.Len(t, ops.finished, 1, "pending 行必须回填终态")
	require.Equal(t, core.OperationFailed, ops.finished[0].Status)
	require.NotEmpty(t, ops.finished[0].ErrorMessage)
}

// 中文验收点：同哈希发布也必须核对载荷——缺失则补写且**不动**指针（真幂等）；
// 载荷存在且自洽时直接返回，不重复写。逆改点（模块卡 §4.3）：恢复原短路 →
// 载荷表仍为空。
// Acceptance point: the same-hash path proves the payload exists, backfills it
// without moving the pointer, and stays a true no-op once the payload is present.
func TestPublish_SameHashWritesMissingPayload(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeWorkspaceFile(t, root, "agents/"+publishPayloadAgent+"/AGENT.md", publishPayloadAgentMD)
	records := newFakeBundleRecords()
	payloads := newFakeBundlePayloads(&records.calls)
	ops := &fakeOperationLog{order: &records.calls}
	logger, _ := newCapturedLogger()
	deps := workspacePublishDeps{
		Root:         root,
		BundleRoot:   t.TempDir(),
		Versions:     records,
		Operations:   ops,
		Payloads:     payloads,
		PayloadStore: payloads,
		MaxBytes:     publishPayloadMaxBytes,
		Logger:       logger,
		Now:          publishPayloadClock,
	}

	// 首轮发布：建立指针与载荷。
	versionID, err := publishWorkspaceAgentBundle(ctx, deps, publishPayloadAgent)
	require.NoError(t, err)
	require.Equal(t, int64(7), versionID)
	require.Equal(t, 1, payloads.putCalls, "首轮发布必须写载荷")
	require.Equal(t, 1, records.publishCalls)

	// 存量指针的载荷缺失（回填前的形态）：同哈希发布只能补写载荷，不能移指针。
	hash := records.versions[versionID].ContentHash
	require.NotEmpty(t, hash)
	delete(payloads.records, hash)
	beforePut := payloads.putCalls
	beforeBegin := len(ops.begun)

	versionID, err = publishWorkspaceAgentBundle(ctx, deps, publishPayloadAgent)
	require.NoError(t, err)
	require.Equal(t, int64(7), versionID, "同哈希必须返回既有版本")
	require.Equal(t, beforePut+1, payloads.putCalls, "载荷缺失必须补写")
	require.Equal(t, 1, records.publishCalls, "补写载荷不得移动指针")
	require.Equal(t, int64(7), records.pointer.PublishedVersionID, "指针必须仍是原版本")
	require.Len(t, ops.begun, beforeBegin, "同哈希补载荷不是一次新发布，不得预写操作行")

	// 载荷存在且自洽：真幂等，不重复写。
	beforePut = payloads.putCalls
	versionID, err = publishWorkspaceAgentBundle(ctx, deps, publishPayloadAgent)
	require.NoError(t, err)
	require.Equal(t, int64(7), versionID)
	require.Equal(t, beforePut, payloads.putCalls, "载荷已存在且自洽时不得重复写")
	require.Equal(t, 1, records.publishCalls)
}
