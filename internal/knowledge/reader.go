package knowledge

// 本文件实现 `DocumentReader` 的工作区版：把**允许根**作为唯一文件系统入口，
// 完成图 3「前置」段的四项判定——路径在允许根内、真实路径投影、大小上限、
// UTF-8 解码（颗粒度文档 §2.3 图 3 / 契约 `POST /collections/{id}/documents`
// 的 400 语义）。
// This file implements the workspace-backed DocumentReader: the allowed root is the
// only filesystem entry, and the four preconditions of figure 3 — root confinement,
// real-path projection, size cap and UTF-8 decoding — all fail closed here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

// RootReader 读取工作区允许根内的相对路径。
// RootReader loads paths relative to the workspace's allowed root.
//
// 一切拒绝都归到 `ErrPathRejected`：契约把"越界 / 扩展名 / 超限 / 编码失败"统一
// 映射为 400 `BAD_REQUEST`，HTTP 面的固定文案（"大小与编码同样受限"）也已按这一
// 归类写死；此处**不得**再引入第二个可被判定的错误类别去改判据面。
// Every rejection folds into ErrPathRejected because the frozen contract maps root
// escapes, the size cap and decode failures onto the same 400 response.
type RootReader struct {
	// root 是**真实路径投影之后**的允许根；比较前缀的两端都必须是已投影的真实路径。
	// root is the projected real path; both sides of the prefix check are real.
	root string
	// maxBytes 是单文档字节上限（配置键 `knowledge.max_document_bytes`）。
	maxBytes int64
}

// 编译期断言：读取器满足摄取侧声明的文档读取端口。
// Compile-time assertion that the reader satisfies the DocumentReader port.
var _ DocumentReader = (*RootReader)(nil)

// NewRootReader 绑定允许根与大小上限。根必须已经存在且能被投影成真实路径——
// 投影不出来说明工作区本身有问题，装配期就该失败，而不是等第一次摄取才报错。
// NewRootReader binds the allowed root and the size cap; an unresolvable root fails
// at assembly time rather than on the first ingest.
func NewRootReader(root string, maxBytes int64) (*RootReader, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("%w: 工作区允许根为空", ErrPathRejected)
	}
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%w: 单文档大小上限必须为正（%d）", ErrPathRejected, maxBytes)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("%w: 工作区允许根无法投影成真实路径（%s）: %w", ErrPathRejected, root, err)
	}
	abs, err := filepath.Abs(real)
	if err != nil {
		return nil, fmt.Errorf("%w: 工作区允许根无法规范为绝对路径（%s）: %w", ErrPathRejected, root, err)
	}
	return &RootReader{root: abs, maxBytes: maxBytes}, nil
}

// Load 读取并解码一份文档。顺序固定：词法判定 → 真实路径投影与根校验 → 拒绝链接 →
// 有界读取 → UTF-8 校验。任何一步失败都返回 `ErrPathRejected`，**零写入**。
// Load is the only entry point; every step that fails returns ErrPathRejected with
// nothing written.
func (r *RootReader) Load(ctx context.Context, path string) (DocumentSource, error) {
	if r == nil || r.root == "" {
		return DocumentSource{}, fmt.Errorf("%w: 读取器未装配", ErrPathRejected)
	}
	if err := ctx.Err(); err != nil {
		return DocumentSource{}, err
	}

	rel, err := r.normalize(path)
	if err != nil {
		return DocumentSource{}, err
	}
	if err := r.rejectLinks(rel); err != nil {
		return DocumentSource{}, err
	}

	// 真实路径投影 + 前缀比较：词法上"在根内"不等于事实上在根内，链接与 `..`
	// 都可能把目标指到根外，因此判定必须打在投影结果上（开发操作 §7 陷阱表第 7 条）。
	// The projection is what the prefix check runs on; a lexical check alone is bypassable.
	real, err := filepath.EvalSymlinks(filepath.Join(r.root, filepath.FromSlash(rel)))
	if err != nil {
		return DocumentSource{}, fmt.Errorf("%w: 来源路径无法投影成真实路径（%s）: %w", ErrPathRejected, rel, err)
	}
	if !withinRoot(real, r.root) {
		return DocumentSource{}, fmt.Errorf("%w: 来源路径在允许根之外（%s）", ErrPathRejected, rel)
	}

	raw, err := r.readBounded(ctx, rel)
	if err != nil {
		return DocumentSource{}, err
	}
	if !utf8.Valid(raw) {
		return DocumentSource{}, fmt.Errorf("%w: 文档不是合法的 UTF-8 文本（%s）", ErrPathRejected, rel)
	}

	sum := sha256.Sum256(raw)
	return DocumentSource{
		// Path 回**相对**路径（契约 `DocumentSummary.source_path`：工作区允许根内的
		// 相对路径），且已规范化——`./docs/a.md` 与 `docs/a.md` 因此落到同一文档行，
		// 不会因写法不同在 `UNIQUE (collection_id, source_path)` 下裂成两条。
		// The relative path is returned normalised so both spellings hit one document row.
		Path:        rel,
		Text:        string(raw),
		ContentHash: hex.EncodeToString(sum[:]),
		SizeBytes:   int64(len(raw)),
	}, nil
}

