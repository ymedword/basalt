# Basalt 运维手册：依赖条件、监听与出网、时区、备份、升级与停机、凭证

> 本文档是 Basalt 三件套工件中的**运维篇**：装、看、备份、升级、停机、凭证六件事的操作口径与判据。契约四件（Run 与错误 / AGENT.md 与工具 / 完整 DDL / 静态 OpenAPI）见 [contracts/](contracts/)，兼容性与迁移边界见 [Compatibility.md](Compatibility.md)，Why/What/How 见 [IndustrySurvey.md](IndustrySurvey.md)、[Requirements.md](Requirements.md)、[TechnicalDesign.md](TechnicalDesign.md)。
>
> 本文与 [TechnicalDesign.md](TechnicalDesign.md) 冲突时**以技术方案为准**。本文不重复设计理由，只写"运维照着做"的动作、判据与代价。

---

## 1. 依赖条件

### 1.1 底座运行时：零外部依赖

- `CGO_ENABLED=0` 静态二进制（linux/amd64、linux/arm64、windows、darwin 交叉编译），`scp` 一个文件 + `systemctl start` 即部署；目标机器**不需要** JVM / Node / Python / 包管理器
- 数据自包含：全部状态在**当前工作目录的 `.basalt/`**（SQLite 库 + markdown 记忆 + Agent 目录 + 技能库 + 日志），无外部数据库、无消息队列、无缓存服务

判据：目标机器上 `file ./basalt` 显示 `statically linked`（linux），且 `basalt status` 在没有网络、没有其他服务的机器上能跑出结果。

### 1.2 例外：某些 Agent 自己选用的解释器

下表依赖**不属于底座**，而是"某个 Agent 的选择"——只在那个 Agent 声明的能力被调用时才需要，缺了只影响它自己：

| 依赖 | 谁需要 | 缺了会怎样 |
| --- | --- | --- |
| Python 3 | 用 `shell` 工具跑 `scripts/*.py` 的 Agent（如每日 D3 的 CSV 检查脚本）、自写的 stdio MCP server | 该工具调用失败（`runtime_error`）；MCP server 连不上 → 它的工具不注册 → **声明该工具的 Agent 整只不注册**（WARN 点名）。⚠ 走 `shell` 的解释器**必须**先登记进 `shell.interpreters`（受控脚本通道：只接受 `[<脚本路径>, <参数…>]` 形态、脚本须落在该 Agent `scripts/` 或技能库内）——**不得**为图省事放宽解释器白名单，那等于引入任意代码执行（见 [contracts/agent-md.md](contracts/agent-md.md) §5.2） |
| Node.js / `npx` | 用 `npx` 通道拉起的社区 MCP server | 同上 |
| 宿主机 `sqlite3` CLI | **仅在线备份**需要（§4.2），不参与运行 | 退化为离线备份（停机拷贝，一样可靠，只是要停机窗口） |

**不得**把"某 Agent 需要 Python"写成"Basalt 需要 Python"——部署说明与验收口径以本表为准。

---

## 2. 监听与出网

### 2.1 监听地址

| 项 | 值 |
| --- | --- |
| 配置键 | `http.addr`（`basalt.yaml`） |
| 默认 | `127.0.0.1:8080`（只对本机开放；核心阶段无认证） |

**必须**：远程访问（多机联调 / 容器对外暴露）显式改 `http.addr`（容器内 `0.0.0.0:8080`），并自行保证网络层隔离——反向代理 + 内网 ACL 优先。

症状与代价：改了 `0.0.0.0` 却不加隔离 = 把"任何人可调 Agent（含 `shell` 工具）"挂到网上。OpenClaw 的 42,665 个暴露实例就是这么来的（见 [IndustrySurvey.md](IndustrySurvey.md) §4–5）。

复核（可复制）：`ss -tlnp | grep 8080`——默认部署只应看到 `127.0.0.1:8080`。

### 2.2 出网：三种流量分开看

| 流量 | 谁控制 | 受不受白名单 |
| --- | --- | --- |
| 工具出网（`http_get` / `http_post`）+ 通知投递（`notify`） | **模型**可控 | **是**——`http.allowed_domains` 域名白名单，scheme 固定 http/https（代码固定，配置放行不了） |
| 供应商模型调用（`providers[].base_url`） | 运维配置 | 否——端点由运维在 `basalt.yaml` 里定，模型碰不到 |
| MCP server 自己的网络行为 | server 自己 | 否——独立进程，白名单管不到（**有意接受**的边界，见技术方案 §4.3） |

企业若要求"模型调用也只能出内网"：把 `providers[].base_url` 指向内网网关（vLLM / 一方网关）即可，白名单不需要动。

**代理不由进程自己读**：`HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` 环境变量在 Basalt 里**一律不生效**（HTTP 客户端不读环境代理）。出网要么直连，要么由**运维侧网关**承担（透明代理 / NAT 出口）——这样"哪条流量从哪个口出去"由网络层决定，而不是由某个进程继承到的环境变量决定（否则同一条 `http_get` 在不同 unit / 容器里走向不同出口，白名单还在、边界已经变了）。MCP server 是独立进程，它读不读代理变量是自己的事（同上表第三行）。

