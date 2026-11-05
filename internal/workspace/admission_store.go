package workspace

// 本文件是技能学习准入的**候选冻结面**（颗粒度文档 US-21 §3.1.3）：把一次已校验的
// 技能候选连同来源声明写成不可变目录
// `.basalt/admissions/candidates/<candidate_id>/{manifest.json,source.json,files/...}`，
// 并提供只读回读、目录存在性判定与补偿删除。它**不取实例锁、不开 DB、不写管理操作行**
// ——审计预写与补偿时序由 app 层编排（§2.3.2）。
//
// 三条边界在这里落地：
//  1. 读取来源目录只接受普通文件：根与每个条目都不跟随软链接，绝对路径 / `..` /
//     隐藏段 / 非 UTF-8 / NUL / 超限一律拒绝并归入 `core.ErrImportRejected`；
//     上限复用技能导入的既有判据（200 文件 / 单文件 256 KiB / 总计 2 MiB）。
//  2. 写入是「staging 构建 → rename 就位 → 置只读」：候选目录在 staging 前缀下
//     完整落盘后才一次 rename 到正式路径；`.staging` / `.backup` 残留与已存在的
//     正式目录一律拒绝，不覆盖、不清理（残留裁决属于运维 / 恢复路径）。就位后
//     文件 0o444、目录 0o555，之后任何改动都会在回读时表现为哈希不符。
//  3. 回读**每次重算**：重新走 `ValidateSkillDraft` 复算内容哈希，并与磁盘上的
//     候选快照逐字比对；不一致返回 `core.ErrAdmissionCandidateMismatch`，绝不
//     静默采用被改动的字节。
//
// This file is the candidate-freezing surface of skill-learning admission: it writes
// an already-validated skill candidate plus its source declaration into an immutable
// workspace directory and offers read-back, existence checks and compensating removal.
// It takes no instance lock, opens no DB and writes no operation row. Reads recompute
// the candidate hash on every access; writes stage, rename into place, then go
// read-only. Residues are refused rather than resolved here.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// AdmissionsDirName 是准入目录名（工作区根部，与 skills/、versions/ 并列）；
	// 它**不在**任何 Agent 读 / 写白名单里（C-12）。
	// AdmissionsDirName is the admission directory under the workspace root; it is
	// deliberately absent from every Agent read/write whitelist.
	AdmissionsDirName = "admissions"

	// 候选目录的内部布局：candidates/<candidate_id>/{manifest.json,source.json,files/}。
	// Internal layout of one frozen candidate directory.
	admissionCandidatesDirName = "candidates"
	admissionManifestFileName  = "manifest.json"
	admissionSourceFileName    = "source.json"
	admissionFilesDirName      = "files"

	// admissionAuxFileMaxBytes 是 manifest.json / source.json / --source-file /
	// --decision-file 的单文件上限（64 KiB）：这些是声明与清单，不是技能正文。
	// 64 KiB cap for the manifest, source declaration and CLI input files.
	admissionAuxFileMaxBytes = 64 * 1024
)

// admissionCandidateIDPrefix 是候选标识的固定前缀（与 DDL 的 CHECK 同口径）。
// The fixed candidate id prefix, matching the DDL CHECK.
const admissionCandidateIDPrefix = "adm_"

// admissionManifestDocument 是 `manifest.json` 的冻结形态：候选标识、技能名、内容哈希
// 与文件清单（清单字节与候选行的 `manifest_json` 完全相同）。`source.json` 单独存放
// 原始来源声明字节，回读时按 SHA-256 复算来源哈希。
//
// 单独存技能名而不是从 SKILL.md frontmatter 反推：回读必须能独立重跑
// `ValidateSkillDraft(name, files)`，frontmatter 的声明名可以与目录绑定名不同，
// 不能作为身份来源。
//
// admissionManifestDocument is the frozen shape of manifest.json: candidate id,
// skill name, content hash and the file digest list (byte-identical to the row's
// manifest_json). The skill name is stored rather than derived from the SKILL.md
// frontmatter because read-back must re-run ValidateSkillDraft independently.
type admissionManifestDocument struct {
	CandidateID   string          `json:"candidate_id"`
	SkillName     string          `json:"skill_name"`
	CandidateHash string          `json:"candidate_hash"`
	Files         json.RawMessage `json:"files"`
}

