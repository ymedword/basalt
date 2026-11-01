package dbmigrate

// 本文件是**工作区资产清单**的纯单元判卷（不依赖 PostgreSQL）：白名单归类与排除、
// 前后两次盘点比对、未归类文件的逐条处置与封装、打包产物复核、三元组绑定与漂移报告。
// 需要真实库与真实名册的取证在 dbmigrate 的集成用例与 CLI 层完成（T070）。
// Unit-level checks for the asset manifest: classification, exclusions, the double inventory,
// per-entry review, packaged-content verification and the triple binding.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	ws "github.com/ymedword/basalt/internal/workspace"
)

// assetFixtureSpecs 是一份形状接近真实部署的白名单：引导文件 + 五个业务目录里的三个。
func assetFixtureSpecs() []AssetSpec {
	return []AssetSpec{
		{Path: "AGENTS.md", Kind: AssetKindAgentDef},
		{Path: "SOUL.md", Kind: AssetKindAgentDef},
		{Path: "agents", Kind: AssetKindAgentDef, RequiredFor: []string{"biz-daily"}},
		{Path: "skills", Kind: AssetKindScript},
		{Path: "memory", Kind: AssetKindState, RequiredFor: []string{"biz-daily"}},
		{Path: "control", Kind: AssetKindState},
	}
}

// newAssetFixture 建一个最小但形状真实的工作区：业务文件、未归类文件与全部必须排除的
// 敏感 / 派生条目。
func newAssetFixture(t *testing.T) (string, []AssetSpec) {
	t.Helper()
	root := t.TempDir()
	assetWriteFile(t, root, "AGENTS.md", "agents\n")
	assetWriteFile(t, root, "SOUL.md", "soul\n")
	assetWriteFile(t, root, "agents/biz-daily/AGENT.md", "agent\n")
	assetWriteFile(t, root, "agents/biz-daily/runs/2026-10-26/daily.md", "daily\n")
	assetWriteFile(t, root, "skills/report/SKILL.md", "skill\n")
	assetWriteFile(t, root, "memory/MEMORY.md", "memory\n")
	assetWriteFile(t, root, "control/schedule.yaml", "cron: 0 0 9 * * *\n")
	assetWriteFile(t, root, "notes/2026-10-26.md", "note\n")

	// 必须排除的具体条目：库与旁路、名册、密钥、环境文件、终端转录、日志与开发密钥。
	assetWriteFile(t, root, "basalt.db", "SQLite format 3\x00")
	assetWriteFile(t, root, "basalt.db-wal", "wal")
	assetWriteFile(t, root, "basalt.db-shm", "shm")
	assetWriteFile(t, root, "basalt.db-journal", "journal")
	assetWriteFile(t, root, "instance.json", `{"version":1,"instance_id":"abc"}`)
	assetWriteFile(t, root, "admin-token", "token")
	assetWriteFile(t, root, "master.key", "key")
	assetWriteFile(t, root, ".env", "API_KEY=placeholder")
	assetWriteFile(t, root, ".env.local", "API_KEY=placeholder")
	assetWriteFile(t, root, "settings.clixml", "<xml/>")
	assetWriteFile(t, root, "logs/2026-10-26.log", "log\n")
	assetWriteFile(t, root, "dev-env/secrets/pg.yaml", "dsn: placeholder\n")
	assetWriteFile(t, root, ".git/config", "[core]\n")
	return root, assetFixtureSpecs()
}

// newAssetBusinessOnlyFixture 是没有任何未归类文件的工作区（核查状态应为 not_required）。
func newAssetBusinessOnlyFixture(t *testing.T) (string, []AssetSpec) {
	t.Helper()
	root, specs := newAssetFixture(t)
	require.NoError(t, os.RemoveAll(filepath.Join(root, "notes")))
	return root, specs
}

// assetWriteFile 按相对路径写文件，自动建目录。
func assetWriteFile(t *testing.T, root, rel, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target, []byte(content), 0o644))
}

// assetBuild 跑一次采集并要求成功。
func assetBuild(t *testing.T, root string, specs []AssetSpec) AssetManifest {
	t.Helper()
	manifest, err := BuildAssetManifest(context.Background(), root, specs)
	require.NoError(t, err)
	return manifest
}

// assetEntry 取一条清单条目；缺项直接判失败（测试断言依赖它存在）。
func assetEntry(t *testing.T, m AssetManifest, rel string) AssetEntry {
	t.Helper()
	for _, entry := range m.Entries {
		if entry.RelPath == rel {
			return entry
		}
	}
	t.Fatalf("资产清单里没有 %s", rel)
	return AssetEntry{}
}

// assetEntryPaths 取全部条目的相对路径。
func assetEntryPaths(m AssetManifest) []string {
	paths := make([]string, 0, len(m.Entries))
	for _, entry := range m.Entries {
		paths = append(paths, entry.RelPath)
	}
	return paths
}

// assetDBFile 取源库文件身份。
func assetDBFile(identities []DBFileIdentity, name string) (DBFileIdentity, bool) {
	for _, identity := range identities {
		if identity.Name == name {
			return identity, true
		}
	}
	return DBFileIdentity{}, false
}

// assetMirrorWorkspace 把清单里的全部条目复制进 `<out>/review/`：受限核查区是工作区的
// **完整镜像**，未归类文件同样先落在这里，处置后才决定入不入包。
func assetMirrorWorkspace(t *testing.T, root, out string, m AssetManifest) {
	t.Helper()
	for _, entry := range m.Entries {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(entry.RelPath)))
		require.NoError(t, err)
		target := filepath.Join(out, assetReviewDirName, filepath.FromSlash(entry.RelPath))
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
		require.NoError(t, os.WriteFile(target, data, 0o600))
	}
}

// assetPackageFiles 列出可交付包里的相对路径。
func assetPackageFiles(t *testing.T, out string) []string {
	t.Helper()
	base := filepath.Join(out, assetPackageDirName)
	var files []string
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(base, p)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, walkErr)
	return files
}

