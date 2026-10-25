package main

// docs-readonly 的协议 / 越界 / 只读测试（颗粒度文档 §3.9 / §4.3 `docs-readonly-tests`）。
// 全部直接驱动 `serve(in, out, root)`，不启动子进程：本 module 零依赖，可用
// `go -C examples/shared/mcp/docs-readonly test -count=1 ./...` 单独运行，
// 根 `go test ./...` 覆盖它（2026-10-26 并回主 module；仍不被产品二进制导入）。
// Every test drives serve(in, out, root) in-process; no child processes are started.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// --- 会话驱动 ---------------------------------------------------------------

// rpcMessage 是任意一条响应行的通用解码结果。
type rpcMessage map[string]any

// runSession 把若干请求行拼成 stdin，跑一次完整会话并逐行解析 stdout。
// 空行与通知不产生响应，因此输出行数可能少于输入行数。
func runSession(t *testing.T, root string, requests ...string) []rpcMessage {
	t.Helper()
	var out bytes.Buffer
	if err := serve(strings.NewReader(strings.Join(requests, "\n")+"\n"), &out, root); err != nil {
		t.Fatalf("serve 返回错误: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	messages := make([]rpcMessage, 0, len(lines))
	for _, line := range lines {
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("响应不是合法 JSON: %v（原文 %q）", err, line)
		}
		messages = append(messages, msg)
	}
	return messages
}

func requestLine(id int, method string, params any) string {
	payload, err := json.Marshal(params)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, payload)
}

func toolCallLine(id int, name string, arguments map[string]any) string {
	return requestLine(id, "tools/call", map[string]any{"name": name, "arguments": arguments})
}

func resultOf(t *testing.T, msg rpcMessage) map[string]any {
	t.Helper()
	result, ok := msg["result"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 result 对象：%v", msg)
	}
	return result
}

func errorOf(t *testing.T, msg rpcMessage) (int, string) {
	t.Helper()
	rpcErr, ok := msg["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象：%v", msg)
	}
	code, ok := rpcErr["code"].(float64)
	if !ok {
		t.Fatalf("error.code 不是数字：%v", rpcErr)
	}
	message, _ := rpcErr["message"].(string)
	return int(code), message
}

// toolOutput 取 tools/call 的文本结果与 isError 标记。
func toolOutput(t *testing.T, msg rpcMessage) (text string, isError bool) {
	t.Helper()
	result := resultOf(t, msg)
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tools/call 结果缺少 content 数组：%v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] 不是对象：%v", content[0])
	}
	if kind, _ := first["type"].(string); kind != "text" {
		t.Fatalf("content[0].type = %q，期望 text", kind)
	}
	text, _ = first["text"].(string)
	isError, _ = result["isError"].(bool)
	return text, isError
}

// --- 夹具 -------------------------------------------------------------------

// newDocsRoot 建一个含两篇 Markdown 与一篇非 Markdown 的临时根目录，
// 并在根目录**外**放一份"越界"文件，供越界用例取证。
func newDocsRoot(t *testing.T) (root string, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "docs")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	writeFile(t, filepath.Join(root, "a.md"), "# A\n")
	writeFile(t, filepath.Join(root, "sub", "b.md"), "# B\n")
	writeFile(t, filepath.Join(root, "note.txt"), "不是 Markdown\n")
	outside = filepath.Join(base, "secret.md")
	writeFile(t, outside, "TOPSECRET\n")
	return root, outside
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %s 失败: %v", path, err)
	}
}

// --- 用例 -------------------------------------------------------------------

