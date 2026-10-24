package web

// 本文件覆盖 Agent 启停端点（模块卡 §3.2.1 handlers_lifecycle_test.go 的 14 例口径）：
// disable / enable 正常路径与生命周期视图、期望状态不符 409、状态文件缺失 / 损坏 409
// `AGENT_UNAVAILABLE`、启用被加载期校验或资源发布检查拒绝 409、适配器失败映射、预写
// 失败 500、缺失装配端口 500、停用不取消在途运行、未认证 401 与 actor 记录。
// 逆改点：存在性判定挪到适配器之后 → 404 / 409 互换；启用不查装载结果 → 「配置无效
// 仍可启用」子例红；启用不查资源发布 → 「缺发布版本」子例红；停用路径触达
// Runs.Cancel → 在途不受影响子例红。
// This file covers the lifecycle endpoints: the happy paths and their view, conflict,
// missing/corrupt state files, enable refused by loader validation, adapter failure
// mapping, pre-write failure, missing port, no cancellation of in-flight runs, auth,
// and actor recording.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// lifecycleCall 记录一次适配器调用（Agent、目标态、前置条件与 actor）。
// lifecycleCall records one adapter invocation.
type lifecycleCall struct {
	agent    string
	target   core.AgentState
	expected core.AgentLifecycleTransition
	actor    core.Actor
}

// lifecycleAdminFake 是 core.AgentAdmin 的记录型假端口：Transition 返回预置记录或
// 错误；ReadContent / UpdateContent 在启停用例里被调用即说明实现走错了分支。
// lifecycleAdminFake records Transition calls; a content call from a lifecycle test is
// itself a failure signal.
type lifecycleAdminFake struct {
	mu        sync.Mutex
	calls     []lifecycleCall
	record    core.LifecycleRecord
	err       error
	readCalls int
}

var _ core.AgentAdmin = (*lifecycleAdminFake)(nil)

func (f *lifecycleAdminFake) ReadContent(context.Context, string) (core.AgentContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls++
	return core.AgentContent{}, errors.New("lifecycle 端点不得调用 ReadContent")
}

func (f *lifecycleAdminFake) UpdateContent(context.Context, string,
	core.AgentContentUpdate, string, core.Actor,
) (core.AgentFiles, error) {
	return core.AgentFiles{}, errors.New("lifecycle 端点不得调用 UpdateContent")
}

func (f *lifecycleAdminFake) Transition(_ context.Context, agent string, target core.AgentState,
	expected core.AgentLifecycleTransition, actor core.Actor,
) (core.LifecycleRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, lifecycleCall{agent: agent, target: target, expected: expected, actor: actor})
	if f.err != nil {
		return core.LifecycleRecord{}, f.err
	}
	return f.record, nil
}

// transitions 返回适配器调用快照。
func (f *lifecycleAdminFake) transitions() []lifecycleCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lifecycleCall(nil), f.calls...)
}

// lifecycleRegistryFake 是只读注册表假端口：Entries / Lookup 都只反映当前快照。
// lifecycleRegistryFake serves the read-only registry port from a fixed snapshot.
type lifecycleRegistryFake struct {
	entries []core.AgentEntry
}

var _ AgentRegistry = (*lifecycleRegistryFake)(nil)

func (f *lifecycleRegistryFake) Entries() []core.AgentEntry {
	return append([]core.AgentEntry(nil), f.entries...)
}

func (f *lifecycleRegistryFake) Lookup(name string) (core.AgentEntry, bool) {
	for _, entry := range f.entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return core.AgentEntry{}, false
}

// lifecycleRunRecorder 在共享假运行服务上记录 Cancel：停用**不得**取消在途运行。
// lifecycleRunRecorder observes Cancel on top of the shared fake run service.
type lifecycleRunRecorder struct {
	*fakeRunService

	mu      sync.Mutex
	cancels []string
}

