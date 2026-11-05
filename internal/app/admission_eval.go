package app

// 本文件是 US-21 的七维确定性评估与策略 / 基线快照（颗粒度文档 §3.1.6、T010）：
//
//	BuildAdmissionPolicy  —— 8 类策略输入的规范化快照与 policy_hash；
//	BuildAdmissionBaseline—— 技能指针 + 引用名单的冻结基线；
//	EvaluateAdmission     —— content / source / permissions / safety / regression /
//	                         effect / cost 七维判定、聚合与风险档；
//	AdmissionRiskTier     —— 批准 / 发布时重新计算风险档的同一口径。
//
// 约束：本文件**不**执行候选脚本、**不**访问网络、**不**调用模型；`now` 由调用方
// 显式传入，固定时钟下结果可复现。评估失败不返回错误而是写入维度判定——只有候选
// 本身的身份 / 时间等前置问题才走错误路径（由 admin 层映射操作终态）。
// The evaluator never executes scripts, touches the network or calls a model; the
// caller supplies `now` so a fixed clock reproduces the same verdicts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// AdmissionGraderVersion 是固定七维评分器的版本；新增评分器版本必须同时改这里，
// 旧评估不得复用（颗粒度文档 §3.4 与 DDL 注释同口径）。
// AdmissionGraderVersion pins the deterministic grader version; a new grader must bump
// this value and must not reuse old evaluations.
const AdmissionGraderVersion = "admission-eval-v1"

// admissionExpiryWindow 是批准有效期上限：决策可填更短，长于 24h 即拒绝。
// admissionExpiryWindow caps every approval's validity at 24 hours.
const admissionExpiryWindow = 24 * time.Hour

// admissionDimensionOrder 是七维报告的固定顺序（与 core 的枚举顺序一致）。
// admissionDimensionOrder keeps the report order identical to the core enumeration.
var admissionDimensionOrder = []core.AdmissionDimensionName{
	core.AdmissionDimensionContent,
	core.AdmissionDimensionSource,
	core.AdmissionDimensionPermissions,
	core.AdmissionDimensionSafety,
	core.AdmissionDimensionRegression,
	core.AdmissionDimensionEffect,
	core.AdmissionDimensionCost,
}

// admissionScriptExtensions 是"脚本 / 可执行内容"的扩展名集合（小写、带点）。
// 只按路径判定，不读内容、不执行；脚本是否有害由人工静态审查负责。AuthorFile
// 不携带文件模式，无扩展名的文件另按"无法证明非脚本"处理（见 admissionScripts）。
// admissionScriptExtensions lists the executable/script extensions; content is never
// inspected or run, and extensionless files are separately treated as scripts.
var admissionScriptExtensions = map[string]struct{}{
	".sh": {}, ".bash": {}, ".zsh": {}, ".fish": {}, ".ps1": {}, ".bat": {}, ".cmd": {},
	".py": {}, ".rb": {}, ".pl": {}, ".php": {}, ".lua": {}, ".tcl": {},
	".js": {}, ".mjs": {}, ".cjs": {}, ".ts": {},
	".exe": {}, ".dll": {}, ".so": {}, ".dylib": {}, ".bin": {}, ".com": {},
	".jar": {}, ".wasm": {},
}

// admissionAgentDefinitionFile 是候选内出现即视为"试图改变 Agent 工具声明"的固定名字。
// 技能目录的契约文件是 SKILL.md；出现 AGENT.md 意味着候选在尝试携带 Agent 定义。
// admissionAgentDefinitionFile marks an attempt to carry an agent definition inside a
// skill candidate; the skill contract file is SKILL.md, not AGENT.md.
const admissionAgentDefinitionFile = "AGENT.md"

