package workspace

// 本文件是 Agent 配置内容的**条件读写面**（颗粒度文档 §3.1.13 / 约束 25–27）：
// 读取给出原文与强 ETag；写入必须带 If-Match，旧 ETag 一律 412 且磁盘零写入。
// 读写都在 Agent 级**共享锁**内完成——与运行准入、生命周期启停共用同一把
// `core.AgentGate`：读锁保证"比对的字节"与"写入的字节"出自同一次读取（约束 25），
// 写锁保证并发保存只有一个成功（约束 27）。
// This file is the conditional read/write surface of AGENT.md: the read publishes
// the raw bytes plus a strong ETag under the shared per-agent read lock, and the
// write re-reads, compares, validates and publishes under the same agent's write
// lock, so compare-and-swap cannot interleave with another writer.
//
// 写路径严格按图 D 的顺序：ETag 比较 → 生成候选（整份或补丁）→ 装载期校验 →
// 快照落库 → 预写 pending 管理操作 → 原子写（同目录备份）→ 注册表替换 → 调度同步
// → 回填终态。任何目标写入之前必须先有 pending 行（宪法 V）；rename 之后的失败
// 从同目录 `.bak` 逐字节回滚，回滚本身失败则操作标为 `unknown` 并留 ERROR（约束 27）。
// The write path follows figure D; the pending operation row precedes every target
// write, and a post-rename failure is rolled back byte-for-byte from the sibling
// backup, with an unrecoverable rollback marked `unknown`.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	"github.com/ymedword/basalt/internal/core"
)

// ErrEditUnsupported 表示结构化补丁无法安全保留原文（块内注释 / 锚点 / 流式集合 /
// 未知字段）：调用方必须改用整份原文，**不得**整份重新生成、重排或丢注释（约束 26）。
// ErrEditUnsupported marks a patch the line-surgical writer cannot preserve; the
// caller must fall back to whole-file replacement instead of regenerating the file.
var ErrEditUnsupported = errors.New("edit unsupported")

// contentOperationAction 是内容写的操作动作字面量（契约 §9.3 / I-52）。
// contentOperationAction is the operation action literal for content updates.
const contentOperationAction = "agent.content.update"

// 终态结果码（契约 §9.3 的固定集合）：成功 `OK`，其余一律 `INTERNAL`——预写之后的
// 失败都属于服务端内部故障，对外只回固定文案（约束 23）。
// Terminal result codes: success is OK; every failure after the pre-write is INTERNAL.
const (
	resultCodeOK       = "OK"
	resultCodeInternal = "INTERNAL"
)

// ContentETag 给出内容的强 ETag：`"sha256:<64 位小写十六进制>"`（含双引号，逐字
// 口径见约束 25 与 docs/contracts/openapi.yaml）。对原始 UTF-8 字节计算，不做任何
// 归一化——同一份字节永远得到同一个标签。
// ContentETag renders the strong ETag as a quoted `sha256:` digest over the raw
// bytes; no normalisation happens anywhere on this path.
func ContentETag(content []byte) string {
	return `"` + checksumPrefix + sha256Hex(content) + `"`
}

// EntryReplacer 是注册表的原子替换面（实现 = internal/app 的 agentRegistry）：
// 内容写成功后用**新装载的 entry** 原子替换旧快照，在跑的 Run 不受影响（约束 27）。
// EntryReplacer swaps one registry entry atomically; running runs keep their snapshots.
type EntryReplacer interface {
	Replace(name string, entry core.AgentEntry)
}

// ScheduleReconciler 是调度器的按 Agent 同步面（实现 = core.Scheduler）：
// 配置变更后按新的 schedules 增减该 Agent 的定时，**不得**整体停机重启。
// ScheduleReconciler reconciles one agent's schedules after a config change.
type ScheduleReconciler interface {
	ReconcileAgent(ctx context.Context, agent string, schedules []core.Schedule) error
}

// entryLookup 是注册表的可选读面：回滚时优先取写前快照，取不到才重新校验旧字节。
// 用可选接口而不是扩 `EntryReplacer`——替换是本面的唯一必需能力，读只是回滚的
// 加速路径（Go 的隐式接口让具体实现顺带满足，不产生第二套端口）。
// entryLookup is the optional read side used only to restore the old entry on
// rollback; it does not widen the required EntryReplacer contract.
type entryLookup interface {
	Lookup(name string) (core.AgentEntry, bool)
}

// renameFunc 是原子 rename 的接缝（默认 `os.Root.Rename`）：测试用它注入
// "替换失败 / 回滚失败"两个红态，不必依赖平台相关的只读目录行为。
// renameFunc is the rename seam; tests inject replacement and rollback failures.
type renameFunc func(root *os.Root, oldname, newname string) error

// AgentEditorDeps 是内容编辑器的依赖集合（颗粒度文档 §3.1.13 的八类依赖：
// root、gate、ops、snapshots、registry、scheduler、validate、now）。
// Validate 由装配层注入：它必须走与热加载**同一道**装载期校验与发布闸门，
// 返回新条目、段一正文与冻结清单（三者出自同一次候选字节）。
// AgentEditorDeps carries the editor dependencies; Validate must be the same
// load-time gate the hot-reload path uses and returns the new entry together with
// its section-one text and frozen manifest.
type AgentEditorDeps struct {
	// Root 是工作区根（`.basalt/` 本身）。
	Root string
	// Gate 必须是与运行准入、生命周期启停共用的同一个 Agent 级锁表。
	Gate *core.AgentGate
	// Ops 是管理操作仓储：**先写 pending、成功后再回填终态**（宪法 V）。
	Ops core.OperationStore
	// Snapshots 是运行级配置快照仓储：写盘之前落库，失败即不动盘。
	Snapshots core.SnapshotStore
	// Registry 是注册表的原子替换面。
	Registry EntryReplacer
	// Scheduler 可为 nil（不装配调度器的进程）；非 nil 时按新 schedules 同步。
	Scheduler ScheduleReconciler
	// Validate 是装载期校验与发布闸门：返回 (entry, 段一正文, 冻结清单, error)。
	Validate func(ctx context.Context, agent string, candidate []byte) (core.AgentEntry, string, []core.ManifestEntry, error)
	// Now 是注入时钟；nil 时退回 time.Now。
	Now func() time.Time
}

// AgentEditor 是 `core.AgentAdmin` 的内容读写实现（组合适配在 internal/app）：
// 所有文件访问经工作区根约束，所有读写都在共享 AgentGate 的锁内完成。
// AgentEditor implements the content read/write surface behind core.AgentAdmin;
// every file access goes through the workspace root and the shared gate.
type AgentEditor struct {
	root      string
	gate      *core.AgentGate
	ops       core.OperationStore
	snapshots core.SnapshotStore
	registry  EntryReplacer
	scheduler ScheduleReconciler
	validate  func(ctx context.Context, agent string, candidate []byte) (core.AgentEntry, string, []core.ManifestEntry, error)
	now       func() time.Time
	rename    renameFunc
}

