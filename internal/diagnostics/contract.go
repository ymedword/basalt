package diagnostics

// 本文件是诊断契约 v0（颗粒度文档 §2.9 / §3.1.6，任务 T123）：事件字段逐列对应冻结
// 契约 `diagnostic_events`，标识按契约为 `ev_` + 26 位 Crockford Base32（ULID 形态，
// 含 48 位毫秒时间前缀），`schema_version` 固定 0。
// This file is the diagnostics contract v0: the event fields mirror the frozen
// diagnostic_events table column by column, identifiers are the contract's `ev_` prefix
// plus 26 Crockford Base32 characters carrying a 48-bit millisecond prefix, and
// schema_version is fixed at 0.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	// SchemaVersion 是诊断契约版本；v0 固定为 0，演进只增字段。
	// SchemaVersion is the diagnostics contract version; v0 is fixed at 0 and only
	// grows by adding fields.
	SchemaVersion = 0

	// EventIDPrefix 是事件标识的固定前缀（契约 `diagnostic_events.event_id`）。
	// EventIDPrefix is the fixed prefix of an event identifier.
	EventIDPrefix = "ev_"

	// eventIDPayloadLength 是 Crockford Base32 载荷长度：26 字符承载 130 位，其中
	// 高 2 位恒为 0，余下 128 位 = 48 位毫秒时间前缀 + 80 位随机。
	eventIDPayloadLength = 26

	// eventIDMillisLimit 是 48 位时间前缀的上界（2^48 - 1 毫秒）。
	eventIDMillisLimit = int64(1)<<48 - 1
)

// crockfordAlphabet 是 Crockford Base32 字符集，与 `core` 的 `ag_` / `rs_` 载荷同一份
// 字面量：排除 I / L / O / U 四个易混字符。此处复制常量而不从 `core` 导出，是因为事件
// 标识的时间前缀布局 core 没有，而 diagnostics 不该为一个字符集反向依赖 core。
// crockfordAlphabet is the Crockford Base32 alphabet shared with core's agent and
// resource identifiers. The constant is copied rather than exported from core: event
// identifiers need a time-prefixed layout core does not have.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ErrInvalidEvent 表示事件不满足契约约束（主锚点缺失、枚举越界、标识格式错）。
// ErrInvalidEvent reports an event that violates the contract constraints.
var ErrInvalidEvent = errors.New("invalid diagnostic event")

// ErrInvalidEventID 表示事件标识不是 `ev_` + 26 位 Crockford 载荷，或时间无法承载。
// ErrInvalidEventID reports a malformed event identifier or an unrepresentable time.
var ErrInvalidEventID = errors.New("invalid diagnostic event id")

// Kind 是诊断事件种类的稳定枚举（契约 `DiagnosticKind` 十值）。
// Kind is the stable event-kind enumeration frozen by the contract.
type Kind string

const (
	// KindIngestStage 覆盖摄取流水线的阶段推进。
	KindIngestStage Kind = "ingest_stage"
	// KindIndexSwitch 覆盖索引代际切换。
	KindIndexSwitch Kind = "index_switch"
	// KindRetrieval 覆盖检索（候选 / 命中 / 拒绝）。
	KindRetrieval Kind = "retrieval"
	// KindAssembly 覆盖上下文装配与裁剪。
	KindAssembly Kind = "assembly"
	// KindEmbedding 覆盖向量化调用。
	KindEmbedding Kind = "embedding"
	// KindLLMCall 覆盖模型调用。
	KindLLMCall Kind = "llm_call"
	// KindToolCall 覆盖工具调用。
	KindToolCall Kind = "tool_call"
	// KindPermissionDecision 覆盖权限判定。
	KindPermissionDecision Kind = "permission_decision"
	// KindQuotaDecision 覆盖入口配额与速率判定。
	KindQuotaDecision Kind = "quota_decision"
	// KindMigration 覆盖迁移与切换。
	KindMigration Kind = "migration"
)

// allKinds 是契约枚举的完整取值集合；`Valid` 与契约测试都只认这一份清单——新增或删除
// 取值必须同时改契约，否则 `TestDiagnostics_ContractFields` 变红。
// allKinds is the complete contract enumeration; Valid and the contract test both read
// this single list, so adding or dropping a value without the contract turns red.
var allKinds = []Kind{
	KindIngestStage, KindIndexSwitch, KindRetrieval, KindAssembly, KindEmbedding,
	KindLLMCall, KindToolCall, KindPermissionDecision, KindQuotaDecision, KindMigration,
}

