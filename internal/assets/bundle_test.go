package assets

// 本文件是 US-24 T027 / US7 的资产 bundle 验收面（模块卡 §2.3.9 / §4 的
// FR-24-24、FR-24-42、FR-24-44）。
//
// 判据全部落在**内容与路径**上，而不是"函数返回了 nil"：
//   - 往返：同一来源两次构建必须得到同一个 BundleHash，导出 / 安装后再复算一致；
//   - 不可变：截断的 bundle 被拒，且既有 hash 目录的字节一个都没变；
//   - 不漂移：装上新版之后，旧 hash 目录仍然解析得到旧内容（在途 Run 的守点）；
//   - 路径一致：Install.Dir、Resolve 返回值、执行期拼出的路径逐字相等且都含
//     `bundles/` 段。
//
// The judges assert content and paths, not merely "no error": identical rebuilds,
// immutable install directories, pinned-hash stability across a new install, and the
// three-way path identity required by FR-24-44.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// 测试用 Agent 身份：`ag_` + 26 位 Crockford Base32（无 I/L/O/U）。
	testAgentUID = core.AgentUID("ag_0123456789ABCDEFGHJKMNPQRS")
	// 测试用技能资源身份：`rs_` + 同一套字母表。
	testSkillUID = core.ResourceUID("rs_0123456789ABCDEFGHJKMNPQRS")
)

// bundleFixture 是一份可复用的来源树：配置文件 + 一个技能版本。
type bundleFixture struct {
	Agent      string
	ConfigRoot string
	ConfigFile map[string]string // 相对路径 → 内容
	SkillRoot  string
	SkillFiles map[string]string
	SkillRef   core.SkillVersionRef
}

// newBundleFixture 在临时目录里铺一份来源树，并按磁盘内容算出技能文件哈希表。
// 技能文件的哈希**由来源计算**：构建侧只比对、不重新定义发布记录。
func newBundleFixture(t *testing.T, agent string) *bundleFixture {
	t.Helper()
	base := t.TempDir()
	fixture := &bundleFixture{
		Agent:      agent,
		ConfigRoot: filepath.Join(base, "config"),
		ConfigFile: map[string]string{
			"AGENT.md":            "---\nname: " + agent + "\n---\n\n# 角色\n\n评审招标要求。\n",
			"bootstrap/policy.md": "# 策略\n\n只读检索。\n",
		},
		SkillRoot: filepath.Join(base, "skills", "tender"),
		SkillFiles: map[string]string{
			"SKILL.md":       "---\nname: tender\n---\n\n# 技能\n",
			"scripts/run.sh": "#!/bin/sh\necho tender\n",
		},
	}
	for rel, content := range fixture.ConfigFile {
		writeFixtureFile(t, fixture.ConfigRoot, rel, content)
	}
	// 技能文件清单按 path 升序构造：发布记录是规整形态（normalizeManifest 排序），而
	// Go map 遍历顺序随机，直接按 map 遍历会让 SkillRef.Files 时对时错（实测门禁
	// 第二条管道偶发 FAIL，见 run-state 失败台账）。哈希值本身不受顺序影响。
	// Build the file list in path order: the published record is canonicalized
	// (normalizeManifest sorts by path) and map iteration order is random.
	filePaths := make([]string, 0, len(fixture.SkillFiles))
	for rel := range fixture.SkillFiles {
		filePaths = append(filePaths, rel)
	}
	sortStrings(filePaths)
	files := make([]core.FileHash, 0, len(filePaths))
	for _, rel := range filePaths {
		content := fixture.SkillFiles[rel]
		writeFixtureFile(t, fixture.SkillRoot, rel, content)
		files = append(files, core.FileHash{
			Path: rel, SHA256: sha256Hex(content), Size: int64(len(content)),
		})
	}
	fixture.SkillRef = core.SkillVersionRef{
		ResourceVersionRef: core.ResourceVersionRef{
			Name:        "tender",
			ResourceUID: testSkillUID,
			VersionID:   11,
			VersionNo:   2,
			ContentHash: sha256Hex(strings.Join(fixture.SkillFileContents(), "\x00")),
		},
		Files: files,
	}
	return fixture
}

