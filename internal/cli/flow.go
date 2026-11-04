package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// FlowRequest is the CLI consumer request; a credential path, never its contents,
// crosses this port. The implementation is a client of the already serving instance.
type FlowRequest struct {
	Operation, Server, TokenFile, ID, InputFile, IdempotencyKey, ETag, Reason, OperationID string
	Cursor                                                                                 string
	Limit                                                                                  int
}

// FlowClient keeps the command surface independent from HTTP and runtime wiring.
type FlowClient interface {
	Request(context.Context, FlowRequest) (json.RawMessage, error)
	ValidateStructure(context.Context, string) (json.RawMessage, error)
}

func newFlowCommand(deps Deps) *cobra.Command {
	group := &cobra.Command{Use: "flow", Short: "持久流程：离线校验或连接已运行实例", Args: cobra.NoArgs}
	var server, tokenFile string
	var emitBundle bool
	group.PersistentFlags().StringVar(&server, "server", "", "已运行实例地址（远端使用 HTTPS）")
	group.PersistentFlags().StringVar(&tokenFile, "token-file", "", "受限凭据文件；不接受命令行明文 token")
	validate := &cobra.Command{Use: "validate <dir>", Short: "只校验本地结构；发布资格仍由实例核对", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if deps.Flow == nil {
			return errors.New("流程客户端未装配")
		}
		data, err := deps.Flow.ValidateStructure(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(data, &result); err != nil {
			return errors.New("离线校验结果无效")
		}
		if emitBundle {
			data = result["definition"]
			if len(data) == 0 {
				return errors.New("未获得完整定义bundle")
			}
		} else {
			delete(result, "definition")
			data, err = json.Marshal(result)
			if err != nil {
				return errors.New("无法编码校验结果")
			}
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}}
	validate.Flags().BoolVar(&emitBundle, "bundle", false, "结构校验成功后将完整 JSON bundle 输出到标准输出，供管理台导入；不证明可发布")
	group.AddCommand(validate)
	for _, operation := range []string{"list", "start", "status", "cancel", "resume"} {
		req := FlowRequest{Operation: operation, Limit: 50}
		use, short := operation, "查询当前可用流程定义"
		args := cobra.NoArgs
		switch operation {
		case "start":
			use += " <definition-uid>"
			short = "持久受理任务；返回不代表任务已完成"
			args = cobra.ExactArgs(1)
		case "status":
			use += " <task-id>"
			short = "读取任务状态，不触发恢复"
			args = cobra.ExactArgs(1)
		case "cancel":
			use += " <task-id>"
			short = "按完整版本请求取消，等待真实执行收束"
			args = cobra.ExactArgs(1)
		case "resume":
			use += " <task-id>"
			short = "核对原权限及动作证据后恢复；未知副作用不重放"
			args = cobra.ExactArgs(1)
		}
		command := &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			if deps.Flow == nil {
				return errors.New("流程客户端未装配")
			}
			if strings.TrimSpace(server) == "" || strings.TrimSpace(tokenFile) == "" {
				return errors.New("请设置 --server 和 --token-file；本命令不会启动第二个实例")
			}
			req.Server, req.TokenFile = server, tokenFile
			if len(args) > 0 {
				req.ID = args[0]
			}
			if req.Operation == "start" {
				if req.InputFile == "" {
					return errors.New("start 需要 --input-file 指定结构化 JSON")
				}
				if req.IdempotencyKey == "" {
					var entropy [16]byte
					if _, err := rand.Read(entropy[:]); err != nil {
						return errors.New("无法生成幂等标识")
					}
					req.IdempotencyKey = "flow-cli-" + hex.EncodeToString(entropy[:])
				}
				// Print before dispatch so an ambiguous network result can be
				// reconciled with the same key instead of creating a second task.
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Idempotency-Key: "+req.IdempotencyKey); err != nil {
					return err
				}
			}
			if req.Operation == "cancel" || req.Operation == "resume" {
				if req.ETag == "" || strings.TrimSpace(req.Reason) == "" {
					return errors.New("控制操作需要 --if-match、--reason；请先读取 status")
				}
				if req.OperationID == "" {
					var entropy [16]byte
					if _, err := rand.Read(entropy[:]); err != nil {
						return errors.New("无法生成操作标识")
					}
					req.OperationID = "flow-cli-" + hex.EncodeToString(entropy[:])
				}
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Operation-ID: "+req.OperationID); err != nil {
					return err
				}
			}
			data, err := deps.Flow.Request(cmd.Context(), req)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(data)); err != nil {
				return err
			}
			if req.Operation == "status" {
				var result struct {
					Status string `json:"status"`
				}
				if err := json.Unmarshal(data, &result); err != nil {
					return errors.New("实例返回的任务状态无效")
				}
				if result.Status == "failed" || result.Status == "stopped" || result.Status == "cancelled" || result.Status == "abandoned" {
					return errors.New("任务已停止或未通过验收；请查看状态中的原因")
				}
				if result.Status != "queued" && result.Status != "running" && result.Status != "blocked" && result.Status != "succeeded" && result.Status != "cancelling" {
					return errors.New("实例返回了无法识别的任务状态，不视为业务成功")
				}
			}
			return nil
		}}
		if operation == "list" || operation == "status" {
			command.Flags().StringVar(&req.Cursor, "cursor", "", "下一页游标")
			command.Flags().IntVar(&req.Limit, "limit", 50, "分页条数（1–100）")
		}
		if operation == "start" {
			command.Flags().StringVar(&req.InputFile, "input-file", "", "输入 JSON 普通文件")
			command.Flags().StringVar(&req.IdempotencyKey, "idempotency-key", "", "重试必须复用首次标识")
		}
		if operation == "cancel" || operation == "resume" {
			command.Flags().StringVar(&req.ETag, "if-match", "", "status 返回的完整 ETag")
			command.Flags().StringVar(&req.Reason, "reason", "", "操作理由")
			command.Flags().StringVar(&req.OperationID, "operation-id", "", "重试复用首次操作标识")
		}
		group.AddCommand(command)
	}
	return group
}
