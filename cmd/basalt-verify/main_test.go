package main

// 本文件覆盖 run() 的全部返回路径；main() 只做 os.Exit 转发，不承载判据。
// This file covers every return path of run(); main() only forwards to os.Exit.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// writeImageRelease 造一个带镜像行的可校验目录；digest 为空表示本地预演没推过
// 镜像仓库。
// writeImageRelease builds a verifiable directory carrying one image row; an empty
// digest models a local rehearsal that never pushed to a registry.
func writeImageRelease(t *testing.T, digest string) string {
	t.Helper()
	dir := t.TempDir()
	sbom := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.5"}` + "\n")
	provenance := []byte(`{"schema":1}` + "\n")
	for name, body := range map[string][]byte{
		"image-sbom.cdx.json": sbom, "image-provenance.json": provenance, "notes.md": []byte("notes\n"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hash := func(value []byte) string {
		t.Helper()
		sum := sha256.Sum256(value)
		return hex.EncodeToString(sum[:])
	}
	matrix := releasekit.SupportMatrix{Schema: 1, Rows: []releasekit.SupportRow{{
		GOOS: "linux", GOARCH: "amd64", Tier: releasekit.TierBinary, Kind: "image",
		Status: releasekit.StatusTested, Checks: []string{"TestUS18_Image_BuildAndRecord"},
	}}}
	matrixBody, err := releasekit.MarshalIndentJSON(matrix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "support-matrix.json"), matrixBody, 0o644); err != nil {
		t.Fatal(err)
	}
	matrixSHA, err := releasekit.FileSHA256(filepath.Join(dir, "support-matrix.json"))
	if err != nil {
		t.Fatal(err)
	}
	notesSHA, err := releasekit.FileSHA256(filepath.Join(dir, "notes.md"))
	if err != nil {
		t.Fatal(err)
	}
	baseDigest := "sha256:" + strings.Repeat("b", 64)
	record := releasekit.ReleaseRecord{
		Schema: 1, Version: "1.2.3", Tag: "v1.2.3", Mode: releasekit.ModeRehearsal,
		Evidence:            matrix.Rows,
		Limitations:         []string{"本地预演，未推镜像仓库"},
		SupportMatrixSHA256: matrixSHA,
		NotesSHA256:         notesSHA,
		Images: []releasekit.ImageRecord{{
			Name: "basalt", Version: "1.2.3", Platform: "linux/amd64", Tier: releasekit.TierBinary,
			Tag: "basalt:1.2.3-linux-amd64", LocalID: "sha256:" + strings.Repeat("a", 64),
			RepoDigest: digest, BaseRef: "alpine:3.23@" + baseDigest, BaseDigest: baseDigest,
			BinarySHA256:     strings.Repeat("c", 64),
			DockerfileSHA256: strings.Repeat("d", 64),
			EntrypointSHA256: strings.Repeat("e", 64),
			ConfigSHA256:     strings.Repeat("f", 64),
			SBOMPath:         "image-sbom.cdx.json", SBOMSHA256: hash(sbom),
			ProvenancePath: "image-provenance.json", ProvenanceSHA256: hash(provenance),
			Scan: releasekit.ScanRecord{Status: "not-run", Reason: "本地预演未安装扫描器"},
		}},
	}
	recordBody, err := releasekit.MarshalIndentJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "release-record.json"), recordBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := releasekit.WriteChecksums(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// 中文验收点（A1 / FR11）：--require-image-digest 必须让缺 repo_digest 的镜像记录
// 失败，而同一个目录不加该开关仍可校验——否则本地预演与对外发布就没有区分。
// Acceptance (A1 / FR11): --require-image-digest must fail a record without
// repo_digest, while the same directory still verifies without the switch — otherwise a
// local rehearsal and an external release would be indistinguishable.
func TestUS18_Verify_RequireImageDigestFlag(t *testing.T) {
	unsigned := []string{"--public-key=", "--require-signature=false"}
	noDigest := writeImageRelease(t, "")
	args := append([]string{"--release-dir", noDigest, "--require-image-digest"}, unsigned...)
	if code := run(context.Background(), args); code != 1 {
		t.Fatalf("要求 digest 但记录缺 repo_digest 应返回 1，实际 %d", code)
	}
	plain := append([]string{"--release-dir", noDigest}, unsigned...)
	if code := run(context.Background(), plain); code != 0 {
		t.Fatalf("未要求 digest 时本地预演目录应返回 0，实际 %d", code)
	}

	malformed := writeImageRelease(t, "sha256:not-a-digest")
	args = append([]string{"--release-dir", malformed, "--require-image-digest"}, unsigned...)
	if code := run(context.Background(), args); code != 1 {
		t.Fatalf("畸形 repo_digest 应返回 1，实际 %d", code)
	}

	valid := writeImageRelease(t, "sha256:"+strings.Repeat("a", 64))
	args = append([]string{"--release-dir", valid, "--require-image-digest"}, unsigned...)
	if code := run(context.Background(), args); code != 0 {
		t.Fatalf("合法 digest 应返回 0，实际 %d", code)
	}
}
