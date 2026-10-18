package web

// 本文件是 handler 层验收（模块卡 §4.3）：假端口记录每次调用，端到端走真实
// `handler()` 链。辅助件在此定义，contract_test.go / server_test.go 同包共用
// （模块卡 §4.3「辅助件」）。
// Handler-level acceptance with fake ports; shared helpers live here and are reused
// by contract_test.go and server_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeRunService 记录每次运行请求；result / err 由用例预置。
// fakeRunService records every run request.
type fakeRunService struct {
	mu     sync.Mutex
	reqs   []core.RunRequest
	result core.RunResult
	err    error
}

func (f *fakeRunService) Run(_ context.Context, req core.RunRequest) (core.RunResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return f.result, f.err
}

func (f *fakeRunService) requests() []core.RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// fakeSessionStore 是内存版会话仓储；getOrCreateCalls 供「invoke 零建会话」断言。
// mu 保护全部读写：R14 的同步屏障用例会从两个 goroutine 并发访问同一实例
// （`-race` 下裸 map 会红）。
// fakeSessionStore is the in-memory session store; mutex-protected because the
// synchronization-barrier test hits it from two goroutines.
type fakeSessionStore struct {
	mu               sync.Mutex
	sessions         map[string]core.SessionRecord
	messages         map[string][]core.Message
	getOrCreateCalls int
	getSessionCalls  int
}

func (f *fakeSessionStore) GetOrCreateSession(_ context.Context,
	rec core.SessionRecord,
) (core.SessionRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getOrCreateCalls++
	if got, ok := f.sessions[rec.ID]; ok {
		return got, false, nil
	}
	f.sessions[rec.ID] = rec
	return rec, true, nil
}

func (f *fakeSessionStore) GetSession(_ context.Context, id string) (core.SessionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getSessionCalls++
	got, ok := f.sessions[id]
	if !ok {
		return core.SessionRecord{}, fs.ErrNotExist
	}
	return got, nil
}

// getSessionCallCount 供「拒绝路径零副作用」断言：跨源拒绝不得触达会话仓储。
func (f *fakeSessionStore) getSessionCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getSessionCalls
}

// getOrCreateCallCount 供建会话参数校验的「零创建、零修改」断言。
func (f *fakeSessionStore) getOrCreateCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getOrCreateCalls
}

func (f *fakeSessionStore) ArchiveSession(_ context.Context, id string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	got, ok := f.sessions[id]
	if !ok {
		return fs.ErrNotExist
	}
	got.Status = statusArchived
	if got.ArchivedAt == nil {
		got.ArchivedAt = &at
	}
	f.sessions[id] = got
	return nil
}

func (f *fakeSessionStore) MessagesTail(_ context.Context, id string, limit int) ([]core.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.messages[id]
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return slices.Clone(all), nil
}

// seed 供测试预置行；并发用例必须走它（直接写 map 会绕过 mu）。
func (f *fakeSessionStore) seed(rec core.SessionRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[rec.ID] = rec
}

// seedMessages 供「拒绝归档不得改动消息」断言。
func (f *fakeSessionStore) seedMessages(id string, msgs ...core.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[id] = msgs
}

type fakeAgents struct{ entries []core.AgentEntry }

func (f fakeAgents) Profiles() []core.AgentEntry { return f.entries }

type fakeTools struct{ tools []ToolDescriptor }

func (f fakeTools) Tools() []ToolDescriptor { return f.tools }

type fakeMemory struct {
	coreText, archiveText string
	err                   error
}

func (f fakeMemory) Sections(context.Context, core.Scope) (string, string, error) {
	return f.coreText, f.archiveText, f.err
}

// newTestServer 是 handler 级用例的统一装配：固定时钟 2026-10-18T12:00:00Z、
// 丢弃日志、内存仓储；mutate 覆盖单个用例关心的端口。
// newTestServer assembles a handler-level server with a fixed clock.
func newTestServer(t *testing.T, mutate func(*Options)) *Server {
	t.Helper()
	opts := Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) },
		Version:       "test",
		MemoryBackend: "markdown",
		Runs:          &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}},
		Sessions:      &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}},
		Agents:        fakeAgents{entries: []core.AgentEntry{{Name: "ops-agent", Registered: true, Profile: &core.Profile{Name: "ops-agent"}}}},
		Tools:         fakeTools{},
		Memory:        fakeMemory{},
	}
	if mutate != nil {
		mutate(&opts)
	}
	srv, err := NewServer(opts)
	require.NoError(t, err)
	return srv
}

// TestSessions_CreateDefaultsAndIdempotency 覆盖默认值、显式值、幂等与两条 400 分支
// （模块卡 §4.1 handlers 表第 1 行，6 例）。
// 逆改点：默认值写错 / 幂等重写 created_at / 校验晚于写入 → 对应断言红。
func TestSessions_CreateDefaultsAndIdempotency(t *testing.T) {
	post := func(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/sessions", strings.NewReader(body)))
		return rec
	}

	t.Run("默认 channel 与 user_id，status 显式 active", func(t *testing.T) {
		s := newTestServer(t, nil)
		rec := post(t, s, `{"agent":"ops-agent"}`)
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Code string             `json:"code"`
			Data SessionSummaryView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, channelWeb, env.Data.Channel)
		require.Equal(t, defaultUserID, env.Data.UserID)
		require.Equal(t, statusActive, env.Data.Status,
			"新建会话的 status 必须显式 active（不依赖 SQLite DEFAULT 回读）")
		require.Equal(t, core.DeriveSessionID(channelWeb, defaultUserID, "ops-agent"), env.Data.SessionID)
		require.Equal(t, "2026-10-18T12:00:00Z", env.Data.CreatedAt)
	})

	t.Run("显式 channel 与 user_id", func(t *testing.T) {
		s := newTestServer(t, nil)
		rec := post(t, s, `{"agent":"ops-agent","channel":"crm","user_id":"u-7"}`)
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Data SessionSummaryView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, "crm", env.Data.Channel)
		require.Equal(t, "u-7", env.Data.UserID)
		require.Equal(t, core.DeriveSessionID("crm", "u-7", "ops-agent"), env.Data.SessionID)
	})

	t.Run("同元组重复创建幂等", func(t *testing.T) {
		s := newTestServer(t, nil)
		first := post(t, s, `{"agent":"ops-agent"}`)
		second := post(t, s, `{"agent":"ops-agent"}`)
		require.Equal(t, http.StatusOK, first.Code)
		require.Equal(t, http.StatusOK, second.Code)

		var a, b struct {
			Data SessionSummaryView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(first.Body.Bytes(), &a))
		require.NoError(t, json.Unmarshal(second.Body.Bytes(), &b))
		require.Equal(t, a.Data.SessionID, b.Data.SessionID)
		require.Equal(t, a.Data.CreatedAt, b.Data.CreatedAt, "幂等返回不得改写 created_at")
		require.Equal(t, 2, s.opts.Sessions.(*fakeSessionStore).getOrCreateCallCount())
	})

	t.Run("未注册 Agent 返回 404", func(t *testing.T) {
		s := newTestServer(t, nil)
		rec := post(t, s, `{"agent":"ghost"}`)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Contains(t, rec.Body.String(), codeNotFound)
		require.Contains(t, rec.Body.String(), "ghost")
	})

	t.Run("agent 缺失返回 400 且零创建", func(t *testing.T) {
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		s := newTestServer(t, func(o *Options) { o.Sessions = store })
		rec := post(t, s, `{}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), codeBadRequest)
		require.Contains(t, rec.Body.String(), "agent 必填")
		require.Equal(t, 0, store.getOrCreateCallCount(), "参数校验失败不得触达会话写入")
	})

	t.Run("agent 为 null 返回 400 且零创建", func(t *testing.T) {
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		s := newTestServer(t, func(o *Options) { o.Sessions = store })
		rec := post(t, s, `{"agent":null}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "agent 必填")
		require.Equal(t, 0, store.getOrCreateCallCount(), "null 与缺失同判，且零写入")
	})
}

