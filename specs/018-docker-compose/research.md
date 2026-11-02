# Phase 0 Research: US-18 Docker／Compose 单实例部署

**Date**: 2026-11-02 | **Feature**: [spec.md](spec.md) | **Plan**: [plan.md](plan.md)

本文件只记录**影响实现路径**的技术裁决；产品范围与验收语义以模块卡 R8 与 `spec.md` 为准。

## R-18-01 配置模板中的 `${BASALT_INGRESS_*}` 能否落进整数字段

**Decision**: `docker/basalt.yaml` 的六项 ingress 直接写 `${BASALT_INGRESS_*}` 占位；值由 `docker/.env` 经 Compose `environment` 注入；`compose.yaml` 用 `${VAR:?message}` 在 `docker compose config` 阶段就拒绝缺项。

**Rationale**: 实测（临时探针测试，已删除）——`internal/cli` 的 `LoadConfig` 先 `resolveTree` 把占位替换为字符串，再 `decodeConfig` 经 `goccy/go-yaml` 回写 / 反序列化；字符串 `"7"` 能成功解进 `int` 字段，`cfg.Callers.Ingress.Run.QPS == 7`。因此不需要在镜像里预生成配置、也不需要新增配置键。

**Alternatives considered**:

- 启动脚本用 `sed` 替换模板 → 多一套模板语义，且值会进入镜像可写层或命令行；
- 新增 `INGRESS_*` 专用环境变量解析 → 违反“不新增配置键”的边界；
- 在 entrypoint 写临时 YAML 到 `/tmp` → 配置进可写层、崩溃后不可见，排障困难。

**Residual risk**: 探针只覆盖 `qps` 一个 int 字段；`burst` 同类型同路径，风险相同。A4 会在真实 Compose 中复核缺项与零值两种失败。

## R-18-02 宿主 secret 的属主与权限

**Decision**: 两个 secret 文件均由宿主 root（或一次性 root 容器）创建并设属主：`postgres_password` 为 `root:root 0400`，`database_dsn` 为 `10001:10001 0400`。`init-secrets.sh` 需要 root，已有文件一律拒绝覆盖（无 `--force`），写后 `stat` 复核；恢复时由 `restore.sh` 重设同样权限。

**Rationale**: Compose 的 file 型 secret 是**绑定挂载**，Docker 不会像 swarm secret 那样转换属主 / 权限；容器内看到的 uid/gid/mode 就是宿主上的值。PostgreSQL 官方 entrypoint 在读 `POSTGRES_PASSWORD_FILE` 后降权到 `postgres` 用户，因此该文件必须 root 可读且不对 10001 开放；应用以 10001 运行，只读自己的 DSN。用户 review ① 实测：宿主 `1000:1000 0600` 的文件在 10001 容器里读取失败（退出码 13）。

**Alternatives considered**:

- `0444` / world-readable → 凭证红线不允许；
- 容器内 entrypoint 以 root 启动再降权 → 破坏非 root、需要额外权限，且 secret 会先落到容器可写层；
- Docker configs / swarm secrets → 单机 Compose 不提供属主转换，且引入 Swarm 依赖。

**Residual risk**: WSL 的 `/mnt/d`（9p/drvfs）忽略 `chown`/`chmod` 且返回 0；脚本的写后 `stat` 复核必须在此非零退出（U-18-11）。本地走查改在 ext4 路径执行。

## R-18-03 镜像内版本与二进制一致性核对

**Decision**: `CheckBinaryTarget` 只用 buildinfo 核对 `GOOS`/`GOARCH`/`CGO_ENABLED=0`；`BuildImage` 在镜像落盘后执行 `docker run --entrypoint /opt/basalt/basalt <image> --version` 与 `sha256sum /opt/basalt/basalt`，分别对照 `ImageInputs.Version` 与 `ImageInputs.Binary` 的 SHA-256，不符即删镜像并报错。

**Rationale**: `main.version` 不是 `debug/buildinfo` 的独立字段；US-17 使用 `-trimpath`，Go 会省略 buildinfo 中的 `-ldflags`，无法从中恢复版本字符串。用户 review ① 已指出并实测该依据。

**Alternatives considered**:

- 从 buildinfo 读 `vcs.revision` / ldflags → 版本不是 VCS 修订，且 `-trimpath` 不可靠；
- 只信构建参数、不查镜像内二进制 → 无法发现 Dockerfile 拷贝了错误文件；
- 在镜像内加版本标签文件 → 多一个可漂移的事实源。

**Residual risk**: `--version` 的输出格式由既有 CLI 固定；若未来改变格式需同步 `BuildImage` 的解析与 A1。

## R-18-04 镜像记录接入发布流程

**Decision**: `assemble` 新增 `--image-record`（可重复）。`AssembleRelease` 在写 manifest / record / `SHA256SUMS` / 签名**之前**完成：`LoadImageRecords` 逐条 `Validate` 与文件哈希核对 → 把 SBOM / provenance 以**文件名不变**复制进发布目录 → `AppendImageRows` 追加 `kind=image` 矩阵行 → 写 `images[]` 到 record / manifest → 再走既有校验和与签名顺序。`VerifyRelease` 在“矩阵含任一 `kind=image` 行或 `record.Images` 非空”时调用 `VerifyImages`，并强制矩阵镜像行与 `images[]` 一一对应；`basalt-verify --require-image-digest` 映射到 `VerifyPolicy.RequireRepoDigest`。

