package releasekit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type sbomComponent struct {
	Type     string        `json:"type"`
	Group    string        `json:"group,omitempty"`
	Name     string        `json:"name"`
	Version  string        `json:"version"`
	PURL     string        `json:"purl,omitempty"`
	Scope    string        `json:"scope,omitempty"`
	Licenses []sbomLicense `json:"licenses,omitempty"`
}

// sbomLicense 是 CycloneDX 的许可证条目形态；apk 包已知许可证按名写，未知写
// NOASSERTION——不猜许可证。
// sbomLicense is the CycloneDX license entry shape; known apk licenses are written
// by name and unknown ones as NOASSERTION rather than guessed.
type sbomLicense struct {
	License sbomLicenseName `json:"license"`
}

type sbomLicenseName struct {
	Name string `json:"name"`
}

type sbomDocument struct {
	BOMFormat   string          `json:"bomFormat"`
	SpecVersion string          `json:"specVersion"`
	Version     int             `json:"version"`
	Metadata    sbomMetadata    `json:"metadata"`
	Components  []sbomComponent `json:"components"`
	Gaps        []string        `json:"gaps"`
}

type sbomMetadata struct {
	Component sbomComponent `json:"component"`
}

// BuildSBOM 为每个归档生成 CycloneDX 1.5 SBOM；前端或 runtime 清单缺失时写入 gaps。
// BuildSBOM writes a CycloneDX 1.5 SBOM per archive; missing frontend or runtime
// inventories are reported as gaps rather than silently omitted.
func BuildSBOM(in Inputs, pkgs []PackageResult, dir string) error {
	for _, pkg := range pkgs {
		// SBOM 只描述归档里真实存在的内容：binary 档不带 runtime/，也就不列 runtime 组件。
		components, gaps, err := collectSBOMComponents(in.Repo, pkg.Target.Tier)
		if err != nil {
			return err
		}
		document := sbomDocument{
			BOMFormat:   "CycloneDX",
			SpecVersion: "1.5",
			Version:     1,
			Metadata: sbomMetadata{Component: sbomComponent{
				Type: "application", Name: "basalt", Version: in.Version,
				PURL: "pkg:generic/basalt@" + in.Version,
			}},
			Components: components,
			Gaps:       gaps,
		}
		base := "basalt-" + in.Version + "-" + pkg.Target.GOOS + "-" + pkg.Target.GOARCH
		path := filepath.Join(dir, base+".sbom.cdx.json")
		if err := writeJSON(path, document); err != nil {
			return err
		}
	}
	return nil
}

func collectSBOMComponents(repo string, tier Tier) ([]sbomComponent, []string, error) {
	components := []sbomComponent{{Type: "application", Name: "basalt", Version: ""}}
	components = components[:0]
	var gaps []string
	modules, err := parseGoMod(filepath.Join(repo, "go.mod"))
	if err != nil {
		gaps = append(gaps, "go.mod unavailable")
	} else {
		for _, module := range modules {
			components = append(components, sbomComponent{
				Type: "library", Name: module.name, Version: module.version,
				PURL: "pkg:golang/" + module.name + "@" + module.version,
			})
		}
	}
	frontendPath := filepath.Join(repo, "assets", "console", "package.json")
	if body, err := os.ReadFile(frontendPath); err != nil { // #nosec G304 -- 仓库内固定路径
		gaps = append(gaps, "frontend package.json unavailable")
	} else {
		var pkg struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := json.Unmarshal(body, &pkg); err != nil || pkg.Version == "" {
			gaps = append(gaps, "frontend package.json has no version")
		} else {
			name := pkg.Name
			if name == "" {
				name = "basalt-console"
			}
			components = append(components, sbomComponent{
				Type: "application", Name: name, Version: pkg.Version,
				Scope: "optional", PURL: "pkg:npm/" + name + "@" + pkg.Version,
			})
		}
	}
	runtimePath := filepath.Join(repo, "release", "runtime-manifest.json")
	if tier != TierFull {
		// binary 档归档里没有 runtime/，列出 runtime 组件会描述不存在的内容。
	} else if body, err := os.ReadFile(runtimePath); err != nil { // #nosec G304 -- 仓库内固定路径
		gaps = append(gaps, "runtime manifest unavailable")
	} else {
		var manifest struct {
			Components []struct {
				ID      string `json:"id"`
				Version string `json:"version"`
				License string `json:"license"`
				Source  string `json:"source"`
			} `json:"components"`
		}
		if err := json.Unmarshal(body, &manifest); err != nil {
			gaps = append(gaps, "runtime manifest invalid")
		} else {
			for _, component := range manifest.Components {
				components = append(components, sbomComponent{
					Type: "application", Name: component.ID, Version: component.Version,
					Scope: "required", PURL: component.Source,
				})
			}
		}
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].Name == components[j].Name {
			return components[i].Version < components[j].Version
		}
		return components[i].Name < components[j].Name
	})
	sort.Strings(gaps)
	return components, gaps, nil
}

type goModule struct {
	name    string
	version string
}

func parseGoMod(path string) ([]goModule, error) {
	file, err := os.Open(path) // #nosec G304 -- 仓库内固定路径
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var modules []goModule
	inBlock := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "require ("):
			inBlock = true
			continue
		case inBlock && line == ")":
			inBlock = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		case !inBlock:
			continue
		}
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "v") {
			continue
		}
		modules = append(modules, goModule{name: fields[0], version: fields[1]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return modules, nil
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s：%w", filepath.Base(path), err)
	}
	body = append(body, '\n')
	// #nosec G306 -- 发布目录里的 JSON（SBOM / 记录 / 矩阵）必须对下载者可读
	return os.WriteFile(path, body, 0o644)
}
