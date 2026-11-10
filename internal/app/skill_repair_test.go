package app

// 本文件是 US-25C FR-25C-17 的技能联动与运行期修复判据（模块卡 §2.3.7）。判据分三层，
// 每层都落在**可观察的结果**上，而不是"代码里写了什么"：
//
//  1. 依赖发现只看共享发布记录：`TestSkillPublish_RepublishesDependents` 断言只有真正
//     引用该技能的 Agent 进结果，已引用 live 版本的走 `skipped`，不引用技能的 Agent
//     根本不出现——结果按 Agent 名排序，`skipped` 不产生任何发布写；
//  2. 重建来源是**已安装目录 + 已发布技能版本目录**，不是工作副本：
//     `TestSkillPublish_DoesNotPublishWorkCopy` 让工作副本与工作区 AGENT.md 同时分叉，
//     断言重建出的 bundle 里仍是已发布字节；`rebuild_bundle_skill_test.go` 直接钉
//     `rebuildBundleWithSkill` 的输入输出；
//  3. 乱序发布与运行期自愈：`TestSkillRepublish_OutOfOrderPublishKeepsNewest` 让发布前的
//     复查看到更新的技能版本，断言 `superseded` 且零发布；`TestSkillRepair_*` 断言失败后
//     下一轮（推进时钟）自动重试、连续 8 次转 `blocked`、全量扫描复位，且 Worker 的修复
//     入口为 nil。
//
// 为什么用消费方接口夹具而不是 `*postgres.ResourceStore`：与 `bundlePublisher` 的
// `bundleRecordStore` 是同一条缝（assets_publish.go:78 的注释已说明理由）。没有它，
// "晚到的旧版本不得覆盖新版本"只能靠真库并发复现，判据落不到单元层。PG 侧的真实发布
// 链路由 `skill_republish_integration_test.go`（T040）承担。
// The fakes are the same consumer-side seam the bundle publisher already uses; the real
// PostgreSQL publication path is judged by the integration layer, not here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/assets"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// repairSkillName 是夹具技能名（单个目录名，与 storage_path 契约形态一致）。
	repairSkillName = "web"
	// repairAgentAlpha / Beta / Gamma 是三个 Agent：alpha 引用旧版本、beta 引用当前版本、
	// gamma 不引用技能。三种形态正好覆盖"重发 / 跳过 / 不相关"三条分支。
	repairAgentAlpha = "alpha"
	repairAgentBeta  = "beta"
	repairAgentGamma = "gamma"

	// repairSkillV1Body / V2 / V3 是三次技能发布的正文；1 是 alpha 的旧引用，2 是夹具的
	// live 版本，3 只用于乱序发布用例（晚到的 V2 不得覆盖 V3）。
	repairSkillV1Body = "web v1 正文\n"
	repairSkillV2Body = "web v2 正文\n"
	repairSkillV3Body = "web v3 正文\n"
	// repairWorkCopyBody 是工作副本正文：它**从未发布**，任何重建都不得采用。
	repairWorkCopyBody = "工作副本（未发布）\n"
	// repairAgentDivergedBody 是工作区 AGENT.md 的分叉正文：已批准版本是别的字节，
	// 重建必须沿用旧 bundle 里的配置，而不是这份。
	repairAgentDivergedBody = "alpha 正文（工作副本未发布）\n"

	// repairStatus* 是 `core.BundlePublishResult.Status` 的五个固定字面量（§3.3）。
	repairStatusOK         = "ok"
	repairStatusSkipped    = "skipped"
	repairStatusSuperseded = "superseded"
	repairStatusMissing    = "missing"
	repairStatusFailed     = "failed"

	// repairMaxBundleBytes 是夹具载荷上限：够装下这几个小文件。
	repairMaxBundleBytes = int64(1 << 20)
	// repairRepairBatch / repairRepairInterval 是修复器夹具的轮次参数：每轮最多 5 个、
	// 退避基数 30s（上限 5m 由实现固定，夹具不覆盖）。
	repairRepairBatch    = 5
	repairRepairInterval = 30 * time.Second
	// repairBlockedRounds 是"连续失败转 blocked"的轮数（模块卡 §2.3.7 固定为 8）。
	repairBlockedRounds = 8
)

// repairAgentBody 返回某 Agent 的**已发布**定义正文：夹具不解析 AGENT.md（bundle 构建
// 只做字节快照），因此正文可以是任意可辨识的字节串。
func repairAgentBody(agent string) string { return agent + " 正文（已发布版）\n" }

// repairActor 是联动重发的操作主体：非零，避免 `publishAs` 回落到 CLI 身份。
func repairActor() core.Actor {
	return core.Actor{
		Subject: core.ActorSubjectCLIPrefix + "repair-test",
		Method:  core.AuthMethodCLI,
		Source:  "test",
	}
}

