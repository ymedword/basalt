package app

// 本文件是 `basalt db export / seal / import / verify / switch` 的**生产适配器**
// （cli.DatabaseMigration 的实现，端口定义在 internal/cli/ports.go）。它做三件命令面
// 做不到的事：拿到工作区与文件系统的真实句柄、打开 SQLite 源库、打开 PostgreSQL。
//
// 判据一律留在 `internal/dbmigrate`——三元组绑定、`pending` 包拒收、复核不通过不写，
// 这里只是**按顺序调用**并把结果投影成命令面能打印的普通数据。适配器不新增判据、
// 不"顺手"重排、不把失败翻译成别的码：多一条判据就多一处可能与合同分叉的地方。
// The production adapter behind the four migration subcommands. Every judgement lives in
// internal/dbmigrate; this file only sequences the calls and projects their results.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/dbmigrate"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// migrationPort 实现 cli.DatabaseMigration。无状态：每次调用自建短命连接并释放——
// 与 `postgresMigratorPort` 同一口径，迁移链路的句柄不会变成第二个运行期句柄。
// migrationPort is stateless: every call opens and releases its own handle.
type migrationPort struct {
	Logger *slog.Logger

	// Now 是可注入的时钟（切换时刻）；nil 时取 time.Now。显式传入与
	// `CutoverRequest.CutoverAt` 同一口径——测试要能钉住 cutover_id。
	// Now is the injectable clock behind the cutover instant.
	Now func() time.Time
}

// logger / clock 保证零值 migrationPort 也能工作（与其它适配器同法）。
func (p migrationPort) logger() *slog.Logger {
	if p.Logger == nil {
		return slog.Default()
	}
	return p.Logger
}