// BuildAdmissionPolicy 组装策略快照并计算 policy_hash。规范化顺序固定：
//
//  1. sandbox.cross_agent_read（排序）
//  2. shell.allowed_commands 与程序目录可执行名（合并、排序）
//  3. shell.interpreters（排序）
//  4. execution.allow_run_code
//  5. 隔离后端 / tools 映射 / isolation.profiles.* 的全部档位（按名字排序）/ 配置档位名
//  6. 数据库 execution_policy.profile 与 revision
//  7. 数据库 network_domain_policy 两行（scope、revision、排序后 domains）
//  8. network_exceptions 的 (kind, value) 排序集合
//
// 行序与列表顺序不影响哈希；域名表 revision 变化即使域名集合相同也产生新哈希。
// BuildAdmissionPolicy normalises the eight documented inputs and hashes them in a
// fixed order; row order never matters while a domain-table revision always does.
func BuildAdmissionPolicy(cfg core.AdmissionPolicyConfig, dbPolicy core.ExecutionPolicy, domains []core.NetworkDomainPolicy, exceptions []core.NetworkException) core.AdmissionPolicy {
	commands := make([]string, 0, len(cfg.AllowedCommands)+len(cfg.ProgramDirNames))
	commands = append(commands, cfg.AllowedCommands...)
	commands = append(commands, cfg.ProgramDirNames...)
	commands = admissionSortedUnique(commands)

	tools := make(map[string]string, len(cfg.IsolationTools))
	for name, target := range cfg.IsolationTools {
		tools[name] = target
	}

	// 档位按名字排序：map 迭代顺序随机，排序后同一配置逐字得到同一哈希。
	// Profiles are sorted by name so map iteration order never affects the hash.
	profiles := slices.Clone(cfg.IsolationProfiles)
	slices.SortStableFunc(profiles, func(a, b core.AdmissionIsolationProfile) int {
		return strings.Compare(a.Name, b.Name)
	})

	domainRows := make([]core.AdmissionDomainPolicy, 0, len(domains))
	for _, row := range domains {
		domainRows = append(domainRows, core.AdmissionDomainPolicy{
			Scope:    row.Scope,
			Revision: row.Revision,
			Domains:  admissionSortedUnique(row.Domains),
		})
	}
	slices.SortStableFunc(domainRows, func(a, b core.AdmissionDomainPolicy) int {
		if order := strings.Compare(a.Scope, b.Scope); order != 0 {
			return order
		}
		if a.Revision != b.Revision {
			if a.Revision < b.Revision {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.Join(a.Domains, "\x00"), strings.Join(b.Domains, "\x00"))
	})

	exceptionRows := make([]core.AdmissionNetworkException, 0, len(exceptions))
	for _, exception := range exceptions {
		exceptionRows = append(exceptionRows, core.AdmissionNetworkException{
			Kind:  exception.Kind,
			Value: exception.Value,
		})
	}
	slices.SortStableFunc(exceptionRows, func(a, b core.AdmissionNetworkException) int {
		if order := strings.Compare(a.Kind, b.Kind); order != 0 {
			return order
		}
		return strings.Compare(a.Value, b.Value)
	})
	exceptionRows = slices.CompactFunc(exceptionRows, func(a, b core.AdmissionNetworkException) bool {
		return a.Kind == b.Kind && a.Value == b.Value
	})

	policy := core.AdmissionPolicy{
		CrossAgentRead:    admissionSortedUnique(cfg.CrossAgentRead),
		AllowedCommands:   commands,
		Interpreters:      admissionSortedUnique(cfg.Interpreters),
		DomainPolicies:    domainRows,
		AllowRunCode:      cfg.AllowRunCode,
		IsolationBackend:  cfg.IsolationBackend,
		IsolationTools:    tools,
		IsolationProfiles: profiles,
		ExecutionProfile:  string(dbPolicy.Profile),
		ExecutionRevision: dbPolicy.Revision,
		NetworkExceptions: exceptionRows,
	}
	policy.Hash = core.AdmissionSHA256Hex(admissionPolicyHashJSON(cfg, dbPolicy, policy))
	return policy
}

// admissionPolicyHashInput 是 policy_hash 的固定输入面（字段序即契约序）。
// admissionPolicyHashInput fixes the field order of the policy hash input.
type admissionPolicyHashInput struct {
	CrossAgentRead    []string                         `json:"cross_agent_read"`
	AllowedCommands   []string                         `json:"allowed_commands"`
	Interpreters      []string                         `json:"interpreters"`
	AllowRunCode      bool                             `json:"allow_run_code"`
	IsolationBackend  string                           `json:"isolation_backend"`
	IsolationTools    map[string]string                `json:"isolation_tools"`
	IsolationProfiles []core.AdmissionIsolationProfile `json:"isolation_profiles"`
	ConfigProfile     string                           `json:"config_profile"`
	ExecutionProfile  string                           `json:"execution_profile"`
	ExecutionRevision int64                            `json:"execution_revision"`
	DomainPolicies    []core.AdmissionDomainPolicy     `json:"domain_policies"`
	NetworkExceptions []core.AdmissionNetworkException `json:"network_exceptions"`
}

// admissionPolicyHashJSON 序列化规范化后的策略快照；map 键由 encoding/json 排序，
// 因此同一输入逐字得到同一字节串。序列化失败只可能是不可达的类型错误——返回空串
// 让哈希成为固定失败值，调用方比对必然不相等（fail-closed）。
// admissionPolicyHashJSON serialises the normalised snapshot; a marshal failure is
// unreachable but yields "" so comparisons fail closed.
func admissionPolicyHashJSON(cfg core.AdmissionPolicyConfig, dbPolicy core.ExecutionPolicy, policy core.AdmissionPolicy) []byte {
	wire := admissionPolicyHashInput{
		CrossAgentRead:    policy.CrossAgentRead,
		AllowedCommands:   policy.AllowedCommands,
		Interpreters:      policy.Interpreters,
		AllowRunCode:      policy.AllowRunCode,
		IsolationBackend:  policy.IsolationBackend,
		IsolationTools:    policy.IsolationTools,
		IsolationProfiles: policy.IsolationProfiles,
		ConfigProfile:     cfg.ExecutionProfile,
		ExecutionProfile:  policy.ExecutionProfile,
		ExecutionRevision: policy.ExecutionRevision,
		DomainPolicies:    policy.DomainPolicies,
		NetworkExceptions: policy.NetworkExceptions,
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil
	}
	return encoded
}

// admissionSortedUnique 返回升序去重副本；nil 归一为空切片，保证 JSON 是 `[]` 而非 null。
// admissionSortedUnique returns a sorted, de-duplicated copy; nil becomes an empty slice.
func admissionSortedUnique(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	slices.Sort(out)
	out = slices.Compact(out)
	return out
}

// BuildAdmissionBaseline 读取技能当前的发布指针与引用名单并冻结为基线：评估、批准、
// 发布都绑定同一个 CanonicalJSON，任何漂移（新发布、新引用、删除）都会改变它。
// 指针行不存在是 `absent`（从未发布），已打删除标记是 `deleted`（墓碑仍在）。
// BuildAdmissionBaseline freezes the skill pointer and referrer list; a missing pointer
// is absent, a tombstone is deleted, and any later drift changes the canonical JSON.
func BuildAdmissionBaseline(ctx context.Context, resources *postgres.ResourceStore, references func(string) ([]string, error), skillName string) (core.AdmissionBaseline, error) {
	if resources == nil {
		return core.AdmissionBaseline{}, errors.New("构建准入基线需要资源仓储")
	}
	baseline := core.AdmissionBaseline{SkillState: core.AdmissionSkillAbsent}
	pointer, err := resources.Pointer(ctx, core.ResourceSkill, skillName)
	switch {
	case errors.Is(err, core.ErrResourceNotFound):
		// 从未发布：保持 absent，不把"没有行"当错误。
	case err != nil:
		return core.AdmissionBaseline{}, fmt.Errorf("读取技能 %s 的发布指针: %w", skillName, err)
	default:
		baseline.ResourceUID = pointer.ResourceUID
		baseline.PublishedVersionID = pointer.PublishedVersionID
		baseline.PointerRevision = int64(pointer.Revision)
		if pointer.DeletedAt != nil {
			baseline.SkillState = core.AdmissionSkillDeleted
		} else {
			baseline.SkillState = core.AdmissionSkillPresent
		}
		version, err := resources.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return core.AdmissionBaseline{}, fmt.Errorf("读取技能 %s 的当前版本 %d: %w",
				skillName, pointer.PublishedVersionID, err)
		}
		baseline.VersionNo = version.VersionNo
		baseline.ContentHash = version.ContentHash
	}
	referrers := []string{}
	if references != nil {
		referrers, err = references(skillName)
		if err != nil {
			return core.AdmissionBaseline{}, fmt.Errorf("读取技能 %s 的引用名单: %w", skillName, err)
		}
	}
	baseline.Referrers = admissionSortedUnique(referrers)
	baseline.ReferrersHash = core.AdmissionRequestHash(baseline.Referrers...)
	baseline.CanonicalJSON = core.AdmissionBaselineCanonical(baseline)
	return baseline, nil
}

