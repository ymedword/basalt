//go:build e2e

package e2e

// US-18 A1：真实 `docker build` 产出的镜像记录必须与镜像本身、输入二进制和基础
// 镜像锁一致；同一二进制重复构建只能有局部标识与 provenance 哈希不同。
// A1: the real image record matches the image, the input binary and the pinned base,
// and a repeat build differs only in the local identity and provenance hash.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ymedword/basalt/scripts/releasekit"
)

// TestUS18_Image_BuildAndRecord 覆盖 A1 的全部判据。
// TestUS18_Image_BuildAndRecord covers every A1 verdict.
func TestUS18_Image_BuildAndRecord(t *testing.T) {
	us18RequireDocker(t)
	root := us18RepoRoot(t)
	lock := mustImageLock(t, root)
	us18EnsureImage(t, lock.AppBase)

	first := us18BuildImageOnce(t, us18DefaultVersion, lock.AppBase)

	if err := first.Validate(); err != nil {
		t.Fatalf("ImageRecord.Validate 失败: %v", err)
	}
	if first.Platform != "linux/amd64" {
		t.Fatalf("镜像平台 = %q，期望 linux/amd64", first.Platform)
	}
	if first.Tier != releasekit.TierBinary {
		t.Fatalf("镜像档位 = %q，期望 %q", first.Tier, releasekit.TierBinary)
	}
	if strings.TrimSpace(first.Tag) == "" {
		t.Fatal("镜像记录的 tag 为空")
	}
	if first.BaseRef != lock.AppBase {
		t.Fatalf("镜像记录的基础引用 = %q，期望锁文件里的 %q", first.BaseRef, lock.AppBase)
	}
	if !strings.HasSuffix(lock.AppBase, first.BaseDigest) || first.BaseDigest == "" {
		t.Fatalf("镜像记录的基础摘要 = %q，与锁文件 %q 不符", first.BaseDigest, lock.AppBase)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	inspect, err := us18DockerOutput(ctx, "image", "inspect", "--format", "{{.Id}}", first.Tag)
	if err != nil {
		t.Fatalf("检查镜像 %s 失败: %v", first.Tag, err)
	}
	if got := strings.TrimSpace(inspect); got != first.LocalID {
		t.Fatalf("docker image inspect Id = %q，记录 local_id = %q", got, first.LocalID)
	}

	binary := us18BinaryFor(t, us18DefaultVersion)
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("读取二进制失败: %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != first.BinarySHA256 {
		t.Fatalf("binary_sha256 = %q，输入文件哈希 = %q", first.BinarySHA256, got)
	}

	// 同输入、独立产物目录的第二次构建：除镜像局部标识与 provenance 哈希（记录构建
	// 时间）之外，所有字段必须逐字相同；路径类字段必须是同一相对名，不能把临时目录
	// 带进记录，否则发布的记录一出宿主就失效。
	// A second build from the same inputs and a different output directory differs
	// only in the local identity and the provenance hash.
	second := us18BuildImageOnce(t, us18DefaultVersion, lock.AppBase)
	trimmed := func(record releasekit.ImageRecord) releasekit.ImageRecord {
		record.LocalID = ""
		record.ProvenanceSHA256 = ""
		return record
	}
	if !reflect.DeepEqual(trimmed(first), trimmed(second)) {
		firstJSON, _ := json.MarshalIndent(trimmed(first), "", "  ")
		secondJSON, _ := json.MarshalIndent(trimmed(second), "", "  ")
		t.Fatalf("同输入两次构建的记录不一致：\n第一次：\n%s\n第二次：\n%s", firstJSON, secondJSON)
	}
}
