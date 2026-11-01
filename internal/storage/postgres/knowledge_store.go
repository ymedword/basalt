package postgres

// 本文件是知识库的**存储适配层**（US-16 §3.1.3「领域仓储适配」行 / T045），在同一份
// `kb_*` 模式上实现三个消费方窄端口：
//
//   - `knowledge.IngestStore` 与 `knowledge.SourceReader`：`knowledgeStore` 一个类型同时
//     实现两者——它们没有同名方法，且读写的是同一批行；
//   - `knowledge.KeywordSearcher` / `knowledge.VectorSearcher`：**两个独立类型**
//     （`keywordSearcher` / `vectorSearcher`）。两者的 `Search` 签名不同（一个收查询
//     文本，一个收查询向量与代际），Go 没有方法重载，同一类型无法同时满足；把参数
//     退化成 `any` 会丢掉编译期检查。上层只按接口取用，不需要知道这里拆过类型。
//
// 四条纪律：
//
//  1. **状态机只在这里落库**：`pending → indexed → active`，`active` 是唯一生效点；
//     `ActivateVersion` 在同一事务里置新 active、supersede 旧 active、更新
//     `latest_version_id`——三件事分开做就会出现"两个 active"或"没有 active"的窗口。
//  2. **过滤在 SQL 内**：检索的两条路径都把可见性（`versions.status = 'active'`、
//     文档与集合的 `deleted_at IS NULL`、集合范围）写进 `WHERE`，被剔除的行**只回
//     计数**、不回标识（C-08）；在 Go 侧过滤会让被拒片段先进入进程内存。
//  3. **时间是显式入参**：写方法都收 `at time.Time`，统一 `at.UTC().UnixMilli()` 落
//     BIGINT 毫秒（C-13 / C-14）；只有 `WriteChunks` 的建行时刻取句柄时钟（端口签名
//     没有时间参数）。
//  4. **装配缺失按失败关闭**：没有库句柄时一律返回错误，**不得**用空结果冒充"没有数据"。
//
// This file is the storage adapter for the knowledge base. One type implements
// IngestStore and SourceReader; the keyword and vector searchers must be two separate
// types because Go has no method overloading and their Search signatures differ.
// Visibility filtering happens inside the SQL, and every write takes an explicit clock.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/knowledge"
)

const (
	// knowledgeDocumentIDPrefix / knowledgeVersionIDPrefix / knowledgeChunkIDPrefix
	// 是本层分配的三类主键前缀。冻结契约只要求"不透明字符串"，没有规定形态；沿用
	// `quota_store.go` 的 Crockford 载荷，让日志里能一眼看出标识属于哪张表。
	// The three id prefixes keep the keys opaque but recognisable in logs.
	knowledgeDocumentIDPrefix = "doc_"
	knowledgeVersionIDPrefix  = "kver_"
	knowledgeChunkIDPrefix    = "chunk_"
)

var (
	// errKnowledgeStoreUnconfigured 是装配错误：没有数据库句柄时拒绝服务。
	// errKnowledgeStoreUnconfigured fails closed when the handle is missing.
	errKnowledgeStoreUnconfigured = errors.New("postgres: knowledge store has no database handle")

	// errKnowledgeInvalidArgument 表示入参形状非法（空标识、空正文、零维度）。
	// errKnowledgeInvalidArgument rejects malformed arguments.
	errKnowledgeInvalidArgument = errors.New("postgres: invalid knowledge argument")

	// errKnowledgeStateConflict 表示行的当前状态不允许这次操作（如对 `active` 版本
	// 再开摄取、对非 `pending` 版本写片段）。这类冲突**不得**被当成成功吞掉。
	// errKnowledgeStateConflict rejects a transition the row's current state forbids.
	errKnowledgeStateConflict = errors.New("postgres: knowledge row state conflict")

	// errKnowledgeIngestInFlight 表示同一版本还有一条 `pending` / `running` 作业——
	// 并发的第二次摄取**不得**与在途尝试同时写同一版本的片段。
	// errKnowledgeIngestInFlight rejects a second attempt while one job is still open.
	errKnowledgeIngestInFlight = errors.New("postgres: another ingest attempt is in flight")

	// errKnowledgeArtifactsIncomplete 表示激活前的产物核对未通过：片段为空，或该代际
	// 的向量数与片段数不等。版本**不得**带着残缺产物生效。
	// errKnowledgeArtifactsIncomplete blocks activation on incomplete artifacts.
	errKnowledgeArtifactsIncomplete = errors.New("postgres: version artifacts are incomplete")

	// errKnowledgeGenerationMissing 表示要激活到的索引代际不在 `kb_index_generations`。
	// 代际行由启动期代际管理建立，缺失说明装配或迁移有缺口。
	// errKnowledgeGenerationMissing reports a generation row that does not exist.
	errKnowledgeGenerationMissing = errors.New("postgres: index generation does not exist")
)

// knowledgeStore 是 `knowledge.IngestStore` 与 `knowledge.SourceReader` 的 PG 实现。
// knowledgeStore implements the ingest store and the source reader.
type knowledgeStore struct {
	db *DB
}

// 编译期断言：两个消费方端口都由本类型满足。
// The implementation satisfies both consumer ports.
var (
	_ knowledge.IngestStore     = (*knowledgeStore)(nil)
	_ knowledge.SourceReader    = (*knowledgeStore)(nil)
	_ knowledge.KeywordSearcher = (*keywordSearcher)(nil)
	_ knowledge.VectorSearcher  = (*vectorSearcher)(nil)
)

// NewKnowledgeStore 构造知识库读写存储；返回类型不导出，调用方按端口使用。
// NewKnowledgeStore builds the store; the concrete type stays unexported.
func NewKnowledgeStore(db *DB) *knowledgeStore {
	return &knowledgeStore{db: db}
}

// ready 是所有方法的统一前置：句柄缺失即失败关闭。
// ready is the shared fail-closed precondition.
func (s *knowledgeStore) ready() error {
	if s == nil || s.db == nil || s.db.pool == nil {
		return errKnowledgeStoreUnconfigured
	}
	return nil
}

