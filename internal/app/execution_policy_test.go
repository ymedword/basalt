package app

// 本文件判卷执行档位的管理服务（14D 设计 §2.2.1 / §2.2.3）：库是唯一权威源、配置只作
// 首次种子、条件写的三条判据（If-Match / expected_profile / 修订号 CAS）与失败路径的
// 零写入 + 操作标识。真实 SQLite + 真实管理操作仓储作底座，只有需要注入冲突的用例
// 才包一层假 store。
// Acceptance tests for the managed execution profile over a real SQLite database.

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
)

// profileEnv 是档位服务的共享夹具：真库 + 真管理操作仓储 + 真策略实例。
type profileEnv struct {
	service *ExecutionPolicyService
	store   *storage.ExecutionPolicyStore
	ops     *storage.ManagementStore
	db      *storage.DB
	policy  *tool.WhitelistPolicy
}

// profileClock 是夹具的固定时钟（写入时间可逐字断言）。
func profileClock() time.Time { return time.Date(2026, 10, 26, 9, 15, 0, 0, time.UTC) }

// newProfileEnv 装配夹具；`mutate` 可替换 store / 解析器 / 命令白名单。
func newProfileEnv(t *testing.T, mutate func(*ExecutionPolicyDeps)) *profileEnv {
	t.Helper()
	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(t.TempDir(), "basalt.db"), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	store := storage.NewExecutionPolicyStore(db)
	ops := storage.NewManagementStore(db)
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{Root: t.TempDir()})
	logger, _ := newCapturedLogger()

	deps := ExecutionPolicyDeps{
		Store:      store,
		Operations: ops,
		Policy:     policy,
		Logger:     logger,
		Now:        profileClock,
	}
	if mutate != nil {
		mutate(&deps)
	}
	if deps.Store == nil {
		deps.Store = store
	}
	if deps.Operations == nil {
		deps.Operations = ops
	}
	return &profileEnv{
		service: NewExecutionPolicyService(deps),
		store:   store,
		ops:     ops,
		db:      db,
		policy:  policy,
	}
}

// profileActor 是夹具固定的管理凭据主体。
func profileActor() core.Actor {
	return core.Actor{Subject: "token:def456", Method: "bearer", Source: "127.0.0.1:5000"}
}

// policyOperations 读回某条动作的全部管理操作行（按目标过滤）。
func (e *profileEnv) policyOperations(t *testing.T, action, targetID string) []core.OperationRecord {
	t.Helper()
	rows, err := e.ops.ListOperations(context.Background(), core.OperationFilter{
		Action: action, TargetID: targetID, Limit: 200,
	})
	require.NoError(t, err)
	return rows
}

// TestExecutionProfile_StorageIsRuntimeSource：新建库无档位行 → 首次种子按配置或
// `standard` 写入并记 `system.instance_init`；此后**配置不再覆盖**——第二个服务实例
// 即使拿到相反配置也只读库，且策略实例与生效快照同步。
// A fresh database gets its seed once; afterwards the config key is ignored and the
// live policy instance follows the row.
func TestExecutionProfile_StorageIsRuntimeSource(t *testing.T) {
	ctx := context.Background()

	// ① 新库无行（schema.sql 建表、不写行）：唯一能识别"新实例"的条件。
	env := newProfileEnv(t, nil)
	_, exists, err := env.store.Load(ctx)
	require.NoError(t, err)
	require.False(t, exists, "新建库不应预置档位行")

	// ② 未配置 → standard；来源与修订号如实。
	require.NoError(t, env.service.EnsureSeeded(ctx, ""))
	seeded := env.service.Current()
	require.Equal(t, core.ProfileStandard, seeded.Profile)
	require.Equal(t, core.NetworkAllowlistSourceInstanceInit, seeded.Source())
	require.Equal(t, core.SystemOperationInstanceInit, seeded.OperationID)
	require.Equal(t, int64(1), seeded.Revision)
	require.Equal(t, core.ProfileStandard, env.policy.ExecutionProfile(),
		"种子必须同时写进运行期策略实例")

	// ③ 第二个服务实例拿到相反配置：库是权威源，配置被忽略。
	second := NewExecutionPolicyService(ExecutionPolicyDeps{
		Store:      env.store,
		Operations: env.ops,
		Policy:     env.policy,
		Now:        profileClock,
	})
	require.NoError(t, second.EnsureSeeded(ctx, string(core.ProfileStrict)))
	require.Equal(t, core.ProfileStandard, second.Current().Profile,
		"已有行时配置不得覆盖库内档位")
	require.Equal(t, core.ProfileStandard, env.policy.ExecutionProfile())

	// ④ 种子行本身也来自库（重读一次确认落库，不是仅内存）。
	stored, exists, err := env.store.Load(ctx)
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.ProfileStandard, stored.Profile)
}

// conflictOnceStore 让第一次迁移冲突落败：模拟"另一进程在你读完 ETag 之后写了一次"。
type conflictOnceStore struct {
	*storage.ExecutionPolicyStore
	armed atomic.Bool
}

// Update 在 armed 时直接返回修订号冲突，其余转调真仓储。
// Update returns the revision conflict once, then forwards to the real store.
func (s *conflictOnceStore) Update(ctx context.Context, profile core.ExecutionProfile,
	expectedRevision int64, operationID, actor string, now time.Time,
) (core.ExecutionPolicy, error) {
	if s.armed.Swap(false) {
		return core.ExecutionPolicy{}, core.ErrPolicyRevisionConflict
	}
	return s.ExecutionPolicyStore.Update(ctx, profile, expectedRevision, operationID, actor, now)
}

