package knowledge

// 本文件实现来源视图（US-16 §2.10.4 / §3.1.3 / C-26 / T040）：按 `EvidenceRef`
// **精确读取**某个版本的原文、结构化来源事实与来源 / 章节摘要。五条硬约束：
//
//  1. **身份显式必填**：`SourceView` 的 Principal 是参数而不是请求体字段；零值 →
//     `core.ErrUnauthorized`，且在读取任何正文之前拒绝；
//  2. **每次重查当前状态**（C-26）：ACL、删除与版本都按**本次** Principal 重算——
//     两个调用方读同一 `EvidenceRef` 可以有不同结果（一个 403，一个 200）；
//  3. **不得用新原文冒充旧引用**：只读引用点名的版本；版本已被取代时回**旧版本**
//     原文并置 `Superseded=true`，绝不改取 `latest_version_id`；
//  4. **截断必须显式**：`budget_tokens` 超限时返回前缀 + `Truncated=true` +
//     `NextCursor`；范围越界是请求级错误，**不得**夹到文档末尾装成合法窗口；
//  5. **来源摘要不含任务进度**（K-02）：`SourceSummary` 只有文档标题 / 标题路径 /
//     章节摘要三个字段，全部来自来源自身；任务状态归 C，**不得**混进这里。
//
// **正文口径**（缺陷修复 R59）：`Text` 非空即"逐字原文"——切分器裁掉标题行与首尾
// 空白，片段重建不出原文，因此存储端只在快照存在、或重建结果的 sha256 等于
// `content_hash` 时才回正文。`Text` 为空表示该版本没有可回读的原文：此时只回状态，
// `content_hash` 与 `conversion` 一并置空，**不得**把近似文本标成 `verbatim`。
// A non-empty Text always means the verbatim body; an empty one means the body is not
// retained, and the read must not pretend otherwise.
//
// 契约口径裁决（tasks.md 附录 R50.2）：冻结契约 `ValidityView.reason` 的
// `version_changed` 表示"引用的版本不是当前版本"，且端点描述规定 `validity.valid=
// false` 时正文 / 事实 / 摘要**一律为空**。本实现在 Go 侧保留旧版本正文并置
// `Superseded`（设计 R9 的验收断言如此），HTTP 投影（T042）**必须**按契约清空——
// `Superseded` **不得**投影成新的响应字段（契约没有该字段），两个面各自遵守各自
// 的事实源：Go 侧以设计卡为准，HTTP 侧以冻结契约为准。
//
// This file implements the source view: an exact read by EvidenceRef with a fresh
// ACL / deletion / version check on every call. A superseded version is served as its
// own old text plus an explicit Superseded flag and is never replaced by the newer
// text; the HTTP projection must blank the body when validity.valid is false per the
// frozen contract, and Superseded itself is never projected as a response field.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// ErrForbidden 是来源视图的越权拒绝（scope 不足或集合 ACL 拒绝）。它是
// `core.ErrForbidden` 的**同值别名**：设计 R9 的用例按本包名断言，两个拼写
// `errors.Is` 都成立，不产生第二套拒绝语义。
// ErrForbidden aliases core.ErrForbidden so both spellings match the same error value;
// the design's R9 cases assert the package-local name.
var ErrForbidden = core.ErrForbidden

// errSourceViewUnavailable 表示来源视图没有装配（端口缺一）。零值 `SourceViewer`
// 与缺端口都走这里，**不得**用默认值补齐端口后继续读正文。
// errSourceViewUnavailable reports a missing port; the request is rejected before any
// content is read and the missing port is never replaced by a default.
var errSourceViewUnavailable = errors.New("knowledge: source view is not configured")

// ValidityReason 的取值逐字取自冻结契约 `ValidityView.reason`（`valid=true` 时是
// 空串）。四个原因码与 `compaction.Validity` 同源，**不得**在这里发明第五个值。
// The reason literals are the contract's four-value enumeration; valid reads carry "".
const (
	// ReasonRevoked 表示版本因撤权退出（`retired` 且文档仍在）。
	// ReasonRevoked marks a version retired by revocation.
	ReasonRevoked = "revoked"
	// ReasonDeleted 表示文档或集合已软删。
	// ReasonDeleted marks a soft-deleted document or collection.
	ReasonDeleted = "deleted"
	// ReasonVersionChanged 表示引用点名的版本已不是当前版本（被取代 / 从未生效）。
	// ReasonVersionChanged marks a reference whose version is no longer current.
	ReasonVersionChanged = "version_changed"
	// ReasonNotFound 表示该版本在存储里不存在。
	// ReasonNotFound marks a version that does not exist.
	ReasonNotFound = "not_found"
)

