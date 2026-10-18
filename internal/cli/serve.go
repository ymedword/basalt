package cli

// 本文件是两条进程模式的命令面（颗粒度文档 §3.1 cli 表 / 图 C / 注 6 / 注 7）：
// `serve` 把 HTTP 面的生命周期交给装配层注入的 Server 端口，自身只负责信号接线；
// `gateway` 本阶段只交付进程壳——打一条占位 INFO 后阻塞至信号，**不启 HTTP**，
// 也不伪造调度能力（调度器 / 通道族归 US-10 / US-11）。
// The serve and gateway process modes. serve only wires signals around the injected
// server port; gateway has no HTTP and no schedulers yet, so it blocks after one INFO.

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

// newServeCommand 组装 serve：端口缺件**点名缺件**并拒绝启动（装配缺陷不该退化成
// "进程起来了但什么也没监听"），端口在位则把信号上下文交给 Server.Run。
// newServeCommand wires SIGINT / SIGTERM around the injected server port.
//
// signal.NotifyContext 的 ctx 只承载取消：宽限期与在途请求的解耦发生在
// web.Server 内部（newHTTPServer 先 WithoutCancel 再 WithCancel，约束 20）。
// The signal context carries cancellation only; the grace-period decoupling lives
// inside web.Server.
func newServeCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "启动 HTTP 服务（/api/v1 核心 REST 端点）",
		Long: "启动 HTTP 服务：默认监听 127.0.0.1:8080，暴露 /api/v1 下的核心 REST 端点。\n" +
			"启动时幂等兜底初始化工作区；SIGINT / SIGTERM 触发五阶段优雅停机。\n" +
			"Start the HTTP service on 127.0.0.1:8080 by default; SIGINT / SIGTERM " +
			"triggers the five-phase graceful shutdown.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deps.Server == nil {
				return fmt.Errorf("HTTP 服务端口未注入——serve 需要装配层注入的 Server 端口（internal/app 提供）")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return deps.Server.Run(ctx)
		},
	}
}

// newGatewayCommand 组装 gateway：本阶段是**占位常驻**——占位 INFO 明示能力归属，
// 然后阻塞到信号为止；不启 HTTP（§6.5 未把 HTTP 列入 gateway）。
// newGatewayCommand is the placeholder resident process: one INFO, then block until
// a signal; no HTTP at all.
func newGatewayCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "gateway",
		Short: "启动常驻网关（本阶段：占位常驻，调度器 / 通道族归后续 US）",
		Long: "启动常驻网关。本阶段只做重装配与常驻：不启 HTTP，调度器与通道族分别归 " +
			"US-10 / US-11。SIGINT / SIGTERM 正常退出。\n" +
			"Run the resident gateway; this phase only keeps the process alive after the " +
			"heavy assembly. Schedulers and channel families belong to later stories.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps.Logger.Info("gateway 已启动：调度器与通道族归 US-10 / US-11，本阶段占位常驻（不启 HTTP）",
				"mode", "gateway")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			<-ctx.Done()
			return nil
		},
	}
}
