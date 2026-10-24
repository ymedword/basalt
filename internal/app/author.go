package app

// 本文件是作者模型的调用面（颗粒度文档 §3.1.7 / 约束 29）：配置校验与单次草稿生成。
// `Draft` 只经 `ProviderService.Chat` 发出一次模型调用（两阶段 `llm_calls` 审计），
// **不创建 Run、不写 `management_operations`、不调用工具、不落盘、不注册**；草稿
// 只存在于返回值里，保存必须另走带 `If-Match` 的多文件提交。
//
// 配置的"成对键 / temperature / 已发布 Provider"在**调用时**校验：CLI 装载期只做
// 语法解析（`author:` 段缺键不阻断启动），端点被调用时才判定作者功能是否可用——
// 不可用一律返回 `core.ErrProviderNotConfigured`（映射 503 `NOT_CONFIGURED`）。
// This file is the author-model call surface. Draft issues exactly one audited chat
// call, creates no run, writes no management operation, executes no tools and touches
// no disk. Configuration is judged at call time, not at startup.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	cronadapter "github.com/ymedword/basalt/internal/adapters/cron"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// authorDraftDefinitionPath 是草稿里必须存在的 Agent 定义文件（契约 §1）。
	// authorDraftDefinitionPath is the mandatory definition file in every draft.
	authorDraftDefinitionPath = "AGENT.md"

	// authorRunIDPrefix 是作者调用的伪 Run 标识前缀：`llm_calls.run_id` 是 TEXT 且
	// 无外键，该值不是 Run（约束 29 / 注 3）。
	// authorRunIDPrefix prefixes the pseudo run id carried by the audit row.
	authorRunIDPrefix = "agent-author-"

	// scheduleRunIDPrefix 是定时候选调用的伪 Run 前缀（R6.4.2）：与草稿调用分开，
	// 审计行仍只是 `llm_calls` 的一次两阶段记录，不建 Run、不写管理操作。
	// scheduleRunIDPrefix separates schedule-draft audit rows from draft rows.
	scheduleRunIDPrefix = "agent-schedule-"
)

// authorChatter 是作者服务对模型调用的**唯一**依赖面：生产装配传
// `*provider.ProviderService`，两阶段审计因此不可能被绕过（宪法 V）。
// authorChatter is the only model dependency of the author service; production passes
// the auditing provider service.
type authorChatter interface {
	Chat(ctx context.Context, req core.ChatRequest) (core.ChatResponse, error)
}

// 编译期断言：审计服务满足作者调用的端口，装配层不存在"绕过审计"的第二条路径。
// Compile-time assertion that the auditing service is the one used in production.
var _ authorChatter = (*provider.ProviderService)(nil)

// AuthorService 执行一次作者模型草稿生成（约束 29）。字段由装配层在每次调用前
// 用**当时的**已发布资源快照填充：提示词里的候选名与草稿引用的校验名单必须同源，
// 不得在服务内部缓存一份过期列表。
// AuthorService performs one author-model draft. The assembly fills its fields from the
// current published snapshot per call.
type AuthorService struct {
	config    core.AuthorConfig
	chat      authorChatter
	providers []workspace.ProviderEntry
	skills    []string
	mcp       []string
	template  string
	maxBytes  int64
	// now 是定时候选「未来三次触发时间」的唯一时钟来源（R6.4.4）：测试注入固定时间，
	// 生产装配传 `AgentAdmin.now`，同一轮枚举只读一次。
	// now is the single clock source for the next-three-trigger computation.
	now    func() time.Time
	logger *slog.Logger
}