// TestAssetManifest_BuildClassifiesAndExcludes 盘点一次：白名单命中的进 business、
// 未归类的单列、库 / 锁 / 名册 / 密钥 / 日志一律不进清单，源库文件身份同时采集。
// 逆改点：把 `excludedFile` 判断改成只排除 basalt.db → 敏感条目断言红；
// 未归类不置 pending → ReviewStatus 断言红。
func TestAssetManifest_BuildClassifiesAndExcludes(t *testing.T) {
	root, specs := newAssetFixture(t)
	manifest := assetBuild(t, root, specs)

	require.Equal(t, AssetFormatVersion, manifest.FormatVersion)
	require.NotEmpty(t, manifest.WorkspaceID)
	require.False(t, manifest.GeneratedAt.IsZero())
	// 批次 / 实例 / 快照归 db export 在拿到名册与快照后补写：本包不猜。
	require.Empty(t, manifest.MigrationBatchID)
	require.Empty(t, manifest.SourceInstanceID)
	require.Empty(t, manifest.SourceInstanceIDOrigin)
	require.Empty(t, manifest.SourceSnapshotSHA256)

	paths := assetEntryPaths(manifest)
	require.Len(t, paths, 8, "七个白名单文件 + 一个未归类文件")
	for _, excluded := range []string{
		"basalt.db", "basalt.db-wal", "basalt.db-shm", "basalt.db-journal",
		"basalt.lock", "instance.json", "admin-token", "master.key",
		".env", ".env.local", "settings.clixml",
		"logs/2026-10-26.log", "dev-env/secrets/pg.yaml", ".git/config",
	} {
		require.NotContains(t, paths, excluded, "%s 不得进资产清单", excluded)
	}

	agent := assetEntry(t, manifest, "AGENTS.md")
	require.Equal(t, AssetClassBusiness, agent.Class)
	require.Equal(t, AssetKindAgentDef, agent.Kind)
	require.Equal(t, AssetDispositionIncluded, agent.Disposition)
	require.NotEmpty(t, agent.SHA256)

	note := assetEntry(t, manifest, "notes/2026-10-26.md")
	require.Equal(t, AssetClassUnclassified, note.Class)
	require.Empty(t, note.Kind)
	require.Empty(t, note.Disposition, "未归类文件在核查前不得有处置")

	require.Equal(t, AssetReviewPending, manifest.ReviewStatus)
	require.Len(t, manifest.UnclassifiedDigest, 64)
	require.Equal(t, []string{"biz-daily"}, manifest.Agents())

	// 条目按 rel_path 严格升序：两份清单对账靠它，顺序不许依赖遍历实现。
	for i := 1; i < len(manifest.Entries); i++ {
		require.Less(t, manifest.Entries[i-1].RelPath, manifest.Entries[i].RelPath)
	}

	// 源库文件身份：basalt.db 与 -wal 记尺寸 + 摘要，-shm 只记存在性。
	mainDB, present := assetDBFile(manifest.SourceDBFiles, "basalt.db")
	require.True(t, present)
	require.True(t, mainDB.Present)
	require.NotEmpty(t, mainDB.SHA256)
	require.Equal(t, int64(len("SQLite format 3\x00")), mainDB.Size)

	wal, present := assetDBFile(manifest.SourceDBFiles, "basalt.db-wal")
	require.True(t, present)
	require.True(t, wal.Present)
	require.NotEmpty(t, wal.SHA256)

	shm, present := assetDBFile(manifest.SourceDBFiles, "basalt.db-shm")
	require.True(t, present)
	require.Empty(t, shm.SHA256, "-shm 只记存在性，不记摘要")
	require.Zero(t, shm.Size, "-shm 只记存在性，不记尺寸")
}

// TestAssetManifest_LongestPrefixWins 更长路径的申报覆盖其祖先：runs/ 下的日报记 report、
// 同一 Agent 的 AGENT.md 仍记 agent-def，Agents() 取并集。
func TestAssetManifest_LongestPrefixWins(t *testing.T) {
	root := t.TempDir()
	assetWriteFile(t, root, "agents/biz-daily/AGENT.md", "agent\n")
	assetWriteFile(t, root, "agents/biz-daily/runs/2026-10-26/daily.md", "daily\n")

	manifest := assetBuild(t, root, []AssetSpec{
		{Path: "agents", Kind: AssetKindAgentDef, RequiredFor: []string{"biz-daily"}},
		{Path: "agents/biz-daily/runs", Kind: AssetKindReport, RequiredFor: []string{"biz-daily", "repo-digest"}},
	})

	daily := assetEntry(t, manifest, "agents/biz-daily/runs/2026-10-26/daily.md")
	require.Equal(t, AssetKindReport, daily.Kind)
	require.ElementsMatch(t, []string{"biz-daily", "repo-digest"}, daily.RequiredFor)

	definition := assetEntry(t, manifest, "agents/biz-daily/AGENT.md")
	require.Equal(t, AssetKindAgentDef, definition.Kind)
	require.Equal(t, []string{"biz-daily"}, definition.RequiredFor)

	require.Equal(t, []string{"biz-daily", "repo-digest"}, manifest.Agents())
}

// TestAssetManifest_NoUnclassifiedIsNotRequired 没有未归类文件时核查状态为 not_required，
// 且未归类摘要为空串——"没有"与"有但摘要为空"必须可分。
func TestAssetManifest_NoUnclassifiedIsNotRequired(t *testing.T) {
	root, specs := newAssetBusinessOnlyFixture(t)
	manifest := assetBuild(t, root, specs)

	require.Equal(t, AssetReviewNotRequired, manifest.ReviewStatus)
	require.Empty(t, manifest.UnclassifiedDigest)
}

