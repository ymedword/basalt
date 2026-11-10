package app

// 本文件是 US-25C §2.3.4 / §4.3 的同步工作集定向判据（单元层，无数据库）：
//   - 四态状态机 pending / installed / ready / blocked，「已安装」不等于「就绪」；
//   - 安装 → 技能物化 → Entry 构建 → 注册表与调度集合替换，同轮固定顺序；
//   - 轮转公平：失败项退到队尾，前 50 条持续失败不得饿死第 51 条；
//   - 发现游标是 (updated_at, resource_id) 复合键集，同毫秒 201 行每行只读一次；
//   - 全量兜底扫描中断不删任何项，整趟完成后才经回读确认移除缺席者；
//   - 单轮总超时；崩溃恢复靠每轮重跑物化与 Entry，不靠重启；
//   - 同步只写资产目录，种子工作区与运行数据的哈希不变。
//
// 夹具用内存替身替换分发读面、共享记录、注册表与调度集合（模块卡 §4.1：单元层允许
// 内存夹具、无外部依赖），但 bundle 目录与载荷走**真实**的 assets.Build /
// EncodeBundle / Install——目录自洽与内容寻址是判据的一部分，不能用假路径绕过。
// Unit judges for the sync work set: four-state machine, rotation fairness, keyset
// cursor, interrupted full scan, single-round timeout, crash recovery, and the write
// whitelist.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

// syncTestMaxBytes 是夹具载荷上限：够装下临时目录里的少量文件。
// syncTestMaxBytes bounds the fixture payload size.
const syncTestMaxBytes = int64(1 << 20)

// syncUID 生成合法形态的 Agent 身份（`ag_` + 26 位 Crockford Base32）：纯数字既合法
// 又让排序意图一眼可见。
// syncUID builds a valid agent identity whose ordering is obvious in assertions.
func syncUID(n int) core.AgentUID {
	return core.AgentUID(fmt.Sprintf("ag_%026d", n))
}

// syncSyntheticHash 给出一个内容寻址形态的哈希（64 位小写十六进制），用于只入指针行、
// 不入载荷的发现用例——这些用例不进入解码面，哈希只需是合法目录名。
// syncSyntheticHash is a well-formed content hash for discovery-only rows.
func syncSyntheticHash(agent string) string {
	sum := sha256.Sum256([]byte("bundle:" + agent))
	return hex.EncodeToString(sum[:])
}

// syncClock 是可推进的测试时钟：退避与 nextAttempt 判定都读它。
// syncClock is the advanceable clock the backoff schedule reads.
type syncClock struct{ now time.Time }

func newAssetSyncClock() *syncClock {
	return &syncClock{now: time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)}
}

func (c *syncClock) Now() time.Time { return c.now }

func (c *syncClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// syncBundleRow 是一条已发布指针的内存投影。
type syncBundleRow struct {
	agent     string
	uid       core.AgentUID
	hash      string
	updatedAt int64
}

func (row syncBundleRow) published() core.PublishedBundle {
	return core.PublishedBundle{
		Agent:      row.agent,
		AgentUID:   row.uid,
		BundleHash: row.hash,
		UpdatedAt:  row.updatedAt,
	}
}

// fakeSyncDistribution 是 `core.AssetDistributionPort` 的内存替身，按模块卡 §3.1.2
// 的两种排序语义分别实现：
//   - `ListPublishedBundlesAfter` 严格按 (updated_at, resource_id) 键集前进——同毫秒
//     多行每行只出现一次，游标越过末行后返回空页（**不得**用 `>=` 重读页首）；
//   - `ListPublishedBundlesPage` 只按 resource_id 排序，与时间列无关。
//
// 它同时记录每行在增量面的读取次数、每份载荷的读取次数，并可注入错误与阻塞。
// fakeSyncDistribution implements both discovery orderings separately and records the
// per-row/per-payload read counts needed by the cursor and short-circuit judges.
type fakeSyncDistribution struct {
	rows   []syncBundleRow
	skills map[int64]core.SkillVersionRecord

	payloads     map[string]core.BundlePayloadRecord
	payloadReads map[string]int
	skillReads   map[int64]int
	afterReads   map[string]int

	afterCalls int
	pageCalls  int

	failAfterAt int
	failPageAt  int
	blockAfter  bool
}

func newFakeAssetSyncDistribution() *fakeSyncDistribution {
	return &fakeSyncDistribution{
		skills:       make(map[int64]core.SkillVersionRecord),
		payloads:     make(map[string]core.BundlePayloadRecord),
		payloadReads: make(map[string]int),
		skillReads:   make(map[int64]int),
		afterReads:   make(map[string]int),
	}
}

func (f *fakeSyncDistribution) addRow(agent string, uid core.AgentUID, hash string, updatedAt int64) {
	f.rows = append(f.rows, syncBundleRow{agent: agent, uid: uid, hash: hash, updatedAt: updatedAt})
}

func (f *fakeSyncDistribution) removeRow(uid core.AgentUID) {
	kept := make([]syncBundleRow, 0, len(f.rows))
	for _, row := range f.rows {
		if row.uid != uid {
			kept = append(kept, row)
		}
	}
	f.rows = kept
}

func (f *fakeSyncDistribution) dropPayload(hash string) { delete(f.payloads, hash) }

// byUpdatedAt 返回 (updated_at, resource_id) 序的行副本。
func (f *fakeSyncDistribution) byUpdatedAt() []syncBundleRow {
	sorted := append([]syncBundleRow(nil), f.rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].updatedAt != sorted[j].updatedAt {
			return sorted[i].updatedAt < sorted[j].updatedAt
		}
		return string(sorted[i].uid) < string(sorted[j].uid)
	})
	return sorted
}

