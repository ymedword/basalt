package workspace

// 本文件是 Agent 目录的**生命周期目录操作**（颗粒度文档 §3.1.5 / 约束 27 / 31 / 49）：
// 创建走 `agents/<name>.staging` 构建 → 单次 rename 顶上；复制 / 恢复从源目录构建
// staging（改写 `name` / `identity.agent_name`、清空 `schedules`）→ 单次 rename；
// 归档走单次 rename 到 `archive/<name>-<stamp>/` 并计算目录清单与 `content_hash`。
//
// **锁与权威的归属**：这里的函数**不取实例锁、不开 DB、不写生命周期状态文件、不写
// `agent_archives` 行**——调用方必须是已持有实例锁的 `app.AgentAdmin`（HTTP 管理请求）
// 或 `app/ports.go` 的 CLI 写路径。状态文件唯一写者是 lifecycle.go（约束 49），
// 归档行的写入归存储层；本文件只负责目录本身。
// This file holds the agent directory lifecycle operations. None of them takes the
// instance lock, opens the database, or writes lifecycle/archive records: the caller
// owns both the lock and the records.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/core"
)

// utf8BOMPrefix 是 UTF-8 BOM 的字节序列（EF BB BF）。用字节转义而不是码点转义写进源码：
// 两者等价，但字节转义不会被编辑器还原成字面量（字面量会触发 Go 扫描器报错）。
// utf8BOMPrefix is the UTF-8 BOM as byte escapes so it never becomes a source literal.
const utf8BOMPrefix = "\xef\xbb\xbf"

// ErrArchiveTargetExists 表示归档目标目录已被占用（约束 33）：调用方据此映射 409
// CONFLICT（源目录原样保留），而不是把"稍后重试可能成功"的冲突报成 500。
// ErrArchiveTargetExists marks an occupied archive target; callers map it to 409.
var ErrArchiveTargetExists = errors.New("archive target already exists")

// ArchiveResult 是一次目录归档的产物：归档标识（= `archive/` 下的目录名）、归档目录
// 内容清单的规整哈希与清单 JSON（`[{path,sha256,size}]`，path 相对 Agent 根、升序）。
// `content_hash` 是裸的 64 位小写十六进制（与资源版本列同一口径）；ETag 形态的
// `sha256:` 前缀由读写侧自行拼接。
// ArchiveResult is one directory archive: the archive id (the directory name under
// archive/), the canonical hash of its content manifest, and the manifest JSON.
type ArchiveResult struct {
	ArchiveID    string
	ContentHash  string
	ManifestJSON string
}

// agentManifestEntry 是归档清单的一行（JSON 字段名逐字：path / sha256 / size）。
// agentManifestEntry is one manifest row; the JSON field names are fixed.
type agentManifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// CreateAgentStaged 在 `agents/<name>.staging` 构建模板目录，校验通过后单次 rename 顶上
// 正式目录（约束 27）：失败**只删本次构建的 staging**（既有目录一律不动），不取实例锁。
//
// staging 已存在 → 拒绝并点名（那是崩溃残留：恢复路径的第三态"保留现场并点名"，本函数
// **不**替运维决定删除）。正式目录已存在 → 拒绝且零写入。
// CreateAgentStaged builds the template under <name>.staging and renames it into place;
// it never takes the instance lock and only removes the staging tree it built itself.
func CreateAgentStaged(root string, spec core.AgentSpec, providers []string) ([]core.AgentFileInfo, error) {
	return CreateAgentStagedWithBindings(root, spec, providers, core.ResourceBindings{})
}

