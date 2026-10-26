package app

// 本文件是**启动基线**与**管理面恢复**（颗粒度文档 §3.1.19 / 约束 13 / 约束 38 /
// 图 A ③④ / 图 C）：迁移之后、开放服务之前，把"库里的资源发布指针"和"磁盘上的工作副本与
// 状态文件"对齐；再把上次崩溃留下的 pending 管理操作逐条**查证**并回填终态。
// 两条纪律贯穿全文件：① 只凭内容哈希与状态文件判断，不凭时间戳猜；② 恢复只查证、不重做
// ——重放一次停用会让 generation 跳两格，重放一次内容写会覆盖第三方的修改。
// This file runs the startup baseline and the management recovery. The baseline aligns
// published pointers with the workspace before serving; recovery verifies pending
// operations and writes their terminal state — it never replays a change.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/workspace"
)

// 管理操作与目标的固定字面量（契约 §9.3 / 约束 12 的写操作闭集）。
// The fixed literals of the management actions and target kinds this file writes.
const (
	actionBaseline      = "system.baseline_v2"
	actionContentUpdate = "agent.content.update"
	actionDisable       = "agent.disable"
	actionEnable        = "agent.enable"

	targetKindSystem = "system"
	targetKindAgent  = "agent"
)

// 六个凭据 / 范围动作的固定字面量（契约 §9.3）：`target_id` 是资源 `resource_uid`
// （不是名称），`detail_json` 只记资源名与凭据元数据，不记明文。
// The six credential / scope action literals; their target id is the resource uid.
const (
	actionModelConnectionCredentialWrite  = "model_connection.credential.write"
	actionModelConnectionCredentialRevoke = "model_connection.credential.revoke"
	actionModelConnectionScope            = "model_connection.scope"
	actionNotifyChannelCredentialWrite    = "notify_channel.credential.write"
	actionNotifyChannelCredentialRevoke   = "notify_channel.credential.revoke"
	actionNotifyChannelScope              = "notify_channel.scope"
)

// 状态文件的 schema 版本（与 internal/workspace/lifecycle.go 的
// lifecycleSchemaVersionV1 / lifecycleSchemaVersion 同值）：v1 是 US-11 形态、不带
// `agent_uid`，v2（US-12 R4）追加该字段。基线 v3 的恢复判据只区分这两档。
// Lifecycle state-file schema versions: v1 carries no agent_uid, v2 adds it.
const (
	lifecycleFileSchemaV1 = 1
	lifecycleFileSchemaV2 = 2
)

const (
	// baselineVersionNo 是基线建立的固定版本号：首版永远是 1（契约 §9.6）。
	// The baseline always writes version 1; later versions belong to explicit publishes.
	baselineVersionNo = 1

	// baselineResultOK / baselineResultInternal 是 result_code 的两个固定取值（契约 §7.4）。
	// The two fixed result codes used by the baseline operation.
	baselineResultOK       = "OK"
	baselineResultInternal = "INTERNAL"

	// baselineSource 是迁移流程的 source 字面量；与 `core.ActorSubjectMigration` 同值但
	// 语义不同（一个是 source 列，一个是 actor_subject 列），故各写一份、不互相引用。
	// The source literal of a migration-started operation.
	baselineSource = "migration"

	// mcpServersFileName 与 workspace 包内的同名常量是同一份约定（未导出，引用不到）。
	// The MCP definition file name, mirroring workspace's unexported constant.
	mcpServersFileName = "mcp_servers.yaml"

	// providerDefinitionsKey / mcpServersDefinitionsKey 是两份原文里的顶层键。
	// The top-level keys holding the provider and MCP server definitions.
	providerDefinitionsKey   = "providers"
	mcpServersDefinitionsKey = "servers"
)

// BaselineDeps 是启动基线的全部依赖（§3.1.19 字面量逐字；`Now` 显式传入以便重放）。
// BaselineDeps carries every dependency of the startup baseline; the clock is explicit.
type BaselineDeps struct {
	Operations core.OperationStore
	Resources  *storage.ResourceStore
	Skills     *workspace.SkillLibrary
	Lifecycle  *workspace.LifecycleStore
	Providers  []ProviderDefinition
	MCPServers []MCPServerDefinition
	Agents     []core.AgentEntry
	Logger     *slog.Logger
	Now        func() time.Time
	// NewUID 是"指针缺失、待建首版"资源的身份分配器（缺省 `newResourceUID`）。
	// 生产装配保持随机身份；夹具用确定性分配把版本目录变成可预期的断言目标。
	// 已存在版本行的名称不会走到这里——那种情况复用既有 UID，不补发第二个身份。
	// NewUID mints identities for resources that have no version yet; tests inject a
	// deterministic allocator so the version directory is a predictable assertion target.
	NewUID func(kind core.ResourceKind, name string) (core.ResourceUID, error)
}

// RecoveryDeps 是启动恢复的全部依赖；与 runBaseline 分开，恢复只查证、不重做。
// US-11 的查证只需要磁盘与状态文件；US-12 按约束 44 扩到定义文件、版本行、发布指针、
// 归档行与通知渠道行，因此新增的依赖一律**可选**：某一组查证的依赖缺失时该组只能
// 判 indeterminate（→ unknown），**不得**因为装配少一项就把操作猜成成功或失败。
// 生产装配在 wire.go 一次给全；`Registry` / `Scheduler` 在 ⑤ / ⑦ 才构建，恢复只
// 查证它们的持久面，不读它们的当前值。
// RecoveryDeps carries the recovery dependencies. The US-12 fields are optional on
// purpose: a verifier whose dependency is missing returns indeterminate (unknown)
// instead of guessing, because an incomplete assembly is never proof of an outcome.
type RecoveryDeps struct {
	Operations core.OperationStore
	Resources  *storage.ResourceStore
	Lifecycle  *workspace.LifecycleStore
	Editor     *workspace.AgentEditor
	Registry   *agentRegistry
	Scheduler  *core.Scheduler
	Logger     *slog.Logger
	Now        func() time.Time

	// Root 是工作区根（`.basalt`）：Agent 目录、技能工作副本与 `mcp_servers.yaml`
	// 都在它下面（约束 44 的"目标文件树"）。
	// Root is the workspace root (.basalt) holding agent dirs, skill work copies and
	// mcp_servers.yaml.
	Root string
	// ConfigPath 是实例配置（`basalt.yaml`）路径：模型连接定义的唯一宿主文件。
	// ConfigPath is the instance config file owning the provider definitions.
	ConfigPath string
	// Skills 是技能库：工作副本哈希是技能写操作唯一的文件侧判据。
	// Skills provides the work-copy hash used by every skill verdict.
	Skills *workspace.SkillLibrary
	// Gate 是进程级 Agent 写锁表：原名恢复的崩溃窗口补偿必须在它里面做（约束 44）。
	// Gate is the per-agent write lock the original-name restore compensation needs.
	Gate *core.AgentGate
	// Archives 是归档行仓储：归档 / 恢复按 `operation_id` 与 `restored_at` 查证。
	// Archives backs the archive and restore verdicts.
	Archives *storage.AgentAdminStore
	// Notify 是通知渠道仓储：按行存在性与脱敏字段哈希查证。
	// Notify backs the notify-channel verdicts.
	Notify *storage.NotifyAdminStore
	// Credentials 是凭据仓储：凭据动作的四证（提交事实 / 版本状态 / 目标绑定哈希）
	// 由它回答；缺失时该组只能判 indeterminate。
	// Credentials backs the credential-action evidence.
	Credentials *storage.CredentialStore
	// Schema 回答库当前 schema 版本：v3 迁移操作的完成判据之一是库已在目标版本。
	// Schema answers the current schema version for the v3-migration verdict.
	Schema SchemaVersionSource
	// NetworkPolicy 是网络域名白名单仓储：恢复按修订号与候选域名哈希判定三态。
	// NetworkPolicy backs the allowlist verdict; nil means indeterminate only.
	NetworkPolicy core.NetworkDomainPolicyStore
	// NetworkExceptions 是访问例外仓储：新增按 `exception_id` + `operation_id` 认领
	// 行，删除按行是否仍在判定三态（契约 §9.4）。nil 只意味着该组判 unknown。
	// NetworkExceptions backs the exception verdicts; nil means indeterminate only.
	NetworkExceptions core.NetworkExceptionStore
	// ExecutionPolicy 是执行档位单行表仓储：恢复按前后档位与修订号判定三态。档位行
	// 没有"候选哈希"——一行表 + 修订号 CAS 本身就足以区分"这次写进去了"与"没有"。
	// ExecutionPolicy backs the profile verdict; the row plus its revision fence is
	// enough to tell "this write landed" from "it did not".
	ExecutionPolicy core.ExecutionPolicyStore
	// InstanceSecrets 是实例初始化记录仓储：`system.secrets_init` 的查证要读它。
	// InstanceSecrets backs the secrets-init verdict; nil means indeterminate only.
	InstanceSecrets *storage.InstanceSecretsStore
	// MasterKeyFile 是主密钥文件路径：查证要走完整 `LoadMasterKey` 并核对链接数。
	// MasterKeyFile is the key path the secrets-init verdict re-reads.
	MasterKeyFile string
}

// SchemaVersionSource 回答库当前的 schema 版本（`*storage.DB` 满足）。
// SchemaVersionSource reports the schema version held by the database.
type SchemaVersionSource interface {
	SchemaVersion(ctx context.Context) (int, error)
}

// ProviderDefinition 是一个模型连接定义的原文（配置文件中该条目逐行保真）。
// ProviderDefinition is the verbatim text of one model-connection entry.
type ProviderDefinition struct {
	Name    string
	Content []byte
}

// MCPServerDefinition 是一个 MCP server 定义的原文（同上逐行保真）。
// MCPServerDefinition is the verbatim text of one MCP server entry.
type MCPServerDefinition struct {
	Name    string
	Content []byte
}

// validate 在任何写入之前拒绝残缺依赖——缺一个仓储就写不出可核对的基线。
func (d BaselineDeps) validate() error {
	switch {
	case d.Operations == nil:
		return errors.New("启动基线：缺少管理操作仓储")
	case d.Resources == nil:
		return errors.New("启动基线：缺少资源仓储")
	case d.Skills == nil:
		return errors.New("启动基线：缺少技能库")
	case d.Lifecycle == nil:
		return errors.New("启动基线：缺少生命周期存储")
	}
	return nil
}

