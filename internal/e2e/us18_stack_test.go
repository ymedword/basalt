//go:build e2e

package e2e

// US-18 容器验收的栈夹具：真实镜像与真实 Compose，独立项目 / 卷 / 端口。
// 模块卡 §3.1 把本文件定义为 composeStack 及全部方法的落点；夹具内部另加三个实现
// 细节（见 run-state 失败台账）：`t` 供 restore 注册清理、`lastOutput` 保留最近一次
// compose 输出供失败断言、`token` 缓存管理面令牌。
// The US-18 stack fixture: a real image and real Compose under an isolated project,
// volumes and port. composeStack and every method live here per module card §3.1.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ymedword/basalt/scripts/releasekit"
)

// 容器 e2e 的固定事实：入口限流六项按「> 0」的启动前置条件给足；测试版本见下方
// 随机版本说明。
// Fixed container-e2e facts: non-zero ingress values (serve refuses to start with any
// at zero); test versions are random per run (see below).
const (
	us18ProjectPrefixEnv = "BASALT_E2E_PROJECT_PREFIX"
	us18RequireDockerEnv = "BASALT_E2E_REQUIRE_DOCKER"
	us18KeepEnv          = "BASALT_US18_KEEP"
	us18DefaultPrefix    = "us18"

	// us18ContainerPort 是容器内固定监听端口：compose 把它映射到随机宿主端口。
	// The container always listens on 8080; compose maps it to a random host port.
	us18ContainerPort = 8080

	us18IngressQPS   = "50"
	us18IngressBurst = "50"

	// us18FakeKey 是测试 overlay 注入的一次性占位值，不是任何真实凭证。
	// us18FakeKey is the throwaway placeholder the test overlay injects.
	us18FakeKey = "us18-fake-key-not-a-secret"
)

// 测试版本每次运行随机生成合法 semver：releasekit 把镜像 tag 固定推导为
// basalt:<version>-<linux>-<amd64>，版本唯一即 tag 唯一——测试因此不会覆盖宿主
// 已有的同名镜像标签（例如正式发布留下的 basalt:0.0.1-linux-amd64）；清理也只删
// 本次构建登记过 ID 的 tag。
// Test versions are random valid semver per run: releasekit derives the tag
// basalt:<version>-<os>-<arch>, so a unique version means a unique tag that cannot
// shadow a host image, and cleanup deletes only tags whose recorded ID still matches.
var (
	us18VersionSeed    = us18NewVersionSeed()
	us18DefaultVersion = us18VersionAt(0)
	us18UpgradeVersion = us18VersionAt(1)
)

// us18NewVersionSeed 起随机种子：panic 即夹具环境不可用（与随机项目名同一口径）。
// us18NewVersionSeed seeds the per-run version; a panic means the fixture host is
// unusable, matching the random project-name helper.
func us18NewVersionSeed() int {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		panic("e2e us18: 读取随机源失败: " + err.Error())
	}
	return (int(buf[0])<<24 | int(buf[1])<<16 | int(buf[2])<<8 | int(buf[3])) % 800000000
}

// us18VersionAt 返回本次运行独有的 X.Y.Z；offset 区分同一次运行的不同版本。
// us18VersionAt returns a per-run-unique X.Y.Z; offset separates versions of one run.
func us18VersionAt(offset int) string {
	return fmt.Sprintf("0.0.%d", 100000000+us18VersionSeed+offset)
}

// us18ImageTag 配对测试镜像 tag 与本次构建产出的局部 ID：清理时只有 tag 仍指向
// 这个 ID（归属核对通过）才删除，避免删掉宿主后来指向别的镜像的同名 tag。
// us18ImageTag pairs a test tag with the local ID this run produced; cleanup removes
// the tag only while it still points at that ID.
type us18ImageTag struct {
	tag string
	id  string
}

// us18Artifacts 记录本次进程构建过的镜像 tag 与临时目录；TestMain 只清理这些。
// us18Artifacts records the image tags and temp dirs this process created; TestMain
// removes only these.
var us18Artifacts = struct {
	mu   sync.Mutex
	dirs []string
	tags []us18ImageTag
}{}

// us18RecordDir 登记一个需要清理的临时目录。
// us18RecordDir registers a temp dir for cleanup.
func us18RecordDir(dir string) {
	us18Artifacts.mu.Lock()
	defer us18Artifacts.mu.Unlock()
	if dir != "" {
		us18Artifacts.dirs = append(us18Artifacts.dirs, dir)
	}
}

// us18RecordImageTag 登记本次构建的镜像 tag 与其局部 ID（清理归属判据）。
// us18RecordImageTag registers the tag and local ID of one build (the cleanup
// ownership verdict).
func us18RecordImageTag(tag, id string) {
	us18Artifacts.mu.Lock()
	defer us18Artifacts.mu.Unlock()
	if tag != "" {
		us18Artifacts.tags = append(us18Artifacts.tags, us18ImageTag{tag: tag, id: id})
	}
}

