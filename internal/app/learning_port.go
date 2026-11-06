package app

// 本文件是受控自我进化端口的生产适配器（颗粒度文档 §3.1.10 / T023 / T027）：
// 命令面只认 `cli.LearningPort`，存储、草案目录与实例锁的实现全部在这里装配。
//
// 三条纪律：
//  1. **查询只读**：status / extractions / drafts 走 `postgres.OpenReadOnly`，
//     不取实例锁（服务运行时可执行），也不打开可写会话。
//  2. **handoff 先锁后库**：实例锁在打开任何数据库之前取得（与 agentsPort 的
//     `withCLIAdmin` 同序），锁内才开可写库并构造 US-21 的 `AdmissionAdmin`——
//     提议入口只有一份实现，本适配器不复制准入写出逻辑。
//  3. **不开第二套运行期**：不装配 HTTP / MCP / 调度器 / 文件监视器；每次调用自建
//     句柄并在返回前关闭。
//
// The production adapter behind cli.LearningPort. Queries open a server-pinned
// read-only session and take no instance lock; the handoff takes the instance lock
// before any database is opened and reuses the US-21 admission admin for the propose
// entry point. No second runtime is assembled.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/memory"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// learningPort 是 `cli.LearningPort` 的生产实现。open / openReadOnly 是测试注入点
// （默认 `postgres.Open` / `postgres.OpenReadOnly`）：用例据此证明查询路径零可写连接。
// The production LearningPort. The two open hooks default to the real openers and exist
// so a test can prove the read commands never open a writable session.
type learningPort struct {
	WorkDir                string
	SupportedSchemaVersion int
	Database               cli.DatabaseConfig
	AutoExtract            bool
	Agents                 []string
	Logger                 *slog.Logger

	open         func(context.Context, postgres.Config) (*postgres.DB, error)
	openReadOnly func(context.Context, postgres.Config) (*postgres.DB, error)
	now          func() time.Time
}

// learningPortConfig 投影 `learning` 段；配置装载失败（cfg=nil）时给零值 = 关闭，
// 与 databaseConfig 同一口径——缺配置由各路径 fail-closed，不在适配器里猜。
// learningPortConfig projects the learning section; a missing config yields the zero
// value (disabled), matching databaseConfig.
func learningPortConfig(cfg *cli.Config) cli.LearningConfig {
	if cfg == nil {
		return cli.LearningConfig{}
	}
	return cfg.Learning
}

// learningSession 是一次端口调用的库句柄与组件：查询走 admin，handoff 走
// repo（草案读取）+ admission（提议入口）。
// learningSession pairs one short-lived database handle with the read face or the
// handoff components.
type learningSession struct {
	db        *postgres.DB
	repo      *postgres.LearningRepo
	admin     *LearningAdmin
	admission *AdmissionAdmin
}

// Status 只读返回学习面状态（含配置名单与未注册名单）。
// Status returns the learning status read model.
func (p learningPort) Status(ctx context.Context) (core.LearningStatus, error) {
	var status core.LearningStatus
	err := p.withReadSession(ctx, true, func(ctx context.Context, session *learningSession) error {
		var err error
		status, err = session.admin.Status(ctx)
		return err
	})
	return status, err
}

// ListExtractions 只读列出抽取账本投影。
// ListExtractions lists the extraction projections read-only.
func (p learningPort) ListExtractions(ctx context.Context, filter core.LearningExtractionFilter) ([]core.LearningExtractionView, error) {
	var views []core.LearningExtractionView
	err := p.withReadSession(ctx, false, func(ctx context.Context, session *learningSession) error {
		var err error
		views, err = session.admin.ListExtractions(ctx, filter)
		return err
	})
	return views, err
}

// LoadExtraction 只读返回一次抽取；不存在映射为 `core.ErrLearningNotFound`。
// LoadExtraction returns one extraction or ErrLearningNotFound.
func (p learningPort) LoadExtraction(ctx context.Context, extractionID string) (core.LearningExtractionView, error) {
	var view core.LearningExtractionView
	err := p.withReadSession(ctx, false, func(ctx context.Context, session *learningSession) error {
		var err error
		view, err = session.admin.LoadExtraction(ctx, extractionID)
		return err
	})
	return view, err
}

// ListDrafts 只读列出草案元数据并从 DRAFT.md 填充正文（正文只在 drafts show 出口使用）。
// ListDrafts lists draft metadata with bodies filled from the fixed layout.
func (p learningPort) ListDrafts(ctx context.Context, filter core.LearningDraftFilter) ([]core.LearningDraftView, error) {
	var views []core.LearningDraftView
	err := p.withReadSession(ctx, false, func(ctx context.Context, session *learningSession) error {
		var err error
		views, err = session.admin.ListDrafts(ctx, filter)
		return err
	})
	return views, err
}

