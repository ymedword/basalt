package knowledge

// 本文件实现索引代际的建立、校验、激活与退役（US-16 §2.3 图 3 / C-09 / ADR-03 /
// T037）：**任一时刻至多一个 active 代际**，切换必须先校验，旧代际在同一事务内
// 退休而**不删除**（行与向量保留到回退窗口结束）。
//
// 三条纪律：
//  1. **校验是切换的前置**：`Validate` 产出证据，`Activate` 只认本包签发且未过期的
//     证据形状；没有证据的激活一律拒绝（`TestIndex_SwitchRequiresValidation` 的
//     单元层守点，集成层由 `index.go` 的 PG 实现再守一次）。
//  2. **切换是单事务**：退休旧 active 与激活目标由 `GenerationStore.ActivateGeneration`
//     一次完成——中间不存在"零 active"或"两个 active"的窗口；契约
//     `ux_kb_index_generations_active` 是数据库侧兜底，代码侧**不得**靠触发它再补救。
//  3. **退休不删除**：`retired` 行带 `retired_at` 且清空 `activated_at`（冻结 CHECK
//     要求 `(status = 'active') = (activated_at IS NOT NULL)`）；回退窗口内可重新
//     校验后切回，窗口结束后的清理不在本 US 范围。
//
// 与文档的两处实现层取舍（记入 tasks.md 附录 R47）：
//   - ADR-03 写 `building → validated → active`，但冻结 DDL 的 `status` 只有
//     building / active / retired 三值——`validated` 是**前置条件**而不是持久状态，
//     以契约为准，`Validated` 由 `GenerationValidation` 承载；
//   - 代际行本身就是权威记录（`status` + `activated_at` / `retired_at`），不写
//     `management_operations`（action 取值冻结，没有代际类）；失败路径同时落 WARN /
//     ERROR 日志（工程规范 §4 双轨口径的日志侧）。
//
// This file implements index-generation lifecycle: at most one active generation,
// activation gated on validation evidence, and a single transactional switch that
// retires (never deletes) the previous active generation.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

var (
	// ErrGenerationUnavailable 表示代际管理没有装配存储端口：请求在任何读写之前
	// 被拒绝，**不得**用默认代际把缺失配置补成可用。
	// ErrGenerationUnavailable reports a missing store port; nothing is read or written.
	ErrGenerationUnavailable = errors.New("knowledge: generation store is not configured")

	// ErrGenerationUnvalidated 表示激活没有可核对的校验证据：证据为零值、不是本包
	// 签发（`verified` 未置位）或校验本身没通过。**不得**因为"上次校验过"放行。
	// ErrGenerationUnvalidated reports missing or failed validation evidence.
	ErrGenerationUnvalidated = errors.New("knowledge: index generation has not passed validation")

	// ErrGenerationMismatch 表示 active 代际与请求的模型 / 维度 / 距离不一致。C-09：
	// 换模型**必须**先新建代际、校验通过再切换，**不得**在启动或请求路径里自动切换。
	// ErrGenerationMismatch reports an active generation that differs from the spec.
	ErrGenerationMismatch = errors.New("knowledge: active index generation does not match the requested spec")

	// ErrGenerationNotInitialized 表示库里没有 active 代际：只读固定（`Pin`）不做
	// 首代际兜底，必须由控制面先创建并激活（FR-25C-12）。与 `ErrGenerationMismatch`
	// 分开——一个是"从未初始化"，一个是"初始化过但配置已换"。
	// ErrGenerationNotInitialized reports that no active generation exists; only the
	// control plane may create and activate the first one.
	ErrGenerationNotInitialized = errors.New("knowledge: no active index generation is initialized")
)

// GenerationStatus 是 `kb_index_generations.status` 的三值闭集（契约冻结）。
// GenerationStatus is the frozen three-value status set of `kb_index_generations`.
type GenerationStatus string

const (
	// GenerationBuilding 表示代际在建：向量可写入，但对查询不可见。
	// GenerationBuilding accepts writes and is invisible to retrieval.
	GenerationBuilding GenerationStatus = "building"
	// GenerationActive 表示代际生效；至多一个，检索固定用它。
	// GenerationActive is the single visible generation.
	GenerationActive GenerationStatus = "active"
	// GenerationRetired 表示已退出：行与向量保留，等待回退窗口结束。
	// GenerationRetired keeps its rows and vectors for the rollback window.
	GenerationRetired GenerationStatus = "retired"
)

