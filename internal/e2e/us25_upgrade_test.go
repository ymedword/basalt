//go:build e2e

package e2e

// US-25 容器验收 · 两节点升级与整组回滚（FR-25-27 / US5）。
//
// 同 schema 边界：两枚测试镜像都由当前源码构建（US-25C 起 schema 均为 v13），新控制面
// 启动时**没有迁移可跑**——本用例
// 因此不宣称"制品版本门禁"；跨 schema 的制品拒绝由
// TestUS25_Migration_CrossSchemaRefusal（us25_migration_test.go）用旧版本二进制实测，
// 本用例只验同 schema 下的另一半：停全部旧进程、制品一致（两个容器同一镜像 ID）、
// 升级本身不改写业务面，以及"只恢复数据库不足以回滚、必须整组回滚"。
//
// 五个阶段：
//  0. 镜像 A 上建立两节点基线：一次真实执行钉住 bundle h0；冷备导出（同时停控制面）
//     把已发布资产种子给 Worker，再起控制面凑齐两个在线节点。
//  1. 停全部旧进程（各自退出码 0、在线 Worker 归零），备份 PostgreSQL（pg_dump -Fc）、
//     两个工作区（tar）、主密钥（字节副本 + 摘要）与旧制品（docker save）。
//  2. 换镜像 B 强制重建两个容器：schema 仍 13、build_version 变 B、两个容器镜像 ID
//     相同且与 A 不同；业务表、身份索引、生命周期文件、名册与 h0 资产树逐字不变；
//     新执行仍钉住 h0 并正常准入。
//  3. 停机后在工作区制造定义漂移并 `assets install`：发布指针推进到新 bundle h1。
//  4. 只恢复数据库：指针回到 h0，但工作区定义仍是漂移后的字节 → 新执行在准入被拒
//     （无 Run、无模型调用）。这是"只恢复数据库不足以回滚"的反证。
//  5. 整组回滚：恢复数据库 + 两个工作区 + 主密钥，切回镜像 A；业务表回到基线、漂移
//     产生的 bundle 目录消失、两个容器重新一致地跑镜像 A，新执行重新准入并钉住 h0。
//
// 逆改点（改坏这里就会红）：
//   - 让升级在新控制面启动时改写业务表 / 身份索引 / 状态文件：阶段 2 的逐字比较红；
//   - 让两个容器跑不同制品（例如只重建控制面）：阶段 2 / 5 的容器镜像 ID 断言红；
//   - 让准入不核对"工作区定义 vs 已发布 bundle"：阶段 4 的执行会真正跑起来，拒绝
//     断言与 Run/调用计数红；
//   - 让回滚只恢复数据库（少恢复工作区或密钥）：阶段 5 的业务表 / 状态文件 / 资产树
//     比较红，新执行仍被拒；
//   - 让 Worker 在种子或启动时改写名册：阶段 0 / 2 的名册逐字比较红。
//
// 本文件同时提供 M2 用例（us25_migration_test.go）共用的只读指纹助手：治理表与执行
// 表的整表指纹、探针身份 / 发布 bundle 读数、卷内文件与资产树的逐字比较。判据只有
// 一份定义，两个用例不各写一套。
//
// The same-schema upgrade: no migration runs, so this test does not claim an artifact
// version gate. It covers the other half — stop-all, artifact consistency, an upgrade
// that writes nothing business-facing, and the proof that restoring only the database is
// not a rollback. Shared read-only fingerprint helpers live here for both US5 tests.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 卷内相对路径（`/data` 之后的部分）。lifecycle 状态文件是生命周期的唯一权威，
// 名册是工作区身份；两者都要求跨升级逐字不变。
// Volume-relative paths: the lifecycle state file is the sole authority, the roster is
// the workspace identity; both must stay byte-identical across the upgrade.
const (
	us25VolumeRosterRel     = "/.basalt/instance.json"
	us25VolumeLifecycleRel  = "/.basalt/control/agents/" + us25ProbeAgent + ".json"
	us25VolumeDefinitionRel = "/.basalt/agents/" + us25ProbeAgent + "/AGENT.md"
)

// us25BundleRel 是内容寻址资产包在卷内的目录（`/data` 之后的部分）。
// us25BundleRel is the content-addressed bundle directory inside the workspace.
func us25BundleRel(bundleHash string) string {
	return "/.basalt/assets/bundles/" + us25ProbeAgent + "/" + bundleHash
}

// us25BundleDefinitionRel 是资产包里那份 Agent 定义的副本。bundle 布局是
// `agent/<工作区根相对路径>`（`assets.ConfigBundleRel`），而工作区根是 `/.basalt`，
// 定义的工作区根相对路径是 `agents/<agent>/AGENT.md`——不是卷相对的 `/.basalt/agents/...`。
// us25BundleDefinitionRel is the in-bundle copy of the definition: the bundle stores
// workspace-root-relative config paths under `agent/`, so the `.basalt` segment is not
// repeated inside the bundle.
func us25BundleDefinitionRel(bundleHash string) string {
	return us25BundleRel(bundleHash) + "/agent/agents/" + us25ProbeAgent + "/AGENT.md"
}

// us25TableNamePattern 是表名白名单：指纹查询按表名拼接 SQL，名字只能来自本文件的
// 常量列表，正则只是把"误传外部字符串"挡在查询之前。
// Table names are concatenated into SQL, so a whitelist pattern guards the seam.
var us25TableNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// us25DerivedStampColumns 是**每次启动都会被重算**的派生时间戳，不是业务事实，指纹
// 投影里必须剔除（否则"启动了一次"会被误判成"改写了业务表"）：
//   - `scheduled_tasks.updated_at` 由启动时的定义对账 upsert 改写；
//   - `agent_resource_bindings.projected_at` 由绑定投影全量重建（DELETE + INSERT）刷新。
//
// 只剔除这两列；行数、其余列（含 next_fire_at / last_run_at / resource_uid /
// content_hash）都在指纹内。
// Derived stamps recomputed on every boot: scheduled_tasks.updated_at (startup
// reconcile upsert) and agent_resource_bindings.projected_at (full projection rebuild).
var us25DerivedStampColumns = map[string]string{
	"scheduled_tasks":         "updated_at",
	"agent_resource_bindings": "projected_at",
}