// TestAssetManifest_RejectsUnsafeSpecs 白名单不接受绝对路径、盘符、UNC、反斜杠、`..`、
// 未知类别、排除项与重复条目。
// 逆改点：去掉 `normalizeRelPath` 的 `..` 段检查 → 越界条目断言红。
func TestAssetManifest_RejectsUnsafeSpecs(t *testing.T) {
	root := t.TempDir()
	assetWriteFile(t, root, "AGENTS.md", "agents\n")

	cases := []struct {
		name   string
		specs  []AssetSpec
		expect string
	}{
		{"empty", nil, "白名单"},
		{"escaping", []AssetSpec{{Path: "../outside", Kind: AssetKindScript}}, ".."},
		{"absolute", []AssetSpec{{Path: "/etc/passwd", Kind: AssetKindScript}}, "绝对路径"},
		{"unc", []AssetSpec{{Path: "//server/share", Kind: AssetKindScript}}, "绝对路径"},
		{"drive", []AssetSpec{{Path: "C:/secrets", Kind: AssetKindScript}}, "盘符"},
		{"backslash", []AssetSpec{{Path: "agents\\biz-daily", Kind: AssetKindScript}}, "正斜杠"},
		{"blank", []AssetSpec{{Path: "  ", Kind: AssetKindScript}}, "为空"},
		{"unknown kind", []AssetSpec{{Path: "AGENTS.md", Kind: "mystery"}}, "未知"},
		{"excluded", []AssetSpec{{Path: "logs", Kind: AssetKindScript}}, "排除项"},
		{"excluded file", []AssetSpec{{Path: "basalt.db", Kind: AssetKindSampleDB}}, "排除项"},
		{"duplicate", []AssetSpec{
			{Path: "AGENTS.md", Kind: AssetKindAgentDef},
			{Path: "./AGENTS.md", Kind: AssetKindAgentDef},
		}, "两次"},
		{"blank agent", []AssetSpec{{Path: "AGENTS.md", Kind: AssetKindAgentDef, RequiredFor: []string{" "}}}, "空 Agent"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := BuildAssetManifest(context.Background(), root, testCase.specs)
			require.Error(t, err)
			require.Contains(t, err.Error(), testCase.expect)
		})
	}
}

// TestAssetManifest_WorkspaceBusy 活实例持锁时拒绝采集：取不到锁就没有静止窗口，
// 不能拿"看起来没变"当静止。
// 逆改点：去掉 AcquireInstanceLock 调用 → 本用例不再报 workspace_busy 而红。
func TestAssetManifest_WorkspaceBusy(t *testing.T) {
	root, specs := newAssetFixture(t)

	lock, err := ws.AcquireInstanceLock(root)
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Release()) }()

	_, err = BuildAssetManifest(context.Background(), root, specs)
	require.ErrorIs(t, err, ErrWorkspaceBusy)
}

// TestAssetManifest_RejectsSymlink 符号链接一律拒绝、不跟随（含指向根外的链接）。
func TestAssetManifest_RejectsSymlink(t *testing.T) {
	root := t.TempDir()
	assetWriteFile(t, root, "AGENTS.md", "agents\n")
	if err := os.Symlink(filepath.Join(root, "AGENTS.md"), filepath.Join(root, "link.md")); err != nil {
		t.Skipf("本机不允许创建符号链接，无法验证拒绝路径: %v", err)
	}

	_, err := BuildAssetManifest(context.Background(), root, []AssetSpec{{Path: "AGENTS.md", Kind: AssetKindAgentDef}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "符号链接")
}

// TestAssetManifest_RejectsHardlink Unix 上硬链接会让"一个路径一条记录"失真，拒绝；
// Windows 不采集 Nlink，判据只覆盖 Unix（报告里如实写未验证）。
func TestAssetManifest_RejectsHardlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不采集 Nlink，硬链接判据只在 Unix 上生效")
	}
	root := t.TempDir()
	assetWriteFile(t, root, "AGENTS.md", "agents\n")
	require.NoError(t, os.Link(filepath.Join(root, "AGENTS.md"), filepath.Join(root, "copy.md")))

	_, err := BuildAssetManifest(context.Background(), root, []AssetSpec{{Path: "AGENTS.md", Kind: AssetKindAgentDef}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "硬链接")
}

// TestAssets_ChangedDuringCaptureIsRejected 验收点：A1 与 A2 之间工作区被改动时，本次
// 静止窗口作废——**不得**产出清单，错误必须是 `ErrAssetsChangedDuringCapture`。
// 三个形态各判一次：改未归类文件（摘要变）、新增未归类文件（条目集合变）、改业务文件
// （归类内容变）。作废的窗口也不得留下半份清单给下游误用。
// 逆改点：去掉 sameCapture 比对、直接返回 second → 三种形态全红（拿到清单而非错误）。
func TestAssets_ChangedDuringCaptureIsRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, root string)
	}{
		{
			name: "改写未归类文件",
			mutate: func(t *testing.T, root string) {
				assetWriteFile(t, root, "notes/2026-10-26.md", "note changed in window\n")
			},
		},
		{
			name: "新增未归类文件",
			mutate: func(t *testing.T, root string) {
				assetWriteFile(t, root, "notes/2026-10-27.md", "late note\n")
			},
		},
		{
			name: "改写业务文件",
			mutate: func(t *testing.T, root string) {
				assetWriteFile(t, root, "agents/biz-daily/AGENT.md", "agent v2\n")
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, specs := newAssetFixture(t)
			manifest, err := BuildAssetManifestAround(context.Background(), root, specs, AssetWindow{
				Between: func(context.Context) error {
					testCase.mutate(t, root)
					return nil
				},
			})
			require.ErrorIs(t, err, ErrAssetsChangedDuringCapture, "窗口内改动必须作废本次盘点")
			require.Empty(t, manifest.Entries, "作废的窗口不得产出可用清单")
			require.Zero(t, manifest.UnclassifiedDigest, "作废的窗口不得留下摘要")
		})
	}
}

