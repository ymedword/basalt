package app

// 本文件是通知意图三态的解析与准入冻结（颗粒度文档 R4.7 / T122）。
//
// 三态的边界（契约 agent-md §2、模块卡 R4.7）：
//   - `none`：任何 notify 调用固定报 `notify_disabled`，不查渠道、不投递；
//   - `all_eligible`：准入枚举「共享 + 本 Agent 专属」的渠道，把**具体 UID** 冻进
//     `run_notify_refs`——集合里永远不出现 `*` 通配；
//   - `allowlist`：只冻结 `notify_channels` 声明的渠道，每个名字必须已有绑定、
//     UID 是当前发布指针、且本 Agent 有资格。
//
// 未声明 `notify_mode` 的旧文件按兼容规则**读取**（空渠道 → all_eligible，非空 →
// allowlist），读取不改写文件；保存期才要求显式写出三态。
// 空数组**不得**表示禁用——禁用只有 `notify_mode: none` 一种表达。
//
// This file resolves the three-state notification intent and freezes the concrete
// channel uids at admission; there is no wildcard, and an undeclared mode is read
// through the documented compatibility rule without rewriting the file.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// NotifyChannelCatalog 是渠道注册表的只读面；`*storage.NotifyChannelRepo` 满足它。
// 返回集合已排除墓碑行，顺序按名称。
// NotifyChannelCatalog is the read-only channel registry face (tombstones excluded).
type NotifyChannelCatalog interface {
	ListNotifyChannels(ctx context.Context) ([]core.NotifyChannel, error)
}

// NotifyBindingSource 读某 Agent 的绑定投影；`*storage.BindingStore` 满足它。
// withoutBindings 不是"没有绑定"的同义词——读失败与没有行必须能区分，因此用 error。
// NotifyBindingSource reads the binding projection; a read failure is not "no bindings".
type NotifyBindingSource interface {
	Bindings(ctx context.Context, agentUID core.AgentUID) ([]core.AgentResourceBinding, error)
}

// NotifyModeResolution 是解析后的通知模式；`Legacy` 表示文件没有显式声明
// `notify_mode`，当前值是按兼容规则推断出来的（页面据此显示「兼容模式」）。
// NotifyModeResolution carries the effective mode; Legacy marks a compatibility-derived one.
type NotifyModeResolution struct {
	Mode   core.NotifyMode
	Legacy bool
}

// ResolveNotifyMode 解析通知模式：显式声明逐字校验，未声明按兼容规则推断。
// 显式声明与渠道列表矛盾（none / all_eligible 配非空 notify_channels）按定义非法拒绝
// ——加载期只是 WARN，准入若照单全收就等于替用户猜意图。
// ResolveNotifyMode validates an explicit mode and applies the compatibility rule to an
// undeclared one; a contradictory declaration is refused rather than reinterpreted.
func ResolveNotifyMode(profile *core.Profile) (NotifyModeResolution, error) {
	if profile == nil {
		return NotifyModeResolution{}, errors.New("解析通知模式: 缺少 Agent 定义")
	}
	raw := strings.TrimSpace(string(profile.NotifyMode))
	if raw == "" {
		// 兼容推断走 core 里的**同一份**规则：管理台视图也要显示这个有效值，
		// 两处各写一份就是第二套约定。
		return NotifyModeResolution{
			Mode:   core.EffectiveNotifyMode("", profile.NotifyChannels),
			Legacy: true,
		}, nil
	}

	mode, err := core.ParseNotifyMode(raw)
	if err != nil {
		return NotifyModeResolution{}, fmt.Errorf("解析通知模式: %w", err)
	}
	switch mode {
	case core.NotifyModeNone, core.NotifyModeAllEligible:
		if len(profile.NotifyChannels) > 0 {
			return NotifyModeResolution{}, fmt.Errorf(
				"%w: notify_mode=%s 不得同时声明 notify_channels（实际 %d 项）",
				core.ErrAgentResourceBindingInvalid, mode, len(profile.NotifyChannels))
		}
	case core.NotifyModeAllowlist:
		if len(profile.NotifyChannels) == 0 {
			return NotifyModeResolution{}, fmt.Errorf(
				"%w: notify_mode=allowlist 至少需要一个渠道", core.ErrAgentResourceBindingInvalid)
		}
	}
	return NotifyModeResolution{Mode: mode}, nil
}

