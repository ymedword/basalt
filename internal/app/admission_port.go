package app

// 本文件是学习准入端口的生产适配器（颗粒度文档 §3.1.10、§3.2 P-04、T012）：
// 命令面只认 `cli.AdmissionPort`，存储 / 工作区 / 策略表的实现全部在这里装配。
//
// 四条纪律：
//   - **实例锁由命令面持有**：适配器**不**再次取锁（独占锁会直接失败），只在
//     `basalt.yaml` 与工作区检查通过后打开数据库；写会话内部再按技能取进程内互斥，
//     与手工技能写共用同一张锁表（`ResourceAdmin.LockResource`）。
//   - **策略快照在锁内拍**：`execution_policy`、`network_domain_policy` 两行与
//     `network_exceptions` 从数据库读，YAML 侧的四段由命令面传入；任一表缺行即失败，
//     **不**用空值代替（空策略会让 policy_hash 假装"没有限制"）。
//   - **读会话只读打开**：list / show 走 `postgres.OpenReadOnly`——不迁移、不取锁、
//     不写审计；写会话走 `postgres.Open`（内含前向迁移），关闭时按"业务已生效"的
//     口径只记 ERROR，不改写成功的结论。
//   - **只构造管理面**：不装配 HTTP / MCP / 调度器 / 文件监视器，不启动第二套运行期。
//
// The production adapter behind cli.AdmissionPort. The command surface already holds the
// instance lock; this layer only opens the database, wires the minimal management surface
// and snapshots the database policy rows. Reads open read-only and never migrate.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// admissionPort 是 `cli.AdmissionPort` 的生产实现。open / openReadOnly 是测试注入点
// （默认 `postgres.Open` / `postgres.OpenReadOnly`）：用例据此证明"不可信身份在开库前
// 被拒绝、零连接"，而不必靠日志旁证。
// The production AdmissionPort. The two open hooks default to the real openers and exist
// so a test can prove the untrusted-actor path never opens a connection.
type admissionPort struct {
	WorkDir                string
	SupportedSchemaVersion int
	Database               cli.DatabaseConfig
	Logger                 *slog.Logger

	open         func(context.Context, postgres.Config) (*postgres.DB, error)
	openReadOnly func(context.Context, postgres.Config) (*postgres.DB, error)
}

// admissionSession 是一次端口调用的库句柄与准入编排器。
// admissionSession pairs one short-lived database handle with the orchestrator.
type admissionSession struct {
	db    *postgres.DB
	admin *AdmissionAdmin
}

// ImportFiles 转调工作区的目录读取（不跟随软链接、拒绝穿越、复用草稿体积上限）。
// 放在端口上的原因写在接口注释里：cli 不许 import workspace，边界检查只能有一份实现。
// ImportFiles delegates to the workspace reader so the boundary checks keep one home.
func (p admissionPort) ImportFiles(dir string) ([]core.AuthorFile, error) {
	return workspace.ReadSkillSourceDir(dir)
}

// ReadSourceFile 转调工作区的来源声明读取（≤64 KiB、拒绝软链接与非普通文件）。
// ReadSourceFile delegates to the bounded workspace reader.
func (p admissionPort) ReadSourceFile(path string) ([]byte, error) {
	return workspace.ReadAdmissionSourceFile(path)
}

// ReadDecisionFile 转调工作区的决策文件读取：与来源声明同一体积与文件类型约束。
// ReadDecisionFile delegates to the same bounded reader the source file uses.
func (p admissionPort) ReadDecisionFile(path string) ([]byte, error) {
	return workspace.ReadAdmissionDecisionFile(path)
}

// Propose 在写会话里冻结候选；调用方必须已持有实例锁。
// Propose freezes the candidate inside a write session; the caller holds the lock.
func (p admissionPort) Propose(ctx context.Context, actor core.Actor,
	req core.AdmissionProposeRequest,
) (core.AdmissionCandidate, error) {
	var candidate core.AdmissionCandidate
	err := p.withWriteSession(ctx, func(ctx context.Context, session *admissionSession) error {
		var err error
		candidate, err = session.admin.Propose(ctx, actor, req)
		return err
	})
	return candidate, err
}