// CreateAgentStagedWithBindings 与 `CreateAgentStaged` 同一编排，额外把**服务端选定**的
// 资源绑定写进模板（颗粒度文档 R4.6「模板创建是产生绑定的三类入口之一」）：绑定经
// `PatchResourceBindings` 整块插入，块外字节由同一套块编辑器保证，UID 直接来自已发布
// 条目——**不**按名称重新解析。空绑定等价于原行为（模板不带 `resource_bindings` 块）。
// CreateAgentStagedWithBindings additionally writes the server-chosen bindings into the
// template through the same block editor; empty bindings keep the historical bytes.
func CreateAgentStagedWithBindings(root string, spec core.AgentSpec, providers []string,
	bindings core.ResourceBindings,
) ([]core.AgentFileInfo, error) {
	if err := ValidateAgentName(spec.Name); err != nil {
		return nil, err
	}
	content, err := agentTemplate(spec, providers)
	if err != nil {
		return nil, err
	}
	if len(bindings.ModelConnections)+len(bindings.NotifyChannels) > 0 {
		patched, patchErr := PatchResourceBindings([]byte(content), bindings)
		if patchErr != nil {
			return nil, fmt.Errorf("写入模板的资源绑定: %w", patchErr)
		}
		content = string(patched)
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		// 关闭失败不改变业务结论：目录句柄的关闭错误在本函数返回后已无补救动作。
		_ = base.Close()
	}()

	// 未初始化工作区：agents/ 缺失 → 点名 + 指引（与命令面同一口径）。
	if _, statErr := base.Stat(agentsDirName); statErr != nil {
		return nil, fmt.Errorf("工作区未初始化：缺少目录 %s（先执行 basalt init）: %w", agentsDirName, statErr)
	}

	relDir := path.Join(agentsDirName, spec.Name)
	if _, statErr := base.Stat(filepath.FromSlash(relDir)); statErr == nil {
		return nil, fmt.Errorf("同名目录已存在，拒绝覆盖：%s", relDir)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("检查 Agent 目录 %s: %w", relDir, statErr)
	}

	relStaging := path.Join(agentsDirName, spec.Name+stagingSuffix)
	if _, statErr := base.Stat(filepath.FromSlash(relStaging)); statErr == nil {
		return nil, fmt.Errorf("staging 残留目录已存在，拒绝覆盖：%s（确认无价值后手工删除，或重启由恢复路径处置）", relStaging)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("检查 staging 目录 %s: %w", relStaging, statErr)
	}

	discard := func(cause error) error {
		if removeErr := base.RemoveAll(filepath.FromSlash(relStaging)); removeErr != nil {
			return errors.Join(cause,
				fmt.Errorf("清理残留目录 %s 失败，请手工删除：%w", relStaging, removeErr))
		}
		return cause
	}

	if err := base.Mkdir(filepath.FromSlash(relStaging), dirPerm); err != nil {
		return nil, writeFailed(spec.Name, fmt.Errorf("创建 staging 目录 %s: %w", relStaging, err))
	}
	relStagedFile := path.Join(relStaging, agentDefinitionFile)
	if err := base.WriteFile(filepath.FromSlash(relStagedFile), []byte(content), filePerm); err != nil {
		return nil, writeFailed(spec.Name, discard(fmt.Errorf("写入 Agent 定义 %s: %w", relStagedFile, err)))
	}
	if err := base.Rename(filepath.FromSlash(relStaging), filepath.FromSlash(relDir)); err != nil {
		return nil, writeFailed(spec.Name, discard(fmt.Errorf("替换 Agent 目录 %s → %s: %w", relStaging, relDir, err)))
	}

	return []core.AgentFileInfo{{
		Path: path.Join(relDir, agentDefinitionFile),
		Size: int64(len(content)),
	}}, nil
}

// CopyAgentDir 把活动区的 `agents/<source>` 复制为 `agents/<target>`（约束 31）：完整
// 复制目录 → 在 `agents/<target>.staging` 改写定义文件的 `name` / `identity.agent_name`、
// 清空 `schedules`，并按 `rewrites`（应用层已解析校验的资源映射计划，FR-070）改写
// 资源引用与绑定块 → **单次 rename** 顶上。会话 / 记忆 / 审计数据不在 Agent 目录内，
// 因此不会被复制（契约 agent-md.md §2.2 的复制口径）。
//
// 源是否处于活动区（`active` / `disabled`）、目标初始 `disabled` 的建立与映射计划是否
// 合法都归调用方：生命周期迁移由 `Lifecycle.Bootstrap` 完成（约束 49），本函数只碰目录，
// 计划与文件不符时 fail-closed。失败**只删本次构建的 staging**（目标目录零残留）；
// 目标已存在 / staging 残留 → 拒绝并点名。
// CopyAgentDir copies an active-area agent directory into agents/<target> through a
// staging build that rewrites the identity fields, clears schedules and materializes the
// caller-validated resource-mapping plan, then renames it into place. It takes no
// instance lock and writes no lifecycle state.
func CopyAgentDir(root, source, target string, rewrites []AgentCopyRewrite) ([]core.AgentFileInfo, error) {
	if err := ValidateAgentName(source); err != nil {
		return nil, err
	}
	if err := ValidateAgentName(target); err != nil {
		return nil, err
	}
	if source == target {
		return nil, fmt.Errorf("复制源与目标同名，拒绝原地复制：%s", source)
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	srcRel := path.Join(agentsDirName, source)
	info, statErr := base.Lstat(filepath.FromSlash(srcRel))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("复制源 Agent 目录不存在：%s", srcRel)
		}
		return nil, fmt.Errorf("检查复制源目录 %s: %w", srcRel, statErr)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("复制源条目不是目录：%s（%s）", srcRel, fileKind(info))
	}

	return copyAgentDirInto(base, srcRel, target, "复制", rewrites)
}

