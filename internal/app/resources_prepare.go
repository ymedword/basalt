package app

// 本文件是同步阶段的资源准备（US-25C §3.1.4 / §2.3.8 G5）：每轮先把**已发布**的
// 模型连接逐条构建成客户端（失败隔离、逐条点名），再把成功名集与 MCP / 工具名集
// 组装成 `PreparedResources` 只读快照，Entry 构建只读这份快照。
//
// 顺序不可交换：`validateProfileWith` 规则 2 / 4 / 6 判的就是这些名字集；先建 Entry
// 后准备资源会让"本轮还没准备好的资源"被误报成 entry 非法，缺项也就无法点名。
// This file prepares published resources before entry construction: per-item provider
// builds with failure isolation, then one read-only snapshot for entry validation.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// PreparedResources 是 §3.1.4 的判定输入快照。结构体声明在 `internal/workspace`：
// `EntryFromBundle` 是 `*workspace.AgentLoader` 的方法，而 workspace 不得反向依赖
// app（app 已依赖 workspace，反向即循环）。类型别名让 app 侧仍以本名引用同一类型，
// 不产生第二份定义。
// PreparedResources aliases the workspace snapshot so the loader (which app depends
// on) can consume it without a reverse import.
type PreparedResources = workspace.PreparedResources

// CatalogReport 是一轮 Provider 准备的结果：`Providers` 只含**构建成功**的名字
// （升序，交给 `PreparedResources.Providers`），`Failures` 逐条点名失败原因。
// 失败项不加入目录，也不影响其他 Provider——整体失败只发生在列指针本身读不出来时。
// CatalogReport summarises one provider preparation round; failures are named and
// isolated, never aggregated into a round failure.
type CatalogReport struct {
	Providers []string
	Failures  []CatalogFailure
}

// CatalogFailure 是单个 Provider 的准备失败记录（名字 + 可读原因）。
// CatalogFailure names one provider that failed preparation this round.
type CatalogFailure struct {
	Name   string
	Reason string
}

// providerPrepareDeps 是本轮准备的全部输入。`catalog` 是运行期生效目录的原子持有者
// （copy-on-write 换入），`store` 读已发布指针与版本，`credentials` 解析凭据并给出
// pin，`override` 只服务评测替身（非 nil 时不构造真实客户端）。
// providerPrepareDeps carries everything one preparation round needs.
type providerPrepareDeps struct {
	catalog     *providerCatalogStore
	store       providerPrepareStore
	override    core.ProviderClient
	credentials catalogCredentialSource
	logger      *slog.Logger
}

// providerPrepareStore 是本轮准备对存储的**最小读面**：列已发布指针 + 按版本号取版本行。
// 接口声明在消费方（包纪律第 2 条），真实实现是 `*postgres.ResourceStore`，定向用例用
// 最小替身即可覆盖失败隔离与零重建判据。
// providerPrepareStore is the minimal storage face this round needs; the real store
// satisfies it, and the directed tests can use a small double.
type providerPrepareStore interface {
	ListPointers(ctx context.Context, kind core.ResourceKind) ([]core.ResourcePointer, error)
	VersionByID(ctx context.Context, id int64) (core.ResourceVersion, error)
}

