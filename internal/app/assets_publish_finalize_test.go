package app

// 本文件是 2026-11-10 复核 P1（发布失败后的审计回填没有时间上限）的定向判据：
//   - `finishFailed` 与同内容并发发布的成功收尾都必须带**有限**预算；
//     回填上下文不得只 `WithoutCancel` 而把外层 30s 发布时限一并剥掉。
//   - 超时留 pending 行由 `verifyBundlePublish` 按"未确认"恢复，因此有界回填
//     失败是安全方向（恢复链判据见 assets_publish 的既有恢复测试）。
//
// 判据只看回填调用收到的 context：是否带 deadline、剩余预算是否不超过
// `bundleFinalizeTimeout`。逆改点：把 `finalizeContext` 还原成
// `context.WithCancel(context.WithoutCancel(ctx))` → 两个用例都因"无 deadline"变红。
// Directed judges for the bounded audit finalization added on 2026-11-10.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// finalizeCall 记录一次终态回填收到的上下文特征。
type finalizeCall struct {
	status      core.OperationStatus
	hasDeadline bool
	remaining   time.Duration
}

// deadlineRecordingOperations 在 `fakeOperationLog` 之上截获回填上下文：
// 判定"回填有界"必须看调用方拿到的 ctx，而不是实现里的注释。
type deadlineRecordingOperations struct {
	*fakeOperationLog
	calls []finalizeCall
}

func (d *deadlineRecordingOperations) FinishOperation(ctx context.Context, operationID string,
	status core.OperationStatus, resultCode, errorMessage string, detailJSON []byte, now time.Time,
) error {
	call := finalizeCall{status: status}
	if deadline, ok := ctx.Deadline(); ok {
		call.hasDeadline = true
		call.remaining = time.Until(deadline)
	}
	d.calls = append(d.calls, call)
	return d.fakeOperationLog.FinishOperation(ctx, operationID, status, resultCode,
		errorMessage, detailJSON, now)
}

// requireBoundedFinalize 断言一次回填带有限预算，且不超过 `bundleFinalizeTimeout`。
// 下限只要求正数：实现若把预算再收窄（例如按剩余时限截断）也应通过。
func requireBoundedFinalize(t *testing.T, calls []finalizeCall, want core.OperationStatus) {
	t.Helper()
	require.Len(t, calls, 1, "必须恰好回填一次终态")
	require.Equal(t, want, calls[0].status)
	require.True(t, calls[0].hasDeadline,
		"审计终态回填必须带有限预算：不得只用 WithoutCancel 继承无期限上下文")
	require.Greater(t, calls[0].remaining, time.Duration(0), "回填预算在调用时必须尚未耗尽")
	require.LessOrEqual(t, calls[0].remaining, bundleFinalizeTimeout,
		"回填预算不得超过 bundleFinalizeTimeout（%s）", bundleFinalizeTimeout)
}

// 中文验收点：载荷写失败后的 failed 回填必须受 `bundleFinalizeTimeout` 约束；
// 数据库 / 连接池等待不得因 `WithoutCancel` 变成无限等待。
// Acceptance point: the failed-status finalization is bounded.
func TestPublish_FailureFinalizeIsBounded(t *testing.T) {
	records := newFakeBundleRecords()
	payloads := newFakeBundlePayloads(&records.calls)
	payloads.putErr = errors.New("载荷写入失败（夹具）")
	ops := &deadlineRecordingOperations{fakeOperationLog: &fakeOperationLog{order: &records.calls}}
	logger, _ := newCapturedLogger()
	publisher := newBundlePublisher(records, ops, payloads, publishPayloadMaxBytes,
		logger, publishPayloadClock)

	installed := publishPayloadInstalled(t, publishPayloadHash)
	_, err := publisher.publishAs(context.Background(), installed, core.Actor{})
	require.Error(t, err, "载荷写失败必须拒绝发布")

	requireBoundedFinalize(t, ops.calls, core.OperationFailed)
}

// sameContentRecords 模拟"另一个 Worker 刚发布了同一份内容"：CAS 返回不成立且
// 不改指针，重读后指针已指向本版本。生产里这次 CAS 的事务整体回滚，pending 行的
// 终态必须由本进程回填——回填同样是审计收官，必须单独设预算。
type sameContentRecords struct {
	*fakeBundleRecords
}

func (f *sameContentRecords) PublishAgentBundle(context.Context, core.AgentUID,
	int64, int64, string, time.Time,
) (bool, error) {
	f.note("publish-pointer")
	f.publishCalls++
	return false, nil
}

// 中文验收点：同内容并发发布的成功收尾同样必须有界——这是用户报告的第二个
// `WithoutCancel` 分支（`compareAndSet` 重读后判成功处）。
// Acceptance point: the same-content success close is bounded too.
func TestPublish_SameContentCloseIsBounded(t *testing.T) {
	records := &sameContentRecords{fakeBundleRecords: newFakeBundleRecords()}
	// 指针已指向版本 7（`EnsureVersion` 的夹具 ID）：CAS 不成立后重读即命中同内容分支。
	records.pointerSet = true
	records.pointer = core.ResourcePointer{
		Kind:               core.ResourceAgentBundle,
		ResourceID:         string(publishPayloadUID),
		ResourceUID:        core.ResourceUID(publishPayloadUID),
		PublishedVersionID: 7,
	}
	payloads := newFakeBundlePayloads(&records.calls)
	ops := &deadlineRecordingOperations{fakeOperationLog: &fakeOperationLog{order: &records.calls}}
	logger, _ := newCapturedLogger()
	publisher := newBundlePublisher(records, ops, payloads, publishPayloadMaxBytes,
		logger, publishPayloadClock)

	installed := publishPayloadInstalled(t, publishPayloadHash)
	versionID, err := publisher.publishAs(context.Background(), installed, core.Actor{})
	require.NoError(t, err, "同内容并发发布按幂等成功收尾")
	require.Equal(t, int64(7), versionID)
	require.Equal(t, 1, records.publishCalls, "只做一次 CAS 尝试（同一内容不再重试）")

	requireBoundedFinalize(t, ops.calls, core.OperationSuccess)
}

// compile-time pin：替身必须仍满足发布器依赖的接口（接口变更时本文件先红）。
var (
	_ bundleRecordStore   = (*sameContentRecords)(nil)
	_ core.OperationStore = (*deadlineRecordingOperations)(nil)
)