// LoadDraft 只读返回一份草案（元数据 + 正文）；不存在映射为 `core.ErrLearningDraftNotFound`。
// LoadDraft returns one draft or ErrLearningDraftNotFound.
func (p learningPort) LoadDraft(ctx context.Context, draftID string) (core.LearningDraftView, error) {
	var view core.LearningDraftView
	err := p.withReadSession(ctx, false, func(ctx context.Context, session *learningSession) error {
		var err error
		view, err = session.admin.LoadDraft(ctx, draftID)
		return err
	})
	return view, err
}

// Handoff 先取实例锁、再开可写库，最后调用 `LearningHandoff` 的薄适配。
// 锁在开库之前取得（agentsPort 同序）：实例在跑时在这里就失败，不先做一次迁移。
// Handoff takes the instance lock before opening the writable database and then runs
// the thin handoff adapter; a running service fails here before any migration.
func (p learningPort) Handoff(ctx context.Context, req core.LearningHandoffRequest) (core.LearningHandoffResult, error) {
	var result core.LearningHandoffResult
	err := p.withWriteSession(ctx, func(ctx context.Context, session *learningSession) error {
		handoff := NewLearningHandoff(LearningHandoffDeps{
			Drafts:      learningDraftReader{repo: session.repo},
			Admission:   session.admission,
			Root:        p.root(),
			AcquireLock: learningLockAlreadyHeld,
			Logger:      p.logger(),
		})
		var err error
		result, err = handoff.Handoff(ctx, req)
		return err
	})
	return result, err
}

// learningLockAlreadyHeld 是交给 `LearningHandoff` 的取锁器：真正的实例锁已由
// `withWriteSession` 在**打开数据库之前**持有，这里只返回一个空释放函数。
// 实例锁不可重入，适配器内再取一次会把一次正常转交变成「锁被占用」；薄适配的
// nil 检查与「锁内才读草案」次序仍然成立。
// learningLockAlreadyHeld reports the instance lock the port already holds; taking it
// twice would turn a valid handoff into a lock conflict. The adapter's nil check and
// its lock-before-draft-read ordering are unaffected.
func learningLockAlreadyHeld(string) (func() error, error) {
	return func() error { return nil }, nil
}

// learningDraftReader 是 handoff 的草案读取面：只经锁内的同一仓储取元数据，
// 不读文件、不写任何行（文件布局与只读性归 internal/memory）。
// learningDraftReader reads draft metadata through the session repository only.
type learningDraftReader struct {
	repo *postgres.LearningRepo
}

// LoadDraft 返回草案元数据与存在性；不存在时 found=false（映射归薄适配）。
// LoadDraft returns the draft metadata and whether the row exists.
func (r learningDraftReader) LoadDraft(ctx context.Context, draftID string) (core.LearningDraftView, bool, error) {
	if r.repo == nil {
		return core.LearningDraftView{}, false, errors.New("学习转交：仓储未装配")
	}
	return r.repo.LoadDraft(ctx, draftID)
}

// withReadSession 打开只读会话并执行 fn：不取实例锁、不迁移、不写审计；
// 句柄在成功与失败路径都关闭。
// withReadSession opens the server-pinned read-only session and closes it on both
// outcomes; it takes no instance lock and never migrates.
func (p learningPort) withReadSession(ctx context.Context, withUnknown bool,
	fn func(context.Context, *learningSession) error,
) error {
	open := p.openReadOnly
	if open == nil {
		open = postgres.OpenReadOnly
	}
	db, err := open(ctx, postgresConfig(p.Database))
	if err != nil {
		return err
	}
	session, err := p.newReadSession(db, withUnknown)
	if err != nil {
		return errors.Join(err, db.Close())
	}
	runErr := fn(ctx, session)
	closeErr := db.Close()
	if runErr != nil {
		return errors.Join(runErr, closeErr)
	}
	if closeErr != nil {
		p.logger().Warn("学习查询路径关闭数据库失败", "workspace", p.root(), "error", closeErr)
	}
	return nil
}

