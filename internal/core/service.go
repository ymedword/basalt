package core

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"time"
)

// 本文件是**唯一运行入口**（颗粒度文档 §2.3 流程 A）：准入 → 冻结快照并落库 → 取会话 →
// 组装运行环境 → 预写账本 → 写用户消息 → 循环 → 终态落库 → 返回。
// This file is the single run entry point (flow A): admission, frozen snapshot, session,
// run environment, ledger pre-write, user message, loop, terminal write, return.
//
// 两条纪律。① **准入不排队**：同会话忙、实例满都是直接拒绝——排队会把"立即失败"变成
// "过一会儿失败"，调用方拿到的是错误的调度语义（契约 §1.2）。② **证据先行**：快照落库、
// 账本预写、用户消息写入三步都在模型调用之前，任一步失败即拒绝启动——审计链要求的
// "先留痕再发出"在运行级同样成立。
// Two rules: admission never queues, and evidence is written before the model is called.

// defaultMaxConcurrentRuns 是实例级并发运行数的缺省上限（流程 A ①）。
// defaultMaxConcurrentRuns is the default instance-level concurrency cap.
const defaultMaxConcurrentRuns = 32

// 运行预算的缺省值（契约 §6）：调用方未填的项在这里补齐。
// Default budget values (contract §6), applied when the caller leaves a field unset.
const (
	defaultMaxIterations = 10
	defaultMaxToolCalls  = 50
)

// AgentSource 是运行入口需要的 Agent 装载面——**端口定义在消费方**（core），适配器在 app
// （宪法·架构约束：接口在消费方、实现在别处、装配在 app）。
// AgentSource is the agent-loading face the run entry point needs; the port is declared by
// its consumer and adapted in app.
type AgentSource interface {
	// Exists 判定 Agent 是否已注册（未注册 = 调用方问题，不落任何痕迹）。
	// Exists reports whether the agent is registered.
	Exists(name string) bool

	// Dir 返回 Agent 目录的绝对路径（进 `ToolEnv.AgentDir`，工具据它做沙箱判定）。
	// Dir returns the agent directory, which tools use for sandbox decisions.
	Dir(name string) string

	// Snapshot 冻结一次运行的配置快照（同一份原始 AGENT.md → Profile + 段一正文 + 哈希）。
	// Snapshot freezes the per-run configuration snapshot from a single read.
	Snapshot(ctx context.Context, name string, now time.Time) (*ConfigSnapshot, error)
}

// SessionStatusStore 是会话状态端口（消费方 = AgentService，实现 = storage.SessionStore）：
// 运行准入据它拒绝已归档会话，归档动作经它与在途运行互斥。未命中一律 fs.ErrNotExist。
// SessionStatusStore is the session-status port the entry point consumes; a miss is
// always fs.ErrNotExist.
type SessionStatusStore interface {
	GetSession(ctx context.Context, id string) (SessionRecord, error)  // 未命中返回 fs.ErrNotExist
	ArchiveSession(ctx context.Context, id string, at time.Time) error // 未命中返回 fs.ErrNotExist
}

// ServiceOptions 是运行入口的装配面。字段即依赖：没有任何一项从包级状态或 context 里取
// （宪法 VII）——`Now` 也显式注入，业务时间戳因此在测试里完全可控。
// ServiceOptions is the assembly surface of the run entry point; every dependency is an
// explicit field, including the clock.
type ServiceOptions struct {
	Agents        AgentSource          // Agent 装载面（是否存在 / 目录 / 运行级快照）
	Client        ProviderClient       // 唯一的模型调用入口（实现为 ProviderService：路由 + 两阶段审计在其内）
	Messages      MessageStore         // 会话历史写透（无状态调用也可为 nil）
	Runs          RunStore             // 运行账本
	Snapshots     SnapshotStore        // 配置快照落库
	Audit         ToolInvocationReader // 半截组补齐的分类依据（只读；可为 nil → 一律走"无行"分支）
	Memory        MemoryReader         // 记忆读取端口（可空：nil = 段 2 恒空，行为与无记忆逐字节相同）
	Loop          *ReActLoop           // 自实现的循环本体
	Logger        *slog.Logger         // 为空 → 进程缺省日志器
	Now           func() time.Time     // 时钟注入；为空 → time.Now
	MaxConcurrent int                  // 实例级并发运行上限；<=0 → 32
	// Sessions 是会话状态端口（归档 / 缺失判定）；生产装配必须由 wire.go 单点传入。
	// nil = 不做会话门禁（仅既有零值单测），此时 Archive 返回 *SessionGateUnavailableError，
	// 不得 nil 解引用。
	// Sessions is the session-status port. Nil disables the gate (zero-value tests only);
	// Archive then returns *SessionGateUnavailableError instead of dereferencing nil.
	Sessions SessionStatusStore
}

