package callers

// 本文件实现调用方 Key 的生命周期（US-16 §3.1.5 / C-07）：签发 / 轮换 / 吊销。
// 三条规则贯穿全文件：
//
//  1. **先校验、后留痕、再执行**——scope 非法在写任何行之前拒绝（`ErrInvalidScope`，
//     不留 `pending` 操作）；查找失败（`ErrNotFound`）同样不留操作行；预写失败
//     **不得**执行变更（宪法 V）。
//  2. **明文只出现一次**——服务端只存 SHA-256 指纹与展示前缀；`IssuedKey.Secret`
//     之外的任何位置（记录、审计 `detail_json`、错误文案）都不得出现明文。
//  3. **不隐式改绑**——轮换时是否把定时任务再绑定到新 Key 由调用方显式声明；
//     未能再绑定的任务逐条进操作结果与审计，吊销则**不**触碰任何任务绑定。
//
// This file implements the caller key lifecycle. Validation precedes any durable
// write; the plaintext exists only in IssuedKey; and task rebinding happens only when
// explicitly requested, with every task left bound reported back.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// 包级错误哨兵：web 层按 `errors.Is` 映射 401 / 403 / 404 / 409 / 429。
// `ErrInvalidScope` 只在 Key 发放 / 轮换时返回；`ErrUnauthorized` 见 authenticate.go。
// Package-level sentinels; the web layer maps them with errors.Is.
var (
	// ErrForbidden 表示已认证但无权执行：轮换请求的归属调用方与目标 Key 不一致。
	// ErrForbidden marks an authenticated but disallowed action.
	ErrForbidden = errors.New("callers: forbidden")

	// ErrNotFound 表示按 `(caller_id, key_id)` 找不到 Key（含归属不匹配与调用方停用）。
	// 调用方**不得**据错误区分这几种情形。
	// ErrNotFound marks a key that (caller_id, key_id) does not resolve to.
	ErrNotFound = errors.New("callers: not found")

	// ErrInvalidScope 表示发放 / 轮换时给出的 scope 非法：空集合、未知字面量、
	// 通配 `agents:invoke:*` 或管理面 scope（`diagnostics:read` / `callers:admin`）。
	// ErrInvalidScope marks an empty, unknown, wildcard or admin-only scope.
	ErrInvalidScope = errors.New("callers: invalid scope")
)

// 管理操作字面量（`docs/contracts/run-and-errors.md` §9.3 逐字；本包产生的集合）。
// 定义在消费方而不是 core：这三条只由 KeyIssuer 写，core 不需要认识它们。
// Management-action literals copied verbatim from the contract, limited to the ones
// this package writes.
const (
	actionCallerKeyIssue  = "caller.key.issue"
	actionCallerKeyRevoke = "caller.key.revoke"
	actionCallerKeyRotate = "caller.key.rotate"
	targetKindCaller      = "caller"
)

// 操作 `result_code`：成功固定 `OK`，失败按分类哨兵投影（与 app 层同一口径）。
// Result codes: success is OK, failures are projected from the sentinels.
const (
	resultOK           = "OK"
	resultForbidden    = "FORBIDDEN"
	resultNotFound     = "NOT_FOUND"
	resultInvalidScope = "INVALID_SCOPE"
	resultInternal     = "INTERNAL"
)

// Key 明文的格式常量：`bk_` + 40 位小写十六进制（20 字节 = 160 位随机）。
// 不用 Crockford 表是为了不复制 `core` 的身份字符集；明文只在签发响应出现一次，
// 由机器复制粘贴，不需要人读友好。展示前缀取前 8 个字符（`bk_` + 5 位）。
// The plaintext shape is bk_ plus 40 lowercase hex characters (160 bits) and the
// display prefix is its first eight characters.
const (
	keySecretPrefix = "bk_"
	keySecretBytes  = 20
	keyPrefixLength = 8
)

