package web

// 本文件是**管理台与用户可见文案的唯一中文映射处**（R8 裁决 1，模块卡 R8.3.2）。
// 输入只认类型与机器可判的分类：生命周期拒绝看 `core.AgentUnavailableError.Kind`
// （构造点写入），装载拒因看 `core.AgentRejectKind`（装载器与注册表写入）；**不**解析
// 底层错误文本，**不**把英文原文回显给用户。英文 `RejectReason` 仍进日志与审计
// `error_message`（R8.3.3），错误码 / HTTP 状态 / `data` 字段名一律不变。
// This file is the single Chinese projection for console-visible copy. It switches on
// types and machine-judgeable categories only; raw English reasons stay in logs and in
// management_operations.error_message, never in the response body.

import (
	"fmt"
	"strings"

	"github.com/ymedword/basalt/internal/core"
)

// agentUnavailableMessage 把生命周期拒绝按 Kind 投影为中文（五种，见卡 R8.3.2）。
// 分类不读 `Reason` 文本：文案一改，字符串匹配就会悄悄失效。
// agentUnavailableMessage maps the refusal category to Chinese copy; the Kind field is
// the only input, never the English Reason text.
func agentUnavailableMessage(err *core.AgentUnavailableError) string {
	switch err.KindOrDefault() {
	case core.AgentUnavailableDisabled:
		return fmt.Sprintf("Agent「%s」已停用，请先启用后再执行任务。", err.Name)
	case core.AgentUnavailableArchived:
		return fmt.Sprintf("Agent「%s」已归档，请先从归档恢复后再执行任务。", err.Name)
	case core.AgentUnavailableMissing:
		return fmt.Sprintf("Agent「%s」的生命周期状态文件缺失，请联系管理员恢复状态文件后再执行。", err.Name)
	case core.AgentUnavailableCorrupt:
		return fmt.Sprintf("Agent「%s」的生命周期状态文件损坏，请联系管理员用可信副本恢复。", err.Name)
	default:
		return fmt.Sprintf("Agent「%s」当前不可执行，请先确认其生命周期状态。", err.Name)
	}
}

// lifecycleConflictMessage 是 409 `CONFLICT` 的固定中文：期望状态 / 代数不符与
// 「已在目标态」共用一条（判定口径不变，只换文案）。英文哨兵只进日志。
// lifecycleConflictMessage is the fixed Chinese copy for the 409 conflict; the English
// sentinel stays in the log only.
func lifecycleConflictMessage(agent string) string {
	return fmt.Sprintf("Agent「%s」的状态已被其他操作改变，请刷新后重试。", agent)
}

// agentRejectMessage 把装载拒绝按**类型化分类**投影为中文（SC-033：不按英文文本
// 匹配）。detail 只在分类自身承诺"可读点名"时填入：名字 / 相对路径 / 本仓自产中文；
// 读取失败、frontmatter 解析与 cron 语法细节只留服务端日志，避免英文库文案或绝对
// 路径进入界面。零值分类是保守兜底，同样不读任何英文文本。
// agentRejectMessage projects a typed rejection kind to Chinese. Detail is used only
// where the category promises a readable pointer; raw library text never ships.
func agentRejectMessage(kind core.AgentRejectKind, detail string) string {
	switch kind {
	case core.AgentRejectReadFailed:
		return "读取 AGENT.md 失败（详情见服务端日志）"
	case core.AgentRejectInvalidFrontmatter:
		return "AGENT.md 的 frontmatter 无法解析（详情见服务端日志）"
	case core.AgentRejectInvalidResourceBindings:
		return "资源绑定块（resource_bindings）结构非法"
	case core.AgentRejectNameMismatch:
		return problemDetail("目录名与 AGENT.md 的 name 不一致：%s", detail)
	case core.AgentRejectProviderNotRegistered:
		return problemDetail("引用的模型连接未发布或不存在：%s", detail)
	case core.AgentRejectBlankModel:
		return "未在 AGENT.md 中声明默认模型"
	case core.AgentRejectInvalidTemperature:
		return problemDetail("输出随机性（temperature）取值非法：%s", detail)
	case core.AgentRejectToolNotRegistered:
		return problemDetail("工具未注册：%s", detail)
	case core.AgentRejectSkillNotFound:
		return problemDetail("技能未找到：%s", detail)
	case core.AgentRejectMCPServerSkipped:
		return problemDetail("MCP 服务不可用（未配置或未连接）：%s", detail)
	case core.AgentRejectBootstrapMissing:
		return problemDetail("上下文文件缺失：%s", detail)
	case core.AgentRejectInvalidSchedule:
		// cron 库原文是英文：分类固定中文，细节只进日志（SC-033）。
		return "定时任务声明非法：请检查 cron 表达式、时区、消息与投递目标（详情见服务端日志）"
	case core.AgentRejectInvalidNotifyChannels:
		return problemDetail("通知渠道声明非法：%s", detail)
	case core.AgentRejectInvalidNotifyMode:
		return problemDetail("通知模式声明非法：%s", detail)
	case core.AgentRejectSkillVersionMissing:
		return problemDetail("绑定的技能没有已发布版本：%s", detail)
	default:
		return "装载校验未通过（详情见服务端日志）"
	}
}

// entryRejectMessage 是装载条目的统一出口：有分类走分类，没有分类时给保守中文，
// **不**回显条目里的英文 `RejectReason`。生产路径的拒绝一律带分类（装载器与注册表
// 两处写入）；零分类只可能来自人工构造的条目。
// entryRejectMessage is the single exit for loader entries; a missing kind yields the
// conservative fallback instead of echoing the English reason.
func entryRejectMessage(entry core.AgentEntry) string {
	if entry.RejectKind == "" {
		return "装载校验未通过（详情见服务端日志）"
	}
	return agentRejectMessage(entry.RejectKind, entry.RejectDetail)
}

// problemDetail 填入点名细节；空细节时去掉模板尾部的冒号，避免悬空标点。
// problemDetail fills the pointer slot and drops a dangling colon when detail is empty.
func problemDetail(format, detail string) string {
	if detail == "" {
		return strings.TrimSuffix(format, "：%s")
	}
	return fmt.Sprintf(format, detail)
}
