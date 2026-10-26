package core

// 本文件声明 MCP 每个槽位的凭据来源与它的视图状态（14D / 设计 §2.3.2 / §3.3）。
//
// **槽位是凭据的身份**：`env` 的键名或 `http.headers` 的头名，一个槽位一条来源声明、
// 一套版本与撤销（`credentials.binding_key` 与 `mcp_credential_sources.binding_key`
// 是同一个键空间）。契约 `McpCredentialInput.key` / `McpCredentialKeyView.key` 与迁移
// `0006` 的列注释都以槽位名为准——不是 `${VAR}` 里的变量名：同一条配置可以把两个槽位
// 指向同一个变量，按变量名归并就无法表达"这一槽走托管、那一槽走环境"。
//
// 一条纪律：**来源必须显式记录**。靠"托管行是否存在"推断来源，会让一次撤销变成
// 隐式回落进程环境——撤了还在用。因此 `mcp_credential_sources` 每个槽位一行，
// `managed` 缺失 / 撤销 / 解密失败一律立即失败，从不回落 `env`。
// This file declares per-slot MCP credential sources and their view state. The slot is
// the env key or the header name; the source is always recorded explicitly because
// inferring it from "does a managed row exist" would turn a revoke into an implicit
// fallback to the process environment.

import (
	"fmt"
	"slices"
	"strings"
)

// MCPCredentialSource 是 `mcp_credential_sources` 的行视图：一个 MCP server 的一个
// 槽位当前声明的凭据来源。托管值按它逐条独立版本与撤销。
// MCPCredentialSource is the row of one slot's declared credential source; the binding
// key shares its namespace with credentials.binding_key.
type MCPCredentialSource struct {
	ServerUID   ResourceUID
	BindingKey  string
	Mode        CredentialMode
	Revision    int
	UpdatedAt   int64
	OperationID string
}

// MCPCredentialKeyState 是槽位在资源视图里的来源状态（契约 `credential_key_state`）。
// 四个字面量是冻结值：
//
//	managed         — 声明托管且当前版本可用，运行期解密注入
//	managed_revoked — 声明托管但凭据已撤销：不可用，**不**回落进程环境
//	env             — 声明读进程环境；变量当前是否就绪由诊断的 env 轴回答
//	missing         — 没有来源声明（或声明托管却没有托管值）：配置校验失败，要求显式选择
//
// MCPCredentialKeyState is the frozen credential_key_state of one slot.
type MCPCredentialKeyState string

const (
	// MCPCredentialManaged 表示托管值可用。
	MCPCredentialManaged MCPCredentialKeyState = "managed"
	// MCPCredentialManagedRevoked 表示托管值已撤销，且不回落进程环境。
	MCPCredentialManagedRevoked MCPCredentialKeyState = "managed_revoked"
	// MCPCredentialEnv 表示按进程环境解析。
	MCPCredentialEnv MCPCredentialKeyState = "env"
	// MCPCredentialMissing 表示没有来源声明 / 声明托管但无值。
	MCPCredentialMissing MCPCredentialKeyState = "missing"
)

// Valid 报告该取值是否属于契约闭集（四值）。
// Valid reports whether the value is one of the four frozen literals.
func (s MCPCredentialKeyState) Valid() bool {
	switch s {
	case MCPCredentialManaged, MCPCredentialManagedRevoked, MCPCredentialEnv, MCPCredentialMissing:
		return true
	default:
		return false
	}
}

