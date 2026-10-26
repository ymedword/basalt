package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseOptions_DefaultsAndValidation(t *testing.T) {
	opts, err := parseOptions([]string{"-binary", "bin/basalt", "-runtime", "bin/runtime", "-version", "0.1.0"})
	if err != nil {
		t.Fatalf("解析合法参数失败: %v", err)
	}
	if !filepath.IsAbs(opts.manifest) || !strings.HasSuffix(opts.manifest, filepath.FromSlash(defaultManifest)) {
		t.Fatalf("默认清单路径应落在仓库根下: %s", opts.manifest)
	}
	if opts.outDir != defaultOutDir {
		t.Fatalf("默认输出目录 = %q，期望 %q", opts.outDir, defaultOutDir)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"缺少二进制", []string{"-runtime", "bin/runtime", "-version", "0.1.0"}},
		{"缺少运行时目录", []string{"-binary", "bin/basalt", "-version", "0.1.0"}},
		{"版本号不合法", []string{"-binary", "bin/basalt", "-runtime", "bin/runtime", "-version", "v0.1"}},
		{"不接受位置参数", []string{"-binary", "bin/basalt", "-runtime", "bin/runtime", "-version", "0.1.0", "extra"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseOptions(tc.args); err == nil {
				t.Fatal("非法参数应被拒绝")
			}
		})
	}
}

func TestPlatformLabel(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"windows", "amd64", "Windows amd64"},
		{"linux", "amd64", "Linux amd64"},
		{"linux", "arm64", "Linux arm64"},
		{"plan9", "386", "plan9 386"},
	}
	for _, tc := range cases {
		if got := platformLabel(tc.goos, tc.goarch); got != tc.want {
			t.Fatalf("platformLabel(%s, %s) = %q，期望 %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestCheckReleaseBinary_RejectsNonGoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-binary.exe")
	writeTestFile(t, path, "plain text")
	if err := checkReleaseBinary(path); err == nil {
		t.Fatal("非 Go 二进制应被拒绝")
	}
}

func TestCheckReleaseBinary_ReadsBuildInfo(t *testing.T) {
	// 测试进程自身就是 Go 构建产物；只核对读数路径能走通、且平台与当前一致。
	// The test binary is a Go build; this exercises the reading path and platform match.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("无法定位测试可执行文件: %v", err)
	}
	err = checkReleaseBinary(exe)
	if err == nil {
		return
	}
	// 测试二进制通常开启 CGO，因此只允许"CGO_ENABLED 非 0"这一种可解释的拒绝。
	// Test binaries often enable CGO; only that refusal is expected here.
	if !strings.Contains(err.Error(), "CGO_ENABLED") {
		t.Fatalf("本机测试二进制的拒绝原因应只与 CGO 有关，实际: %v", err)
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		t.Logf("Linux 测试二进制 CGO 已开启，发布构建必须另用 CGO_ENABLED=0 构建: %v", err)
	}
}

func TestRun_PropagatesOptionErrors(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"-version", "0.1.0"}, &stdout); err == nil {
		t.Fatal("缺少必填参数时 run 应返回错误")
	}
	if stdout.Len() != 0 {
		t.Fatalf("失败时不应输出读数: %s", stdout.String())
	}
}
