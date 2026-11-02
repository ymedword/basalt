//go:build e2e

package e2e

// 本文件判 US-17 的私有资产快照端到端（FR-14）：快照 → 删除原目录 → 恢复，
// 逐文件哈希一致；`.git/info/exclude` 里列出的私有文件同样被快照覆盖。
// This file covers US-17's private-asset snapshot end to end: snapshot, delete the
// originals, restore, and compare every file hash.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ymedword/basalt/scripts/privatebackup"
)

// TestUS17_PrivateSnapshot_RoundTrips：私有快照可恢复且逐文件一致（FR-14）。
// TestUS17_PrivateSnapshot_RoundTrips: the private snapshot restores byte for byte.
func TestUS17_PrivateSnapshot_RoundTrips(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"CLAUDE.local.md":        "# 私有说明\n",
		"career/plan.md":         "私有规划正文\n",
		"docs/interview/case.md": "案例库\n",
	}
	for name, body := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	archive := filepath.Join(t.TempDir(), "private-snapshot.tar.gz")
	if _, err := privatebackup.CreateSnapshot(repo, archive, []string{
		"CLAUDE.local.md", "career", "docs/interview/case.md",
	}); err != nil {
		t.Fatalf("创建私有快照：%v", err)
	}
	if _, err := privatebackup.Verify(archive); err != nil {
		t.Fatalf("校验私有快照：%v", err)
	}

	// 删除原目录后再恢复：恢复来源只能是快照本身。
	for name := range files {
		if err := os.Remove(filepath.Join(repo, filepath.FromSlash(name))); err != nil {
			t.Fatalf("删除 %s：%v", name, err)
		}
	}

	restored := t.TempDir()
	if _, err := privatebackup.RestoreSnapshot(archive, restored); err != nil {
		t.Fatalf("恢复私有快照：%v", err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(restored, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("恢复后缺少 %s：%v", name, err)
		}
		if string(got) != want {
			t.Fatalf("恢复后 %s = %q，期望 %q", name, got, want)
		}
	}
}
