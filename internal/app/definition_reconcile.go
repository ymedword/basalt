package app

// 本文件是启动定义对账（颗粒度文档 §3.1.7 / 约束 34、注 7）：把定义文件与已发布
// 版本逐资源比对，按四种决策收场——一致（无动作）、文件新且候选合法（建新版本并
// 发布，`published` 与 Provider / 技能 `applied` 同步）、文件新但候选非法（WARN 并
// 保留旧版本）、文件缺失（忽略删除，WARN 点名——删除只能经 DELETE API）。已发布
// 缺失而文件仍在 → 资源没有发布版本，Agent 不得注册，直接拒绝启动。
//
// 对账**不**改写定义文件、**不**重新序列化 YAML：候选版本存的就是文件里的条目原文
// （与基线同一份 `extractDefinitionBlocks` 切块实现），相同哈希即跳过，因此重复启动
// 是幂等的（注 7）。每个真正发生的发布都先预写一条 `pending` 管理操作，actor 为
// `cli:<本机用户>`、`auth_method=cli`、`detail_json.source=startup_reconcile`；任一步
// 数据库写入失败都把操作回填为 `failed` 并拒绝启动。
//
// MCP 的条目对账走同一张决策表，唯一差别是**发布不推进 applied**（约束 21：待重启）
// ——发布了新内容而当前进程还没连接，`pending_restart` 因此如实为真；supervisor 在本
// 函数之后启动，连接成功才把 applied 追平，连接失败保留旧 applied 并标记未连接。
// MCP reconciliation applies the same decision table but publishes with applied=false:
// the supervisor starts afterwards and advances applied only on a successful connect.
// Startup reconciliation never rewrites a file, never re-serialises YAML, is
// idempotent on repeated startup, and refuses startup when a file entry has no
// published version or a database write fails.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// reconcileDetailJSON 是启动对账操作行的固定 `detail_json`：来源字段让审计一眼看出
// 这条操作来自启动对账，而不是管理台或 CLI 交互（注 7）。用常量而不是包级切片，
// 避免共享可变状态。
// reconcileDetailJSON is the fixed detail_json of a startup reconcile operation.
const reconcileDetailJSON = `{"source":"startup_reconcile"}`

// reconcileSource 是操作行的来源归因字面量（`source` 列的合法取值之一）。
// reconcileSource is the literal written to the operation row's source column.
const reconcileSource = "cli"

// ReconcileDeps 是启动定义对账的装配入参：定义文件路径、资源版本仓储、管理操作
// 台账、技能版本库、运行准入闸门、日志器、时钟与本机用户名。可选字段为零值时按
// 生产缺省补齐（Logger→slog.Default、Now→time.Now、UserName→os/user、
// PublishVersion→Store.PublishWithVersion），Gate 为零值时退化为独立实例——生产装配
// **必须**传入与准入共用的那一份（约束 15 / 32）。
// ReconcileDeps wires the definition files, storage, operation ledger, skill library
// and admission gate for startup reconciliation.
type ReconcileDeps struct {
	ConfigPath string
	// MCPServersPath 是 `.basalt/mcp_servers.yaml` 的绝对路径（约束 34 的 MCP 分支）。
	// 与 ConfigPath 一样必填：缺了就静默跳过一整类资源，那正是对账最该避免的漏判。
	// MCPServersPath is the absolute path of mcp_servers.yaml and is likewise required.
	MCPServersPath string
	Store          *storage.ResourceStore
	Operations     core.OperationStore
	Skills         *workspace.SkillLibrary
	Gate           *core.ResourceGate
	Logger         *slog.Logger
	Now            func() time.Time
	UserName       string
	// PublishVersion 是「版本 + 指针 + 操作终态」同事务提交的注入位；零值走真实实现，
	// 用例只在这里注入失败，不伪造第二套发布路径。`applied` 是资源种类的发布语义：
	// Provider / 技能为 true（发布即生效），MCP 为 false（待重启才追平，约束 21）。
	// PublishVersion is the single transactional commit seam; applied follows the
	// resource kind's publish semantics (MCP stays pending until the next connect).
	PublishVersion func(ctx context.Context, v core.ResourceVersion, applied bool, operationID string, now time.Time) (core.ResourceVersion, error)
	// Credentials 是托管凭据的核对面（生产装配传凭据解析器）：托管条目没有 `${VAR}`
	// 占位可解析，候选能否发布只能由「已存凭据能否配合同一份候选定义」回答。为零时
	// 托管条目按候选非法保留旧版本（失败关闭）；env 条目不使用本字段。
	// Credentials answers whether a stored managed credential can work with the
	// candidate definition; nil keeps managed candidates retained (fail closed).
	Credentials reconcileCredentialGuard
}

