//go:build e2e

package e2e

// US-25 容器验收 · 双角色编排（FR-25-12 / 13 / 14 / 15 / 16）。
//
// 五个用例各建一套独立 compose 项目（独立卷 / 端口 / secrets），跑真实镜像与真实
// `docker/compose.multiworker.yaml`；静态判据一律渲染原文件（不带测试 overlay），
// 只有"起得来"这一步用 overlay 去掉 PostgreSQL 的宿主端口。
// 缺容器宿主时按 REQUIRE_DOCKER 决定失败或 t.Skip，跳过即登记未验证。
//
// Five container-level cases over the real multi-worker compose file. Static verdicts
// render the shipped file; the test overlay only drops PostgreSQL's host port so
// consecutive stacks cannot fight over 5432.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// us25AppImageRef 是文件里应用镜像的**原文**占位形态：部署时由 `BASALT_IMAGE`
// 给成 `basalt:X.Y.Z@sha256:…`，因而镜像在文件层面就是"必须给 digest 引用"的形态。
// The app image placeholder, which forces a digest-pinned reference at deploy time.
const us25AppImageRef = "${BASALT_IMAGE:?set BASALT_IMAGE to basalt:X.Y.Z@sha256:...}"

// TestUS25_Compose_TwoRolesAndVolumes 判 FR-25-12：`--profile control` 起控制面与
// PostgreSQL，`--profile worker` 只起 Worker；Worker 容器内的 `gateway` 进程没有 HTTP
// 监听；两个角色各自持有独立的数据卷。
//
// 逆改点：给 Worker 加 `ports:`、把 Worker 命令改成 `serve`、或让两个角色共用同一个
// 数据卷 → profile 集合 / 端口 / 进程表 / 挂载表断言红。
func TestUS25_Compose_TwoRolesAndVolumes(t *testing.T) {
	s := us25NewStack(t, "roles")

	// ① profile 切分（渲染原文件）：control 三件、worker 一件，集合互不包含。
	control := s.profileServices(t, "control")
	worker := s.profileServices(t, "worker")
	requireSameSet(t, "control profile", control,
		[]string{us25PreflightService, us25PostgresService, us25ControlService})
	requireSameSet(t, "worker profile", worker, []string{us25WorkerService})
	for _, name := range worker {
		if strings.Contains(strings.Join(control, " "), name) {
			t.Fatalf("服务 %s 同时出现在两个 profile 里", name)
		}
	}

	// ② 角色命令：控制面 serve、Worker gateway；Worker 不发布端口。
	cfg := s.configJSON(t, s.stackFiles(), nil)
	requireStringList(t, "basalt-control.command",
		us25StringList(t, us25Service(t, cfg, us25ControlService)["command"], "command"),
		[]string{"serve"})
	requireStringList(t, "basalt-worker.command",
		us25StringList(t, us25Service(t, cfg, us25WorkerService)["command"], "command"),
		[]string{"gateway"})
	if ports := us25Ports(t, us25Service(t, cfg, us25WorkerService)); len(ports) != 0 {
		t.Fatalf("Worker 服务不得发布端口，实际 %d 条", len(ports))
	}

	// ③ 端到端：控制面首次启动 → 停写导出主密钥与种子 → 导入 Worker 卷 → 两角色同时运行。
	instanceID := s.startControl(t)
	_, seedPath := s.exportControlAssets(t)
	s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("重启控制面失败：%v", err)
	}
	if err := s.up(ctx, "worker"); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}

	if state := s.serviceState(ctx, us25ControlService); state != "running" {
		t.Fatalf("控制面状态 %q（要求 running）", state)
	}
	if health := s.healthState(ctx, us25ControlService); health != "healthy" {
		t.Fatalf("控制面健康状态 %q（要求 healthy）", health)
	}
	if state := s.serviceState(ctx, us25PostgresService); state != "running" {
		t.Fatalf("PostgreSQL 状态 %q（要求 running）", state)
	}
	if code, err := s.serviceExitCode(ctx, us25PreflightService); err != nil || code != 0 {
		t.Fatalf("预检容器退出码 %d / %v（要求 0）", code, err)
	}
	if state := s.serviceState(ctx, us25WorkerService); state != "running" {
		t.Fatalf("Worker 状态 %q（要求 running）", state)
	}

	// ④ Worker 不发布端口、容器内 8080 没有 HTTP 应答、进程是 gateway 不是 serve。
	if published, err := s.publishedPorts(ctx, us25WorkerService); err != nil {
		t.Fatalf("读取 Worker 端口映射失败：%v", err)
	} else if len(published) != 0 {
		t.Fatalf("Worker 容器发布了端口：%v", published)
	}
	if out, err := s.exec(ctx, us25WorkerService, "wget", "-q", "-T", "2", "-O", "/dev/null",
		"http://127.0.0.1:8080/api/v1/health"); err == nil {
		t.Fatalf("Worker 容器内 8080 有 HTTP 应答（输出 %q）——gateway 不得提供 HTTP 面", us18Tail(out, 500))
	}
	top, err := s.containerTop(ctx, us25WorkerService)
	if err != nil {
		t.Fatalf("读取 Worker 进程表失败：%v", err)
	}
	if !strings.Contains(top, "/opt/basalt/basalt gateway") {
		t.Fatalf("Worker 进程表里没有 gateway 进程：\n%s", top)
	}
	if strings.Contains(top, "/opt/basalt/basalt serve") {
		t.Fatalf("Worker 容器里跑了 serve：\n%s", top)
	}

	// ⑤ 两个角色各持独立数据卷；主密钥只读 bind 挂进 0700 目录；没有 docker socket。
	assertMount(t, s, ctx, us25ControlService, "/data", "volume",
		s.project+"_control-workspace", true)
	assertMount(t, s, ctx, us25ControlService, us25PrivateRoot, "volume",
		s.project+"_control-private", true)
	assertMount(t, s, ctx, us25WorkerService, "/data", "volume",
		s.project+"_worker-workspace", true)
	assertMount(t, s, ctx, us25WorkerService, us25PrivateRoot, "volume",
		s.project+"_worker-private", true)
	assertMount(t, s, ctx, us25WorkerService, us25KeyMountPath, "bind", "", false)

	for _, service := range []string{us25ControlService, us25WorkerService} {
		mounts, err := s.containerMounts(ctx, service)
		if err != nil {
			t.Fatalf("读取 %s 挂载失败：%v", service, err)
		}
		for _, mount := range mounts {
			if strings.Contains(mount.Source, "docker.sock") || strings.Contains(mount.Destination, "docker.sock") {
				t.Fatalf("%s 挂了 docker socket：%+v", service, mount)
			}
		}
	}

	// ⑥ 种子工作区里的名册仍是控制面的那份：Worker 没有铸出第二个实例。
	roster, err := s.volumeRead(ctx, s.project+"_worker-workspace", "/.basalt/instance.json")
	if err != nil {
		t.Fatalf("读取 Worker 卷内名册失败：%v", err)
	}
	if got := s.instanceID(t, roster); got != instanceID {
		t.Fatalf("Worker 卷内名册 instance_id=%s（要求与控制面一致 %s）", got, instanceID)
	}
}