// RestoreAgentDir 把归档区目录 `archive/<archive_id>` 恢复为 `agents/<target>`（约束 31）：
// 复制归档内容 → staging 内改写 `name` / `identity.agent_name`、清空 `schedules` →
// 单次 rename。**恢复是复制不是搬移**：归档目录原样保留，`restored_at` 标记归存储层。
//
// 归档目录先做清单可复算性前置（只收录普通文件、可逐字节复算）——不满足则**在任何写入
// 之前**拒绝，目标与 staging 零残留。与归档行 `content_hash` 的比对属于调用方
// `app.AgentAdmin.Restore`：那里才同时持有归档行、实例锁与生命周期端口；原名 / 异名的
// 状态迁移（`Transition` 保留 generation 或 `Bootstrap`）同样归调用方（约束 49）。
// RestoreAgentDir copies an archive directory into agents/<target> through the same
// staging build. The archive stays in place; comparing its hash against the archive row
// and migrating lifecycle state belong to the caller.
func RestoreAgentDir(root, archiveDir, target string) ([]core.AgentFileInfo, error) {
	if err := ValidateAgentName(target); err != nil {
		return nil, err
	}
	archiveRel, err := normalizeArchiveRelDir(archiveDir)
	if err != nil {
		return nil, err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	info, statErr := base.Lstat(filepath.FromSlash(archiveRel))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("归档目录不存在：%s", archiveRel)
		}
		return nil, fmt.Errorf("检查归档目录 %s: %w", archiveRel, statErr)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("归档条目不是目录：%s（%s）", archiveRel, fileKind(info))
	}

	if _, _, manifestErr := agentDirManifest(base, archiveRel); manifestErr != nil {
		return nil, fmt.Errorf("归档目录清单无法复算，拒绝恢复：%s: %w", archiveRel, manifestErr)
	}

	return copyAgentDirInto(base, archiveRel, target, "恢复", nil)
}

// normalizeArchiveRelDir 校验恢复源路径：必须是 `archive/<归档标识>` 形态的目录，沿用
// 写面同一组路径规则（拒绝绝对路径、`.` / `..` 段、控制字符与 staging / backup 后缀）。
// 恢复源不得指向活动区（`agents/`）或更深的子目录——那会把非归档内容当成归档副本。
// normalizeArchiveRelDir validates that a restore source is exactly archive/<id>.
func normalizeArchiveRelDir(raw string) (string, error) {
	normalized, err := normalizeAgentFilePath(raw)
	if err != nil {
		return "", fmt.Errorf("归档目录路径非法: %w", err)
	}
	segments := strings.Split(normalized, "/")
	if len(segments) != 2 || segments[0] != archiveDirName {
		return "", fmt.Errorf("恢复源必须是 %s/<归档标识> 形态的目录：%s", archiveDirName, normalized)
	}
	return normalized, nil
}

// ReadAgentDefinition 读取 `agents/<name>/AGENT.md` 的原始字节——写路径重建绑定投影时
// 用它拿"磁盘上的权威定义"（不重新序列化、不读其它文件）。只读。
// ReadAgentDefinition reads the authoritative AGENT.md bytes of one active agent.
func ReadAgentDefinition(root, name string) ([]byte, error) {
	if err := ValidateAgentName(name); err != nil {
		return nil, err
	}
	return readDefinitionAt(root, path.Join(agentsDirName, name))
}

// ReadArchivedAgentDefinition 读取 `archive/<archive_id>/AGENT.md` 的原始字节：归档把
// 目录整体 rename 走、字节不变，归档后重建投影仍以这份文件为准。只读。
// ReadArchivedAgentDefinition reads the AGENT.md bytes of one archived agent.
func ReadArchivedAgentDefinition(root, archiveID string) ([]byte, error) {
	archiveRel, err := normalizeArchiveRelDir(path.Join(archiveDirName, archiveID))
	if err != nil {
		return nil, err
	}
	return readDefinitionAt(root, archiveRel)
}

// readDefinitionAt 读取工作区相对目录下的定义文件：必须是普通文件（符号链接与特殊文件
// 一律拒绝——与受管文件清单同一判据），缺失 → 点名文件名。
// readDefinitionAt reads AGENT.md under one workspace-relative directory.
func readDefinitionAt(root, relDir string) ([]byte, error) {
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	relDef := path.Join(relDir, agentDefinitionFile)
	info, statErr := base.Lstat(filepath.FromSlash(relDef))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("定义文件不存在：%s: %w", relDef, fs.ErrNotExist)
		}
		return nil, fmt.Errorf("检查定义文件 %s: %w", relDef, statErr)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("定义文件不是普通文件：%s（%s）", relDef, fileKind(info))
	}
	raw, readErr := base.ReadFile(filepath.FromSlash(relDef))
	if readErr != nil {
		return nil, fmt.Errorf("读取定义文件 %s: %w", relDef, readErr)
	}
	return raw, nil
}