// TestSessions_GetDetailWindow 覆盖最近 100 条窗口、升序、created_at 回填与归档可读
// （模块卡 §4.1 handlers 表第 2 行，4 例）。
// 逆改点：窗口取最旧 100 条 / 顺序反转 / 不选 created_at / 归档被拒 → 对应断言红。
func TestSessions_GetDetailWindow(t *testing.T) {
	seed := func(store *fakeSessionStore, status string, count int) {
		rec := core.SessionRecord{
			ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: status,
			CreatedAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		}
		store.seed(rec)
		msgs := make([]core.Message, 0, count)
		for i := range count {
			msgs = append(msgs, core.Message{
				Role:      "user",
				Content:   msgContent(i),
				CreatedAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second),
			})
		}
		store.seedMessages("web-x", msgs...)
	}
	get := func(s *Server) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/web-x", nil))
		return rec
	}
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) SessionDetailView {
		t.Helper()
		var env struct {
			Data SessionDetailView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		return env.Data
	}

	t.Run("最近 100 条窗口", func(t *testing.T) {
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		seed(store, statusActive, 120)
		s := newTestServer(t, func(o *Options) { o.Sessions = store })
		detail := decode(t, get(s))
		require.Len(t, detail.Messages, historyLimit)
		require.Equal(t, msgContent(20), detail.Messages[0].Content, "窗口必须从最新的 100 条开始")
		require.Equal(t, msgContent(119), detail.Messages[99].Content)
	})

	t.Run("按 seq 升序", func(t *testing.T) {
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		seed(store, statusActive, 3)
		s := newTestServer(t, func(o *Options) { o.Sessions = store })
		detail := decode(t, get(s))
		require.Len(t, detail.Messages, 3)
		for i, m := range detail.Messages {
			require.Equal(t, msgContent(i), m.Content)
		}
	})

	t.Run("created_at 回填为 RFC3339 UTC", func(t *testing.T) {
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		seed(store, statusActive, 1)
		s := newTestServer(t, func(o *Options) { o.Sessions = store })
		detail := decode(t, get(s))
		require.Len(t, detail.Messages, 1)
		require.Equal(t, "2026-10-18T12:00:00Z", detail.Messages[0].CreatedAt)
	})

	t.Run("归档会话可读", func(t *testing.T) {
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		seed(store, statusArchived, 2)
		s := newTestServer(t, func(o *Options) { o.Sessions = store })
		rec := get(s)
		require.Equal(t, http.StatusOK, rec.Code)
		detail := decode(t, rec)
		require.Equal(t, statusArchived, detail.Status)
		require.Len(t, detail.Messages, 2)
	})
}

