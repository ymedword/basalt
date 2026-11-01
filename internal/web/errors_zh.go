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

// errorCodeMessageTemplates 是契约 §8 本轮（US-16，2026-10-28 冻结）新增六个错误码的
// 固定中文模板。其余既有码的文案由各调用点持有（多为已冻结常量或按上下文组装），本表
// **只**承载新码，避免出现第二份"全量文案表"与既有出口漂移。模板尾部的 `：%s` 由
// problemDetail 填入点名细节；无细节时连同冒号一起去掉。
// Fixed Chinese templates for the six §8 codes added by US-16; pre-existing codes keep
// their call-site copy. The trailing `：%s` carries a machine-derived pointer.
var errorCodeMessageTemplates = map[string]string{
	codeInvalidScope:             "作用域非法（只接受已登记的业务作用域）：%s",
	codeIdempotencyConflict:      "幂等键冲突（同一幂等键的再次请求参数与首次不一致）：%s",
	codeGone:                     "目标已被保留策略或软删除移除，不再可读：%s",
	codeCallerRateLimited:        "调用方请求速率超限，请按响应头 Retry-After 稍后重试：%s",
	codeCallerConcurrencyLimited: "调用方并发运行名额已用尽，请等待运行结束后重试：%s",
	codeQuotaExhausted:           "调用方周期配额不足，请等待下一周期开始或联系管理员核对：%s",
}

// errorCodeMessage 把本轮新增的错误码投影为固定中文文案。detail 只许填**机器可判**的
// 点名信息（scope 字面量 / 冲突参数名 / 当前上限 / 恢复时间 / 重试秒数），**不得**填
// 原始错误文本、用户内容或路径——那些只进日志与审计。未登记的码**不得**回退成通用
// 文案而丢失码字面量：兜底点名 `code` 并丢弃 detail（未知码的点名信息安全性无从判定），
// 调用点也**不得**把未知码改写成 INTERNAL。
// errorCodeMessage renders the fixed Chinese copy for the US-16 codes; an unregistered
// code falls back to a message that keeps the literal code and drops the detail.
func errorCodeMessage(code, detail string) string {
	if template, ok := errorCodeMessageTemplates[code]; ok {
		return problemDetail(template, detail)
	}
	return fmt.Sprintf("未知错误码 %s（详情见服务端日志）", code)
}

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
	case core.AgentRejectInvalidKnowledge:
		return "知识库集合声明（knowledge）结构非法"
	case core.AgentRejectKnowledgeCollectionMissing:
		// 点名 + 下一步（用户语言）：声明缺失会让整只 Agent 不注册，只说"不存在"
		// 会让人以为"只是那个集合用不了"。
		return problemDetail("声明的知识库集合不存在或同名不唯一：%s（请先在「知识库」页建立同名集合，"+
			"或改掉 AGENT.md 的 knowledge 声明名——声明缺失会让该 Agent 整只不注册）", detail)
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
