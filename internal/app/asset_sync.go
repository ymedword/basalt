package app

// 本文件实现 US-25C §2.3.4 的**已发布资产同步**：把数据库里的发布指针变成各进程可用
// 的本地资产，并把「哪些 Agent 已经真正就绪」做成可读快照，供领取前就绪门使用。
//
// 一轮的顺序是**契约**（§2.3.4）：发现（全量兜底 → 增量键集）→ 合并工作集 → 资源准备
// （Provider 目录、MCP 实例，先于 Entry 构建）→ 有界轮转处理（安装 → 技能物化 →
// Entry 构建）→ 注册表与调度集合同轮替换（固定顺序，仅 worker）→ 技能修复一轮
// （仅 control）→ 提交状态。
//
// 两条纪律贯穿全文件：
//   - **`installed` 不等于 `ready`**：安装只证明目录按哈希自洽；技能物化与 Entry 构建
//     每轮都重做，集合替换成功才置 `ready`。崩溃在"安装成功、注册之前"的目录因此能
//     靠 `assets.Resolve` 命中恢复，不需要重启；
//   - **失败项不得离开工作集**：失败只按 `attemptSeq` 退到队尾并退避，前 N 条持续
//     失败的条目不能饿死第 N+1 条。
//
// This file implements the published-asset sync loop: discovery, a four-state work set,
// bounded rotation with backoff, skill materialization, resource preparation before
// entry building, and same-round registry/scheduler replacement. `installed` never
// implies `ready`, and a failed item never leaves the work set.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// syncPageSize 是增量与全量翻页的页大小（§2.3.4 的 200）。
	// syncPageSize is the page size of both discovery orders.
	syncPageSize = 200
	// syncScanMaxPages / syncFullScanMaxPages 是单轮翻页上限：增量每轮至多 5 页，
	// 全量一趟至多 50 页（跨轮续跑）。两者都是**每轮**边界，不是总量边界。
	// syncScanMaxPages / syncFullScanMaxPages bound one round's paging.
	syncScanMaxPages     = 5
	syncFullScanMaxPages = 50
	// syncBackoffCap 是失败退避上限；syncFailCountCap 是退避指数的封顶（§2.3.4）。
	// syncBackoffCap caps the backoff; syncFailCountCap caps the exponent.
	syncBackoffCap   = 5 * time.Minute
	syncFailCountCap = 6

	// syncBundleSkillsDir 是 bundle 内技能目录段（`assets.skillsDirName` 的镜像）：
	// bundle 布局把技能文件放在 `skills/<技能名>/<相对路径>`，物化从该位置逐文件复制。
	// 布局段的唯一来源仍是 assets 包；这里只读它的产物，不定义布局。
	// syncBundleSkillsDir mirrors assets' in-bundle skills prefix for reading only.
	syncBundleSkillsDir = "skills"
	// syncSkillDefinitionFile 是技能定义文件名（`workspace.skillDefinitionFile` 的镜像），
	// 发布记录的文件清单必须包含它，否则 L1 卡片与 L2 读取都无从落地。
	// syncSkillDefinitionFile mirrors workspace.skillDefinitionFile.
	syncSkillDefinitionFile = "SKILL.md"

	syncMaterializedFilePerm os.FileMode = 0o600
	syncMaterializedDirPerm  os.FileMode = 0o750
)

// 工作集四态（§2.3.4 状态机）。字面量进 `SyncReport` 与 `assets status`，不得改词。
// The four work-set states; the literals are reported verbatim.
const (
	syncStatePending   = "pending"
	syncStateInstalled = "installed"
	syncStateReady     = "ready"
	syncStateBlocked   = "blocked"
)

// 阻塞原因（报告与 `assets status` 共用）。`payload_missing` / `corrupt` / `entry_invalid`
// 与模块卡逐字一致；其余是本文件新增的细分原因，仍属"未就绪"这一档。
// `not_installed` 只在只读诊断里出现：同步的工作集把"还没装"记作 `pending`（不是
// 失败），诊断则要如实说清"载荷在、本机还没装"。
// Blocked reasons; payload_missing, corrupt and entry_invalid are the card's literals,
// the rest are finer-grained "not ready" causes added here, and not_installed is the
// diagnostic's own word because the sync records pending instead.
const (
	syncBlockedPayloadMissing = "payload_missing"
	syncBlockedPayloadRead    = "payload_unavailable"
	syncBlockedCorrupt        = "corrupt"
	syncBlockedInstallFailed  = "install_failed"
	syncBlockedSkills         = "skills_unavailable"
	syncBlockedEntryInvalid   = "entry_invalid"
	syncBlockedNotInstalled   = "not_installed"
	syncBlockedFailed         = "failed"

	syncLifecycleUnknown = "unknown"
)

// AgentRegistryReplacer 是注册表的窄写面：单次快照 upsert、**不删除**未出现的 Agent
// （`agentRegistry.ReplaceAll`）。`AssetSync` 只依赖它，不依赖具体注册表类型。
// AgentRegistryReplacer is the narrow registry write face used by the sync.
type AgentRegistryReplacer interface {
	ReplaceAll(entries []core.AgentEntry) error
}

// ScheduleSetUpdater 是调度集合的窄写面（`core.Scheduler.ReplaceEntries`）：只重建本
// 进程内存集合，不碰共享库里的任务行。
// ScheduleSetUpdater is the narrow scheduler write face.
type ScheduleSetUpdater interface {
	ReplaceEntries(entries []core.AgentEntry) error
}

// 编译期断言：调度器已实现窄写面（`core.Scheduler.ReplaceEntries` 只重建本进程内存
// 集合，不动共享库任务行——FR-25C-10）。
// Compile-time assertion: the scheduler satisfies the sync's write face.
var _ ScheduleSetUpdater = (*core.Scheduler)(nil)

// BundleEntryBuilder 是 Entry 构建面（`loader.EntryFromBundle`）：定义取自已安装
// bundle，技能判定取自已物化的发布目录，Provider / MCP / 工具名只读本轮快照。
// BundleEntryBuilder builds one entry from the installed bundle and the round snapshot.
type BundleEntryBuilder interface {
	EntryFromBundle(root string, installed assets.Installed, prepared PreparedResources,
		published map[string]string) (core.AgentEntry, error)
}

// MCPPrepareResult 是一条 MCP 准备的结局（§2.3.8）：`Outcome` 区分复用 / 受控重建 /
// 首次连接 / 未 applied / 被顶替 / 失败，`Reused` 让"零副作用"可直接断言。
// MCPPrepareResult is one MCP preparation outcome.
type MCPPrepareResult struct {
	Name             string
	Outcome          string
	AppliedVersionID int64
	Reused           bool
	FailureClass     string
}

// MCPReport 是一轮 MCP 准备的读数：三态名单分开给（configured / connected / applied
// 由结果自证），`Tools` 是**本轮**已连接 server 的工具名快照。
// MCPReport separates the three name sets and carries the round's tool snapshot.
type MCPReport struct {
	Configured []string
	Connected  []string
	Tools      []string
	Results    []MCPPrepareResult
}