// us25GovernanceTables 是治理与发布面：身份索引、绑定、凭证、流程定义与指针、知识库
// 顶层、学习审批、资源版本与指针、管理面审计、实例密钥与资源身份。种子导入、Worker
// 启动、制品切换都**不得**改写它们——它们记录"谁是谁、发布了什么"。
// Governance and publication rows; seeding, worker startup and artifact swaps must not
// rewrite them.
var us25GovernanceTables = []string{
	"agent_lifecycle",
	"agent_resource_bindings",
	"credential_versions",
	"credentials",
	"flow_definitions",
	"flow_pointers",
	"instance_secrets",
	"kb_collections",
	"kb_document_versions",
	"kb_documents",
	"management_operations",
	"resource_identities",
	"resource_pointers",
	"resource_versions",
	"skill_admission_candidates",
	"skill_admission_decisions",
	"skill_admission_evaluations",
	"skill_admission_publications",
}

// us25ExecutionTables 是执行与审计面：Run / 消息 / 调用 / 工具 / 租约 / 配额 / 投递 /
// 执行行与任务行。`worker_nodes` 是心跳投影（不是业务事实），`diagnostic_events` 与
// `observability_*` 是可采样诊断——两者都刻意不入指纹。
// Execution and audit rows; worker_nodes is a heartbeat projection and the observability
// tables are sampled diagnostics, so neither belongs in the fingerprint.
var us25ExecutionTables = []string{
	"caller_usage_periods",
	"embedding_calls",
	"execution_leases",
	"lease_events",
	"llm_calls",
	"messages",
	"notify_deliveries",
	"quota_reservations",
	"run_credential_refs",
	"run_notify_refs",
	"run_resource_manifests",
	"run_resource_refs",
	"runs",
	"sandbox_executions",
	"schedule_deliveries",
	"scheduled_tasks",
	"sessions",
	"task_executions",
	"tool_invocations",
}