// reconcileCredentialGuard 是启动对账对托管凭据的最小依赖：只做「身份 / 当前版本
// active + 目标绑定与候选定义一致」的只读核对，**不解密**、不写审计。与准入期
// `PinModelCredential` 同一判据，避免对账用另一套口径。
// reconcileCredentialGuard is the read-only binding check reconcile needs for managed
// entries; it never decrypts and mirrors the admission-time predicate.
type reconcileCredentialGuard interface {
	PinModelCredential(ctx context.Context, uid core.ResourceUID, targetBinding string) (core.CredentialPin, error)
}

// ReconcileResult 是一次对账的决策计数：每个已发布资源恰好落进 Published /
// Retained / Unchanged / Ignored 之一（文件有新条目但没有发布版本的情形直接报错，
// 不计入四类）。
// ReconcileResult counts one decision per published resource.
type ReconcileResult struct {
	// Published 是「文件新且候选合法」、已建新版本并发布的资源数。
	Published int
	// Retained 是「文件新但候选非法 / 工作副本不可读」、保留旧版本的资源数。
	Retained int
	// Unchanged 是文件与已发布版本逐字节一致的资源数。
	Unchanged int
	// Ignored 是定义文件中已无该条目、按删除忽略并 WARN 的资源数。
	Ignored int
}

// ReconcileDefinitions 逐资源比对定义文件与已发布版本并执行四种决策（约束 34）：
// 一致 → 无动作；文件新且候选合法 → 预写操作、建新版本并发布；文件非法 → WARN 并
// 保留旧版本；文件缺失 → 忽略删除并 WARN。已发布缺失而文件仍在时返回错误，由装配层
// 拒绝启动。重复调用对同一份文件是幂等的（相同哈希跳过，不写操作行）。
// ReconcileDefinitions applies the four-way decision table and is idempotent on a
// repeated startup with the same file content.
func ReconcileDefinitions(ctx context.Context, deps ReconcileDeps) (ReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return ReconcileResult{}, err
	}
	if err := deps.validate(); err != nil {
		return ReconcileResult{}, err
	}
	deps = deps.normalized()

	now := deps.Now()
	actor := core.Actor{
		Subject: core.ActorSubjectCLIPrefix + deps.reconcileUserName(),
		Method:  core.AuthMethodCLI,
		Source:  reconcileSource,
	}

	result := ReconcileResult{}
	if err := reconcileProviderDefinitions(ctx, deps, actor, now, &result); err != nil {
		return result, err
	}
	if err := reconcileSkillWorkCopies(ctx, deps, actor, now, &result); err != nil {
		return result, err
	}
	if err := reconcileMCPServers(ctx, deps, actor, now, &result); err != nil {
		return result, err
	}
	return result, nil
}

// validate 拒绝缺关键依赖的装配：对账要写版本、写操作行、读技能库，缺一件都不空转。
// validate refuses an assembly missing any required dependency.
func (d ReconcileDeps) validate() error {
	if d.ConfigPath == "" {
		return errors.New("启动对账: 定义文件路径不能为空")
	}
	if d.MCPServersPath == "" {
		return errors.New("启动对账: MCP 定义文件路径不能为空")
	}
	if d.Store == nil {
		return errors.New("启动对账: 资源版本仓储不能为空")
	}
	if d.Operations == nil {
		return errors.New("启动对账: 管理操作台账不能为空")
	}
	if d.Skills == nil {
		return errors.New("启动对账: 技能版本库不能为空")
	}
	return nil
}