**必须记住的生效时间差**：

- **改完需重启**：`http.allowed_domains`、`shell.allowed_commands`、`shell.interpreters`、`shell.env_passthrough`、`sandbox.cross_agent_read`、`mcp_servers.yaml`、`http.addr`
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

### 4.1 备份什么

`.basalt/` 整目录：

| 内容 | 类别 | 丢了会怎样 |
| --- | --- | --- |
| `basalt.db`（+ `-wal` / `-shm`） | **不可再生** | 会话 / 消息 / 审计三表 / 记忆（sqlite 档）/ 定时状态全丢 |
| `agents/`、`skills/` | 配置（可重建，但重建成本高） | Agent 与技能全丢 |
| `memory/*.md`（markdown 档） | **不可再生** | 长期记忆丢（记忆后端为 markdown 时） |
| `mcp_servers.yaml`、`AGENTS.md` / `SOUL.md` / `USER.md` | 配置 | 同上 |
| `archive/` | 已下线 Agent 的目录 | 归档配置丢（会话 / 记忆 / 审计不受影响，它们不在文件里） |
| `logs/` | 可弃 | 只丢日志 |

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
#   ③ 出网收口：副本 basalt.yaml 的 http.allowed_domains 改成**只含测试端点**——**写成域名形态**
#      （localhost，或 hosts 里自造的测试域名）；**裸 IP 字面量会被直接拒**（白名单按域名匹配，
#      放行 IP 等于绕过它，见技术方案 §4.3.1）。未被列入的域名调用以 sandbox_denied 失败
#      ——正是演练想要的"副作用走不出去"（把白名单清空是最严的收口，一样达到目的）

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

---

## 5. 升级、回滚与停机

### 5.1 升级（受控前向迁移）

顺序**必须**固定，且**从停服到二进制就位之间不得起服**：

1. **停服**：`systemctl stop basalt`
2. **备份，并保持停机**：按 §4.2 的离线步骤做（**跳过其中 `systemctl start` 那一行**）——升级前必做，这是回滚的唯一依托；备完不起服，直接接第 3 步
3. **换二进制**：旧版改名留底（`mv basalt basalt.bak-<旧版本>`）→ 放新二进制 → `chmod +x`
4. **启动并迁移**：`systemctl start basalt`——启动时按 `schema_version` 应用增量 SQL（迁移脚本入库、随二进制发布；**已发布的迁移不得改写**，只许追加新版本）。迁移失败**必须**报错退出——**不得**带着半迁移的库继续跑
5. **验证**：`basalt status` → `basalt session list` → `GET /api/v1/health` → 跑一次真实调用（同 §4.3 判据的 1~5）
6. **观察**：`logs/` 里确认无 ERROR 级噪声

**不得**把"停"与"起"拆成两段（先起旧版 → 换文件 → 再起新版）：那两段之间跑的还是**旧代码**——迁移没跑、验证假绿，真正的切换只发生在文件替换那一刻，而没有任何步骤检查过它。

建议：先用备份在独立目录起一次新版做 dry run（与 §4.3 同一手法，含其中"演练隔离"三步），确认迁移与启动无误再动生产目录。

### 5.2 回滚

| 情形 | 动作 |
| --- | --- |
| 只是二进制问题（库结构未变） | 停服 → 换回旧二进制 → 起服（**不动**库与配置） |
| 库已被新版迁移 | **旧二进制 + 升级前备份**：停服 → 按 §4.3 恢复库与配置（**跳过其中 `systemctl start` 那一行**）→ 换回旧二进制 → 起服 |

两种情形同样是"**停 → 整备 → 起**"：**不得**在换回旧二进制之前先起一次服务——旧二进制面对已被新版迁移过的库，正是"库结构对不上"的现场，让它起来只会在日志里再制造一轮错误。

核心阶段只做**前向迁移**，不提供反向迁移脚本——**不得**指望"降级脚本"，回滚的唯一可靠路径是"备份 + 旧二进制"。

### 5.3 停机顺序与工作目录

**停机是有序收尾，不是杀进程**。收到 `SIGTERM`（`systemctl stop` 的默认信号）后，顺序**必须**是：

1. **停调度新触发**：调度器不再接受新的触发点（在跑的照常收尾，不新起 Run）
2. **取消并等待在跑 Run**：向所有在跑 Run 发出取消，**有界等待**其退出——等的是"审计与消息落库完成"，不是"让模型把话说完"
3. **停 HTTP**：`http.Server.Shutdown`（拒新连接、等在途请求结束）
4. **关 MCP**：关闭 MCP 客户端与其子进程（stdio server 的子进程不留给系统收割）
5. **关库**：关闭 SQLite 连接（WAL 落盘、`busy_timeout` 不再有等待者）
6. **退出**——`.basalt/basalt.lock` 由**内核**释放（独占文件锁随进程消亡解锁）：**不得**写"退出前删锁文件"的收尾，那会在删除与下一个进程加锁之间制造竞态