// TestDocsReadOnly_ToolNames 逐字对应 §4.3 的「工具名」判据：只暴露三个只读工具，
// initialize 回报协商版本与 server 身份。
func TestDocsReadOnly_ToolNames(t *testing.T) {
	root, _ := newDocsRoot(t)
	messages := runSession(t, root,
		requestLine(1, "initialize", map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		}),
		requestLine(2, "tools/list", map[string]any{}),
	)
	if len(messages) != 2 {
		t.Fatalf("期望 2 条响应，实际 %d 条: %v", len(messages), messages)
	}

	initResult := resultOf(t, messages[0])
	if got, _ := initResult["protocolVersion"].(string); got != "2025-06-18" {
		t.Errorf("protocolVersion = %q，期望回显客户端版本 2025-06-18", got)
	}
	serverInfo, ok := initResult["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("initialize 缺少 serverInfo：%v", initResult)
	}
	if got, _ := serverInfo["name"].(string); got != "docs-readonly" {
		t.Errorf("serverInfo.name = %q，期望 docs-readonly", got)
	}
	capabilities, ok := initResult["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("initialize 缺少 capabilities：%v", initResult)
	}
	if _, ok := capabilities["tools"]; !ok {
		t.Errorf("capabilities 未声明 tools：%v", capabilities)
	}

	listResult := resultOf(t, messages[1])
	tools, ok := listResult["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list 缺少 tools 数组：%v", listResult)
	}
	names := make([]string, 0, len(tools))
	writeish := []string{"write", "delete", "remove", "edit", "create", "patch", "move", "rename", "mkdir"}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("tools[] 元素不是对象：%v", item)
		}
		name, _ := tool["name"].(string)
		names = append(names, name)
		lowered := strings.ToLower(name)
		for _, banned := range writeish {
			if strings.Contains(lowered, banned) {
				t.Errorf("工具名 %q 含写操作语义 %q——只读 server 不得暴露写工具", name, banned)
			}
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("工具 %q 缺少 inputSchema", name)
		}
		if got, _ := schema["type"].(string); got != "object" {
			t.Errorf("工具 %q 的 inputSchema.type = %q，期望 object", name, got)
		}
	}
	sort.Strings(names)
	want := []string{"document_metadata", "list_documents", "read_document"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("工具名集合 = %v，期望 %v", names, want)
	}
}

// TestDocsReadOnly_ToolResults 覆盖三个工具的读写语义（正文逐字、元信息哈希、
// 只列 Markdown）——这是新增的第七个用例，不是 §4.3 六条的替代。
func TestDocsReadOnly_ToolResults(t *testing.T) {
	root, _ := newDocsRoot(t)

	listText, isError := toolOutput(t, runSession(t, root,
		toolCallLine(1, "list_documents", map[string]any{}))[0])
	if isError {
		t.Fatalf("list_documents 报错: %s", listText)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(listText), &entries); err != nil {
		t.Fatalf("list_documents 结果不是 JSON 数组: %v（%q）", err, listText)
	}
	var paths []string
	for _, entry := range entries {
		path, _ := entry["path"].(string)
		paths = append(paths, path)
		if _, ok := entry["size"]; !ok {
			t.Errorf("条目 %q 缺少 size", path)
		}
		if _, ok := entry["modified_at"]; !ok {
			t.Errorf("条目 %q 缺少 modified_at", path)
		}
	}
	if strings.Join(paths, ",") != "a.md,sub/b.md" {
		t.Errorf("文档集合 = %v，期望 [a.md sub/b.md]（只列 Markdown、路径用 / 分隔、按路径升序）", paths)
	}

	readText, isError := toolOutput(t, runSession(t, root,
		toolCallLine(1, "read_document", map[string]any{"path": "a.md"}))[0])
	if isError {
		t.Fatalf("read_document 报错: %s", readText)
	}
	if readText != "# A\n" {
		t.Errorf("read_document 正文 = %q，期望逐字返回 %q", readText, "# A\n")
	}

	metaText, isError := toolOutput(t, runSession(t, root,
		toolCallLine(1, "document_metadata", map[string]any{"path": "sub/b.md"}))[0])
	if isError {
		t.Fatalf("document_metadata 报错: %s", metaText)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(metaText), &meta); err != nil {
		t.Fatalf("document_metadata 结果不是 JSON: %v（%q）", err, metaText)
	}
	sum := sha256.Sum256([]byte("# B\n"))
	if got, _ := meta["sha256"].(string); got != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 = %q，期望 %q", got, hex.EncodeToString(sum[:]))
	}
	if got, _ := meta["path"].(string); got != "sub/b.md" {
		t.Errorf("path = %q，期望 sub/b.md", got)
	}
	if got, _ := meta["size"].(float64); got != 4 {
		t.Errorf("size = %v，期望 4", meta["size"])
	}

	missingText, isError := toolOutput(t, runSession(t, root,
		toolCallLine(1, "read_document", map[string]any{"path": "nope.md"}))[0])
	if !isError {
		t.Fatalf("读取不存在的文档应返回 isError=true，实际 %q", missingText)
	}
	if !strings.Contains(missingText, "不存在") {
		t.Errorf("缺失文档的错误文案 = %q，期望点名“不存在”", missingText)
	}
}