// normalize 做**纯词法**判定并返回以 `/` 分隔的相对路径：
// 空路径、绝对路径、盘符 / UNC、越出根的子路径、Windows 设备名一律拒绝。
// `filepath.IsLocal` 一次覆盖上述词法条件（Go 1.20+ 的标准库判据），这里不再手写第二份。
// normalize performs the lexical gate through filepath.IsLocal and returns a
// slash-separated relative path.
func (r *RootReader) normalize(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: 来源路径为空", ErrPathRejected)
	}
	if !filepath.IsLocal(path) {
		// 绝对路径 / 盘符 / UNC / `..` 越界 / Windows 保留设备名都落在这里。
		// Absolute paths, drive letters, UNC, escaping .. and reserved names land here.
		return "", fmt.Errorf("%w: 只接受工作区允许根内的相对路径（%s）", ErrPathRejected, path)
	}
	rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if rel == "." {
		return "", fmt.Errorf("%w: 来源路径指向允许根本身（%s）", ErrPathRejected, path)
	}
	return rel, nil
}

// rejectLinks 逐段 Lstat，拒绝任何符号链接 / 联接点成分——设计前置要求"拒绝
// 符号链接"，而投影成功并不区分"链接指向根内"与"真实路径"。
// rejectLinks refuses any symlink or junction component, even one that stays inside
// the root: the design rejects links outright, not merely escapes.
func (r *RootReader) rejectLinks(rel string) error {
	current := r.root
	for _, part := range strings.Split(rel, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			// 不存在的路径与"读不了"同样拒绝；`fs.ErrNotExist` 只影响文案。
			// Missing and unreadable paths are rejected alike; only the wording differs.
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: 来源文件不存在（%s）", ErrPathRejected, rel)
			}
			return fmt.Errorf("%w: 来源路径不可读（%s）: %w", ErrPathRejected, rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: 来源路径包含符号链接或联接点（%s）", ErrPathRejected, rel)
		}
	}
	return nil
}

// readBounded 在读取前限制上界：最多读 `maxBytes+1` 字节，超出即拒绝——不先把
// 整个文件读进内存再判大小，也不按上限截断后继续索引（截断会改变内容哈希）。
// readBounded never reads more than maxBytes+1 bytes and rejects anything over the
// cap; it never truncates, because truncation would change the content hash.
//
// 打开走 `os.Root`：路径解析与打开在同一个根句柄内完成，投影判定之后被换链
// （TOCTOU）也逃不出允许根；同时不再把变量路径交给 `os.Open`（gosec G304 的
// 判定面），安全属性由内核的目录句柄保证而不是审计例外。
// The open goes through os.Root so resolution is confined to the allowed root even
// if a component is swapped after the projection check.
func (r *RootReader) readBounded(ctx context.Context, rel string) ([]byte, error) {
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return nil, fmt.Errorf("%w: 工作区允许根打不开（%s）: %w", ErrPathRejected, rel, err)
	}
	defer func() { _ = root.Close() }()

	file, err := root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("%w: 来源文件打不开（%s）: %w", ErrPathRejected, rel, err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: 来源文件状态不可读（%s）: %w", ErrPathRejected, rel, err)
	}
	if !info.Mode().IsRegular() {
		// 目录 / 设备 / 管道都不是"文档"；读它们没有上限保证。
		// Directories, devices and pipes are not documents and have no size bound.
		return nil, fmt.Errorf("%w: 来源不是普通文件（%s）", ErrPathRejected, rel)
	}
	if info.Size() > r.maxBytes {
		return nil, fmt.Errorf("%w: 文件大小 %d 字节超过上限 %d", ErrPathRejected, info.Size(), r.maxBytes)
	}

	raw, err := io.ReadAll(io.LimitReader(file, r.maxBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: 来源文件读取失败（%s）: %w", ErrPathRejected, rel, err)
	}
	// Stat 与读取之间文件可能被改大：以实际读到的字节数为准再判一次。
	// The file may have grown between Stat and the read, so re-check the real size.
	if int64(len(raw)) > r.maxBytes {
		return nil, fmt.Errorf("%w: 文件超过上限 %d 字节（已读 %d）", ErrPathRejected, r.maxBytes, len(raw))
	}
	return raw, nil
}

// withinRoot 判定 real 是否落在 root 之内（含 root 自身）：两端都必须已投影。
// Windows 上折叠大小写比较（NTFS 不区分大小写），其余平台逐字比较。
// withinRoot compares two already-projected paths, folding case on Windows where the
// filesystem itself is case-insensitive.
func withinRoot(real, root string) bool {
	real, root = foldPathCase(filepath.Clean(real)), foldPathCase(filepath.Clean(root))
	rel, err := filepath.Rel(root, real)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// foldPathCase 在 Windows 上折叠大小写，其余平台原样返回（大小写敏感文件系统上
// 折叠会把两个不同目录判成一个）。
// foldPathCase lowercases only on Windows; folding elsewhere would merge distinct dirs.
func foldPathCase(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
