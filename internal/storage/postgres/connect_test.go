package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 本文件是启动校验的**无库**用例：目标版本、连接串规范化、不可达服务端与扩展声明。
// 需要真实 PostgreSQL 的状态用例在 connect_integration_test.go（`integration` 构建标签）。
// The no-database half of the startup gate: version target, DSN normalization,
// unreachable server and the shipped extension declaration.

// TestStartupVerification_RejectsNonPositiveTarget 判目标版本从 1 起：0 / 负数都是
// 编程错误，必须在**发起任何查询之前**拒绝（nil Queryer 证明没碰库）。
// 逆改点：删掉 `target < 1` 分支 → 本用例对 nil Queryer 触发空指针而红。
func TestStartupVerification_RejectsNonPositiveTarget(t *testing.T) {
	t.Parallel()
	for _, target := range []int{0, -1} {
		err := verifySchemaVersion(context.Background(), nil, target)
		require.Error(t, err, "目标版本 %d 必须被拒绝", target)
		require.Contains(t, err.Error(), "目标版本")
	}
}

// TestStartupVerification_RefusesEmptyDSN 判空连接串（含未解析的 ${...} 占位残留）在
// 建池前即拒绝，且错误文案不回显输入。
// 逆改点：normalizeDSN 去掉空值分支 → Open 会把空串交给 pgx，错误形状与本断言不符。
func TestStartupVerification_RefusesEmptyDSN(t *testing.T) {
	t.Parallel()
	for _, dsn := range []string{"", "   ", "${BASALT_PG_DSN}"} {
		_, err := Open(context.Background(), Config{DSN: dsn})
		require.Error(t, err, "连接串 %q 必须被拒绝", dsn)
	}
}

// TestStartupVerification_RefusesUnreachableServer 判连接失败即拒绝启动，并且错误链里
// **不得**出现口令明文（凭证不回显是硬性纪律，连接串最容易在错误文案里泄漏）。
// 回环上没人监听的端口：不出网、不需要数据库。
// 逆改点：把 Ping 失败吞掉继续返回句柄 → 本用例拿到 nil error 而红。
func TestStartupVerification_RefusesUnreachableServer(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-should-never-appear"
	dsn := "postgres://basalt_test:" + secret + "@127.0.0.1:1/basalt_test?sslmode=disable"
	_, err := Open(context.Background(), Config{DSN: dsn, ConnectTimeout: 500 * time.Millisecond})
	require.Error(t, err, "连不上服务端必须拒绝启动")
	require.Contains(t, err.Error(), "连接 PostgreSQL 失败")
	require.NotContains(t, err.Error(), secret, "错误文案不得回显口令")
}

// TestStartupVerification_RequiredExtensionsDeclaration 钉住本 US 冻结的扩展前置：
// `vector` 必带 `hnsw_iterative_scan` 能力、`pg_trgm` 只要求安装；每个声明的能力都必须
// 有探针，否则启动会按"无法验证"拒绝——声明与探针表漂移在这里先红。
// 逆改点：把 vector 的能力名改成未登记的名字 → 探针缺失断言红；删掉 pg_trgm → 数量断言红。
func TestStartupVerification_RequiredExtensionsDeclaration(t *testing.T) {
	t.Parallel()
	require.Len(t, requiredExtensions, 2, "扩展前置只有 vector 与 pg_trgm 两条")
	byName := map[string]ExtensionRequirement{}
	for _, req := range requiredExtensions {
		byName[req.Name] = req
	}
	require.Equal(t, "hnsw_iterative_scan", byName["vector"].MinCapability)
	require.Empty(t, byName["pg_trgm"].MinCapability)
	buildProbe, ok := capabilityProbes[byName["vector"].MinCapability]
	require.True(t, ok, "声明的能力必须有探针，否则启动按无法验证拒绝")
	probe := buildProbe("public")
	require.Contains(t, probe, "current_setting('hnsw.iterative_scan', true)",
		"探针必须读的就是它声称验证的设置")
	require.Contains(t, probe, ".vector", "类型名必须限定到扩展 schema")
	require.NotContains(t, probe, "::vector",
		"未限定的类型名按会话 search_path 解析，隔离 schema 下会 42704")
}