// AdmissionCandidateRelDir 给出候选目录相对工作区根（`.basalt/`）的路径。调用方
// 必须先用 `core.ParseAdmissionID` 校验 candidateID；本函数只做路径拼接。
// AdmissionCandidateRelDir returns the candidate directory relative to the workspace
// root. Callers must validate the id first; this function only joins paths.
func AdmissionCandidateRelDir(candidateID string) string {
	return path.Join(AdmissionsDirName, admissionCandidatesDirName, candidateID)
}

// ReadSkillSourceDir 读取 `basalt admission propose --from-dir` 的来源目录：根与每个
// 条目都不跟随软链接，只收普通文件，相对路径规范化后复用技能导入的三条上限。
// 任何边界违反都归入 `core.ErrImportRejected`（CLI 映射 IMPORT_REJECTED）。
//
// ReadSkillSourceDir reads a propose source directory without following symlinks at
// the root or any entry; boundary violations map to ErrImportRejected.
func ReadSkillSourceDir(dir string) ([]core.AuthorFile, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, importRejected(fmt.Errorf("读取来源目录 %s: %w", dir, err))
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, importRejected(fmt.Errorf("来源目录 %s 是符号链接，拒绝读取", dir))
	}
	if !info.IsDir() {
		return nil, importRejected(fmt.Errorf("来源路径 %s 不是目录（%s），拒绝读取", dir, fileKind(info)))
	}

	base, err := os.OpenRoot(dir)
	if err != nil {
		return nil, importRejected(fmt.Errorf("打开来源目录 %s: %w", dir, err))
	}
	defer func() {
		_ = base.Close()
	}()

	files, err := readAdmissionFileTree(base, ".")
	if err != nil {
		return nil, importRejected(err)
	}
	return files, nil
}