// BoundTask 是一条**未能**再绑定到新 Key 的定时任务：任务标识 + 稳定原因码
// （`BoundTaskScopeMissing` / `BoundTaskCallerMismatch`）。它进 `RotateResult` 与
// `caller.key.rotate` 审计，供人判断该任务接下来是补 scope 还是换归属。
// BoundTask is one scheduled task that stayed bound to the old key, with a stable
// reason code for the audit trail.
type BoundTask struct {
	// TaskID 是定时任务标识。
	TaskID string
	// Reason 是原因码（`scope_missing` / `caller_mismatch`）。
	Reason string
}

// 再绑定失败的两个原因码（契约 §3.1.5 逐字）。
// The two left-bound reason codes frozen by the design.
const (
	// BoundTaskScopeMissing 表示新 Key 的 scope 不覆盖该任务需要的 `agents:invoke:<name>`。
	BoundTaskScopeMissing = "scope_missing"
	// BoundTaskCallerMismatch 表示任务的归属调用方与新 Key 不一致。
	BoundTaskCallerMismatch = "caller_mismatch"
)

// RotateOptions 是轮换的可选项：是否在同一存储事务内把定时任务再绑定到新 Key。
// **缺省 false**——任务保持旧绑定，下次触发按 `owner_unauthorized` fail-closed；
// 没有任何"顺手帮用户绑上"的默认值。
// RotateOptions carries the explicit rebind request; the default keeps tasks on the
// old key so the next trigger fails closed.
type RotateOptions struct {
	// RebindScheduledTasks 为真时，存储层在同一事务内把符合条件任务的
	// `authorizing_key_id` 改为新 Key。
	RebindScheduledTasks bool
}

// RotateResult 是一次轮换的结果：新 Key 行、已再绑定的任务标识，以及未能再绑定的
// 任务清单。整批结果写入 `caller.key.rotate` 管理审计——**不得**隐式改绑，也**不得**
// 为了让任务可绑定而放宽新 Key 的 scope。
// RotateResult reports the new key row, the rebound task ids and every task left bound.
type RotateResult struct {
	// Key 是落库后的新 Key 行（不含明文）。
	Key KeyRecord
	// ReboundTaskIDs 是已再绑定到新 Key 的任务标识。
	ReboundTaskIDs []string
	// LeftBound 是仍绑定在旧 Key 上的任务与原因。
	LeftBound []BoundTask
}

// ScopeSet 是发放期冻结的 scope 集合：已去重、保序、全部通过词表与权限校验。
// 只能由 `ParseScopeSet` 构造——直接写字面量无法绕过发放期拒绝。
// ScopeSet is a validated, de-duplicated, order-preserving scope set; ParseScopeSet
// is the only constructor so issuance cannot bypass validation.
type ScopeSet struct {
	scopes []string
}

// ParseScopeSet 校验并解析一组 scope：空集合、未知字面量、通配 `agents:invoke:*`
// 与管理面 scope 一律 `ErrInvalidScope`；重复项按首次出现去重（保序）。
// 词表取自 `core.KnownScope`，管理面判定取自 `core.AdminOnlyScope`——**不**各写一份。
// ParseScopeSet validates the scopes and de-duplicates them in first-seen order;
// vocabulary and admin-only rules come from core rather than local copies.
func ParseScopeSet(raw []string) (ScopeSet, error) {
	if len(raw) == 0 {
		return ScopeSet{}, fmt.Errorf("%w: scope 不得为空", ErrInvalidScope)
	}
	seen := make(map[string]struct{}, len(raw))
	scopes := make([]string, 0, len(raw))
	for _, scope := range raw {
		if !core.KnownScope(scope) || core.AdminOnlyScope(scope) {
			return ScopeSet{}, fmt.Errorf("%w: %q", ErrInvalidScope, scope)
		}
		if _, dup := seen[scope]; dup {
			continue
		}
		seen[scope] = struct{}{}
		scopes = append(scopes, scope)
	}
	return ScopeSet{scopes: scopes}, nil
}

// List 返回 scope 副本（升序不变、调用方改不动内部切片）。
// List returns a copy of the scopes in their original order.
func (s ScopeSet) List() []string { return append([]string(nil), s.scopes...) }

