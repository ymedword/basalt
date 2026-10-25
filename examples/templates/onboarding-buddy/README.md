# onboarding-buddy —— 新同事入职导读模板

用自研只读 MCP `docs-readonly` 扫描入职资料库，按固定分桶规则给出阅读顺序，并报告
相对上次运行的新增 / 变更 / 移除。排序与变更口径由 `onboarding` 技能规定，
**无凭据、无脚本、无外网**。

档位：A（全量）

## 领域

人事培训——新同事第一周该读什么、以什么顺序读，以及这次资料库又多了什么。

## 能力

- 只读列出资料库里的全部 Markdown 文档（路径 / 大小 / 修改时间）；
- 逐篇取元信息（`size` / `modified_at` / `sha256`），元信息失败的那篇进「待确认」；
- 按路径形态分三桶（必读 / 按角色 / 参考）排阅读顺序，桶内按路径升序；
- 以记忆里的 `path → sha256` 表为基线，判定新增 / 内容变更 / 移除（待核实）；
- 只对新文档与变更文档读正文，最多 5 篇，其余写「未读正文」；
- 有文档元信息取不到时保留旧基线，下一轮重读同一段。

## 工具与域名

| 项 | 值 |
| --- | --- |
| 内置 `tools:` 声明 | `write_file`、`save_memory`、`recall_memory` |
| MCP 工具名 | `list_documents`、`read_document`、`document_metadata`（自研 server 的三个工具，仓内可核实） |
| `skills:` 声明 | `onboarding`（实体在本目录 `skills/onboarding/SKILL.md`） |
| `mcp_servers:` 声明 | `docs-readonly`（stdio，见下） |
| 出网域名 | 无——只读本地目录，不访问网络 |

**MCP 自身出网范围**：`docs-readonly` 是本地 stdio 进程，只读打开根目录下的
Markdown 文件，不联网；它的行为不受 Basalt 域名白名单约束（白名单只管内置
`http_get` / `http_post`）。

**docs-readonly 配置要点**（`mcp_servers.yaml` 里一条）：

```yaml
docs-readonly:
  transport: stdio
  command: <go run 的绝对路径 / 预编译二进制路径>
  args:
    - "-root"
    - "<资料库根目录绝对路径>"
```

根目录也可用环境变量 `DOCS_READONLY_ROOT` 给（`-root` 优先）。server 拒绝 `..`、
绝对路径与软链接，只有三个只读工具，没有写工具。

## 文件范围

| 路径 | 读 / 写 |
| --- | --- |
| `onboarding-plan.md`（Agent 根目录） | 写（整文件覆盖） |
| 记忆 | 读基线、全部元信息成功时写新基线（`onboarding-buddy 上次运行`） |
| `fixtures/docs/*.md` | 只读（仅离线复跑用；对模型不是必读路径） |
| `skills/onboarding/SKILL.md` | 复制到 `.basalt/skills/onboarding/SKILL.md`；对模型只读 |

## 凭据

无。模板里不出现任何 `${...}` 占位；资料库里的示例文档是虚构内容。

## 依赖

| 依赖 | 说明 |
| --- | --- |
| Basalt 实例 | 提供文件工具、记忆工具、MCP 客户端池与调度 |
| `docs-readonly` | 仓内 `examples/shared/mcp/docs-readonly`（主 module 内 `package main`，纯标准库、零第三方依赖；`basalt demo install` 导出源码并生成 `go.mod`）；`go build` 后指到你的资料库根目录 |
| 技能实体 | 复制 `skills/onboarding/` 到 `.basalt/skills/onboarding/`，否则声明名无实体、整只 Agent 不注册 |
| 模型连接 | `provider.name: local-model` 是占位，导入后绑定 |

## 档位

**档位：A（全量）**——完整 Agent 目录 + 技能 + 夹具 + 固定期望结果 + 本 README；
可装载；声明的内置工具全部是已注册内置工具；技能声明有实体；声明的 MCP 工具就是
`docs-readonly` 的完整 `tools/list`；无凭据可在本机复跑（server 由仓内源码构建）。

