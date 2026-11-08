//go:build e2e

package e2e

// US-25 容器验收的栈夹具：真实镜像 + 真实 `docker/compose.multiworker.yaml`，按
// profile 分两段启动（control：预检 + PostgreSQL + 控制面；worker：Worker）。
//
// 与 US-18 夹具的关系：
//   - 复用 `us18RequireDocker` / `us18EnsureImage` / `us18ImageFor` / `us18RepoRoot` /
//     `mustImageLock` / `us18DockerOutput` / `us18NonEmptyLines` / `us18ProjectName` /
//     `us18FreePort` / `us18RandomHex` / `mergeEnv` / `us18CopyFile` / `us18CopyTree` /
//     `waitFor` / `us18Tail`。US-18 的夹具与断言面**不动**。
//   - 本夹具不复制 `composeStack`：US-25 的 compose 文件、profile、secret 布局（
//     `database_dsn` + 主密钥 bind mount）与单实例文件不同，硬塞进同一个结构体会让
//     两个 US 的判据互相污染。
//
// 测试 overlay（`compose.test.yaml`）只做一件夹具级的事：`ports: !reset []` 去掉
// PostgreSQL 的宿主端口发布。生产文件在单宿主把 5432 绑回环（FR-25-14 的判据由
// `TestUS25_Compose_DatabasePrivateAddress` 直接渲染**原文件**核对），而并行/连续
// 跑多个栈时宿主 5432 会冲突；容器间仍走 Docker 网络，判据不受影响。
//
// The US-25 stack fixture runs the real multi-worker compose file by profile. The test
// overlay only drops PostgreSQL's host port publish so consecutive stacks cannot fight
// over 5432; the shipped file itself is rendered unchanged by the private-address test.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 容器内固定路径（与 docker/entrypoint.sh、compose.multiworker.yaml 一致）。
// Fixed in-container paths, byte-identical to the entrypoint and compose file.
const (
	us25ControlService   = "basalt-control"
	us25WorkerService    = "basalt-worker"
	us25PostgresService  = "postgres"
	us25PreflightService = "basalt-preflight"

	// 两个 profile 与 compose 文件逐字一致；`down` 必须显式带上它们才能移除容器。
	// The two profile names, verbatim from the compose file; down must name them.
	us25ControlProfile = "control"
	us25WorkerProfile  = "worker"

	us25RosterPath    = "/data/.basalt/instance.json"
	us25ConfigPath    = "/data/basalt.yaml"
	us25SQLitePath    = "/data/.basalt/basalt.db"
	us25KeyMountPath  = "/run/basalt-secrets/master_key"
	us25KeyParentPath = "/run/basalt-secrets"
	us25PrivateRoot   = "/var/lib/basalt"

	// us25KeyInSecretsPath 是**故意**放进默认 0755 /run/secrets 的密钥路径：FR-25-13
	// 要求同一把正确密钥在 0755 父目录被拒绝，这一侧由该路径构造。
	// The deliberately-too-wide key location used by the negative judgement.
	us25KeyInSecretsPath = "/run/secrets/master_key"

	us25StartTimeout = 4 * time.Minute
	us25ExitTimeout  = 2 * time.Minute
)

// us25Stack 是一套独立的 compose 项目：独立项目名、独立卷、独立宿主端口与 secrets。
// us25Stack is one isolated compose project with its own volumes, host port and secrets.
type us25Stack struct {
	t          *testing.T
	project    string
	dir        string
	secretsDir string
	port       int
	image      string
	password   string
	dsn        string
	env        map[string]string
	extraFiles []string
	lastOutput string
}

