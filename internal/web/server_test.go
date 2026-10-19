package web

// 本文件是 HTTP 面生命周期验收（模块卡 §4.1 server 表 / W11 / W12）：默认值与超时、
// 优雅停机两分支、真实连接写预算、recover、跨源写保护与请求日志。停机用例一律用
// 同步事件记录代替 sleep 竞猜；唯一的时间等待是「宽限期内仍存活」的负向断言。
// Lifecycle acceptance: defaults, timeouts, graceful shutdown (both branches), real-
// connection write budget, recover, cross-origin protection, and request logging.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// TestServer_Defaults 验证 NewServer 补齐的四个缺省值（W11）。
// 逆改点：默认监听改成 0.0.0.0 / 空，或漏补任一缺省 → 对应断言红。
func TestServer_Defaults(t *testing.T) {
	s := newTestServer(t, nil)
	require.Equal(t, defaultAddr, s.opts.Addr, "默认只监听本机回环")
	require.Equal(t, "127.0.0.1:8080", s.opts.Addr)
	require.Equal(t, int64(defaultMaxRequestBytes), s.opts.MaxRequestBytes)
	require.Equal(t, defaultRunTimeout, s.opts.RunTimeout)
	require.Equal(t, defaultShutdownGrace, s.opts.ShutdownGrace)
}

// TestServer_TimeoutsAndWriteBudget 逐字保留模块卡 R10（W11）。
// 逆改点：默认 addr 改成 0.0.0.0 或空 → 断言红；WriteTimeout 调回 90s 或写成
// runTimeout+30s → 等式断言红（写 deadline 从读完请求头起算，覆盖读 body + 运行 +
// 写响应，必须能容纳 ReadTimeout + RunTimeout + writeMargin）。
func TestServer_TimeoutsAndWriteBudget(t *testing.T) {
	s := newTestServer(t, nil)
	require.Equal(t, defaultAddr, s.opts.Addr)
	require.Equal(t, "127.0.0.1:8080", s.opts.Addr)
	require.Equal(t, int64(defaultMaxRequestBytes), s.opts.MaxRequestBytes)
	require.Equal(t, defaultRunTimeout, s.opts.RunTimeout)
	require.Equal(t, defaultShutdownGrace, s.opts.ShutdownGrace)

	srv := s.newHTTPServer(context.Background())
	require.Equal(t, 5*time.Second, srv.ReadHeaderTimeout)
	require.Equal(t, 60*time.Second, srv.ReadTimeout)
	require.Equal(t, 120*time.Second, srv.IdleTimeout)
	require.Equal(t, 130*time.Second, srv.WriteTimeout)
	require.Equal(t, readTimeout+s.opts.RunTimeout+writeMargin, srv.WriteTimeout,
		"写超时必须等于 读超时 + 运行时限 + 收尾余量")
}