// TestUS25_Upgrade_TwoNodeAbortAndRollback 判 FR-25-27 的同 schema 部分。
// TestUS25_Upgrade_TwoNodeAbortAndRollback covers the same-schema half of FR-25-27.
func TestUS25_Upgrade_TwoNodeAbortAndRollback(t *testing.T) {
	s := us25NewStack(t, "upgrade")
	s.addProbeEnvOverlay(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	// ── 阶段 0：镜像 A 上建立两节点基线 ─────────────────────────────────────
	controlVolume := s.prepareControlForProbe(t, us25ProbeRefusedURL)
	s.startControl(t)
	s.installProbeAssets(t)
	if err := s.up(ctx, us25ControlProfile, us25ControlService); err != nil {
		t.Fatalf("安装探针资产后重启控制面失败：%v", err)
	}
	s.enableProbeAgent(t)
	s.waitProbeTaskRow(t, 2*time.Minute)

	firstOwner := s.waitLiveWorkerIDs(t, 1, 2*time.Minute)[0]
	s.armProbe(t, us25ProbeArmDelay)
	first := s.waitProbeExecution(t, 1, 90*time.Second)
	if first[3] != firstOwner {
		t.Fatalf("基线执行行 owner 是 %q，要求控制面进程 %q", first[3], firstOwner)
	}
	if status := s.waitExecutionStatus(t, first[0], 3*time.Minute); status != "failed" {
		t.Fatalf("基线执行行终态是 %q，要求 failed（模型地址 127.0.0.1:1 必然拒连）", status)
	}
	firstRun := s.probeRunID(t, first[0])
	s.waitForQuery(t, 2*time.Minute, "基线 Run 收尾",
		"SELECT status FROM runs WHERE run_id = '"+firstRun+"'",
		func(v string) bool { return v != "" && v != "running" })
	bundleA := s.probeRunBundleHash(t, firstRun)
	if published := s.probePublishedBundleHash(t); published != bundleA {
		t.Fatalf("基线执行钉住的 bundle 是 %s，发布指针是 %s（准入与指针必须同一份）", bundleA, published)
	}

	// 冷备导出会停控制面；种子把已发布 bundle 原样带到 Worker 卷。
	_, seedPath := s.exportControlAssets(t)
	workerVolume := s.seedVolume(t, us25WorkerService, "worker-workspace", seedPath)
	s.prepareWorkerForProbe(t, workerVolume, us25ProbeRefusedURL)
	if err := s.up(ctx, us25WorkerProfile); err != nil {
		t.Fatalf("启动 Worker 失败：%v", err)
	}
	workerA := s.waitLiveWorkerIDs(t, 1, 3*time.Minute)[0]
	if err := s.up(ctx, us25ControlProfile); err != nil {
		t.Fatalf("重启控制面失败：%v", err)
	}
	liveA := s.waitLiveWorkerIDs(t, 2, 3*time.Minute)
	if !us25Contains(liveA, workerA) {
		t.Fatalf("Worker %s 不在在线集合 %v 里", workerA, liveA)
	}

	tagA := s.env["BASALT_IMAGE"]
	idA := s.imageRefID(t, ctx, tagA)
	for _, service := range []string{us25ControlService, us25WorkerService} {
		if got := s.containerImageID(t, ctx, service); got != idA {
			t.Fatalf("%s 容器跑的镜像 ID 是 %s，要求与镜像 A（%s）一致", service, got, idA)
		}
	}
	if got := s.liveArtifactFingerprint(t, ctx); got != us18DefaultVersion+"|13" {
		t.Fatalf("基线在线节点的制品读数是 %q，要求 %q", got, us18DefaultVersion+"|13")
	}

	baseline := s.stableBusinessFingerprint(t, 5*time.Second)
	baselineIdentity := s.probeIdentitySnapshot(t)
	baselineLifecycle := us25VolumeText(t, s, ctx, controlVolume, us25VolumeLifecycleRel)
	baselineRoster := us25VolumeText(t, s, ctx, controlVolume, us25VolumeRosterRel)
	baselineWorkerRoster := us25VolumeText(t, s, ctx, workerVolume, us25VolumeRosterRel)
	baselineBundle := us25VolumeTreeText(t, s, ctx, controlVolume, us25BundleRel(bundleA))
	if got := s.psql(t, "SELECT max(version)::text FROM schema_version"); got != "13" {
		t.Fatalf("基线 schema 版本是 %q，本用例的判据按 v13 组织", got)
	}
	baselineKeyDigest := s.masterKeyDigest(t, ctx)
	if got := s.instanceID(t, baselineRoster); got != s.instanceID(t, baselineWorkerRoster) {
		t.Fatalf("Worker 名册的 instance_id 与源工作区不一致（Worker 不得改写名册）")
	}

	// ── 阶段 1：停全部旧进程 → 备份四件 ─────────────────────────────────────
	s.stopAll(t, ctx)
	dumpPath := s.backupPostgres(t, ctx)
	controlBackup := s.backupWorkspace(t, ctx, controlVolume, "control-workspace")
	workerBackup := s.backupWorkspace(t, ctx, workerVolume, "worker-workspace")
	keyBackup, keyBackupDigest := s.backupMasterKey(t, ctx)
	if keyBackupDigest != baselineKeyDigest {
		t.Fatalf("主密钥备份与基线摘要不一致（备份不是同一份密钥）")
	}
	s.backupArtifact(t, ctx, tagA, filepath.Join(s.dir, "image-a.tar"))

	// ── 阶段 2：换镜像 B，同 schema 直接启动 ────────────────────────────────
	tagB := us18ImageFor(t, us18UpgradeVersion)
	idB := s.imageRefID(t, ctx, tagB)
	if idB == idA {
		t.Fatalf("镜像 A 与镜像 B 的 ID 相同（%s）——本用例要求可区分的制品", idA)
	}
	s.env["BASALT_IMAGE"] = tagB
	if err := s.up(ctx, us25ControlProfile, "--force-recreate", us25ControlService); err != nil {
		t.Fatalf("用镜像 B 重建控制面失败：%v", err)
	}
	if err := s.up(ctx, us25WorkerProfile, "--force-recreate", us25WorkerService); err != nil {
		t.Fatalf("用镜像 B 重建 Worker 失败：%v", err)
	}
	liveB := s.waitLiveWorkerIDs(t, 2, 3*time.Minute)
	for _, service := range []string{us25ControlService, us25WorkerService} {
		if got := s.containerImageID(t, ctx, service); got != idB {
			t.Fatalf("%s 容器跑的镜像 ID 是 %s，要求与镜像 B（%s）一致（制品必须整组切换）", service, got, idB)
		}
	}
	if got := s.liveArtifactFingerprint(t, ctx); got != us18UpgradeVersion+"|13" {
		t.Fatalf("升级后在线节点的制品读数是 %q，要求 %q", got, us18UpgradeVersion+"|13")
	}
	if got := s.psql(t, "SELECT max(version)::text FROM schema_version"); got != "13" {
		t.Fatalf("同 schema 升级后版本是 %q，要求仍为 13（本用例不宣称制品版本门禁）", got)
	}

	// 升级本身不改写业务面：指纹、身份、状态文件、名册与资产树逐字不变。
	if after := s.stableBusinessFingerprint(t, 5*time.Second); !maps.Equal(after, baseline) {
		t.Fatalf("切换制品改写了业务表：%s", us25FingerprintDiff(baseline, after))
	}
	if got := s.probeIdentitySnapshot(t); got != baselineIdentity {
		t.Fatalf("升级后生命周期索引读数 = %q，要求与基线一致（%q）", got, baselineIdentity)
	}
	if got := us25VolumeText(t, s, ctx, controlVolume, us25VolumeLifecycleRel); got != baselineLifecycle {
		t.Fatalf("升级改写了生命周期状态文件（Agent UID / 代数必须原样保留）")
	}
	if got := us25VolumeText(t, s, ctx, controlVolume, us25VolumeRosterRel); got != baselineRoster {
		t.Fatalf("升级改写了控制面名册")
	}
	if got := us25VolumeText(t, s, ctx, workerVolume, us25VolumeRosterRel); got != baselineWorkerRoster {
		t.Fatalf("升级改写了 Worker 名册")
	}
	if got := us25VolumeTreeText(t, s, ctx, controlVolume, us25BundleRel(bundleA)); got != baselineBundle {
		t.Fatalf("升级改写了控制面工作区的旧 bundle 树（Worker / 控制面不得安装或重建资产）")
	}
	if got := s.probePublishedBundleHash(t); got != bundleA {
		t.Fatalf("升级后发布指针是 %s，要求仍是 %s", got, bundleA)
	}

	// 升级后仍能正常准入，并钉住旧 bundle。
	s.armProbe(t, us25ProbeArmDelay)
	second := s.waitProbeExecution(t, 2, 3*time.Minute)
	if !us25Contains(liveB, second[3]) {
		t.Fatalf("升级后的执行行 owner 是 %q，不在在线集合 %v 里", second[3], liveB)
	}
	if status := s.waitExecutionStatus(t, second[0], 3*time.Minute); status != "failed" {
		t.Fatalf("升级后执行行终态是 %q，要求 failed（拒绝的回环地址）", status)
	}
	if got := s.probeRunBundleHash(t, s.probeRunID(t, second[0])); got != bundleA {
		t.Fatalf("升级后的执行钉住 bundle %s，要求旧 bundle %s", got, bundleA)
	}

	// ── 阶段 3：停机后制造定义漂移，发布推进到 h1 ───────────────────────────
	s.stopAll(t, ctx)
	if out, err := s.execInVolume(ctx, controlVolume,
		"set -eu\nprintf '\\n<!-- us25-upgrade-drift -->\\n' >> /data"+us25VolumeDefinitionRel+"\n"); err != nil {
		t.Fatalf("在控制面工作区制造定义漂移失败：%v（%s）", err, us18Tail(out, 2000))
	}
	driftedDefinition := us25VolumeDigest(t, s, ctx, controlVolume, us25VolumeDefinitionRel)
	if got := us25VolumeDigest(t, s, ctx, controlVolume, us25BundleDefinitionRel(bundleA)); got == driftedDefinition {
		t.Fatalf("漂移没有生效：工作区定义与旧 bundle 里的副本仍然相同")
	}
	if out, err := s.runOneOff(ctx, us25ControlProfile, nil, us25ControlService,
		"assets", "install", "--agent", us25ProbeAgent); err != nil {
		t.Fatalf("漂移后重装资产失败：%v（%s）", err, us18Tail(out, 3000))
	}
	bundleDrift := s.probePublishedBundleHash(t)
	if bundleDrift == bundleA {
		t.Fatalf("漂移后重新安装没有推进发布指针（仍是 %s）", bundleA)
	}
	if !s.volumeHas(ctx, controlVolume, us25BundleRel(bundleDrift)) {
		t.Fatalf("新发布指针 %s 对应的 bundle 目录不存在", bundleDrift)
	}

	// ── 阶段 4：只恢复数据库——不足以回滚 ────────────────────────────────────
	s.restorePostgres(t, ctx, dumpPath)
	if err := s.up(ctx, us25ControlProfile, us25ControlService); err != nil {
		t.Fatalf("只恢复数据库后启动控制面失败：%v", err)
	}
	controlOnly := s.waitLiveWorkerIDs(t, 1, 3*time.Minute)[0]
	if got := s.probePublishedBundleHash(t); got != bundleA {
		t.Fatalf("只恢复数据库后发布指针是 %s，要求回到 %s（数据库侧回滚生效）", got, bundleA)
	}
	if got := us25VolumeDigest(t, s, ctx, controlVolume, us25VolumeDefinitionRel); got != driftedDefinition {
		t.Fatalf("只恢复数据库不应改动工作区定义（读数与漂移后不一致）")
	}
	callsBefore := s.probeRunCallCounts(t)
	s.armProbe(t, us25ProbeArmDelay)
	third := s.waitProbeExecution(t, 2, 3*time.Minute)
	assertAssetRefusal(t, s, third, controlOnly)
	s.waitForLog(t, us25ControlService, "运行资源不可用，拒绝新运行", 2*time.Minute)
	if got := s.probeRunCallCounts(t); got != callsBefore {
		t.Fatalf("拒绝路径改变了 Run/调用计数（%s → %s），要求逐项相同", callsBefore, got)
	}

	// ── 阶段 5：整组回滚（数据库 + 两个工作区 + 主密钥）──────────────────────
	s.stopAll(t, ctx)
	s.restorePostgres(t, ctx, dumpPath)
	s.restoreWorkspace(t, ctx, controlVolume, controlBackup)
	s.restoreWorkspace(t, ctx, workerVolume, workerBackup)
	s.restoreMasterKey(t, ctx, keyBackup, baselineKeyDigest)
	s.env["BASALT_IMAGE"] = tagA
	if err := s.up(ctx, us25ControlProfile, "--force-recreate", us25ControlService); err != nil {
		t.Fatalf("回滚控制面到镜像 A 失败：%v", err)
	}
	if err := s.up(ctx, us25WorkerProfile, "--force-recreate", us25WorkerService); err != nil {
		t.Fatalf("回滚 Worker 到镜像 A 失败：%v", err)
	}
	liveRollback := s.waitLiveWorkerIDs(t, 2, 3*time.Minute)
	for _, service := range []string{us25ControlService, us25WorkerService} {
		if got := s.containerImageID(t, ctx, service); got != idA {
			t.Fatalf("回滚后 %s 容器跑的镜像 ID 是 %s，要求镜像 A（%s）", service, got, idA)
		}
	}
	if got := s.liveArtifactFingerprint(t, ctx); got != us18DefaultVersion+"|13" {
		t.Fatalf("回滚后在线节点的制品读数是 %q，要求 %q", got, us18DefaultVersion+"|13")
	}
	if got := s.psql(t, "SELECT max(version)::text FROM schema_version"); got != "13" {
		t.Fatalf("回滚后 schema 版本是 %q，要求仍是 13", got)
	}

	// 四件备份全部生效：数据库、两个工作区、主密钥都回到基线。
	if after := s.stableBusinessFingerprint(t, 5*time.Second); !maps.Equal(after, baseline) {
		t.Fatalf("整组回滚后业务表没有回到基线：%s", us25FingerprintDiff(baseline, after))
	}
	if got := s.probeIdentitySnapshot(t); got != baselineIdentity {
		t.Fatalf("回滚后生命周期索引读数 = %q，要求基线 %q", got, baselineIdentity)
	}
	if got := us25VolumeText(t, s, ctx, controlVolume, us25VolumeLifecycleRel); got != baselineLifecycle {
		t.Fatalf("回滚后生命周期状态文件与基线不一致")
	}
	if got := us25VolumeText(t, s, ctx, controlVolume, us25VolumeRosterRel); got != baselineRoster {
		t.Fatalf("回滚后控制面名册与基线不一致")
	}
	if got := us25VolumeText(t, s, ctx, workerVolume, us25VolumeRosterRel); got != baselineWorkerRoster {
		t.Fatalf("回滚后 Worker 名册与基线不一致")
	}
	if got := us25VolumeTreeText(t, s, ctx, controlVolume, us25BundleRel(bundleA)); got != baselineBundle {
		t.Fatalf("回滚后控制面工作区的旧 bundle 树与基线不一致")
	}
	if s.volumeHas(ctx, controlVolume, us25BundleRel(bundleDrift)) {
		t.Fatalf("回滚后控制面工作区仍留着漂移产生的 bundle %s（工作区没有整组恢复）", bundleDrift)
	}
	if got := s.masterKeyDigest(t, ctx); got != baselineKeyDigest {
		t.Fatalf("回滚后宿主主密钥与基线摘要不一致（密钥副本没有恢复到位）")
	}
	if got := s.probePublishedBundleHash(t); got != bundleA {
		t.Fatalf("回滚后发布指针是 %s，要求 %s", got, bundleA)
	}

	// 整组回滚之后，新执行重新通过准入并钉住旧 bundle。
	s.armProbe(t, us25ProbeArmDelay)
	fourth := s.waitProbeExecution(t, 2, 3*time.Minute)
	if !us25Contains(liveRollback, fourth[3]) {
		t.Fatalf("回滚后的执行行 owner 是 %q，不在在线集合 %v 里", fourth[3], liveRollback)
	}
	if status := s.waitExecutionStatus(t, fourth[0], 3*time.Minute); status != "failed" {
		t.Fatalf("回滚后执行行终态是 %q，要求 failed（拒绝的回环地址）", status)
	}
	if got := s.probeRunBundleHash(t, s.probeRunID(t, fourth[0])); got != bundleA {
		t.Fatalf("回滚后的执行钉住 bundle %s，要求旧 bundle %s", got, bundleA)
	}
	s.assertSingleExecution(t, fourth[1])
}

// ── 共享只读指纹 ─────────────────────────────────────────────────────────────

// businessFingerprint 一次查询取回全部治理表与执行表的 `count:md5` 指纹。
// 每次启动都会被重算的派生时间戳（见 us25DerivedStampColumns）在投影里剔除；其余列
// 全部参与指纹。
// One query returns every governance and execution table's count:md5 fingerprint, with
// the derived stamps listed in us25DerivedStampColumns dropped from the projection.
func (s *us25Stack) businessFingerprint(t *testing.T) map[string]string {
	t.Helper()
	tables := append(append([]string{}, us25GovernanceTables...), us25ExecutionTables...)
	parts := make([]string, 0, len(tables))
	for _, table := range tables {
		if !us25TableNamePattern.MatchString(table) {
			t.Fatalf("表名 %q 不在指纹白名单形态内", table)
		}
		expr := "to_jsonb(t)"
		if column, derived := us25DerivedStampColumns[table]; derived {
			expr = "(to_jsonb(t) - '" + column + "')"
		}
		parts = append(parts, "SELECT '"+table+"' || '=' || count(*)::text || ':' || "+
			"COALESCE(md5(string_agg("+expr+"::text, chr(10) ORDER BY "+expr+"::text)), '-') "+
			"FROM "+table+" t")
	}
	out := s.psql(t, strings.Join(parts, "\nUNION ALL\n"))
	fingerprints := make(map[string]string, len(tables))
	for _, line := range us18NonEmptyLines(out) {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("指纹行格式异常：%q", line)
		}
		fingerprints[name] = value
	}
	if len(fingerprints) != len(tables) {
		t.Fatalf("指纹行数 %d（要求 %d）", len(fingerprints), len(tables))
	}
	return fingerprints
}

