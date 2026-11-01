package knowledge

// 本文件覆盖 T040 的来源视图（US-16 §2.10.4 / §3.1.3 / C-26）。前三条用例逐字落地
// 设计 §4 R9（`newKnowledgeHarness` 在本仓不存在，按 T038 先例用同语义的单元级假端口
// 承载，断言逐条保真；真库 ACL / 版本行未验，记入 run-state 未验证清单）；其后是
// 本轮补强：R9 只断言"旧正文在、新正文不在"，一个"永远返回 not_found"的实现同样能
// 全绿，因此补正向对照（active 版本精确读取）、分页完整性、摘要字段白名单（K-02）
// 与不存在版本的契约口径（200 + 原因码，不是 404）。
//
// This file covers the source view. The first three cases land verbatim from design
// R9, carried by unit-level fake ports because the design's harness type does not
// exist in this repo; the extra cases pin the positive path, pagination integrity,
// the summary field whitelist (K-02) and the not-found contract shape.

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeSourceReader 是按 (document_id, version_id) 精确读取的假端口：只认登记过的
// 键，没登记就回 `ErrNotFound`——**不**做"降级到最新版本"这种猜测。
// fakeSourceReader loads exactly the named key; it never falls back to the latest
// version and reports a missing row as ErrNotFound.
type fakeSourceReader struct {
	records      map[string]SourceVersionRecord
	err          error
	calls        int
	lastDocument string
	lastVersion  string
}

// sourceRecordKey 是假端口的键：文档 + 版本，两者缺一都读不到。
// sourceRecordKey names one exact version row.
func sourceRecordKey(documentID, versionID string) string {
	return documentID + "\x00" + versionID
}

// LoadVersion 精确读取一个版本行；未登记按 `ErrNotFound` 处理。
// LoadVersion reads exactly one row and reports a missing one as ErrNotFound.
func (f *fakeSourceReader) LoadVersion(_ context.Context, documentID, versionID string) (SourceVersionRecord, error) {
	f.calls++
	f.lastDocument, f.lastVersion = documentID, versionID
	if f.err != nil {
		return SourceVersionRecord{}, f.err
	}
	record, ok := f.records[sourceRecordKey(documentID, versionID)]
	if !ok {
		return SourceVersionRecord{}, ErrNotFound
	}
	return record, nil
}

// sourceViewHarness 组装来源视图与其假端口（ACL 与版本行都在内存里）。
// sourceViewHarness wires the viewer against in-memory ACL and version rows.
type sourceViewHarness struct {
	t      *testing.T
	authz  *fakeAuthz
	reader *fakeSourceReader
	viewer *SourceViewer
	logs   *bytes.Buffer
}

// newSourceViewHarness 建一套干净的夹具：默认集合 ACL 为空（全部项目可见）。
// newSourceViewHarness builds a clean fixture; an empty ACL means every project sees it.
func newSourceViewHarness(t *testing.T) *sourceViewHarness {
	t.Helper()
	logger, buf := captureLogger(t)
	h := &sourceViewHarness{
		t:      t,
		authz:  &fakeAuthz{},
		reader: &fakeSourceReader{records: map[string]SourceVersionRecord{}},
		logs:   buf,
	}
	h.viewer = NewSourceViewer(SourceViewerDeps{Reader: h.reader, Authz: h.authz, Logger: logger})
	return h
}

// seedActive 登记一条 active 版本并返回它的引用（引用里的哈希与实际内容一致）。
// seedActive registers one active version and returns its reference.
func (h *sourceViewHarness) seedActive(collectionID, documentID, versionID, text string) EvidenceRef {
	h.t.Helper()
	h.authz.register(collectionID, "")
	h.reader.records[sourceRecordKey(documentID, versionID)] = SourceVersionRecord{
		DocumentID:    documentID,
		VersionID:     versionID,
		CollectionID:  collectionID,
		SourcePath:    "ops/" + documentID,
		DocumentTitle: "运维手册",
		ContentHash:   sha256Hex(text),
		Status:        VersionActive,
		Text:          text,
		Facts:         []SourceFact{{Key: "owner", Value: "平台组"}},
		Summary: SourceSummary{
			DocumentTitle:  "运维手册",
			HeadingPath:    "恢复 > 回滚",
			SectionSummary: "本节描述回滚步骤。",
		},
	}
	return EvidenceRef{
		Kind:        "knowledge_document",
		ID:          documentID,
		VersionID:   versionID,
		ContentHash: sha256Hex(text),
	}
}

