package releasekit

// 本文件实现发布制品的宿主烟测：解包、版本回显、启动、健康检查、管理台静态壳与
// 一条真实任务。证据只写这里真的跑过的检查，不预先写死检查清单。
// This file implements the host smoke for release artifacts: unpack, version echo,
// boot, health, the console shell and one real task. Evidence lists only the checks
// that actually ran.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ymedword/basalt/internal/cli"
)

const (
	smokeAgentDefault = "demo"
	smokeAddrDefault  = "127.0.0.1:8080"
	smokeTokenEnv     = "BASALT_SMOKE_ADMIN_TOKEN" // #nosec G101 -- 环境变量名，不是凭据值
	smokeTaskMessage  = "release smoke"
)

// smokeChecks 返回该档位烟测实际会执行的检查 ID；证据按此写，少跑一项就会少一行。
// smokeChecks lists the check IDs a smoke run performs for the tier; evidence follows
// this list, so a check that did not run cannot appear.
func smokeChecks(target Target) []string {
	if target.Tier == TierFull {
		return []string{"artifact-unpack", "version", "health", "mock-task"}
	}
	return []string{"artifact-unpack", "version"}
}

// RunSmoke 解包归档并核对二进制自报版本；full 档再在给定工作区启动服务、查健康、
// 开管理台并跑一条任务。任何一项失败都返回错误——没有"部分通过"的证据。
// RunSmoke unpacks the archive and checks the binary's self-reported version; for the
// full tier it also starts the service in the given workspace, checks health, opens the
// console and runs one task. Any failure is an error: no partial evidence.
func RunSmoke(ctx context.Context, pkg PackageResult, opts ReleaseOptions) error {
	version := versionFromPackage(pkg.Name)
	if version == "" {
		return fmt.Errorf("无法从归档名 %q 解析版本", pkg.Name)
	}
	dir, err := os.MkdirTemp("", "basalt-release-smoke-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := extractArchive(pkg.Archive, dir); err != nil {
		return err
	}
	binary := filepath.Join(dir, "basalt-"+version, pkg.Target.BinaryName())

	var stderr bytes.Buffer
	versionCmd := exec.CommandContext(ctx, binary, "--version") // #nosec G204 -- binary 是刚解包的本仓发布物，参数固定
	versionCmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	versionCmd.Stderr = &stderr
	out, err := versionCmd.Output()
	if err != nil {
		return fmt.Errorf("冒烟启动失败：%w\n%s", err, stderr.String())
	}
	// cobra 的 --version 输出是 "<app> version <v>"；只比对整行，避免把告警当版本。
	if got, want := strings.TrimSpace(string(out)), "basalt version "+version; got != want {
		return fmt.Errorf("归档版本回显不符：%q，期望 %q", got, want)
	}
	if pkg.Target.Tier != TierFull {
		return nil
	}
	return smokeFull(ctx, binary, version, opts)
}

func smokeFull(ctx context.Context, binary, version string, opts ReleaseOptions) error {
	if opts.Workspace == "" {
		return errors.New("full 档烟测需要 --workspace（含 basalt.yaml 的部署工作目录）")
	}
	configPath := filepath.Join(opts.Workspace, "basalt.yaml")
	if opts.DSN != "" {
		cfg, err := cli.LoadConfig(configPath, os.LookupEnv)
		if err != nil {
			return fmt.Errorf("读取 %s：%w", configPath, err)
		}
		if cfg.Database.DSN != opts.DSN {
			return errors.New("--dsn 与 basalt.yaml 的 database.dsn 不一致——灌库与迁移必须是同一个库")
		}
	}
	token := os.Getenv(opts.AdminTokenEnv)
	if opts.AdminTokenEnv == "" {
		token = os.Getenv(smokeTokenEnv)
	}
	if token == "" {
		return fmt.Errorf("full 档烟测需要管理面 token：请设置 %s", smokeTokenName(opts))
	}

	var serveOut bytes.Buffer
	serve := exec.CommandContext(ctx, binary, "serve") // #nosec G204 -- binary 是刚解包的本仓发布物，子命令固定
	serve.Dir = opts.Workspace
	serve.Env = os.Environ()
	serve.Stdout = &serveOut
	serve.Stderr = &serveOut
	if err := serve.Start(); err != nil {
		return fmt.Errorf("启动制品服务失败：%w", err)
	}
	defer func() {
		_ = serve.Process.Kill()
		_, _ = serve.Process.Wait()
	}()
	addr := opts.Addr
	if addr == "" {
		addr = smokeAddrDefault
	}
	base := "http://" + addr
	if err := waitHealthy(ctx, base+"/api/v1/health", 60*time.Second); err != nil {
		return fmt.Errorf("%w\n%s", err, serveOut.String())
	}
	if err := smokeConsole(ctx, base, token); err != nil {
		return err
	}
	return smokeTask(ctx, base, token, smokeAgent(opts))
}

func smokeTokenName(opts ReleaseOptions) string {
	if opts.AdminTokenEnv != "" {
		return opts.AdminTokenEnv
	}
	return smokeTokenEnv
}

func smokeAgent(opts ReleaseOptions) string {
	if opts.Agent != "" {
		return opts.Agent
	}
	return smokeAgentDefault
}

func smokeConsole(ctx context.Context, base, token string) error {
	body, status, err := smokeRequest(ctx, http.MethodGet, base+"/console", token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("管理台返回 HTTP %d", status)
	}
	if !strings.Contains(strings.ToLower(string(body)), "<!doctype html") {
		return errors.New("管理台首页不是 HTML 壳")
	}
	return nil
}

func smokeTask(ctx context.Context, base, token, agent string) error {
	payload, err := json.Marshal(map[string]string{"agent": agent})
	if err != nil {
		return err
	}
	body, status, err := smokeRequest(ctx, http.MethodPost, base+"/api/v1/sessions", token, payload)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("建会话返回 HTTP %d：%s", status, body)
	}
	var session struct {
		SessionID string `json:"session_id"`
	}
	if err := decodeSmokeData(body, &session); err != nil {
		return fmt.Errorf("解析建会话响应：%w", err)
	}
	if session.SessionID == "" {
		return errors.New("建会话响应缺少 session_id")
	}
	message, err := json.Marshal(map[string]string{"message": smokeTaskMessage})
	if err != nil {
		return err
	}
	body, status, err = smokeRequest(ctx, http.MethodPost,
		base+"/api/v1/sessions/"+session.SessionID+"/messages", token, message)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("烟测任务返回 HTTP %d：%s", status, body)
	}
	var run struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := decodeSmokeData(body, &run); err != nil {
		return fmt.Errorf("解析烟测任务响应：%w", err)
	}
	if run.Status != "completed" {
		return fmt.Errorf("烟测任务状态 %q，期望 completed（reason %q）", run.Status, run.Reason)
	}
	return nil
}

// decodeSmokeData 取信封里的 data；缺失即错误，避免把空响应当成功。
func decodeSmokeData(body []byte, dst any) error {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("响应缺少 data")
	}
	return json.Unmarshal(envelope.Data, dst)
}

func smokeRequest(ctx context.Context, method, url, token string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 90 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, response.StatusCode, err
	}
	return raw, response.StatusCode, nil
}
