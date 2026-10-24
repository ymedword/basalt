package workspace

// 本文件是生命周期状态文件的关键回归用例（颗粒度文档 §3.2 的
// workspace/lifecycle_test.go 十八例与 §4.3.3 写出代码；族 TestLifecycleStore_* /
// TestLifecycleState_*）。采用**同包白盒**：编码器 / 解码器 / 校验和是包内函数，
// 规范字节与校验和向量必须直接对着它们判卷。
// Regression tests for the lifecycle state file. White-box on purpose: the
// canonical encoder, strict decoder and checksum are package-private and are
// judged directly, including golden byte vectors.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// lifecycleTestBase 是注入时钟的基准时刻（UTC，避免本机时区影响 updated_at 断言）。
// lifecycleTestBase is the fixed UTC instant used by the injected test clock.
var lifecycleTestBase = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

// lifecycleGoldenEmptyChecksumJSON 是校验和向量的**独立**输入：checksum 置空串时的
// 规范字节。测试自己算 sha256，不调用被测的 lifecycleChecksum。
// lifecycleGoldenEmptyChecksumJSON is the independent checksum vector input.
const lifecycleGoldenEmptyChecksumJSON = `{"schema_version":2,"agent":"reporter","agent_uid":"ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A","state":"disabled","generation":1,"updated_at":1760000000000,"operation_id":"op-1760000000000-a1b2c3","checksum":""}`

// lifecycleTestClock 是可推进的测试时钟；并发测试里多 goroutine 读取，故加锁。
// lifecycleTestClock is an advanceable clock, guarded for concurrent readers.
type lifecycleTestClock struct {
	mu sync.Mutex
	at time.Time
}

// now 返回当前测试时刻。
func (c *lifecycleTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

// advance 推进测试时刻（模拟两次管理操作之间的时间差）。
func (c *lifecycleTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// newLifecycleTestStore 建一个临时工作区根上的存储，并返回共享 gate 与测试时钟。
// newLifecycleTestStore wires a store over a temp workspace root.
func newLifecycleTestStore(t *testing.T) (*LifecycleStore, string, *core.AgentGate, *lifecycleTestClock) {
	t.Helper()
	root := t.TempDir()
	gate := core.NewAgentGate()
	clock := &lifecycleTestClock{at: lifecycleTestBase}
	return NewLifecycleStore(root, clock.now), root, gate, clock
}

// lifecycleGoldenRecord 是规范字节向量对应的领域记录。
// lifecycleGoldenRecord is the domain record behind the golden byte vector.
func lifecycleGoldenRecord() core.LifecycleRecord {
	return core.LifecycleRecord{
		SchemaVersion: lifecycleSchemaVersion,
		Agent:         "reporter",
		AgentUID:      core.AgentUID("ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"),
		State:         core.AgentDisabled,
		Generation:    1,
		UpdatedAt:     1760000000000,
		OperationID:   "op-1760000000000-a1b2c3",
	}
}

// lifecycleGoldenChecksum 独立计算金标校验和（sha256 覆盖空 checksum 的规范字节）。
func lifecycleGoldenChecksum() string {
	sum := sha256.Sum256([]byte(lifecycleGoldenEmptyChecksumJSON))
	return checksumPrefix + hex.EncodeToString(sum[:])
}

// readLifecycleStateFile 读出状态文件的原始字节（判卷「磁盘零写入 / 逐字节不变」用）。
func readLifecycleStateFile(t *testing.T, root, agent string) []byte {
	t.Helper()
	raw, err := os.ReadFile(stateFilePath(root, agent))
	require.NoError(t, err)
	return raw
}

// writeLifecycleStateFile 在状态文件路径落任意字节（构造损坏 / 篡改夹具）。
func writeLifecycleStateFile(t *testing.T, root, agent string, raw []byte) {
	t.Helper()
	target := stateFilePath(root, agent)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), dirPerm))
	require.NoError(t, os.WriteFile(target, raw, filePerm))
}

