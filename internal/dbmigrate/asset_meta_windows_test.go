//go:build windows

package dbmigrate

// 本文件守颗粒度文档 §4.3 C-50 的 **Windows 口径**：`os.FileMode` 只有只读位，
// 属主 / 硬链接数 / ACL 都不采集——报告必须如实写"Windows DACL 未验证"，不得把
// 空属主与 0 链接数当成已取证。
// The Windows half of C-50: only the read-only bit is observable; owner, link count
// and ACL must stay explicitly uncollected rather than faked.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAssets_WindowsModeDoesNotClaimACL 判 `platformMeta` 在 Windows 上的三个返回值：
// mode 只反映只读位（内容权限位可观察），owner 恒空、nlink 恒 0（未采集）。
//
// 逆改点：在 Windows 上编一个属主（如 "0:0"）或把 nlink 断言成真实计数 → 对应断言红；
// 恒返回 0o644 不看只读位 → 翻转只读位那一步红。
// TestAssets_WindowsModeDoesNotClaimACL pins the Windows platformMeta contract.
func TestAssets_WindowsModeDoesNotClaimACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte("agents\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	// Windows 的 `Mode().Perm()` 由只读属性合成（可写即 0666 一类），**不**等于创建时
	// 请求的 0644；因此只判"写位可观察"，不钉具体数值。
	mode, owner, nlink := platformMeta(info)
	require.NotZero(t, mode&0o200, "可写文件的写位必须可观察")
	require.Empty(t, owner, "Windows 不采集属主：留空表示未采集，不得编造")
	require.Zero(t, nlink, "Windows 不采集链接数：0 表示未采集，不是'没有硬链接'的结论")

	// 否定面：mode 必须跟着只读位走，而不是恒定值。
	require.NoError(t, os.Chmod(path, 0o444))
	info, err = os.Stat(path)
	require.NoError(t, err)
	mode, _, _ = platformMeta(info)
	require.Zero(t, mode&0o200, "只读文件的写位必须为 0")
}
