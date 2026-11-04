package flow

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ymedword/basalt/internal/core"
)

// TaskStatus 是持久任务状态，与 core.RunStatus 独立。
// TaskStatus is independent of the existing Run status vocabulary.
type TaskStatus string

const (
	// Queued 是已持久接受、尚待推进的任务。
	// Queued means durably accepted and awaiting advancement.
	Queued TaskStatus = "queued"
	// Running 是当前持有执行许可的任务。
	// Running means an execution permit is held.
	Running TaskStatus = "running"
	// Succeeded 表示全部必需验收通过。
	// Succeeded means every required verification passed.
	Succeeded TaskStatus = "succeeded"
	// Failed 表示已知失败，不推导未发生副作用。
	// Failed reports a known failure without asserting absence of side effects.
	Failed TaskStatus = "failed"
	// Stopped 表示预算、截止或无进展等规则停止。
	// Stopped is a bounded policy stop, including budget and deadline.
	Stopped TaskStatus = "stopped"
	// Blocked 表示无法证明可安全推进，保留未知事实。
	// Blocked preserves an unresolved safety or recovery condition.
	Blocked TaskStatus = "blocked"
	// Cancelling 表示用户关闭意图已提交但本地执行尚未收束。
	// Cancelling retains a close intent while local work is joining.
	Cancelling TaskStatus = "cancelling"
	// Cancelled 是实际退出后的用户取消终态。
	// Cancelled is the joined user-cancellation terminal state.
	Cancelled TaskStatus = "cancelled"
	// Abandoned 是管理救援放弃后的终态，unknown 不变。
	// Abandoned closes administrative rescue without changing unknown outcomes.
	Abandoned TaskStatus = "abandoned"
)

var (
	// ErrTaskClosed 拒绝终态或带关闭意图的任务复活。
	// ErrTaskClosed rejects reviving terminal or closing tasks.
	ErrTaskClosed = errors.New("flow task closed")
	// ErrStateConflict 拒绝旧状态、revision 或执行代次的写入。
	// ErrStateConflict rejects stale state, revision or epoch writes.
	ErrStateConflict = errors.New("flow state conflict")
	// ErrActionUnknown 拒绝未决逻辑动作被重放。
	// ErrActionUnknown rejects replay of an unresolved logical action.
	ErrActionUnknown = errors.New("flow action unknown")
	// ErrCapacity 表示持久容量耗尽，不是无限排队。
	// ErrCapacity is a bounded durable-capacity refusal.
	ErrCapacity = errors.New("flow capacity exhausted")
	// ErrDrainTimeout 表示排空宽限已过；真实join仍必须完成，取消不能吞掉此失败。
	// ErrDrainTimeout reports grace expiry without manufacturing a join.
	ErrDrainTimeout = errors.New("flow drain timeout")
	// ErrInteractiveReserve refuses new acceptance when the configured caller
	// sublimit leaves no Flow slot. It does not represent transient occupancy.
	ErrInteractiveReserve = errors.New("flow interactive reserve leaves no capacity")
	// ErrBudget 表示祖先预算不可再分配。
	// ErrBudget prevents further ancestor allocations.
	ErrBudget = core.ErrFlowBudget
	// ErrNotFound 隐藏跨调用方任务的存在性。
	// ErrNotFound also hides cross-caller task existence.
	ErrNotFound = errors.New("flow task not found")
	// ErrDefinitionInvalid 表示发布结构、路线或规则无效。
	// ErrDefinitionInvalid reports invalid publication content.
	ErrDefinitionInvalid = errors.New("flow definition invalid")
	// ErrCheckpointUnsupported 保留而不猜测未知恢复格式。
	// ErrCheckpointUnsupported preserves unknown checkpoint formats.
	ErrCheckpointUnsupported = errors.New("flow checkpoint unsupported")
	// ErrDeadline 拒绝续期或已过期的新动作。
	// ErrDeadline rejects new dispatch after the immutable deadline.
	ErrDeadline = errors.New("flow deadline expired")
	// ErrCommitUnknown 要求按原事务事实查证，不能用新键重做。
	// ErrCommitUnknown requires reconciliation of the original transaction.
	ErrCommitUnknown = errors.New("flow commit outcome unknown")
	// ErrDefinitionNotFound 不泄露未发布定义内容。
	// ErrDefinitionNotFound reports a missing publication.
	ErrDefinitionNotFound = errors.New("flow definition not found")
	// ErrDefinitionVersionNotFound 区别不存在版本与保留旧版。
	// ErrDefinitionVersionNotFound distinguishes an absent version from an old one.
	ErrDefinitionVersionNotFound = errors.New("flow definition version not found")
	// ErrDefinitionRetired 拒绝退役定义的新接受。
	// ErrDefinitionRetired rejects new acceptance of a retired definition.
	ErrDefinitionRetired = errors.New("flow definition retired")
	// ErrDefinitionNotCurrent 拒绝用新键固定非当前版本。
	// ErrDefinitionNotCurrent rejects new keys targeting an old version.
	ErrDefinitionNotCurrent = errors.New("flow definition not current")
	// ErrSchemaUnsupported distinguishes invalid or unsupported author schemas
	// from a valid schema rejecting a particular task input.
	ErrSchemaUnsupported = errors.New("flow schema unsupported")
	// ErrIdempotencyConflict 拒绝同键换输入或版本。
	// ErrIdempotencyConflict rejects changing the meaning of an existing key.
	ErrIdempotencyConflict = errors.New("flow idempotency conflict")
)

