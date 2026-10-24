package web

// 本文件是 `handlers_admin_agents.go` 的 handler 级验收（颗粒度文档 §3.2.1 第 1 行，
// 计划 12 例）：创建 / 草稿 / 多文件提交的响应与错误码、GET files 的集合 ETag、
// 过期提交 412 且不重试。响应形状与错误码以契约与约束 43 为锚，**不**比对实现文本。
//
// 用法说明：多文件提交的"零操作行"由适配器（app 层）保证，T021 的
// TestAgentAdmin_SubmitStaleETagNoOperation 断言零操作行与零写入；本层对应判据是
// "412 之后不重试、不自动修复、失败信封不带 data"。
// This file is the handler-level acceptance for the five agent endpoints. Zero
// operation rows on a stale submit are asserted at the adapter layer (T021); here the
// judged surface is the 412 mapping and the absence of any retry.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errFakeAgentAdminUnused 标记用例没有为该端口方法准备行为（比 panic 更好读）。
// errFakeAgentAdminUnused marks a port method the case never intended to call.
var errFakeAgentAdminUnused = errors.New("fake agent admin: unexpected call")

// fakeAgentAdminService 是可注入行为的 AgentAdminService 替身：每个方法只记录入参
// 并返回用例给定的值；未设置的方法按"意外调用"报错。
// fakeAgentAdminService is a behavior-injected AgentAdminService; an unset method
// reports an unexpected call instead of silently succeeding.
type fakeAgentAdminService struct {
	createFn   func(ctx context.Context, actor core.Actor, spec core.AgentSpec) (AgentFileSet, error)
	generateFn func(ctx context.Context, name, description string, answers []core.AuthorAnswer) (AgentDraftResult, error)
	scheduleFn func(ctx context.Context, name, instruction string) (ScheduleDraftResult, error)
	filesFn    func(ctx context.Context, name string) (AgentFileSet, error)
	submitFn   func(ctx context.Context, actor core.Actor, name, ifMatch string, files []core.AuthorFile) (AgentFileSet, error)
	copyFn     func(ctx context.Context, actor core.Actor, source, target string, mapping []core.ResourceMapping) (AgentFileSet, error)
}

// Create 委托创建的注入行为。
func (f *fakeAgentAdminService) Create(ctx context.Context, actor core.Actor, spec core.AgentSpec) (AgentFileSet, error) {
	if f.createFn == nil {
		return AgentFileSet{}, errFakeAgentAdminUnused
	}
	return f.createFn(ctx, actor, spec)
}

// GenerateFiles 委托草稿的注入行为（R6.3：答案原样透传）。
func (f *fakeAgentAdminService) GenerateFiles(ctx context.Context, name, description string,
	answers []core.AuthorAnswer,
) (AgentDraftResult, error) {
	if f.generateFn == nil {
		return AgentDraftResult{}, errFakeAgentAdminUnused
	}
	return f.generateFn(ctx, name, description, answers)
}

// ScheduleDraft 委托定时候选的注入行为（R6.4）。
func (f *fakeAgentAdminService) ScheduleDraft(ctx context.Context, name, instruction string) (ScheduleDraftResult, error) {
	if f.scheduleFn == nil {
		return ScheduleDraftResult{}, errFakeAgentAdminUnused
	}
	return f.scheduleFn(ctx, name, instruction)
}

// Files 委托文件集合读取的注入行为。
func (f *fakeAgentAdminService) Files(ctx context.Context, name string) (AgentFileSet, error) {
	if f.filesFn == nil {
		return AgentFileSet{}, errFakeAgentAdminUnused
	}
	return f.filesFn(ctx, name)
}

// SubmitFiles 委托多文件提交的注入行为。
func (f *fakeAgentAdminService) SubmitFiles(ctx context.Context, actor core.Actor, name, ifMatch string, files []core.AuthorFile) (AgentFileSet, error) {
	if f.submitFn == nil {
		return AgentFileSet{}, errFakeAgentAdminUnused
	}
	return f.submitFn(ctx, actor, name, ifMatch, files)
}