func (r *lifecycleRunRecorder) Cancel(_ context.Context, runID string) (core.RunDetail, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels = append(r.cancels, runID)
	return core.RunDetail{}, nil
}

func (r *lifecycleRunRecorder) cancelIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cancels...)
}

// lifecycleRecord 是成功迁移的预置记录：updated_at 取固定时刻的 Unix 毫秒。
// lifecycleRecord is the preset successful-transition record.
func lifecycleRecord(state core.AgentState) core.LifecycleRecord {
	return core.LifecycleRecord{
		SchemaVersion: 1,
		Agent:         "ops-agent",
		State:         state,
		Generation:    4,
		UpdatedAt:     time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC).UnixMilli(),
		OperationID:   "op-1761057600000-abc123",
		Checksum:      "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
}

// decodeLifecycleView 解析 200 信封里的 AgentLifecycleView。
// decodeLifecycleView decodes the lifecycle view from a success envelope.
func decodeLifecycleView(t *testing.T, rec *httptest.ResponseRecorder) AgentLifecycleView {
	t.Helper()
	var env struct {
		Code string             `json:"code"`
		Data AgentLifecycleView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, codeOK, env.Code)
	return env.Data
}

// TestLifecycle_Disable_Success 覆盖停用成功路径与状态视图（模块卡 §3.2.1 第 1 行，
// 含契约 AgentLifecycleView 的七个字段）。
// 逆改点：视图缺 valid / state 投影错 / actor 未透传 → 对应断言红。
func TestLifecycle_Disable_Success(t *testing.T) {
	const body = `{"expected_state":"active","expected_generation":3}`

	t.Run("returns the lifecycle view", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentDisabled)}
		s := newTestServer(t, func(o *Options) { o.AgentAdmin = admin })

		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable", body))
		require.Equal(t, http.StatusOK, rec.Code, "%s", rec.Body.String())

		view := decodeLifecycleView(t, rec)
		require.Equal(t, "ops-agent", view.Agent)
		require.NotNil(t, view.State)
		require.Equal(t, string(core.AgentDisabled), *view.State)
		require.Equal(t, int64(4), view.Generation)
		require.Equal(t, "2026-10-21T12:00:00Z", view.UpdatedAt)
		require.Equal(t, "op-1761057600000-abc123", view.OperationID)
		require.True(t, view.Valid, "成功迁移意味着状态文件刚通过校验")
		require.Empty(t, view.LifecycleError)

		calls := admin.transitions()
		require.Len(t, calls, 1)
		require.Equal(t, "ops-agent", calls[0].agent)
		require.Equal(t, core.AgentDisabled, calls[0].target)
		require.Equal(t, core.AgentLifecycleTransition{
			ExpectedState: core.AgentActive, ExpectedGeneration: 3,
		}, calls[0].expected)
	})

	t.Run("registry entry alone proves existence", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentDisabled)}
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.Agents = fakeAgents{} // 装载结果为空：存在性由注册表快照兜底
			o.Registry = &lifecycleRegistryFake{entries: []core.AgentEntry{
				{Name: "ops-agent", Registered: true, Profile: &core.Profile{Name: "ops-agent"}},
			}}
		})

		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable", body))
		require.Equal(t, http.StatusOK, rec.Code, "%s", rec.Body.String())
		require.Len(t, admin.transitions(), 1)
	})
}

// TestLifecycle_Enable_Success 覆盖启用成功路径：目标态 active，返回视图 state=active。
// 逆改点：enable 与 disable 的目标态写反 → 本用例与停用用例同时红。
func TestLifecycle_Enable_Success(t *testing.T) {
	admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
	s := newTestServer(t, func(o *Options) { o.AgentAdmin = admin })

	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/enable",
		`{"expected_state":"disabled","expected_generation":3}`))
	require.Equal(t, http.StatusOK, rec.Code, "%s", rec.Body.String())

	view := decodeLifecycleView(t, rec)
	require.NotNil(t, view.State)
	require.Equal(t, string(core.AgentActive), *view.State)
	require.True(t, view.Valid)

	calls := admin.transitions()
	require.Len(t, calls, 1)
	require.Equal(t, core.AgentActive, calls[0].target)
	require.Equal(t, core.AgentLifecycleTransition{
		ExpectedState: core.AgentDisabled, ExpectedGeneration: 3,
	}, calls[0].expected)
}

