package app

// 本文件是 US-22 受控自我进化的**只读查询面**（颗粒度文档 §3.1.1 / §3.1.8）：
// `core.LearningAdmin` 的生产实现。它把状态、抽取账本、候选投影与方法草案投影
// 交给 web / CLI，自身**没有**任何写方法——确认与删除复用既有记忆管理动作，
// 恢复查证在 `learning_recovery.go`，抽取本身在 `internal/memory`。
//
// 三条边界：
//  1. **只读**：本文件不调用任何写端口，也不触发抽取；状态页的计数全部来自
//     `LearningAdminStore` 的读方法。
//  2. **草案正文以文件为唯一权威**：DB 只存哈希与元数据，`Content` 由本层从
//     `DraftRoot/<draft_id>/DRAFT.md` 读取填充；读不到时记 WARN 并留空，
//     不把一条坏行升级成整个列表失败（孤儿目录由状态页计数报告，不自动删）。
//  3. **状态错误必须显式**：存储不可用时返回错误，**不得**把错误吞成空计数或
//     空列表——「没有抽取」与「读不到」在排障面上是两件事（C-27 / §4.4）。
//
// The read-only learning surface of US-22: status, extraction ledger and draft
// projections. It has no write method at all; draft bodies come from the fixed
// DRAFT.md on disk, and store errors stay errors instead of being flattened into
// empty counts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
)

// LearningAdminDeps 是只读学习查询面的装配依赖。
// LearningAdminDeps carries the read-only learning dependencies.
type LearningAdminDeps struct {
	// Store 是只读与恢复查证端口——**必填**：缺它即查询面未装配，每个方法
	// fail-closed，不返回空列表冒充「没有抽取」。
	// Store is required; without it every method fails closed instead of pretending
	// an empty instance.
	Store memory.LearningAdminStore
	// DraftRoot 是草案目录的绝对路径（`<workspace>/.basalt/learning/drafts`）；
	// 空串 = 草案正文不可读（只返回 DB 元数据）。
	// DraftRoot is the absolute drafts directory; empty means metadata only.
	DraftRoot string
	// Enabled 是生效的启用状态（`learning.auto_extract`）；状态页原样展示。
	// Enabled mirrors the effective learning.auto_extract flag.
	Enabled bool
	// Agents 是配置的 Agent 名单（原样展示，顺序保持配置文件里的顺序）。
	// Agents is the configured allow-list, preserved in file order.
	Agents []string
	// UnknownAgents 是名单内但**未注册**的 Agent（Q7：WARN + 状态页列出，
	// 不阻止启动、不抽取）。由装配层用已注册 Agent 名集合算好传入。
	// UnknownAgents lists configured names that are not registered; the assembly
	// computes the difference against the registered set.
	UnknownAgents []string
	// Limits 提供预算窗口与尝试上限；零值回落 §3.1.12 的缺省常量。
	// Limits supplies the budget window and attempt cap; zero falls back to defaults.
	Limits memory.LearningLimits
	// Logger / Now 为 nil 时回落进程默认值。
	// Logger and Now fall back to process defaults when nil.
	Logger *slog.Logger
	Now    func() time.Time
}

// LearningAdmin 是 `core.LearningAdmin` 的生产实现；**没有**写方法。
// LearningAdmin implements core.LearningAdmin and deliberately exposes no write
// method: promotion stays with the existing memory admin actions.
type LearningAdmin struct {
	store         memory.LearningAdminStore
	draftRoot     string
	enabled       bool
	agents        []string
	unknownAgents []string
	limits        memory.LearningLimits
	logger        *slog.Logger
	now           func() time.Time
}

var _ core.LearningAdmin = (*LearningAdmin)(nil)

