//go:build integration

package app

// 本文件是准入编排面的验收面（颗粒度文档 §2.3.2 / §2.3.3 / §3.1.5、T011）：
//
//	A1 `TestUS21_Propose_FreezeAndHash`——合法候选逐字冻结、行与操作终态一致、回读复算哈希
//	A2 `TestUS21_Propose_ActiveCandidateExists`——重复活跃候选 409 且零写入
//	A3 `TestUS21_Propose_BaselineTriState`——基线 absent / present / deleted 三态
//	A4 `TestUS21_Evaluate_PersistedWithTerminal`——评估行、候选状态与操作终态同事务落库
//	A5 `TestUS21_Evaluate_DBFailureKeepsState`——评估落库失败：无评估行、候选状态不变、操作 failed
//	A6 `TestUS21_Evaluate_TamperedDiskRejected`——冻结目录被改写后复算哈希不符即拒绝
//	A7 `TestUS21_Propose_DBFailureRecordsUnknownKeepsOrphan`——磁盘成功库失败记 unknown，孤儿不删
//	A7b `TestUS21_Propose_CommitUnknown_KeepsSnapshot`——提交结果未知时保留冻结目录并记 unknown
//	A7c `TestUS21_Propose_FreezeUncertainKeepsDir`——目录已就位但写入报错：保留目录并记 unknown
//	A8 `TestUS21_Admission_UntrustedActorRejectedBeforeState`——不可信身份在状态读取前拒绝
//	A9 `TestUS21_Get_DetailIncludesFilesAndDiff`——只读详情 / 列表与可选文件、diff
//
// 判据只看真实磁盘与真实数据库行：候选目录是真实冻结实现写的，候选 / 评估行与
// `management_operations` 都来自真实仓储；只有失败注入点替换成返回固定错误的钩子。
// Every judgement here reads real files and real database rows; only the failure
// injection hooks are replaced, and they return a fixed error without touching storage.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
	"github.com/ymedword/basalt/internal/workspace"
)

// us21AdmissionSourceJSON 是一份通过来源规则的最小声明：任务不适用、直接确认、
// 授权与脱敏已声明、无保留期。夹具固定这一份字节，便于逐字比较来源哈希。
// us21AdmissionSourceJSON is the minimal declaration that passes every source rule.
const us21AdmissionSourceJSON = `{"kind":"manual","ref":"周会记录 2026-11-01",` +
	`"task_outcome":"not_applicable","confirmation":"direct","authorization":"declared",` +
	`"redaction":"declared","applicable":true,"evidence":["会议纪要"],` +
	`"counterexamples":[],"limitations":[]}`

// us21CandidateDirRel 是候选目录相对工作区根的固定布局（`admissions/candidates/<id>`）；
// `candidates` 段在 workspace 内部未导出，用例按布局字面量核对磁盘。
// us21CandidateDirRel mirrors the frozen on-disk layout for assertions.
func us21CandidateDirRel(candidateID string) string {
	return filepath.Join(workspace.AdmissionsDirName, "candidates", candidateID)
}

// us21AdminFixture 是 T011 的真实夹具：临时工作区 + 隔离 PG schema + 真实仓储，
// LockResource 取自与手工技能写同一个 ResourceAdmin 实例（§3.1.5 的装配纪律）。
// us21AdminFixture wires a real workspace, an isolated PG schema and the real stores;
// LockResource comes from the same ResourceAdmin instance used by manual skill writes.
type us21AdminFixture struct {
	t         *testing.T
	root      string
	db        *postgres.DB
	resources *postgres.ResourceStore
	store     *postgres.AdmissionStore
	ops       *postgres.ManagementStore
	gate      *core.ResourceGate
	admin     *AdmissionAdmin
	// deps 保留装配输入：审计预写失败注入只换 Operations 一个字段重建编排器，
	// 其余依赖（真实仓储 / 锁 / 时钟）与生产装配逐字一致。
	// deps keeps the wiring so a test can rebuild the orchestrator with one field swapped.
	deps AdmissionAdminDeps
	// resourceAdmin 与 admin 共用同一 gate 与锁表：撤回用例经它做手工发布 / 更新 /
	// 删除，制造"被超越"与"已删除"两种分类。
	resourceAdmin *ResourceAdmin
	now           time.Time
	actor         core.Actor
	source        []byte
	referrers     []string
	logs          *bytes.Buffer
}