// TestLifecycle_RejectsInvalidBody 覆盖请求体校验（图 C 第 ① 步）：非法输入 400 且
// **零适配器调用**（静态校验先于任何留痕与写入）。
// 逆改点：先调 Transition 再校验 / 未知字段被忽略 → 对应子例红。
func TestLifecycle_RejectsInvalidBody(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantInText string
	}{
		{"malformed json", `{`, messageLifecycleBody},
		{"unknown field", `{"expected_state":"active","expected_generation":1,"extra":true}`, messageLifecycleBody},
		{"trailing document", `{"expected_state":"active","expected_generation":1}{}`, messageLifecycleBody},
		{"missing expected_state", `{"expected_generation":1}`, "expected_state 必填"},
		{"null expected_state", `{"expected_state":null,"expected_generation":1}`, "expected_state 必填"},
		{"missing expected_generation", `{"expected_state":"active"}`, "expected_generation 必填"},
		{"archived is not reachable", `{"expected_state":"archived","expected_generation":1}`, "active / disabled"},
		{"non-numeric generation", `{"expected_state":"active","expected_generation":"1"}`, messageLifecycleBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
			s := newTestServer(t, func(o *Options) { o.AgentAdmin = admin })

			rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable", tc.body))
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s", rec.Body.String())
			env := decodeAuthEnvelope(t, rec)
			require.Equal(t, codeBadRequest, env.Code)
			require.Contains(t, env.Message, tc.wantInText)
			require.Empty(t, admin.transitions(), "非法请求体不得触达适配器（零留痕、零写入）")
		})
	}

	t.Run("oversized body is rejected before parsing", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.MaxRequestBytes = 16
		})
		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable",
			`{"expected_state":"active","expected_generation":1}`))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code)
		require.Contains(t, env.Message, "上限")
		require.Empty(t, admin.transitions())
	})
}

// TestLifecycle_UnknownAgent_NotFound 覆盖「Agent 不存在 → 404」（错误表）：存在性
// 由注册表与装载结果的并集判定，未命中即 404，**不**触达适配器。
// 逆改点：把缺失状态文件当 404 → 本用例与缺失 409 用例互换而红。
func TestLifecycle_UnknownAgent_NotFound(t *testing.T) {
	for _, action := range []string{"disable", "enable"} {
		t.Run(action, func(t *testing.T) {
			admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
			s := newTestServer(t, func(o *Options) {
				o.AgentAdmin = admin
				o.Agents = fakeAgents{}
			})

			rec := serveAdmin(s, adminRequest(http.MethodPost,
				"/api/v1/agents/ghost-agent/"+action,
				`{"expected_state":"active","expected_generation":1}`))
			require.Equal(t, http.StatusNotFound, rec.Code, "%s", rec.Body.String())
			env := decodeAuthEnvelope(t, rec)
			require.Equal(t, codeNotFound, env.Code)
			require.Contains(t, env.Message, "ghost-agent")
			require.Empty(t, admin.transitions(), "不存在的 Agent 不得进入适配器")
		})
	}
}

