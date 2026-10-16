package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
)

// 本文件是 `basalt tool list` 的验收面（颗粒度文档 §3.2 的 cli/tool_test.go 六条）：
// 渲染形态（逐行「名称：描述」+ 末尾空行 + 「共 N 件工具」/ 空池文案）、端口缺件的
// 报错、组命令范式（未知子命令与 --help）、以及「清单读取本身不修改状态」。
// The acceptance face of `basalt tool list`: the rendering shape, the missing-port
// failure, the group-command paradigm, and the read-only nature of the listing.
//
// 包外用 `cli_test`：判据只经**公开装配面**（`cli.Deps` + `cli.NewRootCommand`）驱动——
// 渲染函数内部长什么样不属判卷面，命令注册、cobra 派发与错误出口才是。
// The judgement drives the public assembly surface only; how the renderer is spelled
// inside the package is not part of the verdict.
//
// 两条纪律：① 端口是**假实现**（本文件内定义），零数据库、零工作区——`tool list` 的
// 装配面由 `app` 注入，命令面自己不该碰任何实现包；② 命令属**重路径**（颗粒度文档
// 注 2），但本文件驱动的是命令面本身，启动四步不在判据内（注 9）。
// The port is a fake defined here: zero database, zero workspace. The command is a
// heavy-path command, but the four startup steps belong to the assembly layer and are
// judged elsewhere.

// fakeToolLister 是 `cli.ToolLister` 的假实现：记录**读取次数**并把注入的清单原样交出。
// 交出的是同一份切片（不做拷贝）——"命令面不得改写注入的清单"因此成为可判据的事实：
// 任何就地重排 / 就地修改都会在这份切片上留下痕迹。
// A fake lister that counts reads and hands out the very slice it was given, so any
// in-place reordering by the command would leave a trace.
type fakeToolLister struct {
	specs []core.ToolSpec
	calls int
}

// Tools 返回注入的清单（假实现：不做拷贝、不加锁——判据就是要看见就地修改）。
func (f *fakeToolLister) Tools() []core.ToolSpec {
	f.calls++
	return f.specs
}

// toolRun 是一次命令运行的返回错误与三路输出（stdout / stderr / 结构化日志）。
// toolRun captures the error and the three output tracks of one run.
type toolRun struct {
	err    error
	stdout string
	stderr string
	logs   string
}

// executeTool 在注入的工作目录上跑一次 `tool` 命令组，走**真根命令**（命令注册、
// cobra 派发与退出码出口都属被测面）。其余端口留 nil：命令面这一路一次都不该碰它们。
// executeTool drives the real root command; every other port stays nil because this
// command must never touch them.
func executeTool(t *testing.T, workDir string, lister cli.ToolLister, args ...string) toolRun {
	t.Helper()

	var logs bytes.Buffer
	deps := cli.Deps{
		WorkDir:                workDir,
		SupportedSchemaVersion: 7, // 合成值：轻装配面用不到版本，给非零值以免零值把"其实用到了"掩盖过去
		Logger:                 slog.New(slog.NewTextHandler(&logs, nil)),
		Tools:                  lister,
	}

	root := cli.NewRootCommand(deps)
	root.SetArgs(args)

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(context.Background())
	return toolRun{err: err, stdout: stdout.String(), stderr: stderr.String(), logs: logs.String()}
}

