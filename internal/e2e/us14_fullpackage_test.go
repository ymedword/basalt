//go:build e2e

package e2e

// 本文件是 T117 的自动化面：**从候选二进制装包 → 解压到含空格的程序目录 → 首次启动
// → 用自动生成的访问密码登录 → 随包依赖就位**，并给出体积实测读数。它只覆盖模块卡
// M5 的自动化那一半——WSL2 Ubuntu 的实际安装（T118）与其余人工项仍是人工，单平台读数
// 不代替两者。
//
// 包内容按设计 §2.5.3「完整包 = 二进制 + `runtime/`」组装，但**只装本机可离线构建的
// 组件**：`docs-readonly` 是纯标准库、随主 module 构建；Node / Python / npm 组件要经包
// 仓库下载，属未授权的外部源（stage approval 原文），因此不下载、不内置。冒烟如实记录
// 它们缺席（dbhub 发布但未连接），**不**用一个装了半截的包冒充完整包，也**不**把设计
// §2.5.3 的 150 MB 估算当成实测。
//
// This file automates the Windows half of M5 only: assemble a package from the built
// candidate, unpack it into a path containing spaces, first start, log in with the
// generated password, and confirm the bundled dependency works. Node/Python/npm
// components need package registries, which the stage declares unauthorized, so they
// are neither downloaded nor bundled; the smoke records them as absent instead.

import (
	"archive/zip"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// us14dDocsReadOnlyVersion 必须与运行时组件清单（`mcpRuntimeComponents`）逐字一致：
	// 依赖轴按 `<runtime>/mcp/docs-readonly/<版本>/` 定位，版本目录写错就等于组件不存在。
	// The component version directory must match the runtime component manifest exactly.
	us14dDocsReadOnlyVersion = "1.0.0"
)

// us14dPackage 是一次装包的全部读数：路径、二进制摘要与**实测**字节数。
// us14dPackage carries the paths, the binary digest and the measured byte counts.
type us14dPackage struct {
	ProgDir      string // 打包暂存目录里的程序目录（解压前）
	ArchivePath  string
	BinaryPath   string
	BinarySHA256 string
	BinaryBytes  int64
	RuntimeBytes int64 // runtime/ 树的总字节
	RawBytes     int64 // 整棵程序目录的总字节
	ArchiveBytes int64
	FileCount    int
}

// us14dExeSuffix 返回当前平台的平台可执行件后缀。
// us14dExeSuffix returns the platform executable suffix.
func us14dExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// us14dAssemblePackage 组装一次完整包并压缩，返回实测读数。
// us14dAssemblePackage assembles the package once and returns the measured readings.
func us14dAssemblePackage(t *testing.T) us14dPackage {
	t.Helper()
	if binaryPath == "" {
		t.Fatal("被测二进制未构建——TestMain 未运行或构建失败")
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}

	progDir := filepath.Join(t.TempDir(), "stage", "basalt")
	binaryName := "basalt" + us14dExeSuffix()
	packagedBinary := filepath.Join(progDir, binaryName)
	us14dCopyFile(t, binaryPath, packagedBinary, 0o755)

	// 随包组件：自研只读 MCP server 按发行布局落到
	// `<程序目录>/runtime/mcp/docs-readonly/<版本>/docs-readonly[.exe]`。
	componentDir := filepath.Join(progDir, "runtime", "mcp", "docs-readonly", us14dDocsReadOnlyVersion)
	if err := os.MkdirAll(componentDir, 0o755); err != nil {
		t.Fatalf("创建运行时组件目录失败: %v", err)
	}
	componentPath := filepath.Join(componentDir, "docs-readonly"+us14dExeSuffix())
	build := exec.Command("go", "build", "-trimpath", "-o", componentPath,
		"./examples/shared/mcp/docs-readonly")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建随包组件 docs-readonly 失败: %v\n%s", err, out)
	}

	archivePath := filepath.Join(filepath.Dir(progDir), "basalt-full.zip")
	rawBytes, fileCount := us14dZipTree(t, progDir, archivePath)
	archiveInfo, err := os.Stat(archivePath)
	if err != nil {
		t.Fatalf("读取包体积失败: %v", err)
	}
	binaryInfo, err := os.Stat(packagedBinary)
	if err != nil {
		t.Fatalf("读取二进制体积失败: %v", err)
	}
	sum, err := fileSHA256(packagedBinary)
	if err != nil {
		t.Fatalf("计算随包二进制哈希失败: %v", err)
	}
	runtimeBytes, _ := us14dTreeSize(t, filepath.Join(progDir, "runtime"))
	return us14dPackage{
		ProgDir:      progDir,
		ArchivePath:  archivePath,
		BinaryPath:   packagedBinary,
		BinarySHA256: sum,
		BinaryBytes:  binaryInfo.Size(),
		RuntimeBytes: runtimeBytes,
		RawBytes:     rawBytes,
		ArchiveBytes: archiveInfo.Size(),
		FileCount:    fileCount,
	}
}

