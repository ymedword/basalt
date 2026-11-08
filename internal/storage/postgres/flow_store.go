package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

const (
	flowTaskColumns = `t.task_id,t.root_task_id,COALESCE(t.parent_task_id,''),t.caller_id,t.authorizing_kind,COALESCE(t.authorizing_key_id,''),t.definition_uid,t.definition_version,t.contract_version,t.status,t.state_version,t.execution_epoch,t.current_revision,t.cancel_requested,t.close_intent,t.deadline,t.created_at,t.updated_at,t.next_eligible_at,t.idempotency_key,t.request_hash,t.reason,t.depth,t.children_created,c.content,c.content_hash,c.definition_uid,c.definition_version,r.checkpoint`
	flowTaskJoin    = ` FROM flow_tasks t JOIN flow_task_contracts c ON c.task_id=t.task_id AND c.contract_version=t.contract_version JOIN flow_task_revisions r ON r.task_id=t.task_id AND r.revision=t.current_revision `
)

func scanFlowTask(ctx context.Context, q Queryer, row flowScanner) (flow.Task, error) {
	task, err := scanFlowTaskData(ctx, row)
	if err != nil {
		return task, err
	}
	task.AcceptedVersion, err = readAcceptedFlowDefinition(ctx, q, task)
	return task, err
}

func readAcceptedFlowDefinition(ctx context.Context, q Queryer, task flow.Task) (flow.DefinitionVersion, error) {
	return scanFlowDefinition(q.QueryRowContext(ctx, `SELECT `+flowDefinitionColumns+` FROM flow_definitions d JOIN flow_pointers p ON p.definition_uid=d.definition_uid WHERE d.definition_uid=$1 AND d.version=$2`, task.DefinitionUID, task.DefinitionVersion))
}

func scanFlowTaskData(ctx context.Context, row flowScanner) (flow.Task, error) {
	var task flow.Task
	var contract, checkpoint []byte
	var contractHash, contractDefinition string
	var contractDefinitionVersion int64
	err := row.Scan(&task.ID, &task.RootTaskID, &task.ParentTaskID, &task.CallerID, &task.AuthorizingKind, &task.AuthorizingKeyID, &task.DefinitionUID, &task.DefinitionVersion, &task.ContractVersion, &task.Status, &task.Version.StateVersion, &task.Version.ExecutionEpoch, &task.Version.Revision, &task.CancelRequested, &task.CloseIntent, &task.Deadline, &task.CreatedAt, &task.UpdatedAt, &task.NextEligibleAt, &task.IdempotencyKey, &task.RequestHash, &task.Reason, &task.Depth, &task.ChildrenCreated, &contract, &contractHash, &contractDefinition, &contractDefinitionVersion, &checkpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return task, flow.ErrNotFound
	}
	if err != nil {
		return task, err
	}
	if flowDataHash(contract) != contractHash || contractDefinition != task.DefinitionUID || contractDefinitionVersion != task.DefinitionVersion {
		return task, flow.ErrCheckpointUnsupported
	}
	task.Contract, err = flow.DecodeTaskContractJSON(ctx, contract)
	if err != nil {
		return task, err
	}
	if task.Contract.ContractVersion != task.ContractVersion {
		return task, flow.ErrCheckpointUnsupported
	}
	if err := json.Unmarshal(checkpoint, &task.Revision); err != nil {
		return task, err
	}
	// JSONB may rewrite whitespace/number spellings inside RawMessage. Restore
	// semantic canonical bytes before checking frozen input and output identities.
	//
	// 归一化失败**不得**一律当成"检查点损坏"：CanonicalJSON 在 ctx 结束时返回
	// ctx.Err()，若与哈希不符一起折成 ErrCheckpointUnsupported，正常停机就会把
	// `flow.Worker.Run` 变成致命错误（CI `-race` 019-flow-race 的偶发失败即这条
	// 路径：停机取消恰好落在契约解码之后、输入归一化之中）。取消原样上抛。
	// A cancelled canonicalization must propagate as cancellation, not corruption;
	// only a live context plus a real data fault maps to ErrCheckpointUnsupported.
	task.Revision.Input, err = flow.CanonicalJSON(ctx, task.Revision.Input)
	if err != nil {
		return task, flowCheckpointScanError(ctx, err)
	}
	if flowDataHash(task.Revision.Input) != task.Revision.InputHash {
		return task, flow.ErrCheckpointUnsupported
	}
	for key, output := range task.Revision.Outputs {
		output.Value, err = flow.CanonicalJSON(ctx, output.Value)
		if err != nil {
			return task, flowCheckpointScanError(ctx, err)
		}
		if flowDataHash(output.Value) != output.Hash {
			return task, flow.ErrCheckpointUnsupported
		}
		task.Revision.Outputs[key] = output
	}
	if task.Revision.FormatVersion != 1 || task.Revision.TaskID != task.ID || task.Revision.Revision != task.Version.Revision || task.Revision.Attempts == nil || task.Revision.Outputs == nil || task.Revision.Slots == nil || task.Revision.Reworks == nil || !flow.ValidCloseIntent(task.Status, task.CloseIntent, task.CancelRequested) {
		return task, flow.ErrCheckpointUnsupported
	}
	return task, nil
}

