//go:build e2e

package e2e

// US-25 容器验收 · 定时执行探针（FR-25-19 / FR-25-20 共用的素材层）。
//
// 探针由三块组成，全部落在**真实容器**里：
//   - 控制面配置：镜像模板 + 一段 `providers:`，base_url 由用例给（回环拒连 / 挂起
//     监听器 / 已停的监听器），凭据只以 ${BASALT_US25_PROBE_KEY} 占位、值由 overlay
//     注入两个容器的进程环境；
//   - 一只最小 Agent（AGENT.md 预置进工作区，首次启动由启动基线写成 disabled）：无
//     工具、无 MCP、一个 6 段 cron 任务；
//   - 一组只读 SQL 读数：执行行、租约、租约事件、worker_nodes 与生命周期状态文件。
//
// 为什么用 SQL 直接「上弦」：到期判定读的是 `scheduled_tasks.next_fire_at`。把 cron
// 排到真实时刻只能干等（最坏 24 小时），而把 next_fire_at 推到 ~1.5s 之后走的仍是
// **同一条**领取路径（到期扫描 → ClaimDueTrigger → 执行行 + 租约），不绕过任何判据。
//
// 两个刻意的分工：
//   - 「谁在执行」用 `worker_nodes.instance_id`（每进程随机铸造，`serve` 与 `gateway`
//     各有一份），**不是**工作区名册的 instance_id——后者是工作区身份，跨进程不变；
//   - 探针的模型调用一律指向必然失败的地址（回环 1 端口 / 挂起监听器），用例判的是
//     领取、执行权与收尾，不判模型输出，也不注入任何真实模型凭据。
//
// The probe is a config block, one minimal AGENT.md and read-only SQL readers. Arming
// rewrites scheduled_tasks.next_fire_at so the very same due-scan claim path runs within
// seconds instead of waiting for a wall-clock cron slot. "Who executed" is read from
// worker_nodes, which carries the per-process instance id, not the workspace roster.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 探针常量：Agent 目录名 / 任务 id / provider 名与模型三者各自独立，装载期要求
// provider.name 有已发布版本、provider.model 非空。
// Probe constants: the agent directory, the task id and the provider triple.
const (
	us25ProbeAgent    = "us25-probe"
	us25ProbeTaskID   = "us25-probe"
	us25ProbeProvider = "us25-probe"
	us25ProbeModel    = "us25-probe"

	// us25ProbeKeyEnv 是 provider 凭据的占位符名；值只经 overlay 注入容器环境，
	// 不写进任何文件、也不进日志。
	// The credential placeholder; the value reaches the container through the overlay only.
	us25ProbeKeyEnv   = "BASALT_US25_PROBE_KEY"
	us25ProbeKeyValue = "us25-probe-not-a-secret"

	// us25ProbeCron 是装载与重算的合法输入，同时**刻意**选成闰日 02-29 03:00——下一次
	// 激活在 2028 年，墙钟不可能与用例的上弦撞车；若写成每天都跑，恰好在此刻跑测试就会
	// 多出一条不是本用例上弦产生的执行行，判据随之失真。
	// The expression deliberately lands on Feb 29 so the wall clock can never race the
	// armed injection; a daily expression would add an execution row the test did not arm.
	us25ProbeCron = "0 0 3 29 2 *"

	// us25ProbeArmDelay 是上弦后留给到期扫描的余量（poll_interval_ms=500）。
	// The head start left to the due scan after arming (poll_interval_ms=500).
	us25ProbeArmDelay = 1500 * time.Millisecond

	// us25ProbeRefusedURL 是必然拒连的回环地址：容器内 1 端口没有监听者。
	// A port nothing listens on: the provider call fails immediately.
	us25ProbeRefusedURL = "http://127.0.0.1:1/v1"

	// us25HangService 是挂起监听器的服务名（同时是 provider 的主机名）。
	// The hanging peer's service name, doubled as the provider's host.
	us25HangService = "us25-hang"

	// us25HangBaseURL 指向挂起监听器：TCP 能连上、但永远收不到响应。
	// Connects but never answers.
	us25HangBaseURL = "http://" + us25HangService + ":9099/v1"
)

