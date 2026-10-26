package web

// 本文件判卷实例加密端点的 HTTP 面（14D / T121；契约 /instance/secrets 与
// /instance/secrets/init）。只判 HTTP 层的四件事：
//
//  1. 只读视图逐字段对契约（`InstanceSecretsView` / `CredentialKeySummary`），
//     `credential_summary` 与资源列表读**同一批**视图投影，未声明槽位按契约计入
//     `missing`；
//  2. GET 在恢复态与纯 env 实例上都是 200——状态就是状态，不是读错误；
//  3. 初始化端点的 400 / 409 / 500 三个出口：两种 409 与"不可初始化"都在预写之前
//     （零调用），500 只在服务层失败之后出现并带 `data.operation_id`；
//  4. 成功响应是**重新读出**的投影，不是回显服务层返回值。
//
// 判定表、`O_EXCL` 发布与审计预写的语义在 `internal/app` 的同名用例里判。
// This file judges the HTTP surface only; the decision table lives in internal/app.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeInstanceSecrets 是实例加密端口的内存替身：状态可编程、初始化可编程，两个
// 调用计数用于断言"请求校验与 409 判定都没触达写入路径"。
// fakeInstanceSecrets is the programmable port; the counters pin the fact that body
// validation and both 409 verdicts never reach Init.
type fakeInstanceSecrets struct {
	status InstanceSecretsStatus

	statusFn func(context.Context) (InstanceSecretsStatus, error)
	initFn   func(context.Context, core.Actor) (string, error)

	statusCalls int
	initCalls   int
	// lastActor 是最后一次 Init 收到的审计归因：处理器必须把管理员的真实身份交给
	// 服务层，而不是让初始化审计记成启动流程。
	lastActor core.Actor
}

func (f *fakeInstanceSecrets) Status(ctx context.Context) (InstanceSecretsStatus, error) {
	f.statusCalls++
	if f.statusFn != nil {
		return f.statusFn(ctx)
	}
	return f.status, nil
}

func (f *fakeInstanceSecrets) Init(ctx context.Context, actor core.Actor) (string, error) {
	f.initCalls++
	f.lastActor = actor
	if f.initFn != nil {
		return f.initFn(ctx, actor)
	}
	return "", nil
}

// 编译期断言：假件实现被测端口——端口漂移在编译期就红。
var _ InstanceSecretsService = (*fakeInstanceSecrets)(nil)

// readySecretsStatus 是就绪态的固定夹具：初始化时间与合同里的日期格式对齐。
// readySecretsStatus is the fixed ready-state fixture.
func readySecretsStatus() InstanceSecretsStatus {
	return InstanceSecretsStatus{
		State:         string(core.SecretStateReady),
		Reason:        string(core.SecretRecoveryNone),
		KeyID:         "mk-20261026",
		KeyFile:       "/var/lib/basalt/instance/master.key",
		InitializedAt: time.Date(2026, 10, 26, 9, 15, 0, 0, time.UTC),
		CanInitialize: false,
	}
}

// summaryFixture 是四档计数的固定夹具：MCP 一槽 managed + 一个未声明槽位（missing）；
// 模型连接 managed+active / env / managed+revoked 各一；通知渠道 managed+active 一。
// summaryFixture exercises every bucket through the three resource faces.
func summaryFixture() fakeResources {
	return fakeResources{
		mcpServers: []McpServerView{{
			Name:      "github",
			EnvKeys:   []string{"GITHUB_PERSONAL_ACCESS_TOKEN", "UNDECLARED_SLOT"},
			Connected: true,
			CredentialKeys: []McpCredentialKeyView{{
				Key: "GITHUB_PERSONAL_ACCESS_TOKEN", Mode: "managed", State: "managed",
			}},
		}},
		modelConns: []ModelConnectionView{
			{Name: "primary", CredentialMode: "managed", CredentialState: "active"},
			{Name: "fallback", CredentialMode: "", CredentialState: ""},
			{Name: "retired", CredentialMode: "managed", CredentialState: "revoked"},
		},
	}
}

