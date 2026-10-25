package app

// 本文件是颗粒度文档 §4.3 与约束 C-04 的验收面：MCP server 只读依赖诊断回答"当前这条
// **已发布**配置按真实启动方式能否定位"（located / not_found / unconfirmed × reason）、
// "env 键是否就绪"（只回布尔）、"是否待重启"（与连接无关），并证明诊断本身**不启动
// 进程、不连接、不写操作行、不改 published / applied**。
// These cases pin the read-only dependency diagnosis: the three-state command verdict,
// boolean env presence, pending-restart independence, and the read-only invariants.
//
// 说明：卡片里的 `env.get(...)` 形态是该判据的 HTTP 面（`internal/web` 的
// `TestMCPDiagnostics_DoesNotStartProcess`，T017 落地）；本文件在 app 层用同一批断言
// 直接压 `DiagnoseMCPServer`，并用哨兵文件证明"没有子进程被启动"。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

const (
	// diagSentinelArgPrefix 是"诊断若真的启动了子进程"的哨兵参数前缀：子进程由包级
	// TestMain 拦截——写下哨兵文件后立即退出，于是"文件出现"就等于"诊断启动了进程"。
	// The sentinel flag prefix intercepted by TestMain; the file's appearance proves a
	// child process was started.
	diagSentinelArgPrefix = "-basalt-diag-sentinel="
	// diagSecondEnv 是 env 布尔用例的第二个键：它已就绪，另一个键在保存后被清空——
	// 这样"只回布尔、按 key 升序"才有判别力。
	diagSecondEnv = "MCP_DIAG_READY"
)

// saveDiagnosedMCPEntry 经真实写路径保存并发布一条 stdio 条目（新建即发布，applied
// 留空）。条目自带一个已就绪的 `${MCP_TEST_TOKEN}` 占位，保证保存期的 env 闸门通过。
// saveDiagnosedMCPEntry publishes one stdio entry through the real write path; the
// ready placeholder keeps the save-time env gate satisfied.
func saveDiagnosedMCPEntry(t *testing.T, env *resourceEnv, name, command string, args []string,
	extraEnv map[string]string,
) core.ResourceVersion {
	t.Helper()
	values := map[string]string{mcpTestTokenEnv: "${" + mcpTestTokenEnv + "}"}
	for key, value := range extraEnv {
		values[key] = value
	}
	version, err := env.admin.UpsertMCPServer(context.Background(), resourceActor(), MCPServerRequest{
		Name:      name,
		Transport: "stdio",
		Command:   command,
		Args:      args,
		Env:       values,
		Create:    true,
	})
	require.NoError(t, err)
	return version
}

// diagnoseMCP 调一次诊断并断言"零操作行"：调用前后的操作行数必须一致。
// diagnoseMCP runs one diagnosis and asserts the operation-row count is unchanged.
func diagnoseMCP(t *testing.T, env *resourceEnv, name string) core.MCPServerDiagnosis {
	t.Helper()
	before := env.operationCount()
	diagnosis, err := env.admin.DiagnoseMCPServer(context.Background(), resourceActor(), name)
	require.NoError(t, err)
	require.Equal(t, before, env.operationCount(), "诊断不得写 management_operations")
	return diagnosis
}

// requireMCPPointerUnchanged 断言发布指针（published / applied / 身份）逐字段不变。
// requireMCPPointerUnchanged asserts the whole publish pointer is untouched.
func requireMCPPointerUnchanged(t *testing.T, env *resourceEnv, name string, before core.ResourcePointer) {
	t.Helper()
	require.Equal(t, before, env.pointerOf(core.ResourceMCPServer, name), "诊断不得改变发布指针")
}

