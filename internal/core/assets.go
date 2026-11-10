package core

// 本文件声明**已发布资产分发**的消费方端口与记录（US-25C 模块卡 §3.1.2）：载荷仓储
// 是内容寻址的只写面，分发读取面把「增量发现（复合键集）」与「全量兜底（按
// resource_id）」分成两个方法——两者的排序语义不同，合成一个方法就必然有一边被
// 时间列绑架。技能物化只按 bundle 清单里**固定的版本 ID** 读取，不解析「最新版本」。
//
// 本 US 刻意**不提供**载荷清理 / 裁剪方法（D-25C-09）：只做内容寻址去重，存储有界
// 不在交付范围内，任何调用方不得假设载荷会被回收。
// This file declares the asset-distribution consumer ports: a content-addressed payload
// store and a read side that keeps the keyset (incremental) and resource_id-ordered
// (full scan) readings apart. There is deliberately no prune method in this US.

import "context"

// BundlePayloadRecord 是一份已发布 bundle 的确定性载荷记录（`agent_bundle_payloads`
// 行的领域形态）。身份是 `BundleHash`（内容寻址）：同一哈希只允许一份载荷，
// `PayloadSHA256` 与 `SizeBytes` 是同一份字节的派生值，读回时逐字段核对。
// `CreatedAt` 是存储写入的 Unix 毫秒值，由 Go 写入（表不设数据库 DEFAULT）。
// BundlePayloadRecord is one content-addressed payload row; CreatedAt is written by Go
// in Unix milliseconds and every field is re-read to verify the stored bytes.
type BundlePayloadRecord struct {
	BundleHash    string
	Agent         string
	AgentUID      AgentUID
	FormatVersion int
	Payload       []byte
	PayloadSHA256 string
	SizeBytes     int64
	CreatedAt     int64
}

// PublishedBundle 是一条已发布的 Agent bundle 指针投影，身份三项都从版本行的
// `manifest_json` 解出（指针表本身只有 `resource_id` 与版本外键）。
// `UpdatedAt` 是**存储写入的毫秒值**，不是提交时刻——键集游标按它前进，不得用本地
// 时钟从提交时间重算。
// PublishedBundle is one published bundle pointer; UpdatedAt is the stored millisecond
// value, never recomputed from the committing process's clock.
type PublishedBundle struct {
	Agent      string
	AgentUID   AgentUID
	BundleHash string
	UpdatedAt  int64
}

// PublishedBundleCursor 是增量发现的复合键集游标：服务端按
// `(updated_at, resource_id) > (cursor.UpdatedAt, cursor.ResourceID)` 严格前进。
// `agent_bundle` 种类下 `ResourceID` 就是 Agent UID；零值游标 = 从最早一行开始。
// PublishedBundleCursor is the strictly advancing (updated_at, resource_id) keyset
// cursor; the zero value starts from the earliest row.
type PublishedBundleCursor struct {
	UpdatedAt  int64
	ResourceID string
}

// SkillVersionRecord 是技能发布版本的物化输入：`VersionID` 是 bundle 清单里固定的
// 那一版（不是「最新版」），`StoragePath` 是工作区相对路径，`Files` 是逐文件清单，
// 物化层靠它逐文件核对哈希。`Name` 是版本行的 `resource_id`（技能名）。
// SkillVersionRecord is the materialization input for one pinned skill version: the
// file list comes from that version row, and StoragePath is workspace-relative.
type SkillVersionRecord struct {
	Name        string
	VersionID   int64
	StoragePath string
	Files       []FileHash
}

// AssetDistributionPort 是分发读取面：增量与全量分开，未命中用 `(零值, false, nil)`
// 表达而不是伪记录或错误。
//
//   - `ListPublishedBundlesAfter` 按 `(updated_at, resource_id)` 键集前进，只返回未删除
//     的指针；同毫秒多行每行只出现一次，游标越过末行后返回空页。
//   - `ListPublishedBundlesPage` 是全量兜底：只按 `resource_id` 排序翻页，与时间列无关
//     ——迟到提交（更新时间早于已走过游标）靠它补齐；读取路径不写库、不删缺席者。
//   - `ReadBundlePayload` 按哈希读载荷。
//   - `ReadSkillVersion` 按**版本 ID** 读技能发布记录。
//
// AssetDistributionPort is the distribution read face: keyset incremental discovery and
// a resource_id-ordered full scan are separate methods, and misses are `(zero, false, nil)`.
type AssetDistributionPort interface {
	ListPublishedBundlesAfter(ctx context.Context, cursor PublishedBundleCursor, limit int) ([]PublishedBundle, error)
	ListPublishedBundlesPage(ctx context.Context, afterResourceID string, limit int) ([]PublishedBundle, error)
	ReadBundlePayload(ctx context.Context, hash string) (BundlePayloadRecord, bool, error)
	ReadSkillVersion(ctx context.Context, versionID int64) (SkillVersionRecord, bool, error)
}

// BundlePayloadStore 是载荷写入面（发布路径的插入点）：内容寻址、幂等。
// 同一 `BundleHash` 重复写入是 no-op；同哈希但 `payload_sha256` 不同必须是明确错误，
// 且原有行逐字不变——不得静默沿用旧行，也不得覆盖。
// BundlePayloadStore is the content-addressed payload write face: same hash is a no-op,
// a different payload under the same hash is an explicit error, and the original row is
// never rewritten.
type BundlePayloadStore interface {
	PutBundlePayload(ctx context.Context, rec BundlePayloadRecord) error
}