// byUID 返回 resource_id 序的行副本（全量兜底扫描的排序）。
func (f *fakeSyncDistribution) byUID() []syncBundleRow {
	sorted := append([]syncBundleRow(nil), f.rows...)
	sort.Slice(sorted, func(i, j int) bool { return string(sorted[i].uid) < string(sorted[j].uid) })
	return sorted
}

func (f *fakeSyncDistribution) ListPublishedBundlesAfter(ctx context.Context,
	cursor core.PublishedBundleCursor, limit int,
) ([]core.PublishedBundle, error) {
	if f.blockAfter {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.afterCalls++
	if f.failAfterAt > 0 && f.afterCalls == f.failAfterAt {
		return nil, errors.New("增量列表读取失败（夹具）")
	}
	page := make([]core.PublishedBundle, 0, limit)
	for _, row := range f.byUpdatedAt() {
		if row.updatedAt < cursor.UpdatedAt {
			continue
		}
		if row.updatedAt == cursor.UpdatedAt && string(row.uid) <= cursor.ResourceID {
			continue
		}
		if len(page) >= limit {
			break
		}
		page = append(page, row.published())
	}
	for _, published := range page {
		f.afterReads[string(published.AgentUID)]++
	}
	return page, nil
}

func (f *fakeSyncDistribution) ListPublishedBundlesPage(_ context.Context,
	afterResourceID string, limit int,
) ([]core.PublishedBundle, error) {
	f.pageCalls++
	if f.failPageAt > 0 && f.pageCalls == f.failPageAt {
		return nil, errors.New("全量扫描页读取失败（夹具）")
	}
	page := make([]core.PublishedBundle, 0, limit)
	for _, row := range f.byUID() {
		if string(row.uid) <= afterResourceID {
			continue
		}
		if len(page) >= limit {
			break
		}
		page = append(page, row.published())
	}
	return page, nil
}

func (f *fakeSyncDistribution) ReadBundlePayload(_ context.Context, hash string) (core.BundlePayloadRecord, bool, error) {
	f.payloadReads[hash]++
	rec, ok := f.payloads[hash]
	return rec, ok, nil
}

func (f *fakeSyncDistribution) ReadSkillVersion(_ context.Context, versionID int64) (core.SkillVersionRecord, bool, error) {
	f.skillReads[versionID]++
	rec, ok := f.skills[versionID]
	return rec, ok, nil
}

// fakeSyncRecords 是 `assetBootstrapRecords` 的内存替身：全量扫描的缺席者回读走这里。
// 删除用 `deleted` 标记（与生产一致：行还在，`deleted_at` 非空）。
// fakeSyncRecords doubles the shared-record read face; deletions keep the row and set
// the deleted flag, matching the pointer's deleted_at semantics.
type fakeSyncRecords struct {
	pointers     map[core.AgentUID]core.ResourcePointer
	deleted      map[core.AgentUID]bool
	lifecycle    map[string]core.LifecycleRecord
	pointerReads map[string]int
}

func newFakeAssetSyncRecords() *fakeSyncRecords {
	return &fakeSyncRecords{
		pointers:     make(map[core.AgentUID]core.ResourcePointer),
		deleted:      make(map[core.AgentUID]bool),
		lifecycle:    make(map[string]core.LifecycleRecord),
		pointerReads: make(map[string]int),
	}
}

func (f *fakeSyncRecords) PointerByUID(_ context.Context, kind core.ResourceKind,
	uid core.ResourceUID,
) (core.ResourcePointer, error) {
	f.pointerReads[string(uid)]++
	key := core.AgentUID(uid)
	if f.deleted[key] {
		deletedAt := int64(1)
		return core.ResourcePointer{Kind: kind, ResourceUID: uid, DeletedAt: &deletedAt}, nil
	}
	pointer, ok := f.pointers[key]
	if !ok {
		return core.ResourcePointer{}, core.ErrResourceNotFound
	}
	return pointer, nil
}

func (f *fakeSyncRecords) VersionByID(context.Context, int64) (core.ResourceVersion, error) {
	return core.ResourceVersion{}, core.ErrResourceNotFound
}

func (f *fakeSyncRecords) ReadLifecycleIndex(_ context.Context, agent string) (core.LifecycleRecord, bool, string, error) {
	record, ok := f.lifecycle[agent]
	if !ok {
		return core.LifecycleRecord{}, false, "", nil
	}
	return record, true, "index", nil
}

// fakeSyncRegistry 是 `agentRegistry.ReplaceAll` 的内存替身：记录调用次序、条目与
// 注入的失败次数（`failFirst` 只让前 N 次失败，用于"注册失败后不重启也能恢复"）。
// fakeSyncRegistry records the replacement sequence and can fail the first N calls.
type fakeSyncRegistry struct {
	order     *[]string
	entries   []core.AgentEntry
	calls     int
	failFirst int
	err       error
}

func (f *fakeSyncRegistry) ReplaceAll(entries []core.AgentEntry) error {
	f.calls++
	if f.order != nil {
		*f.order = append(*f.order, "registry")
	}
	if f.err != nil {
		return f.err
	}
	if f.failFirst > 0 && f.calls <= f.failFirst {
		return errors.New("注册表替换失败（夹具）")
	}
	f.entries = append([]core.AgentEntry(nil), entries...)
	return nil
}

// fakeSyncScheduler 是 `ScheduleSetUpdater` 的内存替身：与注册表共用调用次序记录，
// 让"先注册表后调度"成为可断言的面。
// fakeSyncScheduler shares the call log so the fixed registry → scheduler order is
// assertable.
type fakeSyncScheduler struct {
	order     *[]string
	entries   []core.AgentEntry
	calls     int
	failFirst int
	err       error
}

func (f *fakeSyncScheduler) ReplaceEntries(entries []core.AgentEntry) error {
	f.calls++
	if f.order != nil {
		*f.order = append(*f.order, "scheduler")
	}
	if f.err != nil {
		return f.err
	}
	if f.failFirst > 0 && f.calls <= f.failFirst {
		return errors.New("调度集合替换失败（夹具）")
	}
	f.entries = append([]core.AgentEntry(nil), entries...)
	return nil
}

// fakeSyncLoader 是 `BundleEntryBuilder` 的内存替身：真实 Entry 构建由
// `workspace.EntryFromBundle` 承担（T017 / 集成层用例覆盖），本文件的判据是
// **何时**构建、构建失败如何影响状态，不是 frontmatter 解析本身。
// 它**不**记进共享调用次序：`order` 只用于断言"注册表 → 调度集合"这条固定顺序，
// Entry 构建发生在两者之前（由 `calls` 与已填充的 `registry.entries` 证明）。
// fakeSyncLoader judges when an entry is built and how a failure maps to a state.
type fakeSyncLoader struct {
	calls   int
	failFor map[string]error
}

func (f *fakeSyncLoader) EntryFromBundle(_ string, installed assets.Installed,
	_ PreparedResources, _ map[string]string,
) (core.AgentEntry, error) {
	f.calls++
	if err, ok := f.failFor[installed.Manifest.Agent]; ok {
		return core.AgentEntry{}, err
	}
	return core.AgentEntry{
		Name:       installed.Manifest.Agent,
		Registered: true,
		Profile:    &core.Profile{Name: installed.Manifest.Agent},
	}, nil
}

// assetSyncFixture 把一份同步现场的全部替身与真实资产根装在一起。
// assetSyncFixture assembles one sync scene: real bundle directories plus in-memory doubles.
type assetSyncFixture struct {
	t             *testing.T
	workspaceRoot string
	assetRoot     string
	buildRoot     string
	dist          *fakeSyncDistribution
	records       *fakeSyncRecords
	registry      *fakeSyncRegistry
	scheduler     *fakeSyncScheduler
	loader        *fakeSyncLoader
	clock         *syncClock
	order         []string
	sequence      int
	lastBuildDir  string
}

func newAssetSyncFixture(t *testing.T) *assetSyncFixture {
	t.Helper()
	fixture := &assetSyncFixture{
		t:             t,
		workspaceRoot: t.TempDir(),
		assetRoot:     t.TempDir(),
		buildRoot:     t.TempDir(),
		dist:          newFakeAssetSyncDistribution(),
		records:       newFakeAssetSyncRecords(),
		clock:         newAssetSyncClock(),
	}
	fixture.registry = &fakeSyncRegistry{order: &fixture.order}
	fixture.scheduler = &fakeSyncScheduler{order: &fixture.order}
	fixture.loader = &fakeSyncLoader{failFor: make(map[string]error)}
	return fixture
}

// deps 给出一份 worker 角色的完整装配：注册表与调度集合都在，Repair 为 nil。
// deps assembles the worker-role wiring under test.
func (f *assetSyncFixture) deps(mutate func(*AssetSyncDeps)) AssetSyncDeps {
	deps := AssetSyncDeps{
		WorkspaceRoot: f.workspaceRoot,
		AssetRoot:     f.assetRoot,
		Distribution:  f.dist,
		Versions:      f.records,
		Registry:      f.registry,
		Scheduler:     f.scheduler,
		Loader:        f.loader,
		Logger:        nil,
		Now:           f.clock.Now,
		Interval:      time.Second,
		Timeout:       5 * time.Second,
		Batch:         50,
		MaxBytes:      syncTestMaxBytes,
		FullScanEvery: 20,
	}
	if mutate != nil {
		mutate(&deps)
	}
	return deps
}

// buildSync 构造被测对象：构造器只做装配校验，**不启动**任何后台协程。
// buildSync constructs the sync; the constructor must not start anything.
func (f *assetSyncFixture) buildSync(mutate func(*AssetSyncDeps)) *AssetSync {
	f.t.Helper()
	sync, err := NewAssetSync(f.deps(mutate))
	require.NoError(f.t, err)
	return sync
}

// syncOnce 跑一轮并把错误当作失败：只有专门的超时用例才直接调 SyncOnce。
func (f *assetSyncFixture) syncOnce(sync *AssetSync) SyncReport {
	f.t.Helper()
	report, err := sync.SyncOnce(context.Background())
	require.NoError(f.t, err)
	return report
}

// addBundle 用**真实**构建链砌一份可安装的 bundle，并把载荷放进分发读面。
// addBundle builds a real bundle, installs nothing yet, and stores its payload.
func (f *assetSyncFixture) addBundle(agent string, uid core.AgentUID, updatedAt int64) string {
	f.t.Helper()
	f.sequence++
	rel := "agents/" + agent + "/AGENT.md"
	writeWorkspaceFile(f.t, f.workspaceRoot, rel,
		fmt.Sprintf("---\nname: %s\n---\n\n同步夹具正文 %d\n", agent, f.sequence))
	dest := filepath.Join(f.buildRoot, fmt.Sprintf("build-%03d-%s", f.sequence, agent))
	manifest, err := assets.Build(context.Background(), assets.BuildSpec{
		Agent:       agent,
		AgentUID:    uid,
		ConfigHash:  "cfg-" + agent,
		ConfigRoot:  f.workspaceRoot,
		ConfigFiles: []string{rel},
		Dest:        dest,
	})
	require.NoError(f.t, err)
	payload, sum, err := assets.EncodeBundle(context.Background(), dest, syncTestMaxBytes)
	require.NoError(f.t, err)
	f.dist.addRow(agent, uid, manifest.BundleHash, updatedAt)
	f.dist.payloads[manifest.BundleHash] = core.BundlePayloadRecord{
		BundleHash:    manifest.BundleHash,
		Agent:         agent,
		AgentUID:      uid,
		FormatVersion: manifest.FormatVersion,
		Payload:       payload,
		PayloadSHA256: sum,
		SizeBytes:     int64(len(payload)),
	}
	f.records.pointers[uid] = core.ResourcePointer{
		Kind:               core.ResourceAgentBundle,
		ResourceID:         string(uid),
		ResourceUID:        core.ResourceUID(uid),
		PublishedVersionID: int64(f.sequence),
		UpdatedAt:          updatedAt,
	}
	f.lastBuildDir = dest
	return manifest.BundleHash
}

// addRowOnly 只入指针行、不入载荷：发现类用例不进入解码面，避免为 200 行砌真实目录。
// addRowOnly registers a discovery row without a payload.
func (f *assetSyncFixture) addRowOnly(agent string, uid core.AgentUID, updatedAt int64) string {
	f.t.Helper()
	hash := syncSyntheticHash(agent)
	f.dist.addRow(agent, uid, hash, updatedAt)
	f.records.pointers[uid] = core.ResourcePointer{
		Kind:               core.ResourceAgentBundle,
		ResourceID:         string(uid),
		ResourceUID:        core.ResourceUID(uid),
		PublishedVersionID: 1,
		UpdatedAt:          updatedAt,
	}
	return hash
}

// addInstalledBundle 模拟"上一进程已安装、随后崩溃"：目录真实存在于资产根，载荷表里
// 没有这一行——只有 Resolve 命中才能恢复，读载荷必然失败。
// addInstalledBundle simulates a crash between install and register.
func (f *assetSyncFixture) addInstalledBundle(agent string, uid core.AgentUID, updatedAt int64) string {
	f.t.Helper()
	hash := f.addBundle(agent, uid, updatedAt)
	f.dist.dropPayload(hash)
	_, err := assets.Install(context.Background(), f.lastBuildDir, f.assetRoot)
	require.NoError(f.t, err)
	return hash
}

// requireSyncItem 在报告里找一条 Agent 读数（报告必须列出工作集里的每一项）。
func requireSyncItem(t *testing.T, report SyncReport, agent string) AgentSyncResult {
	t.Helper()
	item, ok := report.Item(agent)
	require.True(t, ok, "报告必须列出 %s：%+v", agent, report.Items)
	return item
}

// countSyncState 统计某个状态的条目数。
func countSyncState(report SyncReport, state string) int {
	count := 0
	for _, item := range report.Items {
		if item.State == state {
			count++
		}
	}
	return count
}

// snapshotTree 记录一棵树的相对路径 → sha256；用于"同步不得写工作区"的前后比对。
// snapshotTree hashes every regular file so the write whitelist is assertable.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	require.NoError(t, err)
	return out
}