// ingestAndActivate 模拟一次新摄取：同文档的旧 active 版本先被置为 superseded，
// 再生效新版本——"旧正文仍在、但已不是当前版本"。
// ingestAndActivate supersedes the document's current active version before the new
// one goes live, mirroring the ingest path.
func (h *sourceViewHarness) ingestAndActivate(collectionID, documentID, versionID, text string) EvidenceRef {
	h.t.Helper()
	for key, record := range h.reader.records {
		if record.DocumentID == documentID && record.Status == VersionActive {
			record.Status = VersionSuperseded
			h.reader.records[key] = record
		}
	}
	return h.seedActive(collectionID, documentID, versionID, text)
}

// setCollectionACL 改写集合的当前 ACL（模拟"冻结之后撤权"）。
// setCollectionACL rewrites the collection ACL, modelling a revocation after the freeze.
func (h *sourceViewHarness) setCollectionACL(collectionID, acl string) {
	for i := range h.authz.collections {
		if h.authz.collections[i].id == collectionID {
			h.authz.collections[i].acl = acl
		}
	}
}

// view 以给定身份发起一次来源视图。
// view issues one source view as the given principal.
func (h *sourceViewHarness) view(p core.Principal, req SourceViewRequest) (SourceViewResult, error) {
	h.t.Helper()
	return h.viewer.SourceView(context.Background(), p, req)
}

// R9 —— 原文回查：旧引用必须返回当时版本；撤权后精确读取不得再出正文；身份显式必填。
// 逆改点：SourceView 按 document_id 取 latest_version → 第一个用例返回新正文，红；
// 精确读取跳过当前 ACL（只看历史命中时的权限）→ 第二个用例仍返回正文，红；
// SourceView 不收 Principal（内部补 local-admin）→ 第三个用例零值读到正文、两调用方结果相同，红。
func TestSourceView_SupersededVersionNotMasqueraded(t *testing.T) {
	h := newSourceViewHarness(t)
	ref := h.ingestAndActivate("runbooks", "runbook.md", "v1", "回滚步骤：先停写入，再执行 db switch --dry-run")
	h.ingestAndActivate("runbooks", "runbook.md", "v2", "回滚步骤：改为直接切换只读副本")

	got, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref, IncludeFacts: true})
	require.NoError(t, err)
	require.Equal(t, ref.VersionID, got.VersionID)
	require.Contains(t, got.Text, "db switch --dry-run", "旧引用必须返回当时版本原文")
	require.NotContains(t, got.Text, "只读副本", "不得用新版本正文冒充旧引用")
	require.True(t, got.Superseded, "版本已被取代必须显式标注")
	require.False(t, got.Validity.Valid, "被取代的版本不是当前版本")
	require.Equal(t, ReasonVersionChanged, got.Validity.Reason)
	require.Equal(t, ref.ContentHash, got.ContentHash, "返回内容必须仍是引用点名的那个版本")
	require.Equal(t, "v1", h.reader.lastVersion, "读取的版本必须是引用里的版本，不是最新版本")
}

func TestSourceView_DeniedAfterFreezeReturnsNoContent(t *testing.T) {
	h := newSourceViewHarness(t)
	const canary = "CANARY-REVOKED-91c4"
	h.authz.register("secret", "team-b")
	ref := h.seedActive("secret", "plan.md", "v1", canary+" 的原文")
	h.setCollectionACL("secret", "team-c") // 调用方 team-b 现在无权

	got, err := h.view(principalFor("team-b"), SourceViewRequest{Ref: ref})
	require.ErrorIs(t, err, ErrForbidden)
	require.Empty(t, got.Text, "拒绝时不得返回任何正文")
	require.NotContains(t, err.Error(), canary, "拒绝信息不得泄露内容")
	// C-08：被拒集合的标识与来源路径是"回读才发现的信息"，拒绝路径的日志里不得出现。
	// C-08: identifiers discovered only after the read must not reach the rejection log.
	require.NotContains(t, h.logs.String(), canary, "日志不得泄露内容")
	require.NotContains(t, h.logs.String(), "secret", "被拒集合的标识不得进日志")
	require.NotContains(t, h.logs.String(), "ops/plan.md", "被拒来源路径不得进日志")
}