// us18CleanupImages 由 TestMain 在 m.Run 之后调用：按归属删除本次构建的镜像 tag
// 与临时目录，并用 `docker image inspect` 复检每个 tag 确已消失——残留即返回错误，
// 由 TestMain 并入退出码（A15 的镜像类判据）。
// `BASALT_US18_KEEP=1` 时保留现场，便于人工排障。
// us18CleanupImages removes the tags and temp dirs built by this run after an ownership
// check and re-checks with `docker image inspect` that each tag is gone; any leftover is
// returned and folded into TestMain's exit code (the image arm of A15).
// BASALT_US18_KEEP=1 keeps the scene for debugging.
func us18CleanupImages() error {
	if os.Getenv(us18KeepEnv) == "1" {
		return nil
	}
	us18Artifacts.mu.Lock()
	dirs := append([]string(nil), us18Artifacts.dirs...)
	tags := append([]us18ImageTag(nil), us18Artifacts.tags...)
	us18Artifacts.mu.Unlock()
	// 同一 tag 可能被本进程构建多次（A1 两次构建）：以最后一次登记的 ID 为归属，
	// 更早的镜像已被同 tag 覆盖，按“已不再指向该 ID”跳过。
	// One tag may be built several times in one process (A1 builds twice): the last
	// registered ID owns the tag; earlier images were already replaced.
	last := make(map[string]string, len(tags))
	for _, tag := range tags {
		last[tag.tag] = tag.id
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var failures []error
	for tag, id := range last {
		removed, note, err := us18RemoveImageTag(ctx, tag, id)
		switch {
		case err != nil:
			failures = append(failures, err)
		case !removed && note != "":
			fmt.Fprintf(os.Stderr, "e2e: %s\n", note)
		}
	}
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			failures = append(failures, fmt.Errorf("清理临时目录 %s 失败: %w", dir, err))
		}
	}
	return errors.Join(failures...)
}

// us18ImageID 返回 tag 当前指向的镜像 ID；tag 不存在时 ok=false。
// us18ImageID returns the image ID the tag currently points at; ok=false when the tag
// does not exist.
func us18ImageID(ctx context.Context, tag string) (string, bool) {
	out, err := us18DockerOutput(ctx, "image", "inspect", "--format", "{{.Id}}", tag)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// us18RemoveImageTag 按归属删除一个测试镜像 tag：先 inspect 当前 ID，只有与本次
// 构建登记的 ID 一致才执行 `docker image rm`。返回值：
//   - tag 已不存在 → removed=false, note="", err=nil；
//   - 当前 ID 与登记不符 → removed=false, note=保留说明, err=nil（不删别人的镜像）；
//   - 缺少登记 ID → err（没有归属证据就拒绝删除）；
//   - 删除失败或删除后仍能 inspect 到 → err（A15 红态）。
//
// us18RemoveImageTag deletes a test tag only while it still points at the ID this run
// built; a tag re-pointed by someone else is left untouched and explained.
func us18RemoveImageTag(ctx context.Context, tag, expectedID string) (bool, string, error) {
	if expectedID == "" {
		return false, "", fmt.Errorf("镜像 %s 缺少登记 ID，拒绝删除（无法核对归属）", tag)
	}
	currentID, exists := us18ImageID(ctx, tag)
	if !exists {
		return false, "", nil
	}
	if currentID != expectedID {
		return false, fmt.Sprintf("镜像 %s 现指向 %s，不是本次构建的 %s，保留不动",
			tag, currentID, expectedID), nil
	}
	if _, err := us18DockerOutput(ctx, "image", "rm", tag); err != nil {
		// rm 报错可能只是标签已被删掉；是否存在以 inspect 的事实为准。
		// The rm error may only mean the tag is already gone; inspect is the arbiter.
		if _, still := us18ImageID(ctx, tag); still {
			return false, "", fmt.Errorf("删除测试镜像 %s 失败：%w", tag, err)
		}
		return true, "", nil
	}
	if _, still := us18ImageID(ctx, tag); still {
		return false, "", fmt.Errorf("镜像 %s 在清理后仍存在", tag)
	}
	return true, "", nil
}

// us18RepoRoot 包装既有 repoRoot()（模块卡 §3.1 的 repoRoot(t) 与既有符号重名，
// run-state 记录了改名为 us18RepoRoot 的偏离）。
// us18RepoRoot wraps the existing repoRoot helper; the module card's repoRoot(t) name
// collides with it, and run-state records the wrapper.
func us18RepoRoot(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("定位仓库根失败: %v", err)
	}
	return root
}

// mustImageLock 读并校验 release/image-lock.json。
// mustImageLock reads and validates the image lock file.
func mustImageLock(t *testing.T, repo string) releasekit.ImageLock {
	t.Helper()
	lock, err := releasekit.ReadImageLock(filepath.Join(repo, "release", "image-lock.json"))
	if err != nil {
		t.Fatalf("读取镜像锁文件失败: %v", err)
	}
	return lock
}

// us18RequireDocker 在缺 daemon 时按 `BASALT_E2E_REQUIRE_DOCKER=1` 决定失败还是跳过：
// CI 必须红，本地缺环境如实记跳过，不冒充通过。
// us18RequireDocker fails under BASALT_E2E_REQUIRE_DOCKER=1 and skips otherwise, so a
// missing daemon is never reported as a pass.
func us18RequireDocker(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := us18DockerOutput(ctx, "info", "--format", "{{.ServerVersion}}")
	if err == nil {
		return
	}
	if os.Getenv(us18RequireDockerEnv) == "1" {
		t.Fatalf("容器 e2e 需要本机 Docker（%s）：%v", us18RequireDockerEnv, err)
	}
	t.Skipf("本机没有可用的 Docker daemon，容器 e2e 跳过并登记未验证：%v（%s）",
		err, strings.TrimSpace(out))
}

