//go:build integration

package app

// 本文件是 `internal/app/memory_admin.go` 与 `baseline.go` 记忆查证段的验收面
// （US-16 裁决 A / 颗粒度文档 C-21）：
//
//	P1 `TestMemoryAdmin_ProposeWritesCandidateWithAudit`——提议落候选；审计留痕先写 pending 再回填
//	P2 `TestMemoryAdmin_ProposeMergedReceiptReportsExistingRow`——同源去重不新增行、回执如实报状态
//	P3 `TestMemoryAdmin_ProposeInvalidInputLeavesNoOperation`——入参错误零操作行、零写入
//	P4 `TestMemoryAdmin_ProposePrewriteFailureRefusesWrite`——预写失败即拒绝执行（宪法 V）
//	C1 `TestMemoryAdmin_ConfirmPromotesAndRecordsStateBefore`——唯一晋升路径 + `state_before`
//	C2 `TestMemoryAdmin_ConfirmNonCandidateConflicts`——非候选 409、查不到 404，且都不留假成功
//	R1 `TestMemoryAdmin_CorrectSupersedesActiveTarget`——纠正原子退役旧条、新条接替
//	R2 `TestMemoryAdmin_CorrectOnNonActiveTargetRecordsConflicted`——非有效目标落 conflicted
//	D1 `TestMemoryAdmin_DeleteIsSoftAndIdempotent`——软删保留行、重复删除幂等
//	L1 `TestMemoryAdmin_ListEntriesFiltersByStateAndScope`——读模型六态全集、分层与状态过滤
//	A1 `TestMemoryAdmin_NotAssembledFailsClosed`——未装配五入口全部拒绝
//	V1 `TestVerifyMemoryOperation_ProposeVerdicts`——提议的三态判定（合并是设计使然）
//	V2 `TestVerifyMemoryOperation_ConfirmAndDeleteVerdicts`——确认 / 软删的三态判定
//	V3 `TestVerifyMemoryOperation_CorrectVerdicts`——纠正两条分支的三态判定
//	V4 `TestVerifyMemoryOperation_ForeignShapesAreIndeterminate`——目标类型 / 动作 / detail 读不懂
//
// 判据只看"库里发生了什么"：条目行、`management_operations` 行与 `confirmed_by` 列都由
// 真实实现写入，不使用调用记录替身。**正文不进审计**是本文件逐条核对的安全判据
// （P1 / P2 / R1 各查一次 `request_hash` 与 `detail_json`）。
// Everything below asserts real rows: entry states, joined audit rows and the
// confirmed_by column. No call-recording doubles are used for the write surface.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/storage/postgres/pgtest"
)

// memoryAdminEnv 是管理面记忆的夹具：隔离 schema 上的真库 + 真仓储 + 固定时钟。
// 句柄与 schema 都随 `t.Cleanup` 回收，用例之间互不可见。
// The fixture wires the real stores over an isolated schema with a fixed clock.
type memoryAdminEnv struct {
	db     *postgres.DB
	store  *postgres.MemoryStore
	ops    *postgres.ManagementStore
	admin  *MemoryAdmin
	logger *slog.Logger
	logs   *bytes.Buffer
}

// newMemoryAdminEnv 组装夹具：缺一不可的只有存储与操作仓储，logger 只用于断言错误路径。
// newMemoryAdminEnv builds the fixture; the captured log backs the failure-path assertions.
func newMemoryAdminEnv(t *testing.T) *memoryAdminEnv {
	t.Helper()
	db := pgtest.NewTestDB(t)
	ops := postgres.NewManagementStore(db)
	store := postgres.NewMemoryStore(db, baselineClock)
	logger, logs := newCapturedLogger()
	return &memoryAdminEnv{
		db:     db,
		store:  store,
		ops:    ops,
		logger: logger,
		logs:   logs,
		admin: NewMemoryAdmin(MemoryAdminDeps{
			Store:      store,
			Operations: ops,
			Logger:     logger,
			Now:        baselineClock,
		}),
	}
}

// entry 读回一条条目；不存在直接失败（用例里出现的每个主键都应指向真行）。
// entry loads one row; a missing row fails the case outright.
func (env *memoryAdminEnv) entry(t *testing.T, id int64) memory.MemoryEntry {
	t.Helper()
	entry, found, err := env.store.LoadEntry(context.Background(), id)
	require.NoError(t, err)
	require.True(t, found, "条目 %d 必须存在", id)
	return entry
}

// confirmedBy 直接读 `confirmed_by` 列——晋升记录"谁确认的"是回执与审计之外的第三方证据。
// confirmedBy reads the column directly, independently of the receipt and the audit.
func (env *memoryAdminEnv) confirmedBy(t *testing.T, id int64) string {
	t.Helper()
	var value sql.NullString
	require.NoError(t, env.db.QueryRowContext(context.Background(),
		`SELECT confirmed_by FROM memory_entries WHERE id = $1`, id).Scan(&value))
	require.True(t, value.Valid, "条目 %d 的 confirmed_by 必须已写", id)
	return value.String
}

// seedEntry 直接落一条指定状态的条目（corrected / superseded / deleted 只能这样造——
// 生命周期入口不提供"直接写终态"的路径）。
// seedEntry inserts a row in a given state; terminal states have no public write path.
func (env *memoryAdminEnv) seedEntry(t *testing.T, scope core.Scope, section core.Section,
	state, content string,
) int64 {
	t.Helper()
	id, err := env.store.InsertEntry(context.Background(), memory.MemoryEntry{
		Scope:   scope,
		Section: section,
		Content: content,
		State:   state,
	})
	require.NoError(t, err)
	return id
}

