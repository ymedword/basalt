package knowledge

// 本文件覆盖 T037 的索引代际生命周期：没有校验证据不得激活、切换时旧 active 在
// 同一事务内退休（行与向量保留）、任一时刻至多一个 active、退休不改行数、
// 首代际引导，以及"启动时不得静默换模型"。假存储照抄冻结约束——部分唯一索引
// （第二个 active 直接报唯一键冲突）与两条时间列 CHECK（active ⇔ activated_at，
// retired ⇔ retired_at）——否则测试只能证明假存储自己成立。
//
// This file covers the index-generation lifecycle: activation only with validation
// evidence, a single transactional switch, at most one active generation, rows kept
// after retirement, first-generation bootstrap, and the refusal to switch models
// implicitly at startup. The fake store mirrors the frozen unique index and the two
// timestamp CHECKs so the tests exercise the same invariants as PostgreSQL.

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGenerationStore 是 `GenerationStore` 的内存实现，按冻结契约建模：
// 至多一个 active（第二个 active 触发唯一键冲突）、状态与时间列成对、退休不删行。
// fakeGenerationStore models the frozen contract, including the partial unique index.
type fakeGenerationStore struct {
	rows  map[int]IndexGeneration
	stats map[int]GenerationVectorStats
	next  int
	reads int

	createErr   error
	statsErr    error
	activateErr error
	retireErr   error
	activeErr   error
	readFailAt  int

	createCalls   int
	activateCalls []int
	retireCalls   []int
	lastSpec      GenerationSpec
}

// newFakeGenerationStore 返回空的假存储，代际号从 1 起。
// newFakeGenerationStore returns an empty store whose first generation id is 1.
func newFakeGenerationStore() *fakeGenerationStore {
	return &fakeGenerationStore{
		rows:  map[int]IndexGeneration{},
		stats: map[int]GenerationVectorStats{},
		next:  1,
	}
}

// seedGeneration 直接写入一行，供"已有 active / retired 代际"的初始状态使用。
// seedGeneration injects a row to set up an existing state.
func (f *fakeGenerationStore) seedGeneration(id int, model string, dimensions int, status GenerationStatus) IndexGeneration {
	row := IndexGeneration{
		Generation:     id,
		EmbeddingModel: model,
		Dimensions:     dimensions,
		Distance:       DistanceCosine,
		Status:         status,
		CreatedAt:      time.UnixMilli(1_600_000_000_000).UTC(),
	}
	if status == GenerationActive {
		row.ActivatedAt = time.UnixMilli(1_600_000_000_000).UTC()
	}
	if status == GenerationRetired {
		row.RetiredAt = time.UnixMilli(1_600_000_000_000).UTC()
	}
	f.rows[id] = row
	if id >= f.next {
		f.next = id + 1
	}
	return row
}

// activeIDs 返回当前全部 active 代际号，用来断言"至多一个"。
// activeIDs lists the active generations so tests can assert the single-active rule.
func (f *fakeGenerationStore) activeIDs() []int {
	var ids []int
	for id, row := range f.rows {
		if row.Status == GenerationActive {
			ids = append(ids, id)
		}
	}
	return ids
}

// ActiveGeneration 返回当前 active 代际。
func (f *fakeGenerationStore) ActiveGeneration(context.Context) (IndexGeneration, bool, error) {
	if f.activeErr != nil {
		return IndexGeneration{}, false, f.activeErr
	}
	for _, row := range f.rows {
		if row.Status == GenerationActive {
			return row, true, nil
		}
	}
	return IndexGeneration{}, false, nil
}

// Generation 按代际号读行；`readFailAt` 命中第 N 次读取时返回注入错误。
func (f *fakeGenerationStore) Generation(_ context.Context, generation int) (IndexGeneration, error) {
	f.reads++
	if f.readFailAt > 0 && f.reads == f.readFailAt {
		return IndexGeneration{}, errors.New("injected read failure")
	}
	row, ok := f.rows[generation]
	if !ok {
		return IndexGeneration{}, ErrNotFound
	}
	return row, nil
}

