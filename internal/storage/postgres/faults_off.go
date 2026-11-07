//go:build !integration

package postgres

import (
	"context"
	"database/sql"
)

// 本文件是提交故障钩子的**生产构建空壳**（设计 §3.1.1 `DB` 行："仅测试构建可见的
// 提交故障钩子"）。`newCommitFaults` 返回 nil，于是 `Tx` / `VerifyTx` 里的注入分支
// 恒不进入——发布二进制**不存在**故障注入面，也不携带任何与之相关的对外符号。
//
// This is the production stub: newCommitFaults returns nil, so no injection branch is
// ever reachable and the released binary exposes nothing fault-related.

// commitFaults 只保留类型与方法签名，保证共享代码在无 `integration` 标签时也能编译。
// commitFaults keeps only the shape needed to compile the shared code.
type commitFaults struct{}

// newCommitFaults 在生产构建里恒为 nil（钩子不可用）。
// newCommitFaults is always nil outside integration builds.
func newCommitFaults(*DB) *commitFaults { return nil }

// commitWithFaults 恒报"未装载故障"，把控制权交回正常提交路径。
func (f *commitFaults) commitWithFaults(*sql.Tx, TxAttempt,
	func(error, TxAttempt) (TxOutcome, error),
) (TxOutcome, error, bool) {
	return "", nil, false
}

// replicaPool 在生产构建里恒为 nil：核查只走主库连接池。
func (f *commitFaults) replicaPool() *sql.DB { return nil }

// holdAdmissionLock 在生产构建里是无操作：发布二进制不携带任何准入暂停点。
// holdAdmissionLock is a no-op in the production build.
func (f *commitFaults) holdAdmissionLock(context.Context) error { return nil }