// copyAgentDirInto 是复制与恢复共享的目录构建段：目标 / staging 冲突检查 → 建 staging →
// 完整复制源目录 → 改写定义文件身份 → 单次 rename。失败**只删本次构建的 staging**；
// 成功返回落盘后重新读取的受管文件清单（path 相对 Agent 目录、按升序）。
// copyAgentDirInto is the shared staging build for copy and restore.
func copyAgentDirInto(base *os.Root, srcRel, target, operation string, rewrites []AgentCopyRewrite) ([]core.AgentFileInfo, error) {
	relDir := path.Join(agentsDirName, target)
	if _, statErr := base.Lstat(filepath.FromSlash(relDir)); statErr == nil {
		return nil, fmt.Errorf("目标 Agent 目录已存在，拒绝%s：%s", operation, relDir)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("检查目标 Agent 目录 %s: %w", relDir, statErr)
	}

	relStaging := path.Join(agentsDirName, target+stagingSuffix)
	if _, statErr := base.Lstat(filepath.FromSlash(relStaging)); statErr == nil {
		return nil, fmt.Errorf("staging 残留目录已存在，拒绝覆盖：%s（确认无价值后手工删除，或重启由恢复路径处置）", relStaging)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("检查 staging 目录 %s: %w", relStaging, statErr)
	}

	if err := base.Mkdir(filepath.FromSlash(relStaging), dirPerm); err != nil {
		return nil, writeFailed(target, fmt.Errorf("创建 staging 目录 %s: %w", relStaging, err))
	}
	discard := func(cause error) error {
		if removeErr := base.RemoveAll(filepath.FromSlash(relStaging)); removeErr != nil {
			return errors.Join(cause,
				fmt.Errorf("清理残留目录 %s 失败，请手工删除：%w", relStaging, removeErr))
		}
		return cause
	}

	if _, err := copyAgentTree(base, srcRel, relStaging); err != nil {
		return nil, writeFailed(target, discard(fmt.Errorf("%s %s 到候选目录: %w", operation, srcRel, err)))
	}
	if err := rewriteStagedAgentDefinition(base, relStaging, target, rewrites); err != nil {
		return nil, writeFailed(target, discard(err))
	}
	if err := base.Rename(filepath.FromSlash(relStaging), filepath.FromSlash(relDir)); err != nil {
		return nil, writeFailed(target, discard(fmt.Errorf(
			"安装候选目录 %s → %s（%s）: %w", relStaging, relDir, operation, err)))
	}

	files, err := managedFilesIn(base, relDir)
	if err != nil {
		return nil, err
	}
	infos := make([]core.AgentFileInfo, len(files))
	for i, file := range files {
		infos[i] = core.AgentFileInfo{Path: file.Path, Size: int64(len(file.Content))}
	}
	return infos, nil
}

// AgentCopyRewrite 是复制时对定义文件里一条资源引用的改写计划（FR-070）：应用层在
// 预写**之前**以源绑定为准解析并校验它，workspace 只在 staging 树里一次性落盘。
// 计划按 (类别, 来源 UID) 定位绑定，**不**按名称猜目标、**不**重新解析 UID——目标名与
// 目标 UID 都是应用层查证过的字面值。
// AgentCopyRewrite replaces one source binding with an explicitly resolved target; the
// workspace layer only materializes an already-validated plan.
type AgentCopyRewrite struct {
	Kind     core.BindingKind
	FromName string
	FromUID  core.ResourceUID
	ToName   string
	ToUID    core.ResourceUID
}

// rewriteStagedAgentDefinition 改写候选树里的定义文件（约束 31）：`name` 必须等于目标目录名
// （漏改会让目标 Agent 整只不注册）、`identity.agent_name` 存在时一并改写、`schedules`
// 清空（复制 / 恢复不得继承定时任务——全局 id 会冲突）、复制计划里的资源引用按映射改写
// （恢复不带计划）。候选树没有定义文件 → 拒绝：目标必须是完整 Agent 目录，不能复制出半只。
// rewriteStagedAgentDefinition rewrites the staged AGENT.md to the target identity and, on
// the copy path, applies the validated resource-mapping plan.
func rewriteStagedAgentDefinition(base *os.Root, relStaging, target string, rewrites []AgentCopyRewrite) error {
	relDef := path.Join(relStaging, agentDefinitionFile)
	info, statErr := base.Lstat(filepath.FromSlash(relDef))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("候选树缺少 %s（复制 / 恢复的目标必须是完整 Agent 目录）", agentDefinitionFile)
		}
		return fmt.Errorf("检查候选定义 %s: %w", relDef, statErr)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("候选定义不是普通文件：%s（%s）", relDef, fileKind(info))
	}

	raw, err := base.ReadFile(filepath.FromSlash(relDef))
	if err != nil {
		return fmt.Errorf("读取候选定义 %s: %w", relDef, err)
	}
	renamed, err := RewriteAgentName(raw, target)
	if err != nil {
		return fmt.Errorf("改写候选定义 %s 的 name: %w", relDef, err)
	}
	cleared, err := clearAgentSchedules(renamed)
	if err != nil {
		return fmt.Errorf("清空候选定义 %s 的 schedules: %w", relDef, err)
	}
	mapped, err := applyCopyRewrites(cleared, rewrites)
	if err != nil {
		return fmt.Errorf("按资源映射改写候选定义 %s: %w", relDef, err)
	}
	if err := base.WriteFile(filepath.FromSlash(relDef), mapped, filePerm); err != nil {
		return fmt.Errorf("写回候选定义 %s: %w", relDef, err)
	}
	return nil
}

