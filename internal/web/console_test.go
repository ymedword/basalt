package web

// 本文件是管理台静态壳的验收（模块卡 §3.1.3、§3.2.1 的 console_test.go 十五例、图 H）：
// 产物集合与逐文件 sha256、深链接回退、未知路径、CSP 与安全头、缓存头、`/` 重定向、
// 无第三方资源引用。用例一律读**真实** go:embed 产物——门禁判的就是要发布的那份。
// Console shell acceptance: artifact integrity, deep-link fallback, security and cache
// headers, root redirect and the no-third-party rule, all against the embedded dist.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/assets"
)

// consoleCSPLiteral 是模块卡 §3.1.3 的逐字策略。测试另抄一份字面量：策略被"顺手
// 放宽"时，常量与断言同时改才通不过——这是契约判据，不是实现镜像。
// consoleCSPLiteral is the card's literal policy duplicated on purpose.
const consoleCSPLiteral = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; " +
	"frame-ancestors 'none'"

// newConsoleTestHandler 用真实内嵌产物构造处理器。
func newConsoleTestHandler(t *testing.T) *consoleHandler {
	t.Helper()
	h, err := NewConsoleHandler(assets.Console())
	require.NoError(t, err)
	return h
}

// firstConsoleAsset 返回 dist/assets 下第一个文件名（含 assets/ 前缀），用于缓存与
// 内容断言；目录为空直接判失败——空产物说明构建没落地。
func firstConsoleAsset(t *testing.T, fsys fs.FS) string {
	t.Helper()
	entries, err := fs.ReadDir(fsys, "assets")
	require.NoError(t, err)
	require.NotEmpty(t, entries, "dist/assets 为空：前端构建产物缺失")
	return "assets/" + entries[0].Name()
}

// consoleAssetWithExt 按扩展名取产物（.js / .css）：管理台样式与脚本同属构建产物，
// 两者都要各自验证 MIME 与长缓存。找不到即判失败——缺了任何一类产物说明样式或
// 脚本没有随构建落地。
// consoleAssetWithExt returns the first bundled asset with the given extension; the
// stylesheet and the script are both build artifacts and each is verified on its own.
func consoleAssetWithExt(t *testing.T, fsys fs.FS, ext string) string {
	t.Helper()
	entries, err := fs.ReadDir(fsys, "assets")
	require.NoError(t, err)
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ext) {
			return "assets/" + entry.Name()
		}
	}
	require.FailNowf(t, "缺少前端产物", "dist/assets 下没有 %s 文件：构建未落地", ext)
	return ""
}

// consoleGet 直接调用处理器（不经路由表），返回响应。
func consoleGet(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestConsole_DeepLinkFallback 逐字落地模块卡 §4.3.8：三条深链接都回退 index.html，
// Content-Type 是 HTML，CSP 含 `default-src 'none'`。
func TestConsole_DeepLinkFallback(t *testing.T) {
	t.Parallel()
	fsys := assets.Console() // 单返回值 fs.FS；构建错误在编译期 / 构建脚本暴露
	h, err := NewConsoleHandler(fsys)
	if err != nil {
		t.Fatalf("NewConsoleHandler: %v", err)
	}

	for _, path := range []string{"/console/", "/console/agents/reporter", "/console/runs/run-1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("%s content-type = %q", path, ct)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
			t.Fatalf("%s CSP = %q", path, csp)
		}
	}
}

// TestConsole_ServesEmbeddedIndex `/console` 与 `/console/` 都直接返回同一份
// index.html（图 H）；前者不靠 ServeMux 的 301 转跳。
func TestConsole_ServesEmbeddedIndex(t *testing.T) {
	t.Parallel()
	fsys := assets.Console()
	index, err := fs.ReadFile(fsys, "index.html")
	require.NoError(t, err)
	h := newConsoleTestHandler(t)

	for _, path := range []string{"/console", "/console/"} {
		rec := consoleGet(t, h, http.MethodGet, path)
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.Equal(t, string(index), rec.Body.String(), path)
		require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"), path)
	}
}

