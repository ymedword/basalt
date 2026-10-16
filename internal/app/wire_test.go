package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	// 判卷侧走裸连接读库（与 providers_test.go / realcall_integration_test.go 同一先例）。
	_ "modernc.org/sqlite"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// 本文件是重路径装配的验收面（颗粒度文档 §3.2 的 app/wire_test.go 5 条 T1~T5）：
//
//	T1 重路径次序在日志里可见（取锁 → 打开 → 迁移 → 恢复）
//	T2 注册池名字集流入装载器（池内名字放行、池外名字整只不注册）
//	T3 端口注入（命令面的两个端口确实到达唯一运行入口与会话仓储）
//	T4 轻路径零装配（只读命令不取锁、不开数据库写入）
//	T5 快照先落库再开跑（零模型调用也留下快照行）
//
// 全部离线：任何一条用例都不访问真实网络与真实模型——"零模型调用"由 `llm_calls` 行数
// 从库侧判，而不是由"我们没看见请求"推断（FR-005 / FR-020 的离线判据）。
// Every case is offline; "zero model calls" is read from the database, not inferred.

// wireFixture 是一次装配夹具：临时工作目录 + 已建骨架 + 一份只含"探针 provider"的配置。
// 探针 provider 的端点是回环上没人监听的端口——**不是**为了让调用失败，而是为了让任何一次
// 误发的调用都无法真的出网（用例从库侧判"零调用"，端点是第二道兜底）。
// The probe provider points at a dead loopback port so a stray call cannot leave the machine.
type wireFixture struct {
	workDir string
	root    string
	cfg     *cli.Config
	logs    *bytes.Buffer
	logger  *slog.Logger
}

// newWireFixture 建骨架 + 配置 + 记录型日志器；**不开库**（库由重路径自己开，
// 轻路径用例据此判"库文件从未出现"）。
// newWireFixture builds the skeleton, the configuration and a recording logger; no
// database is created here.
func newWireFixture(t *testing.T) *wireFixture {
	t.Helper()

	workDir := t.TempDir()
	root := filepath.Join(workDir, workspaceDirName)
	_, err := workspace.Init(root)
	require.NoError(t, err, "工作区骨架是重路径的前置条件")

	logs := &bytes.Buffer{}
	return &wireFixture{
		workDir: workDir,
		root:    root,
		cfg: &cli.Config{Providers: []cli.ProviderConfig{{
			Name:         "probe",
			BaseURL:      "http://127.0.0.1:1",
			APIKey:       "placeholder-unused-3c71",
			DefaultModel: "probe-model",
		}}},
		logs:   logs,
		logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// dbPath 返回夹具工作区里的库文件路径（判"库已建 / 库未建"共用同一处）。
func (f *wireFixture) dbPath() string {
	return filepath.Join(f.root, databaseFileName)
}

// writeAgent 落一份可装载的 AGENT.md：provider 指向探针，tools 取声明值。
// 名字声明与目录名一致是装载规则 1；声明 pool 外的工具名 → 整只不注册（规则 4）。
// writeAgent writes one loadable AGENT.md whose declared provider is the probe.
func (f *wireFixture) writeAgent(t *testing.T, name string, tools ...string) {
	t.Helper()

	dir := filepath.Join(f.root, agentsDirName, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	declared := "tools: []"
	if len(tools) > 0 {
		declared = "tools:\n"
		for _, toolName := range tools {
			declared += "  - " + toolName + "\n"
		}
	}
	content := "---\n" +
		"name: " + name + "\n" +
		"description: 装配探针\n" +
		"provider:\n  name: probe\n  model: probe-model\n" +
		declared +
		"---\n\n你是「" + name + "」。\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(content), 0o644))
}

// assemble 走一次重路径装配，登记清理；返回的句柄归用例所有。
// assemble runs one heavy-path assembly and registers its release.
func (f *wireFixture) assemble(t *testing.T) *heavyRuntime {
	t.Helper()

	rt, err := f.app().newRuntime(context.Background())
	require.NoError(t, err, "重路径装配必须成功（失败时先看日志里的 step 停在哪一步）")
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	return rt
}

// app 构造装配面：时钟与工作目录显式传入（时间显式传入是判卷前提）。
func (f *wireFixture) app() *App {
	return &App{
		workDir:          f.workDir,
		supportedVersion: storage.SupportedSchemaVersion,
		logger:           f.logger,
		cfg:              f.cfg,
	}
}

// logSteps 取出日志里带 `step` 键的记录，按出现次序返回取值。
// logSteps returns the `step` values of the log records that carry one, in order.
func logSteps(t *testing.T, logs string) []string {
	t.Helper()

	var steps []string
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "日志行必须是 JSON：%s", line)
		if value, ok := rec["step"].(string); ok {
			steps = append(steps, value)
		}
	}
	return steps
}

// findStep 返回某一步的记录（判携带的读数用）。
// findStep returns the record of one step.
func findStep(t *testing.T, logs, step string) map[string]any {
	t.Helper()

	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec))
		if value, ok := rec["step"].(string); ok && value == step {
			return rec
		}
	}
	require.Failf(t, "日志里没有这一步", "step=%s\n日志：\n%s", step, logs)
	return nil
}

