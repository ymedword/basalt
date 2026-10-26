package web

// 本文件是管理台登录验收的网页侧判卷（14D T093；模块卡 F1 / F3 / F4）：
// 未认证一律 401、没有持久会话、错误密码不回显也不进日志、登录页四件文案固定，
// 以及"401 后跳到登录页并保留 next"的管理台侧接线。
// 真实密码文件的读取与生成属装配层（internal/app 的 TestAdminToken_*）；这里用
// **同一形态**的文件来源值构造闸门，判的是网页侧行为。
// These tests pin the web-side login contract: 401 on every protected route, no
// persistent session, no value echo, and the four fixed copies of the login page.

import (
	"bytes"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newGuardWithLog 构造带可观察日志出口的真实闸门：日志内容（是否含密码值）是判据。
// newGuardWithLog builds a real guard whose log output the test can inspect.
func newGuardWithLog(t *testing.T, token string, buf *bytes.Buffer) AdminAuth {
	t.Helper()
	guard, err := NewAuthGuard(AdminAuthConfig{
		Mode:           adminAuthModeToken,
		Token:          token,
		AllowedHosts:   []string{"127.0.0.1:8080"},
		AllowedOrigins: []string{"http://127.0.0.1:8080"},
	}, slog.New(slog.NewTextHandler(buf, nil)), func() time.Time { return authFixedNow })
	require.NoError(t, err)
	return guard
}

// fileSourcedToken 在临时目录写一个密码文件并读回：形态与实例私有目录的
// `admin-token` 相同（一行密码 + 尾随换行），但**不**触碰真实用户目录。
// fileSourcedToken returns the trimmed value of a freshly written password file.
func fileSourcedToken(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "admin-token")
	require.NoError(t, os.WriteFile(path, []byte(value+"\n"), 0o600))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.TrimSpace(string(raw))
}

// TestAdminToken_Unauthenticated401 覆盖 F1 与 F4：未认证访问管理端点一律 401
// 固定信封、不下发任何 Cookie、Cookie 不构成凭据；正确密码 200 但同样不建立
// 持久会话（刷新后必须重新输入）；错误密码 401 且值不回显、不落日志。
// 逆改点：引入会话 Cookie 或"记住登录" → Cookie 与刷新断言红；401 回显提交值 /
// 日志记录密码 → 回显与日志断言红。
func TestAdminToken_Unauthenticated401(t *testing.T) {
	resolved := fileSourcedToken(t, "file-sourced-admin-password-4a2f")

	t.Run("未认证 → 401 固定信封且不建立会话", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = newGuardWithLog(t, resolved, &bytes.Buffer{})
		})
		rec := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/info", ""))
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		require.Equal(t, codeUnauthorized, decodeAuthEnvelope(t, rec).Code)
		require.Empty(t, rec.Header().Values("Set-Cookie"), "未认证响应不得下发任何 Cookie")
		require.NotContains(t, rec.Body.String(), resolved, "401 不得回显任何密码值")
	})

	t.Run("Cookie 不构成凭据", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = newGuardWithLog(t, resolved, &bytes.Buffer{})
		})
		req := adminRequest(http.MethodGet, "/api/v1/info", "")
		req.AddCookie(&http.Cookie{Name: "basalt_session", Value: resolved})
		rec := serveAdmin(s, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code,
			"管理台没有持久会话：即使 Cookie 里带着正确密码也必须 401")
	})

	t.Run("正确密码 200，但刷新后必须重新输入", func(t *testing.T) {
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = newGuardWithLog(t, resolved, &bytes.Buffer{})
		})
		ok := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/info", ""), resolved))
		require.Equal(t, http.StatusOK, ok.Code, "密码文件来源的值必须能登录")
		require.Empty(t, ok.Header().Values("Set-Cookie"), "成功响应也不得下发会话 Cookie")

		again := serveAdmin(s, adminRequest(http.MethodGet, "/api/v1/info", ""))
		require.Equal(t, http.StatusUnauthorized, again.Code,
			"刷新（不带凭据）必须重新输入——不得有服务端会话兜底")
	})

	t.Run("错误密码 401 且不回显值、日志无值", func(t *testing.T) {
		var logs bytes.Buffer
		s := newTestServer(t, func(o *Options) {
			o.AdminAuth = newGuardWithLog(t, resolved, &logs)
		})
		const wrongValue = "wrong-password-attempt-91cd"
		// 限速语义（约束 7）：窗口内最多记录 max-1 次失败，第 max 次请求起 429。
		for i := 0; i < authFailureLimit-1; i++ {
			rec := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/info", ""), wrongValue))
			require.Equal(t, http.StatusUnauthorized, rec.Code, "第 %d 次失败必须 401", i+1)
			require.NotContains(t, rec.Body.String(), wrongValue, "401 不得回显提交值")
			require.NotContains(t, rec.Body.String(), resolved, "401 不得回显正确值")
		}
		// 第 authFailureLimit 次进入限速：429 同样不得带值，限速 WARN 也不得带值。
		limited := serveAdmin(s, withBearer(adminRequest(http.MethodGet, "/api/v1/info", ""), wrongValue))
		require.Equal(t, http.StatusTooManyRequests, limited.Code, "配额用尽后必须进入限速")
		require.NotContains(t, limited.Body.String(), wrongValue)
		require.Contains(t, logs.String(), "来源进入认证限速", "配额用尽后必须留下限速 WARN")
		require.NotContains(t, logs.String(), wrongValue, "日志不得记录提交的密码值")
		require.NotContains(t, logs.String(), resolved, "日志不得记录正确密码值")
	})
}

// TestAdminLogin_FixedCopy 覆盖 F3 与 F1 的管理台侧接线：登录页四件文案（标题 /
// 字段 / 提示 / 按钮）逐字固定；401 后跳到登录页并保留 next，next 经 safeNextPath
// 清洗后才参与跳转。
// 逆改点：任一文案被改写或删除 → 对应断言红；去掉 next 参数 → next 断言红。
func TestAdminLogin_FixedCopy(t *testing.T) {
	t.Parallel()
	files := consoleSourceFiles(t)

	login, ok := files["views/login.ts"]
	require.True(t, ok, "登录视图源文件必须存在：views/login.ts")
	require.Contains(t, login, "'登录管理台'", "标题必须固定为「登录管理台」")
	require.Contains(t, login, "'访问密码'", "字段必须固定为「访问密码」")
	require.Contains(t, login, "'请输入部署者提供的访问密码'", "提示必须固定")
	require.Contains(t, login, "'登录'", "按钮必须固定为「登录」")

	app, ok := files["app.ts"]
	require.True(t, ok, "控制台入口源文件必须存在：app.ts")
	require.Contains(t, app, "login: '登录管理台'", "路由标题必须与登录页标题一致")

	t.Run("未认证跳转保留 next", func(t *testing.T) {
		require.Contains(t, app, "loginPath(next)", "401 后必须跳到登录页并带上 next")
		router, ok := files["router.ts"]
		require.True(t, ok, "路由源文件必须存在：router.ts")
		require.Contains(t, router, "export function loginPath(")
		require.Contains(t, router, "export function safeNextPath(",
			"next 必须先经站内路径清洗才参与跳转")
	})
}