// 转换策略标识：整段原样返回是 `verbatim`，按字符窗口 / 游标分页是 `char_window`
// （契约 `conversion` 字段的示例即"按段落 / 按字符窗口"，没有冻结枚举）。
// Conversion identifiers follow the contract's examples; no enumeration is frozen.
const (
	sourceConversionVerbatim = "verbatim"
	sourceConversionWindow   = "char_window"
)

// EvidenceRange 是版本原文中的字符区间，左闭右开（与契约 `EvidenceRange` 同形）。
// 偏移按**字符**（rune）计，不是字节：中文原文的"第 10 个字符"不会落在半个汉字里。
// EvidenceRange is a rune-offset, half-open window into the version text.
type EvidenceRange struct {
	// Start 是起始字符偏移（含）。
	Start int
	// End 是结束字符偏移（不含）。
	End int
}

// SourceFact 是一条结构化来源事实（契约 `SourceFact`）：键值对，已脱敏，
// **不得**含凭证或完整 URL（脱敏由实现端口的存储层负责）。
// SourceFact is one redacted structured fact about the source.
type SourceFact struct {
	// Key 是事实名。
	Key string
	// Value 是事实值（不得含凭证 / 完整 URL）。
	Value string
}

// SourceSummary 是来源 / 章节摘要（契约 `SourceSummary`）。**只**描述来源本身：
// 任务目标、执行进度与未决项归 C 的 `TaskSummary`（K-02），**不得**混入本类型。
// SourceSummary describes the source only; task goal, progress and open items belong
// to C's TaskSummary and must never be merged into this type (K-02).
type SourceSummary struct {
	// DocumentTitle 是文档标题。
	DocumentTitle string
	// HeadingPath 是该片段所在的标题路径（"A > B > C"）。
	HeadingPath string
	// SectionSummary 是章节摘要，取自来源正文。
	SectionSummary string
}

// ValidityResult 是"这个引用现在还能不能按原样读"的判定（消费方 `Validity`
// 端口的返回形状）。它同时回答版本、删除状态与**当前** ACL 决定：
//   - `Valid=true`：版本仍是当前生效版本，正文可按引用读取；
//   - `Valid=false` + `Reason`：引用已失效，原因取四个冻结字面量之一。
//
// `VersionID` / `ContentHash` 描述**实际检查的那个版本**，不是最新版本。
// ValidityResult answers whether the referenced version can still be read as-is; it
// never describes a newer version.
type ValidityResult struct {
	// Valid 表示该版本仍是当前生效版本。
	Valid bool
	// Reason 是失效原因码；有效时为空串。
	Reason string
	// VersionID 是实际检查的版本。
	VersionID string
	// ContentHash 是该版本内容的哈希；不返回正文时为空串。
	ContentHash string
	// DeletedAt 是软删时刻；仅 `ReasonDeleted` 时有值。
	DeletedAt time.Time
}

// SourceVersionRecord 是一个版本的**精确读取视图**（`SourceReader` 的返回形状）。
// 端口实现必须按 (document_id, version_id) 取行，**不得**改取最新版本；被删文档 /
// 集合同样要能被读到（由调用方判定，而不是让存储层假装不存在）。
// SourceVersionRecord is the exact-read view of one version. Implementations must load
// by the requested ids only, and must report soft deletion instead of hiding the row.
type SourceVersionRecord struct {
	// DocumentID 是文档标识（必须与请求一致）。
	DocumentID string
	// VersionID 是版本标识（必须与请求一致）。
	VersionID string
	// CollectionID 是所属集合，供当前 ACL 重查。
	CollectionID string
	// SourcePath 是允许根内的相对路径（日志用，**不得**当标题扩散）。
	SourcePath string
	// DocumentTitle 是文档标题（摘要用；来源自身元数据）。
	DocumentTitle string
	// ContentHash 是该版本内容的 sha256。
	ContentHash string
	// Status 是版本状态，取 `VersionStatus` 的六值闭集。
	Status VersionStatus
	// DocumentDeleted 表示文档已软删。
	DocumentDeleted bool
	// CollectionDeleted 表示集合已软删。
	CollectionDeleted bool
	// DeletedAt 是软删时刻；未删除时为零值。
	DeletedAt time.Time
	// Text 是该版本的完整原文；未保留时为空串（于是只回状态，不回正文）。
	Text string
	// Facts 是结构化来源事实（已脱敏）。
	Facts []SourceFact
	// Summary 是来源 / 章节摘要；**不得**含任务进度（K-02）。
	Summary SourceSummary
}

