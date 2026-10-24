package workspace

// 本文件是系统提示词**段一**的拼装与**运行级配置快照**的冻结（技术方案 §2.2.2「段 1
// 组合式」/ §8.3 段一 / §11.5 运行快照）：identity.prompt（若配置）→ 自我称谓 →
// AGENT.md 正文 → Bootstrap 文件（声明顺序）→ 技能卡片 L1 → 末尾当前时间。
//
// **读即所记**（§2.2 约束 7 / §2.4 注 2）：段一注入的每一个文件与清单条的 sha256 出自
// **同一次读取**，快照哈希再由整份清单的规整序列化派生——注入内容与"这次运行用的是哪版
// 配置"不可能分叉。故读取只有一条路径（`freeze`）：`Snapshot` 与 `SectionOne` 都走它。
// This file assembles section one and freezes the run-level config snapshot. Both are
// products of a single read, so the injected bytes and the manifest can never drift.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// ContextLoader 组装系统提示词段一、并冻结运行级配置快照（技术方案 §3.3 / §3.6）。
// ContextLoader assembles section one and freezes the run-level config snapshot; it
// holds no state beyond its logger — every call re-reads its sources.
type ContextLoader struct {
	logger *slog.Logger
}

// NewContextLoader 构造装配器；nil Logger 回落 slog.Default（与装载器同一口径）。
// NewContextLoader builds the assembler.
func NewContextLoader(logger *slog.Logger) *ContextLoader {
	if logger == nil {
		logger = slog.Default()
	}
	return &ContextLoader{logger: logger}
}

// SectionOne 组装段一：identity.prompt + 自我称谓 + AGENT.md 正文 + Bootstrap 文件 +
// 技能卡片 L1 + 当前日期时间（格式 2006-01-02 15:04:05 -07:00——缺省 UTC、含偏移，§2.4 注 10）。
// now 显式传入；每次调用重读文件（无缓存——§2.2 约束 7）；任一步读取失败 → 返回错误（§2.2 约束 6）。
//
// 签名与行为自 US-3 起不变；US-4 起本方法**委托给 Snapshot**——段一就是快照的注入面，
// 两条读取路径分家正是"注入内容与清单哈希分叉"的根因（§2.2 约束 7）。
// SectionOne returns the section-one text produced by Snapshot, and nothing else.
func (c *ContextLoader) SectionOne(root string, entry core.AgentEntry, now time.Time) (string, error) {
	snap, err := c.Snapshot(root, entry, now)
	if err != nil {
		return "", err
	}
	return snap.SectionOne, nil
}

// Snapshot 冻结一次运行的配置（技术方案 §3.6）：**同一份原始 AGENT.md** 解析出 Profile
// 与段一正文，原始字节另存 `Content`；清单逐条记 `{path, sha256}`，`Hash` = 清单**规整
// 序列化**的 sha256——**当前时间不得混入**（同一内容不同时刻必须同哈希）。
// Snapshot freezes the config for one run from a single read: profile, section one,
// the raw AGENT.md, and a hash derived from the manifest alone (never the clock).
//
// 生命周期 = 每次运行一份：磁盘上的 AGENT.md 在运行期间怎么改都不会让这次运行中途换
// 配置（下一次运行才生效）；不缓存、不复用。
//
// 本方法是**工作副本**口径（技能卡片读 `skills/<name>/SKILL.md`）；已发布版本的运行
// 由 SnapshotPublished 冻结（约束 31），两者共用同一条读取 / 组装实现。
func (c *ContextLoader) Snapshot(root string, entry core.AgentEntry, now time.Time) (*core.ConfigSnapshot, error) {
	return c.snapshot(root, entry, now, nil)
}

// PublishedSkills 是"技能名 → 发布目录（storage_path）"的显式映射（约束 31）：值取自
// 准入解析结果 `ResolvedResources.SkillDirs`，形态为契约字面量
// `.basalt/versions/skills/<name>/<version_no>`。映射缺失即失败关闭——发布版本快照
// **不得**回落到工作副本。
// PublishedSkills maps each declared skill to its published storage_path; a missing
// entry fails closed instead of falling back to the work copy.
type PublishedSkills map[string]string

