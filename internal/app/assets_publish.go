package app

// 本文件是资产 bundle 的**发布面**与**启动自举面**（US-24 颗粒度文档 §2.3.9）。
//
// 发布协议固定三步，全程落在一个管理操作里：
//
//	① 预写 pending `agent_bundle.publish`（预写失败即拒绝执行，宪法 V）；
//	② `EnsureVersion`：按内容哈希幂等建版本行（唯一键冲突**不是**幂等命中）；
//	③ 指针 CAS（`PublishAgentBundle`）：期望不成立 = 丢更新，重读后**有界**重试一次，
//	   仍不成立按版本竞争失败返回——**不得**无条件覆盖别人的发布。
//
// 启动自举只补"共享记录在、本地 hash 目录缺"这一种情形：本地代次必须与共享索引
// **相等**才构建（低了不准入、高了走管理操作单调修复），且构建结果必须与发布记录的
// `content_hash` 逐字相等。首次初始化**不**自动发布——多 Worker 同时首启会各自宣布
// 自己的 `AGENT.md` 为真相，正是共享发布记录要消灭的分歧（§2.3.9 首次初始化）。
//
// This file implements the bundle publication protocol and the startup bootstrap. The
// protocol pre-writes a pending operation, ensures the version by content hash and moves
// the pointer with a compare-and-set; the bootstrap rebuilds only a missing hash
// directory from the shared record, and never publishes anything on first boot.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// resolveAssetBundleRoot 解析 `assets.bundle_root`：空值取缺省（只出现在零值装配里——
// 配置校验已拒绝空串），相对路径按**进程工作目录**展开，与工作区根同一基准。生产与
// CLI 安装因此看到同一个资产根；两处各拼一次会让"装在哪"与"读哪"分叉（FR-24-44）。
// resolveAssetBundleRoot resolves the configured bundle root against the process working
// directory, the very base the workspace uses.
func resolveAssetBundleRoot(cfg *cli.Config, workDir string) string {
	root := ""
	if cfg != nil {
		root = strings.TrimSpace(cfg.Assets.BundleRoot)
	}
	if root == "" {
		root = cli.DefaultAssetsConfig().BundleRoot
	}
	if filepath.IsAbs(root) {
		return filepath.Clean(root)
	}
	return filepath.Join(workDir, filepath.FromSlash(root))
}

// bundlePublishDetail 是 `agent_bundle.publish` 操作行的 `detail_json`（契约 §9.3）：
// 只记 Agent 名、本次 bundle 哈希与发布前的指针值。恢复查证按"指针是否已指向本
// 哈希"判 applied / not_applied，因此不需要任何正文——清单正文在版本行里。
// bundlePublishDetail is the detail_json of a bundle publication: the agent name, the
// bundle hash and the previous pointer. The recovery verifier needs nothing else.
type bundlePublishDetail struct {
	Agent                      string `json:"agent"`
	BundleHash                 string `json:"bundle_hash"`
	PreviousPublishedVersionID int64  `json:"previous_published_version_id"`
}

// bundleRecordStore 是发布链对共享资源记录的最小读取 / 写入面（接口定义在消费方）：
// 生产实现是 `*postgres.ResourceStore`，单元用例用内存夹具替换——没有这条缝，
// "先载荷后指针"只能靠集成库观察，判据就落不到模块卡 §4.3 要求的单元层。
// bundleRecordStore is the consumer-side view of the shared resource record used by the
// publish chain; production passes *postgres.ResourceStore, unit tests a memory fake.
type bundleRecordStore interface {
	AgentIdentity(ctx context.Context, agent string) (core.AgentUID, string, bool, error)
	Pointer(ctx context.Context, kind core.ResourceKind, resourceID string) (core.ResourcePointer, error)
	PointerByUID(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) (core.ResourcePointer, error)
	VersionByID(ctx context.Context, id int64) (core.ResourceVersion, error)
	EnsureVersion(ctx context.Context, v core.ResourceVersion) (core.ResourceVersion, bool, error)
	PublishAgentBundle(ctx context.Context, uid core.AgentUID, versionID, expectedPublished int64,
		operationID string, now time.Time) (bool, error)
}

// bundlePayloadReader 是同哈希发布核对载荷用的只读面：只回答"这一版载荷在不在、
// 自不自洽"。读写分属两个生产实现（`AssetDistributionStore` / `BundlePayloadStore`），
// 因此这里只声明消费方真正用到的方法。
// bundlePayloadReader is the read face the same-hash publication needs to prove a
// payload exists and is self-consistent.
type bundlePayloadReader interface {
	ReadBundlePayload(ctx context.Context, hash string) (core.BundlePayloadRecord, bool, error)
}

// bundlePublisher 是 `assets.Deps.Publish` 的生产实现：装好之后写共享发布记录。
// CLI 安装入口与服务启动路径的装配复用同一份实现——发布只有这一条路。
// bundlePublisher is the single implementation behind every install path.
type bundlePublisher struct {
	versions   bundleRecordStore
	operations core.OperationStore
	payloads   core.BundlePayloadStore
	maxBytes   int64
	logger     *slog.Logger
	now        func() time.Time
}

// bundleFinalizeTimeout 是审计终态回填的独立有界预算（2026-11-10 复核 P1）。
// 外层发布时限（`bundlePublishTimeout`）在回填处被 `context.WithoutCancel` 一并剥掉，
// 数据库阻塞或连接池等待就没有任何上限——发布在编辑器写锁内执行，回填挂住会把保存
// 请求一起占住。超时留 pending 行，由 `verifyBundlePublish` 按"未确认"恢复，因此
// 有界回填失败是安全方向。
// bundleFinalizeTimeout bounds the audit finalization independently of the caller's
// cancellation; a timed-out finalization leaves the pending row for the recovery path.
const bundleFinalizeTimeout = 5 * time.Second

