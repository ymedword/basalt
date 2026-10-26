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
	"errors"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/workspace"
)

const (
	// mcpGateStagePreflight 是闸门唯一会失败的阶段：任何进程 / 连接动作之前。
	// The gate can only fail in the preflight stage.
	mcpGateStagePreflight = "preflight"
	// mcpGateClassEnvMissing 是"声明读进程环境，变量未设置或为空"。
	// mcpGateClassEnvMissing: an env-sourced slot whose variable is missing or empty.
	mcpGateClassEnvMissing = "env_missing"
	// mcpGateClassCredentialUnavailable 是"声明托管，但无值 / 已撤销 / 解密失败"。
	// 它与 env 缺失同为 503，但原因不同——合报会把"撤了还在用"引向"去查环境变量"。
	// 这两个字面量只属于保存闸门的 detail，**不**扩测试记录的 `failure_class` 枚举（D-05）：
	// 那套码描述一次真实连接尝试，本结构描述"根本没到连接"。
	// mcpGateClassCredentialUnavailable: a managed slot with no usable value. These
	// literals belong to the save gate's detail only; the test-record enum is unchanged.
	// #nosec G101 -- 闸门分类字面量，不是凭据（变量名命中）。
	mcpGateClassCredentialUnavailable = "credential_unavailable"
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

// mcpGateFailureClassOf 把可用性失败归到闸门的两个分类。
// mcpGateFailureClassOf maps an availability failure to the gate's two classes.
func mcpGateFailureClassOf(cause error) string {
	var envFailure mcpEnvSlotFailure
	if errors.As(cause, &envFailure) {
		return mcpGateClassEnvMissing
	}
	return mcpGateClassCredentialUnavailable
}

// validateMCPPublishedCredentials 把**已发布 / 回滚目标版本**的条目按声明的来源取值
// 一遍（`env` 读进程环境，`managed` 解密托管库），拿到的明文立即清零：发布闸门只关心
// "取得到 / 取不到"，不需要值本身。条目本身不可解析时原样上抛——那是候选非法，按 400。
// It resolves the target version by declared source and discards the plaintext.
func (s *ResourceAdmin) validateMCPPublishedCredentials(ctx context.Context, name string,
	uid core.ResourceUID, entryRaw []byte,
) error {
	if s.mcpCredentials == nil {
		return nil
	}
	cfg, err := workspace.ParseMCPServerBlock(entryRaw)
	if err != nil {
		return err
	}
	values, err := s.mcpCredentials.Resolve(ctx, name, uid, cfg)
	if err != nil {
		return err
	}
	ZeroMCPCredentialValues(values)
	return nil
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
		FailureClass: mcpGateFailureClassOf(cause),
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
