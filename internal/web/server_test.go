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

func (s *slowRunService) ListRuns(context.Context, core.RunFilter) ([]core.RunDetail, error) {
	return nil, nil
}

func (s *slowRunService) GetRun(context.Context, string) (core.RunDetail, error) {
	return core.RunDetail{}, nil
}

func (s *slowRunService) Cancel(context.Context, string) (core.RunDetail, error) {
	return core.RunDetail{}, nil
}

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

// 账本读面与取消同样是编译适配桩：由 T028 起的运行端点用例判卷。
func (f runFunc) ListRuns(context.Context, core.RunFilter) ([]core.RunDetail, error) {
	return nil, nil
}

func (f runFunc) GetRun(context.Context, string) (core.RunDetail, error) {
	return core.RunDetail{}, nil
}

func (f runFunc) Cancel(context.Context, string) (core.RunDetail, error) {
	return core.RunDetail{}, nil
}

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

// routeKey 是路由表断言用的稳定标识（方法 + 模式）。
// routeKey is the stable identity used by the route-table assertions.
func routeKey(rt route) string { return rt.method + " " + rt.pattern }

// concretePath 把 ServeMux 模式里的通配段替换成具体值，供 Handler 探测；根路径
// 用精确结束标记 `{$}`，探测时换成真正的 "/"。
// concretePath substitutes wildcard segments with concrete values; the exact-root
// marker maps to the literal "/" path.
func concretePath(pattern string) string {
	if pattern == "/{$}" {
		return "/"
	}
	// US-12 写面的三段通配（T016 挂载）先替换，避免 `{id}` 之外的复合段残留。
	// The US-12 wildcard segments are substituted before the older single-segment ones.
	p := strings.ReplaceAll(pattern, "{archive_id}", "ops-agent-20261016T093000Z")
	p = strings.ReplaceAll(p, "{version_id}", "7")
	p = strings.ReplaceAll(p, "{kind}", "skill")
	p = strings.ReplaceAll(p, "{id}", "x")
	p = strings.ReplaceAll(p, "{name}", "ops-agent")
	// 管理台子树通配（T049）取一个资源路径；模式解析必须回到同一行。
	// The console subtree wildcard resolves with a concrete asset path.
	return strings.ReplaceAll(p, "{path...}", "assets/index.js")
}

// TestRouteTable_PublicSurfaceIsClosed 公开面闭集（约束 8）：只允许健康检查、
// 管理台静态壳与根重定向三类；新增公开入口必须同时修订契约与该表。
// 逆改点：把任一受保护端点标成 public → 本用例红。
func TestRouteTable_PublicSurfaceIsClosed(t *testing.T) {
	s := newTestServer(t, nil)

	public := 0
	for _, rt := range s.routeTable() {
		if rt.visibility != routePublic {
			continue
		}
		public++
		switch {
		case rt.method == http.MethodGet && rt.pattern == "/api/v1/health":
		// 管理台静态壳（约束 8 的公开闭集之一）：`/console` 精确路径与 `/console/*`
		// 子树都由 consoleHandler 提供（T049）。
		// The console shell is one closed public category: exact path plus subtree.
		case rt.method == http.MethodGet && (rt.pattern == "/console" || strings.HasPrefix(rt.pattern, "/console/")):
		case rt.method == http.MethodGet && rt.pattern == "/{$}":
		default:
			t.Errorf("未获批准的公开路由：%s", routeKey(rt))
		}
	}
	require.Positive(t, public, "健康检查必须在公开面内")
}

// TestRouteTable_ExistingEndpointsEnumerated 既有端点逐个在表内且标记受保护。
// 逆改点：漏登记任一既有端点或把方法写错 → 对应子例红。
func TestRouteTable_ExistingEndpointsEnumerated(t *testing.T) {
	s := newTestServer(t, nil)

	want := []string{
		"GET /api/v1/health",
		"GET /api/v1/info",
		"GET /api/v1/profiles",
		"GET /api/v1/tools",
		"GET /api/v1/memory",
		"POST /api/v1/sessions",
		"GET /api/v1/sessions/{id}",
		"DELETE /api/v1/sessions/{id}",
		"POST /api/v1/sessions/{id}/messages",
		"POST /api/v1/agents/{name}/invoke",
		"GET /api/v1/schedules",
		"PUT /api/v1/schedules/{id}",
		"GET /api/v1/schedules/{id}/executions",
		"POST /api/v1/schedules/{id}/run",
	}
	got := map[string]routeVisibility{}
	for _, rt := range s.routeTable() {
		got[routeKey(rt)] = rt.visibility
	}
	for _, key := range want {
		visibility, ok := got[key]
		require.True(t, ok, "路由表缺少 %s", key)
		wantVisibility := routeProtected
		if key == "GET /api/v1/health" {
			wantVisibility = routePublic
		}
		require.Equal(t, wantVisibility, visibility, "%s 的可见性", key)
	}
}

