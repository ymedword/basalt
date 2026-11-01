package core

// 本文件守 C-32 / 设计卡 §2.14 / §3.4.2 的**后台执行授权归属**：调度器按任务行上的
// `(caller_id, authorizing_kind, authorizing_key_id)` 重建执行身份，归属永远是任务
// 的归属方，执行者单独标 `scheduler`，**不得**提升成内置管理员；caller-key 任务执行
// 前必须经复核端口重读**那一把**原授权 Key。
// This file guards the scheduled-execution ownership rules: identity comes from the task
// row, the executor is marked scheduler, and caller-key tasks re-read that exact key.

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeOwnerKeys 是 `OwnerKeyVerifier` 的测试替身：记录每次复核的入参，并可注入
// 拒绝结果。它的 scope 读数用于证明调度器拿到的是**这一次复核**的结果，而不是
// 任务行上残留的旧快照。
// fakeOwnerKeys records verification arguments and can be made to reject.
type fakeOwnerKeys struct {
	mu     sync.Mutex
	scopes []string
	err    error
	calls  [][3]string // callerID, keyID, agent
}

// VerifyOwnerKey 记录入参后按注入结果返回。
func (f *fakeOwnerKeys) VerifyOwnerKey(_ context.Context, callerID, keyID, agent string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [3]string{callerID, keyID, agent})
	if f.err != nil {
		return nil, f.err
	}
	return f.scopes, nil
}

// verifications 返回复核入参副本。
func (f *fakeOwnerKeys) verifications() [][3]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][3]string(nil), f.calls...)
}

// TestScheduler_ExecutionKeepsTaskOwner 守 C-32 / §3.4.2：调度执行的身份归属恒为
// 任务行上的调用方，执行者标注 `scheduler`，既不是内置管理员，也不是"谁触发谁执行"。
// 复核端口必须按 `(caller_id, key_id, agent)` 三元组读**那一把** Key——按调用方
// "随便找一把有效 Key"顶替会让越权 Key 复活。
// 逆改点：用 `LocalAdminPrincipal()` 建身份 → CallerID 变 local-admin 且 Builtin=true；
// 丢弃 `AuthorizingKeyID`（改传空）→ 复核入参三元组断言红。
func TestScheduler_ExecutionKeepsTaskOwner(t *testing.T) {
	verifier := &fakeOwnerKeys{scopes: []string{AgentInvokeScopePrefix + schedAgent}}
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.OwnerKeys = verifier
	})
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
		CallerID:         "caller-2",
		AuthorizingKind:  AuthorizingKindCallerKey,
		AuthorizingKeyID: "key-c2",
	})
	f.start(t)
	f.fire(t, schedTaskID)

	reqs := f.invoker.requests()
	require.Len(t, reqs, 1, "授权可满足时必须真的发起一次运行")
	p := reqs[0].Principal
	require.Equal(t, "caller-2", p.CallerID, "调度不得改写任务归属")
	require.Equal(t, ActorScheduler, p.Actor, "执行者是调度，不是外部调用")
	require.False(t, p.Builtin, "后台执行不得提升成内置管理员")
	require.Equal(t, PrincipalCaller, p.Kind)
	require.Equal(t, "key-c2", p.KeyID, "执行授权绑定的是任务的原授权 Key")

	require.Equal(t, [][3]string{{"caller-2", "key-c2", schedAgent}}, verifier.verifications(),
		"复核端口必须收到任务行上的归属三元组")
	require.True(t, p.HasAgentScope(schedAgent), "身份携带的是复核返回的当前 scope")
}

// TestScheduler_LocalTrustNotTreatedAsDisabled 守 §2.14：启动期管理认证姿态里
// `local_trust` 与 `token` 都是**合法启用态**——把 local_trust 读成"管理认证关闭"
// 会让本机模式下的 admin-builtin 任务集体停摆。只有 `unavailable` 才拒绝。
// 逆改点：拒绝条件写成 `posture != AdminPostureToken`（或把 local_trust 归入
// unavailable）→ 本轮不发起运行，调用计数为 0 且执行行原因是 admin_auth_unavailable。
func TestScheduler_LocalTrustNotTreatedAsDisabled(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.AdminPosture = func() AdminPosture { return AdminPostureLocalTrust }
	})
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
	})
	f.start(t)
	f.fire(t, schedTaskID)

	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1, "到点必须落一条执行行")
	require.NotEqual(t, schedulerAdminAuthUnavailable, finishes[0].ErrMsg,
		"local_trust 是合法启用态，不得被当成管理认证关闭")
	require.Equal(t, int32(1), f.invoker.calls.Load(), "任务必须真能跑")
}