// TestMeta_HealthInfo 覆盖 health / info 的只读读数与凭证面（模块卡 §4.1 handlers
// 表第 9 行，4 例；约束 25）。
// 逆改点：agents 用 len(Profiles()) 计数（含未注册）→ agents==1 断言红；mcp_servers
// 硬编码 0 → 非零断言红；ProviderInfo 加回凭证字段 → 无凭证断言红。
func TestMeta_HealthInfo(t *testing.T) {
	get := func(s *Server, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	entries := []core.AgentEntry{
		{Name: "ops-agent", Registered: true, Profile: &core.Profile{Name: "ops-agent"}},
		{Name: "ghost", Registered: false, RejectReason: "缺 system prompt"},
	}
	infoSrv := newTestServer(t, func(o *Options) {
		o.Agents = fakeAgents{entries: entries}
		o.Version = "9.0.0"
		o.MCPConnected = 2
		o.StartedAt = time.Date(2026, 10, 18, 11, 59, 30, 0, time.UTC)
		o.Providers = []ProviderInfo{{Name: "openai", Models: []string{"gpt-5"}, Configured: true}}
	})

	t.Run("health", func(t *testing.T) {
		rec := get(newTestServer(t, nil), "/api/v1/health")
		require.Equal(t, http.StatusOK, rec.Code)
		var env struct {
			Code string `json:"code"`
			Data struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, "ok", env.Data.Status)
	})

	t.Run("info 只数已注册 Agent 且 MCP 计数非零", func(t *testing.T) {
		rec := get(infoSrv, "/api/v1/info")
		require.Equal(t, http.StatusOK, rec.Code)
		var env struct {
			Data InfoView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, "9.0.0", env.Data.Version)
		require.EqualValues(t, 30, env.Data.UptimeSeconds)
		require.Equal(t, 1, env.Data.Agents, "只数 Registered == true 的条目")
		require.Equal(t, 2, env.Data.MCPServers, "MCP 连接数必须来自装配，不得硬编码 0")
		require.Equal(t, 0, env.Data.Tools)
		require.Len(t, env.Data.Providers, 1)
		require.Equal(t, "openai", env.Data.Providers[0].Name)
		require.Equal(t, []string{"gpt-5"}, env.Data.Providers[0].Models)
		require.True(t, env.Data.Providers[0].Configured)
	})

	t.Run("profiles 返回全部装载条目", func(t *testing.T) {
		rec := get(infoSrv, "/api/v1/profiles")
		require.Equal(t, http.StatusOK, rec.Code)
		var env struct {
			Data []ProfileView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Len(t, env.Data, 2, "/info 的 agents 与 /profiles 的条目数是两个口径")
	})

	t.Run("provider 摘要无凭证字段", func(t *testing.T) {
		rec := get(infoSrv, "/api/v1/info")
		body := rec.Body.String()
		require.NotContains(t, body, "api_key")
		require.NotContains(t, body, "base_url")
		require.NotContains(t, body, "sk-")

		var env struct {
			Data struct {
				Providers []map[string]json.RawMessage `json:"providers"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Len(t, env.Data.Providers, 1)
		keys := make([]string, 0, len(env.Data.Providers[0]))
		for k := range env.Data.Providers[0] {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		require.Equal(t, []string{"configured", "models", "name"}, keys,
			"provider 摘要的字段集固定为契约声明的三项")
	})
}

// TestMeta_ProfilesToolsMemory 覆盖 profiles 顺序与 problems、tools 三来源与顺序、
// memory 分区 / backend 与读取失败（模块卡 §4.1 handlers 表第 10 行，7 例；
// 约束 26 / 27 / 28）。
// 逆改点：profiles 重排或未注册项 problems 为空 / tools 按名字猜来源 / memory 空
// 分区被包上标题 / 读取失败回 200 → 对应断言红。
func TestMeta_ProfilesToolsMemory(t *testing.T) {
	get := func(s *Server, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	entries := []core.AgentEntry{
		{
			Name:       "ops-agent",
			Registered: true,
			Profile: &core.Profile{
				Name:        "ops-agent",
				Description: "运维助手",
				Provider:    core.ProviderRef{Name: "openai", Model: "gpt-5"},
				Tools:       []string{"fs_read"},
				Skills:      []string{"diagnose"},
				Schedules: []core.Schedule{
					{ID: "s1"}, {ID: "s2"},
				},
			},
		},
		{Name: "ghost", Registered: false, RejectReason: "缺少 system prompt"},
		{Name: "pending", Registered: false},
	}
	profilesSrv := newTestServer(t, func(o *Options) { o.Agents = fakeAgents{entries: entries} })

	t.Run("profiles 按装载顺序", func(t *testing.T) {
		rec := get(profilesSrv, "/api/v1/profiles")
		require.Equal(t, http.StatusOK, rec.Code)
		var env struct {
			Data []ProfileView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Len(t, env.Data, 3)
		require.Equal(t, []string{"ops-agent", "ghost", "pending"},
			[]string{env.Data[0].Name, env.Data[1].Name, env.Data[2].Name})
		require.True(t, env.Data[0].Registered)
		require.Equal(t, "运维助手", env.Data[0].Description)
		require.Equal(t, "openai", env.Data[0].Provider)
		require.Equal(t, "gpt-5", env.Data[0].Model)
		require.Equal(t, []string{"fs_read"}, env.Data[0].Tools)
		require.Equal(t, []string{"diagnose"}, env.Data[0].Skills)
		require.Equal(t, 2, env.Data[0].Schedules)
		require.Empty(t, env.Data[0].Problems)
	})

	t.Run("未注册项 problems 恰含拒因", func(t *testing.T) {
		var env struct {
			Data []ProfileView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(get(profilesSrv, "/api/v1/profiles").Body.Bytes(), &env))
		require.False(t, env.Data[1].Registered)
		require.Equal(t, []string{"缺少 system prompt"}, env.Data[1].Problems,
			"registered=false 时 problems 恰含 RejectReason 一项")
	})

	t.Run("Profile 为 nil 时派生字段为空且数组为 []", func(t *testing.T) {
		rec := get(profilesSrv, "/api/v1/profiles")
		require.Contains(t, rec.Body.String(), `"problems":[]`)
		var env struct {
			Data []ProfileView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, ProfileView{Name: "pending", Tools: []string{}, Skills: []string{}, Problems: []string{}},
			env.Data[2])
	})

	toolsSrv := newTestServer(t, func(o *Options) {
		o.Tools = fakeTools{tools: []ToolDescriptor{
			{Spec: core.ToolSpec{Name: "alpha_builtin", Description: "内置"}, Source: "builtin"},
			{Spec: core.ToolSpec{Name: "beta_mcp", Description: "MCP"}, Source: "mcp"},
			{Spec: core.ToolSpec{Name: "gamma_code", Description: "代码注册"}, Source: "code"},
		}}
	})

	t.Run("tools 按注册表顺序", func(t *testing.T) {
		rec := get(toolsSrv, "/api/v1/tools")
		require.Equal(t, http.StatusOK, rec.Code)
		var env struct {
			Data []ToolView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Len(t, env.Data, 3)
		require.Equal(t, "alpha_builtin", env.Data[0].Name)
		require.Equal(t, "beta_mcp", env.Data[1].Name)
		require.Equal(t, "gamma_code", env.Data[2].Name)
	})

	t.Run("tools 三来源", func(t *testing.T) {
		var env struct {
			Data []ToolView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(get(toolsSrv, "/api/v1/tools").Body.Bytes(), &env))
		require.Equal(t, []string{"builtin", "mcp", "code"},
			[]string{env.Data[0].Source, env.Data[1].Source, env.Data[2].Source})
		require.Equal(t, "内置", env.Data[0].Description)
		require.Equal(t, "MCP", env.Data[1].Description)
	})

	t.Run("memory 两分区与 backend", func(t *testing.T) {
		memSrv := newTestServer(t, func(o *Options) {
			o.MemoryBackend = "sqlite"
			o.Memory = fakeMemory{coreText: "## 核心记忆\n- a", archiveText: "## 归档记忆\n- b"}
		})
		rec := get(memSrv, "/api/v1/memory")
		require.Equal(t, http.StatusOK, rec.Code)
		var env struct {
			Data MemoryView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, "global", env.Data.Scope)
		require.Empty(t, env.Data.Agent)
		require.Equal(t, "sqlite", env.Data.Backend)
		require.Equal(t, "## 核心记忆\n- a", env.Data.Core)
		require.Equal(t, "## 归档记忆\n- b", env.Data.Archive)
	})

	t.Run("memory 读取失败 500", func(t *testing.T) {
		memSrv := newTestServer(t, func(o *Options) {
			o.Memory = fakeMemory{err: errors.New("read MEMORY.md: disk on fire")}
		})
		rec := get(memSrv, "/api/v1/memory")
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Contains(t, rec.Body.String(), codeInternal)
		require.Contains(t, rec.Body.String(), messageInternal)
		require.NotContains(t, rec.Body.String(), "disk on fire", "底层错误不得透出")
	})
}

// msgContent 渲染测试消息正文（m000…m119）：序号参与断言，必须稳定。
// msgContent renders a fixed-width fixture body.
func msgContent(i int) string { return fmt.Sprintf("m%03d", i) }

// TestMessages_SuccessRunView 覆盖 completed 的 200 视图字段与运行请求构造
// （模块卡 §4.1 handlers 表第 4 行，2 例；约束 9 的渠道 / 触发固定）。
// 逆改点：视图用会话列 channel 而非固定 web / trigger 写错 / session_id 取自 RunResult
// → 对应字段断言红。
func TestMessages_SuccessRunView(t *testing.T) {
	runs := &fakeRunService{result: core.RunResult{
		RunID:      "run-1",
		Status:     core.StatusCompleted,
		FinalText:  "done",
		Iterations: 2,
		ToolCalls:  1,
		ConfigHash: "cfg-1",
	}}
	store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
	store.seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: "crm", UserID: "u-7", Status: statusActive,
	})
	s := newTestServer(t, func(o *Options) { o.Runs = runs; o.Sessions = store })

	post := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))
		return rec
	}

	t.Run("completed 视图字段", func(t *testing.T) {
		rec := post(t)
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Code string  `json:"code"`
			Data RunView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, "run-1", env.Data.RunID)
		require.Equal(t, "web-x", env.Data.SessionID)
		require.Equal(t, "ops-agent", env.Data.Agent)
		require.Equal(t, channelWeb, env.Data.Channel, "REST 运行渠道固定 web，不取会话列")
		require.Equal(t, triggerAPI, env.Data.Trigger)
		require.Equal(t, "completed", env.Data.Status)
		require.Equal(t, "done", env.Data.FinalText)
		require.Equal(t, 2, env.Data.Iterations)
		require.Equal(t, 1, env.Data.ToolCalls)
		require.Equal(t, "cfg-1", env.Data.ConfigHash)
		require.Equal(t, "2026-10-18T12:00:00Z", env.Data.StartedAt)
		require.Equal(t, "2026-10-18T12:00:00Z", env.Data.FinishedAt)
	})

	t.Run("运行请求携带会话上下文", func(t *testing.T) {
		_ = post(t)
		reqs := runs.requests()
		require.NotEmpty(t, reqs)
		req := reqs[len(reqs)-1] // 前一个子用例已跑过一次，取本次
		require.Equal(t, "ops-agent", req.Agent, "Agent 取会话行的 ProfileName")
		require.Equal(t, "web-x", req.SessionID)
		require.Equal(t, "hi", req.Message)
		require.Equal(t, channelWeb, req.Channel)
		require.Equal(t, "u-7", req.UserID, "UserID 取会话行，不取请求体")
	})
}

// TestMessages_RequestValidation 覆盖解码与入口校验的七条拒绝路径（模块卡 §4.1
// handlers 表第 5 行；约束 16 / 33）。每条拒绝都必须发生在运行入口之前。
// 逆改点：尾随内容被静默接受 / null 与缺失不分 / 请求体超限返回 413 → 对应断言红。
func TestMessages_RequestValidation(t *testing.T) {
	newWithRunSpy := func(t *testing.T) (*Server, *fakeRunService, *fakeSessionStore) {
		t.Helper()
		runs := &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		s := newTestServer(t, func(o *Options) { o.Runs = runs; o.Sessions = store })
		return s, runs, store
	}
	post := func(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/sessions/web-x/messages", strings.NewReader(body)))
		return rec
	}

	t.Run("message 缺失", func(t *testing.T) {
		s, runs, _ := newWithRunSpy(t)
		rec := post(t, s, `{}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "message 必填")
		require.Empty(t, runs.requests(), "校验失败不得进入运行")
	})

	t.Run("message 为 null", func(t *testing.T) {
		s, runs, _ := newWithRunSpy(t)
		rec := post(t, s, `{"message":null}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "message 必填")
		require.Empty(t, runs.requests())
	})

	t.Run("非法 JSON", func(t *testing.T) {
		s, runs, _ := newWithRunSpy(t)
		rec := post(t, s, `{"message":`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "请求体不是合法 JSON 对象")
		require.Empty(t, runs.requests())
	})

	t.Run("尾随内容", func(t *testing.T) {
		s, runs, _ := newWithRunSpy(t)
		rec := post(t, s, `{"message":"hi"}garbage`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "请求体不是合法 JSON 对象")
		require.Empty(t, runs.requests(), "解析后必须读到 EOF")
	})

	t.Run("32 KiB 超限", func(t *testing.T) {
		s, runs, _ := newWithRunSpy(t)
		body, err := json.Marshal(map[string]string{"message": strings.Repeat("a", maxMessageBytes+1)})
		require.NoError(t, err)
		rec := post(t, s, string(body))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "消息超过 32768 字节上限")
		require.Empty(t, runs.requests())
	})

	t.Run("归档会话", func(t *testing.T) {
		s, runs, store := newWithRunSpy(t)
		store.seed(core.SessionRecord{
			ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusArchived,
		})
		rec := post(t, s, `{"message":"hi"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "已归档")
		require.Empty(t, runs.requests(), "归档会话不得进入运行入口")
	})

	t.Run("请求体超 max_request_bytes", func(t *testing.T) {
		runs := &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
		s := newTestServer(t, func(o *Options) {
			o.Runs = runs
			o.MaxRequestBytes = 64
		})
		body, err := json.Marshal(map[string]string{"message": strings.Repeat("a", 200)})
		require.NoError(t, err)
		rec := post(t, s, string(body))
		require.Equal(t, http.StatusBadRequest, rec.Code, "契约要求 400，不是 net/http 惯用的 413")
		require.Contains(t, rec.Body.String(), "请求体超过 64 字节上限")
		require.Empty(t, runs.requests())
	})
}

// TestMessages_MessageCapCountsBytesBeforeRun 逐字保留模块卡 R3 的判定（W4）。
// 逆改点：用 utf8.RuneCountInString 计数 → 10923 个三字节字符（32769 字节）被误判放行
// （10923 个 rune ≤ 32768），"32769 字节拒绝"的中文用例红；把上限判定挪到 Run 之后 →
// over-limit 请求进入 runs 记录，调用次数断言红。
func TestMessages_MessageCapCountsBytesBeforeRun(t *testing.T) {
	runs := &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
	s := newTestServer(t, func(o *Options) { o.Runs = runs })
	store := s.opts.Sessions.(*fakeSessionStore)
	store.seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})

	post := func(msg string) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]string{"message": msg})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/web-x/messages", bytes.NewReader(body))
		s.handler().ServeHTTP(rec, req)
		return rec
	}

	require.Equal(t, http.StatusOK, post(strings.Repeat("宽", maxMessageBytes/3)).Code,
		"32766 字节（10922 个三字节字符）放行")
	require.Equal(t, http.StatusOK, post(strings.Repeat("a", maxMessageBytes)).Code,
		"恰好 32768 字节放行")
	require.Equal(t, http.StatusBadRequest, post(strings.Repeat("宽", maxMessageBytes/3+1)).Code,
		"32769 字节（10923 个三字节字符）拒绝——按字节而非按字符计数")
	require.Equal(t, http.StatusBadRequest, post(strings.Repeat("a", maxMessageBytes+1)).Code,
		"32769 字节拒绝")
	require.Len(t, runs.requests(), 2, "超限请求一次都不得进入运行入口")
}