// IssueRequest 是一次签发请求：归属调用方、scope 集合与可选过期时刻。
// `ExpiresAt` 为 nil 表示不过期；时刻本身不在此判定（存储与认证层负责），
// 已过期的时刻只会签发出一把不可用的 Key，由 web 层按 400 挡在前面。
// IssueRequest carries the caller, the scopes and an optional expiry.
type IssueRequest struct {
	// CallerID 是归属调用方；空串按 `ErrNotFound` 拒绝（不存在这样的调用方）。
	CallerID string
	// Scopes 是发放期冻结的 scope 集合（发放后不可改，只能吊销重发）。
	Scopes []string
	// ExpiresAt 是过期时刻；nil 表示不过期。
	ExpiresAt *time.Time
}

// RevokeRequest 是一次吊销请求：归属调用方、Key 标识与写入审计的理由。
// `Reason` 是可选的自由文本，**不得**包含凭据；重复吊销幂等。
// RevokeRequest carries the caller, the key and an optional audit reason.
type RevokeRequest struct {
	// CallerID 是归属调用方。
	CallerID string
	// KeyID 是要吊销的 Key。
	KeyID string
	// Reason 是写入管理审计的理由（可为空，不得含凭据）。
	Reason string
}

// RotateRequest 是一次轮换请求。`Next` 只取 `Scopes` 与 `ExpiresAt`：KeyID / KeyHash /
// KeyPrefix / Status 由签发方生成，调用方给什么都不生效；`Next.CallerID` 为空表示沿用
// 外层 `CallerID`，给了不一致的值按 `ErrForbidden` 拒绝（跨调用方轮换不是本动作）。
// RotateRequest carries the old key and the next key's scopes/expiry; the identity
// fields of Next are minted here and a conflicting CallerID is refused.
type RotateRequest struct {
	// CallerID 是归属调用方。
	CallerID string
	// OldKeyID 是要被替换的旧 Key。
	OldKeyID string
	// Next 是新 Key 的 scope 与过期时刻来源。
	Next KeyRecord
	// RebindScheduledTasks 显式声明是否把符合条件的任务再绑定到新 Key（缺省 false）。
	RebindScheduledTasks bool
}

// IssuedKey 是签发 / 轮换的响应：Key 行的完整投影加**一次性明文**。
// `Secret` 只在这里出现一次，不落库、不进审计、不进日志与错误；客户端丢失只能
// 吊销重发（C-07）。
// IssuedKey is the one-time issuance response: the key row plus the plaintext secret,
// which exists nowhere else.
type IssuedKey struct {
	KeyRecord
	// Secret 是明文 Key，只在本次响应出现一次。
	Secret string
}

// KeyIssuer 执行 Key 的签发 / 轮换 / 吊销，并在每次变更前后写管理操作审计。
// 它不判定调用方是否存在（由存储的外键与 web 层负责），也不缓存任何 Key。
// KeyIssuer performs key issuance, rotation and revocation with fail-closed
// management-operation auditing; it keeps no keys in memory.
type KeyIssuer struct {
	keys   KeyStore
	ops    core.OperationStore
	logger *slog.Logger
	// now 只用于操作标识与审计时间；测试注入固定时钟。
	now func() time.Time
	// newSecret 生成明文 Key；测试注入熵失败。
	newSecret func() (string, error)
}

// NewKeyIssuer 构造签发器。`ops` 为 nil 时每次变更都拒绝执行——没有审计就不签发
// （宪法 V：预写失败不得执行）；`keys` 为 nil 同理。
// NewKeyIssuer builds the issuer; a nil operation store or key store refuses every
// change because auditing is a precondition of the change itself.
func NewKeyIssuer(keys KeyStore, ops core.OperationStore) *KeyIssuer {
	return &KeyIssuer{
		keys:      keys,
		ops:       ops,
		logger:    slog.Default(),
		now:       time.Now,
		newSecret: newKeySecret,
	}
}