// NewLearningAdmin 装配只读查询面。Store 缺失时不 panic：查询方法按 store error
// 失败关闭（与 MemoryAdmin 的 nil-store 语义同口径）。
// NewLearningAdmin assembles the read-only surface; a nil store fails closed per
// call rather than panicking at assembly time.
func NewLearningAdmin(deps LearningAdminDeps) *LearningAdmin {
	limits := deps.Limits
	if limits.AttemptsPerAgent <= 0 || limits.BudgetWindow <= 0 {
		limits = memory.DefaultLearningLimits()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &LearningAdmin{
		store:         deps.Store,
		draftRoot:     deps.DraftRoot,
		enabled:       deps.Enabled,
		agents:        slices.Clone(deps.Agents),
		unknownAgents: slices.Clone(deps.UnknownAgents),
		limits:        limits,
		logger:        logger,
		now:           now,
	}
}

// unknownLearningAgents 返回配置名单中未注册的 Agent（排序固定）。
// 大小写与字符集校验归配置装载（§3.1.11）；这里只做集合差，不改写名字。
// unknownLearningAgents returns configured names that are not registered, sorted;
// character/duplicate validation stays with the configuration loader.
func unknownLearningAgents(configured, registered []string) []string {
	if len(configured) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(registered))
	for _, name := range registered {
		known[name] = struct{}{}
	}
	var out []string
	for _, name := range configured {
		if _, ok := known[name]; !ok {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// reportUnknownLearningAgents 计算名单内未注册的 Agent 并按 Q7 记 WARN，返回同一份
// 名单供状态页展示。它**不**把未知名字升级成启动错误：启用时每条记一次 WARN（该
// Agent 不抽取——没有 Run 就不会有抽取），未启用时静默返回（配置尚未生效，仍供
// 状态页列出）。装配层只调用它一次，WARN 与状态页因此不会出现两份计算。
// reportUnknownLearningAgents logs one WARN per unknown name when the section is
// enabled and returns the same list for the status page; unknown names never abort
// startup. Assembly calls it once so the warning and the status share one computation.
func reportUnknownLearningAgents(logger *slog.Logger, enabled bool, configured, registered []string) []string {
	unknown := unknownLearningAgents(configured, registered)
	if !enabled {
		return unknown
	}
	for _, name := range unknown {
		logger.Warn("学习抽取：名单内的 Agent 未注册——该 Agent 不抽取（Q7）", "agent", name)
	}
	return unknown
}

// Status 返回学习面的状态读模型：预算计数、待决 / 未查证计数、版本与孤儿草案数。
// 计数由存储层一次查询给出；孤儿草案是目录扫描（只报告，不删）。
// Status reports the learning status; counters come from one store round-trip and
// orphan drafts from a bounded directory scan that never deletes anything.
func (a *LearningAdmin) Status(ctx context.Context) (core.LearningStatus, error) {
	if a.store == nil {
		return core.LearningStatus{}, errors.New("学习状态：只读存储未装配")
	}
	// 滚动窗口按「每行 last_attempt_at >= now-24h 的 SUM(attempts)」计（§3.1.12）：
	// 重试计入，保守归组——宁可少算可用额度，也不突破尝试上限。
	// The rolling window sums attempts per row touched within the window; retries count.
	counts, err := a.store.LearningCounts(ctx, a.now().UTC().Add(-a.limits.BudgetWindow))
	if err != nil {
		return core.LearningStatus{}, fmt.Errorf("学习状态：读取预算与计数: %w", err)
	}
	last, _, err := a.store.LastExtractionAt(ctx)
	if err != nil {
		return core.LearningStatus{}, fmt.Errorf("学习状态：读取最近抽取时间: %w", err)
	}
	orphans, err := a.countOrphanDrafts(ctx)
	if err != nil {
		return core.LearningStatus{}, fmt.Errorf("学习状态：统计孤儿草案: %w", err)
	}
	return core.LearningStatus{
		Enabled:       a.enabled,
		Agents:        slices.Clone(a.agents),
		UnknownAgents: slices.Clone(a.unknownAgents),
		// 版本号来自 `internal/memory` 的代码常量：状态页不能自己复制一份字面量，
		// 否则改规则时会出现「抽取行写 v2、状态页仍报 v1」的漂移。
		// Versions alias the code constants in internal/memory; the read face never
		// duplicates the literals.
		RuleVersion:        memory.LearningRuleVersion,
		PromptVersion:      memory.LearningPromptVersion,
		AttemptsUsed:       counts.Attempts,
		AttemptLimit:       a.limits.AttemptsPerAgent,
		Exhausted:          counts.Attempts >= a.limits.AttemptsPerAgent,
		BudgetWindow:       a.limits.BudgetWindow,
		PendingExtractions: counts.Pending,
		UnknownExtractions: counts.Unknown,
		OrphanDrafts:       orphans,
		LastExtractionAt:   last,
	}, nil
}

// ListExtractions 返回抽取列表投影（账本行 + 候选 + 草案摘要）。
// ListExtractions lists the extraction projections with their candidates and drafts.
func (a *LearningAdmin) ListExtractions(ctx context.Context, filter core.LearningExtractionFilter) ([]core.LearningExtractionView, error) {
	if a.store == nil {
		return nil, errors.New("学习查询：只读存储未装配")
	}
	views, err := a.store.ListExtractions(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("学习查询：列出抽取: %w", err)
	}
	return views, nil
}

// LoadExtraction 返回一次抽取的读模型；不存在返回 `core.ErrLearningNotFound`
// （web 映射 404，**不**与状态冲突 409 混用）。
// LoadExtraction returns one extraction; absence maps to ErrLearningNotFound.
func (a *LearningAdmin) LoadExtraction(ctx context.Context, extractionID string) (core.LearningExtractionView, error) {
	if a.store == nil {
		return core.LearningExtractionView{}, errors.New("学习查询：只读存储未装配")
	}
	view, found, err := a.store.LoadExtraction(ctx, extractionID)
	if err != nil {
		return core.LearningExtractionView{}, fmt.Errorf("学习查询：读取抽取（%s）: %w", extractionID, err)
	}
	if !found {
		return core.LearningExtractionView{}, core.ErrLearningNotFound
	}
	return view, nil
}

// ListDrafts 返回草案元数据，并从 DRAFT.md 填充正文。
// ListDrafts lists draft metadata and fills each body from its DRAFT.md.
func (a *LearningAdmin) ListDrafts(ctx context.Context, filter core.LearningDraftFilter) ([]core.LearningDraftView, error) {
	if a.store == nil {
		return nil, errors.New("学习查询：只读存储未装配")
	}
	views, err := a.store.ListDrafts(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("学习查询：列出草案: %w", err)
	}
	for i := range views {
		views[i] = a.fillDraftContent(ctx, views[i])
	}
	return views, nil
}

// LoadDraft 返回一份草案（元数据 + DRAFT.md 正文 + evidence.json 已在视图里）；
// 不存在返回 `core.ErrLearningDraftNotFound`。
// LoadDraft returns one draft; absence maps to ErrLearningDraftNotFound.
func (a *LearningAdmin) LoadDraft(ctx context.Context, draftID string) (core.LearningDraftView, error) {
	if a.store == nil {
		return core.LearningDraftView{}, errors.New("学习查询：只读存储未装配")
	}
	view, found, err := a.store.LoadDraft(ctx, draftID)
	if err != nil {
		return core.LearningDraftView{}, fmt.Errorf("学习查询：读取草案（%s）: %w", draftID, err)
	}
	if !found {
		return core.LearningDraftView{}, core.ErrLearningDraftNotFound
	}
	return a.fillDraftContent(ctx, view), nil
}

// fillDraftContent 从固定布局读取 DRAFT.md。读不到时只 WARN 并留空：DB 行存在而
// 文件缺失属于运维面异常（被手工删除 / 磁盘故障），不应让整个列表或详情 500；
// 孤儿计数的方向相反（有目录无行），两者都不自动修补。
// fillDraftContent reads the fixed-layout DRAFT.md; a missing file only warns and
// leaves the body empty so one damaged row cannot take down the whole read face.
func (a *LearningAdmin) fillDraftContent(ctx context.Context, view core.LearningDraftView) core.LearningDraftView {
	if a.draftRoot == "" || view.DraftID == "" {
		return view
	}
	// DraftID 来自数据库，但路径只允许由它拼出**单层**目录名：任何含分隔符或
	// 上跳成分的取值都不读盘（防御性投影，正常行恒为 `dft_` + base32）。`.` 与
	// `..` 经 filepath.Base 后原样返回，必须显式拒绝，否则 Join 会跳出草案根。
	// The id must be a single path element; "." and ".." survive filepath.Base and are
	// therefore refused explicitly — Join would otherwise escape the drafts root.
	if view.DraftID == "." || view.DraftID == ".." ||
		filepath.Base(view.DraftID) != view.DraftID || strings.ContainsAny(view.DraftID, `/\`) {
		a.logger.WarnContext(ctx, "学习查询：草案标识不是合法目录名，跳过正文读取", "draft_id", view.DraftID)
		return view
	}
	path := filepath.Join(a.draftRoot, view.DraftID, "DRAFT.md")
	// #nosec G304 -- 目录名已在上方拒绝分隔符与上跳（必须是单层路径元素），文件名是
	// 固定常量，根目录来自部署配置；这里只读，不写也不执行。
	raw, err := os.ReadFile(path)
	if err != nil {
		// 日志只带标识，不带路径与正文（C-30 同一口径）。
		a.logger.WarnContext(ctx, "学习查询：草案正文不可读，按空正文返回", "draft_id", view.DraftID)
		return view
	}
	view.Content = string(raw)
	return view
}

// ─────────────────────── 方法草案转交（handoff，C-19） ───────────────────────

// handoff 强制写入来源声明的三个固定字面量：管理员提供的来源文件只能声明
// 「哪些事实」（任务结果、授权、脱敏、备注、证据），**不能**自选来源类型或引用。
// 这三个值是 US-21 准入契约里已有的枚举取值，不新增动作或类型。
// The three fixed source-declaration literals the handoff forces; they are existing
// admission-contract enum values, not new literals.
const (
	learningDraftRefPrefix                 = "learning/draft/"
	learningHandoffSourceKind              = "model_proposal"
	learningHandoffConfirmationUserConfirm = "user_confirmed"
)

// learningHandoffDrafts 是 handoff 唯一的草案读取面：只查 DRAFT 行是否存在并取元数据。
// handoff 不读、不改、不删草案文件——文件布局与只读性归 internal/memory。
// The handoff reads draft metadata only; the files stay owned by internal/memory.
type learningHandoffDrafts interface {
	LoadDraft(ctx context.Context, draftID string) (core.LearningDraftView, bool, error)
}

// learningHandoffAdmission 是 US-21 准入编排面的完整写动作集合。**声明全部七个动作**
// 是刻意的：handoff 只允许调用 Propose（C-19），用例据此对「只调 Propose」做可判读的
// 反向断言——若实现路径直接调用评估 / 批准 / 发布，计数器立刻非零。
// The full seven-action surface is declared on purpose so "Propose only" is assertable.
type learningHandoffAdmission interface {
	Propose(ctx context.Context, actor core.Actor, req core.AdmissionProposeRequest) (core.AdmissionCandidate, error)
	Evaluate(ctx context.Context, actor core.Actor, candidateID string,
		policy core.AdmissionPolicy) (core.AdmissionEvaluation, error)
	Approve(ctx context.Context, actor core.Actor, candidateID string, decisionJSON []byte,
		policy core.AdmissionPolicy) (core.AdmissionDecision, error)
	Reject(ctx context.Context, actor core.Actor, candidateID string,
		decisionJSON []byte) (core.AdmissionDecision, error)
	Cancel(ctx context.Context, actor core.Actor, candidateID, reason string) (core.AdmissionCandidate, error)
	Publish(ctx context.Context, actor core.Actor, candidateID string,
		policy core.AdmissionPolicy) (core.AdmissionPublication, error)
	Withdraw(ctx context.Context, actor core.Actor, candidateID, reason string) (core.AdmissionPublication, error)
}

// LearningHandoffDeps 是转交薄适配的装配输入。
// LearningHandoffDeps wires the thin handoff adapter.
type LearningHandoffDeps struct {
	// Drafts / Admission 为 nil 时 handoff 失败关闭（拒绝执行，**不**降级为无检查直通）。
	Drafts    learningHandoffDrafts
	Admission learningHandoffAdmission
	// Root 是工作区根（`<WorkDir>/.basalt`），实例锁文件位于其下。
	Root string
	// AcquireLock 必须由装配层注入实例锁获取器（`workspace.AcquireInstanceLock` 的适配）；
	// nil 或取锁失败一律拒绝——handoff 是写命令，不允许在服务运行时并发写准入。
	// AcquireLock is required; a nil acquires nothing and the handoff refuses to run.
	AcquireLock func(root string) (release func() error, err error)
	Logger      *slog.Logger
}

// LearningHandoff 是 `learning handoff` 的薄适配（C-19 / §2.3.7）。请求与回执类型
// 在 `core`（`core.LearningHandoffRequest` / `core.LearningHandoffResult`）：命令面
// 只认 core，本层实现同一形状。
//
//  1. 主体必须是可信本机 CLI（`cli:<OS user>`，非 unknown）——复用准入的同一判据；
//  2. 来源声明严格解码，`confirmation` 必须是 `user_confirmed`；
//  3. 程序强制 `kind=model_proposal` 与 `ref=learning/draft/<draft-id>`，其余字段
//     保留管理员填写的值（任务结果、授权、脱敏、证据等）；
//  4. 取实例锁后再读草案、调用既有 `AdmissionAdmin.Propose`——**不**直接评估、
//     批准、发布或撤回，也不写任何自己的审计（propose 的预写与终态归 US-21）；
//  5. 草案不存在即拒绝（`ErrLearningDraftNotFound`），不凭手写 ID 造来源引用。
//
// 前两项是纯校验，先于取锁执行（与 CLI 写命令「身份 / 参数先于锁与开库」同一纪律）。
// The first two checks are pure validation and run before the lock, mirroring the CLI
// write-command order; the lock then covers the draft read and the Propose call.
type LearningHandoff struct {
	drafts      learningHandoffDrafts
	admission   learningHandoffAdmission
	root        string
	acquireLock func(root string) (release func() error, err error)
	logger      *slog.Logger
}

// NewLearningHandoff 装配转交薄适配；Logger 为 nil 时回落进程默认值。
// NewLearningHandoff assembles the adapter; a nil logger falls back to the default.
func NewLearningHandoff(deps LearningHandoffDeps) *LearningHandoff {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &LearningHandoff{
		drafts:      deps.Drafts,
		admission:   deps.Admission,
		root:        deps.Root,
		acquireLock: deps.AcquireLock,
		logger:      logger,
	}
}

// Handoff 执行一次受控转交；成功时只返回草案元数据与 propose 回执。
// Handoff runs one controlled handoff and returns only the draft metadata and receipt.
func (h *LearningHandoff) Handoff(ctx context.Context, req core.LearningHandoffRequest,
) (result core.LearningHandoffResult, err error) {
	if err := checkTrustedActor(req.Actor); err != nil {
		h.logger.WarnContext(ctx, "学习转交：操作主体不是可信本机 CLI，拒绝", "draft_id", req.DraftID)
		return core.LearningHandoffResult{}, err
	}
	draftID := strings.TrimSpace(req.DraftID)
	if !strings.HasPrefix(draftID, memory.LearningDraftIDPrefix) {
		return core.LearningHandoffResult{}, fmt.Errorf("%w: 草案标识必须以 %s 开头",
			core.ErrAdmissionInvalidInput, memory.LearningDraftIDPrefix)
	}
	source, err := core.ParseAdmissionSource(req.SourceJSON)
	if err != nil {
		// 原错误已点名字段与约束，且不含声明正文（严格解码不落日志）。
		return core.LearningHandoffResult{}, err
	}
	if source.Confirmation != learningHandoffConfirmationUserConfirm {
		h.logger.WarnContext(ctx, "学习转交：来源声明不是用户确认，拒绝",
			"draft_id", draftID, "confirmation", source.Confirmation)
		return core.LearningHandoffResult{}, fmt.Errorf(
			"%w: 来源声明 confirmation 必须是 %s（当前 %q）",
			core.ErrAdmissionInvalidInput, learningHandoffConfirmationUserConfirm, source.Confirmation)
	}
	// 强制两个来源字段：管理员文件里的取值被覆盖，不参与任何后续判定。
	// The two source fields are overwritten; the admin-provided values never survive.
	source.Kind = learningHandoffSourceKind
	source.Ref = learningDraftRefPrefix + draftID
	forcedSource, err := json.Marshal(source)
	if err != nil {
		return core.LearningHandoffResult{}, fmt.Errorf("来源声明重编码失败: %w", err)
	}

	if h.drafts == nil || h.admission == nil {
		return core.LearningHandoffResult{}, errors.New("学习转交：草案读取面或准入入口未装配，拒绝执行")
	}
	if h.acquireLock == nil {
		return core.LearningHandoffResult{}, errors.New("学习转交：实例锁端口未装配，拒绝执行")
	}
	release, err := h.acquireLock(h.root)
	if err != nil {
		// 锁被占用原样上抛：结论是「现在不能写」，不是「转交失败」。
		h.logger.WarnContext(ctx, "学习转交：实例锁不可得，拒绝", "draft_id", draftID)
		return core.LearningHandoffResult{}, err
	}
	defer func() {
		if relErr := release(); relErr != nil {
			h.logger.ErrorContext(ctx, "学习转交：释放实例锁失败", "draft_id", draftID, "error", relErr)
			err = errors.Join(err, relErr)
		}
	}()

	draft, found, err := h.drafts.LoadDraft(ctx, draftID)
	if err != nil {
		return core.LearningHandoffResult{}, fmt.Errorf("学习转交：读取草案（%s）: %w", draftID, err)
	}
	if !found {
		return core.LearningHandoffResult{}, fmt.Errorf("%w: %s", core.ErrLearningDraftNotFound, draftID)
	}
	candidate, err := h.admission.Propose(ctx, req.Actor, core.AdmissionProposeRequest{
		SkillName:  req.SkillName,
		Files:      req.Files,
		SourceJSON: forcedSource,
	})
	if err != nil {
		return core.LearningHandoffResult{}, err
	}
	return core.LearningHandoffResult{Draft: draft, Candidate: candidate}, nil
}

// 编译期断言：生产装配必须满足转交面；签名漂移在编译期先红。
// Compile-time assertions that the production assemblies satisfy the handoff surfaces.
var _ learningHandoffAdmission = (*AdmissionAdmin)(nil)

// countOrphanDrafts 统计「有目录、无 DB 行」的草案目录数；只报告，不删除、不修补。
// 目录扫描只认 `dft_` 前缀的**直接子目录**（符号链接不算目录，自动跳过）。
// countOrphanDrafts counts draft directories without a database row; it reports
// only and never deletes. Symlinks are not directories and are skipped.
func (a *LearningAdmin) countOrphanDrafts(ctx context.Context) (int, error) {
	if a.draftRoot == "" || a.store == nil {
		return 0, nil
	}
	entries, err := os.ReadDir(a.draftRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	orphans := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), memory.LearningDraftIDPrefix) {
			continue
		}
		_, found, err := a.store.LoadDraft(ctx, entry.Name())
		if err != nil {
			return 0, err
		}
		if !found {
			orphans++
		}
	}
	return orphans, nil
}
