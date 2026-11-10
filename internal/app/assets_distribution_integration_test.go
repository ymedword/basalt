//go:build integration

package app

// 本文件是 US-25C T025 的集成验收面（模块卡 §4.1，覆盖 FR-25C-06 / 07 / 08 / 09）：
// 「控制面发布 → 各 Worker 同步 → 物化 → 就绪 → 准入 → 执行版本一致」的整条链路，
// 以及「安装完成、注册未成」崩溃窗口的恢复。依赖全是真实 PostgreSQL 仓储与真实的
// 构建 / 安装 / 物化实现；"另一个进程"用在同一组根目录上新构造的 `AssetSync` 表示
// ——它不携带上一进程的任何内存状态，与真实重启的差别只在进程号。
// Integration acceptance for the distribution chain: publication, per-worker install,
// materialization, readiness, admission pinning, and recovery from the crash window
// between install and register. The only simulation is that "another process" is a
// freshly constructed AssetSync over the same roots; every dependency is real.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// assetsDistributionAgent 是被发布的 Agent 名：两个 Worker 都按它判卷。
	// assetsDistributionAgent is the published agent both workers are judged by.
	assetsDistributionAgent = "dist-writer"
	// assetsDistributionSkill 是它声明并依赖的技能名。
	// assetsDistributionSkill is the skill the agent declares.
	assetsDistributionSkill = "dist-style"
)

// distributionScene 是一份「控制面 + 共享库」的现场：真实 PostgreSQL 里的发布记录与
// 载荷、控制面工作区里的定义与技能版本目录，以及断言要用的期望值。
// distributionScene is the control-plane scene: real publication rows, payloads and the
// control workspace holding the definition and the published skill version directory.
type distributionScene struct {
	ctx        context.Context
	t          *testing.T
	db         *postgres.DB
	versions   *postgres.ResourceStore
	ops        *postgres.ManagementStore
	dist       *postgres.AssetDistributionStore
	payloads   *postgres.BundlePayloadStore
	root       string
	bundleRoot string
	clock      *syncClock

	agent      string
	agentUID   core.AgentUID
	skill      string
	skillUID   core.ResourceUID
	skillRel   string
	skillFiles map[string]string

	bundleHash string
	versionID  int64
	definition string
}

// newDistributionScene 建一份完整的已发布现场：基线操作 → 身份镜像 → 技能发布 →
// Agent bundle 发布。发布走生产的 `publishWorkspaceAgentBundle`，不手写版本行。
// newDistributionScene publishes one skill and one agent bundle through the production
// chain; nothing about the publication rows is hand-assembled.
func newDistributionScene(t *testing.T) *distributionScene {
	t.Helper()
	ctx := t.Context()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	db := pgtest.NewTestDB(t)
	scene := &distributionScene{
		ctx:        ctx,
		t:          t,
		db:         db,
		versions:   postgres.NewResourceStore(db),
		ops:        postgres.NewManagementStore(db),
		dist:       postgres.NewAssetDistributionStore(db),
		payloads:   postgres.NewBundlePayloadStore(db),
		root:       root,
		bundleRoot: filepath.Join(t.TempDir(), "bundles"),
		clock:      newAssetSyncClock(),
		agent:      assetsDistributionAgent,
		skill:      assetsDistributionSkill,
	}

	// 版本行的 `operation_id` 是非空外键；基线行与生产基线同形（`system.baseline`）。
	baselineID := scene.beginOperation("system.baseline", "system", "system")

	// 控制面发布要求生命周期索引里有该 Agent 的稳定身份（`resolveWorkspaceAgentUID`）。
	agentUID, err := core.NewAgentUID()
	require.NoError(t, err)
	scene.agentUID = agentUID
	require.NoError(t, scene.versions.PutLifecycleIndex(ctx, core.LifecycleRecord{
		SchemaVersion: 2,
		Agent:         scene.agent,
		AgentUID:      agentUID,
		State:         core.AgentActive,
		Generation:    1,
		UpdatedAt:     scene.clock.Now().UnixMilli(),
		OperationID:   baselineID,
	}, true, ""))

	scene.publishSkill()
	scene.definition = scene.writeAgentDefinition(1)
	scene.publishBundle()
	return scene
}

