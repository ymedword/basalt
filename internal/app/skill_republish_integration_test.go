//go:build integration

package app

// 本文件是 US-25C T040 的集成验收面（模块卡 §3.4 集成行 / §4.3 `us25c-sync`）：
// 技能联动重发（FR-25C-17）在**真实 PostgreSQL** 上跑完整链路——发布记录、载荷表、
// 操作台账与内容寻址安装目录都是真的；"另一个进程"用同一组根目录上的新装配表示，
// 与真实重启的差别只在进程号。
//
// 三条判据都盯可观察结果，不盯代码写了什么：
//
//  1. `TestSkillRepublishIntegration_PerAgentResultsVisible`：技能发布成功、一个依赖
//     Agent 重发失败 → `ResourcePublishResult.publish` 里逐 Agent 可见（`ok` / `failed`
//     与固定原因），失败项零发布；没有依赖方的技能发布保持该字段为 nil（契约省略语义）。
//  2. `TestSkillRepublishIntegration_NextRoundRepair`：失败项由**同一个**修复器实例在
//     运行期下一轮（退避到期后）修好——进程不重启，故障解除即自愈。
//  3. `TestSkillRepublishIntegration_RestartIsFallbackOnly`：运行期已收敛时重启兜底
//     （`repairStaleBundleSkills`）零发布；只有运行期没轮到的落后项才由它补齐。
//
// 失败注入用"篡改已安装目录里的定义文件"：清单完好（`assets.Resolve` 仍解析得到）、
// 内容哈希对不上（`assets.Verify` 拒绝），得到确定的 `failed`——不伪造错误路径，也不
// 走"删目录回落到载荷"的另一种结局。
// The failure is injected by corrupting an installed config file: resolution still
// succeeds while verification fails, a deterministic `failed` outcome.

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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
	// republishAgentAlpha / Beta 是两个直接依赖该技能的 Agent：alpha 判"重发成功"，
	// beta 判"逐项失败可见、下一轮自愈"。
	// Both agents directly depend on the skill; alpha judges success, beta failure.
	republishAgentAlpha = "repub-alpha"
	republishAgentBeta  = "repub-beta"
	// republishSkillName 是被依赖的技能名（单个目录名）。
	// republishSkillName is the depended-on skill.
	republishSkillName = "repub-style"
	// republishRepairBatch / republishRepairInterval 与生产缺省一致：每轮 5 个、退避基数 30s。
	// The repair parameters match the production defaults.
	republishRepairBatch    = 5
	republishRepairInterval = 30 * time.Second
	// republishTamperedBody 是失败注入写进已安装目录的字节：任何已发布版本都不可能是它。
	// republishTamperedBody is the injected bytes no published version can match.
	republishTamperedBody = "被篡改的已安装内容（集成夹具）\n"
)

// republishScene 是「控制面 + 真实 PostgreSQL」的现场：共享发布记录、载荷表与操作台账
// 都是真实仓储，工作区里放着技能发布版本目录与 Agent 定义。
// republishScene is the control-plane scene over real repositories.
type republishScene struct {
	ctx        context.Context
	t          *testing.T
	versions   *postgres.ResourceStore
	ops        *postgres.ManagementStore
	dist       *postgres.AssetDistributionStore
	payloads   *postgres.BundlePayloadStore
	root       string
	bundleRoot string
	clock      *syncClock

	skill           string
	skillUID        core.ResourceUID
	agents          map[string]core.AgentUID
	skillVersionIDs map[int]int64
}