// Valid 报告种类是否落在契约枚举内。
// Valid reports whether the kind is one of the contract values.
func (k Kind) Valid() bool { return slices.Contains(allKinds, k) }

// ReasonCode 是稳定原因码枚举（契约 `DiagnosticReasonCode` 十七值）；面向人的文案由
// UI 映射，**不得**把自由文本写进该字段。
// ReasonCode is the stable reason enumeration; human-facing text is mapped by the UI.
type ReasonCode string

const (
	// ReasonVectorUnavailable 表示向量通道不可用（降级到关键词）。
	ReasonVectorUnavailable ReasonCode = "vector_unavailable"
	// ReasonKeywordUnavailable 表示关键词通道不可用。
	ReasonKeywordUnavailable ReasonCode = "keyword_unavailable"
	// ReasonRerankTimeout 表示重排超时。
	ReasonRerankTimeout ReasonCode = "rerank_timeout"
	// ReasonPermissionDenied 表示权限拒绝；此时**不得**记录被拒资料。
	ReasonPermissionDenied ReasonCode = "permission_denied"
	// ReasonBudgetTrimmed 表示按预算裁剪。
	ReasonBudgetTrimmed ReasonCode = "budget_trimmed"
	// ReasonHistoryTrimmed 表示历史被裁剪。
	ReasonHistoryTrimmed ReasonCode = "history_trimmed"
	// ReasonNoAnswer 表示没有可用答案。
	ReasonNoAnswer ReasonCode = "no_answer"
	// ReasonCitationUnresolved 表示引用无法落位。
	ReasonCitationUnresolved ReasonCode = "citation_unresolved"
	// ReasonChunkerFailed 表示切分失败。
	ReasonChunkerFailed ReasonCode = "chunker_failed"
	// ReasonEmbedFailed 表示向量化失败。
	ReasonEmbedFailed ReasonCode = "embed_failed"
	// ReasonIndexFailed 表示建索引失败。
	ReasonIndexFailed ReasonCode = "index_failed"
	// ReasonQuotaExhausted 表示配额耗尽。
	ReasonQuotaExhausted ReasonCode = "quota_exhausted"
	// ReasonRateLimited 表示速率受限。
	ReasonRateLimited ReasonCode = "rate_limited"
	// ReasonKeyRevoked 表示调用方 Key 已撤销。
	ReasonKeyRevoked ReasonCode = "key_revoked"
	// ReasonMigrationVerifyFailed 表示迁移校验失败。
	ReasonMigrationVerifyFailed ReasonCode = "migration_verify_failed"
	// ReasonLLMCallFailed 表示模型调用本身失败（供应商错误 / 超时 / 协议解析失败）。
	// 该码记的是**发出的调用没拿到可用响应**；调用被审计预写拦住（根本没发出）不属于它，
	// 那种情形由审计 fail-closed 路径回答，诊断不得冒充一次失败的模型调用。
	// ReasonLLMCallFailed names a model call that went out and came back unusable.
	ReasonLLMCallFailed ReasonCode = "llm_call_failed"
	// ReasonConcurrencyLimited 表示并发名额不足（Run 准入 / 调用级并发上限）。
	// 与 `quota_exhausted`（周期配额用尽）区分：一个会随在跑请求结束而恢复，另一个
	// 要等到下个配额周期，排障方向完全不同。
	// ReasonConcurrencyLimited separates a full slot table from an exhausted period quota.
	ReasonConcurrencyLimited ReasonCode = "concurrency_limited"
	// ReasonChunkInvalidated 表示轮次间撤权/删除/换版导致注入范围收窄。
	// ReasonChunkInvalidated records narrowed context after revalidation.
	ReasonChunkInvalidated ReasonCode = "chunk_invalidated"
)

// allReasonCodes 是原因码契约枚举的完整取值集合（与 `allKinds` 同一条纪律）。
// allReasonCodes is the complete reason enumeration, governed like allKinds.
var allReasonCodes = []ReasonCode{
	ReasonVectorUnavailable, ReasonKeywordUnavailable, ReasonRerankTimeout,
	ReasonPermissionDenied, ReasonBudgetTrimmed, ReasonHistoryTrimmed, ReasonNoAnswer,
	ReasonCitationUnresolved, ReasonChunkerFailed, ReasonEmbedFailed, ReasonIndexFailed,
	ReasonQuotaExhausted, ReasonRateLimited, ReasonKeyRevoked, ReasonMigrationVerifyFailed,
	ReasonLLMCallFailed, ReasonConcurrencyLimited, ReasonChunkInvalidated,
}