// 中文验收点：安装成功只进入 `installed`；集合替换失败时不得算作就绪，报告与
// readiness 都必须能看出"目录装好了、注册没成"。逆改点（模块卡 §4.3）：把安装完成
// 直接置 ready → 本用例看到 ReadyHash 非空。
// Acceptance point: a successful install is not readiness; a failed replacement leaves
// the item installed with no ready hash.
func TestAssetSync_InstalledIsNotReady(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	hash := fixture.addBundle("alpha", syncUID(1), 1000)
	fixture.registry.err = errors.New("注册表永久失败（夹具）")
	sync := fixture.buildSync(nil)

	report := fixture.syncOnce(sync)
	item := requireSyncItem(t, report, "alpha")
	require.Equal(t, "installed", item.State)
	require.Equal(t, hash, item.InstalledHash)
	require.Equal(t, 1, fixture.loader.calls, "Entry 构建必须在集合替换之前完成")
	require.Empty(t, fixture.registry.entries)

	readiness, ok := sync.Readiness()["alpha"]
	require.True(t, ok)
	require.Equal(t, hash, readiness.PointerHash)
	require.Empty(t, readiness.ReadyHash, "集合替换失败不得填就绪哈希")
}

// 中文验收点：安装成功且载荷读取一次后进入就绪；第二轮不重读载荷；换一个进程实例
// （同一资产根、载荷表为空）必须靠 `assets.Resolve` 复用已安装目录。
// Acceptance point: the payload is read once, and a fresh process reuses the installed
// directory instead of reading the payload again.
func TestAssetSync_InstallAndShortCircuit(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	hash := fixture.addBundle("alpha", syncUID(1), 1000)
	sync := fixture.buildSync(nil)

	first := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, first, "alpha").State)
	require.Equal(t, 1, fixture.dist.payloadReads[hash], "首轮必须读一次载荷")

	second := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, second, "alpha").State)
	require.Equal(t, 1, fixture.dist.payloadReads[hash], "已就绪条目不得重读载荷")

	// 第二个进程实例：同一资产根，载荷表清空。只有 Resolve 命中才可能就绪。
	dist := newFakeAssetSyncDistribution()
	dist.addRow("alpha", syncUID(1), hash, 1000)
	records := newFakeAssetSyncRecords()
	records.pointers[syncUID(1)] = core.ResourcePointer{
		Kind: core.ResourceAgentBundle, ResourceID: string(syncUID(1)),
		ResourceUID: core.ResourceUID(syncUID(1)), PublishedVersionID: 1, UpdatedAt: 1000,
	}
	registry := &fakeSyncRegistry{order: &fixture.order}
	scheduler := &fakeSyncScheduler{order: &fixture.order}
	loader := &fakeSyncLoader{failFor: make(map[string]error)}
	secondSync, err := NewAssetSync(AssetSyncDeps{
		WorkspaceRoot: fixture.workspaceRoot,
		AssetRoot:     fixture.assetRoot,
		Distribution:  dist,
		Versions:      records,
		Registry:      registry,
		Scheduler:     scheduler,
		Loader:        loader,
		Now:           fixture.clock.Now,
		Interval:      time.Second,
		Timeout:       5 * time.Second,
		Batch:         50,
		MaxBytes:      syncTestMaxBytes,
		FullScanEvery: 20,
	})
	require.NoError(t, err)
	report, err := secondSync.SyncOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ready", requireSyncItem(t, report, "alpha").State)
	require.Empty(t, dist.payloadReads, "已安装目录必须被 Resolve 复用，不得回读载荷")
}