// TestDocsReadOnly_RejectsEscape 逐字对应 §4.3 的「越界」判据：`..` 一律拒绝，
// 且根目录外的内容不得出现在任何响应里。
func TestDocsReadOnly_RejectsEscape(t *testing.T) {
	root, _ := newDocsRoot(t)

	escapes := []string{"../secret.md", "sub/../../secret.md", "./../secret.md"}
	if runtime.GOOS == "windows" {
		escapes = append(escapes, `..\secret.md`, `sub\..\..\secret.md`)
	}
	requests := make([]string, 0, len(escapes)+1)
	for index, path := range escapes {
		requests = append(requests, toolCallLine(index+1, "read_document", map[string]any{"path": path}))
	}
	requests = append(requests, toolCallLine(len(escapes)+1, "list_documents", map[string]any{}))

	messages := runSession(t, root, requests...)
	if len(messages) != len(requests) {
		t.Fatalf("期望 %d 条响应，实际 %d 条", len(requests), len(messages))
	}
	for index, path := range escapes {
		text, isError := toolOutput(t, messages[index])
		if !isError {
			t.Errorf("path=%q 应被拒绝（isError=true），实际返回 %q", path, text)
			continue
		}
		if !strings.Contains(text, "..") {
			t.Errorf("path=%q 的拒绝文案 = %q，期望点名 `..`", path, text)
		}
		if strings.Contains(text, "TOPSECRET") {
			t.Errorf("path=%q 的响应泄露了根目录外内容", path)
		}
	}
	listText, isError := toolOutput(t, messages[len(escapes)])
	if isError {
		t.Fatalf("list_documents 报错: %s", listText)
	}
	if strings.Contains(listText, "secret") || strings.Contains(listText, "TOPSECRET") {
		t.Errorf("list_documents 列出了根目录外的条目: %s", listText)
	}
}

// TestDocsReadOnly_RejectsAbsolutePath 逐字对应 §4.3 的「绝对路径」判据。
func TestDocsReadOnly_RejectsAbsolutePath(t *testing.T) {
	root, outside := newDocsRoot(t)

	absolutes := []string{outside, filepath.ToSlash(outside)}
	if runtime.GOOS == "windows" {
		// 盘符相对形式（`C:secret.md`）不是 IsAbs，但同样不得放行。
		absolutes = append(absolutes, filepath.VolumeName(outside)+"secret.md")
	}
	requests := make([]string, 0, len(absolutes))
	for index, path := range absolutes {
		requests = append(requests, toolCallLine(index+1, "read_document", map[string]any{"path": path}))
	}
	messages := runSession(t, root, requests...)
	if len(messages) != len(requests) {
		t.Fatalf("期望 %d 条响应，实际 %d 条", len(requests), len(messages))
	}
	for index, path := range absolutes {
		text, isError := toolOutput(t, messages[index])
		if !isError {
			t.Errorf("path=%q 应被拒绝（isError=true），实际返回 %q", path, text)
			continue
		}
		if !strings.Contains(text, "绝对路径") {
			t.Errorf("path=%q 的拒绝文案 = %q，期望点名“绝对路径”", path, text)
		}
		if strings.Contains(text, "TOPSECRET") {
			t.Errorf("path=%q 的响应泄露了根目录外内容", path)
		}
	}
}

// fakeSymlinkInfo 让纯逻辑用例在任何平台都能断言"软链接一律拒绝"，
// 不依赖创建真实软链接所需的 OS 权限；真实重解析点另由 real_filesystem 用例覆盖。
type fakeSymlinkInfo struct{ name string }