// Valid 报告原因码是否落在契约枚举内；空串表示未给出原因，按合法处理。
// Valid reports whether the reason code is a contract value; the empty string means
// "no reason given" and is accepted.
func (r ReasonCode) Valid() bool {
	return r == "" || slices.Contains(allReasonCodes, r)
}

// Decision 是判定结果的稳定枚举（契约 `decision` 列）。
// Decision is the stable enumeration of event decisions.
type Decision string

const (
	// DecisionAllow 表示放行。
	DecisionAllow Decision = "allow"
	// DecisionDeny 表示拒绝。
	DecisionDeny Decision = "deny"
	// DecisionTrim 表示裁剪。
	DecisionTrim Decision = "trim"
	// DecisionDegrade 表示降级。
	DecisionDegrade Decision = "degrade"
)

// allDecisions 是判定枚举的完整取值集合。
// allDecisions is the complete decision enumeration.
var allDecisions = []Decision{DecisionAllow, DecisionDeny, DecisionTrim, DecisionDegrade}

// Valid 报告判定是否落在枚举内；空串表示未给出判定，按合法处理。
// Valid reports whether the decision is a contract value; the empty string is accepted.
func (d Decision) Valid() bool { return d == "" || slices.Contains(allDecisions, d) }

// Actor 是"谁在执行"的稳定枚举（契约 `actor` 列，与 `runs.actor` 同口径）；归属调用方
// 由 `run_id` / `task_id` 关联查询，**不得**在这里记调用方身份或冒充管理员。
// Actor names who is executing; ownership is derived from correlation, never from here.
type Actor string

const (
	// ActorCaller 表示外部调用方请求。
	ActorCaller Actor = "caller"
	// ActorScheduler 表示调度器触发。
	ActorScheduler Actor = "scheduler"
	// ActorRecovery 表示恢复路径。
	ActorRecovery Actor = "recovery"
	// ActorAdminCLI 表示管理命令行。
	ActorAdminCLI Actor = "admin-cli"
)

// allActors 是执行者枚举的完整取值集合。
// allActors is the complete actor enumeration.
var allActors = []Actor{ActorCaller, ActorScheduler, ActorRecovery, ActorAdminCLI}

// Valid 报告执行者是否落在枚举内；空串表示未标注，按合法处理。
// Valid reports whether the actor is a contract value; the empty string is accepted.
func (a Actor) Valid() bool { return a == "" || slices.Contains(allActors, a) }

// Correlation 是一组关联标识：主锚点（RequestID / RunID / OperationID / IngestJobID）
// 至少一个，TaskID 只是调度限定符，**不得**单独成立；**不得**为了凑关联伪造 Run。
// Correlation groups the correlation anchors; at least one primary anchor is required
// and TaskID only qualifies a scheduled event.
type Correlation struct {
	// RequestID 是入口请求标识，入口期事件（尚未产生 Run）用它作主锚点。
	RequestID string
	// RunID 是 Run 标识，Run 级事件用它作主锚点。
	RunID string
	// TaskID 是调度任务标识，只作限定符。
	TaskID string
	// OperationID 是管理操作标识，管理面事件用它作主锚点。
	OperationID string
	// IngestJobID 是摄取作业标识，摄取事件用它作主锚点。
	IngestJobID string
}

// Validate 校验主锚点至少一个——与契约的
// `CHECK (num_nonnulls(request_id, run_id, operation_id, ingest_job_id) >= 1)` 同口径。
// Validate enforces at least one primary anchor, mirroring the table's CHECK constraint.
func (c Correlation) Validate() error {
	if c.RequestID == "" && c.RunID == "" && c.OperationID == "" && c.IngestJobID == "" {
		return fmt.Errorf("%w: 主锚点缺失——request_id / run_id / operation_id / ingest_job_id 至少一个，task_id 只是限定符", ErrInvalidEvent)
	}
	return nil
}

