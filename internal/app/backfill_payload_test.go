package app

// 本文件是 US-25C §2.3.3 / §4.3 的存量载荷回填定向判据（单元层，无数据库）：
//   - 存量指针（载荷缺失、目录自洽）必须逐条回填，且**不发布新版本、不移指针**（FR-25C-05 / C15）；
//   - 目录缺失必须如实记为 `payload_missing`，不伪造、不写空载荷；
//   - 目录在但内容不可复算必须记为 `corrupt`，同样不写库、不移指针。
//
// 夹具走真实发布链（构建 → 安装 → 版本行 → 指针）再把载荷删掉，模拟 US-25 及以前
// 发布的存量现场；共享记录、操作日志与载荷表用内存替身（模块卡 §4.1：单元层允许
// 内存夹具、无外部依赖）。
// Unit judges for the legacy payload backfill: fill without moving the pointer, report a
// missing directory as payload_missing, and refuse a directory that fails verification.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// fakeDistribution 是 `core.AssetDistributionPort` 的内存替身：只实现回填用到的
// 全量翻页与按哈希读载荷；增量发现与技能版本读取不被回填调用，给零值实现。
// fakeDistribution serves the full-scan page and the payload read; the incremental
// listing and skill-version read are outside the backfill's call surface.
type fakeDistribution struct {
	*fakeBundlePayloads
	bundles []core.PublishedBundle
	listErr error
}

// newFakeDistribution 按 `resource_id` 升序建一份全量列表——存储层就是以这个序翻页。
func newFakeDistribution(payloads *fakeBundlePayloads, bundles ...core.PublishedBundle) *fakeDistribution {
	sorted := make([]core.PublishedBundle, len(bundles))
	copy(sorted, bundles)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && string(sorted[j].AgentUID) < string(sorted[j-1].AgentUID); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return &fakeDistribution{fakeBundlePayloads: payloads, bundles: sorted}
}

func (f *fakeDistribution) ListPublishedBundlesAfter(context.Context, core.PublishedBundleCursor, int) ([]core.PublishedBundle, error) {
	return nil, nil
}

func (f *fakeDistribution) ListPublishedBundlesPage(_ context.Context, afterResourceID string, limit int) ([]core.PublishedBundle, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	page := make([]core.PublishedBundle, 0, limit)
	for _, bundle := range f.bundles {
		if string(bundle.AgentUID) <= afterResourceID {
			continue
		}
		if len(page) >= limit {
			break
		}
		page = append(page, bundle)
	}
	return page, nil
}

func (f *fakeDistribution) ReadSkillVersion(context.Context, int64) (core.SkillVersionRecord, bool, error) {
	return core.SkillVersionRecord{}, false, nil
}

// backfillFixture 是"已发布但载荷缺失"的存量现场：bundle 目录来自真实发布链的
// 构建与安装，共享记录（指针 + 版本行）由同一条链写入，载荷表按存量口径清空。
type backfillFixture struct {
	root      string
	assetRoot string
	records   *fakeBundleRecords
	payloads  *fakeBundlePayloads
	dist      *fakeDistribution
	hash      string
	versionID int64
	logger    *slog.Logger
	logs      *bytes.Buffer
}

func newBackfillFixture(t *testing.T) *backfillFixture {
	t.Helper()
	ctx := context.Background()
	fixture := &backfillFixture{root: t.TempDir(), assetRoot: t.TempDir()}
	writeWorkspaceFile(t, fixture.root, "agents/"+publishPayloadAgent+"/AGENT.md", publishPayloadAgentMD)
	fixture.records = newFakeBundleRecords()
	fixture.payloads = newFakeBundlePayloads(&fixture.records.calls)
	ops := &fakeOperationLog{order: &fixture.records.calls}
	fixture.records.ops = ops
	logger, logs := newCapturedLogger()
	fixture.logger, fixture.logs = logger, logs

	versionID, err := publishWorkspaceAgentBundle(ctx, workspacePublishDeps{
		Root:         fixture.root,
		BundleRoot:   fixture.assetRoot,
		Versions:     fixture.records,
		Operations:   ops,
		Payloads:     fixture.payloads,
		PayloadStore: fixture.payloads,
		MaxBytes:     publishPayloadMaxBytes,
		Logger:       logger,
		Now:          publishPayloadClock,
	}, publishPayloadAgent)
	require.NoError(t, err)
	version, err := fixture.records.VersionByID(ctx, versionID)
	require.NoError(t, err)
	fixture.versionID = versionID
	fixture.hash = version.ContentHash
	require.NotEmpty(t, fixture.hash)

	// 存量现场：指针在、目录在，载荷表里没有这一行。
	delete(fixture.payloads.records, fixture.hash)
	fixture.dist = newFakeDistribution(fixture.payloads, core.PublishedBundle{
		Agent:      publishPayloadAgent,
		AgentUID:   publishPayloadUID,
		BundleHash: fixture.hash,
	})
	return fixture
}