// stableBusinessFingerprint 轮询到连续两次读数相同才返回：收尾写入是异步的，
// 拿一次瞬时读数当基线会把"还在写"误判成"被改写"。
// stableBusinessFingerprint polls until two consecutive reads agree.
func (s *us25Stack) stableBusinessFingerprint(t *testing.T, settle time.Duration) map[string]string {
	t.Helper()
	previous := s.businessFingerprint(t)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(settle)
		current := s.businessFingerprint(t)
		if maps.Equal(current, previous) {
			return current
		}
		previous = current
	}
	t.Fatalf("业务表指纹在 3 分钟内没有稳定下来（收尾写入仍在持续）")
	return nil
}

// us25FingerprintCount 解析指纹里的行数。
// us25FingerprintCount parses the row count out of one fingerprint.
func us25FingerprintCount(t *testing.T, fingerprint string) int {
	t.Helper()
	countText, _, ok := strings.Cut(fingerprint, ":")
	if !ok {
		t.Fatalf("指纹格式异常：%q", fingerprint)
	}
	count, err := strconv.Atoi(countText)
	if err != nil {
		t.Fatalf("指纹计数不是整数：%q", fingerprint)
	}
	return count
}

// us25FingerprintDiff 给出两份指纹的差异表（只列变了的表）。
// us25FingerprintDiff names every table whose fingerprint changed.
func us25FingerprintDiff(before, after map[string]string) string {
	tables := append(append([]string{}, us25GovernanceTables...), us25ExecutionTables...)
	var diffs []string
	for _, table := range tables {
		if before[table] != after[table] {
			diffs = append(diffs, fmt.Sprintf("%s: %s → %s", table, before[table], after[table]))
		}
	}
	if len(diffs) == 0 {
		return "无差异"
	}
	return strings.Join(diffs, "; ")
}

