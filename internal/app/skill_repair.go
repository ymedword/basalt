package app

// 本文件实现 US-25C §2.3.7 的「运行期有界技能修复」与角色判定入口（FR-25C-17）。
//
// 分工：`skill_republish.go` 负责**一次**联动重发（锁内复核、发布前复查、逐 Agent
// 结果）；本文件负责在 control 角色上把发布请求里没修成的项（`failed` / `missing` /
// `superseded`）在后续轮次补齐。两条判据必须同源——"这个 Agent 是否还落后"与
// `republishOne` 的 `skipped` 用的是同一句比较（`skillRefIsLive`），这里**不得**另写
// 一套"大概落后"的启发式：判据两处，迟早给出两种结论。
//
// 三条边界：
//  1. Worker 不发起重发：`assetSyncRepair` 按角色决定入口是否为 nil，与装配了什么无关；
//  2. 有界：每轮最多 `skill_repair_batch` 个、失败按 `min(2^(n-1) × interval, 5m)`
//     退避、连续 8 次转 `blocked` 并记 ERROR；**运行期每一次全量扫描轮**
//     （`AssetSync` 的 `fullScanDue`）先复位 blocked / 退避计数与游标，再按同一批次上限
//     重试——复位不解除批次上限，也不退化成无界全量重发；启动兜底的 `RepairAll`
//     仍是独立入口（一次全量、串行）；
//  3. 游标只在进程内：`RepairOnce` 的轮转游标跨轮推进，复位与 `RepairAll` 清空——
//     重启后从头扫，最坏多扫一遍，不会漏。
//
// The repairer reuses the exact staleness comparison of republishOne, is bounded per
// round, backs off on failure, blocks after eight consecutive failures and resets its
// retry state on every runtime full-scan round (batch cap stays in force). Workers never
// get a repair entry point.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

const (
	// skillRepairScanPageSize 是扫描已发布 bundle 的页大小（模块卡 §2.3.7：每页 100）。
	// skillRepairScanPageSize is the published-bundle scan page size.
	skillRepairScanPageSize = 100
	// skillRepairDefaultBatch 是每轮最多尝试的条数（`skill_repair_batch` 的缺省值）。
	// skillRepairDefaultBatch bounds how many items one round may attempt.
	skillRepairDefaultBatch = 5
	// skillRepairDefaultInterval 是退避基数缺省值（生产传 `assets.sync_interval`）。
	// skillRepairDefaultInterval is the fallback backoff base.
	skillRepairDefaultInterval = 30 * time.Second
	// skillRepairBlockedRounds 是连续失败转 `blocked` 的轮数（模块卡 §2.3.7 固定为 8）。
	// skillRepairBlockedRounds is the fixed consecutive-failure budget before blocking.
	skillRepairBlockedRounds = 8
	// skillRepairBackoffCap 是单次退避上限（模块卡 §2.3.7：5 分钟）。
	// skillRepairBackoffCap is the fixed backoff ceiling.
	skillRepairBackoffCap = 5 * time.Minute
)

// runtimeRole 是进程在部署里的角色：control 负责共享初始化写入与技能修复，worker 只做
// 只读装配与执行协调写入（§2.3.1 / FR-25C-12）。**零值必须是 worker**——未判定的角色
// 不得默认拿到定义写权。
// runtimeRole is the process role; the zero value must be the worker so an undecided
// role never inherits write authority.
type runtimeRole int

const (
	// roleWorker 是零值：未判定按最小权限处理。
	// roleWorker is the zero value: undecided means least privilege.
	roleWorker runtimeRole = iota
	// roleControl 是控制面（`serve`）。
	// roleControl is the control plane.
	roleControl
)

// skillRepairVersions 是修复器对共享记录的最小读取面：Agent 发布指针、版本行与技能
// 当前已发布版本。定义在消费方，生产实现是 `*postgres.ResourceStore`。
// skillRepairVersions is the repairer's consumer-side view of the shared records.
type skillRepairVersions interface {
	PointerByUID(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) (core.ResourcePointer, error)
	VersionByID(ctx context.Context, id int64) (core.ResourceVersion, error)
	PointerVersion(ctx context.Context, kind core.ResourceKind, resourceID string) (int64, error)
}

