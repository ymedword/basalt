package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/builtin"
	"github.com/ymedword/basalt/internal/core"
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
	group.AddCommand(newDemoWorkflowPrepareCommand(deps))
	return group
}

// newDemoWorkflowPrepareCommand 是 `demo workflow prepare`：计划总是执行（只发 GET），
// `--yes` 才应用。端口缺失是装配错误——不得"没注入就跳过"。
// The command always plans (GET only) and only applies with --yes; a missing port is a
// wiring error, never a silent skip.
func newDemoWorkflowPrepareCommand(deps Deps) *cobra.Command {
	var (
		server    string
		tokenFile string
		yes       bool
	)
	command := &cobra.Command{
		Use: "prepare", Short: "预演并写入两个 Agent 的只读知识绑定（创建集合、摄取 9 个文档）", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDemoWorkflowPrepare(cmd.Context(), cmd.OutOrStdout(), deps,
				DemoPrepareRequest{Server: server, TokenFile: tokenFile}, yes)
		},
	}
	command.Flags().StringVar(&server, "server", "", "管理面地址（缺省从配置 http.addr 派生；远端须用 https）")
	command.Flags().StringVar(&tokenFile, "token-file", "", "管理面凭据文件（缺省读配置或实例私有 admin-token；不会生成新凭据）")
	command.Flags().BoolVar(&yes, "yes", false, "确认应用：创建集合、摄取文档并改写两个 Agent 的 knowledge 绑定")
	return command
}

// runDemoWorkflowPrepare 运行计划与应用两段：逐项输出每个集合、文档与 Agent 的结果，
// **不**凭 HTTP 200 或退出码 0 判成功。部分成功（至少一个 Agent 未绑定）退出 3，
// 硬失败退出 1。
// runDemoWorkflowPrepare prints every item's outcome; partial success exits 3 and a hard
// failure exits 1, because an HTTP 200 alone is never treated as success.
func runDemoWorkflowPrepare(ctx context.Context, out io.Writer, deps Deps, req DemoPrepareRequest, yes bool) error {
	if deps.DemoPrepare == nil {
		return errors.New("demo 准备端口未注入，拒绝执行 prepare")
	}
	plan, err := deps.DemoPrepare.Plan(ctx, req)
	if err != nil {
		return err
	}
	if err := printDemoPreparePlan(out, plan); err != nil {
		return err
	}
	if !yes {
		_, err := fmt.Fprintln(out, "只读预演：未执行任何写入。确认无误后加 --yes 应用（会创建集合、摄取文档并改写两个 Agent 的 knowledge 绑定，摄取可能产生 embedding 费用）。")
		return err
	}
	result, applyErr := deps.DemoPrepare.Apply(ctx, req, plan)
	if err := printDemoPrepareResult(out, result); err != nil {
		return err
	}
	if applyErr != nil {
		return applyErr
	}
	if result.Partial {
		return WithExitCode(3, errors.New("部分成功：至少一个 Agent 未绑定（见上逐项结果）；修正后重跑只补失败项"))
	}
	_, err = fmt.Fprintln(out, "全部完成：两个集合、9 个文档与两个 Agent 的只读绑定均已在位。")
	return err
}

// printDemoPreparePlan 输出只读预演：集合 / 文档 / 绑定的逐项动作与费用提示。
// printDemoPreparePlan prints the plan item by item.
func printDemoPreparePlan(out io.Writer, plan DemoPreparePlan) error {
	if _, err := fmt.Fprintf(out, "server: %s\n", plan.Server); err != nil {
		return err
	}
	if workflowDir := strings.TrimSpace(plan.WorkflowDir); workflowDir != "" {
		if _, err := fmt.Fprintf(out, "workflow_dir: %s\n", workflowDir); err != nil {
			return err
		}
	}
	for _, collection := range plan.Collections {
		if _, err := fmt.Fprintf(out, "collection %s: %s%s\n", collection.Name, collection.Action, demoCollectionSuffix(collection.CollectionID)); err != nil {
			return err
		}
	}
	for _, document := range plan.Documents {
		if _, err := fmt.Fprintf(out, "document %s %s: %s\n", document.Collection, document.Path, document.Action); err != nil {
			return err
		}
	}
	for _, binding := range plan.Bindings {
		if _, err := fmt.Fprintf(out, "binding %s: %s%s\n", binding.Agent, binding.Action, demoBindingSuffix(binding.Knowledge)); err != nil {
			return err
		}
	}
	for _, warning := range plan.Warnings {
		if _, err := fmt.Fprintf(out, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

// printDemoPrepareResult 输出应用结果：逐项状态来自服务端或条件写判定，不是 HTTP 码。
// printDemoPrepareResult prints each item's verdict, never the HTTP status code.
func printDemoPrepareResult(out io.Writer, result DemoPrepareResult) error {
	for _, collection := range result.Collections {
		state := "reused"
		if collection.Created {
			state = "created"
		}
		if _, err := fmt.Fprintf(out, "collection %s: %s (%s)\n", collection.Name, state, collection.CollectionID); err != nil {
			return err
		}
	}
	for _, document := range result.Documents {
		suffix := ""
		if document.Deduplicated {
			suffix = " (deduplicated)"
		}
		if _, err := fmt.Fprintf(out, "document %s %s: %s%s\n", document.Collection, document.Path, document.Status, suffix); err != nil {
			return err
		}
	}
	for _, binding := range result.Bindings {
		if _, err := fmt.Fprintf(out, "binding %s: %s\n", binding.Agent, binding.Status); err != nil {
			return err
		}
	}
	if result.Partial {
		_, err := fmt.Fprintln(out, "partial: true")
		return err
	}
	return nil
}

// demoCollectionSuffix 把集合 ID 渲染成输出后缀（预演时复用项才有 ID）。
// demoCollectionSuffix renders the reused collection id for the preview.
func demoCollectionSuffix(collectionID string) string {
	if strings.TrimSpace(collectionID) == "" {
		return ""
	}
	return " (" + collectionID + ")"
}

// demoBindingSuffix 把目标绑定渲染成 ` → name(read), name2(read)`。
// demoBindingSuffix renders the wanted bindings for the preview.
func demoBindingSuffix(bindings []core.KnowledgeBinding) string {
	if len(bindings) == 0 {
		return ""
	}
	parts := make([]string, len(bindings))
	for i, binding := range bindings {
		parts[i] = fmt.Sprintf("%s(%s)", binding.Name, binding.Access)
	}
	return " → " + strings.Join(parts, ", ")
}
