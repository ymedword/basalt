//go:build integration

package postgres

// 本文件是准入四表仓储的判卷（颗粒度文档 US-21 §3.1.4 / T006 DoD）：
// 同名活跃候选唯一、候选状态 CAS、`Latest*` 按插入序、发布事务原子性与并发单赢家、
// 撤回三种分类。夹具用隔离 schema（`portTestDB`），每个用例自造管理操作行。
// Verdicts for the admission store: unique active name, candidate CAS, seq-ordered
// latest reads, atomic publish with a single concurrent winner, and the three
// withdrawal classifications, all on an isolated migrated schema.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// admissionAt 是判卷统一的写库时刻（UTC 毫秒，断言可复算）。
var admissionAt = time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

// admissionHash 造一个合法的 64 位小写十六进制哈希；字符区分不同语义字段。
func admissionHash(ch byte) string { return strings.Repeat(string(ch), 64) }

// 判卷用固定标识（前缀 + 26 位 Crockford 载荷；末位区分同用例内的多条记录）。
const (
	admCandidateA = "adm_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	admCandidateB = "adm_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"
	admCandidateC = "adm_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C"
	admEvalA      = "eval_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	admEvalB      = "eval_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"
	admEvalC      = "eval_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C"
	admDecisionA  = "dec_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	admDecisionB  = "dec_01J8Z6YQ4M2S3T7V9W0X1Y2Z3B"
	admDecisionC  = "dec_01J8Z6YQ4M2S3T7V9W0X1Y2Z3C"
	admDecisionD  = "dec_01J8Z6YQ4M2S3T7V9W0X1Y2Z3D"
)

// seedAdmissionOperation 预写一行管理操作（四张表的 operation_id 外键要求它先存在）。
func seedAdmissionOperation(t *testing.T, db *DB, operationID, action string) {
	t.Helper()
	require.NoError(t, NewManagementStore(db).BeginOperation(context.Background(), core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   core.TargetKindSkillAdmission,
		TargetID:     admCandidateA,
		ActorSubject: "cli:alice",
		AuthMethod:   core.AuthMethodCLI,
		Source:       "cli",
		CreatedAt:    admissionAt,
	}))
}

// admissionCandidate 造一条形状完整的提议；调用方按用例改字段。
func admissionCandidate(id, skill string) core.AdmissionCandidate {
	return core.AdmissionCandidate{
		CandidateID:        id,
		SkillName:          skill,
		CandidateHash:      admissionHash('c'),
		SourceHash:         admissionHash('b'),
		ManifestJSON:       `{"files":[]}`,
		SourceJSON:         `{"kind":"manual"}`,
		BaselineJSON:       `{"skill_state":"absent"}`,
		ProposerSubject:    "cli:alice",
		ProposerAuthMethod: "cli",
		ProposerSource:     "cli",
	}
}

// admissionEvaluation 造一条通过的评估；CandidateRevision 由调用方按当时候选设置。
func admissionEvaluation(c core.AdmissionCandidate, evaluationID, operationID string, revision int64) core.AdmissionEvaluation {
	return core.AdmissionEvaluation{
		EvaluationID:      evaluationID,
		CandidateID:       c.CandidateID,
		CandidateRevision: revision,
		CandidateHash:     c.CandidateHash,
		EvaluationHash:    admissionHash('e'),
		GraderVersion:     "admission-grader/v1",
		PolicyHash:        admissionHash('d'),
		Dimensions: []core.AdmissionDimension{
			{Name: core.AdmissionDimensionContent, Status: core.AdmissionDimensionPassed},
		},
		Scripts:      []string{},
		Aggregate:    core.AdmissionAggregatePassed,
		RiskTier:     core.AdmissionRiskNormal,
		Referrers:    []string{},
		BaselineJSON: c.BaselineJSON,
		EvaluatedAt:  admissionAt,
		OperationID:  operationID,
	}
}

// admissionApproval 造一条批准的决策行（四个非空列齐全）。
func admissionApproval(c core.AdmissionCandidate, e core.AdmissionEvaluation, decisionID, operationID string, revision int64, expiresAt time.Time) core.AdmissionDecision {
	return core.AdmissionDecision{
		DecisionID:         decisionID,
		CandidateID:        c.CandidateID,
		CandidateRevision:  revision,
		CandidateHash:      c.CandidateHash,
		EvaluationID:       e.EvaluationID,
		EvaluationHash:     e.EvaluationHash,
		Decision:           core.AdmissionApprove,
		Reason:             "reviewed on host",
		Acknowledged:       []core.AdmissionDimensionName{core.AdmissionDimensionEffect},
		ApprovedScopeJSON:  `{"skill":"demo"}`,
		ScriptReviewJSON:   `[]`,
		BaselineJSON:       c.BaselineJSON,
		RiskTier:           core.AdmissionRiskNormal,
		ApproverSubject:    "cli:bob",
		ApproverAuthMethod: "cli",
		ApproverSource:     "cli",
		ExpiresAt:          expiresAt,
		CreatedAt:          admissionAt,
		OperationID:        operationID,
	}
}

