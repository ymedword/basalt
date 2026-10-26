package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/workspace"
)

// resetOutboundGuard 在用例结束时卸掉进程级守门器（14D T104）：`Run` 会安装它，而
// 包内测试共用一个进程——后面的用例若还拿受控客户端连本机假服务，会被受保护地址规则
// 拒掉。生产进程只有一个 `Run`，不存在"卸掉"这条路径。
// The guard is process-wide; tests calling Run reset it so later cases keep the
// bare-test-double transport. A production process has exactly one Run.
func resetOutboundGuard(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { tool.SetNetGuard(nil) })
}

// TestRun_ExitCodes 验收点（文档用例名同名）：装配骨架是退出码的唯一出口——
// 成功 0、失败 1，原因由文案区分而不是靠不同退出码堆叠。
func TestRun_ExitCodes(t *testing.T) {
	t.Run("成功路径返回 0", func(t *testing.T) {
		resetOutboundGuard(t)
		require.Equal(t, 0, Run(context.Background(), []string{"--version"}))
	})

	t.Run("失败路径返回 1", func(t *testing.T) {
		resetOutboundGuard(t)
		require.Equal(t, 1, Run(context.Background(), []string{"no-such-command"}))
	})
}

// TestRun_HeavyCommandSplit 追加一条（US-5 / T031）：`tool` 属**重路径**——工具清单的答案
// 来自装配期构建的内存注册池，注册池之外根本不存在（颗粒度文档注 2 的推导链：答案涉及
// 注册表或数据库 ⇒ 走完整装配），因此它与 `chat` 同路；`session` 仍走轻路径的**只读库句柄**
// 支路（不取锁、不迁移、不恢复——FR-024）。
// `tool` joins the heavy path because its answer lives in the assembly-time registry;
// `session` keeps the read-only handle branch.
//
// 判据落在分流函数上（两条清单就是分流的全部依据，`Run` 的分支只是照它走）：清单改了而
// 行为没跟上、或行为改了而清单没跟上，这里都会红。
func TestRun_HeavyCommandSplit(t *testing.T) {
	require.True(t, needsRuntime([]string{"tool", "list"}), "tool 是重命令（与 chat 同路）")
	require.True(t, needsRuntime([]string{"chat", "--message", "hi"}), "chat 仍是重命令")
	require.False(t, needsReadStore([]string{"tool", "list"}),
		"重路径**不再**走只读句柄支路——两条支路互斥，答案只能来自其中一条")

	require.False(t, needsRuntime([]string{"session", "list"}), "session 仍是轻命令（不取锁）")
	require.True(t, needsReadStore([]string{"session", "list"}), "session 需要一条只读库句柄")

	require.True(t, needsRuntime([]string{"serve"}), "serve 是重命令（US-9：全量装配 + HTTP 面）")
	require.True(t, needsRuntime([]string{"gateway"}), "gateway 是重命令（US-9：全量装配 + 常驻）")

	require.False(t, needsRuntime([]string{"--version"}), "全是选项的调用没有命令名")
}

// TestRunWithRuntime_ReleasesAfterExec 对应颗粒度文档 R17（原文保留）：
// 「逆改点：把 defer rt.Close() 留在 runWithRuntime 之外或提前执行 → order 变成
// ["runtime-close", "exec"]，断言红；runWithRuntime 忘记 defer → exec 出错时资源不
// 释放，第二分支红；Close 调用两次 → closeCalls 断言红」。
func TestRunWithRuntime_ReleasesAfterExec(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("正常返回后释放", func(t *testing.T) {
		rt := &closeRecorderRuntime{}
		err := runWithRuntime(rt, logger, func() error {
			rt.record("exec")
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, []string{"exec", "runtime-close"}, rt.order(),
			"共享资源必须在 exec（Serve）返回之后才释放")
		require.Equal(t, 1, rt.closeCallCount(), "Close 必须恰好调用一次")
	})

	t.Run("exec 出错也释放", func(t *testing.T) {
		rt := &closeRecorderRuntime{}
		err := runWithRuntime(rt, logger, func() error {
			rt.record("exec")
			return errors.New("serve failed")
		})
		require.Error(t, err)
		require.Equal(t, []string{"exec", "runtime-close"}, rt.order())
		require.Equal(t, 1, rt.closeCallCount())
	})
}

// closeRecorderRuntime 是 runtimeCloser 的记录型实现。heavyRuntime.Close 内部
// 「MCP → DB → 锁」的顺序由既有实现保证（wire.go），本用例只验证调用时机与单次性。
type closeRecorderRuntime struct {
	mu         sync.Mutex
	events     []string
	closeCalls int
}

func (r *closeRecorderRuntime) record(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *closeRecorderRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "runtime-close")
	r.closeCalls++
	return nil
}

