package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
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

	// agents 按写入次序记下夹具落过 AGENT.md 的名字：装配之后要经真实迁移入口把它们
	// 从基线态启用（见 `enableAgents`）。
	// agents records the written agent names so assembly can enable them afterwards.
	agents []string

	// override 是可选的进程内模型替身（生产路径恒 nil）：需要判"实际调用了哪个
	// Provider"的用例在这里挂脚本替身，装配时原样交给 `App.providerOverride`。
	// override is the optional in-process model double handed to the assembly.
	override core.ProviderClient
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
	f := &wireFixture{
		workDir: workDir,
		root:    root,
		cfg: &cli.Config{
			Providers: []cli.ProviderConfig{{
				Name:         "probe",
				BaseURL:      "http://127.0.0.1:1",
				APIKey:       "placeholder-unused-3c71",
				DefaultModel: "probe-model",
			}},
			// US-12 起 MCP 连接在**启动时**按传输过策略（约束 19）：stdio 走命令授权。
			// 夹具的 MCP server 以 `unused-mcp` 为命令，白名单必须放行它，连接替身才会
			// 被调用；策略拒绝的真实路径由 supervisor 自己的用例判。
			// The supervisor authorizes stdio connects as shell commands at startup, so the
			// fixture allowlist must admit the fake command.
			Shell: cli.ShellConfig{AllowedCommands: []string{"unused-mcp"}},
		},
		logs:   logs,
		logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	// 启动基线按**原文**取 Provider 定义块（`definition_yaml` 记的是运维写下的那一段），
	// 所以内存里交给装配面的配置名必须能在磁盘上找到同名块：夹具一律落一份同源的
	// basalt.yaml，名字对不上就由基线拒绝启动——那正是"拿错定义"该有的失败方式。
	// The baseline reads provider blocks from disk, so the fixture persists the very
	// configuration it hands the assembly in memory.
	f.writeInstanceConfig(t)
	return f
}

// dbPath 返回夹具工作区里的库文件路径（判"库已建 / 库未建"共用同一处）。
func (f *wireFixture) dbPath() string {
	return filepath.Join(f.root, databaseFileName)
}

// providerUID 给出夹具配置里某个 Provider 名的**确定性**资源身份（`fixtureResourceUIDFor`，
// 生产路径仍由 `core.NewResourceUID` 生成）。AGENT.md 的 `resource_bindings` 要求 uid 与
// 当前发布指针逐字相等，而清单在库还不存在时就已落盘；同一个派生函数让"先写清单、
// 后种子版本"两个动作对齐。名字不在当前配置里 → `false`：这类 Agent 的绑定由用例按需
// 补，夹具不替它们编造。
// providerUID returns the fixture's deterministic identity for a configured provider; the
// second result is false for names the fixture configuration does not declare.
func (f *wireFixture) providerUID(name string) (core.ResourceUID, bool) {
	for _, entry := range f.cfg.Providers {
		if entry.Name == name {
			return fixtureResourceUIDFor(core.ResourceModelConnection, name), true
		}
	}
	return "", false
}

// seedPublishedProviders 在**装配之前**为配置里**每个** provider 块写下首个已发布版本：
// 版本行、发布指针、身份行与操作终态走的是与基线建首版同一条发布事务，因此基线再读
// 文件会判"内容一致"、不重复建版，版本号仍从 1 起。重复调用幂等（指针已在即跳过）——
// 重启序列的每一轮都会经这里，但只有第一轮真的写。只种 probe 一条会让"两 Provider
// 配置"的用例在种子阶段就与磁盘分叉（名字集对不上直接失败），所以按磁盘块逐条种。
// seedPublishedProviders publishes the first version of every provider block declared in
// the fixture configuration, through the same publish transaction the baseline uses; a
// second call is a no-op per provider.
func (f *wireFixture) seedPublishedProviders(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	expected := make([]string, 0, len(f.cfg.Providers))
	for _, entry := range f.cfg.Providers {
		expected = append(expected, entry.Name)
	}
	definitions, err := loadProviderDefinitions(filepath.Join(f.workDir, configFileName), expected)
	require.NoError(t, err, "种子版取的是配置里每个 provider 块的原文")
	require.Len(t, definitions, len(expected), "配置条目与磁盘块必须逐条对上")

	db, err := storage.OpenWritable(ctx, f.dbPath(), storage.SupportedSchemaVersion)
	require.NoError(t, err, "种子版必须能写模式打开库（缺库时由本步建出）")
	defer func() { require.NoError(t, db.Close()) }()

	versions := storage.NewResourceStore(db)
	operations := storage.NewManagementStore(db)
	for _, definition := range definitions {
		if _, err := versions.Pointer(ctx, core.ResourceModelConnection, definition.Name); err == nil {
			continue
		} else {
			require.ErrorIs(t, err, core.ErrResourceNotFound, "读指针的失败方式只能是「不存在」")
		}

		now := time.Now()
		operationID := core.NewOperationID(now)
		contentHash := baselineResourceHash(string(core.ResourceModelConnection),
			definition.Name, definition.Content)
		require.NoError(t, operations.BeginOperation(ctx, core.OperationRecord{
			OperationID:  operationID,
			Action:       core.ActionModelConnectionCreate,
			TargetKind:   core.TargetKindModelConnection,
			TargetID:     definition.Name,
			ActorSubject: core.ActorSubjectCLIPrefix + "fixture",
			AuthMethod:   core.AuthMethodCLI,
			Source:       "127.0.0.1:0",
			RequestHash:  contentHash,
			Status:       core.OperationPending,
			DetailJSON:   []byte("{}"),
			CreatedAt:    now,
		}))
		_, err = versions.PublishWithVersion(ctx, core.ResourceVersion{
			Kind:           core.ResourceModelConnection,
			ResourceUID:    fixtureResourceUIDFor(core.ResourceModelConnection, definition.Name),
			ResourceID:     definition.Name,
			ContentHash:    contentHash,
			DefinitionYAML: string(definition.Content),
			OperationID:    operationID,
			CreatedAt:      now.UnixMilli(),
		}, operationID, now)
		require.NoError(t, err)
	}
}

// writeAgent 落一份可装载的 AGENT.md：provider 指向探针，tools 取声明值。
// 名字声明与目录名一致是装载规则 1；声明 pool 外的工具名 → 整只不注册（规则 4）。
// writeAgent writes one loadable AGENT.md whose declared provider is the probe.
func (f *wireFixture) writeAgent(t *testing.T, name string, tools ...string) {
	t.Helper()

	f.writeAgentContent(t, name, "probe", "probe-model", tools)
}

// writeAgentWithProvider 与 `writeAgent` 同形，只把 provider 名与模型换成调用方给的值：
// 判"注册表按哪份 Provider 名集装载"的用例需要一只引用**配置文件里没有、只存在于已
// 发布版本**的 Provider 的 Agent。
// writeAgentWithProvider writes an AGENT.md that names an arbitrary provider, so a case
// can point one agent at a published-only connection.
func (f *wireFixture) writeAgentWithProvider(t *testing.T, name, provider, model string) {
	t.Helper()

	f.writeAgentContent(t, name, provider, model, nil)
}

// writeAgentContent 是两份写法的公共实现。
// writeAgentContent is the shared writer behind both helpers.
func (f *wireFixture) writeAgentContent(t *testing.T, name, provider, model string, tools []string) {
	t.Helper()

	dir := filepath.Join(f.root, agentsDirName, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))

	declared := "tools: []\n"
	if len(tools) > 0 {
		declared = "tools:\n"
		for _, toolName := range tools {
			declared += "  - " + toolName + "\n"
		}
	}
	// 声明它引用的那条已发布连接（v3 起准入要求 1:1 绑定，缺绑定即拒绝）。只有配置里
	// 声明过的 provider 才有可对齐的种子身份；其余 provider 名的 Agent 由各自用例按需
	// 补，夹具不替它们编造绑定。
	// Agents pointing at a configured provider declare its seeded binding; others stay bare.
	bindings := ""
	if uid, ok := f.providerUID(provider); ok {
		bindings = bindingsBlock([]core.ResourceBinding{{Name: provider, UID: uid}}, nil)
	}
	content := "---\n" +
		"name: " + name + "\n" +
		"description: 装配探针\n" +
		"provider:\n  name: " + provider + "\n  model: " + model + "\n" +
		declared +
		bindings +
		"---\n\n你是「" + name + "」。\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AGENT.md"), []byte(content), 0o644))
	f.agents = append(f.agents, name)
}