// TestAdmissionStore_CreateCandidateEnforcesActiveName：同名活跃候选只能有一条；
// 取消后同名可再提议；非法候选标识在写库之前被拒。
func TestUS21_Store_CreateCandidateEnforcesActiveName(t *testing.T) {
	db := portTestDB(t)
	store := NewAdmissionStore(db)
	ctx := context.Background()

	seedAdmissionOperation(t, db, "op-create-a", core.ActionSkillAdmissionPropose)
	a := admissionCandidate(admCandidateA, "demo-skill")
	require.NoError(t, store.CreateCandidate(ctx, a, "op-create-a", admissionAt))

	got, err := store.GetCandidate(ctx, a.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionProposed, got.State)
	require.Equal(t, int64(1), got.Revision)
	require.Equal(t, core.AdmissionRiskUnknown, got.RiskTier)
	require.Empty(t, got.TerminalReason)
	require.Equal(t, admissionAt.UnixMilli(), got.CreatedAt.UnixMilli())

	// 同名第二条（不同标识、不同操作）→ 活跃唯一索引拒绝，不重试、不覆盖。
	seedAdmissionOperation(t, db, "op-create-b", core.ActionSkillAdmissionPropose)
	b := admissionCandidate(admCandidateB, "demo-skill")
	err = store.CreateCandidate(ctx, b, "op-create-b", admissionAt)
	require.ErrorIs(t, err, core.ErrAdmissionActiveCandidateExists)

	// 取消 A 后同名可再次提议（部分索引只覆盖 proposed / evaluated / approved）。
	seedAdmissionOperation(t, db, "op-cancel-a", core.ActionSkillAdmissionCancel)
	_, err = store.CancelCandidate(ctx, a.CandidateID, "superseded by hand", "op-cancel-a", 1, admissionAt)
	require.NoError(t, err)
	seedAdmissionOperation(t, db, "op-create-b2", core.ActionSkillAdmissionPropose)
	require.NoError(t, store.CreateCandidate(ctx, b, "op-create-b2", admissionAt))

	// 非法标识与缺字段在任何 SQL 之前被拒。
	err = store.CreateCandidate(ctx, admissionCandidate("not-an-admission-id", "other"), "op-create-b2", admissionAt)
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
	broken := admissionCandidate(admCandidateC, "third")
	broken.CandidateHash = ""
	require.ErrorIs(t, store.CreateCandidate(ctx, broken, "op-create-b2", admissionAt), core.ErrAdmissionInvalidInput)
}

