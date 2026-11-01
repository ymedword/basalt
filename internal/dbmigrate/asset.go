package dbmigrate

// 本文件是**工作区资产清单**（C-44 / C-46 / C-49 / C-50，颗粒度文档 §2.11.5 / §2.11.6 /
// §3.1.2）：白名单收集、静止窗口内前后两次盘点、未归类文件的两段式核查、打包产物复核与
// 三元组绑定。四条纪律：
//  1. `RelPath` 基准是**工作区根** `<WorkDir>/.basalt`，只登记普通文件；符号链接、联接点、
//     硬链接与越界路径一律拒绝，**不跟随**、**不猜**。
//  2. 排除的是**具体条目**（库与旁路、锁、名册、密钥、`.env*`、`*.clixml`、`logs/`、
//     `.git/`），**不得**整目录排除工作区——那会把 Agent 与业务状态一起丢掉。
//  3. 同一静止窗口内盘点两次（A1 / A2），条目集合与逐项字段全等才产出清单；两次只证明
//     "两个时刻一致"，**不证明**窗口内"改后又改回"，也**不覆盖**锁外的写者。
//  4. 未归类文件先只出现在受限核查区，操作者逐条 `include` / `exclude` / `handoff` 后
//     `--seal` 才生成可交付包；`exclude` / `handoff` 条目**留在最终清单**里可对账。
//
// This file is the workspace asset manifest: whitelist capture, the double inventory inside
// one quiet window, the two-stage review of unclassified files, the packaged-content check and
// the triple binding to the database manifest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	ws "github.com/ymedword/basalt/internal/workspace"
)

const (
	// AssetFormatVersion 是 `assets.json` 的格式版本；不认识的版本**必须**中止。
	// AssetFormatVersion is the assets.json format version; unknown versions abort.
	AssetFormatVersion = 1

	// AssetManifestFileName 是资产清单在导出目录里的固定文件名。
	AssetManifestFileName = "assets.json"

	// assetReviewDirName 是受限核查区（逐文件镜像 + 核查清单）；未封装前**不得**
	// 生成可交付包。assetPackageDirName 是封装后产出的可交付目录。
	assetReviewDirName  = "review"
	assetPackageDirName = "package"
)

// PackageRoot 返回可交付包在导出目录下的物理位置（`<dir>/package`）。导出 / 复核 /
// 导入三处都按这个布局读写，调用方不得自己拼这个字面量——布局一改，拼字面量的地方
// 会静默指向一个不存在的目录，而不是编译失败。
// PackageRoot is the physical location of the deliverable package under the export dir;
// callers must not spell the layout literal themselves.
func PackageRoot(exportDir string) string {
	return filepath.Join(exportDir, assetPackageDirName)
}

// 枚举字面量与颗粒度文档 §3.1.2 逐字一致；清单里写错一个字母，对账就失去意义。
// The enum literals are frozen by the design document.
const (
	// AssetClassBusiness 是白名单命中的业务资产；AssetClassUnclassified 是既不在白名单、
	// 也不命中排除项的普通文件——**不得**静默丢弃。
	AssetClassBusiness     = "business"
	AssetClassUnclassified = "unclassified"

	// AssetKind 区分资产的用途；`required_for` 另记"缺了它哪个 Agent 不完整"。
	AssetKindAgentDef = "agent-def"
	AssetKindScript   = "script"
	AssetKindFixture  = "fixture"
	AssetKindSampleDB = "sample-db"
	AssetKindDocInput = "doc-input"
	AssetKindReport   = "report"
	AssetKindState    = "state"

	// AssetDisposition 是逐条处置：`excluded` / `handoff` **必须**带原因且留在清单里。
	AssetDispositionIncluded = "included"
	AssetDispositionExcluded = "excluded"
	AssetDispositionHandoff  = "handoff"

	// 核查状态：无需核查（没有未归类文件）/ 待核查（不可交付）/ 已封装（可交付）。
	AssetReviewNotRequired = "not_required"
	AssetReviewPending     = "pending"
	AssetReviewSealed      = "sealed"

	// 实例标识的来源：名册原生，或经 C-47 采纳（**不得**把采纳说成原生）。
	AssetOriginRoster               = "roster"
	AssetOriginAdoptedWithoutRoster = "adopted_without_roster"
)

// 资产侧的判定错误。每个哨兵对应颗粒度文档的一个 `code`；**不得**用同一个错误码
// 覆盖两种失败——调用方要靠它区分"重采"与"转人工"。
// Each sentinel mirrors one design-document error code.
var (
	// ErrWorkspaceBusy 表示工作区实例锁被活实例持有：窗口不静，拒绝采集。
	ErrWorkspaceBusy = errors.New("dbmigrate: 工作区实例锁被活实例持有（workspace_busy），拒绝在不安全的窗口里采集资产")

	// ErrAssetsChangedDuringCapture 表示 A1 与 A2 不一致：本次静止窗口作废，**不产出**清单。
	ErrAssetsChangedDuringCapture = errors.New("dbmigrate: 前后两次盘点不一致（assets_changed_during_capture），作废本次静止窗口")

	// ErrUnclassifiedUnreviewed 表示未归类文件的逐条处置不完整：缺条目、摘要不符或决定非法。
	ErrUnclassifiedUnreviewed = errors.New("dbmigrate: 未归类文件的逐条处置不完整（unclassified_unreviewed），拒绝封装与切换")

	// ErrPackagedDigestMismatch 表示包内内容与盘点内容不一致（缺文件 / 多文件 / 摘要不符）。
	ErrPackagedDigestMismatch = errors.New("dbmigrate: 打包产物的摘要与清单不符（packaged_digest_mismatch），拒绝出包")

	// ErrSourceChangedSinceSnapshot 表示源端在采集之后发生了变化：旧快照作废，须重采。
	ErrSourceChangedSinceSnapshot = errors.New("dbmigrate: 源端在快照之后发生了变化（source_changed_since_snapshot），拒绝用旧快照切换")

	// 三元组绑定失败：批次 / 实例 / 快照任一不等即拒绝，**不得**"取交集"或"合并"。
	ErrBatchMismatch    = errors.New("dbmigrate: 两份清单的 migration_batch_id 不一致（batch_mismatch）")
	ErrInstanceMismatch = errors.New("dbmigrate: 两份清单的 source_instance_id 不一致（instance_mismatch）")
	ErrSnapshotMismatch = errors.New("dbmigrate: 两份清单的 source_snapshot_sha256 不一致（snapshot_mismatch）")
)

// 硬编码的排除项：导出包里出现其中任何一项都等于把库、锁或密钥交出去。按**具体条目**
// 排除（文件名 / 指定目录），**不得**整目录排除工作区。
// The hard-coded exclusions are concrete entries, never the workspace as a whole.
var (
	assetExcludedFiles = map[string]struct{}{
		"basalt.db":         {},
		"basalt.db-wal":     {},
		"basalt.db-shm":     {},
		"basalt.db-journal": {},
		"basalt.lock":       {},
		"instance.json":     {},
		"master.key":        {},
		"admin-token":       {},
	}

	assetExcludedDirs = []string{"logs", ".git", "dev-env/secrets"}
)

// DBFileIdentity 是源库文件在采集时刻的身份（C-46）：`basalt.db` 的尺寸 + SHA-256 是
// **唯一必需**判据；`-wal` 只在存在时记录并比对；`-shm` 是 WAL 的派生文件，**只记存在性**
// （Size / SHA256 恒为零值），内容随连接建立 / 关闭变化，不参与判据。
// DBFileIdentity is one source database file's identity at capture time.
type DBFileIdentity struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256,omitempty"`
}