// Issue 签发一把新 Key：先校验 scope（非法即 `ErrInvalidScope`，零写入、无操作行），
// 再生成明文与标识，预写 `pending`，写 Key 行，最后回填终态。
// Issue validates the scopes, pre-writes the pending operation, creates the key row
// and finishes the operation; the plaintext is returned exactly once.
func (i *KeyIssuer) Issue(ctx context.Context, actor core.Actor, req IssueRequest) (IssuedKey, error) {
	scopes, err := ParseScopeSet(req.Scopes)
	if err != nil {
		return IssuedKey{}, err
	}
	if err := i.ready(); err != nil {
		return IssuedKey{}, err
	}
	if req.CallerID == "" {
		return IssuedKey{}, fmt.Errorf("%w: 调用方标识为空", ErrNotFound)
	}
	secret, record, err := i.mint(req.CallerID, scopes, req.ExpiresAt)
	if err != nil {
		return IssuedKey{}, err
	}
	detail := issueOperationDetail{
		CallerID:            record.CallerID,
		KeyID:               record.KeyID,
		KeyPrefix:           record.KeyPrefix,
		Scopes:              record.Scopes,
		ExpiresAtUnixMillis: expiresAtMillis(record.ExpiresAt),
	}
	operationID, err := i.begin(ctx, actor, actionCallerKeyIssue, record.CallerID,
		issueRequestHash(req, scopes), detail)
	if err != nil {
		return IssuedKey{}, err
	}
	if createErr := i.keys.Create(ctx, record); createErr != nil {
		i.finish(ctx, operationID, core.OperationFailed, keyResultCode(createErr), createErr.Error(), detail)
		return IssuedKey{}, fmt.Errorf("签发调用方 Key 失败: %w", createErr)
	}
	i.finish(ctx, operationID, core.OperationSuccess, resultOK, "", detail)
	return IssuedKey{KeyRecord: record, Secret: secret}, nil
}

// Revoke 吊销一把 Key：先按 `(caller_id, key_id)` 定位（未命中即 `ErrNotFound`，
// 不留操作行），再预写 `pending`，最后置为 `revoked`。重复吊销幂等；**不**改动任何
// 定时任务的 `authorizing_key_id`——受影响任务在下次触发时 fail-closed（C-32）。
// Revoke looks the key up before writing anything, then pre-writes the pending
// operation and revokes; repeated revocation is idempotent and task bindings are
// never touched.
func (i *KeyIssuer) Revoke(ctx context.Context, actor core.Actor, req RevokeRequest) error {
	if err := i.ready(); err != nil {
		return err
	}
	if req.CallerID == "" || req.KeyID == "" {
		return fmt.Errorf("%w: 调用方或 Key 标识为空", ErrNotFound)
	}
	record, err := i.keys.LookupByID(ctx, req.CallerID, req.KeyID)
	if err != nil {
		return err
	}
	detail := revokeOperationDetail{
		CallerID:  record.CallerID,
		KeyID:     record.KeyID,
		KeyPrefix: record.KeyPrefix,
		Reason:    req.Reason,
	}
	operationID, err := i.begin(ctx, actor, actionCallerKeyRevoke, record.CallerID,
		revokeRequestHash(req), detail)
	if err != nil {
		return err
	}
	if setErr := i.keys.SetStatus(ctx, req.CallerID, req.KeyID, KeyStatusRevoked); setErr != nil {
		i.finish(ctx, operationID, core.OperationFailed, keyResultCode(setErr), setErr.Error(), detail)
		return fmt.Errorf("吊销调用方 Key 失败: %w", setErr)
	}
	i.finish(ctx, operationID, core.OperationSuccess, resultOK, "", detail)
	return nil
}

