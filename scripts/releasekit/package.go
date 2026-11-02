package releasekit

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const packageManifestName = "MANIFEST.json"

var archiveFixedTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

type packageFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   string `json:"mode"`
}

type packageManifest struct {
	Schema  int           `json:"schema"`
	Version string        `json:"version"`
	Target  Target        `json:"target"`
	Files   []packageFile `json:"files"`
}

// runtimeComponentManifest 只取 full 档完整性校验需要的字段；完整清单由
// release/runtime-manifest.json 维护。
// runtimeComponentManifest keeps only the fields needed to check full-tier
// completeness; the full inventory lives in release/runtime-manifest.json.
type runtimeComponentManifest struct {
	Schema     int `json:"schema_version"`
	Components []struct {
		ID    string            `json:"id"`
		Paths map[string]string `json:"paths"`
	} `json:"components"`
}

// checkRuntimeComponents 要求 full 档在出包前备齐清单声明的全部组件。
// 缺组件时拒绝出包，而不是把只有部分 runtime 的归档标成 full；binary 档不调用本检查。
// checkRuntimeComponents requires every component declared in the runtime inventory
// before a full-tier package is written. A partial runtime must not be labeled full;
// binary packages never call this check.
func checkRuntimeComponents(repo, runtimeDir string, target Target) error {
	manifestPath := filepath.Join(repo, "release", "runtime-manifest.json")
	body, err := os.ReadFile(manifestPath) // #nosec G304 -- 仓库内固定路径
	if err != nil {
		return fmt.Errorf("full 档缺少 runtime 清单 %s，无法确认组件完整性；如只需产品二进制请改用 --tier binary", manifestPath)
	}
	var manifest runtimeComponentManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return fmt.Errorf("解析 runtime 清单 %s：%w", manifestPath, err)
	}
	if len(manifest.Components) == 0 {
		return fmt.Errorf("runtime 清单 %s 未声明任何组件，拒绝生成 full 包", manifestPath)
	}
	var missing []string
	for _, component := range manifest.Components {
		rel := strings.TrimSpace(component.Paths[target.GOOS])
		if rel == "" {
			missing = append(missing, component.ID+"（清单未声明 "+target.GOOS+" 路径）")
			continue
		}
		if !fileExists(filepath.Join(runtimeDir, filepath.FromSlash(rel))) {
			missing = append(missing, component.ID+"（缺 "+rel+"）")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("full 档 runtime 组件不齐，拒绝出包（补齐后重试或改用 --tier binary）：%s",
			strings.Join(missing, "、"))
	}
	return nil
}