// newUS21AdminFixture 建夹具；固定时钟与固定主体让两次运行的读数逐字可比。
// newUS21AdminFixture builds the fixture with a fixed clock and identity.
func newUS21AdminFixture(t *testing.T) *us21AdminFixture {
	t.Helper()
	f := &us21AdminFixture{
		t:      t,
		root:   t.TempDir(),
		now:    admissionEvalNow,
		source: []byte(us21AdmissionSourceJSON),
		actor: core.Actor{
			Subject: "cli:alice",
			Method:  core.AuthMethodCLI,
			Source:  core.AuthMethodCLI,
		},
	}
	db := pgtest.NewTestDB(t)
	f.db = db
	f.resources = postgres.NewResourceStore(db)
	f.store = postgres.NewAdmissionStore(db)
	f.ops = postgres.NewManagementStore(db)
	gate := &core.ResourceGate{}
	f.gate = gate
	logger, logs := newCapturedLogger()
	f.logs = logs
	resourceAdmin := NewResourceAdmin(ResourceAdminDeps{
		Root:       f.root,
		Store:      f.resources,
		Operations: f.ops,
		Gate:       gate,
		Logger:     logger,
		Now:        func() time.Time { return f.now },
	})
	f.resourceAdmin = resourceAdmin
	f.deps = AdmissionAdminDeps{
		Root:         f.root,
		Store:        f.store,
		Resources:    f.resources,
		Operations:   f.ops,
		Gate:         gate,
		LockResource: resourceAdmin.LockResource,
		References:   func(string) ([]string, error) { return f.referrers, nil },
		Logger:       logger,
		Now:          func() time.Time { return f.now },
	}
	f.admin = NewAdmissionAdmin(f.deps)
	// 冻结目录是只读的（目录 0o555 / 文件 0o444），Windows 的 RemoveAll 会因只读失败；
	// 清理前恢复可写，避免 t.TempDir 的清收把用例判红。
	// The frozen tree is read-only, which breaks t.TempDir cleanup on Windows; restore
	// write permission first.
	t.Cleanup(func() { makeUS21TreeWritable(t, f.root) })
	return f
}

// makeUS21TreeWritable 把树恢复成普通可写权限（目录 0o755 / 文件 0o644）。
// makeUS21TreeWritable restores ordinary write permission over the tree.
func makeUS21TreeWritable(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(p, 0o755)
		}
		return os.Chmod(p, 0o644)
	})
}

// proposeRequest 造一次最小合法提议（根 SKILL.md + 固定来源声明）。
// proposeRequest builds a minimal valid proposal.
func (f *us21AdminFixture) proposeRequest(skillName string) core.AdmissionProposeRequest {
	return core.AdmissionProposeRequest{
		SkillName:  skillName,
		Files:      admissionEvalFiles(skillName),
		SourceJSON: f.source,
	}
}

// beginOperation 预写一行 pending 管理操作，供 PublishWithVersion / MarkDeleted 等
// 既有原语在事务内回填终态；返回操作标识。
// beginOperation pre-writes a pending row for the existing primitives to finish.
func (f *us21AdminFixture) beginOperation(t *testing.T, action, targetKind, targetID string) string {
	t.Helper()
	operationID := core.NewOperationID(f.now)
	require.NoError(t, f.ops.BeginOperation(t.Context(), core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   targetKind,
		TargetID:     targetID,
		ActorSubject: f.actor.Subject,
		AuthMethod:   f.actor.Method,
		Source:       "cli",
		RequestHash:  core.AdmissionRequestHash(action, targetID),
		DetailJSON:   []byte("{}"),
		CreatedAt:    f.now,
	}))
	return operationID
}

// candidateDirs 列出 `admissions/candidates` 下的条目名（升序），用于"零写入"断言。
// candidateDirs lists the entries under admissions/candidates for zero-write checks.
func (f *us21AdminFixture) candidateDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, workspace.AdmissionsDirName, "candidates"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// operations 按过滤条件读管理操作行（固定窗口足够本文件用例）。
// operations lists management operation rows under the filter.
func (f *us21AdminFixture) operations(t *testing.T, filter core.OperationFilter) []core.OperationRecord {
	t.Helper()
	rows, err := f.ops.ListOperations(t.Context(), filter)
	require.NoError(t, err)
	return rows
}

