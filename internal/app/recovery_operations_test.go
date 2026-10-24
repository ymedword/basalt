package app

// 本文件是启动恢复的验收面（颗粒度文档 §3.2.1：internal/app/recovery_operations_test.go，
// 10 例）。口径是约束 38：**只查证、不重做**——content.update 三态、disable / enable 三态、
// 无结论保留 unknown、已终态的行不被覆盖、`recovered_at` / `verification` 回填。
// 恢复在开放服务前单线程执行，全部用例离线。
// This file covers startup recovery: verify only, never replay.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// recoveryEnv 是一套恢复夹具：临时工作区 + 临时库 + 固定时钟 + captured logger；
// US-12 的查证面（定义文件 / 技能库 / 归档与通知仓储 / 凭据仓储 / 库版本 / Agent 写锁）
// 全部落在同一临时工作区与同一库句柄上。
type recoveryEnv struct {
	deps        RecoveryDeps
	store       *storage.ManagementStore
	credentials *storage.CredentialStore
	policy      *storage.NetworkPolicyStore
	root        string
	logs        *bytes.Buffer
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
	credentials := storage.NewCredentialStore(db)
	policy := storage.NewNetworkPolicyStore(db)
	logger, logs := newCapturedLogger()
	return &recoveryEnv{
		root:        root,
		store:       store,
		credentials: credentials,
		policy:      policy,
		logs:        logs,
		deps: RecoveryDeps{
			Operations: store,
			Resources:  storage.NewResourceStore(db),
			Lifecycle:  workspace.NewLifecycleStore(root, baselineClock),
			Editor:     workspace.NewAgentEditor(workspace.AgentEditorDeps{Root: root}),
			Logger:     logger,
			Now:        baselineClock,

			Root:        root,
			ConfigPath:  filepath.Join(root, configFileName),
			Skills:      workspace.NewSkillLibrary(root),
			Gate:        core.NewAgentGate(),
			Archives:    storage.NewAgentAdminStore(db),
			Notify:      storage.NewNotifyAdminStore(db),
			Credentials: credentials,
			Schema:      db,
			// 网络域名白名单策略行：恢复按修订号 + 候选域名哈希判三态（R8.2.4）。
			NetworkPolicy: policy,
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
func mustDetailJSON(t *testing.T, detail any) []byte {
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

// --- US-12 动作组（T127）：凭据写入 / 撤销、范围转换、v3 迁移的四证查证 ---

// seedOperation 预写并收口一条管理操作行：版本行 / 凭据行 / 索引行的 `operation_id`
// 都外键引用它，seed 出来的提交事实必须落在一条真实操作上。
// seedOperation registers a finished operation row for the seeded commit facts.
func seedOperation(t *testing.T, env *recoveryEnv, action, targetKind, targetID string) string {
	t.Helper()
	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      action,
		TargetKind:  targetKind,
		TargetID:    targetID,
	})
	require.NoError(t, env.store.FinishOperation(context.Background(), operationID,
		core.OperationSuccess, baselineResultOK, "", nil, baselineClock()))
	return operationID
}

// seedModelConnection 发布一个模型连接并返回其发布指针：凭据与范围动作按
// `resource_uid` 寻址，指针行的 `revision` / `operation_id` 就是 DB 提交事实。
// seedModelConnection publishes one model connection and returns its pointer.
func seedModelConnection(t *testing.T, env *recoveryEnv, name string) core.ResourcePointer {
	t.Helper()
	ctx := context.Background()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	seedID := seedOperation(t, env, "model_connection.publish",
		string(core.ResourceModelConnection), name)
	version, _, err := env.deps.Resources.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    uid,
		ResourceID:     name,
		ContentHash:    "sha256:" + strings.Repeat("a", 64),
		DefinitionYAML: "name: " + name + "\nbase_url: https://api.example.com\n",
		OperationID:    seedID,
	})
	require.NoError(t, err)
	require.NoError(t, env.deps.Resources.Publish(ctx,
		core.ResourceModelConnection, name, version.ID, true, seedID, baselineClock()))
	ptr, err := env.deps.Resources.PointerByUID(ctx, core.ResourceModelConnection, uid)
	require.NoError(t, err)
	return ptr
}

// seedIndex 用一条**真实**操作行把状态文件记录写进 `agent_lifecycle` 镜像：
// 索引的 `operation_id` 外键引用管理操作，测试不得凭空造标识。
// seedIndex mirrors one state-file record into the lifecycle index under a real op.
func seedIndex(t *testing.T, env *recoveryEnv, rec core.LifecycleRecord) {
	t.Helper()
	rec.OperationID = seedOperation(t, env, actionBaselineV3, targetKindSystem, rec.Agent)
	require.NoError(t, env.deps.Resources.PutLifecycleIndex(context.Background(), rec, true, ""))
}

