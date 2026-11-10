//go:build integration

package app

// 本文件落 T040 的跨入口协调验收（颗粒度文档 §3.2.1 的 app/crossentry_test.go，
// 夹具与用例骨架见 §4.3）：真实 AgentService + 真实 SQLite + 真实 HTTP 面 +
// 真实 core.Scheduler 的全链路，只把模型客户端（gateClient 语义）与 cron 引擎
// （capturingEngine）换成可控替身。R27：归档与在途运行的互斥不得因触发源
// （cron / 手动补跑）而分叉——两条入口都经唯一的 AgentService.Run 与 Archive 判定。
// Cross-entry coordination acceptance: the real service, SQLite, HTTP face and
// scheduler; only the model client and cron engine are controllable stand-ins.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
)

// crossEntryEngine 是 CronEngine 的最小替身：本夹具不解析 cron 表达式（解析 / 枚举由
// cron_test.go 覆盖）。US-24 起触发权属于数据库——引擎不再登记作业，测试改由拨动
// `next_fire_at` 制造到期（见 Fire），因此这里只剩"下一次 / 区间"两个纯计算方法。
// A minimal CronEngine stand-in: the trigger right moved into the database, so the test
// makes a task due via next_fire_at instead of capturing a registered job.
type crossEntryEngine struct{}

// Next 把下一次触发固定推后一小时：用例只关心"这一笔到期是否被领取"，不关心节奏。
// Next pushes the next occurrence one hour out; the fixture only cares about one claim.
func (crossEntryEngine) Next(_ string, from time.Time) (time.Time, error) {
	return from.Add(time.Hour), nil
}

// Occurrences 返回空集：错过触发点的计数不在本文件覆盖。
// Occurrences returns nothing: missed-trigger accounting is not exercised here.
func (crossEntryEngine) Occurrences(string, time.Time, time.Time, int) []time.Time { return nil }

// crossEntryWorker 是本夹具的 Worker 身份：调度器领取执行权必须带真实归属
// （空实例标识不是"轻装配"，而是装配缺陷）。
// crossEntryWorker is the fixture's worker identity; the scheduler refuses to run
// without one because a lease needs a real owner.
var crossEntryWorker = core.WorkerIdentity{
	InstanceID:    "crossentry-worker",
	Name:          "crossentry",
	Hostname:      "localhost",
	Version:       "test",
	SchemaVersion: 12,
}

// crossEntryEnv 是跨入口用例的全链路夹具：真实库、真实运行入口、真实调度器与
// 真实 HTTP 面；gate 状态用于制造一次可观测的在途运行窗口。
// crossEntryEnv wires the real store, run entry, scheduler and HTTP face; the gate
// state creates one observable in-flight run window.
type crossEntryEnv struct {
	t        *testing.T
	db       *postgres.DB
	sessions *postgres.SessionStore
	store    core.TaskStore
	svc      *core.AgentService
	sched    *core.Scheduler
	srv      *web.Server
	engine   core.CronEngine
	baseURL  string
	taskID   string
	// fireSeq 让每次 Fire 拿到互不相同的 planned_at：`(task_id, planned_at)` 去重
	// 只认这一对，同一毫秒两次触发会被判成"已领取"而静默跳过。
	fireSeq atomic.Int64

	gateMu   sync.Mutex
	pending  bool
	entered  chan struct{}
	release  chan struct{}
	released bool

	bodyMu sync.Mutex
	bodies map[*http.Response]string
	client *http.Client
}

