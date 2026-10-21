package workspace

// 本文件是技能**不可变发布版本**的落地（颗粒度文档 §3.1.15 / 约束 31、32）：工作副本
// skills/<name>/ 只用于编辑与预览，Run 准入冻结的版本一律来自
// versions/skills/<name>/<no>/（逻辑路径 .basalt/versions/skills/<name>/<no>）。
// 四个哈希按固定字节定义，**不做**换行归一、不 trim、不重新序列化 YAML——哈希必须能在
// 不同机器上复算出同一结果，规范化会掩盖 CRLF 与编码差异。
// This file implements immutable skill versions. Work copies under skills/<name>/ are
// for editing and preview only; runs freeze versions under
// versions/skills/<name>/<no>/. All four hashes use the fixed byte definitions with
// no newline normalization, trimming, or YAML re-serialization.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// versionsDirName 是工作区根（.basalt/）下的不可变版本目录名。
	// versionsDirName is the immutable version directory under the workspace root.
	versionsDirName = "versions"

	// skillVersionsPrefix 是技能发布目录相对工作区根（.basalt/）的字面前缀
	//（约束 31：storage_path 的实现取 .basalt/versions/skills/<name>/<version_no>）。
	// skillVersionsPrefix is the contract literal of a published skill version dir.
	skillVersionsPrefix = ".basalt/versions/skills"
)

// SkillVersion 是一个技能版本的全部可核对事实：发布目录、四个固定哈希与完整文件清单。
// 工作副本（HashWorkCopy）没有发布目录与版本号：VersionNo=0、Dir 为空。
// SkillVersion carries every checkable fact of one version: the directory, the four
// fixed hashes and the full file list. Work copies have VersionNo 0 and an empty Dir.
type SkillVersion struct {
	Name        string
	VersionNo   int
	Dir         string // 工作区相对路径：.basalt/versions/skills/<name>/<no>；工作副本为空
	ContentHash string
	SkillMDHash string
	L1Hash      string
	L2Hash      string
	L3Hash      string
	Files       []core.FileHash
}

// SkillLibrary 是技能不可变版本的读写面；root 是工作区根（.basalt/ 本身），
// 与 LifecycleStore / AgentLoader 的根口径一致。
// SkillLibrary is the read/write surface for skill versions; root is the workspace
// root (.basalt itself), matching every other workspace store.
type SkillLibrary struct {
	root string
}

// NewSkillLibrary 组装技能版本库（无内部状态，可并发使用；写路径由启动期单线程调用）。
// NewSkillLibrary returns a stateless library safe for concurrent readers.
func NewSkillLibrary(root string) *SkillLibrary {
	return &SkillLibrary{root: root}
}

