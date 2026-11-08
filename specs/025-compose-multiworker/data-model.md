# US25 数据模型与接口形状

本 US **不新增数据库表、迁移或端点**（§3.4）。以下实体均为 Go 侧类型、部署工件或部署变量；表引用只描述既有表的只读查询。

## 1. 首次安装端口（`internal/cli`）

### FirstInstallReport

命令面只读报告，与 `app.instanceBootstrapReport` 分开，避免 `cli` 依赖 `app`。

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| `Fresh` | `bool` | 本次准备前是全新实例（库与名册都不存在） |
| `ConfigCreated` | `bool` | 本次写了配置文件 |
| `ProvisioningRecorded` | `bool` | 本次写了 `provisioning.pending` |
| `MarkerCreated` | `bool` | 本次写了名册 |
| `InstanceID` | `string` | 名册中的实例标识 |

映射：`Fresh ← report.Fresh`、`ConfigCreated ← report.ConfigCreated`、`ProvisioningRecorded ← report.ProvisioningRecorded`、`MarkerCreated ← report.MarkerCreated`、`InstanceID ← report.InstanceID`，逐字段一一对应，不改 `Fresh` 语义。

### FirstInstall 接口

```text
type FirstInstall interface {
    Prepare(workDir string) (FirstInstallReport, error)
}
```

消费方（`cli`）声明；`app` 用无状态 `firstInstallAdapter{}` 实现并调 `ensureNewInstanceConfig(workDir, programRuntimeRoot())`。`Deps.FirstInstall` 为必填，nil 在 `runInit` 是装配错误。

### 调用顺序（`internal/cli/init.go`）

```text
Workspace.Init(wsRoot)                        # 既有
    ↓
deps.FirstInstall.Prepare(deps.WorkDir)       # 新增：配置 → pending → 内置安装意图 → 名册
    ↓
Store.Bootstrap(ctx, dbPath, SourceSchemaVersion)   # 既有：建 SQLite 源库
```

`Prepare` 不取实例锁；第二次调用 `detectNewInstance=false`，零写入。并发首次启动由 `O_EXCL` 写文件与名册判定保证只有一个成功。

## 2. 主密钥资格（`internal/app` + `internal/storage/postgres`）

### SecretsFacts 扩展

| 新字段 | 类型 | 来源 | 语义 |
| --- | --- | --- | --- |
| `ProvisioningPending` | `bool` | `adminTokenProvisioningPending(workDir)` | 首次进度记录为 pending（名册可读、记录可读、`instance_id` 一致、状态 pending） |
| `RuntimeHistory` | `bool` | `Store.HasRuntimeHistory(ctx)` | 四张业务表任一非空 |
| `RuntimeHistoryErr` | `error` | 同上 | 查询失败；非 nil 时判定必须落 `await_init`，不得当成 `false` |

既有字段（`Configured` / `FreshInstance` / `AutoInit` / `Record` / `KeyPresent` / `KeyID` / `KeyErr` / `ManagedCiphertexts` / `CiphertextsVerified` / `CiphertextsErr`）与 D-40 语义不变。

### 判定表（`EvaluateSecrets`，只改 create 一支）

| 场景 | 记录 | 密钥文件 | 托管密文 | 历史 | auto_init | fresh / pending | 判定 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 全新实例 | 无 | 无 | 0 | 无 | 真 | fresh | `create` |
| 中断重试 | 无 | 无 | 0 | 无 | 真 | pending | `create` |
| 有业务历史 | 无 | 无 | 0 | 有 | 任意 | 任意 | `await_init` |
| 历史查询失败 | 无 | 无 | 0 | 错 | 任意 | 任意 | `await_init` + 错误原因 |
| 已就绪 | 有 | 有 | 0 | 任意 | 任意 | 任意 | `ready` |
| 恢复态 | 缺一 | 缺一 | >0 | 任意 | 任意 | 任意 | `recover` |
| 无记录、无密文、非 auto_init | 无 | 无 | 0 | 无 | 假 | 任意 | `await_init` |

### HasRuntimeHistory（`internal/storage/postgres/instance_secrets_store.go`）

```text
var runtimeHistoryTables = [...]string{"runs", "task_executions", "agent_lifecycle", "kb_documents"}

func (s *InstanceSecretsStore) HasRuntimeHistory(ctx context.Context) (bool, error)
```

- 单条只读查询（逐表 `EXISTS` 或 `UNION ALL … LIMIT 1`），不把整表读进内存；受调用方 `ctx` 超时约束。
- 任一表不存在或查询失败返回错误；调用方落 `await_init`。
- `management_operations` **不计入**；该判据只回答“本次能否自动建钥”，**不得**表述为“证明整个共享库从未使用”。
- 不新增 DDL；若现有索引不足，先用 `EXPLAIN` 证明再评估，US-25 不新增迁移。

## 3. Demo 准备（`internal/cli` 端口 + `internal/app` 服务）

### 请求与计划