func (p migrationPort) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Export 导出 SQLite v6 工作区。
//
// 顺序是判据的一部分（图 9b 的 [1] → [5]）：
//
//	① 检查输出目录（必须不存在或为空——复用旧目录会让上一批残留混进新批次）
//	② **先**只读打开源库，再进静止窗口（打开会建 -wal / -shm，窗口内打开会让
//	   A1 与 A2 的源库文件身份不等；T070 实测）
//	③ 进窗口：锁 → 读名册（缺 → instance_identity_missing）→ A1 → 快照与逐表导出
//	   → A2 → 释放锁
//	④ 补三元组并绑定 → 铺 review/ 镜像 → 有待核查则只落 pending 清单；
//	   无需核查则直接封包
//	⑤ 最后关源库（A2 之后）
//
// Export keeps that order: the source handle opens before the quiet window and closes
// after it, because opening creates the WAL side files.
func (p migrationPort) Export(ctx context.Context, req cli.MigrationExportRequest) (cli.MigrationExportReport, error) {
	workspace := strings.TrimSpace(req.Workspace)
	out := strings.TrimSpace(req.Out)
	if workspace == "" {
		return cli.MigrationExportReport{}, errors.New("db export：必须给出源工作区目录（缺省为 <WorkDir>/.basalt）")
	}
	if out == "" {
		return cli.MigrationExportReport{}, errors.New("db export：必须给出迁移包输出目录（--out）")
	}
	if err := requireSeparateDirs(workspace, out); err != nil {
		return cli.MigrationExportReport{}, err
	}
	if err := requireEmptyOrAbsentDir(out); err != nil {
		return cli.MigrationExportReport{}, err
	}
	dbPath := filepath.Join(workspace, databaseFileName)
	info, err := os.Stat(dbPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cli.MigrationExportReport{}, fmt.Errorf(
			"db export：工作区 %s 里没有 SQLite 库（%s）——导出只服务建过库的存量工作区，本命令不建库", workspace, databaseFileName)
	case err != nil:
		return cli.MigrationExportReport{}, fmt.Errorf("db export：检查源库 %s: %w", dbPath, err)
	case info.IsDir():
		return cli.MigrationExportReport{}, fmt.Errorf("db export：%s 是目录，不是 SQLite 库文件", dbPath)
	}

	src, err := dbmigrate.OpenSourceReadOnly(ctx, dbPath)
	if err != nil {
		return cli.MigrationExportReport{}, err
	}
	defer func() {
		// 关闭失败不改判已经拿到的导出结果，但必须留痕：残留句柄会让下一次导出的
		// 源库文件身份与本次不同。
		if closeErr := src.Close(); closeErr != nil {
			p.logger().Warn("db export：释放源库只读句柄失败", "error", closeErr)
		}
	}()

	// 导出器认识的版本是**源谱系**（SQLite v6），不是 PostgreSQL 谱系版本（1）。两个常量
	// 一旦混用，真实 v6 工作区会被判成"不受支持"——这条判据只能由源谱系给。
	// The exporter's accepted version is the SQLite lineage's (6), never the PostgreSQL one.
	exporter := dbmigrate.NewExporter(src, dbmigrate.SourceSchemaVersion)
	var (
		marker   instanceMarker
		database dbmigrate.Manifest
	)
	assets, err := dbmigrate.BuildAssetManifestAround(ctx, workspace, dbmigrate.BuiltinAssetSpecs(),
		dbmigrate.AssetWindow{
			AfterLock: func(ctx context.Context) error {
				found, present, readErr := readInstanceMarkerAt(workspace)
				if readErr != nil {
					return readErr
				}
				if !present {
					return fmt.Errorf("%w（instance_identity_missing）：db export 需要源实例标识来绑定清单；"+
						"存量无名册实例必须先显式采纳身份——basalt instance adopt"+
						"（见颗粒度文档 C-47 / §3.1.8a），"+
						"不得当作新实例、不得自动补写名册", errInstanceIdentityMissing)
				}
				marker = found
				return nil
			},
			Between: func(ctx context.Context) error {
				exported, exportErr := exporter.Export(ctx, out)
				if exportErr != nil {
					return exportErr
				}
				database = exported
				return nil
			},
		})
	if err != nil {
		return cli.MigrationExportReport{}, err
	}

	// 三元组由本层补齐（dbmigrate 不猜实例身份），随即用 BindAssetManifest 判一次：
	// 两边逐字相等才算同一批次。
	//
	// 数据库清单**同样**要补：导出器只写它能证明的字段（批次号 / 快照摘要），实例标识
	// 留空等名册；补完必须**重写** `manifest.json`——后续 import / verify / switch 都从
	// 盘上的两份清单读三元组，只在内存里补会让 `readBoundPackage` 报 instance_mismatch。
	// The dump manifest gets the roster id too and is rewritten to disk: every later
	// command reads the triple off the files, not off the in-memory value.
	database.SourceInstanceID = marker.InstanceID
	assets.MigrationBatchID = database.MigrationBatchID
	assets.SourceInstanceID = database.SourceInstanceID
	assets.SourceInstanceIDOrigin = dbmigrate.AssetOriginRoster
	assets.SourceSnapshotSHA256 = database.SourceSnapshotSHA256
	if err := dbmigrate.BindAssetManifest(assets, database); err != nil {
		return cli.MigrationExportReport{}, err
	}
	if err := dbmigrate.WriteManifest(out, database); err != nil {
		return cli.MigrationExportReport{}, err
	}

	// review/ 镜像**必须**先铺：封装路径只从它取件，而镜像复制时会重算哈希——
	// 快照之后源端再变化就在这里暴露。
	if err := dbmigrate.MaterializeReview(ctx, workspace, out, assets); err != nil {
		return cli.MigrationExportReport{}, err
	}

	// 内容扫描只对未归类条目做启发式提示（C-49）：结果只进报告，**不**改变核查状态、
	// 处置决定或清单摘要，也**不**作为"包内无秘密"的证明。扫描失败按导出失败处理——
	// 少一项可见性比悄悄跳过更糟。
	// The content scan is a hint for unclassified entries only; it never changes the
	// review state or proves the package clean. A scan failure fails the export.
	contentHints, err := dbmigrate.ScanUnclassifiedHints(ctx, workspace, assets)
	if err != nil {
		return cli.MigrationExportReport{}, err
	}

	report := cli.MigrationExportReport{
		OutDir:                 out,
		MigrationBatchID:       assets.MigrationBatchID,
		SourceInstanceID:       assets.SourceInstanceID,
		SourceInstanceIDOrigin: assets.SourceInstanceIDOrigin,
		SourceSnapshotSHA256:   assets.SourceSnapshotSHA256,
		EntryCount:             len(assets.Entries),
		UnclassifiedCount:      countUnclassified(assets),
		UnclassifiedDigest:     assets.UnclassifiedDigest,
		ContentHints:           contentHints,
		Agents:                 assets.Agents(),
	}
	if assets.ReviewStatus == dbmigrate.AssetReviewPending {
		if err := dbmigrate.WriteAssetManifest(out, assets); err != nil {
			return report, err
		}
		report.ReviewStatus = assets.ReviewStatus
		report.Deliverable = false
		p.logger().Warn("db export：存在未归类文件，只产出受限核查包（不可交付）",
			"out", out, "unclassified", report.UnclassifiedCount)
		if len(contentHints) > 0 {
			// 只记规则 ID 与计数：日志同样不得出现命中内容或凭据原文。
			p.logger().Warn("db export：未归类文件内容扫描有启发式命中（仅提示，不证明安全）",
				"out", out, "hints", contentHints)
		}
		return report, nil
	}
	sealed, err := dbmigrate.SealAssetPackage(ctx, out, assets, dbmigrate.ReviewFile{})
	if err != nil {
		return report, err
	}
	report.ReviewStatus = sealed.ReviewStatus
	report.Deliverable = sealed.ReviewStatus != dbmigrate.AssetReviewPending
	p.logger().Info("db export 完成", "out", out, "batch", sealed.MigrationBatchID,
		"entries", len(sealed.Entries), "review_status", sealed.ReviewStatus)
	return report, nil
}