// prepareProviderCatalog 逐条构建已发布的模型连接（§2.3.8）：已构建且版本与凭据 pin
// 都相符的条目**零重建**，其余构建并 copy-on-write 换入；单条失败只记 `CatalogFailure`，
// 不影响其他条目。调用方**必须**持 `ResourceGate` 写锁（与发布互斥），准备期间不得
// 有指针替换穿插——判据见 §2.3.8 的锁序说明。
// prepareProviderCatalog rebuilds stale entries only and isolates per-item failures;
// the caller must hold the ResourceGate write lock.
func prepareProviderCatalog(ctx context.Context, deps providerPrepareDeps) (CatalogReport, error) {
	pointers, err := deps.store.ListPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return CatalogReport{}, fmt.Errorf("列出已发布模型连接: %w", err)
	}

	current := deps.catalog.Load()
	report := CatalogReport{Providers: make([]string, 0, len(pointers))}
	for _, pointer := range pointers {
		entry, version, err := publishedProviderEntry(ctx, deps.store, pointer)
		if err != nil {
			report.recordFailure(deps.logger, pointer.ResourceID, err)
			continue
		}
		if deps.override == nil {
			reusable, reuseErr := providerReusable(ctx, current, entry, version, deps.credentials)
			if reuseErr != nil {
				report.recordFailure(deps.logger, entry.Name, reuseErr)
				continue
			}
			if reusable {
				report.Providers = append(report.Providers, entry.Name)
				continue
			}
		}
		client := deps.override
		proof := core.BuiltVersion{}
		if deps.override == nil {
			client, proof, err = buildProviderClient(ctx, pointer, entry, version, deps.credentials)
			if err != nil {
				report.recordFailure(deps.logger, entry.Name, err)
				continue
			}
		}
		current = current.with(entry.Name, client, entry.DefaultModel, proof)
		report.Providers = append(report.Providers, entry.Name)
	}
	deps.catalog.Store(current)
	return report, nil
}

// recordFailure 追加一条具名失败并记 WARN（失败路径日志与报告两者都要）。
// recordFailure appends a named failure and logs it; both are required for triage.
func (r *CatalogReport) recordFailure(logger *slog.Logger, name string, cause error) {
	r.Failures = append(r.Failures, CatalogFailure{Name: name, Reason: cause.Error()})
	if logger != nil {
		logger.Warn("provider prepare failed", "provider", name, "error", cause.Error())
	}
}

// providerReusable 判定当前目录里的同名客户端是否仍然描述**这一版**发布与**这一份**
// 凭据（§2.3.8 P1 的 Provider 版本）：版本 ID、名字与凭据 pin 三项都相符才复用；
// 任一不符（发布换代、凭据轮换）都走受控重建。零值证据（评测替身 / 轻装配）一律
// 不复用——"没有证据"不是"相符"。
// providerReusable reuses a client only when its build proof still describes the
// published version and the very same credential material.
func providerReusable(ctx context.Context, catalog *providerCatalog, entry workspace.ProviderEntry,
	version core.ResourceVersion, credentials catalogCredentialSource,
) (bool, error) {
	if catalog == nil || catalog.built == nil {
		return false, nil
	}
	proof, ok := catalog.built[entry.Name]
	if !ok || proof.IsZero() {
		return false, nil
	}
	if proof.Resource.Name != entry.Name || proof.Resource.VersionID != version.ID {
		return false, nil
	}
	definition := resourceProviderDefinition{
		Name:           entry.Name,
		BaseURL:        entry.BaseURL,
		CredentialMode: entry.CredentialMode,
	}
	pin, err := credentialPinOf(ctx, entry.Name, version.ResourceUID, definition, credentials)
	if err != nil {
		return false, err
	}
	return proof.Credential == pin, nil
}

// —— MCP 准备（§2.3.8 的自由函数面） ——

// mcpPrepareTarget 是一条已发布 MCP server 的准备输入：`Applied` 表示库里已有 applied
// 指针——只有它允许连接；`Published` 是 applied 指向的那一版（配置 + 版本 + 凭据代次）。
// 未 applied 的条目**不得**被本函数隐式 apply（D-12）。
// mcpPrepareTarget is one published server; only applied pointers may be connected.
type mcpPrepareTarget struct {
	Name      string
	Applied   bool
	Published core.PublishedMCPServer
}

// mcpPrepareDeps 是本轮 MCP 准备的全部输入：`Targets` 读已发布指针与 applied 判定，
// `Prepare` 对已 applied 的条目做 P1 复用 / P2–P6 受控重建，`Status` 取 supervisor
// 实时状态（connected / tools 的读数来源）。
// mcpPrepareDeps carries the targets, the prep call and the live status reader.
type mcpPrepareDeps struct {
	Targets func(ctx context.Context) ([]mcpPrepareTarget, error)
	Prepare func(ctx context.Context, published core.PublishedMCPServer) MCPApplyResult
	Status  func(name string) (MCPServerStatus, bool)
}

