package cli

// 本文件是学习准入命令面的验收面（颗粒度文档 §3.1.10、T012）：
//
//	L1 `TestUS21_CLI_UntrustedActorRejected`——cli:unknown
//	   在取锁与调用端口之前被拒（端口即开库入口，未调用即未开库）
//	L2 `TestUS21_CLI_WriteCommandsTakeLock`——写命令持锁执行、退出释放
//	L3 `TestUS21_CLI_ReadCommandsNoLock`——list / show 只读、零取锁
//	L4 `TestUS21_CLI_WriteRequiresConfigBeforeLock`——YAML 缺失在取锁之前失败
//	L5 `TestUS21_CLI_ShowKeepsBodiesOffHumanOutput`——人类输出不回显正文与决策理由，
//	   --files 与 --json 才是显式的正文出口
//	L6 `TestUS21_CLI_MissingPortIsWiringGap`——端口缺失报缺件而不是自造第二条路径
//
// 判据只看命令面的行为：端口是记录调用的替身，锁是计数闭包——不需要数据库。
// The command surface's acceptance: a recording fake port and a counting lock closure.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeAdmissionPort 记录调用顺序；未注入的方法返回零值（用到的用例都显式给）。
// fakeAdmissionPort records call order; unset methods return zero values.
type fakeAdmissionPort struct {
	calls []string

	propose     func(context.Context, core.Actor, core.AdmissionProposeRequest) (core.AdmissionCandidate, error)
	evaluate    func(context.Context, core.Actor, string, core.AdmissionPolicyConfig) (core.AdmissionEvaluation, error)
	approve     func(context.Context, core.Actor, string, []byte, core.AdmissionPolicyConfig) (core.AdmissionDecision, error)
	reject      func(context.Context, core.Actor, string, []byte) (core.AdmissionDecision, error)
	cancel      func(context.Context, core.Actor, string, string) (core.AdmissionCandidate, error)
	publish     func(context.Context, core.Actor, string, core.AdmissionPolicyConfig) (core.AdmissionPublication, error)
	withdraw    func(context.Context, core.Actor, string, string) (core.AdmissionPublication, error)
	list        func(context.Context, core.AdmissionFilter) ([]core.AdmissionCandidate, error)
	get         func(context.Context, string, core.AdmissionShowOptions) (core.AdmissionDetail, error)
	importFiles func(string) ([]core.AuthorFile, error)
	readSource  func(string) ([]byte, error)
	readDecide  func(string) ([]byte, error)
	onEveryCall func(string)
}

func (p *fakeAdmissionPort) record(name string) {
	p.calls = append(p.calls, name)
	if p.onEveryCall != nil {
		p.onEveryCall(name)
	}
}

func (p *fakeAdmissionPort) ImportFiles(dir string) ([]core.AuthorFile, error) {
	p.record("import")
	if p.importFiles != nil {
		return p.importFiles(dir)
	}
	return nil, nil
}

func (p *fakeAdmissionPort) ReadSourceFile(path string) ([]byte, error) {
	p.record("source")
	if p.readSource != nil {
		return p.readSource(path)
	}
	return nil, nil
}

func (p *fakeAdmissionPort) ReadDecisionFile(path string) ([]byte, error) {
	p.record("decision")
	if p.readDecide != nil {
		return p.readDecide(path)
	}
	return nil, nil
}

func (p *fakeAdmissionPort) Approve(ctx context.Context, actor core.Actor, candidateID string,
	decisionJSON []byte, policy core.AdmissionPolicyConfig,
) (core.AdmissionDecision, error) {
	p.record("approve")
	if p.approve != nil {
		return p.approve(ctx, actor, candidateID, decisionJSON, policy)
	}
	return core.AdmissionDecision{}, nil
}

func (p *fakeAdmissionPort) Reject(ctx context.Context, actor core.Actor, candidateID string,
	decisionJSON []byte,
) (core.AdmissionDecision, error) {
	p.record("reject")
	if p.reject != nil {
		return p.reject(ctx, actor, candidateID, decisionJSON)
	}
	return core.AdmissionDecision{}, nil
}