// TestAssets_CaptureWindowStableKeepsOrder 验收点：回调顺序是判据的一部分——
// `AfterLock` 必须在实例锁已持有、A1 之前执行（此刻再取一次锁应被挡），`Between` 在
// A1 与 A2 之间执行；窗口内无人改动时清单照常产出，回调本身不被误判成改动；函数返回后
// 锁已释放（可以再次取到）。
// 逆改点：把 AfterLock 挪到取锁之前 → 回调里再取锁成功、断言红；把 Between 挪到 A2
// 之后 → 顺序断言红；锁不释放 → 返回后的再取锁断言红。
func TestAssets_CaptureWindowStableKeepsOrder(t *testing.T) {
	root, specs := newAssetFixture(t)
	var order []string

	manifest, err := BuildAssetManifestAround(context.Background(), root, specs, AssetWindow{
		AfterLock: func(context.Context) error {
			order = append(order, "after_lock")
			contender, lockErr := ws.AcquireInstanceLock(root)
			require.Error(t, lockErr, "AfterLock 执行时实例锁必须已被窗口持有")
			if lockErr == nil {
				require.NoError(t, contender.Release())
			}
			// 位置探针：AfterLock 在 A1 **之前**执行，所以它此刻写下的条目 A1 与 A2 都
			// 看得见、窗口不判变化；一旦被挪到 A1 之后，这次写入就会落进两次盘点之间。
			assetWriteFile(t, root, "notes/2026-10-25.md", "written after lock, before A1\n")
			return nil
		},
		Between: func(context.Context) error {
			order = append(order, "between")
			return nil
		},
	})
	require.NoError(t, err, "窗口内无改动时不得误判")
	require.Equal(t, []string{"after_lock", "between"}, order, "回调顺序固定为 锁 → A1 → Between → A2")
	require.NotEmpty(t, manifest.Entries)
	require.Equal(t, AssetReviewPending, manifest.ReviewStatus)

	released, err := ws.AcquireInstanceLock(root)
	require.NoError(t, err, "函数返回后实例锁必须已释放")
	require.NoError(t, released.Release())
}

// TestAssets_WindowCallbackErrorAborts 验收点：回调返回错误时窗口中止、错误原样上抛，
// 不继续盘点也不产出清单——`db export` 在窗口里做快照，快照失败不能当成"没变化"。
// 逆改点：忽略 AfterLock / Between 的返回错误 → 本用例红（拿到 nil 错误与清单）。
func TestAssets_WindowCallbackErrorAborts(t *testing.T) {
	sentinel := errors.New("窗口回调失败：快照写不进去")

	for _, testCase := range []struct {
		name   string
		window AssetWindow
	}{
		{name: "AfterLock 失败", window: AssetWindow{AfterLock: func(context.Context) error { return sentinel }}},
		{name: "Between 失败", window: AssetWindow{Between: func(context.Context) error { return sentinel }}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, specs := newAssetFixture(t)
			manifest, err := BuildAssetManifestAround(context.Background(), root, specs, testCase.window)
			require.ErrorIs(t, err, sentinel)
			require.Empty(t, manifest.Entries)
		})
	}
}

// TestDuplicatePath_CaseInsensitivity Windows 按大小写不敏感查重：`Agents/a.md` 与
// `agents/a.md` 视为冲突；其他平台只有逐字相同才冲突。
func TestDuplicatePath_CaseInsensitivity(t *testing.T) {
	entries := []AssetEntry{{RelPath: "Agents/a.md"}, {RelPath: "agents/a.md"}}

	require.NotEmpty(t, duplicatePath(entries, true))
	require.Empty(t, duplicatePath(entries, false))
	require.Empty(t, duplicatePath(nil, true))
}

// TestUnclassifiedDigest_OrderIndependent 未归类摘要只由内容决定：书写顺序不改变摘要，
// 内容 / 尺寸变化才改变摘要；没有未归类文件时是空串。
func TestUnclassifiedDigest_OrderIndependent(t *testing.T) {
	first := []AssetEntry{
		{RelPath: "notes/a.md", Class: AssetClassUnclassified, Size: 1, SHA256: "aa"},
		{RelPath: "notes/b.md", Class: AssetClassUnclassified, Size: 2, SHA256: "bb"},
	}
	second := []AssetEntry{first[1], first[0]}
	require.Equal(t, unclassifiedDigest(first), unclassifiedDigest(second))

	changed := []AssetEntry{
		{RelPath: "notes/a.md", Class: AssetClassUnclassified, Size: 1, SHA256: "aa"},
		{RelPath: "notes/b.md", Class: AssetClassUnclassified, Size: 2, SHA256: "cc"},
	}
	require.NotEqual(t, unclassifiedDigest(first), unclassifiedDigest(changed))

	require.Empty(t, unclassifiedDigest([]AssetEntry{{RelPath: "AGENTS.md", Class: AssetClassBusiness, Size: 1, SHA256: "aa"}}))
}