// TestConsole_StaticAssetCacheHeaders 真实产物按不可变长缓存服务，内容与内嵌字节
// 逐字一致（约束 42 的"构建产物就是发布物"）。脚本与样式各自验证：样式丢 MIME
// 或缓存头同样是发布缺陷。
func TestConsole_StaticAssetCacheHeaders(t *testing.T) {
	t.Parallel()
	fsys := assets.Console()
	h := newConsoleTestHandler(t)

	cases := []struct {
		ext         string
		contentType string
	}{
		{ext: ".js", contentType: "javascript"},
		{ext: ".css", contentType: "text/css"},
	}
	for _, tc := range cases {
		asset := consoleAssetWithExt(t, fsys, tc.ext)
		data, err := fs.ReadFile(fsys, asset)
		require.NoError(t, err)

		rec := consoleGet(t, h, http.MethodGet, "/console/"+asset)
		require.Equal(t, http.StatusOK, rec.Code, asset)
		require.Equal(t, consoleImmutable, rec.Header().Get("Cache-Control"), asset)
		require.Contains(t, rec.Header().Get("Content-Type"), tc.contentType, asset)
		require.Equal(t, string(data), rec.Body.String(), asset)
	}
}

// TestConsole_IndexIsNoCache 静态壳用 no-cache：升级后第一次加载就拿到新壳。
func TestConsole_IndexIsNoCache(t *testing.T) {
	t.Parallel()
	h := newConsoleTestHandler(t)

	rec := consoleGet(t, h, http.MethodGet, "/console/")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, consoleNoCache, rec.Header().Get("Cache-Control"))
}

// TestConsole_UnknownPathFallsBackToIndex 未知深链接与缺失资源都回退静态壳：服务端
// 不解析前端路由，也不借 404 暴露目录结构（约束 41）。
func TestConsole_UnknownPathFallsBackToIndex(t *testing.T) {
	t.Parallel()
	fsys := assets.Console()
	index, err := fs.ReadFile(fsys, "index.html")
	require.NoError(t, err)
	h := newConsoleTestHandler(t)

	for _, path := range []string{"/console/nope", "/console/agents/reporter/extra", "/console/assets/missing.js"} {
		rec := consoleGet(t, h, http.MethodGet, path)
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.Equal(t, string(index), rec.Body.String(), path)
		require.Equal(t, consoleNoCache, rec.Header().Get("Cache-Control"), path)
	}
}

// TestConsole_SecurityHeaders 每个响应都带模块卡 §3.1.3 的三项安全头，CSP 逐字相等。
func TestConsole_SecurityHeaders(t *testing.T) {
	t.Parallel()
	fsys := assets.Console()
	asset := firstConsoleAsset(t, fsys)
	h := newConsoleTestHandler(t)

	for _, path := range []string{"/console/", "/console/runs/run-1", "/console/" + asset} {
		rec := consoleGet(t, h, http.MethodGet, path)
		require.Equal(t, consoleCSPLiteral, rec.Header().Get("Content-Security-Policy"), path)
		require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		require.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"), path)
	}
}

// TestConsole_HeadRequest HEAD 有头无体（浏览器预检与缓存探测走这条路）。
func TestConsole_HeadRequest(t *testing.T) {
	t.Parallel()
	h := newConsoleTestHandler(t)

	rec := consoleGet(t, h, http.MethodHead, "/console/agents/reporter")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Body.String())
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, consoleCSPLiteral, rec.Header().Get("Content-Security-Policy"))
}

// TestConsole_MethodNotAllowed 公开面只登记 GET（约束 8）：处理器被直接调用时
// 对其他动词一律 405 并给出 Allow。
func TestConsole_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newConsoleTestHandler(t)

	rec := consoleGet(t, h, http.MethodPost, "/console/")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Contains(t, rec.Header().Get("Allow"), http.MethodGet)
}

// TestConsole_RejectsTraversal 路径穿越既不能读到 dist 之外的文件，也不能绕过
// SPA 回退：非法路径按静态壳返回（约束 40 / 43 的纵深防御）。
func TestConsole_RejectsTraversal(t *testing.T) {
	t.Parallel()
	h := newConsoleTestHandler(t)

	for _, path := range []string{
		"/console/../go.mod",
		"/console/assets/../../go.mod",
		"/console/assets/..%2f..%2fgo.mod",
	} {
		rec := consoleGet(t, h, http.MethodGet, path)
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.NotContains(t, rec.Body.String(), "module github.com", path)
		require.Contains(t, rec.Header().Get("Content-Type"), "text/html", path)
	}
}

