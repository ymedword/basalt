package web

// 本文件是中间件链（模块卡 §3.1）：顺序固定 recover → 请求日志 → 跨源保护（约束 21）。
// panic 只把堆栈写进日志、响应仍是固定 500 信封；跨源写拒绝经标准库
// http.CrossOriginProtection 的 deny handler 适配为项目信封，且**不输出任何**
// Access-Control-Allow-* 头（跨域读取默认关闭）。
// The fixed middleware chain; panics never leak stacks to clients and cross-origin
// rejections use the project envelope without any CORS allow headers.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"runtime/debug"
	"time"
)

// requestIDKey 是请求日志标识在 context 里的唯一键。context 只承载追踪标识
// （宪法 VII 的允许面）；业务状态一律走显式参数。
// requestIDKey is the only context key; it carries a tracing id and nothing else.
type requestIDKey struct{}

// requestIDFrom 取请求标识；未设置时返回空串（日志字段缺席，不伪造）。
// requestIDFrom returns the tracing id, or "" when absent.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// recoverMiddleware 是链首：**在这里生成本次请求的追踪标识并注入 context**——
// panic 日志与内层请求日志因此共享同一个非空标识；panic 记 ERROR（标识 + 堆栈，
// 堆栈只进日志），尚未写响应头则补 500 信封，已写头只记日志（注 12）。
// recoverMiddleware generates the tracing id at the head of the chain so panic and
// completion logs share the same non-empty id; it emits the fixed 500 envelope when
// nothing has been written yet.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID()
		// 追踪标识是 context 的允许用途（宪法 VII）；不承载任何业务状态。
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))

		rec, ok := w.(*statusRecorder)
		if !ok {
			rec = &statusRecorder{ResponseWriter: w}
		}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			s.logger().Error("HTTP handler panic",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"panic", v,
				"stack", string(debug.Stack()))
			if !rec.wroteHeader {
				s.writeError(rec, http.StatusInternalServerError, codeInternal, messageInternal)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// requestLogMiddleware 在进出各计一次在途等待组（约束 20 的 ④ 阶段依赖它），并在
// 请求结束时记录 request_id / method / path / status / 耗时。完成日志放在 defer 里：
// panic 时 next 之后的代码不会执行，但这次请求仍须留下完成行（status 记为 500，
// 除非 handler 已写出真实状态码）；defer 只记日志、**不 recover**，panic 继续向上
// 交给链首 recover，原始堆栈不被吞掉。
// requestLogMiddleware counts in-flight handlers and logs one line per request; the
// completion line is deferred so a panicking request still records status 500.
func (s *Server) requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestIDFrom(r.Context())
		if id == "" {
			// 兜底：单独装配本中间件时补一个标识，链首装配下不会走到这里。
			id = requestID()
			r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		}
		started := s.now()
		if !s.beginRequest() {
			// 收尾已开始：不触达路由与共享资源，也不写响应（连接正在被关闭）。
			s.logger().Warn("停机收尾中，拒绝新请求",
				"request_id", id, "method", r.Method, "path", r.URL.Path)
			return
		}
		defer s.endRequest()

		completed := false
		defer func() {
			status := 0
			if rec, ok := w.(*statusRecorder); ok {
				status = rec.status
			}
			if !completed && status == 0 {
				// panic 且尚未写头：链首 recover 将补 500 信封，完成日志记同一状态。
				status = http.StatusInternalServerError
			}
			s.logger().Info("HTTP 请求",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration", s.now().Sub(started).Round(time.Millisecond))
		}()

		next.ServeHTTP(w, r)
		completed = true
	})
}

// crossOriginMiddleware 直接使用标准库跨源保护；无害方法直通，跨源非安全请求由
// deny handler 写 403 信封（零副作用，不读 body）。
// crossOriginMiddleware delegates to the standard library protection.
func (s *Server) crossOriginMiddleware(next http.Handler) http.Handler {
	return s.crossOrigin.Handler(next)
}

// statusRecorder 记录状态码与「是否已写响应头」——panic 后据此决定能否补 500 信封。
// statusRecorder tracks the status and whether the header was written.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader 只在首次调用时落状态码并转发（重复调用是 handler 错误，忽略即可）。
// WriteHeader records and forwards only the first call.
func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

// Write 在隐式 200 时先补记状态码，再转发字节。
// Write records the implicit 200 before forwarding the bytes.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// requestID 生成每请求日志标识（crypto/rand 8 字节 hex；不进入业务签名）。
// requestID generates a per-request tracing id from crypto/rand.
func requestID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 随机源失败时给固定占位而不是时间戳：标识不得可预测（全局不变量 8）。
		return "unknown"
	}
	return hex.EncodeToString(buf[:])
}
