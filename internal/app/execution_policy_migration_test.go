package app

// 本文件判卷存量升级（14D / D-11）：迁移行一律 `strict`（白名单为空也不放宽），启动后
// 生成两组待确认项，逐条「加例外」/「保持拒绝」都写审计，**未确认不放行**。
// 迁移脚本本身（v5 旧库含数据、空白名单）由 `TestMigrate_V5ToV6` 判；本文件判的是
// 那行落到运行期之后的后果：档位不被配置覆盖、两轴待确认项、处置与授权结论。
// Acceptance point (D11): the migrated row stays strict even when config says otherwise,
// both pending groups surface, every decision is audited, and nothing unconfirmed is
// admitted — neither at the domain face nor at the dial face.
//
// 判据纪律：解析器与拨号器都是注入的替身——不打真实 DNS、不连外网。
// Both the resolver and the dialer are injected; no real DNS or socket is used.

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
)

// migrationResolver 是确定性的地址表替身：没登记的域名按 NXDOMAIN 失败，绝不回落到
// 真实解析器（回落会让判断依赖本机 DNS）。
// migrationResolver answers from a fixed table and fails NXDOMAIN otherwise.
type migrationResolver struct{ answers map[string][]string }

// LookupIPAddr 查表返回；未登记即 `IsNotFound`。
// LookupIPAddr serves the table; an unlisted host is an NXDOMAIN.
func (r migrationResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	raw, ok := r.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	addrs := make([]net.IPAddr, 0, len(raw))
	for _, one := range raw {
		addrs = append(addrs, net.IPAddr{IP: net.ParseIP(one)})
	}
	return addrs, nil
}

// migrationDial 记录守门器**实际拨出**的地址，并以固定错误收尾——放宽 / 拒绝都只看
// 拨出与否，判据不需要真套接字。
// migrationDial records the address the guard reaches and stops before a real socket.
type migrationDial struct {
	fail error

	mu    sync.Mutex
	addrs []string
}

// DialContext 记录目标地址后返回固定错误。
// DialContext records the target then fails with the fixture error.
func (d *migrationDial) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, address)
	d.mu.Unlock()
	return nil, d.fail
}

// dialed 返回已记录的实拨地址副本。
// dialed returns a copy of the recorded addresses.
func (d *migrationDial) dialed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.addrs...)
}

// requirePendingItem 按稳定标识取一条待确认项（找不到即失败）。
// requirePendingItem fetches one pending item by its stable id.
func requirePendingItem(t *testing.T, items []core.PendingPolicyConfirmation,
	id string,
) core.PendingPolicyConfirmation {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	require.Failf(t, "待确认项缺失", "期望列出 %q，实际只有 %v", id, pendingIDs(items))
	return core.PendingPolicyConfirmation{}
}

// requireNoPendingItem 断言某标识不在待确认集合内。
// requireNoPendingItem asserts an id is absent from the pending set.
func requireNoPendingItem(t *testing.T, items []core.PendingPolicyConfirmation, id string) {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			require.Failf(t, "待确认项不该出现", "%q 仍在列表里", id)
		}
	}
}

// pendingIDs 渲染待确认集合的标识清单（失败信息可读性）。
// pendingIDs renders the ids for failure messages.
func pendingIDs(items []core.PendingPolicyConfirmation) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// requireDialRefused 断言受保护地址在拨号面被拒（没有生效例外 → `exception_missing`），
// 且**没有任何**连接被拨出。
// requireDialRefused asserts an exception_missing refusal with nothing dialled.
func requireDialRefused(t *testing.T, guard *tool.NetGuard, address string) {
	t.Helper()
	_, err := guard.DialContext(context.Background(), "tcp", address)
	var violation *core.SandboxViolationError
	require.ErrorAs(t, err, &violation, "拨号必须被拒：%s", address)
	require.Equal(t, core.DenialExceptionMissing, violation.Reason,
		"受保护地址没有生效例外 → exception_missing：%s", address)
}

