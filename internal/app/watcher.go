package app

// 本文件是配置热加载在装配层的联动（颗粒度文档 §3.1.21 / 约束 39、图 E）：
// workspace.Watcher 只上报防抖后的变更集，这里做一次全量 `LoadAll`，逐 dirty Agent
// 决定新增 / 替换 / 保留旧 / 移除，并让注册表与调度器保持**同一份**集合——管理台、
// 准入与调度器读的都是注册表快照，不在三处各缓存一份。
// This file wires the workspace watcher to the registry and the scheduler. The watcher
// only reports a debounced change set; this layer rescans, then advances the registry
// and the scheduler together, so the console, admission and scheduling agree.

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// watcherSkillPendingMessage 是技能变更的唯一 WARN 文案（注 9 / US-11 不自动发布）。
// watcherSkillPendingMessage is the single WARN text for a changed skill work copy.
const watcherSkillPendingMessage = "技能内容已变化，待发布；本版本不自动发布"

// ScheduleRegistry 是调度器在注册表联动里的消费面（§3.1.21 逐字）：core.Scheduler
// 直接满足它，装配层也可以传入兼任 claim 基线的编辑适配器。
// ScheduleRegistry is the scheduler surface the registry sync consumes (§3.1.21).
type ScheduleRegistry interface {
	SetRegistered(agent string, registered bool) error
	ReconcileAgent(ctx context.Context, agent string, schedules []core.Schedule) error
}

// registrySync 把一次变更集落到注册表与调度器上；它自身无状态（并发调用由注册表
// 的 CAS 与调度器的 stateMu 串行化）。
// registrySync applies one change set to the registry and the scheduler; it holds no
// state of its own.
type registrySync struct {
	registry *agentRegistry
	loader   *workspace.AgentLoader
	skills   *workspace.SkillLibrary
	// versions 是技能发布指针的来源：注册装配据此执行约束 36 的技能分支（F-03 A）。
	// 与启动装配共用同一个判定；查询失败保留旧注册表，不把"读不到"当成"都正常"。
	// versions feeds the same registration judgement as startup assembly; a failed
	// lookup keeps the old registry instead of defaulting to "everything published".
	versions  skillPointerSource
	scheduler ScheduleRegistry
	logger    *slog.Logger
}

// newRegistrySync 组装联动器；logger 为 nil 时用默认出口。
// newRegistrySync wires the sync; a nil logger falls back to the default.
func newRegistrySync(registry *agentRegistry, loader *workspace.AgentLoader, skills *workspace.SkillLibrary, versions skillPointerSource, scheduler ScheduleRegistry, logger *slog.Logger) *registrySync {
	if logger == nil {
		logger = slog.Default()
	}
	return &registrySync{
		registry:  registry,
		loader:    loader,
		skills:    skills,
		versions:  versions,
		scheduler: scheduler,
		logger:    logger,
	}
}

// OnChange 处理一个防抖窗口的变更：全量 rescan 一次，再按变更集逐 Agent 比对。
// 全量扫描失败（工作区结构不可读）不推进任何状态——保留旧注册表比半套新状态安全。
// OnChange rescans once and then advances each dirty agent. A failed full scan leaves
// the old registry untouched rather than publishing a partial one.
func (s *registrySync) OnChange(ctx context.Context, cs workspace.ChangeSet) {
	entries, err := s.loader.LoadAll(s.registry.root)
	if err != nil {
		s.logger.Warn("热加载全量扫描失败，保留旧注册表", "error", err)
		return
	}
	// 技能发布指针每个防抖窗口只读一次：同一轮 rescan 的判定看到同一份集合。查询失败
	// **不得**默认为"无指针"或"全部已发布"——保留旧注册表，与全量扫描失败同一收口。
	// The publication set is read once per rescan window; a failed lookup keeps the old
	// registry, matching the failed-scan policy.
	published, err := publishedSkillNames(ctx, s.versions)
	if err != nil {
		s.logger.Warn("读取技能发布指针失败，保留旧注册表", "error", err)
		return
	}
	loaded := make(map[string]core.AgentEntry, len(entries))
	for _, entry := range entries {
		loaded[entry.Name] = entry
	}
	for _, name := range cs.Agents {
		s.syncAgent(ctx, name, loaded, published)
	}
	for _, name := range cs.Skills {
		s.syncSkill(name)
	}
}

