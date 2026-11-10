package app

// 本文件是资产准入的**校验面**（US-24 §2.3.9 / FR-24-25）：把本地内容寻址目录与
// 共享发布记录对上。期望哈希的**唯一来源**是
// `resource_pointers(kind='agent_bundle', resource_id=<AgentUID>)` →
// `resource_versions.content_hash`；**不是** `config_snapshots`——快照记的是本进程
// 装配时的本地事实，用它当期望值等于让本地给自己判卷（P-19 的判据理由）。
//
// 校验是**无条件**不变量：装配没接校验器即失败关闭，不存在 `require_bundle` 这类
// 跳过开关（R2 已删除该配置键）。
// This file is the admission-side asset verifier. The expected hash comes only from the
// shared publication record, never from config_snapshots, and a missing verifier fails
// closed because verification has no bypass switch.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// AssetVerifier 是准入消费的资产校验面（接口定义在消费方）。返回本次准入固定的
// 资产包：哈希写进 Run 的清单并参与事务内复查，目录与逐文件清单交给执行期读
// （`core.AssetBundlePin`）。零值读作"未记录"（轻装配与既有快照兼容路径）。
// AssetVerifier returns the pinned bundle: the hash goes into the manifest and the
// directory plus file lists drive execution; the zero value means "unrecorded".
type AssetVerifier interface {
	VerifyBundle(ctx context.Context, agent string, uid core.AgentUID) (core.AssetBundlePin, error)
}

// assetVerifier 是 `AssetVerifier` 的生产实现：读共享发布指针 → 解析不可变目录 →
// 复算 manifest 与逐文件哈希 → 断言清单属于同一 Agent 身份。
// assetVerifier resolves the published pointer, then verifies the immutable directory
// end to end and checks the manifest belongs to the same agent identity.
type assetVerifier struct {
	versions *postgres.ResourceStore
	root     string
}

// 编译期断言：生产实现满足消费方端口。
// Compile-time assertion that the production verifier satisfies its consumer port.
var _ AssetVerifier = (*assetVerifier)(nil)

// newAssetVerifier 构造资产校验器。`root` 取 `assets.bundle_root`（必须非空，见
// worker 配置校验）；空值在准入处失败关闭，而不是退化成相对路径。
// newAssetVerifier builds the verifier over the configured bundle root.
func newAssetVerifier(versions *postgres.ResourceStore, root string) AssetVerifier {
	return &assetVerifier{versions: versions, root: root}
}

// VerifyBundle 校验一个 Agent 的资产包。错误分类固定（web 在 T035 映射）：
//   - 指针缺失 / 已软删除、本地目录缺失或不可读 → `core.ErrAssetUnavailable`（503）；
//   - 目录名、manifest 哈希、逐文件哈希、身份任一项不符 → `core.ErrAssetVersionMismatch`（409）。
//
// 两者都**不**静默换版本：换成"最新"会让在途 Run 读到从未校验过的内容。
// VerifyBundle classifies failures as unavailable (503) or version mismatch (409) and
// never silently switches to "latest".
func (v *assetVerifier) VerifyBundle(ctx context.Context, agent string, uid core.AgentUID) (core.AssetBundlePin, error) {
	if v == nil || v.versions == nil {
		return core.AssetBundlePin{}, fmt.Errorf("%w: 资产校验器未接资源仓储", core.ErrAssetUnavailable)
	}
	if strings.TrimSpace(v.root) == "" {
		return core.AssetBundlePin{}, fmt.Errorf("%w: 资产根为空（assets.bundle_root 必须非空）", core.ErrAssetUnavailable)
	}
	pointer, err := v.versions.PointerByUID(ctx, core.ResourceAgentBundle, core.ResourceUID(uid))
	if err != nil {
		if errors.Is(err, core.ErrResourceNotFound) {
			return core.AssetBundlePin{}, fmt.Errorf("%w: Agent %s 没有资产发布指针，先执行 `basalt assets install`",
				core.ErrAssetUnavailable, agent)
		}
		return core.AssetBundlePin{}, err
	}
	if pointer.DeletedAt != nil {
		return core.AssetBundlePin{}, fmt.Errorf("%w: Agent %s 的资产发布指针已删除", core.ErrAssetUnavailable, agent)
	}
	version, err := v.versions.VersionByID(ctx, pointer.PublishedVersionID)
	if err != nil {
		return core.AssetBundlePin{}, err
	}
	// 归属核对：版本行必须属于 agent_bundle 且 `resource_id` 就是本身份（两者同值，
	// 见 `ResourceAgentBundle` 的身份适配）。不核对会让别的种类的版本行冒充资产发布。
	if version.Kind != core.ResourceAgentBundle || version.ResourceID != string(uid) {
		return core.AssetBundlePin{}, fmt.Errorf("%w: Agent %s 的资产版本 %d 归属不符（got %s/%s）",
			core.ErrAssetVersionMismatch, agent, version.ID, version.Kind, version.ResourceID)
	}
	expected := strings.TrimSpace(version.ContentHash)
	if expected == "" {
		return core.AssetBundlePin{}, fmt.Errorf("%w: Agent %s 的资产版本 %d 没有内容哈希",
			core.ErrAssetVersionMismatch, agent, version.ID)
	}
	return v.verifyAt(ctx, agent, uid, expected)
}

