package app

// 本文件是分布式执行两个 CLI 端口的**生产适配器**（US-24 模块卡 §3.1.12）：
//
//	Workers —— `basalt workers list`：只读、不取实例锁、不写任何行
//	Assets  —— `basalt assets export|install|verify`：装配资产根与共享发布记录写入
//
// 三条纪律：
//  1. **查询只读**：Worker 列表走 `postgres.OpenReadOnly`（服务端固定只读事务），
//     服务运行中也能执行；心跳不刷新——诊断命令不得替任何人续命。
//  2. **install 先锁后库**：共享发布记录是**写**，实例锁在打开可写库之前取得
//     （与 learningPort / agentsPort 同序）；锁内构造 US-24 的 `bundlePublisher`，
//     发布协议只有这一份实现，本适配器不复制预写审计与指针 CAS。
//  3. **不开第二套运行期**：不装配 HTTP / MCP / 调度器 / 文件监视器；每次调用自建
//     句柄并在返回前关闭。
//
// The production adapters behind the two distributed CLI ports: the worker listing is
// read-only and lock-free, while install takes the instance lock before opening the
// writable database and reuses bundlePublisher wholesale. No second runtime is built.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// workersPort 是 `cli.WorkerLister` 的生产实现。`openReadOnly` 是测试注入点（默认
// `postgres.OpenReadOnly`）：用例据此证明列表路径没有打开可写会话。
// workersPort backs the read-only listing; the open hook defaults to the real opener.
type workersPort struct {
	Database     cli.DatabaseConfig
	openReadOnly func(context.Context, postgres.Config) (*postgres.DB, error)
}

// 编译期断言：两个适配器满足命令面声明的端口。
// Compile-time assertions that both adapters satisfy the consumer-side ports.
var (
	_ cli.WorkerLister = workersPort{}
	_ cli.AssetsPort   = assetsPort{}
)

// ListNodes 只读返回全部 Worker 实例的投影（含活跃租约数）。
// ListNodes returns every worker node with its active lease count.
func (p workersPort) ListNodes(ctx context.Context) ([]core.WorkerNodeView, error) {
	open := p.openReadOnly
	if open == nil {
		open = postgres.OpenReadOnly
	}
	db, err := open(ctx, postgresConfig(p.Database))
	if err != nil {
		return nil, err
	}
	nodes, listErr := postgres.NewWorkerStore(db).ListNodes(ctx)
	closeErr := db.Close()
	if listErr != nil {
		return nil, errors.Join(listErr, closeErr)
	}
	if closeErr != nil {
		return nodes, fmt.Errorf("workers list 关闭数据库失败: %w", closeErr)
	}
	return nodes, nil
}

// assetsPort 是 `cli.AssetsPort` 的生产实现。`Root` 已由装配层按 `assets.bundle_root`
// 解析成绝对路径；`open` / `now` 是测试注入点。
// assetsPort carries the resolved bundle root and the testable open/clock hooks.
type assetsPort struct {
	WorkDir                string
	SupportedSchemaVersion int
	Database               cli.DatabaseConfig
	Logger                 *slog.Logger

	// Root 是已经解析（绝对化）的内容寻址 bundle 根目录。
	// Root is the already-resolved content-addressed bundle root.
	Root string
	// open 默认为 postgres.Open（含前向迁移）；测试注入以证明锁的次序。
	// open defaults to postgres.Open; a test injects it to observe the lock order.
	open func(context.Context, postgres.Config) (*postgres.DB, error)
	// openReadOnly 默认为 postgres.OpenReadOnly：构建只读共享发布记录，不开可写库、
	// 不取实例锁（写仍归 Publish）。测试注入以证明构建没有打开可写会话。
	// openReadOnly defaults to postgres.OpenReadOnly; building never opens a writable
	// database, so the lock-before-write order of Publish is untouched.
	openReadOnly func(context.Context, postgres.Config) (*postgres.DB, error)
	now          func() time.Time
}

// AssetRoot 返回装配层解析后的绝对路径——命令面不再拼一次相对基准。
// AssetRoot returns the resolved absolute path; the command never re-bases it.
func (p assetsPort) AssetRoot() string { return p.Root }