// credentialIDFor 给出资源实例的凭据标识（`cred_` + 同一份 Crockford 载荷）：
// 测试可以**先**写 detail 再写凭据，不必读回仓储。
// credentialIDFor derives the credential id so a test can write the detail first.
func credentialIDFor(uid core.ResourceUID) string {
	return "cred_" + strings.TrimPrefix(string(uid), core.ResourceUIDPrefix)
}

// seedCredential 为一条模型连接写入凭据身份与第 1 个版本，返回 identity / 版本 / 目标绑定哈希。
// seedCredential writes the credential identity and version 1 of one model connection.
func seedCredential(t *testing.T, env *recoveryEnv, name string, uid core.ResourceUID,
	operationID string,
) (core.CredentialRecord, core.CredentialVersionRecord, string) {
	t.Helper()
	credentialID := credentialIDFor(uid)
	targetBinding := "sha256:" + strings.Repeat("b", 64)
	require.NoError(t, env.credentials.Create(context.Background(), core.CredentialRecord{
		ID:          credentialID,
		OwnerKind:   core.BindingModelConnection,
		OwnerUID:    uid,
		OwnerName:   name,
		KeyID:       "key-2026-10",
		OperationID: operationID,
	}, core.CredentialVersionRecord{
		KeyID:         "key-2026-10",
		TargetBinding: targetBinding,
		Last4:         "abcd",
		Nonce:         bytes.Repeat([]byte{0x01}, 12),
		Ciphertext:    []byte("ciphertext"),
		OperationID:   operationID,
	}))
	identity, err := env.credentials.CredentialByOwner(context.Background(),
		core.BindingModelConnection, uid)
	require.NoError(t, err)
	version, err := env.credentials.Version(context.Background(), credentialID, 1)
	require.NoError(t, err)
	return identity, version, targetBinding
}

// credentialDetail 构造凭据动作的 detail 投影（与写路径同一键名）。
func credentialDetail(uid core.ResourceUID, name, credentialID, targetBinding string,
	revision, previousRevision, versionNo, previousVersion int,
) credentialActionDetail {
	return credentialActionDetail{
		Kind:             string(core.BindingModelConnection),
		Name:             name,
		ResourceUID:      string(uid),
		Revision:         revision,
		PreviousRevision: previousRevision,
		CredentialID:     credentialID,
		VersionNo:        versionNo,
		PreviousVersion:  previousVersion,
		TargetBinding:    targetBinding,
	}
}

// TestRecoverPendingOperations_CredentialWriteApplied：身份与版本行都带本操作的
// `operation_id`、版本号与目标绑定哈希与 detail 一致 → applied / success。
func TestRecoverPendingOperations_CredentialWriteApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	ptr := seedModelConnection(t, env, "primary")
	operationID := core.NewOperationID(baselineClock())
	credentialID := credentialIDFor(ptr.ResourceUID)
	targetBinding := "sha256:" + strings.Repeat("b", 64)
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionModelConnectionCredentialWrite,
		TargetKind:  string(core.ResourceModelConnection),
		TargetID:    string(ptr.ResourceUID),
		DetailJSON: mustDetailJSON(t, credentialDetail(ptr.ResourceUID, "primary", credentialID,
			targetBinding, ptr.Revision, ptr.Revision, 1, 0)),
	})
	seedCredential(t, env, "primary", ptr.ResourceUID, operationID)
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, core.VerificationApplied, op.Verification)
	require.False(t, op.RecoveredAt.IsZero())
}

// TestRecoverPendingOperations_CredentialWriteNotApplied：没有任何凭据身份，且资源行
// 仍停在写前 revision → 可证明未提交 → not_applied / failed（**不**补写凭据）。
func TestRecoverPendingOperations_CredentialWriteNotApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	ptr := seedModelConnection(t, env, "primary")
	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionModelConnectionCredentialWrite,
		TargetKind:  string(core.ResourceModelConnection),
		TargetID:    string(ptr.ResourceUID),
		DetailJSON: mustDetailJSON(t, credentialDetail(ptr.ResourceUID, "primary",
			credentialIDFor(ptr.ResourceUID), "sha256:"+strings.Repeat("b", 64),
			ptr.Revision, ptr.Revision, 1, 0)),
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)

	_, err := env.credentials.CredentialByOwner(ctx, core.BindingModelConnection, ptr.ResourceUID)
	require.ErrorIs(t, err, core.ErrResourceNotFound, "恢复不得重新加密或补写凭据版本")
}