// list 走读模型列条目；读面失败直接失败用例。
// list goes through the read model.
func (env *memoryAdminEnv) list(t *testing.T, scope core.Scope, state string) []core.MemoryEntryRecord {
	t.Helper()
	records, err := env.admin.ListEntries(context.Background(),
		core.MemoryEntryFilter{Scope: scope, State: state})
	require.NoError(t, err)
	return records
}

// TestMemoryAdmin_ProposeWritesCandidateWithAudit 判提议的**全部可观察后果**：条目以
// candidate 落归档区、来源是 `explicit_propose`，且审计行先 pending 后回填 success。
// 逆改点：把 Propose 直接写 active → 状态断言红；不写 detail 的 content_sha256 →
// 摘要断言红；把正文塞进 detail_json 或 request_hash → NotContains 断言红。
func TestMemoryAdmin_ProposeWritesCandidateWithAudit(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	actor := consoleActor()
	content := "成本口径以财务系统为准"

	result, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Content: "  " + content + "  "})
	require.NoError(t, err)
	require.NotZero(t, result.EntryID)
	require.Equal(t, memory.StateCandidate, result.State, "新行恒为候选")
	require.False(t, result.Merged, "首次提议没有可合并的行")
	require.Zero(t, result.ReplacedID)

	entry := env.entry(t, result.EntryID)
	require.Equal(t, memory.StateCandidate, entry.State)
	require.Equal(t, core.GlobalScope, entry.Scope)
	require.Equal(t, core.SectionArchive, entry.Section, "空分区按归档区落——候选不进注入面")
	require.Equal(t, content, entry.Content, "正文两端空白必须规范化后落库")
	require.Equal(t, memory.SourceExplicitPropose, entry.Source(), "普通写入的来源由状态推导")

	row := findConsoleOperation(t, env.ops, core.ActionMemoryPropose)
	require.Equal(t, core.TargetKindMemory, row.TargetKind)
	require.Equal(t, "global", row.TargetID, "target_id 回答动的是哪一层")
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Equal(t, "OK", row.ResultCode)
	require.Empty(t, row.Verification, "正常完成路径不写 verification")
	require.False(t, row.FinishedAt.IsZero(), "终态必须回填完成时刻")
	require.Equal(t, actor.Subject, row.ActorSubject)
	require.Equal(t, actor.Method, row.AuthMethod)
	require.Equal(t, actor.Source, row.Source)

	require.NotContains(t, row.RequestHash, content, "request_hash 不得含正文")
	require.NotContains(t, string(row.DetailJSON), content, "审计 detail 不得含正文")

	detail := detailMap(t, row.DetailJSON)
	require.EqualValues(t, result.EntryID, detail["entry_id"])
	require.EqualValues(t, 0, detail["replaced_id"])
	require.Equal(t, "archive", detail["section"])
	require.Equal(t, "", detail["agent"])
	require.Equal(t, memory.StateCandidate, detail["state_after"])
	require.Equal(t, memoryDigestHex(content), detail["content_sha256"],
		"摘要按规范化正文求，删掉正文也能核对改的是哪一条")
}

// TestMemoryAdmin_ProposeMergedReceiptReportsExistingRow 判同源去重的**回执诚实性**：
// 重复转述不新增行，回执给既有主键且 `merged=true`；合并进一条 **active** 条目时回执
// 如实报 active——若一律报 candidate，页面会给已生效的记忆显示"待确认"，点确认只会
// 拿到 409。另一种分区不算同源（去重键含分区），必须新增行。
// 逆改点：去掉合并判断 → 第二次提议 NewID 不同 / 行数 2；把回执状态硬写成 candidate →
// 第三条断言红。
func TestMemoryAdmin_ProposeMergedReceiptReportsExistingRow(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	actor := consoleActor()
	content := "巡检窗口固定在凌晨两点"

	first, err := env.admin.Propose(context.Background(), actor, core.MemoryProposal{Content: content})
	require.NoError(t, err)
	require.False(t, first.Merged)

	second, err := env.admin.Propose(context.Background(), actor, core.MemoryProposal{Content: content})
	require.NoError(t, err)
	require.True(t, second.Merged, "重复转述必须合并到既有候选")
	require.Equal(t, first.EntryID, second.EntryID)
	require.Equal(t, memory.StateCandidate, second.State)
	require.Len(t, env.list(t, core.GlobalScope, ""), 1, "合并不新增行")

	rows := listConsoleOperations(t, env.ops)
	require.Len(t, rows, 2, "两次提议都是成功的写，各自留一行审计")
	for _, row := range rows {
		require.Equal(t, core.ActionMemoryPropose, row.Action)
		require.Equal(t, "OK", row.ResultCode)
		require.EqualValues(t, first.EntryID, detailMap(t, row.DetailJSON)["entry_id"],
			"两次预写都记下了合并目标")
	}

	_, err = env.admin.Confirm(context.Background(), actor, first.EntryID)
	require.NoError(t, err)

	third, err := env.admin.Propose(context.Background(), actor, core.MemoryProposal{Content: content})
	require.NoError(t, err)
	require.True(t, third.Merged, "有效条目同样吸收同源提议")
	require.Equal(t, first.EntryID, third.EntryID)
	require.Equal(t, memory.StateActive, third.State,
		"回执的状态是动作后该行的真实状态，不得伪造成候选")
	require.Equal(t, memory.StateActive, env.entry(t, first.EntryID).State, "合并不得把有效行降级")

	// 去重键含分区：同正文的核心区提议是另一条记忆。
	other, err := env.admin.Propose(context.Background(), actor, core.MemoryProposal{
		Section: core.SectionCore, Content: content,
	})
	require.NoError(t, err)
	require.False(t, other.Merged, "分区不同不是同源")
	require.NotEqual(t, first.EntryID, other.EntryID)
	require.Equal(t, core.SectionCore, env.entry(t, other.EntryID).Section)
}

