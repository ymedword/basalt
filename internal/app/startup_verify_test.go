package app

// 本文件判 `serve --verification-window`（§2.12.0b 步骤 7 / C-51 / §2.15.2，任务 T143）。
//
// 两条具名用例覆盖两件不同的事：
//   - `TestStartupVerification_OfflineGateBeforeServe` 判**顺序**：离线门不过就绝不能有
//     端口被打开，MCP 与模型外呼计数必须为零。正例同时跑一遍健康副本——没有正例，
//     "ListenerOpened 为 false" 就分不清是判据生效还是这个标志位根本没被置过。
//   - `TestStartupVerification_RollbackBoundary` 判**回退四值**：干净失败才允许自动回退，
//     MCP 已启动 / 工作区已变化 / 情况无法证明，三者都必须阻断，且**不得**顺手恢复原工作区。
//
// 两个用例都**不依赖 PostgreSQL**：库探针是注入的桩。窗口在正常路径上根本不装配运行期，
// 这一点正是判据本身，所以夹具不需要真库。
//
// The two named cases pin the ordering (gate before any listener) and the four rollback
// decisions. Both stay PostgreSQL-free: the window never assembles the runtime at all.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/dbmigrate"
	"github.com/ymedword/basalt/internal/storage"
)

// windowProbeStub 是 PG-free 的只读库探针：报本二进制支持的 schema 版本、一条
// first_write_at 为空的切换行与一份固定行数快照。
// windowProbeStub stands in for the read-only PostgreSQL probe: a supported schema
// version, an open rollback window and a fixed row-count baseline.
func windowProbeStub() windowDatabaseProbe {
	state := windowDatabaseState{
		SchemaVersion: storage.SupportedSchemaVersion,
		CutoverID:     "cutover-fixture",
		CutoverAt:     1_700_000_000_000,
		RowCounts:     map[string]int64{"runs": 0, "messages": 3},
	}
	return func(context.Context) (windowDatabaseState, error) { return state, nil }
}

// windowProbeStates 依次返回给定的读数（窗口开始一次、结束一次）；用尽后重复最后一个。
// windowProbeStates replays one state per call, then repeats the last one.
func windowProbeStates(states ...windowDatabaseState) windowDatabaseProbe {
	index := 0
	return func(context.Context) (windowDatabaseState, error) {
		state := states[len(states)-1]
		if index < len(states) {
			state = states[index]
		}
		index++
		return state, nil
	}
}

// boundedWindowCtx 返回带上限的上下文，供"预期在监听之前就被拒绝"的子例使用。
//
// 正常路径根本用不到这个上限（拒绝发生在 listen 之前）；一旦逆改让窗口错误地开了
// 监听，窗口会在上限到点后自己停下，红态因此是一行断言失败，而不是 `go test`
// 默认的 10 分钟超时 panic——超时也能变红，但那不是可读的判据。
// boundedWindowCtx keeps a wrongly-opened listener from turning a red state into the
// test binary's 10-minute timeout panic.
func boundedWindowCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// openWindowState 返回 first_write_at 为空的读数（可按需改表行数）。
// openWindowState is a rollback-window-open reading with adjustable row counts.
func openWindowState(rows map[string]int64) windowDatabaseState {
	return windowDatabaseState{
		SchemaVersion: storage.SupportedSchemaVersion,
		CutoverID:     "cutover-fixture",
		CutoverAt:     1_700_000_000_000,
		RowCounts:     rows,
	}
}