// finalizeContext 为终态回填构造一条**有界**且不受调用方取消影响的上下文：取消不丢
// 终态，但任何数据库 / 连接池等待都不得超过 `bundleFinalizeTimeout`。
// finalizeContext keeps finalization independent of caller cancellation while bounding it.
func (p *bundlePublisher) finalizeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bundleFinalizeTimeout)
}

// newBundlePublisher 构造发布器。仓储必须显式传入：缺接线时 `Publish` 失败关闭，
// **不**退化成"只装本地"（§2.3.9 的首次初始化由显式安装完成，没有旁路）。
// newBundlePublisher wires the publisher; a missing store fails closed instead of
// degrading into a local-only install.
func newBundlePublisher(versions bundleRecordStore, operations core.OperationStore,
	payloads core.BundlePayloadStore, maxBytes int64, logger *slog.Logger, now func() time.Time,
) *bundlePublisher {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &bundlePublisher{
		versions: versions, operations: operations, payloads: payloads,
		maxBytes: maxBytes, logger: logger, now: now,
	}
}

// resolveMaxBundleBytes 取 `assets.max_bundle_bytes`：配置校验已拒绝非正值，零值只
// 出现在零值装配（测试 / 轻路径）里，此时回落缺省上限而不是让载荷编码把一切都判超限。
// resolveMaxBundleBytes falls back to the default limit for a zero-value assembly.
func resolveMaxBundleBytes(cfg *cli.Config) int64 {
	if cfg != nil && cfg.Assets.MaxBundleBytes > 0 {
		return cfg.Assets.MaxBundleBytes
	}
	return cli.DefaultAssetsConfig().MaxBundleBytes
}

// payloadSelfConsistent 复核一份载荷记录是否**自洽**（C15 的读取侧判据）：哈希对得上
// 指针、长度与声明一致、重算的 sha256 与 `payload_sha256` 逐字相等。不做这一步时，
// 「载荷存在」只证明行在，证明不了字节就是发布时那一份。
// payloadSelfConsistent re-derives the stored payload digest; an existing row alone does
// not prove the bytes are the ones that were published.
func payloadSelfConsistent(rec core.BundlePayloadRecord, hash string) bool {
	if rec.BundleHash != hash || rec.PayloadSHA256 == "" || len(rec.Payload) == 0 {
		return false
	}
	if int64(len(rec.Payload)) != rec.SizeBytes {
		return false
	}
	sum := sha256.Sum256(rec.Payload)
	return hex.EncodeToString(sum[:]) == rec.PayloadSHA256
}

// writePayload 把已安装目录编码成确定性载荷并写进内容寻址载荷表（§2.3.2 的冻结字面量）。
// 失败一律上抛：调用方据此拒绝推进指针——"只装本地不写载荷"正是 C15 要消灭的形态。
// CreatedAt 由 Go 写毫秒值（表不设数据库 DEFAULT）；上限语义由 `EncodeBundle` 判，
// 超限返回错误而不截断。
// writePayload encodes the installed directory and inserts the content-addressed payload;
// every failure is raised so the caller refuses to move the pointer.
func writePayload(ctx context.Context, dir string, m assets.Manifest,
	store core.BundlePayloadStore, maxBytes int64,
) error {
	if store == nil {
		return errors.New("资产载荷未接线：缺载荷写入仓储（不得只装本地）")
	}
	if maxBytes <= 0 {
		return errors.New("资产载荷未接线：缺少 max_bundle_bytes 上限（装配错误）")
	}
	payload, sum, err := assets.EncodeBundle(ctx, dir, maxBytes)
	if err != nil {
		return fmt.Errorf("编码 Agent %s 的资产载荷: %w", m.Agent, err)
	}
	if err := store.PutBundlePayload(ctx, core.BundlePayloadRecord{
		BundleHash:    m.BundleHash,
		Agent:         m.Agent,
		AgentUID:      m.AgentUID,
		FormatVersion: m.FormatVersion,
		Payload:       payload,
		PayloadSHA256: sum,
		SizeBytes:     int64(len(payload)),
		CreatedAt:     time.Now().UnixMilli(),
	}); err != nil {
		return fmt.Errorf("写入 Agent %s 的资产载荷: %w", m.Agent, err)
	}
	return nil
}

// workspaceBundleDeps 是"从工作区构建一个 Agent bundle"的输入：工作区根（配置文件的
// 来源基准）与共享发布记录（技能版本的唯一来源）。它只读工作区、只写暂存目录，
// **不**碰共享记录——发布仍只由 `bundlePublisher` 一处完成。
// workspaceBundleDeps carries the build inputs; it never touches the shared record,
// which only bundlePublisher writes.
type workspaceBundleDeps struct {
	Root     string
	Versions bundleRecordStore
}

// resolveWorkspaceAgentUID 取 Agent 在生命周期索引里的身份（构建清单的 `agent_uid`）。
// 索引没有身份即拒绝：没有身份的 Agent 进不了共享发布记录，构建出来也无处发布。
// resolveWorkspaceAgentUID refuses an agent the lifecycle index has no identity for.
func resolveWorkspaceAgentUID(ctx context.Context, versions bundleRecordStore, agent string) (core.AgentUID, error) {
	if err := workspace.ValidateAgentName(agent); err != nil {
		return "", &core.InvalidInputError{Tool: "assets", Reason: err.Error()}
	}
	uid, _, ok, err := versions.AgentIdentity(ctx, agent)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("构建 Agent %s 的资产: 生命周期索引里没有身份（基线尚未写下该 Agent）", agent)
	}
	if _, err := core.ParseAgentUID(string(uid)); err != nil {
		return "", fmt.Errorf("构建 Agent %s 的资产: %w", agent, err)
	}
	return uid, nil
}