// TestLifecycleStore_ReadMissingReportsAbsent：状态文件缺失 → exists=false 且带原因，
// 绝不返回伪造的 active 记录（失败关闭）。
func TestLifecycleStore_ReadMissingReportsAbsent(t *testing.T) {
	t.Parallel()
	store, root, _, _ := newLifecycleTestStore(t)

	rec, exists, err := store.Read(context.Background(), "reporter")
	require.False(t, exists)
	require.Error(t, err)
	require.Contains(t, err.Error(), "缺失")
	require.Empty(t, rec.State)
	require.NoFileExists(t, stateFilePath(root, "reporter"))
}

// TestLifecycleStore_ReadRejectsInvalidAgentName：越界 / 空名称连路径都不拼（约束 14 的
// 名称前置校验与 ValidateAgentName 共用）。
func TestLifecycleStore_ReadRejectsInvalidAgentName(t *testing.T) {
	t.Parallel()
	store, root, _, _ := newLifecycleTestStore(t)

	for _, name := range []string{"", ".", "..", "ops/agent", `ops\agent`, "bad\x00name"} {
		_, exists, err := store.Read(context.Background(), name)
		require.False(t, exists, name)
		require.Error(t, err, name)
	}
	require.NoDirExists(t, filepath.Join(root, controlDirName))
}

// TestLifecycleStore_ReadRejectsCorruptFile：非法 JSON / 未知字段 → exists=false 且报错，
// 不得降级读成可用状态。
func TestLifecycleStore_ReadRejectsCorruptFile(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"非法 JSON": "{not json",
		"未知字段":    strings.Replace(lifecycleGoldenEmptyChecksumJSON, `"checksum":""`, `"checksum":"","extra":1`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, root, _, _ := newLifecycleTestStore(t)
			writeLifecycleStateFile(t, root, "reporter", []byte(raw))

			rec, exists, err := store.Read(context.Background(), "reporter")
			require.False(t, exists)
			require.Error(t, err)
			require.Contains(t, err.Error(), "无效")
			require.Empty(t, rec.State)
		})
	}
}

// TestLifecycleStore_ReadRejectsAgentNameMismatch：文件里的 agent 字段与目录名不符 →
// 拒绝（拷贝状态文件不得冒充别的 Agent）。
func TestLifecycleStore_ReadRejectsAgentNameMismatch(t *testing.T) {
	t.Parallel()
	store, root, _, _ := newLifecycleTestStore(t)

	intruder := lifecycleGoldenRecord()
	intruder.Agent = "intruder"
	raw, err := encodeLifecycleState(intruder)
	require.NoError(t, err)
	writeLifecycleStateFile(t, root, "reporter", raw)

	_, exists, err := store.Read(context.Background(), "reporter")
	require.False(t, exists)
	require.Error(t, err)
	require.Contains(t, err.Error(), "不一致")
}

// TestLifecycleStore_BootstrapWritesDisabledAndChecksum 是模块卡 §4.3.3 写出代码的逐条
// 落地：首建 disabled / generation=1、operation_id 落盘、篡改 generation 后校验和拒绝。
func TestLifecycleStore_BootstrapWritesDisabledAndChecksum(t *testing.T) {
	t.Parallel()
	store, root, _, _ := newLifecycleTestStore(t)

	rec, err := store.Bootstrap(context.Background(), "reporter", "op-1700000000000-abc123")
	require.NoError(t, err)
	require.Equal(t, core.AgentDisabled, rec.State)
	require.EqualValues(t, 1, rec.Generation)
	require.Equal(t, "op-1700000000000-abc123", rec.OperationID)
	require.Equal(t, lifecycleTestBase.UnixMilli(), rec.UpdatedAt)

	raw := readLifecycleStateFile(t, root, "reporter")
	decoded, err := decodeLifecycleState(raw)
	require.NoError(t, err)
	require.Equal(t, "op-1700000000000-abc123", decoded.OperationID)
	require.Equal(t, rec.Checksum, decoded.Checksum)

	// 篡改一个字段后必须被校验和拒绝
	index := bytes.Index(raw, []byte(`"generation":1`))
	require.NotEqual(t, -1, index)
	tampered := bytes.Clone(raw)
	tampered[index+len(`"generation":`)] = '2'
	writeLifecycleStateFile(t, root, "reporter", tampered)

	_, exists, err := store.Read(context.Background(), "reporter")
	require.False(t, exists)
	require.Error(t, err)
	require.Contains(t, err.Error(), "校验和不符")
}