// authorServiceDeps 是 AuthorService 的装配入参（包内可见：它不是对外概念）。
// authorServiceDeps is the unexported assembly input of AuthorService.
type authorServiceDeps struct {
	Config    core.AuthorConfig
	Chat      authorChatter
	Providers []workspace.ProviderEntry
	Skills    []string
	MCP       []string
	Template  string
	// MaxBytes 是 `http.max_request_bytes`；≤0 表示不施加额外上限（核心草稿上限仍生效）。
	// MaxBytes is http.max_request_bytes; ≤0 adds no further limit.
	MaxBytes int64
	// Now 是未来触发时间的时钟来源；nil 回落到 time.Now。
	// Now is the clock behind the next-three computation; nil falls back to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// newAuthorService 装配作者服务：依赖全部走类型化参数（宪法 VII），日志器缺省
// 落到进程默认值，避免调用方为日志再写分支。
// newAuthorService wires the author service from explicit typed dependencies.
func newAuthorService(deps authorServiceDeps) *AuthorService {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &AuthorService{
		config:    deps.Config,
		chat:      deps.Chat,
		providers: deps.Providers,
		skills:    deps.Skills,
		mcp:       deps.MCP,
		template:  deps.Template,
		maxBytes:  deps.MaxBytes,
		now:       now,
		logger:    logger,
	}
}

// ValidateAuthorConfig 判定作者配置在**调用时**是否可用：两个键必须成对出现、
// temperature 必须在 `[0,2]` 闭区间内且有限、配置的 Provider 必须是已发布条目之一。
// 全部失败都包 `core.ErrProviderNotConfigured`——对错两键与未发布同属"作者模型不
// 可用"，端点统一映射 503 `NOT_CONFIGURED`（失败分类表）。整段缺失（两键皆空）不是
// 错误：作者功能关闭由 `Draft` 判定并同样映射 503。
//
// 该函数只在调用时执行；CLI 装载期不做本判定（缺键不阻断启动），因此"Provider 是否
// 存在留到调用时"与成对键检查落在同一处，签名里的 providers 正是已发布条目快照。
// ValidateAuthorConfig judges the author configuration at call time. Every failure wraps
// ErrProviderNotConfigured so the endpoint answers 503; an entirely absent pair is not an
// error here (Draft turns "feature off" into the same 503).
func ValidateAuthorConfig(cfg core.AuthorConfig, providers []workspace.ProviderEntry) error {
	if err := validateAuthorTemperature(cfg.Temperature); err != nil {
		return err
	}

	providerSet, modelSet := cfg.Provider != "", cfg.Model != ""
	switch {
	case providerSet && !modelSet:
		return fmt.Errorf("%w: author.provider 已配置但 author.model 为空——两个键必须成对配置",
			core.ErrProviderNotConfigured)
	case !providerSet && modelSet:
		return fmt.Errorf("%w: author.model 已配置但 author.provider 为空——两个键必须成对配置",
			core.ErrProviderNotConfigured)
	}
	if !providerSet {
		// 两键皆空 = 作者功能关闭；是否允许调用由 Draft 判定。
		return nil
	}

	for _, entry := range providers {
		if entry.Name == cfg.Provider {
			return nil
		}
	}
	return fmt.Errorf("%w: author.provider %q 不在已发布 Provider 列表内",
		core.ErrProviderNotConfigured, cfg.Provider)
}

// validateAuthorTemperature 校验 temperature 的闭区间与有限性：NaN 与无穷必须显式
// 拒绝——区间比较对 NaN 恒为假，不查会静默放行（与 CLI 装载期同一规则）。
// validateAuthorTemperature rejects NaN, infinities and out-of-range values.
func validateAuthorTemperature(temperature float64) error {
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) ||
		temperature < 0 || temperature > 2 {
		return fmt.Errorf("%w: author.temperature 必须是 [0,2] 区间内的有限数字（当前值 %v）",
			core.ErrProviderNotConfigured, temperature)
	}
	return nil
}

// ensureChatReady 判定作者功能在**调用时**是否可用（草稿与定时候选共用）：整段配置
// 缺失、成对键缺一、温度越界、Provider 未发布或装配缺模型依赖，一律拒绝。配置类
// 错误仍包 `core.ErrProviderNotConfigured`（→ 503）；日志里的 operation 区分调用面。
// ensureChatReady judges availability at call time for both author call sites.
func (s *AuthorService) ensureChatReady(name, operation string) error {
	if s.config.Provider == "" && s.config.Model == "" {
		err := fmt.Errorf("%w: 未配置 author.provider / author.model", core.ErrProviderNotConfigured)
		s.logger.Warn("作者模型未配置——拒绝"+operation, "agent", name, "error", err)
		return err
	}
	if err := ValidateAuthorConfig(s.config, s.providers); err != nil {
		s.logger.Warn("作者模型配置不可用——拒绝"+operation, "agent", name, "error", err)
		return err
	}
	if s.chat == nil {
		return errors.New("作者服务缺少模型调用依赖（装配错误）")
	}
	return nil
}

