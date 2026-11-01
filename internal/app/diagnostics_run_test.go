//go:build integration

package app

// 本文件是诊断生产端在**真实 Run** 上的判卷（US-16 / T095；模块卡 §4.3 的
// `TestDiagnostics_WriteFailureDoesNotBlockRun`，`us16-pg` 具名用例）。
//
// 判的是两条**相反**的路径同时成立（模块卡 C-04）：
//
//   - **诊断可丢弃**：诊断写入失败只留 WARN，Run 照常跑完、模型照常被调用；
//   - **审计 fail-closed**：同一批装配里，审计预写失败必须拒绝发出调用
//     （由 `TestAudit_WriteFailureBlocksCall` 判，本文件不重复）。
//
// 写法上刻意**不**替换装配：走 `newWireFixture` + `assemble` 的生产装配，只把
// `diagnostic_events` 从夹具自己的隔离 schema 里移除——这就是"写诊断时存储不可用"
// 的最短真实形态。若换成进程内假 store，"装配根本没接上诊断"这类回归会静默通过。
//
// The verdicts: a broken diagnostics store must never block a run, and the failure must
// still leave a WARN behind. The assembly is the production one; only the table is gone.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestDiagnostics_WriteFailureDoesNotBlockRun 判"诊断写失败不阻塞 Run"。
//
// 逆改点：把诊断写入改成阻塞（`Recorder.Record` 返回错误且装饰器上抛）→ 首条
// `require.NoError` / `StatusCompleted` 红；诊断根本没接进运行路径 → "WARN 一行都没
// 出现"断言红（没有尝试过写入就不可能有这条日志）；把失败写成静默 → 同样红。
// Reverting any of the three turns exactly one assertion red.
func TestDiagnostics_WriteFailureDoesNotBlockRun(t *testing.T) {
	fx := newWireFixture(t)
	fx.writeAgent(t, "runner")
	scripted := &scriptedProviderClient{replies: []core.ChatResponse{{Content: "done"}}}
	fx.override = scripted
	rt := fx.assemble(t)

	// 让诊断写入必然失败：把事件表从**夹具自己的隔离 schema** 里移除。生产里对应的
	// 现实是"存储不可用"，判据相同——写入路径必须吞掉这个错误。
	// The table is dropped from the fixture's own isolated schema so every insert fails.
	raw := openRawWrite(t, fx.dsn)
	_, err := raw.ExecContext(context.Background(), `DROP TABLE diagnostic_events`)
	require.NoError(t, err, "移除事件表是制造写入失败的手段，失败说明夹具 schema 不对")

	res, err := rt.service.Run(context.Background(), core.RunRequest{
		Principal: core.LocalAdminPrincipal(),
		Agent:     "runner",
		SessionID: "diagnostics-write-failure",
		Message:   "你好",
		Channel:   "cli",
	})
	require.NoError(t, err, "诊断写失败不得把 Run 判成错误")
	require.Equal(t, core.StatusCompleted, res.Status, "reason=%s", res.Reason)
	require.NotEmpty(t, scripted.requests(), "诊断写失败不得阻止模型调用")

	// 诊断是**可丢弃**的，不是可静默的：失败必须留一行 WARN，否则线上排障无从知道
	// "这段时间没有诊断事件"是采样丢弃还是存储坏了。
	// A dropped diagnostic is still observable through exactly one WARN line.
	require.Contains(t, fx.logs.String(), "诊断事件写入失败——已丢弃",
		"诊断写失败必须留 WARN（诊断可丢弃 ≠ 可以静默）")
}
