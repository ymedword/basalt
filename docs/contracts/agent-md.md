# AGENT.md 与工具契约

> 本文档是 Basalt 运行时契约的**配置与工具**篇：`AGENT.md` frontmatter 的完整字段规范，以及全部内置工具的模型可见契约（名称 / 参数 JSON Schema / 返回与失败语义）。
>
> 与 [技术方案](../TechnicalDesign.md) §4（外部动作与工具）、§3（工作区与 Agent 目录）冲突时以技术方案为准。本文档中的字段名、工具名、参数名、返回文本是固定字面量，**不得**在实现中改写。
>
> **文档状态**：待验证设计草案（冻结条件：五链路原型验证通过 + 真实模型调用往返 + **双底座样例加载实测**——两个基座各装载 [Compatibility.md](../Compatibility.md) §5 的样例，见 [DevelopmentLoop.md](../DevelopmentLoop.md) §5.2）。
>
> **参数稳定性**：工具名称、参数名、类型和必填性以本文为准（含 `save_memory.scope`、`http_post.body`）。标注“扩展”的可选参数保留既有缺省语义；修改公开参数必须同步实现、测试与变更记录。外部资产兼容范围单独见 [Compatibility.md](../Compatibility.md)。
>
> **基线**：本文所称“基线”指 NivroOS 在 commit `50e1b30` 的已交付行为；对照范围与差异裁决见 [Compatibility.md](../Compatibility.md) §1 / §2。

---

## 1. AGENT.md 结构

`AGENT.md` = **frontmatter**（运行配置，派生成 `Profile`）+ **正文**（任务指令，注入系统提示词）。

```markdown
---
name: ops-agent
description: 运维助手
identity:
  agent_name: 运维小欧
  prompt: 你是一个专业的运维助手……
provider:
  name: deepseek
  model: deepseek-chat
  temperature: 0.7
tools:
  - read_file
  - shell
  - save_memory
skills:
  - digest-format
mcp_servers:
  - github-mcp
notify_mode: none
notify_channels: []
resource_bindings:
  model_connection:
    - name: deepseek
      uid: rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A  # 系统分配，不得手填
  notify_channel: []
bootstrap:
  - AGENTS.md
  - SOUL.md
  - USER.md
settings:
  max_iterations: 10
  max_history_turns: 20
schedules:
  - id: daily-weather
    cron: "0 0 8 * * *"
    zone: Asia/Shanghai
    message: "查一下今天的天气并给出穿衣建议"
---

你是一个专业的运维助手。被触发时……（正文，注入系统提示词）
```

**一个目录 = 一个 Agent**：`.basalt/agents/<name>/` 内是 `AGENT.md` + 可选 `scripts/`、`REFERENCE.md`。目录名即 Agent 名，**必须**与 frontmatter 的 `name` 逐字一致（不一致整只不注册）。

---

## 2. frontmatter 字段规范

| 字段 | 类型 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- | --- |
| `name` | string | **是** | — | 必须等于目录名（改名 = 新建 + 归档旧的） |
| `description` | string | 否 | 空 | 一句话描述，管理台展示用 |
| `display_name` | string | 否 | 空 | 管理台展示名。**只影响展示**（列表 / 详情 / 面包屑）：不参与提示词、目录名、`agent_uid` 或运行标识，改名不改身份。去空白后为空 = 未声明，展示回退 `name`。结构化写入拒绝控制字符与超过 64 字符的值；加载期不对它报错 |
| `identity.agent_name` | string | 否 | `name` | 人格短名，注入提示词的自我称谓 |
| `identity.prompt` | string | 否 | 空 | 人格提示词，拼在系统提示词开头 |
| `provider.name` | string | **是** | — | 必须是已发布模型连接名；保存与准入要求 `resource_bindings.model_connection` 中有且仅有一条同名绑定，且绑定 UID 是当前发布指针（加载期只判名字可用，见 §2.1） |
| `provider.model` | string | **是** | — | 模型名；**空白即拒**（脚手架填占位符「请在此填写模型名」） |
| `provider.temperature` | float | 否 | 供应商缺省 | 0–2 |
| `tools` | []string | 否 | `[]` | **不声明 = 空池**：模型一行工具都看不到。唯一例外是 `notify` 的受控装配：保存与装载按 `notify_mode` 与渠道资格自动增删且必须回报变更，见 §2.6 |
| `skills` | []string | 否 | `[]` | 元素 = 公共技能库目录名；**声明即绑定**；只写名字，**不支持** `skill@version`；声明名在库里没有实体、或没有**已发布版本** → 整只不注册（Run 开始时冻结该发布版本的完整内容清单，见 §2.4） |
| `mcp_servers` | []string | 否 | `[]` | 元素 = `mcp_servers.yaml` 里的 server 名 |
| `channels` | []object | 否 | `[{name: cli}]` | 接入渠道（核心阶段只有 `cli`） |
| `bootstrap` | []string | 否 | `[]` | 注入提示词的引导文件（相对工作区根） |
| `notify_mode` | string | 否 | 未声明的兼容规则见下 | 通知意图三态：`none`（不发送）/ `all_eligible`（可使用全部有资格渠道）/ `allowlist`（仅限 `notify_channels` 列出的渠道）。`none` 与 `all_eligible` 时 `notify_channels` **必须**为空数组；`allowlist` 至少一个元素。**空数组不表示「禁用通知」**——禁用只能由 `notify_mode: none` 表达。Run 准入冻结模式与渠道 UID 集合，`notify` 工具按冻结值判定。未声明时的兼容读取：`notify_channels` 为空 / 缺省 → `all_eligible`；非空 → `allowlist`；解析结果不改写文件 |
| `notify_channels` | []string | 否 | `[]` | `allowlist` 模式下的渠道名（= 全局通知渠道表的 `name`）。每个名字必须有资格且 `resource_bindings.notify_channel` 中有同名绑定；声明外的渠道按 `not_found` 失败并回填可用名。它只限制“能用哪些渠道”，**不**表示任务结束自动发送 |
| `resource_bindings` | object | 否（准入必填） | `{}` | 资源绑定的**唯一权威**（AGENT.md 为源头，数据库只做投影）。结构见 §2.5：`model_connection[]` 与 `notify_channel[]`，元素为 `{name, uid}`。加载期不要求完整（模板占位符与旧 Agent 仍可加载）；**保存期与 Run 准入**要求与 `provider.name` / `notify_channels` 一一对应、UID 为当前发布指针、已发布、有资格。同名新资源不自动接管旧绑定 |
| `settings.max_iterations` | int | 否 | 10 | ReAct 最大轮次；达上限 = `limit_reached`（见 [run-and-errors.md](run-and-errors.md) §2） |
| `settings.max_history_turns` | int | 否 | 20 | 送进提示词的对话轮数上限 |
| `schedules` | []object | 否 | `[]` | 定时任务（见 §3） |

