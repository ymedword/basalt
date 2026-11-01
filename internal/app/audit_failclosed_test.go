//go:build integration

package app

// 本文件判审计的两条硬约束（US-16 / T095；模块卡 C-04 与 §2.9 的归属-执行者二分）：
//
//   - `TestAudit_WriteFailureBlocksCall`（C-04）：审计预写失败 → 该次模型调用**不得**
//     发出，Run 收在 failed。写诊断失败可以丢，写审计失败不可以——两条相反路径由本
//     文件与 `diagnostics_run_test.go` 各自把住一头。
//   - `TestAudit_ActorDistinctFromOwner`（C-32）：`runs` 行必须同时记下**归属**与**执行
//     者**两列；一次真实的后台触发里，归属是任务面向的调用方，执行者是调度器，两者
//     互不顶替。
//
// 写法仍走生产装配：制造"审计不可用"的手段是把 `llm_calls` 从夹具自己的隔离 schema
// 里移除，而不是换一个进程内假 store——后者会让"装配根本没接上审计"这类回归静默通过。
// The verdicts: a broken audit store refuses the call outright, and a scheduled run keeps
// owner and executor in two columns of the very same row.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestAudit_WriteFailureBlocksCall 判"审计预写失败即拒绝发出"。
//
// 逆改点：把预写错误当成"记不下来也照发"→ 模型调用计数 1、Run 收在 completed，两处
// 断言红；把失败原因写成别的分支（例如 provider 不在表内）→ fail-closed 日志断言红，
// 因为那条日志只在预写失败时出现。
// Reverting the fail-closed branch turns the call count and the log assertion red.
func TestAudit_WriteFailureBlocksCall(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	scripted := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "done"}}}
	fx.override = scripted
	rt := fx.assemble(t)

	// 让审计预写必然失败：把 `llm_calls` 改名——此后写审计表就是"表不存在"。
	// 不能直接 `DROP`：`context_assembly_records.llm_call_id` 有外键指向它，DROP 会被
	// 依赖挡下（实测 SQLSTATE 2BP01）；改名让外键跟着走，写入面则真的不可用。
	// 生产里对应的现实是"审计存储不可用"，判据相同——没有留痕的调用不得发出（宪法 V）。
	// The table is renamed rather than dropped: a dependent FK makes DROP fail, while a
	// rename leaves the writer talking to a relation that no longer exists.
	raw := openRawWrite(t, fx.dsn)
	_, err := raw.ExecContext(context.Background(), `ALTER TABLE llm_calls RENAME TO llm_calls_audit_off`)
	require.NoError(t, err, "改名审计表是制造预写失败的手段，失败说明夹具 schema 不对")

	res, _ := rt.service.Run(context.Background(), core.RunRequest{
		Principal: core.LocalAdminPrincipal(),
		Agent:     "runner",
		SessionID: "audit-write-failure",
		Message:   "你好",
		Channel:   "cli",
	})
	require.Equal(t, core.StatusFailed, res.Status, "审计预写失败必须让 Run 收在 failed")
	require.Zero(t, len(scripted.requests()), "审计预写失败必须拒绝发出调用")
	// 只判"没发出去"不够：失败得能指认到审计这一侧，否则运维会去查模型或网络。
	require.Contains(t, fx.logs.String(), "审计预写失败——拒绝发出该次调用（fail-closed）",
		"拒绝必须留下可定位的 fail-closed 日志")
}

// TestAudit_ActorDistinctFromOwner 判"归属与执行者同时可追溯"。
//
// 用一条 caller-key 任务走真实后台触发：任务归属 `caller-audit`、授权绑定
// `key-audit`，执行者是调度器。`runs` 行因此必须同时满足——`caller_id` 是归属方、
// `actor` 是 `scheduler`、`authorizing_kind`/`key_id` 记下授权来源。少记任何一侧，
// 事故复盘时分不清"谁的任务"和"谁跑的"。
//
// 逆改点：把 `actor` 写成归属方的 `caller`（只记归属）→ actor 断言红；把 `caller_id`
// 写成 `req.UserID`（只记执行者，值为 `scheduler`）→ 归属断言红且外键先一步拒绝；
// 丢弃 `key_id` → 授权来源断言红。
// Reverting any one of the three columns turns exactly one assertion red.
func TestAudit_ActorDistinctFromOwner(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	fx.override = &scriptedProviderClient{replies: []core.ChatResponse{{Content: "done"}}}
	rt := fx.assemble(t)

	ctx := context.Background()
	now := time.Now().UnixMilli()
	raw := openRawWrite(t, fx.dsn)
	_, err := raw.ExecContext(ctx, `
		INSERT INTO callers (caller_id, kind, name, project, status, created_at)
		VALUES ($1, 'service', $1, '', 'active', $2)`, "caller-audit", now)
	require.NoError(t, err, "runs.caller_id 是外键，归属方必须先存在")
	fx.seedCallerLimit(t, "caller-audit")
	_, err = raw.ExecContext(ctx, `
		INSERT INTO caller_keys (key_id, caller_id, key_hash, key_prefix, scopes, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		"key-audit", "caller-audit", "hash-key-audit", "bk_key_audit",
		[]string{core.AgentInvokeScopePrefix + "runner"}, now)
	require.NoError(t, err)

	// 对账会把未声明的任务退休，所以任务行必须在 `Start` **之后**插。
	// Reconciliation retires undeclared tasks, so the row goes in after Start.
	require.NoError(t, rt.scheduler.Start(ctx), "调度器启动先对账出任务行")
	const taskID = "audit-owner-task"
	_, err = raw.ExecContext(ctx, `
		INSERT INTO scheduled_tasks
			(task_id, agent_name, cron, zone, message, deliver_to, enabled, definition_hash,
			 created_at, updated_at, caller_id, authorizing_kind, authorizing_key_id)
		VALUES ($1, 'runner', '0 */5 * * * *', 'UTC', '巡检', '', true, 'hash', $2, $2,
		        'caller-audit', 'caller-key', 'key-audit')`, taskID, now)
	require.NoError(t, err)

	req, res, err := rt.scheduler.RunNow(ctx, taskID)
	require.NoError(t, err)
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)
	require.NotEmpty(t, res.RunID)

	// 执行身份在入口处已经分开：归属方 + 调度器执行者 + 那一把 Key。
	require.Equal(t, "caller-audit", req.Principal.CallerID, "归属是任务面向的调用方")
	require.Equal(t, core.ActorScheduler, req.Principal.Actor, "执行者是调度器")
	require.Equal(t, "key-audit", req.Principal.KeyID)

	// 判据面是**落库的那一行**，不是内存里的 Principal：审计要能事后回答这两个问题。
	// The verdict reads the persisted row, which is what an audit actually replays.
	read := openRead(t, fx.dsn)
	var callerID, authorizingKind, keyID, actor, channel string
	require.NoError(t, read.QueryRowContext(ctx, `
		SELECT caller_id, authorizing_kind, COALESCE(key_id, ''), actor, channel
		FROM runs WHERE run_id = $1`, res.RunID).Scan(
		&callerID, &authorizingKind, &keyID, &actor, &channel))
	require.Equal(t, "caller-audit", callerID, "归属列记的是任务归属方")
	require.Equal(t, "scheduler", actor, "执行者列记的是调度器，与归属互不顶替")
	require.Equal(t, "caller-key", authorizingKind, "授权来源按任务行显式二分")
	require.Equal(t, "key-audit", keyID, "授权绑定的是任务行上那一把 Key")
	require.Equal(t, "scheduler", channel, "通道与归属是两件事：后台触发不进外部通道")
}