// Seal 用逐条处置把受限核查包封成可交付包。
//
// 有待核查条目却没有处置文件时**在调用前**拒绝：`SealAssetPackage` 会把它报成
// "摘要与清单不一致"，那句话会把运维指向错误的方向。
// Seal refuses a review-less seal before delegating, so the failure names the real cause.
func (p migrationPort) Seal(ctx context.Context, req cli.MigrationSealRequest) (cli.MigrationSealReport, error) {
	dir := strings.TrimSpace(req.Dir)
	if dir == "" {
		return cli.MigrationSealReport{}, errors.New("db export --seal：必须给出待封包的导出目录")
	}
	assets, err := dbmigrate.ReadAssetManifest(dir)
	if err != nil {
		return cli.MigrationSealReport{}, err
	}
	review, err := readReviewFile(strings.TrimSpace(req.ReviewFile), assets)
	if err != nil {
		return cli.MigrationSealReport{}, err
	}
	sealed, err := dbmigrate.SealAssetPackage(ctx, dir, assets, review)
	if err != nil {
		return cli.MigrationSealReport{}, err
	}
	report := cli.MigrationSealReport{
		Dir:          dir,
		ReviewStatus: sealed.ReviewStatus,
		ReviewDigest: sealed.ReviewDigest,
		Reviewer:     sealed.Reviewer,
	}
	for _, entry := range sealed.Entries {
		switch entry.Disposition {
		case dbmigrate.AssetDispositionIncluded:
			report.Included++
		case dbmigrate.AssetDispositionExcluded:
			report.Excluded++
		case dbmigrate.AssetDispositionHandoff:
			report.Handoff++
		}
	}
	p.logger().Info("db export --seal 完成", "dir", dir, "review_status", sealed.ReviewStatus,
		"included", report.Included, "excluded", report.Excluded, "handoff", report.Handoff)
	return report, nil
}

// Import 把迁移包导入目标库。先绑三元组、再拒收未复核包，最后才打开数据库——
// 打开数据库是唯一会碰外部后端的动作，前面任何一条不过都不该发生。
// Import binds and refuses before it opens the target: the opener is the only step that
// touches an external backend.
func (p migrationPort) Import(ctx context.Context, database cli.DatabaseConfig, req cli.MigrationImportRequest) (cli.MigrationImportReport, error) {
	dir := strings.TrimSpace(req.Dump)
	if dir == "" {
		return cli.MigrationImportReport{}, errors.New("db import：必须给出迁移包目录（--dump）")
	}
	manifest, assets, err := readBoundPackage(dir)
	if err != nil {
		return cli.MigrationImportReport{}, err
	}
	if err := requireReviewedPackage(assets); err != nil {
		return cli.MigrationImportReport{}, err
	}
	db, err := postgres.Open(ctx, postgresConfig(database))
	if err != nil {
		return cli.MigrationImportReport{}, err
	}
	defer func() { p.closeTarget(db, "db import") }()

	imported, err := dbmigrate.NewImporter(db, dir, manifest).Import(ctx)
	report := projectImportReport(imported)
	if err != nil {
		return report, err
	}
	p.logger().Info("db import 完成", "dump", dir, "batch", manifest.MigrationBatchID,
		"tables", report.Tables != nil, "rows", report.TotalRows)
	return report, nil
}