// newCrossEntryEnv 组装夹具：工作区 AGENT.md 落盘（Snapshot 每次重读，§2.2 约束 7）、
// 临时库显式迁移、真实仓储 / 运行入口 / 调度器（Start 触发对账与注册）与监听在
// 127.0.0.1:0 的真实 HTTP 服务。CronSpec 返回 task id，作业由 Fire 驱动。
// newCrossEntryEnv assembles the fixture: a real workspace file, an explicitly
// migrated temporary database, the production stores and entry point, a started
// scheduler, and a real HTTP server; CronSpec returns the task id.
func newCrossEntryEnv(t *testing.T, entries ...core.AgentEntry) *crossEntryEnv {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	for _, entry := range entries {
		writeAgentFixture(t, root, entry.Name)
	}

	db := openScheduleDB(t)
	sessions := postgres.NewSessionStore(db)
	invocations := postgres.NewToolInvocationRepo(db)
	registry := tool.NewRegistry(logger)
	loop := core.NewReActLoop(
		core.NewPromptBuilder(registry),
		core.NewToolExecutor(registry, invocations, logger),
		logger,
	)

	env := &crossEntryEnv{
		t:        t,
		db:       db,
		sessions: sessions,
		store:    postgres.NewTaskStore(db),
		engine:   crossEntryEngine{},
		bodies:   make(map[*http.Response]string),
		client:   &http.Client{Timeout: 30 * time.Second},
	}
	for _, entry := range entries {
		if entry.Profile != nil && len(entry.Profile.Schedules) > 0 {
			env.taskID = entry.Profile.Schedules[0].ID
			break
		}
	}
	require.NotEmpty(t, env.taskID, "夹具必须有至少一条调度定义")

	// 账本只建一次：AgentService 写、web 适配器读（与生产装配同一条纪律）。
	// One store instance: AgentService writes, the web adapter reads.
	runs := postgres.NewRunStore(db)
	env.svc = core.NewAgentService(core.ServiceOptions{
		Agents:        newAgentRegistry(root, entries, logger),
		Client:        providerClientFunc(env.chat),
		Messages:      sessions,
		Sessions:      sessions,
		Runs:          runs,
		Snapshots:     postgres.NewSnapshotStore(db),
		Audit:         invocations,
		Loop:          loop,
		Logger:        logger,
		MaxConcurrent: 4,
	})

	env.sched = core.NewScheduler(core.SchedulerOptions{
		Store:      env.store,
		Invoker:    env.svc,
		Engine:     env.engine,
		Entries:    entries,
		CronSpec:   func(s core.Schedule) (string, error) { return s.ID, nil },
		Budget:     core.Budget{MaxIterations: 2, MaxToolCalls: 2, ContextTokens: 4096},
		RunTimeout: scheduleRunTimeout,
		StopGrace:  scheduleStopGrace,
		Logger:     logger,
		// US-24：触发权在数据库，调度器必须有真实归属与续约端口才能启动
		// （空装配在 Start 处失败关闭）。轮询间隔取小值，让测试不必等 500ms。
		Leases:        postgres.NewLeaseStore(db),
		Worker:        crossEntryWorker,
		LeaseTTL:      60 * time.Second,
		RenewInterval: 20 * time.Second,
		PollInterval:  20 * time.Millisecond,
		// 宽限放宽到 10 分钟：夹具从不验证"错过窗口"（那在 core / postgres 用例里），
		// 只要求本机与容器之间的时钟偏差不会把一次正常触发误判成错失。
		MissedGrace: 10 * time.Minute,
		// 夹具建模控制面实例：启动对账照常写共享定义列（US-25C T033 起的显式开关）。
		DefinitionAuthority: true,
	})
	require.NoError(t, env.sched.Start(context.Background()))
	t.Cleanup(env.sched.Stop)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	env.srv, err = web.NewServer(web.Options{
		Addr:          ln.Addr().String(),
		Listener:      ln,
		RunBudget:     core.Budget{MaxIterations: 2, MaxToolCalls: 2, ContextTokens: 4096},
		ShutdownGrace: 5 * time.Second,
		Logger:        logger,
		Runs:          NewRunService(env.svc, runs),
		Sessions:      sessions,
		Agents:        agentProfilesPort{entries: entries},
		Tools:         newToolCatalogPort(registry, nil),
		Memory:        emptyMemorySections{},
		Schedules:     env.sched,
		Console:       assets.Console(),
	})
	require.NoError(t, err)
	env.baseURL = "http://" + ln.Addr().String()

	runCtx, cancelRun := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- env.srv.Run(runCtx) }()
	t.Cleanup(func() {
		cancelRun()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("HTTP 服务未在时限内收尾")
		}
	})
	// gate 释放注册在**最后**（Cleanup 后进先出）：失败路径下先解开阻塞的 Chat，
	// 再等待 HTTP / 调度器收尾，避免在途 handler 拖住停机。
	// The gate release is registered last so LIFO runs it first on failure.
	t.Cleanup(env.releaseGate)
	return env
}

