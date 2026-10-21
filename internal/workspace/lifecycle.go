package workspace

// 本文件是 Agent 生命周期状态文件的**唯一读写面**（颗粒度文档 §3.1.12 / 约束 14）：
// 路径固定 <工作区根>/control/agents/<name>.json（即契约全文 .basalt/control/agents/<name>.json），
// 紧凑 JSON + 固定字段顺序 + 校验和 + 同目录临时文件 + 原子 rename。
// 状态文件是生命周期的唯一权威；数据库的 agent_lifecycle 只是可重建索引。
// This file is the single read/write surface of the agent lifecycle state file: a
// canonical compact-JSON document with a checksum, written through a sibling temp
// file and an atomic rename. The file is the authority; the SQLite index is
// rebuildable.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// controlDirName 是受保护管理目录（相对工作区根 .basalt/ 的 control/）——不在
	// watcher 监听、不在工作区浏览、不在 Agent 文件工具白名单（约束 30）。
	// controlDirName is the protected management directory under the workspace root.
	controlDirName = "control"

	// lifecycleAgentsDirName 是状态文件子目录：control/agents/<name>.json。
	// lifecycleAgentsDirName holds one state file per agent.
	lifecycleAgentsDirName = "agents"

	// lifecycleSchemaVersion 是状态文件首版固定版本号（契约 agent-md.md §2.2）；
	// 未来版本变更必须让旧二进制拒绝误读，而不是按兼容子集猜测。
	// lifecycleSchemaVersion pins the first on-disk schema; future versions must be
	// rejected by older binaries rather than guessed at.
	lifecycleSchemaVersion = 1

	// checksumPrefix 是校验和字面量前缀（"sha256:" + 64 位小写十六进制）。
	// checksumPrefix prefixes the lowercase hex checksum.
	checksumPrefix = "sha256:"

	// operationIDPrefix 是管理操作标识前缀；契约明确状态文件不得写 "migration" 字面量。
	// operationIDPrefix is the only accepted operation-id prefix.
	operationIDPrefix = "op-"
)

// 编译期断言：方法集与 core.LifecyclePort 逐字一致（§3.1.12）。
// Compile-time assertion that the method set matches the consumer-side port.
var _ core.LifecyclePort = (*LifecycleStore)(nil)

// LifecycleStore 管理状态文件的读写：root 是工作区根（.basalt/ 本身），now 是唯一时钟
// 来源。本存储**不持有也不取任何锁**——Agent 级互斥由调用方持有的共享 core.AgentGate
// 提供（Read 在准入读锁内；Transition / Bootstrap / Discard 要求调用方持写锁，见
// §3.1.30）。这样同一把锁既能与准入互斥，又不会因创建 / 回滚流程已持锁而自死锁。
// LifecycleStore owns the state file; root is the workspace root (.basalt itself) and
// now is the only clock source. The store holds no lock: the caller-supplied shared
// core.AgentGate provides per-agent exclusion (read lock for admission, write lock for
// transitions), so the same lock excludes admission without re-taking it here.
type LifecycleStore struct {
	root string
	now  func() time.Time
}

// NewLifecycleStore 组装状态文件存储；now 为 nil 时回落 time.Now。
// NewLifecycleStore assembles the store; a nil clock falls back to time.Now.
func NewLifecycleStore(root string, now func() time.Time) *LifecycleStore {
	if now == nil {
		now = time.Now
	}
	return &LifecycleStore{root: root, now: now}
}

// lifecycleStateRelPath 给出状态文件相对工作区根（.basalt/）的路径——契约字面量
// .basalt/control/agents/<agent>.json 去掉 .basalt/ 前缀后的形态。
// lifecycleStateRelPath returns the workspace-relative state-file path.
func lifecycleStateRelPath(agent string) string {
	return filepath.Join(controlDirName, lifecycleAgentsDirName, agent+".json")
}

// stateFilePath 给出状态文件在当前平台上的完整路径（root 为工作区根 .basalt/）。
// stateFilePath joins the state-file path onto the workspace root.
func stateFilePath(root, agent string) string {
	return filepath.Join(root, lifecycleStateRelPath(agent))
}

