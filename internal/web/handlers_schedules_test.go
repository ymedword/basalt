package web

// 本文件是定时任务管理面的 handler 级验收（颗粒度文档 §3.2.1，12 个函数 / 30 例）：
// 列表与执行历史的视图投影、PUT / run 的状态映射、两路 409 的冻结文案、可空列渲染
// 与错误原因投影（关键回归 R17 / R22 / R23 / R27）。
// Handler-level acceptance for the four /schedules endpoints: view projection, status
// mapping, frozen messages, nullable columns, and the public error-reason projection.

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// fakeScheduleService 是 ScheduleService 的假端口：记录每次调用参数，结果由用例预置。
// waitDeadline 为真时 RunNow 先等 ctx 截止再返回（与既有 deadlineRunService 同构）——
// 超时用例用它触达「运行 ctx 已过期」的两条判定路径。
// fakeScheduleService records every call; waitDeadline blocks until the run context
// expires, mirroring the existing deadlineRunService fixture.
type fakeScheduleService struct {
	mu sync.Mutex

	tasks     []core.TaskRecord
	listErr   error
	listCalls int

	setCalls []enabledCall
	setErr   error

	executions       []core.ExecutionRecord
	executionsTaskID string
	executionsLimit  int
	executionsErr    error

	runReq       core.RunRequest
	runRes       core.RunResult
	runErr       error
	runCalls     int
	runTaskID    string
	waitDeadline bool
}

// enabledCall 记录一次 SetEnabled 的入参。
type enabledCall struct {
	taskID  string
	enabled bool
}

func (f *fakeScheduleService) List(context.Context) ([]core.TaskRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tasks, nil
}

func (f *fakeScheduleService) SetEnabled(_ context.Context, taskID string, enabled bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls = append(f.setCalls, enabledCall{taskID: taskID, enabled: enabled})
	return f.setErr
}

func (f *fakeScheduleService) Executions(
	_ context.Context,
	taskID string,
	limit int,
) ([]core.ExecutionRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.executionsTaskID = taskID
	f.executionsLimit = limit
	if f.executionsErr != nil {
		return nil, f.executionsErr
	}
	return f.executions, nil
}

func (f *fakeScheduleService) RunNow(
	ctx context.Context,
	taskID string,
) (core.RunRequest, core.RunResult, error) {
	f.mu.Lock()
	f.runCalls++
	f.runTaskID = taskID
	wait := f.waitDeadline
	req, res, err := f.runReq, f.runRes, f.runErr
	f.mu.Unlock()

	if wait {
		<-ctx.Done()
	}
	if err != nil {
		return core.RunRequest{}, core.RunResult{}, err
	}
	return req, res, nil
}

func (f *fakeScheduleService) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeScheduleService) setSnapshot() []enabledCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]enabledCall(nil), f.setCalls...)
}

func (f *fakeScheduleService) executionsArgs() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.executionsTaskID, f.executionsLimit
}

func (f *fakeScheduleService) runCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCalls
}

// scheduleRequest 发一次管理面请求；body 为空串时等价于无请求体。
// scheduleRequest issues one management request through the real handler chain.
func scheduleRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// decodeScheduleEnvelope 解统一信封；data 字段由调用方按端点类型再解。
// decodeScheduleEnvelope decodes the shared envelope; callers decode data by endpoint.
func decodeScheduleEnvelope(t *testing.T, rec *httptest.ResponseRecorder) Envelope {
	t.Helper()
	var env Envelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	return env
}

