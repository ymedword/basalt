package app

// 本文件是 US-25C §2.3.3 的**存量载荷回填**：把 US-25 及以前发布的指针补到
// C15 的口径上（指针存在 ⇒ 载荷存在且 `payload_sha256` 自洽）。它只在控制面启动期
// 执行，幂等、可重复，逐条处理互不阻塞。
//
// 回填只接受**目录自洽**的来源，次序固定（§2.3.3）：
//
//	① 现行安装目录 `assets.Resolve`（内容寻址，目录名与清单必须指向同一份内容）；
//	② 版本行 `storage_path` 的**同机候选**（basename 必须等于哈希，且位于资产根之内）；
//	③ 按共享发布记录重建（本地代次 == 索引代次，且重建结果的 BundleHash 与记录逐字相等）。
//
// 三条都不成立 → `payload_missing`；目录在但不可复算 → `corrupt`。两种结局都**不伪造**
// 载荷、**不**写空字节、**不**移动指针，也**不**因为回填发布新版本。编码**之前**先
// `assets.Verify`：哈希不符的目录绝不产出载荷。
//
// This file backfills payloads for pointers published before US-25C. It only accepts a
// self-consistent directory (resolved, same-machine candidate, or rebuilt from the
// shared record), verifies before encoding, and never publishes, moves a pointer, or
// fabricates payloads.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// backfillPageSize 是全量翻页的页大小：回填走 `resource_id` 序的整趟扫描，页大小只
// 影响单次查询的内存面，不改变覆盖范围。
// backfillPageSize bounds one page of the resource_id-ordered full scan.
const backfillPageSize = 200

// 回填逐条的结局（报告与启动日志共用）。`payload_missing` / `corrupt` 是模块卡
// §2.3.3 的固定字面量，`assets status` 用同一组词标注缺口。
// Per-pointer outcomes; payload_missing / corrupt are the literals fixed by the card.
const (
	backfillStatusExists         = "exists"
	backfillStatusFilled         = "filled"
	backfillStatusPayloadMissing = "payload_missing"
	backfillStatusCorrupt        = "corrupt"
	backfillStatusFailed         = "failed"
)

// BackfillItem 是一条已发布指针的回填结局：`Detail` 只在非成功结局上给出可检索的原因。
// BackfillItem is one pointer's outcome; Detail carries the reason on non-success.
type BackfillItem struct {
	Agent      string
	BundleHash string
	Status     string
	Detail     string
}

// BackfillReport 是一趟回填的读数：启动日志逐类汇总，测试按条目断言。
// BackfillReport is one pass's reading: per-item outcomes for tests and the log summary.
type BackfillReport struct {
	Items []BackfillItem
}

// Count 返回某个结局的条数。
// Count reports how many pointers ended in the given status.
func (r BackfillReport) Count(status string) int {
	count := 0
	for _, item := range r.Items {
		if item.Status == status {
			count++
		}
	}
	return count
}

// BackfillDeps 是存量载荷回填的全部装配输入（control 启动期）。
//
// `Versions` / `Lifecycle` / `WorkspaceRoot` 是"按共享记录重建"的来源；任一为空即
// 不重建——只走现行目录与同机候选。`AssetRoot` 同时是候选的包含判据与重建的安装根。
// BackfillDeps carries the assembly inputs; Versions / Lifecycle / WorkspaceRoot enable
// the rebuild step and any of them being empty simply disables it.
type BackfillDeps struct {
	Distribution  core.AssetDistributionPort
	PayloadStore  core.BundlePayloadStore
	Versions      assetBootstrapRecords
	Lifecycle     *workspace.LifecycleStore
	WorkspaceRoot string
	AssetRoot     string
	MaxBytes      int64
	Logger        *slog.Logger
	PageSize      int
}

