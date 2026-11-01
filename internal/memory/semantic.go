package memory

// 本文件是**语义记忆存储**（颗粒度文档 §3.1.4 / 设计图 5）：在既有门面之上加语义召回与
// Run 冻结两个端口，三方法兼容面逐字保留。载体仍经门面承载——生效层回退与 per-scope
// 写锁**只有** `Service` 那一份实现，本类型不复制、也不再判一次。
// This file adds semantic recall and the run-start freeze on top of the existing facade;
// the compat three-method surface is forwarded verbatim and the effective-layer fallback
// stays in Service, the single implementation.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// RecallModeSemantic / RecallModeKeyword 是 `recall_memory` 的两种召回模式（P-07）：
// 缺省（空串）= 语义召回；`keyword` = 旧的整串子串行为，逐字保留。
// The two recall modes; the empty string means semantic, "keyword" keeps the old behavior.
const (
	// RecallModeSemantic 是缺省模式：query 经 embedding 后在归档区做向量召回。
	RecallModeSemantic = "semantic"
	// RecallModeKeyword 是兼容模式：整串子串、区分大小写、按写入顺序（旧行为逐字保留）。
	RecallModeKeyword = "keyword"
)

// 装配与依赖缺失一律 fail-closed：**不得**静默退化成"无记忆"或关键词路径——那会把
// 装配故障藏进一次看起来正常、实际丢召回的运行里。
// Missing assembly or a missing embedder fails closed; neither silently degrades.
var (
	errSemanticNotAssembled = errors.New("memory: semantic store is not assembled")
	errEmbedderNotAssembled = errors.New("memory: semantic recall requires an embedder")
)

// IsNotAssembled 判定错误是否表示**语义路径未装配**（缺语义载体或缺 embedding 连接）。
// 装配层据此把两种内部错误收敛成消费方可判的哨兵，工具层再翻译成可自我纠正的
// `invalid_input`——两边都**不得**比对错误文本。哨兵本身不导出：装配细节不进公共面。
// IsNotAssembled classifies the two unassembled states without exporting the sentinels.
func IsNotAssembled(err error) bool {
	return errors.Is(err, errSemanticNotAssembled) || errors.Is(err, errEmbedderNotAssembled)
}

// EmbedBatch 是一次 embedding 调用的回执——与 `internal/knowledge` 的同名形状逐字段
// 一致；本包**不得** import knowledge（依赖方向），故在此声明同一形状而不是复用。
// EmbedBatch mirrors knowledge's shape without importing that package.
type EmbedBatch struct {
	// Vectors 与输入文本一一对应，顺序与长度都必须匹配；调用方逐条校验。
	Vectors [][]float32
	// PromptTokens 是这次调用的输入 Token 数（配额结算用，本包只透传）。
	PromptTokens int64
	// Dimensions 是向量维度（0 = 未声明）。
	Dimensions int
	// Model 是产出这批向量的模型标识，随召回查询交给载体选代际（T052）。
	Model string
}

// Embedder 是查询侧 embedding 的**消费方接口**（本包声明）：实现由 app 装配，避免
// `memory` → `knowledge` 的反向依赖（§3.1.4）。
// Embedder is declared here to keep memory from importing knowledge.
type Embedder interface {
	// Embed 对一批文本产出向量；返回的 Vectors 必须与 texts 等长。
	Embed(ctx context.Context, texts []string) (EmbedBatch, error)
}