func (f fakeSymlinkInfo) Name() string       { return f.name }
func (f fakeSymlinkInfo) Size() int64        { return 0 }
func (f fakeSymlinkInfo) Mode() fs.FileMode  { return fs.ModeSymlink | 0o777 }
func (f fakeSymlinkInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (f fakeSymlinkInfo) IsDir() bool        { return false }
func (f fakeSymlinkInfo) Sys() any           { return nil }

// TestDocsReadOnly_RejectsSymlink 逐字对应 §4.3 的「软链接」判据：
// ① 纯逻辑断言——任何一段被 lstat 报成软链接就拒绝（跨平台恒跑，不依赖 OS 权限）；
// ② 真实文件系统断言——在根目录内造一个指向根目录外的重解析点，读它必须被拒绝、
// 列它必须不出现，且根目录外内容不得泄露。Linux 用软链接；Windows 无 symlink 权限时
// 退到 junction（`mklink /J` 不需要特权，Go 把它报成 irregular 而不是 symlink），
// 因此"真实重解析点逃逸被挡住"在每个平台都有实测，不靠 t.Skip 换取绿态。
// The real-filesystem branch always runs: POSIX symlink, or a Windows junction when the
// token lacks SeCreateSymbolicLinkPrivilege.
func TestDocsReadOnly_RejectsSymlink(t *testing.T) {
	t.Run("component_check", func(t *testing.T) {
		fakeLstat := func(path string) (fs.FileInfo, error) {
			if strings.HasSuffix(filepath.ToSlash(path), "/docs/link.md") {
				return fakeSymlinkInfo{name: "link.md"}, nil
			}
			return os.Lstat(path)
		}
		root := filepath.Join(t.TempDir(), "docs")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		_, _, err := resolveDocument(root, "link.md", fakeLstat)
		if err == nil {
			t.Fatal("lstat 报软链接时必须拒绝，实际放行")
		}
		if !strings.Contains(err.Error(), "软链接") {
			t.Errorf("拒绝文案 = %q，期望点名“软链接”", err.Error())
		}
	})

	t.Run("real_filesystem", func(t *testing.T) {
		root, _ := newDocsRoot(t)
		outsideDir := filepath.Join(t.TempDir(), "outside")
		if err := os.MkdirAll(outsideDir, 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		writeFile(t, filepath.Join(outsideDir, "secret.md"), "TOPSECRET\n")

		linkName, mechanism := createReparsePoint(t, root, outsideDir)
		t.Logf("真实重解析点机制：%s（%s）", mechanism, linkName)

		messages := runSession(t, root,
			toolCallLine(1, "read_document", map[string]any{"path": linkName + "/secret.md"}),
			toolCallLine(2, "document_metadata", map[string]any{"path": linkName + "/secret.md"}),
			toolCallLine(3, "list_documents", map[string]any{}),
		)
		for index := 0; index < 2; index++ {
			text, isError := toolOutput(t, messages[index])
			if !isError {
				t.Errorf("经重解析点读取根目录外文档应被拒绝，实际返回 %q", text)
				continue
			}
			if strings.Contains(text, "TOPSECRET") {
				t.Errorf("拒绝文案泄露了根目录外内容: %q", text)
			}
			if mechanism == "symlink" && !strings.Contains(text, "软链接") {
				t.Errorf("软链接的拒绝文案 = %q，期望点名“软链接”", text)
			}
		}
		listText, _ := toolOutput(t, messages[2])
		if strings.Contains(listText, linkName) || strings.Contains(listText, "secret") {
			t.Errorf("list_documents 不应跟随重解析点: %s", listText)
		}
	})
}

// TestDocsReadOnly_RejectsSwappedFile 锁定「检查后换文件」的防护（US-14 复核 F6）：
// resolveDocument 的 lstat 与 readBounded 的 open 之间若被换成另一份文件，打开句柄的
// fstat 与检查结果 os.SameFile 不一致，必须拒绝，而不是把替换后的内容读出去。
// The check-then-open window must fail closed when the file identity changes.
func TestDocsReadOnly_RejectsSwappedFile(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "doc.md")
	replacement := filepath.Join(root, "other.md")
	writeFile(t, original, "ORIGINAL\n")
	writeFile(t, replacement, "TOPSECRET\n")

	path, checked, err := resolveDocument(root, "doc.md", os.Lstat)
	if err != nil {
		t.Fatalf("解析正常文档失败: %v", err)
	}
	content, info, err := readBounded(path, "doc.md", checked)
	if err != nil {
		t.Fatalf("文件未被替换时必须能读: %v", err)
	}
	if string(content) != "ORIGINAL\n" {
		t.Fatalf("读取内容 = %q，期望 ORIGINAL", content)
	}
	if !os.SameFile(checked, info) {
		t.Fatal("未替换时打开句柄与检查结果必须是同一文件")
	}

	if err := os.Rename(replacement, original); err != nil {
		t.Fatalf("替换文件失败: %v", err)
	}
	if _, _, err := readBounded(path, "doc.md", checked); err == nil {
		t.Fatal("检查后文件被替换时必须拒绝，实际读取成功")
	} else if !strings.Contains(err.Error(), "替换") {
		t.Errorf("拒绝文案 = %q，期望点名“替换”", err.Error())
	}
}

// createReparsePoint 在根目录内造一个指向根目录外的重解析点，返回链接名与机制。
// 优先真软链接；Windows 上符号链接要求 SeCreateSymbolicLinkPrivilege，普通令牌没有，
// 于是退到 junction（`mklink /J` 不需要特权）。两者都失败说明环境缺前置——按失败处理，
// 不跳过：判据面不允许用跳过换绿态。
// A missing prerequisite is a failure here, never a skip.
func createReparsePoint(t *testing.T, root, target string) (linkName, mechanism string) {
	t.Helper()
	const name = "escape"
	linkPath := filepath.Join(root, name)
	if err := os.Symlink(target, linkPath); err == nil {
		return name, "symlink"
	} else if runtime.GOOS != "windows" {
		t.Fatalf("创建软链接失败: %v", err)
	}

	// 参数分开传：Go 给含空格的参数加引号时用反斜杠转义内部引号，而 cmd.exe
	// 不认这套转义——拼成一整串再交给 /c 会得到"语法不正确"。
	// Pass the arguments separately: cmd.exe does not understand backslash escaping.
	output, err := exec.Command("cmd", "/c", "mklink", "/J", linkPath, target).CombinedOutput()
	if err != nil {
		t.Fatalf("软链接与 junction 都无法创建（%v；%s）——真实重解析点用例缺前置，按失败处理",
			err, strings.TrimSpace(string(output)))
	}
	return name, "junction"
}

// TestDocsReadOnly_ReadOnly 逐字对应 §4.3 的「只读」判据：跑完整会话后根目录
// 逐文件（内容 + 修改时间）不变，且写类工具调用只会得到协议错误。
func TestDocsReadOnly_ReadOnly(t *testing.T) {
	root, _ := newDocsRoot(t)
	before := snapshotTree(t, root)

	messages := runSession(t, root,
		requestLine(1, "tools/list", map[string]any{}),
		toolCallLine(2, "list_documents", map[string]any{}),
		toolCallLine(3, "read_document", map[string]any{"path": "a.md"}),
		toolCallLine(4, "document_metadata", map[string]any{"path": "sub/b.md"}),
		toolCallLine(5, "write_document", map[string]any{"path": "new.md", "content": "x"}),
		toolCallLine(6, "delete_document", map[string]any{"path": "a.md"}),
	)
	if len(messages) != 6 {
		t.Fatalf("期望 6 条响应，实际 %d 条", len(messages))
	}
	for _, index := range []int{4, 5} {
		code, message := errorOf(t, messages[index])
		if code != codeInvalidParams {
			t.Errorf("写类工具调用应返回 -32602，实际 %d（%s）", code, message)
		}
		if !strings.Contains(message, "未知工具") {
			t.Errorf("写类工具的错误文案 = %q，期望点名“未知工具”", message)
		}
	}
	after := snapshotTree(t, root)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("会话期间根目录发生变化：\n前: %v\n后: %v", before, after)
	}
	if _, err := os.Stat(filepath.Join(root, "new.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("写类工具调用创建了文件 new.md（err=%v）", err)
	}
}

// snapshotTree 记录根目录下每个文件的路径、大小、修改时间与内容哈希。
func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		lines = append(lines, fmt.Sprintf("%s|%d|%s|%s",
			filepath.ToSlash(rel), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano),
			hex.EncodeToString(sum[:])))
		return nil
	})
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	sort.Strings(lines)
	return lines
}