// TestUS21_Propose_FreezeAndHash：合法候选冻结后，磁盘回读、候选行与操作终态三处
// 一致；基线在从未发布时是 absent（U9 的 absent 态）。
// TestUS21_Propose_FreezeAndHash pins the frozen bytes, the row and the terminal.
func TestUS21_Propose_FreezeAndHash(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)

	candidate, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.NoError(t, err)
	require.Equal(t, core.AdmissionProposed, candidate.State)
	require.Equal(t, int64(1), candidate.Revision)
	require.Equal(t, "cli:alice", candidate.ProposerSubject)
	require.Equal(t, core.AuthMethodCLI, candidate.ProposerAuthMethod)
	require.Equal(t, core.AuthMethodCLI, candidate.ProposerSource)
	require.Equal(t, core.AdmissionSHA256Hex(f.source), candidate.SourceHash)
	require.NotEmpty(t, candidate.OperationID)
	require.Equal(t, admissionEvalNow, candidate.CreatedAt)

	// 磁盘回读重跑 ValidateSkillDraft：内容哈希与来源哈希都必须复算一致。
	stored, files, sourceJSON, err := workspace.ReadAdmissionCandidate(f.root, candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, candidate.CandidateHash, stored.CandidateHash)
	require.Equal(t, candidate.ManifestJSON, stored.ManifestJSON)
	require.Equal(t, f.source, sourceJSON)
	require.Len(t, files, 1)
	require.Equal(t, "SKILL.md", files[0].Path)
	require.True(t, workspace.AdmissionCandidateDirExists(f.root, candidate.CandidateID))

	// 数据库行与返回快照一致；提议时的风险档是 unknown。
	row, err := f.store.GetCandidate(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, candidate.CandidateHash, row.CandidateHash)
	require.Equal(t, candidate.SourceHash, row.SourceHash)
	require.Equal(t, candidate.BaselineJSON, row.BaselineJSON)
	require.Equal(t, candidate.SourceJSON, row.SourceJSON)
	require.Equal(t, core.AdmissionRiskUnknown, row.RiskTier)

	state, referrers, ok := admissionBaselineState(candidate.BaselineJSON)
	require.True(t, ok, "基线必须是可解析的规范 JSON")
	require.Equal(t, core.AdmissionSkillAbsent, state)
	require.Empty(t, referrers)

	// 审计：先写 pending、成功后在**同一事务**里回填 success。
	operation, err := f.ops.GetOperation(t.Context(), candidate.OperationID)
	require.NoError(t, err)
	require.Equal(t, core.OperationSuccess, operation.Status)
	require.Equal(t, core.ActionSkillAdmissionPropose, operation.Action)
	require.Equal(t, core.TargetKindSkillAdmission, operation.TargetKind)
	require.Equal(t, candidate.CandidateID, operation.TargetID)
	require.Equal(t, "cli:alice", operation.ActorSubject)
}

// TestUS21_Propose_ActiveCandidateExists：同名活跃候选第二次提议必须 409、零写入；
// 失败的审计行仍保留，但其 target 指向的候选目录不得存在。
// TestUS21_Propose_ActiveCandidateExists proves the duplicate write is rejected with
// zero writes while the failed audit row survives.
func TestUS21_Propose_ActiveCandidateExists(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)

	first, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.NoError(t, err)

	_, err = f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.ErrorIs(t, err, core.ErrAdmissionActiveCandidateExists)

	rows, err := f.store.ListCandidates(t.Context(), core.AdmissionFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, first.CandidateID, rows[0].CandidateID)
	require.Equal(t, []string{first.CandidateID}, f.candidateDirs(t),
		"重复活跃候选必须零写入：补偿删除后只剩第一次的候选目录")

	operations := f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionPropose})
	require.Len(t, operations, 2, "两次提议各留一行审计")
	var failed []core.OperationRecord
	for _, operation := range operations {
		if operation.Status == core.OperationFailed {
			failed = append(failed, operation)
		}
	}
	require.Len(t, failed, 1)
	require.Equal(t, core.TargetKindSkillAdmission, failed[0].TargetKind)
	require.NotEqual(t, first.CandidateID, failed[0].TargetID)
	require.False(t, workspace.AdmissionCandidateDirExists(f.root, failed[0].TargetID))
}