// TestSameCapture_DetectsEveryDrift A1 与 A2 的逐项比对：路径集合、尺寸、摘要、平台权限
// 字段与源库文件身份任一不同都不算全等。两次全等只证明"两个时刻一致"，不证明窗口内没有
// "改后又改回"。
func TestSameCapture_DetectsEveryDrift(t *testing.T) {
	base := assetCapture{
		entries: []AssetEntry{{
			RelPath: "AGENTS.md", Size: 6, SHA256: "aa", Mode: 0o644, Owner: "1000:1000", ACLDigest: "acl",
		}},
		dbFiles: []DBFileIdentity{{Name: "basalt.db", Present: true, Size: 16, SHA256: "dd"}},
	}

	same, difference := sameCapture(base, base)
	require.True(t, same)
	require.Empty(t, difference)

	cases := []struct {
		name   string
		mutate func(*assetCapture)
	}{
		{"extra entry", func(c *assetCapture) { c.entries = append(c.entries, AssetEntry{RelPath: "SOUL.md", SHA256: "bb"}) }},
		{"renamed entry", func(c *assetCapture) { c.entries[0].RelPath = "AGENTS.md.bak" }},
		{"size", func(c *assetCapture) { c.entries[0].Size = 7 }},
		{"sha", func(c *assetCapture) { c.entries[0].SHA256 = "bb" }},
		{"mode", func(c *assetCapture) { c.entries[0].Mode = 0o600 }},
		{"owner", func(c *assetCapture) { c.entries[0].Owner = "1001:1001" }},
		{"acl", func(c *assetCapture) { c.entries[0].ACLDigest = "" }},
		{"db identity count", func(c *assetCapture) { c.dbFiles = nil }},
		{"db presence", func(c *assetCapture) { c.dbFiles[0].Present = false }},
		{"db size", func(c *assetCapture) { c.dbFiles[0].Size = 17 }},
		{"db sha", func(c *assetCapture) { c.dbFiles[0].SHA256 = "ee" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			drifted := assetCapture{
				entries: append([]AssetEntry(nil), base.entries...),
				dbFiles: append([]DBFileIdentity(nil), base.dbFiles...),
			}
			testCase.mutate(&drifted)
			same, difference := sameCapture(base, drifted)
			require.False(t, same)
			require.NotEmpty(t, difference)
		})
	}
}

// TestSealAssetPackage_PackagesIncludedAndSeals 逐条处置齐备后封装：`included` 条目进
// `package/`、清单置 sealed 并记核查摘要，`assets.json` 可回读。
// 逆改点：跳过 verifyPackagedContent → 打包摘要用例红；不写 ReviewDigest → 本用例红。
func TestSealAssetPackage_PackagesIncludedAndSeals(t *testing.T) {
	root, specs := newAssetFixture(t)
	manifest := assetBuild(t, root, specs)
	out := t.TempDir()
	assetMirrorWorkspace(t, root, out, manifest)

	reviewedAt := time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC)
	review := ReviewFile{
		UnclassifiedDigest: manifest.UnclassifiedDigest,
		Reviewer:           "ops@example",
		ReviewedAt:         reviewedAt,
		Decisions: []ReviewDecision{
			{RelPath: "notes/2026-10-26.md", Disposition: "include"},
		},
	}

	sealed, err := SealAssetPackage(context.Background(), out, manifest, review)
	require.NoError(t, err)
	require.Equal(t, AssetReviewSealed, sealed.ReviewStatus)
	require.Len(t, sealed.ReviewDigest, 64)
	require.Equal(t, "ops@example", sealed.Reviewer)
	require.True(t, sealed.ReviewedAt.Equal(reviewedAt))

	require.Equal(t, AssetDispositionIncluded, assetEntry(t, sealed, "notes/2026-10-26.md").Disposition)
	require.ElementsMatch(t, assetEntryPaths(manifest), assetPackageFiles(t, out),
		"全部条目都 included 时，包内文件集合应与清单一致")

	raw, readErr := os.ReadFile(filepath.Join(out, AssetManifestFileName))
	require.NoError(t, readErr)
	var reread AssetManifest
	require.NoError(t, json.Unmarshal(raw, &reread))
	require.Equal(t, sealed.ReviewDigest, reread.ReviewDigest)
	require.Equal(t, AssetReviewSealed, reread.ReviewStatus)
	require.Equal(t, sealed.Entries, reread.Entries)
}

// TestSealAssetPackage_ExcludedAndHandoffStayInManifest exclude / handoff 的条目**留在**
// 清单里（带处置、原因与哈希）可对账，只是不进包。
func TestSealAssetPackage_ExcludedAndHandoffStayInManifest(t *testing.T) {
	for _, disposition := range []string{"exclude", "handoff"} {
		t.Run(disposition, func(t *testing.T) {
			root, specs := newAssetFixture(t)
			manifest := assetBuild(t, root, specs)
			out := t.TempDir()
			assetMirrorWorkspace(t, root, out, manifest)

			sealed, err := SealAssetPackage(context.Background(), out, manifest, ReviewFile{
				UnclassifiedDigest: manifest.UnclassifiedDigest,
				Reviewer:           "ops@example",
				ReviewedAt:         time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC),
				Decisions: []ReviewDecision{{
					RelPath:     "notes/2026-10-26.md",
					Disposition: disposition,
					Reason:      "含内部草稿，随交接单人工转移",
				}},
			})
			require.NoError(t, err)

			wantDisposition := AssetDispositionExcluded
			if disposition == AssetDispositionHandoff {
				wantDisposition = AssetDispositionHandoff
			}
			entry := assetEntry(t, sealed, "notes/2026-10-26.md")
			require.Equal(t, wantDisposition, entry.Disposition)
			require.Equal(t, "含内部草稿，随交接单人工转移", entry.DispositionReason)
			require.NotEmpty(t, entry.SHA256, "被排除的条目仍留哈希，供事后对账")

			require.NotContains(t, assetPackageFiles(t, out), "notes/2026-10-26.md")
		})
	}
}

