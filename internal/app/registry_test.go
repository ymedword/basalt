package app

// 本文件是不可变注册表的回归用例（颗粒度文档 §3.2.1 的 `internal/app/registry_test.go`
// 十二例：原子替换、并发读、增删查、三面一致性、排序、AgentSource 方法）。同包白盒——
// 快照形态与不变量都在包内判定。
// Regression tests for the immutable registry: atomic replacement, concurrent readers,
// upsert/remove, three-face consistency, ordering and the AgentSource methods.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// fakeSkillPointers 是注册装配的技能发布指针来源：published 里的技能视为有未撤回指针；
// err 非 nil 时模拟存储查询失败（F-03 A 的失败关闭路径）。
// fakeSkillPointers reports the named skills as published; a non-nil err simulates a
// failed store lookup.
type fakeSkillPointers struct {
	published map[string]struct{}
	err       error
}

// ListPointers 只回传技能类资源的指针；其它资源类返回空。
func (f fakeSkillPointers) ListPointers(_ context.Context, kind core.ResourceKind) ([]core.ResourcePointer, error) {
	if f.err != nil {
		return nil, f.err
	}
	if kind != core.ResourceSkill {
		return nil, nil
	}
	pointers := make([]core.ResourcePointer, 0, len(f.published))
	for id := range f.published {
		pointers = append(pointers, core.ResourcePointer{Kind: kind, ResourceID: id})
	}
	return pointers, nil
}

// fakeKnowledgeCollections 是知识集合目录替身（设计 D-19）：默认把每个名字解析成
// `col_` + 名字；`missing` 里的名字按未解析返回（等同库里没有活动集合）；
// `err` 非 nil 时模拟目录查询失败。
// fakeKnowledgeCollections resolves every name to "col_"+name unless listed in missing;
// a non-nil err simulates a failed catalog read.
type fakeKnowledgeCollections struct {
	missing map[string]struct{}
	err     error
}

// CollectionIDsByName 实现 app.KnowledgeCollectionSource。
func (f fakeKnowledgeCollections) CollectionIDsByName(_ context.Context, names []string) (map[string]string, []string, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	ids := make(map[string]string, len(names))
	var unresolved []string
	for _, name := range names {
		if _, bad := f.missing[name]; bad {
			unresolved = append(unresolved, name)
			continue
		}
		ids[name] = "col_" + name
	}
	return ids, unresolved, nil
}

// registeredAgent 造一个注册成功的条目。
// registeredAgent builds a registered entry.
func registeredAgent(name string) core.AgentEntry {
	return core.AgentEntry{Name: name, Registered: true}
}

// TestAgentProfilesPort_LiveRegistryMerge 对应约束 39（三面一致）：`/profiles` 的已注册
// 条目**每次读**都取自注册表——热加载替换 / 移除立即反映；装配期的未注册项只在仍未注册
// 时保留（problems 的唯一来源），后来注册成功则旧拒因消失，从磁盘删除的 Agent 不得从
// 装配期快照复活。
// 逆改点：端口直接返回装配期切片（缺陷 3 的原形态）→ 替换与移除两条断言红。
// The /profiles port must serve registered entries from the live registry and keep
// assembly-time rejects only while they remain unregistered.
func TestAgentProfilesPort_LiveRegistryMerge(t *testing.T) {
	startup := []core.AgentEntry{
		{Name: "demo", Registered: true, Profile: &core.Profile{
			Provider: core.ProviderRef{Name: "fake", Model: "model-v1"},
		}},
		{Name: "broken", RejectReason: "provider not registered: ghost"},
	}
	registry, _ := newTestRegistry(t, startup)
	port := agentProfilesPort{registry: registry, entries: startup}

	byName := func() map[string]core.AgentEntry {
		out := make(map[string]core.AgentEntry)
		for _, entry := range port.Profiles() {
			out[entry.Name] = entry
		}
		return out
	}
	names := func() []string {
		entries := port.Profiles()
		out := make([]string, 0, len(entries))
		for _, entry := range entries {
			out = append(out, entry.Name)
		}
		return out
	}

	require.Equal(t, []string{"broken", "demo"}, names(), "输出按 name 升序（与注册表同一遍历序）")
	require.False(t, byName()["broken"].Registered, "装配期未注册项必须保留（problems 的来源）")

	// 热加载替换：新模型立即出现。
	registry.Replace("demo", core.AgentEntry{Name: "demo", Registered: true, Profile: &core.Profile{
		Provider: core.ProviderRef{Name: "fake", Model: "model-v2"},
	}})
	require.Equal(t, "model-v2", byName()["demo"].Profile.Provider.Model,
		"注册条目必须逐次取自注册表（热加载替换后 /profiles 立即反映）")

	// 后来注册成功：旧拒因不得再展示。
	registry.Replace("broken", registeredAgent("broken"))
	require.True(t, byName()["broken"].Registered, "后来注册成功的条目以注册表为准")
	require.Empty(t, byName()["broken"].RejectReason, "旧拒因不得残留")

	// 移除：不得从装配期快照复活。
	registry.Remove("demo")
	require.NotContains(t, byName(), "demo", "被移除的 Agent 不得从装配期快照复活")
}

