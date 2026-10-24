package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/testutil"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// TestNewNotifyAdapters_ExactTypeSet —— P1（约束 7 / R11）：类型 → 适配器映射只有
// 装配层一处定义，且**恰好** `webhook` / `feishu` 两键；多一键（擅自承诺原生协议）
// 少一键都当场落红——工具内核按类型查表，装配层漏键即该类型渠道全部不可用。
// P1 (R11): the type→adapter map is defined only here and holds exactly two keys.
func TestNewNotifyAdapters_ExactTypeSet(t *testing.T) {
	adapters := newNotifyAdapters()
	require.Len(t, adapters, 2, "类型 → 适配器映射恰好两键")

	keys := make([]string, 0, len(adapters))
	for name := range adapters {
		keys = append(keys, name)
	}
	slices.Sort(keys)
	require.Equal(t, []string{"feishu", "webhook"}, keys)
	require.IsType(t, &tool.WebhookNotifyAdapter{}, adapters["webhook"], "两键的值类型不得互换")
	require.IsType(t, &tool.FeishuNotifyAdapter{}, adapters["feishu"], "两键的值类型不得互换")
}

// TestAgentsPort_LightPathAcceptsNotifyDeclaration —— P2（约束 23 / R11）：轻路径
// （`profile list` / `profile show`）不建注册池，装载判定拿装配层的**声明名集**；
// 声明了 `tools: [notify]` 的 Agent 必须与重路径得到同一结论——注册。声明名集漏掉
// `notify` 时同一份 AGENT.md 在轻路径被判未注册注册，两条路开始分叉。
// P2 (R11): the light path judges registration against the assembly's declared name set;
// an agent declaring notify must register exactly as it would on the heavy path.
func TestAgentsPort_LightPathAcceptsNotifyDeclaration(t *testing.T) {
	workDir := testutil.TempWorkspace(t)
	root := filepath.Join(workDir, ".basalt")
	_, err := workspace.Init(root)
	require.NoError(t, err)

	writeAgentDir(t, root, "notifier", `---
name: notifier
provider:
  name: deepseek
  model: deepseek-chat
tools: [notify]
---

巡检结束后把摘要推到 ops 渠道。
`)

	port := agentsPort{WorkDir: workDir}
	entries, err := port.List(root, []string{"deepseek"})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered,
		"轻路径与重路径共用同一份声明名集：notify 必须可注册：%s", entries[0].RejectReason)
}

// notifySend 造一条渠道行并直接跑一次发送（不经管理写面——那里会拒绝不可发送的
// 渠道；发送面必须自己守住同样的边界）。
// notifySend inserts a row directly and runs one send; the send face must enforce the
// same boundaries on its own.
func notifySend(t *testing.T, channelType, url, input string) (core.Result, error) {
	t.Helper()
	env := newNotifyEnv(t)
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	require.NoError(t, env.store.Create(context.Background(), core.NotifyChannel{
		ResourceUID: uid, Name: "ops-alert", Type: channelType, URL: url,
		OperationID: "op_notify_send_fixture",
	}))
	toolInstance := newNotifyTool(storage.NewNotifyChannelRepo(env.db), env.deps.Policy, nil, nil)
	return toolInstance.Execute(context.Background(), core.ToolEnv{AgentName: "tester"},
		json.RawMessage(input))
}

// 中文验收点：发送面读**数据库**渠道表——管理写面刚落下的行下一发即可见；空地址的
// 渠道在发送时报错（暂不可发送不是静默跳过）。
// Acceptance point: the send face reads the database registry, so an empty URL fails the
// send instead of being silently skipped.
func TestNewNotifyTool_ReadsRegisteredChannel(t *testing.T) {
	_, err := notifySend(t, "webhook", "", `{"channel":"ops-alert","content":"摘要"}`)
	require.ErrorContains(t, err, "占位", "行是从数据库读出来的：空地址必须走到占位判定")
	require.ErrorContains(t, err, "ops-alert")
	require.NotContains(t, err.Error(), "不存在", "读到的渠道不得被误判为未登记")
}

