package workspace

// 本文件是 Agent 受管文件集合的读面与多文件条件提交（颗粒度文档 §3.1.5 / 约束 30）：
// `ManagedAgentFiles` 只收录 Agent 根目录下的普通文件并按 path 升序计算集合 ETag；
// `SubmitAgentFiles` 在调用方已持有 Agent 写锁的前提下，锁内重读并比较 `If-Match`，
// 通过后在 `<name>.staging` 构建**完整副本**（未列出的现有文件原样复制）、写入提交的
// 文件、两次 rename 交换，再返回落盘后重新读取的文件清单与新的集合 ETag。
//
// **锁与所有权**：本文件不取实例锁、不开 DB、不写管理操作行——412 与操作行的时序由
// `app.AgentAdmin.SubmitFiles` 编排（比较前置、预写失败零写入）。并发安全来自调用方
// 持有的 Agent 写锁：集合 ETag 的比较与候选树的构建必须在同一临界区内完成。
// This file holds the managed-file read surface and the conditional multi-file submit.
// It takes no instance lock and writes no operation row; the caller owns both.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

// ManagedAgentFiles 读取 Agent 根目录下的受管文件集合与集合 ETag（约束 30）：
// 只收录**普通文件**（不递归子目录、不跟随软连接），跳过 `.staging` / `.backup` 残留
// 名，按 path 升序返回；path 相对 Agent 目录并规范化为 `/`。
// ManagedAgentFiles lists the regular files directly under the agent directory and
// returns the set ETag; symlinks and staging/backup residue are not managed files.
func ManagedAgentFiles(root, name string) ([]core.AuthorFile, string, error) {
	if err := ValidateAgentName(name); err != nil {
		return nil, "", err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := path.Join(agentsDirName, name)
	files, err := managedFilesIn(base, relDir)
	if err != nil {
		return nil, "", err
	}
	return files, FileSetETag(files), nil
}

// FileSetETag 计算受管文件集合的强 ETag（约束 30）：按 path 升序把每个文件的
// `path\0sha256(内容)\0` 串接后求 sha256，渲染为与单文件 ETag 同语法的
// `"sha256:<64 位小写十六进制>"`。**不得**先重新序列化文件再求值——ETag 必须只由
// 磁盘上的原始字节决定，且与 `GET /agents/{name}/files` 的顺序无关。
// FileSetETag hashes the canonical `path\0sha256\0` sequence of the file set; it is
// byte-defined and independent of the order the caller happens to pass.
func FileSetETag(files []core.AuthorFile) string {
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b core.AuthorFile) int {
		return strings.Compare(a.Path, b.Path)
	})

	var canonical strings.Builder
	for _, file := range sorted {
		sum := sha256.Sum256([]byte(file.Content))
		canonical.WriteString(file.Path)
		canonical.WriteByte(0)
		canonical.WriteString(hex.EncodeToString(sum[:]))
		canonical.WriteByte(0)
	}
	return ContentETag([]byte(canonical.String()))
}

