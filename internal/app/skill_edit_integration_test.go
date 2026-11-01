//go:build integration

package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSkillEdit_ReadPublishedSnapshotAndETag(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	unlockTreeOnCleanup(t, env.root)
	created := env.createSkill(t, "edit-example", "测试技能")
	before := env.operationCount()

	detail, err := env.admin.ReadSkillForEdit(context.Background(), "edit-example")
	require.NoError(t, err)
	require.Equal(t, created.ETag, detail.ETag)
	require.Equal(t, created.VersionID, detail.VersionID)
	require.Equal(t, "测试技能", detail.Description)
	require.Len(t, detail.Files, 1)
	require.Equal(t, "SKILL.md", detail.Files[0].Path)
	require.Contains(t, string(detail.Files[0].Content), "测试技能")
	require.Equal(t, before, env.operationCount(), "读取不能写管理审计")
}