// repairLogger 静默修复器日志：用例判状态，不判日志。
func repairLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// repairSHA256 计算夹具文件的内容哈希。
func repairSHA256(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// repairClock 是可控时钟：修复器的退避与"发布前复查"都按它推进，用例因此不需要 sleep。
type repairClock struct {
	mu  sync.Mutex
	now time.Time
}

// newRepairClock 从固定起点开始（2026-11-09T12:00:00Z）。
func newRepairClock() *repairClock {
	return &repairClock{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}

// Now 返回当前夹具时刻。
func (c *repairClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 把夹具时刻向前推 d。
func (c *repairClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// skillRepairPublish 是一次 `PublishAgentBundle` 调用的读数：`expected` 就是发布器
// 交给 CAS 的期望指针——它必须来自**锁内重读**，而不是调用方入参。
type skillRepairPublish struct {
	uid      core.AgentUID
	version  int64
	expected int64
}

// skillRepairStore 是共享资源记录的多 Agent 内存替身：同时满足 `bundleRecordStore`
// （发布链）、`skillRepublishStore`（联动重发）与 `skillRepairVersions`（运行期修复）
// 三条消费面，六个发布链方法逐项显式实现。
// skillRepairStore satisfies all three consumer interfaces at once, with every
// bundleRecordStore method implemented explicitly.
type skillRepairStore struct {
	mu         sync.Mutex
	agents     map[string]core.AgentUID
	identities map[core.AgentUID]string
	pointers   map[string]core.ResourcePointer // `kind\x00resourceID`
	byUID      map[string]core.ResourcePointer // `kind\x00uid`
	versions   map[int64]core.ResourceVersion
	// liveScript 是 `PointerVersion` 的脚本序列：按调用次序逐项消耗，模拟"两次读之间
	// 有人发布了更新版本"的竞态。耗尽后回落到 liveSteady。
	liveScript map[string][]int64
	liveSteady map[string]int64
	nextID     int64
	publishes  []skillRepairPublish
}

// newSkillRepairStore 建一份空记录：还没有任何 Agent、指针或版本行。
func newSkillRepairStore() *skillRepairStore {
	return &skillRepairStore{
		agents:     make(map[string]core.AgentUID),
		identities: make(map[core.AgentUID]string),
		pointers:   make(map[string]core.ResourcePointer),
		byUID:      make(map[string]core.ResourcePointer),
		versions:   make(map[int64]core.ResourceVersion),
		liveScript: make(map[string][]int64),
		liveSteady: make(map[string]int64),
		nextID:     201,
	}
}

// recordKey 拼出 `kind\x00id` 形态的映射键（id 可以是 resource_id 或 uid）。
func recordKey(kind core.ResourceKind, id string) string {
	return string(kind) + "\x00" + id
}

// registerAgent 登记一个 Agent 的身份。
func (s *skillRepairStore) registerAgent(agent string, uid core.AgentUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[agent] = uid
	s.identities[uid] = agent
}

// putVersion 写入一条版本行（夹具直接给固定 ID，断言因此可复算）。
func (s *skillRepairStore) putVersion(version core.ResourceVersion) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versions[version.ID] = version
}

// pointTo 把某个资源指针指向 `versionID`，同时登记两种读取键。
func (s *skillRepairStore) pointTo(kind core.ResourceKind, resourceID string,
	uid core.ResourceUID, versionID int64,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pointer := core.ResourcePointer{
		Kind:               kind,
		ResourceID:         resourceID,
		ResourceUID:        uid,
		PublishedVersionID: versionID,
	}
	s.pointers[recordKey(kind, resourceID)] = pointer
	s.byUID[recordKey(kind, string(uid))] = pointer
}

// setLive 固定某资源的当前已发布版本号（`PointerVersion` 的稳定值）。
func (s *skillRepairStore) setLive(kind core.ResourceKind, resourceID string, versionID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveSteady[recordKey(kind, resourceID)] = versionID
}

// scriptPointerVersion 给某资源的 `PointerVersion` 装一段脚本序列。
func (s *skillRepairStore) scriptPointerVersion(kind core.ResourceKind, resourceID string, ids ...int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveScript[recordKey(kind, resourceID)] = append([]int64(nil), ids...)
}

// publishCalls 返回发布指针 CAS 的读数快照。
func (s *skillRepairStore) publishCalls() []skillRepairPublish {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]skillRepairPublish(nil), s.publishes...)
}

// AgentIdentity 实现 `bundleRecordStore`。
func (s *skillRepairStore) AgentIdentity(_ context.Context, agent string) (core.AgentUID, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ok := s.agents[agent]
	return uid, "", ok, nil
}

// Pointer 按 resource_id 读指针。
func (s *skillRepairStore) Pointer(_ context.Context, kind core.ResourceKind, resourceID string) (core.ResourcePointer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pointer, ok := s.pointers[recordKey(kind, resourceID)]
	if !ok {
		return core.ResourcePointer{}, core.ErrResourceNotFound
	}
	return pointer, nil
}

// PointerByUID 按资源身份读指针。
func (s *skillRepairStore) PointerByUID(_ context.Context, kind core.ResourceKind, uid core.ResourceUID) (core.ResourcePointer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pointer, ok := s.byUID[recordKey(kind, string(uid))]
	if !ok {
		return core.ResourcePointer{}, core.ErrResourceNotFound
	}
	return pointer, nil
}

// ListPointers 返回某类资源的全部指针（含软删除行：调用方自己按 DeletedAt 过滤）。
func (s *skillRepairStore) ListPointers(_ context.Context, kind core.ResourceKind) ([]core.ResourcePointer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.ResourcePointer, 0, len(s.byUID))
	for _, pointer := range s.byUID {
		if pointer.Kind == kind {
			out = append(out, pointer)
		}
	}
	return out, nil
}

// VersionByID 读版本行。
func (s *skillRepairStore) VersionByID(_ context.Context, id int64) (core.ResourceVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version, ok := s.versions[id]
	if !ok {
		return core.ResourceVersion{}, core.ErrResourceNotFound
	}
	return version, nil
}

// EnsureVersion 追加一条版本行并分配夹具 ID（同内容去重不在本用例的判据面内）。
func (s *skillRepairStore) EnsureVersion(_ context.Context, version core.ResourceVersion) (core.ResourceVersion, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version.ID = s.nextID
	s.nextID++
	s.versions[version.ID] = version
	return version, false, nil
}

