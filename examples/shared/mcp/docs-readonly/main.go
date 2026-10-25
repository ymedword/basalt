// Package main 是 examples 用的最小只读 MCP server（颗粒度文档 §3.9 / J-03 / Q-03）：
// 纯标准库、零依赖；2026-10-26 起并回主 module（go:embed 跨不过嵌套 module，
// 而发行包必须能把源码交给用户），因此随根 `go test ./...` 一起测试，但它不被
// 产品二进制导入，运行时仍是独立 MCP 进程。它只做三件事——
// 列目录、读正文、给元信息；所有路径都在根目录内按段 lstat 解析，拒绝 `..` /
// 绝对路径 / 软链接，本进程不提供也不执行任何写操作。
// Package main is the minimal read-only MCP server used by the examples: stdlib only,
// zero dependencies, and no write surface at all. Every path is resolved component by
// component under a fixed root, rejecting parent references, absolute paths and symlinks.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	serverName    = "docs-readonly"
	serverVersion = "0.1.0"

	// maxDocumentBytes / maxDocuments 是有界读取的上限：示例 server 不追求通用性，
	// 但必须有界——否则一次 tools/call 就能把整个磁盘读进内存。
	// Bounded reads: one call must never swallow an unbounded amount of data.
	maxDocumentBytes = 1 << 20
	maxDocuments     = 1000

	// rootEnvVar 是根目录的第二来源（`-root` 优先）；两者都没有时用当前目录。
	rootEnvVar = "DOCS_READONLY_ROOT"
)

// JSON-RPC 2.0 错误码（协议层）。工具自身的失败走 result.isError，不是这些码。
// Protocol-level error codes; tool failures use result.isError instead.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// supportedProtocolVersions 是 initialize 可回显的版本集合。客户端请求的版本在集合内
// 就原样回显（MCP 版本协商要求），否则回 latestLegacyProtocolVersion。
// Echo the client version when known, otherwise answer with a version we do support.
var supportedProtocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
	"2025-11-25": true,
}

const latestLegacyProtocolVersion = "2025-06-18"

// lstatFunc 让"逐段检查软链接"可以在没有创建软链接权限的环境里照样被断言。
// lstatFunc is the injection seam for the symlink rule (see main_test.go).
type lstatFunc func(string) (fs.FileInfo, error)

// rpcRequest / rpcResponse 是 JSON-RPC 2.0 的最小信封；ID 用 RawMessage 以原样回声。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// documentEntry 是 list_documents 的条目形态：路径统一用 `/` 分隔，时间用 UTC RFC3339。
type documentEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
}

type documentMetadata struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	ModifiedAt string `json:"modified_at"`
	SHA256     string `json:"sha256"`
}

type server struct {
	root string
	// dir 是根目录的 os.Root 句柄：文档一律经它打开，越界（含软链接跳出去）在
	// 文件系统层就被挡住，不依赖字符串前缀比较。resolveDocument 的手工逐段检查
	// 仍在前面跑——它负责给出可读的拒绝文案，两层各自独立。
	// dir is the os.Root handle every document open goes through, so containment
	// is enforced by the filesystem rather than by comparing path prefixes.
	dir *os.Root
}

func main() {
	rootFlag := flag.String("root", "", "文档根目录（默认取 $"+rootEnvVar+"，再缺省为当前目录）")
	flag.Parse()

	root, err := resolveRoot(*rootFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serverName, err)
		os.Exit(2)
	}
	if err := serve(os.Stdin, os.Stdout, root); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", serverName, err)
		os.Exit(1)
	}
}