// assemble 走一次重路径装配，登记清理；返回的句柄归用例所有。
// assemble runs one heavy-path assembly and registers its release.
func (f *wireFixture) assemble(t *testing.T) *heavyRuntime {
	t.Helper()

	// 探针 provider 先有一份已发布版本，AGENT.md 的绑定才有可对齐的身份（见
	// `seedPublishedProviders`）；不种子时基线会另发一个身份，绑定必然对不上。
	// The probe must already be published so the fixture's bindings name a real pointer.
	f.seedPublishedProviders(t)
	rt, err := f.app().newRuntime(context.Background())
	require.NoError(t, err, "重路径装配必须成功（失败时先看日志里的 step 停在哪一步）")
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	// 基线把既有 Agent 一律写成 disabled（D-01 A / 注 1）——这是本 US 的**行为变更**。
	// 夹具经真实迁移入口把它们启用，让本 US 之前的用例继续验证各自原本的判据（随已批准
	// 接口的行为适配，不改任何断言）。判"基线写下 disabled 且准入拒绝"的用例不走本入口，
	// 而是直接调 `newRuntime`。
	// The baseline disables every pre-existing agent; the fixture enables them through the
	// real transition entry so earlier cases keep judging what they were written to judge.
	f.enableAgents(t, rt)
	return rt
}

// enableAgents 经**运行期自己**的 Agent 级闸门与状态文件存储把夹具 Agent 迁到 active：
// 走的是生产装配传下来的同一份组合（约束 25），不是另起一套。
// enableAgents walks the fixture's agents to active through the runtime's own gate and
// store — the very instances the production assembly wired.
func (f *wireFixture) enableAgents(t *testing.T, rt *heavyRuntime) {
	t.Helper()

	for _, name := range f.agents {
		release := rt.agentGate.Lock(name)
		err := activateAgent(context.Background(), rt.lifecycle, name)
		release()
		require.NoError(t, err, "启用 Agent %s 失败——基线必须已写下 disabled/1 的状态文件", name)
	}
}

// enableAgentsBesideRuntime 用于只拿得到包外句柄（`OpenRuntime`）的用例：另建一份状态
// 文件存储完成同一迁移。此时没有在途运行，文件是唯一权威；闸门只用于让迁移调用满足
// "调用方持写锁"的前置条件。
// enableAgentsBesideRuntime performs the same transition through a stand-alone store, for
// cases that only hold the exported handle; nothing is in flight at that point.
func (f *wireFixture) enableAgentsBesideRuntime(t *testing.T) {
	t.Helper()

	gate := core.NewAgentGate()
	store := workspace.NewLifecycleStore(f.root, time.Now)
	for _, name := range f.agents {
		release := gate.Lock(name)
		err := activateAgent(context.Background(), store, name)
		release()
		require.NoError(t, err, "启用 Agent %s 失败——基线必须已写下 disabled/1 的状态文件", name)
	}
}

// activateAgent 把状态文件迁到 active：已是 active 时原样返回（重复装配的用例因此
// 可以再跑一次），其余状态按**读到的**状态与代数作前置条件——与生产迁移同一口径。
// activateAgent transitions the state file to active; an already-active agent is a no-op
// so restart cases can enable twice.
func activateAgent(ctx context.Context, store *workspace.LifecycleStore, name string) error {
	record, exists, err := store.Read(ctx, name)
	if err != nil {
		return err
	}
	if exists && record.State == core.AgentActive {
		return nil
	}
	_, err = store.Transition(ctx, name, record.State, record.Generation, core.AgentActive,
		core.NewOperationID(time.Now()))
	return err
}

