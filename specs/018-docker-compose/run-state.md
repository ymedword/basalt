# US-18 Run State：Docker／Compose 单实例部署

> 1. 本文件是**过程状态，非设计工件**——speckit 命令不得把它当设计输入；
> 2. 失败台账**只追加**：一行一次尝试，不改旧行；
> 3. 唯一允许原地翻转的是未验证清单的「未验证 → 已验证」，翻转必须注日期与证据。

## 1. 基线

| 项 | 值 |
| --- | --- |
| 起始 commit | `09fce2446f37378cfd96c7365d299dad353cedf7`（`main`；US-17 收口记录） |
| 分支 / worktree | `018-docker-compose` / `D:\code\basalt` |
| 模块卡人审依据 | 用户 2026-11-02 回复「我确认 US18 R7 通过人工 review ①，授权你自动推进实施」（R7 文档；R8 为同轮授权的残留卷清理前缀修正） |
| 模块卡 | `docs/us/us18-docker-compose.md`，R8，sha256 `02dcc37a4a4d13ee33339ef1306036aaff723760c51710d5be49c60a4f476b65`（R8 首轮 `fd2a26f5…`；按用户授权修正 §4.7 残留卷清理前缀后重算） |
| spec | `specs/018-docker-compose/spec.md`，sha256 `0e457a7d8d145b7e9b218ca1fce841d2ae2a94bc1a8616365a0ab31d020ffde3`（clarify 后重算） |
| plan | `specs/018-docker-compose/plan.md`，sha256 `5e533639f478f5acf5a80ccae8dad5550a243e059a464510fd235e5843362535`；配套 `research.md` `f5f0076e…`、`data-model.md` `f29cbd7a…`、`quickstart.md` `0a392879…` |
| tasks（冻结时） | `specs/018-docker-compose/tasks.md`，53 项，sha256 `44829fbc513ad6bf419470d9bff79284a69e0ea650a65d328145a655e89efaf2`（analyze 前 `37120642…`；按分析补 SC-001 60s、备份目录 0700、恢复前校验和、旧目录兼容断言后重算）；AI 比对结论：交付物清单 §3.1–§3.3 逐项有任务（image.go / imagelock / record / matrix / verify / assemble / 两个 CLI / 5 个 docker 资产 / 6 个 e2e 文件 / 4 个 stage / ci job / 文档），22 个 `TestUS18_*` 全部映射到任务（A1 T014；A2 T013；A3 T013；A4 T021；A5 T022；A6 T023；A7 T026；A8 T036；A9 T027；A10 T028；A11 T029；A12 T031；A13 T032；A14 T033；A15 T024；A16 T037；A17 T030；Assemble/Verify/Flag/Docs 见 T045–T046），测试与实现同任务落地，无超出已审范围的新概念 |
| 开跑时未提交改动 | `specs/018-docker-compose/`（本 US 派生工件）；`docs/us/REGISTRY.md` 已由 `/us-register 18` 追加 US-18 行（私有排除，不在 git 变更面） |
| 允许触碰范围 | `scripts/releasekit/`、`cmd/basalt-verify/`、`internal/e2e/`、`docker/`、`release/image-lock.json`、`harness/stages/us18-*.json`、`.github/workflows/ci.yml`、`.gitignore`、`docs/Operations.md`、`docs/TechnicalDesign.md`、`docs/Requirements.md`、`docs/Compatibility.md`、`README.md`、`makefile` / `Makefile`（仅接线所需）、`specs/018-docker-compose/` |

## 2. 失败台账