// order / closeCallCount 返回加锁副本（-race 下不得直接读未加锁字段）。
func (r *closeRecorderRuntime) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *closeRecorderRuntime) closeCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closeCalls
}

// 下面七个用例是 US-12 的启动顺序与发布—准入一致性验收面（颗粒度文档 §3.2.2：
// `internal/app/run_test.go` 的启动顺序、发布—准入一致性与 `Runtime` 释放）：
//
//	S1 `TestStartupOrder_PublishedBeforeRegistry`——对账先于运行目录与注册表（约束 18）
//	S2 `TestStartupReconcile_ExternalEditPublishesWithCLIActor`——外部编辑经启动对账发布，
//	    actor / auth_method / source 逐项核对，第三次启动幂等（约束 34）
//	S3 `TestStartupReconcile_InvalidFileKeepsPublished`——非法编辑保留已发布版本（约束 18 / 34）
//	S4 `TestStartupReconcile_RemovalIgnored`——条目被删忽略且保留指针（约束 34）
//	A1 `TestRunAdmission_PinsProviderVersion`——新 Run 的清单固定**当前发布版本**（约束 17）
//	A2 `TestRunAdmission_PublishGateConsistent`——发布写锁与准入读锁共用同一道闸（约束 17）
//	A3 `TestRunRuntime_ReleaseAfterFinish`——`FinishRun` 之后才 `Release`，且恰好一次（约束 17）
//
// 全部走真实重路径装配（实例锁 / SQLite / 基线 / 对账 / 目录 / 注册表），模型调用经
// `scriptedProviderClient` 进程内替身，判据读真实数据库行与真实日志。
// Every case drives the real heavy-path assembly; model calls stay in-process and the
// verdicts read real database rows and real log records.

// writeProbeConfigModel 以生产线格式重写实例配置的 provider 条目（模拟运维手工编辑
// `basalt.yaml`）：名字与顺序不变，只改 `default_model`。
// rewriteProbeConfigModel simulates an operator editing basalt.yaml by hand.
func (f *wireFixture) writeProbeConfigModel(t *testing.T, model string) {
	t.Helper()

	content := "providers:\n" +
		"  - name: probe\n" +
		"    base_url: http://127.0.0.1:1\n" +
		"    api_key: ${BASALT_WIRE_KEY}\n" +
		"    default_model: " + model + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(f.workDir, configFileName), []byte(content), 0o644))
}

// writeInvalidProbeConfig 落一份**结构可解析、条目非法**的定义文件（缺 base_url 的值）：
// 对账必须把它判成"候选非法"并保留旧版本，而不是让启动失败或静默生效。
// writeInvalidProbeConfig writes a parseable file whose entry is invalid.
func (f *wireFixture) writeInvalidProbeConfig(t *testing.T) {
	t.Helper()

	content := "providers:\n  - name: probe\n    base_url: \"\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(f.workDir, configFileName), []byte(content), 0o644))
}

// restartSequence 在同一工作区连续启动重路径：实例锁要求上一轮**先关**，下一轮才起得
// 来；每个运行期恰好关闭一次（显式重启时关旧的，`t.Cleanup` 关最后一个）。装配动作与
// `assemble` 逐句一致，只把关闭时机交给调用方。
// restartSequence starts the heavy path repeatedly in one workspace: the instance lock
// permits one writer, so each runtime is closed exactly once before the next starts.
type restartSequence struct {
	t    *testing.T
	fx   *wireFixture
	last *heavyRuntime
}

func newRestartSequence(t *testing.T, fx *wireFixture) *restartSequence {
	t.Helper()

	seq := &restartSequence{t: t, fx: fx}
	t.Cleanup(seq.closeLast)
	return seq
}

// start 关掉上一轮并按生产入口重新装配。
// start closes the previous runtime and assembles a fresh one.
func (s *restartSequence) start() *heavyRuntime {
	s.t.Helper()

	s.closeLast()
	// 与 `assemble` 同一前置：探针 provider 的已发布版本要先在库里，绑定才有可对齐的身份。
	// Same precondition as `assemble`: the probe's published version must precede the agent.
	s.fx.seedPublishedProviders(s.t)
	rt, err := s.fx.app().newRuntime(context.Background())
	require.NoError(s.t, err, "重路径装配必须成功（失败时先看日志里的 step 停在哪一步）")
	s.fx.enableAgents(s.t, rt)
	s.last = rt
	return rt
}

