package web

// 本文件是知识库十个端点的**响应投影**（US-16 / T042）：形状逐字段取自冻结契约
// `docs/contracts/openapi.yaml` 的知识组（`CollectionSummary` / `DocumentSummary` /
// `IngestResult` / `DocumentVersionSummary` / `SearchResult` / `SourceViewResult`），
// 沿用两条既有视图口径——数组恒为 `[]`（不是 `null`）、时间经 `formatTime` 渲染
// 为 RFC3339 UTC（零值空串）。
//
// 三条**投影级**约束（不是格式化偏好）：
//
//  1. `SearchHit` **没有** `content_hash`：`knowledge.Hit` 带该字段是给装配记录用的，
//     投影层**不得**把它放出去——契约靠"命中不带哈希"保证检索结果在签名层面就
//     不能充当引用依据。
//  2. `SourceViewResult.Superseded` **不投影**：它是 Go 层的"当时版本"标注，契约
//     没有这个字段（R50.2）；另按契约与 R50.2 的口径，`validity.valid=false` 时
//     `text` / `facts` / `summary` / `content_hash` 一律清空——设计卡要求被取代版本
//     回旧正文，HTTP 面要求失效引用不吐内容，两面各守各自事实源。
//  3. 可选字段用 `omitempty` 区分"没这个值"与"值是空"：`error_kind`（仅 failed 非空）、
//     `latest_status`（尚无版本时为空串，而空串不在六值枚举内）、`summary`（仅
//     `include_summary=true` 时返回）都**不得**以空值出站冒充合法枚举值。
//
// This file projects the ten knowledge endpoints; the two non-negotiable rules are that
// SearchHit never carries content_hash and that an invalid source view serves no text,
// facts, summary or hash. Optional fields are omitted rather than emitting a value that
// falls outside a frozen enum.

import (
	"github.com/ymedword/basalt/internal/knowledge"
)

// CollectionSummaryView 是 `CollectionSummary`（契约必填：collection_id / name /
// project / created_at / deleted_at；`document_count` 可缺省）。
// CollectionSummaryView mirrors the contract's CollectionSummary.
type CollectionSummaryView struct {
	CollectionID        string   `json:"collection_id"`
	Name                string   `json:"name"`
	Project             string   `json:"project"`
	Description         string   `json:"description"`
	DescriptionRevision int64    `json:"description_revision"`
	DeclaredAgents      []string `json:"declared_agents"`
	DocumentCount       *int     `json:"document_count,omitempty"`
	CreatedAt           string   `json:"created_at"`
	DeletedAt           string   `json:"deleted_at"`
}

// collectionSummaryViews 投影集合列表；空输入输出空切片（`[]`）。
// collectionSummaryViews renders the list, mapping an empty input to [].
func collectionSummaryViews(items []KnowledgeCollection) []CollectionSummaryView {
	out := make([]CollectionSummaryView, 0, len(items))
	for _, item := range items {
		out = append(out, collectionSummaryView(item))
	}
	return out
}

// collectionSummaryView 投影一行：`DocumentCount` 为负表示适配器没统计，省略字段
// ——0 是"确实没有未删除文档"，与"没统计"不可混同。
// collectionSummaryView omits a negative DocumentCount, which means "not counted".
func collectionSummaryView(item KnowledgeCollection) CollectionSummaryView {
	view := CollectionSummaryView{
		CollectionID:        item.CollectionID,
		Name:                item.Name,
		Project:             item.Project,
		Description:         item.Description,
		DescriptionRevision: item.DescriptionRevision,
		DeclaredAgents:      append([]string{}, item.DeclaredAgents...),
		CreatedAt:           formatTime(item.CreatedAt),
		DeletedAt:           formatTime(item.DeletedAt),
	}
	if item.DocumentCount >= 0 {
		count := item.DocumentCount
		view.DocumentCount = &count
	}
	return view
}