// TestLifecycleStore_BootstrapIsIdempotent：重复基线不动已存在的合法文件（含已迁移到
// 更高代数的状态）；时钟推进也不改写字节。
func TestLifecycleStore_BootstrapIsIdempotent(t *testing.T) {
	t.Parallel()
	store, root, gate, clock := newLifecycleTestStore(t)
	ctx := context.Background()

	first, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-1")
	require.NoError(t, err)
	before := readLifecycleStateFile(t, root, "reporter")

	clock.advance(time.Minute)
	second, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-2")
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, before, readLifecycleStateFile(t, root, "reporter"))

	// 已迁移到 active/2 的文件同样原样返回，不被基线重置为 disabled/1。
	release := gate.Lock("reporter")
	_, err = store.Transition(ctx, "reporter", core.AgentDisabled, 1, core.AgentActive, "op-enable-1")
	release()
	require.NoError(t, err)
	activeBytes := readLifecycleStateFile(t, root, "reporter")

	clock.advance(time.Minute)
	third, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-3")
	require.NoError(t, err)
	require.Equal(t, core.AgentActive, third.State)
	require.EqualValues(t, 2, third.Generation)
	require.Equal(t, activeBytes, readLifecycleStateFile(t, root, "reporter"))
}

// TestLifecycleStore_BootstrapRejectsCorruptExisting：已存在但损坏 → 基线失败、原文件
// 逐字节不动（不覆盖、不静默重建为 active）。
func TestLifecycleStore_BootstrapRejectsCorruptExisting(t *testing.T) {
	t.Parallel()
	store, root, _, _ := newLifecycleTestStore(t)

	corrupt := []byte(`{"schema_version":1,"agent":"reporter"}`)
	writeLifecycleStateFile(t, root, "reporter", corrupt)

	_, err := store.Bootstrap(context.Background(), "reporter", "op-bootstrap-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "基线失败")
	require.Equal(t, corrupt, readLifecycleStateFile(t, root, "reporter"))
}

// TestLifecycleStore_TransitionBumpsGeneration：一次成功迁移 = 状态替换 + 代数 +1 +
// 注入时钟的 updated_at + 本次 operation_id；落盘字节可解码且与返回值一致。
func TestLifecycleStore_TransitionBumpsGeneration(t *testing.T) {
	t.Parallel()
	store, root, gate, clock := newLifecycleTestStore(t)
	ctx := context.Background()

	_, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-1")
	require.NoError(t, err)
	clock.advance(2 * time.Minute)

	release := gate.Lock("reporter")
	rec, err := store.Transition(ctx, "reporter", core.AgentDisabled, 1, core.AgentActive, "op-enable-1")
	release()
	require.NoError(t, err)
	require.Equal(t, core.AgentActive, rec.State)
	require.EqualValues(t, 2, rec.Generation)
	require.Equal(t, "op-enable-1", rec.OperationID)
	require.Equal(t, clock.now().UnixMilli(), rec.UpdatedAt)

	decoded, err := decodeLifecycleState(readLifecycleStateFile(t, root, "reporter"))
	require.NoError(t, err)
	require.Equal(t, rec, decoded)
}

// TestLifecycleStore_TransitionRejectsStalePrecondition：期望状态 / 期望代数任一不符 →
// ErrLifecycleConflict，且文件逐字节不变（乐观锁不产生半截写）。
func TestLifecycleStore_TransitionRejectsStalePrecondition(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		expectedState      core.AgentState
		expectedGeneration int64
	}{
		"期望状态不符": {expectedState: core.AgentActive, expectedGeneration: 1},
		"期望代数不符": {expectedState: core.AgentDisabled, expectedGeneration: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, root, gate, _ := newLifecycleTestStore(t)
			ctx := context.Background()

			_, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-1")
			require.NoError(t, err)
			before := readLifecycleStateFile(t, root, "reporter")

			release := gate.Lock("reporter")
			_, err = store.Transition(ctx, "reporter", tc.expectedState, tc.expectedGeneration, core.AgentActive, "op-enable-1")
			release()
			require.ErrorIs(t, err, core.ErrLifecycleConflict)
			require.Equal(t, before, readLifecycleStateFile(t, root, "reporter"))
		})
	}
}

