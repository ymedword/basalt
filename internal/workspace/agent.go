package workspace

// 本文件是「一个目录 = 一个 Agent」的装载主体：扫描 .basalt/agents/ 的每个子目录，
// 解析 AGENT.md 的围栏 frontmatter，执行八条加载期校验（规则 4 的工具门禁自 US-4 起
// 执行、规则 6 的「连接成功」半条随 US-7，其余全量），产出 core.AgentEntry 列表。
// 单只失败不拖垮整批——失败进条目（Registered=false）并记一条点名 WARN，顶层 error
// 只留给「工作区结构不可读」。
// This file loads agent directories: it parses the fenced frontmatter of AGENT.md,
// runs the load-time checks, and returns one entry per directory. A single failure
// never blocks the others; the top-level error is reserved for an unreadable
// workspace structure.

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/robfig/cron/v3"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// agentsDirName / agentDefinitionFile 是加载单元的路径字面量（契约 §1）：
	// .basalt/agents/<name>/AGENT.md——AGENT.md 是唯一定义入口。
	agentsDirName       = "agents"
	agentDefinitionFile = "AGENT.md"

	// mcpServersFile 是规则 6 的判据来源：顶层键名集即「已配置的 server 名」。
	mcpServersFile = "mcp_servers.yaml"

	// defaultZone 是 schedule 缺省时区（契约 §3「zone 缺省 UTC」）——不得回落进程本地时区。
	defaultZone = "UTC"

	// 两个运行参数上限的缺省值（契约 §2 字段表缺省列）；显式 0 必须原样保留。
	defaultMaxIterations   = 10
	defaultMaxHistoryTurns = 20

	// 6 段 cron 的段数与下标（秒 分 时 日 月 周）：日字段与周字段是「不得同时受限」
	// 与「周字段 7 拒绝」两条子集规则的判定位置。
	cronFieldCount = 6
	cronDomIndex   = 3
	cronDowIndex   = 5

	// cronTimezonePrefixRule 是「表达式自带时区前缀」的**统一**拒绝句：装载期
	//（validateScheduleEntry）与出口（CronSpec）共用同一条常量——两处判定分家正是
	// 双前缀缺陷的根因（R2 评审修复：以 zone 字段为唯一时区来源）。
	cronTimezonePrefixRule = "cron 内不得自带时区前缀（TZ= / CRON_TZ=）——时区一律由 zone 字段声明"
)

// cronParser 是本仓的 6 段解析器（秒 分 时 日 月 周）——5 段制式一律拒绝（契约 §3.1）。
// 无状态、不可变，故可安全共享（不得用包级变量承载业务状态——宪法 VII）。
// The shared six-field parser; it is stateless and therefore safe to share.
var cronParser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// 本仓自产的解析错误（无库文案，可原样进对外文案）：围栏切分与顶层形态由我们判定，
// 其余 YAML 错误一律经 frontmatterErrorSummary 压成「位置 + 自产类别」。
var (
	errNoFrontmatterFence    = errors.New("缺少 frontmatter 围栏——AGENT.md 必须以 --- 开头")
	errOpenFrontmatterFence  = errors.New("frontmatter 围栏未闭合——缺少收尾的 --- 行")
	errFrontmatterNotMapping = errors.New("frontmatter 顶层必须是映射（键值对）")
)

// AgentLoaderOptions 是装载器的显式依赖面（宪法 VII：不塞 context、不用包级变量）。
// AgentLoaderOptions carries the loader's explicit dependencies.
type AgentLoaderOptions struct {
	// Providers 是已注册 provider 名（由调用方经 LoadConfig 取得后注入——装载器
	// 不得自己再读一次配置：同一文件两次读取正是陷阱 39 的根因形态）。
	Providers []string

	// Tools 是已注册工具名（注册池名字快照）：装配期从工具注册表导出后注入，
	// workspace 因此**不**反向依赖 tool 包（§2.4 注 4）。零值 = **空池**——未声明
	// `tools:` 的 Agent 照常注册，声明了 `tools:` 的 Agent 整只不注册。
	Tools []string

	// Logger 是点名 WARN 的出口；nil 时用 slog.Default。
	Logger *slog.Logger
}

// AgentLoader 扫描 .basalt/agents/ 的每个子目录：解析 → 校验 → 派生 Profile / 拒绝。
// AgentLoader turns every agent directory into one AgentEntry, registered or not.
type AgentLoader struct {
	providers []string
	tools     []string
	logger    *slog.Logger
}

// NewAgentLoader 构造装载器；provider 与工具名集切片在构造时各拷贝一份，调用方
// 后续改动不影响判定（判定输入一旦给定就冻结）。
// NewAgentLoader snapshots the injected provider and tool names.
func NewAgentLoader(opts AgentLoaderOptions) *AgentLoader {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentLoader{
		providers: slices.Clone(opts.Providers),
		tools:     slices.Clone(opts.Tools),
		logger:    logger,
	}
}