// us25ProbeLifecyclePath 是探针 Agent 的生命周期状态文件（容器内绝对路径）。
// 状态文件是生命周期的唯一权威；数据库的 agent_lifecycle 只是可重建索引。
// The lifecycle state file inside the control container: the sole authority.
const us25ProbeLifecyclePath = "/data/.basalt/control/agents/" + us25ProbeAgent + ".json"

// controllerTemplatePath 与控制面模板路径（镜像内，entrypoint 之后永不覆盖）。
// The control-plane config template inside the image.
const controllerTemplatePath = "/opt/basalt/basalt.yaml"

// workerTemplatePath 是 Worker 模板路径（没有 http / callers 两段）。
// The worker config template inside the image (no http or callers section).
const workerTemplatePath = "/opt/basalt/basalt.worker.yaml"

// us25ProbeProvidersBlock 返回探针的 `providers:` 段：一家指向给定 base_url 的
// provider，凭据是 ${VAR} 占位（装载期解析，缺失即拒绝加载）。
// us25ProbeProvidersBlock renders the probe provider pointing at baseURL.
func us25ProbeProvidersBlock(baseURL string) string {
	return "providers:\n" +
		"  - name: " + us25ProbeProvider + "\n" +
		"    base_url: " + baseURL + "\n" +
		"    api_key: ${" + us25ProbeKeyEnv + "}\n" +
		"    default_model: " + us25ProbeModel + "\n"
}

// us25ProbeAgentDoc 返回探针的 AGENT.md：一个 6 段 cron 任务、无工具、无 MCP、
// 通知关闭。provider.model 必须非空（装载规则 3），schedules[].message 必填。
//
// modelUID 为空时写空的绑定组：首次启动之前拿不到模型连接的资源 UID（它由启动基线
// 随机铸造）；非空时写 provider 引用对应的绑定——准入要求「引用与绑定一一对应」
// （`core.ResourceBindings.ValidateCorrespondence`），缺这一条会以
// 「agent resource bindings are missing」拒绝新 Run。
//
// us25ProbeAgentDoc renders the probe AGENT.md. An empty modelUID writes empty binding
// groups because the resource uid only exists after the first boot; the bound form is
// what admission requires, since a declared provider must have a matching binding.
func us25ProbeAgentDoc(modelUID string) string {
	bindings := "resource_bindings:\n  model_connection: []\n  notify_channel: []\n"
	if modelUID != "" {
		bindings = "resource_bindings:\n  model_connection:\n    - name: " + us25ProbeProvider +
			"\n      uid: " + modelUID + "\n  notify_channel: []\n"
	}
	return fmt.Sprintf(`---
name: %s
description: US-25 容器验收探针（定时触发，无工具）
provider:
  name: %s
  model: %s
tools: []
mcp_servers: []
notify_mode: none
%sschedules:
  - id: %s
    cron: "%s"
    zone: UTC
    message: US-25 探针定时触发
---
US-25 容器验收用的最小 Agent：只由定时触发驱动，正文不参与执行。
`, us25ProbeAgent, us25ProbeProvider, us25ProbeModel, bindings, us25ProbeTaskID, us25ProbeCron)
}

// us25ProbeAgentDocScript 只（覆盖）写探针 AGENT.md：首次启动读到资源 UID 之后，
// 把带绑定的定义写回工作区，随后由 `assets install` 构建进 bundle。
// us25ProbeAgentDocScript overwrites only the probe AGENT.md with the bound definition.
func us25ProbeAgentDocScript(modelUID string) string {
	dir := "/data/.basalt/agents/" + us25ProbeAgent
	return "set -eu\n" +
		"mkdir -p " + dir + "\n" +
		"cat > " + dir + "/AGENT.md <<'MD'\n" +
		us25ProbeAgentDoc(modelUID) +
		"MD\n"
}