// normalized 补齐零值依赖；调用前 validate 已保证 Store 非空。
// normalized fills the zero-value dependencies after validate.
func (d ReconcileDeps) normalized() ReconcileDeps {
	if d.Gate == nil {
		d.Gate = &core.ResourceGate{}
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.PublishVersion == nil {
		d.PublishVersion = d.Store.PublishWithVersionState
	}
	return d
}

// reconcileUserName 给出操作主体的本机用户名：优先使用装配显式传入的值（用例与
// 受控环境），否则读操作系统账户；两者都取不到时记 `cli:unknown` 并 WARN——审计行
// 缺名比编造名字更诚实，但不因此拒绝启动。
// reconcileUserName resolves the OS user for the cli:<user> actor subject.
func (d ReconcileDeps) reconcileUserName() string {
	if name := strings.TrimSpace(d.UserName); name != "" {
		return name
	}
	if current, err := user.Current(); err == nil && strings.TrimSpace(current.Username) != "" {
		return current.Username
	}
	d.Logger.Warn("启动对账：无法确定本机用户名，操作主体记为 cli:unknown")
	return "unknown"
}

// reconcileProviderDefinitions 对账 `basalt.yaml` 的 `providers:` 块。文件整体不可读
// （除不存在外）、条目结构不可解析、候选校验失败、单个条目运行时客户端构建失败，都
// 按「候选非法」处理：WARN 点名、保留已发布版本、零写入。文件条目没有发布版本
// （基线也跳过）→ 资源无法进入注册表，返回错误拒绝启动。
// reconcileProviderDefinitions applies the decision table to the providers block.
func reconcileProviderDefinitions(ctx context.Context, deps ReconcileDeps, actor core.Actor,
	now time.Time, result *ReconcileResult,
) error {
	raw, readErr := os.ReadFile(deps.ConfigPath)
	fileMissing := errors.Is(readErr, fs.ErrNotExist)

	var (
		blocks         []definitionBlock
		blocksErr      error
		candidateErr   error
		fileEntries    []workspace.ProviderEntry
		fileIsReadable = readErr == nil
	)
	switch {
	case fileIsReadable:
		blocks, blocksErr = extractDefinitionBlocks(raw, providersBlockKey)
		if blocksErr == nil {
			// 整份候选校验只做一次：块外内容被破坏同样必须在这里被拒。
			fileEntries, candidateErr = workspace.ValidateProviderEntries(raw)
		}
	case fileMissing:
		// 定义文件不在：与「条目被删」同一档——删除只能经 DELETE API，逐资源 WARN。
	default:
		return fmt.Errorf("启动对账：读取模型连接定义 %s: %w", deps.ConfigPath, readErr)
	}
	if blocksErr != nil {
		deps.Logger.Warn("启动对账：模型连接定义文件结构不可解析，保留全部已发布版本",
			"path", deps.ConfigPath, "error", blocksErr)
	}
	if candidateErr != nil {
		deps.Logger.Warn("启动对账：模型连接定义未通过候选校验，本次不发布任何改动",
			"path", deps.ConfigPath, "error", candidateErr)
	}

	blockByName := make(map[string]definitionBlock, len(blocks))
	for _, block := range blocks {
		blockByName[block.name] = block
	}

	// 文件里有、库里没有发布版本：资源无发布版本时 Agent 不得注册（约束 34 第 5 行）。
	// 结构不可解析时无法枚举条目，按「文件无效」保留，不做本项判定。
	if blocksErr == nil {
		for _, block := range blocks {
			if err := requirePublishedProvider(ctx, deps, block.name); err != nil {
				return err
			}
		}
	}

	pointers, err := deps.Store.ListPointers(ctx, core.ResourceModelConnection)
	if err != nil {
		return fmt.Errorf("启动对账：列出模型连接发布指针: %w", err)
	}

	for _, pointer := range pointers {
		name := pointer.ResourceID
		published, err := deps.Store.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return fmt.Errorf("启动对账：读取模型连接 %s 的发布版本: %w", name, err)
		}

		if fileMissing || blocksErr != nil {
			retainProvider(deps, result, name, blocksErr)
			continue
		}
		block, exists := blockByName[name]
		if !exists {
			result.Ignored++
			deps.Logger.Warn("启动对账：定义文件中已没有该条目，保留已发布版本（删除只能经 DELETE API）",
				"kind", string(core.ResourceModelConnection), "resource", name)
			continue
		}
		if definitionBlockMatchesFor(raw, providersBlockKey, name, published) {
			result.Unchanged++
			continue
		}
		if candidateErr != nil {
			retainProvider(deps, result, name, candidateErr)
			continue
		}
		entry, ok := providerEntryNamed(fileEntries, name)
		if !ok {
			retainProvider(deps, result, name, errors.New("条目在候选定义中不可解析"))
			continue
		}
		// 约束 7：候选的运行时客户端必须能在预写之前构建——env 解析本条目 `${VAR}`，
		// 托管条目核对已存凭据与候选定义。失败 → 保留旧版本并点名。
		if err := validateReconciledProviderEntry(ctx, deps, pointer, name, entry); err != nil {
			retainProvider(deps, result, name, err)
			continue
		}
		if err := publishProviderVersion(ctx, deps, actor, now, pointer.ResourceUID, name, block.content); err != nil {
			return err
		}
		result.Published++
	}
	return nil
}