// openRead 开一条只读判卷连接（裸 sql.Open，与既有先例同一手法）。
// openRead opens a read-only connection for the verdict queries.
func openRead(t *testing.T, path string) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}

// countRows 数一行标量读数（判"零调用 / 一行快照"这类计数）。
// countRows returns a single scalar count.
func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()

	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), query, args...).Scan(&n))
	return n
}

// writeInstanceConfig 落一份可装载的 basalt.yaml（provider 指向探针）并在进程内把凭证变量
// 设成非机密哨兵值：加载期的形态门禁拒绝明文，凭证本身也不该出现在测试里。
// writeInstanceConfig writes a loadable basalt.yaml; the credential stays a placeholder and
// the value it expands to is a non-secret sentinel set in-process.
func (f *wireFixture) writeInstanceConfig(t *testing.T) {
	t.Helper()

	t.Setenv("BASALT_WIRE_KEY", "wire-sentinel-not-a-secret")
	content := "providers:\n" +
		"  - name: probe\n" +
		"    base_url: http://127.0.0.1:1\n" +
		"    api_key: ${BASALT_WIRE_KEY}\n" +
		"    default_model: probe-model\n"
	require.NoError(t, os.WriteFile(filepath.Join(f.workDir, configFileName), []byte(content), 0o644))
}

// scriptedProviderClient 是进程内模型替身：按调用次序回放脚本，**不访问网络**。
// scriptedProviderClient replays one scripted response per call and never touches the network.
type scriptedProviderClient struct {
	mu      sync.Mutex
	replies []core.ChatResponse
	seen    []core.ChatRequest
}

// Chat 回放第 n 条脚本（脚本用尽后重复最后一条），并留存入站请求供判卷。
// Chat replays the n-th scripted response and records the inbound request.
func (c *scriptedProviderClient) Chat(_ context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seen = append(c.seen, req)
	idx := len(c.seen) - 1
	if idx >= len(c.replies) {
		idx = len(c.replies) - 1
	}
	return c.replies[idx], nil
}

// requests 复制已捕获的入站请求（加锁读取：调用在循环一侧，判卷在主 goroutine）。
// requests copies the captured inbound requests under the lock.
func (c *scriptedProviderClient) requests() []core.ChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]core.ChatRequest(nil), c.seen...)
}

// TestWire_HeavyPathStepsAppearInOrder 是 T1：重路径的四个次序读数在日志里**按序**可见，
// 且恢复完成的一条携带启动摘要的五个计数；装配失败时句柄不半开（锁被放回）。
func TestWire_HeavyPathStepsAppearInOrder(t *testing.T) {
	fx := newWireFixture(t)

	rt := fx.assemble(t)

	steps := logSteps(t, fx.logs.String())
	require.Equal(t, []string{"lock", "open", "migrate", "recover"}, steps,
		"重路径次序固定：取锁 → 写模式打开 → 显式迁移 → 崩溃恢复（次序即正确性）")

	recoverRec := findStep(t, fx.logs.String(), "recover")
	for _, key := range []string{"unknowns", "repaired_groups", "closed_runs", "rolled_back", "rolled_forward"} {
		require.Contains(t, recoverRec, key, "恢复完成的一条必须携带启动摘要（契约 §4.3.2 第 6 步）")
	}

	require.FileExists(t, fx.dbPath(), "重路径第二步写模式打开：库文件此时已存在")

	t.Run("Release 之后可再次装配", func(t *testing.T) {
		require.NoError(t, rt.Close(), "释放失败不得静默")

		second, err := fx.app().newRuntime(context.Background())
		require.NoError(t, err, "句柄关掉后锁必须回到可取得状态——锁没放回时这里会红")
		require.NoError(t, second.Close())
	})

	t.Run("骨架缺失时点名 init", func(t *testing.T) {
		bare := &App{
			workDir:          t.TempDir(), // 空目录：没有 .basalt
			supportedVersion: storage.SupportedSchemaVersion,
			logger:           fx.logger,
			cfg:              fx.cfg,
		}
		_, err := bare.newRuntime(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "basalt init", "骨架缺失要给出可操作的指引，而不是裸 ENOENT")
	})
}

