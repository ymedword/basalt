package app

// 本文件是工作区只读浏览的回归（颗粒度文档 §3.1.7 / 约束 40、注 19，check
// `012-app-notify-author`）：路径越界、软链接不跟随、256 KiB 上限、二进制拒绝、
// 残骸不可见、目录 / 文件误用与缺失路径。所有断言都直接判错误类型或条目内容，
// 不依赖面向用户的文案排序。
// Regression tests for read-only workspace browsing: escapes, symlinks that are never
// followed, the 256 KiB cap, binary rejection, invisible residue, and path kind
// mismatches. Assertions check error types and entry payloads, never message wording.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// newBrowseFixture 建真实工作区骨架（`workspace.Init` 的六目录），浏览根为其
// `agents/`；日志收进缓冲，拒绝路径不污染测试输出。
// newBrowseFixture builds a real workspace skeleton and a browse service over agents/.
func newBrowseFixture(t *testing.T) (string, *WorkspaceBrowse) {
	t.Helper()

	root := t.TempDir()
	_, err := workspace.Init(root)
	require.NoError(t, err)
	logger, _ := newCapturedLogger()
	return root, NewWorkspaceBrowse(WorkspaceBrowseDeps{Root: root, Logger: logger})
}

// writeBrowseFile 在浏览根下写一个文件（自动建父目录），路径用 `/` 书写。
// writeBrowseFile writes one file under the browse root using a slash path.
func writeBrowseFile(t *testing.T, root, rel string, content []byte) {
	t.Helper()

	full := filepath.Join(root, agentsDirName, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, content, 0o644))
}

func TestWorkspaceBrowse_TreeListsEntries(t *testing.T) {
	root, browse := newBrowseFixture(t)
	definition := []byte("name: writer\n")
	writeBrowseFile(t, root, "writer/AGENT.md", definition)
	writeBrowseFile(t, root, "writer/memory/core.md", []byte("# core\n"))

	ctx := context.Background()
	entries, err := browse.Tree(ctx, "")
	require.NoError(t, err)
	// 根只列直接条目；目录不带大小，路径相对浏览根。
	require.Equal(t, []WorkspaceEntry{{Path: "writer", Type: "dir", Size: 0}}, entries)

	children, err := browse.Tree(ctx, "writer")
	require.NoError(t, err)
	require.Equal(t, []WorkspaceEntry{
		{Path: "writer/AGENT.md", Type: "file", Size: int64(len(definition))},
		{Path: "writer/memory", Type: "dir", Size: 0},
	}, children)

	// `.` 是规范写法：与空路径指向同一个浏览根。
	dotEntries, err := browse.Tree(ctx, ".")
	require.NoError(t, err)
	require.Equal(t, entries, dotEntries)
}

