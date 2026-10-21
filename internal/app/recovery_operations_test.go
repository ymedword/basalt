package app

// 本文件是启动恢复的验收面（颗粒度文档 §3.2.1：internal/app/recovery_operations_test.go，
// 10 例）。口径是约束 38：**只查证、不重做**——content.update 三态、disable / enable 三态、
// 无结论保留 unknown、已终态的行不被覆盖、`recovered_at` / `verification` 回填。
// 恢复在开放服务前单线程执行，全部用例离线。
// This file covers startup recovery: verify only, never replay.

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// recoveryEnv 是一套恢复夹具：临时工作区 + 临时库 + 固定时钟 + captured logger。
type recoveryEnv struct {
	deps  RecoveryDeps
	store *storage.ManagementStore
	root  string
	logs  *bytes.Buffer
}

// newRecoveryEnv 建夹具；`Editor` 与 `Lifecycle` 都落在同一临时工作区上。
func newRecoveryEnv(t *testing.T) *recoveryEnv {
	t.Helper()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(root, databaseFileName), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	store := storage.NewManagementStore(db)
	logger, logs := newCapturedLogger()
	return &recoveryEnv{
		root:  root,
		store: store,
		logs:  logs,
		deps: RecoveryDeps{
			Operations: store,
			Resources:  storage.NewResourceStore(db),
			Lifecycle:  workspace.NewLifecycleStore(root, baselineClock),
			Editor:     workspace.NewAgentEditor(workspace.AgentEditorDeps{Root: root}),
			Logger:     logger,
			Now:        baselineClock,
		},
	}
}

// beginPendingOperation 预写一条 pending 行：可空列零值由仓储映射 NULL。
func beginPendingOperation(t *testing.T, store core.OperationStore, rec core.OperationRecord) {
	t.Helper()
	if rec.OperationID == "" {
		rec.OperationID = core.NewOperationID(baselineClock())
	}
	if rec.ActorSubject == "" {
		rec.ActorSubject = core.ActorSubjectLocalTrust
	}
	if rec.AuthMethod == "" {
		rec.AuthMethod = core.AuthMethodLocalTrust
	}
	if rec.Source == "" {
		rec.Source = "127.0.0.1:1"
	}
	rec.Status = core.OperationPending
	rec.CreatedAt = baselineClock()
	require.NoError(t, store.BeginOperation(context.Background(), rec), "预写必须成功")
}

// mustDetailJSON 把恢复需要的 detail 投影序列化（与生产侧写入口同一键名）。
func mustDetailJSON(t *testing.T, detail operationDetail) []byte {
	t.Helper()
	raw, err := json.Marshal(detail)
	require.NoError(t, err)
	return raw
}

// getOperation 按标识取回一行（判 status / verification / recovered_at / detail 用）。
func getOperation(t *testing.T, store core.OperationStore, operationID string) core.OperationRecord {
	t.Helper()
	op, err := store.GetOperation(context.Background(), operationID)
	require.NoError(t, err)
	return op
}

// expectedState / expectedGeneration 构造 detail 的可空字段。
func expectedState(state core.AgentState) *string {
	value := string(state)
	return &value
}

func expectedGeneration(generation int64) *int64 {
	return &generation
}

// contentUpdateOperation 预写一条 `agent.content.update`，返回其标识。
func contentUpdateOperation(t *testing.T, env *recoveryEnv, detail operationDetail, expectedETag string) string {
	t.Helper()
	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID:  operationID,
		Action:       actionContentUpdate,
		TargetKind:   targetKindAgent,
		TargetID:     "writer",
		ExpectedETag: expectedETag,
		DetailJSON:   mustDetailJSON(t, detail),
	})
	return operationID
}

// TestRecoverPendingOperations_ContentUpdateApplied：磁盘哈希 = 候选哈希 → applied / success。
func TestRecoverPendingOperations_ContentUpdateApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	previous := []byte("---\nname: writer\n---\n\n旧正文\n")
	candidate := []byte("---\nname: writer\n---\n\n新正文\n")
	writeWorkspaceFile(t, env.root, "agents/writer/AGENT.md", string(candidate))

	operationID := contentUpdateOperation(t, env, operationDetail{
		CandidateSHA256: sha256Hex(t, candidate),
		PreviousSHA256:  sha256Hex(t, previous),
	}, "")
	require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, core.VerificationApplied, op.Verification)
	require.False(t, op.RecoveredAt.IsZero(), "恢复必须记下查证时间")
	require.False(t, op.FinishedAt.IsZero())
}

