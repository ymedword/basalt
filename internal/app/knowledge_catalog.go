package app

// 本文件是**目录读面的装配层适配器**（US-16 / T042 的 `KnowledgeCatalog` 端口）：
// 把 `*postgres.KnowledgeCatalogStore` 的记录搬运成 `web` 包声明的消费方记录。
// 只做字段搬运，不重写任何判定：
//
//   - ACL、排序、软删口径、同名集合复活都在存储层（`knowledge_catalog.go` 的
//     五条纪律），这里**不得**再判一次——两处各判一次，迟早分叉；
//   - `DocumentCount` 原样透传：存储层永远给出真实计数，负数是留给未来带预算的
//     适配器的"未统计"语义，本层**不得**把未知改写成 0（0 是一个真实答案）；
//   - 版本状态从 `knowledge.VersionStatus` 转成契约字符串，闭集由领域保证。
//
// This file adapts the PostgreSQL catalog store to the web consumer port. It only maps
// shapes: ACL, ordering and soft-delete semantics live in the store, and the document
// count is passed through unchanged because "not counted" must stay distinguishable
// from an empty collection.

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/web"
)

// knowledgeCatalogAdapter 把存储层的目录读面接到 web 的消费方端口。
// knowledgeCatalogAdapter maps the catalog store onto the web consumer port.
type knowledgeCatalogAdapter struct {
	store *postgres.KnowledgeCatalogStore
}

// 编译期断言：端口形状由本类型满足。
// The adapter satisfies the consumer port.
var _ web.KnowledgeCatalog = (*knowledgeCatalogAdapter)(nil)

// newKnowledgeCatalogAdapter 构造适配器；存储句柄缺失由存储层各方法失败关闭。
// newKnowledgeCatalogAdapter builds the adapter; a missing handle fails closed in the store.
func newKnowledgeCatalogAdapter(store *postgres.KnowledgeCatalogStore) *knowledgeCatalogAdapter {
	return &knowledgeCatalogAdapter{store: store}
}

// ListCollections 列集合（管理面，契约无 Principal 参数）。
// ListCollections lists collections; the admin route already authenticated the caller.
func (a *knowledgeCatalogAdapter) ListCollections(ctx context.Context, project string,
	includeDeleted bool,
) ([]web.KnowledgeCollection, error) {
	records, err := a.store.ListCollections(ctx, project, includeDeleted)
	if err != nil {
		return nil, err
	}
	out := make([]web.KnowledgeCollection, 0, len(records))
	for _, record := range records {
		out = append(out, web.KnowledgeCollection{
			CollectionID:        record.CollectionID,
			Name:                record.Name,
			Project:             record.Project,
			Description:         record.Description,
			DescriptionRevision: record.DescriptionRevision,
			DeclaredAgents:      record.DeclaredAgents,
			DocumentCount:       record.DocumentCount,
			CreatedAt:           record.CreatedAt,
			DeletedAt:           record.DeletedAt,
		})
	}
	return out, nil
}

// CreateCollection 建集合；同名软删行在存储层复活，本层不感知。
// CreateCollection creates a collection; revival of a soft-deleted namesake is the
// store's concern.
func (a *knowledgeCatalogAdapter) CreateCollection(ctx context.Context, p core.Principal,
	name, project string,
) (web.KnowledgeCollection, error) {
	record, err := a.store.CreateCollection(ctx, p, name, project)
	if err != nil {
		return web.KnowledgeCollection{}, err
	}
	return web.KnowledgeCollection{
		CollectionID:        record.CollectionID,
		Name:                record.Name,
		Project:             record.Project,
		Description:         record.Description,
		DescriptionRevision: record.DescriptionRevision,
		DocumentCount:       record.DocumentCount,
		CreatedAt:           record.CreatedAt,
		DeletedAt:           record.DeletedAt,
	}, nil
}

// ListDocuments 列集合内文档；ACL 与排序由存储层保证。
// ListDocuments lists a collection's documents; ACL and ordering live in the store.
func (a *knowledgeCatalogAdapter) ListDocuments(ctx context.Context, p core.Principal,
	collectionID string, includeDeleted bool,
) ([]web.KnowledgeDocument, error) {
	records, err := a.store.ListDocuments(ctx, p, collectionID, includeDeleted)
	if err != nil {
		return nil, err
	}
	out := make([]web.KnowledgeDocument, 0, len(records))
	for _, record := range records {
		out = append(out, web.KnowledgeDocument{
			DocumentID:      record.DocumentID,
			CollectionID:    record.CollectionID,
			SourcePath:      record.SourcePath,
			ContentHash:     record.ContentHash,
			LatestVersionID: record.LatestVersionID,
			LatestStatus:    record.LatestStatus,
			CreatedAt:       record.CreatedAt,
			UpdatedAt:       record.UpdatedAt,
			DeletedAt:       record.DeletedAt,
		})
	}
	return out, nil
}

// ListVersions 列文档版本；`ActivatedAt` 的推导在存储层（active 行的最后写者）。
// ListVersions lists document versions; the activation instant is derived by the store.
func (a *knowledgeCatalogAdapter) ListVersions(ctx context.Context, p core.Principal,
	documentID string,
) ([]web.KnowledgeDocumentVersion, error) {
	records, err := a.store.ListVersions(ctx, p, documentID)
	if err != nil {
		return nil, err
	}
	out := make([]web.KnowledgeDocumentVersion, 0, len(records))
	for _, record := range records {
		out = append(out, web.KnowledgeDocumentVersion{
			VersionID:      record.VersionID,
			DocumentID:     record.DocumentID,
			Revision:       record.Revision,
			ContentHash:    record.ContentHash,
			Status:         string(record.Status),
			ParserVersion:  record.ParserVersion,
			ChunkerVersion: record.ChunkerVersion,
			Generation:     record.Generation,
			ErrorKind:      record.ErrorKind,
			CreatedAt:      record.CreatedAt,
			ActivatedAt:    record.ActivatedAt,
		})
	}
	return out, nil
}

func (a *knowledgeCatalogAdapter) ListIngestAttempts(ctx context.Context, documentID string) ([]web.KnowledgeIngestAttempt, bool, error) {
	rows, truncated, err := a.store.ListIngestAttempts(ctx, documentID)
	if err != nil {
		return nil, false, err
	}
	out := make([]web.KnowledgeIngestAttempt, 0, len(rows))
	for _, row := range rows {
		out = append(out, web.KnowledgeIngestAttempt{
			JobID: row.JobID, VersionID: row.VersionID, Attempt: row.Attempt,
			Stage: row.Stage, Status: row.Status, ErrorKind: row.ErrorKind, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt,
		})
	}
	return out, truncated, nil
}
