package web

// 本文件是工作区只读浏览的 handler 级验收（模块卡 §3.2.1 web 行：6 例）。判据分三层：
// ① 契约形状——目录 tree 的条目字段逐项相等、空目录输出 `[]` 而不是 `null`、文件
// 视图 `truncated` 显式且恒 false；② 错误映射——越界 / 超限是 400 `BAD_REQUEST`、
// 不存在 / 软链接 / 残骸统一 404 `NOT_FOUND`，失败信封不带 data；③ 本层零副作用
// ——`path` 为空的文件请求在触达端口之前就被拒绝，端口调用次数为零；路径原样转交，
// web 层不做归一化、不读磁盘、不合成条目。
// This file is the handler-level acceptance surface for the workspace endpoints:
// contract shapes, error mapping, and the absence of handler side effects. All path
// rules stay in the adapter, so the fake port records exactly what it receives.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// errFakeWorkspaceUnused 标记用例没有为该端口方法准备行为。
// errFakeWorkspaceUnused marks a port method the case never intended to call.
var errFakeWorkspaceUnused = errors.New("fake workspace: unexpected call")

// fakeWorkspaceService 是可注入行为的 `WorkspaceService` 替身，并记录每次调用的
// 方法与路径——用来断言 web 层原样转交、且纯请求错误零调用。
// fakeWorkspaceService records each call so the cases can assert verbatim forwarding
// and zero calls on pure request errors.
type fakeWorkspaceService struct {
	treeFn func(context.Context, string) (WorkspaceTreeView, error)
	fileFn func(context.Context, string) (WorkspaceFileView, error)
	calls  []string
}

// Tree 记录并委托目录树行为。
func (f *fakeWorkspaceService) Tree(ctx context.Context, path string) (WorkspaceTreeView, error) {
	f.calls = append(f.calls, "tree:"+path)
	if f.treeFn == nil {
		return nil, errFakeWorkspaceUnused
	}
	return f.treeFn(ctx, path)
}

// File 记录并委托文件预览行为。
func (f *fakeWorkspaceService) File(ctx context.Context, path string) (WorkspaceFileView, error) {
	f.calls = append(f.calls, "file:"+path)
	if f.fileFn == nil {
		return WorkspaceFileView{}, errFakeWorkspaceUnused
	}
	return f.fileFn(ctx, path)
}

// 编译期断言：假件实现被测端口——端口漂移在编译期就红。
var _ WorkspaceService = (*fakeWorkspaceService)(nil)

// workspaceHarness 是工作区用例的装配：真实 handler 链 + 注入浏览端口。
// workspaceHarness wires the real handler chain over the injected browse port.
type workspaceHarness struct {
	*Server
	fake *fakeWorkspaceService
}

// newWorkspaceHarness 构造固定时钟、注入浏览端口的服务器。
func newWorkspaceHarness(t *testing.T, mutate func(*Options)) *workspaceHarness {
	t.Helper()
	fake := &fakeWorkspaceService{}
	srv := newTestServer(t, func(o *Options) {
		o.AdminAuth = resourceAuth{}
		o.WorkspaceService = fake
		if mutate != nil {
			mutate(o)
		}
	})
	return &workspaceHarness{Server: srv, fake: fake}
}

// workspaceCall 发送一个已认证的工作区只读请求。
func workspaceCall(method, path string) *http.Request {
	return resourceCall(method, path, "")
}

// decodeWorkspaceTree 解码成功信封里的目录条目数组。
func decodeWorkspaceTree(t *testing.T, rec *httptest.ResponseRecorder) WorkspaceTreeView {
	t.Helper()
	var tree WorkspaceTreeView
	require.NoError(t, json.Unmarshal(decodeResourceEnvelope(t, rec).Data, &tree),
		"body=%s", rec.Body.String())
	return tree
}