// FindActiveByContentHash 在同集合内找同内容哈希的 active 版本（去重路径）。已软删的
// 文档与集合不算命中——否则删除后再上传同内容会直接"复活"一份不可见的版本。
// FindActiveByContentHash is the dedup lookup; soft-deleted documents never match.
func (s *knowledgeStore) FindActiveByContentHash(ctx context.Context, collectionID, contentHash string) (knowledge.VersionRecord, bool, error) {
	if err := s.ready(); err != nil {
		return knowledge.VersionRecord{}, false, err
	}
	if strings.TrimSpace(collectionID) == "" || strings.TrimSpace(contentHash) == "" {
		return knowledge.VersionRecord{}, false, errKnowledgeInvalidArgument
	}
	var (
		record     knowledge.VersionRecord
		status     string
		generation int
	)
	err := s.db.pool.QueryRowContext(ctx, `
		SELECT v.version_id, v.document_id, v.content_hash, v.status,
		       COALESCE(v.generation, 0),
		       (SELECT COUNT(*) FROM kb_chunks c WHERE c.version_id = v.version_id)
		FROM kb_document_versions v
		JOIN kb_documents d ON d.document_id = v.document_id
		JOIN kb_collections col ON col.collection_id = d.collection_id
		WHERE d.collection_id = $1
		  AND v.content_hash = $2
		  AND v.status = 'active'
		  AND d.deleted_at IS NULL
		  AND col.deleted_at IS NULL
		LIMIT 1`,
		collectionID, contentHash,
	).Scan(&record.VersionID, &record.DocumentID, &record.ContentHash, &status, &generation, &record.ChunkCount)
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.VersionRecord{}, false, nil
	}
	if err != nil {
		return knowledge.VersionRecord{}, false, fmt.Errorf("查询同内容哈希的 active 版本: %w", err)
	}
	record.Status = knowledge.VersionStatus(status)
	record.Generation = generation
	return record, true, nil
}

// StartJob 原子地建（或命中）文档行、建（或复用）版本行，并开一条 `running` 作业。
//
// 锁序是 **集合 `FOR SHARE` → 文档 `FOR UPDATE` → 版本 `FOR UPDATE`**：与
// `ActivateVersion` 同向，不会与只锁版本的 `WriteChunks` 成环。
// 集合共享锁的含义是"这条摄取期间集合不得被软删"，读完放行。
//
// 复用规则：只检查该文档的最新版本；显式重建和已退役的历史版本新建行。
//   - 最新且内容相同的 `failed`：复位 `pending`、刷新解析器版本、清错误与代际；
//   - `pending` / `indexed`：**只有**在没有任何 `pending` / `running` 作业时才允许接管
//     （上次尝试崩在途中），否则说明确有并发尝试在跑，返回 `errKnowledgeIngestInFlight`；
//   - `active`：同内容已经有生效版本，第二次摄取**不得**把它打回 `pending`（那会让
//     在服务的版本瞬间退出检索），返回 `errKnowledgeStateConflict`——正常路径在
//     `FindActiveByContentHash` 就该命中去重，走到这里说明两次摄取抢在了激活之前。
//
// StartJob allocates the document, reuses or creates the version row, and opens one
// running job. The lock order matches ActivateVersion; a version is only taken over
// when no other job is still open.
func (s *knowledgeStore) StartJob(ctx context.Context, req knowledge.StartJobRequest) (knowledge.JobRecord, error) {
	if err := s.ready(); err != nil {
		return knowledge.JobRecord{}, err
	}
	if strings.TrimSpace(req.JobID) == "" || strings.TrimSpace(req.CollectionID) == "" ||
		strings.TrimSpace(req.SourcePath) == "" || strings.TrimSpace(req.ContentHash) == "" {
		return knowledge.JobRecord{}, errKnowledgeInvalidArgument
	}
	at := req.At.UTC().UnixMilli()

	var out knowledge.JobRecord
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 1. 集合共享锁：存在且未软删，否则整条摄取按"目标不存在"拒绝。
		var collectionID string
		err := tx.QueryRowContext(ctx, `
			SELECT collection_id FROM kb_collections
			WHERE collection_id = $1 AND deleted_at IS NULL
			FOR SHARE`, req.CollectionID).Scan(&collectionID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 集合 %s", knowledge.ErrNotFound, req.CollectionID)
		}
		if err != nil {
			return fmt.Errorf("锁定集合 %s: %w", req.CollectionID, err)
		}

		// 2. 文档行：先锁后建，杜绝两个并发摄取各自建一条同 (collection, path) 的文档。
		documentID, deleted, err := lockOrCreateDocument(ctx, tx, req, at)
		if err != nil {
			return err
		}
		if req.ExpectedDocumentID != "" && (req.ExpectedDocumentID != documentID || deleted) {
			return fmt.Errorf("%w: 重建时文档已删除或发生变化", knowledge.ErrNotFound)
		}
		if deleted {
			// 软删文档被同路径重新摄取：复活它。同 (collection, path) 只允许一行，
			// 不复活就只能换路径——那会丢掉既有版本的引用连续性。
			if _, err := tx.ExecContext(ctx, `
				UPDATE kb_documents SET deleted_at = NULL, updated_at = $2
				WHERE document_id = $1`, documentID, at); err != nil {
				return fmt.Errorf("复活文档 %s: %w", documentID, err)
			}
		}

		// 3. 根据内容、重建标记与最新版本状态决定复用或新建。
		versionID, err := lockOrCreateVersion(ctx, tx, documentID, req, at)
		if err != nil {
			return err
		}

		// 4. 作业序号：同一版本上每次尝试一行，从 1 起。
		var attempt int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(attempt), 0) + 1 FROM kb_ingest_jobs
			WHERE version_id = $1`, versionID).Scan(&attempt); err != nil {
			return fmt.Errorf("计算作业序号: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO kb_ingest_jobs
				(ingest_job_id, document_id, version_id, stage, attempt, status, started_at)
			VALUES ($1, $2, $3, $4, $5, 'running', $6)`,
			req.JobID, documentID, versionID, knowledge.JobStageParse, attempt, at); err != nil {
			return fmt.Errorf("开摄取作业 %s: %w", req.JobID, err)
		}

		out = knowledge.JobRecord{
			JobID:      req.JobID,
			DocumentID: documentID,
			VersionID:  versionID,
			Attempt:    attempt,
		}
		return nil
	})
	if err != nil {
		return knowledge.JobRecord{}, err
	}
	return out, nil
}