// validateReconciledProviderEntry 在预写之前判定候选条目能否构建运行时客户端：
// env 模式解析本条目精确 `${VAR}`（缺失 / 空值 → 错误点名变量）；托管模式没有占位
// 可解析，改为按**候选** `base_url` 重算目标绑定并向凭据解析面核对——与准入期固定
// 凭据同一判据。没有凭据解析面时托管条目失败关闭（保留已发布版本），**不得**把它当
// 缺 `${VAR}` 占位误报。
// validateReconciledProviderEntry validates a file-new candidate by its declared
// credential mode before the pending operation is written.
func validateReconciledProviderEntry(ctx context.Context, deps ReconcileDeps,
	pointer core.ResourcePointer, name string, entry workspace.ProviderEntry,
) error {
	mode, err := credentialModeOfEntry(name, entry)
	if err != nil {
		return err
	}
	if mode != core.CredentialModeManaged {
		_, err := buildModelConnectionClient(name, entry)
		return err
	}
	if deps.Credentials == nil {
		return fmt.Errorf("模型连接 %s 使用托管凭据，但启动对账没有凭据解析面", name)
	}
	target, err := CredentialTargetBinding(core.BindingModelConnection, entry.BaseURL, "")
	if err != nil {
		return err
	}
	if _, err := deps.Credentials.PinModelCredential(ctx, pointer.ResourceUID, target); err != nil {
		return fmt.Errorf("模型连接 %s 的托管凭据与候选定义不匹配: %w", name, err)
	}
	return nil
}

// retainProvider 记一次「保留已发布版本」并 WARN 点名；err 为空表示文件整体缺失 /
// 不可解析，用固定短语说明原因。
// retainProvider records one retained published version with a naming WARN.
func retainProvider(deps ReconcileDeps, result *ReconcileResult, name string, err error) {
	result.Retained++
	attrs := []any{"kind", string(core.ResourceModelConnection), "resource", name, "path", deps.ConfigPath}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	deps.Logger.Warn("启动对账：候选定义不可用，保留已发布版本", attrs...)
}

// requirePublishedProvider 判定文件条目有未删除的发布指针；没有 → 资源没有发布版本，
// 拒绝启动（约束 34 第 5 行）。删除行同样按「已发布缺失」处理。
// requirePublishedProvider refuses startup when a file entry has no live pointer.
func requirePublishedProvider(ctx context.Context, deps ReconcileDeps, name string) error {
	pointer, err := deps.Store.Pointer(ctx, core.ResourceModelConnection, name)
	if errors.Is(err, core.ErrResourceNotFound) {
		return fmt.Errorf("启动对账：模型连接定义文件里的 %q 没有已发布版本（基线未建立 v1），拒绝启动", name)
	}
	if err != nil {
		return fmt.Errorf("启动对账：读取模型连接 %s 的发布指针: %w", name, err)
	}
	if pointer.DeletedAt != nil {
		return fmt.Errorf("启动对账：模型连接定义文件里的 %q 已没有已发布版本（资源已删除）；删除只能经 DELETE API，拒绝启动", name)
	}
	return nil
}

// providerEntryNamed 在已校验的候选条目里按名取一条。
// providerEntryNamed looks up one parsed candidate entry by name.
func providerEntryNamed(entries []workspace.ProviderEntry, name string) (workspace.ProviderEntry, bool) {
	for _, entry := range entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return workspace.ProviderEntry{}, false
}

// definitionBlockMatchesFor 是分歧闸门的判据（按顶层键参数化）：文件里目标块的当前
// 字节是否仍对应发布版本的 `content_hash`。两种已落库形态都接受——管理写路径存
// `BlockEntry.Raw`（保留缩进与 `- `）的 sha256，基线 / 对账路径存剥掉序列指示符并
// 去缩进后的条目映射字节的 sha256。两个哈希都**不**重新序列化 YAML：基线形态直接
// 复用 `extractDefinitionBlocks` 的同一份切块实现，因此与写入时的哈希逐字节同源。
// definitionBlockMatchesFor accepts both stored shapes and never re-serialises YAML.
func definitionBlockMatchesFor(doc []byte, key, name string, published core.ResourceVersion) bool {
	blocks, err := workspace.BlockEntries(doc, key)
	if err != nil {
		return false
	}
	for _, block := range blocks {
		if block.Name != name {
			continue
		}
		if baselineResourceHash(string(published.Kind), published.ResourceID, block.Raw) == published.ContentHash {
			return true
		}
		break
	}
	definitions, err := extractDefinitionBlocks(doc, key)
	if err != nil {
		return false
	}
	for _, definition := range definitions {
		if definition.name != name {
			continue
		}
		return baselineResourceHash(string(published.Kind), published.ResourceID, definition.content) == published.ContentHash
	}
	return false
}

// definitionBlockAbsent 判定定义文件里**没有**目标条目：两种已落库形态（块式列表与
// 基线映射）都查一遍。它只服务删除重试的幂等补齐（约束 13）：上次删除已把块移出文件
// 但指针未提交时，文件侧已到终态，重试不应再被分歧闸门判成 409。解析失败按"不缺席"
// 处理——看不出来就不声称已经删过。
// definitionBlockAbsent reports that the target block is missing from either accepted
// file shape; parse failures count as "not absent", so a malformed file never gets
// mistaken for a completed delete.
func definitionBlockAbsent(doc []byte, key, name string) bool {
	blocks, err := workspace.BlockEntries(doc, key)
	if err != nil {
		return false
	}
	for _, block := range blocks {
		if block.Name == name {
			return false
		}
	}
	definitions, err := extractDefinitionBlocks(doc, key)
	if err != nil {
		return false
	}
	for _, definition := range definitions {
		if definition.name == name {
			return false
		}
	}
	return true
}

