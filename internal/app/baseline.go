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
}

// RecoveryDeps 是启动恢复的全部依赖；与 runBaseline 分开，恢复只查证、不重做。
// `Registry` 与 `Scheduler` 属 US-12 的扩展面：本 US 的查证只需要磁盘与状态文件，
// 注册表在 ⑤ 才构建、调度器在 ⑦ 才构造，恢复不依赖它们的当前值。
// RecoveryDeps carries the recovery dependencies. Registry and Scheduler are carried
// for US-12's expansion; this US verifies against the disk and the state file only.
type RecoveryDeps struct {
	Operations core.OperationStore
	Resources  *storage.ResourceStore
	Lifecycle  *workspace.LifecycleStore
	Editor     *workspace.AgentEditor
	Registry   *agentRegistry
	Scheduler  *core.Scheduler
	Logger     *slog.Logger
	Now        func() time.Time
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
	pending, err := operations.PendingOperations(ctx)
	if err != nil {
		return "", false, fmt.Errorf("启动基线：读取未完成操作失败: %w", err)
	}
	var ids []string
	for _, op := range pending {
		if op.Action == actionBaseline {
			ids = append(ids, op.OperationID)
		}
	}
	switch len(ids) {
	case 0:
		return "", false, nil
	case 1:
		return ids[0], true, nil
	default:
		return "", false, fmt.Errorf("启动基线：发现 %d 条未完成的 %s 操作（%s），无法确定复用哪一条",
			len(ids), actionBaseline, strings.Join(ids, "、"))
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
			work = append(work, skillWork{name: name, create: true, work: copyVersion})
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
		materialized, err := deps.Skills.Materialize(ctx, item.name, item.version.VersionNo, item.version.ContentHash)
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
			if _, materializeErr := deps.Skills.Materialize(ctx, item.name, existing.VersionNo, existing.ContentHash); materializeErr != nil {
				return fmt.Errorf("技能 %s 的发布版本 v%d 快照缺失，且无法按该版本重建: %w",
					item.name, existing.VersionNo, materializeErr)
			}
		}
		return nil
	}

	materialized, err := deps.Skills.Materialize(ctx, item.name, baselineVersionNo, item.work.ContentHash)
	if err != nil {
		return fmt.Errorf("技能 %s 物化首版快照: %w", item.name, err)
	}
	manifest, err := marshalSkillFileList(materialized.Files)
	if err != nil {
		return fmt.Errorf("技能 %s 的文件清单: %w", item.name, err)
	}
	version, inserted, err := deps.Resources.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceSkill,
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

// verifyOperation 按 action 组选择查证方式；其它 action（含 `system.baseline_v2`——
// 它已在 ③ 完成或已拒绝启动）一律无法判定 → indeterminate，US-12 再扩展具体查证。
// verifyOperation dispatches by action; unknown actions stay indeterminate.
func verifyOperation(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	switch op.Action {
	case actionContentUpdate:
		return verifyContentUpdate(ctx, deps, op)
	case actionDisable, actionEnable:
		return verifyLifecycleOperation(ctx, deps, op)
	default:
		return core.VerificationIndeterminate
	}
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
