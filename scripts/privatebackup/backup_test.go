package privatebackup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUS17_PrivateSnapshot_RoundTrips(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"CLAUDE.local.md":         "private instructions",
		"career/plan.md":          "private plan",
		"docs/interview/cases.md": "cases",
	}
	for name, body := range files {
		writePrivateFile(t, filepath.Join(repo, filepath.FromSlash(name)), body)
	}
	archive := filepath.Join(t.TempDir(), "private.tar.gz")
	manifest, err := CreateSnapshot(repo, archive, []string{"CLAUDE.local.md", "career", "docs/interview"})
	if err != nil {
		t.Fatalf("创建快照：%v", err)
	}
	if len(manifest.Entries) != len(files) {
		t.Fatalf("快照条目 = %d，期望 %d", len(manifest.Entries), len(files))
	}
	if _, err := Verify(archive); err != nil {
		t.Fatalf("校验快照：%v", err)
	}
	for name := range files {
		if err := os.Remove(filepath.Join(repo, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	dest := t.TempDir()
	if _, err := RestoreSnapshot(archive, dest); err != nil {
		t.Fatalf("恢复快照：%v", err)
	}
	for name, want := range files {
		body, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("恢复后读取 %s：%v", name, err)
		}
		if string(body) != want {
			t.Fatalf("%s 内容不符：%q", name, body)
		}
	}
}

// TestUS17_PrivateSnapshot_IncludesExclude：快照必须自行读取 .git/info/exclude，
// 不靠调用方把同一路径再传一遍。
func TestUS17_PrivateSnapshot_IncludesExclude(t *testing.T) {
	repo := t.TempDir()
	writePrivateFile(t, filepath.Join(repo, ".git", "info", "exclude"),
		"# private assets\n/docs/interview/\nCLAUDE.local.md\n!keep.txt\n")
	writePrivateFile(t, filepath.Join(repo, "CLAUDE.local.md"), "private")
	writePrivateFile(t, filepath.Join(repo, "docs", "interview", "cases.md"), "cases")
	writePrivateFile(t, filepath.Join(repo, "README.md"), "public")
	archive := filepath.Join(t.TempDir(), "private.tar.gz")
	manifest, err := CreateSnapshot(repo, archive, nil)
	if err != nil {
		t.Fatalf("创建快照：%v", err)
	}
	got := map[string]bool{}
	for _, entry := range manifest.Entries {
		got[entry.Path] = true
	}
	if !got["CLAUDE.local.md"] || !got["docs/interview/cases.md"] {
		t.Fatalf("排除规则内的文件未被快照：%+v", manifest.Entries)
	}
	if got["README.md"] {
		t.Fatalf("快照不得包含未被排除的公开文件：%+v", manifest.Entries)
	}
}

// TestUS17_PrivateSnapshot_RejectsOutputInsideWorktree：私有快照落在工作区内会被
// 误提交，必须在写入前拒绝。
func TestUS17_PrivateSnapshot_RejectsOutputInsideWorktree(t *testing.T) {
	repo := t.TempDir()
	writePrivateFile(t, filepath.Join(repo, "CLAUDE.local.md"), "private")
	_, err := CreateSnapshot(repo, filepath.Join(repo, "private.tar.gz"), nil)
	if err == nil || !strings.Contains(err.Error(), "之外") {
		t.Fatalf("工作区内输出必须失败，实际：%v", err)
	}
}

// 中文验收点：只按 ".." 前缀判越界会把工作区里的 `..snap/` 目录放过去，快照就落回
// 仓库内；输出路径必须是 `..` 或以 `..<分隔符>` 开头才算工作区之外。
// A sibling-looking name such as `..snap/` is still inside the worktree and must be
// rejected.
func TestUS17_PrivateSnapshot_RejectsDotDotPrefixInsideWorktree(t *testing.T) {
	repo := t.TempDir()
	writePrivateFile(t, filepath.Join(repo, "CLAUDE.local.md"), "private")
	// 快照仍落在工作区内，只是目录名以 ".." 开头：旧判据只看前缀会放行。
	// The output is still inside the worktree; only its directory name starts with "..".
	outside := filepath.Join(repo, "..snap", "private.tar.gz")
	if _, err := CreateSnapshot(repo, outside, nil); err == nil || !strings.Contains(err.Error(), "之外") {
		t.Fatalf("工作区内 `..` 前缀目录（%s）必须被拒绝，实际：%v", outside, err)
	}
}

func TestUS17_PrivateSnapshot_RejectsSymlinkEscape(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "evil.tar.gz")
	empty := sha256.Sum256(nil)
	manifest := Manifest{Schema: 1, CreatedAt: time.Unix(0, 0).UTC(), Entries: []Entry{
		{Path: "link", SHA256: hex.EncodeToString(empty[:]), Size: 0, Mode: 0o644},
	}}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	writer := tar.NewWriter(gzipWriter)
	if err := writer.WriteHeader(&tar.Header{
		Name: manifestName, Mode: 0o600,
		Size: int64(len(body)), ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{
		Name: filesPrefix + "link", Typeflag: tar.TypeSymlink,
		Linkname: "../../etc/passwd", Mode: 0o777, ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(archive); err != nil {
		t.Fatalf("符号链接条目应先通过清单校验：%v", err)
	}
	if _, err := RestoreSnapshot(archive, t.TempDir()); err == nil {
		t.Fatal("恢复必须拒绝符号链接")
	}
}

func writePrivateFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
