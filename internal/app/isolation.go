package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ymedword/basalt/internal/cli"
	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/isolation"
	"github.com/ymedword/basalt/internal/tool"
	"github.com/ymedword/basalt/internal/web"
)

type isolationDeps struct {
	Config           cli.IsolationConfig
	ShellConfig      cli.ShellConfig
	Policy           core.Policy
	Root, InstanceID string
	Store            core.SandboxExecutionStore
	Reader           core.SandboxExecutionReader
	Docker           tool.ContainerCLI
	ApprovedDigests  []string
	Logger           *slog.Logger
	// 以下接缝仅供同包测试，生产不注入。
	// These seams are only injected by same-package tests.
	TmpfsSize func(string) (int64, error)
	Probe     func(context.Context, *tool.ContainerRunner) error
}

type isolationRuntime struct {
	Shell     *tool.ShellTool
	RunCode   *tool.RunCodeTool
	Admission *tool.SandboxAdmission
	Status    web.IsolationView
	runners   map[string]*tool.ContainerRunner
}

func disabledIsolationView() web.IsolationView {
	return web.IsolationView{Profiles: []web.IsolationProfileView{}, Tools: map[string]string{"shell": "local"}}
}

func runtimeIsolationView(rt *heavyRuntime) web.IsolationView {
	if rt == nil || rt.isolation == nil {
		return disabledIsolationView()
	}
	return rt.isolation.Status
}

func runtimeIsolationReader(rt *heavyRuntime) core.SandboxExecutionReader {
	if rt == nil || rt.db == nil {
		return nil
	}
	return rt.db
}

func isolationView(cfg cli.IsolationConfig) web.IsolationView {
	if cfg.Backend == "" {
		return disabledIsolationView()
	}
	tools := maps.Clone(cfg.Tools)
	if tools == nil {
		tools = map[string]string{}
	}
	if tools["shell"] == "" {
		tools["shell"] = "local"
	}
	return web.IsolationView{Enabled: true, Backend: cfg.Backend, Profiles: []web.IsolationProfileView{}, Tools: tools, MaxConcurrent: cfg.MaxConcurrent, MaxTotalCPUs: cfg.MaxTotalCPUs, MaxTotalMemoryMB: cfg.MaxTotalMemoryMB, MaxTotalTempBytes: cfg.MaxTotalTempBytes, MaxTotalArtifactBytes: cfg.MaxTotalArtifactBytes}
}

func loadIsolationDigests(deps isolationDeps) ([]string, error) {
	if deps.ApprovedDigests != nil {
		return slices.Clone(deps.ApprovedDigests), nil
	}
	key, err := isolation.ReleasePublicKey()
	if err != nil {
		return nil, err
	}
	approved, err := isolation.LoadApprovedImages(deps.Config.ReleaseManifest, key)
	if err != nil {
		return nil, fmt.Errorf("image_untrusted: %w", err)
	}
	return slices.Sorted(maps.Keys(approved.Digests())), nil
}

func isolationImageDigest(image string) string {
	if _, digest, ok := strings.Cut(image, "@"); ok {
		return digest
	}
	return image
}