// TaskVersion 是每次条件写必须同时匹配的三元版本。
// TaskVersion is the version tuple required by every conditional mutation.
type TaskVersion struct {
	Revision       int64 `json:"revision"`
	StateVersion   int64 `json:"state_version"`
	ExecutionEpoch int64 `json:"execution_epoch"`
}

// ETag 把任务身份与完整条件版本编码为不透明标签，不承担授权。
// ETag binds task identity and the full version tuple without granting authority.
func (v TaskVersion) ETag(taskID string) string {
	value := fmt.Sprintf("%s:%d:%d:%d", taskID, v.Revision, v.StateVersion, v.ExecutionEpoch)
	return `"` + base64.RawURLEncoding.EncodeToString([]byte(value)) + `"`
}

// Terminal 判断是否已不可复活，不按“非running”推导完成。
// Terminal reports irreversible completion, not merely absence of a running state.
func (s TaskStatus) Terminal() bool {
	return s == Succeeded || s == Failed || s == Stopped || s == Cancelled || s == Abandoned
}

// Valid 判定持久状态是否属于协议闭集。
// Valid recognizes only the durable task status vocabulary.
func (s TaskStatus) Valid() bool {
	return s == Queued || s == Running || s == Blocked || s == Cancelling || s.Terminal()
}

// CanTransition 是协议封闭转移矩阵；取消不能被 deadline 改写为 stopped。
// CanTransition is the closed transition matrix, excluding cancelling-to-stopped.
func CanTransition(from, to TaskStatus) bool {
	switch from {
	case Queued:
		return to == Running || to == Cancelled || to == Abandoned || to == Stopped
	case Running:
		return to == Succeeded || to == Failed || to == Stopped || to == Blocked || to == Cancelling
	case Cancelling:
		return to == Cancelled || to == Abandoned || to == Blocked
	case Blocked:
		return to == Queued || to == Cancelled || to == Abandoned || to == Stopped
	default:
		return false
	}
}

// CloseIntent 是与持久取消位共同维护的封闭关闭意图。
// CloseIntent is maintained atomically with the persistent cancellation bit.
type CloseIntent string

const (
	// CloseNone 表示没有主动关闭意图。
	// CloseNone means no close intent.
	CloseNone CloseIntent = "none"
	// CloseCancel 表示调用方取消。
	// CloseCancel means caller cancellation.
	CloseCancel CloseIntent = "cancel"
	// CloseAbandon 表示管理员放弃。
	// CloseAbandon means administrative abandonment.
	CloseAbandon CloseIntent = "abandon"
	// CloseDeadline 表示墙钟过期，不伪造用户取消。
	// CloseDeadline means wall-clock expiry, not user cancellation.
	CloseDeadline CloseIntent = "deadline"
)

// ValidCloseIntent 校验四种合法意图／取消位配对及 cancelling 附加约束。
// ValidCloseIntent validates the four legal intent/bit pairs and cancelling constraint.
func ValidCloseIntent(status TaskStatus, intent CloseIntent, cancelRequested bool) bool {
	if !status.Valid() {
		return false
	}
	if intent != CloseNone && intent != CloseCancel && intent != CloseAbandon && intent != CloseDeadline {
		return false
	}
	if cancelRequested != (intent == CloseCancel || intent == CloseAbandon) {
		return false
	}
	return status != Cancelling || cancelRequested
}

// Task 是跨 Run 的权威工作单元；其终态不改变逻辑动作的 unknown。
// Task is the authoritative cross-Run work unit; closing it never resolves actions.
type Task struct {
	NewlyAccepted     bool              `json:"-"` // Per-call acceptance result, not persisted authority.
	ID                string            `json:"task_id"`
	RootTaskID        string            `json:"root_task_id"`
	ParentTaskID      string            `json:"parent_task_id,omitempty"`
	CallerID          string            `json:"caller_id"`
	AuthorizingKind   string            `json:"authorizing_kind"`
	AuthorizingKeyID  string            `json:"authorizing_key_id,omitempty"`
	DefinitionUID     string            `json:"definition_uid"`
	DefinitionVersion int64             `json:"definition_version"`
	ContractVersion   int64             `json:"contract_version"`
	Status            TaskStatus        `json:"status"`
	Version           TaskVersion       `json:"version"`
	CancelRequested   bool              `json:"cancel_requested"`
	CloseIntent       CloseIntent       `json:"close_intent"`
	Deadline          int64             `json:"deadline"`
	CreatedAt         int64             `json:"created_at"`
	UpdatedAt         int64             `json:"updated_at"`
	NextEligibleAt    int64             `json:"next_eligible_at"`
	IdempotencyKey    string            `json:"-"`
	RequestHash       string            `json:"-"`
	AcceptedVersion   DefinitionVersion `json:"-"`
	Contract          TaskContract      `json:"-"`
	Revision          TaskRevision      `json:"revision"`
	Reason            string            `json:"reason,omitempty"`
	Depth             int               `json:"depth"`
	ChildrenCreated   int               `json:"children_created"`
}

