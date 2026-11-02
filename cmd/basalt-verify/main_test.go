package main

// 本文件覆盖 run() 的全部返回路径；main() 只做 os.Exit 转发，不承载判据。
// This file covers every return path of run(); main() only forwards to os.Exit.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ymedword/basalt/scripts/releasekit"
)

// writeRelease 造一个最小可校验目录：数据文件 + schema=1 的发布记录 + 校验和。
// writeRelease builds the minimal verifiable directory: one data file, a schema=1
// release record and its checksums.
func writeRelease(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "basalt.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 记录里的 support-matrix / notes 哈希必须与目录内文件一致：验证器会逐项对账。
	// The record's support-matrix and notes hashes must match the files in the
	// directory because the verifier reconciles every recorded hash.
	for name, body := range map[string]string{"support-matrix.json": "{}\n", "notes.md": "notes\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	matrixSHA, err := releasekit.FileSHA256(filepath.Join(dir, "support-matrix.json"))
	if err != nil {
		t.Fatal(err)
	}
	notesSHA, err := releasekit.FileSHA256(filepath.Join(dir, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	record := fmt.Sprintf(`{"schema":1,"support_matrix_sha256":%q,"notes_sha256":%q,"limitations":["最小校验目录，不含真实平台"]}`+"\n", matrixSHA, notesSHA)
	if err := os.WriteFile(filepath.Join(dir, "release-record.json"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := releasekit.WriteChecksums(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRun_RequiresReleaseDir(t *testing.T) {
	if code := run(context.Background(), nil); code != 2 {
		t.Fatalf("缺 --release-dir 应返回 2，实际 %d", code)
	}
}

func TestRun_RejectsUnknownFlag(t *testing.T) {
	if code := run(context.Background(), []string{"--unknown"}); code != 2 {
		t.Fatalf("未知参数应返回 2，实际 %d", code)
	}
}

func TestRun_VerifiesUnsignedRehearsal(t *testing.T) {
	dir := writeRelease(t)
	// 预演目录没有签名文件：必须显式清空公钥，否则会按“有公钥就必须有签名”被判失败。
	// An unsigned rehearsal has no .sig files, so the public key must be cleared.
	args := []string{"--release-dir", dir, "--public-key=", "--require-signature=false"}
	if code := run(context.Background(), args); code != 0 {
		t.Fatalf("无签名预演目录应返回 0，实际 %d", code)
	}
}

func TestRun_RejectsTamperedFile(t *testing.T) {
	dir := writeRelease(t)
	if err := os.WriteFile(filepath.Join(dir, "basalt.txt"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--release-dir", dir, "--public-key=", "--require-signature=false"}
	if code := run(context.Background(), args); code != 1 {
		t.Fatalf("篡改文件应返回 1，实际 %d", code)
	}
}

func TestRun_RequireSignatureRejectsUnsignedRecord(t *testing.T) {
	dir := writeRelease(t)
	if code := run(context.Background(), []string{"--release-dir", dir}); code != 1 {
		t.Fatalf("要求签名但记录未签应返回 1，实际 %d", code)
	}
}
