package web

// 本文件覆盖 Agent 层记忆读取端点（模块卡 §3.1.27 / §4.1 第 23 行，6 例）：
// scope 精确传给仓储、空层**不**回退全局、未知与未注册 Agent 的 404（零读取）、
// 读取失败的固定 500（不出半份内容），以及响应字段集与契约一致（无内部字段出站）。
// Agent-layer memory acceptance: exact scope, no global fallback, 404 for unknown and
// unregistered agents, a fixed 500 with no partial body, and the contract field set.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errMemoryReadFailed 是记忆读取失败的注入错误（文案只用于「不得出站」断言）。
var errMemoryReadFailed = errors.New("sqlite: disk I/O error")

// recordingMemory 记录每次 Sections 的作用域：查询是否回退全局层，最直接的证据
// 就是"读了几次、每次带的 scope 是什么"。
// recordingMemory records every Sections scope, so a fallback would show up as either a
// second call or a global-scope read.
type recordingMemory struct {
	mu       sync.Mutex
	scopes   []core.Scope
	sections func(core.Scope) (string, string, error)
}

func (m *recordingMemory) Sections(_ context.Context, scope core.Scope) (string, string, error) {
	m.mu.Lock()
	m.scopes = append(m.scopes, scope)
	fn := m.sections
	m.mu.Unlock()
	if fn != nil {
		return fn(scope)
	}
	return "", "", nil
}

// recordedScopes 返回已记录的作用域副本（按调用顺序）。
func (m *recordingMemory) recordedScopes() []core.Scope {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]core.Scope(nil), m.scopes...)
}

// newAgentMemoryServer 装配带记录型记忆端口的 handler 级服务。
func newAgentMemoryServer(t *testing.T, mutate func(*Options)) (*Server, *recordingMemory) {
	t.Helper()
	mem := &recordingMemory{}
	s := newTestServer(t, func(o *Options) {
		o.Memory = mem
		if mutate != nil {
			mutate(o)
		}
	})
	return s, mem
}

// getAgentMemory 请求 `GET /api/v1/agents/{name}/memory`。
func getAgentMemory(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	return serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/agents/"+name+"/memory", ""))
}

