# Tasks: US-18 Docker／Compose 单实例部署

**Input**: [spec.md](spec.md)、[plan.md](plan.md)、[research.md](research.md)、[data-model.md](data-model.md)、[contracts/](contracts/)；模块卡 [docs/us/us18-docker-compose.md](../../docs/us/us18-docker-compose.md) R8

**Tests**: 本仓按 Harness 先行执行——每个用户故事的测试与实现同任务或先行落地；关键回归测试的断言逐字取自模块卡 §4.5。

**Organization**: 按 `spec.md` 的六个用户故事分阶段；Phase 1–2 为共享前置，Story 阶段按优先级 P1 → P2。

## Format: `[ID] [P?] [Story] Description`

- **[P]**: 可并行（不同文件、无未完成依赖）
- **[Story]**: 对应 `spec.md` 的用户故事
- 所有路径相对仓库根

## Phase 1: Setup（共享基础设施）

**Purpose**: 固定输入与基础镜像 digest，先立 Harness 判据

- [x] T001 核对基线并读取编码规范：确认 `git rev-parse HEAD` 为 `09fce24…`、分支 `018-docker-compose`；读 `docs/EngineeringStandards.md` §1–§4 与 `docs/DevelopmentLoop.md` §7 陷阱表相关条目；确认 `scripts/releasekit` 只依赖标准库
- [x] T002 [P] 解析并写 `release/image-lock.json`：用 `docker buildx imagetools inspect` 或 `docker pull` + `docker inspect` 取得 `alpine:3.23` 与 `pgvector/pgvector:0.8.7-pg17-bookworm` 的 digest；字段 `schema/app_base/postgres`；取不到 digest 时不得写猜测值，登记未验证并停在该任务
- [x] T003 [P] 按模块卡 §4.3 生成 `harness/stages/us18-base.json`、`harness/stages/us18-linux.json`、`harness/stages/us18-windows.json`、`harness/stages/us18-aggregate.json`：`required_tests` 写全名，`inputs` 只列 git 跟踪文件（私有模块卡只进 base）
- [x] T004 [P] 配置忽略规则：根 `.gitignore` 追加 `docker/secrets/*` 与 `!docker/secrets/.gitkeep`（顺序不可反）；创建 `docker/secrets/.gitkeep`

---

## Phase 2: Foundational（阻塞前置）

**Purpose**: 镜像构建与记录的数据结构、构建/校验入口；US1 与 US6 共同依赖

**⚠️ CRITICAL**: 本阶段完成前不得开始用户故事