// PublishAgentBundle 是发布指针 CAS 的替身：**期望值必须等于当前指针**，否则返回
// false（生产语义是"期望不成立、不覆盖他人发布"）。记录每次调用供断言。
func (s *skillRepairStore) PublishAgentBundle(_ context.Context, uid core.AgentUID,
	versionID, expected int64, _ string, _ time.Time,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := recordKey(core.ResourceAgentBundle, string(uid))
	current, ok := s.byUID[key]
	if expected != 0 || ok {
		if !ok || current.PublishedVersionID != expected {
			return false, nil
		}
	}
	s.publishes = append(s.publishes, skillRepairPublish{uid: uid, version: versionID, expected: expected})
	current.PublishedVersionID = versionID
	current.Kind = core.ResourceAgentBundle
	current.ResourceUID = core.ResourceUID(uid)
	current.ResourceID = string(uid)
	s.byUID[key] = current
	s.pointers[recordKey(core.ResourceAgentBundle, string(uid))] = current
	return true, nil
}

// PointerVersion 是"当前已发布版本号"的单快照读：先消耗脚本序列，再回落稳定值。
func (s *skillRepairStore) PointerVersion(_ context.Context, kind core.ResourceKind, resourceID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pointerVersionLocked(kind, resourceID)
}

// pointerVersionLocked 是 `PointerVersion` 的加锁版本（内部合成重发要用）。
func (s *skillRepairStore) pointerVersionLocked(kind core.ResourceKind, resourceID string) (int64, error) {
	key := recordKey(kind, resourceID)
	if sequence := s.liveScript[key]; len(sequence) > 0 {
		next := sequence[0]
		s.liveScript[key] = sequence[1:]
		return next, nil
	}
	if id, ok := s.liveSteady[key]; ok {
		return id, nil
	}
	return 0, core.ErrResourceNotFound
}

// ReadLifecycleIndex 让替身满足 `assetBootstrapRecords`；本文件不走生命周期判定。
func (s *skillRepairStore) ReadLifecycleIndex(context.Context, string) (core.LifecycleRecord, bool, string, error) {
	return core.LifecycleRecord{}, false, "", nil
}

// publishedBundles 是 `AssetDistributionPort.ListPublishedBundlesPage` 的实现：按
// `resource_id`（Agent 身份）严格前进的分页，与生产键集语义同形。
func (s *skillRepairStore) publishedBundles(afterResourceID string, limit int) []core.PublishedBundle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]core.PublishedBundle, 0, len(s.byUID))
	for key, pointer := range s.byUID {
		if pointer.Kind != core.ResourceAgentBundle || key == "" {
			continue
		}
		if string(pointer.ResourceUID) <= afterResourceID {
			continue
		}
		version, ok := s.versions[pointer.PublishedVersionID]
		if !ok {
			continue
		}
		out = append(out, core.PublishedBundle{
			Agent:      s.identities[core.AgentUID(pointer.ResourceUID)],
			AgentUID:   core.AgentUID(pointer.ResourceUID),
			BundleHash: version.ContentHash,
			UpdatedAt:  version.CreatedAt,
		})
	}
	sortPublishedBundles(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// sortPublishedBundles 按 Agent 身份升序（键集分页的前进方向）。
func sortPublishedBundles(rows []core.PublishedBundle) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].AgentUID < rows[j-1].AgentUID; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// republishSynthetic 模拟一次**成功**的 `RepublishOne`：把该 Agent 的发布指针移到一个
// 引用当前 live 技能版本的 bundle 上。修复器只从记录里判"是否需要修"，因此不需要真的
// 重建内容——但指针必须真的移动，否则下一轮判据会永远显示"还落后"。
// republishSynthetic moves the pointer to a bundle that references the live skill
// version, which is exactly what a successful RepublishOne leaves behind.
func (s *skillRepairStore) republishSynthetic(agent, skillName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ok := s.agents[agent]
	if !ok {
		return
	}
	key := recordKey(core.ResourceAgentBundle, string(uid))
	pointer, ok := s.byUID[key]
	if !ok {
		return
	}
	version, ok := s.versions[pointer.PublishedVersionID]
	if !ok {
		return
	}
	var manifest assets.Manifest
	if err := json.Unmarshal([]byte(version.ManifestJSON), &manifest); err != nil {
		return
	}
	liveID, err := s.pointerVersionLocked(core.ResourceSkill, skillName)
	if err != nil {
		return
	}
	live, ok := s.versions[liveID]
	if !ok {
		return
	}
	var files []core.FileHash
	if err := json.Unmarshal([]byte(live.ManifestJSON), &files); err != nil {
		return
	}
	manifest.Skills = []core.SkillVersionRef{{
		ResourceVersionRef: core.ResourceVersionRef{
			Name:        skillName,
			ResourceUID: live.ResourceUID,
			VersionID:   live.ID,
			VersionNo:   live.VersionNo,
			ContentHash: live.ContentHash,
		},
		Files: files,
	}}
	manifest.BundleHash = "synthetic-" + agent + "-" + strconv.FormatInt(s.nextID, 10)
	raw, err := json.Marshal(manifest)
	if err != nil {
		return
	}
	id := s.nextID
	s.nextID++
	s.versions[id] = core.ResourceVersion{
		ID:           id,
		Kind:         core.ResourceAgentBundle,
		ResourceUID:  core.ResourceUID(uid),
		ResourceID:   string(uid),
		VersionNo:    version.VersionNo + 1,
		ContentHash:  manifest.BundleHash,
		ManifestJSON: string(raw),
		StoragePath:  "/synthetic/" + manifest.BundleHash,
	}
	pointer.PublishedVersionID = id
	s.byUID[key] = pointer
	s.pointers[recordKey(core.ResourceAgentBundle, string(uid))] = pointer
}