// TestServer_GracefulShutdown 覆盖宽限期内的两阶段收尾（W11）：
// ① 监听器先关、在途请求后收尾；② 无在途请求时 Run 干净返回。
// 逆改点：宽限期内的运行被信号直接取消 / 监听器未先关 → 事件序或响应断言红。
func TestServer_GracefulShutdown(t *testing.T) {
	t.Run("在途请求收尾", func(t *testing.T) {
		var (
			mu     sync.Mutex
			events []string
		)
		record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
		has := func(e string) bool {
			mu.Lock()
			defer mu.Unlock()
			return slices.Contains(events, e)
		}

		runStarted := make(chan struct{})
		release := make(chan struct{})
		runs := runFunc(func(context.Context, core.RunRequest) (core.RunResult, error) {
			record("run-start")
			close(runStarted)
			<-release
			record("run-returned")
			return core.RunResult{RunID: "run-1", Status: core.StatusCompleted}, nil
		})
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		store.seed(core.SessionRecord{
			ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
		})
		ln := &recordingListener{Listener: mustLoopbackListener(t), onClose: func() { record("listener-close") }}
		s := newTestServer(t, func(o *Options) {
			o.Runs = runs
			o.Sessions = store
			o.Listener = ln
			o.ShutdownGrace = 5 * time.Second
			o.Now = time.Now
		})
		signalCtx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- s.Run(signalCtx) }()

		type postResult struct {
			resp *http.Response
			err  error
		}
		postCh := make(chan postResult, 1)
		go func() {
			resp, err := http.Post("http://"+ln.Addr().String()+"/api/v1/sessions/web-x/messages",
				"application/json", strings.NewReader(`{"message":"hi"}`))
			postCh <- postResult{resp: resp, err: err}
		}()
		<-runStarted

		cancel() // ① 停止接入 + ② 宽限等待
		require.Eventually(t, func() bool { return has("listener-close") }, 5*time.Second, 10*time.Millisecond)
		require.False(t, has("run-returned"), "宽限期内运行必须继续，不得被信号取消")

		close(release) // 在途运行完成 → 连接空闲 → Shutdown 返回
		var result postResult
		select {
		case result = <-postCh:
		case <-time.After(5 * time.Second):
			t.Fatal("在途请求未收尾")
		}
		require.NoError(t, result.err)
		require.Equal(t, http.StatusOK, result.resp.StatusCode, "宽限期内的请求必须完整返回")
		_ = result.resp.Body.Close()

		select {
		case err := <-errCh:
			require.NoError(t, err, "宽限内完成收尾的 Run 必须正常返回")
		case <-time.After(5 * time.Second):
			t.Fatal("Run 未返回")
		}

		mu.Lock()
		defer mu.Unlock()
		require.Less(t, slices.Index(events, "run-start"), slices.Index(events, "listener-close"))
		require.Less(t, slices.Index(events, "listener-close"), slices.Index(events, "run-returned"))
	})

	t.Run("无在途请求也按序收尾", func(t *testing.T) {
		closed := make(chan struct{}, 1)
		ln := &recordingListener{Listener: mustLoopbackListener(t), onClose: func() { closed <- struct{}{} }}
		s := newTestServer(t, func(o *Options) { o.Listener = ln })
		signalCtx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- s.Run(signalCtx) }()
		// 先等 Serve 真正接管监听器再发信号：Shutdown 只关闭已登记到 http.Server 的
		// 监听器，Serve 启动前发信号会让 Close 事件永不发生（假红）。
		require.Eventually(t, func() bool {
			client := &http.Client{Timeout: 100 * time.Millisecond}
			resp, err := client.Get("http://" + ln.Addr().String() + "/api/v1/health")
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		}, 5*time.Second, 10*time.Millisecond)
		cancel()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("监听器未关闭")
		}
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("Run 未返回")
		}
	})
}