- [x] T005 [P] 在 `scripts/releasekit/image.go` 新增 `ImageInputs`、`ImageRecord`、`ScanRecord`、`VerifyImageOptions`、`OSPackage` 与 `ImageRecord.Validate`：字段逐字对齐模块卡 §3.1；校验必填、`platform == linux/amd64`、`tier == binary`、哈希格式、SBOM/provenance 路径为记录目录内的文件名
- [x] T006 [P] 在 `scripts/releasekit/image.go` 实现 `CheckBinaryTarget(path string) (Target, error)`：只读 buildinfo 核对 `GOOS/GOARCH/CGO_ENABLED=0`，不读版本；错误点名实际读数
- [x] T007 [P] 在 `scripts/releasekit/image.go` 实现 `CheckImageContext(dir string) error`：文件名命中私钥/secret/私有路径，或文件内容含 `BEGIN PRIVATE KEY` 即拒绝并点名文件；正常五件套放行
- [x] T008 在 `scripts/releasekit/image.go` 实现 `BuildImage(ctx, ImageInputs) (ImageRecord, error)`：`ReadImageLock` → `CheckBinaryTarget` → `CheckImageContext` → `docker build --build-arg BASE_IMAGE=<digest>`；镜像内 `--entrypoint /opt/basalt/basalt … --version`（只解析 stdout 的 `basalt version <v>`）与 `sha256sum /opt/basalt/basalt`；失败删镜像；写 `image-record.json`
- [x] T009 [P] 在 `scripts/releasekit/image.go` 实现 `BuildImageSBOM`（CycloneDX 1.5，归档组件 + `OSPackage`，未知许可证 `["NOASSERTION"]`）与 `BuildImageProvenance`（自定义 v1；记录二进制哈希、base digest、模板哈希、时间）
- [x] T010 [P] 在 `scripts/releasekit/image.go` 实现 `VerifyImage`、`LoadImageRecords`、`CheckImageRecordFiles`：逐条 `Validate`、核对文件存在与哈希、`RequireLocalImage` 时 `docker image inspect` 的 Id == `local_id`、`RequireRepoDigest` 时校验 `sha256:<64 hex>`
- [x] T011 [P] 新建 `scripts/releasekit/imagelock.go`：`ImageLock/ReadImageLock/CheckImageLock`；比对面固定为 Dockerfile `ARG BASE_IMAGE`、compose 数据库镜像、`.env.example` 的 `BASALT_DB_IMAGE`
- [x] T012 在 `scripts/releasekit/cmd/releasekit/main.go` 新增 `image build` 与 `image verify` 子命令：参数按 [contracts/image-cli.md](contracts/image-cli.md)；未知参数与缺失必填均非零退出
- [x] T013 [P] 新建 `scripts/releasekit/image_test.go` 与 `imagelock_test.go`：落地模块卡 §4.5 的 `TestUS18_Image_RejectsPrivateKeyInContext`（三层断言），补 `TestUS18_Image_RecordValidate`（篡改哈希、错误平台/档位、路径越界）与 `TestUS18_Image_LockMatchesSources`（A3，改任一处即红）
- [x] T014 [P] 新建 `internal/e2e/us18_image_test.go` 的 `TestUS18_Image_BuildAndRecord`：真实 `docker build`（`-tags=e2e`）；断言 `linux/amd64`、`tier=binary`、`local_id == docker image inspect Id`、`binary_sha256` 一致、重复构建除 `local_id`/`provenance_sha256` 外字段一致；`BASALT_E2E_REQUIRE_DOCKER=1` 时缺 daemon/镜像为 `t.Fatal`

**Checkpoint**: `go test -run '^TestUS18_' ./scripts/releasekit` 与 A1 镜像构建可独立通过；US1 / US6 可开始

---

## Phase 3: User Story 1 - 用官方镜像与 Compose 起一个可登录的单实例 (Priority: P1) 🎯 MVP

**Goal**: 一份 Compose 单元能启动应用与 PostgreSQL，管理台可登录，缺项 / 缺 secret 在启动前失败

**Independent Test**: 在 ext4 路径执行 `docker compose config` + `up -d`，health 200，正确 token 200、错误 token 401

### Implementation for User Story 1