// us25ProbeConfigScript 返回只写 /data/basalt.yaml（角色模板 + 探针 provider 段）的脚本。
//
// 两条判据落在这里：
//   - 种子归档**不含** basalt.yaml，所以 Worker 侧的配置只能由本脚本写；
//   - 配置必须在**首次启动之前**就位：启动基线按配置里的 provider 建 v1 并发布指针，
//     之后 enable 的资源就绪判据才通过。
//
// 它**不碰** `.basalt/agents/`：Worker 的 AGENT.md 由种子带来，且是**已绑定资源 UID**
// 的那一份——bundle 的 `agents/<name>/AGENT.md` 正是按这些字节构建的，准入的
// `checkDefinitionMatchesBundle` 会逐字节比对工作副本。在 Worker 侧重写定义会让两处
// 从这一刻起分叉，准入以「agent bundle is unavailable or disagrees with the shared
// publication record」拒绝每一条新 Run。
//
// The script writes only basalt.yaml (role template plus the probe provider block). It
// must run before the first boot so the baseline publishes the config provider, and it
// must never touch the agents directory: the seeded AGENT.md is the bound definition the
// published bundle was built from, and admission compares those very bytes.
func us25ProbeConfigScript(template, baseURL string) string {
	return "set -eu\n" +
		"cp " + template + " /data/basalt.yaml\n" +
		"printf '\\n' >> /data/basalt.yaml\n" +
		"cat >> /data/basalt.yaml <<'YAML'\n" +
		us25ProbeProvidersBlock(baseURL) +
		"YAML\n"
}

// us25ProbeWorkspaceScript 在配置脚本之外再把探针 AGENT.md 放进工作区——只用于控制面
// 的**首次启动之前**：那时还没有资源 UID，写下的只能是未绑定定义，随后由
// `installProbeAssets` 用绑定后的字节覆盖并构建 bundle。
//
// 一次性容器以镜像 USER（10001）运行，写下的文件属主本来就对，**不得** chown
// （非 root 也做不到）。
//
// us25ProbeWorkspaceScript additionally drops the unbound probe AGENT.md; it is for the
// control plane's pre-first-boot only, where no resource uid exists yet.
func us25ProbeWorkspaceScript(template, baseURL string) string {
	dir := "/data/.basalt/agents/" + us25ProbeAgent
	return us25ProbeConfigScript(template, baseURL) +
		"mkdir -p " + dir + "\n" +
		"cat > " + dir + "/AGENT.md <<'MD'\n" +
		us25ProbeAgentDoc("") +
		"MD\n"
}

// addProbeEnvOverlay 把探针凭据注入控制面与 Worker 两个容器的环境。
// compose 的 environment 是映射形态，overlay 按 key 合并，不会顶掉模板变量。
// addProbeEnvOverlay injects the probe credential into both containers; compose merges
// the environment map key by key.
func (s *us25Stack) addProbeEnvOverlay(t *testing.T) string {
	t.Helper()
	body := fmt.Sprintf(`# US-25 夹具：探针 provider 的凭据（值不是真实密钥）。
services:
  %[1]s:
    environment:
      %[2]s: %[3]s
  %[4]s:
    environment:
      %[2]s: %[3]s
`, us25ControlService, us25ProbeKeyEnv, us25ProbeKeyValue, us25WorkerService)
	return s.addOverlay(t, body)
}

