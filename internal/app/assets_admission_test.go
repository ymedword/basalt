//go:build integration

package app

// 本文件是 US-24 T029 的准入一致性验收面（模块卡 §4.4 / FR-24-25 / FR-24-42 / P-16 /
// P-17）。判的是**只有共享持久事实才成立**的四组性质：
//
//	A 资产包一致：期望值只来自共享发布记录；本地目录逐文件哈希不符 / 缺失一律拒绝；
//	  新安装不改旧 Run 固定的原 hash 目录。
//	B 本地实例版本证明：构建证据与目标版本 + 凭据 pin 不一致 → 按**同一目标版本**
//	  重建-换入-再比；仍不符 503 RESOURCE_VERSION_MISMATCH。MCP 的基准是 applied，
//	  准入**不得**隐式 apply。
//	C 事务内复查：指针在准入窗口内被推进、索引代次与期望不符 / 缺失 → 拒绝且零 Run 行。
//	D 启动基线 P-17：本地代次高于索引 → 不写索引、不写第二条基线操作。
//
// 判别力来源：替身都**每次调用新建视图**（生产目录是写时复制），否则"重建后重取"
// 会拿到重建前的同一证据，再比恒成立——判据恒假绿。逆改点写在每个用例头部。
// The doubles mint a fresh view per call, mirroring the copy-on-write catalogue; reusing
// one instance would make every rebuild self-confirming.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/provider"
	"github.com/ymedword/basalt/internal/storage/postgres"
	"github.com/ymedword/basalt/internal/workspace"
)

// —— 替身 ——

// builtProviderRouter 是 `pinnedProviderService` 的替身：**每次 Pinned 都按当前证据
// 新建一个** `provider.ProviderService`。`WithBuiltVersion` 是就地写（装配期一次性
// 配置），复用同一实例时"重建后重取"拿到的是同一份证据，`ensureProviderInstance`
// 的再比恒成立——判据恒假绿。
// builtProviderRouter mints a fresh service per call, mirroring the copy-on-write
// provider catalogue.
type builtProviderRouter struct {
	clients map[string]core.ProviderClient
	models  map[string]string

	mu    sync.Mutex
	built map[string]core.BuiltVersion
}

var _ pinnedProviderService = (*builtProviderRouter)(nil)

// newBuiltProviderRouter 建替身；初始证据为空（未声明），由用例逐条设置。
// newBuiltProviderRouter starts with undeclared proofs; cases set them explicitly.
func newBuiltProviderRouter(clients map[string]core.ProviderClient,
	models map[string]string,
) *builtProviderRouter {
	return &builtProviderRouter{clients: clients, models: models, built: make(map[string]core.BuiltVersion)}
}

// setBuilt 换掉某个 Provider 的构建证据（模拟"另一条路径发布了新版本 / 轮换了凭据"）。
// setBuilt swaps one provider's build proof.
func (r *builtProviderRouter) setBuilt(name string, built core.BuiltVersion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.built[name] = built
}

// Pinned 每次新建视图，重建后的重取因此能看到新证据。
// Pinned builds a fresh view so the post-rebuild re-fetch observes the new proof.
func (r *builtProviderRouter) Pinned(name string) (*provider.ProviderService, bool) {
	client, ok := r.clients[name]
	if !ok {
		return nil, false
	}
	r.mu.Lock()
	built := r.built[name]
	r.mu.Unlock()
	clients := map[string]core.ProviderClient{name: client}
	models := make(map[string]string, 1)
	if model, ok := r.models[name]; ok {
		models[name] = model
	}
	service := provider.NewProviderService(clients, models, nil, slog.New(slog.DiscardHandler))
	return service.WithBuiltVersion(built), true
}

// SummaryPinned 交出同一份裸协议客户端（摘要面不在本文件判据内）。
// SummaryPinned hands out the same raw client; the summary path is not under test here.
func (r *builtProviderRouter) SummaryPinned(name string) (core.ProviderClient, bool) {
	client, ok := r.clients[name]
	return client, ok
}

// recordingRebuild 是 `ResourceRebuildPort` 的记录型替身：记录调用次数与**收到的目标**，
// 由钩子决定"重建成功（并换入新证据）/ 失败 / 无操作"。每个用例都必须能回答
// "重建收到的是不是**同一目标版本**"——按别的版本重建出来的实例会被误当成修复成功。
// recordingRebuild records calls and the target each one received.
type recordingRebuild struct {
	mu sync.Mutex

	providerCalls int
	mcpCalls      int
	lastProvider  core.ResourceVersionRef
	lastPin       core.CredentialPin
	lastMCP       core.ResourceVersionRef
	providerErr   error
	onProvider    func(core.ResourceVersionRef, core.CredentialPin) error
	onMCP         func(core.ResourceVersionRef) error
}

var _ ResourceRebuildPort = (*recordingRebuild)(nil)

// RebuildProvider 先记录再走钩子；钩子与注入错误都原样上抛（判"重建失败 → 503"）。
// RebuildProvider records first, then applies the hook; a hook error propagates.
func (r *recordingRebuild) RebuildProvider(_ context.Context, target core.ResourceVersionRef,
	pin core.CredentialPin,
) error {
	r.mu.Lock()
	r.providerCalls++
	r.lastProvider, r.lastPin = target, pin
	hook, failure := r.onProvider, r.providerErr
	r.mu.Unlock()
	if hook != nil {
		if err := hook(target, pin); err != nil {
			return err
		}
	}
	return failure
}

// RebuildMCP 同形；`target` 必须是**已应用**版本，不是发布指针。
// RebuildMCP records and delegates; the target must be the applied version.
func (r *recordingRebuild) RebuildMCP(_ context.Context, target core.ResourceVersionRef) error {
	r.mu.Lock()
	r.mcpCalls++
	r.lastMCP = target
	hook := r.onMCP
	r.mu.Unlock()
	if hook != nil {
		if err := hook(target); err != nil {
			return err
		}
	}
	return nil
}

// mcpRebuildCount 读 MCP 重建次数（"尚未 apply"的发布版本**不得**触发重建）。
func (r *recordingRebuild) mcpRebuildCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mcpCalls
}

// providerRebuildCount 读 Provider 重建次数。
func (r *recordingRebuild) providerRebuildCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.providerCalls
}

// —— 夹具 ——

// assetsAdmissionEnv 在 `runResourcesEnv` 之上补三件准入真正需要的东西：内容寻址资产根、
// 重建接缝、以及每次新建视图的 Provider 路由替身。
// assetsAdmissionEnv adds the asset root, the rebuild seam, and the copy-on-write router.
type assetsAdmissionEnv struct {
	*runResourcesEnv
	assetRoot string
	agentUID  core.AgentUID
	agentDir  string
	router    *builtProviderRouter
	rebuild   *recordingRebuild
	mcp       *mcpRuntime
	deps      AdmissionDeps
}

// newAssetsAdmissionEnv 建夹具：写入 AGENT.md 目录、建空资产根、把**生产**资产校验器
// 与重建接缝接进准入依赖。校验器不走替身——替身判不出"期望哈希来自共享发布记录"。
// newAssetsAdmissionEnv wires the production verifier and the recording rebuild seam.
func newAssetsAdmissionEnv(t *testing.T) *assetsAdmissionEnv {
	t.Helper()
	base := newRunResourcesEnv(t)
	ctx := context.Background()

	uid, _, found, err := base.versions.AgentIdentity(ctx, "writer")
	require.NoError(t, err)
	require.True(t, found, "夹具必须先有 Agent 身份行（bundle 的键是 AgentUID）")

	agentDir := filepath.Join(base.root, agentsDirName, "writer")
	require.NoError(t, os.MkdirAll(agentDir, 0o755))

	rebuild := &recordingRebuild{}
	assetRoot := filepath.Join(t.TempDir(), "assets")
	deps := base.admission
	deps.Assets = newAssetVerifier(base.versions, assetRoot)
	deps.Rebuild = rebuild

	return &assetsAdmissionEnv{
		runResourcesEnv: base,
		assetRoot:       assetRoot,
		agentUID:        uid,
		agentDir:        agentDir,
		router:          newBuiltProviderRouter(base.providers.clients, base.providers.models),
		rebuild:         rebuild,
		mcp:             base.mcp,
		deps:            deps,
	}
}

