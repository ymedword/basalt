package app

// 本文件是内网访问例外的管理面服务（14D §2.2.6 / T107）：例外是**网络**面的唯一放行
// 口子——文件由目录边界决定、命令由白名单 / 安装清单决定，不在这里开口子。权威源是
// `network_exceptions` 表，运行期有三处消费同一份集合：策略实例（域名面 host 例外与
// `ExceptionsCount`）、进程级守门器快照（拨号面 `ip_port` / 受保护地址）与页面视图。
// 三处必须由**同一次 apply** 替换，否则"页面显示已放行、拨号仍拒"这类分叉无从解释。
//
// 写入语义与白名单条件写同构：校验 → （删除才有的）集合 ETag 比较 → 预写 `pending`
// → 落库 → 原子替换 → 回填终态；预写失败即拒绝执行且不留行。删除后**下一次**判定即
// 失效：守门器每次拨号读活快照，不缓存、不需要重建客户端。
// This file owns the network-exception service. The policy instance, the process-wide
// dial guard snapshot and the page view are swapped by one apply call, so a rendered
// exception and the dial verdict can never disagree. A failed pre-write refuses the
// change outright; deletion takes effect at the very next judgement.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// NetworkExceptionDeps 是例外服务的装配参数。Policy 必须与运行期消费者共用同一
// 实例；Store / Operations 必须与启动恢复查证共用同一批实例——恢复读到的就是写
// 路径写进去的那一行。
// The policy must be the very instance every consumer uses; the store and the
// operation ledger are the same instances startup recovery reads.
type NetworkExceptionDeps struct {
	Store      core.NetworkExceptionStore
	Operations core.OperationStore
	Policy     *tool.WhitelistPolicy
	Logger     *slog.Logger
	Now        func() time.Time
}

// NetworkExceptionService 串行化「读当前集合 → 比较 ETag → 预写 → 写入 → 应用」，
// 并以原子快照对外提供 List / ETag；库内唯一键 `(kind, value)` 是第二道闸。
// The service serializes read-compare-write-apply; the unique (kind, value) key is
// the second gate. Readers are lock-free.
type NetworkExceptionService struct {
	deps    NetworkExceptionDeps
	mu      sync.Mutex
	current atomic.Pointer[[]core.NetworkException]
}

// 编译期断言：服务满足 web 依赖的管理端口。
// Compile-time assertion that the service satisfies the web-facing port.
var _ core.NetworkExceptionAdmin = (*NetworkExceptionService)(nil)

// NewNetworkExceptionService 装配服务：缺省 logger 丢弃、缺省时钟取系统时间。
// NewNetworkExceptionService wires the service with a discard logger and the clock.
func NewNetworkExceptionService(deps NetworkExceptionDeps) *NetworkExceptionService {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &NetworkExceptionService{deps: deps}
}

// EnsureLoaded 在启动时把权威行装载进策略实例、守门器快照与视图快照三处。读失败
// 时**不**凭空清空守门器已有的集合（那是 `refreshNetworkExceptions` 在工具装配前
// 装载的那份）：把当前守门器快照复制过来，让三处继续同源，并如实返回错误供调用方
// 记 WARN——宁可少放行，也不让页面与拨号看到两份集合。
// EnsureLoaded loads the authoritative rows into all three faces. A failed read keeps
// the guard's existing snapshot and mirrors it, so the faces never diverge.
func (s *NetworkExceptionService) EnsureLoaded(ctx context.Context) error {
	list, err := s.deps.Store.List(ctx)
	if err != nil {
		s.apply(processExceptions.Snapshot())
		return err
	}
	s.apply(list)
	return nil
}

// List 返回当前**生效**的例外集合副本（按 `kind` + `value` 升序，与契约同序）。
// List returns a copy of the live set in the contract's stable order.
func (s *NetworkExceptionService) List() []core.NetworkException {
	if snapshot := s.current.Load(); snapshot != nil {
		return append(make([]core.NetworkException, 0, len(*snapshot)), *snapshot...)
	}
	return []core.NetworkException{}
}

// ETag 返回整份集合的强 ETag（删除的 `If-Match` 唯一取值来源）。
// ETag renders the collection's strong ETag.
func (s *NetworkExceptionService) ETag() string { return core.NetworkExceptionETag(s.List()) }