### 2.1 加载期校验（任一不过 = 该 Agent 整只不注册 + 点名 WARN）

| # | 规则 | 不通过时 |
| --- | --- | --- |
| 1 | `name` 等于目录名 | WARN `name mismatch` |
| 2 | `provider.name` 在**已发布 Provider 版本表**里（`resource_pointers` 有未删除的发布版本） | WARN `provider version missing: <name>` |
| 3 | `provider.model` 非空白 | WARN `blank model` |
| 4 | `tools` 里每个名字都能解析（内置 11 个 / MCP / 代码内注册） | WARN `tool not registered: <name>` |
| 5 | `skills` 里每个名字在技能库有实体（加载期），且存在**已发布版本**（注册装配期） | 无实体 → WARN `skill not found: <name>`；有实体但无已发布版本 → WARN `skill version missing: <name>[, <name>…]`（多个缺失名按升序、以逗号加空格连接，一条 WARN 点名该 Agent 的全部缺失技能） |
| 6 | `mcp_servers` 里每个名字有**实际生效**的版本（待重启内容不算）且连接成功 | WARN `mcp server skipped: <name>` |
| 7 | `bootstrap` 里每个文件存在 | WARN `bootstrap file missing` |
| 8 | `schedules[].cron` 为 6 段且合法、`zone` 合法、`id` 全局唯一（**≥2 项时每项 `id` 必须显式声明**，见 §3） | WARN 并跳过该 Agent |
| 9 | `notify_channels` 的每个元素非空白 | WARN `invalid notify_channels` |
| 10 | `notify_mode` 取值合法且组合成立（`none` / `all_eligible` 时 `notify_channels` 必须为空；`allowlist` 至少一个元素；未声明按兼容规则读取，不改写文件） | WARN `invalid notify_mode` |
| 11 | `resource_bindings` 结构合法（对象；只允许 `model_connection` / `notify_channel` 两个已知键；元素为 `{name, uid}` 且均非空） | WARN `invalid resource_bindings` |

**加载期只判「当前可用 / 结构合法」**：名字能否解析、取值是否合法、结构是否完整。**绑定完整性不在加载期判**——模板占位符与旧 Agent 仍可加载（`resource_bindings` 可能为空或未覆盖全部引用）。每个引用恰有一条有效绑定、UID 是当前发布指针、已发布、有资格，属于**保存期与 Run 准入**判据（§2.3 / §2.5）；准入不通过只拒绝该 Agent 的新 Run 并点名，不把已加载的配置判成语法无效。

**校验失败即整只不注册**（不是"跳过那一条继续"）——半可用的 Agent 比明确不可用更危险。

**校验与生命周期是两件事**：上表判的是“配置是否可用”；`disabled` / `archived` / 状态文件损坏判的是“是否允许新 Run”。配置有效的 `disabled` Agent 仍然可见（`registered=true`、`runnable=false`），**不得**把它混报成 `registered=false`；状态文件缺失 / 损坏同样保留可见性，由 `lifecycle_error` 点名，并在 Run 准入时拒绝。

> 遗留形态备案：Agent 本地曾用 `skills/` 目录下的相对软连接表达技能绑定。该机制**已废弃**（Windows 下不可行），本地 `skills/` 目录被无视并记一条 WARN，绑定一律以 frontmatter 的 `skills:` 声明为准。

### 2.2 Agent 生命周期状态文件

生命周期状态**不在** `AGENT.md` frontmatter 里，也不由目录是否存在推断。权威载体是受保护管理目录中的一个 JSON 文件：

```text
<workspace>/.basalt/control/agents/<agent>.json
```

