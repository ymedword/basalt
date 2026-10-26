package app

// 本文件判卷内网访问例外的管理服务（14D / D-13；契约 §9.4 的 `network_exception.*`
// 与 `/network-exceptions`）：理由必填、云元数据地址拒绝登记、重复登记不覆盖、
// 预写 `pending` 之后才有操作行、删除后**下一次**判定即失效、集合 ETag 一票否决。
// 真实 SQLite + 真实管理操作仓储作底座，判据只认类型与仓储读数，不比对文案。
// Acceptance tests for the exception service over a real SQLite database.

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/storage"
	"github.com/ymedword/basalt/internal/tool"
)

// exceptionEnv 是例外服务的共享夹具：真库 + 真管理操作仓储 + 真策略实例。
type exceptionEnv struct {
	service *NetworkExceptionService
	store   *storage.NetworkExceptionStore
	ops     *storage.ManagementStore
	policy  *tool.WhitelistPolicy
	logs    *bytes.Buffer
}

// exceptionClock 是夹具的固定时钟（创建时间可逐字断言）。
func exceptionClock() time.Time { return time.Date(2026, 10, 26, 9, 15, 0, 0, time.UTC) }

// newExceptionEnv 装配夹具；`mutate` 可替换 store / operations。
func newExceptionEnv(t *testing.T, mutate func(*NetworkExceptionDeps)) *exceptionEnv {
	t.Helper()
	db, err := storage.OpenWritable(context.Background(),
		filepath.Join(t.TempDir(), "basalt.db"), storage.SupportedSchemaVersion)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	store := storage.NewNetworkExceptionStore(db)
	ops := storage.NewManagementStore(db)
	policy := tool.NewWhitelistPolicy(tool.PolicyOptions{Root: t.TempDir()})
	logger, logs := newCapturedLogger()

	deps := NetworkExceptionDeps{
		Store:      store,
		Operations: ops,
		Policy:     policy,
		Logger:     logger,
		Now:        exceptionClock,
	}
	if mutate != nil {
		mutate(&deps)
	}
	if deps.Store == nil {
		deps.Store = store
	}
	if deps.Operations == nil {
		deps.Operations = ops
	}
	return &exceptionEnv{
		service: NewNetworkExceptionService(deps),
		store:   store,
		ops:     ops,
		policy:  policy,
		logs:    logs,
	}
}

// exceptionActor 是夹具固定的管理凭据主体。
func exceptionActor() core.Actor {
	return core.Actor{Subject: "token:abc123", Method: "bearer", Source: "127.0.0.1:5000"}
}

// pendingRows 读回全部 pending 操作行（断言"零操作行"用）。
func (e *exceptionEnv) pendingRows(t *testing.T) []core.OperationRecord {
	t.Helper()
	pending, err := e.ops.PendingOperations(context.Background())
	require.NoError(t, err)
	return pending
}

// operationRow 读回一行管理操作；不存在时返回 exists=false。
func (e *exceptionEnv) operationRow(t *testing.T, operationID string) (core.OperationRecord, bool) {
	t.Helper()
	rec, err := e.ops.GetOperation(context.Background(), operationID)
	if err != nil {
		return core.OperationRecord{}, false
	}
	return rec, true
}