| 日期 | 问题（测试名 / 首错位置 + 症状） | 假设 | 结果 | 同题计数 |
| --- | --- | --- | --- | --- |
| 2026-11-02 | T015 要求 `ARG BASE_IMAGE` 无默认值，但 A3 / `CheckImageLock` / T011 要求 Dockerfile 默认值与 `release/image-lock.json` 逐字一致 | 以 A3 与判卷代码为准保留默认值，T015 的字面例外按派生冲突记录 | Dockerfile 保留锁文件 digest 默认值；`CheckImageLock` 以此为比对面 | 1 |
| 2026-11-02 | 模块卡 §4.7 要求测试镜像 tag 以项目前缀开头，但 `BuildImage` 的 tag 固定为 `basalt:<version>-<os>-<arch>` 且 `ValidateVersion` 只收 `X.Y.Z` | 测试版本固定为合法 semver、镜像由 e2e 清理显式删除，并在 run-state 记录偏离 | e2e 使用 `basalt:0.0.1-linux-amd64`；TestMain 结束后 `docker image rm`；A15 仍按项目前缀核对容器 / 卷 / 网络，镜像按固定 tag 单独断言 | 1 |
| 2026-11-02 | 容器只发布随机宿主端口，而 `allowed_hosts` 模板固定 `127.0.0.1:8080`，U18-Q7 禁止新增配置键 | 测试请求显式携带 `Host: 127.0.0.1:8080`，拨号使用随机端口 | `composeStack.request` 设置 Host 头；被测实例无需改配置 | 1 |
| 2026-11-02 | 容器内 provider `api_key` 必须是 `${VAR}` 占位，测试没有对应环境变量 | 在 `docker/compose.test.yaml` 增加测试专用 `BASALT_US18_FAKE_KEY`（`${…:?}`），e2e 恒设该值 | 只在测试 overlay 出现，生产 compose 不含该变量 | 1 |
| 2026-11-02 | 卡片 §3.1 要求工作区不可写时在 `internal/cli` 启动前检查，但 CLI 层没有工作区路径上下文 | 在 `internal/app/run.go` 的 serve 初始化处加 fail-closed 可写检查（点名路径），T029 记录位置偏离 | 实现落 `internal/app`，行为判据不变 | 1 |
| 2026-11-02 | 卡片 §3.1 列 `repoRoot(t *testing.T) string`，与既有 `repoRoot() (string, error)` 重名 | 复用既有函数，夹具包装成 `us18RepoRoot(t) string` | 不新增第二个符号 | 1 |
| 2026-11-02 | `LoadImageRecords` 先查文件哈希再查平台重复，同平台两条记录共用文件名时报“哈希不符”而非“重复” | 平台重复判卷前置，再查文件哈希 | `TestUS18_Image_RecordFilesAndLoad` 绿；错误信息指向真实原因 | 1 |
| 2026-11-02 | 镜像 SBOM 计算了 apk 许可证却未写进组件，`TestUS18_Image_SBOMIncludesOSPackages` 红 | `sbomComponent` 增加 `licenses` 字段并按 CycloneDX `{license:{name}}` 形态写出 | 测试绿；未知许可证仍写 `NOASSERTION` | 1 |
| 2026-11-03 | `harness/stages/us18-{linux,windows,aggregate}.json` 的 protect 列了不存在的 `us18_faults_test.go` / `us18_backup_test.go` | 与模块卡 §4.3 对照，Compose 用例全部落在 `us18_container_test.go` | 三个 stage 的 protect 改为四个真实文件；CI 引用的 `us18-linux` 输入均为 git 跟踪面 | 1 |
| 2026-11-03 | `us18SeedConfig` 调 `docker compose create --no-start`，WSL Compose v5.6.1 报 `unknown flag: --no-start`，A6 / A7 / A12 三条用例起栈即红 | `create` 本身不启动容器，`--no-start` 是 `up` 的旗标 | 改为 `compose create`；重跑该批用例（证据见 §5） | 1 |
| 2026-11-03 | `go test ./scripts/acceptance -run TestStages_CIInputsGitVisible` 红：`specs/018-docker-compose/spec.md` 与 `release/image-lock.json` 尚未被 git 跟踪 | 本 US 尚未提交；stage 的 CI 输入在提交后进入跟踪面 | 待 T052 提交 / 推送后复跑该测试与 CI `container` job 再判 | 1 |
| 2026-11-03 | A6 / A7（TaskAndKnowledge、RecreatePreservesIdentity）起栈后 `us18EnableAgent` 30s 超时：`POST /agents/worker/enable` 恒 409 CONFLICT，日志显示 `期望 disabled/0，当前 disabled/1`，重试仍发 `expected_generation=0` | `GET /agents/{name}/files` 的 lifecycle 按设计只投影 state / valid / lifecycle_error，generation 恒为零值（views.go `agentFilesView` 注释明示「不编造」）；夹具误把它当权威代数 | 新增 `us18AgentGeneration` 从 `/api/v1/profiles` 读生命周期代数，`us18BuildAgent` 与 409 重试分支改用它；断言不变（仍须 30s 内启用成功）。复跑：Recreate / GracefulShutdown PASS，A6 暴露下一层问题（见下条） | 2 |
| 2026-11-03 | A6 `TaskAndKnowledge`：启用已通过，`us18Invoke` 的响应 `final_text` 正确，但 `GET /api/v1/runs/{id}` 的 `final_text` 为空 | 无状态 invoke 按设计只落 Run 与审计、**不**写消息（service.go「空 session_id → 仅内存」）；`final_text` 由该 Run 最新 assistant 消息派生（run_query.go `finalText`），无状态运行必然为空 | A6 改走会话入口：新增 `us18CreateSession` / `us18PostMessage`，创建会话后同步发消息再读账本；A6 判据（账本终态 completed、回复非空）不放宽，模型调用数与检索断言不变。复跑证据见 §5 | 2 |
| 2026-11-03 | A13 `RestartDoesNotRerun` 等不到 `running`：模型发起的 `shell` 调用 `/bin/sleep` 被拒（日志 `sandbox_denied`「交互式 shell 不得放行：/bin/sleep」） | 镜像基础层是 Alpine，`sleep` 等 applet 都是 busybox 的软链；执行侧按**真实路径**命中 shell 名称集合（`runner.go` 的 realpath 判定），Run 立即走完而非停在 running | 挂起点改为 FIFO：`write_file` 对无读者的命名管道 `open(O_WRONLY)` 阻塞；模型步骤改 `write_file`，`us18BuildAgent` 声明 `write_file`，起栈后用一次性 root 容器 `mkfifo` 代理目录下的 `block.pipe`。判据（在途 Run、SIGKILL、`failed` / `unknown`、无重跑）逐条不变。复跑 PASS（40.05s） | 1 |
| 2026-11-03 | A14 `SecondInstanceRejected` 日志断言恒空（实例锁行为本身正确：退出码 1、日志点名锁） | 容器的 slog 输出走 stderr，`docker logs` 把它留在 stderr；`us18DockerOutput` 成功时只回 stdout → 读到空串（非落盘竞态） | 新增 `us18DockerLogs` 合并两路输出并复跑；A14 PASS（30.70s） | 2 |
| 2026-11-03 | A8 / A16 备份步骤红：宿主 `sha256sum: secrets/database_dsn: Permission denied`，`TempDir` 清理也被 0700 的 `secrets/` 挡住 | 脚本按卡片用一次性 `--user 0` 容器写归档与 secret 副本，产物归 root；脚本同时承诺「免宿主 root」，两者冲突 | `backup.sh` 在写 SHA256SUMS 前用一次性 root 容器把整份备份目录 `chown -R` 回调用者 `$(id -u):$(id -g)`；卡片判据（备份目录 0700、secret 副本 0600）不变 | 1 |
| 2026-11-03 | A8 / A16 恢复步骤红：`invalid mount config for type "bind": bind source path does not exist: .../secrets/postgres_password` | Compose 的 file secret 是 bind mount，`create` 时就校验源文件存在；`restore.sh` 沿用卡片的「最后放 secret」顺序，必然晚于 `create` | secret 放置前移到 `create` 之前（用 `BASALT_IMAGE` 或源项目容器解析助手镜像），属恢复**步骤顺序**偏离卡片 §2.9，最终状态与属主 / 权限判据不变；顺带把 `create --no-start` 改成 `create`（同上条旗标问题）。复跑 A8 PASS（55.64s）、A16 PASS（68.90s） | 1 |
| 2026-11-03 | T051 首跑 `make check` 的 `sec` 目标红：gosec（dev 版）新规则 G122 命中 `scripts/releasekit/image.go:636` 的 `os.Open(path)`——WalkDir 回调内按走查路径打开，判 symlink TOCTOU；既有 `#nosec G304` 不覆盖 G122 | 按门禁纪律修实现（不是加抑制）：改用 `os.OpenRoot(dir)` + `root.Open(rel)` 做 root-scoped 打开，把读取限制在暂存目录内 | gosec 全仓零命中（`GOSEC_EXIT=0`，输出为空）；`TestUS18_Image_*` 单测复跑 `ok`；`make check` 整体复跑（证据见 §5） | 1 |
| 2026-11-03 | 续（上条之后）：`make check` 的 `test` 目标红，唯一失败 `TestStages_CIInputsGitVisible`——stage 的 inputs / protect 尚未进入 git 索引（本 US 未提交） | 与假设一致：测试用 `git ls-files --error-unmatch`，未跟踪即红；与代码缺陷无关 | `git add` 本 US 改动面后该测试复跑 PASS（0.49s）；随后整跑 `make check`，最终以提交后的门禁与 CI 复跑为准 | 1 |
| 2026-11-03 | 更正 2026-11-02 行「A15 … 镜像按固定 tag 单独断言」：`cleanupResidue` 的镜像类按 `Repository:Tag` 含项目名子串计数，固定 tag `basalt:<version>-linux-amd64` 不在前缀面内，该维在 A15 中恒为 0（当时并未单独断言） | 模块卡 §4.7 要求测试 tag 带项目前缀，正是为了让镜像类可扫描；固定 tag 偏离的后果需要显式判据 | `us18CleanupImages` 每个 tag `docker image rm` 后用 `docker image inspect` 复检，仍存在即失败；`TestMain` 把清理失败并入退出码。A15 容器 / 卷 / 网络仍为实断言；镜像类由 TestMain 复检承担。复跑 `TestUS18_Image_BuildAndRecord` PASS（32.78s，清理后 `basalt:*` 计数 0）；全 stage 复跑见 §5 | 2 |
| 2026-11-03 | T040 写 `AppendImageRows` 落 `types.go` / `matrix.go`，实际落 `scripts/releasekit/image.go`（与 `ImageRecord` / `BuildImage` 同文件） | 同包内位置选择，避免矩阵文件反向引用镜像记录类型；行为与判据不变 | 不改代码；位置偏离在此记录，`go test ./scripts/releasekit` 全绿 | 1 |
| 2026-11-03 | 独立复核 F1（HIGH）：`restore.sh` 在 `set -u` 下引用从未赋值的 `$project`，`BASALT_IMAGE` 缺失时的助手镜像兜底分支从未可达（e2e 恒设 `BASALT_IMAGE`，故未暴露） | 项目名必须由调用方显式给出，脚本不猜 | 必填 `COMPOSE_PROJECT_NAME` 并在入口校验；兜底分支改为由该变量解析 | 1 |
| 2026-11-03 | 独立复核 F2（MEDIUM）：`assemble` 没有 CLI 入口填充 `ImageRecord.Evidence`，矩阵镜像行 `tested` 分支只在单测可达，发布记录实际只能写 `not-tested` | 证据由发布编排显式给出，不在记录里静默补 | 新增可重复的 `--image-evidence`（必须与 `--image-record` 同用）；记录已带证据时只接受完全一致。新增 `TestUS18_Image_AssembleAppliesEvidence` / `RejectsConflictingEvidence`；`go test ./scripts/releasekit` PASS（40.994s） | 1 |
| 2026-11-03 | 独立复核 F3（MEDIUM）：`restore.sh` 把备份 secret 复制进共享 `docker/secrets`，会把仍在运行的源实例凭据覆盖掉 | 目标 secret 已存在且与备份不同即拒绝，恢复目标必须是独立部署目录 | 脚本增加一次性 root 容器 `cmp` 比对；`docs/Operations.md` §8.2 同步恢复目录要求 | 1 |
| 2026-11-03 | 独立复核 F4 + F9：`init-secrets.sh` 从环境变量读 `POSTGRES_DB` / `POSTGRES_USER`，而 `sudo` 会丢弃它们；拒绝覆盖的提示不含首启重试与轮换处置 | 从 secret 目录上一级的 `.env` 解析（环境变量优先），补齐库名 / 用户名字符集校验 | 与 `docs/Operations.md` §8.2 同步（一次性容器需 `-e`） | 1 |
| 2026-11-03 | 独立复核 F5：`backup.sh` 对 `compose start basalt` 用恒真兜底，吞掉重启失败仍打印成功 | 备份制品与恢复运行分开判：制品有效但应用未起必须非零退出并给定位命令 | 重启后核对 running；失败非零退出；文档同步 | 1 |
| 2026-11-03 | 独立复核 F6：CI 清理步骤在 `set -uo pipefail` 下枚举失败只得到空集，残留计数为 0——"没查成"会被读成"已清理" | 枚举失败必须让步骤失败 | 改 `set -euo pipefail`，四条枚举一律先落变量；只可经 PR CI 实跑验证（本地无 CI runner） | 1 |
| 2026-11-03 | 独立复核 F7（文档）：compose 未设 `restart` 策略，宿主 / 守护进程重启后容器不自启，文档未登记 | 保持不加自动拉起，登记恢复命令与排障入口 | `docs/Operations.md` §8.2「日常停机与重启」补条目 | 1 |
| 2026-11-03 | 独立复核 F8：`restore.sh` 中途失败会在目标项目留下已建卷 / 容器且无提示 | 失败路径必须给出可执行清理提示，成功路径不得误报 | EXIT trap 在未完成时打印 `docker compose -p <新项目> down -v` 提示；成功后置 `restore_done=1` | 1 |
| 2026-11-03 | 独立复核 F10：记录 / 矩阵的未知 `kind` 会被归档核对与镜像核对同时跳过，篡改矩阵可静默过关 | 取值集合只允许 `""` / `image`，双向显式拒绝 | `VerifyImages` 与 `crossCheckRecord` 各自拒绝；新增 `TestUS18_Image_RejectsUnknownEvidenceKind` 覆盖两处 | 1 |
| 2026-11-03 | 草稿 PR 首次 CI：`container` job 在「Pull pinned public base images」步骤红——`docker.io/library/alpine@sha256:7cb3c9…` 返回 `not found`（本机 e2e 全绿是因为镜像已在本地，CI 才是首次真实拉取） | 锁文件里两个 digest 已非上游现行值：本机守护进程使用 containerd 镜像存储（`overlayfs` / containerd snapshotter），镜像 ID 即 manifest digest，`RepoDigest == ID` 属正常现象（先前“合成引用”推断错误，已按此更正）。经两个独立 pull-through 镜像站按 tag 取到的 manifest 字节哈希与响应头一致且两站相同（alpine `ff7f0c72…`、pgvector `7d0a41…`），按 digest 请求均 200；旧值不在当前任何多架构索引中。本机无法直连 Docker Hub，拉取时只能经镜像缓存，缓存给出的旧 manifest 已在上游被替换 / 回收 | 锁文件、`Dockerfile` 的 `ARG BASE_IMAGE` 默认值、`compose.yaml`、`.env.example` 四处同步改为上游 digest（`alpine:3.23@sha256:ff7f0c72…`、`pgvector/pgvector:0.8.7-pg17-bookworm@sha256:7d0a41…`）；`go test ./scripts/releasekit ./cmd/basalt-verify` 通过。本机无法重跑镜像构建（Docker Hub 不可达、无 sudo 配置镜像代理），最终由 PR CI `container` job 复判。模块卡 §2.4 的字面 digest 由锁文件单一维护，卡片文本不改，以锁文件为准 | 1 |
| 2026-11-03 | 外部 review 发现①（HIGH）：`restore.sh` 的 EXIT trap 在前置检查**之前**注册，目标项目已有卷 / 容器的拒绝路径也会打印 `docker compose -p <项目> down -v`——提示删除的正是别人的数据 | 清理提示只应对**本次运行实际创建**的资源给出；前置检查拒绝时项目资源不属本次运行 | 引入 `created_project` 门：置 1 紧邻 `restore_compose create`（此前已确认项目无卷 / 容器）；顺带补容器前置检查（原只有卷检查）。新增回归 `TestUS18_Compose_RestoreRefusesExistingProject`：预置带 compose 标签的卷，断言非零退出、点名「已存在卷」、输出**不含** `down -v`、卷仍在。反向验证：临时去掉 `created_project` 门后该用例 FAIL 并复现危险提示，恢复后 PASS（0.58s） | 1 |
| 2026-11-03 | 外部 review 发现②（HIGH）：容器 e2e 固定使用 `basalt:0.0.1-linux-amd64`，真实 `docker build` 会覆盖宿主同名 tag；清理按 tag 名 `docker image rm`，宿主原有镜像就此失去标签 | 测试侧隔离即可消除破坏面，不必改 releasekit 的 tag 推导与 `ValidateVersion`（用户明确不借此重构发布工具） | 三处：① 测试版本每次运行随机（`0.0.<100000000+seed+偏移>`，仍满足 `X.Y.Z`），tag 自然唯一；② 镜像登记改为（tag, LocalID）对，同一 tag 重复构建以最后登记的 ID 为准；③ 清理前 `docker image inspect` 核对现指向 ID 与登记一致，不一致保留不动并说明。新增回归 `TestUS18_Image_PreExistingTagPreserved`：预置 `basalt:0.0.1-linux-amd64`，断言构建不沿用该 tag、归属不匹配拒绝删除、预置 ID 原样保留、阳性对照（探针 tag 归属一致可删）。反向验证：把版本临时改回固定 0.0.1 后该用例 FAIL 并指名固定 tag，恢复后 PASS（21.68s） | 1 |
| 2026-11-03 | 更正上表 2026-11-02 行「模块卡 §4.7 镜像 tag 项目前缀」的处置：当时以固定 tag 实施并登记偏离；修复发现②后改为**每次运行随机版本**的 tag | 归属保护（登记 ID 核对）取代前缀扫描承担清理正确性；tag 前缀在 `ValidateVersion` 的 `X.Y.Z` 约束下不可行，且前缀只解决“扫得到”，不解决“删得对” | 偏离性质变更：镜像类残留不再靠前缀扫描，而由（tag, LocalID）登记 + 归属核对承担；A15 容器 / 卷 / 网络判据不变，stage 的 `us18-image-build` 新增该回归于 `^TestUS18_Image_` 组内 | 2 |