// SkillRepairerDeps 是运行期修复器的装配输入（模块卡 §3.1.4 冻结）：**仅 control 装配**。
// 缺省 logger / now / batch / interval 用安全缺省值；三条必需依赖缺失时每次调用都
// 失败关闭（不 panic、不静默跳过）。
// SkillRepairerDeps is the repairer wiring input; it is only assembled on the control
// role, and a half-wired assembly fails closed per call.
type SkillRepairerDeps struct {
	Distribution core.AssetDistributionPort
	Versions     skillRepairVersions
	Admin        SkillRepublisher
	Logger       *slog.Logger
	Now          func() time.Time
	Batch        int
	Interval     time.Duration
}

// RepairReport 是一轮修复的读数：尝试数、逐项结果与仍处于 blocked 的 Agent。
// `Blocked` 按 Agent 名去重升序——它同时是"为什么这轮没修"的可见原因。
// RepairReport is one round's outcome; Blocked lists the agents still blocked.
type RepairReport struct {
	Attempted int
	Results   []core.BundlePublishResult
	Blocked   []string
}

// skillRepairerState 是单个（Agent, 技能）的进程内重试状态。
// skillRepairerState is the in-process retry state of one (agent, skill) pair.
type skillRepairerState struct {
	failures    int
	nextAttempt time.Time
	blocked     bool
}

// staleSkill 是一条"已发布 bundle 仍引用旧技能版本"的待修项。
// staleSkill is one published bundle that still references an outdated skill version.
type staleSkill struct {
	agent string
	skill string
}

// key 是轮转游标与状态表的键：先 Agent 名、后技能名，与排序口径一致。
// key orders by agent first, then skill — the same order the rotation uses.
func (s staleSkill) key() string { return s.agent + "\x00" + s.skill }

// SkillRepairer 是 control 侧的技能修复器：单飞、有界、按 Agent 名轮转。
// SkillRepairer runs one bounded repair round at a time, rotating by agent name.
type SkillRepairer struct {
	deps     SkillRepairerDeps
	logger   *slog.Logger
	now      func() time.Time
	batch    int
	interval time.Duration

	mu     sync.Mutex
	states map[string]*skillRepairerState
	cursor string
}

// NewSkillRepairer 装配修复器；缺省值与生产缺省一致（batch 5、退避 30s、上限 5m 固定）。
// NewSkillRepairer assembles the repairer with the production defaults.
func NewSkillRepairer(deps SkillRepairerDeps) *SkillRepairer {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	batch := deps.Batch
	if batch <= 0 {
		batch = skillRepairDefaultBatch
	}
	interval := deps.Interval
	if interval <= 0 {
		interval = skillRepairDefaultInterval
	}
	return &SkillRepairer{
		deps:     deps,
		logger:   logger,
		now:      now,
		batch:    batch,
		interval: interval,
		states:   make(map[string]*skillRepairerState),
	}
}

// wired 判断三条必需依赖是否齐全。
// wired reports whether the mandatory dependencies are present.
func (r *SkillRepairer) wired() bool {
	return r != nil && r.deps.Distribution != nil && r.deps.Versions != nil && r.deps.Admin != nil
}

// RepairOnce 是运行期一轮有界修复：扫一遍已发布 bundle，挑出仍落后于当前技能版本的项，
// 跳过 blocked 与退避未到期的项，按 Agent 名轮转取最多 `batch` 个尝试。
// RepairOnce runs one bounded round and never retries a blocked or backed-off item.
func (r *SkillRepairer) RepairOnce(ctx context.Context) (RepairReport, error) {
	return r.run(ctx, false)
}

// RepairAll 是全量兜底（启动期 `repairStaleBundleSkills`）：清空重试状态与游标，本轮
// 不看退避、不受 batch 限制，把当前所有落后项串行修一遍。
// RepairAll resets retry state and attempts every stale item serially.
func (r *SkillRepairer) RepairAll(ctx context.Context) (RepairReport, error) {
	return r.run(ctx, true)
}