// TestRecoverPendingOperations_ContentUpdateNotAppliedByPreviousHash：磁盘仍是旧哈希 →
// not_applied / failed（变更未生效，不得标成功）。
func TestRecoverPendingOperations_ContentUpdateNotAppliedByPreviousHash(t *testing.T) {
	env := newRecoveryEnv(t)
	previous := []byte("---\nname: writer\n---\n\n旧正文\n")
	candidate := []byte("---\nname: writer\n---\n\n新正文\n")
	writeWorkspaceFile(t, env.root, "agents/writer/AGENT.md", string(previous))

	operationID := contentUpdateOperation(t, env, operationDetail{
		CandidateSHA256: sha256Hex(t, candidate),
		PreviousSHA256:  sha256Hex(t, previous),
	}, "")
	require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)
	require.False(t, op.RecoveredAt.IsZero())
}

// TestRecoverPendingOperations_ContentUpdateNotAppliedByExpectedETag：detail 里没有旧哈希，
// 但磁盘 ETag 等于条件写的期望值 → 同样判 not_applied。
func TestRecoverPendingOperations_ContentUpdateNotAppliedByExpectedETag(t *testing.T) {
	env := newRecoveryEnv(t)
	previous := []byte("---\nname: writer\n---\n\n旧正文\n")
	candidate := []byte("---\nname: writer\n---\n\n新正文\n")
	writeWorkspaceFile(t, env.root, "agents/writer/AGENT.md", string(previous))

	operationID := contentUpdateOperation(t, env,
		operationDetail{CandidateSHA256: sha256Hex(t, candidate)},
		workspace.ContentETag(previous))
	require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)
}

// TestRecoverPendingOperations_ContentUpdateNotAppliedWhenFileMissing：目标文件不存在属于
// "变更未生效"（契约 §9.4），不是"读不出来"。
func TestRecoverPendingOperations_ContentUpdateNotAppliedWhenFileMissing(t *testing.T) {
	env := newRecoveryEnv(t)
	candidate := []byte("---\nname: writer\n---\n\n新正文\n")

	operationID := contentUpdateOperation(t, env, operationDetail{
		CandidateSHA256: sha256Hex(t, candidate),
	}, "")
	require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)
}

// TestRecoverPendingOperations_ContentUpdateIndeterminateKeepsDetail：第三方改写让两个哈希都
// 对不上 → indeterminate / unknown，**原样保留** detail_json（查证依据不得被清掉）。
func TestRecoverPendingOperations_ContentUpdateIndeterminateKeepsDetail(t *testing.T) {
	env := newRecoveryEnv(t)
	previous := []byte("---\nname: writer\n---\n\n旧正文\n")
	candidate := []byte("---\nname: writer\n---\n\n新正文\n")
	thirdParty := []byte("---\nname: writer\n---\n\n别人改的\n")
	writeWorkspaceFile(t, env.root, "agents/writer/AGENT.md", string(thirdParty))

	detail := mustDetailJSON(t, operationDetail{
		CandidateSHA256: sha256Hex(t, candidate),
		PreviousSHA256:  sha256Hex(t, previous),
	})
	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionContentUpdate,
		TargetKind:  targetKindAgent,
		TargetID:    "writer",
		DetailJSON:  detail,
	})
	require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)
	require.Equal(t, string(detail), string(op.DetailJSON), "unknown 必须保留查证依据")
	require.Contains(t, env.logs.String(), "无法判定")
}

// TestRecoverPendingOperations_DisableApplied：状态文件已是目标状态、代数 +1、operation_id
// 指向本次操作 → applied / success。
func TestRecoverPendingOperations_DisableApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	_, err := env.deps.Lifecycle.Bootstrap(ctx, "writer", core.NewOperationID(baselineClock()))
	require.NoError(t, err)
	_, err = env.deps.Lifecycle.Transition(ctx, "writer", core.AgentDisabled, 1,
		core.AgentActive, core.NewOperationID(baselineClock()))
	require.NoError(t, err)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionDisable,
		TargetKind:  targetKindAgent,
		TargetID:    "writer",
		DetailJSON: mustDetailJSON(t, operationDetail{
			ExpectedState:      expectedState(core.AgentActive),
			ExpectedGeneration: expectedGeneration(2),
		}),
	})
	_, err = env.deps.Lifecycle.Transition(ctx, "writer", core.AgentActive, 2, core.AgentDisabled, operationID)
	require.NoError(t, err)

	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, core.VerificationApplied, op.Verification)
	require.False(t, op.RecoveredAt.IsZero())
}

