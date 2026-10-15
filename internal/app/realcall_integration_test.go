//go:build integration

package app

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	// 判卷侧走裸连接读库（与 providers_test.go 同一先例）。
	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// 本文件是 US-2 验收 Demo 的可判面（颗粒度文档 §4.1 集成层）：
//
//   - 正例走**真实供应商**——双门控（显式执行开关 + 凭证 env），缺一即 t.Skip；
//     **跳过 ≠ 通过**：缺凭证的那次运行只在日志里留一行 SKIP，不构成"真实调用
//     验收通过"（§6 收口时按 --- PASS / --- SKIP 区分）。
//   - 反例用**不可达端点**（回环上没人监听）——不出网、不需要凭证，故不设门控：
//     它是 fail-closed 的正面判据（失败也必须留痕）。
//
// The integration layer: the positive case is double-gated and skip-never-counts-as-pass;
// the negative case reaches no network and therefore needs no gate.

// realCallTimeout 是正例的显式截止时间（60 秒测试常量，**不是**配置键——「已决事项」
// 原待决 #3：客户端不设自有超时，截止时间一律由调用方 ctx 承载）。
// realCallTimeout is an explicit test constant, never a configuration key.
const realCallTimeout = 60 * time.Second

// TestProviderService_RealCallWritesAudit 验收 Demo 正例：显式执行开关 + 凭证 env
// 双门控 → 真实调用成功一次 → 库中可见 success 行，token 三列与响应一致。
func TestProviderService_RealCallWritesAudit(t *testing.T) {
	if os.Getenv("BASALT_REAL_CALL") != "1" {
		t.Skip("未设置 BASALT_REAL_CALL=1——真实调用由人显式开关；跳过不构成验收通过")
	}

	demoDir := os.Getenv("BASALT_DEMO_DIR")
	if demoDir == "" {
		t.Skip("未设置 BASALT_DEMO_DIR=<演示工作区>——演示用独立工作区与独立库，不复用开发库；跳过不构成验收通过")
	}

	// 凭证门控：配置里的 ${VAR} 由环境变量注入（不落盘 / 不落日志 / 不进对话）。
	// 用一层记录型 lookup 区分"凭证没注入"（跳过）与"配置本身就是坏的"（失败）——
	// 后者若也跳过，一份写错的演示配置会伪装成"未执行"。
	// A recording lookup separates a missing credential (skip) from a broken
	// configuration (failure), so a typo cannot masquerade as "not run".
	var missing []string
	lookup := func(name string) (string, bool) {
		value, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return value, ok
	}

	cfgPath := filepath.Join(demoDir, configFileName)
	cfg, err := cli.LoadConfig(cfgPath, lookup)
	if err != nil {
		if len(missing) > 0 {
			t.Skipf("凭证未注入：环境变量 %s 未设置——真实调用验收未执行（跳过不构成通过）",
				strings.Join(missing, "、"))
		}
		t.Fatalf("演示配置无效（%s）：%v", cfgPath, err)
	}
	require.NotEmpty(t, cfg.Providers, "演示配置至少要有一家可调用的 Provider")
	entry := cfg.Providers[0]

	ctx, cancel := context.WithTimeout(context.Background(), realCallTimeout)
	defer cancel()

	path := filepath.Join(t.TempDir(), "basalt.db")
	db, err := storage.OpenWritable(ctx, path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	svc, err := newProviderService(cfg, storage.NewLLMCallRepo(db),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	// run_id 用合成值：runs 表的第一行要到 US-4 才写入，而 llm_calls.run_id 无外键
	// （§2.4 裁决注 4）——明示为演示数据。
	resp, err := svc.Chat(ctx, core.ChatRequest{
		Provider:  entry.Name,
		RunID:     "demo-" + time.Now().UTC().Format("20060102T150405Z"),
		SessionID: "demo-session",
		Round:     1,
		Messages:  []core.Message{{Role: "user", Content: "ping"}},
	})
	require.NoError(t, err, "真实调用必须成功（失败时先看库中审计行的 error_kind 与 error_message）")

	read, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })

	var (
		status, provider, model, errorKind string
		prompt, completion, total          int
	)
	require.NoError(t, read.QueryRowContext(ctx,
		`SELECT status, provider, model, COALESCE(error_kind, ''), `+
			`prompt_tokens, completion_tokens, total_tokens FROM llm_calls`).
		Scan(&status, &provider, &model, &errorKind, &prompt, &completion, &total))

	require.Equal(t, string(core.LLMCallSuccess), status)
	require.Equal(t, entry.Name, provider)
	require.NotEmpty(t, model, "生效模型必须落审计行")
	require.Empty(t, errorKind, "成功行的 error_kind 为 NULL")

	// token 判据 = **审计行与响应三列一致**（既定口径原文）。usage 可空不是失败，故不得把
	// "三列 > 0"写成无条件判据；也**不**由 `total_tokens > 0` 去反推"供应商返回了 usage"——
	// 总数盖不住"usage 在但两列都是 0"与"usage 缺失"的区别，用它当分支条件等于拿一个不可靠
	// 读数把判据收窄（复核记录：该推断强于既定判据，本轮撤除）。
	// The token criterion is equality with the response; usage presence is not inferable
	// from total_tokens, so no such branch is asserted.
	require.Equal(t, resp.Usage.PromptTokens, prompt, "审计行与响应的 token 必须一致")
	require.Equal(t, resp.Usage.CompletionTokens, completion)
	require.Equal(t, resp.Usage.TotalTokens, total)
	if prompt == 0 && completion == 0 && total == 0 {
		t.Log("本次响应三列 token 为 0：按 0 记录，仍然判成功（供应商未返回 usage）")
	}
}