// SkillFileContents 按固定顺序返回技能文件内容，供 ContentHash 计算保持稳定。
func (f *bundleFixture) SkillFileContents() []string {
	paths := make([]string, 0, len(f.SkillFiles))
	for path := range f.SkillFiles {
		paths = append(paths, path)
	}
	sortStrings(paths)
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		out = append(out, f.SkillFiles[path])
	}
	return out
}

// ConfigPaths 按固定顺序返回配置文件相对路径。
func (f *bundleFixture) ConfigPaths() []string {
	paths := make([]string, 0, len(f.ConfigFile))
	for path := range f.ConfigFile {
		paths = append(paths, path)
	}
	sortStrings(paths)
	return paths
}

// spec 组装一次构建输入；`dest` 是本次构建目标目录。
func (f *bundleFixture) spec(dest string) BuildSpec {
	return BuildSpec{
		Agent:       f.Agent,
		AgentUID:    testAgentUID,
		ConfigHash:  "cfg-" + f.Agent,
		ConfigRoot:  f.ConfigRoot,
		ConfigFiles: f.ConfigPaths(),
		Skills:      []SkillSource{{Ref: f.SkillRef, Root: f.SkillRoot}},
		Dest:        dest,
	}
}

// TestAssets_BuildInstallVerifyRoundTrip 判 FR-24-24 的往返：构建 → 校验 →
// 安装 → 解析 → 导出 全程逐文件复算一致，且同一来源两次构建得到同一个 BundleHash。
func TestAssets_BuildInstallVerifyRoundTrip(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-requirements")
	work := t.TempDir()

	manifest, err := Build(ctx, fixture.spec(filepath.Join(work, "build-1")))
	require.NoError(t, err)
	require.Equal(t, FormatVersion, manifest.FormatVersion)
	require.Equal(t, fixture.Agent, manifest.Agent)
	require.Equal(t, testAgentUID, manifest.AgentUID)
	require.Len(t, manifest.BundleHash, hashHexLength)
	require.True(t, validHash(manifest.BundleHash), "BundleHash 必须是 64 位小写十六进制")
	require.Equal(t, []string{"AGENT.md", "bootstrap/policy.md"}, pathsOf(manifest.ConfigFiles),
		"配置文件清单必须按 path 升序")
	require.Len(t, manifest.Skills, 1)
	require.Equal(t, fixture.SkillRef, manifest.Skills[0])

	recomputed, err := computeBundleHash(manifest)
	require.NoError(t, err)
	require.Equal(t, manifest.BundleHash, recomputed, "BundleHash 必须可由清单复算")

	verified, err := Verify(ctx, filepath.Join(work, "build-1"))
	require.NoError(t, err)
	require.True(t, sameManifest(verified, manifest), "Verify 必须逐项还原清单")
	require.Equal(t, 4, totalFiles(manifest), "两个配置文件 + 两个技能文件")

	// 同一来源再次构建：哈希必须相同（内容寻址的前提）。
	again, err := Build(ctx, fixture.spec(filepath.Join(work, "build-2")))
	require.NoError(t, err)
	require.Equal(t, manifest.BundleHash, again.BundleHash, "同一来源两次构建必须得到同一个 BundleHash")

	assetRoot := filepath.Join(work, "assets")
	installed, err := Install(ctx, filepath.Join(work, "build-1"), assetRoot)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(assetRoot, bundlesDirName, fixture.Agent, manifest.BundleHash), installed.Dir)
	require.True(t, sameManifest(installed.Manifest, manifest))

	// 幂等安装：同一 bundle 再装一次返回同一目录，不重写。
	second, err := Install(ctx, filepath.Join(work, "build-1"), assetRoot)
	require.NoError(t, err)
	require.Equal(t, installed.Dir, second.Dir)

	// 安装目录里的清单与内容同样可复算。
	installedManifest, err := Verify(ctx, installed.Dir)
	require.NoError(t, err)
	require.True(t, sameManifest(installedManifest, manifest))

	// 导出到另一个根再校验：内容寻址的往返在跨目录时同样成立。
	exportDest := filepath.Join(work, "exported")
	require.NoError(t, Export(ctx, manifest, installed.Dir, exportDest))
	exportedManifest, err := Verify(ctx, exportDest)
	require.NoError(t, err)
	require.True(t, sameManifest(exportedManifest, manifest))

	// bundle.json 必须在 bundle 根，且可由 Resolve 解析出的目录读到。
	raw, err := os.ReadFile(filepath.Join(exportDest, manifestFileName))
	require.NoError(t, err)
	require.Contains(t, string(raw), manifest.BundleHash)
}