// TestUS21_Propose_BaselineTriState：基线按指针行区分 absent / present / deleted，
// present 携带版本号与内容哈希，引用名单进入基线并参与 CanonicalJSON。
// TestUS21_Propose_BaselineTriState pins the three baseline states against a real store.
func TestUS21_Propose_BaselineTriState(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	f.referrers = []string{"agent-b", "agent-a"}

	// absent：从未发布（引用名单仍记入基线）。
	absent, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("skill-absent"))
	require.NoError(t, err)
	state, referrers, ok := admissionBaselineState(absent.BaselineJSON)
	require.True(t, ok)
	require.Equal(t, core.AdmissionSkillAbsent, state)
	require.Equal(t, []string{"agent-a", "agent-b"}, referrers)

	// present：发布 v1 后提议，基线绑定 UID / 版本 / 内容哈希。
	const presentSkill = "skill-present"
	uid := core.ResourceUID("rs_00000000000000000000000021")
	operationID := f.beginOperation(t, core.ActionSkillCreate, core.TargetKindSkill, presentSkill)
	version, err := f.resources.PublishWithVersion(t.Context(), core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceID:   presentSkill,
		ResourceUID:  uid,
		ContentHash:  core.AdmissionSHA256Hex([]byte("present-v1")),
		ManifestJSON: `{"files":[]}`,
		StoragePath:  "skills/" + presentSkill + "/1",
		OperationID:  operationID,
	}, operationID, f.now)
	require.NoError(t, err)

	present, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest(presentSkill))
	require.NoError(t, err)
	var presentWire struct {
		SkillState         string   `json:"skill_state"`
		ResourceUID        string   `json:"resource_uid"`
		PublishedVersionID int64    `json:"published_version_id"`
		VersionNo          int      `json:"version_no"`
		ContentHash        string   `json:"content_hash"`
		Referrers          []string `json:"referrers"`
	}
	require.NoError(t, json.Unmarshal([]byte(present.BaselineJSON), &presentWire))
	require.Equal(t, core.AdmissionSkillPresent, presentWire.SkillState)
	require.Equal(t, string(uid), presentWire.ResourceUID)
	require.Equal(t, version.ID, presentWire.PublishedVersionID)
	require.Equal(t, version.VersionNo, presentWire.VersionNo)
	require.Equal(t, version.ContentHash, presentWire.ContentHash)
	require.Equal(t, []string{"agent-a", "agent-b"}, presentWire.Referrers)

	// deleted：墓碑仍在 → deleted，而不是 absent。用独立技能：一个技能同时只能有一个
	// 活跃候选（与 present 用例的候选互斥），墓碑形态并不依赖 present 用例那条指针。
	const deletedSkill = "skill-deleted"
	deletedUID := core.ResourceUID("rs_00000000000000000000000022")
	publishOp := f.beginOperation(t, core.ActionSkillCreate, core.TargetKindSkill, deletedSkill)
	_, err = f.resources.PublishWithVersion(t.Context(), core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceID:   deletedSkill,
		ResourceUID:  deletedUID,
		ContentHash:  core.AdmissionSHA256Hex([]byte("deleted-v1")),
		ManifestJSON: `{"files":[]}`,
		StoragePath:  "skills/" + deletedSkill + "/1",
		OperationID:  publishOp,
	}, publishOp, f.now)
	require.NoError(t, err)
	deleteOp := f.beginOperation(t, core.ActionSkillDelete, core.TargetKindSkill, deletedSkill)
	require.NoError(t, f.resources.MarkDeleted(t.Context(), core.ResourceSkill, deletedSkill, deleteOp, f.now))
	deleted, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest(deletedSkill))
	require.NoError(t, err)
	state, _, ok = admissionBaselineState(deleted.BaselineJSON)
	require.True(t, ok)
	require.Equal(t, core.AdmissionSkillDeleted, state)
}