// SourceReader 是来源视图的持久化窄端口（实现在 `storage/postgres`）。它只做
// "按引用点名取行"这一件事：不判权限、不判有效性——ACL 由 `Authz` 重查，有效性
// 由本文件的判定表决定。
// SourceReader loads exactly the named version; permission and validity are decided
// above, never inside the store.
type SourceReader interface {
	// LoadVersion 按 (document_id, version_id) 精确读取；不存在返回 `ErrNotFound`。
	LoadVersion(ctx context.Context, documentID, versionID string) (SourceVersionRecord, error)
}

// SourceViewRequest 是来源视图的请求（契约 `SourceViewRequest` 的领域形状）。
// 身份**不**藏在请求体里：Principal 是 `SourceView` 的显式参数。
// SourceViewRequest mirrors the contract; the principal is an explicit argument of
// SourceView and never a field here.
type SourceViewRequest struct {
	// Ref 是资料引用（文档 / 资料 ID + 版本）；两个标识都必填。
	Ref EvidenceRef
	// Range 是要读取的字符区间；nil 表示整段原文。
	Range *EvidenceRange
	// Budget 是单次返回的 Token 上限；<=0 表示不裁剪。
	Budget int
	// Cursor 是上一页返回的 `NextCursor`，原样回传即可续读。
	Cursor string
	// IncludeFacts 要求返回结构化来源事实。
	IncludeFacts bool
	// IncludeSummary 要求返回来源 / 章节摘要。
	IncludeSummary bool
}

// SourceViewResult 是来源视图的结果（契约 `SourceViewResult` 的领域形状）。
// `Superseded` **不**在契约里：它是 Go 侧的"当时版本"标注，HTTP 投影（T042）
// **不得**把它投影成新字段（裁决见文件头）。
// SourceViewResult mirrors the contract except for Superseded, which is an internal
// "that version, not this one" marker and is never projected as a new response field.
type SourceViewResult struct {
	// Ref 原样回显请求的引用。
	Ref EvidenceRef
	// Validity 是本次按当前 ACL / 删除 / 版本重查的结论。
	Validity ValidityResult
	// VersionID 是实际读取的版本（恒等于请求的版本，**不会**换成最新版本）。
	VersionID string
	// ContentHash 是实际返回内容的 sha256；不返回正文时为空串。
	ContentHash string
	// Text 是原文窗口；`Validity.Valid=false` 且非 `Superseded` 时为空串。
	Text string
	// Range 是本次返回内容对应的字符区间。
	Range EvidenceRange
	// Truncated 表示本次返回被预算截断（**不得**让调用方以为是全文）。
	Truncated bool
	// NextCursor 是续读游标；无下一页时为空串。
	NextCursor string
	// Conversion 是转换策略标识（`verbatim` / `char_window`）。
	Conversion string
	// ParentRefs 是父引用（窗口读取时为文档级引用）；整段读取时为空。
	ParentRefs []EvidenceRef
	// Facts 仅在 `IncludeFacts=true` 时返回。
	Facts []SourceFact
	// Summary 仅在 `IncludeSummary=true` 时返回；不含任务进度。
	Summary SourceSummary
	// Superseded 表示引用点名的版本已被取代：返回的是**当时版本**的原文。
	Superseded bool
}

// SourceViewerDeps 是来源视图的装配依赖。`Reader` / `Authz` 缺一即视为装配错误
// （`SourceView` 在读取正文之前返回错误）；`Logger` / `Now` 留空时用包内默认值。
// SourceViewerDeps carries the assembly dependencies; a missing port fails closed.
type SourceViewerDeps struct {
	// Reader 是版本精确读取端口。
	Reader SourceReader
	// Authz 是当前可见范围解析端口（每次调用都重查）。
	Authz Authz
	// Logger 是运行日志目标；缺省用 `slog.Default()`。
	Logger *slog.Logger
	// Now 是时钟；缺省 `time.Now`，测试注入固定时刻。
	Now func() time.Time
}