## 证据状态

| 证据 | 状态 |
| --- | --- |
| 可装载、内置工具可解析、技能有实体 | 仓内 `TestTemplates_LoadAndTools` 覆盖（e2e tag） |
| MCP 工具 ⊆ `docs-readonly` 的 `tools/list` | 仓内 `docs-readonly` 单测（`docs-readonly-tests`）覆盖工具名与只读行为 |
| 夹具复跑（阅读顺序与首跑语义） | 仓内夹具 + 人工按 `AGENT.md`「夹具复跑」起一次本地 server 走一遍 |
| 真实资料库实连 | **未验证**——需要你指到自己组织的资料库并确认内容可给模型读 |
| 真实模型 Run | **未验证**——需要你自己绑定模型连接 |

## 已知限制

- 只认 `.md`（大小写不敏感），不解析 PDF / Word / 网页——这些格式要先转换入库；
- 分桶只看路径形态，不看你写的目录说明：资料放错目录就会排错顺序，属于资料治理
  问题，模板不兜底；
- 单篇正文上限 1 MiB、清单上限由 server 常量决定；超限的那次运行按失败处理；
- 记忆条目以固定前缀 `onboarding-buddy 上次运行` 开头：**手动改写成别的格式会让
  下一轮读不到基线而退回首跑**；
- 不会自动发送给新同事（基线 `notify_mode: none`），要先配通知渠道；
- 本 Agent 声明了 MCP server：`docs-readonly` 没配好或连不上时，**整只 Agent 不注册**
  （加载期校验第 6 条，WARN `mcp server skipped: docs-readonly`）。

## 夹具与期望结果

`fixtures/docs/` 共 4 篇虚构资料：`handbook.md`、`security-basics.md`、
`teams/data-access.md`、`archive/2025-policy.md`。把 server 根目录指向它、清空
同名记忆后复跑（首跑），期望：

| 桶 | 顺序 | 依据 |
| --- | --- | --- |
| 必读 | 1. `handbook.md`；2. `security-basics.md` | 根目录下的 Markdown，桶内按路径升序 |
| 按角色 | 1. `teams/data-access.md` | 路径含 `/` 且不以 `archive/` 开头 |
| 参考 | 1. `archive/2025-policy.md` | 路径以 `archive/` 开头（先命中归档规则） |

顺序按**桶**优先、桶内再按路径升序——注意全局路径升序会把 `archive/` 排在最前面，
那正是归档文档不该出现的位置，所以分桶必须先于排序。

变化段应为「首跑，无基线」；`未变` 写「首跑，无基线」而不是 0 篇。第二次复跑
（不改文件）应为：三集合全空、`未变 4 篇`。改动其中一篇的正文再跑一次，应出现
一条「内容变更」并写出新旧哈希；把一篇文档移出目录再跑，应出现一条「移除（待
核实）」。

> 注：`examples/shared/fixtures/docs/`（D4 `docs-diff` 用的那套 4 篇）可以替代本目录
> 作为只读根目录复跑，此时桶分布变为 必读 3（`handbook.md`、`security-basics.md`、
> `tooling.md`）+ 按角色 1（`teams/data-access.md`）、参考 0。期望值以你实际指向的
> 根目录为准，README 这里只固定本模板自带夹具的读数。

## 通知路径

基线 `notify_mode: none`（零通知）。要每天推给新同事：管理台把 `notify_mode` 改成
`allowlist` 并勾选渠道，再给 `daily-onboarding-plan` 配 `deliver_to`（通知渠道的
`resource_uid`，`rs_` 前缀）；该 Run 会被强制 `notify_mode: none` 且工具面去掉
`notify`，由系统代投，模型不自己调用。

## 生效方式

复制目录后：`docs-readonly` 用 `go build` 出二进制（或直接用 `go run`），在
`mcp_servers.yaml` 加条目，技能复制到 `.basalt/skills/onboarding/`，绑定模型连接，
重启 Basalt（或走既有受控发布流程）。`mcp_servers.yaml`、技能与 `AGENT.md` 的变更
都要重启后生效。
