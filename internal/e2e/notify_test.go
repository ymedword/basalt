//go:build e2e

package e2e

// 本文件回填 US-8（Notify）的六个人工项（模块卡 §2.6.8 映射表）。全部用例**只走拒绝
// 路径与登记面**：真实 webhook / 飞书外发归收口时授权执行的 `TestBackfill_RealFeishu`，
// 本文件不发起任何真实外发，接收端也不监听在策略放行的默认端口上。
// This file backfills US-8's six manual items through rejection paths and the registry
// surface only; real outbound sends belong to TestBackfill_RealFeishu.

import (
	"testing"
	"time"
)

const (
	// notifyChannelName 是种子渠道名（渠道名大小写敏感、逐字匹配）。
	// notifyChannelName is the seeded channel name.
	notifyChannelName = "ops"
	// notifyEnvRef 是渠道 URL 占位引用的变量名。
	// notifyEnvRef is the placeholder variable the channel URL refers to.
	notifyEnvRef = "BASALT_NOTIFY_OPS"
)

// TestBackfill_Notify 覆盖通知渠道的六个人工项。
// TestBackfill_Notify covers the six manual items of the notify module.
func TestBackfill_Notify(t *testing.T) {
	t.Run("channel_registration", testNotifyChannelRegistration)
	t.Run("sentinel_redaction", testNotifySentinelRedaction)
	t.Run("agent_decl", testNotifyAgentDecl)
	t.Run("domain_denied", testNotifyDomainDenied)
	t.Run("env_negative", testNotifyEnvNegative)
	t.Run("effect_timing", testNotifyEffectTiming)
}

// testNotifyChannelRegistration：手工登记渠道（§6.1 第 4 项）。判据：直接往临时库插入
// `notify_channels` 行并重启后，工具从「渠道查无」转为「读到行、解析出 ${VAR}」——失败
// 下沉到策略判定；行内 `url` 必须是 `${VAR}` 引用形态，明文不入库。
func testNotifyChannelRegistration(t *testing.T) {
	content := "巡检摘要：登记渠道"
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-reg-1", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": content,
		})),
		textResponse("登记前调用完成"),
		toolCallResponse("call-reg-2", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": content,
		})),
		textResponse("重启后调用完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"notify"}), "渠道登记演示")
	env := append(fixtureEnv(), notifyEnvRef+"=http://127.0.0.1:1")

	before := h.run(t, env, "chat", "--agent", "demo", "--message", "登记前")
	requireExit(t, before, 0, "登记前调用")
	beforeReqs := fake.Requests()
	if len(beforeReqs) != 2 {
		t.Fatalf("登记前 chat 假模型应收到 2 次请求，实际 %d 次", len(beforeReqs))
	}
	requireToolResultContains(t, beforeReqs[1],
		"通知渠道 ops 不存在（可用渠道：（注册表为空））", "登记前查无")

	insertNotifyChannel(t, h, notifyChannelName, "webhook", "${"+notifyEnvRef+"}", "演示渠道")
	if typ, url := notifyChannelRow(t, h, notifyChannelName); typ != "webhook" || url != "${"+notifyEnvRef+"}" {
		t.Fatalf("登记的渠道行 = type %q / url %q，期望 webhook / ${%s}（引用形态不得明文）",
			typ, url, notifyEnvRef)
	}

	after := h.run(t, env, "chat", "--agent", "demo", "--message", "登记后")
	requireExit(t, after, 0, "重启后调用")
	reqs := fake.Requests()
	if len(reqs) != 4 {
		t.Fatalf("两次 chat 假模型应收到 4 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[3], "出网目标不是域名：http://127.0.0.1",
		"重启后读到渠道行并解析出 ${VAR}")
	requireAbsent(t, lastToolContent(reqs[3]), "不存在", "登记后不再查无")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, error_kind FROM tool_invocations ORDER BY id`)
	if len(rows) != 2 {
		t.Fatalf("tool_invocations 应有 2 行，实际 %d 行：%v", len(rows), rows)
	}
	if got, _ := rows[0]["error_kind"].(string); got != "not_found" {
		t.Fatalf("登记前 error_kind = %q，期望 not_found", got)
	}
	if got, _ := rows[1]["error_kind"].(string); got != "sandbox_denied" {
		t.Fatalf("登记后 error_kind = %q，期望 sandbox_denied（行已读到、失败下沉到策略）", got)
	}
}

// testNotifySentinelRedaction：凭证面（§6.1 第 7 项甲档）。哨兵值经渠道 URL 的 userinfo
// / path / query 进入判定拒绝后，进程输出、模型上下文、审计与工作区全部零回显，只出
// `scheme://host`。
func testNotifySentinelRedaction(t *testing.T) {
	const sentinel = "n0tify-sentinel-7d2a"
	target := "http://user:" + sentinel + "@notify-e2e.example/hook/" + sentinel + "?token=" + sentinel
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-sentinel", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": "脱敏读数",
		})),
		textResponse("脱敏读数完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+"http:\n  allowed_domains:\n    - other.example\n")
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"notify"}), "凭证脱敏演示")
	insertNotifyChannel(t, h, notifyChannelName, "webhook", "${"+notifyEnvRef+"}", "哨兵渠道")

	res := h.run(t, append(fixtureEnv(), notifyEnvRef+"="+target), "chat", "--agent", "demo", "--message", "触发拒绝")
	requireExit(t, res, 0, "凭证脱敏")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	result := lastToolContent(reqs[1])
	requireContains(t, result, "域名不在白名单：http://notify-e2e.example", "只出 scheme://host")
	requireAbsent(t, result, sentinel, "模型上下文不得回显哨兵")
	requireAbsent(t, result, "/hook/", "不得回显 path")
	requireAbsent(t, result, "user:", "不得回显 userinfo")
	requireAbsent(t, res.Stdout+res.Stderr, sentinel, "进程输出不得回显哨兵")
	requireAbsentInWorkspace(t, h.Root, sentinel, "哨兵不得落盘到工作区")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, error_kind, error_message FROM tool_invocations ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if got, _ := rows[0]["error_kind"].(string); got != "sandbox_denied" {
		t.Fatalf("审计 error_kind = %q，期望 sandbox_denied", got)
	}
	message, _ := rows[0]["error_message"].(string)
	requireContains(t, message, "域名不在白名单：http://notify-e2e.example", "审计只出 scheme://host")
	requireAbsent(t, message, sentinel, "审计不得回显哨兵")
	requireAbsent(t, message, "/hook/", "审计不得回显 path")
}