func (p *fakeAdmissionPort) Cancel(ctx context.Context, actor core.Actor, candidateID, reason string,
) (core.AdmissionCandidate, error) {
	p.record("cancel")
	if p.cancel != nil {
		return p.cancel(ctx, actor, candidateID, reason)
	}
	return core.AdmissionCandidate{}, nil
}

func (p *fakeAdmissionPort) Publish(ctx context.Context, actor core.Actor, candidateID string,
	policy core.AdmissionPolicyConfig,
) (core.AdmissionPublication, error) {
	p.record("publish")
	if p.publish != nil {
		return p.publish(ctx, actor, candidateID, policy)
	}
	return core.AdmissionPublication{}, nil
}

func (p *fakeAdmissionPort) Withdraw(ctx context.Context, actor core.Actor, candidateID, reason string,
) (core.AdmissionPublication, error) {
	p.record("withdraw")
	if p.withdraw != nil {
		return p.withdraw(ctx, actor, candidateID, reason)
	}
	return core.AdmissionPublication{}, nil
}

func (p *fakeAdmissionPort) Propose(ctx context.Context, actor core.Actor,
	req core.AdmissionProposeRequest,
) (core.AdmissionCandidate, error) {
	p.record("propose")
	if p.propose != nil {
		return p.propose(ctx, actor, req)
	}
	return core.AdmissionCandidate{}, nil
}

func (p *fakeAdmissionPort) Evaluate(ctx context.Context, actor core.Actor, candidateID string,
	policy core.AdmissionPolicyConfig,
) (core.AdmissionEvaluation, error) {
	p.record("evaluate")
	if p.evaluate != nil {
		return p.evaluate(ctx, actor, candidateID, policy)
	}
	return core.AdmissionEvaluation{}, nil
}

func (p *fakeAdmissionPort) List(ctx context.Context,
	filter core.AdmissionFilter,
) ([]core.AdmissionCandidate, error) {
	p.record("list")
	if p.list != nil {
		return p.list(ctx, filter)
	}
	return nil, nil
}

func (p *fakeAdmissionPort) Get(ctx context.Context, candidateID string,
	options core.AdmissionShowOptions,
) (core.AdmissionDetail, error) {
	p.record("get")
	if p.get != nil {
		return p.get(ctx, candidateID, options)
	}
	return core.AdmissionDetail{}, nil
}

// admissionWriteActorDeps 是"配置齐备 + 工作区存在"的最小写命令环境。
// admissionWriteActorDeps is the minimal environment for a write command.
func admissionWriteActorDeps(t *testing.T, actor core.Actor, port AdmissionPort,
	lockCalls *int,
) Deps {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, workspaceDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, configFileName), []byte(dsnConfig), 0o644))
	return Deps{
		WorkDir:   dir,
		Logger:    quietLogger(),
		Admission: port,
		CLIWriteActor: func() core.Actor {
			return actor
		},
		AcquireWorkspaceLock: func(string) (func() error, error) {
			*lockCalls++
			return func() error { return nil }, nil
		},
	}
}

