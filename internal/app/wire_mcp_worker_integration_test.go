//go:build integration

package app

// 本文件是 US-25C 2026-11-10 复核 **P1（Worker 的 MCP 准备链路）**的生产装配判据：
// 在真实 `newRuntime` 装配下（不是组件夹具）验证三段行为——
//  1. 首次连接：Worker 装配期**不**连接（`sharedInitMCPApplied` 是控制面的写），
//     第一轮同步由 `assetSyncDeps.MCP` 按 applied 指针连接、登记工具；
//  2. 版本更新：发布并应用一个新版本后，下一轮**受控重建**——连接一次、整组换工具；
//  3. 无变化复用：第三轮版本与凭据代次都相符、连接仍 live → 零 Connect / 零 Close；
//  4. 接线守卫：生产 `MCPSupervisorDeps` 字面量必须带 `PrepareVerify`，且准备轮**不得**
//     在外层持 `ResourceGate`——外层再持会与 `commitReplace` 的写锁自锁（`core.ResourceGate`
//     是不可重入的 `sync.RWMutex`，§2.3.8 M3）。逆改点：删掉 `PrepareVerify` 接线 → 首轮
//     失败关闭（dependency_unconfirmed）；保留 wire 闭包外层 `resourceGate.Lock()` →
//     第二轮受控重建永久挂住，本文件的守卫以超时落红。
//
// 装配拓扑说明：生产里控制面与 Worker 分处两台宿主；`workspace.AcquireInstanceLock`
// 一个工作区只容一个实例，所以本文件先起控制面（完成共享初始化写、连接并把 applied
// 写进库），关闭它之后再以**同一工作区 + 同一库**起 Worker——这正是 Worker 依赖的
// "控制面已初始化"前置。
//
// 凭据轮换的覆盖边界：Wire 夹具是纯 env 实例（无主密钥，`TestWire_CredentialComponentsAssembled`
// 钉住了这一点），托管凭据无法在本夹具里造行。换代语义因此由组件面承担——
// `TestMCPPrepare_CredentialRotationRebuilds`（版本号不变、代次变化 → 恰好重建一次）与
// `TestMCPPrepareVerify_BranchTable`（代次不符 / 同版本撤销 → superseded）。
//
// Production-assembly judges for the worker MCP prepare chain: first connect, controlled
// rebuild on a version update, zero-side-effect reuse on an unchanged round, and the
// wiring guards for PrepareVerify and the outer ResourceGate.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/tool"
)

const (
	// wireWorkerMCPServer 是夹具 server 名（文件条目 / 指针行 / 定义条目名逐字一致）。
	wireWorkerMCPServer = "alpha-mcp"
	// wireWorkerMCPDefV2 是第二个版本：新增一个参数，用来证明**新配置**真的换进了连接
	// （连接器收到的 spec 带 `--generation-2`），而不是只换了个版本号。
	wireWorkerMCPDefV2 = "name: alpha-mcp\ntransport: stdio\ncommand: unused-mcp\nargs:\n  - --generation-2\n"
	// wireWorkerMCPToolV1 / V2 是两代连接各自提供的工具名：整组换入的判据是"新的在、
	// 旧的没"——逐条追加会把旧名字留成孤儿。
	wireWorkerMCPToolV1 = "alpha_tool"
	wireWorkerMCPToolV2 = "alpha_v2_tool"
	// mcpRoundGuard 是准备轮的硬超时：锁序回归时轮内挂住不返回，用例必须落红而不是挂死。
	mcpRoundGuard = 30 * time.Second
)

// mcpRoundConnector 是按请求 spec 现造连接的替身：每次 `Connect` 记下 spec 并按参数区分
// 两代工具集——同一个替身实例同时服务控制面与 Worker，计数即"真实建连次数"。
// mcpRoundConnector synthesizes connections from each requested spec and counts connects
// and closes; the tool set differs by generation so a whole-set swap is observable.
type mcpRoundConnector struct {
	mu      sync.Mutex
	specs   []core.MCPServerConfig
	closes  int
	toolFor func(core.MCPServerConfig) []core.Tool
}