// app 构造装配面：时钟与工作目录显式传入（时间显式传入是判卷前提）。
func (f *wireFixture) app() *App {
	return &App{
		workDir:          f.workDir,
		supportedVersion: storage.SupportedSchemaVersion,
		logger:           f.logger,
		cfg:              f.cfg,
		providerOverride: f.override,
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

	f.writeInstanceConfigBackend(t, "")
}

// writeInstanceConfigBackend 同上，并可显式写一行 `memory.backend`——**换档只改这一行配置**
// 是 FR-017 的原话，记忆用例因此都经"配置装载"这条真路取档位，而不是在 Go 结构里塞值。
// 空 backend = 不写 memory 段（缺省档由 cli 的取值域判定）。
// The memory cases switch carriers by this very configuration line, loaded from disk.
func (f *wireFixture) writeInstanceConfigBackend(t *testing.T, memoryBackend string) {
	t.Helper()

	t.Setenv("BASALT_WIRE_KEY", "wire-sentinel-not-a-secret")
	content := "providers:\n" +
		"  - name: probe\n" +
		"    base_url: http://127.0.0.1:1\n" +
		"    api_key: ${BASALT_WIRE_KEY}\n" +
		"    default_model: probe-model\n"
	if memoryBackend != "" {
		content += "memory:\n  backend: " + memoryBackend + "\n"
	}
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

// TestWire_HeavyPathStepsAppearInOrder 是 T1：重路径的**九个**次序读数在日志里**按序**
// 可见，且恢复完成的一条携带启动摘要的五个计数；装配失败时句柄不半开（锁被放回）。
// 基线与管理面恢复各自是一条读数（图 A ③④）；US-12 追加对账 → 目录 → 注册表（图 A
// ⑤⑥⑦）——定义对账先于运行目录与注册表，中间被别的步骤插队就是接线错了。
func TestWire_HeavyPathStepsAppearInOrder(t *testing.T) {
	fx := newWireFixture(t)

	rt := fx.assemble(t)

	steps := logSteps(t, fx.logs.String())
	require.Equal(t, []string{
		"lock", "open", "migrate", "recover", "baseline", "recovery",
		"reconcile", "catalog", "registry",
	}, steps,
		"重路径次序固定：取锁 → 写模式打开 → 显式迁移 → 崩溃恢复 → 启动基线 → 管理面恢复 → 定义对账 → 运行目录 → 注册表（次序即正确性）")

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

// TestStartupOrder_HeavyPathStepsAppearInOrder 是颗粒度文档 §4.4 `011-app-baseline` 的
// `TestStartupOrder_*` 族名落点：判据是重路径日志里 `step` 读数的先后顺序——
// 取锁 → 写模式打开 → 显式迁移 → 崩溃恢复 → 启动基线 → 管理面恢复（次序即正确性）。
// 断言体与 US-4 交付的 `TestWire_HeavyPathStepsAppearInOrder` 同源：这里只补一个卡上
// 点名的具名入口，**不**写第二份断言；历史函数名保持可复跑（US-4 的冻结证据不失效）。
// Named entry point for the card's TestStartupOrder_* family; it reuses the US-4
// assertion body instead of duplicating it, so the historical name stays runnable.
func TestStartupOrder_HeavyPathStepsAppearInOrder(t *testing.T) {
	TestWire_HeavyPathStepsAppearInOrder(t)
}

// TestWire_RegistryNamesReachAgentLoader 是 T2：装配期注册池的名字集**流进**装载器——
// 声明池内工具名的 Agent 注册成功，声明池外工具名的 Agent 整只不注册（宪法 IV）。
// 判据落在行为上：同一个运行入口对前者不报"不存在"，对后者报"不存在"。
//
// 本 US 的名字清单**扩至八件**（+ 两件记忆工具）：清单是"池内名"的**行为面**载体，与
// `ports_test.go` 的恰好八件名集（数据面）成对——数据面判"池里有什么"，这里判"池里的
// 名字真能一路走到装载器"。
func TestWire_RegistryNamesReachAgentLoader(t *testing.T) {
	fx := newWireFixture(t)
	// 池内：三件文件工具之一 + US-5 四件 + 两件记忆工具（名字集没喂进装载器时，
	// 本行声明的 Agent 会整只不注册 → 下面的 NoError 断言红）。
	fx.writeAgent(t, "reader", "read_file", "shell", "http_get", "http_post", "save_memory", "recall_memory")
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
		// 直接调包外入口也要先种发布版本：启用时准入要求绑定指向真实指针。
		// The exported entry needs the seed before admission can accept the binding.
		outside.seedPublishedProviders(t)

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
		// 基线把既有 Agent 写成 disabled（D-01 A）——启用后本子例继续判它原本要判的闭环。
		outside.enableAgentsBesideRuntime(t)

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

// —— US-6 追加三条（段 2 端到端 / Demo 二闭环 / sqlite 档载体隔离）+ 池名集扩至八件 ——
// Three added by US-6. All of them assemble through the exported `OpenRuntime`, which loads
// the configuration from disk — "switch the carrier by one configuration line" is therefore
// exercised as a real configuration load rather than poked into an in-memory struct.

// memoryEntry 是本节三条用例共用的那条核心记忆正文：判据只关心"这段文本是否出现在模型
// 入站请求里"，故取可辨认的中文短语；它同时被写进工具参数（JSON 字符串），不做任何转义。
// The one core-memory body shared by the memory cases; never escaped, so the scripted tool
// call and the assertions compare the very same literal.
const memoryEntry = "偏好：以后回答简洁点，先给结论。"

// openWireRuntime 走**包外装配面**建一次重路径运行期（配置从盘上装载）。
// openWireRuntime assembles one heavy-path runtime through the exported surface.
func openWireRuntime(t *testing.T, fx *wireFixture, client core.ProviderClient) *Runtime {
	t.Helper()

	// 包外入口同样要先有探针的已发布版本：AGENT.md 的绑定指向它（与 `assemble` 同一前置）。
	// The exported entry needs the same pre-seeded published version as `assemble`.
	fx.seedPublishedProviders(t)
	rt, err := OpenRuntime(context.Background(), Options{
		WorkDir:                fx.workDir,
		SupportedSchemaVersion: storage.SupportedSchemaVersion,
		Logger:                 fx.logger,
		ProviderClient:         client,
	})
	require.NoError(t, err, "重路径装配必须成功（失败时先看日志里的 step 停在哪一步）")
	t.Cleanup(func() { require.NoError(t, rt.Close()) })
	// 包外句柄拿不到装配出来的闸门与存储：用一条独立存储在**无在途运行**时完成同一迁移。
	// The exported handle hides the assembly's gate; a stand-alone store performs the same
	// transition while nothing is in flight.
	fx.enableAgentsBesideRuntime(t)
	return rt
}

// systemMessage 取出站请求里的 system 消息正文——段 1 与段 2 合成**一条**且恒在首位
// （`core.roleSystem` 未导出，这里按契约字面量判角色）。
// systemMessage returns the merged system message; segments one and two share one message.
func systemMessage(t *testing.T, req core.ChatRequest) string {
	t.Helper()

	require.NotEmpty(t, req.Messages, "出站请求至少要有段 1 + 段 2 合成的 system 消息")
	first := req.Messages[0]
	require.Equal(t, "system", first.Role, "段 1 与段 2 合成一条 system 消息，排在首位")
	return first.Content
}

// appendCoreEntry 往 markdown 载体的核心区**手工**加一条条目，保留模板原有结构（文件标题
// 在前、两分区标题齐备）——判据输入因此是人手编辑过的载体，不是夹具另造的一份文本。
// appendCoreEntry edits the markdown carrier by hand while keeping the template's shape.
func appendCoreEntry(t *testing.T, path, entry string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	text := string(raw)
	require.Contains(t, text, "## 核心记忆", "工作区模板自带核心区标题")
	updated := strings.Replace(text, "## 核心记忆", "## 核心记忆\n\n- "+entry, 1)
	require.NoError(t, os.WriteFile(path, []byte(updated), 0o644))
}

// saveMemoryThenRestartAndRecall 跑一次 **Demo 二最小闭环**：真装配（含真记忆工具）里让
// 替身模型调 `save_memory` 写一条核心记忆 → 关掉运行期（**进程重启语义**：实例锁与库句柄
// 一起放掉）→ 再装配、开一个新会话再跑一次，返回第二次运行的入站 system 正文。
//
// 两次运行各用各的替身：第二次的入站请求**只可能**来自第二次运行，判据因此不靠调用次序去猜。
// saveMemoryThenRestartAndRecall walks Demo two: a real tool write, a restart, and a second
// run whose inbound system message is handed back.
func saveMemoryThenRestartAndRecall(t *testing.T, fx *wireFixture, backend string) string {
	t.Helper()

	fx.writeInstanceConfigBackend(t, backend)
	fx.writeAgent(t, "reader", "save_memory", "recall_memory")

	// 第一跳：模型要写核心记忆；第二跳：确认（无工具调用 → 运行完成）。
	writer := &scriptedProviderClient{replies: []core.ChatResponse{
		{
			ToolCalls: []core.ToolCall{{
				ID:        "call-mem-write",
				Name:      "save_memory",
				Arguments: `{"scope":"CORE","content":"` + memoryEntry + `"}`,
			}},
			Usage: core.Usage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17},
		},
		{Content: "记住了", Usage: core.Usage{PromptTokens: 21, CompletionTokens: 3, TotalTokens: 24}},
	}}

	first := openWireRuntime(t, fx, writer)
	res, err := first.RunService().Run(context.Background(), core.RunRequest{
		Agent: "reader", SessionID: "wire-t029-a", Message: "以后回答简洁点", Channel: "cli",
	})
	require.NoError(t, err, "声明两件记忆工具的 Agent 必须注册成功——工具没进注册池时整只不注册")
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)
	require.Equal(t, 1, res.ToolCalls, "第一跳必须真的执行了一次 save_memory")
	require.NoError(t, first.Close(), "重启语义：第一次运行的运行期必须真的关掉（锁与库句柄）")

	reader := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "好的"}}}
	second := openWireRuntime(t, fx, reader)
	res, err = second.RunService().Run(context.Background(), core.RunRequest{
		Agent: "reader", SessionID: "wire-t029-b", Message: "继续", Channel: "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)

	sent := reader.requests()
	require.Len(t, sent, 1, "第二次运行只该发一次请求（无工具调用的完成路径）")
	return systemMessage(t, sent[0])
}

// TestWire_MemorySegmentReachesModelRequest 是 T029 第 1 条：段 2 **端到端**进模型入站请求
// 的 system 消息（约束 22）——人手写进全局层载体的核心条目，经"配置装载 → 装配 → 记忆门面
// → 运行入口 → 提示词组装 → 出站请求"整条路出现在 system 里；该 Agent 层为空故走全局层回退
// （FR-004 的装配面读数），而归档区**一个字都不进**（FR-009）。
func TestWire_MemorySegmentReachesModelRequest(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeInstanceConfig(t)
	fx.writeAgent(t, "reader", "read_file")
	appendCoreEntry(t, filepath.Join(fx.root, "memory", "MEMORY.md"), memoryEntry)

	fake := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "好"}}}
	rt := openWireRuntime(t, fx, fake)

	res, err := rt.RunService().Run(context.Background(), core.RunRequest{
		Agent: "reader", SessionID: "wire-t029-inject", Message: "以后回答简洁点", Channel: "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)

	sent := fake.requests()
	require.Len(t, sent, 1, "一次无工具调用的运行只该发一次请求")
	system := systemMessage(t, sent[0])
	require.Contains(t, system, "## 核心记忆", "段 2 连分区标题一并进 system（读的是重建结果）")
	require.Contains(t, system, memoryEntry,
		"生效层（此处 Agent 层为空 → 全局层）的核心条目必须进模型入站 system——"+
			"装配没把记忆门面接进运行入口时，这里逐字缺这条")
	require.NotContains(t, system, "## 归档记忆",
		"段 2 只含核心区：归档区不进系统提示词，只有 recall_memory 取得到")
}

// TestWire_SaveMemoryThenSecondRunRecalls 是 T029 第 2 条：**Demo 二最小闭环**——真装配 +
// 真记忆工具写一条核心记忆，关掉运行期（进程重启语义）后再装配、再跑，第二次运行的段 2
// 含该条目。**两档各跑一次**：换档只改 `memory.backend` 一行配置（FR-017）。
func TestWire_SaveMemoryThenSecondRunRecalls(t *testing.T) {
	for _, backend := range []string{"markdown", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			fx := newWireFixture(t)

			system := saveMemoryThenRestartAndRecall(t, fx, backend)

			require.Contains(t, system, memoryEntry,
				"第一次运行经真工具写下的核心记忆必须进第二次运行的段 2（Demo 二：跨进程重启仍然记得）")
			require.Contains(t, system, "## 核心记忆")
		})
	}
}

