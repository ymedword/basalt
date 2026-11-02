package releasekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 镜像发布工件的固定文件名与平台口径。本 US 只交付 linux/amd64 的 binary 档镜像；
// 其他平台或 full 档留待后续 US 单独评估。
// Fixed artifact names and the platform policy of image releases. This US ships the
// linux/amd64 binary-tier image only; other platforms and the full tier need their
// own evaluation.
const (
	imageRecordName     = "image-record.json"
	imageSBOMName       = "image-sbom.cdx.json"
	imageProvenanceName = "image-provenance.json"
	imageArtifactName   = "basalt"
	imagePlatform       = "linux/amd64"
	imageBinaryPath     = "/opt/basalt/basalt"
	kindImage           = "image"

	scanNotRun = "not-run"
	scanPassed = "passed"
	scanFailed = "failed"
)

// imageScanReason 是未扫描时的如实说明：本机与 CI 都没有镜像扫描器与漏洞库。
// imageScanReason records why the image was not scanned: neither the local host nor
// CI carries an image scanner with a vulnerability database.
const imageScanReason = "本机与 CI 未安装镜像扫描器与漏洞库，本 US 范围内不执行扫描，未得出漏洞结论"

// ImageInputs 是一次镜像构建的输入。
// ImageInputs carries the inputs of one image build.
type ImageInputs struct {
	Repo       string
	Version    string
	Target     Target
	Binary     string
	BaseRef    string
	Dockerfile string
	Entrypoint string
	ConfigFile string
	License    string
	OutDir     string
	RuntimeDir string // 空 = binary 档；非空必须通过 US-17 full 档完整性检查
}

// ImageRecord 是镜像的权威记录，进 release-record.json 的 images[]。
// ImageRecord is the authoritative image record stored in release-record.json.
type ImageRecord struct {
	Name             string     `json:"name"`
	Version          string     `json:"version"`
	Platform         string     `json:"platform"`
	Tier             Tier       `json:"tier"`
	Tag              string     `json:"tag"`
	LocalID          string     `json:"local_id"`
	RepoDigest       string     `json:"repo_digest,omitempty"`
	BaseRef          string     `json:"base_ref"`
	BaseDigest       string     `json:"base_digest"`
	BinarySHA256     string     `json:"binary_sha256"`
	DockerfileSHA256 string     `json:"dockerfile_sha256"`
	EntrypointSHA256 string     `json:"entrypoint_sha256"`
	ConfigSHA256     string     `json:"config_sha256"`
	SBOMPath         string     `json:"sbom_path"`
	SBOMSHA256       string     `json:"sbom_sha256"`
	ProvenancePath   string     `json:"provenance_path"`
	ProvenanceSHA256 string     `json:"provenance_sha256"`
	Scan             ScanRecord `json:"scan"`
	Evidence         []string   `json:"evidence,omitempty"`
}

// ScanRecord 记录镜像漏洞扫描状态；没扫描就写 not-run 与原因，不编造结论。
// ScanRecord records the vulnerability scan state; an unscanned image says not-run
// with a reason instead of pretending to have a result.
type ScanRecord struct {
	Status  string `json:"status"` // not-run | passed | failed
	Scanner string `json:"scanner,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Validate 校验扫描状态三档与其必填字段。
// Validate enforces the three scan states and their mandatory fields.
func (s ScanRecord) Validate() error {
	switch s.Status {
	case scanNotRun:
		if strings.TrimSpace(s.Reason) == "" {
			return errors.New("scan.status=not-run 时必须写原因")
		}
	case scanPassed, scanFailed:
		if strings.TrimSpace(s.Scanner) == "" {
			return fmt.Errorf("scan.status=%s 时必须写扫描器", s.Status)
		}
	default:
		return fmt.Errorf("scan.status %q 不在 not-run / passed / failed 内", s.Status)
	}
	return nil
}

// VerifyImageOptions 控制镜像校验的严格程度。
// VerifyImageOptions controls image verification strictness.
type VerifyImageOptions struct {
	RequireLocalImage bool
	RequireRepoDigest bool
}

// OSPackage 是镜像内 apk 包的一条 SBOM 组件；许可证未知写 ["NOASSERTION"]，不猜。
// OSPackage is one apk component of the image SBOM; unknown licenses are recorded as
// ["NOASSERTION"] rather than guessed.
type OSPackage struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Licenses []string `json:"licenses,omitempty"`
}

// imageProvenance 是镜像的自定义 schema v1 来源记录（不是 SLSA）。
// imageProvenance is the schema-v1 provenance record of an image (not SLSA).
type imageProvenance struct {
	Schema           int       `json:"schema"`
	Version          string    `json:"version"`
	Platform         string    `json:"platform"`
	Tier             Tier      `json:"tier"`
	Tag              string    `json:"tag"`
	LocalID          string    `json:"local_id"`
	BaseRef          string    `json:"base_ref"`
	BaseDigest       string    `json:"base_digest"`
	BinarySHA256     string    `json:"binary_sha256"`
	DockerfileSHA256 string    `json:"dockerfile_sha256"`
	EntrypointSHA256 string    `json:"entrypoint_sha256"`
	ConfigSHA256     string    `json:"config_sha256"`
	Builder          string    `json:"builder"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
}