// NewAgentEditor 组装内容编辑器；gate 为 nil 时退化为私有锁表（仅串行化本编辑器
// 自身的并发，**不**提供与准入的互斥——生产装配必须传共享实例，与 LifecycleStore
// 同一口径）。
// NewAgentEditor assembles the editor; a nil gate falls back to a private gate,
// which never provides mutual exclusion against the admission path.
func NewAgentEditor(deps AgentEditorDeps) *AgentEditor {
	gate := deps.Gate
	if gate == nil {
		gate = core.NewAgentGate()
	}
	return &AgentEditor{
		root:      deps.Root,
		gate:      gate,
		ops:       deps.Ops,
		snapshots: deps.Snapshots,
		registry:  deps.Registry,
		scheduler: deps.Scheduler,
		validate:  deps.Validate,
		now:       deps.Now,
	}
}

// ReadContent 在 Agent 级读锁内读取 AGENT.md 原文并计算强 ETag（约束 25）：
// 返回的 `Content` 与 `ETag` 出自同一次读取，且读取期间没有同 Agent 的内容写或
// 启停写穿插。Agent 不存在（目录或 AGENT.md 缺失）时返回包装了 `fs.ErrNotExist`
// 的错误——HTTP 层据此映射 404，**不得**把缺失当成空内容。
// ReadContent reads the raw AGENT.md under the shared read lock; the returned
// bytes and ETag come from one read, and a missing agent wraps fs.ErrNotExist.
func (e *AgentEditor) ReadContent(ctx context.Context, agent string) (content core.AgentContent, err error) {
	if err := ctx.Err(); err != nil {
		return core.AgentContent{}, err
	}
	if err := ValidateAgentName(agent); err != nil {
		return core.AgentContent{}, err
	}

	release := e.gate.RLock(agent)
	defer release()

	base, err := os.OpenRoot(e.root)
	if err != nil {
		return core.AgentContent{}, fmt.Errorf("打开工作区目录 %s: %w", e.root, err)
	}
	defer func() {
		// 读取已完成：关闭失败不改变已读出的字节，但**不得**静默丢弃（H1）。
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", e.root, closeErr))
		}
	}()

	defPath := agentDefinitionPath(agent)
	raw, err := base.ReadFile(filepath.FromSlash(defPath))
	if err != nil {
		return core.AgentContent{}, fmt.Errorf("读取 Agent 正文 %s: %w", defPath, err)
	}
	return core.AgentContent{Content: string(raw), ETag: ContentETag(raw)}, nil
}

// contentOperationDetail 是内容写操作记录里的查证依据（键名由 I-34 固定）：
// 启动恢复拿 `candidate_sha256` 与磁盘上的 ETag 比对，判断变更是否已生效。
// contentOperationDetail is the recovery evidence recorded in detail_json.
type contentOperationDetail struct {
	CandidateSHA256 string `json:"candidate_sha256"`
	PreviousSHA256  string `json:"previous_sha256"`
}

// UpdateContent 是一次条件写的完整流程（图 D）：
//
//	① 写锁内读当前字节并算 ETag；
//	② If-Match 缺失 / 非法 / 弱 / `*` / 不等 → 412（零写入、不写操作行）；
//	③ 生成候选（整份替换或行级补丁；补丁无法安全保留 → 400 EDIT_UNSUPPORTED）；
//	④ 装载期校验（与热加载同一道闸门）→ 失败 400 且磁盘逐字节不变；
//	⑤ 快照落库 → 预写 pending 管理操作 → 原子写（同目录 `.bak` 备份）；
//	⑥ 替换注册表、同步调度；任一步失败按约束 27 回滚；
//	⑦ 回填终态并返回受管文件集合。
//
// UpdateContent runs the conditional write; preconditions are checked before any
// intent is recorded, and every post-rename failure is rolled back from the backup.
func (e *AgentEditor) UpdateContent(ctx context.Context, agent string, update core.AgentContentUpdate, ifMatch string, actor core.Actor) (core.AgentFiles, error) {
	if err := ctx.Err(); err != nil {
		return core.AgentFiles{}, err
	}
	if err := ValidateAgentName(agent); err != nil {
		return core.AgentFiles{}, err
	}
	if update.Changes != nil && update.Content != "" {
		return core.AgentFiles{}, &core.InvalidInputError{Tool: "agent.content", Reason: "content 与 changes 必须恰给一个"}
	}
	if update.Changes == nil && update.Content == "" {
		return core.AgentFiles{}, &core.InvalidInputError{Tool: "agent.content", Reason: "必须提供 content 或 changes"}
	}
	if err := e.requireWriteDeps(); err != nil {
		return core.AgentFiles{}, err
	}

	release := e.gate.Lock(agent)
	defer release()

	base, err := os.OpenRoot(e.root)
	if err != nil {
		return core.AgentFiles{}, fmt.Errorf("打开工作区目录 %s: %w", e.root, err)
	}
	defer func() {
		// 写入已结束：关闭失败不改变磁盘结果，留 WARN 即可（不得静默丢弃）。
		if closeErr := base.Close(); closeErr != nil {
			slog.Default().Warn("关闭工作区目录失败", "root", e.root, "error", closeErr)
		}
	}()

	defPath := agentDefinitionPath(agent)
	current, err := base.ReadFile(filepath.FromSlash(defPath))
	if err != nil {
		return core.AgentFiles{}, fmt.Errorf("读取 Agent 正文 %s: %w", defPath, err)
	}
	currentETag := ContentETag(current)
	if !strongETagMatches(ifMatch, currentETag) {
		return core.AgentFiles{}, fmt.Errorf("%w: 当前 ETag 为 %s", core.ErrPreconditionFailed, currentETag)
	}

	candidate := []byte(update.Content)
	if update.Changes != nil {
		candidate, err = ApplyChanges(current, *update.Changes)
		if err != nil {
			if errors.Is(err, ErrEditUnsupported) {
				reason := strings.TrimPrefix(err.Error(), ErrEditUnsupported.Error()+": ")
				return core.AgentFiles{}, fmt.Errorf("%w: %s", core.ErrEditUnsupported, reason)
			}
			return core.AgentFiles{}, err
		}
	}

	entry, section, manifest, err := e.validateCandidate(ctx, agent, candidate)
	if err != nil {
		return core.AgentFiles{}, &core.InvalidInputError{Tool: "agent.content", Reason: err.Error()}
	}

	now := e.nowFn()
	opID := core.NewOperationID(now)
	detail, err := json.Marshal(contentOperationDetail{
		CandidateSHA256: sha256Hex(candidate),
		PreviousSHA256:  sha256Hex(current),
	})
	if err != nil {
		return core.AgentFiles{}, fmt.Errorf("序列化操作明细: %w", err)
	}
	record := core.OperationRecord{
		OperationID:  opID,
		Action:       contentOperationAction,
		TargetKind:   "agent",
		TargetID:     agent,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  checksumPrefix + sha256Hex(candidate),
		ExpectedETag: currentETag,
		Status:       core.OperationPending,
		DetailJSON:   detail,
		CreatedAt:    now,
	}
	// 宪法 V：预写失败即拒绝执行——此刻磁盘尚未变更。
	if err := e.ops.BeginOperation(ctx, record); err != nil {
		slog.Default().Error("内容写预写失败，拒绝执行", "agent", agent, "operation_id", opID, "error", err)
		return core.AgentFiles{}, fmt.Errorf("预写管理操作 %s: %w", opID, err)
	}
	finish := func(status core.OperationStatus, code, message string) {
		// 终态回填不随请求取消而放弃：客户端断开不能让审计停在 pending。
		if err := e.ops.FinishOperation(context.WithoutCancel(ctx), opID, status, code, message, detail, e.nowFn()); err != nil {
			slog.Default().Error("内容写回填终态失败", "agent", agent, "operation_id", opID, "status", string(status), "error", err)
		}
	}

	snapshot, err := core.BuildConfigSnapshot(agent, candidate, section, entry.Profile, manifest)
	if err != nil {
		slog.Default().Error("内容写快照组装失败", "agent", agent, "operation_id", opID, "error", err)
		finish(core.OperationFailed, resultCodeInternal, "配置快照组装失败")
		return core.AgentFiles{}, fmt.Errorf("组装配置快照: %w", err)
	}
	if err := e.snapshots.SaveSnapshot(ctx, &snapshot); err != nil {
		slog.Default().Error("内容写快照落库失败", "agent", agent, "operation_id", opID, "error", err)
		finish(core.OperationFailed, resultCodeInternal, "配置快照落库失败")
		return core.AgentFiles{}, fmt.Errorf("保存配置快照: %w", err)
	}

	if err := writeContentAtomic(base, defPath, current, candidate, e.rename); err != nil {
		slog.Default().Error("内容写盘失败", "agent", agent, "operation_id", opID, "error", err)
		finish(core.OperationFailed, resultCodeInternal, "配置写入失败")
		return core.AgentFiles{}, err
	}

	oldEntry, haveOld := lookupEntry(e.registry, agent)
	e.registry.Replace(agent, entry)

	if e.scheduler != nil {
		if schedErr := e.scheduler.ReconcileAgent(ctx, agent, entry.Profile.Schedules); schedErr != nil {
			if rollbackErr := e.rollbackContent(ctx, base, defPath, agent, current, oldEntry, haveOld); rollbackErr != nil {
				slog.Default().Error("内容写回滚失败，配置可能未恢复",
					"agent", agent, "operation_id", opID, "error", errors.Join(schedErr, rollbackErr))
				finish(core.OperationUnknown, resultCodeInternal, "回滚失败，配置可能未恢复")
				return core.AgentFiles{}, fmt.Errorf("同步 Agent 定时失败且回滚失败（操作 %s 标为 unknown）: %w", opID, errors.Join(schedErr, rollbackErr))
			}
			slog.Default().Error("内容写调度同步失败，已回滚配置", "agent", agent, "operation_id", opID, "error", schedErr)
			finish(core.OperationFailed, resultCodeInternal, "调度同步失败，已回滚配置")
			return core.AgentFiles{}, fmt.Errorf("同步 Agent 定时（operation %s）: %w", opID, schedErr)
		}
	}

	finish(core.OperationSuccess, resultCodeOK, "")
	removeBackup(base, defPath)

	files, err := listManagedFiles(base, agent)
	if err != nil {
		// 写与发布都已成功：这次 500 只表示"响应视图没能组装"，操作行保持 success，
		// 客户端可用 GET 取新 ETag 后继续（**不得**把已生效的写入说成失败）。
		slog.Default().Error("内容写成功但文件清单读取失败", "agent", agent, "operation_id", opID, "error", err)
		return core.AgentFiles{}, fmt.Errorf("读取 Agent 文件清单（operation %s）: %w", opID, err)
	}
	return core.AgentFiles{Agent: agent, Files: files}, nil
}