// repairBounded 是运行期**全量扫描轮**的入口（FR-25C-17「下一轮全量扫描复位」）：
// `reset` 为真时先复位重试状态与游标（故障解除后 blocked 项不再永久卡死），随后仍按
// `batch` 上限跑一轮有界修复——复位**不**解除批次上限，与 `RepairAll` 的全量串行
// 不同。复位与轮次在同一把单飞锁内完成，启动兜底不会插进两者之间。
// repairBounded resets retry state on a full-scan round and then runs one bounded round;
// the batch cap stays in force, unlike the startup RepairAll drain.
func (r *SkillRepairer) repairBounded(ctx context.Context, reset bool) (RepairReport, error) {
	if !r.wired() {
		return RepairReport{}, errors.New("技能修复器未接线：缺资产分发端口 / 版本读取面 / 重发实现")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if reset {
		r.states = make(map[string]*skillRepairerState)
		r.cursor = ""
	}
	return r.runLocked(ctx, false)
}

// run 是两种导出入口的共同主体；`full` 区分"有界轮次"与"全量复位"。
// run is the shared body; full selects the reset-and-drain behaviour.
func (r *SkillRepairer) run(ctx context.Context, full bool) (RepairReport, error) {
	if !r.wired() {
		return RepairReport{}, errors.New("技能修复器未接线：缺资产分发端口 / 版本读取面 / 重发实现")
	}
	// 单飞：同一实例的轮次不叠加（AssetSync 串行调用，这里防的是启动兜底与运行期轮次
	// 撞在一起）。
	// Single-flight: rounds never overlap on one instance.
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runLocked(ctx, full)
}

// runLocked 是持有单飞锁的轮次主体。
// runLocked runs one round; the caller holds the single-flight lock.
func (r *SkillRepairer) runLocked(ctx context.Context, full bool) (RepairReport, error) {
	stale, err := r.scanStale(ctx)
	if err != nil {
		return RepairReport{}, err
	}
	if full {
		r.states = make(map[string]*skillRepairerState)
		r.cursor = ""
	}
	// 已不再落后的项清状态：blocked 也不能永久挂着（外部修好后本轮就应消失）。
	// Forget state for items that are no longer stale.
	current := make(map[string]struct{}, len(stale))
	for _, item := range stale {
		current[item.key()] = struct{}{}
	}
	for key := range r.states {
		if _, ok := current[key]; !ok {
			delete(r.states, key)
		}
	}

	report := RepairReport{}
	now := r.now()
	due := make([]staleSkill, 0, len(stale))
	for _, item := range stale {
		state := r.stateOf(item.key())
		if state.blocked {
			report.Blocked = appendUniqueAgent(report.Blocked, item.agent)
			continue
		}
		if now.Before(state.nextAttempt) {
			continue
		}
		due = append(due, item)
	}
	if !full {
		if r.cursor != "" {
			if idx := slices.IndexFunc(due, func(item staleSkill) bool { return item.key() > r.cursor }); idx > 0 {
				due = append(due[idx:], due[:idx]...)
			}
		}
		if len(due) > r.batch {
			due = due[:r.batch]
		}
	}
	for _, item := range due {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result := r.deps.Admin.RepublishOne(ctx, item.agent, item.skill, skillRepairActor())
		report.Attempted++
		report.Results = append(report.Results, result)
		r.recordResult(item, result, now)
		r.cursor = item.key()
	}
	return report, nil
}

// scanStale 按页（100）遍历已发布 bundle，返回仍落后于当前技能版本的（Agent, 技能）。
// 判据与 `republishOne` 的 `skipped` 是同一句比较（`skillRefIsLive`）。
// scanStale pages through published bundles and returns the outdated skill references.
func (r *SkillRepairer) scanStale(ctx context.Context) ([]staleSkill, error) {
	var out []staleSkill
	seen := make(map[string]struct{})
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := r.deps.Distribution.ListPublishedBundlesPage(ctx, after, skillRepairScanPageSize)
		if err != nil {
			return nil, fmt.Errorf("扫描已发布 Agent 资产: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, bundle := range page {
			stale, err := r.staleSkillsOf(ctx, bundle)
			if err != nil {
				return nil, err
			}
			for _, item := range stale {
				if _, ok := seen[item.key()]; ok {
					continue
				}
				seen[item.key()] = struct{}{}
				out = append(out, item)
			}
		}
		if len(page) < skillRepairScanPageSize {
			break
		}
		after = string(page[len(page)-1].AgentUID)
	}
	slices.SortFunc(out, func(a, b staleSkill) int { return strings.Compare(a.key(), b.key()) })
	return out, nil
}

// staleSkillsOf 读一个已发布 bundle 的清单，逐条技能与当前已发布版本比对。
// 指针 / 版本行缺失按"已不在发布集"跳过（由同步与生命周期收口，不是修复对象）；
// 技能侧没有已发布版本时该引用算落后——它只能以 `missing` 结束，交给有界重试收口。
// staleSkillsOf compares every skill reference of one bundle with the live version.
func (r *SkillRepairer) staleSkillsOf(ctx context.Context, bundle core.PublishedBundle) ([]staleSkill, error) {
	if bundle.Agent == "" {
		return nil, nil
	}
	pointer, err := r.deps.Versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(bundle.AgentUID))
	if errors.Is(err, core.ErrResourceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 Agent %s 的资产指针: %w", bundle.Agent, err)
	}
	version, err := r.deps.Versions.VersionByID(ctx, pointer.PublishedVersionID)
	if errors.Is(err, core.ErrResourceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 Agent %s 的资产版本 %d: %w", bundle.Agent, pointer.PublishedVersionID, err)
	}
	var manifest assets.Manifest
	if err := json.Unmarshal([]byte(version.ManifestJSON), &manifest); err != nil {
		r.logger.Warn("技能修复：已发布清单无法解析，跳过",
			"agent", bundle.Agent, "version_id", version.ID, "error", err)
		return nil, nil
	}
	out := make([]staleSkill, 0, len(manifest.Skills))
	for _, ref := range manifest.Skills {
		liveID, err := r.deps.Versions.PointerVersion(ctx, core.ResourceSkill, ref.Name)
		if errors.Is(err, core.ErrResourceNotFound) {
			out = append(out, staleSkill{agent: bundle.Agent, skill: ref.Name})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("读取技能 %s 的当前发布版本: %w", ref.Name, err)
		}
		if !skillRefIsLive(ref, liveID) {
			out = append(out, staleSkill{agent: bundle.Agent, skill: ref.Name})
		}
	}
	return out, nil
}

