//go:build e2e

package e2e

// US-25 容器验收 · 首次安装链路（FR-25-01 / 02 / 05 / 06）。
//
// 三条用例各占一个真实容器的阶段：
//   - InitThenServe：在**全新卷**上先跑 `init`，再起控制面；判三件同在、实例标识不变、
//     主密钥与访问密码各落一次、进度记录从 pending 走到 minted。
//   - RetryAfterIngressExit：入口限流为空时首次 serve 必须退出且**不铸钥**，补上配置后
//     第二次启动补发主密钥——不存在"已有实例、永远没有密钥"的死局。
//   - WorkerSeedSkipsBootstrap：Worker 从控制面种子启动后不建库、不写名册、不铸钥、
//     不发密码，只登记一行。
//
// 分工说明（避免把判据说大）：`init` 的**执行顺序**（骨架 → 首次准备 → 引导建库）与
// 端口缺失时的失败关闭由 `internal/cli/init_test.go` 的 recording store 逐调用断言；
// 本文件判的是容器里可观察的**结果**——首次准备会不会留下三件、serve 会不会补发密钥。
// 容器里 `init` 之前已由入口脚本按角色播种配置（`seed_config` 从不覆盖），因此
// `config_created=false`：这正是部署形态，不是缺项。
//
// The container cases judge the observable end state of first install: three artefacts,
// an unchanged instance id, exactly one key and one admin password, and a worker that
// only registers. The execution order inside `init` is asserted by the unit test with a
// recording store; the entrypoint seeds the role's config before `init`, so
// `config_created=false` here by design.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// us25IngressEnv 是控制面要求的六项入口限流环境变量（与 compose 文件逐字一致）。
// The six ingress variables the control plane requires, verbatim from the compose file.
var us25IngressEnv = []string{
	"BASALT_INGRESS_RUN_QPS", "BASALT_INGRESS_RUN_BURST",
	"BASALT_INGRESS_INGEST_QPS", "BASALT_INGRESS_INGEST_BURST",
	"BASALT_INGRESS_READ_QPS", "BASALT_INGRESS_READ_BURST",
}

