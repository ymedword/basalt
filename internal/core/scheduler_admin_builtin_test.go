package core

// 本文件守 C-32 的 **admin-builtin 与 caller-key 二分**（设计卡 §2.14 / §3.4.2）：
// 授权来源只由任务行上的 `authorizing_kind` 决定，**不得**按"有没有 key_id"推断；
// 管理认证姿态里 `token` 与 `local_trust` 都是合法启用态，只有 `unavailable` 拒绝；
// caller-key 任务执行前必须复核**那一把** Key，被拒后既不得借用管理员，也不得改用
// 同一调用方的另一把 Key。
// This file guards the admin-builtin / caller-key split, both valid admin postures, and
// the no-substitution rule for caller-key tasks.

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedRunnableTask 种一条可触发任务行；归属与授权来源显式给出，不用默认值，
// 免得断言在默认值漂移时静默跟着变。
// seedRunnableTask seeds a runnable task row with explicit ownership.
func seedRunnableTask(f *schedFixture, callerID string, kind AuthorizingKind, keyID string) {
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
		CallerID:         callerID,
		AuthorizingKind:  kind,
		AuthorizingKeyID: keyID,
	})
}

// TestScheduler_AdminBuiltinTaskRuns 守"部署内置管理员任务真的能触发"：归属
// `local-admin`、无 Key、无 scope，执行者仍是 `scheduler`——不得回落成外部调用的
// `caller` 身份，否则审计分不清是谁触发的。
// 逆改点：改用 `LocalAdminPrincipal()` 直接建身份（Actor 变 caller）→ 执行者断言红；
// 把 admin-builtin 归入 `unavailable` 分支 → 运行计数 0、执行行 failed。
func TestScheduler_AdminBuiltinTaskRuns(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	seedRunnableTask(f, LocalAdminCallerID, AuthorizingKindAdminBuiltin, "")
	f.start(t)
	f.fire(t, schedTaskID)

	reqs := f.invoker.requests()
	require.Len(t, reqs, 1, "admin-builtin 任务必须真的发起一次运行")
	p := reqs[0].Principal
	require.Equal(t, PrincipalLocalAdmin, p.Kind)
	require.True(t, p.Builtin)
	require.Equal(t, LocalAdminCallerID, p.CallerID)
	require.Equal(t, ActorScheduler, p.Actor, "后台管理员任务的执行者必须标 scheduler")
	require.Empty(t, p.KeyID, "admin-builtin 不绑定任何调用方 Key")

	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionCompleted, finishes[0].Status)
}

// TestScheduler_AdminBuiltinRunsUnderBothAuthModes 守 §2.14：`token` 与 `local_trust`
// 都是**合法启用态**，两者下 admin-builtin 任务都必须能跑。把 local_trust 读成
// "管理认证关闭"会让本机部署的五个内置 Agent 集体停摆。
// 逆改点：拒绝条件写成 `posture != AdminPostureToken` → local_trust 组运行计数 0、红。
func TestScheduler_AdminBuiltinRunsUnderBothAuthModes(t *testing.T) {
	for _, posture := range []AdminPosture{AdminPostureToken, AdminPostureLocalTrust} {
		t.Run(string(posture), func(t *testing.T) {
			f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
				opts.AdminPosture = func() AdminPosture { return posture }
			})
			seedRunnableTask(f, LocalAdminCallerID, AuthorizingKindAdminBuiltin, "")
			f.start(t)
			f.fire(t, schedTaskID)

			require.Equal(t, int32(1), f.invoker.calls.Load(), "合法启用态下任务必须能跑")
			finishes := f.store.snapshotFinishCalls()
			require.Len(t, finishes, 1)
			require.NotEqual(t, schedulerAdminAuthUnavailable, finishes[0].ErrMsg)
		})
	}
}

// TestScheduler_AdminBuiltinRefusedWhenAdminUnavailable 守"只有 unavailable 才拒绝"：
// 管理能力真正不可用时，admin-builtin 任务**不得**照常出网——本轮不发起运行，
// 执行行收尾 failed 且原因码固定 `admin_auth_unavailable`（与归属失效分开）。
// 逆改点：删掉 `adminPostureUnavailable` 分支 → 运行计数 1、红；原因码写成
// `owner_unauthorized` → 原因断言红（运维会去查错的那一侧）。
func TestScheduler_AdminBuiltinRefusedWhenAdminUnavailable(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.AdminPosture = func() AdminPosture { return AdminPostureUnavailable }
	})
	seedRunnableTask(f, LocalAdminCallerID, AuthorizingKindAdminBuiltin, "")
	f.start(t)
	f.fire(t, schedTaskID)

	require.Zero(t, f.invoker.calls.Load(), "管理能力不可用时不得发起运行")
	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1, "拒绝也要留下执行行")
	require.Equal(t, ExecutionFailed, finishes[0].Status)
	require.Equal(t, schedulerAdminAuthUnavailable, finishes[0].ErrMsg,
		"管理不可用是与归属失效分开的原因码")
}