// TestSchedules_ListShapeAndOrder 覆盖列表端点的信封形态、视图投影与顺序（3 例）。
// 逆改点：漏投影 zone / message / last_* 字段、时间不用 RFC3339 UTC、web 层重排
// 或自行增删行 → 对应断言红。
func TestSchedules_ListShapeAndOrder(t *testing.T) {
	t.Run("envelope and view projection", func(t *testing.T) {
		svc := &fakeScheduleService{tasks: []core.TaskRecord{{
			TaskID:     "demo-1",
			Agent:      "demo",
			Cron:       "0 8 * * * *",
			Zone:       "Asia/Shanghai",
			Message:    "巡检",
			Enabled:    true,
			LastRunAt:  time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC),
			LastStatus: core.ExecutionCompleted,
		}}}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodGet, "/api/v1/schedules", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Code      string         `json:"code"`
			Message   string         `json:"message"`
			Timestamp string         `json:"timestamp"`
			Data      []ScheduleView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, messageSuccess, env.Message)
		require.Equal(t, "2026-10-18T12:00:00Z", env.Timestamp)
		require.Equal(t, []ScheduleView{{
			ID:         "demo-1",
			Agent:      "demo",
			Cron:       "0 8 * * * *",
			Zone:       "Asia/Shanghai",
			Message:    "巡检",
			Enabled:    true,
			LastRunAt:  "2026-10-18T11:00:00Z",
			LastStatus: "completed",
		}}, env.Data)
	})

	t.Run("order follows the service", func(t *testing.T) {
		svc := &fakeScheduleService{tasks: []core.TaskRecord{
			{TaskID: "alpha-1"},
			{TaskID: "demo-10"},
			{TaskID: "demo-2"},
		}}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodGet, "/api/v1/schedules", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Data []ScheduleView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		ids := make([]string, 0, len(env.Data))
		for _, view := range env.Data {
			ids = append(ids, view.ID)
		}
		require.Equal(t, []string{"alpha-1", "demo-10", "demo-2"}, ids,
			"task_id 升序由仓储 SQL 保证，web 层不得重排")
	})

	t.Run("hidden rows are filtered by the service", func(t *testing.T) {
		// retired / Agent 未注册的行由 scheduler 的共用可见性判定过滤；web 只透传，
		// 不自行补行也不二次过滤。
		svc := &fakeScheduleService{tasks: []core.TaskRecord{{TaskID: "demo-1"}}}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodGet, "/api/v1/schedules", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Data []ScheduleView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Len(t, env.Data, 1)
		require.Equal(t, "demo-1", env.Data[0].ID)
		require.Equal(t, 1, svc.listCallCount(), "列表只读一次服务，不重复查询")
	})
}

