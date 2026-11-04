# US20 验证指南

本轮读数与限制见 [验收报告](acceptance.md)。以下容器命令仅适用于用户已授权的本机 WSL Ubuntu；不把本次授权扩展到其他宿主、生产数据或真实模型。

## 1. 环境与候选

Linux amd64、非 root 用户、Docker 的本地 Unix socket、已注册的 runc/runsc、独立 PG 测试库、两个专用 tmpfs。实测 WSL 用户 UID/GID 均为 1000，容器 UID/GID 为 65534。Go 1.27.3；Node 24.22.0 只用于前端构建。

为避开主工作区历史 `.loop` Go 备份，本轮完整门禁在 `/tmp/basalt-us20-validation-tucsxg1v` 运行；2312 个 Git 可见路径在冻结时逐字 SHA256 核对。主树产品和测试文件是同一份内容，证据整理后的文档差异另记，不能拿副本指纹冒充整个主树指纹。

```sh
cd /tmp/basalt-us20-validation-tucsxg1v
export GOPROXY=https://goproxy.cn
go version
docker version
docker info
docker image inspect basalt:us20-test
```

预期命令成功、版本符合前提、runc/runsc 均存在。已保留本地夹具镜像 `basalt:us20-test`，镜像 ID 为 `sha256:32c7ea41a1bce7c55a4fc1b650b589fb4a46b144c9bf2c6b14af9b0538ac5a54`。它按本仓 Dockerfile 构建，测试覆盖 BusyBox 任务入口；镜像内 Basalt 是较早候选，不作为最终服务镜像发布证据。最终程序由 full 的 build 目标产生：`bin/basalt`。

## 2. 公共全量与 source

`with-postgres.sh` 从既有 WSL 私有运行目录在内存注入测试 DSN；不打印或写入凭据。下面命令均预期 exit 0 / passed。

```sh
bash /mnt/d/code/basalt/dev-env/scripts/with-postgres.sh \
  sh scripts/check-acceptance.sh --profile full
sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us20-base.json
go test -count=1 ./internal/core ./internal/tool ./internal/cli ./internal/isolation ./internal/app ./internal/web -run '^TestUS20_'
go test -race -count=10 ./internal/tool -run '^TestUS20_ArtifactPublishSerialized$'
go list -f '{{.Imports}}' ./internal/core
```

最后一条只应列标准库；并发关键回归必须十次全过。base 不访问 Docker，不能替代两档真实测试。不要并行启动多个 Harness：验收入口有独占锁。

## 3. 真实两档

原测试挂载在收尾时卸载；复跑须先创建新的专用挂载，不能把普通 `/tmp` 目录或其他实例输出根传进来。以下准备命令在 WSL 非 root shell 中执行，只有 mount 需要管理员权限。

```sh
export BASALT_US20_OUTPUT_TMPFS="$(mktemp -d /tmp/basalt-us20-output.XXXXXX)"
export BASALT_US20_APP_OUTPUT_TMPFS="$(mktemp -d /tmp/basalt-us20-app-output.XXXXXX)"
sudo mount -t tmpfs -o "size=64m,mode=0700,uid=$(id -u),gid=$(id -g),nodev,nosuid" tmpfs "$BASALT_US20_OUTPUT_TMPFS"
sudo mount -t tmpfs -o "size=64m,mode=0700,uid=$(id -u),gid=$(id -g),nodev,nosuid" tmpfs "$BASALT_US20_APP_OUTPUT_TMPFS"
export BASALT_US20_AUTH=user-2026-11-04-us20-wsl
export BASALT_E2E_BINARY="$PWD/bin/basalt"
bash /mnt/d/code/basalt/dev-env/scripts/with-postgres.sh \
  sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us20-oci.json
bash /mnt/d/code/basalt/dev-env/scripts/with-postgres.sh \
  sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us20-gvisor.json \
  --authorization-ref user-2026-11-04-us20-wsl \
  --authorization-scope 'WSL Ubuntu local Docker runsc; bounded US20 suite; synthetic files and dedicated test PG; no production data, no real models, no fees; network-none tasks and local listener controls'
```

预期 OCI 三个 check、gVisor 一个 check 均 passed，required_tests 无缺失、无 skip；两份报告 candidate SHA256 相同。gVisor 缺授权退出 2。这里的行为测试从测试二进制调用生产 ToolExecutor、Policy、Runner、PG 和恢复逻辑；候选 hash 标记不等同于全套真实服务发布演练。

结束后核对 `docker ps -a --filter label=basalt.instance_id` 和本次创建的目录，仅回收本 US 对应资源。验证变量指向刚创建的两个独占 tmpfs 后，执行 `sudo umount "$BASALT_US20_OUTPUT_TMPFS" "$BASALT_US20_APP_OUTPUT_TMPFS"`，再 `rmdir` 两个空挂载目录。不要删除其他实例容器或通配清理 `/tmp`。

## 4. 管理台与官网

在主工作区 PowerShell 使用已安装的 Node 24.22.0；以下均预期退出 0。前端构建独立于 Go 七目标。

```powershell
$env:PATH = 'D:\code\basalt\dev-env\tools\node;' + $env:PATH
npm.cmd --prefix assets/console test
npm.cmd --prefix assets/console run build
npm.cmd --prefix website test
npm.cmd --prefix website run build
npm.cmd --prefix website run check
```

Demo 锚点：默认 `run_code` 拒绝；两档未知/缺失时不回退；跨 UID 取回 report.txt 及哈希；取消后清理；崩溃后 unknown+cleaned 释放槽位；只读页面不把未知退出码显示为 0。真实行为已有 Harness 证据；页面实际视觉仍待人工查看，浏览器工具故障不记通过。