// AgentService 是唯一运行入口的实现：准入计数 + 一次运行的完整编排。
// **唯一入口**不是修辞：定时触发、评测基座、REST 端点都经它跑，渠道差异只体现在
// `RunRequest.Channel` 上（触发源的映射由仓储按 Channel 完成）。
// AgentService is *the* run entry point: schedulers, evals and future REST endpoints all
// run through it, and channels differ only by RunRequest.Channel.
type AgentService struct {
	opts ServiceOptions

	mu      sync.Mutex
	busy    map[string]struct{} // 会话忙集：键 = session_id（空串不入集——无状态调用之间互不相干）
	running int                 // 实例级在跑数
}

// NewAgentService 组装运行入口。缺省值在此补齐（缺省日志器 / 时钟 / 并发上限），
// 免得每个消费点各判一次 nil。
// NewAgentService assembles the entry point and fills the three documented defaults.
func NewAgentService(opts ServiceOptions) *AgentService {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = defaultMaxConcurrentRuns
	}
	return &AgentService{opts: opts, busy: map[string]struct{}{}}
}

// Run 执行一次运行。返回的 `RunResult` 无论成败都非零（Status 表达结局）；**函数级 error
// 只表达调用方问题**：Agent 不存在 / 同会话忙 / 实例满（契约 §1.2）——这三类**不排队**、
// 不落任何痕迹。其余失败一律体现在 `RunResult.Status`（含 failed 的 Reason）。
// Run runs one run. A function-level error means a caller-side problem only; everything
// else shows up in the result status.
func (s *AgentService) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	// ① 准入：未知 Agent → 会话锁 → 实例计数（顺序即错误优先级），三道都**不排队**。
	if s.opts.Agents == nil || !s.opts.Agents.Exists(req.Agent) {
		return RunResult{}, &AgentNotFoundError{Name: req.Agent}
	}
	release, err := s.admit(ctx, req)
	if err != nil {
		return RunResult{}, err
	}
	defer release()

	if s.opts.Loop == nil {
		// 装配错误（引擎没接上）：照实报，不落痕迹——半截账本比不跑更糟。
		return RunResult{}, errors.New("agent service: run loop is not configured")
	}

	now := s.opts.Now()
	// 调用方给了 RunID 就原样用（调度器预生成，用于把执行行与 Run 关联）；否则按既有
	// 公式生成——既有 CLI / REST / 评测调用方零改动（约束 13 / 14）。
	runID := req.RunID
	if runID == "" {
		runID = NewRunID(req.Channel, now, runSuffix())
	}

	// ② 冻结运行级快照并**先落库**：读盘失败或落库失败都拒绝启动（零 LLM 调用、零工具执行）。
	// 这一份快照在 Run 内不变——中途改盘不换配置（读即所记）。
	snap, err := s.opts.Agents.Snapshot(ctx, req.Agent, now)
	if err != nil {
		return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error()}, nil
	}
	if s.opts.Snapshots != nil {
		if err := s.opts.Snapshots.SaveSnapshot(ctx, snap); err != nil {
			return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error()}, nil
		}
	}
	req.ConfigHash = snap.Hash // 运行期填充（调用方不填）——与审计列同源

	// ③ 取会话：有 session_id → 建行并载入历史；空 → 仅内存（无状态调用仍落 Run 与审计）。
	sess := NewSession(req.SessionID, s.opts.Messages, s.opts.Audit)
	if req.SessionID != "" && s.opts.Messages != nil {
		if err := s.opts.Messages.UpsertSession(ctx, SessionRecord{
			ID:           req.SessionID,
			ProfileName:  req.Agent,
			Channel:      req.Channel,
			UserID:       req.UserID,
			CreatedAt:    now,
			LastActiveAt: now,
		}); err != nil {
			return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error(), ConfigHash: snap.Hash}, nil
		}
	}
	if err := sess.Load(ctx); err != nil {
		return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error(), ConfigHash: snap.Hash}, nil
	}

	// ④ 组装运行环境：ToolSet 取**快照冻结的** Profile（中途改盘不换配置），
	// 预算按"请求值与 Profile 收紧取小"合并；模型客户端经显式映射表的查表结果注入；
	// 记忆**在运行开始时读一次**、写进运行环境后冻结（运行内不重读——中途改盘不换记忆）。
	env := &RunEnv{
		Req:       req,
		Snapshot:  snap,
		Session:   sess,
		Budget:    mergeBudget(req.Budget, snap.Profile),
		ToolSet:   snapshotTools(snap),
		llmClient: s.opts.Client,
		agentDir:  s.opts.Agents.Dir(req.Agent),
	}
	if s.opts.Memory != nil {
		text, err := s.opts.Memory.LoadEffective(ctx, req.Agent)
		if err != nil {
			// 读取失败 = 拒绝启动（与段一读取失败同口径：零 LLM 调用、零工具执行），
			// 形状沿用既有 StatusFailed——不另立一套失败形态。
			return RunResult{RunID: runID, Status: StatusFailed, Reason: err.Error(), ConfigHash: snap.Hash}, nil
		}
		env.memory = text
	}

	// ⑤ 预写账本行（status=running）：**写失败 → 直接 error**——账本不落下就不开跑。
	if s.opts.Runs != nil {
		if err := s.opts.Runs.StartRun(ctx, RunRecord{
			RunID:      runID,
			SessionID:  req.SessionID,
			Agent:      req.Agent,
			Channel:    req.Channel,
			UserID:     req.UserID,
			Trigger:    req.Trigger,
			ConfigHash: snap.Hash,
			StartedAt:  now,
		}); err != nil {
			return RunResult{}, fmt.Errorf("start run ledger row: %w", err)
		}
	}

	// ⑥ 用户输入写进消息仓储：**宪法 II 规定的唯一写入点**。写失败 = fail-closed →
	// Run failed（先把账本收口，再如实返回失败）。
	if err := sess.AppendMessage(ctx, runID, Message{Role: RoleUser, Content: req.Message}); err != nil {
		return s.finish(ctx, runID, env, RunResult{
			Status: StatusFailed, Reason: err.Error(), ConfigHash: snap.Hash,
		}), nil
	}

	// ⑦ 循环（error 恒 nil：运行期失败归 Status）。
	res, _ := s.opts.Loop.run(ctx, runID, env)
	res.RunID = runID // RunEnv 的字段字不含 RunID（契约 §1.1），由运行入口填

	// ⑧⑨ 终态落库后返回——落库失败**不覆盖**已得结果（记 ERROR，行留 running 待恢复扫描收尾）。
	return s.finish(ctx, runID, env, res), nil
}