// SnapshotPublished 按**发布版本**冻结一次运行的配置（约束 31）：卡片三要素、段一注入
// 与清单哈希仍出自同一次读取，读取目录换成本次运行准入时固定的发布真目录。L1 卡片行
// 展示发布真路径 `.basalt/versions/skills/<name>/<no>/SKILL.md`，快照清单仍记逻辑路径
// `.basalt/skills/<name>/SKILL.md`（逻辑路径只用于清单与展示映射，不用于运行期读取）。
// 声明技能缺目录映射、目录形态不符或 SKILL.md 读不到，一律失败关闭。
// SnapshotPublished freezes the run configuration from the pinned published version
// directories: the card shows the real published path while the manifest keeps the
// logical path; a missing directory or an unreadable SKILL.md fails closed.
func (c *ContextLoader) SnapshotPublished(root string, entry core.AgentEntry, skills PublishedSkills, now time.Time) (*core.ConfigSnapshot, error) {
	if skills == nil {
		// 非 nil 空映射：无技能声明的 Agent 照常出快照，声明了技能的 Agent 必在卡片源处
		// 失败关闭——**不得**因为"没有映射"就退回工作副本。
		skills = PublishedSkills{}
	}
	return c.snapshot(root, entry, now, skills)
}

// snapshot 是两个公开入口的共用实现：published 非 nil = 发布版本来源，nil = 工作副本。
// snapshot is the shared implementation behind both public entry points.
func (c *ContextLoader) snapshot(root string, entry core.AgentEntry, now time.Time, published PublishedSkills) (*core.ConfigSnapshot, error) {
	frozen, err := c.freeze(root, entry, now, published)
	if err != nil {
		return nil, err
	}

	// 排序 / 规整序列化 / 哈希只有一份实现（`core.BuildConfigSnapshot`）：工作副本与
	// 发布版本两条来源共用同一段代码，哈希不可能分叉。重复路径不合并（声明两次出两条
	// 清单条——与卡片"不去重"同口径，"读即所记"记的是读取行为）。
	snapshot, err := core.BuildConfigSnapshot(entry.Name, frozen.raw, frozen.section, frozen.profile, frozen.manifest)
	if err != nil {
		// 元素全是字符串字段，Marshal 实际不会失败；仍如实上抛，不吞。
		return nil, err
	}

	// 留痕只到「条数」为止——**不得**回显文件全文或配置原文（FR-027 可观测性双轨）。
	c.logger.Debug("配置快照已冻结", "agent", entry.Name, "hash", snapshot.Hash, "files", len(snapshot.Manifest))

	return &snapshot, nil
}

// frozenConfig 是一次读取的全部产物（Snapshot 的内部形态）：段一、Profile、原文与清单
// 出自同一份字节，故它们只可能在同一个函数里被组装（四条一起走，不得只走其中一条）。
// frozenConfig is everything one read produced — the four fields are inseparable.
type frozenConfig struct {
	raw      []byte // AGENT.md 原文（含 frontmatter——content 列存的就是它）
	profile  *core.Profile
	section  string // 段一正文（本次运行注入的内容）
	manifest []core.ManifestEntry
}

// freeze 读一次盘并产出快照的全部素材：AGENT.md（原文 + Profile + 正文）→ 技能卡片
// （工作副本或发布版本，由 published 选择）→ Bootstrap 文件。读取顺序 = 段一拼装顺序 =
// 清单记账顺序（清单在 Snapshot 里再按路径排序）。
// freeze performs the single read behind Snapshot and section one; published selects
// the card source (nil = work copy).
func (c *ContextLoader) freeze(root string, entry core.AgentEntry, now time.Time, published PublishedSkills) (frozen *frozenConfig, err error) {
	if entry.Profile == nil || !entry.Registered {
		// 未注册的 Agent 不得组装系统提示词、也不得冻结快照（契约 §2.1「整只不注册」
		// 的执行面，§2.4 注 3）。
		return nil, fmt.Errorf("未注册的 Agent %s，不得组装系统提示词", entry.Name)
	}

	base, err := os.OpenRoot(root) // 全部读取经根约束（含越界软连接的拦截）
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	// 快照字节取**准入条目携带的已校验字节**（entry.Definition）：注册表与本次快照因此是
	// 同一份配置——准入之后磁盘若被合法改写（尚未发布 / 热加载未替换）或被改坏（watcher
	// 拒绝、注册表保留旧条目），回读都会让清单与实际模型调用分叉（review ② 缺陷 1）。
	// 手工构造的条目（无 Definition）才退回读盘。
	// The snapshot freezes the very bytes admission validated; only hand-built entries
	// fall back to the disk.
	raw := entry.Definition
	if raw == nil {
		defPath := agentDefinitionPath(entry.Name)
		var readErr error
		raw, readErr = base.ReadFile(filepath.FromSlash(defPath))
		if readErr != nil {
			// 装载后消失 / 不可读 → 报错**点名路径**，不得静默省略该节。
			return nil, fmt.Errorf("读取 Agent 正文 %s: %w", defPath, readErr)
		}
	}
	return c.freezeBytes(base, root, entry.Name, raw, now, published)
}