// TestAssets_TruncatedBundleLeavesOriginalIntact 判 FR-24-24 的拒收面：截断 / 缺文件 /
// 清单损坏一律拒绝，且**不破坏**既有 hash 目录（不可变）。
func TestAssets_TruncatedBundleLeavesOriginalIntact(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-technical")
	work := t.TempDir()

	bundleDir := filepath.Join(work, "build")
	manifest, err := Build(ctx, fixture.spec(bundleDir))
	require.NoError(t, err)
	assetRoot := filepath.Join(work, "assets")
	installed, err := Install(ctx, bundleDir, assetRoot)
	require.NoError(t, err)

	originalContent := readFile(t, filepath.Join(installed.Dir, configDirName, "AGENT.md"))
	require.Equal(t, fixture.ConfigFile["AGENT.md"], originalContent)

	// ① 截断：把一份拷贝里的 AGENT.md 写短（模拟传输中途断开）。
	truncated := filepath.Join(work, "truncated")
	copyTree(t, bundleDir, truncated)
	writeFixtureFile(t, truncated, configDirName+"/AGENT.md", "---\nname: bid-technical\n")
	_, err = Verify(ctx, truncated)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch, "截断的 bundle 必须按版本不符拒绝")
	_, err = Install(ctx, truncated, assetRoot)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch)
	require.Equal(t, originalContent, readFile(t, filepath.Join(installed.Dir, configDirName, "AGENT.md")),
		"被拒的安装不得改动既有 hash 目录")
	_, err = Verify(ctx, installed.Dir)
	require.NoError(t, err, "既有 hash 目录必须仍然可校验")

	// ② 缺文件：删掉一个技能文件。
	missing := filepath.Join(work, "missing")
	copyTree(t, bundleDir, missing)
	require.NoError(t, os.Remove(filepath.Join(missing, skillsDirName, "tender", "scripts", "run.sh")))
	_, err = Verify(ctx, missing)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch)

	// ③ 清单缺失：没有 bundle.json 的目录不是 bundle。
	noManifest := filepath.Join(work, "no-manifest")
	copyTree(t, bundleDir, noManifest)
	require.NoError(t, os.Remove(filepath.Join(noManifest, manifestFileName)))
	_, err = Verify(ctx, noManifest)
	require.ErrorIs(t, err, core.ErrAssetUnavailable, "看不见清单就不准入")

	// ④ 清单损坏：BundleHash 被改写成别的值（内容没变，但不可复算）。
	tampered := filepath.Join(work, "tampered")
	copyTree(t, bundleDir, tampered)
	wrong := strings.Repeat("0", hashHexLength)
	writeFixtureFile(t, tampered, manifestFileName,
		strings.Replace(readFile(t, filepath.Join(tampered, manifestFileName)), manifest.BundleHash, wrong, 1))
	_, err = Verify(ctx, tampered)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch)

	// ⑤ 目标目录已被别的内容占用：拒绝覆盖，且原内容一字不改。
	clashRoot := filepath.Join(work, "clash-assets")
	clashDir := filepath.Join(clashRoot, bundlesDirName, fixture.Agent, manifest.BundleHash)
	require.NoError(t, os.MkdirAll(clashDir, 0o750))
	writeFixtureFile(t, clashDir, manifestFileName, `{"format_version":1,"agent":"forged","bundle_hash":"`)
	_, err = Install(ctx, bundleDir, clashRoot)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch)
	require.Equal(t, `{"format_version":1,"agent":"forged","bundle_hash":"`,
		readFile(t, filepath.Join(clashDir, manifestFileName)), "既存目标目录不得被覆盖")
}