// Evaluate 先拍策略快照再跑七维评估；快照失败即中止，不落任何评估行。
// Evaluate snapshots the policy first; a missing row aborts before any evaluation write.
func (p admissionPort) Evaluate(ctx context.Context, actor core.Actor, candidateID string,
	cfg core.AdmissionPolicyConfig,
) (core.AdmissionEvaluation, error) {
	var evaluation core.AdmissionEvaluation
	err := p.withWriteSession(ctx, func(ctx context.Context, session *admissionSession) error {
		policy, err := admissionPolicySnapshot(ctx, session.db, cfg)
		if err != nil {
			return err
		}
		evaluation, err = session.admin.Evaluate(ctx, actor, candidateID, policy)
		return err
	})
	return evaluation, err
}

// Approve 先拍策略快照再批准：批准必须与评估 / 发布绑定同一策略，快照失败即中止，
// 不落任何决策行（与 Evaluate 同一纪律）。
// Approve snapshots the policy first so the approval binds the same policy hash the
// evaluation saw; a failed snapshot writes no decision row.
func (p admissionPort) Approve(ctx context.Context, actor core.Actor, candidateID string,
	decisionJSON []byte, cfg core.AdmissionPolicyConfig,
) (core.AdmissionDecision, error) {
	var decision core.AdmissionDecision
	err := p.withWriteSession(ctx, func(ctx context.Context, session *admissionSession) error {
		policy, err := admissionPolicySnapshot(ctx, session.db, cfg)
		if err != nil {
			return err
		}
		decision, err = session.admin.Approve(ctx, actor, candidateID, decisionJSON, policy)
		return err
	})
	return decision, err
}

// Reject 在写会话里拒绝候选；拒绝不做策略绑定复核，因此不拍策略快照。
// Reject runs in a write session without a policy snapshot: a rejection binds no policy.
func (p admissionPort) Reject(ctx context.Context, actor core.Actor, candidateID string,
	decisionJSON []byte,
) (core.AdmissionDecision, error) {
	var decision core.AdmissionDecision
	err := p.withWriteSession(ctx, func(ctx context.Context, session *admissionSession) error {
		var err error
		decision, err = session.admin.Reject(ctx, actor, candidateID, decisionJSON)
		return err
	})
	return decision, err
}

// Cancel 在写会话里取消未发布候选。
// Cancel cancels an unpublished candidate inside a write session.
func (p admissionPort) Cancel(ctx context.Context, actor core.Actor, candidateID, reason string,
) (core.AdmissionCandidate, error) {
	var candidate core.AdmissionCandidate
	err := p.withWriteSession(ctx, func(ctx context.Context, session *admissionSession) error {
		var err error
		candidate, err = session.admin.Cancel(ctx, actor, candidateID, reason)
		return err
	})
	return candidate, err
}

// Publish 先拍策略快照再发布：发布复核「评估 / 批准 / 当前策略」三方哈希一致。
// Publish snapshots the policy first: the publish re-check binds evaluation, approval
// and the policy actually in force.
func (p admissionPort) Publish(ctx context.Context, actor core.Actor, candidateID string,
	cfg core.AdmissionPolicyConfig,
) (core.AdmissionPublication, error) {
	var publication core.AdmissionPublication
	err := p.withWriteSession(ctx, func(ctx context.Context, session *admissionSession) error {
		policy, err := admissionPolicySnapshot(ctx, session.db, cfg)
		if err != nil {
			return err
		}
		publication, err = session.admin.Publish(ctx, actor, candidateID, policy)
		return err
	})
	return publication, err
}

// Withdraw 在写会话里撤回已发布候选；上一版恢复与撤回分类都在编排层判定，
// 适配器只负责写会话边界。
// Withdraw runs the withdrawal inside a write session; classification stays in the
// orchestrator and the adapter only owns the session boundary.
func (p admissionPort) Withdraw(ctx context.Context, actor core.Actor, candidateID, reason string,
) (core.AdmissionPublication, error) {
	var publication core.AdmissionPublication
	err := p.withWriteSession(ctx, func(ctx context.Context, session *admissionSession) error {
		var err error
		publication, err = session.admin.Withdraw(ctx, actor, candidateID, reason)
		return err
	})
	return publication, err
}