// TestWire_SqliteBackendLeavesWorkspaceFileUntouched 是 T029 第 3 条：sqlite 档的**载体
// 隔离**——记忆落库，markdown 载体一个字节都不动、也不被读。两条负向判据把"换档只换读写面"
// 钉在装配面上：
//
//	① 写下的条目出现在第二次运行的段 2 → 写入真的发生了（否则下面的"文件没变"是空判）；
//	② `memory/MEMORY.md` 逐字节不变，且人手写进去的那条**不得**进段 2——sqlite 档不读该文件，
//	   Agent 层也不得顺手在 `memory/agents/` 下落文件。
func TestWire_SqliteBackendLeavesWorkspaceFileUntouched(t *testing.T) {
	fx := newWireFixture(t)

	globalFile := filepath.Join(fx.root, "memory", "MEMORY.md")
	const handwritten = "手写的一条：只在 markdown 档生效。"
	appendCoreEntry(t, globalFile, handwritten)
	before, err := os.ReadFile(globalFile)
	require.NoError(t, err)

	system := saveMemoryThenRestartAndRecall(t, fx, "sqlite")

	require.Contains(t, system, memoryEntry,
		"sqlite 档的写入必须真的落到库里并生效——文件没变只有在写入发生过时才是证据")
	require.NotContains(t, system, handwritten,
		"sqlite 档不得回读 markdown 载体：换档换的是读写面，不是两档并读")

	after, err := os.ReadFile(globalFile)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "sqlite 档一个字节都不写 markdown 载体")

	require.NoFileExists(t, filepath.Join(fx.root, "memory", "agents", "reader", "MEMORY.md"),
		"sqlite 档的 Agent 层写入落库，不得顺手在 memory/agents/ 下落文件")
}

// —— US-11 T020 追加四条：闸门 / 注册表 / 生命周期装配与关闭顺序（模块卡 §3.2.2，
// 图 A ⑦ / 图 F ③ / 约束 15 / 25 / 46）——
// Four cases added by US-11 T020: the shared gates, the single registry read face, the
// baseline's disabled verdict, and the release order of the instance lock.

// TestWire_AgentGateSharedWithLifecycleConsole（图 F ③ / 约束 25 / 46）：启停迁移与运行
// 准入必须共用装配层建出的**同一份** Agent 级闸门。判别方式：测试持有该 Agent 的写锁时，
// 装配出来的管理面 Transition 必须等锁；释放之后才进入文件判定，并以"期望代数不符"的
// 冲突收场、零操作行。
// 人工 review ②（US-12 交接）后 Discard 改为调用方持写锁、状态文件存储不再自行取锁，
// 共享关系改由控制台在写锁内调用 Transition 体现——判据对象换了，判据本身不变。
// 逆改点：web.go 的 wireManagementPorts 给 NewAgentConsole 传 nil Gate（自建私有锁表）
// → Transition 立刻返回，红。
// The console's transition must wait on the very gate the assembly gives admission.
func TestWire_AgentGateSharedWithLifecycleConsole(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader", "read_file")
	rt := fx.assemble(t)

	require.NotNil(t, rt.resourceGate, "发布 / 准入闸门必须由装配层建出（约束 46；T045 的解析将共用它）")
	admin := fx.app().webOptions(rt).AgentAdmin
	require.NotNil(t, admin, "重路径必须装配管理面：启停迁移与内容写都经它（失败关闭）")

	release := rt.agentGate.Lock("reader")

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := admin.Transition(context.Background(), "reader", core.AgentDisabled,
			core.AgentLifecycleTransition{ExpectedState: core.AgentActive, ExpectedGeneration: 1},
			consoleActor())
		done <- err
	}()
	<-started

	select {
	case err := <-done:
		release()
		t.Fatalf("Transition 未等待共享写锁就返回（err=%v）——管理面与准入用的不是同一把锁", err)
	case <-time.After(200 * time.Millisecond):
	}

	release()

	select {
	case err := <-done:
		require.ErrorIs(t, err, core.ErrLifecycleConflict,
			"拿到锁后才进入文件判定：期望代数 1 与已启用（代数 2）不符，冲突且零操作行")
	case <-time.After(5 * time.Second):
		t.Fatal("释放写锁后 Transition 仍未返回：管理面与闸门接错了实例")
	}
}

// TestWire_RegistryIsSingleAdmissionSource（§3.1.20 / FR-22）：运行准入读的是注册表的
// **当前快照**，不是装配时那份一次性副本——移除后立即拒绝，放回后立即放行。
// 逆改点：把 ServiceOptions.Agents 退回装载结果副本 → 移除后仍放行，红。
// Admission reads the registry's current snapshot: removing an agent refuses it at once,
// and putting it back admits it again.
func TestWire_RegistryIsSingleAdmissionSource(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader", "read_file")
	rt := fx.assemble(t)

	ctx := context.Background()
	entry, ok := rt.agents.Lookup("reader")
	require.True(t, ok, "装配之后注册表里有该 Agent")

	rt.agents.Remove("reader")
	_, err := rt.service.Run(ctx, core.RunRequest{
		Agent: "reader", SessionID: "wire-registry-off", Message: "hi", Channel: "cli",
	})
	var notFound *core.AgentNotFoundError
	require.ErrorAs(t, err, &notFound, "注册表移除后，同一运行入口必须立即拒绝")
	require.Equal(t, "reader", notFound.Name)

	rt.agents.Replace("reader", entry)
	_, err = rt.service.Run(ctx, core.RunRequest{
		Agent: "reader", SessionID: "wire-registry-on", Message: "hi", Channel: "cli",
		Budget: core.Budget{ContextTokens: 1},
	})
	require.NoError(t, err, "放回注册表后立即放行——两处读的是同一份快照")
}

// TestWire_BaselineDisablesThenEnableAdmits（D-01 A / 注 1 / 图 F ③）：基线先于开放服务，
// 把既有 Agent 写成 disabled / generation 1；准入读到它即失败关闭（零行运行账），经共享
// 闸门启用后才放行。这是本 US 的**行为变更**：既有 Agent 默认停用，不得静默当作可用。
// 逆改点：基线改写成 active，或准入忽略状态文件 → 第一次 Run 不报不可用，红。
// The baseline writes disabled/generation 1 before serving; admission fails closed until a
// real transition enables the agent.
func TestWire_BaselineDisablesThenEnableAdmits(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader", "read_file")

	// **不走**夹具的自动启用：本用例判的正是基线写下的初始态。探针的已发布版本仍要先
	// 种子（与 `assemble` 同一前置）：AGENT.md 的绑定要指向真实指针，否则启用之后放行的
	// 那次 Run 会停在绑定准入而不是预算准入。
	// The probe's version is still seeded first so the post-enable run reaches budget admission.
	fx.seedPublishedProviders(t)
	rt, err := fx.app().newRuntime(context.Background())
	require.NoError(t, err, "重路径装配必须成功（失败时先看日志里的 step 停在哪一步）")
	t.Cleanup(func() { require.NoError(t, rt.Close()) })

	record, exists, err := rt.lifecycle.Read(context.Background(), "reader")
	require.NoError(t, err, "基线必须为既有 Agent 写下状态文件")
	require.True(t, exists)
	require.Equal(t, core.AgentDisabled, record.State, "既有 Agent 一律基线为 disabled（D-01 A）")
	require.EqualValues(t, 1, record.Generation, "首版状态文件是 generation 1")

	ctx := context.Background()
	_, err = rt.service.Run(ctx, core.RunRequest{
		Agent: "reader", SessionID: "wire-lifecycle-off", Message: "hi", Channel: "cli",
	})
	var unavailable *core.AgentUnavailableError
	require.ErrorAs(t, err, &unavailable, "停用的 Agent 在准入处被拒（失败关闭）")
	require.Equal(t, core.AgentDisabled, unavailable.State)
	require.Equal(t, "reader", unavailable.Name)

	read := openRead(t, fx.dbPath())
	require.Zero(t, countRows(t, read, `SELECT COUNT(*) FROM runs`),
		"准入拒绝发生在任何写入之前：一次运行都不该留下账")

	fx.enableAgents(t, rt)

	res, err := rt.service.Run(ctx, core.RunRequest{
		Agent: "reader", SessionID: "wire-lifecycle-on", Message: "hi", Channel: "cli",
		Budget: core.Budget{ContextTokens: 1},
	})
	require.NoError(t, err, "启用之后同一入口放行")
	require.Equal(t, core.StatusFailed, res.Status, "预算准入拒绝走 Status，不掩盖启用本身")

	enabled, exists, err := rt.lifecycle.Read(ctx, "reader")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentActive, enabled.State)
	require.EqualValues(t, 2, enabled.Generation, "一次成功迁移把代数推进一格")
}