// publishProviderVersion 预写 `model_connection.update` 操作 → 同事务提交版本 / 指针 /
// 终态。发布内容就是文件里该条目的原文（不重新序列化），`applied` 随 published 同步。
// publishProviderVersion pre-writes the operation and commits version, pointer and
// terminal state in one transaction.
func publishProviderVersion(ctx context.Context, deps ReconcileDeps, actor core.Actor,
	now time.Time, uid core.ResourceUID, name string, definition []byte,
) error {
	contentHash := baselineResourceHash(string(core.ResourceModelConnection), name, definition)
	operationID, err := beginReconcileOperation(ctx, deps, actor, now,
		core.ActionModelConnectionUpdate, core.TargetKindModelConnection, name, contentHash)
	if err != nil {
		return err
	}
	published, err := commitReconcileVersion(ctx, deps, core.ResourceVersion{
		Kind:           core.ResourceModelConnection,
		ResourceUID:    uid,
		ResourceID:     name,
		ContentHash:    contentHash,
		DefinitionYAML: string(definition),
		OperationID:    operationID,
		CreatedAt:      now.UnixMilli(),
	}, true, operationID, now)
	if err != nil {
		deps.Logger.Error("启动对账：发布模型连接失败，旧版本保留",
			"resource", name, "operation_id", operationID, "error", err)
		return err
	}
	deps.Logger.Info("启动对账：外部编辑已发布为新版本",
		"kind", string(core.ResourceModelConnection), "resource", name,
		"version_no", published.VersionNo, "operation_id", operationID)
	return nil
}

// reconcileSkillWorkCopies 对账技能工作副本与已发布版本。工作副本目录缺失或没有
// `SKILL.md` 与「条目被删」同一档：WARN 并保留已发布版本。变更内容在冻结快照之后
// 发布；快照冻结失败按数据库写入失败处理（操作回填 failed、拒绝启动）。
// reconcileSkillWorkCopies applies the decision table to skill work copies.
func reconcileSkillWorkCopies(ctx context.Context, deps ReconcileDeps, actor core.Actor,
	now time.Time, result *ReconcileResult,
) error {
	names, err := deps.Skills.Names()
	if err != nil {
		return fmt.Errorf("启动对账：枚举技能工作副本: %w", err)
	}

	type candidate struct {
		work workspace.SkillVersion
		err  error
	}
	candidates := make(map[string]candidate, len(names))
	for _, name := range names {
		work, hashErr := deps.Skills.HashWorkCopy(name)
		if errors.Is(hashErr, fs.ErrNotExist) {
			// 缺 SKILL.md 的目录不是技能实体（基线同口径，约束 36），不进入对账。
			continue
		}
		candidates[name] = candidate{work: work, err: hashErr}
		if err := requirePublishedSkill(ctx, deps, name); err != nil {
			return err
		}
	}

	pointers, err := deps.Store.ListPointers(ctx, core.ResourceSkill)
	if err != nil {
		return fmt.Errorf("启动对账：列出技能发布指针: %w", err)
	}

	for _, pointer := range pointers {
		name := pointer.ResourceID
		entry, ok := candidates[name]
		if !ok {
			result.Ignored++
			deps.Logger.Warn("启动对账：技能工作副本已不存在，保留已发布版本（删除只能经 DELETE API）",
				"kind", string(core.ResourceSkill), "resource", name)
			continue
		}
		if entry.err != nil {
			result.Retained++
			deps.Logger.Warn("启动对账：技能工作副本不可读，保留已发布版本",
				"kind", string(core.ResourceSkill), "resource", name, "error", entry.err)
			continue
		}
		published, err := deps.Store.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return fmt.Errorf("启动对账：读取技能 %s 的发布版本: %w", name, err)
		}
		// 哈希按完整文件清单的固定字节定义（约束 13）：相同即无动作，不查环境、不读快照。
		if entry.work.ContentHash == published.ContentHash {
			result.Unchanged++
			continue
		}
		if err := publishSkillVersion(ctx, deps, actor, now, pointer.ResourceUID, name, entry.work); err != nil {
			return err
		}
		result.Published++
	}
	return nil
}