// TestProviderService_UnreachableEndpointRecordsFailedAudit 验收 Demo 反例：
// 不可达端点（回环上没人监听）→ 失败也落审计（`failed` + `external_error`）。
// 不出网、不需要凭证，故**不设门控**——它是 fail-closed 的正面判据。
func TestProviderService_UnreachableEndpointRecordsFailedAudit(t *testing.T) {
	const unreachable = "http://127.0.0.1:1"

	ctx, cancel := context.WithTimeout(context.Background(), realCallTimeout)
	defer cancel()

	path := filepath.Join(t.TempDir(), "basalt.db")
	db, err := storage.OpenWritable(ctx, path, storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	// 直接构造配置（不经装载层）：本用例判的是调用与审计路径，凭证值在这里无意义
	// （端点不可达，请求发不出去）。
	cfg := &cli.Config{Providers: []cli.ProviderConfig{{
		Name:         "unreachable",
		BaseURL:      unreachable,
		APIKey:       "placeholder-unused-9f2a",
		DefaultModel: "unreachable-model",
	}}}

	svc, err := newProviderService(cfg, storage.NewLLMCallRepo(db),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)

	_, err = svc.Chat(ctx, core.ChatRequest{
		Provider: "unreachable",
		RunID:    "demo-unreachable",
		Round:    1,
		Messages: []core.Message{{Role: "user", Content: "ping"}},
	})
	require.Error(t, err, "不可达端点必须报错（一次调用、不重试）")
	require.NotContains(t, err.Error(), unreachable, "错误文案不得携带请求 URL")

	read, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, read.Close()) })

	var errorKind, errorMessage string
	require.NoError(t, read.QueryRowContext(ctx,
		`SELECT COALESCE(error_kind, ''), COALESCE(error_message, '') FROM llm_calls`).
		Scan(&errorKind, &errorMessage))

	require.Equal(t, string(core.ErrorKindExternalError), errorKind,
		"网络不可达归 external_error（契约 §7.3）")
	require.NotEmpty(t, errorMessage, "失败原文必须落审计行（截断后）")
	require.NotContains(t, errorMessage, "placeholder-unused-9f2a", "审计行不得含凭证")
}