func (c *mcpRoundConnector) Connect(_ context.Context, specs []core.MCPServerConfig) ([]tool.MCPConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.specs = append(c.specs, specs...)
	conns := make([]tool.MCPConnection, 0, len(specs))
	for _, spec := range specs {
		conns = append(conns, tool.MCPConnection{Server: spec.Name, Tools: c.toolFor(spec)})
	}
	return conns, nil
}

func (c *mcpRoundConnector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closes++
	return nil
}

// connectCount / closeCount 供断言读取（轮次是同步的，加锁只为与 Connect 的实现一致）。
func (c *mcpRoundConnector) connectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.specs)
}

func (c *mcpRoundConnector) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

func (c *mcpRoundConnector) specAt(t *testing.T, index int) core.MCPServerConfig {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Greater(t, len(c.specs), index, "第 %d 次连接尚未发生", index+1)
	return c.specs[index]
}

// 编译期断言：替身与装配层接口同形。
var _ mcpConnector = (*mcpRoundConnector)(nil)

// mcpToolForGeneration 按 spec 里的参数选代工具集：V2 定义带 `--generation-2`。
func mcpToolForGeneration(spec core.MCPServerConfig) []core.Tool {
	if len(spec.Args) > 0 && spec.Args[0] == "--generation-2" {
		return []core.Tool{&fakeMCPTool{name: wireWorkerMCPToolV2, content: "v2"}}
	}
	return []core.Tool{&fakeMCPTool{name: wireWorkerMCPToolV1, content: "v1"}}
}

// seedPublishedMCPVersions 在装配之前按**生产发布路径**（管理操作行 → 版本行 → 指针）
// 为一个 MCP server 顺序发布若干定义原文。`applied[i]` 决定该版本是否同时推进 applied：
// 运行期更新用 true（等价于一次已完成的 `mcp_server.publish` + `mcp_server.apply`）。
// seedPublishedMCPVersions publishes each definition through the production store calls,
// moving the applied pointer only when asked, and returns the version rows in order.
func seedPublishedMCPVersions(t *testing.T, dsn, name string, definitions []string, applied []bool) []core.ResourceVersion {
	t.Helper()
	require.Len(t, applied, len(definitions), "每个定义必须显式声明是否推进 applied")

	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Config{DSN: dsn})
	require.NoError(t, err, "种子 MCP 版本必须能写模式打开库")
	defer func() { require.NoError(t, db.Close()) }()

	versions := postgres.NewResourceStore(db)
	operations := postgres.NewManagementStore(db)
	// 首版 UID 可能由生产启动基线铸出：已有指针时必须沿用它的资源身份，否则
	// `Publish` 的归属校验会拒绝（"uid 与当前指针不符"）。
	pointer, err := versions.Pointer(ctx, core.ResourceMCPServer, name)
	var resourceUID core.ResourceUID
	switch {
	case err == nil:
		resourceUID = pointer.ResourceUID
	case errors.Is(err, core.ErrResourceNotFound):
		resourceUID = fixtureResourceUIDFor(core.ResourceMCPServer, name)
	default:
		require.NoError(t, err, "读取 MCP 指针必须成功")
	}

	rows := make([]core.ResourceVersion, 0, len(definitions))
	for i, definition := range definitions {
		now := time.Now()
		operationID := core.NewOperationID(now)
		sum := sha256.Sum256([]byte(definition))
		require.NoError(t, operations.BeginOperation(ctx, core.OperationRecord{
			OperationID:  operationID,
			Action:       core.ActionMCPServerPublish,
			TargetKind:   core.TargetKindMCPServer,
			TargetID:     name,
			ActorSubject: core.ActorSubjectCLIPrefix + "fixture",
			AuthMethod:   core.AuthMethodCLI,
			Source:       "127.0.0.1:0",
			RequestHash:  hex.EncodeToString(sum[:]),
			Status:       core.OperationPending,
			DetailJSON:   []byte("{}"),
			CreatedAt:    now,
		}))

		version, _, err := versions.EnsureVersion(ctx, core.ResourceVersion{
			Kind:           core.ResourceMCPServer,
			ResourceUID:    resourceUID,
			ResourceID:     name,
			ContentHash:    hex.EncodeToString(sum[:]),
			DefinitionYAML: definition,
			OperationID:    operationID,
			CreatedAt:      now.UnixMilli(),
		})
		require.NoError(t, err, "第 %d 个 MCP 版本必须写入", i+1)
		require.NoError(t, versions.Publish(ctx, core.ResourceMCPServer, name,
			version.ID, applied[i], operationID, now))
		require.NoError(t, operations.FinishOperation(ctx, operationID,
			core.OperationSuccess, "ok", "", []byte("{}"), now))
		rows = append(rows, version)
	}
	return rows
}