// Read 读取并校验状态文件；exists=false 时 error 说明缺失 / 损坏原因，调用方据此映射
// 为不可用——**不得**把缺失读成 active（失败关闭）。本方法不加锁：准入路径在
// AgentGate 读锁内调用（约束 17），单文件读取由 rename 的原子替换语义保证不读到半截。
// Read takes no lock: admission calls it under the read lock, and rename atomicity
// keeps readers from observing a half-written file.
func (s *LifecycleStore) Read(ctx context.Context, agent string) (rec core.LifecycleRecord, exists bool, err error) {
	if err := ctx.Err(); err != nil {
		return core.LifecycleRecord{}, false, err
	}
	if err := ValidateAgentName(agent); err != nil {
		return core.LifecycleRecord{}, false, err
	}

	base, err := os.OpenRoot(s.root)
	if err != nil {
		return core.LifecycleRecord{}, false, fmt.Errorf("打开工作区目录 %s: %w", s.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", s.root, closeErr))
		}
	}()

	rec, exists, err = readLifecycleState(base, agent)
	return rec, exists, err
}

// Transition 执行一次状态迁移：前置条件（期望状态 + 期望代数）不符返回
// core.ErrLifecycleConflict，文件原样不动；成功时代数 +1 并以注入时钟取 updated_at。
// **调用方必须已持有该 Agent 的写锁**（§3.1.30）——本方法不取锁，重复取锁会自死锁。
// Transition requires the caller to hold the agent's write lock; re-taking it here
// would deadlock against the non-reentrant RWMutex.
func (s *LifecycleStore) Transition(ctx context.Context, agent string, expectedState core.AgentState, expectedGeneration int64, target core.AgentState, operationID string) (rec core.LifecycleRecord, err error) {
	if err := ctx.Err(); err != nil {
		return core.LifecycleRecord{}, err
	}
	if err := ValidateAgentName(agent); err != nil {
		return core.LifecycleRecord{}, err
	}
	if err := validateLifecycleState(target); err != nil {
		return core.LifecycleRecord{}, err
	}

	base, err := os.OpenRoot(s.root)
	if err != nil {
		return core.LifecycleRecord{}, fmt.Errorf("打开工作区目录 %s: %w", s.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", s.root, closeErr))
		}
	}()

	current, _, readErr := readLifecycleState(base, agent)
	if readErr != nil {
		return core.LifecycleRecord{}, fmt.Errorf("%w: Agent %s 不能迁移：%v", core.ErrLifecycleConflict, agent, readErr)
	}
	if current.State != expectedState || current.Generation != expectedGeneration {
		return core.LifecycleRecord{}, fmt.Errorf("%w: Agent %s 期望 %s/%d，当前 %s/%d",
			core.ErrLifecycleConflict, agent, expectedState, expectedGeneration, current.State, current.Generation)
	}
	if current.State == target {
		return core.LifecycleRecord{}, fmt.Errorf("%w: Agent %s 已处于 %s，没有可执行的迁移", core.ErrLifecycleConflict, agent, target)
	}

	next := current
	next.State = target
	next.Generation = current.Generation + 1
	next.UpdatedAt = s.now().UnixMilli()
	next.OperationID = operationID

	checksum, _, err := lifecycleChecksum(next)
	if err != nil {
		return core.LifecycleRecord{}, err
	}
	next.Checksum = checksum
	data, err := encodeLifecycleState(next)
	if err != nil {
		return core.LifecycleRecord{}, fmt.Errorf("生成 Agent %s 的状态文件字节: %w", agent, err)
	}
	if err := writeLifecycleStateAtomic(base, agent, data); err != nil {
		return core.LifecycleRecord{}, err
	}
	return next, nil
}

// Bootstrap 只在状态文件不存在时写 disabled / generation=1；已存在且合法 → 原样返回；
// 已存在但损坏 → 报错且不覆盖（基线失败，拒绝启动）。基线在开放服务前单线程执行，
// 故本方法不加锁；其它调用方必须已持有该 Agent 的写锁。
// Bootstrap writes disabled/generation=1 only when the file is absent; a corrupt
// existing file fails the baseline instead of being rewritten.
func (s *LifecycleStore) Bootstrap(ctx context.Context, agent, operationID string) (rec core.LifecycleRecord, err error) {
	if err := ctx.Err(); err != nil {
		return core.LifecycleRecord{}, err
	}
	if err := ValidateAgentName(agent); err != nil {
		return core.LifecycleRecord{}, err
	}

	base, err := os.OpenRoot(s.root)
	if err != nil {
		return core.LifecycleRecord{}, fmt.Errorf("打开工作区目录 %s: %w", s.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", s.root, closeErr))
		}
	}()

	// 文件不存在是"尚未建立基线"的正常形态（首启 / 新 Agent），只有损坏、名称不符或
	// 读失败才是基线失败——用 fs.ErrNotExist 区分，不靠错误文案。
	existing, exists, readErr := readLifecycleState(base, agent)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return core.LifecycleRecord{}, fmt.Errorf("生命周期基线失败：Agent %s 的状态文件不可用，拒绝覆盖: %v", agent, readErr)
	}
	if exists {
		return existing, nil
	}

	rec = core.LifecycleRecord{
		SchemaVersion: lifecycleSchemaVersion,
		Agent:         agent,
		State:         core.AgentDisabled,
		Generation:    1,
		UpdatedAt:     s.now().UnixMilli(),
		OperationID:   operationID,
	}
	checksum, _, err := lifecycleChecksum(rec)
	if err != nil {
		return core.LifecycleRecord{}, err
	}
	rec.Checksum = checksum
	data, err := encodeLifecycleState(rec)
	if err != nil {
		return core.LifecycleRecord{}, fmt.Errorf("生成 Agent %s 的状态文件字节: %w", agent, err)
	}
	if err := writeLifecycleStateAtomic(base, agent, data); err != nil {
		return core.LifecycleRecord{}, err
	}
	return rec, nil
}