// TestAgentMemory_ReadsAgentLayer —— Agent 层两分区原样返回，scope 恰为 `{Agent: name}`、
// 恰好读取一次，backend 取配置值。
// TestAgentMemory_ReadsAgentLayer pins the agent-scope read and the passthrough body.
func TestAgentMemory_ReadsAgentLayer(t *testing.T) {
	s, mem := newAgentMemoryServer(t, nil)
	mem.sections = func(core.Scope) (string, string, error) {
		return "# Core\nAGENT-CORE", "## Archive\nAGENT-ARCH", nil
	}

	rec := getAgentMemory(t, s, "ops-agent")
	require.Equal(t, http.StatusOK, rec.Code)

	env := decodeResourcesEnvelope[MemoryView](t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Equal(t, memoryScopeAgent, env.Data.Scope)
	require.Equal(t, "ops-agent", env.Data.Agent)
	require.Equal(t, "markdown", env.Data.Backend)
	require.Equal(t, "# Core\nAGENT-CORE", env.Data.Core, "分区正文必须原样透传（含标题行）")
	require.Equal(t, "## Archive\nAGENT-ARCH", env.Data.Archive)
	require.Equal(t, []core.Scope{{Agent: "ops-agent"}}, mem.recordedScopes())
}

// TestAgentMemory_EmptyLayerDoesNotFallBack —— Agent 层为空时返回空分区；全局层有内容
// 也不得被回退读取（回退只发生在提示词组装的 LoadEffective）。
// 逆改点：handler 在空分区时改读 GlobalScope → 次数或分区正文断言红。
// TestAgentMemory_EmptyLayerDoesNotFallBack proves the query never falls back.
func TestAgentMemory_EmptyLayerDoesNotFallBack(t *testing.T) {
	s, mem := newAgentMemoryServer(t, nil)
	mem.sections = func(scope core.Scope) (string, string, error) {
		if scope.Agent == "" {
			return "GLOBAL-CORE", "GLOBAL-ARCH", nil
		}
		return "", "", nil
	}

	env := decodeResourcesEnvelope[MemoryView](t, getAgentMemory(t, s, "ops-agent"))
	require.Equal(t, codeOK, env.Code)
	require.Empty(t, env.Data.Core, "空层不得回退全局正文")
	require.Empty(t, env.Data.Archive)
	require.Equal(t, []core.Scope{{Agent: "ops-agent"}}, mem.recordedScopes(),
		"必须恰好一次 Agent 层读取，不得追加全局读取")
}

// TestAgentMemory_UnknownAgentNotFound —— 装载结果里没有该名字 → 404，且不触达记忆端口。
// TestAgentMemory_UnknownAgentNotFound pins the 404 with zero reads.
func TestAgentMemory_UnknownAgentNotFound(t *testing.T) {
	s, mem := newAgentMemoryServer(t, nil)
	rec := getAgentMemory(t, s, "ghost")

	require.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeResourcesEnvelope[map[string]any](t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Contains(t, env.Message, "ghost")
	require.Empty(t, mem.recordedScopes(), "404 路径不得读取记忆")
}

// TestAgentMemory_UnregisteredAgentNotFound —— 装载结果里有但未注册（被拒）的 Agent
// 同样 404：与建会话 / invoke 共用同一处判定。
// TestAgentMemory_UnregisteredAgentNotFound pins the same 404 for rejected entries.
func TestAgentMemory_UnregisteredAgentNotFound(t *testing.T) {
	s, mem := newAgentMemoryServer(t, func(o *Options) {
		o.Agents = fakeAgents{entries: []core.AgentEntry{
			{Name: "ops-agent", Registered: true, Profile: &core.Profile{Name: "ops-agent"}},
			{Name: "broken", Registered: false, RejectReason: "AGENT.md 语法错误"},
		}}
	})
	rec := getAgentMemory(t, s, "broken")

	require.Equal(t, http.StatusNotFound, rec.Code)
	env := decodeResourcesEnvelope[map[string]any](t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Empty(t, mem.recordedScopes(), "未注册 Agent 不得读取记忆")
}

// TestAgentMemory_ReadFailureInternal —— 读取失败回固定 500 文案且不带半份内容；
// 底层错误只进 ERROR 日志，不进入响应。
// 逆改点：把 err.Error() 写进 message → 正文扫描断言红。
// TestAgentMemory_ReadFailureInternal pins the fixed 500 without partial content.
func TestAgentMemory_ReadFailureInternal(t *testing.T) {
	s, mem := newAgentMemoryServer(t, nil)
	mem.sections = func(core.Scope) (string, string, error) {
		return "HALF-CORE", "", errMemoryReadFailed
	}

	rec := getAgentMemory(t, s, "ops-agent")
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	env := decodeResourcesEnvelope[map[string]any](t, rec)
	require.Equal(t, codeInternal, env.Code)
	require.Equal(t, messageInternal, env.Message)
	require.Nil(t, env.Data, "失败响应不得携带半份内容")
	require.NotContains(t, rec.Body.String(), "HALF-CORE")
	require.NotContains(t, rec.Body.String(), errMemoryReadFailed.Error())
}

// TestAgentMemory_ViewShapeMatchesContract —— 真实 HTTP 载荷的字段集与契约
// MemoryView 完全一致并过 schema：内部字段（Agent 目录、路径）没有任何出站载体，
// 这是「响应脱敏」在本端点的可判口径。
// TestAgentMemory_ViewShapeMatchesContract pins the exact contract property set.
func TestAgentMemory_ViewShapeMatchesContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	s, mem := newAgentMemoryServer(t, nil)
	mem.sections = func(core.Scope) (string, string, error) {
		return "core-text", "archive-text", nil
	}

	rec := getAgentMemory(t, s, "ops-agent")
	require.Equal(t, http.StatusOK, rec.Code)

	var env resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	requireResourcePayloadMatches(t, doc, "MemoryView", env.Data)
}
