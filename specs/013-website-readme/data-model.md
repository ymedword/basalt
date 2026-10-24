# Data Model: US-13 官网展示主页与 README（对外门面）

**Date**: 2026-10-24 | **Plan**: [plan.md](./plan.md) | **Spec**: [spec.md](./spec.md)

本 US 没有数据库实体；这里的"数据"是站点内容与检查规则的结构化定义。所有字面量以 [docs/us/us13-website-readme.md](../../docs/us/us13-website-readme.md) R3 §3 为准（R2 为 review ① 基线，R3 按用户 2026-10-24 实施后确认同步），本文件只描述形状与校验。

## 1. 页面与路由（Page / Route）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | 枚举 | `home` / `quickstart` / `architecture` / `docs` |
| `locale` | 枚举 | `en` / `zh` |
| `path` | 字符串 | 英文 `/`、`/quickstart/`、`/architecture/`、`/docs/`；中文加 `/zh` 前缀 |
| `source` | 字符串 | 英文 `website/index.md` 或 `website/<id>/index.md`；中文 `website/zh/index.md` 或 `website/zh/<id>/index.md` |
| `title` / `description` | 字符串 | 每页唯一，双语各自完整 |
| `anchors` | 字符串集合 | 至少含该页被导航 / 页脚引用的显式锚点；英中同名 |

**路由全集**：

| id | EN | ZH |
| --- | --- | --- |
| home | `/` | `/zh/` |
| quickstart | `/quickstart/` | `/zh/quickstart/` |
| architecture | `/architecture/` | `/zh/architecture/` |
| docs | `/docs/` | `/zh/docs/` |

**校验规则**：

- 八个地址必须全部存在；缺任一页 → 内容检查失败，不得跳过。
- 每页必须有唯一 `title` / `description`；`lang` 与 locale 一致。
- 语言切换函数 `switchLocalePath(path, targetLocale, base)`：
  - 保留查询串与哈希；
  - 英文 → 中文加 `/zh`，中文 → 英文去 `/zh`；
  - base 前缀 `/basalt/` 先剥离、后还原；
  - 无法映射 → 目标语言首页（404 同规则）。

## 2. 能力（Capability）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | 字符串 | 稳定标识，如 `define` / `extend` / `schedule` / `remember` / `observe` / `self-host` |
| `layer` | 枚举 | `platform`（首页六项） / `core`（架构页五项） |
| `title` | `{en, zh}` | 中英标题 |
| `summary` | `{en, zh}` | 一句话说明 |
| `evidence` | 字符串 | 当前实现中的证据文件或机制；文件消失时检查失败 |
| `illustration` | 字符串或空 | 首页六卡的本地插图路径；核心能力无插图 |

**分层规则**：

- 首页六项 = 产品层平台能力，固定六张卡；每张有本地插图与双语 `alt`。
- 架构页五项 = 核心能力 Provider / ReAct / Memory / Tool / Web Service；与六类图配合表达。
- 架构统一采用接入层、应用层、执行引擎层、基础服务层、存储层；调度属于应用层，安全与审计贯穿各层；当前单节点实现与未来规划分开表述。
- 两层不得混写或强行凑成同一数量；未交付能力（协作、分布式、自动学习、向量检索、SSO / 多租户）不得作为现状出现。

## 3. 图（Diagram）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | 枚举 | `project-overview` / `system-architecture` / `execution-flow` / `module-map` / `package-dependencies` / `deployment` |
| `locale` | 枚举 | `en` / `zh` |
| `svg` | 路径 | `website/public/diagrams/<locale>/<id>.svg` |
| `sourcePage` | 字符串 | drawio 图源中的页名 |
| `sourceCommit` | 字符串 | SVG 内 `data-source-commit`，必须是当前 HEAD 的祖先 |
| `viewBox` | 字符串 | 同一 `id` 的英中 `viewBox` 必须一致 |
| `title` / `desc` | 字符串 | SVG 内 `<title>` / `<desc>`，双语各自可读 |

**校验规则**：