// newTestWindow 造一个日志丢弃、探针可换的验证窗口。
// newTestWindow builds a window whose log is discarded and whose probe is injectable.
func newTestWindow(probe windowDatabaseProbe) *verificationWindow {
	return &verificationWindow{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		gate:   handoffVerifyPort{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
		probe:  probe,
		now:    baselineClock,
	}
}

// workspaceDigest 采一份工作区快照（相对路径 → 内容 sha256）。
// workspaceDigest captures the workspace tree.
func workspaceDigest(t *testing.T, ws string) dbmigrate.WorkspaceSnapshot {
	t.Helper()
	snapshot, err := dbmigrate.CaptureWorkspaceTree(ws, ".")
	require.NoError(t, err)
	return snapshot
}

// tamperOneCiphertext 换掉一条托管密文的密文字节，保留 schema 允许的形状（非空 BLOB）：
// 只有解密会失败，行本身仍"看起来"合法。
// tamperOneCiphertext keeps the row shape legal and breaks only the decryption.
func tamperOneCiphertext(t *testing.T, f *handoffFixture) {
	t.Helper()
	f.execRaw(`UPDATE credential_versions SET ciphertext = ?
	            WHERE credential_id = ? AND version_no = 1`,
		bytes.Repeat([]byte{0x5A}, 48), handoffCredentialID)
}

// TestStartupVerification_OfflineGateBeforeServe 判"先离线门、再起 serve"这条顺序。
//
// 逆改点：把 `listenVerificationWindow` 提到离线门之前（或离线门失败仍继续启动）→
// 子例一 `ListenerOpened` 为真、`OnReady` 被回调，`ListenerAddr` 非空；把 MCP / 调度器
// 装进验证路径 → `MCPStarts` / `ModelCalls` 非零。子例二证明 `ListenerOpened` 不是死标志。
// TestStartupVerification_OfflineGateBeforeServe pins gate-before-listener, with a healthy
// control case proving the listener flag is live.
func TestStartupVerification_OfflineGateBeforeServe(t *testing.T) {
	t.Run("tampered_ciphertext_never_opens_the_listener", func(t *testing.T) {
		f := newHandoffFixture(t)
		tamperOneCiphertext(t, f)

		listenerCalls := 0
		report, err := newTestWindow(windowProbeStub()).Run(context.Background(),
			verificationWindowOptions{
				Workspace: f.ws,
				OnReady:   func(string) { listenerCalls++ },
			})

		// 离线门失败必须带稳定码、退出码 1，并且**先**于任何监听发生。
		require.Error(t, err)
		require.Equal(t, 1, cli.ExitStatusOf(err), "离线门不就绪按退出码 1")
		require.Equal(t, handoffCodeIdentityMismatch, report.GateCode,
			"密文被篡改落在统一核验的身份不符码上（颗粒度文档给出的 ciphertext_* 字面量是"+
				"T142 落地前的提案名，落地后统一到 instance_identity_mismatch）")
		require.Contains(t, err.Error(), "托管凭据核验未通过")
		require.False(t, report.GateReady)

		require.False(t, report.ListenerOpened, "离线门失败不得启动 serve")
		require.Empty(t, report.ListenerAddr, "离线门失败不得占用任何端口")
		require.Zero(t, listenerCalls, "离线门失败不得回调 ready")
		require.False(t, report.DatabaseChecked, "离线门失败不得再探库")
		require.Zero(t, report.Evidence.MCPStarts, "离线门失败不得启动 MCP")
		require.Zero(t, report.Evidence.ModelCalls, "离线门失败不得外呼")
		require.Zero(t, report.Evidence.Notifications, "离线门失败不得发通知")
		require.Equal(t, windowDecisionAllowed, report.Decision,
			"零副作用的中止允许回到原工作区")

		// 库文件本身没有被这次运行改动：窗口不迁移、不写业务表。
		require.FileExists(t, f.dbPath)
		require.NoFileExists(t, filepath.Join(f.ws, "runs"), "验证窗口不得产出业务文件")
	})

	t.Run("healthy_copy_opens_loopback_listener", func(t *testing.T) {
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var readyStatus int
		var readyBody string
		report, err := newTestWindow(windowProbeStub()).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady: func(addr string) {
				resp, getErr := http.Get("http://" + addr + "/api/v1/ready")
				if getErr == nil {
					defer func() { _ = resp.Body.Close() }()
					raw, readErr := io.ReadAll(resp.Body)
					if readErr == nil {
						readyStatus, readyBody = resp.StatusCode, string(raw)
					}
				}
				cancel()
			},
		})
		require.NoError(t, err)

		require.True(t, report.GateReady)
		require.True(t, report.DatabaseChecked)
		require.Equal(t, storage.SupportedSchemaVersion, report.SchemaVersion)
		require.True(t, report.ListenerOpened, "健康副本上端口必须真的开过——否则上一条断言没有判别力")
		require.Contains(t, report.ListenerAddr, "127.0.0.1:", "验证窗口只监听回环")
		require.Equal(t, http.StatusOK, readyStatus)
		require.Contains(t, readyBody, `"verification_window":true`)
		require.False(t, report.Evidence.WorkspaceChanged, "窗口内不写业务文件")
		require.Equal(t, windowDecisionAllowed, report.Decision)
	})

	t.Run("non_loopback_address_is_refused", func(t *testing.T) {
		f := newHandoffFixture(t)
		report, err := newTestWindow(windowProbeStub()).Run(context.Background(),
			verificationWindowOptions{Workspace: f.ws, Addr: "0.0.0.0:0"})
		require.Error(t, err)
		require.Equal(t, 2, cli.ExitStatusOf(err), "监听地址不合法按前置不满足")
		require.Contains(t, err.Error(), windowCodeListenerRejected)
		require.False(t, report.ListenerOpened, "被拒的地址不得打开端口")
	})
}