// resolveRoot 固定根目录：`-root` > 环境变量 > 当前目录；解析一次软链接后不再变。
// 根目录本身允许是软链接（部署位置由使用者决定），进入根目录之后的一切都不跟随。
// The root itself may be a symlink; nothing below it ever is.
func resolveRoot(flagValue string) (string, error) {
	root := strings.TrimSpace(flagValue)
	if root == "" {
		root = strings.TrimSpace(os.Getenv(rootEnvVar))
	}
	if root == "" {
		workingDir, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("读取当前目录失败: %v", err)
		}
		root = workingDir
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("解析根目录失败: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("根目录不可用 %s: %v", root, err)
	}
	// os.OpenRoot 只接受目录（文件会报 not a directory），因此它同时完成了
	// "存在 + 是目录 + 可打开"三件事；探测句柄用完即关，serve 会另开一个持有。
	// OpenRoot doubles as the directory check; the probe handle is closed right away.
	probe, err := os.OpenRoot(resolved)
	if err != nil {
		return "", fmt.Errorf("根目录不是可打开的目录 %s: %v", root, err)
	}
	if err := probe.Close(); err != nil {
		return "", fmt.Errorf("关闭根目录探测句柄 %s: %v", root, err)
	}
	return resolved, nil
}

// serve 是 stdin/stdout 上的 JSON-RPC 2.0 行循环：一行一个消息，通知不应答，
// 坏行报 -32700 后继续服务。错误只写 stderr，stdout 只放协议响应。
// One JSON message per line; notifications get no response and a bad line never
// stops the loop. stdout carries protocol responses only.
func serve(in io.Reader, out io.Writer, root string) (err error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("打开根目录 %s: %v", root, err)
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("关闭根目录 %s: %v", root, closeErr)
		}
	}()
	sv := &server{root: root, dir: dir}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	encoder := json.NewEncoder(out)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var request rpcRequest
		if err := json.Unmarshal(line, &request); err != nil {
			// 解析失败时 id 未知，按 JSON-RPC 规定回 null。
			if err := encoder.Encode(rpcResponse{
				JSONRPC: "2.0",
				Error:   &rpcError{Code: codeParseError, Message: "请求不是合法 JSON: " + err.Error()},
			}); err != nil {
				return fmt.Errorf("写响应失败: %w", err)
			}
			continue
		}
		if len(request.ID) == 0 {
			// 通知（含 notifications/initialized）一律不应答。
			continue
		}

		response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
		if request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" {
			response.Error = &rpcError{Code: codeInvalidRequest, Message: "请求缺少 jsonrpc=2.0 或 method"}
		} else {
			result, rpcErr := sv.handle(request)
			response.Result, response.Error = result, rpcErr
		}
		if err := encoder.Encode(response); err != nil {
			return fmt.Errorf("写响应失败: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("读取请求失败: %w", err)
	}
	return nil
}

func (s *server) handle(request rpcRequest) (any, *rpcError) {
	switch request.Method {
	case "initialize":
		return s.initialize(request.Params)
	case "tools/list":
		return map[string]any{"tools": toolDefinitions()}, nil
	case "tools/call":
		return s.callTool(request.Params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "方法不受支持: " + request.Method}
	}
}

func (s *server) initialize(params json.RawMessage) (any, *rpcError) {
	var request struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "initialize 参数不是合法 JSON: " + err.Error()}
		}
	}
	version := strings.TrimSpace(request.ProtocolVersion)
	if !supportedProtocolVersions[version] {
		version = latestLegacyProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		"instructions":    "只读文档服务：只能用 list_documents / read_document / document_metadata，路径必须是根目录内的相对路径。",
	}, nil
}

func toolDefinitions() []any {
	return []any{
		map[string]any{
			"name":        "list_documents",
			"description": fmt.Sprintf("递归列出根目录下的 Markdown 文档（只读；最多 %d 条）。", maxDocuments),
			"inputSchema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
		},
		map[string]any{
			"name":        "read_document",
			"description": "读取一篇 Markdown 文档的正文（只读；单文件上限 1 MiB，必须是 UTF-8）。",
			"inputSchema": pathInputSchema(),
		},
		map[string]any{
			"name":        "document_metadata",
			"description": "返回一篇文档的大小、修改时间与 SHA-256（只读，不返回正文）。",
			"inputSchema": pathInputSchema(),
		},
	}
}

func pathInputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "相对根目录的文档路径，例如 daily/report.md",
			},
		},
		"required":             []string{"path"},
		"additionalProperties": false,
	}
}

func (s *server) callTool(params json.RawMessage) (any, *rpcError) {
	var request struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call 参数不是合法 JSON: " + err.Error()}
	}

	var text string
	var err error
	switch request.Name {
	case "list_documents":
		text, err = s.listDocuments()
	case "read_document":
		text, err = s.readDocument(request.Arguments)
	case "document_metadata":
		text, err = s.documentMetadata(request.Arguments)
	default:
		return nil, &rpcError{Code: codeInvalidParams, Message: "未知工具: " + request.Name}
	}
	if err != nil {
		// 工具自身的失败（越界 / 软链接 / 缺失 / 超限）回给模型看，不是协议错误。
		return toolResult(err.Error(), true), nil
	}
	return toolResult(text, false), nil
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

func (s *server) listDocuments() (string, error) {
	entries := make([]documentEntry, 0, 16)
	walkErr := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == s.root {
			return nil
		}
		// 软链接不进入、不展示：列出它等于替调用方跟随。
		if entry.Type()&fs.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		entries = append(entries, documentEntry{
			Path:       filepath.ToSlash(relative),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
		})
		if len(entries) > maxDocuments {
			return fmt.Errorf("文档数量超过上限 %d", maxDocuments)
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("遍历文档目录失败: %v", walkErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	payload, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("序列化文档清单失败: %v", err)
	}
	return string(payload), nil
}

func (s *server) readDocument(arguments json.RawMessage) (string, error) {
	path, checked, err := s.resolveArgument(arguments)
	if err != nil {
		return "", err
	}
	display := s.displayPath(path)
	content, _, err := readBounded(s.dir, display, checked)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(content) {
		return "", fmt.Errorf("不是 UTF-8 文本: %s", display)
	}
	return string(content), nil
}

func (s *server) documentMetadata(arguments json.RawMessage) (string, error) {
	path, checked, err := s.resolveArgument(arguments)
	if err != nil {
		return "", err
	}
	display := s.displayPath(path)
	content, info, err := readBounded(s.dir, display, checked)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	payload, err := json.Marshal(documentMetadata{
		Path:       display,
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
		SHA256:     hex.EncodeToString(digest[:]),
	})
	if err != nil {
		return "", fmt.Errorf("序列化文档元信息失败: %v", err)
	}
	return string(payload), nil
}

// displayPath 只回相对根目录的路径：错误与元信息会发给 MCP 客户端，不能暴露
// 服务器上的绝对路径。
// displayPath returns a root-relative path so responses never leak absolute paths.
func (s *server) displayPath(path string) string {
	relative, err := filepath.Rel(s.root, path)
	if err != nil {
		return filepath.Base(path)
	}
	return filepath.ToSlash(relative)
}

// ioFailureDetail 去掉 OS 错误携带的绝对路径，只保留原因。
// ioFailureDetail strips the absolute path an OS error carries, keeping the reason.
func ioFailureDetail(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// resolveArgument 解析 `{"path": "..."}` 并落到根目录内的绝对路径，同时交出最终
// 一段的 lstat 结果供读取阶段比对。
func (s *server) resolveArgument(arguments json.RawMessage) (string, fs.FileInfo, error) {
	var payload struct {
		Path string `json:"path"`
	}
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &payload); err != nil {
			return "", nil, fmt.Errorf("参数不是合法 JSON: %v", err)
		}
	}
	return resolveDocument(s.root, payload.Path, os.Lstat)
}

