package app

// 工作区只读浏览服务（US-12 约束 40 / 注 19）：根固定 `.basalt/agents/`，用 `os.Root`
// 做真实路径投影；路径逐段校验，软链接只报类型、不跟随，`.staging` / `.backup` 残骸
// 不可见，单文件超过 256 KiB 直接拒绝（不截断），非 UTF-8 / 含 NUL 的文件按 400 拒绝。
// 本文件没有写路径：浏览端点是纯读，不写管理操作行、不写审计。
// Read-only workspace browsing (US-12 constraint 40 / note 19): the root is fixed to
// `.basalt/agents/` and projected through `os.Root`. Symlinks are listed by type and
// never followed, staging/backup residue is invisible, files over 256 KiB are rejected
// rather than truncated, and non-UTF-8 or NUL-containing files are refused. Nothing
// here writes: browsing never produces management-operation rows.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// maxWorkspaceFileBytes 是单文件预览上限（契约冻结 256 KiB；超限 400，不截断）。
	maxWorkspaceFileBytes = 256 << 10

	// workspaceBrowseTool 是入参拒绝时点名的工作区工具（错误信封的 tool 字段）。
	workspaceBrowseTool = "workspace.browse"

	// 目录条目类型字面量（契约 WorkspaceEntry.type 的枚举）。
	workspaceEntryFile    = "file"
	workspaceEntryDir     = "dir"
	workspaceEntrySymlink = "symlink"
)

// WorkspaceEntry 是一条浏览结果：相对浏览根的路径（`/` 分隔）、类型与字节数。
// 目录与软链接的 Size 恒为 0。
// WorkspaceEntry is one browsed entry: a root-relative `/` path, its type, and its
// byte size (directories and symlinks report 0).
type WorkspaceEntry struct {
	Path string
	Type string
	Size int64
}

// WorkspaceFile 是单文件只读预览。没有截断形态：超过上限的请求直接失败。
// WorkspaceFile is a read-only preview; there is no truncated shape because oversized
// requests fail outright.
type WorkspaceFile struct {
	Path    string
	Content string
	Size    int64
}

// WorkspaceBrowseDeps 是工作区浏览的装配输入。
// WorkspaceBrowseDeps wires the browse service.
type WorkspaceBrowseDeps struct {
	// Root 是工作区根（`.basalt`）：浏览根固定为其下的 `agents/`。空串按装配错误
	// 拒绝——不能回落到进程工作目录下同名的目录。
	Root string
	// Logger 记录被拒绝的请求（路径越界 / 残骸访问）；文件内容一律不进日志。
	Logger *slog.Logger
}

// WorkspaceBrowse 是工作区浏览的唯一实现；字段构造后不再变化。
// WorkspaceBrowse is the sole browse implementation; its fields are fixed at build time.
type WorkspaceBrowse struct {
	root   string
	logger *slog.Logger
}

// NewWorkspaceBrowse 组装浏览服务；Logger 为 nil 时回落 slog 缺省值。
// NewWorkspaceBrowse assembles the service; a nil Logger falls back to slog's default.
func NewWorkspaceBrowse(deps WorkspaceBrowseDeps) *WorkspaceBrowse {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &WorkspaceBrowse{root: deps.Root, logger: logger}
}