// Rotate 用一把新 Key 替换旧 Key：scope 校验在最前（非法即 `ErrInvalidScope`），
// 旧 Key 必须存在且仍为 `active`（否则拒绝且不留操作行），归属声明不一致按
// `ErrForbidden` 拒绝；随后预写 `pending`，由存储层在**单个事务**内新建新 Key、
// 吊销旧 Key、按 `RebindScheduledTasks` 再绑定任务，最后把整批结果写进审计。
// 返回的一次性明文与 `Issue` 同规——只在本次响应出现一次。
// Rotate replaces the old key in one storage transaction and reports every task that
// stayed bound; the plaintext is returned exactly once.
func (i *KeyIssuer) Rotate(ctx context.Context, actor core.Actor, req RotateRequest) (IssuedKey, RotateResult, error) {
	scopes, err := ParseScopeSet(req.Next.Scopes)
	if err != nil {
		return IssuedKey{}, RotateResult{}, err
	}
	if err := i.ready(); err != nil {
		return IssuedKey{}, RotateResult{}, err
	}
	if req.CallerID == "" || req.OldKeyID == "" {
		return IssuedKey{}, RotateResult{}, fmt.Errorf("%w: 调用方或旧 Key 标识为空", ErrNotFound)
	}
	if req.Next.CallerID != "" && req.Next.CallerID != req.CallerID {
		return IssuedKey{}, RotateResult{}, fmt.Errorf("%w: 轮换请求的归属调用方与旧 Key 不一致", ErrForbidden)
	}
	old, err := i.keys.LookupByID(ctx, req.CallerID, req.OldKeyID)
	if err != nil {
		return IssuedKey{}, RotateResult{}, err
	}
	if old.Status != KeyStatusActive {
		// 前置条件不满足即拒绝：已吊销 / 已过期的 Key 不允许被"轮换救活"，
		// 此时变更尚未开始，按 404 / 412 的口径**不写**操作行。
		return IssuedKey{}, RotateResult{}, fmt.Errorf("旧 Key %s 当前状态为 %q，只有 active 的 Key 可轮换",
			old.KeyID, old.Status)
	}
	secret, next, err := i.mint(req.CallerID, scopes, req.Next.ExpiresAt)
	if err != nil {
		return IssuedKey{}, RotateResult{}, err
	}
	detail := rotateOperationDetail{
		CallerID:             next.CallerID,
		OldKeyID:             old.KeyID,
		KeyID:                next.KeyID,
		KeyPrefix:            next.KeyPrefix,
		Scopes:               next.Scopes,
		ExpiresAtUnixMillis:  expiresAtMillis(next.ExpiresAt),
		RebindScheduledTasks: req.RebindScheduledTasks,
	}
	operationID, err := i.begin(ctx, actor, actionCallerKeyRotate, next.CallerID,
		rotateRequestHash(req, scopes), detail)
	if err != nil {
		return IssuedKey{}, RotateResult{}, err
	}
	result, rotateErr := i.keys.Rotate(ctx, req.CallerID, req.OldKeyID, next,
		RotateOptions{RebindScheduledTasks: req.RebindScheduledTasks})
	if rotateErr != nil {
		i.finish(ctx, operationID, core.OperationFailed, keyResultCode(rotateErr), rotateErr.Error(), detail)
		return IssuedKey{}, RotateResult{}, fmt.Errorf("轮换调用方 Key 失败: %w", rotateErr)
	}
	detail.ReboundTaskIDs = append([]string(nil), result.ReboundTaskIDs...)
	detail.LeftBound = leftBoundDetails(result.LeftBound)
	i.finish(ctx, operationID, core.OperationSuccess, resultOK, "", detail)
	return IssuedKey{KeyRecord: result.Key, Secret: secret}, result, nil
}

// ready 报告签发器的两端是否都装配好：缺 Key 存储无法落库，缺操作存储无法留痕——
// 两者任一缺失都必须拒绝变更，而不是"先做了再说"。
// ready reports whether both ports are wired; either missing refuses the change.
func (i *KeyIssuer) ready() error {
	if i.keys == nil || i.ops == nil {
		return errors.New("调用方 Key 生命周期缺少存储依赖（装配错误），拒绝执行")
	}
	return nil
}