// addHangService 追加一个「只接受连接、永不回应」的 HTTP 对端（控制面 profile）。
//
// 实现要点：busybox nc 的 stdin 打开的是**同一个 FIFO 的读写端**（`<>`）。用 `<` 会
// 卡在 open 上等写者、nc 根本不会 listen；用 `<>` 立刻打开、nc 正常监听，而 FIFO
// 永远没有数据，于是它一个字节也不回、连接也不会断——正是「调用已发出、结果不可知」
// 需要的现场。
//
// addHangService appends a peer that accepts and never answers. busybox nc's stdin is
// the same FIFO opened read-write: `<` would block in open and nc would never listen,
// while `<>` listens and simply never writes a byte.
func (s *us25Stack) addHangService(t *testing.T) string {
	t.Helper()
	body := fmt.Sprintf(`# US-25 夹具：挂起的 HTTP 对端（只接受连接、永不回应）。
services:
  %[1]s:
    profiles: [control]
    image: ${BASALT_IMAGE:?set BASALT_IMAGE}
    user: "10001:10001"
    init: true
    read_only: true
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    entrypoint: ["/bin/sh", "-c"]
    command: ["mkfifo /tmp/hold && exec nc -l -p 9099 <> /tmp/hold"]
    tmpfs:
      - /tmp:rw,noexec,nosuid,size=16m
`, us25HangService)
	return s.addOverlay(t, body)
}

// prepareControlForProbe 在控制面容器**创建之后、首次启动之前**写入配置与探针
// Agent，返回控制面工作区卷名。
//
// `compose create` 会一并创建同 profile 的预检与数据库容器（v5.6.1 的 create 没有
// --no-deps），但它们此刻只被创建、不启动，工作区仍是空的。
// prepareControlForProbe creates the control container, then writes the probe config and
// AGENT.md into its workspace before anything starts.
func (s *us25Stack) prepareControlForProbe(t *testing.T, baseURL string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	out, err := s.compose(ctx, "--profile", us25ControlProfile, "create", "--no-recreate", us25ControlService)
	if err != nil {
		t.Fatalf("创建控制面容器失败：%v（%s）", err, us18Tail(out, 4000))
	}
	volume := s.projectVolumeName(t, ctx, "control-workspace")
	if out, err := s.execInVolume(ctx, volume, us25ProbeWorkspaceScript(controllerTemplatePath, baseURL)); err != nil {
		t.Fatalf("写控制面探针配置失败：%v（%s）", err, us18Tail(out, 4000))
	}
	return volume
}

// prepareWorkerForProbe 在**种子导入之后、Worker 启动之前**写入 Worker 配置。
// 种子归档不含 basalt.yaml（配置属于各宿主自己的部署事实），而 Worker 的 provider
// 段必须与控制面一致，否则同一任务在一侧可领取、另一侧拒绝。
//
// 只写配置、不写 AGENT.md：种子里的定义是**已绑定**的那一份，也正是 bundle 构建时
// 的字节；任何在 Worker 侧重写定义的动作都会让准入的逐字节比对失败。
// prepareWorkerForProbe writes only the worker config; the seeded AGENT.md is the bound
// definition the published bundle was built from and must not be rewritten.
func (s *us25Stack) prepareWorkerForProbe(t *testing.T, volume, baseURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if out, err := s.execInVolume(ctx, volume, us25ProbeConfigScript(workerTemplatePath, baseURL)); err != nil {
		t.Fatalf("写 Worker 探针配置失败：%v（%s）", err, us18Tail(out, 4000))
	}
}