// syncAgent 按 rescan 结果推进单个 Agent：目录消失 → 移除；校验失败 → 保留旧条目；
// 绑定技能无发布版本 → 收回（注册不变量优先于"坏编辑保旧"）；校验通过 → 原子替换并让
// 调度器同步（新增与修改走同一条路径，ReconcileAgent 幂等）。
// syncAgent advances one agent: removal, keep-old on validation failure, retirement on
// a missing skill publication, or replace.
func (s *registrySync) syncAgent(ctx context.Context, name string, loaded map[string]core.AgentEntry, published map[string]struct{}) {
	entry, ok := loaded[name]
	if !ok {
		s.removeAgent(name)
		return
	}
	if !entry.Registered {
		// 失败保旧（约束 28）：坏文件不得让 Agent 从注册表消失；新目录非法则本就不在
		// 注册表里，这条 WARN 是它唯一的痕迹。
		s.logger.Warn("Agent 校验失败，保留旧注册与旧快照", "agent", name, "reason", entry.RejectReason)
		return
	}
	if missing := missingPublishedSkills(entry, published); len(missing) > 0 {
		// 与"坏编辑保旧"不同：绑定了一个没有发布版本的技能不是临时坏文件，而是注册
		// 不变量（约束 36 / FR-042）被破坏——即使旧条目曾经可用，也必须收回。
		// Unlike a bad edit, this is a broken registration invariant: the agent is retired.
		s.retireUnpublishedSkill(name, missing)
		return
	}

	s.registry.Replace(name, entry)
	if err := s.scheduler.SetRegistered(name, true); err != nil {
		// 注册表已更新但调度器集合没跟上：如实留痕，不静默吞。
		s.logger.Warn("调度器登记 Agent 失败，注册表已更新", "agent", name, "error", err)
	}
	if err := s.scheduler.ReconcileAgent(ctx, name, schedulesOf(entry)); err != nil {
		s.logger.Warn("调度器对账 Agent 失败，注册表已更新", "agent", name, "error", err)
	}
	s.logger.Info("Agent 配置已热加载", "agent", name)
}

// removeAgent 收回一个已删除的 Agent：先移出注册表（准入立即失败关闭），再让调度器
// 退休其任务。注册表里本来就没有的名字是 no-op。
// removeAgent drops a deleted agent from the registry, then lets the scheduler retire
// its tasks; an agent that was never registered is a no-op.
func (s *registrySync) removeAgent(name string) {
	if !s.registry.Exists(name) {
		return
	}
	s.registry.Remove(name)
	if err := s.scheduler.SetRegistered(name, false); err != nil {
		s.logger.Warn("调度器移出 Agent 失败，注册表已收回", "agent", name, "error", err)
	}
	s.logger.Info("Agent 目录已移除，退出注册表", "agent", name)
}

// retireUnpublishedSkill 收回一个绑定技能已无发布版本的 Agent（F-03 A）：与目录删除同一
// 收口——先移出注册表（准入立即失败关闭），再让调度器退休其任务，并点名缺失技能。
// 从未注册过的名字（新目录本就不满足注册不变量）只留 WARN，不触碰调度器。
// retireUnpublishedSkill retires an agent whose bound skill has no live publication,
// using the same order as directory removal; an agent that was never registered only
// leaves the named WARN.
func (s *registrySync) retireUnpublishedSkill(name string, missing []string) {
	reason := skillPublicationReject(missing)
	if !s.registry.Exists(name) {
		s.logger.Warn("Agent 绑定技能无发布版本，不注册", "agent", name, "reason", reason)
		return
	}
	s.registry.Remove(name)
	if err := s.scheduler.SetRegistered(name, false); err != nil {
		s.logger.Warn("调度器移出 Agent 失败，注册表已收回", "agent", name, "error", err)
	}
	s.logger.Warn("Agent 绑定技能无发布版本，退出注册表", "agent", name, "reason", reason)
}