// applyCopyRewrites 按显式映射改写候选定义里的资源引用与绑定块（FR-070）：
// `provider.name` / `notify_channels` 按名称替换，`resource_bindings` 整块改写成映射后的
// `(name, uid)` 对。三条前置缺一即拒绝（fail-closed，候选树随 staging 一并丢弃）：
// 计划的来源绑定不在文件里、来源绑定的 UID 与计划不一致、被改写的引用没有对应绑定。
// 改写复用 `ApplyChanges` 的行级外科手术与回读校验——围栏外正文、注释与未知键逐字保留；
// 落盘前再核一次引用与绑定的严格一一对应，保证写出的定义本身自洽。
// 空计划是恒等变换：不解析、不改写，未提供映射的复制与恢复走原样字节。
// applyCopyRewrites materializes a validated mapping plan onto the staged definition and
// re-checks reference/binding correspondence before it is written.
func applyCopyRewrites(raw []byte, rewrites []AgentCopyRewrite) ([]byte, error) {
	if len(rewrites) == 0 {
		return raw, nil
	}
	profile, _, err := parseFrontmatter(raw)
	if err != nil {
		return nil, fmt.Errorf("解析候选定义: %w", err)
	}
	if profile == nil {
		return nil, errors.New("候选定义没有 frontmatter，无法应用资源映射")
	}

	// 绑定以 (类别, UID) 定位：UID 是不变身份，名称只是标签（同名重建会换 UID）。
	type bindingKey struct {
		kind core.BindingKind
		uid  core.ResourceUID
	}
	bound := make(map[bindingKey]core.ResourceBinding,
		len(profile.ResourceBindings.ModelConnections)+len(profile.ResourceBindings.NotifyChannels))
	for _, entry := range profile.ResourceBindings.ModelConnections {
		bound[bindingKey{core.BindingModelConnection, entry.UID}] = entry
	}
	for _, entry := range profile.ResourceBindings.NotifyChannels {
		bound[bindingKey{core.BindingNotifyChannel, entry.UID}] = entry
	}

	var providerName *string
	var notifyChannels []string
	changes := core.AgentContentChanges{}
	for _, rewrite := range rewrites {
		entry, ok := bound[bindingKey{rewrite.Kind, rewrite.FromUID}]
		if !ok {
			return nil, fmt.Errorf("资源映射的来源 %s %q 不在定义的 resource_bindings 里",
				rewrite.Kind, rewrite.FromUID)
		}
		if entry.Name != rewrite.FromName {
			return nil, fmt.Errorf("资源映射的来源 %s %q 与定义文件的绑定名 %q 不一致（计划过期）",
				rewrite.Kind, rewrite.FromName, entry.Name)
		}
		if rewrite.ToName == "" {
			return nil, fmt.Errorf("资源映射 %s %q 缺少目标名称", rewrite.Kind, rewrite.FromName)
		}
		if rewrite.ToUID == "" || rewrite.ToUID == rewrite.FromUID {
			return nil, fmt.Errorf("资源映射 %s %q 的目标 UID 非法（不得沿用源 UID）",
				rewrite.Kind, rewrite.FromName)
		}
		delete(bound, bindingKey{rewrite.Kind, rewrite.FromUID})

		switch rewrite.Kind {
		case core.BindingModelConnection:
			if profile.Provider.Name != rewrite.FromName {
				return nil, fmt.Errorf("资源映射的来源模型连接 %q 不是定义里的 provider.name %q（计划过期）",
					rewrite.FromName, profile.Provider.Name)
			}
			name := rewrite.ToName
			providerName = &name
		case core.BindingNotifyChannel:
			if notifyChannels == nil {
				notifyChannels = make([]string, len(profile.NotifyChannels))
				copy(notifyChannels, profile.NotifyChannels)
			}
			replaced := false
			for i, channel := range notifyChannels {
				if channel == rewrite.FromName {
					notifyChannels[i], replaced = rewrite.ToName, true
				}
			}
			if !replaced {
				return nil, fmt.Errorf("资源映射的来源通知渠道 %q 不在定义的 notify_channels 里", rewrite.FromName)
			}
		default:
			return nil, fmt.Errorf("未知的资源映射类别 %q", rewrite.Kind)
		}

		changes.Bindings = append(changes.Bindings, core.BindingChange{
			ResourceKind: rewrite.Kind,
			RefName:      rewrite.ToName,
			ResourceUID:  rewrite.ToUID,
		})
	}

	// 未被改写的绑定原样保留：共享绑定随目录携带，UID 不变（契约复制口径）。
	for _, entry := range profile.ResourceBindings.ModelConnections {
		if _, kept := bound[bindingKey{core.BindingModelConnection, entry.UID}]; kept {
			changes.Bindings = append(changes.Bindings, core.BindingChange{
				ResourceKind: core.BindingModelConnection,
				RefName:      entry.Name,
				ResourceUID:  entry.UID,
			})
		}
	}
	for _, entry := range profile.ResourceBindings.NotifyChannels {
		if _, kept := bound[bindingKey{core.BindingNotifyChannel, entry.UID}]; kept {
			changes.Bindings = append(changes.Bindings, core.BindingChange{
				ResourceKind: core.BindingNotifyChannel,
				RefName:      entry.Name,
				ResourceUID:  entry.UID,
			})
		}
	}
	if providerName != nil {
		changes.Provider = &core.AgentProviderChanges{Name: providerName}
	}
	if notifyChannels != nil {
		changes.NotifyChannels = &notifyChannels
	}

	next, err := ApplyChanges(raw, changes)
	if err != nil {
		return nil, err
	}

	// 落盘前的最后一道核对：改写后的引用与绑定必须严格一一对应（缺绑定 / 孤儿绑定都拒绝）。
	// 这一步不依赖应用层的计划形状，防止"改写成功但定义自相矛盾"的半成品进入候选树。
	rewritten, _, err := parseFrontmatter(next)
	if err != nil {
		return nil, fmt.Errorf("复核改写后的候选定义: %w", err)
	}
	var declared []string
	if rewritten.Provider.Name != "" {
		declared = []string{rewritten.Provider.Name}
	}
	if err := rewritten.ResourceBindings.ValidateCorrespondence(declared, rewritten.NotifyChannels); err != nil {
		return nil, err
	}
	return next, nil
}