// newTestRegistry 在临时工作区根上建注册表；logger 传 nil（装载器回落缺省档）。
// newTestRegistry builds a registry over a temp workspace root.
func newTestRegistry(t *testing.T, entries []core.AgentEntry) (*agentRegistry, string) {
	t.Helper()
	root := t.TempDir()
	return newAgentRegistry(root, entries, nil), root
}

// TestAgentRegistry_NewKeepsRegisteredOnly：未注册条目（含轻路径待确认的第三态）不进注册表
// ——注册不变量只在构造与 Replace 两处判定，准入读不到"存在但不可运行"的条目。
func TestAgentRegistry_NewKeepsRegisteredOnly(t *testing.T) {
	t.Parallel()
	rejected := core.AgentEntry{Name: "rejected", RejectReason: "缺 provider"}
	unverified := core.AgentEntry{Name: "unverified", Unverified: true}
	registry, _ := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha"), rejected, unverified})

	_, ok := registry.Lookup("alpha")
	require.True(t, ok)
	_, ok = registry.Lookup("rejected")
	require.False(t, ok)
	_, ok = registry.Lookup("unverified")
	require.False(t, ok)
	require.Len(t, registry.Entries(), 1)
}

// TestAgentRegistry_EntriesSortedByName：装载顺序不决定遍历序——`/profiles` 与调度器拿到
// 的始终是 name 升序。
func TestAgentRegistry_EntriesSortedByName(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{
		registeredAgent("zeta"),
		registeredAgent("alpha"),
		registeredAgent("mike"),
	})

	names := make([]string, 0, 3)
	for _, entry := range registry.Entries() {
		names = append(names, entry.Name)
	}
	require.Equal(t, []string{"alpha", "mike", "zeta"}, names)
}

// TestAgentRegistry_EntriesReturnsCopy：调用方改动返回的切片不影响注册表。
func TestAgentRegistry_EntriesReturnsCopy(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha")})

	entries := registry.Entries()
	entries[0].Name = "tampered"
	require.Equal(t, "alpha", registry.Entries()[0].Name)
}

// TestAgentRegistry_LookupUnknown：未知名字不返回零值成功——`ok` 是唯一判据。
func TestAgentRegistry_LookupUnknown(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, nil)

	_, ok := registry.Lookup("ghost")
	require.False(t, ok)
	require.Empty(t, registry.Entries())
}

// TestAgentRegistry_ReplaceInsertsNew：upsert 的插入分支（US-12 归档回滚放回旧条目）。
func TestAgentRegistry_ReplaceInsertsNew(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha")})

	registry.Replace("omega", registeredAgent("omega"))
	require.True(t, registry.Exists("omega"))
	require.Len(t, registry.Entries(), 2)
}

// TestAgentRegistry_ReplaceUpdatesExisting：同名替换覆盖内容且不增条目。
func TestAgentRegistry_ReplaceUpdatesExisting(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha")})

	updated := registeredAgent("alpha")
	updated.Unverified = true
	registry.Replace("alpha", updated)

	entry, ok := registry.Lookup("alpha")
	require.True(t, ok)
	require.True(t, entry.Unverified)
	require.Len(t, registry.Entries(), 1)
}

// TestAgentRegistry_ReplaceUnregisteredRemoves：把已注册条目替换成未注册条目 = 移除——
// 注册表不得保留"存在但不可运行"的条目（失败关闭）。
func TestAgentRegistry_ReplaceUnregisteredRemoves(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha")})

	registry.Replace("alpha", core.AgentEntry{Name: "alpha", RejectReason: "坏了"})
	require.False(t, registry.Exists("alpha"))
	require.Empty(t, registry.Entries())
}

// TestAgentRegistry_Remove：移除已存在条目；移除不存在的名字是 no-op（幂等）。
func TestAgentRegistry_Remove(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha"), registeredAgent("beta")})

	registry.Remove("alpha")
	require.False(t, registry.Exists("alpha"))
	require.True(t, registry.Exists("beta"))

	registry.Remove("ghost")
	require.Len(t, registry.Entries(), 1)
}

