package web

// 本文件是管理台静态壳（模块卡 §3.1.3、图 H、约束 41 / 42 / 43）：产物来自 go:embed，
// 运行时不读磁盘。`/console/assets/*` 命中真实文件按不可变长缓存直接服务，其余
// `/console/*` 一律回退 index.html，由前端 History 路由自行解析——服务端**不**解析
// 前端路由，也不为深链接返回 404。每个响应都带 CSP 与安全头：模型文本由前端安全子集
// 渲染，CSP 是最后一道防线（约束 40）。
// This file serves the embedded console shell: immutable assets under /console/assets/,
// index.html for every other console path, and the CSP on every response.

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// consoleCSP 逐字取自模块卡 §3.1.3：默认拒绝一切，只放行同源脚本与样式；不允许内联
// 脚本、外部连接与框架嵌套。`style-src` 的 'unsafe-inline' 是 Vue 运行时写内联样式
// 属性的最低要求，脚本侧**不**放宽。
// consoleCSP is the literal policy from the module card; scripts stay same-origin only.
const consoleCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; " +
	"frame-ancestors 'none'"

const (
	consoleIndexPath   = "index.html"
	consoleAssetPrefix = "assets/"
	// consoleImmutable 只发给带内容哈希的构建产物：升级后文件名变化，旧缓存自然失效。
	// consoleImmutable is only for hashed build output; a new build changes the name.
	consoleImmutable = "public, max-age=31536000, immutable"
	// consoleNoCache 发给静态壳：升级后必须立刻取到引用新资源的 index.html。
	// consoleNoCache keeps the shell fresh so it always points at the new bundle.
	consoleNoCache = "no-cache"
)

// consoleContentTypes 显式钉死 MIME：不查系统注册表（Windows 与容器读数不同会让
// 同一二进制吐出不同 Content-Type），未收录的扩展名一律按字节流处理。
// consoleContentTypes pins MIME types so the binary behaves the same on every host.
var consoleContentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
}

// consoleHandler 提供管理台静态壳。index 在构造期读入内存，避免每次深链接都解一遍
// embed 目录；modTime 取 index.html 的文件时间（embed 下为零值，ServeContent 因此
// 不发 Last-Modified，缓存判定完全落在 Cache-Control 上）。
// consoleHandler serves the shell; index and modTime are captured once at construction.
type consoleHandler struct {
	fsys    fs.FS
	index   []byte
	modTime time.Time
}

// NewConsoleHandler 校验管理台产物可用性并缓存静态壳。fsys 为空或缺少 index.html
// 时返回错误——装配期失败，而不是等第一个请求才 404（失败关闭）。
// NewConsoleHandler validates the asset tree up front; a missing shell fails startup.
func NewConsoleHandler(fsys fs.FS) (*consoleHandler, error) {
	if fsys == nil {
		return nil, errors.New("web: 管理台资源为空")
	}
	index, err := fs.ReadFile(fsys, consoleIndexPath)
	if err != nil {
		return nil, fmt.Errorf("web: 读取管理台静态壳: %w", err)
	}
	info, err := fs.Stat(fsys, consoleIndexPath)
	if err != nil {
		return nil, fmt.Errorf("web: 读取管理台静态壳元信息: %w", err)
	}
	return &consoleHandler{fsys: fsys, index: index, modTime: info.ModTime()}, nil
}

// ServeHTTP 按图 H 分派：`/console/assets/*` 命中真实文件才直接服务，其余路径（含
// 未知深链接与缺失资源）回退 index.html。缺失的构建产物由 `TestConsoleDist_*` 在
// 门禁里判卷，不靠线上 404 暴露目录结构。
// ServeHTTP serves real files under /console/assets/ and falls back to the shell for
// everything else, including unknown deep links and missing bundles.
func (h *consoleHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	writeConsoleHeaders(w)

	rel := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(consoleRootPath, "/"))
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || !strings.HasPrefix(rel, consoleAssetPrefix) || !fs.ValidPath(rel) {
		h.serveIndex(w, r)
		return
	}
	data, err := fs.ReadFile(h.fsys, rel)
	if err != nil {
		// embed 目录里的缺失只可能是版本不一致（壳引用了未打包的文件），按 SPA 回退
		// 处理并保持与深链接同一条路径；真正的产物完整性问题由门禁用例判卷。
		// A missing bundle means the shell and the artifact disagree; fall back and let
		// the integrity gate catch it instead of shaping a 404 around it.
		h.serveIndex(w, r)
		return
	}
	w.Header().Set("Cache-Control", consoleImmutable)
	h.serveFile(w, r, rel, data)
}

// serveIndex 发送静态壳：`no-cache` 让升级后立即取到新壳，安全头已在 ServeHTTP 设置。
// serveIndex sends the shell with no-cache so an upgrade lands on the next load.
func (h *consoleHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", consoleNoCache)
	w.Header().Set("Content-Type", consoleContentTypes[".html"])
	http.ServeContent(w, r, consoleIndexPath, h.modTime, bytes.NewReader(h.index))
}

// serveFile 发送构建产物；Content-Type 先显式设置，ServeContent 因此不再查系统 MIME。
// serveFile serves a bundle file with a pinned content type.
func (h *consoleHandler) serveFile(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	contentType, ok := consoleContentTypes[path.Ext(name)]
	if !ok {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, name, h.modTime, bytes.NewReader(data))
}

// writeConsoleHeaders 写入模块卡 §3.1.3 的三个安全响应头：CSP 限制资源来源，nosniff
// 阻止浏览器改写 Content-Type，no-referrer 避免把管理台路径外带。
// writeConsoleHeaders applies the three security headers from the module card.
func writeConsoleHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Security-Policy", consoleCSP)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
}