// MemoryEntry 是一条记忆条目（召回结果与生命周期动作的公共形状）。
// MemoryEntry is one memory row, shared by recall results and lifecycle actions.
type MemoryEntry struct {
	// ID 是条目主键（载体分配；0 = 尚未落库，只出现在构造侧）。
	ID int64
	// Scope 是**分层**（哪个 Agent 的记忆；零值 = 全局层）。
	Scope core.Scope
	// Section 是**分区**（核心区 / 归档区）；召回只返回归档区。
	Section core.Section
	// Content 是条目正文（已规范化，无载体添加物）。
	Content string
	// State 是六态生命周期取值（active / candidate / superseded / corrected /
	// deleted / conflicted——契约字面量，**没有** expired；到期由 valid_to 表达）；
	// 状态迁移由本包生命周期方法维护（lifecycle.go）。
	State string
	// Score 是召回名次分（keyword 模式恒 0；语义模式由载体给出）。
	Score float64
	// SupersedesID 指向被本条替代的旧条目（0 = 无）。
	SupersedesID int64
	// CreatedAt 是写入时刻（UTC）。
	CreatedAt time.Time

	// source 是写入来源（`memory_entries.source` 闭集）。**故意不导出**：普通写入
	// （`Append` / 生命周期动作）没有任何来源入参，来源由存储层按状态推导；只有旧记忆
	// 导入、数据库迁移与存储回读三条受信任路径经 `WithSource` 赋值，因此调用方**无法**
	// 把普通写入伪装成 `legacy_import`。
	// source stays unexported so ordinary writes cannot masquerade as an import.
	source string

	// confirmedBy 是确认人（`memory_entries.confirmed_by` 审计列），与 source 同一条
	// 纪律：**故意不导出**，普通写入入口没有确认人入参——确认走 `UpdateEntryState`，
	// 纠正由本包生命周期方法在**包内**赋值（`CorrectRequest.ConfirmedBy` 是必填）；
	// 跨包的受信任路径（存储层回读与用例夹具）经 `WithConfirmedBy` 盖章。空串 =
	// 从未记录（候选、旧记忆导入行与既有数据）。
	// confirmedBy is unexported for the same reason as source: no ordinary write takes a
	// confirmer; only the lifecycle writers here and WithConfirmedBy can stamp it.
	confirmedBy string
}

// 来源字面量，与 `docs/contracts/schema.pg.sql` 的 `memory_entries.source` CHECK 闭集
// 逐字同源。前两个由普通写入按状态推导，后两个**只**由受信任路径赋值。
// The source literals mirror the schema's CHECK closure verbatim.
const (
	// SourceSaveMemory 是生效条目的普通写入来源。
	SourceSaveMemory = "save_memory"
	// SourceExplicitPropose 是候选条目的普通写入来源。
	SourceExplicitPropose = "explicit_propose"
	// SourceLegacyImport 是旧记忆一次性只读导入的来源。
	SourceLegacyImport = "legacy_import"
	// SourceMigration 是数据库迁移作业的来源。
	SourceMigration = "migration"
)

// Source 返回条目来源；空串表示"未指定，由存储层按状态推导"。
// Source returns the entry's source; empty means "derive it from the state".
func (e MemoryEntry) Source() string { return e.source }

// WithSource 给条目盖上来源戳。**只给受信任路径用**：旧记忆导入（`ImportLegacy`）、
// 数据库迁移作业，以及存储层回读时还原库里的值。普通写入路径**不得**调用它——Go 的
// 包边界挡不住同进程的误用，这条边界靠评审与用例守（`TestMemory_SourceIsNotCallerAssignable`
// 证明普通入口没有来源入参）。
// WithSource stamps a source for trusted paths only (import, migration, store read-back).
func WithSource(e MemoryEntry, source string) MemoryEntry {
	e.source = source
	return e
}

// ConfirmedBy 返回确认人；空串表示从未记录（候选、旧记忆导入行与既有数据）。
// ConfirmedBy returns the recorded confirmer; empty means none was ever recorded.
func (e MemoryEntry) ConfirmedBy() string { return e.confirmedBy }

// WithConfirmedBy 给条目盖上确认人。**只给受信任路径用**：管理面确认走
// `UpdateEntryState`，纠正由生命周期方法在包内赋值，存储层回读与用例夹具经这里盖章。
// 普通写入入口**不得**多出确认人参数——否则"谁确认的"会退化成调用方可随手填的字段，
// 与 `WithSource` 守卫的来源列同一类完整性要求。
// WithConfirmedBy stamps the confirmer on trusted paths only (read-back, test fixtures);
// ordinary write entry points must not grow a confirmer parameter.
func WithConfirmedBy(e MemoryEntry, who string) MemoryEntry {
	e.confirmedBy = who
	return e
}