// TestWire_CloseReleasesInstanceLockLast（约束 22 / 图 F）：真实装配的释放次序——MCP 连接池
// 关闭时实例锁**仍被持有**，`Close` 返回之后才可再次取得。库先于锁释放由既有
// `TestWire_CloseClosesMCPBeforeDatabase` 判，两条合起来钉住"调度器 → MCP → 库 → 锁"。
// 逆改点：Close 里把 lock.Release() 提到 mcp.Close() 之前 → 连接器回调能取得锁，红。
// The instance lock outlives every other resource; only after Close returns can a second
// instance acquire it.
func TestWire_CloseReleasesInstanceLockLast(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{"probe"})

	lockHeldDuringMCPClose := false
	// 连接必须成功：supervisor 对"零连接"按连接失败处理并当场收池，回调会落在装配期而
	// 不是停机路径上。给一条真实连接，关闭时机才回到 heavyRuntime.Close。
	// The connect must succeed so Close runs on the shutdown path, not during assembly.
	connector := &fakeMCPConnector{
		conns: []tool.MCPConnection{{Server: "probe", Tools: []core.Tool{&fakeMCPTool{name: "probe_tool"}}}},
		onClose: func() error {
			probe, err := workspace.AcquireInstanceLock(fx.root)
			if err != nil {
				var busy *workspace.LockBusyError
				lockHeldDuringMCPClose = errors.As(err, &busy)
				return nil
			}
			// 锁已被提前释放：立刻归还，让断言以"未持有"落红而不是污染后续判定。
			_ = probe.Release()
			return nil
		},
	}

	app := fx.app()
	app.mcpConnect = connector
	rt, err := app.newRuntime(context.Background())
	require.NoError(t, err)
	require.NoError(t, rt.Close(), "释放失败不得静默")

	require.True(t, connector.closed, "Close 必须先关闭 MCP 池")
	require.True(t, lockHeldDuringMCPClose,
		"实例锁不得先于 MCP 释放：有活句柄时另一个实例不得进得来")

	reacquired, err := workspace.AcquireInstanceLock(fx.root)
	require.NoError(t, err, "Close 返回后实例锁必须已释放")
	require.NoError(t, reacquired.Release())
}

// TestRunService_AssemblyWiresPublishedSnapshots（T056 / 约束 31）：重路径装配把资源解析与
// 发布版本快照构建器**成对**接进唯一运行入口——绑定技能的运行冻结出的清单记逻辑路径
// `.basalt/skills/<name>/SKILL.md`（字节取发布目录），卡片真路径不进清单；四个共享句柄
// （versions / ops / mcpRT / skills）由装配层保留。
// 逆改点：ServiceOptions 漏传 RunSnapshots（装配失败关闭）/ Builder 改读工作副本
// （清单记 `skills/...`）→ 断言红。
// The heavy path wires the resource resolver and the published-version snapshot builder
// together: a skill-bound run freezes the logical manifest path while the bytes come
// from the published directory.
func TestRunService_AssemblyWiresPublishedSnapshots(t *testing.T) {
	fx := newWireFixture(t)

	// 技能工作副本先于装配落盘：基线把它发布成版本 1，准入据此固定发布目录。
	full := filepath.Join(fx.root, "skills", "writer-skill", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full,
		[]byte("---\nname: writer-skill\ndescription: 快照探针\n---\n\n正文\n"), 0o644))

	agentDir := filepath.Join(fx.root, agentsDirName, "writer")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "AGENT.md"),
		[]byte("---\nname: writer\ndescription: 快照探针\n"+
			"provider:\n  name: probe\n  model: probe-model\n"+
			"skills:\n  - writer-skill\ntools: []\n"+
			bindingsBlock([]core.ResourceBinding{{
				Name: "probe", UID: fixtureResourceUIDFor(core.ResourceModelConnection, "probe"),
			}}, nil)+
			"---\n\n你是「writer」。\n"), 0o644))
	fx.agents = append(fx.agents, "writer")

	rt := fx.assemble(t)

	require.NotNil(t, rt.versions, "资源版本仓储必须由装配层保留（准入与基线共用同一实例）")
	require.NotNil(t, rt.ops, "管理操作仓储必须由装配层保留")
	require.NotNil(t, rt.mcpRT, "MCP 运行期句柄必须由装配层保留")
	require.NotNil(t, rt.skills, "技能库句柄必须由装配层保留（基线 / 准入 / 快照共用）")

	res, err := rt.service.Run(context.Background(), core.RunRequest{
		Agent: "writer", SessionID: "wire-t056", Message: "hi", Channel: "cli",
		Budget: core.Budget{ContextTokens: 1},
	})
	require.NoError(t, err, "运行期失败走 Status，不走函数级 error（契约 §1.2）")
	require.Equal(t, core.StatusFailed, res.Status)

	read := openRead(t, fx.dbPath())
	var manifestJSON string
	require.NoError(t, read.QueryRowContext(context.Background(),
		`SELECT manifest_json FROM config_snapshots`).Scan(&manifestJSON))
	require.Contains(t, manifestJSON, `".basalt/skills/writer-skill/SKILL.md"`,
		"清单记逻辑路径，字节来自发布目录")
	require.NotContains(t, manifestJSON, ".basalt/versions/",
		"发布真路径只进卡片行，不进清单")
}