// CreateGeneration 分配递增代际号并落一行 building。
func (f *fakeGenerationStore) CreateGeneration(_ context.Context, spec GenerationSpec, at time.Time) (IndexGeneration, error) {
	f.createCalls++
	f.lastSpec = spec
	if f.createErr != nil {
		return IndexGeneration{}, f.createErr
	}
	row := IndexGeneration{
		Generation:     f.next,
		EmbeddingModel: spec.EmbeddingModel,
		Dimensions:     spec.Dimensions,
		Distance:       spec.Distance,
		Status:         GenerationBuilding,
		CreatedAt:      at,
	}
	f.next++
	f.rows[row.Generation] = row
	return row, nil
}

// GenerationVectorStats 返回注入的统计值；行不存在按未找到处理。
func (f *fakeGenerationStore) GenerationVectorStats(_ context.Context, generation int) (GenerationVectorStats, error) {
	if f.statsErr != nil {
		return GenerationVectorStats{}, f.statsErr
	}
	if _, ok := f.rows[generation]; !ok {
		return GenerationVectorStats{}, ErrNotFound
	}
	return f.stats[generation], nil
}

// ActivateGeneration 单事务切换：先退休当前 active，再激活目标；目标已是 active
// 时按部分唯一索引报冲突（与 PG 同形，不是"顺手再激活一次"）。
func (f *fakeGenerationStore) ActivateGeneration(_ context.Context, generation int, at time.Time) (int, error) {
	f.activateCalls = append(f.activateCalls, generation)
	if f.activateErr != nil {
		return 0, f.activateErr
	}
	row, ok := f.rows[generation]
	if !ok {
		return 0, ErrNotFound
	}
	if row.Status == GenerationActive {
		return 0, errors.New(`duplicate key value violates unique constraint "ux_kb_index_generations_active"`)
	}
	retired := 0
	for id, active := range f.rows {
		if active.Status == GenerationActive {
			active.Status = GenerationRetired
			active.RetiredAt = at
			active.ActivatedAt = time.Time{}
			f.rows[id] = active
			retired = id
		}
	}
	row.Status = GenerationActive
	row.ActivatedAt = at
	row.RetiredAt = time.Time{}
	f.rows[generation] = row
	return retired, nil
}

// RetireGeneration 置 retired 并清空 activated_at；行保留。
func (f *fakeGenerationStore) RetireGeneration(_ context.Context, generation int, at time.Time) error {
	f.retireCalls = append(f.retireCalls, generation)
	if f.retireErr != nil {
		return f.retireErr
	}
	row, ok := f.rows[generation]
	if !ok {
		return ErrNotFound
	}
	if row.Status == GenerationActive {
		return errors.New("cannot retire the active generation")
	}
	row.Status = GenerationRetired
	row.RetiredAt = at
	row.ActivatedAt = time.Time{}
	f.rows[generation] = row
	return nil
}

// defaultSpec 是与冻结列宽一致的模型 / 维度组合。
// defaultSpec matches the frozen column width.
func defaultSpec() GenerationSpec {
	return GenerationSpec{EmbeddingModel: "text-embedding-v4", Dimensions: VectorDimensions}
}

// newGenerationHarness 装配固定时钟的受测管理器与捕获日志面。
// newGenerationHarness wires the manager with a fixed clock and a captured log surface.
func newGenerationHarness(t *testing.T) (*GenerationManager, *fakeGenerationStore, *bytes.Buffer) {
	t.Helper()
	logger, buf := captureLogger(t)
	store := newFakeGenerationStore()
	manager := NewGenerationManager(GenerationDeps{Store: store, Logger: logger, Now: fixedClock()})
	return manager, store, buf
}