// DocumentSummaryView 是 `DocumentSummary`（必填：document_id / collection_id /
// source_path / content_hash / created_at / updated_at）。
// DocumentSummaryView mirrors the contract's DocumentSummary.
type DocumentSummaryView struct {
	DocumentID      string `json:"document_id"`
	CollectionID    string `json:"collection_id"`
	SourcePath      string `json:"source_path"`
	ContentHash     string `json:"content_hash"`
	LatestVersionID string `json:"latest_version_id"`
	LatestStatus    string `json:"latest_status,omitempty"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	DeletedAt       string `json:"deleted_at"`
}

// documentSummaryViews 投影文档列表；空输入输出空切片（`[]`）。
// documentSummaryViews renders the list, mapping an empty input to [].
func documentSummaryViews(items []KnowledgeDocument) []DocumentSummaryView {
	out := make([]DocumentSummaryView, 0, len(items))
	for _, item := range items {
		out = append(out, DocumentSummaryView{
			DocumentID:      item.DocumentID,
			CollectionID:    item.CollectionID,
			SourcePath:      item.SourcePath,
			ContentHash:     item.ContentHash,
			LatestVersionID: item.LatestVersionID,
			LatestStatus:    item.LatestStatus,
			CreatedAt:       formatTime(item.CreatedAt),
			UpdatedAt:       formatTime(item.UpdatedAt),
			DeletedAt:       formatTime(item.DeletedAt),
		})
	}
	return out
}

// IngestResultView 是 `IngestResult`（必填：document_id / version_id /
// ingest_job_id / status）；`error_kind` 仅 failed 时给出。
// IngestResultView mirrors the contract's IngestResult.
type IngestResultView struct {
	DocumentID   string `json:"document_id"`
	VersionID    string `json:"version_id"`
	IngestJobID  string `json:"ingest_job_id"`
	Status       string `json:"status"`
	Deduplicated bool   `json:"deduplicated"`
	ChunkCount   int    `json:"chunk_count"`
	Generation   int    `json:"generation"`
	ErrorKind    string `json:"error_kind,omitempty"`
}

// ingestResultView 投影一次摄取结果；status 取六值闭集（领域类型保证）。
// ingestResultView projects one ingest result; the status is the domain's six-value set.
func ingestResultView(result knowledge.IngestResult) IngestResultView {
	return IngestResultView{
		DocumentID:   result.DocumentID,
		VersionID:    result.VersionID,
		IngestJobID:  result.IngestJobID,
		Status:       string(result.Status),
		Deduplicated: result.Deduplicated,
		ChunkCount:   result.ChunkCount,
		Generation:   result.Generation,
		ErrorKind:    result.ErrorKind,
	}
}

// DocumentVersionSummaryView 是 `DocumentVersionSummary`（必填：version_id /
// document_id / content_hash / status / created_at）；`activated_at` 未激活为空串。
// DocumentVersionSummaryView mirrors the contract's DocumentVersionSummary.
type DocumentVersionSummaryView struct {
	VersionID      string `json:"version_id"`
	DocumentID     string `json:"document_id"`
	Revision       int64  `json:"revision"`
	ContentHash    string `json:"content_hash"`
	Status         string `json:"status"`
	ParserVersion  string `json:"parser_version,omitempty"`
	ChunkerVersion string `json:"chunker_version,omitempty"`
	Generation     int    `json:"generation"`
	ErrorKind      string `json:"error_kind,omitempty"`
	CreatedAt      string `json:"created_at"`
	ActivatedAt    string `json:"activated_at"`
}

// documentVersionSummaryViews 投影版本列表；空输入输出空切片（`[]`）。
// documentVersionSummaryViews renders the list, mapping an empty input to [].
func documentVersionSummaryViews(items []KnowledgeDocumentVersion) []DocumentVersionSummaryView {
	out := make([]DocumentVersionSummaryView, 0, len(items))
	for _, item := range items {
		out = append(out, DocumentVersionSummaryView{
			VersionID:      item.VersionID,
			DocumentID:     item.DocumentID,
			Revision:       item.Revision,
			ContentHash:    item.ContentHash,
			Status:         item.Status,
			ParserVersion:  item.ParserVersion,
			ChunkerVersion: item.ChunkerVersion,
			Generation:     item.Generation,
			ErrorKind:      item.ErrorKind,
			CreatedAt:      formatTime(item.CreatedAt),
			ActivatedAt:    formatTime(item.ActivatedAt),
		})
	}
	return out
}

// SearchHitView 是 `SearchHit`（必填：chunk_id / document_id / version_id /
// collection_id / rank / score / snippet）。**刻意不含 `content_hash`**：`knowledge.Hit`
// 带该字段只为装配记录，投影层不得放出去——命中不能当引用依据。
// SearchHitView deliberately drops content_hash; a hit can never act as a citation.
type SearchHitView struct {
	ChunkID      string  `json:"chunk_id"`
	DocumentID   string  `json:"document_id"`
	VersionID    string  `json:"version_id"`
	CollectionID string  `json:"collection_id"`
	SourcePath   string  `json:"source_path,omitempty"`
	HeadingPath  string  `json:"heading_path,omitempty"`
	Rank         int     `json:"rank"`
	Score        float64 `json:"score"`
	Snippet      string  `json:"snippet"`
}

// SearchResultView 是 `SearchResult`（必填：hits / denied_count / strategy）；
// `degraded_reason` 非空表示单路降级——降级必须让调用方看见，不得静默。
// SearchResultView mirrors the contract's SearchResult.
type SearchResultView struct {
	Hits           []SearchHitView `json:"hits"`
	DeniedCount    int             `json:"denied_count"`
	Strategy       string          `json:"strategy"`
	DegradedReason string          `json:"degraded_reason,omitempty"`
	Generation     int             `json:"generation"`
}

// searchResultView 投影检索结果：hits 恒为 `[]`，snippet 取片段正文。
// searchResultView renders hits as [] and copies the chunk text into snippet.
func searchResultView(result knowledge.SearchResult) SearchResultView {
	hits := make([]SearchHitView, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, SearchHitView{
			ChunkID:      hit.ChunkID,
			DocumentID:   hit.DocumentID,
			VersionID:    hit.VersionID,
			CollectionID: hit.CollectionID,
			SourcePath:   hit.SourcePath,
			HeadingPath:  hit.HeadingPath,
			Rank:         hit.Rank,
			Score:        hit.Score,
			Snippet:      hit.Text,
		})
	}
	return SearchResultView{
		Hits:           hits,
		DeniedCount:    result.DeniedCount,
		Strategy:       string(result.Strategy),
		DegradedReason: result.DegradedReason,
		Generation:     result.Generation,
	}
}

// EvidenceRefView 是 `EvidenceRef`（必填：id / version_id）；`content_hash` 可缺省。
// EvidenceRefView mirrors the contract's EvidenceRef.
type EvidenceRefView struct {
	ID          string `json:"id"`
	VersionID   string `json:"version_id"`
	ContentHash string `json:"content_hash,omitempty"`
}

// EvidenceRangeView 是 `EvidenceRange`：字符偏移，左闭右开。
// EvidenceRangeView mirrors the contract's rune-offset range.
type EvidenceRangeView struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// SourceFactView 是 `SourceFact`（必填：key / value）；value 由端口保证已脱敏。
// SourceFactView mirrors the contract's SourceFact.
type SourceFactView struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// SourceSummaryView 是 `SourceSummary`：**只**描述来源本身，不含任务进度（K-02）。
// SourceSummaryView describes the source only; task progress never enters it (K-02).
type SourceSummaryView struct {
	DocumentTitle  string `json:"document_title"`
	HeadingPath    string `json:"heading_path"`
	SectionSummary string `json:"section_summary"`
}

// ValidityView 是 `ValidityView`（必填：valid / reason）；reason 取冻结四值 +
// 空串（valid=true 时）。
// ValidityView mirrors the contract's ValidityView.
type ValidityView struct {
	Valid  bool   `json:"valid"`
	Reason string `json:"reason"`
}

// SourceViewResultView 是 `SourceViewResult`（必填：ref / validity / text /
// truncated）。`Superseded` 不在这里——契约没有该字段（R50.2）。
// SourceViewResultView mirrors the contract; Superseded has no field here (R50.2).
type SourceViewResultView struct {
	Ref         EvidenceRefView    `json:"ref"`
	Validity    ValidityView       `json:"validity"`
	VersionID   string             `json:"version_id,omitempty"`
	ContentHash string             `json:"content_hash,omitempty"`
	Text        string             `json:"text"`
	Range       *EvidenceRangeView `json:"range,omitempty"`
	Truncated   bool               `json:"truncated"`
	NextCursor  string             `json:"next_cursor,omitempty"`
	Conversion  string             `json:"conversion,omitempty"`
	ParentRefs  []EvidenceRefView  `json:"parent_refs"`
	Facts       []SourceFactView   `json:"facts"`
	Summary     *SourceSummaryView `json:"summary,omitempty"`
}

// sourceViewResultView 投影来源视图。**失效即清空**：`validity.valid=false` 时正文、
// 内容哈希、事实、摘要与窗口一律不出去（契约端点描述 + R50.2）；此时 `text` 仍是
// 空串而不是省略——它是必填字段。`Superseded` 无论真假都不投影。
// sourceViewResultView clears text, hash, facts, summary and range whenever the view
// is invalid; Superseded is never projected.
func sourceViewResultView(result knowledge.SourceViewResult) SourceViewResultView {
	// 先只放**身份与判定**：引用本身、有效性、实际读取的版本与截断标志都是元数据。
	// 内容是另一回事——下面只在有效分支里赋值，失效分支因此天然为空的正文 / 哈希 /
	// 事实 / 摘要 / 窗口 / 游标，不需要再逐个"清回去"。
	view := SourceViewResultView{
		Ref: EvidenceRefView{
			ID:          result.Ref.ID,
			VersionID:   result.Ref.VersionID,
			ContentHash: result.Ref.ContentHash,
		},
		Validity: ValidityView{
			Valid:  result.Validity.Valid,
			Reason: result.Validity.Reason,
		},
		VersionID:  result.VersionID,
		Truncated:  result.Truncated,
		ParentRefs: []EvidenceRefView{},
		Facts:      []SourceFactView{},
	}
	if !result.Validity.Valid {
		return view
	}
	view.ParentRefs = evidenceRefViews(result.ParentRefs)
	view.Facts = sourceFactViews(result.Facts)
	view.ContentHash = result.ContentHash
	view.Text = result.Text
	view.Range = &EvidenceRangeView{Start: result.Range.Start, End: result.Range.End}
	view.NextCursor = result.NextCursor
	view.Conversion = result.Conversion
	if result.Summary != (knowledge.SourceSummary{}) {
		view.Summary = &SourceSummaryView{
			DocumentTitle:  result.Summary.DocumentTitle,
			HeadingPath:    result.Summary.HeadingPath,
			SectionSummary: result.Summary.SectionSummary,
		}
	}
	return view
}

// evidenceRefViews 投影父引用数组；空输入输出 `[]`（契约：空数组表示顶层）。
// evidenceRefViews renders an empty list as []; the contract reads [] as "top level".
func evidenceRefViews(refs []knowledge.EvidenceRef) []EvidenceRefView {
	out := make([]EvidenceRefView, 0, len(refs))
	for _, ref := range refs {
		out = append(out, EvidenceRefView{
			ID:          ref.ID,
			VersionID:   ref.VersionID,
			ContentHash: ref.ContentHash,
		})
	}
	return out
}

// sourceFactViews 投影事实数组；空输入输出 `[]`。失效视图的清空值也是 `[]`，
// 两条路径共用同一形状。
// sourceFactViews renders an empty list as [], and the invalid view clears to the same.
func sourceFactViews(facts []knowledge.SourceFact) []SourceFactView {
	out := make([]SourceFactView, 0, len(facts))
	for _, fact := range facts {
		out = append(out, SourceFactView{Key: fact.Key, Value: fact.Value})
	}
	return out
}