// TestExecutionProfile_NetworkException 覆盖 D-13 的授权面与审计面：理由必填且进
// 审计、云元数据地址永不放行、重复登记 409 且不改既有理由、集合 ETag 是删除的唯一
// 条件、删除后下一次判定即失效（策略实例与守门器快照同一次 apply 替换）。
// Acceptance point (D13): the note is mandatory and audited, metadata addresses are
// refused, duplicates conflict, and a delete binds the very next judgement.
func TestExecutionProfile_NetworkException(t *testing.T) {
	t.Cleanup(func() { processExceptions.Replace(nil) })

	t.Run("reason_required_and_audited", func(t *testing.T) {
		env := newExceptionEnv(t, nil)

		// 理由缺失 / 全空白：400 且零写入、零操作行（放行必须带理由）。
		for _, note := range []string{"", "   \n\t "} {
			_, operationID, err := env.service.Create(context.Background(),
				core.NetworkExceptionKindHost, "api.example.com", note, exceptionActor())
			require.Error(t, err)
			var invalid *core.InvalidInputError
			require.ErrorAs(t, err, &invalid)
			require.Empty(t, operationID)
		}
		require.Empty(t, env.pendingRows(t))
		require.Empty(t, env.service.List())

		// 理由超长（按 rune 计）同样拒绝。
		_, _, err := env.service.Create(context.Background(), core.NetworkExceptionKindHost,
			"api.example.com", strings.Repeat("长", core.NetworkExceptionNoteMaxLength+1), exceptionActor())
		var invalid *core.InvalidInputError
		require.ErrorAs(t, err, &invalid)

		// 正常登记：值被规整、理由进审计、三处消费面同一次 apply 生效。
		exception, operationID, err := env.service.Create(context.Background(),
			core.NetworkExceptionKindHost, " API.Example.com ", "  内网财务系统联调  ", exceptionActor())
		require.NoError(t, err)
		require.NotEmpty(t, operationID)
		require.Regexp(t, `^exc_[0-9A-HJKMNP-TV-Z]{26}$`, exception.ExceptionID)
		require.Equal(t, "api.example.com", exception.Value)
		require.Equal(t, "内网财务系统联调", exception.Note)
		require.Equal(t, int64(1), exception.Revision)
		require.Equal(t, "token:abc123", exception.CreatedBy)
		require.Equal(t, operationID, exception.OperationID)

		live := env.service.List()
		require.Len(t, live, 1)
		require.Equal(t, exception.ExceptionID, live[0].ExceptionID)
		require.Equal(t, live, env.policy.NetworkExceptions(), "策略实例必须看到同一次写入")
		require.Equal(t, live, processExceptions.Snapshot(), "守门器快照必须看到同一次写入")

		rec, exists := env.operationRow(t, operationID)
		require.True(t, exists)
		require.Equal(t, core.ActionNetworkExceptionCreate, rec.Action)
		require.Equal(t, core.TargetKindNetworkException, rec.TargetKind)
		require.Equal(t, exception.ExceptionID, rec.TargetID)
		require.Equal(t, "token:abc123", rec.ActorSubject)
		require.Equal(t, core.OperationSuccess, rec.Status)
		require.Equal(t, "OK", rec.ResultCode)
		require.Contains(t, string(rec.DetailJSON), `"kind":"host"`)
		require.Contains(t, string(rec.DetailJSON), `"value":"api.example.com"`)
		require.Contains(t, string(rec.DetailJSON), "内网财务系统联调", "放行理由属审计内容")
	})

	t.Run("cloud_metadata_never_excepted", func(t *testing.T) {
		env := newExceptionEnv(t, nil)
		for _, value := range []string{"169.254.169.254:80", "[fd00:ec2::254]:443"} {
			_, operationID, err := env.service.Create(context.Background(),
				core.NetworkExceptionKindIPPort, value, "想放行云元数据", exceptionActor())
			require.Error(t, err, "云元数据地址任何档位都不放行，也不得登记：%s", value)
			var invalid *core.InvalidInputError
			require.ErrorAs(t, err, &invalid)
			require.Empty(t, operationID)
		}
		require.Empty(t, env.pendingRows(t), "400 发生在预写之前，不得留下操作行")
		require.Empty(t, env.service.List())
		require.Empty(t, env.policy.NetworkExceptions())
	})

	t.Run("duplicate_does_not_overwrite_reason", func(t *testing.T) {
		env := newExceptionEnv(t, nil)
		first, _, err := env.service.Create(context.Background(),
			core.NetworkExceptionKindIPPort, "10.0.0.5:5432", "第一次的理由", exceptionActor())
		require.NoError(t, err)

		_, operationID, err := env.service.Create(context.Background(),
			core.NetworkExceptionKindIPPort, "10.0.0.5:5432", "第二条理由", exceptionActor())
		require.ErrorIs(t, err, core.ErrConflict)
		require.Empty(t, operationID, "重复登记在预写之前就拒绝，没有操作行")

		live := env.service.List()
		require.Len(t, live, 1)
		require.Equal(t, first.ExceptionID, live[0].ExceptionID)
		require.Equal(t, "第一次的理由", live[0].Note, "重复登记不得覆盖既有理由")

		pending := env.pendingRows(t)
		require.Empty(t, pending)
	})

	t.Run("delete_fenced_by_collection_etag", func(t *testing.T) {
		env := newExceptionEnv(t, nil)
		exception, _, err := env.service.Create(context.Background(),
			core.NetworkExceptionKindHost, "intranet.example.com", "联调", exceptionActor())
		require.NoError(t, err)

		// 陈旧 / 非法 ETag：412 且零写入、零操作行，行仍在。
		for _, ifMatch := range []string{"", `"sha256:0000"`} {
			_, err := env.service.Delete(context.Background(), exception.ExceptionID, ifMatch, exceptionActor())
			require.ErrorIs(t, err, core.ErrPreconditionFailed)
		}
		require.Len(t, env.service.List(), 1)
		require.Empty(t, env.pendingRows(t))

		// 正确 ETag：删除后列表、策略实例与守门器快照**同时**清空。
		etag := env.service.ETag()
		operationID, err := env.service.Delete(context.Background(), exception.ExceptionID, etag, exceptionActor())
		require.NoError(t, err)
		require.NotEmpty(t, operationID)
		require.Empty(t, env.service.List())
		require.Empty(t, env.policy.NetworkExceptions(), "删除后下一次判定即失效")
		require.Empty(t, processExceptions.Snapshot())

		rec, exists := env.operationRow(t, operationID)
		require.True(t, exists)
		require.Equal(t, core.ActionNetworkExceptionDelete, rec.Action)
		require.Equal(t, exception.ExceptionID, rec.TargetID)
		require.Equal(t, etag, rec.ExpectedETag)
		require.Equal(t, core.OperationSuccess, rec.Status)
	})

	t.Run("delete_missing_row_is_404_without_audit", func(t *testing.T) {
		env := newExceptionEnv(t, nil)
		_, err := env.service.Delete(context.Background(),
			"exc_00000000000000000000000000", env.service.ETag(), exceptionActor())
		require.ErrorIs(t, err, core.ErrResourceNotFound)
		require.Empty(t, env.pendingRows(t))
	})

	t.Run("pre_write_failure_refuses", func(t *testing.T) {
		env := newExceptionEnv(t, func(d *NetworkExceptionDeps) {
			d.Operations = failingBeginOperations{OperationStore: d.Operations, err: errors.New("审计库不可写")}
		})
		_, operationID, err := env.service.Create(context.Background(),
			core.NetworkExceptionKindHost, "api.example.com", "联调", exceptionActor())
		require.Error(t, err)
		require.Contains(t, err.Error(), "pre-write operation record failed")
		require.Empty(t, operationID)
		require.Empty(t, env.service.List(), "预写失败即拒绝执行：表里不得有行")
		require.Empty(t, env.policy.NetworkExceptions())
	})
}
