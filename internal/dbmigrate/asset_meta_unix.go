//go:build !windows

package dbmigrate

// Unix 口径（C-50）：mode 含 setuid / setgid / sticky 三位，属主记 `uid:gid`，硬链接按
// `Nlink` 判定（`Nlink > 1` 即拒绝——硬链接会让"一个路径一条记录"的清单失真）。
// Unix semantics: mode with setuid/setgid/sticky, owner as uid:gid, hard links by Nlink.

import (
	"fmt"
	"io/fs"
	"syscall"
)

// platformMeta 返回 (mode, owner, nlink)。
func platformMeta(info fs.FileInfo) (uint32, string, uint64) {
	mode := uint32(info.Mode().Perm())
	if info.Mode()&fs.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if info.Mode()&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if info.Mode()&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return mode, "", 0
	}
	return mode, fmt.Sprintf("%d:%d", stat.Uid, stat.Gid), uint64(stat.Nlink)
}