// TestGeneration_ActivateRequiresValidation 守"校验先于切换"：零值证据（包外唯一
// 能构造出来的形状）一律拒绝，且**不得**发起切换——没有证据时存储的切换调用次数
// 必须为 0，代际保持 building。
// TestGeneration_ActivateRequiresValidation refuses to switch without evidence.
func TestGeneration_ActivateRequiresValidation(t *testing.T) {
	manager, store, buf := newGenerationHarness(t)
	ctx := context.Background()

	building, err := manager.Begin(ctx, defaultSpec())
	require.NoError(t, err)
	require.Equal(t, GenerationBuilding, building.Status)

	_, err = manager.Activate(ctx, GenerationValidation{Generation: building.Generation})
	require.ErrorIs(t, err, ErrGenerationUnvalidated)
	assert.Empty(t, store.activateCalls, "没有校验证据不得发起切换")
	assert.Equal(t, GenerationBuilding, store.rows[building.Generation].Status)
	assert.Empty(t, store.activeIDs())

	entry := requireLogEntry(t, buf, "index generation activation rejected")
	assert.Equal(t, "not_validated", entry["reason"])
	assert.Equal(t, "WARN", entry["level"])

	_, err = manager.Activate(ctx, GenerationValidation{})
	require.ErrorIs(t, err, ErrGenerationUnvalidated)
	assert.Len(t, store.activateCalls, 0, "零值证据同样不得发起切换")
}

// TestGeneration_ActivateRetiresPreviousActive 守"切换即退休、且只有一次写入"：
// 旧 active 在同一调用里变 retired（保留行、写 retired_at、清 activated_at），
// 新代际变 active，全程恰好一个 active。
// TestGeneration_ActivateRetiresPreviousActive keeps exactly one active generation.
func TestGeneration_ActivateRetiresPreviousActive(t *testing.T) {
	manager, store, buf := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(1, "text-embedding-v4", VectorDimensions, GenerationActive)
	building, err := manager.Begin(ctx, GenerationSpec{EmbeddingModel: "text-embedding-v5", Dimensions: VectorDimensions})
	require.NoError(t, err)
	store.stats[building.Generation] = GenerationVectorStats{Total: 42}

	evidence, err := manager.Validate(ctx, building.Generation)
	require.NoError(t, err)
	require.True(t, evidence.Passed())
	assert.Equal(t, int64(42), evidence.VectorCount)

	switched, err := manager.Activate(ctx, evidence)
	require.NoError(t, err)
	assert.Equal(t, 1, switched.Retired)
	assert.Equal(t, building.Generation, switched.Active.Generation)
	assert.Equal(t, GenerationActive, switched.Active.Status)
	assert.Equal(t, fixedClock()(), switched.Active.ActivatedAt)
	assert.Len(t, store.activateCalls, 1, "切换必须是一次原子调用，而不是先退休再激活两步")

	assert.Equal(t, []int{building.Generation}, store.activeIDs(), "任一时刻至多一个 active")
	old := store.rows[1]
	assert.Equal(t, GenerationRetired, old.Status)
	assert.Equal(t, fixedClock()(), old.RetiredAt)
	assert.True(t, old.ActivatedAt.IsZero(), "退休行必须清空 activated_at（冻结 CHECK）")

	entry := requireLogEntry(t, buf, "index generation activated")
	assert.Equal(t, float64(1), entry["retired_generation"])
	assert.Equal(t, "INFO", entry["level"])
}

// TestGeneration_ActivateRejectsAlreadyActive 守"陈旧证据不得二次切换"：同一份证据
// 在切换成功后再用一次，目标已是 active → 拒绝，**不得**再发一次切换调用把新
// active 又退休掉。
// TestGeneration_ActivateRejectsAlreadyActive refuses stale evidence.
func TestGeneration_ActivateRejectsAlreadyActive(t *testing.T) {
	manager, store, _ := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(1, "text-embedding-v4", VectorDimensions, GenerationRetired)
	store.seedGeneration(2, "text-embedding-v5", VectorDimensions, GenerationActive)
	store.stats[1] = GenerationVectorStats{Total: 10}

	evidence, err := manager.Validate(ctx, 1)
	require.NoError(t, err)

	switched, err := manager.Activate(ctx, evidence)
	require.NoError(t, err)
	assert.Equal(t, 2, switched.Retired)
	require.Len(t, store.activateCalls, 1)

	_, err = manager.Activate(ctx, evidence)
	require.ErrorIs(t, err, ErrInvalidRequest)
	assert.Len(t, store.activateCalls, 1, "拒绝后不得再写一次切换")
	assert.Equal(t, []int{1}, store.activeIDs())
}