// TestUS25_Compose_WorkerRequiresSeedAndKey 判 FR-25-13：缺种子、缺主密钥、密钥被放进
// 0755 父目录时 Worker 都以 78 退出，且不产生新实例、不写业务表；同一把正确密钥放在
// 0700 父目录 + 0400 文件时通过（判据的两侧都在本用例里）。
//
// 逆改点：去掉预检、预检失败后仍 `exec`、只判文件权限不判父目录、或让 Worker 自建实例
// → 退出码 / worker_nodes 行数 / 卷内文件断言红。
func TestUS25_Compose_WorkerRequiresSeedAndKey(t *testing.T) {
	s := us25NewStack(t, "gates")
	instanceID := s.startControl(t)
	keyPath, seedPath := s.exportControlAssets(t)

	// 三个负向服务与一个正向服务共用同一项目、同一网络与同一份 DSN secret。
	// KeyHostPath 为空表示"不挂主密钥"。
	s.addWorkerService(t, us25WorkerSpec{
		Service: "noseed", VolumePrefix: "noseed", Name: "noseed",
		KeyHostPath: keyPath,
	})
	s.addWorkerService(t, us25WorkerSpec{Service: "nokey", VolumePrefix: "nokey", Name: "nokey"})
	s.addWorkerService(t, us25WorkerSpec{
		Service: "widekey", VolumePrefix: "widekey", Name: "widekey",
		KeyHostPath: keyPath, KeyMountPath: us25KeyInSecretsPath, KeyEnvPath: us25KeyInSecretsPath,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// noseed：空卷（compose 建卷但不导入种子）。
	if err := s.createVolume(ctx, "noseed"); err != nil {
		t.Fatalf("创建 noseed 工作卷失败：%v", err)
	}
	noseedVolume := s.projectVolumeName(t, ctx, "noseed-workspace")
	// 其余三个都用控制面的种子。
	s.seedVolume(t, "nokey", "nokey-workspace", seedPath)
	s.seedVolume(t, "widekey", "widekey-workspace", seedPath)
	workerVolume := s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)

	if err := s.upDetached(ctx, "worker"); err != nil {
		t.Fatalf("启动 Worker 组失败：%v", err)
	}

	for _, service := range []string{"noseed", "nokey", "widekey"} {
		if code := s.awaitExit(t, service, us25ExitTimeout); code != 78 {
			logs, _ := s.logs(ctx, service)
			t.Fatalf("%s 退出码 %d（要求 78，EX_CONFIG），日志尾部：\n%s",
				service, code, us18Tail(logs, 3000))
		}
	}
	// 日志点名各自的拒绝理由：缺种子 ≠ 缺密钥 ≠ 父目录过宽。
	if logs, err := s.logs(ctx, "noseed"); err != nil || !strings.Contains(logs, "未种子") {
		t.Fatalf("noseed 日志没有点名「工作区未种子」：%v\n%s", err, us18Tail(logs, 2000))
	}
	for _, service := range []string{"nokey", "widekey"} {
		logs, err := s.logs(ctx, service)
		if err != nil || !strings.Contains(logs, "主密钥预检失败") {
			t.Fatalf("%s 日志没有点名主密钥预检失败：%v\n%s", service, err, us18Tail(logs, 2000))
		}
	}

	// 正向：种子 + 0700 父目录 / 0400 文件的密钥 → Worker 起得来；判据两侧都显式核对。
	waitFor(t, us25StartTimeout, func() bool {
		return s.healthState(ctx, us25WorkerService) == "healthy"
	})
	parentStat, err := s.containerStat(ctx, us25WorkerService, us25KeyParentPath)
	if err != nil {
		t.Fatalf("stat 主密钥父目录失败：%v", err)
	}
	if parentStat != "10001:10001:700" {
		t.Fatalf("主密钥父目录属性 %q（要求 10001:10001:700）", parentStat)
	}
	keyStat, err := s.containerStat(ctx, us25WorkerService, us25KeyMountPath)
	if err != nil {
		t.Fatalf("stat 主密钥失败：%v", err)
	}
	if keyStat != "10001:10001:400" {
		t.Fatalf("主密钥属性 %q（要求 10001:10001:400）", keyStat)
	}
	// 离线入口本身在容器里也能跑通（Worker 的预检走的就是它）。
	if out, err := s.exec(ctx, us25WorkerService, "/opt/basalt/basalt", "secrets", "check-key",
		"--path", us25KeyMountPath, "--workspace", "/data/.basalt"); err != nil {
		t.Fatalf("check-key 在正确密钥上失败：%v（%s）", err, us18Tail(out, 1000))
	}

	// 不产生新实例：空卷里没有名册、没有 SQLite 库（entrypoint 的 seed_config 只写配置）。
	if s.volumeHas(ctx, noseedVolume, "/.basalt/instance.json") {
		t.Fatalf("noseed 工作区被写入了实例名册")
	}
	if s.volumeHas(ctx, noseedVolume, "/.basalt/basalt.db") {
		t.Fatalf("noseed 工作区被建了 SQLite 库")
	}
	// 不写业务表：三个负向服务没有任何登记行，正向服务恰好一行。
	for _, name := range []string{"noseed", "nokey", "widekey"} {
		if got := s.psql(t, "SELECT count(*) FROM worker_nodes WHERE worker_name = '"+name+"'"); got != "0" {
			t.Fatalf("负向 Worker %s 写了 %s 行 worker_nodes（要求 0）", name, got)
		}
	}
	if got := s.psql(t, "SELECT count(*) FROM worker_nodes WHERE worker_name = 'worker-a'"); got != "1" {
		t.Fatalf("正向 Worker 登记行数 %s（要求 1）", got)
	}
	for _, table := range []string{"runs", "task_executions"} {
		if got := s.psql(t, "SELECT count(*) FROM "+table); got != "0" {
			t.Fatalf("%s 出现 %s 行——Worker 预检失败路径不得写业务表", table, got)
		}
	}
	// 控制面的名册没有被改写，Worker 卷里的名册就是它的副本。
	roster, err := s.volumeRead(ctx, workerVolume, "/.basalt/instance.json")
	if err != nil {
		t.Fatalf("读取 Worker 卷内名册失败：%v", err)
	}
	if got := s.instanceID(t, roster); got != instanceID {
		t.Fatalf("Worker 卷内名册 instance_id=%s（要求 %s）", got, instanceID)
	}
}

// TestUS25_Compose_DatabasePrivateAddress 判 FR-25-14：PostgreSQL 在单宿主只绑回环
// （缺省就是回环）、双宿主只绑给定的私网地址；`0.0.0.0` 被预检拒绝；Worker 侧没有
// 本地 PostgreSQL。
//
// 逆改点：改成 `0.0.0.0:5432`、去掉预检、或给 Worker 加 PG 服务 / 数据卷 → 端口映射、
// 预检退出码或服务清单断言红。
func TestUS25_Compose_DatabasePrivateAddress(t *testing.T) {
	s := us25NewStack(t, "dbaddr")

	cases := []struct {
		name string
		bind string
		want string
	}{
		{"loopback", "127.0.0.1", "127.0.0.1"},
		{"private", "10.41.0.7", "10.41.0.7"},
		{"empty-defaults-to-loopback", "", "127.0.0.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := s.configJSON(t, s.baseFiles(), map[string]string{"BASALT_DB_BIND": tc.bind})
			ports := us25Ports(t, us25Service(t, cfg, us25PostgresService))
			if len(ports) != 1 {
				t.Fatalf("PostgreSQL 端口发布 %d 条（要求 1 条）", len(ports))
			}
			entry, ok := ports[0].(map[string]any)
			if !ok {
				t.Fatalf("端口项不是对象：%T", ports[0])
			}
			hostIP, _ := entry["host_ip"].(string)
			if hostIP != tc.want {
				t.Fatalf("BASALT_DB_BIND=%q 的单宿主绑定是 %q（要求 %q）", tc.bind, hostIP, tc.want)
			}
			if hostIP == "0.0.0.0" {
				t.Fatalf("数据库绑到了所有网卡")
			}
			if target, _ := entry["target"].(float64); target != 5432 {
				t.Fatalf("容器内端口 %v（要求 5432）", entry["target"])
			}
		})
	}

	// 文件层面：端口发布行必须是 `${BASALT_DB_BIND:-127.0.0.1}` 的形态，不能有字面 0.0.0.0。
	body, err := os.ReadFile(filepath.Join(s.dir, "compose.multiworker.yaml"))
	if err != nil {
		t.Fatalf("读取 compose 文件失败：%v", err)
	}
	var portLines []string
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// 只取端口发布行：`- "<host>:5432:5432"` 形态。
		if strings.HasPrefix(line, "- \"") && strings.Contains(line, ":5432:5432") {
			portLines = append(portLines, line)
		}
	}
	if len(portLines) != 1 {
		t.Fatalf("找到 %d 条 5432 发布行（要求 1 条）：%v", len(portLines), portLines)
	}
	if !strings.Contains(portLines[0], `${BASALT_DB_BIND:-127.0.0.1}`) {
		t.Fatalf("5432 发布行不是由 BASALT_DB_BIND 决定的：%s", portLines[0])
	}
	if strings.Contains(portLines[0], "0.0.0.0") {
		t.Fatalf("5432 发布行写了 0.0.0.0：%s", portLines[0])
	}

	// 预检的 live 判据：0.0.0.0 与缺入口限流都被拒，私网地址通过。
	preflight := func(t *testing.T, overrides map[string]string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return s.composeWithEnv(ctx, overrides, "--profile", "control", "run", "--rm", "--no-deps",
			"-T", us25PreflightService)
	}
	t.Run("zero-address-rejected", func(t *testing.T) {
		out, err := preflight(t, map[string]string{"BASALT_DB_BIND": "0.0.0.0"})
		if err == nil {
			t.Fatalf("BASALT_DB_BIND=0.0.0.0 通过了预检：%s", us18Tail(out, 1000))
		}
		if code := us25ExitCode(err); code != 1 {
			t.Fatalf("预检退出码 %d（要求 1），输出：\n%s", code, us18Tail(out, 2000))
		}
		if !strings.Contains(out, "0.0.0.0") {
			t.Fatalf("预检输出没有点名 0.0.0.0：\n%s", us18Tail(out, 2000))
		}
	})
	// IPv6 的 `[::]` 与 0.0.0.0 等价（Docker 在其上同时接受 IPv4 映射连接），必须同样
	// 拒绝，否则双宿主部署可以绕过 IPv4 判据把数据库暴露到所有网卡。
	// `[::]` is IPv6's all-interfaces form and passes IPv4-mapped connections, so it
	// must be rejected exactly like 0.0.0.0.
	t.Run("ipv6-zero-address-rejected", func(t *testing.T) {
		out, err := preflight(t, map[string]string{"BASALT_DB_BIND": "[::]"})
		if err == nil {
			t.Fatalf("BASALT_DB_BIND=[::] 通过了预检：%s", us18Tail(out, 1000))
		}
		if code := us25ExitCode(err); code != 1 {
			t.Fatalf("预检退出码 %d（要求 1），输出：\n%s", code, us18Tail(out, 2000))
		}
		if !strings.Contains(out, "[::]") {
			t.Fatalf("预检输出没有点名 [::]：\n%s", us18Tail(out, 2000))
		}
	})
	t.Run("private-address-accepted", func(t *testing.T) {
		out, err := preflight(t, map[string]string{"BASALT_DB_BIND": "10.41.0.7"})
		if err != nil {
			t.Fatalf("BASALT_DB_BIND=10.41.0.7 被预检拒绝：%v（%s）", err, us18Tail(out, 2000))
		}
	})
	// 空值与 `0.0.0.0` 都是"会把数据库暴露到所有网卡"的取值：端口行的 `:-127.0.0.1`
	// 只是第二道防线，预检本身必须拒绝空值（与 `--profile worker` 的渲染无关）。
	// An empty bind is rejected by the preflight itself; the compose default is only
	// the second line of defence.
	t.Run("empty-address-rejected", func(t *testing.T) {
		out, err := preflight(t, map[string]string{"BASALT_DB_BIND": ""})
		if err == nil {
			t.Fatalf("空 BASALT_DB_BIND 通过了预检：%s", us18Tail(out, 1000))
		}
		if code := us25ExitCode(err); code != 1 {
			t.Fatalf("预检退出码 %d（要求 1），输出：\n%s", code, us18Tail(out, 2000))
		}
		if !strings.Contains(out, "为空") {
			t.Fatalf("预检输出没有点名空值：\n%s", us18Tail(out, 2000))
		}
	})
	t.Run("missing-ingress-rejected", func(t *testing.T) {
		out, err := preflight(t, map[string]string{"BASALT_INGRESS_RUN_QPS": ""})
		if err == nil {
			t.Fatalf("缺 BASALT_INGRESS_RUN_QPS 也通过了预检：%s", us18Tail(out, 1000))
		}
		if !strings.Contains(out, "BASALT_INGRESS") {
			t.Fatalf("预检输出没有点名入口限流：\n%s", us18Tail(out, 2000))
		}
	})

	// Worker 侧没有本地 PostgreSQL：不挂 PG 数据卷、不用 PG 镜像，profile 清单里也没有它。
	cfg := s.configJSON(t, s.stackFiles(), nil)
	workerDef := us25Service(t, cfg, us25WorkerService)
	for _, mount := range us25Mounts(t, workerDef) {
		if strings.Contains(mount.Source, "postgres") {
			t.Fatalf("Worker 挂了 PostgreSQL 数据卷：%+v", mount)
		}
	}
	if image, _ := workerDef["image"].(string); strings.Contains(image, "postgres") {
		t.Fatalf("Worker 用了 PostgreSQL 镜像：%s", image)
	}
}