// resources 按当前依赖重新装配准入解析端口（用例换 MCP 运行时 / 凭据解析器后调用）。
// resources re-assembles the admission port with the fixture's current dependencies.
func (e *assetsAdmissionEnv) resources() core.RunResources {
	return NewRunResources(e.versions, e.providers, e.router, e.mcp, e.skills, e.gate, e.deps)
}

// resolve 走一次真实 `Resolve`；成功路径把读锁租约交给清理函数（与生产交接同形：
// 调用方在 StartRun 之后归还）。
// resolve runs one real Resolve and hands the read-lock lease to t.Cleanup on success.
func (e *assetsAdmissionEnv) resolve(t *testing.T, ctx context.Context,
	entry core.AgentEntry,
) (core.ResolvedResources, error) {
	t.Helper()
	resolved, err := e.resources().Resolve(ctx, entry)
	if err == nil {
		t.Cleanup(resolved.Runtime.Release)
	}
	return resolved, err
}

// buildAndInstall 按给定 AGENT.md 内容构建 bundle 并装进不可变目录；布局与逐文件哈希
// 由**生产**构建器给出，用例不复制一份。
//
// 配置路径用**工作区相对**写法（`agents/writer/AGENT.md`）并与 `e.root` 配对——这正是
// `buildWorkspaceBundle` 与执行期 `SnapshotBundle` 的同一基准；写成 Agent 目录相对会
// 让夹具自证一个生产上不存在的形态（F20 复核发现的偏差）。
// buildAndInstall builds and installs a bundle through the production code paths, using
// the same workspace-relative config paths as the production builder.
func (e *assetsAdmissionEnv) buildAndInstall(t *testing.T, ctx context.Context,
	content string,
) assets.Installed {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(e.agentDir, "AGENT.md"), []byte(content), 0o644))
	staging := filepath.Join(t.TempDir(), "staging")
	_, err := assets.Build(ctx, assets.BuildSpec{
		Agent:       "writer",
		AgentUID:    e.agentUID,
		ConfigHash:  sha256Hex(t, []byte(content)),
		ConfigRoot:  e.root,
		ConfigFiles: []string{"agents/writer/AGENT.md"},
		Dest:        staging,
	})
	require.NoError(t, err)
	installed, err := assets.Install(ctx, staging, e.assetRoot)
	require.NoError(t, err)
	return installed
}

// publishBundle 把已安装的 bundle 写进共享发布记录，返回发布版本 ID。
// publishBundle writes the installed bundle into the shared record.
func (e *assetsAdmissionEnv) publishBundle(t *testing.T, installed assets.Installed) int64 {
	t.Helper()
	versionID, err := newBundlePublisher(e.versions, e.ops, postgres.NewBundlePayloadStore(e.db), defaultPayloadMaxBytes, nil, time.Now).
		Publish(context.Background(), installed)
	require.NoError(t, err)
	return versionID
}

// pinOf 从**库里的**凭据版本行重建期望 pin：目标绑定取落库值，不按用例入参重算
// ——重算会让"pin 从哪来"退化成用例自说自话。
// pinOf rebuilds the expected pin from the stored credential-version row.
func (e *assetsAdmissionEnv) pinOf(t *testing.T, credID string, versionNo int) core.CredentialPin {
	t.Helper()
	record, err := e.credentials.Version(context.Background(), credID, versionNo)
	require.NoError(t, err)
	return core.CredentialPin{
		Mode:          core.CredentialModeManaged,
		ID:            credID,
		Version:       record.VersionNo,
		TargetBinding: record.TargetBinding,
	}
}

// appendCredentialVersion 追加一版托管凭据（目标绑定不变，只换材料与版本号）。
// appendCredentialVersion appends one rotated version with the same target binding.
func (e *assetsAdmissionEnv) appendCredentialVersion(t *testing.T, credID, baseURL string) {
	t.Helper()
	uid := fixtureResourceUID(t, core.ResourceModelConnection, "alpha")
	require.NoError(t, e.credentials.AppendVersion(context.Background(), credID, "master/v1", e.opID,
		core.CredentialVersionRecord{
			KeyID:         "master/v1",
			TargetBinding: core.TargetBinding{Kind: core.BindingModelConnection, Owner: uid, BaseURL: baseURL}.Hash(),
			Last4:         "0002",
			Nonce:         make([]byte, 12),
			Ciphertext:    []byte("sealed:alpha:v2"),
			OperationID:   e.opID,
		}, baselineClock()))
}

// prepareModel 为 MCP 用例备好模型侧的成功前提（资产包 + 已发布连接）。
// prepareModel prepares the model-side success precondition for the MCP cases.
func (e *assetsAdmissionEnv) prepareModel(t *testing.T, ctx context.Context) {
	t.Helper()
	bundle := e.buildAndInstall(t, ctx, "# writer\n\n版本一\n")
	e.publishBundle(t, bundle)
	e.publishModel(t)
}

// publishModel 只播种并发布 "alpha" 模型连接（env 凭据档），不碰资产包：资产用例要自己
// 控制"共享发布记录先缺失、后出现"的顺序，不能被一条顺手带上的连接发布搅乱。
// publishModel publishes the alpha model connection alone so asset cases keep control
// over when the shared records appear.
func (e *assetsAdmissionEnv) publishModel(t *testing.T) {
	t.Helper()
	version := e.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", "https://api.example.com", "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	e.publish(t, core.ResourceModelConnection, "alpha", version.ID, true)
}

