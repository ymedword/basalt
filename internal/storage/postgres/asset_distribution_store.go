package postgres

// 本文件是已发布资产分发的 PostgreSQL 仓储（US-25C §3.1.3）：写面只有内容寻址的
// 载荷插入，读面把增量发现（复合键集 `(updated_at, resource_id)`）与全量兜底
// （只按 `resource_id`）分成两个方法——合成一个方法就必然有一边被时间列绑架。
// 载荷表**没有**清理 / 裁剪入口（D-25C-09）：本 US 只做内容寻址去重，
// `PutBundlePayload` 之外不提供任何写入或删除路径。
//
// `ReadSkillVersion` 复用既有读法（`ResourceStore.VersionByID` + 与
// `internal/app/resources.go` 的 `parseSkillManifest` 同一套失败关闭规则）；
// storage 不得反向依赖 app（后者消费 storage），因此解析规则在这里以
// `parseSkillVersionFiles` 落地，两处语义必须保持一致。
// The PostgreSQL store of published-asset distribution. Payloads are content addressed
// with no prune path in this US; the keyset (incremental) and resource_id-ordered
// (full scan) readings stay separate, and skill manifests fail closed when corrupt.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

// BundlePayloadStore 是载荷写入面（`agent_bundle_payloads`）：内容寻址、幂等。
// BundlePayloadStore is the content-addressed payload write face.
type BundlePayloadStore struct{ db *DB }

// 编译期断言：满足 core 声明的载荷写端口。
// Compile-time assertion that the store satisfies the payload write port.
var _ core.BundlePayloadStore = (*BundlePayloadStore)(nil)

// NewBundlePayloadStore 绑定一个已打开的库句柄。
// NewBundlePayloadStore binds the store to an opened database handle.
func NewBundlePayloadStore(db *DB) *BundlePayloadStore { return &BundlePayloadStore{db: db} }

