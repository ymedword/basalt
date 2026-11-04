# Implementation Plan: 执行隔离升级

**Branch**: `020-execution-isolation` | **Date**: 2026-11-04 | **Spec**: [spec.md](spec.md)

## Summary

复用 Policy / CommandRunner 和 ToolExecutor，新增一个 Docker CLI 后端；受控 shell 显式绑定 runc，生成 shell 代码只能绑定 runsc。静态配置、发布验签、共享准入、PG 生命周期账本、清理恢复和只读投影一起交付。

## Technical Context

**Language/Version**: Go 1.27.3（toolchain 与 CI；go 1.27 为最低要求），单 module，发布 CGO_ENABLED=0。

**Primary Dependencies**: 沿用 go.mod/go.sum；cobra、goccy/go-yaml、pgx 与标准库 os/exec、os.Root、crypto/ed25519。零新增坐标和版本升级；US19 的 jsonschema/v6 生产用途保留。前端锁文件与 Node 24.22.0 不变。

**Storage**: PostgreSQL v8 → v9，只追加 `0009_sandbox_execution.sql`；SQLite v6 及历史迁移冻结。

**Testing**: Go source / race / integration / Linux e2e，公共 Harness 与 US20 base/oci/gvisor stages；管理台独立构建。

**Target Platform**: Linux amd64 真实容器；Windows/macOS 保留默认本机与 source 测试。

**Performance Goals**: 无新增延迟 SLO；每个 Docker 子命令有界，执行期限取输入/profile/Run 最小值；记录冷启动、正常、拒绝及清理耗时。

**Constraints**: 无新调度器、无真实模型调用、无运行期 pull、无热加载、无逐执行输出硬配额承诺。

**Scale/Scope**: 单实例工作区，独占 tmpfs，两档共享 N 个容量槽，单次 shell 与顶层普通文件产物。

## Constitution Check

| 原则 | 本次落实 | 结论 |
| --- | --- | --- |
| I / II | 不引框架；ToolExecutor 唯一调度审计，ReActLoop 既有结果追加 | 通过 |
| III | 工具/profile/runtime 显式映射，未知拒绝 | 通过 |
| IV | 不改变 Agent/skill 绑定，不自动增删 Agent 工具声明 | 通过 |
| V | 调用审计 pending 在先；账本预写后执行，终态失败可恢复 | 通过 |
| VI | 调用方仍经 Policy/CommandRunner；仅已审 additive 字段与动作 | 通过 |
| VII | 同步单次执行；ToolEnv 显式传递，无新 Worker | 通过 |
| VIII | 零新 Go 依赖，CGO=0 发布，Docker/runsc 启动 fail-closed | 通过 |
| IX | 手写 SQL，只追加 PG 0009，事务不跨执行 | 通过 |
| X | 仅登记范围，无 microVM/MCP 全隔离/在线编辑 | 通过 |

core 仅标准库；tool 消费 core；cli 定义自检端口；app 唯一装配；web 只读；postgres 实现仓储端口。设计后复核无原则例外，不修宪。

## Project Structure

```text
internal/core/isolation.go, action.go, store.go
internal/tool/command_guard.go, isolation_runner.go, isolation_ledger.go, runcode.go
internal/tool/runner.go, shelltools.go, policy.go
internal/isolation/trust.go, release_pubkey.go
internal/cli/config_isolation.go, isolation.go, config.go, ports.go, deps.go, root.go
internal/app/isolation.go, wire.go, web.go, run.go
internal/storage/postgres/isolation.go, migrations/0009_sandbox_execution.sql
internal/storage/postgres/history_retention.go, connect.go
internal/web/views.go, server.go, handlers_sandbox.go, handlers_runs.go
assets/console/src/, tests/isolation.test.ts, dist/, integrity.json
internal/e2e/us20_*_test.go
scripts/releasekit/imagelock.go, image.go, tests
release/image-lock.json, docker/Dockerfile, docker/basalt.yaml, docker/compose.yaml
docs/contracts/isolation.md, isolation.pg.sql, openapi.yaml, run-and-errors.md
harness/stages/us20-base.json, us20-oci.json, us20-gvisor.json
```

内部文件可按职责拆分，例如 CLI 进程、Linux Statfs、产物发布，不新增对外组件。

## Implementation sequence

类型与静态配置 → 清单验签及 PG 账本 → 单容器后端与共用命令守卫 → 准入与工具完整切片 → 装配恢复和 CLI → web/console → 真实两档与变异 → 全量门禁与独立复核。

## Safety and lifecycle

- 授权默认 false，与 strict/standard 正交；生成代码仅 gVisor；未知/不可用均拒绝，无回退。
- 无网、非 root、只读根、cap-drop ALL、no-new-privileges、资源/日志限额；仅当前任务和当次输出挂载。
- staging 0755/0644；只把独占 tmpfs 的单次输出目录设 0777；跨 UID 不可读产物拒绝。
- 输出下界 `N*max(output_tmpfs_mb)*MiB`；其他预算 `N*(max(tmpfs_mb)*MiB+max_input_bytes+max(max_output_bytes))`；真实 size 位于输出下界与总量减其他预算之间；正值与溢出检查。
- shared 输出没有逐任务硬配额；清理 helper 同样受限、确定性命名/标签、自身删除复查。未确认即占容量。
- create 响应未知只查询和清理，不重试；确认容器删除后才删目录；未知不伪造为失败或成功。
- 产物扫描后在实例锁内统计所有 Agent 产物并整体发布；失败撤回当次目标。

## Internal clarifications

伪代码缺依赖的内部 helper 落为方法或显式 options；stageRunCode/collectAndPublishRunCode 需要 opts 与 ctx，publishOptions 需要源目录及实例根。保留行为顺序，不用全局变量或业务 context 偷传，不改公共键/API/权限/断言。

ContainerCLI 没有 Kill 方法，Remove 的生产实现使用 `docker rm -f` 终止容器并复查，真实取消测试证明进程树释放。公钥复用现有 Ed25519 发布公钥，不生成新信任根，不读取私钥。

任务、探针、清理共用 `--pull=never` 与 `--ipc=none`，兑现不拉镜像和限定临时可写面的既有要求。Docker daemon 必须本机且共享 bind 路径命名空间，探针用真实挂载读写验证；拒绝远端 context/DOCKER_HOST。FlowAdapterFor 继续不接纳 shell/run_code，注册不等于 Flow 授权。

## Validation

FR 与 tasks/stage 双向映射；required_tests 空匹配/SKIP 不通过；公共 full 包含七目标、PG、固定行为/注入评测。Node 构建独立。关键变异用仓库外 overlay，记录逐字失败，恢复后绿态复验。源码变化使旧证据失效。自动通过后仍等人工 review ②。

## Complexity Tracking

无原则例外。账本服务生命周期而非第二套审计，标准库验签不扩为 OCI 信任平台。
