package web

// 本文件是结构化配置读端点的 HTTP 判卷（颗粒度文档 R4.3 / T138，check
// `012-web-cfg-structured`）：读面与 `GET /agents/{name}/content` 共用**同一个**
// `content_etag`（响应头与 `data.content_etag` 同值，不引入 `config_etag`）；
// 正文提交缺绑定时在 `pending` **之前**拒绝且磁盘零写入；拒绝之后文件不得被补写
// `resource_bindings`，读面如实报告 `missing` 与空 UID；显式提交的绑定块按原字节
// 落盘，读面回显同一绑定。
// These tests judge the structured config endpoint: one shared content_etag, a
// save-time binding refusal that precedes the pending row, and the no-auto-fill rule
// on both the write and read sides.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// contentBoundUID 是一份形状合法的资源身份（Crockford Base32，不含 I / L / O / U），
// 供"显式绑定"用例使用；它不指向任何已发布资源——本文件的判据是提交与回显，资源
// 状态（发布 / 资格 / 墓碑）由 internal/app 的真实读者用例判卷。
// contentBoundUID is a well-formed resource identity for the explicit-binding case.
const contentBoundUID = "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"

// contentBoundDoc 是显式给出绑定的完整原文：provider 与绑定组同名同 UID。
// contentBoundDoc declares the binding explicitly, matching the provider reference.
const contentBoundDoc = `---
name: reporter
description: "旧描述"
# provider 段前的注释必须保留
identity:
  agent_name: "周报助手"
  prompt: "你是周报助手。"
provider:
  name: "mock"
  model: "mock-model"
tools:
  - "file_read"
resource_bindings:
  model_connection:
    - name: "mock"
      uid: "` + contentBoundUID + `"
---

你是周报助手。
`

// fileConfigService 是 `AgentConfigService` 在测试侧的组装：读取与 ETag 都走生产
// 同一函数（`workspace.InspectAgentForMigration` + `workspace.ContentETag`），按
// 定义的引用顺序给行；只判"缺绑定 / 已绑定 / 孤儿"三态。墓碑 / 旧 UID / 重复与
// 发布 / 资格 / 凭据状态的完整矩阵由 internal/app 的真实读者用例判卷（web 不能
// 导入 app，见 handlers_agent_content_test.go 文件头）。
// fileConfigService replays the adapter test-side with the same read and ETag
// functions; the repository-backed resolution matrix is judged in internal/app.
type fileConfigService struct {
	root string
}

var _ AgentConfigService = (*fileConfigService)(nil)

// Config 读一份定义并投影出结构化视图。
// Config projects one definition into the structured view.
func (s *fileConfigService) Config(_ context.Context, name string) (AgentConfigView, error) {
	inspection, err := workspace.InspectAgentForMigration(s.root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return AgentConfigView{}, &core.AgentNotFoundError{Name: name}
		}
		return AgentConfigView{}, err
	}
	profile := inspection.Profile
	references := append(
		configRowsForTest(core.BindingModelConnection, configProviderNames(profile),
			inspection.Bindings.ModelConnections),
		configRowsForTest(core.BindingNotifyChannel, profile.NotifyChannels,
			inspection.Bindings.NotifyChannels)...)
	return AgentConfigView{
		Agent:            name,
		ContentETag:      workspace.ContentETag(inspection.Raw),
		Fields:           configFieldsForTest(profile),
		References:       references,
		BindingComplete:  configAllBound(references),
		NotifyMode:       string(profile.NotifyMode),
		NotifyModeSource: NotifyModeSourceExplicit,
	}, nil
}

// configProviderNames 把 provider 段投影成引用名（空名 = 未声明）。
// configProviderNames projects the provider block into reference names.
func configProviderNames(profile *core.Profile) []string {
	if profile.Provider.Name == "" {
		return nil
	}
	return []string{profile.Provider.Name}
}