// AssetSyncDeps 是同步的全部装配输入。`Repair` 仅 control 非 nil——它同时是角色判据：
// 有 `Repair` 的进程只做安装 / 载荷补齐与技能修复，不做注册表与调度集合替换。
// AssetSyncDeps carries the wiring; a non-nil Repair marks the control role.
type AssetSyncDeps struct {
	WorkspaceRoot string
	AssetRoot     string
	Distribution  core.AssetDistributionPort
	Versions      assetBootstrapRecords
	Registry      AgentRegistryReplacer
	Scheduler     ScheduleSetUpdater
	Loader        BundleEntryBuilder
	// Providers / MCP 返回**本轮**的资源快照；为 nil 表示本轮不做该类准备（Entry 判定
	// 按缺失失败关闭）。Providers 由装配层持 `core.ResourceGate` 写锁调用（目录重建
	// 必须与发布互斥）；MCP 与此相反，**不得**在外层持 Gate——受控重建的提交段自己在
	// `commitReplace` 里按 `ResourceGate → entry.mu` 取锁（§2.3.8 M3），外层再持同一把
	// 非重入锁会自锁。
	// Providers is called under the ResourceGate write lock; MCP must not be, because the
	// commit section takes the very same gate itself (M3) and the lock is not reentrant.
	Providers func(ctx context.Context) (CatalogReport, error)
	MCP       func(ctx context.Context) (MCPReport, error)
	// Repair 仅 control 非 nil：每轮一轮有界的技能修复（§2.3.7）；第二个参数标记本轮
	// 是否发起全量扫描——全量扫描轮先复位重试状态，但批次上限照旧。
	Repair func(ctx context.Context, fullScan bool) error
	Logger *slog.Logger
	Now    func() time.Time
	// Interval 是**发现检查频率**（不是就绪时限）；0 表示只在启动时同步一次。
	Interval      time.Duration
	Timeout       time.Duration
	Batch         int
	MaxBytes      int64
	FullScanEvery int

	// prepared 是**未导出**的交接槽（US-25C §3.1.4 C14 / §2.3.5.1 T2）：同步器每轮把
	// 本轮资源快照发布到盒里，钉住装载面在准入时读最近一轮——两处判定必须同一份快照。
	// 只由装配层填写；nil 表示没有钉住消费方（轻装配）。
	// prepared hands each round's resource snapshot to pinned admission.
	prepared *preparedResourcesBox
}

// AgentSyncResult 是报告里的一条 Agent 读数（`assets status` 的打印面）。
// AgentSyncResult is one agent's reading in a sync report.
type AgentSyncResult struct {
	Agent         string
	State         string
	PointerHash   string
	InstalledHash string
	Skills        []string
	BlockedReason string
	NextAttempt   time.Time
	UpdatedAt     time.Time
}

// SyncReport 是一轮的读数：逐条结果、资源准备两段、移除与错误；`Skipped` 表示上一轮
// 尚未结束、本次调用被单飞挡回。
// SyncReport is one round's reading; Skipped marks a single-flight rejection.
type SyncReport struct {
	Round     int
	Started   time.Time
	Finished  time.Time
	Skipped   bool
	Items     []AgentSyncResult
	Removed   []string
	Providers CatalogReport
	MCP       MCPReport
	Errors    []string
}

// Item 在报告里按 Agent 名取一条读数。
// Item looks one agent's reading up by name.
func (r SyncReport) Item(agent string) (AgentSyncResult, bool) {
	for _, item := range r.Items {
		if item.Agent == agent {
			return item, true
		}
	}
	return AgentSyncResult{}, false
}

// Readiness 是一个 Agent 的就绪投影：领取前就绪门只读它，**不**读文件系统。
// `ReadyHash` 只在 `ready` 状态填充；`Lifecycle` 是共享生命周期索引的读数。
// Readiness is the cheap readiness projection read by the claim gate.
type Readiness struct {
	PointerHash   string
	ReadyHash     string
	Lifecycle     string
	BlockedReason string
}

// syncItem 是工作集里的一项（模块卡 §2.3.4 的最小字段集 + 每轮重用的内存产物）。
// 指针字段只在合并阶段变化；`installed` / `entry` / `published` 是跨轮复用的缓存，
// 任何字段变化都走"换新值"，绝不原地改 map / slice（旧快照要能被并发读）。
// syncItem is one work-set entry; cached artifacts are replaced, never mutated in place.
type syncItem struct {
	agent         string
	pointerUID    core.AgentUID
	targetHash    string
	installedHash string
	state         string

	installed  assets.Installed
	entry      core.AgentEntry
	entryReady bool
	published  map[string]string
	skills     []string

	lifecycle   string
	attemptSeq  uint64
	failCount   int
	nextAttempt time.Time
	lastError   string

	blockedReason string
	updatedAt     time.Time
}

// syncScanPass 是一趟**跨轮**全量扫描的进度：`seen` 是本趟已见到的 Agent，
// `last` 是已读到的 `resource_id`，`maxUpdated` 只作读数。中断时整份保留，下一轮续跑。
// syncScanPass keeps a full scan's progress across rounds so an interruption deletes nothing.
type syncScanPass struct {
	seen       map[string]struct{}
	last       string
	maxUpdated int64
}

// syncTarget 是一条发现结果（同 Agent 只保留首次出现，顺序即 `attemptSeq` 的分配序）。
// syncTarget is one discovery result; the slice order drives attemptSeq assignment.
type syncTarget struct {
	agent string
	uid   core.AgentUID
	hash  string
}

// syncRound 是一轮的工作副本：单飞保证同一时刻只有一个轮次在改它，提交时整体换入。
// syncRound is the round's private working copy, committed wholesale at the end.
type syncRound struct {
	items  map[string]*syncItem
	cursor core.PublishedBundleCursor
	pass   *syncScanPass
	seq    uint64
	round  int
}

func (r *syncRound) nextSeq() uint64 {
	r.seq++
	return r.seq
}