// TestSealAssetPackage_RefusesIncompleteReview 未归类文件的处置必须逐条齐备、摘要相符、
// 决定合法且 exclude / handoff 写明原因；任一不满足都拒绝封装。
// 逆改点：去掉"逐条齐备"循环 → missing decision 用例红；不校验摘要 → 摘要用例红。
func TestSealAssetPackage_RefusesIncompleteReview(t *testing.T) {
	cases := []struct {
		name     string
		review   func(manifest AssetManifest) ReviewFile
		expected error
		hint     string
	}{
		{
			name: "digest mismatch",
			review: func(m AssetManifest) ReviewFile {
				return ReviewFile{UnclassifiedDigest: "deadbeef"}
			},
			expected: ErrUnclassifiedUnreviewed,
			hint:     "unclassified_digest",
		},
		{
			name: "missing decision",
			review: func(m AssetManifest) ReviewFile {
				return ReviewFile{UnclassifiedDigest: m.UnclassifiedDigest}
			},
			expected: ErrUnclassifiedUnreviewed,
			hint:     "没有处置决定",
		},
		{
			name: "unknown path",
			review: func(m AssetManifest) ReviewFile {
				return ReviewFile{
					UnclassifiedDigest: m.UnclassifiedDigest,
					Decisions:          []ReviewDecision{{RelPath: "AGENTS.md", Disposition: "include"}},
				}
			},
			expected: ErrUnclassifiedUnreviewed,
			hint:     "不是清单里的未归类文件",
		},
		{
			name: "escaping path",
			review: func(m AssetManifest) ReviewFile {
				return ReviewFile{
					UnclassifiedDigest: m.UnclassifiedDigest,
					Decisions:          []ReviewDecision{{RelPath: "../escape.md", Disposition: "include"}},
				}
			},
			expected: ErrUnclassifiedUnreviewed,
			hint:     "..",
		},
		{
			name: "duplicate decision",
			review: func(m AssetManifest) ReviewFile {
				return ReviewFile{
					UnclassifiedDigest: m.UnclassifiedDigest,
					Decisions: []ReviewDecision{
						{RelPath: "notes/2026-10-26.md", Disposition: "include"},
						{RelPath: "notes/2026-10-26.md", Disposition: "exclude", Reason: "改主意"},
					},
				}
			},
			expected: ErrUnclassifiedUnreviewed,
			hint:     "两次",
		},
		{
			name: "invalid disposition",
			review: func(m AssetManifest) ReviewFile {
				return ReviewFile{
					UnclassifiedDigest: m.UnclassifiedDigest,
					Decisions:          []ReviewDecision{{RelPath: "notes/2026-10-26.md", Disposition: "maybe"}},
				}
			},
			expected: ErrUnclassifiedUnreviewed,
			hint:     "非法",
		},
		{
			name: "exclude without reason",
			review: func(m AssetManifest) ReviewFile {
				return ReviewFile{
					UnclassifiedDigest: m.UnclassifiedDigest,
					Decisions:          []ReviewDecision{{RelPath: "notes/2026-10-26.md", Disposition: "exclude"}},
				}
			},
			expected: ErrUnclassifiedUnreviewed,
			hint:     "原因",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, specs := newAssetFixture(t)
			manifest := assetBuild(t, root, specs)
			out := t.TempDir()
			assetMirrorWorkspace(t, root, out, manifest)

			_, err := SealAssetPackage(context.Background(), out, manifest, testCase.review(manifest))
			require.ErrorIs(t, err, testCase.expected)
			require.Contains(t, err.Error(), testCase.hint)
		})
	}
}

// TestSealAssetPackage_RefusesPackagedDigestMismatch 核查区内容与清单不符（被改写 / 缺失）
// 时拒绝出包：包内内容必须等于盘点内容。
func TestSealAssetPackage_RefusesPackagedDigestMismatch(t *testing.T) {
	validReview := func(m AssetManifest) ReviewFile {
		return ReviewFile{
			UnclassifiedDigest: m.UnclassifiedDigest,
			Reviewer:           "ops@example",
			ReviewedAt:         time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC),
			Decisions:          []ReviewDecision{{RelPath: "notes/2026-10-26.md", Disposition: "include"}},
		}
	}

	t.Run("tampered", func(t *testing.T) {
		root, specs := newAssetFixture(t)
		manifest := assetBuild(t, root, specs)
		out := t.TempDir()
		assetMirrorWorkspace(t, root, out, manifest)
		// 与源文件等长（6 字节）：尺寸判据查不出，必须靠 SHA-256 抓到。
		require.NoError(t, os.WriteFile(
			filepath.Join(out, assetReviewDirName, "skills", "report", "SKILL.md"),
			[]byte("evil!\n"), 0o600))

		_, err := SealAssetPackage(context.Background(), out, manifest, validReview(manifest))
		require.ErrorIs(t, err, ErrPackagedDigestMismatch)
	})

	t.Run("missing", func(t *testing.T) {
		root, specs := newAssetFixture(t)
		manifest := assetBuild(t, root, specs)
		out := t.TempDir()
		assetMirrorWorkspace(t, root, out, manifest)
		require.NoError(t, os.Remove(filepath.Join(out, assetReviewDirName, "agents", "biz-daily", "AGENT.md")))

		_, err := SealAssetPackage(context.Background(), out, manifest, validReview(manifest))
		require.ErrorIs(t, err, ErrPackagedDigestMismatch)
	})
}

// TestSealAssetPackage_NotRequiredNeedsEmptyReview 没有未归类文件时仍可直接封装，但核查
// 文件必须为空：把别处的处置套到本清单上同样拒绝。
func TestSealAssetPackage_NotRequiredNeedsEmptyReview(t *testing.T) {
	root, specs := newAssetBusinessOnlyFixture(t)
	manifest := assetBuild(t, root, specs)
	out := t.TempDir()
	assetMirrorWorkspace(t, root, out, manifest)

	_, err := SealAssetPackage(context.Background(), out, manifest, ReviewFile{
		Decisions: []ReviewDecision{{RelPath: "AGENTS.md", Disposition: "include"}},
	})
	require.ErrorIs(t, err, ErrUnclassifiedUnreviewed)

	sealed, err := SealAssetPackage(context.Background(), out, manifest, ReviewFile{})
	require.NoError(t, err)
	require.Equal(t, AssetReviewNotRequired, sealed.ReviewStatus)
	require.Empty(t, sealed.ReviewDigest)
	require.ElementsMatch(t, assetEntryPaths(manifest), assetPackageFiles(t, out))
}

