package app

// 本文件是不可变 Agent 注册表（颗粒度文档 §3.1.20 / FR-22）：一次装配产出一份完整快照，
// 热加载以**整份替换**的方式发布新快照，读路径不加锁——管理台、运行准入与调度器读到的
// 永远是同一个 `registrySnapshot` 值，不会出现"页面看得见、准入看不见"的第三种状态。
// This file is the immutable agent registry (§3.1.20 / FR-22): assembly publishes one
// snapshot, hot reload replaces it wholesale, and every reader locks nothing, so the
// console, admission and scheduler always observe the same snapshot value.
//
// 注册不变量（约束 36）：注册表**只**持有 `Registered == true` 的条目。未注册条目即使
// 被 `Replace` 送进来也等价于移除——准入读的是这份集合，失败关闭发生在这里而不是散落在
// 各个调用方。装载器只校验技能**实体**是否存在；"绑定技能是否有未撤回的发布指针"由本文件
// 的装配过滤补齐（F-03 A），启动与热加载共用同一个判定。
// Registration invariant (constraint 36): the registry holds registered entries only; a
// non-registered entry passed to Replace is equivalent to removal. The loader checks skill
// entities only, so this file also judges whether each bound skill has a live published
// pointer; startup assembly and hot reload share that judgement.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

// skillPointerSource 是注册装配读取技能发布指针的最小面：启动装配传入
// `*storage.ResourceStore`，watcher 持有同一实现。查询失败**不得**当作"无指针"或
// "全部已发布"——调用方按失败关闭处理（F-03 A）。
// skillPointerSource is the minimal read face for skill publication pointers. A failed
// lookup is never treated as "no pointers" or "everything published".
type skillPointerSource interface {
	ListPointers(ctx context.Context, kind core.ResourceKind) ([]core.ResourcePointer, error)
}

// publishedSkillNames 读出当前未撤回的技能发布指针集合。`ListPointers` 已过滤撤回行，
// 这里仍逐条复核 `DeletedAt`：判定标准落在本函数，不依赖存储实现的默认过滤。
// publishedSkillNames returns the live skill pointers, re-checking deletion itself so the
// verdict does not depend on the store's default filter.
func publishedSkillNames(ctx context.Context, source skillPointerSource) (map[string]struct{}, error) {
	if source == nil {
		return nil, errors.New("注册装配缺少技能发布指针来源")
	}
	pointers, err := source.ListPointers(ctx, core.ResourceSkill)
	if err != nil {
		return nil, fmt.Errorf("读取技能发布指针: %w", err)
	}
	published := make(map[string]struct{}, len(pointers))
	for _, pointer := range pointers {
		if pointer.DeletedAt == nil {
			published[pointer.ResourceID] = struct{}{}
		}
	}
	return published, nil
}

