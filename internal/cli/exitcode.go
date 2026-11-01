package cli

// 本文件把「四档退出码」从散落的 if 判断收敛成一个可判定的类型：命令面与装配层都可以
// 用 `NewExitError` 表达 `0/1/2/3`，装配骨架（`internal/app`）只负责把它翻译成进程退出码。
//
// 为什么需要它：`secrets recover-record` 与 `secrets adopt-legacy` 的契约（颗粒度文档
// §3.1.8b / §3.1.8c）区分四种结局——校验失败、前置不满足、预写后失败——自动化脚本要据此
// 决定「改参数重试 / 修环境 / 走查证流程」。把四档压成「1」会让预写后的半截状态与纯粹的
// 参数错误无法区分，恢复流程就会对着错误的原因做错误的事。
//
// This file gives the command surface a typed way to express the four documented exit codes;
// the assembly skeleton only translates it into the process exit status.

import (
	"errors"
	"fmt"
)

// ExitCode 是命令面错误的可判定退出码契约：实现者显式声明「这个失败应当以哪个码收场」。
// ExitCode is the contract every exit-code-bearing command error implements.
type ExitCode interface {
	// ExitCode 返回进程退出码（本仓约定 0/1/2/3，见颗粒度文档 §3.1.8b / §3.1.8c）。
	// ExitCode reports the process status this failure maps to.
	ExitCode() int
}

// exitError 是 ExitCode 的默认实现：错误文案与退出码一起传递，`errors.As` 可穿透包装。
// exitError carries a message together with the status it maps to; Unwrap keeps
// errors.Is / errors.As working through wrapping.
type exitError struct {
	code int
	err  error
}

// Error 返回底层错误文案（退出码不进文案——文案是给人看的，码是给脚本看的）。
// Error exposes the wrapped message only; the status is not part of the text.
func (e *exitError) Error() string { return e.err.Error() }

// Unwrap 让 `errors.Is` / `errors.As` 继续看到被包装的哨兵错误。
// Unwrap keeps sentinel errors reachable through the wrapper.
func (e *exitError) Unwrap() error { return e.err }

// ExitCode 实现 ExitCode 接口。
// ExitCode satisfies the ExitCode contract.
func (e *exitError) ExitCode() int { return e.code }

// NewExitError 构造带退出码的命令面错误；参数语义同 `fmt.Errorf`（支持 `%w` 包装）。
// NewExitError builds a status-bearing error; the format arguments follow fmt.Errorf.
func NewExitError(code int, format string, args ...any) error {
	return &exitError{code: code, err: fmt.Errorf(format, args...)}
}

// WithExitCode 给已有错误补一个退出码；err 为 nil 时返回 nil（便于在 `if err != nil` 之后
// 直接套用而不额外判空）。已带退出码的错误保持原码，不被覆盖——外层不该改写内层的判定。
// WithExitCode annotates an existing error; a nil error stays nil and an error that
// already carries a status keeps it (the inner decision wins).
func WithExitCode(code int, err error) error {
	if err == nil {
		return nil
	}
	var existing ExitCode
	if errors.As(err, &existing) {
		return err
	}
	return &exitError{code: code, err: err}
}

// exitCodeOf 取出错误自带的退出码；没有则返回 fallback。
// exitCodeOf reports the carried status, or the fallback when none is present.
func exitCodeOf(err error, fallback int) int {
	var coded ExitCode
	if errors.As(err, &coded) {
		code := coded.ExitCode()
		if code >= 0 && code <= 3 {
			return code
		}
	}
	return fallback
}

// ExitStatusOf 是装配骨架读取退出码的**唯一**入口：带退出码的错误按其码收场，其余一律
// 归为 1（既有「失败即 1」的默认不变——新增四档不改变其他命令的行为）。
// ExitStatusOf is the assembly skeleton's only way to read the status: errors that carry
// one keep it, everything else stays the historical "failure is 1".
func ExitStatusOf(err error) int { return exitCodeOf(err, 1) }