// resolveDocument 是全部越界防护的唯一入口（模块卡 C-05）：拒绝空路径 / NUL / 绝对路径 /
// 盘符相对路径 / 任何 `..` 段 / 软链接，逐段 lstat 后才允许打开，且只允许普通文件。
// 同时按 `/` 与 `\` 两种分隔符切段——在另一平台上写出的 `..\` 不能借平台差异绕过。
// The single confinement gate: reject empty/NUL/absolute/volume-relative paths, any
// `..` component and any symlink, then accept regular files only. It also returns the
// final component's lstat result so the reader can prove the opened file is the one
// that was checked.
func resolveDocument(root, requested string, lstat lstatFunc) (string, fs.FileInfo, error) {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" {
		return "", nil, errors.New("path 不能为空")
	}
	if strings.ContainsRune(trimmed, 0) {
		return "", nil, errors.New("path 不得包含 NUL 字节")
	}
	if filepath.IsAbs(trimmed) || filepath.VolumeName(trimmed) != "" {
		return "", nil, fmt.Errorf("path 必须是相对根目录的路径，不得是绝对路径: %s", requested)
	}
	for _, part := range splitPath(trimmed) {
		if part == ".." {
			return "", nil, fmt.Errorf("path 不得包含 ..（越界拒绝）: %s", requested)
		}
		if runtime.GOOS == "windows" && strings.Contains(part, ":") {
			// Windows 的备用数据流（`a.md:stream`）会让"普通文件"检查失真，直接拒绝。
			return "", nil, fmt.Errorf("path 不得包含冒号（Windows 备用数据流）: %s", requested)
		}
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == string(filepath.Separator) {
		return "", nil, fmt.Errorf("path 必须指向一篇文档，而不是根目录: %s", requested)
	}

	target := filepath.Join(root, clean)
	parts := splitPath(clean)
	current := root
	var checked fs.FileInfo
	for index, part := range parts {
		current = filepath.Join(current, part)
		last := index == len(parts)-1
		info, err := lstat(current)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", nil, fmt.Errorf("文档不存在: %s", filepath.ToSlash(clean))
			}
			return "", nil, fmt.Errorf("读取文档元信息失败: %v", err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", nil, fmt.Errorf("path 不得指向软链接: %s", filepath.ToSlash(clean))
		}
		if !last && !info.IsDir() {
			return "", nil, fmt.Errorf("path 的某一层不是目录: %s", filepath.ToSlash(clean))
		}
		if last && !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("只能读取普通文件: %s", filepath.ToSlash(clean))
		}
		if last {
			checked = info
		}
	}
	return target, checked, nil
}

// splitPath 按两种分隔符切段并丢弃空段与 `.`，保证 `a//b` / `a\b` 得到同一组段。
func splitPath(path string) []string {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' })
	kept := parts[:0]
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		kept = append(kept, part)
	}
	return kept
}

// readBounded 读取上限硬编码为 maxDocumentBytes：多读一个字节就能判断"超限"。
// 打开后用句柄的 fstat 与解析阶段最后一次 lstat 的 FileInfo 做 os.SameFile 比对：
// 检查与打开之间被换成另一份文件（TOCTOU）时拒绝，而不是把替换后的内容读出去；
// 元信息也取打开句柄的读数，不再对路径二次 os.Stat。错误只回相对路径（display），
// 不把服务器绝对路径发给 MCP 客户端。
// readBounded re-checks the opened handle against the lstat'd FileInfo with
// os.SameFile, closing the check-then-open window, and reports the opened file's info.
// Errors carry the root-relative display path only.
func readBounded(dir *os.Root, display string, checked fs.FileInfo) ([]byte, fs.FileInfo, error) {
	file, err := dir.Open(filepath.FromSlash(display))
	if err != nil {
		return nil, nil, fmt.Errorf("打开文档失败: %s（%s）", display, ioFailureDetail(err))
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("读取文档元信息失败: %s（%s）", display, ioFailureDetail(err))
	}
	if checked != nil && !os.SameFile(checked, info) {
		return nil, nil, fmt.Errorf("文档在检查后被替换，拒绝读取: %s", display)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("只能读取普通文件: %s", display)
	}

	content, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("读取文档失败: %s（%s）", display, ioFailureDetail(err))
	}
	if len(content) > maxDocumentBytes {
		return nil, nil, fmt.Errorf("文档超过 %d 字节上限", maxDocumentBytes)
	}
	return content, info, nil
}