// skillRepairDistribution 把 `AssetDistributionPort` 的读面收窄到本文件唯一用到的
// 分页方法：其余三个方法经内嵌 nil 接口 panic，防止把"未观察"当成"已实现"。
type skillRepairDistribution struct {
	core.AssetDistributionPort
	store *skillRepairStore
}

// ListPublishedBundlesPage 按复合键集分页返回已发布 bundle。
func (d *skillRepairDistribution) ListPublishedBundlesPage(_ context.Context, afterResourceID string, limit int) ([]core.PublishedBundle, error) {
	return d.store.publishedBundles(afterResourceID, limit), nil
}

// skillRepairCall 是一次 `RepublishOne` 调用的读数。
type skillRepairCall struct {
	agent string
	skill string
}

// skillRepairAdmin 是 `SkillRepublisher` 的替身：按脚本返回状态，成功时同步把指针
// 移到引用 live 版本的 bundle 上（修复器的"是否需要修"判据跟着变真实）。
type skillRepairAdmin struct {
	store *skillRepairStore

	mu     sync.Mutex
	script []string
	calls  []skillRepairCall
}

// newSkillRepairAdmin 按可变参数装脚本；脚本按调用次序消耗，耗尽后沿用最后一项。
func newSkillRepairAdmin(store *skillRepairStore, script ...string) *skillRepairAdmin {
	return &skillRepairAdmin{store: store, script: append([]string(nil), script...)}
}

// setScript 替换脚本（用例中途翻转结果时用）。
func (a *skillRepairAdmin) setScript(script ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.script = append([]string(nil), script...)
}

// callCount 返回被调次数。
func (a *skillRepairAdmin) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// RepublishOne 实现 `SkillRepublisher`。
func (a *skillRepairAdmin) RepublishOne(_ context.Context, agent, skillName string, _ core.Actor) core.BundlePublishResult {
	a.mu.Lock()
	status := repairStatusOK
	if len(a.script) > 0 {
		status = a.script[0]
		if len(a.script) > 1 {
			a.script = a.script[1:]
		}
	}
	a.calls = append(a.calls, skillRepairCall{agent: agent, skill: skillName})
	a.mu.Unlock()

	if status == repairStatusOK {
		a.store.republishSynthetic(agent, skillName)
	}
	return core.BundlePublishResult{Agent: agent, Status: status}
}

// skillRepairFixture 是一套完整的联动夹具：工作区（定义 + 技能发布目录 + 工作副本）、
// 资产根、共享记录、操作日志、载荷表与一把**测试专用**的 Agent 锁。
//
// `AgentGate` 用新实例：模块卡 §2.3.7 明确允许测试自建，但生产必须与内容编辑共用
// `wire.go:702` 的同一把——夹具注释即该条的依据记录。
// The fixture owns a fresh AgentGate, which the card explicitly permits for tests;
// production must share the single instance used by content editing.
type skillRepairFixture struct {
	t         *testing.T
	root      string
	assetRoot string
	skill     string
	skillUID  core.ResourceUID
	agents    map[string]core.AgentUID

	clock      *repairClock
	store      *skillRepairStore
	operations *fakeOperationLog
	payloads   *fakeBundlePayloads
	gate       *core.AgentGate

	distribution *skillRepairDistribution
	bundleIDs    map[string]int64 // agent → 当前已发布 bundle 版本行 ID
	skillIDs     map[int]int64    // 技能版本号 → 版本行 ID
}

// newSkillRepairFixture 建夹具并预置技能的三次发布（v1 / v2 / v3）。
func newSkillRepairFixture(t *testing.T) *skillRepairFixture {
	t.Helper()
	f := &skillRepairFixture{
		t:          t,
		root:       t.TempDir(),
		assetRoot:  t.TempDir(),
		skill:      repairSkillName,
		skillUID:   fixtureResourceUID(t, core.ResourceSkill, repairSkillName),
		agents:     make(map[string]core.AgentUID),
		clock:      newRepairClock(),
		store:      newSkillRepairStore(),
		operations: &fakeOperationLog{},
		payloads:   newFakeBundlePayloads(nil),
		gate:       core.NewAgentGate(),
		bundleIDs:  make(map[string]int64),
		skillIDs:   make(map[int]int64),
	}
	f.distribution = &skillRepairDistribution{store: f.store}

	for _, agent := range []string{repairAgentAlpha, repairAgentBeta, repairAgentGamma} {
		uid, err := core.NewAgentUID()
		require.NoError(t, err, "夹具需要合法的 Agent 身份")
		f.agents[agent] = uid
		f.store.registerAgent(agent, uid)
		writeWorkspaceFile(t, f.root, filepath.ToSlash(filepath.Join("agents", agent, "AGENT.md")),
			repairAgentBody(agent))
	}
	for number, body := range map[int]string{1: repairSkillV1Body, 2: repairSkillV2Body, 3: repairSkillV3Body} {
		f.seedSkillVersion(number, body)
	}
	// live 固定在 v2：alpha 引用 v1 因此落后，beta 引用 v2 因此已是最新。
	f.store.setLive(core.ResourceSkill, repairSkillName, f.skillVersionID(2))
	// 工作副本故意与任何已发布版本都不同。
	writeWorkspaceFile(t, f.root, "skills/"+repairSkillName+"/SKILL.md", repairWorkCopyBody)
	return f
}

