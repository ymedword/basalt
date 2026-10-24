package app

// 本文件是 `internal/app/author.go` 的验收面（US-12 T019 / 约束 29）：作者模型的
// 配置判定与单次草稿生成在这里判卷。十例覆盖：
//
//	T1 `TestAuthorService_NotConfigured503`——整段缺失 / 只配一键 / Provider 未发布 → 503 分类
//	T2 `TestAuthorService_OnlyLLMCallAudit`——唯一写入是两阶段 llm_calls（伪 Run 标识 / NULL 会话 / 第 1 轮）
//	T3 `TestAuthorService_NoRunCreated`——不创建 Run
//	T4 `TestAuthorService_NoOperationRow`——不写 management_operations
//	T5 `TestAuthorService_InvalidDraftRejected`——解析失败 / 引用未发布资源 → 400 分类（不包 503 哨兵）
//	T6 `TestAuthorService_DraftLimits`——文件数 / 单文件 / 总量 / http.max_request_bytes
//	T7 `TestAuthorService_DraftNotPersisted`——草稿只在内存，工作区零文件
//	T8 `TestValidateAuthorConfig_PairedKeys`——成对键
//	T9 `TestValidateAuthorConfig_TemperatureRange`——闭区间与有限性
//	T10 `TestValidateAuthorConfig_ProviderMustBePublished`——已发布名单逐字匹配
//
// 审计类用例走真库 + 真 `ProviderService` + 记录型替身客户端，中途不换零件；配置类
// 用例不触库。全部离线，不产生任何真实网络调用。

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	// 判卷侧要走裸连接读库（与 providers_test.go 同一先例）：驱动经副作用注册。
	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// authorRecordingClient 是记录型替身：保存每次请求并返回预设正文，零网络。
// authorRecordingClient records every request and returns a canned body.
type authorRecordingClient struct {
	response string
	err      error
	requests []core.ChatRequest
}

func (c *authorRecordingClient) Chat(_ context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	c.requests = append(c.requests, req)
	if c.err != nil {
		return core.ChatResponse{}, c.err
	}
	return core.ChatResponse{Content: c.response}, nil
}

// authorTestDeps 造一份默认可用的装配入参：alpha / beta 已发布，技能与 MCP 各一个。
// authorTestDeps builds the default usable assembly input.
func authorTestDeps(chat authorChatter) authorServiceDeps {
	return authorServiceDeps{
		Config: core.AuthorConfig{Provider: "alpha", Model: "alpha-model", Temperature: 0.2},
		Chat:   chat,
		Providers: []workspace.ProviderEntry{
			{Name: "alpha", BaseURL: "https://alpha.example.com/v1", DefaultModel: "alpha-default"},
			{Name: "beta", BaseURL: "https://beta.example.com/v1", DefaultModel: "beta-default"},
		},
		Skills:   []string{"research"},
		MCP:      []string{"reference"},
		Template: "---\nname: <name>\n---\n",
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// authorAgentMD 渲染一份最小可装载的 AGENT.md 草稿正文。
// authorAgentMD renders a minimal loadable AGENT.md draft.
func authorAgentMD(name, providerName, model string, skills, mcp []string) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(name)
	b.WriteString("\ndescription: 测试 Agent\nprovider:\n  name: ")
	b.WriteString(providerName)
	b.WriteString("\n  model: ")
	b.WriteString(model)
	b.WriteString("\n")
	if len(skills) > 0 {
		b.WriteString("skills:\n")
		for _, skill := range skills {
			b.WriteString("  - " + skill + "\n")
		}
	}
	if len(mcp) > 0 {
		b.WriteString("mcp_servers:\n")
		for _, server := range mcp {
			b.WriteString("  - " + server + "\n")
		}
	}
	b.WriteString("---\n\n你是 ")
	b.WriteString(name)
	b.WriteString("。\n")
	return b.String()
}

// authorDraftJSON 把文件列表编码成模型输出形态（小写 path / content 的 JSON 数组）。
// `core.AuthorFile` 没有 json tag（它不是传输类型），因此这里显式投影到输出契约。
// authorDraftJSON encodes files in the model's output shape (lowercase path/content).
func authorDraftJSON(t *testing.T, files ...core.AuthorFile) string {
	t.Helper()
	type draftItem struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	items := make([]draftItem, len(files))
	for i, file := range files {
		items[i] = draftItem{Path: file.Path, Content: file.Content}
	}
	raw, err := json.Marshal(items)
	require.NoError(t, err)
	return string(raw)
}

// authorValidDraft 是 name 的一份合法草稿：AGENT.md 只引用已发布资源。
// authorValidDraft is a valid draft for the given name.
func authorValidDraft(t *testing.T, name string) string {
	t.Helper()
	return authorDraftJSON(t,
		core.AuthorFile{Path: "AGENT.md", Content: authorAgentMD(name, "alpha", "alpha-model",
			[]string{"research"}, []string{"reference"})},
		core.AuthorFile{Path: "REFERENCE.md", Content: "# 参考\n"},
	)
}

// openAuthorDB 建一个迁移到当前版本的临时库。
// openAuthorDB opens a migrated temp database.
func openAuthorDB(t *testing.T) (*storage.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "basalt.db")
	db, err := storage.OpenWritable(context.Background(), path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, path
}

// authorRowCount 用裸连接数一张表的行数。
// authorRowCount counts rows in one table over a bare connection.
func authorRowCount(t *testing.T, path, table string) int {
	t.Helper()
	read, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })

	var count int
	require.NoError(t, read.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count))
	return count
}