// us25NewStack 准备一套可启动的栈：暂存 compose 资产、写缺省 secret 并校正属主、
// 取随机宿主端口与项目名。缺 daemon / 基础镜像 / 应用镜像时按 REQUIRE_DOCKER 决定
// 失败或跳过（与 US-18 同口径）。
// us25NewStack stages the compose assets, seeds the default secrets and fixes their
// owners, then picks a random project name and host port.
func us25NewStack(t *testing.T, name string) *us25Stack {
	t.Helper()
	us18RequireDocker(t)
	if runtime.GOOS != "linux" {
		// 夹具用宿主目录挂进容器设置属主，Windows 路径映射不在本 US 的验收面内。
		if os.Getenv(us18RequireDockerEnv) == "1" {
			t.Fatalf("容器 e2e 只在 Linux 宿主运行（当前 %s）", runtime.GOOS)
		}
		t.Skipf("容器 e2e 只在 Linux 宿主运行（当前 %s），跳过并登记未验证", runtime.GOOS)
	}
	root := us18RepoRoot(t)
	lock := mustImageLock(t, root)
	us18EnsureImage(t, lock.Postgres)
	image := us18ImageFor(t, us18DefaultVersion)

	dir := t.TempDir()
	us18CopyFile(t, filepath.Join(root, "docker", "compose.multiworker.yaml"),
		filepath.Join(dir, "compose.multiworker.yaml"), 0o644)
	// 夹具 overlay：去掉 PG 的宿主端口发布（见文件头注释）。
	us25WriteFile(t, filepath.Join(dir, "compose.test.yaml"), `# US-25 夹具 overlay：只在测试里去发布 PostgreSQL 的宿主端口。
services:
  postgres:
    ports: !reset []
`)
	us18CopyTree(t, filepath.Join(root, "docker", "initdb"), filepath.Join(dir, "initdb"))

	secretsDir := filepath.Join(dir, "secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil { // 容器要能读目录
		t.Fatalf("创建 secrets 目录失败: %v", err)
	}
	port := us18FreePort(t)
	password := us18RandomHex(t, 16)
	stack := &us25Stack{
		t:          t,
		project:    us18ProjectName("u25-" + name),
		dir:        dir,
		secretsDir: secretsDir,
		port:       port,
		image:      image,
		password:   password,
		// 测试里 DSN 是随机生成的会话内值，不是任何真实凭据；Worker 侧的副本因此
		// 可以直接由夹具写出。
		dsn: "postgres://basalt:" + password + "@postgres:5432/basalt?sslmode=disable",
		env: map[string]string{
			"BASALT_IMAGE":                image,
			"BASALT_DB_BIND":              "127.0.0.1",
			"BASALT_HTTP_BIND":            "127.0.0.1",
			"BASALT_HTTP_PORT":            strconv.Itoa(port),
			"BASALT_INGRESS_RUN_QPS":      us18IngressQPS,
			"BASALT_INGRESS_RUN_BURST":    us18IngressBurst,
			"BASALT_INGRESS_INGEST_QPS":   us18IngressQPS,
			"BASALT_INGRESS_INGEST_BURST": us18IngressBurst,
			"BASALT_INGRESS_READ_QPS":     us18IngressQPS,
			"BASALT_INGRESS_READ_BURST":   us18IngressBurst,
			"BASALT_WORKER_NAME":          "worker-a",
		},
	}
	stack.writeSecret(t, "postgres_password", password)
	stack.writeSecret(t, "database_dsn", stack.dsn)
	if err := stack.fixSecretOwnership(context.Background()); err != nil {
		t.Fatalf("设置 secret 属主失败: %v", err)
	}
	t.Cleanup(func() { stack.cleanup() })
	return stack
}

// us25WriteFile 写一个夹具资产文件；失败即用例失败。
// us25WriteFile writes one fixture asset.
func us25WriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("写夹具文件 %s 失败: %v", path, err)
	}
}

// compose 以本栈的项目名、文件表与环境跑一条 docker compose 子命令。
// compose runs one docker compose subcommand under this stack's project and files.
func (s *us25Stack) compose(ctx context.Context, args ...string) (string, error) {
	return s.composeWithEnv(ctx, nil, args...)
}

// composeWithEnv 允许单次调用覆盖环境（负向用例改 BASALT_DB_BIND / 抹掉入口限流）。
// 空串按 `KEY=` 传入：compose 的 `${VAR:?}` 判空、`${VAR:-缺省}` 取缺省，效果等同未设置，
// 而预检脚本用 `set -u` + 显式 `require_value` 读到的也是空串——两侧语义一致。
// composeWithEnv overrides the environment for one call; an empty value is passed as
// `KEY=`, which compose's `:?`/`:-` and the shell's `set -u` both treat as unset.
func (s *us25Stack) composeWithEnv(ctx context.Context, overrides map[string]string, args ...string) (string, error) {
	env := make(map[string]string, len(s.env)+len(overrides))
	for key, value := range s.env {
		env[key] = value
	}
	for key, value := range overrides {
		env[key] = value
	}
	base := []string{
		"compose", "-p", s.project,
		"-f", filepath.Join(s.dir, "compose.multiworker.yaml"),
		"-f", filepath.Join(s.dir, "compose.test.yaml"),
	}
	for _, extra := range s.extraFiles {
		base = append(base, "-f", extra)
	}
	cmd := exec.CommandContext(ctx, "docker", append(base, args...)...) // 参数由夹具构造
	cmd.Dir = s.dir
	cmd.Env = mergeEnv(os.Environ(), env)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	s.lastOutput = buf.String()
	return s.lastOutput, err
}

// addOverlay 追加一份测试 overlay（相对路径按首个 compose 文件所在目录解析，
// 因此 overlay 与正式文件同放一个目录）。
// addOverlay appends a test overlay; relative paths resolve against the first file.
func (s *us25Stack) addOverlay(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(s.dir, fmt.Sprintf("overlay-%d.yaml", len(s.extraFiles)))
	us25WriteFile(t, path, body)
	s.extraFiles = append(s.extraFiles, path)
	return path
}

// up 启动某个 profile 的全部服务并等待健康；extra 追加到 compose 参数尾部。
// up starts one profile and waits for health.
func (s *us25Stack) up(ctx context.Context, profile string, extra ...string) error {
	args := []string{"--profile", profile, "up", "-d", "--wait", "--wait-timeout", "240"}
	args = append(args, extra...)
	out, err := s.compose(ctx, args...)
	s.lastOutput = out
	if err != nil {
		return fmt.Errorf("compose --profile %s up：%w（%s）",
			profile, err, us18Tail(out, 6000))
	}
	return nil
}