// freezeBytes 从**已读入的候选字节**产出快照素材：内容条件写的装载期闸门
// （AgentLoader.ValidateCandidate）与运行期快照调用同一段组装实现——段一正文、技能卡片
// 与清单不可能在两条路径之间分叉。base 必须是 root 上的 os.Root（读取不得越界）。
// freezeBytes assembles the frozen material from already-read candidate bytes, so the
// candidate-validation path and the run-snapshot path can never diverge.
func (c *ContextLoader) freezeBytes(base *os.Root, root, name string, raw []byte, now time.Time, published PublishedSkills) (*frozenConfig, error) {
	defPath := agentDefinitionPath(name)
	// **同一份字节**解 Profile（不得读第二遍——陷阱 39）：运行期传进来的是准入校验过的
	// entry.Definition，候选路径传进来的就是候选字节——Profile、段一正文与清单哈希因此
	// 不可能与"校验时那份配置"分叉（review ② 缺陷 1 的修复点）。
	// 未知键的 warns 在此丢弃：装载期（agent.go 的 loadOne）已逐条 WARN 报过同一份文件
	// 的同样的键，而冻结是**每次运行**都会发生的读取——重复落 WARN 只会淹掉日志。
	profile, _, err := parseFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("读取 Agent 正文 %s: %s", defPath, frontmatterErrorSummary(err))
	}
	// 段一素材是**围栏外正文**（data-model.md 实体总览：围栏内 → frontmatterDTO，
	// 围栏外正文 → 段一素材之一）——frontmatter 不进系统提示词。
	_, body, err := splitFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("读取 Agent 正文 %s: %s", defPath, frontmatterErrorSummary(err))
	}

	// 技能卡片按来源读取：工作副本经**技能库根**（与装载期同一处约束——越界声明读不到
	// 技能库之外的文件）；发布版本经准入固定的发布目录（约束 31）。两条来源读不到都报错
	// （不得回落空卡片、不得在两条来源之间回落）。
	cards, err := c.skillCards(root, base, profile.Skills, published)
	if err != nil {
		return nil, err
	}

	// 清单从 AGENT.md 起记：**注入面读到的每一个文件**都落一条 {path, sha256}——技术方案
	// §11.5 的三类来源（AGENT.md + bootstrap 文件 + 绑定技能的 SKILL.md）在段一里就是
	// 全部被读取的文件。L2/L3 技能正文与 scripts/ 不在注入面，故不进清单（按需读取、
	// 读时为准）。
	manifest := []core.ManifestEntry{{Path: defPath, SHA256: sha256Hex(raw)}}
	for i := range cards {
		// 技能条按**声明名**（目录名）记路径：frontmatter 的 name 允许覆盖卡片显示名，
		// 清单记的是"这次读了哪个文件"。
		manifest = append(manifest, core.ManifestEntry{
			Path:   cards[i].relativePath,
			SHA256: sha256Hex(cards[i].raw),
		})
	}

	var b strings.Builder
	if profile.Identity.Prompt != "" {
		b.WriteString(profile.Identity.Prompt + "\n\n") // 「拼在系统提示词开头」（契约 §2）
	}
	b.WriteString("你的名字是「" + profile.Identity.AgentName + "」。\n\n")
	b.WriteString(string(body))
	for _, f := range profile.Bootstrap {
		content, err := base.ReadFile(filepath.FromSlash(f))
		if err != nil {
			return nil, fmt.Errorf("读取 bootstrap 文件 %s: %w", f, err) // 不得静默省略
		}
		b.WriteString("\n\n## " + f + "\n\n" + string(content))
		manifest = append(manifest, core.ManifestEntry{Path: f, SHA256: sha256Hex(content)})
	}
	if len(cards) > 0 {
		b.WriteString("\n\n## 技能\n")
		for _, card := range cards {
			// 卡片三要素：name + description + 技能库真路径（技术方案 §3.4.1）——
			// 只给卡片，正文留给模型在 L2 用 read_file 自取（渐进披露）。
			// 渲染与 l1_hash 计算共用 skillCardLine（约束 32：同一份字节）。
			b.WriteString(skillCardLine(card.name, card.description, card.path))
		}
	}
	b.WriteString("\n\n当前时间：" + now.Format("2006-01-02 15:04:05 -07:00"))

	// Debug 级留痕：段一无状态，每次调用重读——这行让「拼了几次、拼进了几张卡片」
	// 可核对（可观测性双轨；错误已由返回值点名，故不落 WARN）。
	c.logger.Debug("段一已组装", "agent", name, "skills", len(cards), "bytes", b.Len())

	return &frozenConfig{raw: raw, profile: profile, section: b.String(), manifest: manifest}, nil
}

