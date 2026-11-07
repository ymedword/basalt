// Package assets 实现内容寻址的 Agent 资产 bundle（US-24 模块卡 §2.3.9 / §3.1.11）。
//
// 一个 bundle 是一次 Agent 定义与其绑定技能版本的**不可变快照**：
//   - 清单 `bundle.json` 记录 format_version / agent / agent_uid / config_hash /
//     config_files / skills / bundle_hash；
//   - 每个文件带 path + sha256 + size；`BundleHash` 是清单（去掉自身）规整 JSON 的
//     sha256，因此一个哈希同时钉住"哪份 AGENT.md"与"哪些技能文件"；
//   - 安装目录只由 hash 寻址：`<assetRoot>/bundles/<agent>/<bundleHash>`，已存在只做
//     逐文件比对、不重写；执行期只按清单固定的 hash 解析，**不**解析"最新版本"、
//     **不**维护可变 current 指针（C-28）。
//
// 三条判据（FR-24-24 / FR-24-42 / FR-24-44）：
//   - 往返：Build → Verify → Install → Resolve 逐字可复算；
//   - 拒收：截断 / 损坏 / 路径逃逸一律拒绝，且不破坏既有目录；
//   - 路径一致：Install 返回的 Dir、Resolve 的返回值与执行期读取路径逐字相等，
//     都含 `bundles/` 段。
//
// 本包不 import 其他 internal 包（只依赖 core 的纯类型与哨兵），也不认识工作区与
// 数据库：构建来源与发布写入由装配层通过 BuildSpec / command.go 的 Deps 注入
// （包纪律：实现放各自的包，core 只承载类型与接口）。
//
// Package assets implements the content-addressed agent bundle: a canonical manifest
// with per-file SHA-256 plus a bundle hash, immutable hash-addressed install
// directories, and hash-only resolution at execution time.
package assets

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
	"slices"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// FormatVersion 是 manifest 的格式版本（模块卡 §3.1.11 冻结为 1）。
	// FormatVersion is the frozen manifest format version.
	FormatVersion = 1

	// manifestFileName 是 bundle 根目录内的清单文件名；目录名与它共同构成可校验的
	// 内容寻址目录（执行期读它、`assets verify` 校验它）。
	// manifestFileName names the manifest inside a bundle root.
	manifestFileName = "bundle.json"

	// bundlesDirName 是资产根下的内容寻址目录段。**三处路径必须逐字相同**
	// （Install / Resolve / 执行期读取）：它缺失时会写一个目录、读另一个目录。
	// bundlesDirName is the hash-addressed segment shared by install, resolve and
	// execution; every path must contain it.
	bundlesDirName = "bundles"

	// configDirName / skillsDirName 是 bundle 内部的布局段：配置文件放
	// `agent/<相对路径>`，技能文件放 `skills/<技能名>/<相对路径>`。清单里的路径保持
	// 来源相对形态，布局前缀只由本包在文件系统边界追加。
	// configDirName / skillsDirName are the in-bundle layout prefixes; manifests keep
	// source-relative paths and only the filesystem boundary adds these prefixes.
	configDirName = "agent"
	skillsDirName = "skills"

	// dirPerm / filePerm 与工作区同一口径：企业私有数据不给"其他用户"留读取面。
	// dirPerm / filePerm match the workspace conventions.
	dirPerm  os.FileMode = 0o750
	filePerm os.FileMode = 0o600

	// hashHexLength 是 sha256 的小写十六进制长度。
	hashHexLength = 64
)

// ConfigBundleRel 给出一个配置文件在 bundle 里的**包内相对路径**（`agent/<rel>`，
// 正斜杠分隔）。构建、校验、导出与执行期读取共用这一份映射：布局段 `agent/` 只在本
// 包出现，调用方不得自己拼（FR-24-44 的"逐字相同"同样覆盖包内布局——安装与读取
// 各拼一次前缀时，改一处就会让两边读不同的文件）。
// ConfigBundleRel is the single in-bundle path mapping for config files; callers never
// spell the layout prefix themselves.
func ConfigBundleRel(rel string) string {
	return path.Join(configDirName, rel)
}

// Manifest 是 bundle 的清单（模块卡 §3.1.11 冻结）。字段顺序即序列化顺序：
// `BundleHash` 由**同一份声明序**的 JSON 计算，写读两处共用一件代码。
// Manifest is the frozen bundle manifest; the declaration order is the serialization
// order that the bundle hash covers.
type Manifest struct {
	FormatVersion int                    `json:"format_version"`
	Agent         string                 `json:"agent"`
	AgentUID      core.AgentUID          `json:"agent_uid"`
	ConfigHash    string                 `json:"config_hash"`
	ConfigFiles   []core.FileHash        `json:"config_files"`
	Skills        []core.SkillVersionRef `json:"skills"`
	BundleHash    string                 `json:"bundle_hash"`
}

// Installed 是安装结果。`Dir` **必须**是 `<assetRoot>/bundles/<agent>/<bundleHash>`：
// 由 BundleHash 寻址且不可变（已存在时只做逐文件比对，不重写）。Install / Resolve /
// 执行期读取三处路径必须逐字相同（含 `bundles/` 段，FR-24-44）。
// Installed is one immutable hash-addressed bundle directory.
type Installed struct {
	Manifest Manifest
	Dir      string
}