// TestUS25_Compose_ReadinessAndRestartPolicy 判 FR-25-15：控制面健康检查打
// `/api/v1/health`；Worker 健康检查**自行**解析 DSN secret 后跑只读的 `workers list`
// （只表示数据库可达），两服务都没有自动重启策略。
//
// 逆改点：给 Worker 加 HTTP 探活、让探活读未经导出的环境变量、或加 `restart: always`
// → compose 渲染断言或"杀掉后不复活"断言红。
func TestUS25_Compose_ReadinessAndRestartPolicy(t *testing.T) {
	s := us25NewStack(t, "ready")

	cfg := s.configJSON(t, s.stackFiles(), nil)
	controlDef := us25Service(t, cfg, us25ControlService)
	workerDef := us25Service(t, cfg, us25WorkerService)

	controlCheck := us25HealthTest(t, controlDef)
	requireStringList(t, "basalt-control.healthcheck.test", controlCheck,
		[]string{"CMD", "wget", "-qO-", "http://127.0.0.1:8080/api/v1/health"})

	workerCheck := us25HealthTest(t, workerDef)
	if len(workerCheck) != 2 || workerCheck[0] != "CMD-SHELL" {
		t.Fatalf("Worker 健康检查形态 %q（要求 [CMD-SHELL <脚本>]）", workerCheck)
	}
	probe := workerCheck[1]
	for _, want := range []string{"/run/secrets/database_dsn", "/opt/basalt/basalt workers list"} {
		if !strings.Contains(probe, want) {
			t.Fatalf("Worker 探活没有自己解析 DSN 或没跑 workers list（缺 %q）：%s", want, probe)
		}
	}
	for _, banned := range []string{"http://", "https://", "wget", "curl", "/api/v1/health"} {
		if strings.Contains(probe, banned) {
			t.Fatalf("Worker 探活出现了 HTTP 面（%q）：%s", banned, probe)
		}
	}
	// 两服务都不得声明自动重启策略（渲染结果里连键都不该有）。
	for _, name := range []string{us25PreflightService, us25PostgresService, us25ControlService, us25WorkerService} {
		if _, ok := us25Service(t, cfg, name)["restart"]; ok {
			t.Fatalf("服务 %s 声明了 restart 策略——接管靠租约，不靠自动重启", name)
		}
	}

	// 端到端：两个角色都健康；控制面的 HTTP 面真的应答；Worker 的探活命令真的能跑。
	_ = s.startControl(t)
	_, seedPath := s.exportControlAssets(t)
	s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("重启控制面失败：%v", err)
	}
	if err := s.up(ctx, "worker"); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}
	waitFor(t, us25StartTimeout, func() bool {
		return s.healthState(ctx, us25ControlService) == "healthy" &&
			s.healthState(ctx, us25WorkerService) == "healthy"
	})

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", s.port)
	response, err := (&http.Client{Timeout: 10 * time.Second}).Get(healthURL)
	if err != nil {
		t.Fatalf("控制面健康端点不可达（%s）：%v", healthURL, err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("健康端点返回 %d（要求 200）", response.StatusCode)
	}
	if out, err := s.exec(ctx, us25WorkerService, "sh", "-c",
		`export BASALT_DATABASE_DSN="$(cat /run/secrets/database_dsn)" && /opt/basalt/basalt workers list >/dev/null`); err != nil {
		t.Fatalf("Worker 探活命令失败：%v（%s）", err, us18Tail(out, 1000))
	}

	// 无自动重启：杀掉 Worker，它在观察窗口内必须保持 exited，不得复活。
	workerID, err := s.serviceContainerID(ctx, us25WorkerService)
	if err != nil {
		t.Fatalf("取 Worker 容器失败：%v", err)
	}
	if _, err := us18DockerOutput(ctx, "kill", workerID); err != nil {
		t.Fatalf("杀掉 Worker 失败：%v", err)
	}
	if code := s.awaitExit(t, us25WorkerService, us25ExitTimeout); code != 137 {
		t.Fatalf("被杀 Worker 退出码 %d（要求 137）", code)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if state := s.serviceState(ctx, us25WorkerService); state != "exited" {
			t.Fatalf("Worker 被自动重启了：状态 %q（要求 exited，无 restart 策略）", state)
		}
		time.Sleep(time.Second)
	}
}

