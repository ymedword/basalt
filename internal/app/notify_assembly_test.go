package app

// 本文件覆盖受控工具装配的生产实现（R7 / 宪法约束 11）：通知意图三态 × 渠道资格 ×
// 身份。断言只看"最终 tools 与变更"，不测内部辅助函数。
// This file covers the production tool assembly across the three notification modes,
// channel eligibility and agent identity.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// fakeAssemblyChannels 是渠道目录的只读替身（可注入读失败）。
// fakeAssemblyChannels is the read-only catalog double; it can fail on demand.
type fakeAssemblyChannels struct {
	channels []core.NotifyChannel
	err      error
}

func (f fakeAssemblyChannels) ListNotifyChannels(context.Context) ([]core.NotifyChannel, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.channels, nil
}

// assemblyProfile 构造一个待装配的 profile：工具列表与通知意图由用例给定。
// assemblyProfile builds the profile under assembly with the given tools and intent.
func assemblyProfile(tools []string, mode core.NotifyMode, channels []string) *core.Profile {
	return &core.Profile{Name: "writer", Tools: tools, NotifyMode: mode, NotifyChannels: channels}
}

// bootstrapAgent 在临时工作区里建立一个 Agent 状态文件并返回其稳定身份。
// bootstrapAgent creates a lifecycle state file in a temp workspace and returns its uid.
func bootstrapAgent(t *testing.T) (core.AgentUID, *workspace.LifecycleStore) {
	t.Helper()
	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	store := workspace.NewLifecycleStore(root, func() time.Time { return time.UnixMilli(1729500000123).UTC() })
	rec, err := store.Bootstrap(context.Background(), "writer", "op-assembly")
	require.NoError(t, err)
	return rec.AgentUID, store
}

// TestAssembleNotifyTool_NoneRemovesNotify：`none` 下必须移除 notify（并把移除带回），
// 没有 notify 时是零变更。
// notify_mode none removes the tool and reports the removal; an absent tool means no change.
func TestAssembleNotifyTool_NoneRemovesNotify(t *testing.T) {
	hook := AssembleNotifyTool(NotifyAssemblyDeps{})

	tools, change, err := hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory", core.NotifyToolName}, core.NotifyModeNone, nil))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory"}, tools)
	require.Equal(t, []string{core.NotifyToolName}, change.Removed)
	require.Empty(t, change.Added)

	tools, change, err = hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory"}, core.NotifyModeNone, nil))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory"}, tools)
	require.True(t, change.Empty())
}

// TestAssembleNotifyTool_AllEligibleRequiresOneEligibleChannel：all_eligible 至少要有
// 一个当前有资格的渠道才装配；空目录不装配。
// all_eligible assembles only when at least one eligible channel exists.
func TestAssembleNotifyTool_AllEligibleRequiresOneEligibleChannel(t *testing.T) {
	tools := []string{"save_memory"}

	hook := AssembleNotifyTool(NotifyAssemblyDeps{Channels: fakeAssemblyChannels{
		channels: []core.NotifyChannel{{Name: "ops", Scope: core.ScopeShared}},
	}})
	got, change, err := hook(context.Background(), "writer",
		assemblyProfile(tools, core.NotifyModeAllEligible, nil))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory", core.NotifyToolName}, got)
	require.Equal(t, []string{core.NotifyToolName}, change.Added)

	empty := AssembleNotifyTool(NotifyAssemblyDeps{Channels: fakeAssemblyChannels{}})
	got, change, err = empty(context.Background(), "writer",
		assemblyProfile(tools, core.NotifyModeAllEligible, nil))
	require.NoError(t, err)
	require.Equal(t, tools, got)
	require.True(t, change.Empty())

	// 缺目录（轻装配）时无法证明资格：按不装配处理。
	noCatalog := AssembleNotifyTool(NotifyAssemblyDeps{})
	got, change, err = noCatalog(context.Background(), "writer",
		assemblyProfile(tools, core.NotifyModeAllEligible, nil))
	require.NoError(t, err)
	require.Equal(t, tools, got)
	require.True(t, change.Empty())
}