// toolSpecs 造三条**非字典序**的清单：顺序判据靠它——命令面若自行重排（比如又排一次序），
// 输出顺序就会与注入顺序分道扬镳。
// Three specs deliberately out of alphabetical order: a command that reorders its input
// would diverge from the injected order here.
func toolSpecs() []core.ToolSpec {
	return []core.ToolSpec{
		{
			Name:        "shell",
			Description: "执行白名单内的可执行文件",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"executable":{"type":"string"}}}`),
		},
		{Name: "http_get", Description: "按 URL 取回内容", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "list_dir", Description: "列出目录一层内容", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
}

// cloneSpecs 深拷贝清单（判"注入的切片未被改写"的对照面）。
// cloneSpecs deep-copies the specs for the mutation verdict.
func cloneSpecs(specs []core.ToolSpec) []core.ToolSpec {
	out := make([]core.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		out = append(out, core.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Parameters:  bytes.Clone(spec.Parameters),
		})
	}
	return out
}

// dirNames 取目录条目名（工作目录"逐条不变"的判据面）。
// dirNames lists the working directory for the byte-identical verdict.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestToolList_RendersSpecsInPortOrderWithSummary：渲染形态逐字——按端口给的顺序逐行
// 「名称：描述」（命令面**零重排**：注入顺序非字典序，输出顺序与它一致）、末尾空行、
// 汇总行「共 N 件工具」（运维恢复演练判据 4 的读数来源），且**不**打印参数 schema。
func TestToolList_RendersSpecsInPortOrderWithSummary(t *testing.T) {
	lister := &fakeToolLister{specs: toolSpecs()}
	run := executeTool(t, t.TempDir(), lister, "tool", "list")

	require.NoError(t, run.err)
	require.Equal(t,
		"shell：执行白名单内的可执行文件\n"+
			"http_get：按 URL 取回内容\n"+
			"list_dir：列出目录一层内容\n"+
			"\n"+
			"共 3 件工具\n",
		run.stdout, "逐行「名称：描述」+ 末尾空行 + 汇总行——顺序由实现侧给，命令面不得重排")
	require.NotContains(t, run.stdout, `"type":"object"`, "清单只报名字与描述，不打印参数 schema")
}

// TestToolList_EmptyPoolRendersExplicitText：空池给**明确文案**——空串与"命令什么都没
// 输出"无法区分（重路径下这一支不可达，属防御分支，仍须可判）。
func TestToolList_EmptyPoolRendersExplicitText(t *testing.T) {
	run := executeTool(t, t.TempDir(), &fakeToolLister{}, "tool", "list")

	require.NoError(t, run.err)
	require.Equal(t, "未注册任何工具\n", run.stdout, "空池不得渲染成空串、也不得报成功却什么都不说")
}

// TestToolList_MissingPortFailsWithNamedCause：端口缺失**点名装配缺件**——工具清单只在
// 重路径装配下存在（颗粒度文档注 2），命令面不自行构造第二套清单，静默成功更不允许。
func TestToolList_MissingPortFailsWithNamedCause(t *testing.T) {
	run := executeTool(t, t.TempDir(), nil, "tool", "list")

	require.Error(t, run.err)
	require.Contains(t, run.err.Error(), "工具清单端口未注入", "报错要点名缺的是哪一件")
	require.Contains(t, run.err.Error(), "重路径", "并指路：该端口由重路径装配注入")
	require.Empty(t, run.stdout, "缺件时不得渲染半截清单")
}

// TestToolList_UnknownSubcommandIsRefused：组命令范式——`tool` 自身可运行（未知子命令
// 走显式分支报错，非零退出），**不得**退化成"请求帮助"式的 0 退出。
func TestToolList_UnknownSubcommandIsRefused(t *testing.T) {
	run := executeTool(t, t.TempDir(), &fakeToolLister{}, "tool", "bogus")

	require.Error(t, run.err, "未知子命令必须报错（可运行的父命令 + 显式分支）")
	require.Contains(t, run.err.Error(), `未知命令 "bogus"`)
	require.Contains(t, run.err.Error(), "--help", "报错要给出下一步")
}

// TestToolList_HelpIsAvailable：`tool --help` 渲染出子命令清单（组命令的可用性判据）。
func TestToolList_HelpIsAvailable(t *testing.T) {
	run := executeTool(t, t.TempDir(), &fakeToolLister{}, "tool", "--help")

	require.NoError(t, run.err)
	require.Contains(t, run.stdout, "list", "帮助里要看得见 list 子命令")
	require.Contains(t, run.stdout, "tool", "帮助要说明这是 tool 命令组")
}

// TestToolList_ReadDoesNotMutateState：**清单读取本身不修改状态**（颗粒度文档注 9 的
// 准确表述）——读取恰好一次、注入的清单未被就地改写、工作目录逐条不变。命令属重路径
// （启动四步含日志）不属本判据：那是装配层的既有成本，由 app 侧判卷。
func TestToolList_ReadDoesNotMutateState(t *testing.T) {
	dir := t.TempDir()
	specs := toolSpecs()
	before := cloneSpecs(specs)
	lister := &fakeToolLister{specs: specs}

	entriesBefore := dirNames(t, dir)
	run := executeTool(t, dir, lister, "tool", "list")
	entriesAfter := dirNames(t, dir)

	require.NoError(t, run.err)
	require.Equal(t, 1, lister.calls, "清单读取恰好一次——答案的唯一来源就是这个端口")
	require.Equal(t, before, lister.specs, "命令面不得就地改写注入的清单（零重排、零修改）")
	require.Equal(t, entriesBefore, entriesAfter, "工作目录逐条不变——清单读取本身不写任何东西")
}