// clearAgentSchedules 把定义文件 frontmatter 顶层的 `schedules` 改写为空列表（约束 31）：
// 块式列表整体替换为 `schedules: []`；流式值只换值并保留行尾注释；键不存在即已是空，
// 原样返回。围栏外正文、其余键与注释逐字保留——**不**重新序列化整份文档（那会丢注释与
// 未知键，与 RewriteAgentName 同一口径）。
// clearAgentSchedules empties the top-level schedules key without re-serialising.
func clearAgentSchedules(agentMD []byte) ([]byte, error) {
	// 容忍 UTF-8 BOM；必须写转义——字面量会触发 Go 扫描器的「illegal byte order mark」。
	// 这里用字节转义（EF BB BF）：与源码里的码点转义等价，且不会被编辑器还原成字面量。
	text := string(agentMD)
	bom := ""
	if strings.HasPrefix(text, utf8BOMPrefix) {
		bom, text = utf8BOMPrefix, strings.TrimPrefix(text, utf8BOMPrefix)
	}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return nil, errNoFrontmatterFence
	}
	closing := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			closing = i
			break
		}
	}
	if closing < 0 {
		return nil, errOpenFrontmatterFence
	}

	keyIdx := -1
	for i := 1; i < closing; i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.TrimSpace(line) == "" || strings.TrimLeft(line, " \t") != line {
			continue // 只认顶层键：缩进行与空行不属于键
		}
		key, _, ok := splitFrontmatterKey(line)
		if !ok || key != "schedules" {
			continue
		}
		if keyIdx >= 0 {
			return nil, fmt.Errorf("frontmatter 含重复键 schedules，无法判定清空目标")
		}
		keyIdx = i
	}
	if keyIdx < 0 {
		return agentMD, nil
	}

	line := strings.TrimSuffix(lines[keyIdx], "\r")
	_, rest, _ := splitFrontmatterKey(line)
	_, comment := splitTrailingComment(rest)

	// 块式列表的续行：缩进行，以及块内空白行；块尾紧邻下一顶层键前的空白行保留。
	end := keyIdx + 1
	for end < closing {
		candidate := strings.TrimSuffix(lines[end], "\r")
		if strings.TrimSpace(candidate) == "" || strings.TrimLeft(candidate, " \t") != candidate {
			end++
			continue
		}
		break
	}
	for end > keyIdx+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}

	replacement := line[:len(line)-len(rest)] + " []"
	if comment != "" {
		replacement += " " + comment
	}
	lines = append(append(lines[:keyIdx:keyIdx], replacement+lineEnding(lines[keyIdx])), lines[end:]...)
	return []byte(bom + strings.Join(lines, "\n")), nil
}