// configFieldsForTest 只填本文件断言到的字段：结构与生产投影一致。
// configFieldsForTest fills only the fields these cases assert.
func configFieldsForTest(profile *core.Profile) AgentConfigFieldsView {
	return AgentConfigFieldsView{
		Name:     profile.Name,
		Identity: AgentConfigIdentityView{AgentName: profile.Identity.AgentName, Prompt: profile.Identity.Prompt},
		Provider: AgentConfigProviderView{
			Name:        profile.Provider.Name,
			Model:       profile.Provider.Model,
			Temperature: profile.Provider.Temperature,
		},
		NotifyChannels: profile.NotifyChannels,
		ResourceBindings: AgentConfigBindingsView{
			ModelConnections: profile.ResourceBindings.ModelConnections,
			NotifyChannels:   profile.ResourceBindings.NotifyChannels,
		},
		Settings: AgentConfigSettingsView{
			MaxIterations:   profile.Settings.MaxIterations,
			MaxHistoryTurns: profile.Settings.MaxHistoryTurns,
		},
	}
}

// configRowsForTest 按"引用为序、孤儿追加"给行：结构判定与生产同一口径。
// configRowsForTest renders rows in reference order with orphans appended.
func configRowsForTest(kind core.BindingKind, refs []string,
	bindings []core.ResourceBinding,
) []AgentConfigReferenceView {
	byName := make(map[string]core.ResourceBinding, len(bindings))
	for _, binding := range bindings {
		byName[binding.Name] = binding
	}
	rows := make([]AgentConfigReferenceView, 0, len(refs)+len(bindings))
	declared := make(map[string]bool, len(refs))
	for _, ref := range refs {
		declared[ref] = true
		row := AgentConfigReferenceView{
			ResourceKind:  string(kind),
			RefName:       ref,
			BindingStatus: BindingStatusMissing,
		}
		if binding, ok := byName[ref]; ok {
			row.ResourceUID = string(binding.UID)
			row.BindingStatus = BindingStatusBound
		}
		rows = append(rows, row)
	}
	for _, binding := range bindings {
		if declared[binding.Name] {
			continue
		}
		rows = append(rows, AgentConfigReferenceView{
			ResourceKind:  string(kind),
			RefName:       binding.Name,
			ResourceUID:   string(binding.UID),
			BindingStatus: BindingStatusOrphan,
		})
	}
	return rows
}

// configAllBound 只判结构完整：发布与资格需要仓储，由 app 层读者判卷。
// configAllBound checks structure only; published and eligibility need the stores.
func configAllBound(refs []AgentConfigReferenceView) bool {
	for _, ref := range refs {
		if ref.BindingStatus != BindingStatusBound {
			return false
		}
	}
	return true
}

// contentBindingAwareValidator 用生产解析函数从候选原文读绑定：负控（无绑定块）
// 与正控（显式绑定）因此都来自真实 frontmatter，而不是手工拼的 Profile。
// contentBindingAwareValidator parses the candidate with the production parser so
// both the negative and positive controls read real frontmatter.
func contentBindingAwareValidator(_ context.Context, agent string,
	raw []byte,
) (core.AgentEntry, string, []core.ManifestEntry, error) {
	bindings, err := workspace.ParseResourceBindings(raw)
	if err != nil {
		return core.AgentEntry{}, "", nil, err
	}
	return core.AgentEntry{
			Name:       agent,
			Registered: true,
			Profile: &core.Profile{
				Name:             agent,
				Provider:         core.ProviderRef{Name: "mock", Model: "mock-model"},
				ResourceBindings: bindings,
			},
		}, "你是" + agent + "。",
		[]core.ManifestEntry{{Path: "AGENT.md", SHA256: strings.Repeat("0", 64)}}, nil
}

