package web

// 本文件是 US-12 写面视图与错误 `data` 投影的判卷（颗粒度文档 §3.2.1
// views_admin_test.go，6 例，check `012-web-etag-routes`）：字段集合与契约逐字
// 一致、空集合输出 `[]`、时间字段按 RFC3339 UTC、失败 `data` 只带四个固定数组。
// These tests pin the US-12 admin view projections and the failure-data whitelist.

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// viewJSON 把视图编码后再解码成通用对象，判卷以线上形状为准（不依赖 Go 字段名）。
// viewJSON round-trips a view through JSON so assertions match the wire shape.
func viewJSON(t *testing.T, view any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, json.Unmarshal(raw, &obj))
	return obj
}

// requireKeys 断言对象的键集合恰好等于契约字段集合（排序后逐一比较）。
// requireKeys asserts the exact sorted key set of a wire object.
func requireKeys(t *testing.T, obj map[string]any, want ...string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(obj))
	slices.Sort(want)
	require.Equal(t, want, got)
}

// TestAdminViews_AgentDraftProjection：草稿视图是 agent + files[path, content] +
// questions[id, question]（R6.3），顺序原样保留；两个空集合都输出 `[]` 而不是 null。
// TestAdminViews_AgentDraftProjection pins the draft fields, order, and empty arrays.
func TestAdminViews_AgentDraftProjection(t *testing.T) {
	view := agentDraftView(AgentDraftResult{Agent: "writer", Files: []core.AuthorFile{
		{Path: "AGENT.md", Content: "name: writer\n"},
		{Path: "skills/demo/SKILL.md", Content: "# demo\n"},
	}})
	obj := viewJSON(t, view)
	requireKeys(t, obj, "agent", "files", "questions")
	require.Equal(t, "writer", obj["agent"])

	files := obj["files"].([]any)
	require.Len(t, files, 2)
	first := files[0].(map[string]any)
	requireKeys(t, first, "path", "content")
	require.Equal(t, "AGENT.md", first["path"])
	require.Equal(t, "name: writer\n", first["content"])
	require.Equal(t, "skills/demo/SKILL.md", files[1].(map[string]any)["path"])
	require.Equal(t, []any{}, obj["questions"])

	empty := viewJSON(t, agentDraftView(AgentDraftResult{Agent: "blank"}))
	require.Equal(t, []any{}, empty["files"])
	require.Equal(t, []any{}, empty["questions"])
	require.Equal(t, "blank", empty["agent"])

	asked := viewJSON(t, agentDraftView(AgentDraftResult{Agent: "blank", Questions: []core.AuthorQuestion{
		{ID: "frequency", Question: "每天几点运行？"},
	}}))
	require.Equal(t, []any{}, asked["files"])
	question := asked["questions"].([]any)[0].(map[string]any)
	requireKeys(t, question, "id", "question")
	require.Equal(t, "frequency", question["id"])
	require.Equal(t, "每天几点运行？", question["question"])
}

// TestAdminViews_ScheduleDraftProjection：定时候选视图是 agent + 四个候选字段 +
// next_runs（RFC3339 UTC 时间串）；没有触发点时输出 `[]`（R6.4）。
// TestAdminViews_ScheduleDraftProjection pins the candidate fields and RFC3339 triggers.
func TestAdminViews_ScheduleDraftProjection(t *testing.T) {
	first := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	view := scheduleDraftView(ScheduleDraftResult{
		Agent: "writer",
		Candidate: core.Schedule{
			ID:      "daily-report",
			Cron:    "0 30 8 * * *",
			Zone:    "Asia/Shanghai",
			Message: "生成每日运维日报",
		},
		NextRuns: []time.Time{first, first.Add(24 * time.Hour), first.Add(48 * time.Hour)},
	})
	obj := viewJSON(t, view)
	requireKeys(t, obj, "agent", "id", "cron", "zone", "message", "next_runs")
	require.Equal(t, "writer", obj["agent"])
	require.Equal(t, "daily-report", obj["id"])
	require.Equal(t, "0 30 8 * * *", obj["cron"])
	require.Equal(t, "Asia/Shanghai", obj["zone"])
	require.Equal(t, "生成每日运维日报", obj["message"])
	require.Equal(t, []any{
		"2026-10-25T01:00:00Z", "2026-10-26T01:00:00Z", "2026-10-27T01:00:00Z",
	}, obj["next_runs"])

	withoutRuns := viewJSON(t, scheduleDraftView(ScheduleDraftResult{Agent: "writer"}))
	require.Equal(t, []any{}, withoutRuns["next_runs"])
}