// upDetached 只创建并启动，不等健康：负向用例要观察容器自己退出。
// upDetached starts without waiting so a negative case can observe the exit itself.
func (s *us25Stack) upDetached(ctx context.Context, profile string, extra ...string) error {
	args := []string{"--profile", profile, "up", "-d"}
	args = append(args, extra...)
	out, err := s.compose(ctx, args...)
	s.lastOutput = out
	if err != nil {
		return fmt.Errorf("compose --profile %s up -d：%w（%s）",
			profile, err, us18Tail(out, 6000))
	}
	return nil
}

// down 停止并移除项目；removeVolumes 时连卷一起删。
//
// **必须**显式带上两个 profile：本文件的服务全部挂在 profile 下，`docker compose down`
// 只处理**活动 profile** 内的服务（compose v5.6.1 实测：不带 profile 时退出码 0、零输出、
// 容器与卷原样留下，随后的镜像删除会因容器仍引用镜像而失败）。
// Both profiles must be named: down only touches services in the active profiles, so a
// bare down leaves every profiled container and volume behind (compose v5.6.1).
func (s *us25Stack) down(ctx context.Context, removeVolumes bool) error {
	args := []string{
		"--profile", us25ControlProfile, "--profile", us25WorkerProfile,
		"down", "--remove-orphans", "--timeout", "90",
	}
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

// cleanup 在用例结束时收掉整个项目：失败时先留一份容器日志，再删卷。
// cleanup brings the project down, keeping log tails when the test failed.
func (s *us25Stack) cleanup() {
	if os.Getenv(us18KeepEnv) == "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if s.t.Failed() {
		for _, service := range []string{us25ControlService, us25WorkerService} {
			if logs, err := s.logs(ctx, service); err == nil && strings.TrimSpace(logs) != "" {
				s.t.Logf("US-25 现场：项目 %s 的 %s 容器日志（尾部）\n%s",
					s.project, service, us18Tail(logs, 4000))
			}
		}
	}
	if err := s.down(ctx, true); err != nil {
		s.t.Logf("US-25 清理项目 %s 失败：%v", s.project, err)
	}
}

// stopService 停一个服务（走 stop_grace_period）。
// stopService stops one service through compose's stop_grace_period.
func (s *us25Stack) stopService(ctx context.Context, service string) error {
	out, err := s.compose(ctx, "stop", service)
	if err != nil {
		return fmt.Errorf("compose stop %s：%w（%s）", service, err, out)
	}
	return nil
}

// startService 启动先前停掉的服务。
// startService starts a previously stopped service.
func (s *us25Stack) startService(ctx context.Context, service string) error {
	out, err := s.compose(ctx, "start", service)
	if err != nil {
		return fmt.Errorf("compose start %s：%w（%s）", service, err, out)
	}
	return nil
}

// logs 读取一个服务的日志（无颜色，尾部有上界）。
// logs reads one service's log with a bounded tail.
func (s *us25Stack) logs(ctx context.Context, service string) (string, error) {
	out, err := s.compose(ctx, "logs", "--no-color", "--tail", "4000", service)
	if err != nil {
		return out, fmt.Errorf("compose logs %s：%w", service, err)
	}
	return out, nil
}

// exec 在服务容器内执行命令（-T 关闭伪终端）。
// exec runs a command inside a service container.
func (s *us25Stack) exec(ctx context.Context, service string, argv ...string) (string, error) {
	args := append([]string{"exec", "-T", service}, argv...)
	out, err := s.compose(ctx, args...)
	if err != nil {
		return out, fmt.Errorf("compose exec %s %s：%w（%s）",
			service, strings.Join(argv, " "), err, us18Tail(out, 3000))
	}
	return out, nil
}

// serviceContainerID 取服务容器 ID；没有容器时报错（失败发生在创建之前）。
// serviceContainerID returns the service container id; a missing one means the
// failure happened before container creation.
func (s *us25Stack) serviceContainerID(ctx context.Context, service string) (string, error) {
	out, err := s.compose(ctx, "ps", "-a", "-q", service)
	if err != nil {
		return "", err
	}
	ids := us18NonEmptyLines(out)
	if len(ids) == 0 {
		return "", fmt.Errorf("没有 %s 容器", service)
	}
	return ids[0], nil
}

// serviceExitCode 读取服务容器的退出码。
// serviceExitCode reads the service container's exit code.
func (s *us25Stack) serviceExitCode(ctx context.Context, service string) (int, error) {
	id, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return 0, err
	}
	inspect, err := us18DockerOutput(ctx, "inspect", "--format", "{{.State.ExitCode}}", id)
	if err != nil {
		return 0, err
	}
	code, convErr := strconv.Atoi(strings.TrimSpace(inspect))
	if convErr != nil {
		return 0, fmt.Errorf("解析退出码 %q：%w", strings.TrimSpace(inspect), convErr)
	}
	return code, nil
}

// serviceState 读容器状态（`created` / `running` / `exited` …）。
// serviceState reads the container's lifecycle state.
func (s *us25Stack) serviceState(ctx context.Context, service string) string {
	id, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return ""
	}
	out, err := us18DockerOutput(ctx, "inspect", "--format", "{{.State.Status}}", id)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// awaitExit 等容器退出（负向用例）：返回退出码，超时判红。