// PackageTarget 组装并压缩一个目标。full 档要求 runtime 目录存在且组件齐全；
// binary 档不复制它。
// PackageTarget stages and archives one target. A full tier requires the runtime
// directory with every declared component present; a binary tier never copies it.
func PackageTarget(ctx context.Context, in Inputs, target Target, build BuildResult, opts ReleaseOptions) (PackageResult, error) {
	if err := ctx.Err(); err != nil {
		return PackageResult{}, err
	}
	if !target.Valid() {
		return PackageResult{}, fmt.Errorf("目标 %s 不在四平台清单内", target.String())
	}
	if build.Target != target {
		return PackageResult{}, fmt.Errorf("构建目标 %s 与打包目标 %s 不一致", build.Target.String(), target.String())
	}
	if target.Tier == TierFull {
		if strings.TrimSpace(opts.RuntimeDir) == "" {
			return PackageResult{}, errors.New("full 档必须提供 runtime 目录")
		}
		if err := checkRuntimeComponents(in.Repo, opts.RuntimeDir, target); err != nil {
			return PackageResult{}, err
		}
	}
	// 归档格式与归档命名绑定：windows 用 zip，其余用 tar.gz；--format 只用于
	// 显式复核，不接受与目标平台不符的取值。
	if format := strings.TrimSpace(opts.Format); format != "" {
		want := "tar.gz"
		if target.GOOS == "windows" {
			want = "zip"
		}
		if format != want {
			return PackageResult{}, fmt.Errorf("%s 的归档格式必须是 %s，实际 --format %s", target.String(), want, format)
		}
	}
	stage, err := os.MkdirTemp("", "basalt-release-stage-")
	if err != nil {
		return PackageResult{}, fmt.Errorf("创建打包暂存目录：%w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()

	programDir := filepath.Join(stage, "basalt-"+in.Version)
	if err := os.MkdirAll(programDir, 0o755); err != nil { // #nosec G301 -- 包内目录
		return PackageResult{}, err
	}
	if err := copyFile(build.Binary, filepath.Join(programDir, target.BinaryName()), 0o755); err != nil {
		return PackageResult{}, fmt.Errorf("复制产品二进制：%w", err)
	}
	if target.Tier == TierFull {
		if err := copyTree(opts.RuntimeDir, filepath.Join(programDir, "runtime")); err != nil {
			return PackageResult{}, fmt.Errorf("复制 runtime/：%w", err)
		}
	}
	if license := filepath.Join(in.Repo, "LICENSE"); fileExists(license) {
		if err := copyFile(license, filepath.Join(programDir, "LICENSE"), 0o644); err != nil {
			return PackageResult{}, fmt.Errorf("复制许可证：%w", err)
		}
	}
	if verify := filepath.Join(in.Repo, "release", "package-VERIFY.txt"); fileExists(verify) {
		if err := copyFile(verify, filepath.Join(programDir, "VERIFY.txt"), 0o644); err != nil {
			return PackageResult{}, fmt.Errorf("复制校验说明：%w", err)
		}
	}
	files, err := collectPackageFiles(programDir)
	if err != nil {
		return PackageResult{}, err
	}
	manifest := packageManifest{Schema: 1, Version: in.Version, Target: target, Files: files}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return PackageResult{}, err
	}
	body = append(body, '\n')
	// #nosec G306 -- 包内 MANIFEST.json 随归档分发，必须对下载者可读
	if err := os.WriteFile(filepath.Join(programDir, packageManifestName), body, 0o644); err != nil {
		return PackageResult{}, err
	}
	if err := CheckPackageContent(programDir); err != nil {
		return PackageResult{}, err
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil { // #nosec G301 -- 发布产物目录
		return PackageResult{}, err
	}
	archivePath := filepath.Join(opts.OutDir, target.ArchiveName(in.Version))
	if target.GOOS == "windows" {
		err = writeZipTree(stage, archivePath)
	} else {
		err = writeTarGzTree(stage, archivePath)
	}
	if err != nil {
		return PackageResult{}, err
	}
	archiveSum, err := FileSHA256(archivePath)
	if err != nil {
		return PackageResult{}, err
	}
	return PackageResult{
		Name:         filepath.Base(archivePath),
		Archive:      archivePath,
		Target:       target,
		SHA256:       archiveSum,
		BinarySHA256: build.BinarySHA,
		FileCount:    len(files) + 1,
	}, nil
}

func collectPackageFiles(root string) ([]packageFile, error) {
	var files []packageFile
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Base(path) == packageManifestName {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			files = append(files, packageFile{Path: rel, Size: int64(len(link)), SHA256: sha256Hex([]byte(link)), Mode: "120000"})
			return nil
		}
		sum, err := FileSHA256(path)
		if err != nil {
			return err
		}
		files = append(files, packageFile{Path: rel, Size: info.Size(), SHA256: sum, Mode: fmt.Sprintf("%04o", canonicalMode(info.Mode()))})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// CheckPackageContent 先判路径白名单，再对文本文件做秘密模式扫描；二进制跳过。
// CheckPackageContent applies the path allowlist first and then scans text files for
// secret patterns; binaries are skipped.
func CheckPackageContent(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if err := checkPackagePath(rel); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.Size() > 5<<20 {
			return nil
		}
		// #nosec G304,G122 -- 路径来自刚打包的暂存目录，暂存目录由本工具创建并独占
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		head := make([]byte, 8192)
		n, _ := io.ReadFull(file, head)
		head = head[:n]
		if isBinary(head) {
			return nil
		}
		body, err := os.ReadFile(path) // #nosec G304,G122 -- 同上，暂存目录由本工具创建
		if err != nil {
			return err
		}
		if pattern := findSecret(string(body)); pattern != "" {
			return fmt.Errorf("包内文件 %s 含疑似凭据（%s），拒绝出包", rel, pattern)
		}
		return nil
	})
}

func checkPackagePath(rel string) error {
	segments := strings.Split(rel, "/")
	for _, segment := range segments {
		switch segment {
		case ".git", "career", ".cache":
			return fmt.Errorf("包内路径 %s 含禁用目录 %q，拒绝出包", rel, segment)
		}
	}
	if strings.HasPrefix(rel, "docs/us/") || strings.HasPrefix(rel, "docs/interview/") {
		return fmt.Errorf("包内路径 %s 属于私有文档，拒绝出包", rel)
	}
	base := segments[len(segments)-1]
	switch base {
	case ".env", "CLAUDE.local.md", "id_ed25519", "id_rsa":
		return fmt.Errorf("包内路径 %s 是禁用文件，拒绝出包", rel)
	}
	return nil
}

func findSecret(text string) string {
	checks := []struct {
		marker  string
		pattern string
	}{
		{"PEM 私钥", `-----BEGIN PRIVATE KEY-----`},
		{"RSA 私钥", `-----BEGIN RSA PRIVATE KEY-----`},
		{"OpenSSH 私钥", `-----BEGIN OPENSSH PRIVATE KEY-----`},
		{"GitHub token", `ghp_[A-Za-z0-9]{20,}`},
		{"AWS access key", `AKIA[0-9A-Z]{16}`},
		{"OpenAI token", `sk-[A-Za-z0-9]{20,}`},
	}
	for _, check := range checks {
		if regexp.MustCompile(check.pattern).MatchString(text) {
			return check.marker
		}
	}
	return ""
}

func isBinary(head []byte) bool {
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// canonicalMode 把文件权限归一为发布归档的两种取值：可执行 0755，其余 0644。
// canonicalMode normalizes file modes to the two values allowed in release
// archives: 0755 when any execute bit is set, 0644 otherwise. 归一化让同一份源码
// 在不同机器上打出相同的归档（不同 umask 不会改变结果）。
func canonicalMode(mode os.FileMode) os.FileMode {
	if mode.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func writeZipTree(root, archivePath string) error {
	file, err := os.Create(archivePath) // #nosec G304 -- 输出路径由调用方给出
	if err != nil {
		return err
	}
	writer := zip.NewWriter(file)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &zip.FileHeader{Name: filepath.ToSlash(rel), Method: zip.Deflate, Modified: archiveFixedTime}
		// 可执行位必须进归档：Linux/macOS 解压后 basalt 才能直接运行；
		// 非可执行文件统一 0644，保证同源码打包结果确定。
		mode := canonicalMode(info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			mode = 0o755
		}
		header.SetMode(mode)
		out, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path) // #nosec G304,G122 -- 打包输入来自本工具创建的暂存目录
		if err != nil {
			return err
		}
		_, err = out.Write(body)
		return err
	})
	if err != nil {
		_ = writer.Close()
		_ = file.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeTarGzTree(root, archivePath string) error {
	file, err := os.Create(archivePath) // #nosec G304 -- 输出路径由调用方给出
	if err != nil {
		return err
	}
	gzipWriter := gzip.NewWriter(file)
	gzipWriter.Header.ModTime = archiveFixedTime
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &tar.Header{
			Name:    filepath.ToSlash(rel),
			Mode:    int64(canonicalMode(info.Mode())),
			ModTime: archiveFixedTime,
			Format:  tar.FormatUSTAR,
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Typeflag = tar.TypeSymlink
			header.Linkname = link
			header.Mode = 0o755
		} else {
			header.Typeflag = tar.TypeReg
			header.Size = info.Size()
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg {
			// #nosec G304,G122 -- 打包输入来自本工具创建的暂存目录
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tarWriter, in)
			closeErr := in.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	})
	for _, closer := range []func() error{tarWriter.Close, gzipWriter.Close, file.Close} {
		if err := closer(); err != nil {
			return err
		}
	}
	return err
}

// VerifyPackageStatic 解包并逐文件核对 MANIFEST.json、版本与内容白名单。
// VerifyPackageStatic extracts the archive and checks its MANIFEST.json, version and
// content allowlist entry by entry.
func VerifyPackageStatic(archive string, in Inputs) error {
	_, err := verifyPackageStatic(archive, in, nil)
	return err
}

// VerifyPackageForTarget 在静态校验之外再核对包内目标与调用方期望的目标一致。
// 逐文件哈希只证明"文件没被改过"；包自述的目标也可能与归档文件名不符——把
// arm64 归档改名成 amd64 上传，只做静态校验是拦不住的。
// VerifyPackageForTarget additionally requires the packaged target to equal the target
// the caller expects, so renaming an archive cannot re-label its platform.
func VerifyPackageForTarget(archive string, in Inputs, want Target) error {
	_, err := verifyPackageStatic(archive, in, &want)
	return err
}

func verifyPackageStatic(archive string, in Inputs, want *Target) (packageManifest, error) {
	dir, err := os.MkdirTemp("", "basalt-release-verify-")
	if err != nil {
		return packageManifest{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := extractArchive(archive, dir); err != nil {
		return packageManifest{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return packageManifest{}, err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return packageManifest{}, errors.New("归档必须只有一个顶层目录")
	}
	root := filepath.Join(dir, entries[0].Name())
	if entries[0].Name() != "basalt-"+in.Version {
		return packageManifest{}, fmt.Errorf("归档顶层目录 %q 与版本 %s 不一致", entries[0].Name(), in.Version)
	}
	body, err := os.ReadFile(filepath.Join(root, packageManifestName)) // #nosec G304 -- 解包目录
	if err != nil {
		return packageManifest{}, fmt.Errorf("读取包内 %s：%w", packageManifestName, err)
	}
	var manifest packageManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return packageManifest{}, fmt.Errorf("解析包内 %s：%w", packageManifestName, err)
	}
	if manifest.Version != in.Version {
		return packageManifest{}, fmt.Errorf("包内版本 %q 与期望 %q 不一致", manifest.Version, in.Version)
	}
	if !manifest.Target.Valid() {
		return packageManifest{}, fmt.Errorf("包内目标 %s 非法", manifest.Target.String())
	}
	if want != nil && manifest.Target != *want {
		return packageManifest{}, fmt.Errorf("包内目标 %s 与期望目标 %s 不一致",
			manifest.Target.String(), want.String())
	}
	actual, err := collectPackageFiles(root)
	if err != nil {
		return packageManifest{}, err
	}
	if len(actual) != len(manifest.Files) {
		return packageManifest{}, fmt.Errorf("包内文件数 %d 与清单 %d 不一致", len(actual), len(manifest.Files))
	}
	byPath := make(map[string]packageFile, len(actual))
	for _, file := range actual {
		byPath[file.Path] = file
	}
	for _, want := range manifest.Files {
		got, ok := byPath[want.Path]
		if !ok {
			return packageManifest{}, fmt.Errorf("清单文件 %s 不在归档内", want.Path)
		}
		if got.SHA256 != want.SHA256 || got.Size != want.Size {
			return packageManifest{}, fmt.Errorf("归档文件 %s 与清单不一致", want.Path)
		}
		// NTFS 无法表达 Unix 可执行位，Windows 宿主解包后 mode 必然与清单不符；
		// 发布归档在 Linux/macOS 上校验时才判权限位。
		if runtime.GOOS != "windows" && got.Mode != want.Mode {
			return packageManifest{}, fmt.Errorf("归档文件 %s 权限 %s 与清单 %s 不一致", want.Path, got.Mode, want.Mode)
		}
	}
	// 逐文件哈希只能证明"文件没被改过"，不能证明它是给这个平台构建的：
	// 再读一次归档内二进制的 buildinfo，核对 GOOS / GOARCH / CGO_ENABLED 与 commit。
	info, err := readBuildInfo(filepath.Join(root, manifest.Target.BinaryName()))
	if err != nil {
		return packageManifest{}, err
	}
	if err := checkBuildInfo(info, manifest.Target, in.Commit); err != nil {
		return packageManifest{}, err
	}
	return manifest, CheckPackageContent(root)
}

func extractArchive(archive, dest string) error {
	if strings.HasSuffix(archive, ".zip") {
		return extractZip(archive, dest)
	}
	return extractTarGz(archive, dest)
}

func secureJoin(dest, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) || clean == ".." {
		return "", fmt.Errorf("归档路径越界：%s", name)
	}
	target := filepath.Join(dest, clean)
	rel, err := filepath.Rel(dest, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("归档路径越界：%s", name)
	}
	return target, nil
}

func extractZip(archive, dest string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	for _, file := range reader.File {
		target, err := secureJoin(dest, file.Name)
		if err != nil {
			return err
		}
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil { // #nosec G301 -- 解包目录
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { // #nosec G301 -- 解包目录
			return err
		}
		in, err := file.Open()
		if err != nil {
			return err
		}
		// 保留归档内的可执行位：Linux/macOS 解包后必须能直接运行 basalt。
		mode := file.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) // #nosec G304,G302 -- target 已做越界校验；解包文件必须对运行者可读
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in) // #nosec G110 -- 归档由本工具打包，条目大小受发布内容约束
		_ = in.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func extractTarGz(archive, dest string) error {
	file, err := os.Open(archive) // #nosec G304 -- 路径由调用方给出
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer func() { _ = gzipReader.Close() }()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := secureJoin(dest, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil { // #nosec G301 -- 解包目录
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { // #nosec G301 -- 解包目录需要可读
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil { // #nosec G122 -- 归档由本工具生成
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { // #nosec G301 -- 解包目录需要可读
				return err
			}
			// 保留归档内的可执行位：Linux/macOS 解包后必须能直接运行 basalt。
			mode := os.FileMode(header.Mode).Perm() // #nosec G115 -- 归档由本工具生成，权限位不超过 0777
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) // #nosec G304,G302 -- target 已做越界校验；解包文件必须对运行者可读
			if err != nil {
				return err
			}
			// #nosec G110 -- 归档由本工具打包，条目大小受发布内容约束
			if _, err := io.Copy(out, reader); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}

// LoadPackages 加载四平台归档并逐项静态验证。
// LoadPackages loads the four platform archives and verifies each one statically.
func LoadPackages(artifactDir string, in Inputs) ([]PackageResult, error) {
	var packages []PackageResult
	for _, target := range DefaultTargets() {
		path := filepath.Join(artifactDir, target.ArchiveName(in.Version))
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("缺少 %s 归档：%w", target.String(), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s 不是普通文件", path)
		}
		if err := VerifyPackageForTarget(path, in, target); err != nil {
			return nil, fmt.Errorf("%s 静态验证失败：%w", target.String(), err)
		}
		sum, err := FileSHA256(path)
		if err != nil {
			return nil, err
		}
		packages = append(packages, PackageResult{
			Name:    filepath.Base(path),
			Archive: path,
			Target:  target,
			SHA256:  sum,
		})
	}
	return packages, nil
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src) // #nosec G304 -- 打包输入
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { // #nosec G301 -- 包内目录
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) // #nosec G302,G304 -- 包内文件需要可读
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func copyTree(srcRoot, dstRoot string) error {
	return filepath.WalkDir(srcRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dstRoot, rel)
		if rel == "." {
			return os.MkdirAll(dstRoot, info.Mode().Perm())
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { // #nosec G301 -- 复制 runtime 目录
				return err
			}
			return os.Symlink(link, target) // #nosec G122 -- runtime 目录由维护者预置
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