func (d BaselineDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func (d BaselineDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// validate 与 BaselineDeps 同一口径：恢复路径的依赖残缺同样是启动失败。
func (d RecoveryDeps) validate() error {
	switch {
	case d.Operations == nil:
		return errors.New("启动恢复：缺少管理操作仓储")
	case d.Resources == nil:
		return errors.New("启动恢复：缺少资源仓储")
	case d.Lifecycle == nil:
		return errors.New("启动恢复：缺少生命周期存储")
	}
	return nil
}

func (d RecoveryDeps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func (d RecoveryDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// resourceWork 是一条待建 / 待补的资源工作项。
// `versionID > 0` 表示指针已发布但**尚未生效**（MCP 首版待重启，或发布被中断）：
// 处理方式是把已有版本补成 applied，**不**建新版本、**不**动发布指针的内容。
// resourceWork is one pending resource item; a positive VersionID means the published
// version still has to become the applied one — no new version is created.
type resourceWork struct {
	kind       core.ResourceKind
	id         string
	definition []byte
	versionID  int64
	// uid 是建首版时要分配的资源身份（v3 起版本行必带 UID）；`versionID > 0`
	// 的补 applied 路径不需要它——指针行的身份取自既有版本行。
	// uid is the identity of a first version; the catch-up path takes it from the
	// existing version row instead.
	uid core.ResourceUID
}

// skillWork 是一条待建 / 待修技能项：`create` 建 v1 并物化快照，否则只按已发布版本
// 补回缺失的不可变快照（版本号、哈希、指针一律不动）。
// skillWork either creates version 1 or repairs the snapshot of an already published
// version, leaving version number, hash and pointer untouched.
type skillWork struct {
	name    string
	create  bool
	work    workspace.SkillVersion
	version core.ResourceVersion
	// uid 只在 `create` 时有意义：建首版要分配身份（复用中断基线留下的那一代）。
	// uid only matters on the create path and is reused from an interrupted baseline.
	uid core.ResourceUID
}

// agentWork 是一条待建 / 待补状态文件或索引的 Agent 项。
// `bootstrap` 为真表示状态文件缺失；否则 `record` 非零表示只用状态文件的当前值
// 重建可重建的 `agent_lifecycle` 索引。
// agentWork bootstraps a missing state file or rebuilds the rebuildable index row.
type agentWork struct {
	name      string
	bootstrap bool
	record    *core.LifecycleRecord
}

// baselineWork 是一次扫描得出的全部待办；空集表示"已就绪"。
// baselineWork is the full set of pending items; an empty value means "already built".
type baselineWork struct {
	providers []resourceWork
	mcp       []resourceWork
	skills    []skillWork
	agents    []agentWork
}

func (w baselineWork) empty() bool {
	return len(w.providers) == 0 && len(w.mcp) == 0 && len(w.skills) == 0 && len(w.agents) == 0
}

// baselineCounts 是 `system.baseline_v2` 成功时写进 `detail_json` 的计数：**本次操作
// 实际处理的条目数**（不是资源总量——行要回答"这次基线补了什么"，不是"库里有几个"）。
// baselineCounts records how many items this very operation processed.
type baselineCounts struct {
	Providers int `json:"providers"`
	MCP       int `json:"mcp"`
	Skills    int `json:"skills"`
	Agents    int `json:"agents"`
}

// marshalBaselineCounts 序列化计数：键与个数由结构体固定，空操作也写全四个 0。
func marshalBaselineCounts(counts baselineCounts) ([]byte, error) {
	raw, err := json.Marshal(counts)
	if err != nil {
		return nil, fmt.Errorf("序列化基线计数: %w", err)
	}
	return raw, nil
}

// baselineResourceHash 给出资源定义的 `content_hash`：**只对定义原文的字节**取 sha256，
// 64 位小写十六进制、无前缀。kind / id 不参与哈希——哈希对象是"这份内容"，资源身份由
// `EnsureVersion` 的 `(kind, resource_id, content_hash)` 三元组另行区分。
// The resource hash covers the definition bytes only; resource identity is a separate
// dimension held by the (kind, resource_id, content_hash) triple.
func baselineResourceHash(kind, id string, definition []byte) string {
	// kind / id 故意不参与：签名保留是为了让每个调用点都显式说出"哪个资源的哪份内容"，
	// 而不是靠调用顺序猜。The identity parameters are deliberately unused; the hash
	// covers content only.
	sum := sha256.Sum256(definition)
	return hex.EncodeToString(sum[:])
}

// runBaseline 在迁移之后、开放服务之前执行 v2 基线（图 A ③）：
// 扫描 → 只在确实有活（或已有 pending 基线）时预写操作 → 逐项幂等补齐 → 回填终态。
// 无活可做时**不写操作行、不改状态文件**（注 13）；任一失败都回填 `failed` 并返回错误，
// 由装配层拒绝启动。
// runBaseline scans first, pre-writes the operation only when there is work, applies
// every item idempotently, and finishes the operation; any failure refuses startup.
func runBaseline(ctx context.Context, deps BaselineDeps) error {
	if err := deps.validate(); err != nil {
		return err
	}
	logger := deps.logger()
	now := deps.now()

	work, err := scanBaselineWork(ctx, deps)
	if err != nil {
		return fmt.Errorf("启动基线：扫描待建项失败: %w", err)
	}
	opID, pending, err := findPendingBaseline(ctx, deps.Operations)
	if err != nil {
		return err
	}
	if work.empty() && !pending {
		logger.Info("启动基线：无需补建，跳过")
		return nil
	}

	if pending {
		logger.Info("启动基线：复用未完成的基线操作", "operation_id", opID)
	} else {
		opID = core.NewOperationID(now)
		record := core.OperationRecord{
			OperationID:  opID,
			Action:       actionBaseline,
			TargetKind:   targetKindSystem,
			TargetID:     targetKindSystem,
			ActorSubject: core.ActorSubjectMigration,
			AuthMethod:   core.AuthMethodMigration,
			Source:       baselineSource,
			Status:       core.OperationPending,
			CreatedAt:    now,
		}
		if beginErr := deps.Operations.BeginOperation(ctx, record); beginErr != nil {
			// 预写失败 = 拒绝执行（宪法 V）：基线一项也不做，启动失败。
			return fmt.Errorf("启动基线：预写操作记录失败，拒绝执行: %w", beginErr)
		}
		logger.Info("启动基线：开始", "operation_id", opID)
	}

	counts, applyErr := applyBaselineWork(ctx, deps, work, opID, now)
	if applyErr != nil {
		if finishErr := deps.Operations.FinishOperation(ctx, opID, core.OperationFailed,
			baselineResultInternal, applyErr.Error(), nil, now); finishErr != nil {
			applyErr = errors.Join(applyErr, fmt.Errorf("回填基线失败终态: %w", finishErr))
		}
		return fmt.Errorf("启动基线：%w", applyErr)
	}

	detail, err := marshalBaselineCounts(counts)
	if err != nil {
		return fmt.Errorf("启动基线：%w", err)
	}
	if err := deps.Operations.FinishOperation(ctx, opID, core.OperationSuccess,
		baselineResultOK, "", detail, now); err != nil {
		return fmt.Errorf("启动基线：回填成功终态失败: %w", err)
	}
	logger.Info("启动基线：完成", "operation_id", opID,
		"providers", counts.Providers, "mcp", counts.MCP,
		"skills", counts.Skills, "agents", counts.Agents)
	return nil
}

// findPendingBaseline 找未完成的基线操作：0 条 = 新建，恰 1 条 = 复用其
// `operation_id`（约束 13 的可重入口径），多条 = 无法判断复用哪一条，拒绝启动。
// findPendingBaseline reuses exactly one pending baseline row and refuses ambiguity.
func findPendingBaseline(ctx context.Context, operations core.OperationStore) (string, bool, error) {
	opID, pending, err := findPendingAction(ctx, operations, actionBaseline)
	if err != nil {
		return "", false, fmt.Errorf("启动基线：%w", err)
	}
	return opID, pending, nil
}

// findPendingAction 是上面两条"启动期迁移类操作"共用的查找面：按动作过滤未完成
// 操作，0 条 = 新建、恰 1 条 = 复用、多条 = 拒绝（不猜复用哪一条）。
// findPendingAction is the shared pending lookup; several rows of one action is refused.
func findPendingAction(ctx context.Context, operations core.OperationStore, action string) (string, bool, error) {
	pending, err := operations.PendingOperations(ctx)
	if err != nil {
		return "", false, fmt.Errorf("读取未完成操作失败: %w", err)
	}
	var ids []string
	for _, op := range pending {
		if op.Action == action {
			ids = append(ids, op.OperationID)
		}
	}
	switch len(ids) {
	case 0:
		return "", false, nil
	case 1:
		return ids[0], true, nil
	default:
		return "", false, fmt.Errorf("发现 %d 条未完成的 %s 操作（%s），无法确定复用哪一条",
			len(ids), action, strings.Join(ids, "、"))
	}
}

// scanBaselineWork 是**只读**的就绪扫描：算出每个资源的内容哈希与每个 Agent 的状态文件
// 情况，得出待办清单。扫描期间不写任何持久面——"无活不写操作行"要求先看清再做。
// scanBaselineWork is the read-only readiness scan; nothing is written before it ends.
func scanBaselineWork(ctx context.Context, deps BaselineDeps) (baselineWork, error) {
	var work baselineWork

	for _, def := range deps.Providers {
		item, needed, err := scanResource(ctx, deps, core.ResourceModelConnection, def.Name, def.Content)
		if err != nil {
			return baselineWork{}, err
		}
		if needed {
			work.providers = append(work.providers, item)
		}
	}
	for _, def := range deps.MCPServers {
		item, needed, err := scanResource(ctx, deps, core.ResourceMCPServer, def.Name, def.Content)
		if err != nil {
			return baselineWork{}, err
		}
		if needed {
			work.mcp = append(work.mcp, item)
		}
	}
	skills, err := scanSkills(ctx, deps)
	if err != nil {
		return baselineWork{}, err
	}
	work.skills = skills
	agents, err := scanAgents(ctx, deps)
	if err != nil {
		return baselineWork{}, err
	}
	work.agents = agents
	return work, nil
}

// scanResource 判定一个 Provider / MCP 定义是否已有发布版本并已生效：
// 指针缺失 → 建 v1；指针已撤回 → 不复活（撤回是显式管理动作）；
// 已发布但未 applied → 补 applied；内容与已发布版本不一致 → 只 WARN"待发布"，
// **不自动升版**（注 9：基线的职责是补缺，不是发布）。
// scanResource never auto-publishes a changed definition; it only fills missing
// pointers and catches up the applied pointer.
func scanResource(ctx context.Context, deps BaselineDeps, kind core.ResourceKind, id string, definition []byte) (resourceWork, bool, error) {
	item := resourceWork{kind: kind, id: id, definition: definition}

	pointer, err := deps.Resources.Pointer(ctx, kind, id)
	switch {
	case errors.Is(err, core.ErrResourceNotFound):
		uid, uidErr := baselineResourceUID(ctx, deps, kind, id)
		if uidErr != nil {
			return resourceWork{}, false, uidErr
		}
		item.uid = uid
		return item, true, nil
	case err != nil:
		return resourceWork{}, false, err
	}
	if pointer.DeletedAt != nil {
		deps.logger().Warn("启动基线：资源定义仍在，但发布指针已撤回——不自动恢复",
			"resource_kind", string(kind), "resource_id", id)
		return resourceWork{}, false, nil
	}

	version, err := deps.Resources.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return resourceWork{}, false, err
	}
	if hash := baselineResourceHash(string(kind), id, definition); version.ContentHash != hash {
		deps.logger().Warn("启动基线：工作区定义与已发布版本不一致——待发布（基线不自动升版）",
			"resource_kind", string(kind), "resource_id", id,
			"published_version", version.VersionNo)
	}
	if pointer.AppliedVersionID == nil || *pointer.AppliedVersionID != pointer.PublishedVersionID {
		// 契约 §9.6：MCP 首版在重启时补齐 applied；Provider 的发布立即生效，走到这里
		// 只可能是发布被中断。两种都按"把已发布版本补成 applied"处理。
		item.versionID = pointer.PublishedVersionID
		return item, true, nil
	}
	return resourceWork{}, false, nil
}

// baselineResourceUID 为"指针缺失、待建首版"的资源确定身份。库里现存该名称的版本行
// 只可能来自"上次基线在建版与发布之间中断"——复用那一代的 UID，**不**给同一实例补发
// 第二个身份（agent 侧同一条纪律）。同一名称下出现两代 UID 说明库被外部改写，无法
// 判定该沿用哪一个：fail-closed 拒绝启动，不按名称猜。
// baselineResourceUID reuses the uid of an interrupted baseline insert instead of
// minting a second identity for the same instance; two uids for one name fail closed.
func baselineResourceUID(ctx context.Context, deps BaselineDeps, kind core.ResourceKind, name string) (core.ResourceUID, error) {
	versions, err := deps.Resources.Versions(ctx, kind, name)
	if err != nil {
		return "", fmt.Errorf("读取 %s %s 的既有版本: %w", kind, name, err)
	}
	if len(versions) == 0 {
		if deps.NewUID != nil {
			return deps.NewUID(kind, name)
		}
		return newResourceUID(kind, name)
	}
	uid := versions[0].ResourceUID
	for _, version := range versions[1:] {
		if version.ResourceUID != uid {
			return "", fmt.Errorf("%s %s 的既有版本分属两个以上资源身份（%s / %s），基线不猜测应沿用哪一个",
				kind, name, uid, version.ResourceUID)
		}
	}
	return uid, nil
}

// scanSkills 逐个技能判定：不是技能实体（缺 SKILL.md）→ WARN 跳过；没有发布指针 → 建 v1；
// 已发布版本快照缺失 → 按该版本补物化；工作副本内容变了 → 只 WARN"待发布"（注 9）。
// scanSkills skips non-entities with a warning and only fixes missing pointers or
// missing snapshots.
func scanSkills(ctx context.Context, deps BaselineDeps) ([]skillWork, error) {
	names, err := deps.Skills.Names()
	if err != nil {
		return nil, err
	}

	var work []skillWork
	for _, name := range names {
		copyVersion, hashErr := deps.Skills.HashWorkCopy(name)
		if hashErr != nil {
			if errors.Is(hashErr, fs.ErrNotExist) {
				// 缺 SKILL.md 的目录不是技能实体（注册不变量，约束 36）。
				deps.logger().Warn("启动基线：技能目录缺少 SKILL.md，不建发布版本", "skill", name)
				continue
			}
			return nil, hashErr
		}

		pointer, ptrErr := deps.Resources.Pointer(ctx, core.ResourceSkill, name)
		switch {
		case errors.Is(ptrErr, core.ErrResourceNotFound):
			uid, uidErr := baselineResourceUID(ctx, deps, core.ResourceSkill, name)
			if uidErr != nil {
				return nil, uidErr
			}
			work = append(work, skillWork{name: name, create: true, work: copyVersion, uid: uid})
			continue
		case ptrErr != nil:
			return nil, ptrErr
		}
		if pointer.DeletedAt != nil {
			deps.logger().Warn("启动基线：技能仍在工作区，但发布指针已撤回——不自动恢复", "skill", name)
			continue
		}

		version, versionErr := deps.Resources.VersionByID(ctx, pointer.PublishedVersionID)
		if versionErr != nil {
			return nil, versionErr
		}
		if version.ContentHash != copyVersion.ContentHash {
			deps.logger().Warn("启动基线：技能内容已变化——待发布（基线不自动升版）",
				"skill", name, "published_version", version.VersionNo)
		}
		if _, loadErr := deps.Skills.LoadPublished(ctx, name, version.VersionNo, version.StoragePath); loadErr != nil {
			if !errors.Is(loadErr, fs.ErrNotExist) {
				return nil, fmt.Errorf("技能 %s 的发布版本 v%d 不可用: %w", name, version.VersionNo, loadErr)
			}
			// 快照目录缺失：按**已发布版本**的哈希与原编号重新物化（不动指针）。
			work = append(work, skillWork{name: name, version: version})
		}
	}
	return work, nil
}

// scanAgents 逐个 Agent 判定：状态文件缺失 → 建 disabled / generation=1；
// 状态文件损坏 → **整个基线失败**（约束 13，不得静默重建）；索引缺失或与状态文件
// 不一致 → 只用状态文件重建索引（索引是派生面，写失败不阻止启动）。
// scanAgents bootstraps missing state files, refuses corrupt ones, and reconciles the
// rebuildable index against the authoritative file.
func scanAgents(ctx context.Context, deps BaselineDeps) ([]agentWork, error) {
	var work []agentWork
	for _, entry := range deps.Agents {
		record, exists, err := deps.Lifecycle.Read(ctx, entry.Name)
		switch {
		case err == nil && exists:
			index, ok, _, indexErr := deps.Resources.LifecycleIndex(ctx, entry.Name)
			if indexErr != nil {
				return nil, indexErr
			}
			if ok && index.State == record.State && index.Generation == record.Generation &&
				index.OperationID == record.OperationID && index.Checksum == record.Checksum {
				continue
			}
			current := record
			work = append(work, agentWork{name: entry.Name, record: &current})
		case errors.Is(err, fs.ErrNotExist):
			work = append(work, agentWork{name: entry.Name, bootstrap: true})
		default:
			// 损坏 / 不可读：拒绝启动，绝不覆盖。错误里带 Agent 名，便于定位是哪份文件。
			return nil, fmt.Errorf("生命周期状态文件不可用（拒绝覆盖），Agent %s: %w", entry.Name, err)
		}
	}
	return work, nil
}

// applyBaselineWork 逐项补齐待办并计数；任一项失败立刻返回，由调用方回填 failed。
// 顺序固定（providers → mcp → skills → agents）：顺序本身是审计可复现的一部分。
func applyBaselineWork(ctx context.Context, deps BaselineDeps, work baselineWork, opID string, now time.Time) (baselineCounts, error) {
	var counts baselineCounts

	for _, item := range work.providers {
		if err := applyResourceWork(ctx, deps, item, opID, now); err != nil {
			return counts, err
		}
		counts.Providers++
	}
	for _, item := range work.mcp {
		if err := applyResourceWork(ctx, deps, item, opID, now); err != nil {
			return counts, err
		}
		counts.MCP++
	}
	for _, item := range work.skills {
		if err := applySkillWork(ctx, deps, item, opID, now); err != nil {
			return counts, err
		}
		counts.Skills++
	}
	for _, item := range work.agents {
		if err := applyAgentWork(ctx, deps, item, opID); err != nil {
			return counts, err
		}
		counts.Agents++
	}
	return counts, nil
}

// applyResourceWork 建 v1 并发布（或只把已发布版本补成 applied）。
// 同哈希版本已存在时复用——`EnsureVersion` 的幂等键就是内容哈希。
func applyResourceWork(ctx context.Context, deps BaselineDeps, item resourceWork, opID string, now time.Time) error {
	if item.versionID > 0 {
		if err := deps.Resources.Publish(ctx, item.kind, item.id, item.versionID, true, opID, now); err != nil {
			return fmt.Errorf("资源 %s/%s 补齐 applied 指针: %w", item.kind, item.id, err)
		}
		return nil
	}

	version, inserted, err := deps.Resources.EnsureVersion(ctx, core.ResourceVersion{
		Kind:           item.kind,
		ResourceUID:    item.uid,
		ResourceID:     item.id,
		VersionNo:      baselineVersionNo,
		ContentHash:    baselineResourceHash(string(item.kind), item.id, item.definition),
		DefinitionYAML: string(item.definition),
		CreatedAt:      now.UnixMilli(),
		OperationID:    opID,
	})
	if err != nil {
		return fmt.Errorf("资源 %s/%s 建首版: %w", item.kind, item.id, err)
	}
	if inserted && version.VersionNo != baselineVersionNo {
		// 唯一键冲突重试会把版本号换到下一个：说明库里已有首版且内容不同，
		// 基线**不得**借机升版（注 9），直接拒绝启动。
		return fmt.Errorf("资源 %s/%s 的首版已被其它内容占用（本次插入被编号为 v%d），基线不自动升版",
			item.kind, item.id, version.VersionNo)
	}
	if err := deps.Resources.Publish(ctx, item.kind, item.id, version.ID, true, opID, now); err != nil {
		return fmt.Errorf("资源 %s/%s 发布首版: %w", item.kind, item.id, err)
	}
	return nil
}

// applySkillWork 建 v1（先物化快照再挂指针）或按已发布版本补回缺失快照。
// 走"先查同哈希版本"而不是直接 `EnsureVersion`，是为了避免在已存在其它版本号时
// 先物化出一个用不上的目录（物化有副作用，插入没有）。
// applySkillWork materialises the snapshot before publishing v1, and repairs a
// missing snapshot from the published version without touching the pointer.
func applySkillWork(ctx context.Context, deps BaselineDeps, item skillWork, opID string, now time.Time) error {
	if !item.create {
		materialized, err := deps.Skills.RestorePublished(ctx, item.name, item.version.VersionNo, item.version.ContentHash, item.version.StoragePath)
		if err != nil {
			return fmt.Errorf("技能 %s 的发布版本 v%d 快照缺失，且无法按该版本重建: %w",
				item.name, item.version.VersionNo, err)
		}
		if materialized.Dir != item.version.StoragePath {
			return fmt.Errorf("技能 %s 的发布目录 %s 与记录 %s 不一致",
				item.name, materialized.Dir, item.version.StoragePath)
		}
		return nil
	}

	existing, reused, err := findSkillVersionByHash(ctx, deps, item.name, item.work.ContentHash)
	if err != nil {
		return err
	}
	if reused {
		// 版本行在、指针缺失：复用该版本并修复它的快照（若有缺失）。
		if pointerErr := deps.Resources.Publish(ctx, core.ResourceSkill, item.name, existing.ID, true, opID, now); pointerErr != nil {
			return fmt.Errorf("技能 %s 复用同哈希版本 v%d 发布: %w", item.name, existing.VersionNo, pointerErr)
		}
		if _, loadErr := deps.Skills.LoadPublished(ctx, item.name, existing.VersionNo, existing.StoragePath); loadErr != nil {
			if !errors.Is(loadErr, fs.ErrNotExist) {
				return fmt.Errorf("技能 %s 的发布版本 v%d 不可用: %w", item.name, existing.VersionNo, loadErr)
			}
			if _, materializeErr := deps.Skills.RestorePublished(ctx, item.name, existing.VersionNo, existing.ContentHash, existing.StoragePath); materializeErr != nil {
				return fmt.Errorf("技能 %s 的发布版本 v%d 快照缺失，且无法按该版本重建: %w",
					item.name, existing.VersionNo, materializeErr)
			}
		}
		return nil
	}

	materialized, err := deps.Skills.Materialize(ctx, item.name, item.uid, baselineVersionNo, item.work.ContentHash)
	if err != nil {
		return fmt.Errorf("技能 %s 物化首版快照: %w", item.name, err)
	}
	manifest, err := marshalSkillFileList(materialized.Files)
	if err != nil {
		return fmt.Errorf("技能 %s 的文件清单: %w", item.name, err)
	}
	version, inserted, err := deps.Resources.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
		ResourceUID:  item.uid,
		ResourceID:   item.name,
		VersionNo:    baselineVersionNo,
		ContentHash:  materialized.ContentHash,
		ManifestJSON: string(manifest),
		StoragePath:  materialized.Dir,
		CreatedAt:    now.UnixMilli(),
		OperationID:  opID,
	})
	if err != nil {
		return fmt.Errorf("技能 %s 建首版: %w", item.name, err)
	}
	if !inserted || version.VersionNo != baselineVersionNo {
		// 预查之后仍被抢建（启动期单线程时只可能是库处于异常状态）：不覆盖、不升版。
		return fmt.Errorf("技能 %s 的首版已被其它内容占用（v%d），基线不自动升版",
			item.name, version.VersionNo)
	}
	if err := deps.Resources.Publish(ctx, core.ResourceSkill, item.name, version.ID, true, opID, now); err != nil {
		return fmt.Errorf("技能 %s 发布首版: %w", item.name, err)
	}
	return nil
}

// findSkillVersionByHash 在既有版本里找同内容哈希的一行（`Versions` 按版本号倒序）。
// 启动期单线程执行，读到写之间没有并发写者。
func findSkillVersionByHash(ctx context.Context, deps BaselineDeps, name, hash string) (core.ResourceVersion, bool, error) {
	versions, err := deps.Resources.Versions(ctx, core.ResourceSkill, name)
	if err != nil {
		return core.ResourceVersion{}, false, err
	}
	for _, version := range versions {
		if version.ContentHash == hash {
			return version, true, nil
		}
	}
	return core.ResourceVersion{}, false, nil
}

// marshalSkillFileList 用与 `workspace.hashFileList` 相同的规则序列化文件清单
// （紧凑 JSON，nil 清单写成 `[]`），使 `manifest_json` 与 `content_hash` 同源可复算。
func marshalSkillFileList(files []core.FileHash) ([]byte, error) {
	if files == nil {
		files = []core.FileHash{}
	}
	raw, err := json.Marshal(files)
	if err != nil {
		return nil, fmt.Errorf("序列化技能文件清单: %w", err)
	}
	return raw, nil
}

// applyAgentWork 建缺失的状态文件并同步索引；索引写失败只 WARN（状态文件才是权威，
// 索引下次启动重建），文件写失败才是基线失败。
func applyAgentWork(ctx context.Context, deps BaselineDeps, item agentWork, opID string) error {
	record := core.LifecycleRecord{}
	switch {
	case item.bootstrap:
		created, err := deps.Lifecycle.Bootstrap(ctx, item.name, opID)
		if err != nil {
			return fmt.Errorf("为 Agent %s 建立生命周期状态文件: %w", item.name, err)
		}
		record = created
	case item.record != nil:
		record = *item.record
	default:
		return nil
	}

	if err := deps.Resources.PutLifecycleIndex(ctx, record, true, ""); err != nil {
		deps.logger().Warn("启动基线：生命周期索引写入失败（状态文件为准，下次启动重建）",
			"agent", item.name, "error", err)
	}
	return nil
}

// verifyOutcome 是恢复路径的三态结论与它对应的终态——一一映射，不留第三种组合。
// verifyOutcome maps each verification verdict onto its single terminal state.
func verifyOutcome(verification string) core.OperationStatus {
	switch verification {
	case core.VerificationApplied:
		return core.OperationSuccess
	case core.VerificationNotApplied:
		return core.OperationFailed
	default:
		return core.OperationUnknown
	}
}

// operationDetail 是恢复查证需要的 `detail_json` 投影：内容写看两个哈希，启停看
// 期望状态与期望代数。缺字段即无法判定，**不得**用零值猜。
// operationDetail is the projection recovery needs from detail_json; missing fields
// mean "cannot decide" rather than a zero-value guess.
type operationDetail struct {
	CandidateSHA256    string  `json:"candidate_sha256"`
	PreviousSHA256     string  `json:"previous_sha256"`
	ExpectedState      *string `json:"expected_state"`
	ExpectedGeneration *int64  `json:"expected_generation"`
	// 网络域名白名单（R8.2.4 第 6 条）：修订号前后值与候选域名哈希。指针类型让
	// "缺字段"区别于零值——缺字段即无法判定，不用 0 猜。
	BeforeRevision *int64 `json:"before_revision"`
	AfterRevision  *int64 `json:"after_revision"`
	// 受控重建（`mcp_server.apply`，契约 §9.4）：本操作的目标版本与操作前的已应用版本。
	// 查证只比对 `applied` 指针，**不重放** apply——候选连接与注册都是进程内的，
	// 重启后自然消失。
	TargetVersionID   *int64 `json:"target_version_id"`
	PreviousVersionID *int64 `json:"previous_version_id"`
	// 执行档位（`execution_policy.update`）：前后档位。与上面两组修订号共用字段，
	// 档位名用指针区分"缺字段"（→ 无法判定）与空串。
	BeforeProfile *string `json:"before_profile"`
	AfterProfile  *string `json:"after_profile"`
	// 网络访问例外（`network_exception.create` / `.delete`）：被放行的 kind 与 value。
	// 查证先按 `exception_id` + `operation_id` 认领行，再用 `(kind, value)` 排除
	// "已被别人占用 / 删除后重建"的混淆；缺字段同样只能用指针表达"无法判定"。
	ExceptionKind  *string `json:"kind"`
	ExceptionValue *string `json:"value"`
}

// decodeOperationDetail 解析 `detail_json`；空串 / 非法 JSON 都返回错误，由调用方
// 归结为 indeterminate（"读不懂"不是"没生效"）。
func decodeOperationDetail(raw []byte) (operationDetail, error) {
	var detail operationDetail
	if len(raw) == 0 {
		return detail, errors.New("detail_json 为空")
	}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return operationDetail{}, err
	}
	return detail, nil
}

