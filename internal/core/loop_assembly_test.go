package core_test

// 本文件是 V-7「上下文装配记录接线」的循环侧判据（2026-10-30 用户裁决）：记录必须在
// **每轮恰好一次**、在模型调用**之前**、在压缩结论**并入之后**交给写入器；Run 的结局
// 不得因为记录而改变（端口无错误返回，写失败由写入器降级为 WARN）。
//
// 三条判据分别对应三个逆改点：
//   - 少写 / 多写一条（把调用移到循环外、或在失败路径上补一条）→ 条数断言红；
//   - 在模型调用之后才写（"调用完再补记录"）→ `callsAtRecord` 的次序断言红；
//   - 丢掉压缩结论（在 `record.Compression = outcome.record` 之前交出记录）→ 压缩段断言红。
//
// This file pins the loop side of V-7: one record per round, written before the model
// call and after the compression verdict is merged; the run's outcome never depends on
// the record.

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// recordingAssembly 是 `core.AssemblyRecorder` 的录制替身。
// `callsAtRecord` 在每次 Record 时记下假模型已收到的调用次数——位移一位就说明记录
// 被挪到了调用之后（第一条变成 1 而不是 0）。
// recordingAssembly records every hand-over and how many model calls had happened by
// then; a shift by one means the record moved after the call.
type recordingAssembly struct {
	mu            sync.Mutex
	records       []core.AssemblyRecord
	callsAtRecord []int
	// observe 在 Record 时刻取"当前已发生的模型调用次数"；nil = 不记次序。
	observe func() int
}

// Record 实现 `core.AssemblyRecorder`（无错误返回：记录可丢弃）。
func (r *recordingAssembly) Record(_ context.Context, record core.AssemblyRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
	if r.observe != nil {
		r.callsAtRecord = append(r.callsAtRecord, r.observe())
	}
}

// snapshot 返回录到的记录副本。
func (r *recordingAssembly) snapshot() []core.AssemblyRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]core.AssemblyRecord(nil), r.records...)
}

// calls 返回每次 Record 时刻的模型调用计数。
func (r *recordingAssembly) calls() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.callsAtRecord...)
}

// TestReActLoop_AssemblyRecordOncePerRound 守"每轮一条、字段由循环补齐"：
// 两轮（工具 + 收尾）必须产生两条记录，Run / 轮次 / 用途 / Agent / 模型逐条对得上，
// 且每一条都写在本轮的模型调用**之前**。
// 逆改点：记录挪到循环外（条数 1）；挪到调用之后（callsAtRecord 变成 [1,2]）；
// 不填 Agent / ModelVersion / StepID（对应字段断言红）。
func TestReActLoop_AssemblyRecordOncePerRound(t *testing.T) {
	rec := &recordingAssembly{}
	h := newLoopHarness(t, withAssemblyRecorder(rec))
	rec.observe = h.provider.callCount
	h.provider.onCall(0, toolCallResponse("c1", "read_file", `{"path":"a.txt"}`))
	h.provider.onCall(1, textResponse("done"))

	res, err := h.service.Run(ctx, h.req("读一下并总结"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status)

	records := rec.snapshot()
	require.Len(t, records, 2, "每轮恰好一条装配记录（两轮模型调用 = 两条记录）")
	require.Equal(t, []int{0, 1}, rec.calls(), "记录必须写在本轮模型调用之前")

	for i, record := range records {
		require.Equal(t, res.RunID, record.RunID, "记录挂在本次 Run 上")
		require.Equal(t, strconv.Itoa(i+1), record.StepID,
			"step_id 取轮次（与配额幂等键 run:<id>:chat:<round>:a1 同一口径）")
		require.Equal(t, core.PurposeChat, record.Purpose, "对话装配的用途固定 chat")
		require.Equal(t, loopAgentName, record.Agent, "Agent 名取自冻结快照")
		require.Equal(t, "fake-model", record.ModelVersion, "模型取快照里的 Provider 绑定")
		require.Zero(t, record.LLMCallID, "装配发生在调用之前：调用标识此时还不存在")
		require.Equal(t, core.EstimateVersion, record.EstimatorVersion, "估算器版本如实入账")
		require.NotEmpty(t, record.FinalSegments, "本轮实际注入的段落必须记下来")
		require.Equal(t, core.DecisionNone, record.Compression.Decision,
			"压缩未接线时的真实结论是 none，不是空串")
	}
}

// TestReActLoop_AssemblyRecordCarriesCompressionVerdict 守"压缩结论随记录交出去"：
// 循环必须在 `record.Compression` 并入**之后**才交出记录——提前交出去会让每一条
// 记录都声称"没有压缩"，与压缩器实际给出的降级结论矛盾。
// 逆改点：把 recordAssembly 移到 `record.Compression = outcome.record` 之前 → 本用例红。
func TestReActLoop_AssemblyRecordCarriesCompressionVerdict(t *testing.T) {
	rec := &recordingAssembly{}
	h := newLoopHarness(t,
		withAssemblyRecorder(rec),
		withCompaction(700, func(*core.RunEnv) core.ContextCompactor { return degradedCompactor{} }),
	)
	h.provider.onCall(0, textResponse("好的"))

	res, err := h.service.Run(ctx, h.req("说话"))
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "降级是结果，不是失败")

	records := rec.snapshot()
	require.Len(t, records, 1)
	require.True(t, records[0].Compression.Enabled, "压缩已接线，记录必须如实标 enabled")
	require.Equal(t, core.DecisionDegraded, records[0].Compression.Decision)
	require.Equal(t, "summary_unavailable", records[0].Compression.ReasonCode,
		"降级原因码必须随记录落账（不得只留在日志里）")
}