// TestLifecycle_ExpectedMismatch_Conflict 覆盖期望状态 / 代数不符 → 409 CONFLICT
// （错误表第 3 行）：适配器在锁内比较失败，web 只做映射并点名 Agent。
// R8 裁决 1 之后文案是固定中文；当前 generation 只进日志，客户端按约定重新读取。
// 逆改点：冲突被误映射成 404 / 500 → 本用例红。
func TestLifecycle_ExpectedMismatch_Conflict(t *testing.T) {
	admin := &lifecycleAdminFake{err: fmt.Errorf(
		"%w: Agent ops-agent 期望 active/3，当前 disabled/4", core.ErrLifecycleConflict)}
	s := newTestServer(t, func(o *Options) { o.AgentAdmin = admin })

	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable",
		`{"expected_state":"active","expected_generation":3}`))
	require.Equal(t, http.StatusConflict, rec.Code, "%s", rec.Body.String())
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeConflict, env.Code)
	require.Equal(t, lifecycleConflictMessage("ops-agent"), env.Message)
	require.NotContains(t, env.Message, "expected_generation",
		"英文哨兵与内部字段名不得回显")
}

// TestLifecycle_StateFileMissing_Unavailable 覆盖状态文件缺失 → 409
// `AGENT_UNAVAILABLE`（约束 16 / 错误表第 5 行）：失败关闭，不当作 active。
// 逆改点：缺失被判成 404 Agent 不存在 → 本用例红。
func TestLifecycle_StateFileMissing_Unavailable(t *testing.T) {
	admin := &lifecycleAdminFake{err: &core.AgentUnavailableError{
		Name:   "ops-agent",
		Kind:   core.AgentUnavailableMissing,
		Reason: "lifecycle state file missing; restore it before running",
		Cause:  fmt.Errorf("open control/agents/ops-agent.json: %w", fs.ErrNotExist),
	}}
	s := newTestServer(t, func(o *Options) { o.AgentAdmin = admin })

	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/enable",
		`{"expected_state":"disabled","expected_generation":2}`))
	require.Equal(t, http.StatusConflict, rec.Code, "%s", rec.Body.String())
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeAgentUnavailable, env.Code)
	require.Contains(t, env.Message, "ops-agent")
	require.Contains(t, env.Message, "生命周期状态文件缺失", "用户可见文案必须是中文（R8 裁决 1）")
	require.NotContains(t, rec.Body.String(), "lifecycle state file missing",
		"英文 Reason 只进日志与审计，不得回显")
	require.NotContains(t, rec.Body.String(), fs.ErrNotExist.Error(),
		"底层原因只经 Unwrap 暴露，不得进入响应")
}

// TestLifecycle_StateFileCorrupt_Unavailable 覆盖状态文件损坏 → 409
// `AGENT_UNAVAILABLE`，且响应不含绝对路径与底层解码原文（约束 45）。
// 逆改点：把底层错误原文直接拼进 message → 本用例红。
func TestLifecycle_StateFileCorrupt_Unavailable(t *testing.T) {
	admin := &lifecycleAdminFake{err: &core.AgentUnavailableError{
		Name:   "ops-agent",
		Kind:   core.AgentUnavailableCorrupt,
		Reason: "lifecycle state file corrupt; restore it before running",
		Cause:  errors.New(`解析状态文件 "D:\\secret\\ops-agent.json" 失败`),
	}}
	s := newTestServer(t, func(o *Options) { o.AgentAdmin = admin })

	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/enable",
		`{"expected_state":"disabled","expected_generation":2}`))
	require.Equal(t, http.StatusConflict, rec.Code)
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeAgentUnavailable, env.Code)
	require.Contains(t, env.Message, "生命周期状态文件损坏", "用户可见文案必须是中文（R8 裁决 1）")
	require.NotContains(t, rec.Body.String(), "secret", "绝对路径不得进入失败文案")
}

