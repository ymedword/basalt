package cli

// 本文件是 Worker 登记行的只读命令面（US-24 模块卡 §3.1.12）：
//
//	basalt workers list
//
// 三条纪律：
//  1. **只读**：不取实例锁、不写任何行、不改心跳——服务运行中也能执行；命令面
//     拿到 nil 端口即报缺件，**不得**自己打开数据库补出第二条读面。
//  2. **身份是 InstanceID**：`worker_nodes.worker_name` 允许重复（两台机器同名是
//     运维常态），因此列表以 instance 为首列，name 只作诊断。
//  3. **心跳不是占有权证明**：`heartbeat` 列与 `active leases` 列来自两处事实
//     （worker_nodes 时间戳 / execution_leases 的有效租约），不得互相推断生死。
//
// The read-only worker listing. It takes no lock, writes nothing and never infers
// liveness from a heartbeat; the instance id is the identity, the name is diagnostic.

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ymedword/basalt/internal/core"
)

// requireWorkerLister 取只读 Worker 端口；缺失即报缺件。
// requireWorkerLister reports a wiring gap instead of opening a database.
func requireWorkerLister(deps Deps, command string) (WorkerLister, error) {
	if deps.Workers == nil {
		e := fmt.Errorf("未注入 Worker 端口——%s 需要装配层注入的 WorkerLister（internal/app 提供）", command)
		deps.Logger.Error(command + " 未通过：Worker 端口未注入")
		return nil, e
	}
	return deps.Workers, nil
}

// newWorkersCommand 组装 `workers` 命令组（当前只有只读的 list）。
// newWorkersCommand groups the worker commands; only the read-only list exists.
func newWorkersCommand(deps Deps) *cobra.Command {
	group := &cobra.Command{
		Use:   "workers",
		Short: "Worker 实例与活跃租约（只读诊断）",
		Long: "列出已登记的 Worker 实例：实例标识、名称、启动与心跳时间、当前活跃租约数。\n" +
			"全部只读，服务运行中也能执行；心跳只作诊断，不参与任何归属判定。\n" +
			"Lists registered worker instances and their active lease counts, read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	group.AddCommand(newWorkersListCommand(deps))
	return group
}

// newWorkersListCommand 组装 `workers list`。
// newWorkersListCommand builds the read-only listing.
func newWorkersListCommand(deps Deps) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "list",
		Short: "列出已登记 Worker 实例与活跃租约数（只读）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			port, err := requireWorkerLister(deps, "workers list")
			if err != nil {
				return err
			}
			nodes, err := port.ListNodes(cmd.Context())
			if err != nil {
				deps.Logger.Error("workers list 未通过", "error", err)
				return err
			}
			if asJSON {
				return writeAdmissionJSON(cmd, workerNodesJSONView(nodes))
			}
			out := cmd.OutOrStdout()
			if len(nodes) == 0 {
				_, err := fmt.Fprintln(out, "（没有已登记的 Worker 实例）")
				return err
			}
			table := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			if _, err := fmt.Fprintln(table, "INSTANCE\tNAME\tSTARTED\tHEARTBEAT\tACTIVE LEASES\tSTOPPED"); err != nil {
				return err
			}
			for _, node := range nodes {
				stopped := "—"
				if node.StoppedAt != nil {
					stopped = admissionTime(*node.StoppedAt)
				}
				if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%d\t%s\n",
					node.InstanceID, node.Name, admissionTime(node.StartedAt),
					admissionTime(node.LastHeartbeatAt), node.ActiveLeases, stopped); err != nil {
					return err
				}
			}
			return table.Flush()
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "以 JSON 输出（机器可读）")
	return command
}

// workerNodeJSONView 是 `workers list --json` 的投影：时间一律 UTC RFC3339，
// 未停机时 `stopped_at` 为 null。字段名与系统概览的 Worker 列表同源同形。
// workerNodeJSONView is the machine-readable projection; the keys match the overview.
type workerNodeJSONView struct {
	Instance      string `json:"instance"`
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	BuildVersion  string `json:"build_version"`
	SchemaVersion int    `json:"schema_version"`
	StartedAt     string `json:"started_at"`
	HeartbeatAt   string `json:"heartbeat_at"`
	ActiveLeases  int64  `json:"active_leases"`
	StoppedAt     string `json:"stopped_at,omitempty"`
}

// workerNodesJSONView 把投影批量转成 JSON 形态；永远是数组（没有实例时为 `[]`）。
// workerNodesJSONView returns always an array.
func workerNodesJSONView(nodes []core.WorkerNodeView) []workerNodeJSONView {
	out := make([]workerNodeJSONView, 0, len(nodes))
	for _, node := range nodes {
		view := workerNodeJSONView{
			Instance:      node.InstanceID,
			Name:          node.Name,
			Hostname:      node.Hostname,
			BuildVersion:  node.BuildVersion,
			SchemaVersion: node.SchemaVersion,
			StartedAt:     admissionTime(node.StartedAt),
			HeartbeatAt:   admissionTime(node.LastHeartbeatAt),
			ActiveLeases:  node.ActiveLeases,
		}
		if node.StoppedAt != nil {
			view.StoppedAt = admissionTime(*node.StoppedAt)
		}
		out = append(out, view)
	}
	return out
}