// missingPublishedSkills 返回条目声明绑定、但没有发布版本的技能名（升序，便于日志与
// 断言逐字比对）。条目没有解析出 Profile 时由装载器负责拒绝，这里不重复判定。
// missingPublishedSkills lists the bound skill names without a published version.
func missingPublishedSkills(entry core.AgentEntry, published map[string]struct{}) []string {
	if entry.Profile == nil {
		return nil
	}
	var missing []string
	for _, name := range entry.Profile.Skills {
		if _, ok := published[name]; !ok {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return missing
}

// skillPublicationReject 是注册拒因的固定前缀，与 `agent-md.md` §2.1 规则 5 的 WARN 同名：
// 排障时日志、页面与契约指向同一个词。
// skillPublicationReject is the fixed reject prefix shared with the contract's rule 5.
func skillPublicationReject(missing []string) string {
	return "skill version missing: " + strings.Join(missing, ", ")
}

// registryEntriesWithSkillPublication 在注册装配的出口执行约束 36 的技能分支：绑定了
// 未发布 / 已撤回技能的 Agent 在这里降级为未注册并点名 WARN（F-03 A）。查询失败上抛，
// 启动因此失败关闭，而不是把"读不到指针"默认成"资源都在"。
// registryEntriesWithSkillPublication demotes agents whose bound skills have no live
// published pointer; a failed lookup aborts startup instead of defaulting to "all good".
func registryEntriesWithSkillPublication(ctx context.Context, source skillPointerSource, entries []core.AgentEntry, logger *slog.Logger) ([]core.AgentEntry, error) {
	if logger == nil {
		logger = slog.Default()
	}
	published, err := publishedSkillNames(ctx, source)
	if err != nil {
		return nil, err
	}
	adjusted := slices.Clone(entries)
	for i := range adjusted {
		if !adjusted[i].Registered {
			continue
		}
		missing := missingPublishedSkills(adjusted[i], published)
		if len(missing) == 0 {
			continue
		}
		adjusted[i].Registered = false
		adjusted[i].RejectReason = skillPublicationReject(missing)
		logger.Warn("Agent 绑定技能无发布版本，不注册",
			"agent", adjusted[i].Name, "reason", adjusted[i].RejectReason)
	}
	return adjusted, nil
}

// registrySnapshot 是注册表的一次不可变快照：entries 按 name 升序（`/profiles` 与调度器
// 的遍历序），byName 是准入查表。发布后**不得**再修改，替换一律走整份换指针。
// registrySnapshot is one immutable view: entries sorted by name for display and the
// scheduler, byName for admission. Published snapshots are never mutated in place.
type registrySnapshot struct {
	entries []core.AgentEntry
	byName  map[string]core.AgentEntry
}

// snapshotFrom 用一份 name → entry 映射构造快照（排序与查表都由这一处定义）。
// snapshotFrom builds a snapshot from a name-keyed map; ordering and lookup live here.
func snapshotFrom(byName map[string]core.AgentEntry) registrySnapshot {
	entries := make([]core.AgentEntry, 0, len(byName))
	for _, entry := range byName {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(a, b core.AgentEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return registrySnapshot{entries: entries, byName: byName}
}

// newRegistrySnapshot 从装载结果建首版快照：未注册条目在这里被挡在外面。
// newRegistrySnapshot keeps the registered entries only, so registration is judged once.
func newRegistrySnapshot(entries []core.AgentEntry) registrySnapshot {
	byName := make(map[string]core.AgentEntry, len(entries))
	for _, entry := range entries {
		if entry.Registered {
			byName[entry.Name] = entry
		}
	}
	return snapshotFrom(byName)
}

// with 返回替换一个条目后的新快照（调用方持有旧快照，原值不变）。
// with returns a new snapshot with one entry replaced; the receiver stays untouched.
func (s registrySnapshot) with(name string, entry core.AgentEntry) registrySnapshot {
	byName := make(map[string]core.AgentEntry, len(s.byName)+1)
	for key, value := range s.byName {
		byName[key] = value
	}
	if entry.Registered {
		byName[name] = entry
	} else {
		// 未注册条目送进来等价于移除：注册表不得出现"存在但不可运行"的条目。
		delete(byName, name)
	}
	return snapshotFrom(byName)
}

// without 返回移除一个条目后的新快照；不存在时原样复制（Remove 因此天然幂等）。
// without returns a snapshot without the named entry; removing an absent name is a
// no-op that still yields an independent snapshot.
func (s registrySnapshot) without(name string) registrySnapshot {
	if _, ok := s.byName[name]; !ok {
		return s
	}
	byName := make(map[string]core.AgentEntry, len(s.byName))
	for key, value := range s.byName {
		byName[key] = value
	}
	delete(byName, name)
	return snapshotFrom(byName)
}

// agentRegistry 是进程内唯一的 Agent 注册表：`current` 指向当前快照，热加载与 US-12 的
// 归档回滚都以 CAS 换指针的方式发布——写者之间串行推进，读者永远看到一个完整版本。
// agentRegistry is the single in-process registry. Writers publish through a CAS on the
// pointer; readers never observe a half-built snapshot.
type agentRegistry struct {
	// root 是工作区根（`.basalt`），`Dir` 与配置快照都相对它解析。
	// root is the workspace root (.basalt), which Dir and snapshots resolve against.
	root string

	// contexts 冻结运行级配置快照（与运行入口共用同一个装载器实现）。
	// contexts freezes per-run configuration snapshots.
	contexts *workspace.ContextLoader

	// current 是当前快照；只通过 Load / CompareAndSwap 访问。
	// current is the published snapshot, touched only through Load / CompareAndSwap.
	current atomic.Pointer[registrySnapshot]
}

// 编译期断言：注册表就是运行入口消费的 AgentSource（§3.1.20 的方法表）。
// Compile-time assertion that the registry satisfies the consumer's port.
var _ core.AgentSource = (*agentRegistry)(nil)

// newAgentRegistry 用装载结果建首版快照；root / logger 供 `Dir` 与 `Snapshot` 使用
// （§3.1.20 的构造签名只列了 entries，但两个 AgentSource 方法需要工作区根与装载器，
// 故按端口形状补齐——见 run-state I-25）。
// newAgentRegistry publishes the initial snapshot; root and logger serve Dir and
// Snapshot, which the port requires and the sketch's signature omits.
func newAgentRegistry(root string, entries []core.AgentEntry, logger *slog.Logger) *agentRegistry {
	registry := &agentRegistry{root: root, contexts: workspace.NewContextLoader(logger)}
	initial := newRegistrySnapshot(entries)
	registry.current.Store(&initial)
	return registry
}

// Current 返回当前快照值（结构体按值返回，内部切片 / 映射只读）。
// Current returns the current snapshot value; its slice and map are read-only.
func (r *agentRegistry) Current() registrySnapshot {
	return *r.current.Load()
}

// Lookup 按名字查一个注册条目。
// Lookup returns the registered entry for name.
func (r *agentRegistry) Lookup(name string) (core.AgentEntry, bool) {
	entry, ok := r.current.Load().byName[name]
	return entry, ok
}

// Entries 返回按 name 升序的副本——调用方改动切片不影响注册表。
// Entries returns a name-sorted copy, so callers cannot mutate the registry.
func (r *agentRegistry) Entries() []core.AgentEntry {
	return slices.Clone(r.current.Load().entries)
}

// Replace 是 upsert：名字不存在即插入（US-12 归档回滚用它把旧条目放回）；重复冲突时
// 重读快照再试，不丢并发写者的更新。entry.Name 以参数 name 为准（键就是名字）。
// Replace upserts and retries on conflict; the name parameter is authoritative.
func (r *agentRegistry) Replace(name string, entry core.AgentEntry) {
	entry.Name = name
	for {
		old := r.current.Load()
		next := old.with(name, entry)
		if r.current.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Remove 移除一个条目；名字不存在时是 no-op（移除天然幂等）。
// Remove drops one entry; an absent name is a no-op.
func (r *agentRegistry) Remove(name string) {
	for {
		old := r.current.Load()
		if _, ok := old.byName[name]; !ok {
			return
		}
		next := old.without(name)
		if r.current.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Exists 报告 Agent 是否已注册——**这是准入的唯一判据来源**。
// Exists reports whether the agent is registered; admission uses this single source.
func (r *agentRegistry) Exists(name string) bool {
	_, ok := r.current.Load().byName[name]
	return ok
}

// Dir 返回 Agent 目录的绝对路径；未注册返回空串（`ToolEnv.AgentDir` 的空值语义是
// "无 Agent 上下文"，工具据此不放宽任何一格）。
// Dir returns the agent directory; an empty string means no agent context.
func (r *agentRegistry) Dir(name string) string {
	if !r.Exists(name) {
		return ""
	}
	return filepath.Join(r.root, agentsDirName, name)
}

// Snapshot 从**传入的准入条目**冻结一次运行的配置快照：字节取条目携带的已校验
// `Definition`，不再按名字二次查表、不回读磁盘——准入解析（资源清单）与快照因此对着
// 同一份配置（人工 review ② 缺陷 1）。未注册条目由冻结实现自身拒绝（失败关闭）。
// Snapshot freezes the run snapshot from the admitted entry's validated bytes; it does
// not look the agent up again, so admission and the snapshot share one configuration.
func (r *agentRegistry) Snapshot(_ context.Context, entry core.AgentEntry, now time.Time) (*core.ConfigSnapshot, error) {
	return r.contexts.Snapshot(r.root, entry, now)
}