// MCPCredentialSlots 按字典序列出一条配置的全部凭据槽位名：`env` 的键名与
// `http.headers` 的头名。同一个名字同时出现在两处时无法判定归属，按配置校验失败
// 拒绝（契约 `McpCredentialInput.key` 的「跨两者重名 → 400」）。
//
// 顺序固定（排序）是判据：视图列、保存时的显式来源检查与运行期注入都按同一顺序走，
// map 遍历顺序随机，**不得**直接 range。
// MCPCredentialSlots lists every slot name of one configuration in sorted order; a name
// used as both an env key and a header name is refused because it has no single owner.
func MCPCredentialSlots(cfg MCPServerConfig) ([]string, error) {
	slots := make([]string, 0, len(cfg.Env)+len(cfg.Headers))
	envKeys := make(map[string]struct{}, len(cfg.Env))
	for key := range cfg.Env {
		envKeys[key] = struct{}{}
		slots = append(slots, key)
	}
	for name := range cfg.Headers {
		if _, clash := envKeys[name]; clash {
			return nil, fmt.Errorf(
				"server %s 的 %q 同时是 env 键名与请求头名——同一名字只能占一个槽位，请改名后重试",
				cfg.Name, name)
		}
		slots = append(slots, name)
	}
	slices.Sort(slots)
	return slots, nil
}

// MCPSlotPlaceholder 返回一个槽位在定义文件里的占位符名（`${VAR}` 里的 `VAR`）。
// `mode=env` 时按它读进程环境——这与 14D 之前的行为逐字相同；托管值不看它。
// MCPSlotPlaceholder returns the ${VAR} name of one slot; env mode reads that process
// variable, which is exactly the pre-14D behaviour.
func MCPSlotPlaceholder(cfg MCPServerConfig, slot string) (string, bool) {
	ref, ok := cfg.Env[slot]
	if !ok {
		ref, ok = cfg.Headers[slot]
	}
	if !ok {
		return "", false
	}
	return EnvPlaceholderName(ref)
}

// MCPHeaderPlaceholders 按头名升序返回 `headers` 的 `(头名, 占位符名)` 对；装配认证头时
// 需要头名与它引用的变量名成对出现。头名不声明来源时按名字对不上，因此只列能解析的。
// MCPHeaderPlaceholders lists (header name, placeholder name) pairs in header order.
func MCPHeaderPlaceholders(headers map[string]string) [][2]string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)
	pairs := make([][2]string, 0, len(names))
	for _, name := range names {
		placeholder, _ := EnvPlaceholderName(headers[name])
		pairs = append(pairs, [2]string{name, placeholder})
	}
	return pairs
}

// MCPCredentialStateOf 把"声明的来源 + 托管凭据身份状态"投影为视图状态。`identity`
// 为 nil 表示没有托管凭据行（未声明 → missing；声明托管但无行 → 也是 missing：
// 声明存在却没有可用的值，运行期必然失败，不能显示成 managed）。
// MCPCredentialStateOf projects the declared mode plus the managed identity state.
func MCPCredentialStateOf(mode CredentialMode, identity *CredentialRecord) MCPCredentialKeyState {
	switch mode {
	case CredentialModeEnv:
		return MCPCredentialEnv
	case CredentialModeManaged:
		switch {
		case identity == nil:
			return MCPCredentialMissing
		case identity.State == CredentialIdentityActive:
			return MCPCredentialManaged
		default:
			return MCPCredentialManagedRevoked
		}
	default:
		return MCPCredentialMissing
	}
}

// MCPBindingKey 校验一个槽位名做绑定键。槽位名来自 `env` 的键名或 `headers` 的头名，
// 因此两种字符集都要接受（头名可以带 `-`），但必须满足：非空、无首尾空白、不含空白 /
// 控制字符 / `$` / `{` / `}` / `=`。占位符形态（`${VAR}`）本身拒绝——那是值，不是槽位。
// 非法输入返回 ok=false，由调用方给出点名错误（不回显原值——它可能是用户误贴进来的
// 凭证）。
// MCPBindingKey validates a slot name for use as a binding key; the error side must
// never echo the raw input, which may be a misplaced credential.
func MCPBindingKey(raw string) (string, bool) {
	if raw == "" || len(raw) > 128 || strings.TrimSpace(raw) != raw {
		return "", false
	}
	if strings.ContainsAny(raw, "${}=") {
		return "", false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] == 0x7f {
			return "", false
		}
	}
	return raw, true
}