// executeAdmission 执行 `basalt admission <args...>` 并返回合并输出与错误。
// executeAdmission runs the admission group and returns combined output.
func executeAdmission(t *testing.T, deps Deps, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand(deps)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"admission"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// trustedActor 是信任档的本机 CLI 主体。
// trustedActor is a trusted local CLI identity.
func trustedActor() core.Actor {
	return core.Actor{Subject: "cli:alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI}
}

// L1：不可信主体在取锁与进入端口之前被拒——端口即开库入口，未调用即未开库。
func TestUS21_CLI_UntrustedActorRejected(t *testing.T) {
	for _, actor := range []core.Actor{
		{Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "cli:", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "http:alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "cli:alice", Method: "http", Source: core.AuthMethodCLI},
		{Subject: "cli:alice", Method: core.AuthMethodCLI, Source: "http"},
	} {
		port := &fakeAdmissionPort{}
		lockCalls := 0
		deps := admissionWriteActorDeps(t, actor, port, &lockCalls)
		_, err := executeAdmission(t, deps, "evaluate", "adm_01")
		require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor, "actor=%+v", actor)
		require.Zero(t, lockCalls, "不可信主体不得取实例锁")
		require.Empty(t, port.calls, "不可信主体不得进入端口（端口即开库入口）")
	}

	// 端口缺失但主体可信：同一个写命令报缺件，而不是绕过去自造路径。
	lockCalls := 0
	deps := admissionWriteActorDeps(t, trustedActor(), nil, &lockCalls)
	_, err := executeAdmission(t, deps, "evaluate", "adm_01")
	require.ErrorContains(t, err, "准入端口未注入")
	require.Zero(t, lockCalls)
}

// L2：写命令在端口调用期间持有实例锁，退出后释放。
func TestUS21_CLI_WriteCommandsTakeLock(t *testing.T) {
	port := &fakeAdmissionPort{}
	lockCalls := 0
	locked, released := false, false
	deps := admissionWriteActorDeps(t, trustedActor(), port, &lockCalls)
	lockedAtCall := false
	port.evaluate = func(_ context.Context, actor core.Actor, candidateID string,
		_ core.AdmissionPolicyConfig,
	) (core.AdmissionEvaluation, error) {
		lockedAtCall = locked
		require.Equal(t, "cli:alice", actor.Subject)
		require.Equal(t, "adm_01", candidateID)
		return core.AdmissionEvaluation{EvaluationID: "eval-1", CandidateID: candidateID}, nil
	}
	base := deps.AcquireWorkspaceLock
	deps.AcquireWorkspaceLock = func(root string) (func() error, error) {
		release, err := base(root)
		if err != nil {
			return nil, err
		}
		locked = true
		return func() error {
			released = true
			return release()
		}, nil
	}
	out, err := executeAdmission(t, deps, "evaluate", "adm_01", "--json")
	require.NoError(t, err)
	require.Equal(t, 1, lockCalls)
	require.True(t, lockedAtCall, "端口必须在实例锁内被调用")
	require.True(t, released, "命令结束必须释放实例锁")
	require.Contains(t, out, `"evaluation_id": "eval-1"`)
}

// L3：list / show 不取实例锁、不校验身份（服务运行中可读）。
func TestUS21_CLI_ReadCommandsNoLock(t *testing.T) {
	port := &fakeAdmissionPort{}
	lockCalls := 0
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, configFileName), []byte(dsnConfig), 0o644))
	deps := Deps{
		WorkDir:   dir,
		Logger:    quietLogger(),
		Admission: port,
		AcquireWorkspaceLock: func(string) (func() error, error) {
			lockCalls++
			return func() error { return nil }, nil
		},
	}
	port.list = func(context.Context, core.AdmissionFilter) ([]core.AdmissionCandidate, error) {
		return nil, nil
	}
	port.get = func(context.Context, string, core.AdmissionShowOptions) (core.AdmissionDetail, error) {
		return core.AdmissionDetail{Candidate: core.AdmissionCandidate{
			CandidateID: "adm_01", SkillName: "demo", State: core.AdmissionProposed, Revision: 1,
		}}, nil
	}

	_, err := executeAdmission(t, deps, "list")
	require.NoError(t, err)
	_, err = executeAdmission(t, deps, "show", "adm_01")
	require.NoError(t, err)
	require.Zero(t, lockCalls, "只读命令不得取实例锁")
	require.Equal(t, []string{"list", "get"}, port.calls)
}