// TestAdmission_RejectsAssetMismatch 覆盖资产包的三条拒绝路径与"旧 Run 固定原目录"。
//
// 逆改点：
//   - `verifyAssets` 改用工作副本当期望值 → 第 3 步"新发布后旧目录仍可解析"失去意义；
//   - `VerifyBundle` 跳过逐文件 `assets.Verify` → 第 4 步损坏文件不再被拒；
//   - 把 `ErrAssetUnavailable` 与 `ErrAssetVersionMismatch` 归一 → 第 1 / 4 步的
//     `errors.Is` 分类断言各自红。
func TestAdmission_RejectsAssetMismatch(t *testing.T) {
	ctx := context.Background()
	env := newAssetsAdmissionEnv(t)
	entry := agentEntry("alpha", nil, nil)

	// ① 共享发布记录缺失：本地就算有目录也不能放行（期望值唯一来源是共享记录）。
	_, err := env.resolve(t, ctx, entry)
	failure := requireUnavailable(t, err, resourceReasonAsset)
	require.ErrorIs(t, failure.Cause, core.ErrAssetUnavailable)

	// ② 发布 H1 后准入固定 H1（模型连接同时就绪：本用例判资产，其余资源必须能放行）。
	first := env.buildAndInstall(t, ctx, "# writer\n\n版本一\n")
	require.NotEmpty(t, first.Manifest.BundleHash)
	env.publishBundle(t, first)
	env.publishModel(t)

	resolved, err := env.resolve(t, ctx, entry)
	require.NoError(t, err)
	require.Equal(t, first.Manifest.BundleHash, resolved.AssetBundle.Hash)

	// ③ 装并发布 H2：准入固定 H2，而**旧 Run** 仍按 H1 解析到原目录（执行期不漂移）。
	second := env.buildAndInstall(t, ctx, "# writer\n\n版本二\n")
	require.NotEqual(t, first.Manifest.BundleHash, second.Manifest.BundleHash)
	env.publishBundle(t, second)

	resolved, err = env.resolve(t, ctx, entry)
	require.NoError(t, err)
	require.Equal(t, second.Manifest.BundleHash, resolved.AssetBundle.Hash)
	require.NotEqual(t, first.Dir, second.Dir, "两个 hash 必须落在两个内容寻址目录")

	oldDir, err := assets.Resolve(ctx, env.assetRoot, "writer", first.Manifest.BundleHash)
	require.NoError(t, err)
	require.Equal(t, first.Dir, oldDir)
	_, err = assets.Verify(ctx, oldDir)
	require.NoError(t, err, "新安装不得改写旧 Run 固定的原 hash 目录")

	// ④ 本地目录被改坏：逐文件哈希与清单不符 → 版本不符（不是"不可用"）。
	// 包内路径经 `assets.ConfigBundleRel` 派生（布局映射只有一处），不手拼：夹具手拼
	// 一次前缀就会改到清单之外的文件上，让"被篡改"变成空过（F20 复核后的实测）。
	corrupted := filepath.Join(second.Dir, filepath.FromSlash(assets.ConfigBundleRel("agents/writer/AGENT.md")))
	require.FileExists(t, corrupted, "篡改点必须落在清单列出的文件上")
	require.NoError(t, os.WriteFile(corrupted, []byte("# writer\n\n被篡改\n"), 0o644))
	_, err = env.resolve(t, ctx, entry)
	failure = requireUnavailable(t, err, resourceReasonAsset)
	require.ErrorIs(t, failure.Cause, core.ErrAssetVersionMismatch)

	// ⑤ 目录整体消失（另一台机器还没装到本地）→ 不可用。
	require.NoError(t, os.Rename(second.Dir, second.Dir+".gone"))
	_, err = env.resolve(t, ctx, entry)
	failure = requireUnavailable(t, err, resourceReasonAsset)
	require.ErrorIs(t, failure.Cause, core.ErrAssetUnavailable)
}

// fileHashPaths 取 bundle 清单里的配置路径序列（判断"包里装了哪几个文件"）。
// fileHashPaths lists the bundle manifest's config paths.
func fileHashPaths(files []core.FileHash) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return paths
}

