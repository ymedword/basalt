//go:build windows

package dbmigrate

// Windows 口径（C-50）：`os.FileMode` 只有只读位，**不含 ACL**；属主与硬链接数不采集，
// 返回空值 / 0 让报告如实写"Windows DACL 未验证"，而不是把 0 当成"没有硬链接"的结论。
// Windows semantics: only the read-only bit is observable; ACL/owner/link count are not
// captured, and the report must say so instead of treating zero as evidence.

import "io/fs"

// platformMeta 返回 (mode, owner, nlink)；Windows 上 owner 恒为空、nlink 恒为 0（未采集）。
func platformMeta(info fs.FileInfo) (uint32, string, uint64) {
	return uint32(info.Mode().Perm()), "", 0
}