// TestLifecycle_Enable_InvalidConfig_Unavailable 覆盖「启用要求加载期校验与资源引用
// 当前仍通过」（约束 16）：未注册的装载条目 409 `AGENT_UNAVAILABLE` 并点名拒绝原因，
// 零适配器调用；拒绝原因为空时回退固定文案。
// 逆改点：只看状态文件不看装载结果 → 「配置无效仍可启用」子例红。
func TestLifecycle_Enable_InvalidConfig_Unavailable(t *testing.T) {
	t.Run("names the loader reject reason", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.Agents = fakeAgents{entries: []core.AgentEntry{
				{
					Name:         "ops-agent",
					Registered:   false,
					RejectKind:   core.AgentRejectNameMismatch,
					RejectDetail: "other-agent",
					// 英文合并拒因只进日志：同一条目上挂着它也不得出现在界面文案里。
					RejectReason: "name mismatch: other-agent",
				},
			}}
		})

		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/enable",
			`{"expected_state":"disabled","expected_generation":2}`))
		require.Equal(t, http.StatusConflict, rec.Code, "%s", rec.Body.String())
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeAgentUnavailable, env.Code)
		require.Contains(t, env.Message, "目录名与 AGENT.md 的 name 不一致：other-agent")
		require.NotContains(t, env.Message, "name mismatch", "英文拒因不得进入用户文案")
		require.Empty(t, admin.transitions(), "配置无效的 Agent 不得触达状态迁移")
	})

	t.Run("falls back to the fixed reason", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.Agents = fakeAgents{entries: []core.AgentEntry{{Name: "ops-agent", Registered: false}}}
		})

		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/enable",
			`{"expected_state":"disabled","expected_generation":2}`))
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeAgentUnavailable, env.Code)
		require.Contains(t, env.Message, enableRejectedReason)
		require.Empty(t, admin.transitions())
	})

	t.Run("disable ignores the loader entry", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentDisabled)}
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.Agents = fakeAgents{entries: []core.AgentEntry{
				{Name: "ops-agent", Registered: false, RejectReason: "任意原因"},
			}}
		})

		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable",
			`{"expected_state":"active","expected_generation":2}`))
		require.Equal(t, http.StatusOK, rec.Code,
			"停用不依赖加载期校验：坏配置的 Agent 也必须能停用")
		require.Len(t, admin.transitions(), 1)
	})
}

// TestLifecycle_Enable_RequiresPublishedResources 覆盖约束 16 的后半句：启用前必须
// 确认 Provider / MCP / 技能引用当前仍有已发布版本。注册只证明工作副本可解析
// （约束 36），发布指针被删除或技能被改成未发布后，启用必须 409，而不是打开一个
// 准入必然失败的 Agent；资源视图读不到时同样失败关闭。
// 逆改点：enable 只看装载结果、不查资源发布 → 「缺发布版本」子例红（返回 200）。
// Requirement 16's other half: enable re-checks that every referenced resource still
// has a published version and fails closed when the index is unreadable.
func TestLifecycle_Enable_RequiresPublishedResources(t *testing.T) {
	referencing := func() core.AgentEntry {
		return core.AgentEntry{
			Name:       "ops-agent",
			Registered: true,
			Profile: &core.Profile{
				Name:       "ops-agent",
				Provider:   core.ProviderRef{Name: "openai", Model: "gpt-test"},
				MCPServers: []string{"files"},
				Skills:     []string{"pdf-extract"},
			},
		}
	}
	published := func() fakeResources {
		return fakeResources{
			modelConns: []ModelConnectionView{sampleModelConnectionView()},
			mcpServers: []McpServerView{sampleMcpServerView()},
			skills:     []SkillView{sampleSkillView()},
		}
	}
	enable := func(t *testing.T, s *Server) *httptest.ResponseRecorder {
		t.Helper()
		return serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/enable",
			`{"expected_state":"disabled","expected_generation":2}`))
	}

	t.Run("refuses when a referenced skill has no published version", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
		res := published()
		res.skills = []SkillView{{Name: "pdf-extract", VersionID: 0}}
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.Agents = fakeAgents{entries: []core.AgentEntry{referencing()}}
			o.Resources = res
		})

		rec := enable(t, s)
		require.Equal(t, http.StatusConflict, rec.Code, "%s", rec.Body.String())
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeAgentUnavailable, env.Code)
		require.Contains(t, env.Message, "skill/pdf-extract")
		require.Empty(t, admin.transitions(), "资源引用未通过的 Agent 不得触达状态迁移")
	})

	t.Run("refuses when the provider or an MCP server is unpublished", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
		res := published()
		res.modelConns = []ModelConnectionView{{Name: "openai"}}
		res.mcpServers = []McpServerView{{Name: "files"}}
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.Agents = fakeAgents{entries: []core.AgentEntry{referencing()}}
			o.Resources = res
		})

		rec := enable(t, s)
		require.Equal(t, http.StatusConflict, rec.Code, "%s", rec.Body.String())
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeAgentUnavailable, env.Code)
		require.Contains(t, env.Message, "model_connection/openai")
		require.Contains(t, env.Message, "mcp_server/files")
		require.Empty(t, admin.transitions())
	})

	t.Run("allows when every referenced resource is published", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.Agents = fakeAgents{entries: []core.AgentEntry{referencing()}}
			o.Resources = published()
		})

		rec := enable(t, s)
		require.Equal(t, http.StatusOK, rec.Code, "%s", rec.Body.String())
		require.Len(t, admin.transitions(), 1)
	})

	t.Run("fails closed when the resource view is unreadable", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
		res := published()
		res.modelListErr = errors.New(`读 resources 失败: D:\data\basalt.db`)
		s := newTestServer(t, func(o *Options) {
			o.AgentAdmin = admin
			o.Agents = fakeAgents{entries: []core.AgentEntry{referencing()}}
			o.Resources = res
		})

		rec := enable(t, s)
		require.Equal(t, http.StatusConflict, rec.Code, "%s", rec.Body.String())
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeAgentUnavailable, env.Code)
		require.Contains(t, env.Message, resourceViewsProblem)
		require.NotContains(t, rec.Body.String(), "basalt.db", "底层原因只进日志")
		require.Empty(t, admin.transitions())
	})
}