// TestWire_RegistryNamesReachAgentLoader 是 T2：装配期注册池的名字集**流进**装载器——
// 声明池内工具名的 Agent 注册成功，声明池外工具名的 Agent 整只不注册（宪法 IV）。
// 判据落在行为上：同一个运行入口对前者不报"不存在"，对后者报"不存在"。
func TestWire_RegistryNamesReachAgentLoader(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader", "read_file")   // 池内：装配期注册的三件文件工具之一
	fx.writeAgent(t, "ghost", "no_such_tool") // 池外：整只不注册

	rt := fx.assemble(t)

	// 零模型调用的运行请求：上下文预算压到准入必拒（离线判据，见 T5）。
	budget := core.Budget{ContextTokens: 1}

	_, err := rt.runService().Run(context.Background(), core.RunRequest{
		Agent: "reader", SessionID: "wire-t2-ok", Message: "hi", Channel: "cli", Budget: budget,
	})
	require.NoError(t, err, "声明池内工具的 Agent 必须注册成功——名字集没喂进装载器时这里会报 Agent 不存在")

	_, err = rt.runService().Run(context.Background(), core.RunRequest{
		Agent: "ghost", SessionID: "wire-t2-ghost", Message: "hi", Channel: "cli", Budget: budget,
	})
	var notFound *core.AgentNotFoundError
	require.ErrorAs(t, err, &notFound, "声明池外工具的 Agent 整只不注册")
	require.Equal(t, "ghost", notFound.Name)

	require.Contains(t, fx.logs.String(), "tool not registered: no_such_tool",
		"拒绝原因点名缺的那个工具名（判定输入只有注册池这一份）")
}

