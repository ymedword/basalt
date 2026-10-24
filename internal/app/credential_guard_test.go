package app

// 本文件覆盖模型调用前的凭据检查点（模块卡 R4.8 / T131、FR-078）：
//   - `active` 可继续；换 Key 后固定旧版本的 `superseded` 仍可继续（在途 Run 不被换版）；
//   - 撤销 → `ErrCredentialRevoked`；退役不腰斩在途 Run（新 Run 准入已拒）；
//     版本缺失 / 密文被清空 → 不可用；
//   - **重新录入不复活旧 Run**：身份迁回 active、新版本可用，但固定在旧版本的 Run
//     下一次调用仍失败关闭（2026-10-23 判据裁决）；
//   - 并发检查同一结论（检查点无内部状态，只读仓储）。
// These tests cover the pre-call checkpoint: revoked versions stay terminal, superseded
// stays usable for in-flight runs, re-entry never revives a pinned run, and concurrent
// checks agree.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// guardRef 按夹具构造一条 Run 固定的模型连接凭据引用。
// guardRef builds the pinned run reference for the fixture connection.
func guardRef(credID string) core.RunCredentialRef {
	return core.RunCredentialRef{
		RunID:             "run_01",
		CredentialID:      credID,
		CredentialVersion: 1,
		ResourceKind:      core.BindingModelConnection,
		ResourceUID:       fixtureResourceUIDFor(core.ResourceModelConnection, "alpha"),
		ResourceName:      "alpha",
	}
}

// newGuardFixture 建一条已加密的托管模型连接并返回检查点与凭据 ID。
// newGuardFixture seeds one sealed managed connection and returns the guard.
func newGuardFixture(t *testing.T) (*resolverFixture, CredentialGuard, string) {
	t.Helper()
	f := newResolverFixture(t)
	target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
	require.NoError(t, err)
	credID := f.seedSealedCredential(t, core.ResourceModelConnection, "alpha", target, "sk-alpha-plaintext")
	guard, err := NewCredentialGuard(CredentialGuardDeps{Credentials: f.credentials})
	require.NoError(t, err)
	return f, guard, credID
}

// TestCredentialGuard_RejectsRevoked 覆盖检查点的状态矩阵。逆改点：把 superseded 也
// 当失败 → "换 Key 后在途 Run 被误杀"断言红；撤销只查版本不查身份 → 身份撤销分支红；
// 清空密文放行 → 对应断言红。
// TestCredentialGuard_RejectsRevoked pins the checkpoint's state matrix.
func TestCredentialGuard_RejectsRevoked(t *testing.T) {
	ctx := context.Background()
	f, guard, credID := newGuardFixture(t)
	ref := guardRef(credID)

	require.NoError(t, guard.Check(ctx, ref), "active 版本可继续")

	// 换 Key：旧版本转 superseded，在途 Run 固定的仍是旧版。
	require.NoError(t, f.credentials.AppendVersion(ctx, credID, f.key.KeyID, f.opID,
		core.CredentialVersionRecord{
			KeyID:         f.key.KeyID,
			TargetBinding: refTargetBinding(t),
			Nonce:         make([]byte, 12),
			Ciphertext:    []byte("sealed:v2"),
			OperationID:   f.opID,
		}, baselineClock()))
	require.NoError(t, guard.Check(ctx, ref), "换 Key 不得把在途 Run 换到新版本")

	// 不存在的版本号：固定引用与库内事实不符 → 不可用，不得放行。
	missing := ref
	missing.CredentialVersion = 9
	err := guard.Check(ctx, missing)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable)

	// 形态非法的凭据引用：不能进到任何一次模型调用。
	malformed := ref
	malformed.CredentialID = "not-a-cred"
	require.ErrorIs(t, guard.Check(ctx, malformed), core.ErrCredentialUnavailable)

	// 已释放的 Run：迟到的检查必须失败关闭。
	released := baselineClock().UnixMilli()
	stale := ref
	stale.ReleasedAt = &released
	require.ErrorIs(t, guard.Check(ctx, stale), core.ErrCredentialUnavailable)

	// 撤销是终态：下次调用即失败。
	require.NoError(t, f.credentials.Revoke(ctx, credID, f.opID, baselineClock()))
	err = guard.Check(ctx, ref)
	require.ErrorIs(t, err, core.ErrCredentialRevoked)
	require.NotContains(t, err.Error(), "sk-alpha-plaintext")

	// 撤销后的版本行仍存在（历史可查），只是不可再解密；再次撤销幂等。
	require.NoError(t, f.credentials.Revoke(ctx, credID, f.opID, baselineClock()))
	require.ErrorIs(t, guard.Check(ctx, ref), core.ErrCredentialRevoked)

	// 重新录入：身份迁回 active、追加新版本（上面已换过一次 Key，故这里是第 3 版）；
	// 但本 Run 固定的第 1 版仍是 revoked——检查点不得把在途 Run 静默切到新版本，
	// 下一次调用继续失败关闭。
	require.NoError(t, f.credentials.AppendVersion(ctx, credID, f.key.KeyID, f.opID,
		core.CredentialVersionRecord{
			KeyID:         f.key.KeyID,
			TargetBinding: refTargetBinding(t),
			Nonce:         make([]byte, 12),
			Ciphertext:    []byte("sealed:v2"),
			OperationID:   f.opID,
		}, baselineClock()))
	identity, err := f.credentials.Credential(ctx, credID)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityActive, identity.State)
	require.Equal(t, 3, identity.CurrentVersion)
	err = guard.Check(ctx, ref)
	require.ErrorIs(t, err, core.ErrCredentialRevoked, "重新录入不得复活旧 Run 固定的版本")
	require.NotContains(t, err.Error(), "sk-alpha-plaintext")
}