// mcpPrepareOutcomeNotApplied 是"已发布但没有 applied 指针"的固定结局字面量：它与
// `failed` 分开——没有应用过不是失败，也**不得**被当成成功。
// mcpPrepareOutcomeNotApplied names the published-but-never-applied outcome.
const mcpPrepareOutcomeNotApplied = "not_applied"

// prepareMCPInstances 只对已 applied 的连接做只读凭据解析与准备（§2.3.8）：逐条调用
// `Prepare`，未 applied 的条目如实记 `not_applied`；三态名单分开报告（configured 是
// 全部已发布，connected / tools 取 supervisor 实时快照）。本函数**不**调用
// `publishedMCPServers`——那条路径含 `BackfillEnvSources` 写面，只属控制面。
// prepareMCPInstances prepares applied servers only and reports the three name sets
// separately; it never touches the backfill write path.
// logMCPStartupPrepare 把启动准备的逐条结局写进日志（§2.3.1 启动角色门）：未 applied
// 与准备失败都用 WARN 点名 server，但不回显 command / 凭据（脱敏口径与 ApplyPublished
// 的 `detail` 一致）；复用与重建只进汇总 INFO，避免每轮刷屏。
// Startup prepare outcomes are logged per server; the command and credentials never are.
func logMCPStartupPrepare(logger *slog.Logger, report MCPReport) {
	for _, result := range report.Results {
		switch result.Outcome {
		case mcpPrepareOutcomeNotApplied:
			// 未 applied 不是失败：Worker 不得隐式 apply（D-12），但依赖它的 Agent 本轮
			// 不注册，运维需要看到"为什么没连"。
			logger.Warn("MCP server 未应用——Worker 不隐式 apply，依赖它的 Agent 本轮不注册",
				"server", result.Name, "outcome", result.Outcome)
		case string(core.MCPApplyFailed):
			logger.Warn("MCP server 准备失败——保留应用版本，依赖它的 Agent 本轮不注册",
				"server", result.Name, "outcome", result.Outcome,
				"failure_class", result.FailureClass)
		}
	}
	logger.Info("MCP 启动准备完成", "configured", len(report.Configured),
		"connected", len(report.Connected), "tools", len(report.Tools))
}

func prepareMCPInstances(ctx context.Context, deps mcpPrepareDeps) (MCPReport, error) {
	if deps.Targets == nil {
		return MCPReport{}, nil
	}
	targets, err := deps.Targets(ctx)
	if err != nil {
		return MCPReport{}, fmt.Errorf("列出已发布 MCP server: %w", err)
	}
	report := MCPReport{
		Configured: make([]string, 0, len(targets)),
		Connected:  []string{},
		Tools:      []string{},
		Results:    make([]MCPPrepareResult, 0, len(targets)),
	}
	for _, target := range targets {
		report.Configured = append(report.Configured, target.Name)
		if !target.Applied {
			report.Results = append(report.Results, MCPPrepareResult{
				Name: target.Name, Outcome: mcpPrepareOutcomeNotApplied,
			})
			continue
		}
		if deps.Prepare == nil {
			// 装配缺准备入口：已 applied 的条目同样失败关闭，不冒充成功。
			report.Results = append(report.Results, MCPPrepareResult{
				Name: target.Name, Outcome: string(core.MCPApplyFailed),
				FailureClass: mcpTestClassDependencyUnconfirmed,
			})
			continue
		}
		result := deps.Prepare(ctx, target.Published)
		report.Results = append(report.Results, MCPPrepareResult{
			Name:             target.Name,
			Outcome:          string(result.Outcome),
			AppliedVersionID: result.AppliedVersionID,
			Reused:           result.Reused,
			FailureClass:     result.FailureClass,
		})
	}
	if deps.Status != nil {
		for _, target := range targets {
			status, ok := deps.Status(target.Name)
			if !ok || !status.Connected {
				continue
			}
			report.Connected = append(report.Connected, target.Name)
			report.Tools = append(report.Tools, status.Tools...)
		}
	}
	return report, nil
}
