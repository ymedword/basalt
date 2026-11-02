package releasekit

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 中文验收点：平台、CGO 与提交号任一项不符都必须拒绝，不能把别的平台产物
// 打上目标平台的名字。
// Every mismatch in platform, CGO or commit must be rejected so a foreign binary
// cannot be published under the requested target's name.
func TestUS17_Build_CheckBuildInfoRejects(t *testing.T) {
	target := Target{GOOS: "linux", GOARCH: "amd64", Tier: TierBinary}
	commit := strings.Repeat("a", 40)
	good := func() buildInfo {
		return buildInfo{goVersion: "go1.27.3", settings: map[string]string{
			"GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0", "vcs.revision": commit,
		}}
	}
	cases := []struct {
		name   string
		mutate func(*buildInfo)
		want   string
	}{
		{"goos", func(info *buildInfo) { info.settings["GOOS"] = "windows" }, "GOOS"},
		{"goarch", func(info *buildInfo) { info.settings["GOARCH"] = "arm64" }, "GOARCH"},
		{"cgo", func(info *buildInfo) { info.settings["CGO_ENABLED"] = "1" }, "CGO_ENABLED"},
		{"revision", func(info *buildInfo) { info.settings["vcs.revision"] = strings.Repeat("b", 40) }, "VCS revision"},
	}
	for _, testCase := range cases {
		info := good()
		testCase.mutate(&info)
		err := checkBuildInfo(info, target, commit)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: 期望错误含 %q，实际 %v", testCase.name, testCase.want, err)
		}
	}
	if err := checkBuildInfo(good(), target, commit); err != nil {
		t.Fatalf("匹配的构建信息应通过：%v", err)
	}
	// 未声明 commit 或二进制没盖 VCS revision 时不做提交号对账。
	if err := checkBuildInfo(good(), target, ""); err != nil {
		t.Fatalf("未声明 commit 不应失败：%v", err)
	}
	noRevision := good()
	delete(noRevision.settings, "vcs.revision")
	if err := checkBuildInfo(noRevision, target, commit); err != nil {
		t.Fatalf("无 VCS revision 不应失败：%v", err)
	}
}

func TestUS17_Build_ReadsOwnBuildInfo(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试二进制：%v", err)
	}
	info, err := readBuildInfo(executable)
	if err != nil {
		t.Fatalf("读取构建信息：%v", err)
	}
	if info.goVersion == "" {
		t.Fatal("goVersion 不应为空")
	}
	if info.settings["GOOS"] == "" || info.settings["GOARCH"] == "" {
		t.Fatalf("构建设置缺少平台字段：%v", info.settings)
	}
	if _, err := readBuildInfo(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("不存在路径应报错")
	}
}

func TestUS17_Build_CompareAndHost(t *testing.T) {
	if got := CompareBuilds(BuildResult{BinarySHA: "same"}, BuildResult{BinarySHA: "same"}); !got.Checked || !got.Equal {
		t.Fatalf("同摘要应记为 checked+equal：%+v", got)
	}
	if got := CompareBuilds(BuildResult{BinarySHA: "a"}, BuildResult{BinarySHA: "b"}); !got.Checked || got.Equal {
		t.Fatalf("不同摘要应记为 checked+not equal：%+v", got)
	}
	if got := CompareBuilds(BuildResult{}, BuildResult{}); !got.Checked || got.Equal {
		t.Fatalf("空摘要不得判为相等：%+v", got)
	}
	host := Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Tier: TierBinary}
	if !HostMatches(host) {
		t.Fatalf("%s 应匹配宿主", host.String())
	}
	other := Target{GOOS: runtime.GOOS, GOARCH: "mips64", Tier: TierBinary}
	if HostMatches(other) {
		t.Fatalf("%s 不应匹配宿主", other.String())
	}
}

// 中文验收点：宿主 binary 档必须真的能按固定参数构建，且 full 档在非宿主上拒绝。
// The host binary tier must build with the fixed flags, and a full tier must be
// rejected on a foreign host.
func TestUS17_Build_HostBinaryAndVerifier(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("取仓库根：%v", err)
	}
	in := Inputs{Repo: repo, Version: "9.9.9-test", Mode: ModeRehearsal}
	target := Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Tier: TierBinary}
	outDir := t.TempDir()
	build, err := BuildTarget(context.Background(), in, target, outDir)
	if err != nil {
		t.Fatalf("宿主构建失败：%v", err)
	}
	if build.CGOEnabled != "0" {
		t.Fatalf("CGO_ENABLED=%q，期望 0", build.CGOEnabled)
	}
	if build.BinarySHA == "" {
		t.Fatal("BinarySHA 不应为空")
	}
	if _, err := os.Stat(build.Binary); err != nil {
		t.Fatalf("构建产物不存在：%v", err)
	}
	verifier, err := BuildVerifier(context.Background(), repo, "9.9.9-test", target, outDir)
	if err != nil {
		t.Fatalf("构建校验器失败：%v", err)
	}
	if _, err := os.Stat(verifier); err != nil {
		t.Fatalf("校验器不存在：%v", err)
	}
	foreign := Target{GOOS: "linux", GOARCH: "arm64", Tier: TierFull}
	if HostMatches(foreign) {
		t.Skip("当前宿主就是 linux/arm64，无法验证 foreign full 拒绝")
	}
	if _, err := BuildTarget(context.Background(), in, foreign, outDir); err == nil ||
		!strings.Contains(err.Error(), "匹配宿主") {
		t.Fatalf("非宿主 full 档应拒绝，实际 %v", err)
	}
}