// SourceViewer 提供精确读取：无状态、同步调用，**不**缓存 ACL 或有效性结论——
// 每次请求都重查（C-26）。
// SourceViewer performs exact reads; it caches neither the ACL nor the validity
// verdict, because both are re-checked on every call.
type SourceViewer struct {
	reader     SourceReader
	authz      Authz
	operations *operationAudit
	logger     *slog.Logger
	now        func() time.Time
}

// NewSourceViewer 装配来源视图。端口缺一不在构造期失败（签名无 error），由入口
// 在读取正文之前返回 `errSourceViewUnavailable`。
// NewSourceViewer assembles the viewer; a missing port fails closed at entry.
func NewSourceViewer(deps SourceViewerDeps) *SourceViewer {
	viewer := &SourceViewer{
		reader: deps.Reader,
		authz:  deps.Authz,
		logger: deps.Logger,
		now:    deps.Now,
	}
	if viewer.logger == nil {
		viewer.logger = slog.Default()
	}
	if viewer.now == nil {
		viewer.now = time.Now
	}
	viewer.operations = newOperationAudit(viewer.logger, viewer.now)
	return viewer
}

// SourceView 精确读取一个版本：身份是**显式必填参数**，零值在读取任何正文之前
// 拒绝；每次按该 Principal 重查当前 ACL / 删除 / 版本（C-26）——两个调用方读同一
// `EvidenceRef` 可以有不同结果。
//
// 返回策略：
//   - 版本不存在 / 已删除 / 已撤权 / 已不是当前版本 → `Validity` 说明原因，正文为空
//     （被取代但旧正文仍保留时例外：回**旧版本**原文并置 `Superseded=true`）；
//   - ACL 拒绝 → `ErrForbidden`，不返回标题、ID、片段或摘要；
//   - 范围 / 游标非法 → `ErrInvalidRequest`（400 级），**不**夹到边界。
//
// SourceView performs the exact read; the principal is an explicit argument and every
// call re-checks the current ACL, deletion state and version.
func (v *SourceViewer) SourceView(ctx context.Context, p core.Principal, req SourceViewRequest) (SourceViewResult, error) {
	trace, err := v.openTrace(ctx, p)
	if err != nil {
		return SourceViewResult{}, err
	}
	if err := v.authorize(p); err != nil {
		trace.reject(ctx, err)
		return SourceViewResult{}, err
	}
	ref := req.Ref
	if !ref.Valid() {
		err := fmt.Errorf("%w: 来源引用缺少文档或版本标识", ErrInvalidRequest)
		trace.reject(ctx, err)
		return SourceViewResult{}, err
	}
	trace.scope(operationTargets{documentID: ref.ID, versionID: ref.VersionID})

	record, err := v.load(ctx, ref)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 版本不存在是契约里的正常读取结论（200 + `not_found`），不是错误。
			trace.succeedRead(ctx)
			return SourceViewResult{Ref: ref, Validity: ValidityResult{Reason: ReasonNotFound}}, nil
		}
		trace.fail(ctx, auditRef{}, "", "", err, nil)
		return SourceViewResult{}, err
	}
	// 当前 ACL 重查必须在返回任何正文之前；被拒时日志只带调用方自带的标识，
	// 集合 / 路径这些"回读才发现的信息"此时**不得**进日志（C-08）。
	if err := v.visible(ctx, p, record.CollectionID); err != nil {
		trace.reject(ctx, err)
		return SourceViewResult{}, err
	}
	trace.scope(operationTargets{collectionID: record.CollectionID, sourcePath: record.SourcePath})

	validity, superseded, serveText, err := judgeValidity(record)
	if err != nil {
		trace.fail(ctx, auditRef{}, "", "", err, nil)
		return SourceViewResult{}, err
	}
	result := SourceViewResult{
		Ref:        ref,
		Validity:   validity,
		VersionID:  record.VersionID,
		Superseded: superseded,
	}
	if !serveText {
		trace.succeedRead(ctx)
		return result, nil
	}
	result.ContentHash = record.ContentHash
	runes := []rune(record.Text)
	start, end, err := resolveWindow(runes, req)
	if err != nil {
		trace.reject(ctx, err)
		return SourceViewResult{}, err
	}
	text, cut, truncated := clipWindow(runes, start, end, req.Budget)
	result.Text = text
	result.Range = EvidenceRange{Start: start, End: cut}
	result.Truncated = truncated
	if truncated {
		result.NextCursor = strconv.Itoa(cut)
	}
	if start == 0 && cut == len(runes) {
		result.Conversion = sourceConversionVerbatim
	} else {
		result.Conversion = sourceConversionWindow
		result.ParentRefs = []EvidenceRef{documentRef(ref, record.ContentHash)}
	}
	if req.IncludeFacts {
		result.Facts = append([]SourceFact(nil), record.Facts...)
	}
	if req.IncludeSummary {
		result.Summary = record.Summary
	}
	trace.succeedRead(ctx)
	return result, nil
}