// TestSchedules_SetEnabled 覆盖启停端点的四态（4 例）：启用 200 无 data、停用、
// 缺字段 400、未知任务 404。
// 逆改点：成功响应带 data → 断言红；缺 enabled 仍调服务 → 调用计数断言红；
// 把 fs.ErrNotExist 折成 500 → 404 断言红。
func TestSchedules_SetEnabled(t *testing.T) {
	t.Run("enable returns 200 without data", func(t *testing.T) {
		svc := &fakeScheduleService{}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodPut, "/api/v1/schedules/demo-1", `{"enabled":true}`)
		require.Equal(t, http.StatusOK, rec.Code)

		env := decodeScheduleEnvelope(t, rec)
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, messageSuccess, env.Message)
		require.Nil(t, env.Data, "PUT 成功与归档同款：唯一无载荷成功")
		require.NotContains(t, rec.Body.String(), `"data"`)
		require.Equal(t, []enabledCall{{taskID: "demo-1", enabled: true}}, svc.setSnapshot())
	})

	t.Run("disable passes enabled=false", func(t *testing.T) {
		svc := &fakeScheduleService{}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodPut, "/api/v1/schedules/demo-1", `{"enabled":false}`)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, codeOK, decodeScheduleEnvelope(t, rec).Code)
		require.Equal(t, []enabledCall{{taskID: "demo-1", enabled: false}}, svc.setSnapshot())
	})

	t.Run("missing or null enabled is 400", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"enabled":null}`} {
			svc := &fakeScheduleService{}
			srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

			rec := scheduleRequest(srv, http.MethodPut, "/api/v1/schedules/demo-1", body)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", body)

			env := decodeScheduleEnvelope(t, rec)
			require.Equal(t, codeBadRequest, env.Code)
			require.Nil(t, env.Data)
			require.Zero(t, svc.setSnapshot(),
				fmt.Sprintf("参数校验失败不得触达服务（body=%s）", body))
		}
	})

	t.Run("unknown task is 404", func(t *testing.T) {
		svc := &fakeScheduleService{
			setErr: fmt.Errorf("scheduler: task %q: %w", "ghost", fs.ErrNotExist),
		}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodPut, "/api/v1/schedules/ghost", `{"enabled":true}`)
		require.Equal(t, http.StatusNotFound, rec.Code)

		env := decodeScheduleEnvelope(t, rec)
		require.Equal(t, codeNotFound, env.Code)
		require.Nil(t, env.Data)
	})
}

// TestSchedules_Executions 覆盖执行历史端点（4 例）：形态与倒序、limit=100、
// 未知任务 404、可空列（started 行的 run_id / finished_at）投影为空串。
// 逆改点：不传固定窗口 → limit 断言红；模板直写零值时间 → 输出 0001-01-01，断言红。
func TestSchedules_Executions(t *testing.T) {
	t.Run("execution shape and order", func(t *testing.T) {
		started := time.Date(2026, 10, 18, 11, 30, 0, 0, time.UTC)
		svc := &fakeScheduleService{executions: []core.ExecutionRecord{
			{
				ID: 2, TaskID: "demo-1", RunID: "scheduler-2", Status: core.ExecutionCompleted,
				Trigger: core.TriggerManual, DurationMS: 1000,
				StartedAt: started, FinishedAt: started.Add(time.Second),
			},
			{
				ID: 1, TaskID: "demo-1", RunID: "scheduler-1", Status: core.ExecutionCompleted,
				Trigger: core.TriggerCron, DurationMS: 800,
				StartedAt: started.Add(-time.Hour), FinishedAt: started.Add(-time.Hour + time.Second),
			},
		}}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodGet, "/api/v1/schedules/demo-1/executions", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Code string          `json:"code"`
			Data []ExecutionView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, []ExecutionView{
			{
				TaskID: "demo-1", RunID: "scheduler-2", Status: "completed", Trigger: "manual",
				DurationMS: 1000, ErrorMessage: "",
				StartedAt: "2026-10-18T11:30:00Z", FinishedAt: "2026-10-18T11:30:01Z",
			},
			{
				TaskID: "demo-1", RunID: "scheduler-1", Status: "completed", Trigger: "cron",
				DurationMS: 800, ErrorMessage: "",
				StartedAt: "2026-10-18T10:30:00Z", FinishedAt: "2026-10-18T10:30:01Z",
			},
		}, env.Data, "固定按 started_at 倒序，web 层保持服务顺序")
	})

	t.Run("history window is 100", func(t *testing.T) {
		svc := &fakeScheduleService{}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodGet, "/api/v1/schedules/demo-1/executions", "")
		require.Equal(t, http.StatusOK, rec.Code)

		taskID, limit := svc.executionsArgs()
		require.Equal(t, "demo-1", taskID)
		require.Equal(t, 100, limit, "执行历史固定窗口 scheduleHistoryLimit = 100")
	})

	t.Run("unknown task is 404", func(t *testing.T) {
		svc := &fakeScheduleService{
			executionsErr: fmt.Errorf("scheduler: task %q: %w", "ghost", fs.ErrNotExist),
		}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodGet, "/api/v1/schedules/ghost/executions", "")
		require.Equal(t, http.StatusNotFound, rec.Code)

		env := decodeScheduleEnvelope(t, rec)
		require.Equal(t, codeNotFound, env.Code)
		require.Nil(t, env.Data)
	})

	t.Run("nullable columns render as empty strings", func(t *testing.T) {
		// 契约 schema.sql 的可空列：started 行的 run_id / finished_at 均为 NULL。
		// 视图必须输出空串，不得把 time.Time 零值写成 0001-01-01T00:00:00Z（R22/H1）。
		started := time.Date(2026, 10, 18, 11, 30, 0, 0, time.UTC)
		svc := &fakeScheduleService{executions: []core.ExecutionRecord{{
			ID: 1, TaskID: "demo-1", RunID: "", Status: core.ExecutionStarted,
			Trigger: core.TriggerCron, StartedAt: started,
		}}}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodGet, "/api/v1/schedules/demo-1/executions", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Data []ExecutionView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Len(t, env.Data, 1)
		require.Equal(t, "", env.Data[0].RunID)
		require.Equal(t, "", env.Data[0].FinishedAt)
		require.Equal(t, "2026-10-18T11:30:00Z", env.Data[0].StartedAt)
		require.NotContains(t, rec.Body.String(), "0001-01-01", "零值时间不得出现在响应体")
	})
}

// TestSchedules_RunSuccess 覆盖手动补跑的成功面（3 例）：200 RunView 且
// trigger=manual、LIMIT_REACHED 的 200 形态、completed 的运行摘要。
// 逆改点：trigger 写死 api → manual 断言红；limit_reached 落成 OK → 错误码断言红。
func TestSchedules_RunSuccess(t *testing.T) {
	manualReq := core.RunRequest{
		RunID:     "scheduler-26ab61a473f25490",
		Agent:     "demo",
		SessionID: "scheduler-26ab61a473f25490",
		Message:   "巡检",
		Channel:   "scheduler",
		UserID:    "scheduler",
		Trigger:   core.TriggerManual,
	}

	t.Run("manual trigger projected in run view", func(t *testing.T) {
		svc := &fakeScheduleService{
			runReq: manualReq,
			runRes: core.RunResult{RunID: manualReq.RunID, Status: core.StatusCompleted},
		}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Code string  `json:"code"`
			Data RunView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeOK, env.Code)
		require.Equal(t, manualReq.RunID, env.Data.RunID)
		require.Equal(t, manualReq.SessionID, env.Data.SessionID)
		require.Equal(t, "demo", env.Data.Agent)
		require.Equal(t, "scheduler", env.Data.Channel)
		require.Equal(t, "manual", env.Data.Trigger,
			"运行视图的 trigger 必须来自 RunRequest（手动补跑为 manual）")
	})

	t.Run("limit reached keeps 200 with LIMIT_REACHED", func(t *testing.T) {
		svc := &fakeScheduleService{
			runReq: manualReq,
			runRes: core.RunResult{
				RunID:  manualReq.RunID,
				Status: core.StatusLimitReached,
				Reason: core.ReasonIterationLimit,
			},
		}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Code    string  `json:"code"`
			Message string  `json:"message"`
			Data    RunView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeLimitReached, env.Code)
		require.Contains(t, env.Message, core.ReasonIterationLimit)
		require.Contains(t, env.Message, "budget.max_iterations", "契约要求点名可调配置键")
		require.Equal(t, string(core.StatusLimitReached), env.Data.Status)
		require.Equal(t, "manual", env.Data.Trigger)
	})

	t.Run("completed status carries the run summary", func(t *testing.T) {
		svc := &fakeScheduleService{
			runReq: manualReq,
			runRes: core.RunResult{
				RunID:      manualReq.RunID,
				Status:     core.StatusCompleted,
				FinalText:  "巡检完成",
				Iterations: 2,
				ToolCalls:  1,
				ConfigHash: "cfg-1",
			},
		}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
		require.Equal(t, http.StatusOK, rec.Code)

		var env struct {
			Data RunView `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, string(core.StatusCompleted), env.Data.Status)
		require.Equal(t, "巡检完成", env.Data.FinalText)
		require.Equal(t, 2, env.Data.Iterations)
		require.Equal(t, 1, env.Data.ToolCalls)
		require.Equal(t, "cfg-1", env.Data.ConfigHash)
		require.Equal(t, "2026-10-18T12:00:00Z", env.Data.StartedAt)
		require.Equal(t, "2026-10-18T12:00:00Z", env.Data.FinishedAt)
	})
}