// TestUS25_Compose_SecurityInvariants 判 FR-25-16：两个角色都满足 user 10001:10001、
// read_only、cap_drop ALL、no-new-privileges、init、tmpfs、无 docker socket、镜像固定
// digest（应用镜像由 `${BASALT_IMAGE:?…}` 强制带 digest，数据库与 lock 文件逐字一致）。
//
// 逆改点：删任一字段、把镜像改成浮动 tag、或把根文件系统改成可写 → 逐字段断言红。
func TestUS25_Compose_SecurityInvariants(t *testing.T) {
	s := us25NewStack(t, "secure")
	root := us18RepoRoot(t)
	lock := mustImageLock(t, root)

	cfg := s.configJSON(t, s.stackFiles(), nil)
	for _, name := range []string{us25ControlService, us25WorkerService} {
		def := us25Service(t, cfg, name)
		if user, _ := def["user"].(string); user != "10001:10001" {
			t.Fatalf("%s 的 user 是 %q（要求 10001:10001）", name, user)
		}
		if readOnly, _ := def["read_only"].(bool); !readOnly {
			t.Fatalf("%s 没有 read_only", name)
		}
		if init, _ := def["init"].(bool); !init {
			t.Fatalf("%s 没有 init: true（僵尸进程与信号转发）", name)
		}
		if caps := us25StringList(t, def["cap_drop"], name+".cap_drop"); !containsString(caps, "ALL") {
			t.Fatalf("%s 的 cap_drop 是 %v（要求 ALL）", name, caps)
		}
		if opts := us25StringList(t, def["security_opt"], name+".security_opt"); !containsString(opts, "no-new-privileges:true") {
			t.Fatalf("%s 的 security_opt 是 %v（要求 no-new-privileges:true）", name, opts)
		}
		tmpfs := us25StringList(t, def["tmpfs"], name+".tmpfs")
		if len(tmpfs) == 0 || !strings.HasPrefix(tmpfs[0], "/tmp") {
			t.Fatalf("%s 的 tmpfs 是 %v（要求 /tmp 可写）", name, tmpfs)
		}
	}

	// 一次性预检服务拿的是同一应用镜像、跑的是启动前判据，同样必须私有化；它只读且
	// 不写文件，因此 tmpfs 不适用，其余五项逐项断言。
	// The one-shot preflight runs the same image and must be hardened too; tmpfs does
	// not apply because it never writes, the other five fields do.
	preflightDef := us25Service(t, cfg, us25PreflightService)
	if user, _ := preflightDef["user"].(string); user != "10001:10001" {
		t.Fatalf("%s 的 user 是 %q（要求 10001:10001）", us25PreflightService, user)
	}
	if readOnly, _ := preflightDef["read_only"].(bool); !readOnly {
		t.Fatalf("%s 没有 read_only", us25PreflightService)
	}
	if init, _ := preflightDef["init"].(bool); !init {
		t.Fatalf("%s 没有 init: true", us25PreflightService)
	}
	if caps := us25StringList(t, preflightDef["cap_drop"], us25PreflightService+".cap_drop"); !containsString(caps, "ALL") {
		t.Fatalf("%s 的 cap_drop 是 %v（要求 ALL）", us25PreflightService, caps)
	}
	if opts := us25StringList(t, preflightDef["security_opt"], us25PreflightService+".security_opt"); !containsString(opts, "no-new-privileges:true") {
		t.Fatalf("%s 的 security_opt 是 %v（要求 no-new-privileges:true）", us25PreflightService, opts)
	}

	// 示例环境变量文件：变量集合固定、默认绑定是回环、凭据零出现。它不在镜像里，
	// 但运维拿它复制成 `.env`，写错默认值等于把数据库暴露出去。
	// The example env file must keep the documented variable set, a loopback default
	// and no credential values.
	envExample, err := os.ReadFile(filepath.Join(root, "docker", ".env.multiworker.example"))
	if err != nil {
		t.Fatalf("读取 docker/.env.multiworker.example 失败：%v", err)
	}
	envText := string(envExample)
	for _, want := range []string{
		"BASALT_IMAGE=", "BASALT_DB_IMAGE=", "BASALT_DB_BIND=127.0.0.1",
		"BASALT_HTTP_BIND=", "BASALT_HTTP_PORT=", "POSTGRES_DB=", "POSTGRES_USER=",
		"BASALT_INGRESS_RUN_QPS=", "BASALT_INGRESS_RUN_BURST=",
		"BASALT_INGRESS_INGEST_QPS=", "BASALT_INGRESS_INGEST_BURST=",
		"BASALT_INGRESS_READ_QPS=", "BASALT_INGRESS_READ_BURST=",
		"BASALT_WORKER_NAME=",
	} {
		if !strings.Contains(envText, want) {
			t.Fatalf("docker/.env.multiworker.example 缺少 %q", want)
		}
	}
	for _, banned := range []string{"POSTGRES_PASSWORD=", "BASALT_MASTER_KEY=", "MASTER_KEY=", "BASALT_DATABASE_DSN="} {
		if strings.Contains(envText, banned) {
			t.Fatalf("docker/.env.multiworker.example 出现凭据类赋值 %q——凭据只经 docker/secrets/ 文件提供", banned)
		}
	}
	if strings.Contains(envText, "BASALT_DB_BIND=0.0.0.0") {
		t.Fatalf("docker/.env.multiworker.example 的默认数据库绑定是 0.0.0.0")
	}

	// 文件层面的镜像钉住：每个 image 都是"必须给 digest 的应用占位"或带 digest 的固定引用。
	refs := imageRefsInFile(t, filepath.Join(root, "docker", "compose.multiworker.yaml"))
	if len(refs) != 4 {
		t.Fatalf("compose 文件里有 %d 个 image 引用（要求 4 个）：%v", len(refs), refs)
	}
	postgresRefs := 0
	for _, ref := range refs {
		switch {
		case ref == us25AppImageRef:
		case ref == lock.Postgres:
			postgresRefs++
		case strings.Contains(ref, "@sha256:"):
			t.Fatalf("出现第三个镜像引用 %q——本文件只允许应用镜像与锁定数据库镜像", ref)
		default:
			t.Fatalf("镜像引用没有钉住 digest：%q", ref)
		}
	}
	if postgresRefs != 1 {
		t.Fatalf("数据库镜像出现 %d 次（要求 1 次），且必须与 image-lock.json 逐字一致", postgresRefs)
	}
	if !strings.Contains(us25AppImageRef, "@sha256:") {
		t.Fatalf("应用镜像占位没有强制 digest 形态：%s", us25AppImageRef)
	}

	// 端到端：真实容器上逐项复核；read_only 用"根目录写不进去、/tmp 与 /data 能写"证明。
	_ = s.startControl(t)
	_, seedPath := s.exportControlAssets(t)
	s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := s.up(ctx, "control"); err != nil {
		t.Fatalf("重启控制面失败：%v", err)
	}
	if err := s.up(ctx, "worker"); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}

	for _, service := range []string{us25ControlService, us25WorkerService} {
		id, err := s.serviceContainerID(ctx, service)
		if err != nil {
			t.Fatalf("取 %s 容器失败：%v", service, err)
		}
		fields := map[string]string{
			"user":      "{{.Config.User}}",
			"read_only": "{{.HostConfig.ReadonlyRootfs}}",
			"init":      "{{.HostConfig.Init}}",
			"restart":   "{{.HostConfig.RestartPolicy.Name}}",
			"cap_drop":  "{{range .HostConfig.CapDrop}}{{.}} {{end}}",
			"sec_opt":   "{{range .HostConfig.SecurityOpt}}{{.}} {{end}}",
			"tmpfs":     "{{range $k, $v := .HostConfig.Tmpfs}}{{$k}} {{end}}",
		}
		values := map[string]string{}
		for key, tmpl := range fields {
			out, err := us18DockerOutput(ctx, "inspect", "--format", tmpl, id)
			if err != nil {
				t.Fatalf("inspect %s 的 %s 失败：%v", service, key, err)
			}
			values[key] = strings.TrimSpace(out)
		}
		if values["user"] != "10001:10001" {
			t.Fatalf("%s 容器实际 user=%q", service, values["user"])
		}
		if values["read_only"] != "true" {
			t.Fatalf("%s 容器实际 read_only=%q", service, values["read_only"])
		}
		if values["init"] != "true" {
			t.Fatalf("%s 容器实际 init=%q", service, values["init"])
		}
		if values["restart"] != "" && values["restart"] != "no" {
			t.Fatalf("%s 容器实际 restart=%q（要求 no）", service, values["restart"])
		}
		if !strings.Contains(values["cap_drop"], "ALL") {
			t.Fatalf("%s 容器实际 cap_drop=%q", service, values["cap_drop"])
		}
		if !strings.Contains(values["sec_opt"], "no-new-privileges:true") {
			t.Fatalf("%s 容器实际 security_opt=%q", service, values["sec_opt"])
		}
		if !strings.Contains(values["tmpfs"], "/tmp") {
			t.Fatalf("%s 容器实际 tmpfs=%q", service, values["tmpfs"])
		}
	}

	// 根文件系统只读的实证：根目录写失败、/tmp 与 /data 写成功。
	if out, err := s.exec(ctx, us25WorkerService, "touch", "/basalt-readonly-probe"); err == nil {
		t.Fatalf("Worker 的根文件系统可写（touch / 成功）：%s", out)
	}
	if out, err := s.exec(ctx, us25WorkerService, "touch", "/tmp/basalt-probe"); err != nil {
		t.Fatalf("Worker 的 /tmp 不可写：%v（%s）", err, out)
	}
	if out, err := s.exec(ctx, us25WorkerService, "touch", "/data/basalt-probe"); err != nil {
		t.Fatalf("Worker 的 /data 不可写：%v（%s）", err, out)
	}
}

