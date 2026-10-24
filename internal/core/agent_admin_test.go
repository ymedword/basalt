package core_test

// 本文件是 Agent 写面领域类型的验收测试（颗粒度文档 §3.2.1：`internal/core/agent_admin_test.go`，
// 五例：名称规则、归档记录字段）。命名族固定为 `TestAgentSpec_*`（§4.4）——纯 DTO 的
// 价值在字段名、单位与可空性，所以这里用字面量把契约面钉住：字段改名、时间单位从
// 毫秒退回秒、或把可空恢复时间改成零值，都会让本文件的编译或断言失败。
// This file pins the Agent write-surface DTOs: field names, units and nullability.
// Test names follow module card §4.4 (TestAgentSpec_*).

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestAgentSpec_RequiredFields：创建的规格只承载名称 / 描述 / 模型三个字段；
// 名称必填、描述可空、模型为空时是"可见占位符"而不是默认值。
// TestAgentSpec_RequiredFields pins the create spec fields: name is required, while an
// empty model means the visible placeholder rather than a default.
func TestAgentSpec_RequiredFields(t *testing.T) {
	t.Parallel()

	spec := core.AgentSpec{Name: "reporter", Description: "整理日报", Model: ""}

	require.Equal(t, "reporter", spec.Name)
	require.Equal(t, "整理日报", spec.Description)
	require.Empty(t, spec.Model)

	withModel := spec
	withModel.Model = "gpt-4o-mini"
	require.Equal(t, "gpt-4o-mini", withModel.Model)
	require.Empty(t, spec.Model, "填充模型不得回写原规格")
}

// TestAgentSpec_ZeroValueHasNoDefaults：零值不替调用方补名称、描述或模型；
// "名称必填"由 workspace 的名称规则判，core 不做隐式默认。
// TestAgentSpec_ZeroValueHasNoDefaults pins that the DTO never fills defaults: the
// required-name rule belongs to the workspace name check, not here.
func TestAgentSpec_ZeroValueHasNoDefaults(t *testing.T) {
	t.Parallel()

	var spec core.AgentSpec

	require.Empty(t, spec.Name)
	require.Empty(t, spec.Description)
	require.Empty(t, spec.Model)
}

// TestAgentSpec_ArchiveRecordFields：归档记录的六个必填字段与归档时间是 Unix 毫秒，
// 归档标识沿用 `<agent>-YYYYMMDD-HHMMSS` 形态。
// TestAgentSpec_ArchiveRecordFields pins the archive record fields and the millisecond
// timestamp unit, and keeps the archive id in the documented shape.
func TestAgentSpec_ArchiveRecordFields(t *testing.T) {
	t.Parallel()

	record := core.AgentArchiveRecord{
		ArchiveID:    "reporter-20261022-120000",
		AgentName:    "reporter",
		ArchivedAt:   1761134400000, // Unix 毫秒 UTC
		ContentHash:  "sha256:abc",
		ManifestJSON: `[{"path":"AGENT.md","size":12}]`,
		OperationID:  "op-1761134400000-a1b2c3",
	}

	require.Equal(t, "reporter-20261022-120000", record.ArchiveID)
	require.Equal(t, "reporter", record.AgentName)
	require.Greater(t, record.ArchivedAt, int64(1_000_000_000_000), "归档时间必须是毫秒而不是秒")
	require.Equal(t, "sha256:abc", record.ContentHash)
	require.NotEmpty(t, record.ManifestJSON)
	require.Equal(t, "op-1761134400000-a1b2c3", record.OperationID)
}

// TestAgentSpec_ArchiveRecordUnrestoredNil：未恢复的记录 RestoredAt 为 nil；
// 恢复后指向目标时间且不改变其他字段。
// TestAgentSpec_ArchiveRecordUnrestoredNil pins the nil-means-unrestored rule.
func TestAgentSpec_ArchiveRecordUnrestoredNil(t *testing.T) {
	t.Parallel()

	record := core.AgentArchiveRecord{ArchiveID: "reporter-20261022-120000", AgentName: "reporter"}
	require.Nil(t, record.RestoredAt)
	require.Empty(t, record.RestoredAgentName)

	restoredAt := int64(1761220800000)
	record.RestoredAt = &restoredAt
	record.RestoredAgentName = "reporter-copy"

	require.NotNil(t, record.RestoredAt)
	require.Equal(t, int64(1761220800000), *record.RestoredAt)
	require.Equal(t, "reporter-copy", record.RestoredAgentName)
}

// TestAgentSpec_FileInfoSize：文件信息是"路径 + int64 字节数"，能承载超过 2 GiB 的
// 单文件读数（int32 会在此溢出）。
// TestAgentSpec_FileInfoSize pins the int64 byte size, which must exceed 2 GiB.
func TestAgentSpec_FileInfoSize(t *testing.T) {
	t.Parallel()

	info := core.AgentFileInfo{Path: "AGENT.md", Size: 3 << 30}

	require.Equal(t, "AGENT.md", info.Path)
	require.Equal(t, int64(3<<30), info.Size)
	require.Greater(t, info.Size, int64(2<<30))
}