// Tree 列出 path 的直接条目。path 为空或 `.` 表示浏览根；路径越界按入参错误拒绝
// （400），不存在、软链接与残骸按不存在处理（404）。条目按 path 升序返回。
// Tree lists the immediate entries under path; an empty path or `.` is the browse root.
// Escapes are invalid input (400); missing paths, symlinks, and residue are absent (404).
func (s *WorkspaceBrowse) Tree(ctx context.Context, rawPath string) ([]WorkspaceEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	segments, err := normalizeWorkspacePath(rawPath, true)
	if err != nil {
		s.logger.Warn("工作区浏览拒绝目录请求", "path", rawPath, "error", err)
		return nil, err
	}
	base, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = base.Close()
	}()

	info, err := resolveWorkspacePath(base, segments)
	if err != nil {
		return nil, err
	}
	if info != nil && !info.IsDir() {
		return nil, invalidWorkspacePath("路径不是目录，不能列出条目")
	}
	dirPath := "."
	if len(segments) > 0 {
		dirPath = strings.Join(segments, "/")
	}
	dir, err := base.Open(dirPath)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", dirPath, err)
	}
	defer func() {
		_ = dir.Close()
	}()
	rawEntries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("读取工作区目录 %s: %w", dirPath, err)
	}

	entries := make([]WorkspaceEntry, 0, len(rawEntries))
	for _, entry := range rawEntries {
		name := entry.Name()
		if isWorkspaceResidueName(name) {
			continue
		}
		entryType := workspaceEntryTypeOf(entry)
		if entryType == "" {
			// 契约类型枚举只有 file / dir / symlink：设备 / 管道等条目不在浏览语义内，
			// 既不列出也不读取（列出来只能给一个契约里不存在的类型）。
			continue
		}
		entryPath := joinWorkspacePath(segments, name)
		var size int64
		if entryType == workspaceEntryFile {
			entryInfo, infoErr := entry.Info()
			if infoErr != nil {
				// 条目在 ReadDir 与 Info 之间消失：目录树跳过它，不让整页失败。
				s.logger.Warn("读取工作区条目信息失败，跳过", "path", entryPath, "error", infoErr)
				continue
			}
			size = entryInfo.Size()
		}
		entries = append(entries, WorkspaceEntry{Path: entryPath, Type: entryType, Size: size})
	}
	slices.SortFunc(entries, func(a, b WorkspaceEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	return entries, nil
}

// File 读取单个文件预览。读取前先按 `Lstat` 判大小，超过 256 KiB 直接 400 且不读正文；
// 打开后再用 `Stat` 复核一次（判卷与读取之间文件可能被替换，`os.Root` 只保证不越界）。
// 目录、软链接、非普通文件与二进制内容都不预览。
// File previews one file. The size is checked with Lstat before reading (oversized files
// are refused without touching their contents) and re-checked after Open, because the
// file may be replaced between the two calls; os.Root only guarantees confinement.
func (s *WorkspaceBrowse) File(ctx context.Context, rawPath string) (WorkspaceFile, error) {
	if err := ctx.Err(); err != nil {
		return WorkspaceFile{}, err
	}
	segments, err := normalizeWorkspacePath(rawPath, false)
	if err != nil {
		s.logger.Warn("工作区浏览拒绝文件请求", "path", rawPath, "error", err)
		return WorkspaceFile{}, err
	}
	if len(segments) == 0 {
		return WorkspaceFile{}, invalidWorkspacePath("path 必须指向文件，不能是浏览根")
	}
	base, err := s.openRoot()
	if err != nil {
		return WorkspaceFile{}, err
	}
	defer func() {
		_ = base.Close()
	}()

	info, err := resolveWorkspacePath(base, segments)
	if err != nil {
		return WorkspaceFile{}, err
	}
	if info.IsDir() {
		return WorkspaceFile{}, invalidWorkspacePath("路径是目录，不能作为文件读取")
	}
	if !info.Mode().IsRegular() {
		return WorkspaceFile{}, invalidWorkspacePath("路径不是普通文件")
	}
	if info.Size() > maxWorkspaceFileBytes {
		return WorkspaceFile{}, oversizeWorkspaceFile()
	}

	rel := strings.Join(segments, "/")
	file, err := base.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return WorkspaceFile{}, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
		}
		return WorkspaceFile{}, fmt.Errorf("打开工作区文件 %s: %w", rel, err)
	}
	defer func() {
		_ = file.Close()
	}()
	opened, err := file.Stat()
	if err != nil {
		return WorkspaceFile{}, fmt.Errorf("查看工作区文件 %s: %w", rel, err)
	}
	if !opened.Mode().IsRegular() {
		return WorkspaceFile{}, invalidWorkspacePath("路径不是普通文件")
	}
	if opened.Size() > maxWorkspaceFileBytes {
		return WorkspaceFile{}, oversizeWorkspaceFile()
	}
	// 多读 1 字节用来识别"判卷之后文件变大"：绝不把整份超大文件读进内存。
	raw, err := io.ReadAll(io.LimitReader(file, maxWorkspaceFileBytes+1))
	if err != nil {
		return WorkspaceFile{}, fmt.Errorf("读取工作区文件 %s: %w", rel, err)
	}
	if len(raw) > maxWorkspaceFileBytes {
		return WorkspaceFile{}, oversizeWorkspaceFile()
	}
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		// 二进制不做 base64：预览只承诺文本，返回半解码内容会误导下游。
		return WorkspaceFile{}, invalidWorkspacePath("文件不是 UTF-8 文本或含 NUL，不能预览")
	}
	return WorkspaceFile{Path: rel, Content: string(raw), Size: int64(len(raw))}, nil
}

// openRoot 打开浏览根（`.basalt/agents`）。工作区根为空按装配错误拒绝。
// openRoot opens the browse root; an empty workspace root is a wiring error.
func (s *WorkspaceBrowse) openRoot() (*os.Root, error) {
	if s.root == "" {
		return nil, fmt.Errorf("工作区浏览：工作区根未配置")
	}
	base, err := os.OpenRoot(filepath.Join(s.root, agentsDirName))
	if err != nil {
		return nil, fmt.Errorf("打开工作区浏览根: %w", err)
	}
	return base, nil
}