// TestUS25_FirstInstall_InitThenServe 判 FR-25-01 / 05：全新工作区先 `init` 再 `serve`，
// 三件（配置、名册、SQLite 源库）同在；serve 首次启动铸主密钥、发一次访问密码，
// 名册上的实例标识不变。
//
// 逆改点：把首次准备挪到引导建库之后 → 名册与进度记录缺失（本用例在 init 输出与卷
// 内容处红）；让 serve 每次启动都铸钥 / 重发密码 → 进度记录不会停在 minted。
func TestUS25_FirstInstall_InitThenServe(t *testing.T) {
	s := us25NewStack(t, "first-init")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	workspaceVol := s.project + "_control-workspace"
	privateVol := s.project + "_control-private"

	// ① 全新卷上跑 init：容器里就是运维敲的那条命令。
	initOut, err := s.runOneOff(ctx, us25ControlProfile, nil, us25ControlService, "init")
	if err != nil {
		t.Fatalf("init 失败：%v（%s）", err, us18Tail(initOut, 4000))
	}
	for _, want := range []string{
		// 配置由入口脚本先播种，init 本次真正写出的是进度与名册——如实逐项报。
		"首次安装：新建 初始化进度 / 实例名册",
		"已创建（schema v6）",
	} {
		if !strings.Contains(initOut, want) {
			t.Fatalf("init 输出缺少 %q：\n%s", want, us18Tail(initOut, 4000))
		}
	}
	// 三件同在：配置、名册、SQLite 源库。
	for _, path := range []string{
		"/basalt.yaml",
		"/.basalt/instance.json",
		"/.basalt/basalt.db",
	} {
		if !s.volumeHas(ctx, workspaceVol, path) {
			t.Fatalf("init 之后工作区缺少 %s", path)
		}
	}
	roster, err := s.volumeRead(ctx, workspaceVol, "/.basalt/instance.json")
	if err != nil {
		t.Fatalf("读取名册失败：%v", err)
	}
	inst := s.instanceID(t, roster)

	// ② 起控制面：首次启动补发主密钥与访问密码，实例标识沿用 init 写下的那个。
	if err := s.up(ctx, us25ControlProfile); err != nil {
		t.Fatalf("启动控制面失败：%v", err)
	}
	if state := s.healthState(ctx, us25ControlService); state != "healthy" {
		t.Fatalf("控制面健康状态 %q（要求 healthy）", state)
	}
	after, err := s.volumeRead(ctx, workspaceVol, "/.basalt/instance.json")
	if err != nil {
		t.Fatalf("serve 之后读取名册失败：%v", err)
	}
	if got := s.instanceID(t, after); got != inst {
		t.Fatalf("serve 改写了实例标识：%s → %s", inst, got)
	}
	if !s.volumeHas(ctx, workspaceVol, "/.basalt/basalt.db") {
		t.Fatalf("serve 之后 SQLite 源库消失——serve 不得改动 init 的产物")
	}

	// ③ 私有卷：主密钥 0400、访问密码在、进度记录走到 minted。
	keyPath := "/basalt/instances/" + inst + "/master.key"
	stat, err := s.volumeStat(ctx, privateVol, keyPath)
	if err != nil {
		t.Fatalf("首次启动没有生成主密钥：%v", err)
	}
	// 卷内的主密钥由 `instance_secrets_publish.go` 以 `O_EXCL` + `0o600` 写出；判据是
	// `mode & 0o077 == 0`（0400 / 0600 都通过，C-08 的固定 `0400` 针对的是
	// `export-master-key.sh` 导出的宿主副本）。这里按实测形态判，不冒充导出口径。
	// The in-volume key is published 0600; the C-08 `0400` belongs to the exported copy.
	if stat != "10001:10001:600" {
		t.Fatalf("主密钥属性 %s（要求 10001:10001:600）", stat)
	}
	record, err := s.volumeRead(ctx, privateVol, "/basalt/instances/"+inst+"/provisioning.json")
	if err != nil {
		t.Fatalf("读取初始化进度失败：%v", err)
	}
	if !strings.Contains(record, `"admin_token": "minted"`) {
		t.Fatalf("初始化进度没有走到 minted：%s", record)
	}
	if !s.volumeHas(ctx, privateVol, "/basalt/instances/"+inst+"/admin-token") {
		t.Fatalf("首次启动没有写出管理台访问密码文件")
	}

	// ④ 同一次启动必须把"为什么铸了钥""密码写在哪"说清楚。
	logs, err := s.logs(ctx, us25ControlService)
	if err != nil {
		t.Fatalf("读取控制面日志失败：%v", err)
	}
	for _, want := range []string{
		"实例加密：主密钥已生成并写入初始化记录",
		"管理台访问密码已写入",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("控制面日志缺少 %q：\n%s", want, us18Tail(logs, 6000))
		}
	}
}