// TestVerifyAssets_DetectsEveryDrift 重盘与清单比对：缺项点名"缺了哪个 Agent 的什么组件"、
// 内容改动进 Mismatched、清单外新增进 Unexpected、权限改动进 PermissionDrift、源库文件
// 改动进 DBFileDrift；比对本身不返回错误。
// 逆改点：把 MissingFor 的来源从 `RequiredFor` 改成固定空表 → 缺项点名断言红。
func TestVerifyAssets_DetectsEveryDrift(t *testing.T) {
	root, specs := newAssetFixture(t)
	manifest := assetBuild(t, root, specs)

	report, err := VerifyAssets(context.Background(), root, manifest)
	require.NoError(t, err)
	require.True(t, report.OK(), "刚采完就复核必须零差异：%v", report)
	require.Empty(t, report.MissingFor)

	require.NoError(t, os.Remove(filepath.Join(root, "agents", "biz-daily", "runs", "2026-10-26", "daily.md")))
	assetWriteFile(t, root, "skills/report/SKILL.md", "changed\n")
	assetWriteFile(t, root, "notes/extra.md", "new\n")
	assetWriteFile(t, root, "basalt.db", "rewritten database\n")
	require.NoError(t, os.Chmod(filepath.Join(root, "AGENTS.md"), 0o444))

	report, err = VerifyAssets(context.Background(), root, manifest)
	require.NoError(t, err)
	require.False(t, report.OK())

	require.Contains(t, report.Missing, "agents/biz-daily/runs/2026-10-26/daily.md")
	require.Equal(t, []string{"biz-daily"}, report.MissingFor["daily.md"])
	require.Contains(t, report.Mismatched, "skills/report/SKILL.md")
	require.NotContains(t, report.Mismatched, "AGENTS.md", "权限漂移单独成列，不混进哈希不符")
	require.Contains(t, report.Unexpected, "notes/extra.md")
	require.Contains(t, report.PermissionDrift, "AGENTS.md")
	require.NotEmpty(t, report.DBFileDrift)

	// 源库文件身份进报告，便于调用方判定"源端已变"（source_changed_since_snapshot）。
	mainDB, present := assetDBFile(report.DBFiles, "basalt.db")
	require.True(t, present)
	require.NotEqual(t, manifest.SourceDBFiles[0].SHA256, mainDB.SHA256)

	if runtime.GOOS == "windows" {
		require.Contains(t, strings.Join(report.Notes, "；"), "Windows DACL 未验证")
	}
}

// TestVerifyAssets_EmptySourceDBFilesIsDrift 清单没有记源库文件身份就等于没取证，
// 复核必须报漂移而不是放行。
func TestVerifyAssets_EmptySourceDBFilesIsDrift(t *testing.T) {
	manifest := AssetManifest{
		FormatVersion: AssetFormatVersion,
		WorkspaceID:   "workspace",
		ReviewStatus:  AssetReviewNotRequired,
	}

	report, err := VerifyAssets(context.Background(), t.TempDir(), manifest)
	require.NoError(t, err)
	require.False(t, report.OK())
	require.Contains(t, strings.Join(report.DBFileDrift, "；"), "未记录源库文件身份")
}

// TestBindAssetManifest_TripleBinding 两份清单靠三元组绑定：批次、实例、快照各自逐字相等，
// 任一不符或为空都拒绝——不得"取交集"或"合并"两个批次。
// 逆改点：把某一项的 `!=` 改成 `==` → 对应用例红。
func TestBindAssetManifest_TripleBinding(t *testing.T) {
	assetManifest := AssetManifest{
		FormatVersion:        AssetFormatVersion,
		ReviewStatus:         AssetReviewNotRequired,
		MigrationBatchID:     "batch-1",
		SourceInstanceID:     "instance-1",
		SourceSnapshotSHA256: "snapshot-1",
	}
	dbManifest := Manifest{
		FormatVersion:        ManifestFormatVersion,
		MigrationBatchID:     "batch-1",
		SourceInstanceID:     "instance-1",
		SourceSnapshotSHA256: "snapshot-1",
	}
	require.NoError(t, BindAssetManifest(assetManifest, dbManifest))

	cases := []struct {
		name     string
		mutate   func(*AssetManifest, *Manifest)
		expected error
	}{
		{"batch", func(a *AssetManifest, d *Manifest) { a.MigrationBatchID = "batch-2" }, ErrBatchMismatch},
		{"batch empty", func(a *AssetManifest, d *Manifest) { a.MigrationBatchID = "" }, ErrBatchMismatch},
		{"instance", func(a *AssetManifest, d *Manifest) { d.SourceInstanceID = "instance-2" }, ErrInstanceMismatch},
		{"instance empty", func(a *AssetManifest, d *Manifest) { d.SourceInstanceID = "" }, ErrInstanceMismatch},
		{"snapshot", func(a *AssetManifest, d *Manifest) { a.SourceSnapshotSHA256 = "snapshot-2" }, ErrSnapshotMismatch},
		{"snapshot empty", func(a *AssetManifest, d *Manifest) { d.SourceSnapshotSHA256 = "" }, ErrSnapshotMismatch},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assetCopy := assetManifest
			dbCopy := dbManifest
			testCase.mutate(&assetCopy, &dbCopy)
			require.ErrorIs(t, BindAssetManifest(assetCopy, dbCopy), testCase.expected)
		})
	}
}

// awsExampleKey 是内容扫描「厂商令牌形态」规则的样本：源码里以运行时拼接构造，仓库中
// 不出现可被凭据扫描器命中的完整厂商密钥形态（与 `credential_leak_test.go` 的正控同法）；
// 写进夹具文件后仍是完整形态，扫描规则的判别力不变。
// awsExampleKey is concatenated at runtime so the source tree holds no complete
// credential shape; the value written to the fixture file is unchanged.
var awsExampleKey = "AKIA" + "IOSFODNN7EXAMPLE"