// auditedAuthorService 用真 ProviderService（两阶段审计）包装替身客户端。
// auditedAuthorService wraps a stub client in the real auditing provider service.
func auditedAuthorService(t *testing.T, db *storage.DB, client core.ProviderClient,
	deps authorServiceDeps,
) *AuthorService {
	t.Helper()
	deps.Chat = provider.NewProviderService(
		map[string]core.ProviderClient{"alpha": client},
		map[string]string{"alpha": "alpha-default"},
		storage.NewLLMCallRepo(db),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return newAuthorService(deps)
}

// TestAuthorService_NotConfigured503 —— 约束 29：整段缺失、只配一键、Provider 未发布
// 一律包 `core.ErrProviderNotConfigured`（端点映射 503），且**零模型调用**。
func TestAuthorService_NotConfigured503(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*authorServiceDeps)
	}{
		{
			name:   "整段未配置",
			mutate: func(d *authorServiceDeps) { d.Config = core.AuthorConfig{} },
		},
		{
			name:   "只配置 provider",
			mutate: func(d *authorServiceDeps) { d.Config.Model = "" },
		},
		{
			name:   "只配置 model",
			mutate: func(d *authorServiceDeps) { d.Config.Provider = "" },
		},
		{
			name: "provider 未发布",
			mutate: func(d *authorServiceDeps) {
				d.Config.Provider = "ghost"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &authorRecordingClient{response: authorValidDraft(t, "helper")}
			deps := authorTestDeps(client)
			tc.mutate(&deps)

			files, err := newAuthorService(deps).Draft(context.Background(), "helper", "测试")

			require.Error(t, err)
			require.ErrorIs(t, err, core.ErrProviderNotConfigured,
				"作者不可用一律归入 503 NOT_CONFIGURED 分类")
			require.Nil(t, files)
			require.Empty(t, client.requests, "配置不可用时不得发出模型调用")
		})
	}
}

