package cli

// 本文件是两条进程模式命令面的验收（颗粒度文档 §3.2 cli/serve_test.go 四条 / R12）：
// 端口缺件如实报错、运行错误原样上抛、gateway 在信号之前**不退出**、根命令注册了两条
// 新命令。判据只驱动**命令面**——HTTP 面与装配属于 internal/web 与 internal/app 的
// 验收面，这里不重复判。
// The command-surface acceptance for the two process modes: the missing-port failure,
// error propagation, gateway's blocking behaviour, and root registration.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeServer 是 Server 端口的假实现：Run 直接返回注入的错误（记录调用次数）。
// A fake server port whose Run returns the injected error and counts calls.
type fakeServer struct {
	runErr error
	calls  int
}

// Run 立即返回注入的错误——命令面的职责只有"把 ctx 交给端口、把错误往外传"。
func (f *fakeServer) Run(context.Context) error {
	f.calls++
	return f.runErr
}

// quietLogger 是丢弃输出的日志器（命令面测试不判日志内容）。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestServeCommand_RequiresServer 验收端口缺件：nil Server 必须得到可读错误，
// 而不是进程起来后什么也没监听。逆改点：删掉缺件分支 → 空指针 panic 或静默 0 退出。
func TestServeCommand_RequiresServer(t *testing.T) {
	cmd := newServeCommand(Deps{Logger: quietLogger()})
	err := cmd.ExecuteContext(context.Background())
	require.Error(t, err, "Deps.Server 为 nil 时必须拒绝启动")
	require.Contains(t, err.Error(), "未注入", "错误文案要点名缺的是端口注入")
}

// TestServeCommand_PropagatesRunError 验收错误上抛：Server.Run 的错误必须原样返回
// （命令面不改写、不吞掉）。逆改点：吞掉错误返回 nil → ErrorIs 断言红。
func TestServeCommand_PropagatesRunError(t *testing.T) {
	wantErr := errors.New("serve failed")
	srv := &fakeServer{runErr: wantErr}

	cmd := newServeCommand(Deps{Logger: quietLogger(), Server: srv})
	err := cmd.ExecuteContext(context.Background())

	require.ErrorIs(t, err, wantErr, "运行错误必须原样上抛")
	require.Equal(t, 1, srv.calls, "Server.Run 恰好调用一次")
}

// TestGatewayCommand_BlocksUntilSignal 验收进程壳语义（R12 逐字）：取消之前必须阻塞
// ——命令"启动即退出"会让 gateway 看起来可用却没有常驻；取消之后返回 nil。
// 逆改点：RunE 直接 return nil → 第一个子用例在 50ms 内收到退出，断言红。
func TestGatewayCommand_BlocksUntilSignal(t *testing.T) {
	deps := Deps{Logger: quietLogger()}

	t.Run("取消前阻塞", func(t *testing.T) {
		cmd := newGatewayCommand(deps)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- cmd.ExecuteContext(ctx) }()

		select {
		case err := <-done:
			t.Fatalf("gateway 在信号之前就退出了: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("取消上下文后 gateway 未退出")
		}
	})

	t.Run("取消后返回 nil", func(t *testing.T) {
		cmd := newGatewayCommand(deps)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := cmd.ExecuteContext(ctx)
		require.NoError(t, err, "信号是正常终止，不是错误")
	})
}

// TestRootCommand_RegistersServeAndGateway 验收根命令的注册点：两条命令都能被
// 名字找到。逆改点：删掉 root.AddCommand 里的一行 → 对应断言红。
func TestRootCommand_RegistersServeAndGateway(t *testing.T) {
	root := NewRootCommand(Deps{Logger: quietLogger()})

	names := make(map[string]bool, len(root.Commands()))
	for _, sub := range root.Commands() {
		names[sub.Name()] = true
	}
	require.True(t, names["serve"], "根命令必须注册 serve")
	require.True(t, names["gateway"], "根命令必须注册 gateway")
}
