package web

// 本文件是工作区只读浏览的两条端点（模块卡 §3.1.8 / §3.7.1）：`GET /workspace/tree`
// 与 `GET /workspace/file`。浏览是纯读——**不**写管理操作行、**不**写审计、**不**发
// 模型调用；根固定 `.basalt/agents/`、真实路径投影、软链接不跟随、`.staging` /
// `.backup` 残骸不可见与 256 KiB 上限全部由适配器裁决
// （`internal/app/workspace_browse.go`）。本层只做三件事：把 `path` 原样转交、
// 按错误类型映射状态码、把视图投影为契约字段——**不**在 web 复制第二份路径规则。
// `GET /workspace/file` 的 `path` 是契约必填参数：缺失或空串在触达端口之前就 400，
// 不产生任何读取。`truncated` 恒为 false——超限是 400，不是截断。
// This file serves the two read-only workspace endpoints. Confinement, symlink
// handling, residue filtering and the 256 KiB cap belong to the adapter; this layer
// forwards path verbatim, maps errors by type, and projects the contract fields.

import (
	"errors"
	"io/fs"
	"net/http"

	"github.com/ymedword/basalt/internal/core"
)

// messageWorkspacePathRequired 是 `GET /workspace/file` 缺 `path` 时的固定文案。
// `GET /workspace/tree` 的 path 可省略（缺省为浏览根），不使用本文案。
// messageWorkspacePathRequired is the fixed text for a missing file path.
const messageWorkspacePathRequired = "path 参数必填：请给出相对工作区根的文件路径"

// handleAdminWorkspaceTree 处理 `GET /workspace/tree?path=`：`path` 可省略或为空串
// 表示浏览根，原样交给适配器（`.` 归一化、越界与残骸判定都在那边）；空目录由
// `nonNilWorkspaceTree` 输出 `[]`，不让页面拿到 `null`。
// handleAdminWorkspaceTree forwards an optional path verbatim; an empty tree renders as
// [] rather than null.
func (s *Server) handleAdminWorkspaceTree(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.WorkspaceService == nil {
		s.writeAdminPortMissing(w, "WorkspaceService", "GET /workspace/tree")
		return
	}
	path := r.URL.Query().Get("path")
	tree, err := s.opts.WorkspaceService.Tree(r.Context(), path)
	if err != nil {
		s.writeAdminWorkspaceError(w, err, path, "读取工作区目录")
		return
	}
	s.writeOK(w, nonNilWorkspaceTree(tree))
}

// handleAdminWorkspaceFile 处理 `GET /workspace/file?path=`：`path` 必填，缺失或
// 空串按纯请求错误 400 拒绝，**不**触达端口（因此不产生任何文件系统读取）。
// 成功回 `WorkspaceFileView`，`truncated` 由适配器恒置 false。
// handleAdminWorkspaceFile rejects a missing path before the port, so no filesystem
// read can happen for a request that cannot succeed.
func (s *Server) handleAdminWorkspaceFile(w http.ResponseWriter, r *http.Request, _ core.Actor) {
	if s.opts.WorkspaceService == nil {
		s.writeAdminPortMissing(w, "WorkspaceService", "GET /workspace/file")
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, messageWorkspacePathRequired)
		return
	}
	file, err := s.opts.WorkspaceService.File(r.Context(), path)
	if err != nil {
		s.writeAdminWorkspaceError(w, err, path, "读取工作区文件")
		return
	}
	s.writeOK(w, file)
}

// writeAdminWorkspaceError 把浏览失败按类型映射为固定响应（约束 43；只认
// errors.Is / errors.As，**不**比对文本）：
//   - *core.InvalidInputError → 400 BAD_REQUEST（越界 / 超限 / 二进制 / 目录与文件误用）
//   - core.ErrResourceNotFound / fs.ErrNotExist → 404 NOT_FOUND（不存在、软链接、残骸）
//   - 其余（浏览根打不开、磁盘错误）→ 500 INTERNAL，底层原因只进日志
//
// 404 与"软链接不跟随"共用同一状态码：链接既不是可读路径，也不该让探测者按状态
// 码区分"有链接"与"没文件"。失败信封不带 data（`writeError` 永不设置载荷）。
// writeAdminWorkspaceError distinguishes invalid input from absence by type only;
// symlinks share the 404 so status codes cannot reveal their existence.
func (s *Server) writeAdminWorkspaceError(w http.ResponseWriter, err error, path, action string) {
	var invalidErr *core.InvalidInputError
	if errors.As(err, &invalidErr) {
		s.writeError(w, http.StatusBadRequest, codeBadRequest, invalidErr.Reason)
		return
	}
	if errors.Is(err, core.ErrResourceNotFound) || errors.Is(err, fs.ErrNotExist) {
		s.writeError(w, http.StatusNotFound, codeNotFound, "工作区路径不存在："+path)
		return
	}
	s.logger().Error("工作区浏览失败", "path", path, "action", action, "error", err)
	s.writeError(w, http.StatusInternalServerError, codeInternal, messageInternal)
}