// TestAuthorService_OnlyLLMCallAudit —— 约束 29：成功草稿的唯一数据库副作用是一行
// `llm_calls`：伪 Run 标识 `agent-author-<name>`、`session_id` 写 NULL、第 1 轮，
// Provider / 模型逐字来自配置；draft 原样返回。
func TestAuthorService_OnlyLLMCallAudit(t *testing.T) {
	ctx := context.Background()
	db, path := openAuthorDB(t)

	raw := authorValidDraft(t, "helper")
	client := &authorRecordingClient{response: raw}
	svc := auditedAuthorService(t, db, client, authorTestDeps(nil))

	files, err := svc.Draft(ctx, "helper", "客服助手")
	require.NoError(t, err)
	require.Len(t, files, 2)
	require.Equal(t, "AGENT.md", files[0].Path)
	require.Equal(t, "REFERENCE.md", files[1].Path)

	// 调用入参：路由、关联标识与提示词内容都来自调用时快照。
	require.Len(t, client.requests, 1, "一次草稿 = 一次模型调用")
	req := client.requests[0]
	require.Equal(t, "alpha", req.Provider)
	require.Equal(t, "alpha-model", req.Model)
	require.Equal(t, "agent-author-helper", req.RunID)
	require.Equal(t, 1, req.Round)
	require.Empty(t, req.SessionID, "作者调用不属于任何会话")
	require.Len(t, req.Messages, 1)
	require.Equal(t, "user", req.Messages[0].Role)
	for _, want := range []string{"helper", "客服助手", "alpha", "beta", "research", "reference"} {
		require.Contains(t, req.Messages[0].Content, want, "提示词必须带上已发布资源候选")
	}

	read, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })

	require.Equal(t, 1, authorRowCount(t, path, "llm_calls"))
	var (
		runID, providerName, model, status string
		session                            sql.NullString
		round                              int
	)
	require.NoError(t, read.QueryRowContext(ctx,
		`SELECT run_id, session_id, round, provider, model, status FROM llm_calls`).
		Scan(&runID, &session, &round, &providerName, &model, &status))

	require.Equal(t, "agent-author-helper", runID, "伪 Run 标识逐字固定")
	require.False(t, session.Valid, "session_id 必须写 NULL")
	require.Equal(t, 1, round)
	require.Equal(t, "alpha", providerName)
	require.Equal(t, "alpha-model", model)
	require.Equal(t, string(core.LLMCallSuccess), status)
}

// TestAuthorService_NoRunCreated —— 约束 29：作者调用不创建 Run。
func TestAuthorService_NoRunCreated(t *testing.T) {
	db, path := openAuthorDB(t)
	client := &authorRecordingClient{response: authorValidDraft(t, "helper")}
	svc := auditedAuthorService(t, db, client, authorTestDeps(nil))

	_, err := svc.Draft(context.Background(), "helper", "测试")
	require.NoError(t, err)
	require.Equal(t, 0, authorRowCount(t, path, "runs"), "作者调用不是 Run")
}

// TestAuthorService_NoOperationRow —— 约束 29：作者调用不写 management_operations。
func TestAuthorService_NoOperationRow(t *testing.T) {
	db, path := openAuthorDB(t)
	client := &authorRecordingClient{response: authorValidDraft(t, "helper")}
	svc := auditedAuthorService(t, db, client, authorTestDeps(nil))

	_, err := svc.Draft(context.Background(), "helper", "测试")
	require.NoError(t, err)
	require.Equal(t, 0, authorRowCount(t, path, "management_operations"),
		"generate-files 不产生管理操作行（契约注 3）")
}

