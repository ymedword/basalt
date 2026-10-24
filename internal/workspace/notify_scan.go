package workspace

// 本文件是通知渠道删除闸门的**文档扫描面**（颗粒度文档 §3.1.4 / 约束 38）：
// `ScanAgentDocuments` 只读地收集活动 Agent 与归档目录里的 `AGENT.md` **全文**，
// `WholeWordMatch` 提供整词判定。口径：
//
//   - 扫描不跟随软链接、不越出工作区根（`os.Root` + `Lstat` 双重约束）；
//   - **归档定义的 frontmatter 解析失败仍然返回文档**——正文扫描不依赖解析，
//     漏扫会让删除闸门放行仍在被引用的渠道（fail-closed）；
//   - 读取失败 / 非普通文件 / 超过上限则**整体报错**：证明不了"没人引用"时，
//     调用方必须拒绝删除，而不是跳过这一份继续放行；
//   - `archive/` 不存在按"没有归档"处理（从未归档过 Agent 是正常状态）；
//     `agents/` 不存在是工作区未初始化，报错。
//
// This file is the document scan surface behind the notify-channel delete gate:
// full-text scans of active and archived AGENT.md files plus whole-word matching.
// Unparseable archives are still returned (the text scan must not miss a
// reference); unreadable or oversized documents fail the whole scan.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
)

// maxAgentDocumentBytes 是单份 AGENT.md 的扫描上限（1 MiB，与 app 侧归档定义同一
// 口径）：超限按错误处理而**不截断**——截断可能把后半段的整词匹配切掉，变成假阴性。
// maxAgentDocumentBytes bounds one scanned AGENT.md; oversize is an error, never a
// truncated read (truncation could hide a reference after the cut).
const maxAgentDocumentBytes = 1 << 20

// AgentDocument 是一份待扫描的 `AGENT.md` 全文：Name 是活动的 Agent 目录名；
// 归档文档取 frontmatter 的 `name`（解析失败回退归档标识——名字只用于人读，正文
// 才是判定面）。Archived / ArchiveID 只对归档目录设置。
// AgentDocument is one AGENT.md to scan. Name is the agent directory for active
// documents and the frontmatter name for archives (falling back to the archive id);
// Archived and ArchiveID are set only for archives.
type AgentDocument struct {
	Name      string
	Text      string
	Archived  bool
	ArchiveID string
}

// ScanAgentDocuments 收集工作区内所有 `AGENT.md` 的全文：活动区按
// `agents/<name>/AGENT.md`、归档区按 `archive/<archive_id>/AGENT.md` 扫描，活动区
// 在前、归档区在后，各自按名字节序排序（不得依赖库的隐式序）。缺 `AGENT.md` 的
// 目录不可能引用任何渠道，跳过；非普通文件的定义、读取失败与超限一律报错
// （fail-closed，见文件头）。
// ScanAgentDocuments returns the full text of every AGENT.md in the workspace,
// active documents first, each group sorted by name.
func ScanAgentDocuments(root string) (docs []AgentDocument, err error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	active, err := scanAgentDocumentDir(base, agentsDirName, false, true)
	if err != nil {
		return nil, err
	}
	archived, err := scanAgentDocumentDir(base, archiveDirName, true, false)
	if err != nil {
		return nil, err
	}
	return append(active, archived...), nil
}