// PutBundlePayload 写入一条载荷记录：`ON CONFLICT DO NOTHING` 之后按
// `payload_sha256` 复核原行——同一份字节重复写入是幂等 no-op；同一 `bundle_hash`
// 对应不同载荷必须是明确错误，原行逐字不变（既不覆盖也不新增行）。
// PutBundlePayload inserts idempotently by content address; a different payload under
// the same hash is an explicit error and the original row stays byte-unchanged.
func (s *BundlePayloadStore) PutBundlePayload(ctx context.Context, rec core.BundlePayloadRecord) error {
	result, err := s.db.pool.ExecContext(ctx, `
		INSERT INTO agent_bundle_payloads
		   (bundle_hash, agent, agent_uid, format_version, payload, payload_sha256,
		    size_bytes, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (bundle_hash) DO NOTHING`,
		rec.BundleHash, rec.Agent, string(rec.AgentUID), rec.FormatVersion,
		rec.Payload, rec.PayloadSHA256, rec.SizeBytes, rec.CreatedAt)
	if err != nil {
		return fmt.Errorf("写入 bundle 载荷 %s: %w", rec.BundleHash, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("写入 bundle 载荷 %s: 读取影响行数: %w", rec.BundleHash, err)
	}
	if affected > 0 {
		return nil
	}

	// 冲突分支：原行已存在。内容寻址的判据是载荷哈希——同哈希即同一份字节，
	// 不同哈希说明调用方拿同一 bundle 身份提交了另一份载荷，必须拒绝而不是沿用。
	var storedSHA string
	err = s.db.pool.QueryRowContext(ctx,
		`SELECT payload_sha256 FROM agent_bundle_payloads WHERE bundle_hash = $1`,
		rec.BundleHash).Scan(&storedSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("写入 bundle 载荷 %s: 冲突后未找到原行", rec.BundleHash)
	}
	if err != nil {
		return fmt.Errorf("写入 bundle 载荷 %s: 复核原行: %w", rec.BundleHash, err)
	}
	if storedSHA != rec.PayloadSHA256 {
		return fmt.Errorf("bundle 载荷 %s 已存在且内容不同（存储 %s，提交 %s）：拒绝覆盖",
			rec.BundleHash, storedSHA, rec.PayloadSHA256)
	}
	return nil
}

// AssetDistributionStore 是分发读取面：增量键集、全量兜底、载荷读、技能版本读。
// AssetDistributionStore is the distribution read face.
type AssetDistributionStore struct{ db *DB }

// 编译期断言：满足 core 声明的分发读取端口。
// Compile-time assertion that the store satisfies the distribution read port.
var _ core.AssetDistributionPort = (*AssetDistributionStore)(nil)

// NewAssetDistributionStore 绑定一个已打开的库句柄。
// NewAssetDistributionStore binds the store to an opened database handle.
func NewAssetDistributionStore(db *DB) *AssetDistributionStore {
	return &AssetDistributionStore{db: db}
}

// publishedBundleColumns 是增量与全量两条查询共用的列序：指针的
// `resource_id` / `updated_at` 与版本行的清单（身份三项从清单里解出）。
// publishedBundleColumns is the shared column order of the two pointer listings.
const publishedBundleColumns = `p.resource_id, p.updated_at, v.manifest_json`

// ListPublishedBundlesAfter 是增量发现：按 `(updated_at, resource_id)` 复合键集
// **严格前进**（不是 `updated_at >= $1`——那会在同毫秒边界重复读；也不是单列
// `updated_at > $1`——那会漏掉同毫秒的其余行），墓碑不出现，游标越过末行返回空页。
// ListPublishedBundlesAfter advances strictly on the (updated_at, resource_id) keyset.
func (s *AssetDistributionStore) ListPublishedBundlesAfter(ctx context.Context, cursor core.PublishedBundleCursor, limit int) ([]core.PublishedBundle, error) {
	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT `+publishedBundleColumns+`
		  FROM resource_pointers p
		  JOIN resource_versions v ON v.id = p.published_version_id
		 WHERE p.resource_kind = $1
		   AND p.deleted_at IS NULL
		   AND (p.updated_at, p.resource_id) > ($2::bigint, $3::text)
		 ORDER BY p.updated_at ASC, p.resource_id ASC
		 LIMIT $4`,
		string(core.ResourceAgentBundle), cursor.UpdatedAt, cursor.ResourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("增量列出已发布 bundle: %w", err)
	}
	defer func() { _ = rows.Close() }()

	bundles, err := scanPublishedBundles(rows)
	if err != nil {
		return nil, fmt.Errorf("增量列出已发布 bundle: %w", err)
	}
	return bundles, nil
}

// ListPublishedBundlesPage 是全量兜底：**只按 `resource_id` 排序**、与时间列无关
// ——迟到提交（更新时间早于已走过游标）靠它补齐；墓碑不出现；读取路径不写库、
// 不删除任何缺席者（移除归同步层在整趟完成后回读确认）。
// ListPublishedBundlesPage is the resource_id-ordered full scan; it never writes.
func (s *AssetDistributionStore) ListPublishedBundlesPage(ctx context.Context, afterResourceID string, limit int) ([]core.PublishedBundle, error) {
	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT `+publishedBundleColumns+`
		  FROM resource_pointers p
		  JOIN resource_versions v ON v.id = p.published_version_id
		 WHERE p.resource_kind = $1
		   AND p.deleted_at IS NULL
		   AND p.resource_id > $2
		 ORDER BY p.resource_id ASC
		 LIMIT $3`,
		string(core.ResourceAgentBundle), afterResourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("全量列出已发布 bundle: %w", err)
	}
	defer func() { _ = rows.Close() }()

	bundles, err := scanPublishedBundles(rows)
	if err != nil {
		return nil, fmt.Errorf("全量列出已发布 bundle: %w", err)
	}
	return bundles, nil
}

// scanPublishedBundles 是两条列表查询的唯一扫描实现：身份三项从版本行的
// `manifest_json`（`assets.Manifest`）解出，`UpdatedAt` 用存储写入的毫秒值。
// 清单损坏或身份字段缺失按失败关闭报错——返回一条空身份的记录会让同步层
// 把 bundle 装到错误的 Agent 名下。
// scanPublishedBundles is the shared scan path; a corrupt manifest fails closed.
func scanPublishedBundles(rows *sql.Rows) ([]core.PublishedBundle, error) {
	var bundles []core.PublishedBundle
	for rows.Next() {
		var (
			resourceID   string
			updatedAt    int64
			manifestJSON sql.NullString
		)
		if err := rows.Scan(&resourceID, &updatedAt, &manifestJSON); err != nil {
			return nil, fmt.Errorf("扫描已发布 bundle: %w", err)
		}
		bundle, err := publishedBundleFromManifest(resourceID, updatedAt, manifestJSON.String)
		if err != nil {
			return nil, err
		}
		bundles = append(bundles, bundle)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历已发布 bundle: %w", err)
	}
	return bundles, nil
}