宽限：systemd 的 `TimeoutStopSec=` **必须大于**停机宽限（工具执行超时可达分钟级）。**不得**指望 `SIGKILL` 兜底——被强杀的进程留下 `running` Run 与 `pending` 审计行，下次启动要走恢复扫描收尾（能收敛，但白丢一次干净退出）。

**工作目录就是工作区**（§1.1：状态全在"当前工作目录的 `.basalt/`"），因此 systemd 单元**必须**显式给 `WorkingDirectory=`：

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

---

## 6. 凭证

| 位置 | 规则 |
| --- | --- |
| `basalt.yaml` 的 `providers[].api_key` | 只允许 `${ENV_VAR}` 占位；启动时解析，未解析（`${` 残留）或为空 → **拒绝启动**并点名配置项 |
| 通知渠道（库表 `notify_channels.url`） | **只存 `${ENV_VAR}` 引用**，不存完整地址；发送时解析，解析失败给**点名环境变量**的可读错误 |
| 作者模型（US-12 的生成能力） | 与 provider 同规则 |

红线段（day-one 硬约束，**不得**违反）：

- 渠道地址（路径与查询串就是 token）**不得**出现在响应 / 日志 / 错误文案——对外只回 **scheme + host**
- 沙箱越界文案里**嵌着原始目标**（可能就是凭证），**不得**原样透传给响应或日志
- 页面编辑框**不回填**已存地址，故「`url` 留空 = 保留原值」
- 环境变量注入方式：systemd `EnvironmentFile=`（文件权限 `600`）或容器 `--env-file`；**不得**走命令行参数（`ps` 可见）或提交进仓库的文件

复核（可复制）：

```bash
grep -rnE "sk-|api_key: [^$]" /opt/basalt/basalt.yaml       # 期望：零命中
sqlite3 /opt/basalt/.basalt/basalt.db \
  "select name, type, length(url) from notify_channels;"    # url 应是 ${...} 占位形态
```

第二条只查 `name` / `type` / 长度——**不得**把 `url` 原值打印出来（一次回显就把它写进了终端历史与截图）。

## 7. 每日 Demo 与隔离演练

五套每日场景 D1–D5 的定义和验收见 [TechnicalDesign.md](TechnicalDesign.md) §8.1 与 [Requirements.md](Requirements.md) §2.5。它们是 US-14 待交付素材；环境未准备完时不能把规划命令或替身读数记成生产验证。

- 每套场景在演练专用工作区运行，使用虚构数据、测试通知接收器及明确登记的凭证引用；恢复备份演练继续执行 §4.3 的停调度与副作用隔离。
- D1 需要可控的 HTTP 测试域名；D2 需要已登记技能；D3 需要 Python 和受控脚本路径；D4 需要连续两次运行的记忆基线；D5 需要显式登记的只读 MCP server。逐项验证依赖，缺失时明确失败，不补造结果。
- 核对工作区、时区、实际触发时刻（以固定起点计算的触发序列作对照，覆盖 DST 日）及 `task_executions` / Run 关联。演练可缩短计划，但保留原每日计划配置和现场读数；短周期或人工触发不能冒充完整每日验证。
- 停机错过触发点不自动补跑；人工补跑单独记录 run_id。任务或通知结果不确定时先查审计，不盲目重试；通知没有端到端恰好一次承诺，重复执行可能产生重复消息。
- 演练结束仅回收本轮拥有的服务、MCP 进程、端口和临时目录；确认记录中没有真实凭证，保留足够的脱敏证据供复验。

## 8. 长期运行与发布前未决项

下列策略必须在首次发布前明确并有实测读数；本次文档补充不代表已经实现自动清理、告警或资源限额。未决项由 US-14 发布清单承接，不能以默认值未说明的方式放行。

| 面 | 发布前需明确 | 需留证据 |
| --- | --- | --- |
| 数据保留 | 会话、三张审计表、任务执行历史、记忆表（`memory_entries`）、配置快照（`config_snapshots`）、文件记忆、日志和备份的保留期 / 大小 / 访问主体 | 正常增长、达到边界、清理后关联和消息组仍完整 |
| 磁盘与 SQLite | WAL 增长、备份空间、磁盘满 / 只读 / 损坏时的拒绝行为；告警由谁执行 | 审计预写失败不发生外部动作；错误可读；恢复后可继续 |
| 外部服务 | Provider / MCP / 通知的超时、限流、凭证轮换与出网边界 | 失败不泄露正文或凭证；取消和停机后资源可回收 |
| 副作用 | HTTP 写与通知可能已成功但回执丢失；人工处理与幂等责任 | 不把 unknown 当作可安全重放；演练能定位对应 run_id |
| 制品与平台 | 二进制与前端资源、平台 / 架构、版本、commit 和 sha256 | 用候选制品走真实控制台、进程树取消、锁、升级 / 恢复；制品变更后重验 |

业务指标默认用低基数维度（操作、结果类别、工具类型）；session_id、run_id、完整 URL、错误原文与凭证不作为指标标签。它们需要关联时放受控日志 / 审计，访问权限与保留期随数据策略一起确定。指标系统若尚未接入，保持为发布 / 扩展阶段设计，不声称已有 Prometheus 或追踪能力。