// valid 判定状态字面量是否属于冻结闭集。
// valid reports whether the literal belongs to the frozen set.
func (s GenerationStatus) valid() bool {
	switch s {
	case GenerationBuilding, GenerationActive, GenerationRetired:
		return true
	}
	return false
}

// GenerationDistance 是 `kb_index_generations.distance` 的三值闭集（契约冻结）。
// GenerationDistance is the frozen distance set; only cosine is in use for now.
type GenerationDistance string

const (
	// DistanceCosine 是当前使用的距离（ADR-03，`<=>`）。
	// DistanceCosine is the distance in use.
	DistanceCosine GenerationDistance = "cosine"
	// DistanceL2 是契约允许但本轮未评估的候选。
	// DistanceL2 is allowed by the contract but not evaluated yet.
	DistanceL2 GenerationDistance = "l2"
	// DistanceInnerProduct 是契约允许但本轮未评估的候选。
	// DistanceInnerProduct is allowed by the contract but not evaluated yet.
	DistanceInnerProduct GenerationDistance = "inner_product"
)

// valid 判定距离字面量是否属于冻结闭集。
// valid reports whether the literal belongs to the frozen set.
func (d GenerationDistance) valid() bool {
	switch d {
	case DistanceCosine, DistanceL2, DistanceInnerProduct:
		return true
	}
	return false
}

// IndexGeneration 是 `kb_index_generations` 的行视图。时间字段用行内原值，
// 零值表示该列在库里是 NULL（未激活 / 未退休）——**不得**用具名零时刻冒充。
// IndexGeneration mirrors one `kb_index_generations` row; zero times mean NULL.
type IndexGeneration struct {
	// Generation 是代际号，从 1 起。
	Generation int
	// EmbeddingModel 是产生该代际向量的模型 id。
	EmbeddingModel string
	// Dimensions 是向量维度；必须等于 `VectorDimensions` 才能被激活。
	Dimensions int
	// Distance 是距离度量。
	Distance GenerationDistance
	// Status 是当前状态。
	Status GenerationStatus
	// CreatedAt 是建行时刻。
	CreatedAt time.Time
	// ActivatedAt 是最近一次激活时刻；非 active 时为零值。
	ActivatedAt time.Time
	// RetiredAt 是退休时刻；非 retired 时为零值。
	RetiredAt time.Time
}

// GenerationSpec 是新建代际的标识三元组。距离留空按 cosine 处理，其余字段不留默认：
// 模型为空或维度不等于冻结列宽一律拒绝（**不得**替调用方"改成 512"）。
// GenerationSpec identifies a new generation; distance defaults to cosine.
type GenerationSpec struct {
	// EmbeddingModel 是模型 id，必填。
	EmbeddingModel string
	// Dimensions 是向量维度，必须等于 `VectorDimensions`。
	Dimensions int
	// Distance 是距离度量，空值按 cosine 处理。
	Distance GenerationDistance
}

// GenerationValidation 是 `Validate` 签发的校验证据：`verified` 未导出，包外无法
// 构造"通过"——`Activate` 据此拒绝"没校验就直接切"。
// GenerationValidation is the evidence issued by Validate; only this package can
// mark it verified, so Activate cannot be talked into switching unvalidated.
type GenerationValidation struct {
	// Generation 是被校验的代际号。
	Generation int
	// VectorCount 是校验时该代际的向量条数（0 表示空代际，仍可激活）。
	VectorCount int64
	// verified 只在 `Validate` 成功时置位。
	verified bool
}

// Passed 报告校验是否真的跑过并通过。零值一律为 false。
// Passed reports whether a real validation run succeeded; the zero value never does.
func (v GenerationValidation) Passed() bool { return v.verified }

// GenerationSwitch 是一次切换的结果：生效的代际与被退休的上一个 active 代际。
// GenerationSwitch reports the new active generation and the retired predecessor.
type GenerationSwitch struct {
	// Active 是切换后生效的代际（重新读回的行）。
	Active IndexGeneration
	// Retired 是被退休的代际号；之前没有 active 代际时为 0。
	Retired int
}