// authorCallFailure 把一次失败的作者模型调用转成**对外可执行**的分类错误（R7 问题 8）：
// 上游超时映射 504 TIMEOUT，其余调用失败映射 500 INTERNAL + 点名 provider 的修复建议；
// 原始错误只留在日志与 `llm_calls` 审计里，不进入响应文案（不回显地址 / 凭证）。
// authorCallFailure turns a failed author call into an actionable error: upstream
// timeouts map to 504 and everything else to 500 with a named repair hint; the raw
// cause stays in the log and the audit row.
func authorCallFailure(cfg core.AuthorConfig, err error) error {
	var callErr *provider.CallError
	// Kind 用 errors.Join 同时保留原始错误链与分类哨兵：`errors.As` 仍能取到
	// `*provider.CallError`（排障与既有断言依赖它），`errors.Is` 也能命中分类。
	// The Kind joins the raw chain with the sentinel so both As and Is still work.
	if errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &callErr) && callErr.Kind == core.ErrorKindTimeout) {
		return &core.AdminError{
			Kind: errors.Join(err, context.DeadlineExceeded),
			Message: fmt.Sprintf(
				"作者模型调用超时或被取消（provider %q）：请稍后重试或检查该模型连接的可达性；本次操作未保存任何内容。",
				cfg.Provider),
		}
	}
	return &core.AdminError{
		Kind: errors.Join(err, core.ErrAuthorModelUnavailable),
		Message: fmt.Sprintf(
			"作者模型调用失败（provider %q）：请检查该模型连接的地址、默认模型与凭据，"+
				"并在模型调用审计中核对本次调用；本次操作未保存任何内容。",
			cfg.Provider),
	}
}

// Draft 生成一份只在内存里的 Agent 文件草稿（约束 29）：提示词由入参与当前已发布
// 资源名组成，模型输出经 `core.ParseAuthorResult` 解析后再做引用校验与请求体上限判定。
// 任一步失败都不留下任何持久化痕迹；成功的唯一数据库副作用是 `llm_calls` 的一行
// 预写与回填（由 `ProviderService.Chat` 完成）。只认文件的旧调用方在模型返回问题时
// 拿到错误——问题只能经 `DraftWithAnswers` 的调用方展示给用户。
// Draft produces an in-memory draft only; its sole persistent side effect is the
// two-phase llm_calls audit row written by ProviderService.Chat.
func (s *AuthorService) Draft(ctx context.Context, name, description string) ([]core.AuthorFile, error) {
	result, err := s.DraftWithAnswers(ctx, name, description, nil)
	if err != nil {
		return nil, err
	}
	if len(result.Questions) > 0 {
		return nil, errors.New("作者模型要求补充信息：请通过问答接口补充后重试")
	}
	return result.Files, nil
}