// VerifyDump 离线复核迁移包：先拒收未复核包，再逐项比 dump 与包内资产。
//
// 两段各出一份差异，合并进同一份报告——"缺了哪个文件"与"哪张表摘要不符"是两类事实，
// 不能压成一句话。
// VerifyDump reports the dump-side and the package-side differences separately.
func (p migrationPort) VerifyDump(ctx context.Context, req cli.MigrationVerifyRequest) (cli.MigrationVerifyReport, error) {
	dir := strings.TrimSpace(req.Dump)
	if dir == "" {
		return cli.MigrationVerifyReport{}, errors.New("db verify：必须给出迁移包目录（--dump）")
	}
	manifest, assets, err := readBoundPackage(dir)
	if err != nil {
		return cli.MigrationVerifyReport{}, err
	}
	if err := requireReviewedPackage(assets); err != nil {
		return cli.MigrationVerifyReport{}, err
	}
	report := cli.MigrationVerifyReport{Mode: "dump"}
	dumpReport, dumpErr := dbmigrate.VerifyDump(ctx, dir, manifest)
	report = projectVerifyReport(report, dumpReport)
	assetReport, assetErr := dbmigrate.VerifyPackage(ctx, dir, assets)
	problems := assetProblems(assetReport)
	report.OK = dumpErr == nil && len(problems) == 0
	report.Duration = dumpReport.Duration
	if dumpErr != nil {
		return report, dumpErr
	}
	if assetErr != nil {
		return report, fmt.Errorf("db verify --dump：复核包内资产失败: %w", assetErr)
	}
	if len(problems) > 0 {
		return report, fmt.Errorf("db verify --dump：包内资产与清单不符（packaged_digest_mismatch）：\n  %s",
			strings.Join(problems, "\n  "))
	}
	p.logger().Info("db verify --dump 通过", "dump", dir, "tables", len(report.Tables))
	return report, nil
}

// VerifyTarget 复核目标库：**只读**打开（`postgres.OpenReadOnly`），会话固定为只读
// 事务——一条复核命令不应该顺手迁移或修补结构。
// VerifyTarget opens read-only; a verification command never migrates.
func (p migrationPort) VerifyTarget(ctx context.Context, database cli.DatabaseConfig, req cli.MigrationVerifyRequest) (cli.MigrationVerifyReport, error) {
	dir := strings.TrimSpace(req.Dump)
	if dir == "" {
		return cli.MigrationVerifyReport{}, errors.New("db verify --target：必须同时给出迁移包目录（--dump）——目标端比对以清单为唯一基准")
	}
	manifest, assets, err := readBoundPackage(dir)
	if err != nil {
		return cli.MigrationVerifyReport{}, err
	}
	if err := requireReviewedPackage(assets); err != nil {
		return cli.MigrationVerifyReport{}, err
	}
	db, err := postgres.OpenReadOnly(ctx, postgresConfig(database))
	if err != nil {
		return cli.MigrationVerifyReport{}, err
	}
	defer func() { p.closeTarget(db, "db verify --target") }()

	targetReport, err := dbmigrate.VerifyTarget(ctx, db, manifest)
	report := projectVerifyReport(cli.MigrationVerifyReport{Mode: "target"}, targetReport)
	if err != nil {
		return report, err
	}
	p.logger().Info("db verify --target 通过", "dump", dir, "tables", len(report.Tables))
	return report, nil
}