// installProbeAssets 在控制面**停机**时显式安装探针 Agent 的资产 bundle。
//
// 为什么必须显式装：新 Agent 没有共享发布记录，首启基线只写生命周期身份、不自动
// 发布（`assets_publish.go` 的「首启不自动发布」）；不装就发 Run，准入会以
// 「agent bundle is unavailable or disagrees with the shared publication record」拒绝，
// 执行行根本不会出现——用例看起来像"调度没触发"，实际是资产面缺件。
//
// 为什么必须趁停机：实例锁（`.basalt/basalt.lock`）由运行中的 serve / gateway **全程**
// 持有，`assets install` 是写模式命令，锁被活实例持有时拒绝启动。这条同时是 FR-25-22
// 「Worker 不执行资产安装或发布」的物理边界：Worker 在跑就装不了，装完再起进程。
//
// 它还顺带补上**模型连接绑定**：模型连接的资源 UID 由启动基线随机铸造，只能在首次
// 启动之后读出来；准入要求 provider 引用与 `resource_bindings` 一一对应，所以先把带
// UID 的 AGENT.md 写回工作区、再构建 bundle（bundle 里的定义必须与工作区逐字一致）。
//
// 返回时控制面**仍是停的**，调用方要自己把它起回来（`up(..., "control")`）。
//
// installProbeAssets installs the probe agent's bundle while the control plane is
// stopped: a brand-new agent has no shared publication record and the startup baseline
// never publishes one, while the instance lock held for the whole runtime lifetime makes
// an install impossible from any live serve or gateway. It also writes the model
// connection binding, whose resource uid only exists after the first boot. The caller
// restarts the control.
func (s *us25Stack) installProbeAssets(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	// 绑定用的 UID 从共享发布记录里读；基线发布指针可能晚于首次启动的其它步骤，
	// 所以轮询到出现为止，读不到就判红而不是写一条猜的 UID。
	modelUID := s.waitForQuery(t, 2*time.Minute, "探针模型连接资源已发布",
		"SELECT resource_uid FROM resource_pointers "+
			"WHERE resource_kind = 'model_connection' AND resource_id = '"+us25ProbeProvider+"'",
		func(v string) bool { return strings.HasPrefix(v, "rs_") })

	if err := s.stopService(ctx, us25ControlService); err != nil {
		t.Fatalf("停控制面失败：%v", err)
	}
	if code := s.awaitExit(t, us25ControlService, us25ExitTimeout); code != 0 {
		logs, _ := s.logs(ctx, us25ControlService)
		t.Fatalf("停控制面退出码 %d（要求 0），日志尾部：\n%s", code, us18Tail(logs, 4000))
	}
	// 一次性容器走镜像入口：DSN 由 resolve_dsn 从 secret 文件导出，工作区卷与配置
	// 与控制面完全相同；`--no-deps` 不动已经在跑（或已停）的其他服务。
	volume := s.projectVolumeName(t, ctx, "control-workspace")
	if out, err := s.execInVolume(ctx, volume, us25ProbeAgentDocScript(modelUID)); err != nil {
		t.Fatalf("写带模型连接绑定的 AGENT.md 失败：%v（%s）", err, us18Tail(out, 2000))
	}
	out, err := s.runOneOff(ctx, us25ControlProfile, nil, us25ControlService,
		"assets", "install", "--agent", us25ProbeAgent)
	if err != nil {
		t.Fatalf("显式安装探针资产失败：%v（%s）", err, us18Tail(out, 4000))
	}
	// 退出码 0 不等于装上了：回读卷里的内容寻址目录，确认 bundle 真的落在工作区
	// （它随后会随种子归档带到 Worker）。
	dir := "/data/.basalt/assets/bundles/" + us25ProbeAgent
	check := "set -eu\ntest -d " + dir + "\nls " + dir
	if listed, err := s.execInVolume(ctx, volume, check); err != nil {
		t.Fatalf("探针资产没有落进控制面工作区（%s）：%v（%s）", dir, err, us18Tail(listed, 2000))
	} else {
		t.Logf("探针资产已安装：%s → %s", us25ProbeAgent, strings.TrimSpace(listed))
	}
}