// requireWriteDeps 在动任何文件之前确认写路径装配完整：缺依赖是装配错误，
// 必须在写锁外/写之前暴露，而不是等到中途才炸。
// requireWriteDeps refuses a half-wired editor before the write lock is taken.
func (e *AgentEditor) requireWriteDeps() error {
	switch {
	case e.ops == nil:
		return errors.New("内容编辑器缺少管理操作仓储")
	case e.snapshots == nil:
		return errors.New("内容编辑器缺少配置快照仓储")
	case e.registry == nil:
		return errors.New("内容编辑器缺少注册表替换面")
	case e.validate == nil:
		return errors.New("内容编辑器缺少装载期校验器")
	}
	return nil
}

// validateCandidate 走装载期校验并强制"已注册"不变量：未注册的候选一律按校验
// 失败处理，错误文案取装载器给的 reject 摘要（用户可读、无路径）。
// validateCandidate enforces the registered invariant on top of the injected gate.
func (e *AgentEditor) validateCandidate(ctx context.Context, agent string, candidate []byte) (core.AgentEntry, string, []core.ManifestEntry, error) {
	entry, section, manifest, err := e.validate(ctx, agent, candidate)
	if err != nil {
		return core.AgentEntry{}, "", nil, err
	}
	if !entry.Registered || entry.Profile == nil {
		reason := entry.RejectReason
		if reason == "" {
			reason = "候选配置未通过装载期校验"
		}
		return core.AgentEntry{}, "", nil, errors.New(reason)
	}
	return entry, section, manifest, nil
}

