package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件用**包内**测试（`package cli`）：`provider list` 的展示层判据要求"直接注入
// 条目、绕过装载期"——配置面已拒绝的地址成分仍要按收窄规则渲染（§2.4 裁决注 9 的
// 纵深防御），而装载期拒绝意味着这条路走不进配置文件，只能注入渲染入口。
// An in-package test on purpose: the presentation layer must be judged with
// injected entries, because the loader rejects exactly the shapes it renders.

// providerRun 是一次 `provider list` 运行的返回错误与三路输出（stdout / stderr /
// 结构化日志）——轻路径没有第四路（不开库）。
// providerRun captures the error and the three output tracks of one run.
type providerRun struct {
	err    error
	stdout string
	stderr string
	logs   string
}

// executeProviderList 在注入的工作目录上跑一次 `provider list`，走真根命令
// （NewRootCommand + cobra 派发）——命令注册、退出码出口与 stderr 文案都属被测面。
// The real root command is exercised so registration and the exit-code path are
// under test too.
//
// 两个端口**故意留 nil**：轻路径不开库、不取实例锁、不做恢复（技术方案 §6.1.2）——
// 一旦有代码碰到它们就当场 panic，而不是悄悄通过。
// Both ports are deliberately nil: the light path must never touch them, and a
// touch panics loudly instead of passing quietly.
func executeProviderList(t *testing.T, workDir string) providerRun {
	t.Helper()

	var logs bytes.Buffer
	deps := Deps{
		WorkDir: workDir,
		// 合成值：轻路径用不到版本，给非零值以免零值把"其实用到了"掩盖过去。
		SupportedSchemaVersion: 7,
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
		Store:                  nil,
		Workspace:              nil,
	}

	root := NewRootCommand(deps)
	root.SetArgs([]string{"provider", "list"})

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(context.Background())
	return providerRun{err: err, stdout: stdout.String(), stderr: stderr.String(), logs: logs.String()}
}