// BuildImage 构建 linux/amd64 binary 档镜像，落盘后立即在镜像内核对版本与二进制
// 哈希，再写出 SBOM、provenance 与记录。任一步失败都删除本地镜像，避免留下没有
// 记录能解释的镜像 tag。
// BuildImage builds the linux/amd64 binary-tier image, verifies the in-image version
// and binary hash right after the build, then writes the SBOM, provenance and record.
// Any failure removes the local image so no tag outlives its record.
func BuildImage(ctx context.Context, in ImageInputs) (ImageRecord, error) {
	if err := in.validate(); err != nil {
		return ImageRecord{}, err
	}
	binarySHA, err := FileSHA256(in.Binary)
	if err != nil {
		return ImageRecord{}, fmt.Errorf("读取二进制哈希：%w", err)
	}
	built, err := CheckBinaryTarget(in.Binary)
	if err != nil {
		return ImageRecord{}, err
	}
	if built.GOOS != in.Target.GOOS || built.GOARCH != in.Target.GOARCH {
		return ImageRecord{}, fmt.Errorf("二进制构建目标是 %s，与声明的镜像平台 %s 不一致",
			built.String(), in.Target.String())
	}
	if in.RuntimeDir != "" {
		if err := checkRuntimeComponents(in.Repo, in.RuntimeDir, in.Target); err != nil {
			return ImageRecord{}, err
		}
	}
	_, baseDigest, err := parseBaseRef(in.BaseRef)
	if err != nil {
		return ImageRecord{}, err
	}
	if err := os.MkdirAll(in.OutDir, 0o755); err != nil { // #nosec G301 -- 发布产物目录
		return ImageRecord{}, fmt.Errorf("创建镜像产物目录：%w", err)
	}

	contextDir, err := os.MkdirTemp(in.OutDir, ".image-context-")
	if err != nil {
		return ImageRecord{}, fmt.Errorf("创建镜像构建上下文：%w", err)
	}
	defer func() { _ = os.RemoveAll(contextDir) }()
	staged := []struct {
		src  string
		name string
		perm fs.FileMode
	}{
		{in.Binary, imageArtifactName, 0o755},
		{in.Dockerfile, "Dockerfile", 0o644},
		{in.Entrypoint, "entrypoint.sh", 0o755},
		{in.ConfigFile, "basalt.yaml", 0o644},
		{in.License, "LICENSE", 0o644},
	}
	for _, item := range staged {
		if err := copyFile(item.src, filepath.Join(contextDir, item.name), item.perm); err != nil {
			return ImageRecord{}, fmt.Errorf("暂存 %s：%w", item.name, err)
		}
	}
	if err := CheckImageContext(contextDir); err != nil {
		return ImageRecord{}, err
	}

	started := time.Now().UTC()
	record := ImageRecord{
		Name:             imageArtifactName,
		Version:          in.Version,
		Platform:         in.Target.String(),
		Tier:             in.Target.Tier,
		Tag:              imageArtifactName + ":" + in.Version + "-" + in.Target.GOOS + "-" + in.Target.GOARCH,
		BaseRef:          in.BaseRef,
		BaseDigest:       baseDigest,
		BinarySHA256:     binarySHA,
		DockerfileSHA256: "",
		EntrypointSHA256: "",
		ConfigSHA256:     "",
		Scan:             ScanRecord{Status: scanNotRun, Reason: imageScanReason},
	}
	for _, item := range []struct {
		path  string
		field *string
	}{
		{in.Dockerfile, &record.DockerfileSHA256},
		{in.Entrypoint, &record.EntrypointSHA256},
		{in.ConfigFile, &record.ConfigSHA256},
	} {
		sum, err := FileSHA256(item.path)
		if err != nil {
			return ImageRecord{}, fmt.Errorf("读取模板哈希：%w", err)
		}
		*item.field = sum
	}

	iidFile, err := os.CreateTemp(in.OutDir, ".image-iid-")
	if err != nil {
		return ImageRecord{}, fmt.Errorf("创建 iidfile：%w", err)
	}
	iidPath := iidFile.Name()
	if err := iidFile.Close(); err != nil {
		return ImageRecord{}, err
	}
	defer func() { _ = os.Remove(iidPath) }()

	if _, err := dockerOutput(ctx, "build",
		"--platform", imagePlatform,
		"--build-arg", "BASE_IMAGE="+in.BaseRef,
		"--iidfile", iidPath,
		"-t", record.Tag,
		"-f", filepath.Join(contextDir, "Dockerfile"),
		contextDir,
	); err != nil {
		return ImageRecord{}, err
	}
	iidBody, err := os.ReadFile(iidPath) // #nosec G304 -- 本函数创建的临时文件
	if err != nil {
		return ImageRecord{}, fmt.Errorf("读取 iidfile：%w", err)
	}
	record.LocalID = strings.TrimSpace(string(iidBody))
	if err := checkImageIdentity(record.LocalID); err != nil {
		removeLocalImage(ctx, record.Tag)
		return ImageRecord{}, fmt.Errorf("docker build 返回的镜像 ID 不可用：%w", err)
	}

	cleanup := func() {
		removeLocalImage(ctx, record.Tag)
		for _, name := range []string{imageSBOMName, imageProvenanceName} {
			_ = os.Remove(filepath.Join(in.OutDir, name))
		}
	}
	if err := verifyImageContents(ctx, record.Tag, in.Version, binarySHA); err != nil {
		cleanup()
		return ImageRecord{}, err
	}
	osPackages, err := imageOSPackages(ctx, record.Tag)
	if err != nil {
		cleanup()
		return ImageRecord{}, err
	}
	goComponents, gaps, err := collectSBOMComponents(in.Repo, TierBinary)
	if err != nil {
		cleanup()
		return ImageRecord{}, err
	}
	sbom, err := buildImageSBOM(record, goComponents, gaps, osPackages)
	if err != nil {
		cleanup()
		return ImageRecord{}, err
	}
	sbomPath := filepath.Join(in.OutDir, imageSBOMName)
	if err := os.WriteFile(sbomPath, sbom, 0o644); err != nil { // #nosec G306 -- 发布产物对下载者可读
		cleanup()
		return ImageRecord{}, fmt.Errorf("写镜像 SBOM：%w", err)
	}
	record.SBOMPath = imageSBOMName
	record.SBOMSHA256 = sha256Hex(sbom)

	finished := time.Now().UTC()
	provenanceBody, err := BuildImageProvenance(in, record, started, finished)
	if err != nil {
		cleanup()
		return ImageRecord{}, err
	}
	if err := os.WriteFile(filepath.Join(in.OutDir, imageProvenanceName), provenanceBody, 0o644); err != nil { // #nosec G306 -- 发布产物对下载者可读
		cleanup()
		return ImageRecord{}, fmt.Errorf("写镜像 provenance：%w", err)
	}
	record.ProvenancePath = imageProvenanceName
	record.ProvenanceSHA256 = sha256Hex(provenanceBody)
	if err := record.Validate(); err != nil {
		cleanup()
		return ImageRecord{}, err
	}
	return record, nil
}