// TestSessions_ArchiveSemantics 覆盖归档的状态与时间、幂等、消息保留、成功无 data、
// 未知 404（模块卡 §4.1 handlers 表第 3 行，5 例；约束 3 / 8 / 14）。
// 逆改点：重复归档改写 archived_at / 成功响应多出 data / 归档删消息 → 对应断言红。
func TestSessions_ArchiveSemantics(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	newStore := func() *fakeSessionStore {
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		store.seed(core.SessionRecord{
			ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
			CreatedAt: now,
		})
		store.seedMessages("web-x", core.Message{Role: "user", Content: "before"})
		return store
	}
	delete := func(s *Server, id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+id, nil))
		return rec
	}

	t.Run("状态与首次归档时间", func(t *testing.T) {
		store := newStore()
		s := newTestServer(t, func(o *Options) {
			o.Sessions = store
			o.Now = func() time.Time { return now }
		})
		rec := delete(s, "web-x")
		require.Equal(t, http.StatusOK, rec.Code)

		got, err := store.GetSession(context.Background(), "web-x")
		require.NoError(t, err)
		require.Equal(t, statusArchived, got.Status)
		require.NotNil(t, got.ArchivedAt)
		require.Equal(t, now, *got.ArchivedAt)
	})

	t.Run("重复归档幂等", func(t *testing.T) {
		store := newStore()
		s := newTestServer(t, func(o *Options) {
			o.Sessions = store
			o.Now = func() time.Time { return now }
		})
		require.Equal(t, http.StatusOK, delete(s, "web-x").Code)
		now = now.Add(time.Hour)
		require.Equal(t, http.StatusOK, delete(s, "web-x").Code, "重复 DELETE 是幂等成功")

		got, err := store.GetSession(context.Background(), "web-x")
		require.NoError(t, err)
		require.Equal(t, statusArchived, got.Status)
		require.NotNil(t, got.ArchivedAt)
		require.Equal(t, now.Add(-time.Hour), *got.ArchivedAt, "archived_at 只在首次归档写入")
	})

	t.Run("消息保留", func(t *testing.T) {
		store := newStore()
		s := newTestServer(t, func(o *Options) { o.Sessions = store })
		require.Equal(t, http.StatusOK, delete(s, "web-x").Code)
		msgs, err := store.MessagesTail(context.Background(), "web-x", 0)
		require.NoError(t, err)
		require.Len(t, msgs, 1, "归档不删消息")
	})

	t.Run("成功响应不含 data 键", func(t *testing.T) {
		store := newStore()
		s := newTestServer(t, func(o *Options) { o.Sessions = store })
		rec := delete(s, "web-x")
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `"code":"OK"`)
		require.Contains(t, rec.Body.String(), `"message":"success"`)
		require.NotContains(t, rec.Body.String(), `"data"`, "无载荷成功不发明归档字段（注 4）")
	})

	t.Run("未知会话 404", func(t *testing.T) {
		s := newTestServer(t, nil)
		rec := delete(s, "nope")
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Contains(t, rec.Body.String(), codeNotFound)
		require.Contains(t, rec.Body.String(), "nope")
	})
}