// newRepublishScene 建一份现场：技能 v1 → 两个 Agent 的 bundle 发布（引用 v1）→ 技能 v2。
// 第二步之后两个 Agent 都落后于 live 版本，正是联动重发与运行期修复的输入。
// newRepublishScene publishes skill v1, both agent bundles, then skill v2 so both
// agents are stale — the exact input of the linkage republish and the repairer.
func newRepublishScene(t *testing.T) *republishScene {
	t.Helper()
	ctx := t.Context()

	root := filepath.Join(t.TempDir(), workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err)

	db := pgtest.NewTestDB(t)
	scene := &republishScene{
		ctx:             ctx,
		t:               t,
		versions:        postgres.NewResourceStore(db),
		ops:             postgres.NewManagementStore(db),
		dist:            postgres.NewAssetDistributionStore(db),
		payloads:        postgres.NewBundlePayloadStore(db),
		root:            root,
		bundleRoot:      filepath.Join(t.TempDir(), "bundles"),
		clock:           newAssetSyncClock(),
		skill:           republishSkillName,
		agents:          make(map[string]core.AgentUID),
		skillVersionIDs: make(map[int]int64),
	}
	if uid, err := core.NewResourceUID(); err == nil {
		scene.skillUID = uid
	}
	require.NotEmpty(t, scene.skillUID, "技能身份必须合法")

	// 版本行的 `operation_id` 是非空外键；基线行与生产基线同形。
	baselineID := scene.beginOperation("system.baseline", "system", "system")

	scene.publishSkillVersion(1)
	for _, agent := range []string{republishAgentAlpha, republishAgentBeta} {
		uid, uidErr := core.NewAgentUID()
		require.NoError(t, uidErr)
		scene.agents[agent] = uid
		require.NoError(t, scene.versions.PutLifecycleIndex(ctx, core.LifecycleRecord{
			SchemaVersion: 2,
			Agent:         agent,
			AgentUID:      uid,
			State:         core.AgentActive,
			Generation:    1,
			UpdatedAt:     scene.clock.Now().UnixMilli(),
			OperationID:   baselineID,
		}, true, ""))
		scene.writeAgentDefinition(agent, 1)
		scene.publishBundle(agent)
	}
	// 时钟先前进：让两次技能发布的指针 `updated_at` 可区分（与生产写入口径一致）。
	scene.clock.Advance(2 * time.Second)
	scene.publishSkillVersion(2)
	return scene
}