func (in ImageInputs) validate() error {
	if in.Repo == "" {
		return errors.New("镜像构建缺少 Repo（用于收集 SBOM 组件）")
	}
	if err := ValidateVersion(in.Version); err != nil {
		return err
	}
	if in.Target.GOOS != "linux" || in.Target.GOARCH != "amd64" || in.Target.Tier != TierBinary {
		return fmt.Errorf("本 US 只构建 %s 的 %s 档镜像，got %s/%s %s",
			imagePlatform, TierBinary, in.Target.GOOS, in.Target.GOARCH, in.Target.Tier)
	}
	for _, item := range []struct {
		label string
		path  string
	}{
		{"Binary", in.Binary},
		{"Dockerfile", in.Dockerfile},
		{"Entrypoint", in.Entrypoint},
		{"ConfigFile", in.ConfigFile},
		{"License", in.License},
		{"OutDir", in.OutDir},
	} {
		if strings.TrimSpace(item.path) == "" {
			return fmt.Errorf("镜像构建缺少 %s", item.label)
		}
	}
	for _, item := range []struct {
		label string
		path  string
	}{
		{"二进制", in.Binary},
		{"Dockerfile", in.Dockerfile},
		{"entrypoint.sh", in.Entrypoint},
		{"basalt.yaml", in.ConfigFile},
		{"LICENSE", in.License},
	} {
		info, err := os.Stat(item.path)
		if err != nil {
			return fmt.Errorf("读取%s %s：%w", item.label, item.path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s %s 不是普通文件", item.label, item.path)
		}
	}
	return nil
}

// Validate 校验镜像记录的必填字段、平台口径与哈希自洽；不访问 docker。
// Validate checks required fields, platform policy and hash consistency without
// touching docker.
func (r ImageRecord) Validate() error {
	if r.Name == "" {
		return errors.New("镜像记录缺少 name")
	}
	if err := ValidateVersion(r.Version); err != nil {
		return fmt.Errorf("镜像记录 %s：%w", r.Name, err)
	}
	if r.Platform != imagePlatform {
		return fmt.Errorf("镜像记录平台 %q 不受支持：本 US 只接受 %s", r.Platform, imagePlatform)
	}
	if r.Tier != TierBinary {
		return fmt.Errorf("镜像记录档位 %q 不受支持：本 US 只接受 %s", r.Tier, TierBinary)
	}
	goos, goarch, err := splitPlatform(r.Platform)
	if err != nil {
		return err
	}
	wantTag := r.Name + ":" + r.Version + "-" + goos + "-" + goarch
	if r.Tag != wantTag {
		return fmt.Errorf("镜像 tag %q 与 name/version/platform 推导的 %q 不一致", r.Tag, wantTag)
	}
	if err := checkImageIdentity(r.LocalID); err != nil {
		return fmt.Errorf("local_id：%w", err)
	}
	if r.RepoDigest != "" {
		if err := checkImageIdentity(r.RepoDigest); err != nil {
			return fmt.Errorf("repo_digest：%w", err)
		}
	}
	_, digest, err := parseBaseRef(r.BaseRef)
	if err != nil {
		return err
	}
	if r.BaseDigest != digest {
		return fmt.Errorf("base_digest %q 与 base_ref 中的 digest %q 不一致", r.BaseDigest, digest)
	}
	for _, item := range []struct {
		label string
		sum   string
	}{
		{"binary_sha256", r.BinarySHA256},
		{"dockerfile_sha256", r.DockerfileSHA256},
		{"entrypoint_sha256", r.EntrypointSHA256},
		{"config_sha256", r.ConfigSHA256},
		{"sbom_sha256", r.SBOMSHA256},
		{"provenance_sha256", r.ProvenanceSHA256},
	} {
		if !isHex64(item.sum) {
			return fmt.Errorf("镜像记录 %s 不是 64 位十六进制摘要：%q", item.label, item.sum)
		}
	}
	if err := checkRecordFileName(r.SBOMPath); err != nil {
		return fmt.Errorf("sbom_path：%w", err)
	}
	if err := checkRecordFileName(r.ProvenancePath); err != nil {
		return fmt.Errorf("provenance_path：%w", err)
	}
	if err := r.Scan.Validate(); err != nil {
		return err
	}
	return nil
}

// VerifyImage 校验记录本身，并在要求时核对本地镜像 tag 指向记录的 local_id。
// VerifyImage validates the record and, when requested, that the local tag points at
// the recorded image ID.
func VerifyImage(ctx context.Context, record ImageRecord, opts VerifyImageOptions) (VerifyReport, error) {
	if err := record.Validate(); err != nil {
		return VerifyReport{}, err
	}
	if opts.RequireRepoDigest && record.RepoDigest == "" {
		return VerifyReport{}, fmt.Errorf("镜像 %s 缺少 repo_digest：正式记录要求镜像已推送", record.Tag)
	}
	if opts.RequireLocalImage {
		out, err := dockerOutput(ctx, "image", "inspect", "--format", "{{.Id}}", record.Tag)
		if err != nil {
			return VerifyReport{}, err
		}
		if got := strings.TrimSpace(out); got != record.LocalID {
			return VerifyReport{}, fmt.Errorf("本地镜像 %s 指向 %s，记录是 %s", record.Tag, got, record.LocalID)
		}
	}
	return VerifyReport{}, nil
}

// BuildImageSBOM 从镜像记录与 apk 包生成 CycloneDX 1.5 SBOM。Go 模块组件需要仓库
// 路径，只能由 BuildImage 在构建时并入；本函数供只持有记录与包清单的调用方使用。
// BuildImageSBOM renders the CycloneDX 1.5 image SBOM from the record and apk
// packages. Go module components need the repository path, so only BuildImage can
// merge them at build time; this function serves callers holding a record and a
// package list.
func BuildImageSBOM(record ImageRecord, osPackages []OSPackage) ([]byte, error) {
	return buildImageSBOM(record, nil, nil, osPackages)
}

func buildImageSBOM(record ImageRecord, goComponents []sbomComponent, gaps []string, osPackages []OSPackage) ([]byte, error) {
	if record.Name == "" || record.Version == "" {
		return nil, errors.New("镜像 SBOM 需要记录里的 name 与 version")
	}
	components := make([]sbomComponent, 0, len(goComponents)+len(osPackages))
	components = append(components, goComponents...)
	for _, pkg := range osPackages {
		name := strings.TrimSpace(pkg.Name)
		version := strings.TrimSpace(pkg.Version)
		if name == "" || version == "" {
			return nil, fmt.Errorf("apk 包条目缺少 name 或 version：%+v", pkg)
		}
		licenses := pkg.Licenses
		if len(licenses) == 0 {
			licenses = []string{"NOASSERTION"}
		}
		entries := make([]sbomLicense, 0, len(licenses))
		for _, license := range licenses {
			entries = append(entries, sbomLicense{License: sbomLicenseName{Name: license}})
		}
		components = append(components, sbomComponent{
			Type: "library", Name: name, Version: version, Scope: "required",
			PURL: "pkg:apk/" + name + "@" + version, Licenses: entries,
		})
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].Name == components[j].Name {
			return components[i].Version < components[j].Version
		}
		return components[i].Name < components[j].Name
	})
	document := sbomDocument{
		BOMFormat:   "CycloneDX",
		SpecVersion: "1.5",
		Version:     1,
		Metadata: sbomMetadata{Component: sbomComponent{
			Type: "container", Name: record.Name, Version: record.Version,
			PURL: "pkg:oci/" + record.Name + "@" + record.Version + "?repository_url=" + record.Name,
		}},
		Components: components,
		Gaps:       gaps,
	}
	if document.Gaps == nil {
		document.Gaps = []string{}
	}
	return MarshalIndentJSON(document)
}