// AuditRef 指向一条审计行（`llm_calls` / `tool_invocations` / `embedding_calls`）：
// 只引用，不替代审计。契约把三张表的 `id` 定为身份列（bigint，无表前缀），单看数字
// 无法回指到表，因此存储列 `audit_ref text` 以 `table:id` 形式承载，读侧还原成对象。
// AuditRef points at one audit row. The three tables use unprefixed bigint keys, so the
// text column stores `table:id` and the read side restores this structure.
type AuditRef struct {
	// Table 是审计表名，取值由契约枚举冻结。
	Table string
	// ID 是审计行的主键（十进制串）。
	ID string
}

// auditRefTables 是契约允许的审计表（`DiagnosticEventView.audit_ref.table` 枚举）。
// auditRefTables is the contract's closed set of audit tables.
var auditRefTables = map[string]struct{}{
	"llm_calls":        {},
	"tool_invocations": {},
	"embedding_calls":  {},
}

// Validate 校验表名在封闭枚举内且主键非空。
// Validate checks the table is one of the contract values and the id is present.
func (r AuditRef) Validate() error {
	if _, ok := auditRefTables[r.Table]; !ok {
		return fmt.Errorf("%w: 审计表 %q 不在 llm_calls / tool_invocations / embedding_calls 之内", ErrInvalidEvent, r.Table)
	}
	if r.ID == "" {
		return fmt.Errorf("%w: 审计行主键为空", ErrInvalidEvent)
	}
	return nil
}

// String 返回存储形态 `table:id`。
// String returns the storage form `table:id`.
func (r AuditRef) String() string { return r.Table + ":" + r.ID }

// ParseAuditRef 解析存储形态 `table:id` 并校验表名枚举。
// ParseAuditRef parses the storage form and validates the table enumeration.
func ParseAuditRef(text string) (AuditRef, error) {
	table, id, ok := strings.Cut(text, ":")
	if !ok || table == "" || id == "" {
		return AuditRef{}, fmt.Errorf("%w: 审计引用 %q 不是 table:id 形态", ErrInvalidEvent, text)
	}
	ref := AuditRef{Table: table, ID: id}
	if err := ref.Validate(); err != nil {
		return AuditRef{}, err
	}
	return ref, nil
}

// DiagnosticEvent 是诊断事件（契约 `diagnostic_events` 的逐列投影）。
// DiagnosticEvent is the event, projected column by column from the frozen table.
type DiagnosticEvent struct {
	// EventID 是 `ev_` + 26 位 Crockford 载荷的全局唯一标识（`NewEventID` 生成）。
	EventID string
	// SchemaVersion 是契约版本，当前固定 0。
	SchemaVersion int
	// Kind 是事件种类。
	Kind Kind
	// OccurredAt 是发生时间（Unix 毫秒 UTC）。
	OccurredAt int64
	// Correlation 是关联标识；主锚点至少一个。
	Correlation Correlation
	// Actor 是谁在执行；归属不在这里。
	Actor Actor
	// StepID 是同一 correlation 内的步骤标识。
	StepID string
	// AttemptID 是重试尝试标识（不使用全局序号）。
	AttemptID string
	// ParentStepID 是同一 correlation 内的父步骤标识。
	ParentStepID string
	// Seq 是同一 correlation 内的本地序号，仅局部有序；0 表示未设置（落库为 NULL）。
	Seq int64
	// ReasonCode 是稳定原因码。
	ReasonCode ReasonCode
	// Decision 是判定结果。
	Decision Decision
	// Attrs 是已脱敏的白名单键值；**不得**含正文、凭证或完整 URL。
	Attrs map[string]any
	// AuditRef 指向审计行；nil 表示不关联。
	AuditRef *AuditRef
}