// TestGeneration_ActivateFailureKeepsPreviousActive 守"切换失败不改变服务面"：
// 存储报错时旧 active 仍是 active、新代际仍是 building，且管理器不重试。
// TestGeneration_ActivateFailureKeepsPreviousActive leaves the old generation live.
func TestGeneration_ActivateFailureKeepsPreviousActive(t *testing.T) {
	manager, store, buf := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(1, "text-embedding-v4", VectorDimensions, GenerationActive)
	building, err := manager.Begin(ctx, GenerationSpec{EmbeddingModel: "text-embedding-v5", Dimensions: VectorDimensions})
	require.NoError(t, err)
	evidence, err := manager.Validate(ctx, building.Generation)
	require.NoError(t, err)

	store.activateErr = errors.New("connection reset by peer")
	_, err = manager.Activate(ctx, evidence)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrInvalidRequest)

	assert.Equal(t, []int{1}, store.activeIDs(), "切换失败后旧 active 继续服务")
	assert.Equal(t, GenerationBuilding, store.rows[building.Generation].Status)
	assert.Len(t, store.activateCalls, 1, "失败后不得自动重试")

	entry := requireLogEntry(t, buf, "index generation activation failed")
	assert.Equal(t, "ERROR", entry["level"])
}

// TestGeneration_ValidateRejectsMismatchedVectors 守"混入别的模型 / 维度的向量
// 不得激活"：不一致条数非零 → 校验失败、无证据，切换无从发起。
// TestGeneration_ValidateRejectsMismatchedVectors fails validation on any mismatch.
func TestGeneration_ValidateRejectsMismatchedVectors(t *testing.T) {
	manager, store, buf := newGenerationHarness(t)
	ctx := context.Background()

	building, err := manager.Begin(ctx, defaultSpec())
	require.NoError(t, err)
	store.stats[building.Generation] = GenerationVectorStats{Total: 7, Mismatched: 2}

	evidence, err := manager.Validate(ctx, building.Generation)
	require.ErrorIs(t, err, ErrGenerationUnvalidated)
	assert.False(t, evidence.Passed(), "失败的校验不得签发证据")

	entry := requireLogEntry(t, buf, "index generation validation rejected")
	assert.Equal(t, "vector_mismatch", entry["reason"])
	assert.Equal(t, float64(2), entry["mismatched_vectors"])
}

// TestGeneration_ValidateRejectsActiveGeneration 守"active 代际不参与校验"：
// 校验是切换前的门，对已生效的代际没有意义——拒绝而不是"顺手再校验一次"。
// TestGeneration_ValidateRejectsActiveGeneration refuses to validate the live one.
func TestGeneration_ValidateRejectsActiveGeneration(t *testing.T) {
	manager, store, buf := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(1, "text-embedding-v4", VectorDimensions, GenerationActive)

	evidence, err := manager.Validate(ctx, 1)
	require.ErrorIs(t, err, ErrInvalidRequest)
	assert.False(t, evidence.Passed())

	entry := requireLogEntry(t, buf, "index generation validation rejected")
	assert.Equal(t, "already_active", entry["reason"])
}

// TestGeneration_BeginRejectsSpecViolations 守"配置错误不落库"：空模型、非冻结
// 维度、未知距离一律拒绝且零写入——**不得**把维度"修正"成 512 建出假代际。
// TestGeneration_BeginRejectsSpecViolations writes nothing for an invalid spec.
func TestGeneration_BeginRejectsSpecViolations(t *testing.T) {
	cases := []struct {
		name string
		spec GenerationSpec
	}{
		{"empty model", GenerationSpec{Dimensions: VectorDimensions}},
		{"wrong dimensions", GenerationSpec{EmbeddingModel: "text-embedding-v4", Dimensions: 768}},
		{"unknown distance", GenerationSpec{EmbeddingModel: "text-embedding-v4", Dimensions: VectorDimensions, Distance: "manhattan"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, store, _ := newGenerationHarness(t)

			_, err := manager.Begin(context.Background(), tc.spec)
			require.ErrorIs(t, err, ErrInvalidRequest)
			assert.Equal(t, 0, store.createCalls, "非法 spec 不得落库")
			assert.Empty(t, store.rows)
		})
	}
}

