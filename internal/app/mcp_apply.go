package app

// 本文件是 MCP 受控重建（`mcp_server.apply`，14D / 设计 §2.3.3、契约 openapi
// `/mcp-servers/{name}/apply`）：把已发布版本切换成**运行生效**版本，只重建这一个
// server，其它连接的调度不受影响。
//
// 算法顺序是硬约束，代码按八步逐段对应：
//  1. 目标已由调用方冻结（`in.Target`）；本文件不读指针，也就没有"草稿"分支；
//  2. 冻结的定义版本与凭据版本由调用方写进操作行 `detail`（本文件只拿已解析好的配置）；
//  3. 就绪预检在调用方（本文件不会启动进程做预检）；
//  4. 授权先于建连；
//  5. 建候选连接时旧连接仍在服务（快照**不**递增 epoch、**不**关连接）；
//  6. 并发校验与 7 的 CAS 在同一段临界区（gate 写锁 + entry.mu）里完成，失败走同一段
//     丢弃：撤回候选注册、关闭候选、`applied` 不变、操作记 `superseded`；
//  7. 注册新工具集 + CAS + epoch 递增，旧代封门进入排空窗口（默认 30 秒，到点强制关闭）；
//  8. 建连失败 → 关闭候选，`applied` 不变，记 `failed` + 失败分类；旧连接**仅当仍然合法**
//     才保留——冻结版本含已撤销凭据时旧连接一并关闭，绝不把撤销"顺延"下去。
// The controlled apply of one published MCP version, step by step as the contract spells it
// out. The snapshot never bumps the epoch: the old generation keeps serving until the commit
// section atomically swaps registry, CAS and epoch.