```json
{
  "schema_version": 2,
  "agent": "ops-agent",
  "agent_uid": "ag_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A",
  "state": "active",
  "generation": 3,
  "updated_at": 1760000000000,
  "operation_id": "op-1760000000000-a1b2c3",
  "checksum": "sha256:<64 位小写十六进制>"
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `schema_version` | integer | 当前为 `2`；`1` 可读，写入时升级为 `2`；不支持的版本必须拒绝旧二进制误读 |
| `agent` | string | 必须与目录名逐字一致 |
| `agent_uid` | string | **稳定身份**（`ag_` 前缀 + ULID 形态）。生命周期事件常驻；复制、新建、同名重建生成新 UID；原名 / 异名恢复沿用原 UID；旧归档无可信来源时标 `legacy_unrecorded`，恢复时生成新 UID 并记录来源 |
| `state` | string | `active` / `disabled` / `archived` |
| `generation` | integer | 每次状态迁移 +1，从 1 起；只做生命周期版本与乐观锁，**不是**身份 |
| `updated_at` | integer | Unix 毫秒（UTC） |
| `operation_id` | string | 写入本次状态的管理操作标识（资源基线使用 `system.baseline_v2`、身份基线使用 `system.baseline_v3` 的 `op-...`，不写 `migration` 字面量） |
| `checksum` | string | 校验和；计算时把本字段置为空串，对文件的 UTF-8 原始字节做 sha256；覆盖 `agent_uid` |

- **写入方式**：临时文件 + 原子 rename；只有生命周期服务可写。该目录不在 Agent 文件工具白名单、工作区浏览与 `fsnotify` 热加载范围内。
- **缺失 / 损坏**：状态文件不存在、JSON 不合法、`agent` 不匹配、`agent_uid` 缺失或格式非法（schema v2）、`schema_version` 不支持或 checksum 不符 → 该 Agent **拒绝新 Run**，索引 `state` 为空、`valid=0`，管理面返回 `lifecycle_error`。**不得**悄悄按 `active` 处理，**不得**自动重建为 active。
- **状态语义**：
  - `active`：允许新 Run；配置校验失败仍按 §2.1 整只不注册。
  - `disabled`：仍在管理面可见、可编辑；拒绝新 Run；已在跑的 Run 继续完成。
  - `archived`：不在活动注册表；只能经受控 restore 恢复，恢复默认 `disabled`。
- **一致性边界**：禁用 / 启用与 Run 准入共用同一 Agent 级读写锁（见 [run-and-errors.md](run-and-errors.md) §3.4）；HTTP、CLI、定时器与内部入口都必须检查。
- **复制与恢复**：复制创建新 Agent 目录与**新 `agent_uid`**，不复制生命周期文件、会话 / 记忆 / 审计、`schedules`（清空以免全局 `id` 冲突）；初始 `disabled`。归档恢复沿用**原 `agent_uid`**（原名与异名两条路径都只改名称、不改身份），默认 `disabled` 并清空 `schedules`，由管理员显式启用 / 重配。删除后同名重建是**新 Agent**：新 `agent_uid`，旧绑定与旧专属资源不自动连接。旧归档缺可信 UID 时标 `legacy_unrecorded`，恢复时生成新 UID，**不得**按同名猜出旧身份。不得把手工移动目录当作正常恢复。

### 2.3 内容版本、ETag 与结构化编辑

- `GET /agents/{name}/content` 返回整份 `AGENT.md` 与强 ETag：`"sha256:<64 位小写十六进制>"`（对文件原始 UTF-8 字节计算）。
- `PUT /agents/{name}/content` **必须**带 `If-Match`；缺失、格式非法或与当前 ETag 不等 → **412** `PRECONDITION_FAILED`，磁盘逐字节不变。`*` 与弱 ETag 不接受。
- `GET /agents/{name}/files` 返回 Agent 受管文件集合与**集合 ETag**：对按 path 升序排列的 `(path, sha256(内容))` 规整序列化后求 sha256，路径用相对目录的 `/` 分隔形式；只含 Agent 目录下的普通文件，不含 `.staging` / `.backup` 残留，不跟随软连接。集合 ETag 与单文件 `AGENT.md` ETag **不可互换**。
- `POST /agents/{name}/files` **必须**带集合 ETag 的 `If-Match`；缺失、格式非法、弱 ETag、`*` 或不匹配 → **412**，**不落盘、不写管理操作行**。比较与 staging 构建在同一 Agent 写锁内完成，锁内重新读取基础版本（防"读旧版本 → 依次保存 → 后写覆盖前写"）。
- `POST /agents/{name}/generate-files` 返回的草稿**没有**磁盘基础版本，响应不含 ETag；草稿不得直接当作已保存内容。
- 请求体二选一：
  - `{ "content": "<整份文件>" }`：整份替换；
  - `{ "changes": { ... } }`：只改已知 frontmatter 字段与正文的结构化 patch。服务端保留未改字段、正文与注释；无法在保留注释的前提下完成时返回 **400** `EDIT_UNSUPPORTED`，提示改用整份原文编辑。**不得**为了 patch 而整份重新生成、重排或丢注释。
- `changes` 已知字段：`description`、`display_name`、`identity.agent_name`、`identity.prompt`、`body`、`provider.name`、`provider.model`、`provider.temperature`、`tools`、`skills`、`mcp_servers`、`notify_mode`、`notify_channels`、`settings.max_iterations`、`settings.max_history_turns`、`schedules`、`bootstrap`、`bindings`。未列出的键 → **400**（严格已知键；不静默忽略拼写错误）。
- `changes.display_name`：三态——缺省 = **不修改**；非空字符串（去空白后）= 写入该行；空串 = 移除该行，展示回退 `name`。移除只在字段存在时改动文件；字段本就不存在时是空操作，ETag 不变。值含控制字符或超过 64 字符 → **400**。
- `changes.body`：缺省 = **不改正文**；给出字符串 = 整段替换正文；空串 = 清空正文（页面须二次确认）。**不得**用「未给 body」推断清空。
- `changes.provider.temperature`：三态——缺省 = **不修改**；数字（含显式 `0`，0 是合法值） = 写入该值；显式 `null` = 移除 `provider.temperature` 行，回到供应商缺省。**不得**把缺省读成清除，也**不得**把 `null` 当作 `0`。移除只在字段存在时改动文件；字段本就不存在时该补丁是空操作，ETag 不变。
- `changes.bindings`：数组 `[{resource_kind, ref_name, resource_uid}]`，用于表单选择资源后同步写入 `resource_bindings`。`resource_kind` 取 `model_connection` / `notify_channel`；服务端在同一写边界内把它合并进 frontmatter 的对应块，**不**按名称重新解析 UID。
- **保存期绑定校验**（与原文保存共用）：合并后的 `resource_bindings` 必须与 `provider.name` / `notify_channels` 一一对应；每条 `{name, uid}` 的 UID 必须存在、是当前发布指针、已发布、有资格（共享或本 Agent 专属）；不得引用已删除 UID、其他 Agent 的专属资源或未发布版本；不得有孤儿绑定或重复名。任一不满足 → **400** 并点名字段，零写入。
- 原文保存**必须自带完整 `resource_bindings`**；服务端**不补写**、不按名解析、不猜意图。缺绑定或绑定与引用不一致 → 400，不落盘。
- `resource_bindings` 写在 frontmatter，因此被 `AGENT.md` 原始字节哈希覆盖：重选资源会改变 `content_etag`，不另设第二套绑定 ETag。
- 新增 `GET /agents/{name}/config`：返回结构化字段值、**一个**与 `GET /agents/{name}/content` 完全一致的强 `content_etag`，以及每个引用的解析状态（绑定 `resource_uid`、名称、是否已删除、是否仍有资格、`notify_mode` 的有效值）。它不复用 `GET /agents/{name}` 的归档视图，也不替代原文读取。
- 两种形态最终都走**同一段**解析、校验与发布闸门（先验后写）；成功后生成新的 `ConfigSnapshot`，返回新 ETag 与落盘真值。
- `disabled` Agent 允许编辑；`archived` Agent 不允许直接编辑，必须先 restore。
- 服务端不得存在绕过该闸门的第二条 `AGENT.md` 写入路径（页面编辑器、创建、多文件提交、技能导入、恢复共用它）。

### 2.4 技能导入、发布与运行冻结

- **按名绑定**：`skills:` 只写技能名；**不支持** `skill@version`，也不得用软连接 / 本地目录另立绑定。
- **发布版本**：技能库由工作副本与不可变版本库组成。工作副本经校验 / 发布闸门生成不可变版本；`resource_pointers` 记录当前发布版本。新 Run 在准入时解析当前发布版本，并冻结完整 L1/L2/L3 文件清单（`RunResourceManifest`）；Run 内按需读取时从冻结清单取，工作副本后续修改不影响本 Run。仍是渐进披露——**不得**预载正文、**不得**新增 `use_skill` 工具。
- **缺失基线**：技能没有已发布版本时，不得把工作副本静默当作已发布版本；该 Agent 不注册，直到建立基线 / 发布。
- **导入边界**：
  - 只接受相对路径；规范化后必须仍在技能目录内；拒绝绝对路径、`..` 越界、软连接 / 目录联接、非普通文件；
  - 单文件 ≤ 256 KiB，单技能总量 ≤ 2 MiB，文件数 ≤ 200；
  - 导入时**不执行**任何脚本，不解析或下载外部依赖；
  - `.git` 等隐藏目录不导入；同名技能存在时导入失败，不静默覆盖。
- 版本撤回 = 用旧版本内容创建新版本并发布；不回退版本号，不改历史版本，不恢复已撤销的凭据解析值。

### 2.5 资源绑定结构（`resource_bindings`）

```yaml
resource_bindings:
  model_connection:
    - name: deepseek
      uid: rs_01J8Z6YQ4M2S3T7V9W0X1Y2Z3A
  notify_channel:
    - name: ops-webhook
      uid: rs_01J8Z7A1B2C3D4E5F6G7H8J9K0
