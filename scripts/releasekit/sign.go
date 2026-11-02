package releasekit

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SignatureInfo 记录一次签名的结果。
// SignatureInfo records one signature result.
type SignatureInfo struct {
	File        string `json:"file"`
	Signature   string `json:"signature"`
	Fingerprint string `json:"public_key_fingerprint"`
}

// GenerateKeyPair 生成临时 Ed25519 密钥对，只供测试与本地演练；生产密钥由用户保管。
// GenerateKeyPair creates a temporary Ed25519 key pair for tests and local drills only;
// production keys are user-owned.
func GenerateKeyPair(privatePath, publicPath string) error {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return err
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	if err := os.WriteFile(privatePath, privatePEM, 0o600); err != nil {
		return err
	}
	// #nosec G306 -- 公钥必须随发布目录分发给下载者，本身不是秘密
	return os.WriteFile(publicPath, publicPEM, 0o644)
}

// SignFile 对目录内文件做分离签名，输出 `<name>.sig`（64 字节原始签名）。
// SignFile writes a detached `<name>.sig` (64 raw bytes) for a file in dir.
func SignFile(keyPath, dir, name string) (SignatureInfo, error) {
	privateKey, err := loadPrivateKey(keyPath)
	if err != nil {
		return SignatureInfo{}, err
	}
	body, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- 发布文件路径
	if err != nil {
		return SignatureInfo{}, err
	}
	signature := ed25519.Sign(privateKey, body)
	sigName := name + ".sig"
	// #nosec G306,G703 -- 签名文件随发布目录分发；文件名来自调用方固定的清单名
	if err := os.WriteFile(filepath.Join(dir, sigName), signature, 0o644); err != nil {
		return SignatureInfo{}, err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	fingerprint, err := fingerprintFor(publicKey)
	if err != nil {
		return SignatureInfo{}, err
	}
	return SignatureInfo{File: sigName, Signature: sigName, Fingerprint: fingerprint}, nil
}

// VerifyFileSignature 用公钥验证分离签名。
// VerifyFileSignature verifies a detached signature with the public key.
func VerifyFileSignature(pubPath, file, sigPath string) error {
	publicKey, err := loadPublicKey(pubPath)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(file) // #nosec G304 -- 发布文件路径
	if err != nil {
		return err
	}
	signature, err := os.ReadFile(sigPath) // #nosec G304 -- 发布文件路径
	if err != nil {
		return err
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("签名文件 %s 长度 %d，期望 %d", sigPath, len(signature), ed25519.SignatureSize)
	}
	if !ed25519.Verify(publicKey, body, signature) {
		return errors.New("签名校验失败：公钥不匹配或文件被篡改")
	}
	return nil
}

// Fingerprint 返回公钥 DER 的 SHA-256 十六进制指纹。
// Fingerprint returns the SHA-256 hex fingerprint of the public key DER.
func Fingerprint(pubPath string) (string, error) {
	publicKey, err := loadPublicKey(pubPath)
	if err != nil {
		return "", err
	}
	return fingerprintFor(publicKey)
}

func fingerprintFor(publicKey ed25519.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- 密钥路径由用户显式传入
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("签名密钥必须是 PKCS#8 PEM（PRIVATE KEY）")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("签名密钥不是 Ed25519")
	}
	return privateKey, nil
}

func loadPublicKey(path string) (ed25519.PublicKey, error) {
	body, err := os.ReadFile(path) // #nosec G304 -- 公钥路径由用户显式传入
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("公钥必须是 PKIX PEM（PUBLIC KEY）")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	publicKey, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("公钥不是 Ed25519")
	}
	return publicKey, nil
}
