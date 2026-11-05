package workspace

// 本文件是 US-21 review ② 阻断项 F-01 的回归：工作副本交换失败且回滚也失败时，
// 磁盘状态既不是旧树也不是新树，错误必须可被 errors.Is 命中 ErrWorkCopyUncertain，
// 调用方据此把管理操作记 unknown 而不是 failed（契约「提交结果未知时禁止磁盘补偿」段）。
// 回滚失败无法跨平台确定性制造，经 writeSkillWorkCopy 的 rename 注入点复现；
// 生产入口 WriteSkillWorkCopy 传 nil，行为不变。
// This file pins the uncertain work-copy rollback of review ② finding F-01 through the
// rename seam of writeSkillWorkCopy; the exported entry keeps passing nil.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// uncertainSkillMD 造一份合法 SKILL.md；marker 区分新旧两版内容。
// uncertainSkillMD builds a valid SKILL.md whose marker distinguishes the versions.
func uncertainSkillMD(marker string) string {
	return "---\nname: demo\ndescription: 回滚失败用例\n---\n\n正文 " + marker + "\n"
}

// TestUS21_WriteSkillWorkCopy_RollbackFailureIsUncertain：正式目录 → `.backup` 成功后，
// staging → 正式目录与 `.backup` → 正式目录两个 rename 都失败。现场必须原样保留：
// 正式目录缺失、旧树在 `.backup`、新树在 `.staging`，一个都不清理、不复原。
// 判别力：把该分支 errors.Join 里的 ErrWorkCopyUncertain 去掉，ErrorIs 断言转红。
func TestUS21_WriteSkillWorkCopy_RollbackFailureIsUncertain(t *testing.T) {
	root := t.TempDir()
	first, err := ValidateSkillDraft("demo", []core.AuthorFile{
		{Path: "SKILL.md", Content: uncertainSkillMD("v1")},
	})
	require.NoError(t, err)
	require.NoError(t, WriteSkillWorkCopy(root, first))

	second, err := ValidateSkillDraft("demo", []core.AuthorFile{
		{Path: "SKILL.md", Content: uncertainSkillMD("v2")},
	})
	require.NoError(t, err)

	calls := 0
	rename := func(base *os.Root, oldname, newname string) error {
		calls++
		if calls == 1 {
			return base.Rename(oldname, newname) // 正式目录 → .backup 成功
		}
		return errors.New("注入：rename 失败")
	}

	err = writeSkillWorkCopy(root, second, rename)
	require.ErrorIs(t, err, ErrWorkCopyUncertain, "回滚失败必须可识别为不确定状态")
	require.ErrorContains(t, err, "回滚目录")
	require.Equal(t, 3, calls, "两次交换 + 一次回滚")

	// 现场原样保留：调用方记 unknown 交人工裁决，这里不替它清理或复原。
	_, statErr := os.Stat(filepath.Join(root, skillsDirName, "demo"))
	require.ErrorIs(t, statErr, fs.ErrNotExist, "正式目录此时不存在")
	require.DirExists(t, filepath.Join(root, skillsDirName, "demo"+backupSuffix), "旧树留在 .backup")
	require.DirExists(t, filepath.Join(root, skillsDirName, "demo"+stagingSuffix), "新树留在 .staging")
}