// load 读版本并校验端口回读的就是请求点名的版本：存储层若"顺手"返回最新版本，
// 这里必须失败而不是让新正文冒充旧引用（C-26 的最后一道闸）。
// load rejects a store response that names a different version than requested; the
// store must never substitute the latest version.
func (v *SourceViewer) load(ctx context.Context, ref EvidenceRef) (SourceVersionRecord, error) {
	record, err := v.reader.LoadVersion(ctx, ref.ID, ref.VersionID)
	if err != nil {
		return SourceVersionRecord{}, err
	}
	if record.DocumentID != ref.ID || record.VersionID != ref.VersionID {
		return SourceVersionRecord{}, fmt.Errorf("knowledge: 来源读取返回了不同的版本（请求 %s/%s，回读 %s/%s）",
			ref.ID, ref.VersionID, record.DocumentID, record.VersionID)
	}
	return record, nil
}

// judgeValidity 是版本状态的判定表：返回有效性、是否"当时版本"标注与是否返回
// 正文三件事。未知状态是存储违约，**不得**当成"有效"或"不存在"糊过去。
// judgeValidity maps the version status onto validity, the superseded marker and
// whether the body may be served; an unknown status is a store contract violation.
func judgeValidity(record SourceVersionRecord) (ValidityResult, bool, bool, error) {
	validity := ValidityResult{VersionID: record.VersionID, ContentHash: record.ContentHash}
	switch {
	case record.DocumentDeleted || record.CollectionDeleted:
		validity.Reason = ReasonDeleted
		validity.ContentHash = ""
		validity.DeletedAt = record.DeletedAt
		return validity, false, false, nil
	case record.Status == VersionActive:
		validity.Valid = true
		if record.Text == "" {
			// 引用有效，但该版本没有可逐字回读的正文（快照未保留且重建结果与
			// content_hash 不符）：只回状态，**不得**回哈希、更不得回近似正文。
			validity.ContentHash = ""
			return validity, false, false, nil
		}
		return validity, false, true, nil
	case record.Status == VersionSuperseded:
		// 被取代但仍保留：回**当时版本**的原文并显式标注；正文未保留时只回状态。
		validity.Reason = ReasonVersionChanged
		if record.Text == "" {
			validity.ContentHash = ""
			return validity, true, false, nil
		}
		return validity, true, true, nil
	case record.Status == VersionRetired:
		validity.Reason = ReasonRevoked
		validity.ContentHash = ""
		return validity, false, false, nil
	case record.Status == VersionPending, record.Status == VersionIndexed, record.Status == VersionFailed:
		// 从未生效的版本不能当有效引用读取（pending / indexed 的内容也不得提前外泄）。
		validity.Reason = ReasonVersionChanged
		validity.ContentHash = ""
		return validity, false, false, nil
	default:
		return ValidityResult{}, false, false, fmt.Errorf("knowledge: 版本 %s 的状态 %q 不在闭集内",
			record.VersionID, string(record.Status))
	}
}

// resolveWindow 计算本次读取的字符窗口：范围先定 [start,end)，游标再覆盖起点。
// 越界一律报错——契约要求越界 400，**不得**静默截断到边界。
// resolveWindow computes [start,end); a cursor overrides the start. Out-of-bounds is
// a request error, never a silent clamp.
func resolveWindow(runes []rune, req SourceViewRequest) (int, int, error) {
	total := len(runes)
	start, end := 0, total
	if req.Range != nil {
		start, end = req.Range.Start, req.Range.End
		if start < 0 || end < start || end > total {
			return 0, 0, fmt.Errorf("%w: 范围 [%d,%d) 越界（原文长度 %d 字符）",
				ErrInvalidRequest, start, end, total)
		}
	}
	if cursor := strings.TrimSpace(req.Cursor); cursor != "" {
		offset, err := strconv.Atoi(cursor)
		if err != nil || offset < 0 || offset > total {
			return 0, 0, fmt.Errorf("%w: 游标 %q 无效", ErrInvalidRequest, cursor)
		}
		if offset > end {
			return 0, 0, fmt.Errorf("%w: 游标 %d 越过窗口末端 %d", ErrInvalidRequest, offset, end)
		}
		start = offset
	}
	return start, end, nil
}