// contentCorrespondenceCheck 与适配器同一判据：定义里的每个引用都要有同名绑定。
// contentCorrespondenceCheck mirrors the adapter's correspondence rule.
func contentCorrespondenceCheck(_ context.Context, _ string, profile *core.Profile) error {
	var providerNames []string
	if profile.Provider.Name != "" {
		providerNames = append(providerNames, profile.Provider.Name)
	}
	return profile.ResourceBindings.ValidateCorrespondence(providerNames, profile.NotifyChannels)
}

// newConfigEnv 在内容夹具之上补结构化配置端口：编辑器与服务端是同一实例。
// newConfigEnv adds the config port on top of the content fixture.
func newConfigEnv(t *testing.T, tune func(*workspace.AgentEditorDeps)) *contentEnv {
	t.Helper()
	env := newContentEnvWith(t, tune)
	env.server = newTestServer(t, func(o *Options) {
		o.AdminAuth = testAdminAuth(testToken)
		o.AgentAdmin = env.admin
		o.Runs = env.runs
		o.AgentConfigService = &fileConfigService{root: env.root}
	})
	return env
}

// configPath 给出结构化配置端点路径。
// configPath builds the config endpoint path.
func configPath(name string) string {
	return "/api/v1/agents/" + name + "/config"
}

// configGET 构造带凭据的配置读请求。
// configGET builds an authenticated config read.
func configGET(name string) *http.Request {
	return withBearer(adminRequest(http.MethodGet, configPath(name), ""), testToken)
}