// EvaluateAdmission 对候选做七维确定性判定并计算聚合与风险档。评估身份
// （evaluation_id / 修订号 / 时间 / 操作号）由 admin 层在预写审计后补齐，本函数
// 只产出可复算的判定面，同一输入在同一 `now` 下逐字相同。
// EvaluateAdmission produces the reproducible verdict surface; identity fields are
// filled by the admin layer after the audit pre-write.
func EvaluateAdmission(cand core.AdmissionCandidate, files []core.AuthorFile, source core.AdmissionSource, policy core.AdmissionPolicy, baseline core.AdmissionBaseline, now time.Time) (core.AdmissionEvaluation, error) {
	scripts := admissionScripts(files)
	dimensions := make([]core.AdmissionDimension, 0, len(admissionDimensionOrder))
	for _, name := range admissionDimensionOrder {
		dimensions = append(dimensions, admissionDimension(name, cand, files, source, baseline, scripts, now))
	}
	evaluation := core.AdmissionEvaluation{
		GraderVersion: AdmissionGraderVersion,
		PolicyHash:    policy.Hash,
		Dimensions:    dimensions,
		Scripts:       scripts,
		Aggregate:     admissionAggregate(dimensions),
		RiskTier:      admissionRiskTier(baseline.SkillState, baseline.Referrers, scripts),
		Referrers:     admissionSortedUnique(baseline.Referrers),
		BaselineJSON:  baseline.CanonicalJSON,
	}
	return evaluation, nil
}