// RecallQuery 是 `Recall` 的入参：身份是**显式字段**（不藏进 context，也不默认成
// 管理员），每次调用都按它重新判定权限（FR-015）。
// RecallQuery carries the principal explicitly; every call re-checks permissions.
type RecallQuery struct {
	// Principal 是本次调用的调用方身份；零值 / 无效值一律拒绝（C-28）。
	Principal core.Principal
	// AuthorizationAgent identifies the invoking Agent even for global scope.
	AuthorizationAgent string
	// Scope 是检索的记忆分层。
	Scope core.Scope
	// Query 是检索文本；keyword 模式按整串子串匹配，语义模式送去 embedding。
	Query string
	// Mode 取 RecallModeSemantic / RecallModeKeyword；空串 = 语义（契约变更，P-07）。
	Mode string
	// Limit 是返回条数上限（<= 0 表示不截断）。
	Limit int
}

// MemoryAccessChecker checks live caller and key state before sharing memory.
type MemoryAccessChecker interface {
	AuthorizeMemory(ctx context.Context, principal core.Principal, agent string) error
}

// MemoryStore 是记忆条目的**窄持久化端口**：兼容三方法原样继承，另加一条只查归档区
// 的语义召回。实现落在 `storage/postgres`（本包不 import 它）。
// MemoryStore is the narrow persistence port implemented by storage/postgres.
type MemoryStore interface {
	core.LongTermMemoryStore

	// RecallEntries 在归档区按向量（语义模式）或按 query（载体自行决定）召回条目；
	// vector 为 nil 表示没走 embedding（兼容路径不得依赖它）。
	// 权限判定在载体内按 query.Principal 执行——每次召回都重查，不复用上次结论。
	// 存储层**必须**只返回 state=active 且未到期的条目；层内还有一道 activeOnly 守点。
	RecallEntries(ctx context.Context, query RecallQuery, vector []float32, model string) ([]MemoryEntry, error)

	// LoadEntry 按主键读一条（生命周期动作的前置检查）；found=false 表示不存在。
	// LoadEntry reads one entry by id; found reports whether the row exists.
	LoadEntry(ctx context.Context, id int64) (entry MemoryEntry, found bool, err error)

	// FindEntryByContent 按（分层, 分区, 正文）查同源条目（提议去重）；正文比较用
	// 存储侧的规范化表示，调用方传**已规范化**的正文。
	// FindEntryByContent is the same-source lookup used for proposal merging.
	FindEntryByContent(ctx context.Context, scope core.Scope, section core.Section, content string) (entry MemoryEntry, found bool, err error)

	// InsertEntry 插入一条新条目并返回主键；state 由调用方给定（candidate / active /
	// conflicted），存储层**不得**自行改写。`ConfirmedBy()` 非空时写进 `confirmed_by`
	// （纠正的 conflicted 分支靠它记录确认人），空值落 NULL。
	// InsertEntry persists one row and returns its id; the store must not rewrite state
	// and writes the confirmer carried by the entry.
	InsertEntry(ctx context.Context, entry MemoryEntry) (int64, error)

	// UpdateEntryState 只迁移状态（可选记录 confirmed_by）；**不得**改动正文。
	// UpdateEntryState migrates state (and optionally confirmed_by) without touching content.
	UpdateEntryState(ctx context.Context, id int64, state string, confirmedBy string) error

	// ReplaceEntry 在一个事务内插入 next 并把 oldID 从 active 迁移为 retireState：纠正 /
	// 替代必须原子完成，**不得**出现两条 active 头。oldID 已不在 active（并发下被别的
	// 动作改过）时返回 core.ErrConflict 且不插入 next。新条的 `ConfirmedBy()` 必须落库
	// ——纠正出的 active 头要留下"谁担保了新正文"的数据级证据。
	// ReplaceEntry inserts next and retires the still-active oldID in one transaction;
	// a target that is no longer active returns core.ErrConflict without inserting, and
	// the new row records next's confirmer.
	ReplaceEntry(ctx context.Context, oldID int64, retireState string, next MemoryEntry) (int64, error)

	// ListEntries 按分层列出条目（六态全集，主键倒序），供管理面读模型使用。
	// 它是**读面**而不是召回面：不过滤状态、不排序相关性、不碰 embedding——候选与
	// 已删条目照常返回，页面才有可确认 / 可删除的对象。
	// ListEntries lists rows for the management read model; it is not a recall surface
	// and deliberately returns candidates and deleted rows too.
	ListEntries(ctx context.Context, query ListQuery) ([]MemoryEntry, error)
}

