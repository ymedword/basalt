package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ymedword/basalt/scripts/releasekit"
)

func TestRun_UsageAndUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), nil, &stdout, &stderr); code != 0 {
		t.Fatalf("无参数应打印用法并返回 0，实际 %d", code)
	}
	if !strings.Contains(stdout.String(), "releasekit") {
		t.Fatalf("用法输出缺少工具名：%q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("-h 应返回 0，实际 %d", code)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(context.Background(), []string{"unknown"}, &stdout, &stderr); code != 2 {
		t.Fatalf("未知子命令应返回 2，实际 %d", code)
	}
	if !strings.Contains(stderr.String(), "未知子命令") {
		t.Fatalf("stderr 未点名未知子命令：%q", stderr.String())
	}
}

// 中文验收点：每个子命令在缺参数时必须失败退出（返回 1），不得静默成功。
// Every subcommand must fail closed (exit 1) when required inputs are missing.
func TestRun_SubcommandsFailClosed(t *testing.T) {
	cases := [][]string{
		{"preflight"},
		{"build"},
		{"package"},
		{"release"},
		{"verify-package"},
		{"record"},
		{"assemble"},
		{"verify"},
		{"repro"},
		{"smoke"},
		{"upgrade"},
		{"sbom"},
		{"provenance"},
		{"sign"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 1 {
			t.Fatalf("%v 应返回 1，实际 %d（stderr=%q）", args, code, stderr.String())
		}
	}
}

func TestRun_RejectsInvalidTargetAndTier(t *testing.T) {
	cases := [][]string{
		{"build", "--target", "plan9/amd64", "--tier", "binary"},
		{"build", "--target", "linux/amd64", "--tier", "huge"},
		{"build", "--target", "linux", "--tier", "binary"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), args, &stdout, &stderr); code != 1 {
			t.Fatalf("%v 应返回 1，实际 %d", args, code)
		}
	}
}

func TestParseTarget(t *testing.T) {
	target, err := parseTarget("linux/amd64", "full")
	if err != nil {
		t.Fatalf("合法目标应通过：%v", err)
	}
	if target.GOOS != "linux" || target.Tier != releasekit.TierFull {
		t.Fatalf("目标解析不符：%+v", target)
	}
	if _, err := parseTarget("linux", "full"); err == nil {
		t.Fatal("缺架构应报错")
	}
	if _, err := parseTarget("linux/amd64", "huge"); err == nil {
		t.Fatal("非法档位应报错")
	}
	if _, err := parseTarget("plan9/amd64", "binary"); err == nil {
		t.Fatal("清单外平台应报错")
	}
}

func TestWriteJSONFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	if err := writeJSONFile(path, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("写 JSON：%v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 JSON：%v", err)
	}
	if !strings.Contains(string(body), "\"a\": \"b\"") {
		t.Fatalf("JSON 内容不符：%q", body)
	}
	if err := writeJSONFile(filepath.Join(t.TempDir(), "missing", "out.json"), map[string]string{}); err == nil {
		t.Fatal("目录不存在应报错")
	}
}

// 中文验收点：--build-dir 缺省时必须给出真实目录（此前空字符串直接让构建失败），
// 显式目录不得被清理。
// resolveBuildDir must yield a real directory when the flag is empty, and must never
// remove a caller-provided directory.
func TestResolveBuildDir(t *testing.T) {
	explicit := t.TempDir()
	got, cleanup, err := resolveBuildDir(explicit)
	if err != nil {
		t.Fatalf("显式目录不应失败：%v", err)
	}
	if got != explicit {
		t.Fatalf("显式目录应原样返回：%q != %q", got, explicit)
	}
	cleanup()
	if _, err := os.Stat(explicit); err != nil {
		t.Fatalf("显式目录不得被清理：%v", err)
	}

	got, cleanup, err = resolveBuildDir("")
	if err != nil {
		t.Fatalf("缺省临时目录不应失败：%v", err)
	}
	if got == "" {
		t.Fatal("缺省时必须返回真实目录，收到空字符串")
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("临时目录应已创建：%v", err)
	}
	cleanup()
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("临时目录应在清理后消失：%v", err)
	}
}