// TestLifecycleStore_TransitionRejectsUnknownOrSameState：未知状态字面量直接拒绝；与当前
// 状态相同的"迁移"按前置条件冲突处理（没有语义变化就不该推进代数）。
func TestLifecycleStore_TransitionRejectsUnknownOrSameState(t *testing.T) {
	t.Parallel()
	store, root, gate, _ := newLifecycleTestStore(t)
	ctx := context.Background()

	_, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-1")
	require.NoError(t, err)
	before := readLifecycleStateFile(t, root, "reporter")

	release := gate.Lock("reporter")
	_, err = store.Transition(ctx, "reporter", core.AgentDisabled, 1, core.AgentState("paused"), "op-bad-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "未知生命周期状态")

	_, err = store.Transition(ctx, "reporter", core.AgentDisabled, 1, core.AgentDisabled, "op-noop-1")
	release()
	require.ErrorIs(t, err, core.ErrLifecycleConflict)
	require.Equal(t, before, readLifecycleStateFile(t, root, "reporter"))
}

// TestLifecycleStore_DiscardRemovesBootstrapFile：撤销同一操作创建、generation=1 的文件 →
// 文件被删除，之后读取回到"缺失"。Discard 的调用方按 §3.1.30 持写锁。
func TestLifecycleStore_DiscardRemovesBootstrapFile(t *testing.T) {
	t.Parallel()
	store, root, gate, _ := newLifecycleTestStore(t)
	ctx := context.Background()

	_, err := store.Bootstrap(ctx, "reporter", "op-create-1")
	require.NoError(t, err)

	release := gate.Lock("reporter")
	err = store.Discard(ctx, "reporter", "op-create-1")
	release()
	require.NoError(t, err)
	require.NoFileExists(t, stateFilePath(root, "reporter"))

	_, exists, err := store.Read(ctx, "reporter")
	require.False(t, exists)
	require.Error(t, err)
}

// TestLifecycleStore_DiscardRollsBackCreationUnderCallerHeldLock：US-12 创建流程的回滚
// 形态——**同一调用方在同一把写锁内**先 Bootstrap 再 Discard（人工 review ② US-12 交接
// 裁决）。旧实现 Discard 自行取写锁，RWMutex 不可重入 → 本用例会卡在 Discard 自死锁；
// 逆改点 = 在 Discard 恢复 gate.Lock(agent) → 10 秒后逐字失败。
// The creation rollback runs Bootstrap then Discard inside one caller-held write lock;
// a self-locking Discard would deadlock against the non-reentrant RWMutex.
func TestLifecycleStore_DiscardRollsBackCreationUnderCallerHeldLock(t *testing.T) {
	t.Parallel()
	store, root, gate, _ := newLifecycleTestStore(t)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		release := gate.Lock("reporter")
		defer release()
		if _, err := store.Bootstrap(ctx, "reporter", "op-create-1"); err != nil {
			done <- err
			return
		}
		done <- store.Discard(ctx, "reporter", "op-create-1")
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("调用方已持写锁时 Discard 未返回：疑似 Discard 重新取锁导致自死锁")
	}
	require.NoFileExists(t, stateFilePath(root, "reporter"))
}

// TestLifecycleStore_DiscardRejectsMovedGeneration：代数已推进的文件不是"创建失败可回滚
// 的初始状态" → 冲突且文件保留（不得用于正常启停）。
func TestLifecycleStore_DiscardRejectsMovedGeneration(t *testing.T) {
	t.Parallel()
	store, root, gate, _ := newLifecycleTestStore(t)
	ctx := context.Background()

	_, err := store.Bootstrap(ctx, "reporter", "op-create-1")
	require.NoError(t, err)
	release := gate.Lock("reporter")
	_, err = store.Transition(ctx, "reporter", core.AgentDisabled, 1, core.AgentActive, "op-enable-1")
	release()
	require.NoError(t, err)
	before := readLifecycleStateFile(t, root, "reporter")

	release = gate.Lock("reporter")
	err = store.Discard(ctx, "reporter", "op-create-1")
	release()
	require.ErrorIs(t, err, core.ErrLifecycleConflict)
	require.Equal(t, before, readLifecycleStateFile(t, root, "reporter"))
}