// us18DockerOutput 运行 docker 并把 stdout / stderr 合并进错误信息。
// us18DockerOutput runs docker and folds stderr into the error.
func us18DockerOutput(ctx context.Context, args ...string) (string, error) {
	// 子命令与参数全部由夹具常量构造，不含外部自由文本。
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s 失败：%w（%s）",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// us18DockerLogs 读取容器日志并合并两路输出：容器的 stderr 在 `docker logs` 里仍是
// stderr，而 us18DockerOutput 成功时只回 stdout——只读一路会把「有日志」读成空串
// （A14 实测）。判据因此固定走本助手。
// us18DockerLogs merges both streams: container stderr stays on stderr in `docker logs`,
// while us18DockerOutput returns stdout only on success.
func us18DockerLogs(ctx context.Context, name string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "logs", name)
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Run(); err != nil {
		return combined.String(), fmt.Errorf("docker logs %s 失败：%w（%s）",
			name, err, strings.TrimSpace(combined.String()))
	}
	return combined.String(), nil
}

// us18EnsureImage 确认镜像在本地；缺失时按 §4.6 的授权匿名拉取固定 digest 公共镜像，
// 拉取也失败则按 REQUIRE_DOCKER 决定失败或跳过。
// us18EnsureImage makes the image local; the anonymous pull of a fixed public digest
// is explicitly authorized by module card §4.6.
func us18EnsureImage(t *testing.T, ref string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := us18DockerOutput(ctx, "image", "inspect", ref); err == nil {
		return
	}
	pullCtx, pullCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer pullCancel()
	out, err := us18DockerOutput(pullCtx, "pull", ref)
	if err == nil {
		return
	}
	if os.Getenv(us18RequireDockerEnv) == "1" {
		t.Fatalf("本地缺少镜像 %s 且拉取失败：%v", ref, err)
	}
	t.Skipf("本地缺少镜像 %s 且匿名拉取失败，容器 e2e 跳过并登记未验证：%v（%s）",
		ref, err, strings.TrimSpace(out))
}

// us18ImageCache 缓存按版本构建的镜像 tag 与二进制路径：同一次 go test 进程里
// 多个用例复用同一次构建。
// us18ImageCache reuses one image and binary build per version inside the test binary.
var us18ImageCache = struct {
	mu       sync.Mutex
	images   map[string]string
	binaries map[string]string
}{images: map[string]string{}, binaries: map[string]string{}}

// buildLinuxAMD64Binary 以 US-17 的发布构建入口产出 linux/amd64 二进制。
// buildLinuxAMD64Binary produces the linux/amd64 binary through the US-17 build seam.
func buildLinuxAMD64Binary(t *testing.T) string {
	t.Helper()
	return us18BinaryFor(t, us18DefaultVersion)
}

