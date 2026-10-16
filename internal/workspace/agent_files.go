package workspace

// 本文件是 Agent 目录的**写模式**面：名称校验（create / delete 共用）、create 的模板落盘、
// delete 的归档移动。写模式命令内部先取实例锁（契约 run-and-errors.md §4.3.1：被活实例
// 持有时拒绝启动，且拒绝路径**零写入**）；归档是**单次 rename**——原子移动，绝不
// "复制 + 删除"、绝不"先删源后建目标"（归档撞名时源目录必须原样保留）。
// This file is the workspace write path: name validation, template creation and
// archive-as-delete. Both writers take the instance lock first.

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// archiveDirName 是归档目录（工作区根部，与 agents/ 并列；basalt init 已建）。
	archiveDirName = "archive"

	// archiveStampLayout 是归档名的时间戳形态：<name>-YYYYMMDD-HHMMSS（§3.1 DeleteAgent 注释）。
	archiveStampLayout = "20060102-150405"
)

// ValidateAgentName 是 create / delete **共用**的名称校验（同一函数，不得各写一份）：
// 非空、单个目录名——不含路径分隔符（/ 与 \）、不为 "." / ".."、不含 NUL。
// 导出是因为它同时是端口层（cli 命令面）的前置判据。
// ValidateAgentName is the shared name check for both writers.
func ValidateAgentName(name string) error {
	switch {
	case name == "":
		return errors.New("目录名不得为空")
	case name == "." || name == "..":
		return fmt.Errorf("目录名不得为 %q", name)
	case strings.ContainsAny(name, "/\\\x00"):
		return fmt.Errorf("目录名不得含路径分隔符或 NUL：%q", name)
	}
	return nil
}