// manifestPaths 取冻结清单里的路径序列（`BuildConfigSnapshot` 已按路径排序）。
// manifestPaths lists the frozen manifest's paths, already sorted by the builder.
func manifestPaths(entries []core.ManifestEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

// TestAdmission_ExecutionReadsPinnedBundleNotWorkspace 在生产链上判 FR-24-42 / FR-24-44：
// 构建入口 → Install → 准入固定目录 → 执行期读取必须是同一条工作区相对路径，且工作
// 副本在发布后被改写时，快照仍然只注入 bundle 里的字节（AGENT.md 与 bootstrap 文件）。
//
// 逆改点：
//   - `runSnapshotBuilder.Build` 退回 `SnapshotPublished`（读工作副本）→ 第 4 步的
//     "工作副本正文 / 工作副本版本"被注入，用例红；
//   - `buildWorkspaceBundle` 的 `ConfigRoot` 换回 Agent 目录（旧约定）→ 第 1 步清单
//     路径与执行期读取的 `agents/writer/AGENT.md` 对不上，构建即失败；
//   - `assetVerifier` 不回填 `Dir` → `resolved.AssetBundle.Dir` 为空、执行期走工作
//     副本分支，第 3 / 4 步红。
func TestAdmission_ExecutionReadsPinnedBundleNotWorkspace(t *testing.T) {
	ctx := context.Background()
	env := newAssetsAdmissionEnv(t)
	entry := agentEntry("alpha", nil, nil)

	// 工作副本：AGENT.md 声明一个工作区根下的 bootstrap 文件，两者都带可区分的标记。
	definition := "---\nname: writer\nbootstrap:\n  - policy.md\n---\n\n# writer\n\nbundle 正文\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.agentDir, "AGENT.md"), []byte(definition), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(env.root, "policy.md"), []byte("# 策略\n\nbundle 版本\n"), 0o644))

	// ① 生产构建入口从工作区构建：清单是工作区相对路径，与执行期读取面同一基准。
	staging := filepath.Join(t.TempDir(), "staging")
	built, err := buildWorkspaceBundle(ctx, workspaceBundleDeps{Root: env.root, Versions: env.versions},
		"writer", env.agentUID, staging)
	require.NoError(t, err)
	require.Equal(t, []string{"agents/writer/AGENT.md", "policy.md"}, fileHashPaths(built.ConfigFiles),
		"bundle 清单必须是工作区相对路径（bootstrap 条目保持其工作区相对写法）")

	// ② 安装 → 发布 → 准入固定到同一个内容寻址目录。
	installed, err := assets.Install(ctx, staging, env.assetRoot)
	require.NoError(t, err)
	env.publishBundle(t, installed)
	env.publishModel(t)

	resolved, err := env.resolve(t, ctx, entry)
	require.NoError(t, err)
	require.Equal(t, installed.Dir, resolved.AssetBundle.Dir,
		"准入固定目录必须逐字等于 Install 的返回值（FR-24-44）")
	require.Contains(t, filepath.ToSlash(resolved.AssetBundle.Dir), "/bundles/")

	// ③ 发布后改写工作副本（热加载尚未生效，或另一 Worker 装了另一版）：执行期不得再读它。
	require.NoError(t, os.WriteFile(filepath.Join(env.agentDir, "AGENT.md"),
		[]byte("---\nname: writer\nbootstrap:\n  - policy.md\n---\n\n# writer\n\n工作副本正文\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(env.root, "policy.md"), []byte("# 策略\n\n工作副本版本\n"), 0o644))

	// ④ 快照只来自 bundle：AGENT.md 与 bootstrap 都是发布时的那一份。
	snapshot, err := newRunSnapshotBuilder(workspace.NewContextLoader(nil), env.root, baselineClock).
		Build(ctx, entry, resolved)
	require.NoError(t, err)
	require.Contains(t, snapshot.SectionOne, "bundle 正文")
	require.NotContains(t, snapshot.SectionOne, "工作副本正文")
	require.Contains(t, snapshot.SectionOne, "bundle 版本")
	require.NotContains(t, snapshot.SectionOne, "工作副本版本")
	require.Equal(t, definition, snapshot.Content, "content 列存的是 bundle 内 AGENT.md 全文")
	require.Equal(t, []string{"agents/writer/AGENT.md", "policy.md"}, manifestPaths(snapshot.Manifest),
		"冻结清单与 bundle 清单必须是同一批文件")
}

// TestAdmission_RejectsWorkCopyDefinitionDriftFromPinnedBundle 判 review ② finding 6：
// **清单侧**（`entry.Profile`，注册表装载时解析工作副本 `AGENT.md`）与**执行侧**
// （`runSnapshotBuilder.Build` 重读资产包内同名文件并重新解析）必须是同一份定义字节。
// 工作副本在发布后被改写（合法热载 / 发布链路失败）时，两处会给出不同的 Provider /
// 模型：清单记 M1、实际调用 M2，且两边都各自"验过"，谁都不报错。
//
// 判据分三步：① 正控——两边同源时模型一致（证明判据不是恒红）；② 直接在生产读取面上
// 展示缺陷形态（清单侧 M1 ≠ 执行侧 M2）——这一步**不经过**新加的守卫；③ 准入门拒绝
// 并给出 409 `ASSET_VERSION_MISMATCH`。
//
// 逆改点：
//   - 删掉 `checkDefinitionMatchesBundle` → 第 ③ 步 `resolve` 成功，用例红；
//   - 守卫改成只判"包内记录了 AGENT.md"而不比哈希 → 第 ③ 步仍放行，用例红（第 ② 步
//     已经证明内容确实分叉）；
//   - `SnapshotBundle` 退回 `entry.Definition` / 工作副本 → 第 ② 步两边相等，演示不成立。
func TestAdmission_RejectsWorkCopyDefinitionDriftFromPinnedBundle(t *testing.T) {
	ctx := context.Background()
	env := newAssetsAdmissionEnv(t)

	// 定义必须自带与夹具一致的绑定（R4.4）：UID 由同一 (kind, name) 派生，`publishModel`
	// 发布的连接正是这个身份——否则第 ① 步会先因绑定失败，掩盖本用例判的定义一致性。
	uid := fixtureResourceUIDFor(core.ResourceModelConnection, "alpha")
	definition := func(model string) string {
		return fmt.Sprintf(`---
name: writer
identity:
  agent_name: 定义一致性
  prompt: 你是用于验证定义一致性的助手。
provider:
  name: alpha
  model: %s
resource_bindings:
  model_connection:
    - name: alpha
      uid: %s
  notify_channel: []
---

# writer

定义正文
`, model, uid)
	}
	released := definition("alpha-model")

	bundle := env.buildAndInstall(t, ctx, released)
	env.publishBundle(t, bundle)
	env.publishModel(t)

	entry := agentEntry("alpha", nil, nil)
	entry.Definition = []byte(released)

	// ① 正控：工作副本与资产包同源时，清单模型 = 执行侧模型。
	resolved, err := env.resolve(t, ctx, entry)
	require.NoError(t, err)
	require.Equal(t, "alpha-model", resolved.ProviderModel)

	builder := newRunSnapshotBuilder(workspace.NewContextLoader(nil), env.root, baselineClock)
	snapshot, err := builder.Build(ctx, entry, resolved)
	require.NoError(t, err)
	require.Equal(t, resolved.ProviderModel, snapshot.Profile.Provider.Model,
		"正控：两边同源时不得出现模型分叉")
	require.Equal(t, released, snapshot.Content)

	// ② 工作副本被改写（热载已换掉注册表条目，但尚未重新发布资产包）。
	drifted := definition("rotated-model")
	require.NoError(t, os.WriteFile(filepath.Join(env.agentDir, "AGENT.md"), []byte(drifted), 0o644))
	driftEntry := entry
	driftEntry.Definition = []byte(drifted)

	// 清单侧来自注册表持有的已校验字节（`SnapshotPublished` 与装载期同一条读取面）。
	manifestSide, err := workspace.NewContextLoader(nil).
		SnapshotPublished(env.root, driftEntry, nil, baselineClock())
	require.NoError(t, err)
	require.Equal(t, "rotated-model", manifestSide.Profile.Provider.Model)

	// 执行侧仍取资产包内的原文件：两边模型不同，正是本用例要挡住的分叉。
	execSide, err := builder.Build(ctx, driftEntry, resolved)
	require.NoError(t, err)
	require.Equal(t, "alpha-model", execSide.Profile.Provider.Model)
	require.NotEqual(t, manifestSide.Profile.Provider.Model, execSide.Profile.Provider.Model,
		"缺陷形态：清单侧与执行侧解析出不同的配置")

	// ③ 准入门拒绝新 Run，并把用户指向重新安装（工作副本与已发布资产包对齐）。
	_, err = env.resolve(t, ctx, driftEntry)
	failure := requireUnavailable(t, err, resourceReasonAsset)
	require.ErrorIs(t, failure.Cause, core.ErrAssetVersionMismatch)
	require.Contains(t, failure.Cause.Error(), "assets install")
}

// TestAssets_FirstDeployFromEmptyWorkspace —— §2.3.9 的首次初始化必须能按
// Operations.md 写的样子出货：**空资产根 + 无共享指针**时，`assets install --agent`
// 从本机工作区构建首个 bundle 并推进共享发布记录，之后准入才放行。判据不经过任何
// 测试自拼的构建 / 发布旁路——命令面收到的就是生产构建器与生产发布器。
//
// 逆改点：
//   - 命令面删掉 `install --agent` 的 Build 分支 → 第 ② 步报"构建未接线"，用例红；
//   - 发布不推进指针（只装本地）→ 第 ③ 步仍是 ASSET_UNAVAILABLE；
//   - 构建入口换成读不到工作区 AGENT.md 的形态 → 第 ② 步构建失败。
//
// The shipping command must be able to produce the very first bundle from a workspace
// that has never been installed; admission stays closed until that install publishes.
func TestAssets_FirstDeployFromEmptyWorkspace(t *testing.T) {
	ctx := context.Background()
	env := newAssetsAdmissionEnv(t)
	entry := agentEntry("alpha", nil, nil)
	definition := "---\nname: writer\n---\n\n# writer\n\n首次部署正文\n"
	require.NoError(t, os.WriteFile(filepath.Join(env.agentDir, "AGENT.md"), []byte(definition), 0o644))
	env.publishModel(t)

	// ① 尚无本地目录与共享指针：准入 fail-closed，给出的是"不可用"而不是别的失败。
	_, err := env.resolve(t, ctx, entry)
	failure := requireUnavailable(t, err, resourceReasonAsset)
	require.ErrorIs(t, failure.Cause, core.ErrAssetUnavailable)
	require.NoDirExists(t, env.assetRoot)

	// ② 出货命令：从工作区构建 → 安装到内容寻址目录 → 写共享发布记录。
	message, err := assets.CommandRoot(ctx, assets.Deps{
		AssetRoot: env.assetRoot,
		Build: func(ctx context.Context, agent, dest string) (assets.Manifest, error) {
			uid, resolveErr := resolveWorkspaceAgentUID(ctx, env.versions, agent)
			if resolveErr != nil {
				return assets.Manifest{}, resolveErr
			}
			return buildWorkspaceBundle(ctx,
				workspaceBundleDeps{Root: env.root, Versions: env.versions}, agent, uid, dest)
		},
		Publish: func(ctx context.Context, installed assets.Installed) (int64, error) {
			return newBundlePublisher(env.versions, env.ops, postgres.NewBundlePayloadStore(env.db), defaultPayloadMaxBytes, nil, time.Now).Publish(ctx, installed)
		},
	}, []string{"install", "--agent", "writer"})
	require.NoError(t, err, "首次部署必须有一条出货命令可用，而不是要求自写构建程序")
	require.Contains(t, message, "已安装并发布")

	// ③ 命令推进的指针就是准入的期望值；本地目录可再次逐文件校验。
	resolved, err := env.resolve(t, ctx, entry)
	require.NoError(t, err, "首次安装发布后准入必须放行")
	require.NotEmpty(t, resolved.AssetBundle.Hash, "准入固定的哈希来自首次安装推进的共享指针")
	require.Contains(t, filepath.ToSlash(resolved.AssetBundle.Dir), "/bundles/writer/")
	_, err = assets.Verify(ctx, resolved.AssetBundle.Dir)
	require.NoError(t, err)
}

// TestAdmission_RejectsStaleProviderInstance 覆盖 Provider 侧本地实例版本证明：证据一致
// 放行、不一致按**同一目标版本**重建-换入-再比、无接缝 / 无成效 / 重建报错一律 503。
//
// 逆改点：
//   - 重建后不重取视图（继续用重建前的 client）→ 第 2 步恒红；
//   - 重建传"当前发布版本"以外的版本 → `lastProvider` 断言红；
//   - 重建失败后降级放行 → 第 3 / 5 步红。
func TestAdmission_RejectsStaleProviderInstance(t *testing.T) {
	ctx := context.Background()
	env := newAssetsAdmissionEnv(t)
	entry := agentEntry("alpha", nil, nil)
	baseURL := "https://api.example.com"

	bundle := env.buildAndInstall(t, ctx, "# writer\n\n版本一\n")
	env.publishBundle(t, bundle)

	v1 := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", baseURL, "${BASALT_ALPHA_KEY}", "alpha-model"), "", "")
	env.publish(t, core.ResourceModelConnection, "alpha", v1.ID, true)

	// 构建证据 = 发布版本 + 本次准入会固定出的同一份 env pin。
	env.router.setBuilt("alpha", core.BuiltVersion{
		Resource: versionRef("alpha", v1), Credential: envPin(baseURL),
	})

	// ① 一致 → 放行，且不发生重建。
	_, err := env.resolve(t, ctx, entry)
	require.NoError(t, err)
	require.Equal(t, 0, env.rebuild.providerRebuildCount(), "证据一致不得触发重建")

	// ② 另一条路径把同名连接重发布为 V2（定义逐字相同、版本行不同）：本进程仍是 V1，
	// 按**V2** 重建-换入-再比后放行。
	v2 := env.seedVersion(t, core.ResourceModelConnection, "alpha",
		providerDefinition("alpha", baseURL, "${BASALT_ALPHA_KEY}", "alpha-model"), "",
		"providers/alpha/v2")
	require.NotEqual(t, v1.ID, v2.ID)
	env.publish(t, core.ResourceModelConnection, "alpha", v2.ID, true)
	env.rebuild.onProvider = func(target core.ResourceVersionRef, pin core.CredentialPin) error {
		env.router.setBuilt("alpha", core.BuiltVersion{Resource: target, Credential: pin})
		return nil
	}

	resolved, err := env.resolve(t, ctx, entry)
	require.NoError(t, err)
	require.Equal(t, 1, env.rebuild.providerRebuildCount())
	require.Equal(t, v2.ID, env.rebuild.lastProvider.VersionID, "重建必须按准入固化的目标版本")
	require.Equal(t, v2.ID, resolved.Provider.VersionID)

	// ③ 无成效的重建（换回的仍是旧证据）→ 503，不重试、不降级。
	env.router.setBuilt("alpha", core.BuiltVersion{Resource: versionRef("alpha", v1)})
	env.rebuild.onProvider = func(core.ResourceVersionRef, core.CredentialPin) error { return nil }
	_, err = env.resolve(t, ctx, entry)
	failure := requireUnavailable(t, err, resourceReasonProvider)
	require.ErrorIs(t, failure.Cause, core.ErrResourceVersionMismatch)

	// ④ 重建接缝缺失 → 同样 503（"没有接缝"不是"无需比对"）。
	missing := *env
	missing.deps.Rebuild = nil
	_, err = missing.resolve(t, ctx, entry)
	failure = requireUnavailable(t, err, resourceReasonProvider)
	require.ErrorIs(t, failure.Cause, core.ErrResourceVersionMismatch)

	// ⑤ 重建报错 → 503，错误链保留底层原因。
	env.rebuild.onProvider = nil
	env.rebuild.providerErr = errors.New("重建器不可用")
	_, err = env.resolve(t, ctx, entry)
	failure = requireUnavailable(t, err, resourceReasonProvider)
	require.ErrorIs(t, failure.Cause, core.ErrResourceVersionMismatch)
	require.Contains(t, failure.Cause.Error(), "重建器不可用")
}

// TestAdmission_PinsMCPAppliedVersion 覆盖 MCP 侧"以 applied 为基准、准入不隐式 apply"。
//
// 逆改点：
//   - `resolveMCP` 改用发布指针 → 第 1 步拿到 V2 而红；
//   - 准入顺手 apply（`MarkApplied`）→ `appliedVersion` 与审计断言红；
//   - `ensureMCPInstance` 在版本相同时仍重建 → `createdCount` 断言红。
func TestAdmission_PinsMCPAppliedVersion(t *testing.T) {
	ctx := context.Background()
	env := newAssetsAdmissionEnv(t)
	mcpEnv := newMCPEnv(t)
	env.mcp = newMCPRuntimeFromSupervisor(mcpEnv.supervisor, []core.MCPServerConfig{{Name: "web"}})
	entry := agentEntry("alpha", []string{"web"}, nil)
	env.prepareModel(t, ctx)

	mcpV1 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "allowed-mcp"), "", "")
	env.publish(t, core.ResourceMCPServer, "web", mcpV1.ID, true)
	require.NoError(t, mcpEnv.supervisor.Start(ctx, allowedStdio("web", mcpV1.ID)))

	// ① 发布 V2 但**不** apply：准入固定在 V1，不重建、不隐式 apply。
	mcpV2 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "allowed-mcp"), "",
		"mcp/web/v2")
	require.NotEqual(t, mcpV1.ID, mcpV2.ID)
	env.publish(t, core.ResourceMCPServer, "web", mcpV2.ID, false)

	resolved, err := env.resolve(t, ctx, entry)
	require.NoError(t, err)
	require.Len(t, resolved.MCPServers, 1)
	require.Equal(t, mcpV1.ID, resolved.MCPServers[0].VersionID, "准入必须固定 applied，不是发布指针")
	require.Equal(t, 0, env.rebuild.mcpRebuildCount(), "版本相同不得触发重建")
	require.Equal(t, 1, mcpEnv.createdCount(), "准入不得隐式 apply")
	applied, ok := mcpEnv.appliedVersion("web")
	require.True(t, ok, "夹具必须记录应用次数")
	require.Equal(t, mcpV1.ID, applied)

	var dbApplied int64
	require.NoError(t, env.db.QueryRowContext(ctx,
		`SELECT applied_version_id FROM resource_pointers
		  WHERE resource_kind = 'mcp_server' AND resource_id = 'web'`).Scan(&dbApplied))
	require.Equal(t, mcpV1.ID, dbApplied)

	// ② V2 被 apply：本地实例仍是 V1 → 按**已应用**版本重建-换入-再比后固定 V2。
	// 重建接缝接的是**真实** supervisor（与生产的 localRebuilder 同一条路径），
	// 否则"重建后本地视图是否对齐"判的是假替身自己写进去的值。
	// The seam drives the real supervisor, the same path localRebuilder takes.
	env.rebuild.onMCP = func(target core.ResourceVersionRef) error {
		return mcpEnv.supervisor.RebuildAtApplied(ctx, allowedStdio(target.Name, target.VersionID))
	}
	env.publish(t, core.ResourceMCPServer, "web", mcpV2.ID, true)
	resolved, err = env.resolve(t, ctx, entry)
	require.NoError(t, err)
	require.Equal(t, 1, env.rebuild.mcpRebuildCount())
	require.Equal(t, mcpV2.ID, env.rebuild.lastMCP.VersionID, "重建必须按已应用版本")
	require.Equal(t, mcpV2.ID, resolved.MCPServers[0].VersionID)
	status, ok := mcpEnv.supervisor.Status("web")
	require.True(t, ok)
	require.Equal(t, mcpV2.ID, status.AppliedVersionID, "重建只对齐本地运行视图")
	// `MarkApplied` 是 supervisor 写库的唯一出口：重建路径不得再调它（指针早就指着 V2，
	// 隐式 apply 会把"准入"变成"又一写者"）。
	// MarkApplied is the only write path; a rebuild must not invoke it.
	applied, ok = mcpEnv.appliedVersion("web")
	require.True(t, ok, "夹具必须记录应用次数")
	require.Equal(t, mcpV1.ID, applied, "重建路径不得隐式写 applied")
}