// Names 枚举技能库里的技能目录名（升序，只保留合法单目录名、跳过非目录条目与符号
// 链接）。启动基线据此决定哪些技能需要建立 version 1。**不**按 SKILL.md 是否存在
// 过滤——缺卡片的目录由调用方的 HashWorkCopy 包装 fs.ErrNotExist 判为"不是技能实体"
// （注册不变量，约束 36），技能实体已损坏（不可读、非 UTF-8）则必须报错而不是跳过。
// 技能库整体缺失 → 空清单（"没有技能库"与"工作区没有技能"是同一状态）。
// Names lists skill directory names ascending; a missing library yields no names.
// A directory without SKILL.md is not filtered here — HashWorkCopy reports it as
// fs.ErrNotExist, which the baseline maps to "not a skill entity".
func (l *SkillLibrary) Names() (names []string, err error) {
	lib, err := openSkillsRoot(l.root)
	if err != nil {
		return nil, err
	}
	if lib == nil {
		return nil, nil
	}
	defer func() {
		if closeErr := lib.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭技能库目录 %s: %w", skillsDirName, closeErr))
		}
	}()

	entries, err := fs.ReadDir(lib.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("读取技能库目录 %s: %w", skillsDirName, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validSkillName(entry.Name()) {
			continue
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names, nil
}

// HashWorkCopy 只读计算工作副本 skills/<name>/ 的版本事实，不写任何文件；基线按它
// 建立 version 1，watcher 按它校验工作副本是否变化。工作副本的 L1 用工作副本卡片
// 路径（.basalt/skills/<name>/SKILL.md，D-02 的预览 / 展示口径），发布版本的 L1 用
// 发布真路径——两者在各自语境里都是「模型会看到的那一行」。
// HashWorkCopy hashes the work copy without writing anything; the work copy's L1 uses
// the work-copy card path, while published versions use their real path.
func (l *SkillLibrary) HashWorkCopy(name string) (version SkillVersion, err error) {
	if !validSkillName(name) {
		return SkillVersion{}, fmt.Errorf("技能名不是单个目录名，拒绝读取技能库外的定义：%q", name)
	}

	base, err := os.OpenRoot(l.root)
	if err != nil {
		return SkillVersion{}, fmt.Errorf("打开工作区目录 %s: %w", l.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", l.root, closeErr))
		}
	}()

	return hashSkillDir(base, name, 0, skillWorkDir(name), "", skillCardPath(name))
}

// Materialize 把工作副本冻结到 versions/skills/<name>/<no>/：已存在且哈希一致 → 直接
// 复用；不一致 → 报错且**不覆盖**（已发布目录是不可变事实）；写入先逐文件校验读取字节
// 与清单哈希相等，任何失败都清理本次创建的部分目录，避免半截快照挡住重试。
// Materialize freezes the work copy into an immutable version dir; an existing dir is
// reused only when hashes match, and a partially copied dir is removed on failure.
func (l *SkillLibrary) Materialize(ctx context.Context, name string, versionNo int, contentHash string) (version SkillVersion, err error) {
	if err := ctx.Err(); err != nil {
		return SkillVersion{}, err
	}
	if !validSkillName(name) {
		return SkillVersion{}, fmt.Errorf("技能名不是单个目录名，拒绝物化：%q", name)
	}
	if versionNo < 1 {
		return SkillVersion{}, fmt.Errorf("技能 %s 的版本号必须 ≥ 1，当前 %d", name, versionNo)
	}
	if !isSHA256Hex(contentHash) {
		return SkillVersion{}, fmt.Errorf("技能 %s 的 content_hash %q 不是 64 位小写十六进制", name, contentHash)
	}

	work, err := l.HashWorkCopy(name)
	if err != nil {
		return SkillVersion{}, err
	}
	if work.ContentHash != contentHash {
		return SkillVersion{}, fmt.Errorf("技能 %s 的工作副本 content_hash 为 %s，与期望的 %s 不一致，拒绝冻结",
			name, work.ContentHash, contentHash)
	}

	base, err := os.OpenRoot(l.root)
	if err != nil {
		return SkillVersion{}, fmt.Errorf("打开工作区目录 %s: %w", l.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", l.root, closeErr))
		}
	}()

	logicalDir := skillVersionLogicalDir(name, versionNo)
	relDir := skillVersionRelDir(name, versionNo)
	info, statErr := base.Stat(filepath.FromSlash(relDir))
	switch {
	case statErr == nil:
		if !info.IsDir() {
			return SkillVersion{}, fmt.Errorf("技能 %s 的发布路径 %s 已存在且不是目录", name, logicalDir)
		}
		published, loadErr := l.LoadPublished(ctx, name, versionNo, logicalDir)
		if loadErr != nil {
			return SkillVersion{}, fmt.Errorf("技能 %s 的发布目录 %s 已存在但不可用，拒绝覆盖: %w", name, logicalDir, loadErr)
		}
		if published.ContentHash != contentHash {
			return SkillVersion{}, fmt.Errorf("技能 %s 的发布目录 %s 已存在且 content_hash 为 %s，与期望的 %s 不一致，拒绝覆盖",
				name, logicalDir, published.ContentHash, contentHash)
		}
		return published, nil
	case errors.Is(statErr, fs.ErrNotExist):
		// 正常路径：继续冻结。
	default:
		return SkillVersion{}, fmt.Errorf("检查技能 %s 的发布路径 %s: %w", name, logicalDir, statErr)
	}

	if err := copySkillVersion(base, skillWorkDir(name), relDir, work.Files); err != nil {
		return SkillVersion{}, err
	}
	return l.LoadPublished(ctx, name, versionNo, logicalDir)
}

// LoadPublished 按 storage_path 读发布目录并重算全部哈希；返回结果由调用方与
// resource_versions.manifest_json / content_hash 逐项比对（签名不含期望清单，
// 比对归属调用方——不一致即注册不变量被破坏）。目录路径只接受契约字面量形态。
// LoadPublished recomputes every hash from the published dir; the caller compares the
// result with manifest_json / content_hash, since the signature carries no expectation.
func (l *SkillLibrary) LoadPublished(ctx context.Context, name string, versionNo int, dir string) (version SkillVersion, err error) {
	if err := ctx.Err(); err != nil {
		return SkillVersion{}, err
	}
	if !validSkillName(name) {
		return SkillVersion{}, fmt.Errorf("技能名不是单个目录名，拒绝读取发布目录：%q", name)
	}
	if versionNo < 1 {
		return SkillVersion{}, fmt.Errorf("技能 %s 的版本号必须 ≥ 1，当前 %d", name, versionNo)
	}

	expected := skillVersionLogicalDir(name, versionNo)
	logicalDir := strings.TrimSpace(dir)
	if logicalDir == "" {
		logicalDir = expected
	}
	if path.Clean(logicalDir) != expected {
		return SkillVersion{}, fmt.Errorf("技能 %s 的发布目录必须是契约字面量 %s，当前 %q", name, expected, dir)
	}

	base, err := os.OpenRoot(l.root)
	if err != nil {
		return SkillVersion{}, fmt.Errorf("打开工作区目录 %s: %w", l.root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", l.root, closeErr))
		}
	}()

	return hashSkillDir(base, name, versionNo, skillVersionRelDir(name, versionNo), logicalDir, publishedCardPath(name, versionNo))
}

// CardPath 返回该版本的卡片真路径：发布版本 → 发布目录里的 SKILL.md；工作副本
// （VersionNo=0）→ 工作副本卡片路径。两种都是对应 L1 哈希所覆盖的那一行路径。
// CardPath returns the card path matching the version's L1 hash input.
func (l *SkillLibrary) CardPath(v SkillVersion) string {
	if v.VersionNo < 1 {
		return skillCardPath(v.Name)
	}
	return publishedCardPath(v.Name, v.VersionNo)
}

// skillWorkDir 给出工作副本相对工作区根（.basalt/）的目录：skills/<name>。
// skillWorkDir is the workspace-relative work-copy directory.
func skillWorkDir(name string) string {
	return path.Join(skillsDirName, name)
}

// skillVersionLogicalDir 给出发布目录的契约字面量：.basalt/versions/skills/<name>/<no>。
// skillVersionLogicalDir is the contract literal of a published version directory.
func skillVersionLogicalDir(name string, versionNo int) string {
	return path.Join(skillVersionsPrefix, name, strconv.Itoa(versionNo))
}

// skillVersionRelDir 给出发布目录在文件系统上相对工作区根的形态（去掉 .basalt/ 前缀）。
// skillVersionRelDir strips the .basalt/ prefix for filesystem access.
func skillVersionRelDir(name string, versionNo int) string {
	return path.Join(versionsDirName, skillsDirName, name, strconv.Itoa(versionNo))
}

// publishedCardPath 给出发布版本里 SKILL.md 的真路径（约束 31 的 L1 卡片字面量）。
// publishedCardPath is the L1 card path of a published version.
func publishedCardPath(name string, versionNo int) string {
	return path.Join(skillVersionLogicalDir(name, versionNo), skillDefinitionFile)
}

// hashSkillDir 在已打开的根上计算一个技能目录的版本事实：递归收集普通文件、拒绝
// 非普通文件与非 UTF-8 的 SKILL.md、按 path 升序输出清单，四个哈希按约束 32 的定义。
// hashSkillDir hashes one skill directory: regular files only, UTF-8 SKILL.md required,
// paths ascending, and the four hashes per constraint 32.
func hashSkillDir(base *os.Root, name string, versionNo int, relDir, logicalDir, cardPath string) (SkillVersion, error) {
	files, skillMD, err := scanSkillFiles(base, relDir)
	if err != nil {
		return SkillVersion{}, err
	}
	if skillMD == nil {
		// 包装 fs.ErrNotExist：调用方（基线枚举技能目录）据此区分"不是技能实体"与
		// "技能实体已损坏"——缺 SKILL.md 的目录不建发布版本（注册不变量，约束 36）。
		return SkillVersion{}, fmt.Errorf("技能 %s 缺少 %s（%s）: %w", name, skillDefinitionFile, path.Join(relDir, skillDefinitionFile), fs.ErrNotExist)
	}
	if !utf8.Valid(skillMD) {
		return SkillVersion{}, fmt.Errorf("技能 %s 的 %s 不是合法 UTF-8，拒绝计算哈希", name, skillDefinitionFile)
	}

	cardName, description, err := skillCardFields(skillMD, name)
	if err != nil {
		return SkillVersion{}, fmt.Errorf("解析技能定义 %s: %s", path.Join(relDir, skillDefinitionFile), frontmatterErrorSummary(err))
	}
	l1Hash := sha256Hex([]byte(skillCardLine(cardName, description, cardPath)))

	l3Files := make([]core.FileHash, 0, len(files))
	for _, file := range files {
		if file.Path != skillDefinitionFile {
			l3Files = append(l3Files, file)
		}
	}
	contentHash, err := hashFileList(files)
	if err != nil {
		return SkillVersion{}, fmt.Errorf("技能 %s 的文件清单: %w", name, err)
	}
	l3Hash, err := hashFileList(l3Files)
	if err != nil {
		return SkillVersion{}, fmt.Errorf("技能 %s 的 L3 清单: %w", name, err)
	}

	return SkillVersion{
		Name:        name,
		VersionNo:   versionNo,
		Dir:         logicalDir,
		ContentHash: contentHash,
		SkillMDHash: sha256Hex(skillMD),
		L1Hash:      l1Hash,
		L2Hash:      sha256Hex(skillBodyBytes(skillMD)),
		L3Hash:      l3Hash,
		Files:       files,
	}, nil
}

// scanSkillFiles 递归收集技能目录内的普通文件（path 相对技能目录、正斜杠、升序），
// 并单独返回 SKILL.md 的原始字节。目录缺失、符号链接等非普通文件一律报错：
// 清单哈希必须能在别的机器上复算，无法稳定复现的条目不得进入清单。
// scanSkillFiles collects regular files (paths relative to the skill dir, ascending)
// and returns the raw SKILL.md bytes; anything non-regular is rejected.
func scanSkillFiles(base *os.Root, relDir string) ([]core.FileHash, []byte, error) {
	rootFS := base.FS()
	files := make([]core.FileHash, 0, 8)
	var skillMD []byte

	walkErr := fs.WalkDir(rootFS, relDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("遍历技能目录 %s: %w", relDir, err)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("技能目录 %s 含非普通文件 %s，拒绝计算哈希", relDir, p)
		}
		raw, readErr := base.ReadFile(filepath.FromSlash(p))
		if readErr != nil {
			return fmt.Errorf("读取技能文件 %s: %w", p, readErr)
		}
		rel := strings.TrimPrefix(p, relDir+"/")
		files = append(files, core.FileHash{Path: rel, SHA256: sha256Hex(raw), Size: int64(len(raw))})
		if rel == skillDefinitionFile {
			skillMD = raw
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}

	// WalkDir 的字典序只是实现细节；清单顺序由这里的显式排序决定。
	slices.SortFunc(files, func(a, b core.FileHash) int {
		return strings.Compare(a.Path, b.Path)
	})
	return files, skillMD, nil
}

// hashFileList 计算文件清单的规整哈希：json.Marshal 的紧凑字节（字段序 path、sha256、
// size），空清单序列化为 []（不是 null）——"没有 L3 文件"与"未记录"必须在字节上可分。
// hashFileList hashes the compact JSON of the file list; an empty list is [].
func hashFileList(files []core.FileHash) (string, error) {
	if files == nil {
		files = []core.FileHash{}
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("序列化文件清单: %w", err)
	}
	return sha256Hex(raw), nil
}

// skillBodyBytes 取 SKILL.md 中 frontmatter 之后的正文原始字节（约束 32）：闭合围栏
// 行的换行符之后即正文起点；围栏缺失或未闭合按"无 frontmatter"处理 → 整份字节。
// 不做 Trim——正文开头 / 结尾的空白与 CRLF 都进哈希。
// skillBodyBytes returns the raw bytes after the closing fence; without a closed
// fence the whole file is the body. Nothing is trimmed.
func skillBodyBytes(raw []byte) []byte {
	content := bytes.TrimPrefix(raw, utf8BOM)

	line, rest, ok := splitSkillLine(content)
	if !ok || !bytes.Equal(line, []byte("---")) {
		return raw
	}

	cursor := rest
	for {
		if len(cursor) == 0 {
			return raw // 围栏未闭合：整份字节
		}
		line, next, terminated := splitSkillLine(cursor)
		if !terminated {
			// 最后一行没有换行符：恰好是闭合围栏时正文为空，否则按无 frontmatter 处理。
			if bytes.Equal(bytes.TrimSuffix(cursor, []byte("\r")), []byte("---")) {
				return nil
			}
			return raw
		}
		if bytes.Equal(line, []byte("---")) {
			return next
		}
		cursor = next
	}
}

// utf8BOM 是 SKILL.md 允许出现的字节序标记（与 splitFrontmatter 的容忍口径一致）。
// 必须写转义——字面量会触发 Go 扫描器的「illegal byte order mark」。
// utf8BOM is the byte-order mark tolerated at the start of SKILL.md.
var utf8BOM = []byte("\ufeff")

// splitSkillLine 切出首行（去掉行尾 \r）与剩余字节；没有换行符时 ok=false。
// splitSkillLine cuts the first line; ok is false when no newline exists.
func splitSkillLine(b []byte) (line, rest []byte, ok bool) {
	i := bytes.IndexByte(b, '\n')
	if i < 0 {
		return nil, nil, false
	}
	return bytes.TrimSuffix(b[:i], []byte("\r")), b[i+1:], true
}

// copySkillVersion 按清单逐文件复制工作副本到发布目录：先建父目录，以 O_EXCL 写入，
// 写入字节与清单哈希不符 → 失败并清理本次创建的整个发布目录（不留下半截快照）。
// copySkillVersion copies the listed files into the version dir, verifies each hash,
// and removes the partially created dir on any failure.
func copySkillVersion(base *os.Root, workDir, relDir string, files []core.FileHash) error {
	if err := base.MkdirAll(filepath.FromSlash(relDir), dirPerm); err != nil {
		return fmt.Errorf("创建技能发布目录 %s: %w", relDir, err)
	}
	for _, file := range files {
		if err := copySkillFile(base, workDir, relDir, file); err != nil {
			if removeErr := base.RemoveAll(filepath.FromSlash(relDir)); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
				return errors.Join(err, fmt.Errorf("清理半截发布目录 %s: %w", relDir, removeErr))
			}
			return err
		}
	}
	return nil
}

// copySkillFile 复制单个文件并校验哈希：读取字节的 sha256 必须与清单条一致，否则
// 说明工作副本在哈希与复制之间被改动——拒绝冻结，不复制不一致的字节。
// copySkillFile verifies the exact bytes it writes against the manifest entry.
func copySkillFile(base *os.Root, workDir, relDir string, file core.FileHash) error {
	src := path.Join(workDir, file.Path)
	raw, err := base.ReadFile(filepath.FromSlash(src))
	if err != nil {
		return fmt.Errorf("读取技能文件 %s: %w", src, err)
	}
	if got := sha256Hex(raw); got != file.SHA256 {
		return fmt.Errorf("技能文件 %s 在哈希与复制之间发生变化（清单 %s，实读 %s），拒绝冻结", src, file.SHA256, got)
	}

	dst := path.Join(relDir, file.Path)
	if err := base.MkdirAll(filepath.Dir(filepath.FromSlash(dst)), dirPerm); err != nil {
		return fmt.Errorf("创建技能发布子目录 %s: %w", path.Dir(dst), err)
	}
	out, err := base.OpenFile(filepath.FromSlash(dst), os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	if err != nil {
		return fmt.Errorf("创建技能发布文件 %s: %w", dst, err)
	}
	if _, err := out.Write(raw); err != nil {
		return closeAndRemove(base, out, dst, fmt.Errorf("写入技能发布文件 %s: %w", dst, err))
	}
	if err := out.Sync(); err != nil {
		return closeAndRemove(base, out, dst, fmt.Errorf("刷盘技能发布文件 %s: %w", dst, err))
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("关闭技能发布文件 %s: %w", dst, err)
	}
	return nil
}

// closeAndRemove 收拢"失败即清理单个文件"：关闭与删除的错误都并入返回文本。
// closeAndRemove closes and removes a failed output file, joining both errors.
func closeAndRemove(base *os.Root, out *os.File, dst string, cause error) error {
	err := cause
	if closeErr := out.Close(); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("关闭技能发布文件 %s: %w", dst, closeErr))
	}
	if removeErr := base.Remove(filepath.FromSlash(dst)); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		err = errors.Join(err, fmt.Errorf("清理技能发布文件 %s: %w", dst, removeErr))
	}
	return err
}

// isSHA256Hex 判定 64 位小写十六进制（与 sha256Hex 的输出形态逐一对应）。
// isSHA256Hex accepts exactly 64 lowercase hex characters.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