// TestGeneration_RetireRefusesActiveGeneration 守"active 只能经切换移开"：直接退休
// active 会让索引没有生效代际，一律拒绝。
// TestGeneration_RetireRefusesActiveGeneration refuses to retire the live generation.
func TestGeneration_RetireRefusesActiveGeneration(t *testing.T) {
	manager, store, buf := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(1, "text-embedding-v4", VectorDimensions, GenerationActive)

	_, err := manager.Retire(ctx, 1)
	require.ErrorIs(t, err, ErrInvalidRequest)
	assert.Empty(t, store.retireCalls)
	assert.Equal(t, []int{1}, store.activeIDs())

	entry := requireLogEntry(t, buf, "index generation retirement rejected")
	assert.Equal(t, "active_generation", entry["reason"])
}

// TestGeneration_RetireIsIdempotentAndKeepsRow 守"退休不删除、重试不重复写"：
// 退休后的行仍在（回退窗口靠它），重复退休幂等且不再调用存储。
// TestGeneration_RetireIsIdempotentAndKeepsRow keeps the row and stays idempotent.
func TestGeneration_RetireIsIdempotentAndKeepsRow(t *testing.T) {
	manager, store, _ := newGenerationHarness(t)
	ctx := context.Background()

	building, err := manager.Begin(ctx, defaultSpec())
	require.NoError(t, err)

	retired, err := manager.Retire(ctx, building.Generation)
	require.NoError(t, err)
	assert.Equal(t, GenerationRetired, retired.Status)
	assert.Equal(t, fixedClock()(), retired.RetiredAt)
	assert.True(t, retired.ActivatedAt.IsZero())
	require.Len(t, store.rows, 1, "退休不得删除行")
	require.Len(t, store.retireCalls, 1)

	again, err := manager.Retire(ctx, building.Generation)
	require.NoError(t, err)
	assert.Equal(t, GenerationRetired, again.Status)
	assert.Len(t, store.retireCalls, 1, "已退休的代际不得重复写")
}

// TestGeneration_EnsureBootstrapsFirstGeneration 守"无 active 代际时建首代际"：
// 空库上 Ensure 建一代际、校验（0 条向量也算通过）、激活，且只切换一次。
// TestGeneration_EnsureBootstrapsFirstGeneration creates and activates generation 1.
func TestGeneration_EnsureBootstrapsFirstGeneration(t *testing.T) {
	manager, store, _ := newGenerationHarness(t)
	ctx := context.Background()

	active, err := manager.Ensure(ctx, defaultSpec())
	require.NoError(t, err)
	assert.Equal(t, 1, active.Generation)
	assert.Equal(t, GenerationActive, active.Status)
	assert.Equal(t, "text-embedding-v4", active.EmbeddingModel)
	assert.Equal(t, 1, store.createCalls)
	assert.Equal(t, []int{1}, store.activateCalls)
	assert.Equal(t, []int{1}, store.activeIDs())
}

// TestGeneration_EnsureReturnsMatchingActiveWithoutWrites 守"启动期不重复建代际"：
// active 与配置一致时直接返回，零写入、零切换。
// TestGeneration_EnsureReturnsMatchingActiveWithoutWrites is a pure read path.
func TestGeneration_EnsureReturnsMatchingActiveWithoutWrites(t *testing.T) {
	manager, store, _ := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(3, "text-embedding-v4", VectorDimensions, GenerationActive)

	active, err := manager.Ensure(ctx, defaultSpec())
	require.NoError(t, err)
	assert.Equal(t, 3, active.Generation)
	assert.Equal(t, 0, store.createCalls)
	assert.Empty(t, store.activateCalls)
	assert.Empty(t, store.retireCalls)
}

// TestGeneration_EnsureRefusesImplicitModelSwitch 守 C-09 的启动面：active 代际与
// 配置不一致时**不得**自动新建并切换——换模型必须走显式切换流程。
// TestGeneration_EnsureRefusesImplicitModelSwitch never switches implicitly.
func TestGeneration_EnsureRefusesImplicitModelSwitch(t *testing.T) {
	manager, store, buf := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(1, "text-embedding-v4", VectorDimensions, GenerationActive)

	_, err := manager.Ensure(ctx, GenerationSpec{EmbeddingModel: "text-embedding-v5", Dimensions: VectorDimensions})
	require.ErrorIs(t, err, ErrGenerationMismatch)
	assert.Equal(t, 0, store.createCalls, "启动路径不得自动建新代际")
	assert.Empty(t, store.activateCalls, "启动路径不得自动切换")
	assert.Equal(t, []int{1}, store.activeIDs())

	entry := requireLogEntry(t, buf, "index generation mismatch rejected")
	assert.Equal(t, "spec_mismatch", entry["reason"])

	// 非冻结维度在 spec 校验处就被拒（更早、更强），不会走到代际比对。
	_, err = manager.Ensure(ctx, GenerationSpec{EmbeddingModel: "text-embedding-v4", Dimensions: 768})
	require.ErrorIs(t, err, ErrInvalidRequest, "非冻结维度在 spec 校验处拒绝")
	assert.Empty(t, store.activateCalls)
}