// beginOperation 预写一条 pending 管理操作：技能发布与基线都复用它承载外键。
// beginOperation opens the pending operation row the version FK points at.
func (s *distributionScene) beginOperation(action, targetKind, targetID string) string {
	s.t.Helper()
	opID := core.NewOperationID(s.clock.Now())
	require.NoError(s.t, s.ops.BeginOperation(s.ctx, core.OperationRecord{
		OperationID:  opID,
		Action:       action,
		TargetKind:   targetKind,
		TargetID:     targetID,
		ActorSubject: core.ActorSubjectMigration,
		AuthMethod:   core.AuthMethodMigration,
		Source:       "test",
		Status:       core.OperationPending,
		CreatedAt:    s.clock.Now(),
	}))
	return opID
}

// publishSkill 在控制面工作区物化一个技能版本目录并发布它：文件清单进版本行的
// `manifest_json`（与 `parseSkillManifest` 同形），`storage_path` 取契约字面量。
// publishSkill writes the skill version directory and publishes its file manifest.
func (s *distributionScene) publishSkill() {
	s.t.Helper()

	skillUID, err := core.NewResourceUID()
	require.NoError(s.t, err)
	s.skillUID = skillUID

	relDir, err := workspace.SkillVersionRelDir(s.skill, skillUID, 1)
	require.NoError(s.t, err)
	logicalDir, err := workspace.SkillVersionLogicalDir(s.skill, skillUID, 1)
	require.NoError(s.t, err)
	s.skillRel = relDir
	s.skillFiles = map[string]string{
		"SKILL.md":            "---\nname: " + s.skill + "\ndescription: 分发夹具技能\n---\n\n发布版本正文\n",
		"references/notes.md": "分发夹具参考\n",
	}

	paths := make([]string, 0, len(s.skillFiles))
	for file := range s.skillFiles {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	refs := make([]core.FileHash, 0, len(paths))
	for _, file := range paths {
		writeWorkspaceFile(s.t, s.root, path.Join(relDir, file), s.skillFiles[file])
		refs = append(refs, materializeFileHash(file, s.skillFiles[file]))
	}
	manifestJSON, err := json.Marshal(refs)
	require.NoError(s.t, err)

	opID := s.beginOperation(core.ActionSkillCreate, core.TargetKindSkill, s.skill)
	_, err = s.versions.PublishWithVersion(s.ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceID:   s.skill,
		ResourceUID:  skillUID,
		ContentHash:  "skill-content-1",
		ManifestJSON: string(manifestJSON),
		StoragePath:  logicalDir,
		CreatedAt:    s.clock.Now().UnixMilli(),
		OperationID:  opID,
	}, opID, s.clock.Now())
	require.NoError(s.t, err)
}

// writeAgentDefinition 把第 revision 版定义落进控制面工作区并返回其字节：正文随修订
// 变化，保证每次发布得到新的内容哈希。
// writeAgentDefinition writes the revision-th definition and returns its exact bytes.
func (s *distributionScene) writeAgentDefinition(revision int) string {
	s.t.Helper()
	definition := fmt.Sprintf(`---
name: %s
description: 分发夹具
provider:
  name: deepseek
  model: deepseek-chat
skills:
  - %s
---

分发夹具正文，修订 %d。
`, s.agent, s.skill, revision)
	writeWorkspaceFile(s.t, s.root,
		path.Join(agentsDirName, s.agent, agentDefinitionFileName), definition)
	return definition
}