// TestAdmission_RejectsMCPRebuildFailure 判"重建失败 / 尚无已应用版本 → 503"。
//
// 逆改点：重建失败后沿用旧实例放行 → 第一条子用例红；发布指针冒充 applied →
// 第二条子用例红。
func TestAdmission_RejectsMCPRebuildFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("重建失败", func(t *testing.T) {
		env := newAssetsAdmissionEnv(t)
		mcpEnv := newMCPEnv(t)
		env.mcp = newMCPRuntimeFromSupervisor(mcpEnv.supervisor, []core.MCPServerConfig{{Name: "web"}})
		entry := agentEntry("alpha", []string{"web"}, nil)
		env.prepareModel(t, ctx)

		v1 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "allowed-mcp"), "", "")
		env.publish(t, core.ResourceMCPServer, "web", v1.ID, true)
		require.NoError(t, mcpEnv.supervisor.Start(ctx, allowedStdio("web", v1.ID)))

		v2 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "allowed-mcp"), "",
			"mcp/web/v2")
		env.publish(t, core.ResourceMCPServer, "web", v2.ID, true)
		// 失败注入必须落在**真实**重建路径上：接缝转发到 supervisor，连接器再报错，
		// 才会走到 `start` 的连接失败分支并原样上抛底层原因。
		// The injected failure must travel the real rebuild path so the supervisor's
		// connect-failure branch is the one that reports it.
		env.rebuild.onMCP = func(target core.ResourceVersionRef) error {
			return mcpEnv.supervisor.RebuildAtApplied(ctx, allowedStdio(target.Name, target.VersionID))
		}
		mcpEnv.setConnectErr(errors.New("连接失败"))

		_, err := env.resolve(t, ctx, entry)
		failure := requireUnavailable(t, err, resourceReasonMCP)
		require.ErrorIs(t, failure.Cause, core.ErrResourceVersionMismatch)
		require.Contains(t, failure.Cause.Error(), "重建")
		require.Contains(t, failure.Cause.Error(), "连接失败")
	})

	t.Run("尚无已应用版本", func(t *testing.T) {
		env := newAssetsAdmissionEnv(t)
		mcpEnv := newMCPEnv(t)
		env.mcp = newMCPRuntimeFromSupervisor(mcpEnv.supervisor, []core.MCPServerConfig{{Name: "web"}})
		entry := agentEntry("alpha", []string{"web"}, nil)
		env.prepareModel(t, ctx)

		v1 := env.seedVersion(t, core.ResourceMCPServer, "web", mcpDefinition("web", "allowed-mcp"), "", "")
		env.publish(t, core.ResourceMCPServer, "web", v1.ID, false)

		_, err := env.resolve(t, ctx, entry)
		failure := requireUnavailable(t, err, resourceReasonMCP)
		// 发布指针存在但从未 apply：这是"本地没有可证明的已应用实例"（503
		// RESOURCE_VERSION_MISMATCH），不是"资源不存在"（404 会让调用方去改名字）。
		// 评审 IDX-21：归到版本证明失败，web 才映射 503 而不是 500。
		require.ErrorIs(t, failure.Cause, core.ErrResourceVersionMismatch)
		require.NotErrorIs(t, failure.Cause, core.ErrResourceNotFound, "不得退回资源不存在")
		require.Equal(t, 0, env.rebuild.mcpRebuildCount(), "没有可固定版本时不得重建")
	})
}

