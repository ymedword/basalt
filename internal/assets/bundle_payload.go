package assets

// 本文件实现已发布资产的**载荷编解码**（US-25C 模块卡 §3.1.1 / FR-25C-04）：把一份
// 构建完成的 bundle 目录编码成可入库的确定性 tar 字节，并把入库字节还原成同哈希的
// 目录。分发介质是数据库载荷表（`agent_bundle_payloads`），因此字节必须是**内容决定
// 的**：同一份 bundle 无论从哪个构建目录、什么时候、以什么权限编码，载荷都逐字节
// 相同，哈希才能同时钉住"发的是哪份内容"。
//
// 编码判据：路径排序、固定 mode、零 / epoch mtime、不跟随符号链接；超限返回明确错误，
// **不截断**产出。解码判据：绝对路径、`..`、符号链接、非普通文件、超限一律拒绝，
// 校验通过之前**不写任何字节**；解出后按 `bundle.json` 复算 BundleHash，必须等于
// 发布指针里的 `wantHash`。
//
// The bundle payload codec: deterministic tar bytes for content-addressed distribution
// through the database, with fail-closed decoding that writes nothing until every entry
// has been validated and requires the recomputed BundleHash to equal the published hash.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// payloadEntryMode / payloadEntryTime 是载荷条目的固定元数据：mode 与包内文件权限
// 同口径，mtime 固定为 Unix epoch。两者都不取自源文件——源目录的 chmod / touch 不得
// 改变载荷字节。
// The fixed per-entry metadata; source file mode and mtime never leak into the payload.
const payloadEntryMode int64 = int64(filePerm)

var payloadEntryTime = time.Unix(0, 0)

// payloadFile 是待编码的一个文件（包内相对路径 + 内容）。
// payloadFile is one file pending encoding (bundle-relative path and bytes).
type payloadFile struct {
	name string
	data []byte
}