// WriteAdmissionCandidate 把已校验的候选冻结到
// `admissions/candidates/<candidate_id>/`：先复核候选哈希与来源哈希自洽，再在
// `.staging` 前缀下完整落盘，一次 rename 就位后置只读。它不触碰 `skills/` 与
// `versions/`——发布阶段的写入归既有技能发布原语（C-01）。
//
// 已存在的正式目录与 `.staging` / `.backup` 残留一律拒绝：候选 ID 是新铸的，
// 出现同名目录说明上一次写入留下孤儿或人为放置，两者都应交人工裁决；不在这里
// 覆盖、删除或"修复"证据。
//
// WriteAdmissionCandidate freezes a validated candidate via a staging tree, one
// rename into place and a read-only sweep. Existing targets and residues are refused.
func WriteAdmissionCandidate(root string, cand core.AdmissionCandidate, files []core.AuthorFile, sourceJSON []byte) error {
	candidateID, err := core.ParseAdmissionID(admissionCandidateIDPrefix, cand.CandidateID)
	if err != nil {
		return admissionWriteFailed(cand, fmt.Errorf("%w: %v", core.ErrAdmissionInvalidInput, err))
	}

	rebuilt, err := ValidateSkillDraft(cand.SkillName, files)
	if err != nil {
		return admissionWriteFailed(cand, err)
	}
	if rebuilt.ContentHash != cand.CandidateHash || rebuilt.ManifestJSON != cand.ManifestJSON {
		return admissionWriteFailed(cand, fmt.Errorf(
			"%w: 候选 %s 的哈希与文件清单不一致，拒绝冻结", core.ErrAdmissionCandidateMismatch, candidateID))
	}
	if string(sourceJSON) != cand.SourceJSON || core.AdmissionSHA256Hex(sourceJSON) != cand.SourceHash {
		return admissionWriteFailed(cand, fmt.Errorf(
			"%w: 候选 %s 的来源声明与来源哈希不一致，拒绝冻结",
			core.ErrAdmissionCandidateMismatch, candidateID))
	}

	manifestJSON, err := json.Marshal(admissionManifestDocument{
		CandidateID:   candidateID,
		SkillName:     cand.SkillName,
		CandidateHash: rebuilt.ContentHash,
		Files:         json.RawMessage(rebuilt.ManifestJSON),
	})
	if err != nil {
		return admissionWriteFailed(cand, fmt.Errorf("序列化候选 %s 的清单: %w", candidateID, err))
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return admissionWriteFailed(cand, fmt.Errorf("打开工作区目录 %s: %w", root, err))
	}
	closeRoot := func() error {
		if closeErr := base.Close(); closeErr != nil {
			return fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr)
		}
		return nil
	}

	relDir := AdmissionCandidateRelDir(candidateID)
	relStaging := relDir + stagingSuffix
	relBackup := relDir + backupSuffix

	// 残留哨兵与正式目录检查都先于任何写入：写了一半才发现拒绝，等于制造新残留。
	for _, rel := range []string{relStaging, relBackup, relDir} {
		_, statErr := base.Lstat(filepath.FromSlash(rel))
		switch {
		case statErr == nil:
			closeErr := closeRoot()
			return admissionWriteFailed(cand, errors.Join(fmt.Errorf(
				"候选路径 %s 已存在，拒绝覆盖（确认无价值后手工删除）", rel), closeErr))
		case !errors.Is(statErr, fs.ErrNotExist):
			closeErr := closeRoot()
			return admissionWriteFailed(cand, errors.Join(fmt.Errorf("检查候选路径 %s: %w", rel, statErr), closeErr))
		}
	}

	if err := base.MkdirAll(filepath.FromSlash(path.Dir(relStaging)), dirPerm); err != nil {
		closeErr := closeRoot()
		return admissionWriteFailed(cand, errors.Join(fmt.Errorf(
			"创建候选父目录 %s: %w", path.Dir(relStaging), err), closeErr))
	}

	writeErr := writeAdmissionStage(base, relStaging, manifestJSON, sourceJSON, rebuilt.Files)
	if writeErr == nil {
		writeErr = base.Rename(filepath.FromSlash(relStaging), filepath.FromSlash(relDir))
	}
	if writeErr != nil {
		// staging 未就位（或 rename 失败）时清掉自己的临时树；正式目录此时不存在，
		// 没有需要保留的旧副本。清理失败如实报告，交由人工处置。
		cleanupErr := base.RemoveAll(filepath.FromSlash(relStaging))
		if cleanupErr != nil && !errors.Is(cleanupErr, fs.ErrNotExist) {
			writeErr = errors.Join(writeErr,
				fmt.Errorf("清理候选 staging 目录 %s 失败，请手工删除: %w", relStaging, cleanupErr))
		}
		closeErr := closeRoot()
		return admissionWriteFailed(cand, errors.Join(writeErr, closeErr))
	}

	// 就位后立即置只读：候选状态从这一刻起只能靠哈希不符被发现改动，而不是被静默覆盖。
	// 复用技能版本目录的只读实现（同一套 0o444 / 0o555 语义，不另写一份）。
	if err := makeSkillVersionReadOnly(root, relDir); err != nil {
		closeErr := closeRoot()
		return admissionWriteFailed(cand, errors.Join(err, closeErr))
	}
	if err := closeRoot(); err != nil {
		return admissionWriteFailed(cand, err)
	}
	return nil
}

// writeAdmissionStage 在 staging 目录写入候选快照：manifest.json、source.json 与
// files/ 下的全部候选文件。文件路径已由 `ValidateSkillDraft` 规范化，这里再走一次
// 路径判据是纵深防御，不替代校验闸门。
// writeAdmissionStage writes the frozen snapshot into the staging tree.
func writeAdmissionStage(base *os.Root, relStaging string, manifestJSON, sourceJSON []byte, files []SkillFile) error {
	if err := base.MkdirAll(filepath.FromSlash(relStaging), dirPerm); err != nil {
		return fmt.Errorf("创建候选 staging 目录 %s: %w", relStaging, err)
	}
	if err := base.WriteFile(filepath.FromSlash(path.Join(relStaging, admissionManifestFileName)),
		manifestJSON, filePerm); err != nil {
		return fmt.Errorf("写入候选清单 %s: %w", admissionManifestFileName, err)
	}
	if err := base.WriteFile(filepath.FromSlash(path.Join(relStaging, admissionSourceFileName)),
		sourceJSON, filePerm); err != nil {
		return fmt.Errorf("写入来源声明 %s: %w", admissionSourceFileName, err)
	}

	relFiles := path.Join(relStaging, admissionFilesDirName)
	for _, file := range files {
		normalized, err := normalizeSkillFilePath(file.Path)
		if err != nil {
			return fmt.Errorf("候选文件路径非法: %w", err)
		}
		target := path.Join(relFiles, normalized)
		if err := base.MkdirAll(filepath.FromSlash(path.Dir(target)), dirPerm); err != nil {
			return fmt.Errorf("创建候选文件目录 %s: %w", path.Dir(target), err)
		}
		if err := base.WriteFile(filepath.FromSlash(target), file.Content, filePerm); err != nil {
			return fmt.Errorf("写入候选文件 %s: %w", normalized, err)
		}
	}
	return nil
}