// awaitExit waits until the container exits and returns its code.
func (s *us25Stack) awaitExit(t *testing.T, service string, timeout time.Duration) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state := s.serviceState(ctx, service)
		if state == "exited" || state == "dead" {
			code, err := s.serviceExitCode(ctx, service)
			if err != nil {
				t.Fatalf("读取 %s 退出码失败：%v", service, err)
			}
			return code
		}
		time.Sleep(500 * time.Millisecond)
	}
	logs, _ := s.logs(ctx, service)
	t.Fatalf("%s 在 %s 内没有退出（状态 %q），日志尾部：\n%s",
		service, timeout, s.serviceState(ctx, service), us18Tail(logs, 4000))
	return 0
}

// configJSON 渲染 compose 配置（带本栈环境与给定文件表）。
// configJSON renders the compose configuration under this stack's environment.
func (s *us25Stack) configJSON(t *testing.T, files []string, envOverrides map[string]string) map[string]any {
	t.Helper()
	env := make(map[string]string, len(s.env)+len(envOverrides))
	for key, value := range s.env {
		env[key] = value
	}
	for key, value := range envOverrides {
		if value == "" {
			delete(env, key) // 空串表示"不设置该变量"
			continue
		}
		env[key] = value
	}
	return us25ComposeConfigJSON(t, s.dir, files, env)
}

// baseFiles 返回正式 compose 文件表（不含测试 overlay）。
// baseFiles is the shipped compose file list without the test overlay.
func (s *us25Stack) baseFiles() []string {
	return []string{filepath.Join(s.dir, "compose.multiworker.yaml")}
}

// stackFiles 返回本栈实际使用的文件表（正式文件 + 测试 overlay + 追加 overlay）。
// stackFiles is the file list this stack actually runs with.
func (s *us25Stack) stackFiles() []string {
	files := []string{
		filepath.Join(s.dir, "compose.multiworker.yaml"),
		filepath.Join(s.dir, "compose.test.yaml"),
	}
	return append(files, s.extraFiles...)
}

// us25ComposeConfigJSON 调 `docker compose config --format json` 并解析。
// us25ComposeConfigJSON renders and parses `docker compose config --format json`.
func us25ComposeConfigJSON(t *testing.T, dir string, files []string, env map[string]string) map[string]any {
	t.Helper()
	args := []string{"compose"}
	for _, file := range files {
		args = append(args, "-f", file)
	}
	// `--profile "*"` 让渲染包含全部服务：不带 profile 时只有"无 profile"的服务，
	// 本文件的每个服务都带 profile。
	args = append(args, "--profile", "*", "config", "--format", "json")
	cmd := exec.CommandContext(context.Background(), "docker", args...)
	cmd.Dir = dir
	cmd.Env = mergeEnv(os.Environ(), env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("渲染 compose 配置失败：%v（%s）", err, strings.TrimSpace(stderr.String()))
	}
	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		t.Fatalf("解析 compose 配置 JSON 失败：%v", err)
	}
	return parsed
}

// us25Services 从渲染结果取 services 表。
// us25Services returns the rendered services map.
func us25Services(t *testing.T, cfg map[string]any) map[string]any {
	t.Helper()
	raw, ok := cfg["services"]
	if !ok {
		t.Fatalf("渲染结果缺少 services")
	}
	services, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("services 不是对象：%T", raw)
	}
	return services
}

// us25Service 取单个服务定义。
// us25Service returns one rendered service definition.
func us25Service(t *testing.T, cfg map[string]any, name string) map[string]any {
	t.Helper()
	service, ok := us25Services(t, cfg)[name]
	if !ok {
		t.Fatalf("渲染结果缺少服务 %s", name)
	}
	definition, ok := service.(map[string]any)
	if !ok {
		t.Fatalf("服务 %s 不是对象：%T", name, service)
	}
	return definition
}

// us25ServiceNames 从 `config --services`（或渲染结果）取服务名集合。
// us25ServiceNames lists the services of a rendered configuration.
func us25ServiceNames(t *testing.T, cfg map[string]any) []string {
	t.Helper()
	names := make([]string, 0, len(us25Services(t, cfg)))
	for name := range us25Services(t, cfg) {
		names = append(names, name)
	}
	return names
}

// us25StringList 把渲染结果里的字符串数组转成 []string。
// us25StringList converts a rendered string array.
func us25StringList(t *testing.T, raw any, label string) []string {
	t.Helper()
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s 不是数组：%T", label, raw)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			t.Fatalf("%s 含非字符串项：%T", label, item)
		}
		out = append(out, text)
	}
	return out
}

// us25Mount 是渲染后的一个挂载点。
// us25Mount is one rendered mount.
type us25Mount struct {
	Type     string
	Source   string
	Target   string
	ReadOnly bool
}

// us25Mounts 取出一个服务的挂载表。
// us25Mounts reads one service's mounts.
func us25Mounts(t *testing.T, definition map[string]any) []us25Mount {
	t.Helper()
	raw, ok := definition["volumes"]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("volumes 不是数组：%T", raw)
	}
	out := make([]us25Mount, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("volumes 项不是对象：%T", item)
		}
		mount := us25Mount{}
		mount.Type, _ = entry["type"].(string)
		mount.Source, _ = entry["source"].(string)
		mount.Target, _ = entry["target"].(string)
		mount.ReadOnly, _ = entry["read_only"].(bool)
		out = append(out, mount)
	}
	return out
}

