package releasekit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUS17_Verify_RejectsTampered(t *testing.T) {
	dir, publicKey, _ := signedReleaseFixture(t)
	artifact := filepath.Join(dir, "basalt-1.2.3-linux-amd64.tar.gz")
	if err := os.WriteFile(artifact, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := VerifyRelease(context.Background(), dir, VerifyPolicy{
		PublicKey: publicKey, RequireSignature: true,
	})
	if err == nil || !strings.Contains(err.Error(), "哈希不符") {
		t.Fatalf("篡改归档必须失败，实际：%v", err)
	}
}

func TestUS17_Verify_RejectsMissingSignature(t *testing.T) {
	dir, publicKey, _ := signedReleaseFixture(t)
	if err := os.Remove(filepath.Join(dir, sumsSignature)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRelease(context.Background(), dir, VerifyPolicy{
		PublicKey: publicKey, RequireSignature: true,
	}); err == nil || !strings.Contains(err.Error(), sumsSignature) {
		t.Fatalf("缺校验和签名必须失败，实际：%v", err)
	}
}

func TestUS17_Verify_UnsignedRehearsal(t *testing.T) {
	dir, _, _ := signedReleaseFixture(t)
	for _, name := range []string{sumsSignature, recordSignature} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	recordPath := filepath.Join(dir, releaseRecordName)
	record, err := readRecord(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	record.Signing = SigningInfo{Status: "not_signed", Algorithm: "none"}
	if err := writeJSON(recordPath, record); err != nil {
		t.Fatal(err)
	}
	if err := WriteChecksums(dir); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyRelease(context.Background(), dir, VerifyPolicy{})
	if err != nil {
		t.Fatalf("无签名预演应允许：%v", err)
	}
	if report.Signed {
		t.Fatal("无签名预演不得标成 signed")
	}
}

func signedReleaseFixture(t *testing.T) (dir, publicKey, privateKey string) {
	t.Helper()
	dir = t.TempDir()
	privateKey = filepath.Join(t.TempDir(), "release.key")
	publicKey = filepath.Join(t.TempDir(), "release.pub")
	if err := GenerateKeyPair(privateKey, publicKey); err != nil {
		t.Fatal(err)
	}
	base := "basalt-1.2.3-linux-amd64"
	files := map[string][]byte{
		base + ".tar.gz":          []byte("artifact"),
		base + ".sbom.cdx.json":   []byte(`{"bomFormat":"CycloneDX"}`),
		base + ".provenance.json": []byte(`{"schema":1}`),
		"support-matrix.json":     []byte(`{"schema":1,"rows":[]}`),
		"notes.md":                []byte("# 1.2.3\n"),
	}
	sums := make(map[string]string, len(files))
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		sum, err := FileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		sums[name] = sum
	}
	record := ReleaseRecord{
		Schema: 1, RecordID: "1.2.3-final-0123456789ab-20260101T000000Z", Version: "1.2.3",
		Tag: "v1.2.3", Mode: ModeFinal, Commit: strings.Repeat("a", 40),
		Artifacts: []ArtifactRecord{{
			Name: base + ".tar.gz", GOOS: "linux", GOARCH: "amd64", Tier: TierFull,
			SHA256:           sums[base+".tar.gz"],
			SBOMSHA256:       sums[base+".sbom.cdx.json"],
			ProvenanceSHA256: sums[base+".provenance.json"],
		}},
		SupportMatrixSHA256: sums["support-matrix.json"],
		NotesSHA256:         sums["notes.md"],
		Limitations:         []string{"外部模型未由发布流程验证"},
		Signing: SigningInfo{
			Status: "signed", Algorithm: "ed25519",
			SignedFiles: []string{recordSignature, sumsSignature},
		},
	}
	if err := writeJSON(filepath.Join(dir, releaseRecordName), record); err != nil {
		t.Fatal(err)
	}
	if _, err := SignFile(privateKey, dir, releaseRecordName); err != nil {
		t.Fatal(err)
	}
	if err := WriteChecksums(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := SignFile(privateKey, dir, checksumsName); err != nil {
		t.Fatal(err)
	}
	return dir, publicKey, privateKey
}

// imageReleaseFixture 造一个未签名、带镜像行的发布目录：矩阵里同平台既有归档行
// 也有镜像行，用来证明两者按 (goos, goarch, kind) 分开核对。
// imageReleaseFixture builds an unsigned release directory carrying an image row:
// the matrix holds both an archive row and an image row on the same platform, which
// proves the two are matched by (goos, goarch, kind).
func imageReleaseFixture(t *testing.T) (string, ReleaseRecord, SupportMatrix) {
	t.Helper()
	dir := t.TempDir()
	sbom := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.5"}` + "\n")
	provenance := []byte(`{"schema":1}` + "\n")
	files := map[string][]byte{
		imageSBOMName: sbom, imageProvenanceName: provenance, "notes.md": []byte("# 1.2.3\n"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := validImageRecord()
	image.SBOMSHA256 = sha256Hex(sbom)
	image.ProvenanceSHA256 = sha256Hex(provenance)
	matrix := SupportMatrix{Schema: 1, Rows: []SupportRow{
		{
			GOOS: "linux", GOARCH: "amd64", Tier: TierBinary,
			Status: StatusCompiledOnly, Reason: "交叉编译目标，无宿主运行证据",
		},
		{
			GOOS: "linux", GOARCH: "amd64", Tier: TierBinary, Kind: kindImage,
			Status: StatusTested, Checks: []string{"TestUS18_Image_BuildAndRecord"},
		},
	}}
	record := ReleaseRecord{
		Schema: 1, RecordID: "1.2.3-rehearsal-0123456789ab-20260101T000000Z",
		Version: "1.2.3", Tag: "v1.2.3", Mode: ModeRehearsal,
		Evidence: matrix.Rows, Images: []ImageRecord{image},
		Limitations: []string{"rehearsal 产物不可对外发布"},
	}
	rewriteImageRelease(t, dir, record, matrix)
	return dir, record, matrix
}

// rewriteImageRelease 重写矩阵与记录并刷新校验和；记录里的矩阵哈希按新文件重算，
// 否则用例只会红在哈希上，测不到镜像核对本身。
// rewriteImageRelease rewrites the matrix and record and refreshes the checksums; the
// matrix hash is recomputed so a failure points at the image check, not at the hash.
func rewriteImageRelease(t *testing.T, dir string, record ReleaseRecord, matrix SupportMatrix) {
	t.Helper()
	if err := writeJSON(filepath.Join(dir, "support-matrix.json"), matrix); err != nil {
		t.Fatal(err)
	}
	sum, err := FileSHA256(filepath.Join(dir, "support-matrix.json"))
	if err != nil {
		t.Fatal(err)
	}
	record.SupportMatrixSHA256 = sum
	if record.NotesSHA256 == "" {
		notesSum, err := FileSHA256(filepath.Join(dir, "notes.md"))
		if err != nil {
			t.Fatal(err)
		}
		record.NotesSHA256 = notesSum
	}
	if err := writeJSON(filepath.Join(dir, releaseRecordName), record); err != nil {
		t.Fatal(err)
	}
	if err := WriteChecksums(dir); err != nil {
		t.Fatal(err)
	}
}

// 中文验收点：镜像记录进入发布目录后，VerifyRelease 必须逐项核对——缺字段、digest
// 格式错、文件不存在、矩阵行不匹配、任一方孤儿行都要失败；只按 images[] 是否为空
// 触发会让“删记录但留矩阵行”蒙混过关。
// Acceptance: once an image record is in the release directory, VerifyRelease checks
// it field by field — a missing field, a malformed digest, a missing file, a
// mismatched matrix row and an orphan on either side all fail. Triggering only on a
// non-empty images[] would let "drop the record but keep the row" through.
func TestUS18_Verify_ImagesRecord(t *testing.T) {
	ctx := context.Background()

	dir, _, _ := imageReleaseFixture(t)
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{}); err != nil {
		t.Fatalf("基准镜像发布目录必须通过：%v", err)
	}

	// 缺字段：local_id 为空。
	dir, record, matrix := imageReleaseFixture(t)
	record.Images[0].LocalID = ""
	rewriteImageRelease(t, dir, record, matrix)
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "local_id") {
		t.Fatalf("缺 local_id 必须失败，got %v", err)
	}

	// digest 格式错：--require-image-digest 下 repo_digest 必须是 sha256:<64 hex>。
	dir, record, matrix = imageReleaseFixture(t)
	record.Images[0].RepoDigest = "sha256:not-a-digest"
	rewriteImageRelease(t, dir, record, matrix)
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{RequireRepoDigest: true}); err == nil ||
		!strings.Contains(err.Error(), "repo_digest") {
		t.Fatalf("畸形 repo_digest 必须失败，got %v", err)
	}
	// repo_digest 为空时，未要求 digest 的校验不得失败（本地预演）。
	dir, record, matrix = imageReleaseFixture(t)
	rewriteImageRelease(t, dir, record, matrix)
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{}); err != nil {
		t.Fatalf("本地预演不得要求 repo_digest：%v", err)
	}
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{RequireRepoDigest: true}); err == nil ||
		!strings.Contains(err.Error(), "repo_digest") {
		t.Fatalf("要求 digest 时空 repo_digest 必须失败，got %v", err)
	}

	// 路径不存在：记录指向同目录里不存在的 SBOM。
	dir, record, matrix = imageReleaseFixture(t)
	record.Images[0].SBOMPath = "image-sbom-missing.json"
	rewriteImageRelease(t, dir, record, matrix)
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "image-sbom-missing.json") {
		t.Fatalf("SBOM 文件不存在必须点名失败，got %v", err)
	}

	// 矩阵行档位不匹配：同一平台但 tier 不同。
	dir, record, matrix = imageReleaseFixture(t)
	matrix.Rows[1].Tier = TierFull
	rewriteImageRelease(t, dir, record, matrix)
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "档位") {
		t.Fatalf("矩阵行档位不匹配必须失败，got %v", err)
	}

	// 孤儿行（矩阵侧）：删掉 images[] 但保留 kind=image 行。
	dir, record, matrix = imageReleaseFixture(t)
	record.Images = nil
	rewriteImageRelease(t, dir, record, matrix)
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "支持矩阵有") {
		t.Fatalf("矩阵孤儿镜像行必须失败，got %v", err)
	}

	// 孤儿行（记录侧）：保留 images[]，但矩阵只剩归档行。
	dir, record, matrix = imageReleaseFixture(t)
	matrix.Rows = matrix.Rows[:1]
	rewriteImageRelease(t, dir, record, matrix)
	if _, err := VerifyRelease(ctx, dir, VerifyPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "kind=image") {
		t.Fatalf("记录孤儿镜像必须失败，got %v", err)
	}
}

func readRecord(path string) (ReleaseRecord, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- 测试夹具
	if err != nil {
		return ReleaseRecord{}, err
	}
	var record ReleaseRecord
	err = json.Unmarshal(body, &record)
	return record, err
}