// ListQuery 是条目列表的入参。排序与窗口由存储层固定（`id DESC`），调用方只给过滤值。
// ListQuery carries the list filters; ordering stays fixed in storage.
type ListQuery struct {
	// Scope 是必给的过滤分层（零值 = 全局层，不是"不过滤"）。
	Scope core.Scope
	// State 是可选状态过滤（六态字面量之一）；空串 = 六态全列，域外取值由调用方拒绝。
	State string
	// Limit 是返回条数上限（<= 0 表示不截断）。
	Limit int
}

// FreezeRequest 是 Run 开始冻结注入文本的请求。冻结读的是
// 生效层核心区文本，不是按身份过滤的召回结果（设计图 5）。
// FreezeRequest carries the run's principal because the freeze may read the global layer.
type FreezeRequest struct {
	// RunID 标识这次 Run（快照按 Run 归属）。
	RunID string
	// Scope 是 Run 的记忆分层（Agent 名；零值 = 全局层）。
	Scope core.Scope
	// Principal 是本次 Run 的实际身份：冻结走的是 `LoadEffective` 的同一条回退链，
	// **全局记忆参与读取**时（Agent 层为空而回退全局层 / 无 Agent 上下文）必须按
	// 身份、Agent scope 与 Key 当前状态复核。零值会被拒绝，不是「匿名读」。
	// Principal is the run's identity; the freeze shares LoadEffective's fallback chain,
	// so reading the global layer is re-authorized. A zero principal is refused.
	Principal core.Principal
	// Now 是冻结时刻（调用方注入时钟，本包不读全局时间）。
	Now time.Time
}

// FreezeSnapshot 是冻结结果：`MemoryText` 逐字等于该 Run 的段 2 注入文本。
// FreezeSnapshot is the frozen injection text for one run.
type FreezeSnapshot struct {
	// RunID 回显请求里的 Run 标识。
	RunID string
	// MemoryText 是生效层核心区文本；空层为 `""`（FR-018 逐字节基线的前提）。
	MemoryText string
	// FrozenAt 回显请求里的冻结时刻。
	FrozenAt time.Time
}

// SemanticDeps 是语义记忆存储的装配面（字段即依赖，无包级状态、无 context 取值）。
// SemanticDeps is the assembly surface; every dependency is an explicit field.
type SemanticDeps struct {
	// Store 是窄持久化端口——**必填**（缺装配即 fail-closed）。
	Store MemoryStore
	// Embedder 是查询侧 embedding 客户端——语义模式必填，keyword 模式不用。
	Embedder Embedder
}

// SemanticStore 是语义记忆的**唯一入口**：三方法兼容面 + `Recall` + `Freeze`。
// 写锁与生效层回退都经内部门面（与 US2/US4 交付的是同一份实现）。
// SemanticStore is the single semantic entry, forwarding the compat surface through the
// facade so the write lock and the fallback judgement stay single-sourced.
type SemanticStore struct {
	deps   SemanticDeps
	facade *Service
	// initErr 记录装配失败（设计签名不带 error，故把失败存进实例并让每次调用拒绝）。
	initErr error
}

// 编译期断言：兼容三方法逐字保留（`core.LongTermMemoryStore` 仍是唯一读写形状）。
var _ core.LongTermMemoryStore = (*SemanticStore)(nil)