// TestAssembleNotifyTool_ExclusiveChannelsFollowAgentIdentity：专属渠道只对属主 Agent
// 有资格——身份来自生命周期状态文件，与准入同一口径。
// An exclusive channel is eligible only for its owner agent, whose uid comes from the
// lifecycle state file exactly as admission reads it.
func TestAssembleNotifyTool_ExclusiveChannelsFollowAgentIdentity(t *testing.T) {
	agentUID, lifecycle := bootstrapAgent(t)
	otherUID, _ := bootstrapAgent(t)
	hook := AssembleNotifyTool(NotifyAssemblyDeps{
		Lifecycle: lifecycle,
		Channels: fakeAssemblyChannels{channels: []core.NotifyChannel{
			{Name: "mine", Scope: core.ScopeExclusive, OwnerAgentUID: agentUID},
			{Name: "theirs", Scope: core.ScopeExclusive, OwnerAgentUID: otherUID},
		}},
	})

	// 属主的专属渠道可用。
	got, change, err := hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory"}, core.NotifyModeAllowlist, []string{"mine"}))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory", core.NotifyToolName}, got)
	require.Equal(t, []string{core.NotifyToolName}, change.Added)

	// 别人的专属渠道不可用：不改装配，交给保存期绑定校验点名。
	got, change, err = hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory"}, core.NotifyModeAllowlist, []string{"theirs"}))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory"}, got)
	require.True(t, change.Empty())
}

// TestAssembleNotifyTool_AllowlistRequiresEveryDeclaredChannel：allowlist 要求声明的
// 每个渠道都存在且有资格；任一不满足就不装配。
// allowlist assembles only when every declared channel exists and is eligible.
func TestAssembleNotifyTool_AllowlistRequiresEveryDeclaredChannel(t *testing.T) {
	hook := AssembleNotifyTool(NotifyAssemblyDeps{Channels: fakeAssemblyChannels{
		channels: []core.NotifyChannel{{Name: "ops", Scope: core.ScopeShared}},
	}})

	got, change, err := hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory"}, core.NotifyModeAllowlist, []string{"ops"}))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory", core.NotifyToolName}, got)
	require.Equal(t, []string{core.NotifyToolName}, change.Added)

	got, change, err = hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory"}, core.NotifyModeAllowlist, []string{"ops", "missing"}))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory"}, got)
	require.True(t, change.Empty())
}

// TestAssembleNotifyTool_LegacyIntentFollowsCompatibilityRule：未声明 notify_mode 的
// 旧文件按兼容规则得到有效模式——空渠道 = all_eligible，有渠道 = allowlist；装配
// 与准入读的是同一份兼容规则。
// An undeclared intent follows the documented compatibility rule before assembly.
func TestAssembleNotifyTool_LegacyIntentFollowsCompatibilityRule(t *testing.T) {
	hook := AssembleNotifyTool(NotifyAssemblyDeps{Channels: fakeAssemblyChannels{
		channels: []core.NotifyChannel{{Name: "ops", Scope: core.ScopeShared}},
	}})

	got, _, err := hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory"}, "", nil))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory", core.NotifyToolName}, got)

	got, _, err = hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory"}, "", []string{"ops"}))
	require.NoError(t, err)
	require.Equal(t, []string{"save_memory", core.NotifyToolName}, got)
}

// TestAssembleNotifyTool_InvalidIntentLeavesValidationToTheGates：非法字面量 / 组合
// 矛盾不在装配层抢答——装配返回零变更，由装载期规则 10 与保存期校验拒绝。
// An invalid intent is not reinterpreted here; the load-time and binding gates refuse it.
func TestAssembleNotifyTool_InvalidIntentLeavesValidationToTheGates(t *testing.T) {
	hook := AssembleNotifyTool(NotifyAssemblyDeps{Channels: fakeAssemblyChannels{
		channels: []core.NotifyChannel{{Name: "ops", Scope: core.ScopeShared}},
	}})

	for _, profile := range []*core.Profile{
		assemblyProfile([]string{"save_memory"}, "sometimes", nil),
		assemblyProfile([]string{"save_memory"}, core.NotifyModeNone, []string{"ops"}),
		assemblyProfile([]string{"save_memory"}, core.NotifyModeAllowlist, nil),
	} {
		got, change, err := hook(context.Background(), "writer", profile)
		require.NoError(t, err)
		require.Equal(t, profile.Tools, got)
		require.True(t, change.Empty())
	}
}

// TestAssembleNotifyTool_CatalogFailurePropagates：目录读失败不得降级成"没有渠道"——
// 保存必须失败关闭，而不是静默不装配。
// A catalog read failure propagates; it never degrades into "no channels".
func TestAssembleNotifyTool_CatalogFailurePropagates(t *testing.T) {
	sentinel := errors.New("渠道目录不可读")
	hook := AssembleNotifyTool(NotifyAssemblyDeps{Channels: fakeAssemblyChannels{err: sentinel}})

	_, _, err := hook(context.Background(), "writer",
		assemblyProfile([]string{"save_memory"}, core.NotifyModeAllEligible, nil))
	require.ErrorIs(t, err, sentinel)
}