// TestMemoryAdmin_ProposeInvalidInputLeavesNoOperation 判纯请求校验发生在**预写之前**：
// 被拒绝的提议不留操作行、不落条目。空正文 / 未知分区 / 非法 Agent 名各判一次。
// 逆改点：把 BeginOperation 挪到校验之前 → 操作行计数非零；校验只判空串不判闭集 →
// 未知分区分支不再可判。
func TestMemoryAdmin_ProposeInvalidInputLeavesNoOperation(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)

	cases := []struct {
		name     string
		proposal core.MemoryProposal
		want     string
	}{
		{"空正文", core.MemoryProposal{Content: "   "}, "提议正文不得为空"},
		{"未知分区", core.MemoryProposal{Section: "dispatch", Content: "任意"}, "未知的记忆分区"},
		{"路径分隔符", core.MemoryProposal{
			Scope: core.Scope{Agent: "a/b"}, Content: "任意",
		}, "目录名不得含路径分隔符"},
		// 点前缀先于分隔符判：`../escape` 命中的是点前缀规则，两条规则各判一次，
		// 断言不合并——否则改一条规则会让另一条静默失去用例。
		{"点前缀", core.MemoryProposal{
			Scope: core.Scope{Agent: "../escape"}, Content: "任意",
		}, "目录名不得以点开头"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.admin.Propose(context.Background(), consoleActor(), tc.proposal)
			require.Error(t, err)
			require.True(t, core.IsInvalidInput(err), "入参错误必须有可判类型，web 据此 400")
			require.Contains(t, err.Error(), tc.want)
		})
	}

	require.Zero(t, countOperations(t, env.ops, core.ActionMemoryPropose), "被拒绝的请求不得留操作行")
	require.Empty(t, env.list(t, core.GlobalScope, ""), "被拒绝的请求不得落条目")
}

// TestMemoryAdmin_ProposePrewriteFailureRefusesWrite 判宪法第 5 条的 fail-closed：预写
// 失败即拒绝执行——条目一个字节都不写、连 pending 行都不许留下，错误上抛由 web 映射
// 500。逆改点：把 BeginOperation 挪到领域调用之后 → 条目已落库而操作行空，断言红。
func TestMemoryAdmin_ProposePrewriteFailureRefusesWrite(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	admin := NewMemoryAdmin(MemoryAdminDeps{
		Store:      env.store,
		Operations: failingBeginOperations{OperationStore: env.ops, err: errors.New("数据库不可用")},
		Logger:     env.logger,
		Now:        baselineClock,
	})

	_, err := admin.Propose(context.Background(), consoleActor(),
		core.MemoryProposal{Content: "预写失败不得落库"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "拒绝执行")
	require.Empty(t, listConsoleOperations(t, env.ops), "预写失败不得留下任何操作行")
	require.Empty(t, env.list(t, core.GlobalScope, ""), "预写失败不得写入条目")
	require.Contains(t, env.logs.String(), "记忆管理预写操作记录失败",
		"失败路径必须有日志（审计 + 日志双轨）")
}

// TestMemoryAdmin_ConfirmPromotesAndRecordsStateBefore 判唯一晋升路径的全部后果：
// 状态迁到 active、`confirmed_by` 记 actor_subject（不是 token 明文）、审计记下
// `state_before`（恢复查证靠它区分"已生效"与"没生效"）。
// 逆改点：去掉 state_before 预读 → detail 断言红；把 token 明文写进 confirmed_by →
// confirmed_by 断言红。
func TestMemoryAdmin_ConfirmPromotesAndRecordsStateBefore(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	actor := consoleActor()

	proposed, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Content: "每季度复核一次模型价格"})
	require.NoError(t, err)

	result, err := env.admin.Confirm(context.Background(), actor, proposed.EntryID)
	require.NoError(t, err)
	require.Equal(t, proposed.EntryID, result.EntryID, "确认不产生新行")
	require.Equal(t, memory.StateActive, result.State)
	require.Zero(t, result.ReplacedID)

	require.Equal(t, memory.StateActive, env.entry(t, proposed.EntryID).State)
	require.Equal(t, actor.Subject, env.confirmedBy(t, proposed.EntryID),
		"确认人记脱敏主体，不记凭据")

	row := findConsoleOperation(t, env.ops, core.ActionMemoryConfirm)
	require.Equal(t, core.TargetKindMemory, row.TargetKind)
	require.Equal(t, "global", row.TargetID)
	require.Equal(t, core.OperationSuccess, row.Status)
	detail := detailMap(t, row.DetailJSON)
	require.EqualValues(t, proposed.EntryID, detail["entry_id"])
	require.Equal(t, memory.StateCandidate, detail["state_before"])
	require.Equal(t, memory.StateActive, detail["state_after"])
	require.NotContains(t, string(row.DetailJSON), "每季度复核一次模型价格", "审计不得含正文")
}