// BuildImageProvenance 生成镜像的自定义 schema v1 来源记录；时间由调用方给出，
// 便于测试与复现。
// BuildImageProvenance renders the schema-v1 provenance of an image; the caller
// supplies the timestamps so tests stay deterministic.
func BuildImageProvenance(in ImageInputs, record ImageRecord, started, finished time.Time) ([]byte, error) {
	if started.IsZero() || finished.IsZero() {
		return nil, errors.New("镜像 provenance 需要构建起止时间")
	}
	if finished.Before(started) {
		return nil, errors.New("镜像 provenance 的完成时间早于开始时间")
	}
	document := imageProvenance{
		Schema: 1, Version: record.Version, Platform: record.Platform, Tier: record.Tier,
		Tag: record.Tag, LocalID: record.LocalID, BaseRef: record.BaseRef,
		BaseDigest: record.BaseDigest, BinarySHA256: record.BinarySHA256,
		DockerfileSHA256: record.DockerfileSHA256, EntrypointSHA256: record.EntrypointSHA256,
		ConfigSHA256: record.ConfigSHA256, Builder: "releasekit/" + releasekitVersion,
		StartedAt: started.UTC(), FinishedAt: finished.UTC(),
	}
	return MarshalIndentJSON(document)
}

// LoadImageRecords 读取 assemble 的 --image-record 输入；逐条校验并核对引用的
// SBOM / provenance 文件存在且哈希一致，同平台重复即拒绝。
// LoadImageRecords reads the --image-record inputs of assemble, validates each record
// and checks that its SBOM / provenance files exist with matching hashes. Duplicate
// platforms are rejected.
func LoadImageRecords(paths []string) ([]ImageRecord, error) {
	records := make([]ImageRecord, 0, len(paths))
	seen := make(map[string]string, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path) // #nosec G304 -- 路径由 assemble 显式传入
		if err != nil {
			return nil, fmt.Errorf("读取镜像记录 %s：%w", path, err)
		}
		var record ImageRecord
		if err := json.Unmarshal(body, &record); err != nil {
			return nil, fmt.Errorf("解析镜像记录 %s：%w", path, err)
		}
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("镜像记录 %s：%w", path, err)
		}
		// 平台重复先判：两条同平台记录的文件可能同名同目录，先查哈希只会得到一个
		// 与真实原因无关的「哈希不符」。
		// The duplicate-platform check runs first: two records for the same platform
		// may share file names in one directory, and checking hashes first would
		// report an unrelated mismatch instead of the actual defect.
		if previous, ok := seen[record.Platform]; ok {
			return nil, fmt.Errorf("镜像记录平台 %s 重复：%s 与 %s", record.Platform, previous, path)
		}
		seen[record.Platform] = path
		if err := CheckImageRecordFiles(record, filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("镜像记录 %s：%w", path, err)
		}
		records = append(records, record)
	}
	return records, nil
}