// Switch 在机器可判的三道门禁都过后写入 cutover 行。
//
// 门禁**重新跑一遍**而不是相信"刚才 verify 过了"：上一次复核与这一次切换之间的任何
// 变化（换了一份包、目标库被写、源端被改）都只有重跑才看得见。三道门禁是：
// ① 包内资产与清单逐字节一致；② dump 的逐表摘要与清单一致；③ 目标库与清单一致；
// ④ 源端自快照以来未漂移（FR-092 要求「切换前**必须**重盘」：`--workspace` 为必填，
// 缺失即拒绝，不存在"跳过并留 WARN"的路径）。
//
// handoff 门禁**不在本方法内**（由 `db verify --handoff` 承担，T142）：实现侧不跑、
// 也不声称跑过——命令面必须把它作为"未核对"如实打印。
// Switch re-runs every machine-checkable gate instead of trusting an earlier run. The
// handoff gate belongs to db verify --handoff (T142) and is never claimed here.
func (p migrationPort) Switch(ctx context.Context, database cli.DatabaseConfig, req cli.MigrationSwitchRequest) (cli.MigrationSwitchReport, error) {
	dir := strings.TrimSpace(req.Dump)
	if dir == "" {
		return cli.MigrationSwitchReport{}, errors.New("db switch：必须给出迁移包目录（--dump）")
	}
	workspace := strings.TrimSpace(req.Workspace)
	// FR-092 / C-46：切换前**必须**重盘 A_cut 与源库身份（尺寸 + SHA-256）。源工作区
	// 因此不是可选项——缺省跳过会让 source_changed_since_snapshot 判据 fail-open，
	// 停机没停干净的旧实例在切换后丢写入也无人发现。拒绝发生在读包与连库之前。
	// Missing --workspace is a refusal: skipping the source re-disk would leave
	// source_changed_since_snapshot fail-open.
	if workspace == "" {
		return cli.MigrationSwitchReport{}, errors.New(
			"db switch：必须给出源工作区（--workspace）——切换前必须重盘 A_cut 与源库身份（尺寸 + SHA-256，FR-092）；缺省跳过会让 source_changed_since_snapshot 判据 fail-open")
	}
	manifest, assets, err := readBoundPackage(dir)
	if err != nil {
		return cli.MigrationSwitchReport{}, err
	}
	if err := requireReviewedPackage(assets); err != nil {
		return cli.MigrationSwitchReport{}, err
	}
	drift, driftErr := dbmigrate.VerifyAssets(ctx, workspace, assets)
	if driftErr != nil {
		return cli.MigrationSwitchReport{}, driftErr
	}
	if problems := assetProblems(drift); len(problems) > 0 {
		return cli.MigrationSwitchReport{}, fmt.Errorf(
			"db switch：源工作区自快照以来已变化（source_changed_since_snapshot）：\n  %s",
			strings.Join(problems, "\n  "))
	}
	packaged, err := dbmigrate.VerifyPackage(ctx, dir, assets)
	if err != nil {
		return cli.MigrationSwitchReport{}, fmt.Errorf("db switch：复核包内资产失败: %w", err)
	}
	if problems := assetProblems(packaged); len(problems) > 0 {
		return cli.MigrationSwitchReport{}, fmt.Errorf(
			"db switch：包内资产与清单不符（packaged_digest_mismatch）：\n  %s", strings.Join(problems, "\n  "))
	}
	if _, err := dbmigrate.VerifyDump(ctx, dir, manifest); err != nil {
		return cli.MigrationSwitchReport{}, err
	}

	db, err := postgres.Open(ctx, postgresConfig(database))
	if err != nil {
		return cli.MigrationSwitchReport{}, err
	}
	defer func() { p.closeTarget(db, "db switch") }()
	if _, err := dbmigrate.VerifyTarget(ctx, db, manifest); err != nil {
		return cli.MigrationSwitchReport{}, err
	}
	// 导入前的实测基线：目标复核刚确认清单内的表与库一致，这里再点名读到导入前的
	// 确切行数——不靠清单推导，也不依赖清单里有没有这张表。
	baselineRows, err := dbmigrate.CountLegacyMemoryRows(ctx, db)
	if err != nil {
		return cli.MigrationSwitchReport{}, err
	}
	// 旧记忆一次性只读导入：必须落在 VerifyTarget 之后、WriteCutover 之前（判据见
	// migration_memory.go 的文件头）。导入不完整即返回错误——storage_cutover 不落行，
	// 实例停在切换前。
	legacyMemory, err := p.importLegacyMemory(ctx, db, dir)
	if err != nil {
		return cli.MigrationSwitchReport{}, err
	}
	// 导入完成后的行数对账（S-F04）：导入前实测 + 本次导入条数 = 导入后实测。不等即
	// 拒绝写入切换记录——多出的行与少掉的行都不允许被"导入后基线"吸收。这份证据同时
	// 是回退判据该取的 memory_entries 基线。
	memoryBaseline, err := dbmigrate.CheckLegacyMemoryImport(ctx, db, baselineRows, int64(len(legacyMemory.Imported)))
	if err != nil {
		return cli.MigrationSwitchReport{}, err
	}

	record, err := dbmigrate.WriteCutover(ctx, db, dbmigrate.CutoverRequest{
		ManifestDir: dir,
		Operator:    cliWriteActor(p.logger()).Subject,
		CutoverAt:   p.clock(),
	})
	if err != nil {
		return cli.MigrationSwitchReport{}, err
	}
	p.logger().Info("db switch 完成", "dump", dir, "cutover_id", record.CutoverID,
		"manifest_sha256", record.ManifestSHA256, "source_schema_version", record.SourceSchemaVersion,
		"legacy_memory_imported", len(legacyMemory.Imported), "legacy_memory_skipped", len(legacyMemory.Skipped),
		"legacy_memory_total", legacyMemory.Total(),
		"memory_entries_before", memoryBaseline.BaselineRows,
		"memory_entries_imported", memoryBaseline.Imported,
		"memory_entries_observed", memoryBaseline.ObservedRows)
	return cli.MigrationSwitchReport{
		CutoverID:           record.CutoverID,
		CutoverAt:           time.UnixMilli(record.CutoverAt).UTC(),
		ManifestSHA256:      record.ManifestSHA256,
		SourceSchemaVersion: record.SourceSchemaVersion,
		RollbackWindowOpen:  record.RollbackWindowOpen(),
		Memory:              projectMemoryImport(legacyMemory),
		MemoryBaseline:      projectMemoryBaseline(memoryBaseline),
	}, nil
}