func (s *restartSequence) closeLast() {
	if s.last == nil {
		return
	}
	rt := s.last
	s.last = nil
	require.NoError(s.t, rt.Close())
}

// publishProbeVersionForTest 直接经**同一条发布事务**写入 probe 的新版本与指针（含预写
// 管理操作行），模拟一次端到端发布在 `ResourceGate` 写锁内的提交段。用例借此制造
// "装配之后发布、运行之前准入"的版本差。
// publishProbeVersionForTest commits one new published version through the real publish
// transaction, so tests can create a version gap between assembly and admission.
func publishProbeVersionForTest(t *testing.T, rt *heavyRuntime, model string) core.ResourceVersion {
	t.Helper()

	return publishModelConnectionForTest(t, rt, "probe", model)
}

// publishModelConnectionForTest 是 `publishProbeVersionForTest` 的按名变体：判"已发布但
// 不在配置文件里"的连接时，目标名就是判据的一部分，不能借用探针名。
// publishModelConnectionForTest is the name-parameterised variant, used when the connection
// under test exists only in the published set.
func publishModelConnectionForTest(t *testing.T, rt *heavyRuntime, name, model string) core.ResourceVersion {
	t.Helper()

	ctx := context.Background()
	now := baselineClock()
	definition := providerDefinitionFor(t, providerEntryText(name, "http://127.0.0.1:1", model))
	contentHash := baselineResourceHash(string(core.ResourceModelConnection), name, definition)
	operationID := core.NewOperationID(now)
	require.NoError(t, rt.ops.BeginOperation(ctx, core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionModelConnectionUpdate,
		TargetKind:   core.TargetKindModelConnection,
		TargetID:     name,
		ActorSubject: "test",
		AuthMethod:   core.AuthMethodLocalTrust,
		Source:       "127.0.0.1:41002",
		RequestHash:  contentHash,
		Status:       core.OperationPending,
		DetailJSON:   []byte("{}"),
		CreatedAt:    now,
	}))
	// 资源身份：已发布的连接沿用当前指针的 UID（更新语义）；只存在于已发布集合的
	// 连接由夹具新分配一个身份（创建语义）。
	uid := core.ResourceUID("")
	if pointer, ptrErr := rt.versions.Pointer(ctx, core.ResourceModelConnection, name); ptrErr == nil {
		uid = pointer.ResourceUID
	} else {
		require.ErrorIs(t, ptrErr, core.ErrResourceNotFound)
		minted, mintErr := core.NewResourceUID()
		require.NoError(t, mintErr)
		uid = minted
	}
	version, err := rt.versions.PublishWithVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    uid,
		ResourceID:     name,
		ContentHash:    contentHash,
		DefinitionYAML: string(definition),
		OperationID:    operationID,
		CreatedAt:      now.UnixMilli(),
	}, operationID, now)
	require.NoError(t, err)
	return version
}

// probePointer 读 probe 的发布指针（多个用例共用同一读数口径）。
// probePointer reads the probe provider's publish pointer.
func probePointer(t *testing.T, rt *heavyRuntime) core.ResourcePointer {
	t.Helper()

	ptr, err := rt.versions.Pointer(context.Background(), core.ResourceModelConnection, "probe")
	require.NoError(t, err)
	return ptr
}

// probeUpdateOperations 取启动对账写下的 `model_connection.update` 操作行。
// probeUpdateOperations lists the operations written by startup reconciliation.
func probeUpdateOperations(t *testing.T, rt *heavyRuntime) []core.OperationRecord {
	t.Helper()

	rows, err := rt.ops.ListOperations(context.Background(), core.OperationFilter{
		Action: core.ActionModelConnectionUpdate,
		Limit:  50,
	})
	require.NoError(t, err)
	return rows
}

// probeVersions 取 probe 的全部资源版本（新到旧）。
// probeVersions returns every version of the probe provider.
func probeVersions(t *testing.T, rt *heavyRuntime) []core.ResourceVersion {
	t.Helper()

	versions, err := rt.versions.Versions(context.Background(), core.ResourceModelConnection, "probe")
	require.NoError(t, err)
	return versions
}