// TestGeneration_RollbackActivatesRetiredGeneration 守"旧代际保留到回退窗口结束"：
// 退休代际的向量还在，重新校验后可切回；切回时当前 active 按同一条单事务规则退休。
// TestGeneration_RollbackActivatesRetiredGeneration switches back to a retired one.
func TestGeneration_RollbackActivatesRetiredGeneration(t *testing.T) {
	manager, store, _ := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(1, "text-embedding-v4", VectorDimensions, GenerationRetired)
	store.seedGeneration(2, "text-embedding-v5", VectorDimensions, GenerationActive)
	store.stats[1] = GenerationVectorStats{Total: 192}

	evidence, err := manager.Validate(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(192), evidence.VectorCount)

	switched, err := manager.Activate(ctx, evidence)
	require.NoError(t, err)
	assert.Equal(t, 2, switched.Retired)
	assert.Equal(t, GenerationActive, switched.Active.Status)
	assert.True(t, switched.Active.RetiredAt.IsZero(), "重新激活必须清空 retired_at（冻结 CHECK）")
	assert.Equal(t, []int{1}, store.activeIDs())
	assert.Equal(t, GenerationRetired, store.rows[2].Status)
	assert.Len(t, store.rows, 2, "回退不删除任何代际行")
}

// TestGeneration_ActivateReadBackFailureIsReported 守"切换完成但回读失败要说清楚"：
// 返回错误但库里已经是新 active；调用方重试会命中"已是 active"，不会二次切换。
// TestGeneration_ActivateReadBackFailureIsReported surfaces a post-switch read failure.
func TestGeneration_ActivateReadBackFailureIsReported(t *testing.T) {
	manager, store, _ := newGenerationHarness(t)
	ctx := context.Background()

	store.seedGeneration(1, "text-embedding-v4", VectorDimensions, GenerationActive)
	building, err := manager.Begin(ctx, GenerationSpec{EmbeddingModel: "text-embedding-v5", Dimensions: VectorDimensions})
	require.NoError(t, err)
	evidence, err := manager.Validate(ctx, building.Generation)
	require.NoError(t, err)

	store.readFailAt = 3 // Begin=0, Validate=1, Activate 前读=2, 回读=3
	_, err = manager.Activate(ctx, evidence)
	require.Error(t, err)
	assert.Equal(t, []int{building.Generation}, store.activeIDs(), "切换已经发生，只是回读失败")
}

// TestGeneration_UnavailableStoreRejected 守"缺端口即拒绝"：零值管理器与空依赖
// 都在任何读写之前返回 `ErrGenerationUnavailable`。
// TestGeneration_UnavailableStoreRejected fails closed without a store.
func TestGeneration_UnavailableStoreRejected(t *testing.T) {
	ctx := context.Background()

	var nilManager *GenerationManager
	_, err := nilManager.Ensure(ctx, defaultSpec())
	require.ErrorIs(t, err, ErrGenerationUnavailable)

	empty := NewGenerationManager(GenerationDeps{})
	_, err = empty.Begin(ctx, defaultSpec())
	require.ErrorIs(t, err, ErrGenerationUnavailable)
	_, err = empty.Validate(ctx, 1)
	require.ErrorIs(t, err, ErrGenerationUnavailable)
	_, err = empty.Activate(ctx, GenerationValidation{Generation: 1})
	require.ErrorIs(t, err, ErrGenerationUnavailable)
	_, err = empty.Retire(ctx, 1)
	require.ErrorIs(t, err, ErrGenerationUnavailable)
}
