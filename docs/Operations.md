# Basalt 运维手册：依赖条件、监听与出网、时区、备份、升级与停机、凭证

> 本文档是 Basalt 的**运维篇**：装、看、备份、升级、停机、凭证六件事的操作口径与判据。契约四件（Run 与错误 / AGENT.md 与工具 / 完整 DDL / 静态 OpenAPI）见 [contracts/](contracts/)，项目入口见 [README](../README.md)。设计正文（兼容性与迁移、Why / What / How）由维护者内部维护，不随公开仓库发布，本文不重复。
>
> 本文与维护者的技术方案冲突时**以技术方案为准**（技术方案不随公开仓库发布）。本文不重复设计理由，只写"运维照着做"的动作、判据与代价。

---

## 1. 依赖条件

### 1.1 底座运行时：单二进制 + PostgreSQL

- `CGO_ENABLED=0` 静态二进制（linux/amd64、linux/arm64、windows、darwin 交叉编译），`scp` 一个文件 + `systemctl start` 即部署；目标机器**不需要** JVM / Node / Python / 包管理器（完整发布包另附固定版本 Node / Python 供选配 Agent 使用，见 §1.4——同样不需要用户自行安装）
- **运行期存储是外部 PostgreSQL（含 pgvector 扩展），这是部署前置，不是可选项**：会话 / 消息 / 运行审计 / 记忆 / 知识 / 调用方与配额全部在 PostgreSQL 里；连接失败、扩展缺失或 schema 版本不符时**拒绝启动**，**不**回退 SQLite
- 工作区文件面仍在**当前工作目录的 `.basalt/`**（Agent 目录 + 技能库 + 技能版本快照 + 控制状态 + markdown 记忆档 + 日志）；这两块（PostgreSQL + 工作区）是**两个**备份对象，缺一不可

判据：目标机器上 `file ./basalt` 显示 `statically linked`（linux）；在一台**只有** PostgreSQL 的机器上（不装其他服务），配好 DSN 后 `basalt status` 能跑出结果；故意不给 DSN 时 `basalt serve` 以点名 `database.dsn` 的错误拒绝启动，且**不**尝试打开 SQLite。

### 1.2 PostgreSQL 准备与 DSN 提供

**版本与扩展**（2026-10-30 部署侧实测口径）：PostgreSQL **16 或 17**（实测组合：PostgreSQL 17.11 + pgvector 0.8.7 + pg_trgm 1.6，HNSW 索引与迭代扫描可用）。**两个扩展都是启动前置**：`vector`（≥ 0.8，且必须能探测到 `hnsw.iterative_scan` 能力）与 `pg_trgm`（关键词检索的三元组索引）。扩展由**部署者**安装（发行版包 `postgresql-17-pgvector` / 官方 `pgvector/pgvector:0.8.7-pg17-bookworm` 镜像均已带），启动时缺扩展或能力不足会以点名的错误拒绝启动——**不**会在运行中静默降级，应用也**不会**自动 `CREATE EXTENSION`。

**建角色与建库**（最小权限；示例值按环境替换，密码不进文档也不要写进仓库）：

```sql
CREATE ROLE basalt LOGIN PASSWORD '<password>';
CREATE DATABASE basalt OWNER basalt;
\c basalt
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```

**DSN 的两种提供方式**——`database.dsn` 是**无条件必填**的新配置键（运行期唯一后端就是 PostgreSQL）：

| 方式 | 写法 | 适用 |
| --- | --- | --- |
| 环境变量（推荐） | `basalt.yaml` 里写 `database:\n  dsn: ${BASALT_DATABASE_DSN}`，部署环境导出该变量 | 密码不落配置文件；与 `basalt.yaml` 一起备份时也**不**带密钥材料 |
| 字面量 | `basalt.yaml` 里直接写完整连接串 | 仅限本地 / 演示；此时 `basalt.yaml` 含密码，**不得**进版本库，备份按凭证对待 |

连接串格式：URL（`postgres://user:pass@host:5432/basalt?sslmode=disable`）或 `key=value` 串（`host=… port=… user=… password=… dbname=… sslmode=…`）均可。

> **模板里的占位符不是"已经配好"**：新实例首次启动生成的 `basalt.yaml` 带的是 `dsn: ${BASALT_DATABASE_DSN}` 这一行**占位**——它只保证配置键存在，**不**代表数据库已经准备好。占位未解析（环境变量未导出）与解析为空**同样按空值处理**，启动以「PostgreSQL 连接串为空——database.dsn 必须解析为实际值」拒绝；**不得**把"模板有占位符"理解为"用户无需准备数据库"。

**首启前的可选预迁移**：`basalt db migrate` 按配置里的 DSN 把库带到当前 schema 版本（幂等）。`serve` 首次启动也会自己做迁移；数据库账号**没有 DDL 权限**时，预迁移是唯一路径——由具备建表权限的账号先跑一次 `db migrate`，再以受限账号 `serve`。

**首次启动的四个阶段**（中断后各自的可观察信号，排障按此逐段核对）：

| 阶段 | 做什么 | 成功信号 | 失败时的状态 |
| --- | --- | --- | --- |
| ① 配置生成 | 无 `basalt.yaml` / 实例名册时写最小配置模板、初始化进度记录（`provisioning.json`，`pending`）与实例名册，次序固定：**配置 → 记录 → 名册** | 日志 `新实例：最小配置与实例名册已就绪`，字段 `config_created=true provisioning_recorded=true marker_created=true` | 进度记录或名册写不出来 → 启动失败且**不**写名册（工作区仍算新实例；已生成的配置不重写）；修好后重跑走完整流程 |
| ② 数据库连接 | 解析 DSN → 连接 → 校验扩展 → 迁移 → 校验 schema 版本 | 无告警，继续往下 | 失败以 `打开 PostgreSQL: …` 包装点名原因（连接串为空 / 连接失败 / 库版本高于二进制）；**已写入的配置与名册保留**，不会改写 |
| ③ 密码就绪 | `http.admin_auth.mode: token` 且未显式配置 token 时，读实例私有目录的 `admin-token`；本次属于"首次初始化还没做完"时生成一次 | 非交互终端打印 `管理台访问密码已写入 <路径>`（**不**显示值）；交互式终端显示一次值 | 生成失败即启动失败；文件写成功但进度记录改写失败只告警（密码已可用） |
| ④ 监听 | 起 HTTP 监听、装配管理台 | 监听日志 / `curl -fsS http://<listen>/healthz`（或管理台可登录） | 端口占用等在此阶段失败；②③ 已完成的文件保留，重跑不会重复发放 |

**中断后重试的口径**（2026-10-30 用户裁决）：②③④ 任一阶段失败后，修好 DSN / 数据库 / 端口，重新 `basalt serve` 即可**续做**——启动日志会带 `首次初始化未完成：配置与实例名册已在盘上，管理面访问密码尚未生成——本次启动继续完成`，密码发放阶段还会带 `首次初始化未完成：本次启动继续生成管理面访问密码（不覆盖既有配置、主密钥或密码文件）`。四条边界：

- 续做**只**发生在"初始化进度记录仍是 `pending`"的实例上；记录缺失、状态已是 `minted`、标识不符或读不懂，一律按"已有凭据的实例"失败关闭——不补发、不覆盖；
- 续做**不**改写已生成的 `basalt.yaml`、实例名册与 `master.key`；已有 `admin-token` 文件时以盘上为准，**不**生成第二把；
- 进度记录为 `minted` 之后，运维**手工删掉** `admin-token` 不会被静默补发（管理面密码的更换必须是显式动作）；确需重置密码时按 §6 的凭证操作执行，**不得**靠删文件或删库"重来"；
- 恢复态**不得**通过重新生成主密钥或重建实例来绕过——那会让既有托管凭据全部不可读。

### 1.3 例外：某些 Agent 自己选用的解释器

下表依赖**不属于底座**，而是"某个 Agent 的选择"——只在那个 Agent 声明的能力被调用时才需要，缺了只影响它自己：

| 依赖 | 谁需要 | 缺了会怎样 |
| --- | --- | --- |
| Python 3 | 用 `shell` 工具跑 `scripts/*.py` 的 Agent（五个每日 Demo 与部分模板的脚本） | 该工具调用失败（`runtime_error`）。⚠ 走 `shell` 的解释器**必须**先登记进 `shell.interpreters`（受控脚本通道：只接受 `[<脚本路径>, <参数…>]` 形态、脚本须落在该 Agent `scripts/` 或技能库内）——**不得**为图省事放宽解释器白名单，那等于引入任意代码执行（见 [contracts/agent-md.md](contracts/agent-md.md) §5.2） |
| Node.js / `npx` | 用 `npx` 通道拉起的社区 MCP server（首批配方中的 `brave-search` / `fetch` / `playwright` / `dbhub` / `filesystem-readonly`） | MCP server 连不上 → 它的工具不注册 → **声明该工具的 Agent 整只不注册**（WARN 点名） |
| Go 工具链 | **构建**仓库与共享只读 MCP server `examples/shared/mcp/docs-readonly`（主 module 内 `package main`；安装器导出源码并生成 `go.mod` 后可在工作区独立构建）；运行发布二进制与已构建的 server 不需要 | 仓库无法编译；`docs-readonly` 构建失败 → D4 与 `onboarding-buddy` 模板的 server 不可用（依赖它的 Agent 整只不注册） |
| 宿主机 `sqlite3` CLI | **仅在线备份**需要（§4.2），不参与运行 | 退化为离线备份（停机拷贝，一样可靠，只是要停机窗口） |

**不得**把"某 Agent 需要 Python"写成"Basalt 需要 Python"——部署说明与验收口径以本表为准。

### 1.4 完整发布包：随包固定版本组件

完整发布包 = 产品二进制 + `runtime/`（固定版本的解释器与 MCP 组件）+ `LICENSES/`。底座是**单 Go 二进制**，但运行期仍需 PostgreSQL（§1.1 的部署前置）；包里附带的是上表"某些 Agent 选用"的解释器副本，装到目标机器后**不需要**再装 Node / Python，也**不需要** `npm` / `pip`：

| 组件 | 包内路径（Windows 例） | 版本来源 |
| --- | --- | --- |
| Node.js | `runtime/node/` | 版本与来源固定在 `release/runtime-manifest.json` |
| Python | `runtime/python/` | 同上 |
| DBHub | `runtime/mcp/dbhub/` | `@bytebase/dbhub`，随包 `node_modules` |
| GitHub MCP Server | `runtime/mcp/github-mcp-server/` | 官方 release 二进制 |
| Fetch | `runtime/mcp/fetch/` | `mcp-server-fetch` 固定版本 |
| docs-readonly | `runtime/mcp/docs-readonly/` | 本仓自研只读 MCP |

要点：

- **构建期出包，运行期不下载**：`go run ./scripts/release-package` 按清单逐组件核对版本，版本对不上就拒绝出包；产品进程不会在运行时下载或安装组件。
- **组件存在 / 可用 / 执行授权是三件事**：文件在 `runtime/` 里只证明"存在"；"可用"看 MCP 是否真实连接；"执行授权"看 stdio 启动命令是否登记进 `shell.allowed_commands`（新实例初始化自动写入免凭据条目，存量实例由启动基线补齐已就绪项）。管理台「执行环境 → 运行环境」分列显示并提供「查看依赖」入口。
- **缺件不静默补齐**：组件缺失时引用它的 MCP server 连不上，声明该 server 工具的 Agent 整只不注册（WARN 点名）；需要解压完整包或按清单补组件后重启。
- **许可声明随包**：第三方文本在 `LICENSES/`（含 `THIRD-PARTY-NOTICES.md` 汇总）；产品许可见包根 `LICENSE`。

首启验收（Windows 与 Linux 同一组动作）：解压 → `basalt serve` → 用访问密码登录 → 「执行环境 → 运行环境」核对组件存在 / 可用 / 执行授权 → 对已配置 MCP server 点「测试」看到 `stage=complete` 与工具清单。缺少用户个人凭据（如 `GITHUB_PERSONAL_ACCESS_TOKEN`）只显示待配置，不算包缺陷。

---

## 2. 监听与出网

### 2.1 监听地址与管理认证

| 项 | 值 |
| --- | --- |
| 配置键 | `http.addr`（`basalt.yaml`） |
| 默认 | `127.0.0.1:8080`（只对本机开放） |
| 管理认证 | `http.admin_auth.mode` **必填**：`token` / `local_trust`。未显式配置 → 拒绝启动。`token` 模式下 `http.admin_auth.token` 可省略——省略即读实例私有目录的 `admin-token` 文件（见 §2.1.1） |

**必须**：远程访问（多机联调 / 容器对外暴露）显式改 `http.addr`（容器内 `0.0.0.0:8080`），并配置 `http.admin_auth.mode=token`。访问密码的配置 / 环境变量 / 密码文件**三处都没有 → 拒绝启动**并点名三处，**不得**回落到 `local_trust`（那等于把管理面在无凭据状态下开放）。写了 `http.admin_auth.token` 键就必须解析出非空值——占位变量缺失、解析为空或残留 `${...}` 都在装载期拒绝，**不**悄悄改读密码文件。反向代理 / 隧道**不得**因上游连接是回环而绕过认证；`local_trust` 只在所有监听地址都是回环时允许，且只看**直接对端**，不看 `X-Forwarded-For` / `X-Real-IP`。

症状与代价：改了 `0.0.0.0` 却不配 token、不做 TLS 与网络隔离 = 把"可调 Agent（含 `shell` 工具）"连同 bearer token 一起暴露在明文链路上。admin token 只是核心阶段的最低门槛，**没有**企业 SSO / 多租户 / RBAC（扩展阶段）；远程管理**必须**经 HTTPS 或明确的可信加密通道，TLS 终止与证书轮换由部署方负责。公开报道中已有大量 agent 管理端口暴露在公网导致会话与凭证泄漏的案例，不要把"能连上"当成"可以这样部署"。

复核（可复制）：`ss -tlnp | grep 8080`——默认部署只应看到 `127.0.0.1:8080`；非回环部署在配置项、环境变量与 `admin-token` **三处都缺失**时必须观察到启动失败（fail-closed），而不是"起来了但没人能进"。新建实例首次 `serve` 会自动生成密码文件，要验这条失败路径，请在**副本工作区**里验证，不要先删生产实例的密码文件。

#### 2.1.1 生成 token 与注入环境变量

管理 token 是**最小管理认证**：只证明"持有管理凭据"，不是个人账户，也不提供企业级身份治理（无 SSO / RBAC，见 §6 红线段）。审计能把操作归因到这份凭据，但多人共用同一 token 时**无法区分到具体的人**——需要按人追责时按角色分开发放，不要共用。生成一次、妥善保存；轮换 = 换值后重启进程。

```bash
# 生成 32 字节随机 token（64 位十六进制）；不要用人类可猜的短语
openssl rand -hex 32
# 无 openssl 的环境等价写法
head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
```

`basalt.yaml` 里**只写占位符**，值只放在进程环境里：

```yaml
http:
  addr: 127.0.0.1:8080
  admin_auth:
    mode: token
    token: ${BASALT_ADMIN_TOKEN}
```

**第三来源：实例私有目录的 `admin-token` 文件**（与主密钥同目录、**分文件**——恢复语义不同：主密钥丢了要恢复或重录凭据，密码丢了重新生成即可）。`token` 键整体省略时，`serve` 读这个文件：内容按一行密码处理（去掉尾部空白，空文件不算有效来源）。新建实例首次 `serve` 自动生成 32 字节随机密码（base64url）并写入该文件（Linux `0600`）；**存量实例不自动生成**。提示纪律：非交互终端只打印文件路径，交互式只在生成那一刻显示一次值，密码本身**不得**进日志或错误文案。轮换 = 替换文件内容（或改配置里的占位环境变量）后重启进程。

注入方式三选一（选与部署形态同生命周期的那种）：

| 方式 | 做法 | 注意 |
| --- | --- | --- |
| systemd | `EnvironmentFile=/etc/basalt/basalt.env`，文件内一行 `BASALT_ADMIN_TOKEN=<值>`，`chmod 600` | unit 文件里**不得**直接写 token 值（unit 常被复制 / 进版本库） |
| 容器 | `docker run --env-file /etc/basalt/basalt.env …` | 同主机有权限的进程能读到环境变量；隔离靠宿主机权限 |
| 交互 shell（仅本机调试） | `export BASALT_ADMIN_TOKEN=<值>` | 换终端即丢失；生产不要用 |

启动时校验（fail-closed，违反即**拒绝启动**并点名配置项）：写了 `token` 键却占位符未配置、解析为空、仍含 `${...}`，或在 `basalt.yaml` 里直接写了明文 token；`token` 键省略时改按上文密码文件，**三处都没有**才拒绝启动。token 明文**不得**进入响应 / 日志 / 错误文案 / 管理审计；审计只记 `token:<sha256 前 16 位十六进制>`（§6）。

#### 2.1.2 三种部署形态

##### 形态 A：仅本机回环（默认）

```yaml
http:
  addr: 127.0.0.1:8080
  admin_auth:
    mode: local_trust   # 所有监听地址都是回环时才允许
```

浏览器直接开 `http://127.0.0.1:8080/console/`；脚本带 `X-Basalt-Local-Trust: 1` 直连回环。凡携带任何转发头（`Forwarded` / `X-Forwarded-For` / `X-Real-IP` / `X-Forwarded-Host` / `X-Forwarded-Proto`）→ 403——同机反向代理不能替公网请求"代签"本机信任。

##### 形态 B：反向代理终止 TLS（推荐的生产形态）

Basalt 仍只监听回环，由 nginx / Caddy 对外提供 HTTPS，认证**照常开启**：

```yaml
http:
  addr: 127.0.0.1:8080
  admin_auth:
    mode: token
    token: ${BASALT_ADMIN_TOKEN}
    allowed_hosts:
      - basalt.example.com          # 写请求 Host 头里的值（HTTPS 默认端口在 Host 头中省略）
    allowed_origins:
      - https://basalt.example.com
    trusted_proxies:
      - 127.0.0.1/32                # 代理所在地址；跨机部署时换成代理的真实网段
```

要点：

- 代理**必须**保留原始 Host（nginx：`proxy_set_header Host $host;`），否则 Host 白名单不命中 → 403
- `mode` **不得**降为 `local_trust`：代理会把公网请求变成回环连接，`local_trust` 只看直接对端且**拒绝任何转发头**——它在这里既不放行也不该被使用
- `trusted_proxies` 只影响失败限速的**来源归因**（取最右侧非可信地址），不参与认证放行
- 代理需同时设置 `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`，429 才按真实客户端计数；不设置时所有失败都记在代理地址上，多个客户端会互相连坐

