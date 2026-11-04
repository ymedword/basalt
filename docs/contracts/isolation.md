# 执行隔离契约（US-20）

状态：已批准范围的实施契约，人工收口 review ② 待完成。授权 Policy 与执行 CommandRunner 分离；工具只经既有 ToolExecutor 调度和调用审计。隔离账本不代替调用审计，不允许重放未知执行。

## 静态配置

`isolation` 缺省关闭；既有 `shell` 保持本地执行。`run_code` 始终登记，但未配置或未授权时拒绝，绝不回退。配置修改需重启，管理台只读。`execution.allow_run_code` 默认 `false`，独立于 strict / standard 授权档位。

| 键 | 约束 |
| --- | --- |
| `isolation.backend` | 仅 `docker`，Linux 本机 Unix socket；拒绝远程 daemon |
| `output_root` | 绝对路径、本实例独占的真实 tmpfs 挂载点，禁止软链接 |
| `release_manifest` | 绝对路径；同目录 `<文件>.sig`，内嵌发布公钥验证原始字节签名 |
| `run_as_uid` / `run_as_gid` | 默认 65534，必须为正，拒绝 root |
| `max_concurrent` / `max_total_cpus` / `max_total_memory_mb` | 必填正数；并发数为整数，CPU 必须有限 |
| `max_total_temp_bytes` / `max_input_bytes` / `max_total_artifact_bytes` | 必填正整数字节数；输入含副本与 `task/main.sh` |
| `tools.shell` | `local` 或 `oci`；省略为 local |
| `tools.run_code` | 只能为 `gvisor`，且必须同时设置 `execution.allow_run_code: true` |
| `profiles.oci.runtime` / `profiles.gvisor.runtime` | 固定 `runc` / `runsc` |
| `profiles.*.image` | 完整 sha256 固定引用，必须命中已验签清单，并与实际镜像复核；不自动拉取 |
| `profiles.*.cpus` / `memory_mb` / `pids` / `tmpfs_mb` / `output_tmpfs_mb` / `max_output_bytes` | 必填正数；除 CPU 外为整数；MB 字段单位 MiB |
| `profiles.*.timeout_seconds` | 整数 1–600，与调用上下文取最小 |

未知键、null、未知执行档/工具绑定均拒绝。至少显式绑定一个容器档；未绑定档显示未探测，不冒充可用。所有凭据引用只用 `${ENV_VAR}`，隔离任务不注入服务凭据。

令 N 为并发上限，maxP 为已绑定档最大值，所有量转为字节后：

```text
N × maxP(output_tmpfs_mb × 1MiB) ≤ output_root 实测 size
实测 size + N × (maxP(tmpfs_mb × 1MiB) + max_input_bytes + maxP(max_output_bytes)) ≤ max_total_temp_bytes
N × maxP(cpus) ≤ max_total_cpus
N × maxP(memory_mb) ≤ max_total_memory_mb
```

共享输出 tmpfs 有总量硬限；v1 **没有单任务输出硬配额**，单任务可争用共享容量，完成后另校验单文件、文件数与总量。实例产物预算在同一实例共享锁内检查与发布，只支持单实例写入。

## 执行与文件

OCI 仅受控 `shell` argv；禁止 shell 内联代码，解释器按基名识别，仅接受 Agent `scripts/` 内的相对脚本并映射到 `/task`。生成代码只能用 gVisor `run_code`：`{code, inputs?, timeout_seconds?}`；code 1–65536 字节、inputs 最多 16 个路径、单路径最多 512 字节、可选超时 1–300 秒。

输入在实际工作区 `<Root>/sandbox/<run>/<invocation>/task` 暂存并只读挂载 `/task`。仅单次输出叶目录设 0777 并 rw 挂载 `/output`；容器 UID 默认 65534。容器停止且确认移除后，宿主检查输出顶层普通文件，拒绝子目录、链接、特殊文件、越界与超限，复制并逐文件校验 SHA-256 到 Agent `artifacts/<run>/<invocation>/`。取消时不提交产物，失败回滚本次发布目录。

两档均无网络、只读根、移除 capability、no-new-privileges，限制 CPU、内存、pids、tmpfs、nofile、日志和输出。不给宿主 socket、控制 API、设备或服务环境。清理按容器、输出、staging 三项确认；权限受阻时使用同镜像/运行时/UID 的有界清理容器，它同样必须移除并按标签复查。

## 信任与持久化

`release/image-lock.json` 源 schema 2 的 sandbox 引用可以暂空；发布时 releasekit 合并实际镜像记录并对外置清单签名。运行期要求完整 `app_base`、`postgres`、`sandbox.oci`、`sandbox.gvisor` 的 `repo:tag@sha256:<64hex>` 引用；验签通过但内容非法仍拒绝。生产没有配置公钥覆盖或跳过签名选项。

PG 前向迁移 0009 新增 `sandbox_executions`，结构见 [isolation.pg.sql](isolation.pg.sql)。先写调用审计 pending，再写隔离账本 creating/pending，然后创建容器。状态闭集：`creating/running/finished/canceled/timeout/failed/unknown`；清理闭集：`pending/cleaned/failed`。pending 仅预写产生，全部释放才 cleaned。未知执行不重放；`unknown + cleaned` 不占容量。历史保留不得删除仍占资源的记录。

启动先探针，再恢复：账本行清理失败 WARN 且保留容量；已处理的任务/清理容器不被标签扫描二次处理；无账本孤儿清理失败、枚举失败或无账本目录清扫失败拒绝启动。最后一次性刷新账本基线，与本进程活跃数在同一锁内准入。

## 错误与只读投影

既有八类 `error_kind` 和四类 `policy_denial_reason` 不变。隔离诊断原因独立保存，不扩展授权拒绝四类：`config_missing/not_ready/policy_denied/admission_rejected/admission_unavailable/image_untrusted/input_rejected/create_failed/create_unknown/start_failed/wait_failed/state_unknown/artifact_rejected/artifact_publish_failed/limit_exceeded/canceled/timeout/orphan_cleaned/cleanup_failed`，无原因用空串。

管理 API `GET /sandbox` 增加 `isolation`；`GET /runs/{id}` 增加 `isolation_executions`，空值用 `[]`。固定字段以 [openapi.yaml](openapi.yaml) 为准。无新增页面/写接口。`basalt isolation check` 只探测静态配置、清单、运行时与挂载，不触数据库；关闭时正常报告关闭，配置或能力不足时非零退出。

MCP、HTTP、通知工具保持原路径，不自动套隔离；返回内容始终低信任数据。US18 服务容器部署不等同任务隔离；Windows/macOS 原生不声明同等隔离。真实 Linux 两档验收、取消与清理失败证据是 US20 收口必要条件。
