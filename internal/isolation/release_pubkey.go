package isolation

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// releasePublicPEM 是既有发布公钥；轮换必须经发布评审，不接受环境覆盖。
// releasePublicPEM is the existing release public key; rotation requires release review and cannot use environment overrides.
const releasePublicPEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAXPoQA+7u+UPtxOYz+L2/VObnSYDlCoak6ZiraMC2y3Q=
-----END PUBLIC KEY-----
`

// ReleasePublicKey 返回内嵌 Ed25519 发布公钥的新副本。
// ReleasePublicKey returns a fresh copy of the embedded Ed25519 release public key.
func ReleasePublicKey() (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(releasePublicPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("内嵌发布公钥格式无效")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("内嵌发布公钥不是 Ed25519")
	}
	return append(ed25519.PublicKey(nil), pub...), nil
}