func TestWorkspaceBrowse_RejectsEscape(t *testing.T) {
	root, browse := newBrowseFixture(t)
	writeBrowseFile(t, root, "writer/AGENT.md", []byte("name: writer\n"))
	// 工作区外的秘密文件：任何越界请求若被放行，内容都会出现在结果里。
	secret := filepath.Join(t.TempDir(), "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("TOP-SECRET"), 0o644))

	cases := []string{
		"..",
		"../secret.txt",
		"writer/../../secret.txt",
		"/etc/passwd",
		"/",
		"C:/Windows/win.ini",
		`C:\Windows\win.ini`,
		`\\server\share\secret.txt`,
		"..\\secret.txt",
		"writer//AGENT.md",
		"writer/",
		"writer/AGENT.md\x00.txt",
		"writer/\x1fAGENT.md",
	}
	for _, raw := range cases {
		t.Run(fmt.Sprintf("tree/%q", raw), func(t *testing.T) {
			entries, err := browse.Tree(context.Background(), raw)
			var invalid *core.InvalidInputError
			require.ErrorAs(t, err, &invalid)
			require.Equal(t, workspaceBrowseTool, invalid.Tool)
			require.Nil(t, entries)
			require.NotContains(t, invalid.Reason, "TOP-SECRET")
		})
		t.Run(fmt.Sprintf("file/%q", raw), func(t *testing.T) {
			file, err := browse.File(context.Background(), raw)
			var invalid *core.InvalidInputError
			require.ErrorAs(t, err, &invalid)
			require.Empty(t, file)
		})
	}
}

func TestWorkspaceBrowse_NoSymlinkEscape(t *testing.T) {
	root, browse := newBrowseFixture(t)
	writeBrowseFile(t, root, "writer/AGENT.md", []byte("name: writer\n"))

	// 根外的目标：文件与目录各一条链接。放行任意一条都等于读到宿主任意位置。
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("TOP-SECRET"), 0o644))
	agentDir := filepath.Join(root, agentsDirName, "writer")
	if err := os.Symlink(secret, filepath.Join(agentDir, "leak.txt")); err != nil {
		// Windows 非开发者模式 / 无 SeCreateSymbolicLinkPrivilege 时无法建链接；
		// 跳过并写明理由，CI 的 Linux job 仍会执行本用例。
		t.Skipf("跳过软链接用例：当前账号无法创建符号链接（%v）", err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(agentDir, "leakdir")); err != nil {
		t.Skipf("跳过软链接用例：当前账号无法创建目录符号链接（%v）", err)
	}

	ctx := context.Background()
	entries, err := browse.Tree(ctx, "writer")
	require.NoError(t, err)
	// 软链接只报类型，不进入目标，也不展开大小。
	require.Contains(t, entries, WorkspaceEntry{Path: "writer/leak.txt", Type: "symlink", Size: 0})
	require.Contains(t, entries, WorkspaceEntry{Path: "writer/leakdir", Type: "symlink", Size: 0})

	file, err := browse.File(ctx, "writer/leak.txt")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.Empty(t, file.Content)
	require.NotContains(t, err.Error(), "TOP-SECRET")

	// 链接目录不展开：既不能列树，也不能借它读到目标里的文件。
	leaked, err := browse.Tree(ctx, "writer/leakdir")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
	require.Nil(t, leaked)
	_, err = browse.File(ctx, "writer/leakdir/secret.txt")
	require.ErrorIs(t, err, core.ErrResourceNotFound)
}

func TestWorkspaceBrowse_SizeCap(t *testing.T) {
	root, browse := newBrowseFixture(t)
	exact := bytes.Repeat([]byte("a"), maxWorkspaceFileBytes)
	writeBrowseFile(t, root, "writer/exact.txt", exact)
	writeBrowseFile(t, root, "writer/over.txt", bytes.Repeat([]byte("b"), maxWorkspaceFileBytes+1))

	ctx := context.Background()
	file, err := browse.File(ctx, "writer/exact.txt")
	require.NoError(t, err)
	require.Equal(t, maxWorkspaceFileBytes, len(file.Content))
	require.Equal(t, int64(maxWorkspaceFileBytes), file.Size)

	over, err := browse.File(ctx, "writer/over.txt")
	var invalid *core.InvalidInputError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Reason, "256")
	// 超限不截断：不返回任何正文片段。
	require.Empty(t, over.Content)
	require.Zero(t, over.Size)
}

func TestWorkspaceBrowse_BinaryRejected(t *testing.T) {
	root, browse := newBrowseFixture(t)
	writeBrowseFile(t, root, "writer/invalid.txt", []byte{0xff, 0xfe, 0xfd})
	writeBrowseFile(t, root, "writer/nul.txt", []byte("name: writer\x00\n"))
	writeBrowseFile(t, root, "writer/chinese.md", []byte("# 记忆\n"))

	ctx := context.Background()
	for _, rel := range []string{"writer/invalid.txt", "writer/nul.txt"} {
		_, err := browse.File(ctx, rel)
		var invalid *core.InvalidInputError
		require.ErrorAs(t, err, &invalid)
		require.Contains(t, invalid.Reason, "UTF-8")
	}

	// 合法 UTF-8（含中文）按原字节返回。
	file, err := browse.File(ctx, "writer/chinese.md")
	require.NoError(t, err)
	require.Equal(t, "# 记忆\n", file.Content)
	require.Equal(t, int64(len("# 记忆\n")), file.Size)
}