// TestUS25_FirstInstall_RetryAfterIngressExit 判 FR-25-02 / 03：入口限流为空时首次
// serve 在监听前退出，工作区保留名册与 provisioning.pending；补上六项后第二次启动
// 的密钥判定返回 create 并生成主密钥。
//
// 逆改点：首次失败时回滚名册 / 删进度记录 → 第二次启动会被判成"全新实例"（判据仍在
// 但 FR-25-02 的续做语义丢了）；失败时顺手铸钥 → 主密钥非空断言红。
func TestUS25_FirstInstall_RetryAfterIngressExit(t *testing.T) {
	s := us25NewStack(t, "first-retry")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	workspaceVol := s.project + "_control-workspace"
	privateVol := s.project + "_control-private"

	// ① 六项入口限流全空：模板里 `${BASALT_INGRESS_*}` 展开为空，配置装载即失败，
	//    进程在监听之前退出（实测输出点名配置项类型非法——不是启动后再拒请求）。
	empty := make(map[string]string, len(us25IngressEnv))
	for _, name := range us25IngressEnv {
		empty[name] = ""
	}
	out, err := s.runOneOff(ctx, us25ControlProfile, empty, us25ControlService, "serve")
	if err == nil {
		t.Fatalf("入口限流全空仍启动成功：%s", us18Tail(out, 4000))
	}
	if !strings.Contains(out, "basalt.yaml") {
		t.Fatalf("失败输出没有点名配置文件：\n%s", us18Tail(out, 6000))
	}

	// ② 失败之后的状态：名册在、进度记录 pending、没有密钥也没有密码。
	roster, err := s.volumeRead(ctx, workspaceVol, "/.basalt/instance.json")
	if err != nil {
		t.Fatalf("首次 serve 失败后工作区没有名册：%v", err)
	}
	inst := s.instanceID(t, roster)
	// serve 从不建 SQLite 源库（那是 `init` 的职责）：这里既没跑 init，就不该有库文件。
	// serve never creates the SQLite source database; only init does.
	if s.volumeHas(ctx, workspaceVol, "/.basalt/basalt.db") {
		t.Fatalf("首次 serve 失败后出现了 basalt.db——serve 不建源库")
	}
	record, err := s.volumeRead(ctx, privateVol, "/basalt/instances/"+inst+"/provisioning.json")
	if err != nil {
		t.Fatalf("首次 serve 失败后没有初始化进度记录：%v", err)
	}
	if !strings.Contains(record, `"admin_token": "pending"`) {
		t.Fatalf("初始化进度不是 pending：%s", record)
	}
	keyPath := "/basalt/instances/" + inst + "/master.key"
	if s.volumeHas(ctx, privateVol, keyPath) {
		t.Fatalf("首次 serve 已经失败，却生成了主密钥——失败路径不得铸钥")
	}
	if s.volumeHas(ctx, privateVol, "/basalt/instances/"+inst+"/admin-token") {
		t.Fatalf("首次 serve 已经失败，却写出了访问密码")
	}

	// ③ 补齐六项后重试：同一个工作区、同一个实例标识、补发主密钥与密码。
	if err := s.up(ctx, us25ControlProfile); err != nil {
		t.Fatalf("补上入口限流后启动失败：%v", err)
	}
	if state := s.healthState(ctx, us25ControlService); state != "healthy" {
		t.Fatalf("重试后控制面健康状态 %q（要求 healthy）", state)
	}
	retried, err := s.volumeRead(ctx, workspaceVol, "/.basalt/instance.json")
	if err != nil {
		t.Fatalf("重试后读取名册失败：%v", err)
	}
	if got := s.instanceID(t, retried); got != inst {
		t.Fatalf("重试换了实例标识：%s → %s", inst, got)
	}
	if stat, err := s.volumeStat(ctx, privateVol, keyPath); err != nil {
		t.Fatalf("重试没有补发主密钥：%v", err)
	} else if stat != "10001:10001:600" {
		t.Fatalf("补发的主密钥属性 %s（要求 10001:10001:600）", stat)
	}
	record, err = s.volumeRead(ctx, privateVol, "/basalt/instances/"+inst+"/provisioning.json")
	if err != nil {
		t.Fatalf("重试后读取初始化进度失败：%v", err)
	}
	if !strings.Contains(record, `"admin_token": "minted"`) {
		t.Fatalf("重试后进度记录没有走到 minted：%s", record)
	}
	logs, err := s.logs(ctx, us25ControlService)
	if err != nil {
		t.Fatalf("读取控制面日志失败：%v", err)
	}
	if !strings.Contains(logs, "首次初始化未完成：配置与实例名册已在盘上") {
		t.Fatalf("重试启动没有说明这是续做：\n%s", us18Tail(logs, 6000))
	}
	if !strings.Contains(logs, "实例加密：主密钥已生成并写入初始化记录") {
		t.Fatalf("重试启动没有铸主密钥的记录：\n%s", us18Tail(logs, 6000))
	}
}