// providerClientFunc 让一个函数满足 core.ProviderClient（显式端口适配）。
// providerClientFunc adapts a function to core.ProviderClient.
type providerClientFunc func(context.Context, core.ChatRequest) (core.ChatResponse, error)

// Chat 转调底层函数。
// Chat dispatches to the wrapped function.
func (f providerClientFunc) Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error) {
	return f(ctx, req)
}

// chat 是 gateClient 语义的唯一实现：被 blockNextRun 标记的下一次调用先发"已进入"
// 信号再阻塞在 gate 上，其余调用立即返回纯文本完成响应（无工具调用 → 运行完成）。
// chat implements the gate semantic: the next call after blockNextRun signals entry
// and blocks on the gate; all other calls complete immediately with plain text.
func (e *crossEntryEnv) chat(ctx context.Context, _ core.ChatRequest) (core.ChatResponse, error) {
	e.gateMu.Lock()
	blocked := e.pending
	var entered, release chan struct{}
	if blocked {
		e.pending = false
		entered, release = e.entered, e.release
	}
	e.gateMu.Unlock()

	if blocked {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return core.ChatResponse{}, ctx.Err()
		}
	}
	return core.ChatResponse{Content: "巡检完成"}, nil
}

// blockNextRun 让下一次模型调用阻塞在 gate 上（制造在途运行窗口）。
// blockNextRun makes the next model call block on the gate.
func (e *crossEntryEnv) blockNextRun() {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	e.pending = true
	e.entered = make(chan struct{})
	e.release = make(chan struct{})
	e.released = false
}

// ReleaseRun 放行被 gate 拦住的模型调用（幂等）。
// ReleaseRun opens the gate; repeated calls are no-ops.
func (e *crossEntryEnv) ReleaseRun() {
	e.releaseGate()
}

// releaseGate 是 ReleaseRun 与失败清理共用的幂等放行口。
// releaseGate is the idempotent release shared by ReleaseRun and cleanup.
func (e *crossEntryEnv) releaseGate() {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	if !e.released && e.release != nil {
		e.released = true
		close(e.release)
	}
}

// WaitRunStarted 等待在途窗口建立（模型调用已进入）。
// WaitRunStarted waits until the in-flight window is established.
func (e *crossEntryEnv) WaitRunStarted() {
	e.t.Helper()
	e.gateMu.Lock()
	entered := e.entered
	e.gateMu.Unlock()
	require.NotNil(e.t, entered, "WaitRunStarted 之前必须先 blockNextRun")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		e.t.Fatal("运行未在 5s 内进入模型调用（在途窗口未建立）")
	}
}

// WaitRunFinished 等待最近一条执行行离开 started（运行 ctx 已收尾）。
// WaitRunFinished waits until the newest execution row leaves the started state.
func (e *crossEntryEnv) WaitRunFinished() {
	e.t.Helper()
	require.Eventually(e.t, func() bool {
		rows, err := e.store.ListExecutions(context.Background(), e.taskID, 1)
		return err == nil && len(rows) == 1 && rows[0].Status != core.ExecutionStarted
	}, 5*time.Second, 5*time.Millisecond, "运行未在时限内收尾")
}

