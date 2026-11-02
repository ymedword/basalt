# Quickstart：US-18 验证步骤

**Feature**: US-18 | **Plan**: [plan.md](plan.md) | **模块卡**: [docs/us/us18-docker-compose.md](../../docs/us/us18-docker-compose.md) §4 / §6.3

本文件只给**可执行验证步骤**与预期读数；实现细节在 `tasks.md` 与源码中。所有命令在仓库根执行，除非另行说明。

## 0. 前置检查

```sh
go version                      # 期望 go1.27.3（toolchain 自动安装；基础安装可低于此）
docker version && docker compose version
docker manifest inspect alpine:3.23 >/dev/null && echo base-ok
docker manifest inspect pgvector/pgvector:0.8.7-pg17-bookworm >/dev/null && echo pg-ok
```

- Windows 宿主没有 Docker CLI → 在 WSL Ubuntu 内执行（`wsl -d Ubuntu`）。
- secret 目录必须在 Linux 原生文件系统（ext4）；WSL 的 `/mnt/d` 上 `chown`/`chmod` 静默失效。

## 1. 源码层（不需要 Docker）

```sh
GOPROXY=https://goproxy.cn go test -count=1 ./scripts/releasekit ./cmd/basalt-verify -run '^TestUS18_'
GOPROXY=https://goproxy.cn go test -count=1 ./scripts/releasekit -run '^TestUS17_'
```

**期望**: 全 PASS。若 `TestUS17_Docs_CommandsMatchCLI` 因 `releasekit image` 失败，说明 `docs_test.go` 的 `known` 映射未同步。

```sh
GOPROXY=https://goproxy.cn go test -count=1 ./internal/e2e -tags=e2e -run '^TestUS18_Compose_ConfigRequiresSecrets$'
```

**期望**: PASS（只校验 `docker compose config` 的 fail-closed 行为，不启动容器）；本机无 Docker 时应 SKIP 并写明原因——SKIP 不是通过。

## 2. 构建 Linux/amd64 二进制与真实镜像

```sh
make build                       # 或 releasekit build --target linux/amd64 --tier binary
BASE=$(sh -c 'sed -n "s/.*\"app_base\": *\"\([^\"]*\)\".*/\1/p" release/image-lock.json')
GOPROXY=https://goproxy.cn go run ./scripts/releasekit/cmd/releasekit image build \
  --repo . --version 0.18.0 --binary dist/build/basalt --base-ref "$BASE" \
  --out dist/images --record dist/images/image-record.json
GOPROXY=https://goproxy.cn go run ./scripts/releasekit/cmd/releasekit image verify \
  --record dist/images/image-record.json --require-local-image
```

**期望**: 记录含 `platform=linux/amd64`、`tier=binary`、`local_id`；镜像内 `--version` 与二进制哈希均匹配；`image verify` 退出 0。

## 3. Compose 启动（WSL ext4 路径）

```sh
# 在 WSL 内，仓库必须位于 ext4（例如 ~/basalt），不能是 /mnt/d
cp docker/.env.example docker/.env      # 按需改端口 / ingress 示例值
sudo sh docker/init-secrets.sh docker/secrets
stat -c '%u:%g %a %n' docker/secrets/*  # 期望 0:0 400 postgres_password；10001:10001 400 database_dsn
docker compose -f docker/compose.yaml --env-file docker/.env up -d
docker compose -f docker/compose.yaml ps
curl -fsS http://127.0.0.1:8080/api/v1/health   # {"status":"ok"}
```

**期望**: `postgres` healthy 后 `basalt` 才启动；缺任一 `BASALT_INGRESS_*` 或 `BASALT_IMAGE` 时 `docker compose config` 即失败并点名变量。

## 4. 登录、任务、知识检索

```sh
TOKEN=$(docker compose -f docker/compose.yaml exec basalt \
  sh -c 'cat /var/lib/basalt/basalt/instances/*/admin-token')
curl -fsS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/api/v1/workspace/tree
```

**期望**: 200；不带 / 错 token 为 401。用管理台或 API 建一个最小 Agent 跑一次对话（mock 模型可用），上传 `.md` 摄取后 `POST /api/v1/search` 命中同一片段。

## 5. 重建、备份恢复、升级回退

```sh
docker compose -f docker/compose.yaml up -d --force-recreate basalt
# 期望：容器 ID 变、instance_id 不变、旧 token 仍 200

sudo sh docker/backup.sh /tmp/us18-backup         # 停应用 → pg_dump → 两卷打包 → secrets → SHA256SUMS
# 全新项目恢复
sudo sh docker/restore.sh /tmp/us18-backup <new-project>
# 期望：旧 token 登录、旧知识命中、instance_id 一致、secret 权限仍为规定值

# 升级：改 BASALT_IMAGE 到另一镜像 → up -d → 数据 / 凭据仍在
# 回退：新项目 + 升级前备份 + 旧镜像 → up -d → 同一数据可查
```

## 6. 故障用例

| 用例 | 操作 | 期望 |
| --- | --- | --- |
| 数据库不可用 | `docker compose stop postgres` | health 仍 200；`GET /api/v1/runs` 5xx；`/data` 无 `*.db` / `*.sqlite`；起库后恢复 200 |
| 错误凭据 | 本项目 secret 改成错误口令 | 应用非零退出；日志 `连接 PostgreSQL 失败` / `打开 PostgreSQL`；不含口令 |
| 卷权限 | 工作区挂载为不可写 | 非零退出并点名路径；属主改 10001 后可启动 |
| secret 权限 | `database_dsn` 保持宿主用户 0600 | 非零退出，日志 `Permission denied`；按准备步骤修好后 healthy；`mode & 0004 == 0` |
| 优雅停机 | IDLE / 会话运行中 `docker compose stop basalt` | 退出码 0，≤ 90s；在途 Run 落终态；再启动正常 |
| 崩溃不重跑 | 在途 Run 时 `docker compose kill basalt` → `up -d` | `running → failed`、半截工具组 `unknown`；mock 无新增调用 |
| 第二实例 | 共享同卷与数据库再起一个应用容器 | 非零退出，日志 `工作区实例锁已被占用` |

## 7. Harness 与 CI

```sh
# 容器阶段（Windows 经 WSL 包装）
wsl -d Ubuntu -- bash -lc 'cd /mnt/d/code/basalt && BASALT_E2E_REQUIRE_DOCKER=1 sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us18-linux.json'

# CI 同口径（container job）
BASALT_E2E_REQUIRE_DOCKER=1 BASALT_E2E_PROJECT_PREFIX=us18-ci-<run_id> \
  sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us18-linux.json
```

**期望**: `required_tests` 全部逐字 PASS；清理后 `docker ps -a` / `docker volume ls` / `docker network ls` / `docker images` 无 `us18-ci-<run_id>-` 前缀对象。

## 8. 失败与跳过口径

| 状态 | 处理 |
| --- | --- |
| 本机无 Docker | `t.Skip` 并登记未验证（U-18-01 之外的本地环境项）；不得写成通过 |
| CI 无 daemon / 镜像 | `BASALT_E2E_REQUIRE_DOCKER=1` 下 `t.Fatal`，job 失败 |
| 无 arm64 / 扫描器 / registry 授权 | `external` 层不执行，退出 2；登记未验证 |
| 9p/drvfs 上 `init-secrets.sh` 非零 | 预期行为，不是缺陷；改用 ext4 或 Docker 卷 |