// stateOf 取（必要时新建）一项的重试状态。
// stateOf returns (creating if needed) the retry state of one item.
func (r *SkillRepairer) stateOf(key string) *skillRepairerState {
	state, ok := r.states[key]
	if !ok {
		state = &skillRepairerState{}
		r.states[key] = state
	}
	return state
}

// recordResult 按本轮结果推进状态：成功 / skipped 清状态；可重试失败累加并按
// `min(2^(n-1) × interval, cap)` 排下一次；满 8 次转 blocked 并记 ERROR。
// recordResult advances the retry state according to the outcome.
func (r *SkillRepairer) recordResult(item staleSkill, result core.BundlePublishResult, now time.Time) {
	switch result.Status {
	case core.BundlePublishOK, core.BundlePublishSkipped:
		delete(r.states, item.key())
		return
	}
	state := r.stateOf(item.key())
	state.failures++
	if state.failures >= skillRepairBlockedRounds {
		if !state.blocked {
			state.blocked = true
			r.logger.Error("技能修复连续失败已达上限，转 blocked 等待全量扫描复位",
				"agent", item.agent, "skill", item.skill, "rounds", state.failures,
				"status", result.Status, "reason", result.Error)
		}
		return
	}
	state.nextAttempt = now.Add(r.backoff(state.failures))
}

// backoff 返回第 n 次连续失败后的退避：`min(2^(n-1) × interval, 5m)`。
// backoff returns min(2^(n-1) * interval, 5m).
func (r *SkillRepairer) backoff(failures int) time.Duration {
	shift := failures - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 20 {
		shift = 20
	}
	d := time.Duration(int64(1)<<uint(shift)) * r.interval
	if d <= 0 || d > skillRepairBackoffCap {
		return skillRepairBackoffCap
	}
	return d
}