// TestAssets_OldRunUsesPinnedHashAfterNewInstall 判 FR-24-42 / C-28：装上新版之后，
// 在途 Run 固定的旧 hash 仍然解析到旧目录与旧内容，**不**随新安装漂移。
func TestAssets_OldRunUsesPinnedHashAfterNewInstall(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-lead")
	work := t.TempDir()
	assetRoot := filepath.Join(work, "assets")

	oldManifest, err := Build(ctx, fixture.spec(filepath.Join(work, "build-old")))
	require.NoError(t, err)
	oldInstalled, err := Install(ctx, filepath.Join(work, "build-old"), assetRoot)
	require.NoError(t, err)

	// 在途 Run 在准入时固定了这个 hash（模拟 manifest.asset_bundle_hash）。
	pinnedHash := oldManifest.BundleHash
	pinnedDir, err := Resolve(ctx, assetRoot, fixture.Agent, pinnedHash)
	require.NoError(t, err)
	require.Equal(t, oldInstalled.Dir, pinnedDir)
	require.Equal(t, fixture.ConfigFile["AGENT.md"],
		readFile(t, filepath.Join(pinnedDir, configDirName, "AGENT.md")))

	// 管理员改了 AGENT.md 并安装新版。
	fixture.ConfigFile["AGENT.md"] = "---\nname: bid-lead\n---\n\n# 角色（第二版）\n"
	writeFixtureFile(t, fixture.ConfigRoot, "AGENT.md", fixture.ConfigFile["AGENT.md"])
	newManifest, err := Build(ctx, fixture.spec(filepath.Join(work, "build-new")))
	require.NoError(t, err)
	require.NotEqual(t, oldManifest.BundleHash, newManifest.BundleHash, "内容变化必须改变 BundleHash")
	newInstalled, err := Install(ctx, filepath.Join(work, "build-new"), assetRoot)
	require.NoError(t, err)
	require.NotEqual(t, oldInstalled.Dir, newInstalled.Dir, "新版必须装进另一个不可变目录")

	// 旧 hash 仍然解析到旧目录、旧内容；新 hash 解析到新内容。
	stillPinned, err := Resolve(ctx, assetRoot, fixture.Agent, pinnedHash)
	require.NoError(t, err)
	require.Equal(t, oldInstalled.Dir, stillPinned, "旧 Run 固定的 hash 不得被新安装改写")
	require.Equal(t,
		"---\nname: bid-lead\n---\n\n# 角色\n\n评审招标要求。\n",
		readFile(t, filepath.Join(stillPinned, configDirName, "AGENT.md")),
		"旧 hash 目录的内容必须保持第一版")
	require.Equal(t, fixture.ConfigFile["AGENT.md"],
		readFile(t, filepath.Join(newInstalled.Dir, configDirName, "AGENT.md")))

	// 解析未安装的 hash：失败关闭，不回落成"最新版本"。
	_, err = Resolve(ctx, assetRoot, fixture.Agent, strings.Repeat("a", hashHexLength))
	require.ErrorIs(t, err, core.ErrAssetUnavailable)
}

