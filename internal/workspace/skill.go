package workspace

// 本文件是技能面的落地：装载期按声明找实体（规则 5），运行期按声明渲染 L1 卡片
//（段一素材之一）。绑定**只**由 AGENT.md 的 skills: 声明——Agent 本地 skills/ 目录
// 形态被无视（WARN 落在 agent.go 的装载路径上）。卡片只含三要素，正文留给模型按需
// read_file 取（渐进披露 L1/L2/L3，宪法 IV；不得预载正文、不得另设取用技能的工具）。
//
// 技能实体的查找与读取一律经**技能库根**（skills/ 子目录自己的 os.Root）——只约束在
// 工作区根内不够：`skills: [../agents/private]` 出不了工作区，却能读到技能库之外的文件
// （Agent 本地 skills/ 目录里的实体经 path.Join 清洁后正好被命中，其 description 会随
// 卡片进段一）。名字另**必须**是单个目录名，判据与 create / delete 共用同一份。
// This file resolves declared skills: existence at load time and the L1 card for
// section one. Both the lookup and the read are confined to the skill-library root,
// and a declared name must be a single directory name.
//
// 两处修复的来由与判据见 US-3 验收报告 R2 段（评审回合 P1）。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
)

const (
	// skillsDirName 是技能库目录名（工作区根部，与 agents/ 并列）；skillDefinitionFile
	// 是技能定义文件名——技能库形态为 skills/<name>/SKILL.md（技术方案 §3.4.1）。
	skillsDirName       = "skills"
	skillDefinitionFile = "SKILL.md"

	// skillLibraryPrefix 是卡片里「技能库真路径」的字面前缀（技术方案 §3.4.1）：
	// 相对**工作区根**（.basalt/）的展示路径，US-4 的 read_file 必须能吃下该形态。
	skillLibraryPrefix = ".basalt/skills"
)

// skillCard 是段一的 L1 卡片三要素（技术方案 §3.4.1：name + description + 技能库真路径），
// 外加两件读取产物：清单路径与原始字节——快照的 `{path, sha256}` 必须与**这张卡片**
// 出自同一次读取（§2.2 约束 7「注入内容与哈希共享同次读取产物」），故随卡片带出。
// skillCard is the L1 card (three display elements) plus the read artefacts the
// snapshot manifest needs from the very same read.
type skillCard struct {
	name         string // SKILL.md frontmatter name；缺失回落技能目录名（声明即绑定以目录名为准）
	description  string // SKILL.md frontmatter description；缺失为空串（契约 §2.1 无此校验规则——不发明）
	path         string // 逐字 .basalt/skills/<name>/SKILL.md（技术方案 §3.4.1 字面量）
	relativePath string // 相对工作区根的读取路径 skills/<目录名>/SKILL.md——清单的 path 列（按**声明名**，frontmatter name 改了也不动它）
	raw          []byte // 同一份读取的原始字节——清单条 sha256 的来源
}

// skillRelativePath 给出技能定义相对**工作区根**（.basalt/）的路径——只用于文案与
// 卡片的比对基准（错误点名让运维按 .basalt/ 下的位置找）；实际读取一律经技能库根 +
// skillEntryPath，两条路径都由本文件产出（不散落到调用点各拼一份）。
// skillRelativePath is the workspace-relative path used in messages only.
func skillRelativePath(name string) string {
	return path.Join(skillsDirName, name, skillDefinitionFile)
}

// skillEntryPath 给出技能实体在**技能库根内**的路径（<name>/SKILL.md）——读取用。
// skillEntryPath is the read path inside the skill-library root.
func skillEntryPath(name string) string {
	return path.Join(name, skillDefinitionFile)
}

// openSkillsRoot 打开技能库根（工作区根下的 skills/）。技能库整体缺失 → (nil, nil)：
// 装载面按「所有声明都没有实体」处理（每只声明了技能的 Agent 都以 skill not found
// 拒载），运行面由调用方报错点名路径（已注册的 Agent 不该遇到这种状态）。
// openSkillsRoot returns the skill-library root, or nil when the library is absent.
func openSkillsRoot(root string) (*os.Root, error) {
	libPath := filepath.Join(root, skillsDirName)
	lib, err := os.OpenRoot(libPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("打开技能库目录 %s: %w", libPath, err)
	}
	return lib, nil
}

// validSkillName 判定技能名是否为**单个目录名**——与 create / delete 的名称校验
// （ValidateAgentName）**共用同一份判据**：技能库形态是 skills/<name>/SKILL.md，
// 名字规则与 Agent 目录名同形，不得各写一份。
// validSkillName shares the single-directory-name rule with the write path.
func validSkillName(name string) bool {
	return ValidateAgentName(name) == nil
}

// skillCardPath 给出卡片展示用的技能库真路径（含 .basalt/ 前缀，正斜杠）。
// skillCardPath is the card-visible library path, prefixed with .basalt/.
func skillCardPath(name string) string {
	return path.Join(skillLibraryPrefix, name, skillDefinitionFile)
}

// skillCardLine 渲染一条 L1 卡片行（含中文冒号、全角括号与结尾换行）——**唯一**渲染点：
// 段一注入用它与 l1_hash 计算用它是同一份字节（颗粒度文档约束 32），两处不得各写一份。
// skillCardLine renders one L1 card line; section-one rendering and l1_hash hashing
// share this exact byte sequence.
func skillCardLine(name, description, cardPath string) string {
	return "- " + name + "：" + description + "（" + cardPath + "）\n"
}