// results 把工作集投影成按 Agent 名排序的读数。
// results projects the work set into a name-sorted reading list.
func (r *syncRound) results() []AgentSyncResult {
	out := make([]AgentSyncResult, 0, len(r.items))
	for _, item := range r.items {
		out = append(out, AgentSyncResult{
			Agent:         item.agent,
			State:         item.state,
			PointerHash:   item.targetHash,
			InstalledHash: item.installedHash,
			Skills:        slices.Clone(item.skills),
			BlockedReason: item.blockedReason,
			NextAttempt:   item.nextAttempt,
			UpdatedAt:     item.updatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// syncBlockError 给失败点标注阻塞原因，供报告与 `assets status` 直接使用。
// syncBlockError attaches a blocked reason to a failure.
type syncBlockError struct {
	reason string
	cause  error
}

func (e *syncBlockError) Error() string { return fmt.Sprintf("%s: %v", e.reason, e.cause) }

func (e *syncBlockError) Unwrap() error { return e.cause }

func blockReason(err error) string {
	var blocked *syncBlockError
	if errors.As(err, &blocked) {
		return blocked.reason
	}
	return syncBlockedFailed
}

// AssetSync 是一份有界后台同步（§2.3.4）：构造只做装配校验、**不启动**任何协程；
// `SyncOnce` 单飞、整轮超时；`Run` 只负责按发现频率触发，停机由 ctx 取消收敛。
// AssetSync is the bounded background sync; the constructor starts nothing.
type AssetSync struct {
	workspaceRoot string
	assetRoot     string
	distribution  core.AssetDistributionPort
	versions      assetBootstrapRecords
	registry      AgentRegistryReplacer
	scheduler     ScheduleSetUpdater
	loader        BundleEntryBuilder
	providers     func(context.Context) (CatalogReport, error)
	mcp           func(context.Context) (MCPReport, error)
	repair        func(context.Context, bool) error
	logger        *slog.Logger
	now           func() time.Time

	interval      time.Duration
	timeout       time.Duration
	batch         int
	maxBytes      int64
	fullScanEvery int
	control       bool

	// prepared 发布每轮资源快照（见 `AssetSyncDeps.prepared`）。
	// prepared publishes each round's snapshot for pinned admission.
	prepared *preparedResourcesBox

	running atomic.Bool
	mu      sync.Mutex
	items   map[string]*syncItem
	cursor  core.PublishedBundleCursor
	pass    *syncScanPass
	seq     uint64
	round   int
}

// NewAssetSync 校验装配并返回同步对象。角色由 `Repair` 是否接线判定：control 不得带
// 注册表 / 调度集合 / Entry 构建器（它的工作区与 watcher 才是权威来源），worker 三者
// 必须齐备——缺项即接线错误，不静默降级成"只装本地"。
// NewAssetSync validates the wiring; the role is derived from Repair.
func NewAssetSync(deps AssetSyncDeps) (*AssetSync, error) {
	if deps.Distribution == nil {
		return nil, errors.New("资产同步未接线：缺分发读取面（不得只装本地）")
	}
	if deps.Versions == nil {
		return nil, errors.New("资产同步未接线：缺共享记录读取面（缺席者回读确认依赖它）")
	}
	if strings.TrimSpace(deps.AssetRoot) == "" {
		return nil, errors.New("资产同步未接线：资产根为空（assets.bundle_root 必须非空）")
	}
	if deps.MaxBytes <= 0 {
		return nil, errors.New("资产同步未接线：max_bundle_bytes 必须为正")
	}
	if deps.Interval < 0 {
		return nil, errors.New("assets.sync_interval 不得为负（0 表示只在启动时同步一次）")
	}
	control := deps.Repair != nil
	if control {
		if deps.Registry != nil || deps.Scheduler != nil || deps.Loader != nil {
			return nil, errors.New("资产同步接线错误：control 角色不得替换注册表 / 调度集合（工作区才是权威来源）")
		}
	} else {
		if deps.Registry == nil || deps.Scheduler == nil || deps.Loader == nil {
			return nil, errors.New("资产同步未接线：worker 角色必须提供注册表、调度集合与 Entry 构建器")
		}
		if strings.TrimSpace(deps.WorkspaceRoot) == "" {
			return nil, errors.New("资产同步未接线：worker 的工作区根为空（技能物化目标）")
		}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	batch := deps.Batch
	if batch <= 0 {
		batch = 50
	}
	fullScanEvery := deps.FullScanEvery
	if fullScanEvery <= 0 {
		fullScanEvery = 20
	}
	return &AssetSync{
		workspaceRoot: deps.WorkspaceRoot,
		assetRoot:     deps.AssetRoot,
		distribution:  deps.Distribution,
		versions:      deps.Versions,
		registry:      deps.Registry,
		scheduler:     deps.Scheduler,
		loader:        deps.Loader,
		providers:     deps.Providers,
		mcp:           deps.MCP,
		repair:        deps.Repair,
		logger:        logger,
		now:           now,
		interval:      deps.Interval,
		timeout:       timeout,
		batch:         batch,
		maxBytes:      deps.MaxBytes,
		fullScanEvery: fullScanEvery,
		control:       control,
		prepared:      deps.prepared,
		items:         make(map[string]*syncItem),
	}, nil
}

// SyncOnce 跑一轮同步。整轮受 `sync_timeout` 约束；上一轮未结束时直接返回 `Skipped`
// （单飞不叠加）。发现 / 准备 / 条目级的失败只进报告，**除超时外不返回错误**——一轮
// 有缺口不等于同步不可用，下一轮继续收敛。
// SyncOnce runs one round: single-flight, bounded by the round timeout, and only a
// cancelled round surfaces as an error.
func (s *AssetSync) SyncOnce(ctx context.Context) (SyncReport, error) {
	if !s.running.CompareAndSwap(false, true) {
		return SyncReport{Skipped: true, Round: s.currentRound()}, nil
	}
	defer s.running.Store(false)

	started := s.now()
	roundCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	round := s.beginRound()
	report := SyncReport{Round: round.round, Started: started}

	targets := s.discover(roundCtx, round, &report)
	s.merge(round, targets)
	if err := roundCtx.Err(); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("本轮在第 %d 轮被超时截断: %v", report.Round, err))
		s.finishRound(round, &report)
		return report, err
	}

	prepared := s.prepare(roundCtx, &report)
	s.process(roundCtx, round, prepared)
	s.applyEntries(roundCtx, round, &report)
	if s.control {
		s.repairOnce(roundCtx, round.round, &report)
	}
	s.finishRound(round, &report)
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := roundCtx.Err(); err != nil {
		return report, err
	}
	return report, nil
}

// Run 按发现检查频率触发 SyncOnce，直到 ctx 取消。它**不**做首次同步（启动期由装配层
// 显式调用一次，见 §2.3.1）；`Interval == 0` 表示只同步一次，这里直接阻塞到停机。
// 停机时，在飞的那一轮已经随 ctx 取消返回（join 在本协程内完成），协调器随后才关库。
// Run ticks SyncOnce until shutdown; the first sync belongs to startup.
func (s *AssetSync) Run(ctx context.Context) error {
	if s.interval <= 0 {
		<-ctx.Done()
		return nil
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			report, err := s.SyncOnce(ctx)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				s.logger.Warn("asset sync round failed",
					"round", report.Round, "error", err)
			}
		}
	}
}

// Readiness 返回当前就绪投影（只读副本）。领取前就绪门每次扫描读它，因此这里只取
// 短锁、不做任何 I/O。
// Readiness returns a short-lock copy of the readiness projection.
func (s *AssetSync) Readiness() map[string]Readiness {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Readiness, len(s.items))
	for name, item := range s.items {
		readyHash := ""
		if item.state == syncStateReady {
			readyHash = item.installedHash
		}
		out[name] = Readiness{
			PointerHash:   item.targetHash,
			ReadyHash:     readyHash,
			Lifecycle:     item.lifecycle,
			BlockedReason: item.blockedReason,
		}
	}
	return out
}

// ClaimReadyHash 是领取前就绪门的**带哈希**读数（US-25C ADJ-1 / ADJ-3）：返回本进程
// 就绪快照里的已安装哈希；`false` 表示该 Agent 暂不可领取。读数只取内存工作集，
// 不读文件系统、不查库。
//
// 角色差异是刻意的：Worker 只有在 `ready`（安装、技能物化、Entry 构建与两份集合替换
// 全部完成）才可领取；控制面不替换注册表 / 调度集合（`applyEntries` 早退），工作项
// 走完 `installed` 已是本进程的全部同步责任，因此两者同判。两种角色都要求
// `installedHash == targetHash`——已装目录不是当前指针那一份时不可领取。
// ClaimReadyHash returns the claimable hash from the in-memory work set; workers need
// ready items, the control needs installed-or-ready, and both need hash equality.
func (s *AssetSync) ClaimReadyHash(agent string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[agent]
	if !ok {
		return "", false
	}
	claimable := item.state == syncStateReady
	if s.control {
		claimable = item.state == syncStateInstalled || item.state == syncStateReady
	}
	if !claimable || item.installedHash == "" || item.installedHash != item.targetHash {
		return "", false
	}
	return item.installedHash, true
}