// TestMessages_AdmissionErrorMapping 逐字保留模块卡 R4（W8）。
// 逆改点：改一个标点（409）→ 字面量断言红；429 不写 Limit → 含 "7" 断言红。
func TestMessages_AdmissionErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantSubstr string
	}{
		{
			"同会话忙", &core.SessionBusyError{SessionID: "web-x"}, http.StatusConflict,
			"SESSION_BUSY", "该会话有正在执行的任务，请等待完成或稍后重试",
		},
		{
			"实例满", &core.InstanceFullError{Limit: 7}, http.StatusTooManyRequests,
			"TOO_MANY_RUNS", "7",
		},
		{
			"Agent 消失", &core.AgentNotFoundError{Name: "ops-agent"}, http.StatusNotFound,
			"NOT_FOUND", "ops-agent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := &fakeRunService{err: tc.err}
			s := newTestServer(t, func(o *Options) { o.Runs = runs })
			s.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
				ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
			})
			rec := httptest.NewRecorder()
			body := strings.NewReader(`{"message":"hi"}`)
			s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/api/v1/sessions/web-x/messages", body))
			require.Equal(t, tc.wantStatus, rec.Code)
			require.Contains(t, rec.Body.String(), tc.wantCode)
			require.Contains(t, rec.Body.String(), tc.wantSubstr)
		})
	}
}

// TestMessages_RunStatusMapping 逐字保留模块卡 R5（W9）。
// 逆改点：把 LIMIT_REACHED 映射成 4xx → 状态断言红；message 不点名配置键 → 含
// "budget.max_iterations" 断言红；把 deadline 映射成 500 → 状态断言红；把 failed
// 映射成 500 → 状态断言红（契约只有 200/400/404/409/429/504）；failed 分支把原始
// Reason 直出 → 「运行失败」与 NotContains 断言红（完整判别力夹具见 R15）。
func TestMessages_RunStatusMapping(t *testing.T) {
	limitSrv := newTestServer(t, func(o *Options) {
		o.Runs = &fakeRunService{result: core.RunResult{
			RunID: "run-1", Status: core.StatusLimitReached, Reason: core.ReasonIterationLimit,
		}}
	})
	limitSrv.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	rec := httptest.NewRecorder()
	limitSrv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"LIMIT_REACHED"`)
	require.Contains(t, rec.Body.String(), `"status":"limit_reached"`)
	require.Contains(t, rec.Body.String(), "budget.max_iterations",
		"LIMIT_REACHED 的 message 必须点名可调配置键（run-and-errors §8）")

	failedSrv := newTestServer(t, func(o *Options) {
		o.Runs = &fakeRunService{result: core.RunResult{
			RunID: "run-1", Status: core.StatusFailed, Reason: "模型调用失败",
		}}
	})
	failedSrv.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	rec = httptest.NewRecorder()
	failedSrv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"status":"failed"`)
	require.Contains(t, rec.Body.String(), "运行失败", "对外 reason 必须是固定公开文案")
	require.NotContains(t, rec.Body.String(), "模型调用失败", "原始原因不得透出")

	timeoutSrv := newTestServer(t, func(o *Options) {
		o.RunTimeout = 10 * time.Millisecond
		o.Runs = deadlineRunService{}
	})
	timeoutSrv.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	rec = httptest.NewRecorder()
	timeoutSrv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"TIMEOUT"`)
}

// deadlineRunService 等到请求截止后返回 cancelled，模拟运行入口的超时收尾。
type deadlineRunService struct{}

func (deadlineRunService) Run(ctx context.Context, _ core.RunRequest) (core.RunResult, error) {
	<-ctx.Done()
	return core.RunResult{RunID: "run-1", Status: core.StatusCancelled, Reason: "超过运行时限"}, nil
}

// TestInvoke_Stateless 逐字保留模块卡 R6（W7），并补未知 Agent 与 32 KiB 两条拒绝路径。
// 逆改点：给 invoke 填一个派生会话 id → SessionID 断言红；调用 GetOrCreateSession →
// 调用计数断言红；写入会话历史 → 消息断言红。
func TestInvoke_Stateless(t *testing.T) {
	runs := &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted, FinalText: "ok"}}
	store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
	s := newTestServer(t, func(o *Options) { o.Runs = runs; o.Sessions = store })

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/ops-agent/invoke", strings.NewReader(`{"message":"巡检"}`)))
	require.Equal(t, http.StatusOK, rec.Code)

	reqs := runs.requests()
	require.Len(t, reqs, 1)
	require.Equal(t, "", reqs[0].SessionID, "无状态调用的 SessionID 必须是空串")
	require.Equal(t, channelWeb, reqs[0].Channel)
	require.Equal(t, defaultUserID, reqs[0].UserID)
	require.Equal(t, 0, store.getOrCreateCallCount(), "invoke 不得建会话")
	require.Empty(t, store.messages, "invoke 不得写会话历史")
	require.Contains(t, rec.Body.String(), `"session_id":""`)
	require.Contains(t, rec.Body.String(), `"trigger":"api"`)

	t.Run("未知 Agent 404", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/agents/ghost/invoke", strings.NewReader(`{"message":"hi"}`)))
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Contains(t, rec.Body.String(), "ghost")
		require.Len(t, runs.requests(), 1, "未知 Agent 不得进入运行入口")
	})

	t.Run("32 KiB 超限 400", func(t *testing.T) {
		body, err := json.Marshal(map[string]string{"message": strings.Repeat("a", maxMessageBytes+1)})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/agents/ops-agent/invoke", bytes.NewReader(body)))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "消息超过 32768 字节上限")
		require.Len(t, runs.requests(), 1, "invoke 与 message 同受此上限，且判定先于运行")
	})
}

// TestMessages_RunBudgetPropagation 验证预算显式传导（约束 35）：message 与 invoke
// 两条入口的 Budget 都必须取 Options.RunBudget。
// 逆改点：任一入口漏带 Budget（或依赖 AgentService 硬编码缺省）→ MaxIterations 断言红。
func TestMessages_RunBudgetPropagation(t *testing.T) {
	budget := core.Budget{MaxIterations: 2, MaxToolCalls: 3}
	runs := &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
	store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
	store.seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	s := newTestServer(t, func(o *Options) { o.Runs = runs; o.Sessions = store; o.RunBudget = budget })

	t.Run("message 分支", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))
		require.Equal(t, http.StatusOK, rec.Code)
		reqs := runs.requests()
		require.Len(t, reqs, 1)
		require.Equal(t, 2, reqs[0].Budget.MaxIterations)
		require.Equal(t, 3, reqs[0].Budget.MaxToolCalls)
	})

	t.Run("invoke 分支", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/agents/ops-agent/invoke", strings.NewReader(`{"message":"hi"}`)))
		require.Equal(t, http.StatusOK, rec.Code)
		reqs := runs.requests()
		require.Len(t, reqs, 2)
		require.Equal(t, 2, reqs[1].Budget.MaxIterations)
		require.Equal(t, 3, reqs[1].Budget.MaxToolCalls)
	})
}

// TestMessages_ClientDisconnectWritesNothing 验证客户端断开后零响应字节（W4）：
// 写已断开的连接只会得到 broken pipe，HTTP 层必须直接返回。
// 逆改点：Run 返回后先写响应再判 r.Context() → 响应字节数断言红。
func TestMessages_ClientDisconnectWritesNothing(t *testing.T) {
	store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
	store.seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newTestServer(t, func(o *Options) {
		o.Sessions = store
		o.Runs = cancellingRunService{cancel: cancel}
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`))
	s.handler().ServeHTTP(rec, req.WithContext(ctx))

	require.Zero(t, rec.Body.Len(), "客户端离开后不得写任何响应字节")
	require.Empty(t, rec.Header().Get("Content-Type"), "连响应头都不得落盘")
}

