package cli

// 本文件是受控自我进化命令面的验收面（颗粒度文档 §3.1.10、T023 / T027）：
//
//	L1 `TestUS22_CLI_Status`——status 读取启用 / 名单 / 预算 / 待决计数；
//	   只读、零取锁、零身份校验
//	L2 `TestUS22_CLI_ListShow`——extractions / drafts 的 list 与 show；
//	   过滤条件原样传端口，正文只在 drafts show 一个出口
//	L3 `TestUS22_CLI_HandoffTakesLock`——handoff 经**持有实例锁的端口**执行：
//	   命令面只调用一次 Handoff、自己不取锁（独占锁不可重入），
//	   锁被占用的错误原样上抛且不重试
//	L4 `TestUS22_CLI_HandoffUntrustedActor`——cli:unknown 在读文件与调用端口之前被拒
//	L5 `TestUS22_CLI_HandoffStrictSource`——来源声明与文件清单原样转交，
//	   严格解码归适配器；读取失败不调用端口
//	L6 `TestUS22_CLI_HandoffCallsPropose`——handoff 只触发 Handoff（适配器内部只调
//	   提议入口）；命令面从不自己调用 evaluate / approve / publish，输出不含草案正文
//
// 判据只看命令面的行为：端口是记录调用的替身，锁是计数闭包——不需要数据库。
// The command surface's acceptance: recording fakes and a counting lock closure.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeLearningPort 记录调用顺序；未注入的方法返回零值。
// fakeLearningPort records call order; unset methods return zero values.
type fakeLearningPort struct {
	calls []string

	status          func(context.Context) (core.LearningStatus, error)
	listExtractions func(context.Context, core.LearningExtractionFilter) ([]core.LearningExtractionView, error)
	loadExtraction  func(context.Context, string) (core.LearningExtractionView, error)
	listDrafts      func(context.Context, core.LearningDraftFilter) ([]core.LearningDraftView, error)
	loadDraft       func(context.Context, string) (core.LearningDraftView, error)
	handoff         func(context.Context, core.LearningHandoffRequest) (core.LearningHandoffResult, error)
}

func (p *fakeLearningPort) record(name string) { p.calls = append(p.calls, name) }

func (p *fakeLearningPort) Status(ctx context.Context) (core.LearningStatus, error) {
	p.record("status")
	if p.status != nil {
		return p.status(ctx)
	}
	return core.LearningStatus{}, nil
}

func (p *fakeLearningPort) ListExtractions(ctx context.Context,
	filter core.LearningExtractionFilter,
) ([]core.LearningExtractionView, error) {
	p.record("list-extractions")
	if p.listExtractions != nil {
		return p.listExtractions(ctx, filter)
	}
	return nil, nil
}

func (p *fakeLearningPort) LoadExtraction(ctx context.Context,
	extractionID string,
) (core.LearningExtractionView, error) {
	p.record("load-extraction")
	if p.loadExtraction != nil {
		return p.loadExtraction(ctx, extractionID)
	}
	return core.LearningExtractionView{}, nil
}

func (p *fakeLearningPort) ListDrafts(ctx context.Context,
	filter core.LearningDraftFilter,
) ([]core.LearningDraftView, error) {
	p.record("list-drafts")
	if p.listDrafts != nil {
		return p.listDrafts(ctx, filter)
	}
	return nil, nil
}

func (p *fakeLearningPort) LoadDraft(ctx context.Context, draftID string) (core.LearningDraftView, error) {
	p.record("load-draft")
	if p.loadDraft != nil {
		return p.loadDraft(ctx, draftID)
	}
	return core.LearningDraftView{}, nil
}

func (p *fakeLearningPort) Handoff(ctx context.Context,
	req core.LearningHandoffRequest,
) (core.LearningHandoffResult, error) {
	p.record("handoff")
	if p.handoff != nil {
		return p.handoff(ctx, req)
	}
	return core.LearningHandoffResult{}, nil
}