// CheckImageRecordFiles 核对记录引用的 SBOM / provenance 文件存在且哈希一致。
// dir 是记录所在目录；记录里的路径只能是该目录内的相对文件名。
// CheckImageRecordFiles checks that the SBOM / provenance files referenced by the
// record exist with matching hashes. dir is the record's directory and the recorded
// paths must be plain file names inside it.
func CheckImageRecordFiles(record ImageRecord, dir string) error {
	for _, item := range []struct {
		label string
		name  string
		sum   string
	}{
		{"SBOM", record.SBOMPath, record.SBOMSHA256},
		{"provenance", record.ProvenancePath, record.ProvenanceSHA256},
	} {
		if err := checkRecordFileName(item.name); err != nil {
			return fmt.Errorf("%s 路径：%w", item.label, err)
		}
		path, err := secureJoin(dir, item.name)
		if err != nil {
			return fmt.Errorf("%s 路径：%w", item.label, err)
		}
		got, err := FileSHA256(path)
		if err != nil {
			return fmt.Errorf("读取 %s %s：%w", item.label, item.name, err)
		}
		if got != item.sum {
			return fmt.Errorf("%s %s 哈希不符：记录 %s，实际 %s", item.label, item.name, item.sum, got)
		}
	}
	return nil
}

// CheckImageContext 是 BuildImage 调用 docker 之前的守卫：文件名命中私钥 / secret /
// 私有路径，或内容含 BEGIN PRIVATE KEY，即拒绝并点名文件。
// CheckImageContext guards the build context before docker runs: a private-key or
// secret-looking name, a private path, or private-key content rejects the context
// and names the offending file.
func CheckImageContext(dir string) error {
	// 用 os.Root 做 root-scoped 打开：WalkDir 给出的 path 在走查与打开之间可能
	// 被换成符号链接（TOCTOU，gosec G122），root.Open(rel) 把打开限制在暂存目录
	// 内，越界即失败。
	// Open through os.Root: a path handed out by WalkDir can be swapped for a symlink
	// between the walk and the open (TOCTOU, gosec G122); root-scoped opens keep the
	// read inside the staging directory.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if err := checkPrivateContextName(rel); err != nil {
				return err
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("构建上下文 %s 是符号链接：镜像上下文不得包含链接", rel)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("构建上下文 %s 不是普通文件", rel)
		}
		if err := checkPrivateContextName(rel); err != nil {
			return err
		}
		file, err := root.Open(rel) // #nosec G304 -- rel 取自 WalkDir，root 已把打开限制在暂存目录内
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		found, err := containsPrivateKeyMarker(file)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("构建上下文文件 %s 含私钥内容（BEGIN PRIVATE KEY）", rel)
		}
		return nil
	})
}

