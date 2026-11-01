package web

// 本文件钉住 US-16 知识库管理面新增的**闭集与投影**，防止三处各写一遍字符串后漂开：
//
//  1. `OperationView.target_kind` 的 openapi 枚举、run-and-errors §9.3 的字面量行、
//     以及读侧过滤器 `validOperationTargetKind` 的接受集合必须**三者相等**——少一处，
//     写入端就能写出读侧筛不出的操作行（宪法第 5 条"审计可检索"的落地判据）；
//  2. `badOperationKindMessage` 的 400 文案必须列全同一个闭集，否则调用方照着错误
//     提示改参数仍会被拒；
//  3. `DocumentVersionSummary` 的字段集与必填项：版本列表把 `revision` 当作跨版本
//     定位的依据，漏投影会让页面无法按序重放。
//
// This file pins the closed sets and projections introduced by the US-16 knowledge
// management surface so the writer, the read-side filter and the contract cannot drift.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestContractsKnowledge_TargetKindsStayInSync 判 target_kind 的三处定义相等。
// 逆改点：只往 openapi 加一个取值而不改过滤器 → 接受集合缺项，本测试红；
// 反过来只改过滤器 → openapi 与 §9.3 缺项，同样红。
func TestContractsKnowledge_TargetKindsStayInSync(t *testing.T) {
	t.Parallel()

	doc := readContractDoc(t, "openapi.yaml")
	openAPI := contractEnum(t, doc, "OperationView", "target_kind")
	docLine := contractBulletTokens(t, readContractText(t, "run-and-errors.md"), "- `target_kind` 字面量")
	require.ElementsMatch(t, openAPI, docLine,
		"run-and-errors §9.3 的 target_kind 字面量行必须与 openapi 的 OperationView.target_kind 相等")

	// 读侧接受集合用"候选值全表"探针：契约枚举全收，且一组形近错值全拒。
	// 只判"枚举里的都收了"不够——一律返回 true 的实现也能过。
	candidates := append(append([]string{}, openAPI...),
		"", "Agent", "knowledge", "knowledge_chunk", "document", "collection",
		"knowledge_upload ", " target_kind", "knowledge-document",
	)
	require.ElementsMatch(t, openAPI, acceptedBy(candidates, validOperationTargetKind),
		"validOperationTargetKind 的接受集合必须恰好等于契约枚举")

	// 400 文案要列全同一个闭集：调用方照着提示改参数必须能改对。
	for _, kind := range openAPI {
		require.Contains(t, badOperationKindMessage, kind,
			"400 文案必须列出 target_kind 取值 %q", kind)
	}
}

// TestContractsKnowledge_KnowledgeActionsAndKinds 判三个知识动作与三个目标类型
// 在契约与 core 常量之间逐字一致。写入端与读侧共用 core 常量，这里钉住常量本身
// 没有偷偷改名。
// The three knowledge actions and target kinds must match the contract literals verbatim.
func TestContractsKnowledge_KnowledgeActionsAndKinds(t *testing.T) {
	t.Parallel()

	require.Equal(t, "knowledge_document.upload", core.ActionKnowledgeUpload)
	require.Equal(t, "knowledge_document.reindex", core.ActionKnowledgeReindex)
	require.Equal(t, "knowledge_collection.description_update", core.ActionKnowledgeDescription)
	require.Equal(t, "knowledge_upload", core.TargetKindKnowledgeUpload)
	require.Equal(t, "knowledge_document", core.TargetKindKnowledgeDocument)
	require.Equal(t, "knowledge_collection", core.TargetKindKnowledgeCollection)

	errorsDoc := readContractText(t, "run-and-errors.md")
	actions := contractBulletTokens(t, errorsDoc, "- `action` 字面量")
	require.Subset(t, actions, []string{
		core.ActionKnowledgeUpload, core.ActionKnowledgeReindex, core.ActionKnowledgeDescription,
	}, "三个知识动作必须逐字出现在 §9.3 的动作字面量行")

	kinds := contractBulletTokens(t, errorsDoc, "- `target_kind` 字面量")
	require.Subset(t, kinds, []string{
		core.TargetKindKnowledgeUpload, core.TargetKindKnowledgeDocument, core.TargetKindKnowledgeCollection,
	}, "三个知识目标类型必须逐字出现在 §9.3 的目标类型字面量行")

	// 落点映射是读侧过滤的前提，也是排障时定位 target_id 语义的唯一说明。
	landing := errorsDoc[strings.Index(errorsDoc, "- 三个 `knowledge_*` 动作的落点"):]
	landing = landing[:strings.IndexByte(landing, '\n')]
	for _, want := range []string{
		"`knowledge_document.upload` 的 `target_kind = knowledge_upload`",
		"`knowledge_document.reindex` 的 `target_kind = knowledge_document`",
		"`knowledge_collection.description_update` 的 `target_kind = knowledge_collection`",
	} {
		require.Contains(t, landing, want, "§9.3 必须逐字给出动作与目标类型的落点映射")
	}
}

// TestContractsKnowledge_DocumentVersionSummaryProjects 判版本投影与契约逐字段一致，
// 且整对象过 schema。`revision` 是必填项：漏投影会让页面无法按序重放版本历史。
// 逆改点：去掉 Revision 字段 → 字段集与 schema 必填项双红。
func TestContractsKnowledge_DocumentVersionSummaryProjects(t *testing.T) {
	t.Parallel()

	doc := readContractDoc(t, "openapi.yaml")

	// 失败版本带全十一个字段：这是唯一能同时给出 parser/chunker/generation 与
	// error_kind 的状态，字段集相等与 schema 校验都在这一份上判。
	failed := DocumentVersionSummaryView{
		VersionID:      "ver-2",
		DocumentID:     "doc-1",
		Revision:       2,
		ContentHash:    "hash-2",
		Status:         "failed",
		ParserVersion:  "text-v1",
		ChunkerVersion: "chunk-v1",
		Generation:     3,
		ErrorKind:      "embed_failed",
		CreatedAt:      "2026-10-27T08:30:00Z",
		ActivatedAt:    "",
	}
	requireViewMatchesSchema(t, doc, "DocumentVersionSummary", failed)

	// 未激活的版本：activated_at 渲染空串（必填字段不得缺席），error_kind 仅失败时出现。
	inactive := DocumentVersionSummaryView{
		VersionID:   "ver-1",
		DocumentID:  "doc-1",
		Revision:    1,
		ContentHash: "hash-1",
		Status:      "superseded",
		CreatedAt:   "2026-10-27T07:30:00Z",
	}
	keys := viewKeys(t, inactive)
	require.True(t, keys["activated_at"], "activated_at 是必填字段：未激活输出空串，不得缺席")
	require.False(t, keys["error_kind"], "非失败版本不输出 error_kind")
	require.True(t, keys["revision"], "revision 是必填字段，任何状态都必须出现")
}