// —— 轮次骨架 ——

func (s *AssetSync) currentRound() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.round
}

// beginRound 深拷贝一份工作副本：本轮只改副本，提交时整体换入，`Readiness` 因此
// 永远读到一份完整快照（不会看见半轮）。
// beginRound deep-copies the work set so readers never observe a half round.
func (s *AssetSync) beginRound() *syncRound {
	s.mu.Lock()
	defer s.mu.Unlock()
	round := &syncRound{
		items:  make(map[string]*syncItem, len(s.items)),
		cursor: s.cursor,
		seq:    s.seq,
		round:  s.round,
	}
	if s.pass != nil {
		round.pass = &syncScanPass{
			seen:       make(map[string]struct{}, len(s.pass.seen)),
			last:       s.pass.last,
			maxUpdated: s.pass.maxUpdated,
		}
		for name := range s.pass.seen {
			round.pass.seen[name] = struct{}{}
		}
	}
	for name, item := range s.items {
		round.items[name] = cloneSyncItem(item)
	}
	return round
}

func cloneSyncItem(item *syncItem) *syncItem {
	clone := *item
	clone.skills = slices.Clone(item.skills)
	if item.published != nil {
		clone.published = make(map[string]string, len(item.published))
		for name, dir := range item.published {
			clone.published[name] = dir
		}
	}
	return &clone
}

// finishRound 提交本轮并填报告。
// finishRound commits the round and completes the report.
func (s *AssetSync) finishRound(round *syncRound, report *SyncReport) {
	s.mu.Lock()
	s.items = round.items
	s.cursor = round.cursor
	s.pass = round.pass
	s.seq = round.seq
	s.round = round.round + 1
	s.mu.Unlock()
	report.Finished = s.now()
	report.Items = round.results()
}

// —— 发现 ——

// discover 依次做全量兜底与增量键集，返回**有序**目标列表（顺序即 attemptSeq 分配序，
// 保证轮转公平与用例可复现）。全量整趟完成才处理缺席者。
// discover runs the full scan (when due) and the incremental keyset; order is stable.
func (s *AssetSync) discover(ctx context.Context, round *syncRound, report *SyncReport) []syncTarget {
	ordered := make([]syncTarget, 0, len(round.items))
	seenTarget := make(map[string]struct{}, len(round.items))
	add := func(rows []core.PublishedBundle) {
		for _, row := range rows {
			if _, ok := seenTarget[row.Agent]; ok {
				continue
			}
			seenTarget[row.Agent] = struct{}{}
			ordered = append(ordered, syncTarget{agent: row.Agent, uid: row.AgentUID, hash: row.BundleHash})
		}
	}

	if s.fullScanDue(round.round) {
		rows, completed := s.fullScan(ctx, round, report)
		add(rows)
		if completed {
			s.removeAbsent(ctx, round, report)
		}
	}
	if err := ctx.Err(); err != nil {
		return ordered
	}
	add(s.incrementalScan(ctx, round, report))
	return ordered
}

// fullScanDue 判定本轮是否**发起**一次全量兜底：首轮与每 `full_scan_every` 轮。
func (s *AssetSync) fullScanDue(round int) bool {
	return round == 0 || round%s.fullScanEvery == 0
}

// fullScan 按 `resource_id` 键集续跑一趟全量扫描：单轮至多 `syncFullScanMaxPages` 页，
// 中断时整份进度保留（`pass.last` / `pass.seen`），下一轮从断点继续，**不重头开始**。
// 返回值是本轮读到的行与"整趟是否完成"。
// fullScan continues one resource_id-ordered pass, keeping progress on failure.
func (s *AssetSync) fullScan(ctx context.Context, round *syncRound, report *SyncReport) ([]core.PublishedBundle, bool) {
	if round.pass == nil {
		round.pass = &syncScanPass{seen: make(map[string]struct{})}
	}
	pass := round.pass
	collected := make([]core.PublishedBundle, 0, syncPageSize)
	for page := 0; page < syncFullScanMaxPages; page++ {
		if err := ctx.Err(); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("全量扫描被取消（已读 %d 页，进度保留）: %v", page, err))
			return collected, false
		}
		rows, err := s.distribution.ListPublishedBundlesPage(ctx, pass.last, syncPageSize)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("全量扫描第 %d 页读取失败（下一轮续跑，不删除任何项）: %v", page+1, err))
			return collected, false
		}
		collected = append(collected, rows...)
		for _, row := range rows {
			pass.seen[row.Agent] = struct{}{}
			if row.UpdatedAt > pass.maxUpdated {
				pass.maxUpdated = row.UpdatedAt
			}
		}
		if len(rows) > 0 {
			pass.last = string(rows[len(rows)-1].AgentUID)
		}
		if len(rows) < syncPageSize {
			return collected, true
		}
	}
	return collected, false
}

// removeAbsent 在**整趟完成后**处理缺席者：逐个 `PointerByUID` 回读，只有墓碑或确实
// 无行才移出工作集；回读失败一律保留（"读不到"不是"已删除"）。
// removeAbsent re-confirms every absent agent as a tombstone before dropping it.
func (s *AssetSync) removeAbsent(ctx context.Context, round *syncRound, report *SyncReport) {
	if round.pass == nil {
		return
	}
	seen := round.pass.seen
	absent := make([]string, 0, len(round.items))
	for name := range round.items {
		if _, ok := seen[name]; !ok {
			absent = append(absent, name)
		}
	}
	sort.Strings(absent)
	for _, name := range absent {
		item := round.items[name]
		pointer, err := s.versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(item.pointerUID))
		switch {
		case errors.Is(err, core.ErrResourceNotFound):
			delete(round.items, name)
			report.Removed = append(report.Removed, name)
			s.logger.Info("asset sync removed an agent whose publication pointer is gone",
				"agent", name)
		case err != nil:
			report.Errors = append(report.Errors, fmt.Sprintf("全量扫描回读 %s 的发布指针失败（本轮不移除）: %v", name, err))
		case pointer.DeletedAt != nil:
			delete(round.items, name)
			report.Removed = append(report.Removed, name)
			s.logger.Info("asset sync removed a soft-deleted agent",
				"agent", name)
		}
	}
	round.pass = nil
}

// incrementalScan 按 `(updated_at, resource_id)` 复合键集前进：游标 = 本页末行的完整
// 元组，每行只出现一次；单轮至多 `syncScanMaxPages` 页，剩余的下一轮继续。
// incrementalScan advances the composite keyset cursor page by page.
func (s *AssetSync) incrementalScan(ctx context.Context, round *syncRound, report *SyncReport) []core.PublishedBundle {
	collected := make([]core.PublishedBundle, 0, syncPageSize)
	for page := 0; page < syncScanMaxPages; page++ {
		if err := ctx.Err(); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("增量扫描被取消（已读 %d 页）: %v", page, err))
			return collected
		}
		rows, err := s.distribution.ListPublishedBundlesAfter(ctx, round.cursor, syncPageSize)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("增量扫描第 %d 页读取失败（游标停在已读末行）: %v", page+1, err))
			return collected
		}
		if len(rows) == 0 {
			return collected
		}
		collected = append(collected, rows...)
		last := rows[len(rows)-1]
		round.cursor = core.PublishedBundleCursor{
			UpdatedAt:  last.UpdatedAt,
			ResourceID: string(last.AgentUID),
		}
		if len(rows) < syncPageSize {
			return collected
		}
	}
	return collected
}