// DraftWithAnswers 执行一次作者调用并返回「文件数组或问题数组」（R6.3）：有答案时
// 提示词追加已澄清段并要求直接产出文件；没有答案时与 `Draft` 的提示词逐字节一致。
// 模型返回问题时**不做**文件校验（没有文件），也不写任何持久化；返回文件时仍走
// 引用校验与请求体上限（约束 29）。
// DraftWithAnswers returns either files or questions; a questions response skips the
// file checks because there are no files to judge.
func (s *AuthorService) DraftWithAnswers(ctx context.Context, name, description string,
	answers []core.AuthorAnswer,
) (core.AuthorDraftResult, error) {
	if err := s.ensureChatReady(name, "生成草稿"); err != nil {
		return core.AuthorDraftResult{}, err
	}
	normalized, err := core.NormalizeAuthorAnswers(answers)
	if err != nil {
		return core.AuthorDraftResult{}, err
	}

	prompt := core.BuildAuthorPromptWithAnswers(name, description, core.AuthorReferences{
		Providers: authorProviderNames(s.providers),
		Skills:    s.skills,
		MCP:       s.mcp,
		Template:  s.template,
	}, normalized)

	resp, err := s.chat.Chat(ctx, core.ChatRequest{
		Provider: s.config.Provider,
		RunID:    authorRunIDPrefix + name,
		// SessionID 留空 → 列写 NULL：作者调用不属于任何会话（约束 29）。
		SessionID: "",
		Round:     1,
		Model:     s.config.Model,
		Messages:  []core.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		s.logger.Warn("作者模型调用失败", "agent", name, "provider", s.config.Provider, "error", err)
		return core.AuthorDraftResult{}, authorCallFailure(s.config, err)
	}

	result, err := core.ParseAuthorResult(resp.Content)
	if err != nil {
		s.logger.Warn("作者输出不可解析——拒绝返回", "agent", name, "reason", err.Error())
		return core.AuthorDraftResult{}, fmt.Errorf("作者草稿不可用: %w", err)
	}
	if len(result.Questions) > 0 {
		return result, nil
	}
	if err := s.validateDraft(name, result.Files); err != nil {
		s.logger.Warn("作者草稿引用或结构非法——拒绝返回", "agent", name, "reason", err.Error())
		return core.AuthorDraftResult{}, err
	}
	if err := s.checkDraftRequestLimit(result.Files); err != nil {
		s.logger.Warn("作者草稿超过请求体上限——拒绝返回", "agent", name, "reason", err.Error())
		return core.AuthorDraftResult{}, err
	}
	return result, nil
}

// ScheduleDraftResult 是一次定时候选生成的结果（R6.4）：候选字段与**服务端**计算的
// 未来三次触发时间。触发时间只在服务端算，浏览器不按 cron 自行推导。
// ScheduleDraftResult pairs the candidate with the server-computed next three triggers.
type ScheduleDraftResult struct {
	Candidate core.Schedule
	NextRuns  []time.Time
}

// DraftSchedule 执行一次定时候选生成（R6.4）：一次 `ProviderService.Chat`（伪 Run
// `agent-schedule-<name>`），不写管理操作、不落盘、不注册、不改调度器。候选先按
// core 的形状规则解析，再用 `workspace.ValidateScheduleCandidate` 复用装载期校验，
// 最后由 `cronadapter.ParseOccurrences` 在服务端枚举未来三次触发时间（最多 366 天）。
// DraftSchedule issues exactly one audited chat call and never touches disk or the
// scheduler; the candidate is validated with the load-time rules verbatim.
func (s *AuthorService) DraftSchedule(ctx context.Context, name, instruction string) (ScheduleDraftResult, error) {
	normalizedInstruction, err := core.NormalizeScheduleInstruction(instruction)
	if err != nil {
		return ScheduleDraftResult{}, err
	}
	if err := s.ensureChatReady(name, "生成定时候选"); err != nil {
		return ScheduleDraftResult{}, err
	}

	prompt := core.BuildScheduleDraftPrompt(name, normalizedInstruction)
	resp, err := s.chat.Chat(ctx, core.ChatRequest{
		Provider: s.config.Provider,
		RunID:    scheduleRunIDPrefix + name,
		// SessionID 留空 → 列写 NULL：定时候选调用不属于任何会话。
		SessionID: "",
		Round:     1,
		Model:     s.config.Model,
		Messages:  []core.Message{{Role: "user", Content: prompt}},
	})
	if err != nil {
		s.logger.Warn("定时候选模型调用失败", "agent", name, "provider", s.config.Provider, "error", err)
		return ScheduleDraftResult{}, authorCallFailure(s.config, err)
	}

	candidate, err := core.ParseScheduleCandidate(resp.Content)
	if err != nil {
		s.logger.Warn("定时候选不可解析——拒绝返回", "agent", name, "reason", err.Error())
		return ScheduleDraftResult{}, fmt.Errorf("定时候选不可用: %w", err)
	}
	if err := core.ValidateScheduleDraftID(candidate.ID); err != nil {
		s.logger.Warn("定时候选 id 非法——拒绝返回", "agent", name, "reason", err.Error())
		return ScheduleDraftResult{}, err
	}
	// 作者模型只起草任务三要素；**代投目标不交给模型**（R7：由运维显式配置渠道），
	// 因此这里逐字段构造、DeliverTo 留空，而不是整结构体转换。
	normalized, err := workspace.ValidateScheduleCandidate(core.Schedule{
		ID:      candidate.ID,
		Cron:    candidate.Cron,
		Zone:    candidate.Zone,
		Message: candidate.Message,
	})
	if err != nil {
		s.logger.Warn("定时候选未通过装载期规则——拒绝返回", "agent", name, "reason", err.Error())
		return ScheduleDraftResult{}, err
	}
	spec, err := workspace.CronSpec(normalized)
	if err != nil {
		return ScheduleDraftResult{}, err
	}

	now := s.now()
	return ScheduleDraftResult{
		Candidate: normalized,
		NextRuns:  cronadapter.ParseOccurrences(spec, now, now.AddDate(0, 0, 366), 3),
	}, nil
}

// authorProviderNames 按传入顺序取已发布 Provider 名：提示词与引用校验共用同一份
// 快照，且**不**遍历 map（顺序稳定，宪法 III 的确定性要求）。
// authorProviderNames preserves the published entry order for prompt rendering.
func authorProviderNames(entries []workspace.ProviderEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name
	}
	return names
}

