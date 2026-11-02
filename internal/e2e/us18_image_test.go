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

// TestUS18_Image_PreExistingTagPreserved 是镜像 tag 隔离与清理归属保护的回归用例。
// 修复前测试固定使用 basalt:0.0.1-linux-amd64：构建会覆盖宿主同名 tag，清理按 tag
// 名直接删除，宿主原有镜像就此失去标签。现在测试版本每次运行随机（tag 唯一），
// 清理前核对本次构建登记过的镜像 ID。判据：
//  1. 宿主预先存在的 basalt:0.0.1-linux-amd64（连同其 ID）在本次构建与清理后原样保留；
//  2. 登记 ID 与 tag 当前指向不一致时，删除被拒绝并说明原因；
//  3. 阳性对照：归属一致时删除生效（清理不是空转）。
//
// TestUS18_Image_PreExistingTagPreserved guards tag isolation and ownership-checked
// cleanup: a pre-existing basalt:0.0.1-linux-amd64 keeps its tag and ID, a mismatched
// recorded ID refuses deletion, and a matching ID deletes normally.
func TestUS18_Image_PreExistingTagPreserved(t *testing.T) {
	us18RequireDocker(t)
	root := us18RepoRoot(t)
	lock := mustImageLock(t, root)
	us18EnsureImage(t, lock.AppBase)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 宿主“预先存在”的 tag：修复前的测试正是用这个名字覆盖 / 删除别人的镜像。
	const preTag = "basalt:0.0.1-linux-amd64"
	preID, existed := us18ImageID(ctx, preTag)
	created := ""
	if !existed {
		if _, err := us18DockerOutput(ctx, "tag", lock.AppBase, preTag); err != nil {
			t.Fatalf("准备预先存在的镜像 tag 失败: %v", err)
		}
		preID, existed = us18ImageID(ctx, preTag)
		if !existed {
			t.Fatalf("tag %s 创建后仍不存在", preTag)
		}
		created = preID
		// 样本由本用例创建：断言全部完成后按归属删除，不给宿主留新 tag。
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			if _, _, err := us18RemoveImageTag(cleanupCtx, preTag, created); err != nil {
				t.Errorf("清理用例创建的 tag %s 失败: %v", preTag, err)
			}
		})
	}

	// 走与其他用例相同的构建路径：tag 必须来自本次运行独有的版本。
	record := us18BuildImageOnce(t, us18DefaultVersion, lock.AppBase)
	if record.Tag == preTag {
		t.Fatalf("测试镜像仍使用宿主可能已有的固定 tag %s", preTag)
	}

	// 归属保护：把预先存在的 tag 配上本次构建的 ID，删除必须被拒绝。
	removed, note, err := us18RemoveImageTag(ctx, preTag, record.LocalID)
	if err != nil {
		t.Fatalf("归属不匹配不应报错: %v", err)
	}
	if removed {
		t.Fatalf("tag %s 与登记 ID 不一致时仍被删除", preTag)
	}
	if note == "" {
		t.Fatal("归属不一致时必须说明保留原因")
	}
	if got, ok := us18ImageID(ctx, preTag); !ok || got != preID {
		t.Fatalf("预先存在的 tag %s 被改动：ID = %q（存在=%v），期望 %q", preTag, got, ok, preID)
	}

	// 阳性对照：只属于本用例的探针 tag，归属一致时删除生效。
	probeTag := "us18-e2e-probe:" + us18DefaultVersion
	if _, exists := us18ImageID(ctx, probeTag); exists {
		t.Logf("探针 tag %s 已存在（非常规残留），跳过阳性对照", probeTag)
		return
	}
	if _, err := us18DockerOutput(ctx, "tag", lock.AppBase, probeTag); err != nil {
		t.Fatalf("创建探针 tag 失败: %v", err)
	}
	probeID, ok := us18ImageID(ctx, probeTag)
	if !ok {
		t.Fatalf("探针 tag %s 创建后不存在", probeTag)
	}
	removed, _, err = us18RemoveImageTag(ctx, probeTag, probeID)
	if err != nil || !removed {
		t.Fatalf("归属一致时删除应成功：removed=%v err=%v", removed, err)
	}
	if _, still := us18ImageID(ctx, probeTag); still {
		t.Fatalf("归属一致删除后探针 tag %s 仍存在", probeTag)
	}
}
