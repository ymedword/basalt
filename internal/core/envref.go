package core

import "strings"

// EnvPlaceholderName 判定精确 `${VAR}` 形态并返回变量名：整串必须是 `${` + 标识符 + `}`，
// 标识符以字母或下划线开头，其后可带数字。半截占位（`${A}${B}` / `${A}x` / `x${A}`）、
// `$A`、空名 `${}`、数字开头 `${1A}` 一律 false——放行它们等于把"凭证只能来自环境"的
// 门禁变成前缀匹配。本函数是 `${VAR}` 判定的**唯一实现**：`workspace`（MCP env 值）、
// `cli`（配置凭证键）与 `tool`（通知渠道 url）三处调用点共用，不得复制第二份。
// EnvPlaceholderName accepts exactly ${VAR}: a brace-wrapped identifier starting with a
// letter or underscore, optionally followed by digits. Every near-miss form is rejected.
// This is the single implementation shared by workspace, cli and tool.
func EnvPlaceholderName(s string) (name string, ok bool) {
	if len(s) < 4 || !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", false
	}
	name = s[2 : len(s)-1]
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return "", false
		}
	}
	return name, true
}