// TestScheduler_AdminBuiltForCallerBindsCallerKey 守"管理员代建面向某调用方的任务
// **不得**走 admin-builtin"：这类任务在创建期就绑定该调用方一把现存有效 Key，
// 执行时归属与授权都是该调用方——内置管理员能力不外溢给业务用量。
// 逆改点：把代建任务写成 `local-admin` + admin-builtin → Builtin=true、归属断言红；
// 丢弃 `AuthorizingKeyID` → 复核三元组断言红。
func TestScheduler_AdminBuiltForCallerBindsCallerKey(t *testing.T) {
	verifier := &fakeOwnerKeys{scopes: []string{AgentInvokeScopePrefix + schedAgent}}
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.OwnerKeys = verifier
	})
	seedRunnableTask(f, "caller-built", AuthorizingKindCallerKey, "key-built")
	f.start(t)
	f.fire(t, schedTaskID)

	reqs := f.invoker.requests()
	require.Len(t, reqs, 1)
	p := reqs[0].Principal
	require.False(t, p.Builtin, "管理员代建的任务不得继承内置管理员身份")
	require.Equal(t, "caller-built", p.CallerID, "归属是任务面向的调用方")
	require.Equal(t, "key-built", p.KeyID, "授权绑定创建期写入的那把 Key")
	require.Equal(t, ActorScheduler, p.Actor)
	require.Equal(t, [][3]string{{"caller-built", "key-built", schedAgent}}, verifier.verifications())
}

// TestScheduler_CallerKeyTaskDoesNotBorrowAdmin 守"归属正确不等于授权仍在"：原 Key
// 被撤销时，即使管理能力完全可用、同一调用方还有别的 Key，任务也必须失败关闭——
// 不借用管理员、不改用别把 Key、不跳过校验出网。
// 逆改点：复核失败后回落到管理员身份 → 运行计数 1、Builtin 断言红；改用同调用方
// 另一把 Key 重试 → 复核入参断言红。
func TestScheduler_CallerKeyTaskDoesNotBorrowAdmin(t *testing.T) {
	verifier := &fakeOwnerKeys{err: errors.New("authorizing key revoked")}
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.OwnerKeys = verifier
		// 管理能力满血可用：借不借管理员，完全由授权来源决定。
		opts.AdminPosture = func() AdminPosture { return AdminPostureToken }
	})
	seedRunnableTask(f, "caller-3", AuthorizingKindCallerKey, "key-c3")
	f.start(t)
	f.fire(t, schedTaskID)

	require.Zero(t, f.invoker.calls.Load(), "原 Key 不可用时不得以任何身份出网")
	require.Equal(t, [][3]string{{"caller-3", "key-c3", schedAgent}}, verifier.verifications(),
		"复核只针对任务行上那一把 Key，不得改用同调用方的其他 Key")
	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionFailed, finishes[0].Status)
	require.Equal(t, schedulerOwnerUnauthorized, finishes[0].ErrMsg)
}

// TestScheduler_RevokedAuthorizingKeyFailsClosed 守 caller-key 的失败关闭：复核端口
// 报"已撤销 / 已过期 / scope 不再满足"时，本轮不执行、不留 Run、原因码
// `owner_unauthorized`——不得因为同调用方还有别的 Key 就继续。
// 逆改点：把复核错误当成"跳过校验"继续 → 运行计数 1、红。
func TestScheduler_RevokedAuthorizingKeyFailsClosed(t *testing.T) {
	verifier := &fakeOwnerKeys{err: errors.New("key expired")}
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, func(opts *SchedulerOptions) {
		opts.OwnerKeys = verifier
	})
	seedRunnableTask(f, "caller-4", AuthorizingKindCallerKey, "key-c4")
	f.start(t)
	f.fire(t, schedTaskID)

	require.Zero(t, f.invoker.calls.Load())
	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionFailed, finishes[0].Status)
	require.Equal(t, schedulerOwnerUnauthorized, finishes[0].ErrMsg)
}

// TestScheduler_LegacyMigratedTaskIsAdminBuiltin 守 SQLite → PG 迁移后的存量任务：
// `internal/dbmigrate/import.go` 的常量映射把它们一律补成 `caller_id='local-admin'` +
// `authorizing_kind='admin-builtin'`（存量期没有调用方 Key）。这类任务没有
// `authorizing_key_id`，**不得**被当成 `key_missing` 而永远触发不了。
// 逆改点：按"有没有 key_id"推断授权来源 → 执行行 failed + key 缺失原因，红。
func TestScheduler_LegacyMigratedTaskIsAdminBuiltin(t *testing.T) {
	f := newSchedFixture(t, []Schedule{demoTask(schedTaskID)}, nil)
	// 逐字按迁移常量映射构造：caller_id 与 authorizing_kind 是常量 fill，key 列为空。
	f.store.seedTask(TaskRecord{
		TaskID: schedTaskID, Agent: schedAgent, Cron: schedCron, Zone: "UTC",
		Message: schedMessage, Enabled: true,
		CallerID:         "local-admin",
		AuthorizingKind:  AuthorizingKindAdminBuiltin,
		AuthorizingKeyID: "",
	})
	f.start(t)
	f.fire(t, schedTaskID)

	reqs := f.invoker.requests()
	require.Len(t, reqs, 1, "存量迁移任务必须能照常触发")
	p := reqs[0].Principal
	require.True(t, p.Builtin)
	require.Equal(t, ActorScheduler, p.Actor)
	finishes := f.store.snapshotFinishCalls()
	require.Len(t, finishes, 1)
	require.Equal(t, ExecutionCompleted, finishes[0].Status)
}
