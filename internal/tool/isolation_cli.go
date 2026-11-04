package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContainerCLI 是容器操作端口；不会运行代理循环或执行工具调度。
// ContainerCLI is the container operations port, never an agent loop or tool scheduler.
type ContainerCLI interface {
	Version(context.Context) (string, map[string]bool, error)
	ImageInspect(context.Context, string) ([]string, string, error)
	Create(context.Context, []string) (string, error)
	Start(context.Context, string) error
	Wait(context.Context, string) (int, error)
	Logs(context.Context, string, int) (string, bool, error)
	Remove(context.Context, string) error
	ListByLabel(context.Context, string) ([]string, error)
}

// DockerCLI 通过有界子进程调用本机 Docker daemon；不加载 Docker SDK。
// DockerCLI invokes a local Docker daemon using bounded processes without a Docker SDK.
type DockerCLI struct {
	Path    string
	once    sync.Once
	host    string
	hostErr error
}

// NewDockerCLI 构造 CLI 客户端，不发出调用。
// NewDockerCLI constructs a client without issuing calls.
func NewDockerCLI(path string) ContainerCLI { return &DockerCLI{Path: path} }

// boundedCLIOutput 在写入时限流并继续排空，避免 stdout 或 stderr 洪泛撑大内存。
// boundedCLIOutput caps retained output while draining both streams.
type boundedCLIOutput struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (b *boundedCLIOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := min(len(p), b.limit-len(b.data))
	b.data = append(b.data, p[:n]...)
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}

func (d *DockerCLI) raw(ctx context.Context, limit int, args ...string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, d.Path, args...) // #nosec G204 -- 固定 CLI 及分离 argv；不通过 shell
	cmd.WaitDelay = time.Second
	// 明确 host 后去掉环境中的 context/host，避免 Docker 选择规则覆盖冻结的本机 endpoint。
	// Strip context/host overrides after the local endpoint has been frozen.
	for _, entry := range os.Environ() {
		if d.host != "" && (strings.HasPrefix(entry, "DOCKER_HOST=") || strings.HasPrefix(entry, "DOCKER_CONTEXT=")) {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	out := &boundedCLIOutput{limit: limit}
	stderr := &boundedCLIOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		operation := args[0]
		if operation == "--host" && len(args) > 2 {
			operation = args[2]
		}
		return string(out.data), out.truncated, fmt.Errorf("docker %s: %w", operation, err)
	}
	return string(out.data), out.truncated, nil
}

func (d *DockerCLI) localHost(ctx context.Context) error {
	d.once.Do(func() {
		if runtime.GOOS != "linux" {
			d.hostErr = errors.New("容器执行要求 Linux 本机 daemon")
			return
		}
		host := os.Getenv("DOCKER_HOST")
		if os.Getenv("DOCKER_CONTEXT") != "" || host == "" {
			probe, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			body, truncated, err := d.raw(probe, 1<<20, "context", "inspect")
			if err != nil {
				d.hostErr = err
				return
			}
			var contexts []struct {
				Endpoints map[string]struct{ Host string }
			}
			if truncated || json.Unmarshal([]byte(body), &contexts) != nil || len(contexts) != 1 {
				d.hostErr = errors.New("docker context 读取无效")
				return
			}
			host = contexts[0].Endpoints["docker"].Host
		}
		if !strings.HasPrefix(host, "unix:///") || strings.ContainsAny(host, "\r\n") {
			d.hostErr = errors.New("拒绝远程 Docker daemon：绑定路径必须与服务同机")
			return
		}
		d.host = host
	})
	return d.hostErr
}

func (d *DockerCLI) call(ctx context.Context, limit int, args ...string) (string, bool, error) {
	if err := d.localHost(ctx); err != nil {
		return "", false, err
	}
	return d.raw(ctx, limit, append([]string{"--host", d.host}, args...)...)
}