// us25MountAt 按容器内目标路径找挂载；找不到时返回 nil。
// us25MountAt finds a mount by its in-container target path.
func us25MountAt(t *testing.T, definition map[string]any, target string) *us25Mount {
	t.Helper()
	for _, mount := range us25Mounts(t, definition) {
		if mount.Target == target {
			found := mount
			return &found
		}
	}
	return nil
}

// us25Ports 取出一个服务发布的端口表。
// us25Ports reads one service's published ports.
func us25Ports(t *testing.T, definition map[string]any) []any {
	t.Helper()
	raw, ok := definition["ports"]
	if !ok {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		t.Fatalf("ports 不是数组：%T", raw)
	}
	return items
}

// us25HealthTest 取健康检查命令（字符串数组）。
// us25HealthTest reads the healthcheck command.
func us25HealthTest(t *testing.T, definition map[string]any) []string {
	t.Helper()
	raw, ok := definition["healthcheck"]
	if !ok {
		return nil
	}
	check, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("healthcheck 不是对象：%T", raw)
	}
	return us25StringList(t, check["test"], "healthcheck.test")
}

// writeSecret 在宿主 secrets 目录写一个 secret 文件（先写临时文件再改名，理由同
// US-18 夹具：定好属主与 0400 之后就写不进去了）。
// writeSecret writes one secret through a rename, matching the US-18 fixture.
func (s *us25Stack) writeSecret(t *testing.T, name, value string) string {
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

// fixSecretOwnership 用一次性 --user 0 容器设置 secret 的属主与 0400：
// postgres_password 归 0:0，database_dsn 归 10001:10001（与 US-18 同判据）。
// fixSecretOwnership sets owner and 0400 inside a one-shot root container.
func (s *us25Stack) fixSecretOwnership(ctx context.Context, names ...string) error {
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
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", s.secretsDir+":/secrets",
		"--entrypoint", "sh", s.image, "-c", script.String())
	if err != nil {
		return fmt.Errorf("设置 secret 属主失败：%w（%s）", err, strings.TrimSpace(out))
	}
	return nil
}

// exportMasterKey 把控制面私有卷里的主密钥复制到宿主 `secrets/master.key`
// （Compose 的 bind mount 源），并设置 0400 / 10001:10001。判据与
// docker/export-master-key.sh 相同（单一密钥、非空、复制后 stat 复核），只是由
// 一次性 root 容器执行——脚本本身要求宿主 root 与 `docker volume inspect`，
// 测试进程与容器内都看不到宿主守护进程。
//
// 宿主 `secrets/` 目录保持测试用户可写（0755）：产品判据看的是**容器内**的直接
// 父目录（`/run/basalt-secrets`，镜像预建 0700）；宿主目录只影响 10001 能否穿越
// 到 bind 源，0755 允许穿越。生产脚本把宿主目录设成 0700/10001:10001 的那条
// 判据由脚本自身承担，不在本夹具重复。
// exportMasterKey copies the control plane's key to the compose bind-mount source
// under the same verdicts as export-master-key.sh; the host directory stays
// test-user-writable because the product checks the in-container parent, which the
// image pre-creates at 0700.
func (s *us25Stack) exportMasterKey(ctx context.Context) (string, error) {
	volume := s.project + "_control-private"
	dest := filepath.Join(s.secretsDir, "master.key")
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("主密钥目标 %s 已存在，拒绝覆盖", dest)
	}
	script := strings.Join([]string{
		"set -eu",
		`set -- /src/basalt/instances/*/master.key`,
		`[ -e "$1" ] || { echo "卷里没有主密钥" >&2; exit 1; }`,
		`[ "$#" -eq 1 ] || { echo "卷里有 $# 个主密钥" >&2; exit 1; }`,
		`[ -s "$1" ] || { echo "主密钥为空" >&2; exit 1; }`,
		"umask 077",
		`cat "$1" > /secrets/master.key`,
		"chmod 0400 /secrets/master.key",
		"chown 10001:10001 /secrets/master.key",
		`[ "$(stat -c '%u:%g:%a' /secrets/master.key)" = "10001:10001:400" ] || { echo "主密钥属性不符" >&2; exit 1; }`,
	}, "\n")
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", volume+":/src:ro",
		"-v", s.secretsDir+":/secrets",
		"--entrypoint", "sh", s.image, "-c", script)
	if err != nil {
		return "", fmt.Errorf("导出主密钥失败：%w（%s）", err, strings.TrimSpace(out))
	}
	return dest, nil
}

// exportSeed 用镜像里的 docker/seed-workspace.sh 把控制面工作区导出成种子归档。
// 调用前**必须**已停掉控制面（脚本是冷备，不是热同步）。
// exportSeed runs the shipped seed script against the control workspace volume; the
// caller must have stopped the control plane first.
func (s *us25Stack) exportSeed(ctx context.Context) (string, error) {
	volume := s.project + "_control-workspace"
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", volume+":/src:ro",
		"-v", s.dir+":/seed",
		"--entrypoint", "/opt/basalt/seed-workspace.sh", s.image,
		"export", "/src", "/seed/seed.tar")
	if err != nil {
		return "", fmt.Errorf("导出种子失败：%w（%s）", err, strings.TrimSpace(out))
	}
	return filepath.Join(s.dir, "seed.tar"), nil
}