// SkillSource 是一个技能版本的**来源目录**：`Ref` 携带发布记录里的版本与完整文件
// 清单（路径 / 哈希 / 大小），`Root` 是该版本在磁盘上的根目录。构建时逐文件比对
// ——来源文件与发布记录不符即拒绝，不以本地内容覆盖共享记录。
// SkillSource pairs a published skill version record with its on-disk root.
type SkillSource struct {
	Ref  core.SkillVersionRef
	Root string
}

// BuildSpec 是一次构建的输入：
//   - `Agent` / `AgentUID` / `ConfigHash` 原样进清单（ConfigHash 是配置快照哈希，
//     不在这里重算——快照清单的路径是工作区相对形态，与 bundle 布局不同）；
//   - `ConfigRoot` + `ConfigFiles` 给出参与 bundle 的配置文件（AGENT.md 与 bootstrap
//     文件）及其相对路径；哈希与大小**从磁盘计算**——首次构建没有更早的记录可依；
//   - `Skills` 给出已发布技能版本；每个技能的文件哈希**必须**与发布记录逐项相符，
//     否则拒绝（资产不可变的前提是来源不许漂移）。
//
// BuildSpec is the input of one build. Config file hashes are computed from disk
// (first publication has no earlier record); skill files must match the published
// version record exactly.
type BuildSpec struct {
	Agent       string
	AgentUID    core.AgentUID
	ConfigHash  string
	ConfigRoot  string
	ConfigFiles []string
	Skills      []SkillSource
	// Dest 是本次构建写入的目录：必须不存在，或存在且为空——半成品与已构建的
	// bundle 混在同一目录会让 Verify 面对一个既非空又不可复算的树。
	// Dest must be absent or empty; mixing with an existing bundle would make Verify
	// face a tree it cannot reproduce from the manifest.
	Dest string
}

// Build 从来源目录构建一个 bundle 到 `spec.Dest` 并返回清单：逐文件复制并计算
// sha256 + 大小，组装规整清单，算出 BundleHash，写 `bundle.json`，最后用 Verify
// 反读一遍证明往返成立。任何一步不符都在**写坏目标之外**失败——已存在的 hash 目录
// 不受影响（调用方把 Build 指向暂存目录）。
// Build assembles a bundle into spec.Dest, computes the bundle hash and proves the
// round trip with Verify.
func Build(ctx context.Context, spec BuildSpec) (Manifest, error) {
	if err := validateBuildSpec(spec); err != nil {
		return Manifest{}, err
	}
	if err := prepareDir(spec.Dest, "构建目标"); err != nil {
		return Manifest{}, err
	}

	manifest := Manifest{
		FormatVersion: FormatVersion,
		Agent:         spec.Agent,
		AgentUID:      spec.AgentUID,
		ConfigHash:    spec.ConfigHash,
	}
	for _, rel := range spec.ConfigFiles {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		if err := safeRelPath(rel); err != nil {
			return Manifest{}, fmt.Errorf("配置文件路径 %q: %w", rel, err)
		}
		bundleRel := ConfigBundleRel(rel)
		dst := filepath.Join(spec.Dest, filepath.FromSlash(bundleRel))
		written, err := copyBundleFile(ctx, spec.ConfigRoot, rel, dst, nil)
		if err != nil {
			return Manifest{}, fmt.Errorf("构建配置文件 %s: %w", rel, err)
		}
		manifest.ConfigFiles = append(manifest.ConfigFiles, core.FileHash{
			Path: rel, SHA256: written.SHA256, Size: written.Size,
		})
	}
	for _, source := range spec.Skills {
		for _, file := range source.Ref.Files {
			if err := ctx.Err(); err != nil {
				return Manifest{}, err
			}
			bundleRel := path.Join(skillsDirName, source.Ref.Name, file.Path)
			dst := filepath.Join(spec.Dest, filepath.FromSlash(bundleRel))
			// 来源必须与发布记录逐项相符，清单沿用发布记录的值。
			if _, err := copyBundleFile(ctx, source.Root, file.Path, dst, &file); err != nil {
				return Manifest{}, fmt.Errorf("构建技能 %s 文件 %s: %w", source.Ref.Name, file.Path, err)
			}
		}
		manifest.Skills = append(manifest.Skills, source.Ref)
	}

	normalized, err := normalizeManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	hash, err := computeBundleHash(normalized)
	if err != nil {
		return Manifest{}, err
	}
	normalized.BundleHash = hash
	if err := writeManifest(spec.Dest, normalized); err != nil {
		return Manifest{}, err
	}
	verified, err := Verify(ctx, spec.Dest)
	if err != nil {
		return Manifest{}, fmt.Errorf("构建结果自校验: %w", err)
	}
	if !sameManifest(verified, normalized) {
		return Manifest{}, fmt.Errorf("%w: 构建结果与清单不一致", core.ErrAssetVersionMismatch)
	}
	return normalized, nil
}

