# Phase 1 Data Model: US-18 Docker／Compose 单实例部署

**Feature**: [spec.md](spec.md) | **Plan**: [plan.md](plan.md)

本 US **不新增数据库表、不新增配置键、不新增 HTTP 端点**。下列实体全部是发布工件、部署资产与校验选项；产品契约（`docs/contracts/`）零变更。

## 1. 发布工件实体

### ImageRecord（`release-record.json` 的 `images[]` 元素）

| 字段 | 类型 | 必填 | 校验规则 |
| --- | --- | --- | --- |
| `name` | string | 是 | 非空；镜像仓库名（本地预演可为 `basalt`） |
| `version` | string | 是 | 非空；与发布记录 `version` 一致 |
| `platform` | string | 是 | 本 US 只接受 `linux/amd64` |
| `tier` | Tier | 是 | 本 US 只接受 `binary`；`full` 需 US-17 组件齐备后另评 |
| `tag` | string | 是 | 非空；`docker image inspect <tag>` 的 Id 必须等于 `local_id` |
| `local_id` | string | 是 | `sha256:<64 hex>`；本地镜像 ID |
| `repo_digest` | string | 否 | 推送后才填；`RequireRepoDigest` 时必须匹配 `sha256:<64 hex>` |
| `base_ref` | string | 是 | 与 `image-lock.json` 的 `app_base` 一致 |
| `base_digest` | string | 是 | `sha256:<64 hex>`；从 `base_ref` 解析的真实 digest |
| `binary_sha256` | string | 是 | 64 hex；与输入二进制及镜像内 `sha256sum` 一致 |
| `dockerfile_sha256` | string | 是 | 64 hex；构建上下文中的 Dockerfile |
| `entrypoint_sha256` | string | 是 | 64 hex；构建上下文中的 entrypoint.sh |
| `config_sha256` | string | 是 | 64 hex；构建上下文中的 basalt.yaml |
| `sbom_path` | string | 是 | 相对**记录所在目录**的文件名；文件必须存在且哈希一致 |
| `sbom_sha256` | string | 是 | 64 hex |
| `provenance_path` | string | 是 | 同 `sbom_path` 的路径规则 |
| `provenance_sha256` | string | 是 | 64 hex |
| `scan` | ScanRecord | 是 | 见下；未扫描写 `not-run` + 原因，不编造结论 |
| `evidence` | []string | 否 | 证据标识（check ID 等） |

**关系**: `images[]` 与 `support-matrix.json` 的 `kind=image` 行**一一对应**；每条记录的两个文件进入发布目录后保持文件名，并被 `SHA256SUMS` 覆盖。

### ScanRecord

| 字段 | 类型 | 校验规则 |
| --- | --- | --- |
| `status` | string | `not-run` / `passed` / `failed` 三选一 |
| `scanner` | string | `status != not-run` 时非空 |
| `reason` | string | `status == not-run` 时非空，写明缺扫描器 / 未授权 |

### ImageLock（`release/image-lock.json`）

| 字段 | 类型 | 校验规则 |
| --- | --- | --- |
| `schema` | int | 固定 `1` |
| `app_base` | string | `alpine:<tag>@sha256:<64 hex>`；等于 Dockerfile 的 `ARG BASE_IMAGE` |
| `postgres` | string | `pgvector/pgvector:<tag>@sha256:<64 hex>`；等于 compose 的数据库镜像与 `.env.example` 的 `BASALT_DB_IMAGE` |

### SupportRow 的镜像扩展

| 字段 | 类型 | 校验规则 |
| --- | --- | --- |
| `kind` | string | 空 = 归档行（旧语义）；`image` = 镜像行；其他值拒绝 |
| `goos` / `goarch` | string | 镜像行写镜像平台（`linux` / `amd64`） |
| `tier` | Tier | 镜像行为 `binary` |
| `status` | string | `tested` 必须有非空 `checks`；`compiled-only` / `not-tested` 必须写 `reason` |
| `checks` / `reason` | []string / string | 由 `CheckSupportRow` 判卷 |

**唯一性**: 同一 `(goos, goarch, kind)` 不得重复；`AppendImageRows` 任一行失败则整次追加不生效。

### VerifyPolicy 扩展

| 字段 | 类型 | 语义 |
| --- | --- | --- |
| `require_repo_digest` | bool | 默认 `false`；`true` 时每条 `images[].repo_digest` 必须非空且为 `sha256:<64 hex>` |

## 2. 部署资产实体

### 卷（命名卷，Compose 自动加项目前缀）

