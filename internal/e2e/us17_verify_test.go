//go:build e2e

package e2e

// 本文件判 US-17 的「仅下载目录验证」（FR-10）：发布目录必须自带宿主平台的
// `basalt-verify`，在**没有源码、没有 Go 工具链**的环境里仍能完成校验。
// This file covers US-17's download-only verification: the release directory ships a
// host-target verifier and it must work with no source tree and no Go toolchain.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// releaseDirEnv / publicKeyEnv 是仅下载验证的两个输入：发布目录与带外公钥。
	// releaseDirEnv / publicKeyEnv feed download-only verification.
	releaseDirEnv = "BASALT_E2E_RELEASE_DIR"
	publicKeyEnv  = "BASALT_E2E_PUBLIC_KEY"
)

// TestUS17_VerifyDownload：只读发布目录 + 带外公钥 + 最小 PATH 完成验签。
// TestUS17_VerifyDownload: verify with only the release directory, the out-of-band
// public key and a minimal PATH.
func TestUS17_VerifyDownload(t *testing.T) {
	skipCandidateOnly(t, releaseDirEnv)
	dir := os.Getenv(releaseDirEnv)
	if dir == "" {
		t.Fatalf("%s 未设置：本检查只判下载目录，不读源码", releaseDirEnv)
	}
	publicKey := os.Getenv(publicKeyEnv)
	if publicKey == "" {
		t.Fatalf("%s 未设置：必须提供带外公钥（发布目录不含信任锚）", publicKeyEnv)
	}
	if !filepath.IsAbs(dir) || !filepath.IsAbs(publicKey) {
		t.Fatalf("%s / %s 必须是绝对路径（实际 %q / %q）", releaseDirEnv, publicKeyEnv, dir, publicKey)
	}
	version := os.Getenv(artifactVersionEnv)
	if !artifactVersionPattern.MatchString(version) {
		t.Fatalf("%s 必须是 X.Y.Z，实际 %q", artifactVersionEnv, version)
	}

	name := fmt.Sprintf("basalt-verify-%s-%s-%s", version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	verifier := filepath.Join(dir, name)
	if info, err := os.Stat(verifier); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("发布目录缺少宿主平台验证器 %s：%v", name, err)
	}

	// 工作目录刻意放在发布目录之外、PATH 指向空目录：验证器只能靠自己与下载文件。
	// The working directory sits outside the release dir and PATH is empty, so the
	// verifier can rely on nothing but itself and the downloaded files.
	work := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, verifier, "--release-dir", dir, "--public-key", publicKey)
	cmd.Dir = work
	cmd.Env = minimalVerifierEnv(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("仅下载目录验证失败：%v\n%s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "signed=true") {
		t.Fatalf("验证器未确认签名：%s", text)
	}
	if !strings.Contains(text, "files=") || strings.Contains(text, "files=0 ") {
		t.Fatalf("验证器未核对任何文件：%s", text)
	}
}

// minimalVerifierEnv 清掉 Go 与源码相关的环境，只保留进程启动必需项。
// minimalVerifierEnv drops the Go toolchain variables and pins PATH to an empty
// directory, keeping only what the OS needs to start a process.
func minimalVerifierEnv(t *testing.T) []string {
	t.Helper()
	emptyPath := t.TempDir()
	env := []string{"PATH=" + emptyPath}
	for _, key := range []string{"SystemRoot", "TEMP", "TMP", "HOME", "USERPROFILE"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}