// TestAdminWorkspace_TreeSkipsStaging：缺省 path 是浏览根且以空串原样进端口；条目
// 逐项等于端口返回值（本层不合成、不过滤——残骸过滤属于适配器）；直接请求残骸目录
// 与"不存在"同一 404；空目录输出 `[]`，不是 `null`。
// 逆改点：handler 自行读盘补条目 / 缺省路径改成 "." / 空 tree 输出 null / 残骸改成
// 200 或 500 → 对应断言红。
// TestAdminWorkspace_TreeSkipsStaging: the handler forwards the path verbatim, renders
// the port's entries unchanged, maps residue to the same 404 as absence, and emits []
// for an empty tree.
func TestAdminWorkspace_TreeSkipsStaging(t *testing.T) {
	t.Parallel()
	h := newWorkspaceHarness(t, nil)
	h.fake.treeFn = func(_ context.Context, path string) (WorkspaceTreeView, error) {
		switch path {
		case "":
			return WorkspaceTreeView{
				{Path: "writer", Type: "dir", Size: 0},
				{Path: "writer/AGENT.md", Type: "file", Size: 13},
			}, nil
		case "writer.staging", "writer.backup":
			return nil, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
		default:
			return nil, errFakeWorkspaceUnused
		}
	}

	rec := serveAdmin(h.Server, workspaceCall(http.MethodGet, "/api/v1/workspace/tree"))
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	require.Equal(t, WorkspaceTreeView{
		{Path: "writer", Type: "dir", Size: 0},
		{Path: "writer/AGENT.md", Type: "file", Size: 13},
	}, decodeWorkspaceTree(t, rec))
	// 残骸不出现，且缺省 path 以空串原样进端口（归一化不发生在 web 层）。
	require.NotContains(t, rec.Body.String(), ".staging")
	require.NotContains(t, rec.Body.String(), ".backup")
	require.Equal(t, []string{"tree:"}, h.fake.calls)

	// 直接请求残骸目录：与"不存在"同一状态码，不暴露磁盘结构。
	for _, residue := range []string{"writer.staging", "writer.backup"} {
		rec = serveAdmin(h.Server, workspaceCall(http.MethodGet, "/api/v1/workspace/tree?path="+residue))
		require.Equal(t, http.StatusNotFound, rec.Code, residue)
		require.Equal(t, codeNotFound, decodeResourceEnvelope(t, rec).Code, residue)
	}

	// 空目录输出 []：页面拿到空数组而不是 null（端口返回 nil 时由投影兜底）。
	h.fake.treeFn = func(context.Context, string) (WorkspaceTreeView, error) {
		return nil, nil
	}
	rec = serveAdmin(h.Server, workspaceCall(http.MethodGet, "/api/v1/workspace/tree?path=writer/empty"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"data":[]`)
	require.NotContains(t, rec.Body.String(), `"data":null`)
}

// TestAdminWorkspace_FilePreview：文件预览按端口返回值逐字段出站，`truncated`
// 显式且恒 false；`path` 原样转交。
// 逆改点：省略 truncated / 置 true / 重写 path → 对应断言红。
// TestAdminWorkspace_FilePreview keeps the preview fields verbatim and emits
// truncated:false explicitly.
func TestAdminWorkspace_FilePreview(t *testing.T) {
	t.Parallel()
	h := newWorkspaceHarness(t, nil)
	content := "# 记忆\n"
	h.fake.fileFn = func(_ context.Context, path string) (WorkspaceFileView, error) {
		if path != "writer/memory/core.md" {
			return WorkspaceFileView{}, errFakeWorkspaceUnused
		}
		return WorkspaceFileView{Path: path, Content: content, Size: int64(len(content))}, nil
	}

	rec := serveAdmin(h.Server, workspaceCall(http.MethodGet, "/api/v1/workspace/file?path=writer/memory/core.md"))
	require.Equal(t, http.StatusOK, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeOK, env.Code)
	var file WorkspaceFileView
	require.NoError(t, json.Unmarshal(env.Data, &file), "body=%s", rec.Body.String())
	require.Equal(t, WorkspaceFileView{
		Path:      "writer/memory/core.md",
		Content:   content,
		Size:      int64(len(content)),
		Truncated: false,
	}, file)
	// truncated 显式出现且为 false：契约不接受"省略"或 true。
	require.Contains(t, rec.Body.String(), `"truncated":false`)
	require.Equal(t, []string{"file:writer/memory/core.md"}, h.fake.calls)
}

// TestAdminWorkspace_FileRequiresPath：文件端点的 `path` 是契约必填参数——缺失或
// 空串是纯请求错误，400 且端口零调用（不可能产生文件系统读取）。
// 逆改点：把空 path 交给端口 / 改成 404 / 文案不带 path → 对应断言红。
// TestAdminWorkspace_FileRequiresPath rejects a missing path before the port.
func TestAdminWorkspace_FileRequiresPath(t *testing.T) {
	t.Parallel()
	h := newWorkspaceHarness(t, nil)
	for _, target := range []string{
		"/api/v1/workspace/file",
		"/api/v1/workspace/file?path=",
	} {
		rec := serveAdmin(h.Server, workspaceCall(http.MethodGet, target))
		require.Equal(t, http.StatusBadRequest, rec.Code, target)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeBadRequest, env.Code, target)
		require.Equal(t, messageWorkspacePathRequired, env.Message, target)
	}
	require.Empty(t, h.fake.calls, "纯请求校验不得触达端口")
}

// TestAdminWorkspace_NotFound：目录树与文件的不存在路径都映射 404 `NOT_FOUND`，
// 点名请求路径，失败信封不带 data。
// 逆改点：不存在映射成 400 / 500，或失败信封携带 data → 对应断言红。
// TestAdminWorkspace_NotFound maps absence to 404 for both endpoints and names the
// requested path without carrying data.
func TestAdminWorkspace_NotFound(t *testing.T) {
	t.Parallel()
	h := newWorkspaceHarness(t, nil)
	h.fake.treeFn = func(context.Context, string) (WorkspaceTreeView, error) {
		return nil, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
	}
	h.fake.fileFn = func(context.Context, string) (WorkspaceFileView, error) {
		return WorkspaceFileView{}, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
	}

	for _, target := range []string{
		"/api/v1/workspace/tree?path=ghost",
		"/api/v1/workspace/file?path=ghost/AGENT.md",
	} {
		rec := serveAdmin(h.Server, workspaceCall(http.MethodGet, target))
		require.Equal(t, http.StatusNotFound, rec.Code, target)
		env := decodeResourceEnvelope(t, rec)
		require.Equal(t, codeNotFound, env.Code, target)
		require.Contains(t, env.Message, strings.SplitN(target, "path=", 2)[1], target)
		require.Empty(t, env.Data, target)
	}
}

// TestAdminWorkspace_SizeCap：超限由适配器判为 `*core.InvalidInputError` → 400
// `BAD_REQUEST`，文案点名 256 KiB；响应没有正文、没有 `truncated:true`——超限
// **不**截断。
// 逆改点：映射成 200 + truncated:true / 丢弃适配器文案 → 对应断言红。
// TestAdminWorkspace_SizeCap maps an oversized file to 400 without any content or a
// truncated flag.
func TestAdminWorkspace_SizeCap(t *testing.T) {
	t.Parallel()
	h := newWorkspaceHarness(t, nil)
	h.fake.fileFn = func(context.Context, string) (WorkspaceFileView, error) {
		return WorkspaceFileView{}, &core.InvalidInputError{
			Tool:   "workspace.browse",
			Reason: "文件超过 256 KiB 上限，拒绝读取（不截断）",
		}
	}

	rec := serveAdmin(h.Server, workspaceCall(http.MethodGet, "/api/v1/workspace/file?path=writer/big.log"))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	env := decodeResourceEnvelope(t, rec)
	require.Equal(t, codeBadRequest, env.Code)
	require.Contains(t, env.Message, "256")
	require.Empty(t, env.Data)
	require.NotContains(t, rec.Body.String(), `"truncated":true`)
	require.NotContains(t, rec.Body.String(), `"content"`)
}

// TestAdminWorkspace_SymlinkNotFollowed：软链接只在目录树里以 `type=symlink`、
// size 0 出现；读取链接目标、下钻链接目录与读取其下文件都按不存在处理（404），
// 响应不含目标内容。
// 逆改点：跟随链接返回 200 / 把链接类型改成 file / 泄露目标正文 → 对应断言红。
// TestAdminWorkspace_SymlinkNotFollowed lists links by type only and refuses every
// traversal through them.
func TestAdminWorkspace_SymlinkNotFollowed(t *testing.T) {
	t.Parallel()
	h := newWorkspaceHarness(t, nil)
	h.fake.treeFn = func(_ context.Context, path string) (WorkspaceTreeView, error) {
		if path != "writer" {
			return nil, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
		}
		return WorkspaceTreeView{
			{Path: "writer/leak.txt", Type: "symlink", Size: 0},
			{Path: "writer/leakdir", Type: "symlink", Size: 0},
		}, nil
	}
	h.fake.fileFn = func(context.Context, string) (WorkspaceFileView, error) {
		return WorkspaceFileView{}, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
	}

	rec := serveAdmin(h.Server, workspaceCall(http.MethodGet, "/api/v1/workspace/tree?path=writer"))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"type":"symlink"`)
	require.NotContains(t, rec.Body.String(), "TOP-SECRET")

	for _, target := range []string{
		"/api/v1/workspace/file?path=writer/leak.txt",
		"/api/v1/workspace/tree?path=writer/leakdir",
		"/api/v1/workspace/file?path=writer/leakdir/secret.txt",
	} {
		rec = serveAdmin(h.Server, workspaceCall(http.MethodGet, target))
		require.Equal(t, http.StatusNotFound, rec.Code, target)
		require.Empty(t, decodeResourceEnvelope(t, rec).Data, target)
		require.NotContains(t, rec.Body.String(), "TOP-SECRET", target)
	}
}
