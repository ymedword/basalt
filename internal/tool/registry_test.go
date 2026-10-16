package tool_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// 本文件是工具注册池的关键回归用例（颗粒度文档 §3.2 的 registry_test.go 六例，H1~H6）。
// 注册池是"声明名 ↔ 实体"的唯一事实源：名字集由装配期导出并注入装载器（宪法 III），
// 重名会静默顶掉既有注册、未命中会静默回落——两种都属"装错了却以为装上了"。
// Regression cases for the tool pool: duplicate names and misses must be loud, and the
// exported name set must be stable (map iteration order is not).

// 编译期断言（宪法 VI 的编译期判据）：注册池满足**消费方**声明的解析接口——
// 换实现只动装配处，循环与提示词组装不感知具体类型。
var _ core.ToolResolver = (*tool.Registry)(nil)

// stubTool 是注册池用例的最小工具替身：只承载名字 / 描述 / 入参 schema，
// 执行体不参与本文件任何断言（工具调度归 ToolExecutor，不在注册面判卷）。
type stubTool struct {
	name   string
	desc   string
	schema json.RawMessage
}

func (s *stubTool) Name() string { return s.name }

func (s *stubTool) Description() string { return s.desc }

func (s *stubTool) InputSchema() json.RawMessage { return s.schema }

func (s *stubTool) Execute(context.Context, core.ToolEnv, json.RawMessage) (core.Result, error) {
	return core.Result{}, nil
}

func newStubTool(name string) *stubTool {
	return &stubTool{
		name:   name,
		desc:   name + " 的描述",
		schema: json.RawMessage(`{"type":"object"}`),
	}
}

func newRegistry(t *testing.T) *tool.Registry {
	t.Helper()
	return tool.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestRegistry_RegistersAndResolvesExplicitly 覆盖 H1：显式注册后按名命中，
// 且拿到的是**同一个实例**（不是复制品——工具持有可能的状态 / 依赖）。
// Acceptance point (H1): registration is explicit and lookup returns the same instance.
func TestRegistry_RegistersAndResolvesExplicitly(t *testing.T) {
	registry := newRegistry(t)
	readFile := newStubTool("read_file")
	require.NoError(t, registry.Register(readFile))

	got, ok := registry.Resolve("read_file")
	require.True(t, ok, "显式注册过的工具必须命中")
	require.Same(t, readFile, got, "解析必须返回注册时的那个实例")
}

// TestRegistry_RefusesDuplicateNameAndKeepsOriginal 覆盖 H2：重名注册报错，
// 且**不覆盖**原注册——静默覆盖会让"声明即绑定"的判据漂移到另一份实现上。
// Acceptance point (H2): a duplicate is refused and the original registration survives.
func TestRegistry_RefusesDuplicateNameAndKeepsOriginal(t *testing.T) {
	registry := newRegistry(t)
	first := newStubTool("read_file")
	second := newStubTool("read_file")
	require.NoError(t, registry.Register(first))

	err := registry.Register(second)
	require.Error(t, err, "重名注册必须报错")
	require.Contains(t, err.Error(), "read_file")

	got, ok := registry.Resolve("read_file")
	require.True(t, ok)
	require.Same(t, first, got, "重名被拒后原注册必须原样保留")
	require.Len(t, registry.Names(), 1, "被拒的注册不得进池")
}

// TestRegistry_NamesAreSortedAndStable 覆盖 H3：名字集排序稳定——同一池连取两次、
// 以及打乱注册顺序重建，读数必须一致（map 遍历顺序随机，直接导出会让装配读数漂移）。
// Acceptance point (H3): the exported name set is sorted and stable across builds.
func TestRegistry_NamesAreSortedAndStable(t *testing.T) {
	registry := newRegistry(t)
	for _, name := range []string{"write_file", "list_dir", "read_file"} {
		require.NoError(t, registry.Register(newStubTool(name)))
	}

	want := []string{"list_dir", "read_file", "write_file"}
	require.Equal(t, want, registry.Names())
	require.Equal(t, want, registry.Names(), "同一池连取两次必须同序")

	rebuilt := newRegistry(t)
	for _, name := range []string{"read_file", "write_file", "list_dir"} {
		require.NoError(t, rebuilt.Register(newStubTool(name)))
	}
	require.Equal(t, want, rebuilt.Names(), "注册顺序不同不得改变名字集顺序")
}

// TestRegistry_SpecsKeepRequestedOrder 覆盖 H4：规格按**传入顺序**返回（段 4 的工具列表
// 与 AGENT.md 的声明顺序一致），字段取自工具自身，未命中的名字跳过（归属校验已先行把关）。
// Acceptance point (H4): specs keep the caller's order and carry the tool's own fields.
func TestRegistry_SpecsKeepRequestedOrder(t *testing.T) {
	registry := newRegistry(t)
	require.NoError(t, registry.Register(newStubTool("read_file")))
	require.NoError(t, registry.Register(newStubTool("write_file")))

	specs := registry.Specs([]string{"write_file", "ghost_tool", "read_file"})
	require.Len(t, specs, 2, "未命中的名字跳过（它不是本池的注册项）")
	require.Equal(t, "write_file", specs[0].Name, "顺序必须与传入一致，不得排序")
	require.Equal(t, "read_file", specs[1].Name)
	require.Equal(t, "write_file 的描述", specs[0].Description, "描述取自工具自身")
	require.JSONEq(t, `{"type":"object"}`, string(specs[0].Parameters), "schema 原样透传")
}

// TestRegistry_UnresolvedNameReturnsFalse 覆盖 H5：未命中明确返回 `ok=false`——
// **不得**回落到"唯一的那个"（遍历推断是宪法 III 禁止的形态）。
// Acceptance point (H5): a miss is reported as a miss, never as a fallback guess.
func TestRegistry_UnresolvedNameReturnsFalse(t *testing.T) {
	registry := newRegistry(t)
	require.NoError(t, registry.Register(newStubTool("read_file")))

	got, ok := registry.Resolve("shell")
	require.False(t, ok, "未注册的名字必须返回 ok=false")
	require.Nil(t, got)
	require.Empty(t, registry.Specs([]string{"shell"}))
}

// TestRegistry_SatisfiesCoreToolResolver 覆盖 H6：注册池经**接口**使用（编译期断言在文件头，
// 这里是运行期同形验证）——提示词组装只认 `core.ToolResolver`，换实现不动消费方。
// Acceptance point (H6): the pool is consumed through the core interface only.
func TestRegistry_SatisfiesCoreToolResolver(t *testing.T) {
	var resolver core.ToolResolver = newRegistry(t)
	require.NoError(t, resolver.(*tool.Registry).Register(newStubTool("read_file")))

	got, ok := resolver.Resolve("read_file")
	require.True(t, ok)
	require.Equal(t, "read_file", got.Name())
	specs := resolver.Specs([]string{"read_file"})
	require.Len(t, specs, 1)
	require.Equal(t, "read_file", specs[0].Name)
}
