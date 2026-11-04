package isolation

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUS20_TrustManifestSignature(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("1", 64)
	body := `{"schema":2,"app_base":"alpine:3@` + digest + `","postgres":"pg:17@` + digest + `","sandbox":{"oci":"basalt:test@` + digest + `","gvisor":"basalt:test@` + digest + `"}}`
	path := filepath.Join(t.TempDir(), "image-lock.json")
	write := func(content string, sign bool) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if sign {
			if err := os.WriteFile(path+".sig", ed25519.Sign(key, []byte(content)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(body, true)
	imgs, err := LoadApprovedImages(path, pub)
	if err != nil || !imgs.Approved(digest) || len(imgs.Digests()) != 1 {
		t.Fatalf("valid manifest: images=%v err=%v", imgs, err)
	}
	copyDigests := imgs.Digests()
	copyDigests[digest] = false
	if !imgs.Approved(digest) {
		t.Fatal("caller mutated trust set")
	}
	if imgs.Approved("sha256:" + strings.Repeat("2", 64)) {
		t.Fatal("unlisted digest trusted")
	}
	write(body+" ", false)
	if _, err := LoadApprovedImages(path, pub); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	write(body, true)
	wrong, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovedImages(path, wrong); err == nil {
		t.Fatal("wrong public key accepted")
	}
	for name, invalid := range map[string]string{
		"old_schema":      strings.Replace(body, `"schema":2`, `"schema":1`, 1),
		"short_digest":    strings.ReplaceAll(body, digest, "sha256:1111"),
		"tag_only":        strings.ReplaceAll(body, "basalt:test@"+digest, "basalt:latest"),
		"missing_sandbox": `{"schema":2,"app_base":"alpine:3@` + digest + `","postgres":"pg:17@` + digest + `"}`,
		"unknown_field":   strings.Replace(body, `"schema":2`, `"schema":2,"approved":true`, 1),
		"trailing_json":   body + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			write(invalid, true)
			if _, err := LoadApprovedImages(path, pub); err == nil {
				t.Fatal("signed invalid content accepted")
			}
		})
	}
	if err := os.Remove(path + ".sig"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadApprovedImages(path, pub); err == nil {
		t.Fatal("missing signature accepted")
	}
	if _, err := LoadApprovedImages(path, nil); err == nil {
		t.Fatal("nil key accepted")
	}
	if _, err := LoadApprovedImages(filepath.Join(t.TempDir(), "missing"), pub); err == nil {
		t.Fatal("missing manifest accepted")
	}
	if _, err := ReleasePublicKey(); err != nil {
		t.Fatal(err)
	}
}