// TestUS21_Evaluate_PersistedWithTerminal：评估行、候选状态（evaluated / 修订号 +1）
// 与操作终态一次事务写入，读回的评估标识与哈希逐字一致。
// TestUS21_Evaluate_PersistedWithTerminal pins the atomic evaluation write.
func TestUS21_Evaluate_PersistedWithTerminal(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)

	candidate, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.NoError(t, err)
	policy := admissionEvalPolicy()
	evaluation, err := f.admin.Evaluate(t.Context(), f.actor, candidate.CandidateID, policy)
	require.NoError(t, err)

	require.Equal(t, candidate.CandidateID, evaluation.CandidateID)
	require.Equal(t, candidate.CandidateHash, evaluation.CandidateHash)
	require.Equal(t, int64(1), evaluation.CandidateRevision)
	require.Equal(t, policy.Hash, evaluation.PolicyHash)
	require.Equal(t, AdmissionGraderVersion, evaluation.GraderVersion)
	require.Equal(t, core.AdmissionAggregateIncomplete, evaluation.Aggregate,
		"effect / cost 不可评估，聚合只能是 incomplete")
	require.Equal(t, core.AdmissionRiskNormal, evaluation.RiskTier)
	require.NotEmpty(t, evaluation.EvaluationHash)
	require.NotEmpty(t, evaluation.OperationID)
	require.True(t, evaluation.EvaluatedAt.Equal(admissionEvalNow))

	storedEvaluation, err := f.store.LatestEvaluation(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, evaluation.EvaluationID, storedEvaluation.EvaluationID)
	require.Equal(t, evaluation.EvaluationHash, storedEvaluation.EvaluationHash)
	require.Equal(t, evaluation.Aggregate, storedEvaluation.Aggregate)

	row, err := f.store.GetCandidate(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionEvaluated, row.State)
	require.Equal(t, int64(2), row.Revision)
	require.Equal(t, core.AdmissionRiskNormal, row.RiskTier)

	operation, err := f.ops.GetOperation(t.Context(), evaluation.OperationID)
	require.NoError(t, err)
	require.Equal(t, core.OperationSuccess, operation.Status)
	require.Equal(t, core.ActionSkillAdmissionEvaluate, operation.Action)
	require.Equal(t, candidate.CandidateID, operation.TargetID)
}

// TestUS21_Evaluate_DBFailureKeepsState：评估落库失败时没有评估行、候选保持
// proposed / 修订号不变，操作回填 failed——三者同事务的证据。
// TestUS21_Evaluate_DBFailureKeepsState proves the failed write leaves no partial state.
func TestUS21_Evaluate_DBFailureKeepsState(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)

	candidate, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.NoError(t, err)
	injected := errors.New("注入：评估行写入失败")
	f.admin.insertEvaluation = func(context.Context, core.AdmissionEvaluation, int64, time.Time) error {
		return injected
	}

	_, err = f.admin.Evaluate(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, injected)

	_, err = f.store.LatestEvaluation(t.Context(), candidate.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound)
	row, err := f.store.GetCandidate(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionProposed, row.State)
	require.Equal(t, int64(1), row.Revision)
	require.Equal(t, core.AdmissionRiskUnknown, row.RiskTier)

	operations := f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionEvaluate})
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationFailed, operations[0].Status)
	require.Contains(t, operations[0].ErrorMessage, injected.Error())
}

// TestUS21_Evaluate_TamperedDiskRejected：候选冻结后磁盘被改写，评估从磁盘重读并
// 复算哈希，发现与候选行不一致即拒绝，且不留下评估行。
// TestUS21_Evaluate_TamperedDiskRejected proves evaluate re-reads and re-hashes the disk.
func TestUS21_Evaluate_TamperedDiskRejected(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)

	candidate, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.NoError(t, err)

	// 恢复写位后改写候选正文：内容变了但候选行与清单仍指向旧哈希。
	dir := filepath.Join(f.root, us21CandidateDirRel(candidate.CandidateID))
	makeUS21TreeWritable(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "files", "SKILL.md"),
		[]byte(admissionEvalSkillMD("weekly-report", "被改写的描述")), 0o644))

	_, err = f.admin.Evaluate(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionCandidateMismatch)

	_, err = f.store.LatestEvaluation(t.Context(), candidate.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound)
	row, err := f.store.GetCandidate(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionProposed, row.State)
	require.Equal(t, int64(1), row.Revision)
	// 篡改在审计预写**之前**就被识别（复算哈希属于读取校验，不是一次变更尝试），
	// 因此这里没有评估操作行——与资源写面的 412 不产生行同一口径。
	require.Empty(t, f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionEvaluate}))
}

