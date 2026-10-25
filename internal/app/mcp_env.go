package app

// 本文件是保存期 env 闸门（模块卡 §2.3 图 2-2 / §3.1.4）：保存 / 发布 / 回滚在**第一次
// 写定义文件之前**证明"当前 Basalt 进程环境满足这条条目的 `${VAR}` 引用"。闸门只复用
// 装配期的解析规则（`resolveMCPEnv`），不新增第二条 YAML 解析路径，也不解析 `args`
// 中的占位（设计边界，见模块卡 §2.5.5）。
// The save-time env gate: prove the current process environment satisfies the entry's
// ${VAR} references before the first definition-file write, reusing the assembly rule.

import (
	"context"
	"encoding/json"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// mcpGateStagePreflight / mcpGateClassEnvMissing 是 D-05 枚举里本闸门只用到的两个
	// 值：失败发生在任何进程 / 连接动作之前。
	// The gate can only fail in the preflight stage, with the env_missing class.
	mcpGateStagePreflight  = "preflight"
	mcpGateClassEnvMissing = "env_missing"
)

// mcpEnvGateDetail 是闸门失败写进 `detail_json` 的**脱敏**结构：只有分类与"点名
// server / 键 / 变量名"的错误消息，没有任何值字段——脱敏是结构性质，不靠调用方自觉。
// mcpEnvGateDetail is the redacted detail payload: classification only, never values.
type mcpEnvGateDetail struct {
	ErrorCode    string `json:"error_code"`
	Stage        string `json:"stage"`
	FailureClass string `json:"failure_class"`
	ErrorMessage string `json:"error_message"`
}

// validateMCPEntryEnv 把候选条目的 `${VAR}` 占位按当前进程环境解析一遍：
// 未设置与空值同档拒绝（与 `resolveMCPEnv` 同一判据），错误只点名 server / 键 / 变量名。
// 条目本身不可解析时原样上抛——那是候选非法，调用方按 400 处理。
// validateMCPEntryEnv resolves the entry's placeholders with the assembly-time rule;
// missing and empty values are refused alike and the error never carries a value.
func validateMCPEntryEnv(entryRaw []byte, lookup func(string) (string, bool)) error {
	cfg, err := workspace.ParseMCPServerBlock(entryRaw)
	if err != nil {
		return err
	}
	_, err = resolveMCPEnv([]core.MCPServerConfig{cfg}, lookup)
	return err
}

// rejectMCPEnvGate 把闸门失败收敛为「failed 操作行 + 503 载体」：创建 / 修改 / 发布 /
// 回滚四个入口共用这一处拼装，detail 形状与 HTTP 映射口径不会各自漂移。
// 操作行先落 `NOT_CONFIGURED` 再返回；文件 / 版本 / 指针由调用方的"闸门在第一次写之前"
// 位置保证零变化。
// rejectMCPEnvGate converges the four entry points onto one rejection shape.
func (s *ResourceAdmin) rejectMCPEnvGate(ctx context.Context, operationID, message string,
	cause error,
) *core.AdminError {
	payload, marshalErr := json.Marshal(mcpEnvGateDetail{
		ErrorCode:    resourceResultNotConfigured,
		Stage:        mcpGateStagePreflight,
		FailureClass: mcpGateClassEnvMissing,
		ErrorMessage: cause.Error(),
	})
	if marshalErr != nil {
		// 结构化 detail 编不出来时宁可不带 detail，也不退回把原始信息拼进列里。
		s.logger.Error("env 闸门 detail 序列化失败", "operation_id", operationID, "error", marshalErr)
		payload = nil
	}
	s.finish(ctx, operationID, core.OperationFailed, resourceResultNotConfigured, message, payload)
	return &core.AdminError{
		Kind:    core.ErrProviderNotConfigured,
		Message: cause.Error(),
		Data:    map[string]any{"operation_id": operationID},
	}
}