// TestMemoryAdmin_ConfirmNonCandidateConflicts 判两类拒绝都**不谎报成功**：已有效条目
// 再次确认 → `core.ErrConflict` 且该次审计回填 failed / CONFLICT；目标不存在 → 404 分类
// 且**不留操作行**（预读发生在预写之前）。
// 逆改点：确认前不判状态、直接迁移 → 第二次确认成功，冲突断言红。
func TestMemoryAdmin_ConfirmNonCandidateConflicts(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	actor := consoleActor()

	proposed, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Content: "发布前必须双人复核"})
	require.NoError(t, err)
	_, err = env.admin.Confirm(context.Background(), actor, proposed.EntryID)
	require.NoError(t, err)

	_, err = env.admin.Confirm(context.Background(), actor, proposed.EntryID)
	require.ErrorIs(t, err, core.ErrConflict, "已有效条目不是候选：状态冲突")

	require.Equal(t, 2, countOperations(t, env.ops, core.ActionMemoryConfirm))
	var failed core.OperationRecord
	for _, row := range listConsoleOperations(t, env.ops) {
		if row.Action == core.ActionMemoryConfirm && row.ResultCode == "CONFLICT" {
			failed = row
		}
	}
	require.Equal(t, core.OperationFailed, failed.Status, "冲突必须落失败终态")
	require.NotEmpty(t, failed.ErrorMessage, "失败原因必须留痕")

	_, err = env.admin.Confirm(context.Background(), actor, 987654)
	require.ErrorIs(t, err, core.ErrMemoryEntryNotFound, "查不到条目是 404 分类")
	require.Equal(t, 2, countOperations(t, env.ops, core.ActionMemoryConfirm),
		"查不到目标不得留操作行（预读先于预写）")
}

// TestMemoryAdmin_CorrectSupersedesActiveTarget 判纠正的原子分支：旧条转 corrected、
// 新条 active 并指向旧条，回执同时给新旧两个主键；审计记 state_before / state_after 与
// 新条正文摘要（不含正文）。
// 逆改点：纠正后不退役旧条 → 旧行状态断言红；把返回状态硬写成 active 不管领域结果 →
// R2 的 conflicted 断言红。
func TestMemoryAdmin_CorrectSupersedesActiveTarget(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	actor := consoleActor()
	oldContent := "备份保留七天"
	newContent := "备份保留三十天"

	proposed, err := env.admin.Propose(context.Background(), actor, core.MemoryProposal{Content: oldContent})
	require.NoError(t, err)
	_, err = env.admin.Confirm(context.Background(), actor, proposed.EntryID)
	require.NoError(t, err)

	result, err := env.admin.Correct(context.Background(), actor,
		core.MemoryCorrection{EntryID: proposed.EntryID, Content: newContent})
	require.NoError(t, err)
	require.Equal(t, memory.StateActive, result.State)
	require.Equal(t, proposed.EntryID, result.ReplacedID)
	require.NotEqual(t, proposed.EntryID, result.EntryID, "纠正产生新行")

	old := env.entry(t, proposed.EntryID)
	require.Equal(t, memory.StateCorrected, old.State, "旧条必须退役")
	require.Equal(t, oldContent, old.Content, "退役不改旧正文")

	next := env.entry(t, result.EntryID)
	require.Equal(t, memory.StateActive, next.State)
	require.Equal(t, newContent, next.Content)
	require.Equal(t, proposed.EntryID, next.SupersedesID, "新条指向前驱")
	require.Equal(t, actor.Subject, env.confirmedBy(t, result.EntryID))

	row := findConsoleOperation(t, env.ops, core.ActionMemoryCorrect)
	require.Equal(t, core.OperationSuccess, row.Status)
	detail := detailMap(t, row.DetailJSON)
	require.EqualValues(t, result.EntryID, detail["entry_id"], "entry_id 是结果行")
	require.EqualValues(t, proposed.EntryID, detail["replaced_id"], "旧条单列")
	require.Equal(t, memory.StateActive, detail["state_before"])
	require.Equal(t, memory.StateActive, detail["state_after"])
	require.Equal(t, memoryDigestHex(newContent), detail["content_sha256"])
	require.NotContains(t, string(row.DetailJSON), newContent, "审计不得含正文")
	require.NotContains(t, row.RequestHash, newContent, "request_hash 不得含正文")
}

// TestMemoryAdmin_CorrectOnNonActiveTargetRecordsConflicted 判纠正的冲突分支：目标已不
// 在有效态时**不替人裁决谁对**——新条落 conflicted 且指向目标，目标原样不动。
// 逆改点：把非有效分支也改成 ReplaceEntry → 返回状态变成 active 且目标被改状态，断言红。
func TestMemoryAdmin_CorrectOnNonActiveTargetRecordsConflicted(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	actor := consoleActor()

	proposed, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Content: "候选阶段的口径"})
	require.NoError(t, err)

	result, err := env.admin.Correct(context.Background(), actor,
		core.MemoryCorrection{EntryID: proposed.EntryID, Content: "与候选矛盾的口径"})
	require.NoError(t, err)
	require.Equal(t, memory.StateConflicted, result.State,
		"目标不是有效条：新条落冲突待裁决")
	require.Equal(t, proposed.EntryID, result.ReplacedID)

	require.Equal(t, memory.StateCandidate, env.entry(t, proposed.EntryID).State,
		"冲突分支不得改目标状态")

	next := env.entry(t, result.EntryID)
	require.Equal(t, memory.StateConflicted, next.State)
	require.Equal(t, proposed.EntryID, next.SupersedesID)
	require.Equal(t, actor.Subject, env.confirmedBy(t, result.EntryID),
		"conflicted 分支同样记录提出纠正的人")

	row := findConsoleOperation(t, env.ops, core.ActionMemoryCorrect)
	detail := detailMap(t, row.DetailJSON)
	require.Equal(t, memory.StateCandidate, detail["state_before"])
	require.Equal(t, memory.StateConflicted, detail["state_after"],
		"审计如实记领域给的后态")
}