// writeProviderConfig 把配置写进给定目录下的 basalt.yaml（provider list 读的就是
// 这个位置：与工作区同级、不在 .basalt/ 内）。
// writeProviderConfig writes the instance configuration where the command reads it.
func writeProviderConfig(t *testing.T, dir, content string) string {
	t.Helper()

	path := filepath.Join(dir, "basalt.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestProviderList_ListsConfiguredProviders 验收点（文档用例名同名）：输出含每家
// name / 脱敏后的 base_url / default_model + 汇总；exit 0。
func TestProviderList_ListsConfiguredProviders(t *testing.T) {
	t.Setenv("BASALT_ALPHA_KEY", "resolved-alpha-9f2a")
	t.Setenv("BASALT_BETA_KEY", "resolved-beta-7c31")

	dir := t.TempDir()
	writeProviderConfig(t, dir, `log:
  level: info
providers:
  - name: alpha
    base_url: https://alpha.example.com/v1
    api_key: ${BASALT_ALPHA_KEY}
    default_model: m-alpha
  - name: beta
    base_url: https://beta.example.com
    api_key: ${BASALT_BETA_KEY}
`)

	run := executeProviderList(t, dir)
	require.NoError(t, run.err, "正常形态必须 exit 0；stderr=%s", run.stderr)

	require.Contains(t, run.stdout, "alpha")
	require.Contains(t, run.stdout, "https://alpha.example.com/v1", "地址按收窄规则渲染（合法形态原样保留）")
	require.Contains(t, run.stdout, "m-alpha")
	require.Contains(t, run.stdout, "beta")
	require.Contains(t, run.stdout, "https://beta.example.com")
	require.Contains(t, run.stdout, "汇总：2 家 Provider")
}

// TestProviderList_RedactsBaseURLUserinfoAndQuery —— §2.4 裁决注 9 的**展示层防御**：
// 直接注入含 userinfo / query / fragment 的条目（**绕过装载期**——配置面已拒绝，展示层
// 不得依赖这一点）→ `https://u:p@h/v1?key=x` 渲染为 `https://h/v1`，三部分都不出现在输出里。
func TestProviderList_RedactsBaseURLUserinfoAndQuery(t *testing.T) {
	var out bytes.Buffer
	renderProviderList(&out, []ProviderConfig{{
		Name:         "injected",
		BaseURL:      "https://u:p@h/v1?key=x#frag",
		APIKey:       "resolved-secret-9f2a",
		DefaultModel: "m-injected",
	}})
	rendered := out.String()

	require.Contains(t, rendered, "https://h/v1", "只留 scheme://host/path 三部分")
	require.NotContains(t, rendered, "https://u:p@h/v1", "原 URL 不得回显")
	require.NotContains(t, rendered, "u:p", "userinfo（可能就是凭证）不得回显")
	require.NotContains(t, rendered, "key=x", "query 不得回显")
	require.NotContains(t, rendered, "frag", "fragment 不得回显")
	require.NotContains(t, rendered, "resolved-secret-9f2a", "凭证值不得回显")

	// 解析不出 scheme + host 的取值：隐去，而不是把未收窄的原文当输出。
	for _, raw := range []string{"http:api.example.com/v1", "api.example.com/v1", ""} {
		got := redactedBaseURL(raw)
		require.NotContainsf(t, got, "api.example.com", "不可识别的取值必须隐去（输入 %q）", raw)
	}
}

// TestProviderList_NoCredentialLeakInOutput 凭证三路不泄露：stdout / stderr /
// 结构化日志都不含解析后的凭证值（与 US-1 同口径），且凭证状态文案只报状态、不报值。
func TestProviderList_NoCredentialLeakInOutput(t *testing.T) {
	const secret = "resolved-secret-9f2a"
	t.Setenv("BASALT_DEMO_KEY", secret)

	dir := t.TempDir()
	writeProviderConfig(t, dir, `providers:
  - name: demo
    base_url: https://api.example.com/v1
    api_key: ${BASALT_DEMO_KEY}
    default_model: demo-model
`)

	run := executeProviderList(t, dir)
	require.NoError(t, run.err, "stderr=%s", run.stderr)

	require.NotContains(t, run.stdout, secret, "标准输出不得含凭证值")
	require.NotContains(t, run.stderr, secret, "标准错误不得含凭证值")
	require.NotContains(t, run.logs, secret, "日志轨不得含凭证值")
	require.Contains(t, run.stdout, "凭证 已配置", "只报状态文案")
}

// TestProviderList_MissingConfigFailsWithPath —— §2.4 裁决注 2：缺配置 → **失败**并
// 点名路径（不沿用 status 的容忍语义）。
func TestProviderList_MissingConfigFailsWithPath(t *testing.T) {
	dir := t.TempDir() // 刻意不写配置

	run := executeProviderList(t, dir)
	require.Error(t, run.err, "缺配置对 provider list 计失败（命令目的未达成）")

	path := filepath.Join(dir, "basalt.yaml")
	require.Contains(t, run.err.Error(), path, "必须点名配置文件路径")
	require.Contains(t, run.stderr, path, "给人看的那一路同样点名路径")
	require.Contains(t, run.logs, path, "日志轨同样点名路径")
}

// TestProviderList_EmptyProvidersSectionFails：`providers` 段为空 → 失败并点名段名。
// 两种空法都算：写成空数组、整段缺失。
func TestProviderList_EmptyProvidersSectionFails(t *testing.T) {
	cases := map[string]string{
		"空数组": `providers: []
`,
		"段落缺失": `log:
  level: info
`,
	}

	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeProviderConfig(t, dir, content)

			run := executeProviderList(t, dir)
			require.Error(t, run.err, "无 Provider 即无输出对象——命令目的未达成")
			require.Contains(t, run.err.Error(), "providers", "必须点名段名")
			require.Contains(t, run.logs, "providers", "日志轨同样点名段名")
		})
	}
}

// TestProviderList_InvalidConfigFailsAndNamesKeyPath：配置无效 → 原错误上抛（点名
// 键路径与变量名、**不含凭证值**）——同一次装载里已解析出的凭证值不得借错误文案回流到
// 任一路输出。
func TestProviderList_InvalidConfigFailsAndNamesKeyPath(t *testing.T) {
	const secret = "resolved-secret-9f2a"
	t.Setenv("BASALT_DEMO_KEY", secret)

	dir := t.TempDir()
	writeProviderConfig(t, dir, `providers:
  - name: demo
    base_url: https://api.example.com/v1
    api_key: ${BASALT_DEMO_KEY}
  - name: broken
    base_url: https://broken.example.com/v1
    api_key: ${BASALT_ABSENT_VAR_9F2A}
`)

	run := executeProviderList(t, dir)
	require.Error(t, run.err, "装载期拒绝即命令失败")

	require.Contains(t, run.err.Error(), "providers[1].api_key", "必须点名键路径")
	require.Contains(t, run.err.Error(), "BASALT_ABSENT_VAR_9F2A", "必须点名变量名")

	for track, content := range map[string]string{
		"错误": run.err.Error(), "标准输出": run.stdout,
		"标准错误": run.stderr, "日志": run.logs,
	} {
		require.NotContainsf(t, content, secret, "%s轨不得含已解析出的凭证值", track)
	}
}