// us18BinaryFor 构建（或复用）指定版本的 linux/amd64 二进制。
// us18BinaryFor builds or reuses the linux/amd64 binary of one version.
func us18BinaryFor(t *testing.T, version string) string {
	t.Helper()
	us18ImageCache.mu.Lock()
	path, ok := us18ImageCache.binaries[version]
	us18ImageCache.mu.Unlock()
	if ok {
		return path
	}
	root := us18RepoRoot(t)
	dir, err := os.MkdirTemp("", "us18-bin-"+version+"-")
	if err != nil {
		t.Fatalf("创建二进制输出目录失败: %v", err)
	}
	us18RecordDir(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	result, err := releasekit.BuildTarget(ctx, releasekit.Inputs{
		Repo:      root,
		Version:   version,
		Mode:      releasekit.ModeRehearsal,
		StartedAt: time.Now().UTC(),
	}, releasekit.Target{GOOS: "linux", GOARCH: "amd64", Tier: releasekit.TierBinary}, dir)
	if err != nil {
		t.Fatalf("构建 linux/amd64 二进制失败: %v", err)
	}
	us18ImageCache.mu.Lock()
	us18ImageCache.binaries[version] = result.Binary
	us18ImageCache.mu.Unlock()
	return result.Binary
}

// us18ImageFor 返回指定版本的镜像 tag（首次调用时构建并登记清理）。
// us18ImageFor returns the image tag of one version, building it on first use.
func us18ImageFor(t *testing.T, version string) string {
	t.Helper()
	us18ImageCache.mu.Lock()
	tag, ok := us18ImageCache.images[version]
	us18ImageCache.mu.Unlock()
	if ok {
		return tag
	}
	root := us18RepoRoot(t)
	lock := mustImageLock(t, root)
	us18EnsureImage(t, lock.AppBase)
	record := us18BuildImageOnce(t, version, lock.AppBase)
	us18ImageCache.mu.Lock()
	us18ImageCache.images[version] = record.Tag
	us18ImageCache.mu.Unlock()
	return record.Tag
}

// us18BuildImageOnce 用给定的基础镜像引用构建一次镜像记录；A1 需要同输入两次构建，
// 因此不复用缓存。产物目录登记给 TestMain 清理。
// us18BuildImageOnce builds one image record from explicit inputs; A1 needs two builds
// from the same inputs, so this helper stays uncached.
func us18BuildImageOnce(t *testing.T, version, baseRef string) releasekit.ImageRecord {
	t.Helper()
	root := us18RepoRoot(t)
	outDir, err := os.MkdirTemp("", "us18-image-"+version+"-")
	if err != nil {
		t.Fatalf("创建镜像产物目录失败: %v", err)
	}
	us18RecordDir(outDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	record, err := releasekit.BuildImage(ctx, releasekit.ImageInputs{
		Repo:       root,
		Version:    version,
		Target:     releasekit.Target{GOOS: "linux", GOARCH: "amd64", Tier: releasekit.TierBinary},
		Binary:     us18BinaryFor(t, version),
		BaseRef:    baseRef,
		Dockerfile: filepath.Join(root, "docker", "Dockerfile"),
		Entrypoint: filepath.Join(root, "docker", "entrypoint.sh"),
		ConfigFile: filepath.Join(root, "docker", "basalt.yaml"),
		License:    filepath.Join(root, "LICENSE"),
		// US-25 起镜像上下文还带 Worker 模板与两个运维脚本；US-18 的镜像判据不变。
		// Since US-25 the context also carries the worker template and both operator
		// scripts; US-18's image assertions stay unchanged.
		WorkerConfigFile: filepath.Join(root, "docker", "basalt.worker.yaml"),
		ExportKeyScript:  filepath.Join(root, "docker", "export-master-key.sh"),
		SeedScript:       filepath.Join(root, "docker", "seed-workspace.sh"),
		OutDir:           outDir,
	})
	if err != nil {
		t.Fatalf("构建镜像 %s 失败: %v", version, err)
	}
	us18RecordImageTag(record.Tag, record.LocalID)
	return record
}

// composeStack 是一套独立 Compose 项目：独立项目名、独立卷、独立宿主端口、
// 独立 secrets 目录。字段与模块卡 §3.1 一致；`t` 供 restore 注册新栈的清理。
// composeStack is one isolated Compose project; `t` lets restore register the
// destination stack's cleanup.
type composeStack struct {
	project    string
	dir        string
	secretsDir string
	env        map[string]string
	port       int

	t          *testing.T
	token      string // 管理面令牌缓存：A7 要证明「旧 token 仍可用」 / cached admin token
	lastOutput string // 最近一次 compose 输出：失败断言要读它 / last compose output
}

// newComposeStack 准备一套可启动的栈：暂存 compose 资产、写缺省 secret 并校正属主、
// 取随机宿主端口与项目名。缺 daemon / 基础镜像 / 应用镜像时按 REQUIRE_DOCKER 决定
// 失败或跳过。
// newComposeStack stages the compose assets, seeds default secrets with their owners,
// and picks a random project name and host port.
func newComposeStack(t *testing.T, name string) *composeStack {
	t.Helper()
	us18RequireDocker(t)
	if runtime.GOOS != "linux" {
		// 夹具用宿主目录挂进容器设置属主，Windows 路径映射不在本 US 的验收面内。
		// The fixture mounts host dirs into containers; Windows path mapping is out of
		// this US's verified surface.
		if os.Getenv(us18RequireDockerEnv) == "1" {
			t.Fatalf("容器 e2e 只在 Linux 宿主运行（当前 %s）", runtime.GOOS)
		}
		t.Skipf("容器 e2e 只在 Linux 宿主运行（当前 %s），跳过并登记未验证", runtime.GOOS)
	}
	root := us18RepoRoot(t)
	lock := mustImageLock(t, root)
	us18EnsureImage(t, lock.Postgres)
	image := us18ImageFor(t, us18DefaultVersion)

	dir := us18StageAssets(t, root)
	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil { // 容器要能读目录
		t.Fatalf("创建 secrets 目录失败: %v", err)
	}
	port := us18FreePort(t)
	stack := &composeStack{
		t:          t,
		project:    us18ProjectName(name),
		dir:        dir,
		secretsDir: secretsDir,
		port:       port,
		env: map[string]string{
			"BASALT_IMAGE":                image,
			"BASALT_HTTP_BIND":            "127.0.0.1",
			"BASALT_HTTP_PORT":            strconv.Itoa(port),
			"BASALT_INGRESS_RUN_QPS":      us18IngressQPS,
			"BASALT_INGRESS_RUN_BURST":    us18IngressBurst,
			"BASALT_INGRESS_INGEST_QPS":   us18IngressQPS,
			"BASALT_INGRESS_INGEST_BURST": us18IngressBurst,
			"BASALT_INGRESS_READ_QPS":     us18IngressQPS,
			"BASALT_INGRESS_READ_BURST":   us18IngressBurst,
			"BASALT_US18_FAKE_KEY":        us18FakeKey,
		},
	}
	password := us18RandomHex(t, 16)
	stack.writeSecret(t, "postgres_password", password)
	stack.writeSecret(t, "database_dsn",
		"postgres://basalt:"+password+"@postgres:5432/basalt?sslmode=disable")
	if err := stack.fixSecretOwnership(context.Background()); err != nil {
		t.Fatalf("设置 secret 属主失败: %v", err)
	}
	t.Cleanup(func() { stack.us18CleanupStack() })
	return stack
}

// us18CleanupStack 在用例结束时收掉整个项目：失败时先留一份容器日志，再删卷。
// us18CleanupStack brings the project down, keeping a log tail when the test failed.
func (s *composeStack) us18CleanupStack() {
	if os.Getenv(us18KeepEnv) == "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if s.t.Failed() {
		if logs, err := s.logs(ctx, "basalt"); err == nil && strings.TrimSpace(logs) != "" {
			s.t.Logf("US-18 现场：项目 %s 的 basalt 容器日志（尾部）\n%s",
				s.project, us18Tail(logs, 4000))
		}
	}
	if err := s.down(ctx, true); err != nil {
		s.t.Logf("US-18 清理项目 %s 失败：%v", s.project, err)
	}
}

// us18Tail 保留文本尾部（容器日志可能很长）。
// us18Tail keeps the tail of a text.
func us18Tail(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return "…" + text[len(text)-limit:]
}

// up 启动整套服务并等待健康。
// up starts the whole stack and waits for health.
func (s *composeStack) up(ctx context.Context, extra ...string) error {
	args := []string{"up", "-d", "--wait", "--wait-timeout", "180"}
	args = append(args, extra...)
	out, err := s.compose(ctx, args...)
	s.lastOutput = out
	if err != nil {
		return fmt.Errorf("compose up：%w（%s）", err, out)
	}
	return nil
}

// upExpectFailure 断言启动失败且目标服务以非零码退出：compose 退出 0 时也判红
// （"什么都没发生"不是被拒绝）。
// upExpectFailure requires a failed up AND a non-zero exit of the target service;
// a zero exit from compose is itself a red outcome.
func (s *composeStack) upExpectFailure(ctx context.Context) error {
	out, err := s.compose(ctx, "up", "-d", "--wait", "--wait-timeout", "60")
	s.lastOutput = out
	if err == nil {
		return fmt.Errorf("compose up 必须失败，实际退出 0：%s", out)
	}
	code, codeErr := s.serviceExitCode(ctx, "basalt")
	if codeErr != nil {
		return fmt.Errorf("compose up 失败，但读不到 basalt 的退出码：%v（%s）", codeErr, out)
	}
	if code == 0 {
		return fmt.Errorf("compose up 失败，但 basalt 退出码为 0——不是被拒绝：%s", out)
	}
	return nil
}

// serviceExitCode 读取服务容器的退出码；没有容器时报错（说明失败发生在创建之前）。
// serviceExitCode reads the service container's exit code; a missing container means
// the failure happened before creation.
func (s *composeStack) serviceExitCode(ctx context.Context, service string) (int, error) {
	out, err := s.compose(ctx, "ps", "-a", "-q", service)
	if err != nil {
		return 0, err
	}
	ids := us18NonEmptyLines(out)
	if len(ids) == 0 {
		return 0, fmt.Errorf("没有 %s 容器", service)
	}
	inspect, err := us18DockerOutput(ctx, "inspect", "--format", "{{.State.ExitCode}}", ids[0])
	if err != nil {
		return 0, err
	}
	code, convErr := strconv.Atoi(strings.TrimSpace(inspect))
	if convErr != nil {
		return 0, fmt.Errorf("解析退出码 %q：%w", strings.TrimSpace(inspect), convErr)
	}
	return code, nil
}

// down 停止并移除项目；removeVolumes 时连卷一起删（A15 的清理判据）。
// down stops and removes the project; removeVolumes also drops the volumes.
func (s *composeStack) down(ctx context.Context, removeVolumes bool) error {
	args := []string{"down", "--remove-orphans", "--timeout", "60"}
	if removeVolumes {
		args = append(args, "-v")
	}
	out, err := s.compose(ctx, args...)
	s.lastOutput = out
	if err != nil {
		return fmt.Errorf("compose down：%w（%s）", err, out)
	}
	return nil
}

// stopService 停一个服务（走 compose 的 stop_grace_period）。
// stopService stops one service through compose's stop_grace_period.
func (s *composeStack) stopService(ctx context.Context, service string) error {
	out, err := s.compose(ctx, "stop", service)
	if err != nil {
		return fmt.Errorf("compose stop %s：%w（%s）", service, err, out)
	}
	return nil
}

// startService 启动先前停掉的服务。
// startService starts a previously stopped service.
func (s *composeStack) startService(ctx context.Context, service string) error {
	out, err := s.compose(ctx, "start", service)
	if err != nil {
		return fmt.Errorf("compose start %s：%w（%s）", service, err, out)
	}
	return nil
}

// logs 读取一个服务的日志（无颜色，尾部有上界）。
// logs reads a service's logs without color and with a bounded tail.
func (s *composeStack) logs(ctx context.Context, service string) (string, error) {
	out, err := s.compose(ctx, "logs", "--no-color", "--tail", "4000", service)
	if err != nil {
		return out, fmt.Errorf("compose logs %s：%w（%s）", service, err, out)
	}
	return out, nil
}

// exec 在服务容器内执行命令（-T 关闭伪终端，非交互）。
// exec runs a command inside the service container (-T disables the TTY).
func (s *composeStack) exec(ctx context.Context, service string, argv ...string) (string, error) {
	args := append([]string{"exec", "-T", service}, argv...)
	out, err := s.compose(ctx, args...)
	if err != nil {
		return out, fmt.Errorf("compose exec %s %s：%w（%s）",
			service, strings.Join(argv, " "), err, out)
	}
	return out, nil
}

// adminToken 从实例私有卷读取首启生成的管理面令牌（与主密钥同目录、分文件）。
// adminToken reads the first-boot management token from the instance private volume.
func (s *composeStack) adminToken(ctx context.Context) (string, error) {
	instance, err := s.instanceID(ctx)
	if err != nil {
		return "", err
	}
	out, err := s.exec(ctx, "basalt", "cat", us18AdminTokenPath(instance))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return "", fmt.Errorf("管理面令牌文件为空（实例 %s）", instance)
	}
	return token, nil
}