// skillCardFields 从 SKILL.md 原始字节解析卡片要素：frontmatter 围栏缺失（或未闭合）
// 按字段缺失处理（回落目录名 + 空 description）；围栏在但解析失败 → 返回错误
// （作者缺陷不得静默降级成空卡片）。
// skillCardFields parses the card elements from the raw SKILL.md bytes; a missing
// fence degrades to defaults, a malformed fence is an error.
func skillCardFields(raw []byte, fallbackName string) (name, description string, err error) {
	front, _, splitErr := splitFrontmatter(raw)
	if splitErr != nil {
		if errors.Is(splitErr, errNoFrontmatterFence) || errors.Is(splitErr, errOpenFrontmatterFence) {
			return fallbackName, "", nil
		}
		return "", "", splitErr
	}

	var dto skillFrontmatterDTO
	if err := yaml.Unmarshal(front, &dto); err != nil {
		return "", "", err
	}
	name = fallbackName
	if declared := strings.TrimSpace(dto.Name); declared != "" {
		name = declared
	}
	return name, strings.TrimSpace(dto.Description), nil
}

// missingSkill 按声明顺序找**第一个没有实体**的技能名（全有 → 空串）。规则 5 只判
// 「有实体」：目录缺失、缺 SKILL.md、SKILL.md 是目录，三种都算没有实体。
//
// 技能库缺失（lib 为 nil）与名字不是单个目录名（越界声明）同样判「没有实体」——
// 越界名**不做任何拼接**：它本就不该被解析成一条路径（判据落在判定面，不落在文件系统
// 的反馈上——"越界的恰好命中了一个文件"不得让整只 Agent 注册成功）。
// missingSkill reports the first declared skill that has no library entity.
func missingSkill(lib *os.Root, declared []string) string {
	for _, name := range declared {
		if lib == nil || !validSkillName(name) {
			return name
		}
		info, err := lib.Stat(filepath.FromSlash(skillEntryPath(name)))
		if err != nil || info.IsDir() {
			return name
		}
	}
	return ""
}

// cardsFor 按声明顺序逐条渲染卡片（**不去重**：声明两次就出两张——绑定以声明为准）。
// 任一条目读取失败即返回错误并点名路径——不得回落空卡片、不得静默省略该节。
// cardsFor renders one card per declaration, in declaration order, without dedup.
func cardsFor(lib *os.Root, declared []string) ([]skillCard, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	if lib == nil {
		// 已注册的 Agent 声明了技能，技能库却整个不见了：报错并点名路径，不得回落空卡片。
		return nil, fmt.Errorf("技能库目录 %s 缺失——无法渲染技能卡片", skillsDirName)
	}
	cards := make([]skillCard, 0, len(declared))
	for _, name := range declared {
		card, err := skillCardFor(lib, name)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// skillCardFor 读技能库实体并产出单张卡片：SKILL.md 读不到 → 报错点名路径；
// 围栏整体缺失 → 按字段缺失处理（回落目录名 + 空 description——装载期只判「有实体」，
// 运行期不得因一份没有围栏的说明就把「已注册」翻成「不可用」）；
// 围栏**在但解析失败** → 报错（作者缺陷不得静默降级成空卡片，文案压缩不吐库原文）。
// skillCardFor reads one skill entity; a read failure is always an error.
func skillCardFor(lib *os.Root, name string) (skillCard, error) {
	if !validSkillName(name) {
		// 防线在读取之前：越界名连路径都不拼（卡片展示的 .basalt/skills/<name>/… 会
		// 被清洁成技能库之外的路径，交给模型的就是一条越界真路径）。
		return skillCard{}, fmt.Errorf("技能名不是单个目录名，拒绝读取技能库外的定义：%q", name)
	}
	rel := skillRelativePath(name) // 文案用工作区根相对路径（读取走技能库根内的路径）
	raw, err := lib.ReadFile(filepath.FromSlash(skillEntryPath(name)))
	if err != nil {
		return skillCard{}, fmt.Errorf("读取技能定义 %s: %w", rel, err)
	}

	card := skillCard{
		name: name, // 缺省 = 技能目录名（声明即绑定以目录名为准）
		path: skillCardPath(name),
		// 清单路径按**声明名**（卡片显示名可被 frontmatter 覆盖，读取路径不受影响）；
		// raw 与 rel 同源：这条 sha256 对应的正是进段一那一份字节。
		relativePath: rel,
		raw:          raw,
	}

	cardName, description, err := skillCardFields(raw, name)
	if err != nil {
		return skillCard{}, fmt.Errorf("解析技能定义 %s: %s", rel, frontmatterErrorSummary(err))
	}
	card.name = cardName
	card.description = description
	return card, nil
}

// skillFrontmatterDTO 是 SKILL.md 头的解析 DTO（技术方案 §3.4.1：frontmatter 为
// name + description，正文是技能说明）。缺失与显式空串同判——卡片是展示面，不是判定面。
// skillFrontmatterDTO parses the SKILL.md header; both keys are optional here.
type skillFrontmatterDTO struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}