// AssetEntry 是清单里的一条资产：路径、归类、处置与平台权限口径。
// `Mode` 在 Windows 只有只读位与类型位（C-50），`ACLDigest` 未采集时为空——
// 此时报告**必须**写"Windows DACL 未验证"，不得把 `Mode` 相等表述成权限已完整保真。
// AssetEntry is one manifest row; Mode alone never proves Windows ACL fidelity.
type AssetEntry struct {
	RelPath           string   `json:"rel_path"`
	Class             string   `json:"class"`
	Kind              string   `json:"kind,omitempty"`
	Disposition       string   `json:"disposition,omitempty"`
	DispositionReason string   `json:"disposition_reason,omitempty"`
	Size              int64    `json:"size"`
	Mode              uint32   `json:"mode"`
	Owner             string   `json:"owner,omitempty"`
	ACLDigest         string   `json:"acl_digest,omitempty"`
	SHA256            string   `json:"sha256"`
	RequiredFor       []string `json:"required_for,omitempty"`
}

// AssetManifest 是**工作区资产**的独立清单（与数据库 dump 分开存放、分开校验，靠三元组
// 绑定）。批次号 / 实例标识 / 快照摘要由 `db export` 在拿到名册与快照后补写——
// 本包不猜实例身份（与 `Exporter.Export` 同一口径）。
// AssetManifest is the workspace-side manifest; the binding triple is filled by the CLI layer.
type AssetManifest struct {
	FormatVersion          int              `json:"format_version"`
	MigrationBatchID       string           `json:"migration_batch_id,omitempty"`
	SourceInstanceID       string           `json:"source_instance_id,omitempty"`
	SourceInstanceIDOrigin string           `json:"source_instance_id_origin,omitempty"`
	SourceSnapshotSHA256   string           `json:"source_snapshot_sha256,omitempty"`
	SourceDBFiles          []DBFileIdentity `json:"source_db_files"`
	WorkspaceID            string           `json:"workspace_id"`
	GeneratedAt            time.Time        `json:"generated_at"`
	Entries                []AssetEntry     `json:"entries"`
	UnclassifiedDigest     string           `json:"unclassified_digest,omitempty"`
	ReviewStatus           string           `json:"review_status"`
	ReviewDigest           string           `json:"review_digest,omitempty"`
	Reviewer               string           `json:"reviewer,omitempty"`
	ReviewedAt             time.Time        `json:"reviewed_at,omitzero"`
}