func readFlowTask(ctx context.Context, q Queryer, id string) (flow.Task, error) {
	return scanFlowTask(ctx, q, q.QueryRowContext(ctx, `SELECT `+flowTaskColumns+flowTaskJoin+` WHERE t.task_id=$1`, id))
}

func (s *FlowStore) Read(ctx context.Context, principal core.Principal, id string) (flow.Task, error) {
	if !principal.Valid() {
		return flow.Task{}, core.ErrUnauthorized
	}
	return scanFlowTask(ctx, s.db.pool, s.db.pool.QueryRowContext(ctx, `SELECT `+flowTaskColumns+flowTaskJoin+` WHERE t.task_id=$1 AND t.caller_id=$2`, id, principal.CallerID))
}

func (s *FlowStore) FindAccepted(ctx context.Context, principal core.Principal, key string) (flow.Task, error) {
	if !principal.Valid() {
		return flow.Task{}, core.ErrUnauthorized
	}
	return scanFlowTask(ctx, s.db.pool, s.db.pool.QueryRowContext(ctx, `SELECT `+flowTaskColumns+flowTaskJoin+` WHERE t.caller_id=$1 AND t.idempotency_key=$2`, principal.CallerID, key))
}

func (s *FlowStore) ReadForWorker(ctx context.Context, id string) (flow.Task, error) {
	return readFlowTask(ctx, s.db.pool, id)
}

func lockFlowRoot(ctx context.Context, tx *sql.Tx, root string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryKey("basalt/flow/root/"+root))
	return err
}

func lockedFlowTask(ctx context.Context, tx *sql.Tx, id string) (flow.Task, error) {
	var root string
	if err := tx.QueryRowContext(ctx, `SELECT root_task_id FROM flow_tasks WHERE task_id=$1`, id).Scan(&root); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return flow.Task{}, flow.ErrNotFound
		}
		return flow.Task{}, err
	}
	if err := lockFlowRoot(ctx, tx, root); err != nil {
		return flow.Task{}, err
	}
	return readFlowTask(ctx, tx, id)
}

func lockedFlowOwnedTask(ctx context.Context, tx *sql.Tx, id string, p core.Principal) (flow.Task, error) {
	if !p.Valid() {
		return flow.Task{}, core.ErrUnauthorized
	}
	var root string
	err := tx.QueryRowContext(ctx, `SELECT root_task_id FROM flow_tasks WHERE task_id=$1 AND caller_id=$2`, id, p.CallerID).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return flow.Task{}, flow.ErrNotFound
	}
	if err != nil {
		return flow.Task{}, err
	}
	if err := lockFlowRoot(ctx, tx, root); err != nil {
		return flow.Task{}, err
	}
	return readFlowTask(ctx, tx, id)
}