// TestWire_PortsReachRunEntryAndSessionStore 是 T3：命令面消费的两个端口在装配后确实
// 到达唯一运行入口与会话仓储——判据是行为（错误类型 + 仓储读出），不是类型断言。
func TestWire_PortsReachRunEntryAndSessionStore(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader", "read_file")

	rt := fx.assemble(t)

	t.Run("RunService 到达 AgentService", func(t *testing.T) {
		_, err := rt.runService().Run(context.Background(), core.RunRequest{Agent: "nobody"})
		var notFound *core.AgentNotFoundError
		require.ErrorAs(t, err, &notFound,
			"准入判定的错误类型只有真运行入口给得出——端口接错时会退化成别的形态")
	})

	t.Run("SessionLister 到达同一库", func(t *testing.T) {
		sessions, err := rt.sessionLister().ListSessions(context.Background())
		require.NoError(t, err)
		require.Empty(t, sessions, "还一次运行都没有：清单为空")

		// 一次运行（预算压到零模型调用）会先建会话行——存储面写入后，只读端口必须看得见。
		_, err = rt.runService().Run(context.Background(), core.RunRequest{
			Agent: "reader", SessionID: "wire-t3", Message: "hi", Channel: "cli",
			Budget: core.Budget{ContextTokens: 1},
		})
		require.NoError(t, err)

		sessions, err = rt.sessionLister().ListSessions(context.Background())
		require.NoError(t, err)
		require.Len(t, sessions, 1)
		require.Equal(t, "wire-t3", sessions[0].ID)
	})

	// 包外装配面（`OpenRuntime` + `Runtime` 五个访问器）——评测基座走的那条路。判据是一条
	// **完整的闭环**：替身的两轮脚本跑通"模型 → 工具 → 回填 → 再问模型"，两个只读面读到的
	// 正是同一批审计行。
	// The exported surface is judged by one complete loop whose audit rows are read back
	// through the very accessors evals uses.
	t.Run("包外装配面：替身落在审计链内、两个只读面同源", func(t *testing.T) {
		// 独立夹具：实例锁按工作区一把，父夹具的运行期已持锁——共用会让这次装配取不到锁，
		// 而那与本子例要判的东西无关。
		outside := newWireFixture(t)
		outside.writeInstanceConfig(t)
		outside.writeAgent(t, "reader", "read_file")

		const body = "装配探针读到的正文"
		agentDir := filepath.Join(outside.root, agentsDirName, "reader")
		require.NoError(t, os.WriteFile(filepath.Join(agentDir, "notes.txt"), []byte(body), 0o644))

		fake := &scriptedProviderClient{replies: []core.ChatResponse{
			{
				ToolCalls: []core.ToolCall{{
					ID: "call-wire-1", Name: "read_file", Arguments: `{"path":"notes.txt"}`,
				}},
				Usage: core.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
			},
			{
				Content: "已经读过 notes.txt 了",
				Usage:   core.Usage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15},
			},
		}}

		rt, err := OpenRuntime(context.Background(), Options{
			WorkDir:                outside.workDir,
			SupportedSchemaVersion: storage.SupportedSchemaVersion,
			Logger:                 outside.logger,
			ProviderClient:         fake,
		})
		require.NoError(t, err, "包外装配必须成功（失败先看日志里的 step 停在哪一步）")
		t.Cleanup(func() { require.NoError(t, rt.Close(), "释放失败不得静默") })

		res, err := rt.RunService().Run(context.Background(), core.RunRequest{
			Agent: "reader", SessionID: "wire-open", Message: "读一下 notes.txt", Channel: "cli", UserID: "tester",
		})
		require.NoError(t, err, "运行期失败体现在状态里；函数级 error 只报调用方问题")
		require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)
		require.Equal(t, "已经读过 notes.txt 了", res.FinalText)
		require.Equal(t, 2, res.Iterations)
		require.Equal(t, 1, res.ToolCalls)
		require.NotEmpty(t, res.ConfigHash, "配置哈希由运行时从快照填充")

		// 闭环的第二跳：上一轮的工具结果**必须**进本轮历史（提示词每轮重新组装）。
		sent := fake.requests()
		require.Len(t, sent, 2)
		last := sent[1].Messages[len(sent[1].Messages)-1]
		require.Equal(t, core.RoleTool, last.Role)
		require.Contains(t, last.Content, body, "工具结果回填给模型：文件正文原样进第二跳")

		// 用量读数来自**审计行**：替身经路由 + 两阶段审计，"资源用量"因此不是另一套统计。
		calls, err := rt.LLMCalls().CallsByRun(context.Background(), res.RunID)
		require.NoError(t, err)
		require.Len(t, calls, 2, "两轮模型调用各一行——替身若绕过审计链，这里是零行")
		require.Equal(t, []core.LLMCallView{
			{Round: 1, Status: core.LLMCallSuccess, PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
			{Round: 2, Status: core.LLMCallSuccess, PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15},
		}, calls, "轮号与三 token 列来自替身脚本：审计链把响应里的用量原样落库")

		view, found, err := rt.Invocations().Lookup(context.Background(), res.RunID, "call-wire-1")
		require.NoError(t, err)
		require.True(t, found, "工具调用先预写审计行——执行过就必须留行")
		require.Equal(t, core.LLMCallSuccess, view.Status)
		require.Contains(t, string(view.ResultJSON), body)

		sessions, err := rt.Sessions().ListSessions(context.Background())
		require.NoError(t, err)
		require.Len(t, sessions, 1)
		require.Equal(t, "wire-open", sessions[0].ID)
	})

	t.Run("包外装配的入参缺一即拒", func(t *testing.T) {
		_, err := OpenRuntime(context.Background(), Options{SupportedSchemaVersion: storage.SupportedSchemaVersion})
		require.Error(t, err, "工作目录空值会让工作区落到相对路径——必须拒绝")

		_, err = OpenRuntime(context.Background(), Options{WorkDir: t.TempDir()})
		require.Error(t, err, "零值 schema 版本建不出带版本行的库——必须拒绝")
	})
}