// TestAdmissionStore_CandidateStateCAS：每次状态推进都带修订号 CAS；未命中整体回滚
// （包括已被同一事务取代的活跃批准），已终态不再接受推进。
func TestUS21_Store_CandidateStateCAS(t *testing.T) {
	db := portTestDB(t)
	store := NewAdmissionStore(db)
	ctx := context.Background()

	seedAdmissionOperation(t, db, "op-c1", core.ActionSkillAdmissionPropose)
	c := admissionCandidate(admCandidateA, "demo-skill")
	require.NoError(t, store.CreateCandidate(ctx, c, "op-c1", admissionAt))

	seedAdmissionOperation(t, db, "op-e1", core.ActionSkillAdmissionEvaluate)
	e := admissionEvaluation(c, admEvalA, "op-e1", 1)

	// 修订号不匹配 → CAS 未命中。证据行与 CAS 期望必须同一修订（存储层前置校验），
	// 故用"一致的 7 对库内 1"表达陈旧修订，而不是让两者互相打架。
	// Stale revision: the evidence row and the CAS expectation agree with each other
	// (both 7) but not with the stored revision (1), so the CAS is what rejects it.
	stale := admissionEvaluation(c, admEvalA, "op-e1", 7)
	require.ErrorIs(t, store.InsertEvaluation(ctx, stale, 7, admissionAt), core.ErrAdmissionInvalidState)
	require.NoError(t, store.InsertEvaluation(ctx, e, 1, admissionAt))

	got, err := store.GetCandidate(ctx, c.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionEvaluated, got.State)
	require.Equal(t, int64(2), got.Revision)
	require.Equal(t, core.AdmissionRiskNormal, got.RiskTier)

	// 批准：只有 evaluated 接受；成功后候选 approved、修订号 +1。
	seedAdmissionOperation(t, db, "op-d1", core.ActionSkillAdmissionApprove)
	d := admissionApproval(c, e, admDecisionA, "op-d1", 2, admissionAt.Add(time.Hour))
	require.NoError(t, store.InsertDecision(ctx, d, 2, admissionAt))
	got, err = store.GetCandidate(ctx, c.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionApproved, got.State)
	require.Equal(t, int64(3), got.Revision)

	active, err := store.ActiveApproval(ctx, c.CandidateID)
	require.NoError(t, err)
	require.Equal(t, admDecisionA, active.DecisionID)

	// 二次批准：approve 的 CAS 要求 evaluated；事务内的 supersede 必须随回滚消失，
	// 原批准仍是活跃的（逆改点：去掉状态条件会让第二次批准成功或把首条标成 superseded）。
	seedAdmissionOperation(t, db, "op-d2", core.ActionSkillAdmissionApprove)
	d2 := admissionApproval(c, e, admDecisionB, "op-d2", 3, admissionAt.Add(time.Hour))
	require.ErrorIs(t, store.InsertDecision(ctx, d2, 3, admissionAt), core.ErrAdmissionInvalidState)
	active, err = store.ActiveApproval(ctx, c.CandidateID)
	require.NoError(t, err)
	require.Equal(t, admDecisionA, active.DecisionID)
	require.True(t, active.SupersededAt.IsZero())

	// 拒绝允许从 approved 走：候选进 rejected、写终态字段、活跃批准被取代。
	seedAdmissionOperation(t, db, "op-r1", core.ActionSkillAdmissionReject)
	reject := core.AdmissionDecision{
		DecisionID:         admDecisionC,
		CandidateID:        c.CandidateID,
		CandidateRevision:  3,
		CandidateHash:      c.CandidateHash,
		EvaluationID:       e.EvaluationID,
		EvaluationHash:     e.EvaluationHash,
		Decision:           core.AdmissionReject,
		Reason:             "permissions too wide",
		RiskTier:           core.AdmissionRiskNormal,
		ApproverSubject:    "cli:bob",
		ApproverAuthMethod: "cli",
		ApproverSource:     "cli",
		CreatedAt:          admissionAt,
		OperationID:        "op-r1",
	}
	require.NoError(t, store.InsertDecision(ctx, reject, 3, admissionAt))
	got, err = store.GetCandidate(ctx, c.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionRejected, got.State)
	require.Equal(t, "permissions too wide", got.TerminalReason)
	require.Equal(t, "op-r1", got.TerminalOperationID)
	_, err = store.ActiveApproval(ctx, c.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound)

	// rejected 是终态：评估与批准都不再接受。
	seedAdmissionOperation(t, db, "op-e2", core.ActionSkillAdmissionEvaluate)
	require.ErrorIs(t, store.InsertEvaluation(ctx, admissionEvaluation(c, admEvalB, "op-e2", 4), 4, admissionAt),
		core.ErrAdmissionInvalidState)
	seedAdmissionOperation(t, db, "op-d3", core.ActionSkillAdmissionApprove)
	require.ErrorIs(t, store.InsertDecision(ctx, admissionApproval(c, e, admDecisionD, "op-d3", 4, admissionAt.Add(time.Hour)), 4, admissionAt),
		core.ErrAdmissionInvalidState)
}

// TestAdmissionStore_LatestOrderedBySeq：同一毫秒写入的多条评估 / 决策按插入序取最新；
// 重新评估会取代活跃批准，且 `superseded_by` 记录取代它的操作号。
func TestUS21_Store_LatestOrderedBySeq(t *testing.T) {
	db := portTestDB(t)
	store := NewAdmissionStore(db)
	ctx := context.Background()

	seedAdmissionOperation(t, db, "op-seq-c", core.ActionSkillAdmissionPropose)
	c := admissionCandidate(admCandidateA, "demo-skill")
	require.NoError(t, store.CreateCandidate(ctx, c, "op-seq-c", admissionAt))

	seedAdmissionOperation(t, db, "op-seq-e1", core.ActionSkillAdmissionEvaluate)
	e1 := admissionEvaluation(c, admEvalA, "op-seq-e1", 1)
	require.NoError(t, store.InsertEvaluation(ctx, e1, 1, admissionAt))

	seedAdmissionOperation(t, db, "op-seq-d1", core.ActionSkillAdmissionApprove)
	d1 := admissionApproval(c, e1, admDecisionA, "op-seq-d1", 2, admissionAt.Add(time.Hour))
	require.NoError(t, store.InsertDecision(ctx, d1, 2, admissionAt))

	// 同一时间戳的第二次评估：取代 d1，候选回到 evaluated。
	seedAdmissionOperation(t, db, "op-seq-e2", core.ActionSkillAdmissionEvaluate)
	e2 := admissionEvaluation(c, admEvalB, "op-seq-e2", 3)
	require.NoError(t, store.InsertEvaluation(ctx, e2, 3, admissionAt))

	seedAdmissionOperation(t, db, "op-seq-d2", core.ActionSkillAdmissionApprove)
	d2 := admissionApproval(c, e2, admDecisionB, "op-seq-d2", 4, admissionAt.Add(time.Hour))
	require.NoError(t, store.InsertDecision(ctx, d2, 4, admissionAt))

	latestEval, err := store.LatestEvaluation(ctx, c.CandidateID)
	require.NoError(t, err)
	require.Equal(t, admEvalB, latestEval.EvaluationID)

	latestDecision, err := store.LatestDecision(ctx, c.CandidateID)
	require.NoError(t, err)
	require.Equal(t, admDecisionB, latestDecision.DecisionID)

	active, err := store.ActiveApproval(ctx, c.CandidateID)
	require.NoError(t, err)
	require.Equal(t, admDecisionB, active.DecisionID)

	superseded, err := store.FindDecisionByOperation(ctx, "op-seq-d1")
	require.NoError(t, err)
	require.False(t, superseded.SupersededAt.IsZero())
	require.Equal(t, "op-seq-e2", superseded.SupersededBy)

	byOp, err := store.FindEvaluationByOperation(ctx, "op-seq-e2")
	require.NoError(t, err)
	require.Equal(t, admEvalB, byOp.EvaluationID)
}