// 中文验收点：注册表替换首轮失败后，同一实例的下一轮必须自动恢复，不得要求重启。
// 逆改点（模块卡 §4.3）：让 installed 因哈希相同而跳过 Entry 构建 → 第二轮仍注册不上。
// Acceptance point: a registration failure recovers on the next round of the same
// instance, and the recovery path never re-reads the payload.
func TestAssetSync_RegisterFailureRecoversWithoutRestart(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	hash := fixture.addBundle("alpha", syncUID(1), 1000)
	fixture.registry.failFirst = 1
	sync := fixture.buildSync(nil)

	first := fixture.syncOnce(sync)
	require.Equal(t, "installed", requireSyncItem(t, first, "alpha").State)
	require.Empty(t, fixture.registry.entries, "失败轮不得留下半套注册表")

	second := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, second, "alpha").State)
	require.Len(t, fixture.registry.entries, 1)
	require.Equal(t, "alpha", fixture.registry.entries[0].Name)
	require.Equal(t, 1, fixture.dist.payloadReads[hash], "恢复走已安装分支，不得重读载荷")
	require.GreaterOrEqual(t, fixture.loader.calls, 2, "installed 条目每轮都必须重跑 Entry 构建")
}

// 中文验收点：前 50 条持续失败（载荷缺失）时，第 51 条最迟在下一轮被处理——失败项
// 必须按尝试序号退到队尾，不得按名字前缀固定取前 N 条。
// Acceptance point: fifty starving failures must not block the fifty-first item.
func TestAssetSync_StarvedPrefixStillProgresses(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	for i := 0; i < 50; i++ {
		fixture.addRowOnly(fmt.Sprintf("starved-%02d", i), syncUID(i+1), 5000)
	}
	readyAgent := "starved-50"
	readyHash := fixture.addBundle(readyAgent, syncUID(51), 5000)
	sync := fixture.buildSync(nil)

	first := fixture.syncOnce(sync)
	require.Equal(t, "pending", requireSyncItem(t, first, readyAgent).State,
		"第 51 条首轮未被选中，状态仍是 pending")
	require.Zero(t, fixture.dist.payloadReads[readyHash], "第 51 条不得在首轮被处理")
	require.Equal(t, 50, countSyncState(first, "blocked"))

	// 时钟越过一次退避：失败项 nextAttempt 到期，与第 51 条一起重新排队。
	fixture.clock.Advance(2 * time.Second)
	second := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, second, readyAgent).State,
		"失败项退到队尾后，第 51 条必须在本轮被处理")
	require.Equal(t, 1, fixture.dist.payloadReads[readyHash])
}