// Copy 委托复制的注入行为。
func (f *fakeAgentAdminService) Copy(ctx context.Context, actor core.Actor, source, target string,
	mapping []core.ResourceMapping,
) (AgentFileSet, error) {
	if f.copyFn == nil {
		return AgentFileSet{}, errFakeAgentAdminUnused
	}
	return f.copyFn(ctx, actor, source, target, mapping)
}

// Archive 不在本文件的验收面内（T034 覆盖归档端点）。
func (f *fakeAgentAdminService) Archive(context.Context, core.Actor, string) (core.AgentArchiveRecord, error) {
	return core.AgentArchiveRecord{}, errFakeAgentAdminUnused
}

// Restore 不在本文件的验收面内（T034 覆盖恢复端点）。
func (f *fakeAgentAdminService) Restore(context.Context, core.Actor, string, string) (AgentFileSet, error) {
	return AgentFileSet{}, errFakeAgentAdminUnused
}

// Archived 不在本文件的验收面内（T034 覆盖归档列表端点）。
func (f *fakeAgentAdminService) Archived(context.Context) ([]core.AgentArchiveRecord, error) {
	return nil, errFakeAgentAdminUnused
}

// newAgentAdminServer 装配带真实认证闸门与注入端口的测试服务器。
// newAgentAdminServer wires the real auth guard over the injected port.
func newAgentAdminServer(t *testing.T, svc AgentAdminService) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdminService = svc
	})
}

// agentAdminCall 构造一个已认证的管理写请求。
// agentAdminCall builds an authenticated admin request.
func agentAdminCall(method, path, body string) *http.Request {
	return withBearer(adminRequest(method, path, body), testToken)
}

// agentSetETag 返回一个形状合法的集合 ETag（内容值由用例另行指定）。
// agentSetETag renders a well-formed collection ETag literal.
func agentSetETag(fill string) string {
	return `"sha256:` + strings.Repeat(fill, 64) + `"`
}

// agentFileSet 构造一次成功结果：默认停用、valid=true、给定文件清单与 ETag。
// agentFileSet builds a successful result with a disabled, valid lifecycle.
func agentFileSet(agent, etag string, paths ...string) AgentFileSet {
	state := string(core.AgentDisabled)
	files := make([]core.FileEntry, len(paths))
	for i, path := range paths {
		files[i] = core.FileEntry{Path: path, Size: int64(len(path))}
	}
	return AgentFileSet{
		Files: core.AgentFiles{
			Agent:     agent,
			Lifecycle: core.AgentLifecycleView{State: &state, Valid: true},
			Files:     files,
		},
		ETag: etag,
	}
}

// decodeAdminAgentFilesView 解码成功信封的 AgentFilesView。
// decodeAdminAgentFilesView decodes the AgentFilesView carried by a success envelope.
func decodeAdminAgentFilesView(t *testing.T, rec *httptest.ResponseRecorder) (Envelope, AgentFilesView) {
	t.Helper()
	env := decodeAuthEnvelope(t, rec)
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var view AgentFilesView
	require.NoError(t, json.Unmarshal(raw, &view))
	return env, view
}

// decodeAgentDraftView 解码成功信封的 AgentDraftView。
// decodeAgentDraftView decodes the AgentDraftView carried by a success envelope.
func decodeAgentDraftView(t *testing.T, rec *httptest.ResponseRecorder) (Envelope, AgentDraftView) {
	t.Helper()
	env := decodeAuthEnvelope(t, rec)
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var view AgentDraftView
	require.NoError(t, json.Unmarshal(raw, &view))
	return env, view
}

