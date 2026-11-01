package core_test

// 本文件守执行器的诊断发事件点（V-8）：三条判据写死在这里——
//   - **锚点**取自 Run 环境与循环给的步骤号（`ExecuteStep`），执行器不自己编序号；
//   - **审计 fail-closed 的留痕不依赖诊断**：预写失败时工具一次都不执行、事件一条都
//     不发（这类故障由 ERROR 日志回答，诊断可丢弃，不得冒充它）；
//   - **诊断不替代审计**：真实执行仍恰好一行审计，事件只引用它（引用翻译在 app 侧
//     适配器，本文件只判 `AuditID` 有值）。
//
// This file pins the executor's V-8 emission points: anchors come from the run env and
// the loop's step, the audit fail-closed path emits nothing, and a real execution still
// leaves exactly one audit row that the diagnostic only references.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// recordingToolDiag 收集执行器发出的工具诊断。
// recordingToolDiag collects the executor's tool diagnostics.
type recordingToolDiag struct {
	events []core.ToolDiagnostic
}

func (r *recordingToolDiag) RecordToolDiagnostic(_ context.Context, ev core.ToolDiagnostic) {
	r.events = append(r.events, ev)
}

// diagExecutor 组一个带诊断端口的执行器（与夹具同源的假工具与假审计）。
// diagExecutor builds an executor with the diagnostic port wired.
func diagExecutor(t *testing.T, audit *execAudit, diag *recordingToolDiag) (*core.ToolExecutor, *execTool) {
	t.Helper()
	tool := &execTool{
		name:   "read_file",
		schema: json.RawMessage(execToolSchema),
		result: core.Result{Content: "【read_file 执行结果】"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	executor := core.NewToolExecutor(newExecPool(tool), audit, logger).WithDiagnostics(diag)
	return executor, tool
}

// TestToolExecutor_DiagnosticCarriesStepAndAuditRow 守成功路径：事件带循环给的
// 步骤号、Run 锚点与审计行 id。
//
// 逆改点：`ExecuteStep` 不透传 step（退回零值）→ step/seq 断言红；不填 AuditID →
// 审计引用断言红。
func TestToolExecutor_DiagnosticCarriesStepAndAuditRow(t *testing.T) {
	diag := &recordingToolDiag{}
	audit := &execAudit{}
	executor, _ := diagExecutor(t, audit, diag)

	res := executor.ExecuteStep(context.Background(), execFixtureEnv(), execToolSet,
		execCall("c1", "read_file", `{"path":"notes.txt"}`), core.ToolStep{StepID: "4", Seq: 4})

	require.Equal(t, "【read_file 执行结果】", res.Content)
	require.Len(t, diag.events, 1)
	ev := diag.events[0]
	require.Equal(t, core.ToolDiagnosticCall, ev.Kind)
	require.Equal(t, execRunID, ev.RunID)
	require.Equal(t, "4", ev.StepID)
	require.Equal(t, int64(4), ev.Seq)
	require.Equal(t, "read_file", ev.Tool)
	require.Empty(t, ev.ErrorKind, "成功执行不带失败分类")
	require.NotZero(t, ev.AuditID, "真实执行必须引用它的审计行")
	require.Equal(t, 1, audit.count(), "审计行仍恰好一行（诊断不替代它）")
}

// TestToolExecutor_AuditFailureEmitsNoDiagnostic 守审计 fail-closed 与诊断的边界：
// 预写失败时工具不执行、事件为零——这条留痕归 ERROR 日志，不归可丢弃的诊断。
//
// 逆改点：在审计不可用分支也发一条 `tool_call` → 事件断言红（并会让排障误以为
// 调用发生过）。
func TestToolExecutor_AuditFailureEmitsNoDiagnostic(t *testing.T) {
	diag := &recordingToolDiag{}
	audit := &execAudit{failStart: errors.New("disk full")}
	executor, tool := diagExecutor(t, audit, diag)

	res := executor.ExecuteStep(context.Background(), execFixtureEnv(), execToolSet,
		execCall("c1", "read_file", `{"path":"notes.txt"}`), core.ToolStep{StepID: "1", Seq: 1})

	require.Contains(t, res.Content, "审计不可用")
	require.Zero(t, tool.executedCount(), "预写失败时工具不得执行（fail-closed）")
	require.Empty(t, diag.events, "审计不可用的留痕不得依赖可丢弃的诊断")
	require.Zero(t, audit.count())
}

// TestToolExecutor_MissingAuditStoreIsLoggedAsError 守装配缺陷的痕迹：审计仓储为
// nil 时工具不执行（fail-closed），且**必须**留下 ERROR 日志——这条分支不经过
// `beginInvocation`，没有那一侧的日志兜底，装配错误若只表现为回填文案，线上既无
// 痕迹也无告警。
//
// 逆改点：删掉 nil 分支的 `e.logger.Error` → 日志断言红；放行执行 → 工具执行计数红。
func TestToolExecutor_MissingAuditStoreIsLoggedAsError(t *testing.T) {
	diag := &recordingToolDiag{}
	tool := &execTool{
		name:   "read_file",
		schema: json.RawMessage(execToolSchema),
		result: core.Result{Content: "【read_file 执行结果】"},
	}
	var logs bytes.Buffer
	executor := core.NewToolExecutor(newExecPool(tool), nil,
		slog.New(slog.NewTextHandler(&logs, nil))).WithDiagnostics(diag)

	res := executor.ExecuteStep(context.Background(), execFixtureEnv(), execToolSet,
		execCall("c1", "read_file", `{"path":"notes.txt"}`), core.ToolStep{StepID: "1", Seq: 1})

	require.Contains(t, res.Content, "审计不可用")
	require.Zero(t, tool.executedCount(), "审计仓储缺失时不得执行（fail-closed）")
	require.Empty(t, diag.events, "这条留痕归 ERROR 日志，不归可丢弃的诊断")
	require.Contains(t, logs.String(), "level=ERROR")
	require.Contains(t, logs.String(), "审计仓储未装配")
}

// execFixtureEnv 是带 Run 锚点的执行环境（点名字段与夹具同源）。
// execFixtureEnv is the run-anchored execution environment.
func execFixtureEnv() core.ToolEnv {
	return core.ToolEnv{
		RunID:          execRunID,
		SessionID:      execSessionID,
		AgentName:      "demo",
		Timeout:        0,
		MaxOutputBytes: 256 * 1024,
	}
}