- [x] T015 [P] [US1] 新建 `docker/Dockerfile`：`ARG BASE_IMAGE`（无默认值）；运行阶段 alpine + `/opt/basalt/basalt` + `entrypoint.sh` + `basalt.yaml` + `LICENSE`；`USER 10001:10001`、`WORKDIR /data`、`ENTRYPOINT ["/opt/basalt/entrypoint.sh"]`、`CMD ["serve"]`；不安装 build 工具、不下载依赖
- [x] T016 [P] [US1] 新建 `docker/entrypoint.sh`：`set -eu`；`resolve_dsn` 从 `BASALT_DATABASE_DSN_FILE` 读入并导出、缺失退出 78 且点名路径；`seed_config` 仅在 `/data/basalt.yaml` 缺失时从 `/opt/basalt/basalt.yaml` 复制；`exec /opt/basalt/basalt "$@"`；不打印 DSN
- [x] T017 [P] [US1] 新建 `docker/basalt.yaml`：`http.addr 0.0.0.0:8080`、`admin_auth.mode token`（不写 token）、`database.dsn ${BASALT_DATABASE_DSN}`、六项 ingress 用 `${BASALT_INGRESS_*}` 占位、回环 `allowed_hosts`/`allowed_origins`
- [x] T018 [US1] 新建 `docker/compose.yaml`、`docker/compose.test.yaml`、`docker/initdb/10-extensions.sql`、`docker/.env.example`：字面量逐条对齐模块卡 §3.3（三卷、两 secret、`read_only`、`cap_drop: [ALL]`、`no-new-privileges`、`init: true`、`stop_grace_period: 90s`、`depends_on: service_healthy`、`${VAR:?}` 六项 ingress、`127.0.0.1` 默认端口）；`initdb` 只 `CREATE EXTENSION IF NOT EXISTS vector/pg_trgm`
- [x] T019 [P] [US1] 新建 `docker/init-secrets.sh`：要求 root（非 root 打印 `docker --user 0` 等效命令并退出）；`/dev/urandom` 生成 URL-safe 口令；`postgres_password` 0:0 0400、`database_dsn` 10001:10001 0400；已有文件拒绝覆盖；写后 `stat` 复核，不符非零退出
- [x] T020 [US1] 新建 `internal/e2e/us18_stack_test.go` 与 `us18_fixture_test.go`：`composeStack` / `newComposeStack` / `up` / `upExpectFailure` / `down` / `stopService` / `startService` / `logs` / `exec` / `adminToken` / `instanceID` / `request` / `backup` / `restore` / `cleanupResidue` / `repoRoot` / `buildLinuxAMD64Binary` / `mustImageLock` / `waitFor` / `writeSecret` / `fixSecretOwnership` / `secretStat`；项目名与端口隔离按模块卡 §4.7
- [x] T021 [US1] 在 `internal/e2e/us18_container_test.go` 落地 `TestUS18_Compose_ConfigRequiresSecrets`（A4）：缺 `BASALT_IMAGE`/任一 ingress → `compose config` 非零并点名；缺 secret → up 失败点名 `database_dsn`；ingress 置零 → `serve` 退出 1 且日志含 `服务启动要求下列配置项大于零`；全齐 → config 成功且输出不含口令
- [x] T022 [US1] 落地 `TestUS18_Compose_AdminLogin`（A5）：`up -d --wait` 后 60 秒内可登录（SC-001）；正确 token 200、无/错 token 401、token 只从私有卷读出；断言容器内 `id -u == 10001`、根文件系统只读（写 `/` 失败）
- [x] T023 [US1] 落地 `TestUS18_Compose_TaskAndKnowledge`（A6）：mock 模型完成一次 Run（`GET /api/v1/runs/{id}` 终态 `completed`、回复非空）；上传 `.md` → 摄取 → `POST /api/v1/search` 命中同一片段
- [x] T024 [US1] 落地 `TestUS18_Cleanup_NoResidue`（A15）：项目名以 `us18-` 前缀 + 随机后缀；`down -v` 后按同一前缀核对容器 / 卷 / 网络 / 镜像四类残留为零；全程不对用户实例执行命令（外部 review 修复②后，镜像类改由（tag, LocalID）登记 + 归属核对删除，不再依赖 tag 前缀扫描；A15 四类判据不变，见 run-state 失败台账）

**Checkpoint**: US1 独立可验收（A1、A3–A6、A15）；MVP 可演示

---

## Phase 4: User Story 2 - 容器重建后实例身份、凭据与数据仍可用 (Priority: P1)

**Goal**: 卷布局使实例身份、admin token、工作区数据在重建后保留

**Independent Test**: `up -d --force-recreate basalt` 后容器 ID 变、`instance_id` 不变、旧 token 200、知识仍可检索

- [x] T025 [US2] 按模块卡 §2.7 复核并修正 `docker/compose.yaml` 的卷映射与 `XDG_DATA_HOME`/`HOME`：`private` → `/var/lib/basalt`，`workspace` → `/data`；确认 `private` **不在** `/data` 下
- [x] T026 [US2] 落地 `TestUS18_Compose_RecreatePreservesIdentity`（A7）：重建前后 `instance_id` 相等、旧 admin token 仍 200、知识数据仍在；`docker exec` 确认私有目录位于 `/var/lib/basalt`

**Checkpoint**: US2 独立通过；与 US1 数据面一致

---

## Phase 5: User Story 3 - 故障有可判定的行为（数据库、凭据、卷与停机） (Priority: P1)

**Goal**: 数据库不可用、错误凭据、卷 / secret 权限、SIGTERM、崩溃、第二实例全部有可观察且可区分的失败语义

**Independent Test**: 每个故障单独复现，断言状态码 / 退出码 / 日志字面量，修好后自动恢复