// Create 新增一条例外：值 / 理由校验（400，零写入零操作行）→ 重复登记检查（409，
// 零写入零操作行）→ 预写 `network_exception.create` → 落库 → 应用 → 回填终态。
// 返回落库行与操作标识；预写之后的失败一律置 failed 并把操作标识带回。
// Create performs the whole write; only post-pre-write failures carry an operation id.
func (s *NetworkExceptionService) Create(ctx context.Context, kind, value, note string,
	actor core.Actor,
) (core.NetworkException, string, error) {
	normalized, err := core.ValidateNetworkExceptionValue(kind, value)
	if err != nil {
		// 纯请求校验：非法值 / 云元数据地址零写入、零操作行。
		return core.NetworkException{}, "", err
	}
	trimmedNote := strings.TrimSpace(note)
	if trimmedNote == "" {
		return core.NetworkException{}, "", &core.InvalidInputError{
			Tool:   "network_exception",
			Reason: "note 必填且不能全是空白——放行必须带理由，理由进审计",
		}
	}
	if utf8.RuneCountInString(trimmedNote) > core.NetworkExceptionNoteMaxLength {
		return core.NetworkException{}, "", &core.InvalidInputError{
			Tool: "network_exception",
			Reason: fmt.Sprintf("note 最多 %d 字（当前 %d 字）",
				core.NetworkExceptionNoteMaxLength, utf8.RuneCountInString(trimmedNote)),
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.List()
	for _, item := range current {
		if item.Kind == kind && item.Value == normalized {
			// 重复登记不覆盖既有理由：两条不同理由指向同一放行面会让审计读不清。
			return core.NetworkException{}, "", fmt.Errorf(
				"%w: 例外 %s=%s 已存在（不覆盖既有理由）", core.ErrConflict, kind, normalized)
		}
	}

	exceptionID, idErr := core.NewNetworkExceptionID()
	if idErr != nil {
		return core.NetworkException{}, "", fmt.Errorf("生成例外标识: %w", idErr)
	}
	now := s.deps.Now()
	exception := core.NetworkException{
		ExceptionID: exceptionID,
		Kind:        kind,
		Value:       normalized,
		Note:        trimmedNote,
		Revision:    1,
		CreatedAt:   now,
		CreatedBy:   actor.Subject,
	}
	detail := networkExceptionOperationDetail{
		Kind:       kind,
		Value:      normalized,
		Note:       trimmedNote,
		BeforeETag: core.NetworkExceptionETag(current),
	}
	detailJSON, marshalErr := json.Marshal(detail)
	if marshalErr != nil {
		return core.NetworkException{}, "", fmt.Errorf(
			"assemble network exception operation detail: %w", marshalErr)
	}

	operationID := core.NewOperationID(now)
	exception.OperationID = operationID
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionNetworkExceptionCreate,
		TargetKind:   core.TargetKindNetworkException,
		TargetID:     exceptionID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  agentRequestHash(kind + "\n" + normalized + "\n" + trimmedNote),
		DetailJSON:   detailJSON,
		Status:       core.OperationPending,
		CreatedAt:    now,
	}
	if beginErr := s.deps.Operations.BeginOperation(ctx, record); beginErr != nil {
		// 预写失败 = 拒绝执行：表里一个字节都没动（宪法 V）。
		s.deps.Logger.Error("refusing network exception create: pre-write of operation record failed",
			"error", beginErr)
		return core.NetworkException{}, "", fmt.Errorf(
			"pre-write operation record failed; refusing to create network exception: %w",
			beginErr)
	}

	if insertErr := s.deps.Store.Create(ctx, exception); insertErr != nil {
		s.finish(ctx, operationID, core.OperationFailed, exceptionResultCode(insertErr),
			insertErr, detailJSON)
		if errors.Is(insertErr, core.ErrPolicyRevisionConflict) {
			// 另一进程抢先写入同一 `(kind, value)`：重读使快照与库一致。
			s.reload(ctx)
		}
		return core.NetworkException{}, operationID, insertErr
	}

	applied := append(append([]core.NetworkException(nil), current...), exception)
	s.apply(applied)
	s.finish(ctx, operationID, core.OperationSuccess, "OK", nil, detailJSON)
	return exception, operationID, nil
}

// Delete 删除一条例外：集合 ETag 比较（412，零写入零操作行）→ 行存在性（404）→
// 预写 `network_exception.delete` → 删除 → 应用 → 回填终态。删除后下一次判定即失效。
// Delete removes one exception; the If-Match fences the whole collection the admin saw.
func (s *NetworkExceptionService) Delete(ctx context.Context, exceptionID, ifMatch string,
	actor core.Actor,
) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.List()
	if ifMatch != core.NetworkExceptionETag(current) {
		return "", fmt.Errorf(
			"%w: network exception If-Match does not match the current collection",
			core.ErrPreconditionFailed)
	}
	target, found, lookupErr := s.deps.Store.Lookup(ctx, exceptionID)
	if lookupErr != nil {
		return "", lookupErr
	}
	if !found {
		// 例外不存在：404，零写入零操作行（集合里没有它，也就没有"要删的东西"）。
		return "", fmt.Errorf("%w: 网络访问例外 %s", core.ErrResourceNotFound, exceptionID)
	}

	now := s.deps.Now()
	detail := networkExceptionOperationDetail{
		Kind:       target.Kind,
		Value:      target.Value,
		Note:       target.Note,
		BeforeETag: core.NetworkExceptionETag(current),
	}
	detailJSON, marshalErr := json.Marshal(detail)
	if marshalErr != nil {
		return "", fmt.Errorf("assemble network exception operation detail: %w", marshalErr)
	}
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionNetworkExceptionDelete,
		TargetKind:   core.TargetKindNetworkException,
		TargetID:     exceptionID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  agentRequestHash(exceptionID),
		ExpectedETag: ifMatch,
		DetailJSON:   detailJSON,
		Status:       core.OperationPending,
		CreatedAt:    now,
	}
	if beginErr := s.deps.Operations.BeginOperation(ctx, record); beginErr != nil {
		s.deps.Logger.Error("refusing network exception delete: pre-write of operation record failed",
			"exception_id", exceptionID, "error", beginErr)
		return "", fmt.Errorf(
			"pre-write operation record failed; refusing to delete network exception: %w",
			beginErr)
	}

	if deleteErr := s.deps.Store.Delete(ctx, exceptionID); deleteErr != nil {
		s.finish(ctx, operationID, core.OperationFailed, exceptionResultCode(deleteErr),
			deleteErr, detailJSON)
		if errors.Is(deleteErr, core.ErrResourceNotFound) {
			s.reload(ctx)
		}
		return operationID, deleteErr
	}

	remaining := make([]core.NetworkException, 0, len(current))
	for _, item := range current {
		if item.ExceptionID == exceptionID {
			continue
		}
		remaining = append(remaining, item)
	}
	s.apply(remaining)
	s.finish(ctx, operationID, core.OperationSuccess, "OK", nil, detailJSON)
	return operationID, nil
}

