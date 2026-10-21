package app

// 本文件是 `core.AgentAdmin` 的组合适配器（颗粒度文档 §3.1.30）：内容读写委托给
// workspace.AgentEditor，启停迁移把"先留痕、再动状态文件、再同步可重建索引"组合在
// 同一个适配器里。web 只依赖 core.AgentAdmin，不感知 workspace 的具体实现。
// This file wires core.AgentAdmin: content reads and writes delegate to the workspace
// editor, while lifecycle transitions compose the operation pre-write, the authoritative
// state file and the rebuildable index. The web layer only sees core.AgentAdmin.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strconv"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// lifecycleResultOK / lifecycleResultInternal 是启停操作 result_code 的两个固定取值；
	// 与其他管理写同一码表（契约 §7.4），不新造码。
	// The two result codes used by lifecycle operations; the code table is shared with
	// every other management write.
	lifecycleResultOK       = "OK"
	lifecycleResultInternal = "INTERNAL"

	// 状态文件不可用时的自产分类（约束 45）：只给原因与修复入口，**不**拼底层错误原文
	// （workspace 的读取错误可能带绝对路径），底层错误只经 Unwrap 暴露。
	// Self-authored categories for an unusable state file; the underlying error stays
	// reachable only through Unwrap so paths never reach the client.
	lifecycleMissingReason = "lifecycle state file missing; restore it before toggling"
	lifecycleCorruptReason = "lifecycle state file corrupt; restore it from a trusted copy"

	// lifecycleTool 是启停入参错误的点名标识（*core.InvalidInputError.Tool）。
	// lifecycleTool names the transition surface in invalid-input errors.
	lifecycleTool = "agent.transition"
)

// 编译期断言：适配器满足 web 依赖的消费方端口（§3.1.30）。
// Compile-time assertion that the adapter satisfies the consumer-side port.
var _ core.AgentAdmin = (*agentConsole)(nil)

// AgentConsoleDeps 是组合适配器的全部依赖。卡片的类型骨架列出 Editor / Lifecycle /
// Index 三项；启停"先留痕"所需的共享闸门、操作仓储、日志与时钟按 §3.1.30 的行为要求
// 一并注入（派生补口，见 run-state I-54）。
// AgentConsoleDeps carries every dependency of the adapter. The card's skeleton lists
// Editor / Lifecycle / Index; the shared gate, operation store, logger and clock are
// injected as well because §3.1.30's behaviour requires them (see run-state I-54).
type AgentConsoleDeps struct {
	// Editor 是内容条件写的实现（workspace 的写锁与操作留痕都在它内部）。
	Editor *workspace.AgentEditor
	// Lifecycle 是状态文件的唯一写者；Transition 要求调用方已持有该 Agent 写锁。
	Lifecycle *workspace.LifecycleStore
	// Index 是可重建的生命周期索引写端口。
	Index core.LifecycleIndex
	// Gate 必须与运行准入、内容写共用同一个实例（生产装配传共享闸门）。
	Gate *core.AgentGate
	// Operations 是管理操作仓储：先写 pending、成功 / 失败后回填终态。
	Operations core.OperationStore
	// Logger 记录索引同步失败等"变更已生效但外围失败"的 ERROR。
	Logger *slog.Logger
	// Now 是操作标识与时间戳的唯一时钟来源。
	Now func() time.Time
}

// agentConsole 是 core.AgentAdmin 的组合实现；字段全部私有，构造后不再变化。
// agentConsole implements core.AgentAdmin; its fields are fixed at construction.
type agentConsole struct {
	editor     *workspace.AgentEditor
	lifecycle  *workspace.LifecycleStore
	index      core.LifecycleIndex
	gate       *core.AgentGate
	operations core.OperationStore
	logger     *slog.Logger
	now        func() time.Time
}