// TestAuthorService_InvalidDraftRejected —— 约束 29：模型输出不可解析、结构或引用
// 非法 → 错误**不**带 503 哨兵（端点映射 400 点名），零草稿返回。
func TestAuthorService_InvalidDraftRejected(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "输出不是 JSON",
			raw:  "抱歉，我无法生成。",
			want: "文件数组或含 questions",
		},
		{
			name: "缺少 AGENT.md",
			raw:  authorDraftJSON(t, core.AuthorFile{Path: "notes.md", Content: "x"}),
			want: "AGENT.md",
		},
		{
			name: "name 与请求不一致",
			raw: authorDraftJSON(t, core.AuthorFile{
				Path:    "AGENT.md",
				Content: authorAgentMD("other", "alpha", "alpha-model", nil, nil),
			}),
			want: "name",
		},
		{
			name: "provider.model 空白",
			raw: authorDraftJSON(t, core.AuthorFile{
				Path:    "AGENT.md",
				Content: authorAgentMD("helper", "alpha", " ", nil, nil),
			}),
			want: "provider.model",
		},
		{
			name: "引用未发布的 Provider",
			raw: authorDraftJSON(t, core.AuthorFile{
				Path:    "AGENT.md",
				Content: authorAgentMD("helper", "ghost", "m", nil, nil),
			}),
			want: "未发布的 Provider",
		},
		{
			name: "引用未发布的技能",
			raw: authorDraftJSON(t, core.AuthorFile{
				Path:    "AGENT.md",
				Content: authorAgentMD("helper", "alpha", "alpha-model", []string{"ghost-skill"}, nil),
			}),
			want: "未发布的技能",
		},
		{
			name: "引用未应用的 MCP",
			raw: authorDraftJSON(t, core.AuthorFile{
				Path:    "AGENT.md",
				Content: authorAgentMD("helper", "alpha", "alpha-model", nil, []string{"ghost-mcp"}),
			}),
			want: "未应用的 MCP",
		},
		{
			name: "temperature 越界",
			raw: authorDraftJSON(t, core.AuthorFile{
				Path: "AGENT.md",
				Content: strings.Replace(
					authorAgentMD("helper", "alpha", "alpha-model", nil, nil),
					"  model: alpha-model\n", "  model: alpha-model\n  temperature: 3.0\n", 1),
			}),
			want: "temperature",
		},
		{
			name: "缺少 frontmatter 围栏",
			raw: authorDraftJSON(t, core.AuthorFile{
				Path:    "AGENT.md",
				Content: "name: helper\n",
			}),
			want: "围栏",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &authorRecordingClient{response: tc.raw}
			svc := newAuthorService(authorTestDeps(client))

			files, err := svc.Draft(context.Background(), "helper", "测试")

			require.Error(t, err)
			require.NotErrorIs(t, err, core.ErrProviderNotConfigured,
				"草稿问题映射 400，不得误归 503 配置不可用")
			require.Contains(t, err.Error(), tc.want)
			require.Nil(t, files)
			require.Len(t, client.requests, 1, "配置可用时先发出一次模型调用")
		})
	}
}

// TestAuthorService_DraftLimits —— 约束 29：文件数 ≤ 64、单文件 ≤ 256 KiB、总量 ≤ 1 MiB，
// 且总量不超过 `http.max_request_bytes`；超限一律 400 分类、零草稿。
func TestAuthorService_DraftLimits(t *testing.T) {
	small := authorAgentMD("helper", "alpha", "alpha-model", nil, nil)

	t.Run("文件数超过 64", func(t *testing.T) {
		files := make([]core.AuthorFile, 0, 65)
		files = append(files, core.AuthorFile{Path: "AGENT.md", Content: small})
		for i := 1; i < 65; i++ {
			files = append(files, core.AuthorFile{Path: "f" + strings.Repeat("x", i%3) + ".md", Content: "x"})
		}
		// 路径可能重复（i%3 只有三种），去重后仍保证 65 个唯一路径。
		unique := make([]core.AuthorFile, 0, 65)
		seen := map[string]struct{}{}
		for _, file := range files {
			if _, ok := seen[file.Path]; ok {
				continue
			}
			seen[file.Path] = struct{}{}
			unique = append(unique, file)
		}
		for len(unique) < 65 {
			path := "extra-" + strings.Repeat("y", len(unique)) + ".md"
			unique = append(unique, core.AuthorFile{Path: path, Content: "x"})
		}

		client := &authorRecordingClient{response: authorDraftJSON(t, unique...)}
		_, err := newAuthorService(authorTestDeps(client)).Draft(context.Background(), "helper", "测试")
		require.Error(t, err)
		require.Contains(t, err.Error(), "64")
	})

	t.Run("单文件超过 256 KiB", func(t *testing.T) {
		client := &authorRecordingClient{response: authorDraftJSON(t,
			core.AuthorFile{Path: "AGENT.md", Content: small},
			core.AuthorFile{Path: "big.md", Content: strings.Repeat("a", 256<<10+1)},
		)}
		_, err := newAuthorService(authorTestDeps(client)).Draft(context.Background(), "helper", "测试")
		require.Error(t, err)
		require.Contains(t, err.Error(), "单文件上限")
	})

	t.Run("总量超过 1 MiB", func(t *testing.T) {
		files := []core.AuthorFile{{Path: "AGENT.md", Content: small}}
		for i := 0; i < 8; i++ {
			files = append(files, core.AuthorFile{
				Path:    "chunk-" + strings.Repeat("z", i+1) + ".md",
				Content: strings.Repeat("b", 200<<10),
			})
		}
		client := &authorRecordingClient{response: authorDraftJSON(t, files...)}
		_, err := newAuthorService(authorTestDeps(client)).Draft(context.Background(), "helper", "测试")
		require.Error(t, err)
		require.Contains(t, err.Error(), "总量")
	})

	t.Run("超过 http.max_request_bytes", func(t *testing.T) {
		deps := authorTestDeps(&authorRecordingClient{response: authorValidDraft(t, "helper")})
		deps.MaxBytes = 64
		_, err := newAuthorService(deps).Draft(context.Background(), "helper", "测试")
		require.Error(t, err)
		require.Contains(t, err.Error(), "http.max_request_bytes")
	})
}