// instanceID 读工作区名册的实例标识。
// instanceID reads the instance identity from the workspace roster.
func (s *composeStack) instanceID(ctx context.Context) (string, error) {
	out, err := s.exec(ctx, "basalt", "cat", us18RosterPath)
	if err != nil {
		return "", err
	}
	var roster struct {
		Version    int    `json:"version"`
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal([]byte(out), &roster); err != nil {
		return "", fmt.Errorf("解析实例名册失败（%s）：%w", out, err)
	}
	if roster.InstanceID == "" {
		return "", fmt.Errorf("实例名册缺少 instance_id（%s）", out)
	}
	return roster.InstanceID, nil
}

// request 发一次宿主到容器的 HTTP 请求：拨号走随机宿主端口，Host 头固定为模板里的
// 白名单值 127.0.0.1:8080（U18-Q7 不允许为测试新增配置键）。
// request dials the random host port but sends the fixed allowlisted Host header,
// because U18-Q7 forbids new config keys for tests.
func (s *composeStack) request(ctx context.Context, method, path string, body []byte, token string) (*http.Response, []byte, error) {
	return s.requestHeaders(ctx, method, path, body, token, nil)
}

// requestHeaders 是 request 的带附加头版本：管理面的条件写要 `If-Match`，契约签名
// 里没有头参数，故加这一个私有层（run-state 记录为夹具内部补充）。
// requestHeaders adds extra headers for conditional admin writes; the contract's
// request signature has no header parameter, so this private layer carries it.
func (s *composeStack) requestHeaders(ctx context.Context, method, path string, body []byte, token string, headers map[string]string) (*http.Response, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.port, path)
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, nil, err
	}
	req.Host = fmt.Sprintf("127.0.0.1:%d", us18ContainerPort)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := us18HTTPClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp, nil, readErr
	}
	return resp, raw, nil
}

