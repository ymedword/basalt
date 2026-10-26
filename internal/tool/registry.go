package tool

// 本文件是进程内的工具注册池（技术方案 §4.1.1）：显式注册、按名解析、导出名字集。
// US-12 起装配期一次构建、之后只读；14D 的受控重建（`mcp_server.apply`）让 MCP 工具
// 可以在运行期整组换掉，因此这里加锁并把「归属方」记下来——**只有**归属方自己的
// 工具能被它整组替换，内置工具与别的 server 的工具不受影响（C1：只换目标）。
// The registry gains an owner column and a lock in 14D: a controlled apply may swap
// exactly one server's tool set at runtime, and nothing else.
//
// 重名语义一个字未改：先到先得、拒绝重复、内置赢家（约束 8 / D-06）。

import (
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/ymedword/basalt/internal/core"
)

// Registry 是进程内的工具注册池：显式注册、按名解析、导出名字集。
// 解析**只查表**：未命中就是未命中，不遍历、不推断。
// `owners` 记每个工具名的归属（MCP server 名）；内置工具没有归属方。
// Registry is the in-process tool pool. Lookup never infers or falls back; `owners`
// records which MCP server owns a name, and built-in tools have no owner.
type Registry struct {
	mu     sync.RWMutex
	logger *slog.Logger
	tools  map[string]core.Tool
	owners map[string]string
}

// 编译期断言：注册池就是循环消费的解析面（宪法 VI——调用方只依赖接口）。
// Compile-time assertion that the pool satisfies the consumer-side interface.
var _ core.ToolResolver = (*Registry)(nil)

// NewRegistry 建一个空池；logger 用于注册期的可观测性（重名告警）。
// NewRegistry builds an empty pool; the logger carries registration-time warnings.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{
		logger: logger,
		tools:  make(map[string]core.Tool),
		owners: make(map[string]string),
	}
}

// Register 显式登记一个**无归属**工具（内置工具走这里），名字取自工具自身。
// 重名是装配期错误——拒绝并**保留原注册**：静默覆盖会让"声明即绑定"的判据漂移，
// 而声明名与实体对不上的后果是整只 Agent 不注册（加载期门禁）。
// MCP 工具**不走**这里：它们由 `ReplaceOwner` 按归属整组登记，否则受控重建时
// 撤不下旧名字（14D）。
// Register adds one ownerless tool under its own Name(); a duplicate is refused and the
// original registration is kept. MCP tools go through ReplaceOwner instead.
func (r *Registry) Register(t core.Tool) error {
	name := t.Name()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; ok {
		err := fmt.Errorf("工具重名：%s 已在注册池内", name)
		r.logger.Warn("拒绝重复注册的工具", "tool", name, "error", err)
		return err
	}
	r.tools[name] = t
	return nil
}

// ReplaceOwner 在一次加锁内把归属方的整组工具换成给定集合（14D 受控重建的唯一注册
// 入口）：先撤下该归属方此前登记的名字，再按给定顺序逐条登记。返回值与 `tools` 等长，
// nil 表示该条登记成功；重名（内置或其它归属方已占用）仍然拒绝并保留原注册。
// 撤下的是**注册表里的名字**——已发给在途 Run 的工具实例不受影响（C4：旧 Run 的快照
// 不被改写），它们随后由连接各自的排空态负责明确失败。
// ReplaceOwner swaps one owner's whole tool set under a single lock. Withdrawing a name
// never reaches into a live run's pinned instances; their link decides that.
func (r *Registry) ReplaceOwner(owner string, tools []core.Tool) []error {
	errs := make([]error, len(tools))
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner != "" {
		for name, existing := range r.owners {
			if existing != owner {
				continue
			}
			delete(r.tools, name)
			delete(r.owners, name)
		}
	}
	for i, t := range tools {
		name := t.Name()
		if _, ok := r.tools[name]; ok {
			err := fmt.Errorf("工具重名：%s 已在注册池内", name)
			r.logger.Warn("拒绝重复注册的工具", "tool", name, "owner", owner, "error", err)
			errs[i] = err
			continue
		}
		r.tools[name] = t
		if owner != "" {
			r.owners[name] = owner
		}
	}
	return errs
}

// Resolve 按名查池；未命中返回 ok=false——**不**回落到"唯一的那个"当默认。
// Resolve looks the pool up by name; a miss returns ok=false rather than a guess.
func (r *Registry) Resolve(name string) (core.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回**排序后**的名字集：消费方（装载器注入、日志、后续的工具清单命令）拿到的是
// 稳定顺序——map 遍历顺序随机，直接导出会让同一次装配在两次运行里输出不同（宪法 III 的同类陷阱）。
// Names returns the sorted name set so consumers see a stable order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Specs 按传入名字的**顺序**逐名取规格（段 4 的工具列表与声明顺序一致）；未命中的名字跳过
// ——运行时归属校验已先行把关，此处再报错只会把"已被拒绝的调用"变成第二个失败点。
// Specs keeps the caller's order and skips unknown names: ownership is checked upstream.
func (r *Registry) Specs(names []string) []core.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	specs := make([]core.ToolSpec, 0, len(names))
	for _, name := range names {
		t, ok := r.tools[name]
		if !ok {
			continue
		}
		specs = append(specs, core.ToolSpec{
			Name:        name,
			Description: t.Description(),
			Parameters:  t.InputSchema(),
		})
	}
	return specs
}

// Pin 在**一次读锁内**按名取出一组工具实例与它们的规格（14D / C4 的运行期快照）：
// 准入之后注册池再被受控重建换掉，本次运行手里的仍是这一组实例；未命中的名字跳过
// （与 Specs 同一口径）。返回的 map 与切片都归调用方所有。
// Pin snapshots instances plus specs under one read lock, so a later apply can never
// retarget a run that already admitted.
func (r *Registry) Pin(names []string) (map[string]core.Tool, []core.ToolSpec) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pinned := make(map[string]core.Tool, len(names))
	specs := make([]core.ToolSpec, 0, len(names))
	for _, name := range names {
		t, ok := r.tools[name]
		if !ok {
			continue
		}
		pinned[name] = t
		specs = append(specs, core.ToolSpec{
			Name:        name,
			Description: t.Description(),
			Parameters:  t.InputSchema(),
		})
	}
	return pinned, specs
}