// TestAdminAgents_CreateZeroResidue 覆盖创建端点的成功投影与两条失败出口
// （模块卡 §4.4 的 012-web-admin-agents 锚点）：
//   - 成功：请求体字段逐项交给端口，响应是落盘后的真值（默认停用、文件清单、ETag）；
//   - 适配器校验失败 → 400 BAD_REQUEST 且失败信封不带 data（本层不补写任何东西）；
//   - 预写失败 → 固定 500 INTERNAL，底层原因（含路径）不进响应。
//
// 逆改点：handler 自己写盘 / 失败时也带 data / 500 回显适配器文本 → 对应断言红。
func TestAdminAgents_CreateZeroResidue(t *testing.T) {
	t.Run("success projects the adapter truth", func(t *testing.T) {
		etag := agentSetETag("a")
		var gotSpec core.AgentSpec
		var gotActor core.Actor
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			createFn: func(_ context.Context, actor core.Actor, spec core.AgentSpec) (AgentFileSet, error) {
				gotActor, gotSpec = actor, spec
				return agentFileSet("alpha", etag, "AGENT.md", "prompt.md"), nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents",
			`{"name":"alpha","description":"每日日报","model":"alpha-model"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		require.Equal(t, core.AgentSpec{Name: "alpha", Description: "每日日报", Model: "alpha-model"}, gotSpec)
		require.Equal(t, core.AuthMethodBearer, gotActor.Method, "认证闸门必须把显式 actor 交给端口")
		require.NotEmpty(t, gotActor.Subject)

		env, view := decodeAdminAgentFilesView(t, rec)
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, "alpha", view.Agent)
		require.Equal(t, etag, view.ETag)
		require.Equal(t, []AgentFileView{{Path: "AGENT.md", Size: 8}, {Path: "prompt.md", Size: 9}}, view.Files)
		require.NotNil(t, view.Lifecycle.State)
		require.Equal(t, string(core.AgentDisabled), *view.Lifecycle.State, "创建默认停用")
		require.True(t, view.Lifecycle.Valid)
	})

	t.Run("invalid name maps to 400 without data", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			createFn: func(context.Context, core.Actor, core.AgentSpec) (AgentFileSet, error) {
				return AgentFileSet{}, &core.InvalidInputError{Tool: "agent.create", Reason: "Agent 名不能为空"}
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents", `{}`))
		require.Equal(t, http.StatusBadRequest, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Equal(t, "Agent 名不能为空", env.Message)
		require.Nil(t, env.Data, "失败信封不带 data")
	})

	t.Run("pre-write failure is a fixed 500 without the cause", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			createFn: func(context.Context, core.Actor, core.AgentSpec) (AgentFileSet, error) {
				return AgentFileSet{}, errors.New("预写管理操作失败：/home/secret/basalt.db")
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents", `{"name":"alpha"}`))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.Nil(t, env.Data)
		require.NotContains(t, rec.Body.String(), "/home/secret", "底层原因与绝对路径不得进入响应")
	})
}

// TestAdminAgents_CreateRejectsUnknownField 断言管理写拒绝拼错的键：未知字段 → 400
// 且端口零调用（解码先于任何写动作）。
// Unknown keys are refused before the port is ever called.
func TestAdminAgents_CreateRejectsUnknownField(t *testing.T) {
	var calls int
	srv := newAgentAdminServer(t, &fakeAgentAdminService{
		createFn: func(context.Context, core.Actor, core.AgentSpec) (AgentFileSet, error) {
			calls++
			return AgentFileSet{}, nil
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents",
		`{"name":"alpha","extra":true}`))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, messageAgentCreateBody, env.Message)
	require.Nil(t, env.Data)
	require.Zero(t, calls, "解码失败不得触达端口")
}

// TestAdminAgents_GenerateDraft 覆盖草稿端点的成功投影与缺 description 的 400：
// 成功后返回内存草稿（Agent 名 + 文件正文），不落盘、不写操作行由端口保证。
// 逆改点：成功后重读磁盘 / 缺描述仍调用模型 → 对应断言红。
func TestAdminAgents_GenerateDraft(t *testing.T) {
	t.Run("returns the in-memory draft", func(t *testing.T) {
		var gotName, gotDescription string
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			generateFn: func(_ context.Context, name, description string, answers []core.AuthorAnswer) (AgentDraftResult, error) {
				gotName, gotDescription = name, description
				require.Empty(t, answers)
				return AgentDraftResult{Agent: name, Files: []core.AuthorFile{
					{Path: "AGENT.md", Content: "---\nname: alpha\n---\n"},
					{Path: "notes.md", Content: "草稿"},
				}}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/generate-files",
			`{"description":"一个每日日报 Agent"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		require.Equal(t, "alpha", gotName)
		require.Equal(t, "一个每日日报 Agent", gotDescription)

		env, view := decodeAgentDraftView(t, rec)
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, "alpha", view.Agent)
		require.Equal(t, []AgentDraftFileView{
			{Path: "AGENT.md", Content: "---\nname: alpha\n---\n"},
			{Path: "notes.md", Content: "草稿"},
		}, view.Files)
	})

	t.Run("missing description is 400 with zero model calls", func(t *testing.T) {
		var calls int
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			generateFn: func(context.Context, string, string, []core.AuthorAnswer) (AgentDraftResult, error) {
				calls++
				return AgentDraftResult{}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/generate-files", `{}`))
		require.Equal(t, http.StatusBadRequest, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Equal(t, messageAgentDraftDescription, env.Message)
		require.Zero(t, calls, "缺 description 不得发起模型调用")
	})
}

// TestAdminAgents_GenerateNotConfigured 断言未配置作者模型 → 503 NOT_CONFIGURED，
// 文案点名 author.provider / author.model（契约要求点名配置键）。
// An unconfigured author model answers 503 naming both configuration keys.
func TestAdminAgents_GenerateNotConfigured(t *testing.T) {
	srv := newAgentAdminServer(t, &fakeAgentAdminService{
		generateFn: func(context.Context, string, string, []core.AuthorAnswer) (AgentDraftResult, error) {
			return AgentDraftResult{}, fmt.Errorf("%w: 未配置 author.provider / author.model", core.ErrProviderNotConfigured)
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/generate-files",
		`{"description":"日报"}`))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeNotConfigured, env.Code)
	require.Contains(t, env.Message, "author.provider")
	require.Contains(t, env.Message, "author.model")
	require.Nil(t, env.Data)
}

// TestAdminAgents_GetFilesSetETag 断言 GET files 的响应头 ETag 与 data.etag 逐字
// 相同，且文件清单与生命周期按端口结果投影（顺序保持）。
// The header and data carry the same collection ETag, and file order is preserved.
func TestAdminAgents_GetFilesSetETag(t *testing.T) {
	etag := agentSetETag("b")
	srv := newAgentAdminServer(t, &fakeAgentAdminService{
		filesFn: func(_ context.Context, name string) (AgentFileSet, error) {
			require.Equal(t, "alpha", name)
			return agentFileSet("alpha", etag, "AGENT.md", "skills/research/SKILL.md"), nil
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodGet, "/api/v1/agents/alpha/files", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, etag, rec.Header().Get("ETag"))

	env, view := decodeAdminAgentFilesView(t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Equal(t, etag, view.ETag)
	require.Equal(t, etag, rec.Header().Get("ETag"), "响应头与视图必须是同一个集合 ETag")
	require.Equal(t, []AgentFileView{
		{Path: "AGENT.md", Size: 8},
		{Path: "skills/research/SKILL.md", Size: 24},
	}, view.Files)
}

// TestAdminAgents_GetFilesNotFound 断言未知 Agent → 404 NOT_FOUND 且点名名字。
// An unknown agent answers 404 with the requested name.
func TestAdminAgents_GetFilesNotFound(t *testing.T) {
	srv := newAgentAdminServer(t, &fakeAgentAdminService{
		filesFn: func(context.Context, string) (AgentFileSet, error) {
			return AgentFileSet{}, &core.AgentNotFoundError{Name: "ghost"}
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodGet, "/api/v1/agents/ghost/files", ""))
	require.Equal(t, http.StatusNotFound, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Contains(t, env.Message, "ghost")
	require.Nil(t, env.Data)
}

// TestAdminAgents_SubmitProjectsFreshView 断言多文件提交把文件逐项转换成端口类型、
// 把 If-Match 原样传入，并把端口给出的落盘后真值投影为响应。
// 逆改点：归一化 If-Match / 回显请求体而不是端口结果 → 对应断言红。
func TestAdminAgents_SubmitProjectsFreshView(t *testing.T) {
	ifMatch := agentSetETag("c")
	fresh := agentSetETag("d")
	var gotIfMatch string
	var gotFiles []core.AuthorFile
	srv := newAgentAdminServer(t, &fakeAgentAdminService{
		submitFn: func(_ context.Context, actor core.Actor, name, match string, files []core.AuthorFile) (AgentFileSet, error) {
			require.Equal(t, "alpha", name)
			require.Equal(t, core.AuthMethodBearer, actor.Method)
			gotIfMatch, gotFiles = match, files
			return agentFileSet("alpha", fresh, "AGENT.md", "keep.txt"), nil
		},
	})

	req := agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/files",
		`{"files":[{"path":"AGENT.md","content":"---\nname: alpha\n---\n"}]}`)
	req.Header.Set("If-Match", ifMatch)
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.Equal(t, ifMatch, gotIfMatch, "If-Match 必须原样交给适配器比较")
	require.Equal(t, []core.AuthorFile{
		{Path: "AGENT.md", Content: "---\nname: alpha\n---\n"},
	}, gotFiles)

	env, view := decodeAdminAgentFilesView(t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Equal(t, fresh, view.ETag)
	require.Equal(t, fresh, rec.Header().Get("ETag"))
	require.Equal(t, []AgentFileView{{Path: "AGENT.md", Size: 8}, {Path: "keep.txt", Size: 8}}, view.Files)
}

// TestAdminAgents_SubmitStaleETagNoOperation 断言过期集合 ETag 由适配器在锁内判定
// 后映射 412：只调用一次端口（不重试、不自动修复），失败信封不带 data。零写入与
// 零操作行由适配器保证（TestAgentAdmin_SubmitStaleETagNoOperation）。
// A stale ETag maps to 412 exactly once; the handler never retries or self-heals.
func TestAdminAgents_SubmitStaleETagNoOperation(t *testing.T) {
	var calls int
	srv := newAgentAdminServer(t, &fakeAgentAdminService{
		submitFn: func(context.Context, core.Actor, string, string, []core.AuthorFile) (AgentFileSet, error) {
			calls++
			return AgentFileSet{}, core.ErrPreconditionFailed
		},
	})

	req := agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/files",
		`{"files":[{"path":"AGENT.md","content":"---\nname: alpha\n---\n"}]}`)
	req.Header.Set("If-Match", agentSetETag("e"))
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codePreconditionFailed, env.Code)
	require.Equal(t, messageAgentFilesConflict, env.Message)
	require.Nil(t, env.Data, "412 失败信封不带 data")
	require.Equal(t, 1, calls, "412 之后不得重试或自动修复")
}

// TestAdminAgents_SubmitRejectsMissingIfMatch 断言条件写中间件在进入处理函数之前
// 拒绝缺失的 If-Match：412 且端口零调用（也就零操作行）。
// The conditional-write middleware answers 412 before the handler runs.
func TestAdminAgents_SubmitRejectsMissingIfMatch(t *testing.T) {
	var calls int
	srv := newAgentAdminServer(t, &fakeAgentAdminService{
		submitFn: func(context.Context, core.Actor, string, string, []core.AuthorFile) (AgentFileSet, error) {
			calls++
			return AgentFileSet{}, nil
		},
	})

	rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/files",
		`{"files":[{"path":"AGENT.md","content":"---\nname: alpha\n---\n"}]}`))
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codePreconditionFailed, env.Code)
	require.Equal(t, ifMatchRequiredMessage, env.Message)
	require.Zero(t, calls, "缺 If-Match 不得触达端口")
}

// TestAdminAgents_SubmitRequiresFiles 断言请求体缺 files → 400 且端口零调用；
// 空数组留给适配器按缺 AGENT.md 点名（本层不做内容级判断）。
// A missing files key is refused here; an empty array reaches the adapter.
func TestAdminAgents_SubmitRequiresFiles(t *testing.T) {
	var calls int
	srv := newAgentAdminServer(t, &fakeAgentAdminService{
		submitFn: func(context.Context, core.Actor, string, string, []core.AuthorFile) (AgentFileSet, error) {
			calls++
			return AgentFileSet{}, nil
		},
	})

	req := agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/files", `{}`)
	req.Header.Set("If-Match", agentSetETag("f"))
	rec := serveAdmin(srv, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, messageAgentFilesRequired, env.Message)
	require.Zero(t, calls, "缺 files 不得触达端口")
}

// TestAdminAgents_Copy 覆盖复制端点：成功时源与目标逐项交给端口并投影新 Agent；
// 目标冲突（`*core.AdminError` + ErrConflict）→ 409 CONFLICT 且沿用适配器文案。
// The copy endpoint forwards source/target and maps a duplicate target to 409.
func TestAdminAgents_Copy(t *testing.T) {
	t.Run("success projects the copied agent", func(t *testing.T) {
		etag := agentSetETag("1")
		var gotSource, gotTarget string
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			copyFn: func(_ context.Context, actor core.Actor, source, target string,
				_ []core.ResourceMapping,
			) (AgentFileSet, error) {
				require.Equal(t, core.AuthMethodBearer, actor.Method)
				gotSource, gotTarget = source, target
				return agentFileSet(target, etag, "AGENT.md"), nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/copy",
			`{"new_name":"alpha-copy"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		require.Equal(t, "alpha", gotSource)
		require.Equal(t, "alpha-copy", gotTarget)

		_, view := decodeAdminAgentFilesView(t, rec)
		require.Equal(t, "alpha-copy", view.Agent)
		require.Equal(t, etag, view.ETag)
	})

	t.Run("duplicate target is 409 with the adapter wording", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			copyFn: func(context.Context, core.Actor, string, string, []core.ResourceMapping) (AgentFileSet, error) {
				return AgentFileSet{}, &core.AdminError{
					Kind:    core.ErrConflict,
					Message: "目标已存在：alpha-copy",
				}
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/copy",
			`{"new_name":"alpha-copy"}`))
		require.Equal(t, http.StatusConflict, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeConflict, env.Code)
		require.Equal(t, "目标已存在：alpha-copy", env.Message)
		require.Nil(t, env.Data)
	})
}

// TestAdminAgents_MissingPort 断言写面端口为 nil 是装配错误：固定 500 INTERNAL，
// 不冒充空结果、不泄露缺的是哪个实现。
// A nil write port fails closed with a fixed 500.
func TestAdminAgents_MissingPort(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })

	t.Run("POST /agents", func(t *testing.T) {
		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents", `{"name":"alpha"}`))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
	})

	t.Run("GET /agents/{name}/files", func(t *testing.T) {
		rec := serveAdmin(srv, agentAdminCall(http.MethodGet, "/api/v1/agents/alpha/files", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
	})
}

// TestAdminAgents_GenerateDraftAnswers 覆盖问答补全的透传（R6.3）：请求体里的
// `answers` 逐字段交给端口（id / question / answer 不改写、不裁剪），响应把端口返回的
// `questions` 原样投影；缺省答案以空集合到达端口。
// Answers reach the port verbatim and returned questions are projected unchanged.
func TestAdminAgents_GenerateDraftAnswers(t *testing.T) {
	t.Run("answers pass through to the port", func(t *testing.T) {
		var got []core.AuthorAnswer
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			generateFn: func(_ context.Context, _, _ string, answers []core.AuthorAnswer) (AgentDraftResult, error) {
				got = answers
				return AgentDraftResult{
					Agent: "alpha",
					Questions: []core.AuthorQuestion{
						{ID: "frequency", Question: "每天几点运行？"},
					},
				}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/generate-files",
			`{"description":"日报 Agent","answers":[{"id":"frequency","question":"每天几点运行？","answer":"08:30"}]}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		require.Equal(t, []core.AuthorAnswer{
			{ID: "frequency", Question: "每天几点运行？", Answer: "08:30"},
		}, got, "答案必须逐字段原样透传")

		_, view := decodeAgentDraftView(t, rec)
		require.Empty(t, view.Files)
		require.Equal(t, []AgentQuestionView{{ID: "frequency", Question: "每天几点运行？"}}, view.Questions)
	})

	t.Run("absent answers reach the port as empty", func(t *testing.T) {
		called := false
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			generateFn: func(_ context.Context, _, _ string, answers []core.AuthorAnswer) (AgentDraftResult, error) {
				called = true
				require.Empty(t, answers)
				return AgentDraftResult{Agent: "alpha"}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/generate-files",
			`{"description":"日报 Agent"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.True(t, called)
	})
}

// decodeScheduleDraftView 解码成功信封的 ScheduleDraftView。
// decodeScheduleDraftView decodes the ScheduleDraftView carried by a success envelope.
func decodeScheduleDraftView(t *testing.T, rec *httptest.ResponseRecorder) (Envelope, ScheduleDraftView) {
	t.Helper()
	env := decodeAuthEnvelope(t, rec)
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var view ScheduleDraftView
	require.NoError(t, json.Unmarshal(raw, &view))
	return env, view
}

// TestAdminAgents_ScheduleDraft 覆盖定时候选端点的成功投影（R6.4）：候选四字段原样
// 输出，未来触发时间由服务端给出的集合按 RFC3339 UTC 投影；空集合输出 `[]` 而不是
// `null`（浏览器直接渲染数组）。候选尚未保存，本层不写盘、不写操作行由适配器保证。
// The candidate is projected verbatim; server-computed triggers are RFC3339 UTC and an
// empty list never marshals to null.
func TestAdminAgents_ScheduleDraft(t *testing.T) {
	t.Run("candidate and next runs are projected", func(t *testing.T) {
		var gotName, gotInstruction string
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(_ context.Context, name, instruction string) (ScheduleDraftResult, error) {
				gotName, gotInstruction = name, instruction
				return ScheduleDraftResult{
					Agent: name,
					Candidate: core.Schedule{
						ID:      "daily-report",
						Cron:    "0 30 8 * * *",
						Zone:    "Asia/Shanghai",
						Message: "生成本周周报",
					},
					NextRuns: []time.Time{
						time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC),
						time.Date(2026, 10, 26, 0, 30, 0, 0, time.UTC),
						time.Date(2026, 10, 27, 0, 30, 0, 0, time.UTC),
					},
				}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/schedule-draft",
			`{"instruction":"每天早上八点半生成周报"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		require.Equal(t, "alpha", gotName)
		require.Equal(t, "每天早上八点半生成周报", gotInstruction)

		env, view := decodeScheduleDraftView(t, rec)
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, ScheduleDraftView{
			Agent:    "alpha",
			ID:       "daily-report",
			Cron:     "0 30 8 * * *",
			Zone:     "Asia/Shanghai",
			Message:  "生成本周周报",
			NextRuns: []string{"2026-10-25T00:30:00Z", "2026-10-26T00:30:00Z", "2026-10-27T00:30:00Z"},
		}, view)
	})

	t.Run("empty next runs stay an array", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(_ context.Context, name, _ string) (ScheduleDraftResult, error) {
				return ScheduleDraftResult{
					Agent:     name,
					Candidate: core.Schedule{ID: "daily", Cron: "0 0 8 * * *", Zone: "UTC", Message: "周报"},
				}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/schedule-draft",
			`{"instruction":"每天八点"}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		_, view := decodeScheduleDraftView(t, rec)
		require.NotNil(t, view.NextRuns)
		require.Empty(t, view.NextRuns)
		require.Contains(t, rec.Body.String(), `"next_runs":[]`, "空集合必须投影为 []")
	})
}

// TestAdminAgents_ScheduleDraftErrors 覆盖定时候选端点的失败档位（R6.4）：
// 请求体不合法 → 400 且端口零调用；候选非法（`*core.InvalidInputError`）→ 400 点名
// 原因；作者未配置 → 503 NOT_CONFIGURED 点名配置键；其他失败 → 固定 500 不泄露底层
// 原因；端口缺失 → 固定 500。
// The schedule endpoint maps each failure class to its contract status code.
func TestAdminAgents_ScheduleDraftErrors(t *testing.T) {
	t.Run("malformed body is 400 with zero port calls", func(t *testing.T) {
		var calls int
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(context.Context, string, string) (ScheduleDraftResult, error) {
				calls++
				return ScheduleDraftResult{}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/schedule-draft",
			`{"instruction":42}`))
		require.Equal(t, http.StatusBadRequest, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Equal(t, messageAgentScheduleBody, env.Message)
		require.Nil(t, env.Data)
		require.Zero(t, calls, "解码失败不得触达端口")
	})

	t.Run("unknown field is 400 with zero port calls", func(t *testing.T) {
		var calls int
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(context.Context, string, string) (ScheduleDraftResult, error) {
				calls++
				return ScheduleDraftResult{}, nil
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/schedule-draft",
			`{"instruction":"每天八点","cron":"0 0 8 * * *"}`))
		require.Equal(t, http.StatusBadRequest, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Equal(t, messageAgentScheduleBody, env.Message)
		require.Zero(t, calls, "未知字段不得触达端口")
	})

	t.Run("invalid candidate is 400 naming the reason", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(context.Context, string, string) (ScheduleDraftResult, error) {
				return ScheduleDraftResult{}, &core.InvalidInputError{
					Tool: "schedule-draft", Reason: "候选的 cron 不是 6 段制式",
				}
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/schedule-draft",
			`{"instruction":"每天八点"}`))
		require.Equal(t, http.StatusBadRequest, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Equal(t, "候选的 cron 不是 6 段制式", env.Message)
		require.Nil(t, env.Data)
	})

	t.Run("unconfigured author is 503", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(context.Context, string, string) (ScheduleDraftResult, error) {
				return ScheduleDraftResult{}, fmt.Errorf("%w: 未配置 author.provider / author.model", core.ErrProviderNotConfigured)
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/schedule-draft",
			`{"instruction":"每天八点"}`))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeNotConfigured, env.Code)
		require.Contains(t, env.Message, "author.provider")
		require.Contains(t, env.Message, "author.model")
		require.Nil(t, env.Data)
	})

	t.Run("unexpected failure is a fixed 500", func(t *testing.T) {
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(context.Context, string, string) (ScheduleDraftResult, error) {
				return ScheduleDraftResult{}, errors.New("模型网关返回 502：/home/secret/trace")
			},
		})

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/schedule-draft",
			`{"instruction":"每天八点"}`))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.NotContains(t, rec.Body.String(), "/home/secret", "底层原因与绝对路径不得进入响应")
	})

	t.Run("missing port is a fixed 500", func(t *testing.T) {
		srv := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })

		rec := serveAdmin(srv, agentAdminCall(http.MethodPost, "/api/v1/agents/alpha/schedule-draft",
			`{"instruction":"每天八点"}`))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
	})
}

// TestAdminAgents_AuthorFailureIsActionable —— R7 问题 8：作者模型调用失败不再回
// 笼统的"服务器内部错误"。分类哨兵 `ErrAuthorModelUnavailable` → 500 INTERNAL，
// 但文案取适配器给的可执行提示（点名 provider 与修复入口）；上游超时经
// `errors.Is(context.DeadlineExceeded)` → 504 且同样保留可执行文案。
// 中文验收点：一键创建失败时，运维看到"改哪里"，不是"内部错误"。
// TestAdminAgents_AuthorFailureIsActionable keeps the codes stable while surfacing
// the adapter's actionable wording for author-model failures and timeouts.
func TestAdminAgents_AuthorFailureIsActionable(t *testing.T) {
	t.Parallel()

	t.Run("调用失败带修复入口", func(t *testing.T) {
		t.Parallel()
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(context.Context, string, string) (ScheduleDraftResult, error) {
				return ScheduleDraftResult{}, &core.AdminError{
					Kind:    core.ErrAuthorModelUnavailable,
					Message: "作者模型调用失败（provider \"deepseek\"）：请检查该模型连接的地址、默认模型与凭据；本次操作未保存任何内容。",
				}
			},
		})
		rec := serveAdmin(srv, agentAdminCall(http.MethodPost,
			"/api/v1/agents/alpha/schedule-draft", `{"instruction":"每天 8:30"}`))
		require.Equal(t, http.StatusInternalServerError, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Contains(t, env.Message, `provider "deepseek"`)
		require.Contains(t, env.Message, "未保存任何内容")
		require.NotEqual(t, messageInternal, env.Message, "不得退回笼统的内部错误文案")
	})

	t.Run("上游超时映射504", func(t *testing.T) {
		t.Parallel()
		srv := newAgentAdminServer(t, &fakeAgentAdminService{
			scheduleFn: func(context.Context, string, string) (ScheduleDraftResult, error) {
				return ScheduleDraftResult{}, &core.AdminError{
					Kind:    context.DeadlineExceeded,
					Message: "作者模型调用超时或被取消（provider \"deepseek\"）：请稍后重试；本次操作未保存任何内容。",
				}
			},
		})
		rec := serveAdmin(srv, agentAdminCall(http.MethodPost,
			"/api/v1/agents/alpha/schedule-draft", `{"instruction":"每天 8:30"}`))
		require.Equal(t, http.StatusGatewayTimeout, rec.Code)

		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeTimeout, env.Code)
		require.Contains(t, env.Message, "请稍后重试")
	})
}