**Rationale**: 现有 `AssembleRelease` 会重新生成 record / manifest；若只在汇总后追加字段，文件哈希与签名会覆盖不到镜像文件，形成“记录有字段、校验和没覆盖”的假证据。镜像记录路径以**记录所在目录**为基准（R7 裁决），复制进发布目录后保持文件名，避免同一文件两个名字。矩阵匹配按 `(goos, goarch, kind)`，空 `kind` 为归档行、`image` 为镜像行，防止同平台归档与镜像撞键。

**Alternatives considered**:

- 汇总后追加字段 → 违反 `SHA256SUMS` / 签名顺序，且需要第二条写记录路径；
- 新增第二套矩阵结构 → 破坏既有 `support-matrix.json` 消费方；
- 只按 `images[]` 非空触发校验 → 删掉 `images[]` 但保留矩阵镜像行的记录会假绿（A15 / `TestUS18_Verify_ImagesRecord` 的逆改点）。

**Residual risk**: 旧发布目录（无 `images[]`、矩阵无镜像行）必须继续通过；新增字段全部 `omitempty`，由 `TestUS17_*` 回归覆盖。

## R-18-05 CI 容器 job 与残留清理

**Decision**: `.github/workflows/ci.yml` 新增独立 `container` job（不改 `test` job 名称），`ubuntu-latest`、`timeout-minutes: 180`；匿名拉取两个固定 digest 的公开镜像（失败即红）；`BASALT_E2E_REQUIRE_DOCKER=1` 把“缺 daemon / 缺镜像”的跳过改判失败；执行 `sh scripts/check-acceptance.sh --stage-only --stage harness/stages/us18-linux.json`；失败时上传日志 artifact；清理步骤 `if: always()`——先 `docker compose ls -a --filter name=us18-ci-<run_id>-` 枚举真实项目并逐个 `down -v --remove-orphans`，再按**统一前缀** `us18-ci-<run_id>-` 删除残留容器 / 卷 / 网络 / 镜像，最后断言四类对象均无该前缀，有残留即 job 失败。

**Rationale**: 测试项目名是 `us18-ci-<run_id>-<用例名>-<随机后缀>`；`-p <prefix>` 只匹配精确项目名，会静默漏清理。Compose 卷名是 `<project>_<volume>`，project 段本身以 `us18-ci-<run_id>-` 开头，所以同一前缀既能匹配容器（`...-1`）也能匹配卷（`..._workspace`）——R8 已把 R7 的 `…_` 独立前缀删除。必须带 `--stage-only`，否则默认 full profile 因缺 `BASALT_TEST_PG_DSN` 在起跑前 exit 2，容器用例一条都跑不到。

**Alternatives considered**:

- 只用 `-p us18-ci-<run_id>` → 静默漏清理（已被 A15 与用户 review 指出）；
- 在 `test` job 内追加容器步骤 → 拉长必过检查并改变分支保护语义；
- 允许 pull 失败后跳过 → 干净 runner 会整套跳过，制造假绿。

**Residual risk**: runner 的 Docker / Compose 版本首次实跑后登记（U-18-10）；若受限网络下匿名 pull 失败，job 按设计失败而不是跳过。

## R-18-06 本地真实容器验收路径（Windows + WSL）

**Decision**: 本机验收走 WSL Ubuntu：`us18-windows.json` 用 `wsl -d Ubuntu -- bash -lc 'cd /mnt/d/code/basalt && …'` 包装 `go test -tags=e2e`；涉及 secret 属主的 Compose 手工走查把仓库（或至少 `docker/`）复制到 ext4 的 `~/basalt` 再执行。测试自身生成的 secret 在 `t.TempDir()`（WSL 的 ext4 `/tmp`），不受 9p 限制。

**Rationale**: Windows 侧无 Docker CLI；`/mnt/d` 是 9p/drvfs，`chown`/`chmod` 静默失效，`init-secrets.sh` 的 stat 复核会在该路径非零退出（这是预期行为，不是缺陷）。测试用临时目录可避开该限制，真实手工部署必须换原生文件系统或 Docker 卷。

**Alternatives considered**:

- 在 Windows 用 Docker Desktop → 本机未安装，且不改用户环境；
- 放宽 `init-secrets.sh` 的 stat 复核 → 让“权限没设上”的部署假绿；
- 把 secret 改成 Docker 卷 + 初始化容器 → 偏离 file 型 secret 的既定字面量。

**Residual risk**: WSL 内的 Docker 与 CI runner 版本可能不同；CI 实跑读数单独登记（U-18-10）。

## R-18-07 基础镜像 digest 的解析与固定

**Decision**: `release/image-lock.json` 固定 `app_base`（`alpine:3.23@sha256:…`）与 `postgres`（`pgvector/pgvector:0.8.7-pg17-bookworm@sha256:…`）。实现阶段用 `docker buildx imagetools inspect` 或 `docker pull` 后 `docker inspect --format '{{index .RepoDigests 0}}'` 取得**当前** digest 并写锁文件；`CheckImageLock` 保证 Dockerfile `ARG BASE_IMAGE`、compose 数据库镜像与 `.env.example` 的 `BASALT_DB_IMAGE` 与锁文件一致。

**Rationale**: 固定 digest 是 CI 与本地可复现的前提，也避免浮动 tag 在两次构建间漂移；Go 版本不进锁文件，`go.mod` 的 `toolchain` 与 CI `setup-go` 是唯一事实源。

**Alternatives considered**:

- 使用浮动 tag（`alpine:3.23`）→ 两次构建可能不同，镜像记录失真；
- 同时锁 Go patch 版本 → 与 `go.mod` 形成第二事实源；
- 自建基础镜像 → 超出本 US 范围并增加供应链面。

**Residual risk**: digest 依赖公开仓库可达；本地首次 pull 与 CI 匿名 pull 各一次，未拉取前不写“已验证”。