// NewSemanticStore 组装语义记忆存储。签名按设计不带 error 返回，因此装配失败**不 panic**
// 也不静默降级：失败原因存进实例，任何一次调用都返回该错误（fail-closed）。
// Assembly failure is recorded and reported by every method rather than panicking or
// silently degrading.
func NewSemanticStore(deps SemanticDeps) *SemanticStore {
	s := &SemanticStore{deps: deps}
	if deps.Store == nil {
		s.initErr = errSemanticNotAssembled
		return s
	}
	facade, err := NewService(Options{Backend: deps.Store})
	if err != nil {
		s.initErr = err
		return s
	}
	s.facade = facade
	return s
}

// ready 是每个入口的第一道检查：装配未完成就不触碰任何载体。
func (s *SemanticStore) ready() error {
	if s.initErr != nil {
		return s.initErr
	}
	if s.facade == nil {
		return errSemanticNotAssembled
	}
	return nil
}

// Append 转发到门面：per-scope 写锁与载体配额判定都留在原有实现里（不复制）。
// Append forwards to the facade so the per-scope write lock stays single-sourced.
func (s *SemanticStore) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error {
	if err := s.ready(); err != nil {
		return err
	}
	return s.facade.Append(ctx, scope, section, content)
}

// Load 转发到门面：返回该层原文（管理面 / 巡检用；注入面走 Freeze）。
// Load forwards to the facade and returns the raw layer text.
func (s *SemanticStore) Load(ctx context.Context, scope core.Scope) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	return s.facade.Load(ctx, scope)
}

// RecallByKeyword 转发到门面：整串子串检索的旧行为逐字保留（`mode=keyword` 的底座）。
// RecallByKeyword forwards to the facade and keeps the whole-string behavior verbatim.
func (s *SemanticStore) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	return s.facade.RecallByKeyword(ctx, scope, query)
}

// Recall 是 `recall_memory` 的语义召回入口（P-07）。两条路径：
//   - 缺省 / `semantic`：query 经 Embedder 产出一个向量，交载体在归档区召回；
//   - `keyword`：走兼容面，与旧工具行为逐字一致（**不**调用 Embedder）。
//
// 零值 / 无效 Principal 在触碰载体与 Embedder 之前就拒绝（C-28）——身份缺失不得默认
// 成管理员，也不得先花一次 embedding 再拒绝。
// Recall rejects an unusable principal before touching the store or the embedder; the
// keyword path never calls the embedder and keeps the legacy behavior verbatim.
func (s *SemanticStore) Recall(ctx context.Context, query RecallQuery) ([]MemoryEntry, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if !query.Principal.Valid() {
		return nil, core.ErrUnauthorized
	}
	if query.Scope.Agent != "" && !query.Principal.HasAgentScope(query.Scope.Agent) {
		return nil, core.ErrForbidden
	}
	if query.AuthorizationAgent != "" {
		if !query.Principal.HasAgentScope(query.AuthorizationAgent) {
			return nil, core.ErrForbidden
		}
		if query.Principal.Kind == core.PrincipalCaller {
			checker, ok := s.deps.Store.(MemoryAccessChecker)
			if !ok {
				return nil, core.ErrForbidden
			}
			if err := checker.AuthorizeMemory(ctx, query.Principal, query.AuthorizationAgent); err != nil {
				return nil, err
			}
		}
	}

	mode := query.Mode
	if mode == "" {
		mode = RecallModeSemantic
	}
	switch mode {
	case RecallModeKeyword:
		hits, err := s.facade.RecallByKeyword(ctx, query.Scope, query.Query)
		if err != nil {
			return nil, err
		}
		entries := make([]MemoryEntry, 0, len(hits))
		for _, content := range hits {
			entries = append(entries, MemoryEntry{
				Scope:   query.Scope,
				Section: core.SectionArchive,
				Content: content,
				// 旧载体没有状态列：能返回的就是生效条目，按 active 标注（C-21 的
				// 候选不参与召回在 markdown 档天然成立）。
				State: StateActive,
			})
		}
		return limitEntries(entries, query.Limit), nil

	case RecallModeSemantic:
		if s.deps.Embedder == nil {
			return nil, errEmbedderNotAssembled
		}
		batch, err := s.deps.Embedder.Embed(ctx, []string{query.Query})
		if err != nil {
			return nil, err
		}
		if len(batch.Vectors) != 1 {
			return nil, fmt.Errorf("memory: embedder returned %d vectors for 1 input", len(batch.Vectors))
		}
		entries, err := s.deps.Store.RecallEntries(ctx, query, batch.Vectors[0], batch.Model)
		if err != nil {
			return nil, err
		}
		return limitEntries(activeOnly(entries), query.Limit), nil

	default:
		return nil, fmt.Errorf("memory: unknown recall mode %q", mode)
	}
}