```

| 键 | 类型 | 说明 |
| --- | --- | --- |
| `model_connection` | []object | 模型连接绑定；与 `provider.name` 严格一一对应（当前配置中每个 Agent 恰有一个 provider，故恰一条） |
| `notify_channel` | []object | 通知渠道绑定；仅在 `notify_mode: allowlist` 时非空，且与 `notify_channels` 严格一一对应 |
| 元素 `name` | string | 资源当前名称；只用于展示与对照，**不**作为身份 |
| 元素 `uid` | string | 稳定资源身份（`rs_` 前缀）；由服务端在创建资源时分配，**不得**手填、不得改写 |

- **权威与投影**：`AGENT.md` frontmatter 是绑定唯一权威；SQLite `agent_resource_bindings` 是可重建投影，供列表与引用视图查询，不作为写入口。
- **身份优先于名称**：同名资源删除后重建会得到**新 `resource_uid`**，旧绑定继续指向旧 UID（墓碑可查），**不自动接管**新资源。要改用新资源必须显式重新选择并保存。
- **不自动补绑**：迁移、复制、恢复、启动、Run 准入、原文保存都**不得**因为「名称当前能解析到某个资源」而写入或改写绑定。只有三个入口可以产生绑定：① 新建模板由服务端写入创建时选定的资源；② 表单显式选择（`changes.bindings`）；③ 原文显式写出。
- **随配置流转**：复制、归档、恢复、导出携带绑定（UID 不变）；复制遇到源 Agent 专属资源时**阻断并点名**，要求显式映射（不提供「一键解除」），副本获得新 `agent_uid`，不复制源凭据。
- **引用已删资源**：绑定仍指向旧 UID；Agent 可加载（加载期不判完整性），但**保存与准入**点名要求重新选择。恢复或改名不使旧 UID 复活。

### 2.6 通知工具的受控装配（仅 notify）

**唯一受控例外**：除 `notify` 外，系统**不得**自动增删 `tools` 中的任何工具名。`notify` 的装配结果必须与通知意图一致，并作为可见变更回报，不得静默改写：

| `notify_mode` | 渠道资格 | `tools` 中的 `notify` | 保存响应 |
| --- | --- | --- | --- |
| `none` | 不适用 | **必须不装配**（原文件有则移除） | `changes.tools_removed` 含 `notify`（仅在确实移除时） |
| `all_eligible` | 至少一个渠道有资格 | **必须装配**（原文件没有则补上） | `changes.tools_added` 含 `notify`（仅在确实补上时） |
| `allowlist` | 声明的每个渠道都有资格 | **必须装配** | 同上 |
| `allowlist` | 任一渠道无资格 / 未绑定 | **不装配**，保持原工具面 | 由装载 / 绑定闸门报错点名，不静默改写 |
| 非法取值 | 不适用 | 不改写 | 交给既有 `invalid notify_mode` 校验 |

- **保存期**：装配在条件写提交锁内进行；只在需要增删时改 `tools` 列表，`tools_added` / `tools_removed` 随 `AgentFilesView.changes` 返回，供页面展示。
- **装载期**：以文件为准；不因通知意图回写文件。缺 `notify` 但 `notify_mode` 非 `none` 时按加载规则处理并在页面提示，下一次保存才装配。
- **系统代投接管**：任务配置了 `deliver_to` 时，该轮 Run 在准入阶段固定 `notify_mode = none` 并从工具面剔除 `notify`（授权层仍兜底拒绝）；模型只返回正文，由系统在成功后发送。此规则**不**改写 `AGENT.md`。
- **单一存储**：装配只改 `tools` 列表，不新建通知配置表；渠道权限边界仍由 `notify_mode` + `notify_channels` + `resource_bindings.notify_channel` 决定。

---

## 3. schedules 段（定时任务）

| 字段 | 类型 | 必填 | 缺省 | 说明 |
| --- | --- | --- | --- | --- |
| `id` | string | 否（单项可缺省） | `<agent>-1`（仅当该 Agent 恰有一个任务时派生） | **两项及以上每项必须显式声明**（否则列表重排会静默改变任务身份）；全局唯一，显式撞车 = 该 Agent 不注册；给派生任务补显式 `id` = 换身份（旧状态不跟随） |
| `cron` | string | **是** | — | **6 段**：秒 分 时 日 月 周（`0 0 8 * * *` = 每天 8 点）；5 段拒 |
| `zone` | string | 否 | `UTC` | IANA 时区名；**不得**回落服务器本地时区 |
| `message` | string | **是** | — | 到点发给该 Agent 的消息（等价于人推的一条用户消息） |
| `deliver_to` | string | 否 | `""` | 系统代投目标：通知渠道的 **`resource_uid`**（`rs_` 前缀；**不写渠道名**）。空串 = 不代投。保存时校验渠道存在、属于该 Agent `notify` 声明范围且当前有资格；运行开始时再次按 UID 校验并**冻结**目标，同名重建不自动接管。配置后这一轮运行由系统在成功后发送正文，不向模型提供 `notify`，见 §3.2 |

### 3.1 cron 支持语法与时区

**6 段制式不代表所有解析器语法等价**。Basalt 的支持范围与样例约束如下：

| 语法 | 支持 / 样例约束 | 说明 |
| --- | --- | --- |
| `*` 通配 | 是 | |
| 数字 `5` | 是 | |
| 范围 `1-5` | 是 | |
| 列表 `1,3,5` | 是 | |
| 步长 `*/5`、`1-30/5` | 是 | |
| 月/周缩写 `JAN`、`MON` | 是 | 按解析器支持的名称填写 |
| 星期 `7` | **否** | 合法域为 0–6；Sunday **用 0** |
| `L`（最后一天） | **否** | 当前解析器不支持 |
| `W`（最近工作日） | **否** | 当前解析器不支持 |
| `#`（第 N 个星期几） | **否** | 当前解析器不支持 |
| **`dom` 与 `dow` 同时被限定** | **不纳入样例** | 解析器取**并集**（OR，任一满足即触发）；样例**硬性规则：至少一个是裸 `*`** |

