package cli

// 本文件对应颗粒度文档 §3.2 的 cli/session 四条（S1~S4）。判卷面是**只读端口**：假
// `SessionLister` 顶替实现（cli 不得 import storage），**调用序列本身就是判据**——
// "只读命令不取实例锁、不做崩溃恢复"落在可判的那一面：整条路径唯一触达的端口方法就是
// `ListSessions`（恢复要先读消息，读了消息就说明它在做恢复该做的事）。
// 真库、真锁与真 `schema_version` 判定在装配层判（颗粒度文档 §4.3 的 T1 / T4），本层判
// 命令面自己那半边：**一个只读调用、零写入**。
// The four cases here judge the command surface's own half: exactly one read-only call
// and no write. The real database, lock and schema verdict belong to the assembly layer.

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeSessionLister 是 SessionLister 端口的判卷替身：返回预置结果或预置错误，并记下
// 每一次调用（"命令面到底碰了哪些方法"是 S4 的直接判据）。
// fakeSessionLister returns canned results and records every call: which methods the
// command touched is itself the acceptance criterion.
type fakeSessionLister struct {
	sessions    []core.SessionRecord
	sessionsErr error
	messages    map[string][]core.Message
	messagesErr error

	calls []string
}

func (f *fakeSessionLister) ListSessions(context.Context) ([]core.SessionRecord, error) {
	f.calls = append(f.calls, "ListSessions")
	return f.sessions, f.sessionsErr
}

func (f *fakeSessionLister) Messages(_ context.Context, sessionID string) ([]core.Message, error) {
	f.calls = append(f.calls, "Messages:"+sessionID)
	return f.messages[sessionID], f.messagesErr
}

// sessionRun 是一次 session 子命令运行的返回错误与三路输出。
// sessionRun captures the error and the three output tracks of one command run.
type sessionRun struct {
	err    error
	stdout string
	stderr string
	logs   string
}

// sessionDeps 组装命令面依赖：Store / Workspace / Agents / RunService 一律留 nil——
// `session list` 只该碰会话端口，碰到别的就是缺陷（nil 接口调用当场 panic，判卷比"悄悄
// 通过"更早也更响）。
// sessionDeps leaves every other port nil on purpose: session list must touch the
// session port only, so any other touch panics loudly instead of passing quietly.
func sessionDeps(workDir string, lister SessionLister) Deps {
	return Deps{
		WorkDir:                workDir,
		SupportedSchemaVersion: 7,
		SessionLister:          lister,
	}
}

// executeSession 在注入的依赖上跑一次**真根命令**（注册、参数校验与出口码都在被测面内）。
// executeSession runs the real root command, keeping registration and the exit-code
// path under test.
func executeSession(t *testing.T, deps Deps, args ...string) sessionRun {
	t.Helper()

	var logs bytes.Buffer
	deps.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	root := NewRootCommand(deps)
	root.SetArgs(args)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(context.Background())
	return sessionRun{err: err, stdout: stdout.String(), stderr: stderr.String(), logs: logs.String()}
}

// S1 渲染：一行一会话 `<id>：<消息数> 条（最后活动 <2006-01-02 15:04:05>）` + 末尾
// `共 N 个会话`（§6.2 固定口径 5 逐字——改措辞等于改判据，故期望值是整段字面量）。
// S1 renders one line per session plus the summary, verbatim.
func TestSessionList_RendersRowsAndSummary(t *testing.T) {
	// 夹具用 **UTC 时刻**（库侧存 UNIX 毫秒、读回是 UTC，展示层按 UTC 呈现）：期望值因此
	// 是不随机器时区漂移的整段字面量。非 UTC 时刻的转换由 S3 的显式非 UTC 夹具单独判。
	active := time.Date(2026, 10, 16, 9, 30, 5, 0, time.UTC)
	lister := &fakeSessionLister{sessions: []core.SessionRecord{{
		ID:           "cli-3f9a1c07b2d84e55",
		ProfileName:  "notes-agent",
		Channel:      "cli",
		UserID:       "yunwei",
		MessageCount: 6,
		CreatedAt:    active.Add(-time.Hour),
		LastActiveAt: active,
	}}}

	run := executeSession(t, sessionDeps(t.TempDir(), lister), "session", "list")

	require.NoError(t, run.err, "只读清单不该失败")
	assert.Equal(t,
		"cli-3f9a1c07b2d84e55：6 条（最后活动 2026-10-16 09:30:05）\n共 1 个会话\n",
		run.stdout, "渲染模板与汇总行")
	assert.Equal(t, []string{"ListSessions"}, lister.calls, "只读清单恰好一次调用")
}