// TestRouteTable_NoDuplicateEntries 同一 (方法, 模式) 只允许登记一次——重复登记在
// ServeMux 上会 panic，而在表上会掩盖后一条被覆盖的事实。
// 逆改点：复制粘贴出第二条相同路由 → 本用例红。
func TestRouteTable_NoDuplicateEntries(t *testing.T) {
	s := newTestServer(t, nil)

	seen := map[string]bool{}
	for _, rt := range s.routeTable() {
		key := routeKey(rt)
		require.False(t, seen[key], "路由重复登记：%s", key)
		seen[key] = true
	}
}

// TestRouteTable_FallbackIsUniqueAndMethodAgnostic 兜底项恰一条、方法为空、
// 模式为 "/"：未匹配的路径与方法统一落到 404 信封（注 3）。
// 逆改点：给兜底项写死方法 → 未知动词会得到纯文本 405 而非信封。
func TestRouteTable_FallbackIsUniqueAndMethodAgnostic(t *testing.T) {
	s := newTestServer(t, nil)

	var fallbacks []route
	for _, rt := range s.routeTable() {
		if rt.visibility == routeFallback {
			fallbacks = append(fallbacks, rt)
		}
	}
	require.Len(t, fallbacks, 1, "兜底项必须唯一")
	require.Empty(t, fallbacks[0].method, "兜底项必须方法无关")
	require.Equal(t, "/", fallbacks[0].pattern)
}

// TestRouteTable_ProtectedMethodsAreExplicit 受保护项必须显式给出合法动词且挂在
// /api/ 下——空方法会把受保护端点变成"任意动词可命中"。
// 逆改点：漏写方法或把受保护项挂到非 /api/ 路径 → 本用例红。
func TestRouteTable_ProtectedMethodsAreExplicit(t *testing.T) {
	s := newTestServer(t, nil)

	allowed := map[string]bool{
		http.MethodGet: true, http.MethodPost: true,
		http.MethodPut: true, http.MethodDelete: true,
	}
	for _, rt := range s.routeTable() {
		if rt.visibility != routeProtected {
			continue
		}
		require.True(t, allowed[rt.method], "受保护路由方法非法：%s", routeKey(rt))
		require.True(t, strings.HasPrefix(rt.pattern, "/api/"),
			"受保护路由必须挂在 /api/ 下：%s", routeKey(rt))
	}
}

// TestRouteTable_RouterRegistersEveryEntry 注册与枚举同源：mux 对每一条非兜底项
// 都能解析出与表中逐字相同的模式。
// 逆改点：routes() 与 routeTable() 各写一份清单 → 缺注册的项解析成兜底 "/"。
func TestRouteTable_RouterRegistersEveryEntry(t *testing.T) {
	s := newTestServer(t, nil)

	mux := http.NewServeMux()
	s.routes(mux)
	for _, rt := range s.routeTable() {
		if rt.visibility == routeFallback {
			continue
		}
		req := httptest.NewRequest(rt.method, concretePath(rt.pattern), nil)
		_, pattern := mux.Handler(req)
		require.Equal(t, routeKey(rt), pattern, "路由未按表注册")
	}
}

// TestRouteTable_UnknownPathAndMethodFallBack 未知路径与未知动词都落到兜底：
// 不暴露 ServeMux 的纯文本 404 与自动 405。
// 逆改点：去掉 catch-all 或给 "GET /" 加方法限定 → 对应探测得不到 "/"。
func TestRouteTable_UnknownPathAndMethodFallBack(t *testing.T) {
	s := newTestServer(t, nil)

	mux := http.NewServeMux()
	s.routes(mux)

	cases := []struct{ method, path string }{
		{http.MethodGet, "/nope"},
		{http.MethodPost, "/api/v1/nope"},
		{http.MethodPatch, "/api/v1/health"}, // 已知路径的未知动词
	}
	for _, tc := range cases {
		_, pattern := mux.Handler(httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, "/", pattern, "%s %s 必须落到兜底", tc.method, tc.path)
	}
}

