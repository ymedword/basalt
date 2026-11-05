//go:build integration

package app

// 本文件是学习准入端口适配器的验收面（颗粒度文档 §3.1.10、§3.2 P-04、T012）：
//
//	P1 `TestUS21_AdmissionPort_ProposeEvaluateListShow`——真库 + 真工作区走完整只读 /
//	   写链路：propose → evaluate → list → show，策略快照取自数据库三张表
//	P2 `TestUS21_AdmissionPort_MissingPolicyRowRejected`——策略表缺行时 evaluate 失败且
//	   零写入（不取空默认、不落评估行）
//	P3 `TestUS21_AdmissionPort_UntrustedActorStillRejected`——适配器这一层的纵深防御：
//	   即使绕过命令面，写方法仍拒绝 cli:unknown
//
// 判据只看真实数据库行与真实磁盘：policy_hash 随数据库行变化、评估行 / 操作行来自
// 真实仓储；命令面的"开库前拒绝"由 cli 侧用例证明，这里只验适配器装配。
// Every judgement reads real rows and real files; the CLI's pre-open rejection is proven
// on the command side, while these cases cover the adapter's own assembly.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
	"github.com/ymedword/basalt/internal/workspace"
)

// us21PortFixture 是一次端口用例的装配：真工作区目录 + 隔离 PG schema + 真端口。
// us21PortFixture wires a real workspace, an isolated schema and the real port.
type us21PortFixture struct {
	port   admissionPort
	db     *postgres.DB
	root   string
	actor  core.Actor
	source []byte
}