// deps 给出最小装配：不配重建来源（工作区 / 生命周期），只走现行目录与同机候选。
func (f *backfillFixture) deps() BackfillDeps {
	return BackfillDeps{
		Distribution: f.dist,
		PayloadStore: f.payloads,
		Versions:     f.records,
		AssetRoot:    f.assetRoot,
		MaxBytes:     publishPayloadMaxBytes,
		Logger:       f.logger,
	}
}

// installedDir 解析当前安装目录（发布链刚装好的那一份）。
func (f *backfillFixture) installedDir(t *testing.T) string {
	t.Helper()
	dir, err := assets.Resolve(context.Background(), f.assetRoot, publishPayloadAgent, f.hash)
	require.NoError(t, err)
	return dir
}

// 中文验收点：存量指针的载荷缺失且目录自洽 → 逐条回填，指针不动、不发布新版本；
// 第二轮按真幂等返回已存在，不重复写。逆改点（模块卡 §4.3）：回填跳过缺失载荷 →
// 载荷表仍为空。
// Acceptance point: a legacy pointer is backfilled from its self-consistent directory
// without publishing or moving the pointer, and a second pass is a true no-op.
func TestBackfill_LegacyPointer(t *testing.T) {
	fixture := newBackfillFixture(t)
	ctx := context.Background()

	report, err := BackfillBundlePayloads(ctx, fixture.deps())
	require.NoError(t, err)
	require.Len(t, report.Items, 1)
	require.Equal(t, publishPayloadAgent, report.Items[0].Agent)
	require.Equal(t, fixture.hash, report.Items[0].BundleHash)
	require.Equal(t, "filled", report.Items[0].Status)

	rec, ok := fixture.payloads.records[fixture.hash]
	require.True(t, ok, "存量指针必须补写载荷")
	require.Equal(t, fixture.hash, rec.BundleHash)
	require.Equal(t, publishPayloadAgent, rec.Agent)
	require.Equal(t, publishPayloadUID, rec.AgentUID)
	require.Equal(t, int64(len(rec.Payload)), rec.SizeBytes)
	require.Equal(t, sha256Hex(t, rec.Payload), rec.PayloadSHA256, "载荷必须自洽")
	require.Equal(t, 1, fixture.records.publishCalls, "回填不得发布新版本")
	require.Equal(t, int64(7), fixture.records.pointer.PublishedVersionID, "回填不得移动指针")

	// 幂等：载荷已在且自洽 → 不重复写。
	beforePut := fixture.payloads.putCalls
	report, err = BackfillBundlePayloads(ctx, fixture.deps())
	require.NoError(t, err)
	require.Len(t, report.Items, 1)
	require.Equal(t, "exists", report.Items[0].Status)
	require.Equal(t, beforePut, fixture.payloads.putCalls, "已存在的载荷不得重写")
	require.Equal(t, 1, fixture.records.publishCalls)
}

// 中文验收点：目录缺失（现行目录与同机候选都不可用）→ 报告 `payload_missing`，
// 不伪造、不写空载荷、不移指针，并留下可检索的 WARN。逆改点（模块卡 §4.3）：
// 回填跳过缺失目录 → 报告不列 `payload_missing`。
// Acceptance point: a missing directory is reported as payload_missing, nothing is
// fabricated, and the warning stays searchable.
func TestBackfill_MissingDirVisible(t *testing.T) {
	fixture := newBackfillFixture(t)
	ctx := context.Background()
	// 版本行的 storage_path 就是现行目录：删掉它，同机候选也随之不可用。
	require.NoError(t, os.RemoveAll(fixture.installedDir(t)))

	report, err := BackfillBundlePayloads(ctx, fixture.deps())
	require.NoError(t, err)
	require.Len(t, report.Items, 1)
	require.Equal(t, "payload_missing", report.Items[0].Status)
	require.NotEmpty(t, report.Items[0].Detail, "可见失败必须带上原因")
	require.Empty(t, fixture.payloads.records, "缺失目录不得伪造载荷")
	require.Equal(t, 1, fixture.records.publishCalls, "回填不得发布新版本")
	require.Equal(t, int64(7), fixture.records.pointer.PublishedVersionID, "回填不得移动指针")
	require.Contains(t, fixture.logs.String(), "no self-consistent bundle directory")
}