// Agents 返回清单覆盖到的 Agent 名（`RequiredFor` 的并集，字典序）。
// 未安装的 Agent 不会出现在清单里——调用方据此核对"该有的 Agent 一个都不能少"。
// Agents returns the sorted union of RequiredFor across entries.
func (m AssetManifest) Agents() []string {
	seen := map[string]struct{}{}
	for _, entry := range m.Entries {
		for _, agent := range entry.RequiredFor {
			seen[agent] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AssetSpec 是白名单的一条申报：相对工作区根的路径（文件或目录）、资产类别，以及
// 缺了它哪个 Agent 不完整。目录声明对整棵子树生效；更长路径的申报优先（见 `classify`）。
// AssetSpec declares one whitelist entry; longer paths win over their ancestors.
type AssetSpec struct {
	Path        string   `json:"path"`
	Kind        string   `json:"kind"`
	RequiredFor []string `json:"required_for,omitempty"`
}

// ReviewDecision 是操作者对**一条**未归类文件的处置；`exclude` / `handoff` **必须**写明原因。
type ReviewDecision struct {
	RelPath     string `json:"rel_path"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
}

// ReviewFile 是 `unclassified-review.json` 的结构：先核对未归类摘要，再逐条取处置。
// 摘要不符即整份核查作废——换一份清单配这份核查文件同样过不了。
// ReviewFile is the operator's per-entry review; the digest must match the manifest.
type ReviewFile struct {
	UnclassifiedDigest string           `json:"unclassified_digest"`
	Reviewer           string           `json:"reviewer,omitempty"`
	ReviewedAt         time.Time        `json:"reviewed_at,omitzero"`
	Decisions          []ReviewDecision `json:"decisions"`
}

// AssetReport 是资产复核的差异表：缺项、哈希不符、清单外新增、权限漂移与源库文件漂移
// 分开列。`MissingFor` 按**组件名**点名"缺了它哪个 Agent 不完整"，不只说"依赖缺失"。
// AssetReport keeps each kind of drift apart; MissingFor names the agents affected.
type AssetReport struct {
	Missing         []string            `json:"missing,omitempty"`
	Mismatched      []string            `json:"mismatched,omitempty"`
	Unexpected      []string            `json:"unexpected,omitempty"`
	PermissionDrift []string            `json:"permission_drift,omitempty"`
	DBFileDrift     []string            `json:"db_file_drift,omitempty"`
	MissingFor      map[string][]string `json:"missing_for,omitempty"`
	DBFiles         []DBFileIdentity    `json:"db_files,omitempty"`
	Notes           []string            `json:"notes,omitempty"`
}

// OK 判复核是否整体通过：任一类差异非空都不通过，缺失**不**按"未比较"放过。
func (r AssetReport) OK() bool {
	return len(r.Missing) == 0 && len(r.Mismatched) == 0 && len(r.Unexpected) == 0 &&
		len(r.PermissionDrift) == 0 && len(r.DBFileDrift) == 0
}

// assetRule 是归一化后的白名单条目；`rules` 按路径长度降序，最长前缀优先匹配。
type assetRule struct {
	path        string
	kind        string
	requiredFor []string
}

// assetCapture 是一次完整盘点的结果：文件条目（按 RelPath 升序）与源库文件身份。
type assetCapture struct {
	entries []AssetEntry
	dbFiles []DBFileIdentity
}

// AssetWindow 是静止窗口里的两个回调点（图 9b 的 [1] → [5] 顺序）：`AfterLock` 在取到
// 实例锁之后、第一次盘点（A1）之前；`Between` 在 A1 与 A2 之间，数据库快照与逐表导出
// 落在这里。任一回调报错即整窗作废——**不产出**清单。
//
// 为什么是回调而不是参数：批次号来自导出、实例标识来自名册，两者都只能在窗口内取到；
// 把顺序留给调用方（`db export`）同时保住了本包"不猜实例身份、不编批次"的分工。
// AssetWindow carries the two callback points inside the quiet window: the roster read
// happens after the lock and before A1, the database snapshot between A1 and A2.
type AssetWindow struct {
	// AfterLock 在实例锁之后、A1 之前执行（读名册、固定实例标识）。
	AfterLock func(ctx context.Context) error

	// Between 在 A1 与 A2 之间执行（`VACUUM INTO` 快照与逐表导出）。
	Between func(ctx context.Context) error
}

// BuildAssetManifest 在静止窗口内盘点工作区两次（A1 / A2），两次全等才产出清单。
//
// 采集前**必须**取到 `<ws>/basalt.lock`（与 `serve` 互斥，取不到 → `workspace_busy`）。
// **锁只约束会调 `AcquireInstanceLock` 的 Basalt 进程**：人工编辑、编辑器自动保存、
// `sqlite3`、备份 / 同步 / 杀毒软件都不取锁；A1 = A2 只证明"两个时刻一致"，**不得**
// 表述成"覆盖了所有文件写入"。窗口结束后锁即释放，之后的写入由切换前的复核发现。
//
// 批次号 / 实例标识 / 快照摘要**不在这里生成**：`db export` 在同一窗口内先作快照、
// 读名册，再补写三元组（与 `Exporter.Export` 同一分工）。需要把这两步塞进窗口时用
// `BuildAssetManifestAround`；本函数是"窗口里什么都不做"的薄包装。
// BuildAssetManifest captures the workspace twice inside the quiet window and only then
// returns a manifest; the binding triple is filled later by the CLI layer.
func BuildAssetManifest(ctx context.Context, workspace string, specs []AssetSpec) (AssetManifest, error) {
	return BuildAssetManifestAround(ctx, workspace, specs, AssetWindow{})
}

// BuildAssetManifestAround 与 `BuildAssetManifest` 同一口径，另在窗口内的两个位置回调
// 调用方：`AfterLock`（锁之后、A1 之前）与 `Between`（A1 与 A2 之间）。
//
// 顺序是判据的一部分：数据库连接**必须**在调用本函数之前打开——只读打开会建
// `basalt.db-wal` / `-shm`（T070 实测），窗口内打开会让 A1 与 A2 的源库文件身份不等；
// 关闭放在调用方（`db export`）的 defer 里，A2 之后关，窗口内档案不再变化。
// BuildAssetManifestAround adds the two callback points to the quiet window; the source
// database handle must already be open before the call, because opening creates the -wal
// and -shm files and would make A1 and A2 disagree.
func BuildAssetManifestAround(ctx context.Context, workspace string, specs []AssetSpec, around AssetWindow) (manifest AssetManifest, err error) {
	rootDir := strings.TrimSpace(workspace)
	if rootDir == "" {
		return AssetManifest{}, errors.New("dbmigrate: 资产采集必须给出工作区根目录（<WorkDir>/.basalt）")
	}
	if err := ctx.Err(); err != nil {
		return AssetManifest{}, err
	}
	rules, err := normalizeSpecs(specs)
	if err != nil {
		return AssetManifest{}, err
	}

	lock, err := ws.AcquireInstanceLock(rootDir)
	if err != nil {
		var busy *ws.LockBusyError
		if errors.As(err, &busy) {
			return AssetManifest{}, fmt.Errorf("%w: %v", ErrWorkspaceBusy, err)
		}
		return AssetManifest{}, fmt.Errorf("dbmigrate: 取工作区实例锁: %w", err)
	}
	defer func() {
		if releaseErr := lock.Release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("dbmigrate: 释放工作区实例锁: %w", releaseErr))
		}
	}()

	if around.AfterLock != nil {
		if err := around.AfterLock(ctx); err != nil {
			return AssetManifest{}, err
		}
	}

	first, err := captureWorkspace(ctx, rootDir, rules)
	if err != nil {
		return AssetManifest{}, err
	}
	if around.Between != nil {
		if err := around.Between(ctx); err != nil {
			return AssetManifest{}, err
		}
	}
	second, err := captureWorkspace(ctx, rootDir, rules)
	if err != nil {
		return AssetManifest{}, err
	}
	if same, difference := sameCapture(first, second); !same {
		return AssetManifest{}, fmt.Errorf("%w: %s", ErrAssetsChangedDuringCapture, difference)
	}

	identifier, err := workspaceIdentifier(rootDir)
	if err != nil {
		return AssetManifest{}, err
	}
	status := AssetReviewNotRequired
	if unclassifiedCount(first.entries) > 0 {
		status = AssetReviewPending
	}
	return AssetManifest{
		FormatVersion:      AssetFormatVersion,
		SourceDBFiles:      first.dbFiles,
		WorkspaceID:        identifier,
		GeneratedAt:        time.Now().UTC(),
		Entries:            first.entries,
		UnclassifiedDigest: unclassifiedDigest(first.entries),
		ReviewStatus:       status,
	}, nil
}

// VerifyAssets 把工作区重新盘一遍并与清单逐项比对（缺项 / 哈希 / 权限 / 源库文件身份）。
//
// 它**不取锁**：调用方负责"源端保持停止"这一操作前提（C-46）。比对差别**不**返回错误——
// 差异进报告，由调用方决定映射到哪个码：切换前重盘 → `source_changed_since_snapshot`，
// 打包复核 → `packaged_digest_mismatch`。
// VerifyAssets re-inventories the workspace and reports drift without taking the lock.
func VerifyAssets(ctx context.Context, workspace string, m AssetManifest) (AssetReport, error) {
	rootDir := strings.TrimSpace(workspace)
	if rootDir == "" {
		return AssetReport{}, errors.New("dbmigrate: 资产复核必须给出工作区根目录（<WorkDir>/.basalt）")
	}
	if err := ctx.Err(); err != nil {
		return AssetReport{}, err
	}
	if err := validateAssetManifest(m); err != nil {
		return AssetReport{}, err
	}

	current, err := captureWorkspace(ctx, rootDir, nil)
	if err != nil {
		return AssetReport{}, err
	}

	report := AssetReport{DBFiles: current.dbFiles}
	expected := make(map[string]AssetEntry, len(m.Entries))
	for _, entry := range m.Entries {
		expected[entry.RelPath] = entry
	}
	present := make(map[string]struct{}, len(current.entries))
	for _, now := range current.entries {
		want, listed := expected[now.RelPath]
		if !listed {
			report.Unexpected = append(report.Unexpected, now.RelPath)
			continue
		}
		present[now.RelPath] = struct{}{}
		switch {
		case now.Size != want.Size || now.SHA256 != want.SHA256:
			report.Mismatched = append(report.Mismatched, now.RelPath)
		case now.Mode != want.Mode || now.Owner != want.Owner || now.ACLDigest != want.ACLDigest:
			// 权限口径按平台比较（C-50）；不符只报告，**不**静默应用原值——那会掩盖迁移
			// 过程里的真实变化。
			report.PermissionDrift = append(report.PermissionDrift, now.RelPath)
		}
	}
	for _, want := range m.Entries {
		if _, listed := present[want.RelPath]; listed {
			continue
		}
		report.Missing = append(report.Missing, want.RelPath)
		if len(want.RequiredFor) > 0 {
			if report.MissingFor == nil {
				report.MissingFor = map[string][]string{}
			}
			component := path.Base(want.RelPath)
			report.MissingFor[component] = appendUnique(report.MissingFor[component], want.RequiredFor...)
		}
	}
	report.DBFileDrift = compareDBFiles(m.SourceDBFiles, current.dbFiles)
	if runtime.GOOS == "windows" && anyWithoutACL(m.Entries) {
		report.Notes = append(report.Notes, "Windows DACL 未验证：清单未采集 DACL 摘要，Mode 相等只覆盖只读位（C-50）")
	}

	sort.Strings(report.Missing)
	sort.Strings(report.Mismatched)
	sort.Strings(report.Unexpected)
	sort.Strings(report.PermissionDrift)
	return report, nil
}

// SealAssetPackage 校验逐条处置后把核查区里的 `included` 条目封装成可交付包，并对
// **打包产物**逐项重算哈希与清单比对（C-49）：不符即 `packaged_digest_mismatch`。
//
// `review/` 是操作者核查前的工作镜像；`package/` 只放 `included` 条目。`excluded` /
// `handoff` 条目**留在返回的清单里**（带处置、原因与哈希）可对账，只是不进包。
// SealAssetPackage validates the review, packages the included entries and re-hashes the
// product; excluded/handoff entries stay in the manifest for reconciliation.
func SealAssetPackage(ctx context.Context, outDir string, m AssetManifest, review ReviewFile) (AssetManifest, error) {
	if err := ctx.Err(); err != nil {
		return AssetManifest{}, err
	}
	if strings.TrimSpace(outDir) == "" {
		return AssetManifest{}, errors.New("dbmigrate: 封装必须给出导出目录")
	}
	if err := validateAssetManifest(m); err != nil {
		return AssetManifest{}, err
	}
	switch m.ReviewStatus {
	case AssetReviewPending:
		if review.UnclassifiedDigest == "" || review.UnclassifiedDigest != m.UnclassifiedDigest {
			return AssetManifest{}, fmt.Errorf("%w: 核查文件的 unclassified_digest 与清单不一致", ErrUnclassifiedUnreviewed)
		}
	case AssetReviewNotRequired:
		// 没有未归类文件就不存在"待核查"这一步：核查文件必须为空，否则等于把一份
		// 别处的处置悄悄套到本清单上。
		if review.UnclassifiedDigest != "" || len(review.Decisions) != 0 {
			return AssetManifest{}, fmt.Errorf("%w: 清单没有未归类文件，核查文件却带了摘要 / 处置", ErrUnclassifiedUnreviewed)
		}
	case AssetReviewSealed:
		return AssetManifest{}, fmt.Errorf("dbmigrate: 清单 review_status 已是 %s，拒绝重复封装", AssetReviewSealed)
	}

	sealed, err := applyReview(m, review)
	if err != nil {
		return AssetManifest{}, err
	}
	if m.ReviewStatus == AssetReviewPending {
		reviewDigest, err := digestReview(review)
		if err != nil {
			return AssetManifest{}, err
		}
		sealed.ReviewStatus = AssetReviewSealed
		sealed.ReviewDigest = reviewDigest
		sealed.Reviewer = review.Reviewer
		sealed.ReviewedAt = review.ReviewedAt.UTC()
	}

	if err := assemblePackage(ctx, outDir, sealed); err != nil {
		return AssetManifest{}, err
	}
	if err := verifyPackagedContent(ctx, outDir, sealed); err != nil {
		return AssetManifest{}, err
	}
	if err := WriteAssetManifest(outDir, sealed); err != nil {
		return AssetManifest{}, err
	}
	return sealed, nil
}

// BindAssetManifest 校验两份清单的**三元组**：批次、实例、快照逐字相等。
// 各自单独合法但批次不同同样拒绝——**不得**"取交集"或"合并"两个批次。
// BindAssetManifest enforces the triple binding; nothing is ever merged across batches.
func BindAssetManifest(m AssetManifest, db Manifest) error {
	if err := validateAssetManifest(m); err != nil {
		return err
	}
	if m.MigrationBatchID == "" || db.MigrationBatchID == "" || m.MigrationBatchID != db.MigrationBatchID {
		return fmt.Errorf("%w: assets=%q manifest=%q", ErrBatchMismatch, m.MigrationBatchID, db.MigrationBatchID)
	}
	if m.SourceInstanceID == "" || db.SourceInstanceID == "" || m.SourceInstanceID != db.SourceInstanceID {
		return fmt.Errorf("%w: assets=%q manifest=%q", ErrInstanceMismatch, m.SourceInstanceID, db.SourceInstanceID)
	}
	if m.SourceSnapshotSHA256 == "" || db.SourceSnapshotSHA256 == "" ||
		m.SourceSnapshotSHA256 != db.SourceSnapshotSHA256 {
		return fmt.Errorf("%w: assets=%q manifest=%q", ErrSnapshotMismatch, m.SourceSnapshotSHA256, db.SourceSnapshotSHA256)
	}
	return nil
}

// normalizeSpecs 归一化白名单：路径必须是工作区根下的正斜杠相对路径、类别已知、
// **不得**声明排除项；结果按路径长度降序，供最长前缀匹配。
func normalizeSpecs(specs []AssetSpec) ([]assetRule, error) {
	if len(specs) == 0 {
		return nil, errors.New("dbmigrate: 资产采集必须给出白名单（AssetSpec）——空白名单会把整棵树都当未归类")
	}
	rules := make([]assetRule, 0, len(specs))
	seen := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		rel, err := normalizeRelPath(spec.Path)
		if err != nil {
			return nil, fmt.Errorf("dbmigrate: 白名单条目 %q: %w", spec.Path, err)
		}
		if _, dup := seen[rel]; dup {
			return nil, fmt.Errorf("dbmigrate: 白名单条目 %q 出现两次", rel)
		}
		seen[rel] = struct{}{}
		if !knownKind(spec.Kind) {
			return nil, fmt.Errorf("dbmigrate: 白名单条目 %q 的类别 %q 未知（%s）", rel, spec.Kind, knownKindList())
		}
		if excludedFile(rel) || excludedDir(rel) {
			return nil, fmt.Errorf("dbmigrate: 白名单条目 %q 命中排除项，不得声明", rel)
		}
		for _, agent := range spec.RequiredFor {
			if strings.TrimSpace(agent) == "" {
				return nil, fmt.Errorf("dbmigrate: 白名单条目 %q 的 required_for 里有空 Agent 名", rel)
			}
		}
		rules = append(rules, assetRule{
			path:        rel,
			kind:        spec.Kind,
			requiredFor: append([]string(nil), spec.RequiredFor...),
		})
	}
	sort.Slice(rules, func(i, j int) bool { return len(rules[i].path) > len(rules[j].path) })
	return rules, nil
}

// normalizeRelPath 归一化并校验一条相对路径：拒绝绝对路径、盘符、UNC、`..` 段与
// 反斜杠；结果必须仍是工作区根内的相对路径。
func normalizeRelPath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("路径为空")
	}
	if strings.Contains(trimmed, `\`) {
		return "", errors.New("路径必须用正斜杠")
	}
	if strings.HasPrefix(trimmed, "/") {
		return "", errors.New("拒绝绝对路径 / UNC（//server/share）")
	}
	if len(trimmed) >= 2 && trimmed[1] == ':' {
		return "", errors.New("拒绝盘符路径")
	}
	for _, segment := range strings.Split(trimmed, "/") {
		if segment == ".." {
			return "", errors.New("路径不得包含 .. 段")
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("路径必须指向工作区根内的具体条目")
	}
	return cleaned, nil
}

// classify 按最长前缀判定一条路径属于哪条白名单申报；都不命中即未归类。
func classify(rel string, rules []assetRule) (string, []string) {
	for _, rule := range rules {
		if rel == rule.path || strings.HasPrefix(rel, rule.path+"/") {
			return rule.kind, rule.requiredFor
		}
	}
	return "", nil
}

// excludedDir 判目录是否整体排除（`logs/`、`.git/`、`dev-env/secrets/`）。
func excludedDir(rel string) bool {
	for _, dir := range assetExcludedDirs {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

// excludedFile 判具体文件是否排除：库与旁路、锁、名册、密钥、`.env*`、`*.clixml`。
// 按文件名在**任意深度**命中——同名文件出现在更深层同样不该进包（fail-closed）。
func excludedFile(rel string) bool {
	if excludedDir(rel) {
		return true
	}
	base := path.Base(rel)
	if _, excluded := assetExcludedFiles[base]; excluded {
		return true
	}
	if strings.HasPrefix(base, ".env") || strings.HasSuffix(base, ".clixml") {
		return true
	}
	return false
}

// captureWorkspace 打开工作区根并完整盘点一次：逐文件取证、归类、记源库文件身份。
// `rules` 为空表示只做复核（复核方只关心路径与哈希，不重新归类）。
func captureWorkspace(ctx context.Context, rootDir string, rules []assetRule) (assetCapture, error) {
	handle, err := os.OpenRoot(rootDir)
	if err != nil {
		return assetCapture{}, fmt.Errorf("dbmigrate: 打开工作区 %s: %w", rootDir, err)
	}
	defer func() { _ = handle.Close() }()
	fsys := handle.FS()

	var entries []AssetEntry
	walkErr := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("dbmigrate: 遍历工作区 %s: %w", name, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if name == "." {
			return nil
		}
		rel := path.Clean(filepath.ToSlash(name))
		if d.IsDir() {
			if excludedDir(rel) {
				return fs.SkipDir
			}
			return nil
		}
		// 排除项先判：库 / 锁 / 名册 / 密钥不进清单，也就无所谓"什么类型"。
		if excludedFile(rel) {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("dbmigrate: 资产盘点拒绝符号链接（不跟随）：%s", rel)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("dbmigrate: 资产盘点只接受普通文件：%s", rel)
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return fmt.Errorf("dbmigrate: 读工作区条目 %s: %w", rel, infoErr)
		}
		mode, owner, links := platformMeta(info)
		if links > 1 {
			return fmt.Errorf("dbmigrate: 资产盘点拒绝硬链接（Nlink=%d）：%s", links, rel)
		}
		sum, sumErr := hashWorkspaceFile(fsys, name)
		if sumErr != nil {
			return sumErr
		}
		entry := AssetEntry{
			RelPath: rel,
			Class:   AssetClassUnclassified,
			Size:    info.Size(),
			Mode:    mode,
			Owner:   owner,
			SHA256:  sum,
		}
		if kind, requiredFor := classify(rel, rules); kind != "" {
			entry.Class = AssetClassBusiness
			entry.Kind = kind
			entry.Disposition = AssetDispositionIncluded
			entry.RequiredFor = append([]string(nil), requiredFor...)
		}
		entries = append(entries, entry)
		return nil
	})
	if walkErr != nil {
		return assetCapture{}, walkErr
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RelPath < entries[j].RelPath })
	if duplicate := duplicatePath(entries, pathCaseInsensitive()); duplicate != "" {
		return assetCapture{}, fmt.Errorf("dbmigrate: 工作区里存在仅大小写不同的重名路径：%s", duplicate)
	}

	dbFiles, err := captureDBFiles(fsys)
	if err != nil {
		return assetCapture{}, err
	}
	return assetCapture{entries: entries, dbFiles: dbFiles}, nil
}

// hashWorkspaceFile 按原始字节流式计算 SHA-256（大库不进内存）。
func hashWorkspaceFile(fsys fs.FS, name string) (string, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return "", fmt.Errorf("dbmigrate: 打开工作区文件 %s: %w", name, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", fmt.Errorf("dbmigrate: 读工作区文件 %s: %w", name, copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("dbmigrate: 关闭工作区文件 %s: %w", name, closeErr)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// captureDBFiles 用**文件系统读取**（stat + 原始字节哈希）记录源库文件身份，
// **不得**为取身份再打开 SQLite——打开即可能 checkpoint 并改写 WAL。
func captureDBFiles(fsys fs.FS) ([]DBFileIdentity, error) {
	names := [...]string{"basalt.db", "basalt.db-wal", "basalt.db-shm"}
	identities := make([]DBFileIdentity, 0, len(names))
	for _, name := range names {
		info, err := fs.Stat(fsys, name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				identities = append(identities, DBFileIdentity{Name: name})
				continue
			}
			return nil, fmt.Errorf("dbmigrate: 读源库文件身份 %s: %w", name, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("dbmigrate: 源库文件身份 %s 是目录", name)
		}
		identity := DBFileIdentity{Name: name, Present: true}
		if name != "basalt.db-shm" {
			identity.Size = info.Size()
			sum, err := hashWorkspaceFile(fsys, name)
			if err != nil {
				return nil, err
			}
			identity.SHA256 = sum
		}
		identities = append(identities, identity)
	}
	return identities, nil
}

// sameCapture 判 A1 与 A2 是否全等：条目集合逐项（路径 / 尺寸 / 哈希 / 平台权限字段）
// 与源库文件身份都要相等。返回的第一处差异写进错误，便于操作者知道是哪个文件在动。
func sameCapture(first, second assetCapture) (bool, string) {
	if len(first.entries) != len(second.entries) {
		return false, fmt.Sprintf("A1 有 %d 个文件，A2 有 %d 个", len(first.entries), len(second.entries))
	}
	for i := range first.entries {
		left, right := first.entries[i], second.entries[i]
		if left.RelPath != right.RelPath {
			return false, fmt.Sprintf("条目集合不同：A1 有 %q，A2 有 %q", left.RelPath, right.RelPath)
		}
		if left.Size != right.Size || left.SHA256 != right.SHA256 ||
			left.Mode != right.Mode || left.Owner != right.Owner || left.ACLDigest != right.ACLDigest {
			return false, fmt.Sprintf("%s 在两次盘点之间发生变化", left.RelPath)
		}
	}
	if len(first.dbFiles) != len(second.dbFiles) {
		return false, "源库文件身份的条目数不同"
	}
	for i := range first.dbFiles {
		left, right := first.dbFiles[i], second.dbFiles[i]
		if left.Name != right.Name || left.Present != right.Present ||
			left.Size != right.Size || left.SHA256 != right.SHA256 {
			return false, fmt.Sprintf("源库文件 %s 在两次盘点之间发生变化", left.Name)
		}
	}
	return true, ""
}

// duplicatePath 返回第一条仅大小写不同的重名路径（空串 = 无冲突）：
// Windows 下 `Foo.md` 与 `foo.md` 视为冲突并拒绝。`insensitive` 由平台决定。
func duplicatePath(entries []AssetEntry, insensitive bool) string {
	seen := make(map[string]string, len(entries))
	for _, entry := range entries {
		key := entry.RelPath
		if insensitive {
			key = strings.ToLower(key)
		}
		if previous, duplicated := seen[key]; duplicated && previous != entry.RelPath {
			return entry.RelPath + " / " + previous
		}
		seen[key] = entry.RelPath
	}
	return ""
}

// pathCaseInsensitive 返回当前平台是否按大小写不敏感查重（Windows 是）。
func pathCaseInsensitive() bool { return runtime.GOOS == "windows" }

// unclassifiedDigest 把未归类清单按 `RelPath` 升序摘要：每行 `路径 \x1f 尺寸 \x1f SHA-256`
// 以 `\x1e` 收尾。没有未归类文件时返回空串——它与"有未归类但摘要为空"必须可分。
func unclassifiedDigest(entries []AssetEntry) string {
	unclassified := make([]AssetEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Class == AssetClassUnclassified {
			unclassified = append(unclassified, entry)
		}
	}
	if len(unclassified) == 0 {
		return ""
	}
	sort.Slice(unclassified, func(i, j int) bool { return unclassified[i].RelPath < unclassified[j].RelPath })
	var builder strings.Builder
	for _, entry := range unclassified {
		fmt.Fprintf(&builder, "%s\x1f%d\x1f%s\x1e", entry.RelPath, entry.Size, entry.SHA256)
	}
	return sha256Hex([]byte(builder.String()))
}

// unclassifiedCount 数未归类条目。
func unclassifiedCount(entries []AssetEntry) int {
	count := 0
	for _, entry := range entries {
		if entry.Class == AssetClassUnclassified {
			count++
		}
	}
	return count
}

// applyReview 把逐条处置写回清单：摘要已比对过，这里只认条目齐全、决定合法、原因齐备。
// `include` / `exclude` / `handoff` 是操作者动词，落进清单时归一化成
// `included` / `excluded` / `handoff`（后两者必须有原因）。
func applyReview(m AssetManifest, review ReviewFile) (AssetManifest, error) {
	unclassified := make(map[string]int, len(m.Entries))
	for i, entry := range m.Entries {
		if entry.Class == AssetClassUnclassified {
			unclassified[entry.RelPath] = i
		}
	}

	sealed := m
	sealed.Entries = append([]AssetEntry(nil), m.Entries...)
	decided := make(map[string]struct{}, len(review.Decisions))
	for _, decision := range review.Decisions {
		rel, err := normalizeRelPath(decision.RelPath)
		if err != nil {
			return AssetManifest{}, fmt.Errorf("%w: 处置条目 %q: %v", ErrUnclassifiedUnreviewed, decision.RelPath, err)
		}
		index, listed := unclassified[rel]
		if !listed {
			return AssetManifest{}, fmt.Errorf("%w: 处置条目 %q 不是清单里的未归类文件", ErrUnclassifiedUnreviewed, rel)
		}
		if _, duplicated := decided[rel]; duplicated {
			return AssetManifest{}, fmt.Errorf("%w: 处置条目 %q 出现两次", ErrUnclassifiedUnreviewed, rel)
		}
		decided[rel] = struct{}{}
		disposition, ok := normalizeDisposition(decision.Disposition)
		if !ok {
			return AssetManifest{}, fmt.Errorf("%w: 处置条目 %q 的决定 %q 非法（只接受 include / exclude / handoff）",
				ErrUnclassifiedUnreviewed, rel, decision.Disposition)
		}
		reason := strings.TrimSpace(decision.Reason)
		if disposition != AssetDispositionIncluded && reason == "" {
			return AssetManifest{}, fmt.Errorf("%w: 处置条目 %q 的 %s 决定必须写明原因",
				ErrUnclassifiedUnreviewed, rel, disposition)
		}
		sealed.Entries[index].Disposition = disposition
		sealed.Entries[index].DispositionReason = reason
	}
	for rel := range unclassified {
		if _, ok := decided[rel]; !ok {
			return AssetManifest{}, fmt.Errorf("%w: 未归类文件 %q 没有处置决定", ErrUnclassifiedUnreviewed, rel)
		}
	}
	return sealed, nil
}

// normalizeDisposition 把操作者动词与清单字面量都归一化成清单口径（`handoff` 两者同名）。
func normalizeDisposition(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case "include", AssetDispositionIncluded:
		return AssetDispositionIncluded, true
	case "exclude", AssetDispositionExcluded:
		return AssetDispositionExcluded, true
	case AssetDispositionHandoff:
		return AssetDispositionHandoff, true
	default:
		return "", false
	}
}

// digestReview 计算核查文件的规范摘要：处置按 `RelPath` 排序后序列化再哈希——
// 同一份核查内容换个书写顺序不改变摘要。
func digestReview(review ReviewFile) (string, error) {
	decisions := append([]ReviewDecision(nil), review.Decisions...)
	sort.Slice(decisions, func(i, j int) bool { return decisions[i].RelPath < decisions[j].RelPath })
	canonical := ReviewFile{
		UnclassifiedDigest: review.UnclassifiedDigest,
		Reviewer:           review.Reviewer,
		ReviewedAt:         review.ReviewedAt.UTC(),
		Decisions:          decisions,
	}
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("dbmigrate: 序列化核查文件: %w", err)
	}
	return sha256Hex(raw), nil
}

// MaterializeReview 把清单里的每个条目按原样复制进 `<out>/review/<RelPath>`——两段式
// 核查的工作镜像（C-49）。复制时重算尺寸与 SHA-256 并与清单逐项比对：镜像必须与盘点
// 内容逐字节一致，否则整次导出作废（`assets_changed_during_capture`），**不**把
// "复制出来的东西"当成盘点证据。
//
// 为什么由导出方铺镜像：`assemblePackage` 只从 `review/` 取件——受限核查区是唯一取件源，
// 于是"封装的内容 = 操作者核查过的内容 = 盘点内容"这条链在代码里连续可查，中间没有
// 第三份副本可以偷换。
// MaterializeReview mirrors every manifest entry into review/ and re-hashes each copy; a
// mismatch voids the export instead of turning a stale copy into evidence.
func MaterializeReview(ctx context.Context, workspace, outDir string, m AssetManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sourceDir := strings.TrimSpace(workspace)
	dir := strings.TrimSpace(outDir)
	if sourceDir == "" {
		return errors.New("dbmigrate: 铺核查区必须给出工作区根目录（<WorkDir>/.basalt）")
	}
	if dir == "" {
		return errors.New("dbmigrate: 铺核查区必须给出导出目录")
	}
	if err := validateAssetManifest(m); err != nil {
		return err
	}

	source, err := os.OpenRoot(sourceDir)
	if err != nil {
		return fmt.Errorf("dbmigrate: 打开工作区 %s: %w", sourceDir, err)
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("dbmigrate: 打开导出目录 %s: %w", dir, err)
	}
	defer func() { _ = target.Close() }()

	// 每次重铺先清空旧核查区：上一轮（另一个批次）的镜像留在原处会被当成本批的取件源。
	if err := target.RemoveAll(assetReviewDirName); err != nil {
		return fmt.Errorf("dbmigrate: 清空旧核查区: %w", err)
	}
	if err := target.MkdirAll(assetReviewDirName, 0o750); err != nil {
		return fmt.Errorf("dbmigrate: 建核查区: %w", err)
	}

	for _, entry := range m.Entries {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		raw, err := source.ReadFile(entry.RelPath)
		if err != nil {
			return fmt.Errorf("%w: 读工作区文件 %s: %v", ErrAssetsChangedDuringCapture, entry.RelPath, err)
		}
		if int64(len(raw)) != entry.Size || sha256Hex(raw) != entry.SHA256 {
			return fmt.Errorf("%w: %s 在盘点之后发生变化", ErrAssetsChangedDuringCapture, entry.RelPath)
		}
		rel := assetReviewDirName + "/" + entry.RelPath
		if err := target.MkdirAll(path.Dir(rel), 0o750); err != nil {
			return fmt.Errorf("dbmigrate: 建核查区目录 %s: %w", path.Dir(entry.RelPath), err)
		}
		perm := fs.FileMode(entry.Mode & 0o777)
		if perm == 0 {
			perm = 0o600
		}
		if err := target.WriteFile(rel, raw, perm); err != nil {
			return fmt.Errorf("dbmigrate: 写核查区文件 %s: %w", entry.RelPath, err)
		}
	}
	return nil
}

// VerifyPackage 离线复核导出目录里的 `package/` 与资产清单：清单要求入包的条目缺一个、
// 多一个、尺寸或 SHA-256 不符都进报告——**不**在第一条不符处中止。
//
// 与 `verifyPackagedContent` 的分工：那个是封装路径的内部断言（遇到第一处不符就拒绝出包），
// 这个是 `db verify --dump` 的复核面，要求"缺项逐条列出"（设计 §3.1.8）。
// 权限位只在非 Windows 平台参与判据：Windows 的 Mode 只有只读位（C-50），拿它当
// "权限已保真"的证据是自欺，报告改以 `Notes` 说明未覆盖面。
// VerifyPackage reports every drift instead of stopping at the first one; permission bits
// only count off Windows, where the mode carries just the read-only bit.
func VerifyPackage(ctx context.Context, outDir string, m AssetManifest) (AssetReport, error) {
	if err := ctx.Err(); err != nil {
		return AssetReport{}, err
	}
	dir := strings.TrimSpace(outDir)
	if dir == "" {
		return AssetReport{}, errors.New("dbmigrate: 包内复核必须给出导出目录")
	}
	if err := validateAssetManifest(m); err != nil {
		return AssetReport{}, err
	}

	report := AssetReport{}
	wanted := make(map[string]AssetEntry, len(m.Entries))
	for _, entry := range m.Entries {
		if entry.Disposition == AssetDispositionIncluded {
			wanted[entry.RelPath] = entry
		}
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return AssetReport{}, fmt.Errorf("dbmigrate: 打开导出目录 %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()

	found := make(map[string]struct{}, len(wanted))
	walkErr := fs.WalkDir(fsys, assetPackageDirName, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			// `package/` 不存在：整份清单要求的条目都算缺项，由下面的循环逐条列出。
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipDir
			}
			return fmt.Errorf("dbmigrate: 遍历可交付包 %s: %w", name, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path.Clean(filepath.ToSlash(name)), assetPackageDirName+"/")
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			report.Unexpected = append(report.Unexpected, rel)
			return nil
		}
		entry, listed := wanted[rel]
		if !listed {
			report.Unexpected = append(report.Unexpected, rel)
			return nil
		}
		found[rel] = struct{}{}
		info, infoErr := d.Info()
		if infoErr != nil {
			return fmt.Errorf("dbmigrate: 读包内条目 %s: %w", rel, infoErr)
		}
		sum, sumErr := hashWorkspaceFile(fsys, name)
		if sumErr != nil {
			return sumErr
		}
		if info.Size() != entry.Size || sum != entry.SHA256 {
			report.Mismatched = append(report.Mismatched, rel)
			return nil
		}
		if runtime.GOOS != "windows" && uint32(info.Mode().Perm()) != entry.Mode&0o777 {
			report.PermissionDrift = append(report.PermissionDrift, rel)
		}
		return nil
	})
	if walkErr != nil {
		return AssetReport{}, walkErr
	}
	for rel := range wanted {
		if _, listed := found[rel]; !listed {
			report.Missing = append(report.Missing, rel)
			if entry, ok := wanted[rel]; ok && len(entry.RequiredFor) > 0 {
				if report.MissingFor == nil {
					report.MissingFor = map[string][]string{}
				}
				component := path.Base(entry.RelPath)
				report.MissingFor[component] = appendUnique(report.MissingFor[component], entry.RequiredFor...)
			}
		}
	}
	sort.Strings(report.Missing)
	sort.Strings(report.Mismatched)
	sort.Strings(report.Unexpected)
	sort.Strings(report.PermissionDrift)
	if runtime.GOOS == "windows" {
		report.Notes = append(report.Notes, "Windows 下包内权限位不参与判据（C-50）：Mode 相等不等于 ACL 已保真")
	}
	return report, nil
}

// assemblePackage 从受限核查区把 `included` 条目复制进 `package/`；每次封装先清空旧包，
// 避免上一轮的残留被当成"包内内容"。
func assemblePackage(ctx context.Context, outDir string, m AssetManifest) error {
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return fmt.Errorf("dbmigrate: 打开导出目录 %s: %w", outDir, err)
	}
	defer func() { _ = root.Close() }()

	if err := root.RemoveAll(assetPackageDirName); err != nil {
		return fmt.Errorf("dbmigrate: 清空旧包目录: %w", err)
	}
	if err := root.MkdirAll(assetPackageDirName, 0o750); err != nil {
		return fmt.Errorf("dbmigrate: 建可交付包目录: %w", err)
	}
	for _, entry := range m.Entries {
		if entry.Disposition != AssetDispositionIncluded {
			continue
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		raw, err := root.ReadFile(assetReviewDirName + "/" + entry.RelPath)
		if err != nil {
			return fmt.Errorf("%w: 核查包里缺 %s: %v", ErrPackagedDigestMismatch, entry.RelPath, err)
		}
		target := assetPackageDirName + "/" + entry.RelPath
		if err := root.MkdirAll(path.Dir(target), 0o750); err != nil {
			return fmt.Errorf("dbmigrate: 建包内目录 %s: %w", path.Dir(entry.RelPath), err)
		}
		perm := fs.FileMode(entry.Mode & 0o777)
		if perm == 0 {
			perm = 0o600
		}
		if err := root.WriteFile(target, raw, perm); err != nil {
			return fmt.Errorf("dbmigrate: 写包内文件 %s: %w", entry.RelPath, err)
		}
	}
	return nil
}

// verifyPackagedContent 对**打包产物**逐项重算尺寸与 SHA-256 并与清单比对：
// 缺文件、多文件、摘要不符都算 `packaged_digest_mismatch`。它证明"包内内容 = 盘点内容"，
// **不**证明源端在 A2 之后没再变化（那由切换前的重盘负责）。
func verifyPackagedContent(ctx context.Context, outDir string, m AssetManifest) error {
	root, err := os.OpenRoot(outDir)
	if err != nil {
		return fmt.Errorf("dbmigrate: 打开导出目录 %s: %w", outDir, err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()

	wanted := make(map[string]AssetEntry, len(m.Entries))
	for _, entry := range m.Entries {
		if entry.Disposition == AssetDispositionIncluded {
			wanted[entry.RelPath] = entry
		}
	}

	found := make(map[string]struct{}, len(wanted))
	walkErr := fs.WalkDir(fsys, assetPackageDirName, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("dbmigrate: 遍历可交付包 %s: %w", name, err)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path.Clean(filepath.ToSlash(name)), assetPackageDirName+"/")
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return fmt.Errorf("%w: 包内出现非常规文件 %s", ErrPackagedDigestMismatch, rel)
		}
		entry, listed := wanted[rel]
		if !listed {
			return fmt.Errorf("%w: 包内出现清单外文件 %s", ErrPackagedDigestMismatch, rel)
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return fmt.Errorf("dbmigrate: 读包内条目 %s: %w", rel, infoErr)
		}
		if info.Size() != entry.Size {
			return fmt.Errorf("%w: %s 的尺寸 %d != 清单 %d", ErrPackagedDigestMismatch, rel, info.Size(), entry.Size)
		}
		sum, sumErr := hashWorkspaceFile(fsys, name)
		if sumErr != nil {
			return sumErr
		}
		if sum != entry.SHA256 {
			return fmt.Errorf("%w: %s 的 SHA-256 与清单不符", ErrPackagedDigestMismatch, rel)
		}
		found[rel] = struct{}{}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	for rel := range wanted {
		if _, ok := found[rel]; !ok {
			return fmt.Errorf("%w: 清单要求入包的 %s 不在包里", ErrPackagedDigestMismatch, rel)
		}
	}
	return nil
}

// WriteAssetManifest 把清单写到目录下的 `assets.json`（先写临时文件再改名，避免半截文件；
// 经 `os.Root` 限定在目录内）。
//
// `db export` 用它落 `pending` 清单（`--seal` 稍后据此封包），封装路径自己会写最终版；
// 两种写法的文件与格式是同一份，不存在"待核查清单"与"正式清单"两套结构。
// WriteAssetManifest persists assets.json; the pending and sealed manifests share one
// format, so --seal revisits exactly the file the export wrote.
func WriteAssetManifest(dir string, m AssetManifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("dbmigrate: 序列化资产清单: %w", err)
	}
	raw = append(raw, '\n')

	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("dbmigrate: 打开导出目录: %w", err)
	}
	defer func() { _ = root.Close() }()

	temp := AssetManifestFileName + ".tmp"
	if err := root.WriteFile(temp, raw, 0o600); err != nil {
		return fmt.Errorf("dbmigrate: 写资产清单: %w", err)
	}
	if err := root.Rename(temp, AssetManifestFileName); err != nil {
		return fmt.Errorf("dbmigrate: 落盘资产清单: %w", err)
	}
	return nil
}

// ReadAssetManifest 读目录下的 `assets.json` 并做与写入端同一套校验：清单缺失、版本不认识、
// 条目乱序或处置非法一律报错——"读不出来"**不得**按空清单继续（那会让 `--seal` 与
// `db switch` 在没有核查依据的情况下放行）。
// ReadAssetManifest loads assets.json under the same validation as the writer; a missing
// or malformed manifest is an error, never an empty fallback.
func ReadAssetManifest(dir string) (AssetManifest, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return AssetManifest{}, fmt.Errorf("dbmigrate: 打开导出目录: %w", err)
	}
	defer func() { _ = root.Close() }()

	raw, err := root.ReadFile(AssetManifestFileName)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return AssetManifest{}, fmt.Errorf("dbmigrate: 导出目录 %s 没有 %s——它不是一份完整的迁移包", dir, AssetManifestFileName)
		}
		return AssetManifest{}, fmt.Errorf("dbmigrate: 读资产清单: %w", err)
	}
	var manifest AssetManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return AssetManifest{}, fmt.Errorf("dbmigrate: 解析资产清单: %w", err)
	}
	if err := validateAssetManifest(manifest); err != nil {
		return AssetManifest{}, err
	}
	return manifest, nil
}

// validateAssetManifest 判清单自身的可接受口径：版本认识、条目按路径严格升序、类别与
// 处置是已知字面量、已封装的清单里每条都要有处置决定。
func validateAssetManifest(m AssetManifest) error {
	if m.FormatVersion != AssetFormatVersion {
		return fmt.Errorf("%w: assets format_version=%d（本工具支持 %d）",
			ErrUnsupportedFormat, m.FormatVersion, AssetFormatVersion)
	}
	if !knownReviewStatus(m.ReviewStatus) {
		return fmt.Errorf("dbmigrate: 资产清单的 review_status=%q 未知", m.ReviewStatus)
	}
	if m.ReviewStatus != AssetReviewNotRequired && m.UnclassifiedDigest == "" {
		return fmt.Errorf("%w: 状态为 %s 却没有未归类摘要", ErrUnclassifiedUnreviewed, m.ReviewStatus)
	}
	for i, entry := range m.Entries {
		if entry.RelPath == "" {
			return errors.New("dbmigrate: 资产清单里存在空路径条目")
		}
		if i > 0 && m.Entries[i-1].RelPath >= entry.RelPath {
			return fmt.Errorf("dbmigrate: 资产清单条目未按 rel_path 严格升序（%q 在 %q 之后）",
				entry.RelPath, m.Entries[i-1].RelPath)
		}
		switch entry.Class {
		case AssetClassBusiness:
			if !knownKind(entry.Kind) {
				return fmt.Errorf("dbmigrate: 业务条目 %q 的类别 %q 未知", entry.RelPath, entry.Kind)
			}
		case AssetClassUnclassified:
		default:
			return fmt.Errorf("dbmigrate: 条目 %q 的 class=%q 未知", entry.RelPath, entry.Class)
		}
		if entry.SHA256 == "" {
			return fmt.Errorf("dbmigrate: 条目 %q 没有 SHA-256", entry.RelPath)
		}
		if entry.Disposition != "" {
			if _, ok := normalizeDisposition(entry.Disposition); !ok {
				return fmt.Errorf("dbmigrate: 条目 %q 的 disposition=%q 未知", entry.RelPath, entry.Disposition)
			}
			if entry.Disposition != AssetDispositionIncluded && strings.TrimSpace(entry.DispositionReason) == "" {
				return fmt.Errorf("dbmigrate: 条目 %q 的 %s 没有写明原因", entry.RelPath, entry.Disposition)
			}
		}
	}
	if m.ReviewStatus == AssetReviewSealed {
		for _, entry := range m.Entries {
			if entry.Disposition == "" {
				return fmt.Errorf("%w: 已封装清单里 %s 没有处置决定", ErrUnclassifiedUnreviewed, entry.RelPath)
			}
		}
	}
	return nil
}

// knownKind 判资产类别是否属于 §3.1.2 的封闭集合。
func knownKind(kind string) bool {
	switch kind {
	case AssetKindAgentDef, AssetKindScript, AssetKindFixture, AssetKindSampleDB,
		AssetKindDocInput, AssetKindReport, AssetKindState:
		return true
	default:
		return false
	}
}

// knownKindList 给错误文案列出可接受的类别。
func knownKindList() string {
	return strings.Join([]string{
		AssetKindAgentDef, AssetKindScript, AssetKindFixture, AssetKindSampleDB,
		AssetKindDocInput, AssetKindReport, AssetKindState,
	}, " / ")
}

// knownReviewStatus 判核查状态是否属于封闭集合。
func knownReviewStatus(status string) bool {
	switch status {
	case AssetReviewNotRequired, AssetReviewPending, AssetReviewSealed:
		return true
	default:
		return false
	}
}

// compareDBFiles 比对源库文件身份：`basalt.db` 与 `-wal` 比存在性 + 尺寸 + SHA-256，
// `-shm` **只比存在性**（派生文件内容随连接变化，不参与判据）。
func compareDBFiles(expected, current []DBFileIdentity) []string {
	if len(expected) == 0 {
		return []string{"清单未记录源库文件身份（basalt.db 的尺寸与 SHA-256 是必需判据）"}
	}
	now := make(map[string]DBFileIdentity, len(current))
	for _, identity := range current {
		now[identity.Name] = identity
	}
	var drift []string
	for _, want := range expected {
		got, listed := now[want.Name]
		if !listed {
			drift = append(drift, fmt.Sprintf("%s：清单里有、复核时读不到", want.Name))
			continue
		}
		if want.Present != got.Present {
			drift = append(drift, fmt.Sprintf("%s：存在性 %t -> %t", want.Name, want.Present, got.Present))
			continue
		}
		if !want.Present || want.Name == "basalt.db-shm" {
			continue
		}
		if want.Size != got.Size || want.SHA256 != got.SHA256 {
			drift = append(drift, fmt.Sprintf("%s：尺寸 / SHA-256 与采集时不同", want.Name))
		}
	}
	return drift
}

// anyWithoutACL 判是否有条目没采集 DACL 摘要（Windows 上即"权限未验证"）。
func anyWithoutACL(entries []AssetEntry) bool {
	for _, entry := range entries {
		if entry.ACLDigest == "" {
			return true
		}
	}
	return false
}

// workspaceIdentifier 用工作区**绝对路径**（Windows 下小写归一）的 SHA-256 标识工作区：
// 两份清单同批次却来自不同工作区时，靠它发现"拿错了目录"。
func workspaceIdentifier(rootDir string) (string, error) {
	absolute, err := filepath.Abs(rootDir)
	if err != nil {
		return "", fmt.Errorf("dbmigrate: 解析工作区绝对路径: %w", err)
	}
	cleaned := filepath.ToSlash(filepath.Clean(absolute))
	if pathCaseInsensitive() {
		cleaned = strings.ToLower(cleaned)
	}
	return sha256Hex([]byte(cleaned)), nil
}

// appendUnique 把值并入切片并去重（保持首次出现的顺序）。
func appendUnique(existing []string, values ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(values))
	for _, value := range existing {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		existing = append(existing, value)
	}
	return existing
}