// finish 写终态账本行并返回结果。写失败只记 ERROR：结果照常返回——修复历史不能以
// 扣下已得结果为代价（与工具审计终态回填同一纪律）。
// finish writes the terminal ledger state; a failed write is logged only and the result
// still travels back to the caller.
func (s *AgentService) finish(ctx context.Context, runID string, env *RunEnv, res RunResult) RunResult {
	res.RunID = runID
	if res.ConfigHash == "" {
		res.ConfigHash = env.snapshotHash()
	}
	if s.opts.Runs == nil {
		return res
	}
	if err := s.opts.Runs.FinishRun(ctx, runID, res.Status, res.Reason, res.Iterations, res.ToolCalls, s.opts.Now()); err != nil {
		s.opts.Logger.Error("运行终态落库失败",
			"run_id", runID, "store", "runs", "op", "finish")
	}
	return res
}

// admit 完成准入的四步记账（**全部在同一把 `s.mu` 临界区内**）：忙集 → 实例满 →
// 会话状态（归档 / 缺失）→ 登记忙集。返回的释放函数**幂等**（defer 与错误路径都可能调）。
// 反了顺序就会漏记：先占会话再判实例，实例满时那次占用再也释放不掉。
// 无会话请求（SessionID == ""，无状态 invoke）只走实例上限一步——不得入忙集，否则
// 两条并发 invoke 会因空会话互相 409。
// admit performs the four admission steps inside one critical section: busy set, instance
// cap, session status, then registration. Stateless requests only face the instance cap.
func (s *AgentService) admit(ctx context.Context, req RunRequest) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionID := req.SessionID
	if sessionID != "" {
		if _, busy := s.busy[sessionID]; busy {
			return nil, &SessionBusyError{SessionID: sessionID}
		}
	}
	if s.running >= s.opts.MaxConcurrent {
		return nil, &InstanceFullError{Limit: s.opts.MaxConcurrent}
	}
	// 会话状态门禁：只有"要复用某个会话"的请求才查；DB 读在锁内（单次主键查询，
	// 这把锁只保护准入与归档，不覆盖整个运行——已知取舍）。
	// 未命中：要求既有 → 调用方错误；否则放行，后续 UpsertSession 自动创建。
	// 命中且已归档 → 一律拒绝（不论 RequireExistingSession）——cron / 手动触发也挡在这里。
	// 其它读错误原样上抛（fail-closed，不把基础设施故障当成"会话不存在"）。
	if sessionID != "" && s.opts.Sessions != nil {
		rec, err := s.opts.Sessions.GetSession(ctx, sessionID)
		switch {
		case err == nil:
			if rec.Status == "archived" {
				return nil, &SessionArchivedError{SessionID: sessionID}
			}
		case errors.Is(err, fs.ErrNotExist):
			if req.RequireExistingSession {
				return nil, &SessionNotFoundError{SessionID: sessionID}
			}
		default:
			return nil, err
		}
	}
	if sessionID != "" {
		s.busy[sessionID] = struct{}{}
	}
	s.running++

	released := false
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if released {
			return
		}
		released = true
		if sessionID != "" {
			delete(s.busy, sessionID)
		}
		s.running--
	}, nil
}