func (s *FlowStore) Create(ctx context.Context, req flow.CreateTask) (flow.Task, error) {
	var result flow.Task
	p := req.Request.Principal
	if !p.Valid() {
		return result, core.ErrUnauthorized
	}
	if req.TaskID == "" || req.Request.IdempotencyKey == "" || len(req.Request.IdempotencyKey) > 256 || req.RequestHash == "" || req.Definition.DefinitionUID == "" || req.Config.MaxOpenTasks < 1 || req.Config.MaxOpenTasks > 32 || req.Config.MaxStoredTasks < 1 || req.Config.MaxStoredTasks > 10000 {
		return result, flow.ErrDefinitionInvalid
	}
	err := s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_tasks WHERE caller_id=$1 AND idempotency_key=$2 AND request_hash=$3)`, p.CallerID, req.Request.IdempotencyKey, req.RequestHash).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		// Acceptance capacity precedes the root lock everywhere; no reverse order.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryKey("basalt/flow/acceptance")); err != nil {
			return err
		}
		previous, err := scanFlowTask(ctx, tx, tx.QueryRowContext(ctx, `SELECT `+flowTaskColumns+flowTaskJoin+` WHERE t.caller_id=$1 AND t.idempotency_key=$2`, p.CallerID, req.Request.IdempotencyKey))
		if err == nil {
			if previous.RequestHash != req.RequestHash || req.Request.DefinitionUID != previous.DefinitionUID || req.Request.DefinitionVersion > 0 && req.Request.DefinitionVersion != previous.DefinitionVersion {
				return flow.ErrIdempotencyConflict
			}
			result = previous
			return nil
		}
		if !errors.Is(err, flow.ErrNotFound) {
			return err
		}
		root := req.RootTaskID
		if root == "" {
			root = req.TaskID
		}
		if err := lockFlowRoot(ctx, tx, root); err != nil {
			return err
		}
		if req.ParentTaskID == "" {
			var version int64
			var retired bool
			err := tx.QueryRowContext(ctx, `SELECT current_version,retired FROM flow_pointers WHERE definition_uid=$1 FOR SHARE`, req.Definition.DefinitionUID).Scan(&version, &retired)
			if errors.Is(err, sql.ErrNoRows) {
				return flow.ErrDefinitionNotFound
			}
			if err != nil {
				return err
			}
			if retired {
				return flow.ErrDefinitionRetired
			}
			if version != req.Definition.Version {
				return flow.ErrDefinitionNotCurrent
			}
			if root != req.TaskID || req.Depth != 0 {
				return flow.ErrDefinitionInvalid
			}
		} else {
			parent, err := readFlowTask(ctx, tx, req.ParentTaskID)
			if err != nil {
				return err
			}
			if parent.CallerID != p.CallerID || parent.RootTaskID != root || parent.Status.Terminal() || parent.CancelRequested || parent.CloseIntent != flow.CloseNone || req.Depth != parent.Depth+1 || req.Depth > parent.Contract.Budget.MaxDepth || req.Deadline > parent.Deadline {
				return flow.ErrStateConflict
			}
			if err := ensureFlowAncestorsOpen(ctx, tx, parent, req.CreatedAt); err != nil {
				return err
			}
			rootTask, err := readFlowTask(ctx, tx, root)
			if err != nil {
				return err
			}
			if rootTask.ChildrenCreated >= parent.Contract.Budget.MaxChildren || rootTask.ChildrenCreated >= rootTask.Contract.Budget.MaxChildren || rootTask.ChildrenCreated >= 16 || req.Depth > rootTask.Contract.Budget.MaxDepth {
				return flow.ErrBudget
			}
			if _, err := tx.ExecContext(ctx, `UPDATE flow_tasks SET children_created=children_created+1,state_version=state_version+1,updated_at=$2 WHERE task_id=$1`, root, req.CreatedAt); err != nil {
				return err
			}
			if err := appendFlowTransition(ctx, tx, rootTask, req.TaskID, "child-created", req.RequestHash, rootTask.Status, rootTask.Version.Revision, req.CreatedAt, "bounded child accepted", p); err != nil {
				return err
			}
		}
		var stored, open int
		if err := tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE status IN ('queued','running','blocked','cancelling')) FROM flow_tasks`).Scan(&stored, &open); err != nil {
			return err
		}
		if stored >= req.Config.MaxStoredTasks || open >= req.Config.MaxOpenTasks {
			return flow.ErrCapacity
		}
		definitionHash, err := flow.DefinitionHash(req.Definition.Definition)
		if err != nil || definitionHash != req.Definition.ContentHash {
			return flow.ErrDefinitionInvalid
		}
		input, err := flow.CanonicalJSON(ctx, req.Input)
		if err != nil || flowDataHash(input) != req.InputHash || req.Deadline <= req.CreatedAt {
			return flow.ErrDefinitionInvalid
		}
		if req.Contract.Budget.MaxDurationSeconds < 1 || req.Contract.Budget.MaxDurationSeconds > 1800 || req.Deadline-req.CreatedAt > req.Contract.Budget.MaxDurationSeconds*1000 {
			return flow.ErrBudget
		}
		contract, err := flow.CanonicalJSON(ctx, mustFlowJSON(req.Contract))
		if err != nil {
			return err
		}
		revision := flow.TaskRevision{FormatVersion: 1, TaskID: req.TaskID, Revision: 0, NodeID: req.Definition.Definition.Entry, Input: input, InputHash: req.InputHash, Outputs: map[string]flow.NodeOutput{}, Slots: map[string]flow.Slot{}, Attempts: map[string]int{}, Reworks: map[string]int{}}
		for _, slot := range req.Contract.RequiredSlots {
			revision.Slots[slot.ID] = flow.Slot{ID: slot.ID, State: "missing"}
		}
		revision.Summary = flow.TaskSummary{Goal: req.Contract.Goal, HardConstraints: append([]string(nil), req.Contract.Constraints...), TaskRevision: 0}
		authorizingKind := core.AuthorizingKindCallerKey
		if p.Kind == core.PrincipalLocalAdmin {
			authorizingKind = core.AuthorizingKindAdminBuiltin
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO flow_tasks(task_id,root_task_id,parent_task_id,caller_id,authorizing_kind,authorizing_key_id,definition_uid,definition_version,contract_version,status,state_version,execution_epoch,current_revision,cancel_requested,close_intent,deadline,created_at,updated_at,next_eligible_at,idempotency_key,request_hash,depth) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'queued',1,1,0,false,'none',$10,$11,$11,$11,$12,$13,$14)`, req.TaskID, root, nullIfEmpty(req.ParentTaskID), p.CallerID, string(authorizingKind), nullIfEmpty(p.KeyID), req.Definition.DefinitionUID, req.Definition.Version, req.Contract.ContractVersion, req.Deadline, req.CreatedAt, req.Request.IdempotencyKey, req.RequestHash, req.Depth)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_task_contracts(task_id,contract_version,definition_uid,definition_version,content,content_hash) VALUES($1,$2,$3,$4,$5,$6)`, req.TaskID, req.Contract.ContractVersion, req.Definition.DefinitionUID, req.Definition.Version, string(contract), flowDataHash(contract)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO flow_task_revisions(task_id,revision,contract_version,checkpoint,created_at) VALUES($1,0,$2,$3,$4)`, req.TaskID, req.Contract.ContractVersion, mustFlowJSON(revision), req.CreatedAt); err != nil {
			return err
		}
		result, err = readFlowTask(ctx, tx, req.TaskID)
		return err
	})
	return result, err
}

func flowDataHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// flowCheckpointScanError 把检查点归一化的失败分成两类：ctx 已结束（或错误本身
// 就是取消哨兵）时**原样返回取消**，其余才折叠为 ErrCheckpointUnsupported。
// 取消必须可辨认：`flow.Worker.Run` 用 workerQueryError → CancellationOnly 判定
// 停机期间的纯取消，折成"检查点损坏"会让一次正常停机返回致命错误。
// flowCheckpointScanError keeps a cancelled scan distinct from a corrupted
// checkpoint so worker shutdown is not reported as data corruption.
func flowCheckpointScanError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return flow.ErrCheckpointUnsupported
}

// mustFlowJSON handles only our bounded structs; all arbitrary input is checked
// before this helper. A marshal error produces invalid JSON and fails SQL closed.
func mustFlowJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func checkFlowOwnedVersion(task flow.Task, p core.Principal, expected flow.TaskVersion) error {
	if !p.Valid() {
		return core.ErrUnauthorized
	}
	if task.CallerID != p.CallerID {
		return flow.ErrNotFound
	}
	if task.Version != expected {
		return core.ErrPreconditionFailed
	}
	return nil
}

func (s *FlowStore) Claim(ctx context.Context, req flow.ClaimNode) (flow.NodeAttempt, error) {
	var result flow.NodeAttempt
	a := req.Attempt
	if a.ID == "" || a.NodeInvocationID == "" || a.TaskID != req.TaskID || a.InputHash != flowDataHash(a.Input) {
		return result, flow.ErrStateConflict
	}
	err := s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		var found bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_node_attempts WHERE task_id=$1 AND node_attempt_id=$2 AND input_hash=$3)`, req.TaskID, a.ID, a.InputHash).Scan(&found)
		return found, err
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowOwnedTask(ctx, tx, req.TaskID, req.Principal)
		if err != nil {
			return err
		}
		if err := checkFlowOwnedVersion(task, req.Principal, req.Expected); err != nil {
			return err
		}
		if task.Status != flow.Queued && task.Status != flow.Running || task.CloseIntent != flow.CloseNone || task.CancelRequested {
			return flow.ErrStateConflict
		}
		var active bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_node_attempts WHERE task_id=$1 AND state IN ('prepared','running'))`, task.ID).Scan(&active); err != nil {
			return err
		}
		if active {
			return flow.ErrStateConflict
		}
		if req.Now >= task.Deadline {
			return flow.ErrDeadline
		}
		if err := ensureFlowAncestorsOpen(ctx, tx, task, req.Now); err != nil {
			return err
		}
		if task.Revision.NodeID != a.NodeID || a.TaskRevision != task.Version.Revision || a.ExecutionEpoch != task.Version.ExecutionEpoch || a.Attempt != task.Revision.Attempts[a.NodeID]+1 || a.Attempt > task.Contract.Budget.MaxAttemptsPerNode || task.Revision.ExpandedSteps >= 256 {
			return flow.ErrBudget
		}
		if task.ID != task.RootTaskID {
			root, err := readFlowTask(ctx, tx, task.RootTaskID)
			if err != nil {
				return err
			}
			if root.Revision.ExpandedSteps >= 256 {
				return flow.ErrBudget
			}
			root.Revision.ExpandedSteps++
			if _, err := tx.ExecContext(ctx, `UPDATE flow_task_revisions SET checkpoint=$3 WHERE task_id=$1 AND revision=$2`, root.ID, root.Version.Revision, mustFlowJSON(root.Revision)); err != nil {
				return err
			}
		}
		a.State = "running"
		a.StartedAt = req.Now
		_, err = tx.ExecContext(ctx, `INSERT INTO flow_node_attempts(task_id,node_attempt_id,node_id,node_invocation_id,attempt,task_revision,execution_epoch,run_id,session_id,state,input,input_hash,started_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'running',$10,$11,$12)`, a.TaskID, a.ID, a.NodeID, a.NodeInvocationID, a.Attempt, a.TaskRevision, a.ExecutionEpoch, nullIfEmpty(a.RunID), nullIfEmpty(a.SessionID), string(a.Input), a.InputHash, req.Now)
		if err != nil {
			return err
		}
		task.Revision.Attempts[a.NodeID] = a.Attempt
		task.Revision.ExpandedSteps++
		if _, err := tx.ExecContext(ctx, `UPDATE flow_task_revisions SET checkpoint=$3 WHERE task_id=$1 AND revision=$2`, task.ID, task.Version.Revision, mustFlowJSON(task.Revision)); err != nil {
			return err
		}
		if err := updateFlowHead(ctx, tx, task, flow.Running, task.Version.Revision, task.Version.ExecutionEpoch, flow.CloseNone, false, req.Now, ""); err != nil {
			return err
		}
		if err := appendFlowTransition(ctx, tx, task, a.ID, "claim", a.InputHash, flow.Running, task.Version.Revision, req.Now, "", req.Principal); err != nil {
			return err
		}
		result = a
		return nil
	})
	return result, err
}

// flowWriteAdvances 判定一次任务头写入属于「推进类」还是「关闭 / 停住类」
// （US-24 §2.3.6 / P-04）：写后状态 ∈ queued / running / succeeded / failed，
// 且无关闭意图、未置取消位，才算推进。关闭类（blocked / cancelling / cancelled /
// abandoned / stopped，或任何关闭意图、取消位）只保留既有 CAS——关闭正是失权
// 执行者也要能完成的收尾，不能被租约反过来挡住。
// flowWriteAdvances classifies head writes: only advance-class writes are fenced by
// a live flow_root lease; close-class writes keep the plain CAS.
func flowWriteAdvances(status flow.TaskStatus, intent flow.CloseIntent, cancel bool) bool {
	if cancel || intent != flow.CloseNone {
		return false
	}
	switch status {
	case flow.Queued, flow.Running, flow.Succeeded, flow.Failed:
		return true
	default:
		return false
	}
}

func updateFlowHead(ctx context.Context, tx *sql.Tx, old flow.Task, status flow.TaskStatus, revision, epoch int64, intent flow.CloseIntent, cancel bool, now int64, reason string) error {
	if old.Status.Terminal() || status != old.Status && !flow.CanTransition(old.Status, status) || !flow.ValidCloseIntent(status, intent, cancel) || old.CancelRequested && !cancel {
		return flow.ErrStateConflict
	}
	query := `UPDATE flow_tasks SET status=$2,state_version=state_version+1,current_revision=$3,execution_epoch=$4,close_intent=$5,cancel_requested=$6,updated_at=$7,next_eligible_at=$7,reason=$8 WHERE task_id=$1 AND current_revision=$9 AND state_version=$10 AND execution_epoch=$11`
	if flowWriteAdvances(status, intent, cancel) {
		// 推进类写入必须由当前持有根租约的执行者发起（US-24 §3.1.6 / FR-24-18）：
		// 子任务行自己没有 flow_root 键，按**目标行自己的 root_task_id** 查父根；
		// 过期比较用调用方传入的 now（$7），与 updated_at 同一读数。
		// Advance-class writes require a live lease on the target row's own root.
		query += ` AND EXISTS (SELECT 1 FROM execution_leases l WHERE l.kind='flow_root' AND l.resource_id=flow_tasks.root_task_id AND l.released_at IS NULL AND l.expires_at>$7)`
	}
	changed, err := tx.ExecContext(ctx, query, old.ID, status, revision, epoch, intent, cancel, now, reason, old.Version.Revision, old.Version.StateVersion, old.Version.ExecutionEpoch)
	if err != nil {
		return err
	}
	n, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return core.ErrPreconditionFailed
	}
	return nil
}

// createRunForNode 在 Run 行的**同一事务**内校验「子 Run 创建」的父根条件
// （US-24 §2.3.9 / P-05 / FR-24-37）：
//
//	① 父根（目标任务行的 root_task_id）上存在未释放且未过期的 flow_root 租约，
//	   否则 ErrLeaseLost——没有执行权的 Worker 不得凭空创建子 Run；
//	② attempt 归属的父根必须就是本次固定的 root（root_task_id 归属），
//	   否则 ErrFlowAttemptStale——持根 A 的租约不得为根 B 下的 attempt 建 Run；
//	③ attempt 未被取代：state ∈ prepared / running，execution_epoch 与
//	   task_revision 都等于任务当前值，任务仍 running、未关闭 / 未取消、未过
//	   deadline，否则 ErrFlowAttemptStale——旧代次 / 旧 revision 的 attempt 若
//	   放行，Run 会白跑一轮后才在写回时被拒。
//
// 本函数只做**前置判定**并给出可区分的领域错误；真正的写入仍由 insertRunRow 的
// 唯一 INSERT（带 EXISTS 兜底）完成，两段之间即使发生并发变化也只会落到
// core.ErrPreconditionFailed，不会出现第二条写入路径。父根判定以
// `FlowRunContext.Lease`（rootToken）为**唯一**归属证据：租约行先 `FOR UPDATE`
// 再加 owner + ownership_epoch 条件，INSERT 的 EXISTS 重复同一谓词。
// createRunForNode is the pre-check for child Run creation; the single INSERT remains
// the only write path and re-checks the same owner/epoch predicate via EXISTS.
func createRunForNode(ctx context.Context, ex sqlExecutor, f core.FlowRunContext, now int64) error {
	if f.TaskID == "" || f.NodeAttemptID == "" || f.RootTaskID == "" {
		return flow.ErrFlowAttemptStale
	}
	// ⓪ 凭证形状：没有根租约凭证就没有父根归属可证。声明根与凭证资源必须同一：
	// 只填一半的 token 是装配缺陷，按失权处理（fail-closed），不得退化成
	// "库里有活租约就放行"——那样旧持有者恰能在接管后凭残留租约建子 Run。
	// The credential must be shaped like a flow-root token for this root.
	if f.Lease.Kind != core.LeaseFlowRoot || f.Lease.OwnerWorkerID == "" || f.Lease.ResourceID != f.RootTaskID {
		return core.ErrLeaseLost
	}
	// ① 租约行锁：先锁行，再校验 owner + ownership_epoch + 未过期。EXISTS 快照会
	// 在"校验通过 → 另一 Worker 接管并递增代次 → 本事务继续"的窗口里放行旧持有者；
	// 行锁把接管 UPDATE 与本判定串行化，旧代次只能看到 ErrLeaseLost（§2.3.6）。
	// Lock the flow-root lease row first so a takeover either waits for this
	// transaction or is observed here as a stale owner/epoch.
	var epoch int64
	err := ex.QueryRowContext(ctx, `SELECT ownership_epoch FROM execution_leases
	  WHERE kind='flow_root' AND resource_id=$1 AND owner_worker_id=$2
	    AND released_at IS NULL AND expires_at>$3
	  FOR UPDATE`, f.Lease.ResourceID, f.Lease.OwnerWorkerID, now).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if epoch != f.Lease.OwnershipEpoch {
		return core.ErrLeaseLost
	}
	// ② 归属 / 代次 / revision：持根 A 的租约不得为根 B 下的 attempt 建 Run，
	// 旧 revision 的 attempt 也不得建（Run 白跑一轮后才在写回被拒）。
	var owned, fresh bool
	err = ex.QueryRowContext(ctx, `SELECT
		t.root_task_id=$3,
		a.state IN ('prepared','running') AND a.execution_epoch=t.execution_epoch
		  AND a.task_revision=t.current_revision AND t.current_revision=$5
		  AND t.status='running' AND t.close_intent='none' AND NOT t.cancel_requested
		  AND t.deadline>$4
	  FROM flow_tasks t JOIN flow_node_attempts a ON a.task_id=t.task_id
	 WHERE t.task_id=$1 AND a.node_attempt_id=$2`, f.TaskID, f.NodeAttemptID, f.RootTaskID, now, f.TaskRevision).Scan(&owned, &fresh)
	if errors.Is(err, sql.ErrNoRows) {
		return flow.ErrFlowAttemptStale
	}
	if err != nil {
		return err
	}
	if !owned || !fresh {
		return flow.ErrFlowAttemptStale
	}
	return nil
}

func appendFlowTransition(ctx context.Context, tx *sql.Tx, old flow.Task, op, phase, hash string, status flow.TaskStatus, revision, now int64, reason string, p core.Principal) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO flow_transitions(task_id,seq,operation_id,phase,request_hash,old_state_version,new_state_version,old_revision,new_revision,reason,actor_kind,authorizing_key_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$2,$7,$8,$9,$10,$11,$12)`, old.ID, old.Version.StateVersion+1, op, phase, hash, old.Version.StateVersion, old.Version.Revision, revision, reason, string(p.Actor), nullIfEmpty(p.KeyID), now)
	return err
}