func probeIsolationWithDeps(ctx context.Context, deps isolationDeps) (*isolationRuntime, error) {
	rt := &isolationRuntime{Status: isolationView(deps.Config), runners: map[string]*tool.ContainerRunner{}}
	if !rt.Status.Enabled {
		return rt, nil
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if !instanceIDPattern.MatchString(deps.InstanceID) {
		return rt, errors.New("not_ready: 缺少有效实例名册")
	}
	approved, err := loadIsolationDigests(deps)
	if err != nil {
		rt.Status.UnavailableReason = "image_untrusted"
		return rt, err
	}
	readSize := deps.TmpfsSize
	if readSize == nil {
		readSize = outputTmpfsSize
	}
	size, err := readSize(deps.Config.OutputRoot)
	if err != nil {
		rt.Status.UnavailableReason = "not_ready"
		return rt, err
	}
	floor, other, err := deps.Config.TempBudgets()
	if err != nil {
		return rt, err
	}
	if err := cli.CheckOutputRootTmpfs(size, floor, other, deps.Config.MaxTotalTempBytes); err != nil {
		return rt, err
	}
	for _, name := range []string{"oci", "gvisor"} {
		profile, exists := deps.Config.Profiles[name]
		if !exists {
			continue
		}
		bound := deps.Config.Tools["shell"] == name || deps.Config.Tools["run_code"] == name
		view := web.IsolationProfileView{Name: name, Runtime: profile.Runtime, ImageDigest: isolationImageDigest(profile.Image), Bound: bound, CPUs: profile.CPUs, MemoryMB: profile.MemoryMB, PIDs: profile.PIDs, TmpfsMB: profile.TmpfsMB, TimeoutSeconds: profile.TimeoutSeconds}
		if !bound {
			view.Reason = "未绑定，未探测"
			rt.Status.Profiles = append(rt.Status.Profiles, view)
			continue
		}
		runner, err := tool.NewContainerRunner(tool.ContainerRunnerOptions{Docker: deps.Docker, Root: deps.Root, OutputRoot: deps.Config.OutputRoot, InstanceID: deps.InstanceID, Profile: name, Runtime: profile.Runtime, Image: profile.Image, ImageDigest: view.ImageDigest, ApprovedDigests: approved, Interpreters: deps.ShellConfig.Interpreters, CPUs: profile.CPUs, MemoryMB: profile.MemoryMB, PIDs: profile.PIDs, TmpfsMB: profile.TmpfsMB, OutputTmpfsMB: profile.OutputTmpfsMB, MaxOutputBytes: profile.MaxOutputBytes, RunAsUID: deps.Config.RunAsUID, RunAsGID: deps.Config.RunAsGID, PassEnv: name == "oci", EnvPassthrough: deps.ShellConfig.EnvPassthrough, Logger: deps.Logger})
		if err == nil {
			if deps.Probe != nil {
				err = deps.Probe(ctx, runner)
			} else {
				err = runner.Probe(ctx)
			}
		}
		if err != nil {
			view.Reason = "not_ready"
			rt.Status.Profiles = append(rt.Status.Profiles, view)
			rt.Status.UnavailableReason = view.Reason
			return rt, err
		}
		view.Available = true
		rt.Status.Profiles = append(rt.Status.Profiles, view)
		rt.runners[name] = runner
	}
	rt.Status.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	return rt, nil
}

func buildIsolation(ctx context.Context, deps isolationDeps) (*isolationRuntime, error) {
	if deps.Config.Backend == "" {
		return nil, nil
	}
	if deps.Store == nil || deps.Reader == nil || deps.Policy == nil {
		return nil, errors.New("not_ready: 隔离执行缺少账本、读面或授权端口")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	rt, err := probeIsolationWithDeps(ctx, deps)
	if err != nil {
		return nil, err
	}
	if len(rt.runners) == 0 {
		return nil, errors.New("not_ready: 隔离执行没有有效绑定")
	}
	if err := cleanupIsolationOrphans(ctx, rt.runners, deps); err != nil {
		return nil, err
	}
	rt.Admission = tool.NewSandboxAdmission()
	if err := rt.Admission.RefreshBaseline(ctx, deps.Store, deps.InstanceID); err != nil {
		return nil, fmt.Errorf("admission_unavailable: %w", err)
	}
	if runner := rt.runners["oci"]; runner != nil {
		profile := deps.Config.Profiles["oci"]
		ledger := tool.NewSandboxLedger(deps.Store, deps.InstanceID, "oci", "runc", isolationImageDigest(profile.Image), deps.Logger)
		rt.Shell = tool.NewShellTool(deps.Policy, runner).WithIsolation(tool.ShellIsolationOptions{Ledger: ledger, Admission: rt.Admission, Root: deps.Root, Interpreters: deps.ShellConfig.Interpreters, MaxConcurrent: deps.Config.MaxConcurrent, MaxTimeout: time.Duration(profile.TimeoutSeconds) * time.Second})
	}
	options := tool.RunCodeOptions{Policy: deps.Policy, Root: deps.Root, OutputRoot: deps.Config.OutputRoot, PublishMu: &sync.Mutex{}, Logger: deps.Logger}
	if runner := rt.runners["gvisor"]; runner != nil {
		profile := deps.Config.Profiles["gvisor"]
		options.Runner = runner
		options.Store = deps.Store
		options.Admission = rt.Admission
		options.InstanceID = deps.InstanceID
		options.Profile = "gvisor"
		options.Runtime = "runsc"
		options.ImageDigest = isolationImageDigest(profile.Image)
		options.MaxConcurrent = deps.Config.MaxConcurrent
		options.MaxTimeout = time.Duration(profile.TimeoutSeconds) * time.Second
		options.MaxCodeBytes = 65536
		options.MaxInputFiles = 16
		options.MaxInputBytes = deps.Config.MaxInputBytes
		options.MaxArtifactFiles = 16
		options.MaxArtifactBytes = int64(profile.OutputTmpfsMB) * (1 << 20)
		options.MaxTotalArtifactBytes = deps.Config.MaxTotalArtifactBytes
	}
	rt.RunCode = tool.NewRunCodeTool(options)
	return rt, nil
}