// rollbackContent 按约束 27 回滚后置失败：文件 → 注册表 → 调度。回滚不依赖请求
// 的取消信号（客户端断开也必须收拾干净），因此一律用 `context.WithoutCancel`。
// rollbackContent restores file, registry and schedules, detached from the request
// cancellation so a disconnected client cannot strand a half-applied change.
func (e *AgentEditor) rollbackContent(ctx context.Context, base *os.Root, defPath, agent string, current []byte, old core.AgentEntry, haveOld bool) error {
	cleanupCtx := context.WithoutCancel(ctx)
	var errs []error

	if err := e.renameFn()(base, filepath.FromSlash(defPath+".bak"), filepath.FromSlash(defPath)); err != nil {
		errs = append(errs, fmt.Errorf("从备份恢复 %s: %w", defPath, err))
	}

	restored, ok := old, haveOld && old.Profile != nil
	if !ok {
		// 替换面不提供读时只能重新校验旧字节重建条目；失败 → 回滚不完整。
		entry, _, _, err := e.validateCandidate(cleanupCtx, agent, current)
		if err != nil {
			return errors.Join(append(errs, fmt.Errorf("重建旧注册表条目: %w", err))...)
		}
		restored, ok = entry, true
	}
	if ok {
		e.registry.Replace(agent, restored)
		if e.scheduler != nil {
			if err := e.scheduler.ReconcileAgent(cleanupCtx, agent, restored.Profile.Schedules); err != nil {
				errs = append(errs, fmt.Errorf("恢复调度: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// lookupEntry 取注册表里的写前快照；实现不提供读面时返回 ok=false。
// lookupEntry reads the pre-write entry when the registry exposes it.
func lookupEntry(registry EntryReplacer, agent string) (core.AgentEntry, bool) {
	if lookup, ok := registry.(entryLookup); ok {
		return lookup.Lookup(agent)
	}
	return core.AgentEntry{}, false
}

// nowFn 取注入时钟；nil 时退回 time.Now。
// nowFn returns the injected clock, falling back to time.Now.
func (e *AgentEditor) nowFn() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// renameFn 取注入的 rename 接缝；nil 时用 `os.Root.Rename`。
// renameFn returns the injected rename seam, defaulting to os.Root.Rename.
func (e *AgentEditor) renameFn() renameFunc {
	if e.rename != nil {
		return e.rename
	}
	return func(root *os.Root, oldname, newname string) error {
		return root.Rename(oldname, newname)
	}
}

// strongETagMatches 只接受与当前值逐字相等的强 ETag：缺失、`W/` 前缀、`*`、
// 裸哈希、前后空白或非法形状一律 false（约束 25）。
// strongETagMatches accepts only a well-formed strong ETag equal to the current one.
func strongETagMatches(ifMatch, current string) bool {
	return isStrongETag(ifMatch) && ifMatch == current
}

// isStrongETag 判定 `"sha256:<64 位小写十六进制>"`（含双引号）的逐字形态。
// isStrongETag checks the quoted lowercase-digest form.
func isStrongETag(value string) bool {
	const prefix = `"sha256:`
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, `"`) || len(value) != len(prefix)+64+1 {
		return false
	}
	for i := len(prefix); i < len(value)-1; i++ {
		ch := value[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

// writeContentAtomic 写同目录备份与唯一名临时文件，再原子 rename 覆盖目标（约束 27）：
// `.bak` 是旧字节，替换后保留到调度同步成功（回滚依据）；任何一步失败都清理临时
// 文件并保留原文件。备份允许覆盖上一次崩溃留下的残留（O_TRUNC，不用 O_EXCL）。
// writeContentAtomic writes the sibling backup and a unique temp file, then renames
// the temp over the target; the backup survives until the schedules are reconciled.
func writeContentAtomic(base *os.Root, rel string, current, candidate []byte, rename renameFunc) error {
	dir, file := path.Split(rel)
	tmp := path.Join(dir, file+".tmp-"+scheduleTempSuffix())
	bak := rel + ".bak"

	if err := writeWorkspaceFile(base, bak, current); err != nil {
		return fmt.Errorf("写备份 %s: %w", bak, err)
	}

	f, err := base.OpenFile(filepath.FromSlash(tmp), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return dropBackup(base, bak, fmt.Errorf("建临时文件 %s: %w", tmp, err))
	}
	if _, err := f.Write(candidate); err != nil {
		return dropBackup(base, bak, scheduleTempFailure(base, tmp, fmt.Errorf("写入 %s: %w", tmp, err), f))
	}
	if err := f.Sync(); err != nil {
		return dropBackup(base, bak, scheduleTempFailure(base, tmp, fmt.Errorf("刷盘 %s: %w", tmp, err), f))
	}
	if err := f.Close(); err != nil {
		return dropBackup(base, bak, scheduleTempFailure(base, tmp, fmt.Errorf("关闭临时文件 %s: %w", tmp, err), nil))
	}

	if rename == nil {
		rename = func(root *os.Root, oldname, newname string) error {
			return root.Rename(oldname, newname)
		}
	}
	if err := rename(base, filepath.FromSlash(tmp), filepath.FromSlash(rel)); err != nil {
		// rename 之前失败：目标仍是原字节，备份只是多余副本——清理掉，避免残留。
		return dropBackup(base, bak, scheduleTempFailure(base, tmp, fmt.Errorf("替换 %s: %w", rel, err), nil))
	}
	return nil
}

// dropBackup 在目标未被替换时清理备份：此时备份没有任何回滚价值，留下只会让
// `.basalt/agents/<name>/` 多出一份"看起来像回滚点"的残留。
// dropBackup removes the backup when the target was never replaced.
func dropBackup(base *os.Root, bak string, cause error) error {
	if removeErr := base.Remove(filepath.FromSlash(bak)); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		return errors.Join(cause, fmt.Errorf("清理备份 %s: %w", bak, removeErr))
	}
	return cause
}

// writeWorkspaceFile 覆盖写一份工作区文件并刷盘；失败时关闭并删除半截文件
// （备份写失败意味着还没有动目标，清理后磁盘与调用前一致）。
// writeWorkspaceFile overwrites one workspace file; a failure removes the partial file.
func writeWorkspaceFile(base *os.Root, rel string, data []byte) error {
	f, err := base.OpenFile(filepath.FromSlash(rel), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return scheduleTempFailure(base, rel, fmt.Errorf("写入 %s: %w", rel, err), f)
	}
	if err := f.Sync(); err != nil {
		return scheduleTempFailure(base, rel, fmt.Errorf("刷盘 %s: %w", rel, err), f)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭 %s: %w", rel, err)
	}
	return nil
}

// removeBackup 在调度同步成功后清理 `.bak`；失败只留 WARN（清单按注 19 过滤残留）。
// removeBackup deletes the backup after a fully successful publish.
func removeBackup(base *os.Root, defPath string) {
	bak := defPath + ".bak"
	if err := base.Remove(filepath.FromSlash(bak)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Default().Warn("内容写完成后清理备份失败", "path", bak, "error", err)
	}
}

// listManagedFiles 列出 Agent 目录根下的受管普通文件（注 19）：不递归技能工作副本、
// 不含 `.bak` / `.tmp-*` 残留；`os.File.ReadDir` 已按文件名升序，不再二次排序。
// listManagedFiles lists the managed regular files in the agent directory root only.
func listManagedFiles(base *os.Root, agent string) ([]core.FileEntry, error) {
	dirRel := path.Join(agentsDirName, agent)
	dir, err := base.Open(filepath.FromSlash(dirRel))
	if err != nil {
		return nil, fmt.Errorf("打开 Agent 目录 %s: %w", dirRel, err)
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		_ = dir.Close()
		return nil, fmt.Errorf("列出 Agent 目录 %s: %w", dirRel, err)
	}
	if closeErr := dir.Close(); closeErr != nil {
		return nil, fmt.Errorf("关闭 Agent 目录 %s: %w", dirRel, closeErr)
	}

	files := make([]core.FileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".bak") || strings.Contains(name, ".tmp-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("读取 %s 的文件信息: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		files = append(files, core.FileEntry{Path: name, Size: info.Size()})
	}
	return files, nil
}

// ---------------------------------------------------------------------------
// 结构化补丁：行级外科手术（约束 26）
// ---------------------------------------------------------------------------

// ApplyChanges 把结构化补丁应用到 AGENT.md 原文，返回新的原始字节：
// 只重写目标字段所在的行；未改的字段、正文、注释与行尾风格逐字节保留；目标块内
// 出现无法安全保留的构造（注释、锚点 / 别名、流式集合、块标量、未知字段、重复键）
// 时返回包装 `ErrEditUnsupported` 的错误——**不得**整份重新生成（约束 26）。
// ApplyChanges rewrites only the lines of the targeted fields; every other byte is
// preserved, and an unpreservable target block refuses with ErrEditUnsupported.
func ApplyChanges(original []byte, changes core.AgentContentChanges) ([]byte, error) {
	_, bodyBefore, err := splitFrontmatter(original)
	if err != nil {
		return nil, fmt.Errorf("%w: frontmatter 不可解析: %w", ErrEditUnsupported, err)
	}

	raw := slices.Clone(original)
	if changes.Description != nil {
		if raw, err = patchScalarField(raw, "description", yamlQuote(*changes.Description)); err != nil {
			return nil, err
		}
	}
	if changes.Identity != nil {
		var leaves []patchLeaf
		if changes.Identity.AgentName != nil {
			leaves = append(leaves, patchLeaf{"agent_name", yamlQuote(*changes.Identity.AgentName)})
		}
		if changes.Identity.Prompt != nil {
			leaves = append(leaves, patchLeaf{"prompt", yamlQuote(*changes.Identity.Prompt)})
		}
		if raw, err = patchMapField(raw, "identity", leaves); err != nil {
			return nil, err
		}
	}
	if changes.Provider != nil {
		var leaves []patchLeaf
		if changes.Provider.Name != nil {
			leaves = append(leaves, patchLeaf{"name", yamlQuote(*changes.Provider.Name)})
		}
		if changes.Provider.Model != nil {
			leaves = append(leaves, patchLeaf{"model", yamlQuote(*changes.Provider.Model)})
		}
		if changes.Provider.Temperature != nil {
			leaves = append(leaves, patchLeaf{"temperature", strconv.FormatFloat(*changes.Provider.Temperature, 'f', -1, 64)})
		}
		if raw, err = patchMapField(raw, "provider", leaves); err != nil {
			return nil, err
		}
	}
	for _, seq := range []struct {
		key   string
		items *[]string
	}{
		{"tools", changes.Tools},
		{"skills", changes.Skills},
		{"mcp_servers", changes.MCPServers},
		{"bootstrap", changes.Bootstrap},
	} {
		if seq.items == nil {
			continue
		}
		if raw, err = patchSequenceField(raw, seq.key, *seq.items); err != nil {
			return nil, err
		}
	}
	if changes.Settings != nil {
		var leaves []patchLeaf
		if changes.Settings.MaxIterations != nil {
			leaves = append(leaves, patchLeaf{"max_iterations", strconv.Itoa(*changes.Settings.MaxIterations)})
		}
		if changes.Settings.MaxHistoryTurns != nil {
			leaves = append(leaves, patchLeaf{"max_history_turns", strconv.Itoa(*changes.Settings.MaxHistoryTurns)})
		}
		if raw, err = patchMapField(raw, "settings", leaves); err != nil {
			return nil, err
		}
	}
	if changes.Schedules != nil {
		if raw, err = patchSchedulesField(raw, *changes.Schedules); err != nil {
			return nil, err
		}
	}
	if err := verifyPatched(original, raw, bodyBefore, changes); err != nil {
		return nil, err
	}
	return raw, nil
}

// patchLeaf 是一个待写入的映射叶子：键名 + 已渲染的单行 YAML 标量。
// patchLeaf is one map leaf: its key and its rendered single-line scalar.
type patchLeaf struct {
	key      string
	rendered string
}

// fenced 是 AGENT.md 的按行视图：lines 保留每行原始字节（SplitAfter，含终止符），
// open / closing 是两条围栏的行号，newline 是本文件的换行风格。
// fenced is the line view of AGENT.md with its fence indices and newline style.
type fenced struct {
	lines   []string
	open    int
	closing int
	newline string
}

// parseFenced 定位 frontmatter 围栏；缺失或不闭合 → ErrEditUnsupported（补丁路径
// 不做"帮用户补围栏"的猜测）。首行容忍 UTF-8 BOM。
// parseFenced locates the fences; a missing or open fence refuses the patch.
func parseFenced(raw []byte) (*fenced, error) {
	lines := strings.SplitAfter(string(raw), "\n")
	open, closing := -1, -1
	for i, line := range lines {
		content := scheduleLineContent(line)
		if i == 0 {
			// 必须写转义：字面量 BOM 会触发 Go 扫描器的「illegal byte order mark」。
			content = strings.TrimPrefix(content, string(rune(0xFEFF)))
		}
		if content != "---" {
			continue
		}
		if open < 0 {
			open = i
			continue
		}
		closing = i
		break
	}
	if open < 0 {
		return nil, fmt.Errorf("%w: 缺少 frontmatter 起始围栏", ErrEditUnsupported)
	}
	if closing < 0 {
		return nil, fmt.Errorf("%w: frontmatter 围栏未闭合", ErrEditUnsupported)
	}
	newline := "\n"
	if strings.HasSuffix(lines[open], "\r\n") {
		newline = "\r\n"
	}
	return &fenced{lines: lines, open: open, closing: closing, newline: newline}, nil
}

// bytes 还原原始字节（SplitAfter + Join 是无损往返）。
// bytes reconstructs the raw text losslessly.
func (f *fenced) bytes() []byte {
	return []byte(strings.Join(f.lines, ""))
}

// topBlock 是一个顶层键的块范围：start 是键行，end 为开区间（已排除段间空行）。
// topBlock is a top-level key block, with end excluding separator blank lines.
type topBlock struct {
	start int
	end   int
}

// locateTopBlock 找唯一的顶层键行并算出块尾；同名键出现两次 = 锚点不唯一，拒绝。
// locateTopBlock finds the unique key line; a duplicate key refuses the patch.
func (f *fenced) locateTopBlock(key string) (topBlock, bool, error) {
	start := -1
	for i := f.open + 1; i < f.closing; i++ {
		if !isTopLevelKey(scheduleLineContent(f.lines[i]), key) {
			continue
		}
		if start >= 0 {
			return topBlock{}, false, fmt.Errorf("%w: 顶层键 %s 出现多次", ErrEditUnsupported, key)
		}
		start = i
	}
	if start < 0 {
		return topBlock{}, false, nil
	}
	return topBlock{start: start, end: f.blockEnd(start)}, true, nil
}

// isTopLevelKey 判定一行是否为顶格的 `key:` / `key: value`。
// isTopLevelKey reports a column-zero `key:` line.
func isTopLevelKey(content, key string) bool {
	if strings.HasPrefix(content, " ") || strings.HasPrefix(content, "\t") {
		return false
	}
	return content == key+":" || strings.HasPrefix(content, key+": ")
}

// blockEnd 与 schedules 块同一套归属规则：缩进行与零缩进序列项属块内；顶格注释
// 按 lookahead 决定（后面仍是块内容才算块内）；段间空行留给下一段。
// blockEnd mirrors the schedules block attribution rules.
func (f *fenced) blockEnd(start int) int {
	for i := start + 1; i < f.closing; {
		content := scheduleLineContent(f.lines[i])
		switch {
		case content == "" || isIndentedOrItem(content):
			i++
		case strings.HasPrefix(content, "#"):
			j := i
			for j < f.closing {
				next := scheduleLineContent(f.lines[j])
				if next == "" || strings.HasPrefix(next, "#") {
					j++
					continue
				}
				break
			}
			if j < f.closing && isIndentedOrItem(scheduleLineContent(f.lines[j])) {
				i = j
				continue
			}
			return trimTrailingScheduleBlanks(f.lines, start+1, i)
		default:
			return trimTrailingScheduleBlanks(f.lines, start+1, i)
		}
	}
	return f.closing
}

// isIndentedOrItem 判定一行是否仍是块内容（缩进行或零缩进序列项）。
// isIndentedOrItem reports whether a line still belongs to the current block.
func isIndentedOrItem(content string) bool {
	return strings.HasPrefix(content, " ") || strings.HasPrefix(content, "\t") ||
		content == "-" || strings.HasPrefix(content, "- ")
}

// insertLine 在 at 处插入一行（保留原行字节顺序），并维护围栏下标。
// insertLine inserts one rendered line at the given index.
func (f *fenced) insertLine(at int, line string) {
	lines := make([]string, 0, len(f.lines)+1)
	lines = append(lines, f.lines[:at]...)
	lines = append(lines, line)
	lines = append(lines, f.lines[at:]...)
	f.lines = lines
	if at <= f.closing {
		f.closing++
	}
}

// replaceRange 用渲染文本替换 [start, end) 的行区间；替换后重新解析围栏，
// 让后续步骤拿到一致的下标。
// replaceRange swaps a line range and re-parses the fences.
func (f *fenced) replaceRange(start, end int, text string) ([]byte, error) {
	var b strings.Builder
	for _, line := range f.lines[:start] {
		b.WriteString(line)
	}
	b.WriteString(text)
	for _, line := range f.lines[end:] {
		b.WriteString(line)
	}
	next, err := parseFenced([]byte(b.String()))
	if err != nil {
		return nil, err
	}
	return next.bytes(), nil
}

// patchScalarField 改写一个顶层标量键：只换值，保留键的拼写、缩进（顶格）与行尾；
// 键缺失时按契约顺序插在收尾围栏之前。
// patchScalarField rewrites one top-level scalar, preserving the line's other bytes.
func patchScalarField(raw []byte, key, rendered string) ([]byte, error) {
	f, err := parseFenced(raw)
	if err != nil {
		return nil, err
	}
	blk, found, err := f.locateTopBlock(key)
	if err != nil {
		return nil, err
	}
	if !found {
		f.insertLine(f.closing, key+": "+rendered+f.newline)
		return f.bytes(), nil
	}

	content := scheduleLineContent(f.lines[blk.start])
	rest := content[len(key)+1:]
	if rest == "" || !strings.HasPrefix(rest, " ") {
		return nil, fmt.Errorf("%w: %s 的值不在同一行", ErrEditUnsupported, key)
	}
	value := strings.TrimLeft(rest, " ")
	if value == "" {
		return nil, fmt.Errorf("%w: %s 的值不在同一行", ErrEditUnsupported, key)
	}
	if hazard := yamlValueHazard(value); hazard != "" {
		return nil, fmt.Errorf("%w: %s %s", ErrEditUnsupported, key, hazard)
	}
	if blk.end > blk.start+1 {
		return nil, fmt.Errorf("%w: %s 含多行值", ErrEditUnsupported, key)
	}
	f.lines[blk.start] = key + ": " + rendered + lineTerminator(f.lines[blk.start])
	return f.bytes(), nil
}

// mapChild 是映射块内的一行子项。
// mapChild records one `key: value` child line of a map block.
type mapChild struct {
	index int
	key   string
	value string
}

// patchMapField 改写映射块内的叶子：已存在的键只换值（保留该行的缩进与行尾），
// 缺失的键按块内缩进补在块尾；块本身不存在时整块插在收尾围栏之前。
// patchMapField rewrites existing leaves in place and appends missing ones to the block.
func patchMapField(raw []byte, key string, leaves []patchLeaf) ([]byte, error) {
	if len(leaves) == 0 {
		return raw, nil
	}
	f, err := parseFenced(raw)
	if err != nil {
		return nil, err
	}
	blk, found, err := f.locateTopBlock(key)
	if err != nil {
		return nil, err
	}
	if !found {
		var b strings.Builder
		b.WriteString(key + ":" + f.newline)
		for _, leaf := range leaves {
			b.WriteString("  " + leaf.key + ": " + leaf.rendered + f.newline)
		}
		f.insertLine(f.closing, b.String())
		return f.bytes(), nil
	}

	children, indent, err := f.scanMapBlock(key, blk)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]int, len(children))
	for _, child := range children {
		existing[child.key] = child.index
	}
	for _, leaf := range leaves {
		if at, ok := existing[leaf.key]; ok {
			lead := strings.Repeat(" ", leadingSpaces(scheduleLineContent(f.lines[at])))
			f.lines[at] = lead + leaf.key + ": " + leaf.rendered + lineTerminator(f.lines[at])
			continue
		}
		f.insertLine(blk.end, strings.Repeat(" ", indent)+leaf.key+": "+leaf.rendered+f.newline)
		blk.end++
	}
	return f.bytes(), nil
}

// scanMapBlock 校验映射块可安全改写：块式（无行内值）、单一缩进、无注释、无制表符、
// 无序列项、无多行 / 嵌套值、无字段表之外的键、无重复键。任一不满足 → 拒绝。
// scanMapBlock refuses any construct the line-surgical writer cannot preserve.
func (f *fenced) scanMapBlock(key string, blk topBlock) ([]mapChild, int, error) {
	keyLine := scheduleLineContent(f.lines[blk.start])
	if strings.TrimSpace(strings.TrimPrefix(keyLine, key+":")) != "" {
		return nil, 0, fmt.Errorf("%w: %s 不是块式映射", ErrEditUnsupported, key)
	}

	indent := -1
	seen := make(map[string]bool)
	var children []mapChild
	for i := blk.start + 1; i < blk.end; i++ {
		content := scheduleLineContent(f.lines[i])
		if content == "" {
			continue
		}
		if strings.Contains(content, "\t") {
			return nil, 0, fmt.Errorf("%w: %s 段含制表符", ErrEditUnsupported, key)
		}
		lead := leadingSpaces(content)
		body := content[lead:]
		if strings.HasPrefix(body, "#") {
			return nil, 0, fmt.Errorf("%w: %s 段含块内注释", ErrEditUnsupported, key)
		}
		if lead == 0 {
			return nil, 0, fmt.Errorf("%w: %s 段含顶层行", ErrEditUnsupported, key)
		}
		if indent < 0 {
			indent = lead
		} else if lead != indent {
			return nil, 0, fmt.Errorf("%w: %s 段缩进不一致", ErrEditUnsupported, key)
		}
		if strings.HasPrefix(body, "-") {
			return nil, 0, fmt.Errorf("%w: %s 段含序列项", ErrEditUnsupported, key)
		}
		childKey, childValue, ok := splitChildLine(body)
		if !ok {
			return nil, 0, fmt.Errorf("%w: %s 段含多行标量", ErrEditUnsupported, key)
		}
		if !knownChildKey(key, childKey) {
			return nil, 0, fmt.Errorf("%w: %s 段含字段表之外的键 %s", ErrEditUnsupported, key, childKey)
		}
		if seen[childKey] {
			return nil, 0, fmt.Errorf("%w: %s 段键 %s 重复", ErrEditUnsupported, key, childKey)
		}
		if childValue == "" {
			return nil, 0, fmt.Errorf("%w: %s.%s 不是单行标量", ErrEditUnsupported, key, childKey)
		}
		if hazard := yamlValueHazard(childValue); hazard != "" {
			return nil, 0, fmt.Errorf("%w: %s.%s %s", ErrEditUnsupported, key, childKey, hazard)
		}
		seen[childKey] = true
		children = append(children, mapChild{index: i, key: childKey, value: childValue})
	}
	if indent < 0 {
		indent = 2
	}
	return children, indent, nil
}

// splitChildLine 拆一行 `key: value`：键必须是字段名形态，值必须与冒号间有空格。
// 值里允许再出现冒号（第一个冒号才是分隔符）。
// splitChildLine splits a `key: value` line at its first colon.
func splitChildLine(body string) (key, value string, ok bool) {
	colon := strings.IndexByte(body, ':')
	if colon <= 0 {
		return "", "", false
	}
	key = body[:colon]
	if !isFieldName(key) {
		return "", "", false
	}
	rest := body[colon+1:]
	if rest == "" {
		return key, "", true
	}
	if !strings.HasPrefix(rest, " ") {
		return "", "", false
	}
	return key, strings.TrimLeft(rest, " "), true
}

// isFieldName 判定 YAML 字段名形态（字母 / 数字 / 下划线，首字符非数字）。
// isFieldName reports the plain field-name shape used by the contract.
func isFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z'):
		case i > 0 && ch >= '0' && ch <= '9':
		default:
			return false
		}
	}
	return true
}

// knownChildKey 是契约 §2 字段表在可补丁块内的子键白名单：表外的键一律拒绝
// （"未知字段 → EDIT_UNSUPPORTED"），不猜它是否该被保留。
// knownChildKey is the contract's field whitelist for patchable blocks.
func knownChildKey(parent, child string) bool {
	switch parent {
	case "identity":
		return child == "agent_name" || child == "prompt"
	case "provider":
		return child == "name" || child == "model" || child == "temperature"
	case "settings":
		return child == "max_iterations" || child == "max_history_turns"
	}
	return false
}

// patchSequenceField 替换顶层字符串序列：块式写法整块替换（保留块外一切字节），
// 空列表渲染为 `key: []`；流式集合、嵌套映射项、注释一律拒绝。
// patchSequenceField replaces a top-level string sequence block.
func patchSequenceField(raw []byte, key string, items []string) ([]byte, error) {
	f, err := parseFenced(raw)
	if err != nil {
		return nil, err
	}
	rendered := renderStringSequence(key, items, f.newline)
	blk, found, err := f.locateTopBlock(key)
	if err != nil {
		return nil, err
	}
	if !found {
		f.insertLine(f.closing, rendered)
		return f.bytes(), nil
	}

	content := scheduleLineContent(f.lines[blk.start])
	rest := strings.TrimSpace(content[len(key)+1:])
	if rest != "" && rest != "[]" {
		return nil, fmt.Errorf("%w: %s 不是块式序列", ErrEditUnsupported, key)
	}
	if rest == "" {
		indent := -1
		for i := blk.start + 1; i < blk.end; i++ {
			line := scheduleLineContent(f.lines[i])
			if line == "" {
				continue
			}
			if strings.Contains(line, "\t") {
				return nil, fmt.Errorf("%w: %s 段含制表符", ErrEditUnsupported, key)
			}
			lead := leadingSpaces(line)
			body := line[lead:]
			if strings.HasPrefix(body, "#") {
				return nil, fmt.Errorf("%w: %s 段含块内注释", ErrEditUnsupported, key)
			}
			if !strings.HasPrefix(body, "- ") {
				return nil, fmt.Errorf("%w: %s 段不是单行标量序列", ErrEditUnsupported, key)
			}
			if indent < 0 {
				indent = lead
			} else if lead != indent {
				return nil, fmt.Errorf("%w: %s 段缩进不一致", ErrEditUnsupported, key)
			}
			if hazard := yamlValueHazard(body[2:]); hazard != "" {
				return nil, fmt.Errorf("%w: %s 段 %s", ErrEditUnsupported, key, hazard)
			}
		}
	}
	return f.replaceRange(blk.start, blk.end, rendered)
}

// patchSchedulesField 复用 schedules 的唯一写入器（replaceSchedulesBlock），
// 但先按约束 26 判定块内注释 / 制表符——那个写入器会重建整块，块内注释会丢。
// patchSchedulesField reuses the single schedules writer after a comment pre-check.
func patchSchedulesField(raw []byte, schedules []core.Schedule) ([]byte, error) {
	f, err := parseFenced(raw)
	if err != nil {
		return nil, err
	}
	blk, found, err := f.locateTopBlock("schedules")
	if err != nil {
		return nil, err
	}
	if found {
		content := scheduleLineContent(f.lines[blk.start])
		rest := strings.TrimSpace(content[len("schedules")+1:])
		if rest != "" && rest != "[]" {
			return nil, fmt.Errorf("%w: schedules 不是块式序列", ErrEditUnsupported)
		}
		for i := blk.start + 1; i < blk.end; i++ {
			line := scheduleLineContent(f.lines[i])
			if strings.Contains(line, "\t") || hasUnquotedComment(line) {
				return nil, fmt.Errorf("%w: schedules 段含注释或制表符", ErrEditUnsupported)
			}
		}
	}
	return replaceSchedulesBlock(raw, schedules)
}

// renderStringSequence 渲染顶层字符串序列块：空列表用流式 `[]`（契约示例同形），
// 其余每个元素一行 `- "..."`——元素一律经 yamlQuote，值无法逃出标量。
// renderStringSequence renders a string sequence block; elements are always quoted.
func renderStringSequence(key string, items []string, newline string) string {
	if len(items) == 0 {
		return key + ": []" + newline
	}
	var b strings.Builder
	b.WriteString(key + ":" + newline)
	for _, item := range items {
		b.WriteString("  - " + yamlQuote(item) + newline)
	}
	return b.String()
}

// yamlValueHazard 返回单行值里"重写会改变语义"的 YAML 构造说明；空串表示安全。
// 只认真正的语义构造：注释必须由值首或空白引导，锚点 / 别名必须由值首或空白引导
// 且后接锚点字符，流式集合与块标量必须是值的第一个非空字符——`a=1&b=2` 这类
// 文本不算锚点，避免把真实 prompt 无谓拒掉（约束 26：无法安全保留才拒绝）。
// yamlValueHazard names a YAML construct that a rewrite would reinterpret.
func yamlValueHazard(value string) string {
	if strings.Contains(value, "\t") {
		return "含制表符"
	}
	if hasUnquotedComment(value) {
		return "含行内注释"
	}
	quote := byte(0)
	for i := 0; i < len(value); i++ {
		ch := value[i]
		switch quote {
		case '\'':
			if ch != '\'' {
				continue
			}
			if i+1 < len(value) && value[i+1] == '\'' {
				i++
				continue
			}
			quote = 0
			continue
		case '"':
			if ch == '\\' {
				i++
				continue
			}
			if ch == '"' {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'':
			quote = '\''
		case '"':
			quote = '"'
		case '&', '*':
			if (i == 0 || value[i-1] == ' ') && i+1 < len(value) && isAnchorByte(value[i+1]) {
				return "含锚点或别名"
			}
		}
	}
	trimmed := strings.TrimLeft(value, " ")
	if trimmed == "" {
		return ""
	}
	switch trimmed[0] {
	case '[', ']', '{', '}':
		return "含流式集合"
	case '|', '>':
		return "含块标量"
	}
	return ""
}

// hasUnquotedComment 判定引号外是否存在会被 YAML 当作注释起始的 `#`（值首或空白后）。
// hasUnquotedComment reports a comment indicator outside quotes.
func hasUnquotedComment(s string) bool {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch quote {
		case '\'':
			if ch != '\'' {
				continue
			}
			if i+1 < len(s) && s[i+1] == '\'' {
				i++
				continue
			}
			quote = 0
			continue
		case '"':
			if ch == '\\' {
				i++
				continue
			}
			if ch == '"' {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'':
			quote = '\''
		case '"':
			quote = '"'
		case '#':
			if i == 0 || s[i-1] == ' ' {
				return true
			}
		}
	}
	return false
}

// isAnchorByte 判定 YAML 锚点 / 别名字符集（字母、数字、下划线、连字符）。
// isAnchorByte reports the anchor-name character set.
func isAnchorByte(ch byte) bool {
	return ch == '_' || ch == '-' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}

// leadingSpaces 数行首空格数（制表符由调用方单独拒绝）。
// leadingSpaces counts leading spaces.
func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	return n
}

// lineTerminator 取行尾终止符（LF / CRLF / 空串），改写后的行沿用原行风格。
// lineTerminator returns the original line terminator.
func lineTerminator(line string) string {
	switch {
	case strings.HasSuffix(line, "\r\n"):
		return "\r\n"
	case strings.HasSuffix(line, "\n"):
		return "\n"
	default:
		return ""
	}
}

// verifyPatched 是补丁的**回读闸门**：正文逐字节不变、frontmatter 仍可解析、
// 每个被改字段回读等于请求值；任一不符 → ErrEditUnsupported（宁可拒绝，不静默
// 落一份与请求不一致的配置）。正文比较挡住"补丁越过围栏"这类结构性错误。
// verifyPatched re-reads the candidate: body bytes unchanged, frontmatter parses,
// and every requested field round-trips; otherwise the patch is refused.
func verifyPatched(original, patched, bodyBefore []byte, changes core.AgentContentChanges) error {
	_, bodyAfter, err := splitFrontmatter(patched)
	if err != nil {
		return fmt.Errorf("%w: 补丁后 frontmatter 不可解析: %w", ErrEditUnsupported, err)
	}
	if !bytes.Equal(bodyBefore, bodyAfter) {
		return fmt.Errorf("%w: 补丁改变了围栏外正文", ErrEditUnsupported)
	}

	front, _, err := splitFrontmatter(patched)
	if err != nil {
		return fmt.Errorf("%w: 补丁后 frontmatter 不可解析: %w", ErrEditUnsupported, err)
	}
	var dto frontmatterDTO
	if err := yaml.Unmarshal(front, &dto); err != nil {
		return fmt.Errorf("%w: 补丁后 frontmatter 不可解析: %w", ErrEditUnsupported, err)
	}
	mismatch := func(field string) error {
		return fmt.Errorf("%w: 补丁回读不一致（%s）", ErrEditUnsupported, field)
	}

	if changes.Description != nil && dto.Description != *changes.Description {
		return mismatch("description")
	}
	if changes.Identity != nil {
		if changes.Identity.AgentName != nil && dto.Identity.AgentName != *changes.Identity.AgentName {
			return mismatch("identity.agent_name")
		}
		if changes.Identity.Prompt != nil && dto.Identity.Prompt != *changes.Identity.Prompt {
			return mismatch("identity.prompt")
		}
	}
	if changes.Provider != nil {
		if changes.Provider.Name != nil && dto.Provider.Name != *changes.Provider.Name {
			return mismatch("provider.name")
		}
		if changes.Provider.Model != nil && dto.Provider.Model != *changes.Provider.Model {
			return mismatch("provider.model")
		}
		if changes.Provider.Temperature != nil &&
			(dto.Provider.Temperature == nil || *dto.Provider.Temperature != *changes.Provider.Temperature) {
			return mismatch("provider.temperature")
		}
	}
	if changes.Tools != nil && !slices.Equal(dto.Tools, *changes.Tools) {
		return mismatch("tools")
	}
	if changes.Skills != nil && !slices.Equal(dto.Skills, *changes.Skills) {
		return mismatch("skills")
	}
	if changes.MCPServers != nil && !slices.Equal(dto.MCPServers, *changes.MCPServers) {
		return mismatch("mcp_servers")
	}
	if changes.Bootstrap != nil && !slices.Equal(dto.Bootstrap, *changes.Bootstrap) {
		return mismatch("bootstrap")
	}
	if changes.Settings != nil {
		if changes.Settings.MaxIterations != nil &&
			(dto.Settings.MaxIterations == nil || *dto.Settings.MaxIterations != *changes.Settings.MaxIterations) {
			return mismatch("settings.max_iterations")
		}
		if changes.Settings.MaxHistoryTurns != nil &&
			(dto.Settings.MaxHistoryTurns == nil || *dto.Settings.MaxHistoryTurns != *changes.Settings.MaxHistoryTurns) {
			return mismatch("settings.max_history_turns")
		}
	}
	if changes.Schedules != nil {
		if len(dto.Schedules) != len(*changes.Schedules) {
			return mismatch("schedules")
		}
		for i, want := range *changes.Schedules {
			got := dto.Schedules[i]
			if got.ID != want.ID || got.Cron != want.Cron || got.Zone != want.Zone || got.Message != want.Message {
				return mismatch("schedules")
			}
		}
	}
	return nil
}
