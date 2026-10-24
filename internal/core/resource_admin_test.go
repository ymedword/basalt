package core_test

// 本文件是公共资源写面领域类型的验收测试（颗粒度文档 §3.2.1：`internal/core/resource_admin_test.go`，
// 八例：种类字面量、动作 / 目标类型字面量、版本 / 指针校验、ETag 形状）。
// 命名族固定为 `TestResourceKind_*` / `TestResourceVersion_*` / `TestResourcePointer_*` /
// `TestManagementActions_*`（§4.4）——字面量逐字取自契约，本文件是它们的编译期与运行时钉子。
// This file pins the resource write-surface literals, version/pointer semantics and the
// strong ETag shape. Test names follow module card §4.4.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// hash64 是一个合法的 content_hash 样本：64 位小写十六进制。
// hash64 is a legal content hash sample: 64 lowercase hex characters.
const hash64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestResourceKind_LiteralsOnly：资源种类只取 schema CHECK 的三个字面量，
// 解析结果与常量逐一相等。
// TestResourceKind_LiteralsOnly pins the three frozen resource-kind literals.
func TestResourceKind_LiteralsOnly(t *testing.T) {
	t.Parallel()

	require.Equal(t, "model_connection", string(core.ResourceModelConnection))
	require.Equal(t, "mcp_server", string(core.ResourceMCPServer))
	require.Equal(t, "skill", string(core.ResourceSkill))

	for _, want := range []core.ResourceKind{
		core.ResourceModelConnection,
		core.ResourceMCPServer,
		core.ResourceSkill,
	} {
		got, err := core.ParseResourceKind(string(want))
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
}

// TestResourceKind_RejectsUnknown：大小写变体、别名、空白包裹与空串一律拒绝——
// 资源种类不做模糊匹配。
// TestResourceKind_RejectsUnknown refuses case variants, aliases, padded input and "".
func TestResourceKind_RejectsUnknown(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"",
		"MODEL_CONNECTION",
		"Model_Connection",
		" model_connection",
		"model_connection ",
		"modelconnection",
		"provider",
		"agent",
	} {
		kind, err := core.ParseResourceKind(input)
		require.Error(t, err, "输入 %q 必须被拒绝", input)
		require.Empty(t, kind)
	}
}

// TestManagementActions_LiteralSet：本 US 产生的动作字面量逐字等于契约 §9.3，
// 且集合内无重复；`agent.generate_files` 保留字面量但不写管理操作行。
// TestManagementActions_LiteralSet pins every action literal verbatim with no duplicates.
func TestManagementActions_LiteralSet(t *testing.T) {
	t.Parallel()

	entries := []struct {
		name  string
		value string
		want  string
	}{
		{name: "agent.create", value: core.ActionAgentCreate, want: "agent.create"},
		{name: "agent.files.update", value: core.ActionAgentFilesUpdate, want: "agent.files.update"},
		{name: "agent.generate_files", value: core.ActionAgentGenerateFiles, want: "agent.generate_files"},
		{name: "agent.archive", value: core.ActionAgentArchive, want: "agent.archive"},
		{name: "agent.copy", value: core.ActionAgentCopy, want: "agent.copy"},
		{name: "agent.restore", value: core.ActionAgentRestore, want: "agent.restore"},
		{name: "model_connection.create", value: core.ActionModelConnectionCreate, want: "model_connection.create"},
		{name: "model_connection.update", value: core.ActionModelConnectionUpdate, want: "model_connection.update"},
		{name: "model_connection.delete", value: core.ActionModelConnectionDelete, want: "model_connection.delete"},
		{name: "model_connection.test", value: core.ActionModelConnectionTest, want: "model_connection.test"},
		{name: "model_connection.publish", value: core.ActionModelConnectionPublish, want: "model_connection.publish"},
		{name: "mcp_server.create", value: core.ActionMCPServerCreate, want: "mcp_server.create"},
		{name: "mcp_server.update", value: core.ActionMCPServerUpdate, want: "mcp_server.update"},
		{name: "mcp_server.delete", value: core.ActionMCPServerDelete, want: "mcp_server.delete"},
		{name: "mcp_server.test", value: core.ActionMCPServerTest, want: "mcp_server.test"},
		{name: "mcp_server.publish", value: core.ActionMCPServerPublish, want: "mcp_server.publish"},
		{name: "skill.create", value: core.ActionSkillCreate, want: "skill.create"},
		{name: "skill.import", value: core.ActionSkillImport, want: "skill.import"},
		{name: "skill.update", value: core.ActionSkillUpdate, want: "skill.update"},
		{name: "skill.delete", value: core.ActionSkillDelete, want: "skill.delete"},
		{name: "skill.publish", value: core.ActionSkillPublish, want: "skill.publish"},
		{name: "notify_channel.create", value: core.ActionNotifyChannelCreate, want: "notify_channel.create"},
		{name: "notify_channel.update", value: core.ActionNotifyChannelUpdate, want: "notify_channel.update"},
		{name: "notify_channel.delete", value: core.ActionNotifyChannelDelete, want: "notify_channel.delete"},
	}

	seen := make(map[string]string, len(entries))
	for _, entry := range entries {
		require.Equal(t, entry.want, entry.value, "动作常量 %s 与契约不一致", entry.name)
		if previous, duplicate := seen[entry.value]; duplicate {
			t.Fatalf("动作字面量重复：%q 同时用于 %s 与 %s", entry.value, previous, entry.name)
		}
		seen[entry.value] = entry.name
	}
}