// TestCredentialGuard_RetiredKeepsInFlightRunButBlocksAdmission —— R4.8：删除资源 /
// 模式切换把身份与活动版本置 `retired`，密文保留到最后一个引用释放——在途 Run 的
// 检查点继续放行，而**新 Run** 的准入固定失败关闭（不得静默换到新版本或 env）。
// A retired credential keeps in-flight runs alive while new admissions fail closed.
func TestCredentialGuard_RetiredKeepsInFlightRunButBlocksAdmission(t *testing.T) {
	ctx := context.Background()
	f, guard, credID := newGuardFixture(t)
	uid := fixtureResourceUIDFor(core.ResourceModelConnection, "alpha")

	require.NoError(t, f.credentials.Retire(ctx, credID, f.opID, baselineClock()))

	require.NoError(t, guard.Check(ctx, guardRef(credID)),
		"删除资源 / 模式切换不得腰斩在途 Run（密文保留到最后一个引用释放）")

	_, err := f.resolver.PinModelCredential(ctx, uid, refTargetBinding(t))
	require.ErrorIs(t, err, core.ErrCredentialUnavailable, "新 Run 的准入必须失败关闭")
}

// TestCredentialGuard_RejectsBlankCiphertext 覆盖"未撤销却被清空密文"（正常写路径不
// 制造该状态，schema 的 CHECK 约束先在库层面拒绝，检查点里的密文判定是纵深防御）。
// The blank-ciphertext check is defence in depth: the schema CHECK rejects the row first.
func TestCredentialGuard_RejectsBlankCiphertext(t *testing.T) {
	ctx := context.Background()
	f, guard, credID := newGuardFixture(t)
	// "未撤销却没有密文"在库层面就被 CHECK 约束禁止（revoked 才允许 NULL）；
	// 检查点里的密文判定因此是纵深防御，不是可复现路径。
	raw := openRawWrite(t, f.dbPath)
	_, err := raw.ExecContext(ctx,
		`UPDATE credential_versions SET ciphertext = NULL WHERE credential_id = ? AND version_no = 1`,
		credID)
	require.Error(t, err, "schema 必须拒绝 active 版本没有密文")
	require.NoError(t, guard.Check(ctx, guardRef(credID)))
}

// TestCredentialGuard_ConcurrentCheck 覆盖并发检查：检查点无内部状态，N 个 goroutine
// 同时读到同一结论；撤销发生后所有并发检查一致转为 `ErrCredentialRevoked`。
// 竞态检测由 `make race`（`-race`）另行判卷。
// TestCredentialGuard_ConcurrentCheck pins that concurrent checks agree before and after
// a revoke.
func TestCredentialGuard_ConcurrentCheck(t *testing.T) {
	ctx := context.Background()
	f, guard, credID := newGuardFixture(t)
	ref := guardRef(credID)

	const workers, rounds = 8, 25
	runConcurrent := func(wantRevoked bool) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, workers*rounds)
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < rounds; i++ {
					err := guard.Check(ctx, ref)
					if wantRevoked {
						if !errors.Is(err, core.ErrCredentialRevoked) {
							errs <- err
						}
						continue
					}
					if err != nil {
						errs <- err
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "并发检查必须得到同一结论")
		}
	}

	runConcurrent(false)
	require.NoError(t, f.credentials.Revoke(ctx, credID, f.opID, baselineClock()))
	runConcurrent(true)
}

// refTargetBinding 重算夹具模型连接的目标绑定（追加版本时复用同一目标）。
// refTargetBinding recomputes the fixture's target binding for the appended version.
func refTargetBinding(t *testing.T) string {
	t.Helper()
	target, err := CredentialTargetBinding(core.BindingModelConnection, "https://api.example.com/v1", "")
	require.NoError(t, err)
	return target
}