// TestWire_AgentAdminSharesAssemblyInstances（T023 / 约束 25 / 约束 49）：Agent 写面编排
// 与准入、生命周期迁移共用**同一份** Agent 级锁表、状态文件存储与资源仓储——装配层另建
// 一份会让写锁与准入的互斥静默消失。作者提示词的模板骨架必须与创建模板同源，且**不缓存**
// 装配时刻的发布名单（provider / model 都是可见占位符，名单按每次调用渲染）。
// 逆改点：NewAgentAdmin 漏传 Gate（退化为私有锁表）/ Index 换成新实例 / 作者模板里写入
// 装配时的 Provider 名 → 断言红。
// The write-surface orchestrator must share the very gate, lifecycle store and resource
// store admission uses, and its prompt skeleton must not cache an expiring publish list.
func TestWire_AgentAdminSharesAssemblyInstances(t *testing.T) {
	fx := newWireFixture(t)
	rt := fx.assemble(t)

	require.NotNil(t, rt.agentAdmin, "Agent 写面编排必须由装配层构造并保留")
	require.Same(t, rt.agentGate, rt.agentAdmin.gate,
		"写面必须与准入共用同一把 Agent 级锁表（私有锁表会让互斥静默消失）")
	require.Same(t, rt.lifecycle, rt.agentAdmin.lifecycle,
		"状态文件唯一写者必须是同一份存储")
	require.Same(t, rt.ops, rt.agentAdmin.operations,
		"管理操作仓储必须是同一实例")
	require.Same(t, rt.versions, rt.agentAdmin.index,
		"生命周期索引写端口必须是同一资源仓储")

	providers, err := rt.agentAdmin.published(context.Background())
	require.NoError(t, err)
	require.Len(t, providers, 1, "基线把探针 Provider 发布成版本 1，写面快照必须看得到")
	require.Equal(t, "probe", providers[0].Name)

	skills, err := rt.agentAdmin.publishedSkills(context.Background())
	require.NoError(t, err)
	require.Empty(t, skills)

	mcp, err := rt.agentAdmin.appliedMCP(context.Background())
	require.NoError(t, err)
	require.Empty(t, mcp)

	require.Contains(t, rt.agentAdmin.authorTemplate, "请在此填写 provider 名",
		"作者模板骨架与创建模板同源：无已发布名单时写可见占位符")
	require.Contains(t, rt.agentAdmin.authorTemplate, "你是「<name>」",
		"骨架中的名字是中性占位，不含任何真实 Agent 名")
	require.NotContains(t, rt.agentAdmin.authorTemplate, "probe",
		"模板骨架不得缓存装配时刻的 Provider 名（名单按每次调用渲染）")
}

// TestWire_ResourceAdminSharesAssemblyInstances（T027 / 约束 16 / 17）：资源写面必须
// 与准入共用同一道发布闸门、同一份版本 / 管理操作仓储与**同一个**运行期目录持有者——
// 另建一份会让"发布持写锁、准入持读锁"与"新 Run 读新映射"同时静默失效。定义文件路径
// 必须指向实例配置本身（模型连接块就写在 basalt.yaml）。
// 逆改点：NewResourceAdmin 漏传 Gate（退化为私有闸门）/ Store 换成新实例 / Catalog 换
// 成新持有者 / ConfigPath 指向别处 → 断言红。
// The resource write orchestrator must share the very gate, stores and catalog holder
// the assembly built, and must target the instance configuration file.
func TestWire_ResourceAdminSharesAssemblyInstances(t *testing.T) {
	fx := newWireFixture(t)
	rt := fx.assemble(t)

	require.NotNil(t, rt.resourceAdmin, "资源写面编排必须由装配层构造并保留")
	require.NotNil(t, rt.catalogStore, "运行期目录持有者必须由装配层保留")
	require.Same(t, rt.resourceGate, rt.resourceAdmin.gate,
		"发布闸门必须是准入持读锁的同一实例（私有闸门会让互斥静默消失）")
	require.Same(t, rt.versions, rt.resourceAdmin.store,
		"版本仓储必须是基线与准入共用的同一实例")
	require.Same(t, rt.ops, rt.resourceAdmin.operations,
		"管理操作仓储必须是同一实例")
	require.Same(t, rt.catalogStore, rt.resourceAdmin.catalog,
		"运行期目录持有者必须是同一实例（发布替换后新 Run 才读得到）")
	require.Equal(t, filepath.Join(fx.workDir, configFileName), rt.resourceAdmin.configPath,
		"模型连接块写在实例配置里，路径必须指回工作区的 basalt.yaml")

	// 装配期持有者以**已发布**目录初始化：基线把探针 Provider 发布成版本 1，持有者
	// 里必须已经有它，而不是一个等待第一次发布才填充的空表。
	catalog := rt.catalogStore.Load()
	require.NotNil(t, catalog, "持有者不得以空指针启动")
	require.Contains(t, catalog.clients, "probe",
		"已发布 Provider 必须在装配期就进入运行期目录")
	require.Equal(t, "probe-model", catalog.models["probe"])
}

// TestWire_ResourceAdminPortWiring（T043 / T053）：删除 / 引用 / 版本列表 / 版本详情
// 四个端点必须真正接到装配实例上——端口填槽、三项依赖（归档根 / Agent 快照 / 评测替身）
// 齐备，且端口读数与存储侧同一口径。MCP 方法族自 T048 起转调编排层；技能四方法自
// T053 起转调编排层：创建走完整写路径（工作副本 + 首版快照落在同一工作区根），
// 导入同名按 409、更新缺 If-Match 按 412、校验只读返回裁决——四者都不再是桩错误。
// 逆改点：wire 漏传 Root / Agents / ProviderOverride → 依赖断言红；端口不填槽或 Versions
// 不补指针状态 → 读数断言红；端口换回失败关闭桩 → 技能分支红。
// The T043 wiring must fill the resource port, pass the archive root, the registry
// snapshot and the provider override; since T053 the skill family forwards to the
// real orchestration, so creation lands the work copy and version snapshot on disk.
func TestWire_ResourceAdminPortWiring(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader")
	override := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "ok"}}}
	fx.override = override

	rt := fx.assemble(t)
	ctx := context.Background()

	require.Equal(t, fx.root, rt.resourceAdmin.root, "归档扫描根必须是工作区根")
	require.NotNil(t, rt.resourceAdmin.agents, "删除闸门的 Agent 引用必须来自注册表快照")
	entries, err := rt.resourceAdmin.agents()
	require.NoError(t, err)
	require.Equal(t, rt.agents.Entries(), entries, "快照必须与注册表逐条一致")
	require.Len(t, entries, 1)
	require.Same(t, override, rt.resourceAdmin.providerOverride,
		"评测替身必须与启动装配共用同一个值")

	port := rt.writeServices.resourceAdmin
	require.NotNil(t, port, "T043 之后资源写面端口必须已填槽")

	pointer := probePointer(t, rt)
	versions, err := port.Versions(ctx, core.ResourceModelConnection, "probe")
	require.NoError(t, err)
	require.NotEmpty(t, versions)
	require.Equal(t, pointer.PublishedVersionID, versions[0].Version.ID, "最新版本在前")
	require.True(t, versions[0].Published, "发布指针所在版本必须标 published")
	require.True(t, versions[0].Applied, "模型连接发布即生效")
	require.Nil(t, versions[0].RevertedFromVersionID)

	detail, err := port.VersionDetail(ctx, pointer.PublishedVersionID)
	require.NoError(t, err)
	require.True(t, detail.Published)
	require.NotEmpty(t, detail.DefinitionYAML, "模型连接详情必须带已发布块原文")

	refs, err := port.References(ctx, core.ResourceModelConnection, "probe")
	require.NoError(t, err)
	require.Contains(t, refs.Agents, "reader", "引用视图必须点名当前 Agent")

	// MCP 方法族自 T048 起转调编排层：空标识在写前校验阶段就被点名拒绝，证明端口
	// 已接到真实实现（失败关闭桩只会返回固定桩错误）。
	var invalidMCP *core.InvalidInputError
	_, err = port.UpsertMCPServer(ctx, resourceActor(), web.MCPServerWrite{}, "")
	require.ErrorAs(t, err, &invalidMCP)
	var invalidMCPTest *core.InvalidInputError
	_, err = port.TestMCPServer(ctx, resourceActor(), "")
	require.ErrorAs(t, err, &invalidMCPTest)

	// 技能方法族自 T053 起转调编排层：一次完整创建证明工作副本与首版快照目录都接在
	// 同一工作区根上（桩只会返回固定桩错误，不会落盘）。
	detailSkill, err := port.CreateSkill(ctx, resourceActor(), web.SkillCreateWrite{
		Name:        "wire-skill",
		Description: "接线判据",
	})
	require.NoError(t, err)
	require.Equal(t, "wire-skill", detailSkill.Name)
	require.Equal(t, "接线判据", detailSkill.Description)
	require.NotEmpty(t, detailSkill.Files, "创建响应必须带 SKILL.md 文件清单")
	require.Equal(t, len(detailSkill.Files), detailSkill.FileCount, "file_count 取清单长度")
	require.NotEmpty(t, detailSkill.ETag, "创建响应必须带发布版本 ETag")
	workCopy := filepath.Join(fx.root, "skills", "wire-skill", "SKILL.md")
	require.FileExists(t, workCopy, "工作副本必须落在工作区根的 skills/<name>/ 下")
	// 首版快照按身份分段：versions/skills/<name>/<resource_uid>/1/（身份由创建时分配，
	// 这里按"恰好一个"断言形态，不猜具体 UID）。
	snapshots, err := filepath.Glob(filepath.Join(fx.root, "versions", "skills", "wire-skill", "*", "1", "SKILL.md"))
	require.NoError(t, err)
	require.Len(t, snapshots, 1, "首版快照必须落在 versions/skills/<name>/<resource_uid>/1/ 下")

	// 导入同名按 409（与创建同一闸门），更新缺 If-Match 按 412；两者都在预写之前。
	var conflict *core.AdminError
	_, err = port.ImportSkill(ctx, resourceActor(), web.SkillImportWrite{
		Name:  "wire-skill",
		Files: []web.FileDraftWrite{{Path: "SKILL.md", Content: string(detailSkill.Files[0].Content)}},
	})
	require.ErrorAs(t, err, &conflict)
	require.ErrorIs(t, err, core.ErrConflict, "同名导入必须是 409")
	var precondition *core.AdminError
	_, err = port.UpdateSkill(ctx, resourceActor(), "wire-skill", web.SkillUpdateWrite{
		Files: []web.FileDraftWrite{{Path: "SKILL.md", Content: string(detailSkill.Files[0].Content)}},
	}, "")
	require.ErrorAs(t, err, &precondition)
	require.ErrorIs(t, err, core.ErrPreconditionFailed, "更新缺 If-Match 必须是 412")

	// 校验只读：合法候选返回 valid=true，且工作副本字节不变（不落盘、不执行脚本）。
	valid, err := port.ValidateSkill(ctx, "wire-skill", web.SkillUpdateWrite{
		Files: []web.FileDraftWrite{{Path: "SKILL.md", Content: string(detailSkill.Files[0].Content)}},
	})
	require.NoError(t, err)
	require.True(t, valid.Valid)
	require.Empty(t, valid.Problems)
	after, err := os.ReadFile(workCopy)
	require.NoError(t, err)
	require.Equal(t, detailSkill.Files[0].Content, string(after), "校验不得改写工作副本")
}