// TestServer_ShutdownGraceExhausted 逐字保留模块卡 R16（W11）。
// 逆改点：newHTTPServer 直接把信号 ctx 当 BaseContext → 信号一到运行即被取消，
// “宽限期内仍存活”断言红；省掉 ③ 的 cancelBase → 宽限耗尽后运行永不返回，
// “运行返回 / Run 返回”断言红；资源释放在 Run 返回之前 → 事件序断言红。
func TestServer_ShutdownGraceExhausted(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	has := func(e string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(events, e)
	}

	runStarted := make(chan struct{})
	runReturned := make(chan struct{})
	runs := runFunc(func(ctx context.Context, _ core.RunRequest) (core.RunResult, error) {
		record("run-start")
		close(runStarted)
		<-ctx.Done() // 只有 baseCtx 被取消（宽限耗尽）才返回
		record("run-cancelled")
		close(runReturned)
		return core.RunResult{RunID: "run-1", Status: core.StatusCancelled}, nil
	})

	store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
	store.seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	ln := &recordingListener{Listener: mustLoopbackListener(t), onClose: func() { record("listener-close") }}
	s := newTestServer(t, func(o *Options) {
		o.Runs = runs
		o.Sessions = store // 必须有已存在会话：admitRun 先查会话，缺失即 404、运行永不启动
		o.Listener = ln
		o.ShutdownGrace = 500 * time.Millisecond // 测试接缝：缩短宽限，同时留足 -race 调度余量
		o.Now = time.Now
	})
	signalCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(signalCtx) }()

	// 经真实连接发出一个会阻塞在运行里的请求。
	go func() {
		resp, err := http.Post("http://"+ln.Addr().String()+"/api/v1/sessions/web-x/messages",
			"application/json", strings.NewReader(`{"message":"hi"}`))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-runStarted

	cancelAt := time.Now()
	cancel() // 信号：① 停止接入 + ② 宽限等待
	require.Eventually(t, func() bool { return has("listener-close") }, 5*time.Second, 10*time.Millisecond)

	// 宽限期内运行必须仍存活——BaseContext 已与信号 ctx 解耦。
	// 等待 100ms 而宽限 500ms：余量吸收 -race / 高负载下的调度延迟，避免假红。
	select {
	case <-runReturned:
		t.Fatal("宽限期未到，运行已被取消：BaseContext 未与信号 ctx 解耦")
	case <-time.After(100 * time.Millisecond):
	}

	// 宽限耗尽：③ 取消在途请求 → 运行返回 → ④ 有界收尾 → ⑤ Run 返回。
	select {
	case <-runReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("宽限耗尽后运行未退出：缺少 cancelBase")
	}
	// 下界断言：取消不得早于宽限耗尽（500ms − 50ms 调度容差）。
	require.GreaterOrEqual(t, time.Since(cancelAt), 450*time.Millisecond,
		"运行取消必须发生在宽限耗尽之后，提前取消即 BaseContext 未与信号 ctx 解耦")
	select {
	case err := <-errCh:
		require.Error(t, err, "宽限耗尽时 Run 必须返回错误")
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未返回")
	}
	require.True(t, s.waitInflight(time.Second), "有界收尾后不得留有在途请求")

	mu.Lock()
	defer mu.Unlock()
	require.Less(t, slices.Index(events, "run-start"), slices.Index(events, "listener-close"))
	require.Less(t, slices.Index(events, "listener-close"), slices.Index(events, "run-cancelled"),
		"取消必须发生在宽限耗尽之后")
}