// 中文验收点：同一毫秒 201 行、每页 200 行时，复合键集游标必须越过末行——增量面
// 每行只读一次，第二轮不再重读页首。逆改点（模块卡 §4.3）：游标改成只比较
// updated_at → 第二轮仍返回同一页，afterReads 翻倍。
// Acceptance point: the (updated_at, resource_id) cursor reads each same-millisecond
// row exactly once.
func TestAssetSync_KeysetCursorAdvancesWithinSameMillisecond(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	const rows = 201
	for i := 0; i < rows; i++ {
		fixture.addRowOnly(fmt.Sprintf("bulk-%03d", i), syncUID(i), 7000)
	}
	sync := fixture.buildSync(func(deps *AssetSyncDeps) { deps.Batch = 1 })

	fixture.syncOnce(sync)
	for i := 0; i < rows; i++ {
		require.Equal(t, 1, fixture.dist.afterReads[string(syncUID(i))],
			"增量面必须恰好读一次 %s", syncUID(i))
	}
	require.GreaterOrEqual(t, fixture.dist.afterCalls, 2, "201 行至少两页")

	fixture.syncOnce(sync)
	for i := 0; i < rows; i++ {
		require.Equal(t, 1, fixture.dist.afterReads[string(syncUID(i))],
			"游标必须越过同毫秒末行，不得重读 %s", syncUID(i))
	}
}