// buildWorkspaceBundle 从工作区构建一个 Agent 的 bundle 到 `dest` 并返回清单。
//
// 配置文件一律用**工作区根相对路径**：`agents/<agent>/AGENT.md` 加 `bootstrap:` 声明的
// 条目。这个基准与执行期读取面（`workspace.SnapshotBundle` → `assets.BundleReader`）
// 是同一份——两边各拼一次前缀会让"装进 bundle 的"与"执行时读出的"不是同一批文件
// （FR-24-44 的逐字一致同样覆盖包内布局）。技能取共享发布记录的当前指针，逐文件按
// 版本行的清单比对后入包；准入侧用同一份清单做交叉校验（`checkBundleSkills`）。
//
// `config_hash` 取定义文件的 sha256：准入不比对它，但清单要求非空，写常量会让"不同
// 内容却同哈希"看起来像真的；定义之外的其他配置文件由 `config_files` 的逐项哈希覆盖，
// 因此 bundle hash 仍然钉住全部配置内容。
//
// buildWorkspaceBundle builds one agent's bundle from the workspace into dest. Config
// paths are workspace-root-relative, the very base the execution-time reader uses.
func buildWorkspaceBundle(ctx context.Context, deps workspaceBundleDeps, agent string,
	uid core.AgentUID, dest string,
) (assets.Manifest, error) {
	if strings.TrimSpace(deps.Root) == "" {
		return assets.Manifest{}, errors.New("构建 Agent bundle: 工作区根为空")
	}
	if deps.Versions == nil {
		return assets.Manifest{}, errors.New("构建 Agent bundle: 技能读取未接资源仓储")
	}
	definitionRel := path.Join(agentsDirName, agent, agentDefinitionFileName)
	definition, err := readWorkspaceFile(deps.Root, definitionRel)
	if err != nil {
		return assets.Manifest{}, fmt.Errorf("读取 Agent %s 的定义文件: %w", agent, err)
	}
	profile, err := workspace.ProfileFromDefinition(definition)
	if err != nil {
		return assets.Manifest{}, fmt.Errorf("解析 Agent %s 的定义文件: %w", agent, err)
	}
	configFiles, err := bundleConfigFiles(agent, definitionRel, profile.Bootstrap)
	if err != nil {
		return assets.Manifest{}, err
	}
	skills, err := bundleSkillSources(ctx, deps, profile.Skills)
	if err != nil {
		return assets.Manifest{}, err
	}
	sum := sha256.Sum256(definition)
	return assets.Build(ctx, assets.BuildSpec{
		Agent:       agent,
		AgentUID:    uid,
		ConfigHash:  hex.EncodeToString(sum[:]),
		ConfigRoot:  deps.Root,
		ConfigFiles: configFiles,
		Skills:      skills,
		Dest:        dest,
	})
}

// readWorkspaceFile 经**工作区根句柄**读取一个工作区相对路径（`os.Root`）：读取不可能
// 落进工作区之外，这也是 gosec G304 的消除方式，而不是加抑制注释。
// readWorkspaceFile reads through a workspace-root handle; nothing can escape it.
func readWorkspaceFile(root, rel string) ([]byte, error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区根 %s: %w", root, err)
	}
	data, readErr := base.ReadFile(filepath.FromSlash(rel))
	closeErr := base.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("关闭工作区根 %s: %w", root, closeErr)
	}
	return data, nil
}