// TestAdmission_RejectsStaleCredentialPin 覆盖托管凭据的版本固定：同一版本行上轮换凭据
// 只换 pin、不换版本——只比版本段的实现会在这里漏判。
//
// 逆改点：
//   - `providerInstanceMatches` 只比版本段 → 轮换后仍证"一致"，第 2 步的重建断言红；
//   - 重建不按本次准入固定出的 pin → `lastPin` 断言红；
//   - StartRun 侧不重读当前版本 → 轮换后仍放行而红。
func TestAdmission_RejectsStaleCredentialPin(t *testing.T) {
	ctx := context.Background()
	baseURL := "https://api.example.com"

	t.Run("Resolve 固定当前托管版本", func(t *testing.T) {
		env := newAssetsAdmissionEnv(t)
		entry := agentEntry("alpha", nil, nil)
		bundle := env.buildAndInstall(t, ctx, "# writer\n\n版本一\n")
		env.publishBundle(t, bundle)

		version := env.seedVersion(t, core.ResourceModelConnection, "alpha",
			managedProviderDefinition("alpha", baseURL, "alpha-model"), "", "")
		env.publish(t, core.ResourceModelConnection, "alpha", version.ID, true)

		key := testMasterKey()
		t.Cleanup(func() { key.Zero() })
		resolver := newTestResolver(t, env.runResourcesEnv,
			writeMasterKeyFile(t, t.TempDir(), key.KeyID, key.Key))
		env.deps.Credentials = resolver
		env.deps.CredentialPins = resolver

		credID := env.seedCredential(t, core.ResourceModelConnection, "alpha", baseURL)
		pin1 := env.pinOf(t, credID, 1)
		require.Equal(t, credID, pin1.ID)
		env.router.setBuilt("alpha", core.BuiltVersion{
			Resource: versionRef("alpha", version), Credential: pin1,
		})

		// ① 凭据未轮换 → 放行，零重建。
		resolved, err := env.resolve(t, ctx, entry)
		require.NoError(t, err)
		require.Equal(t, pin1, resolved.ProviderCredential)
		require.Equal(t, 0, env.rebuild.providerRebuildCount())

		// ② 轮换到第 2 版：版本行与内容哈希都不变，只有 pin 变 → 必须按新 pin 重建。
		env.appendCredentialVersion(t, credID, baseURL)
		pin2 := env.pinOf(t, credID, 2)
		require.Equal(t, 2, pin2.Version)
		require.NotEqual(t, pin1, pin2)
		env.rebuild.onProvider = func(target core.ResourceVersionRef, pin core.CredentialPin) error {
			env.router.setBuilt("alpha", core.BuiltVersion{Resource: target, Credential: pin})
			return nil
		}

		resolved, err = env.resolve(t, ctx, entry)
		require.NoError(t, err)
		require.Equal(t, 1, env.rebuild.providerRebuildCount(), "凭据换代必须触发重建")
		require.Equal(t, pin2, env.rebuild.lastPin, "重建必须按本次准入固定出的 pin")
		require.Equal(t, pin2, resolved.ProviderCredential)

		// ③ 重建报错 → 503（不得改钉旧版本或降级为 env）。
		env.rebuild.onProvider = nil
		env.rebuild.providerErr = errors.New("凭据材料不可用")
		env.router.setBuilt("alpha", core.BuiltVersion{
			Resource: versionRef("alpha", version), Credential: pin1,
		})
		_, err = env.resolve(t, ctx, entry)
		failure := requireUnavailable(t, err, resourceReasonProvider)
		require.ErrorIs(t, failure.Cause, core.ErrResourceVersionMismatch)
	})

	t.Run("StartRun 重读当前版本", func(t *testing.T) {
		env := newArchiveDBEnv(t, "writer", 0)
		credID := env.withManagedManifest(t, baseURL)

		// ① 清单固定的版本仍是当前版本 → 放行，引用行记下版本号。
		_, err := env.startRunWithExpectation(ctx, "run-cred-ok", "s-cred-ok", env.expectation())
		require.NoError(t, err)
		var recorded int
		require.NoError(t, env.db.QueryRowContext(ctx,
			`SELECT credential_version FROM run_credential_refs WHERE run_id = $1`,
			"run-cred-ok").Scan(&recorded))
		require.Equal(t, 1, recorded)

		// ② 准入窗口外轮换：当前版本推进到 2，清单仍钉 1 → 拒绝且零行。
		env.appendCredentialVersion(t, credID, baseURL)
		_, err = env.startRunWithExpectation(ctx, "run-cred-stale", "s-cred-stale", env.expectation())
		require.ErrorIs(t, err, core.ErrResourceVersionMismatch)
		require.Equal(t, 0, countRunRows(t, env, "run-cred-stale"))
	})
}

