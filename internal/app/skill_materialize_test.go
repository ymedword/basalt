package app

// 本文件是 US-25C §2.3.4 / FR-25C-07 的**技能物化**定向判据（单元层，无数据库）：
//   - 路径形态：发布记录的 `storage_path` 是契约字面量 `.basalt/versions/skills/<name>/<uid>/<no>`，
//     工作区根本身就是 `.basalt`，物化落点是投影后的 `versions/skills/...`（不得出现双前缀）；
//   - 逐文件哈希：物化字节与发布记录逐项相符，换入后目录冻结为只读；
//   - 工作副本不动：`skills/<name>/**` 在同步前后哈希不变，也不参与任何判定；
//   - 失败不回落：版本行读不到 / 已物化目录被篡改一律 `blocked`，绝不退到工作副本；
//   - 种子缺该技能版本时仍可读：工作副本根本没有该技能，Entry 仍注册、L1 卡片与
//     L2 `read_file` 都取自已物化的发布目录。
//
// 物化对象是**真实**的 `assets.Build` 产物与真实 `workspace` 装载器 / 装配器：路径形态与
// 哈希核对正是判据本身，不能用假路径绕过。
// Unit judges for skill materialization: path shape, per-file digests, an untouched work
// copy, no fallback on failure, and readability when the seed lacks the version.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// materializeWorkCopyMD 是工作副本里的同名技能：内容与发布版本**不同**，任何"回落工作
// 副本"的实现都会让断言读到这段文字。
const materializeWorkCopyMD = "---\nname: writer\ndescription: 工作副本说明\n---\n\n工作副本正文\n"

// materializeSkillSource 是一份技能源文件：路径相对技能根 + 内容。
// materializeSkillSource is one skill source file.
type materializeSkillSource struct{ path, content string }

// materializeScene 是一份「发布记录 + 依赖它的 Agent bundle」现场：`assets.Build` 的真实
// 产物、载荷表里的 bundle、分发读面里的技能版本行，以及供断言用的期望值。
// materializeScene is one published-skill scene with everything the judges need.
type materializeScene struct {
	agent       string
	skill       string
	skillUID    core.ResourceUID
	versionID   int64
	logicalDir  string // 契约字面量：`.basalt/versions/skills/<name>/<uid>/<no>`
	relDir      string // 工作区相对形态：`versions/skills/<name>/<uid>/<no>`
	bundleHash  string
	buildDir    string
	configFiles []core.FileHash
	record      core.SkillVersionRecord
	contents    map[string]string // 技能文件路径 → 内容
	hashes      map[string]core.FileHash
}

// materializeFileHash 按发布记录的形态算一份文件哈希（路径 + sha256 + 大小）。
// materializeFileHash renders one file entry the way a publish record carries it.
func materializeFileHash(path, content string) core.FileHash {
	sum := sha256.Sum256([]byte(content))
	return core.FileHash{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content))}
}

// requireOnlyEntries 断言目录里恰好只有给定子项（用来看暂存目录是否随 rename 消失）。
// requireOnlyEntries asserts a directory holds exactly the given children.
func requireOnlyEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	sort.Strings(want)
	require.Equal(t, want, names, "%s 的子项不符（暂存目录必须在换入后消失）", dir)
}

// requirePathAbsent 断言路径不存在：失败路径不得留下半成品目录。
// requirePathAbsent asserts the path does not exist at all.
func requirePathAbsent(t *testing.T, target string) {
	t.Helper()
	_, err := os.Lstat(target)
	require.ErrorIs(t, err, os.ErrNotExist, "%s 不得存在", target)
}

