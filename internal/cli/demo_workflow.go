package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/builtin"
)

// 工作流素材与启动准备共用安装器；命令不构造运行时或模型客户端。
// The explicit workflow installer shares startup assets without constructing a runtime.
func newDemoWorkflowCommand(deps Deps) *cobra.Command {
	group := &cobra.Command{Use: "workflow", Short: "采购工作流示例（四角色、17 节点、48 条需求）"}
	install := &cobra.Command{
		Use: "install", Short: "准备默认工作流素材；保留已有内容，不自动启用或执行", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			root := filepath.Join(deps.WorkDir, workspaceDirName)
			if _, err := os.Stat(filepath.Join(root, agentsDirName)); err != nil {
				return fmt.Errorf("工作区尚未初始化，请先执行 basalt init: %w", err)
			}
			if deps.AcquireWorkspaceLock == nil {
				return errors.New("工作区锁端口未注入，拒绝安装工作流素材")
			}
			release, err := deps.AcquireWorkspaceLock(root)
			if err != nil {
				return err
			}
			if release == nil {
				return errors.New("工作区锁端口未返回释放句柄，拒绝安装工作流素材")
			}
			defer func() { err = errors.Join(err, release()) }()
			results, err := builtin.InstallWorkflow(root, nil)
			if err != nil {
				return err
			}
			for _, result := range results {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", result.Name, result.State); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "素材位于 .basalt/samples/bid-readiness；按 SETUP.md 配置模型、知识库和确认记忆，再启用 Agent。管理台工作流中载入模板、校验后发布，任务须手动启动。安装没有调用模型。")
			return err
		},
	}
	group.AddCommand(install)
	return group
}