// Export 把一个**已构建**的 bundle 树从 `srcRoot` 复制到 `dest`：逐文件与清单比对
// 后复制，再写 `bundle.json` 并自校验。用于把暂存或已安装的 bundle 交付给另一台
// Worker / 归档；不跟随符号链接、拒绝路径逃逸。
// Export copies an already-built bundle tree, verifying every file against the
// manifest, and writes a fresh bundle.json into dest.
func Export(ctx context.Context, manifest Manifest, srcRoot, dest string) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	if srcRoot == "" || dest == "" {
		return errors.New("导出必须同时给出源目录与目标目录")
	}
	if err := prepareDir(dest, "导出目标"); err != nil {
		return err
	}
	for _, file := range manifest.ConfigFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		bundleRel := ConfigBundleRel(file.Path)
		if _, err := copyBundleFile(ctx, srcRoot, bundleRel, filepath.Join(dest, filepath.FromSlash(bundleRel)), &file); err != nil {
			return fmt.Errorf("导出配置文件 %s: %w", file.Path, err)
		}
	}
	for _, skill := range manifest.Skills {
		for _, file := range skill.Files {
			if err := ctx.Err(); err != nil {
				return err
			}
			bundleRel := path.Join(skillsDirName, skill.Name, file.Path)
			if _, err := copyBundleFile(ctx, srcRoot, bundleRel, filepath.Join(dest, filepath.FromSlash(bundleRel)), &file); err != nil {
				return fmt.Errorf("导出技能 %s 文件 %s: %w", skill.Name, file.Path, err)
			}
		}
	}
	if err := writeManifest(dest, manifest); err != nil {
		return err
	}
	if _, err := Verify(ctx, dest); err != nil {
		return fmt.Errorf("导出结果自校验: %w", err)
	}
	return nil
}

// Verify 复算一个 bundle 目录：读 `bundle.json`、校验清单结构、按清单重算
// BundleHash，再逐文件比对 path + sha256 + 大小。目录名不属于校验面——校验只回答
// "这份内容是否与清单一致"，"目录名是否指向这份内容"由 Resolve 判。
// 缺失或不可读 → `core.ErrAssetUnavailable`；哈希 / 文件不符 → `core.ErrAssetVersionMismatch`。
// Verify recomputes the manifest hash and every file digest; the directory name is
// checked by Resolve, not here.
func Verify(ctx context.Context, dir string) (Manifest, error) {
	manifest, err := readManifest(dir)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	computed, err := computeBundleHash(manifest)
	if err != nil {
		return Manifest{}, err
	}
	if computed != manifest.BundleHash {
		return Manifest{}, fmt.Errorf("%w: 目录 %s 的清单哈希不可复算（记录 %s，实际 %s）",
			core.ErrAssetVersionMismatch, dir, manifest.BundleHash, computed)
	}
	for _, file := range manifest.ConfigFiles {
		if err := verifyBundleFile(ctx, dir, ConfigBundleRel(file.Path), file); err != nil {
			return Manifest{}, err
		}
	}
	for _, skill := range manifest.Skills {
		for _, file := range skill.Files {
			if err := verifyBundleFile(ctx, dir, path.Join(skillsDirName, skill.Name, file.Path), file); err != nil {
				return Manifest{}, err
			}
		}
	}
	return manifest, nil
}

// Install 把 `bundleDir` 安装到 `<assetRoot>/bundles/<agent>/<bundleHash>`：
//   - 先 Verify 来源；来源不完整（截断 / 损坏）即拒绝，**不触碰**目标目录；
//   - 目标已存在时**只做逐文件比对**：一致 → 幂等成功；不一致 → 拒绝并告警，
//     **不覆盖**已有 hash 目录（资产不可变，FR-24-42）；
//   - 目标不存在时写暂存目录（同卷）→ 校验 → rename 换入，保证读者永远看不到
//     半成品；rename 撞上并发安装则按"目标已存在"重走比对。
//
// Install is idempotent and never overwrites an existing hash-addressed directory.
func Install(ctx context.Context, bundleDir, assetRoot string) (Installed, error) {
	if assetRoot == "" {
		return Installed{}, fmt.Errorf("%w: 资产根为空（assets.bundle_root 必须非空）", core.ErrAssetUnavailable)
	}
	source, err := Verify(ctx, bundleDir)
	if err != nil {
		return Installed{}, fmt.Errorf("校验待安装 bundle: %w", err)
	}
	target := bundlePath(assetRoot, source.Agent, source.BundleHash)

	if same, err := compareExisting(ctx, target, source); err != nil {
		return Installed{}, err
	} else if same {
		return Installed{Manifest: source, Dir: target}, nil
	}

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, dirPerm); err != nil {
		return Installed{}, fmt.Errorf("创建资产目录 %s: %w", parent, err)
	}
	staging, err := os.MkdirTemp(parent, ".staging-")
	if err != nil {
		return Installed{}, fmt.Errorf("创建暂存目录: %w", err)
	}
	staged := true
	defer func() {
		if staged {
			_ = os.RemoveAll(staging) // 失败路径清理半成品；成功路径已随 rename 消失
		}
	}()

	for _, file := range source.ConfigFiles {
		bundleRel := ConfigBundleRel(file.Path)
		if _, err := copyBundleFile(ctx, bundleDir, bundleRel, filepath.Join(staging, filepath.FromSlash(bundleRel)), &file); err != nil {
			return Installed{}, fmt.Errorf("安装配置文件 %s: %w", file.Path, err)
		}
	}
	for _, skill := range source.Skills {
		for _, file := range skill.Files {
			bundleRel := path.Join(skillsDirName, skill.Name, file.Path)
			if _, err := copyBundleFile(ctx, bundleDir, bundleRel, filepath.Join(staging, filepath.FromSlash(bundleRel)), &file); err != nil {
				return Installed{}, fmt.Errorf("安装技能 %s 文件 %s: %w", skill.Name, file.Path, err)
			}
		}
	}
	if err := writeManifest(staging, source); err != nil {
		return Installed{}, err
	}
	if _, err := Verify(ctx, staging); err != nil {
		return Installed{}, fmt.Errorf("暂存目录自校验: %w", err)
	}
	if err := os.Rename(staging, target); err != nil {
		// 并发安装同一 hash：对方先 rename 成功。按"目标已存在"重走比对，
		// 对方写坏了就拒绝——**不**替它覆盖。
		if same, cmpErr := compareExisting(ctx, target, source); cmpErr != nil {
			return Installed{}, cmpErr
		} else if same {
			return Installed{Manifest: source, Dir: target}, nil
		}
		return Installed{}, fmt.Errorf("换入资产目录 %s: %w", target, err)
	}
	staged = false
	return Installed{Manifest: source, Dir: target}, nil
}

