package main

// 本文件是运行时组件清单的读取、校验与逐组件核对。清单是"包内应有哪些组件、各自
// 什么版本、许可证是什么、元数据从哪读"的唯一事实源；工具只认清单，不在代码里再
// 写一份版本表——两处版本迟早对不上。
// This file loads and validates the runtime manifest and verifies each component.
// The manifest is the single source of truth; the tool keeps no second version table.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	manifestSchemaVersion = 1

	kindExecutable    = "executable"
	kindRuntime       = "runtime"
	kindNodePackage   = "node-package"
	kindPythonPackage = "python-package"

	scanKindNodeModules = "node_modules"
	scanKindDistInfo    = "dist-info"
)

type manifest struct {
	SchemaVersion int         `json:"schema_version"`
	Components    []component `json:"components"`
}

type component struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	Version         string            `json:"version"`
	License         string            `json:"license"`
	Source          string            `json:"source"`
	Paths           map[string]string `json:"paths"`
	RuntimeLicenses []string          `json:"runtime_licenses"`
	RepoLicenses    []string          `json:"repo_licenses"`
	PackageJSON     string            `json:"package_json"`
	PackageName     string            `json:"package_name"`
	VersionCheck    *versionCheck     `json:"version_check"`
	Scan            *scanSpec         `json:"scan"`
}

type versionCheck struct {
	Args   []string `json:"args"`
	Expect string   `json:"expect"`
}

type scanSpec struct {
	Kind string `json:"kind"`
	Root string `json:"root"`
}

// componentReport 是报告中逐组件的核对结果。
// componentReport records one component's verified readings.
type componentReport struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	License string `json:"license"`
	Source  string `json:"source"`
	File    string `json:"file"`
}