// Discard 只删除"由同一 operationID 创建、generation=1"的状态文件；其它情况（代数已
// 迁移、操作不符、文件缺失或损坏）返回 core.ErrLifecycleConflict——本方法**不得**用于
// 正常启停。**调用方必须已持有该 Agent 的写锁**（与 Bootstrap / Transition 同口径，
// 模块卡 §3.1.30）——本方法不取锁：创建流程的回滚在已持写锁内调用，重复取锁会自
// 死锁（人工 review ② US-12 交接裁决）。
// Discard removes only a generation-1 file created by the same operation; every
// other case is a conflict. The caller holds the agent write lock; taking it here
// again would deadlock against the non-reentrant RWMutex during creation rollback.
func (s *LifecycleStore) Discard(ctx context.Context, agent, operationID string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateAgentName(agent); err != nil {
		return err
	}

	base, err := os.OpenRoot(s.root)
	if err != nil {
		return fmt.Errorf("打开工作区目录 %s: %w", s.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", s.root, closeErr))
		}
	}()

	rec, _, readErr := readLifecycleState(base, agent)
	if readErr != nil {
		return fmt.Errorf("%w: Agent %s 的状态文件不可读（缺失或损坏），无法撤销: %v",
			core.ErrLifecycleConflict, agent, readErr)
	}
	if rec.Generation != 1 || rec.OperationID != operationID {
		return fmt.Errorf("%w: Agent %s 的状态文件 generation=%d operation_id=%q，不是本次操作创建的初始状态，拒绝撤销",
			core.ErrLifecycleConflict, agent, rec.Generation, rec.OperationID)
	}

	rel := filepath.FromSlash(lifecycleStateRelPath(agent))
	if err := base.Remove(rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: Agent %s 的状态文件在撤销前已被移除", core.ErrLifecycleConflict, agent)
		}
		return fmt.Errorf("删除生命周期状态文件 %s: %w", lifecycleStateRelPath(agent), err)
	}
	return nil
}

// lifecycleStateFile 是状态文件的落盘形态：字段顺序即契约顺序（encoding/json 按声明
// 顺序编码），新增字段必须同时升级 schema_version。
// lifecycleStateFile is the on-disk shape; declaration order is the contract order.
type lifecycleStateFile struct {
	SchemaVersion int    `json:"schema_version"`
	Agent         string `json:"agent"`
	State         string `json:"state"`
	Generation    int64  `json:"generation"`
	UpdatedAt     int64  `json:"updated_at"`
	OperationID   string `json:"operation_id"`
	Checksum      string `json:"checksum"`
}

// lifecycleStateFileOf 把领域记录映射为落盘形态。
// lifecycleStateFileOf maps the domain record onto the on-disk shape.
func lifecycleStateFileOf(rec core.LifecycleRecord) lifecycleStateFile {
	return lifecycleStateFile{
		SchemaVersion: rec.SchemaVersion,
		Agent:         rec.Agent,
		State:         string(rec.State),
		Generation:    rec.Generation,
		UpdatedAt:     rec.UpdatedAt,
		OperationID:   rec.OperationID,
		Checksum:      rec.Checksum,
	}
}

// lifecycleRecord 把落盘形态映射回领域记录。
// lifecycleRecord maps the on-disk shape back onto the domain record.
func (f lifecycleStateFile) lifecycleRecord() core.LifecycleRecord {
	return core.LifecycleRecord{
		SchemaVersion: f.SchemaVersion,
		Agent:         f.Agent,
		State:         core.AgentState(f.State),
		Generation:    f.Generation,
		UpdatedAt:     f.UpdatedAt,
		OperationID:   f.OperationID,
		Checksum:      f.Checksum,
	}
}