// mint 生成一次性的明文与 Key 行：标识用 core 的 Crockford 生成器，明文只在这里
// 出现，行里只留指纹与前缀。熵取不到时返回错误——**不**回落到可预测值。
// mint produces the plaintext and the row projection; entropy failure is reported
// rather than replaced by a predictable secret.
func (i *KeyIssuer) mint(callerID string, scopes ScopeSet, expiresAt *time.Time) (string, KeyRecord, error) {
	secret, err := i.newSecret()
	if err != nil {
		return "", KeyRecord{}, err
	}
	keyID, err := core.NewCallerKeyID()
	if err != nil {
		return "", KeyRecord{}, err
	}
	return secret, KeyRecord{
		KeyID:     keyID,
		CallerID:  callerID,
		KeyHash:   keyFingerprint(secret),
		KeyPrefix: keyPrefix(secret),
		Scopes:    scopes.List(),
		ExpiresAt: expiresAt,
		Status:    KeyStatusActive,
	}, nil
}

// begin 预写一条 `pending` 管理操作行并返回操作标识；预写失败即拒绝执行（宪法 V），
// 此时目标一个字节都没动。实现委托给 `operationWriter`——两条管理面共用同一份
// fail-closed 纪律，**不**各写一份。
// begin pre-writes the pending operation through the shared writer.
func (i *KeyIssuer) begin(ctx context.Context, actor core.Actor, action, targetID, requestHash string, detail any) (string, error) {
	return i.writer().begin(ctx, actor, action, targetID, requestHash, detail)
}

// finish 回填终态；回填不随请求取消而放弃（客户端断开不能让审计停在 pending）。
// 回填失败只记 ERROR 并留待启动恢复查证：业务变更可能已经生效，因此**不**把回填
// 失败当成业务失败返回——否则调用方会重试并签发第二把 Key。
// finish writes the terminal state; a failure is logged rather than reported.
func (i *KeyIssuer) finish(ctx context.Context, operationID string, status core.OperationStatus,
	code, message string, detail any,
) {
	i.writer().finish(ctx, operationID, status, code, message, detail)
}

// writer 用签发器自己的依赖构造共享写法器；每次调用现构造，不缓存（保持字段唯一）。
// writer builds the shared writer from the issuer's own dependencies.
func (i *KeyIssuer) writer() operationWriter {
	return operationWriter{ops: i.ops, logger: i.logger, now: i.now, subject: subjectKeyOperation}
}

// issueOperationDetail 是 `caller.key.issue` 的 `detail_json` 固定投影。
// **不含**明文：审计只记"签发了哪一把、什么权限、什么时候过期"。
// issueOperationDetail is the fixed issue projection; it never carries the plaintext.
type issueOperationDetail struct {
	CallerID            string   `json:"caller_id"`
	KeyID               string   `json:"key_id"`
	KeyPrefix           string   `json:"key_prefix"`
	Scopes              []string `json:"scopes"`
	ExpiresAtUnixMillis int64    `json:"expires_at_unix_ms"`
}

// revokeOperationDetail 是 `caller.key.revoke` 的固定投影；`reason` 原样进审计。
// revokeOperationDetail is the fixed revoke projection.
type revokeOperationDetail struct {
	CallerID  string `json:"caller_id"`
	KeyID     string `json:"key_id"`
	KeyPrefix string `json:"key_prefix"`
	Reason    string `json:"reason,omitempty"`
}

// rotateOperationDetail 是 `caller.key.rotate` 的固定投影：请求部分在预写时写入，
// 结果部分（`rebound_task_ids` / `left_bound`）在终态回填时补齐。
// rotateOperationDetail is the fixed rotate projection: request fields at pre-write
// time, result fields at finish time.
type rotateOperationDetail struct {
	CallerID             string            `json:"caller_id"`
	OldKeyID             string            `json:"old_key_id"`
	KeyID                string            `json:"key_id"`
	KeyPrefix            string            `json:"key_prefix"`
	Scopes               []string          `json:"scopes"`
	ExpiresAtUnixMillis  int64             `json:"expires_at_unix_ms"`
	RebindScheduledTasks bool              `json:"rebind_scheduled_tasks"`
	ReboundTaskIDs       []string          `json:"rebound_task_ids,omitempty"`
	LeftBound            []boundTaskDetail `json:"left_bound,omitempty"`
}