// TestWire_NotifyAdminPortWiring（T058）：通知渠道写面必须由装配层构造一次并填槽到
// `writeServices.notifyAdmin`——扫描根、类型表与操作台账都取装配实例：类型表与
// `GET /notify-channels/types`、发送面共用**同一有序切片**（约束 38），操作行落在
// 同一张 `management_operations` 表。端口只做形状投影：空地址创建成功且 URL 保持
// 空串；`${VAR}` 地址只以占位回传（解析值只用于白名单预检，不进响应）；更新留空
// 保留原值；删除走同一整词扫描闸门。
// 逆改点：wire 漏传 Root / Types → 依赖断言红；端口不填槽 → nil 断言红；适配器把
// 占位解析成真实地址回传、或丢掉 keepURL 语义 → 读数断言红。
// The channel write surface is assembled once; the scan root, the shared ordered type
// slice and the operation store all come from that assembly. The port forwards the
// three writes and returns the stored placeholder verbatim.
func TestWire_NotifyAdminPortWiring(t *testing.T) {
	t.Setenv("WIRE_HOOK_URL", "https://hooks.example.com/services/x?token=wire-secret")

	fx := newWireFixture(t)
	fx.writeAgent(t, "reader")
	// 出网白名单是写前预检的授权面：解析结果只用于判定，不落库、不进响应。
	fx.cfg.HTTP.AllowedDomains = []string{"hooks.example.com"}

	rt := fx.assemble(t)
	ctx := context.Background()

	require.NotNil(t, rt.notifyAdmin, "T058 之后通知写面必须由装配层构造")
	require.Equal(t, fx.root, rt.notifyAdmin.root, "删除闸门扫描根必须是工作区根")
	require.Equal(t, notifyTypeNames(), rt.notifyAdmin.types,
		"写面类型表必须与 GET /notify-channels/types 同一有序切片")
	require.NotNil(t, rt.notifyAdmin.operations, "操作台账必须是装配实例")

	port := rt.writeServices.notifyAdmin
	require.NotNil(t, port, "T058 之后通知写面端口必须已填槽")

	// 空地址创建 = 暂不可发送：回显是**存储行**（带身份 / 模式 / 范围 / 修订号），URL 空串。
	created, err := port.Create(ctx, resourceActor(), web.NotifyChannelWrite{
		Name: "wire-empty", Type: "webhook", Description: "暂不可发送",
	})
	require.NoError(t, err)
	require.Equal(t, "wire-empty", created.Name)
	require.Equal(t, "webhook", created.Type)
	require.Equal(t, "暂不可发送", created.Description)
	require.Empty(t, created.URL, "空地址是合法状态，回显不得补值")
	require.NotEmpty(t, created.ResourceUID, "回显必须带存储行的资源身份")
	require.Equal(t, core.CredentialModeEnv, created.CredentialMode)
	require.Equal(t, core.ScopeShared, created.Scope)
	require.Equal(t, 1, created.Revision, "新建行的修订号是列默认值 1")
	require.NotEmpty(t, created.OperationID, "回显必须来自存储行（带创建操作号）")
	require.NotEmpty(t, created.ETag, "写后回显必须带条件 ETag")
	// 回显的 ETag 必须与读面**同一份配方**：条件写靠它比对，两处不一致会让写永远 412。
	read, err := rt.channelRead.Channel(ctx, created.Name)
	require.NoError(t, err)
	require.Equal(t, read.ETag, created.ETag, "写后回显与读面必须共用同一份渠道 ETag 配方")

	// 带 `${VAR}` 地址创建：库里只存占位（解析值只用于白名单预检，不进响应）。
	hookURL := "${WIRE_HOOK_URL}"
	placed, err := port.Create(ctx, resourceActor(), web.NotifyChannelWrite{
		Name: "wire-hook", Type: "webhook", URL: &hookURL,
	})
	require.NoError(t, err)
	require.Equal(t, "${WIRE_HOOK_URL}", placed.URL,
		"响应必须回库里原样占位，不得回解析后的地址")

	// 更新留空 = 保留原值：空 url 原样下传，回读投影仍是占位。
	kept, err := port.Update(ctx, resourceActor(), "wire-hook", web.NotifyChannelWrite{
		Type: "webhook", Description: "保留地址",
	})
	require.NoError(t, err)
	require.Equal(t, "${WIRE_HOOK_URL}", kept.URL, "留空更新不得抹掉已登记地址")
	require.Equal(t, "保留地址", kept.Description)

	// 删除走整词扫描闸门（扫描根来自装配）：无引用即成功，并且每次写都留下操作行。
	// 删除必须显式携带与当前指针一致的 resource_uid（契约 §9.5）。
	require.NoError(t, port.Delete(ctx, resourceActor(), "wire-empty", string(created.ResourceUID), false))
	require.NoError(t, port.Delete(ctx, resourceActor(), "wire-hook", string(placed.ResourceUID), false))

	db := openRead(t, fx.dbPath())
	require.Equal(t, 3, countRows(t, db,
		"SELECT COUNT(*) FROM management_operations WHERE target_kind = ? AND target_id = ? AND status = ?",
		core.TargetKindNotifyChannel, "wire-hook", string(core.OperationSuccess)),
		"创建 + 更新 + 删除各留一行成功操作行（与资源写面同一张表）")
	require.Equal(t, 2, countRows(t, db,
		"SELECT COUNT(*) FROM management_operations WHERE target_kind = ? AND target_id = ? AND status = ?",
		core.TargetKindNotifyChannel, "wire-empty", string(core.OperationSuccess)),
		"创建 + 删除各留一行成功操作行")
	// 删除只写墓碑（FR-073）：行保留供历史查询与审计回溯，活动行必须归零。判"删除发生了"
	// 读的是墓碑列而不是行数——行数在墓碑语义下恒为 2，判不出任何东西。
	// Deletion tombstones the row (FR-073): the verdict reads the tombstone column, not
	// the row count, which is constant under tombstone semantics.
	require.Equal(t, 2, countRows(t, db,
		"SELECT COUNT(*) FROM notify_channels WHERE name IN (?, ?) AND deleted_at IS NOT NULL",
		"wire-empty", "wire-hook"), "两次删除都必须真的写下墓碑")
	require.Zero(t, countRows(t, db,
		"SELECT COUNT(*) FROM notify_channels WHERE name IN (?, ?) AND deleted_at IS NULL",
		"wire-empty", "wire-hook"), "删除后不得留下活动行")
}