// requirePublishedSkill 判定工作副本有未删除的发布指针；没有 → 拒绝启动。
// requirePublishedSkill refuses startup when a work copy has no live pointer.
func requirePublishedSkill(ctx context.Context, deps ReconcileDeps, name string) error {
	pointer, err := deps.Store.Pointer(ctx, core.ResourceSkill, name)
	if errors.Is(err, core.ErrResourceNotFound) {
		return fmt.Errorf("启动对账：技能 %q 的工作副本存在但没有已发布版本（基线未建立 v1），拒绝启动", name)
	}
	if err != nil {
		return fmt.Errorf("启动对账：读取技能 %s 的发布指针: %w", name, err)
	}
	if pointer.DeletedAt != nil {
		return fmt.Errorf("启动对账：技能 %q 的工作副本存在但没有已发布版本（资源已删除）；删除只能经 DELETE API，拒绝启动", name)
	}
	return nil
}

// publishSkillVersion 先预写 `skill.update`，再冻结不可变快照，最后同事务提交版本 /
// 指针 / 终态。快照目录按「最新版本号 + 1」物化——版本号只增不补（约束 9）；物化失败
// 时操作回填 failed 并拒绝启动（快照是发布不可分割的一部分）。
// publishSkillVersion materialises the immutable snapshot and then commits version,
// pointer and terminal state in one transaction.
func publishSkillVersion(ctx context.Context, deps ReconcileDeps, actor core.Actor,
	now time.Time, uid core.ResourceUID, name string, work workspace.SkillVersion,
) error {
	// 版本号跟着资源实例走：只按该 UID 取号，同名重建的另一代不参与（v3 身份规则）。
	versions, err := deps.Store.VersionsByUID(ctx, core.ResourceSkill, uid)
	if err != nil {
		return fmt.Errorf("启动对账：列出技能 %s 的版本: %w", name, err)
	}
	if len(versions) == 0 {
		return fmt.Errorf("启动对账：技能 %s 没有可递增的版本号", name)
	}
	nextNo := versions[0].VersionNo + 1

	operationID, err := beginReconcileOperation(ctx, deps, actor, now,
		core.ActionSkillUpdate, core.TargetKindSkill, name, work.ContentHash)
	if err != nil {
		return err
	}
	materialized, err := deps.Skills.Materialize(ctx, name, uid, nextNo, work.ContentHash)
	if err != nil {
		finishReconcileOperation(deps, operationID, core.OperationFailed,
			lifecycleResultInternal, "启动对账冻结技能快照失败")
		return fmt.Errorf("启动对账：技能 %s 无法冻结 v%d 快照: %w", name, nextNo, err)
	}
	manifest, err := marshalSkillFileList(materialized.Files)
	if err != nil {
		finishReconcileOperation(deps, operationID, core.OperationFailed,
			lifecycleResultInternal, "启动对账序列化技能清单失败")
		return fmt.Errorf("启动对账：技能 %s 的清单无法序列化: %w", name, err)
	}
	published, err := commitReconcileVersion(ctx, deps, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  uid,
		ResourceID:   name,
		ContentHash:  materialized.ContentHash,
		ManifestJSON: string(manifest),
		StoragePath:  materialized.Dir,
		OperationID:  operationID,
		CreatedAt:    now.UnixMilli(),
	}, true, operationID, now)
	if err != nil {
		deps.Logger.Error("启动对账：发布技能失败，旧版本保留",
			"resource", name, "operation_id", operationID, "error", err)
		return err
	}
	deps.Logger.Info("启动对账：技能工作副本变更已发布为新版本",
		"kind", string(core.ResourceSkill), "resource", name,
		"version_no", published.VersionNo, "operation_id", operationID)
	return nil
}