// TestRouteTable_HealthNeedsNoCredentials 公开面唯一 API（健康检查）在无任何
// 凭据时可直接返回成功信封——它是公开闭集的存在性证明。
// 逆改点：把 health 标成受保护并接入未实现认证 → 本用例红。
func TestRouteTable_HealthNeedsNoCredentials(t *testing.T) {
	s := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var env Envelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, codeOK, env.Code)
}

// countingAuth 是 AdminAuth 的计数假件：Wrap 产出的处理函数每次被调用计一次，
// 并以固定 actor 调用 next。它用来证明「受保护路由一律过闸门、公开路由与兜底
// 不过闸门」，与具体守卫的判定逻辑解耦。
// countingAuth counts guard entries and forwards with a fixed actor, isolating the
// mounting matrix from the guard's own decisions.
type countingAuth struct {
	mu    sync.Mutex
	calls int
}

func (c *countingAuth) Wrap(next AdminHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		next(w, r, core.Actor{Subject: "test-subject", Method: core.AuthMethodBearer, Source: "test"})
	})
}

func (c *countingAuth) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestAuth_RouteMatrix 覆盖认证挂载的路由矩阵（模块卡 §4.4 关键回归第 7 条、任务
// T025 的 +8）：非公开路由无凭据一律 401 且零副作用、有效凭据放行、公开面（健康
// 检查 / 根重定向）与兜底 404 不经过闸门、每条受保护项恰过闸门一次、`/` 只做
// 302 且只接受 GET。
// 逆改点：routes() 漏挂任一受保护项 / 把公开路由或兜底也包装 / 根路径改用可吞
// 路径的 "/" 模式 → 对应子例红。
// 中文用例名：非公开端点认证矩阵。
func TestAuth_RouteMatrix(t *testing.T) {
	protectedRows := func(t *testing.T, s *Server) []route {
		t.Helper()
		var rows []route
		for _, rt := range s.routeTable() {
			if rt.visibility == routeProtected {
				rows = append(rows, rt)
			}
		}
		require.Positive(t, len(rows), "路由表必须含受保护端点")
		return rows
	}

	t.Run("every non-public route rejects missing credentials", func(t *testing.T) {
		runs := &fakeRunService{result: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
		store := &fakeSessionStore{sessions: map[string]core.SessionRecord{}, messages: map[string][]core.Message{}}
		svc := &fakeScheduleService{runRes: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
		rows := protectedRows(t, newTestServer(t, nil))

		for _, rt := range rows {
			// 每条路由一个**新装配**：限速窗口按来源累计，同一来源连打十几条
			// 缺凭据请求会在第 10 条起变成 429，掩盖「该路由是否挂了闸门」。
			s := newTestServer(t, func(o *Options) {
				o.AdminAuth = testAdminAuth(testToken)
				o.Runs = runs
				o.Sessions = store
				o.Schedules = svc
			})
			rec := serveAdmin(s, adminRequest(rt.method, concretePath(rt.pattern), ""))
			require.Equal(t, http.StatusUnauthorized, rec.Code, "%s", routeKey(rt))
			require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, rec).Code, "%s", routeKey(rt))
		}

		require.Empty(t, runs.requests(), "未认证请求不得触达运行入口")
		require.Empty(t, runs.archivedIDs(), "未认证请求不得触达归档入口")
		require.Zero(t, store.getOrCreateCallCount(), "未认证请求不得建会话")
		require.Zero(t, store.getSessionCallCount(), "未认证请求不得读会话")
		require.Zero(t, svc.listCallCount(), "未认证请求不得触达调度器")
		require.Zero(t, svc.runCallCount())
	})

	t.Run("valid credentials pass the guard on every protected route", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = testAdminAuth(testToken)
			o.Schedules = &fakeScheduleService{runRes: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
		})
		for _, rt := range protectedRows(t, s) {
			rec := serveAdmin(s, withBearer(adminRequest(rt.method, concretePath(rt.pattern), ""), testToken))
			require.NotEqual(t, http.StatusUnauthorized, rec.Code, "%s 有效凭据不得 401", routeKey(rt))
			require.NotEqual(t, http.StatusForbidden, rec.Code, "%s 有效凭据不得 403", routeKey(rt))
		}
	})

	t.Run("public routes and the 404 fallback bypass the guard", func(t *testing.T) {
		auth := &countingAuth{}
		s := newTestServer(t, func(o *Options) { o.AdminAuth = auth })

		health := serveAdmin(s, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
		require.Equal(t, http.StatusOK, health.Code)
		root := serveAdmin(s, httptest.NewRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusFound, root.Code)
		unknown := serveAdmin(s, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
		require.Equal(t, http.StatusNotFound, unknown.Code)
		require.Equal(t, codeNotFound, decodeAuthEnvelope(t, unknown).Code)

		require.Zero(t, auth.count(), "公开路由与兜底不得进入认证闸门")
	})

	t.Run("every protected route enters the guard exactly once", func(t *testing.T) {
		auth := &countingAuth{}
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = auth
			o.Schedules = &fakeScheduleService{runRes: core.RunResult{RunID: "run-1", Status: core.StatusCompleted}}
		})
		rows := protectedRows(t, s)
		for _, rt := range rows {
			serveAdmin(s, adminRequest(rt.method, concretePath(rt.pattern), ""))
		}
		require.Equal(t, len(rows), auth.count(),
			"受保护路由必须一条不落地经 s.admin 挂载（恰过一次）")
	})

	t.Run("root redirects to the console", func(t *testing.T) {
		s := newTestServer(t, nil)
		rec := serveAdmin(s, httptest.NewRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusFound, rec.Code)
		require.Equal(t, consoleRootPath, rec.Header().Get("Location"))
	})

	t.Run("root is GET-only and other verbs fall back to 404", func(t *testing.T) {
		s := newTestServer(t, nil)
		rec := serveAdmin(s, httptest.NewRequest(http.MethodPost, "/", nil))
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("unknown paths stay 404 even with a guard configured", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
		rec := serveAdmin(s, adminRequest(http.MethodPost, "/api/v1/nope", ""))
		require.Equal(t, http.StatusNotFound, rec.Code, "兜底不参与认证判定")
		require.Equal(t, codeNotFound, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("valid credentials reach the protected handler", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/info", ""), testToken))
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, codeOK, decodeAuthEnvelope(t, rec).Code)
	})
}

// TestConsoleAdmin_WriteRoutesMounted 是 T016 的挂载判卷（check
// `012-web-etag-routes` 的必备锚点）：US-12 的 30 条写面路由全部进入**主路由表**，
// 逐行受保护、恰带一个显式 actor 处理函数；兜底仍唯一且是最后一行；真实 mux 对
// 每一条都能解析回与表中逐字相同的模式——"表里有、注册漏"在本用例直接变红。
// 逆改点：routeTable 不并 adminRoutes / 兜底挪到写面之前 / 任一行改成公开或双处理
// 函数 / 模式被更宽的行遮蔽 → 对应断言红。
// 中文用例名：写面路由并入主路由表。
func TestConsoleAdmin_WriteRoutesMounted(t *testing.T) {
	s := newTestServer(t, nil)
	table := s.routeTable()

	got := make(map[string]route, len(table))
	for _, rt := range table {
		require.NotContains(t, got, routeKey(rt), "路由重复登记：%s", routeKey(rt))
		got[routeKey(rt)] = rt
	}

	for _, key := range wantAdminRouteKeys() {
		rt, ok := got[key]
		require.True(t, ok, "写面路由未并入主路由表：%s", key)
		require.Equal(t, routeProtected, rt.visibility, "%s 必须受保护", key)
		require.NotNil(t, rt.adminHandler, "%s 必须带显式 actor 处理函数", key)
		require.Nil(t, rt.handler, "%s 不得同时挂两个处理函数", key)
	}

	last := table[len(table)-1]
	require.Equal(t, routeFallback, last.visibility, "兜底必须仍是最后一行")
	require.Empty(t, last.method, "兜底必须方法无关")

	mux := http.NewServeMux()
	s.routes(mux)
	for _, key := range wantAdminRouteKeys() {
		rt := got[key]
		req := httptest.NewRequest(rt.method, concreteAdminPath(rt.pattern), nil)
		_, pattern := mux.Handler(req)
		require.Equal(t, key, pattern, "%s 未按表注册或被更宽的模式遮蔽", key)
	}
}

// TestConsoleAdmin_MissingCredentialsRejected：写面并入主表之后，30 条端点逐条
// 无凭据请求一律 401 UNAUTHORIZED——与 `TestAuth_RouteMatrix` 同一口径，但固定
// 写面集合并锁定"并入后的实际可见性"。
// 逆改点：任一行漏挂闸门 / 标成公开 → 对应子例得到非 401。
// 中文用例名：写面缺凭据一律拒绝。
func TestConsoleAdmin_MissingCredentialsRejected(t *testing.T) {
	for _, key := range wantAdminRouteKeys() {
		method, pattern, ok := strings.Cut(key, " ")
		require.True(t, ok, "路由键格式：%s", key)
		// 每条路由一个**新装配**：认证限速按来源累计，同一实例连打会变 429。
		s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
		rec := serveAdmin(s, adminRequest(method, concreteAdminPath(pattern), ""))
		require.Equal(t, http.StatusUnauthorized, rec.Code, "%s", key)
		require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, rec).Code, "%s", key)
	}
}

