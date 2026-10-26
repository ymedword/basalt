package tool_test

import (
	"bytes"
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

// TestRegistry_DuplicateSemanticsUnchanged 是 US-14 的守点：新增"工具来源"视图
// （offered / accepted / skipped）后，注册池的重名语义必须原样保留——拒绝、保留
// 原注册（先注册者 = 内置赢家）、WARN 文案与键名不变。判别力：把注册池改成
// "后到覆盖"或顺手改 WARN 文案 → 本用例红；工具来源只描述装配事实，不得反向
// 修改注册池判据。
// TestRegistry_DuplicateSemanticsUnchanged pins the duplicate semantics while the
// disposition view is added: refused, original kept, warning text unchanged.
func TestRegistry_DuplicateSemanticsUnchanged(t *testing.T) {
	var logs bytes.Buffer
	registry := tool.NewRegistry(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	builtin := newStubTool("read_file")
	require.NoError(t, registry.Register(builtin), "内置工具先注册")

	err := registry.Register(newStubTool("read_file"))
	require.Error(t, err, "同名 MCP 工具必须被拒绝")
	require.Contains(t, err.Error(), "工具重名：read_file 已在注册池内")

	got, ok := registry.Resolve("read_file")
	require.True(t, ok)
	require.Same(t, builtin, got, "内置赢家：被拒的同名工具不得顶掉原注册")
	require.Equal(t, []string{"read_file"}, registry.Names(), "被拒的注册不得进池")

	output := logs.String()
	require.Contains(t, output, "level=WARN")
	require.Contains(t, output, "msg=拒绝重复注册的工具")
	require.Contains(t, output, "tool=read_file")
	require.Contains(t, output, "error=\"工具重名：read_file 已在注册池内\"")
}

// TestRegistry_ReplaceOwnerSwapsOnlyItsOwnSet：14D 受控重建的整组替换——撤下的是**该
// 归属方**此前的名字，内置与其它归属方不动；重名仍按原语义拒绝（positional 错误报在
// 对应下标），空集合把归属方的名字全部撤下。失败/重建路径全靠这条"先撤后登"，否则旧
// 名字会以孤儿身份留在池里、声明名与实体对不上。
// TestRegistry_ReplaceOwnerSwapsOnlyItsOwnSet pins the whole-set swap used by a
// controlled apply: only that owner's names move, duplicates stay refused, and an empty
// set withdraws every name the owner held.
func TestRegistry_ReplaceOwnerSwapsOnlyItsOwnSet(t *testing.T) {
	registry := newRegistry(t)

	alphaRead := newStubTool("alpha_read")
	alphaWrite := newStubTool("alpha_write")
	errs := registry.ReplaceOwner("alpha", []core.Tool{alphaRead, alphaWrite})
	require.Equal(t, []error{nil, nil}, errs)
	require.Equal(t, []string{"alpha_read", "alpha_write"}, registry.Names())

	betaRead := newStubTool("beta_read")
	require.Equal(t, []error{nil}, registry.ReplaceOwner("beta", []core.Tool{betaRead}))

	// 整组替换：alpha 的旧名字全部撤下，新名字登记，beta 不受影响。
	alphaNext := newStubTool("alpha_next")
	require.Equal(t, []error{nil}, registry.ReplaceOwner("alpha", []core.Tool{alphaNext}))
	require.Equal(t, []string{"alpha_next", "beta_read"}, registry.Names(), "Names 保持排序")
	_, ok := registry.Resolve("alpha_read")
	require.False(t, ok, "归属方此前登记的名字必须撤下")
	_, ok = registry.Resolve("alpha_write")
	require.False(t, ok)
	got, ok := registry.Resolve("beta_read")
	require.True(t, ok)
	require.Same(t, betaRead, got, "其它归属方的实例不得被替换")

	// 重名仍拒绝并保留原注册：内置先注册即是赢家。
	builtin := newStubTool("shared")
	require.NoError(t, registry.Register(builtin))
	clash := newStubTool("shared")
	fresh := newStubTool("alpha_fresh")
	errs = registry.ReplaceOwner("alpha", []core.Tool{clash, fresh})
	require.Error(t, errs[0], "重名拒绝报在对应下标")
	require.NoError(t, errs[1])
	winner, ok := registry.Resolve("shared")
	require.True(t, ok)
	require.Same(t, builtin, winner, "内置赢家：整组替换不得顶掉原注册")
	_, ok = registry.Resolve("alpha_fresh")
	require.True(t, ok)

	// 空集合 = 撤下该归属方全部名字（被拒的共享名从未登记，撤回不影响内置）。
	require.Empty(t, registry.ReplaceOwner("alpha", nil))
	require.Equal(t, []string{"beta_read", "shared"}, registry.Names())
}