// SubmitAgentFiles 执行一次有条件多文件提交（约束 30）：锁内重读当前受管文件集合并
// 比较 `ifMatch`——缺失、弱 ETag、`*`、形状非法或不匹配一律返回
// `core.ErrPreconditionFailed` 且**零写入**；通过后把现有目录全量复制到
// `agents/<name>.staging`，写入提交文件（未列出的现有文件原样保留），两次 rename
// 交换（正式目录 → `.backup`，staging → 正式目录，删除 `.backup`），最后重新读取
// 落盘后的受管文件并返回其清单与新集合 ETag。
//
// 崩溃残留的处置规则（与启动恢复同一套，约束 30）：正式目录缺失 + `.backup` 在 →
// 回滚；正式目录完整 + `.backup` 在 → 前滚清理。本函数发现 `.staging` / `.backup`
// 残留时拒绝覆盖并点名——残留的裁决属于恢复路径，不在这里替运维决定删除。
// SubmitAgentFiles re-reads the managed set under the caller's lock, refuses stale
// preconditions without writing, then commits a full-copy staging tree via two renames.
func SubmitAgentFiles(root, name, ifMatch string, drafts []core.AuthorFile) ([]core.AgentFileInfo, string, error) {
	if err := ValidateAgentName(name); err != nil {
		return nil, "", err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, "", fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := path.Join(agentsDirName, name)
	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("目标 Agent 目录不存在：%s", relDir)
		}
		return nil, "", fmt.Errorf("检查 Agent 目录 %s: %w", relDir, statErr)
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("目标路径不是 Agent 目录：%s（%s）", relDir, fileKind(info))
	}

	// 锁内重读基础版本：比较必须在候选构建之前，且与构建同处调用方的 Agent 写锁内。
	current, err := managedFilesIn(base, relDir)
	if err != nil {
		return nil, "", err
	}
	currentETag := FileSetETag(current)
	if !strongETagMatches(ifMatch, currentETag) {
		return nil, "", fmt.Errorf("%w: 当前集合 ETag 为 %s", core.ErrPreconditionFailed, currentETag)
	}

	normalized, err := NormalizeAgentFiles(drafts)
	if err != nil {
		// 入参非法在 core 边界上统一为 *core.InvalidInputError（与 content_edit.go 的
		// 出口同一口径）：管理写据此映射 400，且此处已声明 zero-write（还没建 staging）。
		return nil, "", writeFailed(name, &core.InvalidInputError{
			Tool: core.ActionAgentFilesUpdate, Reason: err.Error(),
		})
	}

	relStaging := path.Join(agentsDirName, name+stagingSuffix)
	relBackup := path.Join(agentsDirName, name+backupSuffix)
	for _, rel := range []string{relStaging, relBackup} {
		if _, statErr := base.Lstat(filepath.FromSlash(rel)); statErr == nil {
			return nil, "", fmt.Errorf(
				"提交残留目录已存在，拒绝覆盖：%s（确认无价值后手工删除，或重启由恢复路径处置）", rel)
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return nil, "", fmt.Errorf("检查残留目录 %s: %w", rel, statErr)
		}
	}

	if err := base.Mkdir(filepath.FromSlash(relStaging), dirPerm); err != nil {
		return nil, "", writeFailed(name, fmt.Errorf("创建 staging 目录 %s: %w", relStaging, err))
	}
	discard := func(cause error) error {
		if removeErr := base.RemoveAll(filepath.FromSlash(relStaging)); removeErr != nil {
			return errors.Join(cause,
				fmt.Errorf("清理残留目录 %s 失败，请手工删除：%w", relStaging, removeErr))
		}
		return cause
	}

	// 完整副本：目录里的每个普通文件都进入 staging，未列出的文件因此原样保留。
	existing, err := copyAgentTree(base, relDir, relStaging)
	if err != nil {
		return nil, "", writeFailed(name, discard(err))
	}
	if err := applyDrafts(base, relStaging, normalized, existing); err != nil {
		// 与现有文件的大小写冲突只能在这里判定（需要磁盘上的当前集合）；它仍是入参
		// 问题 → 400，discard 保证 staging 零残留。
		return nil, "", writeFailed(name, discard(&core.InvalidInputError{
			Tool: core.ActionAgentFilesUpdate, Reason: err.Error(),
		}))
	}
	agentMD, err := base.Lstat(filepath.FromSlash(path.Join(relStaging, agentDefinitionFile)))
	if err != nil || !agentMD.Mode().IsRegular() {
		return nil, "", writeFailed(name, discard(fmt.Errorf(
			"候选树缺少 %s（提交后每个 Agent 必须仍有定义文件）", agentDefinitionFile)))
	}

	// 两次 rename 交换：正式目录 → .backup；staging → 正式目录；删除 .backup。
	if err := base.Rename(filepath.FromSlash(relDir), filepath.FromSlash(relBackup)); err != nil {
		return nil, "", writeFailed(name, discard(fmt.Errorf(
			"交换 Agent 目录 %s → %s: %w", relDir, relBackup, err)))
	}
	if err := base.Rename(filepath.FromSlash(relStaging), filepath.FromSlash(relDir)); err != nil {
		if rollbackErr := base.Rename(filepath.FromSlash(relBackup), filepath.FromSlash(relDir)); rollbackErr != nil {
			// 正式目录缺失、旧树在 .backup、新树在 .staging：保留现场交由启动恢复裁决。
			return nil, "", writeFailed(name, errors.Join(
				fmt.Errorf("安装候选目录 %s → %s: %w", relStaging, relDir, err),
				fmt.Errorf("回滚目录 %s → %s 失败，Agent 目录处于不确定状态: %w", relBackup, relDir, rollbackErr)))
		}
		return nil, "", writeFailed(name, discard(fmt.Errorf(
			"安装候选目录 %s → %s: %w", relStaging, relDir, err)))
	}
	if removeErr := base.RemoveAll(filepath.FromSlash(relBackup)); removeErr != nil {
		// 新树已生效；旧副本残留正是恢复路径可前滚清理的状态——如实记 WARN，不把
		// 已生效的提交说成失败（约束 30 的崩溃残留规则）。
		slog.Default().Warn("Agent 提交后清理 .backup 残留失败（启动恢复会前滚清理）",
			"agent", name, "path", relBackup, "detail", removeErr.Error())
	}

	files, err := managedFilesIn(base, relDir)
	if err != nil {
		return nil, "", err
	}
	infos := make([]core.AgentFileInfo, len(files))
	for i, file := range files {
		infos[i] = core.AgentFileInfo{Path: file.Path, Size: int64(len(file.Content))}
	}
	return infos, FileSetETag(files), nil
}

