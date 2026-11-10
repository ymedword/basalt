//go:build integration

package postgres

// 本文件是 US-25C T006 的仓储验收面（模块卡 §3.1.2 / §3.1.3 / §3.4）：
//   - 载荷仓储是内容寻址、幂等的：同哈希重复写入只留一行；同哈希不同载荷必须是
//     明确错误，且原行逐字不变（`ON CONFLICT DO NOTHING` 之后按 `payload_sha256` 复核）；
//   - 增量发现必须用复合键集 `(updated_at, resource_id)` 严格前进：201 行落在同一
//     毫秒、每页 200 行时，第二页只能看到第 201 行——用 `updated_at >= $1` 会重复前
//     200 行，用 `updated_at > $1` 会让第 201 行永不可见；
//   - 全量兜底只按 `resource_id` 翻页：迟到提交（更新时间早于已走过的游标）仍会被
//     读出，墓碑不出现，中断后从游标续扫不丢项；移除缺席者不属于读取路径（归同步层
//     在整趟完成后回读确认）；
//   - `PointerVersion` 是单条 join、一次快照、不额外取锁：指针指向哪一版就返回哪一
//     版（不是最大版本），且在他人持有指针行锁时仍立即返回。
//
// 全部用例打在同 schema 的真实 PostgreSQL 上：键集顺序、同毫秒并列、`ON CONFLICT`
// 语义与行锁行为都只有真库才判得出来。
// The storage acceptance surface for US-25C: content-addressed payload idempotency, a
// strictly advancing (updated_at, resource_id) keyset, a resource_id-ordered full scan,
// and a single-snapshot pointer read that takes no extra lock.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

// distributionOperationID 是本文件夹具共用的管理操作行（版本行与指针行的外键要求它
// 先存在）；每个用例一个隔离 schema，同名操作互不可见。
// distributionOperationID is the fixture operation row every version and pointer needs;
// each case runs in its own schema, so the same id is collision-free.
const distributionOperationID = "op-us25c-distribution"

// distributionAgentUID 造第 index 个合法的 Agent 身份。载荷是零填充的 26 位 Crockford
// 数字，字典序与数值序一致——键集用例据此得到确定的 `resource_id` 顺序。
// distributionAgentUID builds the index-th agent uid; zero-padded digits make the
// lexicographic order equal to the numeric one.
func distributionAgentUID(index int) core.AgentUID {
	return core.AgentUID(fmt.Sprintf("ag_%026d", index))
}

// distributionBundleHash 造第 index 个 64 位小写十六进制 bundle 哈希。
// distributionBundleHash builds the index-th content-addressed bundle hash.
func distributionBundleHash(index int) string {
	return fmt.Sprintf("%064x", index)
}

