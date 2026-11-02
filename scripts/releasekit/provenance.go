package releasekit

import (
	"fmt"
	"path/filepath"
)

type provenance struct {
	Schema        int    `json:"schema"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	Tier          Tier   `json:"tier"`
	ArchiveSHA256 string `json:"archive_sha256"`
	Builder       string `json:"builder"`
}

// BuildProvenance 为每个归档写自定义 schema v1 的来源记录；不冒充 SLSA。
// BuildProvenance writes a schema-v1 provenance record per archive; it does not
// claim SLSA compliance.
func BuildProvenance(in Inputs, pkgs []PackageResult, dir string) error {
	for _, pkg := range pkgs {
		document := provenance{
			Schema: 1, Version: in.Version, Commit: in.Commit,
			GOOS: pkg.Target.GOOS, GOARCH: pkg.Target.GOARCH, Tier: pkg.Target.Tier,
			ArchiveSHA256: pkg.SHA256, Builder: "releasekit/" + releasekitVersion,
		}
		base := "basalt-" + in.Version + "-" + pkg.Target.GOOS + "-" + pkg.Target.GOARCH
		if err := writeJSON(filepath.Join(dir, base+".provenance.json"), document); err != nil {
			return fmt.Errorf("写 provenance：%w", err)
		}
	}
	return nil
}
