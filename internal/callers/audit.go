package callers

// 本文件是 `internal/callers` 的**唯一**审计模块（US-16 / FR-039 / 宪法 V）：身份
// （`caller.create`）、Key 变更（`caller.key.issue` / `caller.key.revoke` /
// `caller.key.rotate`）与配额处置（`caller.reservation.reconcile`，含
// `detail.scope=overage` 的清零）三类管理动作共用同一条 `pending` 预写 / 终态回填
// 路径。规则只有三条：
//
//  1. 变更**之前**写 `pending`；预写失败即拒绝执行（fail-closed）。
//  2. 终态回填用 `context.WithoutCancel`——客户端断开不能让审计停在 `pending`。
//  3. 回填失败只记 ERROR 并留待启动恢复查证，**不**把它当成业务失败返回：变更
//     可能已经生效，把它报成失败会诱使调用方重试并产生第二次副作用。
//
// 配额**拒绝**刻意不在这条路径上：401 / 403 / 409 / 429 是纯准入判定
// （`docs/contracts/run-and-errors.md` §8 / §9.3），既不写管理操作行，也不留预留。
// 配额侧"先写 `pending` 再执行"的载体是 `quota_reservations` 行本身——`AdmitCall`
// → `QuotaStore.Reserve` 在调用前落 `pending`，再由 `Commit` / `Release` /
// `Reconcile` 收口（C-05 / C-29）；预写失败后的配额释放由消费方以
// `ReleaseAuditFailed` 完成，本包只提供理由字面量。
//
// This file is the single audit module of the callers package: identity, key
// lifecycle and quota reconciliation share one pending-first path. Quota
// rejections are pure admission decisions and never write an operation row; their
// pending record is the reservation row itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// operationSubject 区分两类管理动作的日志文案主语（审计内容与纪律完全相同）。
// operationSubject labels log lines; it never changes the audit discipline itself.
type operationSubject string

const (
	// subjectKeyOperation 是 Key 生命周期动作的日志主语。
	subjectKeyOperation operationSubject = "调用方 Key 操作"
	// subjectCallerOperation 是调用方 / 配额管理动作的日志主语。
	subjectCallerOperation operationSubject = "调用方管理操作"
)

// operationWriter 预写与回填管理操作行。零值不可用；字段由构造方显式给出，
// **不**在这里取全局默认值——否则测试注入的时钟会被悄悄换掉。
// operationWriter writes the pending and terminal operation rows; every field is
// supplied explicitly so injected clocks never get replaced by defaults.
type operationWriter struct {
	ops    core.OperationStore
	logger *slog.Logger
	now    func() time.Time
	// subject 只影响日志主语；审计字段不受它影响。
	subject operationSubject
}

// begin 预写一条 `pending` 管理操作行并返回操作标识；预写失败即拒绝执行，
// 此时目标一个字节都没动。标识只经返回值传递，不落结构体字段。
// begin pre-writes the pending operation; a failure refuses the change.
func (w operationWriter) begin(ctx context.Context, actor core.Actor, action, targetID, requestHash string, detail any) (string, error) {
	payload, err := json.Marshal(detail)
	if err != nil {
		return "", fmt.Errorf("组装 %s 操作详情: %w", action, err)
	}
	now := w.now()
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   targetKindCaller,
		TargetID:     targetID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  requestHash,
		Status:       core.OperationPending,
		DetailJSON:   payload,
		CreatedAt:    now,
	}
	if beginErr := w.ops.BeginOperation(ctx, record); beginErr != nil {
		w.logger.Error(string(w.subject)+"预写记录失败，拒绝执行",
			"action", action, "target", targetID, "error", beginErr)
		return "", fmt.Errorf("%s 预写操作记录失败，拒绝执行: %w", action, beginErr)
	}
	return operationID, nil
}

// finish 回填终态；回填不随请求取消而放弃。
// finish writes the terminal state detached from request cancellation.
func (w operationWriter) finish(ctx context.Context, operationID string, status core.OperationStatus,
	code, message string, detail any,
) {
	payload, err := json.Marshal(detail)
	if err != nil {
		// 详情序列化失败不改业务结果，但终态回填仍必须发生，否则审计停在 pending。
		payload = nil
		w.logger.Error(string(w.subject)+"详情序列化失败，终态照常回填",
			"operation_id", operationID, "error", err)
	}
	if finishErr := w.ops.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, payload, w.now()); finishErr != nil {
		w.logger.Error(string(w.subject)+"回填终态失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(status), "error", finishErr)
	}
}