// Build 从工作区把该 Agent 的当前定义构建成 bundle 到 `dest`（`install --agent` 的
// 前半段）。
//
// 构建只**读**数据库（生命周期身份 + 技能发布指针），因此走只读句柄、不取实例锁；
// 真正的写仍发生在随后的 `Publish`（锁 → 可写库 → 发布），"先锁后库"的次序不变。
// 构建失败不改动任何已安装目录——`assets.Build` 只写调用方给的暂存目录。
// Build reads through the read-only handle and takes no instance lock; the write still
// happens inside Publish, so the lock-before-writable-database order is unchanged.
func (p assetsPort) Build(ctx context.Context, agent, dest string) (assets.Manifest, error) {
	open := p.openReadOnly
	if open == nil {
		open = postgres.OpenReadOnly
	}
	db, err := open(ctx, postgresConfig(p.Database))
	if err != nil {
		return assets.Manifest{}, err
	}
	versions := postgres.NewResourceStore(db)
	uid, uidErr := resolveWorkspaceAgentUID(ctx, versions, agent)
	if uidErr != nil {
		return assets.Manifest{}, errors.Join(uidErr, db.Close())
	}
	manifest, buildErr := buildWorkspaceBundle(ctx,
		workspaceBundleDeps{Root: p.root(), Versions: versions}, agent, uid, dest)
	closeErr := db.Close()
	if buildErr != nil {
		return assets.Manifest{}, errors.Join(buildErr, closeErr)
	}
	if closeErr != nil {
		return assets.Manifest{}, fmt.Errorf("资产构建关闭数据库失败: %w", closeErr)
	}
	return manifest, nil
}

// Publish 在实例锁内打开可写库，用 US-24 的 `bundlePublisher` 写共享发布记录并推进
// 指针，返回本次发布的 `published_version_id`。
//
// 锁 → 开库（含前向迁移）→ 发布 → 关库 → 释放锁：与 learningPort 的 handoff 同序。
// 发布失败时既有的 pending 管理操作行由 `bundlePublisher` 自己回填 failed；本适配器
// 只负责句柄的生命周期，不重写发布协议。
// Publish takes the instance lock, opens the writable database and delegates to the
// single bundlePublisher implementation; the adapter only owns handle lifetimes.
func (p assetsPort) Publish(ctx context.Context, installed assets.Installed) (int64, error) {
	if p.SupportedSchemaVersion <= 0 {
		return 0, errors.New("资产安装缺少 schema 目标版本（装配错误）")
	}
	var versionID int64
	err := withInstanceLockErr(p.root(), func() error {
		open := p.open
		if open == nil {
			open = postgres.Open
		}
		db, err := open(ctx, postgresConfig(p.Database))
		if err != nil {
			return err
		}
		publisher := newBundlePublisher(postgres.NewResourceStore(db),
			postgres.NewManagementStore(db), p.logger(), p.nowOr())
		published, publishErr := publisher.Publish(ctx, installed)
		closeErr := db.Close()
		if publishErr != nil {
			return errors.Join(publishErr, closeErr)
		}
		if closeErr != nil {
			// 发布已提交：关库失败只留痕，不把一次成功发布报成失败（否则运维会
			// 重跑 install，虽然幂等，却会多写一条管理操作）。
			// The publication is committed; a close failure is logged, not raised.
			p.logger().Error("资产安装关闭数据库失败（发布已生效）",
				"workspace", p.root(), "error", closeErr)
		}
		versionID = published
		return nil
	})
	if err != nil {
		return 0, err
	}
	return versionID, nil
}

// root 是工作区根目录（`<WorkDir>/.basalt`），与 learningPort 同一常量来源。
// root is <WorkDir>/.basalt, shared with the learning port.
func (p assetsPort) root() string {
	return filepath.Join(p.WorkDir, workspaceDirName)
}

// logger 保证日志出口永远可用（与 agentsPort / learningPort 同口径）。
// logger keeps the log outlet usable even when none was injected.
func (p assetsPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

// nowOr 保证时间源永远可用（零值回落 time.Now）。
// nowOr falls back to time.Now when no clock was injected.
func (p assetsPort) nowOr() func() time.Time {
	if p.now == nil {
		return time.Now
	}
	return p.now
}
