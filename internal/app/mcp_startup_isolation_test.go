package app

// 本文件是 14D / T115「启动隔离」的判卷（设计 §2.5.1 末段；验收 E3）：
//
//	一条 MCP server 的凭据当前取不到值（env 变量缺失 / 为空、托管未录入 / 已撤销 /
//	版本非 active）→ 只跳过**它自己**，其余照常；页面经资源视图与装载规则显示未就绪。
//	实例级与数据完整性失败（主密钥不可用、定义与存储分叉）→ 仍失败关闭。
//
// 两类必须分开判：把"整个实例的托管凭据都解不开"也当作"某条没配好"跳过，启动会看似
// 成功，而所有托管条目同时静默失效——那是最坏的假成功。
// The two classes are judged separately: skipping instance-level failures would turn
// "every managed credential is unreadable" into a silently successful start.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
)

// isolatedServerNames 抽取本次装配实际放行的 server 名（按返回顺序）。
// isolatedServerNames lists the servers the assembly kept, in order.
func isolatedServerNames(servers []core.PublishedMCPServer) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Config.Name)
	}
	return names
}

// TestPublishedMCPServers_EnvCredentialMissingSkipsOnlyThatServer 覆盖 E3 的"缺一个凭据"：
// 发布时变量存在、启动时为空 → 该 server 被跳过，不返回错误；无凭据的 server 照常装配。
// 逆改点：把 Resolve 错误重新抛成装配失败 → serve 起不来，本用例红。
// An env slot that went empty after publishing skips only that server.
func TestPublishedMCPServers_EnvCredentialMissingSkipsOnlyThatServer(t *testing.T) {
	// 不入 t.Parallel：本用例改写进程环境（t.Setenv）。
	env := newMCPCredentialEnv(t)
	ctx := context.Background()
	const varName = "MCP_ISOLATE_ENV_TOKEN"

	t.Setenv(varName, "present-at-save")
	_, err := env.admin.UpsertMCPServer(ctx, resourceActor(), MCPServerRequest{
		Name:        "needs-env",
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Env:         map[string]string{varName: mcpPlaceholder(varName)},
		Credentials: []MCPCredentialRequest{mcpEnvSource(varName)},
		Create:      true,
	})
	require.NoError(t, err, "发布闸门在变量存在时必须放行")
	_, err = env.admin.UpsertMCPServer(ctx, resourceActor(), MCPServerRequest{
		Name: "plain", Transport: "stdio", Command: "allowed-mcp", Create: true,
	})
	require.NoError(t, err)

	// 事后变量被清空（空值与未设置同档）——这正是"发布过、后来环境变了"的那条路径。
	t.Setenv(varName, "")

	servers, err := publishedMCPServers(ctx, env.store, []string{"needs-env", "plain"},
		env.mcpCredentials, time.Now, env.logger)
	require.NoError(t, err, "缺一条凭据不得拖垮启动")
	require.Equal(t, []string{"plain"}, isolatedServerNames(servers),
		"只跳过取不到凭据的那条，其余照常连接")

	logs := env.logs.String()
	require.Contains(t, logs, "needs-env", "跳过必须留 WARN 点名该 server")
	require.Contains(t, logs, varName, "WARN 点名变量名，便于按提示补值")
	require.NotContains(t, logs, "present-at-save", "日志不得带任何凭据值")
}

// TestPublishedMCPServers_ManagedRevokedSkipsOnlyThatServer 覆盖 E3 的"托管已撤销"：
// 撤销即时生效，但只让该 server 未就绪，不回落进程环境、也不阻断启动。
// 逆改点：撤销后在装配期失败退出，或回落 env 继续起进程 → 本用例红。
// A revoked managed credential skips only its own server.
func TestPublishedMCPServers_ManagedRevokedSkipsOnlyThatServer(t *testing.T) {
	env := newMCPCredentialEnv(t)
	ctx := context.Background()

	_, err := env.admin.UpsertMCPServer(ctx, resourceActor(), MCPServerRequest{
		Name:        "vault",
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Env:         map[string]string{mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv)},
		Credentials: []MCPCredentialRequest{mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaOne)},
		Create:      true,
	})
	require.NoError(t, err)
	_, err = env.admin.UpsertMCPServer(ctx, resourceActor(), MCPServerRequest{
		Name: "plain", Transport: "stdio", Command: "allowed-mcp", Create: true,
	})
	require.NoError(t, err)

	env.revoke(t, "vault", mcpCredAlphaEnv)

	servers, err := publishedMCPServers(ctx, env.store, []string{"vault", "plain"},
		env.mcpCredentials, time.Now, env.logger)
	require.NoError(t, err, "撤销一条凭据不得拖垮启动")
	require.Equal(t, []string{"plain"}, isolatedServerNames(servers))
	require.Contains(t, env.logs.String(), "vault")
	require.NotContains(t, env.logs.String(), mcpCredAlphaOne)
}

