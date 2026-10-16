package tool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/tool"
)

// 本文件是三件内置文件工具的关键回归用例（颗粒度文档 §3.2 的 filetools_test.go 十四例，
// I1~I14，判据面 = 契约 §5.1 的参数与失败类别 + 约束 17 注）。三条纪律决定了用例形态：
// ① 授权面与执行面分离——本文件经**真策略**（policyFixture）判前十二例的判定结果，
// 只有"句柄才是保证"两例换成永远放行的桩策略（把策略面让开，句柄才成为唯一变量）；
// ② 路径基准是**当前 Agent 自有目录**，故所有入参都取相对形态，绝对路径只作对照；
// ③ 失败类别按类型断言（`core.IsSandboxViolation` / `errors.Is(err, fs.ErrNotExist)`）——
// 与执行侧 `classifyToolError` 同一口径，不比对文案全文。
// Regressions for the three builtin file tools (I1-I14). Authorization is judged through
// the real policy except in the two handle cases, where an always-allow stub makes the
// relative root handle the only thing that can refuse an out-of-root write.

// 契约 §5.1 的单文件读取上限：测试**独立抄一份**——上限是模型可见契约的一部分，
// 实现改动应当让这行断言失败。
// An independent copy of the 256 KB read cap from contract §5.1.
const fileReadLimit = 256 * 1024

// 三件工具的内联 Schema 的**独立副本**（实现里内联、测试里再抄）：Schema 是给模型看的
// 外部契约，任何改写都必须让这里失败——引用实现的常量会让两边一起改，钉不住。
// Independent copies of the inline schemas: pinning means the test must not share the
// literal with the implementation, or both would drift together.
const (
	wantReadFileSchema  = `{"type":"object","properties":{"path":{"type":"string","description":"要读取的文件路径：绝对路径，或相对当前 Agent 目录的路径；技能卡片给出的 .basalt/skills/... 形态同样可用"}},"required":["path"]}`
	wantWriteFileSchema = `{"type":"object","properties":{"path":{"type":"string","description":"目标文件路径：绝对路径，或相对当前 Agent 目录的路径；父目录必须已存在，不会自动创建"},"content":{"type":"string","description":"写入内容（整文件覆盖写）"}},"required":["path","content"]}`
	wantListDirSchema   = `{"type":"object","properties":{"path":{"type":"string","description":"要列出的目录路径：绝对路径，或相对当前 Agent 目录的路径；只列本层，不递归"}},"required":["path"]}`
)

// allowAllPolicy 是**永远放行**的桩 Policy，只用于"句柄才是保证"的两例：策略面全放行之后，
// 唯一还能挡住越界写入的就只剩相对根句柄——这才能闭卷证明句柄本身在起作用。
// An always-allow stub: with the policy out of the way, only the handle remains.
type allowAllPolicy struct{}

// Check 恒放行（桩：本类型不参与任何一例的授权判定结论）。
func (allowAllPolicy) Check(context.Context, core.Action) error { return nil }

// fileToolFixture 把三件工具接到工作区骨架上：同一份根、同一个策略实例——三件工具只经
// core.Policy 接口使用它（授权与执行分离的编译期读数）。
// The three tools share one workspace root and one policy instance.
type fileToolFixture struct {
	*policyFixture
	read  *tool.ReadFileTool
	write *tool.WriteFileTool
	list  *tool.ListDirTool
}

// newFileToolFixture 造夹具：工作区骨架沿用 policyFixture（三件工具与授权判定看同一棵树）。
func newFileToolFixture(t *testing.T, mutate func(*tool.PolicyOptions)) *fileToolFixture {
	t.Helper()
	f := newPolicyFixture(t, mutate)
	agentDir := f.path(filepath.Join("agents", policyAgent))
	return &fileToolFixture{
		policyFixture: f,
		read:          tool.NewReadFileTool(f.policy, f.root),
		write:         tool.NewWriteFileTool(f.policy, f.root, agentDir),
		list:          tool.NewListDirTool(f.policy, f.root),
	}
}

// agentDir 是当前 Agent 的自有目录——相对路径的解析基准。
func (f *fileToolFixture) agentDir() string {
	return f.path(filepath.Join("agents", policyAgent))
}

