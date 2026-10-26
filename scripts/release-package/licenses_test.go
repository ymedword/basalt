package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNpmLicense_NormalizesStringAndObject(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"MIT"`, "MIT"},
		{`{"type":"Apache-2.0","url":"https://example.invalid"}`, "Apache-2.0"},
		{`null`, ""},
		{`42`, ""},
	}
	for _, tc := range cases {
		if got := npmLicense([]byte(tc.raw)); got != tc.want {
			t.Errorf("npmLicense(%s) = %q，期望 %q", tc.raw, got, tc.want)
		}
	}
}

func TestScanNodeModules_RecordsAndCopiesLicenses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node_modules")
	destRoot := t.TempDir()
	writeTestFile(t, filepath.Join(root, "top", "package.json"),
		`{"name":"top","version":"1.0.0","license":"MIT"}`)
	writeTestFile(t, filepath.Join(root, "top", "LICENSE"), "MIT text")
	writeTestFile(t, filepath.Join(root, "top", "node_modules", "nested", "package.json"),
		`{"name":"nested","version":"2.0.0","license":{"type":"Apache-2.0"}}`)
	writeTestFile(t, filepath.Join(root, "top", "node_modules", "nested", "LICENSE.txt"), "Apache text")
	// 同一 name@version 出现在另一层时只记一行，但许可证文本仍各复制一份。
	// A repeated name@version is recorded once while its texts are still copied.
	writeTestFile(t, filepath.Join(root, "other", "package.json"),
		`{"name":"other","version":"3.0.0","license":"ISC"}`)
	writeTestFile(t, filepath.Join(root, "other", "node_modules", "nested", "package.json"),
		`{"name":"nested","version":"2.0.0","license":"Apache-2.0"}`)
	writeTestFile(t, filepath.Join(root, "other", "node_modules", "nested", "LICENSE.txt"), "Apache text")
	// 包内子目录的 package.json 是浏览器入口或测试夹具，不属于安装的依赖。
	// package.json files inside a package are entry points or fixtures, not dependencies.
	writeTestFile(t, filepath.Join(root, "top", "dist", "browser", "package.json"), `{"type":"module"}`)
	writeTestFile(t, filepath.Join(root, "top", "tools", "package.json"),
		`{"name":"1to2","version":"1.0.0","license":"MIT"}`)
	// scoped 包的安装位置是 node_modules/@scope/name。
	writeTestFile(t, filepath.Join(root, "@scope", "pkg", "package.json"),
		`{"name":"@scope/pkg","version":"4.0.0","license":"MIT"}`)

	rows, files, err := scanNodeModules(root, destRoot, "dbhub")
	if err != nil {
		t.Fatalf("扫描 npm 依赖树失败: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("应记录 4 个不同包（含 scoped），实际 %d 行: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Name == "" || row.Version == "" {
			t.Fatalf("不应记录空名字或空版本的行: %+v", row)
		}
		if row.Name == "1to2" {
			t.Fatalf("包内测试夹具不应记为依赖: %+v", row)
		}
	}
	if files != 3 {
		t.Fatalf("应复制 3 个许可证文件，实际 %d", files)
	}
	if rows[0].Name != "@scope/pkg" {
		t.Fatalf("排序后的首行应为 @scope/pkg，实际 %+v", rows[0])
	}
	if _, err := os.Stat(filepath.Join(destRoot, "top", "LICENSE")); err != nil {
		t.Fatalf("top 的许可证未复制: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "top", "node_modules", "nested", "LICENSE.txt")); err != nil {
		t.Fatalf("nested 的许可证未复制: %v", err)
	}
}

func TestScanNodeModules_RejectsInvalidPackageJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node_modules")
	writeTestFile(t, filepath.Join(root, "bad", "package.json"), "{not json")
	if _, _, err := scanNodeModules(root, t.TempDir(), "dbhub"); err == nil {
		t.Fatal("非法 package.json 应报错而不是静默跳过")
	}
}

func TestScanPythonPackages_RecordsDistInfoAndCopiesLicenses(t *testing.T) {
	root := t.TempDir()
	destRoot := t.TempDir()
	distDir := filepath.Join(root, "mcp_server_fetch-2026.10.24.dist-info")
	writeTestFile(t, filepath.Join(distDir, "METADATA"),
		"Metadata-Version: 2.1\nName: mcp-server-fetch\nVersion: 2026.10.24\nLicense-Expression: MIT\n\nbody\n")
	writeTestFile(t, filepath.Join(distDir, "licenses", "LICENSE"), "MIT text")
	writeTestFile(t, filepath.Join(distDir, "top-not-a-license.txt"), "ignored")

	rows, files, err := scanPythonPackages(root, destRoot, "fetch")
	if err != nil {
		t.Fatalf("扫描 Python 包失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "mcp-server-fetch" || rows[0].License != "MIT" {
		t.Fatalf("Python 包读数不符合预期: %+v", rows)
	}
	if files != 1 {
		t.Fatalf("应复制 1 个许可证文件，实际 %d", files)
	}
	copied := filepath.Join(destRoot, "mcp_server_fetch-2026.10.24.dist-info", "licenses", "LICENSE")
	if _, err := os.Stat(copied); err != nil {
		t.Fatalf("licenses/ 子树未复制: %v", err)
	}
}

func TestDetectGoLicense_RecognizesCommonFamilies(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"mit", "MIT License\n\nPermission is hereby granted, free of charge, to any person", "MIT"},
		{"apache", "Apache License\nVersion 2.0, January 2004", "Apache-2.0"},
		{"mpl", "Mozilla Public License Version 2.0", "MPL-2.0"},
		{"bsd3", "Redistribution and use in source and binary forms... Neither the name of the copyright holder", "BSD-3-Clause"},
		{"bsd2", "Redistribution and use in source and binary forms, with or without modification", "BSD-2-Clause"},
		{"isc", "Permission to use, copy, modify, and/or distribute this software for any purpose", "ISC"},
		{"unknown", "All rights reserved.", "见许可证文件"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, "LICENSE"), tc.text)
			if got := detectGoLicense(dir); got != tc.want {
				t.Fatalf("detectGoLicense = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestDetectGoLicense_MissingText(t *testing.T) {
	if got := detectGoLicense(t.TempDir()); got != "见许可证文件" {
		t.Fatalf("没有许可证文件时应写「见许可证文件」，实际 %q", got)
	}
}

func TestWriteNotices_IncludesComponentsAndRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "THIRD-PARTY-NOTICES.md")
	components := []componentReport{{
		ID: "node", Version: "24.22.0", License: "MIT",
		Source: "https://nodejs.org/", File: "runtime/node/node.exe",
	}}
	rows := []licenseRow{
		{Group: groupNPM, Name: "@bytebase/dbhub", Version: "1.5.0", License: "MIT", Location: "dbhub/top"},
		{Group: groupPython, Name: "mcp-server-fetch", Version: "2026.10.24", License: "MIT", Location: "fetch/dist-info/licenses"},
		{Group: groupGo, Name: "github.com/spf13/cobra", Version: "v1.10.2", License: "Apache-2.0", Location: "go-modules/github.com/spf13/cobra"},
	}
	if err := writeNotices(path, platformLabel("windows", "amd64"), components, rows, 12); err != nil {
		t.Fatalf("写出通知失败: %v", err)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- 测试内路径
	if err != nil {
		t.Fatalf("读取通知失败: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"Windows amd64", "许可证文本文件数：12", "| node | 24.22.0 | MIT |",
		groupNPM, groupPython, groupGo,
		"@bytebase/dbhub", "mcp-server-fetch", "github.com/spf13/cobra",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("通知缺少 %q:\n%s", want, text)
		}
	}
}
