package app

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	// 判卷侧要走裸连接读库（与 internal/cli/status_test.go 同一先例）：驱动经副作用注册。
	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
)

// providersConfig 造一份装配入参：条目逐条显式，名称用作映射表的键。
// providersConfig builds one assembly input: entries are explicit and their names key the map.
func providersConfig(entries ...cli.ProviderConfig) *cli.Config {
	return &cli.Config{Providers: entries}
}

// TestNewProviderClients_ExplicitMapMatchesConfig —— 宪法 III：映射键集**逐字** = 配置条目名集；
// 无"默认供应商"、无遍历推断留下的额外键（多一个键就是一个隐式兜底）。
func TestNewProviderClients_ExplicitMapMatchesConfig(t *testing.T) {
	cfg := providersConfig(
		cli.ProviderConfig{Name: "alpha", BaseURL: "https://alpha.example.com/v1", DefaultModel: "m-alpha"},
		cli.ProviderConfig{Name: "beta", BaseURL: "https://beta.example.com", DefaultModel: "m-beta"},
		cli.ProviderConfig{Name: "gamma", BaseURL: "http://127.0.0.1:8080/v1"},
	)

	clients, models, err := newProviderClients(cfg)
	require.NoError(t, err)

	require.Len(t, clients, len(cfg.Providers), "键数 = 条目数（不多不少）")
	for _, entry := range cfg.Providers {
		require.Contains(t, clients, entry.Name, "每个条目名都要有客户端")
		require.Contains(t, models, entry.Name, "每个条目名都要有缺省模型")
	}
	require.Equal(t, "m-alpha", models["alpha"])
	require.Equal(t, "m-beta", models["beta"])
	require.Equal(t, "", models["gamma"], "未配置缺省模型 = 空（不推断、不继承别家）")
	require.NotContains(t, clients, "", "不得出现空键（'默认供应商'的出入口）")
}

// TestNewProviderClients_InvalidBaseURLRejected —— 构造期防御校验（§2.4 裁决注 9）：
// 非法 `base_url` → 装配失败并点名 `providers[i]`；文案不回显原 URL 与其中的值。
func TestNewProviderClients_InvalidBaseURLRejected(t *testing.T) {
	const raw = "https://svc-acct:secret-7f@api.example.com/v1"
	cfg := providersConfig(
		cli.ProviderConfig{Name: "ok", BaseURL: "https://ok.example.com/v1", DefaultModel: "m"},
		cli.ProviderConfig{Name: "bad", BaseURL: raw, DefaultModel: "m"},
	)

	clients, models, err := newProviderClients(cfg)

	require.Error(t, err, "含 userinfo 的地址必须拒绝（不静默裁剪）")
	require.Contains(t, err.Error(), "providers[1]", "必须点名配置条目下标")
	require.Contains(t, err.Error(), "base_url", "必须点名键")
	require.Contains(t, err.Error(), "userinfo", "必须点出不支持的成分")
	require.NotContains(t, err.Error(), raw, "不得回显原 URL")
	require.NotContains(t, err.Error(), "secret-7f", "不得回显其中的值（userinfo 可能就是凭证）")
	require.Nil(t, clients, "任一构造失败即整体失败——不返回半张映射表")
	require.Nil(t, models)
}

// TestNewProviderService_ConfigToAuditRowChain —— 端到端（编程指南 §4.2 验收 Demo 的仓内可判面）：
// 配置 → 显式映射 → 调用 → `llm_calls` 落库。走真客户端 + 假服务 + 真库，中途不换零件。
func TestNewProviderService_ConfigToAuditRowChain(t *testing.T) {
	ctx := context.Background()
	srv := testutil.NewFakeOpenAIServer(t, testutil.FakeOpenAIResponse{
		Body: `{"choices":[{"message":{"role":"assistant","content":"pong"}}],` +
			`"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`,
	})

	path := filepath.Join(t.TempDir(), "basalt.db")
	db, err := storage.OpenWritable(ctx, path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	cfg := providersConfig(cli.ProviderConfig{
		Name: "demo", BaseURL: srv.URL(), DefaultModel: "demo-model",
	})
	svc, err := newProviderService(cfg, storage.NewLLMCallRepo(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	resp, err := svc.Chat(ctx, core.ChatRequest{
		Provider: "demo", RunID: "run-chain", SessionID: "sess-chain",
		Round: 1, Messages: []core.Message{{Role: "user", Content: "ping"}},
	})
	require.NoError(t, err)
	require.Equal(t, "pong", resp.Content)

	read, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })

	var (
		status, model, runID string
		prompt, completion   int
	)
	require.NoError(t, read.QueryRowContext(ctx,
		`SELECT status, model, run_id, prompt_tokens, completion_tokens FROM llm_calls`).
		Scan(&status, &model, &runID, &prompt, &completion))

	require.Equal(t, string(core.LLMCallSuccess), status)
	require.Equal(t, "demo-model", model, "缺省模型来自配置条目")
	require.Equal(t, "run-chain", runID, "关联标识随请求落到审计行")
	require.Equal(t, 4, prompt)
	require.Equal(t, 6, completion)

	// 配置里的地址是假服务的回环地址——出站协议由假服务那一侧判卷（§3.2 的 client 用例）。
	require.Len(t, srv.Requests(), 1, "一次调用 = 一次 POST")
	require.Equal(t, "demo-model", srv.Requests()[0]["model"])
}