// CreateAgent 在 root/agents/<name>/ 写入最小 AGENT.md 模板（写模式：内部先取实例锁）。
// 顺序：名称校验 → 取锁 → 建目录 → 写模板 → 释放锁（§2.3 流程 C）。
// 失败：建目录后写文件失败 → 清理**本次创建**的目录（只删本次新建物——既有目录一律不动，
// 这一点比"清理干净"更重要）；清理失败 → 错误文案如实点名残留路径（不掩盖）。
// created 是本次新落盘的相对路径（供命令输出）。
// CreateAgent writes a minimal loadable AGENT.md template, holding the instance lock.
func CreateAgent(root, name string, providers []string) (created []string, err error) {
	if err := ValidateAgentName(name); err != nil {
		return nil, err
	}

	lock, err := AcquireInstanceLock(root)
	if err != nil {
		return nil, err // *LockBusyError 原样上抛：拒绝启动、零业务写入
	}
	defer func() {
		if relErr := lock.Release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()

	base, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	// 未初始化工作区：agents/ 缺失 → 点名 + 指引（与命令面同一口径；Mkdir 也会因此失败，
	// 提前判是为了给出可操作的文案而不是裸的 ENOENT）。
	if _, statErr := base.Stat(filepath.FromSlash(agentsDirName)); statErr != nil {
		return nil, fmt.Errorf("工作区未初始化：缺少目录 %s（先执行 basalt init）: %w", agentsDirName, statErr)
	}

	relDir := path.Join(agentsDirName, name)
	relFile := path.Join(relDir, agentDefinitionFile)

	// agents/<name> 已存在 → 拒绝且**零写入**（不覆盖：先判后写，不得"先写后判"）。
	if _, statErr := base.Stat(filepath.FromSlash(relDir)); statErr == nil {
		return nil, fmt.Errorf("同名目录已存在，拒绝覆盖：%s", relDir)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("检查 Agent 目录 %s: %w", relDir, statErr)
	}

	content, err := agentTemplate(name, providers)
	if err != nil {
		return nil, err
	}

	if err := base.Mkdir(filepath.FromSlash(relDir), dirPerm); err != nil {
		return nil, writeFailed(name, fmt.Errorf("创建 Agent 目录 %s: %w", relDir, err))
	}
	if err := base.WriteFile(filepath.FromSlash(relFile), []byte(content), filePerm); err != nil {
		writeErr := fmt.Errorf("写入 Agent 定义 %s: %w", relFile, err)
		// 只删**本次创建**的目录（Remove 只删空目录——万一有人往里放了东西，它会失败并如实点名）。
		if cleanupErr := base.Remove(filepath.FromSlash(relDir)); cleanupErr != nil {
			writeErr = errors.Join(writeErr,
				fmt.Errorf("清理残留目录 %s 失败，请手工删除：%w", relDir, cleanupErr))
		}
		return nil, writeFailed(name, writeErr)
	}

	return []string{relFile}, nil
}

// DeleteAgent 归档：把 root/agents/<name>/ **单次 rename** 到 root/archive/<name>-<YYYYMMDD-HHMMSS>/；
// 会话 / 记忆 / 审计数据不动（DA §5.14）。写模式：内部先取实例锁——持有实例锁时不存在
// 在跑的 Run 与调度注册，故 §11.4 的「注销定时 → 移出注册 → 取消 Run」在 CLI 路径上是空集。
// 归档目标已存在 → 拒绝（**不覆盖**：同一秒内的二次删除不得吃掉已归档的副本，§2.4 注 11）；
// rename 原子：失败即源目录原样保留。now 显式传入（归档名含时间戳——时间显式传入是可判卷前提）。
// DeleteAgent archives the agent directory in one atomic rename.
func DeleteAgent(root, name string, now time.Time) (archived string, err error) {
	if err := ValidateAgentName(name); err != nil {
		return "", err
	}

	lock, err := AcquireInstanceLock(root)
	if err != nil {
		return "", err // *LockBusyError 原样上抛：拒绝启动、零业务写入
	}
	defer func() {
		if relErr := lock.Release(); relErr != nil {
			err = errors.Join(err, relErr)
		}
	}()

	base, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("打开工作区目录 %s: %w", root, err)
	}
	defer func() {
		if closeErr := base.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭工作区目录 %s: %w", root, closeErr))
		}
	}()

	relDir := path.Join(agentsDirName, name)
	// 源条目**必须**是目录：agents/ 下的普通文件不是 Agent 目录（装载面同样忽略它），
	// 归档它等于把运维的手记挪进 archive/。判定用 **Lstat**——符号链接如实报成符号链接、
	// 不跟随（跟随会拿链接指向的目标类型做判定，等于"指向哪就归档哪"）；R2 评审修复。
	info, statErr := base.Lstat(filepath.FromSlash(relDir))
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return "", fmt.Errorf("待归档目录不存在：%s", relDir)
		}
		return "", fmt.Errorf("检查 Agent 目录 %s: %w", relDir, statErr)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("待归档条目不是目录：%s（%s）", relDir, fileKind(info))
	}

	// 归档目录兜底：basalt init 已建，但工作区被手工清理过时不该失败在裸 ENOENT 上
	// （归档目录是本函数的**目的地**，属它自己拥有的落点）。
	if _, statErr := base.Stat(filepath.FromSlash(archiveDirName)); errors.Is(statErr, fs.ErrNotExist) {
		if mkErr := base.Mkdir(filepath.FromSlash(archiveDirName), dirPerm); mkErr != nil {
			return "", writeFailed(name, fmt.Errorf("创建归档目录 %s: %w", archiveDirName, mkErr))
		}
	} else if statErr != nil {
		return "", fmt.Errorf("检查归档目录 %s: %w", archiveDirName, statErr)
	}

	relTarget := path.Join(archiveDirName, name+"-"+now.Format(archiveStampLayout))
	// 归档目标已存在 → 拒绝且**源目录保留**。显式存在检查是必须的：rename 在多数平台上会
	// **静默覆盖**目标，"撞名自动换名"又会让归档副本的追溯性变差（§2.4 注 11）。
	if _, statErr := base.Stat(filepath.FromSlash(relTarget)); statErr == nil {
		return "", fmt.Errorf("归档目标已存在，拒绝覆盖：%s（请稍后重试）", relTarget)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return "", fmt.Errorf("检查归档目标 %s: %w", relTarget, statErr)
	}

	if err := base.Rename(filepath.FromSlash(relDir), filepath.FromSlash(relTarget)); err != nil {
		return "", writeFailed(name, fmt.Errorf("归档 Agent 目录 %s → %s: %w", relDir, relTarget, err))
	}
	return relTarget, nil
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