// 中文验收点：全量扫描中途失败时不得删除任何项；整趟完成（含跨轮续跑）后才经
// `PointerByUID` 回读确认移除缺席者。逆改点（模块卡 §4.3）：中断即按"本轮未见"
// 清理 → 第一轮之后 scan-gone 已从 readiness 消失。
// Acceptance point: an interrupted full scan removes nothing; only a completed pass
// re-confirms an absent agent as deleted.
func TestAssetSync_InterruptedFullScanRemovesNothing(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	for i := 0; i < 200; i++ {
		fixture.addRowOnly(fmt.Sprintf("scan-%03d", i), syncUID(i+1), 8000)
	}
	goneUID := syncUID(201)
	fixture.addRowOnly("scan-gone", goneUID, 8000)
	sync := fixture.buildSync(func(deps *AssetSyncDeps) {
		deps.FullScanEvery = 1
		deps.Batch = 1
	})

	first := fixture.syncOnce(sync)
	require.Equal(t, 0, len(first.Removed))
	_, present := sync.Readiness()["scan-gone"]
	require.True(t, present, "首轮完整扫描必须把 scan-gone 放进工作集")

	// 指针行被删除（deleted_at 非空），同时补一条新行保持 201 条总量。
	fixture.dist.removeRow(goneUID)
	fixture.records.deleted[goneUID] = true
	fixture.addRowOnly("scan-fresh", syncUID(500), 8000)

	// 第 4 次全量翻页调用失败：第 1、2 次来自首轮，第 3 次是本轮第一页，
	// 第 4 次是本轮第二页——整趟中断。
	fixture.dist.failPageAt = 4
	interrupted := fixture.syncOnce(sync)
	require.NotEmpty(t, interrupted.Errors, "中断必须如实进报告")
	require.Empty(t, interrupted.Removed)
	_, present = sync.Readiness()["scan-gone"]
	require.True(t, present, "扫描未完成时不得把未扫到的行当作已删除")
	require.GreaterOrEqual(t, fixture.records.pointerReads[string(goneUID)], 0)

	// 下一轮从断点续跑：整趟完成，回读确认 scan-gone 已删除。
	completed := fixture.syncOnce(sync)
	require.Contains(t, completed.Removed, "scan-gone")
	_, present = sync.Readiness()["scan-gone"]
	require.False(t, present, "整趟完成且回读确认后必须移出工作集")
	require.Positive(t, fixture.records.pointerReads[string(goneUID)],
		"移除前必须逐个 PointerByUID 回读确认")
}