// VerifyBundleAt 按**给定的固定哈希**解析并校验资产包（US-25C §2.3.5.1 T2）：不读
// 发布指针——领取时钉住的哈希就是权威，同步即使已推进到新版本，本次 Run 仍只认这一份。
// 目录缺失 / 不可读 / 身份或哈希不符一律 `core.ErrAssetUnavailable`（本实例现在给不出
// 领取钉住的那一份），不退回指针目录；目录内容与清单逐文件不符仍按版本不符拒绝。
// VerifyBundleAt verifies the claimed hash without reading the publication pointer; a
// missing or mismatched directory is unavailable rather than "switch to latest".
func (v *assetVerifier) VerifyBundleAt(ctx context.Context, agent string, uid core.AgentUID,
	hash string,
) (core.AssetBundlePin, error) {
	if v == nil || v.versions == nil {
		return core.AssetBundlePin{}, fmt.Errorf("%w: 资产校验器未接资源仓储", core.ErrAssetUnavailable)
	}
	if strings.TrimSpace(v.root) == "" {
		return core.AssetBundlePin{}, fmt.Errorf("%w: 资产根为空（assets.bundle_root 必须非空）", core.ErrAssetUnavailable)
	}
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return core.AssetBundlePin{}, fmt.Errorf("%w: Agent %s 的钉住 bundle 哈希为空", core.ErrAssetUnavailable, agent)
	}
	pin, err := v.verifyAt(ctx, agent, uid, hash)
	if err != nil {
		// 钉住路径的错误分类统一为"不可用"：对本次 Run 而言，领取钉住的那一版
		// 在本实例读不出来就是服务不了，而不是"有一个可用的新版本"。
		// A pinned verification failure always means this version is unserviceable here.
		if errors.Is(err, core.ErrAssetVersionMismatch) {
			return core.AssetBundlePin{}, fmt.Errorf("%w: %v", core.ErrAssetUnavailable, err)
		}
		return core.AssetBundlePin{}, err
	}
	return pin, nil
}

// verifyAt 是两条校验路径共用的尾部：按期望哈希定位不可变目录、逐文件复核、断言清单
// 属于同一 Agent 身份。目录缺失 / 不可读由 `assets.Resolve` / `Verify` 以
// `ErrAssetUnavailable` 报出；内容与记录不符以 `ErrAssetVersionMismatch` 报出。
// verifyAt shares the resolve/verify/identity tail of both verification paths.
func (v *assetVerifier) verifyAt(ctx context.Context, agent string, uid core.AgentUID,
	expected string,
) (core.AssetBundlePin, error) {
	dir, err := assets.Resolve(ctx, v.root, agent, expected)
	if err != nil {
		return core.AssetBundlePin{}, err
	}
	manifest, err := assets.Verify(ctx, dir)
	if err != nil {
		return core.AssetBundlePin{}, err
	}
	// 清单身份必须与本次准入的 Agent 同一：目录名与哈希对得上、身份对不上，说明安装
	// 时把另一个 Agent 的内容装进了本名字下——这不是版本漂移，是身份错位，仍拒绝。
	// The manifest must belong to this very agent; a hash-consistent directory holding
	// another agent's content is an identity mix-up, not a version drift.
	if manifest.Agent != agent || manifest.AgentUID != uid {
		return core.AssetBundlePin{}, fmt.Errorf("%w: 目录 %s 的清单属于 agent=%s uid=%s",
			core.ErrAssetVersionMismatch, dir, manifest.Agent, manifest.AgentUID)
	}
	// 三种路径逐字同一（FR-24-44）：`Dir` 就是 Resolve 的返回值，执行期以它为读取前缀。
	// The returned Dir is the very value Resolve returned; execution reads under it.
	return core.AssetBundlePin{
		Hash:        expected,
		Dir:         dir,
		ConfigFiles: manifest.ConfigFiles,
		Skills:      manifest.Skills,
	}, nil
}

// undeclaredAssets 是"本装配不声明资产版本"的显式实现（测试与轻装配用）：返回零值，
// 读作"未记录"，既有快照兼容路径据此跳过事务内资产复查、执行期退回工作副本口径。
// 它**不是**"跳过校验"的开关——生产装配接的是 `assetVerifier`，装配缺校验器（nil）
// 在准入处失败关闭。
// undeclaredAssets is the explicit "no asset version declared" implementation used by
// tests and light assemblies; nil is still refused at admission.
type undeclaredAssets struct{}

// VerifyBundle 不声明任何资产包。
// VerifyBundle declares no bundle.
func (undeclaredAssets) VerifyBundle(context.Context, string, core.AgentUID) (core.AssetBundlePin, error) {
	return core.AssetBundlePin{}, nil
}

var _ AssetVerifier = undeclaredAssets{}