// TestMemoryAdmin_DeleteIsSoftAndIdempotent 判软删的两条语义：行与来源保留（审计要留
// 得下来、读模型仍看得见），重复删除幂等成功；查不到的标识按 404 分类且不留操作行。
// 逆改点：改成物理删除 → LoadEntry 断言红；重复删除报冲突 → 幂等断言红。
func TestMemoryAdmin_DeleteIsSoftAndIdempotent(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	actor := consoleActor()

	proposed, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Content: "已过时的口径"})
	require.NoError(t, err)
	_, err = env.admin.Confirm(context.Background(), actor, proposed.EntryID)
	require.NoError(t, err)

	result, err := env.admin.Delete(context.Background(), actor, proposed.EntryID)
	require.NoError(t, err)
	require.Equal(t, proposed.EntryID, result.EntryID)
	require.Equal(t, memory.StateDeleted, result.State)

	entry := env.entry(t, proposed.EntryID)
	require.Equal(t, memory.StateDeleted, entry.State)
	require.Equal(t, memory.SourceExplicitPropose, entry.Source(), "来源随行保留")

	again, err := env.admin.Delete(context.Background(), actor, proposed.EntryID)
	require.NoError(t, err, "重复删除幂等成功")
	require.Equal(t, memory.StateDeleted, again.State)
	require.Equal(t, 2, countOperations(t, env.ops, core.ActionMemoryDelete),
		"两次删除都留痕")

	rows := env.list(t, core.GlobalScope, "")
	require.Len(t, rows, 1, "软删的行仍在读模型里")
	require.Equal(t, memory.StateDeleted, rows[0].State)

	_, err = env.admin.Delete(context.Background(), actor, 987654)
	require.ErrorIs(t, err, core.ErrMemoryEntryNotFound)
	require.Equal(t, 2, countOperations(t, env.ops, core.ActionMemoryDelete),
		"查不到目标不得留操作行")

	// 两次删除各留一行审计：首次 active→deleted，重复 deleted→deleted。审计按**请求**
	// 记账而不是按状态变化——重复删除也是一次管理面写请求，抹掉它等于让一次管理动作
	// 在审计里不可检索；`state_before == state_after` 正是"本次没有状态变化"的诚实录法。
	// 逆改点：把重复删除改成不写审计 → 上面的计数断言红；把重复删除伪造成状态变化 →
	// 下面的 deleted→deleted 断言红。
	var sawFirst, sawRepeat bool
	for _, op := range listConsoleOperations(t, env.ops) {
		if op.Action != core.ActionMemoryDelete {
			continue
		}
		require.Equal(t, core.OperationSuccess, op.Status, "幂等成功不是失败")
		detail := detailMap(t, op.DetailJSON)
		require.Equal(t, memory.StateDeleted, detail["state_after"], "两次都如实记终态")
		switch detail["state_before"] {
		case memory.StateActive:
			sawFirst = true
		case memory.StateDeleted:
			sawRepeat = true
		}
	}
	require.True(t, sawFirst, "首次删除的 state_before 是 active")
	require.True(t, sawRepeat, "重复删除如实记 deleted → deleted")
}

// TestMemoryAdmin_ListEntriesFiltersByStateAndScope 判读模型是**管理面读**：六态全集的
// 行照常返回（候选、已删都在）、按分层隔离、按状态过滤、主键倒序；域外状态取值是入参
// 错误而不是静默空页。
// 逆改点：在读模型里套召回面的 activeOnly → 候选 / 已删行消失，计数断言红；忽略 state
// 过滤 → 过滤断言红。
func TestMemoryAdmin_ListEntriesFiltersByStateAndScope(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	actor := consoleActor()
	alice := core.Scope{Agent: "alice"}

	first, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Content: "全局候选一"})
	require.NoError(t, err)
	positive, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Content: "全局待确认二"})
	require.NoError(t, err)
	_, err = env.admin.Confirm(context.Background(), actor, positive.EntryID)
	require.NoError(t, err)
	deleted, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Content: "全局已删三"})
	require.NoError(t, err)
	_, err = env.admin.Delete(context.Background(), actor, deleted.EntryID)
	require.NoError(t, err)
	agentEntry, err := env.admin.Propose(context.Background(), actor,
		core.MemoryProposal{Scope: alice, Content: "alice 的候选"})
	require.NoError(t, err)

	all := env.list(t, core.GlobalScope, "")
	require.Len(t, all, 3, "全局层三条都在（候选、有效、已删）")
	require.Greater(t, all[0].EntryID, all[1].EntryID, "主键倒序")
	require.Greater(t, all[1].EntryID, all[2].EntryID)
	for _, record := range all {
		require.Equal(t, core.GlobalScope, record.Scope)
		require.NotZero(t, record.CreatedAt, "读取时刻必须回传")
	}
	require.Equal(t, memory.SourceExplicitPropose, all[2].Source, "来源随行返回")

	candidates := env.list(t, core.GlobalScope, memory.StateCandidate)
	require.Len(t, candidates, 1)
	require.Equal(t, first.EntryID, candidates[0].EntryID)

	actives := env.list(t, core.GlobalScope, memory.StateActive)
	require.Len(t, actives, 1)
	require.Equal(t, positive.EntryID, actives[0].EntryID)
	require.Equal(t, "全局待确认二", actives[0].Content)

	hidden := env.list(t, alice, "")
	require.Len(t, hidden, 1, "分层隔离：全局条目不得混进 Agent 层")
	require.Equal(t, agentEntry.EntryID, hidden[0].EntryID)
	require.Equal(t, alice, hidden[0].Scope)

	_, err = env.admin.ListEntries(context.Background(), core.MemoryEntryFilter{
		Scope: core.GlobalScope, State: "expired",
	})
	require.Error(t, err)
	require.True(t, core.IsInvalidInput(err), "域外状态是入参错误，不是空页")
	require.Contains(t, err.Error(), "expired")
}

