package web

// 本文件覆盖 14D 就绪视图与启用闸门共用的评估器（设计 §2.5.1 / §2.6.4，验收 E6 / E8）。
// 判据分三轴：
//   - **阻断轴**：缺模型连接 / 缺服务凭据 / 资源未发布 → pending 非空且启用 409；
//   - **提示轴**：预算被压住 / 并发可能超限 / 通知渠道缺失 → notices 非空但 ready 不变；
//   - **失败关闭**：资源视图读不到 → 500，**不**降级成"就绪"。
// 逆改点：把任一类提示写成阻断（或反之）→ 对应子例红；视图字段与契约漂移 →
// TestReadinessView_MatchesContract 红。
// The readiness evaluator is pinned on three axes: blocking items, advisory notices, and
// fail-closed reads. Swapping a notice into pending (or the reverse) turns a subtest red.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// readinessProfile 是一只把所有引用都声明齐的 Agent：Provider + 一个 MCP + 一个技能。
// 用例按需改写其中一处，让每个子例只测一条判定。
// readinessProfile declares every kind of reference so each case can break exactly one.
func readinessProfile() *core.Profile {
	return &core.Profile{
		Name:       "ops-agent",
		Provider:   core.ProviderRef{Name: "openai", Model: "gpt-test"},
		MCPServers: []string{"files"},
		Skills:     []string{"pdf-extract"},
	}
}

// readinessPublished 是"全部已发布且凭据就绪"的资源视图：正是 ready 的基线。
// readinessPublished is the fully published baseline behind a ready verdict.
func readinessPublished() fakeResources {
	return fakeResources{
		modelConns: []ModelConnectionView{sampleModelConnectionView()},
		mcpServers: []McpServerView{sampleMcpServerView()},
		skills:     []SkillView{sampleSkillView()},
	}
}

// readinessEntry 用给定 profile 造一个已注册条目。
// readinessEntry wraps a profile in a registered loader entry.
func readinessEntry(profile *core.Profile) core.AgentEntry {
	return core.AgentEntry{Name: "ops-agent", Registered: true, Profile: profile}
}

// readinessServer 装配一只 Agent 与一份资源视图；mutate 覆盖其余端口。
// readinessServer assembles one agent plus one resource view.
func readinessServer(t *testing.T, entry core.AgentEntry, res ResourceReader, mutate func(*Options)) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		o.Agents = fakeAgents{entries: []core.AgentEntry{entry}}
		o.Resources = res
		if mutate != nil {
			mutate(o)
		}
	})
}

// TestAgentReadiness_ReadyNeedsEveryService 覆盖"全齐即就绪"基线与四类阻断项：
// 每类缺一项都必须点名，且**只**影响自己那一项。
// 逆改点：漏判任一类 / 把未发布读成就绪 → 对应子例红。
func TestAgentReadiness_ReadyNeedsEveryService(t *testing.T) {
	t.Run("every reference published yields ready", func(t *testing.T) {
		s := readinessServer(t, readinessEntry(readinessProfile()), readinessPublished(), nil)

		view, err := s.agentReadiness(context.Background(), readinessEntry(readinessProfile()))
		require.NoError(t, err)
		require.True(t, view.Ready)
		require.Empty(t, view.Pending)
		require.Empty(t, view.Notices)
	})

	cases := []struct {
		name     string
		mutate   func(*fakeResources)
		wantKind string
		wantItem string
		wantFix  string
	}{
		{
			name:     "unpublished model connection",
			mutate:   func(res *fakeResources) { res.modelConns = nil },
			wantKind: readinessModelConnection,
			wantItem: "model_connection/openai",
			wantFix:  consoleModelConnsPath,
		},
		{
			name: "model credential unavailable",
			mutate: func(res *fakeResources) {
				conn := sampleModelConnectionView()
				conn.Availability = core.AvailabilityUnavailable
				res.modelConns = []ModelConnectionView{conn}
			},
			wantKind: readinessCredential,
			wantItem: "model_connection/openai",
			wantFix:  consoleModelConnsPath,
		},
		{
			name:     "unpublished mcp server",
			mutate:   func(res *fakeResources) { res.mcpServers = []McpServerView{{Name: "files"}} },
			wantKind: readinessResource,
			wantItem: "mcp_server/files",
			wantFix:  consoleMCPServersPath,
		},
		{
			name:     "unpublished skill",
			mutate:   func(res *fakeResources) { res.skills = []SkillView{{Name: "pdf-extract"}} },
			wantKind: readinessResource,
			wantItem: "skill/pdf-extract",
			wantFix:  consoleSkillsPath,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := readinessPublished()
			tc.mutate(&res)
			s := readinessServer(t, readinessEntry(readinessProfile()), res, nil)

			view, err := s.agentReadiness(context.Background(), readinessEntry(readinessProfile()))
			require.NoError(t, err)
			require.False(t, view.Ready, "有待补项就不能就绪")
			require.Len(t, view.Pending, 1, "只缺一项时只点名一项：%+v", view.Pending)
			require.Equal(t, tc.wantKind, view.Pending[0].Kind)
			require.Equal(t, tc.wantItem, view.Pending[0].Item)
			require.Equal(t, tc.wantFix, view.Pending[0].Fix)
			require.NotEmpty(t, view.Pending[0].Detail)

			// 同一份评估器必须让启用端点也拒绝，并把名字与跳转写进 409 文案。
			reason := s.enableResourceRejection(context.Background(), readinessEntry(readinessProfile()))
			require.Contains(t, reason, tc.wantItem)
			require.Contains(t, reason, tc.wantFix)
		})
	}
}