// TestSourceView_DeletedDocumentServesStatusOnly 判**软删**在来源视图这条组装路径上的
// 口径（判定表本身由 `validity_test.go` 判，本用例判"读进来之后怎么出"）：引用仍可被
// 精确点名（软删是状态，不是读取错误，**不**得冒充 not_found），但正文、内容哈希、
// 结构化事实一个都不回，`Validity` 必须是 `deleted` 且带删除时刻。
// 逆改点：删掉 `judgeValidity` 的 `DocumentDeleted` 分支 → 删除后照回正文，
// "正文必须为空"与原因码断言双双红；把 `DeletedAt` 从判定里去掉 → 时刻断言红；
// 让它返回 `ErrNotFound` → `require.NoError` 红（软删不得伪装成不存在）。
func TestSourceView_DeletedDocumentServesStatusOnly(t *testing.T) {
	h := newSourceViewHarness(t)
	const text = "删除前的正文：回滚步骤。"
	ref := h.seedActive("ops", "retired.md", "v1", text)
	deletedAt := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	record := h.reader.records[sourceRecordKey("retired.md", "v1")]
	record.DocumentDeleted = true
	record.DeletedAt = deletedAt
	h.reader.records[sourceRecordKey("retired.md", "v1")] = record

	got, err := h.view(principalFor("team-a"),
		SourceViewRequest{Ref: ref, IncludeFacts: true, IncludeSummary: true})
	require.NoError(t, err, "软删是状态，不是读取错误")
	require.False(t, got.Validity.Valid, "删掉的版本不是当前有效版本")
	require.Equal(t, ReasonDeleted, got.Validity.Reason)
	require.Equal(t, deletedAt, got.Validity.DeletedAt, "删除时刻必须原样带出")
	require.Empty(t, got.Validity.ContentHash, "不回正文时哈希必须清空")
	require.Empty(t, got.Text, "已删除的版本不得返回任何正文")
	require.Equal(t, "", got.ContentHash)
	require.Empty(t, got.Facts, "删除后同样不得返回结构化事实")
	require.Equal(t, SourceSummary{}, got.Summary, "删除后同样不得返回摘要")
}

// 身份必须显式：零值 Principal 不读正文；两个调用方读同一 EvidenceRef 结果不同。
func TestSourceView_RequiresPrincipalAndScopesByCaller(t *testing.T) {
	h := newSourceViewHarness(t)
	h.authz.register("public", "team-a")
	h.authz.register("secret", "team-b")
	pub := h.seedActive("public", "a.md", "v1", "公开内容")
	sec := h.seedActive("secret", "b.md", "v1", "机密内容")

	got, err := h.view(core.Principal{}, SourceViewRequest{Ref: pub})
	require.ErrorIs(t, err, core.ErrUnauthorized, "零值身份不得读取任何来源")
	require.Empty(t, got.Text)
	require.Zero(t, h.reader.calls, "零值身份不得触发任何正文读取")

	before := h.reader.calls
	weak := core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller_a",
		Project:  "team-b",
		Scopes:   []string{core.ScopeSessionsRead},
		Actor:    core.ActorCaller,
	}
	_, err = h.view(weak, SourceViewRequest{Ref: sec})
	require.ErrorIs(t, err, ErrForbidden, "没有 knowledge:read 的身份不得读取来源")
	require.Equal(t, before, h.reader.calls, "缺少读 scope 时同样不得读取正文")

	a := principalFor("team-a")
	b := principalFor("team-b")
	_, errA := h.view(a, SourceViewRequest{Ref: sec})
	_, errB := h.view(b, SourceViewRequest{Ref: sec})
	require.ErrorIs(t, errA, ErrForbidden, "team-a 不得读 secret")
	require.NoError(t, errB, "team-b 可读 secret——同一引用按身份出不同结果")
}

// 范围越界是请求级错误（契约 400），**不得**夹到文档末尾装成合法窗口。
func TestSourceView_RangeOutOfBounds(t *testing.T) {
	h := newSourceViewHarness(t)
	ref := h.seedActive("ops", "short.md", "v1", "一二三四五")
	cases := map[string]EvidenceRange{
		"beyond_end":   {Start: 0, End: 100},
		"negative":     {Start: -1, End: 5},
		"end_before_s": {Start: 3, End: 2},
	}
	for name, window := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref, Range: &window})
			require.ErrorIs(t, err, ErrInvalidRequest)
			require.Empty(t, got.Text)
		})
	}
}