// publishBundle 走生产发布链把当前定义发布出去，并刷新夹具上的期望值。
// publishBundle publishes through the production chain and refreshes the expectations.
func (s *distributionScene) publishBundle() {
	s.t.Helper()
	versionID, err := publishWorkspaceAgentBundle(s.ctx, workspacePublishDeps{
		Root:         s.root,
		BundleRoot:   s.bundleRoot,
		Versions:     s.versions,
		Operations:   s.ops,
		Payloads:     s.dist,
		PayloadStore: s.payloads,
		MaxBytes:     syncTestMaxBytes,
		Logger:       nil,
		Now:          s.clock.Now,
	}, s.agent)
	require.NoError(s.t, err, "控制面发布必须成功")
	s.versionID = versionID

	pointer, err := s.versions.PointerByUID(s.ctx, core.ResourceAgentBundle,
		core.ResourceUID(s.agentUID))
	require.NoError(s.t, err)
	require.Equal(s.t, versionID, pointer.PublishedVersionID, "指针必须指向本次发布的版本")
	version, err := s.versions.VersionByID(s.ctx, pointer.PublishedVersionID)
	require.NoError(s.t, err)
	s.bundleHash = version.ContentHash
}

// republish 发布下一版定义：时钟必须先前进，否则指针行的 `updated_at`（增量发现的
// 复合键集）不变化，各 Worker 的下一轮发现看不到这一行。
// republish advances the clock first so the pointer's updated_at moves and the
// incremental discovery actually rediscovers the row.
func (s *distributionScene) republish(revision int) {
	s.t.Helper()
	previous := s.bundleHash
	s.clock.Advance(2 * time.Second)
	s.definition = s.writeAgentDefinition(revision)
	s.publishBundle()
	require.NotEqual(s.t, previous, s.bundleHash, "正文变化必须得到新的 bundle 哈希")
}

// countingDistribution 包住真实分发读面并统计载荷 / 技能版本读取次数：崩溃恢复的
// 「不重读载荷」判据需要一个可数的观察点。
// countingDistribution wraps the real port to make "recovery does not re-read the
// payload" observable.
type countingDistribution struct {
	inner core.AssetDistributionPort

	mu           sync.Mutex
	payloadReads int
	skillReads   int
}

// 编译期断言：包装器仍是同步消费的分发读面。
// Compile-time assertion that the wrapper still satisfies the distribution port.
var _ core.AssetDistributionPort = (*countingDistribution)(nil)

func (c *countingDistribution) ListPublishedBundlesAfter(ctx context.Context,
	cursor core.PublishedBundleCursor, limit int,
) ([]core.PublishedBundle, error) {
	return c.inner.ListPublishedBundlesAfter(ctx, cursor, limit)
}

func (c *countingDistribution) ListPublishedBundlesPage(ctx context.Context,
	afterResourceID string, limit int,
) ([]core.PublishedBundle, error) {
	return c.inner.ListPublishedBundlesPage(ctx, afterResourceID, limit)
}

func (c *countingDistribution) ReadBundlePayload(ctx context.Context, hash string,
) (core.BundlePayloadRecord, bool, error) {
	c.mu.Lock()
	c.payloadReads++
	c.mu.Unlock()
	return c.inner.ReadBundlePayload(ctx, hash)
}

func (c *countingDistribution) ReadSkillVersion(ctx context.Context, versionID int64,
) (core.SkillVersionRecord, bool, error) {
	c.mu.Lock()
	c.skillReads++
	c.mu.Unlock()
	return c.inner.ReadSkillVersion(ctx, versionID)
}

// payloadReadCount 返回累计的载荷读取次数。
// payloadReadCount reports how many payload reads happened so far.
func (c *countingDistribution) payloadReadCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.payloadReads
}

// distributionWorker 是一个独立 Worker 进程的投影：自己的资产根与工作区根，
// 共享的只有数据库。
// distributionWorker is one worker process: its own asset root and workspace root,
// sharing only the database with the control plane and the other worker.
type distributionWorker struct {
	ctx       context.Context
	t         *testing.T
	scene     *distributionScene
	root      string
	assetRoot string
	logger    *slog.Logger
	registry  *agentRegistry
	scheduler *fakeSyncScheduler
	dist      *countingDistribution
	syncer    *AssetSync
}