// TestAssets_UnclassifiedSensitiveContentFlaggedNotProven C-49：文件名不显眼但内容含
// 敏感材料的未归类文件必须被启发式扫描点名（规则 ID + 计数），而扫描本身**不是**
// "包内无秘密"的证明——它不返回内容、不返回文件名、不改变核查状态，也不自动放行或
// 删除任何文件。
// 逆改点：扫描结果带上命中内容或文件名 / 命中后自动置处置或清空未归类摘要 → 对应断言红。
// TestAssets_UnclassifiedSensitiveContentFlaggedNotProven pins hints-as-hints.
func TestAssets_UnclassifiedSensitiveContentFlaggedNotProven(t *testing.T) {
	root, specs := newAssetFixture(t)
	const rel = "notes/2026-10-27-meeting.txt"
	const privateKeyLine = "-----BEGIN OPENSSH PRIVATE KEY-----"
	tokenLine := awsExampleKey
	const randomLine = "Zx9Qm2Lp7Rt4Vb8Nw3Kc6Yd1Hs5Jf0Ga"
	assetWriteFile(t, root, rel, "会议记录\n"+privateKeyLine+"\naccess="+tokenLine+"\nblob="+randomLine+"\n")

	manifest := assetBuild(t, root, specs)
	require.Equal(t, AssetReviewPending, manifest.ReviewStatus)
	entry := assetEntry(t, manifest, rel)
	require.Equal(t, AssetClassUnclassified, entry.Class)
	require.Empty(t, entry.Disposition, "扫描前不得有处置")
	digestBefore := manifest.UnclassifiedDigest

	hints, err := ScanUnclassifiedHints(context.Background(), root, manifest)
	require.NoError(t, err)
	require.GreaterOrEqual(t, hints[contentHintPrivateKey], 1, "私钥头是已知形态")
	require.GreaterOrEqual(t, hints[contentHintTokenShape], 1, "厂商令牌形态是已知形态")
	require.GreaterOrEqual(t, hints[contentHintHighEntropy], 1, "混合字符类的高熵串要提示")

	// 结果只允许是「规则 ID → 计数」的闭集：既没有文件名，也没有命中内容。
	known := []string{
		contentHintPrivateKey, contentHintTokenShape, contentHintHighEntropy,
		contentHintBinary, contentHintTruncated,
	}
	for rule, count := range hints {
		require.Contains(t, known, rule, "报告只输出已登记规则 ID")
		require.Positive(t, count)
	}

	// "不证明"：清单状态、摘要与处置一字未动，文件仍在原处——扫描不参与放行 / 删除。
	require.Equal(t, AssetReviewPending, manifest.ReviewStatus, "扫描不改变核查状态")
	require.Equal(t, digestBefore, manifest.UnclassifiedDigest, "扫描不改变未归类摘要")
	require.Empty(t, assetEntry(t, manifest, rel).Disposition, "扫描不得自动放行 / 删除")
	raw, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	require.NoError(t, readErr)
	require.Contains(t, string(raw), tokenLine, "扫描不得删改源文件")

	// 已归类业务文件在扫描范围之外：同一次扫描里没有给 business 条目记任何命中。
	require.Equal(t, 1, hints[contentHintPrivateKey], "只有那一个文件命中，业务条目不计入")
}

// TestAssets_UnclassifiedSensitiveStagedUntilReviewed C-49：含敏感内容的未归类文件在
// 逐条处置之前**只**进受限核查区（`review/`），`package/` 根本不存在；操作者 exclude
// 之后它仍以处置 + 原因 + 哈希留在最终清单里可对账，包里没有它，其它业务条目照常入包。
// 逆改点：未归类文件直接铺进 package/ → `package/` 不存在断言红；封装时把 excluded
// 条目从清单删掉 → 对账断言红。
// TestAssets_UnclassifiedSensitiveStagedUntilReviewed pins the staged-until-reviewed flow.
func TestAssets_UnclassifiedSensitiveStagedUntilReviewed(t *testing.T) {
	root, specs := newAssetFixture(t)
	const rel = "notes/2026-10-27-meeting.txt"
	assetWriteFile(t, root, rel, "meeting\naccess="+awsExampleKey+"\n")

	manifest := assetBuild(t, root, specs)
	require.Equal(t, AssetReviewPending, manifest.ReviewStatus)

	out := t.TempDir()
	require.NoError(t, MaterializeReview(context.Background(), root, out, manifest))

	staged := filepath.Join(out, assetReviewDirName, filepath.FromSlash(rel))
	raw, err := os.ReadFile(staged)
	require.NoError(t, err, "确认前只进受限核查区")
	require.Contains(t, string(raw), awsExampleKey, "核查区里是原始字节，供人工判断")
	require.NoDirExists(t, filepath.Join(out, assetPackageDirName), "确认前不得产出可交付包")

	const reason = "含真实访问密钥，随交接单人工处置"
	sealed, err := SealAssetPackage(context.Background(), out, manifest, ReviewFile{
		UnclassifiedDigest: manifest.UnclassifiedDigest,
		Reviewer:           "ops@example",
		ReviewedAt:         time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC),
		Decisions: []ReviewDecision{
			{RelPath: rel, Disposition: AssetDispositionExcluded, Reason: reason},
			{RelPath: "notes/2026-10-26.md", Disposition: AssetDispositionIncluded},
		},
	})
	require.NoError(t, err)
	require.Equal(t, AssetReviewSealed, sealed.ReviewStatus)

	excluded := assetEntry(t, sealed, rel)
	require.Equal(t, AssetDispositionExcluded, excluded.Disposition)
	require.Equal(t, reason, excluded.DispositionReason)
	require.NotEmpty(t, excluded.SHA256, "被排除的条目仍留哈希供对账")
	require.NotContains(t, assetPackageFiles(t, out), rel, "排除项不进可交付包")
	require.Contains(t, assetPackageFiles(t, out), "AGENTS.md", "其它业务条目照常入包")
}