// K-02：来源摘要只描述来源自身，任务目标 / 进度 / 未决项归 C 的 TaskSummary。
// 逆改点：给 SourceSummary 加 Progress / TaskStatus 之类的字段 → 字段白名单红。
func TestSourceView_SummaryExcludesTaskProgress(t *testing.T) {
	h := newSourceViewHarness(t)
	ref := h.seedActive("ops", "runbook.md", "v1", "回滚步骤：先停写入。")

	want := SourceSummary{
		DocumentTitle:  "运维手册",
		HeadingPath:    "恢复 > 回滚",
		SectionSummary: "本节描述回滚步骤。",
	}
	got, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref, IncludeSummary: true})
	require.NoError(t, err)
	require.Equal(t, want, got.Summary)

	var fields []string
	typ := reflect.TypeOf(SourceSummary{})
	for i := 0; i < typ.NumField(); i++ {
		fields = append(fields, typ.Field(i).Name)
	}
	require.Equal(t, []string{"DocumentTitle", "HeadingPath", "SectionSummary"}, fields,
		"来源摘要只许有来源自身的三个字段，任务进度归 C（K-02）")

	plain, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref})
	require.NoError(t, err)
	require.Equal(t, SourceSummary{}, plain.Summary, "未请求摘要时不得携带")
	require.Empty(t, plain.Facts, "未请求事实时不得携带")
}

// 正向对照：active 版本的整段原文必须原样返回——只有负向用例时，一个"永远空正文"
// 的实现也能全绿。
func TestSourceView_ActiveVersionServesExactText(t *testing.T) {
	h := newSourceViewHarness(t)
	const text = "备份目录为 /var/backups，保留 7 天。"
	ref := h.seedActive("ops", "backup.md", "v1", text)

	got, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref, IncludeFacts: true})
	require.NoError(t, err)
	require.True(t, got.Validity.Valid)
	require.Empty(t, got.Validity.Reason)
	require.False(t, got.Superseded)
	require.Equal(t, text, got.Text)
	require.Equal(t, EvidenceRange{Start: 0, End: len([]rune(text))}, got.Range)
	require.Equal(t, sourceConversionVerbatim, got.Conversion)
	require.False(t, got.Truncated)
	require.Empty(t, got.NextCursor)
	require.Equal(t, ref.VersionID, got.VersionID)
	require.Equal(t, ref.ContentHash, got.ContentHash)
	require.Len(t, got.Facts, 1)

	window := EvidenceRange{Start: 0, End: 4}
	part, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref, Range: &window})
	require.NoError(t, err)
	require.Equal(t, "备份目录", part.Text)
	require.Equal(t, sourceConversionWindow, part.Conversion)
	require.Len(t, part.ParentRefs, 1, "窗口读取必须给出文档级父引用")
	require.Equal(t, ref.ID, part.ParentRefs[0].ID)
}

// 预算截断必须显式：返回前缀 + Truncated + 游标；按回游标续读能拼回原文，
// 且每一页都前进（**不得**在同一游标上原地打转）。
// 逆改点：clipWindow 忽略预算直接回全文 → 第一页 Truncated 为假；游标不前进 → 循环上限红。
func TestSourceView_BudgetPaginatesWithoutSilentTruncation(t *testing.T) {
	h := newSourceViewHarness(t)
	const text = "01234567890123456789012345678901234567890123456789" // 50 个窄字符
	ref := h.seedActive("ops", "long.md", "v1", text)

	var assembled strings.Builder
	cursor := ""
	pages := 0
	for {
		pages++
		require.Less(t, pages, 10, "分页必须在有限步内走完——游标不前进会在这里红")
		got, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref, Budget: 5, Cursor: cursor})
		require.NoError(t, err)
		require.Equal(t, sourceConversionWindow, got.Conversion)
		assembled.WriteString(got.Text)
		if !got.Truncated {
			require.Empty(t, got.NextCursor)
			break
		}
		require.NotEmpty(t, got.NextCursor, "截断必须给出续读游标")
		require.NotEqual(t, cursor, got.NextCursor, "游标必须前进")
		cursor = got.NextCursor
	}
	require.Equal(t, text, assembled.String(), "按游标续读必须能无损拼回原文")
	require.Greater(t, pages, 1, "预算小于原文时必须真的分页")

	whole, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref, Budget: 10000})
	require.NoError(t, err)
	require.False(t, whole.Truncated)
	require.Equal(t, text, whole.Text)
}

// 版本不存在是契约里的正常读取结论（200 + `validity.reason=not_found`），不是错误。
// 逆改点：把 ErrNotFound 当 500 级别错误上抛 → 本用例 NoError 红。
func TestSourceView_MissingVersionReportsNotFound(t *testing.T) {
	h := newSourceViewHarness(t)
	ref := EvidenceRef{Kind: "knowledge_document", ID: "ghost.md", VersionID: "v9"}

	got, err := h.view(principalFor("team-a"), SourceViewRequest{Ref: ref})
	require.NoError(t, err)
	require.False(t, got.Validity.Valid)
	require.Equal(t, ReasonNotFound, got.Validity.Reason)
	require.Empty(t, got.Text)
	require.Equal(t, ref, got.Ref)
}