// executeLearning 执行 `basalt learning <args...>` 并返回合并输出与错误。
// executeLearning runs the learning group and returns combined output.
func executeLearning(t *testing.T, deps Deps, args ...string) (string, error) {
	t.Helper()
	root := NewRootCommand(deps)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"learning"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// learningReadDeps 是只读命令的最小环境：不需要配置文件，也不需要工作区。
// learningReadDeps is the minimal read-only environment: no config, no workspace.
func learningReadDeps(t *testing.T, port LearningPort, lockCalls, actorCalls *int) Deps {
	t.Helper()
	return Deps{
		WorkDir:  t.TempDir(),
		Logger:   quietLogger(),
		Learning: port,
		CLIWriteActor: func() core.Actor {
			*actorCalls++
			return trustedActor()
		},
		AcquireWorkspaceLock: func(string) (func() error, error) {
			*lockCalls++
			return func() error { return nil }, nil
		},
	}
}

// L1：status 只读、零取锁、零身份校验，机器投影与 §3.1.10 字段同源。
func TestUS22_CLI_Status(t *testing.T) {
	port := &fakeLearningPort{
		status: func(context.Context) (core.LearningStatus, error) {
			return core.LearningStatus{
				Enabled:            true,
				Agents:             []string{"demo", "ops"},
				UnknownAgents:      []string{"ghost"},
				RuleVersion:        "learning-v1",
				PromptVersion:      "memory-extract-v1",
				AttemptsUsed:       3,
				AttemptLimit:       200,
				BudgetWindow:       24 * time.Hour,
				PendingExtractions: 1,
				UnknownExtractions: 2,
				OrphanDrafts:       3,
				LastExtractionAt:   time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
			}, nil
		},
	}
	lockCalls, actorCalls := 0, 0
	deps := learningReadDeps(t, port, &lockCalls, &actorCalls)

	out, err := executeLearning(t, deps, "status", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"attempts_used": 3`)
	require.Contains(t, out, `"attempt_limit": 200`)
	require.Contains(t, out, `"budget_window_seconds": 86400`)
	require.Contains(t, out, `"unknown_agents": [`)
	require.Contains(t, out, `"ghost"`)
	require.Contains(t, out, `"last_extraction_at": "2026-11-06T10:00:00Z"`)

	out, err = executeLearning(t, deps, "status")
	require.NoError(t, err)
	require.Contains(t, out, "学习抽取：已启用")
	require.Contains(t, out, "名单内未注册（不抽取）：ghost")
	require.Contains(t, out, "提取额度：3/200")
	require.Contains(t, out, "未耗尽")
	require.Contains(t, out, "pending=1 unknown=2 孤儿草案=3")

	require.Equal(t, []string{"status", "status"}, port.calls)
	require.Zero(t, lockCalls, "只读命令不得取实例锁")
	require.Zero(t, actorCalls, "只读命令不得校验写身份")
}

// L1b：未启用时状态页有明确文案；空列表渲染成 [] 而不是 null。
func TestUS22_CLI_StatusDisabled(t *testing.T) {
	port := &fakeLearningPort{}
	lockCalls, actorCalls := 0, 0
	deps := learningReadDeps(t, port, &lockCalls, &actorCalls)

	out, err := executeLearning(t, deps, "status", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"enabled": false`)
	require.Contains(t, out, `"agents": []`)

	out, err = executeLearning(t, deps, "status")
	require.NoError(t, err)
	require.Contains(t, out, "学习抽取：未启用")
	require.Contains(t, out, "最近抽取：无")
}

// L2：过滤条件原样传端口；正文只在 drafts show 一个出口。
func TestUS22_CLI_ListShow(t *testing.T) {
	const body = "DRAFT BODY 只经 drafts show"
	port := &fakeLearningPort{
		listExtractions: func(_ context.Context,
			filter core.LearningExtractionFilter,
		) ([]core.LearningExtractionView, error) {
			require.Equal(t, "demo", filter.Agent)
			require.Equal(t, "failed", filter.State)
			require.Equal(t, 7, filter.Limit)
			return []core.LearningExtractionView{{
				Extraction: core.LearningExtraction{
					ExtractionID: "lrn_01", Agent: "demo",
					Status:       core.LearningExtractionFailed,
					FailureClass: core.LearningFailureModel, Attempts: 2,
					CreatedAt: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC),
				},
			}}, nil
		},
		loadExtraction: func(_ context.Context, extractionID string) (core.LearningExtractionView, error) {
			require.Equal(t, "lrn_01", extractionID)
			return core.LearningExtractionView{
				Extraction: core.LearningExtraction{
					ExtractionID: "lrn_01", Agent: "demo", Status: core.LearningExtractionSuccess,
				},
				Candidates: []core.LearningCandidateView{{EntryID: 42, Kind: "fact", State: "candidate"}},
				Drafts:     []core.LearningDraftView{{DraftID: "dft_01", Name: "weekly-report"}},
			}, nil
		},
		listDrafts: func(_ context.Context, filter core.LearningDraftFilter) ([]core.LearningDraftView, error) {
			require.Equal(t, "demo", filter.Agent)
			require.Equal(t, 0, filter.Limit)
			return []core.LearningDraftView{{
				DraftID: "dft_01", Name: "weekly-report", Agent: "demo",
				Content: body, CreatedAt: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC),
			}}, nil
		},
		loadDraft: func(_ context.Context, draftID string) (core.LearningDraftView, error) {
			require.Equal(t, "dft_01", draftID)
			return core.LearningDraftView{
				DraftID: "dft_01", Name: "weekly-report", Agent: "demo",
				ExtractionID: "lrn_01", Content: body, ContentSHA256: "abc123",
				RelDir: "learning/drafts/dft_01",
				Evidence: []core.LearningEvidence{{
					MessageID: 7, RunID: "run_01", Role: "user", Quote: "每周五交周报",
				}},
			}, nil
		},
	}
	lockCalls, actorCalls := 0, 0
	deps := learningReadDeps(t, port, &lockCalls, &actorCalls)

	out, err := executeLearning(t, deps, "extractions", "list",
		"--agent", "demo", "--state", "failed", "--limit", "7")
	require.NoError(t, err)
	require.Contains(t, out, "lrn_01")
	require.Contains(t, out, "failure=model_error")

	out, err = executeLearning(t, deps, "extractions", "show", "lrn_01")
	require.NoError(t, err)
	require.Contains(t, out, "候选 #42")
	require.Contains(t, out, "草案 dft_01")

	out, err = executeLearning(t, deps, "drafts", "list", "--agent", "demo")
	require.NoError(t, err)
	require.Contains(t, out, "dft_01")
	require.NotContains(t, out, body, "草案正文不得出现在列表输出里")

	out, err = executeLearning(t, deps, "drafts", "show", "dft_01", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"content": "DRAFT BODY 只经 drafts show"`)
	require.Contains(t, out, `"message_id": 7`)

	require.Equal(t, []string{
		"list-extractions", "load-extraction", "list-drafts", "load-draft",
	}, port.calls)
	require.Zero(t, lockCalls, "只读命令不得取实例锁")
	require.Zero(t, actorCalls, "只读命令不得校验写身份")
}

// L2b：非法过滤值在进入端口之前被拒（不把拼错的状态当成"没有记录"）。
func TestUS22_CLI_ListRejectsBadFilters(t *testing.T) {
	port := &fakeLearningPort{}
	lockCalls, actorCalls := 0, 0
	deps := learningReadDeps(t, port, &lockCalls, &actorCalls)

	_, err := executeLearning(t, deps, "extractions", "list", "--state", "sucess")
	require.ErrorContains(t, err, "--state 取值非法")
	_, err = executeLearning(t, deps, "extractions", "list", "--limit", "500")
	require.ErrorContains(t, err, "--limit 取值非法")
	// 显式 0 与帮助 / HTTP 面的「1–200」一致地拒绝；省略才走缺省 50。
	_, err = executeLearning(t, deps, "extractions", "list", "--limit", "0")
	require.ErrorContains(t, err, "--limit 取值非法")
	_, err = executeLearning(t, deps, "drafts", "list", "--limit", "0")
	require.ErrorContains(t, err, "--limit 取值非法")
	require.Empty(t, port.calls, "参数校验失败不得进入端口")
}

// L3：handoff 经持有实例锁的端口执行——命令面只调用一次 Handoff，自己不取锁。
// 真实取锁行为由 app 的 `TestUS22_Handoff_TakesLock` 举证（端口适配器持锁）；
// 本用例守"命令面不重复取锁"与"锁错误原样上抛、不重试"。
// 逆改点：命令面自己调用 AcquireWorkspaceLock / 失败后重试 Handoff → 本用例变红。
func TestUS22_CLI_HandoffTakesLock(t *testing.T) {
	busyErr := errors.New("workspace: another instance holds the lock")
	port := &fakeLearningPort{}
	handoffCalls := 0
	port.handoff = func(_ context.Context, req core.LearningHandoffRequest) (core.LearningHandoffResult, error) {
		handoffCalls++
		require.Equal(t, "cli:alice", req.Actor.Subject, "端口在实例锁内收到可信主体")
		require.Equal(t, "dft_01", req.DraftID)
		if handoffCalls == 1 {
			return core.LearningHandoffResult{}, busyErr
		}
		return core.LearningHandoffResult{}, nil
	}
	lockCalls := 0
	reader := &fakeAdmissionPort{
		importFiles: func(string) ([]core.AuthorFile, error) {
			return []core.AuthorFile{{Path: "SKILL.md", Content: "---\nname: weekly-report\n---\n"}}, nil
		},
		readSource: func(string) ([]byte, error) { return []byte(`{"confirmation":"user_confirmed"}`), nil },
	}
	deps := learningWriteDeps(t, trustedActor(), port, reader, &lockCalls)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "source.json"),
		[]byte(`{"confirmation":"user_confirmed"}`), 0o644))

	_, err := executeLearning(t, deps, "handoff", "dft_01",
		"--name", "weekly-report", "--from-dir", dir, "--source-file", filepath.Join(dir, "source.json"))
	require.ErrorIs(t, err, busyErr, "锁被占用必须原样上抛")
	require.Equal(t, 1, handoffCalls, "失败后不得重试：端口已持有锁，重试只会重复开库")
	require.Zero(t, lockCalls, "命令面不得再取一次实例锁（独占锁不可重入）")
	require.Equal(t, []string{"import", "source"}, reader.calls)
}

// L4：不可信主体在读取任何文件之前被拒。
func TestUS22_CLI_HandoffUntrustedActor(t *testing.T) {
	for _, actor := range []core.Actor{
		{Subject: "cli:unknown", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "cli:", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
		{Subject: "http:alice", Method: core.AuthMethodCLI, Source: core.AuthMethodCLI},
	} {
		port := &fakeLearningPort{}
		reader := &fakeAdmissionPort{}
		lockCalls := 0
		deps := learningWriteDeps(t, actor, port, reader, &lockCalls)
		_, err := executeLearning(t, deps, "handoff", "dft_01",
			"--name", "weekly-report", "--from-dir", "unused", "--source-file", "unused.json")
		require.ErrorIs(t, err, core.ErrAdmissionUntrustedActor, "actor=%+v", actor)
		require.Empty(t, port.calls, "不可信主体不得进入端口（端口即开库入口）")
		require.Empty(t, reader.calls, "不可信主体不得读取任何文件")
		require.Zero(t, lockCalls)
	}
}

// L5：来源声明与文件清单原样转交（严格解码归适配器）；读取失败不调用端口。
// 逆改点：命令面自己 json.Unmarshal 来源声明 / 丢字节日志化 → 断言变红。
func TestUS22_CLI_HandoffStrictSource(t *testing.T) {
	sourceBytes := []byte("{not json — 严格解码是适配器的职责")
	files := []core.AuthorFile{{Path: "SKILL.md", Content: "body"}}
	port := &fakeLearningPort{}
	port.handoff = func(_ context.Context, req core.LearningHandoffRequest) (core.LearningHandoffResult, error) {
		require.Equal(t, sourceBytes, req.SourceJSON, "来源声明必须逐字节原样转交")
		require.Equal(t, files, req.Files)
		require.Equal(t, "weekly-report", req.SkillName)
		return core.LearningHandoffResult{}, errors.New("来源声明不是合法 JSON")
	}
	reader := &fakeAdmissionPort{
		importFiles: func(string) ([]core.AuthorFile, error) { return files, nil },
		readSource:  func(string) ([]byte, error) { return sourceBytes, nil },
	}
	lockCalls := 0
	deps := learningWriteDeps(t, trustedActor(), port, reader, &lockCalls)

	_, err := executeLearning(t, deps, "handoff", "dft_01",
		"--name", "weekly-report", "--from-dir", "skills", "--source-file", "source.json")
	require.ErrorContains(t, err, "来源声明不是合法 JSON", "适配器错误原样上抛")
	require.Equal(t, []string{"handoff"}, port.calls)

	// 读取失败：不调用端口（端口即开库入口）。
	port = &fakeLearningPort{}
	reader = &fakeAdmissionPort{
		readSource: func(string) ([]byte, error) { return nil, errors.New("来源声明超过 64 KiB") },
	}
	deps = learningWriteDeps(t, trustedActor(), port, reader, &lockCalls)
	_, err = executeLearning(t, deps, "handoff", "dft_01",
		"--name", "weekly-report", "--from-dir", "skills", "--source-file", "source.json")
	require.ErrorContains(t, err, "来源声明超过 64 KiB")
	require.Empty(t, port.calls, "读取失败不得进入端口")
}

// L6：handoff 只触发 Handoff（适配器内部只调提议入口）；命令面从不自己调用
// evaluate / approve / publish，输出只含草案元数据与提议回执。
// 逆改点：命令面直接调用 AdmissionPort.Propose 或把草案正文打印出来 → 断言变红。
func TestUS22_CLI_HandoffCallsPropose(t *testing.T) {
	const body = "DRAFT BODY 不得出现在转交输出里"
	result := core.LearningHandoffResult{
		Draft: core.LearningDraftView{
			DraftID: "dft_01", ExtractionID: "lrn_01", Agent: "demo", Name: "weekly-report",
			Content: body, ContentSHA256: "sha256-abc", RelDir: "learning/drafts/dft_01",
		},
		Candidate: core.AdmissionCandidate{
			CandidateID: "adm_01", SkillName: "weekly-report",
			State: core.AdmissionProposed, Revision: 1,
		},
	}
	port := &fakeLearningPort{}
	port.handoff = func(context.Context, core.LearningHandoffRequest) (core.LearningHandoffResult, error) {
		return result, nil
	}
	reader := &fakeAdmissionPort{
		importFiles: func(string) ([]core.AuthorFile, error) {
			return []core.AuthorFile{{Path: "SKILL.md", Content: body}}, nil
		},
		readSource: func(string) ([]byte, error) { return []byte("{}"), nil },
	}
	lockCalls := 0
	deps := learningWriteDeps(t, trustedActor(), port, reader, &lockCalls)

	out, err := executeLearning(t, deps, "handoff", "dft_01",
		"--name", "weekly-report", "--from-dir", "skills", "--source-file", "source.json", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"draft_id": "dft_01"`)
	require.Contains(t, out, `"candidate_id": "adm_01"`)
	require.NotContains(t, out, body, "转交输出不得回显草案正文")

	out, err = executeLearning(t, deps, "handoff", "dft_01",
		"--name", "weekly-report", "--from-dir", "skills", "--source-file", "source.json")
	require.NoError(t, err)
	require.Contains(t, out, "草案已转交：dft_01")
	require.Contains(t, out, "候选已冻结：adm_01")
	require.Contains(t, out, "basalt admission evaluate adm_01")
	require.NotContains(t, out, body)

	require.Equal(t, []string{"handoff", "handoff"}, port.calls)
	require.Equal(t, []string{"import", "source", "import", "source"}, reader.calls,
		"命令面只做有界读取，不直接调用提议入口")
	require.Zero(t, lockCalls)
}

// learningWriteDeps 是 handoff 的最小环境：两个端口 + 可信主体，无配置文件。
// learningWriteDeps is the minimal handoff environment; no config file is needed
// because the lock and the database live behind the port.
func learningWriteDeps(t *testing.T, actor core.Actor, learning LearningPort,
	admission AdmissionPort, lockCalls *int,
) Deps {
	t.Helper()
	return Deps{
		WorkDir:   t.TempDir(),
		Logger:    quietLogger(),
		Learning:  learning,
		Admission: admission,
		CLIWriteActor: func() core.Actor {
			return actor
		},
		AcquireWorkspaceLock: func(string) (func() error, error) {
			*lockCalls++
			return func() error { return nil }, nil
		},
	}
}