// seedSkillVersion 把技能正文冻结到 `versions/skills/<name>/<uid>/<no>` 并登记版本行。
func (f *skillRepairFixture) seedSkillVersion(number int, body string) {
	f.t.Helper()
	// 落盘用生产投影（`storage_path` 去掉 `.basalt/` 前缀）——工作区根本身就是 `.basalt`，
	// 直接拿契约字面量落盘会写出双前缀 `/.basalt/.basalt/...`，读侧永远对不上。
	// storage_path keeps the contract literal; the on-disk dir is its workspace projection.
	storagePath := fmt.Sprintf(".basalt/versions/skills/%s/%s/%d", f.skill, f.skillUID, number)
	relDir, err := workspace.PublishedVersionRelDir(storagePath)
	require.NoError(f.t, err, "夹具里的 storage_path 必须是合法契约形态")
	writeWorkspaceFile(f.t, f.root, relDir+"/SKILL.md", body)
	files := []core.FileHash{{Path: "SKILL.md", SHA256: repairSHA256(body), Size: int64(len(body))}}
	raw, err := json.Marshal(files)
	require.NoError(f.t, err)
	id := int64(100 + number)
	f.store.putVersion(core.ResourceVersion{
		ID:           id,
		Kind:         core.ResourceSkill,
		ResourceUID:  f.skillUID,
		ResourceID:   f.skill,
		VersionNo:    number,
		ContentHash:  repairSHA256(body),
		ManifestJSON: string(raw),
		StoragePath:  storagePath,
	})
	f.skillIDs[number] = id
}

// skillVersionID 返回某次技能发布的版本行 ID。
func (f *skillRepairFixture) skillVersionID(number int) int64 {
	f.t.Helper()
	id, ok := f.skillIDs[number]
	require.True(f.t, ok, "技能版本 v%d 未在夹具里登记", number)
	return id
}

// skillSource 把一条技能版本记录还原成 `assets.SkillSource`：`Ref` 带发布记录的完整
// 文件清单，`Root` 是该版本目录的绝对路径（**不是**工作副本）。
func (f *skillRepairFixture) skillSource(number int) assets.SkillSource {
	f.t.Helper()
	version, err := f.store.VersionByID(context.Background(), f.skillVersionID(number))
	require.NoError(f.t, err)
	var files []core.FileHash
	require.NoError(f.t, json.Unmarshal([]byte(version.ManifestJSON), &files))
	relDir, err := workspace.PublishedVersionRelDir(version.StoragePath)
	require.NoError(f.t, err)
	return assets.SkillSource{
		Ref: core.SkillVersionRef{
			ResourceVersionRef: core.ResourceVersionRef{
				Name:        f.skill,
				ResourceUID: version.ResourceUID,
				VersionID:   version.ID,
				VersionNo:   version.VersionNo,
				ContentHash: version.ContentHash,
			},
			Files: files,
		},
		Root: filepath.Join(f.root, filepath.FromSlash(relDir)),
	}
}

// seedBundle 按"已发布内容"构建并安装某 Agent 的 bundle，再登记版本行与指针。
// 不引用技能的 Agent 传 0。
func (f *skillRepairFixture) seedBundle(agent string, skillVersionNo int) {
	f.t.Helper()
	uid, ok := f.agents[agent]
	require.True(f.t, ok, "Agent %s 未在夹具里登记", agent)
	spec := assets.BuildSpec{
		Agent:       agent,
		AgentUID:    uid,
		ConfigHash:  "cfg-" + agent,
		ConfigRoot:  f.root,
		ConfigFiles: []string{filepath.ToSlash(filepath.Join("agents", agent, "AGENT.md"))},
		Dest:        f.t.TempDir(),
	}
	if skillVersionNo > 0 {
		spec.Skills = []assets.SkillSource{f.skillSource(skillVersionNo)}
	}
	manifest, err := assets.Build(context.Background(), spec)
	require.NoError(f.t, err)
	installed, err := assets.Install(context.Background(), spec.Dest, f.assetRoot)
	require.NoError(f.t, err)
	require.Equal(f.t, manifest.BundleHash, installed.Manifest.BundleHash)

	raw, err := json.Marshal(installed.Manifest)
	require.NoError(f.t, err)
	id := int64(200 + len(f.bundleIDs) + 1)
	f.store.putVersion(core.ResourceVersion{
		ID:           id,
		Kind:         core.ResourceAgentBundle,
		ResourceUID:  core.ResourceUID(uid),
		ResourceID:   string(uid),
		VersionNo:    1,
		ContentHash:  installed.Manifest.BundleHash,
		ManifestJSON: string(raw),
		StoragePath:  installed.Dir,
	})
	f.store.pointTo(core.ResourceAgentBundle, string(uid), core.ResourceUID(uid), id)
	f.bundleIDs[agent] = id
}

// publishedManifest 读回某 Agent 当前已发布 bundle 的清单。
func (f *skillRepairFixture) publishedManifest(agent string) assets.Manifest {
	f.t.Helper()
	uid := f.agents[agent]
	pointer, err := f.store.PointerByUID(context.Background(), core.ResourceAgentBundle, core.ResourceUID(uid))
	require.NoError(f.t, err)
	version, err := f.store.VersionByID(context.Background(), pointer.PublishedVersionID)
	require.NoError(f.t, err)
	var manifest assets.Manifest
	require.NoError(f.t, json.Unmarshal([]byte(version.ManifestJSON), &manifest))
	return manifest
}