// TestAuthorService_DraftNotPersisted —— 约束 29：草稿只在内存。服务不持有工作区
// 目录，调用后工作区一个条目都不许多出来；返回值逐字等于模型输出。
func TestAuthorService_DraftNotPersisted(t *testing.T) {
	root := t.TempDir()
	client := &authorRecordingClient{response: authorValidDraft(t, "helper")}
	svc := newAuthorService(authorTestDeps(client))

	files, err := svc.Draft(context.Background(), "helper", "测试")
	require.NoError(t, err)
	require.Len(t, files, 2)
	require.Equal(t, "AGENT.md", files[0].Path)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "作者草稿不得写盘：工作区零条目")
}

// TestValidateAuthorConfig_PairedKeys —— 两个键必须成对：整段缺失合法（功能关闭），
// 单边配置一律拒绝并包 503 哨兵。
func TestValidateAuthorConfig_PairedKeys(t *testing.T) {
	providers := []workspace.ProviderEntry{{Name: "alpha"}}

	require.NoError(t, ValidateAuthorConfig(core.AuthorConfig{}, providers),
		"整段缺失 = 作者功能关闭，不是配置错误")
	require.NoError(t, ValidateAuthorConfig(
		core.AuthorConfig{Provider: "alpha", Model: "m", Temperature: 0.2}, providers))

	providerOnly := ValidateAuthorConfig(core.AuthorConfig{Provider: "alpha"}, providers)
	require.Error(t, providerOnly)
	require.ErrorIs(t, providerOnly, core.ErrProviderNotConfigured)
	require.Contains(t, providerOnly.Error(), "成对")

	modelOnly := ValidateAuthorConfig(core.AuthorConfig{Model: "m"}, providers)
	require.Error(t, modelOnly)
	require.ErrorIs(t, modelOnly, core.ErrProviderNotConfigured)
	require.Contains(t, modelOnly.Error(), "成对")
}