// admissionDimension 按固定口径判定单个维度。每个分支显式写出理由，评估 JSON 因此
// 自带"为什么"而不仅是状态位。
// admissionDimension evaluates one dimension and always records why.
func admissionDimension(name core.AdmissionDimensionName, cand core.AdmissionCandidate, files []core.AuthorFile, source core.AdmissionSource, baseline core.AdmissionBaseline, scripts []string, now time.Time) core.AdmissionDimension {
	switch name {
	case core.AdmissionDimensionContent:
		return admissionContentDimension(cand, files)
	case core.AdmissionDimensionSource:
		return admissionSourceDimension(source, now)
	case core.AdmissionDimensionPermissions:
		return admissionPermissionsDimension(files, scripts)
	case core.AdmissionDimensionSafety:
		return admissionSafetyDimension(files, source)
	case core.AdmissionDimensionRegression:
		if baseline.SkillState == core.AdmissionSkillPresent {
			return core.AdmissionDimension{
				Name: name, Status: core.AdmissionDimensionNotEvaluable,
				Reason: "更新既有技能；本 US 不做行为回归评估",
			}
		}
		return core.AdmissionDimension{
			Name: name, Status: core.AdmissionDimensionPassed,
			Reason: "新技能，无旧版本可回归",
		}
	case core.AdmissionDimensionEffect:
		return core.AdmissionDimension{
			Name: name, Status: core.AdmissionDimensionNotEvaluable,
			Reason: "real_model_not_authorized",
		}
	case core.AdmissionDimensionCost:
		return core.AdmissionDimension{
			Name: name, Status: core.AdmissionDimensionNotEvaluable,
			Reason: "no_real_model_invocation",
		}
	default:
		// 未知维度不可能进入固定枚举；按失败处理而不是静默放行（fail-closed）。
		return core.AdmissionDimension{
			Name: name, Status: core.AdmissionDimensionFailed,
			Reason: "未知维度，拒绝放行",
		}
	}
}