// List 是只读查询：只读会话、不取实例锁、不写审计。
// List is the read-only query: no lock, no audit row, no migration.
func (p admissionPort) List(ctx context.Context, filter core.AdmissionFilter) ([]core.AdmissionCandidate, error) {
	var candidates []core.AdmissionCandidate
	err := p.withReadSession(ctx, func(ctx context.Context, session *admissionSession) error {
		var err error
		candidates, err = session.admin.List(ctx, filter)
		return err
	})
	return candidates, err
}

// Get 是只读详情；文件正文与 diff 由编排层按选项从磁盘重读并复算哈希。
// Get is the read-only detail; bodies are re-read and hash-checked by the orchestrator.
func (p admissionPort) Get(ctx context.Context, candidateID string,
	options core.AdmissionShowOptions,
) (core.AdmissionDetail, error) {
	var detail core.AdmissionDetail
	err := p.withReadSession(ctx, func(ctx context.Context, session *admissionSession) error {
		var err error
		detail, err = session.admin.Get(ctx, candidateID, options)
		return err
	})
	return detail, err
}

// withWriteSession 打开可写会话并执行 fn：句柄在成功与失败路径都关闭；成功路径的关闭
// 失败只记 ERROR——写入已生效时不得把结论改写为失败。
// withWriteSession closes the handle on both outcomes; a close failure after commits is
// logged, never turned into a failure verdict.
func (p admissionPort) withWriteSession(ctx context.Context, fn func(context.Context, *admissionSession) error) error {
	if p.SupportedSchemaVersion <= 0 {
		return errors.New("学习准入写路径缺少 schema 目标版本（装配错误）")
	}
	open := p.open
	if open == nil {
		open = postgres.Open
	}
	db, err := open(ctx, postgresConfig(p.Database))
	if err != nil {
		return err
	}
	runErr := fn(ctx, p.newSession(db, true))
	closeErr := db.Close()
	if runErr != nil {
		return errors.Join(runErr, closeErr)
	}
	if closeErr != nil {
		p.logger().Error("学习准入写路径关闭数据库失败（业务写入已生效）",
			"workspace", p.root(), "error", closeErr)
	}
	return nil
}

// withReadSession 打开只读会话（`postgres.OpenReadOnly`：不建库、不迁移、会话只读）。
// withReadSession opens the server-pinned read-only session and never migrates.
func (p admissionPort) withReadSession(ctx context.Context, fn func(context.Context, *admissionSession) error) error {
	open := p.openReadOnly
	if open == nil {
		open = postgres.OpenReadOnly
	}
	db, err := open(ctx, postgresConfig(p.Database))
	if err != nil {
		return err
	}
	runErr := fn(ctx, p.newSession(db, false))
	closeErr := db.Close()
	if runErr != nil {
		return errors.Join(runErr, closeErr)
	}
	if closeErr != nil {
		p.logger().Warn("学习准入只读路径关闭数据库失败", "workspace", p.root(), "error", closeErr)
	}
	return nil
}