// TestAssets_InstallResolveAndPinnedReaderUseSamePath 判 FR-24-44 的资产层一半：
// Install 返回的 Dir、Resolve 的返回值与准入写进 `pin.Dir` 的路径**逐字相等**且都含
// `bundles/` 段；随后用**生产的执行期读取入口** `OpenBundleReader` 从该 pin 读回
// AGENT.md，证明这条路径上读到的确实是 bundle 字节，而不是字符串巧合。
//
// 执行期"只读 bundle、不读工作副本"的整链判据在 internal/app 的
// TestAdmission_ExecutionReadsPinnedBundleNotWorkspace（生产构建 → 安装 → 准入 →
// runSnapshotBuilder.Build，工作副本改写后快照仍是 bundle 内容）。本用例**不**手拼
// executionDir 常量冒充"执行期路径"（评审 IDX-23）。
// The asset-layer half of FR-24-44: install/resolve/pin identity, then the production
// reader reads the pinned bytes; the chain-level judge lives in internal/app.
func TestAssets_InstallResolveAndPinnedReaderUseSamePath(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "daily-report")
	work := t.TempDir()
	assetRoot := filepath.Join(work, "assets")

	manifest, err := Build(ctx, fixture.spec(filepath.Join(work, "build")))
	require.NoError(t, err)
	installed, err := Install(ctx, filepath.Join(work, "build"), assetRoot)
	require.NoError(t, err)

	resolvedDir, err := Resolve(ctx, assetRoot, fixture.Agent, manifest.BundleHash)
	require.NoError(t, err)

	require.Equal(t, installed.Dir, resolvedDir, "Install 与 Resolve 的路径必须逐字相同")
	for _, candidate := range []string{installed.Dir, resolvedDir} {
		require.Contains(t, pathSegments(candidate), bundlesDirName,
			"资产路径必须含 %s/ 段：%s", bundlesDirName, candidate)
		require.FileExists(t, filepath.Join(candidate, manifestFileName))
	}

	// 准入把 Resolve 的返回值原样写进 pin（FR-24-44 的第三处）；执行期读取只认 pin.Dir。
	pin := core.AssetBundlePin{
		Hash: manifest.BundleHash, Dir: resolvedDir,
		ConfigFiles: manifest.ConfigFiles, Skills: manifest.Skills,
	}
	reader, err := OpenBundleReader(pin)
	require.NoError(t, err)
	got, err := reader.ReadConfig("AGENT.md")
	require.NoError(t, err)
	require.Equal(t, fixture.ConfigFile["AGENT.md"], string(got),
		"执行期读取面必须读回 bundle 里的原文")

	// 反向对照：工作副本被改写后，pin 仍读安装时的字节（目录不可变）；
	// 清单未覆盖的路径一律拒绝，不静默读取目录里的其他内容。
	writeFixtureFile(t, fixture.ConfigRoot, "AGENT.md", "---\nname: daily-report\n---\n\n# 角色（工作副本改过）\n")
	still, err := reader.ReadConfig("AGENT.md")
	require.NoError(t, err)
	require.Equal(t, fixture.ConfigFile["AGENT.md"], string(still),
		"工作副本改写不得影响已固定的 bundle 目录")
	_, err = reader.ReadConfig("bootstrap/other.md")
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch, "清单外的文件必须拒绝")
}