// TestAdmissionStore_ListCandidatesFiltersExpired：`expired` 是列表过滤的 SQL 翻译，
// 存储态保持 approved；未过期与终态候选不混进结果。
func TestUS21_Store_ListCandidatesFiltersExpired(t *testing.T) {
	db := portTestDB(t)
	store := NewAdmissionStore(db)
	ctx := context.Background()

	// 候选 A：批准已过期（expires_at 在过去）。
	seedAdmissionOperation(t, db, "op-list-ca", core.ActionSkillAdmissionPropose)
	a := admissionCandidate(admCandidateA, "alpha-skill")
	require.NoError(t, store.CreateCandidate(ctx, a, "op-list-ca", admissionAt))
	seedAdmissionOperation(t, db, "op-list-ea", core.ActionSkillAdmissionEvaluate)
	ea := admissionEvaluation(a, admEvalA, "op-list-ea", 1)
	require.NoError(t, store.InsertEvaluation(ctx, ea, 1, admissionAt))
	seedAdmissionOperation(t, db, "op-list-da", core.ActionSkillAdmissionApprove)
	da := admissionApproval(a, ea, admDecisionA, "op-list-da", 2, time.Now().Add(-time.Hour))
	require.NoError(t, store.InsertDecision(ctx, da, 2, admissionAt))

	// 候选 B：批准仍在有效期内。
	seedAdmissionOperation(t, db, "op-list-cb", core.ActionSkillAdmissionPropose)
	b := admissionCandidate(admCandidateB, "beta-skill")
	require.NoError(t, store.CreateCandidate(ctx, b, "op-list-cb", admissionAt))
	seedAdmissionOperation(t, db, "op-list-eb", core.ActionSkillAdmissionEvaluate)
	eb := admissionEvaluation(b, admEvalB, "op-list-eb", 1)
	require.NoError(t, store.InsertEvaluation(ctx, eb, 1, admissionAt))
	seedAdmissionOperation(t, db, "op-list-db", core.ActionSkillAdmissionApprove)
	dbb := admissionApproval(b, eb, admDecisionB, "op-list-db", 2, time.Now().Add(time.Hour))
	require.NoError(t, store.InsertDecision(ctx, dbb, 2, admissionAt))

	expired, err := store.ListCandidates(ctx, core.AdmissionFilter{State: core.AdmissionExpired})
	require.NoError(t, err)
	require.Len(t, expired, 1)
	require.Equal(t, admCandidateA, expired[0].CandidateID)
	// 过滤不改变存储态：A 仍是 approved。
	require.Equal(t, core.AdmissionApproved, expired[0].State)

	approved, err := store.ListCandidates(ctx, core.AdmissionFilter{State: core.AdmissionApproved})
	require.NoError(t, err)
	require.Len(t, approved, 2)

	bySkill, err := store.ListCandidates(ctx, core.AdmissionFilter{Skill: "beta-skill"})
	require.NoError(t, err)
	require.Len(t, bySkill, 1)
	require.Equal(t, admCandidateB, bySkill[0].CandidateID)

	_, err = store.ListCandidates(ctx, core.AdmissionFilter{State: "bogus"})
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)
}

// admissionFixture 是发布 / 撤回用例的固定装置：一条已发布的旧版技能、一个已批准
// 候选，以及按需追加的管理操作号。
type admissionFixture struct {
	db         *DB
	store      *AdmissionStore
	resources  *ResourceStore
	skill      string
	uid        core.ResourceUID
	v1         core.ResourceVersion
	baseline   core.AdmissionBaseline
	candidate  core.AdmissionCandidate
	evaluation core.AdmissionEvaluation
	decision   core.AdmissionDecision
	opSeq      int
}