// BackfillBundlePayloads 为存量已发布指针补写载荷（§2.3.3）。幂等：载荷已存在且自洽
// 的条目直接跳过，重复启动不重复写。
//
// 单条失败不影响其他条目；整趟列出失败才返回错误（调用方按"不阻塞启动"处置，
// 缺口在 `assets status` 里仍可见）。
// BackfillBundlePayloads is idempotent per pointer; only a failure to list the pointers
// aborts the pass, and every item outcome is reported either way.
func BackfillBundlePayloads(ctx context.Context, deps BackfillDeps) (BackfillReport, error) {
	var report BackfillReport
	if deps.Distribution == nil || deps.PayloadStore == nil || deps.AssetRoot == "" || deps.MaxBytes <= 0 {
		return report, errors.New("存量载荷回填未接线：缺分发读面、载荷写面、资产根或载荷上限（不得只装本地）")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	pageSize := deps.PageSize
	if pageSize <= 0 {
		pageSize = backfillPageSize
	}

	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		page, err := deps.Distribution.ListPublishedBundlesPage(ctx, after, pageSize)
		if err != nil {
			return report, fmt.Errorf("列出已发布 bundle 指针: %w", err)
		}
		if len(page) == 0 {
			break
		}
		for _, published := range page {
			report.Items = append(report.Items, backfillOnePointer(ctx, deps, logger, published))
		}
		after = string(page[len(page)-1].AgentUID)
	}

	logger.Info("bundle payload backfill finished",
		"pointers", len(report.Items),
		"filled", report.Count(backfillStatusFilled),
		"existing", report.Count(backfillStatusExists),
		"payload_missing", report.Count(backfillStatusPayloadMissing),
		"corrupt", report.Count(backfillStatusCorrupt),
		"failed", report.Count(backfillStatusFailed))
	return report, nil
}

// backfillOnePointer 处理一条指针：载荷已在且自洽即跳过；否则先定位目录，再**先验后编**。
// backfillOnePointer proves an existing payload first and otherwise verifies the
// directory before encoding it.
func backfillOnePointer(ctx context.Context, deps BackfillDeps, logger *slog.Logger,
	published core.PublishedBundle,
) BackfillItem {
	item := BackfillItem{Agent: published.Agent, BundleHash: published.BundleHash}
	rec, ok, err := deps.Distribution.ReadBundlePayload(ctx, published.BundleHash)
	if err != nil {
		item.Status = backfillStatusFailed
		item.Detail = err.Error()
		logger.Warn("bundle payload backfill cannot read the payload row",
			"agent", published.Agent, "bundle", published.BundleHash, "error", err)
		return item
	}
	if ok && payloadSelfConsistent(rec, published.BundleHash) {
		item.Status = backfillStatusExists
		return item
	}

	dir, status, detail := resolveBackfillSource(ctx, deps, logger, published)
	if dir == "" {
		item.Status = status
		item.Detail = detail
		logger.Warn("bundle payload backfill skipped: no self-consistent bundle directory",
			"agent", published.Agent, "bundle", published.BundleHash, "status", status, "reason", detail)
		return item
	}

	// 编码**之前**先验目录：清单哈希不符或逐文件不符就不产出载荷（§2.3.3）。
	manifest, err := assets.Verify(ctx, dir)
	if err == nil && manifest.BundleHash != published.BundleHash {
		err = fmt.Errorf("%w: 目录 %s 的清单哈希 %s 与发布指针 %s 不符",
			core.ErrAssetVersionMismatch, dir, manifest.BundleHash, published.BundleHash)
	}
	if err != nil {
		item.Status = backfillStatusCorrupt
		item.Detail = err.Error()
		logger.Warn("bundle payload backfill skipped: bundle directory failed verification",
			"agent", published.Agent, "bundle", published.BundleHash, "dir", dir, "error", err)
		return item
	}
	if err := writePayload(ctx, dir, manifest, deps.PayloadStore, deps.MaxBytes); err != nil {
		item.Status = backfillStatusFailed
		item.Detail = err.Error()
		logger.Warn("bundle payload backfill failed to store the payload",
			"agent", published.Agent, "bundle", published.BundleHash, "dir", dir, "error", err)
		return item
	}
	item.Status = backfillStatusFilled
	logger.Info("bundle payload backfilled",
		"agent", published.Agent, "bundle", published.BundleHash, "dir", dir)
	return item
}

// resolveBackfillSource 按 §2.3.3 的固定次序找一份目录源：现行安装目录 → 版本行的
// 同机候选 → 按共享发布记录重建。返回空目录时 status 说明是 `payload_missing` 还是
// `corrupt`（目录在但身份 / 内容与指针不符时**不覆盖、不重建**，资产不可变）。
// resolveBackfillSource applies the fixed candidate order; a directory that exists but
// disagrees with the pointer is corrupt and is never overwritten or rebuilt.
func resolveBackfillSource(ctx context.Context, deps BackfillDeps, logger *slog.Logger,
	published core.PublishedBundle,
) (dir, status, detail string) {
	dir, err := assets.Resolve(ctx, deps.AssetRoot, published.Agent, published.BundleHash)
	if err == nil {
		return dir, "", ""
	}
	if !errors.Is(err, core.ErrAssetUnavailable) {
		return "", backfillStatusCorrupt, err.Error()
	}
	if candidate := sameMachineBundleDir(ctx, deps, published); candidate != "" {
		return candidate, "", ""
	}
	rebuilt, rebuildErr := rebuildBundleDirForBackfill(ctx, deps, logger, published)
	if rebuildErr != nil {
		return "", backfillStatusPayloadMissing, rebuildErr.Error()
	}
	if rebuilt != "" {
		return rebuilt, "", ""
	}
	return "", backfillStatusPayloadMissing, err.Error()
}