// merge 把发现结果合进工作集：新 Agent 入队为 `pending`；哈希或身份变化回到 `pending`
// 并清掉旧的内存产物（旧安装目录保留在磁盘上，不降级）；哈希相同且已 `ready` 的不重新
// 入队（每轮仍参与集合替换）。
// merge folds discovery into the work set without downgrading installed directories.
func (s *AssetSync) merge(round *syncRound, targets []syncTarget) {
	now := s.now()
	for _, target := range targets {
		item, ok := round.items[target.agent]
		if !ok {
			round.items[target.agent] = &syncItem{
				agent:      target.agent,
				pointerUID: target.uid,
				targetHash: target.hash,
				state:      syncStatePending,
				attemptSeq: round.nextSeq(),
				updatedAt:  now,
			}
			continue
		}
		if item.targetHash == target.hash && item.pointerUID == target.uid {
			continue
		}
		item.pointerUID = target.uid
		item.targetHash = target.hash
		item.state = syncStatePending
		item.installed = assets.Installed{}
		item.installedHash = ""
		item.entry = core.AgentEntry{}
		item.entryReady = false
		item.published = nil
		item.skills = nil
		item.blockedReason = ""
		item.lastError = ""
		item.failCount = 0
		item.nextAttempt = time.Time{}
		item.attemptSeq = round.nextSeq()
		item.updatedAt = now
	}
}

// —— 资源准备 ——

// prepare 先于 Entry 构建取本轮资源快照（§2.3.4）：Provider 目录与 MCP 实例各自失败
// 隔离，整体失败只记报告、按缺失处理——依赖未准备资源的 Agent 本轮 `blocked`，绝不
// 回落到目录里可能残留的旧实例。
// prepare builds the round's read-only resource snapshot before any entry is built.
func (s *AssetSync) prepare(ctx context.Context, report *SyncReport) PreparedResources {
	catalog, err := s.prepareProviderCatalog(ctx)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("Provider 目录准备失败（本轮按缺失判定）: %v", err))
		catalog = CatalogReport{}
	}
	mcpReport, err := s.prepareMCPInstances(ctx)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("MCP 实例准备失败（本轮按缺失判定）: %v", err))
		mcpReport = MCPReport{}
	}
	report.Providers = catalog
	report.MCP = mcpReport
	// Tools 是**进程内注册池**的名字快照，不是 MCP 报告本身（§3.1.4 C14 / §2.3.8）：
	// 内置十二件取自装配期声明名集（启动时已与注册池逐名核对，`declaredToolNames`），
	// MCP 工具在本轮连接成功后才进入注册池、由 MCP 报告给出。只取 MCP 报告会把声明
	// `read_file` 一类内置工具的 Agent 在 bundle 路径整只判成未注册（规则 4 报
	// `tool not registered`，2026-11-10 定时任务实测复现该回归）。
	// Tools is the registry name snapshot — builtins from the assembly-time declaration
	// plus this round's connected MCP tools — never the MCP report alone.
	tools := append(slices.Clone(declaredToolNames), mcpReport.Tools...)
	slices.Sort(tools)
	prepared := PreparedResources{
		Providers:     slices.Clone(catalog.Providers),
		MCPConfigured: slices.Clone(mcpReport.Configured),
		MCPConnected:  slices.Clone(mcpReport.Connected),
		Tools:         tools,
	}
	// 发布本轮快照：钉住装载面构建 Entry 时读到的就是这一份（§3.1.4 C14）。
	// Publish the round's snapshot so pinned admission judges against the same set.
	s.prepared.store(prepared)
	return prepared
}

// prepareProviderCatalog 逐条构建已发布 Provider；未接线时返回空快照（Entry 判定失败关闭）。
// prepareProviderCatalog delegates to the wiring, which holds the resource gate.
func (s *AssetSync) prepareProviderCatalog(ctx context.Context) (CatalogReport, error) {
	if s.providers == nil {
		return CatalogReport{}, nil
	}
	return s.providers(ctx)
}

// prepareMCPInstances 只对已 applied 的连接做只读凭据解析与准备；未接线时返回空快照。
// prepareMCPInstances delegates to the wiring's applied-only preparation.
func (s *AssetSync) prepareMCPInstances(ctx context.Context) (MCPReport, error) {
	if s.mcp == nil {
		return MCPReport{}, nil
	}
	return s.mcp(ctx)
}

// —— 有界处理 ——

// process 按 `attemptSeq` 轮转取至多 `sync_batch` 条可尝试项：失败项在失败时已把自己
// 的 `attemptSeq` 推到队尾，所以这里只按序号升序取——**不**按名字前缀固定取前 N 条。
func (s *AssetSync) process(ctx context.Context, round *syncRound, prepared PreparedResources) {
	now := s.now()
	candidates := make([]*syncItem, 0, len(round.items))
	for _, item := range round.items {
		if item.state == syncStateReady {
			continue
		}
		if !item.nextAttempt.IsZero() && item.nextAttempt.After(now) {
			continue
		}
		candidates = append(candidates, item)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].attemptSeq != candidates[j].attemptSeq {
			return candidates[i].attemptSeq < candidates[j].attemptSeq
		}
		return candidates[i].agent < candidates[j].agent
	})
	for index, item := range candidates {
		if index >= s.batch {
			return
		}
		if err := ctx.Err(); err != nil {
			return
		}
		s.processItem(ctx, round, item, prepared)
	}
}

// processItem 推进一条：首装、重试、装成功但未注册走**同一条**代码路径；每一步失败都
// 落 `blocked` 并按 `attemptSeq` 退到队尾，**不**移出工作集。
// processItem advances one item; every failure lands in blocked without leaving the set.
func (s *AssetSync) processItem(ctx context.Context, round *syncRound, item *syncItem, prepared PreparedResources) {
	item.attemptSeq = round.nextSeq()
	item.updatedAt = s.now()
	item.lifecycle = s.lifecycleOf(ctx, item.agent)

	if item.state == syncStateBlocked {
		// 从**失败阶段**重跑：目录已按目标哈希自洽就只重做物化与 Entry。
		if item.installed.Dir != "" && item.installed.Manifest.BundleHash == item.targetHash {
			item.state = syncStateInstalled
		} else {
			item.state = syncStatePending
		}
	}
	switch item.state {
	case syncStatePending:
		if err := s.installItem(ctx, item); err != nil {
			s.blockItem(item, err)
			return
		}
		fallthrough
	case syncStateInstalled:
		if err := s.buildEntry(ctx, item, prepared); err != nil {
			s.blockItem(item, err)
			return
		}
	default:
		return
	}
	item.blockedReason = ""
	item.lastError = ""
	item.failCount = 0
	item.nextAttempt = time.Time{}
}