// GenerationVectorStats 是一个代际下的向量统计。`Mismatched` 是模型或维度与代际
// 不一致的条数——非零即校验失败：混入别的模型的向量会让单代际查询返回错向量。
// GenerationVectorStats counts the generation's vectors and how many disagree with
// its model or dimensions; any mismatch fails validation.
type GenerationVectorStats struct {
	// Total 是向量总条数。
	Total int64
	// Mismatched 是模型 / 维度与代际不一致的条数。
	Mismatched int64
}

// GenerationStore 是代际管理的持久化窄端口，由 `storage/postgres` 的 `index.go`
// 实现（T129）。实现必须保证：
//   - `CreateGeneration` 分配的代际号严格递增、从 1 起（`max(generation) + 1`）；
//   - `ActivateGeneration` 在**单个事务**内完成"退休当前 active → 激活目标"，并让
//     目标行的 `activated_at` 有值、`retired_at` 为空；旧 active 行反向——这是
//     冻结 CHECK 的要求，也是"任一时刻至多一个 active"的唯一写入点；
//   - `RetireGeneration` 置 `retired_at` 并清空 `activated_at`，**不删除**行；
//   - 所有时间入参按 Unix 毫秒 UTC 落库。
//
// GenerationStore is the persistence port; the switch must be one transaction and a
// retirement keeps the row for the rollback window.
type GenerationStore interface {
	// ActiveGeneration 返回当前 active 代际；没有时 ok 为 false。
	ActiveGeneration(ctx context.Context) (IndexGeneration, bool, error)
	// Generation 按代际号读行；不存在返回 `ErrNotFound`。
	Generation(ctx context.Context, generation int) (IndexGeneration, error)
	// CreateGeneration 建一行 `building` 代际并回读。
	CreateGeneration(ctx context.Context, spec GenerationSpec, at time.Time) (IndexGeneration, error)
	// GenerationVectorStats 统计该代际的向量总数与不一致条数。
	GenerationVectorStats(ctx context.Context, generation int) (GenerationVectorStats, error)
	// ActivateGeneration 单事务切换，返回被退休的代际号（无旧 active 时为 0）。
	ActivateGeneration(ctx context.Context, generation int, at time.Time) (int, error)
	// RetireGeneration 把非 active 代际置 retired 并保留行。
	RetireGeneration(ctx context.Context, generation int, at time.Time) error
}

// GenerationDeps 是代际管理的装配依赖；`Logger` / `Now` 留空用包内默认值，
// `Store` 缺失即视为装配错误（每个方法在任何读写之前返回 `ErrGenerationUnavailable`）。
// GenerationDeps carries the assembly dependencies; a missing store fails closed.
type GenerationDeps struct {
	// Store 是持久化端口。
	Store GenerationStore
	// Logger 是运行日志目标；缺省 `slog.Default()`（与 `Ingestor` 同口径）。
	Logger *slog.Logger
	// Now 是时钟；缺省 `time.Now`，测试注入固定时刻。
	Now func() time.Time
}

// GenerationManager 管理索引代际的生命周期：建立、校验、激活与退役。
// GenerationManager owns the index-generation lifecycle.
type GenerationManager struct {
	store  GenerationStore
	logger *slog.Logger
	now    func() time.Time
}

// NewGenerationManager 装配代际管理；nil 日志与时钟按包内默认值补齐。
// NewGenerationManager wires the manager with the package defaults.
func NewGenerationManager(deps GenerationDeps) *GenerationManager {
	manager := &GenerationManager{store: deps.Store, logger: deps.Logger, now: deps.Now}
	if manager.logger == nil {
		manager.logger = slog.Default()
	}
	if manager.now == nil {
		manager.now = time.Now
	}
	return manager
}