// TaskRevision 是唯一当前头指向的检查点；旧 revision 不被原地重写。
// TaskRevision is a checkpoint selected by the single current head.
type TaskRevision struct {
	FormatVersion int                            `json:"format_version"`
	TaskID        string                         `json:"task_id"`
	Revision      int64                          `json:"revision"`
	NodeID        string                         `json:"node_id"`
	Input         json.RawMessage                `json:"input"`
	InputHash     string                         `json:"input_hash"`
	Outputs       map[string]NodeOutput          `json:"outputs"`
	Slots         map[string]Slot                `json:"slots"`
	Summary       TaskSummary                    `json:"summary"`
	Attempts      map[string]int                 `json:"attempts"`
	Reworks       map[string]int                 `json:"reworks"`
	ExpandedSteps int                            `json:"expanded_steps"`
	Progress      map[string]core.ProgressWindow `json:"progress,omitempty"`
}

// NodeOutput 是完成且通过结构校验后才可映射的版本化上游输出。
// NodeOutput is a versioned upstream result usable only after validation.
type NodeOutput struct {
	Value         json.RawMessage `json:"value"`
	Hash          string          `json:"hash"`
	NodeAttemptID string          `json:"node_attempt_id"`
	Revision      int64           `json:"revision"`
	Verified      bool            `json:"verified"`
	Claims        []ClaimRef      `json:"claims,omitempty"`
}

// Slot 是程序维护的证据状态，模型不得直接设置 verified。
// Slot is trusted evidence state; models cannot directly mark it verified.
type Slot struct {
	ID              string   `json:"id"`
	State           string   `json:"state"`
	EvidenceIDs     []string `json:"evidence_ids"`
	VerificationIDs []string `json:"verification_ids"`
}

// TaskSummary 是带版本的有界派生视图，不是第二个任务状态机。
// TaskSummary is a bounded versioned projection, not another task authority.
type TaskSummary struct {
	TaskRevision      int64    `json:"task_revision"`
	Goal              string   `json:"goal"`
	HardConstraints   []string `json:"hard_constraints"`
	Progress          []string `json:"progress"`
	Unresolved        []string `json:"unresolved"`
	AcceptedArtifacts []string `json:"accepted_artifacts"`
	BasisRefs         []string `json:"basis_refs"`
}

// CloseState 计算关闭的安全投影；只使用实际 join 证明，不把超时当退出。
// CloseState derives safe closure from actual join proof, never from a timeout.
func CloseState(task Task, requested CloseIntent, joined, adminRescue bool) (TaskStatus, CloseIntent, bool, error) {
	if task.Status.Terminal() {
		return task.Status, task.CloseIntent, task.CancelRequested, ErrTaskClosed
	}
	if !ValidCloseIntent(task.Status, task.CloseIntent, task.CancelRequested) {
		return task.Status, task.CloseIntent, task.CancelRequested, ErrStateConflict
	}
	intent := requested
	if task.CloseIntent == CloseCancel {
		intent = CloseCancel
		if !joined && adminRescue && requested == CloseAbandon {
			intent = CloseAbandon
		}
	} else if task.CloseIntent == CloseAbandon {
		intent = CloseAbandon
	}
	if intent == CloseNone || !ValidCloseIntent(Blocked, intent, intent == CloseCancel || intent == CloseAbandon) {
		return task.Status, task.CloseIntent, task.CancelRequested, ErrStateConflict
	}
	if !joined {
		if task.Status == Queued {
			return Queued, intent, intent == CloseCancel || intent == CloseAbandon, nil
		}
		if task.Status == Blocked {
			return Blocked, intent, intent == CloseCancel || intent == CloseAbandon, nil
		}
		if intent == CloseCancel || task.Status == Cancelling && intent == CloseAbandon {
			return Cancelling, intent, true, nil
		}
		return Blocked, intent, intent == CloseAbandon, nil
	}
	switch intent {
	case CloseCancel:
		return Cancelled, intent, true, nil
	case CloseAbandon:
		return Abandoned, intent, true, nil
	case CloseDeadline:
		return Stopped, intent, false, nil
	default:
		return task.Status, task.CloseIntent, task.CancelRequested, ErrStateConflict
	}
}