// TestDocsReadOnly_ProtocolErrors 逐字对应 §4.3 的「协议」判据：
// 解析错误 / 未知方法 / 未知工具各自的错误码，通知不应答，坏行之后仍能继续服务。
func TestDocsReadOnly_ProtocolErrors(t *testing.T) {
	root, _ := newDocsRoot(t)
	messages := runSession(t, root,
		`{"jsonrpc":"2.0","id":1,"method":`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		requestLine(2, "resources/list", map[string]any{}),
		toolCallLine(3, "shell_exec", map[string]any{"command": "rm -rf /"}),
		`{"jsonrpc":"2.0","id":4}`,
		requestLine(5, "tools/list", map[string]any{}),
	)
	if len(messages) != 5 {
		t.Fatalf("期望 5 条响应（通知不应答），实际 %d 条: %v", len(messages), messages)
	}

	if code, _ := errorOf(t, messages[0]); code != codeParseError {
		t.Errorf("坏 JSON 的错误码 = %d，期望 %d", code, codeParseError)
	}
	if id := messages[0]["id"]; id != nil {
		t.Errorf("解析错误响应的 id 应为 null，实际 %v", id)
	}
	if code, _ := errorOf(t, messages[1]); code != codeMethodNotFound {
		t.Errorf("未知方法（resources/list）的错误码 = %d，期望 %d", code, codeMethodNotFound)
	}
	if code, _ := errorOf(t, messages[2]); code != codeInvalidParams {
		t.Errorf("未知工具（shell_exec）的错误码 = %d，期望 %d", code, codeInvalidParams)
	}
	if code, _ := errorOf(t, messages[3]); code != codeInvalidRequest {
		t.Errorf("缺少 method 的请求错误码 = %d，期望 %d", code, codeInvalidRequest)
	}
	if _, ok := messages[4]["result"]; !ok {
		t.Errorf("坏行之后 tools/list 仍应成功，实际响应 %v", messages[4])
	}
}
