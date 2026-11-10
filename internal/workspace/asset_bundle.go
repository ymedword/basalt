package workspace

// 本文件是 bundle 形态的注册条目构建（US-25C §3.1.4 `loader.EntryFromBundle`）：
// Worker 不读工作区的 `agents/` 目录，而是从**已安装的不可变 bundle 目录**读定义，
// 并按本轮资源准备的 `prepared` 快照判定 Provider / MCP / 工具名。判定规则与工作区
// 路径逐条共用 `validateProfileWith`——两条路径各写一份校验必然会漂移。
//
// 技能实体按**已物化的发布目录**判定：`published` 是技能名 → 工作区相对发布目录
// （`versions/skills/<name>/<uid>/<no>`），不是工作副本 `skills/<name>`——发布版本
// 才是运行期读取面（FR-25C-07「不得回落到工作副本」）。
// This file builds registration entries from an installed, immutable bundle. The
// validation rules are shared with the workspace path through validateProfileWith,
// and declared skills resolve against materialized published directories only.

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
)

// PreparedResources 是本轮资源准备输出的**只读快照**（US-25C §3.1.4 C14）：
// `EntryFromBundle` 的判定只认它，不再经 `ProvidersFunc` / `ConnectedNames()` /
// `registry.Names()` 读一次实时目录——否则"本轮准备成功的资源"与"Entry 判定看到的
// 资源"可以不一致，缺项会被误报成 entry 非法而不是点名缺哪个资源。
// PreparedResources is the read-only snapshot produced by one round of resource
// preparation; the bundle entry validation reads nothing else.
type PreparedResources struct {
	// Providers 是本轮**构建成功**的已发布模型连接（升序，名集）。
	Providers []string
	// MCPConfigured 是已 configured 的 server 名；MCPConnected 只含**已连接**项。
	MCPConfigured []string
	MCPConnected  []string
	// Tools 是本轮进程内注册池的名字快照（MCP 连接成功后才进入）。
	Tools []string
}

// EntryFromBundle 从已安装的 bundle 构建一只 Agent 的注册条目：定义根取
// `<installed.Dir>/agent`（bundle 布局是工作区根的投影，定义文件仍是
// `agents/<name>/AGENT.md`），技能判定面是已物化的发布目录，名字集只读 `prepared`。
// 失败返回 `*core.AgentDefinitionRejectedError`，Kind / Detail 与工作区装载同名同义，
// 同步层据此点名阻塞原因。
// EntryFromBundle builds one registration entry from an installed bundle; every
// failure carries the same typed rejection the workspace loader reports.
func (l *AgentLoader) EntryFromBundle(root string, installed assets.Installed, prepared PreparedResources, published map[string]string) (core.AgentEntry, error) {
	name := installed.Manifest.Agent
	if strings.TrimSpace(name) == "" {
		return core.AgentEntry{}, fmt.Errorf("bundle 清单缺少 agent 名，无法构建注册条目")
	}
	if installed.Dir == "" {
		return core.AgentEntry{Name: name}, fmt.Errorf("bundle %s（%s）缺少安装目录", name, installed.Manifest.BundleHash)
	}

	// 定义根 = bundle 内的配置布局段（assets 包是布局前缀的唯一来源，本文件不拼字面量）。
	defRoot := filepath.Join(installed.Dir, filepath.FromSlash(assets.ConfigBundleRel("")))
	base, err := os.OpenRoot(defRoot)
	if err != nil {
		return core.AgentEntry{Name: name}, fmt.Errorf("打开 bundle 定义根 %s: %w", defRoot, err)
	}
	defer func() { _ = base.Close() }()

	defPath := agentDefinitionPath(name)
	raw, err := base.ReadFile(filepath.FromSlash(defPath))
	if err != nil {
		return core.AgentEntry{Name: name}, &core.AgentDefinitionRejectedError{
			Kind:   core.AgentRejectReadFailed,
			Detail: defPath,
			Cause:  err,
		}
	}
	profile, warns, err := parseFrontmatter(raw)
	for _, w := range warns {
		// 未知键只 WARN 不拒载：detail 只放键路径、不打印值（与 loadOne 同口径）。
		l.logger.Warn("frontmatter 含未知键——已忽略", "agent", name,
			"reason", "unknown frontmatter key", "detail", w)
	}
	if err != nil {
		kind := core.AgentRejectInvalidFrontmatter
		detail := frontmatterErrorSummary(err)
		if errors.Is(err, core.ErrAgentResourceBindingInvalid) {
			kind = core.AgentRejectInvalidResourceBindings
			detail = "块结构非法"
		}
		if errors.Is(err, core.ErrAgentKnowledgeBindingInvalid) {
			kind = core.AgentRejectInvalidKnowledge
			detail = "块结构非法"
		}
		return core.AgentEntry{Name: name}, &core.AgentDefinitionRejectedError{
			Kind:   kind,
			Detail: detail,
			Cause:  err,
		}
	}

	entry := core.AgentEntry{Name: name, Profile: profile, Definition: slices.Clone(raw)}

	skillOK, closeSkills, err := publishedSkillResolver(root, profile.Skills, published)
	if err != nil {
		return entry, err
	}
	defer closeSkills()

	rej, deferred := l.validateProfileWith(base, name, profile, validationNames{
		providers:    prepared.Providers,
		tools:        prepared.Tools,
		mcpServers:   prepared.MCPConfigured,
		mcpConnected: prepared.MCPConnected,
	}, skillOK, map[string]string{})
	if rej.kind != "" {
		return entry, &core.AgentDefinitionRejectedError{Kind: rej.kind, Detail: rej.detail}
	}
	if deferred {
		// 连接结果未知（prepared 未给出已连接名单）时不得把条目当成已注册：
		// bundle 路径没有"待确认"第三态，只按失败关闭处理。
		return entry, &core.AgentDefinitionRejectedError{
			Kind:   core.AgentRejectMCPServerSkipped,
			Detail: "本轮资源准备未提供已连接 MCP 名单",
		}
	}
	entry.Registered = true
	return entry, nil
}

// publishedSkillResolver 构造 bundle 路径的技能判定面：技能名必须出现在 `published`
// 里，且该发布目录下的 SKILL.md 是普通文件。`skills` 为空时**不打开**工作区根
// （没有声明就没有判定，避免无谓的目录句柄）。
// publishedSkillResolver resolves declared skills against materialized publications only.
func publishedSkillResolver(root string, skills []string, published map[string]string) (skillResolver, func(), error) {
	noop := func() {}
	if len(skills) == 0 {
		return func(string) bool { return true }, noop, nil
	}
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, noop, fmt.Errorf("打开工作区目录 %s 以判定已物化技能: %w", root, err)
	}
	resolver := func(skillName string) bool {
		if !validSkillName(skillName) {
			return false
		}
		dir, ok := published[skillName]
		if !ok || strings.TrimSpace(dir) == "" {
			return false
		}
		info, statErr := base.Stat(filepath.FromSlash(path.Join(dir, skillDefinitionFile)))
		return statErr == nil && info.Mode().IsRegular()
	}
	return resolver, func() { _ = base.Close() }, nil
}