// TestAgentRegistry_ReplaceAllUpsertsWithoutDeleting —— 同步工作集的整批发布（US-25C
// C18 / FR-25C-10）：输入之外的条目**不得**被删除（共享库缺席只从本进程内存移除，
// 注册表不推断"这次没送来"等于"该下线"）；批内同名条目整条替换；未注册条目按移除
// 处理（注册不变量仍只由本文件维护）。
// 逆改点：ReplaceAll 从空映射重建而不复制旧快照 → legacy 被抹掉，断言红。
// ReplaceAll upserts one batch without deleting entries outside it.
func TestAgentRegistry_ReplaceAllUpsertsWithoutDeleting(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{
		registeredAgent("alpha"), registeredAgent("beta"), registeredAgent("legacy"),
	})

	updated := registeredAgent("alpha")
	updated.Unverified = true
	require.NoError(t, registry.ReplaceAll([]core.AgentEntry{
		updated,
		registeredAgent("gamma"),
		{Name: "beta", RejectReason: "同步拒绝"},
	}))

	entry, ok := registry.Lookup("alpha")
	require.True(t, ok)
	require.True(t, entry.Unverified, "批内同名条目必须整条替换")
	require.True(t, registry.Exists("gamma"), "批内新条目必须插入")
	require.False(t, registry.Exists("beta"), "未注册条目按移除处理")
	require.True(t, registry.Exists("legacy"), "输入之外的条目不得被删除")

	names := make([]string, 0, len(registry.Entries()))
	for _, got := range registry.Entries() {
		names = append(names, got.Name)
	}
	require.Equal(t, []string{"alpha", "gamma", "legacy"}, names, "快照仍按 name 升序")
}

// TestAgentRegistry_CurrentSnapshotIsImmutable：已发布的快照不会被后续替换改写——持有旧
// 快照的读者看到的仍是它读到的那一刻（原子替换的语义）。
func TestAgentRegistry_CurrentSnapshotIsImmutable(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha")})

	before := registry.Current()
	registry.Replace("beta", registeredAgent("beta"))
	registry.Remove("alpha")

	require.Len(t, before.entries, 1, "旧快照不得被后续写者在原地修改")
	require.Equal(t, "alpha", before.entries[0].Name)
	require.Len(t, registry.Current().entries, 1)
	require.Equal(t, "beta", registry.Current().entries[0].Name)
}

// TestAgentRegistry_ConcurrentReplace：并发写者 + 并发读者下无丢更新、无半成品快照——
// 最终集合恰好是全部被插入的名字（011-race 的同族形态由 TestAgentRegistry_ConcurrentReplace
// 承担，见颗粒度文档 §4.4）。
func TestAgentRegistry_ConcurrentReplace(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, nil)

	const writers = 8
	const perWriter = 16
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				name := fmt.Sprintf("agent-%d-%d", writer, i)
				registry.Replace(name, registeredAgent(name))
				// 并发读者：每次读都必须是完整快照（entries 与 byName 长度一致）。
				// 子 goroutine 里不用 require（FailNow 只能在测试 goroutine 调）。
				current := registry.Current()
				if len(current.entries) != len(current.byName) {
					t.Errorf("快照不完整：entries=%d byName=%d", len(current.entries), len(current.byName))
					return
				}
				_ = registry.Entries()
			}
		}(writer)
	}
	wg.Wait()

	current := registry.Current()
	require.Len(t, current.entries, writers*perWriter)
	require.Len(t, current.byName, writers*perWriter)
	require.Len(t, registry.Entries(), writers*perWriter)
}

// TestAgentRegistry_DirAndSnapshotGuards：未注册 Agent 没有目录、也没有可冻结的配置——
// 两个 AgentSource 方法都按"不存在"回答，不退回磁盘现读。快照的拒绝落在唯一的冻结
// 实现里（`Snapshot` 不再按名字二次查表，改用准入条目携带的已校验字节——人工 review ②
// 缺陷 1），这里经注册表适配器判同一条守卫。
func TestAgentRegistry_DirAndSnapshotGuards(t *testing.T) {
	t.Parallel()
	registry, root := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha")})

	require.Equal(t, filepath.Join(root, agentsDirName, "alpha"), registry.Dir("alpha"))
	require.Empty(t, registry.Dir("ghost"))

	_, err := registry.Snapshot(context.Background(), core.AgentEntry{Name: "ghost"}, time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "未注册")
}

// TestAgentRegistry_ThreeFacesConsistent：`Current()` / `Entries()` / `Lookup()` 三面在
// 任意写序之后仍然一致——管理台、准入、调度器读的是同一份集合（FR-22）。
func TestAgentRegistry_ThreeFacesConsistent(t *testing.T) {
	t.Parallel()
	registry, _ := newTestRegistry(t, []core.AgentEntry{registeredAgent("alpha"), registeredAgent("beta")})

	registry.Replace("gamma", registeredAgent("gamma"))
	registry.Remove("alpha")
	registry.Replace("beta", core.AgentEntry{Name: "beta", Registered: true, Unverified: true})

	current := registry.Current()
	entries := registry.Entries()
	require.Equal(t, current.entries, entries)
	for _, entry := range entries {
		looked, ok := registry.Lookup(entry.Name)
		require.True(t, ok)
		require.Equal(t, entry, looked)
	}
	for name := range current.byName {
		require.True(t, registry.Exists(name))
	}
	require.False(t, registry.Exists("alpha"))
}