// seedOp 预写一行管理操作并返回标识（同一 fixture 内递增，避免主键冲突）。
func (f *admissionFixture) seedOp(t *testing.T, action string) string {
	t.Helper()
	f.opSeq++
	id := f.skill + "-op-" + string(rune('a'+f.opSeq))
	seedAdmissionOperation(t, f.db, id, action)
	return id
}

// newAdmissionFixture 造「旧版 v1 已发布 + 候选已评估已批准」的起点。
func newAdmissionFixture(t *testing.T, skill, candidateID string, uidLast byte) *admissionFixture {
	t.Helper()
	db := portTestDB(t)
	f := &admissionFixture{
		db:        db,
		store:     NewAdmissionStore(db),
		resources: NewResourceStore(db),
		skill:     skill,
		uid:       testResourceUID(uidLast),
	}
	ctx := context.Background()

	opV1 := f.seedOp(t, core.ActionSkillCreate)
	v1, err := f.resources.PublishWithVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceID:   skill,
		ResourceUID:  f.uid,
		ContentHash:  admissionHash('1'),
		ManifestJSON: `{"files":[]}`,
		StoragePath:  "skills/" + skill + "/1",
		OperationID:  opV1,
	}, opV1, admissionAt)
	require.NoError(t, err)
	f.v1 = v1

	f.baseline = core.AdmissionBaseline{
		SkillState:         core.AdmissionSkillPresent,
		ResourceUID:        f.uid,
		PublishedVersionID: v1.ID,
		VersionNo:          v1.VersionNo,
		ContentHash:        v1.ContentHash,
		PointerRevision:    1,
		CanonicalJSON:      `{"skill_state":"present"}`,
	}

	c := admissionCandidate(candidateID, skill)
	c.BaselineJSON = f.baseline.CanonicalJSON
	require.NoError(t, f.store.CreateCandidate(ctx, c, f.seedOp(t, core.ActionSkillAdmissionPropose), admissionAt))

	e := admissionEvaluation(c, admEvalA, f.seedOp(t, core.ActionSkillAdmissionEvaluate), 1)
	e.BaselineJSON = f.baseline.CanonicalJSON
	require.NoError(t, f.store.InsertEvaluation(ctx, e, 1, admissionAt))

	d := admissionApproval(c, e, admDecisionA, f.seedOp(t, core.ActionSkillAdmissionApprove), 2, admissionAt.Add(time.Hour))
	d.BaselineJSON = f.baseline.CanonicalJSON
	require.NoError(t, f.store.InsertDecision(ctx, d, 2, admissionAt))

	f.candidate, f.evaluation, f.decision = c, e, d
	return f
}

// publishRequest 造一个版本号与基线固定的发布请求（版本号 = 2）。
func (f *admissionFixture) publishRequest(operationID string) core.AdmissionPublishRequest {
	return core.AdmissionPublishRequest{
		CandidateID:       f.candidate.CandidateID,
		CandidateRevision: 3,
		DecisionID:        f.decision.DecisionID,
		Version: core.ResourceVersion{
			Kind:         core.ResourceSkill,
			ResourceID:   f.skill,
			ResourceUID:  f.uid,
			VersionNo:    2,
			ContentHash:  admissionHash('2'),
			ManifestJSON: `{"files":[]}`,
			StoragePath:  "skills/" + f.skill + "/2",
		},
		Baseline:    f.baseline,
		OperationID: operationID,
	}
}