// normalizeWorkspacePath 把请求路径拆成规范段：拒绝绝对路径、盘符 / UNC、反斜杠、
// NUL / 控制字符、空段与 `..`；`.` 段按规范省略。`.staging` / `.backup` 残骸不是
// 入参错误——它们在浏览语义里不存在，按 404 拒绝。空路径仅在 allowEmpty 时接受
// （目录树缺省为浏览根）。
// normalizeWorkspacePath splits the request into canonical segments. Residue is not an
// input error: it does not exist in the browse surface and is refused as a 404.
func normalizeWorkspacePath(raw string, allowEmpty bool) ([]string, error) {
	if raw == "" {
		if allowEmpty {
			return nil, nil
		}
		return nil, invalidWorkspacePath("path 不得为空")
	}
	for _, r := range raw {
		if r == 0 || r < 0x20 || r == 0x7f {
			return nil, invalidWorkspacePath("路径不得含 NUL 或控制字符")
		}
	}
	// 只认 `/` 分隔：Windows 上反斜杠是等价分隔符，放行会让同一路径有两种写法，
	// 也会让 `..\` 绕过逐段判定。
	if strings.ContainsRune(raw, '\\') {
		return nil, invalidWorkspacePath("路径分隔符只接受 /")
	}
	if strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) || filepath.VolumeName(raw) != "" {
		return nil, invalidWorkspacePath("路径必须是相对浏览根的相对路径")
	}
	// 盘符判定不依赖平台：在非 Windows 上 `C:` 也是非法形状。
	if len(raw) >= 2 && raw[1] == ':' && isASCIILetter(raw[0]) {
		return nil, invalidWorkspacePath("路径不得含盘符")
	}
	parts := strings.Split(raw, "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "":
			return nil, invalidWorkspacePath("路径不得含空段（含尾随 /）")
		case ".":
			continue
		case "..":
			return nil, invalidWorkspacePath("路径不得含 ..")
		}
		if isWorkspaceResidueName(part) {
			return nil, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
		}
		segments = append(segments, part)
	}
	return segments, nil
}

// resolveWorkspacePath 逐段投影路径：每段都用 `Lstat` 判卷，任一段是软链接即按不存在
// 处理（不跟随、不进入目标）；中间段必须是目录。空路径（浏览根本身）返回 nil。
// resolveWorkspacePath projects the path one segment at a time. Any symlink component
// is treated as absent and never followed; intermediate components must be directories.
func resolveWorkspacePath(base *os.Root, segments []string) (fs.FileInfo, error) {
	var (
		info    fs.FileInfo
		current strings.Builder
	)
	for index, segment := range segments {
		if index > 0 {
			current.WriteString("/")
		}
		current.WriteString(segment)
		got, err := base.Lstat(current.String())
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
			}
			return nil, fmt.Errorf("查看工作区路径 %s: %w", current.String(), err)
		}
		if got.Mode()&fs.ModeSymlink != 0 {
			// 软链接只出现在目录列表里（type=symlink）；读取与下行都按不存在处理。
			return nil, fmt.Errorf("%w: 工作区没有该路径", core.ErrResourceNotFound)
		}
		if index < len(segments)-1 && !got.IsDir() {
			return nil, invalidWorkspacePath(fmt.Sprintf("路径 %s 不是目录", current.String()))
		}
		info = got
	}
	return info, nil
}

// workspaceEntryTypeOf 把目录条目映射为契约类型；枚举外的条目返回空串（不列出）。
// workspaceEntryTypeOf maps a directory entry onto the contract enum; other kinds
// return the empty string and are never listed.
func workspaceEntryTypeOf(entry fs.DirEntry) string {
	switch {
	case entry.IsDir():
		return workspaceEntryDir
	case entry.Type()&fs.ModeSymlink != 0:
		return workspaceEntrySymlink
	case entry.Type().IsRegular():
		return workspaceEntryFile
	default:
		return ""
	}
}

// isWorkspaceResidueName 判定 staging / backup 残骸。大小写不敏感：Windows 上
// `a.STAGING` 与 `a.staging` 是同一目录，两种写法都必须隐藏。
// isWorkspaceResidueName detects staging/backup residue case-insensitively, because
// Windows treats `a.STAGING` and `a.staging` as the same directory.
func isWorkspaceResidueName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".staging") || strings.HasSuffix(lower, ".backup")
}

// joinWorkspacePath 拼出面向契约的相对路径（恒为 `/` 分隔）。
// joinWorkspacePath builds the contract-facing `/`-separated relative path.
func joinWorkspacePath(segments []string, name string) string {
	if len(segments) == 0 {
		return name
	}
	return strings.Join(segments, "/") + "/" + name
}

// invalidWorkspacePath 构造 400 入参错误（契约 §7.3 invalid_input）。
// invalidWorkspacePath builds the 400 invalid-input carrier.
func invalidWorkspacePath(reason string) error {
	return &core.InvalidInputError{Tool: workspaceBrowseTool, Reason: reason}
}

// oversizeWorkspaceFile 是超过 256 KiB 的固定拒绝文案（点名上限，不截断）。
// oversizeWorkspaceFile is the fixed refusal for files over the 256 KiB cap.
func oversizeWorkspaceFile() error {
	return invalidWorkspacePath(fmt.Sprintf(
		"文件超过 %d KiB 上限，拒绝读取（不截断）", maxWorkspaceFileBytes>>10))
}

// isASCIILetter 判 ASCII 字母（盘符形状判定用，不依赖平台语义）。
// isASCIILetter reports an ASCII letter for the drive-letter shape check.
func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