// 中文验收点：发送时环境变量未设置或为空 → 本次发送失败并点名渠道与变量；错误里
// **不得**出现任何明文地址回退。
// Acceptance point: a missing or empty variable fails the send, naming the channel and
// the variable, with no plaintext fallback anywhere in the error.
func TestNewNotifyTool_MissingEnvFailsSend(t *testing.T) {
	t.Setenv("BASALT_TEST_EMPTY_HOOK", "")
	_, err := notifySend(t, "webhook", "${BASALT_TEST_EMPTY_HOOK}", `{"channel":"ops-alert","content":"x"}`)
	require.ErrorContains(t, err, "BASALT_TEST_EMPTY_HOOK")
	require.ErrorContains(t, err, "未设置或为空")
	require.NotContains(t, err.Error(), "https://")

	_, err = notifySend(t, "webhook", "${BASALT_TEST_UNSET_HOOK_9F3A}", `{"channel":"ops-alert","content":"x"}`)
	require.ErrorContains(t, err, "BASALT_TEST_UNSET_HOOK_9F3A")
	require.NotContains(t, err.Error(), "https://")
}

// 中文验收点：发送面的策略拒绝沿用沙箱四段式，但目标被改写成 scheme + host——解析后
// 的完整地址（尤其查询串凭证）**不得**进入错误文案。
// Acceptance point: a policy refusal keeps the sandbox four-segment text but rewrites
// the target to scheme+host; the query-string secret never reaches the error text.
func TestNewNotifyTool_PolicyRefusalRedacted(t *testing.T) {
	t.Setenv("BASALT_TEST_EVIL_HOOK", "https://evil.example.com/hook?token=super-secret")
	_, err := notifySend(t, "webhook", "${BASALT_TEST_EVIL_HOOK}", `{"channel":"ops-alert","content":"x"}`)

	var violation *core.SandboxViolationError
	require.ErrorAs(t, err, &violation)
	require.Equal(t, "https://evil.example.com", violation.Target)
	require.NotContains(t, err.Error(), "super-secret")
	require.NotContains(t, err.Error(), "/hook")
}

// 中文验收点：未登记渠道名 → not_found（错误链锚 fs.ErrNotExist）；库里的类型不在
// 已装配映射 → 装配错误点名列出的已装配集合（类型表唯一真值）。
// Acceptance point: an unknown channel anchors fs.ErrNotExist, and a stored type outside
// the assembled map is a wiring error naming the assembled set.
func TestNewNotifyTool_UnknownChannelAndType(t *testing.T) {
	_, err := notifySend(t, "webhook", "${BASALT_TEST_OPS_HOOK}", `{"channel":"nope","content":"x"}`)
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, err = notifySend(t, "slack", "${BASALT_TEST_OPS_HOOK}", `{"channel":"ops-alert","content":"x"}`)
	require.ErrorContains(t, err, "slack")
	require.ErrorContains(t, err, "feishu、webhook")
}

// 中文验收点：入参在查表与外呼之前判卷——非 JSON 对象与超过 20 KiB 的 content 都是
// 输入错误，不触达渠道表。
// Acceptance point: malformed input and over-limit content are input errors judged
// before any registry read or outbound call.
func TestNewNotifyTool_ContentCap(t *testing.T) {
	_, err := notifySend(t, "webhook", "${BASALT_TEST_OPS_HOOK}", `not-json`)
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)

	oversized, marshalErr := json.Marshal(map[string]string{
		"channel": "ops-alert", "content": strings.Repeat("a", 20*1024+1),
	})
	require.NoError(t, marshalErr)
	_, err = notifySend(t, "webhook", "${BASALT_TEST_OPS_HOOK}", string(oversized))
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Reason, "上限")
}