// TestMemoryAdmin_NotAssembledFailsClosed 判装配失败不静默：缺存储端口时五个入口全部
// 拒绝，错误可判且不含"没有条目"这类伪造结果，零操作行。
// 逆改点：未装配时返回空列表 / 零回执 → 任一断言红。
func TestMemoryAdmin_NotAssembledFailsClosed(t *testing.T) {
	t.Parallel()
	env := newMemoryAdminEnv(t)
	admin := NewMemoryAdmin(MemoryAdminDeps{Operations: env.ops, Logger: env.logger, Now: baselineClock})

	ctx := context.Background()
	actor := consoleActor()

	_, listErr := admin.ListEntries(ctx, core.MemoryEntryFilter{Scope: core.GlobalScope})
	_, proposeErr := admin.Propose(ctx, actor, core.MemoryProposal{Content: "任意"})
	_, confirmErr := admin.Confirm(ctx, actor, 1)
	_, correctErr := admin.Correct(ctx, actor, core.MemoryCorrection{EntryID: 1, Content: "任意"})
	_, deleteErr := admin.Delete(ctx, actor, 1)

	for name, err := range map[string]error{
		"ListEntries": listErr, "Propose": proposeErr, "Confirm": confirmErr,
		"Correct": correctErr, "Delete": deleteErr,
	} {
		require.Error(t, err, "%s：未装配必须拒绝", name)
		require.Contains(t, err.Error(), "未装配", "%s：错误必须点名装配缺失", name)
		require.NotErrorIs(t, err, core.ErrMemoryEntryNotFound, "%s：不得伪造成查不到条目", name)
	}
	require.Empty(t, listConsoleOperations(t, env.ops), "未装配不得留操作行")
}

// memoryVerifyEnv 是恢复查证用例的夹具：真库 + 只有 Memory 一项的 `RecoveryDeps`。
// The fixture wires RecoveryDeps with only the memory port populated.
type memoryVerifyEnv struct {
	env  *memoryAdminEnv
	deps RecoveryDeps
}

// newMemoryVerifyEnv 组装查证夹具。
func newMemoryVerifyEnv(t *testing.T) *memoryVerifyEnv {
	t.Helper()
	env := newMemoryAdminEnv(t)
	return &memoryVerifyEnv{env: env, deps: RecoveryDeps{Memory: env.store}}
}

// verdict 走**生产的分派入口**（`verifyOperation`）取结论——不直接调记忆分支，保证
// 动作 → 查证方式的映射本身也被判。
// verdict goes through the production dispatcher so the action routing is judged too.
func (v *memoryVerifyEnv) verdict(t *testing.T, op core.OperationRecord) string {
	t.Helper()
	return verifyOperation(context.Background(), v.deps, op)
}

// memoryVerifyRecord 组装一条查证输入：detail 由调用方逐键给出，缺字段就真的缺。
// memoryVerifyRecord builds one verification input; absent keys stay absent.
func memoryVerifyRecord(t *testing.T, action, targetKind string, detail map[string]any) core.OperationRecord {
	t.Helper()
	raw, err := json.Marshal(detail)
	require.NoError(t, err)
	return core.OperationRecord{
		OperationID: "op-memory-verify",
		Action:      action,
		TargetKind:  targetKind,
		DetailJSON:  raw,
	}
}

