package web

import (
	"fmt"

	"github.com/ymedword/basalt/internal/core"
)

// 本文件只在**启动期**把管理认证配置折算成 `core.AdminPosture`（US-16 §2.14 /
// C-42 / D-31 / T027）。三条纪律：
//  1. **先校验、后折算**——`ValidateAdminAuth` 失败即返回零值姿态与错误，调用方
//     必须拒绝启动；**不得**返回一个"看起来还能用"的姿态。
//  2. **token 姿态看解析结果**——空 token 与残留 `${...}` 占位都由
//     `ValidateAdminAuth` 拒绝，不是"字段非空"就算数。
//  3. **不产生 `unavailable`**——真实启动中非法配置已在监听前被拒；该值只由测试
//     直接注入调度器，用来证明管理能力缺失时 fail-closed。
//
// This file resolves the startup-time admin posture. Validation comes first; a failed
// configuration yields the zero posture plus an error, and the real startup path can
// never produce AdminPostureUnavailable.

// ResolveAdminPosture 在监听器已绑定后计算管理能力姿态：`token` 与 `local_trust`
// 都是**合法启用态**（`local_trust` 不得被当成"认证关闭"）；配置缺失 / 模式非法 /
// token 未解析 / 非回环未列白名单一律返回错误，由启动流程拒绝启动。
//
// 返回值只可能是两个启用态之一：错误路径返回零值（`Valid()` 为 false），成功路径
// **不会**是 `core.AdminPostureUnavailable`——真实启动不存在"管理能力关闭但继续跑"
// 的第三种结果。
// ResolveAdminPosture derives the posture from the validated configuration. Only the
// two enabled states can be returned; configuration errors yield the zero posture.
func ResolveAdminPosture(cfg AdminAuthConfig, listeners []string) (core.AdminPosture, error) {
	if err := ValidateAdminAuth(cfg, listeners); err != nil {
		return "", err
	}
	switch cfg.Mode {
	case adminAuthModeToken:
		return core.AdminPostureToken, nil
	case adminAuthModeLocalTrust:
		return core.AdminPostureLocalTrust, nil
	default:
		// ValidateAdminAuth 已拒绝其它取值；这里只防两处判定漂移时静默放行。
		// ValidateAdminAuth already rejects other modes; this stops silent drift.
		return "", fmt.Errorf("管理认证模式 %q 无法折算为管理能力姿态", cfg.Mode)
	}
}