// runMCPRoundGuarded 在硬超时内跑一轮 `assetSyncDeps.MCP`（生产闭包本体）。
// 超时即判锁序回归：`context` 截止**中断不了互斥锁等待**，轮内自锁只能用外部计时发现。
// runMCPRoundGuarded runs the production MCP closure under a hard deadline so a lock-order
// regression fails the test instead of hanging the suite.
func runMCPRoundGuarded(t *testing.T, deps *AssetSyncDeps) MCPReport {
	t.Helper()
	require.NotNil(t, deps, "Worker 装配必须保留 assetSyncDeps")
	require.NotNil(t, deps.MCP, "Worker 装配必须接上 MCP 准备闭包")

	type outcome struct {
		report MCPReport
		err    error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), mcpRoundGuard)
	defer cancel()
	go func() {
		report, err := deps.MCP(ctx)
		done <- outcome{report: report, err: err}
	}()
	select {
	case got := <-done:
		require.NoError(t, got.err, "MCP 准备轮不得返回错误")
		return got.report
	case <-ctx.Done():
		t.Fatalf("MCP 准备轮超过 %s 未返回：外层持 ResourceGate 会让 commitReplace 自锁（§2.3.8 M3）",
			mcpRoundGuard)
		return MCPReport{}
	}
}