// TestAssets_CommandRootVerifyExportInstall 判 CLI 入口的三个子命令都能跑通，
// 且 install 必须把已安装结果交给发布步骤（不得只装本地）。
func TestAssets_CommandRootVerifyExportInstall(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-requirements")
	work := t.TempDir()
	bundleDir := filepath.Join(work, "build")
	_, err := Build(ctx, fixture.spec(bundleDir))
	require.NoError(t, err)

	// verify
	out, err := CommandRoot(ctx, Deps{}, []string{"verify", bundleDir})
	require.NoError(t, err)
	require.Contains(t, out, "校验通过")
	require.Contains(t, out, fixture.Agent)

	// verify 一个不存在 / 损坏的目录：非空错误且不回显文件内容
	_, err = CommandRoot(ctx, Deps{}, []string{"verify", filepath.Join(work, "absent")})
	require.Error(t, err)

	// export
	exportDest := filepath.Join(work, "exported")
	out, err = CommandRoot(ctx, Deps{}, []string{"export", bundleDir, exportDest})
	require.NoError(t, err)
	require.Contains(t, out, "已导出")
	require.DirExists(t, exportDest)
	_, err = CommandRoot(ctx, Deps{}, []string{"export", bundleDir, bundleDir})
	require.Error(t, err, "源与目标相同必须拒绝（会覆盖自身）")

	// install：必须调用注入的发布步骤，并把安装目录原样交给它
	assetRoot := filepath.Join(work, "assets")
	var published []Installed
	deps := Deps{
		AssetRoot: assetRoot,
		Publish: func(_ context.Context, installed Installed) (int64, error) {
			published = append(published, installed)
			return 7, nil
		},
	}
	out, err = CommandRoot(ctx, deps, []string{"install", bundleDir})
	require.NoError(t, err)
	require.Contains(t, out, "已安装并发布")
	require.Contains(t, out, "published_version_id=7")
	require.Len(t, published, 1)
	require.Equal(t,
		filepath.Join(assetRoot, bundlesDirName, fixture.Agent, published[0].Manifest.BundleHash),
		published[0].Dir)

	// 缺发布接线 → 失败关闭（不允许"只装本地"）
	_, err = CommandRoot(ctx, Deps{AssetRoot: assetRoot}, []string{"install", bundleDir})
	require.Error(t, err)
	require.Contains(t, err.Error(), "发布未接线")

	// 空资产根 → 失败关闭
	_, err = CommandRoot(ctx, Deps{Publish: deps.Publish}, []string{"install", bundleDir})
	require.ErrorIs(t, err, core.ErrAssetUnavailable)

	// 发布失败：本地目录保留、错误如实上抛（不宣称已发布）
	deps.Publish = func(context.Context, Installed) (int64, error) {
		return 0, errors.New("指针 CAS 丢失更新")
	}
	_, err = CommandRoot(ctx, deps, []string{"install", bundleDir})
	require.Error(t, err)
	require.Contains(t, err.Error(), "发布指针未推进")
}

// TestAssets_CommandRootRejectsBadArgs 判命令面的错误路径：缺子命令 / 参数个数不对 /
// 未知子命令都必须报错并给出用法，而不是静默成功。
func TestAssets_CommandRootRejectsBadArgs(t *testing.T) {
	ctx := context.Background()
	cases := [][]string{
		{},
		{"verify"},
		{"verify", "a", "b"},
		{"export"},
		{"export", "a"},
		{"install"},
		{"install", "a", "b"},
		{"install", "--agent"},                // 缺值
		{"install", "--agent="},               // 空值
		{"install", "a", "--agent", "writer"}, // 两种形态互斥
		{"install", "--agent", "w", "--agent", "x"},
		{"install", "--force", "a"}, // 未知选项
		{"latest"},
	}
	for _, args := range cases {
		_, err := CommandRoot(ctx, Deps{}, args)
		require.Error(t, err, "参数 %v 必须被拒绝", args)
	}
}

