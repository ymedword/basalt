# 契约：容器部署的可观察行为

**Feature**: US-18 | **Plan**: [../plan.md](../plan.md) | **模块卡**: §2.7 / §2.8 / §2.9

本契约固定容器部署对运维者可见的行为：启动失败、健康检查、停机、权限与数据保留。它复用产品既有语义，**不新增**配置键、HTTP 端点或错误类型。

## 1. 服务与依赖

| 服务 | 镜像 | 依赖 | 健康检查 |
| --- | --- | --- | --- |
| `postgres` | `pgvector/pgvector:0.8.7-pg17-bookworm@sha256:…` | 无 | `pg_isready` |
| `basalt` | `${BASALT_IMAGE}`（`basalt:X.Y.Z@sha256:…`） | `postgres: service_healthy` | `wget -qO- http://127.0.0.1:8080/api/v1/health` |

- 单副本；不支持水平扩展或第二实例共享同一工作区。
- 应用与数据库在同一专用 bridge 网络；**不**设 `internal: true`（应用需要出网调用模型）。
- 端口默认 `127.0.0.1:${BASALT_HTTP_PORT:-8080}`；远程访问必须显式设 `BASALT_HTTP_BIND=0.0.0.0` 并在 `/data/basalt.yaml` 补 `allowed_hosts`。

## 2. 启动门禁与退出码

| 阶段 | 失败条件 | 可观察结果 |
| --- | --- | --- |
| `docker compose config` | 缺 `BASALT_IMAGE` 或任一 `BASALT_INGRESS_*` | 非零退出，点名缺失变量 |
| 容器启动（entrypoint） | secret 文件缺失 / 为空 | 退出 `78`，日志点名 `/run/secrets/database_dsn` |
| 容器启动（entrypoint） | secret 不可读（属主 / 权限不符） | 非零退出，日志含 `Permission denied`，不打印内容 |
| `basalt serve` | 六项 ingress 缺失或 ≤ 0 | 退出 `1`，日志含 `服务启动要求下列配置项大于零` |
| `basalt serve` | DB 不可达 / 口令错 | 非零退出，日志含 `连接 PostgreSQL 失败` 或 `打开 PostgreSQL`，不含口令 |
| `basalt serve` | 扩展缺失 / 能力不足 | 非零退出，日志点名缺哪个扩展或能力 |
| `basalt serve` | 迁移失败 / schema 过新 | 非零退出，日志点名版本 |
| 正常 | — | 监听 `0.0.0.0:8080`，health 返回 `{"status":"ok"}` |

**健康检查语义**: `/api/v1/health` 只代表进程存活，不反映数据库状态。数据库停机时 health 仍 `200`，业务端点（如 `GET /api/v1/runs`）返回 5xx；数据库恢复后同一请求自动恢复，无需重启应用。

## 3. 停机语义

| 场景 | 行为 | 容器退出码 |
| --- | --- | --- |
| IDLE | `SIGTERM` → 停止接受新请求 → 宽限内退出 | `0` |
| 会话运行在途（`RunTimeout 60s`） | 等在途 HTTP 完成（≤ 70s）；Run 按自身结果落终态 | `0` |
| 手动补跑在途（`ScheduleRunTimeout 300s`） | 宽限到期 → 取消在途请求 → 非零退出 | 非零（预期） |
| Compose `stop_grace_period: 90s` 到期 | Docker 发送 `SIGKILL` | `137` |
| `docker compose kill`（SIGKILL） | 无优雅停机；下次启动恢复扫描收口 | `137` |

**重启后**: 数据保留与未完成任务恢复是两件事——卷内数据保留；`running` Run 由恢复扫描改为 `failed`、半截工具组改为 `unknown`；**不**自动重跑。调度器在途任务受独立 `StopGrace` 约束，终态独立落库。

## 4. 卷与 secret 权限

| 对象 | 要求 | 违反时 |
| --- | --- | --- |
| `database-data` | 由 PostgreSQL 镜像初始化，属主由镜像管理 | 启动失败，日志点名数据目录 |
| `workspace` | 应用 UID 10001 可写 | 应用非零退出并点名路径 |
| `private` | 应用 UID 10001 可写；内含 `master.key` / `admin-token`（0600） | 身份 / 凭据生成失败，非零退出 |
| `database_dsn` secret | `10001:10001 0400`，other 读位为 0 | 应用非零退出，`Permission denied` |
| `postgres_password` secret | `root:root 0400`，other 读位为 0 | PostgreSQL 启动失败 |

**不变量**: 不使用 `0444` 或 world-readable 让用例变绿；secret 目录必须位于 Linux 原生文件系统（ext4 / xfs / btrfs 或 Docker 卷）；9p/drvfs 上 `init-secrets.sh` 的写后 stat 复核必须非零退出。

## 5. 备份、恢复、升级、回退

| 操作 | 前置 | 步骤 | 成功判据 |
| --- | --- | --- | --- |
| 备份 | 应用已停止；root / 一次性 root 容器 | `pg_dump -Fc` → 打包 workspace / private → 复制 secrets → `SHA256SUMS` | 四件套完整、校验和生成 |
| 恢复 | 全新项目；root / 一次性 root 容器 | 建卷 → 校正属主 10001 → 解包 → `pg_restore` → 放 secrets 并重设权限 → `up -d` | 旧 admin token 登录、旧知识命中、`instance_id` 一致 |
| 升级 | 先做四件套备份 | 改 `BASALT_IMAGE` 到新 digest → `up -d`（启动即前向迁移） | 数据、`instance_id`、旧 token 可用 |
| 回退 | 升级前备份存在 | 停应用 → 在新项目恢复备份 → `BASALT_IMAGE` 指向旧镜像 → `up -d` | 同一数据可查；secret 权限正确 |

**禁止**: 裸镜像降级（不恢复备份直接换旧镜像）；依赖自动升级平台（本 US 不提供）。

## 6. 隔离与清理（开发 / CI）

- 项目名 `<prefix>-<用例名>-<随机后缀>`，默认前缀 `us18`；禁止使用默认项目名 `basalt`。
- 端口从临时端口段取空闲值，只绑 `127.0.0.1`；不使用 8080。
- 每个用例 `t.Cleanup` 执行 `down -v`；全套结束后断言无同前缀容器 / 卷 / 网络 / 镜像残留。
- CI 前缀 `us18-ci-<run_id>-`（含尾随 `-`）；清理按真实项目枚举后逐个 `down -v`，再按统一前缀删除四类残留对象，有残留即 job 失败。
- 清理**不得**对用户实例执行任何命令；测试凭据只存在临时目录与测试进程内。