- 六类 ID 齐全、无多余、无改名；英中成对，缺一失败。
- drawio 图源恰好 12 页，每个 ID 出现两次。
- 架构页展示完整项目全景与全部专题图；两版 README 各只嵌 `system-architecture` 一张简化图，多嵌或少嵌都失败。
- 禁止 `<script>`、`<foreignObject>`、外部 `href` / `xlink:href`、CSS `url(http...)` 与 `@import`。
- `execution-flow` 的 `<desc>` 必须含 `completed` / `limit_reached` / `cancelled` / `failed` 与失败 / 拒绝 / 取消 / 恢复分支。
- 部署图只画当前单节点形态，不画 K8s / 容器现状；未来规划必须与当前实现视觉或文字分开。

## 4. 快速开始步骤（Quickstart Step）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | 枚举 | `config` / `init` / `create` / `serve` / `console` / `stop` / `chat` / `verify` / `commands` / `troubleshoot` / `deploy` |
| `command` | 字符串或空 | 真实命令；管理台步骤为空，由说明文字描述 |
| `prereq` | 字符串 | 前置条件（工作区、服务、模型、启用状态） |
| `expected` | 字符串 | 预期结果或界面状态 |
| `forbidden` | 字符串集合 | 不得出现的写法，如 `--profile`、`profile enable`、手改生命周期文件 |

**固定顺序**：最小 `basalt.yaml` → `basalt init` → `basalt profile create demo` → `basalt serve` → 管理台配置模型连接并启用 Agent → 停止 `serve` → `basalt chat --agent demo --message "Hello"` → `basalt session list`。

**校验规则**：

- 站点数据、README 标记块、首页 transcript 三处命令逐字一致。
- `registered=true` 必须解释为"定义与 provider 名已装载"，不等于已配置或可运行。
- 模型名占位符原文为 `请在此填写模型名`；公开材料不出现真实 key。
- 不写未测量的时长；`DURATION_MEASURED=false` 时禁用时长词。
- Windows 不假设 `openssl`：同时给 PowerShell 生成 token 的方法。

## 5. 演示记录（Demo Transcript）

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `capturedAt` | ISO-8601 | 采集当天 |
| `source` | 字符串 | `local-openai-compatible` |
| `stub` | 路径 | `website/demo/local-model.mjs` |
| `command` | 字符串 | 必须等于快速开始的 chat 命令 |
| `lines` | 字符串数组 | 与 stub 最终回复逐字一致；原始 CLI 输出，工具进度标签可为中文 |
| `exitCode` | 整数 | 0 |

**校验规则**：字段齐全；`command` 与 `QUICKSTART_COMMANDS` 逐字相等；`lines` 与 stub 最终文案一致（stub 使用 `read_file` 读取样例 README 后给出项目摘要）；stub 回复改变而 transcript 未更新 → 检查失败。页面必须显示脚本化标注，不得作为模型效果证据；展示层把工具进度标签译为英文，终端内容全英文且不随站点语言切换，原始 transcript 不改写。

## 6. 公开边界规则（Public Boundary Rule）

| 规则 | 内容 |
| --- | --- |
| 允许的 `docs/` 目标 | `docs/Operations.md`、`docs/contracts/**`、`docs/assets/**` |
| 禁止的路径前缀 | `career/`、`docs/interview/`、`docs/us/`、`harness/`、`.claude/`、`specs/`、`.specify/` |
| 禁止的文件 | `CLAUDE.md`、`.gitmessage`、`CLAUDE.local.md` |
| 外部链接 | 只允许真实仓库 / Issues 等显式 allowlist；无外部 CDN、第三方脚本、远程字体 |
| 空链接 | `href="#"` / `href=""` / `javascript:` 一律失败 |

**校验规则**：`check-links` 扫描构建后的 `dist/**/*.html`；`public_docs_test.go` 的 `operations-links` 子测试扫描公开 `docs/Operations.md`；两者无法互相替代。

## 7. 检查状态（Check Status）

| 状态 | 含义 |
| --- | --- |
| `passed` | 命令成功且断言通过 |
| `failed` | 断言失败 |
| `error` | 执行错误（环境、网络、工具缺失） |
| `not_run` | 未执行（必须写明原因） |

`not_run` 是 check 的合法运行时状态，但**不得**作为 review ② 的收口状态：收口前必须转为 `passed`，或附 CI `website` job 的通过日志。报告级另有 `running` / `source_changed`，不用于 check。