// scanAgentDocumentDir 扫描一个目录下的文档单元（`<dir>/<entry>/AGENT.md`）。
// required 为真时目录缺失是错误（agents/ 是工作区骨架）；为假时缺失按空集处理
// （archive/ 可以从未创建过）。条目只认**真实目录**：非目录条目与软链接一律忽略
// 且不跟随（软链接目录不是 Agent 单元，扫描它等于"指向哪就扫哪"）。
// scanAgentDocumentDir scans one directory of document units.
func scanAgentDocumentDir(base *os.Root, dirRel string, archived, required bool) ([]AgentDocument, error) {
	dir, err := base.Open(dirRel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !required {
			return nil, nil
		}
		return nil, fmt.Errorf("打开目录 %s: %w", dirRel, err)
	}
	defer func() {
		_ = dir.Close()
	}()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("读取目录 %s: %w", dirRel, err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

	docs := make([]AgentDocument, 0, len(entries))
	for _, entry := range entries {
		// 只有真实目录才是文档单元；符号链接与普通文件都不读（不跟随、不越界）。
		if !entry.IsDir() {
			continue
		}
		unit := entry.Name()
		rel := path.Join(dirRel, unit, agentDefinitionFile)
		raw, readErr := readAgentDocument(base, rel)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue // 缺 AGENT.md 的目录不可能引用任何渠道
		}
		if readErr != nil {
			return nil, fmt.Errorf("读取文档 %s: %w", rel, readErr)
		}
		doc := AgentDocument{Text: string(raw)}
		if archived {
			doc.Name = archivedAgentDocumentName(raw, unit)
			doc.Archived = true
			doc.ArchiveID = unit
		} else {
			doc.Name = unit
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

// readAgentDocument 在 `os.Root` 内读取一份定义：先 `Lstat` 判类型（**不跟随**软链接），
// 再打开后复核一次（防两次判定之间被换掉）；只接受普通文件，超过上限按错误处理。
// readAgentDocument reads one definition inside the workspace root without
// following symlinks; non-regular or oversized files are errors.
func readAgentDocument(base *os.Root, rel string) ([]byte, error) {
	info, err := base.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("定义不是普通文件（不跟随软链接）")
	}
	if info.Size() > maxAgentDocumentBytes {
		return nil, fmt.Errorf("定义超过 %d 字节上限", maxAgentDocumentBytes)
	}

	file, err := base.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() > maxAgentDocumentBytes {
		return nil, fmt.Errorf("定义在打开时已不是可扫描的普通文件")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxAgentDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAgentDocumentBytes {
		return nil, fmt.Errorf("定义超过 %d 字节上限", maxAgentDocumentBytes)
	}
	return raw, nil
}

// archivedAgentDocumentName 取归档定义 frontmatter 的 `name`；围栏缺失或 YAML
// 不可解时回退归档标识——正文已经读到，名字只影响人读的点名文案，**不得**因为
// 名字取不到就丢掉文档（那会让整词扫描漏掉这份正文）。
// archivedAgentDocumentName reads the frontmatter name and falls back to the
// archive id; the document itself is never dropped over a naming failure.
func archivedAgentDocumentName(raw []byte, archiveID string) string {
	front, _, err := splitFrontmatter(raw)
	if err != nil {
		return archiveID
	}
	var dto struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(front, &dto); err != nil || dto.Name == "" {
		return archiveID
	}
	return dto.Name
}

// WholeWordMatch 判断 word 是否在 text 中**整词**出现：匹配两侧都不得是 Unicode
// 字母 / 数字 / 下划线（其余字符——空白、标点、括号——都算边界），大小写敏感
// （渠道名是冻结字面量）。空 word 一律不匹配——不得退化成"处处匹配"。
// WholeWordMatch reports whether word occurs in text with non-word characters on
// both sides; the match is case-sensitive and the empty word never matches.
func WholeWordMatch(text, word string) bool {
	if word == "" || len(word) > len(text) {
		return false
	}
	for offset := 0; offset <= len(text)-len(word); {
		i := strings.Index(text[offset:], word)
		if i < 0 {
			return false
		}
		start := offset + i
		end := start + len(word)
		if !wordRuneBefore(text, start) && !wordRuneAfter(text, end) {
			return true
		}
		offset = start + 1
	}
	return false
}

// wordRuneBefore / wordRuneAfter 判匹配两侧的字符是否属于词内字符（越界算边界）。
// wordRuneBefore and wordRuneAfter report whether the adjacent rune is a word
// character; out of range counts as a boundary.
func wordRuneBefore(text string, at int) bool {
	if at <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(text[:at])
	return isWordRune(r)
}

func wordRuneAfter(text string, at int) bool {
	if at >= len(text) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(text[at:])
	return isWordRune(r)
}

// isWordRune 是整词判定的词内字符集：Unicode 字母 / 数字 / 下划线。
// isWordRune is the word-character set: Unicode letters, digits and underscore.
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