// addSkillBundle 砌一份声明了技能的真实 bundle：技能源文件按发布记录逐项复制进 bundle，
// 版本行与载荷同时入分发读面。每个场景各自生成正文，后一次调用必然得到新的 bundle 哈希。
// addSkillBundle builds a real bundle that declares one skill and registers its payload.
func (f *assetSyncFixture) addSkillBundle(agent string, uid core.AgentUID, skill string,
	skillUID core.ResourceUID, versionID int64, versionNo int, updatedAt int64,
) *materializeScene {
	f.t.Helper()
	f.sequence++
	sources := []materializeSkillSource{
		{path: "SKILL.md", content: "---\nname: " + skill + "\ndescription: 写入技能\n---\n\n发布版本正文\n"},
		{path: "references/notes.md", content: "写作参考 " + skill + "\n"},
	}
	contents := make(map[string]string, len(sources))
	hashes := make(map[string]core.FileHash, len(sources))
	refs := make([]core.FileHash, 0, len(sources))
	for _, source := range sources {
		file := materializeFileHash(source.path, source.content)
		contents[source.path] = source.content
		hashes[source.path] = file
		refs = append(refs, file)
	}
	// 发布记录的文件清单按 path 严格升序（`validateManifest` 的结构不变量）。
	sort.Slice(refs, func(i, j int) bool { return refs[i].Path < refs[j].Path })

	// 技能源目录：`assets.Build` 从这里按 file.Path 逐项复制并复核哈希。
	sourceRoot := filepath.Join(f.buildRoot, fmt.Sprintf("skill-%03d-%s", f.sequence, skill))
	for _, source := range sources {
		writeWorkspaceFile(f.t, sourceRoot, source.path, source.content)
	}

	logicalDir, err := workspace.SkillVersionLogicalDir(skill, skillUID, versionNo)
	require.NoError(f.t, err)
	relDir, err := workspace.PublishedVersionRelDir(logicalDir)
	require.NoError(f.t, err)

	rel := "agents/" + agent + "/AGENT.md"
	writeWorkspaceFile(f.t, f.workspaceRoot, rel, fmt.Sprintf(
		"---\nname: %s\ndescription: 物化夹具\nprovider:\n  name: deepseek\n  model: deepseek-chat\n"+
			"skills:\n  - %s\n---\n\n技能物化夹具正文 %d\n", agent, skill, f.sequence))
	dest := filepath.Join(f.buildRoot, fmt.Sprintf("build-%03d-%s", f.sequence, agent))
	manifest, err := assets.Build(context.Background(), assets.BuildSpec{
		Agent:       agent,
		AgentUID:    uid,
		ConfigHash:  "cfg-" + agent,
		ConfigRoot:  f.workspaceRoot,
		ConfigFiles: []string{rel},
		Skills: []assets.SkillSource{{
			Ref: core.SkillVersionRef{
				ResourceVersionRef: core.ResourceVersionRef{
					Name:        skill,
					ResourceUID: skillUID,
					VersionID:   versionID,
					VersionNo:   versionNo,
					ContentHash: "skill-content-" + skill,
				},
				Files: refs,
			},
			Root: sourceRoot,
		}},
		Dest: dest,
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
	record := core.SkillVersionRecord{Name: skill, VersionID: versionID, StoragePath: logicalDir, Files: refs}
	f.dist.skills[versionID] = record
	f.lastBuildDir = dest
	return &materializeScene{
		agent:       agent,
		skill:       skill,
		skillUID:    skillUID,
		versionID:   versionID,
		logicalDir:  logicalDir,
		relDir:      relDir,
		bundleHash:  manifest.BundleHash,
		buildDir:    dest,
		configFiles: manifest.ConfigFiles,
		record:      record,
		contents:    contents,
		hashes:      hashes,
	}
}

// readyProviderCatalog 是本轮资源快照：声明 `deepseek` 的 bundle 才能注册。
// readyProviderCatalog is the round's prepared provider snapshot.
func readyProviderCatalog(context.Context) (CatalogReport, error) {
	return CatalogReport{Providers: []string{"deepseek"}}, nil
}

// recordingEntryBuilder 记录本轮交给 Entry 构建的「技能 → 发布目录」映射，判定委托给
// 真实装载器：路径形态由真实 `publishedSkillResolver` 判卷，测试只旁证映射字面量。
// recordingEntryBuilder delegates to the real loader and records the published map.
type recordingEntryBuilder struct {
	inner     BundleEntryBuilder
	published map[string]string
}

func (r *recordingEntryBuilder) EntryFromBundle(root string, installed assets.Installed,
	prepared PreparedResources, published map[string]string,
) (core.AgentEntry, error) {
	r.published = published
	return r.inner.EntryFromBundle(root, installed, prepared, published)
}

// 中文验收点：同步把技能物化到 `versions/skills/<name>/<uid>/<no>`，逐文件哈希与发布记录
// 相符、目录冻结只读、工作副本哈希不变；Entry 注册走真实装载器（技能判定只认已物化目录）。
// 逆改点（模块卡 §4.4 / FR-25C-07）：只装 bundle 不物化 → Entry 判 `skill_not_found`、
// 下面所有物化断言全红；写成 `filepath.Join(workspaceRoot, storage_path)` → 双前缀目录。
// Acceptance point: materialization lands on the projected path, matches every recorded
// digest, freezes read-only, and leaves the work copy untouched; the real loader registers.
func TestAssetSync_MaterializesSkills(t *testing.T) {
	t.Run("首次安装物化到发布版本目录", func(t *testing.T) {
		fixture := newAssetSyncFixture(t)
		unlockTreeOnCleanup(t, fixture.workspaceRoot) // 冻结后的只读权限要能随临时目录清理
		scene := fixture.addSkillBundle("writer-agent", syncUID(1), "writer",
			fixtureResourceUID(t, core.ResourceSkill, "writer"), 7, 1, 1000)
		// 工作副本里已有同名技能：既不得被写，也不得被当作注册依据。
		writeWorkspaceFile(t, fixture.workspaceRoot, "skills/writer/SKILL.md", materializeWorkCopyMD)
		workCopyBefore := snapshotTree(t, filepath.Join(fixture.workspaceRoot, "skills"))

		recorder := &recordingEntryBuilder{inner: workspace.NewAgentLoader(workspace.AgentLoaderOptions{})}
		sync := fixture.buildSync(func(deps *AssetSyncDeps) {
			deps.Loader = recorder
			deps.Providers = readyProviderCatalog
		})
		report := fixture.syncOnce(sync)
		item := requireSyncItem(t, report, scene.agent)
		require.Equal(t, syncStateReady, item.State)
		require.Equal(t, []string{scene.skill}, item.Skills)
		require.Empty(t, item.BlockedReason)
		require.Equal(t, scene.bundleHash, item.InstalledHash)

		// 路径形态：契约字面量投影成工作区相对目录，工作区根即 `.basalt`。
		require.Equal(t, ".basalt/versions/skills/writer/"+string(scene.skillUID)+"/1", scene.logicalDir)
		require.Equal(t, "versions/skills/writer/"+string(scene.skillUID)+"/1", scene.relDir)
		requirePathAbsent(t, filepath.Join(fixture.workspaceRoot, ".basalt"))
		require.Equal(t, scene.relDir, recorder.published[scene.skill],
			"Entry 构建拿到的工作区相对发布目录必须是投影形态")

		// 逐文件哈希：物化字节与发布记录逐项相符。
		publishedRoot := filepath.Join(fixture.workspaceRoot, filepath.FromSlash(scene.relDir))
		for path, want := range scene.contents {
			target := filepath.Join(publishedRoot, filepath.FromSlash(path))
			raw, err := os.ReadFile(target)
			require.NoError(t, err, "物化文件必须存在：%s", path)
			require.Equal(t, want, string(raw))
			require.Equal(t, scene.hashes[path].SHA256, sha256Hex(t, raw))
		}
		// 暂存目录必须随 rename 消失（同一父目录里只剩版本目录本身）。
		requireOnlyEntries(t, filepath.Dir(publishedRoot), "1")

		// 冻结为只读：后续轮次不得就地改写已发布目录。
		if os.Geteuid() != 0 {
			skillMD := filepath.Join(publishedRoot, "SKILL.md")
			require.Error(t, os.WriteFile(skillMD, []byte("篡改"), 0o644), "已发布版本文件必须只读")
			raw, err := os.ReadFile(skillMD)
			require.NoError(t, err)
			require.Equal(t, scene.contents["SKILL.md"], string(raw), "被拒的写入不得改动字节")
		}

		// 工作副本不动，且注册表里的技能来自发布版本目录而不是它。
		require.Equal(t, workCopyBefore, snapshotTree(t, filepath.Join(fixture.workspaceRoot, "skills")),
			"同步不得写工作副本")
		require.Equal(t, []string{"registry", "scheduler"}, fixture.order)
		require.Len(t, fixture.registry.entries, 1)
		require.Equal(t, []string{scene.skill}, fixture.registry.entries[0].Profile.Skills)
		require.Contains(t, string(fixture.registry.entries[0].Definition), "技能物化夹具正文",
			"Entry 定义取自已安装 bundle")
	})

	t.Run("复用分支仍物化", func(t *testing.T) {
		fixture := newAssetSyncFixture(t)
		unlockTreeOnCleanup(t, fixture.workspaceRoot)
		scene := fixture.addSkillBundle("writer-agent", syncUID(1), "writer",
			fixtureResourceUID(t, core.ResourceSkill, "writer"), 7, 1, 1000)
		// 上一进程装好目录后崩溃：载荷表没有这一行，只有 `assets.Resolve` 能命中。
		fixture.dist.dropPayload(scene.bundleHash)
		_, err := assets.Install(context.Background(), scene.buildDir, fixture.assetRoot)
		require.NoError(t, err)

		recorder := &recordingEntryBuilder{inner: workspace.NewAgentLoader(workspace.AgentLoaderOptions{})}
		sync := fixture.buildSync(func(deps *AssetSyncDeps) {
			deps.Loader = recorder
			deps.Providers = readyProviderCatalog
		})
		report := fixture.syncOnce(sync)
		require.Equal(t, syncStateReady, requireSyncItem(t, report, scene.agent).State)
		require.Zero(t, fixture.dist.payloadReads[scene.bundleHash], "复用分支不得再读载荷")
		require.Equal(t, scene.relDir, recorder.published[scene.skill],
			"复用分支同样要物化——否则崩溃窗口里的目录永久缺技能")
		require.FileExists(t, filepath.Join(fixture.workspaceRoot,
			filepath.FromSlash(scene.relDir), "SKILL.md"))
	})

	t.Run("就绪条目不重读物化但仍参与集合替换", func(t *testing.T) {
		fixture := newAssetSyncFixture(t)
		unlockTreeOnCleanup(t, fixture.workspaceRoot)
		scene := fixture.addSkillBundle("writer-agent", syncUID(1), "writer",
			fixtureResourceUID(t, core.ResourceSkill, "writer"), 7, 1, 1000)

		sync := fixture.buildSync(func(deps *AssetSyncDeps) {
			deps.Loader = workspace.NewAgentLoader(workspace.AgentLoaderOptions{})
			deps.Providers = readyProviderCatalog
		})
		require.Equal(t, syncStateReady, requireSyncItem(t, fixture.syncOnce(sync), scene.agent).State)
		require.Equal(t, 1, fixture.dist.skillReads[scene.versionID])

		second := fixture.syncOnce(sync)
		require.Equal(t, syncStateReady, requireSyncItem(t, second, scene.agent).State)
		require.Equal(t, 1, fixture.dist.skillReads[scene.versionID],
			"ready 条目不再进入处理队列，不得重复读版本行")
		require.Equal(t, 2, fixture.registry.calls, "ready 条目每轮仍参与注册表替换")
		require.Equal(t, 2, fixture.scheduler.calls, "ready 条目每轮仍参与调度集合替换")
	})
}

// 中文验收点：版本行读不到（发布记录缺失）或已物化目录被篡改时，该 Agent 判 `blocked`、
// 不出现在注册表、不产生 Entry，**任何情况下都不回落到工作副本**；失败项留在工作集，
// 退避到期后在运行期自愈、不需要重启。
// 逆改点：把 `materializeSkills` 的错误吞掉并在 `published` 里回落到 `skills/<name>` →
// 下面的 `blocked` 断言与"工作副本未被动用"断言同时红。
// Acceptance point: a missing version row or a tampered directory blocks the agent without
// ever falling back to the work copy; the failed item self-heals on a later round.
func TestAssetSync_MaterializeFailureDoesNotFallBack(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	unlockTreeOnCleanup(t, fixture.workspaceRoot)
	scene := fixture.addSkillBundle("writer-agent", syncUID(1), "writer",
		fixtureResourceUID(t, core.ResourceSkill, "writer"), 7, 1, 1000)
	// 工作副本存在且内容不同：任何回落都会让"未就绪"看起来像成功。
	writeWorkspaceFile(t, fixture.workspaceRoot, "skills/writer/SKILL.md", materializeWorkCopyMD)

	// 发布记录缺失：bundle 声明了该技能，但版本行读不到。
	delete(fixture.dist.skills, scene.versionID)

	sync := fixture.buildSync(func(deps *AssetSyncDeps) {
		deps.Loader = workspace.NewAgentLoader(workspace.AgentLoaderOptions{})
		deps.Providers = readyProviderCatalog
	})
	report := fixture.syncOnce(sync)
	item := requireSyncItem(t, report, scene.agent)
	require.Equal(t, syncStateBlocked, item.State)
	require.Equal(t, syncBlockedSkills, item.BlockedReason)
	require.Empty(t, item.Skills, "未就绪的条目不得报告技能")
	require.Zero(t, fixture.registry.calls, "未就绪的条目不得进入注册表")
	require.Zero(t, fixture.scheduler.calls, "未就绪的条目不得进入调度集合")
	requirePathAbsent(t, filepath.Join(fixture.workspaceRoot, filepath.FromSlash(scene.relDir)))

	readiness := sync.Readiness()[scene.agent]
	require.Empty(t, readiness.ReadyHash, "未就绪没有就绪哈希")
	require.Equal(t, syncBlockedSkills, readiness.BlockedReason)

	// 工作副本必须原样还在（它既不参与判定，也不被当作"已经好了"）。
	raw, err := os.ReadFile(filepath.Join(fixture.workspaceRoot, "skills", "writer", "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, materializeWorkCopyMD, string(raw))

	// 修复后运行期自愈：失败项留在工作集，退避到期即重试，不需要重启。
	fixture.dist.skills[scene.versionID] = scene.record
	fixture.clock.Advance(2 * time.Second)
	second := fixture.syncOnce(sync)
	selfHealed := requireSyncItem(t, second, scene.agent)
	require.Equal(t, syncStateReady, selfHealed.State)
	require.Equal(t, []string{scene.skill}, selfHealed.Skills)
	raw, err = os.ReadFile(filepath.Join(fixture.workspaceRoot, filepath.FromSlash(scene.relDir), "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, scene.contents["SKILL.md"], string(raw), "自愈后物化的是发布版本正文")
}

// 中文验收点：已物化目录按发布记录逐文件核对——不一致即失败，**不得覆盖、不得就地修补**
// （已发布目录不可覆盖）；新 bundle 重发时晚到的内容不能改写已冻结的版本正文。
// 逆改点：复用分支跳过 `verifyMaterializedSkill` 直接返回 → 篡改内容被当作成功，断言红。
// Acceptance point: a tampered published directory fails verification and is never
// overwritten by a later republish of the same version.
func TestAssetSync_MaterializedDirIsNotOverwritten(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	unlockTreeOnCleanup(t, fixture.workspaceRoot)
	scene := fixture.addSkillBundle("writer-agent", syncUID(1), "writer",
		fixtureResourceUID(t, core.ResourceSkill, "writer"), 7, 1, 1000)

	sync := fixture.buildSync(func(deps *AssetSyncDeps) {
		deps.Loader = workspace.NewAgentLoader(workspace.AgentLoaderOptions{})
		deps.Providers = readyProviderCatalog
	})
	require.Equal(t, syncStateReady, requireSyncItem(t, fixture.syncOnce(sync), scene.agent).State)

	// 篡改已物化目录（等价的磁盘损坏 / 旧内容混入）。
	tampered := filepath.Join(fixture.workspaceRoot, filepath.FromSlash(scene.relDir), "SKILL.md")
	if os.Geteuid() != 0 {
		require.NoError(t, os.Chmod(filepath.Dir(tampered), 0o755))
		require.NoError(t, os.Chmod(tampered, 0o644))
		require.NoError(t, os.WriteFile(tampered, []byte(materializeWorkCopyMD), 0o644))
	}

	// 同一技能版本、新 bundle 重发（bundle 哈希变化 → 该 Agent 重新处理）。
	fixture.addSkillBundle("writer-agent", syncUID(1), "writer",
		fixtureResourceUID(t, core.ResourceSkill, "writer"), 7, 1, 2000)
	report := fixture.syncOnce(sync)
	item := requireSyncItem(t, report, scene.agent)
	require.Equal(t, syncStateBlocked, item.State)
	require.Equal(t, syncBlockedSkills, item.BlockedReason)

	raw, err := os.ReadFile(tampered)
	require.NoError(t, err)
	if os.Geteuid() != 0 {
		require.Equal(t, materializeWorkCopyMD, string(raw), "已物化目录不得被覆盖或就地修补")
	}
}

// 中文验收点：只装 bundle 不物化技能时，运行期快照组装失败关闭；同步物化之后，L1 卡片给出
// 发布真路径、快照清单仍记逻辑路径且哈希取发布字节，L2 `read_file` 只放行**本次 Run 绑定**的
// 发布目录——种子工作区没有该技能也照样可读（不靠重导种子）。
// 逆改点：技能卡片改读 `.basalt/skills/**` → 卡片行与 L2 读取都指向工作副本，断言红。
// Acceptance point: the run snapshot and L2 reads resolve against the materialized
// publication only, and they work when the seed lacks the skill entirely.
func TestRun_ReadsL2FromMaterializedSkill(t *testing.T) {
	fixture := newAssetSyncFixture(t)
	unlockTreeOnCleanup(t, fixture.workspaceRoot)
	scene := fixture.addSkillBundle("writer-agent", syncUID(1), "writer",
		fixtureResourceUID(t, core.ResourceSkill, "writer"), 7, 1, 1000)
	// 种子工作区里**没有**该技能的任何副本：本用例只允许读已物化的发布版本。
	requirePathAbsent(t, filepath.Join(fixture.workspaceRoot, "skills", "writer"))

	builder := newRunSnapshotBuilder(workspace.NewContextLoader(nil), fixture.workspaceRoot, fixture.clock.Now)
	entry := core.AgentEntry{
		Name:       scene.agent,
		Registered: true,
		Profile: &core.Profile{
			Name:     scene.agent,
			Provider: core.ProviderRef{Name: "deepseek", Model: "deepseek-chat"},
			Skills:   []string{scene.skill},
		},
	}

	// 未物化：准入固定的发布目录不存在 → 快照组装失败关闭，不回落到工作副本。
	_, err := builder.Build(context.Background(), entry, core.ResolvedResources{
		AssetBundle: core.AssetBundlePin{Hash: scene.bundleHash, Dir: scene.buildDir, ConfigFiles: scene.configFiles},
		SkillDirs:   map[string]string{scene.skill: scene.logicalDir},
	})
	require.ErrorContains(t, err, "读取技能定义", "未物化时快照组装必须失败关闭")

	// 同步：安装 bundle + 物化技能。
	sync := fixture.buildSync(func(deps *AssetSyncDeps) {
		deps.Loader = workspace.NewAgentLoader(workspace.AgentLoaderOptions{})
		deps.Providers = readyProviderCatalog
	})
	require.Equal(t, syncStateReady, requireSyncItem(t, fixture.syncOnce(sync), scene.agent).State)

	// 运行期只认准入固定的已安装目录（`assets.Resolve` + `assets.Verify` 是生产路径）。
	installedDir, err := assets.Resolve(context.Background(), fixture.assetRoot, scene.agent, scene.bundleHash)
	require.NoError(t, err)
	manifest, err := assets.Verify(context.Background(), installedDir)
	require.NoError(t, err)
	installed := assets.Installed{Manifest: manifest, Dir: installedDir}

	loaded, err := workspace.NewAgentLoader(workspace.AgentLoaderOptions{}).EntryFromBundle(
		fixture.workspaceRoot, installed,
		workspace.PreparedResources{Providers: []string{"deepseek"}},
		map[string]string{scene.skill: scene.relDir})
	require.NoError(t, err)
	require.True(t, loaded.Registered, "种子缺技能时仍按已物化的发布目录注册")
	require.Equal(t, []string{scene.skill}, loaded.Profile.Skills)

	snapshot, err := builder.Build(context.Background(), loaded, core.ResolvedResources{
		AssetBundle: core.AssetBundlePin{Hash: manifest.BundleHash, Dir: installedDir, ConfigFiles: manifest.ConfigFiles},
		SkillDirs:   map[string]string{scene.skill: scene.logicalDir},
	})
	require.NoError(t, err)
	require.Contains(t, snapshot.SectionOne, "- writer：写入技能（"+scene.logicalDir+"/SKILL.md）",
		"L1 卡片展示发布真路径")
	require.NotContains(t, snapshot.SectionOne, ".basalt/skills/writer/SKILL.md", "卡片行不得指向工作副本")
	manifestHashesOf := manifestHashes(snapshot)
	require.Equal(t, scene.hashes["SKILL.md"].SHA256, manifestHashesOf[".basalt/skills/writer/SKILL.md"],
		"清单记逻辑路径，哈希取发布版本的字节")
	require.NotContains(t, manifestHashesOf, scene.logicalDir+"/SKILL.md", "发布真路径不进清单")

	// L2：`read_file` 只放行本次 Run 绑定的发布目录。
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{Root: fixture.workspaceRoot})
	read := tool.NewReadFileTool(policy, fixture.workspaceRoot)
	env := core.ToolEnv{
		RunID:          "run-us25c-1",
		SessionID:      "sess-us25c-1",
		AgentName:      scene.agent,
		AgentDir:       filepath.Join(fixture.workspaceRoot, "agents", scene.agent),
		MaxOutputBytes: 256 * 1024,
		SkillDirs:      []string{scene.logicalDir},
		RunBound:       true,
	}
	l2Path := scene.logicalDir + "/references/notes.md"
	result, err := read.Execute(context.Background(), env, json.RawMessage(fmt.Sprintf(`{"path":%q}`, l2Path)))
	require.NoError(t, err, "已物化的 L2 正文必须读得到")
	require.Equal(t, scene.contents["references/notes.md"], result.Content)

	// 没有绑定 → 失败关闭（不因为目录存在就放行）。
	unbound := env
	unbound.SkillDirs = nil
	_, err = read.Execute(context.Background(), unbound, json.RawMessage(fmt.Sprintf(`{"path":%q}`, l2Path)))
	require.ErrorContains(t, err, "读取目标不在可读根之内")

	// Run 内不读技能工作副本：补上工作副本后仍然拒绝。
	writeWorkspaceFile(t, fixture.workspaceRoot, "skills/writer/SKILL.md", materializeWorkCopyMD)
	_, err = read.Execute(context.Background(), env,
		json.RawMessage(`{"path":".basalt/skills/writer/SKILL.md"}`))
	require.ErrorContains(t, err, "技能工作副本在运行中不可读")

	// 声明了技能却缺发布目录映射 → 快照失败关闭，不回落工作副本。
	_, err = builder.Build(context.Background(), loaded, core.ResolvedResources{
		AssetBundle: core.AssetBundlePin{Hash: manifest.BundleHash, Dir: installedDir, ConfigFiles: manifest.ConfigFiles},
		SkillDirs:   map[string]string{},
	})
	require.ErrorContains(t, err, "缺少发布目录")
}