// us14dTreeSize 返回一棵目录树的总字节与文件数。
// us14dTreeSize returns the total bytes and file count of a tree.
func us14dTreeSize(t *testing.T, dir string) (int64, int) {
	t.Helper()
	var (
		total int64
		count int
	)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
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
		total += info.Size()
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("统计 %s 失败: %v", dir, err)
	}
	return total, count
}

// us14dCopyFile 复制一个普通文件并设定权限。
// us14dCopyFile copies one regular file and sets its mode.
func us14dCopyFile(t *testing.T, src, dst string, perm os.FileMode) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("复制 %s 失败: %v", src, err)
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("创建 %s 的父目录失败: %v", dst, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		t.Fatalf("创建 %s 失败: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatalf("写 %s 失败: %v", dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("关闭 %s 失败: %v", dst, err)
	}
}

// us14dZipTree 把 srcDir 压成一个 zip（相对路径保存，解压即还原发行布局），返回
// 压缩前的总字节与文件数。
// us14dZipTree zips srcDir with relative paths and returns the pre-compression totals.
func us14dZipTree(t *testing.T, srcDir, archivePath string) (int64, int) {
	t.Helper()
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("创建压缩包失败: %v", err)
	}
	writer := zip.NewWriter(archive)
	var (
		raw   int64
		files int
	)
	walkErr := filepath.WalkDir(srcDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Deflate
		target, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		if _, err := io.Copy(target, src); err != nil {
			return err
		}
		raw += info.Size()
		files++
		return nil
	})
	if walkErr != nil {
		_ = writer.Close()
		_ = archive.Close()
		t.Fatalf("遍历 %s 失败: %v", srcDir, walkErr)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("收尾压缩包失败: %v", err)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("关闭压缩包失败: %v", err)
	}
	return raw, files
}