// closeTarget 释放目标库句柄；失败只留痕不改判（已有的业务结论更有价值）。
func (p migrationPort) closeTarget(db *postgres.DB, command string) {
	if db == nil {
		return
	}
	if err := db.Close(); err != nil {
		p.logger().Warn(command+"：释放 PostgreSQL 连接池失败", "error", err)
	}
}

// requireSeparateDirs 拒绝"包目录与工作区互相包含"的两种形态。
//
// 把包写进工作区会被下一次盘点当成资产（或者被排除项吃掉、静默消失）；把工作区写进
// 包目录则让"包"里多出一整棵源树。两种都在**写入之前**拒绝，不靠事后清理。
// requireSeparateDirs refuses overlapping layout before anything is written.
func requireSeparateDirs(workspace, out string) error {
	wsAbs, err := filepath.Abs(workspace)
	if err != nil {
		return fmt.Errorf("db export：解析工作区路径 %s: %w", workspace, err)
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("db export：解析输出目录 %s: %w", out, err)
	}
	if containsPath(wsAbs, outAbs) || containsPath(outAbs, wsAbs) {
		return fmt.Errorf("db export：输出目录 %s 与工作区 %s 互相包含——迁移包必须落在工作区之外", outAbs, wsAbs)
	}
	return nil
}

// containsPath 判 child 是否等于 parent 或位于其下（`filepath.Rel` 的 `..` 前缀判定）。
func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// requireEmptyOrAbsentDir 要求导出目录不存在或为空。
//
// 复用旧目录会把上一批的 `*.jsonl` / `manifest.json` 留在原地：清单只描述本次导出的表，
// 复核按清单逐表读文件，多出来的旧文件**没有任何一步会看它**——于是"复核通过"与
// "目录里躺着上一批的数据"可以同时成立。写入之前拒绝，是唯一不依赖事后清理的做法。
// requireEmptyOrAbsentDir refuses a reused directory: a leftover dump would be invisible
// to every downstream check, so "verified" and "stale data present" could coexist.
func requireEmptyOrAbsentDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("db export：检查输出目录 %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("db export：输出目录 %s 非空（%d 项）——迁移包必须导进新目录或空目录，"+
			"复用旧目录会让上一批的残留文件混进本批次", dir, len(entries))
	}
	return nil
}

