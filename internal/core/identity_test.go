package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 验收点（T103）：前缀、26 位 Crockford Base32、易混字符拒绝、大小写不敏感、
// 生成去重。身份是后续所有版本键与迁移的主键，格式错一位就会让历史失联。
// Acceptance points (T103): prefix, 26-character Crockford payload, confusable
// characters refused, case-insensitive parse and unique generation.

func TestIdentity_Format(t *testing.T) {
	t.Parallel()
	agentUID, err := NewAgentUID()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(agentUID.String(), AgentUIDPrefix))
	require.Len(t, agentUID.String(), len(AgentUIDPrefix)+uidPayloadLength)
	require.True(t, agentUID.Valid())

	resourceUID, err := NewResourceUID()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(resourceUID.String(), ResourceUIDPrefix))
	require.Len(t, resourceUID.String(), len(ResourceUIDPrefix)+uidPayloadLength)
	require.True(t, resourceUID.Valid())

	parsed, err := ParseAgentUID(strings.ToLower(agentUID.String()))
	require.NoError(t, err)
	require.Equal(t, agentUID, parsed, "小写输入必须解码为同一身份")
	require.Equal(t, agentUID.String(), agentUID.String())

	bad := []string{
		"",
		"ag_",
		"rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",  // 前缀不符
		"ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3",   // 少一位
		"ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3AA", // 多一位
		"ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3-",  // 非法字符
	}
	for _, raw := range bad {
		_, parseErr := ParseAgentUID(raw)
		require.ErrorIs(t, parseErr, ErrInvalidAgentUID, raw)
		require.False(t, AgentUID(raw).Valid(), raw)
	}
}

func TestIdentity_RejectsConfusableChars(t *testing.T) {
	t.Parallel()
	const valid = "01J8Z6YQ4M2S3T7V9W0X1Y2Z3A"
	for i, confusable := range []byte{'I', 'L', 'O', 'U'} {
		payload := []byte(valid)
		payload[i] = confusable
		_, err := ParseResourceUID(ResourceUIDPrefix + string(payload))
		require.ErrorIs(t, err, ErrInvalidResourceUID, "必须拒绝易混字符 %q", confusable)
	}
	// 小写形态同样拒绝：解码不把 i/l/o/u 映射成 1/0。
	_, err := ParseResourceUID(strings.ToLower(ResourceUIDPrefix + "I" + valid[1:]))
	require.ErrorIs(t, err, ErrInvalidResourceUID)
}

func TestIdentity_UniqueGeneration(t *testing.T) {
	t.Parallel()
	const rounds = 512
	seen := make(map[string]struct{}, rounds*2)
	for i := 0; i < rounds; i++ {
		agentUID, err := NewAgentUID()
		require.NoError(t, err)
		_, duplicate := seen[agentUID.String()]
		require.False(t, duplicate, "重复生成 Agent UID: %s", agentUID)
		seen[agentUID.String()] = struct{}{}

		resourceUID, err := NewResourceUID()
		require.NoError(t, err)
		_, duplicate = seen[resourceUID.String()]
		require.False(t, duplicate, "重复生成资源 UID: %s", resourceUID)
		seen[resourceUID.String()] = struct{}{}
	}
}