// TestRecoverPendingOperations_CredentialWriteIndeterminate：版本行由**另一次**操作写入 →
// 四证不是全部指向候选终态 → unknown（证据矛盾不得猜成成功或失败）。
func TestRecoverPendingOperations_CredentialWriteIndeterminate(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	ptr := seedModelConnection(t, env, "primary")
	otherOp := seedOperation(t, env, actionModelConnectionCredentialWrite,
		string(core.ResourceModelConnection), string(ptr.ResourceUID))
	seedCredential(t, env, "primary", ptr.ResourceUID, otherOp)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionModelConnectionCredentialWrite,
		TargetKind:  string(core.ResourceModelConnection),
		TargetID:    string(ptr.ResourceUID),
		DetailJSON: mustDetailJSON(t, credentialDetail(ptr.ResourceUID, "primary",
			credentialIDFor(ptr.ResourceUID), "sha256:"+strings.Repeat("b", 64),
			ptr.Revision, ptr.Revision, 1, 0)),
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)
}

// TestRecoverPendingOperations_CredentialRevokeApplied：身份与全部版本已是 `revoked`、
// 密文已清、纪元等于候选值、行上的 `operation_id` 是本次撤销 → applied。
func TestRecoverPendingOperations_CredentialRevokeApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	ptr := seedModelConnection(t, env, "primary")
	writeOp := seedOperation(t, env, actionModelConnectionCredentialWrite,
		string(core.ResourceModelConnection), string(ptr.ResourceUID))
	identity, _, targetBinding := seedCredential(t, env, "primary", ptr.ResourceUID, writeOp)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionModelConnectionCredentialRevoke,
		TargetKind:  string(core.ResourceModelConnection),
		TargetID:    string(ptr.ResourceUID),
		DetailJSON: mustDetailJSON(t, credentialActionDetail{
			Kind:             string(core.BindingModelConnection),
			Name:             "primary",
			ResourceUID:      string(ptr.ResourceUID),
			Revision:         ptr.Revision,
			PreviousRevision: ptr.Revision,
			CredentialID:     identity.ID,
			VersionNo:        identity.CurrentVersion,
			TargetBinding:    targetBinding,
			RevocationEpoch:  identity.RevocationEpoch + 1,
			PreviousEpoch:    identity.RevocationEpoch,
		}),
	})
	require.NoError(t, env.credentials.Revoke(ctx, identity.ID, operationID, baselineClock()))
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, core.VerificationApplied, op.Verification)
}

// TestRecoverPendingOperations_CredentialRevokeNotApplied：身份仍是 active、纪元未变、
// 没有任何版本行由本操作写过 → not_applied（**不得**重放撤销）。
func TestRecoverPendingOperations_CredentialRevokeNotApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	ptr := seedModelConnection(t, env, "primary")
	writeOp := seedOperation(t, env, actionModelConnectionCredentialWrite,
		string(core.ResourceModelConnection), string(ptr.ResourceUID))
	identity, _, targetBinding := seedCredential(t, env, "primary", ptr.ResourceUID, writeOp)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionModelConnectionCredentialRevoke,
		TargetKind:  string(core.ResourceModelConnection),
		TargetID:    string(ptr.ResourceUID),
		DetailJSON: mustDetailJSON(t, credentialActionDetail{
			Kind:             string(core.BindingModelConnection),
			Name:             "primary",
			ResourceUID:      string(ptr.ResourceUID),
			Revision:         ptr.Revision,
			PreviousRevision: ptr.Revision,
			CredentialID:     identity.ID,
			VersionNo:        identity.CurrentVersion,
			TargetBinding:    targetBinding,
			RevocationEpoch:  identity.RevocationEpoch + 1,
			PreviousEpoch:    identity.RevocationEpoch,
		}),
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)

	still, err := env.credentials.Credential(ctx, identity.ID)
	require.NoError(t, err)
	require.Equal(t, core.CredentialIdentityActive, still.State, "恢复不得重放撤销")
}