// appendUniqueAgent 按出现顺序去重追加 Agent 名（stale 已按名排序，结果因此稳定）。
// appendUniqueAgent appends an agent name once, preserving order.
func appendUniqueAgent(list []string, agent string) []string {
	if slices.Contains(list, agent) {
		return list
	}
	return append(list, agent)
}

// skillRepairActor 是后台修复发布的操作主体：不是用户发起的写，也不是启动迁移，
// 因此方法记 `migration`、来源记 `skill_repair`，操作行里能与其他写路径区分开。
// skillRepairActor is the background repair actor recorded on the publication rows.
func skillRepairActor() core.Actor {
	return core.Actor{
		Subject: core.ActorSubjectMigration,
		Method:  core.AuthMethodMigration,
		Source:  "skill_repair",
	}
}

// assetSyncRepair 按角色给出 `AssetSyncDeps.Repair` 入口：control 才真的接到修复器，
// worker 恒为 nil——角色决定，与装配了什么无关（FR-25C-17）。`fullScan` 由同步轮传入：
// 全量扫描轮先复位 blocked / 退避计数再跑一轮有界修复（FR-25C-17 的复位语义），
// 其余轮次只跑有界修复。
// assetSyncRepair returns the repair hook for the role; workers always get nil. The
// full-scan flag resets blocked/backoff state but keeps the per-round batch cap.
func assetSyncRepair(role runtimeRole, repairer *SkillRepairer) func(context.Context, bool) error {
	if role != roleControl || repairer == nil {
		return nil
	}
	return func(ctx context.Context, fullScan bool) error {
		report, err := repairer.repairBounded(ctx, fullScan)
		if err != nil {
			return err
		}
		if report.Attempted > 0 || fullScan {
			repairer.logger.Info("技能修复轮次完成",
				"attempted", report.Attempted, "blocked", len(report.Blocked),
				"full_scan_reset", fullScan)
		}
		return nil
	}
}

// RepairDeps 是 control 启动兜底（`repairStaleBundleSkills`）的装配输入：它在
// `BackfillBundlePayloads` 之后、`NewResourceAdmin` 之前执行，因此自带一份联动重发
// 装配，而不是等资源写面就绪。`AgentGate` 必须是进程里那一把（模块卡 §2.3.7 锁序）。
// RepairDeps wires the control-startup fallback, which runs before the resource admin
// exists; AgentGate must be the process-wide instance.
type RepairDeps struct {
	Distribution  core.AssetDistributionPort
	AssetRoot     string
	WorkspaceRoot string
	Versions      skillRepublishStore
	Operations    core.OperationStore
	Payloads      bundlePayloadReader
	PayloadStore  core.BundlePayloadStore
	AgentGate     *core.AgentGate
	MaxBytes      int64
	Logger        *slog.Logger
	Now           func() time.Time
	Batch         int
	Interval      time.Duration
}

// repairStaleBundleSkills 是 control 启动期的全量兜底：内部收敛到 `RepairAll`——
// 一轮全量、串行、不受退避与 batch 限制。运行期由 `SkillRepairer.RepairOnce` 每轮补齐，
// 这里只保证"重启后不用等下一轮就能修"。
// repairStaleBundleSkills is the control-startup drain; it converges to RepairAll.
func repairStaleBundleSkills(ctx context.Context, deps RepairDeps) (RepairReport, error) {
	republisher := newSkillRepublisher(skillRepublishDeps{
		AssetRoot:     deps.AssetRoot,
		WorkspaceRoot: deps.WorkspaceRoot,
		Versions:      deps.Versions,
		Operations:    deps.Operations,
		Payloads:      deps.Payloads,
		PayloadStore:  deps.PayloadStore,
		AgentGate:     deps.AgentGate,
		MaxBytes:      deps.MaxBytes,
		Logger:        deps.Logger,
		Now:           deps.Now,
	})
	repairer := NewSkillRepairer(SkillRepairerDeps{
		Distribution: deps.Distribution,
		Versions:     deps.Versions,
		Admin:        republisher,
		Logger:       deps.Logger,
		Now:          deps.Now,
		Batch:        deps.Batch,
		Interval:     deps.Interval,
	})
	return repairer.RepairAll(ctx)
}
