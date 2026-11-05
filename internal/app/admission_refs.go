package app

// 本文件实现 US-21 学习准入的技能引用扫描（颗粒度文档 §3.1.8）：基线里的
// `referrers` 名单回答"发布这个技能会影响哪些 Agent"，批准与发布都按它复核漂移。
//
// 三条纪律：
//   - **不新增第二套解析器**：活动 Agent 用 `workspace.ReadAgentDefinition` 读原始字节，
//     归档用 `readArchivedAgentDefinition` 读，两者都交给作者草稿的同一个窄解析器
//     `parseArchivedAgentFrontmatter`（只取 `skills` 等引用字段），再用
//     `draftReferencesResource` 判定引用——与删除闸门的既有口径完全一致；
//   - **fail-closed**：解析失败 / 读取失败的 Agent 按"可能引用"计入，点名为
//     `<agent>（unparseable）` / `archive:<archive_id>`；宁可让基线更保守，也不漏掉
//     一个可能被影响的 Agent；
//   - **只读、不跟随软链接**：只处理目录条目；定义文件由既有两个读取函数按"普通文件"
//     判据读入，符号链接与特殊文件一律不读。
//
// 缺失 `agents/` 或 `archive/` 目录是正常状态（未建 Agent / 未归档过），返回空名单；
// 工作区根打不开才是错误——调用方据此区分"没有引用者"与"读不到工作区"。
//
// This file scans active and archived AGENT.md files for references to one skill. It
// reuses the existing readers and narrow frontmatter parser, and treats anything
// unreadable or unparseable as a possible reference (fail-closed).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// admissionAgentsDirName 是活动 Agent 目录名（与 workspace 的私有同名字面量逐字一致，
// 本包已有同形态先例 `agentArchiveDirName`）；不引入第二种写法。
// admissionAgentsDirName mirrors the workspace literal for the active agent directory.
const admissionAgentsDirName = "agents"

// scanSkillReferences 返回引用指定技能的活动与归档 Agent 名单（去重、升序）。
// 同一份工作区两次调用得到同一顺序——基线 JSON 直接进哈希，顺序不得依赖 map 遍历。
//
// scanSkillReferences returns the sorted, de-duplicated names of active and archived
// agents that reference the skill; anything unreadable counts as a possible reference.
func scanSkillReferences(root, skillName string) ([]string, error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区根 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	refs := make([]string, 0)
	live, err := scanActiveSkillReferences(base, root, skillName)
	if err != nil {
		return nil, err
	}
	refs = append(refs, live...)
	archived, err := scanArchivedSkillReferences(base, skillName)
	if err != nil {
		return nil, err
	}
	refs = append(refs, archived...)
	slices.Sort(refs)
	return slices.Compact(refs), nil
}

// scanActiveSkillReferences 扫描 `agents/*/AGENT.md`：非目录条目（附属物、软链接）
// 如实忽略——它们不是 Agent，不进基线；目录内定义缺失或不可解析按可能引用计入。
// scanActiveSkillReferences scans every active agent directory and counts unreadable
// definitions as possible references.
func scanActiveSkillReferences(base *os.Root, root, skillName string) ([]string, error) {
	dir, err := base.Open(admissionAgentsDirName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开 Agent 目录: %w", err)
	}
	defer func() {
		_ = dir.Close()
	}()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("读取 Agent 目录: %w", err)
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

	refs := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		raw, readErr := workspace.ReadAgentDefinition(root, name)
		if readErr != nil {
			refs = append(refs, name+unparseableAgentMarker)
			continue
		}
		dto, parseErr := parseArchivedAgentFrontmatter(raw)
		if parseErr != nil {
			refs = append(refs, name+unparseableAgentMarker)
			continue
		}
		if draftReferencesResource(dto, core.ResourceSkill, skillName) {
			refs = append(refs, name)
		}
	}
	return refs, nil
}

// scanArchivedSkillReferences 扫描 `archive/<archive_id>/AGENT.md`：口径与既有归档
// 删除闸门一致——缺 `AGENT.md` 的归档目录跳过；读取 / 解析失败点名 `archive:<id>`。
// scanArchivedSkillReferences scans archived definitions and names failures as
// archive:<id> so they still count as possible references.
func scanArchivedSkillReferences(base *os.Root, skillName string) ([]string, error) {
	dir, err := base.Open(agentArchiveDirName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开 Agent 归档目录: %w", err)
	}
	defer func() {
		_ = dir.Close()
	}()

	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("读取 Agent 归档目录: %w", err)
	}

	refs := make([]string, 0)
	for _, entry := range entries {
		// 只有目录才是归档单元；符号链接与普通文件都不读（不跟随、不越界）。
		if !entry.IsDir() {
			continue
		}
		archiveID := entry.Name()
		rel := filepath.FromSlash(path.Join(agentArchiveDirName, archiveID, authorDraftDefinitionPath))
		raw, readErr := readArchivedAgentDefinition(base, rel)
		if errors.Is(readErr, fs.ErrNotExist) {
			continue
		}
		if readErr != nil {
			refs = append(refs, "archive:"+archiveID)
			continue
		}
		dto, parseErr := parseArchivedAgentFrontmatter(raw)
		if parseErr != nil {
			refs = append(refs, "archive:"+archiveID)
			continue
		}
		if draftReferencesResource(dto, core.ResourceSkill, skillName) {
			refs = append(refs, "archive:"+archiveID)
		}
	}
	return refs, nil
}
