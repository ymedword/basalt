package tool

import (
	"fmt"
	"log/slog"
	"slices"

	"github.com/ymedword/basalt/internal/core"
)

// Registry 是进程内的工具注册池（技术方案 §4.1.1）：显式注册、按名解析、导出名字集。
// 装配期一次构建、之后只读——名字集由 `app` 导出并注入 `AgentLoader`（宪法 III 的显式面；
// `workspace` 不反向依赖本包）。解析**只查表**：未命中就是未命中，不遍历、不推断。
// Registry is the in-process tool pool: explicit registration, lookup by name, and a
// stable name set exported at assembly time. Lookup never infers or falls back.
type Registry struct {
	logger *slog.Logger
	tools  map[string]core.Tool
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
	return &Registry{logger: logger, tools: make(map[string]core.Tool)}
}

// Register 显式登记一个工具，名字取自工具自身（注册名 = 调用名，逐字一致）。
// 重名是装配期错误——拒绝并**保留原注册**：静默覆盖会让"声明即绑定"的判据漂移，
// 而声明名与实体对不上的后果是整只 Agent 不注册（加载期门禁）。
// Register adds one tool under its own Name(); a duplicate is refused and the original
// registration is kept.
func (r *Registry) Register(t core.Tool) error {
	name := t.Name()
	if _, ok := r.tools[name]; ok {
		err := fmt.Errorf("工具重名：%s 已在注册池内", name)
		r.logger.Warn("拒绝重复注册的工具", "tool", name, "error", err)
		return err
	}
	r.tools[name] = t
	return nil
}

// Resolve 按名查池；未命中返回 ok=false——**不**回落到"唯一的那个"当默认。
// Resolve looks the pool up by name; a miss returns ok=false rather than a guess.
func (r *Registry) Resolve(name string) (core.Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回**排序后**的名字集：消费方（装载器注入、日志、后续的工具清单命令）拿到的是
// 稳定顺序——map 遍历顺序随机，直接导出会让同一次装配在两次运行里输出不同（宪法 III 的同类陷阱）。
// Names returns the sorted name set so consumers see a stable order.
func (r *Registry) Names() []string {
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