// TestAdminViews_AgentArchiveProjection：归档记录的 6 个字段；archived_at 由毫秒
// 投影为 RFC3339 UTC；未恢复行的 restored_at / restored_agent_name 为空串。
// TestAdminViews_AgentArchiveProjection pins archive fields and the un-restored shape.
func TestAdminViews_AgentArchiveProjection(t *testing.T) {
	archivedAt := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC).UnixMilli()
	rec := core.AgentArchiveRecord{
		ArchiveID:   "writer-20261016T093000Z",
		AgentName:   "writer",
		ArchivedAt:  archivedAt,
		ContentHash: "sha256:abc",
	}
	obj := viewJSON(t, agentArchiveView(rec))
	requireKeys(t, obj, "archive_id", "agent", "archived_at", "content_hash", "restored_at", "restored_agent_name")
	require.Equal(t, "writer-20261016T093000Z", obj["archive_id"])
	require.Equal(t, "writer", obj["agent"])
	require.Equal(t, "2026-10-16T09:30:00Z", obj["archived_at"])
	require.Equal(t, "sha256:abc", obj["content_hash"])
	require.Equal(t, "", obj["restored_at"])
	require.Equal(t, "", obj["restored_agent_name"])

	restoredAt := time.Date(2026, 10, 17, 1, 2, 3, 0, time.UTC).UnixMilli()
	rec.RestoredAt = &restoredAt
	rec.RestoredAgentName = "writer-copy"
	restored := viewJSON(t, agentArchiveView(rec))
	require.Equal(t, "2026-10-17T01:02:03Z", restored["restored_at"])
	require.Equal(t, "writer-copy", restored["restored_agent_name"])

	list := viewJSON(t, map[string]any{"archives": agentArchiveViews(nil)})
	require.Equal(t, []any{}, list["archives"], "empty archive list must marshal as []")
}

// TestAdminViews_ResourceVersionProjection：版本行的 11 个契约字段（含 R4 的
// `resource_uid`）；未撤回版本的 reverted_from_version_id 为 null；发布结果的
// published / applied 与撤回来源按请求派生（幂等发布不派生来源，待重启的版本未应用）。
// TestAdminViews_ResourceVersionProjection pins version fields and publish derivation.
func TestAdminViews_ResourceVersionProjection(t *testing.T) {
	createdAt := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixMilli()
	version := core.ResourceVersion{
		ID:             7,
		Kind:           core.ResourceModelConnection,
		ResourceID:     "primary",
		ResourceUID:    "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
		VersionNo:      3,
		ContentHash:    "sha256:def",
		CreatedAt:      createdAt,
		OperationID:    "op-123",
		StoragePath:    "/should/not/leak",
		DefinitionYAML: "leak",
	}
	obj := viewJSON(t, resourceVersionView(version, true, false, nil))
	requireKeys(t, obj, "version_id", "resource_kind", "resource_id", "resource_uid", "version_no",
		"content_hash", "created_at", "operation_id", "published", "applied",
		"reverted_from_version_id")
	require.EqualValues(t, 7, obj["version_id"])
	require.Equal(t, "model_connection", obj["resource_kind"])
	require.Equal(t, "primary", obj["resource_id"])
	require.Equal(t, "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", obj["resource_uid"])
	require.EqualValues(t, 3, obj["version_no"])
	require.Equal(t, "sha256:def", obj["content_hash"])
	require.Equal(t, "2026-10-18T12:00:00Z", obj["created_at"])
	require.Equal(t, "op-123", obj["operation_id"])
	require.True(t, obj["published"].(bool))
	require.False(t, obj["applied"].(bool))
	require.Nil(t, obj["reverted_from_version_id"])

	// 以旧版本创建新版本：来源版本 = 请求版本，published 恒真，待重启 → applied 假。
	reqVersion := int64(2)
	result := core.ResourcePublishResult{
		Version:        core.ResourceVersion{ID: 5, Kind: core.ResourceMCPServer, ResourceID: "docs", VersionNo: 9},
		PendingRestart: true,
	}
	publish := viewJSON(t, resourcePublishResultView(core.ResourcePublishRequest{VersionID: &reqVersion}, result))
	requireKeys(t, publish, "version", "pending_restart")
	require.True(t, publish["pending_restart"].(bool))
	published := publish["version"].(map[string]any)
	require.EqualValues(t, 2, published["reverted_from_version_id"])
	require.True(t, published["published"].(bool))
	require.False(t, published["applied"].(bool))

	// 幂等发布：请求版本就是返回版本，不派生撤回来源。
	sameVersion := int64(5)
	idempotent := viewJSON(t, resourcePublishResultView(core.ResourcePublishRequest{VersionID: &sameVersion}, core.ResourcePublishResult{
		Version: result.Version,
	}))
	require.Nil(t, idempotent["version"].(map[string]any)["reverted_from_version_id"])
	require.True(t, idempotent["version"].(map[string]any)["applied"].(bool))

	// 发布最新（不带版本号）：同样不派生来源。
	latest := viewJSON(t, resourcePublishResultView(core.ResourcePublishRequest{}, core.ResourcePublishResult{Version: result.Version}))
	require.Nil(t, latest["version"].(map[string]any)["reverted_from_version_id"])
}