// importSeed 把种子归档导入一个命名卷（卷必须先由 compose create 建好，以便继承
// 镜像内 /data 的 10001:10001 属主）。
// importSeed imports the archive into a named volume created by compose, so it
// inherits the image's /data ownership.
func (s *us25Stack) importSeed(ctx context.Context, volume, archive string) error {
	dir, name := filepath.Split(filepath.Clean(archive))
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", volume+":/data",
		"-v", dir+":/seed:ro",
		"--entrypoint", "/opt/basalt/seed-workspace.sh", s.image,
		"import", "/seed/"+name, "/data")
	if err != nil {
		return fmt.Errorf("导入种子到 %s 失败：%w（%s）", volume, err, strings.TrimSpace(out))
	}
	return nil
}

// createVolume 让 compose 先建好（并打标签）worker 用的命名卷。
// createVolume asks compose to create the project's worker volumes first.
func (s *us25Stack) createVolume(ctx context.Context, service string) error {
	out, err := s.compose(ctx, "--profile", "worker", "create", "--no-recreate", service)
	if err != nil {
		return fmt.Errorf("compose create %s：%w（%s）", service, err, us18Tail(out, 4000))
	}
	return nil
}

// seedVolume 为某个 worker 服务建卷、导入种子，返回真实卷名。
// seedVolume creates one worker's volumes through compose, then imports the seed.
func (s *us25Stack) seedVolume(t *testing.T, service, logicalVolume, archive string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := s.createVolume(ctx, service); err != nil {
		t.Fatalf("创建 %s 的工作卷失败：%v", service, err)
	}
	volume := s.projectVolumeName(t, ctx, logicalVolume)
	if err := s.importSeed(ctx, volume, archive); err != nil {
		t.Fatalf("导入种子到 %s 失败：%v", service, err)
	}
	return volume
}

// startControl 起控制面（control profile）并等首次启动写出实例名册，返回实例标识。
// startControl brings the control profile up and returns the roster's instance id.
func (s *us25Stack) startControl(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("启动控制面失败：%v", err)
	}
	return s.instanceID(t, s.waitRoster(t))
}

// exportControlAssets 停控制面（冷备前提）→ 导出共享主密钥与种子归档。
// 控制面随后可以再次启动；本函数只保证导出这一刻没有进程在写工作区。
// exportControlAssets stops the control plane (the cold-copy precondition) and exports
// the shared master key and the seed archive.
func (s *us25Stack) exportControlAssets(t *testing.T) (keyPath, seedPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.stopService(ctx, us25ControlService); err != nil {
		t.Fatalf("停控制面失败：%v", err)
	}
	if code := s.awaitExit(t, us25ControlService, us25ExitTimeout); code != 0 {
		logs, _ := s.logs(ctx, us25ControlService)
		t.Fatalf("控制面停机退出码 %d（要求 0），日志尾部：\n%s", code, us18Tail(logs, 4000))
	}
	key, err := s.exportMasterKey(ctx)
	if err != nil {
		t.Fatalf("导出主密钥失败：%v", err)
	}
	seed, err := s.exportSeed(ctx)
	if err != nil {
		t.Fatalf("导出种子失败：%v", err)
	}
	return key, seed
}

// projectVolumeName 取逻辑卷的真实名字（按 compose 标签，与 US-18 同判据）。
// projectVolumeName resolves a logical volume's real name through compose labels.
func (s *us25Stack) projectVolumeName(t *testing.T, ctx context.Context, logical string) string {
	t.Helper()
	out, err := us18DockerOutput(ctx, "volume", "ls", "-q",
		"--filter", "label=com.docker.compose.project="+s.project,
		"--filter", "label=com.docker.compose.volume="+logical)
	if err != nil {
		t.Fatalf("列卷失败: %v", err)
	}
	names := us18NonEmptyLines(out)
	if len(names) == 0 {
		t.Fatalf("项目 %s 找不到 %s 卷（compose create 是否执行过？）", s.project, logical)
	}
	return names[0]
}

// volumeRead 在一次性容器里读卷内文件（只读）。
// volumeRead reads one file from a named volume in a one-shot container.
func (s *us25Stack) volumeRead(ctx context.Context, volume, path string) (string, error) {
	out, err := us18DockerOutput(ctx, "run", "--rm",
		"-v", volume+":/data:ro",
		"--entrypoint", "sh", s.image, "-c", "cat /data"+path)
	if err != nil {
		return "", fmt.Errorf("读取卷 %s 的 %s 失败：%w（%s）", volume, path, err, strings.TrimSpace(out))
	}
	return out, nil
}