// S1：启动次序固定为"对账 → 运行目录 → 注册表"（约束 18）。最强的判据不是日志次序，
// 而是目录内容：第二次启动前把文件改成新模型，若目录在对账之前构建，装配期读到的仍是
// v1，`models["probe"]` 会是旧值。
// TestStartupOrder_PublishedBeforeRegistry proves the catalog is built after
// reconciliation by reading the catalog contents, not merely the log order.
func TestStartupOrder_PublishedBeforeRegistry(t *testing.T) {
	fx := newWireFixture(t)
	seq := newRestartSequence(t, fx)
	rt := seq.start()
	require.EqualValues(t, 1, probeVersions(t, rt)[0].VersionNo)

	fx.writeProbeConfigModel(t, "probe-model-v2")
	fx.logs.Reset()
	rt2 := seq.start()

	steps := logSteps(t, fx.logs.String())
	require.Equal(t, []string{
		"lock", "open", "migrate", "secrets", "recover", "baseline", "recovery",
		"reconcile", "catalog", "registry",
	}, steps, "对账必须先于运行目录与注册表（约束 18 的次序即正确性）")

	catalog := rt2.catalogStore.Load()
	require.Equal(t, "probe-model-v2", catalog.models["probe"],
		"目录必须由**对账之后**的已发布版本构建：改了文件而目录仍是旧模型 = 对账排在目录之后")
	require.Contains(t, catalog.clients, "probe")
}

// S2：外部编辑经启动对账发布新版本，审计归属 `cli:<本机用户>` / `auth_method=cli` /
// `detail_json.source=startup_reconcile`；第三次启动相同内容幂等（零新版本、零新操作行）。
// TestStartupReconcile_ExternalEditPublishesWithCLIActor covers the publish decision and
// idempotency at the assembly level.
func TestStartupReconcile_ExternalEditPublishesWithCLIActor(t *testing.T) {
	fx := newWireFixture(t)
	seq := newRestartSequence(t, fx)
	rt := seq.start()
	v1 := probePointer(t, rt)

	fx.writeProbeConfigModel(t, "probe-model-v2")
	rt2 := seq.start()

	pointer := probePointer(t, rt2)
	require.NotEqual(t, v1.PublishedVersionID, pointer.PublishedVersionID,
		"文件新且候选合法 → 启动对账必须发布新版本")
	require.NotNil(t, pointer.AppliedVersionID)
	require.Equal(t, pointer.PublishedVersionID, *pointer.AppliedVersionID,
		"Provider 的 applied 必须与 published 同步")

	published, err := rt2.versions.VersionByID(context.Background(), pointer.PublishedVersionID)
	require.NoError(t, err)
	require.EqualValues(t, 2, published.VersionNo)
	require.Contains(t, published.DefinitionYAML, "probe-model-v2",
		"发布内容取文件里的条目原文")

	operations := probeUpdateOperations(t, rt2)
	require.Len(t, operations, 1, "一次对账发布写一条 update 操作行")
	operation := operations[0]
	require.Equal(t, core.TargetKindModelConnection, operation.TargetKind)
	require.Equal(t, "probe", operation.TargetID)
	require.True(t, strings.HasPrefix(operation.ActorSubject, core.ActorSubjectCLIPrefix),
		"actor 必须是 cli:<本机用户>，实际 %q", operation.ActorSubject)
	require.Equal(t, core.AuthMethodCLI, operation.AuthMethod)
	require.Equal(t, core.OperationSuccess, operation.Status)
	require.JSONEq(t, `{"source":"startup_reconcile"}`, string(operation.DetailJSON))

	// 第三次启动：文件内容与已发布版本一致 → 幂等（注 7）。
	fx.logs.Reset()
	rt3 := seq.start()

	reconcile := findStep(t, fx.logs.String(), "reconcile")
	require.EqualValues(t, 0, reconcile["published"], "相同内容不得再发布")
	require.EqualValues(t, 1, reconcile["unchanged"])
	require.Len(t, probeVersions(t, rt3), 2, "版本只追加，幂等启动不再新增")
	require.Len(t, probeUpdateOperations(t, rt3), 1, "幂等启动不写新操作行")
	require.Equal(t, pointer.PublishedVersionID, probePointer(t, rt3).PublishedVersionID)
}

