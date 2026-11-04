package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"

	"github.com/ymedword/basalt/internal/cli"
)

type isolationProbePort struct {
	Config    *cli.Config
	ConfigErr error
	Root      string
	Logger    *slog.Logger
}

func (p isolationProbePort) CheckIsolation(ctx context.Context) (cli.IsolationCheckReport, error) {
	report := cli.IsolationCheckReport{Profiles: []cli.IsolationCheckProfile{}, Tools: map[string]string{"shell": "local"}, Reason: "隔离未配置；shell 为本机受控进程，run_code 拒绝执行"}
	if p.ConfigErr != nil && !errors.Is(p.ConfigErr, fs.ErrNotExist) {
		return report, p.ConfigErr
	}
	if p.Config == nil || p.Config.Isolation.Backend == "" {
		return report, nil
	}
	report.Enabled = true
	report.Backend = p.Config.Isolation.Backend
	report.Reason = "not_ready"
	marker, present, err := readInstanceMarkerAt(p.Root)
	if err != nil {
		return report, err
	}
	if !present {
		return report, errInstanceIdentityMissing
	}
	rt, err := probeIsolationWithDeps(ctx, isolationDeps{Config: p.Config.Isolation, ShellConfig: p.Config.Shell, Root: p.Root, InstanceID: marker.InstanceID, Logger: p.Logger})
	if rt == nil {
		return report, err
	}
	report.Tools = rt.Status.Tools
	report.Reason = rt.Status.UnavailableReason
	for _, profile := range rt.Status.Profiles {
		report.Profiles = append(report.Profiles, cli.IsolationCheckProfile{Name: profile.Name, Runtime: profile.Runtime, ImageDigest: profile.ImageDigest, Bound: profile.Bound, Available: profile.Available, Reason: profile.Reason})
	}
	if err != nil && report.Reason == "" {
		report.Reason = "not_ready"
	}
	return report, err
}