// profileServices 用 `docker compose config --services` 取某个 profile 的服务清单。
// profileServices lists a profile's services through `compose config --services`.
func (s *us25Stack) profileServices(t *testing.T, profile string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := s.compose(ctx, "--profile", profile, "config", "--services")
	if err != nil {
		t.Fatalf("列 %s profile 的服务失败：%v（%s）", profile, err, us18Tail(out, 2000))
	}
	names := us18NonEmptyLines(out)
	sort.Strings(names)
	return names
}

// requireSameSet 判两个字符串集合逐项相等（排序后比较）。
// requireSameSet compares two string sets after sorting.
func requireSameSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	requireStringList(t, label, g, w)
}

// requireStringList 判两个字符串数组逐项相等。
// requireStringList compares two string slices element by element.
func requireStringList(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s 是 %v（要求 %v）", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s 是 %v（要求 %v）", label, got, want)
		}
	}
}

// assertMount 核对运行中容器的某个挂载点（类型 / 卷名 / 读写）。
// assertMount checks one live mount of a running container.
func assertMount(t *testing.T, s *us25Stack, ctx context.Context,
	service, destination, mountType, source string, readWrite bool,
) {
	t.Helper()
	mounts, err := s.containerMounts(ctx, service)
	if err != nil {
		t.Fatalf("读取 %s 挂载失败：%v", service, err)
	}
	mount := us25LiveMountAt(mounts, destination)
	if mount == nil {
		t.Fatalf("%s 没有挂 %s（实际 %+v）", service, destination, mounts)
	}
	if mount.Type != mountType {
		t.Fatalf("%s 的 %s 是 %s 挂载（要求 %s）", service, destination, mount.Type, mountType)
	}
	// 命名卷比卷名（.Name），bind 比宿主路径（.Source）——命名卷的 .Source 是宿主路径，
	// 直接比卷名会永远不等。
	// Named volumes are matched by name; bind mounts by host path.
	actualSource := mount.Source
	if mountType == "volume" {
		actualSource = mount.Name
	}
	if source != "" && actualSource != source {
		t.Fatalf("%s 的 %s 来自 %s（要求 %s）", service, destination, actualSource, source)
	}
	if mount.ReadWrite != readWrite {
		t.Fatalf("%s 的 %s 读写标志是 %v（要求 %v）", service, destination, mount.ReadWrite, readWrite)
	}
}

// imageRefsInFile 按行提取 compose 文件里的 image 取值（跳过注释与空行）。
// imageRefsInFile collects every image reference in a compose file.
func imageRefsInFile(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	var refs []string
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		if value, ok := strings.CutPrefix(line, "image:"); ok {
			refs = append(refs, strings.TrimSpace(value))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("扫描 %s 失败：%v", path, err)
	}
	return refs
}

// us25ExitCode 取命令的退出码；不是 ExitError 时返回 -1。
// us25ExitCode extracts the process exit code, or -1 when it is not an exit error.
func us25ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