// testNotifyAgentDecl：Agent 声明（§6.1 第 9 项）。声明 `notify` 的 Agent 启动注册成功、
// 可聊；`profile list` 轻路径同结论；工具调用真实落到执行器并留下审计。
func testNotifyAgentDecl(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-decl", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": "声明面读数",
		})),
		textResponse("声明面调用完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL()))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"notify"}), "声明面演示")

	light := h.run(t, fixtureEnv(), "profile", "list")
	requireExit(t, light, 0, "profile list")
	requireContains(t, light.Stdout, "demo：registered=true", "轻路径确认注册")

	list := h.run(t, fixtureEnv(), "tool", "list")
	requireExit(t, list, 0, "tool list")
	requireContains(t, list.Stdout, "notify：", "工具清单含 notify")

	res := h.run(t, fixtureEnv(), "chat", "--agent", "demo", "--message", "调用通知")
	requireExit(t, res, 0, "声明后聊天")
	requireContains(t, res.Stdout, "声明面调用完成", "Agent 可聊")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	requireToolResultContains(t, reqs[1], "通知渠道 ops 不存在", "工具调用落到执行器")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT tool_name, status, error_kind FROM tool_invocations ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if got, _ := rows[0]["tool_name"].(string); got != "notify" {
		t.Fatalf("审计 tool_name = %q，期望 notify", got)
	}
	if got, _ := rows[0]["error_kind"].(string); got != "not_found" {
		t.Fatalf("审计 error_kind = %q，期望 not_found", got)
	}
}