// TestWire_WorkspaceBrowsePortWiring（T062）：工作区浏览由装配层构造一次并填槽到
// `writeServices.workspace`——浏览根取自装配输入的工作区根（服务自己拼 `agents/`），
// 端口把目录条目与文件预览按契约形状投影：条目逐字段相等、`truncated` 恒 false，
// staging / backup 残骸不可见。浏览端口没有写面，本用例也不经它产生操作行。
// 逆改点：wire 漏传工作区根 → 目录树报错 / 空；端口不填槽 → nil 断言红；端口伪造
// `truncated` 或改写路径 → 读数断言红；残骸被列出 → 目录断言红。
// The browser is assembled once, rooted at the fixture workspace, projected through the
// web port; residue stays invisible and truncated is always false.
func TestWire_WorkspaceBrowsePortWiring(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "reader")
	// 残骸目录：与活动 Agent 同级，只用于判"目录树不列出残骸"。
	// Residue beside the live agent: the tree must not list it.
	require.NoError(t, os.MkdirAll(
		filepath.Join(fx.root, agentsDirName, "reader.staging"), 0o755))

	rt := fx.assemble(t)
	require.NotNil(t, rt.workspaceBrowse, "T062 之后浏览服务必须由装配层构造")
	port := rt.writeServices.workspace
	require.NotNil(t, port, "T062 之后浏览端口必须已填槽")

	ctx := context.Background()
	tree, err := port.Tree(ctx, "")
	require.NoError(t, err)
	require.Equal(t, web.WorkspaceTreeView{{Path: "reader", Type: "dir", Size: 0}}, tree,
		"浏览根必须是 <工作区根>/agents，且残骸目录不可见")

	tree, err = port.Tree(ctx, "reader")
	require.NoError(t, err)
	require.Len(t, tree, 1)
	require.Equal(t, "reader/AGENT.md", tree[0].Path)
	require.Equal(t, "file", tree[0].Type)
	require.Positive(t, tree[0].Size)

	file, err := port.File(ctx, "reader/AGENT.md")
	require.NoError(t, err)
	require.Equal(t, "reader/AGENT.md", file.Path)
	require.Contains(t, file.Content, "你是「reader」")
	require.Equal(t, int64(len(file.Content)), file.Size)
	require.False(t, file.Truncated, "契约固定 truncated=false：超限是错误，不是截断")
}

// TestWire_CredentialComponentsAssembled（T134 / R4.8）：凭据五件必须由装配层一次
// 构造并保留——解析器与迁移 / 写面共用**同一份**凭据仓储与版本仓储，纯 env 实例没有
// 主密钥路径（不得凭空猜一个默认位置），secrets 服务经包外访问器给出同一实例。
// 纯 env 是合法形态：`status` 报未初始化且零计数，`verify` 零检查零失败，`init` 按
// 固定错误拒绝而不是在某处落盘造密钥。
// 逆改点：解析器换新仓储实例 / MasterKeyFile 被填默认值 / 访问器另建服务 → 断言红。
// The five credential components are built once and share the assembly's stores; a
// pure-env instance reports uninitialised and refuses init instead of inventing a path.
func TestWire_CredentialComponentsAssembled(t *testing.T) {
	fx := newWireFixture(t)
	rt := fx.assemble(t)

	require.NotNil(t, rt.resolver, "凭据解析器必须由装配层构造并保留")
	require.NotNil(t, rt.secrets, "secrets 服务必须由装配层构造并保留")
	require.Same(t, rt.credentials, rt.resolver.deps.Credentials,
		"解析器必须与迁移 / 写面共用同一份凭据仓储（另建一份会让撤销状态读不到）")
	require.Same(t, rt.versions, rt.resolver.deps.Resources,
		"解析器必须与版本仓储同源（绑定目标取自当前发布版本）")
	require.Empty(t, rt.resolver.deps.MasterKeyFile,
		"纯 env 实例未配置主密钥：解析器不得自行猜一个默认路径")
	require.Equal(t, fx.root, rt.resolver.deps.WorkspaceRoot,
		"工作区根必须与装配面一致（主密钥不得落在工作区内）")

	runtime := &Runtime{inner: rt}
	require.Same(t, rt.secrets, runtime.Secrets(), "包外访问器必须给出同一个服务实例")

	ctx := context.Background()
	status, err := runtime.Secrets().Status(ctx)
	require.NoError(t, err, "未配置主密钥是合法形态，不是错误")
	require.False(t, status.Initialized)
	require.Empty(t, status.KeyID)
	require.Empty(t, status.Fingerprint)
	require.Zero(t, status.ManagedModelConnections)
	require.Zero(t, status.ManagedNotifyChannels)

	report, err := runtime.Secrets().Verify(ctx)
	require.NoError(t, err)
	require.Zero(t, report.Checked, "没有托管身份就没有可查验的凭据")
	require.Zero(t, report.Failed)

	_, err = runtime.Secrets().Init(ctx)
	require.ErrorIs(t, err, core.ErrCredentialUnavailable,
		"纯 env 实例可以跳过 init：空路径按固定错误拒绝，不在默认位置凭空造密钥")
	require.NoFileExists(t, filepath.Join(fx.workDir, "master.key"), "拒绝路径不得落盘")
}

// TestWire_StartupRunsMigrationOrchestration（T134）：重路径的 ③ 必须走六步迁移编排，
// 而不是直接调 `db.Migrate`。两条可观察判据：
//
//  1. 新库由 `OpenWritable` 直接建到目标版本 → 编排返回零报告，**不得**留下迁移操作行
//     （每次启动都写一行 `system.baseline_v3` 会让操作台账被启动噪声淹没）；
//  2. 骨架缺 `agents/` 时启动在**迁移**这一步失败——编排的入参校验要扫描 Agent 目录
//     （绑定回填要按名单判身份连续性），只调 `db.Migrate` 的实现根本不看工作区。
//
// 逆改点：迁移块换回 `db.Migrate` → 判据 2 的启动会成功（随后由基线报错，错误串不含
// 「扫描 Agent 目录」）→ 断言红；编排提前写操作行 → 判据 1 读数非零 → 断言红。
// The startup step must run the six-step orchestration, not a bare db.Migrate: an
// up-to-date database leaves no operation row, and a skeleton missing agents/ fails at
// the migration step because the orchestration scans the agent directory.
func TestWire_StartupRunsMigrationOrchestration(t *testing.T) {
	t.Run("新库不写迁移操作行", func(t *testing.T) {
		fx := newWireFixture(t)
		rt := fx.assemble(t)

		version, err := rt.db.SchemaVersion(context.Background())
		require.NoError(t, err)
		require.Equal(t, storage.SupportedSchemaVersion, version, "启动后库必须停在目标版本")

		db := openRead(t, fx.dbPath())
		require.Zero(t, countRows(t, db,
			"SELECT COUNT(*) FROM management_operations WHERE action = ?", actionBaselineV3),
			"库已是目标版本：编排返回零报告，不得写迁移操作行")
	})

	t.Run("缺 Agent 目录在迁移步失败", func(t *testing.T) {
		fx := newWireFixture(t)
		require.NoError(t, os.RemoveAll(filepath.Join(fx.root, agentsDirName)))

		_, err := fx.app().newRuntime(context.Background())
		require.Error(t, err)
		require.ErrorContains(t, err, "迁移数据库", "失败必须发生并归因到迁移这一步")
		require.ErrorContains(t, err, "扫描 Agent 目录",
			"只有六步编排会扫描 Agent 目录——错误串据此区分编排与裸 db.Migrate")
	})
}
