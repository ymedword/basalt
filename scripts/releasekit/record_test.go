package releasekit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUS17_Record_SigningOrder(t *testing.T) {
	dir, publicKey, _ := signedReleaseFixture(t)
	entries, err := ParseChecksums(filepath.Join(dir, checksumsName))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name == recordSignature {
			found = true
		}
	}
	if !found {
		t.Fatal("SHA256SUMS 必须覆盖记录签名")
	}
	if err := os.WriteFile(filepath.Join(dir, "late.sig"), []byte("late"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyRelease(t.Context(), dir, VerifyPolicy{
		PublicKey: publicKey, RequireSignature: true,
	}); err == nil || !strings.Contains(err.Error(), "不在 SHA256SUMS") {
		t.Fatalf("校验和之后新增的文件必须失败，实际：%v", err)
	}
}

func TestUS17_Record_NoSecrets(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "basalt-1.2.3-linux-amd64.tar.gz")
	if err := os.WriteFile(archive, []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := "basalt-1.2.3-linux-amd64"
	for _, suffix := range []string{".sbom.cdx.json", ".provenance.json"} {
		if err := os.WriteFile(filepath.Join(dir, base+suffix), []byte(`{"schema":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkg := PackageResult{
		Name: filepath.Base(archive), Archive: archive,
		Target: Target{GOOS: "linux", GOARCH: "amd64", Tier: TierBinary},
		SHA256: "abc", BinarySHA256: "def",
	}
	in := Inputs{
		Version: "1.2.3", Tag: "v1.2.3", Mode: ModeFinal,
		Commit: strings.Repeat("a", 40),
	}
	record, err := BuildReleaseRecord(in, []PackageResult{pkg}, SupportMatrix{Schema: 1}, ReleaseManifest{}, Reproducible{})
	if err != nil {
		t.Fatalf("生成记录：%v", err)
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, dir) {
		t.Fatalf("记录不得包含绝对路径：%s", text)
	}
	for _, secret := range []string{"ghp_", "AKIA", "BEGIN PRIVATE KEY", "sk-live"} {
		if strings.Contains(text, secret) {
			t.Fatalf("记录不得包含凭据样式 %q：%s", secret, text)
		}
	}
}

func TestUS17_Assemble_RequiresFullEvidence(t *testing.T) {
	final := Inputs{Mode: ModeFinal}
	if err := RequireEvidence(final, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "宿主证据") {
		t.Fatalf("final 缺 full 证据必须失败，实际：%v", err)
	}
	rehearsal := Inputs{Mode: ModeRehearsal}
	if err := RequireEvidence(rehearsal, t.TempDir()); err != nil {
		t.Fatalf("rehearsal 不应因缺证据失败：%v", err)
	}
	matrix, err := BuildSupportMatrix(rehearsal, t.TempDir())
	if err != nil {
		t.Fatalf("rehearsal 支持矩阵：%v", err)
	}
	for _, row := range matrix.Rows {
		if row.Status != StatusCompiledOnly || row.Reason == "" {
			t.Fatalf("缺证据的 rehearsal 行必须降级为 compiled-only 且写原因：%+v", row)
		}
	}
}

// 中文验收点：rehearsal 只记录双构建结论；final 必须先比较过且相等，否则阻止发布。
// Rehearsal records the comparison; final refuses to publish without an equal one.
func TestUS17_Assemble_FinalRequiresReproducible(t *testing.T) {
	if err := checkReproducible(ModeRehearsal, Reproducible{}); err != nil {
		t.Fatalf("rehearsal 不应因缺双构建结论失败：%v", err)
	}
	if err := checkReproducible(ModeFinal, Reproducible{}); err == nil ||
		!strings.Contains(err.Error(), "双构建比较") {
		t.Fatalf("final 缺双构建比较必须失败，实际：%v", err)
	}
	if err := checkReproducible(ModeFinal, Reproducible{Checked: true, Equal: false}); err == nil ||
		!strings.Contains(err.Error(), "不一致") {
		t.Fatalf("final 双构建不一致必须失败，实际：%v", err)
	}
	if err := checkReproducible(ModeFinal, Reproducible{Checked: true, Equal: true}); err != nil {
		t.Fatalf("final 双构建相等应通过：%v", err)
	}
}

// TestUS17_Matrix_FinalRequiresFullEvidenceOnly：final 只强制两份 full 证据，
// binary 档没有运行证据时写 compiled-only；记录带 UTC 时间戳 id 与非空限制。
func TestUS17_Matrix_FinalRequiresFullEvidenceOnly(t *testing.T) {
	dir := t.TempDir()
	for _, target := range DefaultTargets() {
		if target.Tier != TierFull {
			continue
		}
		if err := WriteEvidenceFile(dir, Evidence{
			GOOS: target.GOOS, GOARCH: target.GOARCH, Tier: target.Tier,
			Status: StatusTested, Checks: []string{"artifact-unpack"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	matrix, err := BuildSupportMatrix(Inputs{Mode: ModeFinal}, dir)
	if err != nil {
		t.Fatalf("final 只缺 binary 证据时应通过：%v", err)
	}
	for _, row := range matrix.Rows {
		switch row.Tier {
		case TierFull:
			if row.Status != StatusTested {
				t.Fatalf("full 行应沿用证据状态：%+v", row)
			}
		default:
			if row.Status != StatusCompiledOnly || row.Reason == "" {
				t.Fatalf("binary 行必须写 compiled-only 与原因：%+v", row)
			}
		}
	}
	pkg := PackageResult{
		Name:   "basalt-1.2.3-linux-amd64.tar.gz",
		Target: Target{GOOS: "linux", GOARCH: "amd64", Tier: TierFull},
	}
	artifactDir := t.TempDir()
	archive := filepath.Join(artifactDir, pkg.Name)
	if err := os.WriteFile(archive, []byte("artifact"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := "basalt-1.2.3-linux-amd64"
	for _, suffix := range []string{".sbom.cdx.json", ".provenance.json"} {
		if err := os.WriteFile(filepath.Join(artifactDir, base+suffix), []byte(`{"schema":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pkg.Archive = archive
	in := Inputs{Version: "1.2.3", Tag: "v1.2.3", Mode: ModeFinal, Commit: strings.Repeat("a", 40)}
	record, err := BuildReleaseRecord(in, []PackageResult{pkg}, matrix, ReleaseManifest{}, Reproducible{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(record.RecordID, "1.2.3-final-aaaaaaaaaaaa-") ||
		len(record.RecordID) != len("1.2.3-final-aaaaaaaaaaaa-20060102T150405Z") {
		t.Fatalf("record_id 必须带 UTC 时间戳，实际 %q", record.RecordID)
	}
	if len(record.Limitations) == 0 {
		t.Fatal("final 记录也必须写未验证平台与外部项限制")
	}
}