// TestLifecycleStore_DiscardRejectsOtherOperationAndMissingFile：操作标识不符或文件缺失 →
// 冲突（撤销的前置条件无法查证，宁可拒绝）。
func TestLifecycleStore_DiscardRejectsOtherOperationAndMissingFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("操作标识不符", func(t *testing.T) {
		t.Parallel()
		store, root, gate, _ := newLifecycleTestStore(t)
		_, err := store.Bootstrap(ctx, "reporter", "op-create-1")
		require.NoError(t, err)
		before := readLifecycleStateFile(t, root, "reporter")

		release := gate.Lock("reporter")
		err = store.Discard(ctx, "reporter", "op-create-2")
		release()
		require.ErrorIs(t, err, core.ErrLifecycleConflict)
		require.Equal(t, before, readLifecycleStateFile(t, root, "reporter"))
	})

	t.Run("文件缺失", func(t *testing.T) {
		t.Parallel()
		store, _, gate, _ := newLifecycleTestStore(t)
		release := gate.Lock("reporter")
		err := store.Discard(ctx, "reporter", "op-create-1")
		release()
		require.ErrorIs(t, err, core.ErrLifecycleConflict)
	})
}

// TestLifecycleStore_WriteLeavesNoTempResidue：连续写入后目录里只剩状态文件本身——
// 临时文件 + rename 的写法不得留下残片。
func TestLifecycleStore_WriteLeavesNoTempResidue(t *testing.T) {
	t.Parallel()
	store, root, gate, _ := newLifecycleTestStore(t)
	ctx := context.Background()

	_, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-1")
	require.NoError(t, err)
	release := gate.Lock("reporter")
	_, err = store.Transition(ctx, "reporter", core.AgentDisabled, 1, core.AgentActive, "op-enable-1")
	require.NoError(t, err)
	_, err = store.Transition(ctx, "reporter", core.AgentActive, 2, core.AgentDisabled, "op-disable-1")
	release()
	require.NoError(t, err)

	entries, err := os.ReadDir(filepath.Dir(stateFilePath(root, "reporter")))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{"reporter.json"}, names)
}