// TestVerifyMemoryOperation_ProposeVerdicts 判提议的恢复三态：预写时还不知道主键，
// 只有"同源同摘要的行一条都没有"才可证明未生效；行在但**不能证明是本操作写的**
// （提议本来就会合并）一律 indeterminate。有主键时按主键 + 摘要认领。
// 逆改点：把"找到同源行"判成 applied → 第二条断言红（会把别人的行算成本操作成果）；
// 把缺 agent 读成全局层 → 缺字段断言红。
func TestVerifyMemoryOperation_ProposeVerdicts(t *testing.T) {
	t.Parallel()
	v := newMemoryVerifyEnv(t)
	env := v.env
	content := "甲口径"

	candidateID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateCandidate, content)
	activeID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateActive, "乙口径")
	deletedID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateDeleted, "丙口径")

	cases := []struct {
		name   string
		detail map[string]any
		want   string
	}{
		{
			name: "按主键认领候选行",
			detail: map[string]any{
				"entry_id": candidateID, "agent": "", "section": "archive",
				"content_sha256": memoryDigestHex(content),
			},
			want: core.VerificationApplied,
		},
		{
			name: "按主键认领有效行",
			detail: map[string]any{
				"entry_id": activeID, "agent": "", "section": "archive",
				"content_sha256": memoryDigestHex("乙口径"),
			},
			want: core.VerificationApplied,
		},
		{
			name: "主键命中但摘要不符",
			detail: map[string]any{
				"entry_id": candidateID, "agent": "", "section": "archive",
				"content_sha256": memoryDigestHex("别的正文"),
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "主键命中的行已删除",
			detail: map[string]any{
				"entry_id": deletedID, "agent": "", "section": "archive",
				"content_sha256": memoryDigestHex("丙口径"),
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "主键不存在",
			detail: map[string]any{
				"entry_id": 987654, "agent": "", "section": "archive",
				"content_sha256": memoryDigestHex("任意"),
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "无主键且无同源行可证明未生效",
			detail: map[string]any{
				"agent": "", "section": "archive",
				"content_sha256": memoryDigestHex("从未写入过的正文"),
			},
			want: core.VerificationNotApplied,
		},
		{
			name: "无主键但同源行已存在",
			detail: map[string]any{
				"agent": "", "section": "archive",
				"content_sha256": memoryDigestHex(content),
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "detail缺agent",
			detail: map[string]any{
				"section": "archive", "content_sha256": memoryDigestHex("任意"),
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "detail缺section",
			detail: map[string]any{
				"agent": "", "content_sha256": memoryDigestHex("任意"),
			},
			want: core.VerificationIndeterminate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := memoryVerifyRecord(t, core.ActionMemoryPropose, core.TargetKindMemory, tc.detail)
			require.Equal(t, tc.want, v.verdict(t, op))
		})
	}
}

// TestVerifyMemoryOperation_ConfirmAndDeleteVerdicts 判确认与软删的三态：后态已成立 →
// applied；仍停在前态 → not_applied；第三种状态（别人改过）→ indeterminate。
// 逆改点：只判"状态变了"而不判变成哪种 → 第三种状态那条红；把"没变化"判成 applied →
// not_applied 断言红。
func TestVerifyMemoryOperation_ConfirmAndDeleteVerdicts(t *testing.T) {
	t.Parallel()
	v := newMemoryVerifyEnv(t)
	env := v.env

	activeID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateActive, "已生效")
	candidateID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateCandidate, "仍是候选")
	deletedID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateDeleted, "已删除")
	conflictedID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateConflicted, "待裁决")

	cases := []struct {
		name   string
		action string
		detail map[string]any
		want   string
	}{
		{
			name: "确认已生效", action: core.ActionMemoryConfirm,
			detail: map[string]any{
				"entry_id": activeID, "agent": "", "section": "archive",
				"state_before": memory.StateCandidate,
			},
			want: core.VerificationApplied,
		},
		{
			name: "确认仍停在候选", action: core.ActionMemoryConfirm,
			detail: map[string]any{
				"entry_id": candidateID, "agent": "", "section": "archive",
				"state_before": memory.StateCandidate,
			},
			want: core.VerificationNotApplied,
		},
		{
			name: "确认被第三方改成第三种状态", action: core.ActionMemoryConfirm,
			detail: map[string]any{
				"entry_id": deletedID, "agent": "", "section": "archive",
				"state_before": memory.StateCandidate,
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "确认缺state_before", action: core.ActionMemoryConfirm,
			detail: map[string]any{"entry_id": activeID, "agent": "", "section": "archive"},
			want:   core.VerificationIndeterminate,
		},
		{
			name: "软删已删除", action: core.ActionMemoryDelete,
			detail: map[string]any{
				"entry_id": deletedID, "agent": "", "section": "archive",
				"state_before": memory.StateActive,
			},
			want: core.VerificationApplied,
		},
		{
			name: "软删仍是有效", action: core.ActionMemoryDelete,
			detail: map[string]any{
				"entry_id": activeID, "agent": "", "section": "archive",
				"state_before": memory.StateActive,
			},
			want: core.VerificationNotApplied,
		},
		{
			name: "软删遇到第三种状态", action: core.ActionMemoryDelete,
			detail: map[string]any{
				"entry_id": conflictedID, "agent": "", "section": "archive",
				"state_before": memory.StateActive,
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "目标行已消失", action: core.ActionMemoryConfirm,
			detail: map[string]any{
				"entry_id": 987654, "agent": "", "section": "archive",
				"state_before": memory.StateCandidate,
			},
			want: core.VerificationIndeterminate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := memoryVerifyRecord(t, tc.action, core.TargetKindMemory, tc.detail)
			require.Equal(t, tc.want, v.verdict(t, op))
		})
	}
}

