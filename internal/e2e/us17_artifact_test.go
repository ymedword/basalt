//go:build e2e

package e2e

// 本文件判 US-17 的发布制品烟测（模块卡 §4.2/§4.4 FR-11）：被测二进制只能是
// `BASALT_E2E_ARTIFACT` 指向的归档解出来的那一份，不临时构建；解压、启动、健康检查、
// 管理台静态壳与一条 mock 任务都要真的走通。平台不适配时写明理由跳过——但 stage
// 只在对应宿主上运行对应检查，跳过不算通过。
// This file covers US-17's artifact smoke tests: the binary under test is exactly the
// one extracted from BASALT_E2E_ARTIFACT, never a fresh build, and it must start,
// answer health, serve the console shell, and complete one mock task.

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// requirePlatform 在平台不符时跳过并写明理由；stage 只在匹配宿主上运行该检查。
// requirePlatform skips with a written reason on a platform mismatch.
func requirePlatform(t *testing.T, goos, goarch string) {
	t.Helper()
	if runtime.GOOS != goos || runtime.GOARCH != goarch {
		t.Skipf("本检查只在 %s/%s 上判卷（当前 %s/%s）；stage 在匹配宿主上运行，跳过不算通过",
			goos, goarch, runtime.GOOS, runtime.GOARCH)
	}
}

// skipCandidateOnly 在只提供候选二进制、且制品接缝缺失时跳过制品用例：`make e2e`
// 就是这个形态，它判的是 build 出来的候选产物，不提供归档或发布目录接缝。制品判卷由
// us17-windows / us17-linux / us17-linux-arm64 / us17-aggregate 四个 stage 在对应
// 宿主上执行，那里的 required_tests 逐名命中——跳过不计为通过，也不改变判据面。
// skipCandidateOnly skips artifact cases when only the candidate binary is provided
// (the `make e2e` shape); the us17-* stages judge artifacts on their hosts.
func skipCandidateOnly(t *testing.T, required ...string) {
	t.Helper()
	for _, name := range required {
		if os.Getenv(name) != "" {
			return
		}
	}
	if os.Getenv(candidateBinaryEnv) == "" {
		return
	}
	t.Skipf("make e2e 只提供候选二进制（%s），未提供 %v；制品检查由 us17-* stage 判卷，跳过不算通过",
		candidateBinaryEnv, required)
}

// requireArtifact 断言制品接缝已由 stage 提供；缺省即失败，绝不回退到临时构建。
// requireArtifact fails closed when the stage did not provide the artifact seam.
func requireArtifact(t *testing.T) string {
	t.Helper()
	skipCandidateOnly(t, artifactEnv)
	archive := os.Getenv(artifactEnv)
	if archive == "" {
		t.Fatalf("%s 未设置：e2e 必须判归档内的二进制，不得临时构建", artifactEnv)
	}
	if os.Getenv(artifactVersionEnv) == "" {
		t.Fatalf("%s 未设置：必须声明归档版本", artifactVersionEnv)
	}
	return archive
}

// artifactSmoke 跑一次最小可用的制品烟测：版本回显、serve 起在回环地址、健康检查、
// 管理台静态壳、一条 mock 任务完成。
// artifactSmoke runs the minimal artifact smoke: version, serve, health, console, mock run.
func artifactSmoke(t *testing.T, archive string) {
	t.Helper()
	if info, err := os.Stat(archive); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("归档不可用 %s：%v", archive, err)
	}
	h := newHarness(t)

	res := h.run(t, nil, "--version")
	requireExit(t, res, 0, "归档 --version")
	requireContains(t, res.Stdout, expectedVersion, "归档 --version 必须回显归档版本")

	fake := newFakeModel(t, textResponse("制品烟测完成"))
	addr, p := startReleaseServe(t, h, fake.URL(), "制品烟测")
	defer p.stop(t)

	console := webRequest(t, addr, http.MethodGet, "/console", "", nil)
	requireWebStatus(t, console, http.StatusOK, "", "", "打开管理台")
	if !strings.Contains(strings.ToLower(console.Raw), "<!doctype html") {
		t.Fatalf("管理台首页不是 HTML 壳：%.200s", console.Raw)
	}

	sess := webCreateSession(t, addr, "demo", nil)
	message := webPostMessage(t, addr, sess.SessionID, "制品烟测")
	requireWebStatus(t, message, http.StatusOK, "OK", "", "制品烟测任务")
	var run webRun
	decodeWebData(t, message, &run, "制品烟测任务")
	if run.Status != "completed" {
		t.Fatalf("制品烟测任务 status = %q，期望 completed（reason %q）", run.Status, run.Reason)
	}
}