// marshalLifecycleState 按固定字段顺序输出紧凑 JSON（无缩进、无尾随换行）——字节
// 稳定性是校验和可核对的前提，故不得换成 map 或带缩进的编码。
// marshalLifecycleState emits compact JSON in the fixed field order; byte stability
// is what makes the checksum checkable.
func marshalLifecycleState(rec core.LifecycleRecord) ([]byte, error) {
	raw, err := json.Marshal(lifecycleStateFileOf(rec))
	if err != nil {
		return nil, fmt.Errorf("编码生命周期状态: %w", err)
	}
	return raw, nil
}

// lifecycleChecksum 计算校验和：checksum 字段置空串后取整份 UTF-8 字节的 sha256，
// 返回 "sha256:<64 位小写十六进制>" 与被哈希的字节（第二返回值供复算与测试对照）。
// lifecycleChecksum hashes the canonical bytes with an empty checksum field and
// returns the digest together with the exact bytes it covered.
func lifecycleChecksum(rec core.LifecycleRecord) (string, []byte, error) {
	rec.Checksum = ""
	covered, err := marshalLifecycleState(rec)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(covered)
	return checksumPrefix + hex.EncodeToString(sum[:]), covered, nil
}

// encodeLifecycleState 生成最终落盘字节：先校验字段，再以空 checksum 计算、回填后重编码。
// encodeLifecycleState validates the record, computes the checksum over the empty
// field, backfills it and re-encodes the final bytes.
func encodeLifecycleState(rec core.LifecycleRecord) ([]byte, error) {
	if err := validateLifecycleRecord(rec); err != nil {
		return nil, err
	}
	checksum, _, err := lifecycleChecksum(rec)
	if err != nil {
		return nil, err
	}
	rec.Checksum = checksum
	return marshalLifecycleState(rec)
}

// decodeLifecycleState 严格解析：拒绝未知字段、非规范字节（字段顺序 / 空白 / 重复键
// 都会在重编码比对时暴露）与校验和不符——宽松解析会让"校验和通过"失去意义。
// decodeLifecycleState rejects unknown fields, non-canonical bytes (order,
// whitespace, duplicate keys) and checksum mismatches.
func decodeLifecycleState(raw []byte) (core.LifecycleRecord, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file lifecycleStateFile
	if err := decoder.Decode(&file); err != nil {
		return core.LifecycleRecord{}, fmt.Errorf("解析状态文件 JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return core.LifecycleRecord{}, errors.New("状态文件在首个 JSON 文档之后仍有多余内容")
	}

	rec := file.lifecycleRecord()
	canonical, err := marshalLifecycleState(rec)
	if err != nil {
		return core.LifecycleRecord{}, err
	}
	if !bytes.Equal(canonical, raw) {
		return core.LifecycleRecord{}, errors.New("状态文件不是规范字节：字段顺序、空白或重复键均不得偏离契约形态")
	}
	if err := validateLifecycleRecord(rec); err != nil {
		return core.LifecycleRecord{}, err
	}
	checksum, _, err := lifecycleChecksum(rec)
	if err != nil {
		return core.LifecycleRecord{}, err
	}
	if checksum != rec.Checksum {
		return core.LifecycleRecord{}, fmt.Errorf("状态文件校验和不符：文件为 %q，重算为 %q", rec.Checksum, checksum)
	}
	return rec, nil
}

// validateLifecycleRecord 校验领域字段：schema 版本、名称、状态字面量、代数、时间与
// 操作标识。任何一条不成立都按损坏处理（拒绝新 Run），不得降级猜测。
// validateLifecycleRecord checks every field; any failure is corruption, never a
// reason to guess.
func validateLifecycleRecord(rec core.LifecycleRecord) error {
	if rec.SchemaVersion != lifecycleSchemaVersion {
		return fmt.Errorf("不支持的 schema_version %d（本二进制只读 %d）", rec.SchemaVersion, lifecycleSchemaVersion)
	}
	if err := ValidateAgentName(rec.Agent); err != nil {
		return fmt.Errorf("状态文件 agent 字段无效: %w", err)
	}
	if err := validateLifecycleState(rec.State); err != nil {
		return err
	}
	if rec.Generation < 1 {
		return fmt.Errorf("generation 必须 ≥ 1，当前 %d", rec.Generation)
	}
	if rec.UpdatedAt <= 0 {
		return fmt.Errorf("updated_at 必须为正的 Unix 毫秒数，当前 %d", rec.UpdatedAt)
	}
	if !strings.HasPrefix(rec.OperationID, operationIDPrefix) || len(rec.OperationID) == len(operationIDPrefix) {
		return fmt.Errorf("operation_id 必须是 %q 开头的管理操作标识，当前 %q", operationIDPrefix, rec.OperationID)
	}
	return nil
}

// validateLifecycleState 校验三值状态字面量（契约 agent-md.md §2.2）。
// validateLifecycleState checks the three-value literal set.
func validateLifecycleState(state core.AgentState) error {
	switch state {
	case core.AgentActive, core.AgentDisabled, core.AgentArchived:
		return nil
	default:
		return fmt.Errorf("未知生命周期状态 %q（只接受 active / disabled / archived）", state)
	}
}

// readLifecycleState 在已打开的根上读取并校验状态文件；exists=false 时 error 说明缺失 /
// 损坏原因。agent 字段必须与目录名逐字一致（防止拷贝状态文件冒充别的 Agent）。
// readLifecycleState reads and validates the state file through an opened root; the
// agent field must match the directory name exactly.
func readLifecycleState(base *os.Root, agent string) (core.LifecycleRecord, bool, error) {
	rel := lifecycleStateRelPath(agent)
	raw, err := base.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return core.LifecycleRecord{}, false, fmt.Errorf("生命周期状态文件缺失（%s）: %w", rel, err)
		}
		return core.LifecycleRecord{}, false, fmt.Errorf("读取生命周期状态文件 %s: %w", rel, err)
	}

	rec, err := decodeLifecycleState(raw)
	if err != nil {
		return core.LifecycleRecord{}, false, fmt.Errorf("生命周期状态文件 %s 无效: %w", rel, err)
	}
	if rec.Agent != agent {
		return core.LifecycleRecord{}, false, fmt.Errorf("状态文件 %s 的 agent 字段 %q 与目录名 %q 不一致", rel, rec.Agent, agent)
	}
	return rec, true, nil
}