// clipWindow 按 Token 预算裁剪窗口：返回从 start 起的最长前缀、实际结束偏移与
// 是否被截断。预算计入的规则与 `estimateHitTokens` 同源（CJK 类 1 token/字，其余
// 4 字节/token 向上取整）；预算连一个字符都放不下时仍返回**一个**字符，保证分页
// 一定前进（否则调用方会在同一个游标上死循环）。
// clipWindow returns the longest affordable prefix, the cut offset and the truncation
// flag. It always makes progress: at least one rune is returned when the window is
// non-empty, so a paginating caller can never livelock on one cursor.
func clipWindow(runes []rune, start, end, budget int) (string, int, bool) {
	if budget <= 0 || start >= end {
		return string(runes[start:end]), end, false
	}
	wide, narrow := 0, 0
	cut := start
	for i := start; i < end; i++ {
		if isWideRune(runes[i]) {
			wide++
		} else {
			narrow += utf8.RuneLen(runes[i])
		}
		if wide+(narrow+3)/4 > budget && i > start {
			break
		}
		cut = i + 1
	}
	return string(runes[start:cut]), cut, cut < end
}

// documentRef 构造文档级引用（窗口读取的父引用）：同一个文档与版本，内容哈希
// 取实际读到的那个版本。
// documentRef builds the document-level parent reference for a windowed read.
func documentRef(ref EvidenceRef, contentHash string) EvidenceRef {
	return EvidenceRef{
		Kind:        ref.Kind,
		ID:          ref.ID,
		VersionID:   ref.VersionID,
		ContentHash: contentHash,
	}
}

// openTrace 统一准入顺序：装配 → 身份 → 审计轨迹（与 `Ingestor` / `Retriever` 同构）。
// 零值身份在 `begin` 里就被拒绝，因此本方法返回错误时没有任何正文被读取。
// openTrace orders admission as assembly, identity, then the audit trace; a zero
// principal is rejected before any content is read.
func (v *SourceViewer) openTrace(ctx context.Context, p core.Principal) (*operationTrace, error) {
	started := v.startedAt()
	if err := v.ready(); err != nil {
		v.rejectAdmission(ctx, p, err)
		return nil, err
	}
	trace, err := v.operations.begin(p, opSourceView, started)
	if err != nil {
		v.rejectAdmission(ctx, p, err)
		return nil, err
	}
	return trace, nil
}

// rejectAdmission 记录没有轨迹的准入拒绝；零值 `SourceViewer` 没有日志面。
// rejectAdmission logs a rejection that never reached a trace.
func (v *SourceViewer) rejectAdmission(ctx context.Context, p core.Principal, cause error) {
	if v == nil {
		return
	}
	v.operations.rejected(ctx, opSourceView, p, cause, operationTargets{})
}

// startedAt 取本次操作的起点；零值 `SourceViewer` 回落墙上时钟。
// startedAt reads the operation start; the wall clock keeps durations meaningful.
func (v *SourceViewer) startedAt() time.Time {
	if v == nil || v.now == nil {
		return time.Now()
	}
	return v.now()
}

// ready 判定装配是否可用：端口缺一即拒绝，**不得**用默认值补齐。
// ready fails closed on a missing port.
func (v *SourceViewer) ready() error {
	if v == nil || v.reader == nil || v.authz == nil {
		return errSourceViewUnavailable
	}
	return nil
}

// authorize 是读取路径的身份校验：零值 / 无读 scope 的身份在任何读取之前拒绝。
// authorize rejects a zero-value or read-scope-less principal before any read.
func (v *SourceViewer) authorize(p core.Principal) error {
	if !p.Valid() {
		return core.ErrUnauthorized
	}
	if !p.HasScope(core.ScopeKnowledgeRead) {
		return ErrForbidden
	}
	return nil
}

// visible 按**当前** ACL 重查集合可见性（C-26）：实现只回计数与可见集合，被拒
// 集合的标识不出端口；这里只关心目标集合在不在可见集合里。
// visible re-checks the collection against the current ACL; only membership matters.
func (v *SourceViewer) visible(ctx context.Context, p core.Principal, collectionID string) error {
	scope, err := v.authz.Resolve(ctx, p, "", []string{collectionID})
	if err != nil {
		return err
	}
	if !slices.Contains(scope.Collections, collectionID) {
		return ErrForbidden
	}
	return nil
}