// newSession 按 P-04 的装配顺序构造最小组件：资源存储 / 管理操作 → 最小
// `ResourceAdmin` → `AdmissionAdmin`（共享 `Gate` 与 `LockResource` 锁表）。
//
// 只读会话不建 `ResourceAdmin`：`LockResource` / `References` 传 nil，写方法因此
// fail-closed 而不是在没有锁表的情况下静默执行（纵深防御）。
// newSession follows the P-04 assembly order. A read session passes nil LockResource and
// References, so a misused write method fails closed instead of running unlocked.
func (p admissionPort) newSession(db *postgres.DB, writable bool) *admissionSession {
	root := p.root()
	resources := postgres.NewResourceStore(db)
	operations := postgres.NewManagementStore(db)
	gate := &core.ResourceGate{}
	admin := NewAdmissionAdmin(AdmissionAdminDeps{
		Root:       root,
		Store:      postgres.NewAdmissionStore(db),
		Resources:  resources,
		Operations: operations,
		Gate:       gate,
		Logger:     p.logger(),
		Now:        time.Now,
	})
	if writable {
		// 最小 ResourceAdmin 只为取得与手工技能写**同一张**按键互斥表；它的发布入口
		// 在本路径上不被调用（发布由 AdmissionAdmin 经 publishTx 完成，T015）。
		// The minimal ResourceAdmin exists solely to share the keyed lock table.
		resourceAdmin := NewResourceAdmin(ResourceAdminDeps{
			Root:       root,
			Store:      resources,
			Operations: operations,
			Gate:       gate,
			Logger:     p.logger(),
			Now:        time.Now,
		})
		admin.lockResource = resourceAdmin.LockResource
		admin.references = func(skillName string) ([]string, error) {
			return scanSkillReferences(root, skillName)
		}
	}
	return &admissionSession{db: db, admin: admin}
}

// admissionPolicySnapshot 读取数据库三张策略表并合成完整策略快照。
//
// 缺行一律失败：`execution_policy` 尚无生效行说明策略从未发布，`network_domain_policy`
// 缺任一行说明域名策略不完整——用空集合顶替会让 policy_hash 与真实生效策略脱钩，
// 正是"评估绑定的策略"要防的事。
// A missing row is an error, never an empty default: substituting an empty set would
// decouple policy_hash from the policy actually in force.
func admissionPolicySnapshot(ctx context.Context, db *postgres.DB,
	cfg core.AdmissionPolicyConfig,
) (core.AdmissionPolicy, error) {
	execution, ok, err := postgres.NewExecutionPolicyStore(db).Load(ctx)
	if err != nil {
		return core.AdmissionPolicy{}, fmt.Errorf("读取执行策略: %w", err)
	}
	if !ok {
		return core.AdmissionPolicy{}, errors.New(
			"策略快照不完整：execution_policy 尚无生效行——先由管理面发布一次执行策略")
	}
	allowed, ok, err := postgres.NewNetworkPolicyStore(db).Load(ctx, core.PolicyScopeHTTPDomains)
	if err != nil {
		return core.AdmissionPolicy{}, fmt.Errorf("读取出网域名策略: %w", err)
	}
	if !ok {
		return core.AdmissionPolicy{}, fmt.Errorf(
			"策略快照不完整：network_domain_policy 缺少 %s 行", core.PolicyScopeHTTPDomains)
	}
	denied, ok, err := postgres.NewNetworkPolicyStore(db).Load(ctx, core.PolicyScopeHTTPDeniedDomains)
	if err != nil {
		return core.AdmissionPolicy{}, fmt.Errorf("读取出网禁止域名策略: %w", err)
	}
	if !ok {
		return core.AdmissionPolicy{}, fmt.Errorf(
			"策略快照不完整：network_domain_policy 缺少 %s 行", core.PolicyScopeHTTPDeniedDomains)
	}
	exceptions, err := postgres.NewNetworkExceptionStore(db).List(ctx)
	if err != nil {
		return core.AdmissionPolicy{}, fmt.Errorf("读取网络例外: %w", err)
	}
	return BuildAdmissionPolicy(cfg, execution,
		[]core.NetworkDomainPolicy{allowed, denied}, exceptions), nil
}

// root 是工作区根目录（`<WorkDir>/.basalt`）；与 run.go 的装配口径同一常量。
// root is <WorkDir>/.basalt, sharing the assembly layer's constant.
func (p admissionPort) root() string {
	return filepath.Join(p.WorkDir, workspaceDirName)
}

// logger 保证日志出口永远可用（与 agentsPort 同口径）。
// logger keeps the log outlet usable even when none was injected.
func (p admissionPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

// 编译期断言：适配器始终满足命令面的端口；签名漂移在编译期就暴露。
// Compile-time assertion that the adapter still satisfies the command surface's port.
var _ cli.AdmissionPort = admissionPort{}
