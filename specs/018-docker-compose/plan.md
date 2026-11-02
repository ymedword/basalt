# Implementation Plan: US-18 Docker／Compose 单实例部署

**Branch**: `018-docker-compose` | **Date**: 2026-11-02 | **Spec**: [spec.md](spec.md)

**Input**: US-18 R8 模块卡（[docs/us/us18-docker-compose.md](../../docs/us/us18-docker-compose.md)，人工 review ① 已通过）+ 本目录 `spec.md`

## Summary

交付一个可起、可看、可备份、可升级、可判定失败的 **Linux/amd64 单实例容器部署单元**：US-17 已产出的 `CGO_ENABLED=0` 静态二进制装进固定 digest 的 alpine 运行镜像，与 PostgreSQL 17 + pgvector 0.8.7 一起由 Compose 编排（三卷、两 secret、非 root、只读根文件系统、`cap_drop: ALL`、`no-new-privileges`、`stop_grace_period: 90s`）。运行期存储沿用 US-16 的 PostgreSQL 唯一后端，本 US **零迁移、零产品契约变更**。

发布侧新增镜像面：`releasekit image build|verify`、`assemble --image-record`、`release-record.json` 的 `images[]`、`support-matrix.json` 的 `rows[].kind`、`basalt-verify --require-image-digest`，并保证“镜像记录 → assemble → verify”单一路径贯通（不提供汇总后补字段的第二条路径）。验收由 17 个真实镜像 / 真实 Compose 用例（A1–A17）与源码层单元测试承载，CI 新增独立 `container` job。

## Technical Context

**Language/Version**: Go 1.27.3（`go.mod` toolchain 与 CI `setup-go` 固定；`go 1.27` 仅为最低要求）；容器脚本为 POSIX `sh`（Alpine BusyBox ash 兼容）；Compose 文件为 Compose Spec（Compose v2）。

**Primary Dependencies**: 无新增 Go 坐标——`scripts/releasekit` 只依赖标准库，镜像构建通过 `docker` CLI 而非 Docker SDK；运行镜像基于 `alpine:3.23`（digest 固定），数据库镜像 `pgvector/pgvector:0.8.7-pg17-bookworm`（digest 固定）；数据库驱动沿用 `github.com/jackc/pgx/v5 v5.11.0`。

**Storage**: PostgreSQL 17 + pgvector 0.8.x + `pg_trgm`（运行期唯一后端，`SupportedSchemaVersion = 7`）；三个命名卷：`database-data` → `/var/lib/postgresql/data`、`workspace` → `/data`、`private` → `/var/lib/basalt`；两个 file 型 Compose secret：`database_dsn`（10001:10001 0400）、`postgres_password`（root:root 0400）。

**Testing**: 三层——`source`（`go test ./scripts/releasekit ./cmd/basalt-verify`，不需要 Docker）、`process`（`go test -tags=e2e ./internal/e2e`，真实 Docker daemon 与真实 Compose，模型用 mock）、`external`（镜像仓库推送 / 扫描 / arm64，本 US **未授权**，不执行）。Harness stage 落 `harness/stages/us18-*.json`，CI 经 `scripts/check-acceptance.sh --stage-only` 执行。

**Target Platform**: 部署对象 `linux/amd64` 容器；开发与本地验收宿主为 Windows 11 + WSL2 Ubuntu（Docker 29.9.2 / Compose v5.6.1），CI 为 GitHub Actions `ubuntu-latest`。

**Project Type**: 单 Go module 产品 + 独立发布工具包（`scripts/releasekit`）+ 容器部署资产（`docker/`）+ e2e 测试包。

**Performance Goals**: 本 US 不新增性能指标。沿用既有语义：健康检查 `/api/v1/health` 只代表进程存活；启动就绪由“DB 就绪 → 扩展能力检查 → 迁移 → 监听”顺序保证；停机宽限 `defaultShutdownGrace = 70s`，Compose `stop_grace_period: 90s`（外部兜底 SIGKILL）。

**Constraints**: `CGO_ENABLED=0` 发布构建；容器非 root（UID/GID 10001）；只读根文件系统 + `tmpfs /tmp`；凭证只经 secret 文件 / `${ENV_VAR}` 注入，不明文落盘；发布签名私钥**不得**进入构建上下文、镜像或容器；正常启动**不得**下载依赖；`docker/secrets/` 必须位于 Linux 原生文件系统（9p/drvfs 上 `chown`/`chmod` 静默失效，脚本须 fail-closed）。