// TestAgentReadiness_MCPCredentialStates 覆盖四个凭据状态的判定：managed / env 就绪，
// managed_revoked / missing 阻断，未知值按不可确认阻断（**不**当成就绪）。
// 逆改点：把 env 判成阻断（T114-R2 的判定反转）或放过 revoked → 对应子例红。
func TestAgentReadiness_MCPCredentialStates(t *testing.T) {
	cases := []struct {
		state      string
		wantBlock  bool
		wantDetail string
	}{
		{state: string(core.MCPCredentialManaged), wantBlock: false},
		{state: string(core.MCPCredentialEnv), wantBlock: false},
		{state: string(core.MCPCredentialManagedRevoked), wantBlock: true, wantDetail: readinessCredRevokedDetail},
		{state: string(core.MCPCredentialMissing), wantBlock: true, wantDetail: readinessCredMissingDetail},
		{state: "unexpected", wantBlock: true, wantDetail: readinessCredUnknownDetail},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			res := readinessPublished()
			server := sampleMcpServerView()
			server.CredentialKeys = []McpCredentialKeyView{{Key: "MCP_TOKEN", Mode: "managed", State: tc.state}}
			res.mcpServers = []McpServerView{server}
			s := readinessServer(t, readinessEntry(readinessProfile()), res, nil)

			view, err := s.agentReadiness(context.Background(), readinessEntry(readinessProfile()))
			require.NoError(t, err)
			if !tc.wantBlock {
				require.Empty(t, view.Pending)
				require.True(t, view.Ready)
				return
			}
			require.Len(t, view.Pending, 1, "%+v", view.Pending)
			require.Equal(t, readinessCredential, view.Pending[0].Kind)
			require.Equal(t, "mcp_server/files#MCP_TOKEN", view.Pending[0].Item)
			require.Equal(t, tc.wantDetail, view.Pending[0].Detail)
			require.Equal(t, "/console/resources/mcp-servers/files", view.Pending[0].Fix)
		})
	}
}