// 中文验收点：目录在但内容不可复算（文件被改写）→ 报告 `corrupt`，不编码、不写库、
// 不移指针。逆改点（模块卡 §4.3）：先编码后验目录 → 载荷表出现不自洽的字节。
// Acceptance point: a directory that fails verification is reported as corrupt and
// never turned into a payload.
func TestBackfill_DoesNotMovePointer(t *testing.T) {
	fixture := newBackfillFixture(t)
	ctx := context.Background()
	dir := fixture.installedDir(t)
	// 清单不动、只改文件内容：目录名与清单仍对得上，逐文件摘要对不上。
	writeWorkspaceFile(t, dir, "agent/agents/"+publishPayloadAgent+"/AGENT.md", "被篡改的正文\n")

	report, err := BackfillBundlePayloads(ctx, fixture.deps())
	require.NoError(t, err)
	require.Len(t, report.Items, 1)
	require.Equal(t, "corrupt", report.Items[0].Status)
	require.Empty(t, fixture.payloads.records, "不自洽的目录不得产出载荷")
	require.Equal(t, 1, fixture.records.publishCalls, "回填不得发布新版本")
	require.Equal(t, int64(7), fixture.records.pointer.PublishedVersionID, "回填不得移动指针")
	require.Contains(t, fixture.logs.String(), "failed verification")
}

// 中文验收点：现行布局不可用但版本行的 storage_path 是资产根内的**同机候选**时，
// 候选目录必须被采用（basename == 哈希 + 位于资产根之内）。
// Acceptance point: the version row's storage_path is adopted as a same-machine
// candidate when it resolves to hash-named content under the asset root.
func TestBackfill_SameMachineStoragePathCandidate(t *testing.T) {
	fixture := newBackfillFixture(t)
	ctx := context.Background()

	legacy := filepath.Join(fixture.assetRoot, "legacy", fixture.hash)
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
	require.NoError(t, os.Rename(fixture.installedDir(t), legacy))
	version := fixture.records.versions[fixture.versionID]
	version.StoragePath = legacy
	fixture.records.versions[fixture.versionID] = version

	report, err := BackfillBundlePayloads(ctx, fixture.deps())
	require.NoError(t, err)
	require.Len(t, report.Items, 1)
	require.Equal(t, "filled", report.Items[0].Status)
	require.NotEmpty(t, fixture.payloads.records, "同机候选目录必须被采用")
	require.Equal(t, 1, fixture.records.publishCalls, "回填不得发布新版本")
}

// 中文验收点：现行目录缺失、同机候选也不可用，但本地生命周期代次与共享索引相等时，
// 允许按共享发布记录重建本地目录，再从重建结果编码载荷（哈希必须与记录逐字相等）。
// Acceptance point: with generations in agreement, a missing directory is rebuilt from
// the published record before the payload is encoded.
func TestBackfill_RebuildsFromPublishedRecord(t *testing.T) {
	fixture := newBackfillFixture(t)
	ctx := context.Background()
	require.NoError(t, os.RemoveAll(fixture.installedDir(t)))

	lifecycle := workspace.NewLifecycleStore(t.TempDir(), publishPayloadClock)
	record, err := lifecycle.BootstrapIdentity(ctx, publishPayloadAgent, publishPayloadUID, "op-backfill-fixture")
	require.NoError(t, err)
	fixture.records.lifecycleIndex = map[string]core.LifecycleRecord{publishPayloadAgent: record}

	deps := fixture.deps()
	deps.Lifecycle = lifecycle
	deps.WorkspaceRoot = fixture.root

	report, err := BackfillBundlePayloads(ctx, deps)
	require.NoError(t, err)
	require.Len(t, report.Items, 1)
	require.Equal(t, "filled", report.Items[0].Status)
	require.NotEmpty(t, fixture.payloads.records, "重建后的目录必须产出载荷")
	require.NotEmpty(t, fixture.installedDir(t), "重建必须把目录装回资产根")
	require.Equal(t, 1, fixture.records.publishCalls, "回填不得发布新版本")
	require.Equal(t, int64(7), fixture.records.pointer.PublishedVersionID, "回填不得移动指针")
}