// syncAgentMarkdown 造一份可装载的 AGENT.md：provider 名 fake 由用例注入名字集，
// schedules 段用于验证热加载后调度器拿到的是新定义。
// syncAgentMarkdown builds a loadable AGENT.md whose schedules exercise the sync path.
func syncAgentMarkdown(name, model string) string {
	return fmt.Sprintf(`---
name: %s
description: 热加载夹具
provider:
  name: fake
  model: %s
schedules:
  - id: %s-daily
    cron: "0 0 8 * * *"
    zone: UTC
    message: "%s 播报"
---

你是 %s。
`, name, model, name, name, name)
}

// syncAgentMarkdownWithSkills 在可装载夹具上追加 skills 绑定（行内 YAML 列表）。
// syncAgentMarkdownWithSkills appends a skill binding to the loadable fixture.
func syncAgentMarkdownWithSkills(name, model string, skills ...string) string {
	return strings.Replace(syncAgentMarkdown(name, model),
		"schedules:", "skills: ["+strings.Join(skills, ", ")+"]\nschedules:", 1)
}

// syncAgentMarkdownWithKnowledge 在可装载夹具上追加 `knowledge:` 声明（块式列表，
// access 全写 read——闭集只认 read）。
// syncAgentMarkdownWithKnowledge appends a `knowledge:` block to the fixture.
func syncAgentMarkdownWithKnowledge(name, model string, collections ...string) string {
	block := "knowledge:\n"
	for _, collection := range collections {
		block += "  - name: " + collection + "\n    access: read\n"
	}
	return strings.Replace(syncAgentMarkdown(name, model), "schedules:", block+"schedules:", 1)
}

// fakeScheduleRegistry 记录注册集合与对账结果（装配层联动的调度器消费面）。
// fakeScheduleRegistry records the registry surface the scheduler exposes.
type fakeScheduleRegistry struct {
	mu         sync.Mutex
	registered map[string]bool
	reconciled map[string][]core.Schedule
}

func newFakeScheduleRegistry() *fakeScheduleRegistry {
	return &fakeScheduleRegistry{
		registered: make(map[string]bool),
		reconciled: make(map[string][]core.Schedule),
	}
}

func (f *fakeScheduleRegistry) SetRegistered(agent string, registered bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered[agent] = registered
	return nil
}

func (f *fakeScheduleRegistry) ReconcileAgent(_ context.Context, agent string, schedules []core.Schedule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconciled[agent] = append([]core.Schedule(nil), schedules...)
	return nil
}

func (f *fakeScheduleRegistry) isRegistered(agent string) (value, seen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, seen = f.registered[agent]
	return value, seen
}

func (f *fakeScheduleRegistry) schedulesOf(agent string) []core.Schedule {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Schedule(nil), f.reconciled[agent]...)
}

// newSyncFixture 建一套真实装载器 + 注册表 + 假调度器的联动夹具。
// newSyncFixture builds the sync fixture over a real loader and registry.
func newSyncFixture(t *testing.T) (*registrySync, *agentRegistry, *fakeScheduleRegistry, string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0o750))
	logger, _ := newCapturedLogger()
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{
		Providers: []string{"fake"},
		Logger:    logger,
	})
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	registry := newAgentRegistry(root, entries, logger)
	scheduler := newFakeScheduleRegistry()
	syncer := newRegistrySync(registry, loader, workspace.NewSkillLibrary(root), fakeSkillPointers{}, fakeKnowledgeCollections{}, scheduler, logger)
	return syncer, registry, scheduler, root
}

// TestRegistrySync_ReplacesChangedAgent：改名 / 改模型 / 改定时后，注册表原子替换，
// 调度器同时拿到新定义（替换路径 = SetRegistered(true) + ReconcileAgent）。
func TestRegistrySync_ReplacesChangedAgent(t *testing.T) {
	syncer, registry, scheduler, root := newSyncFixture(t)
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})
	before, ok := registry.Lookup("alpha")
	require.True(t, ok)
	require.Equal(t, "model-v1", before.Profile.Provider.Model)

	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v2"))
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})

	after, ok := registry.Lookup("alpha")
	require.True(t, ok)
	require.Equal(t, "model-v2", after.Profile.Provider.Model)
	registered, seen := scheduler.isRegistered("alpha")
	require.True(t, seen)
	require.True(t, registered)
	require.Len(t, scheduler.schedulesOf("alpha"), 1)
	require.Equal(t, "alpha-daily", scheduler.schedulesOf("alpha")[0].ID)
}