func (s *FlowStore) ReadyRoots(ctx context.Context, now int64, limit int) ([]flow.Task, error) {
	return s.flowRoots(ctx, true, now, limit)
}

func (s *FlowStore) OpenRoots(ctx context.Context, limit int) ([]flow.Task, error) {
	return s.flowRoots(ctx, false, int64(0), limit)
}

func (s *FlowStore) flowRoots(ctx context.Context, readyOnly bool, now int64, limit int) ([]flow.Task, error) {
	return readFlowRoots(ctx, s.db.pool, readyOnly, now, limit)
}

func readFlowRoots(ctx context.Context, q Queryer, readyOnly bool, now int64, limit int) ([]flow.Task, error) {
	if limit < 1 || limit > 32 {
		return nil, flow.ErrCapacity
	}
	// 就绪扫描必须排除"根上已有未释放且未过期 flow_root 租约"的行（US-24 §3.1.6 / R7
	// 修正 17）：`released_at IS NULL` 与 `expires_at > now` **两个条件都要**——Release
	// 会写 released_at 但保留未来的 expires_at，漏掉前半句会让刚交还的根被排除一整个 TTL，
	// 把租约误当成"领取冷却时间"。OpenRoots（关闭扫描）不加这条：关闭类写入不要求
	// 根租约，持有根租约的执行者也不能阻止关闭。
	// Ready roots exclude rows whose flow_root lease is live (not released, not expired).
	rows, err := q.QueryContext(ctx, `SELECT `+flowTaskColumns+flowTaskJoin+` WHERE t.parent_task_id IS NULL AND
	 (($3 AND (t.status IN ('queued','running') OR t.status='blocked' AND t.reason='bounded_rework_required') AND NOT t.cancel_requested AND t.close_intent='none' AND t.next_eligible_at<=$1
	      AND NOT EXISTS (SELECT 1 FROM execution_leases l WHERE l.kind='flow_root' AND l.resource_id=t.task_id AND l.released_at IS NULL AND l.expires_at>$1))
	 OR (NOT $3 AND t.status IN ('queued','running','blocked','cancelling') AND $1>=0))
	 ORDER BY t.next_eligible_at,t.created_at,t.task_id LIMIT $2`, now, limit, readyOnly)
	if err != nil {
		return nil, err
	}
	var result []flow.Task
	for rows.Next() {
		task, err := scanFlowTaskData(ctx, rows)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		result = append(result, task)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	// 先关闭批量游标，再读取不可变定义；单连接池也不会等待自身持有的连接。
	// Close the batch cursor before loading definitions, including with a one-connection pool.
	versions := make(map[flow.DefinitionRef]flow.DefinitionVersion)
	for i := range result {
		task := &result[i]
		ref := flow.DefinitionRef{UID: task.DefinitionUID, Version: task.DefinitionVersion}
		version, found := versions[ref]
		if !found {
			version, err = readAcceptedFlowDefinition(ctx, q, *task)
			if err != nil {
				return nil, err
			}
			versions[ref] = version
		}
		task.AcceptedVersion = version
	}
	return result, nil
}

func (s *FlowStore) HasUnknownActions(ctx context.Context, id string) (bool, error) {
	var root string
	err := s.db.pool.QueryRowContext(ctx, `SELECT root_task_id FROM flow_tasks WHERE task_id=$1`, id).Scan(&root)
	if errors.Is(err, sql.ErrNoRows) {
		return false, flow.ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return unknownFlowActions(ctx, s.db, root)
}

func (s *FlowStore) Defer(ctx context.Context, id string, expected flow.TaskVersion, eligible int64) error {
	op := "defer:" + strconv.FormatInt(expected.StateVersion, 10)
	hash := flowOperationHash("defer", struct {
		TaskID   string
		Expected flow.TaskVersion
		Eligible int64
	}{id, expected, eligible})
	return s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return findFlowOperation(ctx, q, id, op, "defer", hash)
	}, func(ctx context.Context, tx *sql.Tx) error {
		task, err := lockedFlowTask(ctx, tx, id)
		if err != nil {
			return err
		}
		if found, err := findFlowOperation(ctx, tx, id, op, "defer", hash); found || err != nil {
			return err
		}
		if task.Version != expected || task.Status != flow.Queued && task.Status != flow.Running || task.CancelRequested || task.CloseIntent != flow.CloseNone || eligible >= task.Deadline {
			return flow.ErrStateConflict
		}
		if task.Status == flow.Running {
			allowed := false
			for _, node := range task.AcceptedVersion.Definition.Nodes {
				if node.ID == task.Revision.NodeID && node.Kind == "subflow" {
					allowed = true
				}
			}
			if !allowed {
				return flow.ErrStateConflict
			}
		}
		scopes := task.Contract.Scope.Scopes
		if task.AuthorizingKind == string(core.AuthorizingKindAdminBuiltin) {
			scopes = nil
		}
		principal, err := core.OwnedPrincipal(task.CallerID, core.AuthorizingKind(task.AuthorizingKind), task.AuthorizingKeyID, scopes, core.ActorRecovery)
		if err != nil {
			return err
		}
		now := s.db.clock().UnixMilli()
		if err := updateFlowHead(ctx, tx, task, task.Status, task.Version.Revision, task.Version.ExecutionEpoch, task.CloseIntent, task.CancelRequested, now, "capacity deferred"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE flow_tasks SET next_eligible_at=$2 WHERE task_id=$1`, id, eligible); err != nil {
			return err
		}
		return appendFlowTransition(ctx, tx, task, op, "defer", hash, task.Status, task.Version.Revision, now, "capacity deferred", principal)
	})
}

func flowOperationHash(kind string, req any) string {
	return flowDataHash(mustFlowJSON(struct {
		Kind    string
		Request any
	}{kind, req}))
}

// Every leaf permission checks persisted ancestors under the same root lock.
// Local Worker ownership alone cannot authorize a child after root closure.
func ensureFlowAncestorsOpen(ctx context.Context, q Queryer, task flow.Task, now int64) error {
	var count int
	var open, hasRoot bool
	err := q.QueryRowContext(ctx, `WITH RECURSIVE lineage AS (
 SELECT task_id,parent_task_id,status,cancel_requested,close_intent,deadline,0 AS level FROM flow_tasks WHERE task_id=$1
 UNION ALL SELECT p.task_id,p.parent_task_id,p.status,p.cancel_requested,p.close_intent,p.deadline,l.level+1 FROM flow_tasks p JOIN lineage l ON p.task_id=l.parent_task_id WHERE l.level<3
 ) SELECT count(*),COALESCE(bool_and(status NOT IN ('succeeded','failed','stopped','cancelled','abandoned') AND NOT cancel_requested AND close_intent='none' AND deadline>$3),false),COALESCE(bool_or(task_id=$2),false) FROM lineage`, task.ID, task.RootTaskID, now).Scan(&count, &open, &hasRoot)
	if err != nil {
		return err
	}
	if count != task.Depth+1 || !open || !hasRoot {
		return flow.ErrStateConflict
	}
	return nil
}

func findFlowOperation(ctx context.Context, q Queryer, taskID, op, phase, hash string) (bool, error) {
	var stored string
	err := q.QueryRowContext(ctx, `SELECT request_hash FROM flow_transitions WHERE task_id=$1 AND operation_id=$2 AND phase=$3`, taskID, op, phase).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stored != hash {
		return false, flow.ErrIdempotencyConflict
	}
	return true, nil
}