// CheckBinaryTarget 读取二进制的 build info，核对 GOOS / GOARCH / CGO_ENABLED=0。
// 不读版本：main.version 不是 buildinfo 字段，-trimpath 会省略 -ldflags；版本由
// BuildImage 在镜像内跑 `basalt --version` 核对。
// CheckBinaryTarget reads the binary build info and checks GOOS / GOARCH and
// CGO_ENABLED=0. It does not read the version: main.version is not a buildinfo field
// and -trimpath drops -ldflags, so BuildImage verifies the version inside the image.
func CheckBinaryTarget(path string) (Target, error) {
	info, err := readBuildInfo(path)
	if err != nil {
		return Target{}, err
	}
	goos, goarch := info.settings["GOOS"], info.settings["GOARCH"]
	if goos == "" || goarch == "" {
		return Target{}, fmt.Errorf("%s 的 build info 缺少 GOOS / GOARCH", path)
	}
	if got := info.settings["CGO_ENABLED"]; got != "0" {
		return Target{}, fmt.Errorf("%s 的 CGO_ENABLED=%q，镜像构建必须是 0", path, got)
	}
	return Target{GOOS: goos, GOARCH: goarch, Tier: TierBinary}, nil
}

// AppendImageRows 把镜像记录追加进支持矩阵：kind=image，同 (goos, goarch, kind)
// 重复即拒绝。带上 Evidence 的记录形成 tested 行，否则按 scan 原因写 not-tested；
// 每行都要过 CheckSupportRow；任一行失败则整次追加不生效。
// AppendImageRows appends image rows (kind=image) to the support matrix and refuses
// duplicate (goos, goarch, kind) keys. Records carrying Evidence become tested rows;
// others become not-tested with the scan reason. Every row must pass CheckSupportRow
// and one failure leaves the matrix unchanged.
func AppendImageRows(matrix SupportMatrix, images []ImageRecord) (SupportMatrix, error) {
	rows := append([]SupportRow(nil), matrix.Rows...)
	seen := make(map[string]struct{}, len(rows)+len(images))
	for _, row := range rows {
		seen[supportRowKey(row)] = struct{}{}
	}
	for _, image := range images {
		if err := image.Validate(); err != nil {
			return matrix, err
		}
		goos, goarch, err := splitPlatform(image.Platform)
		if err != nil {
			return matrix, err
		}
		row := SupportRow{
			GOOS: goos, GOARCH: goarch, Tier: image.Tier, Kind: kindImage,
			Status: StatusTested, Checks: nonBlank(image.Evidence),
		}
		if len(image.Evidence) == 0 {
			row.Status = StatusNotTested
			row.Reason = strings.TrimSpace(image.Scan.Reason)
		}
		if err := CheckSupportRow(row); err != nil {
			return matrix, fmt.Errorf("镜像行 %s/%s：%w", goos, goarch, err)
		}
		key := supportRowKey(row)
		if _, ok := seen[key]; ok {
			return matrix, fmt.Errorf("支持矩阵已有 %s/%s(%s) 行：镜像行不得重复", goos, goarch, row.Kind)
		}
		seen[key] = struct{}{}
		rows = append(rows, row)
	}
	matrix.Rows = rows
	return matrix, nil
}