// NewAgentConsole 组装适配器；Gate 为 nil 时退化为私有锁表（只防本适配器自身并发，
// **不**提供与准入的互斥——生产装配必须传共享实例），Logger / Now 为 nil 时回落默认值。
// NewAgentConsole assembles the adapter. A nil gate falls back to a private lock table
// that never serializes against admission; production assembly must pass the shared gate.
func NewAgentConsole(deps AgentConsoleDeps) core.AgentAdmin {
	gate := deps.Gate
	if gate == nil {
		gate = core.NewAgentGate()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &agentConsole{
		editor:     deps.Editor,
		lifecycle:  deps.Lifecycle,
		index:      deps.Index,
		gate:       gate,
		operations: deps.Operations,
		logger:     logger,
		now:        now,
	}
}

// ReadContent 委托给编辑器的读面：原文与强 ETag 来自同一次读取。
// ReadContent delegates to the editor so content and ETag come from the same read.
func (a *agentConsole) ReadContent(ctx context.Context, agent string) (core.AgentContent, error) {
	return a.editor.ReadContent(ctx, agent)
}

// UpdateContent 委托给编辑器的条件写：锁、ETag 比较、补丁、快照、原子写、注册表与
// 调度同步、操作留痕都在 workspace 内部完成（图 D），适配器只转交 actor，并在成功
// 之后补上生命周期投影——编辑器不依赖状态文件（§3.1.13 的依赖集里没有它），而响应
// 契约（AgentFilesView.lifecycle）要求给出该 Agent 状态文件的真值。
// UpdateContent delegates the conditional write and then fills the lifecycle
// projection: the editor has no lifecycle dependency, while the response contract
// requires the state-file truth for this agent.
func (a *agentConsole) UpdateContent(ctx context.Context, agent string, update core.AgentContentUpdate, ifMatch string, actor core.Actor) (core.AgentFiles, error) {
	files, err := a.editor.UpdateContent(ctx, agent, update, ifMatch, actor)
	if err != nil {
		return core.AgentFiles{}, err
	}
	return a.withLifecycle(ctx, files), nil
}

// withLifecycle 把状态文件读取结果投影到 `AgentFiles.Lifecycle`：分类与启停前置
// 校验同一口径——缺失 / 损坏记 valid=false 并给不自带路径的固定原因（约束 45），
// **不得**把缺失读成 active；读取失败也**不得**把已生效的写入改写成错误。
// withLifecycle projects the state file with the same fail-closed classification the
// transition precondition uses; a read failure never turns a successful write into
// an error, and the reason text never carries a path.
func (a *agentConsole) withLifecycle(ctx context.Context, files core.AgentFiles) core.AgentFiles {
	if a.lifecycle == nil {
		return files
	}
	// 写入已经生效：客户端断开也要投影真实状态，因此不随请求取消而放弃。
	rec, exists, err := a.lifecycle.Read(context.WithoutCancel(ctx), files.Agent)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		files.Lifecycle = core.AgentLifecycleView{LifecycleError: lifecycleMissingReason}
	case err != nil:
		files.Lifecycle = core.AgentLifecycleView{LifecycleError: lifecycleCorruptReason}
	case !exists:
		files.Lifecycle = core.AgentLifecycleView{LifecycleError: lifecycleMissingReason}
	default:
		state := string(rec.State)
		files.Lifecycle = core.AgentLifecycleView{State: &state, Valid: true}
	}
	return files
}

// Transition 执行一次启停迁移：静态校验与期望校验都在写锁内完成，且期望不符
// **不写操作行**（错误表：生命周期期望不符 → 409 CONFLICT、零操作行）。预写成功后
// 才动状态文件；状态文件生效后同步可重建索引，索引失败不回滚（状态文件是权威）。
// Transition performs one lifecycle change. Static and precondition checks run under
// the shared write lock and a failed precondition writes no operation row; only after
// the pending row lands does the authoritative file change. An index failure is logged
// and never rolls the file back.
func (a *agentConsole) Transition(ctx context.Context, agent string, target core.AgentState, expected core.AgentLifecycleTransition, actor core.Actor) (core.LifecycleRecord, error) {
	action, err := lifecycleAction(target)
	if err != nil {
		return core.LifecycleRecord{}, err
	}
	if err := workspace.ValidateAgentName(agent); err != nil {
		return core.LifecycleRecord{}, &core.InvalidInputError{Tool: lifecycleTool, Reason: err.Error()}
	}

	// 写锁与运行准入读锁、内容写锁共用同一把（约束 25 / 图 D）：期望校验、预写与
	// 状态迁移必须在同一临界区，否则两个并发启停会各自读到同一代数。
	// One critical section for precondition, pre-write and the file change, under the
	// very lock admission reads under.
	unlock := a.gate.Lock(agent)
	defer unlock()

	if err := a.checkTransitionPrecondition(ctx, agent, target, expected); err != nil {
		return core.LifecycleRecord{}, err
	}

	now := a.now()
	detail, err := json.Marshal(lifecycleOperationDetail{
		ExpectedState:      string(expected.ExpectedState),
		ExpectedGeneration: expected.ExpectedGeneration,
	})
	if err != nil {
		// 结构体只有字符串与整数，序列化失败属于不可达路径；仍如实上报而不是吞掉。
		return core.LifecycleRecord{}, fmt.Errorf("组装启停操作详情: %w", err)
	}
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   targetKindAgent,
		TargetID:     agent,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  lifecycleRequestHash(action, agent, expected),
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	}
	if beginErr := a.operations.BeginOperation(ctx, record); beginErr != nil {
		// 预写失败 = 拒绝执行（约束 10 / 图 C ②）：状态文件一个字节都没动。
		a.logger.Error("启停预写操作记录失败，拒绝执行",
			"agent", agent, "action", action, "error", beginErr)
		return core.LifecycleRecord{}, fmt.Errorf("启停预写操作记录失败，拒绝执行: %w", beginErr)
	}

	rec, transErr := a.lifecycle.Transition(ctx, agent,
		expected.ExpectedState, expected.ExpectedGeneration, target, operationID)
	if transErr != nil {
		a.finish(ctx, operationID, core.OperationFailed, lifecycleResultInternal,
			"生命周期状态写入失败", detail)
		return core.LifecycleRecord{}, transErr
	}

	if a.index != nil {
		if idxErr := a.index.PutLifecycleIndex(ctx, rec, true, ""); idxErr != nil {
			// 索引可从状态文件重建：状态文件已生效，不回滚、不把成功说成失败；
			// 记 ERROR，由下次启动基线修复索引（§3.1.30）。
			// The file is authoritative and already changed; keep the success semantics
			// and let the next startup baseline rebuild the index.
			a.logger.Error("启停：生命周期索引同步失败（状态文件为准，下次启动基线修复）",
				"agent", agent, "operation_id", operationID, "error", idxErr)
		}
	}

	a.finish(ctx, operationID, core.OperationSuccess, lifecycleResultOK, "", detail)
	return rec, nil
}