// newDistributionWorker 建一个 Worker：真实注册表、真实装载器、真实同步对象；
// 资产根与工作区根都是全新的空目录（"另一台机器"）。
// newDistributionWorker builds one worker over fresh, empty roots.
func newDistributionWorker(t *testing.T, scene *distributionScene) *distributionWorker {
	t.Helper()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)
	// 物化目录按发布版本冻结为只读，临时目录清理需要能解冻。
	unlockTreeOnCleanup(t, root)

	logger, _ := newCapturedLogger()
	worker := &distributionWorker{
		ctx:       scene.ctx,
		t:         t,
		scene:     scene,
		root:      root,
		assetRoot: t.TempDir(),
		logger:    logger,
		registry:  newAgentRegistry(root, nil, logger),
		scheduler: &fakeSyncScheduler{},
		dist:      &countingDistribution{inner: scene.dist},
	}
	worker.syncer = worker.buildSync(worker.dist, worker.registry, worker.scheduler)
	return worker
}

// buildSync 按给定写面构造一份同步对象：它只做装配校验，不启动任何协程；崩溃恢复
// 用例因此可以用同一组根目录、不同写面重建（等价于新进程的首次同步）。
// buildSync wires one sync object over the worker's roots; nothing starts here.
func (w *distributionWorker) buildSync(dist core.AssetDistributionPort,
	registry AgentRegistryReplacer, scheduler ScheduleSetUpdater,
) *AssetSync {
	w.t.Helper()
	syncer, err := NewAssetSync(AssetSyncDeps{
		WorkspaceRoot: w.root,
		AssetRoot:     w.assetRoot,
		Distribution:  dist,
		Versions:      w.scene.versions,
		Registry:      registry,
		Scheduler:     scheduler,
		Loader:        workspace.NewAgentLoader(workspace.AgentLoaderOptions{}),
		Providers:     readyProviderCatalog,
		Logger:        w.logger,
		Now:           w.scene.clock.Now,
		Interval:      time.Second,
		Timeout:       20 * time.Second,
		Batch:         50,
		MaxBytes:      syncTestMaxBytes,
		FullScanEvery: 20,
	})
	require.NoError(w.t, err)
	return syncer
}

// syncOnce 跑一轮并把错误当作失败（超时用例才直接调 SyncOnce）。
// syncOnce runs one round and treats an error as a failure.
func (w *distributionWorker) syncOnce() SyncReport {
	w.t.Helper()
	report, err := w.syncer.SyncOnce(w.ctx)
	require.NoError(w.t, err)
	return report
}

// requireDistributionOnlyWrites 断言同步只新增 `versions/skills/**` 下的文件：其余路径
// 逐文件哈希不变（FR-25C-09 的写入面白名单）。
// requireDistributionOnlyWrites asserts the only added files live under the published
// skill directory and every pre-existing file is byte-identical (FR-25C-09).
func requireDistributionOnlyWrites(t *testing.T, before, after map[string]string, skillRel string) {
	t.Helper()
	for file, sum := range before {
		require.Equal(t, sum, after[file], "同步不得改写工作区文件 %s（FR-25C-09）", file)
	}
	prefix := filepath.ToSlash(skillRel) + "/"
	for file := range after {
		if _, existed := before[file]; existed {
			continue
		}
		require.True(t, strings.HasPrefix(file, prefix),
			"同步新增了白名单之外的文件 %s（只允许 %s/**）", file, prefix)
	}
	require.NotEmpty(t, after, "快照不得为空")
}

// requireInstalledDefinition 断言已安装目录里的定义字节就是发布出去的那一份。
// requireInstalledDefinition asserts the installed definition bytes are the published ones.
func requireInstalledDefinition(t *testing.T, dir, want, agent string) {
	t.Helper()
	rel := assets.ConfigBundleRel(path.Join(agentsDirName, agent, agentDefinitionFileName))
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	require.NoError(t, err, "已安装 bundle 必须包含定义文件 %s", rel)
	require.Equal(t, want, string(raw), "已安装目录里的定义必须与发布字节逐字一致")
}