// Resolve 只按 `(assetRoot, agent, bundleHash)` 解析出安装目录，**不**解析"最新版本"。
// 返回前断言：目录存在、`bundle.json` 的 agent / bundle_hash 指向同一目标、哈希可
// 复算——目录名与被请求的 hash 不一致即拒绝执行（不静默降级）。
// Resolve answers only from the frozen hash; a directory whose manifest disagrees
// with the requested hash is refused rather than silently downgraded.
func Resolve(ctx context.Context, assetRoot, agent, bundleHash string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if assetRoot == "" {
		return "", fmt.Errorf("%w: 资产根为空（assets.bundle_root 必须非空）", core.ErrAssetUnavailable)
	}
	if err := safeSegment(agent); err != nil {
		return "", fmt.Errorf("%w: Agent 名 %q 不能作为资产目录段: %v", core.ErrAssetUnavailable, agent, err)
	}
	if !validHash(bundleHash) {
		return "", fmt.Errorf("%w: 非法的 bundle hash %q", core.ErrAssetVersionMismatch, bundleHash)
	}
	dir := bundlePath(assetRoot, agent, bundleHash)
	manifest, err := readManifest(dir)
	if err != nil {
		return "", err
	}
	if manifest.Agent != agent || manifest.BundleHash != bundleHash {
		return "", fmt.Errorf("%w: 目录 %s 的内容不属于 agent=%s bundle=%s",
			core.ErrAssetVersionMismatch, dir, agent, bundleHash)
	}
	computed, err := computeBundleHash(manifest)
	if err != nil {
		return "", err
	}
	if computed != bundleHash {
		return "", fmt.Errorf("%w: 目录 %s 的清单哈希不可复算（记录 %s，实际 %s）",
			core.ErrAssetVersionMismatch, dir, bundleHash, computed)
	}
	return dir, nil
}

// —— 清单计算 ——