// L3b：准入命令组**没有**任何身份旁路参数——批准者只能来自 cliWriteActor。
// 逆改点：给任一子命令加 `--approver` / `--subject` 之类的 flag → 本用例变红。
// L3b: no identity bypass flag exists anywhere in the admission command group.
func TestUS21_CLI_NoApproverFlag(t *testing.T) {
	group := newAdmissionCommand(Deps{Logger: quietLogger()})
	banned := []string{"approver", "actor", "subject", "auth-method", "source"}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, name := range banned {
			require.Nil(t, cmd.Flags().Lookup(name),
				"命令 %q 暴露了身份旁路参数 --%s；批准者只能来自 cliWriteActor", cmd.Name(), name)
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(group)
	require.NotNil(t, group.Commands(), "命令组必须挂载子命令")
}

// L4：basalt.yaml 缺失时写命令在取锁之前失败（YAML 先于数据库打开）。
func TestUS21_CLI_WriteRequiresConfigBeforeLock(t *testing.T) {
	port := &fakeAdmissionPort{}
	lockCalls := 0
	deps := Deps{
		WorkDir:   t.TempDir(),
		Logger:    quietLogger(),
		Admission: port,
		CLIWriteActor: func() core.Actor {
			return trustedActor()
		},
		AcquireWorkspaceLock: func(string) (func() error, error) {
			lockCalls++
			return func() error { return nil }, nil
		},
	}
	for _, args := range [][]string{
		{"evaluate", "adm_01"},
		{"approve", "adm_01", "--decision-file", "d.json"},
		{"publish", "adm_01"},
	} {
		_, err := executeAdmission(t, deps, args...)
		require.ErrorContains(t, err, configFileName, "args=%v", args)
		require.Zero(t, lockCalls, "args=%v", args)
		require.Empty(t, port.calls, "args=%v", args)
	}
}

// L5：人类输出不回显候选正文、来源正文与决策理由全文；--files 与 --json 是显式出口。
func TestUS21_CLI_ShowKeepsBodiesOffHumanOutput(t *testing.T) {
	const (
		manifestMarker = "MANIFEST-BODY-MARKER"
		sourceMarker   = "SOURCE-BODY-MARKER"
		baselineMarker = "BASELINE-BODY-MARKER"
		reasonMarker   = "DECISION-REASON-MARKER"
		fileMarker     = "PRIVATE-FILE-BODY-MARKER"
	)
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	detail := core.AdmissionDetail{
		Candidate: core.AdmissionCandidate{
			CandidateID:   "adm_01",
			SkillName:     "demo",
			State:         core.AdmissionEvaluated,
			Revision:      2,
			CandidateHash: "sha256:candidate",
			SourceHash:    "sha256:source",
			ManifestJSON:  manifestMarker,
			SourceJSON:    sourceMarker,
			BaselineJSON:  baselineMarker,
			RiskTier:      core.AdmissionRiskNormal,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		Source: core.AdmissionSource{Kind: "manual", Ref: "会议纪要"},
		Evaluation: &core.AdmissionEvaluation{
			EvaluationID: "eval-01",
			Aggregate:    core.AdmissionAggregateIncomplete,
			RiskTier:     core.AdmissionRiskNormal,
			Dimensions: []core.AdmissionDimension{{
				Name: core.AdmissionDimensionContent, Status: core.AdmissionDimensionPassed,
			}},
			BaselineJSON: baselineMarker,
			EvaluatedAt:  now,
		},
		Decision: &core.AdmissionDecision{
			DecisionID:      "dec-01",
			Decision:        core.AdmissionApprove,
			Reason:          reasonMarker,
			ApproverSubject: "cli:alice",
			ExpiresAt:       now.Add(time.Hour),
		},
		Files: []core.AuthorFile{{Path: "SKILL.md", Content: fileMarker}},
	}
	port := &fakeAdmissionPort{get: func(context.Context, string,
		core.AdmissionShowOptions,
	) (core.AdmissionDetail, error) {
		return detail, nil
	}}
	deps := Deps{WorkDir: t.TempDir(), Logger: quietLogger(), Admission: port}
	require.NoError(t, os.WriteFile(filepath.Join(deps.WorkDir, configFileName), []byte(dsnConfig), 0o644))

	human, err := executeAdmission(t, deps, "show", "adm_01", "--files")
	require.NoError(t, err)
	require.NotContains(t, human, reasonMarker, "人类输出不得回显决策理由全文")
	require.Contains(t, human, fileMarker, "--files 是本机 CLI 的正文出口")

	raw, err := executeAdmission(t, deps, "show", "adm_01", "--json")
	require.NoError(t, err)
	require.Contains(t, raw, reasonMarker, "--json 是机器可读出口")
	for _, marker := range []string{manifestMarker, sourceMarker} {
		require.NotContains(t, raw, marker, "候选 / 来源原文不进 CLI 输出（基线是绑定摘要，保留）")
	}
	// baseline_json 是评估 / 决策 / 发布共同绑定的规范化摘要，机器消费者必须能核对，
	// 因此**保留**在 JSON 里；它不含文件正文或来源正文。
	// The canonical baseline stays in the machine output: consumers must be able to
	// verify the binding, and it carries neither file bodies nor the source body.
	require.Contains(t, raw, baselineMarker)
	var view map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &view), "输出必须是合法 JSON")
	require.Contains(t, view, "candidate")
	require.Contains(t, view, "source")
}