// TestAdminViews_ResourceVersionDetailFields：版本详情 = 版本行字段摊平 +
// definition_yaml + manifest；清单文件为 path / sha256 / size；技能的定义正文
// 为空串、空清单输出 `{"files": []}`。
// TestAdminViews_ResourceVersionDetailFields pins the flattened detail field set.
func TestAdminViews_ResourceVersionDetailFields(t *testing.T) {
	detail := core.ResourceVersionDetail{
		Version: core.ResourceVersion{
			ID: 11, Kind: core.ResourceSkill, ResourceID: "demo",
			ResourceUID: "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
			VersionNo:   1,
			ContentHash: "sha256:ghi", OperationID: "op-9",
		},
		Manifest: []core.ResourceManifestFile{
			{Path: "SKILL.md", SHA256: "sha256:file1", Size: 42},
		},
	}
	obj := viewJSON(t, resourceVersionDetailView(detail))
	requireKeys(t, obj, "version_id", "resource_kind", "resource_id", "resource_uid", "version_no",
		"content_hash", "created_at", "operation_id", "published", "applied",
		"reverted_from_version_id", "definition_yaml", "manifest")
	require.Equal(t, "skill", obj["resource_kind"])
	require.Equal(t, "rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A", obj["resource_uid"],
		"版本必须带稳定身份——同名重建后新旧版本靠它区分（R4.5）")
	require.Equal(t, "", obj["definition_yaml"], "skills carry no definition block")

	manifest := obj["manifest"].(map[string]any)
	requireKeys(t, manifest, "files")
	files := manifest["files"].([]any)
	require.Len(t, files, 1)
	file := files[0].(map[string]any)
	requireKeys(t, file, "path", "sha256", "size")
	require.Equal(t, "SKILL.md", file["path"])
	require.Equal(t, "sha256:file1", file["sha256"])
	require.EqualValues(t, 42, file["size"])

	empty := viewJSON(t, resourceVersionDetailView(core.ResourceVersionDetail{
		Version: core.ResourceVersion{Kind: core.ResourceModelConnection},
	}))
	require.Equal(t, []any{}, empty["manifest"].(map[string]any)["files"])
}