// newUS21PortFixture 建夹具；策略行默认已种（P2 用 seedPolicy=false 造缺行）。
// newUS21PortFixture seeds the policy rows unless the case wants them missing.
func newUS21PortFixture(t *testing.T, seedPolicy bool) *us21PortFixture {
	t.Helper()
	fixture := pgtest.NewFixture(t)
	workDir := t.TempDir()
	root := filepath.Join(workDir, workspaceDirName)
	require.NoError(t, os.MkdirAll(root, 0o755))
	// 冻结目录只读，Windows 的 TempDir 清收需要先恢复可写。
	// The frozen tree is read-only; restore write permission before cleanup.
	t.Cleanup(func() { makeUS21TreeWritable(t, root) })

	if seedPolicy {
		seedUS21PortPolicy(t, fixture.DB)
	}
	logger, _ := newCapturedLogger()
	return &us21PortFixture{
		port: admissionPort{
			WorkDir:                workDir,
			SupportedSchemaVersion: postgres.SupportedSchemaVersion,
			Database:               cli.DatabaseConfig{DSN: fixture.DSN},
			Logger:                 logger,
		},
		db:     fixture.DB,
		root:   root,
		actor:  core.Actor{Subject: "cli:alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		source: []byte(us21AdmissionSourceJSON),
	}
}

// seedUS21PortPolicy 种入策略快照的三张表（执行档位 + 两个域名作用域；无例外）。
// seedUS21PortPolicy inserts the three policy sources the snapshot requires.
func seedUS21PortPolicy(t *testing.T, db *postgres.DB) {
	t.Helper()
	now := admissionEvalNow
	_, err := postgres.NewExecutionPolicyStore(db).Seed(t.Context(), core.ProfileStandard,
		"op-us21-port-seed", now)
	require.NoError(t, err)
	_, err = postgres.NewNetworkPolicyStore(db).Seed(t.Context(),
		core.PolicyScopeHTTPDomains, []string{"api.example.com"}, now)
	require.NoError(t, err)
	_, err = postgres.NewNetworkPolicyStore(db).Seed(t.Context(),
		core.PolicyScopeHTTPDeniedDomains, nil, now)
	require.NoError(t, err)
}

// proposeOne 走端口冻结一个候选。
// proposeOne freezes one candidate through the port.
func (f *us21PortFixture) proposeOne(t *testing.T, skillName string) core.AdmissionCandidate {
	t.Helper()
	candidate, err := f.port.Propose(t.Context(), f.actor, core.AdmissionProposeRequest{
		SkillName:  skillName,
		Files:      admissionEvalFiles(skillName),
		SourceJSON: f.source,
	})
	require.NoError(t, err)
	return candidate
}

// P1：propose → evaluate → list → show 全链路；policy_hash 来自数据库策略快照。
func TestUS21_AdmissionPort_ProposeEvaluateListShow(t *testing.T) {
	t.Parallel()
	f := newUS21PortFixture(t, true)

	candidate := f.proposeOne(t, "demo-skill")
	require.Equal(t, core.AdmissionProposed, candidate.State)
	require.Equal(t, f.actor.Subject, candidate.ProposerSubject)
	require.DirExists(t, filepath.Join(f.root, us21CandidateDirRel(candidate.CandidateID)))

	policyCfg := core.AdmissionPolicyConfig{
		CrossAgentRead:  []string{"b", "a"},
		AllowedCommands: []string{"git", "go"},
		AllowRunCode:    true,
	}
	evaluation, err := f.port.Evaluate(t.Context(), f.actor, candidate.CandidateID, policyCfg)
	require.NoError(t, err)
	require.Equal(t, candidate.CandidateID, evaluation.CandidateID)
	require.Len(t, evaluation.Dimensions, 7)
	require.Equal(t, core.AdmissionAggregateIncomplete, evaluation.Aggregate)
	require.NotEmpty(t, evaluation.PolicyHash)
	require.NotEmpty(t, evaluation.EvaluationHash)

	// 同一输入再拍一次快照：哈希稳定（数据库行未变，规范化必须与遍历顺序无关）。
	again, err := admissionPolicySnapshot(t.Context(), f.db, core.AdmissionPolicyConfig{
		CrossAgentRead:  []string{"a", "b"},
		AllowedCommands: []string{"go", "git"},
		AllowRunCode:    true,
	})
	require.NoError(t, err)
	require.Equal(t, evaluation.PolicyHash, again.Hash, "策略哈希必须与输入顺序无关")

	candidates, err := f.port.List(t.Context(), core.AdmissionFilter{Skill: "demo-skill"})
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.Equal(t, core.AdmissionEvaluated, candidates[0].State)

	detail, err := f.port.Get(t.Context(), candidate.CandidateID,
		core.AdmissionShowOptions{IncludeFiles: true})
	require.NoError(t, err)
	require.Equal(t, candidate.CandidateID, detail.Candidate.CandidateID)
	require.NotNil(t, detail.Evaluation)
	require.Equal(t, evaluation.EvaluationID, detail.Evaluation.EvaluationID)
	require.Equal(t, "manual", detail.Source.Kind)
	require.Len(t, detail.Files, 1)
	require.Equal(t, "SKILL.md", detail.Files[0].Path)
}

// P2：策略表缺行时 evaluate 失败且零写入（不取空默认）。
func TestUS21_AdmissionPort_MissingPolicyRowRejected(t *testing.T) {
	t.Parallel()
	f := newUS21PortFixture(t, false)
	candidate := f.proposeOne(t, "demo-skill")

	_, err := f.port.Evaluate(t.Context(), f.actor, candidate.CandidateID,
		core.AdmissionPolicyConfig{})
	require.ErrorContains(t, err, "策略快照不完整")

	store := postgres.NewAdmissionStore(f.db)
	row, err := store.GetCandidate(t.Context(), candidate.CandidateID)
	require.NoError(t, err)
	require.Equal(t, core.AdmissionProposed, row.State, "快照失败不得推进候选状态")
	_, err = store.LatestEvaluation(t.Context(), candidate.CandidateID)
	require.ErrorIs(t, err, core.ErrAdmissionNotFound, "快照失败不得落评估行")
}

// P3：适配器层的纵深防御——写方法仍拒绝不可信主体（命令面是第一道）。
func TestUS21_AdmissionPort_UntrustedActorStillRejected(t *testing.T) {
	t.Parallel()
	f := newUS21PortFixture(t, true)
	untrusted := core.Actor{Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI}

	_, err := f.port.Propose(t.Context(), untrusted, core.AdmissionProposeRequest{
		SkillName:  "demo-skill",
		Files:      admissionEvalFiles("demo-skill"),
		SourceJSON: f.source,
	})
	require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor)
	require.NoDirExists(t, filepath.Join(f.root, workspace.AdmissionsDirName, "candidates"))
}