// distributionSHA256 是载荷字节的 sha256（`payload_sha256` 口径，不是 BundleHash 的
// 目录内容口径）。
// distributionSHA256 hashes the archive bytes, the payload_sha256 notion.
func distributionSHA256(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// distributionManifestJSON 造版本行 `manifest_json` 的真实形态（与 `assets_publish.go`
// 的发布路径同一结构）：同步层靠它解出 Agent / AgentUID / BundleHash 三项身份。
// distributionManifestJSON marshals the same manifest the publish path stores, so the
// distribution read has to parse real structure rather than a stub.
func distributionManifestJSON(t *testing.T, agent string, uid core.AgentUID, bundle string) string {
	t.Helper()
	raw, err := json.Marshal(assets.Manifest{
		FormatVersion: assets.FormatVersion,
		Agent:         agent,
		AgentUID:      uid,
		ConfigHash:    strings.Repeat("c", 64),
		ConfigFiles:   []core.FileHash{},
		Skills:        []core.SkillVersionRef{},
		BundleHash:    bundle,
	})
	require.NoError(t, err)
	return string(raw)
}

// distributionSeed 是一行已发布 Agent bundle 的夹具：一条版本行 + 一条指针行。
// distributionSeed describes one published agent bundle fixture.
type distributionSeed struct {
	agent     string
	uid       core.AgentUID
	bundle    string
	updatedAt int64
	deleted   bool
}

// seedPublishedBundle 按发布路径的行形态落一行版本与指针；deleted 时指针带墓碑。
// seedPublishedBundle plants one version row and one pointer row; a deleted pointer
// carries a tombstone stamp.
func seedPublishedBundle(t *testing.T, db *DB, seed distributionSeed) int64 {
	t.Helper()
	ctx := context.Background()
	var versionID int64
	require.NoError(t, db.pool.QueryRowContext(ctx, `
		INSERT INTO resource_versions
		   (resource_kind, resource_uid, resource_id, version_no, content_hash,
		    manifest_json, storage_path, created_at, operation_id)
		VALUES ($1, $2, $3, 1, $4, $5, $6, $7, $8)
		RETURNING id`,
		string(core.ResourceAgentBundle), string(seed.uid), string(seed.uid), seed.bundle,
		distributionManifestJSON(t, seed.agent, seed.uid, seed.bundle),
		"/srv/basalt/assets/bundles/"+seed.agent+"/"+seed.bundle,
		seed.updatedAt, distributionOperationID).Scan(&versionID))

	var deletedAt any
	if seed.deleted {
		deletedAt = seed.updatedAt
	}
	_, err := db.pool.ExecContext(ctx, `
		INSERT INTO resource_pointers
		   (resource_kind, resource_id, resource_uid, published_version_id, applied_version_id,
		    scope, owner_agent_uid, revision, deleted_at, updated_at, operation_id)
		VALUES ($1, $2, $2, $3, $3, 'shared', NULL, 1, $4, $5, $6)`,
		string(core.ResourceAgentBundle), string(seed.uid), versionID, deletedAt,
		seed.updatedAt, distributionOperationID)
	require.NoError(t, err)
	return versionID
}

// seedSkillVersion 落一行技能版本（`manifest_json` 是 `core.FileHash` 数组，与
// `parseSkillManifest` 读的形态一致），返回版本 ID。
// seedSkillVersion plants one skill version whose manifest_json is the file array the
// published-skill reader parses.
func seedSkillVersion(t *testing.T, db *DB, name string, versionNo int, storagePath string, files []core.FileHash) int64 {
	t.Helper()
	raw, err := json.Marshal(files)
	require.NoError(t, err)
	var versionID int64
	require.NoError(t, db.pool.QueryRowContext(context.Background(), `
		INSERT INTO resource_versions
		   (resource_kind, resource_uid, resource_id, version_no, content_hash,
		    manifest_json, storage_path, created_at, operation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id`,
		string(core.ResourceSkill), string(resourceUIDFor(core.ResourceSkill, name)), name,
		versionNo, distributionSHA256([]byte(storagePath)), string(raw), storagePath,
		opNow.UnixMilli(), distributionOperationID).Scan(&versionID))
	return versionID
}

// upsertSkillPointer 把技能指针指向指定版本（首次插入、后续移动都走这一条）。
// upsertSkillPointer points a skill at one version, inserting or moving the pointer.
func upsertSkillPointer(t *testing.T, db *DB, name string, versionID int64, updatedAt int64) {
	t.Helper()
	_, err := db.pool.ExecContext(context.Background(), `
		INSERT INTO resource_pointers
		   (resource_kind, resource_id, resource_uid, published_version_id, applied_version_id,
		    scope, owner_agent_uid, revision, deleted_at, updated_at, operation_id)
		VALUES ($1, $2, $3, $4, $4, 'shared', NULL, 1, NULL, $5, $6)
		ON CONFLICT (resource_kind, resource_id) DO UPDATE
		   SET published_version_id = EXCLUDED.published_version_id,
		       applied_version_id = EXCLUDED.applied_version_id,
		       updated_at = EXCLUDED.updated_at`,
		string(core.ResourceSkill), name, string(resourceUIDFor(core.ResourceSkill, name)),
		versionID, updatedAt, distributionOperationID)
	require.NoError(t, err)
}

// TestBundlePayloadStore_Idempotent 判载荷仓储的内容寻址与幂等（FR-25C-01 / FR-25C-02）：
//   - 写入后逐字段读回一致（载荷字节 / payload_sha256 / size_bytes / 身份 / 时间）；
//   - 同一记录重复写入是幂等 no-op，表里仍只有一行；
//   - 同 `bundle_hash` 但载荷不同必须返回明确错误，且原有行逐字不变——这正是
//     `ON CONFLICT DO NOTHING` 之后按 `payload_sha256` 复核的那一步；
//   - 未命中的哈希返回 `(零值, false, nil)`，不伪造记录。
//
// 逆改点：去掉写后复核 → 冲突载荷用例拿到 nil；把 `ON CONFLICT` 改成 DO UPDATE →
// 「原行不变」断言红；按 payload 而不用 bundle_hash 做主键 → 幂等行数断言红。
// Reverse-edit points: dropping the post-insert hash recheck accepts a conflicting
// payload; DO UPDATE rewrites the original row; a non-content key breaks the row count.
func TestBundlePayloadStore_Idempotent(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	payloads := NewBundlePayloadStore(db)
	distribution := NewAssetDistributionStore(db)

	payload := []byte("deterministic-tar-archive-bytes")
	record := core.BundlePayloadRecord{
		BundleHash:    strings.Repeat("a", 64),
		Agent:         "bid-requirements",
		AgentUID:      distributionAgentUID(1),
		FormatVersion: assets.FormatVersion,
		Payload:       payload,
		PayloadSHA256: distributionSHA256(payload),
		SizeBytes:     int64(len(payload)),
		CreatedAt:     opNow.UnixMilli(),
	}
	require.NoError(t, payloads.PutBundlePayload(ctx, record))

	got, ok, err := distribution.ReadBundlePayload(ctx, record.BundleHash)
	require.NoError(t, err)
	require.True(t, ok, "刚写入的载荷必须可读回")
	require.Equal(t, record, got, "读回的记录必须逐字段等于写入值")

	// 幂等：同一哈希同一载荷重复写入不报错、不产生第二行。
	require.NoError(t, payloads.PutBundlePayload(ctx, record), "同一载荷重复写入必须是幂等 no-op")
	var rows int
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_bundle_payloads WHERE bundle_hash = $1`, record.BundleHash).Scan(&rows))
	require.Equal(t, 1, rows, "内容寻址：同哈希只存一行")

	// 同哈希不同载荷：必须报错，且原行不被覆盖。
	conflicting := record
	conflicting.Payload = []byte("a different archive for the same hash")
	conflicting.PayloadSHA256 = distributionSHA256(conflicting.Payload)
	conflicting.SizeBytes = int64(len(conflicting.Payload))
	require.Error(t, payloads.PutBundlePayload(ctx, conflicting),
		"同 bundle_hash 不同载荷必须是明确错误，不能静默沿用旧行")

	unchanged, ok, err := distribution.ReadBundlePayload(ctx, record.BundleHash)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, record.Payload, unchanged.Payload, "冲突写入不得覆盖原有载荷")
	require.Equal(t, record.PayloadSHA256, unchanged.PayloadSHA256, "冲突写入不得改写原有哈希")
	require.Equal(t, record.SizeBytes, unchanged.SizeBytes)
	require.NoError(t, db.pool.QueryRowContext(ctx,
		`SELECT count(*) FROM agent_bundle_payloads WHERE bundle_hash = $1`, record.BundleHash).Scan(&rows))
	require.Equal(t, 1, rows, "冲突写入不得新增行")

	// 未命中：零值 + false + nil（调用方据此走 payload_missing，而不是拿到伪记录）。
	missing, ok, err := distribution.ReadBundlePayload(ctx, strings.Repeat("f", 64))
	require.NoError(t, err)
	require.False(t, ok, "不存在的哈希必须返回 ok=false")
	require.Equal(t, core.BundlePayloadRecord{}, missing)
}

// TestAssetDistribution_KeysetSameMillisecond 判增量发现的键集前进（FR-25C-06 / C19）：
// 201 行落在同一毫秒、每页 200 行时，两页合起来必须恰好覆盖 201 行，每行只读一次，
// 且第二页的每一行都严格大于第一页最后一行的 `(updated_at, resource_id)`。
//
// 逆改点：谓词改成 `updated_at >= $1` → 第二页重复前 200 行、第 201 行缺失；改成
// `updated_at > $1` → 第二页为空；漏掉 `deleted_at IS NULL` → 墓碑混进结果。
// Reverse-edit points: `>=` repeats the first page, `>` loses the 201st row, and a
// missing tombstone predicate leaks the deleted pointer.
func TestAssetDistribution_KeysetSameMillisecond(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	seedOperation(t, db, distributionOperationID)
	store := NewAssetDistributionStore(db)

	const total = 201
	const pageSize = 200
	sameMillis := opNow.UnixMilli()
	for index := 1; index <= total; index++ {
		seedPublishedBundle(t, db, distributionSeed{
			agent:     fmt.Sprintf("agent-%03d", index),
			uid:       distributionAgentUID(index),
			bundle:    distributionBundleHash(index),
			updatedAt: sameMillis,
		})
	}
	seedPublishedBundle(t, db, distributionSeed{
		agent:     "agent-tombstone",
		uid:       distributionAgentUID(total + 1),
		bundle:    distributionBundleHash(total + 1),
		updatedAt: sameMillis,
		deleted:   true,
	})

	first, err := store.ListPublishedBundlesAfter(ctx, core.PublishedBundleCursor{}, pageSize)
	require.NoError(t, err)
	require.Len(t, first, pageSize, "第一页必须拿满一页")
	cursor := core.PublishedBundleCursor{
		UpdatedAt:  first[len(first)-1].UpdatedAt,
		ResourceID: string(first[len(first)-1].AgentUID),
	}

	second, err := store.ListPublishedBundlesAfter(ctx, cursor, pageSize)
	require.NoError(t, err)
	require.Len(t, second, 1, "同毫秒的 201 行里，第二页必须恰好只剩第 201 行")
	require.Equal(t, distributionAgentUID(total), second[0].AgentUID,
		"第 201 行必须可见（键集按 resource_id 前进，不是按时间跳跃）")

	seen := map[string]bool{}
	for _, page := range [][]core.PublishedBundle{first, second} {
		for _, bundle := range page {
			require.False(t, seen[string(bundle.AgentUID)], "同一行不得被读到两次：%s", bundle.AgentUID)
			seen[string(bundle.AgentUID)] = true
			require.Equal(t, sameMillis, bundle.UpdatedAt, "UpdatedAt 必须是存储写入的毫秒值")
			require.NotEmpty(t, bundle.Agent, "Agent 名必须从 manifest_json 解出")
			require.NotEmpty(t, bundle.BundleHash, "BundleHash 必须从 manifest_json 解出")
			require.False(t, strings.HasPrefix(bundle.Agent, "agent-tombstone"),
				"已删除的指针不得出现在增量结果里")
		}
	}
	require.Len(t, seen, total, "两页合起来必须覆盖全部 201 行")
	for index := 1; index <= total; index++ {
		require.True(t, seen[string(distributionAgentUID(index))], "第 %d 行不得缺失", index)
	}

	// 游标严格前进：第二页的每一行都大于第一页最后一行。
	lastKey := cursor
	for _, bundle := range second {
		key := core.PublishedBundleCursor{UpdatedAt: bundle.UpdatedAt, ResourceID: string(bundle.AgentUID)}
		require.True(t, key.UpdatedAt > lastKey.UpdatedAt ||
			(key.UpdatedAt == lastKey.UpdatedAt && key.ResourceID > lastKey.ResourceID),
			"游标必须严格前进：(%d, %s) 不大于 (%d, %s)",
			key.UpdatedAt, key.ResourceID, lastKey.UpdatedAt, lastKey.ResourceID)
		lastKey = key
	}

	// 走到末尾后必须终止：空页而不是从头再来。
	tail, err := store.ListPublishedBundlesAfter(ctx, lastKey, pageSize)
	require.NoError(t, err)
	require.Empty(t, tail, "游标越过末行后必须返回空页")
}

// TestAssetDistribution_FullScanResume 判全量兜底的翻页语义（FR-25C-06 / C19）：
//   - 只按 `resource_id` 排序翻页，与 `updated_at` 无关——迟到提交（更新时间早于
//     已走过的游标）仍会被读出，这是增量键集不保证、必须靠全量补的那一类；
//   - 从上次的 `resource_id` 续扫不丢项、不重复，中断前后都不删除任何行；
//   - 墓碑不出现在结果里；
//   - 读取路径不写库：缺席者的移除归同步层在整趟完成后回读确认，不在存储层。
//
// 逆改点：改成按 `updated_at` 排序 → 期望顺序断言红；加 `updated_at >= 起点` 之类的
// 时间过滤 → 迟到提交行消失；缓存整趟结果 → 续扫从第二页开始时丢第一页之外的行。
// Reverse-edit points: ordering by updated_at, a time predicate, or caching the trip
// each turns a named assertion red.
func TestAssetDistribution_FullScanResume(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	seedOperation(t, db, distributionOperationID)
	store := NewAssetDistributionStore(db)

	base := opNow.UnixMilli()
	seeds := []distributionSeed{
		{agent: "agent-a", uid: distributionAgentUID(1), bundle: distributionBundleHash(1), updatedAt: base + 5},
		// agent-b 的更新时间**最早**：增量游标一旦走过 base+5 就永远看不到它，
		// 全量扫描必须仍然返回它（迟到提交）。
		{agent: "agent-b", uid: distributionAgentUID(2), bundle: distributionBundleHash(2), updatedAt: base + 1},
		{agent: "agent-c", uid: distributionAgentUID(3), bundle: distributionBundleHash(3), updatedAt: base + 4},
		{agent: "agent-d", uid: distributionAgentUID(4), bundle: distributionBundleHash(4), updatedAt: base + 2},
		{agent: "agent-e", uid: distributionAgentUID(5), bundle: distributionBundleHash(5), updatedAt: base + 3},
		{agent: "agent-gone", uid: distributionAgentUID(6), bundle: distributionBundleHash(6), updatedAt: base + 6, deleted: true},
	}
	for _, seed := range seeds {
		seedPublishedBundle(t, db, seed)
	}
	countRows := func() int {
		var rows int
		require.NoError(t, db.pool.QueryRowContext(ctx,
			`SELECT count(*) FROM resource_pointers WHERE resource_kind = 'agent_bundle'`).Scan(&rows))
		return rows
	}
	before := countRows()

	// 第一页：按 resource_id 升序，与创建时间无关。
	first, err := store.ListPublishedBundlesPage(ctx, "", 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	require.Equal(t, []core.AgentUID{distributionAgentUID(1), distributionAgentUID(2)},
		[]core.AgentUID{first[0].AgentUID, first[1].AgentUID},
		"全量扫描必须只按 resource_id 排序（agent-b 的更新时间最早也不得被推后）")
	require.Equal(t, base+5, first[0].UpdatedAt)
	require.Equal(t, base+1, first[1].UpdatedAt, "UpdatedAt 必须如实返回存储写入值")

	// 模拟中断：到这里就停（没有整趟完成，也没有任何移除动作）。
	require.Equal(t, before, countRows(), "扫描中断不得删除任何行")

	// 续扫：从第一页末行的 resource_id 继续，直到空页。
	var scanned []core.PublishedBundle
	scanned = append(scanned, first...)
	after := string(first[len(first)-1].AgentUID)
	for {
		page, err := store.ListPublishedBundlesPage(ctx, after, 2)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		scanned = append(scanned, page...)
		after = string(page[len(page)-1].AgentUID)
	}

	seen := make([]string, 0, len(scanned))
	for _, bundle := range scanned {
		seen = append(seen, string(bundle.AgentUID))
	}
	require.Len(t, seen, 5, "全量扫描必须覆盖全部未删除行（墓碑不算）")
	require.True(t, slices.IsSorted(seen), "全量扫描结果必须按 resource_id 严格升序")
	require.NotContains(t, seen, string(distributionAgentUID(6)), "墓碑不得出现在全量结果里")
	require.Equal(t, before, countRows(), "整趟读取不写库，行数不变")
}

// TestAssetDistribution_ReadSkillVersion 判按**版本 ID** 读技能发布记录（FR-25C-07）：
// 返回 `resource_id`（技能名）、`storage_path`（工作区相对）与 `manifest_json` 的逐文件
// 清单；未命中的版本 ID 返回 `(零值, false, nil)`。物化层靠这三项逐文件核对哈希。
//
// 逆改点：改按名称取最新版本 → 指向旧版本的用例读到新版本；忘记解 files → 清单为
// 空；把未命中写成错误 → ok=false 断言红。
// Reverse-edit points: resolving by name picks the wrong version; skipping the manifest
// leaves the file list empty; reporting a miss as an error breaks the ok=false contract.
func TestAssetDistribution_ReadSkillVersion(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	seedOperation(t, db, distributionOperationID)
	store := NewAssetDistributionStore(db)

	files := []core.FileHash{
		{Path: "SKILL.md", SHA256: distributionSHA256([]byte("card")), Size: 4},
		{Path: "references/guide.md", SHA256: distributionSHA256([]byte("guide")), Size: 5},
	}
	versionID := seedSkillVersion(t, db, "tender", 1, "skills/tender/v1", files)
	// 同技能再发一版：按版本 ID 读必须拿指向的那一版，而不是最新一版。
	newerFiles := []core.FileHash{{Path: "SKILL.md", SHA256: distributionSHA256([]byte("card v2")), Size: 7}}
	seedSkillVersion(t, db, "tender", 2, "skills/tender/v2", newerFiles)

	record, ok, err := store.ReadSkillVersion(ctx, versionID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "tender", record.Name, "Name 是版本行的 resource_id（技能名）")
	require.Equal(t, versionID, record.VersionID)
	require.Equal(t, "skills/tender/v1", record.StoragePath, "storage_path 必须工作区相对、原样返回")
	require.Equal(t, files, record.Files, "文件清单必须来自该版本行的 manifest_json")

	missing, ok, err := store.ReadSkillVersion(ctx, versionID+1000)
	require.NoError(t, err)
	require.False(t, ok, "不存在的版本 ID 必须返回 ok=false")
	require.Equal(t, core.SkillVersionRecord{}, missing)
}

// TestResourceStore_PointerVersion 判技能联动复核用的指针读（FR-25C-17 / §2.3.7）：
//   - 返回**指针当前指向的版本 ID**，不是该技能的最大版本号；
//   - 指针移动后立即读到新值（不缓存）；
//   - 与 `Pointer` 读到的 `published_version_id` 一致（同一份指针语义）；
//   - 未命中的名称返回 `core.ErrResourceNotFound`；
//   - 单快照、不额外取锁：另一个事务对指针行持有 `FOR UPDATE` 时，本读仍立即返回
//     已提交值——实现若改用 `FOR SHARE` 等行锁会阻塞到上下文超时。
//
// 逆改点：改成 `MAX(version_no)` → v2 已存在但指针在 v1 的用例红；加行锁 → 持锁
// 用例超时；每次读都缓存结果 → 移动指针后的断言红。
// Reverse-edit points: MAX(version_no) returns the newest version instead of the
// pointer's; a FOR SHARE read blocks behind the held row lock; caching misses the move.
func TestResourceStore_PointerVersion(t *testing.T) {
	db := portTestDB(t)
	ctx := context.Background()
	seedOperation(t, db, distributionOperationID)
	resources := NewResourceStore(db)

	const skillName = "tender"
	v1 := seedSkillVersion(t, db, skillName, 1, "skills/tender/v1",
		[]core.FileHash{{Path: "SKILL.md", SHA256: distributionSHA256([]byte("v1")), Size: 2}})
	v2 := seedSkillVersion(t, db, skillName, 2, "skills/tender/v2",
		[]core.FileHash{{Path: "SKILL.md", SHA256: distributionSHA256([]byte("v2")), Size: 2}})

	upsertSkillPointer(t, db, skillName, v1, opNow.UnixMilli())
	got, err := resources.PointerVersion(ctx, core.ResourceSkill, skillName)
	require.NoError(t, err)
	require.Equal(t, v1, got, "指针停在 v1 时必须返回 v1，而不是已存在的最大版本 v2")

	upsertSkillPointer(t, db, skillName, v2, opNow.Add(time.Minute).UnixMilli())
	got, err = resources.PointerVersion(ctx, core.ResourceSkill, skillName)
	require.NoError(t, err)
	require.Equal(t, v2, got, "指针移动后必须立即读到新版本（不得缓存）")

	pointer, err := resources.Pointer(ctx, core.ResourceSkill, skillName)
	require.NoError(t, err)
	require.Equal(t, pointer.PublishedVersionID, got, "PointerVersion 与 Pointer 必须读同一份指针语义")

	_, err = resources.PointerVersion(ctx, core.ResourceSkill, "no-such-skill")
	require.ErrorIs(t, err, core.ErrResourceNotFound, "未命中的名称必须是 ErrResourceNotFound")

	// 单快照的另一半：读不得额外取锁。另一个事务锁住指针行期间，纯 MVCC 读立即
	// 返回已提交值；取共享行锁的实现会阻塞并被上下文超时打断。
	lockConn, err := db.pool.Conn(ctx)
	require.NoError(t, err)
	lockTx, err := lockConn.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = lockTx.ExecContext(ctx,
		`SELECT 1 FROM resource_pointers WHERE resource_kind = $1 AND resource_id = $2 FOR UPDATE`,
		string(core.ResourceSkill), skillName)
	require.NoError(t, err)

	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	got, err = resources.PointerVersion(readCtx, core.ResourceSkill, skillName)
	require.NoError(t, err, "指针读取不得阻塞在他人持有的行锁上（一次快照、不额外取锁）")
	require.Equal(t, v2, got)

	require.NoError(t, lockTx.Rollback())
	require.NoError(t, lockConn.Close())
}