// TestSchedules_RunConflict 逐字保留模块卡 §4.3 的两个子例（2 例）：web 层只断言
// 状态码 / 错误码 / 冻结文案；「零执行行 / 恰一行 skipped」由 scheduler 层的
// TestScheduler_RunNow_BusyReturnsErrTaskBusy / _SessionBusyRecordsSkipped 断言。
// 逆改点：不区分 ErrTaskBusy（直接 writeRunError）→ 错误码断言红；把 messageTaskBusy
// 改成 messageBusy → 逐字红；把兄弟任务占会话的既有 messageBusy 换成 messageTaskBusy
// → 逐字红。
func TestSchedules_RunConflict(t *testing.T) {
	t.Run("task lock conflict maps to 409", func(t *testing.T) {
		svc := &fakeScheduleService{runErr: fmt.Errorf("%w: demo-1", core.ErrTaskBusy)}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
		require.Equal(t, http.StatusConflict, rec.Code)

		var env Envelope
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeSessionBusy, env.Code)
		require.Equal(t, messageTaskBusy, env.Message)
		require.Nil(t, env.Data)
	})

	t.Run("sibling session busy reuses frozen message", func(t *testing.T) {
		svc := &fakeScheduleService{runErr: &core.SessionBusyError{SessionID: "scheduler:scheduler:demo"}}
		srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

		rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
		require.Equal(t, http.StatusConflict, rec.Code)

		var env Envelope
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
		require.Equal(t, codeSessionBusy, env.Code)
		require.Equal(t, messageBusy, env.Message, "复用既有会话忙冻结文案")
		require.Nil(t, env.Data)
	})
}

