//go:build e2e

package e2e

// 本文件判 US-17 的确定性构建（FR-03）：同一源码、同一工具链连续构建两次，二进制
// 摘要必须一致。走发布工具的真实构建参数（`-trimpath` + 版本注入 + CGO 关闭）。
// This file covers US-17's deterministic build: two consecutive builds of the same
// source with the release flags must produce the same digest.

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/ymedword/basalt/scripts/releasekit"
)

// TestUS17_Reproducible：双构建哈希一致（FR-03）。
// TestUS17_Reproducible: two builds produce the same binary digest.
func TestUS17_Reproducible(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	in := releasekit.Inputs{Repo: root, Version: "0.0.0"}
	target := releasekit.Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Tier: releasekit.TierBinary}

	first, err := releasekit.BuildTarget(ctx, in, target, t.TempDir())
	if err != nil {
		t.Fatalf("第一次构建：%v", err)
	}
	second, err := releasekit.BuildTarget(ctx, in, target, t.TempDir())
	if err != nil {
		t.Fatalf("第二次构建：%v", err)
	}
	if first.BinarySHA != second.BinarySHA {
		t.Fatalf("双构建摘要不一致：%s vs %s（-trimpath 或构建参数被破坏）",
			first.BinarySHA, second.BinarySHA)
	}
}