// Archive 归档一个会话：与在途运行互斥（同一把锁保护准入与归档），重复归档幂等。
// 空会话 → *SessionNotFoundError（无可归档）；nil 门禁 → *SessionGateUnavailableError
// （装配缺陷，映射 500——不 nil 转发、不静默成功）；仓储未命中 → *SessionNotFoundError，
// 其余错误原样上抛。
// Archive archives a session, mutually exclusive with in-flight runs; archiving twice is
// idempotent. A nil gate is an assembly defect and never a silent success.
func (s *AgentService) Archive(ctx context.Context, sessionID string) error {
	if s.opts.Sessions == nil {
		return &SessionGateUnavailableError{}
	}
	if sessionID == "" {
		return &SessionNotFoundError{SessionID: sessionID}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.busy[sessionID]; busy {
		return &SessionBusyError{SessionID: sessionID}
	}
	if err := s.opts.Sessions.ArchiveSession(ctx, sessionID, s.opts.Now()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &SessionNotFoundError{SessionID: sessionID}
		}
		return err
	}
	return nil
}

// mergeBudget 合并实际预算：请求值未填的项补契约 §6 缺省，再让 Profile **只能收紧**迭代上限。
//
// 装载层保留了 `settings.max_iterations` 的**显式零值**（US-3 注 9），故这里**不得**把 0
// 当成"未声明"——它是一句"该 Agent 不许调用模型"的明确配置，收紧到 0 正是它的语义。
// mergeBudget fills the documented defaults and lets the profile tighten the iteration cap
// only. An explicit zero survives the loader on purpose and is honoured here.
func mergeBudget(req Budget, profile *Profile) Budget {
	if req.MaxIterations <= 0 {
		req.MaxIterations = defaultMaxIterations
	}
	if req.MaxToolCalls <= 0 {
		req.MaxToolCalls = defaultMaxToolCalls
	}
	if req.ToolTimeout <= 0 {
		req.ToolTimeout = defaultToolTimeout
	}
	if req.MaxOutputBytes <= 0 {
		req.MaxOutputBytes = defaultMaxOutputBytes
	}
	if req.ContextTokens <= 0 {
		req.ContextTokens = defaultContextTokens
	}
	if profile != nil && profile.Settings.MaxIterations < req.MaxIterations {
		req.MaxIterations = profile.Settings.MaxIterations
	}
	return req
}

// snapshotTools 取快照冻结的工具名集；Profile 缺失（未注册的 Agent 不会有快照）时为空池。
// **空池 ≠ 全部可用**：不声明就是没有工具（陷阱 #9）。
// snapshotTools returns the frozen tool-name set; an absent profile means an empty pool,
// which never means "everything is allowed".
func snapshotTools(snap *ConfigSnapshot) []string {
	if snap == nil || snap.Profile == nil {
		return nil
	}
	return snap.Profile.Tools
}

// runSuffix 生成 RunID 的随机段：**6 位 base36 小写**（§6 固定口径）。
// 随机源取不到时不回落时间 / 计数器（那会退化成可预测的 id）：以全零段收场，唯一性仍由
// 时间戳段保证，而这次故障会由启动期的完整性检查暴露。
// runSuffix returns the six-character base36 suffix of a run id; a missing entropy source
// never degrades into a predictable counter.
func runSuffix() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	// 252 = 36×7：把 ≥252 的字节剔除，取模偏差即归零（256 = 36×7 + 4）。
	const cutoff = 252
	var (
		out [6]byte
		buf [1]byte
	)
	for i := 0; i < len(out); {
		if _, err := rand.Read(buf[:]); err != nil {
			return "000000"
		}
		if int(buf[0]) >= cutoff {
			continue
		}
		out[i] = alphabet[int(buf[0])%36]
		i++
	}
	return string(out[:])
}