// us25RequireGovernanceUnchanged 只判治理面的逐字不变：执行面在"多跑一次执行"的
// 阶段本应变化，由调用方各自判增量。
// us25RequireGovernanceUnchanged judges byte equality of the governance tables only.
func us25RequireGovernanceUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	for _, table := range us25GovernanceTables {
		if before[table] != after[table] {
			t.Fatalf("治理表 %s 在只应产生执行行的阶段被改写：%s → %s",
				table, before[table], after[table])
		}
	}
}

// probeIdentitySnapshot 读探针 Agent 的身份索引：名称 / UID / 来源 / 状态 / 代数 /
// 状态文件哈希 / 有效性。升级与回滚都必须让它逐字不变。
// probeIdentitySnapshot reads the probe's lifecycle index row.
func (s *us25Stack) probeIdentitySnapshot(t *testing.T) string {
	t.Helper()
	return s.psql(t, "SELECT agent_name || '|' || COALESCE(agent_uid,'-') || '|' || "+
		"COALESCE(uid_source,'-') || '|' || COALESCE(state,'-') || '|' || generation::text || '|' || "+
		"COALESCE(state_file_hash,'-') || '|' || valid::text FROM agent_lifecycle "+
		"WHERE agent_name = '"+us25ProbeAgent+"'")
}