// cancellingRunService 在运行期间取消请求上下文，模拟客户端断开。
type cancellingRunService struct{ cancel context.CancelFunc }

func (c cancellingRunService) Run(context.Context, core.RunRequest) (core.RunResult, error) {
	c.cancel()
	return core.RunResult{RunID: "run-1", Status: core.StatusCompleted}, nil
}

// TestSessions_ArchiveRejectedWhileRunInflight 逐字保留模块卡 R14（W6/W8）：
// 屏障建在准入窗口内，不靠 sleep 竞猜。
// 逆改点：状态复核 / 计数与归档拆成无锁两步 → DELETE 会在 POST 还卡在 GetSession 时
// 就完成归档（100ms 内完成的断言红），或运行中途被归档（状态断言红）；
// archived 检查挪出 admitRun → 第二个子用例的零运行断言红；
// releaseRun 只减不删 → 末尾 len(inflightRuns) 断言红。
func TestSessions_ArchiveRejectedWhileRunInflight(t *testing.T) {
	seed := core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	}

	t.Run("运行先：准入锁挡住 DELETE，放行后 409，会话保持 active", func(t *testing.T) {
		runs := &blockingRunService{started: make(chan struct{}), release: make(chan struct{})}
		store := &gatedSessionStore{
			fakeSessionStore: &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}},
			entered:          make(chan struct{}),
			release:          make(chan struct{}),
		}
		store.fakeSessionStore.seed(seed)
		store.seedMessages("web-x", core.Message{Role: "user", Content: "before"})
		s := newTestServer(t, func(o *Options) { o.Runs = runs; o.Sessions = store })

		postDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))
			postDone <- rec
		}()
		<-store.entered // POST 已持有 admissionMu，卡在 GetSession 内

		deleteDone := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/web-x", nil))
			deleteDone <- rec
		}()
		select {
		case <-deleteDone:
			t.Fatal("DELETE 在运行准入窗口内完成——状态复核与归档未互斥")
		case <-time.After(100 * time.Millisecond):
			// 预期：DELETE 被准入锁挡住
		}

		close(store.release) // 放行 POST：计数登记 → 运行启动
		<-runs.started

		delRec := <-deleteDone
		require.Equal(t, http.StatusConflict, delRec.Code, "在途运行期间归档必须 409")
		require.Contains(t, delRec.Body.String(), `"code":"SESSION_BUSY"`)
		got, err := s.opts.Sessions.GetSession(context.Background(), "web-x")
		require.NoError(t, err)
		require.Equal(t, statusActive, got.Status, "归档被拒绝后会话必须保持 active")
		require.Nil(t, got.ArchivedAt, "归档被拒绝后 archived_at 不得写入")
		msgs, err := s.opts.Sessions.MessagesTail(context.Background(), "web-x", 0)
		require.NoError(t, err)
		require.Len(t, msgs, 1, "归档被拒绝后消息不得变化")

		close(runs.release)
		postRec := <-postDone
		require.Equal(t, http.StatusOK, postRec.Code, "已准入运行必须继续到底")
		s.admissionMu.Lock()
		require.Zero(t, len(s.inflightRuns), "运行结束后 inflightRuns 不得留下零值项")
		s.admissionMu.Unlock()
	})

	t.Run("归档先：POST 400，零运行", func(t *testing.T) {
		runs := &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
		s := newTestServer(t, func(o *Options) { o.Runs = runs })
		s.opts.Sessions.(*fakeSessionStore).seed(seed)

		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/web-x", nil))
		require.Equal(t, http.StatusOK, rec.Code)

		rec = httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
			"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "已归档")
		require.Empty(t, runs.requests(), "归档会话不得进入运行入口")
	})
}

// gatedSessionStore 让**第一次** GetSession 在返回前停住：POST 的 admitRun 已持有
// admissionMu、尚未登记计数。fake 的 mu 已在 GetSession 返回前释放，屏障只卡调用方。
type gatedSessionStore struct {
	*fakeSessionStore
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedSessionStore) GetSession(ctx context.Context, id string) (core.SessionRecord, error) {
	rec, err := g.fakeSessionStore.GetSession(ctx, id)
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return rec, err
}