// checkTransitionPrecondition 在写锁内重读状态文件并核对期望：缺失 / 损坏映射为
// *core.AgentUnavailableError（409 AGENT_UNAVAILABLE，与运行准入同一分类），期望状态 /
// 代数不符与"已在目标态"映射为 core.ErrLifecycleConflict（409 CONFLICT）。
// checkTransitionPrecondition re-reads the state file under the write lock: a missing or
// corrupt file is AgentUnavailable, while a mismatched expectation or a no-op move is a
// lifecycle conflict. Neither path writes an operation row or touches the disk.
func (a *agentConsole) checkTransitionPrecondition(ctx context.Context, agent string, target core.AgentState, expected core.AgentLifecycleTransition) error {
	current, exists, err := a.lifecycle.Read(ctx, agent)
	switch {
	case err != nil && errors.Is(err, fs.ErrNotExist):
		return &core.AgentUnavailableError{Name: agent, Reason: lifecycleMissingReason, Cause: err}
	case err != nil:
		return &core.AgentUnavailableError{Name: agent, Reason: lifecycleCorruptReason, Cause: err}
	case !exists:
		return &core.AgentUnavailableError{Name: agent, Reason: lifecycleMissingReason}
	case current.State != expected.ExpectedState || current.Generation != expected.ExpectedGeneration:
		return fmt.Errorf("%w: Agent %s 期望 %s/%d，当前 %s/%d",
			core.ErrLifecycleConflict, agent, expected.ExpectedState, expected.ExpectedGeneration,
			current.State, current.Generation)
	case current.State == target:
		return fmt.Errorf("%w: Agent %s 已处于 %s，没有可执行的迁移",
			core.ErrLifecycleConflict, agent, target)
	}
	return nil
}

// finish 回填终态；终态回填不随请求取消而放弃（客户端断开不能让审计停在 pending）。
// 回填失败只记 ERROR：状态文件是否已变由启动恢复按 operation_id 查证。
// finish writes the terminal state detached from request cancellation. A finish failure
// is logged only; startup recovery decides the outcome from the state file's operation id.
func (a *agentConsole) finish(ctx context.Context, operationID string, status core.OperationStatus, code, message string, detail []byte) {
	if err := a.operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, detail, a.now()); err != nil {
		a.logger.Error("启停回填操作终态失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// lifecycleAction 把目标状态映射为写操作字面量。US-11 的写操作闭集只有
// `agent.disable` / `agent.enable`（约束 12）；归档属 US-12，不走本适配器。
// lifecycleAction maps the target state to the fixed action literals of US-11.
func lifecycleAction(target core.AgentState) (string, error) {
	switch target {
	case core.AgentDisabled:
		return actionDisable, nil
	case core.AgentActive:
		return actionEnable, nil
	default:
		return "", &core.InvalidInputError{
			Tool:   lifecycleTool,
			Reason: fmt.Sprintf("不支持的目标状态 %q：本模块只提供 enable / disable", target),
		}
	}
}

// lifecycleRequestHash 是脱敏请求摘要：动作、Agent、期望状态与代数按固定顺序拼接后
// 取 sha256；不含凭证与正文，供审计对账。
// lifecycleRequestHash digests the sanitized request shape for audit reconciliation.
func lifecycleRequestHash(action, agent string, expected core.AgentLifecycleTransition) string {
	sum := sha256.Sum256([]byte(action + "|" + agent + "|" +
		string(expected.ExpectedState) + "|" + strconv.FormatInt(expected.ExpectedGeneration, 10)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// lifecycleOperationDetail 是启停操作 `detail_json` 的固定投影；键名与启动恢复
// （baseline.go 的 operationDetail）逐字一致——恢复按这两个字段判定 applied /
// not_applied（图 C）。缺字段即无法判定，**不得**用零值猜。
// lifecycleOperationDetail is the fixed detail_json projection whose keys match the
// recovery-side struct byte for byte; figure C's verdicts depend on both fields.
type lifecycleOperationDetail struct {
	ExpectedState      string `json:"expected_state"`
	ExpectedGeneration int64  `json:"expected_generation"`
}