// TestSchedules_RunNotFound 覆盖未知 / 不可见任务的 404 映射（1 例）。
// 逆改点：fs.ErrNotExist 折成 500 或 409 → 状态码断言红。
func TestSchedules_RunNotFound(t *testing.T) {
	svc := &fakeScheduleService{
		runErr: fmt.Errorf("scheduler: task %q: %w", "ghost", fs.ErrNotExist),
	}
	srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

	rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/ghost/run", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	env := decodeScheduleEnvelope(t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Nil(t, env.Data)
	require.Equal(t, 1, svc.runCallCount())
}

// TestSchedules_RunTimeout 覆盖 504 的两条判定路径（2 例）：err 为截止错误；
// err == nil 但运行 ctx 已截止。
// 逆改点：成功路径漏判截止 → 返回 200 视图，断言红。
func TestSchedules_RunTimeout(t *testing.T) {
	t.Run("deadline error maps to 504", func(t *testing.T) {
		svc := &fakeScheduleService{waitDeadline: true, runErr: context.DeadlineExceeded}
		srv := newTestServer(t, func(o *Options) {
			o.RunTimeout = 10 * time.Millisecond
			o.Schedules = svc
		})

		rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
		require.Equal(t, http.StatusGatewayTimeout, rec.Code)

		env := decodeScheduleEnvelope(t, rec)
		require.Equal(t, codeTimeout, env.Code)
		require.Nil(t, env.Data)
	})

	t.Run("nil error after deadline still maps to 504", func(t *testing.T) {
		svc := &fakeScheduleService{
			waitDeadline: true,
			runRes:       core.RunResult{RunID: "run-timeout", Status: core.StatusCompleted},
		}
		srv := newTestServer(t, func(o *Options) {
			o.RunTimeout = 10 * time.Millisecond
			o.Schedules = svc
		})

		rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
		require.Equal(t, http.StatusGatewayTimeout, rec.Code, "截止已到不得写 200 + 视图")

		env := decodeScheduleEnvelope(t, rec)
		require.Equal(t, codeTimeout, env.Code)
		require.Nil(t, env.Data)
	})
}

// TestSchedules_ExecutionMessageProjection 覆盖约束 34（R10-H2）的四类输入（4 例）。
// 逆改点：skipped 一律映射为通用文案 → ② 断言红；用子串 / 前缀匹配 → ④ 误判，断言红。
func TestSchedules_ExecutionMessageProjection(t *testing.T) {
	t.Run("failed projects to the fixed public text", func(t *testing.T) {
		raw := "open /home/ops/private/agent.md: permission denied"
		view := executionView(core.ExecutionRecord{Status: core.ExecutionFailed, ErrorMessage: raw})
		require.Equal(t, "运行失败", view.ErrorMessage)
		require.NotContains(t, view.ErrorMessage, "/home/ops", "原始原因永不出站")
	})

	t.Run("archived skip keeps the frozen text", func(t *testing.T) {
		view := executionView(core.ExecutionRecord{
			Status:       core.ExecutionSkipped,
			ErrorMessage: "会话已归档，本轮跳过",
		})
		require.Equal(t, "会话已归档，本轮跳过", view.ErrorMessage, "D-21：归档跳过沿用冻结文案")
	})

	t.Run("sibling session busy projects to the generic skip text", func(t *testing.T) {
		view := executionView(core.ExecutionRecord{
			Status:       core.ExecutionSkipped,
			ErrorMessage: "同一 Agent 的另一任务仍在执行",
		})
		require.Equal(t, "上一轮尚未结束，本轮跳过", view.ErrorMessage)
	})

	t.Run("unknown skip reason never leaves the handler", func(t *testing.T) {
		raw := "scheduler: task busy: demo-1"
		view := executionView(core.ExecutionRecord{Status: core.ExecutionSkipped, ErrorMessage: raw})
		require.Equal(t, "上一轮尚未结束，本轮跳过", view.ErrorMessage)
		require.NotContains(t, view.ErrorMessage, "task busy", "未识别原文一律落回通用文案")
	})
}