// TestStartupVerification_RollbackBoundary 判三维判据投影出的四个回退结论。
//
// 逆改点：`decideWindowRollback` 只按"验证失败"判回退（不看外部副作用）→ 子例二变
// allowed；窗口结束时不重盘工作区 → 子例三变 allowed 且 `WorkspaceChanged` 为假；
// 探针缺席被当成"没问题" → 子例四变 allowed。任何子例里出现"自动恢复原工作区"
// （文件被搬动 / 删除），对应用例都会红。
// TestStartupVerification_RollbackBoundary pins the four decisions and the "never restore
// the source workspace on its own" rule.
func TestStartupVerification_RollbackBoundary(t *testing.T) {
	t.Run("clean_failure_allows_source_restore", func(t *testing.T) {
		f := newHandoffFixture(t)
		// 采纳之后改配置：离线门必拒，但窗口一条工作区写入都没做。
		edited := []byte(adoptConfigYAML("local_trust", "", adoptKeyLine(f.keyPath), "handoff_note: edited"))
		require.NoError(t, os.WriteFile(f.configPath, edited, 0o600))
		before := workspaceDigest(t, f.ws)

		report, err := newTestWindow(windowProbeStub()).Run(context.Background(),
			verificationWindowOptions{Workspace: f.ws})
		require.Error(t, err)
		require.Equal(t, handoffCodeIdentityMismatch, report.GateCode)
		require.Equal(t, windowDecisionAllowed, report.Decision, "零副作用才允许自动回退")

		// 只读打开 SQLite 会在工作区留下 `-shm` / `-wal` 附属文件（这是离线门自己的痕迹，
		// 不是本用例要判的"回退"）。判据是**原有文件一个都没被动过**：搬走 / 恢复原工作区
		// 会让这里的某个路径消失或摘要改变。
		// The read-only SQLite open leaves -shm/-wal behind; what this case pins is that no
		// pre-existing file was moved, restored or rewritten.
		after := workspaceDigest(t, f.ws)
		for path, sum := range before {
			require.Equal(t, sum, after[path],
				"回退判定只是结论：窗口不得自行恢复 / 搬动原工作区（%s 被改动或搬走）", path)
		}
		require.FileExists(t, f.dbPath)
		require.FileExists(t, filepath.Join(f.ws, instanceMarkerFileName))
	})

	t.Run("mcp_started_blocks_rollback", func(t *testing.T) {
		// 逆改点：只按"验证失败"判回退，不看 MCP 已启动 → 这个证据会变成 allowed，
		// 外部副作用被掩盖成"可以无损退回"。
		require.Equal(t, windowDecisionBlockedExternal,
			decideWindowRollback(windowEvidence{MCPStarts: 1}))
		require.Equal(t, windowDecisionBlockedExternal,
			decideWindowRollback(windowEvidence{ModelCalls: 1}))
		require.Equal(t, windowDecisionBlockedExternal,
			decideWindowRollback(windowEvidence{Notifications: 1}))

		// 窗口自身永远不拉起 MCP：健康副本上计数必须是零（否则上一条判据没有意义）。
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		report, err := newTestWindow(windowProbeStub()).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady:   func(string) { cancel() },
		})
		require.NoError(t, err)
		require.Zero(t, report.Evidence.MCPStarts, "验证姿态不得启动 MCP supervisor")
		require.Zero(t, report.Evidence.ModelCalls, "验证姿态不得外呼模型")
		require.Zero(t, report.Evidence.Notifications, "验证姿态不得发送通知")
	})

	t.Run("workspace_file_changed_blocks_rollback", func(t *testing.T) {
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		strayPath := filepath.Join(f.ws, "runs", "window", "report.md")
		report, err := newTestWindow(windowProbeStub()).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady: func(string) {
				// 模拟"验证期间触发了一次任务"：窗口自己写不出来，所以由夹具在窗口内写。
				if mkErr := os.MkdirAll(filepath.Dir(strayPath), 0o700); mkErr != nil {
					t.Errorf("夹具建目录失败: %v", mkErr)
				}
				if writeErr := os.WriteFile(strayPath, []byte("window drift\n"), 0o600); writeErr != nil {
					t.Errorf("夹具写文件失败: %v", writeErr)
				}
				cancel()
			},
		})
		require.NoError(t, err)
		require.True(t, report.Evidence.WorkspaceChanged, "窗口内新增业务文件必须被重盘发现")
		require.Equal(t, windowDecisionBlockedWorkspace, report.Decision)
		require.FileExists(t, strayPath,
			"阻断自动回退时不得顺手清理 / 恢复工作区——两侧证据都要留给人工")
	})

	t.Run("unknown_external_result_requires_manual", func(t *testing.T) {
		// 逆改点：探针缺席被当成"没查到问题"→ decision 变 allowed，数据库维度被静默放过。
		f := newHandoffFixture(t)
		report, err := newTestWindow(nil).Run(context.Background(),
			verificationWindowOptions{Workspace: f.ws})
		require.Error(t, err)
		require.Equal(t, windowCodeProbeMissing, windowRefusalCode(err))
		require.True(t, report.Evidence.DatabaseUnprovable, "证明不了就必须如实记录，不能默认干净")
		require.Equal(t, windowDecisionBlockedUnknown, report.Decision, "无法证明一律转人工")
		require.False(t, report.ListenerOpened, "无法证明时不得先把 serve 起来")
	})

	t.Run("unprovable_workspace_is_not_clean", func(t *testing.T) {
		// 与上一条同源：工作区快照采不到时同样只能转人工。
		require.Equal(t, windowDecisionBlockedUnknown,
			decideWindowRollback(windowEvidence{WorkspaceUnprovable: true}))
		require.Equal(t, windowDecisionBlockedUnknown,
			decideWindowRollback(windowEvidence{UnknownExternal: true}))
		require.Equal(t, windowDecisionAllowed, decideWindowRollback(windowEvidence{}))
		// 已确认的脏维优先于"无法证明"：报告要先说不可撤销的那一条。
		require.Equal(t, windowDecisionBlockedExternal,
			decideWindowRollback(windowEvidence{MCPStarts: 1, WorkspaceUnprovable: true}))
		require.Equal(t, windowDecisionBlockedWorkspace,
			decideWindowRollback(windowEvidence{WorkspaceChanged: true, DatabaseUnprovable: true}))
		// 数据库业务写入同样优先于"无法证明"与工作区变化：first_write_at 非空之后
		// 只允许前滚（C-13），这条比"再改一次副本文件"更靠前。
		require.Equal(t, windowDecisionBlockedDatabase,
			decideWindowRollback(windowEvidence{DatabaseWritten: true}))
		require.Equal(t, windowDecisionBlockedDatabase,
			decideWindowRollback(windowEvidence{DatabaseWritten: true, WorkspaceUnprovable: true}))
		require.Equal(t, windowDecisionBlockedDatabase,
			decideWindowRollback(windowEvidence{DatabaseWritten: true, WorkspaceChanged: true}))
		require.Equal(t, windowDecisionBlockedExternal,
			decideWindowRollback(windowEvidence{MCPStarts: 1, DatabaseWritten: true}),
			"外部副作用仍排第一：它撤不回来")
	})
}