// installedDir 返回某 Agent 当前已发布 bundle 的内容寻址目录。
func (f *skillRepairFixture) installedDir(agent string) string {
	f.t.Helper()
	manifest := f.publishedManifest(agent)
	dir, err := assets.Resolve(context.Background(), f.assetRoot, agent, manifest.BundleHash)
	require.NoError(f.t, err)
	return dir
}

// newRepublisher 按夹具装配联动重发器（T036 的单元入口）。
func (f *skillRepairFixture) newRepublisher() *skillRepublisher {
	return newSkillRepublisher(skillRepublishDeps{
		AssetRoot:     f.assetRoot,
		WorkspaceRoot: f.root,
		Versions:      f.store,
		Operations:    f.operations,
		Payloads:      f.payloads,
		PayloadStore:  f.payloads,
		AgentGate:     f.gate,
		MaxBytes:      repairMaxBundleBytes,
		Logger:        repairLogger(),
		Now:           f.clock.Now,
	})
}

// newRepairer 按夹具装配运行期修复器。
func (f *skillRepairFixture) newRepairer(admin SkillRepublisher) *SkillRepairer {
	return NewSkillRepairer(SkillRepairerDeps{
		Distribution: f.distribution,
		Versions:     f.store,
		Admin:        admin,
		Logger:       repairLogger(),
		Now:          f.clock.Now,
		Batch:        repairRepairBatch,
		Interval:     repairRepairInterval,
	})
}

// TestSkillPublish_RepublishesDependents 守 C21 的依赖发现面：只重发**直接依赖**该技能
// 的 Agent，已引用 live 版本走 `skipped`，不依赖的 Agent 不出现；逐 Agent 结果按名排序，
// `skipped` 不产生任何发布写；指针 CAS 的期望值来自锁内重读。
func TestSkillPublish_RepublishesDependents(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1) // 引用 v1（落后于 live=v2）→ 必须重发
	f.seedBundle(repairAgentBeta, 2)  // 引用 v2（= live）→ skipped
	f.seedBundle(repairAgentGamma, 0) // 不引用技能 → 不出现

	results, err := f.newRepublisher().republishDependentBundles(context.Background(), repairSkillName, repairActor())
	require.NoError(t, err)

	require.Len(t, results, 2, "只重发直接依赖该技能的 Agent：gamma 不得出现在逐 Agent 结果里")
	require.Equal(t, repairAgentAlpha, results[0].Agent, "逐 Agent 结果必须按 Agent 名排序")
	require.Equal(t, repairStatusOK, results[0].Status, "引用旧版本且重建成功必须是 ok")
	require.Equal(t, repairAgentBeta, results[1].Agent)
	require.Equal(t, repairStatusSkipped, results[1].Status, "已引用 live 版本必须幂等判 skipped")
	for _, result := range results {
		require.NotEqual(t, repairAgentGamma, result.Agent, "不依赖该技能的 Agent 不得进入修复范围")
	}

	publishes := f.store.publishCalls()
	require.Len(t, publishes, 1, "skipped 的 Agent 不得产生发布写")
	require.Equal(t, f.agents[repairAgentAlpha], publishes[0].uid)
	require.Equal(t, f.bundleIDs[repairAgentAlpha], publishes[0].expected,
		"指针 CAS 的期望值必须是锁内重读到的当前指针（不是调用方入参）")

	manifest := f.publishedManifest(repairAgentAlpha)
	require.Len(t, manifest.Skills, 1)
	require.Equal(t, f.skillVersionID(2), manifest.Skills[0].VersionID,
		"重发后的 bundle 必须引用锁内复核过的 live 版本")
}

// TestSkillPublish_DoesNotPublishWorkCopy 守"重建不读工作副本"（§2.3.7 关键约束）：
// 工作副本与工作区 AGENT.md 同时分叉时，重建结果仍必须逐字等于已发布内容——技能字节
// 来自已发布版本目录，配置字节来自旧 bundle 的 `agent/` 子树。
func TestSkillPublish_DoesNotPublishWorkCopy(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1)
	// 工作副本与工作区定义同时漂移：任何"顺手读工作区"的实现都会把这两份字节装进去。
	writeWorkspaceFile(t, f.root, "skills/"+repairSkillName+"/SKILL.md", repairWorkCopyBody)
	writeWorkspaceFile(t, f.root, "agents/"+repairAgentAlpha+"/AGENT.md", repairAgentDivergedBody)

	results, err := f.newRepublisher().republishDependentBundles(context.Background(), repairSkillName, repairActor())
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, repairStatusOK, results[0].Status)

	dir := f.installedDir(repairAgentAlpha)
	skillBody, err := os.ReadFile(filepath.Join(dir, "skills", repairSkillName, "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, repairSkillV2Body, string(skillBody),
		"技能字节必须来自已发布版本目录，不得是工作副本")

	configBody, err := os.ReadFile(filepath.Join(dir, "agent", "agents", repairAgentAlpha, "AGENT.md"))
	require.NoError(t, err)
	require.Equal(t, repairAgentBody(repairAgentAlpha), string(configBody),
		"配置字节必须来自旧 bundle，不得是工作区里未发布的改动")
}