**Scale/Scope**: 单实例、单副本；不做 Kubernetes / Swarm / 多副本 / 自动升级平台 / 镜像仓库推送 / OCI 签名 / arm64 镜像 / full 档镜像 / 外部 PostgreSQL 实测 / 跨 schema 回退实测。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 条款 | 本方案的落实 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | 镜像只装载既有 `cmd/basalt` 二进制；`go.mod` 零新增坐标；不引入 Agent 框架或协议 SDK | 通过 |
| II LLM 客户端只做协议调用 | 不改 `provider` 包；Compose 用例使用 mock 模型，不启用真实模型或自动工具循环 | 通过 |
| III Provider 显式映射 | 不触碰装配路径；容器不改变 provider 映射的发布 / 固定语义 | 通过 |
| IV 一目录一 Agent；技能声明式绑定 | 不改 agent 注册与技能披露路径 | 通过 |
| V 审计 day-one 且 fail-closed | 不新增调用路径、不新增审计写入方；备份 / 恢复脚本不接触审计表；数据库仍是唯一权威存储 | 通过 |
| VI 授权与执行分离 | 不触碰 `Policy` / `CommandRunner` 接口 | 通过 |
| VII 同步执行；context 只承载取消 / 截止 / 追踪 | 不引入异步编排；容器健康检查、备份、恢复脚本是部署期运维工具，不是产品运行路径；e2e 只用 `context` 传取消与超时 | 通过 |
| VIII 单二进制纪律 | 镜像构建输入为 `CGO_ENABLED=0` 二进制；`CheckBinaryTarget` 在构建前核对 GOOS / GOARCH / CGO；`-race` 与发布构建分属不同命令 / job；零新增 Go 依赖 | 通过 |
| IX 不用 ORM，手写 SQL；迁移只追加 | 不新增 / 不改写迁移；`initdb/10-extensions.sql` 只在**空库首启**创建扩展，应用侧只做能力检查，不执行 `CREATE EXTENSION` | 通过 |
| X 五大能力优先，范围卡紧 | 范围限定单实例、Linux/amd64、binary 镜像；full 档、其他架构与自动升级明确列为不宣称能力 | 通过 |
| 架构约束 · 包依赖方向 | `core` 零改动；`scripts/releasekit` 是独立工具包（不进入产品二进制）；`internal/e2e` 仅测试；不新增 `internal/` 包 | 通过 |
| 架构约束 · 接口定义在消费方 | 不新增跨包接口；`VerifyImages` / `AppendImageRows` 等只在 `releasekit` 内部消费 | 通过 |
| 架构约束 · 第三方实现放消费方包内 | 不新增第三方实现；Docker 交互走 CLI，不引入 SDK | 通过 |

**Phase 0 结论**：无 NEEDS CLARIFICATION，无宪法违规，无需 Complexity Tracking 的豁免项。

## Project Structure

### Documentation (this feature)

```text
specs/018-docker-compose/
├── plan.md              # 本文件
├── research.md          # 关键技术裁决（占位符解析、secret 属主、镜像记录接线、CI 清理）
├── data-model.md        # ImageRecord / ImageLock / 矩阵 kind / 卷与 secret 布局
├── quickstart.md        # 可执行的验证步骤（源码、真实镜像、Compose、备份恢复）
├── contracts/
│   ├── image-cli.md     # releasekit image 与 assemble --image-record 命令契约
│   └── deploy-runtime.md# 部署期可观察契约（退出码、日志、卷与 secret 权限）
├── checklists/
│   └── requirements.md  # 规格质量检查表（已生成）
├── spec.md              # 功能规格（已生成）
└── tasks.md             # /speckit-tasks 输出（不在本阶段生成）
```

### Source Code (repository root)

```text
scripts/releasekit/            # 标准库-only 发布工具包（不进入产品二进制）
├── image.go                   # BuildImage / VerifyImage / CheckImageContext / CheckBinaryTarget
│                              # BuildImageSBOM / BuildImageProvenance / LoadImageRecords /
│                              # CheckImageRecordFiles / ImageRecord.Validate
├── imagelock.go               # ImageLock / ReadImageLock / CheckImageLock
├── image_test.go              # TestUS18_Image_RejectsPrivateKeyInContext / _RecordValidate /
│                              # _AssembleBindsRecord
├── imagelock_test.go          # TestUS18_Image_LockMatchesSources
├── record.go                  # 追加 ReleaseRecord.Images（omitempty）、ReleaseManifest.Images
├── matrix.go                  # SupportRow.Kind、AppendImageRows
├── verify.go                  # VerifyImages、VerifyPolicy.RequireRepoDigest 接线
├── release.go                 # AssembleOptions.Images 与 assemble 内的读取 / 复制 / 校验顺序
├── docs_test.go               # known 映射补 image；TestUS18_Docs_ImageCommands
└── cmd/releasekit/main.go     # image build / image verify 子命令

cmd/basalt-verify/             # 独立校验器
├── main.go                    # --require-image-digest → VerifyPolicy.RequireRepoDigest
└── main_test.go               # TestUS18_Verify_RequireImageDigestFlag

docker/                        # 部署单元（全部新增）
├── Dockerfile                 # ARG BASE_IMAGE；runtime 阶段 USER 10001:10001
├── entrypoint.sh              # resolve_dsn / seed_config / exec
├── basalt.yaml                # 容器配置模板（六项 ingress 用 ${BASALT_INGRESS_*}）
├── compose.yaml               # 应用 + PostgreSQL；三卷两 secret；安全选项
├── compose.test.yaml          # 测试 overlay（extra_hosts；不进生产编排）
├── initdb/10-extensions.sql   # 空库首启创建 vector / pg_trgm
├── init-secrets.sh            # 生成 secret、设属主、写后 stat 复核；无 --force
├── backup.sh / restore.sh     # 四件套备份 / 全新项目恢复
├── .env.example               # 镜像 / 端口 / 库名 / 六项 ingress 示例值（无凭据）
└── secrets/.gitkeep           # 占位；实际 secret 文件被 .gitignore 排除

internal/e2e/                  # process 层（构建 tag e2e；仅测试）
├── us18_image_test.go         # A1 真实镜像构建与记录
├── us18_container_test.go     # A4–A17 Compose 用例
├── us18_stack_test.go         # composeStack / cleanupResidue / secret 属主助手
└── us18_fixture_test.go       # 夹具：临时项目名、端口、secret、mock 模型

harness/stages/
├── us18-base.json             # 源码级检查
├── us18-linux.json            # 真实容器检查
├── us18-windows.json          # Windows 经 WSL 包装
└── us18-aggregate.json        # 汇总与判别力映射

.github/workflows/ci.yml       # 新增 container job（不改 test job 名称）
release/image-lock.json        # app_base / postgres 的 digest 单一来源
.gitignore                     # docker/secrets/* 与 !docker/secrets/.gitkeep
```