**每任务时区**：Basalt 侧经 `CRON_TZ=<zone>` 前缀实现（`robfig/cron` v3.0.1 无 `parser.WithLocation`，时区是**调度器级** `cron.WithLocation` 或**任务级** `TZ=`/`CRON_TZ=` 前缀）。装载时把 `zone` 字段转成前缀，公开配置仍使用 `zone` 字段。

**验收样例**（从**固定起点** 2026-10-01T00:00:00Z 计算未来 10 次触发，断言时间与预期；**不得**用“当前时间”作起点）：

```text
cron = "0 0 8 * * *"     zone = Asia/Shanghai   → 每天 08:00:00 +08:00
cron = "0 30 23 * * 0"   zone = UTC             → 每周日 23:30:00 UTC
cron = "0 0 */6 * * *"   zone = Asia/Shanghai   → 每 6 小时整点
```

**歧义防护例（不得写进公开样例 `AGENT.md`）**：`0 15 9 1-7 * 1`（Asia/Shanghai）同时限定日与星期，必须按 OR 语义理解，不能误读成“每月首个周一”。以 2026-10-01 北京时间零点为起点，首个预期触发是当日 09:15；实现读数需由解析器测试确认。跨实现差异与对拍程序只在 [Compatibility.md](../Compatibility.md) §5.2 维护。

DST 边界例（`America/New_York`，2027-03-14 跳 02:00–03:00）：`0 30 2 * * *` 在该日的行为**必须实测、不得推断**；跳过和重复时段分别留证，固定预期后再纳入回归。兼容性结论不由本节的单侧读数自动推导。

### 3.2 结果投递（deliver_to）

`deliver_to` 是**系统代投**，不是模型工具调用。语义固定如下：

1. **目标身份**：值为渠道 `resource_uid`；保存时校验渠道存在、在 `notify` 声明范围内且有资格；运行开始时按 UID 再校验并冻结，运行中途改绑定 / 重建同名渠道**不**改变该轮目标。校验失败 → 该轮在发模型调用**之前**失败并留记录，不以模型结果兜底。
2. **只发成功结果**：仅当本轮执行状态为「完成」才发送；取消 / 失败 / 超时不发送，投递记录为 `skipped`（或按恢复规则记为 `unknown`）。
3. **先留痕再发送**：发送前写 `schedule_deliveries` 的 `pending` 行，与运行共用 `run_id`；系统投递**只认这条权威记录**，不伪造 `notify` 工具调用，也不写模型工具审计。发送成功后写终态；写终态失败的行由启动恢复处理。
4. **三值结论**：`sent`（HTTP 2xx；`feishu` 还要求业务回执 `code == 0`）/ `failed`（确定失败）/ `unknown`（超时、回执不可解析）。**未知不得当作成功，也不得自动重发**；`skipped` 表示按规则未发送。
5. **系统接管**：该轮 Run 准入时固定 `notify_mode = none` 并向模型剔除 `notify`（授权层兜底拒绝），避免模型与系统各发一次。此接管**不**改写 `AGENT.md`。
6. **分开展示**：运行执行状态与投递状态是两条记录；视图以 `delivery_status` / `delivery_channel` / `delivery_detail` 投影，`delivery_status` 为空串表示**没有投递记录**，不得渲染成成功。

正文格式固定为 `【<agent>】定时任务 <taskId> 执行完成` 加本轮最终文本；无最终文本时写明未产生文本。发送使用与 `notify` 相同的渠道适配器与凭据解析路径。

---

## 4. 完整消息模型

会话历史**不是** `(role, content)` 二元组——工具调用轮次的完整结构必须原样保留并回传，否则供应商侧会直接报错。

### 4.1 消息结构