// S3：文件新但候选非法 → 启动继续、保留已发布版本、零写入、WARN 点名（约束 18 / 34）。
// TestStartupReconcile_InvalidFileKeepsPublished is the card's review point for a
// hand-edited definition that does not pass candidate validation.
func TestStartupReconcile_InvalidFileKeepsPublished(t *testing.T) {
	fx := newWireFixture(t)
	seq := newRestartSequence(t, fx)
	rt := seq.start()
	v1 := probePointer(t, rt)

	fx.writeInvalidProbeConfig(t)
	fx.logs.Reset()
	rt2 := seq.start()

	require.Equal(t, v1.PublishedVersionID, probePointer(t, rt2).PublishedVersionID,
		"候选非法必须保留已发布版本：手工改坏文件不等于撤回")
	require.Len(t, probeVersions(t, rt2), 1)
	require.Empty(t, probeUpdateOperations(t, rt2), "保留决策零写入")

	reconcile := findStep(t, fx.logs.String(), "reconcile")
	require.EqualValues(t, 1, reconcile["retained"])
	require.EqualValues(t, 0, reconcile["published"])
	require.Contains(t, fx.logs.String(), "启动对账：候选定义不可用，保留已发布版本")
	require.Contains(t, fx.logs.String(), `"resource":"probe"`)
}

// S4：定义文件里条目被删 → 忽略删除并保留指针（删除只能经 DELETE API），启动继续。
// 内存配置同步清空：基线的 `checkDefinitionNames` 要求文件条目与配置名集**逐位等长**，
// 只删文件会让基线以"拿错定义"拒绝启动——那与本用例要判的删除语义无关。
// TestStartupReconcile_RemovalIgnored covers the removal decision at the assembly level;
// the in-memory config is emptied too because the baseline matches file entries to it.
func TestStartupReconcile_RemovalIgnored(t *testing.T) {
	fx := newWireFixture(t)
	seq := newRestartSequence(t, fx)
	rt := seq.start()
	v1 := probePointer(t, rt)

	fx.cfg = &cli.Config{}
	require.NoError(t, os.WriteFile(filepath.Join(fx.workDir, configFileName),
		[]byte("# 运维手工删除了 providers 段（不被接受为删除）\n"), 0o644))
	fx.logs.Reset()
	rt2 := seq.start()

	require.Equal(t, v1.PublishedVersionID, probePointer(t, rt2).PublishedVersionID,
		"条目被删不是删除动作：指针必须原样保留")
	require.Len(t, probeVersions(t, rt2), 1)
	require.Empty(t, probeUpdateOperations(t, rt2), "忽略决策零写入")

	reconcile := findStep(t, fx.logs.String(), "reconcile")
	require.EqualValues(t, 1, reconcile["ignored"])
	require.Contains(t, fx.logs.String(), "定义文件中已没有该条目")
}

// B1（T032）：注册表只由**已发布** Provider 名集构建（约束 18 / FR17）。差别用例：一个
// 只存在于数据库、不在 basalt.yaml 里的模型连接，配一只声明它的 Agent——装载器若按文件
// 解析结果判成员，这只 Agent 会以 `provider not registered` 整只不注册；按已发布版本判
// 则必须注册。判据同时钉住目录来源：catalogStore 里必须有这个只存在于库里的连接。
// TestAgentLoad_PublishedOnly proves the registry pass judges membership against the
// published provider set, not against the parsed configuration file.
func TestAgentLoad_PublishedOnly(t *testing.T) {
	fx := newWireFixture(t)
	seq := newRestartSequence(t, fx)
	rt := seq.start()

	// 库侧多出一条连接：对账把"文件里没有的已发布条目"保留为忽略决策（S4），于是
	// 重启之后"已发布"严格大于"文件"。
	publishModelConnectionForTest(t, rt, "extra", "extra-model")

	fx.writeAgentWithProvider(t, "extra-runner", "extra", "extra-model")
	rt2 := seq.start()

	require.Contains(t, rt2.catalogStore.Load().clients, "extra",
		"运行目录必须由已发布版本构建——库里的连接要进目录")
	require.Contains(t, rt2.catalogStore.Load().models, "extra")

	entry, ok := rt2.agents.Lookup("extra-runner")
	require.True(t, ok, "Agent 目录里的条目必须进入注册表快照")
	require.True(t, entry.Registered,
		"只引用已发布 Provider 的 Agent 必须注册（reject=%q）", entry.RejectReason)
}