// TestExecutionProfile_MigrationStrict 覆盖 D-11：迁移行（strict / system.migrate_v6）
// 不被配置覆盖，空白名单不放宽；两组待确认项各自列出；「保持拒绝」写审计且结论不变；
// 「加例外」写审计且只放行被登记的那一条；未确认的条目既留在列表里，也仍然拒绝。
// Acceptance point (D11): the migrated row wins over config, both pending groups are
// listed, every decision is audited, and only an explicit exception admits a protected
// host — an unconfirmed entry stays refused.
func TestExecutionProfile_MigrationStrict(t *testing.T) {
	ctx := context.Background()
	t.Cleanup(func() { processExceptions.Replace(nil) })

	resolver := migrationResolver{answers: map[string][]string{
		"intranet.example.com":        {"10.20.30.40"},   // RFC1918：受保护
		"legacy-intranet.example.com": {"10.20.30.41"},   // RFC1918：受保护
		"public.example.com":          {"93.184.216.34"}, // 公网
	}}
	stop := errors.New("test dial stop")
	dialer := &migrationDial{fail: stop}

	env := newProfileEnv(t, func(d *ExecutionPolicyDeps) {
		d.Resolver = resolver
		// 命令白名单里恰好有两条命中 shell 名称，一条不命中。
		d.ShellCommands = []string{"git", "bash", "cmd.exe"}
	})

	// ① 迁移行原样落库后启动：配置写着 standard（最坏情形）也不得覆盖；白名单为空同样
	// 不放宽——公网域名在 strict 下没有白名单就是拒绝（standard 会放行）。
	_, err := env.store.Seed(ctx, core.ProfileStrict, core.SystemOperationMigrateV6, profileClock())
	require.NoError(t, err)
	require.NoError(t, env.service.EnsureSeeded(ctx, string(core.ProfileStandard)))
	require.Equal(t, core.ProfileStrict, env.service.Current().Profile)
	require.Equal(t, core.SystemOperationMigrateV6, env.service.Current().OperationID)
	require.Equal(t, core.NetworkAllowlistSourceMigration, env.service.Current().Source())
	require.Equal(t, int64(1), env.service.Current().Revision)
	require.Equal(t, core.ProfileStrict, env.policy.ExecutionProfile(),
		"迁移档位必须同时落到运行期策略实例")
	err = env.policy.Check(ctx, core.Action{
		Type: core.ActionHTTPRequest, Target: "https://public.example.com/", Agent: "demo-agent",
	})
	require.True(t, core.IsSandboxViolation(err), "空白名单 + strict：公网域名不放行")

	// ② 两组待确认项：解析到受保护地址段的白名单条目（公网条目不列）、命中 shell 名称
	// 的命令条目。白名单按 v5 存量行装载进运行期策略实例。
	env.policy.SetAllowedDomains([]string{
		"intranet.example.com", "legacy-intranet.example.com", "public.example.com",
	})
	pending, err := env.service.PendingConfirmations(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 4, "两条受保护地址 + 两条 shell 名称：%v", pendingIDs(pending))

	networkItem := requirePendingItem(t, pending, "network_allowlist:intranet.example.com")
	require.Equal(t, core.PendingAxisNetworkAllowlist, networkItem.Axis)
	require.Equal(t, core.PendingReasonProtectedAddress, networkItem.Reason)
	require.Equal(t, []string{"10.20.30.40"}, networkItem.ResolvedProtected)
	legacyItem := requirePendingItem(t, pending, "network_allowlist:legacy-intranet.example.com")
	require.Equal(t, []string{"10.20.30.41"}, legacyItem.ResolvedProtected)
	requirePendingItem(t, pending, "shell_command:bash")
	requirePendingItem(t, pending, "shell_command:cmd.exe")
	requireNoPendingItem(t, pending, "network_allowlist:public.example.com")

	guard := tool.NewNetGuard(tool.NetGuardOptions{
		Resolver:    resolver,
		DialContext: dialer.DialContext,
		Exceptions: func() []core.NetworkException {
			// 与生产同一份活快照来源：策略实例装的例外集合。
			return env.policy.NetworkExceptions()
		},
	})

	// ③ 未确认不放行：受保护条目在拨号面被拒（没有例外），shell 名称在授权面被拒。
	requireDialRefused(t, guard, "intranet.example.com:443")
	requireDialRefused(t, guard, "legacy-intranet.example.com:443")
	require.Empty(t, dialer.dialed(), "被拒的条目一个连接都不许拨出")
	for _, name := range []string{"bash", "cmd.exe"} {
		shellErr := env.policy.Check(ctx, core.Action{
			Type: core.ActionShellCommand, Target: name, Agent: "demo-agent",
		})
		require.True(t, core.IsSandboxViolation(shellErr), "shell 名称两档都拒：%s", name)
	}

	// ④ 「保持拒绝」写审计、结论不变：条目从列表消失，但拨号仍旧拒绝。
	confirmOp, already, err := env.service.Confirm(ctx, networkItem.ID, profileActor())
	require.NoError(t, err)
	require.False(t, already, "首次处置不是重复提交")
	require.NotEmpty(t, confirmOp)
	row, err := env.ops.GetOperation(ctx, confirmOp)
	require.NoError(t, err)
	require.Equal(t, core.ActionExecutionPolicyConfirm, row.Action)
	require.Equal(t, core.TargetKindExecutionPolicy, row.TargetKind)
	require.Equal(t, networkItem.ID, row.TargetID)
	require.Equal(t, core.OperationSuccess, row.Status)
	require.Contains(t, string(row.DetailJSON), `"axis":"network_allowlist"`)

	pending, err = env.service.PendingConfirmations(ctx)
	require.NoError(t, err)
	requireNoPendingItem(t, pending, networkItem.ID)
	requireDialRefused(t, guard, "intranet.example.com:443")

	// ⑤ 「加例外」同样写审计，且只放行被登记的那一条：legacy 可拨，intranet 仍拒。
	logger, _ := newCapturedLogger()
	exceptions := NewNetworkExceptionService(NetworkExceptionDeps{
		Store:      storage.NewNetworkExceptionStore(env.db),
		Operations: env.ops,
		Policy:     env.policy,
		Logger:     logger,
		Now:        profileClock,
	})
	exception, exceptionOp, err := exceptions.Create(ctx, core.NetworkExceptionKindHost,
		"legacy-intranet.example.com", "存量内网系统联调", profileActor())
	require.NoError(t, err)
	require.NotEmpty(t, exceptionOp)
	exceptionRow, err := env.ops.GetOperation(ctx, exceptionOp)
	require.NoError(t, err)
	require.Equal(t, core.ActionNetworkExceptionCreate, exceptionRow.Action)
	require.Equal(t, exception.ExceptionID, exceptionRow.TargetID)
	require.Equal(t, core.OperationSuccess, exceptionRow.Status)
	require.Contains(t, string(exceptionRow.DetailJSON), "存量内网系统联调", "放行理由属审计内容")

	pending, err = env.service.PendingConfirmations(ctx)
	require.NoError(t, err)
	requireNoPendingItem(t, pending, legacyItem.ID)
	_, err = guard.DialContext(ctx, "tcp", "legacy-intranet.example.com:443")
	require.ErrorIs(t, err, stop, "有例外的受保护地址必须真的拨出")
	require.Contains(t, dialer.dialed(), "10.20.30.41:443",
		"拨的是已校验的地址，域名留给 Host / SNI")
	requireDialRefused(t, guard, "intranet.example.com:443") // 未登记例外的条目保持拒绝

	// ⑥ shell 轴：确认后条目消失，命令仍被拒；未确认的条目继续列出、同样不放行；确认与
	// 例外都**不**推进档位修订号。
	shellOp, already, err := env.service.Confirm(ctx, "shell_command:bash", profileActor())
	require.NoError(t, err)
	require.False(t, already)
	shellRow, err := env.ops.GetOperation(ctx, shellOp)
	require.NoError(t, err)
	require.Equal(t, core.ActionExecutionPolicyConfirm, shellRow.Action)
	require.Equal(t, "shell_command:bash", shellRow.TargetID)
	require.Contains(t, string(shellRow.DetailJSON), `"axis":"shell_command"`)

	pending, err = env.service.PendingConfirmations(ctx)
	require.NoError(t, err)
	requireNoPendingItem(t, pending, "shell_command:bash")
	requirePendingItem(t, pending, "shell_command:cmd.exe")
	shellErr := env.policy.Check(ctx, core.Action{
		Type: core.ActionShellCommand, Target: "cmd.exe", Agent: "demo-agent",
	})
	require.True(t, core.IsSandboxViolation(shellErr), "未确认的 shell 条目同样不放行")

	require.Equal(t, core.ProfileStrict, env.service.Current().Profile)
	require.Equal(t, int64(1), env.service.Current().Revision,
		"确认与例外都不改档位与修订号")
}
