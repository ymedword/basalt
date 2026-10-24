package workspace

// 本文件是技能管理写的**文件面**（颗粒度文档 §3.1.4 / 约束 24、25、26）：导入边界
// 校验、工作副本全量替换、不可变版本快照与删除归档。它**不取实例锁、不开 DB、不写
// 管理操作行**——操作行与条件写的时序由 app 层编排（约束 1 / 3 / 25）。
//
// 三条边界在这里落地：
//  1. 导入只接受相对路径的普通文本文件（约束 24）：越界 / 盘符 / UNC / `..` /
//     控制字符 / 隐藏段 / 大小写重复 / 非 UTF-8 / 含 NUL 一律拒绝；单文件 ≤ 256 KiB、
//     总量 ≤ 2 MiB、文件数 ≤ 200。文件按字节存储，**从不执行**，也不跟随软连接。
//  2. 工作副本是编辑面、版本目录是发布面（约束 25）：PUT 是全量替换——候选在
//     `skills/<name>.staging` 构建、校验通过后两次 rename 交换，失败回滚；已发布
//     版本目录写完后只读。`SnapshotSkill` 经 `Materialize` 的哈希比对，保证发布的
//     就是校验过的那份字节。
//  3. 校验与写入共用同一份判据（约束 26）：`buildSkillCandidate` 是校验端点与三处
//     写入口共用的唯一闸门，且不触盘——"校验不落盘"因此是结构性的，不靠调用方自觉。
//
// This file is the file-level surface of skill management writes: import-boundary
// validation, full-replacement work copies, immutable version snapshots and delete
// archives. It takes no instance lock, opens no DB and writes no operation row.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// 技能导入的三条硬上限（约束 24）：单文件 256 KiB、总量 2 MiB、文件数 200。
	// HTTP 请求体另有 http.max_request_bytes（缺省 1 MiB）；总量上限因此是
	// min(2 MiB, http.max_request_bytes)——请求体那一半由 web 层把关，本文件只判
	// 固定的 2 MiB，不在工作区层读配置。
	// The three fixed import limits; the HTTP body cap is enforced by the web layer.
	skillImportMaxFileBytes  = 256 * 1024
	skillImportMaxTotalBytes = 2 * 1024 * 1024
	skillImportMaxFiles      = 200

	// 已发布版本目录的只读权限（约束 25）：文件 0o444、目录 0o555。Windows 上
	// os.Chmod 只映射只读属性，语义仍是"不许再改写"。
	// Read-only permissions of a published version directory.
	skillVersionFilePerm os.FileMode = 0o444
	skillVersionDirPerm  os.FileMode = 0o555
)

// SkillFile 是技能候选里的一个文件：路径相对技能目录、正斜杠；内容按字节保存
// （文本闸门在构建候选时已判过），SHA256 / Size 由构建结果给出，不采信调用方填值。
// SkillFile is one file of a skill candidate. The hashes are outputs of candidate
// construction; caller-supplied values are ignored.
type SkillFile struct {
	Path    string
	Content []byte
	SHA256  string
	Size    int64
}

// SkillCandidate 是一次通过边界校验的技能候选：文件按 path 升序、清单哈希可复算。
// ContentHash 与 manifest_json 的字节定义与已发布版本一致（sha256(规整 JSON 清单)），
// 因此它同时是技能的 ETag（约束 4）。
// SkillCandidate is a validated skill candidate: files sorted by path and the
// manifest hash reproducible from bytes. ContentHash doubles as the skill ETag.
type SkillCandidate struct {
	Name         string
	Files        []SkillFile
	ContentHash  string
	ManifestJSON string
}

// ValidateSkillDraft 是技能创建 / 导入 / 更新 / 校验端点**共用**的前置闸门（约束 26）：
// 只做导入边界与 SKILL.md 校验，**不读写磁盘、不写管理操作行、不执行脚本**。任何
// 边界违反都归入 `core.ErrImportRejected`（web 映射 400 `IMPORT_REJECTED`），文案
// 点名违规路径与上限。
// ValidateSkillDraft is the single gate shared by create/import/update/validate. It
// touches no disk and classifies every boundary violation as ErrImportRejected.
func ValidateSkillDraft(name string, files []core.AuthorFile) (SkillCandidate, error) {
	converted := make([]SkillFile, len(files))
	for i, file := range files {
		converted[i] = SkillFile{Path: file.Path, Content: []byte(file.Content)}
	}
	cand, err := buildSkillCandidate(name, converted)
	if err != nil {
		return SkillCandidate{}, importRejected(err)
	}
	return cand, nil
}

