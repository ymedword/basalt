package memory

// 本文件是 markdown 载体（颗粒度文档 §2.3「载体侧 markdown」伪代码逐块同构）：两分区
// 解析与渲染、写入入口规范化、唯一名临时文件 + rename 原子替换、**读取面三档**、归档区
// 条目级整串检索。路径一律取 `workspace.MemoryFilePath`（唯一事实源，注 4）。
// This file is the markdown carrier: the two-section parser and renderer, write-entry
// normalization, atomic replace through a unique temp file + rename, the three-tier read
// face, and archive-only whole-string recall.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// MarkdownStore 是 markdown 档载体：一层一个文件，全部读写经 `os.OpenRoot` 根句柄
// （口径 14——句柄把相对路径锁在工作区根内，不靠字符串前缀比较）。
// MarkdownStore is the file-backed carrier; every read and write goes through an
// os.Root handle opened on the workspace root.
type MarkdownStore struct {
	root string

	// openTemp / rename 是原子替换的**测试注入点**（生产恒为 nil，走真实 os.Root）：
	// 写入 / 关闭 / rename 的失败无法跨平台确定性地制造，故障注入是 FR-014 的验收手段。
	// openTemp / rename are test seams for deterministic replace-failure injection.
	openTemp func(root *os.Root, name string, flag int, perm os.FileMode) (tempFile, error)
	rename   func(root *os.Root, oldname, newname string) error
}

// tempFile 是临时文件的最小写面（*os.File 实现）；接口只服务于故障注入测试。
type tempFile interface {
	WriteString(s string) (int, error)
	Close() error
}

// globalMemoryFileTitle 是全局层文件标题的逐字字面量——与 `workspace` 的
// `templateMemoryMD` 首行**同源**（口径 3）。Agent 层标题未在事实源里冻结，取同构形态。
// The verbatim global-layer title, shared with the workspace template.
const globalMemoryFileTitle = "# MEMORY.md —— 长期记忆 · 全局层"

// NewMarkdownStore 构造载体。根路径由装配层显式传入（不在包内读配置或环境）。
func NewMarkdownStore(root string) *MarkdownStore {
	return &MarkdownStore{root: root}
}

// 编译期断言：markdown 档与 sqlite 档跑同一张场景表（约束 17）的前提是接口面一致。
var _ core.LongTermMemoryStore = (*MarkdownStore)(nil)

// Load 返回该层文件**原文**（管理面与巡检用，含文件标题与说明段；注入面走
// `LoadEffective`——那才读重建结果，口径 22）。**合法空层归一成空文本**：文件不存在、
// 零字节、只有结构行而无任何条目——三者都返回 `("", nil)`，与 sqlite 档零行渲染
// 同形（FR-017 两档同语义）。**非空且结构损坏 → 返回错误**，错误点名文件与症状
// （约束 27 第三档：拒绝使用、拒绝覆盖，**不得**当空层回退）。
// Load returns the file verbatim; a legal empty layer normalizes to "" so both carriers
// agree, and a damaged non-empty file is an error rather than a silent empty layer.
func (m *MarkdownStore) Load(ctx context.Context, scope core.Scope) (string, error) {
	rel := workspace.MemoryFilePath(scope.Agent)
	var text string
	err := m.withRoot(func(root *os.Root) error {
		var err error
		text, err = loadLayer(root, rel)
		return err
	})
	if err != nil {
		return "", err
	}
	return text, nil
}

// Append 向该层 section 分区追加一条条目。序列照 §2.3：**规范化在最前**（先于两条配额
// 检查——约束 29 / P1-b）→ 读原文（三档）→ `parseSections` → 配额判定 → 重建 → 唯一名
// 临时文件 + rename 原子替换。任一步失败都不触碰目标文件：**旧文件逐字节不变**（约束 12）。
// Append normalizes first, then reads, parses, checks quota, rebuilds and atomically
// replaces; a failure anywhere leaves the original file byte-for-byte untouched.
func (m *MarkdownStore) Append(ctx context.Context, scope core.Scope, section core.Section, content string) error {
	added := normalizeEntry(content) // 写入入口规范化一次（约束 29）
	rel := workspace.MemoryFilePath(scope.Agent)
	title := fileTitle(scope.Agent)

	return m.withRoot(func(root *os.Root) error {
		text, err := loadLayer(root, rel)
		if err != nil {
			return err
		}
		coreEntries, archiveEntries, err := parseSections(text)
		if err != nil {
			return err
		}

		// 两条配额检查都作用于**规范化后的条目正文**（约束 29）：单条先于总量，
		// 故越限档位可判（entry / section）。归档区无配额（约束 7）。
		if section == core.SectionCore {
			if len(added) > core.MaxCoreEntryBytes {
				return &core.MemoryQuotaError{Kind: core.QuotaKindEntry, Limit: core.MaxCoreEntryBytes}
			}
			if sectionBytes(coreEntries)+len(added) > core.MaxCoreSectionBytes {
				return &core.MemoryQuotaError{Kind: core.QuotaKindSection, Limit: core.MaxCoreSectionBytes}
			}
			coreEntries = append(coreEntries, added)
		} else {
			archiveEntries = append(archiveEntries, added)
		}

		return m.writeFileAtomically(root, rel, renderMemory(title, coreEntries, archiveEntries))
	})
}

