# US25 验证指南（Quickstart）

本文件只做验证步骤与预期结果；接口形状见 [data-model.md](data-model.md)，FR 与验收映射见模块卡 §4.4，权威契约见 `docs/contracts/`（本 US 不改）。

## 0. 前置

- Go 1.27.3（`go.mod` 的 `toolchain` 决定），`GOPROXY=https://goproxy.cn`。
- 本机 PostgreSQL 夹具（`BASALT_TEST_PG_DSN` 指向 127.0.0.1:55432）。
- `us25-base` / `us25-docs` 可在 Windows 原生跑；`us25-compose` / `us25-fault` 需要 Docker 或 WSL2 的 Linux 容器，缺环境按 `t.Skip` 登记未验证。
- 不注入真实模型凭据、不连外网；Demo 准备对本地协议桩运行。

## 1. 首次安装（FR-25-01～06）

```sh
# 全新临时目录
tmp=$(mktemp -d)
cd "$tmp"
basalt init
# 预期：basalt.yaml / .basalt/instance.json / .basalt/basalt.db 同时存在；
#       init 输出显示首次准备先于源库创建（FirstInstallReport 的 ConfigCreated / MarkerCreated 为真）
basalt init          # 第二次
# 预期：零写入（第二次 Prepare 返回 Fresh=false 且各 Created=false），源库零变化
```

```sh
# 模拟首次 serve 缺入口限流退出后重试
BASALT_INGRESS_ENABLED=true basalt serve   # 缺 BASALT_INGRESS_* 配置 → 失败退出，保留 pending + 名册
basalt serve                               # 补齐配置后重试
# 预期：生成主密钥、打印一次管理面密码、完成开箱 Agent 安装；不出现“已有实例但无密钥”
```

自动化对应：`TestCLI_InitPreparesFirstInstallBeforeBootstrap`、`TestCLI_InitNilFirstInstallFailsClosed`、`TestFirstInstall_PrepareOrderAndIdempotency`、`TestFirstInstall_ConcurrentStartOneWinner`、`TestUS25_FirstInstall_HalfWrittenRosterFailsClosed`、`TestFirstInstall_SecretsEligibility`、`TestRuntimeHistory_*`。

## 2. Demo 准备（FR-25-07～11）

```sh
basalt demo workflow prepare --server http://127.0.0.1:8080
# 预期：只读；列出 9 个文档、2 个 Agent 绑定与 embedding 费用提示；零写请求

basalt demo workflow prepare --server http://127.0.0.1:8080 --yes
# 预期：两个集合存在、9 个文档 active、两个 Agent 知识绑定一致；逐项输出

basalt demo workflow prepare --server http://127.0.0.1:8080 --yes
# 预期：幂等——集合复用、文档去重、绑定不变、无新增模型调用

basalt demo workflow prepare --server http://10.0.0.5:8080
# 预期：拒绝（非回环明文 HTTP）
```

部分成功：让一个 Agent 的绑定冲突、另一个可写，运行 `--yes` → 逐项结果 + `Partial=true` + 退出码 3；修复后重跑只补失败项。自动化对应：`TestDemoPrepare_*` 8 条与 `TestCLI_DemoWorkflowPrepare*`。

## 3. 双角色 Compose（FR-25-12～17）

```sh
cd docker
cp .env.multiworker.example .env

# 首次准备控制面（示例顺序）
# 1) 初始化控制面工作区与密钥
docker compose -f compose.multiworker.yaml --profile control up -d postgres basalt-control
# 预期：控制面健康检查 /api/v1/health 通过；PG 端口不发布或只绑 127.0.0.1

# 2) 导出主密钥到宿主私有目录（0700、10001:10001）
sh export-master-key.sh basalt docker/secrets/master.key

# 3) 导出种子工作区
sh seed-workspace.sh export <control-workspace> seed.tar
sh seed-workspace.sh verify seed.tar
# 预期：清单逐项校验通过；篡改一个字节 → 拒绝

# 4) Worker 启动
docker compose -f compose.multiworker.yaml --profile worker up -d basalt-worker
# 预期：Worker 无端口；日志显示预检通过并进入 gateway
```

拒绝场景：缺种子 / 缺主密钥 / 密钥不可读 / 文件或父目录权限过宽 → 退出 78，不写业务表。自动化对应：`TestUS25_Compose_*`、`TestUS25_Seed_ManifestAndTamper`、`TestUS25_Workers_BothRegistered`。

## 4. 执行与故障边界（FR-25-18～23）

- 两个 Worker 同时在线：同一 `(task_id, planned_at)` 只一条执行记录；`basalt workers list` 两行。
- 停控制面：存活 Worker 回收器在有限截止内收尾旧执行（置终态 + 释放租约），旧计划时刻不产生第二个 Run、调用与工具计数不变；旧持有者回写被 fencing 拒绝；后续新的计划时刻由存活 Worker 正常领取。
- 数据库短暂中断恢复：未确认副作用标停止 / 待查，不自动重放；Run 终态与审计一致。
- 资产 bundle 缺失或哈希不符：新 Run 准入被拒。
- 停机：先停领取、续约至执行退出、审计终态与钩子结算完成、再释放执行权、最后关库。

自动化对应：`TestUS25_Schedule_ExecutesOnceAcrossWorkers`、`TestUS25_Takeover_FencesOldHolder`、`TestUS25_DatabaseOutage_NoReplay`、`TestUS25_AssetMissing_RejectsNewRun`、`TestUS25_Shutdown_ReleasesBeforeClose`（容器宿主）。

## 5. 迁移与升级（FR-25-24～27）

```sh
# M1：SQLite v6 → PostgreSQL v12 九步
basalt db migrate --from sqlite --to postgres ...
basalt db verify --handoff ...
# 预期：交接校验通过；运行 / 审计 / 资源引用 / 技能版本计数与抽样一致；
#       源无 flow_* / 学习审批 / kb_* 对象逐项登记；知识引用 0 且写明原因

# M2：单实例 → 多 Worker
# 预期：生命周期索引与 Agent 身份保留、准入通过、计数一致、旧 bundle 可读；
#       非空 Flow 与学习审批在此判定
```

两节点升级：停全部旧进程 → 备份 PG + 每个工作区 + 密钥 + 旧二进制 → 新控制面迁移 → 新 Worker。跨 schema 用旧制品验证拒绝；同 schema 不宣称版本门禁，只验停旧进程、制品一致、整组回滚。自动化对应：`TestUS25_Migration_*`、`TestUS25_Upgrade_TwoNodeAbortAndRollback`。

## 6. 文档口径（FR-25-28～31）

```sh
go test -json -count=1 -timeout=5m ./internal/cli -run ^TestDocs_
# 预期：四个文档面九个首配锚点顺序一致；隔离警告五要素齐全；多 Worker 五要素齐全；
#       支持矩阵只含 Compose 多 Worker 与 LocalRunner
```

## 7. 完整门禁

```sh
# Windows
.\scripts\check-acceptance.ps1 --stage us25-base
.\scripts\check-acceptance.ps1 --stage us25-docs
.\scripts\check-acceptance.ps1 --stage us25-compose   # 需 Linux 容器
.\scripts\check-acceptance.ps1 --stage us25-fault     # 需 Linux 容器
# 完成的定义：make check 七目标全绿 + 四个 stage 全绿（缺环境项登记未验证）
```

## 8. 不在本指南范围

真实双宿主（A+B）、真实模型、真实 webhook、K8s、数据库 HA、容器隔离（OCI/gVisor）验证、发布 tag / 签名 / Release 上传——全部标未验证，见 [run-state.md](run-state.md) §4。