// us18HTTPClient 的超时要盖住 60s 的会话运行与优雅停机窗口。
// The client timeout covers a 60s session run plus the graceful-stop window.
var us18HTTPClient = &http.Client{Timeout: 150 * time.Second}

// backup 用暂存的 backup.sh 做四件套备份；项目名与 COMPOSE_FILE 显式传入。
// backup runs the staged backup.sh with explicit project and COMPOSE_FILE.
func (s *composeStack) backup(ctx context.Context, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建备份目录：%w", err)
	}
	script := filepath.Join(s.dir, "backup.sh")
	env := mergeEnv(s.processEnv(), map[string]string{
		"COMPOSE_PROJECT_NAME": s.project,
		"COMPOSE_FILE":         s.composeFiles(),
	})
	cmd := exec.CommandContext(ctx, "sh", script, dir)
	cmd.Dir = s.dir
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("backup.sh 失败：%w（%s）", err, us18Tail(buf.String(), 4000))
	}
	return nil
}

// restore 用暂存的 restore.sh 把备份恢复到全新项目名，返回目标栈（新端口与新
// secrets 目录）；目标栈的清理也登记到本用例。
// restore restores a backup into a brand-new project and returns the destination
// stack (new port and secrets dir), registering its cleanup as well.
func (s *composeStack) restore(ctx context.Context, dir string) (*composeStack, error) {
	root := us18RepoRoot(s.t)
	destDir := us18StageAssets(s.t, root)
	secretsDir := filepath.Join(destDir, "secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil { // 容器要能读目录
		return nil, fmt.Errorf("创建目标 secrets 目录：%w", err)
	}
	port := us18FreePort(s.t)
	destProject := s.project + "r" + us18RandomHex(s.t, 4)
	destEnv := make(map[string]string, len(s.env))
	for key, value := range s.env {
		destEnv[key] = value
	}
	destEnv["BASALT_HTTP_PORT"] = strconv.Itoa(port)
	dest := &composeStack{
		t:          s.t,
		project:    destProject,
		dir:        destDir,
		secretsDir: secretsDir,
		port:       port,
		env:        destEnv,
	}
	script := filepath.Join(destDir, "restore.sh")
	// 清理必须先于脚本登记：restore.sh 中途失败时卷可能已经建好，而 dest 尚未返回
	// 给调用方——实测失败尝试会在 down -v 之外留下半建项目。
	// Register cleanup before the script runs: a mid-way failure can leave volumes
	// behind while dest has not been returned to the caller yet.
	s.t.Cleanup(func() { dest.us18CleanupStack() })
	// 恢复出来的项目用**目标栈**的环境启动：端口必须换，否则与仍在运行的源栈抢占
	// 同一宿主端口；只有 COMPOSE_PROJECT_NAME（备份来源）与 COMPOSE_FILE 是覆盖值。
	// The restored project starts with the destination env so it does not fight the
	// still-running source for the same host port.
	env := mergeEnv(dest.processEnv(), map[string]string{
		"COMPOSE_PROJECT_NAME": s.project,
		"COMPOSE_FILE":         dest.composeFiles(),
	})
	cmd := exec.CommandContext(ctx, "sh", script, dir, destProject)
	cmd.Dir = destDir
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("restore.sh 失败：%w（%s）", err, us18Tail(buf.String(), 4000))
	}
	return dest, nil
}