// TestAdmissionStore_PublishThenWithdrawRestoresPrevious：发布事务写版本、指针、发布记录
// 与候选状态；撤回时把上一版内容作为新版本重新发布，并记录 restored_previous。
func TestUS21_Store_PublishThenWithdrawRestoresPrevious(t *testing.T) {
	f := newAdmissionFixture(t, "demo-skill", admCandidateA, 'S')
	ctx := context.Background()

	pubOp := f.seedOp(t, core.ActionSkillAdmissionPublish)
	publication, err := f.store.PublishAdmission(ctx, f.publishRequest(pubOp), admissionAt)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionPublicationActive, publication.State)
	require.Equal(t, int64(2), publication.VersionNo)
	require.Equal(t, f.uid, publication.SkillUID)
	require.Equal(t, f.decision.DecisionID, publication.DecisionID)

	got, err := f.store.GetCandidate(ctx, f.candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionPublished, got.State)
	require.Equal(t, int64(4), got.Revision)

	pointer, err := f.resources.Pointer(ctx, core.ResourceSkill, f.skill)
	require.NoError(t, err)
	require.Equal(t, publication.VersionID, pointer.PublishedVersionID)

	stored, err := f.store.GetPublication(ctx, f.candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, publication.VersionID, stored.VersionID)
	byOp, err := f.store.FindPublicationByOperation(ctx, pubOp)
	require.NoError(t, err)
	require.Equal(t, publication.VersionID, byOp.VersionID)

	// 再次发布：一个候选最多一次成功发布——发布记录已存在即在事务内拒绝，
	// 版本、指针与候选状态全部保持原样。
	repub := f.publishRequest(pubOp)
	repub.Version.VersionNo = 0
	_, repubErr := f.store.PublishAdmission(ctx, repub, admissionAt)
	require.ErrorIs(t, repubErr, core.ErrAdmissionInvalidState)

	pointerAfter, err := f.resources.Pointer(ctx, core.ResourceSkill, f.skill)
	require.NoError(t, err)
	require.Equal(t, publication.VersionID, pointerAfter.PublishedVersionID)

	// 撤回：指针仍指向本次发布，上一版内容作为新版本（v3）重新发布。
	withdrawOp := f.seedOp(t, core.ActionSkillAdmissionWithdraw)
	withdraw, err := f.store.WithdrawAdmission(ctx, core.AdmissionWithdrawRequest{
		CandidateID:       f.candidate.CandidateID,
		CandidateRevision: 4,
		Publication:       publication,
		WithdrawVersion: &core.ResourceVersion{
			Kind:         core.ResourceSkill,
			ResourceID:   f.skill,
			ResourceUID:  f.uid,
			VersionNo:    3,
			ContentHash:  f.v1.ContentHash,
			ManifestJSON: `{"files":[]}`,
			StoragePath:  "skills/" + f.skill + "/3",
		},
		Reason:      "regression found",
		OperationID: withdrawOp,
	}, admissionAt)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionPublicationWithdrawn, withdraw.State)
	require.Equal(t, core.AdmissionWithdrawRestoredPrevious, withdraw.WithdrawReason)
	require.NotZero(t, withdraw.WithdrawVersionID)

	pointer, err = f.resources.Pointer(ctx, core.ResourceSkill, f.skill)
	require.NoError(t, err)
	require.Equal(t, withdraw.WithdrawVersionID, pointer.PublishedVersionID)
	require.NotEqual(t, publication.VersionID, pointer.PublishedVersionID)

	got, err = f.store.GetCandidate(ctx, f.candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionWithdrawn, got.State)
	require.Equal(t, "regression found", got.TerminalReason)

	byWithdraw, err := f.store.FindPublicationByWithdrawOperation(ctx, withdrawOp)
	require.NoError(t, err)
	require.Equal(t, withdraw.WithdrawVersionID, byWithdraw.WithdrawVersionID)

	// 已撤回的发布记录不能再撤回。
	_, againErr := f.store.WithdrawAdmission(ctx, core.AdmissionWithdrawRequest{
		CandidateID:       f.candidate.CandidateID,
		CandidateRevision: 5,
		Publication:       withdraw,
		Reason:            "again",
		OperationID:       withdrawOp,
	}, admissionAt)
	require.ErrorIs(t, againErr, core.ErrAdmissionInvalidState)
}

// TestAdmissionStore_WithdrawSuperseded：指针已被后续发布超越时只记录撤回，不移动指针。
func TestUS21_Store_WithdrawSuperseded(t *testing.T) {
	f := newAdmissionFixture(t, "demo-skill", admCandidateA, 'T')
	ctx := context.Background()

	publication, err := f.store.PublishAdmission(ctx, f.publishRequest(f.seedOp(t, core.ActionSkillAdmissionPublish)), admissionAt)
	require.NoError(t, err)

	// 另一次普通发布把指针推进到 v3。
	opLater := f.seedOp(t, core.ActionSkillUpdate)
	later, err := f.resources.PublishWithVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceID:   f.skill,
		ResourceUID:  f.uid,
		ContentHash:  admissionHash('3'),
		ManifestJSON: `{"files":[]}`,
		StoragePath:  "skills/" + f.skill + "/3",
		OperationID:  opLater,
	}, opLater, admissionAt)
	require.NoError(t, err)

	withdrawn, err := f.store.WithdrawAdmission(ctx, core.AdmissionWithdrawRequest{
		CandidateID:       f.candidate.CandidateID,
		CandidateRevision: 4,
		Publication:       publication,
		Reason:            "superseded by a later release",
		OperationID:       f.seedOp(t, core.ActionSkillAdmissionWithdraw),
	}, admissionAt)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionWithdrawSuperseded, withdrawn.WithdrawReason)
	require.Zero(t, withdrawn.WithdrawVersionID)

	pointer, err := f.resources.Pointer(ctx, core.ResourceSkill, f.skill)
	require.NoError(t, err)
	require.Equal(t, later.ID, pointer.PublishedVersionID)

	// 分类已由事务内指针决定：此时再传撤回版本会与分类冲突，必须报错而不是写错版本。
	_, conflictErr := f.store.WithdrawAdmission(ctx, core.AdmissionWithdrawRequest{
		CandidateID:       f.candidate.CandidateID,
		CandidateRevision: 5,
		Publication:       withdrawn,
		WithdrawVersion:   &core.ResourceVersion{Kind: core.ResourceSkill, ResourceID: f.skill, ResourceUID: f.uid, ContentHash: admissionHash('3'), ManifestJSON: `{"files":[]}`, StoragePath: "skills/" + f.skill + "/4"},
		Reason:            "again",
		OperationID:       f.seedOp(t, core.ActionSkillAdmissionWithdraw),
	}, admissionAt)
	require.ErrorIs(t, conflictErr, core.ErrAdmissionInvalidState)
}