// lockOrCreateDocument 取文档行并加行锁；不存在时建行。返回是否处于软删状态。
// lockOrCreateDocument locks the document row, creating it when absent.
func lockOrCreateDocument(ctx context.Context, tx *sql.Tx, req knowledge.StartJobRequest, at int64) (string, bool, error) {
	var (
		documentID string
		deletedAt  sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT document_id, deleted_at FROM kb_documents
		WHERE collection_id = $1 AND source_path = $2
		FOR UPDATE`, req.CollectionID, req.SourcePath).Scan(&documentID, &deletedAt)
	switch {
	case err == nil:
		return documentID, deletedAt.Valid, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, fmt.Errorf("锁定文档 %s: %w", req.SourcePath, err)
	}

	// 行不存在：先插后查。`ON CONFLICT DO NOTHING` 覆盖"另一事务刚插入并提交"的窗口，
	// 冲突时回到 `FOR UPDATE` 取那一行——不靠捕获唯一约束的报错来分支。
	newID, err := newKnowledgeID(knowledgeDocumentIDPrefix)
	if err != nil {
		return "", false, err
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO kb_documents
			(document_id, collection_id, source_path, content_hash, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $5)
		ON CONFLICT (collection_id, source_path) DO NOTHING`,
		newID, req.CollectionID, req.SourcePath, req.ContentHash, at)
	if err != nil {
		return "", false, fmt.Errorf("建文档 %s: %w", req.SourcePath, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", false, fmt.Errorf("建文档 %s 的行数: %w", req.SourcePath, err)
	}
	if affected == 1 {
		return newID, false, nil
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT document_id, deleted_at FROM kb_documents
		WHERE collection_id = $1 AND source_path = $2
		FOR UPDATE`, req.CollectionID, req.SourcePath).Scan(&documentID, &deletedAt); err != nil {
		return "", false, fmt.Errorf("回读并发建立的文档 %s: %w", req.SourcePath, err)
	}
	return documentID, deletedAt.Valid, nil
}

// lockOrCreateVersion 取版本行并加行锁；不存在时建 `pending` 行，已存在时按复用规则
// 复位。`active` 与"仍有在途作业"的两条拒绝路径见 `StartJob` 的注释。
// lockOrCreateVersion locks or creates the version row and applies the reuse rules.
func lockOrCreateVersion(ctx context.Context, tx *sql.Tx, documentID string, req knowledge.StartJobRequest, at int64) (string, error) {
	if req.ForceNewVersion {
		return createKnowledgeVersion(ctx, tx, documentID, req, at)
	}
	var (
		versionID   string
		status      string
		contentHash string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT version_id, status, content_hash FROM kb_document_versions
		WHERE document_id = $1
		ORDER BY revision DESC LIMIT 1 FOR UPDATE`, documentID).Scan(&versionID, &status, &contentHash)
	if errors.Is(err, sql.ErrNoRows) {
		return createKnowledgeVersion(ctx, tx, documentID, req, at)
	}
	if err != nil {
		return "", fmt.Errorf("锁定版本行: %w", err)
	}
	if contentHash != req.ContentHash || knowledge.VersionStatus(status) == knowledge.VersionSuperseded ||
		knowledge.VersionStatus(status) == knowledge.VersionRetired {
		return createKnowledgeVersion(ctx, tx, documentID, req, at)
	}

	if knowledge.VersionStatus(status) == knowledge.VersionActive {
		return "", fmt.Errorf("%w: 版本 %s 已是 active", errKnowledgeStateConflict, versionID)
	}
	if knowledge.VersionStatus(status) == knowledge.VersionPending ||
		knowledge.VersionStatus(status) == knowledge.VersionIndexed {
		var open int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM kb_ingest_jobs
			WHERE version_id = $1 AND status IN ('pending', 'running')`,
			versionID).Scan(&open); err != nil {
			return "", fmt.Errorf("检查在途作业: %w", err)
		}
		if open > 0 {
			return "", fmt.Errorf("%w: 版本 %s", errKnowledgeIngestInFlight, versionID)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE kb_document_versions
		SET status = 'pending', parser_version = $2, chunker_version = $3,
		    error_kind = NULL, generation = NULL,
		    source_text = COALESCE(source_text, $5), updated_at = $4
		WHERE version_id = $1`,
		versionID, req.ParserVersion, req.ChunkerVersion, at,
		sourceTextOrNull(req.SourceText)); err != nil {
		return "", fmt.Errorf("复位版本 %s: %w", versionID, err)
	}
	return versionID, nil
}

func createKnowledgeVersion(ctx context.Context, tx *sql.Tx, documentID string, req knowledge.StartJobRequest, at int64) (string, error) {
	newID, err := newKnowledgeID(knowledgeVersionIDPrefix)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO kb_document_versions
			(version_id, document_id, content_hash, status, parser_version, chunker_version,
			 source_text, created_at, updated_at)
		VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7, $7)`,
		newID, documentID, req.ContentHash, req.ParserVersion, req.ChunkerVersion,
		sourceTextOrNull(req.SourceText), at); err != nil {
		return "", fmt.Errorf("建版本行 %s: %w", newID, err)
	}
	return newID, nil
}

// WriteChunks 在版本维度重写切块：先删该版本在**所有代际**的向量，再删片段，最后按
// 草稿顺序插入。重试因此不会留下上一轮的孤儿行（`UNIQUE (version_id, seq)` 也不会
// 撞上旧序号）。只锁版本行、不取文档锁，与 `ActivateVersion` 的 文档 → 版本 同向无环。
// WriteChunks rewrites the version's chunks; it locks the version row only, so it cannot
// deadlock against ActivateVersion's document-then-version order.
func (s *knowledgeStore) WriteChunks(ctx context.Context, versionID string, chunks []knowledge.ChunkDraft) ([]knowledge.ChunkRef, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(versionID) == "" {
		return nil, errKnowledgeInvalidArgument
	}
	at := s.db.clock().UTC().UnixMilli()

	refs := make([]knowledge.ChunkRef, 0, len(chunks))
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var status string
		err := tx.QueryRowContext(ctx, `
			SELECT status FROM kb_document_versions WHERE version_id = $1 FOR UPDATE`,
			versionID).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 版本 %s", knowledge.ErrNotFound, versionID)
		}
		if err != nil {
			return fmt.Errorf("锁定版本 %s: %w", versionID, err)
		}
		if knowledge.VersionStatus(status) != knowledge.VersionPending {
			return fmt.Errorf("%w: 版本 %s 的状态是 %s，只有 pending 可写片段",
				errKnowledgeStateConflict, versionID, status)
		}

		if _, err := tx.ExecContext(ctx, `
			DELETE FROM kb_chunk_embeddings
			WHERE chunk_id IN (SELECT chunk_id FROM kb_chunks WHERE version_id = $1)`,
			versionID); err != nil {
			return fmt.Errorf("清理版本 %s 的旧向量: %w", versionID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM kb_chunks WHERE version_id = $1`, versionID); err != nil {
			return fmt.Errorf("清理版本 %s 的旧片段: %w", versionID, err)
		}

		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO kb_chunks
				(chunk_id, version_id, seq, heading_path, char_start, char_end, text, content_hash, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`)
		if err != nil {
			return fmt.Errorf("准备片段插入: %w", err)
		}
		defer stmt.Close()

		for _, draft := range chunks {
			chunkID, err := newKnowledgeID(knowledgeChunkIDPrefix)
			if err != nil {
				return err
			}
			if _, err := stmt.ExecContext(ctx, chunkID, versionID, draft.Seq, draft.HeadingPath,
				draft.CharStart, draft.CharEnd, draft.Text, draft.ContentHash, at); err != nil {
				return fmt.Errorf("写版本 %s 的第 %d 条片段: %w", versionID, draft.Seq, err)
			}
			refs = append(refs, knowledge.ChunkRef{ChunkID: chunkID, Seq: draft.Seq})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return refs, nil
}

// MarkIndexed 把版本从 `pending` 置 `indexed`：产物齐全但**尚未**参与检索。
// MarkIndexed flips pending to indexed; the version is not yet visible.
func (s *knowledgeStore) MarkIndexed(ctx context.Context, versionID string, at time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(versionID) == "" {
		return errKnowledgeInvalidArgument
	}
	millis := at.UTC().UnixMilli()
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE kb_document_versions SET status = 'indexed', updated_at = $2
			WHERE version_id = $1 AND status = 'pending'`, versionID, millis)
		if err != nil {
			return fmt.Errorf("置版本 %s 为 indexed: %w", versionID, err)
		}
		return requireSingleVersionUpdate(ctx, tx, res, versionID, "pending")
	})
}