// syncSkill 只校验技能工作副本并留 WARN：US-11 不自动发布，注册表与技能版本指针
// 都不动（注 9）；校验失败同样不推进任何状态。
// syncSkill validates the work copy and logs the pending-release WARN only; US-11
// never publishes from a file change.
func (s *registrySync) syncSkill(name string) {
	if s.skills == nil {
		s.logger.Warn(watcherSkillPendingMessage, "skill", name)
		return
	}
	if _, err := s.skills.HashWorkCopy(name); err != nil {
		s.logger.Warn("技能工作副本校验失败，注册表不变", "skill", name, "error", err)
		return
	}
	s.logger.Warn(watcherSkillPendingMessage, "skill", name)
}

// schedulesOf 取条目里声明的定时任务（未解析出 Profile 的条目没有定义可对账）。
// schedulesOf returns the entry's declared schedules; a nil profile has none.
func schedulesOf(entry core.AgentEntry) []core.Schedule {
	if entry.Profile == nil {
		return nil
	}
	return append([]core.Schedule(nil), entry.Profile.Schedules...)
}

// startServeWatcher 按**显式依赖**组装 serve 的配置监听（图 A ⑧–⑩）：注册表、装载器、
// 技能库与调度联动端口在这里合成 rescan 回调，启动时机由 `Run` 的启动顺序决定。
// 调度联动端口**不得**为 nil：半联动状态下热加载只换注册表、不动调度器，管理台显示
// 已启用而调度器不知道——比不监听更难查，因此在这里失败关闭。
// startServeWatcher composes the serve-time watcher from explicit dependencies; a nil
// scheduler seam fails closed because a half-wired reload is worse than none.
func startServeWatcher(ctx context.Context, root string, registry *agentRegistry, loader *workspace.AgentLoader, skills *workspace.SkillLibrary, versions skillPointerSource, schedules ScheduleRegistry, logger *slog.Logger) (*workspace.Watcher, error) {
	if schedules == nil {
		return nil, errors.New("配置监听缺少调度联动端口——拒绝在半联动状态下启动")
	}
	if versions == nil {
		return nil, errors.New("配置监听缺少技能发布指针来源——拒绝在无法判定注册不变量时启动")
	}
	return startWatcher(ctx, root, newRegistrySync(registry, loader, skills, versions, schedules, logger), logger)
}

// startWatcher 建立监听并在后台消费事件直到 ctx 取消；返回的 Watcher 供调用方在
// 停机路径上显式关闭（关闭与 ctx 取消都收敛到同一个停止点，重复调用幂等）。
// startWatcher starts the watcher in the background until ctx is cancelled; the
// returned watcher can be closed explicitly on shutdown and closing is idempotent.
func startWatcher(ctx context.Context, root string, sync *registrySync, logger *slog.Logger) (*workspace.Watcher, error) {
	if logger == nil {
		logger = slog.Default()
	}
	watcher, err := workspace.NewWatcher(root, func(cs workspace.ChangeSet) {
		sync.OnChange(ctx, cs)
	}, logger)
	if err != nil {
		return nil, err
	}
	go func() {
		if runErr := watcher.Run(ctx); runErr != nil {
			logger.Warn("配置监听异常退出", "error", runErr)
		}
		if closeErr := watcher.Close(); closeErr != nil {
			logger.Warn("关闭配置监听失败", "error", closeErr)
		}
	}()
	return watcher, nil
}