// TestStartupVerification_DatabaseDimension 判 S-F01（2026-10-31 裁决第 1 条）：
// 验证窗口必须真的读 `storage_cutover.first_write_at` 并覆盖四种情况——已写入（窗口
// 开始前 / 窗口内）、未写入、读取失败、结果未知；行数变化是"提交结果未知"的观测面。
//
// 逆改点：窗口不读切换行（返回 `FirstWriteAt` 恒空）→ 已写入子例变 allowed；
// 不重读结束状态 → 窗口内写入子例变 allowed；把读失败当"没写过" → 失败子例变
// allowed；不比对行数 → 行数增长子例变 allowed。
// TestStartupVerification_DatabaseDimension pins the first_write_at wiring and the four
// situations the adjudication names.
func TestStartupVerification_DatabaseDimension(t *testing.T) {
	t.Run("already_written_refuses_before_listening", func(t *testing.T) {
		f := newHandoffFixture(t)
		written := int64(1_700_000_100_000)
		probe := func(context.Context) (windowDatabaseState, error) {
			state := openWindowState(map[string]int64{"runs": 2})
			state.FirstWriteAt = &written
			return state, nil
		}
		listenerCalls := 0
		report, err := newTestWindow(probe).Run(boundedWindowCtx(t),
			verificationWindowOptions{
				Workspace: f.ws,
				OnReady:   func(string) { listenerCalls++ },
			})

		require.Error(t, err)
		require.Equal(t, windowCodeDatabaseWritten, windowRefusalCode(err),
			"已越过回退点的实例必须用独立稳定码拒绝")
		require.Equal(t, 1, cli.ExitStatusOf(err))
		require.True(t, report.DatabaseChecked, "库确实被读过")
		require.True(t, report.Evidence.DatabaseWritten)
		require.False(t, report.Evidence.DatabaseUnprovable, "这是**已确认**的写入，不是无法证明")
		require.Equal(t, windowDecisionBlockedDatabase, report.Decision)
		require.NotNil(t, report.FirstWriteAt)
		require.Equal(t, written, *report.FirstWriteAt)
		require.False(t, report.ListenerOpened, "回退窗口已关闭时不得先开监听")
		require.Zero(t, listenerCalls)
	})

	t.Run("missing_cutover_marker_is_refused", func(t *testing.T) {
		f := newHandoffFixture(t)
		probe := func(context.Context) (windowDatabaseState, error) {
			return windowDatabaseState{}, fmt.Errorf("%w（目标库可连不代表它被切换过）",
				dbmigrate.ErrNoCutoverMarker)
		}
		report, err := newTestWindow(probe).Run(boundedWindowCtx(t),
			verificationWindowOptions{Workspace: f.ws})

		require.Error(t, err)
		require.Equal(t, windowCodeCutoverMissing, windowRefusalCode(err),
			"没切换过的库不是本窗口的对象，稳定码要单独给")
		require.Equal(t, 1, cli.ExitStatusOf(err))
		require.True(t, report.Evidence.DatabaseUnprovable)
		require.Equal(t, windowDecisionBlockedUnknown, report.Decision)
		require.False(t, report.ListenerOpened)
	})

	t.Run("probe_failure_is_unprovable", func(t *testing.T) {
		f := newHandoffFixture(t)
		probe := func(context.Context) (windowDatabaseState, error) {
			return windowDatabaseState{}, errors.New("connection reset by peer")
		}
		report, err := newTestWindow(probe).Run(boundedWindowCtx(t),
			verificationWindowOptions{Workspace: f.ws})

		require.Error(t, err)
		require.Equal(t, windowCodeDatabaseMissing, windowRefusalCode(err))
		require.True(t, report.Evidence.DatabaseUnprovable)
		require.Equal(t, windowDecisionBlockedUnknown, report.Decision,
			"读失败与「提交结果未知」同一处置：不知道就不算干净（C-34）")
		require.False(t, report.ListenerOpened)
	})

	t.Run("first_write_during_window_blocks_rollback", func(t *testing.T) {
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		written := int64(1_700_000_200_000)
		end := openWindowState(map[string]int64{"runs": 0, "messages": 3})
		end.FirstWriteAt = &written
		probe := windowProbeStates(
			openWindowState(map[string]int64{"runs": 0, "messages": 3}), end)

		report, err := newTestWindow(probe).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady:   func(string) { cancel() },
		})
		require.NoError(t, err)
		require.True(t, report.ListenerOpened, "窗口正常开过——否则下面的断言没有判别力")
		require.True(t, report.Evidence.DatabaseWritten,
			"窗口内提交的首个业务写入必须被判脏")
		require.False(t, report.Evidence.DatabaseUnprovable)
		require.Equal(t, windowDecisionBlockedDatabase, report.Decision)
		require.Len(t, report.DatabaseFindings, 1)
		require.Contains(t, report.DatabaseFindings[0], "first_write_at")
	})

	t.Run("row_growth_during_window_blocks_rollback", func(t *testing.T) {
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		probe := windowProbeStates(
			openWindowState(map[string]int64{"runs": 0, "messages": 3}),
			openWindowState(map[string]int64{"runs": 1, "messages": 3}))

		report, err := newTestWindow(probe).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady:   func(string) { cancel() },
		})
		require.NoError(t, err)
		require.True(t, report.Evidence.DatabaseWritten,
			"绕过 MarkFirstWrite 的写入也必须被行数变化抓到")
		require.Equal(t, windowDecisionBlockedDatabase, report.Decision)
		require.Len(t, report.DatabaseFindings, 1)
		require.Contains(t, report.DatabaseFindings[0], "表 runs")
		require.Contains(t, report.DatabaseFindings[0], "+1")
	})

	t.Run("cutover_row_change_is_unprovable", func(t *testing.T) {
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		start := openWindowState(map[string]int64{"runs": 0})
		end := start
		end.CutoverID = "cutover-second"
		end.CutoverAt = start.CutoverAt + 1
		probe := windowProbeStates(start, end)

		report, err := newTestWindow(probe).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady:   func(string) { cancel() },
		})
		require.NoError(t, err)
		require.True(t, report.Evidence.DatabaseUnprovable,
			"窗口内又切换了一次：新行的 first_write_at 也是空的，单看空标记会读成干净")
		require.False(t, report.Evidence.DatabaseWritten)
		require.Equal(t, windowDecisionBlockedUnknown, report.Decision)
	})

	t.Run("end_read_failure_is_unprovable", func(t *testing.T) {
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		calls := 0
		probe := func(context.Context) (windowDatabaseState, error) {
			calls++
			if calls > 1 {
				return windowDatabaseState{}, errors.New("i/o timeout")
			}
			return openWindowState(map[string]int64{"runs": 0}), nil
		}

		report, err := newTestWindow(probe).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady:   func(string) { cancel() },
		})
		require.NoError(t, err)
		require.True(t, report.Evidence.DatabaseUnprovable,
			"结束重读失败时不得沿用开始时的「干净」")
		require.Equal(t, windowDecisionBlockedUnknown, report.Decision)
		require.Len(t, report.DatabaseFindings, 1)
		require.Contains(t, report.DatabaseFindings[0], "无法证明")
	})

	// 停机信号**就是**结束重读的触发条件：若重读继承 serve 的取消上下文，每一次正常
	// 停机都会被记成"数据库维度无法证明"，三维都干净的回退结论永远拿不到。
	// The serve cancellation is what triggers the re-read; inheriting it would mark every
	// clean shutdown unprovable and make decision=allowed unreachable.
	t.Run("end_read_outlives_serve_cancellation", func(t *testing.T) {
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		calls := 0
		canceledSeen := false
		probe := func(probeCtx context.Context) (windowDatabaseState, error) {
			calls++
			if probeCtx.Err() != nil {
				canceledSeen = true
				return windowDatabaseState{}, fmt.Errorf("装配 PostgreSQL 连接池: %w", probeCtx.Err())
			}
			return openWindowState(map[string]int64{"runs": 0, "messages": 3}), nil
		}

		report, err := newTestWindow(probe).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady:   func(string) { cancel() },
		})
		require.NoError(t, err)
		require.Equal(t, 2, calls, "窗口开始与结束各读一次")
		require.False(t, canceledSeen, "结束重读不得继承 serve 的取消上下文")
		require.False(t, report.Evidence.DatabaseUnprovable, "三维都干净时数据库维度必须可证明")
		require.Equal(t, windowDecisionAllowed, report.Decision,
			"正常停机后三维都干净必须判 allowed")
	})

	t.Run("unchanged_window_stays_allowed", func(t *testing.T) {
		f := newHandoffFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		rows := map[string]int64{"runs": 0, "messages": 3}
		probe := windowProbeStates(openWindowState(rows), openWindowState(rows))

		report, err := newTestWindow(probe).Run(ctx, verificationWindowOptions{
			Workspace: f.ws,
			OnReady:   func(string) { cancel() },
		})
		require.NoError(t, err)
		require.False(t, report.Evidence.DatabaseWritten)
		require.False(t, report.Evidence.DatabaseUnprovable)
		require.Empty(t, report.DatabaseFindings)
		require.Equal(t, windowDecisionAllowed, report.Decision,
			"first_write_at 为空且起止行数一致——数据库维度干净")
		require.NotNil(t, report.DatabaseNote)
		require.Contains(t, report.DatabaseNote, "MarkFirstWrite",
			"报告必须写明空标记的覆盖范围，不能让人读成「无损可回退」")
	})
}