// ActivateVersion 是**唯一生效点**：同一事务里置新 active、把同文档旧 active 置
// `superseded`、更新 `latest_version_id` 与 `content_hash`，并返回被取代的版本标识。
//
// 生效前的三道 fail-closed 核对：目标版本的当前状态必须是 `indexed`；代际行必须存在；
// 该代际的向量数必须与片段数相等且非空。任一条不满足都不生效——半个版本进检索比
// 检索失败更难挽回。
//
// ActivateVersion is the single visibility point. It refuses to activate a version whose
// artifacts are incomplete, because a half-indexed version in retrieval is worse than an
// error.
func (s *knowledgeStore) ActivateVersion(ctx context.Context, versionID string, generation int, at time.Time) (string, error) {
	return s.activateVersion(ctx, versionID, generation, at, nil)
}

func (s *knowledgeStore) ActivateReindex(ctx context.Context, principal core.Principal, versionID string, generation int, at time.Time) (string, error) {
	return s.activateVersion(ctx, versionID, generation, at, &principal)
}

func (s *knowledgeStore) AuthorizeReindex(ctx context.Context, principal core.Principal, documentID string) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return s.authorizeReindexTx(ctx, tx, principal, documentID)
	})
}

// authorizeReindexTx locks current caller/key and collection authority; a revoked
// key or changed project cannot race past the activation commit.
func (s *knowledgeStore) authorizeReindexTx(ctx context.Context, tx *sql.Tx, principal core.Principal, documentID string) error {
	if !principal.Valid() || !principal.HasScope(core.ScopeKnowledgeWrite) {
		return core.ErrUnauthorized
	}
	var project string
	err := tx.QueryRowContext(ctx, `SELECT project FROM callers WHERE caller_id = $1 AND status = 'active' FOR SHARE`, principal.CallerID).Scan(&project)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrUnauthorized
	}
	if err != nil {
		return fmt.Errorf("检查重建调用方: %w", err)
	}
	if principal.Kind == core.PrincipalCaller {
		var valid bool
		err = tx.QueryRowContext(ctx, `
			SELECT revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $3) AND $4 = ANY(scopes)
			FROM caller_keys WHERE caller_id = $1 AND key_id = $2 FOR SHARE`,
			principal.CallerID, principal.KeyID, s.db.clock().UTC().UnixMilli(), core.ScopeKnowledgeWrite).Scan(&valid)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !valid) {
			return core.ErrUnauthorized
		}
		if err != nil {
			return fmt.Errorf("检查重建授权密钥: %w", err)
		}
	}
	var collectionProject string
	err = tx.QueryRowContext(ctx, `
		SELECT c.project FROM kb_documents d JOIN kb_collections c ON c.collection_id = d.collection_id
		WHERE d.document_id = $1 AND d.deleted_at IS NULL AND c.deleted_at IS NULL FOR SHARE OF c`, documentID).Scan(&collectionProject)
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("检查重建所属集合: %w", err)
	}
	if project != "" && project != collectionProject {
		return core.ErrForbidden
	}
	return nil
}