// NotifyGrantDeps 是冻结通知授权所需的只读依赖。
// NotifyGrantDeps holds the read-only dependencies needed to freeze the grant.
type NotifyGrantDeps struct {
	Channels NotifyChannelCatalog
	Bindings NotifyBindingSource
}

// FreezeNotifyGrant 在准入时把通知授权冻结成具体渠道集合（写入 `run_notify_refs` 的
// 就是这份集合）。`none` 不读任何目录；`allowlist` 每个声明名必须解析到绑定与当前
// 渠道行；`all_eligible` 枚举当时有资格的渠道并按 UID 稳定排序。
// FreezeNotifyGrant resolves the grant into concrete channel uids at admission.
func FreezeNotifyGrant(ctx context.Context, deps NotifyGrantDeps, agentUID core.AgentUID,
	profile *core.Profile,
) (core.NotifyManifest, error) {
	if _, err := core.ParseAgentUID(string(agentUID)); err != nil {
		return core.NotifyManifest{}, fmt.Errorf("冻结通知授权: %w", err)
	}
	resolution, err := ResolveNotifyMode(profile)
	if err != nil {
		return core.NotifyManifest{}, err
	}

	switch resolution.Mode {
	case core.NotifyModeNone:
		// 未启用通知：不读目录、不解析绑定——"查不到渠道"与"没开通知"是两件事。
		return core.NotifyManifest{Mode: core.NotifyModeNone, Channels: []core.NotifyChannelRef{}}, nil
	case core.NotifyModeAllowlist:
		return freezeAllowlist(ctx, deps, agentUID, profile.NotifyChannels)
	case core.NotifyModeAllEligible:
		return freezeAllEligible(ctx, deps, agentUID)
	default:
		// ParseNotifyMode 已挡住未知字面量；走到这里说明 core 与 app 的字面量集合漂移了。
		return core.NotifyManifest{}, fmt.Errorf("冻结通知授权: 未处理的通知模式 %q", resolution.Mode)
	}
}

// freezeAllowlist 只冻结声明过的渠道：每个名字必须先有绑定投影，再解析到当前渠道行，
// 三者（声明名、绑定 UID、当前指针 UID）一致才放行。
// freezeAllowlist freezes only declared channels, requiring declaration, binding and
// current channel row to agree on one uid.
func freezeAllowlist(ctx context.Context, deps NotifyGrantDeps, agentUID core.AgentUID,
	names []string,
) (core.NotifyManifest, error) {
	if deps.Bindings == nil {
		return core.NotifyManifest{}, errors.New("冻结通知授权: 缺少绑定投影，无法校验 allowlist")
	}
	bound, err := deps.Bindings.Bindings(ctx, agentUID)
	if err != nil {
		return core.NotifyManifest{}, fmt.Errorf("冻结通知授权: 读取绑定投影: %w", err)
	}
	byName := make(map[string]core.ResourceUID, len(bound))
	for _, entry := range bound {
		if entry.Kind == core.BindingNotifyChannel {
			byName[entry.RefName] = entry.ResourceUID
		}
	}

	current := make(map[string]core.NotifyChannel)
	if deps.Channels != nil {
		channels, err := deps.Channels.ListNotifyChannels(ctx)
		if err != nil {
			return core.NotifyManifest{}, fmt.Errorf("冻结通知授权: 读取渠道注册表: %w", err)
		}
		for _, channel := range channels {
			current[channel.Name] = channel
		}
	}

	out := core.NotifyManifest{
		Mode:     core.NotifyModeAllowlist,
		Channels: make([]core.NotifyChannelRef, 0, len(names)),
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, dup := seen[name]; dup {
			return core.NotifyManifest{}, fmt.Errorf("%w: notify_channels 重复声明 %q",
				core.ErrAgentResourceBindingInvalid, name)
		}
		seen[name] = struct{}{}

		bindingUID, ok := byName[name]
		if !ok {
			return core.NotifyManifest{}, fmt.Errorf(
				"%w: notify_channels 声明的渠道 %q 没有 resource_bindings.notify_channel 绑定",
				core.ErrAgentResourceBindingMissing, name)
		}
		channel, ok := current[name]
		if !ok {
			return core.NotifyManifest{}, fmt.Errorf(
				"%w: 通知渠道 %q 不存在或已删除", core.ErrAgentResourceUnavailable, name)
		}
		if channel.ResourceUID != bindingUID {
			return core.NotifyManifest{}, fmt.Errorf(
				"%w: 通知渠道 %q 的绑定指向 %s，当前渠道是 %s（同名重建需重新绑定）",
				core.ErrAgentResourceBindingInvalid, name, bindingUID, channel.ResourceUID)
		}
		if !core.Eligible(channel.Scope, channel.OwnerAgentUID, agentUID) {
			return core.NotifyManifest{}, fmt.Errorf(
				"%w: 通知渠道 %q 是其他 Agent 的专属资源", core.ErrResourceNotEligible, name)
		}
		out.Channels = append(out.Channels, core.NotifyChannelRef{
			Name:        channel.Name,
			ResourceUID: channel.ResourceUID,
		})
	}
	sortChannelRefs(out.Channels)
	return out, nil
}