// readBoundPackage 读两份清单并校验三元组绑定。
func readBoundPackage(dir string) (dbmigrate.Manifest, dbmigrate.AssetManifest, error) {
	manifest, err := dbmigrate.ReadManifest(dir)
	if err != nil {
		return dbmigrate.Manifest{}, dbmigrate.AssetManifest{}, err
	}
	assets, err := dbmigrate.ReadAssetManifest(dir)
	if err != nil {
		return dbmigrate.Manifest{}, dbmigrate.AssetManifest{}, err
	}
	if err := dbmigrate.BindAssetManifest(assets, manifest); err != nil {
		return dbmigrate.Manifest{}, dbmigrate.AssetManifest{}, err
	}
	return manifest, assets, nil
}

// requireReviewedPackage 拒收未封包的迁移包：`pending` 意味着源工作区里还有操作者
// 没处置过的未归类文件，把它导进目标库等于替操作者做了那个决定。
// requireReviewedPackage refuses a pending package; importing it would make the review
// decision on the operator's behalf.
func requireReviewedPackage(assets dbmigrate.AssetManifest) error {
	switch assets.ReviewStatus {
	case dbmigrate.AssetReviewNotRequired, dbmigrate.AssetReviewSealed:
		return nil
	case dbmigrate.AssetReviewPending:
		return fmt.Errorf("迁移包尚未复核（review_status=%s）：先按 review/ 逐条处置未归类文件，"+
			"再执行 db export --seal --review-file 封包", assets.ReviewStatus)
	default:
		return fmt.Errorf("迁移包的 review_status=%q 不可识别——本工具只接受 %s / %s / %s",
			assets.ReviewStatus, dbmigrate.AssetReviewNotRequired, dbmigrate.AssetReviewPending,
			dbmigrate.AssetReviewSealed)
	}
}