| 卷 | 容器路径 | 内容 | 重建后保留 |
| --- | --- | --- | --- |
| `database-data` | `/var/lib/postgresql/data` | PostgreSQL 数据（会话 / 运行 / 审计 / 知识 / 记忆 / 诊断 / 调度） | 是 |
| `workspace` | `/data` | `basalt.yaml`、`.basalt/instance.json`、`.basalt/basalt.lock`、`.basalt/uploads/` | 是 |
| `private` | `/var/lib/basalt` | `basalt/instances/<instance_id>/master.key`、`admin-token` | 是 |

**不变量**: 实例身份与管理凭据在 `private` 卷，**不在**容器临时层；`workspace` 卷被删除 ≠ 实例身份丢失（仍可由 `private` 恢复），但两者必须**配套**备份。

### Secret 文件（宿主路径 → 容器路径）

| 宿主 | 容器 | 属主 / 权限 | 消费方 |
| --- | --- | --- | --- |
| `docker/secrets/postgres_password` | `/run/secrets/postgres_password` | `root:root 0400` | PostgreSQL entrypoint（降权前） |
| `docker/secrets/database_dsn` | `/run/secrets/database_dsn` | `10001:10001 0400` | 应用 entrypoint → `BASALT_DATABASE_DSN` |

**不变量**: 两文件不得世界可读（`mode & 0004 == 0`）；不得写入镜像、命令行、日志或 Git；`init-secrets.sh` 写后 `stat` 复核，不符即非零退出。

### 实例目录（`private` 卷内）

```text
/var/lib/basalt/                       # XDG_DATA_HOME
└── basalt/
    └── instances/
        └── <instance_id>/
            ├── master.key             # 0600
            └── admin-token            # 0600
```

**关系**: `<instance_id>` 同时出现在 `workspace` 卷的 `.basalt/instance.json` 中；两者一致才构成完整实例。容器重建（新容器 ID、同卷）后该目录不变，旧 admin token 继续有效。

### 备份四件套

| 工件 | 生成方式 | 恢复后校验 |
| --- | --- | --- |
| `database.dump` | `pg_dump -Fc`（停应用后） | `pg_restore` 到新库；旧 admin token 可登录 |
| `workspace.tar.gz` | 打包 `workspace` 卷 | 旧知识片段可检索 |
| `private.tar.gz` | 打包 `private` 卷 | `instance_id` 一致、旧 token 有效 |
| `secrets/` | 复制两个 secret 文件 | 属主 / 权限恢复为规定值 |
| `SHA256SUMS` | 对以上工件生成 | 恢复前校验，失败即停止 |

## 3. 状态与转移

### 镜像记录生命周期

```text
构建 → ImageRecord.Validate 通过 → image verify（本地镜像 + 文件哈希）
  → assemble（复制 SBOM/provenance、追加矩阵行、写 images[]、校验和、签名）
  → VerifyRelease（矩阵含镜像行或 images[] 非空即校验）
  → basalt-verify [--require-image-digest]（正式记录要求 repo_digest）
```

任何一步失败：记录不得进入发布目录的最终校验和 / 签名；本地镜像由 `BuildImage` 删除，避免留下无记录镜像。

### 容器生命周期（与既有产品语义一致）

| 事件 | 数据 | 未完成 Run | 实例身份 / 凭据 |
| --- | --- | --- | --- |
| `restart` / 重建容器（同卷） | 保留 | 恢复扫描：`running → failed`、半截工具组 → `unknown`；不自动重跑 | 保留 |
| 优雅停机（SIGTERM，宽限内） | 保留 | 在途 HTTP 正常完成，Run 按自身结果落终态 | 保留 |
| 宽限到期 | 保留 | 在途请求被取消，Run 落 `cancelled` 或失败终态 | 保留 |
| SIGKILL（Compose 90s 兜底 / `kill`） | 保留 | 下次启动恢复扫描收口；无第二次外部动作 | 保留 |
| `down -v` | 删除 | 不适用 | 删除（仅限测试项目 / 显式销毁） |

## 4. 约束汇总（可判定）

| # | 约束 | 判据 |
| --- | --- | --- |
| D-1 | 运行期无 SQLite | 容器 `/data` 下 `find -name '*.db' -o -name '*.sqlite'` 为空；停库后业务 5xx |
| D-2 | 应用非 root | `docker exec basalt id -u` == `10001` |
| D-3 | 只读根文件系统 | `compose.yaml` 的 `read_only: true`；可写面只有卷与 `tmpfs /tmp` |
| D-4 | 零新增 Go 坐标 / 零迁移 | `git diff go.mod go.sum` 为空；`schema.sql` / 迁移目录无 diff |
| D-5 | 私钥不进镜像 | `CheckImageContext` 对文件名与内容双重拒绝；镜像内无私钥内容 |
| D-6 | 矩阵与记录一一对应 | 镜像行数 == `len(images[])`，且 `(goos,goarch,kind)` 无孤儿 |