// TestManagementActions_TargetKinds：目标类型只取契约 §9.3 的五个字面量。
// TestManagementActions_TargetKinds pins the five target-kind literals.
func TestManagementActions_TargetKinds(t *testing.T) {
	t.Parallel()

	require.Equal(t, "agent", core.TargetKindAgent)
	require.Equal(t, "model_connection", core.TargetKindModelConnection)
	require.Equal(t, "mcp_server", core.TargetKindMCPServer)
	require.Equal(t, "skill", core.TargetKindSkill)
	require.Equal(t, "notify_channel", core.TargetKindNotifyChannel)
}

// TestResourceVersion_Fields：版本行的领域形态——版本号逐资源递增、content_hash 为
// 64 位小写十六进制、created_at 是 Unix 毫秒、definition_yaml 原样保存。
// TestResourceVersion_Fields pins the version row shape, including millisecond time.
func TestResourceVersion_Fields(t *testing.T) {
	t.Parallel()

	version := core.ResourceVersion{
		ID:             9,
		Kind:           core.ResourceMCPServer,
		ResourceID:     "search",
		VersionNo:      3,
		ContentHash:    hash64,
		DefinitionYAML: "  name: search\n  command: search-mcp\n",
		ManifestJSON:   "[]",
		CreatedAt:      1761134400000,
		OperationID:    "op-1761134400000-a1b2c3",
	}

	require.Equal(t, int64(9), version.ID)
	require.Equal(t, core.ResourceMCPServer, version.Kind)
	require.Equal(t, "search", version.ResourceID)
	require.Equal(t, 3, version.VersionNo)
	require.Len(t, version.ContentHash, 64)
	require.Equal(t, strings.ToLower(version.ContentHash), version.ContentHash)
	require.Equal(t, "  name: search\n  command: search-mcp\n", version.DefinitionYAML)
	require.Greater(t, version.CreatedAt, int64(1_000_000_000_000), "创建时间必须是毫秒而不是秒")
	require.NotEmpty(t, version.OperationID)
}

// TestResourceVersion_ETagShape：强 ETag 形如 `"sha256:<64 hex>"`，哈希按存储值原样
// 拼接——不同哈希得到不同 ETag，不重新求哈希。
// TestResourceVersion_ETagShape pins the strong ETag shape and verbatim hash use.
func TestResourceVersion_ETagShape(t *testing.T) {
	t.Parallel()

	etag := core.FormatStrongETag(hash64)

	require.Equal(t, `"sha256:`+hash64+`"`, etag)
	require.Regexp(t, `^"sha256:[0-9a-f]{64}"$`, etag)
	require.True(t, strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`))
	require.NotEqual(t, etag, core.FormatStrongETag(strings.Repeat("0", 64)))
}

// TestResourcePointer_Validation：指针的发布版本必为正；未生效 / 未删除用 nil 表达，
// 不得用 0 冒充"已删除"或"已生效"。
// TestResourcePointer_Validation pins nil-means-absent semantics on the pointer.
func TestResourcePointer_Validation(t *testing.T) {
	t.Parallel()

	pointer := core.ResourcePointer{
		Kind:               core.ResourceModelConnection,
		ResourceID:         "openai-main",
		PublishedVersionID: 7,
		OperationID:        "op-1761134400000-a1b2c3",
	}

	require.Positive(t, pointer.PublishedVersionID)
	require.Nil(t, pointer.AppliedVersionID, "未生效必须是 nil 而不是 0")
	require.Nil(t, pointer.DeletedAt, "未删除必须是 nil 而不是 0")

	zero := core.ResourcePointer{}
	require.Zero(t, zero.PublishedVersionID, "零值表示从未发布")
}

// TestResourcePointer_DeletedAndAppliedSemantics：删除标记一旦落下即视为已删除；
// 已发布与已生效版本可以不同（MCP 待重启），两者相等才表示无待重启。
// TestResourcePointer_DeletedAndAppliedSemantics pins deletion flags and the
// published-vs-applied difference used by pending-restart.
func TestResourcePointer_DeletedAndAppliedSemantics(t *testing.T) {
	t.Parallel()

	deletedAt := int64(1761220800000)
	appliedVersion := int64(5)
	pointer := core.ResourcePointer{
		Kind:               core.ResourceMCPServer,
		ResourceID:         "search",
		PublishedVersionID: 7,
		AppliedVersionID:   &appliedVersion,
		DeletedAt:          &deletedAt,
	}

	require.NotNil(t, pointer.DeletedAt)
	require.Equal(t, int64(1761220800000), *pointer.DeletedAt)
	require.NotEqual(t, pointer.PublishedVersionID, *pointer.AppliedVersionID, "7 != 5 表示待重启")
	require.False(t, pointer.PublishedVersionID == *pointer.AppliedVersionID)

	same := int64(7)
	pointer.AppliedVersionID = &same
	require.Equal(t, pointer.PublishedVersionID, *pointer.AppliedVersionID, "相等表示无待重启")
}