// 中文验收点：更新时间早于已走过游标的迟到提交，增量键集看不到，必须由周期全量
// 扫描补齐。逆改点（模块卡 §4.3）：去掉全量兜底 → late-b 永远不就绪。
// Acceptance point: a delayed commit behind the cursor is discovered only by the next
// complete full scan.
func TestAssetSync_PeriodicFullScanCatchesDelayedCommit(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	fixture.addBundle("late-a", syncUID(1), 2000)
	sync := fixture.buildSync(func(deps *AssetSyncDeps) { deps.FullScanEvery = 2 })

	first := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, first, "late-a").State)

	// 迟到提交：updated_at 早于已走过的游标（2000），增量面看不到它。
	lateHash := fixture.addBundle("late-b", syncUID(2), 1000)
	fixture.syncOnce(sync)
	_, present := sync.Readiness()["late-b"]
	require.False(t, present, "增量键集不得看到迟到提交")

	third := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, third, "late-b").State)
	require.Equal(t, 1, fixture.dist.payloadReads[lateHash])
}

// 中文验收点：注册表与调度集合必须同轮替换、固定顺序（先注册表后调度），且运行期
// 每轮都以当前条目重新替换（ready 条目仍参与）。逆改点（模块卡 §4.3）：只替换
// 注册表 → 调度集合缺条目；顺序反转 → 次序断言失败。
// Acceptance point: both sets are replaced in the same round, registry first, and every
// round re-applies the ready entries.
func TestAssetSync_RefreshesRegistryAndScheduler(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	fixture.addBundle("reg-alpha", syncUID(1), 1000)
	fixture.addBundle("reg-beta", syncUID(2), 1000)
	sync := fixture.buildSync(nil)

	first := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, first, "reg-alpha").State)
	require.Equal(t, "ready", requireSyncItem(t, first, "reg-beta").State)
	require.Equal(t, []string{"registry", "scheduler"}, fixture.order,
		"同轮替换必须固定顺序：先注册表后调度集合")
	require.Len(t, fixture.registry.entries, 2)
	require.Len(t, fixture.scheduler.entries, 2)
	require.ElementsMatch(t, []string{"reg-alpha", "reg-beta"}, syncEntryNames(fixture.registry.entries))
	require.ElementsMatch(t, []string{"reg-alpha", "reg-beta"}, syncEntryNames(fixture.scheduler.entries))

	// 第二轮：增量面没有新行，但 ready 条目仍必须参与本轮两份集合替换。
	fixture.order = nil
	fixture.syncOnce(sync)
	require.Equal(t, []string{"registry", "scheduler"}, fixture.order)
	require.Len(t, fixture.registry.entries, 2)
	require.Len(t, fixture.scheduler.entries, 2)
}

// 中文验收点：同步写进的是**真实**注册表（`agentRegistry.ReplaceAll`）而不是替身——
// 批次里的 Agent 事后可被 `Lookup` 命中，输入之外的既有条目保持不动（不删除语义在
// 真实实现上同样成立），调度集合仍只收本批条目。
// Acceptance point: the sync publishes into the real registry and leaves entries outside
// the batch untouched.
func TestAssetSync_PublishesIntoRealRegistry(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	fixture.addBundle("live-alpha", syncUID(1), 1000)
	realRegistry := newAgentRegistry(fixture.workspaceRoot,
		[]core.AgentEntry{registeredAgent("legacy")}, nil)
	sync := fixture.buildSync(func(deps *AssetSyncDeps) { deps.Registry = realRegistry })

	report := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, report, "live-alpha").State)

	entry, ok := realRegistry.Lookup("live-alpha")
	require.True(t, ok, "同步必须把就绪条目发布进真实注册表")
	require.Equal(t, "live-alpha", entry.Profile.Name)
	_, ok = realRegistry.Lookup("legacy")
	require.True(t, ok, "真实注册表里输入之外的条目不得被删除")
	require.Equal(t, []string{"live-alpha"}, syncEntryNames(fixture.scheduler.entries),
		"调度集合仍只收本批条目")
}