// skillCards 是快照的卡片来源（唯一分叉点）：published 为 nil 时读工作副本（US-3 起的
// 既有口径：经技能库根、缺实体即报错），非 nil 时读准入固定的发布版本目录。
// 两条来源共用同一段"读不到即报错"的语义，**不得**互相回落。
// skillCards is the single branch between the work-copy and published card sources.
func (c *ContextLoader) skillCards(root string, base *os.Root, declared []string, published PublishedSkills) (cards []skillCard, err error) {
	if published != nil {
		return publishedCards(base, declared, published)
	}
	lib, err := openSkillsRoot(root)
	if err != nil {
		return nil, err
	}
	if lib != nil {
		defer func() {
			if closeErr := lib.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("关闭技能库目录 %s: %w", skillsDirName, closeErr))
			}
		}()
	}
	return cardsFor(lib, declared) // SKILL.md 读不到 → 报错（不得回落空卡片）
}

// publishedCards 按声明顺序读发布版本卡片（**不去重**：与工作副本同口径，声明两次出两张）。
// 每个声明技能都必须有目录映射；缺映射、目录形态不符、SKILL.md 读不到，一律失败关闭。
// publishedCards reads one card per declaration from the pinned published directories.
func publishedCards(base *os.Root, declared []string, skills PublishedSkills) ([]skillCard, error) {
	cards := make([]skillCard, 0, len(declared))
	for _, name := range declared {
		dir, ok := skills[name]
		if !ok || strings.TrimSpace(dir) == "" {
			return nil, fmt.Errorf("技能 %s 缺少发布目录（准入未固定发布版本），拒绝组装配置快照", name)
		}
		card, err := publishedCardFor(base, name, dir)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}
	return cards, nil
}

// publishedCardFor 读一个发布版本的 SKILL.md 并产出卡片：目录必须是持久化 storage_path
// 的契约形态（身份分段 `.basalt/versions/skills/<name>/<uid>/<no>` 或旧扁平
// `.basalt/versions/skills/<name>/<no>`，版本号为正整数，且属于声明技能），读取经工作区
// 根句柄——发布目录之外（含工作副本）一律读不到。卡片行展示记录里的发布真路径，清单条
// 记逻辑路径。
// publishedCardFor reads one published SKILL.md after validating its recorded literal.
func publishedCardFor(base *os.Root, name, dir string) (skillCard, error) {
	if !validSkillName(name) {
		return skillCard{}, fmt.Errorf("技能名不是单个目录名，拒绝读取发布目录外的定义：%q", name)
	}
	target, err := parseSkillStoragePath(dir)
	if err != nil {
		return skillCard{}, fmt.Errorf("技能 %s 的发布目录不可用：%w", name, err)
	}
	if target.name != name {
		return skillCard{}, fmt.Errorf("技能 %s 的发布目录 %q 属于技能 %s，拒绝读取", name, dir, target.name)
	}
	cardPath := publishedCardPath(target.logicalDir)
	raw, err := base.ReadFile(filepath.FromSlash(path.Join(target.relDir, skillDefinitionFile)))
	if err != nil {
		return skillCard{}, fmt.Errorf("读取技能定义 %s: %w", cardPath, err)
	}

	card := skillCard{
		name: name, // 缺省 = 技能声明名（frontmatter name 可覆盖显示名，读取路径不受影响）
		// L1 卡片行展示发布版本真路径（约束 31 的字面量）。
		path: cardPath,
		// 清单仍记逻辑路径：逻辑路径只用于快照清单与展示映射，运行期读取只认发布目录。
		relativePath: path.Join(skillLibraryPrefix, name, skillDefinitionFile),
		raw:          raw,
	}
	cardName, description, err := skillCardFields(raw, name)
	if err != nil {
		return skillCard{}, fmt.Errorf("解析技能定义 %s: %s", cardPath, frontmatterErrorSummary(err))
	}
	card.name = cardName
	card.description = description
	return card, nil
}

// sha256Hex 给出字节的 sha256 十六进制小写（64 字符）——清单条与快照哈希共用同一形态。
// sha256Hex renders the digest shared by the manifest rows and the snapshot hash.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