// L5b：被拒候选的终态原因就是决策理由，人类输出同样不得回显全文（--json 可以）。
// L5b: a rejected candidate's terminal reason is the decision reason, so the human
// output must not echo it either; --json remains the machine outlet.
func TestUS21_CLI_ShowHidesRejectTerminalReason(t *testing.T) {
	const reasonMarker = "REJECT-TERMINAL-REASON-MARKER"
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	detail := core.AdmissionDetail{
		Candidate: core.AdmissionCandidate{
			CandidateID:    "adm_02",
			SkillName:      "demo",
			State:          core.AdmissionRejected,
			Revision:       2,
			RiskTier:       core.AdmissionRiskNormal,
			TerminalReason: reasonMarker,
			CreatedAt:      now,
			UpdatedAt:      now,
		},
		Source: core.AdmissionSource{Kind: "manual", Ref: "会议纪要"},
	}
	port := &fakeAdmissionPort{get: func(context.Context, string,
		core.AdmissionShowOptions,
	) (core.AdmissionDetail, error) {
		return detail, nil
	}}
	deps := Deps{WorkDir: t.TempDir(), Logger: quietLogger(), Admission: port}
	require.NoError(t, os.WriteFile(filepath.Join(deps.WorkDir, configFileName), []byte(dsnConfig), 0o644))

	human, err := executeAdmission(t, deps, "show", "adm_02")
	require.NoError(t, err)
	require.NotContains(t, human, reasonMarker, "拒绝决策理由不得在人类输出中回显")
	require.Contains(t, human, "终态原因：", "仍要提示有终态原因，只是不回显全文")

	raw, err := executeAdmission(t, deps, "show", "adm_02", "--json")
	require.NoError(t, err)
	require.Contains(t, raw, reasonMarker, "--json 是机器可读出口")
}

// L6：端口缺失报缺件（读命令同样不许自造第二条读面）。
func TestUS21_CLI_MissingPortIsWiringGap(t *testing.T) {
	deps := Deps{WorkDir: t.TempDir(), Logger: quietLogger()}
	require.NoError(t, os.WriteFile(filepath.Join(deps.WorkDir, configFileName), []byte(dsnConfig), 0o644))
	for _, args := range [][]string{{"list"}, {"show", "adm_01"}, {"propose", "--name", "x", "--from-dir", ".", "--source-file", "s.json"}} {
		_, err := executeAdmission(t, deps, args...)
		require.ErrorContains(t, err, "准入端口未注入", "args=%v", args)
	}
}