import (
	"context"
	"errors"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// defaultMCPDrainTimeout 是旧连接排空窗口的默认长度（契约 `drain_timeout_seconds` 默认 30）。
// The default drain window; the contract fixes 30 seconds.
const defaultMCPDrainTimeout = 30 * time.Second

// mcpApplyEvent 是 apply 授权审计的事件名（与 Run 调用审计分开，动作字面量不在其闭集内）。
// The audit event name of a controlled apply.
const mcpApplyEvent = "mcp.apply"

// MCPApplyInput 是一次受控重建的输入。`Target` 是**已解析占位符**的目标配置与版本号；
// `PreviousVersionID` 是第 1 步读到的 applied（写进操作行 detail，本文件只用它做返回值）；
// `Commit` 与 `OldLegal` 是装配层的两个回调，见字段注释。
// MCPApplyInput carries the frozen target plus two assembly callbacks.
type MCPApplyInput struct {
	Target            core.PublishedMCPServer
	PreviousVersionID int64
	// DrainTimeout 是旧连接排空窗口；0 取默认 30 秒。
	DrainTimeout time.Duration
	// Commit 在 gate 写锁 + entry.mu 内执行并发校验与 CAS 指针。返回非空 `supersededBy`
	// 表示期间有 publish / revoke / 另一次 apply 先完成——本次丢弃候选。它**不**碰注册池：
	// 注册池的整组替换由 `ReplaceTools` 在同一段临界区里紧跟它完成。
	// Commit runs inside gate+entry.mu: concurrent check and CAS only. The registry swap
	// follows it in the same critical section through ReplaceTools.
	Commit func(ctx context.Context) (core.MCPApplySupersededBy, error)
	// OldLegal 回答冻结的旧版本在本次失败后是否仍然合法（凭据仍可用）。nil = 无法判定，
	// 按合法处理（没有证据就不关别人的连接）。
	// OldLegal reports whether the previous version is still legal after a failure; nil
	// means undeterminable and is treated as legal.
	OldLegal func(ctx context.Context) bool
}

// MCPApplyResult 是受控重建的结论（与契约 `McpServerApplyResult` 一一对应；`server` 与
// `operation_id` 由调用方补，因为它们不属于连接层）。
// MCPApplyResult mirrors the contract result without the operation and view fields.
type MCPApplyResult struct {
	Outcome          core.MCPApplyOutcome
	AppliedVersionID int64
	DrainDeadline    time.Time
	FailureClass     string
	SupersededBy     core.MCPApplySupersededBy
}

// mcpReplaceCommit 是提交段的读数。
// mcpReplaceCommit is the outcome of the commit section.
type mcpReplaceCommit struct {
	supersededBy core.MCPApplySupersededBy
	err          error
	drainAt      time.Time
}

// Replace 执行一次受控重建（八步中的 4–8；1–3 在调用方）。
// Replace runs the controlled rebuild of one server.
func (s *MCPSupervisor) Replace(ctx context.Context, in MCPApplyInput) MCPApplyResult {
	cfg := in.Target.Config
	if err := validateSupervisorConfig(cfg); err != nil {
		// 配置本身不可用属于"预检之后仍无法建连"的同一档：不猜具体原因。
		return MCPApplyResult{
			Outcome: core.MCPApplyFailed, AppliedVersionID: in.PreviousVersionID,
			FailureClass: mcpTestClassDependencyUnconfirmed,
		}
	}

	// 第 4 步：先授权、后建连。拒绝**不碰**旧连接——本次没有切换，旧版本仍在合法运行。
	if err := s.authorize(ctx, mcpApplyEvent, cfg, in.Target.VersionID); err != nil {
		s.deps.Logger.Error("MCP 受控重建被策略拒绝", "server", cfg.Name,
			"target_version_id", in.Target.VersionID)
		return MCPApplyResult{
			Outcome: core.MCPApplyFailed, AppliedVersionID: in.PreviousVersionID,
			FailureClass: mcpResultPolicyDenied,
		}
	}

	// 第 5 步：快照旧代。**不**递增 epoch、**不**关闭连接——新连接建立期间旧连接仍服务。
	entry := s.entry(cfg.Name)
	entry.mu.Lock()
	epoch := entry.epoch
	oldConn := entry.conn
	oldLink := entry.link
	appliedBefore := entry.status.AppliedVersionID
	entry.mu.Unlock()

	connector := s.newConnector()
	conns, failures, err := connectDetailed(ctx, connector, []core.MCPServerConfig{cfg})
	if err == nil && len(conns) == 0 {
		// 连接池把单 server 失败吸收为"零连接"，这里必须显式转成失败。
		err = errors.New(mcpStatusConnectFailed)
	}
	if err != nil {
		s.closeQuietly(connector, cfg.Name, "候选")
		class := mcpApplyConnectFailureClass(ctx, failures)
		s.releaseIllegalOld(ctx, entry, epoch, oldConn, oldLink, appliedBefore, in.OldLegal)
		s.deps.Logger.Error("MCP 受控重建：新连接失败——保留旧连接与应用版本",
			"server", cfg.Name, "target_version_id", in.Target.VersionID, "failure_class", class)
		return MCPApplyResult{
			Outcome: core.MCPApplyFailed, AppliedVersionID: appliedBefore, FailureClass: class,
		}
	}

	link := newMCPLink(cfg.Name)
	conns = mcpWrapConnections(link, conns)
	window := in.DrainTimeout
	if window <= 0 {
		window = defaultMCPDrainTimeout
	}
	commit := s.commitReplace(ctx, entry, epoch, in, connector, link, conns, window)

	if commit.supersededBy != core.MCPApplySupersededByNone {
		// 第 6 步：候选整组丢弃。`Commit` 在 CAS 之前判定，注册池未被改动。
		s.closeQuietly(connector, cfg.Name, "候选（被并发写取代）")
		s.deps.Logger.Info("MCP 受控重建被并发写取代", "server", cfg.Name,
			"superseded_by", string(commit.supersededBy))
		return MCPApplyResult{
			Outcome: core.MCPApplySuperseded, AppliedVersionID: appliedBefore,
			SupersededBy: commit.supersededBy,
		}
	}
	if commit.err != nil {
		// 注册 / CAS 失败：候选关闭，旧连接按同一合法性规则处理。
		s.closeQuietly(connector, cfg.Name, "候选（提交失败）")
		s.releaseIllegalOld(ctx, entry, epoch, oldConn, oldLink, appliedBefore, in.OldLegal)
		s.deps.Logger.Error("MCP 受控重建：提交段失败——保留旧连接与应用版本",
			"server", cfg.Name, "target_version_id", in.Target.VersionID)
		return MCPApplyResult{
			Outcome: core.MCPApplyFailed, AppliedVersionID: appliedBefore,
			FailureClass: mcpTestClassDependencyUnconfirmed,
		}
	}

	// 第 7 步后半：旧代封门进入排空；窗口内已发出的调用继续，到点强制关闭。
	failureClass := s.drainOld(ctx, oldLink, oldConn, window)
	s.deps.Logger.Info("MCP 受控重建完成", "server", cfg.Name,
		"target_version_id", in.Target.VersionID, "previous_version_id", appliedBefore,
		"drain_deadline", commit.drainAt.UTC().Format(time.RFC3339), "failure_class", failureClass)
	return MCPApplyResult{
		Outcome: core.MCPApplyApplied, AppliedVersionID: in.Target.VersionID,
		DrainDeadline: commit.drainAt, FailureClass: failureClass,
	}
}

// commitReplace 是第 6–7 步的临界区：gate → entry.mu（约束 42 的唯一允许组合），
// 在里面做并发校验、CAS 与工具集替换。换池与指针必须同段完成——否则新 Run 可能看到
// "指针已换、工具还是旧的"。
// commitReplace runs the concurrent check, CAS and tool swap inside gate+entry.mu.
func (s *MCPSupervisor) commitReplace(ctx context.Context, entry *mcpServerEntry, epoch uint64,
	in MCPApplyInput, connector mcpConnector, link *mcpLink, conns []tool.MCPConnection,
	window time.Duration,
) mcpReplaceCommit {
	if s.deps.Gate != nil {
		s.deps.Gate.Lock()
		defer s.deps.Gate.Unlock()
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if entry.epoch != epoch {
		// 期间被 Start / Stop / RevokeServer 顶替：候选不再属于当前槽位。
		return mcpReplaceCommit{supersededBy: core.MCPApplySupersededByApply}
	}
	if in.Commit != nil {
		// 已完成的切换不得因调用方取消而丢失（取消只废止还没做的事）。
		by, err := in.Commit(context.WithoutCancel(ctx))
		if err != nil {
			return mcpReplaceCommit{err: err}
		}
		if by != core.MCPApplySupersededByNone {
			return mcpReplaceCommit{supersededBy: by}
		}
	}
	// CAS 成功之后立刻换池，仍在同一段临界区：新 Run 要么看到旧的一整组，要么看到新的
	// 一整组（C1 / C4）。候选在 CAS 之前从未登记，因此丢弃分支不需要"撤回注册"。
	// The swap follows the CAS under the same lock; a discarded candidate was never
	// registered, so the rollback branch has nothing to withdraw.
	if s.deps.ReplaceTools != nil {
		s.deps.ReplaceTools(in.Target.Config.Name, mcpAdapterTools(conns))
	}

	entry.epoch++
	entry.conn = connector
	entry.link = link
	entry.httpHost = ""
	if mcpTransport(in.Target.Config.Transport) == mcpTransportHTTP {
		entry.httpHost = mcpHost(in.Target.Config.URL)
	}
	entry.status = MCPServerStatus{
		Name: in.Target.Config.Name, Transport: mcpTransport(in.Target.Config.Transport),
		Connected: true, Tools: toolNamesOf(conns),
		AppliedVersionID: in.Target.VersionID,
	}
	return mcpReplaceCommit{drainAt: s.deps.Now().Add(window)}
}

// drainOld 让旧代进入排空：门切到排空态（新调用立刻拒绝），已发出的调用按窗口等待；
// 窗口到点仍未归零则强制关闭并返回 `drain_timeout`。窗口内自然归零、或调用方取消
// （切换已提交，取消不废止已完成的事）都按正常关闭处理，不记失败分类。
// drainOld seals the old generation's gate, waits up to the window for in-flight calls,
// then force-closes. Only a window expiry is classified as drain_timeout.
func (s *MCPSupervisor) drainOld(ctx context.Context, oldLink *mcpLink, oldConn mcpConnector,
	window time.Duration,
) string {
	if oldConn == nil {
		return ""
	}
	if oldLink == nil {
		// 正常路径不会到这里（连接必有门）；没有门就没有"在途"可等，直接关闭。
		s.closeQuietly(oldConn, "", "旧")
		return ""
	}
	idle := oldLink.beginDrain(mcpLinkDrainingReason)
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
		oldLink.seal(mcpLinkClosedReason)
		s.closeQuietly(oldConn, oldLink.server, "旧（排空窗口到点）")
		s.deps.Logger.Warn("MCP 旧连接排空窗口到点，已强制关闭",
			"server", oldLink.server, "window_seconds", int(window.Seconds()))
		return core.MCPApplyFailureDrainTimeout
	case <-ctx.Done():
		oldLink.seal(mcpLinkClosedReason)
		s.closeQuietly(oldConn, oldLink.server, "旧（调用方取消后收尾）")
		return ""
	}
	oldLink.seal(mcpLinkClosedReason)
	s.closeQuietly(oldConn, oldLink.server, "旧")
	return ""
}

// releaseIllegalOld 在失败路径上处理旧连接：仍然合法就保留（apply 失败不该中断服务），
// 已不合法（冻结版本含已撤销凭据）则关闭并把状态标成"连接已关闭 + 凭据已撤销"。
// `applied` 保留——版本事实不变，运行记录里那次运行确实用的是该版本。
// releaseIllegalOld keeps or closes the old connection after a failed apply, never
// letting a revoked credential survive inside an old link.
func (s *MCPSupervisor) releaseIllegalOld(ctx context.Context, entry *mcpServerEntry, epoch uint64,
	oldConn mcpConnector, oldLink *mcpLink, applied int64, legal func(context.Context) bool,
) {
	if oldConn == nil {
		return
	}
	if legal == nil || legal(ctx) {
		return
	}
	entry.mu.Lock()
	stillOurs := entry.epoch == epoch
	name := entry.status.Name
	if stillOurs {
		entry.conn = nil
		entry.link = nil
		entry.httpHost = ""
		entry.status.Connected = false
		entry.status.Tools = nil
		entry.status.Error = mcpLinkRevokedReason
	}
	entry.mu.Unlock()
	if !stillOurs {
		// 已被新代顶替：那条连接由顶替者负责关闭（与 abandonApply 同一条纪律）。
		return
	}
	if oldLink != nil {
		oldLink.seal(mcpLinkRevokedReason)
	}
	s.closeQuietly(oldConn, name, "旧（冻结版本含已撤销凭据）")
	s.deps.Logger.Warn("MCP 受控重建失败且旧版本已不合法——已关闭旧连接",
		"server", name, "applied_version_id", applied)
}

// RevokeServer 让一次凭据撤销**立即**生效：门先关死（之后的调用拒绝发出，而不是发出去
// 再收 401），连接随后关闭（stdio 进程树 / http 会话一并回收）。`applied` 保留——版本
// 事实不变，运行记录里那次运行确实用的是这个已撤销版本。撤销不等 apply。
// RevokeServer makes a credential revocation effective immediately: gate first, then the
// connection. Applied is kept because the version fact did not change.
func (s *MCPSupervisor) RevokeServer(ctx context.Context, name string) error {
	_ = ctx // 关闭本身没有可取消的远端等待
	entry, ok := s.lookup(name)
	if !ok {
		return nil
	}
	entry.mu.Lock()
	entry.epoch++
	conn := entry.conn
	link := entry.link
	entry.conn = nil
	entry.link = nil
	entry.httpHost = ""
	entry.status.Connected = false
	entry.status.Tools = nil
	entry.status.Error = mcpLinkRevokedReason
	entry.mu.Unlock()

	if link != nil {
		link.seal(mcpLinkRevokedReason)
	}
	if conn == nil {
		return nil
	}
	return conn.Close()
}

// mcpApplyConnectFailureClass 把候选连接的失败翻成 apply 的失败分类：超时（含预算到点
// 与连接池吸收的 `class=timeout`）优先，其余取结构化分类，没有结构化信息落
// `dependency_unconfirmed`——不猜"未安装"。
// mcpApplyConnectFailureClass maps a candidate connect failure onto the apply class set.
func mcpApplyConnectFailureClass(ctx context.Context, failures []tool.MCPConnectFailure) string {
	class := ""
	if len(failures) > 0 && failures[0].Class != "" {
		class = failures[0].Class
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) ||
		class == mcpTestClassTimeout {
		return mcpTestClassTimeout
	}
	if class == "" {
		return mcpTestClassDependencyUnconfirmed
	}
	return class
}

// mcpAdapterTools 摊平连接结果里的工具实例（注册池与提交回调的入参形状）。
// mcpAdapterTools flattens the connected adapters for the commit callback.
func mcpAdapterTools(conns []tool.MCPConnection) []core.Tool {
	tools := make([]core.Tool, 0, len(conns))
	for _, conn := range conns {
		tools = append(tools, conn.Tools...)
	}
	return tools
}

// closeQuietly 关闭一个连接器：失败只记 ERROR 不改变结论（连接已不可用，重试没有意义，
// 但排障要能看到）。
// closeQuietly closes a connector, logging failures without changing the verdict.
func (s *MCPSupervisor) closeQuietly(conn mcpConnector, server, label string) {
	if conn == nil {
		return
	}
	if err := conn.Close(); err != nil {
		s.deps.Logger.Error("关闭 MCP 连接失败", "server", server, "link", label,
			"detail", mcpErrorSummary(err))
	}
}