// managedFilesIn 读取一个 Agent 目录的受管文件（约束 30）：`fs.ReadDir` 已按文件名
// 升序返回；目录、软连接、特殊文件与 `.staging` / `.backup` 残留名一律跳过。
// managedFilesIn lists the managed regular files directly under the agent directory.
func managedFilesIn(base *os.Root, relDir string) ([]core.AuthorFile, error) {
	entries, err := fs.ReadDir(base.FS(), relDir)
	if err != nil {
		return nil, fmt.Errorf("读取 Agent 目录 %s: %w", relDir, err)
	}

	files := make([]core.AuthorFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, stagingSuffix) || strings.HasSuffix(name, backupSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("读取条目信息 %s/%s: %w", relDir, name, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := fs.ReadFile(base.FS(), path.Join(relDir, name))
		if err != nil {
			return nil, fmt.Errorf("读取 %s/%s: %w", relDir, name, err)
		}
		files = append(files, core.AuthorFile{Path: name, Content: string(data)})
	}
	slices.SortFunc(files, func(a, b core.AuthorFile) int {
		return strings.Compare(a.Path, b.Path)
	})
	return files, nil
}

// copyAgentTree 把一个 Agent 目录**全量**复制到 staging：目录结构照搬、普通文件逐字节
// 复制；目录内出现软连接 / 特殊文件或大小写重复的路径时失败关闭（候选树必须能被
// 逐字节复算，且 Windows 上大小写重复会互相覆盖）。返回 staging 内相对路径的
// 小写索引（key 小写 → 原样路径），供写入提交文件时判大小写冲突。
// copyAgentTree copies the whole agent directory and returns a lowercase path index.
func copyAgentTree(base *os.Root, srcRel, dstRel string) (map[string]string, error) {
	seen := make(map[string]string, 8)
	walkErr := fs.WalkDir(base.FS(), srcRel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRel, p)
		if err != nil {
			return fmt.Errorf("计算相对路径 %s: %w", p, err)
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			if err := base.MkdirAll(filepath.FromSlash(path.Join(dstRel, rel)), dirPerm); err != nil {
				return fmt.Errorf("创建候选目录 %s: %w", path.Join(dstRel, rel), err)
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("读取条目信息 %s: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("目录中含非普通文件：%s（%s）", p, fileKind(info))
		}
		lower := strings.ToLower(rel)
		if previous, dup := seen[lower]; dup && previous != rel {
			return fmt.Errorf("目录中存在大小写重复的路径：%s 与 %s", previous, rel)
		}
		seen[lower] = rel

		data, err := fs.ReadFile(base.FS(), p)
		if err != nil {
			return fmt.Errorf("读取 %s: %w", p, err)
		}
		if err := base.WriteFile(filepath.FromSlash(path.Join(dstRel, rel)), data, filePerm); err != nil {
			return fmt.Errorf("复制 %s 到候选目录: %w", p, err)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return seen, nil
}

// applyDrafts 把提交的文件写进 staging（覆盖同名的现有副本）：同一路径重复出现
// （含仅大小写不同）或与现有文件仅大小写不同 → 拒绝；每个路径按约束 41 逐段校验、
// 内容按 UTF-8 校验。写前逐级创建父目录，路径始终在 staging 内（os.Root 兜底）。
// applyDrafts writes the submitted files into staging and rejects case collisions.
func applyDrafts(base *os.Root, relStaging string, drafts []core.AuthorFile,
	existing map[string]string,
) error {
	seen := make(map[string]string, len(drafts))
	for i, draft := range drafts {
		lower := strings.ToLower(draft.Path)
		if previous, dup := seen[lower]; dup {
			return fmt.Errorf("提交的文件路径重复（大小写不敏感）：%s 与 %s", previous, draft.Path)
		}
		seen[lower] = draft.Path

		if onDisk, ok := existing[lower]; ok && onDisk != draft.Path {
			return fmt.Errorf("提交的文件路径与现有文件仅大小写不同：%s 与 %s", onDisk, draft.Path)
		}

		target := path.Join(relStaging, draft.Path)
		if err := base.MkdirAll(filepath.FromSlash(path.Dir(target)), dirPerm); err != nil {
			return fmt.Errorf("创建候选目录 %s: %w", path.Dir(target), err)
		}
		if err := base.WriteFile(filepath.FromSlash(target), []byte(draft.Content), filePerm); err != nil {
			return fmt.Errorf("写入提交的第 %d 个文件 %s: %w", i+1, draft.Path, err)
		}
	}
	return nil
}

// NormalizeAgentFiles 按约束 41 规整并校验一次提交的文件集合：路径相对、分隔符归一为
// `/`、逐段合法、无控制字符、不以 `.staging` / `.backup` 结尾；正文必须是 UTF-8 文本；
// 同一集合内仅大小写不同的重复路径拒绝。返回规整后的副本（路径已归一），供候选树
// 合并与集合 ETag 计算共用。
//
// 本函数**不读写磁盘**：管理写因此可以在预写操作行之前完成纯请求校验（约束 1），
// 失败不留操作行。与磁盘现有文件的大小写冲突只能在 staging 构建时判定（applyDrafts），
// 那一层由调用方在预写之后执行。
// NormalizeAgentFiles validates one submitted file set under constraint 41 and returns
// normalized copies. It touches no disk, so callers can enforce pure request validation
// before pre-writing the operation row; case collisions with on-disk files can only be
// detected later, while building the staging tree.
func NormalizeAgentFiles(drafts []core.AuthorFile) ([]core.AuthorFile, error) {
	out := make([]core.AuthorFile, len(drafts))
	seen := make(map[string]string, len(drafts))
	for i, draft := range drafts {
		normalized, err := normalizeAgentFilePath(draft.Path)
		if err != nil {
			return nil, fmt.Errorf("提交的第 %d 个文件路径非法: %w", i+1, err)
		}
		if !utf8.ValidString(draft.Content) {
			return nil, fmt.Errorf("提交的文件 %s 不是合法的 UTF-8 文本", normalized)
		}
		lower := strings.ToLower(normalized)
		if previous, dup := seen[lower]; dup {
			return nil, fmt.Errorf("提交的文件路径重复（大小写不敏感）：%s 与 %s", previous, normalized)
		}
		seen[lower] = normalized
		out[i] = core.AuthorFile{Path: normalized, Content: draft.Content}
	}
	return out, nil
}

// normalizeAgentFilePath 按约束 41 校验并规整提交路径：`\` 归一为 `/`，拒绝绝对路径
// （前导 `/`、Windows 盘符）、空段、`.` / `..` 段、控制字符（含 NUL）与
// `.staging` / `.backup` 后缀。只做校验与分隔符归一，**不做** `path.Clean`——
// 清洗会把 `a/../b` 洗成合法路径，正好放过要拒绝的越界形态。
//
// 与 `core.normalizeAuthorPath`（作者草稿解析）是同一组固定规则的两处落地：前者校验
// 模型输出、后者校验管理写请求；两者都不得放宽，规则变更必须同步。
// normalizeAgentFilePath validates and normalizes a submitted path under
// constraint 41. It deliberately never applies path.Clean.
func normalizeAgentFilePath(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("路径不得为空")
	}
	if !utf8.ValidString(raw) {
		return "", errors.New("路径不是合法的 UTF-8 文本")
	}

	normalized := strings.ReplaceAll(raw, `\`, "/")
	if strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("路径不得是绝对路径：%q", raw)
	}
	if len(normalized) >= 2 && normalized[1] == ':' && isASCIIAlpha(normalized[0]) {
		return "", fmt.Errorf("路径不得是绝对路径：%q", raw)
	}
	for _, r := range normalized {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("路径不得包含控制字符：%q", raw)
		}
	}
	lower := strings.ToLower(normalized)
	if strings.HasSuffix(lower, stagingSuffix) || strings.HasSuffix(lower, backupSuffix) {
		return "", fmt.Errorf("路径不得以 %s / %s 结尾：%q", stagingSuffix, backupSuffix, raw)
	}
	for _, segment := range strings.Split(normalized, "/") {
		switch segment {
		case "":
			return "", fmt.Errorf("路径存在空段：%q", raw)
		case ".", "..":
			return "", fmt.Errorf("路径不得包含 . 或 .. 段：%q", raw)
		}
	}
	return normalized, nil
}

// isASCIIAlpha 判一个字节是否是 ASCII 字母（Windows 盘符判定用）。
// isASCIIAlpha reports whether the byte is an ASCII letter.
func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