// installItem 把目标哈希装到本机资产根：先试 `Resolve`（崩溃恢复 / 已装复用），未命中
// 才走载荷解码 + 安装。`Resolve` 命中也要 `Verify`——目录名相同不等于内容自洽。
// installItem prefers the installed directory and falls back to the payload.
func (s *AssetSync) installItem(ctx context.Context, item *syncItem) error {
	dir, err := assets.Resolve(ctx, s.assetRoot, item.agent, item.targetHash)
	if err == nil {
		manifest, verifyErr := assets.Verify(ctx, dir)
		if verifyErr != nil {
			return &syncBlockError{reason: syncBlockedCorrupt, cause: verifyErr}
		}
		if manifest.BundleHash != item.targetHash {
			return &syncBlockError{reason: syncBlockedCorrupt, cause: fmt.Errorf(
				"已安装目录 %s 的清单哈希 %s 与发布指针 %s 不符", dir, manifest.BundleHash, item.targetHash)}
		}
		item.installed = assets.Installed{Manifest: manifest, Dir: dir}
		item.installedHash = manifest.BundleHash
		item.state = syncStateInstalled
		return nil
	}
	if !errors.Is(err, core.ErrAssetUnavailable) {
		return &syncBlockError{reason: syncBlockedCorrupt, cause: err}
	}

	payload, ok, readErr := s.distribution.ReadBundlePayload(ctx, item.targetHash)
	if readErr != nil {
		return &syncBlockError{reason: syncBlockedPayloadRead, cause: readErr}
	}
	if !ok {
		return &syncBlockError{reason: syncBlockedPayloadMissing, cause: fmt.Errorf(
			"载荷表缺少 bundle %s（发布指针存在但载荷缺失）", item.targetHash)}
	}
	staging, err := os.MkdirTemp("", "basalt-bundle-decode-")
	if err != nil {
		return &syncBlockError{reason: syncBlockedInstallFailed, cause: err}
	}
	defer func() { _ = os.RemoveAll(staging) }() // 解码失败会自清；成功路径由 Install 复制
	if err := assets.DecodeBundle(ctx, payload.Payload, staging, item.targetHash, s.maxBytes); err != nil {
		return &syncBlockError{reason: syncBlockedCorrupt, cause: err}
	}
	installed, err := assets.Install(ctx, staging, s.assetRoot)
	if err != nil {
		return &syncBlockError{reason: syncBlockedInstallFailed, cause: err}
	}
	item.installed = installed
	item.installedHash = installed.Manifest.BundleHash
	item.state = syncStateInstalled
	return nil
}

// buildEntry 每轮重做物化与 Entry 构建（§2.3.4）：`installed` 的条目**不得**因为
// "哈希没变"跳过——跳过会让"安装成功、注册失败"永久不恢复。
// 控制面没有 Entry 构建面（工作区才是它的权威来源，`AssetSyncDeps.Loader` 恒 nil）：
// 只做技能物化与已装读数，不建 Entry、不进 `ready`——它的注册集合由工作区装载与
// watcher 维护，**不得**被 bundle 快照替换（§2.3.4 的 control 分支）。
// buildEntry re-materializes skills and rebuilds the entry every round; control has no
// entry builder and therefore only materializes.
func (s *AssetSync) buildEntry(ctx context.Context, item *syncItem, prepared PreparedResources) error {
	published, skills, err := s.materializeSkills(ctx, item.installed)
	if err != nil {
		return &syncBlockError{reason: syncBlockedSkills, cause: err}
	}
	item.published = published
	item.skills = skills
	item.installedHash = item.installed.Manifest.BundleHash
	if s.loader == nil {
		return nil
	}
	entry, err := s.loader.EntryFromBundle(s.workspaceRoot, item.installed, prepared, published)
	if err != nil {
		return &syncBlockError{reason: syncBlockedEntryInvalid, cause: err}
	}
	item.entry = entry
	item.entryReady = true
	return nil
}

// blockItem 把失败落成 `blocked`：清掉本轮已失效的 Entry 产物，`failCount` 封顶，
// 退避后重试。安装结果（`installed`）保留——失败可能发生在物化或 Entry 阶段。
// blockItem records the failure; the installed directory survives.
func (s *AssetSync) blockItem(item *syncItem, err error) {
	item.state = syncStateBlocked
	item.entry = core.AgentEntry{}
	item.entryReady = false
	item.published = nil
	item.skills = nil
	item.lastError = err.Error()
	item.blockedReason = blockReason(err)
	item.failCount++
	if item.failCount > syncFailCountCap {
		item.failCount = syncFailCountCap
	}
	item.nextAttempt = s.now().Add(s.backoff(item.failCount))
	s.logger.Warn("asset sync item is not ready",
		"agent", item.agent, "hash", item.targetHash,
		"reason", item.blockedReason, "error", err)
}

// backoff = min(2^(failCount-1) × sync_interval, 5m)；`sync_interval == 0` 时无周期，
// 退避为零（下一轮就是启动期那一次，不会再发生）。
// backoff implements the card's capped exponential backoff.
func (s *AssetSync) backoff(failCount int) time.Duration {
	if failCount < 1 {
		failCount = 1
	}
	if s.interval <= 0 {
		return 0
	}
	shift := failCount - 1
	if shift > syncFailCountCap {
		shift = syncFailCountCap
	}
	delay := s.interval << shift
	if delay <= 0 || delay > syncBackoffCap {
		return syncBackoffCap
	}
	return delay
}

// lifecycleOf 读共享生命周期索引：无索引行时不猜状态（回退本地文件是调度门的职责）。
// lifecycleOf reads the shared lifecycle index; a missing row stays unknown.
func (s *AssetSync) lifecycleOf(ctx context.Context, agent string) string {
	record, ok, _, err := s.versions.ReadLifecycleIndex(ctx, agent)
	if err != nil || !ok {
		return syncLifecycleUnknown
	}
	return string(record.State)
}

// applyEntries 同轮、固定顺序替换两份集合（仅 worker）：先注册表后调度集合，任一步
// 失败都把这些条目退回 `installed`，下一轮重做——**不**留下"注册表有、调度集合无"。
// applyEntries replaces registry then scheduler in one round.
func (s *AssetSync) applyEntries(ctx context.Context, round *syncRound, report *SyncReport) {
	if s.control {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	names := make([]string, 0, len(round.items))
	entries := make([]core.AgentEntry, 0, len(round.items))
	for name, item := range round.items {
		if !item.entryReady {
			continue
		}
		if item.installed.Manifest.BundleHash != item.targetHash {
			continue
		}
		names = append(names, name)
		entries = append(entries, item.entry)
	}
	if len(entries) == 0 {
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	if err := s.registry.ReplaceAll(entries); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("注册表替换失败（条目退回 installed，下一轮重做）: %v", err))
		revertEntries(round, names)
		return
	}
	if err := s.scheduler.ReplaceEntries(entries); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("调度集合替换失败（条目退回 installed，下一轮重做）: %v", err))
		revertEntries(round, names)
		return
	}
	for _, name := range names {
		if item, ok := round.items[name]; ok {
			item.state = syncStateReady
		}
	}
}

// revertEntries 把本轮参与替换的条目退回 `installed`（`ready` 的也退），下一轮重新
// 物化与构建 Entry——两份集合必须同轮生效，不能靠"上一轮装过什么"。
// revertEntries returns the round's participants to installed.
func revertEntries(round *syncRound, names []string) {
	for _, name := range names {
		item, ok := round.items[name]
		if !ok {
			continue
		}
		item.state = syncStateInstalled
		item.entry = core.AgentEntry{}
		item.entryReady = false
	}
}

