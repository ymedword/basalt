package releasekit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ymedword/basalt/internal/isolation"
)

func TestUS20_ImageManifestSignedRelease(t *testing.T) {
	dir, record, matrix := imageReleaseFixture(t)
	key, pub := filepath.Join(t.TempDir(), "private.pem"), filepath.Join(t.TempDir(), "public.pem")
	if err := GenerateKeyPair(key, pub); err != nil {
		t.Fatal(err)
	}
	record.Images[0].RepoDigest = "sha256:" + hex64("7")
	record.Signing = SigningInfo{Status: "signed", Algorithm: "ed25519", SignedFiles: []string{recordSignature, sumsSignature, imageLockSignature}}
	lock, err := MergeSandboxImages(ImageLock{Schema: 2, AppBase: lockFixtureBase, Postgres: lockFixturePG}, record.Images, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, imageLockName), lock); err != nil {
		t.Fatal(err)
	}
	rewriteImageRelease(t, dir, record, matrix)
	for _, name := range []string{releaseRecordName, imageLockName} {
		if _, err := SignFile(key, dir, name); err != nil {
			t.Fatal(err)
		}
	}
	resignSums := func() {
		t.Helper()
		if err := WriteChecksums(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := SignFile(key, dir, checksumsName); err != nil {
			t.Fatal(err)
		}
	}
	resignSums()
	policy := VerifyPolicy{PublicKey: pub, RequireSignature: true}
	report, err := VerifyRelease(context.Background(), dir, policy)
	if err != nil || !report.Signed || len(report.VerifiedSignatures) != 3 {
		t.Fatalf("signed chain: %+v %v", report, err)
	}
	publicKey, err := loadPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := isolation.LoadApprovedImages(filepath.Join(dir, imageLockName), publicKey)
	if err != nil || !approved.Approved(record.Images[0].RepoDigest) {
		t.Fatalf("runtime trust: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, imageLockSignature)); err != nil {
		t.Fatal(err)
	}
	resignSums()
	if _, err := VerifyRelease(context.Background(), dir, policy); err == nil || !strings.Contains(err.Error(), imageLockSignature) {
		t.Fatalf("missing manifest signature: %v", err)
	}
	lock.Sandbox.GVisor = "basalt:other@sha256:" + hex64("8")
	if err := writeJSON(filepath.Join(dir, imageLockName), lock); err != nil {
		t.Fatal(err)
	}
	if _, err := SignFile(key, dir, imageLockName); err != nil {
		t.Fatal(err)
	}
	resignSums()
	if _, err := VerifyRelease(context.Background(), dir, policy); err == nil || !strings.Contains(err.Error(), "不一致") {
		t.Fatalf("signed mismatch accepted: %v", err)
	}
}