// summaryChannels 是通知渠道侧的固定夹具：一条托管有效地址。
// summaryChannels is the notify-side fixture: one active managed channel.
func summaryChannels() fakeNotifyChannels {
	return fakeNotifyChannels{channels: []core.NotifyChannel{{
		Name: "ops", CredentialMode: core.CredentialModeManaged,
		CredentialState: core.CredentialIdentityActive,
	}}}
}

// newInstanceSecretsServer 装配带假实例加密端口的 handler 级服务。
// newInstanceSecretsServer assembles a handler-level server with the fake port.
func newInstanceSecretsServer(t *testing.T, port InstanceSecretsService,
	resources ResourceReader, notify core.NotifyChannelReader,
) *Server {
	t.Helper()
	return newTestServer(t, func(o *Options) {
		o.InstanceSecrets = port
		o.Resources = resources
		o.Notify = notify
	})
}

// TestInstanceSecrets_ReadView：`GET /instance/secrets` 的字段集与契约
// `InstanceSecretsView` 完全一致（含嵌套 `CredentialKeySummary`），四档计数与三份
// 资源视图同源；响应里**没有**指纹或密钥材料字段。
// 逆改点：漏投影任一字段 / 计数换成另一条读取路径 / 把指纹带进响应 → 红。
func TestInstanceSecrets_ReadView(t *testing.T) {
	fake := &fakeInstanceSecrets{status: readySecretsStatus()}
	s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())

	rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/instance/secrets", ""))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	doc := readContractDoc(t, "openapi.yaml")
	requireResourcePayloadMatches(t, doc, "InstanceSecretsView", raw.Data)
	summary, ok := raw.Data["credential_summary"].(map[string]any)
	require.True(t, ok, "credential_summary 必须是对象")
	requireResourcePayloadMatches(t, doc, "CredentialKeySummary", summary)

	view := decodeResourcesEnvelope[InstanceSecretsView](t, rec).Data
	require.Equal(t, "ready", view.State)
	require.Equal(t, "mk-20261026", view.KeyID)
	require.Equal(t, "/var/lib/basalt/instance/master.key", view.KeyFile)
	require.Equal(t, "2026-10-26T09:15:00Z", view.InitializedAt)
	require.Empty(t, view.RecoveryReason)
	require.Equal(t, CredentialKeySummaryView{Managed: 3, ManagedRevoked: 1, Env: 1, Missing: 1},
		view.CredentialSummary)
	require.NotContains(t, rec.Body.String(), "fingerprint", "指纹不得进入响应")
	require.Zero(t, fake.initCalls, "只读端点不得调用初始化")
}

// TestInstanceSecrets_ReadStates：未初始化（纯 env 与待初始化两种）与恢复态都是
// 200——GET 报的是状态；恢复态的 `recovery_reason` 逐码透出，`key_id` 只在 ready
// 时非空。
// 逆改点：恢复态回 500 / ready 以外也回 key_id / 恢复原因被吞掉 → 红。
func TestInstanceSecrets_ReadStates(t *testing.T) {
	cases := []struct {
		name       string
		status     InstanceSecretsStatus
		wantState  string
		wantKeyID  string
		wantKeyFle string
		wantReason string
	}{
		{
			name:      "uninitialized pure env instance",
			status:    InstanceSecretsStatus{State: "uninitialized", Reason: ""},
			wantState: "uninitialized",
		},
		{
			name: "uninitialized awaiting explicit init",
			status: InstanceSecretsStatus{
				State: "uninitialized", Reason: "",
				KeyFile: "/var/lib/basalt/instance/master.key", CanInitialize: true,
			},
			wantState:  "uninitialized",
			wantKeyFle: "/var/lib/basalt/instance/master.key",
		},
		{
			name:       "recovery: key file missing",
			status:     InstanceSecretsStatus{State: "recovery_required", Reason: "missing"},
			wantState:  "recovery_required",
			wantReason: "missing",
		},
		{
			name:       "recovery: corrupt",
			status:     InstanceSecretsStatus{State: "recovery_required", Reason: "corrupt"},
			wantState:  "recovery_required",
			wantReason: "corrupt",
		},
		{
			name:       "recovery: unprovable provenance",
			status:     InstanceSecretsStatus{State: "recovery_required", Reason: "unprovable_provenance"},
			wantState:  "recovery_required",
			wantReason: "unprovable_provenance",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newInstanceSecretsServer(t, &fakeInstanceSecrets{status: tc.status},
				summaryFixture(), summaryChannels())
			rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/instance/secrets", ""))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			view := decodeResourcesEnvelope[InstanceSecretsView](t, rec).Data
			require.Equal(t, tc.wantState, view.State)
			require.Equal(t, tc.wantKeyID, view.KeyID)
			require.Equal(t, tc.wantKeyFle, view.KeyFile)
			require.Equal(t, tc.wantReason, view.RecoveryReason)
			require.Empty(t, view.InitializedAt, "没有初始化记录时 initialized_at 是空串")
		})
	}
}