// A1：Run 准入固定的版本写进清单（约束 17）。装配之后、运行之前发布 v2，新 Run 的
// manifest 必须记 v2——它判的是"准入读的是当前发布指针"，不是装配期快照。
// TestRunAdmission_PinsProviderVersion pins the published version at admission time.
func TestRunAdmission_PinsProviderVersion(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	fx.override = &scriptedProviderClient{replies: []core.ChatResponse{{Content: "ok"}}}
	rt := fx.assemble(t)
	v1 := probePointer(t, rt)

	v2 := publishProbeVersionForTest(t, rt, "probe-model-v2")

	res, err := rt.service.Run(context.Background(), core.RunRequest{
		Agent: "runner", SessionID: "pins-provider", Message: "hi", Channel: "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)

	detail, err := rt.runs.GetRun(context.Background(), res.RunID)
	require.NoError(t, err)
	require.NotNil(t, detail.ResourceManifest, "准入清单必须随运行落库")
	require.NotEqual(t, v1.PublishedVersionID, detail.ResourceManifest.Provider.VersionID,
		"装配之后发布的版本必须被新 Run 固定")
	require.Equal(t, v2.ID, detail.ResourceManifest.Provider.VersionID)
	require.EqualValues(t, 2, detail.ResourceManifest.Provider.VersionNo)
	require.Equal(t, v2.ContentHash, detail.ResourceManifest.Provider.ContentHash)
}

// A2：发布写锁与准入读锁共用同一道闸（约束 17）：写锁持有期间新 Run 完不成准入；
// 锁内提交 v2 后放行，运行清单记 v2——若准入用装配期快照或先于发布读指针，这里会红。
// TestRunAdmission_PublishGateConsistent proves admission cannot observe a half-published
// pointer: it either waits for the write lock or sees the committed v2.
func TestRunAdmission_PublishGateConsistent(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	fx.override = &scriptedProviderClient{replies: []core.ChatResponse{{Content: "ok"}}}
	rt := fx.assemble(t)

	type runOutcome struct {
		res core.RunResult
		err error
	}
	rt.resourceGate.Lock()
	started := make(chan struct{})
	done := make(chan runOutcome, 1)
	go func() {
		close(started)
		res, err := rt.service.Run(context.Background(), core.RunRequest{
			Agent: "runner", SessionID: "publish-gate", Message: "hi", Channel: "cli",
		})
		done <- runOutcome{res: res, err: err}
	}()
	<-started

	select {
	case outcome := <-done:
		rt.resourceGate.Unlock()
		t.Fatalf("发布写锁持有期间新 Run 不得完成准入（err=%v，status=%s）",
			outcome.err, outcome.res.Status)
	case <-time.After(100 * time.Millisecond):
	}

	v2 := publishProbeVersionForTest(t, rt, "probe-model-v2")
	rt.resourceGate.Unlock()

	var outcome runOutcome
	select {
	case outcome = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("释放写锁后准入仍未返回")
	}
	require.NoError(t, outcome.err)
	require.Equal(t, core.StatusCompleted, outcome.res.Status, "reason=%s", outcome.res.Reason)

	detail, err := rt.runs.GetRun(context.Background(), outcome.res.RunID)
	require.NoError(t, err)
	require.NotNil(t, detail.ResourceManifest)
	require.Equal(t, v2.ID, detail.ResourceManifest.Provider.VersionID,
		"锁内提交的版本必须被随后放行的准入读到")
}

// B2（T032）：模型调用按**调用时刻**的目录路由（约束 16 / 场景 2 的"新 Run 立即使用新
// 映射"）。装配期挂的第一个替身代表旧目录；在写锁内原子换表之后再开 Run，只有新替身
// 收到请求——路由若停在装配期快照，旧替身会先收到调用。
// TestRunAdmission_RoutingFollowsPublishedCatalog proves a publish-then-run sequence
// reaches the newly published client, not the assembly-time snapshot.
func TestRunAdmission_RoutingFollowsPublishedCatalog(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	assemblyClient := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "stale"}}}
	fx.override = assemblyClient
	rt := fx.assemble(t)

	publishedClient := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "fresh"}}}
	rt.resourceGate.Lock()
	rt.catalogStore.Store(rt.catalogStore.Load().with("probe", publishedClient, "probe-model-v2"))
	rt.resourceGate.Unlock()

	res, err := rt.service.Run(context.Background(), core.RunRequest{
		Agent: "runner", SessionID: "routing-follows-catalog", Message: "hi", Channel: "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)

	require.NotEmpty(t, publishedClient.requests(), "换表之后的新 Run 必须路由到新客户端")
	require.Empty(t, assemblyClient.requests(), "装配期快照不得再被路由命中")
}

// runEventRecorder 记录"终态落库"与"运行时释放"两类事件及其相对次序。
// runEventRecorder records finish and release events with their relative order.
type runEventRecorder struct {
	mu       sync.Mutex
	events   []string
	releases int
	runtime  *recordingRunRuntime
}

func (r *runEventRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	if event == "runtime-release" {
		r.releases++
	}
}

func (r *runEventRecorder) setRuntime(rt *recordingRunRuntime) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runtime = rt
}