// toolEnv 造一次调用的显式执行环境：Agent 名与自有目录**成对给出**，工具不得从别处猜。
// The explicit per-call environment; the tool never guesses its context.
func (f *fileToolFixture) toolEnv() core.ToolEnv {
	// 三件文件工具不消费输出上限（截断是执行侧的事），故这里按契约缺省给一个具体值即可。
	return core.ToolEnv{
		RunID:          "run-files-1",
		SessionID:      "sess-files-1",
		AgentName:      policyAgent,
		AgentDir:       f.agentDir(),
		MaxOutputBytes: 256 * 1024,
	}
}

// runTool 调一次工具：入参是对象字面量的 JSON（与执行侧同形，不经任何封装）。
func runTool(t *testing.T, tl core.Tool, env core.ToolEnv, input string) (core.Result, error) {
	t.Helper()
	return tl.Execute(context.Background(), env, json.RawMessage(input))
}

// requiredFields 取 Schema 的 required 数组：执行侧按它判必填缺失（回 invalid_input 并写审计行）。
func requiredFields(t *testing.T, tl core.Tool) []string {
	t.Helper()
	var spec struct {
		Required []string `json:"required"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &spec), "Schema 必须是合法 JSON")
	return spec.Required
}

// TestFileTools_PinInputSchemasVerbatim 覆盖 I1：三件工具的入参 Schema 与内联常量逐字一致、
// 名字与描述非空、required 数组逐条钉住（必填缺失的判定依据就是它）。
// Acceptance point (I1): schemas, names and required arrays are pinned verbatim.
func TestFileTools_PinInputSchemasVerbatim(t *testing.T) {
	f := newFileToolFixture(t, nil)
	for _, c := range []struct {
		tool     core.Tool
		name     string
		schema   string
		required []string
	}{
		{f.read, "read_file", wantReadFileSchema, []string{"path"}},
		{f.write, "write_file", wantWriteFileSchema, []string{"path", "content"}},
		{f.list, "list_dir", wantListDirSchema, []string{"path"}},
	} {
		require.Equal(t, c.name, c.tool.Name(), "工具名与 AGENT.md 的 tools: 声明逐字比对")
		require.Equal(t, c.schema, string(c.tool.InputSchema()), "InputSchema 必须与内联常量逐字一致")
		require.Equal(t, c.required, requiredFields(t, c.tool))
		require.NotEmpty(t, c.tool.Description(), "描述是给模型的能力说明，不能为空")
	}
}

// TestReadFileTool_ResolvesAgentDirAndSkillCardPaths 覆盖 I2：真读工作区文件并回填内容；
// 相对路径按**当前 Agent 自有目录**解析，技能卡片逐字给出的 `.basalt/skills/<name>/SKILL.md`
// 形态同样能吃下（US-3 §2.2 约束 6 的跨模块约定），绝对路径原样。
// Acceptance point (I2): relative paths resolve from the agent dir, and the skill-card
// path form injected by the L1 disclosure is readable.
func TestReadFileTool_ResolvesAgentDirAndSkillCardPaths(t *testing.T) {
	f := newFileToolFixture(t, nil)
	note := f.path(filepath.Join("agents", policyAgent, "notes", "note.md"))
	require.NoError(t, os.WriteFile(note, []byte("第一行\n第二行\n"), 0o644))
	skillDir := f.path(filepath.Join("skills", "demo-skill"))
	require.NoError(t, os.MkdirAll(skillDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# 技能卡片\n"), 0o644))

	result, err := runTool(t, f.read, f.toolEnv(), `{"path":"notes/note.md"}`)
	require.NoError(t, err)
	require.Equal(t, "第一行\n第二行\n", result.Content, "相对路径的基准是当前 Agent 自有目录")

	result, err = runTool(t, f.read, f.toolEnv(), `{"path":".basalt/skills/demo-skill/SKILL.md"}`)
	require.NoError(t, err)
	require.Equal(t, "# 技能卡片\n", result.Content, "技能卡片的字面形态按工作区根解析")

	result, err = runTool(t, f.read, f.toolEnv(), fmt.Sprintf(`{"path":%q}`, note))
	require.NoError(t, err)
	require.Equal(t, "第一行\n第二行\n", result.Content, "绝对路径原样使用")
}

// TestReadFileTool_DecodesInvalidUTF8Tolerantly 覆盖 I3：UTF-8 宽容解码——非法字节以替换
// 字符呈现，**不**因编码拒绝读取（契约 §5.1）；回填给模型的内容必须仍是合法 UTF-8。
// Acceptance point (I3): invalid bytes become replacement characters instead of a refusal.
func TestReadFileTool_DecodesInvalidUTF8Tolerantly(t *testing.T) {
	f := newFileToolFixture(t, nil)
	require.NoError(t, os.WriteFile(
		f.path(filepath.Join("agents", policyAgent, "notes", "latin1.txt")),
		[]byte("A\xffB\xfeC"), 0o644,
	))
	result, err := runTool(t, f.read, f.toolEnv(), `{"path":"notes/latin1.txt"}`)
	require.NoError(t, err)
	require.Equal(t, "A�B�C", result.Content)
	require.True(t, utf8.ValidString(result.Content), "回填内容必须是合法 UTF-8")
}

// TestReadFileTool_MissingFileIsNotFound 覆盖 I4：失败类别逐字——文件不存在 → `not_found`。
// 判据按类型：错误链上必须能 `errors.Is` 到 `fs.ErrNotExist`，否则执行侧会把它归成 `runtime_error`。
// Acceptance point (I4): a missing file classifies as not_found, not runtime_error.
func TestReadFileTool_MissingFileIsNotFound(t *testing.T) {
	f := newFileToolFixture(t, nil)
	_, err := runTool(t, f.read, f.toolEnv(), `{"path":"notes/没有这个文件.txt"}`)
	require.Error(t, err)
	require.ErrorIs(t, err, fs.ErrNotExist, "缺失必须是 fs.ErrNotExist（执行侧据此归 not_found）")
	require.False(t, core.IsSandboxViolation(err), "普通缺失不是沙箱拒绝")
}

// TestReadFileTool_OutsideWorkspaceIsSandboxDenied 覆盖 I5：失败类别逐字——越界读
// → `sandbox_denied`，且拒绝文案点名规则与目标（四段式的段 1 与段 2）。
// Acceptance point (I5): reads outside the readable roots are sandbox_denied by rule name.
func TestReadFileTool_OutsideWorkspaceIsSandboxDenied(t *testing.T) {
	f := newFileToolFixture(t, nil)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("工作区外\n"), 0o644))

	for _, input := range []string{
		fmt.Sprintf(`{"path":%q}`, secret),
		`{"path":"../../../secret.txt"}`,
	} {
		_, err := runTool(t, f.read, f.toolEnv(), input)
		require.Error(t, err, "越界读必须被拒：%s", input)
		require.True(t, core.IsSandboxViolation(err), "拒绝必须是 *core.SandboxViolationError")
		require.Contains(t, err.Error(), "读取目标不在可读根之内", "段 1 点名规则")
		require.Contains(t, err.Error(), "secret.txt", "段 2 点名具体目标")
		require.Contains(t, err.Error(), refusalFixedSentence)
	}
}

// TestReadFileTool_DirectoryAndOversizeAreRuntimeErrors 覆盖 I6：失败类别逐字——除
// `not_found` 与 `sandbox_denied` 之外的读取失败都归 `runtime_error`：目标是目录（读目录
// 该用 list_dir）、超过 256 KB 上限（给可读错误并提示改用 shell 工具）。上限的边界两侧都判。
// Acceptance point (I6): directory targets and oversize files are runtime errors with
// actionable copy; the 256 KB boundary holds on both sides.
func TestReadFileTool_DirectoryAndOversizeAreRuntimeErrors(t *testing.T) {
	f := newFileToolFixture(t, nil)
	notes := filepath.Join("agents", policyAgent, "notes")

	_, err := runTool(t, f.read, f.toolEnv(), `{"path":"notes"}`)
	require.Error(t, err, "读目录必须失败")
	require.False(t, core.IsSandboxViolation(err))
	require.NotErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), "list_dir", "读目录要被指路到 list_dir")

	require.NoError(t, os.WriteFile(f.path(filepath.Join(notes, "上限.txt")),
		bytes.Repeat([]byte("x"), fileReadLimit), 0o644))
	require.NoError(t, os.WriteFile(f.path(filepath.Join(notes, "超限.txt")),
		bytes.Repeat([]byte("x"), fileReadLimit+1), 0o644))

	result, err := runTool(t, f.read, f.toolEnv(), `{"path":"notes/上限.txt"}`)
	require.NoError(t, err, "恰好等于上限可读")
	require.Len(t, result.Content, fileReadLimit)

	_, err = runTool(t, f.read, f.toolEnv(), `{"path":"notes/超限.txt"}`)
	require.Error(t, err, "超过上限必须失败")
	require.False(t, core.IsSandboxViolation(err))
	require.NotErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), "shell", "超限文案必须提示改用 shell 工具处理大文件")
}

// TestWriteFileTool_WritesBytesAndOverwrites 覆盖 I7：出参是**写入字节数**与目标路径；
// 覆盖写（旧内容不残留）；落点在当前 Agent 自有目录之内。
// Acceptance point (I7): the result reports the byte count and the write overwrites.
func TestWriteFileTool_WritesBytesAndOverwrites(t *testing.T) {
	f := newFileToolFixture(t, nil)
	content := "写入内容\n第二行\n"
	result, err := runTool(t, f.write, f.toolEnv(),
		fmt.Sprintf(`{"path":"notes/new.md","content":%q}`, content))
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("已写入 %d 字节：notes/new.md", len(content)), result.Content)

	target := f.path(filepath.Join("agents", policyAgent, "notes", "new.md"))
	onDisk, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, content, string(onDisk), "落点在当前 Agent 自有目录之内")

	_, err = runTool(t, f.write, f.toolEnv(), `{"path":"notes/new.md","content":"短"}`)
	require.NoError(t, err)
	onDisk, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "短", string(onDisk), "覆盖写：旧内容不得残留")

	// H7 读数（json 解码保真）：入参由标准库编码器产出——它对尖括号与 & 默认写成转义序列
	// （与模型发来的常规写法同形）；工具必须解成真实字节落盘，而不是把转义序列的原文字符
	// 写进文件。前提一并断言：编码器确实没把尖括号原样写出去，否则这行读数会退化成空转。
	escaped, err := json.Marshal(map[string]string{
		"path":    "notes/escaped.md",
		"content": "a<b&c",
	})
	require.NoError(t, err)
	require.NotContains(t, string(escaped), "<", "读数前提：编码器把尖括号转义了（未产出裸字符）")
	_, err = runTool(t, f.write, f.toolEnv(), string(escaped))
	require.NoError(t, err, "带转义序列的入参必须被正常解码")
	onDisk, err = os.ReadFile(f.path(filepath.Join("agents", policyAgent, "notes", "escaped.md")))
	require.NoError(t, err)
	require.Equal(t, "a<b&c", string(onDisk))
}

// TestWriteFileTool_MissingParentDirIsRefused 覆盖 I8：父目录不存在 → 拒（契约 §5.1：
// 不自动创建），且拒绝之后**不得**留下任何目录——工具不替模型建路径。
// Acceptance point (I8): a missing parent is refused and nothing is created.
func TestWriteFileTool_MissingParentDirIsRefused(t *testing.T) {
	f := newFileToolFixture(t, nil)
	_, err := runTool(t, f.write, f.toolEnv(), `{"path":"notes/deep/new.md","content":"x"}`)
	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err), "父目录缺失由授权面判拒（四段式，模型可见）")
	require.Contains(t, err.Error(), "目标父目录不存在")
	require.Contains(t, err.Error(), refusalFixedSentence)
	require.NoDirExists(t, f.path(filepath.Join("agents", policyAgent, "notes", "deep")),
		"拒绝之后不得代建父目录")
}

// TestWriteFileTool_ProtectedTargetsAreRefused 覆盖 I9：失败类别逐字——写保护面
// （`scripts/` 只读区、`AGENT.md` 保护文件）一律 `sandbox_denied`，保护规则先于一般放行。
// Acceptance point (I9): protected targets are sandbox_denied by their rule names.
func TestWriteFileTool_ProtectedTargetsAreRefused(t *testing.T) {
	f := newFileToolFixture(t, nil)
	for _, c := range []struct{ path, rule string }{
		{filepath.Join("scripts", "debug.sh"), "scripts/ 是只读区，写入被拒绝"},
		{"AGENT.md", "AGENT.md 是受保护文件，写入被拒绝"},
	} {
		input := fmt.Sprintf(`{"path":%q,"content":"改一改"}`, c.path)
		_, err := runTool(t, f.write, f.toolEnv(), input)
		require.Error(t, err, "写保护面必须被拒：%s", c.path)
		require.True(t, core.IsSandboxViolation(err))
		require.Contains(t, err.Error(), c.rule)
		require.Contains(t, err.Error(), refusalFixedSentence)
	}
}

// TestWriteFileTool_RelativeRootHandleBlocksEscape 覆盖 I10：**写入经相对根句柄**——换成
// 永远放行的桩策略后，越界写入仍失败，且工作区外/写入根外**没有**留下文件。这正是
// "先检查路径、随后普通 open 证明不了竞争窗口"的闭卷判据：挡住它的是句柄，不是策略。
// 本机（Windows 未开开发者模式）建不了软连接，故这里判 `绝对路径` 与 `..` 两形态；
// 软连接形态见 I11，两半合起来才是完整判据。
// Acceptance point (I10): with the policy out of the way the handle alone refuses escape.
func TestWriteFileTool_RelativeRootHandleBlocksEscape(t *testing.T) {
	f := newFileToolFixture(t, nil)
	write := tool.NewWriteFileTool(allowAllPolicy{}, f.root, f.agentDir())

	outside := filepath.Join(t.TempDir(), "escaped.txt")
	_, err := runTool(t, write, f.toolEnv(),
		fmt.Sprintf(`{"path":%q,"content":"逃逸"}`, outside))
	require.Error(t, err, "工作区外的绝对路径必须被句柄拒")
	require.ErrorContains(t, err, "escapes from parent", "拒绝出自相对根句柄（标准库的逃逸判定）")
	require.NoFileExists(t, outside)

	_, err = runTool(t, write, f.toolEnv(), `{"path":"../../escape-me.txt","content":"逃逸"}`)
	require.Error(t, err, "写出写入根（`..` 形态）必须被句柄拒")
	require.ErrorContains(t, err, "escapes from parent")
	require.NoFileExists(t, f.path("escape-me.txt"), "句柄拒了，就不得有文件落盘")
}

// TestWriteFileTool_RelativeRootHandleBlocksSymlinkedParent 覆盖 I11：竞争窗口的靶心——
// 授权面看到的父目录无害（桩策略恒放行，等价于"判定时还是个普通目录"），随后父目录是指向
// 工作区外的软连接；只有相对根句柄能挡住这次写入，工作区外不得留下文件。
// Acceptance point (I11): a parent swapped for an out-of-workspace symlink still fails.
func TestWriteFileTool_RelativeRootHandleBlocksSymlinkedParent(t *testing.T) {
	f := newFileToolFixture(t, nil)
	outside := t.TempDir()
	parent := f.path(filepath.Join("agents", policyAgent, "swappable"))
	require.NoError(t, os.Mkdir(parent, 0o750))
	require.NoError(t, os.Remove(parent))
	if err := os.Symlink(outside, parent); err != nil {
		t.Skip("本平台不允许创建符号链接（Windows 需开发者模式或特权）：" + err.Error())
	}
	write := tool.NewWriteFileTool(allowAllPolicy{}, f.root, f.agentDir())

	_, err := runTool(t, write, f.toolEnv(), `{"path":"swappable/escaped.txt","content":"逃逸"}`)
	require.Error(t, err, "软连接父目录必须被句柄拒")
	require.NoFileExists(t, filepath.Join(outside, "escaped.txt"),
		"写入不得穿到软连接指向的工作区外")
}

// TestListDirTool_ListsOneLevelWithTypeSizeName 覆盖 I12：出参格式逐字「类型 大小 名字」、
// 按名字排序（标准库契约）、大小只对普通文件有意义（其余 `-` 占位）、空目录给明确文案。
// Acceptance point (I12): the listing format, ordering and the empty-directory text.
func TestListDirTool_ListsOneLevelWithTypeSizeName(t *testing.T) {
	f := newFileToolFixture(t, nil)
	base := f.path(filepath.Join("agents", policyAgent, "listing"))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "sub"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "empty"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(base, "a.txt"), []byte("aaa"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, "b.txt"), []byte("bb"), 0o644))

	result, err := runTool(t, f.list, f.toolEnv(), `{"path":"listing"}`)
	require.NoError(t, err)
	require.Equal(t, "file 3 a.txt\nfile 2 b.txt\ndir - empty\ndir - sub\n", result.Content)

	result, err = runTool(t, f.list, f.toolEnv(), `{"path":"listing/empty"}`)
	require.NoError(t, err)
	require.Equal(t, "(空目录)", result.Content, "空目录不能被渲染成空串（空串与「工具什么都没返回」无法区分）")
}

// TestListDirTool_DoesNotRecurseAndDoesNotFollowSymlinks 覆盖 I13：只列**本层**——两层深的
// 用例逐字断言输出，子目录内容不得摊平进来；被换成软连接的条目以 `link` 出现在本层，
// **不跟随**（链接指向的目录内容不得出现在结果里）。
// Acceptance point (I13): one level only, and symlinks are listed rather than followed.
func TestListDirTool_DoesNotRecurseAndDoesNotFollowSymlinks(t *testing.T) {
	f := newFileToolFixture(t, nil)
	base := f.path(filepath.Join("agents", policyAgent, "listing"))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "sub", "deep"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(base, "sub", "inner.txt"), []byte("i"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(base, "sub", "deep", "deeper.txt"), []byte("dd"), 0o644))

	t.Run("不递归", func(t *testing.T) {
		result, err := runTool(t, f.list, f.toolEnv(), `{"path":"listing/sub"}`)
		require.NoError(t, err)
		require.Equal(t, "dir - deep\nfile 1 inner.txt\n", result.Content,
			"孙文件不得出现：即便只多列一层也会被这行抓住")
	})

	t.Run("不跟随软连接", func(t *testing.T) {
		outside := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("工作区外\n"), 0o644))
		if err := os.Symlink(outside, filepath.Join(base, "sublink")); err != nil {
			t.Skip("本平台不允许创建符号链接（Windows 需开发者模式或特权）：" + err.Error())
		}
		result, err := runTool(t, f.list, f.toolEnv(), `{"path":"listing"}`)
		require.NoError(t, err)
		require.Contains(t, result.Content, "link - sublink\n", "链接以本层条目出现")
		require.NotContains(t, result.Content, "secret.txt", "链接指向的目录内容不得被摊平进来")
	})
}

// TestListDirTool_FailuresMatchContract 覆盖 I14：失败类别逐字——目录不存在 → `not_found`；
// 越界目录 → `sandbox_denied`（四段式）；目标是文件 → `runtime_error`（其余两类的反面判据）。
// Acceptance point (I14): the three failure classes of list_dir match the contract.
func TestListDirTool_FailuresMatchContract(t *testing.T) {
	f := newFileToolFixture(t, nil)

	_, err := runTool(t, f.list, f.toolEnv(), `{"path":"notes/没有这个目录"}`)
	require.Error(t, err)
	require.ErrorIs(t, err, fs.ErrNotExist, "目录缺失 → not_found")
	require.False(t, core.IsSandboxViolation(err))

	outside := t.TempDir()
	_, err = runTool(t, f.list, f.toolEnv(), fmt.Sprintf(`{"path":%q}`, outside))
	require.Error(t, err)
	require.True(t, core.IsSandboxViolation(err), "越界目录 → sandbox_denied")
	require.Contains(t, err.Error(), "读取目标不在可读根之内")
	require.Contains(t, err.Error(), refusalFixedSentence)

	_, err = runTool(t, f.list, f.toolEnv(), `{"path":"notes/notes.txt"}`)
	require.Error(t, err, "目标是文件必须失败")
	require.False(t, core.IsSandboxViolation(err))
	// 契约为 list_dir 只列了 not_found / sandbox_denied 两类：目标不是目录按"这个目录
	// 不存在"归 not_found 并指路 read_file——**跨平台一致**（直接靠 os.ReadDir 的平台
	// 错误会让同一输入在 Windows 归 not_found、在 Unix 归 runtime_error）。
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), "read_file", "要指路到读文件的工具")
}
