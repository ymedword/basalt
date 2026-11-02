package releasekit

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// BuildTarget 以固定参数构建一个目标。full 档只能在匹配宿主构建。
// BuildTarget builds one target with fixed flags. A full tier is only built on a
// matching host.
func BuildTarget(ctx context.Context, in Inputs, target Target, outDir string) (BuildResult, error) {
	if !target.Valid() {
		return BuildResult{}, fmt.Errorf("目标 %s 不在四平台清单内", target.String())
	}
	if target.Tier == TierFull && !HostMatches(target) {
		return BuildResult{}, fmt.Errorf("full 档必须在匹配宿主构建：当前 %s/%s，目标 %s",
			runtime.GOOS, runtime.GOARCH, target.String())
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil { // #nosec G301 -- 发布产物目录对所有用户可读
		return BuildResult{}, fmt.Errorf("创建构建目录：%w", err)
	}
	binary := filepath.Join(outDir, target.BinaryName())
	args := []string{
		"build", "-trimpath",
		"-ldflags", "-X main.version=" + in.Version,
		"-o", binary,
		"./cmd/basalt",
	}
	cmd := exec.CommandContext(ctx, "go", args...) // #nosec G204 -- 固定 go build 与显式构造的发布参数，不来自外部输入
	cmd.Dir = in.Repo
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+target.GOOS,
		"GOARCH="+target.GOARCH,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return BuildResult{}, fmt.Errorf("构建 %s 失败：%w\n%s", target.String(), err, out)
	}
	info, err := readBuildInfo(binary)
	if err != nil {
		return BuildResult{}, err
	}
	if err := checkBuildInfo(info, target, in.Commit); err != nil {
		return BuildResult{}, err
	}
	sum, err := FileSHA256(binary)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{
		Target:     target,
		Version:    in.Version,
		Commit:     in.Commit,
		GoVersion:  info.goVersion,
		CGOEnabled: info.settings["CGO_ENABLED"],
		Binary:     binary,
		BinarySHA:  sum,
	}, nil
}

// BuildVerifier 构建四平台校验器。
// BuildVerifier builds the download-directory verifier for one target.
func BuildVerifier(ctx context.Context, repo, version string, target Target, outDir string) (string, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil { // #nosec G301 -- 发布产物目录
		return "", fmt.Errorf("创建校验器输出目录：%w", err)
	}
	name := "basalt-verify-" + version + "-" + target.GOOS + "-" + target.GOARCH
	if target.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(outDir, name)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", // #nosec G204 -- 固定 go build 与显式构造的校验器参数
		"-ldflags", "-X main.version="+version, "-o", path, "./cmd/basalt-verify")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+target.GOOS, "GOARCH="+target.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("构建校验器 %s 失败：%w\n%s", target.String(), err, out)
	}
	return path, nil
}

type buildInfo struct {
	goVersion string
	settings  map[string]string
}

func readBuildInfo(path string) (buildInfo, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return buildInfo{}, fmt.Errorf("读取构建信息 %s：%w", path, err)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	return buildInfo{goVersion: info.GoVersion, settings: settings}, nil
}

// checkBuildInfo 核对二进制确实按请求的平台与 CGO 关闭构建，并在能读到 VCS
// revision 时与声明的 commit 对账；不一致即拒绝，避免把别的平台产物打上目标平台的名字。
// checkBuildInfo verifies the binary was built for the requested platform with CGO
// disabled and, when the VCS revision is stamped, that it matches the declared commit.
func checkBuildInfo(info buildInfo, target Target, commit string) error {
	if got := info.settings["GOOS"]; got != target.GOOS {
		return fmt.Errorf("二进制 GOOS=%q 与目标 %q 不一致——跨平台包必须在目标平台构建", got, target.GOOS)
	}
	if got := info.settings["GOARCH"]; got != target.GOARCH {
		return fmt.Errorf("二进制 GOARCH=%q 与目标 %q 不一致", got, target.GOARCH)
	}
	if got := info.settings["CGO_ENABLED"]; got != "0" {
		return fmt.Errorf("二进制 CGO_ENABLED=%q，发布构建必须是 0", got)
	}
	if revision := info.settings["vcs.revision"]; commit != "" && revision != "" && revision != commit {
		return fmt.Errorf("二进制 VCS revision %s 与声明 commit %s 不一致", revision, commit)
	}
	return nil
}

// CompareBuilds 比较两次构建的二进制摘要。
// CompareBuilds compares the binary digests of two builds.
func CompareBuilds(first, second BuildResult) Reproducible {
	return Reproducible{
		Checked:           true,
		Equal:             first.BinarySHA != "" && first.BinarySHA == second.BinarySHA,
		SecondBuildSHA256: second.BinarySHA,
	}
}

// HostMatches 报告目标是否就是当前宿主。
// HostMatches reports whether the target is the current host.
func HostMatches(target Target) bool {
	return target.GOOS == runtime.GOOS && target.GOARCH == runtime.GOARCH
}

// FileSHA256 返回文件的 SHA-256 十六进制摘要。
// FileSHA256 returns the file's SHA-256 hex digest.
func FileSHA256(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- 路径来自发布工具显式输入
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