// freezeAllEligible 枚举准入时有资格的渠道（共享 ∪ 本 Agent 专属）。没有有资格渠道
// 是合法结果（空集合），不是错误——"现在没渠道"与"没开通知"由模式区分。
// freezeAllEligible enumerates the currently eligible channels; an empty set is legal.
func freezeAllEligible(ctx context.Context, deps NotifyGrantDeps, agentUID core.AgentUID,
) (core.NotifyManifest, error) {
	out := core.NotifyManifest{
		Mode:     core.NotifyModeAllEligible,
		Channels: []core.NotifyChannelRef{},
	}
	if deps.Channels == nil {
		return out, nil
	}
	channels, err := deps.Channels.ListNotifyChannels(ctx)
	if err != nil {
		return core.NotifyManifest{}, fmt.Errorf("冻结通知授权: 读取渠道注册表: %w", err)
	}
	for _, channel := range channels {
		if !core.Eligible(channel.Scope, channel.OwnerAgentUID, agentUID) {
			continue
		}
		out.Channels = append(out.Channels, core.NotifyChannelRef{
			Name:        channel.Name,
			ResourceUID: channel.ResourceUID,
		})
	}
	sortChannelRefs(out.Channels)
	return out, nil
}

// sortChannelRefs 按资源 UID 升序（名称只是标签；清单哈希要求顺序稳定）。
// sortChannelRefs orders refs by resource uid for a stable manifest hash.
func sortChannelRefs(refs []core.NotifyChannelRef) {
	slices.SortFunc(refs, func(a, b core.NotifyChannelRef) int {
		return strings.Compare(string(a.ResourceUID), string(b.ResourceUID))
	})
}

// FrozenChannelUIDs 返回冻结集合里的具体 UID（按当前顺序）。集合里**不得**出现通配：
// 任何非法 UID 都是构造缺陷，直接返回错误而不是把 `*` 当渠道名放行。
// FrozenChannelUIDs returns the concrete uids; a wildcard is a construction defect.
func FrozenChannelUIDs(manifest core.NotifyManifest) ([]core.ResourceUID, error) {
	out := make([]core.ResourceUID, 0, len(manifest.Channels))
	for _, channel := range manifest.Channels {
		uid, err := core.ParseResourceUID(string(channel.ResourceUID))
		if err != nil {
			return nil, fmt.Errorf("冻结通知集合含非法渠道身份 %q: %w", channel.ResourceUID, err)
		}
		out = append(out, uid)
	}
	return out, nil
}