// checkDraftRequestLimit 判定草稿总量是否超过 `http.max_request_bytes`（约束 29）：
// 核心解析器已限制 64 个文件 / 单文件 256 KiB / 总量 1 MiB，这里补请求体上限——
// 两个上限同时生效，后者更小时以它为准。
// checkDraftRequestLimit enforces the request-body cap on top of the core draft limits.
func (s *AuthorService) checkDraftRequestLimit(files []core.AuthorFile) error {
	if s.maxBytes <= 0 {
		return nil
	}
	var total int64
	for _, file := range files {
		total += int64(len(file.Content))
	}
	if total > s.maxBytes {
		return fmt.Errorf("作者草稿总量 %d 字节超过 http.max_request_bytes 上限 %d 字节",
			total, s.maxBytes)
	}
	return nil
}

// validateDraft 校验草稿的 AGENT.md 结构与引用（约束 29「frontmatter、引用已发布资源」）：
// 定义文件必须以围栏开头且围栏闭合、YAML 可解析，`name` 与请求名逐字一致，
// `provider.name` / `provider.model` 非空白、temperature 合法，且 Provider / 技能 /
// MCP 三个声明只引用当前已发布（MCP 为已应用）的名字。未知 frontmatter 键不拒绝——
// 装载期对这些键只 WARN，草稿不得比装载更严。
// validateDraft checks the draft's AGENT.md structure and its references to published
// resources; unknown frontmatter keys stay tolerated, mirroring load-time warnings.
func (s *AuthorService) validateDraft(name string, files []core.AuthorFile) error {
	var definition string
	for _, file := range files {
		if file.Path == authorDraftDefinitionPath {
			definition = file.Content
			break
		}
	}
	if definition == "" {
		return fmt.Errorf("作者草稿缺少非空 %s", authorDraftDefinitionPath)
	}

	front, err := splitDraftFrontmatter(definition)
	if err != nil {
		return err
	}

	var dto authorDraftFrontmatter
	if err := yaml.Unmarshal(front, &dto); err != nil {
		return fmt.Errorf("草稿 %s 的 frontmatter 不是合法 YAML: %w", authorDraftDefinitionPath, err)
	}

	if dto.Name != name {
		return fmt.Errorf("草稿 %s 的 name %q 与请求的 Agent 名 %q 不一致",
			authorDraftDefinitionPath, dto.Name, name)
	}
	if strings.TrimSpace(dto.Provider.Name) == "" {
		return fmt.Errorf("草稿 %s 的 provider.name 为空", authorDraftDefinitionPath)
	}
	if strings.TrimSpace(dto.Provider.Model) == "" {
		return fmt.Errorf("草稿 %s 的 provider.model 为空（加载期会拒绝空白模型名）", authorDraftDefinitionPath)
	}
	if dto.Provider.Temperature != nil {
		if err := validateDraftTemperature(*dto.Provider.Temperature); err != nil {
			return err
		}
	}

	if !slices.Contains(authorProviderNames(s.providers), dto.Provider.Name) {
		return fmt.Errorf("草稿 %s 引用了未发布的 Provider %q", authorDraftDefinitionPath, dto.Provider.Name)
	}
	if unknown := unknownAuthorNames(dto.Skills, s.skills); len(unknown) > 0 {
		return fmt.Errorf("草稿 %s 引用了未发布的技能 %s",
			authorDraftDefinitionPath, strings.Join(unknown, "、"))
	}
	if unknown := unknownAuthorNames(dto.MCP, s.mcp); len(unknown) > 0 {
		return fmt.Errorf("草稿 %s 引用了未应用的 MCP server %s",
			authorDraftDefinitionPath, strings.Join(unknown, "、"))
	}
	return nil
}