// TestUS21_Propose_DBFailureRecordsUnknownKeepsOrphan：磁盘已冻结、数据库未确认且
// 补偿删除也失败（原路径被换成普通文件）时，操作记 unknown，孤儿目录与占位文件
// 都原样保留，供人工检查——失败路径不删证据。
// TestUS21_Propose_DBFailureRecordsUnknownKeepsOrphan proves the orphan is preserved.
func TestUS21_Propose_DBFailureRecordsUnknownKeepsOrphan(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)

	injected := errors.New("注入：候选行写入失败")
	var candidateID string
	f.admin.createCandidate = func(_ context.Context, c core.AdmissionCandidate, _ string, _ time.Time) error {
		candidateID = c.CandidateID
		dir := filepath.Join(f.root, us21CandidateDirRel(c.CandidateID))
		// 把刚落盘的候选目录改名，再在原路径放一个普通文件：补偿删除会因
		// "不是普通目录"拒绝，从而走到 finishUnknown。
		require.NoError(t, os.Rename(dir, dir+".orphan"))
		require.NoError(t, os.WriteFile(dir, []byte("orphan"), 0o600))
		return injected
	}

	_, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.ErrorIs(t, err, injected)
	require.NotEmpty(t, candidateID)

	directory := filepath.Join(f.root, us21CandidateDirRel(candidateID))
	orphanInfo, statErr := os.Stat(directory + ".orphan")
	require.NoError(t, statErr)
	require.True(t, orphanInfo.IsDir(), "改名后的孤儿目录必须保留")
	placeholderInfo, statErr := os.Stat(directory)
	require.NoError(t, statErr)
	require.True(t, placeholderInfo.Mode().IsRegular(), "原路径占位文件不得被补偿删除清掉")

	rows, err := f.store.ListCandidates(t.Context(), core.AdmissionFilter{})
	require.NoError(t, err)
	require.Empty(t, rows, "数据库未确认的候选不得留下行")

	operations := f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionPropose})
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationUnknown, operations[0].Status)
	require.Equal(t, candidateID, operations[0].TargetID)
	require.Contains(t, operations[0].ErrorMessage, injected.Error())
}

// TestUS21_Propose_CommitUnknown_KeepsSnapshot 是 review ② 发现 1 在提议面的回归：
// 仓储提交返回"结果未知"只证明事务**可能**已生效——候选行可能已经落库，冻结目录
// 是唯一证据，补偿不得删除；操作记 unknown 交恢复查证。
// 判别力：删掉 Propose 的 ErrAdmissionCommitUnknown 分支（回到"任何失败都删目录"），
// 操作变 failed、冻结目录被删除，两条断言变红。
func TestUS21_Propose_CommitUnknown_KeepsSnapshot(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	f.admin.createCandidate = func(context.Context, core.AdmissionCandidate, string, time.Time) error {
		return fmt.Errorf("写入准入候选 x: 提交结果未知: %w: %v",
			core.ErrAdmissionCommitUnknown, errors.New("connection reset"))
	}

	_, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.ErrorIs(t, err, core.ErrAdmissionCommitUnknown)
	require.ErrorContains(t, err, "提交结果未知")

	operations := f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionPropose})
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationUnknown, operations[0].Status)
	require.Len(t, f.candidateDirs(t), 1, "提交结果未知时不得删除冻结候选目录")
}

// TestUS21_Propose_FreezeUncertainKeepsDir 是 review ② F-02 的回归：冻结目录已经
// 就位、写入却报错（置只读或关根目录失败）时，"行不存在但目录存在"是恢复表的
// 第三态——操作记 unknown、目录保留，不按确定失败删证据。
// 判别力：删掉 Propose 的 AdmissionCandidateDirExists 分支（退回无条件 finishFailed），
// 状态断言由 unknown 变 failed 转红。
func TestUS21_Propose_FreezeUncertainKeepsDir(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)
	f.admin.writeCandidate = func(root string, c core.AdmissionCandidate,
		files []core.AuthorFile, sourceJSON []byte,
	) error {
		// 真实冻结先落盘，再报"目录就位后失败"——模拟置只读 / 关根目录的错误分支。
		if err := workspace.WriteAdmissionCandidate(root, c, files, sourceJSON); err != nil {
			return err
		}
		return errors.New("注入：冻结目录就位后置只读失败")
	}

	_, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.Error(t, err)

	operations := f.operations(t, core.OperationFilter{Action: core.ActionSkillAdmissionPropose})
	require.Len(t, operations, 1)
	require.Equal(t, core.OperationUnknown, operations[0].Status, "目录已就位必须记 unknown")
	require.Len(t, f.candidateDirs(t), 1, "不得删除已就位的冻结目录")
	rows, listErr := f.store.ListCandidates(t.Context(), core.AdmissionFilter{})
	require.NoError(t, listErr)
	require.Empty(t, rows, "数据库里不得留下候选行")
}