// reconcileMCPServers 对账 `mcp_servers.yaml` 的 `servers:` 块（约束 34 的 MCP 分支）。
// 决策表与 Provider 相同（一致 / 文件新且合法 / 非法 / 条目被删），唯一差别是发布用
// `applied=false`（约束 21）：本进程还没连接新内容，`pending_restart` 因此如实为真；
// supervisor 随后启动，连接成功才把 applied 追平，连接失败保留旧 applied 并标记未连接。
// reconcileMCPServers applies the decision table to the servers block; publishes with
// applied=false so the supervisor's later successful connect is what advances applied.
func reconcileMCPServers(ctx context.Context, deps ReconcileDeps, actor core.Actor,
	now time.Time, result *ReconcileResult,
) error {
	raw, readErr := os.ReadFile(deps.MCPServersPath)
	fileMissing := errors.Is(readErr, fs.ErrNotExist)

	var (
		blocks       []definitionBlock
		blocksErr    error
		candidateErr error
	)
	switch {
	case readErr == nil:
		blocks, blocksErr = extractDefinitionBlocks(raw, mcpServersDefinitionsKey)
		if blocksErr == nil {
			// 整份候选校验只做一次：块外内容被破坏同样必须在这里被拒。
			candidateErr = workspace.ValidateMCPServersDocument(raw)
		}
	case fileMissing:
		// 定义文件不在：与「条目被删」同一档——删除只能经 DELETE API，逐资源 WARN。
	default:
		return fmt.Errorf("启动对账：读取 MCP 定义 %s: %w", deps.MCPServersPath, readErr)
	}
	if blocksErr != nil {
		deps.Logger.Warn("启动对账：MCP 定义文件结构不可解析，保留全部已发布版本",
			"path", deps.MCPServersPath, "error", blocksErr)
	}
	if candidateErr != nil {
		deps.Logger.Warn("启动对账：MCP 定义未通过候选校验，本次不发布任何改动",
			"path", deps.MCPServersPath, "error", candidateErr)
	}

	blockByName := make(map[string]definitionBlock, len(blocks))
	for _, block := range blocks {
		blockByName[block.name] = block
	}

	// 文件里有、库里没有发布版本：资源没有发布版本时 Agent 不得注册（约束 34 第 5 行）。
	// 结构不可解析时无法枚举条目，按「文件无效」保留，不做本项判定。
	if blocksErr == nil {
		for _, block := range blocks {
			if err := requirePublishedMCP(ctx, deps, block.name); err != nil {
				return err
			}
		}
	}

	pointers, err := deps.Store.ListPointers(ctx, core.ResourceMCPServer)
	if err != nil {
		return fmt.Errorf("启动对账：列出 MCP server 发布指针: %w", err)
	}

	for _, pointer := range pointers {
		name := pointer.ResourceID
		published, err := deps.Store.VersionByID(ctx, pointer.PublishedVersionID)
		if err != nil {
			return fmt.Errorf("启动对账：读取 MCP server %s 的发布版本: %w", name, err)
		}

		if fileMissing || blocksErr != nil {
			retainMCP(deps, result, name, blocksErr)
			continue
		}
		block, exists := blockByName[name]
		if !exists {
			result.Ignored++
			deps.Logger.Warn("启动对账：定义文件中已没有该条目，保留已发布版本（删除只能经 DELETE API）",
				"kind", string(core.ResourceMCPServer), "resource", name)
			continue
		}
		if definitionBlockMatchesFor(raw, mcpServersDefinitionsKey, name, published) {
			result.Unchanged++
			continue
		}
		if candidateErr != nil {
			retainMCP(deps, result, name, candidateErr)
			continue
		}
		// 逐条目按发布路径同一解析器判定合法性（兼容旧多词 command，与基线 / 运行期
		// 同口径）；解析失败 = 候选非法，保留旧版本。
		cfg, err := workspace.ParseMCPServerBlock(block.content)
		if err != nil {
			retainMCP(deps, result, name, err)
			continue
		}
		if cfg.Name != name {
			retainMCP(deps, result, name, fmt.Errorf("条目名 %q 与资源标识不一致", cfg.Name))
			continue
		}
		// 占位解析在发布前完成：缺失 / 空值 → 保留旧版本，错误里只有键名与变量名。
		if _, err := resolveMCPEnv([]core.MCPServerConfig{cfg}, os.LookupEnv); err != nil {
			retainMCP(deps, result, name, err)
			continue
		}
		if err := publishMCPVersion(ctx, deps, actor, now, pointer.ResourceUID, name, block.content); err != nil {
			return err
		}
		result.Published++
	}
	return nil
}

// retainMCP 记一次「保留已发布版本」并 WARN 点名。
// retainMCP records one retained MCP published version with a naming WARN.
func retainMCP(deps ReconcileDeps, result *ReconcileResult, name string, err error) {
	result.Retained++
	attrs := []any{"kind", string(core.ResourceMCPServer), "resource", name, "path", deps.MCPServersPath}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	deps.Logger.Warn("启动对账：候选定义不可用，保留已发布版本", attrs...)
}

// requirePublishedMCP 判定文件条目有未删除的发布指针；没有 → 拒绝启动（约束 34 第 5 行）。
// requirePublishedMCP refuses startup when a file entry has no live pointer.
func requirePublishedMCP(ctx context.Context, deps ReconcileDeps, name string) error {
	pointer, err := deps.Store.Pointer(ctx, core.ResourceMCPServer, name)
	if errors.Is(err, core.ErrResourceNotFound) {
		return fmt.Errorf("启动对账：MCP server 定义文件里的 %q 没有已发布版本（基线未建立 v1），拒绝启动", name)
	}
	if err != nil {
		return fmt.Errorf("启动对账：读取 MCP server %s 的发布指针: %w", name, err)
	}
	if pointer.DeletedAt != nil {
		return fmt.Errorf("启动对账：MCP server 定义文件里的 %q 已没有已发布版本（资源已删除）；删除只能经 DELETE API，拒绝启动", name)
	}
	return nil
}