// TestAdmission_RejectsStaleLifecycle 判事务内生命周期复查：索引代次前移 → STALE；
// 期望值超前索引 → MISMATCH（需管理操作单调修复）；被拒一律零 Run 行。
//
// 逆改点：复查退化成"读快照后不比对代次" → 两条子断言各自红；拒绝路径先写 Run 行
// 再回滚失败 → 零行断言红。
func TestAdmission_RejectsStaleLifecycle(t *testing.T) {
	ctx := context.Background()
	env := newArchiveDBEnv(t, "writer", 0)

	// ① 对照：期望值与索引一致 → 成功。
	_, err := env.startRunWithExpectation(ctx, "run-lifecycle-ok", "s-lifecycle-ok", env.expectation())
	require.NoError(t, err)
	require.Equal(t, 1, countRunRows(t, env, "run-lifecycle-ok"))

	// ② 索引代次前移（另一台机器做了迁移）：本进程的期望值落后 → STALE。
	_, err = env.db.ExecContext(ctx,
		`UPDATE agent_lifecycle SET generation = generation + 1 WHERE agent_name = 'writer'`)
	require.NoError(t, err)
	_, err = env.startRunWithExpectation(ctx, "run-lifecycle-stale", "s-lifecycle-stale", env.expectation())
	require.ErrorIs(t, err, core.ErrAgentLifecycleStale)
	require.Equal(t, 0, countRunRows(t, env, "run-lifecycle-stale"))

	// ③ 期望值超前索引 → MISMATCH（只能由管理操作单调修复，不能自动放行）。
	ahead := core.LifecycleExpectation{
		Generation:    env.generation + 2,
		StateFileHash: env.stateHash,
	}
	_, err = env.startRunWithExpectation(ctx, "run-lifecycle-ahead", "s-lifecycle-ahead", ahead)
	require.ErrorIs(t, err, core.ErrAgentLifecycleMismatch)
	require.Equal(t, 0, countRunRows(t, env, "run-lifecycle-ahead"))
}

// TestStartRunRejectsLifecycleExpectationMismatch 判 P-16 的四条拒绝分支：缺期望值、
// 状态文件哈希不符、同代次状态不一致、索引行缺失。每条都必须零行、且会话标识互不
// 相同（否则会被 SESSION_BUSY 抢先拒绝而掩盖真正的分支）。
//
// 逆改点：零值交给"跳过复查" → 第 1 条红；只比代次不比哈希 → 第 2 条红；只比状态
// 不查索引存在性 → 第 4 条红。
func TestStartRunRejectsLifecycleExpectationMismatch(t *testing.T) {
	ctx := context.Background()

	t.Run("未携带期望值", func(t *testing.T) {
		env := newArchiveDBEnv(t, "writer", 0)
		_, err := env.startRunWithExpectation(ctx, "run-noexpect", "s-noexpect", core.LifecycleExpectation{})
		require.ErrorIs(t, err, core.ErrAgentLifecycleMismatch)
		require.Contains(t, err.Error(), "P-16")
		require.Equal(t, 0, countRunRows(t, env, "run-noexpect"))
	})

	t.Run("状态文件哈希不符", func(t *testing.T) {
		env := newArchiveDBEnv(t, "writer", 0)
		wrong := core.LifecycleExpectation{
			Generation:    env.generation,
			StateFileHash: sha256Hex(t, []byte("other-state-file")),
		}
		_, err := env.startRunWithExpectation(ctx, "run-badhash", "s-badhash", wrong)
		require.ErrorIs(t, err, core.ErrAgentLifecycleMismatch)
		require.Equal(t, 0, countRunRows(t, env, "run-badhash"))
	})

	t.Run("同一代次状态不一致", func(t *testing.T) {
		env := newArchiveDBEnv(t, "writer", 0)
		_, err := env.db.ExecContext(ctx,
			`UPDATE agent_lifecycle SET state = 'disabled' WHERE agent_name = 'writer'`)
		require.NoError(t, err)
		_, err = env.startRunWithExpectation(ctx, "run-badstate", "s-badstate", env.expectation())
		require.ErrorIs(t, err, core.ErrAgentLifecycleMismatch)
		require.Equal(t, 0, countRunRows(t, env, "run-badstate"))
	})

	t.Run("索引行缺失", func(t *testing.T) {
		env := newArchiveDBEnv(t, "writer", 0)
		_, err := env.db.ExecContext(ctx,
			`DELETE FROM agent_lifecycle WHERE agent_name = 'writer'`)
		require.NoError(t, err)
		_, err = env.startRunWithExpectation(ctx, "run-noindex", "s-noindex", env.expectation())
		require.ErrorIs(t, err, core.ErrAgentLifecycleMismatch)
		require.Equal(t, 0, countRunRows(t, env, "run-noindex"))
	})
}

// TestAdmission_RejectsPublishRace 判"准入窗口内发布被推进"的两条路径，以及成功路径的
// 清单字段（`asset_bundle_hash` 必须落库、格式版本为 3）。
//
// 逆改点：复查只读进程内缓存 → 指针推进 / 哈希漂移两条各自红；清单不写 bundle 哈希
// → 第 3 条红。
func TestAdmission_RejectsPublishRace(t *testing.T) {
	ctx := context.Background()

	t.Run("Provider 指针在准入窗口内被推进", func(t *testing.T) {
		env := newArchiveDBEnv(t, "writer", 0)
		ref := env.manifest.Provider.ResourceVersionRef
		next, _, err := env.versions.EnsureVersion(ctx, core.ResourceVersion{
			Kind:           core.ResourceModelConnection,
			ResourceID:     ref.Name,
			ResourceUID:    ref.ResourceUID,
			ContentHash:    sha256Hex(t, []byte("alpha-republished")),
			DefinitionYAML: "name: alpha\nbase_url: https://api.example.com\ncredential_mode: managed\ndefault_model: alpha-model\n",
			CreatedAt:      baselineClock().UnixMilli(),
			OperationID:    env.seedOperation(t),
		})
		require.NoError(t, err)
		require.NotEqual(t, ref.VersionID, next.ID, "同一资源的新版本必须换版本行")
		require.NoError(t, env.versions.Publish(ctx, core.ResourceModelConnection, ref.Name, next.ID,
			true, env.seedOperation(t), baselineClock()))

		_, err = env.startRunWithExpectation(ctx, "run-pubrace", "s-pubrace", env.expectation())
		require.ErrorIs(t, err, core.ErrResourceVersionMismatch)
		// 409 的判定依据是"哨兵**未**被 `*ResourceUnavailableError` 包装"（评审 IDX-15）：
		// 准入事务内复查发现指针在窗口内被推进，web 映射 409「重试这一笔」，不是 503
		// 「本地实例证明不成立」。这条形态断言与 web 包的双状态契约用例成对。
		var unavailable *core.ResourceUnavailableError
		require.False(t, errors.As(err, &unavailable), "准入窗口内漂移必须保持裸哨兵形态（409）")
		require.Equal(t, 0, countRunRows(t, env, "run-pubrace"))
	})

	t.Run("bundle 内容哈希在准入窗口内漂移", func(t *testing.T) {
		env := newArchiveDBEnv(t, "writer", 0)
		_, err := env.db.ExecContext(ctx, `
UPDATE resource_versions SET content_hash = $1
 WHERE resource_kind = 'agent_bundle' AND resource_uid = $2`,
			sha256Hex(t, []byte("drifted-bundle")), string(env.manifest.AgentUID))
		require.NoError(t, err)

		_, err = env.startRunWithExpectation(ctx, "run-bundlerace", "s-bundlerace", env.expectation())
		require.ErrorIs(t, err, core.ErrAssetVersionMismatch)
		require.Equal(t, 0, countRunRows(t, env, "run-bundlerace"))
	})

	t.Run("清单写出 asset_bundle_hash 与 v3 格式", func(t *testing.T) {
		env := newArchiveDBEnv(t, "writer", 0)
		_, err := env.startRunWithExpectation(ctx, "run-manifest", "s-manifest", env.expectation())
		require.NoError(t, err)

		var (
			formatVersion int
			snapshot      string
			storedHash    string
		)
		require.NoError(t, env.db.QueryRowContext(ctx,
			`SELECT format_version, snapshot_json, manifest_hash
			   FROM run_resource_manifests WHERE run_id = $1`, "run-manifest").
			Scan(&formatVersion, &snapshot, &storedHash))
		require.Equal(t, 3, formatVersion, "资产包哈希属于清单 v3")
		require.Equal(t, env.manifest.Hash, storedHash)

		var got core.RunResourceManifest
		require.NoError(t, json.Unmarshal([]byte(snapshot), &got))
		require.Equal(t, env.manifest.AssetBundleHash, got.AssetBundleHash)
		require.NotEmpty(t, got.AssetBundleHash, "清单必须记下准入固定的 bundle 哈希")
	})
}

