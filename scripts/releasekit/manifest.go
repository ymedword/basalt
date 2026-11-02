package releasekit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BuildReleaseManifest 生成发布目录的顶层清单。
// BuildReleaseManifest builds the top-level release manifest.
func BuildReleaseManifest(in Inputs, pkgs []PackageResult) (ReleaseManifest, error) {
	manifest := ReleaseManifest{
		Schema:  1,
		Version: in.Version,
		Tag:     in.Tag,
		Mode:    in.Mode,
		Commit:  in.Commit,
	}
	for _, pkg := range pkgs {
		manifest.Artifacts = append(manifest.Artifacts, ArtifactRecord{
			Name:   pkg.Name,
			GOOS:   pkg.Target.GOOS,
			GOARCH: pkg.Target.GOARCH,
			Tier:   pkg.Target.Tier,
			SHA256: pkg.SHA256,
		})
	}
	return manifest, nil
}

// CheckNotes 校验发布说明的六章结构与版本号。
// CheckNotes checks the six required sections and the version in the notes.
func CheckNotes(path, version string) error {
	body, err := os.ReadFile(path) // #nosec G304 -- 说明路径由发布命令显式传入
	if err != nil {
		return fmt.Errorf("读取发布说明：%w", err)
	}
	text := string(body)
	required := []string{"摘要", "变更", "升级步骤", "已知问题", "校验与签名", "回滚"}
	for _, heading := range required {
		if !strings.Contains(text, heading) {
			return fmt.Errorf("发布说明缺少章节：%s", heading)
		}
	}
	if !strings.Contains(text, version) {
		return fmt.Errorf("发布说明未出现版本号 %s", version)
	}
	return nil
}

func artifactRecords(pkgs []PackageResult, outDir string, in Inputs) ([]ArtifactRecord, error) {
	records := make([]ArtifactRecord, 0, len(pkgs))
	for _, pkg := range pkgs {
		base := "basalt-" + in.Version + "-" + pkg.Target.GOOS + "-" + pkg.Target.GOARCH
		sbom := filepath.Join(outDir, base+".sbom.cdx.json")
		provenance := filepath.Join(outDir, base+".provenance.json")
		sbomSum, err := FileSHA256(sbom)
		if err != nil {
			return nil, fmt.Errorf("读取 SBOM %s：%w", filepath.Base(sbom), err)
		}
		provenanceSum, err := FileSHA256(provenance)
		if err != nil {
			return nil, fmt.Errorf("读取 provenance %s：%w", filepath.Base(provenance), err)
		}
		records = append(records, ArtifactRecord{
			Name: pkg.Name, GOOS: pkg.Target.GOOS, GOARCH: pkg.Target.GOARCH,
			Tier: pkg.Target.Tier, SHA256: pkg.SHA256,
			SBOMSHA256: sbomSum, ProvenanceSHA256: provenanceSum,
		})
	}
	return records, nil
}