// TestRegistrySync_KeepsOldEntryOnInvalidEdit：写坏 AGENT.md → 旧条目原样保留，
// 调度器不动，WARN 点名原因（约束 28 的失败保旧）。
func TestRegistrySync_KeepsOldEntryOnInvalidEdit(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0o750))
	logger, buf := newCapturedLogger()
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{Providers: []string{"fake"}, Logger: logger})
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	registry := newAgentRegistry(root, entries, logger)
	scheduler := newFakeScheduleRegistry()
	syncer := newRegistrySync(registry, loader, workspace.NewSkillLibrary(root), fakeSkillPointers{}, fakeKnowledgeCollections{}, scheduler, logger)

	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", "not: [valid")
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})

	kept, ok := registry.Lookup("alpha")
	require.True(t, ok)
	require.Equal(t, entries[0], kept, "坏编辑必须保留旧条目")
	_, seen := scheduler.isRegistered("alpha")
	require.False(t, seen, "校验失败不得触碰调度器集合")
	require.Empty(t, scheduler.schedulesOf("alpha"))
	require.Contains(t, buf.String(), "保留旧注册")
}

// TestRegistrySync_RemovesDeletedAgent：目录删除 → 注册表收回 + 调度器退休任务。
func TestRegistrySync_RemovesDeletedAgent(t *testing.T) {
	syncer, registry, scheduler, root := newSyncFixture(t)
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})
	_, ok := registry.Lookup("alpha")
	require.True(t, ok)

	require.NoError(t, os.RemoveAll(filepath.Join(root, "agents", "alpha")))
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})

	_, ok = registry.Lookup("alpha")
	require.False(t, ok)
	registered, seen := scheduler.isRegistered("alpha")
	require.True(t, seen)
	require.False(t, registered)
}

// TestRegistrySync_AddsNewAgent：新合法目录加入注册表并登记定时；未知名字的移除是 no-op。
func TestRegistrySync_AddsNewAgent(t *testing.T) {
	syncer, registry, scheduler, root := newSyncFixture(t)
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"ghost"}})
	_, seen := scheduler.isRegistered("ghost")
	require.False(t, seen, "从未注册的名字不通知调度器")

	writeWorkspaceFile(t, root, "agents/beta/AGENT.md", syncAgentMarkdown("beta", "model-b"))
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"beta"}})

	added, ok := registry.Lookup("beta")
	require.True(t, ok)
	require.Equal(t, "beta", added.Profile.Name)
	registered, seen := scheduler.isRegistered("beta")
	require.True(t, seen)
	require.True(t, registered)
	require.Len(t, scheduler.schedulesOf("beta"), 1)
	require.Equal(t, "beta-daily", scheduler.schedulesOf("beta")[0].ID)
}

// TestRegistrySync_SkillChangeLeavesRegistryUntouched：技能变更只校验工作副本并留
// 「待发布」WARN，注册表与调度器一律不动（注 9：US-11 不自动发布）。
func TestRegistrySync_SkillChangeLeavesRegistryUntouched(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0o750))
	logger, buf := newCapturedLogger()
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{Providers: []string{"fake"}, Logger: logger})
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	writeWorkspaceFile(t, root, "skills/digest/SKILL.md", "---\nname: digest\ndescription: 摘要\n---\n\n正文\n")
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	registry := newAgentRegistry(root, entries, logger)
	scheduler := newFakeScheduleRegistry()
	syncer := newRegistrySync(registry, loader, workspace.NewSkillLibrary(root), fakeSkillPointers{}, fakeKnowledgeCollections{}, scheduler, logger)
	before := registry.Entries()

	syncer.OnChange(context.Background(), workspace.ChangeSet{Skills: []string{"digest"}})

	require.Equal(t, before, registry.Entries(), "技能变更不得触碰注册表")
	_, seen := scheduler.isRegistered("alpha")
	require.False(t, seen)
	require.Empty(t, scheduler.schedulesOf("alpha"))
	require.Contains(t, buf.String(), "待发布")
}