// TestAdminViews_ErrorDataShape：失败 `data` 只投影四个契约数组、当前 ETag 与
// `pending` 之后的操作标识——未知键丢弃、类型不符丢弃、空数组合法（输出 `[]`）；
// 没有白名单字段时不带 data。
// TestAdminViews_ErrorDataShape pins the failure-data whitelist and its edge cases.
func TestAdminViews_ErrorDataShape(t *testing.T) {
	if _, ok := errorDataView(errors.New("plain")); ok {
		t.Fatal("non-admin errors must not produce failure data")
	}
	if _, ok := errorDataView(nil); ok {
		t.Fatal("nil error must not produce failure data")
	}

	err := &core.AdminError{
		Kind:    core.ErrConflict,
		Message: "resource is referenced",
		Data: map[string]any{
			"agents":          []string{"writer", "reviewer"},
			"active_runs":     []string{"run-42"},
			"problems":        []string{},
			"operation_id":    "op-123",
			"definition_yaml": "api_key: ${TEST_KEY}",  // 非白名单键必须丢弃
			"run_ids":         []any{"not", "strings"}, // 类型不符必须丢弃
		},
	}
	data, ok := errorDataView(err)
	require.True(t, ok)
	require.NotNil(t, data.Agents)
	require.Equal(t, []string{"writer", "reviewer"}, *data.Agents)
	require.NotNil(t, data.ActiveRuns)
	require.Equal(t, []string{"run-42"}, *data.ActiveRuns)
	require.NotNil(t, data.Problems, "present empty array must stay present")
	require.Empty(t, *data.Problems)
	require.Nil(t, data.RunIDs)
	require.NotNil(t, data.OperationID, "post-pending failures must stay traceable")
	require.Equal(t, "op-123", *data.OperationID)

	obj := viewJSON(t, data)
	requireKeys(t, obj, "agents", "active_runs", "problems", "operation_id")
	require.Equal(t, []any{"writer", "reviewer"}, obj["agents"])
	require.Equal(t, []any{}, obj["problems"])
	require.Equal(t, "op-123", obj["operation_id"])

	// 空串操作标识视为未携带（形状合法但没有可追查的值）。
	emptyID := &core.AdminError{Kind: core.ErrConflict, Data: map[string]any{"operation_id": ""}}
	if _, ok := errorDataView(emptyID); ok {
		t.Fatal("empty operation id must not be projected")
	}

	// 只有非白名单键 → 视为无 data，避免出站空对象或未知形状。
	unknownOnly := &core.AdminError{Kind: core.ErrConflict, Data: map[string]any{"definition_yaml": "x"}}
	if _, ok := errorDataView(unknownOnly); ok {
		t.Fatal("unknown-only data must be dropped entirely")
	}
}

// TestAdminViews_EmptyCollections：所有数组字段为空时输出 `[]`（引用 / MCP 工具 /
// 技能问题 / 技能详情文件 / 工作区目录树）；工作区文件恒不截断。
// TestAdminViews_EmptyCollections pins every array field's empty form.
func TestAdminViews_EmptyCollections(t *testing.T) {
	refs := viewJSON(t, resourceReferencesView(core.ResourceReferences{
		Kind: core.ResourceSkill, ID: "demo", PublishedVersionID: 3,
	}))
	require.Equal(t, []any{}, refs["agents"])
	require.Equal(t, []any{}, refs["active_runs"])

	mcp := viewJSON(t, mcpServerTestResultView(core.ResourceTestResult{OK: true, LatencyMS: 12}))
	require.Equal(t, []any{}, mcp["tools"])
	// US-14（D-03）追加四键：operation_id / tested_version_id / stage / failure_class。
	// 零值也是合法取值（成功时 stage=complete、failure_class 为空串），因此它们**始终**
	// 出现，不做 omitempty。
	// D-03 adds four fields; their zero values are meaningful, so they always appear.
	requireKeys(t, mcp, "ok", "latency_ms", "tools", "error_code", "error_message",
		"operation_id", "tested_version_id", "stage", "failure_class")

	model := viewJSON(t, modelConnectionTestResultView(core.ResourceTestResult{OK: true}))
	requireKeys(t, model, "ok", "latency_ms", "model", "error_code", "error_message")

	check := viewJSON(t, skillValidateResultView(core.SkillValidateResult{Valid: true}))
	require.Equal(t, []any{}, check["problems"])
	requireKeys(t, check, "valid", "problems")

	detail := viewJSON(t, nonNilSkillDetail(SkillDetailView{SkillView: SkillView{Name: "demo"}}))
	require.Equal(t, []any{}, detail["files"])
	require.Equal(t, "demo", detail["name"])

	tree := viewJSON(t, map[string]any{"entries": nonNilWorkspaceTree(nil)})
	require.Equal(t, []any{}, tree["entries"])

	file := viewJSON(t, WorkspaceFileView{Path: "a.md", Content: "# a", Size: 3})
	requireKeys(t, file, "path", "content", "size", "truncated")
	require.False(t, file["truncated"].(bool))

	entry := viewJSON(t, WorkspaceEntryView{Path: "agents/writer/AGENT.md", Type: "file", Size: 128})
	requireKeys(t, entry, "path", "type", "size")
	require.Equal(t, "file", entry["type"])
	require.EqualValues(t, 128, entry["size"])
}