// boundTaskDetail 是未能再绑定任务在审计里的逐条形态。
// boundTaskDetail is the per-task shape of a left-bound entry.
type boundTaskDetail struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// leftBoundDetails 转换未能再绑定的任务清单；nil 保持 nil（审计里省略该键）。
// leftBoundDetails converts the left-bound list, keeping nil nil.
func leftBoundDetails(tasks []BoundTask) []boundTaskDetail {
	if len(tasks) == 0 {
		return nil
	}
	out := make([]boundTaskDetail, 0, len(tasks))
	for _, task := range tasks {
		// 两个结构体字段布局相同，直接转换（结构体标签不参与底层类型比较）；
		// 任何一侧新增字段都会在编译期报错，比逐字段抄写更不容易漏。
		out = append(out, boundTaskDetail(task))
	}
	return out
}

// keyResultCode 把存储错误投影成操作 `result_code`；未识别的错误一律 `INTERNAL`。
// keyResultCode projects storage errors into result codes.
func keyResultCode(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return resultNotFound
	case errors.Is(err, ErrForbidden):
		return resultForbidden
	case errors.Is(err, ErrInvalidScope):
		return resultInvalidScope
	default:
		return resultInternal
	}
}

// issueRequestHash 是签发请求的脱敏摘要（scope 与过期时刻参与，**不含**明文）。
// issueRequestHash is the redacted request digest of an issuance.
func issueRequestHash(req IssueRequest, scopes ScopeSet) string {
	return operationRequestHash("issue", req.CallerID,
		strings.Join(scopes.List(), ","), fmt.Sprint(expiresAtMillis(req.ExpiresAt)))
}

// revokeRequestHash 是吊销请求的脱敏摘要（理由参与：同一把 Key 换理由复议可区分）。
// revokeRequestHash is the redacted request digest of a revocation.
func revokeRequestHash(req RevokeRequest) string {
	return operationRequestHash("revoke", req.CallerID, req.KeyID, req.Reason)
}

// rotateRequestHash 是轮换请求的脱敏摘要（再绑定声明参与：同一轮换的两种取舍
// 不是同一个请求）。
// rotateRequestHash is the redacted request digest of a rotation.
func rotateRequestHash(req RotateRequest, scopes ScopeSet) string {
	return operationRequestHash("rotate", req.CallerID, req.OldKeyID,
		strings.Join(scopes.List(), ","), fmt.Sprint(expiresAtMillis(req.Next.ExpiresAt)),
		fmt.Sprint(req.RebindScheduledTasks))
}

// operationRequestHash 用 NUL 分隔各字段再做 SHA-256：分隔符不出现在任何字段里，
// 避免「a|b + c」与「a + b|c」算出同一摘要。
// operationRequestHash joins fields with NUL before hashing so field boundaries are
// unambiguous.
func operationRequestHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// expiresAtMillis 把过期时刻换成 Unix 毫秒 UTC；nil 返回 0（表示不过期）。
// 与库里的时间列同一表示（C-13 / C-14）。
// expiresAtMillis renders the expiry as Unix milliseconds UTC; nil is 0.
func expiresAtMillis(at *time.Time) int64 {
	if at == nil {
		return 0
	}
	return at.UnixMilli()
}

// newKeySecret 生成一把明文 Key；随机源失败时返回错误，不回落到可预测值。
// newKeySecret mints the plaintext; randomness failure is reported.
func newKeySecret() (string, error) {
	var buf [keySecretBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("生成调用方 Key 随机源失败: %w", err)
	}
	return keySecretPrefix + hex.EncodeToString(buf[:]), nil
}

// keyFingerprint 是明文 Key 的 SHA-256 十六进制指纹（小写）——认证路径的查表键，
// 也是库里唯一保存的 Key 派生值（C-07）。与 `Authenticator` 用同一算法。
// keyFingerprint is the lowercase hex SHA-256 used as the lookup key and the only
// key-derived value stored.
func keyFingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// keyPrefix 取展示前缀（前 8 个字符）；短于此的输入原样返回（正常路径不会出现）。
// keyPrefix returns the eight-character display prefix.
func keyPrefix(secret string) string {
	if len(secret) <= keyPrefixLength {
		return secret
	}
	return secret[:keyPrefixLength]
}