// TestRegistrySync_RetiresAgentWithoutPublishedSkill 覆盖 F-03 A 的热加载路径：装载器只
// 校验技能实体，热加载必须以发布指针补判——绑定未发布技能的 Agent 退出注册表并让调度器
// 退休任务；指针存在时同一 Agent 正常注册（正向对照）。逆改点：syncAgent 去掉
// missingPublishedSkills 分支 → 第一条 Lookup 断言红。
// The loader accepts the skill entity; hot reload retires the agent unless a published
// pointer exists, and registers it once the pointer is present.
func TestRegistrySync_RetiresAgentWithoutPublishedSkill(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0o750))
	logger, buf := newCapturedLogger()
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{Providers: []string{"fake"}, Logger: logger})
	writeWorkspaceFile(t, root, "skills/digest/SKILL.md", "---\nname: digest\ndescription: 摘要\n---\n\n正文\n")
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdownWithSkills("alpha", "model-v1", "digest"))
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Registered, "装载器只校验技能实体（本用例的前提）")

	registry := newAgentRegistry(root, entries, logger)
	scheduler := newFakeScheduleRegistry()
	skillLibrary := workspace.NewSkillLibrary(root)
	syncer := newRegistrySync(registry, loader, skillLibrary, fakeSkillPointers{}, fakeKnowledgeCollections{}, scheduler, logger)
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})

	_, ok := registry.Lookup("alpha")
	require.False(t, ok, "绑定技能无发布版本 → 退出注册表")
	registered, seen := scheduler.isRegistered("alpha")
	require.True(t, seen)
	require.False(t, registered)
	require.Contains(t, buf.String(), "skill version missing: digest")

	// 运行中新建的技能工作副本与绑定它的新 Agent：未发布 → 同样不注册（F-03 A 的
	// 第二条路径：热加载必须对"新目录"重复同一判定，而不是只处理已有条目）。
	// A skill work copy and its new agent created while serving: no publication → not
	// registered, proving the hot-reload path re-applies the invariant to new entries.
	writeWorkspaceFile(t, root, "skills/fresh/SKILL.md", "---\nname: fresh\ndescription: 新技能\n---\n\n正文\n")
	writeWorkspaceFile(t, root, "agents/beta/AGENT.md", syncAgentMarkdownWithSkills("beta", "model-v1", "fresh"))
	syncer.OnChange(context.Background(), workspace.ChangeSet{Skills: []string{"fresh"}, Agents: []string{"beta"}})
	_, ok = registry.Lookup("beta")
	require.False(t, ok, "新建工作副本 + 新 Agent 无发布版本 → 不注册")
	_, seen = scheduler.isRegistered("beta")
	require.False(t, seen)
	require.Contains(t, buf.String(), "skill version missing: fresh")

	published := newRegistrySync(registry, loader, skillLibrary,
		fakeSkillPointers{published: map[string]struct{}{"digest": {}}}, fakeKnowledgeCollections{}, scheduler, logger)
	published.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})
	added, ok := registry.Lookup("alpha")
	require.True(t, ok, "发布指针存在时注册不变量满足")
	require.Equal(t, "alpha", added.Name)
}

// TestRegistrySync_PointerLookupFailureKeepsOldRegistry 覆盖 F-03 A 的失败关闭：存储查询
// 失败时本轮变更整体不推进——旧条目与调度器集合原样保留，留 WARN 点名失败。
// 逆改点：OnChange 忽略查询错误并当作"无指针" → 已注册的 alpha 被错误收回，断言红。
func TestRegistrySync_PointerLookupFailureKeepsOldRegistry(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0o750))
	logger, buf := newCapturedLogger()
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{Providers: []string{"fake"}, Logger: logger})
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	registry := newAgentRegistry(root, entries, logger)
	scheduler := newFakeScheduleRegistry()
	skillLibrary := workspace.NewSkillLibrary(root)
	initial := newRegistrySync(registry, loader, skillLibrary, fakeSkillPointers{}, fakeKnowledgeCollections{}, scheduler, logger)
	initial.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})
	before, ok := registry.Lookup("alpha")
	require.True(t, ok)

	failing := newRegistrySync(registry, loader, skillLibrary,
		fakeSkillPointers{err: errors.New("db unavailable")}, fakeKnowledgeCollections{}, scheduler, logger)
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v2"))
	failing.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})

	after, ok := registry.Lookup("alpha")
	require.True(t, ok, "查询失败不得收回旧条目")
	require.Equal(t, before, after, "查询失败保留旧注册表")
	registered, seen := scheduler.isRegistered("alpha")
	require.True(t, seen)
	require.True(t, registered)
	require.Contains(t, buf.String(), "读取技能发布指针失败")
}

// TestRegistryEntriesWithKnowledgeCollections_DemotesMissing —— D-19 的"注册闸门"
// 一步：声明的集合名解析不到唯一活动实体时，该 Agent 降级为未注册并点名缺失项；
// 没有声明的 Agent 不受影响，能解析的 Agent 保持注册。
// 逆改点：闸门放行缺失声明 → Registered 断言红；RejectDetail 写成固定短语而非点名
// → 缺失项断言红。
func TestRegistryEntriesWithKnowledgeCollections_DemotesMissing(t *testing.T) {
	logger, buf := newCapturedLogger()
	entries := []core.AgentEntry{
		{Name: "alpha", Registered: true, Profile: &core.Profile{Knowledge: []core.KnowledgeBinding{
			{Name: "runbooks", Access: core.KnowledgeAccessRead},
		}}},
		{Name: "beta", Registered: true, Profile: &core.Profile{Knowledge: []core.KnowledgeBinding{
			{Name: "faq", Access: core.KnowledgeAccessRead},
		}}},
		{Name: "gamma", Registered: true, Profile: &core.Profile{}},
	}

	adjusted, err := registryEntriesWithKnowledgeCollections(context.Background(),
		fakeKnowledgeCollections{missing: map[string]struct{}{"runbooks": {}}}, entries, logger)
	require.NoError(t, err)
	require.Len(t, adjusted, 3)

	require.False(t, adjusted[0].Registered, "缺失声明 → 整只不注册")
	require.Equal(t, core.AgentRejectKnowledgeCollectionMissing, adjusted[0].RejectKind)
	require.Equal(t, "runbooks", adjusted[0].RejectDetail, "拒因必须点名缺失的集合")
	require.Contains(t, adjusted[0].RejectReason, "runbooks")
	require.True(t, adjusted[1].Registered, "能解析的 Agent 保持注册")
	require.True(t, adjusted[2].Registered, "没有声明的 Agent 不参与判定")
	require.Contains(t, buf.String(), "runbooks")

	// 入参不被就地改写：闸门返回新切片，装配方拿到的旧快照仍是旧值。
	require.True(t, entries[0].Registered, "入参不得被就地修改")

	// 全部解析成功时原样返回。
	ok, err := registryEntriesWithKnowledgeCollections(context.Background(),
		fakeKnowledgeCollections{}, entries, logger)
	require.NoError(t, err)
	require.True(t, ok[0].Registered)
}