// apply 把同一份集合同步到策略实例、守门器快照与视图快照：三处必须来自同一次写入，
// 否则"保存即生效"只改了副本（删除后下一次拨号 / 下一次域名判定读的仍是旧集合）。
// apply swaps all three faces from one list so no consumer can see a stale copy.
func (s *NetworkExceptionService) apply(list []core.NetworkException) {
	sorted := append(make([]core.NetworkException, 0, len(list)), list...)
	// 契约的稳定顺序（`kind` 升序、`value` 升序）：页面列表与集合 ETag 因此不依赖
	// 插入顺序。
	// The contract's stable order keeps the list and the collection ETag independent of
	// insertion order.
	slices.SortFunc(sorted, func(a, b core.NetworkException) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(a.Value, b.Value)
	})
	s.deps.Policy.SetNetworkExceptions(sorted)
	processExceptions.Replace(sorted)
	snapshot := sorted
	s.current.Store(&snapshot)
}

// reload 从库里重读并应用：仅在并发冲突 / 行已被删除的失败路径上调用，让视图与
// 判定随后看到数据库真值。重读失败只记 WARN，不臆造当前集合。
// reload converges the live faces after a conflict; a failed reload is only logged.
func (s *NetworkExceptionService) reload(ctx context.Context) {
	list, err := s.deps.Store.List(ctx)
	if err != nil {
		s.deps.Logger.Warn("failed to reload network exceptions after conflict", "error", err)
		return
	}
	s.apply(list)
}

// finish 回填终态；回填不随请求取消而放弃，失败只记 ERROR（留待启动恢复查证）。
// finish writes the terminal state detached from cancellation.
func (s *NetworkExceptionService) finish(ctx context.Context, operationID string,
	status core.OperationStatus, code string, cause error, detail []byte,
) {
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if err := s.deps.Operations.FinishOperation(context.WithoutCancel(ctx), operationID,
		status, code, message, detail, s.deps.Now()); err != nil {
		s.deps.Logger.Error("failed to record network exception operation outcome; left for startup recovery",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}

// exceptionResultCode 把例外写入失败映射为审计码：唯一键 / 修订号冲突是 CONFLICT，
// 行已不存在是 NOT_FOUND，其余按内部错误。
// exceptionResultCode maps a failure onto the shared result-code table.
func exceptionResultCode(err error) string {
	switch {
	case errors.Is(err, core.ErrPolicyRevisionConflict), errors.Is(err, core.ErrConflict):
		return "CONFLICT"
	case errors.Is(err, core.ErrResourceNotFound):
		return "NOT_FOUND"
	default:
		return "INTERNAL"
	}
}

// networkExceptionOperationDetail 是 `detail_json` 的固定投影：`kind` / `value` /
// `note` 是审计内容（放行理由必填），`before_etag` 帮助恢复区分"删除生效"与"集合
// 被他人改过"。**不**记请求体其余字段。
// The detail carries the audit material only: kind, value, note and the collection
// ETag seen before the write.
type networkExceptionOperationDetail struct {
	Kind       string `json:"kind"`
	Value      string `json:"value"`
	Note       string `json:"note"`
	BeforeETag string `json:"before_etag"`
}