// TestAssets_BuildRejectsIncompleteSpec 判构建前的输入校验：来源不完整时**不创建**
// 任何文件（写面错误必须发生在建目录之前）。
func TestAssets_BuildRejectsIncompleteSpec(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-requirements")
	dest := filepath.Join(t.TempDir(), "build")

	base := func() BuildSpec { return fixture.spec(dest) }

	cases := map[string]func(BuildSpec) BuildSpec{
		"缺 Agent 名":     func(spec BuildSpec) BuildSpec { spec.Agent = ""; return spec },
		"点开头 Agent":     func(spec BuildSpec) BuildSpec { spec.Agent = ".hidden"; return spec },
		"Agent 带斜杠":     func(spec BuildSpec) BuildSpec { spec.Agent = "a/b"; return spec },
		"非法 Agent UID":  func(spec BuildSpec) BuildSpec { spec.AgentUID = "rs_0123456789ABCDEFGHJKMNPQRS"; return spec },
		"缺 config_hash": func(spec BuildSpec) BuildSpec { spec.ConfigHash = ""; return spec },
		"缺配置文件":         func(spec BuildSpec) BuildSpec { spec.ConfigFiles = nil; return spec },
		"缺目标目录":         func(spec BuildSpec) BuildSpec { spec.Dest = ""; return spec },
		"技能无来源目录":       func(spec BuildSpec) BuildSpec { spec.Skills[0].Root = ""; return spec },
		"技能文件清单为空":      func(spec BuildSpec) BuildSpec { spec.Skills[0].Ref.Files = nil; return spec },
		"技能名非法":         func(spec BuildSpec) BuildSpec { spec.Skills[0].Ref.Name = "../evil"; return spec },
	}
	for name, mutate := range cases {
		_, err := Build(ctx, mutate(base()))
		require.Error(t, err, "用例 %s 必须被拒绝", name)
	}
	require.NoDirExists(t, dest, "输入不合法时不得创建目标目录")
}

// TestAssets_RejectsPathEscape 判路径逃逸拒绝：清单 / 构建输入里的 `..`、绝对路径、
// 反斜杠、盘符都不得落到文件系统。
func TestAssets_RejectsPathEscape(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-requirements")

	for _, escape := range []string{"../evil.md", "/etc/passwd", `config\AGENT.md`, "C:/AGENT.md", "a//b.md", "./AGENT.md"} {
		spec := fixture.spec(filepath.Join(t.TempDir(), "build"))
		spec.ConfigFiles = []string{escape}
		_, err := Build(ctx, spec)
		require.Error(t, err, "路径 %q 必须被拒绝", escape)
	}

	// 清单里写逃逸路径同样被拒（Verify 不信任 bundle.json）。
	bundleDir := filepath.Join(t.TempDir(), "build")
	manifest, err := Build(ctx, fixture.spec(bundleDir))
	require.NoError(t, err)
	forged := manifest
	forged.ConfigFiles = []core.FileHash{{
		Path: "../escape.md", SHA256: manifest.ConfigFiles[0].SHA256, Size: manifest.ConfigFiles[0].Size,
	}}
	hash, err := computeBundleHash(forged)
	require.NoError(t, err)
	forged.BundleHash = hash
	require.NoError(t, writeManifest(bundleDir, forged))
	_, err = Verify(ctx, bundleDir)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch, "清单里的逃逸路径必须被拒绝")
}