// TestRegistryEntriesWithKnowledgeCollections_FailsClosed —— 有声明却缺目录来源是
// 装配错误（不得默认"集合都在"）；目录查询失败同样上抛，启动失败关闭。
func TestRegistryEntriesWithKnowledgeCollections_FailsClosed(t *testing.T) {
	logger, _ := newCapturedLogger()
	entries := []core.AgentEntry{
		{Name: "alpha", Registered: true, Profile: &core.Profile{Knowledge: []core.KnowledgeBinding{
			{Name: "runbooks", Access: core.KnowledgeAccessRead},
		}}},
	}

	_, err := registryEntriesWithKnowledgeCollections(context.Background(), nil, entries, logger)
	require.Error(t, err, "有声明但没有目录来源：不得当成集合都在")

	_, err = registryEntriesWithKnowledgeCollections(context.Background(),
		fakeKnowledgeCollections{err: errors.New("db unavailable")}, entries, logger)
	require.ErrorContains(t, err, "db unavailable")

	// 没有声明的装配不需要目录来源（US-1 起的旧装配不因本闸门无法启动）。
	plain := []core.AgentEntry{{Name: "gamma", Registered: true, Profile: &core.Profile{}}}
	got, err := registryEntriesWithKnowledgeCollections(context.Background(), nil, plain, logger)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

// TestRegistrySync_RetiresAgentWithoutKnowledgeCollection 覆盖 D-19 的热加载路径：
// 运行中把声明的集合删掉（目录不再解析）→ Agent 退出注册表并让调度器退休任务，
// WARN 点名缺失集合；目录恢复后同一 Agent 重新注册（正向对照）。
// 逆改点：syncAgent 去掉 missingKnowledgeCollections 分支 → 第一条 Lookup 断言红。
func TestRegistrySync_RetiresAgentWithoutKnowledgeCollection(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0o750))
	logger, buf := newCapturedLogger()
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{Providers: []string{"fake"}, Logger: logger})
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	registry := newAgentRegistry(root, entries, logger)
	scheduler := newFakeScheduleRegistry()
	skillLibrary := workspace.NewSkillLibrary(root)
	syncer := newRegistrySync(registry, loader, skillLibrary, fakeSkillPointers{},
		fakeKnowledgeCollections{}, scheduler, logger)
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})
	_, ok := registry.Lookup("alpha")
	require.True(t, ok)

	// 声明一个当前不存在的集合：本轮变更必须收回注册。
	failing := newRegistrySync(registry, loader, skillLibrary, fakeSkillPointers{},
		fakeKnowledgeCollections{missing: map[string]struct{}{"runbooks": {}}}, scheduler, logger)
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md",
		syncAgentMarkdownWithKnowledge("alpha", "model-v1", "runbooks"))
	failing.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})

	_, ok = registry.Lookup("alpha")
	require.False(t, ok, "声明的集合解析不到 → 退出注册表")
	registered, seen := scheduler.isRegistered("alpha")
	require.True(t, seen)
	require.False(t, registered)
	require.Contains(t, buf.String(), "knowledge collection missing: runbooks")

	// 目录恢复（集合建出来了）：同一份文件重新注册成功。
	recovered := newRegistrySync(registry, loader, skillLibrary, fakeSkillPointers{},
		fakeKnowledgeCollections{}, scheduler, logger)
	recovered.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})
	added, ok := registry.Lookup("alpha")
	require.True(t, ok, "集合恢复后重新注册")
	require.Equal(t, "alpha", added.Name)
	registered, _ = scheduler.isRegistered("alpha")
	require.True(t, registered)
}