// TestStartupVerification_WindowArgsParsing 判参数面的两条硬规则：`--asset-package`
// 单独出现是用法错误（不是"顺手忽略"），非窗口调用必须原样放行。
// TestStartupVerification_WindowArgsParsing pins the argument face.
func TestStartupVerification_WindowArgsParsing(t *testing.T) {
	t.Run("plain_serve_is_not_a_window", func(t *testing.T) {
		_, matched, err := parseVerificationWindowArgs([]string{"serve"})
		require.NoError(t, err)
		require.False(t, matched)
	})

	t.Run("asset_package_alone_is_a_usage_error", func(t *testing.T) {
		_, matched, err := parseVerificationWindowArgs([]string{"serve", "--asset-package", "/tmp/pkg"})
		require.True(t, matched, "必须被认出来，才不会被静默忽略")
		require.Error(t, err)
		require.Equal(t, 2, cli.ExitStatusOf(err))
	})

	t.Run("window_with_asset_package", func(t *testing.T) {
		opts, matched, err := parseVerificationWindowArgs([]string{
			"serve", "--verification-window", "--asset-package=/tmp/pkg",
		})
		require.NoError(t, err)
		require.True(t, matched)
		require.Equal(t, "/tmp/pkg", opts.AssetPackage)
	})

	t.Run("other_commands_are_untouched", func(t *testing.T) {
		_, matched, err := parseVerificationWindowArgs([]string{"gateway", "--verification-window"})
		require.NoError(t, err)
		require.False(t, matched, "验证窗口只属于 serve")
	})

	t.Run("help_text_names_the_flags", func(t *testing.T) {
		// 旗标不注册到 cobra（由装配层消费），所以帮助文本是唯一的发现入口。
		// The flags are consumed before cobra exists, so --help is where they are found.
		var serve *cobra.Command
		for _, cmd := range cli.NewRootCommand(cli.Deps{}).Commands() {
			if cmd.Name() == "serve" {
				serve = cmd
				break
			}
		}
		require.NotNil(t, serve, "serve 必须在根命令上")
		require.True(t, serve.HasSubCommands() == false)
		require.Contains(t, serve.Long, "--verification-window")
		require.Contains(t, serve.Long, "--asset-package")
	})
}
