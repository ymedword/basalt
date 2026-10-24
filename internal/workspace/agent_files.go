package workspace

// 本文件是 Agent 目录的**名称判据**面（create / copy / restore / archive 共用同一函数，
// 不得各写一份）：单段目录名、不得以点开头、不得以 `.staging` / `.backup` 结尾（这两个
// 后缀是写路径自己的残骸名，颗粒度文档约束 27 / 30 / 40）。目录生命周期本身在
// agent_admin.go：那里的函数**不取实例锁、不写生命周期状态文件**——锁与状态迁移由
// app.AgentAdmin（HTTP）与 app/ports.go（CLI）编排（约束 49：状态文件唯一写者是
// lifecycle.go）。
// This file holds the shared agent-name rules. Directory lifecycle lives in
// agent_admin.go and deliberately takes no instance lock and writes no lifecycle
// state: the callers own both.

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

const (
	// archiveDirName 是归档目录（工作区根部，与 agents/ 并列；basalt init 已建）。
	archiveDirName = "archive"

	// archiveStampLayout 是归档名的时间戳形态：<name>-YYYYMMDD-HHMMSS（UTC）。
	archiveStampLayout = "20060102-150405"
)

// stagingSuffix / backupSuffix 由 recovery.go 声明：残留处置与名称判据必须同一份后缀
// （staging 是构建中的候选树，backup 是两次 rename 交换时的旧副本）。两者都不得被用作
// Agent 名——否则残留目录会让装载面把半成品当成正式 Agent（约束 27 / 30 / 40）。

// ValidateAgentName 是目录写路径**共用**的名称校验（同一函数，不得各写一份）：
// 非空、单个目录名——不以点开头（`.staging` / `.backup` 与隐藏目录都属保留面）、
// 不以保留后缀结尾、不含路径分隔符（/ 与 \）、不为 "." / ".."、不含 NUL。
// 导出是因为它同时是端口层（cli 命令面）与管理写面的前置判据。
// ValidateAgentName is the shared name check for every directory writer.
func ValidateAgentName(name string) error {
	switch {
	case name == "":
		return errors.New("目录名不得为空")
	case name == "." || name == "..":
		return fmt.Errorf("目录名不得为 %q", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("目录名不得以点开头（点前缀属于 staging / backup 残骸的保留面）：%q", name)
	case strings.HasSuffix(name, stagingSuffix) || strings.HasSuffix(name, backupSuffix):
		return fmt.Errorf("目录名不得以 %s / %s 结尾（写路径保留后缀）：%q", stagingSuffix, backupSuffix, name)
	case strings.ContainsAny(name, "/\\\x00"):
		return fmt.Errorf("目录名不得含路径分隔符或 NUL：%q", name)
	}
	return nil
}

// writeFailed 记一条 ERROR（工作区写入失败的可观测性面——FR-028）并原样返回错误。
// 键沿用装载路径的固定三键 agent / reason / detail（不新造键）；本函数签名里没有 logger
// （§3.1 冻结的签名不含 logger），故走进程默认日志器。
// writeFailed logs the write failure with the fixed keys and returns the error unchanged.
func writeFailed(name string, err error) error {
	slog.Default().Error("Agent 工作区写入失败",
		"agent", name, "reason", "workspace write failed", "detail", err.Error())
	return err
}