// TestSchedules_DeliveryProjection：投递结果与执行结果**分开**投影——"任务完成 /
// 飞书投递失败"必须能同时看到；没有投递记录时三项均为空串，不臆造状态（R7）。
// The delivery verdict is projected separately from the execution status; a row
// without a delivery record renders empty strings instead of an invented status.
func TestSchedules_DeliveryProjection(t *testing.T) {
	view := executionView(core.ExecutionRecord{
		Status:          core.ExecutionCompleted,
		Delivery:        core.DeliveryFailed,
		DeliveryChannel: "rs_deliverytarget00000000000000",
		DeliveryDetail:  "飞书返回 code=19001",
	})
	require.Equal(t, "completed", view.Status)
	require.Equal(t, "failed", view.DeliveryStatus)
	require.Equal(t, "rs_deliverytarget00000000000000", view.DeliveryChannel)
	require.Equal(t, "飞书返回 code=19001", view.DeliveryDetail)

	plain := executionView(core.ExecutionRecord{Status: core.ExecutionCompleted})
	require.Empty(t, plain.DeliveryStatus, "无投递记录不得投影成任何状态")
	require.Empty(t, plain.DeliveryChannel)
	require.Empty(t, plain.DeliveryDetail)
}

// TestSchedules_ScheduleViewCarriesDeliverTo：任务列表回显系统代投目标（资源 UID）；
// 未绑定时为空串。
func TestSchedules_ScheduleViewCarriesDeliverTo(t *testing.T) {
	view := scheduleView(core.TaskRecord{
		TaskID:    "demo-1",
		Agent:     "reporter",
		DeliverTo: "rs_deliverytarget00000000000000",
	})
	require.Equal(t, "rs_deliverytarget00000000000000", view.DeliverTo)
	require.Empty(t, scheduleView(core.TaskRecord{TaskID: "demo-2"}).DeliverTo)
}

// TestSchedules_MethodNotAllowed404 覆盖方法不匹配落到既有 404 信封（1 例）：
// GET 命中 POST-only 的 /run 不得返回 ServeMux 的纯文本 405。
// 逆改点：外层的 catch-all 被移除 → ServeMux 返回 405 纯文本，断言红。
func TestSchedules_MethodNotAllowed404(t *testing.T) {
	srv := newTestServer(t, nil)

	rec := scheduleRequest(srv, http.MethodGet, "/api/v1/schedules/demo-1/run", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	env := decodeScheduleEnvelope(t, rec)
	require.Equal(t, codeNotFound, env.Code)
	require.Nil(t, env.Data)
}

// TestSchedules_RunTooManyRuns 覆盖实例准入上限的完整复用（1 例）：429 + TOO_MANY_RUNS。
// 逆改点：把准入错误一律折成 500 → 断言红。
func TestSchedules_RunTooManyRuns(t *testing.T) {
	svc := &fakeScheduleService{runErr: &core.InstanceFullError{Limit: 3}}
	srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

	rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)

	env := decodeScheduleEnvelope(t, rec)
	require.Equal(t, codeTooManyRuns, env.Code)
	require.Equal(t, "实例并发上限 3，请稍后重试", env.Message)
	require.Nil(t, env.Data)
}

// TestSchedules_NilServiceReturns500 覆盖装配缺失的兜底（4 例）：Options.Schedules
// 为 nil 时四个端点均 500、INTERNAL、固定文案、无 data、不 panic。
// 逆改点：nil 分支按 404 或空列表 200 返回 → 状态码 / 信封断言红。
func TestSchedules_NilServiceReturns500(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"list", http.MethodGet, "/api/v1/schedules", ""},
		{"set enabled", http.MethodPut, "/api/v1/schedules/demo-1", `{"enabled":true}`},
		{"run", http.MethodPost, "/api/v1/schedules/demo-1/run", ""},
		{"executions", http.MethodGet, "/api/v1/schedules/demo-1/executions", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, nil) // Schedules 保持零值 nil

			rec := scheduleRequest(srv, tc.method, tc.path, tc.body)
			require.Equal(t, http.StatusInternalServerError, rec.Code,
				"装配缺失是错误，不是 404、也不是空列表")

			env := decodeScheduleEnvelope(t, rec)
			require.Equal(t, codeInternal, env.Code)
			require.Equal(t, messageInternal, env.Message)
			require.Nil(t, env.Data)
			require.NotContains(t, rec.Body.String(), `"data"`)
		})
	}
}