// publishMCPVersion 预写 `mcp_server.update` 操作 → 同事务提交版本 / 指针 / 终态。
// 与 Provider 的唯一差别是 `applied=false`：新内容要等 supervisor 连接成功才生效
// （约束 21），管理面据此显示「待重启」。
// publishMCPVersion pre-writes the operation and commits version, pointer and terminal
// state in one transaction, leaving applied untouched until the next successful connect.
func publishMCPVersion(ctx context.Context, deps ReconcileDeps, actor core.Actor,
	now time.Time, uid core.ResourceUID, name string, definition []byte,
) error {
	contentHash := baselineResourceHash(string(core.ResourceMCPServer), name, definition)
	operationID, err := beginReconcileOperation(ctx, deps, actor, now,
		core.ActionMCPServerUpdate, core.TargetKindMCPServer, name, contentHash)
	if err != nil {
		return err
	}
	published, err := commitReconcileVersion(ctx, deps, core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceUID:    uid,
		ResourceID:     name,
		ContentHash:    contentHash,
		DefinitionYAML: string(definition),
		OperationID:    operationID,
		CreatedAt:      now.UnixMilli(),
	}, false, operationID, now)
	if err != nil {
		deps.Logger.Error("启动对账：发布 MCP server 失败，旧版本保留",
			"resource", name, "operation_id", operationID, "error", err)
		return err
	}
	deps.Logger.Info("启动对账：MCP 定义外部编辑已发布为新版本（待重启生效）",
		"kind", string(core.ResourceMCPServer), "resource", name,
		"version_no", published.VersionNo, "operation_id", operationID)
	return nil
}

// beginReconcileOperation 预写一条 `pending` 操作行；预写失败即拒绝执行（约束 1 /
// 宪法 V）。`request_hash` 是定义内容的 sha256（内容里只有 `${VAR}` 占位，不含凭证）。
// beginReconcileOperation pre-writes the pending row; a failed pre-write refuses the
// change.
func beginReconcileOperation(ctx context.Context, deps ReconcileDeps, actor core.Actor,
	now time.Time, action, targetKind, targetID, requestHash string,
) (string, error) {
	operationID := core.NewOperationID(now)
	record := core.OperationRecord{
		OperationID:  operationID,
		Action:       action,
		TargetKind:   targetKind,
		TargetID:     targetID,
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash:  requestHash,
		Status:       core.OperationPending,
		DetailJSON:   []byte(reconcileDetailJSON),
		CreatedAt:    now,
	}
	if err := deps.Operations.BeginOperation(ctx, record); err != nil {
		deps.Logger.Error("启动对账：预写管理操作失败，拒绝执行",
			"kind", targetKind, "resource", targetID, "action", action, "error", err)
		return "", fmt.Errorf("启动对账：预写管理操作失败: %w", err)
	}
	return operationID, nil
}

// commitReconcileVersion 在运行准入闸门的写锁内提交版本 / 指针 / 操作终态（同一事务）。
// 失败时回填 `failed` 并返回错误——对账落库失败 = 拒绝启动（注 7）；已写入的版本与
// 指针由事务回滚，旧指针保持。
// commitReconcileVersion commits under the admission gate's write lock and finishes
// the operation as failed on any error.
func commitReconcileVersion(ctx context.Context, deps ReconcileDeps, version core.ResourceVersion,
	applied bool, operationID string, now time.Time,
) (core.ResourceVersion, error) {
	deps.Gate.Lock()
	published, err := deps.PublishVersion(ctx, version, applied, operationID, now)
	deps.Gate.Unlock()
	if err != nil {
		finishReconcileOperation(deps, operationID, core.OperationFailed,
			lifecycleResultInternal, "启动对账发布失败")
		return core.ResourceVersion{}, fmt.Errorf("启动对账：提交 %s/%s 失败: %w",
			version.Kind, version.ResourceID, err)
	}
	return published, nil
}

// finishReconcileOperation 回填终态；回填失败只记日志——启动恢复会按动作组查证
// `pending` 行，不能让它卡住错误路径。
// finishReconcileOperation writes the terminal state without letting a ledger failure
// mask the original error.
func finishReconcileOperation(deps ReconcileDeps, operationID string, status core.OperationStatus,
	code, message string,
) {
	if err := deps.Operations.FinishOperation(context.Background(), operationID,
		status, code, message, nil, deps.Now()); err != nil {
		deps.Logger.Error("启动对账：回填操作终态失败，留待启动恢复查证",
			"operation_id", operationID, "status", string(status), "error", err)
	}
}