// Fire 触发一次**定时**触发：把 `next_fire_at` 拨到刚刚过去，交给调度器的到期扫描
// 领取并执行（US-24 起触发权在数据库，测试不再直接调作业）。返回的 channel 在本次
// planned_at 对应的执行行离开 started 后关闭——与 cron 作业返回同语义。
// Fire makes the task due in the database and returns a channel that closes once this
// planned instant has a terminal execution row.
func (e *crossEntryEnv) Fire(taskID string) <-chan struct{} {
	e.t.Helper()
	e.awaitInitialized(taskID)
	planned := time.Now().UTC().Truncate(time.Millisecond).
		Add(-time.Duration(e.fireSeq.Add(1)) * 100 * time.Millisecond)
	// 夹具直写拨表：`SetNextFire` 的 nil 预期只服务"初始化 NULL 行"，是行条件写，
	// 不再兼作强制改期（外部写入不会被迟到的初始化覆盖，ADJ-20）。这里要的是把已
	// 初始化行的触发点直接改到过去。
	res, err := e.db.ExecContext(context.Background(),
		`UPDATE scheduled_tasks SET next_fire_at = $2 WHERE task_id = $1 AND retired_at IS NULL`,
		taskID, planned.UnixMilli())
	require.NoError(e.t, err, "Fire 必须能把任务拨到已到期")
	affected, err := res.RowsAffected()
	require.NoError(e.t, err)
	require.EqualValues(e.t, 1, affected, "Fire 必须命中恰一条未退休任务行")
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.awaitTerminal(taskID, planned)
	}()
	return done
}

// awaitInitialized 等到首次初始化把 `next_fire_at` 写进未来（任务离开到期集合）。
//
// 对账后的第一轮扫描看到 NULL 只做初始化、不执行；初始化是行条件写（ADJ-20），
// 观察到"任务不在到期集合里"即证明那一笔已提交且任务行已存在，此后它不会再改写
// 非 NULL 行。随后 Fire 的夹具直写才有一个确定的目标行可拨。
// Wait until the first init pass has committed and the task row exists.
func (e *crossEntryEnv) awaitInitialized(taskID string) {
	e.t.Helper()
	require.Eventually(e.t, func() bool {
		due, err := e.store.ListDispatchable(context.Background(), 32)
		if err != nil {
			return false
		}
		for _, item := range due {
			if item.Task.TaskID == taskID {
				return false
			}
		}
		return true
	}, 10*time.Second, 5*time.Millisecond, "任务 %s 未完成首次 next_fire_at 初始化", taskID)
}