// TestConsoleAdmin_ConditionalWritesDemandIfMatch：挂载之后 8 条条件写仍要求
// If-Match——缺失与弱 ETag 都在处理函数之前 412；非条件写行不因并入主表而开始
// 要求请求头。认证先于 If-Match 已在 admin_routes_test 锁定，这里判的是**挂载后
// 的实际行为**。
// 逆改点：并入时丢掉 conditional 标记 / 中间件套错方向 → 对应子例红。
// 中文用例名：条件写在主表上仍要求 If-Match。
func TestConsoleAdmin_ConditionalWritesDemandIfMatch(t *testing.T) {
	t.Run("conditional rows reject a missing If-Match", func(t *testing.T) {
		for _, key := range wantConditionalRouteKeys() {
			method, pattern, ok := strings.Cut(key, " ")
			require.True(t, ok, "路由键格式：%s", key)
			s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
			rec := serveAdmin(s, withBearer(adminRequest(method, concreteAdminPath(pattern), ""), testToken))
			require.Equal(t, http.StatusPreconditionFailed, rec.Code, "%s", key)
			require.Equal(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code, "%s", key)
		}
	})

	t.Run("a weak If-Match is rejected on the mounted table", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
		req := withBearer(adminRequest(http.MethodPut, "/api/v1/model-connections/primary", ""), testToken)
		req.Header.Set("If-Match", "W/"+formatETag(strings.Repeat("a", 64)))
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusPreconditionFailed, rec.Code)
		require.Equal(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("non-conditional rows do not demand If-Match", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodPost, "/api/v1/notify-channels", ""), testToken))
		require.NotEqual(t, http.StatusPreconditionFailed, rec.Code)
		require.NotEqual(t, codePreconditionFailed, decodeAuthEnvelope(t, rec).Code)
	})
}