// probeRunBundleHash 读一次 Run 在准入时固定的资产包哈希（v3 资源清单）。
// snapshot_json 是 text：先 `::json` 再取键。
// probeRunBundleHash reads the asset bundle hash pinned by one run's manifest.
func (s *us25Stack) probeRunBundleHash(t *testing.T, runID string) string {
	t.Helper()
	out := s.psql(t, "SELECT COALESCE(snapshot_json::json->>'asset_bundle_hash','') "+
		"FROM run_resource_manifests WHERE run_id = '"+runID+"'")
	if out == "" {
		t.Fatalf("Run %s 的资源清单缺少 asset_bundle_hash", runID)
	}
	return out
}

// probePublishedBundleHash 读探针 Agent 当前共享发布记录里的 agent_bundle 哈希。
// resource_pointers 的 resource_id 对 agent_bundle 是规范 AgentUID（`ag_...`），
// 不是 Agent 名——所以经 agent_lifecycle 的 UID 反查。
// probePublishedBundleHash reads the published agent_bundle hash through the uid.
func (s *us25Stack) probePublishedBundleHash(t *testing.T) string {
	t.Helper()
	out := s.psql(t, "SELECT COALESCE((SELECT v.content_hash FROM resource_pointers p "+
		"JOIN resource_versions v ON v.id = p.published_version_id "+
		"WHERE p.resource_kind = 'agent_bundle' AND p.resource_id = "+
		"(SELECT agent_uid FROM agent_lifecycle WHERE agent_name = '"+us25ProbeAgent+"')), '')")
	if out == "" {
		t.Fatalf("探针 Agent 没有已发布的 agent_bundle 发布指针")
	}
	return out
}

// ── 在线制品读数 ─────────────────────────────────────────────────────────────

// workerListJSON 在 Worker 容器内跑只读列表并把输出裁到第一个 `[`：CLI 会先在同一
// 条流上写一行启动日志。等价写法与 TestUS25_Workers_BothRegistered 一致。
// workerListJSON runs the read-only listing inside the worker container and trims the
// startup log line the CLI writes to the same stream.
func (s *us25Stack) workerListJSON(t *testing.T, ctx context.Context) string {
	t.Helper()
	out, err := s.exec(ctx, us25WorkerService, "sh", "-c",
		`export BASALT_DATABASE_DSN="$(cat /run/secrets/database_dsn)"; exec /opt/basalt/basalt workers list --json`)
	if err != nil {
		t.Fatalf("Worker 容器内 workers list --json 失败：%v（%s）", err, us18Tail(out, 2000))
	}
	start := strings.Index(out, "[")
	if start < 0 {
		t.Fatalf("workers list --json 输出不是数组：%s", us18Tail(out, 3000))
	}
	return out[start:]
}

// liveArtifactFingerprint 把所有在线节点的制品读数拍成一个 `build_version|schema_version`：
// 每个进程注册时写下自己二进制里的版本与 schema，逐行不同就说明制品没有整组切换。
// liveArtifactFingerprint collapses every live node's artifact reading into one string.
func (s *us25Stack) liveArtifactFingerprint(t *testing.T, ctx context.Context) string {
	t.Helper()
	var nodes []struct {
		Instance      string `json:"instance"`
		BuildVersion  string `json:"build_version"`
		SchemaVersion int    `json:"schema_version"`
		StoppedAt     string `json:"stopped_at"`
	}
	if err := json.Unmarshal([]byte(s.workerListJSON(t, ctx)), &nodes); err != nil {
		t.Fatalf("解析 workers list --json 失败：%v", err)
	}
	fingerprint := ""
	live := 0
	for _, node := range nodes {
		if node.StoppedAt != "" {
			continue
		}
		live++
		current := fmt.Sprintf("%s|%d", node.BuildVersion, node.SchemaVersion)
		if fingerprint == "" {
			fingerprint = current
			continue
		}
		if current != fingerprint {
			t.Fatalf("在线节点的制品读数不一致：%s 与 %s（制品必须整组一致）", fingerprint, current)
		}
	}
	if live == 0 {
		t.Fatalf("workers list 里没有在线节点，制品读数无从判定")
	}
	return fingerprint
}

// containerImageID 读某个服务容器实际使用的镜像 ID。
// containerImageID reads the image id a service container actually runs.
func (s *us25Stack) containerImageID(t *testing.T, ctx context.Context, service string) string {
	t.Helper()
	id, err := s.serviceContainerID(ctx, service)
	if err != nil {
		t.Fatalf("取 %s 容器 ID 失败：%v", service, err)
	}
	out, err := us18DockerOutput(ctx, "inspect", "--format", "{{.Image}}", id)
	if err != nil {
		t.Fatalf("读取 %s 容器镜像 ID 失败：%v", service, err)
	}
	return strings.TrimSpace(out)
}

// imageRefID 读某个镜像 tag 指向的镜像 ID。
// imageRefID reads the image id behind one tag.
func (s *us25Stack) imageRefID(t *testing.T, ctx context.Context, ref string) string {
	t.Helper()
	out, err := us18DockerOutput(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		t.Fatalf("读取镜像 %s 的 ID 失败：%v", ref, err)
	}
	return strings.TrimSpace(out)
}

// ── 停全部旧进程与四件备份 ───────────────────────────────────────────────────