// S2 空库文案：`未发现会话`（§6.2 固定口径 5）——空库是成功而不是失败，且**不**再补一行
// `共 0 个会话`：口径把空库文案与汇总写成并列的两种收场，叠加会给出两行同义文本。
// S2 reports an empty database with the fixed line and nothing else.
func TestSessionList_EmptyDatabaseReportsMissingSessions(t *testing.T) {
	for name, sessions := range map[string][]core.SessionRecord{
		"nil 切片": nil,
		"空切片":    {},
	} {
		t.Run(name, func(t *testing.T) {
			lister := &fakeSessionLister{sessions: sessions}
			run := executeSession(t, sessionDeps(t.TempDir(), lister), "session", "list")

			require.NoError(t, run.err, "空库不是失败")
			assert.Equal(t, "未发现会话\n", run.stdout, "空库文案")
		})
	}
}

// S3 顺序：渲染**保序**——命令面不重排（排序是实现侧职责，见 §3.2 的 N8；命令面再排一次
// 会把"库给的顺序"变成第二个顺序来源）。两个夹具连在一起判两件事：保序 + 时刻按 UTC 展示
// （第一个夹具是**显式非 UTC 时刻**，判"转换确实发生"而不只是"原样打印"）。
// S3 keeps the port's order and renders instants in UTC; the first fixture carries an
// explicit non-UTC zone so the case proves a real conversion, not a passthrough.
func TestSessionList_PreservesPortOrderAndShowsUTCTime(t *testing.T) {
	// `time.FixedZone` 是随夹具走的显式时区：不读机器设置、不依赖 tzdata，读出的期望值
	// 在任何开发机上一致（夹具若取机器本地时区，判卷输入里就多掺一项本机设置）。
	first := time.Date(2026, 10, 15, 22, 4, 8, 0, time.FixedZone("UTC+8", 8*60*60))
	second := time.Date(2026, 9, 1, 3, 7, 9, 0, time.UTC)

	lister := &fakeSessionLister{sessions: []core.SessionRecord{
		{ID: "cli-b", MessageCount: 2, LastActiveAt: first},
		{ID: "cli-a", MessageCount: 11, LastActiveAt: second},
	}}

	run := executeSession(t, sessionDeps(t.TempDir(), lister), "session", "list")

	require.NoError(t, run.err, "只读清单不该失败")
	assert.Equal(t,
		fmt.Sprintf("cli-b：2 条（最后活动 2026-10-15 14:04:08）\ncli-a：11 条（最后活动 %s）\n共 2 个会话\n",
			second.UTC().Format("2006-01-02 15:04:05")),
		run.stdout, "端口给的顺序原样呈现；非 UTC 时刻换算成 UTC 展示")
}

// S4 只读纪律与版本门槛：① 别的实例持锁时命令照常可用——命令面只发一个只读调用、
// 零写入；② `schema_version` 不匹配（只读路径在打开库之前校验）→ 拒绝且**无任何输出**。
// S4 judges the read-only discipline and the version gate.
func TestSessionList_ReadOnlyWhenLockedAndGatedOnSchemaVersion(t *testing.T) {
	t.Run("持锁时仍可用（单次只读调用、零写入）", func(t *testing.T) {
		lister := &fakeSessionLister{sessions: []core.SessionRecord{{
			ID: "cli-locked", MessageCount: 1, LastActiveAt: time.Date(2026, 10, 16, 1, 2, 3, 0, time.UTC),
		}}}
		// 假端口在这里扮演"另一个实例正持锁、库被占着"的世界：只读读得回来。
		// 真锁由装配层判（app/wire_test T1 / T4）；本用例判的是命令面没有第二个动作。
		run := executeSession(t, sessionDeps(t.TempDir(), lister), "session", "list")

		require.NoError(t, run.err, "持锁不影响只读命令")
		assert.Contains(t, run.stdout, "cli-locked：1 条", "清单照常渲染")
		// 恰一次 ListSessions：没有 Messages（恢复要读消息）、没有别的端口（其余是 nil，
		// 碰到即 panic——用例若红会以 panic 而不是断言失败的形式出现）。
		assert.Equal(t, []string{"ListSessions"}, lister.calls, "唯一触达的方法")
	})

	t.Run("schema_version 不匹配即拒绝", func(t *testing.T) {
		lister := &fakeSessionLister{sessionsErr: fmt.Errorf(
			"打开只读库 %s: %w", "basalt.db", core.ErrSchemaOutdated)}

		run := executeSession(t, sessionDeps(t.TempDir(), lister), "session", "list")

		require.Error(t, run.err, "版本不匹配必须拒绝")
		assert.ErrorIs(t, run.err, core.ErrSchemaOutdated, "哨兵错误经 %w 保链")
		assert.Empty(t, run.stdout, "拒绝时不产出半截清单")
		assert.Contains(t, run.logs, "读取会话清单失败", "失败路径留痕")
	})
}

// 编译期桩：假端口只在本文件用；留一个显式断言，端口形状变化时在这里先红。
var _ SessionLister = (*fakeSessionLister)(nil)