// publishedBundleFromManifest 把一行指针投影解成身份三项；`resource_id` 与清单的
// AgentUID 必须一致（`agent_bundle` 指针按 UID 寻址），不一致说明行被外部改写。
// publishedBundleFromManifest projects one pointer row; a name/uid mismatch is corrupt.
func publishedBundleFromManifest(resourceID string, updatedAt int64, raw string) (core.PublishedBundle, error) {
	var manifest assets.Manifest
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		return core.PublishedBundle{}, fmt.Errorf("解析已发布 bundle 清单 %s: %w", resourceID, err)
	}
	if manifest.Agent == "" || manifest.AgentUID == "" || manifest.BundleHash == "" {
		return core.PublishedBundle{}, fmt.Errorf("已发布 bundle 清单 %s 缺少 Agent / AgentUID / BundleHash", resourceID)
	}
	if string(manifest.AgentUID) != resourceID {
		return core.PublishedBundle{}, fmt.Errorf("已发布 bundle 指针 %s 与清单身份 %s 不一致", resourceID, manifest.AgentUID)
	}
	return core.PublishedBundle{
		Agent:      manifest.Agent,
		AgentUID:   manifest.AgentUID,
		BundleHash: manifest.BundleHash,
		UpdatedAt:  updatedAt,
	}, nil
}

// ReadBundlePayload 按哈希读载荷；未命中返回 `(零值, false, nil)`，不伪造记录。
// ReadBundlePayload reads one payload by content hash; a miss is (zero, false, nil).
func (s *AssetDistributionStore) ReadBundlePayload(ctx context.Context, hash string) (core.BundlePayloadRecord, bool, error) {
	var (
		record   core.BundlePayloadRecord
		agentUID string
	)
	err := s.db.pool.QueryRowContext(ctx, `
		SELECT bundle_hash, agent, agent_uid, format_version, payload, payload_sha256,
		       size_bytes, created_at
		  FROM agent_bundle_payloads
		 WHERE bundle_hash = $1`, hash).Scan(
		&record.BundleHash, &record.Agent, &agentUID, &record.FormatVersion,
		&record.Payload, &record.PayloadSHA256, &record.SizeBytes, &record.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return core.BundlePayloadRecord{}, false, nil
	}
	if err != nil {
		return core.BundlePayloadRecord{}, false, fmt.Errorf("读取 bundle 载荷 %s: %w", hash, err)
	}
	record.AgentUID = core.AgentUID(agentUID)
	return record, true, nil
}

// ReadSkillVersion 按**版本 ID** 读技能发布记录（不是「最新版本」）：`Name` 是版本行
// 的 `resource_id`，`StoragePath` 原样返回（工作区相对），`Files` 来自该版本行的
// `manifest_json`。未命中或版本不属于技能种类返回 `(零值, false, nil)`。
// ReadSkillVersion reads the pinned skill version by id, never the newest one.
func (s *AssetDistributionStore) ReadSkillVersion(ctx context.Context, versionID int64) (core.SkillVersionRecord, bool, error) {
	version, err := NewResourceStore(s.db).VersionByID(ctx, versionID)
	if errors.Is(err, core.ErrResourceNotFound) {
		return core.SkillVersionRecord{}, false, nil
	}
	if err != nil {
		return core.SkillVersionRecord{}, false, fmt.Errorf("读取技能版本 %d: %w", versionID, err)
	}
	if version.Kind != core.ResourceSkill {
		return core.SkillVersionRecord{}, false, nil
	}
	files, err := parseSkillVersionFiles(version.ResourceID, version.ManifestJSON)
	if err != nil {
		return core.SkillVersionRecord{}, false, err
	}
	return core.SkillVersionRecord{
		Name:        version.ResourceID,
		VersionID:   version.ID,
		StoragePath: version.StoragePath,
		Files:       files,
	}, true, nil
}

// parseSkillVersionFiles 与 `internal/app/resources.go` 的 `parseSkillManifest` 同一套
// 失败关闭规则：必须是文件数组，`[]` 是合法空列表，`null` / 空原文 / 空路径都按
// 损坏行报错——不当成空技能继续（否则会冻结一个运行期必然缺 SKILL.md 的版本）。
// parseSkillVersionFiles mirrors parseSkillManifest's fail-closed rules.
func parseSkillVersionFiles(name, raw string) ([]core.FileHash, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("技能 %s 的发布版本缺少文件清单", name)
	}
	var files []core.FileHash
	if err := json.Unmarshal([]byte(raw), &files); err != nil {
		return nil, fmt.Errorf("技能 %s 的文件清单无法解析: %w", name, err)
	}
	if files == nil {
		return nil, fmt.Errorf("技能 %s 的文件清单不是数组", name)
	}
	for _, file := range files {
		if file.Path == "" {
			return nil, fmt.Errorf("技能 %s 的文件清单含空路径", name)
		}
	}
	return files, nil
}