// testNotifyDomainDenied：域名白名单拒绝（§6.1 第 10 项）。渠道域名不在
// `http.allowed_domains` → 发送前拒绝（`sandbox_denied`），模型与审计都只见
// `scheme://host`；判定先于外呼，输出不出现任何传输失败文案。
func testNotifyDomainDenied(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-domain", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": "域名拒绝读数",
		})),
		textResponse("域名拒绝完成"),
	)
	h.writeConfig(t, configWithProvider(fake.URL())+"http:\n  allowed_domains:\n    - other.example\n")
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"notify"}), "域名拒绝演示")
	insertNotifyChannel(t, h, notifyChannelName, "webhook", "${"+notifyEnvRef+"}", "域名渠道")

	res := h.run(t, append(fixtureEnv(), notifyEnvRef+"=http://notify-e2e.example/hook"), "chat",
		"--agent", "demo", "--message", "发送")
	requireExit(t, res, 0, "域名拒绝")
	reqs := fake.Requests()
	if len(reqs) != 2 {
		t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
	}
	result := lastToolContent(reqs[1])
	requireContains(t, result, "域名不在白名单：http://notify-e2e.example", "四段式段 2 只出 scheme://host")
	requireAbsent(t, result, "/hook", "段 2 不得含 path")
	requireAbsent(t, res.Stdout+res.Stderr, "连接失败", "拒绝先于外呼——不得出现传输失败")
	requireAbsent(t, res.Stdout+res.Stderr, "DNS 解析失败", "拒绝先于外呼——不得出现 DNS 失败")

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT status, error_kind, error_message FROM tool_invocations ORDER BY id`)
	if len(rows) != 1 {
		t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(rows), rows)
	}
	if got, _ := rows[0]["status"].(string); got != "failed" {
		t.Fatalf("审计 status = %q，期望 failed", got)
	}
	if got, _ := rows[0]["error_kind"].(string); got != "sandbox_denied" {
		t.Fatalf("审计 error_kind = %q，期望 sandbox_denied", got)
	}
	message, _ := rows[0]["error_message"].(string)
	requireContains(t, message, "域名不在白名单：http://notify-e2e.example", "审计段 2 只出 scheme://host")
}

// testNotifyEnvNegative：环境负向（§6.1 第 11 项）。未设置与空值都必须 fail-closed，
// 错误点名 `${VAR}` 且不回显值；没有可解析目标，也就零外呼。
func testNotifyEnvNegative(t *testing.T) {
	cases := []struct {
		name string
		env  []string
	}{
		{name: "unset", env: fixtureEnv()},
		{name: "empty", env: append(fixtureEnv(), notifyEnvRef+"=")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			initWorkspace(t, h)
			fake := newFakeModel(t,
				toolCallResponse("call-env", "notify", mustJSON(map[string]string{
					"channel": notifyChannelName, "content": "环境负向读数",
				})),
				textResponse("环境负向完成"),
			)
			h.writeConfig(t, configWithProvider(fake.URL()))
			h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"notify"}), "环境负向演示")
			insertNotifyChannel(t, h, notifyChannelName, "webhook", "${"+notifyEnvRef+"}", "环境渠道")

			res := h.run(t, tc.env, "chat", "--agent", "demo", "--message", "发送")
			requireExit(t, res, 0, "环境负向")
			reqs := fake.Requests()
			if len(reqs) != 2 {
				t.Fatalf("假模型应收到 2 次请求，实际 %d 次", len(reqs))
			}
			requireToolResultContains(t, reqs[1],
				"通知渠道 ops 引用的环境变量 ${"+notifyEnvRef+"} 未设置或为空", "点名占位变量")
			requireAbsent(t, res.Stdout+res.Stderr, "连接失败", "缺少目标即零外呼")

			db := openDB(t, h.dbPath())
			rows := queryRows(t, db, `SELECT error_kind, error_message FROM tool_invocations ORDER BY id`)
			if len(rows) != 1 {
				t.Fatalf("tool_invocations 应有 1 行，实际 %d 行：%v", len(rows), rows)
			}
			if got, _ := rows[0]["error_kind"].(string); got != "runtime_error" {
				t.Fatalf("审计 error_kind = %q，期望 runtime_error", got)
			}
			message, _ := rows[0]["error_message"].(string)
			requireContains(t, message, "${"+notifyEnvRef+"}", "审计点名占位变量")
		})
	}
}

// testNotifyEffectTiming：生效时机（§6.1 第 13 项）。同一进程内改渠道行、下一发即生效
// （每次发送读一次注册表、不缓存）；环境变量值在进程启动时继承，"改值需重启"由进程模型
// 本身保证（无法在运行期改子进程环境）——本用例守可观测的那一半。
func testNotifyEffectTiming(t *testing.T) {
	h := newHarness(t)
	initWorkspace(t, h)
	fake := newFakeModel(t,
		toolCallResponse("call-time-1", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": "时机一",
		})),
		textResponse("时机一完成"),
		toolCallResponse("call-time-2", "notify", mustJSON(map[string]string{
			"channel": notifyChannelName, "content": "时机二",
		})),
		textResponse("时机二完成"),
	)
	addr := freeLoopbackAddr(t)
	h.writeConfig(t, configWithAddr(fake.URL(), addr))
	h.writeAgent(t, "demo", agentFrontmatter("demo", []string{"notify"}), "生效时机演示")
	insertNotifyChannel(t, h, notifyChannelName, "webhook", "${BASALT_NOTIFY_OPS_A}", "时机渠道")

	env := append(fixtureEnv(), "BASALT_NOTIFY_OPS_B=http://127.0.0.1:1")
	startServeWithEnv(t, h, addr, env)

	first := invokeAgent(t, addr, "demo", "时机一")
	if first.FinalText != "时机一完成" {
		t.Fatalf("第一次最终文本 = %q，期望 时机一完成", first.FinalText)
	}

	updateNotifyChannelURL(t, h, notifyChannelName, "${BASALT_NOTIFY_OPS_B}")

	second := invokeAgent(t, addr, "demo", "时机二")
	if second.FinalText != "时机二完成" {
		t.Fatalf("第二次最终文本 = %q，期望 时机二完成", second.FinalText)
	}

	db := openDB(t, h.dbPath())
	rows := queryRows(t, db, `SELECT error_message FROM tool_invocations ORDER BY id`)
	if len(rows) != 2 {
		t.Fatalf("tool_invocations 应有 2 行，实际 %d 行：%v", len(rows), rows)
	}
	firstMessage, _ := rows[0]["error_message"].(string)
	requireContains(t, firstMessage, "${BASALT_NOTIFY_OPS_A}", "首次发送读的是旧行")
	secondMessage, _ := rows[1]["error_message"].(string)
	requireContains(t, secondMessage, "出网目标不是域名：http://127.0.0.1",
		"改行后下一发即生效（无需重启）")
	requireAbsent(t, secondMessage, "BASALT_NOTIFY_OPS_A", "第二次不得再引用旧占位")
}

// startServeWithEnv 以指定环境拉起 `basalt serve` 并等健康检查就绪。
// startServeWithEnv launches `basalt serve` with the given environment.
func startServeWithEnv(t *testing.T, h *harness, addr string, env []string) *process {
	t.Helper()
	p := h.start(t, env, "serve")
	p.waitHTTP(t, "http://"+addr+"/api/v1/health", 30*time.Second)
	return p
}

// insertNotifyChannel 直接往临时库插入一行渠道登记（不经工具面）。
// insertNotifyChannel inserts one channel row directly into the temporary database.
func insertNotifyChannel(t *testing.T, h *harness, name, typ, url, description string) {
	t.Helper()
	db := openDB(t, h.dbPath())
	if _, err := db.Exec(
		`INSERT INTO notify_channels(name, type, url, description, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, typ, url, description, time.Now().UnixMilli()); err != nil {
		t.Fatalf("插入渠道行失败: %v", err)
	}
}

// updateNotifyChannelURL 直接改渠道行的 url（生效时机用）。
// updateNotifyChannelURL rewrites the channel URL in place.
func updateNotifyChannelURL(t *testing.T, h *harness, name, url string) {
	t.Helper()
	db := openDB(t, h.dbPath())
	result, err := db.Exec(`UPDATE notify_channels SET url = ? WHERE name = ?`, url, name)
	if err != nil {
		t.Fatalf("更新渠道行失败: %v", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("读取更新行数失败: %v", err)
	}
	if affected != 1 {
		t.Fatalf("更新渠道行影响 %d 行，期望 1 行", affected)
	}
}

// notifyChannelRow 读回渠道行的类型与 URL（登记面判据）。
// notifyChannelRow reads back the channel type and URL.
func notifyChannelRow(t *testing.T, h *harness, name string) (typ, url string) {
	t.Helper()
	db := openDB(t, h.dbPath())
	if err := db.QueryRow(
		`SELECT type, url FROM notify_channels WHERE name = ?`, name,
	).Scan(&typ, &url); err != nil {
		t.Fatalf("读取渠道行 %s 失败: %v", name, err)
	}
	return typ, url
}