// WriteSkillWorkCopy 把候选**全量替换**进工作副本 `skills/<name>/`（约束 25）：
// 候选在 `skills/<name>.staging` 构建，成功后两次 rename 交换（正式目录 → `.backup`，
// staging → 正式目录，删除 `.backup`）；任一步失败清理 staging 并保留旧副本。未列出的
// 现有文件随旧树一起消失——这是"全量替换"与 Agent 多文件提交"保留未列出文件"的区别。
//
// 入口先重建候选：候选的哈希必须与文件清单自洽（防止校验后被改动），边界规则再跑一遍
// 是纵深防御，不替代 `ValidateSkillDraft`。发现 `.staging` / `.backup` 残留时拒绝写入
// 并点名——残留的裁决属于运维 / 恢复路径，不在这里替调用方决定删除。
// WriteSkillWorkCopy replaces the work copy wholesale via a staging tree and two
// renames. Existing staging/backup residue is refused rather than resolved here.
func WriteSkillWorkCopy(root string, cand SkillCandidate) error {
	rebuilt, err := buildSkillCandidate(cand.Name, cand.Files)
	if err != nil {
		return skillWriteFailed(cand.Name, importRejected(err))
	}
	if rebuilt.ContentHash != cand.ContentHash || rebuilt.ManifestJSON != cand.ManifestJSON {
		return skillWriteFailed(cand.Name, fmt.Errorf(
			"技能候选的哈希与文件清单不一致（候选在构建后被改动），拒绝写入"))
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return skillWriteFailed(cand.Name, fmt.Errorf("打开工作区目录 %s: %w", root, err))
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := path.Join(skillsDirName, cand.Name)
	relStaging := relDir + stagingSuffix
	relBackup := relDir + backupSuffix

	for _, rel := range []string{relStaging, relBackup} {
		if _, statErr := base.Lstat(filepath.FromSlash(rel)); statErr == nil {
			return skillWriteFailed(cand.Name, fmt.Errorf(
				"写入残留目录已存在，拒绝覆盖：%s（确认无价值后手工删除）", rel))
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return skillWriteFailed(cand.Name, fmt.Errorf("检查残留目录 %s: %w", rel, statErr))
		}
	}

	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	exists := statErr == nil
	switch {
	case exists:
		if !info.IsDir() {
			return skillWriteFailed(cand.Name, fmt.Errorf(
				"技能工作副本路径 %s 已存在且不是目录（%s），拒绝覆盖", relDir, fileKind(info)))
		}
	case !errors.Is(statErr, fs.ErrNotExist):
		return skillWriteFailed(cand.Name, fmt.Errorf("检查技能工作副本 %s: %w", relDir, statErr))
	}

	if err := writeSkillStage(base, relStaging, rebuilt.Files); err != nil {
		return skillWriteFailed(cand.Name, err)
	}
	discard := func(cause error) error {
		if removeErr := base.RemoveAll(filepath.FromSlash(relStaging)); removeErr != nil {
			return errors.Join(cause,
				fmt.Errorf("清理残留目录 %s 失败，请手工删除：%w", relStaging, removeErr))
		}
		return cause
	}

	if exists {
		if err := base.Rename(filepath.FromSlash(relDir), filepath.FromSlash(relBackup)); err != nil {
			return skillWriteFailed(cand.Name, discard(fmt.Errorf(
				"交换技能工作副本 %s → %s: %w", relDir, relBackup, err)))
		}
	}
	if err := base.Rename(filepath.FromSlash(relStaging), filepath.FromSlash(relDir)); err != nil {
		if exists {
			if rollbackErr := base.Rename(
				filepath.FromSlash(relBackup), filepath.FromSlash(relDir),
			); rollbackErr != nil {
				// 正式目录缺失、旧树在 .backup、新树在 .staging：保留现场交由人工裁决。
				return skillWriteFailed(cand.Name, errors.Join(
					fmt.Errorf("安装技能候选目录 %s → %s: %w", relStaging, relDir, err),
					fmt.Errorf("回滚目录 %s → %s 失败，技能工作副本处于不确定状态: %w",
						relBackup, relDir, rollbackErr)))
			}
		}
		return skillWriteFailed(cand.Name, discard(fmt.Errorf(
			"安装技能候选目录 %s → %s: %w", relStaging, relDir, err)))
	}
	if exists {
		if removeErr := base.RemoveAll(filepath.FromSlash(relBackup)); removeErr != nil {
			// 新树已生效；旧副本残留可被下一次写入前的人工清理处置——如实记 WARN，
			// 不把已生效的替换说成失败。
			slog.Default().Warn("技能写入后清理 .backup 残留失败",
				"skill", cand.Name, "path", relBackup, "detail", removeErr.Error())
		}
	}
	return nil
}

// SnapshotSkill 把候选冻结为**新发布**的不可变版本
// `versions/skills/<name>/<resource_uid>/<versionNo>/`（约束 25）并返回契约字面量
// storage_path。落盘交给 `SkillLibrary.Materialize`：它重算工作副本哈希并要求与候选
// 一致——工作副本在校验后被改动（含被塞进软连接 / 特殊文件）都会在此失败，**不复制
// 不一致的字节**。目录写完后置为只读；已存在且哈希一致的目录按幂等复用，不一致的拒绝
// 覆盖。旧扁平目录不受影响：新身份与上一代的版本 1 不会撞在同一个路径上。
// SnapshotSkill freezes the candidate into the identity-segmented version dir through
// Materialize's hash re-check, then makes the published tree read-only.
func SnapshotSkill(root string, cand SkillCandidate, uid core.ResourceUID, versionNo int) (string, error) {
	version, err := NewSkillLibrary(root).Materialize(
		context.Background(), cand.Name, uid, versionNo, cand.ContentHash)
	if err != nil {
		return "", skillWriteFailed(cand.Name, err)
	}
	relDir, err := PublishedVersionRelDir(version.Dir)
	if err != nil {
		return "", skillWriteFailed(cand.Name, err)
	}
	if err := makeSkillVersionReadOnly(root, relDir); err != nil {
		return "", skillWriteFailed(cand.Name, err)
	}
	return version.Dir, nil
}

// ArchiveSkillWorkCopy 把技能工作副本移入工作区归档区
// `archive/skills/<name>-<YYYYMMDD-HHMMSS>`（UTC，与 Agent 归档同一时间基准 / 根目录）。
// 返回工作区相对路径；目标已存在 → `core.ErrConflict`（不自动换名，源目录原样保留）；
// 移动用 rename，内容逐字节保留。
// ArchiveSkillWorkCopy moves the work copy into archive/skills/<name>-<stamp> and
// refuses to overwrite an existing archive target.
func ArchiveSkillWorkCopy(root, name string, at time.Time) (string, error) {
	if err := ValidateAgentName(name); err != nil {
		return "", skillWriteFailed(name, fmt.Errorf("技能名非法: %w", err))
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return "", skillWriteFailed(name, fmt.Errorf("打开工作区目录 %s: %w", root, err))
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := path.Join(skillsDirName, name)
	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	switch {
	case statErr == nil:
		if !info.IsDir() {
			return "", skillWriteFailed(name, fmt.Errorf(
				"技能工作副本 %s 不是目录（%s），拒绝归档", relDir, fileKind(info)))
		}
	case errors.Is(statErr, fs.ErrNotExist):
		return "", skillWriteFailed(name, fmt.Errorf("技能工作副本不存在：%s", relDir))
	default:
		return "", skillWriteFailed(name, fmt.Errorf("检查技能工作副本 %s: %w", relDir, statErr))
	}

	stamp := at.UTC().Format(archiveStampLayout)
	archiveRel := path.Join(archiveDirName, skillsDirName, name+"-"+stamp)
	if _, statErr := base.Lstat(filepath.FromSlash(archiveRel)); statErr == nil {
		return "", skillWriteFailed(name, &core.AdminError{
			Kind:    core.ErrConflict,
			Message: fmt.Sprintf("归档目标已存在，拒绝覆盖：%s（请稍后重试）", archiveRel),
		})
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", skillWriteFailed(name, fmt.Errorf("检查归档目标 %s: %w", archiveRel, statErr))
	}

	if err := base.MkdirAll(filepath.FromSlash(path.Dir(archiveRel)), dirPerm); err != nil {
		return "", skillWriteFailed(name, fmt.Errorf("创建归档目录 %s: %w", path.Dir(archiveRel), err))
	}
	if err := base.Rename(filepath.FromSlash(relDir), filepath.FromSlash(archiveRel)); err != nil {
		return "", skillWriteFailed(name, fmt.Errorf(
			"归档技能工作副本 %s → %s: %w", relDir, archiveRel, err))
	}
	return archiveRel, nil
}

// buildSkillCandidate 是导入边界与 SKILL.md 校验的唯一实现（约束 24 / 26）：路径逐段
// 校验、内容 UTF-8 且无 NUL、大小写折叠后不重复、三条上限、根级 SKILL.md 必须存在且
// frontmatter 可解析；通过后按 path 升序产出文件清单与规整 JSON。**不触盘**。
// buildSkillCandidate is the single boundary implementation; it never touches disk.
func buildSkillCandidate(name string, files []SkillFile) (SkillCandidate, error) {
	if err := ValidateAgentName(name); err != nil {
		return SkillCandidate{}, fmt.Errorf("技能名非法: %w", err)
	}
	if len(files) == 0 {
		return SkillCandidate{}, fmt.Errorf("技能 %s 的文件列表为空，至少需要根级 %s",
			name, skillDefinitionFile)
	}
	if len(files) > skillImportMaxFiles {
		return SkillCandidate{}, fmt.Errorf("技能 %s 的文件数量 %d 超过上限 %d",
			name, len(files), skillImportMaxFiles)
	}

	seen := make(map[string]string, len(files))
	out := make([]SkillFile, 0, len(files))
	hashes := make([]core.FileHash, 0, len(files))
	var (
		total      int64
		hasSkillMD bool
		skillMD    []byte
	)
	for i, file := range files {
		normalized, err := normalizeSkillFilePath(file.Path)
		if err != nil {
			return SkillCandidate{}, fmt.Errorf("第 %d 个技能文件路径非法: %w", i+1, err)
		}
		lower := strings.ToLower(normalized)
		if previous, dup := seen[lower]; dup {
			return SkillCandidate{}, fmt.Errorf(
				"技能文件路径大小写折叠后重复：%s 与 %s", previous, normalized)
		}
		seen[lower] = normalized

		if !utf8.Valid(file.Content) {
			return SkillCandidate{}, fmt.Errorf(
				"技能文件 %s 不是合法的 UTF-8 文本（二进制文件不允许导入）", normalized)
		}
		if bytes.IndexByte(file.Content, 0) >= 0 {
			return SkillCandidate{}, fmt.Errorf("技能文件 %s 含 NUL 字节，拒绝导入", normalized)
		}

		size := int64(len(file.Content))
		if size > skillImportMaxFileBytes {
			return SkillCandidate{}, fmt.Errorf(
				"技能文件 %s 大小 %d 字节超过单文件上限 %d 字节", normalized, size, skillImportMaxFileBytes)
		}
		total += size
		if total > skillImportMaxTotalBytes {
			return SkillCandidate{}, fmt.Errorf(
				"技能 %s 的文件总量超过上限 %d 字节（当前已到 %d 字节）",
				name, skillImportMaxTotalBytes, total)
		}

		if normalized == skillDefinitionFile {
			hasSkillMD = true
			skillMD = file.Content
		}
		sum := sha256Hex(file.Content)
		out = append(out, SkillFile{Path: normalized, Content: file.Content, SHA256: sum, Size: size})
		hashes = append(hashes, core.FileHash{Path: normalized, SHA256: sum, Size: size})
	}
	if !hasSkillMD {
		return SkillCandidate{}, fmt.Errorf("技能 %s 缺少根级 %s", name, skillDefinitionFile)
	}
	if _, _, err := skillCardFields(skillMD, name); err != nil {
		return SkillCandidate{}, fmt.Errorf("解析技能定义 %s: %s",
			skillDefinitionFile, frontmatterErrorSummary(err))
	}

	slices.SortFunc(out, func(a, b SkillFile) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(hashes, func(a, b core.FileHash) int { return strings.Compare(a.Path, b.Path) })
	contentHash, err := hashFileList(hashes)
	if err != nil {
		return SkillCandidate{}, err
	}
	manifest, err := json.Marshal(hashes)
	if err != nil {
		return SkillCandidate{}, fmt.Errorf("序列化技能文件清单: %w", err)
	}
	return SkillCandidate{
		Name:         name,
		Files:        out,
		ContentHash:  contentHash,
		ManifestJSON: string(manifest),
	}, nil
}

// normalizeSkillFilePath 复用 Agent 写面的路径判据（约束 41：相对路径、逐段合法、拒绝
// 盘符 / 绝对路径 / 控制字符 / `.staging` / `.backup` 后缀），再叠加约束 24 的隐藏段
// 规则：任一段以 `.` 开头（隐藏目录 / 隐藏文件）一律拒绝。`\` 与 `/` 都先归一为 `/`。
// normalizeSkillFilePath adds the hidden-segment rule on top of the shared agent
// path rules and normalizes both separators to '/'.
func normalizeSkillFilePath(raw string) (string, error) {
	normalized, err := normalizeAgentFilePath(raw)
	if err != nil {
		return "", err
	}
	for _, segment := range strings.Split(normalized, "/") {
		if strings.HasPrefix(segment, ".") {
			return "", fmt.Errorf("路径不得包含以点开头的段（隐藏目录 / 文件）：%q", raw)
		}
	}
	return normalized, nil
}

// writeSkillStage 在 staging 目录写入候选的完整文件集：父目录按需创建，文件以
// `filePerm` 落盘；任何失败原样返回，由调用方决定清理与回滚。
// writeSkillStage writes the candidate file set into the staging tree.
func writeSkillStage(base *os.Root, relStaging string, files []SkillFile) error {
	if err := base.MkdirAll(filepath.FromSlash(relStaging), dirPerm); err != nil {
		return fmt.Errorf("创建 staging 目录 %s: %w", relStaging, err)
	}
	for i, file := range files {
		target := path.Join(relStaging, file.Path)
		if err := base.MkdirAll(filepath.FromSlash(path.Dir(target)), dirPerm); err != nil {
			return fmt.Errorf("创建候选目录 %s: %w", path.Dir(target), err)
		}
		if err := base.WriteFile(filepath.FromSlash(target), file.Content, filePerm); err != nil {
			return fmt.Errorf("写入候选的第 %d 个文件 %s: %w", i+1, file.Path, err)
		}
	}
	return nil
}

// makeSkillVersionReadOnly 把刚冻结的版本目录置为只读（约束 25）：文件先置 0o444，
// 目录再自底向上置 0o555——顺序反了会先失去对子项的写权限。
// makeSkillVersionReadOnly makes a freshly materialized version read-only.
func makeSkillVersionReadOnly(root, relDir string) (err error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	dirs := make([]string, 0, 4)
	walkErr := fs.WalkDir(base.FS(), relDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		return base.Chmod(filepath.FromSlash(p), skillVersionFilePerm)
	})
	if walkErr != nil {
		return fmt.Errorf("设置技能版本目录只读（文件）%s: %w", relDir, walkErr)
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if chmodErr := base.Chmod(filepath.FromSlash(dirs[i]), skillVersionDirPerm); chmodErr != nil {
			return fmt.Errorf("设置技能版本目录只读（目录）%s: %w", dirs[i], chmodErr)
		}
	}
	return nil
}

// importRejected 把边界校验失败归入 `core.ErrImportRejected`（web 映射 400
// IMPORT_REJECTED）；文案原样保留，便于调用方点名违规路径与上限。
// importRejected classifies a boundary failure as ErrImportRejected.
func importRejected(err error) error {
	return &core.AdminError{Kind: core.ErrImportRejected, Message: err.Error()}
}

// skillWriteFailed 记一条 ERROR（技能工作区写入失败的可观测性面）并原样返回错误。
// 键为固定三件 skill / reason / detail，与 Agent 写面的 writeFailed 同形。
// skillWriteFailed logs the failure with the fixed keys and returns it unchanged.
func skillWriteFailed(name string, err error) error {
	slog.Default().Error("技能工作区写入失败",
		"skill", name, "reason", "workspace write failed", "detail", err.Error())
	return err
}