// withWriteSession 在实例锁内打开可写库并执行 fn：锁 → 开库（含前向迁移）→ 执行 →
// 关库 → 释放锁，次序与 `agentsPort.withCLIAdmin` 相同。
// withWriteSession runs fn under the instance lock: lock, open (with the forward
// migration), run, close, release — the same order as agentsPort.withCLIAdmin.
func (p learningPort) withWriteSession(ctx context.Context, fn func(context.Context, *learningSession) error) error {
	if p.SupportedSchemaVersion <= 0 {
		return errors.New("学习转交缺少 schema 目标版本（装配错误）")
	}
	return withInstanceLockErr(p.root(), func() error {
		open := p.open
		if open == nil {
			open = postgres.Open
		}
		db, err := open(ctx, postgresConfig(p.Database))
		if err != nil {
			return err
		}
		session := p.newWriteSession(db)
		runErr := fn(ctx, session)
		closeErr := db.Close()
		if runErr != nil {
			return errors.Join(runErr, closeErr)
		}
		if closeErr != nil {
			p.logger().Error("学习转交关闭数据库失败（业务写入已生效）",
				"workspace", p.root(), "error", closeErr)
		}
		return nil
	})
}

// newReadSession 构造只读查询面。`withUnknown` 只在 status 为真：计算未注册名单
// 需要扫描活动 Agent 目录，其余查询不为此读盘。
// newReadSession builds the read face; the unknown-agent scan runs for status only.
func (p learningPort) newReadSession(db *postgres.DB, withUnknown bool) (*learningSession, error) {
	var unknown []string
	if withUnknown {
		registered, err := learningRegisteredAgents(p.root())
		if err != nil {
			return nil, fmt.Errorf("学习状态：扫描 Agent 目录: %w", err)
		}
		unknown = unknownLearningAgents(p.Agents, registered)
	}
	repo := postgres.NewLearningRepo(db)
	admin := NewLearningAdmin(LearningAdminDeps{
		Store:         repo,
		DraftRoot:     p.draftRoot(),
		Enabled:       p.AutoExtract,
		Agents:        p.Agents,
		UnknownAgents: unknown,
		Limits:        memory.DefaultLearningLimits(),
		Logger:        p.logger(),
		Now:           p.nowOr(),
	})
	return &learningSession{db: db, repo: repo, admin: admin}, nil
}

// newWriteSession 构造 handoff 写会话：草案读取用同一仓储，提议入口复用 US-21
// 的 `AdmissionAdmin`（含按技能取锁与引用扫描的接线），不复制准入写出逻辑。
// newWriteSession wires the handoff session; the propose entry point reuses the US-21
// AdmissionAdmin wholesale instead of re-implementing admission writes.
func (p learningPort) newWriteSession(db *postgres.DB) *learningSession {
	admissionSession := admissionPort{
		WorkDir:                p.WorkDir,
		SupportedSchemaVersion: p.SupportedSchemaVersion,
		Database:               p.Database,
		Logger:                 p.Logger,
	}.newSession(db, true)
	return &learningSession{
		db:        db,
		repo:      postgres.NewLearningRepo(db),
		admission: admissionSession.admin,
	}
}

// learningRegisteredAgents 返回工作区活动 Agent 目录名（归档不在注册集合里）。
// 只扫描 `AGENT.md` 的存在性单元：目录名合法但缺 `AGENT.md` 的不算已注册。
// learningRegisteredAgents lists the active agent directories that own an AGENT.md;
// archived entries are excluded, matching what the loader would register.
func learningRegisteredAgents(root string) ([]string, error) {
	docs, err := workspace.ScanAgentDocuments(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(docs))
	for _, doc := range docs {
		if doc.Archived {
			continue
		}
		names = append(names, doc.Name)
	}
	return names, nil
}

// root 是工作区根目录（`<WorkDir>/.basalt`）；与 run.go 的装配口径同一常量。
// root is <WorkDir>/.basalt, sharing the assembly layer's constant.
func (p learningPort) root() string {
	return filepath.Join(p.WorkDir, workspaceDirName)
}

// draftRoot 是方法草案目录（`<workspace>/.basalt/learning/drafts`），与 wire.go
// 的装配口径一致——CLI 与运行期读到的是同一批文件。
// draftRoot is <workspace>/.basalt/learning/drafts, the same directory the runtime uses.
func (p learningPort) draftRoot() string {
	return filepath.Join(p.root(), "learning", "drafts")
}

// nowOr 保证时间源永远可用（零值回落 time.Now，与装配层同口径）。
// nowOr falls back to time.Now when no clock was injected.
func (p learningPort) nowOr() func() time.Time {
	if p.now == nil {
		return time.Now
	}
	return p.now
}

// logger 保证日志出口永远可用（与 agentsPort 同口径）。
// logger keeps the log outlet usable even when none was injected.
func (p learningPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

// 编译期断言：生产适配器始终满足命令面的学习端口；签名漂移在编译期先红。
// Compile-time assertion that the adapter still satisfies the command surface's port.
var _ cli.LearningPort = learningPort{}