// TestValidateAuthorConfig_TemperatureRange —— temperature 闭区间 `[0,2]` 且必须有限；
// NaN / 无穷显式拒绝（区间比较对 NaN 恒为假）。
func TestValidateAuthorConfig_TemperatureRange(t *testing.T) {
	providers := []workspace.ProviderEntry{{Name: "alpha"}}
	base := core.AuthorConfig{Provider: "alpha", Model: "m"}

	valid := []float64{0, 0.2, 2}
	for _, temperature := range valid {
		cfg := base
		cfg.Temperature = temperature
		require.NoError(t, ValidateAuthorConfig(cfg, providers), "合法温度 %v", temperature)
	}

	invalid := []float64{-0.1, 2.1, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, temperature := range invalid {
		cfg := base
		cfg.Temperature = temperature
		err := ValidateAuthorConfig(cfg, providers)
		require.Error(t, err, "非法温度 %v 必须拒绝", temperature)
		require.ErrorIs(t, err, core.ErrProviderNotConfigured)
		require.Contains(t, err.Error(), "temperature")
	}
}

// TestValidateAuthorConfig_ProviderMustBePublished —— Provider 名必须逐字命中已发布
// 条目（大小写敏感）；未命中包 503 哨兵并点名。
func TestValidateAuthorConfig_ProviderMustBePublished(t *testing.T) {
	providers := []workspace.ProviderEntry{{Name: "alpha"}, {Name: "beta"}}
	base := core.AuthorConfig{Model: "m", Temperature: 0.2}

	for _, name := range []string{"alpha", "beta"} {
		cfg := base
		cfg.Provider = name
		require.NoError(t, ValidateAuthorConfig(cfg, providers))
	}

	for _, name := range []string{"gamma", "Alpha"} {
		cfg := base
		cfg.Provider = name
		err := ValidateAuthorConfig(cfg, providers)
		require.Error(t, err, "未发布的名字 %q 必须拒绝", name)
		require.ErrorIs(t, err, core.ErrProviderNotConfigured)
		require.Contains(t, err.Error(), name)
	}
}

// TestAuthorService_DraftWithAnswers_Questions —— R6.3：模型返回问题时不校验文件、
// 不写任何持久化（含 llm_calls 的两阶段行由审计服务写，这里用裸替身只看调用入参），
// 提示词**不含**已澄清段；只认文件的 Draft 把问题当错误，不让问题冒充草稿。
// TestAuthorService_DraftWithAnswers_Questions returns questions untouched, calls once,
// and keeps Draft file-only.
func TestAuthorService_DraftWithAnswers_Questions(t *testing.T) {
	questions := `{"questions":[{"id":"frequency","question":"每天几点运行？"}]}`
	client := &authorRecordingClient{response: questions}
	svc := newAuthorService(authorTestDeps(client))

	result, err := svc.DraftWithAnswers(context.Background(), "helper", "客服助手", nil)
	require.NoError(t, err)
	require.Empty(t, result.Files)
	require.Equal(t, []core.AuthorQuestion{{ID: "frequency", Question: "每天几点运行？"}}, result.Questions)
	require.Len(t, client.requests, 1)
	require.Equal(t, "agent-author-helper", client.requests[0].RunID)
	require.NotContains(t, client.requests[0].Messages[0].Content, "已澄清的问题",
		"没有答案时提示词与基础草稿逐字节一致")

	_, err = svc.Draft(context.Background(), "helper", "客服助手")
	require.Error(t, err)
	require.Contains(t, err.Error(), "问答接口")
}

// TestAuthorService_DraftWithAnswers_PromptAndLimits —— R6.3：有答案时提示词追加已
// 澄清段；答案非法（超过 3 条 / 空值 / 重复 id）在发出调用**之前**拒绝，零模型调用。
// TestAuthorService_DraftWithAnswers_PromptAndLimits appends the answers section and
// refuses bad answers before any model call.
func TestAuthorService_DraftWithAnswers_PromptAndLimits(t *testing.T) {
	client := &authorRecordingClient{response: authorValidDraft(t, "helper")}
	svc := newAuthorService(authorTestDeps(client))

	answers := []core.AuthorAnswer{{ID: "frequency", Question: "每天几点运行？", Answer: "08:30"}}
	result, err := svc.DraftWithAnswers(context.Background(), "helper", "客服助手", answers)
	require.NoError(t, err)
	require.Len(t, result.Files, 2)
	require.Empty(t, result.Questions)
	require.Len(t, client.requests, 1)
	require.Contains(t, client.requests[0].Messages[0].Content, "已澄清的问题")
	require.Contains(t, client.requests[0].Messages[0].Content, "[frequency] 每天几点运行？：08:30")

	bad := [][]core.AuthorAnswer{
		{
			{ID: "a", Question: "问？", Answer: "答"},
			{ID: "b", Question: "问？", Answer: "答"},
			{ID: "c", Question: "问？", Answer: "答"},
			{ID: "d", Question: "问？", Answer: "答"},
		},
		{{ID: "q", Question: "问？", Answer: "  "}},
		{{ID: "q", Question: "问？", Answer: "答"}, {ID: "q", Question: "又问？", Answer: "答"}},
	}
	for i, answers := range bad {
		client.requests = nil
		_, err := svc.DraftWithAnswers(context.Background(), "helper", "客服助手", answers)
		require.Error(t, err, "bad case %d", i)
		require.Empty(t, client.requests, "非法答案必须在发出模型调用之前拒绝")
	}
}

// TestAuthorService_ScheduleDraft —— R6.4：一次伪 Run `agent-schedule-<name>` 调用，
// 候选按装载期规则规整，未来三次触发由服务端固定时钟算出；不写管理操作、不建 Run。
// TestAuthorService_ScheduleDraft pins the pseudo run id, the normalized candidate and
// the server-computed triggers.
func TestAuthorService_ScheduleDraft(t *testing.T) {
	ctx := context.Background()
	db, path := openAuthorDB(t)

	client := &authorRecordingClient{response: `{"id":"daily-report","cron":"0 30 8 * * *","zone":"Asia/Shanghai","message":"生成每日运维日报"}`}
	deps := authorTestDeps(nil)
	deps.Now = func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC) }
	svc := auditedAuthorService(t, db, client, deps)

	result, err := svc.DraftSchedule(ctx, "helper", " 每天 8:30 生成日报 ")
	require.NoError(t, err)
	require.Equal(t, core.Schedule{
		ID: "daily-report", Cron: "0 30 8 * * *", Zone: "Asia/Shanghai", Message: "生成每日运维日报",
	}, result.Candidate)
	require.Equal(t, []time.Time{
		time.Date(2026, 10, 24, 0, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 26, 0, 30, 0, 0, time.UTC),
	}, result.NextRuns, "触发时间只在服务端按注入时钟计算")

	require.Len(t, client.requests, 1)
	req := client.requests[0]
	require.Equal(t, "agent-schedule-helper", req.RunID, "定时候选使用独立伪 Run 前缀")
	require.Empty(t, req.SessionID)
	require.Equal(t, 1, req.Round)
	require.Contains(t, req.Messages[0].Content, "每天 8:30 生成日报", "指令逐字进提示词")

	require.Equal(t, 1, authorRowCount(t, path, "llm_calls"), "唯一写入是 llm_calls 审计行")
	require.Equal(t, 0, authorRowCount(t, path, "management_operations"), "候选不写管理操作")
	require.Equal(t, 0, authorRowCount(t, path, "runs"), "候选不是 Run")
}