## 3. 停止预算与计数

| 项 | 默认 | 已用 |
| --- | --- | --- |
| 同一问题修复次数 | 3 | 0 |
| 阶段总尝试 | 5 | 0 |
| 会话中断恢复 | 不限 | 0 |

## 4. 未验证清单

| # | 项 | 原因 | 恢复条件 |
| --- | --- | --- | --- |
| U-18-01 | 镜像仓库登录 / 推送 | 未授权 | 用户授权并提供仓库 |
| U-18-02 | linux/arm64 镜像 | 无 arm64 宿主 | 提供宿主或书面延期 |
| U-18-03 | 漏洞扫描 | 无扫描器与漏洞库 | 提供扫描器与授权 |
| U-18-04 | Docker Desktop / podman / rootless | 本机只有 WSL Docker Engine | 提供宿主 |
| U-18-05 | OCI 签名 / SLSA | 首发动作，本 US 不做 | 用户授权后 |
| U-18-06 | 镜像字节级复现 | 构建时间进入镜像配置 | 工具链支持后 |
| U-18-07 | 跨 schema 升级回退 | 只有一个版本 | 第二版本出现后 |
| U-18-08 | 外部 PostgreSQL | 只测随附 PG | 提供外部库环境 |
| U-18-09 | full 档镜像 | 运行期组件未备齐 | US-17 组件齐备后 |
| U-18-10 | 部署 CI 宿主的 Docker / Compose 版本 | **已验证（2026-11-02，run 41521764079 / job 125539295588）**：GitHub `ubuntu-latest` runner 为 Docker Engine 29.4.2（API 1.54）、containerd 2.4.2、runc 1.5.2、Docker Compose v5.1.3（`container` job 日志 step “Record runner Docker / Compose versions”） | 无（已登记） |
| U-18-11 | WSL `/mnt/d`（9p/drvfs）上的 secret 属主 / 权限 | 文件系统忽略 chown/chmod；脚本会拒绝 | 改用 ext4 或 Docker 卷 |