// stopAll 按 Worker → 控制面的顺序优雅停机：两边退出码都必须是 0，且在线 Worker
// 归零（这是"停全部旧进程"的可判读数，不只是容器状态）。
// stopAll stops the worker then the control plane; both must exit 0 and no live worker
// row may remain.
func (s *us25Stack) stopAll(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, service := range []string{us25WorkerService, us25ControlService} {
		if err := s.stopService(ctx, service); err != nil {
			t.Fatalf("停 %s 失败：%v", service, err)
		}
		if code := s.awaitExit(t, service, us25ExitTimeout); code != 0 {
			logs, _ := s.logs(ctx, service)
			t.Fatalf("%s 停机退出码 %d（要求 0，即优雅停机），日志尾部：\n%s",
				service, code, us18Tail(logs, 4000))
		}
	}
	s.waitLiveWorkerIDs(t, 0, us25ExitTimeout)
}

// backupPostgres 在数据库容器里 `pg_dump -Fc` 到 /tmp，再 `docker cp` 到宿主。
// 备份与恢复都只用容器内环境变量拼命令行，不把 DSN / 口令写进任何 argv。
// backupPostgres dumps the database inside its container, then copies the archive out;
// the DSN and password never reach argv.
func (s *us25Stack) backupPostgres(t *testing.T, ctx context.Context) string {
	t.Helper()
	out, err := s.exec(ctx, us25PostgresService, "sh", "-c",
		`set -eu
pg_dump -Fc -U "$POSTGRES_USER" -d "$POSTGRES_DB" -f /tmp/us25-upgrade.dump
wc -c < /tmp/us25-upgrade.dump`)
	if err != nil {
		t.Fatalf("备份 PostgreSQL 失败：%v（%s）", err, us18Tail(out, 2000))
	}
	size, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || size <= 0 {
		t.Fatalf("数据库备份大小读数是 %q，要求 > 0", strings.TrimSpace(out))
	}
	container, err := s.serviceContainerID(ctx, us25PostgresService)
	if err != nil {
		t.Fatalf("取 postgres 容器 ID 失败：%v", err)
	}
	dest := filepath.Join(s.dir, "postgres-upgrade.dump")
	if _, err := us18DockerOutput(ctx, "cp", container+":/tmp/us25-upgrade.dump", dest); err != nil {
		t.Fatalf("导出数据库备份失败：%v", err)
	}
	if info, err := os.Stat(dest); err != nil || info.Size() != int64(size) {
		t.Fatalf("数据库备份落盘后大小与容器内读数不一致（容器内 %d 字节）", size)
	}
	return dest
}

// restorePostgres 原地重建数据库：DROP → CREATE → 先建两个扩展 → pg_restore。
// 扩展必须**先于**恢复建好：`kb_chunk_embeddings` 用了 `vector(1536)`，缺扩展时
// pg_restore 会在第一条建表语句就失败。
// restorePostgres recreates the database in place; both extensions must exist before
// pg_restore because kb_chunk_embeddings declares a vector column.
func (s *us25Stack) restorePostgres(t *testing.T, ctx context.Context, dumpPath string) {
	t.Helper()
	container, err := s.serviceContainerID(ctx, us25PostgresService)
	if err != nil {
		t.Fatalf("取 postgres 容器 ID 失败：%v", err)
	}
	if _, err := us18DockerOutput(ctx, "cp", dumpPath, container+":/tmp/us25-restore.dump"); err != nil {
		t.Fatalf("送入数据库备份失败：%v", err)
	}
	script := `set -eu
psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS \"$POSTGRES_DB\" WITH (FORCE)"
psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 \
  -c "CREATE DATABASE \"$POSTGRES_DB\" OWNER \"$POSTGRES_USER\""
psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 \
  -c "CREATE EXTENSION IF NOT EXISTS vector; CREATE EXTENSION IF NOT EXISTS pg_trgm"
pg_restore --no-owner --no-privileges --exit-on-error \
  -U "$POSTGRES_USER" -d "$POSTGRES_DB" /tmp/us25-restore.dump
`
	if out, err := s.exec(ctx, us25PostgresService, "sh", "-c", script); err != nil {
		t.Fatalf("恢复 PostgreSQL 失败：%v（%s）", err, us18Tail(out, 3000))
	}
}

// backupWorkspace 用一个 root 一次性容器把工作区卷打包到宿主备份目录（保留数字属主）。
// backupWorkspace tars one workspace volume to the host backup directory.
func (s *us25Stack) backupWorkspace(t *testing.T, ctx context.Context, volume, name string) string {
	t.Helper()
	archive := filepath.Join(s.dir, name+".tar")
	script := "set -eu\ntar --numeric-owner -C /data -cf /backup/" + name + ".tar .\n" +
		"wc -c < /backup/" + name + ".tar"
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", volume+":/data:ro",
		"-v", s.dir+":/backup",
		"--entrypoint", "sh", s.image, "-c", script)
	if err != nil {
		t.Fatalf("备份工作区 %s 失败：%v（%s）", name, err, us18Tail(out, 2000))
	}
	if size, err := strconv.Atoi(strings.TrimSpace(out)); err != nil || size <= 0 {
		t.Fatalf("工作区备份 %s 的大小读数是 %q，要求 > 0", name, strings.TrimSpace(out))
	}
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("工作区备份 %s 没有落盘", name)
	}
	return archive
}

// restoreWorkspace 清空卷内容后解包备份，并把属主统一回 10001:10001（镜像用户）。
// restoreWorkspace empties the volume, unpacks the archive and restores the image user.
func (s *us25Stack) restoreWorkspace(t *testing.T, ctx context.Context, volume, archive string) {
	t.Helper()
	dir, name := filepath.Split(filepath.Clean(archive))
	script := `set -eu
find /data -mindepth 1 -maxdepth 1 -exec rm -rf {} +
tar --numeric-owner -C /data -xpf /backup/` + name + `
chown -R 10001:10001 /data
`
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", volume+":/data",
		"-v", dir+":/backup:ro",
		"--entrypoint", "sh", s.image, "-c", script)
	if err != nil {
		t.Fatalf("恢复工作区 %s 失败：%v（%s）", volume, err, us18Tail(out, 2000))
	}
}