// beginOperation 预写一条 pending 管理操作，供版本行的外键引用。
// beginOperation opens the pending operation row the version FK points at.
func (s *republishScene) beginOperation(action, targetKind, targetID string) string {
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

// publishSkillVersion 物化第 number 版技能正文到工作区发布版本目录并发布它：文件清单
// 进版本行的 `manifest_json`（与 `parseSkillManifest` 同形），`storage_path` 取契约
// 字面量。版本号由存储按发布次序分配，这里断言与目录号一致。
// publishSkillVersion materializes and publishes one skill version through the
// production record shape.
func (s *republishScene) publishSkillVersion(number int) {
	s.t.Helper()

	relDir, err := workspace.SkillVersionRelDir(s.skill, s.skillUID, number)
	require.NoError(s.t, err)
	logicalDir, err := workspace.SkillVersionLogicalDir(s.skill, s.skillUID, number)
	require.NoError(s.t, err)

	files := map[string]string{
		"SKILL.md": "---\nname: " + s.skill + "\ndescription: 联动重发夹具技能\n---\n\n" +
			"联动重发夹具正文，第 " + strconv.Itoa(number) + " 版\n",
		"references/notes.md": "联动重发夹具参考，第 " + strconv.Itoa(number) + " 版\n",
	}
	paths := make([]string, 0, len(files))
	for file := range files {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	refs := make([]core.FileHash, 0, len(paths))
	for _, file := range paths {
		writeWorkspaceFile(s.t, s.root, path.Join(relDir, file), files[file])
		refs = append(refs, materializeFileHash(file, files[file]))
	}
	manifestJSON, err := json.Marshal(refs)
	require.NoError(s.t, err)

	opID := s.beginOperation(core.ActionSkillCreate, core.TargetKindSkill, s.skill)
	published, err := s.versions.PublishWithVersion(s.ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceID:   s.skill,
		ResourceUID:  s.skillUID,
		ContentHash:  "skill-content-" + strconv.Itoa(number),
		ManifestJSON: string(manifestJSON),
		StoragePath:  logicalDir,
		CreatedAt:    s.clock.Now().UnixMilli(),
		OperationID:  opID,
	}, opID, s.clock.Now())
	require.NoError(s.t, err)
	require.Equal(s.t, number, published.VersionNo, "版本号按发布次序分配，与目录号一致")
	s.skillVersionIDs[number] = published.ID
}

// writeAgentDefinition 把某 Agent 的定义写进工作区并返回字节：定义声明该技能，
// bundle 构建因此会把它解析成当前已发布版本。
// writeAgentDefinition writes one agent definition declaring the fixture skill.
func (s *republishScene) writeAgentDefinition(agent string, revision int) string {
	s.t.Helper()
	definition := "---\n" +
		"name: " + agent + "\n" +
		"description: 联动重发夹具\n" +
		"provider:\n  name: deepseek\n  model: deepseek-chat\n" +
		"skills:\n  - " + s.skill + "\n" +
		"---\n\n" +
		"联动重发夹具正文，修订 " + strconv.Itoa(revision) + "。\n"
	writeWorkspaceFile(s.t, s.root,
		path.Join(agentsDirName, agent, agentDefinitionFileName), definition)
	return definition
}

// publishBundle 走生产发布链把当前定义发布出去（载荷先写、指针后移）。
// publishBundle publishes through the production chain.
func (s *republishScene) publishBundle(agent string) {
	s.t.Helper()
	versionID, err := publishWorkspaceAgentBundle(s.ctx, workspacePublishDeps{
		Root:         s.root,
		BundleRoot:   s.bundleRoot,
		Versions:     s.versions,
		Operations:   s.ops,
		Payloads:     s.dist,
		PayloadStore: s.payloads,
		MaxBytes:     syncTestMaxBytes,
		Logger:       repairLogger(),
		Now:          s.clock.Now,
	}, agent)
	require.NoError(s.t, err, "控制面发布必须成功")
	pointer, err := s.versions.PointerByUID(s.ctx, core.ResourceAgentBundle,
		core.ResourceUID(s.agents[agent]))
	require.NoError(s.t, err)
	require.Equal(s.t, versionID, pointer.PublishedVersionID, "指针必须指向本次发布的版本")
}

// installedBundle 解析某 Agent 当前已发布 bundle 的安装目录与版本行。
// installedBundle resolves the installed directory and version row of one agent.
func (s *republishScene) installedBundle(agent string) (string, core.ResourceVersion) {
	s.t.Helper()
	version := s.publishedVersion(agent)
	dir, err := assets.Resolve(s.ctx, s.bundleRoot, agent, version.ContentHash)
	require.NoError(s.t, err, "已发布 bundle 必须已装入内容寻址目录")
	return dir, version
}

// publishedVersion 读某 Agent 指针指向的版本行。
// publishedVersion reads the version row the agent pointer refers to.
func (s *republishScene) publishedVersion(agent string) core.ResourceVersion {
	s.t.Helper()
	pointer, err := s.versions.PointerByUID(s.ctx, core.ResourceAgentBundle,
		core.ResourceUID(s.agents[agent]))
	require.NoError(s.t, err)
	version, err := s.versions.VersionByID(s.ctx, pointer.PublishedVersionID)
	require.NoError(s.t, err)
	return version
}

// publishedManifest 读回某 Agent 当前已发布 bundle 的清单。
// publishedManifest reads back the published manifest of one agent.
func (s *republishScene) publishedManifest(agent string) assets.Manifest {
	s.t.Helper()
	var manifest assets.Manifest
	require.NoError(s.t, json.Unmarshal([]byte(s.publishedVersion(agent).ManifestJSON), &manifest))
	return manifest
}

// breakInstalledDefinition 篡改某 Agent 已安装 bundle 里的定义文件，返回恢复闭包。
// 清单（`bundle.json`）保持完好，`assets.Resolve` 仍能解析；逐文件哈希对不上，
// `assets.Verify` 必然拒绝——联动重发得到确定的 `failed`。
// breakInstalledDefinition corrupts one installed config file and returns a restore
// closure; the manifest stays intact so resolution succeeds while verification fails.
func (s *republishScene) breakInstalledDefinition(agent string) func() {
	s.t.Helper()
	dir, _ := s.installedBundle(agent)
	rel := assets.ConfigBundleRel(path.Join(agentsDirName, agent, agentDefinitionFileName))
	full := filepath.Join(dir, filepath.FromSlash(rel))
	original, err := os.ReadFile(full)
	require.NoError(s.t, err, "已安装 bundle 必须包含定义文件")
	require.NoError(s.t, os.WriteFile(full, []byte(republishTamperedBody), 0o600))
	return func() {
		require.NoError(s.t, os.WriteFile(full, original, 0o600), "恢复被篡改的已安装内容")
	}
}

// newRepublisher 按现场装配联动重发器：版本读取、操作台账、载荷表与资产根都是真的。
// newRepublisher wires the linkage republisher over the real repositories.
func (s *republishScene) newRepublisher() *skillRepublisher {
	return newSkillRepublisher(skillRepublishDeps{
		AssetRoot:     s.bundleRoot,
		WorkspaceRoot: s.root,
		Versions:      s.versions,
		Operations:    s.ops,
		Payloads:      s.dist,
		PayloadStore:  s.payloads,
		AgentGate:     core.NewAgentGate(),
		MaxBytes:      syncTestMaxBytes,
		Logger:        repairLogger(),
		Now:           s.clock.Now,
	})
}

// newRepairer 按现场装配运行期修复器（同一个实例跨轮复用 = 不重启）。
// newRepairer wires the runtime repairer; reusing the instance across rounds is the
// "no restart" condition.
func (s *republishScene) newRepairer(admin SkillRepublisher) *SkillRepairer {
	return NewSkillRepairer(SkillRepairerDeps{
		Distribution: s.dist,
		Versions:     s.versions,
		Admin:        admin,
		Logger:       repairLogger(),
		Now:          s.clock.Now,
		Batch:        republishRepairBatch,
		Interval:     republishRepairInterval,
	})
}

// repairAtStartup 模拟一次**重启**：全新建起重发器与修复器，走生产的
// `repairStaleBundleSkills`（Full 复位 + 串行排空）——不携带任何进程内状态。
// repairAtStartup simulates a restart through the production startup drain.
func (s *republishScene) repairAtStartup() (RepairReport, error) {
	return repairStaleBundleSkills(s.ctx, RepairDeps{
		Distribution:  s.dist,
		AssetRoot:     s.bundleRoot,
		WorkspaceRoot: s.root,
		Versions:      s.versions,
		Operations:    s.ops,
		Payloads:      s.dist,
		PayloadStore:  s.payloads,
		AgentGate:     core.NewAgentGate(),
		MaxBytes:      syncTestMaxBytes,
		Logger:        repairLogger(),
		Now:           s.clock.Now,
		Batch:         republishRepairBatch,
		Interval:      republishRepairInterval,
	})
}

// resultOf 取出某 Agent 的逐项结果；缺失直接失败——"逐项可见"本身就是判据。
// resultOf returns one agent's outcome; a missing entry fails the test.
func resultOf(t *testing.T, results []core.BundlePublishResult, agent string) core.BundlePublishResult {
	t.Helper()
	for _, result := range results {
		if result.Agent == agent {
			return result
		}
	}
	require.FailNowf(t, "逐 Agent 结果缺失", "结果里没有 %s：%v", agent, results)
	return core.BundlePublishResult{}
}

// 中文验收点：技能发布成功后，联动重发把逐 Agent 结果装进发布的 `publish` 字段——
// 成功的 Agent 已换到新技能版本、实际安装内容就是新正文；失败的 Agent 得到 `failed`
// 与固定原因，且**零发布**（指针与安装内容都不动）。没有依赖方的技能发布保持字段为
// nil（契约省略语义）。逆改点：联动结果不进发布结果 / 失败也推进指针 → 本用例红。
// Acceptance point: the contract field carries per-agent outcomes; a failed agent
// keeps both its pointer and its installed bytes untouched.
func TestSkillRepublishIntegration_PerAgentResultsVisible(t *testing.T) {
	t.Parallel()
	scene := newRepublishScene(t)
	admin := &ResourceAdmin{republisher: scene.newRepublisher(), logger: repairLogger()}

	betaBefore := scene.publishedVersion(republishAgentBeta).ID
	t.Cleanup(scene.breakInstalledDefinition(republishAgentBeta))

	result := admin.withSkillLinkage(scene.ctx, repairActor(), republishSkillName,
		core.ResourcePublishResult{Version: core.ResourceVersion{
			Kind: core.ResourceSkill, ResourceID: republishSkillName,
		}})
	require.Len(t, result.Publish, 2, "两个直接依赖该技能的 Agent 都必须逐项出现")

	alpha := resultOf(t, result.Publish, republishAgentAlpha)
	require.Equal(t, core.BundlePublishOK, alpha.Status, "重建成功的 Agent 必须是 ok")
	require.Empty(t, alpha.Error, "ok 不携带原因")

	beta := resultOf(t, result.Publish, republishAgentBeta)
	require.Equal(t, core.BundlePublishFailed, beta.Status, "已安装内容不可复算必须是 failed")
	require.Equal(t, "已安装资产不可复算", beta.Error, "失败原因必须是固定短语")

	// 成功侧：指针换到新版本，实际安装目录里的技能正文就是第 2 版。
	require.Equal(t, scene.skillVersionIDs[2],
		scene.publishedManifest(republishAgentAlpha).Skills[0].VersionID,
		"ok 的 Agent 必须引用新的技能版本")
	alphaDir, _ := scene.installedBundle(republishAgentAlpha)
	installed, err := os.ReadFile(filepath.Join(alphaDir,
		syncBundleSkillsDir, republishSkillName, "SKILL.md"))
	require.NoError(t, err, "已安装 bundle 必须含物化的技能正文")
	require.Contains(t, string(installed), "第 2 版",
		"安装内容必须是新版本正文，不是重建前的副本")

	// 失败侧：零发布——版本行与安装内容都停在重建前那一份。
	require.Equal(t, betaBefore, scene.publishedVersion(republishAgentBeta).ID,
		"failed 的 Agent 不得推进指针")
	require.Equal(t, scene.skillVersionIDs[1],
		scene.publishedManifest(republishAgentBeta).Skills[0].VersionID,
		"failed 的 Agent 仍引用旧技能版本")

	// 没有依赖方的技能发布：契约只在触发联动时给该字段，保持 nil。
	orphan := admin.withSkillLinkage(scene.ctx, repairActor(), "repub-unreferenced",
		core.ResourcePublishResult{})
	require.Nil(t, orphan.Publish, "没有依赖方时 publish 字段必须保持 nil")
}

// 中文验收点：同一个修复器实例（进程不重启）在故障解除后的下一轮把失败项修好：
// 第一轮 alpha 成功、beta 失败；恢复被篡改的文件并推进时钟越过退避后，第二轮只重试
// 仍落后的 beta 并成功——alpha 不因修复器存在而被重复发布。
// Acceptance point: the same repairer instance retries only the still-stale agent on
// the next round after the fault is cleared; no restart happens.
func TestSkillRepublishIntegration_NextRoundRepair(t *testing.T) {
	t.Parallel()
	scene := newRepublishScene(t)
	repairer := scene.newRepairer(scene.newRepublisher())

	restore := scene.breakInstalledDefinition(republishAgentBeta)

	first, err := repairer.RepairOnce(scene.ctx)
	require.NoError(t, err)
	require.Equal(t, 2, first.Attempted, "两个落后 Agent 都必须在第一轮被尝试")
	require.Equal(t, core.BundlePublishOK,
		resultOf(t, first.Results, republishAgentAlpha).Status)
	require.Equal(t, core.BundlePublishFailed,
		resultOf(t, first.Results, republishAgentBeta).Status)
	require.Equal(t, scene.skillVersionIDs[1],
		scene.publishedManifest(republishAgentBeta).Skills[0].VersionID,
		"第一轮失败后 beta 仍引用旧版本")

	alphaVersionID := scene.publishedVersion(republishAgentAlpha).ID

	// 故障解除；进程不重启，只等退避到期（30s 基数）→ 下一轮自愈。
	restore()
	scene.clock.Advance(republishRepairInterval + time.Second)

	second, err := repairer.RepairOnce(scene.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, second.Attempted, "只有仍落后的 beta 需要再修")
	require.Equal(t, core.BundlePublishOK, second.Results[0].Status)
	require.Equal(t, republishAgentBeta, second.Results[0].Agent)
	require.Equal(t, scene.skillVersionIDs[2],
		scene.publishedManifest(republishAgentBeta).Skills[0].VersionID,
		"下一轮修复后 beta 必须引用新的技能版本")
	require.Equal(t, alphaVersionID, scene.publishedVersion(republishAgentAlpha).ID,
		"已就绪的 Agent 不得被重复发布")
}

// 中文验收点：重启兜底**只在运行期没修到时**产生发布。运行期一轮收敛后，新装配的
// `repairStaleBundleSkills`（等价新进程）零尝试；随后发布技能第 3 版但运行期不再介入
// （模拟控制面在发布后、修复前退出），重启兜底把两个 Agent 一次排空到第 3 版。
// Acceptance point: the restart drain is idle when the runtime already converged and
// only picks up what the runtime missed.
func TestSkillRepublishIntegration_RestartIsFallbackOnly(t *testing.T) {
	t.Parallel()
	scene := newRepublishScene(t)
	repairer := scene.newRepairer(scene.newRepublisher())

	round, err := repairer.RepairOnce(scene.ctx)
	require.NoError(t, err)
	require.Equal(t, 2, round.Attempted)
	for _, result := range round.Results {
		require.Equal(t, core.BundlePublishOK, result.Status, "运行期一轮必须把两个都修好")
	}

	// 运行期已收敛：重启兜底无事可做，零发布。
	idle, err := scene.repairAtStartup()
	require.NoError(t, err)
	require.Equal(t, 0, idle.Attempted, "运行期已收敛时重启兜底不得产生发布")
	require.Empty(t, idle.Results)

	// 运行期没轮到（控制面在发布后、修复前退出）：直接重启，兜底必须补齐。
	scene.clock.Advance(2 * time.Second)
	scene.publishSkillVersion(3)
	restart, err := scene.repairAtStartup()
	require.NoError(t, err)
	require.Equal(t, 2, restart.Attempted, "兜底负责补齐运行期没有轮到的落后项")
	for _, result := range restart.Results {
		require.Equal(t, core.BundlePublishOK, result.Status)
	}
	for _, agent := range []string{republishAgentAlpha, republishAgentBeta} {
		require.Equal(t, scene.skillVersionIDs[3],
			scene.publishedManifest(agent).Skills[0].VersionID,
			"重启兜底后 %s 必须引用第 3 版技能", agent)
	}
}