## 5. 收口结论

**状态：待人工 review ②**（AI 自动门禁、真实容器验收与部署 CI 按本节证据推进；绿态不代替用户验收，本节不自行写“已收口”。）

### 5.1 本机门禁与验收

| 项 | 命令 | 结果 | 证据 |
| --- | --- | --- | --- |
| 全量验收（11 项 profile） | `sh scripts/check-acceptance.sh`（WSL Ubuntu） | PASS，11/11，指纹 `d258b91d…` | `.loop/runs/us18-full/report.json`（2026-11-02T18:17Z） |
| 全量验收复跑（F1–F10 修复后） | `bash dev-env/scripts/with-postgres.sh sh scripts/check-acceptance.sh`（WSL Ubuntu） | PASS，11/11，指纹 `f1f1ecbb…` | `.loop/runs/us18-full-final/report.json`（2026-11-02T19:57Z；与 `us18-aggregate` 同指纹） |
| `make check` | `make check`（WSL Ubuntu） | PASS（退出码 0，覆盖率全绿） | `.loop/runs/us18-make-check.log`（2026-11-03，`MAKE_CHECK_EXIT=0`） |
| `go test -race` | 含于全量 profile 的 `test` 检查（`go test -race -count=1 -cover ./...`）与 `make check` | PASS | 同上 |
| us18-base（source：image-unit / docs） | `--stage-only --stage harness/stages/us18-base.json` | PASS，2/2 | `.loop/runs/us18-base/report.json`（18:47Z） |
| us18-windows（process：6 检查 / 15 用例） | `--stage-only --stage harness/stages/us18-windows.json` | PASS，6/6，15 用例，0 跳过 | `.loop/runs/us18-windows-final/report.json`（19:00Z，指纹 `c00236b3…`） |
| us18-aggregate（source＋process 同轮） | `--stage-only --stage harness/stages/us18-aggregate.json` | PASS，8/8 检查，34 用例，0 跳过，指纹 `f1f1ecbb…` | `.loop/runs/us18-aggregate/report.json`（2026-11-02T19:48Z；跑在 review 修复后的未提交树上，提交后的判卷见 PR CI） |
| us18-linux（CI `container` job） | `.github/workflows/ci.yml`（run 41521764079，job 125539295588） | PASS，6/6 检查，15 用例，0 跳过；按前缀清理并断言零残留 | `.loop/runs/us18-ci-41521764079-1/report.json`（CI artifact `container-evidence`，2026-11-02T20:51Z） |
| 全量验收（CI `test` job，提交后判卷） | `.github/workflows/ci.yml`（run 41521764079，job 125539295329） | PASS，11/11，指纹 `51430a7e…`；stage lint、current-tree、us15-linux、tzdata 同 job 通过 | `.loop/runs/ci-41521764079-1/report.json`（CI artifact `acceptance-evidence` ID 13179856735，2026-11-02T20:56Z） |
| 修复①定点回归（本地） | `go test -tags=e2e ./internal/e2e -run '^TestUS18_Compose_RestoreRefusesExistingProject$'`（WSL Ubuntu） | PASS（0.58s）；反向验证失败态已复现并回滚 | 本机实测 2026-11-03；提交后由 PR CI `container` job 的 `us18-backup-restore` 检查复判 |
| 修复②定点回归（本地） | `go test -tags=e2e ./internal/e2e -run '^TestUS18_Image_'`（WSL Ubuntu，含 `BuildAndRecord` + `PreExistingTagPreserved`） | PASS（A1 31.11s，回归 14.24s）；反向验证失败态已复现并回滚 | 本机实测 2026-11-03；提交后由 PR CI `container` job 的 `us18-image-build` 检查复判 |
| 修复后备份恢复组复跑（本地） | `go test -tags=e2e ./internal/e2e -run '^TestUS18_Compose_(BackupRestore\|UpgradeRollback\|RestoreRefusesExistingProject)$'` | PASS（62.29s / 72.92s / 0.55s） | 本机实测 2026-11-03 |
| 格式与静态检查（修复后） | `sh scripts/check-format.sh`；`go vet -tags=e2e ./internal/e2e` | 均干净 / 退出 0 | 本机实测 2026-11-03 |
| 走查实例更新（最终提交 `733a2f3`） | `releasekit build` + `image build`（版本 0.0.3）后 `docker compose -p basalt-us18-demo up -d --wait` | PASS：应用镜像 `sha256:e21a68e0…`（由最终提交重建）、PostgreSQL `sha256:7d0a41…`（仓库固定 digest）、健康 200；实例身份 `inst_8887959b107334c3` 与旧 admin token 仍有效（带 token 200、无 token 401），三个数据卷未变 | 本机实测 2026-11-03，`127.0.0.1:8080`；实例部署目录 `~/basalt-us18-demo` 的 compose 已与仓库同步 |
| us18-linux（CI `container` job，修复后） | `.github/workflows/ci.yml`（run 41543696424，job 125613870840；head `733a2f3`） | PASS，6/6 检查，17 用例（含两个新回归），0 跳过；按前缀清理并断言零残留 | `.loop/runs/us18-ci-41543696424-1/container-evidence/…/report.json`（CI artifact `container-evidence` ID 13189070073，指纹 `ae401cc6…`，2026-11-02T23:21Z） |
| 全量验收（CI `test` job，修复后） | `.github/workflows/ci.yml`（run 41543696424，job 125613870443；head `733a2f3`） | PASS，11/11；current stage 33 检查、us15-linux 同 job 通过；stage lint 只读预检通过 | `.loop/runs/ci-41543696424-1/acceptance-evidence/…/report.json`（CI artifact `acceptance-evidence` ID 13188992647，指纹 `ae401cc6…`，2026-11-02T23:41Z） |
| 最终 HEAD 全量 CI（`30393db`，含 stage 锚点强化） | run 41547602884（push；PR run 41547612855 同 SHA）：`container` job 125626904397、`test` job 125626904525 | 六 job 全绿；`container` 6/6 检查、17 用例、0 跳过、零残留；`test` 全量 profile 11/11、current stage 33 检查、us15-linux 通过 | `.loop/runs/ci-41547602884-1/…`（artifacts `container-evidence` ID 13190375837、`acceptance-evidence` ID 13190273516，指纹 `1970a9f0…`，2026-11-02T23:52Z / 2026-11-03T00:09Z） |
| 走查实例重建（HEAD `07b041d`，版本 0.0.4） | `releasekit build` + `image build`（版本 0.0.4）后 `docker compose -p basalt-us18-demo up -d --wait`（PostgreSQL 容器未重建） | PASS：应用镜像 `sha256:3dfd4285…`、二进制 `sha256:38332596…`（go1.27.3，CGO 关；`733a2f3..07b041d` 仅文档与 stage 配置差异，构建输入未变）、PostgreSQL `sha256:7d0a41…`、健康 200、`/console/` 200；实例身份 `inst_8887959b107334c3`、旧 admin token（43 字节；带 token 200、无 token 401）与三个数据卷未变；库 schema_version=7、60 表 | 本机实测 2026-11-03，`127.0.0.1:8080`（容器 `container_image=sha256:3dfd4285…`，状态 healthy） |

说明：`us18-full` 跑在 F-02 清理断言改动之前；修复后的内容由 `us18-full-final`（`f1f1ecbb`）与 `us18-aggregate`（同指纹）两轮全量覆盖——前者覆盖 11 项 profile，后者覆盖 8 项 source/process 检查（34 用例）。`us18-aggregate`、`us18-full-final` 与 `make check` 均跑在未提交的 review 修复树上，指纹绑定当时内容。提交后的判卷由 PR #2 CI（commit `2891ece`，run 41521764079）完成：`container` job 的 `us18-linux` 阶段 6/6 与 `test` job 的全量 profile 11/11 均绿；同 SHA 的 push run 41521755007 六个 job 亦全绿。CI 指纹为 `51430a7e…`，与本地 `f1f1ecbb…` 不同是因为 digest 修正（F11，四处锁文件消费点）发生在本地全量运行之后、提交之前——两者各自绑定实际被判卷的树。`container` job 首次实跑记录 runner 环境：Docker Engine 29.4.2（API 1.54）、containerd 2.4.2、runc 1.5.2、Docker Compose v5.1.3。