// admissionContentDimension 校验文件集、根 SKILL.md、技能卡片与候选哈希。
// admissionContentDimension revalidates the file set, root SKILL.md, card and hashes.
func admissionContentDimension(cand core.AdmissionCandidate, files []core.AuthorFile) core.AdmissionDimension {
	name := core.AdmissionDimensionContent
	rebuilt, err := workspace.ValidateSkillDraft(cand.SkillName, files)
	if err != nil {
		return core.AdmissionDimension{
			Name: name, Status: core.AdmissionDimensionFailed,
			Reason: "文件集、根 SKILL.md 或技能卡片校验未通过",
		}
	}
	if rebuilt.ContentHash != cand.CandidateHash {
		return core.AdmissionDimension{
			Name: name, Status: core.AdmissionDimensionFailed,
			Reason: "候选内容哈希与文件集不一致",
		}
	}
	if rebuilt.ManifestJSON != cand.ManifestJSON {
		return core.AdmissionDimension{
			Name: name, Status: core.AdmissionDimensionFailed,
			Reason: "候选文件清单与文件集不一致",
		}
	}
	return core.AdmissionDimension{Name: name, Status: core.AdmissionDimensionPassed}
}

// admissionSourceDimension 按来源声明判定：授权 / 脱敏必须已声明，确认方式必须是
// 直接或用户确认，任务完成必须有证据、不适用必须显式适用，保留期未过。
// admissionSourceDimension applies the source declaration rules from §3.1.6.
func admissionSourceDimension(source core.AdmissionSource, now time.Time) core.AdmissionDimension {
	name := core.AdmissionDimensionSource
	failed := func(reason string) core.AdmissionDimension {
		return core.AdmissionDimension{Name: name, Status: core.AdmissionDimensionFailed, Reason: reason}
	}
	if source.Authorization == "unknown" {
		return failed("授权声明为 unknown，未确认不可准入")
	}
	if source.Redaction == "unknown" {
		return failed("脱敏声明为 unknown，未确认不可准入")
	}
	if source.Confirmation != "direct" && source.Confirmation != "user_confirmed" {
		return failed("确认方式不是 direct / user_confirmed")
	}
	if !source.Applicable {
		return failed("适用性声明为 false")
	}
	switch source.TaskOutcome {
	case "completed":
		if len(source.Evidence) == 0 {
			return failed("任务结果为 completed 但缺少证据")
		}
	case "not_applicable":
	case "failed", "unknown":
		return failed("任务结果为 failed / unknown")
	default:
		return failed("任务结果不在允许集合内")
	}
	if err := admissionRetentionValid(source, now); err != nil {
		return failed("来源材料的保留期已过")
	}
	return core.AdmissionDimension{Name: name, Status: core.AdmissionDimensionPassed}
}