func TestWorkspaceBrowse_HidesResidue(t *testing.T) {
	root, browse := newBrowseFixture(t)
	writeBrowseFile(t, root, "writer/AGENT.md", []byte("name: writer\n"))
	writeBrowseFile(t, root, "writer.staging/AGENT.md", []byte("name: writer\n"))
	writeBrowseFile(t, root, "writer.backup/AGENT.md", []byte("name: writer\n"))
	writeBrowseFile(t, root, "ghost.STAGING/AGENT.md", []byte("name: ghost\n"))

	ctx := context.Background()
	entries, err := browse.Tree(ctx, "")
	require.NoError(t, err)
	// 半成品目录不出现在列表里，正式 Agent 不受影响（后缀判定大小写不敏感）。
	require.Equal(t, []WorkspaceEntry{{Path: "writer", Type: "dir", Size: 0}}, entries)

	for _, residue := range []string{"writer.staging/AGENT.md", "writer.backup/AGENT.md", "ghost.STAGING/AGENT.md"} {
		file, err := browse.File(ctx, residue)
		require.ErrorIs(t, err, core.ErrResourceNotFound)
		require.Empty(t, file)
	}
	for _, residue := range []string{"writer.staging", "writer.backup", "ghost.STAGING"} {
		listed, err := browse.Tree(ctx, residue)
		require.ErrorIs(t, err, core.ErrResourceNotFound)
		require.Nil(t, listed)
	}
}

func TestWorkspaceBrowse_FileAndTreeMismatch(t *testing.T) {
	root, browse := newBrowseFixture(t)
	writeBrowseFile(t, root, "writer/AGENT.md", []byte("name: writer\n"))

	ctx := context.Background()
	var invalid *core.InvalidInputError
	_, err := browse.Tree(ctx, "writer/AGENT.md")
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Reason, "不是目录")

	_, err = browse.File(ctx, "writer")
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, invalid.Reason, "目录")

	// 浏览根本身也不是文件。
	_, err = browse.File(ctx, ".")
	require.ErrorAs(t, err, &invalid)
}

func TestWorkspaceBrowse_MissingPath(t *testing.T) {
	root, browse := newBrowseFixture(t)
	writeBrowseFile(t, root, "writer/AGENT.md", []byte("name: writer\n"))

	// 不存在的路径按 404 处理，不区分"没有这个 Agent"与"没有这个文件"。
	for _, call := range []func() error{
		func() error {
			_, err := browse.Tree(context.Background(), "ghost")
			return err
		},
		func() error {
			_, err := browse.Tree(context.Background(), "writer/ghost")
			return err
		},
		func() error {
			_, err := browse.File(context.Background(), "ghost/AGENT.md")
			return err
		},
		func() error {
			_, err := browse.File(context.Background(), "writer/ghost.md")
			return err
		},
	} {
		require.ErrorIs(t, call(), core.ErrResourceNotFound)
	}

	// `.` 段按规范省略：`./writer` 与 `writer` 等价，`writer/./AGENT.md` 可读。
	entries, err := browse.Tree(context.Background(), "./writer")
	require.NoError(t, err)
	require.Equal(t, []WorkspaceEntry{
		{Path: "writer/AGENT.md", Type: "file", Size: int64(len("name: writer\n"))},
	}, entries)
	_, err = browse.File(context.Background(), "writer/./AGENT.md")
	require.NoError(t, err)

	// 取消信号在入口判卷：不再发起文件系统读取。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = browse.Tree(cancelled, "")
	require.ErrorIs(t, err, context.Canceled)
}

// TestWorkspaceBrowse_ConcurrentRead：只读浏览在并发下不共享可变状态——同一目录树与
// 同一文件的多次并发读取返回一致视图（012-race 必跑）。
// TestWorkspaceBrowse_ConcurrentRead asserts concurrent reads agree; the browser is
// read-only and holds no per-call mutable state.
func TestWorkspaceBrowse_ConcurrentRead(t *testing.T) {
	root, browse := newBrowseFixture(t)
	writeBrowseFile(t, root, "writer/AGENT.md", []byte("name: writer\n"))
	writeBrowseFile(t, root, "writer/memory/core.md", []byte("# 记忆\n"))

	wantTree, err := browse.Tree(context.Background(), "writer")
	require.NoError(t, err)
	wantFile, err := browse.File(context.Background(), "writer/AGENT.md")
	require.NoError(t, err)

	const readers = 16
	start := make(chan struct{})
	treeErrs := make([]error, readers)
	fileErrs := make([]error, readers)
	trees := make([][]WorkspaceEntry, readers)
	files := make([]WorkspaceFile, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			trees[index], treeErrs[index] = browse.Tree(context.Background(), "writer")
			files[index], fileErrs[index] = browse.File(context.Background(), "writer/AGENT.md")
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < readers; i++ {
		require.NoError(t, treeErrs[i], "reader %d", i)
		require.NoError(t, fileErrs[i], "reader %d", i)
		require.Equal(t, wantTree, trees[i], "并发列目录必须返回同一视图")
		require.Equal(t, wantFile, files[i], "并发读文件必须返回同一内容")
	}
}