// requireMCPDiagSentinelAbsent 在一个短宽限窗口内反复确认哨兵文件始终不出现：若实现
// 真的 `Start` 了子进程，子进程写文件的时序不受本进程控制，单次即时检查会漏判。
// requireMCPDiagSentinelAbsent polls for a short grace window, so a spawned child's
// asynchronous write cannot slip past a single instant check.
func requireMCPDiagSentinelAbsent(t *testing.T, path string, grace time.Duration) {
	t.Helper()
	deadline := time.Now().Add(grace)
	for {
		if _, err := os.Stat(path); err == nil {
			t.Fatal("诊断不得启动子进程：哨兵文件出现")
		} else if !os.IsNotExist(err) {
			t.Fatalf("读取哨兵状态失败: %v", err)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMCPDiagnostics_NotConfigured：未配置 / 已删除的 server 按 404 语义拒绝，且失败
// 的诊断也不得留下操作行。
// 逆改点：诊断里预写操作行 / 把未知名字答成空诊断 → 红。
func TestMCPDiagnostics_NotConfigured(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	before := env.operationCount()
	_, err := env.admin.DiagnoseMCPServer(context.Background(), resourceActor(), "ghost")
	require.ErrorIs(t, err, core.ErrResourceNotFound, "未配置的 server 必须按 404 语义拒绝")
	require.Equal(t, before, env.operationCount(), "失败的诊断也不得写操作行")
}

// TestMCPDiagnostics_LocatedLookPath：裸命令名经 PATH 定位成功 → located / lookpath，
// 且 Path 是解析后的绝对路径。
// 逆改点：不调构造器、自己拼路径猜 → Path / reason 对不上；把裸名答成 absolute → 红。
func TestMCPDiagnostics_LocatedLookPath(t *testing.T) {
	// 改进程环境（PATH），不能并行。
	env := newResourceEnv(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	exe, err = filepath.Abs(exe)
	require.NoError(t, err)

	// PATH 前置测试二进制所在目录，命令用**不带扩展名**的裸名：Windows 由 PATHEXT
	// 补 `.EXE`，Unix 靠执行位——两条平台解析路径都被覆盖。
	name := filepath.Base(exe)
	if runtime.GOOS == "windows" {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	t.Setenv("PATH", filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))

	saveDiagnosedMCPEntry(t, env, "demo", name, nil, nil)
	before := env.pointerOf(core.ResourceMCPServer, "demo")

	diagnosis := diagnoseMCP(t, env, "demo")
	require.NotNil(t, diagnosis.Command, "stdio 诊断必须给出命令三态")
	require.Equal(t, "located", diagnosis.Command.State)
	require.Equal(t, "lookpath", diagnosis.Command.Reason)
	require.Equal(t, exe, diagnosis.Command.Path, "located 必须给出解析后的绝对路径")
	requireMCPPointerUnchanged(t, env, "demo", before)
}

// TestMCPDiagnostics_LocatedAbsolute：绝对路径存在且可启动 → located / absolute。
// 逆改点：绝对路径也按 PATH 解析、把 reason 写成 lookpath → 红。
func TestMCPDiagnostics_LocatedAbsolute(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	exe, err = filepath.Abs(exe)
	require.NoError(t, err)

	saveDiagnosedMCPEntry(t, env, "demo", exe, nil, nil)
	before := env.pointerOf(core.ResourceMCPServer, "demo")

	diagnosis := diagnoseMCP(t, env, "demo")
	require.NotNil(t, diagnosis.Command)
	require.Equal(t, "located", diagnosis.Command.State)
	require.Equal(t, "absolute", diagnosis.Command.Reason)
	require.Equal(t, exe, diagnosis.Command.Path)
	requireMCPPointerUnchanged(t, env, "demo", before)
}

// TestMCPDiagnostics_NotFound：不存在只由两条证据支持——`exec.ErrNotFound`（裸名的
// PATH 搜索失败）与 `os.Stat` 明确返回不存在（绝对路径）。其余无法证明的情形不得
// 落 not_found（见 `_Permission` 与 C-04）。
// 逆改点：把一切解析失败都答成 not_found → 本用例仍绿但 `_Permission` / `_NotFound`
// 的绝对路径分支会互相矛盾；把绝对路径缺失答成 unconfirmed → 红。
func TestMCPDiagnostics_NotFound(t *testing.T) {
	t.Parallel()

	t.Run("absolute", func(t *testing.T) {
		env := newResourceEnv(t)
		missing := filepath.Join(t.TempDir(), "missing-mcp-server.exe")
		saveDiagnosedMCPEntry(t, env, "demo", missing, nil, nil)

		diagnosis := diagnoseMCP(t, env, "demo")
		require.Equal(t, "not_found", diagnosis.Command.State)
		require.Equal(t, "not_found", diagnosis.Command.Reason)
		require.Empty(t, diagnosis.Command.Path, "not_found 不得给出路径")
	})

	t.Run("lookpath", func(t *testing.T) {
		env := newResourceEnv(t)
		saveDiagnosedMCPEntry(t, env, "demo", "basalt-missing-mcp-command", nil, nil)

		diagnosis := diagnoseMCP(t, env, "demo")
		require.Equal(t, "not_found", diagnosis.Command.State)
		require.Equal(t, "not_found", diagnosis.Command.Reason)
		require.Empty(t, diagnosis.Command.Path)
	})
}

// TestMCPDiagnostics_Permission：存在但不可能按现行方式启动 → unconfirmed / permission。
// 判据落在 `os.Stat`（目录不是可执行文件）与 Unix 执行位上：Windows 的 LookPath 会把
// 目录折叠成 ErrNotExist，所以"不存在"不能只信构造器（§2.5.12 / C-04）。
// 逆改点：直接拿 `cmd.Err` 分类（Windows 上答成 not_found）→ 红；把 permission 写成
// unknown → 红。
func TestMCPDiagnostics_Permission(t *testing.T) {
	t.Parallel()

	t.Run("directory", func(t *testing.T) {
		env := newResourceEnv(t)
		blocked := filepath.Join(t.TempDir(), "blocked-command")
		require.NoError(t, os.Mkdir(blocked, 0o755))
		saveDiagnosedMCPEntry(t, env, "demo", blocked, nil, nil)

		diagnosis := diagnoseMCP(t, env, "demo")
		require.Equal(t, "unconfirmed", diagnosis.Command.State, "无法证明可启动一律 unconfirmed")
		require.Equal(t, "permission", diagnosis.Command.Reason)
		require.Empty(t, diagnosis.Command.Path)
	})

	if runtime.GOOS != "windows" {
		t.Run("no-exec-bit", func(t *testing.T) {
			env := newResourceEnv(t)
			plain := filepath.Join(t.TempDir(), "plain-mcp-tool")
			require.NoError(t, os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o600))
			saveDiagnosedMCPEntry(t, env, "demo", plain, nil, nil)

			diagnosis := diagnoseMCP(t, env, "demo")
			require.Equal(t, "unconfirmed", diagnosis.Command.State)
			require.Equal(t, "permission", diagnosis.Command.Reason)
		})
	}
}

// TestMCPDiagnostics_CmdShimUnconfirmed：`.cmd` / `.bat` 是脚本包装，不能当可执行文件
// 直接启动，**不**默认 `cmd.exe /c`（§2.5.12）；后缀规则与平台无关，Linux 上同样是
// unconfirmed / script。
// 逆改点：把 `.cmd` 当 located、或改成 `cmd.exe /c` 拼命令行 → 红。
func TestMCPDiagnostics_CmdShimUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{".cmd", ".bat"} {
		t.Run(suffix, func(t *testing.T) {
			t.Parallel()
			env := newResourceEnv(t)
			shim := filepath.Join(t.TempDir(), "mcp-shim"+suffix)
			require.NoError(t, os.WriteFile(shim, []byte("@echo off\r\n"), 0o755))
			saveDiagnosedMCPEntry(t, env, "demo", shim, nil, nil)

			diagnosis := diagnoseMCP(t, env, "demo")
			require.Equal(t, "unconfirmed", diagnosis.Command.State)
			require.Equal(t, "script", diagnosis.Command.Reason)
			require.Empty(t, diagnosis.Command.Path)
		})
	}
}

// TestMCPDiagnostics_RelativePathUnconfirmed：含分隔符的相对入口按 `cmd.Dir` 解析，
// 不启动就无法证明 → unconfirmed / relative_path（正 / 反斜杠两种形态同判）。
// 逆改点：relativize 后 stat、或当成本地文件 located → 红。
func TestMCPDiagnostics_RelativePathUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, command string }{
		{"forward-slash", "./workspace/mcp-tool"},
		{"backslash", `workspace\tool.exe`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := newResourceEnv(t)
			saveDiagnosedMCPEntry(t, env, "demo", tc.command, nil, nil)

			diagnosis := diagnoseMCP(t, env, "demo")
			require.Equal(t, "unconfirmed", diagnosis.Command.State)
			require.Equal(t, "relative_path", diagnosis.Command.Reason)
			require.Empty(t, diagnosis.Command.Path)
		})
	}
}