// admissionPermissionsDimension 只看候选是否携带脚本 / 可执行文件或 Agent 定义：
// 含脚本不执行、列入 Scripts 并判 not_evaluable（批准时逐条静态审查）；携带
// AGENT.md 视为试图改变工具声明，直接 failed。
// admissionPermissionsDimension never executes scripts; it lists them for static review
// and rejects an embedded agent definition outright.
func admissionPermissionsDimension(files []core.AuthorFile, scripts []string) core.AdmissionDimension {
	name := core.AdmissionDimensionPermissions
	for _, file := range files {
		if strings.EqualFold(path.Base(strings.ReplaceAll(file.Path, `\`, "/")), admissionAgentDefinitionFile) {
			return core.AdmissionDimension{
				Name: name, Status: core.AdmissionDimensionFailed,
				Reason: "候选包含 AGENT.md，可能改变 Agent 工具声明",
			}
		}
	}
	if len(scripts) > 0 {
		return core.AdmissionDimension{
			Name: name, Status: core.AdmissionDimensionNotEvaluable,
			Reason: "候选含脚本 / 可执行文件，未执行；批准需逐条静态审查",
		}
	}
	return core.AdmissionDimension{
		Name: name, Status: core.AdmissionDimensionPassed,
		Reason: "文件集不含脚本 / 可执行内容",
	}
}

// admissionSafetyDimension 做凭据启发式扫描；命中即 failed（不走风险档、不能靠换人
// 批准解除）。路径 / 编码 / 软链接校验由候选冻结与 ValidateSkillDraft 负责。
// admissionSafetyDimension fails on credential hits; path, encoding and symlink checks
// belong to candidate freezing and ValidateSkillDraft.
func admissionSafetyDimension(files []core.AuthorFile, source core.AdmissionSource) core.AdmissionDimension {
	name := core.AdmissionDimensionSafety
	hits := ScanAdmissionSecrets(files)
	sourceHit := AdmissionSourceHasSecret(source)
	if len(hits) == 0 && !sourceHit {
		return core.AdmissionDimension{Name: name, Status: core.AdmissionDimensionPassed}
	}
	reason := "凭据扫描命中候选文件"
	if len(hits) > 0 {
		reason = "凭据扫描命中候选文件：" + strings.Join(hits, ", ")
	}
	if sourceHit {
		if len(hits) > 0 {
			reason += "；来源声明同时命中"
		} else {
			reason = "凭据扫描命中的来源声明"
		}
	}
	return core.AdmissionDimension{Name: name, Status: core.AdmissionDimensionFailed, Reason: reason}
}

// admissionScripts 列出候选内的脚本 / 可执行文件（升序去重）；空集合即无脚本。
// 判定面是"扩展名命中"加"无扩展名一律按脚本"：无扩展名的文件无法被证明不是
// 脚本（可执行二进制、无 shebang 的脚本都可能），漏判会绕过脚本静态审查与
// 高风险提出者 / 批准者分离，因此宁可误判为脚本也不放行。文件名以点结尾
// （`collect.`）在 Linux 上 path.Ext 返回 `.`——名义扩展名为空，同按无扩展名处理，
// 否则改名即可绕过审查（契约「改文件名不能绕过审查」）。
//
// 末尾空白先剥掉再取扩展名（review ② F-03 的纵深防御）：`run.sh ` 的原始扩展名是
// `".sh "`，既不命中脚本集合也不是空串，会被当成"非脚本"。路径校验已拒绝这类名字，
// 这里再兜一层，保证任何来源的候选都不会因尾随空白漏判。
// admissionScripts lists candidate scripts; extensionless files are always treated as
// scripts because they cannot be proven non-executable, and a miss would bypass the
// script review and the high-risk separation. Trailing whitespace is trimmed before the
// extension check so `run.sh ` can never be classified as a non-script.
func admissionScripts(files []core.AuthorFile) []string {
	scripts := make([]string, 0, 2)
	for _, file := range files {
		normalized := strings.ReplaceAll(file.Path, `\`, "/")
		extension := strings.ToLower(path.Ext(strings.TrimRightFunc(normalized, unicode.IsSpace)))
		if extension == "" || extension == "." {
			scripts = append(scripts, normalized)
			continue
		}
		if _, ok := admissionScriptExtensions[extension]; ok {
			scripts = append(scripts, normalized)
		}
	}
	return admissionSortedUnique(scripts)
}

// admissionAggregate 聚合七维：任一 failed → failed；否则任一 not_evaluable →
// incomplete；全部 passed 才是 passed。`not_evaluable` **不等于**通过。
// admissionAggregate rolls the dimensions up; not_evaluable never counts as passed.
func admissionAggregate(dimensions []core.AdmissionDimension) core.AdmissionAggregate {
	aggregate := core.AdmissionAggregatePassed
	for _, dimension := range dimensions {
		switch dimension.Status {
		case core.AdmissionDimensionFailed:
			return core.AdmissionAggregateFailed
		case core.AdmissionDimensionNotEvaluable:
			aggregate = core.AdmissionAggregateIncomplete
		}
	}
	return aggregate
}

// admissionRiskTier 是风险档的**唯一**计算口径：基线 present 且 referrers 非空，
// 或候选含脚本 / 可执行文件 → high；其余 normal。
// admissionRiskTier is the single risk-tier rule: a bound-skill update or any script
// makes the candidate high risk.
func admissionRiskTier(skillState string, referrers []string, scripts []string) core.AdmissionRiskTier {
	if len(scripts) > 0 {
		return core.AdmissionRiskHigh
	}
	if skillState == core.AdmissionSkillPresent && len(referrers) > 0 {
		return core.AdmissionRiskHigh
	}
	return core.AdmissionRiskNormal
}

// AdmissionRiskTier 在批准 / 发布时用评估自带的基线重算风险档；与评估写入的档位
// 不等即 drift。基线 JSON 解析失败时回退到候选自己的快照，两者都不可读则按 normal
// 处理——真正的漂移由基线逐字比对先拦住，这里不制造第二套判据。
// AdmissionRiskTier recomputes the tier from the evaluation's own baseline; the
// authoritative drift check remains the verbatim baseline comparison.
func AdmissionRiskTier(cand core.AdmissionCandidate, evaluation core.AdmissionEvaluation) core.AdmissionRiskTier {
	if len(evaluation.Scripts) > 0 {
		return core.AdmissionRiskHigh
	}
	state, referrers, ok := admissionBaselineState(evaluation.BaselineJSON)
	if !ok {
		state, referrers, ok = admissionBaselineState(cand.BaselineJSON)
	}
	if !ok {
		return admissionRiskTier("", nil, nil)
	}
	return admissionRiskTier(state, referrers, nil)
}

// admissionBaselineWire 只用于读回 canonical JSON 的两个字段（风险档重算）。
// admissionBaselineWire reads back only the two fields the risk tier needs.
type admissionBaselineWire struct {
	SkillState string   `json:"skill_state"`
	Referrers  []string `json:"referrers"`
}

// admissionBaselineState 解析基线 JSON；空串或结构不符返回 ok=false。
// admissionBaselineState parses the baseline JSON, reporting ok=false on any mismatch.
func admissionBaselineState(raw string) (string, []string, bool) {
	if raw == "" {
		return "", nil, false
	}
	var wire admissionBaselineWire
	if err := json.Unmarshal([]byte(raw), &wire); err != nil {
		return "", nil, false
	}
	if wire.SkillState == "" {
		return "", nil, false
	}
	return wire.SkillState, wire.Referrers, true
}

// admissionRetentionValid 是保留期的**唯一**判定函数：空串视为长期有效；否则按
// RFC3339 解析为绝对时刻，`now >= retention_until` 即过期。评估、批准、发布三处
// 共用本函数，比较绝对时刻，不按进程时区取日期。
// admissionRetentionValid is the single retention predicate shared by evaluate,
// approve and publish; it compares absolute instants, never calendar dates.
func admissionRetentionValid(source core.AdmissionSource, now time.Time) error {
	if source.RetentionUntil == "" {
		return nil
	}
	until, err := time.Parse(time.RFC3339, source.RetentionUntil)
	if err != nil {
		return fmt.Errorf("%w: retention_until 不是 RFC3339", core.ErrAdmissionInvalidInput)
	}
	if !now.Before(until) {
		return core.ErrAdmissionRetentionExpired
	}
	return nil
}

// admissionExpiryWithin24h 判定批准有效期：必须晚于当前时刻，且不超过 24 小时
// （可更短；恰好 24 小时仍有效）。
// admissionExpiryWithin24h accepts (now, now+24h]; shorter windows are allowed.
func admissionExpiryWithin24h(at, now time.Time) bool {
	if !at.After(now) {
		return false
	}
	return !at.After(now.Add(admissionExpiryWindow))
}