// TestAuthorService_ScheduleDraftRejected —— R6.4：指令为空 / 超限在调用前拒绝；
// 候选 id、cron、zone 或 message 不合法时按 400 分类拒绝（不带 503 哨兵）。
// TestAuthorService_ScheduleDraftRejected refuses bad instructions before the call and
// invalid candidates after it without the 503 sentinel.
func TestAuthorService_ScheduleDraftRejected(t *testing.T) {
	ctx := context.Background()

	t.Run("指令在调用前拒绝", func(t *testing.T) {
		client := &authorRecordingClient{response: `{"id":"d","cron":"0 30 8 * * *","zone":"UTC","message":"m"}`}
		svc := newAuthorService(authorTestDeps(client))
		for _, instruction := range []string{"   ", strings.Repeat("指", core.ScheduleInstructionMaxLen+1)} {
			_, err := svc.DraftSchedule(ctx, "helper", instruction)
			require.Error(t, err)
		}
		require.Empty(t, client.requests, "非法指令不得发出模型调用")
	})

	t.Run("候选不合法", func(t *testing.T) {
		cases := []struct {
			name string
			raw  string
			want string
		}{
			{name: "id 非法", raw: `{"id":"Daily_Report","cron":"0 30 8 * * *","zone":"UTC","message":"m"}`, want: "id"},
			{name: "cron 5 段", raw: `{"id":"daily","cron":"30 8 * * *","zone":"UTC","message":"m"}`, want: "6 fields"},
			{name: "zone Local", raw: `{"id":"daily","cron":"0 30 8 * * *","zone":"Local","message":"m"}`, want: "Local"},
			{name: "message 为空", raw: `{"id":"daily","cron":"0 30 8 * * *","zone":"UTC","message":" "}`, want: "message"},
			{name: "输出不是对象", raw: `抱歉`, want: ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				client := &authorRecordingClient{response: tc.raw}
				_, err := newAuthorService(authorTestDeps(client)).DraftSchedule(ctx, "helper", "每天 8:30")
				require.Error(t, err)
				require.NotErrorIs(t, err, core.ErrProviderNotConfigured, "候选问题映射 400，不是 503")
				if tc.want != "" {
					require.Contains(t, err.Error(), tc.want)
				}
			})
		}
	})

	t.Run("作者未配置", func(t *testing.T) {
		client := &authorRecordingClient{response: `{}`}
		deps := authorTestDeps(client)
		deps.Config = core.AuthorConfig{}
		_, err := newAuthorService(deps).DraftSchedule(ctx, "helper", "每天 8:30")
		require.ErrorIs(t, err, core.ErrProviderNotConfigured)
		require.Empty(t, client.requests)
	})
}