| 类型 | 字段 | 说明 |
| --- | --- | --- |
| `DemoPrepareRequest` | `Server string`、`TokenFile string` | `Server` 空则从配置 `http.addr` 派生；通配监听改写为同族回环；配置装载失败且未给 `--server` 直接失败。`TokenFile` 空则配置 token → 实例私有 `admin-token`，只读不生成 |
| `DemoCollectionPlan` | `Name`、`CollectionID`、`Exists`、`Action` | `Action`：`create` / `reuse` |
| `DemoDocumentPlan` | `Collection`、`Path`、`Action` | `Action`：`ingest` / `unchanged`；固定 9 项（`bid-request` × 8、`bid-product` × 1） |
| `DemoBindingPlan` | `Agent`、`Knowledge []core.KnowledgeBinding`、`Action` | `Action`：`bind` / `unchanged` / `conflict`；两个 Agent |
| `DemoPreparePlan` | `Server`、`WorkflowDir`、`Collections`、`Documents`、`Bindings`、`Warnings` | `Warnings` 含 embedding 调用次数与费用提示 |

### 结果

| 类型 | 字段 | 说明 |
| --- | --- | --- |
| `DemoCollectionResult` | `Name`、`CollectionID`、`Created` | |
| `DemoDocumentResult` | `Collection`、`Path`、`Status`、`Deduplicated` | `Status` 取服务端 `active` / `failed` |
| `DemoBindingResult` | `Agent`、`Status` | `applied` / `unchanged` / `conflict` / `stale`（计划过期需重跑） |
| `DemoPrepareResult` | `Collections`、`Documents`、`Bindings`、`Partial` | `Partial=true` 表示至少一个 Agent 未绑定；退出码 3 |

### DemoPrepare 接口（`internal/cli`）

```text
type DemoPrepare interface {
    Plan(ctx context.Context, req DemoPrepareRequest) (DemoPreparePlan, error)
    Apply(ctx context.Context, req DemoPrepareRequest, plan DemoPreparePlan) (DemoPrepareResult, error)
}
```

`Plan` 零写入，只发 GET；`Apply` 才写。

### DemoPrepareService（`internal/app/demo_prepare.go`）

```text
type DemoPrepareDeps struct {
    WorkDir       string
    WorkspaceRoot string
    Config        *cli.Config
    ConfigErr     error
    HTTPClient    *http.Client
    Logger        *slog.Logger
    Now           func() time.Time
}
```

- 无数据库句柄、无运行期装配；只发管理面 HTTP。
- `WorkDir` / `WorkspaceRoot` 必填；`HTTPClient` nil 时建默认有界客户端。
- HTTP 纪律：`--server` 无凭据 / 查询 / 路径；明文 HTTP 只允许 `localhost` 或回环 IP；token 文件 ≤ 4096 字节；响应 ≤ 2 MiB；禁重定向；超时 65s（沿用 `flow_cli.go` 口径，不重构该文件）。
- 绑定写入：先 `GET /api/v1/agents/{name}/files` 取基础集合 ETag，再 `GET /api/v1/agents/{name}/content` 读正文，`ApplyChanges(orig, core.AgentContentChanges{Knowledge: &bindings})` 生成新正文，`POST /api/v1/agents/{name}/files` 只提交 `AGENT.md` 且 `If-Match` 必须是基础 ETag；412 → `stale`，不重提。
- 冲突判定：`bindingConflicts(content, want)` 解析正文 `knowledge:` 与目标不等价即 `conflict`，命令非零退出；写前重判。

## 4. 种子归档（部署工件，不是产品运行时契约）

### seed-manifest.json（`format_version = 1`）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `format_version` | `int` | 固定 `1` |
| `created_at` | RFC 3339 字符串 | 导出时刻 |
| `source_instance_id` | `string` | 源实例名册标识 |
| `files[]` | 数组 | 每项 `{path, size, sha256}`；`path` 为相对工作区根的正斜杠路径 |

### 归档与导入规则

- 归档 = `tar` + `seed-manifest.json`；只支持人工传输。
- 排除项**不得**出现在归档内：配置（`basalt.yaml`）、SQLite 库、锁文件、日志、`runs/`、Agent 状态（`state/*.json`）。
- 导入前逐项校验存在性、`size` 与 `sha256`；拒绝篡改（任一字节差异）、截断（清单外或长度不足）、越界路径（`..` / 绝对路径 / 反斜杠绕行）、符号链接；拒绝时目标工作区零落盘。
- 刷新方式：源端停写导出 → 目标 Worker 停机导入 → 重启；不做热同步（Q-25-11）。文件型 Agent 状态每 Worker 独立、非一致（Q-25-12）。

## 5. 部署工件与变量

### 部署变量（Compose / entrypoint 变量，**不是**产品配置键）