- [x] T027 [US3] 落地 `TestUS18_Compose_DatabaseUnavailable`（A9）：停 PG 后 health 200、`GET /api/v1/runs` 5xx、`/data` 无 `*.db`/`*.sqlite`；起 PG 后同请求 200
- [x] T028 [US3] 落地 `TestUS18_Compose_WrongCredentials`（A10）：本项目 secret 写错口令 → 退出非零；日志含 `连接 PostgreSQL 失败` 或 `打开 PostgreSQL`；不含口令 canary
- [x] T029 [US3] 落地 `TestUS18_Compose_VolumePermissions`（A11）：工作区挂载不可写 → 非零退出并点名路径；属主改 10001 后可启动；若现有启动路径不检查可写性，在 `internal/cli` 的启动前检查中补 fail-closed（点名路径）
- [x] T030 [US3] 落地 `TestUS18_Compose_SecretPermissions`（A17）：错误属主 → 退出非零 + `Permission denied`；修正后 healthy；`stat` 断言 `root:root 0400` / `10001:10001 0400` 且 `mode & 0004 == 0`；容器 UID 10001
- [x] T031 [US3] 落地 `TestUS18_Compose_GracefulShutdown`（A12）：IDLE 与会话运行（60s）`docker compose stop basalt` 退出码 0 且 ≤ 90s，在途 Run 终态；手动补跑分支不作为“退出 0”样本
- [x] T032 [US3] 落地 `TestUS18_Compose_RestartDoesNotRerun`（A13）：在途 Run 时 `docker compose kill basalt` → `up -d` → `running → failed`、半截工具组 `unknown`、mock 无新增调用
- [x] T033 [US3] 落地 `TestUS18_Compose_SecondInstanceRejected`（A14）：共享同卷与数据库的第二个应用容器退出非零，日志含 `工作区实例锁已被占用`

**Checkpoint**: US3 全部故障用例独立通过；失败路径不吞错、不回显凭证

---

## Phase 6: User Story 4 - 四件套备份与全新项目恢复 (Priority: P1)

**Goal**: 数据库、工作区、私有卷、secret 可配套备份并在全新项目恢复

**Independent Test**: 备份 → 全新项目恢复 → 旧 token / 旧知识 / `instance_id` 均可用，secret 权限正确

- [x] T034 [P] [US4] 新建 `docker/backup.sh`：要求 root 或 `--user 0`；`stop basalt` → `pg_dump -Fc` → 打包 workspace/private 卷 → 复制 secrets（目录 0700、文件 0600）→ `SHA256SUMS` → `start`；失败不吞错；备份目录仅所有者可访问（FR-013）
- [x] T035 [P] [US4] 新建 `docker/restore.sh`：要求 root 或 `--user 0`；恢复前先按 `SHA256SUMS` 校验四件套（不符即停止）；全新项目：建卷、校正属主 10001、解包、`pg_restore`、放 secrets 并重设 `root:root 0400` / `10001:10001 0400`、`up -d`；拒绝覆盖已有数据卷（外部 review 修复①：清理提示只在本次运行已创建项目资源后给出；新增回归 `TestUS18_Compose_RestoreRefusesExistingProject`，见 run-state 失败台账）
- [x] T036 [US4] 落地 `TestUS18_Compose_BackupRestore`（A8）：四件套备份 → 新项目恢复 → 旧 token 200、旧知识命中、托管模型连接可用、`instance_id` 一致；恢复后 secret `stat` 符合规定

**Checkpoint**: US4 独立通过；备份件不进入仓库或测试日志

---

## Phase 7: User Story 5 - 镜像升级与受控回退 (Priority: P2)

**Goal**: 换镜像 `up -d` 走前向迁移；回退只支持“恢复升级前备份 + 旧镜像”

**Independent Test**: 镜像 A 写数据 → 备份 → 镜像 B `up -d` → 数据仍在；新项目恢复备份 + 镜像 A → 同数据可查

- [x] T037 [US5] 落地 `TestUS18_Compose_UpgradeRollback`（A16）：A→备份→B→数据/实例 ID/旧 token 可用；新项目 + 备份 + A 恢复；断言恢复后 secret 权限；跨 schema 兼容不在本用例（U-18-07）
- [x] T038 [US5] 在 `docker/` 与 `docs/Operations.md` 草案中写明升级 / 回退命令链，禁止裸镜像降级；不新增自动升级入口