// ArchiveAgentDir 把 `agents/<name>/` **单次 rename** 到 `archive/<name>-<stamp>/`（约束 32
// 的目录段），rename 成功后计算目录清单与 `content_hash`。会话 / 记忆 / 审计数据一个字节
// 都不动；不取实例锁、不改状态文件（迁移与归档行由调用方编排）。
//
// 归档目标已存在 → 拒绝且源目录原样保留（不自动换名，约束 32 / 边界 §2.4 注 11）。
// 归档名用 `at.UTC()`——注入时刻的时区不影响归档标识。清单计算失败 → 反向 rename 回滚，
// 磁盘回到"未归档"状态（不得留下没有清单的归档副本）。
// ArchiveAgentDir renames the agent directory into archive/ in one atomic move and then
// hashes the resulting tree, rolling the rename back when the manifest cannot be built.
func ArchiveAgentDir(root, name string, at time.Time) (ArchiveResult, error) {
	if err := ValidateAgentName(name); err != nil {
		return ArchiveResult{}, err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return ArchiveResult{}, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := path.Join(agentsDirName, name)
	// 源条目**必须**是目录：agents/ 下的普通文件不是 Agent 目录（装载面同样忽略它）。
	// 判定用 **Lstat**——符号链接如实报成符号链接、不跟随。
	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return ArchiveResult{}, fmt.Errorf("待归档目录不存在：%s", relDir)
		}
		return ArchiveResult{}, fmt.Errorf("检查 Agent 目录 %s: %w", relDir, statErr)
	}
	if !info.IsDir() {
		return ArchiveResult{}, fmt.Errorf("待归档条目不是目录：%s（%s）", relDir, fileKind(info))
	}

	// 归档目录兜底：basalt init 已建，但工作区被手工清理过时不该失败在裸 ENOENT 上。
	if _, statErr := base.Stat(archiveDirName); errors.Is(statErr, fs.ErrNotExist) {
		if mkErr := base.Mkdir(archiveDirName, dirPerm); mkErr != nil {
			return ArchiveResult{}, writeFailed(name, fmt.Errorf("创建归档目录 %s: %w", archiveDirName, mkErr))
		}
	} else if statErr != nil {
		return ArchiveResult{}, fmt.Errorf("检查归档目录 %s: %w", archiveDirName, statErr)
	}

	archiveID := name + "-" + at.UTC().Format(archiveStampLayout)
	relTarget := path.Join(archiveDirName, archiveID)
	if _, statErr := base.Stat(filepath.FromSlash(relTarget)); statErr == nil {
		return ArchiveResult{}, fmt.Errorf("归档目标已存在，拒绝覆盖：%s（请稍后重试）: %w",
			relTarget, ErrArchiveTargetExists)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return ArchiveResult{}, fmt.Errorf("检查归档目标 %s: %w", relTarget, statErr)
	}

	if err := base.Rename(filepath.FromSlash(relDir), filepath.FromSlash(relTarget)); err != nil {
		return ArchiveResult{}, writeFailed(name, fmt.Errorf("归档 Agent 目录 %s → %s: %w", relDir, relTarget, err))
	}

	manifestJSON, contentHash, err := agentDirManifest(base, relTarget)
	if err != nil {
		if rbErr := base.Rename(filepath.FromSlash(relTarget), filepath.FromSlash(relDir)); rbErr != nil {
			return ArchiveResult{}, writeFailed(name, errors.Join(
				fmt.Errorf("归档清单计算失败：%w", err),
				fmt.Errorf("回滚目录 %s → %s 失败，归档处于不确定状态：%w", relTarget, relDir, rbErr)))
		}
		return ArchiveResult{}, writeFailed(name, fmt.Errorf("归档清单计算失败（目录已回滚）：%w", err))
	}

	return ArchiveResult{ArchiveID: archiveID, ContentHash: contentHash, ManifestJSON: manifestJSON}, nil
}

// ArchiveDirContentHash 复算一个归档目录的 `content_hash`（约束 31 的恢复前置校验）：
// 与 `ArchiveAgentDir` 使用**同一个**清单算法，调用方拿它与归档行比对；只读、不改目录。
// 归档目录缺失 / 不是目录 / 含非普通文件一律报错——清单不可复算的归档不得恢复。
// ArchiveDirContentHash recomputes the content hash of an archive directory with the
// same manifest algorithm the archive used; it is read-only.
func ArchiveDirContentHash(root, archiveDir string) (string, error) {
	archiveRel, err := normalizeArchiveRelDir(archiveDir)
	if err != nil {
		return "", err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	info, statErr := base.Lstat(filepath.FromSlash(archiveRel))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return "", fmt.Errorf("归档目录不存在：%s", archiveRel)
		}
		return "", fmt.Errorf("检查归档目录 %s: %w", archiveRel, statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("归档条目不是目录：%s（%s）", archiveRel, fileKind(info))
	}

	_, contentHash, err := agentDirManifest(base, archiveRel)
	if err != nil {
		return "", fmt.Errorf("归档目录清单无法复算：%s: %w", archiveRel, err)
	}
	return contentHash, nil
}