// TestConsoleDist_IntegrityMatchesFiles 产物文件集合与 integrity.json 的 path 集合
// 完全一致，且逐文件 sha256 相等（约束 42 的 Go 侧判据）。
func TestConsoleDist_IntegrityMatchesFiles(t *testing.T) {
	t.Parallel()
	fsys := assets.Console()

	raw, err := fs.ReadFile(fsys, "integrity.json")
	require.NoError(t, err)
	var manifest map[string]string
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.NotEmpty(t, manifest)

	files := map[string]bool{}
	err = fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || path == "integrity.json" {
			return nil
		}
		files[path] = true
		return nil
	})
	require.NoError(t, err)

	manifestPaths := make([]string, 0, len(manifest))
	for path := range manifest {
		manifestPaths = append(manifestPaths, path)
	}
	filePaths := make([]string, 0, len(files))
	for path := range files {
		filePaths = append(filePaths, path)
	}
	require.ElementsMatch(t, manifestPaths, filePaths, "产物集合与 integrity.json 不一致")

	for path := range files {
		data, readErr := fs.ReadFile(fsys, path)
		require.NoError(t, readErr, path)
		sum := sha256.Sum256(data)
		require.Equal(t, hex.EncodeToString(sum[:]), manifest[path], "%s 的 sha256 与 integrity.json 不一致", path)
	}
}

// TestConsoleDist_NoStrayFiles 产物目录只允许三类文件：index.html、assets/ 下的
// 单层文件、integrity.json——源码、node_modules 与锁文件不得混进二进制（约束 43）。
// 逆改点：把任意源码文件拷进 dist → 本用例红。
func TestConsoleDist_NoStrayFiles(t *testing.T) {
	t.Parallel()
	fsys := assets.Console()

	var paths []string
	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, paths)

	for _, path := range paths {
		switch {
		case path == "index.html":
		case path == "integrity.json":
		case strings.HasPrefix(path, "assets/") && !strings.Contains(strings.TrimPrefix(path, "assets/"), "/"):
		default:
			t.Errorf("dist 中的游离文件：%s", path)
		}
	}
}

// TestConsoleDist_NoExternalReferences 静态壳不得引用第三方运行时资源（约束 40）：
// 没有 http(s) 外链、没有协议相对地址，脚本与样式一律走 `/console/assets/`。
func TestConsoleDist_NoExternalReferences(t *testing.T) {
	t.Parallel()
	fsys := assets.Console()

	index, err := fs.ReadFile(fsys, "index.html")
	require.NoError(t, err)
	html := string(index)

	require.NotContains(t, html, "http://")
	require.NotContains(t, html, "https://")
	require.NotContains(t, html, `src="//`)
	require.NotContains(t, html, `href="//`)
	require.Contains(t, html, `src="/console/assets/`, "脚本必须来自内嵌产物")
}

// TestConsole_ServerServesShellWithoutCredentials 公开面闭集（约束 8）：管理台静态壳
// 与深链接在**无任何凭据**时经完整中间件链返回 200——登录态由前端掌握，壳本身公开。
func TestConsole_ServerServesShellWithoutCredentials(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, nil)

	for _, path := range []string{"/console/", "/console/agents/reporter", "/console/runs/run-1"} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, rec.Code, path)
		require.Contains(t, rec.Header().Get("Content-Type"), "text/html", path)
		require.Equal(t, consoleCSPLiteral, rec.Header().Get("Content-Security-Policy"), path)
	}
}

// TestConsole_RootRedirectsToConsole 根路径重定向到管理台（图 H）：`GET /` → 302
// `/console/`，不带任何状态。
func TestConsole_RootRedirectsToConsole(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, consoleRootPath, rec.Header().Get("Location"))
}

// TestConsole_PostFallsToNotFound 静态壳只接受 GET：POST 落到兜底 404 信封，不暴露
// ServeMux 的纯文本 405（注 3）。
func TestConsole_PostFallsToNotFound(t *testing.T) {
	t.Parallel()
	s := newTestServer(t, nil)

	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/console/", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")

	var envelope struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, codeNotFound, envelope.Code)
}