// blockingRunService 卡住运行直到 release，制造「在途运行」窗口。
type blockingRunService struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingRunService) Run(ctx context.Context, _ core.RunRequest) (core.RunResult, error) {
	close(b.started)
	select {
	case <-b.release:
		return core.RunResult{RunID: "run-1", Status: core.StatusCompleted}, nil
	case <-ctx.Done():
		return core.RunResult{RunID: "run-1", Status: core.StatusCancelled}, nil
	}
}

// TestMessages_FailureReasonRedaction 逐字保留模块卡 R15（W9）。
// 逆改点：writeRunResult 直接把 res.Reason 写进 data.reason → 200 三个子用例红；
// writeError 把 err.Error() 拼进 message → 500 子用例红；publicRunReason 不特判
// cancelled / 两条 core 常量（把常量也映射成「运行达到上限」）→ 对应子用例的
// **data.reason 字段断言**红。断言必须解析信封取 data.reason——limit 的固定 message
// 本身就含常量原文，整串 Contains 没有判别力。响应面**不得**出现哨兵；日志面按
// 约束 23 的反向口径断言（500 的 ERROR 行保留原始原因供排障，200 的 INFO 日志不得带）。
func TestMessages_FailureReasonRedaction(t *testing.T) {
	const (
		hostile       = `open /home/deploy/.basalt/sessions.db: SELECT * FROM messages WHERE id='INTERNAL-MARKER-7f3a'`
		limitFallback = "运行达到上限"
	)
	cases := []struct {
		name           string
		configure      func(*Options)
		wantStatus     int
		wantCode       string
		wantDataReason string   // 200 路径的 data.reason；500 无 data（空串）
		logHasRaw      bool     // 500：原始原因只允许出现在 ERROR 日志
		forbidLog      []string // 200 路径日志不得出现的原文（独立复核 R5R-01 夹具）
		wantLogIn      []string // 200 路径日志必须出现的投影占位
	}{
		{"500 不泄底层错误", func(o *Options) {
			o.Runs = &fakeRunService{err: errors.New(hostile)}
		}, http.StatusInternalServerError, "INTERNAL", "", true, nil, nil},
		{"200+failed 不泄原始原因", func(o *Options) {
			o.Runs = &fakeRunService{result: core.RunResult{
				RunID: "run-1", Status: core.StatusFailed, Reason: hostile,
			}}
		}, http.StatusOK, "OK", "运行失败", false, nil, nil},
		{
			"200+failed 不泄 Authorization Bearer", func(o *Options) {
				o.Runs = &fakeRunService{result: core.RunResult{
					RunID: "run-1", Status: core.StatusFailed,
					Reason: "upstream refused: Authorization: Bearer SUPERSECRET-TOKEN-abc123",
				}}
			}, http.StatusOK, "OK", "运行失败", false,
			[]string{"SUPERSECRET-TOKEN-abc123"},
			[]string{"Authorization=<redacted>"},
		},
		{
			"200+failed 不泄 Authorization Basic", func(o *Options) {
				o.Runs = &fakeRunService{result: core.RunResult{
					RunID: "run-1", Status: core.StatusFailed,
					Reason: "upstream refused: Authorization: Basic dXNlcjpwYXNzd29yZA==",
				}}
			}, http.StatusOK, "OK", "运行失败", false,
			[]string{"dXNlcjpwYXNzd29yZA=="},
			[]string{"Authorization=<redacted>"},
		},
		{"200+cancelled → 固定公开文案", func(o *Options) {
			o.Runs = &fakeRunService{result: core.RunResult{
				RunID: "run-1", Status: core.StatusCancelled, Reason: hostile,
			}}
		}, http.StatusOK, "OK", "运行已取消", false, nil, nil},
		{"200+limit 的 ReasonIterationLimit 原样保留", func(o *Options) {
			o.Runs = &fakeRunService{result: core.RunResult{
				RunID: "run-1", Status: core.StatusLimitReached, Reason: core.ReasonIterationLimit,
			}}
		}, http.StatusOK, "LIMIT_REACHED", core.ReasonIterationLimit, false, nil, nil},
		{"200+limit 的 ReasonToolBudget 原样保留", func(o *Options) {
			o.Runs = &fakeRunService{result: core.RunResult{
				RunID: "run-1", Status: core.StatusLimitReached, Reason: core.ReasonToolBudget,
			}}
		}, http.StatusOK, "LIMIT_REACHED", core.ReasonToolBudget, false, nil, nil},
		{"200+limit 未知原因回落固定文案", func(o *Options) {
			o.Runs = &fakeRunService{result: core.RunResult{
				RunID: "run-1", Status: core.StatusLimitReached, Reason: hostile,
			}}
		}, http.StatusOK, "LIMIT_REACHED", limitFallback, false, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			s := newTestServer(t, func(o *Options) {
				o.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
				tc.configure(o)
			})
			s.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
				ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
			})
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))

			require.Equal(t, tc.wantStatus, rec.Code)
			body := rec.Body.String()

			// 解析信封：data 省略 = 失败响应；data.reason 必须逐字等于白名单投影。
			var env struct {
				Code    string          `json:"code"`
				Message string          `json:"message"`
				Data    json.RawMessage `json:"data"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &env))
			require.Equal(t, tc.wantCode, env.Code)
			if tc.wantDataReason == "" {
				require.Zero(t, len(env.Data), "失败响应必须省略 data")
				require.Equal(t, "服务器内部错误", env.Message)
			} else {
				var run struct {
					Status string `json:"status"`
					Reason string `json:"reason"`
				}
				require.NoError(t, json.Unmarshal(env.Data, &run))
				require.Equal(t, tc.wantDataReason, run.Reason,
					"data.reason 必须逐字等于公开口径（limit 仅两条 core 常量原样保留）")
			}

			require.NotContains(t, body, "INTERNAL-MARKER-7f3a", "哨兵串泄漏进响应")
			require.NotContains(t, body, "/home/deploy", "绝对路径泄漏进响应")
			require.NotContains(t, body, "SELECT", "SQL 片段泄漏进响应")

			if tc.logHasRaw {
				require.Contains(t, logBuf.String(), "INTERNAL-MARKER-7f3a",
					"500 的 ERROR 日志必须保留原始原因供排障（约束 23 的日志面）")
			} else {
				require.NotContains(t, logBuf.String(), "INTERNAL-MARKER-7f3a",
					"200 路径的日志不得带原始原因")
			}
			for _, forbidden := range tc.forbidLog {
				require.NotContains(t, logBuf.String(), forbidden,
					"凭证 / 令牌不得落日志")
				require.NotContains(t, body, forbidden, "凭证 / 令牌不得进响应")
			}
			for _, want := range tc.wantLogIn {
				require.Contains(t, logBuf.String(), want,
					"诊断必须留下固定投影占位")
			}
		})
	}
}

// TestMessages_FailedRunDiagnosticLogged 验证 failed 运行的内部诊断落日志（R24）：
// 响应只给固定公开文案，日志留一条经脱敏的 WARN，并关联 run_id / request_id /
// session_id / agent——core 在 Run 账本创建前失败时，这是唯一线索。
// 逆改点：writeRunResult 对 failed 不记诊断 → 找不到 diagnostic= 行，断言红；
// 诊断直接写原始原因（不脱敏）→ 哨兵 / 绝对路径 / SQL 正文断言红。
func TestMessages_FailedRunDiagnosticLogged(t *testing.T) {
	const hostile = `open /home/deploy/.basalt/sessions.db: SELECT * FROM messages WHERE id='INTERNAL-MARKER-7f3a'`
	var logBuf bytes.Buffer
	s := newTestServer(t, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
		o.Runs = &fakeRunService{result: core.RunResult{
			RunID: "run-42", Status: core.StatusFailed, Reason: hostile,
		}}
	})
	s.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))

	require.Equal(t, http.StatusOK, rec.Code)
	var env struct {
		Code string `json:"code"`
		Data struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "OK", env.Code)
	require.Equal(t, "运行失败", env.Data.Reason, "响应只给固定公开文案")

	var diagnostic string
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(line, "diagnostic=") {
			diagnostic = line
		}
	}
	require.NotEmpty(t, diagnostic, "failed 运行必须留下内部诊断日志")
	require.Contains(t, diagnostic, "level=WARN")
	require.Contains(t, diagnostic, "run_id=run-42")
	require.Contains(t, diagnostic, "session_id=web-x")
	require.Contains(t, diagnostic, "agent=ops-agent")
	_ = logRequestID(t, diagnostic) // 非空 request_id
	require.Contains(t, diagnostic, "<path>", "绝对路径必须被投影")
	require.Contains(t, diagnostic, "<sql>", "SQL 正文必须被投影")
	require.NotContains(t, diagnostic, "INTERNAL-MARKER-7f3a", "引号字面量不得落日志")
	require.NotContains(t, diagnostic, "/home/deploy", "绝对路径不得落日志")
	require.NotContains(t, diagnostic, "SELECT", "SQL 正文不得落日志")

	// 独立复核 R5R-01 / R5R-04 的具名夹具（同函数内扩展，不新增测试函数）：
	// Authorization 头连同 scheme 与凭证整体投影；多行 SQL、含空格路径、UNC 与
	// 转义引号不得留下后半截。逆改点：authorization 交给通用键值规则（只吃 scheme）
	// → Bearer/Basic 凭证重新出现；SQL 只吃到行尾 / 路径遇空格截断 / 引号不支持
	// 转义 → 对应 forbid 串重新出现在诊断里，断言红。
	cases := []struct {
		name   string
		reason string
		wantIn []string
		forbid []string
	}{
		{
			"Authorization Bearer", "refused Authorization: Bearer SUPERSECRET-TOKEN-abc123",
			[]string{"Authorization=<redacted>"},
			[]string{"SUPERSECRET-TOKEN-abc123"},
		},
		{
			"Authorization Basic", "refused Authorization: Basic dXNlcjpwYXNzd29yZA==",
			[]string{"Authorization=<redacted>"},
			[]string{"dXNlcjpwYXNzd29yZA=="},
		},
		{
			"多行 SQL 跨行投影", "run failed\nSELECT *\nFROM secret_table WHERE id=1",
			[]string{"<sql>"},
			[]string{"secret_table", "WHERE id=1"},
		},
		{
			"含空格绝对路径", "open /home/deploy/my dir/secret.db: boom",
			[]string{"<path>"},
			[]string{"my dir"},
		},
		{
			"UNC 路径", `read \\fileserver\share\secret.db failed`,
			[]string{"<path>"},
			[]string{"fileserver"},
		},
		{
			"转义引号字面量", `parse 'it\'s SECRET-3' failed`,
			[]string{"<literal>"},
			[]string{"SECRET-3"},
		},
		// 人工 review ②（R7）：凭证键值与引号的组合——键值规则必须把带引号的
		// 凭证值整体吞掉（含空格 / 转义），未闭合引号也要遮蔽到串尾。逆改点：
		// 凭证值退回 `\S+`（只吃到首个空白）→ 尾部哨兵重新出现在诊断里，断言红。
		{
			"双引号含空格凭证", `failed password="prefix SECRET-TAIL-P41"`,
			[]string{"password=<redacted>"},
			[]string{"SECRET-TAIL-P41"},
		},
		{
			"单引号含空格凭证", `failed token='prefix SECRET-TAIL-T42'`,
			[]string{"token=<redacted>"},
			[]string{"SECRET-TAIL-T42"},
		},
		{
			"转义引号凭证", `failed password="prefix \"SECRET-TAIL-E43\" tail"`,
			[]string{"password=<redacted>"},
			[]string{"SECRET-TAIL-E43", "<literal>"},
		},
		{
			"未闭合引号凭证", `failed password="prefix SECRET-TAIL-U44`,
			[]string{"password=<redacted>"},
			[]string{"SECRET-TAIL-U44"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := newTestServer(t, func(o *Options) {
				o.Logger = slog.New(slog.NewTextHandler(&buf, nil))
				o.Runs = &fakeRunService{result: core.RunResult{
					RunID: "run-42", Status: core.StatusFailed, Reason: tc.reason,
				}}
			})
			s.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
				ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
			})
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))
			require.Equal(t, http.StatusOK, rec.Code)

			var line string
			for _, candidate := range strings.Split(buf.String(), "\n") {
				if strings.Contains(candidate, "diagnostic=") {
					line = candidate
				}
			}
			require.NotEmpty(t, line, "failed 运行必须留下内部诊断日志")
			for _, want := range tc.wantIn {
				require.Contains(t, line, want, "诊断必须保留固定投影占位")
			}
			for _, forbidden := range tc.forbid {
				require.NotContains(t, line, forbidden, "投影不得留下原始片段")
			}
		})
	}

	// 独立复核 R5R-06：客户端在运行返回前断开时，failed 诊断仍必须落日志——
	// 否则「运行失败但原因未入库」的唯一线索随连接一起丢失。逆改点：客户端断开
	// 分支只调 logClientGone、不调 logFailedRun → 找不到 diagnostic= 行，断言红。
	t.Run("客户端断开仍留 failed 诊断", func(t *testing.T) {
		var buf bytes.Buffer
		s := newTestServer(t, func(o *Options) {
			o.Logger = slog.New(slog.NewTextHandler(&buf, nil))
			o.Runs = &fakeRunService{result: core.RunResult{
				RunID: "run-42", Status: core.StatusFailed, Reason: hostile,
			}}
		})
		s.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
			ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)).WithContext(ctx)
		s.handler().ServeHTTP(httptest.NewRecorder(), req)

		var diagnostic string
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "diagnostic=") {
				diagnostic = line
			}
		}
		require.NotEmpty(t, diagnostic, "客户端断开也必须留下 failed 诊断")
		require.Contains(t, diagnostic, "run_id=run-42")
		require.NotContains(t, diagnostic, "INTERNAL-MARKER-7f3a")
		require.Contains(t, buf.String(), "客户端已断开")
	})
}