// TestWireWorker_MCPPrepareChain（复核 P1 的生产装配判据）：首次连接 → 版本更新受控重建 →
// 无变化零副作用复用，全程走真实 `newRuntime` 装配；并钉住 `PrepareVerify` 的接线守卫。
// The production assembly must connect on the first sync round, rebuild on a version
// update, and reuse with zero side effects when nothing changed.
func TestWireWorker_MCPPrepareChain(t *testing.T) {
	fx := newWireFixture(t)
	writeMCPServersYAML(t, fx.root, []string{wireWorkerMCPServer})

	// —— 控制面：完成共享初始化写、连接并把 applied 写进库，然后关闭（释放实例锁）。——
	// 首版由生产启动基线按文件条目建出（与真实首装同路径）；控制面启动对账可能因
	// 文件内容与已发布版本不一致再发一版——具体版本号由生产路径分配，判据只认
	// "启动后 applied 已被写上与 published 对齐"，不假设等于任何固定行。
	controlConn := &mcpRoundConnector{toolFor: mcpToolForGeneration}
	controlApp := fx.app()
	controlApp.mcpConnect = controlConn
	control, err := controlApp.newRuntime(context.Background(), roleControl)
	require.NoError(t, err, "控制面装配必须成功")
	require.Equal(t, 1, controlConn.connectCount(), "控制面启动必须连接已发布的 MCP server")
	require.NoError(t, control.Close(), "控制面关闭必须释放实例锁")

	applied, err := postgresOpenPointer(fx.dsn, wireWorkerMCPServer)
	require.NoError(t, err)
	require.NotNil(t, applied.AppliedVersionID, "控制面启动连接后必须把 applied 写进库")
	require.Equal(t, applied.PublishedVersionID, *applied.AppliedVersionID,
		"控制面启动连接成功后必须把 applied 对齐到最新发布版本")

	// —— Worker：装配期零连接；第一轮同步完成首次连接。——
	workerConn := &mcpRoundConnector{toolFor: mcpToolForGeneration}
	workerApp := fx.app()
	workerApp.mcpConnect = workerConn
	worker, err := workerApp.newRuntime(context.Background(), roleWorker)
	require.NoError(t, err, "Worker 装配必须成功（共享初始化写由控制面完成）")
	t.Cleanup(func() { require.NoError(t, worker.Close()) })

	require.NotNil(t, worker.mcpSupervisor.deps.PrepareVerify,
		"生产装配必须接上换入前复核：缺这条接线时 Worker 的 MCP 永远失败关闭（复核 P1 的 1a）")
	require.Zero(t, workerConn.connectCount(),
		"Worker 装配期不得连接（applied 写与启动连接都是控制面的共享初始化）")

	first := runMCPRoundGuarded(t, worker.assetSyncDeps)
	require.Equal(t, []string{wireWorkerMCPServer}, first.Configured)
	require.Equal(t, []string{wireWorkerMCPServer}, first.Connected, "首轮必须连上已 applied 的 server")
	require.Contains(t, first.Tools, wireWorkerMCPToolV1, "首轮的工具名快照必须来自本次连接")
	require.Len(t, first.Results, 1)
	require.Equal(t, string(core.MCPApplyApplied), first.Results[0].Outcome)
	require.False(t, first.Results[0].Reused, "首次连接不是复用")
	require.Equal(t, applied.PublishedVersionID, first.Results[0].AppliedVersionID,
		"首轮必须连接并生效控制面写下的同一已发布版本")
	require.Equal(t, 1, workerConn.connectCount(), "首轮必须且只建连一次")
	require.Equal(t, wireWorkerMCPServer, workerConn.specAt(t, 0).Name)
	_, registered := worker.registry.Resolve(wireWorkerMCPToolV1)
	require.True(t, registered, "连接成功后的工具必须进入注册池")

	// —— 版本更新：发布并应用 v2，下一轮必须受控重建（连一次、整组换工具）。——
	_ = seedPublishedMCPVersions(t, fx.dsn, wireWorkerMCPServer,
		[]string{wireWorkerMCPDefV2}, []bool{true})
	updated, err := postgresOpenPointer(fx.dsn, wireWorkerMCPServer)
	require.NoError(t, err)
	require.NotNil(t, updated.AppliedVersionID)

	second := runMCPRoundGuarded(t, worker.assetSyncDeps)
	require.Equal(t, []string{wireWorkerMCPServer}, second.Connected)
	require.Contains(t, second.Tools, wireWorkerMCPToolV2)
	require.NotContains(t, second.Tools, wireWorkerMCPToolV1)
	require.Len(t, second.Results, 1)
	require.Equal(t, string(core.MCPApplyApplied), second.Results[0].Outcome)
	require.False(t, second.Results[0].Reused, "版本推进必须受控重建，不是复用")
	require.Equal(t, *updated.AppliedVersionID, second.Results[0].AppliedVersionID)
	require.Equal(t, 2, workerConn.connectCount(), "版本更新必须且只建连一次")
	require.Equal(t, []string{"--generation-2"}, workerConn.specAt(t, 1).Args,
		"换入的连接必须带新版本的参数（证明换的是新配置，不只是版本号）")
	_, oldGone := worker.registry.Resolve(wireWorkerMCPToolV1)
	require.False(t, oldGone, "整组换入必须撤下旧代的工具名（孤儿名字会让声明它的 Agent 解析到无连接实例）")
	_, newRegistered := worker.registry.Resolve(wireWorkerMCPToolV2)
	require.True(t, newRegistered)

	// —— 无变化轮：版本与凭据代次都相符、连接仍 live → 零 Connect / 零 Close。——
	closesBefore := workerConn.closeCount()
	third := runMCPRoundGuarded(t, worker.assetSyncDeps)
	require.Len(t, third.Results, 1)
	require.True(t, third.Results[0].Reused, "无变化轮必须零副作用复用")
	require.Equal(t, string(core.MCPApplyApplied), third.Results[0].Outcome)
	require.Equal(t, 2, workerConn.connectCount(), "无变化轮不得再建连")
	require.Equal(t, closesBefore, workerConn.closeCount(), "无变化轮不得关闭连接")
	require.Contains(t, third.Tools, wireWorkerMCPToolV2)
}

// postgresOpenPointer 用一条独立句柄读 MCP 指针行（判"控制面/种子是否把 applied 写进库"）。
// postgresOpenPointer reads the pointer row over a fresh handle.
func postgresOpenPointer(dsn, name string) (core.ResourcePointer, error) {
	ctx := context.Background()
	db, err := postgres.Open(ctx, postgres.Config{DSN: dsn})
	if err != nil {
		return core.ResourcePointer{}, err
	}
	defer func() { _ = db.Close() }()
	return postgres.NewResourceStore(db).Pointer(ctx, core.ResourceMCPServer, name)
}