// TestUS25_FirstInstall_WorkerSeedSkipsBootstrap 判 FR-25-06：Worker 从控制面种子
// 启动后不写名册、不建 SQLite 库、不铸钥、不发访问密码，只登记一行 worker_nodes。
//
// 逆改点：让 gateway 走 serve 的首次准备 → 名册被改写 / 私有卷出现 master.key /
// 工作区多出 basalt.db，三条断言依次红。
func TestUS25_FirstInstall_WorkerSeedSkipsBootstrap(t *testing.T) {
	s := us25NewStack(t, "first-worker")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	inst := s.startControl(t)
	// 控制面先登记自己那一行（`serve` 也走 `startWorkerRuntime`）；必须在起 Worker 之前
	// 取，否则 SELECT 会一次返回两行、拿不到单一对照值。停机只置 stopped_at，行还在。
	controlID := strings.TrimSpace(s.psql(t, "SELECT instance_id FROM worker_nodes"))
	if controlID == "" {
		t.Fatalf("控制面没有登记 worker_nodes 行")
	}
	_, seedPath := s.exportControlAssets(t)
	workerVol := s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)
	workerPrivateVol := s.projectVolumeName(t, ctx, "worker-private")

	if err := s.up(ctx, us25WorkerProfile); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}

	// ① 名册来自种子：实例标识与控制面一致，一个字节都没被改写。
	roster, err := s.volumeRead(ctx, workerVol, "/.basalt/instance.json")
	if err != nil {
		t.Fatalf("Worker 工作区没有名册：%v", err)
	}
	if got := s.instanceID(t, roster); got != inst {
		t.Fatalf("Worker 名册的实例标识 %s（控制面 %s）", got, inst)
	}

	// ② 不建 SQLite 源库：Worker 不做引导建库。
	if s.volumeHas(ctx, workerVol, "/.basalt/basalt.db") {
		t.Fatalf("Worker 工作区出现了 basalt.db——gateway 不得建源库")
	}

	// ③ 不铸钥、不发密码：私有卷里三件（主密钥 / 密码 / 进度）一个都不许有。
	listing, err := s.execInVolume(ctx, workerPrivateVol,
		"find /data \\( -name master.key -o -name admin-token -o -name provisioning.json \\) -print")
	if err != nil {
		t.Fatalf("列出 Worker 私有卷失败：%v", err)
	}
	if trimmed := strings.TrimSpace(listing); trimmed != "" {
		t.Fatalf("Worker 私有卷出现了实例私密文件：\n%s", trimmed)
	}

	// ④ 只登记一行。控制面自己也是 Worker（`run.go` 的 `needsSchedules` 覆盖 serve 与
	//    gateway，两条路径都走 `startWorkerRuntime`），所以总数是**控制面那一行 + 本
	//    Worker 新增的一行**：不多不少，且新增行的名称来自 Worker 配置。
	// The control plane registers too; exactly one *new* row may come from this worker.
	rows := us18NonEmptyLines(s.psql(t,
		"SELECT instance_id || '|' || coalesce(worker_name, '') FROM worker_nodes ORDER BY instance_id"))
	if len(rows) != 2 {
		t.Fatalf("worker_nodes 行数 %d（要求 2：控制面 + 本 Worker）：%v", len(rows), rows)
	}
	added := 0
	for _, row := range rows {
		parts := strings.SplitN(row, "|", 2)
		if parts[0] == controlID {
			continue
		}
		added++
		if !us25WorkerIDPattern.MatchString(parts[0]) {
			t.Fatalf("Worker 实例标识 %q 不是 wrk_ + 32 位小写十六进制", parts[0])
		}
		if parts[1] != "worker-a" {
			t.Fatalf("Worker 登记名 %q（要求 worker-a，来自 BASALT_WORKER_NAME 缺省）", parts[1])
		}
	}
	if added != 1 {
		t.Fatalf("本 Worker 新增了 %d 行（要求 1）：%v", added, rows)
	}

	// ⑤ 日志里不得出现控制面的首次安装语义。
	logs, err := s.logs(ctx, us25WorkerService)
	if err != nil {
		t.Fatalf("读取 Worker 日志失败：%v", err)
	}
	for _, banned := range []string{
		"新实例：最小配置与实例名册已就绪",
		"管理台访问密码已写入",
		"实例加密：主密钥已生成",
	} {
		if strings.Contains(logs, banned) {
			t.Fatalf("Worker 日志出现控制面语义 %q：\n%s", banned, us18Tail(logs, 6000))
		}
	}

	// ⑥ Worker 的单文件配置没有控制面专属段：入口限流与 admin token 都属控制面。
	config, err := s.volumeRead(ctx, workerVol, "/basalt.yaml")
	if err != nil {
		t.Fatalf("读取 Worker 配置失败：%v", err)
	}
	for _, line := range strings.Split(config, "\n") {
		key := strings.TrimSpace(line)
		if key == "http:" || key == "callers:" {
			t.Fatalf("Worker 配置出现控制面专属段 %q：\n%s", key, config)
		}
	}
}