// RecallByKeyword 在**归档区**做条目级整串检索（约束 9）：`strings.Contains`、区分大小写、
// 按写入顺序；`query` 的规范化由工具侧负责（T033）。未命中 = 空结果 + `nil` error
// （口径 10——不是错误）。核心区不参与检索（它每轮已完整注入）。
// RecallByKeyword searches archive entries by whole-string match, case-sensitive, in
// write order; a miss is an empty result rather than an error.
func (m *MarkdownStore) RecallByKeyword(ctx context.Context, scope core.Scope, query string) ([]string, error) {
	rel := workspace.MemoryFilePath(scope.Agent)
	var hits []string

	err := m.withRoot(func(root *os.Root) error {
		text, err := loadLayer(root, rel)
		if err != nil {
			return err
		}
		_, archiveEntries, err := parseSections(text)
		if err != nil {
			return err
		}
		for _, entry := range archiveEntries {
			if strings.Contains(entry, query) {
				hits = append(hits, entry)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}

// fileTitle 是该层文件标题：全局层与 `templateMemoryMD` 逐字同源；Agent 层取同构形态
// （**未在事实源里冻结**——测试只钳"第 1 行是 `#` 开头的标题行"这一形态）。
// fileTitle derives the layer's title line; only the global-layer text is frozen.
func fileTitle(agent string) string {
	if agent == "" {
		return globalMemoryFileTitle
	}
	return "# MEMORY.md —— 长期记忆 · Agent 层（" + agent + "）"
}

// normalizeEntry 是写入入口的**唯一**规范化：规范表示 = `strings.TrimSpace(原文)`——
// 与 sqlite 档的 `strings.TrimSpace` 是**同一个标准库函数**（约束 29，两处不存在第二套
// 算法）。重复调用幂等；用户内容里的 `"- "` 前缀**不剥**（那是用户内容，不是载体添加物）。
// normalizeEntry is the single normalization at the write entry, identical to the sqlite
// carrier's call; a user-supplied "- " prefix is content, not carrier decoration.
func normalizeEntry(content string) string {
	return strings.TrimSpace(content)
}

// sectionBytes 求和 = 各条目**正文自身**的 UTF-8 字节（约束 29）：只排除载体添加物——
// 文件标题、分区标题、条目前缀 `"- "`、续行缩进、条目间空行都不在正文里，天然不计入。
func sectionBytes(entries []string) int {
	total := 0
	for _, entry := range entries {
		total += len(entry)
	}
	return total
}

// splitLines 按 `\n` 切行并去掉行尾的 `\r`：Windows 记事本手工保存的 CRLF 文件与 LF
// 文件因此得到同一组结构行与正文行。**只影响解析面**——`Load` 返回的文件原文仍逐字节
// 保留（CRLF 不被改写）；重建产物一律用文档化的 LF 行尾。
// splitLines normalizes line endings for parsing only; Load still returns the raw bytes.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

// parseSections 把一层文本切成两分区条目（正文形态，**不带** `"- "` 前缀与续行缩进）。
// 判据只认**三条结构事实**（约束 27 / 28）：两个分区标题都在、各出现一次、核心区在前。
// 合法扩展（多余的 `###` 小节、裸段落、条目正文里的任何行）不算损坏——零缩进的未知行
// 一律按**条目**收集（重建时补 `"- "` 与缩进，注入面读的是重建结果）。
// parseSections splits one layer's text into the two sections' entry bodies; only the
// three structural facts are judged, everything else is content.
func parseSections(text string) (coreEntries, archiveEntries []string, err error) {
	// 零字节 / 纯空白 = 合法空层（约束 27 第一档）：首次创建即走这条路径，**不得**判损坏。
	if strings.TrimSpace(text) == "" {
		return nil, nil, nil
	}

	inCore, inArchive := false, false
	var entry []string
	var target *[]string // 当前收集目标；未分区时丢弃（说明段与文件标题不是条目）

	flush := func() {
		if len(entry) == 0 {
			return
		}
		lines := entry
		entry = nil
		body := strings.TrimSpace(strings.Join(lines, "\n"))
		if body == "" {
			return
		}
		if target != nil {
			*target = append(*target, body)
		}
	}

	for _, line := range splitLines(text) {
		switch {
		case line == sectionCoreTitle:
			flush()
			if inCore {
				return nil, nil, fmt.Errorf("memory: 结构损坏——分区标题出现两次：%s", sectionCoreTitle)
			}
			if inArchive {
				return nil, nil, fmt.Errorf("memory: 结构损坏——分区次序错：%s 早于 %s", sectionArchiveTitle, sectionCoreTitle)
			}
			inCore, target = true, &coreEntries
		case line == sectionArchiveTitle:
			flush()
			if inArchive {
				return nil, nil, fmt.Errorf("memory: 结构损坏——分区标题出现两次：%s", sectionArchiveTitle)
			}
			inArchive, target = true, &archiveEntries
		case strings.HasPrefix(line, "# "): // 文件标题：结构行，其后的正文归属回到"未分区"
			flush()
			target = nil
		case line == "" || strings.HasPrefix(line, " "): // 续行（含空行）：空行**不**切块
			entry = append(entry, stripContinuationIndent(line))
		default: // 零缩进行：新条目的首行（`"- "` 前缀按载体添加物剥掉）
			flush()
			entry = append(entry, strings.TrimPrefix(line, "- "))
		}
	}
	flush()

	// 三条结构事实的其余两条：非空文本必须两个分区标题齐备（缺失即损坏——不得当空层）。
	if !inCore {
		return nil, nil, fmt.Errorf("memory: 结构损坏——缺少分区标题：%s", sectionCoreTitle)
	}
	if !inArchive {
		return nil, nil, fmt.Errorf("memory: 结构损坏——缺少分区标题：%s", sectionArchiveTitle)
	}
	return coreEntries, archiveEntries, nil
}

// stripContinuationIndent 去载体添加的续行缩进（约束 28：正文各行统一加两空格；空行写成
// 空行）。手写的单空格缩进不属于文档化形态，原样保留（不猜、不吞用户字符）。
func stripContinuationIndent(line string) string {
	if strings.HasPrefix(line, "  ") {
		return line[2:]
	}
	return line
}

// renderMemory 重建整层文件：文件标题 + 核心区 + 归档区。说明段**不在**重建产物里
// （签名只收 title 与两分区——§2.3 伪代码即事实源），故人手写的说明段在首次写入后不再保留。
// renderMemory rebuilds the whole file from its title and the two sections.
func renderMemory(title string, coreEntries, archiveEntries []string) string {
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n\n")
	b.WriteString(renderSection(sectionCoreTitle, coreEntries))
	b.WriteString("\n\n")
	b.WriteString(renderSection(sectionArchiveTitle, archiveEntries))
	b.WriteString("\n")
	return b.String()
}

// renderSection 渲染一个分区：无条目时只剩标题行（空分区不放空行——重建幂等的前提）。
func renderSection(heading string, entries []string) string {
	if len(entries) == 0 {
		return heading
	}
	rendered := make([]string, 0, len(entries))
	for _, entry := range entries {
		rendered = append(rendered, renderEntry(entry))
	}
	return heading + "\n\n" + strings.Join(rendered, "\n\n")
}

// renderEntry 渲染一条条目：首行加 `"- "`，其余各行（含空行）加两空格缩进（空行写成空行，
// 无行尾空格——约束 28）。写入与重建因此**无损可逆**。
func renderEntry(body string) string {
	lines := strings.Split(body, "\n")
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(lines[0])
	for _, line := range lines[1:] {
		b.WriteString("\n")
		if line != "" {
			b.WriteString("  ")
			b.WriteString(line)
		}
	}
	return b.String()
}

// loadLayer 是**读取面三档**的唯一入口（约束 27）：① 缺失 / 零字节 / 只有结构行 =
// 合法空层 `("", nil)`（回退照常、首次写入即创建）；② 其他 I/O 失败 = `("", err)`
// （注入面据此拒绝启动、写入面不触碰原文件）；③ 非空且结构损坏 = `("", err)`（点名
// 文件与症状）。三档**不得**用一个 `ok` 抹平。
// loadLayer is the single entry of the three-tier read face; tiers never collapse.
func loadLayer(root *os.Root, rel string) (string, error) {
	raw, err := root.ReadFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil // ① 文件不存在 = 空层（正常取值，不是失败）
	}
	if err != nil {
		return "", fmt.Errorf("memory: 读取 %s: %w", rel, err)
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		return "", nil // ① 零字节 = 空层
	}
	coreEntries, archiveEntries, err := parseSections(text)
	if err != nil {
		return "", fmt.Errorf("memory: %s: %w", rel, err)
	}
	if len(coreEntries) == 0 && len(archiveEntries) == 0 {
		return "", nil // ① 只有结构行而无条目 = 空层
	}
	return text, nil // 原文逐字节返回（Load 面）
}

// writeFileAtomically 写唯一名临时文件再 `os.Root.Rename` 覆盖（口径 14）：目标要么是
// 旧内容、要么是新内容，不会出现半截文件。任一步失败都清理临时文件并原样上抛——
// **旧文件逐字节不变**（约束 12）；清理失败并入错误文本，不静默丢弃。
// 生产路径的 `openTemp` / `rename` 恒为 nil（走真实 os.Root）；同包白盒测试只临时替换
// 它们，制造写入 / 关闭 / rename 的确定性失败（FR-014 的故障注入验收）。
// writeFileAtomically replaces the target through a uniquely named temp file; on any
// failure the temp file is removed and the original stays byte-for-byte intact.
func (m *MarkdownStore) writeFileAtomically(root *os.Root, rel, text string) error {
	dir, base := path.Split(rel)
	if dir != "" {
		if err := root.MkdirAll(dir, 0o750); err != nil { // Agent 层目录按需创建（约束 15）
			return fmt.Errorf("memory: 建目录 %s: %w", dir, err)
		}
	}
	tmp, err := tempName(dir, base)
	if err != nil {
		return err
	}

	openTemp := m.openTemp
	if openTemp == nil {
		openTemp = func(root *os.Root, name string, flag int, perm os.FileMode) (tempFile, error) {
			return root.OpenFile(name, flag, perm)
		}
	}
	f, err := openTemp(root, tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("memory: 建临时文件 %s: %w", tmp, err)
	}
	if _, err := f.WriteString(text); err != nil {
		return tempFailure(root, tmp, fmt.Errorf("memory: 写入 %s: %w", tmp, err), f)
	}
	if err := f.Close(); err != nil {
		return tempFailure(root, tmp, fmt.Errorf("memory: 关闭临时文件 %s: %w", tmp, err), nil)
	}
	rename := m.rename
	if rename == nil {
		rename = func(root *os.Root, oldname, newname string) error {
			return root.Rename(oldname, newname)
		}
	}
	if err := rename(root, tmp, rel); err != nil {
		return tempFailure(root, tmp, fmt.Errorf("memory: 替换 %s: %w", rel, err), nil)
	}
	return nil
}

// tempName 生成同目录的唯一名临时文件路径（`os.Root` **没有** CreateTemp）。唯一名取
// `crypto/rand`（随机源红线：不得 `math/rand`）；同名冲突由 `O_EXCL` 兜底为错误而非覆盖。
func tempName(dir, base string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("memory: 生成临时文件名: %w", err)
	}
	return dir + base + ".tmp-" + hex.EncodeToString(buf[:]), nil
}

// tempFailure 收拢"失败即清理"：可选地关闭尚未关闭的句柄，**无论关闭是否成功都尝试**
// 删除临时文件（关闭失败也先尽力清理，残留另有错误文本点名）；清理失败并入错误文本
// （**不**静默丢弃），cause 仍是 `errors.Is` / `errors.As` 的判据。
// tempFailure always attempts removal even when closing fails, so a combined
// write-and-close failure cannot leave temp-file residue untried.
func tempFailure(root *os.Root, tmp string, cause error, f tempFile) error {
	var closeErr error
	if f != nil {
		closeErr = f.Close()
	}
	removeErr := root.Remove(tmp)
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	switch {
	case closeErr != nil && removeErr != nil:
		return fmt.Errorf("%w（临时文件关闭失败：%v；清理失败：%v）", cause, closeErr, removeErr)
	case closeErr != nil:
		return fmt.Errorf("%w（临时文件关闭失败：%v）", cause, closeErr)
	case removeErr != nil:
		return fmt.Errorf("%w（临时文件清理失败：%v）", cause, removeErr)
	}
	return cause
}

// withRoot 打开工作区根句柄并保证关闭：全部读写经 `os.OpenRoot(root)`（口径 14）。
// 句柄的关闭错误只在没有更重要的错误时接管返回值——清理失败不盖掉真实 I/O 错误，
// 也不静默丢弃。
// withRoot opens the workspace root handle for one operation and always closes it.
func (m *MarkdownStore) withRoot(fn func(root *os.Root) error) error {
	root, err := os.OpenRoot(m.root)
	if err != nil {
		return fmt.Errorf("memory: 打开工作区根 %s: %w", m.root, err)
	}
	err = fn(root)
	if closeErr := root.Close(); err == nil && closeErr != nil {
		return fmt.Errorf("memory: 关闭工作区根 %s: %w", m.root, closeErr)
	}
	return err
}