// loadManifest 读取并校验清单；platform 是当前打包平台（windows / linux）。
// loadManifest reads and validates the manifest for the packaging platform.
func loadManifest(path, platform string) (manifest, error) {
	// #nosec G304 -- 清单路径由调用方显式给出（默认仓库内的 release/runtime-manifest.json）
	raw, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("读取运行时清单 %s：%w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var loaded manifest
	if err := decoder.Decode(&loaded); err != nil {
		return manifest{}, fmt.Errorf("解析运行时清单 %s：%w", path, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return manifest{}, fmt.Errorf("运行时清单 %s 在首个对象之后还有多余内容", path)
	}
	if err := validateManifest(loaded, platform); err != nil {
		return manifest{}, fmt.Errorf("运行时清单 %s：%w", path, err)
	}
	return loaded, nil
}

// validateManifest 校验 schema、必填字段、平台路径与许可证来源。
// validateManifest checks the schema, required fields, platform path and license sources.
func validateManifest(loaded manifest, platform string) error {
	if loaded.SchemaVersion != manifestSchemaVersion {
		return fmt.Errorf("schema_version = %d，期望 %d", loaded.SchemaVersion, manifestSchemaVersion)
	}
	if len(loaded.Components) == 0 {
		return errors.New("components 为空——空清单不是合法的发行包")
	}
	seen := make(map[string]bool, len(loaded.Components))
	for index, comp := range loaded.Components {
		where := fmt.Sprintf("components[%d]", index)
		if comp.ID == "" {
			return fmt.Errorf("%s 缺少 id", where)
		}
		where = "组件 " + comp.ID
		if seen[comp.ID] {
			return fmt.Errorf("%s 重复", where)
		}
		seen[comp.ID] = true
		switch comp.Kind {
		case kindExecutable, kindRuntime, kindNodePackage, kindPythonPackage:
		default:
			return fmt.Errorf("%s 的 kind = %q 不在允许集合", where, comp.Kind)
		}
		if comp.Version == "" || comp.License == "" || comp.Source == "" {
			return fmt.Errorf("%s 缺少 version / license / source", where)
		}
		if comp.Paths[platform] == "" {
			return fmt.Errorf("%s 缺少 %s 平台的 path", where, platform)
		}
		if len(comp.RuntimeLicenses) == 0 && len(comp.RepoLicenses) == 0 {
			return fmt.Errorf("%s 没有声明任何许可证文件", where)
		}
		if comp.Kind == kindRuntime && (comp.VersionCheck == nil || comp.VersionCheck.Expect == "") {
			return fmt.Errorf("%s 是运行时组件，必须声明 version_check", where)
		}
		if comp.Kind == kindNodePackage && comp.PackageJSON == "" {
			return fmt.Errorf("%s 是 npm 组件，必须声明 package_json", where)
		}
		if comp.Kind == kindPythonPackage && comp.PackageName == "" {
			return fmt.Errorf("%s 是 Python 组件，必须声明 package_name", where)
		}
		if comp.Scan != nil {
			if comp.Scan.Root == "" {
				return fmt.Errorf("%s 的 scan.root 为空", where)
			}
			if comp.Scan.Kind != scanKindNodeModules && comp.Scan.Kind != scanKindDistInfo {
				return fmt.Errorf("%s 的 scan.kind = %q 不在允许集合", where, comp.Scan.Kind)
			}
		}
	}
	return nil
}

// verifyComponents 逐个核对清单组件：入口存在、版本自报一致、包元数据版本一致。
// verifyComponents checks each component's entry, self-reported version and metadata.
func verifyComponents(loaded manifest, runtimeRoot, platform string) ([]componentReport, error) {
	reports := make([]componentReport, 0, len(loaded.Components))
	for _, comp := range loaded.Components {
		rel := filepath.FromSlash(comp.Paths[platform])
		entry := filepath.Join(runtimeRoot, rel)
		info, err := os.Stat(entry) // #nosec G304 -- 路径由清单与运行时根拼出，只读
		if err != nil {
			return nil, fmt.Errorf("组件 %s 缺少入口 %s：%w", comp.ID, comp.Paths[platform], err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("组件 %s 的入口 %s 是目录", comp.ID, comp.Paths[platform])
		}
		if comp.VersionCheck != nil {
			reported, err := runVersionCheck(entry, *comp.VersionCheck)
			if err != nil {
				return nil, fmt.Errorf("组件 %s 版本自报失败：%w", comp.ID, err)
			}
			if reported != comp.VersionCheck.Expect {
				return nil, fmt.Errorf("组件 %s 自报版本 %q，清单要求 %q", comp.ID, reported, comp.VersionCheck.Expect)
			}
		}
		if comp.PackageJSON != "" {
			if err := verifyPackageJSON(filepath.Join(runtimeRoot, filepath.FromSlash(comp.PackageJSON)), comp); err != nil {
				return nil, err
			}
		}
		if comp.Kind == kindPythonPackage {
			if err := verifyDistInfo(runtimeRoot, comp); err != nil {
				return nil, err
			}
		}
		reports = append(reports, componentReport{
			ID:      comp.ID,
			Version: comp.Version,
			License: comp.License,
			Source:  comp.Source,
			File:    "runtime/" + comp.Paths[platform],
		})
	}
	return reports, nil
}

// runVersionCheck 运行清单声明的版本探测命令并返回 trim 后的输出。
// runVersionCheck runs the manifest's version probe and returns the trimmed output.
func runVersionCheck(entry string, check versionCheck) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// #nosec G204 -- 可执行件由清单在已验证的运行时目录内指定，参数是清单固定字面量
	output, err := exec.CommandContext(ctx, entry, check.Args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("执行 %s %v：%w（输出 %s）", entry, check.Args, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

// verifyPackageJSON 核对 npm 组件的 package.json 版本。
// verifyPackageJSON checks the npm component's package.json version.
func verifyPackageJSON(path string, comp component) error {
	// #nosec G304 -- 路径由清单从已验证的运行时目录拼出
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("组件 %s 读取 package.json 失败：%w", comp.ID, err)
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return fmt.Errorf("组件 %s 的 package.json 不是合法 JSON：%w", comp.ID, err)
	}
	if pkg.Version != comp.Version {
		return fmt.Errorf("组件 %s 的 package.json 版本 %q，清单要求 %q", comp.ID, pkg.Version, comp.Version)
	}
	return nil
}

// verifyDistInfo 在 site-packages 里找到名称与版本都匹配的 dist-info。
// verifyDistInfo requires a matching dist-info name and version under site-packages.
func verifyDistInfo(runtimeRoot string, comp component) error {
	root := filepath.Join(runtimeRoot, filepath.FromSlash(comp.Scan.Root))
	entries, err := os.ReadDir(root) // #nosec G304 -- 路径由清单从已验证的运行时目录拼出
	if err != nil {
		return fmt.Errorf("组件 %s 读取 %s：%w", comp.ID, comp.Scan.Root, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".dist-info") {
			continue
		}
		meta, err := readDistInfoMetadata(filepath.Join(root, entry.Name()))
		if err != nil {
			return fmt.Errorf("组件 %s 读取 %s 元数据：%w", comp.ID, entry.Name(), err)
		}
		if meta.Name == comp.PackageName && meta.Version == comp.Version {
			return nil
		}
	}
	return fmt.Errorf("组件 %s 在 %s 下找不到 %s 版本 %s 的 dist-info",
		comp.ID, comp.Scan.Root, comp.PackageName, comp.Version)
}

// distInfoMeta 是 METADATA 里本工具关心的字段。
// distInfoMeta carries the METADATA fields this tool reads.
type distInfoMeta struct {
	Name    string
	Version string
	License string
}

// readDistInfoMetadata 解析 dist-info/METADATA 的头部（RFC 822 风格，读到空行为止）。
// readDistInfoMetadata parses the RFC 822-style METADATA header.
func readDistInfoMetadata(distInfoDir string) (distInfoMeta, error) {
	path := filepath.Join(distInfoDir, "METADATA")
	// #nosec G304 -- 路径由 site-packages 下的 dist-info 目录拼出
	raw, err := os.ReadFile(path)
	if err != nil {
		return distInfoMeta{}, err
	}
	meta := distInfoMeta{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Name":
			meta.Name = strings.TrimSpace(value)
		case "Version":
			meta.Version = strings.TrimSpace(value)
		case "License", "License-Expression":
			if meta.License == "" {
				meta.License = strings.TrimSpace(value)
			}
		}
	}
	return meta, nil
}