// 中文验收点：一次发布之后，两个各自独立的 Worker 都能把内容装到本机、把技能物化到
// 本机发布版本目录、构建并注册同一条 Entry；两端安装哈希与发布指针一致；同步只写
// `versions/skills/**`，工作区其余文件逐字节不变。逆改点（模块卡 §4.3）：只装 bundle
// 不物化 → Entry 判 `skill_not_found`、状态停在 `blocked`；跳过 `Verify` → 篡改用例
// 转由 T026 的准入面兜底，本用例看不到"两端同哈希"。
// Acceptance point: two independent workers install the same publication, materialize
// the skill locally, register the same entry, and write nothing outside the published
// skill directory.
func TestAssetsDistribution_CrossProcessInstallAndReady(t *testing.T) {
	t.Parallel()
	scene := newDistributionScene(t)
	workerA := newDistributionWorker(t, scene)
	workerB := newDistributionWorker(t, scene)

	// 同步前后整棵树对照：白名单之外的文件必须逐字节不变。
	beforeA := snapshotTree(t, workerA.root)
	beforeB := snapshotTree(t, workerB.root)

	reportA := workerA.syncOnce()
	reportB := workerB.syncOnce()

	for name, pair := range map[string]struct {
		worker *distributionWorker
		report SyncReport
	}{
		"A": {worker: workerA, report: reportA},
		"B": {worker: workerB, report: reportB},
	} {
		item := requireSyncItem(t, pair.report, scene.agent)
		require.Equal(t, syncStateReady, item.State, "Worker %s 必须就绪", name)
		require.Equal(t, scene.bundleHash, item.PointerHash, "Worker %s 的指针哈希必须来自共享记录", name)
		require.Equal(t, scene.bundleHash, item.InstalledHash, "Worker %s 安装的必须就是发布的哈希", name)
		require.Equal(t, []string{scene.skill}, item.Skills, "Worker %s 必须报告已物化的技能", name)
		require.Empty(t, item.BlockedReason)

		readiness := pair.worker.syncer.Readiness()[scene.agent]
		require.Equal(t, scene.bundleHash, readiness.PointerHash)
		require.Equal(t, scene.bundleHash, readiness.ReadyHash, "Worker %s 的就绪哈希必须是已安装哈希", name)
		require.Empty(t, readiness.BlockedReason)

		require.Equal(t, 1, pair.worker.dist.payloadReadCount(),
			"Worker %s 首轮安装必须读一次载荷（分发介质就是载荷表）", name)
	}

	// 两端装的是同一份内容寻址目录：同哈希、同清单、同身份。
	for name, worker := range map[string]*distributionWorker{"A": workerA, "B": workerB} {
		dir, err := assets.Resolve(scene.ctx, worker.assetRoot, scene.agent, scene.bundleHash)
		require.NoError(t, err, "Worker %s 必须以内容寻址方式解析到已安装目录", name)
		require.Equal(t, scene.bundleHash, filepath.Base(dir), "安装目录名必须是 bundle 哈希")
		require.True(t, strings.HasPrefix(dir, worker.assetRoot), "安装目录必须在本机资产根下")

		manifest, err := assets.Verify(scene.ctx, dir)
		require.NoError(t, err, "Worker %s 的已安装目录必须自洽", name)
		require.Equal(t, scene.bundleHash, manifest.BundleHash)
		require.Equal(t, scene.agentUID, manifest.AgentUID, "清单身份必须是同一 Agent UID")
		require.Len(t, manifest.Skills, 1)
		require.Equal(t, scene.skill, manifest.Skills[0].Name)

		// 技能正文按发布记录逐文件物化到本机发布版本目录（不是工作副本）。
		for file, want := range scene.skillFiles {
			materialized := filepath.Join(worker.root,
				filepath.FromSlash(scene.skillRel), filepath.FromSlash(file))
			raw, err := os.ReadFile(materialized)
			require.NoError(t, err, "Worker %s 必须物化技能文件 %s", name, file)
			require.Equal(t, want, string(raw), "Worker %s 的技能字节必须与发布记录一致", name)
		}

		// 注册条目取自已安装 bundle 的定义字节，而不是本机工作副本。
		entry, ok := worker.registry.Lookup(scene.agent)
		require.True(t, ok, "Worker %s 的注册表必须包含该 Agent", name)
		require.True(t, entry.Registered, "Worker %s 的条目必须通过全部校验", name)
		require.Equal(t, scene.definition, string(entry.Definition),
			"Worker %s 的注册条目必须取自已安装 bundle 的定义字节", name)

		require.Len(t, worker.scheduler.entries, 1, "Worker %s 的调度集合必须同轮收到该条目", name)
		require.Equal(t, scene.agent, worker.scheduler.entries[0].Name)
	}

	requireDistributionOnlyWrites(t, beforeA, snapshotTree(t, workerA.root), scene.skillRel)
	requireDistributionOnlyWrites(t, beforeB, snapshotTree(t, workerB.root), scene.skillRel)
}