// normalizeManifest 复制并排序清单：配置文件按 path、技能按 name、每个技能的文件
// 按 path 升序，列表一律非 nil。排序是 BundleHash 规整化的唯一来源，调用方
// **不得**自行排序后假设等价。
// normalizeManifest returns a sorted copy; list order is the only canonicalization
// the bundle hash relies on.
func normalizeManifest(m Manifest) (Manifest, error) {
	out := m
	out.ConfigFiles = slices.Clone(m.ConfigFiles)
	if out.ConfigFiles == nil {
		out.ConfigFiles = []core.FileHash{}
	}
	slices.SortFunc(out.ConfigFiles, func(a, b core.FileHash) int {
		return strings.Compare(a.Path, b.Path)
	})
	out.Skills = make([]core.SkillVersionRef, len(m.Skills))
	for i := range m.Skills {
		skill := m.Skills[i]
		skill.Files = slices.Clone(m.Skills[i].Files)
		if skill.Files == nil {
			skill.Files = []core.FileHash{}
		}
		slices.SortFunc(skill.Files, func(a, b core.FileHash) int {
			return strings.Compare(a.Path, b.Path)
		})
		out.Skills[i] = skill
	}
	slices.SortFunc(out.Skills, func(a, b core.SkillVersionRef) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// hashInput 返回参与 BundleHash 计算的字节：清单去掉 `BundleHash` 自身的规整 JSON。
// 字段顺序由结构体声明序固定，不含 map 与时间戳——同一内容必然同哈希。
// hashInput serializes the manifest without its own BundleHash.
func hashInput(m Manifest) ([]byte, error) {
	clone, err := normalizeManifest(m)
	if err != nil {
		return nil, err
	}
	clone.BundleHash = ""
	raw, err := json.Marshal(clone)
	if err != nil {
		return nil, fmt.Errorf("序列化 bundle 清单: %w", err)
	}
	return raw, nil
}

// computeBundleHash 计算清单的 BundleHash（小写十六进制 sha256）。
// computeBundleHash derives the bundle hash from the canonical manifest bytes.
func computeBundleHash(m Manifest) (string, error) {
	raw, err := hashInput(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// validateBuildSpec 校验构建输入的完整性。Agent 名与技能名的目录安全、UID 格式、
// 路径清单非空都在这里判——构建是**写入**面，错误必须发生在创建任何文件之前。
// validateBuildSpec refuses incomplete build input before any file is created.
func validateBuildSpec(spec BuildSpec) error {
	if err := safeSegment(spec.Agent); err != nil {
		return fmt.Errorf("非法的 Agent 名 %q: %v", spec.Agent, err)
	}
	if _, err := core.ParseAgentUID(string(spec.AgentUID)); err != nil {
		return fmt.Errorf("非法的 Agent UID %q: %w", spec.AgentUID, err)
	}
	if spec.ConfigHash == "" {
		return errors.New("构建 bundle 必须给出 config_hash")
	}
	if spec.ConfigRoot == "" {
		return errors.New("构建 bundle 必须给出配置文件根目录")
	}
	if len(spec.ConfigFiles) == 0 {
		return errors.New("构建 bundle 至少需要一个配置文件（AGENT.md）")
	}
	if spec.Dest == "" {
		return errors.New("构建 bundle 必须给出目标目录")
	}
	for _, source := range spec.Skills {
		if err := safeSegment(source.Ref.Name); err != nil {
			return fmt.Errorf("非法的技能名 %q: %v", source.Ref.Name, err)
		}
		if source.Root == "" {
			return fmt.Errorf("技能 %s 缺少来源目录", source.Ref.Name)
		}
		if _, err := core.ParseResourceUID(string(source.Ref.ResourceUID)); err != nil {
			return fmt.Errorf("技能 %s 的 UID 非法 %q: %w", source.Ref.Name, source.Ref.ResourceUID, err)
		}
		if len(source.Ref.Files) == 0 {
			return fmt.Errorf("技能 %s 的文件清单为空——发布记录必须列出至少一个文件", source.Ref.Name)
		}
	}
	return nil
}

// validateManifest 校验清单结构与规范化不变量：格式版本、身份格式、路径安全、
// 列表有序且无重复项、哈希形状。Verify 与 Resolve 的读路径共用这一份判据。
// validateManifest enforces the manifest invariants shared by every read path.
func validateManifest(m Manifest) error {
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("%w: 不支持的 bundle 格式版本 %d（只接受 %d）",
			core.ErrAssetVersionMismatch, m.FormatVersion, FormatVersion)
	}
	if err := safeSegment(m.Agent); err != nil {
		return fmt.Errorf("%w: 清单里的 Agent 名 %q 非法: %v", core.ErrAssetVersionMismatch, m.Agent, err)
	}
	canonical, err := core.ParseAgentUID(string(m.AgentUID))
	if err != nil || string(canonical) != string(m.AgentUID) {
		return fmt.Errorf("%w: 清单里的 Agent UID %q 非法", core.ErrAssetVersionMismatch, m.AgentUID)
	}
	if m.ConfigHash == "" {
		return fmt.Errorf("%w: 清单缺少 config_hash", core.ErrAssetVersionMismatch)
	}
	if !validHash(m.BundleHash) {
		return fmt.Errorf("%w: 清单的 bundle_hash %q 非法", core.ErrAssetVersionMismatch, m.BundleHash)
	}
	if err := validateFileList(m.ConfigFiles, "config_files"); err != nil {
		return err
	}
	lastSkill := ""
	for i, skill := range m.Skills {
		if err := safeSegment(skill.Name); err != nil {
			return fmt.Errorf("%w: 技能名 %q 非法: %v", core.ErrAssetVersionMismatch, skill.Name, err)
		}
		if i > 0 && skill.Name <= lastSkill {
			return fmt.Errorf("%w: 技能清单必须按 name 升序且不重复（%q 出现在 %q 之后）",
				core.ErrAssetVersionMismatch, skill.Name, lastSkill)
		}
		lastSkill = skill.Name
		if _, err := core.ParseResourceUID(string(skill.ResourceUID)); err != nil {
			return fmt.Errorf("%w: 技能 %s 的 UID %q 非法", core.ErrAssetVersionMismatch, skill.Name, skill.ResourceUID)
		}
		if skill.ContentHash == "" {
			return fmt.Errorf("%w: 技能 %s 缺少 content_hash", core.ErrAssetVersionMismatch, skill.Name)
		}
		if err := validateFileList(skill.Files, "技能 "+skill.Name); err != nil {
			return err
		}
	}
	return nil
}

// validateFileList 校验一组文件条目：路径安全、按 path 严格升序（同时排除重复）、
// 哈希形状合法。
// validateFileList enforces safe paths and strict ascending order without duplicates.
func validateFileList(files []core.FileHash, label string) error {
	last := ""
	for i, file := range files {
		if err := safeRelPath(file.Path); err != nil {
			return fmt.Errorf("%w: %s 的路径 %q 非法: %v", core.ErrAssetVersionMismatch, label, file.Path, err)
		}
		if i > 0 && file.Path <= last {
			return fmt.Errorf("%w: %s 必须按 path 升序且不重复（%q 出现在 %q 之后）",
				core.ErrAssetVersionMismatch, label, file.Path, last)
		}
		last = file.Path
		if !validHash(file.SHA256) {
			return fmt.Errorf("%w: %s 的文件 %q 哈希非法", core.ErrAssetVersionMismatch, label, file.Path)
		}
		if file.Size < 0 {
			return fmt.Errorf("%w: %s 的文件 %q 大小为负", core.ErrAssetVersionMismatch, label, file.Path)
		}
	}
	return nil
}

// sameManifest 逐项比对两份清单（含 BundleHash）；两边都来自本包的校验路径，
// 列表顺序已被规范化。
// sameManifest compares two validated manifests field by field.
func sameManifest(a, b Manifest) bool {
	return a.FormatVersion == b.FormatVersion &&
		a.Agent == b.Agent &&
		a.AgentUID == b.AgentUID &&
		a.ConfigHash == b.ConfigHash &&
		a.BundleHash == b.BundleHash &&
		slices.Equal(a.ConfigFiles, b.ConfigFiles) &&
		slices.EqualFunc(a.Skills, b.Skills, sameSkillRef)
}

// sameSkillRef 比较两个技能版本引用（含文件清单）。
func sameSkillRef(a, b core.SkillVersionRef) bool {
	return a.ResourceVersionRef == b.ResourceVersionRef && slices.Equal(a.Files, b.Files)
}

// totalFiles 统计清单里的文件总数（供 CLI 输出与测试核对）。
func totalFiles(m Manifest) int {
	total := len(m.ConfigFiles)
	for _, skill := range m.Skills {
		total += len(skill.Files)
	}
	return total
}

// —— 文件系统边界 ——

// bundlePath 拼出内容寻址目录：`<assetRoot>/bundles/<agent>/<bundleHash>`。
// Install / Resolve / 执行期读取三处都必须经由它（或逐字相同的手工拼接），
// 否则 `bundles/` 段会在某处漏掉（FR-24-44）。
// bundlePath is the single place the hash-addressed directory is spelled.
func bundlePath(assetRoot, agent, bundleHash string) string {
	return filepath.Join(assetRoot, bundlesDirName, agent, bundleHash)
}

// prepareDir 准备一个空的输出目录：不存在则创建；存在则必须为空目录。
// 非空目录一律拒绝——构建 / 导出写进已有内容会让结果不可复算。
// prepareDir creates an absent directory or accepts an empty one; non-empty is refused.
func prepareDir(dir, label string) error {
	if dir == "" {
		return fmt.Errorf("%s不能为空", label)
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if mkErr := os.MkdirAll(dir, dirPerm); mkErr != nil {
			return fmt.Errorf("创建%s %s: %w", label, dir, mkErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取%s %s: %w", label, dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s %s 不得是符号链接", label, dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %s 不是目录", label, dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("读取%s %s: %w", label, dir, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("%s %s 非空——必须指向一个空目录", label, dir)
	}
	return nil
}

// readManifest 读取并解析 `<dir>/bundle.json`。缺失 / 不可读 → `core.ErrAssetUnavailable`
// （fail-closed：看不见清单就不准入）；JSON 非法 → `core.ErrAssetVersionMismatch`
// （内容存在但与契约不符）。
// readManifest maps a missing manifest to unavailable and a malformed one to mismatch.
func readManifest(dir string) (Manifest, error) {
	if dir == "" {
		return Manifest{}, fmt.Errorf("%w: bundle 目录为空", core.ErrAssetUnavailable)
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{}, fmt.Errorf("%w: bundle 目录 %s 不存在", core.ErrAssetUnavailable, dir)
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: 读取 bundle 目录 %s: %v", core.ErrAssetUnavailable, dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Manifest{}, fmt.Errorf("%w: bundle 目录 %s 不是真实目录", core.ErrAssetUnavailable, dir)
	}
	// 清单按**裸名**从目录句柄读取（`os.Root`）：目录路径由调用方派生，根限定让读取
	// 不可能落在该目录之外；同时消除裸 `os.ReadFile(变量)` 在静态安全扫描里的判定。
	// The manifest is read by its bare name through a directory handle, which both
	// confines the read to that directory and avoids G304 on a variable path.
	handle, err := os.OpenRoot(dir)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: 打开 bundle 目录 %s: %v", core.ErrAssetUnavailable, dir, err)
	}
	raw, readErr := handle.ReadFile(manifestFileName)
	closeErr := handle.Close()
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return Manifest{}, fmt.Errorf("%w: %s 缺少 %s", core.ErrAssetUnavailable, dir, manifestFileName)
		}
		return Manifest{}, fmt.Errorf("%w: 读取 %s: %v", core.ErrAssetUnavailable, filepath.Join(dir, manifestFileName), readErr)
	}
	if closeErr != nil {
		return Manifest{}, fmt.Errorf("%w: 关闭 bundle 目录 %s: %v", core.ErrAssetUnavailable, dir, closeErr)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("%w: 解析 %s: %v", core.ErrAssetVersionMismatch, filepath.Join(dir, manifestFileName), err)
	}
	// 首个 JSON 值之后**必须**只剩空白：清单是完整文件的哈希输入，尾随任何字节都
	// 意味着内容被追加 / 损坏，必须按 ErrAssetVersionMismatch 拒收（FR-24-24）。
	// json.Decoder 默认在首个值后停下，不查尾随字节，这里显式补上 EOF 判据。
	// The manifest must end after its first JSON value; trailing bytes mean a damaged
	// or appended file and are rejected as ErrAssetVersionMismatch.
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("%w: 解析 %s: 清单 JSON 之后存在尾随内容", core.ErrAssetVersionMismatch, filepath.Join(dir, manifestFileName))
	}
	return manifest, nil
}

// writeManifest 把清单以紧凑 JSON 写进 bundle 根目录。写失败即构建失败——没有
// 清单的目录不是 bundle。
// writeManifest writes the canonical manifest bytes.
func writeManifest(dir string, manifest Manifest) error {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("序列化 bundle 清单: %w", err)
	}
	path := filepath.Join(dir, manifestFileName)
	if err := os.WriteFile(path, raw, filePerm); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	return nil
}