// TestLifecycle_AdapterInvalidInput_BadRequest 覆盖适配器返回入参非法 → 400，原因
// 原样公开（`*core.InvalidInputError` 是自产文案，不含路径）。
// 逆改点：InvalidInputError 落到 500 → 本用例红。
func TestLifecycle_AdapterInvalidInput_BadRequest(t *testing.T) {
	admin := &lifecycleAdminFake{err: &core.InvalidInputError{
		Tool:   "lifecycle",
		Reason: "expected_generation 必须是正整数",
	}}
	s := newTestServer(t, func(o *Options) { o.AgentAdmin = admin })

	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable",
		`{"expected_state":"active","expected_generation":0}`))
	require.Equal(t, http.StatusBadRequest, rec.Code, "%s", rec.Body.String())
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, "expected_generation 必须是正整数", env.Message)
}

// TestLifecycle_PrewriteFailure_Internal 覆盖管理操作预写失败 → 500 INTERNAL，固定
// 文案；底层原因只进日志（约束 45 与宪法 V 的 fail-closed 出口）。
// 逆改点：把预写错误原文拼进响应 → 本用例红。
func TestLifecycle_PrewriteFailure_Internal(t *testing.T) {
	admin := &lifecycleAdminFake{err: errors.New(`写 management_operations 失败: D:\data\basalt.db 只读`)}
	s := newTestServer(t, func(o *Options) { o.AgentAdmin = admin })

	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable",
		`{"expected_state":"active","expected_generation":3}`))
	require.Equal(t, http.StatusInternalServerError, rec.Code, "%s", rec.Body.String())
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
	require.NotContains(t, rec.Body.String(), "management_operations")
	require.NotContains(t, rec.Body.String(), "basalt.db")
}

// TestLifecycle_MissingPort_Internal 覆盖装配缺失：AgentAdmin 为 nil 时 500 固定
// 文案（缺端口是装配错误，不是运行时故障）。
// 逆改点：nil 端口被当成「操作成功」→ 本用例红。
func TestLifecycle_MissingPort_Internal(t *testing.T) {
	s := newTestServer(t, nil)
	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable",
		`{"expected_state":"active","expected_generation":1}`))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	env := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
}