// compose 以本栈的项目名与 compose 文件跑一条 docker compose 子命令。
// compose runs one docker compose subcommand under this stack's project and files.
func (s *composeStack) compose(ctx context.Context, args ...string) (string, error) {
	base := []string{
		"compose", "-p", s.project, "-f", filepath.Join(s.dir, "compose.yaml"),
		"-f", filepath.Join(s.dir, "compose.test.yaml"),
	}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...) // 参数由夹具构造
	cmd.Dir = s.dir
	cmd.Env = s.processEnv()
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// composeFiles 返回 COMPOSE_FILE 用的冒号分隔文件表（备份 / 恢复脚本读它）。
// composeFiles returns the colon-separated file list the scripts consume.
func (s *composeStack) composeFiles() string {
	return filepath.Join(s.dir, "compose.yaml") + ":" + filepath.Join(s.dir, "compose.test.yaml")
}

// processEnv 把栈的环境变量覆盖到宿主环境上（同名覆盖，不留重复键）。
// processEnv overlays the stack env on the host environment without duplicate keys.
func (s *composeStack) processEnv() []string {
	return mergeEnv(os.Environ(), s.env)
}

// mergeEnv 返回环境列表，overrides 中的键替换基表里的同名项。
// mergeEnv overlays overrides onto base, replacing same-named entries.
func mergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[name]; overridden {
			continue
		}
		out = append(out, entry)
	}
	for key, value := range overrides {
		out = append(out, key+"="+value)
	}
	return out
}

// us18StageAssets 把 compose 运行所需的资产复制进用例临时目录：compose 文件里的
// 相对路径（./initdb、./secrets）因此都落在这个目录下。
// us18StageAssets copies the runtime assets into the test's temp dir so every relative
// path in the compose files resolves there.
func us18StageAssets(t *testing.T, root string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"compose.yaml", "compose.test.yaml", "backup.sh", "restore.sh"} {
		us18CopyFile(t, filepath.Join(root, "docker", name), filepath.Join(dir, name), 0o755)
	}
	us18CopyTree(t, filepath.Join(root, "docker", "initdb"), filepath.Join(dir, "initdb"))
	return dir
}

// us18CopyFile 复制一个普通文件并落权限。
// us18CopyFile copies one regular file with explicit permissions.
func us18CopyFile(t *testing.T, src, dst string, perm os.FileMode) {
	t.Helper()
	body, err := os.ReadFile(src) // 源路径由夹具从仓库根拼出
	if err != nil {
		t.Fatalf("复制资产失败（读 %s）：%v", src, err)
	}
	if err := os.WriteFile(dst, body, perm); err != nil { // 部署目录内资产
		t.Fatalf("复制资产失败（写 %s）：%v", dst, err)
	}
}

// us18CopyTree 递归复制一个目录。
// us18CopyTree copies a directory recursively.
func us18CopyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755) // 容器初始化脚本目录
		}
		body, err := os.ReadFile(path) // 源路径由夹具从仓库根拼出
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o644) // initdb 脚本对容器可读
	})
	if err != nil {
		t.Fatalf("复制目录 %s 失败: %v", src, err)
	}
}

// cleanupResidue 返回该项目名下残留的容器 / 卷 / 网络 / 镜像总数：down -v 之后
// 必须为 0（A15）。
// cleanupResidue counts leftover containers, volumes, networks and project-named
// images; it must be zero after down -v (A15).
func cleanupResidue(project string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	total := 0
	for _, args := range [][]string{
		{"ps", "-a", "-q", "--filter", "label=com.docker.compose.project=" + project},
		{"volume", "ls", "-q", "--filter", "label=com.docker.compose.project=" + project},
		{"network", "ls", "-q", "--filter", "label=com.docker.compose.project=" + project},
	} {
		out, err := us18DockerOutput(ctx, args...)
		if err != nil {
			continue
		}
		total += len(us18NonEmptyLines(out))
	}
	images, err := us18DockerOutput(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}")
	if err == nil {
		for _, line := range us18NonEmptyLines(images) {
			if strings.Contains(line, project) {
				total++
			}
		}
	}
	return total
}

// us18NonEmptyLines 返回非空行（去掉首尾空白）。
// us18NonEmptyLines returns the non-empty lines of a text.
func us18NonEmptyLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// waitFor 轮询条件直到成立，超时判红。
// waitFor polls a condition until it holds, failing on timeout.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待条件超时（%s）", timeout)
}