**Checkpoint**: US5 独立通过；升级不改变实例身份

---

## Phase 8: User Story 6 - 镜像进入发布记录与校验链 (Priority: P2)

**Goal**: 镜像记录 → assemble → verify 单一路径贯通；矩阵按 `(goos,goarch,kind)` 分行；`basalt-verify` 支持 digest 强制

**Independent Test**: `image build` → `assemble --image-record` → `basalt-verify` 通过；篡改任一镜像字段 / 删 `images[]` 留下矩阵行 → 失败

- [x] T039 [P] [US6] 在 `scripts/releasekit/types.go` / `record.go` / `manifest.go` 为 `ReleaseRecord`、`ReleaseManifest` 增加 `Images []ImageRecord json:"images,omitempty"`；旧记录无该字段仍合法
- [x] T040 [P] [US6] 在 `scripts/releasekit/types.go` / `matrix.go` 为 `SupportRow` 增加 `Kind string json:"kind,omitempty"`；实现 `AppendImageRows(matrix, images) (SupportMatrix, error)`：每行先 `CheckSupportRow`，`tested` 必须有 check ID、`compiled-only`/`not-tested` 必须有原因，同 `(goos,goarch,kind)` 重复即拒绝，任一行失败整次不生效
- [x] T041 [US6] 在 `scripts/releasekit/verify.go` 实现 `VerifyImages(dir, record, matrix, opts)`：矩阵含任一 `kind=image` 行或 `record.Images` 非空即调用；逐条 `Validate`、文件哈希、矩阵 `(goos,goarch,kind=image)` 一一对应；`RequireRepoDigest` 校验格式；改动 `crossCheckRecord` 只处理 `kind` 为空的行
- [x] T042 [US6] 在 `scripts/releasekit/types.go` / `verify.go` 为 `VerifyPolicy` 增加 `RequireRepoDigest bool json:"require_repo_digest,omitempty"`；`VerifyRelease` 把矩阵与策略传入 `VerifyImages`
- [x] T043 [US6] 在 `scripts/releasekit/release.go` / `types.go` 为 `AssembleOptions` 增加 `Images []string` 并在 `AssembleRelease` 中：`LoadImageRecords` → 复制 SBOM/provenance（保持文件名、重名即失败）→ `AppendImageRows` → 写 record/manifest → 再走 `WriteChecksums` 与签名；不提供事后补字段路径
- [x] T044 [US6] 在 `cmd/basalt-verify/main.go` 增加 `--require-image-digest`，映射到 `VerifyPolicy.RequireRepoDigest`；无镜像记录的旧目录不因该 flag 失败
- [x] T045 [US6] 扩展 `scripts/releasekit/verify_test.go` 与新增 `scripts/releasekit/assemble_image_test.go`：落地 `TestUS18_Image_AssembleBindsRecord`（贯通 + 篡改 SBOM / 删 `images[]` 刷新校验和 / 矩阵孤儿行均失败）与 `TestUS18_Verify_ImagesRecord`（含无镜像字段的旧目录仍通过）；新增 `cmd/basalt-verify/main_test.go` 的 `TestUS18_Verify_RequireImageDigestFlag`（空 / 畸形 digest 返回 1，合法 0；无镜像旧目录返回 0）
- [x] T046 [US6] 扩展 `scripts/releasekit/docs_test.go`：`known` 映射补 `image`；新增 `TestUS18_Docs_ImageCommands` 对账 `docs/Operations.md` 中的 `releasekit image` / `basalt-verify` 命令；确保 `TestUS17_Docs_CommandsMatchCLI` 仍绿

**Checkpoint**: US6 独立通过；`go test ./scripts/releasekit ./cmd/basalt-verify` 全绿

---

## Phase 9: Polish & Cross-Cutting Concerns