// TestLifecycleStore_StateFileLivesUnderControlDirectory：路径固定为
// <工作区根>/control/agents/<name>.json；不落在 agents/ 下（约束 30 的目录面）。
func TestLifecycleStore_StateFileLivesUnderControlDirectory(t *testing.T) {
	t.Parallel()
	store, root, _, _ := newLifecycleTestStore(t)

	_, err := store.Bootstrap(context.Background(), "reporter", "op-bootstrap-1")
	require.NoError(t, err)

	rel, err := filepath.Rel(root, stateFilePath(root, "reporter"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(controlDirName, lifecycleAgentsDirName, "reporter.json"), rel)
	require.NoDirExists(t, filepath.Join(root, agentsDirName, "reporter"))
	require.FileExists(t, filepath.Join(root, controlDirName, lifecycleAgentsDirName, "reporter.json"))
}

// TestLifecycleState_EncodingIsCanonical：编码结果的字段顺序 / 紧凑形态 / 无尾随换行与
// 金标逐字节一致（约束 14 的规范字节判据）。
func TestLifecycleState_EncodingIsCanonical(t *testing.T) {
	t.Parallel()

	rec := lifecycleGoldenRecord()
	checksum, covered, err := lifecycleChecksum(rec)
	require.NoError(t, err)
	require.Equal(t, lifecycleGoldenChecksum(), checksum)
	require.Equal(t, lifecycleGoldenEmptyChecksumJSON, string(covered))

	raw, err := encodeLifecycleState(rec)
	require.NoError(t, err)
	want := strings.Replace(lifecycleGoldenEmptyChecksumJSON, `"checksum":""`,
		`"checksum":"`+lifecycleGoldenChecksum()+`"`, 1)
	require.Equal(t, want, string(raw))
	require.False(t, strings.HasSuffix(string(raw), "\n"), "规范字节不得带尾随换行")
	require.Len(t, lifecycleGoldenChecksum(), len(checksumPrefix)+64)
}

// TestLifecycleState_ChecksumVectorMatchesIndependentHash：校验和 = sha256（checksum
// 置空后的整份字节），前缀与十六进制大小写逐字固定。
func TestLifecycleState_ChecksumVectorMatchesIndependentHash(t *testing.T) {
	t.Parallel()

	const emptyChecksum = `{"schema_version":2,"agent":"reporter","agent_uid":"ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A","state":"active","generation":3,"updated_at":1760000000000,"operation_id":"op-1760000000000-a1b2c3","checksum":""}`
	sum := sha256.Sum256([]byte(emptyChecksum))
	want := "sha256:" + hex.EncodeToString(sum[:])

	rec := lifecycleGoldenRecord()
	rec.State = core.AgentActive
	rec.Generation = 3
	got, covered, err := lifecycleChecksum(rec)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Equal(t, emptyChecksum, string(covered), "校验和必须盖住 checksum 置空后的整份规范字节")
}

// TestLifecycleState_DecodeRejectsNonCanonicalOrUnknownBytes：未知字段 / 重复键 / 乱序 /
// 空白 / 多余文档一律拒绝——重编码比对封住"格式宽松"的绕过面。
func TestLifecycleState_DecodeRejectsNonCanonicalOrUnknownBytes(t *testing.T) {
	t.Parallel()

	golden := lifecycleGoldenEmptyChecksumJSON
	cases := map[string]string{
		"未知字段":  strings.Replace(golden, `"checksum":""`, `"checksum":"","extra":1`, 1),
		"重复键":   strings.Replace(golden, `"generation":1`, `"generation":1,"generation":2`, 1),
		"字段乱序":  `{"agent":"reporter","schema_version":1,"state":"disabled","generation":1,"updated_at":1760000000000,"operation_id":"op-1760000000000-a1b2c3","checksum":""}`,
		"空白偏离":  strings.Replace(golden, `"state":"disabled",`, `"state": "disabled",`, 1),
		"多余文档":  golden + `{}`,
		"截断文件":  golden[:len(golden)/2],
		"空文件":   "",
		"非对象形态": `[]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeLifecycleState([]byte(raw))
			require.Error(t, err)
		})
	}
}

// TestLifecycleState_DecodeRejectsInvalidFields：字段级损坏（版本 / 状态 / 代数 / 时间 /
// 操作标识 / 校验和 / 非整数毫秒）逐个拒绝。
func TestLifecycleState_DecodeRejectsInvalidFields(t *testing.T) {
	t.Parallel()

	fieldCases := map[string]struct {
		mutate func(*core.LifecycleRecord)
		want   string
	}{
		"schema 版本": {mutate: func(rec *core.LifecycleRecord) { rec.SchemaVersion = 3 }, want: "schema_version"},
		"状态字面量":     {mutate: func(rec *core.LifecycleRecord) { rec.State = core.AgentState("paused") }, want: "未知生命周期状态"},
		"代数下界":      {mutate: func(rec *core.LifecycleRecord) { rec.Generation = 0 }, want: "generation"},
		"时间下界":      {mutate: func(rec *core.LifecycleRecord) { rec.UpdatedAt = 0 }, want: "updated_at"},
		"操作标识":      {mutate: func(rec *core.LifecycleRecord) { rec.OperationID = "migration" }, want: "operation_id"},
	}
	for name, tc := range fieldCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := lifecycleGoldenRecord()
			tc.mutate(&rec)
			raw, err := marshalLifecycleState(rec)
			require.NoError(t, err)
			_, err = decodeLifecycleState(raw)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("校验和不符", func(t *testing.T) {
		t.Parallel()
		rec := lifecycleGoldenRecord()
		rec.Checksum = checksumPrefix + strings.Repeat("0", 64)
		raw, err := marshalLifecycleState(rec)
		require.NoError(t, err)
		_, err = decodeLifecycleState(raw)
		require.Error(t, err)
		require.Contains(t, err.Error(), "校验和不符")
	})

	t.Run("非整数毫秒", func(t *testing.T) {
		t.Parallel()
		raw := strings.Replace(lifecycleGoldenEmptyChecksumJSON, `"updated_at":1760000000000`, `"updated_at":1760000000000.5`, 1)
		_, err := decodeLifecycleState([]byte(raw))
		require.Error(t, err)
		require.Contains(t, err.Error(), "JSON")
	})
}

// TestLifecycleStore_ConcurrentTransition：并发迁移在 Agent 写锁 + 期望代数乐观锁下
// 逐一推进——成功次数与最终代数严格相等，冲突之外不得出现别的错误（011-race 指定项）。
func TestLifecycleStore_ConcurrentTransition(t *testing.T) {
	t.Parallel()
	store, _, gate, _ := newLifecycleTestStore(t)
	ctx := context.Background()
	_, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-1")
	require.NoError(t, err)

	// 并发参与者共用装配时传入的同一个 gate：读 + 迁移在写锁内完成，等价于管理面的一次
	// 启停。换成各自独立的 gate 就会退化为"读后写"丢更新——本用例守的正是这条。
	const workers = 8
	const rounds = 12
	var succeeded atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				release := gate.Lock("reporter")
				current, exists, readErr := store.Read(ctx, "reporter")
				if readErr != nil || !exists {
					release()
					errs <- fmt.Errorf("worker %d 读状态失败: exists=%v err=%v", worker, exists, readErr)
					return
				}
				target := core.AgentActive
				if current.State == core.AgentActive {
					target = core.AgentDisabled
				}
				operationID := fmt.Sprintf("op-w%d-r%d", worker, round)
				_, transitionErr := store.Transition(ctx, "reporter", current.State, current.Generation, target, operationID)
				release()
				if transitionErr != nil {
					if !errors.Is(transitionErr, core.ErrLifecycleConflict) {
						errs <- fmt.Errorf("worker %d 迁移出现非冲突错误: %w", worker, transitionErr)
						return
					}
					continue
				}
				succeeded.Add(1)
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	final, exists, err := store.Read(ctx, "reporter")
	require.NoError(t, err)
	require.True(t, exists)
	require.EqualValues(t, 1+succeeded.Load(), final.Generation)
}

// TestLifecycleStore_TransitionPreservesAgentUID：稳定身份跨状态迁移不变——启停与
// 归档（active → archived）都沿用同一 `agent_uid`，只有 v1 记录首写才补发身份。
// 归档恢复对原名的身份校验依赖这一事实：同一 Agent 的 UID 不得因迁移而更换。
// Acceptance point: the stable identity survives every lifecycle transition.
func TestLifecycleStore_TransitionPreservesAgentUID(t *testing.T) {
	t.Parallel()
	store, _, gate, clock := newLifecycleTestStore(t)
	ctx := context.Background()

	first, err := store.Bootstrap(ctx, "reporter", "op-bootstrap-1")
	require.NoError(t, err)
	require.True(t, first.AgentUID.Valid())

	clock.advance(time.Minute)
	release := gate.Lock("reporter")
	active, err := store.Transition(ctx, "reporter", core.AgentDisabled, 1, core.AgentActive, "op-enable-1")
	release()
	require.NoError(t, err)
	require.Equal(t, first.AgentUID, active.AgentUID, "启停不得更换稳定身份")

	clock.advance(time.Minute)
	release = gate.Lock("reporter")
	archived, err := store.Transition(ctx, "reporter", core.AgentActive, 2, core.AgentArchived, "op-archive-1")
	release()
	require.NoError(t, err)
	require.Equal(t, first.AgentUID, archived.AgentUID, "归档不得更换稳定身份")

	// 归档记录里的身份来自状态文件：Read 必须回报同一 UID（恢复路径据此校验）。
	read, exists, err := store.Read(ctx, "reporter")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, first.AgentUID, read.AgentUID)
	require.Equal(t, core.AgentArchived, read.State)
	require.Equal(t, int64(3), read.Generation)
}