// Validate 校验事件是否满足契约：标识格式、版本、枚举、主锚点、序号与审计引用。
// 这里只是可判定的形状校验，写入侧的 fail-open 语义归 `Recorder`（诊断可丢弃）。
// Validate checks the event against the contract: identifier, version, enumerations,
// correlation anchors, sequence and audit reference.
func (e DiagnosticEvent) Validate() error {
	if err := ValidateEventID(e.EventID); err != nil {
		return err
	}
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version 必须是 %d，实际 %d", ErrInvalidEvent, SchemaVersion, e.SchemaVersion)
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("%w: 未知事件种类 %q", ErrInvalidEvent, e.Kind)
	}
	if e.OccurredAt < 0 {
		return fmt.Errorf("%w: occurred_at 必须是非负 Unix 毫秒，实际 %d", ErrInvalidEvent, e.OccurredAt)
	}
	if err := e.Correlation.Validate(); err != nil {
		return err
	}
	if !e.Actor.Valid() {
		return fmt.Errorf("%w: 未知执行者 %q", ErrInvalidEvent, e.Actor)
	}
	if e.Seq < 0 {
		return fmt.Errorf("%w: seq 必须是非负局部序号，实际 %d", ErrInvalidEvent, e.Seq)
	}
	if !e.ReasonCode.Valid() {
		return fmt.Errorf("%w: 未知原因码 %q", ErrInvalidEvent, e.ReasonCode)
	}
	if !e.Decision.Valid() {
		return fmt.Errorf("%w: 未知判定 %q", ErrInvalidEvent, e.Decision)
	}
	if e.AuditRef != nil {
		if err := e.AuditRef.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// NewEventID 生成 `ev_` + 26 位 Crockford Base32：前 48 位是 `now` 的 Unix 毫秒（UTC），
// 其余 80 位来自 crypto/rand。时间无法用 48 位承载或随机源失败时报错，**不**回落到可
// 预测值。同一毫秒内的唯一性由随机位保证，事件只承诺**局部有序**。
// NewEventID mints an identifier whose first 48 bits are the millisecond timestamp and
// whose remaining 80 bits are random. Failures are reported, never replaced by a
// predictable fallback; within a millisecond uniqueness rests on the random bits.
func NewEventID(now time.Time) (string, error) {
	ms := now.UTC().UnixMilli()
	if ms < 0 || ms > eventIDMillisLimit {
		return "", fmt.Errorf("%w: %s 超出 48 位毫秒前缀范围", ErrInvalidEventID, now.UTC().Format(time.RFC3339Nano))
	}
	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("生成事件标识随机源失败: %w", err)
	}
	out := make([]byte, 0, len(EventIDPrefix)+eventIDPayloadLength)
	out = append(out, EventIDPrefix...)
	return string(append(out, encodeEventIDPayload(ms, random)...)), nil
}

// encodeEventIDPayload 把 48 位毫秒 + 80 位随机编码成 26 个 Crockford 字符：130 位里最高
// 2 位是零填充，因此首字符只承载 3 位时间、必然落在 `0`–`7`；后 16 个字符是 80 位随机的
// 高位在前分组。全程只做移位与加宽转换，**不**把毫秒截断成单字节。
// encodeEventIDPayload encodes the 48-bit millisecond stamp and 80 random bits into 26
// Crockford characters; the two leading padding bits keep the first char at 0-7 and the
// remaining sixteen characters carry the random bits, high bits first.
func encodeEventIDPayload(ms int64, random [10]byte) []byte {
	out := make([]byte, eventIDPayloadLength)
	out[0] = crockfordAlphabet[(ms>>45)&0x7]
	for i := 1; i <= 9; i++ {
		out[i] = crockfordAlphabet[(ms>>(45-5*i))&0x1F]
	}

	var high, low uint64
	for i := range 5 {
		high = high<<8 | uint64(random[i])
		low = low<<8 | uint64(random[5+i])
	}
	for i := range 16 {
		shift := 75 - 5*i
		var group uint64
		if shift >= 40 {
			group = (high >> uint(shift-40)) & 0x1F
		} else {
			group = (low >> uint(shift)) & 0x1F
		}
		out[10+i] = crockfordAlphabet[group]
	}
	return out
}

// ValidateEventID 校验标识的前缀、长度与字符集，并要求首字符在 `0`–`7`（高 2 位零填充）。
// ValidateEventID checks prefix, length and alphabet, and requires the first character
// to stay within 0-7 because of the two leading padding bits.
func ValidateEventID(id string) error {
	payload, ok := strings.CutPrefix(id, EventIDPrefix)
	if !ok {
		return fmt.Errorf("%w: 缺少 %q 前缀", ErrInvalidEventID, EventIDPrefix)
	}
	if len(payload) != eventIDPayloadLength {
		return fmt.Errorf("%w: 载荷长度必须是 %d，实际 %d", ErrInvalidEventID, eventIDPayloadLength, len(payload))
	}
	for _, c := range payload {
		if !strings.ContainsRune(crockfordAlphabet, c) {
			return fmt.Errorf("%w: 载荷含非法字符 %q", ErrInvalidEventID, c)
		}
	}
	if payload[0] > '7' {
		return fmt.Errorf("%w: 首字符 %q 超出 130 位编码的高 2 位零填充", ErrInvalidEventID, payload[0])
	}
	return nil
}