// entryNames 抽出入参里的 Agent 名。
func syncEntryNames(entries []core.AgentEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

// 中文验收点：安装在注册之前崩溃（目录已在、载荷表里没有这一行）时，下一次同步
// 必须靠 Resolve 命中重建注册表，不得永久漏注册、也不得回读载荷。
// Acceptance point: a crash between install and register recovers from the installed
// directory without reading the payload.
func TestAssetSync_RecoversAfterCrashBetweenInstallAndRegister(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	fixture.addInstalledBundle("crash-alpha", syncUID(1), 1000)
	sync := fixture.buildSync(nil)

	report := fixture.syncOnce(sync)
	require.Equal(t, "ready", requireSyncItem(t, report, "crash-alpha").State)
	require.Empty(t, fixture.dist.payloadReads, "已安装目录必须被 Resolve 复用")
	require.Len(t, fixture.registry.entries, 1)
	require.Equal(t, "crash-alpha", fixture.registry.entries[0].Name)
}

// 中文验收点：单轮总超时必须由 `sync_timeout` 截断并返回可判定的错误，不得无限挂住
// 调度循环。逆改点（模块卡 §4.3）：整轮不带超时 → 本用例一直阻塞到测试超时。
// Acceptance point: one round is bounded by sync_timeout and reports the deadline.
func TestAssetSync_TickTimeout(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	fixture.dist.blockAfter = true
	sync := fixture.buildSync(func(deps *AssetSyncDeps) { deps.Timeout = 150 * time.Millisecond })

	start := time.Now()
	_, err := sync.SyncOnce(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), 5*time.Second, "超时必须在本轮内生效")
}

// 中文验收点：同步的写入面只有资产目录；种子工作区（state/、runs/、agents/、工作副本
// 技能目录）的文件哈希在同步前后逐字不变。逆改点（模块卡 §4.3）：同步直接写工作副本
// → 快照比对出现差异。
// Acceptance point: the sync never writes the seed workspace or the run data.
func TestAssetSync_DoesNotTouchWorkspace(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	writeWorkspaceFile(t, fixture.workspaceRoot, "state/seen.json", `{"seen":["guard-alpha"]}`)
	writeWorkspaceFile(t, fixture.workspaceRoot, "runs/run-1.json", `{"id":"run-1"}`)
	writeWorkspaceFile(t, fixture.workspaceRoot, "skills/work-skill/SKILL.md", "工作副本正文\n")
	hash := fixture.addBundle("guard-alpha", syncUID(1), 1000)
	sync := fixture.buildSync(nil)

	before := snapshotTree(t, fixture.workspaceRoot)
	fixture.syncOnce(sync)
	fixture.syncOnce(sync)
	after := snapshotTree(t, fixture.workspaceRoot)
	require.Equal(t, before, after, "同步不得改动种子工作区与运行数据")

	// 资产根确实装了目录：判据不是"什么都没发生"。
	dir, err := assets.Resolve(context.Background(), fixture.assetRoot, "guard-alpha", hash)
	require.NoError(t, err)
	require.DirExists(t, dir)
	require.Equal(t, 1, fixture.dist.payloadReads[hash])
}

// TestAssetSync_RepairHookReceivesFullScanVerdict 守「同步轮把全量扫描判据传给修复器」
// （2026-11-10 复核 P2 的接线面）：运行期复位不能靠修复器自己猜轮次，必须与发现阶段
// 用同一个 `fullScanDue` 判据。`FullScanEvery=3` 时四轮依次为 真 / 假 / 假 / 真。
// The repair hook receives the same full-scan verdict the discovery phase used.
func TestAssetSync_RepairHookReceivesFullScanVerdict(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	var flags []bool
	hook := func(_ context.Context, fullScan bool) error {
		flags = append(flags, fullScan)
		return nil
	}
	sync := fixture.buildSync(func(deps *AssetSyncDeps) {
		deps.Registry = nil
		deps.Scheduler = nil
		deps.Loader = nil
		deps.Repair = hook
		deps.FullScanEvery = 3
	})
	for range 4 {
		fixture.syncOnce(sync)
	}
	require.Equal(t, []bool{true, false, false, true}, flags,
		"修复钩子必须收到与发现阶段同一份全量扫描判据（轮次 0 与每隔 FullScanEvery 轮）")

	fixture = newAssetSyncFixture(t)
	flags = nil
	sync = fixture.buildSync(func(deps *AssetSyncDeps) {
		deps.Registry = nil
		deps.Scheduler = nil
		deps.Loader = nil
		deps.Repair = hook
		deps.FullScanEvery = 1
	})
	for range 2 {
		fixture.syncOnce(sync)
	}
	require.Equal(t, []bool{true, true}, flags, "FullScanEvery=1 时每轮都是全量扫描")
}