// TestUS17_Artifact_WindowsFull：Windows full 档归档可运行（FR-01/FR-11）。
// TestUS17_Artifact_WindowsFull: the Windows full-tier archive runs.
func TestUS17_Artifact_WindowsFull(t *testing.T) {
	requirePlatform(t, "windows", "amd64")
	archive := requireArtifact(t)
	if !strings.HasSuffix(filepath.Base(archive), "-windows-amd64.zip") {
		t.Fatalf("Windows full 归档名应为 basalt-<version>-windows-amd64.zip，实际 %s",
			filepath.Base(archive))
	}
	artifactSmoke(t, archive)
}

// TestUS17_Artifact_LinuxFull：Linux amd64 full 档归档可运行（FR-01/FR-11）。
// TestUS17_Artifact_LinuxFull: the Linux amd64 full-tier archive runs.
func TestUS17_Artifact_LinuxFull(t *testing.T) {
	requirePlatform(t, "linux", "amd64")
	archive := requireArtifact(t)
	if !strings.HasSuffix(filepath.Base(archive), "-linux-amd64.tar.gz") {
		t.Fatalf("Linux full 归档名应为 basalt-<version>-linux-amd64.tar.gz，实际 %s",
			filepath.Base(archive))
	}
	artifactSmoke(t, archive)
}

// TestUS17_Artifact_LinuxArm64：Linux arm64 归档可运行（FR-01/FR-11；external 层）。
// TestUS17_Artifact_LinuxArm64: the Linux arm64 archive runs.
func TestUS17_Artifact_LinuxArm64(t *testing.T) {
	requirePlatform(t, "linux", "arm64")
	archive := requireArtifact(t)
	if !strings.HasSuffix(filepath.Base(archive), "-linux-arm64.tar.gz") {
		t.Fatalf("Linux arm64 归档名应为 basalt-<version>-linux-arm64.tar.gz，实际 %s",
			filepath.Base(archive))
	}
	artifactSmoke(t, archive)
}

// TestUS17_Artifact_NoDevToolchain：最小 PATH 下二进制仍能启动并提供健康检查
// （FR-11；归档运行时不依赖本机 Go 工具链）。
// TestUS17_Artifact_NoDevToolchain: the archived binary starts on a minimal PATH.
func TestUS17_Artifact_NoDevToolchain(t *testing.T) {
	requireArtifact(t)
	h := newHarness(t)
	initWorkspace(t, h)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr("http://127.0.0.1:1", addr))

	emptyPath := t.TempDir()
	p := h.start(t, append(fixtureEnv(), "PATH="+emptyPath), "serve")
	p.waitHTTP(t, "http://"+addr+"/api/v1/health", 30*time.Second)
	p.kill()

	version := h.run(t, append(fixtureEnv(), "PATH="+emptyPath), "--version")
	requireExit(t, version, 0, "最小 PATH 下 --version")
	requireContains(t, version.Stdout, expectedVersion, "最小 PATH 下 --version")
}

// TestUS17_Artifact_BindsToArchive：判卷对象必须来自归档——二进制路径落在解压目录、
// 版本与归档声明一致、归档哈希与 stage 声明一致（模块卡 §4.3.3）。
// TestUS17_Artifact_BindsToArchive: the binary under test comes from the archive.
func TestUS17_Artifact_BindsToArchive(t *testing.T) {
	archive := requireArtifact(t)
	version := os.Getenv(artifactVersionEnv)
	if strings.HasPrefix(version, "v") || !artifactVersionPattern.MatchString(version) {
		t.Fatalf("%s 必须是 X.Y.Z（不带 v），实际 %q", artifactVersionEnv, version)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("归档不可用 %s：%v", archive, err)
	}
	if !strings.Contains(binaryPath, "basalt-e2e-artifact-") {
		t.Fatalf("被测二进制 %s 不在归档解压目录里——e2e 必须判归档构建，不得临时构建", binaryPath)
	}
	if binarySource != binarySourceCandidate {
		t.Fatalf("被测二进制来源 = %q，期望 %q（归档）", binarySource, binarySourceCandidate)
	}

	res := newHarness(t).run(t, nil, "--version")
	requireExit(t, res, 0, "归档 --version")
	requireContains(t, res.Stdout, version, "归档 --version 必须回显归档版本")
}