// TestInstanceSecrets_ReadFailsClosed：缺端口、状态读取失败、端口给出契约外取值
// 一律 500，且底层错误文本不出站；缺 Resources / Notify 端口同样 500——四档计数
// 读不到时不能用 0 冒充"没有凭据"。
// 逆改点：把缺端口说成"未初始化"的 200 / 计数读取失败回 0 / 契约外状态写进响应 → 红。
func TestInstanceSecrets_ReadFailsClosed(t *testing.T) {
	t.Run("port missing", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.InstanceSecrets = nil })
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/instance/secrets", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, codeInternal, decodePolicyError(t, rec).Code)
		require.NotContains(t, rec.Body.String(), "uninitialized", "缺端口不得被读成未初始化")
	})

	t.Run("status read failure", func(t *testing.T) {
		fake := &fakeInstanceSecrets{
			statusFn: func(context.Context) (InstanceSecretsStatus, error) {
				return InstanceSecretsStatus{}, errors.New("store exploded")
			},
		}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/instance/secrets", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, messageInternal, env.Message)
		require.NotContains(t, rec.Body.String(), "store exploded", "底层原因只进日志")
	})

	t.Run("state outside the contract enum", func(t *testing.T) {
		fake := &fakeInstanceSecrets{status: InstanceSecretsStatus{State: "half_ready", Reason: ""}}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/instance/secrets", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.NotContains(t, rec.Body.String(), "half_ready", "契约外取值不得写进响应")
	})

	t.Run("recovery reason outside the contract enum", func(t *testing.T) {
		fake := &fakeInstanceSecrets{status: InstanceSecretsStatus{
			State: "recovery_required", Reason: "metaphysical",
		}}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/instance/secrets", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("resource face missing", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) {
			o.InstanceSecrets = &fakeInstanceSecrets{status: readySecretsStatus()}
			o.Resources = nil
			o.Notify = summaryChannels()
		})
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/instance/secrets", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("notify face missing", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) {
			o.InstanceSecrets = &fakeInstanceSecrets{status: readySecretsStatus()}
			o.Resources = summaryFixture()
			o.Notify = nil
		})
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/instance/secrets", ""))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})
}