// recoverPendingOperations 是开放服务前的启动恢复（图 A ④ / 约束 38）：逐条按 action
// 组查证、写终态并记录 `recovered_at` + `verification`。任何一条仓储写失败都中止启动
// ——"标了没查"的记录比不标更危险。
// recoverPendingOperations verifies every pending row before serving; a storage
// failure aborts startup instead of leaving a half-recorded verdict.
func recoverPendingOperations(ctx context.Context, deps RecoveryDeps) error {
	if err := deps.validate(); err != nil {
		return err
	}
	logger := deps.logger()
	now := deps.now()

	pending, err := deps.Operations.PendingOperations(ctx)
	if err != nil {
		return fmt.Errorf("启动恢复：读取未完成操作失败: %w", err)
	}
	if len(pending) == 0 {
		logger.Info("启动恢复：没有待查证的操作")
		return nil
	}

	for _, op := range pending {
		verification := verifyOperation(ctx, deps, op)
		if verification == core.VerificationIndeterminate {
			// 无法判定 → unknown，保留原样；**不重做**（约束 38）。
			logger.Warn("启动恢复：操作结果无法判定，保留 unknown（不重做）",
				"operation_id", op.OperationID, "action", op.Action)
		}
		status := verifyOutcome(verification)
		// 回填时**原样带回** detail_json：仓储对 nil 写 `{}`（而不是保留原值），而查证依据
		// 就在这份 detail 里（候选哈希 / 期望代数）——丢掉它等于把证据一起改没了。
		// Pass the original detail back: the store turns nil into {}, and the verdict's
		// evidence lives in that very detail.
		if err := deps.Operations.RecoverOperation(ctx, op.OperationID, status, verification, op.DetailJSON, now); err != nil {
			return fmt.Errorf("启动恢复：回填操作 %s: %w", op.OperationID, err)
		}
		logger.Info("启动恢复：已回填操作", "operation_id", op.OperationID,
			"action", op.Action, "verification", verification, "status", string(status))
	}
	return nil
}