```go
type Message struct {
    Role              string          `json:"role"`                          // system / user / assistant / tool
    Content           string          `json:"content"`
    ToolCalls         []ToolCall      `json:"tool_calls,omitempty"`          // assistant 发起：每个含 ID / Name / Arguments
    ToolCallID        string          `json:"tool_call_id,omitempty"`        // tool 角色：回填哪次调用
    ReasoningContent  string          `json:"reasoning_content,omitempty"`   // 供应商扩展：思考过程
    ProviderExtra     map[string]any  `json:"provider_extra,omitempty"`      // 供应商扩展字段的透传位
}

type ToolCall struct {
    ID        string `json:"id"`         // 模型生成的调用标识，必须原样回传
    Name      string `json:"name"`
    Arguments string `json:"arguments"`  // JSON 字符串（原样保存，不做二次序列化）
}
```

**三条硬约束**：

1. **`tool_call_id` 必须原样回传**：一个 assistant 消息里可能并列多个工具调用，结果必须以对应 ID 回填，否则供应商无法匹配
2. **`reasoning_content` 必须回传**（DeepSeek 等思考模式模型）：带工具调用的轮次若丢掉该字段，供应商返回 **HTTP 400**；且历史被"洗干净"后**整个会话持续 400**——一旦发生，会话不可恢复
3. **`ProviderExtra` 原样透传**：不认识但供应商发来的字段不能在序列化往返中丢失

### 4.2 历史裁剪：按完整工具交互组

裁剪的**最小单位是"完整工具交互组"**（一个 assistant 消息 + 其全部 tool 结果消息），**不得**只丢一半：

```text
正确：丢掉 [assistant(tool_calls=[A,B])] + [tool(A)] + [tool(B)]
错误：只丢 [tool(A)]，留下 [assistant(tool_calls=[A,B])] + [tool(B)]
      → 供应商报"tool_call 无对应结果"，HTTP 400
```

裁剪顺序：从最早的组开始整组丢弃，直到满足 `max_history_turns` 与 token 预算。**核心记忆区永不裁剪**（见技术方案 §3.5）。

---

## 5. 内置工具契约（11 个）

工具名与参数名是**模型可见契约**：改字面量等于改调用方式。

**扩展标注约定**：表格“必填”列带 **扩展** 的行表示可选增强参数，不传时使用已定义缺省值。该标记保留现有契约分类；它不授权修改其他参数的含义，例如 `timeout_seconds` 不得改变 `arguments` 的传递方式。

### 5.1 FileTools（3 个）

#### read_file

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 是 | 文件路径（绝对或相对工作区根） |

返回：文件内容（UTF-8 宽容解码：非法字节以替换字符呈现，**不**因编码拒绝读取）。
失败：`not_found`（文件不存在）/ `sandbox_denied`（路径越界）/ `runtime_error`（不可读）。
上限：单文件 256 KB，超出返回可读错误（提示用 `shell` 工具处理大文件）。

**读范围**：当前 Agent 自己的目录 + `.basalt/skills`（技能 L2 必需）。**跨 Agent 读取默认被拒**——读别的 Agent 目录需管理员在 `sandbox.cross_agent_read` 显式列出该 Agent 名（缺省空 = 一个都不许），技术方案 §4.3。

#### write_file

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 是 | 目标路径 |
| `content` | string | 是 | 写入内容（覆盖写） |

返回：写入字节数。
失败：`sandbox_denied`（越界、或目标是 `AGENT.md` / 技能库与 `scripts/` **只读区**）。
约束：写入范围 = **当前 Agent 自己的目录**（详见技术方案 §4.3），其中 `scripts/` 子目录**只读**（代码常量拒绝——脚本受控通道的前提是模型改不动脚本，见 §5.2）；父目录不存在时**不**自动创建。

#### list_dir

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `path` | string | 是 | 目录路径 |

返回：条目列表（每行：类型 / 大小 / 名字）；不递归、不跟随软连接。
失败：`not_found` / `sandbox_denied`。

### 5.2 ShellTools（1 个）

#### shell

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `executable` | string | 是 | **可执行文件名**，与 `shell.allowed_commands` **整串逐字**比对（`"git status"` 这类多词串必拒） |
| `arguments` | []string | 否 | 参数数组，逐项原样传递 |
| `timeout_seconds` | int | 否（**扩展**） | 超时（缺省 30 秒）；超时杀**进程树**。基线无此参数，缺省行为与基线一致 |

返回：退出码 + stdout + stderr（合并截断到 256 KB）。
失败：`sandbox_denied`（可执行名不在白名单、解释器形态不合法、或脚本路径不在允许区）/ `timeout` / `runtime_error`（启动失败）。

**argv 契约（不得违反）**：`executable` 与 `arguments` **分开传**，进程 argv 直执、**不经任何 shell 解释**。**不得**传 `bash -c "……"`，也**不得**把参数拼进可执行名——`git status; curl evil.com` 这类串接在 argv 契约下不可能执行。
工作目录：有 Agent 上下文且目录存在 → 该 Agent 自己的目录；无上下文 → 进程默认 CWD。
环境变量：只透传白名单变量（见技术方案 §4.3），**不**继承调用进程完整环境。

**解释器受控通道（`shell.interpreters`）**：解释器（`python3` / `node` 等）**可以**被放行，但形态受硬约束——argv **必须**是 `[<脚本路径>, <参数…>]`：`arguments[0]` 是脚本路径、**不得**以 `-` 开头（`-c` / `-m` / `-e` 这类内联代码形态一律拒），脚本的真实路径（软连接投影后）**必须**落在该 Agent 的 `scripts/` 或 `.basalt/skills/` 内。`scripts/` 对模型 `write_file` **只读**（代码常量拒绝）——脚本约束的前提是模型改不动脚本。`bash` / `sh` **不得**放行（等于把解释层请回来）。解释器进程拥有**宿主同等权限**，容器化隔离属扩展阶段（见 Roadmap.md F 方向）。

### 5.3 HttpTools（2 个）

#### http_get

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `url` | string | 是 | 目标 URL |
| `headers` | object | 否（**扩展**） | 附加请求头 |

