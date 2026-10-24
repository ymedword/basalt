package app

// 本文件是受控工具装配的生产实现（R7 / 宪法约束 11）。
//
// 通知意图用 `notify_mode` 三态表达，生效 `tools` 必须与之一致：
//   - `none`：必须不装配 `notify`——写了也移除，并作为可见变更返回；
//   - `all_eligible`：至少有一个当前有资格的渠道（共享 ∪ 本 Agent 专属）才装配；
//   - `allowlist`：声明的每个渠道都存在且有资格才装配。
//
// 资格判定与准入共用 `core.Eligible` 与同一份渠道目录；绑定完整性（声明 ↔ 绑定 UID
// ↔ 当前指针）由紧随其后的保存期绑定校验负责，本文件不重复解释绑定语义。
// 除 `notify` 外的任何工具名都不在这里出现——系统不得自动增删其他工具。
//
// This file is the production implementation of the controlled tool assembly: the
// effective tool set must agree with the three-state notification intent, and the
// only name the system may add or remove is notify.

import (
	"context"
	"fmt"
	"slices"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// NotifyAssemblyDeps 是受控工具装配的只读依赖：渠道目录与生命周期状态文件
// （从中取 Agent 的稳定身份判专属渠道资格）。两者都可为 nil：缺目录时装配层
// 无法证明资格，按"不装配"处理（保存期绑定校验仍会独立判定）。
// NotifyAssemblyDeps carries the read-only inputs of the assembly; a missing catalog
// leaves the assembly unable to prove eligibility and therefore not assembling.
type NotifyAssemblyDeps struct {
	Channels  NotifyChannelCatalog
	Lifecycle *workspace.LifecycleStore
}

// AssembleNotifyTool 构造注入 `workspace.AgentEditorDeps.AssembleTools` 的钩子：
// 输入补丁后的 profile，输出最终 tools 与需要随保存结果回显的增删。
// AssembleNotifyTool builds the hook injected into the editor; it returns the final
// tool list plus the delta that must be reported with the save result.
func AssembleNotifyTool(deps NotifyAssemblyDeps) func(ctx context.Context, agent string, profile *core.Profile) ([]string, core.ToolChange, error) {
	return func(ctx context.Context, agent string, profile *core.Profile) ([]string, core.ToolChange, error) {
		tools := slices.Clone(profile.Tools)
		hasNotify := slices.Contains(tools, core.NotifyToolName)

		resolution, err := ResolveNotifyMode(profile)
		if err != nil {
			// 通知意图本身非法（字面量 / 组合矛盾）：不替用户猜意图，也不在这里抢答，
			// 交给装载期规则 10 与保存期校验给出标准拒绝文案。
			// An invalid intent is reported by the load-time/binding gates, not here.
			return tools, core.ToolChange{}, nil
		}

		if resolution.Mode == core.NotifyModeNone {
			if !hasNotify {
				return tools, core.ToolChange{}, nil
			}
			return slices.DeleteFunc(tools, func(name string) bool { return name == core.NotifyToolName }),
				core.ToolChange{Removed: []string{core.NotifyToolName}}, nil
		}

		eligible, err := notifyIntentEligible(ctx, deps, agent, profile, resolution.Mode)
		if err != nil {
			return nil, core.ToolChange{}, err
		}
		// 渠道资格未通过时不改装配：保存期绑定校验会点名具体问题（缺失 / 未发布 /
		// 无资格），这里加一个必然被拒的 notify 只会掩盖真正要修的地方。
		// Ineligible channels leave the list untouched; the binding check names the real
		// problem right after this hook.
		if !eligible || hasNotify {
			return tools, core.ToolChange{}, nil
		}
		return append(tools, core.NotifyToolName), core.ToolChange{Added: []string{core.NotifyToolName}}, nil
	}
}

// notifyIntentEligible 判定通知意图下是否存在可用的渠道集合：`all_eligible` 要求至少
// 一个当前有资格的渠道；`allowlist` 要求声明的每个名字都存在且有资格。
// notifyIntentEligible requires at least one eligible channel for all_eligible and an
// existing, eligible channel for each declared name under allowlist.
func notifyIntentEligible(ctx context.Context, deps NotifyAssemblyDeps, agent string,
	profile *core.Profile, mode core.NotifyMode,
) (bool, error) {
	if deps.Channels == nil {
		return false, nil
	}
	agentUID := agentIdentity(ctx, deps.Lifecycle, agent)
	channels, err := deps.Channels.ListNotifyChannels(ctx)
	if err != nil {
		return false, fmt.Errorf("受控工具装配: 读取通知渠道注册表: %w", err)
	}
	eligible := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		if core.Eligible(channel.Scope, channel.OwnerAgentUID, agentUID) {
			eligible[channel.Name] = struct{}{}
		}
	}
	if mode == core.NotifyModeAllEligible {
		return len(eligible) > 0, nil
	}
	for _, name := range profile.NotifyChannels {
		if _, ok := eligible[name]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// assembleNotifyToolsFor 是重路径上的装配钩子：渠道目录与生命周期状态文件都取自
// 运行期共享实例，写面与准入看到的是同一份真值。
// assembleNotifyToolsFor binds the hook to the runtime's shared instances.
func assembleNotifyToolsFor(rt *heavyRuntime) func(ctx context.Context, agent string, profile *core.Profile) ([]string, core.ToolChange, error) {
	deps := NotifyAssemblyDeps{Lifecycle: rt.lifecycle}
	if rt.channelRead != nil {
		deps.Channels = rt.channelRead
	}
	return AssembleNotifyTool(deps)
}

// agentIdentity 取生命周期状态文件里的稳定身份；缺失 / 损坏按空身份处理（与
// `SaveBindingCheck` 同一口径：没有可信身份时专属资源不通过、共享资源照常）。
// agentIdentity mirrors SaveBindingCheck: an absent state file yields an empty uid.
func agentIdentity(ctx context.Context, lifecycle *workspace.LifecycleStore, agent string) core.AgentUID {
	if lifecycle == nil {
		return ""
	}
	if rec, exists, err := lifecycle.Read(ctx, agent); err == nil && exists {
		return rec.AgentUID
	}
	return ""
}