// Begin 建一个 `building` 代际：向量可写入但不生效。同一模型 / 维度的重复调用会
// 建出多个 building 行——废弃的代际由调用方显式 `Retire`，本方法**不**替调用方
// 猜"上次那个还没用完"。
// Begin creates a building generation; abandoned ones must be retired explicitly.
func (m *GenerationManager) Begin(ctx context.Context, spec GenerationSpec) (IndexGeneration, error) {
	if err := m.ready(); err != nil {
		return IndexGeneration{}, err
	}
	normalized, err := normalizeGenerationSpec(spec)
	if err != nil {
		m.logger.WarnContext(ctx, "index generation creation rejected",
			"operation", "generation.begin", "embedding_model", sanitizeField(spec.EmbeddingModel),
			"dimensions", spec.Dimensions, "error", err)
		return IndexGeneration{}, err
	}
	row, err := m.store.CreateGeneration(ctx, normalized, m.now())
	if err != nil {
		m.logger.ErrorContext(ctx, "index generation creation failed",
			"operation", "generation.begin", "embedding_model", normalized.EmbeddingModel,
			"dimensions", normalized.Dimensions, "error", err)
		return IndexGeneration{}, fmt.Errorf("knowledge: 建立索引代际失败: %w", err)
	}
	m.logger.InfoContext(ctx, "index generation created", generationAttrs(row)...)
	return row, nil
}

// Ensure 返回与 spec 一致的 active 代际；没有 active 代际时建首代际、校验并激活
// （C-09 允许的"无 active 代际时建首代际"）。已存在但模型 / 维度 / 距离不一致 →
// `ErrGenerationMismatch`：换模型**必须**走显式切换，启动路径**不得**自动切。
// Ensure bootstraps the first generation when none is active and refuses to switch
// silently when the active one differs from the configured spec.
func (m *GenerationManager) Ensure(ctx context.Context, spec GenerationSpec) (IndexGeneration, error) {
	if err := m.ready(); err != nil {
		return IndexGeneration{}, err
	}
	normalized, err := normalizeGenerationSpec(spec)
	if err != nil {
		return IndexGeneration{}, err
	}
	active, ok, err := m.store.ActiveGeneration(ctx)
	if err != nil {
		m.logger.ErrorContext(ctx, "index generation lookup failed",
			"operation", "generation.ensure", "error", err)
		return IndexGeneration{}, fmt.Errorf("knowledge: 读取 active 代际失败: %w", err)
	}
	if ok {
		if mismatch := specMismatch(normalized, active); mismatch != "" {
			err := fmt.Errorf("%w: active 代际 %d 与配置不一致（%s）——换模型 / 维度必须先新建代际、校验通过再切换",
				ErrGenerationMismatch, active.Generation, mismatch)
			m.logger.WarnContext(ctx, "index generation mismatch rejected",
				append(generationAttrs(active), "operation", "generation.ensure",
					"reason", "spec_mismatch", "error", err)...)
			return IndexGeneration{}, err
		}
		return active, nil
	}
	row, err := m.Begin(ctx, normalized)
	if err != nil {
		return IndexGeneration{}, err
	}
	evidence, err := m.Validate(ctx, row.Generation)
	if err != nil {
		return IndexGeneration{}, err
	}
	switched, err := m.Activate(ctx, evidence)
	if err != nil {
		return IndexGeneration{}, err
	}
	m.logger.InfoContext(ctx, "first index generation activated", generationAttrs(switched.Active)...)
	return switched.Active, nil
}

// Pin 是 `Ensure` 的**只读**对照：返回与 spec 一致的 active 代际，**绝不**建行、
// 激活或退休。Worker 的知识面必须走这里——它只读共享代际，初始化写入归控制面
// （FR-25C-12）。没有 active 代际时失败关闭为 `ErrGenerationNotInitialized` 并点名
// 先由控制面创建并激活；active 代际与配置不一致时仍按 `ErrGenerationMismatch`
// 拒绝，**不得**把它当成"没初始化"再就地补建。
// Pin is the read-only counterpart of Ensure: it never creates, activates or retires a
// generation, and a missing active row fails closed and names the control plane.
func (m *GenerationManager) Pin(ctx context.Context, spec GenerationSpec) (IndexGeneration, error) {
	if err := m.ready(); err != nil {
		return IndexGeneration{}, err
	}
	normalized, err := normalizeGenerationSpec(spec)
	if err != nil {
		return IndexGeneration{}, err
	}
	active, ok, err := m.store.ActiveGeneration(ctx)
	if err != nil {
		m.logger.ErrorContext(ctx, "index generation lookup failed",
			"operation", "generation.pin", "error", err)
		return IndexGeneration{}, fmt.Errorf("knowledge: 读取 active 代际失败: %w", err)
	}
	if !ok {
		err := fmt.Errorf("%w: 数据库中还没有 active 索引代际——请先由控制面（serve）创建并激活，Worker 不执行代际初始化",
			ErrGenerationNotInitialized)
		m.logger.WarnContext(ctx, "index generation pin rejected",
			"operation", "generation.pin", "reason", "not_initialized",
			"embedding_model", sanitizeField(normalized.EmbeddingModel),
			"dimensions", normalized.Dimensions)
		return IndexGeneration{}, err
	}
	if mismatch := specMismatch(normalized, active); mismatch != "" {
		err := fmt.Errorf("%w: active 代际 %d 与配置不一致（%s）——换模型 / 维度必须先新建代际、校验通过再切换",
			ErrGenerationMismatch, active.Generation, mismatch)
		m.logger.WarnContext(ctx, "index generation mismatch rejected",
			append(generationAttrs(active), "operation", "generation.pin",
				"reason", "spec_mismatch", "error", err)...)
		return IndexGeneration{}, err
	}
	return active, nil
}