// L7：approve / reject / cancel / publish 与 propose / evaluate 同走写外壳——
// 不可信主体零取锁、零进端口（端口即开库入口）。
// 逆改点：任一命令绕过 withAdmissionWrite 直接调端口 → 本用例变红。
// L7: every write command rejects an untrusted actor before the lock and the port.
func TestUS21_CLI_WriteCommandsRejectUntrustedActor(t *testing.T) {
	for _, args := range [][]string{
		{"propose", "--name", "x", "--from-dir", ".", "--source-file", "s.json"},
		{"evaluate", "adm_01"},
		{"approve", "adm_01", "--decision-file", "d.json"},
		{"reject", "adm_01", "--decision-file", "d.json"},
		{"cancel", "adm_01", "--reason", "过期"},
		{"publish", "adm_01"},
		{"withdraw", "adm_01", "--reason", "回退到上一版"},
	} {
		port := &fakeAdmissionPort{}
		lockCalls := 0
		deps := admissionWriteActorDeps(t,
			core.Actor{Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
			port, &lockCalls)
		_, err := executeAdmission(t, deps, args...)
		require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor, "args=%v", args)
		require.Zero(t, lockCalls, "不可信主体不得取实例锁：args=%v", args)
		require.Empty(t, port.calls, "不可信主体不得进入端口：args=%v", args)
	}
}

// L8：approve 把 --decision-file 的字节原样交给端口，且在实例锁内取策略输入；
// publish 同样带策略输入。决策文件缺失在取锁与端口调用之前失败。
// 逆改点：命令面自己解析 / 改写决策 JSON、跳过策略快照，或先取锁后校验参数 → 红。
// L8: approve hands the decision bytes to the port unchanged and publishes take the
// policy input; a missing --decision-file fails before the lock.
func TestUS21_CLI_ApprovePublishPlumbing(t *testing.T) {
	const decisionMarker = `{"decision":"approve","candidate_id":"adm_01"}`
	port := &fakeAdmissionPort{}
	lockCalls := 0
	deps := admissionWriteActorDeps(t, trustedActor(), port, &lockCalls)
	locked := false
	base := deps.AcquireWorkspaceLock
	deps.AcquireWorkspaceLock = func(root string) (func() error, error) {
		release, err := base(root)
		if err != nil {
			return nil, err
		}
		locked = true
		return func() error {
			locked = false
			return release()
		}, nil
	}
	port.readDecide = func(path string) ([]byte, error) {
		require.Equal(t, "decision.json", path, "命令面必须把 --decision-file 原样交给读取器")
		return []byte(decisionMarker), nil
	}
	port.approve = func(_ context.Context, actor core.Actor, candidateID string, decisionJSON []byte,
		_ core.AdmissionPolicyConfig,
	) (core.AdmissionDecision, error) {
		require.True(t, locked, "批准必须在实例锁内执行")
		require.Equal(t, "cli:alice", actor.Subject)
		require.Equal(t, "adm_01", candidateID)
		require.Equal(t, decisionMarker, string(decisionJSON), "决策字节不得被命令面改写")
		return core.AdmissionDecision{
			DecisionID: "dec-01", CandidateID: candidateID, Decision: core.AdmissionApprove,
			ApproverSubject: actor.Subject, CandidateRevision: 2,
		}, nil
	}
	out, err := executeAdmission(t, deps, "approve", "adm_01", "--decision-file", "decision.json")
	require.NoError(t, err)
	require.Contains(t, out, "dec-01")
	require.False(t, locked, "命令结束必须释放实例锁")

	port.publish = func(_ context.Context, actor core.Actor, candidateID string,
		_ core.AdmissionPolicyConfig,
	) (core.AdmissionPublication, error) {
		require.True(t, locked, "发布必须在实例锁内执行")
		return core.AdmissionPublication{SkillName: "weekly-report", VersionNo: 3}, nil
	}
	out, err = executeAdmission(t, deps, "publish", "adm_01")
	require.NoError(t, err)
	require.Contains(t, out, "weekly-report")

	// 决策文件缺失：参数校验先于取锁与端口调用。
	before := lockCalls
	_, err = executeAdmission(t, deps, "approve", "adm_01")
	require.ErrorContains(t, err, "decision-file")
	require.Equal(t, before, lockCalls)

	// 端口错误原样上抛（不吞、不改写）。
	port.publish = func(context.Context, core.Actor, string,
		core.AdmissionPolicyConfig,
	) (core.AdmissionPublication, error) {
		return core.AdmissionPublication{}, core.ErrAdmissionInvalidState
	}
	_, err = executeAdmission(t, deps, "publish", "adm_01")
	require.ErrorIs(t, err, core.ErrAdmissionInvalidState)
}