// readReviewFile 读逐条处置文件；没有未归类条目时不接受任何处置文件。
//
// 读经目录句柄（`os.OpenRoot`）：路径由调用方给定，根限定让读取不可能落在所在目录之外。
// readReviewFile opens the decisions file through a root handle and refuses a review file
// when there is nothing to review.
func readReviewFile(file string, assets dbmigrate.AssetManifest) (dbmigrate.ReviewFile, error) {
	if strings.TrimSpace(file) == "" {
		if assets.ReviewStatus == dbmigrate.AssetReviewPending {
			return dbmigrate.ReviewFile{}, fmt.Errorf(
				"db export --seal：清单有 %d 个未归类文件待核查，必须用 --review-file 给出逐条处置"+
					"（include / exclude / handoff，后两者必填原因）", countUnclassified(assets))
		}
		return dbmigrate.ReviewFile{}, nil
	}
	root, err := os.OpenRoot(filepath.Dir(file))
	if err != nil {
		return dbmigrate.ReviewFile{}, fmt.Errorf("db export --seal：打开处置文件所在目录: %w", err)
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(filepath.Base(file))
	if err != nil {
		return dbmigrate.ReviewFile{}, fmt.Errorf("db export --seal：读处置文件 %s: %w", file, err)
	}
	var review dbmigrate.ReviewFile
	if err := json.Unmarshal(raw, &review); err != nil {
		return dbmigrate.ReviewFile{}, fmt.Errorf("db export --seal：处置文件 %s 不是合法 JSON: %w", file, err)
	}
	return review, nil
}

// countUnclassified 数未归类条目。
func countUnclassified(assets dbmigrate.AssetManifest) int {
	count := 0
	for _, entry := range assets.Entries {
		if entry.Class == dbmigrate.AssetClassUnclassified {
			count++
		}
	}
	return count
}

// projectImportReport 把导入报告投影成命令面结构：表名升序，命令面保序打印。
func projectImportReport(imported dbmigrate.ImportReport) cli.MigrationImportReport {
	tables := make([]string, 0, len(imported.Rows))
	for name := range imported.Rows {
		tables = append(tables, name)
	}
	sort.Strings(tables)
	report := cli.MigrationImportReport{
		Tables:    make([]cli.MigrationTableRows, 0, len(tables)),
		TotalRows: imported.TotalRows(),
		Duration:  imported.Duration,
	}
	for _, name := range tables {
		report.Tables = append(report.Tables, cli.MigrationTableRows{Table: name, Rows: imported.Rows[name]})
	}
	return report
}

// projectVerifyReport 把复核报告投影成命令面结构（表名升序，差异项逐条展开）。
func projectVerifyReport(report cli.MigrationVerifyReport, verified dbmigrate.VerifyReport) cli.MigrationVerifyReport {
	tables := make([]string, 0, len(verified.SourceRows))
	for name := range verified.SourceRows {
		tables = append(tables, name)
	}
	sort.Strings(tables)
	report.Tables = make([]cli.MigrationTableCheck, 0, len(tables))
	for _, name := range tables {
		report.Tables = append(report.Tables, cli.MigrationTableCheck{
			Table:       name,
			SourceRows:  verified.SourceRows[name],
			TargetRows:  verified.TargetRows[name],
			RowDigestOK: verified.RowDigestOK[name],
		})
	}
	report.MissingIDs = flattenIDDiffs(verified.MissingIDs, "缺")
	report.ExtraIDs = flattenIDDiffs(verified.ExtraIDs, "多")
	report.ReferenceFailures = flattenMessages(verified.ReferenceFailures)
	report.Notes = flattenNotes(verified.Notes)
	report.OK = verified.OK
	report.Duration = verified.Duration
	return report
}

// flattenIDDiffs 把逐表的主键差异展开成一行一条（表名在前，便于对号）。
func flattenIDDiffs(diffs map[string][]string, label string) []string {
	if len(diffs) == 0 {
		return nil
	}
	tables := make([]string, 0, len(diffs))
	for name := range diffs {
		tables = append(tables, name)
	}
	sort.Strings(tables)
	var lines []string
	for _, name := range tables {
		ids := append([]string(nil), diffs[name]...)
		sort.Strings(ids)
		lines = append(lines, fmt.Sprintf("%s %s: %s", name, label, strings.Join(ids, ", ")))
	}
	return lines
}

// flattenMessages 展开 `map[表][]原因` 形态的差异（键排序后逐条列出）。
func flattenMessages(diffs map[string][]string) []string {
	if len(diffs) == 0 {
		return nil
	}
	tables := make([]string, 0, len(diffs))
	for name := range diffs {
		tables = append(tables, name)
	}
	sort.Strings(tables)
	var lines []string
	for _, name := range tables {
		for _, message := range diffs[name] {
			lines = append(lines, name+": "+message)
		}
	}
	return lines
}

// flattenNotes 展开 `map[表]说明`（键排序）。
func flattenNotes(notes map[string]string) []string {
	if len(notes) == 0 {
		return nil
	}
	tables := make([]string, 0, len(notes))
	for name := range notes {
		tables = append(tables, name)
	}
	sort.Strings(tables)
	lines := make([]string, 0, len(tables))
	for _, name := range tables {
		lines = append(lines, name+": "+notes[name])
	}
	return lines
}

// assetProblems 把资产复核的差异表展开成一行一条：每一类差异分开说，
// 缺项点名"缺了它哪个 Agent 不完整"（`MissingFor` 按 base 名索引）。
func assetProblems(report dbmigrate.AssetReport) []string {
	var problems []string
	for _, missing := range report.Missing {
		line := "缺项 " + missing
		if agents := report.MissingFor[path.Base(missing)]; len(agents) > 0 {
			line += "（缺了它 " + strings.Join(agents, "、") + " 不完整）"
		}
		problems = append(problems, line)
	}
	for _, mismatch := range report.Mismatched {
		problems = append(problems, "尺寸或摘要不符 "+mismatch)
	}
	for _, extra := range report.Unexpected {
		problems = append(problems, "清单外新增 "+extra)
	}
	for _, drift := range report.PermissionDrift {
		problems = append(problems, "权限漂移 "+drift)
	}
	for _, drift := range report.DBFileDrift {
		problems = append(problems, "源库文件漂移 "+drift)
	}
	return problems
}