// probeLifecycleState 读探针 Agent 的生命周期状态文件（state + generation）。
// 迁移请求的 expected_generation 必须取自这里，不能写死。
// probeLifecycleState reads the probe's lifecycle state file.
func (s *us25Stack) probeLifecycleState(ctx context.Context) (string, int64, error) {
	out, err := s.exec(ctx, us25ControlService, "cat", us25ProbeLifecyclePath)
	if err != nil {
		return "", 0, err
	}
	body := strings.TrimSpace(out)
	var doc struct {
		State      string `json:"state"`
		Generation int64  `json:"generation"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return "", 0, fmt.Errorf("解析生命周期状态文件失败：%w（%s）", err, body)
	}
	if doc.State == "" {
		return "", 0, fmt.Errorf("生命周期状态文件缺少 state（%s）", body)
	}
	return doc.State, doc.Generation, nil
}

// enableProbeAgent 把探针 Agent 从 disabled 迁到 active。
//
// 请求体写 `expected_state: disabled`：启停是 CAS，当前态不符会被拒；代数每次重试都
// 重新从状态文件读。判定以**状态文件**为准（它是唯一权威），响应体里的 state 只作
// 二次断言——busybox wget 拿不到状态码，只能靠退出码与正文区分成功与失败。
//
// enableProbeAgent transitions the probe to active through the admin endpoint. The
// verdict is the state file (the authority); the response body is a second assertion.
func (s *us25Stack) enableProbeAgent(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	deadline := time.Now().Add(4 * time.Minute)
	last := ""
	for time.Now().Before(deadline) {
		state, generation, err := s.probeLifecycleState(ctx)
		switch {
		case err != nil:
			last = err.Error()
		case state == "active":
			return
		default:
			script := fmt.Sprintf(`set -eu
TOKEN=$(cat /var/lib/basalt/basalt/instances/*/admin-token)
wget -q -O - --header "Authorization: Bearer $TOKEN" --header "Content-Type: application/json" \
  --post-data '{"expected_state":"disabled","expected_generation":%d}' \
  http://127.0.0.1:8080/api/v1/agents/%s/enable
`, generation, us25ProbeAgent)
			out, err := s.exec(ctx, us25ControlService, "sh", "-c", script)
			last = strings.TrimSpace(out)
			if err == nil && strings.Contains(out, `"state":"active"`) {
				return
			}
			if err != nil {
				last = err.Error()
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("探针 Agent 在 4 分钟内没有进入 active（最后一次读数：%s）", last)
}

// waitProbeTaskRow 等探针的定时任务行出现且启用；启用会触发一次定义重算，任务行
// 由那次重算写入（禁用态不挂任务）。
// waitProbeTaskRow waits for the scheduled_tasks row; enabling triggers the reconcile
// that writes it.
func (s *us25Stack) waitProbeTaskRow(t *testing.T, timeout time.Duration) {
	t.Helper()
	s.waitForQuery(t, timeout, "探针定时任务行就绪",
		"SELECT count(*) || ':' || COALESCE(bool_and(enabled)::text, '-') "+
			"FROM scheduled_tasks WHERE task_id = '"+us25ProbeTaskID+"'",
		func(v string) bool { return v == "1:true" })
}

// armProbe 把任务的到期时刻推到 `delay` 之后：下一轮到期扫描就会按真实领取路径
// 选中它。0 行受影响说明任务行不存在（上弦失败要立刻判红，而不是等满超时）。
// armProbe pushes next_fire_at into the near future so the next due scan claims it
// through the real path; zero affected rows means the task row is missing.
func (s *us25Stack) armProbe(t *testing.T, delay time.Duration) {
	t.Helper()
	tag := s.psql(t, fmt.Sprintf(
		"UPDATE scheduled_tasks SET next_fire_at = (extract(epoch from clock_timestamp())*1000)::bigint + %d "+
			"WHERE task_id = '%s' AND retired_at IS NULL",
		delay.Milliseconds(), us25ProbeTaskID))
	if tag != "UPDATE 1" {
		t.Fatalf("上弦探针任务失败：UPDATE 影响 %q（要求 UPDATE 1）", tag)
	}
}

// waitForQuery 轮询一条只读查询，直到 ok 为真；超时判红并带最后一次读数。
// waitForQuery polls one read-only query until ok holds, naming the last reading.
func (s *us25Stack) waitForQuery(t *testing.T, timeout time.Duration, label, query string, ok func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		last = s.psql(t, query)
		if ok(last) {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s：%s 内未达成（最后一次读数 %q）", label, timeout, last)
	return last
}

// waitLiveWorkerIDs 等在线 Worker（stopped_at 为空）恰好 want 个，返回其 InstanceID。
// 暂停中的容器**不会**写 stopped_at，因此它仍计为在线：这正是接管场景要的语义。
// waitLiveWorkerIDs waits for exactly want live worker rows; a paused container has no
// stopped_at and therefore still counts as live.
func (s *us25Stack) waitLiveWorkerIDs(t *testing.T, want int, timeout time.Duration) []string {
	t.Helper()
	wantText := fmt.Sprintf("%d", want)
	out := s.waitForQuery(t, timeout, fmt.Sprintf("%d 个在线 Worker", want),
		"SELECT count(*)::text FROM worker_nodes WHERE stopped_at IS NULL",
		func(v string) bool { return v == wantText })
	if out != wantText {
		t.Fatalf("在线 Worker 数读数异常：%q", out)
	}
	raw := s.psql(t, "SELECT string_agg(instance_id, ',' ORDER BY instance_id) FROM worker_nodes WHERE stopped_at IS NULL")
	return strings.Split(raw, ",")
}

// waitNewLiveWorker 等一个**不同于 except** 的在线 Worker 出现（接管场景里控制面
// 被暂停、Worker 后起），返回它的 InstanceID。
// waitNewLiveWorker waits for a live worker other than except.
func (s *us25Stack) waitNewLiveWorker(t *testing.T, except string, timeout time.Duration) string {
	t.Helper()
	raw := s.waitForQuery(t, timeout, "新 Worker 上线",
		"SELECT string_agg(instance_id, ',' ORDER BY instance_id) FROM worker_nodes "+
			"WHERE stopped_at IS NULL AND instance_id <> '"+except+"'",
		func(v string) bool { return v != "" })
	ids := strings.Split(raw, ",")
	if len(ids) == 0 || ids[0] == "" {
		t.Fatalf("新 Worker 的 InstanceID 读数为空")
	}
	return ids[0]
}

// probeExecutionLines 返回探针任务的全部执行行：`id|planned_at|status|owner`。
// probeExecutionLines returns every execution row of the probe task.
func (s *us25Stack) probeExecutionLines(t *testing.T) []string {
	t.Helper()
	out := s.psql(t, "SELECT id || '|' || planned_at || '|' || status || '|' || "+
		"COALESCE(owner_worker_id, '-') FROM task_executions "+
		"WHERE task_id = '"+us25ProbeTaskID+"' ORDER BY id")
	if out == "" {
		return nil
	}
	return us18NonEmptyLines(out)
}

// waitProbeExecution 等探针任务出现第 `want` 条执行行（按 id 升序），返回它。
// 用序号而不是时刻判定，是为了让「上弦一次只多一行」本身成为断言。
// waitProbeExecution waits until the probe task has want execution rows in id order.
func (s *us25Stack) waitProbeExecution(t *testing.T, want int, timeout time.Duration) []string {
	t.Helper()
	wantText := fmt.Sprintf("%d", want)
	s.waitForQuery(t, timeout, fmt.Sprintf("第 %d 条执行行", want),
		"SELECT count(*)::text FROM task_executions WHERE task_id = '"+us25ProbeTaskID+"'",
		func(v string) bool { return v == wantText })
	lines := s.probeExecutionLines(t)
	if len(lines) != want {
		t.Fatalf("执行行数 %d（要求 %d）：%v", len(lines), want, lines)
	}
	return strings.Split(lines[want-1], "|")
}

// waitExecutionStatus 等一条执行行离开 started（进入四值域的终态）。
// waitExecutionStatus waits for the execution row to leave started.
func (s *us25Stack) waitExecutionStatus(t *testing.T, executionID string, timeout time.Duration) string {
	t.Helper()
	return s.waitForQuery(t, timeout, "执行行收尾",
		"SELECT status FROM task_executions WHERE id = "+executionID,
		func(v string) bool { return v != "" && v != "started" })
}

// assertSingleExecution 判同一 `(task_id, planned_at)` 只有一条执行行：唯一索引是
// 物理防线，这里的读数是给失败信息用的证据。
// assertSingleExecution judges the one-execution-per-planned-instant rule.
func (s *us25Stack) assertSingleExecution(t *testing.T, plannedAt string) {
	t.Helper()
	got := s.psql(t, "SELECT count(*)::text FROM task_executions "+
		"WHERE task_id = '"+us25ProbeTaskID+"' AND planned_at = "+plannedAt)
	if got != "1" {
		lines := s.probeExecutionLines(t)
		t.Fatalf("planned_at=%s 的执行行有 %s 条（要求 1）：%v", plannedAt, got, lines)
	}
}

// probeLease 读一次执行的租约：`owner|epoch|released_at`（未释放记 -）。
// probeLease reads the execution lease behind one execution row.
func (s *us25Stack) probeLease(t *testing.T, executionID string) []string {
	t.Helper()
	out := s.psql(t, "SELECT owner_worker_id || '|' || ownership_epoch || '|' || "+
		"COALESCE(released_at::text, '-') FROM execution_leases "+
		"WHERE kind = 'schedule_execution' AND resource_id = '"+executionID+"'")
	if out == "" {
		t.Fatalf("执行行 %s 没有 schedule_execution 租约", executionID)
	}
	return strings.Split(out, "|")
}

// probeLeaseEvents 读一次执行的全部租约事件：`event|owner|epoch|previous_owner`。
// probeLeaseEvents reads every lease event of one execution lease.
func (s *us25Stack) probeLeaseEvents(t *testing.T, executionID string) []string {
	t.Helper()
	out := s.psql(t, "SELECT event || '|' || owner_worker_id || '|' || ownership_epoch || '|' || "+
		"COALESCE(previous_owner_worker_id, '-') FROM lease_events "+
		"WHERE kind = 'schedule_execution' AND resource_id = '"+executionID+"' ORDER BY event_id")
	if out == "" {
		return nil
	}
	return us18NonEmptyLines(out)
}

// probeRunID 读执行行关联的 Run 标识。
// probeRunID reads the run id carried by one execution row.
func (s *us25Stack) probeRunID(t *testing.T, executionID string) string {
	t.Helper()
	runID := s.psql(t, "SELECT COALESCE(run_id, '') FROM task_executions WHERE id = "+executionID)
	if runID == "" {
		t.Fatalf("执行行 %s 没有关联 Run", executionID)
	}
	return runID
}

// probeReplaySnapshot 取一份「零重放」指纹：本 Agent 的 runs 数 + 该 Run 的
// messages / llm_calls / tool_invocations 行数。接管只许改状态，不许新增行。
// probeReplaySnapshot fingerprints the row counts takeover must not change.
func (s *us25Stack) probeReplaySnapshot(t *testing.T, runID string) string {
	t.Helper()
	return s.psql(t, "SELECT (SELECT count(*) FROM runs WHERE agent = '"+us25ProbeAgent+"') || '|' || "+
		"(SELECT count(*) FROM messages WHERE run_id = '"+runID+"') || '|' || "+
		"(SELECT count(*) FROM llm_calls WHERE run_id = '"+runID+"') || '|' || "+
		"(SELECT count(*) FROM tool_invocations WHERE run_id = '"+runID+"')")
}

// waitForLog 轮询一个服务的容器日志，直到出现 needle；超时把日志尾部带进失败信息。
// waitForLog polls one service's container log for a needle.
func (s *us25Stack) waitForLog(t *testing.T, service, needle string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout+45*time.Second)
	defer cancel()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		out, err := s.logs(ctx, service)
		if err == nil {
			last = out
			if strings.Contains(out, needle) {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s 的日志在 %s 内没有出现 %q，尾部：\n%s", service, timeout, needle, us18Tail(last, 5000))
}