// TestAgentReadiness_NoticesNeverBlock 覆盖三条**只提示**的判定：预算被实例上限压住、
// 计划数可能超出并发上限、通知渠道缺失。三者都不得改变 ready，也不得写进 409。
// 逆改点：把任一条提示改成阻断 → ready 变 false 且启用被拒，子例红。
func TestAgentReadiness_NoticesNeverBlock(t *testing.T) {
	t.Run("budget cap is named without editing config", func(t *testing.T) {
		profile := readinessProfile()
		profile.Settings.MaxIterations = 30
		s := readinessServer(t, readinessEntry(profile), readinessPublished(), func(o *Options) {
			o.InstanceLimits = InstanceLimits{MaxIterations: 10, MaxConcurrentRuns: 32}
		})

		view, err := s.agentReadiness(context.Background(), readinessEntry(profile))
		require.NoError(t, err)
		require.True(t, view.Ready, "预算提示不阻止启用")
		require.Len(t, view.Notices, 1)
		require.Equal(t, readinessBudget, view.Notices[0].Kind)
		require.Equal(t, "settings.max_iterations", view.Notices[0].Item)
		require.Contains(t, view.Notices[0].Detail, "30")
		require.Contains(t, view.Notices[0].Detail, "10")
		require.Contains(t, view.Notices[0].Detail, "budget.max_iterations", "点名实例配置键")
		require.Empty(t, view.Notices[0].Fix, "配置键没有对应页面，不伪造跳转")
		require.Equal(t, 30, profile.Settings.MaxIterations, "只点名，不改用户配置")

		reason := s.enableResourceRejection(context.Background(), readinessEntry(profile))
		require.Empty(t, reason, "提示项不停启用")
	})

	t.Run("concurrency counts distinct enabled agents", func(t *testing.T) {
		tasks := []core.TaskRecord{
			{TaskID: "t1", Agent: "ops-agent", Enabled: true},
			{TaskID: "t2", Agent: "ops-agent", Enabled: true},
			{TaskID: "t3", Agent: "reporter", Enabled: true},
			{TaskID: "t4", Agent: "digest", Enabled: false},
			{TaskID: "t5", Agent: "retired", Enabled: true, RetiredAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		}
		s := readinessServer(t, readinessEntry(readinessProfile()), readinessPublished(), func(o *Options) {
			o.Schedules = &fakeScheduleService{tasks: tasks}
			o.InstanceLimits = InstanceLimits{MaxIterations: 10, MaxConcurrentRuns: 1}
		})

		view, err := s.agentReadiness(context.Background(), readinessEntry(readinessProfile()))
		require.NoError(t, err)
		require.True(t, view.Ready)
		require.Len(t, view.Notices, 1)
		require.Equal(t, readinessConcurrency, view.Notices[0].Kind)
		require.Equal(t, "server.max_concurrent_runs", view.Notices[0].Item)
		require.Contains(t, view.Notices[0].Detail, "2", "只数启用且未退役计划涉及的**不同** Agent")
		require.Contains(t, view.Notices[0].Detail, "429 TOO_MANY_RUNS")
	})

	t.Run("concurrency stays quiet within the cap", func(t *testing.T) {
		s := readinessServer(t, readinessEntry(readinessProfile()), readinessPublished(), func(o *Options) {
			o.Schedules = &fakeScheduleService{tasks: []core.TaskRecord{
				{TaskID: "t1", Agent: "ops-agent", Enabled: true},
			}}
			o.InstanceLimits = InstanceLimits{MaxIterations: 10, MaxConcurrentRuns: 1}
		})

		view, err := s.agentReadiness(context.Background(), readinessEntry(readinessProfile()))
		require.NoError(t, err)
		require.Empty(t, view.Notices)
	})

	t.Run("missing notify channel is a notice, never a block", func(t *testing.T) {
		profile := readinessProfile()
		profile.NotifyChannels = []string{"ops"}
		s := readinessServer(t, readinessEntry(profile), readinessPublished(), func(o *Options) {
			o.Notify = fakeNotifyChannels{}
		})

		view, err := s.agentReadiness(context.Background(), readinessEntry(profile))
		require.NoError(t, err)
		notices := s.notifyNotice(context.Background(), profile, "/console/agents/ops-agent/config")
		view.Notices = append(view.Notices, notices...)
		require.True(t, view.Ready)
		require.Len(t, view.Notices, 1)
		require.Equal(t, readinessNotifyChannel, view.Notices[0].Kind)
		require.Equal(t, "ops", view.Notices[0].Item)
		require.Equal(t, "/console/agents/ops-agent/config", view.Notices[0].Fix)
	})
}

// TestAgentReadiness_InstanceLimitDefaults 覆盖零值归一：装配层漏投影时，就绪视图与
// 运行面必须用**同一对**缺省值（10 / 32），而不是把"没配置"读成 0。
// 逆改点：把归一删掉 → 每只 Agent 都显示被 0 压住，本用例红。
func TestAgentReadiness_InstanceLimitDefaults(t *testing.T) {
	s := newTestServer(t, nil)
	require.Equal(t, core.DefaultMaxIterations, s.opts.InstanceLimits.MaxIterations)
	require.Equal(t, core.DefaultMaxConcurrentRuns, s.opts.InstanceLimits.MaxConcurrentRuns)
	require.Equal(t, 10, core.DefaultMaxIterations)
	require.Equal(t, 32, core.DefaultMaxConcurrentRuns)
}

// TestAgentReadiness_UnregisteredEntryReportsLoaderReason 覆盖未注册条目：只回装载
// 拒因一项（补别的也没有意义），kind 按拒因分类，且**不**读资源视图。
// 逆改点：未注册条目也去读资源索引 → 资源错误用例会把它变成 500。
func TestAgentReadiness_UnregisteredEntryReportsLoaderReason(t *testing.T) {
	entry := core.AgentEntry{
		Name:         "ops-agent",
		Registered:   false,
		RejectKind:   core.AgentRejectProviderNotRegistered,
		RejectDetail: "ghost",
	}
	s := newTestServer(t, func(o *Options) {
		o.Agents = fakeAgents{entries: []core.AgentEntry{entry}}
		// 资源视图不可读：未注册条目**不应**读到它，否则这条路径会被错误吞成 500。
		o.Resources = fakeResources{modelListErr: errors.New("磁盘已满 at D:/data/basalt.db")}
	})

	view, err := s.agentReadiness(context.Background(), entry)
	require.NoError(t, err)
	require.False(t, view.Ready)
	require.Len(t, view.Pending, 1)
	require.Equal(t, readinessModelConnection, view.Pending[0].Kind)
	require.Empty(t, view.Pending[0].Item, "装载拒因没有机器名")
	require.Equal(t, entryRejectMessage(entry), view.Pending[0].Detail)
	require.Equal(t, consoleModelConnsPath, view.Pending[0].Fix,
		"缺模型连接指到能修它的页面，不是 Agent 配置页")
}

// TestAgentReadiness_RejectFixRoutes 钉住"跳转点得通且指得对"：每类装载拒因必须指到
// 能真正修它的页面；控制台没有的路由一律退回 Agent 配置页。
// 逆改点：把技能 / MCP 拒因指到模型连接页（或反之）→ 对应子例红。
func TestAgentReadiness_RejectFixRoutes(t *testing.T) {
	configFix := "/console/agents/ops-agent/config"
	cases := map[core.AgentRejectKind]string{
		core.AgentRejectProviderNotRegistered: consoleModelConnsPath,
		core.AgentRejectSkillNotFound:         consoleSkillsPath,
		core.AgentRejectSkillVersionMissing:   consoleSkillsPath,
		core.AgentRejectMCPServerSkipped:      consoleMCPServersPath,
		core.AgentRejectBlankModel:            configFix,
		core.AgentRejectNameMismatch:          configFix,
		core.AgentRejectInvalidSchedule:       configFix,
	}
	for kind, want := range cases {
		t.Run(string(kind), func(t *testing.T) {
			require.Equal(t, want, readinessFixForReject(kind, configFix))
		})
	}
}

// TestAdminReadiness_Endpoint 覆盖 HTTP 面：200 视图、404 不存在、500 失败关闭（不
// 泄露底层路径、不带 data）。
// 逆改点：读失败降级成 200 空待补 / 底层错误进响应 → 对应子例红。
func TestAdminReadiness_Endpoint(t *testing.T) {
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) AgentReadinessView {
		t.Helper()
		var env struct {
			Code string             `json:"code"`
			Data AgentReadinessView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeOK, env.Code)
		return env.Data
	}

	t.Run("returns the readiness view", func(t *testing.T) {
		entry := readinessEntry(readinessProfile())
		s := readinessServer(t, entry, readinessPublished(), nil)

		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/agents/ops-agent/readiness", ""))
		require.Equal(t, http.StatusOK, rec.Code, "%s", rec.Body.String())
		view := decode(t, rec)
		require.Equal(t, "ops-agent", view.Agent)
		require.True(t, view.Ready)
		require.NotNil(t, view.Pending, "空集合必须出 `[]` 而不是 null")
		require.NotNil(t, view.Notices)
	})

	t.Run("unknown agent is 404", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.Agents = fakeAgents{} })

		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/agents/ghost/readiness", ""))
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("unreadable resource view fails closed", func(t *testing.T) {
		res := readinessPublished()
		res.modelListErr = errors.New(`读 resources 失败: D:\data\basalt.db`)
		s := readinessServer(t, readinessEntry(readinessProfile()), res, nil)

		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/agents/ops-agent/readiness", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		env := decodeAuthEnvelope(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Contains(t, env.Message, resourceViewsProblem)
		require.NotContains(t, rec.Body.String(), "basalt.db", "底层原因只进日志")
		require.NotContains(t, rec.Body.String(), `"data"`, "失败信封不带 data")
	})
}