// TestSchedules_RunArchived400 覆盖约束 43（D-21）：service 返回
// *core.SessionArchivedError → 400 BAD_REQUEST + 冻结文案，无 data。
// 逆改点：折成 409 / 500 或复用会话忙文案 → 状态码 / 文案断言红。
func TestSchedules_RunArchived400(t *testing.T) {
	svc := &fakeScheduleService{
		runErr: &core.SessionArchivedError{SessionID: "scheduler-26ab61a473f25490"},
	}
	srv := newTestServer(t, func(o *Options) { o.Schedules = svc })

	rec := scheduleRequest(srv, http.MethodPost, "/api/v1/schedules/demo-1/run", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	env := decodeScheduleEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Equal(t, "会话已归档，不可追加消息", env.Message, "冻结文案逐字取自约束 43")
	require.Nil(t, env.Data)
}

// TestAuth_ScheduleEndpoints 覆盖调度四端点的认证与 409 映射（任务 T024 的 +3）：
// 无凭据一律 401 且零端口调用、有效凭据放行、RunNow 返回生命周期不可用 → 409
// `AGENT_UNAVAILABLE`（与 messages / invoke 同一张错误映射表）。
// 逆改点：只给部分调度端点挂闸门 / 把 AGENT_UNAVAILABLE 落回 500 → 对应子例红。
// 中文用例名：调度端点需认证。
func TestAuth_ScheduleEndpoints(t *testing.T) {
	endpoints := []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/schedules", ""},
		{http.MethodPut, "/api/v1/schedules/demo-1", `{"enabled":true}`},
		{http.MethodGet, "/api/v1/schedules/demo-1/executions", ""},
		{http.MethodPost, "/api/v1/schedules/demo-1/run", ""},
	}

	t.Run("missing credentials are rejected on all four endpoints", func(t *testing.T) {
		svc := &fakeScheduleService{}
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.Schedules = svc
		})
		for _, tc := range endpoints {
			rec := serveAdmin(s, adminRequest(tc.method, tc.path, tc.body))
			require.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s", tc.method, tc.path)
			require.Equal(t, codeUnauthorized, decodeScheduleEnvelope(t, rec).Code)
		}
		require.Zero(t, svc.listCallCount(), "未认证请求不得触达调度器")
		require.Empty(t, svc.setSnapshot(), "未认证请求不得改启停")
		require.Zero(t, svc.runCallCount(), "未认证请求不得触发补跑")
	})

	t.Run("valid credentials reach every schedule handler", func(t *testing.T) {
		svc := &fakeScheduleService{
			runRes: core.RunResult{RunID: "run-1", Status: core.StatusCompleted},
		}
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.Schedules = svc
		})
		for _, tc := range endpoints {
			rec := serveAdmin(s, withBearer(adminRequest(tc.method, tc.path, tc.body), testToken))
			require.Equal(t, http.StatusOK, rec.Code, "%s %s; body=%s",
				tc.method, tc.path, rec.Body.String())
		}
		require.Equal(t, 1, svc.listCallCount())
		require.Len(t, svc.setSnapshot(), 1)
		require.Equal(t, 1, svc.runCallCount())
	})

	t.Run("lifecycle refusal on run-now maps to 409", func(t *testing.T) {
		svc := &fakeScheduleService{runErr: &core.AgentUnavailableError{
			Name:   "ops-agent",
			State:  core.AgentDisabled,
			Reason: "lifecycle state disabled; enable the agent first",
		}}
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.Schedules = svc
		})
		rec := serveAdmin(s,
			withBearer(adminRequest(http.MethodPost, "/api/v1/schedules/demo-1/run", ""), testToken))
		require.Equal(t, http.StatusConflict, rec.Code)
		env := decodeScheduleEnvelope(t, rec)
		require.Equal(t, codeAgentUnavailable, env.Code)
		require.Contains(t, env.Message, "ops-agent")
	})
}
