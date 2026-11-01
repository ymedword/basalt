//go:build windows

package knowledge

// 本文件补 Windows 专有的"联接点"用例：`os.Symlink` 需要开发者模式或管理员权限，
// 但目录联接（`mklink /J`）普通用户即可创建，而它同样是重解析点——读取器必须拒绝。
// 本机实测（2026-10-30）：`mklink /J` 在普通用户下成功，因此这条拒绝路径有可执行证据，
// 不依赖符号链接权限（后者在本机被跳过，见 `reader_test.go`）。
//
// This file covers the Windows junction case: junctions are reparse points ordinary
// users can create, so the reader's refusal has executable evidence here.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRootReader_RejectsJunction 守"拒绝联接点"：目录联接指向根内也拒绝。
// 逆改点：删掉 rejectLinks 的 `ModeSymlink` 判定 → 本用例变绿（错误方向），用例红。
func TestRootReader_RejectsJunction(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "real/a.md", []byte("x"))
	// mklink 要求链接的父目录已存在（目标目录也要存在）。
	link := filepath.Join(root, "docs", "junction.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))

	cmd := exec.Command("cmd", "/c", "mklink", "/J", link, filepath.Join(root, "real"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("本机无法创建目录联接（%v）: %s", err, out)
	}

	_, err := mustRootReader(t, root, 1<<20).Load(t.Context(), "docs/junction.md")
	require.ErrorIs(t, err, ErrPathRejected)
}
