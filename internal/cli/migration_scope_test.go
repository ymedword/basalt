package cli

// 本文件守两条**迁移不越界**的判据（C-42 / C-43）：迁移与切换**不得**改变管理认证模式，
// US-16 **不得**宣称多 Worker。两条都不是"功能没写"，而是"不许悄悄出现"——所以断言面
// 落在配置类型、进程旗标与配置文件字节上，而不是某条业务路径的返回值。
// Two "migration stays inside its lane" criteria: the switch never rewrites the admin
// auth mode, and nothing claims multi-worker support.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMigration_DoesNotChangeAdminMode 守 C-42：导入跑完，配置文件里的
// `http.admin_auth.mode` 必须还是 `local_trust`，文件本身逐字节不变。把它归一成 `token`
// （或没有凭据却写死 `token`）会让本机实例升级后**拒绝启动**，五个内置 Agent 全部停摆。
// 逆改点：迁移链路里加入"顺手把认证模式升级成 token"的写配置步骤 → 字节断言与
// 重新装载后的取值断言同时红。
func TestMigration_DoesNotChangeAdminMode(t *testing.T) {
	dir := t.TempDir()
	const body = "database:\n" +
		"  dsn: postgres://basalt@db.example.com:5432/basalt\n" +
		"http:\n" +
		"  admin_auth:\n" +
		"    mode: local_trust\n"
	path := writeProviderConfig(t, dir, body)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	fake := newFakeMigration()
	run := executeDBWith(t, dir, nil, fake, "import", "--dump", "dump-dir")
	require.NoError(t, run.err, "stderr=%s", run.stderr)
	require.Equal(t, 1, fake.importCalls, "导入必须抵达端口——否则「文件没变」什么也证明不了")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "迁移链路不得改写配置文件（C-42：不改认证模式）")

	cfg, err := LoadConfig(path, func(string) (string, bool) { return "", false })
	require.NoError(t, err)
	require.Equal(t, "local_trust", cfg.HTTP.AdminAuth.Mode,
		"迁移后管理认证模式必须仍是 local_trust（local_trust 不是「认证关闭」）")
	require.Empty(t, cfg.HTTP.AdminAuth.Token, "不得凭空补一枚管理凭据")
}

// TestScope_NoMultiWorkerClaim 守 C-43：US-16 只交付"单一运行期存储"，**不得**宣称
// 多 Worker——配置类型里没有 Worker / 并行度字段，进程没有 `--workers` 旗标，帮助文本
// 也不提它。多任务协调、租约与接管归后续拟 US。
// 逆改点：给 Config / Deps 加 `Workers int`、给 serve 注册 `--workers`，或在帮助文本里
// 写"支持多 Worker" → 三条断言各自变红。
func TestScope_NoMultiWorkerClaim(t *testing.T) {
	// ① 配置面：Config 与 Deps 的**全部可导出字段**（三层以内容纳嵌套段）。
	for _, typ := range []reflect.Type{reflect.TypeOf(Config{}), reflect.TypeOf(Deps{})} {
		for _, name := range exportedFieldNames(typ, 3) {
			lower := strings.ToLower(name)
			for _, banned := range []string{"worker", "parallel", "shard"} {
				require.NotContains(t, lower, banned,
					"%s 出现 %q 字段——US-16 不得宣称多 Worker（C-43）", typ.Name(), name)
			}
		}
	}

	// ② 命令面：`serve --workers` 必须被当成未知旗标拒绝，而不是悄悄接受。
	root := NewRootCommand(Deps{WorkDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	root.SetArgs([]string{"serve", "--workers", "4"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.ExecuteContext(context.Background())
	require.Error(t, err, "serve 不得接受 --workers")
	require.Contains(t, err.Error(), "unknown flag: --workers",
		"必须在旗标解析期就被拒绝——被吞掉后才报「端口未注入」等于已经接受了这个旗标")

	// ③ 帮助文本：serve / gateway 的用法面不得出现 Worker 字样。
	for _, name := range []string{"serve", "gateway"} {
		cmd, _, findErr := NewRootCommand(Deps{}).Find([]string{name})
		require.NoError(t, findErr)
		require.NotNil(t, cmd)
		require.NotContains(t, strings.ToLower(cmd.Long+" "+cmd.Short), "worker",
			"%s 的帮助文本不得宣称多 Worker", name)
	}
}

// exportedFieldNames 递归收集结构体的可导出字段路径（含嵌套结构体，深度受限；
// 内嵌指针 / 切片 / 映射只记字段名本身，不再下钻——判据够用且不会绕圈）。
// exportedFieldNames walks exported struct fields up to the given depth.
func exportedFieldNames(typ reflect.Type, depth int) []string {
	var out []string
	seen := map[reflect.Type]bool{}
	var walk func(current reflect.Type, prefix string, left int)
	walk = func(current reflect.Type, prefix string, left int) {
		if current.Kind() != reflect.Struct || left == 0 || seen[current] {
			return
		}
		seen[current] = true
		for i := 0; i < current.NumField(); i++ {
			field := current.Field(i)
			if field.PkgPath != "" { // 未导出
				continue
			}
			out = append(out, prefix+field.Name)
			walk(field.Type, prefix+field.Name+".", left-1)
		}
	}
	walk(typ, "", depth)
	return out
}