// TestMCPDiagnostics_EnvSetBoolean：env 诊断按 key 升序、只回布尔（存在且非空为 true），
// 且回答的是**当前进程环境**——保存成功后清空变量，诊断立刻变成 false（"发布成功 ≠
// 当前环境满足"）。
// 逆改点：回显值、返回整个 map、忽略空串、不排序 → 对应断言红。
func TestMCPDiagnostics_EnvSetBoolean(t *testing.T) {
	// 改进程环境（t.Setenv），不能并行。
	env := newResourceEnv(t)
	t.Setenv(diagSecondEnv, mcpEnvSecretSentinel)
	saveDiagnosedMCPEntry(t, env, "demo", "allowed-mcp", nil, map[string]string{
		diagSecondEnv: "${" + diagSecondEnv + "}",
	})
	t.Setenv(mcpTestTokenEnv, "") // 保存之后才清空：诊断看的是当下

	diagnosis := diagnoseMCP(t, env, "demo")
	require.Equal(t, []core.MCPEnvDiagnosis{
		{Key: diagSecondEnv, Set: true},
		{Key: mcpTestTokenEnv, Set: false},
	}, diagnosis.Env, "env 诊断按 key 升序、空值记为未就绪")

	raw, err := json.Marshal(diagnosis)
	require.NoError(t, err)
	require.NotContains(t, string(raw), mcpEnvSecretSentinel, "诊断响应不得回显 env 值")

	var payload struct {
		Env []map[string]any `json:"env"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Len(t, payload.Env, 2)
	for _, entry := range payload.Env {
		require.Len(t, entry, 2, "env 诊断元素只允许 key / set 两个字段")
		require.Contains(t, entry, "key")
		require.Contains(t, entry, "set")
	}
}

// TestMCPDiagnostics_PendingRestartIndependent：待重启只由 published / applied 决定——
// 发布未生效为 true，applied 追平后为 false；诊断本身不改这两个指针。
// 逆改点：用连接状态推待重启、诊断里顺手推进 applied → 红。
func TestMCPDiagnostics_PendingRestartIndependent(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	version := saveDiagnosedMCPEntry(t, env, "demo", "allowed-mcp", nil, nil)

	first := diagnoseMCP(t, env, "demo")
	require.True(t, first.PendingRestart, "发布未生效 → 待重启")
	require.Equal(t, version.ID, first.PublishedVersionID)
	require.Zero(t, first.AppliedVersionID)
	require.Equal(t, "stdio", first.Transport)
	require.Equal(t, baselineClock(), first.CheckedAt, "检查时间取统一时钟")

	env.setApplied(t, core.ResourceMCPServer, "demo", version.ID)
	before := env.pointerOf(core.ResourceMCPServer, "demo")

	second := diagnoseMCP(t, env, "demo")
	require.False(t, second.PendingRestart, "applied 追平 published → 不再待重启")
	require.Equal(t, version.ID, second.AppliedVersionID)
	requireMCPPointerUnchanged(t, env, "demo", before)
	require.Len(t, env.versionsOfKind(core.ResourceMCPServer, "demo"), 1, "诊断不得追加版本")
}

// TestMCPDiagnostics_NoProcessNoOperation：诊断不得启动子进程。命令指向测试二进制自身
// 并带哨兵参数——一旦被启动，子进程 TestMain 会写下哨兵文件；文件始终不出现即证明
// "只检查、不启动"。同时断言零操作行、指针 / 版本不变。
// 逆改点：diagnose 里 `Start` / `Connect` / 预写操作行 → 红。
func TestMCPDiagnostics_NoProcessNoOperation(t *testing.T) {
	t.Parallel()
	env := newResourceEnv(t)
	exe, err := os.Executable()
	require.NoError(t, err)
	exe, err = filepath.Abs(exe)
	require.NoError(t, err)
	sentinel := filepath.Join(t.TempDir(), "started")
	saveDiagnosedMCPEntry(t, env, "demo", exe, []string{diagSentinelArgPrefix + sentinel}, nil)
	before := env.pointerOf(core.ResourceMCPServer, "demo")

	diagnosis := diagnoseMCP(t, env, "demo")
	require.NotNil(t, diagnosis.Command)
	require.Equal(t, "located", diagnosis.Command.State)
	require.Equal(t, "absolute", diagnosis.Command.Reason)

	requireMCPDiagSentinelAbsent(t, sentinel, 300*time.Millisecond)
	requireMCPPointerUnchanged(t, env, "demo", before)
	require.Len(t, env.versionsOfKind(core.ResourceMCPServer, "demo"), 1, "诊断不得追加版本")
}