// fileDigest 是一次复制的复算结果；路径由调用方按清单语义另行给出。
// fileDigest is the digest of one copied file; the manifest path is supplied by the
// caller because layout-relative and manifest-relative paths differ.
type fileDigest struct {
	SHA256 string
	Size   int64
}

// copyBundleFile 从 `srcRoot/bundleRel` 复制到 `dstPath`，边复制边计算哈希；给出
// `expected` 时逐项比对（哈希 + 大小），不符即拒绝。源必须是普通文件且路径分量
// 不得是符号链接（不跟随符号链接）。
// copyBundleFile copies one file while hashing it, and refuses any disagreement with
// the expected digest.
func copyBundleFile(ctx context.Context, srcRoot, bundleRel, dstPath string, expected *core.FileHash) (fileDigest, error) {
	var result fileDigest
	source, err := openRegular(srcRoot, bundleRel)
	if err != nil {
		return result, err
	}
	defer source.Close()

	if err := os.MkdirAll(filepath.Dir(dstPath), dirPerm); err != nil {
		return result, fmt.Errorf("创建目录 %s: %w", filepath.Dir(dstPath), err)
	}
	// 目标文件经**父目录句柄**创建（`os.Root` + 裸名），拒绝逃出该目录的路径。
	// The destination is created through a parent-directory handle with its bare name.
	parent, err := os.OpenRoot(filepath.Dir(dstPath))
	if err != nil {
		return result, fmt.Errorf("打开目录 %s: %w", filepath.Dir(dstPath), err)
	}
	target, openErr := parent.OpenFile(filepath.Base(dstPath), os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
	parentCloseErr := parent.Close()
	if openErr != nil {
		return result, fmt.Errorf("创建文件 %s: %w", dstPath, openErr)
	}
	if parentCloseErr != nil {
		_ = target.Close() // 主错误是句柄关闭失败；目标文件随即丢弃，其关闭错误不再叠加
		return result, fmt.Errorf("关闭目录 %s: %w", filepath.Dir(dstPath), parentCloseErr)
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(target, hasher), source)
	closeErr := target.Close()
	if copyErr != nil {
		return result, fmt.Errorf("复制 %s: %w", bundleRel, copyErr)
	}
	if closeErr != nil {
		return result, fmt.Errorf("关闭 %s: %w", dstPath, closeErr)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	result.Size = written
	if expected != nil && (expected.SHA256 != result.SHA256 || expected.Size != result.Size) {
		return result, fmt.Errorf("%w: 文件 %s 与记录不符（记录 %s/%d，实际 %s/%d）",
			core.ErrAssetVersionMismatch, bundleRel, expected.SHA256, expected.Size, result.SHA256, result.Size)
	}
	return result, nil
}

// verifyBundleFile 按清单条目复算一个文件的哈希与大小。
// verifyBundleFile recomputes one file digest against its manifest entry.
func verifyBundleFile(ctx context.Context, dir, bundleRel string, expected core.FileHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := openRegular(dir, bundleRel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: 文件 %s 缺失", core.ErrAssetVersionMismatch, bundleRel)
		}
		return fmt.Errorf("%w: 读取 %s: %v", core.ErrAssetVersionMismatch, bundleRel, err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return fmt.Errorf("%w: 读取 %s: %v", core.ErrAssetVersionMismatch, bundleRel, err)
	}
	hasher := sha256.New()
	size, err := io.Copy(hasher, source)
	if err != nil {
		return fmt.Errorf("%w: 读取 %s: %v", core.ErrAssetVersionMismatch, bundleRel, err)
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	if size != expected.Size || sum != expected.SHA256 || info.Size() != expected.Size {
		return fmt.Errorf("%w: 文件 %s 与清单不符（清单 %s/%d，实际 %s/%d）",
			core.ErrAssetVersionMismatch, bundleRel, expected.SHA256, expected.Size, sum, size)
	}
	return nil
}

// compareExisting 判目标目录是否已存在且与来源逐项一致：不存在 → (false, nil)；
// 存在且一致 → (true, nil)；存在但不一致 → 错误（不覆盖、不静默替换）。
// compareExisting reports whether the target already exists and matches byte for byte.
func compareExisting(ctx context.Context, target string, source Manifest) (bool, error) {
	if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("读取目标目录 %s: %w", target, err)
	}
	existing, err := Verify(ctx, target)
	if err != nil {
		return false, fmt.Errorf("目标目录 %s 已存在但校验失败（拒绝覆盖）: %w", target, err)
	}
	if !sameManifest(existing, source) {
		return false, fmt.Errorf("%w: 目标目录 %s 已存在且内容不同（资产目录不可变，拒绝覆盖）",
			core.ErrAssetVersionMismatch, target)
	}
	return true, nil
}

// openRegular 逐分量拒绝符号链接后打开 `root/rel`：中间分量必须是真实目录，
// 末分量必须是普通文件。路径先经 safeRelPath 校验（非绝对、无 `..`、无反斜杠、
// 无盘符），因此 `root` 之外的任何位置都不可达。
// openRegular opens a regular file after refusing symlinks on every path component.
func openRegular(root, rel string) (*os.File, error) {
	if root == "" {
		return nil, errors.New("根目录为空")
	}
	if err := safeRelPath(rel); err != nil {
		return nil, err
	}
	parts := strings.Split(path.Clean(rel), "/")
	current := root
	for i, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("路径分量 %s 是符号链接（不跟随符号链接）", current)
		}
		if i < len(parts)-1 {
			if !info.IsDir() {
				return nil, fmt.Errorf("路径分量 %s 不是目录", current)
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("文件 %s 不是普通文件", current)
		}
	}
	// 最终打开经**根句柄 + 相对路径**（`os.Root`）：上面逐分量的 Lstat 是符号链接策略
	// 判据，根句柄是逃逸防线，两者叠加——不依赖裸 `os.Open(变量)`。
	// The final open goes through the root handle; the Lstat walk above stays the
	// symlink policy, and the handle adds the escape guard.
	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开根目录 %s: %w", root, err)
	}
	file, openErr := base.Open(filepath.FromSlash(path.Clean(rel)))
	baseCloseErr := base.Close()
	if openErr != nil {
		return nil, openErr
	}
	if baseCloseErr != nil {
		_ = file.Close() // 主错误是句柄关闭失败；文件句柄的关闭错误不再叠加
		return nil, fmt.Errorf("关闭根目录 %s: %w", root, baseCloseErr)
	}
	return file, nil
}