| 名称 | 使用处 | 取值与约束 |
| --- | --- | --- |
| `BASALT_ROLE` | entrypoint / Compose | `control`（缺省）或 `worker` |
| `BASALT_CONFIG_TEMPLATE` | entrypoint | 控制面 `/opt/basalt/basalt.yaml`；Worker `/opt/basalt/basalt.worker.yaml` |
| `BASALT_MASTER_KEY_FILE` | Worker 配置 / entrypoint | 容器内 `/run/basalt-secrets/master_key`（`0700` 目录只读 bind mount） |
| `BASALT_WORKER_NAME` | Worker 配置 | 诊断名；允许重复；身份仍是 `InstanceID` |
| `BASALT_DB_BIND` | Compose | 控制面 PG 绑定地址；单宿主 `127.0.0.1`，双宿主私网地址；**不得为空**、**不得** `0.0.0.0`、**不得** IPv6 全地址 `[::]`（与 `0.0.0.0` 等价）——三者都由 `basalt-preflight` 在启动前**拒绝**；端口行的 `:-127.0.0.1` 只是第二道防线，不表示空值可用 |

### compose.multiworker.yaml 形状

| 服务 | profile | 关键约束 |
| --- | --- | --- |
| `basalt-preflight` | `control` | 一次性预检（同一应用镜像、同安全不变量、`read_only`、无 tmpfs）：拒绝空值 / `0.0.0.0` 的 `BASALT_DB_BIND` 与缺失的六项入口限流；`postgres` 与 `basalt-control` 以 `depends_on: service_completed_successfully` 挂在它后面 |
| `postgres` | `control` | 单宿主不发布或只绑回环（本实现选回环）；双宿主只绑 `${BASALT_DB_BIND}` |
| `basalt-control` | `control` | 跑 `serve`；发布 8080；挂 `database_dsn` + `postgres_password` secret；健康检查 `/api/v1/health` |
| `basalt-worker` | `worker` | 跑 `gateway`；无端口；只挂 `database_dsn` secret 与 `master_key` 只读 bind mount；健康检查自行解析 DSN 后跑 `basalt workers list`；无自动重启 |

卷：`postgres-data`、`control-workspace`、`control-private`、`worker-workspace`、`worker-private`。两角色均满足：`user 10001:10001`、`read_only`、`cap_drop: [ALL]`、`no-new-privileges`、`init: true`、tmpfs、无 Docker socket、镜像固定摘要。

### basalt.worker.yaml

```text
database.dsn: ${BASALT_DATABASE_DSN}
secrets.master_key_file: ${BASALT_MASTER_KEY_FILE}
secrets.auto_init: false
worker.name: ${BASALT_WORKER_NAME}
+ scheduler / assets / server / budget / flows / embedding / providers 示例段
+ 无 http / callers（控制面专属）
```

必须可独立解析：控制面专属 secret 或变量缺省时仍通过加载（FR-25-33）。

### ImageInputs / ImageRecord 扩展（releasekit）

| 类型 | 新字段 | 说明 |
| --- | --- | --- |
| `ImageInputs` | `WorkerConfigFile`、`ExportKeyScript`、`SeedScript` | 必填路径；`validate()` 的 `os.Stat` 清单加三项，缺件在 `docker build` 之前报错 |
| `ImageRecord` | 三个对应 SHA-256 摘要字段（`omitempty`） | 新构建非空；US-18 既有记录比对不受影响 |

## 6. 迁移核对报告（M1 / M2，人工 + e2e 证据）

| 项 | 来源 | 规则 |
| --- | --- | --- |
| 运行 / 审计 / 资源引用 / 技能版本计数 | 源库与目标库同口径查询 | 有对应对象时计数一致并抽样内容 |
| 源无对象登记 | 迁移报告 | `flow_*` / 学习审批 / `kb_*` 按“源无对象、目标新建空表”逐项登记 |
| 知识引用 | 报告 | 源库无 `kb_*` 表时计数为 0 **且写明原因**；文档不得出现“知识已迁移” |
| M2 完整性 | 目标库 | 生命周期索引与 Agent 身份保留、准入通过、运行 / Flow / 审计 / 知识计数一致、旧 bundle 可读；非空 Flow 与学习审批在此判定 |

## 7. 状态流转

- **首次安装**：`缺失 → 配置写出 → pending 写出 → 内置安装意图 → 名册写出 → 完成`；中断于任一步，下次 `serve` 依据“名册 + pending”续做；名册半截 / 不可解析 → 失败关闭，等人工处理。
- **主密钥**：`无 → create（预写 pending 审计）→ 就绪`；`无 + 有业务历史 → await_init`；`密文在而记录或密钥缺 → recover`；三态互斥，不得从后两态自动落到 create。
- **Demo 绑定**：`unchanged` / `conflict`（拒绝）/ `bind` → `applied`；提交 412 → `stale`（停止该 Agent，需重跑）；任一 Agent 失败 → 整体 `Partial=true`、退出码 3。
- **Worker**：`启动预检失败 → 退出 78`；`预检通过 → gateway 运行（注册 → 心跳 → 领取 / 回收）`；`停机信号 → 停止领取 → 续约至执行退出 → 交还执行权 → 关库`。
- **种子**：`导出（源停写）→ 清单 + tar → 导入端校验 → 落盘或拒绝（零落盘）`；无热同步路径。