// TestInstanceSecrets_InitRequiresConfirm：请求体必须是 `{"confirm": true}`——缺体、
// 形状非法、缺字段、false、多余字段一律 400，且**零调用**（状态与初始化都不触达）。
// 逆改点：省略即同意 / 多余字段被忽略 / 校验晚于状态判定 → 对应子例红。
func TestInstanceSecrets_InitRequiresConfirm(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing body", ``, instanceSecretsInitBodyMessage},
		{"malformed", `{"confirm":`, instanceSecretsInitBodyMessage},
		{"unknown field", `{"confirm":true,"force":true}`, instanceSecretsInitBodyMessage},
		{"empty object", `{}`, instanceSecretsConfirmMessage},
		{"confirm false", `{"confirm":false}`, instanceSecretsConfirmMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeInstanceSecrets{status: readySecretsStatus()}
			s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
			rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", tc.body))
			require.Equal(t, http.StatusBadRequest, rec.Code)
			env := decodePolicyError(t, rec)
			require.Equal(t, codeBadRequest, env.Code)
			require.Equal(t, tc.want, env.Message)
			require.Zero(t, fake.initCalls)
			require.Zero(t, fake.statusCalls, "请求体校验必须先于状态判定")
		})
	}
}

// TestInstanceSecrets_InitConflicts：四种"不可初始化"都在预写之前回 409，且
// **零调用**（因此零写入、零操作行）——已初始化用 `CONFLICT`，恢复态用
// `SECRETS_RECOVERY_REQUIRED` 并透出恢复原因，待 adopt 与纯 env 实例各说各的原因。
// 逆改点：409 走成 500 / 两种 409 混用业务码 / 恢复原因丢失 / 判定晚于 Init → 红。
func TestInstanceSecrets_InitConflicts(t *testing.T) {
	confirm := `{"confirm":true}`

	t.Run("already ready", func(t *testing.T) {
		fake := &fakeInstanceSecrets{status: readySecretsStatus()}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", confirm))
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeConflict, env.Code)
		require.Equal(t, instanceSecretsReadyConflict, env.Message)
		require.NotContains(t, env.Data, "operation_id", "预写之前没有操作行")
		require.Zero(t, fake.initCalls)
	})

	recovery := []struct {
		reason string
		text   string
	}{
		{"missing", "密钥文件不存在"},
		{"corrupt", "密钥文件损坏或校验失败"},
		{"key_id_mismatch", "密钥与初始化记录不符"},
		{"unprovable_provenance", "存在无法证明来源的密钥文件"},
		{"unavailable", "无法判定（读取失败）"},
	}
	for _, tc := range recovery {
		t.Run("recovery: "+tc.reason, func(t *testing.T) {
			fake := &fakeInstanceSecrets{status: InstanceSecretsStatus{
				State: "recovery_required", Reason: tc.reason, KeyFile: "/var/lib/basalt/master.key",
			}}
			s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
			rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", confirm))
			require.Equal(t, http.StatusConflict, rec.Code)
			env := decodePolicyError(t, rec)
			require.Equal(t, codeSecretsRecoveryRequired, env.Code)
			require.Equal(t, instanceSecretsRecoveryPrefix+tc.text+instanceSecretsRecoverySuffix, env.Message)
			require.Zero(t, fake.initCalls)
		})
	}

	t.Run("pure env instance has nothing to initialize", func(t *testing.T) {
		fake := &fakeInstanceSecrets{status: InstanceSecretsStatus{State: "uninitialized", Reason: ""}}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", confirm))
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeConflict, env.Code)
		require.Equal(t, instanceSecretsUnconfigured, env.Message)
		require.Zero(t, fake.initCalls)
	})

	t.Run("awaiting adopt must not mint a second key", func(t *testing.T) {
		fake := &fakeInstanceSecrets{status: InstanceSecretsStatus{
			State: "uninitialized", Reason: "", KeyFile: "/var/lib/basalt/master.key",
		}}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", confirm))
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeConflict, env.Code)
		require.Equal(t, instanceSecretsAwaitAdopt, env.Message)
		require.Zero(t, fake.initCalls)
	})
}

