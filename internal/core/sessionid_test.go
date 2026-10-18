package core_test

// 本文件判卷会话标识派生（约束 12）：固定向量、元组编码的边界消歧、路径 / 日志
// 安全与恶意 channel 的前缀回落。
// Session id derivation (constraint 12): pinned vectors, length-prefixed tuple
// disambiguation, path/log safety and hostile-channel prefix fallback.

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// safeSessionIDPattern 是会话标识的合法形态：安全前缀（≤32 字符）+ `-` + 16 位
// 小写十六进制。任何 `/`、`%`、空白或控制字符都不允许出现。
var safeSessionIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}-[0-9a-f]{16}$`)

// TestDeriveSessionID_PinnedVectors 钉死两条既有向量：CLI（渠道 `cli`）与 REST
// （渠道 `web`）对固定元组必须给出逐字稳定的标识——迁移成唯一实现后不得变化。
// Pinned vectors: both the pre-existing CLI id and the REST id are frozen.
func TestDeriveSessionID_PinnedVectors(t *testing.T) {
	require.Equal(t, "cli-a2025c15b5cbc303", core.DeriveSessionID("cli", "yunwei", "notes-agent"))
	require.Equal(t, "web-bb27c35eeb3cc34a", core.DeriveSessionID("web", "anonymous", "ops-agent"))
}

// TestDeriveSessionID_AdjacentTuplesDoNotCollide 验证长度前缀的作用：直接拼接时
// `a-b|c` 与 `a|b-c` 字节相同，只有把长度编进元组才会落到不同摘要上。
// The length prefix is what keeps adjacent tuple splits from colliding.
func TestDeriveSessionID_AdjacentTuplesDoNotCollide(t *testing.T) {
	require.NotEqual(t, core.DeriveSessionID("cli", "a-b", "c"), core.DeriveSessionID("cli", "a", "b-c"))
	require.NotEqual(t, core.DeriveSessionID("cli", "ab", "c"), core.DeriveSessionID("cli", "a", "bc"))
	require.NotEqual(t, core.DeriveSessionID("cli", "", "abc"), core.DeriveSessionID("cli", "abc", ""))
	require.NotEqual(t, core.DeriveSessionID("cli", "a", "bc"), core.DeriveSessionID("web", "a", "bc"))
}

// TestDeriveSessionID_ResultIsPathAndLogSafe 验证用户输入（含路径分隔符 / 控制
// 字符）只会进入摘要，标识本身始终是「安全前缀-十六进制」，可当文件名与日志字段。
// Arbitrary user input is folded into the digest; the id itself stays safe.
func TestDeriveSessionID_ResultIsPathAndLogSafe(t *testing.T) {
	ids := []string{
		core.DeriveSessionID("web", "../../etc/passwd", "a/b"),
		core.DeriveSessionID("web", "user\x00name", "pro\tfile"),
		core.DeriveSessionID("web", "用户/名字", "代理"),
	}
	for _, id := range ids {
		require.Regexp(t, safeSessionIDPattern, id)
		require.NotContains(t, id, "/")
		require.NotContains(t, id, `\`)
		require.NotContains(t, id, "%")
		require.NotContains(t, id, " ")
		require.Equal(t, strings.ToLower(id), id)
	}
}

// TestDeriveSessionID_EmptyPartsAreEncoded 验证空 user / 空 agent 仍产出合法标识，
// 且与其它组合不碰撞（空组件同样带长度前缀 `0:`）。
// Empty components are encoded with their zero length, not omitted.
func TestDeriveSessionID_EmptyPartsAreEncoded(t *testing.T) {
	emptyUser := core.DeriveSessionID("web", "", "demo")
	emptyAgent := core.DeriveSessionID("web", "anonymous", "")
	bothEmpty := core.DeriveSessionID("web", "", "")

	for _, id := range []string{emptyUser, emptyAgent, bothEmpty} {
		require.Regexp(t, safeSessionIDPattern, id)
	}
	require.NotEqual(t, emptyUser, emptyAgent)
	require.NotEqual(t, emptyUser, bothEmpty)
	require.NotEqual(t, emptyAgent, bothEmpty)
	require.NotEqual(t, emptyUser, core.DeriveSessionID("web", "anonymous", "demo"))
}

// TestDeriveSessionID_HostileChannelFallsBackToSafePrefix 验证 REST 请求输入的
// channel 不可能把路径分隔符 / 空白 / 控制字符带进标识：不匹配
// `^[a-z0-9][a-z0-9-]{0,31}$` 时前缀固定回落 `ch`；合法前缀原样保留。
// Hostile channels fall back to the fixed "ch" prefix; legal ones stay as-is.
func TestDeriveSessionID_HostileChannelFallsBackToSafePrefix(t *testing.T) {
	hostile := []string{
		"../etc",
		"web x",
		"web%2f",
		"WEB",
		"-web",
		strings.Repeat("a", 33),
		"web\x1f",
	}
	seen := make(map[string]bool, len(hostile))
	for _, channel := range hostile {
		id := core.DeriveSessionID(channel, "anonymous", "ops-agent")
		require.Regexp(t, safeSessionIDPattern, id)
		require.True(t, strings.HasPrefix(id, "ch-"), "channel %q should fall back to ch-", channel)
		require.False(t, seen[id], "hostile channel %q collided with a previous id", channel)
		seen[id] = true
	}

	// 合法前缀原样保留（含内部连字符与数字）。
	require.True(t, strings.HasPrefix(core.DeriveSessionID("web", "u", "p"), "web-"))
	require.True(t, strings.HasPrefix(core.DeriveSessionID("cli-a1", "u", "p"), "cli-a1-"))
	// 32 字符的 channel 仍算合法（边界值）。
	require.True(t, strings.HasPrefix(core.DeriveSessionID(strings.Repeat("a", 32), "u", "p"), strings.Repeat("a", 32)+"-"))
}