// Validate 校验一个非 active 代际的向量一致性：状态必须是 `building` 或 `retired`、
// 维度必须等于冻结列宽、模型 / 维度不一致的向量条数必须为 0。空代际（0 条向量）
// 通过——首次启动时知识库本来就是空的，**不得**用"非空"卡住首代际。
// Validate checks a non-active generation; an empty index passes because a fresh
// installation starts empty.
func (m *GenerationManager) Validate(ctx context.Context, generation int) (GenerationValidation, error) {
	if err := m.ready(); err != nil {
		return GenerationValidation{}, err
	}
	row, err := m.store.Generation(ctx, generation)
	if err != nil {
		return GenerationValidation{}, err
	}
	if row.Status == GenerationActive {
		err := fmt.Errorf("%w: 代际 %d 已是 active，不需要校验；切换请先 Begin 新代际", ErrInvalidRequest, generation)
		m.logger.WarnContext(ctx, "index generation validation rejected",
			append(generationAttrs(row), "operation", "generation.validate",
				"reason", "already_active", "error", err)...)
		return GenerationValidation{}, err
	}
	if !row.Status.valid() {
		err := fmt.Errorf("%w: 代际 %d 的状态 %q 不在冻结闭集内", ErrInvalidRequest, generation, string(row.Status))
		m.logger.WarnContext(ctx, "index generation validation rejected",
			append(generationAttrs(row), "operation", "generation.validate",
				"reason", "unknown_status", "error", err)...)
		return GenerationValidation{}, err
	}
	if row.Dimensions != VectorDimensions {
		err := fmt.Errorf("%w: 代际 %d 的维度是 %d，冻结列宽要求 %d——换维度必须新表 + 版本化迁移",
			ErrInvalidRequest, generation, row.Dimensions, VectorDimensions)
		m.logger.WarnContext(ctx, "index generation validation rejected",
			append(generationAttrs(row), "operation", "generation.validate",
				"reason", "dimension_mismatch", "error", err)...)
		return GenerationValidation{}, err
	}
	stats, err := m.store.GenerationVectorStats(ctx, generation)
	if err != nil {
		m.logger.ErrorContext(ctx, "index generation validation failed",
			append(generationAttrs(row), "operation", "generation.validate", "error", err)...)
		return GenerationValidation{}, fmt.Errorf("knowledge: 统计代际向量失败: %w", err)
	}
	if stats.Mismatched > 0 {
		err := fmt.Errorf("%w: 代际 %d 有 %d 条向量的模型或维度与代际不一致（共 %d 条）",
			ErrGenerationUnvalidated, generation, stats.Mismatched, stats.Total)
		m.logger.WarnContext(ctx, "index generation validation rejected",
			append(generationAttrs(row), "operation", "generation.validate",
				"reason", "vector_mismatch", "vector_count", stats.Total,
				"mismatched_vectors", stats.Mismatched, "error", err)...)
		return GenerationValidation{}, err
	}
	m.logger.InfoContext(ctx, "index generation validated",
		append(generationAttrs(row), "operation", "generation.validate",
			"vector_count", stats.Total)...)
	return GenerationValidation{Generation: generation, VectorCount: stats.Total, verified: true}, nil
}

