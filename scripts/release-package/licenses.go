package main

// 本文件收集随包组件的许可证文本并生成 `LICENSES/THIRD-PARTY-NOTICES.md`。
// 三类来源各自读取**实际安装的元数据**：
//
//   - npm 依赖树：每个 package.json 的 name / version / license；
//   - Python 依赖树：每个 dist-info/METADATA 的 Name / Version / License；
//   - Go 模块：产品二进制 `go version -m` 的 dep 行 + 模块缓存里的许可证文本。
//
// 许可证全文按组件目录原样复制，清单只做索引——"表里说 MIT"不能替代"文本随包"。
// This file collects license texts and writes the third-party notices. All versions
// come from the installed metadata, and the texts are copied verbatim into the package.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	licensesDirName = "LICENSES"
	noticesFileName = "THIRD-PARTY-NOTICES.md"

	groupNPM    = "随附 npm 包（DBHub 依赖树）"
	groupPython = "随附 Python 包（Fetch 依赖树）"
	groupGo     = "Go 模块（产品二进制 go version -m 读数）"
)

// licenseFilePattern 匹配常见的许可证文件名（LICENSE / LICENCE / COPYING / NOTICE / COPYRIGHT）。
// licenseFilePattern matches the conventional license file names.
var licenseFilePattern = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|copyright)([.\-_].*)?$`)

// licenseRow 是通知清单里的一行；Location 是包内路径，便于按行找到全文。
// licenseRow is one notices row; Location points at the in-package license text.
type licenseRow struct {
	Group    string
	Name     string
	Version  string
	License  string
	Location string
}

// licenseInventory 汇总许可证文件数（不含通知文件本身）。
// licenseInventory counts copied license texts (the notices file excluded).
type licenseInventory struct {
	files int
}

// collectLicenses 复制组件许可证与依赖树许可证，并写出通知文件。
// collectLicenses copies component and dependency license texts, then writes the notices.
func collectLicenses(repoRoot, runtimeRoot, binaryPath, programDir string,
	loaded manifest, platformLabel string, components []componentReport,
) (licenseInventory, error) {
	var inventory licenseInventory
	destRoot := filepath.Join(programDir, licensesDirName)
	if err := os.MkdirAll(destRoot, 0o755); err != nil { // #nosec G301 -- 包内公开目录
		return inventory, fmt.Errorf("创建许可证目录：%w", err)
	}
	var rows []licenseRow
	for _, comp := range loaded.Components {
		componentDir := filepath.Join(destRoot, comp.ID)
		for _, rel := range comp.RuntimeLicenses {
			copied, err := copyLicenseFile(filepath.Join(runtimeRoot, filepath.FromSlash(rel)), componentDir)
			if err != nil {
				return inventory, fmt.Errorf("组件 %s 的许可证 %s：%w", comp.ID, rel, err)
			}
			inventory.files += copied
		}
		for _, rel := range comp.RepoLicenses {
			copied, err := copyLicenseFile(filepath.Join(repoRoot, filepath.FromSlash(rel)), componentDir)
			if err != nil {
				return inventory, fmt.Errorf("组件 %s 的许可证 %s：%w", comp.ID, rel, err)
			}
			inventory.files += copied
		}
		if comp.Scan != nil {
			var (
				scanned []licenseRow
				files   int
				err     error
			)
			switch comp.Scan.Kind {
			case scanKindNodeModules:
				scanned, files, err = scanNodeModules(filepath.Join(runtimeRoot, filepath.FromSlash(comp.Scan.Root)),
					filepath.Join(destRoot, comp.ID), comp.ID)
			case scanKindDistInfo:
				scanned, files, err = scanPythonPackages(filepath.Join(runtimeRoot, filepath.FromSlash(comp.Scan.Root)),
					filepath.Join(destRoot, comp.ID), comp.ID)
			}
			if err != nil {
				return inventory, err
			}
			rows = append(rows, scanned...)
			inventory.files += files
		}
	}
	goRows, goFiles, err := collectGoModules(repoRoot, binaryPath, filepath.Join(destRoot, "go-modules"))
	if err != nil {
		return inventory, err
	}
	rows = append(rows, goRows...)
	inventory.files += goFiles

	if err := writeNotices(filepath.Join(destRoot, noticesFileName), platformLabel, components,
		rows, inventory.files); err != nil {
		return inventory, err
	}
	return inventory, nil
}

// copyLicenseFile 复制一个许可证文件到目标目录；目标同名文件已存在时跳过。
// copyLicenseFile copies one license file, skipping an existing same-named target.
func copyLicenseFile(src, destDir string) (int, error) {
	info, err := os.Stat(src)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		stats, err := copyTreeCounted(src, destDir)
		return stats, err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil { // #nosec G301 -- 包内公开目录
		return 0, err
	}
	target := filepath.Join(destDir, filepath.Base(src))
	if _, err := os.Stat(target); err == nil {
		return 0, nil
	}
	if err := copyFileMode(src, target, info.Mode()); err != nil {
		return 0, err
	}
	return 1, nil
}

// copyTreeCounted 复制目录并返回复制出的文件数。
// copyTreeCounted copies a directory and returns the number of copied files.
func copyTreeCounted(src, dst string) (int, error) {
	stats, err := treeStatsOf(src)
	if err != nil {
		return 0, err
	}
	if err := copyTree(src, dst); err != nil {
		return 0, err
	}
	return stats.files, nil
}

// treeStatsOf 统计一棵树里的文件数与字节数（符号链接计一个文件、零字节）。
// treeStatsOf counts files and bytes; a symlink counts as one zero-byte file.
func treeStatsOf(root string) (treeStats, error) {
	var stats treeStats
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			stats.bytes += info.Size()
		}
		stats.files++
		return nil
	})
	return stats, err
}

// licenseFilesIn 返回目录下**非递归**的许可证文件路径（按名称排序）。
// licenseFilesIn lists the license files directly under dir.
func licenseFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir) // #nosec G304 -- 目录来自运行时或模块缓存
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !licenseFilePattern.MatchString(entry.Name()) {
			continue
		}
		files = append(files, filepath.Join(dir, entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

// npmPackage 是 package.json 里本工具关心的字段；license 既可能是字符串也可能是对象。
// npmPackage keeps the fields this tool needs; license may be a string or an object.
type npmPackage struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	License json.RawMessage `json:"license"`
}

// npmLicense 把 package.json 的 license 字段归一成字符串。
// npmLicense normalizes the package.json license field.
func npmLicense(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var object struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		return object.Type
	}
	return ""
}

// isInstalledPackageDir 判断目录是否是 npm 安装位置：直接位于 node_modules 下，
// 或作为 @scope/name 位于 node_modules 下。
// isInstalledPackageDir reports whether dir is an npm install root (node_modules/name
// or node_modules/@scope/name).
func isInstalledPackageDir(path string) bool {
	parent := filepath.Dir(path)
	if filepath.Base(parent) == "node_modules" {
		return true
	}
	return strings.HasPrefix(filepath.Base(parent), "@") &&
		filepath.Base(filepath.Dir(parent)) == "node_modules"
}

// scanKindNodeModules 遍历 npm 依赖树：记录包名/版本/许可证，并复制每个包的许可证文本。
// scanKindNodeModules walks the npm tree, recording name/version/license and copying texts.
func scanNodeModules(root, destRoot, componentID string) ([]licenseRow, int, error) {
	var (
		rows  []licenseRow
		files int
	)
	seen := make(map[string]bool)
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		// 只记录真正的安装位置；包内子目录的 package.json（浏览器入口、测试夹具）
		// 不是依赖，记进来会产出空名字行或把夹具当包。
		// Only real install roots count; fixture or entry-point package.json files are skipped.
		if !isInstalledPackageDir(path) {
			return nil
		}
		packageJSON := filepath.Join(path, "package.json")
		if info, err := os.Stat(packageJSON); err != nil || info.IsDir() {
			return nil
		}
		// #nosec G304 -- 路径来自待打包的 node_modules 目录
		raw, err := os.ReadFile(packageJSON)
		if err != nil {
			return err
		}
		var pkg npmPackage
		if err := json.Unmarshal(raw, &pkg); err != nil {
			return fmt.Errorf("组件 %s 的 %s 不是合法 JSON：%w", componentID, packageJSON, err)
		}
		relDir, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		destDir := filepath.Join(destRoot, relDir)
		licenseFiles, err := licenseFilesIn(path)
		if err != nil {
			return err
		}
		for _, licenseFile := range licenseFiles {
			copied, err := copyLicenseFile(licenseFile, destDir)
			if err != nil {
				return err
			}
			files += copied
		}
		key := pkg.Name + "@" + pkg.Version
		if seen[key] {
			return nil
		}
		seen[key] = true
		location := filepath.ToSlash(filepath.Join(componentID, relDir))
		rows = append(rows, licenseRow{
			Group:    groupNPM,
			Name:     pkg.Name,
			Version:  pkg.Version,
			License:  firstNonEmpty(npmLicense(pkg.License), "见许可证文件"),
			Location: location,
		})
		return nil
	})
	if walkErr != nil {
		return nil, 0, fmt.Errorf("扫描组件 %s 的 npm 依赖树：%w", componentID, walkErr)
	}
	sortRows(rows)
	return rows, files, nil
}

// scanPythonPackages 遍历 site-packages 下的 dist-info：记录元数据并复制 licenses/ 子树。
// scanPythonPackages walks dist-info directories, recording metadata and copying licenses.
func scanPythonPackages(root, destRoot, componentID string) ([]licenseRow, int, error) {
	entries, err := os.ReadDir(root) // #nosec G304 -- 目录来自运行时布局
	if err != nil {
		return nil, 0, fmt.Errorf("读取 %s：%w", root, err)
	}
	var (
		rows  []licenseRow
		files int
	)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".dist-info") {
			continue
		}
		distDir := filepath.Join(root, entry.Name())
		meta, err := readDistInfoMetadata(distDir)
		if err != nil {
			return nil, 0, fmt.Errorf("读取 %s 元数据：%w", entry.Name(), err)
		}
		licenseDir := filepath.Join(distDir, "licenses")
		if info, statErr := os.Stat(licenseDir); statErr == nil && info.IsDir() {
			copied, err := copyTreeCounted(licenseDir, filepath.Join(destRoot, entry.Name(), "licenses"))
			if err != nil {
				return nil, 0, err
			}
			files += copied
		}
		for _, name := range []string{"LICENSE", "LICENSE.txt", "COPYING"} {
			src := filepath.Join(distDir, name)
			if _, statErr := os.Stat(src); statErr != nil {
				continue
			}
			copied, err := copyLicenseFile(src, filepath.Join(destRoot, entry.Name()))
			if err != nil {
				return nil, 0, err
			}
			files += copied
		}
		rows = append(rows, licenseRow{
			Group:    groupPython,
			Name:     meta.Name,
			Version:  meta.Version,
			License:  firstNonEmpty(meta.License, "见许可证文件"),
			Location: filepath.ToSlash(filepath.Join(componentID, entry.Name(), "licenses")),
		})
	}
	sortRows(rows)
	return rows, files, nil
}

// collectGoModules 读取二进制内嵌的模块清单，并从本地模块缓存复制许可证文本。
// collectGoModules reads the binary's embedded modules and copies cached license texts.
func collectGoModules(repoRoot, binaryPath, destRoot string) ([]licenseRow, int, error) {
	// #nosec G204 -- 固定工具名与调用方给出的二进制路径；只读构建信息
	output, err := exec.Command("go", "version", "-m", binaryPath).Output()
	if err != nil {
		return nil, 0, fmt.Errorf("读取二进制模块清单：%w", err)
	}
	var (
		rows  []licenseRow
		files int
	)
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "dep" {
			continue
		}
		module, version := fields[1], fields[2]
		key := module + "@" + version
		if seen[key] {
			continue
		}
		seen[key] = true
		moduleDir, err := goModuleDir(repoRoot, module, version)
		if err != nil {
			return nil, 0, err
		}
		licenseFiles, err := licenseFilesIn(moduleDir)
		if err != nil {
			return nil, 0, fmt.Errorf("模块 %s 读取许可证目录：%w", key, err)
		}
		destDir := filepath.Join(destRoot, filepath.FromSlash(module))
		for _, licenseFile := range licenseFiles {
			copied, err := copyLicenseFile(licenseFile, destDir)
			if err != nil {
				return nil, 0, err
			}
			files += copied
		}
		rows = append(rows, licenseRow{
			Group:    groupGo,
			Name:     module,
			Version:  version,
			License:  detectGoLicense(moduleDir),
			Location: filepath.ToSlash(filepath.Join("go-modules", module)),
		})
	}
	sortRows(rows)
	return rows, files, nil
}

// goModuleDir 用 `go list -m` 解析模块在本地缓存里的目录（离线可解析已构建依赖）。
// goModuleDir resolves a module's local cache directory via `go list -m`.
func goModuleDir(repoRoot, module, version string) (string, error) {
	command := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", module+"@"+version) // #nosec G204 -- 固定工具与清单里的模块坐标
	command.Dir = repoRoot
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("解析模块目录 %s@%s：%w", module, version, err)
	}
	dir := strings.TrimSpace(string(output))
	if dir == "" {
		return "", fmt.Errorf("模块 %s@%s 没有本地目录（未下载？）", module, version)
	}
	return dir, nil
}

// detectGoLicense 按许可证正文的首段识别常见家族；识别不了就如实写"见许可证文件"。
// detectGoLicense recognizes common license families from the text; otherwise says so.
func detectGoLicense(dir string) string {
	licenseFiles, err := licenseFilesIn(dir)
	if err != nil {
		return "见许可证文件"
	}
	for _, licenseFile := range licenseFiles {
		// #nosec G304 -- 路径来自模块缓存目录
		raw, err := os.ReadFile(licenseFile)
		if err != nil {
			continue
		}
		text := strings.ToLower(string(raw))
		if len(text) > 4096 {
			text = text[:4096]
		}
		switch {
		case strings.Contains(text, "apache license") && strings.Contains(text, "2.0"):
			return "Apache-2.0"
		case strings.Contains(text, "mozilla public license") && strings.Contains(text, "2.0"):
			return "MPL-2.0"
		case strings.Contains(text, "permission is hereby granted, free of charge"):
			return "MIT"
		case strings.Contains(text, "redistribution and use in source and binary forms"):
			if strings.Contains(text, "neither the name") {
				return "BSD-3-Clause"
			}
			return "BSD-2-Clause"
		case strings.Contains(text, "permission to use, copy, modify, and/or distribute"):
			return "ISC"
		}
	}
	return "见许可证文件"
}

// writeNotices 生成第三方通知文件（组件表 + 三棵依赖树表）。
// writeNotices renders the notices file with the component table and dependency tables.
func writeNotices(path, platformLabel string, components []componentReport,
	rows []licenseRow, licenseFiles int,
) error {
	var builder strings.Builder
	builder.WriteString("# 第三方组件与许可证\n\n")
	builder.WriteString("本文件由 `scripts/release-package` 在打包时按**随包文件与本地模块缓存**生成；")
	builder.WriteString("组件版本取自实际入口自报、`package.json`、`dist-info/METADATA` 与 `go version -m` 读数，")
	builder.WriteString("不是文档抄写。许可证全文位于本目录下的同名子目录；许可证列无法识别时写「见许可证文件」。\n\n")
	fmt.Fprintf(&builder, "- 平台：%s\n- 许可证文本文件数：%d\n\n", platformLabel, licenseFiles)

	builder.WriteString("## 随包运行时组件\n\n")
	builder.WriteString("| 组件 | 版本 | 许可证 | 来源 | 包内入口 | 许可证文本 |\n")
	builder.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, comp := range components {
		fmt.Fprintf(&builder, "| %s | %s | %s | %s | `%s` | `LICENSES/%s/` |\n",
			comp.ID, comp.Version, comp.License, comp.Source, comp.File, comp.ID)
	}
	builder.WriteString("\n")
	for _, group := range []string{groupNPM, groupPython, groupGo} {
		selected := make([]licenseRow, 0, len(rows))
		for _, row := range rows {
			if row.Group == group {
				selected = append(selected, row)
			}
		}
		if len(selected) == 0 {
			continue
		}
		fmt.Fprintf(&builder, "## %s\n\n", group)
		builder.WriteString("| 包 / 模块 | 版本 | 许可证 | 包内许可证位置 |\n| --- | --- | --- | --- |\n")
		for _, row := range selected {
			fmt.Fprintf(&builder, "| %s | %s | %s | `%s` |\n",
				row.Name, row.Version, row.License, row.Location)
		}
		builder.WriteString("\n")
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o644); err != nil { // #nosec G306 -- 包内公开文档
		return fmt.Errorf("写出第三方通知：%w", err)
	}
	return nil
}

// firstNonEmpty 返回第一个非空字符串。
// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// sortRows 按包名、版本排序，保证通知文件稳定。
// sortRows keeps the notices file stable.
func sortRows(rows []licenseRow) {
	sort.Slice(rows, func(left, right int) bool {
		if rows[left].Name != rows[right].Name {
			return rows[left].Name < rows[right].Name
		}
		return rows[left].Version < rows[right].Version
	})
}