// TestAdmissionStore_WithdrawDeleted：技能已删除时只记录撤回；带撤回版本会报错。
func TestUS21_Store_WithdrawDeleted(t *testing.T) {
	f := newAdmissionFixture(t, "demo-skill", admCandidateA, 'V')
	ctx := context.Background()

	publication, err := f.store.PublishAdmission(ctx, f.publishRequest(f.seedOp(t, core.ActionSkillAdmissionPublish)), admissionAt)
	require.NoError(t, err)

	require.NoError(t, f.resources.MarkDeleted(ctx, core.ResourceSkill, f.skill, f.seedOp(t, core.ActionSkillDelete), admissionAt))

	withdrawn, err := f.store.WithdrawAdmission(ctx, core.AdmissionWithdrawRequest{
		CandidateID:       f.candidate.CandidateID,
		CandidateRevision: 4,
		Publication:       publication,
		Reason:            "skill removed from the host",
		OperationID:       f.seedOp(t, core.ActionSkillAdmissionWithdraw),
	}, admissionAt)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionWithdrawDeleted, withdrawn.WithdrawReason)
	require.Zero(t, withdrawn.WithdrawVersionID)
}

// TestAdmissionStore_ConcurrentPublishSingleWinner：同一候选的两次并发发布只有一个
// 提交；输家整体回滚（版本、发布记录、候选状态都不留痕）。
func TestUS21_Store_ConcurrentPublishSingleWinner(t *testing.T) {
	f := newAdmissionFixture(t, "demo-skill", admCandidateA, 'W')
	ctx := context.Background()

	opLeft := f.seedOp(t, core.ActionSkillAdmissionPublish)
	opRight := f.seedOp(t, core.ActionSkillAdmissionPublish)
	// 版本号预检置 0：两个请求只差事务内的分配与指针期望，失败点落在并发语义本身。
	left, right := f.publishRequest(opLeft), f.publishRequest(opRight)
	left.Version.VersionNo, right.Version.VersionNo = 0, 0
	requests := []core.AdmissionPublishRequest{left, right}

	type outcome struct {
		publication core.AdmissionPublication
		err         error
	}
	results := make([]outcome, len(requests))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, req := range requests {
		wg.Add(1)
		go func(i int, req core.AdmissionPublishRequest) {
			defer wg.Done()
			<-start
			pub, err := f.store.PublishAdmission(ctx, req, admissionAt)
			results[i] = outcome{publication: pub, err: err}
		}(i, req)
	}
	close(start)
	wg.Wait()

	var (
		winners int
		losers  int
		winner  core.AdmissionPublication
	)
	for _, res := range results {
		if res.err == nil {
			winners++
			winner = res.publication
			continue
		}
		losers++
		require.True(t,
			errors.Is(res.err, core.ErrAdmissionStaleBaseline) || errors.Is(res.err, core.ErrAdmissionInvalidState),
			"输家必须因基线漂移或状态 CAS 失败，实际: %v", res.err)
	}
	require.Equal(t, 1, winners, "同一候选只能有一次成功发布")
	require.Equal(t, 1, losers)

	stored, err := f.store.GetPublication(ctx, f.candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, winner.VersionID, stored.VersionID)

	pointer, err := f.resources.Pointer(ctx, core.ResourceSkill, f.skill)
	require.NoError(t, err)
	require.Equal(t, winner.VersionID, pointer.PublishedVersionID)

	candidate, err := f.store.GetCandidate(ctx, f.candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionPublished, candidate.State)
}

// TestAdmissionStore_PublishRejectsStaleBaseline：决策基线之后指针发生变化（这里是删除），
// 发布必须因基线漂移回滚，且候选仍是 approved。
func TestUS21_Store_PublishRejectsStaleBaseline(t *testing.T) {
	f := newAdmissionFixture(t, "demo-skill", admCandidateA, 'X')
	ctx := context.Background()

	require.NoError(t, f.resources.MarkDeleted(ctx, core.ResourceSkill, f.skill, f.seedOp(t, core.ActionSkillDelete), admissionAt))

	_, err := f.store.PublishAdmission(ctx, f.publishRequest(f.seedOp(t, core.ActionSkillAdmissionPublish)), admissionAt)
	require.ErrorIs(t, err, core.ErrAdmissionStaleBaseline)

	candidate, err := f.store.GetCandidate(ctx, f.candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionApproved, candidate.State)

	_, err = f.store.GetPublication(ctx, f.candidate.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound)
}