// VerifyImages 校验发布目录里的镜像记录：逐条 Validate、核对文件哈希，并要求矩阵
// 镜像行与 images[] 按 (goos, goarch, kind) 一一对应；RequireRepoDigest 时要求
// repo_digest 非空。dir 是发布目录，matrix 由 VerifyRelease 读入后传入。
// VerifyImages validates the image records of a release directory, checks their file
// hashes, enforces a one-to-one match between matrix image rows and images[], and
// requires a non-empty repo_digest when asked. dir is the release directory and the
// matrix is the one VerifyRelease read from it.
func VerifyImages(dir string, record ReleaseRecord, matrix SupportMatrix, opts VerifyImageOptions) ([]VerifyReport, error) {
	// kind 只允许空（归档行）与 image（镜像行）；未知取值会被归档循环与镜像核对
	// 同时跳过，必须在这里拒绝，不能让篡改矩阵静默过关。
	// Only "" and image are valid kinds; unknown values would be skipped by both the
	// archive loop and the image reconciliation, so reject them here.
	for _, row := range matrix.Rows {
		if row.Kind != "" && row.Kind != kindImage {
			return nil, fmt.Errorf("支持矩阵 %s/%s 的 kind %q 不在 \"\" / image 内", row.GOOS, row.GOARCH, row.Kind)
		}
	}
	rowByKey := make(map[string]SupportRow, len(matrix.Rows))
	for _, row := range matrix.Rows {
		if row.Kind != kindImage {
			continue
		}
		if err := CheckSupportRow(row); err != nil {
			return nil, fmt.Errorf("支持矩阵镜像行 %s/%s：%w", row.GOOS, row.GOARCH, err)
		}
		key := supportRowKey(row)
		if _, ok := rowByKey[key]; ok {
			return nil, fmt.Errorf("支持矩阵镜像行重复：%s/%s", row.GOOS, row.GOARCH)
		}
		rowByKey[key] = row
	}
	if len(record.Images) == 0 && len(rowByKey) == 0 {
		return nil, nil
	}
	if len(record.Images) == 0 {
		return nil, fmt.Errorf("支持矩阵有 %d 条镜像行，但发布记录没有 images[]", len(rowByKey))
	}
	if len(rowByKey) == 0 {
		return nil, fmt.Errorf("发布记录有 %d 条镜像，但支持矩阵没有 kind=image 行", len(record.Images))
	}
	reports := make([]VerifyReport, 0, len(record.Images))
	recordKeys := make(map[string]struct{}, len(record.Images))
	for _, image := range record.Images {
		if err := image.Validate(); err != nil {
			return nil, err
		}
		if err := CheckImageRecordFiles(image, dir); err != nil {
			return nil, fmt.Errorf("镜像 %s：%w", image.Tag, err)
		}
		if opts.RequireRepoDigest && image.RepoDigest == "" {
			return nil, fmt.Errorf("镜像 %s 缺少 repo_digest：正式记录要求镜像已推送", image.Tag)
		}
		goos, goarch, err := splitPlatform(image.Platform)
		if err != nil {
			return nil, err
		}
		key := goos + "/" + goarch + "/" + kindImage
		if _, ok := recordKeys[key]; ok {
			return nil, fmt.Errorf("发布记录的镜像平台重复：%s/%s", goos, goarch)
		}
		recordKeys[key] = struct{}{}
		row, ok := rowByKey[key]
		if !ok {
			return nil, fmt.Errorf("镜像 %s（%s/%s）在支持矩阵中没有 kind=image 行", image.Tag, goos, goarch)
		}
		if row.Tier != image.Tier {
			return nil, fmt.Errorf("镜像 %s 档位 %q 与支持矩阵行 %q 不一致", image.Tag, image.Tier, row.Tier)
		}
		reports = append(reports, VerifyReport{VerifiedFiles: []string{image.SBOMPath, image.ProvenancePath}})
	}
	for key, row := range rowByKey {
		if _, ok := recordKeys[key]; !ok {
			return nil, fmt.Errorf("支持矩阵镜像行 %s/%s 在 images[] 中没有对应记录", row.GOOS, row.GOARCH)
		}
	}
	return reports, nil
}

// dockerOutput 运行 docker 并分开收集 stdout / stderr：版本行只能从 stdout 解析，
// stderr 的启动日志不能拼进去。
// dockerOutput runs docker with stdout and stderr kept apart: the version line is
// parsed from stdout only, never concatenated with startup logs on stderr.
func dockerOutput(ctx context.Context, args ...string) (string, error) {
	// #nosec G204 -- 子命令与参数全部由 releasekit 构造，不含用户自由文本
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s 失败：%w（%s）",
			dockerCommandLabel(args), err, tailText(stderr.String(), 2000))
	}
	return stdout.String(), nil
}

func dockerCommandLabel(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) == 1 {
		return args[0]
	}
	return args[0] + " " + args[1]
}

func tailText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return "…" + text[len(text)-limit:]
}

func verifyImageContents(ctx context.Context, tag, version, binarySHA string) error {
	out, err := dockerOutput(ctx, "run", "--rm", "--network", "none",
		"--entrypoint", imageBinaryPath, tag, "--version")
	if err != nil {
		return err
	}
	want := "basalt version " + version
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("镜像内 `basalt --version` 输出 %q，期望包含 %q", strings.TrimSpace(out), want)
	}
	out, err = dockerOutput(ctx, "run", "--rm", "--network", "none",
		"--entrypoint", "/bin/sh", tag, "-c", "sha256sum "+imageBinaryPath)
	if err != nil {
		return err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return fmt.Errorf("镜像内 sha256sum 输出为空")
	}
	if fields[0] != binarySHA {
		return fmt.Errorf("镜像内二进制哈希 %s 与构建输入 %s 不一致", fields[0], binarySHA)
	}
	return nil
}