// TestReadinessView_MatchesContract 钉住视图与契约 schema 的字段集一致、kind 闭集一致。
// 逆改点：加字段不更新契约 / 新增 kind 不写进 schema → 本用例红。
func TestReadinessView_MatchesContract(t *testing.T) {
	doc := readContractDoc(t, "openapi.yaml")
	view := AgentReadinessView{
		Agent: "ops-agent",
		Ready: false,
		Pending: []AgentReadinessItem{{
			Kind: readinessModelConnection, Item: "model_connection/openai",
			Detail: readinessMissingModelDetail, Fix: consoleModelConnsPath,
		}},
		Notices: []AgentReadinessItem{{
			Kind: readinessBudget, Item: "settings.max_iterations", Detail: "30 超过 10",
		}},
	}
	requireViewMatchesSchema(t, doc, "AgentReadiness", view)
	requireViewMatchesSchema(t, doc, "AgentReadinessItem", view.Pending[0])

	require.Equal(t,
		slices.Sorted(slices.Values([]string{
			readinessModelConnection, readinessCredential, readinessResource,
			readinessNotifyChannel, readinessBudget, readinessConcurrency,
		})),
		slices.Sorted(slices.Values(contractEnum(t, doc, "AgentReadinessItem", "kind"))),
		"kind 闭集必须与实现逐字一致",
	)
}