func (r *runEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *runEventRecorder) releaseCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releases
}

func (r *runEventRecorder) lastRuntime() *recordingRunRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runtime
}

// recordingRunResources / recordingRunRuntime / recordingRunStore 是判卷包装：只记录
// `Resolve` 返回的运行时与 `FinishRun` 的调用，不改变任何生产行为。
// recordingRunResources wraps the admission port without changing production behaviour.
type recordingRunResources struct {
	core.RunResources
	rec *runEventRecorder
}

func (r *recordingRunResources) Resolve(ctx context.Context, entry core.AgentEntry) (core.ResolvedResources, error) {
	resolved, err := r.RunResources.Resolve(ctx, entry)
	if err != nil {
		return resolved, err
	}
	pinned := &recordingRunRuntime{RunRuntime: resolved.Runtime, rec: r.rec}
	resolved.Runtime = pinned
	r.rec.setRuntime(pinned)
	return resolved, nil
}

type recordingRunRuntime struct {
	core.RunRuntime
	rec *runEventRecorder
}

func (r *recordingRunRuntime) Release() {
	r.rec.record("runtime-release")
	r.RunRuntime.Release()
}

type recordingRunStore struct {
	core.RunStore
	rec *runEventRecorder
}

func (s *recordingRunStore) FinishRun(ctx context.Context, runID string, status core.RunStatus,
	reason string, iterations, toolCalls int, now time.Time,
) error {
	err := s.RunStore.FinishRun(ctx, runID, status, reason, iterations, toolCalls, now)
	s.rec.record("finish")
	return err
}

// A3：`FinishRun` 之后才 `Release`，且恰好一次（约束 17 / 46）。判据是事件次序：
// 释放先于终态落库会让"在途 Run 仍持有固定视图"失效；不释放会让引用回收永远等不到。
// TestRunRuntime_ReleaseAfterFinish re-assembles a production run entry around the very
// same stores and asserts the release happens once, after the terminal row is written.
func TestRunRuntime_ReleaseAfterFinish(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	fx.override = &scriptedProviderClient{replies: []core.ChatResponse{{Content: "done"}}}
	rt := fx.assemble(t)

	rec := &runEventRecorder{}
	catalog := rt.catalogStore.Load()
	client := modelClient(catalog, rt.llmCalls, fx.logger)
	resources := &recordingRunResources{
		RunResources: NewRunResources(rt.versions, catalog, client, rt.mcpRT, rt.skills, rt.resourceGate,
			AdmissionDeps{
				Identities:  rt.versions,
				NotifyAdmin: storage.NewNotifyAdminStore(rt.db),
				Channels:    storage.NewNotifyChannelRepo(rt.db),
				Bindings:    storage.NewBindingStore(rt.db),
			}),
		rec: rec,
	}
	loop := core.NewReActLoop(
		core.NewPromptBuilder(rt.registry),
		core.NewToolExecutor(rt.registry, rt.invocations, fx.logger),
		fx.logger,
	)
	service := core.NewAgentService(core.ServiceOptions{
		Agents:       rt.agents,
		Client:       client,
		Messages:     rt.sessions,
		Runs:         &recordingRunStore{RunStore: rt.runs, rec: rec},
		Snapshots:    storage.NewSnapshotStore(rt.db),
		Audit:        rt.invocations,
		Memory:       rt.mem,
		Loop:         loop,
		Logger:       fx.logger,
		Now:          time.Now,
		Sessions:     rt.sessions,
		Lifecycle:    rt.lifecycle,
		Gate:         rt.agentGate,
		Resources:    resources,
		RunSnapshots: newRunSnapshotBuilder(workspace.NewContextLoader(fx.logger), fx.root, time.Now),
	})

	res, err := service.Run(context.Background(), core.RunRequest{
		Agent: "runner", SessionID: "release-after-finish", Message: "hi", Channel: "cli",
	})
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)

	require.Equal(t, []string{"finish", "runtime-release"}, rec.snapshot(),
		"次序固定：终态落库之后才释放运行时")
	require.Equal(t, 1, rec.releaseCount(), "Release 恰好调用一次")

	pinned := rec.lastRuntime()
	require.NotNil(t, pinned)
	_, ok := pinned.Provider("probe")
	require.False(t, ok, "释放之后不得再发放固定客户端")
}