// TestSkillRepublish_OutOfOrderPublishKeepsNewest 守 C21 的乱序发布面：锁内读到的 live
// 版本在发布前复查时已被更新 → `superseded`、零发布；等 live 稳定后重跑才发布，且发布的
// 引用是**最新**版本，晚到的旧版本从未落库。
func TestSkillRepublish_OutOfOrderPublishKeepsNewest(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1)
	// 锁内读 v2，发布前复查读到 v3：模拟"V3 的联动先完成、V2 的后完成"。
	f.store.scriptPointerVersion(core.ResourceSkill, repairSkillName,
		f.skillVersionID(2), f.skillVersionID(3))

	results, err := f.newRepublisher().republishDependentBundles(context.Background(), repairSkillName, repairActor())
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, repairStatusSuperseded, results[0].Status,
		"发布前复查发现 live 已变，必须判 superseded 而不是照旧发布")
	require.Empty(t, f.store.publishCalls(), "superseded 不得发布任何指针——那会让 bundle 版本倒退")

	stable := f.publishedManifest(repairAgentAlpha)
	require.Equal(t, f.skillVersionID(1), stable.Skills[0].VersionID,
		"被挡下的那一轮不得改动已发布引用")

	// live 稳定在 v3 后重跑：这次必须发布，且引用 v3（不得回落到 v2）。
	f.store.setLive(core.ResourceSkill, repairSkillName, f.skillVersionID(3))
	results, err = f.newRepublisher().republishDependentBundles(context.Background(), repairSkillName, repairActor())
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, repairStatusOK, results[0].Status)
	require.Len(t, f.store.publishCalls(), 1)

	final := f.publishedManifest(repairAgentAlpha)
	require.Equal(t, f.skillVersionID(3), final.Skills[0].VersionID,
		"晚到的旧版本重发不得覆盖新版本：最终引用必须是最新发布")
	require.NotEqual(t, f.skillVersionID(2), final.Skills[0].VersionID)
}

// TestSkillRepair_RecoversWithoutRestart 守"失败项在运行期下一轮自愈"：第一轮失败后，
// 退避未到的下一轮不尝试；推进时钟后**同一实例**自动重试成功，不需要重启。
func TestSkillRepair_RecoversWithoutRestart(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1)
	admin := newSkillRepairAdmin(f.store, repairStatusFailed, repairStatusOK)
	repairer := f.newRepairer(admin)

	report, err := repairer.RepairOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Attempted, "落后项必须在第一轮被尝试")
	require.Len(t, report.Results, 1)
	require.Equal(t, repairStatusFailed, report.Results[0].Status)

	report, err = repairer.RepairOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, report.Attempted, "退避未到时不得立刻重试（否则失败项会打满每一轮）")
	require.Empty(t, report.Results)
	require.Equal(t, 1, admin.callCount())

	f.clock.Advance(repairRepairInterval + time.Second)
	report, err = repairer.RepairOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Attempted, "退避到期后同一实例必须自动重试")
	require.Len(t, report.Results, 1)
	require.Equal(t, repairStatusOK, report.Results[0].Status)
	require.Equal(t, 2, admin.callCount(), "恢复过程不得重启进程或重建实例")

	// 修好之后不再进入修复队列：判据与 republishOne 的 skipped 完全一致。
	report, err = repairer.RepairOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, report.Attempted, "已修好的项不得每轮重复发布")
}

// TestSkillRepair_BlockedAfterRepeatedFailures 守 C21 的另一半：连续失败有**界**——
// 8 次之后转 `blocked` 并不再尝试；一次全量扫描（`RepairAll`）复位计数、重新尝试，
// 不把 Agent 永久卡死。
func TestSkillRepair_BlockedAfterRepeatedFailures(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1)
	admin := newSkillRepairAdmin(f.store, repairStatusFailed)
	repairer := f.newRepairer(admin)

	for round := 1; round <= repairBlockedRounds; round++ {
		report, err := repairer.RepairOnce(context.Background())
		require.NoError(t, err)
		require.Equal(t, 1, report.Attempted, "第 %d 轮仍须尝试（8 次是有界的重试预算，不是提前放弃）", round)
		require.Len(t, report.Results, 1)
		require.Equal(t, repairStatusFailed, report.Results[0].Status)
		f.clock.Advance(6 * time.Minute) // 退避上限 5m，6 分钟必然到期
	}
	require.Equal(t, repairBlockedRounds, admin.callCount())

	report, err := repairer.RepairOnce(context.Background())
	require.NoError(t, err)
	require.Zero(t, report.Attempted, "连续 8 次失败后必须转 blocked、不再每轮重试")
	require.Contains(t, report.Blocked, repairAgentAlpha, "blocked 必须逐 Agent 可见")
	require.Equal(t, repairBlockedRounds, admin.callCount())

	// 下一秒是新一轮全量扫描：复位计数后必须重新尝试，而不是永久卡死。
	admin.setScript(repairStatusOK)
	report, err = repairer.RepairAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, report.Attempted, "全量扫描必须复位 blocked 计数并重新尝试")
	require.Len(t, report.Results, 1)
	require.Equal(t, repairStatusOK, report.Results[0].Status)
}