// volumeStat 在一次性容器里 stat 卷内路径（`%u:%g:%a`）。
// volumeStat reads a volume path's owner and mode in a one-shot container.
func (s *us25Stack) volumeStat(ctx context.Context, volume, path string) (string, error) {
	out, err := us18DockerOutput(ctx, "run", "--rm",
		"-v", volume+":/data:ro",
		"--entrypoint", "sh", s.image, "-c", "stat -c '%u:%g:%a' /data"+path)
	if err != nil {
		return "", fmt.Errorf("stat 卷 %s 的 %s 失败：%w（%s）", volume, path, err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// volumeHas 判卷内路径是否存在（一次性容器）。
// volumeHas reports whether a path exists inside a named volume.
func (s *us25Stack) volumeHas(ctx context.Context, volume, path string) bool {
	_, err := us18DockerOutput(ctx, "run", "--rm",
		"-v", volume+":/data:ro",
		"--entrypoint", "sh", s.image, "-c", "test -e /data"+path)
	return err == nil
}

// waitRoster 等控制面首次启动写出名册。
// waitRoster waits for the control plane's first-boot roster.
func (s *us25Stack) waitRoster(t *testing.T) string {
	t.Helper()
	var raw string
	waitFor(t, us25StartTimeout, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		out, err := s.exec(ctx, us25ControlService, "cat", us25RosterPath)
		if err != nil {
			return false
		}
		raw = out
		return strings.TrimSpace(out) != ""
	})
	return raw
}

// instanceID 解析名册里的实例标识。
// instanceID parses the roster's instance id.
func (s *us25Stack) instanceID(t *testing.T, raw string) string {
	t.Helper()
	var roster struct {
		Version    int    `json:"version"`
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal([]byte(raw), &roster); err != nil {
		t.Fatalf("解析实例名册失败（%s）：%v", raw, err)
	}
	if roster.InstanceID == "" {
		t.Fatalf("实例名册缺少 instance_id（%s）", raw)
	}
	return roster.InstanceID
}

// psql 在 postgres 容器里执行一条只读查询（-tA 单列读数）。
// psql runs one read query inside the postgres container.
func (s *us25Stack) psql(t *testing.T, query string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := s.exec(ctx, us25PostgresService, "sh", "-c",
		`psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "$1"`, "sh", query)
	if err != nil {
		t.Fatalf("查询数据库失败（%s）：%v", query, err)
	}
	return strings.TrimSpace(out)
}

// execInVolume 在一次性容器里跑一段脚本（卷挂到 /data），返回合并输出。
// execInVolume runs a script in a one-shot container with the volume at /data.
func (s *us25Stack) execInVolume(ctx context.Context, volume, script string) (string, error) {
	return us18DockerOutput(ctx, "run", "--rm",
		"-v", volume+":/data",
		"--entrypoint", "sh", s.image, "-c", script)
}

// runOneOff 在服务的一次性容器里跑一条命令：走镜像入口（DSN / 模板 / 预检都按
// 角色生效），挂同一个卷与 secret，但不起服务的常驻进程。
//
// profile **必须**点名：本文件的服务都挂在 profile 下，不激活时 compose 会当作
// 不存在的服务。overrides 为 nil 时按栈环境原样渲染；非 nil 时按 composeWithEnv
// 的空串语义覆盖（空串 = 变量存在但为空）。
// runOneOff runs one command in a one-off container that keeps the service's volumes,
// secrets and entrypoint. The profile must be named, and overrides follow
// composeWithEnv's empty-string semantics.
func (s *us25Stack) runOneOff(ctx context.Context, profile string, overrides map[string]string, service string, argv ...string) (string, error) {
	args := append([]string{"--profile", profile, "run", "--rm", "--no-deps", "-T", service}, argv...)
	out, err := s.composeWithEnv(ctx, overrides, args...)
	if err != nil {
		// 失败时也把输出交回调用方：负向用例要判失败**说了什么**，而不只是退出码。
		// The output is returned on failure too: negative cases judge what it says.
		return out, fmt.Errorf("compose run %s %s：%w", service, strings.Join(argv, " "), err)
	}
	return out, nil
}

// healthState 返回容器的健康状态（`starting` / `healthy` / `unhealthy`）；没有
// 健康检查或容器不存在时返回空串。
// healthState reads the container health status; empty when absent.
func (s *us25Stack) healthState(ctx context.Context, service string) string {
	id, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return ""
	}
	out, err := us18DockerOutput(ctx, "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{end}}", id)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// publishedPorts 返回宿主机上的端口映射行（`docker port`）；没有发布端口时为空。
// publishedPorts lists host port mappings; empty means nothing is published.
func (s *us25Stack) publishedPorts(ctx context.Context, service string) ([]string, error) {
	id, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return nil, err
	}
	out, err := us18DockerOutput(ctx, "port", id)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 的端口映射失败：%w", service, err)
	}
	return us18NonEmptyLines(out), nil
}

// containerMounts 读容器实际挂载表（区分卷与 bind，带读写标志）。
// containerMounts reads the container's live mounts.
func (s *us25Stack) containerMounts(ctx context.Context, service string) ([]us25LiveMount, error) {
	id, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return nil, err
	}
	// 命名卷的 `.Source` 是宿主路径（/var/lib/docker/volumes/<name>/_data），卷名只在
	// `.Name` 里；bind 的 `.Name` 为空。两者都取，按类型各用各的。
	// For named volumes `.Source` is a host path and only `.Name` holds the volume name;
	// bind mounts have an empty `.Name`. Both are read, each used by type.
	out, err := us18DockerOutput(ctx, "inspect", "--format",
		"{{range .Mounts}}{{.Type}}\t{{.Name}}\t{{.Source}}\t{{.Destination}}\t{{.RW}}\n{{end}}", id)
	if err != nil {
		return nil, fmt.Errorf("读取 %s 挂载失败：%w", service, err)
	}
	var mounts []us25LiveMount
	for _, line := range us18NonEmptyLines(out) {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			return nil, fmt.Errorf("无法解析挂载行 %q", line)
		}
		mounts = append(mounts, us25LiveMount{
			Type:        fields[0],
			Name:        fields[1],
			Source:      fields[2],
			Destination: fields[3],
			ReadWrite:   fields[4] == "true",
		})
	}
	return mounts, nil
}