// LoadAll 装载全部 Agent 目录；单只失败不拖垮整批（其余照常）。不提供 LoadOne——
// show 复用本结果过滤（技术方案 §8.7「不得为同一语义写两份实现」）。
// LoadAll never fails because of one bad agent directory: the top-level error is
// reserved for an unreadable workspace structure.
//
// 扫描与读取一律经 os.Root（含目录扫描——软连接越界在根上被挡）；扫描序**显式**
// 按名字节序排序：「先到先得」的调度 id 占用必须以确定序为前提。
// Scanning and reading are confined to the workspace root, and the scan order is
// sorted explicitly because "first come first served" presupposes a fixed order.
func (l *AgentLoader) LoadAll(root string) (entries []core.AgentEntry, err error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	// 技能库根：技能实体的查找只在本根内进行——只约束在工作区根内不够，越界声明
	//（skills: [../agents/private]）会读到技能库之外的文件（见 skill.go 的说明）。
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

	// agents/ 缺失是装载错误（≠ "零只 Agent"）：目录不存在时"没有 Agent"与
	// "工作区没建好"无法区分，后者才是真相，命令面据此点名并指引 basalt init。
	dir, err := base.Open(agentsDirName)
	if err != nil {
		return nil, fmt.Errorf("打开 Agent 目录: %w", err)
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭 Agent 目录: %w", closeErr))
		}
	}()

	dirs, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("读取 Agent 目录: %w", err)
	}
	// 按名字节序显式排序（不得依赖库的隐式序）。
	slices.SortFunc(dirs, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

	mcpNames, err := readMCPServerNames(base)
	if err != nil {
		return nil, err
	}

	claim := make(map[string]string) // 调度 id → 占用者；只由完整校验通过的 Agent 提交
	entries = make([]core.AgentEntry, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() {
			continue // 非目录 / 软连接条目如实忽略（附属物不得成为第二入口）
		}
		entry, rej := l.loadOne(base, lib, d.Name(), mcpNames, claim)
		if rej.reason == "" {
			// **整只提交**：本行是调度 id 占用的唯一提交点（§2.4 注 6 的 R1 裁决）。
			for id, owner := range rej.claimed {
				claim[id] = owner
			}
			entry.Registered = true
		} else {
			entry.RejectReason = rej.mergedReason()
			// 固定键：agent / reason / detail（reason 取契约字面量原样）。
			l.logger.Warn("Agent 未注册", "agent", entry.Name,
				"reason", rej.reason, "detail", rej.detail)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// reject 是加载失败的内部形态（非导出）：reason 取契约字面量原样或自描述短语，
// detail 放点名细节。两者分开是因为日志要**分键**（agent / reason / detail），
// 而 AgentEntry.RejectReason 给人读（mergedReason）。
// reject is the internal failure shape: reason is the contract literal, detail the
// pointer, and LogReason merges the two for human eyes.
//
// claimed 是本只 Agent 的调度 id 占用集——只有"reason 为空"（全过）时非零，
// LoadAll 据此提交；被拒时为零值（本只的占用一律不入表）。
type reject struct {
	reason  string            // name mismatch / blank model / skill not found: <n> / invalid schedule / …
	detail  string            // 键路径 / 文件路径 / 任务 id / cron 原文——契约字面量含名字时不重复填
	claimed map[string]string // 任务 id → 占用者（本只）；被拒时为零值
}

// mergedReason 人读合并：detail 为空 → reason；否则 "reason: detail"
// （契约字面量原样在前——用例断言一律用 Contains 判前缀）。
// mergedReason joins reason and detail for terminal output.
func (r reject) mergedReason() string {
	if r.detail == "" {
		return r.reason
	}
	return r.reason + ": " + r.detail
}

// loadOne 装载单只 Agent 目录：读取 → 解析 → 八条校验 → 拒绝或返回占用集。
// base 是工作区根（定义文件 / bootstrap 文件的读取面），lib 是技能库根（技能实体的
// 查找面）——两个根分开传，技能查找**不得**拿到工作区根。
// loadOne loads a single agent directory and reports its first failure, if any.
func (l *AgentLoader) loadOne(base, lib *os.Root, name string, mcpNames []string, claim map[string]string) (core.AgentEntry, reject) {
	entry := core.AgentEntry{Name: name}

	defPath := agentDefinitionPath(name)
	raw, err := base.ReadFile(filepath.FromSlash(defPath))
	if err != nil {
		return entry, reject{reason: "read AGENT.md failed", detail: defPath}
	}

	// 同一份字节：先未知键扫描、再解 DTO（不得把文件读两遍——陷阱 39）。
	profile, warns, err := parseFrontmatter(raw)
	for _, w := range warns {
		// 未知键不拒载，逐条 WARN：detail 只放键路径、**不打印值**（值是我们还没
		// 理解的输入，落进日志等于把未审计内容写进审计外的第二份记录）。
		l.logger.Warn("frontmatter 含未知键——已忽略", "agent", name,
			"reason", "unknown frontmatter key", "detail", w)
	}
	if err != nil {
		// 解析 / 解码失败：位置 + 自产类别，库文案一个字不外传（§2.4 注 12）。
		return entry, reject{reason: "invalid frontmatter", detail: frontmatterErrorSummary(err)}
	}
	entry.Profile = profile // 解析成功即有 Profile（即使校验未过——供 list / show 展示声明面）

	l.warnLocalSkillsDir(base, name)

	if profile.Name != name { // 规则 1
		return entry, reject{
			reason: "name mismatch",
			detail: fmt.Sprintf("目录名 %s 与 frontmatter name %s 不一致", name, profile.Name),
		}
	}
	if !slices.Contains(l.providers, profile.Provider.Name) { // 规则 2
		return entry, reject{reason: "provider not registered: " + profile.Provider.Name}
	}
	if strings.TrimSpace(profile.Provider.Model) == "" { // 规则 3（占位文本是普通文本，天然放行）
		return entry, reject{reason: "blank model"}
	}
	if t := profile.Provider.Temperature; t != nil && !validTemperature(*t) { // 契约 §2 字段约束
		return entry, reject{
			reason: "invalid provider.temperature",
			detail: "已声明的 temperature 必须落在 0–2 且有限，实际 " + strconv.FormatFloat(*t, 'g', -1, 64),
		}
	}
	// 规则 4（tools 全部可解析）自 US-4 起**执行**：声明的名字必须落在注册池内，
	// 缺一即整只不注册并点名（§2.4 注 4）。报的是**声明序**里的第一个缺名——遍历
	// 按声明序而非池序，判卷才与"池的遍历顺序随机"无关。空池（未注入名字集）下
	// 未声明 `tools:` 的 Agent 照常注册：空声明列表本来就无名字可判。
	for _, toolName := range profile.Tools {
		if !slices.Contains(l.tools, toolName) {
			return entry, reject{reason: "tool not registered: " + toolName}
		}
	}
	if missing := missingSkill(lib, profile.Skills); missing != "" { // 规则 5
		return entry, reject{reason: "skill not found: " + missing}
	}
	for _, m := range profile.MCPServers { // 规则 6（存在性半条；「连接成功」半条随 US-7）
		if !slices.Contains(mcpNames, m) {
			return entry, reject{reason: "mcp server skipped", detail: m}
		}
	}
	for _, b := range profile.Bootstrap { // 规则 7
		if detail, ok := checkBootstrapEntry(base, b); !ok {
			return entry, reject{reason: "bootstrap file missing", detail: detail}
		}
	}
	claimed, err := validateSchedules(profile.Name, profile.Schedules, claim) // 规则 8
	if err != nil {
		return entry, reject{reason: "invalid schedule", detail: err.Error()}
	}
	return entry, reject{claimed: claimed}
}

// warnLocalSkillsDir 对「Agent 本地 skills/ 目录存在」记一条 WARN（遗留形态备案）：
// 技能绑定**只**由 frontmatter skills: 声明（声明即绑定），本地目录被无视且不改变
// 判定结果——它存在的意义只是告诉运维"这份 Agent 用的是旧形态"。
// warnLocalSkillsDir flags a legacy local skills/ directory without changing any
// binding decision.
//
// 判定落在目录是否存在上（不跟随软连接）；stat 失败按"不存在"处理、只记 Debug：
// 这个提示是诊断旁路，不得因为它让一只 Agent 装载失败。
func (l *AgentLoader) warnLocalSkillsDir(base *os.Root, name string) {
	rel := path.Join(agentsDirName, name, skillsDirName)
	info, err := base.Stat(filepath.FromSlash(rel))
	switch {
	case err == nil && info.IsDir():
		l.logger.Warn("本地 skills/ 目录被无视——技能绑定只由 frontmatter skills: 声明",
			"agent", name, "reason", "local skills directory ignored", "detail", rel)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		l.logger.Debug("检查本地 skills/ 目录失败——按不存在处理",
			"agent", name, "detail", rel, "error", err)
	}
}

// agentDefinitionPath 给出 Agent 定义相对工作区根的路径（agents/<name>/AGENT.md）——
// 装载与段一装配共用同一处字面量，防止两条路径各写一份而漂移。
// agentDefinitionPath is the single source of the agent definition path literal.
func agentDefinitionPath(name string) string {
	return path.Join(agentsDirName, name, agentDefinitionFile)
}

// checkBootstrapEntry 校验 bootstrap 声明的单个条目：存在**且是普通文件**才放行，
// 否则返回该条目的 detail（路径 + 原因）。不存在与不可读同判（契约 §2.1 的字面量集里
// 只有 bootstrap file missing 一条——本 US 不新增 reason 字面量）；**类型不符同样归入
// 该字面量**，由 detail 点名"不是普通文件"：目录此前要拖到段一读取时才炸，装载期就该
// 拒（R2 评审修复）。
// 软链接经 os.Root.Stat 跟随，但**出不了工作区根**（越界在根上被挡）；链接指向根内
// 普通文件时按普通文件放行——读取面（段一）读到的正是同一份内容。
// checkBootstrapEntry requires every declared bootstrap entry to be a regular file;
// unreadable counts as absent, and anything else is named in the detail.
func checkBootstrapEntry(base *os.Root, rel string) (detail string, ok bool) {
	info, err := base.Stat(filepath.FromSlash(rel))
	if err != nil {
		return rel, false
	}
	if info.Mode().IsRegular() {
		return "", true
	}
	return fmt.Sprintf("%s（不是普通文件：%s）", rel, fileKind(info)), false
}

// fileKind 把条目的类型压成一句可读文案（判定一律落在 stat 的**类型位**上，不靠文件名
// 后缀猜）；agent_files.go 的 delete 拒绝文案共用本函数。
// 不用 Mode().Type().String()：它把权限位补成连字符，普通文件会渲染成误导性的
// "----------"（本机实测，Windows 与 os.Root 皆然）；逐个类型位点名才念得通。
// fileKind names an entry's type for operator-facing messages.
func fileKind(info fs.FileInfo) string {
	mode := info.Mode()
	switch {
	case info.IsDir():
		return "目录"
	case mode&os.ModeSymlink != 0:
		return "符号链接"
	case mode.IsRegular():
		return "普通文件"
	case mode&os.ModeNamedPipe != 0:
		return "FIFO（命名管道）"
	case mode&os.ModeDevice != 0:
		return "设备文件"
	case mode&os.ModeSocket != 0:
		return "套接字"
	default:
		return "特殊文件"
	}
}

// readMCPServerNames 读 mcp_servers.yaml 的顶层键名集（文件缺失或空文档 → 空集）。
// readMCPServerNames lists the top-level keys of mcp_servers.yaml; a missing file
// yields an empty set.
//
// 文件存在但不可解析 / 顶层不是映射是**工作区结构问题**（不是单只 Agent 的问题）：
// 与「agents/ 缺失」同类，如实上抛——若静默按空集处理，每只声明了 mcp_servers 的
// Agent 都会以 "mcp server skipped" 拒载，报出的原因与真相不符。
// An unreadable mcp_servers.yaml is a workspace-structure problem and is reported
// as such instead of masquerading as a missing server.
func readMCPServerNames(base *os.Root) ([]string, error) {
	raw, err := base.ReadFile(mcpServersFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s: %w", mcpServersFile, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}

	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("解析 %s: %s", mcpServersFile, frontmatterErrorSummary(err))
	}
	mapping, ok := tree.(map[string]any)
	if !ok {
		if tree == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("%s 的顶层必须是映射（server 名 → 启动配置），实际是 %T", mcpServersFile, tree)
	}

	names := make([]string, 0, len(mapping))
	for name := range mapping {
		names = append(names, name)
	}
	slices.Sort(names) // 排序后返回：调用方之外的读数也稳定可比
	return names, nil
}

// parseFrontmatter 从 AGENT.md 的字节解析 frontmatter：围栏切分 → 未知键扫描 →
// 解码进私有 DTO → 显式映射 + 缺省填充。返回的 warns 是未知键路径集（已排序），
// 即便随后解码失败也一并返回——未知键的可见性不因解码失败而丢失。
// parseFrontmatter parses the fenced frontmatter: it scans unknown keys and decodes
// into the private DTO from the very same bytes, then maps into core.Profile.
func parseFrontmatter(raw []byte) (*core.Profile, []string, error) {
	front, _, err := splitFrontmatter(raw)
	if err != nil {
		return nil, nil, err
	}

	warns, err := scanUnknownKeys(front)
	if err != nil {
		return nil, nil, err
	}

	var dto frontmatterDTO
	if err := yaml.Unmarshal(front, &dto); err != nil {
		return nil, warns, err
	}
	return intoProfile(dto), warns, nil
}

// splitFrontmatter 把 AGENT.md 切成围栏内 frontmatter 与围栏外正文两段
// （契约 §1：AGENT.md = --- 围栏 + 正文）。围栏分隔（--- 之后的空行）不是正文的一部分。
// splitFrontmatter separates the fenced frontmatter from the body; the blank line
// that separates them is not part of the body.
func splitFrontmatter(raw []byte) (front, body []byte, err error) {
	// 容忍 UTF-8 BOM；必须写转义——字面量会触发 Go 扫描器的「illegal byte order mark」。
	text := strings.TrimPrefix(string(raw), "\ufeff")
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return nil, nil, errNoFrontmatterFence
	}

	lines := strings.Split(text[strings.Index(text, "\n")+1:], "\n")
	for i, line := range lines {
		if strings.TrimSuffix(line, "\r") == "---" {
			front = []byte(strings.Join(lines[:i], "\n"))
			body = []byte(strings.Trim(strings.Join(lines[i+1:], "\n"), "\r\n"))
			return front, body, nil
		}
	}
	return nil, nil, errOpenFrontmatterFence
}

// scanUnknownKeys 用同一份字节扫出字段表之外的键路径（已排序）：契约 §2 的字段表
// 之外 → 忽略但可见（逐条 WARN），**不采用**严格模式（Compatibility §2.2 的前向
// 兼容精神：基线先于本仓演进协议，严格模式会把"新增可选项"升级成"双底座不兼容"）。
// scanUnknownKeys collects the key paths that fall outside the contract field table;
// such keys are ignored but visible, never fatal.
func scanUnknownKeys(front []byte) ([]string, error) {
	var tree any
	if err := yaml.Unmarshal(front, &tree); err != nil {
		return nil, err
	}
	mapping, ok := tree.(map[string]any)
	if !ok {
		if tree == nil { // 空 frontmatter：无未知键可言，缺字段由后续规则判
			return nil, nil
		}
		return nil, errFrontmatterNotMapping
	}

	var unknown []string
	collectUnknownKeys(mapping, "", &unknown, frontmatterKnownKeys)
	slices.Sort(unknown) // 路径集先排序再输出——不得依赖 map 遍历序
	return unknown, nil
}

// keySpec 描述字段表的已知键结构：children = 映射分支，items = 列表元素键集，
// opaque = 内容归实现（channels[].config——未知键扫描**不进入**）。
// keySpec describes the known shape of a nested field in the contract table.
type keySpec struct {
	children map[string]keySpec
	items    map[string]keySpec
	opaque   bool
}

// frontmatterKnownKeys 是契约 §2 字段表的键集（与 frontmatterDTO 的 yaml 标签一一对应）。
// frontmatterKnownKeys mirrors the contract field table key by key.
var frontmatterKnownKeys = map[string]keySpec{
	"name":        {},
	"description": {},
	"tools":       {},
	"skills":      {},
	"mcp_servers": {},
	"bootstrap":   {},
	"identity": {children: map[string]keySpec{
		"agent_name": {},
		"prompt":     {},
	}},
	"provider": {children: map[string]keySpec{
		"name":        {},
		"model":       {},
		"temperature": {},
	}},
	"settings": {children: map[string]keySpec{
		"max_iterations":    {},
		"max_history_turns": {},
	}},
	"channels": {items: map[string]keySpec{
		"name":   {},
		"config": {opaque: true},
	}},
	"schedules": {items: map[string]keySpec{
		"id":      {},
		"cron":    {},
		"zone":    {},
		"message": {},
	}},
}

// collectUnknownKeys 递归收集未知键路径；**只记键、不记值**。
// collectUnknownKeys walks the mapping and records key paths only — never values.
func collectUnknownKeys(mapping map[string]any, prefix string, out *[]string, spec map[string]keySpec) {
	for key, value := range mapping {
		child, known := spec[key]
		switch {
		case !known:
			*out = append(*out, prefix+key)
		case child.opaque:
			// 内容归渠道实现——不进入（channels[].config 是自由映射）
		case child.children != nil:
			if nested, ok := value.(map[string]any); ok {
				collectUnknownKeys(nested, prefix+key+".", out, child.children)
			}
		case child.items != nil:
			items, ok := value.([]any)
			if !ok {
				continue
			}
			for i, item := range items {
				if nested, ok := item.(map[string]any); ok {
					collectUnknownKeys(nested, fmt.Sprintf("%s%s[%d].", prefix, key, i), out, child.items)
				}
			}
		}
	}
}

// intoProfile 把 DTO 显式映射到 core.Profile 并补"未声明"的缺省：
// settings 未声明 → 10 / 20（显式 0 保留）、channels 未声明 → [{name: cli}]
// （显式 [] → 空）、identity.agent_name 未声明或空白 → name、zone 未声明或空白 → UTC。
// intoProfile maps the DTO explicitly and fills only the documented defaults.
//
// 显式映射而非字段名反射：`agent_name` / `mcp_servers` / `max_iterations` /
// `max_history_turns` 靠 Go 的默认字段匹配一个都对不上（§2.4 注 9）。
func intoProfile(dto frontmatterDTO) *core.Profile {
	channels := make([]core.Channel, 0, len(dto.Channels))
	for _, c := range dto.Channels {
		channels = append(channels, core.Channel{Name: c.Name, Config: c.Config})
	}
	if dto.Channels == nil {
		channels = []core.Channel{{Name: "cli"}} // 未声明 → 缺省渠道；显式 [] 保持为空
	}

	settings := core.Settings{MaxIterations: defaultMaxIterations, MaxHistoryTurns: defaultMaxHistoryTurns}
	if dto.Settings.MaxIterations != nil {
		settings.MaxIterations = *dto.Settings.MaxIterations
	}
	if dto.Settings.MaxHistoryTurns != nil {
		settings.MaxHistoryTurns = *dto.Settings.MaxHistoryTurns
	}

	schedules := make([]core.Schedule, 0, len(dto.Schedules))
	for _, s := range dto.Schedules {
		zone := strings.TrimSpace(s.Zone)
		if zone == "" {
			zone = defaultZone
		}
		schedules = append(schedules, core.Schedule{ID: s.ID, Cron: s.Cron, Zone: zone, Message: s.Message})
	}

	agentName := strings.TrimSpace(dto.Identity.AgentName)
	if agentName == "" {
		agentName = dto.Name // 自我称谓缺省 = name（解析后必非空）
	}

	return &core.Profile{
		Name:        dto.Name,
		Description: dto.Description,
		Identity:    core.Identity{AgentName: agentName, Prompt: dto.Identity.Prompt},
		Provider: core.ProviderRef{
			Name:        dto.Provider.Name,
			Model:       dto.Provider.Model,
			Temperature: dto.Provider.Temperature,
		},
		Tools:      dto.Tools,
		Skills:     dto.Skills,
		MCPServers: dto.MCPServers,
		Bootstrap:  dto.Bootstrap,
		Channels:   channels,
		Settings:   settings,
		Schedules:  schedules,
	}
}

// validTemperature 判定已声明的 temperature 是否合法：0–2 且**有限**
// （NaN / ±Inf 拒——NaN 的区间比较恒 false，必须显式判有限性）。
// validTemperature accepts 0–2 and rejects NaN / ±Inf explicitly.
func validTemperature(t float64) bool {
	return !math.IsNaN(t) && !math.IsInf(t, 0) && t >= 0 && t <= 2
}

// frontmatterDTO 是 AGENT.md frontmatter 的解析 DTO（workspace 私有）：显式 yaml 标签、
// 键名 snake_case 逐字对齐契约 §2；`*T` 区分"未声明"与"显式零值"。
// frontmatterDTO is the private parsing DTO; the pointers keep absent values
// distinguishable from explicit zero ones.
type frontmatterDTO struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Identity    identityDTO   `yaml:"identity"`
	Provider    providerDTO   `yaml:"provider"`
	Tools       []string      `yaml:"tools"`
	Skills      []string      `yaml:"skills"`
	MCPServers  []string      `yaml:"mcp_servers"`
	Bootstrap   []string      `yaml:"bootstrap"`
	Channels    []channelDTO  `yaml:"channels"`
	Settings    settingsDTO   `yaml:"settings"`
	Schedules   []scheduleDTO `yaml:"schedules"`
}

type identityDTO struct {
	AgentName string `yaml:"agent_name"`
	Prompt    string `yaml:"prompt"`
}

type providerDTO struct {
	Name        string   `yaml:"name"`
	Model       string   `yaml:"model"`
	Temperature *float64 `yaml:"temperature"`
}

type channelDTO struct {
	Name   string         `yaml:"name"`
	Config map[string]any `yaml:"config"`
}

type settingsDTO struct {
	MaxIterations   *int `yaml:"max_iterations"`
	MaxHistoryTurns *int `yaml:"max_history_turns"`
}

type scheduleDTO struct {
	ID      string `yaml:"id"`
	Cron    string `yaml:"cron"`
	Zone    string `yaml:"zone"`
	Message string `yaml:"message"`
}

// cronHasTimezonePrefix 判定表达式是否自带时区前缀（robfig 认 TZ= 与 CRON_TZ= 两种）。
// 合法 cron 的首段是秒字段（数字 / * / , / -），两个前缀都不可能与它撞形态。
// cronHasTimezonePrefix reports whether the expression carries its own timezone.
func cronHasTimezonePrefix(expr string) bool {
	fields := strings.Fields(expr)
	if len(fields) == 0 {
		return false
	}
	return strings.HasPrefix(fields[0], "TZ=") || strings.HasPrefix(fields[0], "CRON_TZ=")
}

// validateSchedules 校验一只 Agent 的全部任务，返回**本只**的占用集（局部集）；
// claim 只读——占用由 LoadAll 在整只通过后一次性提交（§2.4 注 6 的 R1 裁决）。
// validateSchedules validates one agent's tasks and returns its own claim set.
//
// 逐任务的判定序：① 段数（剥 TZ 前缀后数段）≠ 6 → 固定句；③ 公共子集（L / W / #、
// 周字段 7、日周同时被限定）；② robfig 语法；④ zone；⑤ message。
// ⑥ id 在全只任务都合法之后判定（"恰一个任务"与"本只内重复"都是**集合**性质）。
//
// ②③ 的次序**有意**与 §2.3 的列表次序相反：robfig 对周字段 7 与 L / W / # 会先报出
// 它自己的解析错误（"end of range (7) above maximum (6)"），若让它先跑，§6 固定口径 5
// 冻结的三行 detail（周字段 7 超出范围 / 字段不支持 L/W/# 语法）就成了不可达的死字面量。
// 预检的目的正是"报出契约固定的句子 + 本仓统一类别"（§2.4 注 6），故子集检查前置。
// The subset check intentionally runs before the parser: otherwise the parser's own
// errors would shadow the frozen wording that the contract requires us to emit.
func validateSchedules(agent string, ss []core.Schedule, claim map[string]string) (map[string]string, error) {
	if len(ss) == 0 {
		return nil, nil
	}
	for i := range ss {
		ss[i].Zone = strings.TrimSpace(ss[i].Zone)
		if ss[i].Zone == "" {
			ss[i].Zone = defaultZone
		}
		if err := validateScheduleEntry(ss[i]); err != nil {
			return nil, err
		}
	}

	claimed := make(map[string]string, len(ss))
	for i := range ss {
		id := strings.TrimSpace(ss[i].ID)
		if id == "" {
			if len(ss) != 1 {
				return nil, fmt.Errorf("id 仅可在恰一个任务时缺省（派生 %s-1）", agent)
			}
			id = agent + "-1" // 派生 id **写回**结果：调度注册（US-10）不再二次派生
			ss[i].ID = id
		}
		if _, dup := claimed[id]; dup {
			return nil, fmt.Errorf("id 重复：%s", id)
		}
		if owner, taken := claim[id]; taken {
			return nil, fmt.Errorf("id 已被 %s 占用：%s", owner, id)
		}
		claimed[id] = agent
	}
	return claimed, nil
}

// validateScheduleEntry 校验单个任务（①②③④⑤；id 归 validateSchedules 的集合判定）。
// validateScheduleEntry checks one task; the id rules are a set-level concern.
func validateScheduleEntry(s core.Schedule) error {
	expr := strings.TrimSpace(s.Cron)
	if cronHasTimezonePrefix(expr) {
		// 时区只有一个来源（任务的 zone 字段）：cron 自带前缀必须在这里就拒——否则
		// 出口（CronSpec）会再叠一层，产出 `CRON_TZ=UTC CRON_TZ=Asia/Shanghai …`，
		// 调度注册时才炸（R2 评审修复）。拒绝句与 CronSpec 共用同一条常量。
		return errors.New(cronTimezonePrefixRule)
	}
	fields := strings.Fields(expr)
	if len(fields) != cronFieldCount {
		// 固定句（Compatibility §5.1 判据 4）——本仓自产，不得让库文案顶替。
		// 首词 Cron 是专有名词，属 ST1005 文档明示例外（"unless beginning with proper
		// nouns or acronyms"），但分析器的启发式只跳过首词含内部大写的形态，识别不到。
		// 本行字面量被 Compatibility §5.1 判据 4 与颗粒度文档 §6 固定口径 5 冻结、并被
		// 四处测试断言，不得改写——按门禁豁免纪律逐条豁免：经用户 2026-10-16 确认，全仓
		// 仅此一处，理由与范围进 US-3 验收报告（EngineeringStandards §1.1）。
		//lint:ignore ST1005 冻结字面量：Compatibility §5.1 判据 4 要求逐字含本句，首词 Cron 为专有名词
		return errors.New("Cron expression must consist of 6 fields")
	}
	if err := checkCronSubset(fields); err != nil {
		return err
	}
	// ③ 解析**最终规范化结果**（zone 前缀由 CronSpec 统一追加）：校验与出口必须是同一份
	// 表达式——此前校验解析的是原始串、出口另行拼前缀，两条路径会各说各话（R2 评审修复）。
	spec, err := CronSpec(s)
	if err != nil {
		return err
	}
	if _, err := cronParser.Parse(spec); err != nil {
		// 语法非法：robfig 原文进 detail（本仓只做分类，reason 恒为 invalid schedule）。
		return fmt.Errorf("%s", err.Error())
	}
	if err := checkZone(s.Zone); err != nil {
		return err
	}
	if strings.TrimSpace(s.Message) == "" {
		return errors.New("message 为必填")
	}
	return nil
}

// checkCronSubset 执行公共子集预检（契约 §3.1）：L / W / # 拒、周字段值 7 拒、
// 日字段与周字段不得同时被限定（至少一个是裸 *）。
// checkCronSubset enforces the documented public subset of cron syntax.
func checkCronSubset(fields []string) error {
	for _, item := range fields {
		// 记号级判定：L 是整项；W 是**项末**的大写 W（15W）——月 / 周缩写（JUL 含 L、
		// WED 含 W）不得误伤；# 出现在项里即拒。
		if strings.EqualFold(item, "L") ||
			strings.HasSuffix(strings.ToUpper(item), "W") ||
			strings.Contains(item, "#") {
			return fmt.Errorf("字段不支持 L/W/# 语法：%s", item)
		}
	}

	// 周字段 7：取值形式（7、1-7、1,5,7）拒——0 才是周日的合法写法。步长里的 7
	// （*/7）是"步长"语义、不在取值之列，故只切 , 与 -。
	for _, item := range strings.Split(fields[cronDowIndex], ",") {
		for _, value := range strings.Split(item, "-") {
			if strings.TrimSpace(value) == "7" {
				return errors.New("周字段 7 超出范围（0–6）")
			}
		}
	}

	if !isBareWildcard(fields[cronDomIndex]) && !isBareWildcard(fields[cronDowIndex]) {
		return errors.New("日与周不得同时受限（至少一个是裸 *）")
	}
	return nil
}

// isBareWildcard 判定字段是否为裸通配（* 或 robfig 同样接受的 ?）——*/2、1-15、
// 1,5、单值都算"受限"。
// isBareWildcard reports whether a field is an unrestricted wildcard.
func isBareWildcard(field string) bool {
	f := strings.TrimSpace(field)
	return f == "*" || f == "?"
}

// checkZone 校验 zone：空值已在填充点补 UTC（这里对空值再兜一次底）；Local 显式
// 黑名单（time.LoadLocation("Local") 会成功——只靠它拦不住"回落进程本地时区"）；
// 其余取值必须可加载。
// checkZone rejects Local explicitly and requires every other zone to load.
func checkZone(zone string) error {
	switch zone {
	case "", defaultZone:
		return nil
	case "Local":
		return errors.New("zone Local 被禁用（需地方时间请显式指定 IANA 时区）")
	}
	if _, err := time.LoadLocation(zone); err != nil {
		// LoadLocation 原文进 detail：zone 名不是秘密，且"未知时区"与"时区数据缺失"
		// 的区分正是运维需要的（§6 固定口径 5）。
		return err
	}
	return nil
}

// CronSpec 把 zone 字段转成 CRON_TZ= 前缀形态（契约 §3.1；调度注册归 US-10）。
// CronSpec renders the normalized expression; registering schedules belongs to
// US-10.
//
// 前缀形态是本仓的**唯一出口**：robfig 的 Parse 缺省时区是**进程本地时区**（本仓
// 禁用项，见根 CLAUDE.md 陷阱表），不显式前缀就会静默按本地时区触发——调用方一律
// 经本函数取值，不得自己拼字符串。
func CronSpec(s core.Schedule) (string, error) {
	expr := strings.TrimSpace(s.Cron)
	if expr == "" {
		return "", errors.New("cron 为空——无法规范化")
	}
	if cronHasTimezonePrefix(expr) {
		// 前缀只由本函数追加一次：表达式自带前缀会叠成 CRON_TZ=UTC CRON_TZ=Asia/Shanghai
		//（robfig 随后解析失败）。装载期已拒同名形态——这里是给直接调用方的第二道防线
		//（拒绝句共用同一条常量，两处不得各写一份）。
		return "", errors.New(cronTimezonePrefixRule)
	}
	zone := strings.TrimSpace(s.Zone)
	if zone == "" {
		zone = defaultZone
	}
	return "CRON_TZ=" + zone + " " + expr, nil
}

// frontmatterErrorSummary 把 frontmatter 的解析错误压成「[行:列] 自产类别」，供对外
// 文案与日志使用；与 cli.yamlErrorSummary 同口径——**两处各一份是既知重复**
// （workspace 不得 import cli、core 零语句不能承载函数，见 §2.4 注 12）。
// frontmatterErrorSummary renders a parse error as a position plus a locally
// produced category; library text never leaks out.
func frontmatterErrorSummary(err error) string {
	switch {
	case errors.Is(err, errNoFrontmatterFence),
		errors.Is(err, errOpenFrontmatterFence),
		errors.Is(err, errFrontmatterNotMapping):
		return err.Error() // 本仓自产文案（无库文案）——原样给用户
	}

	category := frontmatterErrorCategory(err)
	var yamlErr yaml.Error
	if errors.As(err, &yamlErr) {
		if tk := yamlErr.GetToken(); tk != nil {
			return fmt.Sprintf("[%d:%d] %s", tk.Position.Line, tk.Position.Column, category)
		}
	}
	return category
}

// frontmatterErrorCategory 把 YAML 库错误归到本仓自产的类别：库的默认文案会带出错处
// 的源码摘录（陷阱 40），一个字都不外传。
// frontmatterErrorCategory maps a library error onto a locally produced category.
func frontmatterErrorCategory(err error) string {
	var (
		syntaxErr     *yaml.SyntaxError
		typeErr       *yaml.TypeError
		overflowErr   *yaml.OverflowError
		dupKeyErr     *yaml.DuplicateKeyError
		unknownFldErr *yaml.UnknownFieldError
		unexpectedErr *yaml.UnexpectedNodeTypeError
	)
	switch {
	case errors.As(err, &syntaxErr):
		return "YAML 语法错误"
	case errors.As(err, &typeErr):
		return "字段类型非法"
	case errors.As(err, &overflowErr):
		return "字段数值溢出"
	case errors.As(err, &dupKeyErr):
		return "映射键重复"
	case errors.As(err, &unknownFldErr):
		return "含未知字段"
	case errors.As(err, &unexpectedErr):
		return "节点类型不符"
	default:
		return "YAML 解析失败"
	}
}