// TestSkillRepair_RuntimeFullScanRoundRecoversBlocked 守 FR-25C-17「下一轮全量扫描
// 复位」的**运行期**面（2026-11-10 复核 P2）：blocked 项必须由同步轮传入的全量扫描
// 判据复位，不得只靠启动期 `RepairAll`。旧实现里运行期入口恒调 `RepairOnce`，故障
// 解除后即使跑完全量扫描，尝试次数也停在 8，Agent 永久卡死。
// The runtime entry must reset blocked state on a full-scan round, not just at startup.
func TestSkillRepair_RuntimeFullScanRoundRecoversBlocked(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1)
	admin := newSkillRepairAdmin(f.store, repairStatusFailed)
	repairer := f.newRepairer(admin)
	hook := assetSyncRepair(roleControl, repairer)
	require.NotNil(t, hook, "control 的运行期入口必须接到修复器")

	for round := 1; round <= repairBlockedRounds; round++ {
		require.NoError(t, hook(context.Background(), false))
		f.clock.Advance(6 * time.Minute) // 退避上限 5m，6 分钟必然到期
	}
	require.Equal(t, repairBlockedRounds, admin.callCount())

	require.NoError(t, hook(context.Background(), false))
	require.Equal(t, repairBlockedRounds, admin.callCount(),
		"非全量扫描轮不得重试 blocked——运行期不能每轮无界重发")

	// 故障解除后的全量扫描轮：必须复位 blocked 并重新尝试，同一实例、无需重启。
	admin.setScript(repairStatusOK)
	require.NoError(t, hook(context.Background(), true))
	require.Equal(t, repairBlockedRounds+1, admin.callCount(),
		"全量扫描轮必须复位 blocked 计数并在运行期重新尝试")
	final := f.publishedManifest(repairAgentAlpha)
	require.Equal(t, f.skillVersionID(2), final.Skills[0].VersionID,
		"恢复轮必须真的完成重发，而不只是把尝试次数清零")
}

// TestSkillRepair_FullScanResetKeepsBatchCap 守「复位不等于无界全量重发」：全量扫描轮
// 可以复位 blocked / 退避，但每轮仍受 `batch` 上限约束（FR-25C-17 的批次纪律）。
// A full-scan reset must not turn the runtime round into an unbounded drain.
func TestSkillRepair_FullScanResetKeepsBatchCap(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1)
	f.seedBundle(repairAgentBeta, 1)
	f.seedBundle(repairAgentGamma, 1)
	admin := newSkillRepairAdmin(f.store, repairStatusFailed)
	repairer := NewSkillRepairer(SkillRepairerDeps{
		Distribution: f.distribution,
		Versions:     f.store,
		Admin:        admin,
		Logger:       repairLogger(),
		Now:          f.clock.Now,
		Batch:        2,
		Interval:     repairRepairInterval,
	})
	hook := assetSyncRepair(roleControl, repairer)
	require.NotNil(t, hook)

	// 3 项 × 8 次失败、每轮 2 个：轮转为 [α,β] → [γ,α] → [β,γ]，12 轮后全部 blocked。
	for round := 1; round <= 12; round++ {
		require.NoError(t, hook(context.Background(), false))
		f.clock.Advance(6 * time.Minute)
	}
	blockedCalls := admin.callCount()
	require.Equal(t, 3*repairBlockedRounds, blockedCalls,
		"三项都必须在连续 8 次失败后转 blocked")
	require.NoError(t, hook(context.Background(), false))
	require.Equal(t, blockedCalls, admin.callCount(), "blocked 状态必须已全部生效")

	require.NoError(t, hook(context.Background(), true))
	require.Equal(t, blockedCalls+2, admin.callCount(),
		"全量扫描轮必须复位 blocked，但仍只尝试 batch=2 个——不得无界全量重发")
}

// TestSkillRepair_WorkerDoesNotRepublish 守"Worker 不得发起重发"：修复入口由角色决定，
// 与是否装配了修复器无关；control 的入口必须真的接到 `RepairOnce`，生产装配也确实
// 把它挂到 `AssetSyncDeps.Repair` 上。
func TestSkillRepair_WorkerDoesNotRepublish(t *testing.T) {
	t.Parallel()

	f := newSkillRepairFixture(t)
	f.seedBundle(repairAgentAlpha, 1)
	admin := newSkillRepairAdmin(f.store, repairStatusOK)
	repairer := f.newRepairer(admin)

	require.Nil(t, assetSyncRepair(roleWorker, repairer),
		"Worker 的修复入口必须是 nil——角色决定，与装配了什么无关")
	control := assetSyncRepair(roleControl, repairer)
	require.NotNil(t, control, "control 必须装配修复入口")
	require.NoError(t, control(context.Background(), false))
	require.Equal(t, 1, admin.callCount(), "control 的入口必须真的接到 RepairOnce")

	source := compactAppSource(readAppProductionSources(t))
	require.Contains(t, source, "Repair:assetSyncRepair(",
		"生产装配必须按角色挂接修复入口，而不是逐处手写 if")
}

// 编译期钉住（§3.3 冻结字面量）：签名或返回类型漂移在这里就红，不等到行为用例。
var (
	_ func(*ResourceAdmin, context.Context, string, core.Actor) ([]core.BundlePublishResult, error) = (*ResourceAdmin).republishDependentBundles
	_ func(context.Context, string, string, core.SkillVersionRef, string) (assets.Manifest, error)  = rebuildBundleWithSkill
	_ func(*SkillRepairer, context.Context) (RepairReport, error)                                   = (*SkillRepairer).RepairOnce
	_ func(*SkillRepairer, context.Context) (RepairReport, error)                                   = (*SkillRepairer).RepairAll
	_ func(context.Context, RepairDeps) (RepairReport, error)                                       = repairStaleBundleSkills
	_ func(runtimeRole, *SkillRepairer) func(context.Context, bool) error                           = assetSyncRepair
	_ SkillRepublisher                                                                              = (*ResourceAdmin)(nil)
	_ SkillRepublisher                                                                              = (*skillRepairAdmin)(nil)
)
