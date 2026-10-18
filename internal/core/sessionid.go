package core

// 本文件是会话标识派生的**唯一**实现（约束 12）：CLI 与 REST 必须对同一
// (channel, user, agent) 元组给出同一个 id，否则同一段对话会分裂成两套历史。
// The single implementation of session-id derivation (constraint 12): CLI and
// REST must agree on one id per (channel, user, agent) tuple.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

const (
	// sessionIDDomain 是派生哈希的域前缀（固定口径逐字）。
	sessionIDDomain = "basalt/session/v1"
	// sessionIDSep 是规范元组编码的字段分隔字节（0x1F，域内不会自然出现的控制字符）。
	sessionIDSep = 0x1F
	// sessionIDHashHexLen 是取十六进制**前 16 位**（固定口径逐字）。
	sessionIDHashHexLen = 16
	// sessionIDFallbackPrefix 是 channel 不可安全用作前缀时的固定回落。
	sessionIDFallbackPrefix = "ch"
)

// DeriveSessionID 按规范元组编码 + 稳定哈希派生会话标识（逐字自 cli/chat.go 迁移）：
// `sha256("basalt/session/v1" ‖ 0x1F ‖ <len(channel)>:channel ‖ 0x1F ‖ <len(user)>:user ‖
// 0x1F ‖ <len(profile)>:profile)` 取十六进制前 16 位，前缀渠道名。
// 前缀规则（约束 12）：channel 匹配 ^[a-z0-9][a-z0-9-]{0,31}$ 时原样作前缀，
// 否则固定 "ch"——REST 的 channel 是请求输入，不得让路径分隔符 / 控制字符进入标识。
// 身份由含 channel 的摘要承载，回落不改变不同渠道的 id 唯一性。
//
// 长度前缀是这一步的全部要点：`user=a-b, profile=c` 与 `user=a, profile=b-c` 直接
// 拼接出的字节完全一样，只有把每段的长度也编进元组，两种组合才会落到不同摘要上。
// The length prefix is the point: different tuple splits concatenate to the same
// bytes otherwise. The digest folds arbitrary user input into fixed-length hex,
// so the id is always safe to use as a path segment and a log field.
func DeriveSessionID(channel, user, profile string) string {
	digest := sha256.New()
	digest.Write([]byte(sessionIDDomain))
	for _, part := range []string{channel, user, profile} {
		digest.Write([]byte{sessionIDSep})
		fmt.Fprintf(digest, "%d:%s", len(part), part)
	}
	return sessionIDPrefix(channel) + "-" + hex.EncodeToString(digest.Sum(nil))[:sessionIDHashHexLen]
}

// sessionIDPrefix 返回可安全嵌入标识的渠道前缀；不合法时回落固定 "ch"。
// sessionIDPrefix returns the channel prefix when it is safe to embed, or "ch".
func sessionIDPrefix(channel string) string {
	if len(channel) == 0 || len(channel) > 32 {
		return sessionIDFallbackPrefix
	}
	for i, r := range channel {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			continue
		case r == '-' && i > 0:
			continue
		default:
			return sessionIDFallbackPrefix
		}
	}
	return channel
}