// TestPublishedMCPServers_MasterKeyFailureStillFailsClosed 覆盖 E3 的边界：主密钥不可用
// 是**实例级**失败，不属于"某条没配好"。若把它也跳过，启动会看似成功，而所有托管条目
// 同时失效——必须拒绝启动并点名。
// 逆改点：把 LoadMasterKey 的错误也套上"该 server 凭据不可用"标记 → 本用例红。
// An unreadable master key is an instance-level failure and must still abort the start.
func TestPublishedMCPServers_MasterKeyFailureStillFailsClosed(t *testing.T) {
	env := newMCPCredentialEnv(t)
	ctx := context.Background()

	_, err := env.admin.UpsertMCPServer(ctx, resourceActor(), MCPServerRequest{
		Name:        "vault",
		Transport:   "stdio",
		Command:     "allowed-mcp",
		Env:         map[string]string{mcpCredAlphaEnv: mcpPlaceholder(mcpCredAlphaEnv)},
		Credentials: []MCPCredentialRequest{mcpManagedValue(mcpCredAlphaEnv, mcpCredAlphaOne)},
		Create:      true,
	})
	require.NoError(t, err)

	// 同一个库、同一批来源声明，只把主密钥位置换成不存在的那份：其它条件不变，
	// 失败只可能来自主密钥。
	broken, err := NewMCPCredentialService(MCPCredentialServiceDeps{
		Credentials:   storage.NewCredentialStore(env.db),
		Sources:       storage.NewMCPCredentialSourceStore(env.db),
		MasterKeyFile: filepath.Join(t.TempDir(), "missing-master.key"),
		WorkspaceRoot: env.root,
		Logger:        env.logger,
	})
	require.NoError(t, err)

	servers, err := publishedMCPServers(ctx, env.store, nil, broken, time.Now, env.logger)
	require.Error(t, err, "主密钥不可用必须拒绝启动")
	require.False(t, mcpCredentialUnavailable(err), "实例级失败不得被归类成可跳过")
	require.Empty(t, servers)
	require.Contains(t, err.Error(), "vault", "错误点名受影响的 server")

	// 同一份库用正常主密钥装配仍是通的：上面的失败确实只来自主密钥，不是数据坏了。
	healthy, err := publishedMCPServers(ctx, env.store, nil, env.mcpCredentials, time.Now, env.logger)
	require.NoError(t, err)
	require.Equal(t, []string{"vault"}, isolatedServerNames(healthy))
}

// TestPublishedMCPServers_DefinitionDivergenceStillFailsClosed 覆盖另一类不可跳过：
// 已发布版本与资源标识分叉（条目名不一致）说明存储被绕过或写坏，跳过会让 Agent 静默
// 引用一张错表。
// 逆改点：把条目名校验也降级为跳过 → 本用例红。
// A published definition that no longer matches its resource id still aborts the start.
func TestPublishedMCPServers_DefinitionDivergenceStillFailsClosed(t *testing.T) {
	env := newMCPCredentialEnv(t)
	ctx := context.Background()

	_, err := env.admin.UpsertMCPServer(ctx, resourceActor(), MCPServerRequest{
		Name: "moved", Transport: "stdio", Command: "allowed-mcp", Create: true,
	})
	require.NoError(t, err)

	// 直接写入一份条目名不同的版本并推进指针：模拟"版本与资源已分叉"的存储状态。
	// 操作号复用刚才真实保存的那一条——版本行对操作行有外键，凭空造的号会先外语键失败。
	created := env.listOperations(core.OperationFilter{Action: core.ActionMCPServerCreate})
	require.Len(t, created, 1)
	diverged := "- name: elsewhere\n  transport: stdio\n  command: allowed-mcp\n"
	_, err = env.store.PublishWithVersion(ctx, core.ResourceVersion{
		Kind:           core.ResourceMCPServer,
		ResourceID:     "moved",
		ResourceUID:    env.uid(t, "moved"),
		ContentHash:    sha256Hex(t, []byte(diverged)),
		DefinitionYAML: diverged,
		OperationID:    created[0].OperationID,
	}, created[0].OperationID, time.Now())
	require.NoError(t, err)

	servers, err := publishedMCPServers(ctx, env.store, nil, env.mcpCredentials, time.Now, env.logger)
	require.Error(t, err, "定义与存储分叉必须失败关闭")
	require.Contains(t, err.Error(), "不一致")
	require.Empty(t, servers)
}

// TestMCPCredentialUnavailable_OnlyPerServerState 把跳过依据钉在分类上：只有 env 缺值
// 与该 server 自己的托管凭据状态可跳过，实例级哨兵（主密钥、存储）与无关错误不可跳过。
// 逆改点：改成 `errors.Is(err, core.ErrCredentialUnavailable)` 一把抓 → 主密钥失败被
// 误判为可跳过，本用例红。
// The skip predicate is pinned to the per-server classes only.
func TestMCPCredentialUnavailable_OnlyPerServerState(t *testing.T) {
	t.Parallel()

	require.True(t, mcpCredentialUnavailable(mcpEnvSlotFailure{
		err: fmt.Errorf("%w: MCP server x 的槽位 TOKEN 未设置或为空", core.ErrCredentialUnavailable),
	}), "env 变量缺失属该 server 的凭据状态")
	require.True(t, mcpCredentialUnavailable(mcpCredentialStateFailure{
		err: fmt.Errorf("%w: MCP server x 的槽位 TOKEN 的托管凭据已撤销", core.ErrCredentialRevoked),
	}), "托管已撤销属该 server 的凭据状态")

	require.False(t, mcpCredentialUnavailable(fmt.Errorf(
		"%w: 主密钥文件不存在或不可读", core.ErrCredentialUnavailable)),
		"主密钥不可用是实例级失败")
	require.False(t, mcpCredentialUnavailable(errors.New("读取 MCP 槽位 TOKEN 的托管凭据: database is locked")),
		"存储读取失败是实例级失败")
	require.False(t, mcpCredentialUnavailable(nil), "没有错误就没有可跳过项")
}
