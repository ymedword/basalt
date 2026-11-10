//go:build integration

package app

// 本文件是 US-25C T051 的升级链路集成验收面（模块卡 §4.3 e2e 行 / FR-25C-05）：
// 存量现场（v12 口径：指针与版本行在、载荷表没有这一行）→ 控制面启动期回填载荷 →
// **空缓存 Worker** 不靠重导种子、不靠手工复制，直接同步成功并纳入就绪集合。
// 链路全部是真实 PostgreSQL 仓储与真实发布 / 回填 / 同步实现；"另一个进程"用全新
// 根目录上构造的 `AssetSync` 表示——它不携带控制面任何内存状态。
// Integration acceptance for the upgrade path: a v12-era pointer with no payload row is
// backfilled at control-plane startup, after which an empty-cache worker installs and
// reaches ready over the ordinary sync path.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

// 中文验收点：存量指针（载荷缺失、控制面目录自洽）→ 回填按 `filled` 结案且载荷
// 自洽、指针不动；同一条指针在空缓存 Worker 上先是 `blocked`（reason=`payload_missing`、
// 零安装），回填后下一轮同步装上、物化技能、进入 `ready`，安装哈希 = 指针哈希。
// 逆改点（模块卡 §4.3）：回填跳过缺失载荷 → 空缓存 Worker 永远停在 `payload_missing`
// （本用例首断即红）；回填改走"重新发布会"→ 指针版本号变化断言红。
// Acceptance point: backfilling a legacy pointer fills a self-consistent payload without
// moving the pointer, and the empty-cache worker goes from payload_missing to ready.
func TestUpgradeBackfill_V12PointerToEmptyCacheWorkerSync(t *testing.T) {
	t.Parallel()
	scene := newDistributionScene(t)
	ctx := scene.ctx

	// 现场核对：生产发布链（v13 口径）确实写了载荷——后面删掉它才是存量形态。
	_, ok, err := scene.dist.ReadBundlePayload(ctx, scene.bundleHash)
	require.NoError(t, err)
	require.True(t, ok, "生产发布链必须先写载荷再移指针（FR-25C-01）")

	// 退化成 v12 存量现场：指针与版本行保留，载荷表里没有这一行（迁移 0013 不回填）。
	_, err = scene.db.ExecContext(ctx,
		`DELETE FROM agent_bundle_payloads WHERE bundle_hash = $1`, scene.bundleHash)
	require.NoError(t, err)
	_, ok, err = scene.dist.ReadBundlePayload(ctx, scene.bundleHash)
	require.NoError(t, err)
	require.False(t, ok, "存量现场必须没有载荷行")

	// 空缓存 Worker：全新资产根与工作区根，只有数据库是共享的。
	worker := newDistributionWorker(t, scene)
	before := worker.syncOnce()
	item := requireSyncItem(t, before, scene.agent)
	require.Equal(t, syncStateBlocked, item.State,
		"载荷缺失时同步必须失败关闭，不得装半套")
	require.Equal(t, syncBlockedPayloadMissing, item.BlockedReason,
		"阻塞原因必须是卡 §4.3 的固定字面量 payload_missing")
	require.Empty(t, item.InstalledHash, "载荷缺失时不得产生安装读数")
	_, err = assets.Resolve(ctx, worker.assetRoot, scene.agent, scene.bundleHash)
	require.ErrorIs(t, err, core.ErrAssetUnavailable, "载荷缺失时不得凭空出现安装目录")

	// 控制面启动期回填：真实分发读面 + 真实载荷写面 + 真实共享记录。
	backfill, err := BackfillBundlePayloads(ctx, BackfillDeps{
		Distribution: scene.dist,
		PayloadStore: scene.payloads,
		Versions:     scene.versions,
		AssetRoot:    scene.bundleRoot,
		MaxBytes:     syncTestMaxBytes,
	})
	require.NoError(t, err)
	require.Len(t, backfill.Items, 1)
	require.Equal(t, scene.agent, backfill.Items[0].Agent)
	require.Equal(t, scene.bundleHash, backfill.Items[0].BundleHash)
	require.Equal(t, backfillStatusFilled, backfill.Items[0].Status,
		"目录自洽的存量指针必须补写载荷")

	// 回填的产物自洽：载荷字节可复算，身份与发布记录一致；指针**未移动、未发布新版本**。
	rec, ok, err := scene.dist.ReadBundlePayload(ctx, scene.bundleHash)
	require.NoError(t, err)
	require.True(t, ok, "回填后载荷行必须存在")
	require.Equal(t, scene.bundleHash, rec.BundleHash)
	require.Equal(t, scene.agent, rec.Agent)
	require.Equal(t, scene.agentUID, rec.AgentUID)
	require.Equal(t, int64(len(rec.Payload)), rec.SizeBytes)
	require.Equal(t, sha256Hex(t, rec.Payload), rec.PayloadSHA256, "载荷必须自洽")
	pointer, err := scene.versions.PointerByUID(ctx, core.ResourceAgentBundle,
		core.ResourceUID(scene.agentUID))
	require.NoError(t, err)
	require.Equal(t, scene.versionID, pointer.PublishedVersionID,
		"回填不得移动指针、不得发布新版本")

	// 空缓存 Worker 的下一轮同步：先越过失败退避窗口（backoff(1) = 1 × sync_interval）。
	scene.clock.Advance(2 * time.Second) // 越过首次退避 backoff(1) = 1 × sync_interval(1s)
	after := worker.syncOnce()
	item = requireSyncItem(t, after, scene.agent)
	require.Equal(t, syncStateReady, item.State, "回填后空缓存 Worker 必须同步到就绪")
	require.Equal(t, scene.bundleHash, item.PointerHash)
	require.Equal(t, scene.bundleHash, item.InstalledHash, "安装的必须就是发布的哈希")
	require.Empty(t, item.BlockedReason)

	readiness := worker.syncer.Readiness()[scene.agent]
	require.Equal(t, scene.bundleHash, readiness.ReadyHash, "就绪哈希必须是已安装哈希")

	// 安装面独立复核：内容寻址目录自洽、定义字节就是发布出去的那一份。
	dir, err := assets.Resolve(ctx, worker.assetRoot, scene.agent, scene.bundleHash)
	require.NoError(t, err, "回填后必须能按哈希解析到安装目录")
	manifest, err := assets.Verify(ctx, dir)
	require.NoError(t, err)
	require.Equal(t, scene.bundleHash, manifest.BundleHash)
	requireInstalledDefinition(t, dir, scene.definition, scene.agent)
}
