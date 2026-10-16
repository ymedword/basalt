package workspace

// 本文件是系统提示词**段一**的拼装：identity.prompt（若配置）→ 自我称谓 →
// AGENT.md 正文 → Bootstrap 文件（声明顺序）→ 技能卡片 L1 → 末尾当前时间
// （技术方案 §4.2「段 1 组合式」/ §8.3）。本 US 只交付段一——其余三段、运行级
// 快照冻结与清单哈希归 US-4（§2.2 约束 7；注入内容与哈希必须共享同次读取产物，
// 故本接口只管**读并拼**，不在返回后二次读盘）。
// This file assembles section one of the system prompt: identity, self-name, agent
// body, bootstrap files, L1 skill cards, and the closing timestamp.

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// ContextLoader 组装系统提示词段一（技术方案 §8.3：本 US 只交付段一；快照与其余三段归 US-4）。
// ContextLoader assembles section one of the system prompt; it holds no state
// beyond its logger — every call re-reads its sources.
type ContextLoader struct {
	logger *slog.Logger
}

// NewContextLoader 构造段一装配器；nil Logger 回落 slog.Default（与装载器同一口径）。
// NewContextLoader builds the section-one assembler.
func NewContextLoader(logger *slog.Logger) *ContextLoader {
	if logger == nil {
		logger = slog.Default()
	}
	return &ContextLoader{logger: logger}
}

// SectionOne 组装段一：identity.prompt + 自我称谓 + AGENT.md 正文 + Bootstrap 文件 +
// 技能卡片 L1 + 当前日期时间（格式 2006-01-02 15:04:05 -07:00——缺省 UTC、含偏移，§2.4 注 10）。
// now 显式传入；每次调用重读文件（无缓存——§2.2 约束 7）；任一步读取失败 → 返回错误（§2.2 约束 6）。
// SectionOne assembles the first section. It is a stateless primitive: the caller
// passes the clock, and every source is re-read on each call.
func (c *ContextLoader) SectionOne(root string, entry core.AgentEntry, now time.Time) (section string, err error) {
	if entry.Profile == nil || !entry.Registered {
		// 未注册的 Agent 不得组装系统提示词（契约 §2.1「整只不注册」的执行面，§2.4 注 3）。
		return "", fmt.Errorf("未注册的 Agent %s，不得组装系统提示词", entry.Name)
	}

	base, err := os.OpenRoot(root) // 全部读取经根约束（含越界软连接的拦截）
	if err != nil {
		return "", fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	profile := entry.Profile

	defPath := agentDefinitionPath(entry.Name)
	raw, err := base.ReadFile(filepath.FromSlash(defPath))
	if err != nil {
		// 装载后消失 / 不可读 → 报错**点名路径**，不得静默省略该节。
		return "", fmt.Errorf("读取 Agent 正文 %s: %w", defPath, err)
	}
	// 段一素材是**围栏外正文**（data-model.md 实体总览：围栏内 → frontmatterDTO，
	// 围栏外正文 → 段一素材之一）——frontmatter 不进系统提示词。
	_, body, err := splitFrontmatter(raw)
	if err != nil {
		return "", fmt.Errorf("读取 Agent 正文 %s: %s", defPath, frontmatterErrorSummary(err))
	}

	// 技能卡片经**技能库根**读取（与装载期同一处约束——越界声明读不到技能库之外的文件）。
	lib, err := openSkillsRoot(root)
	if err != nil {
		return "", err
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
		return "", err
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
			return "", fmt.Errorf("读取 bootstrap 文件 %s: %w", f, err) // 不得静默省略
		}
		b.WriteString("\n\n## " + f + "\n\n" + string(content))
	}
	if len(cards) > 0 {
		b.WriteString("\n\n## 技能\n")
		for _, card := range cards {
			// 卡片三要素：name + description + 技能库真路径（技术方案 §11.3）——
			// 只给卡片，正文留给模型在 L2 用 read_file 自取（渐进披露）。
			b.WriteString("- " + card.name + "：" + card.description + "（" + card.path + "）\n")
		}
	}
	b.WriteString("\n\n当前时间：" + now.Format("2006-01-02 15:04:05 -07:00"))

	// Debug 级留痕：段一无状态，每次调用重读——这行让「拼了几次、拼进了几张卡片」
	// 可核对（可观测性双轨；错误已由返回值点名，故不落 WARN）。
	c.logger.Debug("段一已组装", "agent", entry.Name, "skills", len(cards), "bytes", b.Len())
	return b.String(), nil
}