func (d *DockerCLI) small(ctx context.Context, args ...string) (string, error) {
	body, truncated, err := d.call(ctx, 1<<20, args...)
	if err != nil {
		return "", err
	}
	if truncated {
		return "", errors.New("docker 元数据超过读取上限")
	}
	return strings.TrimSpace(body), nil
}

// Version 返回服务版本及显式 runtime 集合。
// Version returns the server version and explicit runtime set.
func (d *DockerCLI) Version(ctx context.Context) (string, map[string]bool, error) {
	body, err := d.small(ctx, "version", "--format", "{{json .Server.Version}}")
	if err != nil {
		return "", nil, err
	}
	var version string
	if err := json.Unmarshal([]byte(body), &version); err != nil {
		return "", nil, err
	}
	body, err = d.small(ctx, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return "", nil, err
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		return "", nil, err
	}
	runtimes := make(map[string]bool, len(entries))
	for k := range entries {
		runtimes[k] = true
	}
	return version, runtimes, nil
}

// ImageInspect 只读取已有镜像，不拉取。
// ImageInspect reads an existing image without pulling it.
func (d *DockerCLI) ImageInspect(ctx context.Context, ref string) ([]string, string, error) {
	body, err := d.small(ctx, "image", "inspect", "--", ref)
	if err != nil {
		return nil, "", err
	}
	var images []struct {
		ID          string
		RepoDigests []string
	}
	if err := json.Unmarshal([]byte(body), &images); err != nil {
		return nil, "", err
	}
	if len(images) != 1 {
		return nil, "", errors.New("镜像 inspect 必须唯一")
	}
	return images[0].RepoDigests, images[0].ID, nil
}

// Create 创建但不启动容器，参数由固定构造器生成。
// Create creates a stopped container from the fixed argument builder.
func (d *DockerCLI) Create(ctx context.Context, args []string) (string, error) {
	return d.small(ctx, append([]string{"create"}, args...)...)
}

// Start 启动已创建容器一次。
// Start starts the previously created container once.
func (d *DockerCLI) Start(ctx context.Context, id string) error {
	_, err := d.small(ctx, "start", "--", id)
	return err
}

// Wait 等待容器退出，调用方必须给出截止时间。
// Wait waits for exit under the caller's deadline.
func (d *DockerCLI) Wait(ctx context.Context, id string) (int, error) {
	body, err := d.small(ctx, "wait", "--", id)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(body)
}

// Logs 在读取中截断输出；日志存储也由 create 参数限制。
// Logs truncates while reading; create arguments also bound stored logs.
func (d *DockerCLI) Logs(ctx context.Context, id string, maxBytes int) (string, bool, error) {
	// Docker logs 将容器 stderr 写向自身 stderr；合流保留两条流。
	// Docker logs emits the container's stderr on its own stderr, so combine both.
	if err := d.localHost(ctx); err != nil {
		return "", false, err
	}
	cmd := exec.CommandContext(ctx, d.Path, "--host", d.host, "logs", "--", id) // #nosec G204 -- 固定 CLI 和 argv
	cmd.WaitDelay = time.Second
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DOCKER_HOST=") && !strings.HasPrefix(entry, "DOCKER_CONTEXT=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	out := &boundedCLIOutput{limit: max(1, maxBytes)}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return string(out.data), out.truncated, err
}

// Remove 强制终止并删除容器，调用方随后查证。
// Remove forcibly stops and removes a container for subsequent verification.
func (d *DockerCLI) Remove(ctx context.Context, id string) error {
	_, err := d.small(ctx, "rm", "--force", "--", id)
	return err
}

// ListByLabel 返回含已停止容器的名称列表。
// ListByLabel returns names, including stopped containers.
func (d *DockerCLI) ListByLabel(ctx context.Context, label string) ([]string, error) {
	body, err := d.small(ctx, "ps", "--all", "--filter", "label="+label, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	if body == "" {
		return []string{}, nil
	}
	return strings.Fields(body), nil
}