// masterKeyDigest 在 root 一次性容器里算宿主主密钥的 SHA-256：密钥文件是
// `0400` / `10001:10001`（这正是 FR-25-16 与 `secrets check-key` 要求的姿态），测试
// 进程读不到，只能借容器读。摘要只在测试内比较，从不打印——密钥材料不进日志、失败
// 信息与模型上下文。
// masterKeyDigest hashes the host master key inside a root one-off container: the file
// is 0400/10001:10001, so the test process cannot read it. The digest is compared
// in-test and never printed.
func (s *us25Stack) masterKeyDigest(t *testing.T, ctx context.Context) string {
	t.Helper()
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", s.secretsDir+":/secrets:ro",
		"--entrypoint", "sh", s.image, "-c",
		"sha256sum /secrets/master.key | cut -d' ' -f1")
	if err != nil {
		t.Fatalf("计算主密钥摘要失败：%v", err)
	}
	digest := strings.TrimSpace(out)
	if digest == "" {
		t.Fatalf("主密钥摘要为空（文件缺失或不可读）")
	}
	return digest
}

// backupMasterKey 在 root 容器里把宿主主密钥复制成同卷备份（保留 0400 /
// 10001:10001），返回备份路径与源文件摘要。宿主进程不碰密钥字节。
// backupMasterKey copies the key inside a root container and returns the backup path
// and the source digest; the host process never touches the key bytes.
func (s *us25Stack) backupMasterKey(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	script := `set -eu
cp -p /secrets/master.key /secrets/master.key.us25-backup
sha256sum /secrets/master.key | cut -d' ' -f1`
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", s.secretsDir+":/secrets",
		"--entrypoint", "sh", s.image, "-c", script)
	if err != nil {
		t.Fatalf("备份主密钥失败：%v", err)
	}
	digest := strings.TrimSpace(out)
	if digest == "" {
		t.Fatalf("主密钥备份没有返回摘要")
	}
	return filepath.Join(s.secretsDir, "master.key.us25-backup"), digest
}

// restoreMasterKey 在 root 容器里先校验备份摘要，再把它写回 bind mount 源并重设
// 10001:10001 / 0400——Worker 的 `secrets check-key` 对这两项都是失败关闭的。
// restoreMasterKey verifies the backup digest inside a root container, copies it back
// and resets 10001:10001 / 0400.
func (s *us25Stack) restoreMasterKey(t *testing.T, ctx context.Context, backup, want string) {
	t.Helper()
	script := `set -eu
got="$(sha256sum /secrets/master.key.us25-backup | cut -d' ' -f1)"
[ "$got" = "` + want + `" ]
cp -p /secrets/master.key.us25-backup /secrets/master.key
chown 10001:10001 /secrets/master.key
chmod 0400 /secrets/master.key
[ "$(stat -c '%u:%g:%a' /secrets/master.key)" = "10001:10001:400" ]`
	out, err := us18DockerOutput(ctx, "run", "--rm", "--user", "0",
		"-v", s.secretsDir+":/secrets",
		"--entrypoint", "sh", s.image, "-c", script)
	if err != nil {
		t.Fatalf("恢复主密钥失败（摘要校验或属性复核未通过）：%v（%s）", err, us18Tail(out, 2000))
	}
}

// backupArtifact 用 `docker save` 把旧制品留在宿主：回滚演练要求旧二进制可复原，
// 不能只靠"宿主上大概还在"的镜像 tag。
// backupArtifact saves the old artifact to the host so rollback never depends on a tag
// that might have moved.
func (s *us25Stack) backupArtifact(t *testing.T, ctx context.Context, ref, dest string) {
	t.Helper()
	if _, err := us18DockerOutput(ctx, "save", "-o", dest, ref); err != nil {
		t.Fatalf("备份旧制品 %s 失败：%v", ref, err)
	}
	if info, err := os.Stat(dest); err != nil || info.Size() == 0 {
		t.Fatalf("旧制品备份没有落盘：%s", dest)
	}
}

// ── 卷内文件读数 ─────────────────────────────────────────────────────────────

// us25VolumeText 读卷内一个文件的全文；读不到直接判红（这些文件都是判据素材）。
// us25VolumeText reads one workspace file or fails.
func us25VolumeText(t *testing.T, s *us25Stack, ctx context.Context, volume, rel string) string {
	t.Helper()
	body, err := s.volumeRead(ctx, volume, rel)
	if err != nil {
		t.Fatalf("读取卷 %s 的 %s 失败：%v", volume, rel, err)
	}
	return body
}

// us25VolumeTreeText 取一棵子树的 `sha256 + 相对路径` 指纹；目录不存在即判红——
// 逐字比较需要对象存在，`__absent__` 只适合作否定断言。
// us25VolumeTreeText fingerprints a subtree; an absent directory fails the comparison.
func us25VolumeTreeText(t *testing.T, s *us25Stack, ctx context.Context, volume, rel string) string {
	t.Helper()
	body, err := s.volumeTree(ctx, volume, rel)
	if err != nil {
		t.Fatalf("读取卷 %s 的 %s 树失败：%v", volume, rel, err)
	}
	if body == "__absent__" {
		t.Fatalf("卷 %s 里没有 %s（资产树缺失，逐字比较没有对象）", volume, rel)
	}
	return body
}

// us25VolumeDigest 是卷内一个文件内容的 SHA-256。
// us25VolumeDigest hashes one workspace file's content.
func us25VolumeDigest(t *testing.T, s *us25Stack, ctx context.Context, volume, rel string) string {
	t.Helper()
	return us25DigestContent(us25VolumeText(t, s, ctx, volume, rel))
}

// us25DigestContent 是内容摘要（十六进制 SHA-256）。
// us25DigestContent is the hex SHA-256 of a byte string.
func us25DigestContent(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// us25Contains 判字符串集合里是否有完全相等的一项。
// us25Contains reports whether one whole value equals want.
func us25Contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
