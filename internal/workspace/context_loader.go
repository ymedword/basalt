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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
func (c *ContextLoader) Snapshot(root string, entry core.AgentEntry, now time.Time) (*core.ConfigSnapshot, error) {
	frozen, err := c.freeze(root, entry, now)
	if err != nil {
		return nil, err
	}

	// 规整序列化的要件之一是**确定的顺序**：清单按路径排序（声明顺序不同、内容相同的
	// 两份配置必须得到同一个哈希），`manifest_json` 与 `Hash` 也就此同源。重复路径不合并
	//（声明两次出两条清单条——与卡片"不去重"同口径，"读即所记"记的是读取行为）。
	slices.SortFunc(frozen.manifest, func(a, b core.ManifestEntry) int {
		return strings.Compare(a.Path, b.Path)
	})
	manifestJSON, err := json.Marshal(frozen.manifest)
	if err != nil {
		// 元素全是字符串字段，Marshal 实际不会失败；仍如实上抛，不吞。
		return nil, fmt.Errorf("序列化冻结清单（agent=%s）: %w", entry.Name, err)
	}
	hash := sha256Hex(manifestJSON)

	// 留痕只到「条数」为止——**不得**回显文件全文或配置原文（FR-027 可观测性双轨）。
	c.logger.Debug("配置快照已冻结", "agent", entry.Name, "hash", hash, "files", len(frozen.manifest))

	return &core.ConfigSnapshot{
		AgentName:  entry.Name,
		Hash:       hash,
		Content:    string(frozen.raw),
		Manifest:   frozen.manifest,
		SectionOne: frozen.section,
		Profile:    frozen.profile,
	}, nil
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
// （经技能库根）→ Bootstrap 文件。读取顺序 = 段一拼装顺序 = 清单记账顺序（清单在
// Snapshot 里再按路径排序）。
// freeze performs the single read behind Snapshot and section one.
func (c *ContextLoader) freeze(root string, entry core.AgentEntry, now time.Time) (frozen *frozenConfig, err error) {
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

	defPath := agentDefinitionPath(entry.Name)
	raw, err := base.ReadFile(filepath.FromSlash(defPath))
	if err != nil {
		// 装载后消失 / 不可读 → 报错**点名路径**，不得静默省略该节。
		return nil, fmt.Errorf("读取 Agent 正文 %s: %w", defPath, err)
	}
	// **同一份字节**解 Profile（不得读第二遍——陷阱 39；也不得用装载期缓存的
	// entry.Profile：装载与运行之间隔了多久、磁盘改没改，只有这次读取说了算）。
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

	// 技能卡片经**技能库根**读取（与装载期同一处约束——越界声明读不到技能库之外的文件）。
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

	cards, err := cardsFor(lib, profile.Skills) // SKILL.md 读不到 → 报错（不得回落空卡片）
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
			b.WriteString("- " + card.name + "：" + card.description + "（" + card.path + "）\n")
		}
	}
	b.WriteString("\n\n当前时间：" + now.Format("2006-01-02 15:04:05 -07:00"))

	// Debug 级留痕：段一无状态，每次调用重读——这行让「拼了几次、拼进了几张卡片」
	// 可核对（可观测性双轨；错误已由返回值点名，故不落 WARN）。
	c.logger.Debug("段一已组装", "agent", entry.Name, "skills", len(cards), "bytes", b.Len())

	return &frozenConfig{raw: raw, profile: profile, section: b.String(), manifest: manifest}, nil
}

// sha256Hex 给出字节的 sha256 十六进制小写（64 字符）——清单条与快照哈希共用同一形态。
// sha256Hex renders the digest shared by the manifest rows and the snapshot hash.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