// verifyOperation 按 action 组选择查证方式（约束 44 / 契约 §9.4 的恢复表）：
// `agent.content.update` / `agent.disable|enable` 沿用 US-11 的查证，其余本 US 动作组
// 按目标文件树 / 状态文件 / 定义文件 / 版本行 / 指针 / 删除标记 / 渠道行查证。
// `agent.generate_files` 不写管理操作行（契约 §9.3 注 3），因此不会出现在这里；
// 其它 action（含 `system.baseline_v2`——它已在 ③ 完成或已拒绝启动）一律无法判定。
// verifyOperation dispatches by action group; unknown actions stay indeterminate.
func verifyOperation(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	switch op.Action {
	case actionContentUpdate:
		return verifyContentUpdate(ctx, deps, op)
	case actionDisable, actionEnable:
		return verifyLifecycleOperation(ctx, deps, op)
	case core.ActionAgentCreate, core.ActionAgentCopy:
		return verifyAgentBootstrap(ctx, deps, op)
	case core.ActionAgentFilesUpdate:
		return verifyAgentFilesUpdate(deps, op)
	case core.ActionAgentArchive:
		return verifyAgentArchive(ctx, deps, op)
	case core.ActionAgentRestore:
		return verifyAgentRestore(ctx, deps, op)
	case core.ActionModelConnectionCreate, core.ActionModelConnectionUpdate,
		core.ActionModelConnectionPublish,
		core.ActionMCPServerCreate, core.ActionMCPServerUpdate, core.ActionMCPServerPublish,
		core.ActionSkillCreate, core.ActionSkillImport, core.ActionSkillUpdate,
		core.ActionSkillPublish:
		return verifyResourceWrite(ctx, deps, op)
	case core.ActionModelConnectionDelete, core.ActionMCPServerDelete, core.ActionSkillDelete:
		return verifyResourceDelete(ctx, deps, op)
	case core.ActionModelConnectionTest, core.ActionMCPServerTest, core.ActionNotifyChannelTest:
		return verifyResourceTest(op)
	case core.ActionNotifyChannelCreate, core.ActionNotifyChannelUpdate,
		core.ActionNotifyChannelDelete:
		return verifyNotifyChannel(ctx, deps, op)
	case actionModelConnectionCredentialWrite, actionModelConnectionCredentialRevoke,
		actionNotifyChannelCredentialWrite, actionNotifyChannelCredentialRevoke:
		return verifyCredentialAction(ctx, deps, op)
	case actionModelConnectionScope, actionNotifyChannelScope:
		return verifyScopeConversion(ctx, deps, op)
	case actionBaselineV3:
		return verifyBaselineV3(ctx, deps, op)
	case core.ActionNetworkAllowlistUpdate, core.ActionNetworkDenylistUpdate:
		return verifyNetworkAllowlist(ctx, deps, op)
	case core.ActionNetworkExceptionCreate:
		return verifyNetworkExceptionCreate(ctx, deps, op)
	case core.ActionNetworkExceptionDelete:
		return verifyNetworkExceptionDelete(ctx, deps, op)
	case core.ActionExecutionPolicyUpdate:
		return verifyExecutionPolicy(ctx, deps, op)
	case core.ActionMCPServerApply:
		return verifyMCPApply(ctx, deps, op)
	case core.ActionSystemSecretsInit:
		return verifySecretsInitOperation(ctx, deps, op)
	default:
		return core.VerificationIndeterminate
	}
}

