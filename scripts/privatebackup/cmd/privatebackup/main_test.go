package main

// 本文件覆盖 run() 的四个子命令与用法/错误分支；main() 只做 os.Exit 转发。
// This file covers the four subcommands and the usage/error branches of run();
// main() only forwards to os.Exit.

import (
	"os"
	"path/filepath"
	"testing"
)

// prepareRepo 造一个最小私有资产目录：单文件 + 子目录文件。
// prepareRepo builds a minimal private-asset tree: one file plus a nested file.
func prepareRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "CLAUDE.local.md"), []byte("private notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "career"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "career", "plan.md"), []byte("plan"), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestRun_NoArgsShowsUsage(t *testing.T) {
	if code := run(nil); code != 2 {
		t.Fatalf("无子命令应返回 2，实际 %d", code)
	}
}

func TestRun_UnknownCommand(t *testing.T) {
	if code := run([]string{"pack"}); code != 2 {
		t.Fatalf("未知子命令应返回 2，实际 %d", code)
	}
}

func TestRun_SnapshotVerifyInspectRestore(t *testing.T) {
	repo := prepareRepo(t)
	archive := filepath.Join(t.TempDir(), "private.tar.gz")
	if code := run([]string{"snapshot", "--repo", repo, "--out", archive, "CLAUDE.local.md", "career"}); code != 0 {
		t.Fatalf("snapshot 应返回 0，实际 %d", code)
	}
	if code := run([]string{"verify", "--archive", archive}); code != 0 {
		t.Fatalf("verify 应返回 0，实际 %d", code)
	}
	if code := run([]string{"inspect", "--archive", archive}); code != 0 {
		t.Fatalf("inspect 应返回 0，实际 %d", code)
	}
	dest := filepath.Join(t.TempDir(), "restored")
	if code := run([]string{"restore", "--archive", archive, "--dest", dest}); code != 0 {
		t.Fatalf("restore 应返回 0，实际 %d", code)
	}
	for _, rel := range []string{"CLAUDE.local.md", "career/plan.md"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("恢复后缺少 %s：%v", rel, err)
		}
	}
}

func TestRun_SnapshotRequiresOut(t *testing.T) {
	repo := prepareRepo(t)
	if code := run([]string{"snapshot", "--repo", repo, "CLAUDE.local.md"}); code != 1 {
		t.Fatalf("缺 --out 应返回 1，实际 %d", code)
	}
}

func TestRun_VerifyMissingArchive(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.tar.gz")
	if code := run([]string{"verify", "--archive", missing}); code != 1 {
		t.Fatalf("不存在的归档应返回 1，实际 %d", code)
	}
}

func TestRun_RejectsUnknownFlag(t *testing.T) {
	if code := run([]string{"verify", "--unknown"}); code != 2 {
		t.Fatalf("未知参数应返回 2，实际 %d", code)
	}
}
