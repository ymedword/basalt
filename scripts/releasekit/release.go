package releasekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// RunRelease 构建、打包并静态验证一个目标；匹配宿主时再写运行证据。
// RunRelease builds, packages and statically verifies one target, then records host
// evidence when the target matches the current host.
func RunRelease(ctx context.Context, opts ReleaseOptions) error {
	in, err := LoadInputs(opts.Repo, opts.Version, opts.Tag, opts.Mode)
	if err != nil {
		return err
	}
	if err := Preflight(ctx, in, opts); err != nil {
		return err
	}
	build, err := BuildTarget(ctx, in, opts.Target, opts.WorkDir)
	if err != nil {
		return err
	}
	pkg, err := PackageTarget(ctx, in, opts.Target, build, opts)
	if err != nil {
		return err
	}
	if err := VerifyPackageForTarget(pkg.Archive, in, opts.Target); err != nil {
		return err
	}
	if !HostMatches(opts.Target) {
		return nil
	}
	// 双构建比较：同一源码与工具链再构建一次，结论写进证据；不一致时 final 在汇总
	// 阶段被阻止（C-05），rehearsal 只如实记录。
	reproducible, err := compareSecondBuild(ctx, in, opts, build)
	if err != nil {
		return err
	}
	if err := RunSmoke(ctx, pkg, opts); err != nil {
		return err
	}
	if opts.Target.Tier == TierFull {
		if err := RunUpgradeRecovery(ctx, in, pkg, opts); err != nil {
			return err
		}
	}
	return WriteEvidence(opts.EvidenceDir, in, pkg, reproducible)
}

// checkReproducible 是 final 的双构建门禁：没比较过、或比较不相等都不得发布。
// rehearsal 只如实记录，不阻止。
// checkReproducible is the final-mode reproducibility gate: a release must carry a
// comparison, and that comparison must be equal. Rehearsal only records.
func checkReproducible(mode Mode, reproducible Reproducible) error {
	if mode != ModeFinal {
		return nil
	}
	if !reproducible.Checked {
		return errors.New("final 的宿主证据缺少双构建比较（reproducible.checked=false）：" +
			"证据必须由 releasekit release 在对应宿主上产出")
	}
	if !reproducible.Equal {
		return fmt.Errorf("final 的双构建哈希不一致（second_build_sha256=%s），必须阻止发布",
			reproducible.SecondBuildSHA256)
	}
	return nil
}

