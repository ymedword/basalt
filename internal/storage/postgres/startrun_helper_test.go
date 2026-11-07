package postgres

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
)

// startRunTest 是 US-24 签名适配的测试助手：不关心租约 token 的用例只看错误。
// 租约自身的行为（非零 token、owner / admission 语义）由租约专属用例直接调用 StartRun。
// startRunTest adapts the US-24 StartRun signature for tests that only assert on the
// error; lease-specific cases call StartRun directly.
func startRunTest(store *RunStore, ctx context.Context, rec core.RunRecord, manifest core.RunResourceManifest) error {
	_, err := store.StartRun(ctx, rec, manifest, core.WorkerIdentity{}, core.CallerAdmission{}, rec.RunID)
	return err
}

// startRunOwnedTest 按**真实 Worker 身份**建 Run：StartRun 只在 owner 非零时写 run
// 租约，而 Isolated（子树无活 run 租约）与回收器领取读的正是这条租约。零 owner 的
// startRunTest 对应"遗留 / 无租约"分支，不用于判租约的用例。
// startRunOwnedTest creates the run lease the subtree gates read.
func startRunOwnedTest(store *RunStore, ctx context.Context, rec core.RunRecord,
	manifest core.RunResourceManifest, owner core.WorkerIdentity,
) error {
	_, err := store.StartRun(ctx, rec, manifest, owner, core.CallerAdmission{}, rec.RunID)
	return err
}