// TestStartupVerification_SessionTimeoutsPerConnection 判语句 / 锁等待超时按**逐连接**
// 会话参数下发（设计 §3.3：「逐连接设置」）：值进 RuntimeParams，随每条新连接的启动包
// 生效；毫秒化不能丢精度，配置值优先于连接串里的同名字段。
// 逆改点：删掉 parseConnConfig 里的两个 RuntimeParams 写入 → 前两条子用例红；改成
// 连接串优先 → "config wins" 子用例红。
func TestStartupVerification_SessionTimeoutsPerConnection(t *testing.T) {
	t.Parallel()

	t.Run("config", func(t *testing.T) {
		t.Parallel()
		connCfg, err := parseConnConfig(Config{
			DSN:              "postgres://u:p@h:5432/db?sslmode=disable",
			StatementTimeout: 30 * time.Second,
			LockTimeout:      5 * time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "30000", connCfg.RuntimeParams["statement_timeout"])
		require.Equal(t, "5000", connCfg.RuntimeParams["lock_timeout"])
	})

	t.Run("config wins over dsn", func(t *testing.T) {
		t.Parallel()
		connCfg, err := parseConnConfig(Config{
			DSN:              "postgres://u:p@h:5432/db?sslmode=disable&statement_timeout=1000",
			StatementTimeout: 30 * time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "30000", connCfg.RuntimeParams["statement_timeout"],
			"配置值优先，结果不取决于 DSN 里写了什么")
	})

	t.Run("dsn value kept when config unset", func(t *testing.T) {
		t.Parallel()
		connCfg, err := parseConnConfig(Config{DSN: "postgres://u:p@h:5432/db?sslmode=disable&statement_timeout=1000"})
		require.NoError(t, err)
		require.Equal(t, "1000", connCfg.RuntimeParams["statement_timeout"],
			"配置未设时不抹掉连接串里的显式设置")
		require.NotContains(t, connCfg.RuntimeParams, "lock_timeout")
	})
}

// TestStartupVerification_NormalizeDSN 判两种合法写法的规范化：两种都带上
// `TimeZone=UTC` 与 `target_session_attrs=read-write`，原有参数不丢，同名参数（含大小写
// 变体）只留一份——重复的 runtime 参数会按到达顺序生效，不能留两份。
// 逆改点：删掉 case-insensitive 去重 → 用例里带 `timezone=Asia/Shanghai` 的输入会同时
// 出现两份时区设置，断言红。
func TestStartupVerification_NormalizeDSN(t *testing.T) {
	t.Parallel()

	t.Run("url", func(t *testing.T) {
		t.Parallel()
		got, err := normalizeDSN("postgres://u:p@h:5432/db?sslmode=disable&timezone=Asia/Shanghai&target_session_attrs=any")
		require.NoError(t, err)
		require.Contains(t, got, "sslmode=disable")
		require.Contains(t, got, "TimeZone=UTC")
		require.Contains(t, got, "target_session_attrs=read-write")
		require.NotContains(t, got, "Asia/Shanghai", "同名参数先删再写，不留第二份")
		require.NotContains(t, got, "target_session_attrs=any")
	})

	t.Run("keyword", func(t *testing.T) {
		t.Parallel()
		got, err := normalizeDSN("host=h port=5432 user=u dbname=d sslmode=disable")
		require.NoError(t, err)
		require.Contains(t, got, "TimeZone=UTC")
		require.Contains(t, got, "target_session_attrs=read-write")
	})

	t.Run("unsupported scheme", func(t *testing.T) {
		t.Parallel()
		_, err := normalizeDSN("mysql://h/db")
		require.Error(t, err)
	})
}