// TestLifecycle_Disable_DoesNotCancelRuns 覆盖约束 16「停用只拒绝新准入，在途继续」：
// 停用路径不触达运行端口，更不调用取消。
// 逆改点：disable 顺手取消在途运行 → 本用例红。
func TestLifecycle_Disable_DoesNotCancelRuns(t *testing.T) {
	runs := &lifecycleRunRecorder{fakeRunService: &fakeRunService{
		result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted},
	}}
	admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentDisabled)}
	s := newTestServer(t, func(o *Options) {
		o.AgentAdmin = admin
		o.Runs = runs
	})

	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable",
		`{"expected_state":"active","expected_generation":3}`))
	require.Equal(t, http.StatusOK, rec.Code, "%s", rec.Body.String())
	require.Empty(t, runs.cancelIDs(), "停用不得取消在途运行")
	require.Empty(t, runs.requests(), "停用不得发起新运行")
}

// TestLifecycle_RequiresAuth 覆盖两个启停端点在真实闸门下的认证（401）与放行。
// 逆改点：路由漏挂闸门 → 401 子例红；闸门拒绝后仍进入适配器 → 零副作用断言红。
func TestLifecycle_RequiresAuth(t *testing.T) {
	t.Run("missing credentials are rejected with zero side effects", func(t *testing.T) {
		for _, action := range []string{"disable", "enable"} {
			admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentActive)}
			s := newTestServer(t, func(o *Options) {
				o.AdminAuth = testAdminAuth(testToken)
				o.AgentAdmin = admin
			})

			rec := serveAdmin(s, adminRequest(http.MethodPost,
				"/api/v1/agents/ops-agent/"+action,
				`{"expected_state":"active","expected_generation":1}`))
			require.Equal(t, http.StatusUnauthorized, rec.Code, "%s", action)
			require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, rec).Code)
			require.Empty(t, admin.transitions(), "未认证请求不得触达适配器")
		}
	})

	t.Run("valid credentials reach the adapter", func(t *testing.T) {
		admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentDisabled)}
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.AgentAdmin = admin
		})

		rec := serveAdmin(s, withBearer(adminRequest(http.MethodPost,
			"/api/v1/agents/ops-agent/disable",
			`{"expected_state":"active","expected_generation":8}`), testToken))
		require.Equal(t, http.StatusOK, rec.Code, "%s", rec.Body.String())
		require.Len(t, admin.transitions(), 1)
	})
}

// TestLifecycle_ActorRecorded 覆盖 actor 记录（契约 actor_subject 四形态之一）：
// token 模式只落 sha256 指纹前 16 位十六进制，方法 bearer，来源取直接对端。
// 逆改点：明文 token 进入 actor / 方法写错 / 来源为空 → 对应断言红。
func TestLifecycle_ActorRecorded(t *testing.T) {
	admin := &lifecycleAdminFake{record: lifecycleRecord(core.AgentDisabled)}
	s := newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = admin
	})

	req := withBearer(adminRequest(http.MethodPost, "/api/v1/agents/ops-agent/disable",
		`{"expected_state":"active","expected_generation":3}`), testToken)
	rec := serveAdmin(s, req)
	require.Equal(t, http.StatusOK, rec.Code, "%s", rec.Body.String())

	sum := sha256.Sum256([]byte(testToken))
	wantSubject := core.ActorSubjectTokenPrefix + hex.EncodeToString(sum[:])[:16]

	calls := admin.transitions()
	require.Len(t, calls, 1)
	actor := calls[0].actor
	require.Equal(t, wantSubject, actor.Subject)
	require.Equal(t, core.AuthMethodBearer, actor.Method)
	require.Equal(t, req.RemoteAddr, actor.Source)
	require.NotContains(t, actor.Subject, testToken, "明文 token 不得进入 actor")
}