// KnownState 判定状态取值是否落在六态闭集内。管理面的 `state` 过滤参数与恢复查证都
// 用这一份判据，**不得**各写一条 switch——多一处就多一处漏判。
// KnownState reports whether the value is one of the six contract literals.
func KnownState(state string) bool {
	switch state {
	case StateCandidate, StateActive, StateSuperseded,
		StateCorrected, StateDeleted, StateConflicted:
		return true
	}
	return false
}

// ListEntries 是管理面读模型的唯一入口：身份先判（零值 / 无效一律拒绝，C-28），
// 状态过滤先校验闭集，再交给载体查询。它**不**做生效层过滤——候选与已删条目照常
// 返回，这是管理面与召回面的根本区别；召回面（`Recall`）仍然只出 active。
// ListEntries is the management read model: identity and state filter are checked before
// the store runs, and non-active rows are returned on purpose.
func (s *SemanticStore) ListEntries(ctx context.Context, principal core.Principal, query ListQuery) ([]MemoryEntry, error) {
	if err := s.ready(); err != nil {
		return nil, err
	}
	if !principal.Valid() {
		return nil, core.ErrUnauthorized
	}
	if query.State != "" && !KnownState(query.State) {
		return nil, fmt.Errorf("memory: unknown state %q", query.State)
	}
	return s.deps.Store.ListEntries(ctx, query)
}

// Freeze 在 Run 开始读一次生效层核心区文本并冻结（C-11 / FR-015）。
//
// 落地说明（与设计伪代码的**唯一偏离**，已在任务附录披露）：设计图 5 写 `s.Load(ctx,
// req.Scope)`，其语义是"与 FR-004/FR-005 同一生效层解析 + 核心区切分"；而兼容面
// `Load` 只读**指定层原文**（不做回退、不切分区）。若照着兼容面落地，冻结文本会与
// 实际注入文本不是同一份（多出归档区、或漏掉全局层回退）。因此这里调用门面的
// `LoadEffective`——它是生效层回退与核心区切分的**唯一**实现，冻结文本因此逐字节
// 等于注入文本。**不得**改成按 query 召回，也**不得**在 Run 内每轮重读。
// The pseudo-code's s.Load means "the same effective-layer resolution as FR-004/005";
// the compat Load reads one layer verbatim, so Freeze calls the facade's LoadEffective,
// the single implementation of that resolution, keeping the frozen text byte-identical
// to what is injected.
//
// 身份拒绝与读取失败同形态：返回错误、不产出快照（**不**降级成空文本——空冻结会让
// 一次越权读看起来像"这个 Agent 没有记忆"）。
// An authorization refusal is returned as an error and never degrades to an empty
// snapshot, which would disguise an unauthorized read as "this agent has no memory".
func (s *SemanticStore) Freeze(ctx context.Context, req FreezeRequest) (FreezeSnapshot, error) {
	if err := s.ready(); err != nil {
		return FreezeSnapshot{}, err
	}
	text, err := s.facade.LoadEffective(ctx, req.Principal, req.Scope.Agent)
	if err != nil {
		return FreezeSnapshot{}, err
	}
	return FreezeSnapshot{RunID: req.RunID, MemoryText: text, FrozenAt: req.Now}, nil
}

// limitEntries 按调用方给的上限截断（<= 0 不截断）；不改动入参切片。
func limitEntries(entries []MemoryEntry, limit int) []MemoryEntry {
	if limit <= 0 || len(entries) <= limit {
		return entries
	}
	return entries[:limit]
}
