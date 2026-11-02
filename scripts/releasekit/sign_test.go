package releasekit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUS17_Sign_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	privateKey := filepath.Join(dir, "release.key")
	publicKey := filepath.Join(dir, "release.pub")
	if err := GenerateKeyPair(privateKey, publicKey); err != nil {
		t.Fatalf("生成测试密钥：%v", err)
	}
	target := filepath.Join(dir, "payload")
	if err := os.WriteFile(target, []byte("basalt-release"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := SignFile(privateKey, dir, "payload")
	if err != nil {
		t.Fatalf("签名：%v", err)
	}
	if info.Signature != "payload.sig" || info.Fingerprint == "" {
		t.Fatalf("签名读数不完整：%+v", info)
	}
	if err := VerifyFileSignature(publicKey, target, filepath.Join(dir, "payload.sig")); err != nil {
		t.Fatalf("验签：%v", err)
	}
}

// TestUS17_Sign_RejectsWrongKey：非签发公钥必须验签失败（模块卡 §3.2）。
// TestUS17_Sign_RejectsWrongKey: a public key that did not sign must fail verification.
func TestUS17_Sign_RejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateKeyPair(filepath.Join(dir, "release.key"), filepath.Join(dir, "release.pub")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "payload")
	if err := os.WriteFile(target, []byte("basalt-release"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SignFile(filepath.Join(dir, "release.key"), dir, "payload"); err != nil {
		t.Fatalf("签名：%v", err)
	}
	otherDir := t.TempDir()
	otherKey := filepath.Join(otherDir, "release.pub")
	if err := GenerateKeyPair(filepath.Join(otherDir, "release.key"), otherKey); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFileSignature(otherKey, target, filepath.Join(dir, "payload.sig")); err == nil {
		t.Fatal("错密钥必须验签失败")
	}
}