// repairOnce 是 control 专属的有界技能修复轮次（§2.3.7）；worker 的 Repair 为 nil。
// `fullScan` 与发现阶段用同一个判据（`fullScanDue`）：全量扫描轮由修复器先复位重试
// 状态，再按批次上限重试——blocked 项因此不会永久卡死（FR-25C-17）。
// repairOnce is the control-only bounded repair round; fullScan carries the same
// full-scan verdict the discovery phase used, so blocked state resets periodically.
func (s *AssetSync) repairOnce(ctx context.Context, round int, report *SyncReport) {
	if s.repair == nil {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}
	if err := s.repair(ctx, s.fullScanDue(round)); err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("技能修复轮次失败: %v", err))
	}
}

// —— 技能物化 ——

// materializeSkills 把 bundle 声明的技能物化到本机发布版本目录（§2.3.4），返回
// 「技能名 → 发布目录（工作区相对）」与已物化技能名（升序）。**每轮无条件执行**
// （含 `assets.Resolve` 复用分支）：安装成功但物化在崩溃前没跑完的目录否则会永久缺
// 技能。失败即该 Agent 未就绪，**不**回落到工作副本。
//
// 卡片 §3.1.4 的 `materializeBundleSkills`（只回 error 的包装）与这里的返回物化产物
// 是同一实现的两面；包装没有调用方（staticcheck U1000），故合并成一个函数，判定与
// 执行顺序逐条不变——记录见 run-state T024 行。
// 目录映射交给 `EntryFromBundle` 做注册判卷：声明了但没物化的技能名不在表里，
// `validateProfile` 规则 5 因此判缺失。
// materializeSkills materializes every declared skill and returns the products the entry
// builder needs; it never falls back to the work copy.
func (s *AssetSync) materializeSkills(ctx context.Context, installed assets.Installed) (map[string]string, []string, error) {
	published := make(map[string]string, len(installed.Manifest.Skills))
	names := make([]string, 0, len(installed.Manifest.Skills))
	for _, ref := range installed.Manifest.Skills {
		relDir, err := s.materializeSkill(ctx, installed, ref)
		if err != nil {
			return nil, nil, err
		}
		published[ref.Name] = relDir
		names = append(names, ref.Name)
	}
	sort.Strings(names)
	return published, names, nil
}

// resolveSkillMaterialization 是一个技能版本物化目标的**只读判定**：读版本行 → 校验
// 文件清单 → 投影路径（防双前缀）→ 包含检查，返回发布目录的相对路径与绝对路径。
// 同步物化（`materializeSkill`）与只读诊断（`assets status`）共用这一份规则——两处
// 各写一套迟早给出两种"能不能物化"的判定。
//
// `workspaceRoot` 是工作区根（`.basalt` 本身），`storage_path` 是**契约字面量**（带
// `.basalt/` 前缀）：直接 Join 会得到 `.basalt/.basalt/...`，投影与包含检查缺一不可。
// resolveSkillMaterialization is the read-only judgement shared by the sync and the
// read-only diagnostic, so both answer "can this skill be materialized" identically.
func resolveSkillMaterialization(ctx context.Context, distribution core.AssetDistributionPort,
	workspaceRoot string, ref core.SkillVersionRef,
) (relDir, dest string, err error) {
	version, ok, err := distribution.ReadSkillVersion(ctx, ref.VersionID)
	if err != nil {
		return "", "", fmt.Errorf("读取技能 %s 的发布版本 %d: %w", ref.Name, ref.VersionID, err)
	}
	if !ok {
		return "", "", fmt.Errorf("技能 %s 的发布版本 %d 不存在（bundle 声明了它，但版本行读不到）", ref.Name, ref.VersionID)
	}
	if version.Name != ref.Name {
		return "", "", fmt.Errorf("技能版本行 %d 属于 %q，与 bundle 声明的 %q 不符", ref.VersionID, version.Name, ref.Name)
	}
	if len(version.Files) == 0 {
		return "", "", fmt.Errorf("技能 %s 的发布版本 %d 文件清单为空", ref.Name, ref.VersionID)
	}
	if err := sameSkillFiles(version.Files, ref.Files); err != nil {
		return "", "", fmt.Errorf("技能 %s 的发布版本 %d 与 bundle 清单不符: %w", ref.Name, ref.VersionID, err)
	}
	if !hasSkillDefinition(ref.Files) {
		return "", "", fmt.Errorf("技能 %s 的发布版本 %d 不含 %s", ref.Name, ref.VersionID, syncSkillDefinitionFile)
	}
	relDir, err = workspace.PublishedVersionRelDir(version.StoragePath)
	if err != nil {
		return "", "", fmt.Errorf("技能 %s 的发布目录 %q: %w", ref.Name, version.StoragePath, err)
	}
	dest = filepath.Join(workspaceRoot, filepath.FromSlash(relDir))
	if !bundlePathInside(workspaceRoot, dest) {
		return "", "", fmt.Errorf("技能 %s 的发布目录 %q 落在工作区之外", ref.Name, relDir)
	}
	return relDir, dest, nil
}

// materializeSkill 物化一个技能版本：先走只读判定拿到目标目录，已存在则逐文件核对
// 复用，否则暂存复制并整体换入，最后冻结只读。
// materializeSkill pins one version by id; the recorded storage_path is the only source.
func (s *AssetSync) materializeSkill(ctx context.Context, installed assets.Installed, ref core.SkillVersionRef) (string, error) {
	relDir, dest, err := resolveSkillMaterialization(ctx, s.distribution, s.workspaceRoot, ref)
	if err != nil {
		return "", err
	}

	exists, err := dirExists(dest)
	if err != nil {
		return "", fmt.Errorf("检查技能 %s 的发布目录 %s: %w", ref.Name, relDir, err)
	}
	if exists {
		if err := verifyMaterializedSkill(dest, ref.Files); err != nil {
			return "", fmt.Errorf("技能 %s 的已物化目录 %s 与发布记录不符（已发布目录不可覆盖）: %w", ref.Name, relDir, err)
		}
		// 上次换入后、冻结前崩溃的窗口：复用分支同样要冻结。
		if err := workspace.FinalizeVersionDir(s.workspaceRoot, relDir); err != nil {
			return "", fmt.Errorf("冻结技能版本目录 %s 为只读: %w", relDir, err)
		}
		return relDir, nil
	}
	if err := s.materializeSkillDir(ctx, installed, ref, relDir, dest); err != nil {
		return "", err
	}
	return relDir, nil
}