// TestUS21_Admission_UntrustedActorRejectedBeforeState：不可信身份在读取任何状态
// 之前就被拒绝——不存在的候选也报身份错误而不是 not-found，且零写入。
// TestUS21_Admission_UntrustedActorRejectedBeforeState proves identity precedes state.
func TestUS21_Admission_UntrustedActorRejectedBeforeState(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)

	for _, actor := range []core.Actor{
		{Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "cli:alice", Method: core.AuthMethodLocalTrust, Source: core.AuthMethodCLI},
		{Subject: "cli:alice", Method: core.AuthMethodCLI, Source: "http"},
	} {
		_, err := f.admin.Propose(t.Context(), actor, f.proposeRequest("weekly-report"))
		require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor, "身份 %+v 必须被拒绝", actor)
		require.Empty(t, f.candidateDirs(t), "身份拒绝必须零写入")
	}

	nonexistent, err := core.NewAdmissionCandidateID()
	require.NoError(t, err)
	_, err = f.admin.Evaluate(t.Context(), core.Actor{
		Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI,
	}, nonexistent, admissionEvalPolicy())
	require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor)
	require.NotErrorIs(t, err, core.ErrAdmissionNotFound, "身份校验必须早于状态读取")

	require.Empty(t, f.operations(t, core.OperationFilter{}))
}

// TestUS21_Get_DetailIncludesFilesAndDiff：详情聚合候选 / 评估 / 来源，`IncludeFiles`
// 与 `IncludeDiff` 从磁盘回读；列表按存储态过滤。
// TestUS21_Get_DetailIncludesFilesAndDiff covers the read-only detail projection.
func TestUS21_Get_DetailIncludesFilesAndDiff(t *testing.T) {
	t.Parallel()
	f := newUS21AdminFixture(t)

	candidate, err := f.admin.Propose(t.Context(), f.actor, f.proposeRequest("weekly-report"))
	require.NoError(t, err)

	detail, err := f.admin.Get(t.Context(), candidate.CandidateID, core.AdmissionShowOptions{
		IncludeFiles: true,
		IncludeDiff:  true,
	})
	require.NoError(t, err)
	require.Nil(t, detail.Evaluation, "尚未评估")
	require.Nil(t, detail.Decision)
	require.Nil(t, detail.Publication)
	require.Equal(t, "manual", detail.Source.Kind)
	require.Equal(t, "周会记录 2026-11-01", detail.Source.Ref)
	require.Len(t, detail.Files, 1)
	require.Equal(t, "SKILL.md", detail.Files[0].Path)
	require.Contains(t, detail.Diff, "diff --git a/SKILL.md b/SKILL.md")
	require.Contains(t, detail.Diff, "--- /dev/null", "技能未发布时 diff 呈现为全部新增")
	require.Contains(t, detail.Diff, "+++ b/SKILL.md")

	_, err = f.admin.Evaluate(t.Context(), f.actor, candidate.CandidateID, admissionEvalPolicy())
	require.NoError(t, err)

	detail, err = f.admin.Get(t.Context(), candidate.CandidateID, core.AdmissionShowOptions{})
	require.NoError(t, err)
	require.NotNil(t, detail.Evaluation)
	require.Empty(t, detail.Files)

	listed, err := f.admin.List(t.Context(), core.AdmissionFilter{State: core.AdmissionEvaluated})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, candidate.CandidateID, listed[0].CandidateID)

	missing, err := f.admin.List(t.Context(), core.AdmissionFilter{State: core.AdmissionPublished})
	require.NoError(t, err)
	require.Empty(t, missing)
}