// authorDraftFrontmatter 是草稿 AGENT.md 的**窄读取面**：只取引用校验需要的字段。
// 它不是第二套装载器——完整加载期规则（工具池、bootstrap、schedules）在提交后由
// `workspace.ValidateCandidate` 判卷；草稿阶段只回答"能不能引用、名字对不对"。
// authorDraftFrontmatter is the narrow read surface of a draft; it is not a second loader.
type authorDraftFrontmatter struct {
	Name     string              `yaml:"name"`
	Provider authorDraftProvider `yaml:"provider"`
	Skills   []string            `yaml:"skills"`
	MCP      []string            `yaml:"mcp_servers"`
}

// authorDraftProvider 是草稿 frontmatter 的 provider 段（温度用指针区分"未声明"）。
// authorDraftProvider is the provider section; the pointer keeps "absent" distinct.
type authorDraftProvider struct {
	Name        string   `yaml:"name"`
	Model       string   `yaml:"model"`
	Temperature *float64 `yaml:"temperature"`
}

// splitDraftFrontmatter 从草稿 AGENT.md 中切出围栏内的 frontmatter：容忍 UTF-8 BOM，
// 围栏必须由单独一行的 `---` 开头并以单独一行的 `---` 闭合（契约 §1 的同一形态）。
// 这里只读围栏、不解析正文——草稿正文原样保存，不做任何改写。
// splitDraftFrontmatter extracts the fenced frontmatter block, tolerating a BOM.
func splitDraftFrontmatter(content string) ([]byte, error) {
	// 容忍 UTF-8 BOM；必须写转义——字面量会触发 Go 扫描器的「illegal byte order mark」。
	text := strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(text, "---\n") && !strings.HasPrefix(text, "---\r\n") {
		return nil, fmt.Errorf("草稿 %s 缺少 frontmatter 围栏（必须以 --- 开头）", authorDraftDefinitionPath)
	}
	lines := strings.Split(text[strings.Index(text, "\n")+1:], "\n")
	for i, line := range lines {
		if strings.TrimSuffix(line, "\r") == "---" {
			return []byte(strings.Join(lines[:i], "\n")), nil
		}
	}
	return nil, fmt.Errorf("草稿 %s 的 frontmatter 围栏未闭合", authorDraftDefinitionPath)
}

// validateDraftTemperature 校验草稿里的 provider.temperature：与装载期规则一致，
// 非有限或越界即拒绝（否则草稿被提交后整只 Agent 不注册）。
// validateDraftTemperature mirrors the load-time temperature rule.
func validateDraftTemperature(temperature float64) error {
	if math.IsNaN(temperature) || math.IsInf(temperature, 0) ||
		temperature < 0 || temperature > 2 {
		return fmt.Errorf("草稿 %s 的 provider.temperature 必须是 [0,2] 区间内的有限数字（当前值 %v）",
			authorDraftDefinitionPath, temperature)
	}
	return nil
}

// unknownAuthorNames 返回 declared 中不在 allowed 里的名字（去重后按升序）：错误文案
// 因此与本进程的 map 遍历顺序无关，同一份草稿两次判定得到同一句话。
// unknownAuthorNames returns the sorted, de-duplicated names not in the allowed set.
func unknownAuthorNames(declared, allowed []string) []string {
	seen := make(map[string]struct{}, len(declared))
	unknown := make([]string, 0, len(declared))
	for _, name := range declared {
		if slices.Contains(allowed, name) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		unknown = append(unknown, name)
	}
	slices.Sort(unknown)
	return unknown
}