// EncodeBundle 把 `dir` 编码成确定性 tar 载荷，返回载荷字节与它的 sha256
// （`payload_sha256` 口径；与 `BundleHash` 的目录内容口径不同）。上限语义是
// `size_bytes <= maxBytes`：恰等于上限放行，超出返回明确错误而不是截断。
//
// 编码只看目录内容：普通文件按相对路径升序写入，mode / mtime 固定；符号链接、
// 设备、FIFO 等非普通条目一律拒绝编码（不跟随、不静默跳过——静默跳过会让载荷与
// 目录内容对不上，而调用方还以为编码成功）。
// EncodeBundle produces the deterministic payload bytes and their sha256; the source
// must contain only regular files, and exceeding maxBytes is an error, never truncation.
func EncodeBundle(ctx context.Context, dir string, maxBytes int64) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if dir == "" {
		return nil, "", fmt.Errorf("%w: bundle 目录为空", core.ErrAssetUnavailable)
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", fmt.Errorf("%w: bundle 目录 %s 不存在", core.ErrAssetUnavailable, dir)
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: 读取 bundle 目录 %s: %v", core.ErrAssetUnavailable, dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, "", fmt.Errorf("%w: bundle 目录 %s 不是真实目录", core.ErrAssetUnavailable, dir)
	}

	files := make([]payloadFile, 0, 16)
	walkErr := filepath.WalkDir(dir, func(candidate string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("路径 %s 是符号链接（不跟随符号链接）", candidate)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("路径 %s 不是普通文件", candidate)
		}
		rel, relErr := filepath.Rel(dir, candidate)
		if relErr != nil {
			return fmt.Errorf("计算 %s 的相对路径: %w", candidate, relErr)
		}
		rel = filepath.ToSlash(rel)
		if pathErr := safeRelPath(rel); pathErr != nil {
			return fmt.Errorf("路径 %q 非法: %v", rel, pathErr)
		}
		source, openErr := openRegular(dir, rel)
		if openErr != nil {
			return openErr
		}
		data, readErr := io.ReadAll(source)
		closeErr := source.Close()
		if readErr != nil {
			return fmt.Errorf("读取 %s: %w", rel, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭 %s: %w", rel, closeErr)
		}
		files = append(files, payloadFile{name: rel, data: data})
		return nil
	})
	if walkErr != nil {
		return nil, "", fmt.Errorf("%w: 编码 bundle %s: %v", core.ErrAssetUnavailable, dir, walkErr)
	}
	if len(files) == 0 {
		return nil, "", fmt.Errorf("%w: bundle 目录 %s 没有可编码的文件", core.ErrAssetUnavailable, dir)
	}
	// WalkDir 按目录树深度优先遍历，扁平路径列表并不天然有序（`a.txt` 与 `a/b` 的
	// 先后取决于分隔符与字符的比较），必须显式排序——排序是确定性判据的一部分。
	// The flat path list is not naturally sorted, so the order is imposed explicitly.
	slices.SortFunc(files, func(a, b payloadFile) int { return strings.Compare(a.name, b.name) })

	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, file := range files {
		header := &tar.Header{
			Name:     file.name,
			Mode:     payloadEntryMode,
			Size:     int64(len(file.data)),
			Typeflag: tar.TypeReg,
			ModTime:  payloadEntryTime,
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, "", fmt.Errorf("%w: 写载荷条目 %s: %v", core.ErrAssetUnavailable, file.name, err)
		}
		if len(file.data) == 0 {
			continue
		}
		if _, err := writer.Write(file.data); err != nil {
			return nil, "", fmt.Errorf("%w: 写载荷条目 %s: %v", core.ErrAssetUnavailable, file.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("%w: 收尾载荷 tar: %v", core.ErrAssetUnavailable, err)
	}
	payload := buffer.Bytes()
	if int64(len(payload)) > maxBytes {
		return nil, "", fmt.Errorf("载荷 %d 字节超过 max_bundle_bytes=%d（拒绝截断）", len(payload), maxBytes)
	}
	sum := sha256.Sum256(payload)
	return payload, hex.EncodeToString(sum[:]), nil
}

// payloadTarEntry 是一条已校验的载荷条目（只保留普通文件；内容已读入内存）。
// payloadTarEntry is one validated regular-file entry whose bytes are already loaded.
type payloadTarEntry struct {
	name string
	data []byte
}

// DecodeBundle 把载荷还原到 `dest`（不存在则创建；存在必须是空目录）。整份载荷先
// 全量校验（路径、类型、大小、重复），通过之后才开始写——被拒绝的解码**零写入**，
// 目标目录之外更不可能出现文件（路径已按 `safeRelPath` 拒收绝对路径与 `..`）。
// 写完后按 `bundle.json` 复算 BundleHash，与 `wantHash` 不等即删除解出目录并失败。
// DecodeBundle validates every entry before writing anything, then requires the
// recomputed BundleHash to equal wantHash.
func DecodeBundle(ctx context.Context, payload []byte, dest, wantHash string, maxBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if dest == "" {
		return errors.New("解码目标目录为空")
	}
	if !validHash(wantHash) {
		return fmt.Errorf("%w: 解码期望的 BundleHash %q 不是 64 位小写十六进制", core.ErrAssetVersionMismatch, wantHash)
	}
	if int64(len(payload)) > maxBytes {
		return fmt.Errorf("载荷 %d 字节超过 max_bundle_bytes=%d", len(payload), maxBytes)
	}
	entries, err := scanPayloadEntries(payload, maxBytes)
	if err != nil {
		return err
	}
	if err := prepareDir(dest, "解码目标目录"); err != nil {
		return err
	}
	// 这一层目录里只有本次解码写的内容（prepareDir 已要求空目录）：失败时整体移除，
	// 半成品不得被当成可用 bundle。
	// The directory only ever holds this decode's output, so a failed decode removes it.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dest)
		}
	}()
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writePayloadFile(dest, entry.name, entry.data); err != nil {
			return err
		}
	}
	manifest, err := Verify(ctx, dest)
	if err != nil {
		return fmt.Errorf("解码后校验 bundle: %w", err)
	}
	if manifest.BundleHash != wantHash {
		return fmt.Errorf("%w: 解码载荷的 BundleHash=%s，要求 %s",
			core.ErrAssetVersionMismatch, manifest.BundleHash, wantHash)
	}
	cleanup = false
	return nil
}

