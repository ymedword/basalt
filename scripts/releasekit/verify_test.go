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

func readRecord(path string) (ReleaseRecord, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- 测试夹具
	if err != nil {
		return ReleaseRecord{}, err
	}
	var record ReleaseRecord
	err = json.Unmarshal(body, &record)
	return record, err
}