// failingPreWriteOperations 让预写失败：服务**必须**拒绝执行且不留行。
type failingPreWriteOperations struct{ *storage.ManagementStore }

// BeginOperation 恒失败（宪法 V：预写失败不得发出写入）。
// BeginOperation always fails; the service must refuse to write.
func (failingPreWriteOperations) BeginOperation(context.Context, core.OperationRecord) error {
	return errors.New("pre-write refused by fixture")
}

// TestExecutionProfile_UpdateCAS：切换档位是一次条件写——`If-Match` 陈旧 412、
// `expected_profile` 不匹配 409 且零写入零操作行、预写失败拒绝执行、修订号 CAS
// 落败 409 带操作标识并重读权威行，成功后修订号 +1 且立即生效。
// The switch is one conditional write; every precondition failure leaves the row and
// the operation ledger untouched.
func TestExecutionProfile_UpdateCAS(t *testing.T) {
	ctx := context.Background()
	env := newProfileEnv(t, nil)
	require.NoError(t, env.service.EnsureSeeded(ctx, ""))
	standard := env.service.Current()
	standardETag := core.ExecutionPolicyETag(standard)
	actor := profileActor()

	// ① If-Match 陈旧：412、零写入、零操作行。
	_, operationID, err := env.service.Update(ctx, core.ProfileStrict, core.ProfileStandard,
		`"sha256:deadbeef"`, actor)
	require.ErrorIs(t, err, core.ErrPreconditionFailed)
	require.Empty(t, operationID, "前置条件失败发生在预写之前，没有操作行")
	require.Empty(t, env.policyOperations(t, core.ActionExecutionPolicyUpdate, "global"))
	require.Equal(t, core.ProfileStandard, env.service.Current().Profile)

	// ② 方向确认落空：页面以为当前是 strict，库里是 standard → 409 且零写入。
	_, operationID, err = env.service.Update(ctx, core.ProfileStandard, core.ProfileStrict,
		standardETag, actor)
	require.ErrorIs(t, err, core.ErrExecutionProfileMismatch)
	require.Empty(t, operationID)
	require.Empty(t, env.policyOperations(t, core.ActionExecutionPolicyUpdate, "global"))
	require.Equal(t, int64(1), env.service.Current().Revision)

	// ③ 非法档位：纯请求校验，零写入零操作行。
	_, _, err = env.service.Update(ctx, core.ExecutionProfile("lenient"), core.ProfileStandard,
		standardETag, actor)
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)

	// ④ 预写失败 → 拒绝执行（宪法 V）：零操作行、档位不变。
	broken := NewExecutionPolicyService(ExecutionPolicyDeps{
		Store:      env.store,
		Operations: failingPreWriteOperations{env.ops},
		Policy:     env.policy,
		Now:        profileClock,
	})
	require.NoError(t, broken.EnsureSeeded(ctx, ""))
	_, _, err = broken.Update(ctx, core.ProfileStrict, core.ProfileStandard, standardETag, actor)
	require.ErrorContains(t, err, "pre-write operation record failed")
	require.Empty(t, env.policyOperations(t, core.ActionExecutionPolicyUpdate, "global"))
	require.Equal(t, core.ProfileStandard, env.service.Current().Profile)

	// ⑤ 修订号 CAS 落败：预写之后失败 → 带操作标识、置 failed、重读权威行且不写库。
	conflict := &conflictOnceStore{ExecutionPolicyStore: env.store}
	casService := NewExecutionPolicyService(ExecutionPolicyDeps{
		Store:      conflict,
		Operations: env.ops,
		Policy:     env.policy,
		Now:        profileClock,
	})
	require.NoError(t, casService.EnsureSeeded(ctx, ""))
	conflict.armed.Store(true)
	_, operationID, err = casService.Update(ctx, core.ProfileStrict, core.ProfileStandard,
		standardETag, actor)
	require.ErrorIs(t, err, core.ErrPolicyRevisionConflict)
	require.NotEmpty(t, operationID, "预写之后的失败必须带回操作标识")
	rows := env.policyOperations(t, core.ActionExecutionPolicyUpdate, "global")
	require.Len(t, rows, 1)
	require.Equal(t, core.OperationFailed, rows[0].Status)
	require.Equal(t, core.ProfileStandard, casService.Current().Profile, "落败后重读权威行")
	require.Equal(t, int64(1), casService.Current().Revision)

	// ⑥ 正常切换：修订号 +1、立即生效、操作行 success 且 detail 只含前后档位与修订号。
	updated, operationID, err := env.service.Update(ctx, core.ProfileStrict, core.ProfileStandard,
		standardETag, actor)
	require.NoError(t, err)
	require.NotEmpty(t, operationID)
	require.Equal(t, core.ProfileStrict, updated.Profile)
	require.Equal(t, int64(2), updated.Revision)
	require.Equal(t, core.ProfileStrict, env.policy.ExecutionProfile(),
		"保存成功即对下一次判定生效")
	row, getErr := env.ops.GetOperation(ctx, operationID)
	require.NoError(t, getErr)
	require.Equal(t, core.ActionExecutionPolicyUpdate, row.Action)
	require.Equal(t, core.TargetKindExecutionPolicy, row.TargetKind)
	require.Equal(t, "global", row.TargetID)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Contains(t, string(row.DetailJSON), `"after_profile":"strict"`)
	require.Contains(t, string(row.DetailJSON), `"after_revision":2`)

	// ⑦ 新 ETag 随之变化：页面的下一次条件写必须用新值。
	require.NotEqual(t, standardETag, core.ExecutionPolicyETag(updated))
}
