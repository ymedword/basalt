package cli

// 本文件是两条进程模式的命令面（颗粒度文档 §3.1 cli 表 / 图 C / 注 6 / 注 7）：
// `serve` 把 HTTP 面的生命周期交给装配层注入的 Server 端口；`gateway` 是常驻进程壳
// ——调度器随重装配启动，通道族归后续 US，本阶段不启 HTTP。两条命令都**不自建信号
// 上下文**：信号由 `app.Run` 经 `App.notifyContext` 唯一一次派生，`cmd.Context()`
// 就是那个 runCtx（§3.1.18 / §3.1.20，约束 24 的单一信号源）。
// The serve and gateway process modes. Neither builds its own signal context: app.Run
// derives the single runCtx, which arrives here as cmd.Context().

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newServeCommand 组装 serve：端口缺件**点名缺件**并拒绝启动（装配缺陷不该退化成
// "进程起来了但什么也没监听"），端口在位则把命令上下文交给 Server.Run。
// newServeCommand wires the injected server port onto the command context.
//
// 信号取消由 `cmd.Context()` 承载：宽限期与在途请求的解耦发生在 web.Server 内部
// （newHTTPServer 先 WithoutCancel 再 WithCancel，约束 20）。
// The signal cancellation arrives as cmd.Context(); the grace-period decoupling lives
// inside web.Server.
func newServeCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "启动 HTTP 服务（/api/v1 核心 REST 端点）",
		Long: "启动 HTTP 服务：默认监听 127.0.0.1:8080，暴露 /api/v1 下的核心 REST 端点。\n" +
			"启动时幂等兜底初始化工作区，随后启动调度器；SIGINT / SIGTERM 触发五阶段优雅停机。\n" +
			"Start the HTTP service on 127.0.0.1:8080 by default; the scheduler starts with " +
			"the process, and SIGINT / SIGTERM triggers the five-phase graceful shutdown.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if deps.Server == nil {
				return fmt.Errorf("HTTP 服务端口未注入——serve 需要装配层注入的 Server 端口（internal/app 提供）")
			}
			return deps.Server.Run(cmd.Context())
		},
	}
}

// newGatewayCommand 组装 gateway：常驻进程——重装配（含调度器启动）完成后打一条
// INFO 明示能力归属，然后阻塞到信号为止；不启 HTTP（§6.5 未把 HTTP 列入 gateway）。
// newGatewayCommand is the resident process: one INFO after the scheduler starts, then
// block until the signal propagated through cmd.Context(); no HTTP at all.
func newGatewayCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "gateway",
		Short: "启动常驻网关（调度器已随进程启动；通道族归后续 US）",
		Long: "启动常驻网关：重装配完成后启动调度器并常驻，不启 HTTP；通道族归后续 US。\n" +
			"SIGINT / SIGTERM 正常退出。\n" +
			"Run the resident gateway: the scheduler starts with the process, no HTTP is " +
			"served, and channel families belong to a later story.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps.Logger.Info("调度器已启动；通道族归后续 US，本阶段阻塞至信号",
				"mode", "gateway")
			<-cmd.Context().Done()
			return nil
		},
	}
}