// decodeAgentConfigView 解析配置读面成功信封。
// decodeAgentConfigView decodes the config success envelope.
func decodeAgentConfigView(t *testing.T, rec *httptest.ResponseRecorder) AgentConfigView {
	t.Helper()
	var envelope struct {
		Code string          `json:"code"`
		Data AgentConfigView `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, codeOK, envelope.Code)
	return envelope.Data
}

// TestAgentConfig_SharesContentETag：配置读面与 `/content` 共用同一个强
// `content_etag`——响应头、`data.content_etag` 与 `/content` 的 ETag 三者逐字相同，
// 且响应里**没有** `config_etag`；只读视图不写管理操作行。
// 逆改点：另算一个规范化哈希 → 三值不再相等；引入第二套条件写基准 → 末断言红。
// 中文用例名：配置视图与原文共用 ETag。
func TestAgentConfig_SharesContentETag(t *testing.T) {
	env := newConfigEnv(t, nil)

	contentRec := serveAdmin(env.server, contentGET())
	require.Equal(t, http.StatusOK, contentRec.Code)
	contentETag := contentRec.Header().Get("ETag")
	require.Equal(t, workspace.ContentETag([]byte(contentAgentDoc)), contentETag)

	rec := serveAdmin(env.server, configGET(contentAgentName))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	view := decodeAgentConfigView(t, rec)
	require.Equal(t, contentAgentName, view.Agent)
	require.Equal(t, contentETag, rec.Header().Get("ETag"), "配置读面的 ETag 必须与 /content 逐字相同")
	require.Equal(t, contentETag, view.ContentETag, "响应头与 data.content_etag 必须同值")
	require.Equal(t, "mock", view.Fields.Provider.Name)
	require.Equal(t, "mock-model", view.Fields.Provider.Model)
	require.Empty(t, env.ops.begunRecords(), "只读视图不写管理操作行")

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	data, ok := raw["data"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, data, "config_etag", "不得引入第二套条件写基准")
}

// TestAgentConfig_RequiresBindingsOnContentSubmit：正文声明了 provider 却没有
// `resource_bindings` → 400 `AGENT_RESOURCE_BINDING_MISSING`；绑定校验排在预写
// 之前（零操作行、零快照），磁盘逐字节不变。
// 逆改点：先写 pending 再校验 → 操作行非空；校验晚于写盘 → 磁盘变化。
// 中文用例名：正文提交缺绑定被拒。
func TestAgentConfig_RequiresBindingsOnContentSubmit(t *testing.T) {
	env := newConfigEnv(t, func(deps *workspace.AgentEditorDeps) {
		deps.Validate = contentBindingAwareValidator
		deps.ValidateBindings = contentCorrespondenceCheck
	})
	before := readAgentDoc(t, env.root, contentAgentName)
	oldETag := currentETag(t, env)

	rec := serveAdmin(env.server, contentPUT(contentAgentName,
		contentBody(t, contentAgentDoc+"缺绑定的候选\n"), oldETag))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	envelope := decodeAuthEnvelope(t, rec)
	require.Equal(t, codeAgentResourceBindingMissing, envelope.Code)
	require.Contains(t, envelope.Message, "resource_bindings", "拒绝文案必须指出补绑定的位置")
	require.Equal(t, before, readAgentDoc(t, env.root, contentAgentName), "保存期拒绝必须磁盘零写入")
	require.Empty(t, env.ops.begunRecords(), "绑定校验排在 pending 之前")
	require.Zero(t, env.snapshots.savedCount())
}

// TestAgentConfig_DoesNotAutoFillBindings：拒绝之后文件里不出现绑定块，读面如实
// 报告 `missing` 与空 UID；显式提交的绑定块按原字节落盘，读面回显同一绑定——两个
// 方向都不许"按名称自动补"。
// 逆改点：失败路径补写绑定块 → 首个 NotContains 红；读面按名猜 UID → 空 UID 断言红。
// 中文用例名：保存不自动补绑定。
func TestAgentConfig_DoesNotAutoFillBindings(t *testing.T) {
	t.Run("拒绝之后文件里没有绑定块且读面报告 missing", func(t *testing.T) {
		env := newConfigEnv(t, func(deps *workspace.AgentEditorDeps) {
			deps.Validate = contentBindingAwareValidator
			deps.ValidateBindings = contentCorrespondenceCheck
		})

		rec := serveAdmin(env.server, contentPUT(contentAgentName,
			contentBody(t, contentAgentDoc+"没有绑定\n"), currentETag(t, env)))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.NotContains(t, string(readAgentDoc(t, env.root, contentAgentName)),
			"resource_bindings", "保存失败不得补写绑定块")

		view := decodeAgentConfigView(t, serveAdmin(env.server, configGET(contentAgentName)))
		require.Empty(t, view.Fields.ResourceBindings.ModelConnections,
			"原文没有绑定，视图不得凭空生成")
		require.Len(t, view.References, 1)
		require.Equal(t, string(core.BindingModelConnection), view.References[0].ResourceKind)
		require.Equal(t, "mock", view.References[0].RefName)
		require.Equal(t, BindingStatusMissing, view.References[0].BindingStatus)
		require.Empty(t, view.References[0].ResourceUID, "缺绑定不得按名补 UID")
		require.False(t, view.BindingComplete)
	})

	t.Run("显式绑定按提交字节落盘并回显", func(t *testing.T) {
		env := newConfigEnv(t, func(deps *workspace.AgentEditorDeps) {
			deps.Validate = contentBindingAwareValidator
			deps.ValidateBindings = contentCorrespondenceCheck
		})

		rec := serveAdmin(env.server, contentPUT(contentAgentName,
			contentBody(t, contentBoundDoc), currentETag(t, env)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, contentBoundDoc, string(readAgentDoc(t, env.root, contentAgentName)),
			"编辑器不得改写绑定块")

		view := decodeAgentConfigView(t, serveAdmin(env.server, configGET(contentAgentName)))
		require.Len(t, view.Fields.ResourceBindings.ModelConnections, 1)
		require.Equal(t, "mock", view.Fields.ResourceBindings.ModelConnections[0].Name)
		require.Equal(t, contentBoundUID, string(view.Fields.ResourceBindings.ModelConnections[0].UID))
		require.Len(t, view.References, 1)
		require.Equal(t, BindingStatusBound, view.References[0].BindingStatus)
		require.Equal(t, contentBoundUID, view.References[0].ResourceUID)
		require.True(t, view.BindingComplete)
	})
}