// ReadAdmissionCandidate 从冻结目录回读候选：`manifest.json` 的候选 / 技能 / 哈希
// 绑定必须与目录名一致，`files/` 重新走 `ValidateSkillDraft` 复算内容哈希，任何
// 不一致（内容被改、清单被换、出现软链接 / 特殊文件）都返回
// `core.ErrAdmissionCandidateMismatch`。目录、清单或来源文件缺失则返回
// `core.ErrAdmissionCandidateMissing`——缺失与篡改分开，调用方才能给出正确处置。
//
// ReadAdmissionCandidate reloads a frozen candidate, re-runs ValidateSkillDraft and
// separates missing artifacts from tampered ones.
func ReadAdmissionCandidate(root, candidateID string) (core.AdmissionCandidate, []core.AuthorFile, []byte, error) {
	candidateID, err := core.ParseAdmissionID(admissionCandidateIDPrefix, candidateID)
	if err != nil {
		return core.AdmissionCandidate{}, nil, nil,
			fmt.Errorf("%w: %v", core.ErrAdmissionInvalidInput, err)
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return core.AdmissionCandidate{}, nil, nil,
			fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := AdmissionCandidateRelDir(candidateID)
	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选目录 %s 不存在", core.ErrAdmissionCandidateMissing, relDir)
	case statErr != nil:
		return core.AdmissionCandidate{}, nil, nil,
			fmt.Errorf("检查候选目录 %s: %w", relDir, statErr)
	case info.Mode()&fs.ModeSymlink != 0:
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选目录 %s 是符号链接", core.ErrAdmissionCandidateMismatch, relDir)
	case !info.IsDir():
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选路径 %s 不是目录（%s）", core.ErrAdmissionCandidateMismatch, relDir, fileKind(info))
	}

	manifestRaw, err := readAdmissionRootFile(base, path.Join(relDir, admissionManifestFileName),
		admissionAuxFileMaxBytes)
	if err != nil {
		return core.AdmissionCandidate{}, nil, nil, admissionReadArtifactError(
			err, path.Join(relDir, admissionManifestFileName))
	}
	var doc admissionManifestDocument
	if err := decodeAdmissionManifestDocument(manifestRaw, &doc); err != nil {
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选清单 %s 不可解析: %v",
			core.ErrAdmissionCandidateMismatch, admissionManifestFileName, err)
	}
	if doc.CandidateID != candidateID {
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选清单标识 %q 与目录 %q 不一致",
			core.ErrAdmissionCandidateMismatch, doc.CandidateID, candidateID)
	}

	sourceJSON, err := readAdmissionRootFile(base, path.Join(relDir, admissionSourceFileName),
		admissionAuxFileMaxBytes)
	if err != nil {
		return core.AdmissionCandidate{}, nil, nil, admissionReadArtifactError(
			err, path.Join(relDir, admissionSourceFileName))
	}
	if !utf8.Valid(sourceJSON) || bytes.IndexByte(sourceJSON, 0) >= 0 {
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选来源声明不是合法 UTF-8 文本", core.ErrAdmissionCandidateMismatch)
	}

	files, err := readAdmissionFileTree(base, path.Join(relDir, admissionFilesDirName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
				"%w: 候选文件目录 %s 不存在",
				core.ErrAdmissionCandidateMissing, path.Join(relDir, admissionFilesDirName))
		}
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: %v", core.ErrAdmissionCandidateMismatch, err)
	}

	rebuilt, err := ValidateSkillDraft(doc.SkillName, files)
	if err != nil {
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选文件集无法重新校验: %v", core.ErrAdmissionCandidateMismatch, err)
	}
	if rebuilt.ContentHash != doc.CandidateHash {
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选 %s 的内容哈希与冻结值不一致", core.ErrAdmissionCandidateMismatch, candidateID)
	}
	if rebuilt.ManifestJSON != string(doc.Files) {
		return core.AdmissionCandidate{}, nil, nil, fmt.Errorf(
			"%w: 候选 %s 的清单与磁盘文件不一致", core.ErrAdmissionCandidateMismatch, candidateID)
	}

	return core.AdmissionCandidate{
		CandidateID:   candidateID,
		SkillName:     doc.SkillName,
		CandidateHash: rebuilt.ContentHash,
		ManifestJSON:  rebuilt.ManifestJSON,
		SourceHash:    core.AdmissionSHA256Hex(sourceJSON),
		SourceJSON:    string(sourceJSON),
	}, files, sourceJSON, nil
}