// verifyNetworkAllowlist 按修订号与候选域名哈希判定域名表写入三态（R8.2.4 第 6 条
// 与契约 §9.4）：当前修订 = after 且域名哈希一致 → applied；仍为 before →
// not_applied；其余（含无行 / 读失败）→ indeterminate，**不重做**。白名单与禁止
// 域名表共用本函数，作用域由 `target_kind` 唯一决定——两行共用一张表，按目标类型
// 选错行就会拿另一份域名摘要去比，结论必然错。
// verifyNetworkAllowlist follows the revision-CAS verdicts for both policy rows;
// the scope is derived from target_kind alone.
func verifyNetworkAllowlist(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.NetworkPolicy == nil {
		return core.VerificationIndeterminate
	}
	var scope string
	switch op.TargetKind {
	case core.TargetKindNetworkAllowlist:
		scope = core.PolicyScopeHTTPDomains
	case core.TargetKindNetworkDenylist:
		scope = core.PolicyScopeHTTPDeniedDomains
	default:
		return core.VerificationIndeterminate
	}
	detail, err := decodeOperationDetail(op.DetailJSON)
	if err != nil || detail.BeforeRevision == nil || detail.AfterRevision == nil ||
		detail.CandidateSHA256 == "" {
		return core.VerificationIndeterminate
	}
	policy, exists, loadErr := deps.NetworkPolicy.Load(ctx, scope)
	if loadErr != nil || !exists {
		return core.VerificationIndeterminate
	}
	switch {
	case policy.Revision == *detail.AfterRevision &&
		networkDomainsSHA256(policy.Domains) == detail.CandidateSHA256:
		return core.VerificationApplied
	case policy.Revision == *detail.BeforeRevision:
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// verifyNetworkExceptionCreate 按契约 §9.4 判定新增例外：目标行存在且
// `operation_id` 一致 → applied；目标行不存在且同一 `(kind, value)` 也没有被别的
// 操作占用 → not_applied（可证明未写入）；目标行属于别的操作、或同值已被占用、
// 或读不出来 → indeterminate。**不**改写任何已有行。
// verifyNetworkExceptionCreate claims the row by exception_id plus operation id; an
// absent row is only proof of "not written" when no other operation took the value.
func verifyNetworkExceptionCreate(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.NetworkExceptions == nil || op.TargetKind != core.TargetKindNetworkException {
		return core.VerificationIndeterminate
	}
	detail, err := decodeOperationDetail(op.DetailJSON)
	if err != nil || detail.ExceptionKind == nil || detail.ExceptionValue == nil {
		return core.VerificationIndeterminate
	}
	row, found, lookupErr := deps.NetworkExceptions.Lookup(ctx, op.TargetID)
	if lookupErr != nil {
		return core.VerificationIndeterminate
	}
	if found {
		if row.OperationID == op.OperationID {
			return core.VerificationApplied
		}
		return core.VerificationIndeterminate
	}
	// 目标行不在：再排除"同一 `(kind, value)` 已被别人登记"——那种情况下本次写入
	// 必然失败，但失败原因不是"没执行"，不能直接判 not_applied。
	// An occupied (kind, value) means this write lost the race; that is not the same
	// as proving nothing happened, so the verdict stays unknown.
	return exceptionValueVerdict(ctx, deps, *detail.ExceptionKind, *detail.ExceptionValue,
		core.VerificationNotApplied)
}

// verifyNetworkExceptionDelete 按契约 §9.4 判定删除例外：目标行不存在且同一
// `(kind, value)` 没有重新登记 → applied；目标行仍在且属于本操作 → not_applied；
// 同值已被新行占用（删除后又重建）或读不出来 → indeterminate。**不**再删一次。
// verifyNetworkExceptionDelete treats a vanished row as applied unless the same value
// has reappeared under a different operation.
func verifyNetworkExceptionDelete(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.NetworkExceptions == nil || op.TargetKind != core.TargetKindNetworkException {
		return core.VerificationIndeterminate
	}
	detail, err := decodeOperationDetail(op.DetailJSON)
	if err != nil || detail.ExceptionKind == nil || detail.ExceptionValue == nil {
		return core.VerificationIndeterminate
	}
	row, found, lookupErr := deps.NetworkExceptions.Lookup(ctx, op.TargetID)
	if lookupErr != nil {
		return core.VerificationIndeterminate
	}
	if found {
		if row.OperationID != op.OperationID {
			// 同标识却属于别的操作：行被改写的行为本仓写路径不存在，只能判 unknown。
			return core.VerificationIndeterminate
		}
		return core.VerificationNotApplied
	}
	return exceptionValueVerdict(ctx, deps, *detail.ExceptionKind, *detail.ExceptionValue,
		core.VerificationApplied)
}

// exceptionValueVerdict 在目标行不在时回答"同一 `(kind, value)` 是否被别的行占用"：
// 占用 → indeterminate（无法区分"本次写入没发生"与"已被他人写入 / 删除后重建"）；
// 未占用 → fallback（新增判 not_applied、删除判 applied）；读失败 → indeterminate。
// exceptionValueVerdict resolves the same-value confusion when the row by id is gone.
func exceptionValueVerdict(ctx context.Context, deps RecoveryDeps, kind, value, fallback string) string {
	list, listErr := deps.NetworkExceptions.List(ctx)
	if listErr != nil {
		return core.VerificationIndeterminate
	}
	for _, item := range list {
		if item.Kind == kind && item.Value == value {
			return core.VerificationIndeterminate
		}
	}
	return fallback
}

// verifyExecutionPolicy 按修订号与前后档位判定档位切换三态（契约 §9.4）：当前修订
// = after 且档位 = after → applied；仍为 before 且档位 = before → not_applied；其余
// （含无行 / 读失败 / 缺字段）→ indeterminate，**不重做**。档位切换是幂等的单行写，
// 但重做会把"用户在崩溃前没等到的确认"当成一次新确认——恢复只判不写。
// verifyExecutionPolicy reads the row back and compares both the revision fence and
// the profile; recovery never replays the switch.
func verifyExecutionPolicy(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.ExecutionPolicy == nil || op.TargetKind != core.TargetKindExecutionPolicy {
		return core.VerificationIndeterminate
	}
	detail, err := decodeOperationDetail(op.DetailJSON)
	if err != nil || detail.BeforeRevision == nil || detail.AfterRevision == nil ||
		detail.BeforeProfile == nil || detail.AfterProfile == nil {
		return core.VerificationIndeterminate
	}
	policy, exists, loadErr := deps.ExecutionPolicy.Load(ctx)
	if loadErr != nil || !exists {
		return core.VerificationIndeterminate
	}
	switch {
	case policy.Revision == *detail.AfterRevision && string(policy.Profile) == *detail.AfterProfile:
		return core.VerificationApplied
	case policy.Revision == *detail.BeforeRevision && string(policy.Profile) == *detail.BeforeProfile:
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// verifyMCPApply 按契约 §9.4 判定受控重建：`applied` 指针 = 目标版本 → applied；
// = 操作前版本 → not_applied；第三版本 / 指针或版本行读不出 → indeterminate。
// **不重放** apply——候选注册与候选连接是进程内的，重启后自然消失；重放会把
// "已经失败过一次"的切换当成新请求再试一遍。`superseded` 的 pending 行同样只按
// 指针判：指针仍是操作前版本即为 not_applied（取代者的结果由它自己的操作行记录）。
// verifyMCPApply compares the applied pointer with the target and previous versions;
// the apply itself is never replayed after a restart.
func verifyMCPApply(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Resources == nil || op.TargetKind != core.TargetKindMCPServer {
		return core.VerificationIndeterminate
	}
	detail, err := decodeOperationDetail(op.DetailJSON)
	if err != nil || detail.TargetVersionID == nil || detail.PreviousVersionID == nil {
		return core.VerificationIndeterminate
	}
	// `apply` 的 `target_id` 是资源身份（`resource_uid`），不是名称：按 UID 寻址，
	// 同名重建后旧操作行不会误指新实例。
	uid, uidErr := core.ParseResourceUID(op.TargetID)
	if uidErr != nil {
		return core.VerificationIndeterminate
	}
	pointer, ptrErr := deps.Resources.PointerByUID(ctx, core.ResourceMCPServer, uid)
	if ptrErr != nil || pointer.DeletedAt != nil || pointer.AppliedVersionID == nil {
		return core.VerificationIndeterminate
	}
	applied := *pointer.AppliedVersionID
	var verdict string
	switch applied {
	case *detail.TargetVersionID:
		verdict = core.VerificationApplied
	case *detail.PreviousVersionID:
		verdict = core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
	if _, err := deps.Resources.VersionByID(ctx, applied); err != nil {
		// 指针指向的版本行已不存在：字面对上了也不算查清。
		return core.VerificationIndeterminate
	}
	return verdict
}

// verifyContentUpdate 按契约 §9.4 的三态判定内容写：磁盘哈希 = 目标哈希 → applied；
// 仍为旧哈希或文件不存在 → not_applied；其余（被第三方改写、读不懂 detail）→ indeterminate。
// 读盘失败不改写结论为 not_applied——"读不出来"与"没写进去"是两件事。
// verifyContentUpdate follows the contract's three verdicts for a content write.
func verifyContentUpdate(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Editor == nil || op.TargetKind != targetKindAgent {
		return core.VerificationIndeterminate
	}
	if err := workspace.ValidateAgentName(op.TargetID); err != nil {
		return core.VerificationIndeterminate
	}
	detail, err := decodeOperationDetail(op.DetailJSON)
	if err != nil || !isLowerHexSHA256(detail.CandidateSHA256) {
		return core.VerificationIndeterminate
	}

	content, readErr := deps.Editor.ReadContent(ctx, op.TargetID)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			// 契约 §9.4：目标不存在属于"变更未生效"。
			return core.VerificationNotApplied
		}
		return core.VerificationIndeterminate
	}

	switch {
	case content.ETag == contentETagOf(detail.CandidateSHA256):
		return core.VerificationApplied
	case detail.PreviousSHA256 != "" && content.ETag == contentETagOf(detail.PreviousSHA256):
		return core.VerificationNotApplied
	case op.ExpectedETag != "" && content.ETag == op.ExpectedETag:
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// contentETagOf 用内容摘要拼出强 ETag（`"sha256:<hex>"`，与 workspace.ContentETag 同一
// 字节口径）；恢复路径只拿到摘要，因此在这里按同一格式重建，不重新序列化内容。
func contentETagOf(digest string) string {
	return `"sha256:` + digest + `"`
}

// verifyLifecycleOperation 按图 C 判定启停三态：目标状态 + 代数 + 本次 op → applied；
// 期望状态 + 期望代数 + 非本次 op → not_applied；其余（含状态文件缺失 / 损坏、
// 状态相同但 operation_id 是本次）→ indeterminate。
// verifyLifecycleOperation follows figure C's three verdicts for enable/disable.
func verifyLifecycleOperation(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Lifecycle == nil || op.TargetKind != targetKindAgent {
		return core.VerificationIndeterminate
	}
	if err := workspace.ValidateAgentName(op.TargetID); err != nil {
		return core.VerificationIndeterminate
	}
	detail, err := decodeOperationDetail(op.DetailJSON)
	if err != nil || detail.ExpectedState == nil || detail.ExpectedGeneration == nil {
		return core.VerificationIndeterminate
	}
	expected := core.AgentState(*detail.ExpectedState)
	expectedGeneration := *detail.ExpectedGeneration
	target := core.AgentDisabled
	if op.Action == actionEnable {
		target = core.AgentActive
	}

	record, exists, readErr := deps.Lifecycle.Read(ctx, op.TargetID)
	if readErr != nil || !exists {
		return core.VerificationIndeterminate
	}
	switch {
	case record.State == target && record.Generation == expectedGeneration+1 && record.OperationID == op.OperationID:
		return core.VerificationApplied
	case record.State == expected && record.Generation == expectedGeneration && record.OperationID != op.OperationID:
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// —— US-12 动作组的查证（约束 44 / 契约 §9.4）——
// 三条纪律：① 只读目标面、不重做任何写——唯一例外是约束 44 点名的原名恢复崩溃窗口
// 补偿，那是"把半残状态退回可重试"，不是重做；② 装配缺依赖或读不懂 `detail_json`
// 一律 indeterminate，不把"读不到"读成"没生效"；③ 只有可证实的结果才给
// applied / not_applied，其余一律 indeterminate → unknown 交人判断。
// The US-12 verifiers only read. The sole exception is the original-name restore
// compensation named by constraint 44. Missing dependencies or unreadable details are
// always indeterminate: "cannot read" is never "did not apply".

// verifyAgentBootstrap 判定 `agent.create` / `agent.copy`（契约 §9.4）：Bootstrap 先写
// 状态文件、目录由 staging 单次 rename 落位，两者都在才是 applied；两者都不在是
// not_applied（"目标不存在"）；只剩一半（状态先落 / 目录在而状态不在）→ indeterminate。
// 这里**不**补偿：契约只对原名恢复点名了补偿窗口，其余半残状态交人查证。
// verifyAgentBootstrap accepts both the disabled generation-1 state file and the target
// directory as proof; any half state stays indeterminate and is never compensated.
func verifyAgentBootstrap(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Lifecycle == nil || deps.Root == "" || op.TargetKind != targetKindAgent {
		return core.VerificationIndeterminate
	}
	if err := workspace.ValidateAgentName(op.TargetID); err != nil {
		return core.VerificationIndeterminate
	}

	record, exists, readErr := deps.Lifecycle.Read(ctx, op.TargetID)
	dirPresent, dirErr := agentDirPresent(deps.Root, op.TargetID)
	if dirErr != nil {
		return core.VerificationIndeterminate
	}

	switch {
	case readErr == nil && exists && record.State == core.AgentDisabled &&
		record.Generation == 1 && record.OperationID == op.OperationID && dirPresent:
		return core.VerificationApplied
	case errors.Is(readErr, fs.ErrNotExist) && !dirPresent:
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// verifyAgentFilesUpdate 按契约 §9.4 判定多文件提交：当前受管集合 ETag = 候选 ETag →
// applied；仍等于基础 ETag → not_applied；目标不存在 → not_applied（契约把"不存在"
// 归入未生效）；其余（被第三方改写、读不懂 detail）→ indeterminate。
// verifyAgentFilesUpdate compares the current managed-file ETag with the candidate and
// base ETags recorded before the pre-write.
func verifyAgentFilesUpdate(deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Root == "" || op.TargetKind != targetKindAgent {
		return core.VerificationIndeterminate
	}
	if err := workspace.ValidateAgentName(op.TargetID); err != nil {
		return core.VerificationIndeterminate
	}
	var detail agentFilesOperationDetail
	if err := json.Unmarshal(op.DetailJSON, &detail); err != nil || detail.CandidateETag == "" {
		return core.VerificationIndeterminate
	}

	_, currentETag, err := workspace.ManagedAgentFiles(deps.Root, op.TargetID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 契约 §9.4："目标仍为旧哈希或不存在" 属 not_applied。
			return core.VerificationNotApplied
		}
		return core.VerificationIndeterminate
	}
	switch {
	case currentETag == detail.CandidateETag:
		return core.VerificationApplied
	case detail.BaseETag != "" && currentETag == detail.BaseETag:
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// verifyAgentArchive 按契约 §9.4 判定归档：状态文件 `archived`（本次 op 写的）且归档行
// 存在、归档目录清单哈希与行一致 → applied；状态仍是归档前值且目录仍在活动区 →
// not_applied；目录已移出但归档行缺失 / 清单不完整 → indeterminate（约束 32 ⑦ 的崩溃窗口）。
// verifyAgentArchive requires the archived state, the archive row and a matching manifest
// hash; a directory moved without its row stays indeterminate.
func verifyAgentArchive(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if op.TargetKind != targetKindAgent || deps.Lifecycle == nil || deps.Archives == nil ||
		deps.Root == "" {
		return core.VerificationIndeterminate
	}
	if err := workspace.ValidateAgentName(op.TargetID); err != nil {
		return core.VerificationIndeterminate
	}
	var detail agentArchiveOperationDetail
	if err := json.Unmarshal(op.DetailJSON, &detail); err != nil {
		return core.VerificationIndeterminate
	}
	before := core.AgentState(detail.BeforeState)
	if before != core.AgentActive && before != core.AgentDisabled {
		return core.VerificationIndeterminate
	}

	record, exists, readErr := deps.Lifecycle.Read(ctx, op.TargetID)
	if readErr != nil || !exists {
		// 归档后状态文件必须保留（约束 32）：缺失或损坏都无法判定。
		return core.VerificationIndeterminate
	}
	dirPresent, dirErr := agentDirPresent(deps.Root, op.TargetID)
	if dirErr != nil {
		return core.VerificationIndeterminate
	}

	switch {
	case record.State == core.AgentArchived:
		if record.OperationID != op.OperationID {
			return core.VerificationIndeterminate
		}
		found, row, rowErr := archiveRowForOperation(ctx, deps.Archives, op)
		if rowErr != nil || !found {
			return core.VerificationIndeterminate
		}
		contentHash, hashErr := workspace.ArchiveDirContentHash(deps.Root,
			path.Join(agentArchiveDirName, row.ArchiveID))
		if hashErr != nil || contentHash != row.ContentHash {
			return core.VerificationIndeterminate
		}
		return core.VerificationApplied
	case record.State == before && dirPresent:
		// 含"回滚已完成（generation 再 +1、op 仍是本次）"的形态：效果已退回，未生效。
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// verifyAgentRestore 按契约 §9.4 与约束 44 判定恢复。原名路径状态文件必须保留为
// `archived`；异名路径在 Bootstrap 之前不存在状态文件。唯一补偿窗口：原名恢复在
// `Transition(archived → disabled)` 之后、目录 rename 之前崩溃（状态 disabled、目录
// 不存在）——在 Agent 写锁内以当前 generation 条件补偿回 `archived` 后置 failed；
// 其它不确定形态保持 unknown，不自动补做。
// verifyAgentRestore compensates exactly the original-name crash window constraint 44
// names; every other uncertain shape stays unknown.
func verifyAgentRestore(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if op.TargetKind != targetKindAgent || deps.Lifecycle == nil || deps.Archives == nil ||
		deps.Root == "" {
		return core.VerificationIndeterminate
	}
	var detail agentRestoreOperationDetail
	if err := json.Unmarshal(op.DetailJSON, &detail); err != nil || detail.ArchiveID == "" {
		return core.VerificationIndeterminate
	}
	if detail.Target == "" || detail.Target != op.TargetID {
		return core.VerificationIndeterminate
	}
	if err := workspace.ValidateAgentName(detail.Target); err != nil {
		return core.VerificationIndeterminate
	}
	row, rowErr := deps.Archives.ArchiveByID(ctx, detail.ArchiveID)
	if rowErr != nil {
		return core.VerificationIndeterminate
	}
	if detail.Renamed != (detail.Target != row.AgentName) {
		// detail 与归档行的身份不自洽：不拿它做判据。
		return core.VerificationIndeterminate
	}

	dirPresent, dirErr := agentDirPresent(deps.Root, detail.Target)
	if dirErr != nil {
		return core.VerificationIndeterminate
	}
	record, exists, readErr := deps.Lifecycle.Read(ctx, detail.Target)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return core.VerificationIndeterminate
	}
	if readErr != nil || !exists {
		// 异名路径在 Bootstrap 之前崩溃 → 未生效；原名路径的状态文件归档后必须保留。
		if !dirPresent && detail.Renamed && row.RestoredAt == nil {
			return core.VerificationNotApplied
		}
		return core.VerificationIndeterminate
	}

	switch {
	case record.State == core.AgentDisabled && record.OperationID == op.OperationID:
		if row.RestoredAt != nil && dirPresent {
			// 防御分支：`MarkRestored` 与终态同事务，pending 行本不该命中。
			return core.VerificationApplied
		}
		if dirPresent || detail.Renamed {
			// 目录已落而归档行未标记：效果在、记账缺；异名 Bootstrap 之后崩溃：卡片没有
			// 点名补偿窗口。两种都交人查证，不自动补做。
			return core.VerificationIndeterminate
		}
		return compensateOriginalRestore(ctx, deps, op, detail.Target)
	case !detail.Renamed && record.State == core.AgentArchived &&
		row.RestoredAt == nil && !dirPresent:
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// compensateOriginalRestore 在 Agent 写锁内把"状态 disabled、目录不存在"的原名恢复
// 崩溃窗口补偿回 `archived`（generation 再 +1，不回退），归档行保持未恢复、可重试。
// 这是约束 44 点名的补偿**不是**重做：不重新复制归档内容，失败只保留 unknown。
// compensateOriginalRestore rolls the half-applied original-name restore back to
// archived under the agent write lock, exactly as constraint 44 mandates.
func compensateOriginalRestore(ctx context.Context, deps RecoveryDeps,
	op core.OperationRecord, target string,
) string {
	if deps.Gate == nil {
		return core.VerificationIndeterminate
	}
	unlock := deps.Gate.Lock(target)
	defer unlock()

	// 锁内重读：补偿只建立在"当前事实仍是本操作留下的 disabled"之上。
	current, exists, readErr := deps.Lifecycle.Read(ctx, target)
	if readErr != nil || !exists || current.State != core.AgentDisabled ||
		current.OperationID != op.OperationID {
		return core.VerificationIndeterminate
	}
	compensated, transErr := deps.Lifecycle.Transition(ctx, target, core.AgentDisabled,
		current.Generation, core.AgentArchived, op.OperationID)
	if transErr != nil {
		deps.logger().Error("启动恢复：原名恢复崩溃窗口补偿失败，保留 unknown",
			"agent", target, "operation_id", op.OperationID, "error", transErr)
		return core.VerificationIndeterminate
	}
	if deps.Resources != nil {
		if idxErr := deps.Resources.PutLifecycleIndex(context.WithoutCancel(ctx),
			compensated, true, ""); idxErr != nil {
			// 索引是派生视图：失败只记 ERROR，状态文件才是权威（约束 32 / 36）。
			deps.logger().Error("启动恢复：补偿后生命周期索引同步失败（状态文件为准，下次启动基线修复）",
				"agent", target, "operation_id", op.OperationID, "error", idxErr)
		}
	}
	deps.logger().Warn("启动恢复：原名恢复在目录落位前崩溃，状态已补偿回 archived 并置 failed",
		"agent", target, "operation_id", op.OperationID)
	return core.VerificationNotApplied
}

// verifyResourceWrite 判定模型连接 / MCP / 技能的写与发布（契约 §9.4）：指针指向本次
// 操作写的版本 → applied（防御分支：版本 + 指针 + 终态同事务，pending 行本不该命中）；
// 指针仍指向旧版本且文件仍与旧版本逐字节一致 → not_applied；文件与指针不一致（含文件
// 缺失）→ indeterminate——这正是约束 13 的"文件已改而指针未改"崩溃窗口。
// verifyResourceWrite proves "no byte landed" only when the pointer and the file still
// agree on the old version; any file/pointer divergence stays indeterminate.
func verifyResourceWrite(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Resources == nil {
		return core.VerificationIndeterminate
	}
	kind, ok := resourceKindOfTarget(op.TargetKind)
	if !ok {
		return core.VerificationIndeterminate
	}

	if version, found, err := operationVersion(ctx, deps, kind, op); err != nil {
		return core.VerificationIndeterminate
	} else if found {
		pointer, ptrErr := deps.Resources.Pointer(ctx, kind, op.TargetID)
		if ptrErr == nil && pointer.DeletedAt == nil && pointer.PublishedVersionID == version.ID {
			return core.VerificationApplied
		}
		return core.VerificationIndeterminate
	}

	pointer, ptrErr := deps.Resources.Pointer(ctx, kind, op.TargetID)
	switch {
	case errors.Is(ptrErr, core.ErrResourceNotFound):
		// 没有指针：只有"创建 / 导入"且文件侧也证明资源不存在才是 not_applied。
		return verifyResourceAbsent(deps, kind, op)
	case ptrErr != nil:
		return core.VerificationIndeterminate
	case pointer.DeletedAt != nil:
		// 资源已删除：删除可能发生在本操作之后，无法区分 → 不确定。
		return core.VerificationIndeterminate
	}

	published, err := deps.Resources.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.VerificationIndeterminate
	}
	matches, matchErr := versionMatchesFile(deps, kind, op.TargetID, published)
	if matchErr != nil {
		return core.VerificationIndeterminate
	}
	if matches {
		return core.VerificationNotApplied
	}
	return core.VerificationIndeterminate
}

// verifyResourceAbsent 是"没有发布指针"时的进一步查证：创建 / 导入动作且定义文件里
// 确实没有该条目（技能为工作副本不存在）才可判 not_applied；文件与库不一致时仍然交人。
// verifyResourceAbsent only clears create/import actions when the file side agrees that
// the resource does not exist.
func verifyResourceAbsent(deps RecoveryDeps, kind core.ResourceKind, op core.OperationRecord) string {
	switch op.Action {
	case core.ActionModelConnectionCreate, core.ActionMCPServerCreate,
		core.ActionSkillCreate, core.ActionSkillImport:
	default:
		return core.VerificationIndeterminate
	}

	if kind == core.ResourceSkill {
		if deps.Skills == nil {
			return core.VerificationIndeterminate
		}
		_, err := deps.Skills.HashWorkCopy(op.TargetID)
		switch {
		case err == nil:
			// 工作副本在、指针不在：磁盘与库不一致，启动基线本应先建 v1。
			return core.VerificationIndeterminate
		case errors.Is(err, fs.ErrNotExist):
			return core.VerificationNotApplied
		default:
			return core.VerificationIndeterminate
		}
	}

	doc, err := definitionDocFor(deps, kind)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return core.VerificationNotApplied
		}
		return core.VerificationIndeterminate
	}
	present, presentErr := definitionBlockPresent(doc, definitionKeyOf(kind), op.TargetID)
	if presentErr != nil || present {
		return core.VerificationIndeterminate
	}
	return core.VerificationNotApplied
}

// verifyResourceDelete 判定删除（契约 §9.4 / 约束 13）：删除标记由本次操作写入 →
// applied（防御分支）；指针未删除且文件仍与已发布版本一致 → not_applied；文件已被移除
// 或与指针不一致 → indeterminate（"文件已改而指针未改"，不自动补做）。
// verifyResourceDelete never redoes the delete: only a still-intact file proves that no
// byte was touched.
func verifyResourceDelete(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Resources == nil {
		return core.VerificationIndeterminate
	}
	kind, ok := resourceKindOfTarget(op.TargetKind)
	if !ok {
		return core.VerificationIndeterminate
	}

	pointer, ptrErr := deps.Resources.Pointer(ctx, kind, op.TargetID)
	switch {
	case errors.Is(ptrErr, core.ErrResourceNotFound):
		return core.VerificationApplied
	case ptrErr != nil:
		return core.VerificationIndeterminate
	}
	if pointer.DeletedAt != nil {
		if pointer.OperationID == op.OperationID {
			return core.VerificationApplied
		}
		return core.VerificationIndeterminate
	}

	published, err := deps.Resources.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.VerificationIndeterminate
	}
	matches, matchErr := versionMatchesFile(deps, kind, op.TargetID, published)
	if matchErr != nil {
		return core.VerificationIndeterminate
	}
	if matches {
		return core.VerificationNotApplied
	}
	return core.VerificationIndeterminate
}

// verifyResourceTest 查证模型 / MCP 测试操作（契约 §9.4 / 注 15）：pending 行上的
// `detail_json` 是预写形态 `{}`，既没有完整结果、也证明不了"调用没发出"——两栏都无法
// 成立，只能 indeterminate → unknown；恢复**不得重发**（重发会产生新的 operation_id，
// 且在可能计费的调用上再打一次）。
// A pending test row carries only the pre-write {} and can prove neither outcome, so the
// verdict is always unknown and the call is never resent.
func verifyResourceTest(op core.OperationRecord) string {
	var detail modelTestOperationDetail
	if err := json.Unmarshal(op.DetailJSON, &detail); err != nil {
		return core.VerificationIndeterminate
	}
	if detail.OK || detail.ErrorCode != "" {
		// 防御分支：终态 detail 由 finish 写入，pending 行本不该命中。
		return core.VerificationApplied
	}
	return core.VerificationIndeterminate
}

// verifyNotifyChannel 按契约 §9.4 判定通知渠道写：行字段的脱敏哈希与候选一致 →
// applied；仍是旧值 / 不存在 → not_applied（删除动作的"不存在"即 applied）；行存在但
// 字段被第三方改写 → indeterminate。哈希对象是 `channelRequestHash` 的同一材料，
// 不存渠道原文。
// verifyNotifyChannel compares redacted field hashes; the raw channel never enters the
// operation detail.
func verifyNotifyChannel(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Notify == nil || op.TargetKind != core.TargetKindNotifyChannel ||
		op.TargetID == "" {
		return core.VerificationIndeterminate
	}
	var detail notifyChannelOperationDetail
	if err := json.Unmarshal(op.DetailJSON, &detail); err != nil {
		return core.VerificationIndeterminate
	}
	if detail.BeforeSHA256 == "" && detail.AfterSHA256 == "" {
		return core.VerificationIndeterminate
	}

	current, readErr := deps.Notify.Get(ctx, op.TargetID)
	if errors.Is(readErr, core.ErrResourceNotFound) {
		if op.Action == core.ActionNotifyChannelDelete && detail.AfterSHA256 == "" {
			return core.VerificationApplied
		}
		return core.VerificationNotApplied
	}
	if readErr != nil {
		return core.VerificationIndeterminate
	}
	currentHash := channelRequestHash(current)

	switch op.Action {
	case core.ActionNotifyChannelCreate:
		if detail.AfterSHA256 != "" && currentHash == detail.AfterSHA256 {
			return core.VerificationApplied
		}
		return core.VerificationIndeterminate
	case core.ActionNotifyChannelUpdate:
		switch {
		case detail.AfterSHA256 != "" && currentHash == detail.AfterSHA256:
			return core.VerificationApplied
		case detail.BeforeSHA256 != "" && currentHash == detail.BeforeSHA256:
			return core.VerificationNotApplied
		default:
			return core.VerificationIndeterminate
		}
	case core.ActionNotifyChannelDelete:
		if detail.BeforeSHA256 != "" && currentHash == detail.BeforeSHA256 {
			return core.VerificationNotApplied
		}
		return core.VerificationIndeterminate
	default:
		return core.VerificationIndeterminate
	}
}

// credentialActionDetail 是凭据 / 范围动作 detail_json 的恢复投影（契约 §9.4 的四证）：
// ① 操作 ID 与 detail（资源 kind / name / UID、写前写后 revision、凭据 ID 与版本号、
// 目标绑定哈希、撤销纪元、范围前后值）；② 数据库提交事实由仓储行回答（`operation_id`
// 是"这一事务确实提交过"的落点）；③ 凭据身份与版本状态；④ 目标绑定哈希。
// 缺任何一个必需字段就是"证据不足"，**不得**用零值把它补成成功或失败。
// credentialActionDetail is the four-evidence projection of a credential or scope action;
// a missing field means the evidence is insufficient, never a zero-value guess.
type credentialActionDetail struct {
	Kind                  string `json:"kind"`
	Name                  string `json:"name"`
	ResourceUID           string `json:"resource_uid,omitempty"`
	Revision              int    `json:"revision"`
	PreviousRevision      int    `json:"previous_revision"`
	Scope                 string `json:"scope,omitempty"`
	OwnerAgentUID         string `json:"owner_agent_uid,omitempty"`
	PreviousScope         string `json:"previous_scope,omitempty"`
	PreviousOwnerAgentUID string `json:"previous_owner_agent_uid,omitempty"`
	Mode                  string `json:"mode,omitempty"`
	CredentialID          string `json:"credential_id,omitempty"`
	VersionNo             int    `json:"version_no,omitempty"`
	PreviousVersion       int    `json:"previous_version,omitempty"`
	TargetBinding         string `json:"target_binding,omitempty"`
	// BindingKey 是 MCP 槽位名（env 键名或请求头名）：一个 server 可以有多个槽位，各自
	// 独立版本与撤销，审计必须点名是哪一槽。其它类别省略该字段。
	// BindingKey names the MCP slot; other credential kinds omit it.
	BindingKey      string `json:"binding_key,omitempty"`
	RevocationEpoch int    `json:"revocation_epoch,omitempty"`
	PreviousEpoch   int    `json:"previous_epoch,omitempty"`
	// AffectedEligibleAgents 是切到专属后不再收到该渠道的 Agent 名单（R4.5 的影响提示）：
	// 与响应同源，落进操作记录供事后核对"当时告诉过谁"。空名单省略。
	// AffectedEligibleAgents mirrors the response hint into the audit record.
	AffectedEligibleAgents []string `json:"affected_eligible_agents,omitempty"`
}

// decodeCredentialActionDetail 解析并做形状校验：资源种类必须是两类带凭据的资源之一，
// `resource_uid`（若记录）必须是合法 UID 且与操作行目标一致。读不懂 → 交由调用方判
// indeterminate。
// decodeCredentialActionDetail parses and shape-checks the detail projection.
func decodeCredentialActionDetail(op core.OperationRecord, uid core.ResourceUID) (credentialActionDetail, error) {
	var detail credentialActionDetail
	if len(op.DetailJSON) == 0 {
		return detail, errors.New("detail_json 为空")
	}
	if err := json.Unmarshal(op.DetailJSON, &detail); err != nil {
		return credentialActionDetail{}, err
	}
	if _, err := core.ParseCredentialOwnerKind(detail.Kind); err != nil {
		return credentialActionDetail{}, err
	}
	if detail.ResourceUID != "" {
		recorded, err := core.ParseResourceUID(detail.ResourceUID)
		if err != nil {
			return credentialActionDetail{}, err
		}
		if recorded != uid {
			return credentialActionDetail{}, fmt.Errorf("detail 的资源 UID %s 与操作目标 %s 不一致", recorded, uid)
		}
	}
	return detail, nil
}

// credentialTargetSnapshot 是目标资源行上由管理写维护的提交事实：`revision` 与
// `operation_id` 回答"哪个事务写到了这一行"，`scope` / 属主回答范围动作的结果。
// credentialTargetSnapshot carries the commit facts a credential or scope action leaves
// on the resource row.
type credentialTargetSnapshot struct {
	Revision      int
	OperationID   string
	Scope         core.ResourceScope
	OwnerAgentUID core.AgentUID
}

// readCredentialTarget 按资源种类读目标的提交事实：模型连接读发布指针，通知渠道读渠道行
// （含墓碑——删除之后仍要能查证当时那一次写）。读不到即证据缺失（false）。
// readCredentialTarget reads the commit facts; a read failure is missing evidence.
func readCredentialTarget(ctx context.Context, deps RecoveryDeps, kind core.BindingKind,
	uid core.ResourceUID,
) (credentialTargetSnapshot, bool) {
	switch kind {
	case core.BindingModelConnection:
		if deps.Resources == nil {
			return credentialTargetSnapshot{}, false
		}
		ptr, err := deps.Resources.PointerByUID(ctx, core.ResourceModelConnection, uid)
		if err != nil {
			return credentialTargetSnapshot{}, false
		}
		return credentialTargetSnapshot{
			Revision: ptr.Revision, OperationID: ptr.OperationID,
			Scope: ptr.Scope, OwnerAgentUID: ptr.OwnerAgentUID,
		}, true
	case core.BindingNotifyChannel:
		if deps.Notify == nil {
			return credentialTargetSnapshot{}, false
		}
		channel, err := deps.Notify.GetByUID(ctx, uid)
		if err != nil {
			return credentialTargetSnapshot{}, false
		}
		return credentialTargetSnapshot{
			Revision: channel.Revision, OperationID: channel.OperationID,
			Scope: channel.Scope, OwnerAgentUID: channel.OwnerAgentUID,
		}, true
	default:
		return credentialTargetSnapshot{}, false
	}
}

// verifyCredentialAction 按契约 §9.4 的四证判定凭据写入 / 撤销：
// 四证互相印证且全部指向候选终态 → applied；能证明未提交（旧状态完好且无候选版本行）
// → not_applied；其余（证据缺失、彼此矛盾、只对上一部分）→ indeterminate。
// 恢复**不得**重新加密、重放撤销或自动补写凭据版本。
// verifyCredentialAction follows the contract's four-evidence table and never replays.
func verifyCredentialAction(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Credentials == nil {
		return core.VerificationIndeterminate
	}
	uid, err := core.ParseResourceUID(op.TargetID)
	if err != nil {
		return core.VerificationIndeterminate
	}
	detail, err := decodeCredentialActionDetail(op, uid)
	if err != nil {
		return core.VerificationIndeterminate
	}
	kind := core.BindingKind(detail.Kind)
	target, ok := readCredentialTarget(ctx, deps, kind, uid)
	if !ok {
		return core.VerificationIndeterminate
	}

	identity, identityErr := deps.Credentials.CredentialByOwner(ctx, kind, uid)
	identityFound := true
	switch {
	case errors.Is(identityErr, core.ErrResourceNotFound):
		identityFound = false
	case identityErr != nil:
		return core.VerificationIndeterminate
	}

	if op.Action == actionModelConnectionCredentialRevoke || op.Action == actionNotifyChannelCredentialRevoke {
		return verifyCredentialRevoke(ctx, deps, op, identity, identityFound, target, detail)
	}
	if detail.Mode == string(core.CredentialModeEnv) {
		// managed → env 的退役：身份降为 retired、版本号不变、密文保留（在途可解密）。
		return verifyCredentialRetire(ctx, deps, op, identity, identityFound, target, detail)
	}
	if !identityFound {
		// 写入必然产生身份与版本：查不到身份时，只有"写前无身份 + 资源未被改动"能证明未提交。
		if detail.PreviousVersion == 0 && target.Revision == detail.PreviousRevision {
			return core.VerificationNotApplied
		}
		return core.VerificationIndeterminate
	}
	return verifyCredentialWrite(ctx, deps, op, identity, target, detail)
}

// verifyCredentialWrite 判写入（创建新身份或追加新版本）：写后版本行必须带本操作的
// `operation_id` 与 detail 记录的目标绑定哈希，身份停在 detail 的版本号上。
// verifyCredentialWrite judges a credential write from the committed version row.
func verifyCredentialWrite(ctx context.Context, deps RecoveryDeps, op core.OperationRecord,
	identity core.CredentialRecord, target credentialTargetSnapshot, detail credentialActionDetail,
) string {
	if detail.CredentialID == "" || identity.ID != detail.CredentialID ||
		detail.VersionNo < 1 || detail.TargetBinding == "" {
		return core.VerificationIndeterminate
	}
	versions, err := deps.Credentials.Versions(ctx, identity.ID)
	if err != nil {
		return core.VerificationIndeterminate
	}
	candidate, found := credentialVersionAt(versions, detail.VersionNo)

	applied := found &&
		identity.State == core.CredentialIdentityActive &&
		identity.CurrentVersion == detail.VersionNo &&
		identity.OperationID == op.OperationID &&
		candidate.State == core.CredentialVersionActive &&
		candidate.OperationID == op.OperationID &&
		candidate.TargetBinding == detail.TargetBinding &&
		target.Revision == detail.Revision
	if applied {
		return core.VerificationApplied
	}

	notApplied := !found &&
		target.Revision == detail.PreviousRevision &&
		identity.CurrentVersion == detail.PreviousVersion &&
		identity.OperationID != op.OperationID
	if notApplied {
		return core.VerificationNotApplied
	}
	return core.VerificationIndeterminate
}

// verifyCredentialRevoke 判撤销：身份 `revoked` + 纪元等于候选值 + 全部版本 `revoked`
// 且密文已清（证据 ③），行上的 `operation_id` 是本次操作（证据 ②），当前版本的
// 目标绑定哈希与 detail 一致（证据 ④）。旧状态完好且未被本操作触碰 → not_applied。
// verifyCredentialRevoke judges a revoke; revoked is terminal and never replayed.
func verifyCredentialRevoke(ctx context.Context, deps RecoveryDeps, op core.OperationRecord,
	identity core.CredentialRecord, identityFound bool, target credentialTargetSnapshot,
	detail credentialActionDetail,
) string {
	if !identityFound || detail.TargetBinding == "" ||
		detail.RevocationEpoch <= detail.PreviousEpoch {
		return core.VerificationIndeterminate
	}
	versions, err := deps.Credentials.Versions(ctx, identity.ID)
	if err != nil {
		return core.VerificationIndeterminate
	}
	current, ok := credentialVersionAt(versions, identity.CurrentVersion)
	if !ok || current.TargetBinding != detail.TargetBinding {
		return core.VerificationIndeterminate
	}

	if identity.State == core.CredentialIdentityRevoked {
		if identity.RevocationEpoch == detail.RevocationEpoch &&
			identity.OperationID == op.OperationID &&
			target.Revision == detail.Revision &&
			allCredentialVersionsRevoked(versions) {
			return core.VerificationApplied
		}
		return core.VerificationIndeterminate
	}

	if identity.RevocationEpoch == detail.PreviousEpoch &&
		identity.OperationID != op.OperationID &&
		target.Revision == detail.PreviousRevision &&
		!credentialVersionsTouchedBy(versions, op.OperationID) {
		return core.VerificationNotApplied
	}
	return core.VerificationIndeterminate
}

// verifyCredentialRetire 判 managed → env 的退役：身份 `retired`、当前版本号**未变**
// （退役不追加版本）、行上的 `operation_id` 是本次操作、该版本已 `retired`、资源
// revision 到位 → applied；身份仍 active、版本仍 active、证据全停在写前值且本操作
// 未触碰任何版本 → not_applied；其余 indeterminate。退役**不**清密文（在途可解密），
// 因此密文不是这条动作的判据。
// verifyCredentialRetire judges a managed→env retire from the state transition alone;
// the ciphertext is deliberately kept, so it is not evidence here.
func verifyCredentialRetire(ctx context.Context, deps RecoveryDeps, op core.OperationRecord,
	identity core.CredentialRecord, identityFound bool, target credentialTargetSnapshot,
	detail credentialActionDetail,
) string {
	if !identityFound || detail.CredentialID == "" || detail.VersionNo < 1 ||
		identity.ID != detail.CredentialID {
		return core.VerificationIndeterminate
	}
	versions, err := deps.Credentials.Versions(ctx, identity.ID)
	if err != nil {
		return core.VerificationIndeterminate
	}
	candidate, found := credentialVersionAt(versions, detail.VersionNo)
	if !found {
		return core.VerificationIndeterminate
	}

	applied := identity.State == core.CredentialIdentityRetired &&
		identity.CurrentVersion == detail.VersionNo &&
		identity.OperationID == op.OperationID &&
		candidate.State == core.CredentialVersionRetired &&
		target.Revision == detail.Revision
	if applied {
		return core.VerificationApplied
	}
	if identity.State == core.CredentialIdentityActive &&
		identity.CurrentVersion == detail.PreviousVersion &&
		identity.OperationID != op.OperationID &&
		candidate.State == core.CredentialVersionActive &&
		target.Revision == detail.PreviousRevision &&
		!credentialVersionsTouchedBy(versions, op.OperationID) {
		return core.VerificationNotApplied
	}
	return core.VerificationIndeterminate
}

// verifyScopeConversion 判范围转换：范围 / 属主 / revision 与候选一致且行上的
// `operation_id` 是本次操作 → applied；仍停在写前三者 → not_applied；其余 indeterminate。
// 范围动作只改资源行，不产生版本，因此证据全在提交事实上。
// verifyScopeConversion judges a scope conversion from the committed resource row.
func verifyScopeConversion(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	uid, err := core.ParseResourceUID(op.TargetID)
	if err != nil {
		return core.VerificationIndeterminate
	}
	detail, err := decodeCredentialActionDetail(op, uid)
	if err != nil {
		return core.VerificationIndeterminate
	}
	scope, err := core.ParseScope(detail.Scope)
	if err != nil {
		return core.VerificationIndeterminate
	}
	previous, err := core.ParseScope(detail.PreviousScope)
	if err != nil {
		return core.VerificationIndeterminate
	}
	target, ok := readCredentialTarget(ctx, deps, core.BindingKind(detail.Kind), uid)
	if !ok {
		return core.VerificationIndeterminate
	}

	if target.OperationID == op.OperationID &&
		target.Revision == detail.Revision &&
		target.Scope == scope &&
		target.OwnerAgentUID == core.AgentUID(detail.OwnerAgentUID) {
		return core.VerificationApplied
	}
	if target.OperationID != op.OperationID &&
		target.Revision == detail.PreviousRevision &&
		target.Scope == previous &&
		target.OwnerAgentUID == core.AgentUID(detail.PreviousOwnerAgentUID) {
		return core.VerificationNotApplied
	}
	return core.VerificationIndeterminate
}

// verifyBaselineV3 按契约 §9.4 判 v3 迁移：库已在目标版本，且活动区每个 Agent 的状态
// 文件都是 schema 2、UID 合法、索引镜像一致 → applied；库仍旧版且所有状态文件仍是
// schema 1 / 无 UID → not_applied；部分升级、文件缺失或镜像不一致 → indeterminate。
// **不得**给同一 Agent 补发第二个 UID。
// verifyBaselineV3 judges the migration; a partial upgrade stays unknown.
func verifyBaselineV3(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if op.TargetKind != targetKindSystem || deps.Lifecycle == nil ||
		deps.Resources == nil || deps.Root == "" || deps.Schema == nil {
		return core.VerificationIndeterminate
	}
	targetVersion, err := deps.Schema.SchemaVersion(ctx)
	if err != nil || targetVersion > storage.SupportedSchemaVersion {
		return core.VerificationIndeterminate
	}
	names, err := workspace.AgentNames(deps.Root)
	if err != nil {
		return core.VerificationIndeterminate
	}

	upgraded, legacy := 0, 0
	for _, name := range names {
		rec, exists, readErr := deps.Lifecycle.Read(ctx, name)
		if readErr != nil || !exists {
			// 状态文件缺失或损坏：没有可证明的版本事实，整条操作停在 unknown。
			return core.VerificationIndeterminate
		}
		switch rec.SchemaVersion {
		case lifecycleFileSchemaV2:
			if !rec.AgentUID.Valid() {
				return core.VerificationIndeterminate
			}
			uid, _, found, idxErr := deps.Resources.AgentIdentity(ctx, name)
			if idxErr != nil || !found || uid != rec.AgentUID {
				return core.VerificationIndeterminate
			}
			upgraded++
		case lifecycleFileSchemaV1:
			legacy++
		default:
			return core.VerificationIndeterminate
		}
	}

	switch {
	case targetVersion == storage.SupportedSchemaVersion && legacy == 0:
		return core.VerificationApplied
	case targetVersion < storage.SupportedSchemaVersion && upgraded == 0:
		return core.VerificationNotApplied
	default:
		return core.VerificationIndeterminate
	}
}

// credentialVersionAt 按版本号取凭据版本行。
// credentialVersionAt picks one credential version by number.
func credentialVersionAt(versions []core.CredentialVersionRecord, versionNo int) (core.CredentialVersionRecord, bool) {
	for _, version := range versions {
		if version.VersionNo == versionNo {
			return version, true
		}
	}
	return core.CredentialVersionRecord{}, false
}

// allCredentialVersionsRevoked 判定该凭据的每个版本都已撤销且密文已清（证据 ③）。
// allCredentialVersionsRevoked reports whether every version is revoked and blanked.
func allCredentialVersionsRevoked(versions []core.CredentialVersionRecord) bool {
	narrowed := false
	for _, version := range versions {
		if version.State != core.CredentialVersionRevoked || len(version.Ciphertext) != 0 {
			return false
		}
		narrowed = true
	}
	return narrowed
}

// credentialVersionsTouchedBy 判定是否有版本行由本操作写过（撤销的提交事实）。
// credentialVersionsTouchedBy reports whether this operation wrote any version row.
func credentialVersionsTouchedBy(versions []core.CredentialVersionRecord, operationID string) bool {
	for _, version := range versions {
		if version.OperationID == operationID {
			return true
		}
	}
	return false
}

// rebuildStartupBindings 在启动对账之后全量重建绑定投影（约束 38 / 图 A ⑤）：投影是
// `AGENT.md` 的可重建派生视图，文件与已发布版本对齐后必须从文件重算。扫描失败属于
// 工作区级问题 → 启动失败；单个 Agent 的失败只记 WARN，准入闸门随后按"可能引用"处理
// （fail-closed），**不得**把失败的 Agent 当作无引用。
// rebuildStartupBindings rebuilds the whole projection after reconciliation; a scan
// failure aborts startup, per-agent failures stay fail-closed at the gate.
func rebuildStartupBindings(ctx context.Context, projection *BindingProjection,
	logger *slog.Logger,
) (ProjectionReport, error) {
	report, err := projection.RebuildAll(ctx)
	if err != nil {
		return report, fmt.Errorf("启动绑定投影重建: %w", err)
	}
	for _, failure := range report.Failures {
		logger.Warn("绑定投影：该 Agent 的引用无法从定义文件重建，删除闸门按可能引用处理",
			"agent", failure.Agent, "reason", failure.Reason)
	}
	return report, nil
}

// agentDirPresent 判定 `agents/<name>` 是否是真实目录：Lstat 与 AD-8 的归档判据同口径，
// 符号链接不算 Agent 目录（返回错误 → 交人查证）。
// agentDirPresent reports whether agents/<name> is a real directory; a symlink is an
// error, never "present".
func agentDirPresent(root, name string) (bool, error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer func() { _ = base.Close() }()

	info, statErr := base.Lstat(path.Join(agentsDirName, name))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return false, nil
		}
		return false, statErr
	}
	if !info.IsDir() {
		return false, fmt.Errorf("agents/%s 不是目录", name)
	}
	return true, nil
}

// archiveRowForOperation 按 `operation_id` + Agent 名找归档行：归档行与终态同事务
// （约束 37），pending 行命中只可能是崩溃窗口里的半成品，由调用方保守处理。
func archiveRowForOperation(ctx context.Context, store *storage.AgentAdminStore,
	op core.OperationRecord,
) (bool, core.AgentArchiveRecord, error) {
	rows, err := store.ListArchives(ctx)
	if err != nil {
		return false, core.AgentArchiveRecord{}, err
	}
	for _, row := range rows {
		if row.OperationID == op.OperationID && row.AgentName == op.TargetID {
			return true, row, nil
		}
	}
	return false, core.AgentArchiveRecord{}, nil
}

// operationVersion 在资源版本行里找本操作写的版本：版本只追加（约束 14），按
// `operation_id` 能唯一锁定"本次操作产出的候选版本"。
func operationVersion(ctx context.Context, deps RecoveryDeps, kind core.ResourceKind,
	op core.OperationRecord,
) (core.ResourceVersion, bool, error) {
	versions, err := deps.Resources.Versions(ctx, kind, op.TargetID)
	if err != nil {
		return core.ResourceVersion{}, false, err
	}
	for _, version := range versions {
		if version.OperationID == op.OperationID {
			return version, true, nil
		}
	}
	return core.ResourceVersion{}, false, nil
}

// versionMatchesFile 判定定义文件（技能为工作副本）的当前字节是否仍是 `published`
// 这一版本：两种已落库哈希形态都由 `definitionBlockMatchesFor` 接受，技能按完整文件
// 清单哈希比较；任一读取失败都返回错误（→ indeterminate），不把"读不到"当"一致"。
func versionMatchesFile(deps RecoveryDeps, kind core.ResourceKind, id string,
	published core.ResourceVersion,
) (bool, error) {
	if kind == core.ResourceSkill {
		if deps.Skills == nil {
			return false, errors.New("装配缺技能库")
		}
		version, err := deps.Skills.HashWorkCopy(id)
		if err != nil {
			return false, err
		}
		return version.ContentHash == published.ContentHash, nil
	}
	doc, err := definitionDocFor(deps, kind)
	if err != nil {
		return false, err
	}
	return definitionBlockMatchesFor(doc, definitionKeyOf(kind), id, published), nil
}

// definitionDocFor 读取某资源种类的定义文件原文（模型连接在实例配置、MCP 在
// `mcp_servers.yaml`，技能没有定义文件）。
func definitionDocFor(deps RecoveryDeps, kind core.ResourceKind) ([]byte, error) {
	switch kind {
	case core.ResourceModelConnection:
		if deps.ConfigPath == "" {
			return nil, errors.New("装配缺实例配置路径")
		}
		return readFileUnder(filepath.Dir(deps.ConfigPath), filepath.Base(deps.ConfigPath))
	case core.ResourceMCPServer:
		if deps.Root == "" {
			return nil, errors.New("装配缺工作区根")
		}
		return readFileUnder(deps.Root, mcpServersFileName)
	default:
		return nil, fmt.Errorf("资源种类 %s 没有定义文件", kind)
	}
}

// definitionKeyOf 给出定义文件的顶层键（模型连接 `providers`、MCP `servers`）。
func definitionKeyOf(kind core.ResourceKind) string {
	switch kind {
	case core.ResourceModelConnection:
		return providerDefinitionsKey
	case core.ResourceMCPServer:
		return mcpServersDefinitionsKey
	default:
		return ""
	}
}

// definitionBlockPresent 判定定义文件里是否存在某条目的块：两种已落库形态都接受
// （块编辑器形态与基线切块形态），任一路径能读出即算存在；两条都读不懂才报错。
// definitionBlockPresent accepts both stored block shapes; only an unreadable document
// is an error.
func definitionBlockPresent(doc []byte, key, name string) (bool, error) {
	blocks, blockErr := workspace.BlockEntries(doc, key)
	if blockErr == nil {
		for _, block := range blocks {
			if block.Name == name {
				return true, nil
			}
		}
	}
	definitions, defErr := extractDefinitionBlocks(doc, key)
	if defErr != nil {
		if blockErr != nil {
			return false, blockErr
		}
		return false, defErr
	}
	for _, definition := range definitions {
		if definition.name == name {
			return true, nil
		}
	}
	return false, nil
}

// resourceKindOfTarget 把操作行的目标类型映射成资源种类；`agent` 等非资源目标返回 false。
func resourceKindOfTarget(targetKind string) (core.ResourceKind, bool) {
	switch targetKind {
	case core.TargetKindModelConnection:
		return core.ResourceModelConnection, true
	case core.TargetKindMCPServer:
		return core.ResourceMCPServer, true
	case core.TargetKindSkill:
		return core.ResourceSkill, true
	default:
		return "", false
	}
}

// isLowerHexSHA256 判定 64 位小写十六进制摘要：`detail_json` 里读到的候选哈希必须是
// 这一形态，否则不能拿它去和 ETag 比。
func isLowerHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// loadProviderDefinitions 从实例配置原文里切出 `providers` 每条定义的原文块，并按
// `expected`（配置解析出的名字，文件序）逐名核对：名字或顺序不符即拒绝启动——
// 拿错一条定义去建发布版本，比开不了机更难查。
// loadProviderDefinitions extracts each provider's verbatim block and cross-checks the
// names against the parsed configuration, in file order.
func loadProviderDefinitions(configPath string, expected []string) ([]ProviderDefinition, error) {
	raw, err := readFileUnder(filepath.Dir(configPath), filepath.Base(configPath))
	if err != nil {
		// 配置文件缺失＝空配置（与装配面"缺文件按空配置继续"同口径）：没有声明就没有定义
		// 需要建版本；此时要求文件存在会把"无配置启动"从可用变成不可用。
		if errors.Is(err, fs.ErrNotExist) && len(expected) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("读取实例配置: %w", err)
	}
	blocks, err := extractDefinitionBlocks(raw, providerDefinitionsKey)
	if err != nil {
		return nil, fmt.Errorf("实例配置的 %s 段: %w", providerDefinitionsKey, err)
	}
	if err := checkDefinitionNames(providerDefinitionsKey, blockNames(blocks), expected); err != nil {
		return nil, err
	}
	definitions := make([]ProviderDefinition, 0, len(blocks))
	for _, block := range blocks {
		definitions = append(definitions, ProviderDefinition{Name: block.name, Content: block.content})
	}
	return definitions, nil
}

// loadMCPServerDefinitions 从 `mcp_servers.yaml` 原文里切出 `servers` 每条定义的原文块；
// 文件缺失且确实配置了 server 时拒绝启动（不能凭名字编造定义内容）。
// loadMCPServerDefinitions extracts each MCP server's verbatim block; a missing file
// with servers configured refuses startup instead of inventing content.
func loadMCPServerDefinitions(root string, expected []string) ([]MCPServerDefinition, error) {
	raw, err := readFileUnder(root, mcpServersFileName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && len(expected) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s: %w", mcpServersFileName, err)
	}
	blocks, err := extractDefinitionBlocks(raw, mcpServersDefinitionsKey)
	if err != nil {
		return nil, fmt.Errorf("%s 的 %s 段: %w", mcpServersFileName, mcpServersDefinitionsKey, err)
	}
	if err := checkDefinitionNames(mcpServersDefinitionsKey, blockNames(blocks), expected); err != nil {
		return nil, err
	}
	definitions := make([]MCPServerDefinition, 0, len(blocks))
	for _, block := range blocks {
		definitions = append(definitions, MCPServerDefinition{Name: block.name, Content: block.content})
	}
	return definitions, nil
}

// definitionBlock 是一条序列定义：名字（来自 AST）与逐行保真的原文块。
type definitionBlock struct {
	name    string
	content []byte
}

func blockNames(blocks []definitionBlock) []string {
	names := make([]string, 0, len(blocks))
	for _, block := range blocks {
		names = append(names, block.name)
	}
	return names
}

// checkDefinitionNames 逐名核对两串名字（含顺序）：数量或任一名字不符都拒绝启动。
func checkDefinitionNames(key string, got, expected []string) error {
	if len(got) != len(expected) {
		return fmt.Errorf("%s 段的条目数 %d 与配置解析结果 %d 不一致（%v vs %v）",
			key, len(got), len(expected), got, expected)
	}
	for i := range got {
		if got[i] != expected[i] {
			return fmt.Errorf("%s 段第 %d 条的名字 %q 与配置解析结果 %q 不一致",
				key, i+1, got[i], expected[i])
		}
	}
	return nil
}

// readFileUnder 在给定目录的 `os.Root` 下读一个文件名（不做路径拼接，杜绝目录穿越）。
func readFileUnder(dir, name string) (data []byte, err error) {
	base, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("打开目录 %s: %w", dir, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭目录 %s: %w", dir, closeErr))
		}
	}()
	data, err = base.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// extractDefinitionBlocks 从整份 YAML 原文里切出顶层键对应**块式序列**的每条原文块：
// 行区间由 AST 的 1 基行号确定，缩进由条目的 `-` 列号确定，行尾符原样保留。切出的
// 内容是该条目**映射本身**的原文——序列指示符只属于外层列表，发布出去的定义必须能
// 被 `parseProviderDefinition` / `parseMCPDefinition` 按映射解析。
// 流式序列（`[...]`）直接拒绝——无法逐行保真，而"退化成重新序列化"会丢注释与顺序。
// extractDefinitionBlocks slices each entry's verbatim lines using AST line numbers
// and the dash column; the sequence indicator is stripped because it belongs to the
// enclosing list, not to the definition. Flow-style sequences are refused rather than
// re-serialised.
func extractDefinitionBlocks(raw []byte, key string) ([]definitionBlock, error) {
	file, err := parser.ParseBytes(raw, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("解析 YAML: %w", err)
	}
	if len(file.Docs) == 0 {
		return nil, nil
	}
	if len(file.Docs) > 1 {
		return nil, fmt.Errorf("包含 %d 个 YAML 文档，只接受单文档", len(file.Docs))
	}
	body := file.Docs[0].Body
	if body == nil {
		return nil, nil
	}
	if _, ok := body.(*ast.NullNode); ok {
		return nil, nil
	}
	// 只有注释的文档在 `ParseComments` 下表现为 CommentGroupNode，它等价于空文档
	// （注释不是内容）：没有条目不是结构损坏，启动对账按「条目被删」处理。
	// A comment-only document is an empty document, not a structural error.
	if _, ok := body.(*ast.CommentGroupNode); ok {
		return nil, nil
	}
	mapping, ok := body.(*ast.MappingNode)
	if !ok {
		return nil, fmt.Errorf("顶层必须是映射，实际是 %T", body)
	}

	targetIndex := -1
	for i, value := range mapping.Values {
		name, isString := yamlKeyName(value.Key)
		if !isString || name != key {
			continue
		}
		if targetIndex >= 0 {
			return nil, fmt.Errorf("顶层键 %q 重复出现", key)
		}
		targetIndex = i
	}
	if targetIndex < 0 {
		return nil, nil
	}

	value := mapping.Values[targetIndex].Value
	if value == nil {
		return nil, nil
	}
	if _, isNull := value.(*ast.NullNode); isNull {
		return nil, nil
	}
	sequence, ok := value.(*ast.SequenceNode)
	if !ok {
		return nil, fmt.Errorf("顶层键 %q 必须是序列，实际是 %T", key, value)
	}
	if sequence.IsFlowStyle {
		return nil, fmt.Errorf("顶层键 %q 是流式序列（[...]），请改成逐条 `- key: value` 的块式写法", key)
	}
	if len(sequence.Entries) != len(sequence.Values) {
		return nil, fmt.Errorf("顶层键 %q 的条目数 %d 与值数 %d 不一致", key, len(sequence.Entries), len(sequence.Values))
	}
	if len(sequence.Entries) == 0 {
		return nil, nil
	}

	lines := strings.Split(string(raw), "\n")
	tailLine := 0 // 0 表示"直到文件末尾"
	if targetIndex+1 < len(mapping.Values) {
		token := mapping.Values[targetIndex+1].Key.GetToken()
		if token == nil || token.Position == nil {
			return nil, fmt.Errorf("顶层键 %q 之后的下一个顶层键缺少位置，无法确定切分边界", key)
		}
		tailLine = token.Position.Line
	}

	blocks := make([]definitionBlock, 0, len(sequence.Entries))
	for i, entry := range sequence.Entries {
		start := entry.Start
		if start == nil || start.Position == nil {
			return nil, fmt.Errorf("顶层键 %q 的第 %d 条缺少位置", key, i+1)
		}
		startLine := start.Position.Line - 1
		indent := start.Position.Column - 1

		endLine := len(lines)
		switch {
		case i+1 < len(sequence.Entries):
			next := sequence.Entries[i+1].Start
			if next == nil || next.Position == nil {
				return nil, fmt.Errorf("顶层键 %q 的第 %d 条之后缺少位置", key, i+1)
			}
			endLine = next.Position.Line - 1
		case tailLine > 0:
			endLine = tailLine - 1
		}
		if startLine < 0 || endLine > len(lines) || startLine >= endLine {
			return nil, fmt.Errorf("顶层键 %q 的第 %d 条行区间 [%d,%d) 非法", key, i+1, startLine, endLine)
		}

		content, err := dedentBlockLines(lines[startLine:endLine], indent)
		if err != nil {
			return nil, fmt.Errorf("顶层键 %q 的第 %d 条: %w", key, i+1, err)
		}
		content, err = stripEntryIndicator(content)
		if err != nil {
			return nil, fmt.Errorf("顶层键 %q 的第 %d 条: %w", key, i+1, err)
		}
		// 切出的原文块必须仍是一份合法 YAML：解析成功不保证语义对，但切错行一定在这里
		// 暴露——拿半截定义去建发布版本，比开不了机更难查。
		var probe any
		if err := yaml.Unmarshal(content, &probe); err != nil {
			return nil, fmt.Errorf("顶层键 %q 的第 %d 条切分后不是合法 YAML: %w", key, i+1, err)
		}
		name, err := yamlEntryName(entry)
		if err != nil {
			return nil, fmt.Errorf("顶层键 %q 的第 %d 条: %w", key, i+1, err)
		}
		blocks = append(blocks, definitionBlock{name: name, content: content})
	}
	return blocks, nil
}

// dedentBlockLines 去掉条目公共缩进并裁掉尾部不属于条目的行（空行、缩进不足的注释），
// 行尾符按原样保留（CRLF 不归一）。缩进不足的**非注释**行视为切分错误。
func dedentBlockLines(lines []string, indent int) ([]byte, error) {
	for len(lines) > 1 {
		last := lines[len(lines)-1]
		trimmed := strings.TrimSpace(last)
		if trimmed == "" {
			lines = lines[:len(lines)-1]
			continue
		}
		if strings.HasPrefix(trimmed, "#") && leadingSpaceCount(last) < indent {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}

	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		if leadingSpaceCount(line) >= indent {
			out = append(out, line[indent:])
			continue
		}
		if strings.HasPrefix(strings.TrimLeft(line, " "), "#") {
			out = append(out, line)
			continue
		}
		return nil, fmt.Errorf("行 %q 的缩进少于条目起始列 %d", line, indent+1)
	}
	return []byte(strings.Join(out, "\n")), nil
}

// stripEntryIndicator 把条目原文首行的序列指示符替换成**等宽空格**：`-` 属于外层
// 列表而非资源定义本身，发布出去的定义原文因此是映射（资源视图按映射解析）；用空格
// 而不是删除，是为了让首行之后每一行的列位与行尾符逐字不变（`\r\n` 也原样保留）。
// stripEntryIndicator replaces the leading sequence dash with one space: the dash
// belongs to the enclosing list, not to the definition, and replacing instead of
// deleting keeps every later byte's column and line ending untouched.
func stripEntryIndicator(content []byte) ([]byte, error) {
	if len(content) == 0 || content[0] != '-' {
		return nil, fmt.Errorf("条目原文首行不是 `-` 开头的块式序列条目")
	}
	out := make([]byte, len(content))
	copy(out, content)
	out[0] = ' '
	return out, nil
}

// leadingSpaceCount 数行首空格（YAML 的缩进只允许空格；制表符不计入）。
func leadingSpaceCount(line string) int {
	count := 0
	for count < len(line) && line[count] == ' ' {
		count++
	}
	return count
}

// yamlKeyName 取映射键的字面名字；非字符串键返回 false（本流程不认其它键形态）。
func yamlKeyName(key ast.MapKeyNode) (string, bool) {
	switch typed := key.(type) {
	case *ast.StringNode:
		return typed.Value, true
	case *ast.MappingKeyNode:
		inner, ok := typed.Value.(ast.MapKeyNode)
		if !ok {
			return "", false
		}
		return yamlKeyName(inner)
	}
	return "", false
}

// yamlEntryName 取序列条目的 `name` 键值；条目必须是映射且带非空字符串 name。
func yamlEntryName(entry *ast.SequenceEntryNode) (string, error) {
	switch value := entry.Value.(type) {
	case *ast.MappingNode:
		return yamlMappingName(value.Values)
	case *ast.MappingValueNode:
		return yamlMappingName([]*ast.MappingValueNode{value})
	default:
		return "", fmt.Errorf("序列条目必须是映射（含 name 键），实际是 %T", entry.Value)
	}
}

// yamlMappingName 在一组映射条目里找 `name`。
func yamlMappingName(values []*ast.MappingValueNode) (string, error) {
	for _, value := range values {
		name, ok := yamlKeyName(value.Key)
		if !ok || name != "name" {
			continue
		}
		scalar, isString := value.Value.(*ast.StringNode)
		if !isString || scalar.Value == "" {
			return "", errors.New("name 必须是非空字符串")
		}
		return scalar.Value, nil
	}
	return "", errors.New("条目缺少 name 键")
}
