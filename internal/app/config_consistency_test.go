package app

// 本文件钉住人工 review ② 复现的两条版本一致性缺陷（2026-10-21 退回项）：
//
//	1. 合法配置切换：准入解析（注册表条目）与运行快照（构建时重读磁盘）必须来自
//	   同一份已校验配置——清单里的 Provider 与实际模型调用的 Provider 不得分叉；
//	2. 坏编辑被拒：热身 watcher 拒绝坏文件后，注册表保留旧条目；新 Run 必须继续用
//	   已发布配置，**不得**重读磁盘后以"读取 Agent 正文"失败收场。
//
// 两条回归都以**实际调用**为判据（进程内替身记录入站请求），不从"管理器返回了什么"
// 推断运行用了什么。
// Both regressions judge the actual model call, not only what admission returned.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
)

// useTwoProviders 把夹具换成 alpha / beta 两个 Provider：切换用例需要一个"合法但
// 尚未发布"的第二份配置，才能区分"用了注册表条目"与"用了磁盘新内容"。
// useTwoProviders switches the fixture to two configured providers so a valid disk
// edit can name a different provider from the admitted one.
func (f *wireFixture) useTwoProviders(t *testing.T) {
	t.Helper()

	t.Setenv("BASALT_WIRE_KEY", "wire-sentinel-not-a-secret")
	f.cfg.Providers = []cli.ProviderConfig{
		{Name: "alpha", BaseURL: "http://127.0.0.1:1", APIKey: "wire-sentinel-not-a-secret", DefaultModel: "alpha-model"},
		{Name: "beta", BaseURL: "http://127.0.0.1:1", APIKey: "wire-sentinel-not-a-secret", DefaultModel: "beta-model"},
	}
	content := "providers:\n" +
		"  - name: alpha\n" +
		"    base_url: http://127.0.0.1:1\n" +
		"    api_key: ${BASALT_WIRE_KEY}\n" +
		"    default_model: alpha-model\n" +
		"  - name: beta\n" +
		"    base_url: http://127.0.0.1:1\n" +
		"    api_key: ${BASALT_WIRE_KEY}\n" +
		"    default_model: beta-model\n"
	require.NoError(t, os.WriteFile(filepath.Join(f.workDir, configFileName), []byte(content), 0o644))
}

// writeAgentWithProvider 落一份声明指定 Provider 的 AGENT.md（覆盖同一 Agent 的旧文件）。
// writeAgentWithProvider writes an AGENT.md that names the given provider.
func writeAgentWithProvider(t *testing.T, f *wireFixture, name, provider string) {
	t.Helper()

	dir := filepath.Join(f.root, agentsDirName, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := "---\n" +
		"name: " + name + "\n" +
		"description: 一致性探针\n" +
		"provider:\n  name: " + provider + "\n  model: " + provider + "-model\n" +
		"tools: []\n" +
		"---\n\n你是「" + name + "」。\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(content), 0o644))

	// 只在首次落盘时记名：装配后的覆盖写不是"新增 Agent"，不应再走一次启用。
	// Record the name on first write only; the post-assembly overwrite is an edit.
	known := false
	for _, existing := range f.agents {
		known = known || existing == name
	}
	if !known {
		f.agents = append(f.agents, name)
	}
}

// TestRun_SnapshotUsesValidatedRegistryConfig（review ② 缺陷 1a）：装配后磁盘上的
// AGENT.md 合法地切换到另一个 Provider，注册表仍持有装配时校验通过的条目。新 Run 的
// 清单、快照与实际模型调用都必须用注册表里的 Provider——**不得**混用两份配置。
//
// 逆改点：`freeze` 回到"每次重读盘"→ 实际调用走 beta 而清单记 alpha，本用例红。
// The admitted entry is the single source: manifest, snapshot and the actual call must
// all name alpha even though the disk has since been validly switched to beta.
func TestRun_SnapshotUsesValidatedRegistryConfig(t *testing.T) {
	f := newWireFixture(t)
	f.useTwoProviders(t)
	writeAgentWithProvider(t, f, "writer", "alpha")

	override := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "alpha-answer"}}}
	f.override = override
	rt := f.assemble(t)

	// 装配之后（无 watcher 运行）把磁盘改成合法的 beta 配置：注册表条目不变。
	writeAgentWithProvider(t, f, "writer", "beta")

	res, err := rt.service.Run(context.Background(), core.RunRequest{
		Agent: "writer", Message: "你好", Channel: "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "合法切换后新 Run 必须用已发布配置跑完：%s", res.Reason)
	require.Equal(t, "alpha-answer", res.FinalText)

	requests := override.requests()
	require.Len(t, requests, 1, "一次运行只允许一次模型调用")
	require.Equal(t, "alpha", requests[0].Provider,
		"实际模型调用必须走注册表条目里的 Provider（磁盘已改成 beta，但该编辑未经发布）")

	db := openRead(t, f.dbPath())
	var raw string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT snapshot_json FROM run_resource_manifests WHERE run_id = ?`, res.RunID).Scan(&raw))
	var manifest core.RunResourceManifest
	require.NoError(t, json.Unmarshal([]byte(raw), &manifest))
	require.Equal(t, "alpha", manifest.Provider.Name,
		"资源清单与实际调用必须来自同一份配置（review ② 复现对：manifest_provider / snapshot_provider）")

	var recordedProvider string
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT provider FROM llm_calls WHERE run_id = ?`, res.RunID).Scan(&recordedProvider))
	require.Equal(t, "alpha", recordedProvider, "审计行记录的 Provider 必须与实际调用一致")
}

// TestRun_SnapshotKeepsPublishedConfigAfterRejectedDiskEdit（review ② 缺陷 1b）：坏编辑
// 被 watcher 拒绝后，注册表保留旧的已校验条目；新 Run 必须继续用已发布配置，**不得**
// 重读磁盘后以解析失败拒绝启动（I-81 的症状）。
//
// 本用例不启动 watcher，而是直接制造"磁盘是坏的、注册表是好的"这一 watcher 拒绝后的
// 稳定状态；注册表保留旧条目的行为由 watcher 自身的用例判卷。
// After a rejected edit the registry still serves the last validated config; a new run
// must keep using it instead of re-reading the broken file.
func TestRun_SnapshotKeepsPublishedConfigAfterRejectedDiskEdit(t *testing.T) {
	f := newWireFixture(t)
	// 夹具助手在零工具时拼接缺换行（`tools: []---`），既有调用点都至少声明一个工具；
	// 本用例沿用同一形态，不借机改公共夹具的写盘字节。
	f.writeAgent(t, "writer", "read_file")

	override := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "published-answer"}}}
	f.override = override
	rt := f.assemble(t)

	// watcher 拒绝坏编辑后的状态：磁盘上是坏 frontmatter，注册表仍是装配时的好条目。
	broken := "---\nname: [broken\nprovider: {name: probe, model: probe-model}\n---\n"
	require.NoError(t, os.WriteFile(
		filepath.Join(f.root, agentsDirName, "writer", "AGENT.md"), []byte(broken), 0o644))

	res, err := rt.service.Run(context.Background(), core.RunRequest{
		Agent: "writer", Message: "你好", Channel: "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status,
		"坏编辑被拒后新 Run 必须继续使用已发布配置，实际状态=%s 原因=%s", res.Status, res.Reason)
	require.Equal(t, "published-answer", res.FinalText)
	require.Equal(t, "probe", override.requests()[0].Provider)
}