// safeRelPath 校验 bundle 内的**相对路径**：非空、正斜杠分隔、无空段、无 `.` /
// `..`、不以 `/` 开头、不含反斜杠 / 冒号（Windows 盘符与 ADS）/ NUL。
// 这是"路径逃逸拒绝"的唯一判据，清单与文件系统两侧共用。
// safeRelPath is the single path-escape predicate shared by manifests and the fs.
func safeRelPath(rel string) error {
	switch {
	case rel == "":
		return errors.New("路径不得为空")
	case strings.ContainsRune(rel, 0):
		return errors.New("路径不得含 NUL")
	case strings.Contains(rel, `\`):
		return errors.New("路径必须使用正斜杠分隔（不得含反斜杠）")
	case strings.ContainsRune(rel, ':'):
		return errors.New("路径不得含冒号（盘符 / 数据流）")
	case strings.HasPrefix(rel, "/"):
		return errors.New("路径不得为绝对路径")
	}
	for _, part := range strings.Split(rel, "/") {
		switch part {
		case "":
			return errors.New("路径不得含空段")
		case ".", "..":
			return fmt.Errorf("路径不得含 %q 段（路径逃逸）", part)
		}
	}
	return nil
}

// safeSegment 校验单段名字（Agent 名 / 技能名）：非空、非 `.` / `..`、不以点开头
// （点前缀保留给 staging 残骸）、不含分隔符 / 冒号 / NUL。目录命名策略的其余部分
// 归工作区（`workspace.ValidateAgentName`），本函数只回答"能不能作为目录段"。
// safeSegment answers only "may this be a single path segment".
func safeSegment(segment string) error {
	switch {
	case segment == "":
		return errors.New("名字不得为空")
	case segment == "." || segment == "..":
		return fmt.Errorf("名字不得为 %q", segment)
	case strings.HasPrefix(segment, "."):
		return errors.New("名字不得以点开头（保留给 staging 残骸）")
	case strings.ContainsAny(segment, "/\\:\x00"):
		return errors.New("名字不得含路径分隔符、冒号或 NUL")
	}
	return nil
}

// validHash 判 64 位小写十六进制 sha256。
// validHash accepts exactly 64 lowercase hex characters.
func validHash(value string) bool {
	if len(value) != hashHexLength {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