// TestRegistrySync_CollectionLookupFailureKeepsOldRegistry：目录查询失败与技能指针
// 失败同一收口——保留旧注册表、不动调度器、WARN 点名原因。
// 逆改点：OnChange 把查询错误当成"没有缺失"继续推进 → 注册表被推进，断言红。
func TestRegistrySync_CollectionLookupFailureKeepsOldRegistry(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "agents"), 0o750))
	logger, buf := newCapturedLogger()
	loader := workspace.NewAgentLoader(workspace.AgentLoaderOptions{Providers: []string{"fake"}, Logger: logger})
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	entries, err := loader.LoadAll(root)
	require.NoError(t, err)
	registry := newAgentRegistry(root, entries, logger)
	scheduler := newFakeScheduleRegistry()
	skillLibrary := workspace.NewSkillLibrary(root)
	initial := newRegistrySync(registry, loader, skillLibrary, fakeSkillPointers{},
		fakeKnowledgeCollections{}, scheduler, logger)
	initial.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})
	before, ok := registry.Lookup("alpha")
	require.True(t, ok)

	broken := newRegistrySync(registry, loader, skillLibrary, fakeSkillPointers{},
		fakeKnowledgeCollections{err: errors.New("catalog unavailable")}, scheduler, logger)
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md",
		syncAgentMarkdownWithKnowledge("alpha", "model-v2", "runbooks"))
	broken.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})

	after, ok := registry.Lookup("alpha")
	require.True(t, ok, "目录查询失败不得收回旧条目")
	require.Equal(t, before, after, "目录查询失败保留旧注册表")
	require.Contains(t, buf.String(), "读取知识集合目录失败")
}

// TestRegistrySync_ConcurrentChanges：不同 Agent 的变更集并发推进，注册表最终包含全部
// 条目且每个条目与自身名字一致（CAS 重读不丢并发写者，race 族用例）。
func TestRegistrySync_ConcurrentChanges(t *testing.T) {
	syncer, registry, _, root := newSyncFixture(t)
	names := []string{"alpha", "beta", "gamma", "delta"}
	for _, name := range names {
		writeWorkspaceFile(t, root, "agents/"+name+"/AGENT.md", syncAgentMarkdown(name, "model-"+name))
	}

	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{name}})
			}
		}(name)
	}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for i := 0; i < 50; i++ {
			for _, entry := range registry.Entries() {
				if entry.Name == "" {
					t.Error("快照条目缺名字")
					return
				}
			}
		}
	}()
	wg.Wait()
	<-readerDone

	for _, name := range names {
		entry, ok := registry.Lookup(name)
		require.True(t, ok, "并发推进后必须包含 %s", name)
		require.Equal(t, name, entry.Name)
	}
}

// TestStartWatcher_ReloadsOnFileChange：真实 fsnotify 接线——落盘后注册表自动换到新条目
// （startWatcher 的 Run 与防抖回调确实在后台工作）。
func TestStartWatcher_ReloadsOnFileChange(t *testing.T) {
	syncer, registry, _, root := newSyncFixture(t)
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})

	logger, _ := newCapturedLogger()
	ctx, cancel := context.WithCancel(context.Background())
	watcher, err := startWatcher(ctx, root, syncer, logger)
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		require.NoError(t, watcher.Close())
	})

	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v2"))
	require.Eventually(t, func() bool {
		entry, ok := registry.Lookup("alpha")
		return ok && entry.Profile != nil && entry.Profile.Provider.Model == "model-v2"
	}, 5*time.Second, 20*time.Millisecond, "文件变化必须经 watcher 自动换入注册表")
}

// TestStartWatcher_StopsOnContextCancel：ctx 取消后监听停止，之后的落盘不再推进注册表；
// 显式 Close 与后台收尾重复调用幂等。
func TestStartWatcher_StopsOnContextCancel(t *testing.T) {
	syncer, registry, _, root := newSyncFixture(t)
	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v1"))
	syncer.OnChange(context.Background(), workspace.ChangeSet{Agents: []string{"alpha"}})
	before, ok := registry.Lookup("alpha")
	require.True(t, ok)

	logger, _ := newCapturedLogger()
	ctx, cancel := context.WithCancel(context.Background())
	watcher, err := startWatcher(ctx, root, syncer, logger)
	require.NoError(t, err)
	cancel()
	require.NoError(t, watcher.Close())

	writeWorkspaceFile(t, root, "agents/alpha/AGENT.md", syncAgentMarkdown("alpha", "model-v2"))
	time.Sleep(time.Second)
	after, ok := registry.Lookup("alpha")
	require.True(t, ok)
	require.Equal(t, before, after, "取消之后不得再换入新条目")
}

// TestRegistryEntries_MultipleMissingSkillsSortedJoin 覆盖 agent-md.md 规则 5 的固定
// 字面量（2026-10-22 复核 R-04）：一条 WARN 点名该 Agent 的全部缺失技能，名字按升序、
// 以逗号加空格连接，而不是每个名字各发一条或按声明顺序。
// 逆改点：去掉 slices.Sort 或改连接符 → reason 断言红。
// The fixed reject literal joins every missing skill in ascending order with comma+space.
func TestRegistryEntries_MultipleMissingSkillsSortedJoin(t *testing.T) {
	missing := missingPublishedSkills(core.AgentEntry{
		Name:    "writer",
		Profile: &core.Profile{Skills: []string{"weekly", "digest", "archive"}},
	}, map[string]struct{}{})
	require.Equal(t, []string{"archive", "digest", "weekly"}, missing)
	require.Equal(t, "skill version missing: archive, digest, weekly", skillPublicationReject(missing))
}