// L9：withdraw 与发布同一写外壳——理由必填、空理由在取锁与端口调用之前失败；
// 候选号与理由原样交给端口，输出携带撤回分类；端口错误原样上抛。
// 逆改点：命令面跳过 `--reason` 校验、改写撤回理由或吞端口错误 → 本用例变红。
// L9: withdraw shares the write shell; an empty reason fails before the lock, and the
// candidate id plus reason reach the port unchanged.
func TestUS21_CLI_WithdrawPlumbing(t *testing.T) {
	port := &fakeAdmissionPort{}
	lockCalls := 0
	deps := admissionWriteActorDeps(t, trustedActor(), port, &lockCalls)
	locked := false
	base := deps.AcquireWorkspaceLock
	deps.AcquireWorkspaceLock = func(root string) (func() error, error) {
		release, err := base(root)
		if err != nil {
			return nil, err
		}
		locked = true
		return func() error {
			locked = false
			return release()
		}, nil
	}

	// 空理由：参数校验先于取锁与端口调用。
	_, err := executeAdmission(t, deps, "withdraw", "adm_01")
	require.ErrorContains(t, err, "--reason")
	_, err = executeAdmission(t, deps, "withdraw", "adm_01", "--reason", "   ")
	require.ErrorContains(t, err, "--reason")
	require.Zero(t, lockCalls)
	require.Empty(t, port.calls)

	port.withdraw = func(_ context.Context, actor core.Actor, candidateID, reason string,
	) (core.AdmissionPublication, error) {
		require.True(t, locked, "撤回必须在实例锁内执行")
		require.Equal(t, "cli:alice", actor.Subject)
		require.Equal(t, "adm_01", candidateID)
		require.Equal(t, "回退到上一版", reason, "撤回理由不得被命令面改写")
		return core.AdmissionPublication{
			CandidateID:         candidateID,
			SkillName:           "weekly-report",
			VersionNo:           2,
			State:               core.AdmissionPublicationWithdrawn,
			WithdrawReason:      core.AdmissionWithdrawRestoredPrevious,
			WithdrawVersionID:   3,
			WithdrawOperationID: "op_withdraw",
		}, nil
	}
	out, err := executeAdmission(t, deps, "withdraw", "adm_01", "--reason", "回退到上一版")
	require.NoError(t, err)
	require.Contains(t, out, "weekly-report")
	require.Contains(t, out, core.AdmissionWithdrawRestoredPrevious)
	require.False(t, locked, "命令结束必须释放实例锁")

	raw, err := executeAdmission(t, deps, "withdraw", "adm_01", "--reason", "回退到上一版", "--json")
	require.NoError(t, err)
	var view map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &view), "输出必须是合法 JSON")
	require.Equal(t, string(core.AdmissionPublicationWithdrawn), view["state"])
	require.Equal(t, core.AdmissionWithdrawRestoredPrevious, view["withdraw_reason"])

	// 端口错误原样上抛（含首版撤回到空的哨兵）。
	port.withdraw = func(context.Context, core.Actor, string, string,
	) (core.AdmissionPublication, error) {
		return core.AdmissionPublication{}, core.ErrAdmissionFirstVersionWithdraw
	}
	_, err = executeAdmission(t, deps, "withdraw", "adm_01", "--reason", "回退")
	require.ErrorIs(t, err, core.ErrAdmissionFirstVersionWithdraw)
}
