package tool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// 固定脚本不含任务输入；恢复属主权限并逐项删除，覆盖双点前缀且避免 E2BIG。
// This fixed script restores owner permissions and deletes entries individually, covering double-dot prefixes without E2BIG.
const cleanupContainerCommand = "chmod -R u+rwX /output 2>/dev/null; " +
	`for f in /output/* /output/.[!.]* /output/..?*; do ` +
	`if [ -e "$f" ] || [ -L "$f" ]; then chmod -R u+rwX "$f" 2>/dev/null; rm -rf -- "$f"; fi; done`

func buildCleanupCreateArgs(opts ContainerRunnerOptions, dir, name string) []string {
	id, _ := strconv.ParseInt(filepath.Base(dir), 10, 64)
	args := containerBaseArgs(opts, name, id)
	args = append(args, "--label", "basalt.purpose=cleanup", "--mount", "type=bind,src="+dir+",dst=/output", "--entrypoint", "/bin/sh", opts.Image, "-c", cleanupContainerCommand)
	return args
}

// RemoveOutputDir 先做宿主删除，只有权限失败才用同档同 UID 的有界清理容器。
// RemoveOutputDir first removes on the host and uses a bounded same-profile, same-UID helper only for permission failures.
func (r *ContainerRunner) RemoveOutputDir(_ context.Context, dir string) error {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := validateMountPath(r.opts.OutputRoot, dir, true); err != nil {
		return err
	}
	err := r.opts.RemoveAll(dir)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrPermission) {
		return err
	}
	id, parseErr := strconv.ParseInt(filepath.Base(dir), 10, 64)
	if parseErr != nil || id <= 0 {
		return errors.New("清理目录缺少有效执行 ID")
	}
	name := cleanupContainerName(r.opts.InstanceID, id)
	if _, err := r.RemoveByName(context.Background(), name); err != nil {
		return err
	}
	if err := r.inspect(context.Background()); err != nil {
		return err
	}
	if err := r.runMaintenance(buildCleanupCreateArgs(r.opts, dir, name), name); err != nil {
		return err
	}
	// 清理容器自己必须消失；运行成功不能代替生命周期收敛。
	// Successful cleanup work does not substitute for removal of the helper itself.
	names, err := r.ListByLabel(context.Background(), "basalt.purpose=cleanup")
	if err != nil {
		return err
	}
	if slices.Contains(names, name) {
		return errors.New("清理容器未确认删除")
	}
	return r.opts.RemoveAll(dir)
}

func (r *ContainerRunner) runMaintenance(args []string, name string) (err error) {
	// 每段使用独立截止时间，不能继承已取消的任务 ctx。
	// Every phase has an independent deadline, never a canceled task context.
	create, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	id, createErr := r.opts.Docker.Create(create, args)
	cancel()
	defer func() {
		_, cleanupErr := r.RemoveByName(context.Background(), name)
		err = errors.Join(err, cleanupErr)
	}()
	if createErr != nil {
		return createErr
	}
	if id == "" {
		return errors.New("维护容器创建结果未知")
	}
	start, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	startErr := r.opts.Docker.Start(start, id)
	cancel()
	if startErr != nil {
		return startErr
	}
	wait, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	code, waitErr := r.opts.Docker.Wait(wait, id)
	cancel()
	if waitErr != nil {
		return waitErr
	}
	if code != 0 {
		return fmt.Errorf("维护容器退出码 %d", code)
	}
	return nil
}

// Probe 检查 runtime、镜像和真实 bind/跨 UID 清理行为，不执行用户代码或写工具审计。
// Probe checks runtime, image and actual bind/UID cleanup behavior without user code or tool audit.
func (r *ContainerRunner) Probe(ctx context.Context) error {
	version, cancel := context.WithTimeout(ctx, 20*time.Second)
	server, runtimes, err := r.opts.Docker.Version(version)
	cancel()
	if err != nil {
		return err
	}
	if server == "" || !runtimes[r.opts.Runtime] {
		return isolationDenied("not_ready", "所选 runtime 不可用")
	}
	if err := r.inspect(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// tmpfs 类型及两条容量不等式在装配层统一检查；这里验证 daemon 实际挂载的就是该路径。
	// Wiring checks tmpfs and capacity centrally; here the daemon must mount the actual host path.
	parent, err := os.MkdirTemp(r.opts.OutputRoot, "probe-")
	if err != nil {
		return err
	}
	defer func() {
		if err := r.opts.RemoveAll(parent); err != nil {
			r.opts.Logger.Warn("探针目录清理失败", "error", err)
		}
	}()
	id := r.opts.Now().UnixNano()
	if id <= 0 {
		return errors.New("探针时钟无效")
	}
	dir := filepath.Join(parent, strconv.FormatInt(id, 10))
	// 守护进程只绑定叶目录，探针父目录保持 MkdirTemp 的私有权限。
	// The daemon binds only the leaf; the probe parent keeps MkdirTemp's private permissions.
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		return err
	} // #nosec G302 -- 仅单次输出叶目录允许非特权容器 UID 写入
	name := cleanupContainerName(r.opts.InstanceID, id)
	args := buildCleanupCreateArgs(r.opts, dir, name)
	// 容器 UID 亲自创建 000 子目录，然后执行同一清理脚本；假文件、缺命令或空挂载均失败。
	// The container UID creates locked directories before running the same cleanup script.
	script := `set -e; test "$(id -u)" = ` + strconv.Itoa(r.opts.RunAsUID) + `; test -w /output; ` +
		`mkdir /output/locked /output/..cache; printf probe > /output/locked/file; chmod 000 /output/locked /output/..cache; ` +
		`set +e; ` + cleanupContainerCommand + `; set -e; test ! -e /output/locked; test ! -e /output/..cache; printf verified > /output/probe-result`
	args[len(args)-1] = script
	if err := r.runMaintenance(args, name); err != nil {
		return err
	}
	body, err := os.ReadFile(filepath.Join(dir, "probe-result")) // #nosec G304 -- 固定探针目录及文件名
	if err != nil || string(body) != "verified" {
		return errors.New("隔离探针未验证真实输出挂载及清理行为")
	}
	return r.RemoveOutputDir(context.Background(), dir)
}