// decodeAdmissionManifestDocument 严格解析候选清单文档：未知键、多余尾随内容与
// 非法字段一律拒绝；`files` 必须是规范清单（交给 `core.ParseAdmissionManifest`
// 判升序与取值），不在本层放宽。
// decodeAdmissionManifestDocument strictly decodes manifest.json.
func decodeAdmissionManifestDocument(raw []byte, doc *admissionManifestDocument) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(doc); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("清单文档存在多余内容")
	}
	if doc.CandidateID == "" || doc.SkillName == "" || doc.CandidateHash == "" || len(doc.Files) == 0 {
		return errors.New("清单文档缺少候选标识 / 技能名 / 哈希 / 文件清单")
	}
	if _, err := core.ParseAdmissionManifest(string(doc.Files)); err != nil {
		return err
	}
	return nil
}

// readAdmissionRootFile 在工作区根内读取一个受长度上限约束的普通文件；软链接与
// 非普通文件一律拒绝。`fs.ErrNotExist` 原样保留，供调用方区分缺失与篡改。
// readAdmissionRootFile reads one bounded regular file inside an os.Root.
func readAdmissionRootFile(base *os.Root, rel string, limit int64) ([]byte, error) {
	info, err := base.Lstat(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s 是符号链接，拒绝读取", rel)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件（%s），拒绝读取", rel, fileKind(info))
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s 大小 %d 字节超过上限 %d 字节", rel, info.Size(), limit)
	}
	raw, err := base.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s 大小 %d 字节超过上限 %d 字节", rel, len(raw), limit)
	}
	return raw, nil
}