// TestWire_LightCommandTakesNoAssembly 是 T4：只读命令在**实例锁被持有**时照常可用，
// 且整条路径不产生数据库写入（库文件从未出现）；同一把锁下重命令被拒且零写入。
func TestWire_LightCommandTakesNoAssembly(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader", "read_file")
	t.Chdir(fx.workDir) // Run 以进程工作目录定位工作区

	lock, err := workspace.AcquireInstanceLock(fx.root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lock.Release()) })

	require.Equal(t, 0, Run(context.Background(), []string{"profile", "list"}),
		"轻命令不取锁：锁被持有时必须照常可用（口径 23）")
	require.NoFileExists(t, fx.dbPath(), "轻命令零装配：不得开数据库写入")

	// session list 是轻命令里**需要数据**的那一条：只读打开一次。库不存在即如实报错
	// （ErrDatabaseMissing），而**不是**顺手建一个空库——建库属重路径与 init 的动作。
	require.Equal(t, 1, Run(context.Background(), []string{"session", "list"}),
		"库不存在时只读路径如实报错")
	require.NoFileExists(t, fx.dbPath(), "只读路径不建库：连一次写打开都没有")

	// 已初始化的实例（库在位）：持锁时照样能读——这正是"只读命令不取锁"的意义。
	created, err := storage.OpenWritable(context.Background(), fx.dbPath(), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	require.NoError(t, created.Close())

	require.Equal(t, 0, Run(context.Background(), []string{"session", "list"}),
		"只读命令在别的实例持锁时必须能读（口径 23 / FR-024）")

	read := openRead(t, fx.dbPath())
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM sessions`), "只读路径一行都不写")
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM runs`))

	require.Equal(t, 1, Run(context.Background(), []string{"chat", "--agent", "reader", "--message", "hi"}),
		"重命令取锁失败即拒绝启动（同实例只允许一个写者）")
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM runs`),
		"取锁失败发生在一切写入之前——半开的实例不如不开")
}

// TestWire_SnapshotLandsBeforeTheRunStarts 是 T5：快照在**运行开始之前**落库——
// 上下文准入拒绝的运行（零模型调用）也留下快照行与账本行，且两者哈希同源。
func TestWire_SnapshotLandsBeforeTheRunStarts(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader", "read_file")

	rt := fx.assemble(t)

	read := openRead(t, fx.dbPath())
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM config_snapshots`),
		"装配本身不冻结任何快照：快照是一次运行的事")

	res, err := rt.runService().Run(context.Background(), core.RunRequest{
		Agent: "reader", SessionID: "wire-t5", Message: "hi", Channel: "cli",
		Budget: core.Budget{ContextTokens: 1},
	})
	require.NoError(t, err, "运行期失败走 Status，不走函数级 error（契约 §1.2）")
	require.Equal(t, core.StatusFailed, res.Status)
	require.Contains(t, res.Reason, "budget.context_tokens",
		"拒绝启动要点名该调的旋钮（FR-005）")
	require.Zero(t, res.Iterations)
	require.Zero(t, res.ToolCalls)
	require.NotEmpty(t, res.ConfigHash, "运行结果携带本次快照的版本")

	require.NoError(t, rt.Close())

	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM llm_calls`),
		"零模型调用由库侧判：快照落库先于开跑，准入拒绝时一次调用都没发出")

	require.Equal(t, 1, countRows(t, read, `SELECT COUNT(*) FROM config_snapshots`))
	var agentName, hash, content, manifest string
	var createdAt int64
	require.NoError(t, read.QueryRowContext(context.Background(),
		`SELECT agent_name, hash, content, manifest_json, created_at FROM config_snapshots`).
		Scan(&agentName, &hash, &content, &manifest, &createdAt))
	require.Equal(t, "reader", agentName)
	require.Equal(t, res.ConfigHash, hash, "结果里的配置哈希与快照行同源")
	require.NotEmpty(t, content, "content 存 AGENT.md 全文（人工可读的便捷副本）")
	require.NotEmpty(t, manifest, "冻结清单非空——哈希由它派生")
	require.Positive(t, createdAt, "时间戳存毫秒")

	require.Equal(t, 1, countRows(t, read, `SELECT COUNT(*) FROM runs`))
	var status, reason, configHash string
	var finishedAt int64
	require.NoError(t, read.QueryRowContext(context.Background(),
		`SELECT status, reason, config_hash, finished_at FROM runs`).
		Scan(&status, &reason, &configHash, &finishedAt))
	require.Equal(t, string(core.StatusFailed), status)
	require.NotEmpty(t, reason, "非正常终止的原因必填")
	require.Equal(t, res.ConfigHash, configHash, "账本行与快照行同一版本")
	require.Positive(t, finishedAt, "终态落库")

	// 时间戳是毫秒（判据只在量级上：秒级时间戳会比这个下界小两个数量级）。
	require.Greater(t, createdAt, time.Now().Add(-time.Hour).UnixMilli(), "毫秒时间戳")
}
