// Package isolation 验证执行隔离的发布信任材料，不依赖工具或装配层。
// Package isolation verifies execution-isolation release trust independently of tools and wiring.
package isolation

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// ApprovedImages 保存签名清单批准的不可变 digest 集合。
// ApprovedImages holds the immutable digests approved by a signed manifest.
type ApprovedImages struct{ digests map[string]bool }

// Approved 判断 digest 是否在已验证清单中。
// Approved reports whether a digest belongs to the verified manifest.
func (a ApprovedImages) Approved(digest string) bool { return a.digests[digest] }

// Digests 返回集合副本，防止调用方更改信任源。
// Digests returns a copy so callers cannot mutate the trust source.
func (a ApprovedImages) Digests() map[string]bool {
	m := make(map[string]bool, len(a.digests))
	for d, allowed := range a.digests {
		m[d] = allowed
	}
	return m
}

// ImageManifest 是 schema 2 的外置发布镜像清单。
// ImageManifest is the external schema-2 release image manifest.
type ImageManifest struct {
	Schema   int           `json:"schema"`
	AppBase  string        `json:"app_base"`
	Postgres string        `json:"postgres"`
	Sandbox  SandboxImages `json:"sandbox"`
}

// SandboxImages 分别固定两档执行镜像，允许共用同一个镜像。
// SandboxImages pins both execution profiles and permits a shared image.
type SandboxImages struct {
	OCI    string `json:"oci"`
	GVisor string `json:"gvisor"`
}

var pinnedImage = regexp.MustCompile(`^[^\s@]+:[^\s/@:]+@sha256:[0-9a-f]{64}$`)

// ParseImageManifest 校验 schema、完整引用和未知字段；源锁可留空执行镜像。
// ParseImageManifest validates schema, complete references and unknown fields; source locks may omit execution images.
func ParseImageManifest(body []byte, requireSandbox bool) (ImageManifest, error) {
	var m ImageManifest
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("镜像清单格式无效: %w", err)
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return m, errors.New("镜像清单包含尾随内容")
	}
	if m.Schema != 2 {
		return m, errors.New("镜像清单必须为 schema 2")
	}
	for key, ref := range map[string]string{"app_base": m.AppBase, "postgres": m.Postgres, "sandbox.oci": m.Sandbox.OCI, "sandbox.gvisor": m.Sandbox.GVisor} {
		if !requireSandbox && strings.HasPrefix(key, "sandbox.") && ref == "" {
			continue
		}
		if !pinnedImage.MatchString(ref) {
			return m, fmt.Errorf("镜像清单 %s 必须为完整 tag@sha256 引用", key)
		}
	}
	return m, nil
}

// LoadApprovedImages 先验原始字节签名，再校验清单内容，不接受配置绕过。
// LoadApprovedImages verifies the raw signature before validating content, without a configuration bypass.
func LoadApprovedImages(path string, publicKey ed25519.PublicKey) (ApprovedImages, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return ApprovedImages{}, errors.New("发布公钥无效")
	}
	body, err := readLimited(path, 1<<20)
	if err != nil {
		return ApprovedImages{}, fmt.Errorf("读取镜像清单: %w", err)
	}
	sig, err := readLimited(path+".sig", ed25519.SignatureSize)
	if err != nil {
		return ApprovedImages{}, fmt.Errorf("读取镜像清单签名: %w", err)
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(publicKey, body, sig) {
		return ApprovedImages{}, errors.New("镜像清单签名无效")
	}
	m, err := ParseImageManifest(body, true)
	if err != nil {
		return ApprovedImages{}, err
	}
	digests := make(map[string]bool)
	for _, ref := range []string{m.Sandbox.OCI, m.Sandbox.GVisor} {
		_, digest, _ := strings.Cut(ref, "@")
		digests[digest] = true
	}
	return ApprovedImages{digests: digests}, nil
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- 受信配置指定外置清单路径，不读取任务提供的路径
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("信任材料超出大小限制")
	}
	return b, nil
}
