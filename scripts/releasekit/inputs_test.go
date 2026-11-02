package releasekit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUS17_Version_RejectsDirty(t *testing.T) {
	repo := newGitRepo(t)
	if _, err := LoadInputs(repo, "1.2.3", "v1.2.3", ModeFinal); err != nil {
		t.Fatalf("干净仓库应通过：%v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadInputs(repo, "1.2.3", "v1.2.3", ModeFinal); err == nil ||
		!strings.Contains(err.Error(), "工作区不干净") {
		t.Fatalf("脏工作区必须拒绝，实际：%v", err)
	}
	if _, err := LoadInputs(repo, "v1.2.3", "v1.2.3", ModeFinal); err == nil ||
		!strings.Contains(err.Error(), "v 前缀") {
		t.Fatalf("带 v 的版本必须拒绝，实际：%v", err)
	}
	if _, err := LoadInputs(repo, "1.2.3", "v9.9.9", ModeFinal); err == nil ||
		!strings.Contains(err.Error(), "tag") {
		t.Fatalf("tag 不符必须拒绝，实际：%v", err)
	}
}

func TestUS17_Preflight_RequiresKeyForFinal(t *testing.T) {
	in := Inputs{Mode: ModeFinal}
	if err := Preflight(context.Background(), in, ReleaseOptions{}); err == nil ||
		!strings.Contains(err.Error(), "--signing-key") {
		t.Fatalf("final 缺签名密钥必须拒绝，实际：%v", err)
	}
}

func TestUS17_Preflight_RejectsToolchainMismatch(t *testing.T) {
	if err := verifyGoVersion("go1.27.2"); err == nil ||
		!strings.Contains(err.Error(), "go1.27.3") {
		t.Fatalf("非 1.27.3 工具链必须拒绝，实际：%v", err)
	}
	if err := verifyGoVersion("go1.27.3"); err != nil {
		t.Fatalf("固定工具链应通过：%v", err)
	}
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "-c", "user.email=release@example.invalid", "-c", "user.name=Release", "commit", "-q", "--allow-empty", "-m", "init")
	runGit(t, repo, "tag", "v1.2.3")
	return repo
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v：%v\n%s", args, err, out)
	}
}
