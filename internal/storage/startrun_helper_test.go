package storage

import (
	"context"

	"github.com/ymedword/basalt/internal/core"
)

// startRunTest 是 US-24 签名适配的测试助手：SQLite 源谱系不创建执行租约，用例只看错误。
// startRunTest adapts the US-24 StartRun signature for the SQLite source lineage, which
// creates no execution leases; tests only assert on the error.
func startRunTest(store *RunStore, ctx context.Context, rec core.RunRecord, manifest core.RunResourceManifest) error {
	_, err := store.StartRun(ctx, rec, manifest, core.WorkerIdentity{}, core.CallerAdmission{}, rec.RunID)
	return err
}