// TestAssets_RejectsMalformedManifest 判清单的结构判据：格式版本、未知字段、排序
// 不变量都必须 fail-closed——读路径不"尽力理解"非法清单。
func TestAssets_RejectsMalformedManifest(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-requirements")

	cases := map[string]string{
		"格式版本不支持": `{"format_version":2,"agent":"bid-requirements","bundle_hash":"` + strings.Repeat("a", hashHexLength) + `"}`,
		"非法 JSON": `{"format_version":1,`,
		"未知字段":    `{"format_version":1,"agent":"bid-requirements","bundle_hash":"` + strings.Repeat("a", hashHexLength) + `","extra":1}`,
		// 首个 JSON 值之后还有字节：等价于"文件被追加 / 损坏"，必须拒收而不是
		// 静默忽略（json.Decoder 默认会停在首个值后）。
		"尾随垃圾":        `{"format_version":1,"agent":"bid-requirements","bundle_hash":"` + strings.Repeat("a", hashHexLength) + `"}x`,
		"尾随完整 JSON 值": `{"format_version":1,"agent":"bid-requirements","bundle_hash":"` + strings.Repeat("a", hashHexLength) + `"}{}`,
	}
	for name, raw := range cases {
		dir := filepath.Join(t.TempDir(), "bundle")
		require.NoError(t, os.MkdirAll(dir, 0o750))
		writeFixtureFile(t, dir, manifestFileName, raw)
		_, err := Verify(ctx, dir)
		require.Error(t, err, "用例 %s 必须被拒绝", name)
	}

	// 排序不变量：把已排序的配置文件清单倒过来写成清单 → 拒绝。
	bundleDir := filepath.Join(t.TempDir(), "build")
	manifest, err := Build(ctx, fixture.spec(bundleDir))
	require.NoError(t, err)
	unsorted := manifest
	unsorted.ConfigFiles = []core.FileHash{manifest.ConfigFiles[1], manifest.ConfigFiles[0]}
	hash, err := computeBundleHash(unsorted)
	require.NoError(t, err)
	unsorted.BundleHash = hash
	require.NoError(t, writeManifest(bundleDir, unsorted))
	_, err = Verify(ctx, bundleDir)
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch)

	// 目录名与内容不符：把 bundle 目录改名成另一个 hash → Resolve 拒绝。
	// 用一份全新的构建（上面那份的清单已被改写，安装它本来就是错的）。
	cleanDir := filepath.Join(t.TempDir(), "clean")
	_, err = Build(ctx, fixture.spec(cleanDir))
	require.NoError(t, err)
	assetRoot := filepath.Join(t.TempDir(), "assets")
	installed, err := Install(ctx, cleanDir, assetRoot)
	require.NoError(t, err)
	renamed := filepath.Join(assetRoot, bundlesDirName, fixture.Agent, strings.Repeat("b", hashHexLength))
	require.NoError(t, os.Rename(installed.Dir, renamed))
	_, err = Resolve(ctx, assetRoot, fixture.Agent, strings.Repeat("b", hashHexLength))
	require.ErrorIs(t, err, core.ErrAssetVersionMismatch, "目录名必须与复算哈希一致")
}

// TestAssets_RejectsSymlinkSources 判不跟随符号链接：来源文件是链接时拒绝，
// 避免把资产根之外的文件打进 bundle。
func TestAssets_RejectsSymlinkSources(t *testing.T) {
	ctx := context.Background()
	fixture := newBundleFixture(t, "bid-requirements")
	target := filepath.Join(fixture.ConfigRoot, "AGENT.md")
	link := filepath.Join(fixture.ConfigRoot, "linked.md")
	if err := os.Symlink(target, link); err != nil {
		// 理由：Windows 默认策略下创建符号链接需要管理员或开发者模式；
		// 本机不具备时该分支无法验证，按"跳过 + 依据"登记，不算通过。
		t.Skipf("当前环境不允许创建符号链接（%v）——符号链接拒绝分支未在本平台验证", err)
	}
	spec := fixture.spec(filepath.Join(t.TempDir(), "build"))
	spec.ConfigFiles = []string{"linked.md"}
	_, err := Build(ctx, spec)
	require.Error(t, err, "来源是符号链接时必须拒绝（不跟随）")
	require.Contains(t, err.Error(), "符号链接")
}

// —— 测试助手 ——

// writeFixtureFile 在 root 下写一个相对路径文件（自动建父目录）。
func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
}

// readFile 读一个绝对路径文件。
func readFile(t *testing.T, full string) string {
	t.Helper()
	raw, err := os.ReadFile(full)
	require.NoError(t, err)
	return string(raw)
}

// copyTree 逐文件复制一棵小树（测试夹具用；不保留链接与权限位）。
func copyTree(t *testing.T, src, dest string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o750)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o600)
	}))
}

// sha256Hex 返回内容的 sha256 小写十六进制。
func sha256Hex(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// pathsOf 取文件清单里的路径序列。
func pathsOf(files []core.FileHash) []string {
	out := make([]string, 0, len(files))
	for _, file := range files {
		out = append(out, file.Path)
	}
	return out
}

// pathSegments 把路径拆成段（跨平台用正斜杠）。
func pathSegments(candidate string) []string {
	return strings.Split(filepath.ToSlash(candidate), "/")
}

// sortStrings 是夹具用的升序排序。
func sortStrings(values []string) { slices.Sort(values) }