**Structure Decision**: 沿用单 Go module 与既有包结构，不新增 `internal/` 包。发布工具扩展放在 `scripts/releasekit`（标准库-only，独立 CLI），部署资产放仓库根 `docker/`，容器验收与既有 e2e 同包（`internal/e2e`，构建 tag `e2e`）。生产二进制、`core`、存储层与契约目录零改动。

## Complexity Tracking

> 无宪法违规项，无需豁免。

| 决策 | 为什么需要 | 更简单的替代被否原因 |
| --- | --- | --- |
| `releasekit` 通过 `docker` CLI 调用构建镜像，不引入 Docker SDK | 保持标准库-only 与依赖最小化（原则 VIII） | Docker SDK 新增大量第三方坐标，且会把构建行为绑到库版本 |
| 容器 e2e 放 `internal/e2e` 并用 `-tags=e2e` | 与既有测试分层一致，默认 `go test ./...` 不拉长产物测试 | 新建独立测试模块会复制夹具并破坏“单一验收台账” |
| 镜像记录由 `AssembleRelease` 直接写入记录与矩阵 | 避免“汇总后补字段”的第二条路径（U18-Q1） | 事后追加字段无法保证 `SHA256SUMS` / 签名顺序 |
| secret 由宿主 / 一次性 root 容器显式设属主 | file 型 secret 是绑定挂载，Docker 不做属主转换 | 改 0444 或 world-readable 违反凭证红线；容器内 chown 需要额外能力 |

## Phase 0 / Phase 1 输出

| 工件 | 内容 |
| --- | --- |
| [research.md](research.md) | 6 项关键技术裁决：配置占位符与 int 字段（已实测）、secret 属主与 9p 限制、镜像内版本 / 哈希核对、镜像记录接线与兼容、CI 清理前缀、WSL 本地验收 |
| [data-model.md](data-model.md) | `ImageRecord` / `ScanRecord` / `ImageLock` / `SupportRow.kind` / `VerifyPolicy.RequireRepoDigest` 字段与校验；卷 / secret / 实例目录布局 |
| [contracts/image-cli.md](contracts/image-cli.md) | `releasekit image build` 与 `image verify`、`assemble --image-record`、`basalt-verify --require-image-digest` 的命令、输入输出与失败语义 |
| [contracts/deploy-runtime.md](contracts/deploy-runtime.md) | 容器部署的可观察契约：退出码、日志字面量、secret / 卷权限、健康检查与停机 |
| [quickstart.md](quickstart.md) | 从源码测试到真实镜像、Compose、备份恢复、升级回退的可执行验证步骤 |

## Post-Design Constitution Re-check

Phase 1 设计未新增宪法面风险：仍零新增 Go 坐标、零迁移、零契约变更；镜像上下文守卫在 `docker build` 前拒绝私钥 / secret（原则 V 的凭证红线在部署面同样 fail-closed）；e2e 只用 mock 模型且 external 层不执行；`container` job 与 `test` job 分离，`-race` 不进入发布构建。**结论：宪法检查保持通过。**

## 实施顺序

1. `releasekit` 镜像面（image.go / imagelock.go / record / matrix / verify / release / CLI）与源码层测试；
2. `cmd/basalt-verify` flag 接线与测试；
3. `docker/` 部署资产与 `release/image-lock.json`、`.gitignore`；
4. `internal/e2e` 助手与 A1–A17 用例；
5. `harness/stages/us18-*.json` 与 CI `container` job；
6. 文档同步（Operations / TechnicalDesign §7.2 §8.2 / Requirements §6.3 / Compatibility / README）；
7. 全量门禁、真实容器验收、备份恢复与 CI 实跑；收口材料与人工走查实例。