// TestReActLoop_AssemblyRecordKeptWhenModelCallFails 守"装配过就有记录，失败不补写"：
// 模型调用失败时，本轮装配确实发生过——记录必须保留；失败路径也不得再补一条。
// 逆改点：把写入挪到调用成功之后（记录 0 条）或失败分支里重写（记录 2 条）。
func TestReActLoop_AssemblyRecordKeptWhenModelCallFails(t *testing.T) {
	rec := &recordingAssembly{}
	h := newLoopHarness(t, withAssemblyRecorder(rec))
	h.provider.onError(0, errors.New("provider down"))

	res, err := h.service.Run(ctx, h.req("问一句"))
	require.NoError(t, err, "运行期失败体现在 Status 上")
	require.Equal(t, core.StatusFailed, res.Status)

	records := rec.snapshot()
	require.Len(t, records, 1, "装配发生过就有且只有一条记录；失败分支不得再补一条")
	require.Equal(t, "1", records[0].StepID)
}

// TestReActLoop_AssemblyRecordAbsentWhenBuildFails 守"没装配成功就没有记录"：
// 上下文超限是装配期拒绝（零模型调用），此时不存在"装配过"的事实——写一条记录会
// 让诊断看到一轮从未发生的装配。
// 逆改点：在 Build 的 error 分支里补写记录 → 条数断言红。
func TestReActLoop_AssemblyRecordAbsentWhenBuildFails(t *testing.T) {
	rec := &recordingAssembly{}
	h := newLoopHarness(t, withAssemblyRecorder(rec))
	h.budget.ContextTokens = 10 // 不可裁段就超限：Build 直接拒绝
	h.provider.onCall(0, textResponse("不该被调用"))

	res, err := h.service.Run(ctx, h.req("问一句"))
	require.NoError(t, err)
	require.Equal(t, core.StatusFailed, res.Status)
	require.Zero(t, h.provider.callCount(), "装配失败 = 零模型调用")
	require.Empty(t, rec.snapshot(), "没有装配成功就没有装配记录")
}

// degradedCompactor 是恒返回降级结论的压缩器替身（接线判据用，不测算法）。
// degradedCompactor always reports a degraded verdict.
type degradedCompactor struct{}

// Compact 恒返回 `Degraded` + 固定原因码（真实引擎的原因码由 compaction 包判卷）。
func (degradedCompactor) Compact(context.Context, core.CompactRequest) (core.CompactResult, error) {
	return core.CompactResult{Degraded: true, ReasonCode: "summary_unavailable"}, nil
}
