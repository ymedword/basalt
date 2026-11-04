package cli

import (
	"encoding/json"
	"errors"

	"github.com/spf13/cobra"
)

// IsolationCheckReport 是 isolation check 的机器可读结果。
// IsolationCheckReport is the machine-readable result of isolation check.
type IsolationCheckReport struct {
	Enabled  bool                    `json:"enabled"`
	Backend  string                  `json:"backend"`
	Profiles []IsolationCheckProfile `json:"profiles"`
	Tools    map[string]string       `json:"tools"`
	Reason   string                  `json:"reason"`
}

// IsolationCheckProfile 保存一个执行档的实际探测状态。
// IsolationCheckProfile records a profile's actual probe status.
type IsolationCheckProfile struct {
	Name        string `json:"name"`
	Runtime     string `json:"runtime"`
	ImageDigest string `json:"image_digest"`
	Bound       bool   `json:"bound"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason"`
}

func newIsolationCommand(deps Deps) *cobra.Command {
	command := &cobra.Command{Use: "isolation", Short: "隔离执行运行时自检"}
	command.AddCommand(&cobra.Command{Use: "check", Short: "验证已绑定的容器执行档（不触数据库）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if deps.IsolationProbe == nil {
			return errors.New("隔离自检端口未装配")
		}
		report, probeErr := deps.IsolationProbe.CheckIsolation(cmd.Context())
		if report.Profiles == nil {
			report.Profiles = []IsolationCheckProfile{}
		}
		if report.Tools == nil {
			report.Tools = map[string]string{}
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
		return probeErr
	}})
	return command
}