var apkPackagePattern = regexp.MustCompile(`^(.+)-([0-9][A-Za-z0-9._+~-]*)-r([0-9]+)$`)

// imageOSPackages 从运行中的镜像读取 apk 包清单；版本或包名无法解析时如实标记
// NOASSERTION，不编造版本。
// imageOSPackages lists the apk packages inside the built image. Unparseable names
// or versions are recorded as NOASSERTION instead of inventing values.
func imageOSPackages(ctx context.Context, tag string) ([]OSPackage, error) {
	out, err := dockerOutput(ctx, "run", "--rm", "--network", "none",
		"--entrypoint", "/sbin/apk", tag, "info", "-v")
	if err != nil {
		return nil, err
	}
	return parseAPKPackages(out), nil
}

// parseAPKPackages 解析 `apk info -v` 的逐行输出：`<name>-<version>-r<rev>`。
// 包名本身可以带连字符与数字，因此正则从右往左锚定修订号；解析不出的行如实保留
// 原始行名并把版本记为 NOASSERTION。
// parseAPKPackages parses one `apk info -v` line per package. Names may contain
// hyphens and digits, so the revision anchors from the right; unparseable lines keep
// their raw name with NOASSERTION as the version.
func parseAPKPackages(out string) []OSPackage {
	packages := make([]OSPackage, 0, 16)
	seen := make(map[string]struct{}, 16)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, version := line, "NOASSERTION"
		if matches := apkPackagePattern.FindStringSubmatch(line); matches != nil {
			name, version = matches[1], matches[2]+"-r"+matches[3]
		}
		key := name + "@" + version
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		packages = append(packages, OSPackage{Name: name, Version: version, Licenses: []string{"NOASSERTION"}})
	}
	return packages
}

func removeLocalImage(ctx context.Context, tag string) {
	_, _ = dockerOutput(ctx, "image", "rm", "-f", tag)
}

func checkPrivateContextName(rel string) error {
	segments := strings.Split(strings.ToLower(rel), "/")
	for _, segment := range segments {
		switch {
		case segment == "" || segment == ".":
			continue
		case strings.HasSuffix(segment, ".pem"):
			return fmt.Errorf("构建上下文 %s 命中私钥文件名规则（*.pem）", rel)
		case segment == "secrets" || segment == "career" || segment == "release-keys":
			return fmt.Errorf("构建上下文 %s 命中私有 / secret 路径规则", rel)
		case strings.Contains(segment, "release-keys"):
			return fmt.Errorf("构建上下文 %s 命中 release-keys 规则", rel)
		}
	}
	return nil
}

func containsPrivateKeyMarker(file *os.File) (bool, error) {
	marker := []byte("BEGIN PRIVATE KEY")
	const chunk = 64 * 1024
	buffer := make([]byte, chunk)
	window := make([]byte, 0, chunk+len(marker)-1)
	for {
		n, err := file.Read(buffer)
		if n > 0 {
			window = append(window, buffer[:n]...)
			if bytes.Contains(window, marker) {
				return true, nil
			}
			if keep := len(marker) - 1; len(window) > keep {
				window = window[len(window)-keep:]
			}
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func parseBaseRef(ref string) (string, string, error) {
	name, digest, ok := strings.Cut(ref, "@sha256:")
	if !ok || name == "" || !isHex64(digest) {
		return "", "", fmt.Errorf("基础镜像引用 %q 必须固定 digest（name@sha256:<64 hex>）", ref)
	}
	return name, "sha256:" + digest, nil
}

func checkImageIdentity(value string) error {
	if !strings.HasPrefix(value, "sha256:") || !isHex64(strings.TrimPrefix(value, "sha256:")) {
		return fmt.Errorf("必须是 sha256:<64 hex>，got %q", value)
	}
	return nil
}

func isHex64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9':
		case char >= 'a' && char <= 'f':
		case char >= 'A' && char <= 'F':
		default:
			return false
		}
	}
	return true
}

func splitPlatform(platform string) (string, string, error) {
	parts := strings.Split(platform, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("平台 %q 必须是 goos/goarch", platform)
	}
	return parts[0], parts[1], nil
}

func supportRowKey(row SupportRow) string {
	return row.GOOS + "/" + row.GOARCH + "/" + row.Kind
}

func nonBlank(values []string) []string {
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return kept
}

func checkRecordFileName(name string) error {
	if name == "" {
		return errors.New("路径为空")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." || name != filepath.Base(name) {
		return fmt.Errorf("路径必须是记录所在目录内的文件名：%q", name)
	}
	return nil
}