返回：状态码 + 响应体（截断到 256 KB）。
失败：`sandbox_denied`（域名不在网络域名白名单 / scheme 非 http/https / **重定向目标越界** / **跳数超过 5**）/ `timeout` / `external_error`（4xx/5xx）。

**重定向与目标形态**（两个 http 工具共有）：目标地址按**逐跳**校验——每一跳在**发送前**过域名白名单与 scheme 判定，HTTP 客户端**不得**自行跟随；最多 5 跳，超限或越界一律判 `sandbox_denied`（不是 `external_error`）。白名单条目与目标 host **必须是域名**：IP 字面量目标直接拒（白名单按域名匹配，放行 IP 等于绕过它）。

#### http_post

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `url` | string | 是 | 目标 URL |
| `body` | string | 是 | 请求体（**必填，与基线一致**） |
| `content_type` | string | 否（**扩展**） | 缺省 `application/json` |
| `headers` | object | 否（**扩展**） | 附加请求头 |

返回 / 失败：同 `http_get`。
注意：`http_post` 是**有外部副作用**的调用。承诺边界**逐字**见 [run-and-errors.md](run-and-errors.md) §3.3——运行时不自动重试、崩溃恢复不自动重放；但**客户端**（curl / 浏览器 / 调用方程序）在超时后重发同一请求，服务端**无法识别**是重试还是新意图，会照常执行（v0.1 不做幂等键）。

### 5.4 MemoryTools（2 个）

#### save_memory

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `content` | string | 是 | 要记住的内容 |
| `scope` | string | 否 | `CORE`（核心记忆）/ `ARCHIVAL`（归档记忆）；**省略 = `ARCHIVAL`**。与基线逐字一致 |

返回：写入确认（含作用域：Agent 层 / 全局层）。
失败：`invalid_input`（scope 非法，合法取值仅 `CORE` / `ARCHIVAL`）/ `runtime_error`；**超配额返回可读拒绝**（单条 ≤ 4 KB、每 Agent 核心区 ≤ 32 KB），提示改写归档区。
作用域：有 Agent 上下文 → 该 Agent 的 `memory/agents/<name>/MEMORY.md`；无上下文 → 全局 `memory/MEMORY.md`。

> **两个 `scope` 不是一回事（防混淆，必须看清）**：本工具的 `scope` 参数取值是 `CORE` / `ARCHIVAL`，指的是**记忆分区**（落 `memory_entries.section` 列，值 `core` / `archive`）；而 `memory_entries.scope` **列**指的是**记忆分层**（`agent` / `global`）。二者是两条正交维度：分区由模型选，分层由运行时按上下文决定（有 Agent 上下文 = Agent 层，无 = 全局层）。列名以 [schema.sql](schema.sql) 为唯一事实源。

#### recall_memory

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `query` | string | 是 | **单个关键词** |

返回：命中的归档区条目（核心区不参与检索——它每轮已完整注入）。
失败：无命中返回成功 + 空结果（不报错）。
**契约语义**：按整串 `contains` 匹配，**不分词、不扩展、不向量化**。命中条件是**连续子串**：条目原文里含该连续短语（条目写着 `SQLite 方案 评估`）**会命中**；几个词分散在条目各处、只是"都出现过"（该条分别含 `SQLite` 与 `评估`，但从不连排）**不会**自动组合命中——这是已知行为，不是缺陷。参数仍要求**单个关键词**。

> **2026-10-17 澄清**：本节此前写作"传多词短语**必不命中**"，与 `contains` 语义不符（连续短语在 `strings.Contains` / `instr` 下必然命中）。语义以本段为准：**整串子串匹配、不分词不扩词**；连续短语能命中、分散词不自动组合。同一句在 [Compatibility.md](../Compatibility.md) §2.2、[DevelopmentLoop.md](../DevelopmentLoop.md) §7 陷阱 11、[Roadmap.md](../Roadmap.md) B 方向与 M2 已同步改写；渲染测试**不得**钉"多词必不命中"，改钉"连续短语命中 / 分散词不命中"两态。

### 5.5 NotifyTools（1 个）

#### notify

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `channel` | string | 否 | 通知渠道名（全局通知渠道表里的 `name`）。**缺省即失败**（不猜"默认渠道"——渠道由全局注册表管理，无默认概念），失败文案回填可用渠道名。必填性之所以标"否"：与基线一致，让模型不传时拿到可读失败而不是被供应商侧的 schema 校验拦下 |
| `content` | string | 是 | 推送内容（≤ 20 KB，超限**发送前**失败） |

返回：投递确认。
**可用范围**：Run 准入时按 `notify_mode` 冻结通知授权——`none` 不写任何渠道；`all_eligible` 枚举该 Agent 当前**有资格**的全部渠道并冻结具体 `channel_uid`；`allowlist` 只冻结声明的具体 `channel_uid`。工具**不得**在调用时回读文件，运行中途改声明不影响在途 Run。
**发送判定顺序**：① 先判模式——`none` → 固定错误 `notify_disabled`（固定短语「该 Agent 未启用通知（notify_mode: none）」），不查渠道、不投递；② 再查冻结集合——集合外的渠道按 `not_found`（文案说清是声明限制）；③ 再查渠道当前范围 / 状态（已删除、已转为他人专属、凭据已撤销 → `not_found` 或 `notify_disabled` 的固定短语）；④ 解析当前 `active` 凭据版本后投递。
失败：`notify_disabled`（`notify_mode: none` 或渠道凭据已撤销）/ `not_found`（渠道不存在、已删除或不在本 Agent 的冻结集合内）/ `sandbox_denied`（渠道域名不在白名单）/ `external_error`（投递失败）。
**投递判定**：按渠道类型分派适配器——`webhook` 看 HTTP 状态码；`feishu` **只认响应体 `code == 0`**（投递失败也回 HTTP 200，只看状态码会把"没送达"记成成功）。回执解析不出来同样判失败（"结果未知"**不得**冒充成功）。

### 5.6 ManageScheduleTools（1 个）