// 中文验收点：准入固定的版本必须来自本机已安装目录——未同步时按不可用失败关闭；
// 同步后固定哈希与发布指针一致、定义与技能都取自同一份 bundle；发布新版并同步之后，
// 先前取到的固定目录仍读旧字节（在途 Run 不因同步切版本）；已安装内容被改写时准入
// 按版本不符拒绝，**不**静默换目录、不自动修复。
// Acceptance point: admission pins the installed version, an in-flight pin keeps reading
// its own bytes after a newer publication, and tampered content is rejected rather than
// silently replaced.
func TestAssetsDistribution_AdmissionUsesInstalledVersion(t *testing.T) {
	t.Parallel()
	scene := newDistributionScene(t)
	worker := newDistributionWorker(t, scene)
	verifier := newAssetVerifier(scene.versions, worker.assetRoot)

	// 还没同步：准入必须按"不可用"失败关闭（503 档），不得回落到工作副本或空包。
	_, err := verifier.VerifyBundle(scene.ctx, scene.agent, scene.agentUID)
	require.ErrorIs(t, err, core.ErrAssetUnavailable, "未安装时准入必须报不可用")

	worker.syncOnce()

	firstPin, err := verifier.VerifyBundle(scene.ctx, scene.agent, scene.agentUID)
	require.NoError(t, err)
	require.Equal(t, scene.bundleHash, firstPin.Hash, "固定的哈希必须来自共享发布指针")
	require.Equal(t, scene.bundleHash, filepath.Base(firstPin.Dir), "固定目录必须是内容寻址目录")
	require.True(t, strings.HasPrefix(firstPin.Dir, worker.assetRoot), "固定目录必须在本机资产根下")
	requireInstalledDefinition(t, firstPin.Dir, scene.definition, scene.agent)

	// 执行期一致：注册条目的定义就是固定 bundle 的那一份，技能版本也与包内记录一致。
	entry, ok := worker.registry.Lookup(scene.agent)
	require.True(t, ok)
	require.NoError(t, checkDefinitionMatchesBundle(entry, firstPin),
		"注册条目的定义字节必须就是已发布 bundle 的那一份")
	ref, _, err := resolvePublishedSkill(scene.ctx, scene.versions, scene.skill)
	require.NoError(t, err)
	require.NoError(t, checkBundleSkills(firstPin, []core.SkillVersionRef{ref}),
		"包内技能版本必须与准入固定的一致")

	// 在途 Run 的固定值：新版本发布并同步之后，先前那份 pin 仍读旧字节。
	firstDefinition := scene.definition
	firstDir := firstPin.Dir
	scene.republish(2)

	report := worker.syncOnce()
	item := requireSyncItem(t, report, scene.agent)
	require.Equal(t, syncStateReady, item.State)
	require.Equal(t, scene.bundleHash, item.InstalledHash)
	require.NotEqual(t, firstDir, filepath.Join(worker.assetRoot, "bundles", scene.agent, scene.bundleHash))
	requireInstalledDefinition(t, firstDir, firstDefinition, scene.agent)

	secondPin, err := verifier.VerifyBundle(scene.ctx, scene.agent, scene.agentUID)
	require.NoError(t, err)
	require.Equal(t, scene.bundleHash, secondPin.Hash, "新准入必须固定新版本")
	require.NotEqual(t, firstPin.Dir, secondPin.Dir, "两版内容必须落在两个内容寻址目录")
	requireInstalledDefinition(t, secondPin.Dir, scene.definition, scene.agent)

	// 已安装内容被改写：准入按版本不符拒绝，且**不**自动修复（不覆盖不可变目录）。
	tamperedRel := assets.ConfigBundleRel(path.Join(agentsDirName, scene.agent, agentDefinitionFileName))
	tampered := filepath.Join(secondPin.Dir, filepath.FromSlash(tamperedRel))
	const tamperedBody = "---\nname: tampered\n---\n"
	require.NoError(t, os.WriteFile(tampered, []byte(tamperedBody), 0o600))
	_, err = verifier.VerifyBundle(scene.ctx, scene.agent, scene.agentUID)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch,
		"内容被改写必须按版本不符拒绝，不得静默换目录")
	raw, readErr := os.ReadFile(tampered)
	require.NoError(t, readErr)
	require.Equal(t, tamperedBody, string(raw), "准入不得自动修复 / 覆盖已安装内容")
}