// sameMachineBundleDir 取版本行 `storage_path` 的同机候选：它可能是发布主机的绝对
// 路径，因此**必须**同时满足 basename == 哈希、位于资产根之内、且目录真实存在才采用；
// 任何一条不成立都当作"仍缺失"，不把错误路径送进编码面。
// sameMachineBundleDir adopts the version row's storage_path only when it is an existing
// hash-named directory inside the asset root.
func sameMachineBundleDir(ctx context.Context, deps BackfillDeps, published core.PublishedBundle) string {
	if deps.Versions == nil {
		return ""
	}
	pointer, err := deps.Versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(published.AgentUID))
	if err != nil || pointer.DeletedAt != nil {
		return ""
	}
	version, err := deps.Versions.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil || version.Kind != core.ResourceAgentBundle || version.ContentHash != published.BundleHash {
		return ""
	}
	candidate := strings.TrimSpace(version.StoragePath)
	if candidate == "" || filepath.Base(filepath.Clean(candidate)) != published.BundleHash {
		return ""
	}
	if !bundlePathInside(deps.AssetRoot, candidate) {
		return ""
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return ""
	}
	return candidate
}

// rebuildBundleDirForBackfill 按共享发布记录重建缺失的本地目录，判据与
// `bootstrapAgentBundles` 同款：本地生命周期代次 == 共享索引代次、身份一致、指针仍
// 指向该哈希；重建结果的 BundleHash 由 `buildBundleFromRecord` 逐字核对。
// It rebuilds a missing directory under the same gates as the startup bootstrap.
func rebuildBundleDirForBackfill(ctx context.Context, deps BackfillDeps, logger *slog.Logger,
	published core.PublishedBundle,
) (string, error) {
	if deps.Versions == nil || deps.Lifecycle == nil || deps.WorkspaceRoot == "" {
		return "", nil
	}
	record, exists, err := deps.Lifecycle.Read(ctx, published.Agent)
	if err != nil || !exists {
		return "", nil
	}
	if core.AgentUID(record.AgentUID) != published.AgentUID {
		return "", nil
	}
	index, ok, _, err := deps.Versions.ReadLifecycleIndex(ctx, published.Agent)
	if err != nil {
		return "", err
	}
	if !ok || index.Generation != record.Generation {
		// 代次不等（任一方向）都不重建：低了不准入，高了走管理操作单调修复。
		return "", nil
	}
	pointer, err := deps.Versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(published.AgentUID))
	if err != nil || pointer.DeletedAt != nil {
		return "", nil
	}
	version, err := deps.Versions.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil || version.Kind != core.ResourceAgentBundle || version.ContentHash != published.BundleHash {
		return "", nil
	}

	staging, err := os.MkdirTemp("", "basalt-payload-backfill-")
	if err != nil {
		return "", fmt.Errorf("创建重建暂存目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }() // 构建结果由 Install 复制，暂存目录用毕即删
	manifest, err := buildBundleFromRecord(ctx, assetBootstrapDeps{
		Root:      deps.WorkspaceRoot,
		AssetRoot: deps.AssetRoot,
		Versions:  deps.Versions,
		Lifecycle: deps.Lifecycle,
	}, published.Agent, published.AgentUID, published.BundleHash, version, staging)
	if err != nil {
		return "", err
	}
	installed, err := assets.Install(ctx, staging, deps.AssetRoot)
	if err != nil {
		return "", fmt.Errorf("安装重建的 Agent %s 资产 bundle: %w", published.Agent, err)
	}
	logger.Warn("bundle payload backfill rebuilt a missing bundle directory from the published record",
		"agent", published.Agent, "bundle", manifest.BundleHash, "dir", installed.Dir)
	return installed.Dir, nil
}

// bundlePathInside 判定 target 是否落在 root 之内（含根本身）；不同卷一律不算。
// bundlePathInside reports whether target lies inside root (root included).
func bundlePathInside(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	rel = filepath.Clean(rel)
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}