// TestVerifyMemoryOperation_CorrectVerdicts 判纠正两条分支的查证：目标原先是 active 时
// 依赖 `ReplaceEntry` 的原子事务（目标已转 corrected 即整笔已提交）；目标原不在有效态
// 时只能按"指向目标 + 正文摘要一致"认领 conflicted 新条。
// 逆改点：两条分支互换 → 各断言红；冲突分支不核对摘要 → "摘要不符"那条被判成 applied。
func TestVerifyMemoryOperation_CorrectVerdicts(t *testing.T) {
	t.Parallel()
	v := newMemoryVerifyEnv(t)
	env := v.env

	correctedID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateCorrected, "被纠正的旧条")
	stillActiveID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateActive, "仍是有效")
	conflictedTargetID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateConflicted, "目标本身冲突")
	candidateTargetID := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateCandidate, "目标仍是候选")

	claimed := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateConflicted, "冲突新条甲")
	_, err := env.db.ExecContext(context.Background(),
		`UPDATE memory_entries SET supersedes_id = $2 WHERE id = $1`, claimed, candidateTargetID)
	require.NoError(t, err)

	unmatched := env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateConflicted, "冲突新条乙")
	_, err = env.db.ExecContext(context.Background(),
		`UPDATE memory_entries SET supersedes_id = $2 WHERE id = $1`, unmatched, candidateTargetID)
	require.NoError(t, err)

	cases := []struct {
		name   string
		detail map[string]any
		want   string
	}{
		{
			name: "有效目标已退役",
			detail: map[string]any{
				"entry_id": correctedID, "agent": "", "section": "archive",
				"state_before": memory.StateActive, "content_sha256": memoryDigestHex("被纠正的旧条"),
			},
			want: core.VerificationApplied,
		},
		{
			name: "有效目标未退役",
			detail: map[string]any{
				"entry_id": stillActiveID, "agent": "", "section": "archive",
				"state_before": memory.StateActive, "content_sha256": memoryDigestHex("仍是有效"),
			},
			want: core.VerificationNotApplied,
		},
		{
			name: "有效目标变成第三种状态",
			detail: map[string]any{
				"entry_id": conflictedTargetID, "agent": "", "section": "archive",
				"state_before": memory.StateActive, "content_sha256": memoryDigestHex("目标本身冲突"),
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "冲突分支按摘要认领新条",
			detail: map[string]any{
				"entry_id": candidateTargetID, "agent": "", "section": "archive",
				"state_before": memory.StateCandidate, "content_sha256": memoryDigestHex("冲突新条甲"),
			},
			want: core.VerificationApplied,
		},
		{
			name: "冲突分支摘要不符",
			detail: map[string]any{
				"entry_id": candidateTargetID, "agent": "", "section": "archive",
				"state_before": memory.StateCandidate, "content_sha256": memoryDigestHex("没有任何新条"),
			},
			want: core.VerificationNotApplied,
		},
		{
			name: "冲突分支缺摘要",
			detail: map[string]any{
				"entry_id": candidateTargetID, "agent": "", "section": "archive",
				"state_before": memory.StateCandidate,
			},
			want: core.VerificationIndeterminate,
		},
		{
			name: "目标行已消失",
			detail: map[string]any{
				"entry_id": 987654, "agent": "", "section": "archive",
				"state_before": memory.StateActive, "content_sha256": memoryDigestHex("任意"),
			},
			want: core.VerificationIndeterminate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := memoryVerifyRecord(t, core.ActionMemoryCorrect, core.TargetKindMemory, tc.detail)
			require.Equal(t, tc.want, v.verdict(t, op))
		})
	}
}

// TestVerifyMemoryOperation_ForeignShapesAreIndeterminate 判查证的拒绝面：目标类型不是
// memory、动作不认识、detail_json 空 / 不是 JSON、缺 Memory 端口——一律 indeterminate，
// **不**把"读不懂"当成"没生效"（后者会把一次可能已提交的写改判 failed）。
// 逆改点：把解码失败吞成 not_applied → 相应断言红。
func TestVerifyMemoryOperation_ForeignShapesAreIndeterminate(t *testing.T) {
	t.Parallel()
	v := newMemoryVerifyEnv(t)
	entryID := v.env.seedEntry(t, core.GlobalScope, core.SectionArchive, memory.StateCandidate, "候选")
	detail := map[string]any{
		"entry_id": entryID, "agent": "", "section": "archive",
		"content_sha256": memoryDigestHex("候选"),
	}

	t.Run("目标类型不是memory", func(t *testing.T) {
		op := memoryVerifyRecord(t, core.ActionMemoryPropose, "agent", detail)
		require.Equal(t, core.VerificationIndeterminate, v.verdict(t, op))
	})
	t.Run("未知动作落在分派默认分支", func(t *testing.T) {
		op := memoryVerifyRecord(t, "memory.unknown", core.TargetKindMemory, detail)
		require.Equal(t, core.VerificationIndeterminate, v.verdict(t, op))
	})
	t.Run("detail为空", func(t *testing.T) {
		op := core.OperationRecord{
			OperationID: "op-memory-verify",
			Action:      core.ActionMemoryPropose, TargetKind: core.TargetKindMemory,
		}
		require.Equal(t, core.VerificationIndeterminate, v.verdict(t, op))
	})
	t.Run("detail不是JSON", func(t *testing.T) {
		op := core.OperationRecord{
			OperationID: "op-memory-verify",
			Action:      core.ActionMemoryConfirm, TargetKind: core.TargetKindMemory,
			DetailJSON: []byte("not json"),
		}
		require.Equal(t, core.VerificationIndeterminate, v.verdict(t, op))
	})
	t.Run("缺记忆端口", func(t *testing.T) {
		op := memoryVerifyRecord(t, core.ActionMemoryPropose, core.TargetKindMemory, detail)
		require.Equal(t, core.VerificationIndeterminate,
			verifyOperation(context.Background(), RecoveryDeps{}, op))
	})
}