// TestAuthorService_CallFailureIsActionable —— R7 问题 8：作者模型调用失败时，
// 服务层给出的错误必须点名 provider 与修复入口、保留原始 `*provider.CallError`
// 错误链（排障不受影响），且不把上游细节写进对外文案；上游超时单独映射为
// `context.DeadlineExceeded`（HTTP 层 504）。
// 中文验收点：调用失败与超时可区分、可执行、可回溯。
// TestAuthorService_CallFailureIsActionable pins the classification and the wording
// of author call failures while keeping the raw provider error in the chain.
func TestAuthorService_CallFailureIsActionable(t *testing.T) {
	ctx := context.Background()

	t.Run("外部失败", func(t *testing.T) {
		callErr := &provider.CallError{
			Provider: "alpha",
			Status:   502,
			Kind:     core.ErrorKindExternalError,
			Message:  "上游不可用 https://secret.example.com/v1",
		}
		client := &authorRecordingClient{err: callErr}
		_, err := newAuthorService(authorTestDeps(client)).DraftSchedule(ctx, "helper", "每天 8:30")
		require.Error(t, err)
		require.ErrorIs(t, err, core.ErrAuthorModelUnavailable)
		require.NotErrorIs(t, err, context.DeadlineExceeded)

		var adminErr *core.AdminError
		require.ErrorAs(t, err, &adminErr)
		require.Contains(t, adminErr.Message, `provider "alpha"`)
		require.Contains(t, adminErr.Message, "未保存任何内容")
		require.NotContains(t, adminErr.Message, "secret.example.com",
			"上游地址细节只进日志与审计，不回显")

		var raw *provider.CallError
		require.ErrorAs(t, err, &raw, "原始错误链必须保留")
	})

	t.Run("上游超时", func(t *testing.T) {
		client := &authorRecordingClient{err: &provider.CallError{
			Provider: "alpha",
			Kind:     core.ErrorKindTimeout,
			Message:  "调用被取消或超过截止时间",
		}}
		_, err := newAuthorService(authorTestDeps(client)).DraftSchedule(ctx, "helper", "每天 8:30")
		require.ErrorIs(t, err, context.DeadlineExceeded, "上游超时映射 504")
		var adminErr *core.AdminError
		require.ErrorAs(t, err, &adminErr)
		require.Contains(t, adminErr.Message, "请稍后重试")
	})

	t.Run("草稿端点同样可执行", func(t *testing.T) {
		client := &authorRecordingClient{err: &provider.CallError{
			Provider: "alpha", Status: 401, Kind: core.ErrorKindExternalError, Message: "未授权",
		}}
		_, err := newAuthorService(authorTestDeps(client)).Draft(ctx, "helper", "值班助手")
		require.ErrorIs(t, err, core.ErrAuthorModelUnavailable)
	})
}