#### manage_schedule

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `action` | string | 是 | `set` / `remove` / `list` |
| `id` | string | 否 | 任务 id；`remove` 必填；`set` 省略时仅当该 Agent **恰有一个任务**时生效，定位到**该现有任务的实际 id**（如唯一任务叫 `daily-report` 就更新它，不是 `<agent>-1`）；任务数为 0 或多个时**必须**显式给 id（与 §3 规则一致）。`<agent>-1` **仅用于**加载期对原始配置缺省 id 的规范化 |
| `cron` | string | 否 | 6 段表达式；`set` 必填 |
| `zone` | string | 否 | 缺省 `UTC` |
| `message` | string | 否 | 到点发给自己的消息；`set` 必填 |

返回：`list` 返回全部任务；`set` / `remove` 返回确认（含 id）。
失败：`invalid_input`（action 非法、必填缺失、id 不属于自己）/ `not_found`。

**作用域硬边界**：只能改**当前 Agent 自己**的 `schedules:` 段（身份取自运行时上下文，**不**来自入参）；无 Agent 上下文一律拒绝。改动经与页面编辑器、脚手架创建**同一道**注册写入门（先验后写），不另立解析路径。

> 该工具属于 Basalt v0.1 的任务管理能力：只修改当前 Agent 的 `schedules:`，与页面编辑器共用写入校验。声明该工具但注册池没有它时，该 Agent 以 `tool not registered` 整只不注册。

### 5.7 SearchTools（1 个）

#### web_search

| 参数 | 类型 | 必填 | 说明 |
| --- | --- | --- | --- |
| `query` | string | 是 | 搜索词：1–128 个 Unicode 字符，**不得全为空白**（去空白后判）；支持官方查询运算符（`site:` / `-site:` / `lang:` / `after:` / `before:` 等），工具不解析、原样转发 |
| `count` | int | 否 | 返回条数：1–50，**省略 = 10**（与官方缺省一致）；超出区间按 `invalid_input` 拒绝，不静默夹取 |

返回：JSON 正文 `{"query": "<回显的查询原文>", "results": [...]}`，`results` **恒为数组**
（无命中写 `[]`，不写 `null`）。每条结果**七个字段恒在**：`title` / `url` / `snippet` /
`summary` / `date`（官方 `date` 为 `null` 时写空串，**不**补日期）/ `rerank_score` /
`authority`（`-1` = 官方未给出）。
失败：`invalid_input`（入参解码 / 取值不合规，**零外呼、零凭据解析**）/ `sandbox_denied`
（目标域名未放行、或受控拨号在**建连之前**拒绝）/ `timeout`（请求或重发退避期间 ctx 结束）/
`external_error`（非 2xx、响应超 1 MiB、响应解析失败、传输失败）。

**凭据与判定（不得违反）**：

- **凭据只经既有凭据解析服务**：取该 Agent **已绑定的模型连接**当前凭据（当前实现为
  DeepSeek 连接的 Key），每次调用重新解析、**不缓存**；参数面**不接受**密钥，
  **不得**新增第二条读取路径。凭据缺失 / 解析为空一律失败关闭。
- **判定归 `core.Policy`**：动作类型为 HTTP 请求，目标恒为编译期常量端点
  `https://api.deepseek.com/v1/tools/search`；**每次外呼各判一次**（重发也算一次）。
  严格档需在域名白名单放行 `api.deepseek.com`；标准档按既有档位语义放行。
- **请求体固定**：`{query, count, need_content: false}`——本工具只给摘要与链接，正文
  由 `http_get` 或 Fetch 等抓取通道另取，工具不代抓、不返回正文。
- **有界重试**：仅 408 / 429 / 5xx 重发，**最多 2 次**（三次尝试），退避 200 ms 起、
  逐次翻倍且受 ctx 约束；401 / 402 / 403 / 404 及其余 4xx **不重试**。
- **计次与审计**：一次 `web_search` 调用 = 一次工具调用 = **一条**审计行（重发不另计），
  与其余内置工具共用 `pending` → 终态的两阶段写；结果进入 `max_output_bytes` 截断。

> 本工具是 US-14 的 14D 增量（2026-10-26 裁决）。字段名与错误码的官方核对记录见
> `specs/014-templates-demos-release/run-state.md`。官方限流口径以官方文档为准；
> 契约不声明 QPS 数值。

---

## 6. 拒绝信息格式（模型可见契约）

沙箱拒绝的返回文本是**四段式**，逐字保真：

```text
<规则>：<越界值>。这是安全策略，请勿反复重试；<放行指引>
```

例：

```text
域名不在白名单：evil.example.com。这是安全策略，请勿反复重试；如需访问，请联系管理员在管理台「执行环境」页把该域名加入网络域名白名单，保存后立即生效，无需重启。
```

指引**必须**给出该档真实的放行入口与生效方式——模型据此停止重试并转告人类。域名档的入口是管理台「执行环境」页的**网络域名白名单**：管理员保存后立即生效、不需要重启（`basalt.yaml` 的 `http.allowed_domains` 只在首次启动、数据库无策略行时作种子，此后改配置不覆盖管理台写入的值）。文件档的指引分两种：

- **跨 Agent 读取被拒** → 点名 `sandbox.cross_agent_read`（授权后需重启）
- **工作区之外的路径** → 可读根由工作区约定固定（当前 Agent 自己的目录 + `.basalt/skills`），**没有配置键可以加**；指引如实说明，**不得**点名不存在的配置键

这条文案与普通错误（面向运维）的读者不同，**不得**合并。

---

## 7. 跨文档指引

| 主题 | 文档 |
| --- | --- |
| Run 状态机 / 并发 / 取消 / 崩溃恢复 / 错误码 | [run-and-errors.md](run-and-errors.md) |
| 管理面认证 / 条件写 / 管理操作审计与恢复 | [run-and-errors.md](run-and-errors.md) §9 |
| 数据库结构（会话 / 消息 / 审计 / 快照 / 资源版本 / 生命周期索引） | [schema.sql](schema.sql) |
| REST 端点与信封 | [openapi.yaml](openapi.yaml) |
| 兼容性、迁移与历史对照 | [Compatibility.md](../Compatibility.md) |
| 沙箱与授权边界（Policy / Runner） | [技术方案](../TechnicalDesign.md) §4.3 |