// Activate 用校验证据把代际置 active，旧 active 在同一事务内退休（行与向量保留）。
// 证据零值 / 未通过 → `ErrGenerationUnvalidated`；目标已是 active → 拒绝；
// 目标状态非法（非 building / retired）→ 拒绝。切换成功但回读失败时返回错误——
// 调用方重试会命中"已是 active"，不会造成二次切换。
// Activate switches with validation evidence; the previous active generation is
// retired in the same transaction and the row is kept.
func (m *GenerationManager) Activate(ctx context.Context, evidence GenerationValidation) (GenerationSwitch, error) {
	if err := m.ready(); err != nil {
		return GenerationSwitch{}, err
	}
	if !evidence.Passed() {
		err := fmt.Errorf("%w: 代际 %d 没有通过校验的证据", ErrGenerationUnvalidated, evidence.Generation)
		m.logger.WarnContext(ctx, "index generation activation rejected",
			"operation", "generation.activate", "generation", evidence.Generation,
			"reason", "not_validated", "error", err)
		return GenerationSwitch{}, err
	}
	row, err := m.store.Generation(ctx, evidence.Generation)
	if err != nil {
		return GenerationSwitch{}, err
	}
	if row.Status == GenerationActive {
		err := fmt.Errorf("%w: 代际 %d 已是 active", ErrInvalidRequest, row.Generation)
		m.logger.WarnContext(ctx, "index generation activation rejected",
			append(generationAttrs(row), "operation", "generation.activate",
				"reason", "already_active", "error", err)...)
		return GenerationSwitch{}, err
	}
	if row.Status != GenerationBuilding && row.Status != GenerationRetired {
		err := fmt.Errorf("%w: 代际 %d 的状态 %q 不能激活", ErrInvalidRequest, row.Generation, string(row.Status))
		m.logger.WarnContext(ctx, "index generation activation rejected",
			append(generationAttrs(row), "operation", "generation.activate",
				"reason", "invalid_transition", "error", err)...)
		return GenerationSwitch{}, err
	}
	if row.Dimensions != VectorDimensions {
		err := fmt.Errorf("%w: 代际 %d 的维度是 %d，冻结列宽要求 %d", ErrInvalidRequest, row.Generation, row.Dimensions, VectorDimensions)
		m.logger.WarnContext(ctx, "index generation activation rejected",
			append(generationAttrs(row), "operation", "generation.activate",
				"reason", "dimension_mismatch", "error", err)...)
		return GenerationSwitch{}, err
	}
	retired, err := m.store.ActivateGeneration(ctx, row.Generation, m.now())
	if err != nil {
		m.logger.ErrorContext(ctx, "index generation activation failed",
			append(generationAttrs(row), "operation", "generation.activate", "error", err)...)
		return GenerationSwitch{}, fmt.Errorf("knowledge: 切换索引代际失败: %w", err)
	}
	switched, err := m.store.Generation(ctx, row.Generation)
	if err != nil {
		m.logger.ErrorContext(ctx, "index generation activation completed but the row could not be read back",
			"operation", "generation.activate", "generation", row.Generation,
			"retired_generation", retired, "error", err)
		return GenerationSwitch{}, fmt.Errorf("knowledge: 代际 %d 已切换但回读失败: %w", row.Generation, err)
	}
	m.logger.InfoContext(ctx, "index generation activated",
		append(generationAttrs(switched), "operation", "generation.activate",
			"retired_generation", retired)...)
	return GenerationSwitch{Active: switched, Retired: retired}, nil
}