func (s *knowledgeStore) activateVersion(ctx context.Context, versionID string, generation int, at time.Time, principal *core.Principal) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	if strings.TrimSpace(versionID) == "" || generation < 1 {
		return "", errKnowledgeInvalidArgument
	}
	millis := at.UTC().UnixMilli()

	var superseded string
	err := s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 1. 先读取身份，再按集合 → 文档 → 版本的统一顺序取锁。
		var (
			documentID   string
			contentHash  string
			collectionID string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT v.document_id, v.content_hash, d.collection_id
			FROM kb_document_versions v JOIN kb_documents d ON d.document_id = v.document_id
			WHERE v.version_id = $1`, versionID).Scan(&documentID, &contentHash, &collectionID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 版本 %s", knowledge.ErrNotFound, versionID)
		}
		if err != nil {
			return fmt.Errorf("读版本 %s: %w", versionID, err)
		}
		if principal != nil {
			if err := s.authorizeReindexTx(ctx, tx, *principal, documentID); err != nil {
				return err
			}
		}

		var collectionDeleted sql.NullInt64
		err = tx.QueryRowContext(ctx, `
			SELECT deleted_at FROM kb_collections WHERE collection_id = $1 FOR SHARE`,
			collectionID).Scan(&collectionDeleted)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 集合 %s", knowledge.ErrNotFound, collectionID)
		}
		if err != nil {
			return fmt.Errorf("锁定集合 %s: %w", collectionID, err)
		}
		if collectionDeleted.Valid {
			return fmt.Errorf("%w: 集合 %s 已软删", knowledge.ErrNotFound, collectionID)
		}
		// 2. 文档锁：软删中的文档不得再激活版本。
		var (
			lockedCollectionID string
			deletedAt          sql.NullInt64
		)
		err = tx.QueryRowContext(ctx, `
			SELECT collection_id, deleted_at FROM kb_documents
			WHERE document_id = $1 FOR UPDATE`, documentID).Scan(&lockedCollectionID, &deletedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 文档 %s", knowledge.ErrNotFound, documentID)
		}
		if err != nil {
			return fmt.Errorf("锁定文档 %s: %w", documentID, err)
		}
		if deletedAt.Valid {
			return fmt.Errorf("%w: 文档 %s 已软删", knowledge.ErrNotFound, documentID)
		}

		if lockedCollectionID != collectionID {
			return fmt.Errorf("%w: 文档 %s 所属集合已变化", errKnowledgeStateConflict, documentID)
		}

		// 3. 版本锁与状态核对。
		var status string
		if err := tx.QueryRowContext(ctx, `
			SELECT status FROM kb_document_versions WHERE version_id = $1 FOR UPDATE`,
			versionID).Scan(&status); err != nil {
			return fmt.Errorf("锁定版本 %s: %w", versionID, err)
		}
		if knowledge.VersionStatus(status) != knowledge.VersionIndexed {
			return fmt.Errorf("%w: 版本 %s 的状态是 %s，只有 indexed 可激活",
				errKnowledgeStateConflict, versionID, status)
		}
		var latest bool
		if err := tx.QueryRowContext(ctx, `
			SELECT revision = (SELECT MAX(revision) FROM kb_document_versions WHERE document_id = $2)
			FROM kb_document_versions WHERE version_id = $1`, versionID, documentID).Scan(&latest); err != nil {
			return fmt.Errorf("检查版本顺序 %s: %w", versionID, err)
		}
		if !latest {
			return fmt.Errorf("%w: 版本 %s 已被较新的作业取代", errKnowledgeStateConflict, versionID)
		}

		// 4. 代际与产物核对。
		var generationExists int
		err = tx.QueryRowContext(ctx, `
			SELECT 1 FROM kb_index_generations WHERE generation = $1`, generation).Scan(&generationExists)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 代际 %d", errKnowledgeGenerationMissing, generation)
		}
		if err != nil {
			return fmt.Errorf("核对代际 %d: %w", generation, err)
		}
		if err := requireCompleteArtifacts(ctx, tx, versionID, generation); err != nil {
			return err
		}

		// 5. 旧 active 先让位：部分唯一索引 `ux_kb_document_versions_active` 不允许
		//    同一文档同时有两条 active，顺序反过来会撞索引。
		var previous string
		err = tx.QueryRowContext(ctx, `
			UPDATE kb_document_versions
			SET status = 'superseded', updated_at = $3
			WHERE document_id = $1 AND status = 'active' AND version_id <> $2
			RETURNING version_id`, documentID, versionID, millis).Scan(&previous)
		switch {
		case err == nil:
			superseded = previous
		case errors.Is(err, sql.ErrNoRows):
			// 没有旧 active：首次生效。
		default:
			return fmt.Errorf("取代文档 %s 的旧 active 版本: %w", documentID, err)
		}

		// 6. 新版本生效。
		if _, err := tx.ExecContext(ctx, `
			UPDATE kb_document_versions
			SET status = 'active', generation = $2, error_kind = NULL, updated_at = $3
			WHERE version_id = $1`, versionID, generation, millis); err != nil {
			return fmt.Errorf("激活版本 %s: %w", versionID, err)
		}

		// 7. 文档指向最新版本（循环外键在两张表都建好后补，写入顺序不受限）。
		if _, err := tx.ExecContext(ctx, `
			UPDATE kb_documents
			SET latest_version_id = $2, content_hash = $3, updated_at = $4
			WHERE document_id = $1`, documentID, versionID, contentHash, millis); err != nil {
			return fmt.Errorf("更新文档 %s 的最新版本: %w", documentID, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return superseded, nil
}

// requireCompleteArtifacts 核对激活前的产物：片段非空，且该代际的向量数与片段数相等。
// requireCompleteArtifacts refuses an empty chunk set or a vector count mismatch.
func requireCompleteArtifacts(ctx context.Context, tx *sql.Tx, versionID string, generation int) error {
	var chunkCount, vectorCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunks WHERE version_id = $1`, versionID).Scan(&chunkCount); err != nil {
		return fmt.Errorf("统计版本 %s 的片段: %w", versionID, err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM kb_chunk_embeddings e
		JOIN kb_chunks c ON c.chunk_id = e.chunk_id
		WHERE c.version_id = $1 AND e.generation = $2`, versionID, generation).Scan(&vectorCount); err != nil {
		return fmt.Errorf("统计版本 %s 的向量: %w", versionID, err)
	}
	if chunkCount == 0 || vectorCount != chunkCount {
		return fmt.Errorf("%w: 版本 %s 有 %d 条片段、%d 条代际 %d 的向量",
			errKnowledgeArtifactsIncomplete, versionID, chunkCount, vectorCount, generation)
	}
	return nil
}

// FailVersion 把版本置 `failed` 并写 `error_kind`。**拒绝**把 `active` 版本打成失败：
// 那等于让一个在服务的版本悄悄退出检索——出现这种情况说明调用方搞错了版本，按错误上抛。
// FailVersion never flips an active version to failed.
func (s *knowledgeStore) FailVersion(ctx context.Context, versionID, errorKind string, at time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(versionID) == "" || strings.TrimSpace(errorKind) == "" {
		return errKnowledgeInvalidArgument
	}
	millis := at.UTC().UnixMilli()
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE kb_document_versions
			SET status = 'failed', error_kind = $2, updated_at = $3
			WHERE version_id = $1 AND status IN ('pending', 'indexed')`, versionID, errorKind, millis)
		if err != nil {
			return fmt.Errorf("置版本 %s 为 failed: %w", versionID, err)
		}
		return requireSingleVersionUpdate(ctx, tx, res, versionID, "pending 或 indexed")
	})
}

// FinishJob 写作业终态：stage、status、`error_kind`（空串落 NULL）与完成时刻。
// FinishJob records the terminal job row; an empty error kind becomes NULL.
func (s *knowledgeStore) FinishJob(ctx context.Context, jobID, stage, status, errorKind string, at time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(jobID) == "" || strings.TrimSpace(stage) == "" || strings.TrimSpace(status) == "" {
		return errKnowledgeInvalidArgument
	}
	millis := at.UTC().UnixMilli()
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE kb_ingest_jobs
			SET stage = $2, status = $3, error_kind = NULLIF($4, ''), finished_at = $5
			WHERE ingest_job_id = $1`, jobID, stage, status, errorKind, millis)
		if err != nil {
			return fmt.Errorf("写作业 %s 的终态: %w", jobID, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("写作业 %s 终态的行数: %w", jobID, err)
		}
		if affected == 0 {
			return fmt.Errorf("%w: 作业 %s", knowledge.ErrNotFound, jobID)
		}
		return nil
	})
}

// Document 读文档行；不存在返回 `knowledge.ErrNotFound`。软删的文档照常返回并置
// `Deleted`——由调用方判定，存储层不替它假装不存在。
// Document reports soft deletion instead of hiding the row.
func (s *knowledgeStore) Document(ctx context.Context, documentID string) (knowledge.DocumentRecord, error) {
	if err := s.ready(); err != nil {
		return knowledge.DocumentRecord{}, err
	}
	if strings.TrimSpace(documentID) == "" {
		return knowledge.DocumentRecord{}, errKnowledgeInvalidArgument
	}
	var (
		record    knowledge.DocumentRecord
		deletedAt sql.NullInt64
	)
	err := s.db.pool.QueryRowContext(ctx, `
		SELECT document_id, collection_id, source_path, content_hash, deleted_at
		FROM kb_documents WHERE document_id = $1`, documentID).
		Scan(&record.DocumentID, &record.CollectionID, &record.SourcePath, &record.ContentHash, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.DocumentRecord{}, fmt.Errorf("%w: 文档 %s", knowledge.ErrNotFound, documentID)
	}
	if err != nil {
		return knowledge.DocumentRecord{}, fmt.Errorf("读文档 %s: %w", documentID, err)
	}
	record.Deleted = deletedAt.Valid
	return record, nil
}

// SoftDeleteDocument 软删文档并把其 active 版本置 `retired`。重复删除是幂等的：
// 第二次调用不改任何行，也不报错（契约要求重复删除幂等）。
// SoftDeleteDocument is idempotent; only the active version is retired.
func (s *knowledgeStore) SoftDeleteDocument(ctx context.Context, documentID string, at time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(documentID) == "" {
		return errKnowledgeInvalidArgument
	}
	millis := at.UTC().UnixMilli()
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM kb_documents WHERE document_id = $1 FOR UPDATE`, documentID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 文档 %s", knowledge.ErrNotFound, documentID)
		}
		if err != nil {
			return fmt.Errorf("锁定文档 %s: %w", documentID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE kb_documents SET deleted_at = $2, updated_at = $2
			WHERE document_id = $1 AND deleted_at IS NULL`, documentID, millis); err != nil {
			return fmt.Errorf("软删文档 %s: %w", documentID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE kb_document_versions SET status = 'retired', updated_at = $2
			WHERE document_id = $1 AND status IN ('active', 'pending', 'indexed')`, documentID, millis); err != nil {
			return fmt.Errorf("退役文档 %s 的生效及在途版本: %w", documentID, err)
		}
		return nil
	})
}

