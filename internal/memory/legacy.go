package memory

// 本文件是**旧记忆载体的一次性只读导入**（D-18 / D-22 / 颗粒度文档 §3.1.4）：把旧
// markdown / SQLite 兼容档里的记忆逐条搬进 PG 权威库，源载体只读、不搬迁、不删除、
// 不双读。三条纪律：
//   - ① **逐条对账**：导入 / 跳过 / 冲突 / 不可读四类互斥，一条也不静默丢弃——
//     报告里查得到的才叫"处理过"。
//   - ② **重复导入幂等**：幂等键 =（分层, 分区, 规范化正文），同一条再来一次只会记
//     skipped；权威库里那条已被删 / 被纠正 / 被替代时记 conflict，**不覆盖**当前状态
//     （谁对由人工裁决）。
//   - ③ **来源不可伪造**：导入行经 `WithSource` 盖 `legacy_import`，普通写入入口没有
//     来源入参（见 semantic.go 的 `MemoryEntry.source`）。
// Import is one-shot and read-only towards the legacy carrier: every entry lands in one
// of four mutually exclusive buckets, re-running is idempotent on (scope, section,
// content), and a live-state mismatch is reported as a conflict instead of overwriting.
// Concurrent imports are not supported: this runs as an offline operator step.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// errLegacySourceMissing 是装配缺失：没有源就没有可导入的东西，宁可报错也不返回
// 一份"零条导入"的假通过报告。
// A missing source is an assembly error, never an empty success.
var errLegacySourceMissing = errors.New("memory: legacy source is required")

// LegacyEntry 是旧载体里的一条记忆条目（只读快照）。
// LegacyEntry is one entry read from a legacy carrier.
type LegacyEntry struct {
	// Scope 是条目所属分层（零值 = 全局层）。
	Scope core.Scope
	// Section 是条目所属分区（core / archive）；旧载体里定位不到分区的条目按不可读上报。
	Section core.Section
	// Content 是条目正文；空串（规范化后）按不可读上报，不写库。
	Content string
	// Unreadable 非空表示这条在旧载体里**读不出来**（编码损坏、结构不完整等）：
	// `ImportLegacy` 只把它计入报告、不写库——这正是 D-18 的"不可读条目显式报告、
	// 不静默丢弃"。
	// A non-empty Unreadable marks a source entry that could not be decoded.
	Unreadable string
}

// LegacySource 是旧载体的只读导入源（markdown / SQLite 兼容档）：实现**不得**反向写
// 旧载体（D-22 不搬迁 / 不删除 / 不双读）。
// LegacySource only reads the old carrier; implementations must never write back to it.
type LegacySource interface {
	// Read 返回旧载体里的全部条目（含读不出来的那些，以 `Unreadable` 标注）。
	Read(ctx context.Context) ([]LegacyEntry, error)
}

// ImportedEntry 是一条**已对账**的条目：导入成功记新主键，重复导入跳过记既有主键。
// ImportedEntry is one reconciled entry: a fresh id when imported, the existing id when
// skipped as a duplicate.
type ImportedEntry struct {
	// EntryID 是权威库里的条目主键。
	EntryID int64
	// Scope / Section / Content 回显条目内容（Content 为规范化后的正文）。
	Scope   core.Scope
	Section core.Section
	Content string
}

// ImportConflict 是"旧载体有、权威库也有，但权威库那条不在生效态"的冲突：旧数据说
// 它该存在，当前状态说它已被删除 / 纠正 / 替代。`ImportLegacy` 不写、不覆盖，
// 由人工裁决（D-18 的"冲突显式报告"）。
// ImportConflict records a legacy entry whose live row was retired or deleted; the
// import neither writes nor overwrites it.
type ImportConflict struct {
	// EntryID 是权威库里那条既有条目的主键。
	EntryID int64
	// Scope / Section / Content 回显旧条目内容。
	Scope   core.Scope
	Section core.Section
	Content string
	// State 是权威库里的当前状态（deleted / superseded / corrected / conflicted）。
	State string
}

// UnreadableEntry 是一条没能导入的旧条目及其原因（源标注的或导入侧判定的）。
// UnreadableEntry is one skipped-for-good entry with the reason it could not be read.
type UnreadableEntry struct {
	// Scope / Section / Content 原样回显（内容可能为空——读不出来时本就可能缺）。
	Scope   core.Scope
	Section core.Section
	Content string
	// Reason 是显式原因，不得为空。
	Reason string
}