// writeLifecycleStateAtomic 写同目录唯一名临时文件 → Sync → Close → 原子 rename；任何
// 一步失败都清理临时文件并返回错误，原文件保持不变（约束 14）。
// writeLifecycleStateAtomic writes a sibling temp file and atomically renames it
// over the target; every failure path removes the temp file.
func writeLifecycleStateAtomic(base *os.Root, agent string, data []byte) error {
	rel := lifecycleStateRelPath(agent)
	dir := filepath.Dir(filepath.FromSlash(rel))
	if err := base.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("创建生命周期控制目录 %s: %w", filepath.Dir(rel), err)
	}

	tmp := filepath.Join(dir, filepath.Base(filepath.FromSlash(rel))+".tmp-"+lifecycleTempSuffix())
	file, err := base.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("建临时文件 %s: %w", tmp, err)
	}
	if _, err := file.Write(data); err != nil {
		return lifecycleTempFailure(base, tmp, fmt.Errorf("写入 %s: %w", tmp, err), file)
	}
	if err := file.Sync(); err != nil {
		return lifecycleTempFailure(base, tmp, fmt.Errorf("刷盘 %s: %w", tmp, err), file)
	}
	if err := file.Close(); err != nil {
		return lifecycleTempFailure(base, tmp, fmt.Errorf("关闭临时文件 %s: %w", tmp, err), nil)
	}
	if err := base.Rename(tmp, filepath.FromSlash(rel)); err != nil {
		return lifecycleTempFailure(base, tmp, fmt.Errorf("替换 %s: %w", rel, err), nil)
	}
	return nil
}

// lifecycleTempFailure 收拢"失败即清理"：无论关闭是否成功都尝试删除临时文件，两个错误
// 都进返回文本（不静默丢弃）。
// lifecycleTempFailure removes the temp file on any failure and joins cleanup errors.
func lifecycleTempFailure(base *os.Root, tmp string, cause error, file *os.File) error {
	err := cause
	if file != nil {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭临时文件 %s: %w", tmp, closeErr))
		}
	}
	if removeErr := base.Remove(tmp); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		err = errors.Join(err, fmt.Errorf("清理临时文件 %s: %w", tmp, removeErr))
	}
	return err
}

// lifecycleTempSuffix 生成临时文件唯一后缀（crypto/rand——随机源红线禁止 math/rand）。
// lifecycleTempSuffix yields a random suffix from crypto/rand.
func lifecycleTempSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败是系统级故障；空后缀会让 O_EXCL 撞名，但错误由 O_EXCL 兜底
		// 为明确错误（不 panic，也不退化为可预测后缀）。
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}