// TestInstanceSecrets_InitSuccess：成功后返回的是**重新读出**的投影（状态一次用于
// 前置判定、一次用于组装响应），字段集仍过契约 schema。
// 逆改点：回显服务层中间状态（少一次读取）/ 响应字段漂移 → 红。
func TestInstanceSecrets_InitSuccess(t *testing.T) {
	ready := readySecretsStatus()
	fake := &fakeInstanceSecrets{
		status: InstanceSecretsStatus{
			State: "uninitialized", Reason: "", CanInitialize: true,
			KeyFile: ready.KeyFile,
		},
	}
	// 初始化成功后端口的状态随之变成 ready——重读因此能给出新投影。
	fake.initFn = func(context.Context, core.Actor) (string, error) {
		fake.status = ready
		return "", nil
	}
	writeActor := core.Actor{
		Subject: "token:0123456789abcdef", Method: core.AuthMethodBearer,
		Source: "127.0.0.1:5000",
	}
	s := newTestServer(t, func(o *Options) {
		o.InstanceSecrets = fake
		o.Resources = summaryFixture()
		o.Notify = summaryChannels()
		o.AdminAuth = fixedActorAuth{actor: writeActor}
	})

	rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", `{"confirm":true}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw resourcesEnvelope[map[string]any]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	requireResourcePayloadMatches(t, readContractDoc(t, "openapi.yaml"), "InstanceSecretsView", raw.Data)

	view := decodeResourcesEnvelope[InstanceSecretsView](t, rec).Data
	require.Equal(t, "ready", view.State)
	require.Equal(t, ready.KeyID, view.KeyID)
	require.Equal(t, "2026-10-26T09:15:00Z", view.InitializedAt)
	require.Equal(t, 1, fake.initCalls)
	require.Equal(t, 2, fake.statusCalls, "前置判定一次 + 成功响应重读一次")
	require.Equal(t, writeActor, fake.lastActor, "初始化审计必须归因到发起动作的管理员")
}

// TestInstanceSecrets_InitFailures：预写之后的失败回 500 并带 `data.operation_id`；
// 预写之前的并发拒绝（ErrCredentialUnavailable 且无标识）回 409 且不生成密钥；
// 其余无标识失败回 500 但**不**带操作标识——"有标识却没有操作行"不可出现。
// 逆改点：500 吞掉操作标识 / 并发拒绝回 500 / 无标识时编造标识 → 红。
func TestInstanceSecrets_InitFailures(t *testing.T) {
	uninitialized := InstanceSecretsStatus{
		State: "uninitialized", Reason: "", CanInitialize: true,
		KeyFile: "/var/lib/basalt/master.key",
	}
	confirm := `{"confirm":true}`

	t.Run("post-prewrite failure carries the operation id", func(t *testing.T) {
		fake := &fakeInstanceSecrets{
			status: uninitialized,
			initFn: func(context.Context, core.Actor) (string, error) {
				return "op-secrets-1", errors.New("disk full")
			},
		}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", confirm))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.Equal(t, messageInternal, env.Message)
		require.Equal(t, "op-secrets-1", env.Data["operation_id"])
		require.NotContains(t, rec.Body.String(), "disk full", "底层原因只进日志")
	})

	t.Run("pre-write race rejection is a 409", func(t *testing.T) {
		fake := &fakeInstanceSecrets{
			status: uninitialized,
			initFn: func(context.Context, core.Actor) (string, error) {
				return "", fmt.Errorf("%w: 另一个初始化者已发布主密钥", core.ErrCredentialUnavailable)
			},
		}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", confirm))
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeConflict, env.Code)
		require.Equal(t, instanceSecretsRaceRefused, env.Message)
		require.NotContains(t, env.Data, "operation_id")
	})

	t.Run("failure without a pre-written row carries no id", func(t *testing.T) {
		fake := &fakeInstanceSecrets{
			status: uninitialized,
			initFn: func(context.Context, core.Actor) (string, error) {
				return "", errors.New("residue check failed")
			},
		}
		s := newInstanceSecretsServer(t, fake, summaryFixture(), summaryChannels())
		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/instance/secrets/init", confirm))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		env := decodePolicyError(t, rec)
		require.Equal(t, codeInternal, env.Code)
		require.NotContains(t, env.Data, "operation_id")
	})
}