func compareSecondBuild(ctx context.Context, in Inputs, opts ReleaseOptions, first BuildResult) (*Reproducible, error) {
	dir, err := os.MkdirTemp("", "basalt-repro-build-")
	if err != nil {
		return nil, fmt.Errorf("创建双构建比较目录：%w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	second, err := BuildTarget(ctx, in, opts.Target, dir)
	if err != nil {
		return nil, fmt.Errorf("双构建比较的第二次构建失败：%w", err)
	}
	comparison := CompareBuilds(first, second)
	return &comparison, nil
}

func versionFromPackage(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

// AssembleRelease 收集四平台归档与证据，生成 SBOM、记录、支持矩阵并按顺序签名。
// 输出目录必须为空：陈旧文件会被写进 SHA256SUMS 并被签名；中途失败时清理半成品，
// 避免留下与事实不符的已签名目录。
// AssembleRelease collects the four archives and evidence, writes SBOM, record and
// matrix, and signs in the mandated order. The output directory must be empty, and a
// failed run removes its partial output so no directory claims a signature it lacks.
func AssembleRelease(ctx context.Context, opts AssembleOptions) error {
	if err := prepareReleaseDir(opts.OutDir); err != nil {
		return err
	}
	if err := assembleRelease(ctx, opts); err != nil {
		removeDirEntries(opts.OutDir)
		return err
	}
	return nil
}

func prepareReleaseDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("读取发布目录：%w", err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- 发布目录
			return err
		}
		return nil
	}
	if len(entries) > 0 {
		return fmt.Errorf("发布目录 %s 非空：汇总只写入全新目录，避免陈旧文件被签名", dir)
	}
	return nil
}

func removeDirEntries(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}

func assembleRelease(ctx context.Context, opts AssembleOptions) error {
	in := opts.Inputs
	pkgs, err := LoadPackages(opts.ArtifactDir, in)
	if err != nil {
		return err
	}
	if err := RequireEvidence(in, opts.EvidenceDir); err != nil {
		return err
	}
	reproducible, err := AggregateReproducible(opts.EvidenceDir)
	if err != nil {
		return err
	}
	if err := checkReproducible(in.Mode, reproducible); err != nil {
		return err
	}
	for _, target := range DefaultTargets() {
		src := filepath.Join(opts.EvidenceDir,
			fmt.Sprintf("evidence-%s-%s.json", target.GOOS, target.GOARCH))
		if !fileExists(src) {
			continue
		}
		if err := copyFile(src, filepath.Join(opts.OutDir, filepath.Base(src)), 0o644); err != nil {
			return fmt.Errorf("复制 %s 证据：%w", target.String(), err)
		}
	}
	for index := range pkgs {
		dst := filepath.Join(opts.OutDir, pkgs[index].Name)
		if err := copyFile(pkgs[index].Archive, dst, 0o644); err != nil {
			return fmt.Errorf("复制归档 %s：%w", pkgs[index].Name, err)
		}
		pkgs[index].Archive = dst
	}
	// 镜像记录在写校验和之前读入、校验并复制 SBOM / provenance；记录与矩阵行
	// 由本函数直接写入，不提供“汇总后再补字段”的第二条路径。
	images, err := loadAndCopyImages(opts, pkgs)
	if err != nil {
		return err
	}
	for _, target := range DefaultTargets() {
		if _, err := BuildVerifier(ctx, in.Repo, in.Version, target, opts.OutDir); err != nil {
			return err
		}
	}
	if err := BuildSBOM(in, pkgs, opts.OutDir); err != nil {
		return err
	}
	if err := BuildProvenance(in, pkgs, opts.OutDir); err != nil {
		return err
	}
	matrix, err := BuildSupportMatrix(in, opts.EvidenceDir)
	if err != nil {
		return err
	}
	if len(images) > 0 {
		matrix, err = AppendImageRows(matrix, images)
		if err != nil {
			return err
		}
	}
	manifest, err := BuildReleaseManifest(in, pkgs)
	if err != nil {
		return err
	}
	manifest.Images = images
	if err := CheckNotes(opts.NotesPath, in.Version); err != nil {
		return err
	}
	notesCopy := filepath.Join(opts.OutDir, "notes.md")
	if err := copyFile(opts.NotesPath, notesCopy, 0o644); err != nil {
		return fmt.Errorf("复制发布说明：%w", err)
	}
	notesSum, err := FileSHA256(notesCopy)
	if err != nil {
		return err
	}
	matrixPath := filepath.Join(opts.OutDir, "support-matrix.json")
	if err := writeJSON(matrixPath, matrix); err != nil {
		return err
	}
	matrixSum, err := FileSHA256(matrixPath)
	if err != nil {
		return err
	}
	record, err := BuildReleaseRecord(in, pkgs, matrix, manifest, reproducible)
	if err != nil {
		return err
	}
	record.SupportMatrixSHA256 = matrixSum
	record.NotesSHA256 = notesSum
	record.Images = images
	manifest.Artifacts = record.Artifacts
	if err := writeJSON(filepath.Join(opts.OutDir, "release-manifest.json"), manifest); err != nil {
		return err
	}
	if in.Mode == ModeFinal {
		fingerprint, err := Fingerprint(opts.PublicKey)
		if err != nil {
			return fmt.Errorf("读取公钥指纹：%w", err)
		}
		record.Signing = SigningInfo{
			Status: "signed", Algorithm: "ed25519", PublicKeyFingerprint: fingerprint,
			SignedFiles: []string{recordSignature, sumsSignature},
		}
	}
	if err := writeJSON(filepath.Join(opts.OutDir, releaseRecordName), record); err != nil {
		return err
	}
	if in.Mode == ModeFinal {
		if _, err := SignFile(opts.SigningKey, opts.OutDir, releaseRecordName); err != nil {
			return err
		}
	}
	if err := WriteChecksums(opts.OutDir); err != nil {
		return err
	}
	if in.Mode == ModeFinal {
		if _, err := SignFile(opts.SigningKey, opts.OutDir, checksumsName); err != nil {
			return err
		}
	}
	_, err = VerifyRelease(ctx, opts.OutDir, VerifyPolicy{
		PublicKey: opts.PublicKey, TrustFile: opts.TrustFile,
		RequireSignature: in.Mode == ModeFinal,
	})
	return err
}

// loadAndCopyImages 读入镜像记录并把 SBOM / provenance 复制进发布目录；文件名
// 在发布目录内必须唯一，重名即拒绝（不覆盖归档或另一条镜像的工件）。
// loadAndCopyImages loads the image records and copies their SBOM / provenance into
// the release directory; names must stay unique so nothing overwrites an archive or
// another image's files.
func loadAndCopyImages(opts AssembleOptions, pkgs []PackageResult) ([]ImageRecord, error) {
	if len(opts.Images) == 0 {
		return nil, nil
	}
	images, err := LoadImageRecords(opts.Images)
	if err != nil {
		return nil, err
	}
	// 验收证据由发布编排查询后显式给出；记录里已带证据时只允许完全一致，不做静默
	// 覆盖（两条来源不同即是口径冲突，必须让人看见）。
	// Acceptance evidence comes from the release orchestrator; when a record already
	// carries evidence it must match exactly rather than being silently replaced.
	if evidence := nonBlank(opts.ImageEvidence); len(evidence) > 0 {
		for index := range images {
			existing := nonBlank(images[index].Evidence)
			if len(existing) > 0 {
				if !slices.Equal(existing, evidence) {
					return nil, fmt.Errorf("镜像 %s 的记录已带验收证据 %v，与 --image-evidence %v 冲突",
						images[index].Tag, existing, evidence)
				}
				continue
			}
			images[index].Evidence = append([]string(nil), evidence...)
		}
	}
	taken := make(map[string]string, len(pkgs)+2*len(images))
	for _, pkg := range pkgs {
		taken[pkg.Name] = "归档"
	}
	for index, image := range images {
		sourceDir := filepath.Dir(opts.Images[index])
		for _, name := range []string{image.SBOMPath, image.ProvenancePath} {
			if previous, ok := taken[name]; ok {
				return nil, fmt.Errorf("镜像文件 %s 与%s重名，发布目录里无法并存", name, previous)
			}
			taken[name] = "镜像 " + image.Tag
			if err := copyFile(filepath.Join(sourceDir, name), filepath.Join(opts.OutDir, name), 0o644); err != nil {
				return nil, fmt.Errorf("复制镜像文件 %s：%w", name, err)
			}
		}
	}
	return images, nil
}

// MarshalIndentJSON 供测试与 CLI 输出稳定 JSON。
// MarshalIndentJSON renders stable JSON for tests and CLI output.
func MarshalIndentJSON(value any) ([]byte, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}