// TestServer_ServeErrorUnifiedCleanup 验证监听 / 服务异常退出与信号路径共用收尾
// （R21）：Serve 返回错误后，Run 必须先取消在途运行、关闭活动连接，并一直等到
// handler 真正退出才返回。
//
// 判别面（独立复核 R5R-02 后补强）：handler 收到取消后故意继续阻塞，超过
// drainTimeout 仍不退出——若把等待超时当作完成（删掉最终 inflight.Wait），Run 会
// 在 300ms 观察窗口内提前返回，断言红；若缺少 srv.Close，活动连接不会被关闭，
// 客户端错误断言红。逆改点：`case err := <-errCh: return err` 直接返回 → 取消与
// 连接关闭都不会发生，断言红（调用方会在此后释放数据库 / MCP，而 handler 仍在用）。
func TestServer_ServeErrorUnifiedCleanup(t *testing.T) {
	var (
		mu     sync.Mutex
		events []string
	)
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	has := func(e string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(events, e)
	}

	runStarted := make(chan struct{})
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	runs := runFunc(func(ctx context.Context, _ core.RunRequest) (core.RunResult, error) {
		record("run-start")
		close(runStarted)
		<-ctx.Done() // 只有统一收尾的 cancelBase 能让它退出
		record("run-cancelled")
		// 模拟取消后仍有一段清理时间的 handler：故意不响应上下文，证明 Run 的
		// 返回依赖真正的等待，而不是「取消后 handler 恰好立刻退出」。
		close(cleanupStarted)
		<-releaseCleanup
		return core.RunResult{RunID: "run-1", Status: core.StatusCancelled}, nil
	})
	store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
	store.seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	ln := mustLoopbackListener(t)
	s := newTestServer(t, func(o *Options) {
		o.Runs = runs
		o.Sessions = store
		o.Listener = ln
		o.drainTimeoutOverride = 50 * time.Millisecond
		o.Now = time.Now
	})
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(context.Background()) }()

	clientErr := make(chan error, 1)
	go func() {
		resp, err := http.Post("http://"+ln.Addr().String()+"/api/v1/sessions/web-x/messages",
			"application/json", strings.NewReader(`{"message":"hi"}`))
		if err == nil {
			_ = resp.Body.Close()
		}
		clientErr <- err
	}()
	<-runStarted

	// 外部关闭监听器：Serve 收到 Accept 错误返回（不是 ErrServerClosed），
	// 复现「监听异常退出」路径。
	require.NoError(t, ln.Close())

	select {
	case <-cleanupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("监听异常退出后运行未收到取消：Serve 错误分支未经过统一收尾")
	}

	// srv.Close 必须关闭活动连接：handler 仍阻塞时客户端应观察到连接断开。
	// 缺 Close 时客户端会继续等待响应直到 releaseCleanup，本断言红。
	select {
	case err := <-clientErr:
		require.Error(t, err, "活动连接必须被 srv.Close 关闭（客户端应观察到错误）")
	case <-time.After(5 * time.Second):
		t.Fatal("handler 仍存活时活动连接未被关闭（缺少 srv.Close）")
	}

	// drainTimeout（50ms）已到期而 handler 仍阻塞：Run 不得返回。
	select {
	case err := <-errCh:
		t.Fatalf("Serve 异常退出后 Run 在 handler 存活时返回: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(releaseCleanup)
	select {
	case err := <-errCh:
		require.Error(t, err, "监听异常退出时 Run 必须返回错误")
	case <-time.After(5 * time.Second):
		t.Fatal("handler 退出后 Run 未返回")
	}
	require.True(t, has("run-cancelled"), "Run 返回前必须已取消在途运行")
	require.True(t, s.waitInflight(time.Second), "Run 返回后不得留有在途 handler")
}

// TestServer_ShutdownDrainTimeoutWaitsForHandler 验证收尾等待超时的终止策略（R22）：
// 宽限耗尽且 drainTimeout 到期后，只要 handler 仍存活，Run **不得**返回；handler
// 退出后 Run 才返回。
// 逆改点：把 waitInflight 超时当作收尾完成（只记日志就返回）→ 300ms 窗口内
// Run 提前返回，断言红。
func TestServer_ShutdownDrainTimeoutWaitsForHandler(t *testing.T) {
	runStarted := make(chan struct{})
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	runs := runFunc(func(ctx context.Context, _ core.RunRequest) (core.RunResult, error) {
		close(runStarted)
		<-ctx.Done()
		// 模拟取消后仍需一段清理时间的 handler：故意不响应上下文。
		close(cleanupStarted)
		<-releaseCleanup
		return core.RunResult{RunID: "run-1", Status: core.StatusCancelled}, nil
	})
	store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
	store.seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	ln := &recordingListener{Listener: mustLoopbackListener(t), onClose: func() {}}
	s := newTestServer(t, func(o *Options) {
		o.Runs = runs
		o.Sessions = store
		o.Listener = ln
		o.ShutdownGrace = 100 * time.Millisecond
		o.drainTimeoutOverride = 50 * time.Millisecond
		o.Now = time.Now
	})
	signalCtx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.Run(signalCtx) }()

	go func() {
		resp, err := http.Post("http://"+ln.Addr().String()+"/api/v1/sessions/web-x/messages",
			"application/json", strings.NewReader(`{"message":"hi"}`))
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-runStarted
	cancel()

	select {
	case <-cleanupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("宽限耗尽后运行未收到取消：缺少 cancelBase")
	}
	// 此刻 drainTimeout（50ms）必然已到期：handler 仍阻塞，Run 不得返回。
	select {
	case err := <-errCh:
		t.Fatalf("收尾等待超时后 Run 提前返回（handler 仍存活）: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(releaseCleanup)
	select {
	case err := <-errCh:
		require.Error(t, err, "宽限耗尽时 Run 必须返回错误")
	case <-time.After(5 * time.Second):
		t.Fatal("handler 退出后 Run 未返回")
	}
	require.True(t, s.waitInflight(time.Second), "Run 返回后不得留有在途 handler")
}

// TestServer_PanicLogSharesRequestID 验证 panic 日志与请求完成日志共享同一个非空
// 标识（R23）：标识在链首生成并注入 context，完成日志走 defer。两个分支都覆盖：
// 未写头（补 500 信封、完成行 status=500）与已写头（不补第二份信封、完成行记
// 真实状态 —— 独立复核 R5R-05）。
// 逆改点：标识只在 requestLogMiddleware 生成 → 外层 recover 读到空串，断言红；
// 完成日志写在 next.ServeHTTP 之后（非 defer）→ panic 时没有完成行，断言红。
func TestServer_PanicLogSharesRequestID(t *testing.T) {
	const marker = "PANIC-MARKER-42"
	var logBuf bytes.Buffer
	runs := runFunc(func(context.Context, core.RunRequest) (core.RunResult, error) {
		panic(marker)
	})
	s := newTestServer(t, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
		o.Runs = runs
	})
	s.opts.Sessions.(*fakeSessionStore).seed(core.SessionRecord{
		ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
	})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/web-x/messages", strings.NewReader(`{"message":"hi"}`)))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"INTERNAL"`)
	require.NotContains(t, rec.Body.String(), marker, "panic 细节只进日志")

	var panicID, completionID string
	for _, line := range strings.Split(logBuf.String(), "\n") {
		if strings.Contains(line, "HTTP handler panic") {
			panicID = logRequestID(t, line)
		}
		if strings.Contains(line, "HTTP 请求") && strings.Contains(line, "status=500") {
			completionID = logRequestID(t, line)
		}
	}
	require.NotEmpty(t, panicID, "panic 日志必须带非空 request_id")
	require.Equal(t, panicID, completionID,
		"panic 日志与完成日志必须共享同一个 request_id")

	// panic 前已写响应头：完成日志记真实状态（200），recover 不再补第二份信封
	// （独立复核 R5R-05 补的具名分支）。逆改点：完成日志固定记 500 → status=200
	// 断言红；recover 无条件补 500 → 响应体出现第二份信封，逐字断言红。
	t.Run("panic 已写头", func(t *testing.T) {
		var buf bytes.Buffer
		s := newTestServer(t, func(o *Options) {
			o.Logger = slog.New(slog.NewTextHandler(&buf, nil))
		})
		h := s.recoverMiddleware(s.requestLogMiddleware(http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"code":"OK"}`))
				panic(marker)
			})))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, `{"code":"OK"}`, rec.Body.String(), "已写头后不得补第二份信封")

		var panicID, completionID, completionLine string
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.Contains(line, "HTTP handler panic") {
				panicID = logRequestID(t, line)
			}
			if strings.Contains(line, "HTTP 请求") {
				completionID = logRequestID(t, line)
				completionLine = line
			}
		}
		require.NotEmpty(t, panicID)
		require.Equal(t, panicID, completionID)
		require.Contains(t, completionLine, "status=200",
			"panic 前已写头：完成日志必须记真实状态")
	})
}

// logRequestID 从一行 slog TextHandler 输出里取非空 request_id。
// logRequestID extracts a non-empty request_id from one text log line.
func logRequestID(t *testing.T, line string) string {
	t.Helper()
	const prefix = "request_id="
	i := strings.Index(line, prefix)
	require.GreaterOrEqual(t, i, 0, "日志行缺 request_id: %s", line)
	rest := line[i+len(prefix):]
	if j := strings.IndexAny(rest, " \""); j >= 0 {
		rest = rest[:j]
	}
	require.NotEmpty(t, rest, "request_id 不得为空")
	return rest
}

// TestServer_WriteBudgetRealConnection 逐字保留模块卡 R19（W11）。
// 逆改点：WriteTimeout 退回「只比运行时限大」的口径 → 慢 body 挤占预算，正向子用例
// 在真实连接上截断、JSON 解析失败；负向对照（WriteTimeout 400ms）证明用例看得见截断；
// 若改用 httptest.NewServer（自建零超时 http.Server）→ 负向对照不再截断，说明
// 用例测的是被测 Server 的超时而不是测试服务器的缺省。
func TestServer_WriteBudgetRealConnection(t *testing.T) {
	const budget = 500*time.Millisecond + 300*time.Millisecond + 100*time.Millisecond // 900ms

	newServer := func(t *testing.T, forcedWriteTimeout time.Duration) *httptest.Server {
		t.Helper()
		runs := &slowRunService{delay: 200 * time.Millisecond}
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		store.seed(core.SessionRecord{
			ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
		})
		s := newTestServer(t, func(o *Options) {
			o.Runs = runs
			o.Sessions = store // 缺会话即 404，永远到不了「慢 body + 运行」路径
			o.readTimeoutOverride = 500 * time.Millisecond
			o.RunTimeout = 300 * time.Millisecond
			o.writeMarginOverride = 100 * time.Millisecond
			o.writeTimeoutOverride = forcedWriteTimeout // 非 0 时直接钉死写超时（负向对照用）
		})
		ts := httptest.NewUnstartedServer(s.handler())
		ts.Config = s.newHTTPServer(context.Background()) // 换入被测超时配置
		ts.Start()
		t.Cleanup(ts.Close)
		return ts
	}

	slowBody := func() io.Reader {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte(`{"message":`))
			time.Sleep(300 * time.Millisecond) // 慢 body 吃掉读预算
			_, _ = pw.Write([]byte(`"hi"}`))
			_ = pw.Close()
		}()
		return pr
	}

	t.Run("900ms 预算覆盖慢 body 与运行", func(t *testing.T) {
		ts := newServer(t, 0) // 0 → 按 read + RunTimeout + margin 派生
		resp, err := http.Post(ts.URL+"/api/v1/sessions/web-x/messages", "application/json", slowBody())
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var env map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&env), "响应必须完整未被截断")
		require.Equal(t, "OK", env["code"])
		require.Equal(t, int(budget/time.Millisecond), 900)
	})

	t.Run("负向对照：WriteTimeout 400ms 时截断可被观察", func(t *testing.T) {
		ts := newServer(t, 400*time.Millisecond)
		resp, err := http.Post(ts.URL+"/api/v1/sessions/web-x/messages", "application/json", slowBody())
		if err != nil {
			// 连接层提前断开与响应体截断同属可观察截断；拿到**完整可解析**信封才判负向对照失效。
			return
		}
		defer resp.Body.Close()
		var env map[string]any
		require.Error(t, json.NewDecoder(resp.Body).Decode(&env),
			"写超时过短时响应必须不完整（解析成功说明负向对照无判别力）")
	})
}

// slowRunService 在运行入口等待 delay 后成功返回；delay 必须小于 RunTimeout
// （200ms < 300ms），用于占据写预算中的运行段。
type slowRunService struct{ delay time.Duration }

func (s *slowRunService) Run(ctx context.Context, _ core.RunRequest) (core.RunResult, error) {
	select {
	case <-time.After(s.delay):
		return core.RunResult{RunID: "run-1", Status: core.StatusCompleted}, nil
	case <-ctx.Done():
		return core.RunResult{RunID: "run-1", Status: core.StatusCancelled}, nil
	}
}

func (s *slowRunService) Archive(context.Context, string) error { return nil }

// TestServer_RecoverMiddleware 逐字保留模块卡 R11（W12）。
// 逆改点：recover 只记日志不写 500 → 第一个子用例状态码红；
// 已写头后再写第二份信封 → ResponseRecorder 捕获到双写（body 含两份 JSON）→ 红。
func TestServer_RecoverMiddleware(t *testing.T) {
	s := newTestServer(t, nil)

	t.Run("未写头：500 信封", func(t *testing.T) {
		h := s.recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("boom")
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Contains(t, rec.Body.String(), `"code":"INTERNAL"`)
		require.NotContains(t, rec.Body.String(), "boom", "panic 细节只进日志")
	})

	t.Run("已写头：不写第二份信封", func(t *testing.T) {
		h := s.recoverMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			panic("after write")
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, `{"ok":true}`, rec.Body.String(), "响应不得被追加第二份信封")
	})
}

// TestServer_CrossOriginRejectsWrites 逐字保留模块卡 R18（W12）。
// 逆改点：去掉跨源中间件 → POST / DELETE 非 403；不用 SetDenyHandler 而手写 403
// 文本 → 信封 code / message / Content-Type 断言红；拒绝前读 body → bodyRead 断言红；
// 拒绝仍触达 Run → runs 计数红；拒绝仍查会话仓储 → getSessionCallCount 断言红；
// 输出 ACAO → 头断言红。
func TestServer_CrossOriginRejectsWrites(t *testing.T) {
	const evil = "https://evil.example"
	cases := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		wantStatus int
	}{
		{
			"恶意 Origin 的 GET 放行且无 ACAO", http.MethodGet, "/api/v1/health",
			map[string]string{"Origin": evil},
			http.StatusOK,
		},
		{
			"恶意 Origin 的 POST 拒绝", http.MethodPost, "/api/v1/sessions/web-x/messages",
			map[string]string{"Origin": evil},
			http.StatusForbidden,
		},
		{
			"恶意 Origin 的 DELETE 拒绝", http.MethodDelete, "/api/v1/sessions/web-x",
			map[string]string{"Origin": evil},
			http.StatusForbidden,
		},
		{
			"Sec-Fetch-Site: cross-site 的 POST 拒绝", http.MethodPost, "/api/v1/sessions/web-x/messages",
			map[string]string{"Sec-Fetch-Site": "cross-site"},
			http.StatusForbidden,
		},
		{"无 Origin 的 curl POST 放行", http.MethodPost, "/api/v1/sessions/web-x/messages", nil, http.StatusOK},
		{
			"同源 Origin 的 POST 放行", http.MethodPost, "/api/v1/sessions/web-x/messages",
			map[string]string{"Origin": "http://example.com"},
			http.StatusOK,
		}, // httptest 请求 Host = example.com
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
			s := newTestServer(t, func(o *Options) { o.Runs = runs })
			store := s.opts.Sessions.(*fakeSessionStore)
			store.seed(core.SessionRecord{
				ID: "web-x", ProfileName: "ops-agent", Channel: channelWeb, UserID: "u", Status: statusActive,
			})
			body := &countingReader{r: strings.NewReader(`{"message":"hi"}`)}
			req := httptest.NewRequest(tc.method, tc.path, body)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			s.handler().ServeHTTP(rec, req)
			require.Equal(t, tc.wantStatus, rec.Code)

			if tc.wantStatus == http.StatusForbidden {
				require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
				var env struct{ Code, Message string }
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
				require.Equal(t, "BAD_REQUEST", env.Code)
				require.Equal(t, "跨源请求被拒绝", env.Message)
				require.Zero(t, body.n, "拒绝必须发生在读 body 之前")
				require.Empty(t, runs.requests(), "拒绝不得触达运行入口")
				require.Zero(t, store.getSessionCallCount(), "拒绝不得触达会话仓储")
			}
			require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
		})
	}
}

// countingReader 记录被读走的字节数——用于证明拒绝路径零副作用。
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestServer_RequestLog 验证请求日志含 request_id / method / path / status（W12）。
// 逆改点：日志缺 request id 或状态码恒为 0 → 对应字段断言红。
func TestServer_RequestLog(t *testing.T) {
	var logBuf bytes.Buffer
	s := newTestServer(t, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	})
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	line := logBuf.String()
	require.Contains(t, line, "request_id=")
	require.NotContains(t, line, "request_id=\"\"", "请求标识不得为空")
	require.Contains(t, line, "method=GET")
	require.Contains(t, line, "path=/api/v1/health")
	require.Contains(t, line, "status=200")
}

// 测试辅助：runFunc 把函数适配为 RunService；mustLoopbackListener 在 127.0.0.1:0
// 上监听（真实回环连接，无外部网络）。监听器由 Server.Run 的 Shutdown 关闭，
// t.Cleanup 只做兜底（重复 Close 返回错误被忽略）。
type runFunc func(context.Context, core.RunRequest) (core.RunResult, error)

func (f runFunc) Run(ctx context.Context, req core.RunRequest) (core.RunResult, error) {
	return f(ctx, req)
}

// Archive 是编译适配桩：这些用例不覆盖归档面（判据面零变更，模块卡 §3.5）。
func (f runFunc) Archive(context.Context, string) error { return nil }

func mustLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// recordingListener 在 Close 时记录事件（srv.Shutdown 的第一步就是关监听器）。
type recordingListener struct {
	net.Listener
	onClose func()
}

func (l *recordingListener) Close() error {
	l.onClose()
	return l.Listener.Close()
}