// TestReadinessPendingMessage 钉住 409 文案的拼装：无名字项只出原因，有名字项给
// `名字（原因，前往 路径）`，整体以固定引导语开头。
// 逆改点：空名字拼出 `（）` 或丢掉跳转 → 对应子例红。
func TestReadinessPendingMessage(t *testing.T) {
	cases := []struct {
		name  string
		items []AgentReadinessItem
		want  string
	}{
		{
			name:  "named item carries reason and jump",
			items: []AgentReadinessItem{{Item: "skill/pdf-extract", Detail: readinessSkillDetail, Fix: consoleSkillsPath}},
			want:  pendingConfigPrefix + "skill/pdf-extract（技能没有已发布版本，前往 /console/resources/skills）",
		},
		{
			name:  "nameless item is its reason alone",
			items: []AgentReadinessItem{{Detail: enableRejectedReason}},
			want:  pendingConfigPrefix + enableRejectedReason,
		},
		{
			name: "multiple items join with an enumeration comma",
			items: []AgentReadinessItem{
				{Item: "model_connection/openai", Detail: readinessMissingModelDetail, Fix: consoleModelConnsPath},
				{Item: "mcp_server/files", Detail: readinessMCPServerDetail, Fix: consoleMCPServersPath},
			},
			want: pendingConfigPrefix +
				"model_connection/openai（引用的模型连接未发布或不存在，前往 /console/resources/model-connections）、" +
				"mcp_server/files（MCP server 未发布或不存在，前往 /console/resources/mcp-servers）",
		},
		{
			name:  "empty fix drops the jump clause",
			items: []AgentReadinessItem{{Item: "settings.max_iterations", Detail: "声明 30 超过实例上限 10"}},
			want:  pendingConfigPrefix + "settings.max_iterations（声明 30 超过实例上限 10）",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, readinessPendingMessage(tc.items))
		})
	}
}