// TestBaseline_LocalGenerationAheadDoesNotWriteIndex 判启动基线 P-17：本地状态文件代次
// 高于共享索引时**不得**写回索引，该 Agent 留在"不可自动修复"状态等管理操作。
//
// 逆改点：去掉代次比较（无条件 PutLifecycleIndex）→ 索引被回退、两条断言红；
// 只 WARN 不回退 → 索引代次断言红。
func TestBaseline_LocalGenerationAheadDoesNotWriteIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	env := newBaselineEnv(t)

	require.NoError(t, runBaseline(ctx, env.deps))
	state, generation := readLifecycleIndex(t, env.deps, "writer")
	require.Equal(t, core.AgentDisabled, state)
	require.Equal(t, int64(1), generation)
	require.Len(t, baselineOperations(t, env.deps), 1)

	// 管理操作把本地状态文件推到 active/2，但**没有**同步共享索引（模拟另一条路径
	// 只写了文件）：索引仍停在 disabled/1。
	updated, err := env.deps.Lifecycle.Transition(ctx, "writer", core.AgentDisabled, 1,
		core.AgentActive, core.NewOperationID(baselineClock()))
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Generation)

	require.NoError(t, runBaseline(ctx, env.deps))

	state, generation = readLifecycleIndex(t, env.deps, "writer")
	require.Equal(t, core.AgentDisabled, state, "本地代次高于索引时不得写回索引")
	require.Equal(t, int64(1), generation)
	require.Len(t, baselineOperations(t, env.deps), 1, "第二次启动不得写第二条基线操作")
	require.Contains(t, env.logs.String(), "本地生命周期代次高于共享索引")

	local, exists, err := env.deps.Lifecycle.Read(ctx, "writer")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, core.AgentActive, local.State)
	require.Equal(t, int64(2), local.Generation)
}

// readLifecycleIndex 读共享索引行的 `(state, generation)`；缺失即夹具故障。
// readLifecycleIndex reads the shared lifecycle row.
func readLifecycleIndex(t *testing.T, deps BaselineDeps, agent string) (core.AgentState, int64) {
	t.Helper()
	record, exists, _, err := deps.Resources.ReadLifecycleIndex(context.Background(), agent)
	require.NoError(t, err)
	require.True(t, exists, "基线必须写入共享生命周期索引")
	return record.State, record.Generation
}

// —— archiveDBEnv 的准入期望值扩展（不改既有用例文件）——

// expectation 返回夹具当前状态文件对应的准入期望值。
// expectation returns the admission expectation matching the fixture's state file.
func (e *archiveDBEnv) expectation() core.LifecycleExpectation {
	return core.LifecycleExpectation{Generation: e.generation, StateFileHash: e.stateHash}
}

// startRunWithExpectation 与 `startRun` 同形，但允许用例给出**任意**期望值（P-16 的
// 拒绝分支要能构造零值 / 超前 / 错哈希）；runID 同时作为幂等键。
// startRunWithExpectation mirrors startRun with a caller-supplied expectation.
func (e *archiveDBEnv) startRunWithExpectation(ctx context.Context, runID, sessionID string,
	expectation core.LifecycleExpectation,
) (core.LeaseToken, error) {
	return e.runs.StartRun(ctx, core.RunRecord{
		RunID:     runID,
		SessionID: sessionID,
		Agent:     e.name,
		Channel:   "web",
		UserID:    "u1",
		Principal: core.LocalAdminCLIPrincipal(),
		StartedAt: time.Now().UTC(),
		Lifecycle: expectation,
	}, e.manifest,
		core.WorkerIdentity{InstanceID: "worker-" + runID, Name: "worker-" + runID},
		core.CallerAdmission{}, runID)
}

// withManagedManifest 把夹具清单里的凭据段换成**托管** pin，并建出对应的凭据行；
// 返回凭据 ID。清单经 `BuildRunResourceManifest` 重建（哈希是派生值，不得手改）。
// withManagedManifest rewrites the fixture manifest's credential segment as managed.
func (e *archiveDBEnv) withManagedManifest(t *testing.T, baseURL string) string {
	t.Helper()
	ctx := context.Background()
	uid := e.manifest.Provider.ResourceUID
	store := postgres.NewCredentialStore(e.db)
	credID := seedManagedCredential(t, store, e.seedOperation(t), core.ResourceModelConnection,
		e.manifest.Provider.Name, uid, baseURL)
	record, err := store.Version(ctx, credID, 1)
	require.NoError(t, err)

	resolved := core.ResolvedResources{
		Provider:      e.manifest.Provider.ResourceVersionRef,
		ProviderModel: e.manifest.Provider.Model,
		ProviderCredential: core.CredentialPin{
			Mode:          core.CredentialModeManaged,
			ID:            credID,
			Version:       record.VersionNo,
			TargetBinding: record.TargetBinding,
		},
		Notify:      e.manifest.Notify,
		AssetBundle: core.AssetBundlePin{Hash: e.manifest.AssetBundleHash},
	}
	manifest, err := core.BuildRunResourceManifest(e.manifest.AgentUID, core.AgentActive, resolved)
	require.NoError(t, err)
	e.manifest = manifest
	return credID
}

// appendCredentialVersion 为 archiveDBEnv 的托管凭据追加一版（轮换）。
// appendCredentialVersion rotates the managed credential of the archive fixture.
func (e *archiveDBEnv) appendCredentialVersion(t *testing.T, credID, baseURL string) {
	t.Helper()
	uid := e.manifest.Provider.ResourceUID
	store := postgres.NewCredentialStore(e.db)
	operationID := e.seedOperation(t)
	require.NoError(t, store.AppendVersion(context.Background(), credID, "master/v1", operationID,
		core.CredentialVersionRecord{
			KeyID:         "master/v1",
			TargetBinding: core.TargetBinding{Kind: core.BindingModelConnection, Owner: uid, BaseURL: baseURL}.Hash(),
			Last4:         "0002",
			Nonce:         make([]byte, 12),
			Ciphertext:    []byte("sealed:alpha:v2"),
			OperationID:   operationID,
		}, baselineClock()))
}

// envPin 是 env 模式的凭据固定（目标绑定哈希由规范化 base URL 派生，不含属主）。
// envPin is the env-mode credential pin derived from the normalized base url.
func envPin(baseURL string) core.CredentialPin {
	return core.CredentialPin{
		Mode: core.CredentialModeEnv,
		TargetBinding: core.TargetBinding{
			Kind:    core.BindingModelConnection,
			BaseURL: baseURL,
		}.Hash(),
	}
}
