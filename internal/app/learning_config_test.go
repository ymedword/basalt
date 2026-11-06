//go:build integration

package app

// 本文件是受控自我进化的配置面验收（颗粒度文档 §3.1.11 / C-26、T023）：
//
//	G1 `TestUS22_Config_DefaultsOff`——缺段 / 默认关闭；关闭态装配零告警
//	G2 `TestUS22_Config_EnabledRequiresAgents`——启用但名单为空（含省略）启动报错，
//	   空名单**不得**解释为「全部 Agent」
//	G3 `TestUS22_Config_DuplicateRejected`——重名同时点名两条下标；非法名拒绝启动
//	G4 `TestUS22_Config_UnknownAgentWarn`——名单内未注册的 Agent 只 WARN + 状态页列出
//	   （Q7），不阻止启动；关闭态不告警
//
// 为什么放在 app 而不是 cli：`reportUnknownLearningAgents` 与状态页的
// `unknown_agents` 都在装配层，配置装载只是输入。app 的集成测试统一带 integration
// 标签（US-16 起），本文件沿用同一入口。
// Why here: the WARN + status passthrough live in the assembly layer; the loader is
// only the input.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
)

// noEnvLookup 让配置装载在测试里与环境变量完全隔离。
// noEnvLookup isolates the loader from the process environment.
func noEnvLookup(string) (string, bool) { return "", false }

// writeLearningConfig 写一个最小 basalt.yaml 并返回路径。
// writeLearningConfig writes a minimal basalt.yaml and returns its path.
func writeLearningConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "basalt.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// loadLearningSection 装载配置并返回 learning 段（错误由调用方判）。
// loadLearningSection loads the config and returns the learning section.
func loadLearningSection(t *testing.T, body string) (cli.LearningConfig, error) {
	t.Helper()
	cfg, err := cli.LoadConfig(writeLearningConfig(t, body), noEnvLookup)
	if err != nil {
		return cli.LearningConfig{}, err
	}
	return cfg.Learning, nil
}

// G1：缺段即出厂默认关闭；关闭态装配不产生任何告警。
func TestUS22_Config_DefaultsOff(t *testing.T) {
	for _, body := range []string{
		"",
		"server:\n  max_concurrent_runs: 4\n",
		"learning:\n  auto_extract: false\n  agents: []\n",
	} {
		cfg, err := loadLearningSection(t, body)
		require.NoError(t, err, "config=%q", body)
		require.False(t, cfg.AutoExtract, "learning.auto_extract 出厂默认关闭")
		require.Empty(t, cfg.Agents)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	require.Empty(t, reportUnknownLearningAgents(logger, false, nil, nil))
	require.Empty(t, buf.String(), "关闭态不得产生学习装配告警")
}

// G2：启用但名单为空 / 省略名单都是启动错误；显式给一个合法 Agent 才通过。
func TestUS22_Config_EnabledRequiresAgents(t *testing.T) {
	for _, body := range []string{
		"learning:\n  auto_extract: true\n  agents: []\n",
		"learning:\n  auto_extract: true\n",
	} {
		_, err := loadLearningSection(t, body)
		require.ErrorContains(t, err, "learning.agents 为空", "config=%q", body)
		require.ErrorContains(t, err, "空名单不等于「全部 Agent」")
	}

	cfg, err := loadLearningSection(t, "learning:\n  auto_extract: true\n  agents:\n    - demo\n")
	require.NoError(t, err)
	require.True(t, cfg.AutoExtract)
	require.Equal(t, []string{"demo"}, cfg.Agents)
}

// G3：重名点名两条下标；非法名（大写 / 空格 / 路径分隔符）拒绝启动。
// 关闭开关也判已写名单——拼错的名单等打开开关才报，只会把问题推迟到切换那一刻。
func TestUS22_Config_DuplicateRejected(t *testing.T) {
	_, err := loadLearningSection(t,
		"learning:\n  auto_extract: true\n  agents:\n    - demo\n    - demo\n")
	require.ErrorContains(t, err, "learning.agents[0] 与 learning.agents[1] 重复")
	require.ErrorContains(t, err, `"demo"`)

	for _, name := range []string{"Demo", "de mo", "../demo", "demo/x", "_demo"} {
		_, err := loadLearningSection(t,
			"learning:\n  auto_extract: false\n  agents:\n    - "+name+"\n")
		require.ErrorContains(t, err, "名字非法", "name=%q", name)
	}
}

// G4：名单内未注册的 Agent 只 WARN 并在状态页列出（Q7：不阻止启动、不抽取）。
func TestUS22_Config_UnknownAgentWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	unknown := reportUnknownLearningAgents(logger,
		true, []string{"demo", "ghost"}, []string{"demo"})
	require.Equal(t, []string{"ghost"}, unknown)
	require.Contains(t, buf.String(), `"level":"WARN"`)
	require.Contains(t, buf.String(), `"agent":"ghost"`)

	// 状态页原样列出 unknown_agents；装配层把它传进来，读模型不重新计算。
	admin := NewLearningAdmin(LearningAdminDeps{
		Store:         &fakeLearningAdminStore{},
		Enabled:       true,
		Agents:        []string{"demo", "ghost"},
		UnknownAgents: unknown,
		Logger:        logger,
	})
	status, err := admin.Status(context.Background())
	require.NoError(t, err)
	require.True(t, status.Enabled)
	require.Equal(t, []string{"demo", "ghost"}, status.Agents)
	require.Equal(t, []string{"ghost"}, status.UnknownAgents)

	// 关闭态：集合差照算（供状态页展示），但不告警（未启用零日志正文、零副作用）。
	buf.Reset()
	require.Equal(t, []string{"ghost"}, reportUnknownLearningAgents(logger,
		false, []string{"demo", "ghost"}, []string{"demo"}))
	require.Empty(t, buf.String(), "关闭态不得产生学习装配告警")
}