// SoftDeleteCollection 软删集合并把集合内所有 active 版本置 `retired`。
// 幂等口径与 `SoftDeleteDocument` 相同。
// SoftDeleteCollection retires every active version under the collection.
func (s *knowledgeStore) SoftDeleteCollection(ctx context.Context, collectionID string, at time.Time) error {
	if err := s.ready(); err != nil {
		return err
	}
	if strings.TrimSpace(collectionID) == "" {
		return errKnowledgeInvalidArgument
	}
	millis := at.UTC().UnixMilli()
	return s.db.withTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM kb_collections WHERE collection_id = $1 FOR UPDATE`, collectionID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: 集合 %s", knowledge.ErrNotFound, collectionID)
		}
		if err != nil {
			return fmt.Errorf("锁定集合 %s: %w", collectionID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE kb_collections SET deleted_at = $2
			WHERE collection_id = $1 AND deleted_at IS NULL`, collectionID, millis); err != nil {
			return fmt.Errorf("软删集合 %s: %w", collectionID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE kb_document_versions SET status = 'retired', updated_at = $2
			WHERE status IN ('active', 'pending', 'indexed')
			  AND document_id IN (SELECT document_id FROM kb_documents WHERE collection_id = $1)`,
			collectionID, millis); err != nil {
			return fmt.Errorf("退役集合 %s 的 active 版本: %w", collectionID, err)
		}
		return nil
	})
}

// reservedGapRune 是**没有原文快照**时重建正文的空隙填充字符：切分器把片段两侧的
// 空白与标题行都裁掉，这里用换行占位，**保证片段仍落在原文的绝对字符偏移上**。
// 补出来的正文只在 sha256 与 `content_hash` 相符时才算原文（见 `rebuildVersionText`）。
// The gap filler keeps every chunk at its absolute rune offset during reconstruction.
const reservedGapRune = '\n'

// LoadVersion 按 (document_id, version_id) 精确读取一个版本，**不**改取最新版本。
//
// `Text` 只回**逐字原文**：优先取 `source_text` 快照（摄取时落库）；没有快照的老行
// 退回按片段重建，且只有重建结果的 sha256 等于 `content_hash` 时才采用——否则回空串，
// 由来源视图按"正文未保留"处理。切分器会裁掉标题行与首尾空白，重建结果与原文不同
// 却返回原始哈希，就是拿近似文本冒充原文。
//
// LoadVersion loads exactly the named version and returns only verbatim text: the
// stored snapshot, or a reconstruction whose sha256 matches content_hash; otherwise
// the body is empty and the caller must not claim it as the original.
func (s *knowledgeStore) LoadVersion(ctx context.Context, documentID, versionID string) (knowledge.SourceVersionRecord, error) {
	if err := s.ready(); err != nil {
		return knowledge.SourceVersionRecord{}, err
	}
	if strings.TrimSpace(documentID) == "" || strings.TrimSpace(versionID) == "" {
		return knowledge.SourceVersionRecord{}, errKnowledgeInvalidArgument
	}

	var (
		record            knowledge.SourceVersionRecord
		status            string
		sourceText        sql.NullString
		documentDeletedAt sql.NullInt64
		collectionDeleted sql.NullInt64
	)
	err := s.db.pool.QueryRowContext(ctx, `
		SELECT v.document_id, v.version_id, v.content_hash, v.status, v.source_text,
		       d.collection_id, d.source_path, d.deleted_at, col.deleted_at
		FROM kb_document_versions v
		JOIN kb_documents d ON d.document_id = v.document_id
		JOIN kb_collections col ON col.collection_id = d.collection_id
		WHERE v.version_id = $1 AND v.document_id = $2`, versionID, documentID).
		Scan(&record.DocumentID, &record.VersionID, &record.ContentHash, &status, &sourceText,
			&record.CollectionID, &record.SourcePath, &documentDeletedAt, &collectionDeleted)
	if errors.Is(err, sql.ErrNoRows) {
		return knowledge.SourceVersionRecord{}, fmt.Errorf("%w: 版本 %s/%s",
			knowledge.ErrNotFound, documentID, versionID)
	}
	if err != nil {
		return knowledge.SourceVersionRecord{}, fmt.Errorf("读版本 %s/%s: %w", documentID, versionID, err)
	}
	record.Status = knowledge.VersionStatus(status)
	record.DocumentDeleted = documentDeletedAt.Valid
	record.CollectionDeleted = collectionDeleted.Valid
	switch {
	case documentDeletedAt.Valid:
		record.DeletedAt = time.UnixMilli(documentDeletedAt.Int64).UTC()
	case collectionDeleted.Valid:
		record.DeletedAt = time.UnixMilli(collectionDeleted.Int64).UTC()
	}

	text, err := versionText(ctx, s.db.pool, versionID, record.ContentHash, sourceText)
	if err != nil {
		return knowledge.SourceVersionRecord{}, err
	}
	record.Text = text
	// 摘要只描述来源本身（K-02）：标题取来源路径的文件名，章节标题与摘要留空——
	// 引用点名的是一整个版本，没有"这一段属于哪一节"的信息，**不得**猜一个。
	record.Summary = knowledge.SourceSummary{DocumentTitle: documentTitle(record.SourcePath)}
	return record, nil
}

// versionText 取版本的**逐字原文**：有 `source_text` 快照就直接用（摄取时按原文
// 落库，sha256 必然等于 `content_hash`）；否则按片段重建，只有重建结果的 sha256
// 与 `content_hash` 相符才采用，否则回空串——**不得**把近似重建当成原文返回。
// versionText returns the verbatim body from the snapshot, or a reconstruction only
// when its sha256 matches content_hash; otherwise it returns "".
func versionText(ctx context.Context, pool *sql.DB, versionID, contentHash string, snapshot sql.NullString) (string, error) {
	if snapshot.Valid && snapshot.String != "" {
		// A stored snapshot is not evidence of integrity on its own: verify its
		// bytes against the version digest before serving even a clipped page.
		if sha256HexString(snapshot.String) != contentHash {
			return "", nil
		}
		return snapshot.String, nil
	}
	rebuilt, err := loadVersionText(ctx, pool, versionID)
	if err != nil {
		return "", err
	}
	if rebuilt == "" || sha256HexString(rebuilt) != contentHash {
		return "", nil
	}
	return rebuilt, nil
}

// sha256HexString 是小写十六进制 sha256，与 `kb_document_versions.content_hash` 同形。
// sha256HexString returns the lower-case hex sha256 used by content_hash columns.
func sha256HexString(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// sourceTextOrNull 把空串写成 NULL：`source_text` 的 NULL 表示"未保留"，与空文档区分。
// sourceTextOrNull stores "" as NULL so "not retained" stays distinguishable.
func sourceTextOrNull(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// loadVersionText 按绝对字符偏移把片段写回缓冲：片段正文逐字来自原文（切分器的
// `Text == string(runes[CharStart:CharEnd])` 契约），同节重叠部分内容一致，后写覆盖
// 也不会改变字节。调用方**必须**再用 `content_hash` 校验结果是否等于原文。
// loadVersionText writes each chunk back at its absolute offset; callers must verify
// the result against content_hash before treating it as the original.
func loadVersionText(ctx context.Context, pool *sql.DB, versionID string) (string, error) {
	rows, err := pool.QueryContext(ctx, `
		SELECT char_start, char_end, text FROM kb_chunks
		WHERE version_id = $1 ORDER BY seq`, versionID)
	if err != nil {
		return "", fmt.Errorf("读版本 %s 的片段: %w", versionID, err)
	}
	defer rows.Close()

	type span struct {
		start int
		end   int
		text  string
	}
	var spans []span
	longest := 0
	for rows.Next() {
		var s span
		if err := rows.Scan(&s.start, &s.end, &s.text); err != nil {
			return "", fmt.Errorf("读版本 %s 的片段行: %w", versionID, err)
		}
		if s.end > longest {
			longest = s.end
		}
		spans = append(spans, s)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("遍历版本 %s 的片段: %w", versionID, err)
	}
	if longest <= 0 {
		return "", nil
	}

	buf := make([]rune, longest)
	for i := range buf {
		buf[i] = reservedGapRune
	}
	for _, s := range spans {
		for i, r := range []rune(s.text) {
			pos := s.start + i
			if pos < 0 || pos >= s.end || pos >= longest {
				// 落库区间与正文长度不一致：按区间截断，不越界写到别的片段上。
				break
			}
			buf[pos] = r
		}
	}
	return string(buf), nil
}

// documentTitle 取来源路径的文件名（去扩展名）作标题；路径为空时返回空串。
// documentTitle derives the title from the source path.
func documentTitle(sourcePath string) string {
	base := path.Base(sourcePath)
	if base == "." || base == "/" {
		return ""
	}
	return strings.TrimSuffix(base, path.Ext(base))
}

// requireSingleVersionUpdate 在同事务内区分"版本不存在"与"状态不允许"：条件更新
// 影响 0 行时回读一次状态，前者报 `ErrNotFound`，后者报状态冲突。
// requireSingleVersionUpdate tells a missing row apart from a forbidden transition.
func requireSingleVersionUpdate(ctx context.Context, tx *sql.Tx, res sql.Result, versionID, required string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("统计版本 %s 的更新行数: %w", versionID, err)
	}
	if affected > 0 {
		return nil
	}
	var status string
	err = tx.QueryRowContext(ctx, `
		SELECT status FROM kb_document_versions WHERE version_id = $1`, versionID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: 版本 %s", knowledge.ErrNotFound, versionID)
	}
	if err != nil {
		return fmt.Errorf("回读版本 %s 的状态: %w", versionID, err)
	}
	return fmt.Errorf("%w: 版本 %s 的状态是 %s，要求 %s",
		errKnowledgeStateConflict, versionID, status, required)
}

// keywordSearcher 是关键词路径的实现（`tsv` 生成列 + `pg_trgm` + 原文子串）。
//
// 三路匹配合并成一条 SQL：原文子串（中文短查询最可靠）、`websearch_to_tsquery` 的词元
// 匹配（英文与数字）与 `word_similarity` 的三元组近似（错字 / 词形变化）。打分优先级
// 与 `ORDER BY` 一致：子串命中恒为 1.0，词元命中按 `ts_rank_cd` 落在 0.5～1.0，
// 其余按 `word_similarity`。名次由返回顺序决定，`mergeRRF` 按序取 `1/(k+i+1)`。
//
// The keyword path merges exact substring, token match and trigram similarity into one
// statement; the row order is the rank that RRF consumes.
type keywordSearcher struct {
	db *DB
}

// NewKnowledgeKeywordSearcher 构造关键词检索适配器；调用方按 `knowledge.KeywordSearcher` 使用。
// NewKnowledgeKeywordSearcher builds the keyword adapter.
func NewKnowledgeKeywordSearcher(db *DB) *keywordSearcher {
	return &keywordSearcher{db: db}
}

// Search 在给定范围内做关键词检索；`denied` 是匹配上但被可见性条件剔除的行数。
// The denied count reports rows that matched but were filtered out by visibility.
func (s *keywordSearcher) Search(ctx context.Context, scope knowledge.Scope, query string, k int) ([]knowledge.Hit, int, error) {
	if s == nil || s.db == nil || s.db.pool == nil {
		return nil, 0, errKnowledgeStoreUnconfigured
	}
	q := strings.TrimSpace(query)
	if q == "" || len(scope.Collections) == 0 || k <= 0 {
		return nil, 0, nil
	}

	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT c.chunk_id, v.document_id, v.version_id, d.collection_id, d.source_path,
		       c.heading_path, v.content_hash, c.text,
		       CASE
		         WHEN position(lower($1) IN lower(c.text)) > 0 THEN 1.0
		         WHEN c.tsv @@ websearch_to_tsquery('simple', $1)
		           THEN 0.5 + 0.5 * ts_rank_cd(c.tsv, websearch_to_tsquery('simple', $1))
		         ELSE word_similarity($1, c.text)
		       END AS score
		FROM kb_chunks c
		JOIN kb_document_versions v ON v.version_id = c.version_id
		JOIN kb_documents d ON d.document_id = v.document_id
		JOIN kb_collections col ON col.collection_id = d.collection_id
		WHERE d.collection_id = ANY($2::text[])
		  AND v.status = 'active'
		  AND d.deleted_at IS NULL
		  AND col.deleted_at IS NULL
		  AND (c.tsv @@ websearch_to_tsquery('simple', $1)
		       OR position(lower($1) IN lower(c.text)) > 0
		       OR $1 <% c.text)
		ORDER BY score DESC, c.chunk_id ASC
		LIMIT $3::bigint`, q, scope.Collections, k)
	if err != nil {
		return nil, 0, fmt.Errorf("关键词检索: %w", err)
	}
	hits, err := scanKnowledgeHits(rows)
	if err != nil {
		return nil, 0, fmt.Errorf("读关键词检索结果: %w", err)
	}

	var denied int
	if err := s.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM kb_chunks c
		JOIN kb_document_versions v ON v.version_id = c.version_id
		JOIN kb_documents d ON d.document_id = v.document_id
		JOIN kb_collections col ON col.collection_id = d.collection_id
		WHERE NOT (d.collection_id = ANY($2::text[])
		           AND v.status = 'active'
		           AND d.deleted_at IS NULL
		           AND col.deleted_at IS NULL)
		  AND (c.tsv @@ websearch_to_tsquery('simple', $1)
		       OR position(lower($1) IN lower(c.text)) > 0
		       OR $1 <% c.text)`, q, scope.Collections).Scan(&denied); err != nil {
		return nil, 0, fmt.Errorf("统计关键词路被拒行数: %w", err)
	}
	return hits, denied, nil
}

// vectorSearcher 是向量路径的实现：只查调用方固定的一代（C-09），距离算子按代际行
// 的 `distance` 三选一（cosine / l2 / inner_product），过滤同样写在 SQL 里。
// The vector path reads exactly one generation and picks the distance operator from the
// generation row.
type vectorSearcher struct {
	db *DB
}

// NewKnowledgeVectorSearcher 构造向量检索适配器；调用方按 `knowledge.VectorSearcher` 使用。
// NewKnowledgeVectorSearcher builds the vector adapter.
func NewKnowledgeVectorSearcher(db *DB) *vectorSearcher {
	return &vectorSearcher{db: db}
}

// Search 在固定代际上做向量检索。向量以字面量字符串经 `::vector` 转型传入——不引入
// pgvector 的 Go 编解码包，少一个依赖方向（原则 VIII 依赖最小化）。
// Search passes the embedding as a vector literal; no pgvector Go codec is needed.
func (s *vectorSearcher) Search(ctx context.Context, scope knowledge.Scope, embedding []float32, generation, k int) ([]knowledge.Hit, int, error) {
	if s == nil || s.db == nil || s.db.pool == nil {
		return nil, 0, errKnowledgeStoreUnconfigured
	}
	if len(scope.Collections) == 0 || k <= 0 {
		return nil, 0, nil
	}
	if len(embedding) != knowledge.VectorDimensions {
		return nil, 0, fmt.Errorf("%w: 查询向量维度 %d 与冻结维度 %d 不符",
			errKnowledgeInvalidArgument, len(embedding), knowledge.VectorDimensions)
	}
	if generation < 1 {
		return nil, 0, fmt.Errorf("%w: 索引代际 %d", errKnowledgeInvalidArgument, generation)
	}
	literal, err := vectorLiteral(embedding)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.db.pool.QueryContext(ctx, `
		SELECT c.chunk_id, v.document_id, v.version_id, d.collection_id, d.source_path,
		       c.heading_path, v.content_hash, c.text,
		       CASE g.distance
		         WHEN 'cosine' THEN 1.0 - (e.embedding <=> $1::vector)
		         WHEN 'l2' THEN 1.0 / (1.0 + (e.embedding <-> $1::vector))
		         ELSE -(e.embedding <#> $1::vector)
		       END AS score
		FROM kb_chunk_embeddings e
		JOIN kb_index_generations g ON g.generation = e.generation
		JOIN kb_chunks c ON c.chunk_id = e.chunk_id
		JOIN kb_document_versions v ON v.version_id = c.version_id
		JOIN kb_documents d ON d.document_id = v.document_id
		JOIN kb_collections col ON col.collection_id = d.collection_id
		WHERE e.generation = $2
		  AND d.collection_id = ANY($3::text[])
		  AND v.status = 'active'
		  AND d.deleted_at IS NULL
		  AND col.deleted_at IS NULL
		ORDER BY CASE g.distance
		           WHEN 'cosine' THEN e.embedding <=> $1::vector
		           WHEN 'l2' THEN e.embedding <-> $1::vector
		           ELSE e.embedding <#> $1::vector
		         END ASC, c.chunk_id ASC
		LIMIT $4::bigint`, literal, generation, scope.Collections, k)
	if err != nil {
		return nil, 0, fmt.Errorf("向量检索: %w", err)
	}
	hits, err := scanKnowledgeHits(rows)
	if err != nil {
		return nil, 0, fmt.Errorf("读向量检索结果: %w", err)
	}

	var denied int
	if err := s.db.pool.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM kb_chunk_embeddings e
		JOIN kb_chunks c ON c.chunk_id = e.chunk_id
		JOIN kb_document_versions v ON v.version_id = c.version_id
		JOIN kb_documents d ON d.document_id = v.document_id
		JOIN kb_collections col ON col.collection_id = d.collection_id
		WHERE e.generation = $1
		  AND NOT (d.collection_id = ANY($2::text[])
		           AND v.status = 'active'
		           AND d.deleted_at IS NULL
		           AND col.deleted_at IS NULL)`,
		generation, scope.Collections).Scan(&denied); err != nil {
		return nil, 0, fmt.Errorf("统计向量路被拒行数: %w", err)
	}
	return hits, denied, nil
}