// us25LiveMount 是一个运行中容器的实际挂载。
// us25LiveMount is one live mount of a running container.
type us25LiveMount struct {
	Type        string
	Name        string // 仅命名卷非空；only set for named volumes
	Source      string
	Destination string
	ReadWrite   bool
}

// liveMountAt 按容器内目标路径找实际挂载；找不到返回 nil。
// liveMountAt finds a live mount by destination.
func us25LiveMountAt(mounts []us25LiveMount, destination string) *us25LiveMount {
	for _, mount := range mounts {
		if mount.Destination == destination {
			found := mount
			return &found
		}
	}
	return nil
}

// containerStat 在运行中的容器里 stat 一个路径（`%u:%g:%a`）。
// containerStat stats a path inside the running container.
func (s *us25Stack) containerStat(ctx context.Context, service, path string) (string, error) {
	out, err := s.exec(ctx, service, "stat", "-c", "%u:%g:%a", path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// containerTop 返回容器内的进程表（`docker top`，含完整 argv）。
// containerTop returns the container's process table with full argv.
func (s *us25Stack) containerTop(ctx context.Context, service string) (string, error) {
	id, err := s.serviceContainerID(ctx, service)
	if err != nil {
		return "", err
	}
	out, err := us18DockerOutput(ctx, "top", id, "-eo", "pid,args")
	if err != nil {
		return "", fmt.Errorf("读取 %s 进程表失败：%w", service, err)
	}
	return out, nil
}

// volumeTree 返回卷内某个子树的内容指纹：每行 `<sha256> <相对路径>`，按路径排序。
// 用于判“Worker 没有重装 / 重建资产”：跑前跑后两次数值必须逐字相同。
// volumeTree fingerprints a subtree (sha256 + path, sorted); identical strings before
// and after mean nothing was installed or rebuilt.
func (s *us25Stack) volumeTree(ctx context.Context, volume, path string) (string, error) {
	script := "cd /data" + path + " 2>/dev/null || { echo '__absent__'; exit 0; }\n" +
		"find . -type f -exec sha256sum {} + | LC_ALL=C sort"
	out, err := us18DockerOutput(ctx, "run", "--rm",
		"-v", volume+":/data:ro",
		"--entrypoint", "sh", s.image, "-c", script)
	if err != nil {
		return "", fmt.Errorf("读取卷 %s 的 %s 内容失败：%w（%s）", volume, path, err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

// us25WorkerSpec 描述夹具里额外加的一个 Worker 服务（同项目、同网络、独立卷）。
// us25WorkerSpec describes an extra worker service added to the same project.
type us25WorkerSpec struct {
	// Service 是 compose 服务名；VolumePrefix 决定两个命名卷的前缀。
	Service      string
	VolumePrefix string
	// Name 写进 BASALT_WORKER_NAME（只作诊断）。
	Name string
	// KeyHostPath 为空表示**不挂**主密钥；KeyMountPath 是容器内挂载点；
	// KeyEnvPath 是 BASALT_MASTER_KEY_FILE（缺省取 KeyMountPath）。
	KeyHostPath  string
	KeyMountPath string
	KeyEnvPath   string
}

// addWorkerService 生成并追加一个 Worker 服务的 overlay。
// addWorkerService appends one worker service overlay.
func (s *us25Stack) addWorkerService(t *testing.T, spec us25WorkerSpec) string {
	t.Helper()
	keyMount := ""
	keyEnv := spec.KeyEnvPath
	if spec.KeyHostPath != "" {
		mountPath := spec.KeyMountPath
		if mountPath == "" {
			mountPath = us25KeyMountPath
		}
		keyMount = fmt.Sprintf("      - %s:%s:ro\n", spec.KeyHostPath, mountPath)
		if keyEnv == "" {
			keyEnv = mountPath
		}
	}
	body := fmt.Sprintf(`# US-25 夹具：同项目内的一个额外 Worker（独立卷，可选独立主密钥挂载）。
services:
  %[1]s:
    profiles: [worker]
    image: ${BASALT_IMAGE:?set BASALT_IMAGE}
    user: "10001:10001"
    init: true
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    working_dir: /data
    stop_grace_period: 90s
    command: ["gateway"]
    volumes:
      - %[2]s-workspace:/data
      - %[2]s-private:/var/lib/basalt
%[3]s    tmpfs:
      - /tmp:rw,noexec,nosuid,size=256m
    secrets: [database_dsn]
    environment:
      XDG_DATA_HOME: /var/lib/basalt
      HOME: /var/lib/basalt
      BASALT_ROLE: worker
      BASALT_DATABASE_DSN_FILE: /run/secrets/database_dsn
      BASALT_MASTER_KEY_FILE: %[4]s
      BASALT_WORKER_NAME: %[5]s

volumes:
  %[2]s-workspace:
  %[2]s-private:
`, spec.Service, spec.VolumePrefix, keyMount, keyEnv, spec.Name)
	return s.addOverlay(t, body)
}