// TestConsoleAdmin_UnassembledPortsFailClosed：写面端口尚未装配时，占位处理
// 函数按固定 500 INTERNAL 失败关闭——**不**伪装成功、**不**给空列表、**不**暴露
// 内部原因。条件写行在 If-Match 形状合法时也必须走到占位（证明 412 只由形状
// 决定，更深的比较属于服务层）。
// 逆改点：占位改成 200 / 空数据 / 把 500 换成本文之外的原因 → 对应子例红。
// 中文用例名：缺端口按固定 500 失败关闭。
func TestConsoleAdmin_UnassembledPortsFailClosed(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.AdminAuth = testAdminAuth(testToken) })

	t.Run("a normal write row answers 500 INTERNAL", func(t *testing.T) {
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodPost, "/api/v1/agents", ""), testToken))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("a conditional row with a well-formed ETag reaches the placeholder", func(t *testing.T) {
		req := withBearer(adminRequest(http.MethodPut, "/api/v1/model-connections/primary", ""), testToken)
		req.Header.Set("If-Match", formatETag(strings.Repeat("a", 64)))
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusInternalServerError, rec.Code,
			"合法 If-Match 必须越过中间件，由占位处理函数按装配缺失拒绝")
		require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	})

	t.Run("a read row answers 500 instead of an empty payload", func(t *testing.T) {
		rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/archived-agents", ""), testToken))
		require.Equal(t, http.StatusInternalServerError, rec.Code)
		require.Equal(t, codeInternal, decodeAuthEnvelope(t, rec).Code)
	})
}