// TestRecoverPendingOperations_ScopeConversionApplied：范围 / 属主 / revision 与候选一致，
// 且行上的 `operation_id` 是本次动作 → applied。
func TestRecoverPendingOperations_ScopeConversionApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	ptr := seedModelConnection(t, env, "primary")
	owner, err := core.NewAgentUID()
	require.NoError(t, err)
	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionModelConnectionScope,
		TargetKind:  string(core.ResourceModelConnection),
		TargetID:    string(ptr.ResourceUID),
		DetailJSON: mustDetailJSON(t, credentialActionDetail{
			Kind:             string(core.BindingModelConnection),
			Name:             "primary",
			ResourceUID:      string(ptr.ResourceUID),
			Revision:         ptr.Revision + 1,
			PreviousRevision: ptr.Revision,
			Scope:            string(core.ScopeExclusive),
			OwnerAgentUID:    string(owner),
			PreviousScope:    string(core.ScopeShared),
		}),
	})
	_, err = env.deps.Resources.ConvertScope(ctx, storage.ScopeConversion{
		Kind:          core.ResourceModelConnection,
		Name:          "primary",
		ExpectUID:     ptr.ResourceUID,
		Target:        core.ScopeExclusive,
		OwnerAgentUID: owner,
		OperationID:   operationID,
		At:            baselineClock(),
	})
	require.NoError(t, err)
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, core.VerificationApplied, op.Verification)
}

// TestRecoverPendingOperations_ScopeConversionNotApplied：资源仍停在共享范围与写前
// revision、行上的 `operation_id` 不是本次动作 → not_applied / failed。
func TestRecoverPendingOperations_ScopeConversionNotApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	ptr := seedModelConnection(t, env, "primary")
	owner, err := core.NewAgentUID()
	require.NoError(t, err)
	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionModelConnectionScope,
		TargetKind:  string(core.ResourceModelConnection),
		TargetID:    string(ptr.ResourceUID),
		DetailJSON: mustDetailJSON(t, credentialActionDetail{
			Kind:             string(core.BindingModelConnection),
			Name:             "primary",
			ResourceUID:      string(ptr.ResourceUID),
			Revision:         ptr.Revision + 1,
			PreviousRevision: ptr.Revision,
			Scope:            string(core.ScopeExclusive),
			OwnerAgentUID:    string(owner),
			PreviousScope:    string(core.ScopeShared),
		}),
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)

	after, err := env.deps.Resources.PointerByUID(ctx, core.ResourceModelConnection, ptr.ResourceUID)
	require.NoError(t, err)
	require.Equal(t, core.ScopeShared, after.Scope, "恢复不得重做范围转换")
}

// TestRecoverPendingOperations_ScopeConversionIndeterminate：范围被**另一次**动作改过 →
// 候选与旧状态都对不上 → unknown。
func TestRecoverPendingOperations_ScopeConversionIndeterminate(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	ptr := seedModelConnection(t, env, "primary")
	owner, err := core.NewAgentUID()
	require.NoError(t, err)
	otherOp := seedOperation(t, env, actionModelConnectionScope,
		string(core.ResourceModelConnection), string(ptr.ResourceUID))
	_, err = env.deps.Resources.ConvertScope(ctx, storage.ScopeConversion{
		Kind:          core.ResourceModelConnection,
		Name:          "primary",
		ExpectUID:     ptr.ResourceUID,
		Target:        core.ScopeExclusive,
		OwnerAgentUID: owner,
		OperationID:   otherOp,
		At:            baselineClock(),
	})
	require.NoError(t, err)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionModelConnectionScope,
		TargetKind:  string(core.ResourceModelConnection),
		TargetID:    string(ptr.ResourceUID),
		DetailJSON: mustDetailJSON(t, credentialActionDetail{
			Kind:             string(core.BindingModelConnection),
			Name:             "primary",
			ResourceUID:      string(ptr.ResourceUID),
			Revision:         ptr.Revision + 1,
			PreviousRevision: ptr.Revision,
			Scope:            string(core.ScopeExclusive),
			OwnerAgentUID:    string(owner),
			PreviousScope:    string(core.ScopeShared),
		}),
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)
}

// staticSchemaVersion 是库版本读数的测试替身：恢复判据只读一次，不另开连接。
type staticSchemaVersion int

func (v staticSchemaVersion) SchemaVersion(context.Context) (int, error) { return int(v), nil }

// TestRecoverPendingOperations_BaselineV3Applied：库已在目标版本、活动区每个状态
// 文件都是 schema 2 且镜像一致 → applied / success。
func TestRecoverPendingOperations_BaselineV3Applied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	rec, err := env.deps.Lifecycle.Bootstrap(ctx, "writer", core.NewOperationID(baselineClock()))
	require.NoError(t, err)
	seedIndex(t, env, rec)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionBaselineV3,
		TargetKind:  targetKindSystem,
		TargetID:    actionBaselineV3,
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationSuccess, op.Status)
	require.Equal(t, core.VerificationApplied, op.Verification)
}