// us18ProjectName 组装独立项目名：前缀可经 BASALT_E2E_PROJECT_PREFIX 指定（A15 的
// 清理判据按该前缀核对）。
// us18ProjectName assembles the isolated project name; A15's residue check uses the
// same prefix.
func us18ProjectName(name string) string {
	prefix := os.Getenv(us18ProjectPrefixEnv)
	if prefix == "" {
		prefix = us18DefaultPrefix
	}
	return prefix + "-" + name + "-" + us18RandomHexN(4)
}

// us18RandomHex 取 n 字节的随机十六进制串；失败即夹具环境不可用。
// us18RandomHex returns n random bytes as hex; a failure means the fixture host is
// unusable.
func us18RandomHex(t *testing.T, n int) string {
	t.Helper()
	return us18RandomHexN(n)
}

// us18RandomHexN 是 us18RandomHex 的无 t 版本（项目名在构造栈之前就要用）。
// us18RandomHexN is the t-less variant used before a stack exists.
func us18RandomHexN(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic("e2e us18: 读取随机源失败: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// us18FreePort 取一个当前空闲的回环端口。
// us18FreePort reserves then releases a loopback port and returns the number.
func us18FreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("申请回环端口失败: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("释放回环端口失败: %v", err)
	}
	return port
}

// us18RosterPath 与 us18AdminTokenPath 是容器内的固定路径：名册在工作区，令牌在
// 实例私有卷（与主密钥同目录、分文件）。
// us18RosterPath and us18AdminTokenPath are the fixed in-container paths.
const (
	us18RosterPath       = "/data/.basalt/instance.json"
	us18PrivateRoot      = "/var/lib/basalt/basalt"
	us18AdminTokenFormat = us18PrivateRoot + "/instances/%s/admin-token"
)

// us18AdminTokenPath 返回实例管理令牌路径。
// us18AdminTokenPath returns the admin-token path of one instance.
func us18AdminTokenPath(instance string) string {
	return fmt.Sprintf(us18AdminTokenFormat, instance)
}

// writeSecret 在宿主 secrets 目录写一个 secret 文件（写后用一次性 root 容器校正
// 属主与 0400，见 fixSecretOwnership）。写临时文件再改名：secret 一旦被
// fixSecretOwnership 改成 10001:10001 / 0400，宿主用户就再也写不进去（A10 / A17
// 要替换 secret 内容），而目录本身仍可写。
// writeSecret writes one secret file through a temporary file and a rename, because
// an ownership-fixed 0400 secret is no longer writable by the host user.
func (s *composeStack) writeSecret(t *testing.T, name, value string) string {
	t.Helper()
	path := filepath.Join(s.secretsDir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0o600); err != nil {
		t.Fatalf("写 secret %s 失败: %v", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("替换 secret %s 失败: %v", name, err)
	}
	return path
}

// fixSecretOwnership 用一次性 --user 0 容器把 secret 的属主与 0400 一起设好：
// postgres_password 归 0:0，database_dsn 归 10001:10001。省略 names = 两个都设。
// fixSecretOwnership sets owner and 0400 inside a one-shot root container: the
// password belongs to 0:0, the DSN to 10001:10001; omitting names covers both.
func (s *composeStack) fixSecretOwnership(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		names = []string{"postgres_password", "database_dsn"}
	}
	var script strings.Builder
	script.WriteString("set -eu\n")
	for _, name := range names {
		var owner string
		switch name {
		case "postgres_password":
			owner = "0:0"
		case "database_dsn":
			owner = "10001:10001"
		default:
			return fmt.Errorf("未知 secret %q：只允许 postgres_password / database_dsn", name)
		}
		script.WriteString("chown " + owner + " /secrets/" + name + "\n")
		script.WriteString("chmod 0400 /secrets/" + name + "\n")
	}
	image := s.env["BASALT_IMAGE"]
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", s.secretsDir+":/secrets",
		"--entrypoint", "sh", image, "-c", script.String())
	if err != nil {
		return fmt.Errorf("设置 secret 属主失败：%w（%s）", err, strings.TrimSpace(out))
	}
	return nil
}

// secretStat 用一次性容器读 secret 文件的属主与权限：宿主侧不依赖 root，也不
// 依赖宿主文件系统是否支持 chown（WSL /mnt/d 的 9p 会静默忽略）。
// secretStat reads a secret's owner and mode inside a one-shot container, so the
// assertion never depends on host privileges or 9p behaviour.
func secretStat(t *testing.T, path string) (uid, gid int, mode os.FileMode) {
	t.Helper()
	image := us18ImageFor(t, us18DefaultVersion)
	dir, name := filepath.Split(filepath.Clean(path))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", dir+":/secrets:ro",
		"--entrypoint", "sh", image, "-c",
		"stat -c '%u %g %a' /secrets/"+name)
	if err != nil {
		t.Fatalf("读取 secret 属性 %s 失败: %v", path, err)
	}
	fields := strings.Fields(out)
	if len(fields) != 3 {
		t.Fatalf("secret 属性输出不可解析：%q", strings.TrimSpace(out))
	}
	uid, err = strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("解析 secret uid %q：%v", fields[0], err)
	}
	gid, err = strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("解析 secret gid %q：%v", fields[1], err)
	}
	perm, err := strconv.ParseUint(fields[2], 8, 32)
	if err != nil {
		t.Fatalf("解析 secret 权限 %q：%v", fields[2], err)
	}
	return uid, gid, os.FileMode(perm)
}