// UnarchiveAgentDir 把归档目录**原样 rename 回** `agents/<name>`——归档行写入失败时的
// 反向 rename 补偿（约束 32 ⑧）。它只搬目录：状态文件回退、注册表与定时恢复归调用方。
// 源缺失 / 目标已存在 → 拒绝（不覆盖任何一边；目标占用说明磁盘已被别的写入改变）。
// UnarchiveAgentDir renames an archive directory back into agents/<name>; it is the
// reverse-rename compensation for a failed archive-row insert and only moves the tree.
func UnarchiveAgentDir(root, archiveID, name string) error {
	if err := ValidateAgentName(name); err != nil {
		return err
	}
	archiveRel, err := normalizeArchiveRelDir(path.Join(archiveDirName, archiveID))
	if err != nil {
		return err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	info, statErr := base.Lstat(filepath.FromSlash(archiveRel))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("反向归档失败：归档目录不存在：%s", archiveRel)
		}
		return fmt.Errorf("检查归档目录 %s: %w", archiveRel, statErr)
	}
	if !info.IsDir() {
		return fmt.Errorf("反向归档失败：归档条目不是目录：%s（%s）", archiveRel, fileKind(info))
	}

	relDir := path.Join(agentsDirName, name)
	if _, statErr := base.Lstat(filepath.FromSlash(relDir)); statErr == nil {
		return fmt.Errorf("反向归档失败：目标 Agent 目录已存在，拒绝覆盖：%s", relDir)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("检查目标 Agent 目录 %s: %w", relDir, statErr)
	}

	if err := base.Rename(filepath.FromSlash(archiveRel), filepath.FromSlash(relDir)); err != nil {
		return fmt.Errorf("反向归档目录 %s → %s: %w", archiveRel, relDir, err)
	}
	return nil
}

// RemoveAgentDir 删除 `agents/<name>` 整棵目录，**只用于恢复失败后的回滚**（恢复是复制，
// 归档副本仍在 `archive/` 下）。日常删除必须走归档（`ArchiveAgentDir`），**不得**用本函数
// 替代——那会绕过归档身份与清单。目录不存在是 no-op（回滚可重入）。
// RemoveAgentDir removes agents/<name> as a restore-failure rollback only. Ordinary
// deletion must go through archiving; an absent directory is a no-op.
func RemoveAgentDir(root, name string) error {
	if err := ValidateAgentName(name); err != nil {
		return err
	}

	base, err := os.OpenRoot(root)
	if err != nil {
		return fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		_ = base.Close()
	}()

	relDir := path.Join(agentsDirName, name)
	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("检查 Agent 目录 %s: %w", relDir, statErr)
	}
	if !info.IsDir() {
		return fmt.Errorf("回滚目标不是目录，拒绝删除：%s（%s）", relDir, fileKind(info))
	}
	if err := base.RemoveAll(filepath.FromSlash(relDir)); err != nil {
		return fmt.Errorf("删除恢复目标目录 %s: %w", relDir, err)
	}
	return nil
}

// agentDirManifest 计算一个 Agent 目录的内容清单与规整哈希：只收录**普通文件**，path 相对
// 该目录、按升序规整（约束 4 的同一规整规则：`path\0sha256(content)\0` 串的 sha256）。
// 符号链接与特殊文件一律拒绝——归档副本的哈希必须能在恢复时逐字节复算。
// agentDirManifest hashes the agent directory in canonical path order; symlinks and
// special files are refused so that a restore can recompute the same hash.
func agentDirManifest(base *os.Root, relDir string) (manifestJSON, contentHash string, err error) {
	entries := make([]agentManifestEntry, 0, 8)
	walkErr := fs.WalkDir(base.FS(), relDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return fmt.Errorf("读取条目信息 %s: %w", p, infoErr)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("归档目录含非普通文件：%s（%s）", p, fileKind(info))
		}
		data, readErr := fs.ReadFile(base.FS(), p)
		if readErr != nil {
			return fmt.Errorf("读取 %s: %w", p, readErr)
		}
		sum := sha256.Sum256(data)
		rel, relErr := filepath.Rel(relDir, p)
		if relErr != nil {
			return fmt.Errorf("计算相对路径 %s: %w", p, relErr)
		}
		entries = append(entries, agentManifestEntry{
			Path:   filepath.ToSlash(rel),
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(data)),
		})
		return nil
	})
	if walkErr != nil {
		return "", "", walkErr
	}

	slices.SortFunc(entries, func(a, b agentManifestEntry) int { return strings.Compare(a.Path, b.Path) })

	// 规整哈希输入：按 path 升序的 `path\0sha256\0` 串（与技能清单同一规则）。
	var canonical strings.Builder
	for _, entry := range entries {
		canonical.WriteString(entry.Path)
		canonical.WriteByte(0)
		canonical.WriteString(entry.SHA256)
		canonical.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(canonical.String()))

	manifest, err := json.Marshal(entries)
	if err != nil {
		return "", "", fmt.Errorf("编码归档清单: %w", err)
	}
	return string(manifest), hex.EncodeToString(sum[:]), nil
}