// scanPayloadEntries 只读地校验整份载荷并读出全部普通文件内容；任何路径逃逸、
// 非普通条目、重复条目或超限在写入之前就失败。
// scanPayloadEntries validates the whole payload read-only before any write happens.
func scanPayloadEntries(payload []byte, maxBytes int64) ([]payloadTarEntry, error) {
	reader := tar.NewReader(bytes.NewReader(payload))
	seen := make(map[string]struct{})
	var entries []payloadTarEntry
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: 解析载荷 tar: %v", core.ErrAssetVersionMismatch, err)
		}
		name := header.Name
		if err := safeRelPath(name); err != nil {
			return nil, fmt.Errorf("%w: 载荷条目 %q 非法: %v", core.ErrAssetVersionMismatch, name, err)
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("%w: 载荷条目 %q 重复", core.ErrAssetVersionMismatch, name)
		}
		seen[name] = struct{}{}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("%w: 载荷条目 %q 不是普通文件（typeflag=%d）",
				core.ErrAssetVersionMismatch, name, header.Typeflag)
		}
		if header.Size < 0 || header.Size > maxBytes {
			return nil, fmt.Errorf("%w: 载荷条目 %q 大小 %d 超出 0..%d",
				core.ErrAssetVersionMismatch, name, header.Size, maxBytes)
		}
		if total > maxBytes-header.Size {
			return nil, fmt.Errorf("%w: 载荷解出总量超过 max_bundle_bytes=%d", core.ErrAssetVersionMismatch, maxBytes)
		}
		total += header.Size
		data, readErr := io.ReadAll(reader)
		if readErr != nil {
			return nil, fmt.Errorf("%w: 读取载荷条目 %q: %v", core.ErrAssetVersionMismatch, name, readErr)
		}
		if int64(len(data)) != header.Size {
			return nil, fmt.Errorf("%w: 载荷条目 %q 声明 %d 字节，实际 %d 字节",
				core.ErrAssetVersionMismatch, name, header.Size, len(data))
		}
		entries = append(entries, payloadTarEntry{name: name, data: data})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: 载荷没有任何普通文件条目", core.ErrAssetVersionMismatch)
	}
	return entries, nil
}

// writePayloadFile 把一条载荷条目写进 `root`。路径已由 safeRelPath 校验过；写入仍
// 经父目录句柄 + 裸名（`os.Root`），与 copyBundleFile 同一防线，且 O_EXCL 拒绝覆盖
// 同名条目。
// writePayloadFile writes one validated entry through a parent-directory handle.
func writePayloadFile(root, rel string, data []byte) error {
	target := filepath.Join(root, filepath.FromSlash(rel))
	parentDir := filepath.Dir(target)
	if err := os.MkdirAll(parentDir, dirPerm); err != nil {
		return fmt.Errorf("创建目录 %s: %w", parentDir, err)
	}
	parent, err := os.OpenRoot(parentDir)
	if err != nil {
		return fmt.Errorf("打开目录 %s: %w", parentDir, err)
	}
	file, openErr := parent.OpenFile(filepath.Base(target), os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	parentCloseErr := parent.Close()
	if openErr != nil {
		return fmt.Errorf("创建文件 %s: %w", target, openErr)
	}
	if parentCloseErr != nil {
		_ = file.Close() // 主错误是句柄关闭失败；目标文件随即随目录清理丢弃
		return fmt.Errorf("关闭目录 %s: %w", parentDir, parentCloseErr)
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("写入文件 %s: %w", target, writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("关闭文件 %s: %w", target, closeErr)
	}
	return nil
}