// ImportReport 是导入的**逐条对账**（D-18）：四类互斥，条数加总恒等于源返回的条数。
// ImportReport is the per-entry reconciliation; the four buckets partition the source.
type ImportReport struct {
	// Imported 是本次真正写入的条目。
	Imported []ImportedEntry
	// Skipped 是权威库里已有同一条（生效态）而跳过的条目（重复导入幂等）。
	Skipped []ImportedEntry
	// Conflicts 是权威库里已有同一条但已不在生效态的条目（显式报告，不覆盖）。
	Conflicts []ImportConflict
	// Unreadable 是源里读不出来或内容 / 分区不合法而没写的条目。
	Unreadable []UnreadableEntry
}

// Total 返回对账总条数（四类之和），供调用方与源条数核对。
// Total is the number of source entries the report accounts for.
func (r ImportReport) Total() int {
	return len(r.Imported) + len(r.Skipped) + len(r.Conflicts) + len(r.Unreadable)
}

// ImportLegacy 把旧载体条目一次性导入权威库（D-18 / D-22）。
//
// 幂等键 =（分层, 分区, 规范化正文）：命中生效态（active / candidate）的既有条目记
// skipped 并回显它的主键；命中非生效态（deleted / superseded / corrected / conflicted）
// 记 conflict 且**不写**——旧数据与当前裁决谁对不是一次导入能替人决定的。
//
// 导入行写 `state=active`、`source=legacy_import`：旧载体里的条目本来就是生效的，
// 降级成 candidate 会让它在确认前从注入面消失。中途写库失败即返回已完成的报告与
// 错误（导入不完整不得切换）；重跑按幂等键只补剩余条目。
// Imported rows are active, stamped legacy_import; a storage failure aborts with the
// partial report, and a retry only writes what is still missing.
func ImportLegacy(ctx context.Context, src LegacySource, store *SemanticStore) (ImportReport, error) {
	var report ImportReport
	if src == nil {
		return report, errLegacySourceMissing
	}
	if store == nil {
		return report, errSemanticNotAssembled
	}
	if err := store.ready(); err != nil {
		return report, err
	}

	entries, err := src.Read(ctx)
	if err != nil {
		return report, fmt.Errorf("memory: 读取旧载体失败: %w", err)
	}

	for _, legacy := range entries {
		if legacy.Unreadable != "" {
			report.Unreadable = append(report.Unreadable, UnreadableEntry{
				Scope:   legacy.Scope,
				Section: legacy.Section,
				Content: legacy.Content,
				Reason:  legacy.Unreadable,
			})
			continue
		}

		content := strings.TrimSpace(legacy.Content)
		if content == "" {
			report.Unreadable = append(report.Unreadable, UnreadableEntry{
				Scope:   legacy.Scope,
				Section: legacy.Section,
				Content: legacy.Content,
				Reason:  "正文为空",
			})
			continue
		}
		if legacy.Section != core.SectionCore && legacy.Section != core.SectionArchive {
			report.Unreadable = append(report.Unreadable, UnreadableEntry{
				Scope:   legacy.Scope,
				Section: legacy.Section,
				Content: content,
				Reason:  fmt.Sprintf("分区取值域外：%q", legacy.Section),
			})
			continue
		}

		existing, found, err := store.deps.Store.FindEntryByContent(ctx, legacy.Scope, legacy.Section, content)
		if err != nil {
			return report, err
		}
		if found {
			switch existing.State {
			case StateActive, StateCandidate:
				report.Skipped = append(report.Skipped, ImportedEntry{
					EntryID: existing.ID,
					Scope:   legacy.Scope,
					Section: legacy.Section,
					Content: content,
				})
			default:
				report.Conflicts = append(report.Conflicts, ImportConflict{
					EntryID: existing.ID,
					Scope:   legacy.Scope,
					Section: legacy.Section,
					Content: content,
					State:   existing.State,
				})
			}
			continue
		}

		id, err := store.deps.Store.InsertEntry(ctx, WithSource(MemoryEntry{
			Scope:   legacy.Scope,
			Section: legacy.Section,
			Content: content,
			State:   StateActive,
		}, SourceLegacyImport))
		if err != nil {
			return report, fmt.Errorf("memory: 导入旧记忆条目失败: %w", err)
		}
		report.Imported = append(report.Imported, ImportedEntry{
			EntryID: id,
			Scope:   legacy.Scope,
			Section: legacy.Section,
			Content: content,
		})
	}
	return report, nil
}