// seedAdmissionProvider 在准入夹具里发布一张模型连接：绑定校验与凭据固定都要求
// "指针在、已发布、有资格"，缺一项失败关闭的分类就变了。
// seedAdmissionProvider publishes one model connection for the admission tests.
func seedAdmissionProvider(t *testing.T, env *runResourcesEnv, name string) core.ResourceVersion {
	t.Helper()
	version := env.seedVersion(t, core.ResourceModelConnection, name,
		providerDefinition(name, "https://api.example.com", "${BASALT_"+strings.ToUpper(name)+"_KEY}", name+"-model"), "", "")
	env.publish(t, core.ResourceModelConnection, name, version.ID, true)
	return version
}

// seedNotifyChannel 经管理写入路径落一条共享渠道（身份由 core 生成），返回其 UID。
// seedNotifyChannel creates one shared channel through the admin write path.
func seedNotifyChannel(t *testing.T, env *runResourcesEnv, name string) core.ResourceUID {
	t.Helper()
	uid, err := core.NewResourceUID()
	require.NoError(t, err)
	require.NoError(t, storage.NewNotifyAdminStore(env.db).Create(context.Background(), core.NotifyChannel{
		ResourceUID: uid,
		Name:        name,
		Type:        "webhook",
		URL:         "https://example.com/hook",
		URLHost:     "example.com",
		Scope:       core.ScopeShared,
		OperationID: env.opID,
	}))
	return uid
}

// TestRun_AdmissionRejectsUnbound 覆盖"AGENT.md 声明了 provider 却没有 resource_bindings"：
// 准入失败关闭、不按名补绑，错误同时保留可映射的哨兵（B6 据此回 409
// AGENT_RESOURCE_BINDING_MISSING），且不得发放运行时。
// 逆改点：缺绑定按名称临时解析 / 失败仍返回已固定的运行时 → 断言红。
func TestRun_AdmissionRejectsUnbound(t *testing.T) {
	env := newRunResourcesEnv(t)
	seedAdmissionProvider(t, env, "alpha")

	entry := core.AgentEntry{Name: "writer", Registered: true, Profile: &core.Profile{
		Name:     "writer",
		Provider: core.ProviderRef{Name: "alpha", Model: "alpha-model"},
	}}
	resolved, err := env.resources.Resolve(context.Background(), entry)
	require.Error(t, err, "没有绑定的引用必须拒绝准入")
	require.Nil(t, resolved.Runtime, "准入失败不得把运行时交给调用方")
	require.ErrorIs(t, err, core.ErrAgentResourceBindingMissing)
	requireUnavailable(t, err, resourceReasonBinding)
}

// TestRun_FrozenNotifyUIDsUnaffectedByMidRunChange 覆盖通知授权冻结：`all_eligible` 在
// 准入时枚举成具体 UID；运行中新增渠道不改变已冻结集合，只影响之后的准入。
// 逆改点：清单存通配或名称、发送时重读目录 → 两次断言各自红。
func TestRun_FrozenNotifyUIDsUnaffectedByMidRunChange(t *testing.T) {
	env := newRunResourcesEnv(t)
	ctx := context.Background()
	seedAdmissionProvider(t, env, "alpha")
	opsUID := seedNotifyChannel(t, env, "ops")
	sreUID := seedNotifyChannel(t, env, "sre")

	entry := agentEntry("alpha", nil, nil)
	entry.Profile.NotifyMode = core.NotifyModeAllEligible

	first, err := env.resources.Resolve(ctx, entry)
	require.NoError(t, err)
	t.Cleanup(func() { first.Runtime.Release() })
	frozen, err := FrozenChannelUIDs(first.Notify)
	require.NoError(t, err)
	require.ElementsMatch(t, []core.ResourceUID{opsUID, sreUID}, frozen,
		"all_eligible 必须冻结成具体渠道 UID，不得留通配")

	// 运行还在：新增一条共享渠道。
	lateUID := seedNotifyChannel(t, env, "late")
	after, err := FrozenChannelUIDs(first.Notify)
	require.NoError(t, err)
	require.Equal(t, frozen, after, "已冻结集合不得随目录变化")

	second, err := env.resources.Resolve(ctx, entry)
	require.NoError(t, err)
	t.Cleanup(func() { second.Runtime.Release() })
	next, err := FrozenChannelUIDs(second.Notify)
	require.NoError(t, err)
	require.ElementsMatch(t, []core.ResourceUID{opsUID, sreUID, lateUID}, next,
		"之后的准入才看得到新渠道")
}