// materializeSkillDir 从 bundle 的 `skills/<name>/<file>` 逐文件复制到暂存目录，边写边
// 核对 sha256 与大小，然后整体 rename 换入并冻结权限。
// materializeSkillDir copies bundle files into a staging directory and renames it in.
func (s *AssetSync) materializeSkillDir(ctx context.Context, installed assets.Installed,
	ref core.SkillVersionRef, relDir, dest string,
) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, syncMaterializedDirPerm); err != nil {
		return fmt.Errorf("创建技能版本父目录 %s: %w", parent, err)
	}
	staging, err := os.MkdirTemp(parent, ".staging-")
	if err != nil {
		return fmt.Errorf("创建技能版本暂存目录: %w", err)
	}
	staged := true
	defer func() {
		if staged {
			_ = os.RemoveAll(staging) // 失败路径清理半成品；成功路径已随 rename 消失
		}
	}()

	bundleRoot, err := os.OpenRoot(installed.Dir)
	if err != nil {
		return fmt.Errorf("打开已安装 bundle %s: %w", installed.Dir, err)
	}
	defer func() { _ = bundleRoot.Close() }()

	// 目标写入同样走 os.Root 根约束：暂存目录是本次新建的，写入的相对路径仍由清单的
	// file.Path 拼出，根约束把它挡在暂存目录内——这是真实防护，不只是消警（G304）。
	// The staging root scopes every write even though file.Path comes from a manifest.
	targetRoot, err := os.OpenRoot(staging)
	if err != nil {
		return fmt.Errorf("打开暂存目录 %s: %w", staging, err)
	}
	targetRootClosed := false
	defer func() {
		if !targetRootClosed {
			_ = targetRoot.Close()
		}
	}()

	for _, file := range ref.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		source := path.Join(syncBundleSkillsDir, ref.Name, file.Path)
		if err := copyVerifiedSkillFile(bundleRoot, source, targetRoot, file.Path, file); err != nil {
			return fmt.Errorf("物化技能 %s 的文件 %s: %w", ref.Name, file.Path, err)
		}
	}

	// Windows 上持有暂存目录的打开句柄时 Rename 会报共享冲突（"being used by another
	// process"），必须换入前先关根句柄；Unix 允许目录句柄下 rename，关掉也无副作用。
	// Close the staging root before the swap: Windows refuses to rename a directory
	// that this process still holds open.
	if err := targetRoot.Close(); err != nil {
		return fmt.Errorf("关闭暂存目录 %s: %w", staging, err)
	}
	targetRootClosed = true

	if err := os.Rename(staging, dest); err != nil {
		// 并发物化同一版本：对方先换入。按"已存在"重走核对，核对不过就失败。
		if exists, statErr := dirExists(dest); statErr == nil && exists {
			if verifyErr := verifyMaterializedSkill(dest, ref.Files); verifyErr == nil {
				staged = false
				return workspace.FinalizeVersionDir(s.workspaceRoot, relDir)
			}
		}
		return fmt.Errorf("换入技能版本目录 %s: %w", relDir, err)
	}
	staged = false
	if err := workspace.FinalizeVersionDir(s.workspaceRoot, relDir); err != nil {
		return fmt.Errorf("冻结技能版本目录 %s 为只读: %w", relDir, err)
	}
	return nil
}

// copyVerifiedSkillFile 复制一个技能文件并复算哈希：大小或 sha256 与发布记录不符即失败
// （bundle 与发布记录必须逐字相同，物化不是"尽力而为"）。`target` 是相对暂存目录的
// 清单路径，经 `targetRoot` 的根约束写入，不能越出暂存目录。
// copyVerifiedSkillFile copies one file and proves its digest while writing; the
// target write is scoped by the staging root.
func copyVerifiedSkillFile(bundleRoot *os.Root, source string, targetRoot *os.Root, target string, want core.FileHash) error {
	handle, err := bundleRoot.Open(filepath.FromSlash(source))
	if err != nil {
		return fmt.Errorf("打开 bundle 内的技能文件 %s: %w", source, err)
	}
	defer func() { _ = handle.Close() }()
	info, err := handle.Stat()
	if err != nil {
		return fmt.Errorf("读取 bundle 内技能文件 %s 的状态: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("bundle 内的技能文件 %s 不是普通文件", source)
	}
	relTarget := filepath.FromSlash(target)
	if err := targetRoot.MkdirAll(filepath.Dir(relTarget), syncMaterializedDirPerm); err != nil {
		return fmt.Errorf("创建物化目录 %s: %w", filepath.Dir(target), err)
	}
	out, err := targetRoot.OpenFile(relTarget, os.O_WRONLY|os.O_CREATE|os.O_EXCL, syncMaterializedFilePerm)
	if err != nil {
		return fmt.Errorf("创建物化文件 %s: %w", target, err)
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(out, hasher), handle)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("写入物化文件 %s: %w", target, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭物化文件 %s: %w", target, closeErr)
	}
	if written != want.Size {
		return fmt.Errorf("大小与发布记录不符（记录 %d，实际 %d）", want.Size, written)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != want.SHA256 {
		return fmt.Errorf("sha256 与发布记录不符（记录 %s，实际 %s）", want.SHA256, got)
	}
	return nil
}

// verifyMaterializedSkill 逐文件核对已物化目录：全符才复用，任何差异都是失败
// （已发布目录不可覆盖，也不就地修补）。
// verifyMaterializedSkill proves a materialized directory matches the published list.
func verifyMaterializedSkill(dest string, files []core.FileHash) error {
	base, err := os.OpenRoot(dest)
	if err != nil {
		return fmt.Errorf("打开已物化目录 %s: %w", dest, err)
	}
	defer func() { _ = base.Close() }()
	for _, file := range files {
		handle, err := base.Open(filepath.FromSlash(file.Path))
		if err != nil {
			return fmt.Errorf("缺少文件 %s: %w", file.Path, err)
		}
		info, statErr := handle.Stat()
		if statErr != nil {
			_ = handle.Close()
			return fmt.Errorf("读取文件 %s 的状态: %w", file.Path, statErr)
		}
		if !info.Mode().IsRegular() {
			_ = handle.Close()
			return fmt.Errorf("文件 %s 不是普通文件", file.Path)
		}
		hasher := sha256.New()
		size, copyErr := io.Copy(hasher, handle)
		closeErr := handle.Close()
		if copyErr != nil {
			return fmt.Errorf("读取文件 %s: %w", file.Path, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭文件 %s: %w", file.Path, closeErr)
		}
		if size != file.Size {
			return fmt.Errorf("文件 %s 的大小不符（记录 %d，实际 %d）", file.Path, file.Size, size)
		}
		if got := hex.EncodeToString(hasher.Sum(nil)); got != file.SHA256 {
			return fmt.Errorf("文件 %s 的 sha256 不符（记录 %s，实际 %s）", file.Path, file.SHA256, got)
		}
	}
	return nil
}

// hasSkillDefinition 判定文件清单是否包含技能定义文件。
// hasSkillDefinition reports whether the list carries SKILL.md.
func hasSkillDefinition(files []core.FileHash) bool {
	return slices.ContainsFunc(files, func(file core.FileHash) bool {
		return file.Path == syncSkillDefinitionFile
	})
}

// sameSkillFiles 按路径比对两份文件清单（顺序不参与）：bundle 清单与版本行必须逐字
// 相同，否则物化的内容与发布记录脱钩。
// sameSkillFiles compares two file lists as path-keyed sets.
func sameSkillFiles(versionFiles, bundleFiles []core.FileHash) error {
	if len(versionFiles) != len(bundleFiles) {
		return fmt.Errorf("文件数不符（版本行 %d，bundle %d）", len(versionFiles), len(bundleFiles))
	}
	index := make(map[string]core.FileHash, len(versionFiles))
	for _, file := range versionFiles {
		index[file.Path] = file
	}
	for _, file := range bundleFiles {
		recorded, ok := index[file.Path]
		if !ok {
			return fmt.Errorf("版本行缺少文件 %s", file.Path)
		}
		if recorded.SHA256 != file.SHA256 || recorded.Size != file.Size {
			return fmt.Errorf("文件 %s 的哈希或大小不符", file.Path)
		}
	}
	return nil
}

// dirExists 判定路径是否存在且是目录；不存在不算错误。
// dirExists reports whether the path is an existing directory.
func dirExists(target string) (bool, error) {
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s 已存在但不是目录", target)
	}
	return true, nil
}