// TestAdmissionStore_PublishRejectsVersionNumberDrift：调用方在快照阶段算好的版本号
// 与事务内实际分配不一致时整体回滚——`storage_path` 与版本号必须同源。
func TestUS21_Store_PublishRejectsVersionNumberDrift(t *testing.T) {
	f := newAdmissionFixture(t, "demo-skill", admCandidateA, 'Y')
	ctx := context.Background()

	req := f.publishRequest(f.seedOp(t, core.ActionSkillAdmissionPublish))
	req.Version.VersionNo = 9 // 实际分配为 2
	_, err := f.store.PublishAdmission(ctx, req, admissionAt)
	require.ErrorIs(t, err, core.ErrAdmissionInvalidInput)

	candidate, err := f.store.GetCandidate(ctx, f.candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionApproved, candidate.State)
	_, err = f.store.GetPublication(ctx, f.candidate.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound)
}

// insertAdmissionDecisionRaw 绕过仓储直接写一条 approve 决策行，用来验证迁移层的
// 部分唯一索引本身（仓储会先 supersede，看不到索引的拒绝路径）。
// insertAdmissionDecisionRaw writes an approval row directly to exercise the DDL index.
func insertAdmissionDecisionRaw(t *testing.T, db *DB, d core.AdmissionDecision) error {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO skill_admission_decisions
		   (decision_id, candidate_id, candidate_revision, candidate_hash, evaluation_id,
		    evaluation_hash, decision, reason, acknowledged_json, approved_scope_json,
		    script_review_json, baseline_json, risk_tier, approver_subject,
		    approver_auth_method, approver_source, expires_at, superseded_at,
		    superseded_by, created_at, operation_id)
		 VALUES ($1, $2, $3, $4, $5, $6, 'approve', $7, '["effect"]', $8, $9, $10,
		         'normal', 'cli:bob', 'cli', 'cli', $11, NULL, NULL, $12, $13)`,
		d.DecisionID, d.CandidateID, d.CandidateRevision, d.CandidateHash, d.EvaluationID,
		d.EvaluationHash, d.Reason, d.ApprovedScopeJSON, d.ScriptReviewJSON, d.BaselineJSON,
		d.ExpiresAt.UnixMilli(), admissionAt.UnixMilli(), d.OperationID)
	return err
}

// TestUS21_Migration_PartialUniqueIndex：迁移层的部分唯一索引只约束"活跃"批准——
// 直接插入第二条 approve 触发唯一冲突；把第一条标 superseded 后，同一候选可以再有
// 一条活跃批准。
// 逆改点：把部分索引改成普通 `UNIQUE (candidate_id)`，后半段仍报唯一冲突而红；
// 去掉索引，前半段插入成功而红。
// TestUS21_Migration_PartialUniqueIndex pins the active-approval DDL index directly.
func TestUS21_Migration_PartialUniqueIndex(t *testing.T) {
	db := portTestDB(t)
	store := NewAdmissionStore(db)
	ctx := context.Background()

	seedAdmissionOperation(t, db, "op-u-c", core.ActionSkillAdmissionPropose)
	c := admissionCandidate(admCandidateA, "demo-skill")
	require.NoError(t, store.CreateCandidate(ctx, c, "op-u-c", admissionAt))
	seedAdmissionOperation(t, db, "op-u-e", core.ActionSkillAdmissionEvaluate)
	e := admissionEvaluation(c, admEvalA, "op-u-e", 1)
	require.NoError(t, store.InsertEvaluation(ctx, e, 1, admissionAt))

	seedAdmissionOperation(t, db, "op-u-d1", core.ActionSkillAdmissionApprove)
	first := admissionApproval(c, e, admDecisionA, "op-u-d1", 2, admissionAt.Add(time.Hour))
	require.NoError(t, store.InsertDecision(ctx, first, 2, admissionAt))

	// 第二条活跃批准：索引必须拒绝，且拒绝后只有第一条存在。
	seedAdmissionOperation(t, db, "op-u-d2", core.ActionSkillAdmissionApprove)
	second := admissionApproval(c, e, admDecisionB, "op-u-d2", 2, admissionAt.Add(time.Hour))
	err := insertAdmissionDecisionRaw(t, db, second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "skill_admission_decisions_active_approval")
	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM skill_admission_decisions`).Scan(&count))
	require.Equal(t, 1, count)

	// supersede 之后同一候选可再有一条活跃批准（部分索引只覆盖 superseded_at IS NULL）。
	_, err = db.ExecContext(ctx,
		`UPDATE skill_admission_decisions
		    SET superseded_at = $1, superseded_by = 'op-u-d2'
		  WHERE decision_id = $2`,
		admissionAt.UnixMilli(), admDecisionA)
	require.NoError(t, err)
	require.NoError(t, insertAdmissionDecisionRaw(t, db, second))
}
