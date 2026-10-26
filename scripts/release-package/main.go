// Package main 实现 release-package：按 `release/runtime-manifest.json` 校验随包
// 运行时组件，组装完整发行包（产品二进制 + `runtime/` + 许可证清单），并输出可核对
// 的 JSON 读数。它**只做打包与校验**，不下载组件、不访问网络——组件版本由构建方按
// 清单预置在运行时目录里，工具负责"版本对不上就拒绝出包"。
// Package main implements release-package: it validates the bundled runtime
// components against the pinned manifest, assembles the complete package and prints
// machine-readable readings. It downloads nothing; a version mismatch refuses packaging.
package main

import (
	"debug/buildinfo"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const (
	defaultManifest = "release/runtime-manifest.json"
	defaultReadme   = "release/package-README.txt"
	defaultOutDir   = "dist"
)

var versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// options 是一次打包的显式输入。
// options carries the explicit inputs of one package run.
type options struct {
	repo       string
	manifest   string
	readme     string
	binary     string
	runtimeDir string
	outDir     string
	version    string
}

// report 是打包结果的可核对读数：哈希、体积、组件与许可证文件数。
// report records the package readings: digests, sizes, components and license files.
type report struct {
	Package       string            `json:"package"`
	Version       string            `json:"version"`
	Platform      string            `json:"platform"`
	Arch          string            `json:"arch"`
	BinarySHA256  string            `json:"binary_sha256"`
	ArchiveSHA256 string            `json:"archive_sha256"`
	ArchiveBytes  int64             `json:"archive_bytes"`
	RawBytes      int64             `json:"raw_bytes"`
	FileCount     int               `json:"file_count"`
	Components    []componentReport `json:"components"`
	LicenseFiles  int               `json:"license_files"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "release-package:", err)
		os.Exit(1)
	}
}

// run 解析参数并完成一次打包；测试直接调用它。
// run parses the flags and packages once; tests call it directly.
func run(args []string, stdout io.Writer) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	manifest, err := loadManifest(opts.manifest, runtime.GOOS)
	if err != nil {
		return err
	}
	if err := checkReleaseBinary(opts.binary); err != nil {
		return err
	}
	components, err := verifyComponents(manifest, opts.runtimeDir, runtime.GOOS)
	if err != nil {
		return err
	}

	stageRoot, err := os.MkdirTemp("", "basalt-release-package-")
	if err != nil {
		return fmt.Errorf("创建打包暂存目录：%w", err)
	}
	defer func() { _ = os.RemoveAll(stageRoot) }()

	// 包内顶层目录带版本号：解压多个版本时不会互相覆盖。
	// The archive's top-level directory carries the version.
	packageName := "basalt-" + opts.version
	programDir := filepath.Join(stageRoot, packageName)
	binaryName := "basalt"
	if runtime.GOOS == "windows" {
		binaryName = "basalt.exe"
	}
	if err := os.MkdirAll(programDir, 0o755); err != nil { // #nosec G301 -- 包内目录对所有用户可读
		return fmt.Errorf("创建包内程序目录：%w", err)
	}
	if err := copyFile(opts.binary, filepath.Join(programDir, binaryName)); err != nil {
		return fmt.Errorf("复制产品二进制：%w", err)
	}
	if err := copyTree(opts.runtimeDir, filepath.Join(programDir, "runtime")); err != nil {
		return fmt.Errorf("复制 runtime/：%w", err)
	}
	// 产品自身的许可证与第三方清单分开：LICENSE 是 Basalt 的，LICENSES/ 是随包组件的。
	// The product license stays at the root; third-party texts live under LICENSES/.
	if err := copyFile(filepath.Join(opts.repo, "LICENSE"), filepath.Join(programDir, "LICENSE")); err != nil {
		return fmt.Errorf("复制产品许可证：%w", err)
	}
	label := platformLabel(runtime.GOOS, runtime.GOARCH)
	inventory, err := collectLicenses(opts.repo, opts.runtimeDir, opts.binary, programDir,
		manifest, label, components)
	if err != nil {
		return err
	}
	if err := writeReadme(opts.readme, programDir, opts.version, label); err != nil {
		return err
	}

	if err := os.MkdirAll(opts.outDir, 0o755); err != nil { // #nosec G301 -- 发行产物目录对所有用户可读
		return fmt.Errorf("创建输出目录 %s：%w", opts.outDir, err)
	}
	archivePath := filepath.Join(opts.outDir,
		fmt.Sprintf("basalt-%s-%s-%s.zip", opts.version, runtime.GOOS, runtime.GOARCH))
	// 压缩暂存根而不是程序目录，让包内保留 `basalt-<version>/` 顶层目录：
	// 解压多个版本时不会互相覆盖。
	// Zip the staging root so the archive keeps the versioned top-level directory.
	stats, err := zipTree(stageRoot, archivePath)
	if err != nil {
		return fmt.Errorf("生成压缩包：%w", err)
	}
	binarySum, err := sha256File(filepath.Join(programDir, binaryName))
	if err != nil {
		return err
	}
	archiveSum, err := sha256File(archivePath)
	if err != nil {
		return err
	}
	archiveInfo, err := os.Stat(archivePath)
	if err != nil {
		return err
	}

	result := report{
		Package:       packageName,
		Version:       opts.version,
		Platform:      runtime.GOOS,
		Arch:          runtime.GOARCH,
		BinarySHA256:  binarySum,
		ArchiveSHA256: archiveSum,
		ArchiveBytes:  archiveInfo.Size(),
		RawBytes:      stats.bytes,
		FileCount:     stats.files,
		Components:    components,
		LicenseFiles:  inventory.files,
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化读数：%w", err)
	}
	if _, err := fmt.Fprintln(stdout, string(encoded)); err != nil {
		return err
	}
	return nil
}

// parseOptions 解析命令行并补齐相对仓库根的默认路径。
// parseOptions resolves the flags and the repo-relative defaults.
func parseOptions(args []string) (options, error) {
	var (
		opts     options
		manifest string
		readme   string
	)
	flags := flag.NewFlagSet("release-package", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.repo, "repo", ".", "仓库根（读 LICENSE 与默认清单）")
	flags.StringVar(&manifest, "manifest", "", "运行时组件清单（默认 release/runtime-manifest.json）")
	flags.StringVar(&readme, "readme", "", "包内 README 模板（默认 release/package-README.txt）")
	flags.StringVar(&opts.binary, "binary", "", "产品二进制路径（必填，须 CGO_ENABLED=0 构建）")
	flags.StringVar(&opts.runtimeDir, "runtime", "", "运行时目录路径（必填，含 node/ python/ mcp/）")
	flags.StringVar(&opts.outDir, "out", defaultOutDir, "输出目录")
	flags.StringVar(&opts.version, "version", "", "版本号（必填，形如 0.1.0）")
	if err := flags.Parse(args); err != nil {
		return options{}, fmt.Errorf("解析参数：%w", err)
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("不接受位置参数：%v", flags.Args())
	}
	repo, err := filepath.Abs(opts.repo)
	if err != nil {
		return options{}, fmt.Errorf("解析仓库根：%w", err)
	}
	opts.repo = repo
	opts.manifest = manifest
	if opts.manifest == "" {
		opts.manifest = filepath.Join(repo, filepath.FromSlash(defaultManifest))
	}
	opts.readme = readme
	if opts.readme == "" {
		opts.readme = filepath.Join(repo, filepath.FromSlash(defaultReadme))
	}
	if strings.TrimSpace(opts.binary) == "" || strings.TrimSpace(opts.runtimeDir) == "" {
		return options{}, fmt.Errorf("必须同时给出 -binary 与 -runtime")
	}
	if strings.TrimSpace(opts.outDir) == "" {
		return options{}, fmt.Errorf("-out 不能为空")
	}
	if !versionPattern.MatchString(opts.version) {
		return options{}, fmt.Errorf("版本号 %q 不是 X.Y.Z 形式", opts.version)
	}
	return opts, nil
}

// checkReleaseBinary 拒绝非发布形态的二进制：GOOS / GOARCH 必须与本机一致（打包只能
// 校验本机可运行的组件），且必须 `CGO_ENABLED=0`——发布纪律不能靠回忆保证。
// checkReleaseBinary refuses a binary that is not a CGO-free build for this platform.
func checkReleaseBinary(path string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取二进制构建信息 %s：%w", path, err)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if got := settings["GOOS"]; got != runtime.GOOS {
		return fmt.Errorf("二进制 GOOS=%q 与打包平台 %q 不一致——跨平台包必须在目标平台打包", got, runtime.GOOS)
	}
	if got := settings["GOARCH"]; got != runtime.GOARCH {
		return fmt.Errorf("二进制 GOARCH=%q 与打包架构 %q 不一致", got, runtime.GOARCH)
	}
	if got := settings["CGO_ENABLED"]; got != "0" {
		return fmt.Errorf("二进制 CGO_ENABLED=%q，发布构建必须是 0", got)
	}
	return nil
}