// readAdmissionFileTree 读取一棵候选文件树：只收普通文件，路径必须通过
// `normalizeSkillFilePath`（相对路径、无 `..`、无隐藏段），并复用技能导入的
// 200 / 256 KiB / 2 MiB 三条上限。返回顺序即 WalkDir 的字典序。
// readAdmissionFileTree reads a candidate file tree with the shared import limits.
func readAdmissionFileTree(base *os.Root, relRoot string) ([]core.AuthorFile, error) {
	files := make([]core.AuthorFile, 0, 4)
	var total int64

	walkErr := fs.WalkDir(base.FS(), relRoot, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel := p
		if relRoot != "." {
			if p == relRoot {
				return nil
			}
			rel = strings.TrimPrefix(p, relRoot+"/")
		} else if p == "." {
			return nil
		}
		normalized, err := normalizeSkillFilePath(rel)
		if err != nil {
			return err
		}

		info, err := base.Lstat(filepath.FromSlash(p))
		if err != nil {
			return fmt.Errorf("检查 %s: %w", rel, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("路径 %s 是符号链接，拒绝读取", rel)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("路径 %s 不是普通文件（%s），拒绝读取", rel, fileKind(info))
		}
		if len(files) >= skillImportMaxFiles {
			return fmt.Errorf("文件数量超过上限 %d", skillImportMaxFiles)
		}
		if info.Size() > skillImportMaxFileBytes {
			return fmt.Errorf("文件 %s 大小 %d 字节超过单文件上限 %d 字节",
				rel, info.Size(), skillImportMaxFileBytes)
		}
		total += info.Size()
		if total > skillImportMaxTotalBytes {
			return fmt.Errorf("文件总量超过上限 %d 字节", skillImportMaxTotalBytes)
		}

		content, err := base.ReadFile(filepath.FromSlash(p))
		if err != nil {
			return fmt.Errorf("读取 %s: %w", rel, err)
		}
		if !utf8.Valid(content) {
			return fmt.Errorf("文件 %s 不是合法的 UTF-8 文本", rel)
		}
		if bytes.IndexByte(content, 0) >= 0 {
			return fmt.Errorf("文件 %s 含 NUL 字节", rel)
		}
		files = append(files, core.AuthorFile{Path: normalized, Content: string(content)})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return files, nil
}

// admissionReadArtifactError 把清单 / 来源文件的读取错误分成「缺失」与「不可信」两类：
// 文件不存在归 missing，软链接 / 非普通文件 / 超限归 mismatch。
// admissionReadArtifactError splits missing artifacts from untrusted ones.
func admissionReadArtifactError(err error, rel string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s 不存在", core.ErrAdmissionCandidateMissing, rel)
	}
	return fmt.Errorf("%w: 读取 %s: %v", core.ErrAdmissionCandidateMismatch, rel, err)
}

// AdmissionCandidateDirExists 报告候选目录是否已存在。它只判「是目录且不是符号
// 链接」；权限错误等异常记为 WARN 并返回 false——签名没有错误位，调用方（恢复路径）
// 把"看不到"当作"不存在"处理，但仍留下日志线索。
// AdmissionCandidateDirExists reports whether the candidate directory exists.
func AdmissionCandidateDirExists(root, candidateID string) bool {
	candidateID, err := core.ParseAdmissionID(admissionCandidateIDPrefix, candidateID)
	if err != nil {
		return false
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		slog.Default().Warn("检查准入候选目录时无法打开工作区",
			"root", root, "candidate", candidateID, "detail", err.Error())
		return false
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := AdmissionCandidateRelDir(candidateID)
	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	if errors.Is(statErr, fs.ErrNotExist) {
		return false
	}
	if statErr != nil {
		slog.Default().Warn("检查准入候选目录失败",
			"candidate", candidateID, "path", relDir, "detail", statErr.Error())
		return false
	}
	return info.IsDir() && info.Mode()&fs.ModeSymlink == 0
}

// RemoveAdmissionCandidate 删除候选目录（仅用于提议阶段的数据库失败补偿）。
// 目录已不存在按幂等成功处理；符号链接 / 非目录拒绝删除。冻结目录是只读的，
// 删除前先把整棵树改回可写，否则 Unix 上目录 0o555 会让 unlink 失败。
// RemoveAdmissionCandidate removes a candidate directory for propose compensation.
func RemoveAdmissionCandidate(root, candidateID string) error {
	candidateID, err := core.ParseAdmissionID(admissionCandidateIDPrefix, candidateID)
	if err != nil {
		return fmt.Errorf("%w: %v", core.ErrAdmissionInvalidInput, err)
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := AdmissionCandidateRelDir(candidateID)
	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	if errors.Is(statErr, fs.ErrNotExist) {
		return nil
	}
	if statErr != nil {
		return fmt.Errorf("检查候选目录 %s: %w", relDir, statErr)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("候选路径 %s 不是普通目录（%s），拒绝删除", relDir, fileKind(info))
	}

	if err := makeAdmissionTreeWritable(base, relDir); err != nil {
		return fmt.Errorf("恢复候选目录 %s 的可写权限: %w", relDir, err)
	}
	if err := base.RemoveAll(filepath.FromSlash(relDir)); err != nil {
		return fmt.Errorf("删除候选目录 %s: %w", relDir, err)
	}
	return nil
}

// makeAdmissionTreeWritable 把候选目录树改回可写：目录 0o750、文件 0o644。
// WalkDir 先父后子，父目录先恢复写权限才能继续处理子项。软链接直接跳过：既不该
// 改它的权限（改的是链接本身的属性），也不跟随它——`Root.RemoveAll` 只删链接，
// 不碰链接指向的内容，因此被注入软链接的候选目录同样能被补偿删除。
// makeAdmissionTreeWritable restores write permission over the frozen tree and
// skips symlinks (removal deletes the link itself, never its target).
func makeAdmissionTreeWritable(base *os.Root, relDir string) error {
	return fs.WalkDir(base.FS(), relDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := base.Lstat(filepath.FromSlash(p))
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			return base.Chmod(filepath.FromSlash(p), dirPerm)
		}
		return base.Chmod(filepath.FromSlash(p), filePerm)
	})
}

// ReadAdmissionSourceFile 读取 `--source-file`：≤64 KiB、普通文件、不跟随软链接。
// 边界违反归入 `core.ErrImportRejected`（来源材料属于导入边界）。
// ReadAdmissionSourceFile reads --source-file under the import boundary.
func ReadAdmissionSourceFile(filePath string) ([]byte, error) {
	raw, err := readAdmissionPlainFile(filePath, admissionAuxFileMaxBytes)
	if err != nil {
		return nil, importRejected(err)
	}
	return raw, nil
}

// ReadAdmissionDecisionFile 读取 `--decision-file`：≤64 KiB、普通文件、不跟随
// 软链接。边界违反归入 `core.ErrAdmissionInvalidInput`（决策文件是命令输入，不是
// 候选来源）。
// ReadAdmissionDecisionFile reads --decision-file as a bounded regular file.
func ReadAdmissionDecisionFile(filePath string) ([]byte, error) {
	raw, err := readAdmissionPlainFile(filePath, admissionAuxFileMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", core.ErrAdmissionInvalidInput, err)
	}
	return raw, nil
}

// readAdmissionPlainFile 按 Lstat → Open → 复检的顺序读取普通文件，拒绝软链接、
// 特殊文件与超过上限的输入；读取上限 +1 字节用于判"超限"，不把超限文件整个读入。
// readAdmissionPlainFile reads a bounded regular file and rejects symlinks.
func readAdmissionPlainFile(filePath string, limit int64) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", filePath, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s 是符号链接，拒绝读取", filePath)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件（%s），拒绝读取", filePath, fileKind(info))
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s 大小 %d 字节超过上限 %d 字节", filePath, info.Size(), limit)
	}

	// 文件路径来自命令行（部署者显式指定要导入 / 批准的对象），本身不构成越权；
	// 但仍用 `os.Root` 把打开动作限定在父目录内：任何逃出父目录的符号链接 / 相对
	// 路径都会被拒绝，同时收紧 Lstat 与 Open 之间的替换窗口（G304 的"任意变量路径"
	// 语义由此不再成立，不需要豁免注记）。
	// Open is scoped to the parent directory: the path comes from the operator, and the
	// root rejects anything escaping that directory, narrowing the check-then-open window.
	root, err := os.OpenRoot(filepath.Dir(filePath))
	if err != nil {
		return nil, fmt.Errorf("打开 %s 所在目录: %w", filePath, err)
	}
	defer func() {
		_ = root.Close()
	}()
	handle, err := root.Open(filepath.Base(filePath))
	if err != nil {
		return nil, fmt.Errorf("打开 %s: %w", filePath, err)
	}
	defer func() {
		_ = handle.Close()
	}()
	opened, err := handle.Stat()
	if err != nil {
		return nil, fmt.Errorf("检查已打开的 %s: %w", filePath, err)
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件（%s），拒绝读取", filePath, fileKind(opened))
	}

	raw, err := io.ReadAll(io.LimitReader(handle, limit+1))
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", filePath, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s 大小超过上限 %d 字节", filePath, limit)
	}
	return raw, nil
}

// admissionWriteFailed 记一条 ERROR（准入冻结失败的可观测性面：候选 / 技能 / 原因）
// 并原样返回错误；日志只带路径与错误文案，不打印候选正文或来源正文。
// admissionWriteFailed logs the failure and returns the error unchanged.
func admissionWriteFailed(cand core.AdmissionCandidate, err error) error {
	slog.Default().Error("准入候选冻结失败",
		"candidate", cand.CandidateID, "skill", cand.SkillName,
		"reason", "admission write failed", "detail", err.Error())
	return err
}