// us14dUnzip 把压缩包解到 destDir，返回解出的文件数；条目路径必须是相对路径且不越界
// （发行包只来自本仓构建，但解包器不因此放宽——一个越界条目就能写到程序目录之外）。
// us14dUnzip extracts an archive and refuses any entry that escapes the destination.
func us14dUnzip(t *testing.T, archivePath, destDir string) int {
	t.Helper()
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("打开压缩包失败: %v", err)
	}
	defer func() { _ = reader.Close() }()

	files := 0
	for _, entry := range reader.File {
		target := filepath.Join(destDir, filepath.FromSlash(entry.Name))
		rel, err := filepath.Rel(destDir, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("压缩包条目越界: %q", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("创建目录 %s 失败: %v", target, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		perm := entry.Mode().Perm()
		if perm == 0 {
			perm = 0o644
		}
		src, err := entry.Open()
		if err != nil {
			t.Fatalf("打开条目 %s 失败: %v", entry.Name, err)
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil {
			_ = src.Close()
			t.Fatalf("创建 %s 失败: %v", target, err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			_ = dst.Close()
			_ = src.Close()
			t.Fatalf("写出 %s 失败: %v", target, err)
		}
		_ = dst.Close()
		_ = src.Close()
		files++
	}
	return files
}

// us14dWaitMCP 轮询 MCP 列表直到 named 条目的 connected 等于 want；超时时带上被
// 测进程的输出——连不上的原因（策略拒绝 / 启动失败 / 握手失败）只在服务端日志里。
// us14dWaitMCP polls until the entry's connected flag matches and attaches the process
// output on timeout: the reason only ever shows up in the server log.
func us14dWaitMCP(t *testing.T, p *groupedProcess, addr, token, name string, want bool,
	timeout time.Duration,
) us14dMCPServer {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last us14dMCPServer
	for time.Now().Before(deadline) {
		server, ok := us14dFindMCPServer(us14dMCPServers(t, addr, token), name)
		if ok {
			last = server
			if server.Connected == want {
				return server
			}
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("等待 MCP 条目 %s 的 connected=%v 超时（%s），最后一次读数 %+v\n%s",
		name, want, timeout, last, p.output())
	return us14dMCPServer{}
}

// TestFullPackage_WindowsInstallSmoke 判 M5 的自动化半：解压 → 首次启动 → 登录 →
// 依赖就位。解压目标刻意带空格（设计 §4.1 第 1 步：路径可含空格）。
// E: the Windows half of M5 — unpack (path with spaces), first start, log in, and
// confirm the bundled dependency is actually running.
func TestFullPackage_WindowsInstallSmoke(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("完整包的 Windows 安装冒烟只在 Windows 上执行；Linux 半归 T118（WSL2 Ubuntu，人工）")
	}
	pkg := us14dAssemblePackage(t)

	installDir := filepath.Join(t.TempDir(), "Program Files", "Basalt 0.1.0")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("创建安装目录失败: %v", err)
	}
	// 先按「发行包自洽」判：解出的文件数与打包时一致，二进制逐字节相同。
	if files := us14dUnzip(t, pkg.ArchivePath, installDir); files != pkg.FileCount {
		t.Fatalf("解压得到 %d 个文件，打包时有 %d 个", files, pkg.FileCount)
	}
	installBinary := filepath.Join(installDir, "basalt"+us14dExeSuffix())
	sum, err := fileSHA256(installBinary)
	if err != nil {
		t.Fatalf("计算解压后二进制哈希失败: %v", err)
	}
	if sum != pkg.BinarySHA256 {
		t.Fatalf("解压后的二进制与打包前不一致：打包 %s，解出 %s", pkg.BinarySHA256[:12], sum[:12])
	}

	// 安装目录就是程序目录：serve 以它为 cwd，工作区（`.basalt/`）与上游运行时
	// （`runtime/`）都以它为准——与用户解压后双击运行的形态一致。
	h := &harness{
		Root:         installDir,
		Binary:       installBinary,
		BinarySHA256: pkg.BinarySHA256,
		runTimeout:   defaultRunTimeout,
	}
	us14dRequireFreePort(t, us14dDefaultAddr)
	dataRoot := t.TempDir()
	p := startGrouped(t, h, us14dPrivateEnv(dataRoot), "serve")
	waitHealthHTTP(t, p, us14dDefaultAddr, 120*time.Second)

	// 首次启动：配置、名册与访问密码三件齐备（全新实例，工作区里没有 basalt.yaml）。
	configText := readText(t, filepath.Join(installDir, "basalt.yaml"))
	if !strings.Contains(configText, "addr: 127.0.0.1:8080") {
		t.Fatalf("首次启动生成的配置应监听 127.0.0.1:8080：\n%s", configText)
	}
	token := us14dGeneratedAdminToken(t, dataRoot, us14dInstanceID(t, h))

	// 登录：生成的访问密码可用，错误密码被拒（证明走的是 token 模式，不是本机信任）。
	rows := us14dProfiles(t, us14dDefaultAddr, token)
	for _, name := range us14dBuiltinAgentNames {
		if _, ok := us14dFindProfile(rows, name); !ok {
			t.Fatalf("安装后 /profiles 缺少内置 Agent %s", name)
		}
	}
	wrong := webRequest(t, us14dDefaultAddr, http.MethodGet, "/api/v1/profiles", "",
		us14dAuthHeader("wrong-password"))
	requireWebStatus(t, wrong, http.StatusUnauthorized, "UNAUTHORIZED", "管理凭据缺失或无效", "错误访问密码")

	// 管理台静态壳：随包发布的 dist 由二进制内嵌，解压后不需要额外静态目录。
	console := webRequest(t, us14dDefaultAddr, http.MethodGet, "/console", "", nil)
	requireWebStatus(t, console, http.StatusOK, "", "", "打开管理台")
	if !strings.Contains(strings.ToLower(console.Raw), "<!doctype html") {
		t.Fatalf("管理台首页不是 HTML 壳：%.200s", console.Raw)
	}

	// 依赖就位：随包 docs-readonly 被写进配置、已发布，并且**真的连上了**。
	mcpText := strings.ReplaceAll(readText(t, filepath.Join(installDir, ".basalt", "mcp_servers.yaml")), `\`, "/")
	wantCommand := "runtime/mcp/docs-readonly/" + us14dDocsReadOnlyVersion + "/docs-readonly" + us14dExeSuffix()
	if !strings.Contains(mcpText, wantCommand) {
		t.Fatalf("安装器应把 docs-readonly 指向发行布局 %q：\n%s", wantCommand, mcpText)
	}
	docs := us14dWaitMCP(t, p, us14dDefaultAddr, token, "docs-readonly", true, 60*time.Second)
	if docs.PublishedVersionID <= 0 {
		t.Fatalf("docs-readonly 已连接却没有发布版本：%+v", docs)
	}
	// 未随包的组件如实缺席：dbhub 已成条目并发布，但缺 Node 连不上，进程不受影响。
	dbhub := us14dWaitMCP(t, p, us14dDefaultAddr, token, "dbhub", false, 30*time.Second)
	if dbhub.PublishedVersionID <= 0 {
		t.Fatalf("dbhub 条目应已发布（缺的是运行时，不是配置）：%+v", dbhub)
	}

	p.stop(t)

	t.Logf("full-package-install program_dir=%s binary_bytes=%d runtime_bytes=%d files=%d",
		installDir, pkg.BinaryBytes, pkg.RuntimeBytes, pkg.FileCount)
}

// TestFullPackage_LinuxInstallSmoke 判 M5 的 Linux 半（T118）：**Linux 产物**在 **Linux
// 文件系统内**解压 → 首次启动 → 登录 → 依赖就位，并核对 Windows 上看不到的三件读数——
// 私密目录与文件的权限位、实例锁互斥、优雅停机后无残留子进程。用例只在 linux 上执行，
// Windows 上跳过（另一半是 TestFullPackage_WindowsInstallSmoke）；WSL2 Ubuntu 的实际执行
// 命令与发行版读数记在 run-state，单条绿态**不**扩大为"所有 Linux 部署形态均已验证"。
// E: the Linux half of M5 — a Linux artifact unpacked inside a Linux filesystem, first
// start, login, dependency up, plus the three Linux-only readings: private file modes,
// instance-lock exclusion, and no orphaned children after a graceful stop.
func TestFullPackage_LinuxInstallSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("完整包的 Linux 安装冒烟只在 Linux（含 WSL2 Ubuntu）上执行；Windows 半见 TestFullPackage_WindowsInstallSmoke")
	}
	pkg := us14dAssemblePackage(t)

	// 安装根放在用户主目录下：`/mnt/**` 是 Windows 盘挂载（drvfs），权限位与锁语义都
	// 不代表 Linux——在那里跑出来的绿态没有证据价值，直接判红并点名路径。
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("定位用户主目录失败: %v", err)
	}
	base, err := os.MkdirTemp(home, "basalt-linux-install-")
	if err != nil {
		t.Fatalf("在用户主目录下建安装根失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if strings.HasPrefix(base, "/mnt/") {
		t.Fatalf("安装根落在 Windows 挂载盘（%s）——Linux 安装冒烟必须在 Linux 文件系统内执行", base)
	}
	installDir := filepath.Join(base, "Basalt Install", "Basalt 0.1.0")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatalf("创建安装目录失败: %v", err)
	}
	if files := us14dUnzip(t, pkg.ArchivePath, installDir); files != pkg.FileCount {
		t.Fatalf("解压得到 %d 个文件，打包时有 %d 个", files, pkg.FileCount)
	}
	installBinary := filepath.Join(installDir, "basalt"+us14dExeSuffix())
	sum, err := fileSHA256(installBinary)
	if err != nil {
		t.Fatalf("计算解压后二进制哈希失败: %v", err)
	}
	if sum != pkg.BinarySHA256 {
		t.Fatalf("解压后的二进制与打包前不一致：打包 %s，解出 %s", pkg.BinarySHA256[:12], sum[:12])
	}

	h := &harness{
		Root:         installDir,
		Binary:       installBinary,
		BinarySHA256: pkg.BinarySHA256,
		runTimeout:   defaultRunTimeout,
	}
	us14dRequireFreePort(t, us14dDefaultAddr)
	dataRoot := filepath.Join(base, "data")
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		t.Fatalf("创建数据根失败: %v", err)
	}
	p := startGrouped(t, h, us14dPrivateEnv(dataRoot), "serve")
	waitHealthHTTP(t, p, us14dDefaultAddr, 120*time.Second)

	configPath := filepath.Join(installDir, "basalt.yaml")
	configText := readText(t, configPath)
	if !strings.Contains(configText, "addr: 127.0.0.1:8080") {
		t.Fatalf("首次启动生成的配置应监听 127.0.0.1:8080：\n%s", configText)
	}
	instanceID := us14dInstanceID(t, h)
	token := us14dGeneratedAdminToken(t, dataRoot, instanceID)

	rows := us14dProfiles(t, us14dDefaultAddr, token)
	for _, name := range us14dBuiltinAgentNames {
		if _, ok := us14dFindProfile(rows, name); !ok {
			t.Fatalf("安装后 /profiles 缺少内置 Agent %s", name)
		}
	}
	wrong := webRequest(t, us14dDefaultAddr, http.MethodGet, "/api/v1/profiles", "",
		us14dAuthHeader("wrong-password"))
	requireWebStatus(t, wrong, http.StatusUnauthorized, "UNAUTHORIZED", "管理凭据缺失或无效", "错误访问密码")

	// 依赖就位：随包 docs-readonly 已发布并真的连上；未随包的 dbhub 发布但不连接。
	docs := us14dWaitMCP(t, p, us14dDefaultAddr, token, "docs-readonly", true, 60*time.Second)
	if docs.PublishedVersionID <= 0 {
		t.Fatalf("docs-readonly 已连接却没有发布版本：%+v", docs)
	}
	dbhub := us14dWaitMCP(t, p, us14dDefaultAddr, token, "dbhub", false, 30*time.Second)
	if dbhub.PublishedVersionID <= 0 {
		t.Fatalf("dbhub 条目应已发布（缺的是运行时，不是配置）：%+v", dbhub)
	}

	// Linux 专属 ①：实例私有目录 0700、主密钥与访问密码 0600（设计 §2.4.2 的 Unix 边）。
	privateDir := filepath.Join(dataRoot, "basalt", "instances", instanceID)
	us14dRequireMode(t, privateDir, 0o700, "实例私有目录")
	us14dRequireMode(t, filepath.Join(privateDir, "master.key"), 0o600, "主密钥文件")
	us14dRequireMode(t, filepath.Join(privateDir, "admin-token"), 0o600, "访问密码文件")

	// Linux 专属 ②：实例锁互斥——同一工作区的第二个 serve 被拒，且不改配置文件。
	second := exec.Command(installBinary, "serve")
	second.Dir = installDir
	second.Env = us14dPrivateEnv(dataRoot)
	secondOut, secondErr := second.CombinedOutput()
	if secondErr == nil {
		t.Fatalf("同一工作区的第二个 serve 应被实例锁拒绝，实际成功退出：\n%s", secondOut)
	}
	if !strings.Contains(string(secondOut), "实例锁已被占用") {
		t.Fatalf("第二个 serve 的拒绝原因应点名实例锁：\n%s", secondOut)
	}
	if after := readText(t, configPath); after != configText {
		t.Fatalf("被锁拒绝的进程改写了 basalt.yaml")
	}

	// Linux 专属 ③：优雅停机（SIGINT）以 0 退出，且随包子进程不残留。
	p.stop(t)
	componentPath := filepath.Join(installDir, "runtime", "mcp", "docs-readonly",
		us14dDocsReadOnlyVersion, "docs-readonly"+us14dExeSuffix())
	us14dRequireNoProcess(t, componentPath)

	t.Logf("full-package-linux-install program_dir=%s binary_bytes=%d runtime_bytes=%d files=%d "+
		"instance_id=%s", installDir, pkg.BinaryBytes, pkg.RuntimeBytes, pkg.FileCount, instanceID)
}

// us14dRequireMode 断言路径的 Unix 权限位等于 want（只判 perm 三位，不看 setuid 等）。
// us14dRequireMode asserts the Unix permission bits equal want.
func us14dRequireMode(t *testing.T, path string, want os.FileMode, what string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取%s %s 失败: %v", what, path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s %s 的权限位 = %04o，期望 %04o", what, path, got, want)
	}
}

// us14dRequireNoProcess 轮询 `/proc`，直到没有进程的命令行包含 needle 为止；超时判红并
// 列出仍活着的命令行——"退出码 0 但子进程留着"是 Linux 上最容易漏掉的一类残留。
// us14dRequireNoProcess polls /proc until no command line mentions needle anymore.
func us14dRequireNoProcess(t *testing.T, needle string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var survivors []string
	for time.Now().Before(deadline) {
		survivors = survivingCommandLines(needle)
		if len(survivors) == 0 {
			return
		}
		time.Sleep(readinessPoll)
	}
	t.Fatalf("停机后仍有进程引用 %s：\n%s", needle, strings.Join(survivors, "\n"))
}

// survivingCommandLines 扫 `/proc/<pid>/cmdline`，返回命令行里包含 needle 的进程。
// survivingCommandLines scans /proc for processes whose command line contains needle.
func survivingCommandLines(needle string) []string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name()[0] < '0' || entry.Name()[0] > '9' {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		line := strings.ReplaceAll(string(raw), "\x00", " ")
		if strings.Contains(line, needle) {
			found = append(found, "pid="+entry.Name()+" "+strings.TrimSpace(line))
		}
	}
	return found
}

// TestFullPackage_ReportsMeasuredSize 判体积是**实测**而不是估算：读数的每一条都能
// 对回盘上的字节数，且各分量之和等于整棵目录树——既不漏统计，也不重复统计。
// E: the reported size must be measured — every component ties back to bytes on disk
// and the components sum to the tree total.
func TestFullPackage_ReportsMeasuredSize(t *testing.T) {
	pkg := us14dAssemblePackage(t)

	if pkg.RawBytes <= 0 || pkg.FileCount < 2 {
		t.Fatalf("包内容不合理：raw=%d files=%d", pkg.RawBytes, pkg.FileCount)
	}
	// 二进制分量必须等于盘上该文件的真实大小（不是常量、不是估算）。
	onDisk, err := os.Stat(pkg.BinaryPath)
	if err != nil {
		t.Fatalf("读取二进制失败: %v", err)
	}
	if pkg.BinaryBytes != onDisk.Size() {
		t.Fatalf("二进制字节数 = %d，盘上实际 %d", pkg.BinaryBytes, onDisk.Size())
	}
	// 分量之和 == 整棵树：否则"实测"漏掉了某个文件。
	if pkg.BinaryBytes+pkg.RuntimeBytes != pkg.RawBytes {
		t.Fatalf("分量之和 %d 与整棵树 %d 不符——体积统计漏项或重复",
			pkg.BinaryBytes+pkg.RuntimeBytes, pkg.RawBytes)
	}
	if pkg.RuntimeBytes <= 0 {
		t.Fatalf("runtime/ 分量应大于 0，实际 %d（随包组件未打进包）", pkg.RuntimeBytes)
	}
	// 压缩包确实是压缩产物：既非空，也小于原始字节。
	if pkg.ArchiveBytes <= 0 || pkg.ArchiveBytes >= pkg.RawBytes {
		t.Fatalf("压缩包字节数 = %d，原始 %d——不是有效的压缩产物", pkg.ArchiveBytes, pkg.RawBytes)
	}
	// 条目数与打包时一致：解包方据此判完整性，这里先自证。
	reader, err := zip.OpenReader(pkg.ArchivePath)
	if err != nil {
		t.Fatalf("打开压缩包失败: %v", err)
	}
	entries := 0
	for _, entry := range reader.File {
		if !entry.FileInfo().IsDir() {
			entries++
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("关闭压缩包失败: %v", err)
	}
	if entries != pkg.FileCount {
		t.Fatalf("压缩包条目 %d 与打包文件数 %d 不符", entries, pkg.FileCount)
	}

	t.Logf("full-package-size binary_bytes=%d runtime_bytes=%d raw_bytes=%d archive_bytes=%d files=%d "+
		"ratio=%.2f", pkg.BinaryBytes, pkg.RuntimeBytes, pkg.RawBytes, pkg.ArchiveBytes, pkg.FileCount,
		float64(pkg.ArchiveBytes)/float64(pkg.RawBytes))
}
