package app

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage/postgres"
)

// startRunTest 是 US-24 签名适配的测试助手：夹具建行只看错误、不关心租约 token。
// startRunTest adapts the US-24 StartRun signature for fixtures that only assert on the
// error and do not care about the lease token.
func startRunTest(store *postgres.RunStore, ctx context.Context, rec core.RunRecord, manifest core.RunResourceManifest) error {
	_, err := store.StartRun(ctx, rec, manifest, core.WorkerIdentity{}, core.CallerAdmission{}, rec.RunID)
	return err
}