- [x] T047 定稿 `harness/stages/us18-*.json`：`required_tests` 与模块卡 §4.3 逐字一致；`us18-windows.json` 用 `wsl -d Ubuntu -- bash -lc` 包装；`us18-aggregate.json` 汇总并记录判别力映射
- [x] T048 在 `.github/workflows/ci.yml` 新增 `container` job（不改 `test` job）：匿名拉固定 digest、`make build`、`--stage-only` 跑 `us18-linux`、`--out` 与 artifact 上传、`if: always()` 按 `us18-ci-<run_id>-` 枚举项目并清理、断言四类残留为零（PR #2 CI run 41521764079 / job 125539295588：6/6 检查、15 用例、0 跳过、零残留；证据见 run-state §5）
- [x] T049 [P] 更新 `docs/Operations.md`：容器部署、secret 准备（含 9p 限制）、备份 / 恢复 / 升级 / 回退、故障排障；命令与 `docker/` 实际脚本一致
- [x] T050 [P] 更新 `docs/TechnicalDesign.md` §7.2 / §8.2、`docs/Requirements.md` §6.3、`docs/Compatibility.md`、根 `README.md`：只补容器部署事实，不改产品契约与既有验收口径
- [x] T051 全量门禁：`make check`、`make cover`、`go test -race`、`sh scripts/check-acceptance.sh`（含 `us18-base` 与 `us18-linux`）；逐目标四态记录，失败即修（本机 `make check` 与 11 项全量 profile 通过；`us18-linux` 由草稿 PR 的 `container` job 判卷，证据见 run-state §5）
- [x] T052 在 `018-docker-compose` 提交、推送并创建草稿 PR，跟进 CI；失败按失败台账修复；**不合并 main**（提交 `8b71421`＋digest 修正 `2891ece`；草稿 PR #2；run 41521764079 六 job 全绿。首次 CI 拉取失败按 F11 修复，push 重跑同步跟进）
- [x] T053 准备人工走查实例与收口材料：启动实例、访问方式、简明验收步骤、测试 / CI 证据、已知限制；更新 `run-state.md` 未验证清单与收口结论为“待人工 review ②”（实例 `basalt-us18-demo` 运行于 127.0.0.1:8080；U-18-10 已按 CI 实跑登记；收口结论保持“待人工 review ②”，材料见交付说明）

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**：无依赖，可立即开始
- **Foundational (Phase 2)**：依赖 Setup；**阻塞 US1 与 US6**
- **US1 (Phase 3)**：依赖 Phase 2；后续 US2–US5 均依赖 US1 的 Compose 资产
- **US2 / US3 / US4 (Phase 4–6)**：依赖 US1；彼此可并行（不同测试文件）
- **US5 (Phase 7)**：依赖 US1 + US4（备份件）
- **US6 (Phase 8)**：依赖 Phase 2；可与 US2–US5 并行
- **Polish (Phase 9)**：依赖所有故事完成；T048 依赖 T047

### Within Each User Story

- 测试与实现同任务落地；关键回归先看到预期失败文案
- 先数据 / 类型，再服务 / 接线，再容器用例
- 每个任务完成后写 `run-state.md` 并勾选

### Parallel Opportunities

- T002 / T003 / T004 可并行；T005–T007、T009–T011、T013 可并行
- T015–T017、T019 可并行（不同文件）；T018 需在 T015–T017 之后定稿字面量
- T034 / T035 可并行
- T039 / T040 可并行；T041 依赖 T040；T043 依赖 T039–T042
- T049 / T050 可并行

---

## Implementation Strategy

### MVP First

1. Phase 1 → Phase 2 → US1
2. 验证 A1、A3、A4、A5：镜像可构建、Compose 可启动、管理台可登录
3. 再依次完成 US2 → US3 → US4 → US5 → US6

### Incremental Delivery

- 每个故事完成后跑该故事的全部 `TestUS18_*`，绿后再进下一个
- 源码层（releasekit / basalt-verify）与容器层（internal/e2e）分开跑，容器层缺 Docker 时 SKIP 并登记，不冒充通过
- CI 只跑 `us18-linux`（git 跟踪输入），私有模块卡不进 CI stage

## Notes

- [P] = 不同文件、无未完成依赖
- 判据面：只允许新增测试、等价重构、随已批准接口适配；不得删断言或放宽阈值
- 失败 / 跳过 / 未验证一律如实写进 `run-state.md`；同一问题修复 3 次不收敛即走 `/debug`
- commit / push 仅限 `018-docker-compose`；最终合并不在本任务列表内