// TestRecoverPendingOperations_BaselineV3NotApplied：库仍在旧版本且活动区每个状态
// 文件都是 schema 1（没有 agent_uid）→ 可证明未写入 → not_applied。
func TestRecoverPendingOperations_BaselineV3NotApplied(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	seedLegacyStateFile(t, env.root, "writer")
	env.deps.Schema = staticSchemaVersion(storage.SupportedSchemaVersion - 1)

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionBaselineV3,
		TargetKind:  targetKindSystem,
		TargetID:    actionBaselineV3,
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationFailed, op.Status)
	require.Equal(t, core.VerificationNotApplied, op.Verification)
}

// TestRecoverPendingOperations_BaselineV3PartialKeptUnknown：部分升级（一个 Agent 已
// 是 schema 2、另一个仍是 schema 1）→ unknown，且**不给同一 Agent 补发第二个 UID**。
func TestRecoverPendingOperations_BaselineV3PartialKeptUnknown(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	rec, err := env.deps.Lifecycle.Bootstrap(ctx, "writer", core.NewOperationID(baselineClock()))
	require.NoError(t, err)
	seedIndex(t, env, rec)
	seedLegacyStateFile(t, env.root, "reporter")

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionBaselineV3,
		TargetKind:  targetKindSystem,
		TargetID:    actionBaselineV3,
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)

	still, exists, err := env.deps.Lifecycle.Read(ctx, "writer")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, rec.AgentUID, still.AgentUID, "不得给同一 Agent 补发第二个 UID")
}

// TestRecoverPendingOperations_BaselineV3MirrorMismatchIndeterminate：状态文件是 v2 但
// 索引镜像缺失 → 部分完成 → unknown（镜像与文件对不上就是证据不足）。
func TestRecoverPendingOperations_BaselineV3MirrorMismatchIndeterminate(t *testing.T) {
	env := newRecoveryEnv(t)
	ctx := context.Background()

	_, err := env.deps.Lifecycle.Bootstrap(ctx, "writer", core.NewOperationID(baselineClock()))
	require.NoError(t, err)
	seedAgentDir(t, env.root, "writer")

	operationID := core.NewOperationID(baselineClock())
	beginPendingOperation(t, env.store, core.OperationRecord{
		OperationID: operationID,
		Action:      actionBaselineV3,
		TargetKind:  targetKindSystem,
		TargetID:    actionBaselineV3,
	})
	require.NoError(t, recoverPendingOperations(ctx, env.deps))

	op := getOperation(t, env.store, operationID)
	require.Equal(t, core.OperationUnknown, op.Status)
	require.Equal(t, core.VerificationIndeterminate, op.Verification)
}

// seedLegacyStateFile 在临时工作区里落一份 US-11 形态（schema 1、无 agent_uid）的状态
// 文件：字节形态与 internal/workspace 的落盘形态同构（固定字段顺序、紧凑 JSON、
// checksum 覆盖"checksum 为空"时的那份字节）。
// seedLegacyStateFile writes a v1 state file byte-compatible with the workspace encoder.
func seedLegacyStateFile(t *testing.T, root, agent string) {
	t.Helper()
	state := struct {
		SchemaVersion int    `json:"schema_version"`
		Agent         string `json:"agent"`
		State         string `json:"state"`
		Generation    int64  `json:"generation"`
		UpdatedAt     int64  `json:"updated_at"`
		OperationID   string `json:"operation_id"`
		Checksum      string `json:"checksum"`
	}{
		SchemaVersion: 1,
		Agent:         agent,
		State:         string(core.AgentDisabled),
		Generation:    1,
		UpdatedAt:     baselineClock().UnixMilli(),
		OperationID:   "op-1760000000000-legacy",
	}
	covered, err := json.Marshal(state)
	require.NoError(t, err)
	sum := sha256.Sum256(covered)
	state.Checksum = "sha256:" + hex.EncodeToString(sum[:])
	final, err := json.Marshal(state)
	require.NoError(t, err)

	seedAgentDir(t, root, agent)
	writeWorkspaceFile(t, root, filepath.Join("control", "agents", agent+".json"), string(final))
}

// seedAgentDir 建一个活动区 Agent 目录：`AgentNames` 只列 `agents/` 的直接子目录，
// 没有目录的 Agent 不参与基线迁移判据。本夹具另用 os.MkdirAll（workspace 的目录创建
// 不是公开面），与 writeWorkspaceFile 同一临时工作区。
// seedAgentDir creates one active agent directory for the rebuild verdict.
func seedAgentDir(t *testing.T, root, agent string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(root, agentsDirName, agent), 0o750))
}