##### 形态 C：直接非回环监听（仅限网络已隔离的内网）

```yaml
http:
  addr: 0.0.0.0:8080
  admin_auth:
    mode: token
    token: ${BASALT_ADMIN_TOKEN}
    allowed_hosts:
      - 192.0.2.10:8080             # 非回环地址必须显式列出；缺省派生只覆盖回环
      - basalt.internal:8080
    allowed_origins:
      - http://basalt.internal:8080
```

代价：明文 HTTP 上的 bearer token 可被同网段抓包。**必须**放在隔离网段或加密隧道内，或改走形态 B。`allowed_hosts` 未显式列出非回环地址 → 拒绝启动（fail-closed）。

#### 2.1.3 认证失败排障：401 / 403 / 429

失败响应只给固定分类，不解释命中哪条规则、不回显 token（契约见 [contracts/run-and-errors.md](contracts/run-and-errors.md) §9.2）。对照表：

| 读数 | 错误码 | 常见成因 | 处置 |
| --- | --- | --- | --- |
| 401 | `UNAUTHORIZED` | `token` 模式：缺 `Authorization` 头、方案不是 `Bearer`、token 错 | 核对注入的环境变量与请求头；401 不区分"没提供"与"提供错" |
| 403 | `FORBIDDEN` | Host 不在 `allowed_hosts`（端口不一致 / 代理没转发原始 Host）；Origin 不匹配；`local_trust` 下：非回环直连 / 带任一转发头 / 缺 `X-Basalt-Local-Trust: 1` | 按 §2.1.2 对应形态核对配置；反向代理下先查 `Host` 与 `X-Forwarded-*` 设置 |
| 429 | `AUTH_RATE_LIMITED` | 同一来源 60 秒内认证失败第 10 次起（含第 10 次），在凭据校验**之前**直接拒绝 | 按 `Retry-After`（秒，向上取整）等待后重试；计数在进程内、重启清零；成功认证**不**重置窗口 |
| 429 | `TOO_MANY_RUNS` | 与认证无关：实例并发运行数超过 `server.max_concurrent_runs` | 等运行结束或调高上限（运行准入不是认证限流） |

**429 的两个来源不要混淆**：`AUTH_RATE_LIMITED` 是管理面暴力尝试保护（按**来源**计认证失败次数）；`TOO_MANY_RUNS` 是运行准入（按**实例**计并发运行数）。两者都是 429，但错误码不同、处置方向相反。

限速口径：只统计 token 401 与 `local_trust` 的 403；Host / Origin 类 403 发生在认证之前、**不**消耗配额（否则攻击者可用错误 Host 把正常来源打进 429）。

排障顺序（可复制）：

```bash
# 1. 公开端点：这里就不通 = 进程没起 / 地址不对，不是认证问题
curl -i http://127.0.0.1:8080/api/v1/health
# 2. 带凭据：仍 401 = 进程里的 token 与本次请求不一致（改了环境变量没重启）
curl -i -H "Authorization: Bearer ${BASALT_ADMIN_TOKEN}" http://127.0.0.1:8080/api/v1/profiles
# 3. 仍 403：-v 看实际 Host / Origin，与 allowed_hosts / allowed_origins 逐字核对（大小写不敏感，端口必须一致）
curl -v -H "Authorization: Bearer ${BASALT_ADMIN_TOKEN}" http://127.0.0.1:8080/api/v1/profiles
```

持续 429 说明还有其他来源 / 脚本在连续失败——看 WARN 日志的来源归因（`来源进入认证限速` 一行标出 `source` 与 `retry_after_seconds`）。

### 2.2 出网：按通道分开看

| 流量 | 谁控制 | 受不受出网策略 |
| --- | --- | --- |
| 工具出网（`http_get` / `http_post`）+ 通知投递（`notify`） | **模型**可控 | **是**——按当前**执行档位**判定：`strict` 走网络域名白名单（或 `host` 类型访问例外），`standard` 走禁止域名表；两档都先拦禁止域名与受保护地址段；scheme 固定 http/https（代码固定，配置放行不了） |
| 搜索出网（`web_search` → `https://api.deepseek.com/v1/tools/search`） | **模型**可控（查询词）；**目标端点由代码固定** | **是**——与上一行同一套判定，每次外呼各判一次（含 408 / 429 / 5xx 的有限重发）。`strict` 实例**必须**把 `api.deepseek.com` 加入网络域名白名单，否则每次搜索都判 `sandbox_denied`；`standard` 实例未命中禁止域名表即放行 |
| MCP http 传输的连接 | Basalt 发起 | **是**——连接前按当前执行档位与同一套判定授权；`strict` 收紧白名单后不再允许的连接会被断开 |
| 供应商模型调用（`providers[].base_url`） | 运维配置 | 否——模型连接的独立配置，端点由运维定，模型碰不到 |
| MCP server 自己的网络行为（stdio 进程内） | server 自己 | 否——独立进程，策略管不到（**有意接受**的边界，见技术方案 §4.3） |
| 远程 MCP 下游（上游 server 再去连别的服务） | server 自己 | 否——上游连接受策略约束，下游不受；不宣称统一约束 |
| stdio 启动程序（`mcp_servers.yaml` 的 `command`） | Basalt 发起 | **是**——启动前过命令授权：`strict` 逐字比对 `shell.allowed_commands`；`standard` 另放行安装清单内程序目录的可执行件 |

企业若要求"模型调用也只能出内网"：把 `providers[].base_url` 指向内网网关（vLLM / 一方网关）即可，白名单不需要动。

**代理不由进程自己读**：`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 环境变量在 Basalt 里**一律不生效**（HTTP 客户端不读环境代理）。出网要么直连，要么由**运维侧网关**承担（透明代理 / NAT 出口）——这样"哪条流量从哪个口出去"由网络层决定，而不是由某个进程继承到的环境变量决定（否则同一条 `http_get` 在不同 unit / 容器里走向不同出口，白名单还在、边界已经变了）。MCP server 是独立进程，它读不读代理变量是自己的事（同上表第四行）。

**网络域名白名单：唯一权威源在数据库，页面保存即生效**：

- 权威源是数据库（PostgreSQL）里 `network_domain_policy` 的单行。管理台「执行环境 → 网络域名白名单」保存后，**下一次**出站判定即按新规则，**无需重启**；页面显示实际生效的域名、修订号、ETag 与来源
- `basalt.yaml` 的 `http.allowed_domains` **只在数据库还没有这一行时**作为首次种子（新装 / 空表）；既有实例改配置**不会**覆盖页面保存的值，重启也不回退。排查时先看管理台或 `GET /api/v1/sandbox`，不要只看配置文件
- 管理写与其他管理端点同一套认证；先校验（非法条目 400）、再比 `If-Match`（412）、再预写审计（`network_allowlist.update`）后落库。失败保留页面输入且**不得**显示成功；并发保存只有一方成功，另一方 409 需重新读取
- **如实边界**：已发出的请求不能追溯撤回；放宽不自动重连、不补发此前被拒的请求；收紧后不再允许的 MCP http 长连接会断开（保留已应用版本事实），**不自动重连**

**执行档位与出网基础防护（14D）**：

- **两档**：`strict` 只放行上面的白名单（或一条 `host` 类型访问例外）；`standard` 放行未命中禁止域名表的公网域名，另放行安装清单内程序目录的可执行件（登记 + 真实路径在 `runtime/` + 只读保护生效三项同时满足）。权威源是库内 `execution_policy` 单行表；**存量实例升级一律 `strict`**，新实例初始化写 `standard`
- **禁止域名表**（「执行环境 → 禁止域名」）：与白名单同一张表的第二行，**两档都先生效**——命中即拒，不再看白名单；读面在 `GET /api/v1/execution-policy` 的 `denied_domains`
- **受保护地址段**（私有 / 回环 / 链路本地 / CGNAT / 云元数据）在建连前按 DNS 解析结果拒，只拨已校验的 IP；确需访问内网服务时登记**访问例外**（理由必填，进审计），删除后下一次判定即失效。云元数据地址即使加例外也拒
- **端口档固定**：`http` 只认 80（或省略）、`https` 只认 443（或省略）；目标是 IP 字面量的请求直接拒——请用域名 + 例外
- **拒绝细分**：运行记录与工具结果的 `policy_denial_reason` 为 `profile_denied` / `denied_domain` / `protected_address` / `exception_missing`，页面据此给出"该改哪一条"（改动目标见 §5.4）
- **交互式 shell 名称**（`bash` / `sh` / `powershell` / `cmd` 等）两档都拒，命令白名单里写上也不放行

**必须记住的生效时间差**：

- **管理台热生效**：网络域名白名单、禁止域名表、访问例外、执行档位切换（保存即按新规则判定；已发出的请求不能追溯撤回）
- **改完需重启**：`shell.allowed_commands`、`shell.interpreters`、`shell.env_passthrough`、`sandbox.cross_agent_read`、`mcp_servers.yaml`、`http.addr`
- **热加载（下一个 Run 生效）**：`.basalt/agents/` 下的 `AGENT.md` 与附属文件
- **改环境变量也要重启**：`SSL_CERT_FILE` / `SSL_CERT_DIR` 一类只在进程启动时读一次（§2.3）

### 2.3 TLS 信任根

供应商 API 与工具出网都走 HTTPS，信任根决定"哪些服务端能被信任"。三档场景：

| 场景 | 做法 |
| --- | --- |
| 公共 CA（供应商 API、公共数据源） | **系统信任库即可**——但 `scratch` / distroless / 精简镜像**必须**带 `ca-certificates`（缺了以后一切 HTTPS 调用报 `x509: certificate signed by unknown authority`，且这是**运行期**故障：Agent 照常注册、服务照常起来，第一次真调用才炸） |
| 企业自签根 / 内网 CA / 内网 TLS 拦截 | **不得**把根证书写进代码或烧进二进制；用 Go 原生支持的 `SSL_CERT_FILE=<PEM 文件>` 或 `SSL_CERT_DIR=<目录>`（挂载进来的企业根）注入。**Go 1.27 起这两个变量在四个平台都生效**（Windows / Darwin 同样认，据官方 1.27 发布说明）；此前版本在 Windows / macOS 上被忽略、只认平台信任库——本项目版本线是 Go 1.27.2，不受旧行为影响 |
| 想跳过证书校验 | **不做**——v0.1 不提供 `InsecureSkipVerify` 开关（内网自签场景用上一行的注入方式解决） |

一条**反直觉**的语义（照抄进运维手册的原因）：在 Windows / Darwin 上**设置了**这两个变量，Go 就**不再走平台验证器**，改用内置验证器只读你给的根文件——"顺手挂一个企业根"会让**公共 CA 全部失效**（供应商 API 反过来报 unknown authority）。要"企业根 + 公共根并存"，就把两者都放进 `SSL_CERT_DIR` 指向的目录（或把公共 CA 包一并放进 PEM 文件），不要把系统库当默认兜底。

复核（可复制）：

```bash
# 容器 / 主机上信任库在位（有输出即证书文件在）
ls /etc/ssl/certs | head
# 排查时打开平台验证器的 A/B 开关（临时手段，不是长期配置）
GODEBUG=x509sslcertoverrideplatform=0 basalt status
```

症状定位：日志里出现 `x509` / `certificate signed by unknown authority` → **先查信任根**（是否缺 `ca-certificates`、是否把企业根挂成唯一根），不要先怀疑供应商地址或网络。

---

## 3. 时区数据

IANA 时区名（`Asia/Shanghai`）的解析依赖 tz 数据库。Go 默认去系统里读（`/usr/share/zoneinfo`）——**裸镜像 / distroless / 精简发行版没有这个目录**，于是"每天早上 8 点"变成时区解析失败。

**必须**：二进制内嵌 tz 数据库（`import _ "time/tzdata"`）——代价约几百 KB，换来任何镜像上 `Asia/Shanghai` 都能解析。**不得**只写"镜像里装 tzdata 包"（那要求每个部署者都记得做，且 `scratch` / distroless 装不了）。

症状：带 `schedules[].zone: Asia/Shanghai` 的 Agent 在容器里**整只不注册**（加载期校验失败，WARN 点名），定时任务一个都不触发；而本机（有系统 tzdata）一切正常——"本地能跑、容器不跑"的经典成因。

另两条时区口径与需求文档 §7.6 一致：

- `schedules[].zone` 缺省 **UTC**（不回落服务器本地时区；`ZoneId.systemDefault()` 一类的"取系统时区"在本项目是禁用写法）
- 容器建议显式设 `TZ`——`PATH` / `HOME` / `LANG` / `TZ` 是示例配置 `basalt.yaml.example` 在 `shell.env_passthrough` 里登记的透传变量；若部署配置省略该键或配为空列表，子进程**不透传**父进程环境变量（`TZ` 也不例外），`shell` 工具里 `date` 的输出随宿主机漂移

复核（可复制）：`POST /api/v1/schedules/{id}/run` 手动补跑一次带 `zone: Asia/Shanghai` 的任务，看最新 `task_executions` 行的触发时刻（Unix 毫秒经 `zone` 换算）是否为北京时间。

---

## 4. 备份与恢复

> **US-16 口径变更（2026-10-30）**：运行期存储已切换为 PostgreSQL，**库内容的备份现场是 `pg_dump`，不是 `.basalt/basalt.db`**；工作区文件面（Agent / 技能快照 / 控制状态 / markdown 记忆档 / 配置）与密钥材料是另外两个独立备份对象，三者时间点必须配对。**PostgreSQL 的完整备份 / 恢复程序见 §4.4**；SQLite v6 → PostgreSQL 的迁移、切换与回退见 §5.6 / §5.7。本节下文各条描述的是**文件面**——只按 §4.1～§4.3 备份 `.basalt/` 会漏掉全部会话 / 审计 / 知识数据，**不得**把本节当作完整备份程序。

### 4.1 备份什么

`.basalt/` 整目录：

| 内容 | 类别 | 丢了会怎样 |
| --- | --- | --- |
| `basalt.db`（+ `-wal` / `-shm`） | **SQLite 谱系遗留**：迁移到 PostgreSQL 之后只作回退快照，**不是**运行中的权威数据；权威库内容见上方口径变更说明 | 迁移**前**丢它 = 会话 / 消息 / 运行审计三表 + 管理操作审计 / 资源版本与发布指针 / Run 资源引用 / 记忆（sqlite 档）/ 定时状态全丢；迁移**后**丢它不影响运行，只失去回退窗口 |
| `control/agents/*.json` | **不可再生** | Agent 生命周期状态（`active` / `disabled` / `archived`）丢失；DB 索引可由状态文件重建，反向不行。状态文件 schema 2 的 `agent_uid` 是 **Agent 身份**（绑定、专属资源、归档恢复按它关联），丢失后**不得**按同名猜回旧身份。缺失时该 Agent 拒绝新 Run（fail-closed），不会静默当作 `active` |
| 技能不可变版本快照目录（`resource_versions.storage_path` 指向） | **不可再生** | 已发布技能版本的内容丢失；数据库里的清单与哈希还在，但无法还原文件。Run 冻结的资源清单指向这些版本 |
| `agents/`、`skills/` | 配置（可重建，但重建成本高） | Agent 与技能全丢 |
| `memory/*.md`（markdown 档） | **不可再生** | 长期记忆丢（记忆后端为 markdown 时） |
| `mcp_servers.yaml`、`AGENTS.md` / `SOUL.md` / `USER.md` | 配置 | 同上 |
| `archive/` | 已下线 Agent 的目录；恢复默认 `disabled` | 归档配置丢（会话 / 记忆 / 审计不受影响，它们不在文件里） |
| `logs/` | 可弃 | 只丢日志 |

**控制目录 `.basalt/control/` 说明**：`control/agents/*.json` 是 Agent 启停的**唯一权威**（库里的生命周期索引只是可重建副本）。文件由生命周期服务以"同目录临时文件 + 原子 rename"写入，**不得**手工编辑或用脚本改状态——绕过服务写文件会撞上校验和 / 字段校验，校验失败时该 Agent 拒绝新 Run（fail-closed）。该目录同时**不在**配置热加载监听范围、**不在** Agent 文件工具白名单（模型读写它一律被拒）；备份时随 `.basalt/` 整目录一起带走。

**备份一致性**：`basalt.db` 里的资源发布指针、`run_resource_refs` 与技能快照目录**必须**来自**同一次停机快照**。只恢复数据库、不恢复快照目录（或反过来）会得到"指针指向不存在的版本"——恢复验证（§4.3）**必须**检出这类不一致，**不得**当作恢复成功。

**主密钥单独备份**：托管凭据的主密钥（§6.1）在工作区**之外**，`.basalt/` 备份**不含**它。备份数据库时**必须**同时单独备份主密钥文件与它的 `key_id`，并与数据库备份**分开存放**。恢复时必须使用同一把密钥——缺主密钥的数据库备份仍可恢复，但所有托管凭据不可读（只能重新录入），不得尝试伪造密文或静默降级为 env。

**另外，实例配置 `basalt.yaml`（与工作区同级、**不在** `.basalt/` 内）也应一并备份**：它不含明文凭证（`${ENV_VAR}` 占位，见 §6），丢了要重建 provider 端点与各白名单——备份布局见 §4.2（备份目录里 `basalt.yaml` 与 `.basalt/` 并列），恢复验证与真实恢复都按该布局取文件。

### 4.2 两种备法

**离线（推荐，最稳）**：

```bash
systemctl stop basalt
d=/backup/basalt-$(date +%Y%m%d-%H%M%S)     # 一次算好时间戳，两份文件落进同一个备份目录
mkdir -p "$d"
cp -a /opt/basalt/.basalt "$d/.basalt"
cp /opt/basalt/basalt.yaml "$d/basalt.yaml"
# ★ 日常备份到此结束；**升级 / 回滚前**的备份**不得**执行下一行——保持停机，直接接 §5.1 / §5.2
systemctl start basalt
```

停机拷贝前**必须**停服务：WAL 模式下运行中拷贝可能拿到"主库 + 半个 WAL"的组合。

**升级 / 回滚场景的"停"与"起"之间只做三件事**：备份 → 换二进制（升级），或恢复备份 + 换回旧二进制（回滚）——**不得**在二进制就位之前先起一次服务（旧进程带着旧代码起来，页面上"服务恢复了"，而真正要做的整备一件都还没做）。因此 §4.3 的恢复动作与 §5.1 的升级步骤引用本块 / §4.3 时，**跳过 `systemctl start` 那一行**；起服统一发生在二进制换好之后。

**在线（不停机）**——用 SQLite 在线备份（宿主机需 `sqlite3` CLI，不属于运行时依赖）：

```bash
sqlite3 /opt/basalt/.basalt/basalt.db ".backup '/backup/basalt-live.db'"
```

- `.backup` 走 SQLite 备份 API，得到**一致快照**（不是文件拷贝），WAL 里未合并的事务一并包含
- **不得**用 `cp basalt.db` 做在线备份（漏掉 `-wal`，快照可能缺最近的事务）
- 在线备份只覆盖库；`agents/` / `memory/` 等文件仍需单独同步（改动频率低，可放停机窗口里拷）——`basalt.yaml` 同此

### 4.3 恢复与**恢复验证**

恢复动作：

```bash
systemctl stop basalt
mv /opt/basalt/.basalt /opt/basalt/.basalt.broken
cp -a /backup/basalt-20261014-030000/.basalt /opt/basalt/.basalt
cp /backup/basalt-20261014-030000/basalt.yaml /opt/basalt/basalt.yaml   # 配置与工作区成套回滚（取同一次备份）
# ★ 回滚场景（库已被新版迁移）到此**先换回旧二进制**，再执行下一行（见 §5.2）
systemctl start basalt
```

**必须**先在独立目录做恢复验证（工作区 = 当前目录下的 `.basalt/`；一个空目录 + 一份 `basalt.yaml` + 同一套环境变量 = 一个完整实例——三样缺一，都会把"配置没带过来"误判成"备份坏了"）：

```bash
mkdir -p /tmp/restore-check && cd /tmp/restore-check
cp -a /backup/basalt-20261014-030000/.basalt .    # 工作区：库 + Agent 目录 + 记忆
cp /backup/basalt-20261014-030000/basalt.yaml .   # 同一时点的实例配置（与工作区同级，不在 .basalt/ 内）
set -a; . "$BASALT_ENV_FILE"; set +a              # 与生产 EnvironmentFile= 同一份（权限 600）

# ★ 演练隔离——① 只属 `serve`（不起调度器就没有自动触发）；②③ 对**任何会真正跑 Agent 的路径**都建议做
#   （`basalt chat` 里模型同样可能自发调 `notify` / `http_post`，凭证与白名单照样生效）：
#   副本带着生产的任务定义与同源凭证，`serve` 一起就把调度器也拉起来——
#   "换个端口"只解决监听冲突，**不解决自动触发**，副本会按生产的 schedules 真的执行通知与 HTTP 写。
#   因此起 `serve` 之前必须先做三件事（改的都是副本自己的文件 / 变量，不碰生产）：
#   ① 停掉自动触发：清空副本 agents/*/AGENT.md 里的 schedules 段（定义源在文件，清了就没有任务可注册）
#   ② 副作用调用指向测试目标：先查副本里 notify_channels.url 引用的变量名（`${...}` 里的那串），
#      对每一个**二选一**——export 覆盖成测试值（export BASALT_NOTIFY_TEAM_LARK=<测试 webhook>），
#      或**显式 unset**（unset BASALT_NOTIFY_TEAM_LARK）。注意上面 `set -a; . "$BASALT_ENV_FILE"`
#      已经把生产值载进环境了，"不管它"不会让它消失——不覆盖也不 unset，副本用的就是生产凭证。
#      unset 后该变量在**发送时**解析失败（点名环境变量的可读错误）= **失败关闭**；
#      宁可让演练里的调用失败，也不要让副本往生产群里发消息
#   ③ 出网收口：把副本的白名单改成**只含测试端点**——**写成域名形态**
#      （localhost，或 hosts 里自造的测试域名）；**裸 IP 字面量会被直接拒**（白名单按域名匹配，
#      放行 IP 等于绕过它，见技术方案 §4.3.1）。未被列入的域名调用以 sandbox_denied 失败
#      ——正是演练想要的"副作用走不出去"（把白名单清空是最严的收口，一样达到目的）
#      ⚠ 副本库自带生产的策略行：改副本 basalt.yaml 的 http.allowed_domains **不会**生效
#      （配置只在空表时作种子，见 §2.2）。做法二选一：起服务（已停调度）后到管理台
#      「执行环境 → 网络域名白名单」保存测试端点；或使用空库演练让配置种子生效。
#      **不得**用 SQL 直接改表——那会绕过审计与恢复，演练证据也就不成立