// Retire 放弃一个非 active 的代际：置 `retired` 并保留行与向量。已 retired 幂等
// 返回（重试不报错、不重复写）；**active 代际不得经此退休**——移开 active 只能走
// `Activate`，否则会出现"没有任何 active 代际"的窗口。
// Retire abandons a non-active generation and keeps the row; retiring the active
// generation is refused because only Activate may move the active pointer.
func (m *GenerationManager) Retire(ctx context.Context, generation int) (IndexGeneration, error) {
	if err := m.ready(); err != nil {
		return IndexGeneration{}, err
	}
	row, err := m.store.Generation(ctx, generation)
	if err != nil {
		return IndexGeneration{}, err
	}
	if row.Status == GenerationActive {
		err := fmt.Errorf("%w: 代际 %d 是 active，不能直接退休——请用新代际切换", ErrInvalidRequest, generation)
		m.logger.WarnContext(ctx, "index generation retirement rejected",
			append(generationAttrs(row), "operation", "generation.retire",
				"reason", "active_generation", "error", err)...)
		return IndexGeneration{}, err
	}
	if row.Status == GenerationRetired {
		m.logger.InfoContext(ctx, "index generation already retired",
			append(generationAttrs(row), "operation", "generation.retire", "reason", "already_retired")...)
		return row, nil
	}
	if err := m.store.RetireGeneration(ctx, generation, m.now()); err != nil {
		m.logger.ErrorContext(ctx, "index generation retirement failed",
			append(generationAttrs(row), "operation", "generation.retire", "error", err)...)
		return IndexGeneration{}, fmt.Errorf("knowledge: 退休索引代际失败: %w", err)
	}
	retired, err := m.store.Generation(ctx, generation)
	if err != nil {
		m.logger.ErrorContext(ctx, "index generation retired but the row could not be read back",
			"operation", "generation.retire", "generation", generation, "error", err)
		return IndexGeneration{}, fmt.Errorf("knowledge: 代际 %d 已退休但回读失败: %w", generation, err)
	}
	m.logger.InfoContext(ctx, "index generation retired", generationAttrs(retired)...)
	return retired, nil
}

// ready 判定存储端口是否已装配；零值管理器同样按"未装配"处理。
// ready reports whether the store port was wired.
func (m *GenerationManager) ready() error {
	if m == nil || m.store == nil {
		return ErrGenerationUnavailable
	}
	return nil
}

// normalizeGenerationSpec 校验并补默认距离：模型不得为空、维度必须等于冻结列宽、
// 距离必须属于闭集。**不得**把不合法的维度"修正"成 512——那会把配置错误伪装成
// 一次正常建代际。
// normalizeGenerationSpec validates the spec and defaults the distance; it never
// silently repairs a wrong dimension.
func normalizeGenerationSpec(spec GenerationSpec) (GenerationSpec, error) {
	spec.EmbeddingModel = strings.TrimSpace(spec.EmbeddingModel)
	if spec.EmbeddingModel == "" {
		return GenerationSpec{}, fmt.Errorf("%w: embedding 模型为空", ErrInvalidRequest)
	}
	if spec.Distance == "" {
		spec.Distance = DistanceCosine
	}
	if !spec.Distance.valid() {
		return GenerationSpec{}, fmt.Errorf("%w: 未知距离 %q", ErrInvalidRequest, string(spec.Distance))
	}
	if spec.Dimensions != VectorDimensions {
		return GenerationSpec{}, fmt.Errorf("%w: 代际维度是 %d，冻结列宽要求 %d——换维度必须新表 + 版本化迁移（§3.4.1）",
			ErrInvalidRequest, spec.Dimensions, VectorDimensions)
	}
	return spec, nil
}

// specMismatch 返回配置与 active 代际的第一处不一致（空串表示一致）。
// specMismatch names the first difference between the spec and the active row.
func specMismatch(spec GenerationSpec, active IndexGeneration) string {
	switch {
	case spec.EmbeddingModel != active.EmbeddingModel:
		return fmt.Sprintf("模型 %q != %q", spec.EmbeddingModel, active.EmbeddingModel)
	case spec.Dimensions != active.Dimensions:
		return fmt.Sprintf("维度 %d != %d", spec.Dimensions, active.Dimensions)
	case spec.Distance != active.Distance:
		return fmt.Sprintf("距离 %q != %q", string(spec.Distance), string(active.Distance))
	default:
		return ""
	}
}

// generationAttrs 按固定键名组装代际日志字段（工程规范 §4：键名固定、错误进字段）。
// generationAttrs builds the fixed-key log fields for one generation row.
func generationAttrs(row IndexGeneration) []any {
	return []any{
		"generation", row.Generation,
		"embedding_model", sanitizeField(row.EmbeddingModel),
		"dimensions", row.Dimensions,
		"distance", string(row.Distance),
		"status", string(row.Status),
	}
}