// TestRecoverPendingOperations_EnableNotApplied：状态文件仍停在期望状态与代数、operation_id
// 不是本次操作 → not_applied / failed；**不重做**（不得把 enable 再执行一遍）。
func TestRecoverPendingOperations_EnableNotApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	bootstrapOperation := core.NewOperationID(baselineClock())
	_, err := env.deps.Lifecycle.Bootstrap(ctx, "writer", bootstrapOperation)
	require.NoError(t, err)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionEnable,
		TargetKind:  targetKindAgent,
		TargetID:    "writer",
		DetailJSON: mustDetailJSON(t, operationDetail{
			ExpectedState:      expectedState(core.AgentDisabled),
			ExpectedGeneration: expectedGeneration(1),
		}),
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)

	record, exists, err := env.deps.Lifecycle.Read(ctx, "writer")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, record.State, "恢复不得重做变更")
	require.Equal(t, int64(1), record.Generation)
	require.Equal(t, bootstrapOperation, record.OperationID)
}

// TestRecoverPendingOperations_LifecycleIndeterminate：状态被另一次操作推进过 →
// 三态都对不上 → indeterminate / unknown。
func TestRecoverPendingOperations_LifecycleIndeterminate(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	_, err := env.deps.Lifecycle.Bootstrap(ctx, "writer", core.NewOperationID(baselineClock()))
	require.NoError(t, err)
	_, err = env.deps.Lifecycle.Transition(ctx, "writer", core.AgentDisabled, 1,
		core.AgentActive, core.NewOperationID(baselineClock()))
	require.NoError(t, err)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionEnable,
		TargetKind:  targetKindAgent,
		TargetID:    "writer",
		DetailJSON: mustDetailJSON(t, operationDetail{
			ExpectedState:      expectedState(core.AgentDisabled),
			ExpectedGeneration: expectedGeneration(1),
		}),
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)
}

// TestRecoverPendingOperations_BaselineKeptUnknownWithoutRedo：`system.baseline_v2` 在恢复面
// 无法查证 → unknown，且**一项资源也不补**（基线只由启动路径按幂等重跑）。
func TestRecoverPendingOperations_BaselineKeptUnknownWithoutRedo(t *testing.T) {
	env := newRecoveryEnv(t)
	writeWorkspaceFile(t, env.root, "skills/weekly/SKILL.md",
		"---\nname: weekly\ndescription: 周报汇总\n---\n\n正文\n")

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID:  operationID,
		Action:       actionBaseline,
		TargetKind:   targetKindSystem,
		TargetID:     targetKindSystem,
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       baselineSource,
	})
	require.NoError(t, recoverPendingOperations(context.Background(), env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)
	require.Equal(t, 1, countRows(t, openRead(t, filepath.Join(env.root, databaseFileName)),
		`SELECT COUNT(*) FROM management_operations`), "只回填，不追加操作行")
	versions, err := env.deps.Resources.Versions(context.Background(), core.ResourceSkill, "weekly")
	require.NoError(t, err)
	require.Empty(t, versions, "恢复不得替基线建版本")
	require.Contains(t, env.logs.String(), "不重做")
}

// TestRecoverPendingOperations_TerminalRowsUntouched：第二次启动恢复看不到 pending → 一行不写；
// 已终态行的 status / verification / recovered_at 原样保留。
func TestRecoverPendingOperations_TerminalRowsUntouched(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	candidate := []byte("---\nname: writer\n---\n\n新正文\n")
	writeWorkspaceFile(t, env.root, "agents/writer/AGENT.md", string(candidate))
	recoveredID := contentUpdateOperation(t, env,
		operationDetail{CandidateSHA256: sha256Hex(t, candidate)}, "")

	terminalID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: terminalID,
		Action:      actionDisable,
		TargetKind:  targetKindAgent,
		TargetID:    "writer",
	})
	require.NoError(t, env.store.FinishOperation(ctx, terminalID, core.OperationSuccess,
		baselineResultOK, "", nil, baselineClock()))

	require.NoError(t, recoverPendingOperations(ctx, env.deps))
	first := getOperation(t, env.store, recoveredID)
	terminal := getOperation(t, env.store, terminalID)

	require.NoError(t, recoverPendingOperations(ctx, env.deps))
	second := getOperation(t, env.store, recoveredID)
	require.Equal(t, first, second, "已终态行不得被第二次恢复改写")
	require.Equal(t, terminal, getOperation(t, env.store, terminalID))
	require.Equal(t, core.OperationSuccess, second.Status)
	require.Contains(t, env.logs.String(), "没有待查证的操作")
}