// bundleConfigFiles 组装清单的配置文件路径：定义文件在前，`bootstrap:` 条目按声明序
// 追加，重复项只留第一次出现（清单要求路径唯一；排序由 `assets.Build` 的规整化统一做）。
// 反斜杠换成正斜杠：清单与执行期读取都只认正斜杠形态的工作区相对路径。
// bundleConfigFiles assembles the manifest's config paths in workspace-root-relative
// form, dropping duplicates because the manifest requires unique paths.
func bundleConfigFiles(agent, definitionRel string, bootstrap []string) ([]string, error) {
	files := []string{definitionRel}
	seen := map[string]bool{definitionRel: true}
	for _, declared := range bootstrap {
		rel := path.Clean(filepath.ToSlash(strings.TrimSpace(declared)))
		if rel == "" || rel == "." {
			return nil, fmt.Errorf("agent %s 的 bootstrap 声明了空路径", agent)
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		files = append(files, rel)
	}
	return files, nil
}

// bundleSkillSources 把 `skills:` 声明的每个技能解析成"发布版本 + 磁盘目录"：版本与
// 文件清单取共享发布记录（与准入同一个 `resolvePublishedSkill`），目录取版本行的
// `storage_path`（约束 31 的契约字面量，相对工作区根）。
// bundleSkillSources pairs each declared skill's published version with its on-disk
// directory, using the same publication read as admission.
func bundleSkillSources(ctx context.Context, deps workspaceBundleDeps, names []string) ([]assets.SkillSource, error) {
	if len(names) == 0 {
		return nil, nil
	}
	sources := make([]assets.SkillSource, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		ref, version, err := resolvePublishedSkill(ctx, deps.Versions, name)
		if err != nil {
			return nil, fmt.Errorf("构建技能 %s: %w", name, err)
		}
		relDir, err := workspace.PublishedVersionRelDir(version.StoragePath)
		if err != nil {
			return nil, fmt.Errorf("技能 %s 的版本目录: %w", name, err)
		}
		sources = append(sources, assets.SkillSource{
			Ref:  ref,
			Root: filepath.Join(deps.Root, filepath.FromSlash(relDir)),
		})
	}
	return sources, nil
}

// workspacePublishDeps 是"从工作区构建 → 安装 → 发布"这条完整链路的输入。它与
// `workspaceBundleDeps` 的分工是：后者只读工作区、只写暂存目录，本类型多出的资产根与
// 操作仓储只被 `assets.Install` 与 `bundlePublisher` 消费——发布协议仍然只有一处。
// workspacePublishDeps carries the full build-install-publish chain; the write of the
// shared record still happens only inside bundlePublisher.
type workspacePublishDeps struct {
	Root         string
	BundleRoot   string
	Versions     bundleRecordStore
	Operations   core.OperationStore
	Payloads     bundlePayloadReader     // 同哈希核对载荷存在性（§2.3.2 的 C15 修复面）
	PayloadStore core.BundlePayloadStore // 发布路径的载荷写入面（writePayload 的唯一入口）
	MaxBytes     int64                   // 载荷上限（assets.max_bundle_bytes）
	// Actor 是触发本次发布的真实操作主体（HTTP 会话身份或 CLI 身份）。零值表示
	// "装配面没有给出"，由发布器回落为 CLI 身份——只允许出现在离线安装路径。
	// Actor is the real operation identity behind this publication; the zero value
	// falls back to the CLI identity for offline install paths only.
	Actor  core.Actor
	Logger *slog.Logger
	Now    func() time.Time
}

// publishedBundleMatchesHash 报告共享指针是否已指向**同内容**的 bundle 版本，并在是时
// 返回该版本号。写路径每次成功都要发布，但"同一份定义再提交一次"（例如只改了回显字段
// 或连续两次启用）不应堆积第二条 `agent_bundle.publish`：内容寻址让"同哈希"就是
// "同一份资产"的判据，无需另设去重键。
// publishedBundleMatchesHash reports whether the pointer already names a version with the
// same content hash, so repeated writes of identical content add no operation row.
func publishedBundleMatchesHash(ctx context.Context, versions bundleRecordStore,
	uid core.AgentUID, hash string,
) (int64, bool, error) {
	pointer, err := versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(uid))
	switch {
	case errors.Is(err, core.ErrResourceNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	if pointer.DeletedAt != nil || pointer.PublishedVersionID <= 0 {
		// 软删除的指针不在这里裁决：交给 bundlePublisher 按发布协议拒绝（ErrAssetUnavailable）。
		return 0, false, nil
	}
	version, err := versions.VersionByID(ctx, pointer.PublishedVersionID)
	switch {
	case errors.Is(err, core.ErrResourceNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, err
	}
	if version.ContentHash != hash {
		return 0, false, nil
	}
	return version.ID, true, nil
}

// ensureInstalledPayload 是同哈希发布的**载荷核对**（§2.3.2 的 C15 修复面）：载荷存在
// 且自洽即真幂等返回；缺失（或内容不自洽）只补写载荷、**不动指针**——同哈希发布不是
// 一次新发布，因此不预写操作行，也就没有可回填的 failed 终态。
// 内容不自洽的行不会在这里被覆盖：`PutBundlePayload` 对同哈希不同载荷按明确错误拒绝
// （fail-closed，留给人处置）。
// ensureInstalledPayload proves the payload exists for a same-hash publication and
// backfills a missing one without moving the pointer or opening an operation row.
func ensureInstalledPayload(ctx context.Context, deps workspacePublishDeps, installed assets.Installed) error {
	if deps.Payloads == nil || deps.PayloadStore == nil {
		return errors.New("资产载荷未接线：同哈希发布必须核对载荷，缺读取面或写入面")
	}
	hash := installed.Manifest.BundleHash
	rec, ok, err := deps.Payloads.ReadBundlePayload(ctx, hash)
	if err != nil {
		return fmt.Errorf("核对 Agent %s 的资产载荷: %w", installed.Manifest.Agent, err)
	}
	if ok && payloadSelfConsistent(rec, hash) {
		return nil
	}
	if err := writePayload(ctx, installed.Dir, installed.Manifest, deps.PayloadStore,
		deps.MaxBytes); err != nil {
		return fmt.Errorf("补写 Agent %s 的资产载荷（指针不变）: %w", installed.Manifest.Agent, err)
	}
	return nil
}

// publishWorkspaceAgentBundle 把工作区里的一个 Agent 定义构建、安装并发布为共享资产
// 记录，返回本次发布的版本号。构建 → 安装 → (同哈希则短路) → 发布：安装目录始终由
// `assets.Install` 按内容寻址复制，失败逐层上抛，**不回退**成"只装本地"。
//
// 控制台写路径、CLI 写路径、评测基座与 `basalt assets install --agent` 共用这一份
// 实现：同一个协议，没有第二条发布路径。
// The single build-install-publish chain shared by the console/CLI write paths, the eval
// base and the `install --agent` command.
func publishWorkspaceAgentBundle(ctx context.Context, deps workspacePublishDeps,
	agent string,
) (int64, error) {
	if deps.Versions == nil || deps.Operations == nil || deps.Root == "" || deps.BundleRoot == "" {
		return 0, errors.New("资产发布未接线：缺共享发布记录、操作仓储、工作区根或资产根")
	}
	uid, err := resolveWorkspaceAgentUID(ctx, deps.Versions, agent)
	if err != nil {
		return 0, err
	}
	staging, err := os.MkdirTemp("", "basalt-publish-bundle-")
	if err != nil {
		return 0, fmt.Errorf("创建资产构建暂存目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }() // 构建结果由 Install 复制，暂存目录用毕即删

	manifest, err := buildWorkspaceBundle(ctx,
		workspaceBundleDeps{Root: deps.Root, Versions: deps.Versions}, agent, uid, staging)
	if err != nil {
		return 0, err
	}
	installed, err := assets.Install(ctx, staging, deps.BundleRoot)
	if err != nil {
		return 0, fmt.Errorf("安装 Agent %s 的资产 bundle: %w", agent, err)
	}
	// 指针已是同内容版本：本地目录刚被 Install 幂等重建，共享记录不需要新操作行；
	// 但载荷仍须核对——存量指针（US-25 及以前发布）没有载荷，缺失时在这里补写。
	if versionID, same, err := publishedBundleMatchesHash(ctx, deps.Versions, uid,
		manifest.BundleHash); err != nil {
		return 0, err
	} else if same {
		if err := ensureInstalledPayload(ctx, deps, installed); err != nil {
			return 0, err
		}
		return versionID, nil
	}
	publisher := newBundlePublisher(deps.Versions, deps.Operations, deps.PayloadStore,
		deps.MaxBytes, deps.Logger, deps.Now)
	return publisher.publishAs(ctx, installed, deps.Actor)
}

// PublishWorkspaceAgentBundle 把工作区里的一个 Agent 定义构建、安装并发布为共享资产
// 记录，返回本次发布的版本号；发布指针已指向可用版本时按幂等短路返回它。
//
// 这是**包外装配消费者**（评测基座）的入口：生产运维走 `basalt assets install --agent`
// 与控制台 / CLI 写路径，它们最终都落到同一个 `publishWorkspaceAgentBundle`——本方法
// 不新增发布协议，只在指针已存在时保留"先发布一次即可"的旧语义。
//
// The out-of-package entry used by the eval base. It keeps the "published once already"
// short-circuit and delegates the rest to the shared chain.
func (r *Runtime) PublishWorkspaceAgentBundle(ctx context.Context, agent string) (int64, error) {
	if r == nil || r.inner == nil {
		return 0, errors.New("资产发布未接线：运行期句柄为空")
	}
	rt := r.inner
	if rt.versions == nil || rt.ops == nil || rt.db == nil || rt.root == "" || rt.bundleRoot == "" {
		return 0, errors.New("资产发布未接线：缺共享发布记录、操作仓储、数据库句柄、工作区根或资产根")
	}
	uid, err := resolveWorkspaceAgentUID(ctx, rt.versions, agent)
	if err != nil {
		return 0, err
	}
	pointer, err := rt.versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(uid))
	switch {
	case err == nil && pointer.DeletedAt == nil:
		return pointer.PublishedVersionID, nil // 已发布：重复调用按幂等短路
	case err != nil && !errors.Is(err, core.ErrResourceNotFound):
		// 软删除的指针同样落到这里之后的发布路径：是否允许由 bundlePublisher 统一裁决。
		return 0, err
	}
	return publishWorkspaceAgentBundle(ctx, workspacePublishDeps{
		Root:         rt.root,
		BundleRoot:   rt.bundleRoot,
		Versions:     rt.versions,
		Operations:   rt.ops,
		Payloads:     postgres.NewAssetDistributionStore(rt.db),
		PayloadStore: postgres.NewBundlePayloadStore(rt.db),
		MaxBytes:     resolveMaxBundleBytes(rt.cfg),
		Logger:       nil,
		Now:          time.Now,
	}, agent)
}

// Publish 是 `assets.Deps.Publish` 的生产实现（离线安装 / CLI 路径）：操作主体按
// CLI 固定身份归因。HTTP 写路径必须走 `publishAs` 透传真实操作主体。
// Publish is the offline install path's entry and attributes the operation to the CLI
// identity; HTTP write paths must go through publishAs with the real actor.
func (p *bundlePublisher) Publish(ctx context.Context, installed assets.Installed) (int64, error) {
	return p.publishAs(ctx, installed, core.Actor{})
}

// publishAs 把一个**已安装**的 bundle 写进共享发布记录并推进指针，返回本次发布的
// `published_version_id`。所有失败路径都把预写的 pending 行回填 failed 后原样上抛；
// 唯一"重试"是 CAS 期望不成立时的重读重试，且**有界**（一次）。
// 零值 actor 回落为 CLI 身份（仅离线安装路径），非零 actor 原样记入操作行。
// publishAs writes the installed bundle into the shared record and moves the pointer;
// every failure path closes the pending row as failed. A zero actor falls back to the
// CLI identity for offline installs.
func (p *bundlePublisher) publishAs(ctx context.Context, installed assets.Installed, actor core.Actor) (int64, error) {
	if p == nil || p.versions == nil || p.operations == nil || p.payloads == nil {
		return 0, errors.New("资产发布未接线：缺资源仓储、操作仓储或载荷仓储（不得只装本地）")
	}
	manifest := installed.Manifest
	uid, err := core.ParseAgentUID(string(manifest.AgentUID))
	if err != nil {
		return 0, fmt.Errorf("发布 Agent 资产: %w", err)
	}
	if manifest.Agent == "" || manifest.BundleHash == "" {
		return 0, errors.New("发布 Agent 资产: 清单缺 agent 或 bundle_hash")
	}
	// 目录必须就是内容寻址目录（FR-24-44）：版本行 `storage_path` 原样记它，执行期
	// 只按这个值读，因此"装在哪"与"记成哪"不允许出现第二种说法。
	if installed.Dir == "" || filepath.Base(installed.Dir) != manifest.BundleHash {
		return 0, fmt.Errorf("%w: 安装目录 %q 不是 bundle %s 的内容寻址目录",
			core.ErrAssetVersionMismatch, installed.Dir, manifest.BundleHash)
	}

	now := p.now()
	operationID := core.NewOperationID(now)
	previous, err := p.previousPublished(ctx, uid)
	if err != nil {
		return 0, err
	}
	detail, err := json.Marshal(bundlePublishDetail{
		Agent:                      manifest.Agent,
		BundleHash:                 manifest.BundleHash,
		PreviousPublishedVersionID: previous,
	})
	if err != nil {
		return 0, fmt.Errorf("序列化资产发布详情: %w", err)
	}

	if actor.Subject == "" {
		// 零值只在离线安装路径出现：按 CLI 固定身份归因，不编造会话主体。
		actor = cliWriteActor(p.logger)
	}
	if err := p.operations.BeginOperation(ctx, core.OperationRecord{
		OperationID:  operationID,
		Action:       core.ActionAgentBundlePublish,
		TargetKind:   core.TargetKindAgentBundle,
		TargetID:     string(uid),
		ActorSubject: actor.Subject,
		AuthMethod:   actor.Method,
		Source:       actor.Source,
		RequestHash: agentRequestHash(strings.Join([]string{
			core.ActionAgentBundlePublish, string(uid), manifest.BundleHash,
		}, "|")),
		Status:     core.OperationPending,
		DetailJSON: detail,
		CreatedAt:  now,
	}); err != nil {
		// 预写失败 = 拒绝执行：本地目录可以先行就位（无人引用即无害），但不写共享记录。
		p.logger.Error("资产发布预写操作记录失败，拒绝发布",
			"agent", manifest.Agent, "bundle", manifest.BundleHash, "error", err)
		return 0, fmt.Errorf("资产发布预写操作记录失败，拒绝发布: %w", err)
	}

	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		p.finishFailed(ctx, operationID, "序列化 bundle 清单失败", detail)
		return 0, fmt.Errorf("序列化 bundle 清单: %w", err)
	}
	version, _, err := p.versions.EnsureVersion(ctx, core.ResourceVersion{
		Kind:         core.ResourceAgentBundle,
		ResourceUID:  core.ResourceUID(uid),
		ResourceID:   string(uid),
		ContentHash:  manifest.BundleHash,
		ManifestJSON: string(manifestJSON),
		StoragePath:  installed.Dir,
		CreatedAt:    now.UnixMilli(),
		OperationID:  operationID,
	})
	if err != nil {
		p.finishFailed(ctx, operationID, "写入资产版本行失败", detail)
		return 0, fmt.Errorf("写入资产版本行: %w", err)
	}

	// 载荷严格先于指针 CAS（C15）：载荷落库失败时指针一步不动，pending 行回填 failed。
	// The payload is written strictly before the pointer CAS, so a payload failure
	// never leaves a pointer whose payload is missing.
	if err := writePayload(ctx, installed.Dir, manifest, p.payloads, p.maxBytes); err != nil {
		p.finishFailed(ctx, operationID, err.Error(), detail)
		return 0, err
	}

	published, err := p.compareAndSet(ctx, uid, version.ID, previous, operationID, now)
	if err != nil {
		p.finishFailed(ctx, operationID, err.Error(), detail)
		return 0, err
	}
	if !published {
		p.finishFailed(ctx, operationID, "发布指针期望不成立（版本竞争），未覆盖他人发布", detail)
		return 0, fmt.Errorf("发布 Agent %s 的资产 bundle %s: 指针期望不成立（版本竞争）",
			manifest.Agent, manifest.BundleHash)
	}
	return version.ID, nil
}

// compareAndSet 执行指针 CAS；期望不成立时重读指针并**再试一次**（有界）。重读后
// 指针已指向本版本时视为成功（另一个 Worker 刚发布了同一份内容），由本函数回填终态
// ——失败的 CAS 事务整体回滚，不会替我们收尾。
// compareAndSet moves the pointer with one bounded retry after re-reading it. When the
// re-read already points at this version, the failed CAS rolled back, so this function
// closes the operation itself.
func (p *bundlePublisher) compareAndSet(ctx context.Context, uid core.AgentUID,
	versionID, expected int64, operationID string, now time.Time,
) (bool, error) {
	applied, err := p.versions.PublishAgentBundle(ctx, uid, versionID, expected, operationID, now)
	if err != nil || applied {
		return applied, err
	}
	current, err := p.previousPublished(ctx, uid)
	if err != nil {
		return false, err
	}
	if current == versionID {
		// 指针已指向本版本：同一内容的并发发布，不是丢更新。成功事务属于对方，
		// 我们的 pending 行仍待回填——回填同样走独立有界预算，不得随 `WithoutCancel`
		// 一起无限等待。
		finalizeCtx, cancel := p.finalizeContext(ctx)
		if finishErr := p.operations.FinishOperation(finalizeCtx, operationID,
			core.OperationSuccess, lifecycleResultOK, "", nil, p.now()); finishErr != nil {
			p.logger.Error("回填资产发布终态失败", "operation_id", operationID, "error", finishErr)
		}
		cancel()
		return true, nil
	}
	return p.versions.PublishAgentBundle(ctx, uid, versionID, current, operationID, now)
}

// previousPublished 读当前发布指针；指针缺失返回 0（首次初始化走 INSERT 语义）。
// 已软删除的指针**拒绝**发布：那需要显式管理动作，安装不得静默复活一个已删除的指针。
// previousPublished returns 0 for a missing pointer; a soft-deleted pointer is refused.
func (p *bundlePublisher) previousPublished(ctx context.Context, uid core.AgentUID) (int64, error) {
	pointer, err := p.versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(uid))
	if errors.Is(err, core.ErrResourceNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("读取 Agent %s 的资产发布指针: %w", uid, err)
	}
	if pointer.DeletedAt != nil {
		return 0, fmt.Errorf("%w: Agent %s 的资产发布指针已删除", core.ErrAssetUnavailable, uid)
	}
	return pointer.PublishedVersionID, nil
}

// finishFailed 回填 failed 终态（独立于调用方取消：审计终态不能因请求取消而丢；
// 同时受 `bundleFinalizeTimeout` 约束：数据库 / 连接池等待不得无限挂住写锁）。
// finishFailed closes the pending row independently of caller cancellation, within a
// bounded budget so a stuck store cannot pin the editor's write lock.
func (p *bundlePublisher) finishFailed(ctx context.Context, operationID, message string, detail []byte) {
	finalizeCtx, cancel := p.finalizeContext(ctx)
	defer cancel()
	if err := p.operations.FinishOperation(finalizeCtx, operationID,
		core.OperationFailed, lifecycleResultInternal, message, detail, p.now()); err != nil {
		p.logger.Error("回填资产发布失败终态失败", "operation_id", operationID, "error", err)
	}
}

// verifyBundlePublish 查证崩溃留下的 `agent_bundle.publish` pending 行（契约 §9.4）：
// 版本行插入与指针 CAS 分属两个事务，所以"版本行在、指针没动"能且只能判 not_applied
// ——发布成立的判据是**指针已指向本哈希**，**不**重放安装或发布。
// verifyBundlePublish decides a crashed publication from the pointer alone; a version row
// without the pointer move is provably not published, and nothing is ever replayed.
func verifyBundlePublish(ctx context.Context, deps RecoveryDeps, op core.OperationRecord) string {
	if deps.Resources == nil || op.TargetKind != core.TargetKindAgentBundle {
		return core.VerificationIndeterminate
	}
	var detail bundlePublishDetail
	if len(op.DetailJSON) > 0 {
		if err := json.Unmarshal(op.DetailJSON, &detail); err != nil {
			return core.VerificationIndeterminate
		}
	}
	if op.TargetID == "" || detail.BundleHash == "" {
		return core.VerificationIndeterminate
	}
	versions, err := deps.Resources.Versions(ctx, core.ResourceAgentBundle, op.TargetID)
	if err != nil {
		return core.VerificationIndeterminate
	}
	// 按内容哈希认领版本行：`EnsureVersion` 命中既有同哈希行时，那一行的 operation_id
	// 属于更早一次发布，按操作号找不到它。
	var published core.ResourceVersion
	found := false
	for _, version := range versions {
		if version.ContentHash == detail.BundleHash {
			published, found = version, true
			break
		}
	}
	pointer, pointerErr := deps.Resources.Pointer(ctx, core.ResourceAgentBundle, op.TargetID)
	switch {
	case errors.Is(pointerErr, core.ErrResourceNotFound):
		// 指针不存在：首次初始化没有落库，可证明未发布。
		return core.VerificationNotApplied
	case pointerErr != nil:
		return core.VerificationIndeterminate
	case pointer.DeletedAt != nil:
		// 指针事后被删除：无法区分删除发生在本操作之前还是之后。
		return core.VerificationIndeterminate
	}
	if found && pointer.PublishedVersionID == published.ID {
		return core.VerificationApplied
	}
	if pointer.PublishedVersionID == detail.PreviousPublishedVersionID {
		return core.VerificationNotApplied
	}
	return core.VerificationIndeterminate
}

// assetBootstrapRecords 是自举与存量载荷回填共用的共享记录读取面（消费方接口；
// `*postgres.ResourceStore` 是唯一实现）。它只有读：指针、版本行与生命周期索引，
// 两条启动路径都不写共享记录。
// assetBootstrapRecords is the read-only shared-record face shared by the startup
// bootstrap and the legacy payload backfill.
type assetBootstrapRecords interface {
	PointerByUID(ctx context.Context, kind core.ResourceKind, uid core.ResourceUID) (core.ResourcePointer, error)
	VersionByID(ctx context.Context, id int64) (core.ResourceVersion, error)
	ReadLifecycleIndex(ctx context.Context, agent string) (core.LifecycleRecord, bool, string, error)
}

// assetBootstrapDeps 是启动自举的全部依赖（§2.3.9"启动自举"）：工作区根、资产根、
// 共享发布记录、生命周期文件与索引、活动 Agent 清单。它只读共享记录、只写本地 hash
// 目录——**不**发布、**不**改索引。
// assetBootstrapDeps carries the bootstrap inputs; the bootstrap writes only local
// hash directories and never publishes or touches the index.
type assetBootstrapDeps struct {
	Root      string
	AssetRoot string
	Versions  assetBootstrapRecords
	Lifecycle *workspace.LifecycleStore
	Agents    []core.AgentEntry
	Logger    *slog.Logger
}

// bootstrapAgentBundles 按共享发布记录补齐缺失的 bundle 目录（§2.3.9 启动自举）。
//
// 判据（三条同时成立才构建）：
//   - 本地生命周期代次与共享索引**相等**（低了不准入、高了走管理操作单调修复）；
//   - 指针指向的版本行存在且 `manifest_json` 可解析、身份与本 Agent 一致；
//   - 构建结果的 `BundleHash` 与版本行的 `content_hash` 逐字相等。
//
// 任何一条不成立都只记 WARN 并跳过：单个 Agent 的资产问题**不得**阻塞启动，也
// **不得**用本地内容覆盖共享记录。已有 hash 目录一律不动（资产不可变）。
// bootstrapAgentBundles rebuilds a missing directory only when the shared record and the
// local generation agree; every other case logs a warning and steps over it.
func bootstrapAgentBundles(ctx context.Context, deps assetBootstrapDeps) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if deps.Versions == nil || deps.Lifecycle == nil || deps.Root == "" || deps.AssetRoot == "" {
		return
	}
	for _, entry := range deps.Agents {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := bootstrapOneBundle(ctx, deps, logger, entry); err != nil {
			// 自举失败不是启动失败：该 Agent 的准入会以 ASSET_UNAVAILABLE 失败关闭。
			logger.Warn("启动自举：按共享发布记录补齐资产目录失败，跳过该 Agent",
				"agent", entry.Name, "error", err)
		}
	}
}

// bootstrapOneBundle 处理单个 Agent；返回 nil 表示无需处理或已完成。
// bootstrapOneBundle handles one agent; a nil error means nothing was needed.
func bootstrapOneBundle(ctx context.Context, deps assetBootstrapDeps,
	logger *slog.Logger, entry core.AgentEntry,
) error {
	record, exists, err := deps.Lifecycle.Read(ctx, entry.Name)
	if err != nil || !exists {
		return nil // 状态文件缺失 / 不可读：基线已按各自口径处置，资产自举不插手
	}
	uid, err := core.ParseAgentUID(string(record.AgentUID))
	if err != nil {
		return nil // 无身份的记录进不了共享发布记录
	}
	index, ok, _, err := deps.Versions.ReadLifecycleIndex(ctx, entry.Name)
	if err != nil {
		return err
	}
	if !ok || index.Generation != record.Generation {
		// 代次不等（任一方向）不构建：本地高代次不写索引、由管理操作单调修复；
		// 本地低代次本来就不该准入（P-17 的同一判据）。
		return nil
	}
	pointer, err := deps.Versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(uid))
	if errors.Is(err, core.ErrResourceNotFound) {
		return nil // 没有共享发布记录：首次初始化由显式安装完成，首启不自动发布
	}
	if err != nil {
		return err
	}
	if pointer.DeletedAt != nil {
		return nil
	}
	version, err := deps.Versions.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return err
	}
	expected := strings.TrimSpace(version.ContentHash)
	if expected == "" {
		return fmt.Errorf("资产版本 %d 没有内容哈希", version.ID)
	}
	if _, err := assets.Resolve(ctx, deps.AssetRoot, entry.Name, expected); err == nil {
		return nil // 目录已在且可复算：不动它
	} else if !errors.Is(err, core.ErrAssetUnavailable) {
		// 目录在但哈希 / 身份对不上：不覆盖（资产不可变），留给人工处置。
		return err
	}

	staging, err := os.MkdirTemp("", "basalt-asset-bootstrap-")
	if err != nil {
		return fmt.Errorf("创建构建暂存目录: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }() // 构建结果由 Install 复制，暂存目录用毕即删

	manifest, err := buildBundleFromRecord(ctx, deps, entry.Name, uid, expected, version, staging)
	if err != nil {
		return err
	}
	installed, err := assets.Install(ctx, staging, deps.AssetRoot)
	if err != nil {
		return err
	}
	logger.Warn("启动自举：按共享发布记录重建了缺失的资产目录（bundle.json 原缺失）",
		"agent", entry.Name, "bundle", manifest.BundleHash, "dir", installed.Dir)
	return nil
}

// buildBundleFromRecord 从共享发布记录把 bundle 构建到 `dest` 并返回清单；构建结果的
// 哈希必须与发布记录逐字相等，否则拒绝（本地内容与记录不符不得装机）。
// It builds the bundle from the shared record into dest and refuses a result whose hash
// differs from the published one.
func buildBundleFromRecord(ctx context.Context, deps assetBootstrapDeps, agent string,
	uid core.AgentUID, expected string, version core.ResourceVersion, dest string,
) (assets.Manifest, error) {
	var recorded assets.Manifest
	if err := json.Unmarshal([]byte(version.ManifestJSON), &recorded); err != nil {
		return assets.Manifest{}, fmt.Errorf("解析已发布 bundle 清单: %w", err)
	}
	if recorded.Agent != agent || recorded.AgentUID != uid || recorded.BundleHash != expected {
		return assets.Manifest{}, fmt.Errorf("%w: 已发布清单身份不符（agent=%s uid=%s hash=%s）",
			core.ErrAssetVersionMismatch, recorded.Agent, recorded.AgentUID, recorded.BundleHash)
	}
	configFiles := make([]string, 0, len(recorded.ConfigFiles))
	for _, file := range recorded.ConfigFiles {
		configFiles = append(configFiles, file.Path)
	}
	skillSources := make([]assets.SkillSource, 0, len(recorded.Skills))
	for _, ref := range recorded.Skills {
		skillVersion, err := deps.Versions.VersionByID(ctx, ref.VersionID)
		if err != nil {
			return assets.Manifest{}, fmt.Errorf("读取技能 %s 的发布版本 %d: %w", ref.Name, ref.VersionID, err)
		}
		if skillVersion.Kind != core.ResourceSkill {
			return assets.Manifest{}, fmt.Errorf("技能 %s 的版本 %d 归属不符（got %s）",
				ref.Name, ref.VersionID, skillVersion.Kind)
		}
		relDir, err := workspace.PublishedVersionRelDir(skillVersion.StoragePath)
		if err != nil {
			return assets.Manifest{}, fmt.Errorf("技能 %s 的版本目录: %w", ref.Name, err)
		}
		skillSources = append(skillSources, assets.SkillSource{
			Ref:  ref,
			Root: filepath.Join(deps.Root, filepath.FromSlash(relDir)),
		})
	}

	built, err := assets.Build(ctx, assets.BuildSpec{
		Agent:      agent,
		AgentUID:   uid,
		ConfigHash: recorded.ConfigHash,
		// 记录里的配置路径是**工作区根相对**形态（`agents/<agent>/AGENT.md` 与
		// bootstrap 条目），构建来源因此也是工作区根：换一个基准会让自举读到别的
		// 文件，或干脆找不到文件。
		// The recorded paths are workspace-root-relative, so the source root is too.
		ConfigRoot:  deps.Root,
		ConfigFiles: configFiles,
		Skills:      skillSources,
		Dest:        dest,
	})
	if err != nil {
		return assets.Manifest{}, fmt.Errorf("按共享发布记录构建 bundle: %w", err)
	}
	if built.BundleHash != expected {
		return assets.Manifest{}, fmt.Errorf("%w: 本地构建结果 %s 与发布记录 %s 不符",
			core.ErrAssetVersionMismatch, built.BundleHash, expected)
	}
	return built, nil
}