// scanKnowledgeHits 把两条检索路径共用的结果列读成 `knowledge.Hit`：名次、融合分数与
// Token 估算由上层覆写，这里只填业务字段（`Rank` / `TokenEstimate` 留零）。
// scanKnowledgeHits fills the business fields; rank and token estimate are set upstairs.
func scanKnowledgeHits(rows *sql.Rows) ([]knowledge.Hit, error) {
	defer rows.Close()
	var hits []knowledge.Hit
	for rows.Next() {
		var hit knowledge.Hit
		if err := rows.Scan(&hit.ChunkID, &hit.DocumentID, &hit.VersionID, &hit.CollectionID,
			&hit.SourcePath, &hit.HeadingPath, &hit.ContentHash, &hit.Text, &hit.Score); err != nil {
			return nil, err
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// vectorLiteral 把查询向量编成 pgvector 的字面量 `[a,b,c]`；非有限值直接拒绝——
// `NaN` / `Inf` 交给数据库转型只会得到一条难读的 SQL 报错。
// vectorLiteral rejects non-finite components before the cast reaches Postgres.
func vectorLiteral(embedding []float32) (string, error) {
	var b strings.Builder
	b.Grow(len(embedding)*12 + 2)
	b.WriteByte('[')
	for i, v := range embedding {
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "", fmt.Errorf("%w: 查询向量第 %d 维不是有限值", errKnowledgeInvalidArgument, i+1)
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(f, 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String(), nil
}

// newKnowledgeID 生成 `<前缀><26 位 Crockford Base32>` 的不透明主键；随机源失败即错误。
// newKnowledgeID mints an opaque key with the shared Crockford payload.
func newKnowledgeID(prefix string) (string, error) {
	var buf [reservationPayloadLength]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成知识库标识随机源: %w", err)
	}
	var b strings.Builder
	b.Grow(len(prefix) + reservationPayloadLength)
	b.WriteString(prefix)
	for _, v := range buf {
		b.WriteByte(crockfordAlphabet[int(v)%len(crockfordAlphabet)])
	}
	return b.String(), nil
}
