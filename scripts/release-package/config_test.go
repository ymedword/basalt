package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCheckReleaseBinaryForTarget_ReadsBuildInfo(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	name := "release-package-test"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-trimpath", "-o", binary, "./scripts/release-package")
	cmd.Dir = root
	cmd.Env = append(cmd.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建测试二进制：%v\n%s", err, out)
	}
	if err := CheckReleaseBinaryForTarget(binary, runtime.GOOS, runtime.GOARCH); err != nil {
		t.Fatalf("本机构建应通过：%v", err)
	}
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "windows"
	}
	if err := CheckReleaseBinaryForTarget(binary, other, runtime.GOARCH); err == nil {
		t.Fatal("错误目标平台必须拒绝")
	}
}