// 中文验收点：目录已装好、注册表替换失败（"注册前崩溃"）时条目必须停在 `installed`
// 而不是 `ready`；同一组根目录上新构造的同步对象（= 新进程）能在**不重读载荷**的前提
// 下补齐注册——恢复只依赖已安装目录，不依赖重新分发。
// Acceptance point: a failed registration leaves the item installed, and a fresh process
// over the same roots recovers readiness without reading the payload again.
func TestAssetsDistribution_CrashBetweenInstallAndRegisterRecovers(t *testing.T) {
	t.Parallel()
	scene := newDistributionScene(t)
	worker := newDistributionWorker(t, scene)

	// 第一轮：注入注册表失败，模拟"目录装完、注册前进程退出"的窗口。
	failing := &fakeSyncRegistry{err: errors.New("注册表替换失败（模拟注册前崩溃）")}
	first := worker.buildSync(worker.dist, failing, &fakeSyncScheduler{})
	firstReport, err := first.SyncOnce(scene.ctx)
	require.NoError(t, err)

	item := requireSyncItem(t, firstReport, scene.agent)
	require.Equal(t, syncStateInstalled, item.State,
		"目录装好但注册未成：状态必须停在 installed，不得算就绪")
	require.Equal(t, scene.bundleHash, item.InstalledHash)
	require.Empty(t, first.Readiness()[scene.agent].ReadyHash, "注册失败不得填就绪哈希")
	require.Contains(t, strings.Join(firstReport.Errors, "\n"), "注册表替换失败",
		"报告必须写明注册失败")
	require.Equal(t, 1, worker.dist.payloadReadCount(), "首轮安装读过一次载荷")

	// 崩溃后的新进程：同一组资产根与工作区根，全新的同步对象与内存工作集。
	recovered := worker.buildSync(worker.dist, worker.registry, worker.scheduler)
	secondReport, err := recovered.SyncOnce(scene.ctx)
	require.NoError(t, err)

	item = requireSyncItem(t, secondReport, scene.agent)
	require.Equal(t, syncStateReady, item.State, "重启后的同步必须补上注册")
	require.Equal(t, scene.bundleHash, item.InstalledHash)
	require.Empty(t, item.BlockedReason)
	require.Equal(t, scene.bundleHash, recovered.Readiness()[scene.agent].ReadyHash)
	require.Equal(t, 1, worker.dist.payloadReadCount(),
		"恢复必须命中已安装目录（assets.Resolve），不得重读载荷")

	entry, ok := worker.registry.Lookup(scene.agent)
	require.True(t, ok)
	require.True(t, entry.Registered)
	require.Equal(t, scene.definition, string(entry.Definition))
	require.Len(t, worker.scheduler.entries, 1)
}