basalt status
basalt session list      # 关键判据：能看到备份时点之前已有的会话
```

四点说明（都是"照做就不会踩"的坑）：

- `basalt.yaml` 里 `providers[].api_key` 等是 `${ENV_VAR}` 占位——**占位符未解析或为空会拒绝启动**（§6），所以环境变量必须与生产同源；该文件本身不含明文凭证，可以随验证目录临时拷贝
- 该目录有**自己的实例锁**（`.basalt/basalt.lock`，操作系统级独占文件锁——进程退出即由内核释放，崩溃后不需要手工清理），与生产实例互不影响；但**不得**把验证指向正在运行的生产目录——写模式命令会被生产实例持有的锁拒绝（这正是锁的用途，不是故障）
- **演练副本不是生产**：同源凭证 + 生产任务定义 = "一个随时会自己跑起来的生产分身"。**不得**在未做上述三件隔离的情况下起 `serve`；调度器触发的是真实执行（`notify` 真的推送、`http_post` 真的写），不是干跑
- 步骤 4、5 有 **CLI 路径**与 **HTTP 路径**两条（见判据，选一条走到底）：CLI 路径最省隔离——`basalt chat` 不起调度器、不起 HTTP，"不用 serve"就是最省的隔离；HTTP 路径要先起 `serve`，故必须做完三件隔离，并把 `http.addr` 改到别的端口（如 `127.0.0.1:8081`）——生产实例占着 8080，第二次绑定失败不是恢复失败

判据（每次备份演习与每次真实恢复都要走完）：

1. `basalt status` —— 配置与工作区状态正常
2. `basalt session list` —— 列出备份时点之前的既有会话（**读到既有会话 = 库真的恢复了**）
3. `basalt profile list` —— Agent 数量与预期一致
4. 工具注册表非空（证明白名单与注册链路正常）——CLI 路径 `basalt tool list`；HTTP 路径 `GET /api/v1/tools`
5. 手动触发一次，走完整链路，确认审计表新增记录——CLI 路径 `basalt chat` 发一条；HTTP 路径 `POST /api/v1/sessions/{id}/messages`

**判据 4、5 必须走同一条路径，不得混用**（`chat` 与 `serve` 都要拿工作区实例锁，见下方说明 2）：**CLI 路径** = 全程不起 `serve`，判据 4 用 `basalt tool list`、判据 5 用 `basalt chat`；**HTTP 路径** = `serve` 起着（且已做完三件隔离），判据 4 用 `GET /api/v1/tools`、判据 5 用 `POST /api/v1/sessions/{id}/messages`。`serve` 还在跑时 `chat` 会被拒绝启动——那不是恢复失败，而是实例锁在起作用。**不要**在演练副本上用 `POST /api/v1/schedules/{id}/run`：它要求副本保留 `schedules` 定义，与隔离 ① 直接冲突；要连定时链路一起验，请在独立的测试工作区里做

**不得**在原数据上做第一次恢复验证——验证本身可能覆盖要保的东西。

### 4.4 PostgreSQL 运行期备份与恢复（US-16 起）

运行期有**三个独立**的备份对象，缺一不可，时间点**必须配对**（同一个时间戳目录、同一次停机窗口）：

| 对象 | 内容 | 备份手段 | 丢了会怎样 |
| --- | --- | --- | --- |
| 数据库 | 会话 / 消息 / Run 与调用审计 / 管理操作 / 资源版本与发布指针 / 记忆 / 知识库与文档 / 调用方与配额 / 压缩条目 / 诊断事件 | `pg_dump -Fc`（逻辑备份） | 全部业务数据与审计丢失 |
| 工作区文件面 | `.basalt/`（`agents/` / `skills/` / 技能不可变版本快照 / `control/` / `memory/*.md` / `archive/` / 日志）+ 同级 `basalt.yaml` | 停服后整目录拷贝 | 定义源、生命周期状态、技能版本快照、markdown 记忆档丢失 |
| 密钥材料 | 实例私有目录的 `master.key` 与 `admin-token`（与主密钥同目录、分文件；路径按名册的 `instance_id` 推导，Linux 缺省 `$XDG_DATA_HOME/basalt/instances/<instance_id>`，未设则 `~/.local/share/basalt/instances/<instance_id>`） | **单独受控备份**（ACL 保护的位置） | 有库无钥 = 托管凭据全部不可解（恢复态）；有钥无库 = 数据丢失 |

四条纪律：

- **主密钥材料不得进入数据库备份包、迁移导出包（`db export` 的 `package/`）或日志**——托管凭据的密文在库里，能解开它们的只有那把密钥；导出包泄露不等于凭证泄露，把密钥塞进去才会
- `admin-token` 与主密钥**分开**保管：两者的丢失后果与恢复方式不同（密码丢了重新生成即可，主密钥丢了只能按 §6.4 恢复记录或重录凭据）
- 备份目录收到部署者 ACL（Unix `0700`，文件 `0600`；Windows 不沿用继承的宽松 ACL）
- 切换后 `.basalt/basalt.db` 只是**回退快照**，不再是运行中的权威数据；只备份它 = 漏掉全部业务数据

**备份（Linux，停机窗口内）**：

```bash
systemctl stop basalt
d=/backup/basalt-$(date +%Y%m%d-%H%M%S)
mkdir -p "$d"

# 1. 数据库：逻辑备份（自定义格式，可选择性恢复）。连接参数走 ~/.pgpass 或受限 PGPASSFILE，
#    **不得**把带密码的 DSN 写进命令行 / 脚本 / 报告（进程表与 shell 历史都会留痕）
pg_dump -Fc -h 127.0.0.1 -U basalt -d basalt -f "$d/basalt.dump"

# 2. 工作区文件面与配置（与库同一时间点）
cp -a /opt/basalt/.basalt "$d/.basalt"
cp /opt/basalt/basalt.yaml "$d/basalt.yaml"

# 3. 密钥材料单独受控备份：从实例私有目录复制 master.key 与 admin-token，
#    目标位置按部署者的受控保管方案（不是 $d 下的普通文件，也不与库备份同盘）
#    示例：install -m 600 <私有目录>/master.key / secure-backup/basalt-<ts>/master.key

systemctl start basalt
```

在线（不停机）只能做 `pg_dump`——一致性快照由数据库的 MVCC 保证。工作区文件面**必须**停机拷贝：运行中的 `runs/` 产物与 `state/*.json` 没有跨文件一致性保证。**不得**把在线库备份与另一个时间点的文件面拼成"一个备份集"。

**恢复**：

```bash
systemctl stop basalt

# 1. 建一个空库（不要覆盖正在使用的库）
createdb -h 127.0.0.1 -U postgres -O basalt basalt_restore
# 2. 还原数据库
pg_restore -h 127.0.0.1 -U basalt -d basalt_restore --no-owner "$d/basalt.dump"
# 3. 工作区与配置成套回滚（取同一次备份）
mv /opt/basalt/.basalt /opt/basalt/.basalt.broken
cp -a "$d/.basalt" /opt/basalt/.basalt
cp "$d/basalt.yaml" /opt/basalt/basalt.yaml
# 4. 密钥材料按受控流程放回实例私有目录（权限不变）
systemctl start basalt
```

**恢复验证**（每次演习与每次真实恢复都要走完）：

1. 先在独立库（如 `basalt_restore`）+ 独立工作区目录做恢复，`basalt.yaml` 指向该库、换端口起 `serve`——§4.3 的"演练隔离"三步同样适用（停调度、副作用调用指向测试目标、出网收口）
2. `basalt status` —— 存储节报 `PostgreSQL，schema v1，N 张表`
3. `basalt session list` —— 读到备份时点之前的既有会话（**读到 = 库真的恢复了**）
4. `basalt profile list` —— Agent 数量与预期一致
5. 核对 `storage_cutover` 行（切换过的实例）与 `assets.json` 逐项哈希（文件面）
6. 主密钥就位后 `basalt secrets verify` 通过（托管凭据全部可解、绑定不变）——**没有**这一步，"库恢复了"只证明读表成功，没证明密文可用

**恢复后仍缺主密钥**：实例处于 `recovery_required`（原因码见 §5.8.1），托管凭据 fail-closed；按 §6.4 从可信备份恢复初始化记录，或按 §6.5 处置 schema 3～5 存量。**不得**重新生成主密钥或重建实例来"修复"——那会让既有密文永久不可解。

---

## 5. 升级、回滚与停机

### 5.1 升级（受控前向迁移）

> **US-16 起（2026-10-30）**：运行期存储是 PostgreSQL，升级 = 停服 → **按 §4.4 做三个对象的成套备份** → 换二进制 → 起服（启动时对 PostgreSQL 做内嵌版本化前向迁移；`basalt db migrate` 可预跑，幂等）。**SQLite 谱系冻结在 v6，只作迁移来源与回退快照，不再新增迁移**；SQLite v6 → PostgreSQL 的一次性迁移与切换见 §5.6。下文第 4 步之后的 v2～v5 迁移叙述是 **SQLite 谱系的历史记录**，保留供对账，不再是当前升级路径。

顺序**必须**固定，且**从停服到二进制就位之间不得起服**：

1. **停服**：`systemctl stop basalt`
2. **备份，并保持停机**：按 §4.4 做三个对象的成套备份（库 + 工作区文件面 + 密钥材料；旧 SQLite 实例另按 §4.2 留文件面快照），**跳过其中 `systemctl start` 那一行**——升级前必做，这是回滚的唯一依托；备完不起服，直接接第 3 步
3. **换二进制**：旧版改名留底（`mv basalt basalt.bak-<旧版本>`）→ 放新二进制 → `chmod +x`
4. **启动并迁移**：`systemctl start basalt`——启动时按 `schema_version` 应用增量 SQL（迁移脚本入库、随二进制发布；**已发布的迁移不得改写**，只许追加新版本）。迁移失败**必须**报错退出——**不得**带着半迁移的库继续跑。US-11 / US-12 的首次升级从 `schema_version=1` 前向到 **2**：新增管理操作审计、资源版本 / 指针 / 引用、Agent 生命周期索引与归档表，**不改既有表**；SQL 迁移后还要执行一次资源基线（为已有 Provider / MCP / 技能建立 v1 已发布版本、为已有 Agent 建立初始状态文件，初始状态 `disabled`）。US-12 范围修订把 **2 → 3**：新增 UID 级资源身份 / 墓碑、凭据与凭据版本、绑定投影、通知 Run 引用与发送明细，重建 `resource_versions` / `notify_channels`，给 `resource_pointers` / `run_resource_refs` / `agent_lifecycle` / `agent_archives` 加列；顺序固定为**状态文件先升级（分配 `agent_uid`、升为 schema 2）→ SQL 迁移 → 绑定回填**。两次迁移失败都拒绝启动、重跑必须幂等；v3 前必须确认**没有活动 Run**。US-12 R7 再把 **3 → 4**：新增 `schedule_deliveries`（每轮运行的系统代投权威记录，主键含运行标识），**只追加新表、不改既有表**；迁移失败同样拒绝启动、重跑幂等。US-12 R8 再把 **4 → 5**：新增 `network_domain_policy` 单行表（网络域名白名单的唯一权威源）；升级后表内无行，**首次启动**按 `basalt.yaml` 的 `http.allowed_domains` 写入种子，此后配置键不再读取、页面保存即热生效（§2.2）。三次迁移失败都拒绝启动、重跑必须幂等。升级到 v5 后旧二进制因 `schema_version` 高于自身支持版本而拒绝启动，回滚仍按 §5.2「备份 + 旧二进制」
5. **验证**：`basalt status` → `basalt session list` → `GET /api/v1/health` → 跑一次真实调用（同 §4.3 判据的 1~5）
6. **观察**：`logs/` 里确认无 ERROR 级噪声

**不得**把"停"与"起"拆成两段（先起旧版 → 换文件 → 再起新版）：那两段之间跑的还是**旧代码**——迁移没跑、验证假绿，真正的切换只发生在文件替换那一刻，而没有任何步骤检查过它。

建议：先用备份在独立目录起一次新版做 dry run（与 §4.3 同一手法，含其中"演练隔离"三步），确认迁移与启动无误再动生产目录。

**升级后的首个动作**：v2 资源基线把既有 Agent 的状态统一初始化为 `disabled`（**不得**在升级瞬间悄悄放行新 Run），因此升级完成后需要逐个复核并按需启用——`POST /api/v1/agents/{name}/enable` 或管理台「Agent → 启用」。这是设计行为，不是迁移失败；`GET /api/v1/profiles` 里 `runnable=false` 是同一件事的表现。

**v3 升级不重置生命周期状态**：`2 → 3` 只为活动 Agent 补 `agent_uid` 并回填可证明的绑定，不改变 `active` / `disabled`。升级后如页面显示某 Agent「引用未记录（迁移前无法证明身份）」，说明该资源名在迁移前经历过删除 / 重建或操作台账缺失——这是如实标记，不是迁移失败；显式重新选择绑定即可恢复准入。数据库迁移失败而状态文件已升级为 schema 2 时，**不得**继续用旧库运行，按 §4.3 恢复备份。

### 5.2 回滚

| 情形 | 动作 |
| --- | --- |
| 只是二进制问题（库结构未变） | 停服 → 换回旧二进制 → 起服（**不动**库与配置） |
| 库已被新版迁移 | **旧二进制 + 升级前备份**：停服 → 按 §4.3 恢复库与配置（**跳过其中 `systemctl start` 那一行**）→ 换回旧二进制 → 起服 |

两种情形同样是"**停 → 整备 → 起**"：**不得**在换回旧二进制之前先起一次服务——旧二进制面对已被新版迁移过的库，正是"库结构对不上"的现场，让它起来只会在日志里再制造一轮错误。

核心阶段只做**前向迁移**，不提供反向迁移脚本——**不得**指望"降级脚本"，回滚的唯一可靠路径是"备份 + 旧二进制"。

另一道保险：即使忘了先恢复备份，旧二进制打开已被新版迁移的库时会因 `schema_version` 高于自身支持版本而**拒绝启动**（fail-closed），不会以旧结构误读新库。

### 5.3 停机顺序与工作目录

**停机是有序收尾，不是杀进程**。收到 `SIGTERM`（`systemctl stop` 的默认信号）后，顺序**必须**是：

1. **停调度新触发**：调度器不再接受新的触发点（在跑的照常收尾，不新起 Run）
2. **取消并等待在跑 Run**：向所有在跑 Run 发出取消，**有界等待**其退出——等的是"审计与消息落库完成"，不是"让模型把话说完"
3. **停 HTTP**：`http.Server.Shutdown`（拒新连接、等在途请求结束）
4. **关 MCP**：关闭 MCP 客户端与其子进程（stdio server 的子进程不留给系统收割）
5. **关库**：关闭 PostgreSQL 连接池（在途事务已提交或回滚，不把半截事务留给服务端）
6. **退出**——`.basalt/basalt.lock` 由**内核**释放（独占文件锁随进程消亡解锁）：**不得**写"退出前删锁文件"的收尾，那会在删除与下一个进程加锁之间制造竞态

宽限：systemd 的 `TimeoutStopSec=` **必须大于**停机宽限（工具执行超时可达分钟级）。**不得**指望 `SIGKILL` 兜底——被强杀的进程留下 `running` Run 与 `pending` 审计行，下次启动要走恢复扫描收尾（能收敛，但白丢一次干净退出）。

**工作目录就是工作区**（§1.1：文件面状态全在"当前工作目录的 `.basalt/`"，库数据在 PostgreSQL），因此 systemd 单元**必须**显式给 `WorkingDirectory=`：

```ini
[Service]
WorkingDirectory=/opt/basalt
EnvironmentFile=/etc/basalt/env
ExecStart=/opt/basalt/basalt serve
TimeoutStopSec=90
```

- 不写 `WorkingDirectory=` 时，systemd 把工作目录定为 `/`——实例会在根目录下**新建一个空工作区**起服务，重启后"Agent 全不见了、会话也没了"（其实都在 `/` 下那个新工作区里）。这是自建 unit 最常见的静默事故
- 容器里同理：工作目录由 `WORKDIR` 决定，**不得**在 `CMD` 里 `cd` 拼路径（工作区位置必须写在镜像里、可复核）
- `EnvironmentFile` 权限 `600`、**不得**用 `ExecStart` 拼环境变量（`ps` 可见，§6）

复核（可复制）：

```bash
systemctl show basalt -p WorkingDirectory -p TimeoutStopUSec -p KillSignal   # 三项都要有值且符合预期
systemctl stop basalt && systemctl show basalt -p ActiveState                # 期望 inactive；日志里应有停机收尾序列
```

### 5.4 升级说明：MCP 命令白名单与旧格式迁移

US-12 起 MCP supervisor 在建立连接前先做策略判定（约束 47，连接失败即不连接、不接管）：

| 传输 | 连接前判定 | 未通过时 |
| --- | --- | --- |
| `stdio` | `command` 的可执行名**必须**在 `shell.allowed_commands` 里 | 拒绝连接，WARN 点名 server 与可执行名 |
| `http` | URL 的域名**必须**在**当前生效**的网络域名白名单里 | 拒绝连接，WARN 只记 scheme + host |

因此升级后 MCP 全部连不上时，先核对白名单，而不是先怀疑网络：

```bash
grep -n "allowed_commands" /opt/basalt/basalt.yaml   # stdio server 的 command 是否在内
# http server 的域名看当前生效值（数据库是权威源，配置只在空表时作种子）：
curl -s -H "Authorization: Bearer ${BASALT_ADMIN_TOKEN}" \
  http://127.0.0.1:8080/api/v1/sandbox | \
  jq '.data.network_allowlist, .data.http_allowed_domains'
```

旧格式的多词 `command`（整行命令写在 `command` 里、读取时按空白切分）在**读取既有
文件**时继续兼容：按约束 8 的兼容路径拆成 `command` + `args`，行为与旧的按空白切分
逐字等价，启动日志给出 WARN 提示迁移。管理员一旦在管理台**编辑**该条目，候选走
严格校验，多词 `command` → 400 `BAD_REQUEST` 并点名"改用 `args` 字段"，已发布版本与
文件**保持不变**。手工改写示例：

```yaml
# 旧格式：读取兼容，编辑会被拒绝
servers:
  - name: files
    command: "npx -y @modelcontextprotocol/server-filesystem /data"

# 新格式：command 是单个可执行名，参数进 args
servers:
  - name: files
    command: npx
    args:
      - "-y"
      - "@modelcontextprotocol/server-filesystem"
      - "/data"
```

把 `npx`（或实际可执行名）加入 `basalt.yaml` 的 `shell.allowed_commands` 后重启。
旧技能目录同样收紧：含二进制文件或软链接的目录无法通过管理台发布，启动时以 WARN
点名而不是静默跳过；**不得**为兼容旧目录放宽这些边界。

#### 升级影响清单（受保护地址与 shell 名称）

14D 起，执行档位迁移（`schema_version` 5 → 6）把存量实例落到 `strict`，并把两类存量条目列为**待确认项**：

- `network_allowlist` 轴：出网白名单中解析到受保护地址段（私有 / 回环 / 链路本地 / CGNAT / 云元数据）的域名条目；
- `shell_command` 轴：命令白名单中命中交互式 shell 名称（`bash`、`sh`、`powershell`、`cmd` 等）的条目。

两类条目升级后**默认拒绝**。系统在升级后第一次启动就列出清单，**不**等到第一次运行失败才让人去猜原因。

处置口径：

- 两轴各自独立，不合并成一个"策略问题"列表；通配条目（`*.example.com`）不可解析，不列入，也不猜测。
- 每个待确认项给两个显式动作，**不自动勾选、不默认放行**：
  - 「加例外」→ `POST /api/v1/network-exceptions`（仅域名条目；写 `network_exception.create` 审计，放行理由必填）
  - 「保持拒绝」→ `POST /api/v1/execution-policy/confirmations`（写 `execution_policy.confirm` 审计，**不改授权结论**）
- shell 名称集合是能力上限声明，两档都不放行；该轴只有「保持拒绝」，它只记录"管理员逐条看过"，不改变拒绝结论。
- 未确认的条目继续列出、继续拒绝；确认与例外都**不**推进档位修订号。解析失败（NODATA / 超时）的条目不计入待确认项。

查看（只读，不写操作行、不改策略）：

```bash
curl -s -H "Authorization: Bearer ${BASALT_ADMIN_TOKEN}" \
  http://127.0.0.1:8080/api/v1/execution-policy | \
  jq '.data.pending_confirmations'
```

返回值的形状（契约 `PendingPolicyConfirmation`，值为示意）：

```json
[
  {
    "id": "network_allowlist:intranet.example.com",
    "axis": "network_allowlist",
    "value": "intranet.example.com",
    "reason": "protected_address",
    "resolved_protected": ["10.20.30.40"]
  },
  {
    "id": "shell_command:bash",
    "axis": "shell_command",
    "value": "bash",
    "reason": "shell_name",
    "resolved_protected": []
  }
]
```

管理台入口：执行环境页「待确认项」两组（`/console/settings/sandbox`）。

升级前怎么跑：按 §5.1 的建议，先用备份在独立目录起一次新版做 dry run——`pending_confirmations` 是只读派生信息，在这个副本里把清单看过并确定每条怎么处置，再换生产目录的二进制。升级后的真实清单按当时的白名单与命令名重新派生；已处置的条目不再出现。**不得**为了让清单变短而改动当前实例的配置或数据。

回退不受影响：仍按 §5.2「备份 + 旧二进制」。清单与处置记录都在库里，随备份一起恢复或丢弃。

### 5.5 本机重部署（Windows 开发 / 演示实例）

**自动更新本机实例，不手工杀进程、不手工搬文件**。脚本 [scripts/redeploy-local.ps1](../scripts/redeploy-local.ps1) 在实例目录执行：

```powershell
# 凭据只经进程环境继承；脚本不读取、不打印、不落盘
$env:BASALT_ADMIN_TOKEN = ...   # 由部署者按 §6 的安全方式注入
powershell -NoProfile -File D:\code\basalt\scripts\redeploy-local.ps1            # 沿用已构建前端
powershell -NoProfile -File D:\code\basalt\scripts\redeploy-local.ps1 -Frontend  # 先重跑前端构建
```

顺序**必须**是：识别监听 PID → 核验它是本仓库 `bin/basalt` 且命令行含 `serve`（不符即拒绝，`-AllowOtherBinary` 才放行）→ `CGO_ENABLED=0 go build` 到 `bin/basalt.new` → **优雅停止**（CTRL_BREAK；超时才 `taskkill /F` 并在输出里记 `Stop=forced`）→ 旧件留 `bin/basalt.old`、换上新件 → 以同一工作目录启动（独立隐藏控制台，日志写 `<工作目录>/logs/basalt-<时间戳>.out.log` / `.err.log`）→ 校验。

| 校验项 | 判据 |
| --- | --- |
| 存活 | `GET /api/v1/health` 返回 `data.status=ok` |
| 管理认证 | 带 `BASALT_ADMIN_TOKEN` 的 `GET /api/v1/system/overview` 返回 200；缺 token 时明确报 `Admin=unverified`，**不得**当成已验证 |
| 构建标识 | `/api/v1/info` 的 `version`；`/system/overview` 的 `database.schema_version`（PostgreSQL 谱系自 v1 起；SQLite 谱系另计，冻结在 6，两者**不得**互相套用） |
| 前端资源 | 按 `assets/console/dist/integrity.json` 逐项取 `/console/` 与 `/console/assets/...`，SHA256 必须与清单一致；不一致即失败 |

脚本**不改写**配置、工作区、数据库与主密钥，也**不**自动回滚：`schema_version` 是前向迁移，失败时保留 `bin/basalt.old` 与日志供人工按 §5.2 处置。前端资源哈希不符说明运行中的二进制不是当前 `dist`——加 `-Frontend` 重新构建后重跑。

**Windows 启动解析陷阱（脚本已规避，手工启动同样适用）**：同目录若还留着 `bin\basalt.exe`，经 `cmd /c` 或只写无扩展名的 `bin\basalt` 启动时，CreateProcess 会按追加 `.exe` 的规则解析到 `basalt.exe`——结果是旧构建在跑、部署却报成功。演练实测：cmd 启动实际执行的是 `bin\basalt.exe`（schema 2、旧前端 0/3），改成 `Start-Process -FilePath <完整路径>` 直连后才落到新构建（schema 4、资源 3/3）。因此脚本用完整路径直接启动；手工启动也**必须**给完整路径，并用 `Get-CimInstance Win32_Process` 的 `ExecutablePath` 复核实际镜像。

### 5.6 SQLite v6 → PostgreSQL 的一次性迁移与切换（US-16 起）

已在 PostgreSQL 上的实例日常升级走 §5.1；本节只适用于**数据还在工作区 `basalt.db`** 的旧实例。迁移是一次性的：切过去之后 `basalt.db` 只作回退快照、不再写入，此后备份按 §4.4。

#### 迁移前置

- PostgreSQL 按 §1.2 建库；`vector`（0.8.x，启动要求 `hnsw.iterative_scan` 能力）与 `pg_trgm` 由部署者 `CREATE EXTENSION` 装好——应用**不**自动创建扩展。
- `basalt.yaml` 的 `database.dsn` 写成 `${ENV_VAR}` 占位且能解析出非空值。占位残留、为空或变量缺失 → **拒绝启动 / 拒绝执行**，没有回退 SQLite 的分支。
- 工作区有实例名册 `instance.json`。没有时 `db export` 直接拒绝（`instance_identity_missing`）——存量无名册实例先按 §6.5 跑 `basalt instance adopt`，**不得**把它当新实例自动生成，也**不得**手写一个 `instance_id`。

**迁移包**：`db export` 的产物目录，含 `manifest.json`（批次号 / 源谱系版本 / 实例标识 / 快照摘要）、`assets.json`（工作区资产逐项哈希）、数据文件与 `review/`。格式 `format_version=1`、`source_schema_version=6`；字段带固定类型标签（`i` 整数 / `f` IEEE754 十六进制浮点 / `s` UTF-8 文本 / `b` base64 字节），NULL 无标签（JSON `null`）。包内**不含**主密钥与任何明文凭据：密文随库走，密钥按 §4.4 单独受控交接。

**步骤**（顺序固定；第 1～3 步不碰目标库，第 4 步之后任一步失败都停在原地）

1. **停机**：按 §5.3 停 `serve`，确认没有在跑的 Run、没有在途通知或摄取。停机窗口里只允许一个实例身份写入。
2. **导出**（离线，不要求 DSN；命令取工作区实例锁，实例必须已停）：

   ```bash
   basalt db export --workspace /opt/basalt/.basalt --out /backup/us16-pkg
   ```

   有未归类文件时只产出**受限核查包**（命令非零退出、报告 `deliverable=false`）：按包内 `review/` 逐条处置后封包，再继续。

   ```bash
   basalt db export --seal /backup/us16-pkg --review-file /backup/us16-review.txt
   ```

   未封包的核查包**不得**导入：`db import` 与 `db switch` 都拒收。

3. **复核包**（离线）：`basalt db verify --dump /backup/us16-pkg`
4. **导入**（要求 DSN，单事务）：`basalt db import --dump /backup/us16-pkg`

   - 目标必须是空库：业务表非空、已有 `storage_cutover` 行、或包处于 pending 复核状态 → 拒绝，不会导入半份数据。
   - 导入**不含** `master.key`，也不改写密钥；`instance_secrets` 记录随包进库。
   - **规模经验值来自本机演练，不是容量保证**（2026-11-01 读数与计量口径见 `specs/016-knowledge-memory-history/research.md` R-05）：导入把整表 dump 一次性读进内存，**那个 CLI 进程**的峰值 RSS 在演练夹具上约 **4.2–4.7 KiB/条**，同时写约 **3.5 × dump 字节**的 WAL（LSN 增量，不是稳态写入速率）。参考点：**100 万条消息 → 导入进程峰值 RSS 4,165,116 KiB（4.27 GB / 3.97 GiB）、WAL 4,729,189,056 B（4.73 GB / 4.40 GiB）、整窗 253 s**（PG 17.11，i7-12700，12 GiB 可用内存；表内 RSS 列单位是 **KiB**，WAL 与 dump 列是**字节**，换 GiB 按 1024³ 计）。这几条比值随**消息长度、索引配置、并发与 PG 参数**变化，**不得**按 `可用内存 ÷ 4.5 KiB` 之类的公式推算"能导多少条"；容量规划必须在目标机上用目标数据抽样重跑 `scripts/exp/migration_scale_drill.sh`。导入前确认 dump 所在分区剩余空间 ≥ 4 × dump 字节（WAL ≈ 3.5 × dump + 余量）。
   - **资源不足怎么办**（三选一，**不得**改成部分提交）：耗时超维护窗口 → 扩大窗口（别只算导入，导出与两次校验同量级）；WAL / 磁盘不足 → 先扩分区再跑；内存不足 → 加内存，或**评估分区 / 分批多事务**——后者放弃"失败即空表"的单事务承诺，属产品与契约变更，必须走模块评审，运维现场**不得**用循环调用 `db import` 绕过。这类失败是安全失败：OOM 或任一校验失败都在单事务内整体回滚，目标库保持空业务表。

5. **核对目标库**：`basalt db verify --dump /backup/us16-pkg --target`
6. **交接核对**（离线，只读）：`basalt db verify --handoff <copy-root>/.basalt`

   核对项：名册、`instance_secrets` 记录、密钥路径与权限、全部密文可解、目标配置。`<copy-root>/.basalt` 是**副本的工作区根**（与 `db export --workspace` 同一口径），同级的 `basalt.yaml` 是被核对的配置。密钥在原配置路径可用时，直接对本工作区跑这条命令也成立——它是只读的。

7. **切换**：`basalt db switch --dump /backup/us16-pkg --workspace /opt/basalt/.basalt`

   - 重跑三道门禁（包内资产 / dump 摘要 / 目标库）；再加第四道——源端自快照以来未漂移，漂移即 `source_changed_since_snapshot`。`--workspace` 因此是**必填**：缺省即拒绝（在调用端口之前、零写入），**不得**把没跑的判据说成通过。
   - 通过后写 `storage_cutover` 行（`first_write_at` 留 NULL，留给首个业务事务）。**切换事实只认这一行**，不是"配置里改了 DSN"；同一导出批次不得切换两次。
   - `db switch` **不**跑交接门禁、也不声称跑过——第 6 步必须先通过。
   - 切换前**不要**起服：目标库在导入完成前接受请求，正是 C-39 禁止的"目标实例提前对外服务"。产品没有在启动时拦这一步（全新安装本来就没有 `storage_cutover` 行），这条是操作纪律。

8. **验证窗口**（切换后、首个业务写入前）：从副本根执行。

   ```bash
   cd <copy-root>
   basalt serve --verification-window --asset-package /backup/us16-pkg
   ```

   - 窗口在**任何装配之前**分流：不取实例锁、不迁移、不启动调度 / 通知 / MCP / 模型 / 配置监听，也**不**接受 Run 准入——"窗口内禁写入"是结构性的，不是一组开关。HTTP 面只回环，只有 `/api/v1/health` 与 `/api/v1/ready`。
   - 离线门（身份 / 密钥 / 全部密文 / 配置 + 资产）通过后才开监听；不给 `--asset-package` 时报告明写"资产维度未核对"。核对完按 `SIGINT` / `SIGTERM` 结束窗口。
   - 退出码非零时报告带稳定码：`assets_mismatch` / `database_unavailable` / `cutover_marker_missing` / `database_written` / `verification_window_incomplete` / `listener_not_loopback`；回退结论只有五值之一（`allowed` / `blocked:external_action` / `blocked:database_written` / `blocked:workspace_changed` / `blocked:unknown`），判定口径见 §5.7。
   - 数据库维度读的是**真库**，不是"配置里没改"。开窗口前先读 `storage_cutover` 并给全部用户表拍行数基线：没有切换行 → `cutover_marker_missing`（拒绝，不把库当窗口对象）；读失败 → `database_unavailable`；`first_write_at` 已非空 → `database_written`（拒绝开监听）。窗口结束再读一次：`first_write_at` 由空转非空、或任一业务表行数变化，判**脏**；起止之间切换事实变了、或结束重读失败，判**无法证明**（`blocked:unknown`）。
   - 窗口的行数对比只覆盖**窗口起止**——它不证明"切换之后、开窗口之前"没有写入；那段时间由 `db verify --dump … --target` 的导入基线发现（§5.7）。当前 `first_write_at` 只有经 `MarkFirstWrite` 的写路径会写，窗口的行数基线同时覆盖绕过它的写路径。
   - **没有**离线版 `secrets verify`：在线 `secrets verify` 会装配运行期并启动 MCP，**不得**在此期间运行；密钥与密文的离线核对由第 6 步的 `db verify --handoff` 承担。

9. **起正式服务**：`systemctl start basalt`。首个**成功业务事务**写入 `first_write_at`，回退窗口在这一刻关闭——之后只前滚（§5.7）。

**切换后**：`basalt.db` 留在原处作回退快照、不再写入；后续备份按 §4.4。已发出的通知与外部写按清单对账并人工补偿——即使发生在 `first_write_at` 之前，它们也**不会**随回退撤销。

### 5.7 切换后的回退边界（三维判据）

`first_write_at` 只证明**数据库业务事务**已经写入；它不证明工作区文件有没有变、有没有发出通知或发生外部写。所以"`first_write_at` 还是 NULL"本身**不构成**可以无损回退的证据。

| 时点 | 允许的动作 | 条件 / 代价 |
| --- | --- | --- |
| 导出 / 校验 / 导入失败，尚未切换 | **无损**回到旧实例 | 源库未被写；清掉目标库的导入残留即可重来 |
| 切换后、首次成功业务写入前（回退窗口 R1） | 满足三维判据时**可以**回退 | 目标端可能已有非业务副作用（连接测试、通知试探、工作区文件变化），必须逐维证明 |
| 首次成功业务写入后 | **只前滚** | 新数据只写在 PG；重启旧 SQLite 会丢数据，**不得**描述成无损回退 |
| 切换后已发通知 / 已发生外部写 | 前滚 + 人工补偿 | 外部动作撤不回来；必须列出对象、时间与处置 |

**三维判据**（每维三值：干净 / 脏 / 无法证明；三维全"干净"才允许自动回退）：

| 维度 | 干净的条件 | 让它变脏的行为 |
| --- | --- | --- |
| 数据库业务写入 | `first_write_at IS NULL`，且各表行数 / ID 集 / 摘要与导入报告一致；除导入与切换行外没有新行；不可忽略的管理变更集合为空。**`memory_entries` 单列**：基线取「受控导入完成后」的实测行数，只有同时给出「导入前 / 导入条数 / 导入后实测」三个数且对账成立（导入前 + 导入条数 = 导入后实测）才作数；未申报、少行或证据与报告不符 → 判**脏 / 无法证明**，不得按干净处理 | 切换后跑过一次 Run / 摄取 / 记忆写入（`first_write_at` 已写）；表行数比导入报告多；已撤销或轮换过 Key / 凭据 |
| 工作区文件 | `assets.json` 逐项哈希与切换时一致；没有新增 `runs/` 产物；`state/*.json` 未变 | 验证时触发了一次任务、写出 `runs/<ts>/report.md`；改了 `seen.json` / `baseline.json` / `cursor.json` |
| 外部副作用 | 没有新的 `notify_deliveries` 行、没有外部写型 `tool_invocations`、没有已发出的通知 / 外部 API 调用 | 通知测试真的发到了群里；MCP 写工具改了外部系统；`web_search` 的计费调用已发出（费用撤不回，仍须登记） |

**判定协议**：先冻结目标端写入（不启动 `serve`，或启动后立即停调度与 Run 准入）→ 逐维取证 → 三维全干净才自动回退，且回退入口先过 `first_write_at IS NULL`（非空即拒绝）→ 任一维"脏"或"无法证明"就停自动回退、转人工核查。**不得**因为"哪一维看起来不重要"就跳过。回退动作本身也留痕：`storage_cutover` 行保留不改写，另记回退报告。

`serve --verification-window` 是这条协议的操作化：判定顺序固定为**外部 → 数据库 → 工作区**（外部副作用撤不回，先判它）；数据库维度 = `first_write_at` 读真库 + 窗口起止的行数对比，两者任一为脏即 `blocked:database_written`，起止读数不可比或读失败即 `blocked:unknown`。它判的是窗口那一段；切换与窗口之间的写入仍由导入基线（`db verify --target`）发现——三方判据各自覆盖不同的时间段，**不得**互相顶替。

**写入二分**（哪些写入不关闭回退窗口）：只有可丢弃 / 可重放的维护写入在白名单里——`ensureSchema`、`db migrate`、`db import`、`db switch` 的切换行与**同一次受控旧记忆导入**（其行数经「导入前 + 导入条数 = 导入后实测」对账后才成为 `memory_entries` 基线，见上表）、启动期 `BackfillEnvSources` 补记、校验期任务 `enabled` 的临时开关（先记原值、验后恢复）。**不可忽略的管理变更**（Key 发放 / 撤销 / 轮换、配额调整、凭据与资源发布 / 回滚 / 删除、Agent 生命周期、通知渠道、执行策略与网络例外）集合非空即**不得**自动回退。来源不明或无法归类 → 按管理变更处理，不得当维护写入放行。

### 5.8 启动失败排障（PostgreSQL 与恢复态）

**启动失败一律拒绝，没有降级分支**：不重试成循环、不回退 SQLite、不自动建扩展、不自动补写主密钥记录。日志给出原因；能打开存储时 `basalt status` 报就绪状态与 schema 版本。

| 症状（日志 / 命令输出） | 原因 | 处置 |
| --- | --- | --- |
| 未发现配置 / 配置无效 / `database.dsn 未配置` | 文件缺失、`${ENV_VAR}` 占位未解析、变量为空 | 补 `basalt.yaml` 与进程环境；**不得**把 DSN 明文写进配置或命令行 |
| `连接 PostgreSQL 失败` | 服务未起、地址 / 端口 / 认证错、防火墙 | 按 §1.2 先用 `psql` 单独验一次连接；DSN 只经 `EnvironmentFile` 注入 |
| 连接串无法解析 | 既不是 `postgres://` URL 也不是 `keyword=value` | 修正格式；应用会强制 `TimeZone=UTC` 与 `target_session_attrs=read-write` |
| `扩展 vector 缺失` / `缺少所需能力 hnsw.iterative_scan` / `扩展 pg_trgm 缺失` | 扩展没装或版本太旧 | 部署者在目标库 `CREATE EXTENSION`（`vector` 升到 0.8.x）后重启；应用**不**自动创建 |
| `库内有表但无 schema_version 表` / `schema_version 记录…非法` | 指向了别的库，或库被半建 | 核对 DSN 指向的库名；按 §4.4 恢复正确备份 |
| `库版本 N 高于二进制支持的 M` | 用旧二进制打开了新库 | 换回匹配版本的二进制；**不得**手改 `schema_version` |
| 启动 WARN + 管理台 `state=recovery_required` | 主密钥记录缺失 / 损坏 / 不符 / 来源不可证明 / 不可读 | 按 §5.8.1 的原因码处置；这不是"启动失败"——服务会继续起，但托管凭据写入 fail-closed |
| 管理认证模式非法 | 配置里写了 `token` / `local_trust` 以外的模式，或 `token` 解析失败 | 显式 `token`（`${ENV_VAR}`），或全部监听地址为回环时用 `local_trust`；配置非法即拒绝启动——**没有**"关闭认证"这种模式 |
| `服务启动缺少配置项: callers.ingress.…` / `服务启动要求下列配置项大于零: …` | 六项入口限流缺失或非正（US-16 起 `serve` 必填） | 按 §5.10 补齐三类 `qps` / `burst` 后重启；**不预填缺省**——这是预期 fail-closed，不是回归 |

**诊断读取入口**：`GET /api/v1/diagnostics` 只对管理认证开放（调用方 API Key 一律拒），必须带至少一个主锚（`run_id` / `request_id` / `operation_id` / `ingest_job_id` / `session_id`）；`task_id` 只是限定符，不能单独定位（单给任务号等于全表扫，服务端 400）。返回里 `missing`（没有记录）与 `sampled_out`（有记录但被采样丢弃）是两回事——**不得**把 `sampled_out` 当成"没发生"。

**能力覆盖（US-16A 收口）**：真实生产端覆盖 `retrieval` / `llm_call` / `tool_call` / `permission_decision` / `quota_decision` / `ingest_stage`。会话查询经 `runs.session_id` 解析已有Run，不要求诊断事件直接存会话字段；`operation_id` 诊断关联当前仍无生产端。启动期索引切换没有合法关联锚点时不发 `index_switch`，不以代际号补造操作ID；适配器存在不等于正式装配已覆盖。装配与embedding使用各自事实源，不重复复制成同名诊断事件。空结果只表示该查询没有记录；OBS的管理操作/调度/摄取时间线另从真实账本关联，不能与旧事件覆盖混为一谈。

数据库状态与 schema 版本看 `basalt status` 与 `/api/v1/system/overview` 的 `database.status` / `database.schema_version`（`/api/v1/info` **不含**数据库字段）。

#### 5.8.1 `recovery_required` 的五个原因码

`GET /api/v1/instance/secrets` 与 `basalt secrets status` 给出 `state` 与 `recovery_reason`。恢复态不阻断整体启动，但托管凭据写入 fail-closed，`POST /api/v1/instance/secrets/init` 一律 409 且零写入。

| 原因码 | 含义 | 处置 | 做不到时的结局 |
| --- | --- | --- | --- |
| `missing` | 配置指向的密钥文件不存在 | 从 §4.4 的受控备份把**同一把**密钥放回原路径（目录 0700 / 文件 0600） | 密钥确实丢失 → 现有托管凭据全部不可恢复 |
| `corrupt` | 文件不是合法的 `{"version","key_id","key"}` | 同上，换回正确文件 | 同上 |
| `key_id_mismatch` | 文件里的 `key_id` 与库内记录不符 | 找回落盘时的那把密钥；**不得**改记录迁就文件 | 同上 |
| `unprovable_provenance` | 库里有托管密文但没有 `instance_secrets` 记录（升级 / 崩溃 / 误删），或记录与密钥来源无法证明 | v6 且未切换：按 §6.4 用**产于首次写记录之后**的可信备份 `secrets recover-record`；schema 3～5：按 §6.5 `secrets adopt-legacy` | 保持恢复态；只有经人工评审才可用 `secrets revoke-managed --confirm` 显式退役（§6.2）——那不是恢复 |
| `unavailable` | 密钥文件读不了（权限 / DACL / 句柄） | 按 §6.1 修文件与父目录权限；校验结果不缓存，修好即可用 | 持续失败 = 托管凭据不可用 |

**不得**做的三件事：重新生成主密钥"修复"、改写或删除密文、把托管凭据静默降级成 env。这三件都让原密文永久不可解，也不会让记录变得可证明。

### 5.9 升级后的 Run 任务次数配额（`tasks_per_period`，US-16 起）

**每次逻辑 Run 计 1 个任务次数**（`ClassRun`，幂等键 `run:<run_id>:admit`；在全部准入通过之后、`StartRun` 之前预留并立即结算）。这与 `qps` / `burst` 两列的零值语义（**0 = 不覆盖**，沿用 `callers.ingress.*` 类别预算；契约 `schema.pg.sql` 注释里的「0 = 不限」即指"不额外限制"）**不是一回事**，升级时最容易踩：

| 情形 | 结果 |
| --- | --- |
| 调用方没有 `caller_limits` 行 | 新 Run 被拒（429 `QUOTA_EXHAUSTED`；响应体是固定码文案，内部原因「没有限额行」只用于排障，不回显） |
| `tasks_per_period = 0`（DDL 缺省） | 同样被拒——**零表示不放行**，不是不限量 |
| `tasks_per_period > 0` | 每个逻辑 Run 扣 1；按 `period` / `timezone` 的本地 00:00 重置 |
| `local-admin`（内置管理员），`callers.local_admin.unlimited=true`（缺省） | 不受限，不产生预留行 |
| `local-admin`，`callers.local_admin.unlimited=false` | **走正常预留**：`seedBuiltinCaller` 只建 `callers` 行、不建 `caller_limits` 行 → 内置管理员的 Run 与消耗调用一律 429 `QUOTA_EXHAUSTED`。要关掉不限量，必须同时给 `local-admin` 写一行 `caller_limits` 并填正数 |

**升级动作**：本版本之前 `tasks_per_period` 没有运行期消费方，全 0 的行也无害；升级后**必须**为每个需要发起 Run 的调用方显式写入正数，否则它的 Run 会被一律拒绝。只有「永远不该跑」的调用方可以保持 0。

限额只在**创建调用方**时随 `POST /api/v1/callers` 的 `limits` 块写入；契约目前**没有**更新限额的管理端点，已存在调用方要调整只能由运维直接改库（`UPDATE caller_limits SET tasks_per_period = … WHERE caller_id = …`）。任务次数在每次准入时现读，改库后无需重启。

**失败与结算口径**：预留失败即拒绝，Run 不启动；结算失败（提交结果未知）**不释放**——预留继续占额度、Run 照常执行，由管理面 `reconcile` 收口（见 §9）。同一 `run_id` 重放只扣一次。

### 5.10 入口限流六项（`callers.ingress.*`，US-16 起 `serve` 必填）

三类业务入口各有一对 `qps` / `burst`，**六项在服务启动前校验**（`cli.ValidateServeIngress`）：

```text
callers:
  ingress:
    run:    { qps: <N>, burst: <M> }   # Run / Chat / 会话写
    ingest: { qps: <N>, burst: <M> }   # 上传与摄取触发
    read:   { qps: <N>, burst: <M> }   # 历史 / 来源 / 列表等读端点
```

| 情形 | 行为 |
| --- | --- |
| 六项齐全且都 > 0 | 正常启动；预算按**调用方**分别计量，超预算返回 429 `CALLER_RATE_LIMITED` |
| 任一键缺失 | **拒绝启动**，日志点名缺项（`服务启动缺少配置项: callers.ingress.run.qps, …`） |
| 任一键 ≤ 0 | **拒绝启动**，日志点名非法项（`服务启动要求下列配置项大于零: …`） |
| 无配置文件 / `callers` 段整体缺失 | **拒绝启动**（提示需要配置文件及入口限流项） |
| 维护命令（`basalt status` / `db export` / `db import` / `db verify` / `db switch` / `secrets …`） | **不校验**这六项——旧配置与切换期实例仍能用维护命令 |

**为什么必填而不是给缺省**：缺省值等于替部署者决定容量上限，而这个上限必须来自该实例自己的压测（D-30 尚未冻结）。因此**不预填任何数字**：少了就拒绝启动，把一个"静默限错"的问题变成一个当场可见的启动失败。

**首次启动 / 升级动作**：从旧版本配置升级时，`serve` 会因为缺这六项直接起不来——这是预期的 fail-closed，不是回归。按本实例容量实测补齐三类值后重启即可；`basalt.yaml.example` 的 `callers` 段有同一段模板（样例**不预填数字**）。

**排障**：先看启动日志点名的键，补上再重启。若六项都在仍出现 429 `CALLER_RATE_LIMITED`，那是**运行期预算耗尽**而不是配置缺失——看 `Retry-After`、确认调用方身份（管理 token 不进任何桶），再决定是调高 `qps` / `burst` 还是压调用方速率。不要把它和 `AUTH_RATE_LIMITED`（管理面认证失败限速）或 `TOO_MANY_RUNS`（实例并发准入）混为一谈，见 §2.1.3。

---

<a id="us16a-observability"></a>

### 5.11 US-16A：观测、导出与清理

本节描述 US-16A 收口实现；HTTP 字面量见 [OpenAPI](contracts/openapi.yaml)。默认开启管理读取和内置告警，**导出与清理默认关闭**；30 天保留期自 2026-11-02 补充裁决起为默认值，清理仍需显式启用，且不是生产容量或保留承诺。示例配置见仓库根 `basalt.yaml.example` 的 `observability` 段，不要把示例当作已批准的生产策略。

#### 升级影响清单（US-16A，八项）

| 项 | 变化与动作 |
| --- | --- |
| PostgreSQL版本 | 运行期5→7，只追加`0006_observability.sql`（六张OBS表、索引和可空字段）与`0007_history_retention.sql`（关联历史保留账本）。`schema.pg.sql`仍是v1冻结基线，`0001`–`0006`不改；旧版本不能直接打开更高版本库，回退须依既有备份/切换流程，不删迁移记录冒充降级。 |
| 启动配置 | 新`observability`段严格校验、未知键拒绝；只在启动读取，无热加载。显式0/越界不回落默认；导出关闭时endpoint可空，headers可省略或引用已存在的空ENV。 |
| 管理读面 | 七个`GET /api/v1/observability/{overview,timeline,context,metrics,alerts,export,cleanup}`与`/console/observability`；admin token/local_trust沿用，caller key一律403。没有OBS写、手动重放或修复API。旧`/diagnostics`的锚点与排序不变。 |
| 关联与旧数据 | 真实入口request_id贯穿Run；工具事件含实际tool_call_id；恢复增加recovered_at，不补造结束时间。新增检索摘要可空；历史NULL/缺键按未记录，chunk_invalidated与三种候选决定同步契约；v0事件不升版本，未知旧值仍可读。 |
| 可选外发 | 默认关闭；启用前配置显式http/https Collector目标与ENV headers。sender固定直连、不读HTTP_PROXY/HTTPS_PROXY、无内部重试/重定向；按三信号独立批次回账本。不得把接收副本当本地审计。 |
| 指标/告警 | 进程计数重启归零；数据库窗口不相加。五内置规则开启，三条统计候选关闭、未校准；本版没有逐规则阈值YAML/DSL或外部通知。unknown不显示正常或恢复。 |
| 可选清理 | 默认关闭；启用前确认 OBS 四策略、关联历史与文件保留期和备份。装配保留须不小于诊断/已配置历史窗口；导出启用时仍需覆盖+当前hash行级sent证明，未知/未决保留。删除与成功账本同事务，无逐条墓碑。 |
| 生命周期/回退 | 一个serve后台循环只处理OBS，不启动Run/工具/业务恢复；停机等待其退出再关DB。关闭export/cleanup后重启可停止对应后台动作，但不会撤回已发副本、恢复已删证据或降级schema。审计fail-closed保持。 |

#### 启动参数与凭据

- `enabled=true`、`alerts.enabled=true`；`export.enabled=false`、`retention.cleanup_enabled=false`。只读查询不会评估告警、启动发送或执行清理。
- 导出endpoint为空且disabled时可启动；启用时必须设置显式http/https URL，不接受userinfo/query/fragment。示例把endpoint留空，ENV引用放在注释，避免未启用者也被缺失ENV阻断。
- headers需要配置时只经 `${OTEL_EXPORTER_OTLP_HEADERS}` 注入，格式为逗号分隔的 `name=value`；需要空格时用百分号转义。headers键可省略；显式引用的变量必须存在，值允许为空，缺失引用仍拒绝。header名称和值有界，拒绝重复、控制字符与传输保留头；日志/页面不输出值，页面只显示已配置/未配置。endpoint变量可用 `OTEL_EXPORTER_OTLP_ENDPOINT`，不把值写进证据日志。
- batch默认256、timeout10s、最多10次/24h、lease5m、重叠5m、安全滞后2m、初始回填与对账各24h、扫描页500、指标快照1m；lease必须大于发送timeout，初始回填必须大于安全滞后。清理每批1000、每轮每表最多20批、间隔1h，默认不运行。范围外启动失败，错误只点字段/类别。
- worker按既有间隔轮询；退避记录1s–5m并不承诺在该秒立刻重试，实际还受轮询频率约束。读取/告警/清理/发送使用有界上下文；诊断或OBS失败不使Run变为成功，也不把业务审计失败放行。

#### 查询与故障判断

总览/任务组默认最近24h、最多100组，超出组合汇入“其他”；组内最多20条异常Run链接，不选代表Run。时间线必须恰有一个六类锚点，最多1000条；过滤前编号、过滤后保留编号，不把展示先后当因果。上下文按“记录事实/需核对输出/证据不足”逐项解释，只展示允许候选；混合过滤计数不是文档数或当前匹配行数。空检索不补造no_answer。

指标本地窗口5m/15m/1h/24h；JSON中的window单位为纳秒。进程范围需结合process_started_at；数据库窗口为Gauge。无样本、未记录、来源不支持、读取失败和真实0分别呈现，未知告警的占位值不作为真实0。重启前的进程失败证据不可恢复，规则会显示unknown；须查既有服务日志，不能靠诊断事件替代审计。告警只在管理台查看，不发送外部通知。

| 现象 | 解释与处置 |
| --- | --- |
| 401/403 | 用管理凭据核对既有Host/Origin姿态；caller key不能访问OBS。拒绝响应不提供资源存在性或身份。 |
| 503 NOT_CONFIGURED | 观测功能关闭；不是成功空集。缺装配或读取失败为500，不能解释为“暂无数据”。 |
| sampled_out / not_recorded | 前者可能因采样无行，后者未记录；均不等价于系统正常。 |
| query_failed / unknown | 来源查询或评估/状态存储失败；检查对应服务日志和DB可用性，不把状态重置inactive。 |
| 发生过清理 | 只证明某数据时间窗发生过删除；当前某行是否因此缺失仍无法确认。管理清理历史按执行时间查询，数据窗口另列。 |
| 无发送批次 | 尚无已记录尝试；不显示成0条发送批次。accepted/rejected为NULL时未确认，不能补0；单位区分跨度/日志条/数据点。 |
| partial_unconfirmed | 全批逐项结果未确认。仅`partial_rejected`且响应计数合法时能证明有接收端拒绝，并保留合法批次计数；`protocol_invalid`或`partial_with_transport_error`不能证明实际拒绝，accepted/rejected保持NULL。这些分支均不自动重试或拆成逐项永久失败；本版没有手动ack/重放API。 |
| unknown尝试 | 可能发出，也可能在发出前中断；此前结果未知，可能重复。租约后重发相同稳定ID，不推断上次哪些成功。 |
| pending/retry积压 | 看当前条目数量、年龄、来源覆盖与固定错误分类；不要凭一次发送失败重跑业务。达到次数/年龄预算后的永久失败是导出状态，不改变Run/audit。 |
| 重试技术详情 | `retry_attempts`是当前retry项的再次认领次数合计（每项扣除首次认领），不是已发出网络请求数；无当前retry项为NULL。`next_retry_at`是最早计划下界，实际尝试还受工作循环调度约束。`last_http_status`仅显示已记录响应码，未收到响应为NULL。 |
| scan_not_covered / out_of_coverage | 无完整扫描覆盖或早于首次起点，证据保留。初始回填只决定首次起点，改大后不会自动倒退已有起点；本版没有全历史回填API。 |
| unconfirmed_export / unexported_terminal | 当前投影无已确认sent或有未决/失败项，保留源行。增大reconcile_horizon并重启只能对现有覆盖区间内可对账的晚提交补扫；不保证全历史至少一次。 |
| 跳过且计数0 | 可能本轮未枚举候选，不等于没有符合条件的记录；查看固定跳过原因。 |

启动无锚点的index_switch、真实外部模型和可选外部后端查询未由这些本地读数证明。官方Collector接收/字段映射须使用产品sender独立验收；Collector收到不等于可选后端可查询。

#### 合成容量/清理演练（2026-11-02）

可复跑命令（测试DSN只经环境；`portTestDB`为本用例创建并清理独占schema，不触碰现有实例数据）：

```powershell
. .\dev-env\scripts\Enter-DevEnv.ps1
go test -race -tags integration ./internal/storage/postgres -run '^TestObservabilityStore_CapacityDrill$' -count=1 -v
```

本次为Windows/amd64、Go1.27.3（含race开销）、本地PostgreSQL17.11（Debian/x86_64），单串行调用、无HTTP/外部Collector/并发业务写入。数据由SQL造1000个Run、1000条LLM审计、20000条诊断、20000条历史形状装配记录；ID紧凑、正文为空，每锚点各20条可丢弃记录。合成行集中在2026-10-24 UTC；20000“日行数”是夹具分布，**不是生产增长率**。

报告UTC为`2026-11-01T16:42:57.304Z`；原始输出`.loop/obs-capacity-final.jsonl`、提取JSON`.loop/obs-capacity-measurement.json`（SHA256 `b07e5cb22b4a4be4aff67de03a29ae0ad435739b8a4c33923bbb08b553bd87b5`）。测试源SHA256 `850ffcb1bb232127334987e0639f9cc2360b4a102c14d3d52dc6b54bfa1f3047`；复跑读数会变化，不覆盖此轮记录。

| 本次测量 | 清理前 | 清理后 |
| --- | --- | --- |
| 时间线p50/p95（20次，ms） | 4.6254 / 5.5451 | 4.7882 / 5.7480 |
| 上下文p50/p95（20次，ms） | 2.8210 / 3.2180 | 3.4164 / 3.7908 |
| 指标快照p50/p95（10次，ms） | 18.2178 / 25.2458 | 13.4869 / 16.3976 |
| 诊断表总大小/索引（byte） | 6520832 / 3661824 | 3686400 / 3661824 |
| 装配表总大小/索引（byte） | 6938624 / 3276800 | 6946816 / 3276800 |
| n_dead_tup（诊断/装配，异步统计） | 0 / 0 | 0 / 16000 |

清理压测每批1000、每表最多20批、导出关闭、保留7天（压测参数，非默认30天），实际40个提交批次删除40000条，耗时11284.4139ms；Run保留1000条，证据查询正确出现窗口缺口。独立 `EXPLAIN (ANALYZE, BUFFERS)` 删除1000诊断行在事务内执行后回滚，Planning0.152ms / Execution3.118ms；完整计划含buffers在JSON中。它没有做来源保护/逐批账本，不能拿这3.118ms替代完整清理耗时。

两表普通`VACUUM (ANALYZE)`合计19.9109ms后，报告dead tuples均0；总大小/索引分别为诊断3760128/3735552 byte、装配3325952/3301376 byte。统计可滞后或受autovacuum影响；普通VACUUM不保证所有文件立即缩小，索引/FSM/VM元数据也会占空间。未对生产库执行VACUUM，更未执行VACUUM FULL。

这些延迟包括端口、投影及样本一致性检查；清理后缺口查询增加账本读取，所以不能声称所有查询都变快。未测量真实增长率、进程峰值RSS、外部导出稳态吞吐、导出开启时行级保护成本、大正文/高并发/目标机器与磁盘的表现；不得外推为生产容量或SLA。启用策略前应在目标机用代表性数据独立演练，观察关系/索引大小、实际提交删除数、锁等待、积压与异步统计；未确认导出记录宁可保留增长，也不把派生清理当业务恢复。

#### 管理台排障顺序与读数

“运行排障”按一次运行组织结果、耗时、上下文和异常定位；“诊断明细”逐条展示底层事件，供进一步核对。两者使用关联记录，不是两套独立的失败结论。从“运行排障”总览选择异常任务，再打开失败运行。运行诊断首先显示终止原因、总耗时、轮次和工具调用数；“异常定位”列出途中失败，点击可定位到对应记录。途中工具失败不一定是整次运行终止的原因。Agent 会话详情提供“运行排障”入口，先列出当前加载的关联运行，再打开其中一次运行；截断结果不能代表完整会话。

历史筛选提供“最近1天”（默认）、“最近7天”、“最近30天”和“自定义”，显示实际生效的本地起止时间，自定义最多30天；传输仍使用 RFC3339。按具体运行、会话或操作打开的关联查询默认使用该关联的全部时间，仍受最多1000条及来源读取上限约束，避免旧记录被最近1天排除。指标窗口、后台采集和数据保留策略不随这项历史筛选改变。

“关键执行”展示业务和审计记录，“仅异常”聚焦失败、拒绝或未知结果，“全部记录”补充瞬时判定、诊断事件和装配快照。瞬时事件没有持续时间；LLM 的独立结束时间缺失时保持未知，只显示已记录的耗时；工具结束时间标明记录内派生。时间异常与未记录分开显示，展示编号可能随数据变化重排。

上下文预算超限仅对核心生产者的固定错误格式提取安全分类和数字，不公开任意原始错误。调度行引用关联 Run 的原因时明确标注来源；这项界面解释不改变调度导出事实或业务记录。每条时间线标明证据档位；仅通过真实审计引用关联的冲突才提示以审计为准。

调度执行是外层起止跨度，包含关联 Agent 运行，两者重叠，不能相加。单次运行列出有记录的模型及工具耗时合计、较慢的调用；这些调用也可能重叠，不能与总耗时相加或把差额直接认定为排队、网络等待。上下文超限按“正文估算 + 预留 > 总预算”解释，正文单独小于总预算仍可能超限。

执行摘要分列“本次总时长”“模型调用累计耗时”“模型调用次数”与“工具调用次数”。存在关联调度记录时，总时长采用调度起止，并另列其中的Agent运行时长；未返回调度记录时，明确按Agent起止计时。模型耗时是请求发出至返回的调用耗时，包含等待和网络传输，不能解释为模型服务端的纯处理时间。模型次数来自实际加载的调用审计，包含失败和会话压缩，诊断事件不重复计次；耗时覆盖数量、截断及未知耗时明确说明，未记录不当成零，已记录零耗时仍显示零。

新运行增加“阶段耗时”，默认显示检索全过程、每轮上下文准备、模型首响应等待和响应接收；检索与上下文子阶段展开查看。检索按实际执行情况记录查询向量化、范围解析、关键词/向量查询、融合及预算筛选、命中内容精确读取；上下文准备记录轮首知识权限复核、压缩检查与处理、提示词装配。父阶段包含子阶段，不能重复累计；未执行的路径不制造耗时。合法的范围检查拒绝标为跳过，不冒充运行失败；执行失败或取消保留对应阶段状态。

模型继续使用非流式JSON接口：首响应等待测量从发出HTTP请求到收到响应首字节，**不是首个token等待**；响应接收测量读取并关闭响应体，不等于服务端生成时间。捕获回调不写数据库，调用审计耗时固定后再写阶段证据。阶段计时保留实际起止与单调时钟耗时，作为可采样、可丢弃的诊断记录存储；历史运行不补算，旧瞬时事件不改成跨度。按模型用途和工具可展开次数、累计、平均、最长及记录覆盖；平均只使用有耗时的记录，统计仅覆盖当前返回的记录。

知识检索的访问范围筛选是普通筛选统计，不列为运行异常。历史混合过滤计数既不是被过滤文档数，也不是本次命中数；原始计数及其局限保留在技术详情，不补造逐项原因。管理操作列表的“查看”在当前画面打开详情对话框，显示加载、错误或详情；可用关闭按钮、Escape 或点击背景关闭，再返回原行。

指标的对象、状态、用途、单位及 P50/P95/P99 直接展示；分布桶和其余细分读数可展开查看，不将不同标签、重叠窗口或累计桶相加。总览只展示概况与异常入口，不默认铺开指标明细。时间筛选使用浏览器本地时间，查询仍按原 API 的 RFC3339 时间传输。

导出或清理未启用时，首页说明当前行为与未启用的策略，明细和历史仍可展开。这些页面只读，不会因查看或刷新而启用导出、删除数据或重放业务。

## 6. 凭证

凭证有两种模式，由资源的 `credential_mode` **显式**声明（服务端不按值的形态推断）：

| 模式 | 定义里存什么 | 真值在哪 | 适用 |
| --- | --- | --- | --- |
| `env` | 精确 `${ENV_VAR}` 占位（如 `${DEEPSEEK_API_KEY}`） | 部署者注入进程环境，使用时解析 | 默认；纯 env 实例**不需要**主密钥 |
| `managed` | 不存明文（通知渠道只存解析出的 host） | `credential_versions` 的 AES-256-GCM 密文；主密钥在工作区之外 | 管理台直接录入 Key / Webhook 地址，无需重启 |

| 位置 | 规则 |
| --- | --- |
| `basalt.yaml` 的 `providers[].api_key` | 只允许 `${ENV_VAR}` 占位；启动按当前发布版本解析，未解析（`${` 残留）或为空 → **拒绝启动**并点名配置项 |
| `basalt.yaml` 的 `http.admin_auth.token` | 只允许 `${ENV_VAR}` 占位（如 `${BASALT_ADMIN_TOKEN}`）；写了就必须解析出非空值——占位变量缺失、解析为空或残留 `${` → **拒绝启动**，**不**改读密码文件。`token` 键省略 → 读实例私有目录的 `admin-token`（新建实例首次 `serve` 自动生成）；三处都没有 → **拒绝启动**，不回落 `local_trust`（后者只在全部监听地址为回环时允许） |
| 通知渠道（库表 `notify_channels.url`） | `env` 模式**只存 `${ENV_VAR}` 引用**；`managed` 模式 `url` 为空、密文在 `credential_versions`。发送时解析 / 解密，失败给点名原因的可读错误 |
| MCP server 的 env / header 槽位（`mcp_servers[].env`） | **逐键**显式声明来源：`env` 读进程环境，`managed` 存密文（`credentials.binding_key` = 槽位名，唯一键为资源 + 槽位三元组）。保存 / 发布 / 回滚时逐槽取值一次，任一槽取不到 → 503 + `operation_id`，文件 / 版本 / 指针零变化；幂等发布（无变更）不再校验。运行期 `managed` 取不到即失败，**不**回落 env；撤销清空密文但保留声明，重新录入生成新版本 |
| 管理台创建的模型连接 / 通知渠道 | 创建时未声明 `credential_mode` 只按 env 规则校验（普通值 → 400 `CREDENTIAL_MODE_REQUIRED`，不推断为 managed）。`managed` 的 Key / 地址是 **write-only**：响应只回 `credential_ref`、`credential_version`、`last4` 与 host；`credential_ref` 由服务端生成，请求体出现 → 400 `CREDENTIAL_REF_READ_ONLY` |
| 作者模型（US-12 的生成能力） | 与 provider 同规则（env / managed 均可，推荐 env） |

### 6.1 托管凭据与主密钥

- **生成**：由 `basalt secrets init`（或管理台「实例加密」的初始化按钮）生成 32 字节主密钥，写入 `secrets.master_key_file` 指向的**工作区之外**文件（如 `/etc/basalt/master.key`）；文件为 JSON `{"version":1,"key_id":"k_…","key":"<base64 32 字节>"}`，`key_id` 由初始化流程生成、随密文一起记录，**不是**配置项。新建实例（库与 `instance.json` 均为本次启动新建）在 `secrets.auto_init` 缺省 `true` 时由首次 `serve` 自动完成初始化；**已有实例绝不自动生成**——密钥缺失 / 损坏 / `key_id` 不符 / 来源不可证明一律进恢复态，等待显式修复。`auto_init: false` 显式声明纯 env 实例（此时可省略路径）。**不得**把主密钥放进工作区、仓库、容器镜像或共享配置。
- **权限**：Linux `0600` 且属主为服务账户（`chown basalt:basalt`）；Windows 用 `icacls` 把主密钥**文件与父目录**都只授予服务账户、SYSTEM 与管理员组（`icacls <path> /inheritance:r /grant:r "<服务账户>:(R)" "NT AUTHORITY\\SYSTEM:(R)" "BUILTIN\\Administrators:(R)"`；目录带 `(OI)(CI)` 继承标志）。启动与每次使用前都会校验路径、格式与平台权限：Linux 按权限位（目录 0700 / 文件 0600），Windows 读取文件与父目录的实际 DACL——只允许当前进程用户、SYSTEM 与 Administrators 三类受托人，其他用户 / 组（含 `Users`、`Authenticated Users`、AppContainer 包 SID）、缺失或空的 DACL、无法读取的安全描述符一律拒绝并返回 `CREDENTIAL_UNAVAILABLE`。权限在校验之后被放宽，下一次读取会拒绝（不缓存校验结果）。`os.Chmod` 在 Windows 只切只读位，**不得**用它代替 `icacls`。
- **按需加载**：主密钥**不常驻内存**——每次使用托管凭据时读取并校验（文件缺失 / 权限不符 / 长度不是 32 字节 / `key_id` 不匹配 / 解密失败），失败只影响本次调用（503 `CREDENTIAL_UNAVAILABLE`），**不**自动回退到 env 或旧版本。
- **启动依赖**：先判定当前发布且被绑定的**托管**模型连接；只有它需要解密时才读取主密钥。纯 env 实例（含只配 env 的模型连接）**不因缺主密钥而启动失败**；通知渠道的托管凭据也不阻断启动，失败发生在发送时。
- **备份与恢复**：主密钥与数据库**分开备份、分开存放**；恢复时必须是同一 `key_id` 与原密钥，否则所有托管凭据不可读。
- **丢失的后果**：主密钥丢失 = 现有托管凭据**全部不可恢复**（只能逐个重新录入，生成新凭据版本）；不得尝试改写密文或猜测密钥，也不得把托管凭据静默降级为 env。
- **撤销与替换**：撤销（`POST /resources/{kind}/{name}/credential/revoke`，MCP 必须带 `binding_key` 指定槽位）**不需要读取主密钥**——它只把该凭据身份的全部版本置 `revoked` 并清空密文；重新录入生成新的 `version_no`，旧版本永不复活。Key 替换 = 新凭据版本，不动定义内容。
- **混合实例**：同一实例里 env 与 managed 可以并存；启动依赖只由**当前绑定的托管模型连接**决定，不能用「存在凭据行」推断。
- **就绪视图（14D）**：`GET /api/v1/instance/secrets` 只读返回 `state`（`uninitialized` / `ready` / `recovery_required`）、`key_id`、`initialized_at`、`key_file`、`recovery_reason`（`missing` / `corrupt` / `key_id_mismatch` / `unprovable_provenance` / `unavailable`）与 `credential_summary`（`managed` / `managed_revoked` / `env` / `missing` 四档计数，MCP 槽位、模型连接、通知渠道合并统计）；不含密钥材料。`POST /api/v1/instance/secrets/init`（`{"confirm":true}`）只在「未初始化且无密文」时生成，`ready`、恢复态与纯 env 实例一律 **409 且零写入**——先按 `recovery_reason` 处理文件，不要指望重复初始化。

红线段（day-one 硬约束，**不得**违反）：

- 渠道地址（路径与查询串就是 token）**不得**出现在响应 / 日志 / 错误文案——对外只回 **scheme + host**
- 管理 admin token **不得**出现在响应 / 日志 / 错误文案 / 管理操作审计——认证成功后只记 `auth_method` 与 `actor_subject`（`token:<指纹>`），不记原值
- 沙箱越界文案里**嵌着原始目标**（可能就是凭证），**不得**原样透传给响应或日志
- 页面编辑框**不回填**已存地址，故「`url` 留空 = 保留原值」
- 托管凭据的明文（Key / 完整 URL）与主密钥**不得**出现在响应、日志、审计 `detail_json`、诊断导出或备份脚本的终端输出；密文与 `last4` 可以出现
- 主密钥文件**不得**放在工作区内（备份工作区时不得连带打包）；`basalt.yaml` 只写路径，不写密钥内容
- 环境变量注入方式：systemd `EnvironmentFile=`（文件权限 `600`）或容器 `--env-file`；**不得**走命令行参数（`ps` 可见）或提交进仓库的文件

复核（可复制）：

```bash
grep -rnE "sk-|(api_key|token): [^$]" /opt/basalt/basalt.yaml   # 期望：零命中（占位符以 ${ 开头）
sqlite3 /opt/basalt/.basalt/basalt.db \
  "select name, type, credential_mode, length(url), url_host from notify_channels;"  # env 行 url 应是 ${...}；managed 行 url 为空
sqlite3 /opt/basalt/.basalt/basalt.db \
  "select owner_kind, owner_name, state, current_version, revocation_epoch from credentials;"  # 只查元数据
ls -l /etc/basalt/master.key    # 期望：600，属主为服务账户；路径不在工作区内
```

前两条只查 `name` / `type` / 模式 / 长度 / host——**不得**把 `url` 原值或密文打印出来（一次回显就把它写进了终端历史与截图）。

### 6.2 `basalt secrets` 命令组

六条子命令共用一条纪律：**不自动轮换**、**只回公开标识**（`key_id` 与指纹，绝不回密钥、密文或渠道地址）、**撤销不解密**。其中 `recover-record` 与 `adopt-legacy` 是**离线**命令（不装配运行期、零外呼），其余四条属重命令。纯 env 部署可以完全跳过本节。

| 命令 | 作用 | 拒绝 / 失败条件 |
| --- | --- | --- |
| `basalt secrets init` | 生成 32 字节主密钥写入 `secrets.master_key_file`，Linux 目录 `0700` / 文件 `0600`；Windows 上文件继承父目录 DACL（**部署前**先用 `icacls` 把父目录收成最小权限，否则自检按 §6.1 的 DACL 规则拒绝并删除刚建的文件）。新建实例首次 `serve` 已自动初始化（`auto_init` 缺省 `true`），手动 init 只用于存量实例或关掉自动初始化的部署 | 密钥文件**已存在**（绝不覆盖——覆盖即让现有密文永久不可解；「有文件、缺初始化记录」由下次 `serve` 自动 adopt 补记，**不**生成第二把）；当前仍有托管凭据依赖（拒绝制造孤儿密文）。两种情况都**零写入** |
| `basalt secrets status` | 只读报告：是否就绪、`key_id`、指纹、托管依赖计数（模型连接 / 通知渠道各几个） | 未初始化只影响文案，**不是**命令失败——这是部署状态，不是故障 |
| `basalt secrets verify` | 逐条校验当前 `active` 的托管凭据能否在当前主密钥下解密、目标绑定是否仍与当前定义一致；只回检查与失败计数，明细在 WARN 日志（不含材料） | 失败条数 **> 0** 即非零退出（脚本可判卷）；超过 0 时先查主密钥路径 / 权限 / `key_id`，再决定是否 `revoke-managed` |
| `basalt secrets revoke-managed --confirm` | 清空全部托管凭据密文、推进撤销纪元、为每条写管理操作行；**不**读取主密钥——服务停止、主密钥丢失时的销毁性恢复 | 缺 `--confirm` 时**零调用零副作用**；执行后旧版本**永不复活**，只能重新录入新凭据版本 |
| `basalt secrets recover-record --backup <dir> --expect-backup-sha256 <hex> --confirm` | 从**产于首次写记录之后**的可信备份恢复 `master_key` 记录（名册缺失时逐字复制备份名册）；离线、零外呼 | 状态不是恢复态（`state_not_recovery`）、已有记录（`record_already_exists`）、锚点对不上（`backup_unverified`）等一律零写入；流程见 §6.4 |
| `basalt secrets adopt-legacy --workspace <copy> --backup <dir> --expect-backup-sha256 <hex> (--source <dir> XOR --attest-source-unchanged <ref>) --confirm` | 在**隔离副本**上完成 schema 3～5 → v6 的受控升级，写 1 条 `record_origin=attested_legacy` 记录；离线、零外呼 | 只接受 schema 3～5 且备份中无 `instance_secrets` 表的存量（v6 的记录丢失走 `recover-record`）；源端漂移 / 锚点不足 / 配置越界修改一律拒绝；流程见 §6.5 |

典型时序：全新实例首次 `serve` 自动初始化（也可显式 `secrets init`）→ 管理台录入托管凭据 → 日常 `secrets status`；升级或怀疑密钥问题时 `secrets verify`；记录丢失进恢复态时按 §6.4 / §6.5 从可信备份恢复，**不得**指望重新初始化；只有人工评审确认原密文不可恢复，才用 `secrets revoke-managed --confirm` 显式退役，之后逐个资源重新录入。**不得**把 `revoke-managed` 当常规轮换手段，也**不得**把"重新生成主密钥"当成修复。

### 6.3 一次性初始化导入（仓库外模板）

已有实例补录 `deepseek` 模型连接与 `feishu-ops` 通知渠道时，用 [scripts/import-credentials.ps1](../scripts/import-credentials.ps1)：读仓库外模板 → 经**现有管理写端点**更新既有资源（保留 `resource_uid`，复用服务端加密 / 审计 / 发布路径），不直连数据库、不接触主密钥。

模板是 `KEY=VALUE` 文本，**必须**放在仓库之外（脚本检测到仓库内路径直接拒绝）：

```text
BASALT_BASE_URL=http://127.0.0.1:8080
DEEPSEEK_API_KEY=...
DEEPSEEK_BASE_URL=https://api.deepseek.com
DEEPSEEK_DEFAULT_MODEL=deepseek-chat
FEISHU_WEBHOOK_URL=...
BASALT_ADMIN_TOKEN=...        # 推荐改由进程环境注入，不写进模板
```

```powershell
powershell -NoProfile -File D:\code\basalt\scripts\import-credentials.ps1 `
  -TemplatePath D:\code\basalt-demo-us12\credentials.init -Test
```

| 行为 | 判据 |
| --- | --- |
| 幂等 | 模型连接已是托管凭据且尾号一致 → 跳过；通知渠道已是 `managed_configured` → 跳过（地址无法比对，不猜） |
| 冲突不覆盖 | 尾号不同且未显式 `-Rotate` → 不动凭据、退出码 **3**；`-Rotate` 才生成新凭据版本 |
| 模式升级 | env → managed 一次性切换；`base_url` / `default_model` 可在不带 `api_key` 的情况下更新 |
| 真实测试 | `-Test` 发一次模型最小调用与一次固定文案 `Basalt 连通性测试`；结论分 `success` / `failed` / `unknown`，`unknown` 不显示成已验证 |
| 输出 | 只含模式、尾号、host、状态与脱敏原因；**不**回显 Key / Webhook 地址 / admin token |

前置：托管写入要求 `secrets.master_key_file` 已配置且主密钥可读（§6.1）；主密钥缺失时按 503 `CREDENTIAL_UNAVAILABLE` 失败，脚本不改用 env 兜底。**通知渠道**还要求**当前生效**的网络域名白名单含目标 host（飞书为 `open.feishu.cn`，§2.2 的同一张白名单）：不在白名单时创建 / 更新返回 400 并点名 host（演练实测），先到管理台「执行环境」补上（保存即生效，无需重启）再导入。导入完成后模板里的明文应删除或转入部署者的密钥保管处——脚本不会自动删文件。admin token **不得**贴进对话或工单，只经进程环境或该模板传递。

### 6.4 恢复主密钥记录：`basalt secrets recover-record`（D-40）

**适用**：库里有托管密文，但 `instance_secrets` 的 `master_key` 记录丢失（升级 / 崩溃 / 误删），`state=recovery_required`、`recovery_reason=unprovable_provenance`。命令**离线**执行：不装配运行期、不启 MCP、不发通知、不调模型，零外呼。

```bash
basalt secrets recover-record \
  --workspace /opt/basalt/.basalt \
  --backup /backup/us16-trusted \
  --expect-backup-sha256 <带外记录的十六进制> \
  --confirm
```

前置（全部成立才执行，任一不成立即拒绝且零写入）：

1. `serve` 已停，命令持 `<ws>/basalt.lock`。不接受在线触发，也没有对应的 HTTP 端点。
2. 当前状态**正好**是恢复态：无 `master_key` 记录，且 `credential_versions.ciphertext IS NOT NULL` 计数 > 0。`ready` / `uninitialized` 与其他原因码一律拒绝（`state_not_recovery`）；记录已存在时先报 `record_already_exists`——重跑按幂等拒绝，而不是被拦成状态不符。
3. 备份**产于该实例首次成功写入记录之后**：备份库里有 `scope='master_key'` 行，且其 `operation_id` 能在备份库的 `management_operations` 找到对应的成功操作行。产记录之前的备份**不是**可信来源（`backup_record_missing`）——补写只能是凭空重建。
4. 原密钥已持久化在 `secrets.master_key_file` 指向的绝对路径。命令只读装载，不复制、不生成。
5. `--expect-backup-sha256` 来自备份时留存的**带外**记录（变更单 / 交接单 / 离线台账）。备份自带的哈希只证明内容一致、不是来源证明；缺失或对不上即 `backup_unverified`。

`--backup` 按内容判定路线，**恰好**命中一条，否则 `backup_invalid`：

| 路线 | 内容 | 约束 |
| --- | --- | --- |
| A 导出包 | `manifest.json` + 含 `instance_secrets` 行的 dump | 当前名册的 `instance_id` 必须等于 `manifest.source_instance_id`；**只在当前名册存在时可用**——manifest 没有名册文件，拿它拼 `instance.json` 属于重建 |
| B 原生备份集 | 一致性备份的 `basalt.db` + 同一备份集的 `instance.json` | `PRAGMA integrity_check` 通过。**名册缺失时只有这条路线可用**——备份名册是被恢复身份的唯一来源（逐字复制，不做规范化） |

校验顺序固定（失败保持恢复态、零写入；报告只给类别与计数，不输出密钥材料 / 明文 / 密文）：外部锚点 `backup_unverified` → 名册二选一 `backup_identity_mismatch` / `backup_roster_missing` / `backup_roster_invalid` → 当前无记录 `record_already_exists` → 备份记录恰好一条且可回溯 `backup_record_missing` / `backup_record_invalid` → 原密钥与记录一致 `key_record_mismatch` / `key_unavailable` → 当前全部密文可解且 AAD 六段一致 `ciphertext_undecryptable` / `aad_mismatch` → active 凭据的 `target_binding` 与当前已发布资源一致 `binding_mismatch` → 当前库是备份的延续 `ciphertext_mismatch` → 名册缺失时的 token 来源 `admin_token_source_missing` → 审计预写成功 `audit_prewrite_failed`。

写入顺序：**先名册（仅在缺失时）、再记录**，同一个 `instance.secrets_recover` 审计操作；`detail_json` 记备份类型与哈希、名册是否恢复及其哈希、`key_id` / 指纹与分类计数。中断后重跑按"名册逐字相等 / 记录已存在"判定——**不得**删掉已写名册"重新开始"，也不得把这一步写成 `ready`。

成功后：用同一配置重启，确认 `state=ready`、`secrets verify` 通过、代表性托管凭据可解且绑定不变；第二次启动零写入。

**边界**：

- 只覆盖**切换前**（SQLite 为权威存储）。切到 PostgreSQL 之后没有本命令的恢复路径——受影响实例必须在 `db switch` 之前完成恢复或明确退役。
- 只有产记录前的备份、或没有任何备份 → 实例**保持恢复态**，这就是命令的终态。人工处置只有两条：继续找产记录后的备份（同实例库备份 / 导出包 / 迁移前快照）重跑；或经人工评审显式 `revoke-managed --confirm`（§6.2）。后者是**破坏性操作**，**不是**恢复。
- 回退旧二进制会重新启用自动补写——**不得**用回退规避这条裁决。
- 没有 `--force` / `--overwrite` / `--new-key` 一类开关，不要找；也**不得**改写或删除密文、生成替代密钥来"修复"。

### 6.5 schema 3～5 存量的受控升级：`secrets adopt-legacy` 与 `instance adopt`

旧版（schema 3～5）**没有**安全的离线记录准备方式：`secrets` 是重命令，`serve` 会迁移、`Ensure` 会自动补写记录、启动时应用全部已发布 MCP。因此识别一律用**只读 SQL + 文件核对**，**不得**用旧 `serve` / `secrets status` / `secrets init` 去"试出"记录。

**第 1 步：整实例备份 + 清单 + 外部锚点**。停 `serve`，把 `basalt.db`（连同 `-wal` / `-shm`）、`basalt.yaml`、整个工作区与密钥副本打成一套；目录里生成 `backup.json`：schema 版本、库 / WAL / SHM / 配置 / 名册的 SHA-256、`key_id` / 指纹、密文计数、`records_absent=true`、责任人与时间。把 `backup.json` 的字节级 SHA-256 记到**带外**（变更单 / 交接单 / 离线台账）——备份内的审计行与自哈希**不能**替代外部锚点。清单同时记 `source_config_sha256`（源端基线，只用于漂移检测）与 `backup_config_sha256`（两者必须相等）。

**第 2 步：复制到另一路径的隔离副本**，再跑：

```bash
basalt secrets adopt-legacy \
  --workspace <copy-root>/.basalt \
  --backup <backup-dir> \
  --expect-backup-sha256 <带外锚点> \
  --source /opt/basalt/.basalt \
  --confirm
```

源端本地不可读时用 `--attest-source-unchanged <见证编号>` 替代 `--source`（二选一）。命令只对副本操作：校验锚点 / 清单 / 副本一致 / 原密钥与全部密文 / 源端未漂移 / 名册，然后把副本迁移到 v6 并写 **1 条** `record_origin=attested_legacy` 记录。它**不**装配运行期，**不**写名册、**不**改配置、**不**生成或复制主密钥、**不**改写或删除密文；没有 `--force` / `--new-key` / `--skip-*`。

- 密钥路径在副本主机不可达时，用 `--master-key-file` 受控读取：K1 = 备份内副本（哈希等于清单）；K2 = 备份外副本（必须同时给 `--expect-key-sha256` 锚点）。
- 副本初始配置哈希与清单不符 → `backup_invalid`。
- 原密钥绝对路径确实需要改时，**只能**在 `adopt-legacy` 成功之后、`instance adopt` 之前由部署者改副本同级的 `basalt.yaml`：在变更单里记原 / 目标哈希、原因、操作者、时间与审批编号，再以 `--config-change-ref` 运行 `instance adopt`。命令不代写配置；它重算 `config_without_key_path_sha256` 证明除密钥路径外没动别的键，越界修改即使有引用也以 `config_change_out_of_scope` 拒绝。改过的副本**不得**再称"与备份逐字一致"，`target_config_sha256` 也不得覆盖 `source_config_sha256`。
- 源端在复制之后又变过 → `source_changed`：丢弃副本、重新备份。**不得**在部分迁移的副本上重试——从备份重新复制一份新副本。

**第 3 步：无名册副本先补名册**。

```bash
basalt instance adopt --workspace <copy-root>/.basalt
```

离线执行、零外呼，**只写** `instance.json`：不写 `instance_secrets`、不铸密钥、不改库。名册就位后 `db export` / `db verify --handoff` / `db switch` 才可用（无名册时导出直接拒绝）。需要调整配置时加 `--config-change-ref`，语义同上。有册实例这一步只校验、不改写。

**第 4 步：切换**。切换前**必须**完成 `instance adopt` 并核对：名册存在、`instance_id` 是新值、`db export` 不再报身份缺失、对导出的交接包跑 `db verify --handoff` 通过、当前配置哈希等于审计里的 `target_config_sha256`。然后把原工作区移到 `<ws>.pre-us16-<时间戳>` 留作回退、副本就位，按 §5.6 第 7～9 步切换，并在验证窗口内确认 `ready`。

**失败与边界**：

- 失败时按 §5.7 的三维判据决定能否回退；任一维脏或无法证明就停自动回退。原实例与备份保持可用。
- **不得**就地迁移、**不得**在原库上运行、**不得**在部分迁移的副本上重试。
- 无可用备份 / 原密钥丢失 → 只能保持恢复态，或转 env-only，或重建实例。备份 / 来源 / 锚点不足同样可能过不了这道流程，需进一步核查——**不得**因"能解密"就判定可启动。
- `revoke-managed` 是破坏性操作、**不是**恢复；`instance.secrets_retire`（D-41 候选）**未批准、未实现**，本手册不提供该入口，也不要按任何资料臆造一个。

## 7. 每日 Demo 与隔离演练

五套每日场景 D1–D5 属于维护者发布验收流程（US-14 交付的 `examples/daily/` 素材）；场景定义见需求文档 §2.5 与技术方案 §8.1（维护者内部正文，不在公开站点）。本节只规定演练环境与凭证边界；环境未准备完时不能把规划命令或替身读数记成生产验证。

- 每套场景在演练专用工作区运行，使用虚构数据、测试通知接收器及明确登记的凭证引用；恢复备份演练继续执行 §4.3 的停调度与副作用隔离。
- 依赖逐项核对：D1 需要 GitHub MCP（PAT 未授权时只跑夹具）与 README 读取；D2 需要已登记技能 `repo-digest`；D3 需要 Python 与 DBHub 只读 SQLite（样例库由生成器产出）；D4 需要自研只读 MCP `docs-readonly` 与连续两次运行的基线；D5 需要内置 `web_search` 的搜索凭据（复用该 Agent 绑定的 DeepSeek 模型连接）与 Fetch MCP——严格档实例另需在白名单放行 `api.deepseek.com`。缺失时明确失败，不补造结果；旧 D1 的本地 HTTP 三端点保留为**不调度**的独立回归夹具。
- 核对工作区、时区、实际触发时刻（以固定起点计算的触发序列作对照，覆盖 DST 日）及 `task_executions` / Run 关联。演练可缩短计划，但保留原每日计划配置和现场读数；短周期或人工触发不能冒充完整每日验证。
- 停机错过触发点不自动补跑；人工补跑单独记录 run_id。任务或通知结果不确定时先查审计，不盲目重试；通知没有端到端恰好一次承诺，重复执行可能产生重复消息。
- 演练结束仅回收本轮拥有的服务、MCP 进程、端口和临时目录；确认记录中没有真实凭证，保留足够的脱敏证据供复验。

### 7.1 示例 / 模板依赖与出网边界

`examples/` 下的五个 Demo 与八个模板是**可复制素材**，它们需要什么不等于 Basalt 需要什么（§1.1 / §1.3 口径不变）。复制某个素材前按本表核对：

| 素材 | 解释器 / 可执行 | 外部出网（按配置发送） | 凭据 |
| --- | --- | --- | --- |
| D1 `github-radar` | Python 3（`shell` 受控通道）、GitHub MCP | GitHub API（MCP 取候选仓库，`http_get` 读 README） | `GITHUB_PERSONAL_ACCESS_TOKEN`（`${ENV_VAR}` 引用） |
| D2 `repo-digest` | Python 3、GitHub MCP | GitHub API | 同上 |
| D3 `biz-daily` | Python 3、DBHub（Node） | 无（本地 SQLite 文件，库由生成器产出） | 无（只读 SQLite 样例） |
| D4 `docs-diff` | 自研 `docs-readonly`（Go 构建；主 module 内，安装器导出源码后 `go build`） | 无（只读本地资料目录） | 无 |
| D5 `industry-brief` | 内置 `web_search`、Fetch MCP | DeepSeek Search API（`api.deepseek.com`，受出网策略）、目标网页（由 Fetch 子进程抓取） | 无专用变量——搜索复用该 Agent 绑定的 DeepSeek 模型连接凭据（env 或托管） |
| 八模板 | 依各自 README 的「依赖」节：内置 `http_get` 为主；DBHub / Playwright / Brave / Fetch / `docs-readonly` 按模板声明 | 依各自 README 的「工具与域名」节 | 仅 `funding-watch` 需 Brave key；其余无凭据 |

- 复制素材不改变底座的出网策略：白名单与域名规则的配置主体仍是部署者（§2.2）；示例**不**自动放宽白名单、**不**自动导入 Agent。
- 示例中的脚本与夹具不含真实凭据与真实 UID；需要凭据的模板（`funding-watch`）在 README 明写「未验证，不要当可运行示例」。
- 共享只读 MCP server `examples/shared/mcp/docs-readonly` 只对 `-root` 指定目录开放读取，拒绝越界 / 绝对路径 / 软链接，无写工具；它需要 Go 工具链**构建**，运行时是静态二进制、不再需要 Go。
- 样例 SQLite 库由 `examples/shared/scripts/make_sample_db.py` 生成，不提交 `.db` 二进制；跨机器字节一致性不承诺，引用自己生成的库哈希。

### 7.2 模板更新：仓库内置素材与实例内 Demo 数据的边界

`examples/daily/**`（以及 `examples/shared/` 里被 Demo 引用的共享件）是**仓库内置模板**，随发布走版本控制；`basalt demo install` 只是把模板**复制**成实例数据。复制之后，实例工作区里的那一份就不再是模板：

| 位置 | 属于什么 | 谁改它 | 更新时能不能被覆盖 |
| --- | --- | --- | --- |
| 仓库 `examples/daily/<demo>/**` | 仓库内置模板 | 维护者随发布更新 | 不涉及实例 |
| `<workdir>/.basalt/agents/<demo>/**` | **实例数据**（含模型、绑定、正文） | 用户在管理台或编辑器里改 | **不得**静默覆盖 |
| `<workdir>/.basalt/agents/<demo>/runs/`、`fixtures/`、`scripts/` | **实例数据**（运行产物与用户脚本） | 运行期与用户产生 | **不得**删除 |
| `<workdir>/.basalt/demos/install.json` | 安装清单（实例记录） | 安装器写 | 由安装器维护 |

**`--force` 不是更新路径**：它把选中的 Agent 目录**整个删掉再重建**（连 `runs/`、`fixtures/`、`scripts/` 和你改过的正文一起），命令自身的帮助文案就是这么写的。它只适合"我确认这个 Demo 里没有要留的东西"，用它升级等于静默覆盖用户修改。

#### 四步更新（其他开发者可照做）

1. **备份**——把实例目录整棵复制到**仓库外**的位置：

   ```bash
   cp -a <workdir>/.basalt/agents/github-radar /backup/2026-11-01-github-radar
   ```

   备份**不得**进仓库：里面可能有实例路径、运行结果与用户数据。要回填时把路径反过来复制即可（见第 4 步）。

2. **冲突检测**——对同一个 Demo 跑试运行，只看三态，`--dry-run` **不写任何文件**：

   ```bash
   basalt demo install --dry-run --demo github-radar
   ```

   - `新建`：实例里还没有这个 Demo，直接正式安装即可；
   - `一致`：已装目录与模板逐文件相同（比较时跳过 `skills/`，并把模板里的 `{{BASALT_PYTHON}}` 按本机实际解释器归一），没有要合并的；
   - `已修改：<文件列表>`：这些文件与模板不同，列表还包含"模板有、实例缺"的文件。**正式安装会保留原目录**——三态只报告差异，不替你决定。

3. **人工合并**——按第 2 步列出的文件名，把模板的改动**逐文件**搬进实例目录（模板内容看仓库的 `examples/daily/<demo>/`）。合并后的正文是"模板的变化 + 你的本地修改"两者并集，不是任何一份的副本。**不得**用 `--force` 代替这一步。

4. **校验与回退**——再跑一次第 2 步的命令，确认剩下的差异都是你有意保留的；合并出错就从第 1 步的备份回填：

   ```bash
   rm -rf <workdir>/.basalt/agents/github-radar
   cp -a /backup/2026-11-01-github-radar <workdir>/.basalt/agents/github-radar
   ```

#### 为什么不提供自动更新脚本（也不入库）

模板与实例正文之间的差异是**语义差异**——用户可能改阈值、换模型、加步骤，机器无从判断哪一边该留下。自动合并可行的手段只有"整目录覆盖"，而那正是本节禁止的事。把更新脚本放进仓库还会把实例路径写进版本控制。所以仓库里只放模板，以及 `--dry-run` 这个**只读**的差异判据；真正的合并由人做。

#### 两个已知限制（照实记录，不要绕）

- **带 `--demo` 的部分安装会重写清单**：`install.json` 由本次选择重写，未被选中的 Demo 记录会从清单里消失（它们**目录还在**，只是清单不再列）。因此更新流程全程只用 `--dry-run`；要刷新清单就跑不带 `--demo` 的全量安装——前提是五个 Demo 的差异都已按上面的四步处理完，否则会触发第 3 步该避免的覆盖。
- **共享件没有"保持原样"以外的合并语义**：`<workdir>/.basalt/demos/` 下的样例库、DBHub 只读配置与 `docs-readonly` 源码由安装器按清单重建，不是用户编辑对象；它们被 `--force` 重建时不会碰 `agents/` 下你没点名的 Demo。

## 8. 长期运行与发布前未决项

下列策略必须在首次发布前明确并有实测读数；本次文档补充不代表已经实现自动清理、告警或资源限额。未决项由 US-14 发布清单承接，不能以默认值未说明的方式放行。

| 面 | 发布前需明确 | 需留证据 |
| --- | --- | --- |
| 数据保留 | 会话、三张审计表、任务执行历史、记忆表（`memory_entries`）、配置快照（`config_snapshots`）、文件记忆、日志和备份的保留期 / 大小 / 访问主体 | 正常增长、达到边界、清理后关联和消息组仍完整 |
| 磁盘与 PostgreSQL | WAL / 表空间增长、`pg_dump` 备份空间、磁盘满 / 只读 / 连接耗尽时的拒绝行为；告警由谁执行 | 审计预写失败不发生外部动作；错误可读；恢复后可继续 |
| 外部服务 | Provider / MCP / 通知的超时、限流、凭证轮换与出网边界 | 失败不泄露正文或凭证；取消和停机后资源可回收 |
| 副作用 | HTTP 写与通知可能已成功但回执丢失；人工处理与幂等责任 | 不把 unknown 当作可安全重放；演练能定位对应 run_id |
| 制品与平台 | 二进制与前端资源、平台 / 架构、版本、commit 和 sha256 | 用候选制品走真实控制台、进程树取消、锁、升级 / 恢复；制品变更后重验 |

业务指标默认用低基数维度（操作、结果类别、工具类型）；session_id、run_id、完整 URL、错误原文与凭证不作为指标标签。它们需要关联时放受控日志 / 审计，访问权限与保留期随数据策略一起确定。指标系统若尚未接入，保持为发布 / 扩展阶段设计，不声称已有 Prometheus 或追踪能力。

## 9. 管理面写操作要点（US-12 起）

管理台（`/console/`）的写操作全部先写 `pending` 管理操作再动目标，可在「操作记录」里按 `operation_id` 查证终态。日常写操作有几条固定边界：

| 面 | 固定行为 | 运维注意 |
| --- | --- | --- |
| 资源版本 | 版本只追加、不更新不删除；「撤回」用旧版本内容创建新版本，历史版本仍可查 | 版本号逐**资源 UID** 递增；同名删除后重建是**新 UID**，版本从 1 重新计，旧 UID 的版本、凭据与删除事实保留在 `resource_identities` 下可查；按名查询默认只回当前指针 UID |
| 资源范围 | `shared` = 全部 Agent 可用；`exclusive` = 仅属主 Agent 可用。`shared → exclusive` 前检查其他 Agent 绑定、活动 Run 与消息路由，占用即 409 `RESOURCE_SCOPE_IN_USE`；专属 → 共享保留 UID、版本与引用 | 专属资源对其他人变成「不可见 / 不可选 / 准入拒绝」，不会自动解绑；Agent 复制遇到源专属引用会阻断并要求显式映射 |
| 托管凭据 | Key / 地址写入 `credentials` + `credential_versions`（AES-256-GCM，主密钥在工作区外）；替换生成新版本，撤销把该身份全部版本置 `revoked` 并清空密文 | 撤销后旧 Run 在下一次模型调用检查点终止；通知发送解析当前 `active` 版本。**不**自动回退到 env，也不恢复旧 Key |
| 资源绑定 | 绑定唯一权威是 `AGENT.md` 的 `resource_bindings`（`{name, uid}`）；数据库 `agent_resource_bindings` 是可重建投影 | 迁移、复制、恢复、启动、准入、原文保存都**不得**按同名补绑；引用无绑定时准入报 `AGENT_RESOURCE_BINDING_MISSING`，保存时需显式重新选择 |
| 发布 | 定义文件承载期望内容，发布事实（版本与指针）在数据库；发布失败保留旧文件与旧指针 | 手工编辑定义文件同样走启动对账，不被静默覆盖 |
| MCP 生效 | 发布版本 ≠ 应用版本即「待重启」；连接失败保留旧应用版本并标记未连接 | 改了 `mcp_servers.yaml` 相关条目后按 §5.4 核对白名单（http 域名在管理台热改，stdio 命令名仍需重启）并重启 |
| 归档 / 恢复 | 归档把目录移入 `archive/`，会话 / 记忆 / 审计保留；恢复分两条路径：原名要求归档状态文件仍在且为 `archived`，条件迁移后 generation +1；**异名恢复保留原 `agent_uid`**（只改名称），默认 `disabled`。旧归档无可信 UID 标 `legacy_unrecorded`，恢复时生成新 UID | 归档前会等待在途 Run 有界退出，超时 409 且完整回滚；`basalt profile delete` 与页面归档写同一种归档行，可在归档列表看到；目标名被占 409，不自动换名 |
| 通知渠道 | 地址两模式（env 占位 / managed 密文）；列表只回 `url_host`，`url_host` 为空**不代表**没有地址；删除写墓碑（身份 `state='deleted'`），活动同名新建 409，已删除名可用新 UID 重建；被绑定的 Agent 引用时删除 400 点名 | 全局页的「文档引用的 Agent」是活动与归档 `AGENT.md` 的整词扫描（归档项带 `archive:`），只是线索；**精确范围**由绑定的 UID 决定。Agent 的 `notify_mode` 三态：`none` = 不启用；`all_eligible` = 有资格的全部（共享 + 本人专属）；`allowlist` = `notify_channels` 列出的渠道。空数组**不是** `none`；旧配置空 / 未声明按 `all_eligible`、非空按 `allowlist` 读取。Run 准入冻结**具体渠道 UID 集合**，之后新建的渠道不会加入已运行任务；发送仍需调用时显式指定渠道，没有默认渠道或自动发送 |
| 迁移 0003 | 升级到 `schema_version=3` 时先给活动 Agent 的状态文件分配 `agent_uid`（升级为 schema 2）、再做 SQL 迁移、最后回填可证明的 `resource_bindings` | 执行前确认**没有活动 Run**；无法证明资源身份连续的引用不写绑定，页面对应显示「引用未记录（迁移前无法证明身份）」，需显式重新选择；数据库迁移失败时状态文件可能已是 v2——按备份恢复，不自动降级 |
| 工作区浏览 | 只读，根固定 `.basalt/agents/`；不跟随软链接、隐藏 staging / backup 残留；单文件超过 256 KiB 直接拒绝（不截断） | 浏览端点不写审计、不产生管理操作行；文件编辑仍走 Agent 文件提交 |

条件写（修改 / 删除 / 发布）要求 `If-Match` 为当前 ETag 的逐字强 ETag：缺失或不匹配一律 412 且零写入。两个人同时保存时，后写者需要先重新读取 ETag。

### 记录保留与自动清理（US-16A 补充裁决，2026-11-02）

保留天数默认30天；清理开关仍需显式启用。当前8080走查实例按用户授权启用下列配置。
管理台「运行排障 → 清理」显示生效策略、保护范围和执行历史；该页只读，配置修改后重启生效。

```yaml
diagnostics:
  retention_days: 30
observability:
  retention:
    cleanup_enabled: true
    history_enabled: true
    history_days: 30
    diagnostic_events_days: 30
    assembly_records_days: 30
    alert_history_days: 30
    export_history_days: 30
    interval: 1h
    batch_size: 1000
    max_batches_per_run: 20
```

| 数据 | 清理规则与保护条件 |
| --- | --- |
| 诊断、阶段耗时、装配证据 | 超过30天清理；运行中的关联证据保留；启用导出时诊断需已有确认导出证明 |
| 会话、消息、摘要、所属运行及模型/工具/嵌入审计 | 会话最后活动超过30天，且整个关联组都已结束、无新消息、无未知调用、未决配额、未释放资源或外部引用时，事务内整组清理；与新运行准入互斥 |
| 无会话运行 | 完成时间超过30天且满足上述保护条件后成组清理；活跃会话内的旧运行随会话保留 |
| 摄取历史 | 超过30天的已结束尝试；保留每个版本的最新尝试、最近成功尝试和未决配额/调用；不删除知识文档、版本、片段与向量 |
| 管理操作 | 仅清理已被后续成功操作替代、无引用的历史知识上传/重建/简介编辑和连接测试操作；安全审计、基线、恢复、身份、凭据、资源版本及当前连接测试依据继续保留；未知新操作类型默认保留 |
| 调度/投递、配额用量、配置快照 | 已结束且无运行归属的旧调度/投递可清理；当前用量周期、未结算预留、仍被运行引用的快照保留；孤立审计无法证明安全归属时保留 |
| 告警、导出、清理执行历史 | 旧的详细历史可清理；当前告警状态、恢复依据、未确认/未知导出、最近维护结论及必要删除窗口依据保留；业务清理历史按固定30天桶保留计数汇总，不声称连续覆盖或逐条删除证明 |
| 服务日志 | 启用业务历史清理后写入 `.basalt/retention/logs/`；按天或单文件32 MiB轮转，归档最多30天、总预算256 MiB；容量超限可提前清理旧日志，保留当前文件 |
| 上传临时文件 | 仅处理 `.basalt/uploads/.upload-<32位十六进制>` 的过期普通文件；不碰最终上传源文件、任意系统临时目录或未知路径 |
| 备份 | 只处理 `.basalt/retention/backups/verified-*` 中带完整校验和恢复验证标记的平面备份；超过30天且有更新的已验证备份才清理，至少保留最新两份；损坏、未知文件、符号链接及原有 `.setup/` 备份继续保留 |

有效知识、长期记忆、当前 Agent/模型/MCP/技能/任务定义/绑定/策略、凭据密钥、UID、墓碑、已发布版本与必要审计锚点不按天删除。
启用外部导出的实例会保守保留业务历史源，直到有单独可证明的导出删除协议；页面记录 `export_sources_protected`。
30天是普通历史的回收期限，保护数据可能保留更久；有界批处理可能需要多轮完成。删除后数据库可复用空闲页，不保证数据文件立即缩小。
本进程指标只保存在内存，重启会归零，不代表拥有30天指标时序历史；数据库推导指标受底层记录保留期影响。

备份格式 `basalt-restored-backup-v1` 的 `verified.json` 只能在隔离数据库成功恢复并核对工作区归档后写入。
至少包含 `database.dump`、`workspace.zip`、`basalt.exe` 的 SHA-256，且目录只能含清单列出的普通文件与标记；清理前再次校验全部文件。
保管好配置、环境密钥来源和实例密钥；本机 DPAPI 密文不代表可跨机器解密。
新增 PostgreSQL 前向迁移0007；不能仅换回旧二进制打开v7数据库，回退需使用同一停机点的数据库、工作区、配置和程序备份。