// awaitTerminal 轮询到本次 planned_at 的执行行出现终态为止（在 goroutine 里跑，
// 只能用 Errorf，不能用 require / Fatal）。
// awaitTerminal polls until the execution row of this planned instant is terminal.
func (e *crossEntryEnv) awaitTerminal(taskID string, planned time.Time) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := e.store.ListExecutions(context.Background(), taskID, 64)
		if err == nil {
			for _, row := range rows {
				if row.PlannedAt.Equal(planned) && row.Status != core.ExecutionStarted {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Errorf("任务 %s 的计划触发 %s 未在时限内收尾", taskID, planned.Format(time.RFC3339Nano))
}

// GetHTTP 发出 GET 并返回已读完 body 的响应（RawBody 可再次读取缓存）。
// GetHTTP performs a GET and returns the response with its body cached.
func (e *crossEntryEnv) GetHTTP(path string) *http.Response {
	e.t.Helper()
	resp, err := e.do(http.MethodGet, path)
	require.NoError(e.t, err)
	return resp
}

// PostHTTP 同步发出 POST，**仅用于立即返回的路径**（归档后补跑 400 等）。
// PostHTTP is synchronous and only for paths that return immediately.
func (e *crossEntryEnv) PostHTTP(path string) *http.Response {
	e.t.Helper()
	resp, err := e.do(http.MethodPost, path)
	require.NoError(e.t, err)
	return resp
}

// PostHTTPAsync 异步发起 POST：**同步端点（/run、/messages）必须经此发起**——
// 响应到达后写入带缓冲 channel。
// PostHTTPAsync starts a POST without blocking the test goroutine; mandatory for
// synchronous endpoints such as /run.
func (e *crossEntryEnv) PostHTTPAsync(path string) <-chan *http.Response {
	ch := make(chan *http.Response, 1)
	go func() {
		defer close(ch)
		resp, err := e.do(http.MethodPost, path)
		if err != nil {
			e.t.Errorf("异步 POST %s 失败: %v", path, err)
			ch <- &http.Response{StatusCode: 0}
			return
		}
		ch <- resp
	}()
	return ch
}

// DeleteHTTP 发出 DELETE（归档入口）。
// DeleteHTTP performs the archive request.
func (e *crossEntryEnv) DeleteHTTP(path string) *http.Response {
	e.t.Helper()
	resp, err := e.do(http.MethodDelete, path)
	require.NoError(e.t, err)
	return resp
}

// do 发一次请求并把 body 读入内存后复位，供 RawBody / EnvelopeCode 反复读取。
// do performs one request and caches the body for RawBody / EnvelopeCode.
func (e *crossEntryEnv) do(method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, e.baseURL+path, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	if err := resp.Body.Close(); err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	e.bodyMu.Lock()
	e.bodies[resp] = string(raw)
	e.bodyMu.Unlock()
	return resp, nil
}

// RawBody 返回该响应缓存的原始 body。
// RawBody returns the cached raw body of one response.
func (e *crossEntryEnv) RawBody(resp *http.Response) string {
	e.bodyMu.Lock()
	defer e.bodyMu.Unlock()
	return e.bodies[resp]
}

// EnvelopeCode 解析响应的统一信封并返回 code 字段。
// EnvelopeCode decodes the envelope and returns its code field.
func (e *crossEntryEnv) EnvelopeCode(resp *http.Response) string {
	e.t.Helper()
	var env struct {
		Code string `json:"code"`
	}
	require.NoError(e.t, json.Unmarshal([]byte(e.RawBody(resp)), &env))
	return env.Code
}

// LastExecution 经仓储读指定任务的最近一行执行记录。
// LastExecution reads the newest execution row of one task through the store.
func (e *crossEntryEnv) LastExecution(taskID string) core.ExecutionRecord {
	e.t.Helper()
	rows, err := e.store.ListExecutions(context.Background(), taskID, 1)
	require.NoError(e.t, err)
	require.Len(e.t, rows, 1, "任务 %s 没有执行行", taskID)
	return rows[0]
}

// SchedulerSessionID 返回调度会话标识（与生产同一条派生公式）。
// SchedulerSessionID derives the scheduler session id with the production formula.
func (e *crossEntryEnv) SchedulerSessionID(agent string) string {
	return core.DeriveSessionID("scheduler", "scheduler", agent)
}

// IsArchived 经会话仓储读库判定归档态（不经任何进程内缓存）。
// IsArchived reads the archived status from the session store, never from a cache.
func (e *crossEntryEnv) IsArchived(agent string) bool {
	e.t.Helper()
	rec, err := e.sessions.GetSession(context.Background(), e.SchedulerSessionID(agent))
	require.NoError(e.t, err)
	return rec.Status == "archived"
}

// emptyMemorySections 是 web 记忆读面的空实现：本用例不触达 /memory，端口只为通过
// 装配必填校验。**不得**据此放宽 web 的端口校验（那是装配纪律本身）。
// emptyMemorySections only satisfies the required port; the endpoint is never hit.
type emptyMemorySections struct{}

// Sections 返回两段空文本。
// Sections returns two empty partitions.
func (emptyMemorySections) Sections(context.Context, core.Scope) (string, string, error) {
	return "", "", nil
}

// 同步语义（R11-H2）：POST /run、POST messages 在运行结束前不返回，测试不得在主
// goroutine 先发后等；Fire 也必须异步执行 job（与 cron 的作业 goroutine 同语义），
// 否则 WaitRunStarted 永远到不了。手动面由 TestCrossEntry_ManualRerunArchiveCoordination
// 对称覆盖。
// 逆改点：归档互斥留在 web.Server（admissionMu / inflightRuns）→ cron 经 AgentService.Run
// 绕过准入：① 的 DELETE 返回 200 或 ② 的 cron 复活归档会话，断言红。
func TestCrossEntry_CronArchiveCoordination(t *testing.T) {
	env := newCrossEntryEnv(t, core.AgentEntry{
		Name: "demo", Registered: true,
		Profile: &core.Profile{Schedules: []core.Schedule{{ID: "demo-1", Cron: "*/5 * * * * *", Zone: "UTC", Message: "巡检"}}},
	})

	t.Run("archive is rejected while cron run is in flight", func(t *testing.T) {
		env.blockNextRun()
		jobDone := env.Fire("demo-1") // 异步：cron job 在独立 goroutine 中运行
		env.WaitRunStarted()

		resp := env.DeleteHTTP("/api/v1/sessions/" + url.PathEscape(env.SchedulerSessionID("demo")))
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		require.Equal(t, "SESSION_BUSY", env.EnvelopeCode(resp))

		env.ReleaseRun()
		<-jobDone // job 返回 = Run 与 FinishExecution 已完成
		env.WaitRunFinished()
		resp = env.DeleteHTTP("/api/v1/sessions/" + url.PathEscape(env.SchedulerSessionID("demo")))
		require.Equal(t, http.StatusOK, resp.StatusCode, "在途结束后归档应成功")
	})

	t.Run("archived session is skipped by next cron fire", func(t *testing.T) {
		<-env.Fire("demo-1") // 跳过路径快速返回；等 job 收尾后再读执行行
		row := env.LastExecution("demo-1")
		require.Equal(t, core.ExecutionSkipped, row.Status)
		require.Equal(t, "会话已归档，本轮跳过", row.ErrorMessage)
		require.True(t, env.IsArchived("demo"), "不得重建 / 复活归档会话")

		// 视图投影与库内原因一致（R10-H2）：/executions 返回同一冻结文案，不回显内部串。
		resp := env.GetHTTP("/api/v1/schedules/demo-1/executions")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, env.RawBody(resp), `"error_message":"会话已归档，本轮跳过"`)
	})
}

// 同步语义（R11-H2）：/run 在运行结束前不返回——必须先 PostHTTPAsync、WaitRunStarted，
// 再 DELETE 断言 409、释放运行、最后从 channel 取 200；按同步顺序写会永久阻塞。
// 逆改点：归档判定散落在 web 与调度两层、或把 SessionArchivedError 折成 ErrTaskBusy / 500
// → ② 的状态码与错误码断言红；手动面在途归档另建 web 互斥 → ① 的 409 断言红。
func TestCrossEntry_ManualRerunArchiveCoordination(t *testing.T) {
	env := newCrossEntryEnv(t, core.AgentEntry{
		Name: "demo", Registered: true,
		Profile: &core.Profile{Schedules: []core.Schedule{{ID: "demo-1", Cron: "0 8 * * * *", Zone: "UTC", Message: "巡检"}}},
	})

	t.Run("archive is rejected while manual rerun is in flight", func(t *testing.T) {
		env.blockNextRun()
		respCh := env.PostHTTPAsync("/api/v1/schedules/demo-1/run") // /run 同步：异步发起，不能先等响应
		env.WaitRunStarted()

		resp := env.DeleteHTTP("/api/v1/sessions/" + url.PathEscape(env.SchedulerSessionID("demo")))
		require.Equal(t, http.StatusConflict, resp.StatusCode)
		require.Equal(t, "SESSION_BUSY", env.EnvelopeCode(resp))

		env.ReleaseRun()
		resp = <-respCh // 运行结束后 /run 才返回 200
		require.Equal(t, http.StatusOK, resp.StatusCode)
		env.WaitRunFinished()
		resp = env.DeleteHTTP("/api/v1/sessions/" + url.PathEscape(env.SchedulerSessionID("demo")))
		require.Equal(t, http.StatusOK, resp.StatusCode, "在途结束后归档应成功")
	})

	t.Run("manual rerun on archived session returns 400", func(t *testing.T) {
		// 归档路径在运行前即返回，可同步调用；不会越过任何在途运行。
		resp := env.PostHTTP("/api/v1/schedules/demo-1/run")
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Equal(t, "BAD_REQUEST", env.EnvelopeCode(resp))
		require.Contains(t, env.RawBody(resp), "会话已归档，不可追加消息")
		require.True(t, env.IsArchived("demo"), "不得重建 / 复活归档会话")
	})
}
