//go:build !windows

package dbmigrate

// 本文件守颗粒度文档 §4.3 C-50 的 **Unix 口径**：mode 保留 setuid / setgid / sticky
// 三位，属主记 `uid:gid`，硬链接数按 `Nlink` 判定（清单"一个路径一条记录"，硬链接会
// 让同一份内容出现两个身份）。
// The Unix half of C-50: special mode bits, uid:gid and the hard-link count must all
// be observable — this is exactly what the Windows branch declines to claim.

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAssets_LinuxRecordsOwnerAndMode 判 `platformMeta` 在 Unix 上的三个返回值：特殊位
// 不丢、属主是 `uid:gid`、硬链接能被数出来。
//
// 逆改点：只取 `Perm()` 丢掉 setuid/setgid/sticky → 特殊位断言红；属主返回空串 →
// 属主格式断言红；nlink 恒 0 → 硬链接断言红。
// TestAssets_LinuxRecordsOwnerAndMode pins the Unix platformMeta contract.
func TestAssets_LinuxRecordsOwnerAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte("agents\n"), 0o644))
	// Go 的 FileMode 把 setuid 放在高位标志（`fs.ModeSetuid`）而不是 0o4000 数字位：
	// 直接传 0o4755 只会落成 0755，必须用标志位拼。
	require.NoError(t, os.Chmod(path, 0o755|fs.ModeSetuid))
	info, err := os.Stat(path)
	require.NoError(t, err)

	mode, owner, nlink := platformMeta(info)
	require.Equal(t, uint32(0o4755), mode, "setuid 位必须保留在 mode 里")
	require.Regexp(t, `^\d+:\d+$`, owner, "属主必须记成 uid:gid")
	require.Equal(t, uint64(1), nlink, "普通文件只有一条目录项")

	link := filepath.Join(dir, "AGENTS.link.md")
	require.NoError(t, os.Link(path, link))
	info, err = os.Stat(path)
	require.NoError(t, err)
	_, _, nlink = platformMeta(info)
	require.Equal(t, uint64(2), nlink, "硬链接数必须可判，否则清单会漏掉重复身份")
}
