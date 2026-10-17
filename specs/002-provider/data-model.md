# Phase 1 数据模型：US-2 Provider 对接（能力一）

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-16

本 feature 的「数据」共六类：**供应商配置条目**（配置文件）、**显式映射表**（进程内、装配时一次构建）、**调用请求 / 响应**（进程内值传递）、**审计行**（SQLite，本 US 首次写入）、**失败类别**（固定字面量）、**错误面**（哨兵 + 安全调用错误）。表 / 列 / 状态 / 类别字面量的唯一事实源是 [docs/contracts/schema.sql](../../docs/contracts/schema.sql) 与 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md)；本文件**不抄列清单**，只记实体关系、状态机与校验规则。

## 实体 1：供应商配置条目（Provider entry）

**载体**：`basalt.yaml` 的 `providers:` 段（与工作区同级；`basalt init` 不生成实例配置，仓库只带样例）。

| 成员 | 类型 | 判据（可测） |
| --- | --- | --- |
| `name` | 字符串 | **必填**；**段内唯一**——重名 → 装载期拒绝并点名**两条目下标**（否则映射静默保留最后一条、缺条目无人发现） |
| `base_url` | 字符串 | **必填**（本 US 新增规则）；只支持 scheme（`http` / `https`）+ host + path，**原样保留**（path 可为空）；含 userinfo / query / fragment → 装载期拒绝；缺 scheme **不代补**；缺失 / 非法 → 文案只点名 `providers[i].base_url` 与不支持的成分名，**不回显原 URL 与其中的值** |
| `api_key` | 字符串 | `${VAR}` 占位形态（US-1 已交付的解析与四类拒绝）；解析后的**真实值只进客户端**——不进日志 / 错误 / 审计 / 命令输出 |
| `default_model` | 字符串 | 可空——空表示「本供应商无缺省模型」，模型解析落到请求侧；解析仍为空 → 点名 `provider` 拒绝 |

**校验规则**：三条新规则（重名 / `base_url` 必填 / `base_url` 成分拒绝）并入 US-1 既有六类 fail-closed，**不改既有六类的语义与文案风格**；一切启动路径（含 `provider list`）同判。

**关联**：条目是**显式映射表**的构造来源（逐条、有序），条目名是映射表的键。

## 实体 2：显式映射表（clients + models）——构建契约

**载体**：进程内两个 map，装配时**一次构建**、之后**只读**（并发安全的前提；配置改动重启生效，热加载归 US-11）。

| map | 键 | 值 | 构建方式（可 grep 判卷） |
| --- | --- | --- | --- |
| `clients` | 条目 `name` | `core.ProviderClient`（本 US 为 `*provider.OpenAIClient`） | **逐条**由配置构造（遍历的是配置**数组**——有序显式）；构造失败即整体失败并点名 `providers[i]` |
| `models` | 条目 `name` | `default_model` | 同一循环内同步写入 |

**不变量（每条都是可执行判据）**：

| # | 不变量 | 判据 |
| --- | --- | --- |
| 1 | 键集**逐字** = 配置条目名集 | `TestNewProviderClients_ExplicitMapMatchesConfig`（无「默认供应商」、无多余键） |
| 2 | 路由**只查表** | `s.clients[req.Provider]`；装配处与路由处**无**任何 `range` 选择 / 推断 / 「取第一个」形态（宪法 III 的复核方式） |
| 3 | 查表不中 → 点名拒绝 | 返回 `core.ErrProviderNotConfigured`（含 provider 名）、**零调用、零审计行** |

## 实体 3：调用请求（ChatRequest）

**载体**：进程内值传递（显式类型化参数，**不进 context**——宪法 VII）。两组字段的读者不同：

| 组 | 字段 | 消费者 | 约束 |
| --- | --- | --- | --- |
| 路由与审计关联 | `Provider` / `RunID` / `SessionID` / `Round` / `ConfigHash` | `ProviderService` | `Provider` 必须是映射表的键；`RunID` 空或 `Round < 1` → **拒绝发出且不写审计行**（用法错误，非调用失败）；`SessionID` 可空；`ConfigHash` 本 US 恒空（列写 NULL） |
| 协议内容 | `Model` / `Messages` | `ProviderClient` | `Model` 空 → 用供应商缺省模型；消息模型的字段与 JSON 名**逐字保真**（`tool_calls` / `tool_call_id` / `reasoning_content` / 扩展透传位） |

**模型解析顺序**：请求 `Model` > 供应商缺省模型；两者皆空 → **点名 `provider` 拒绝、零调用、零审计行**；生效模型落审计行。

**工具字段占位**：`Tools []ToolSpec` 本 US **不预造**（类型与组装归 US-4——颗粒度文档 §3.5 占位 1）。

## 实体 4：调用响应（ChatResponse）——可空兜底矩阵

| 字段 | 可空性 | 兜底判据 |
| --- | --- | --- |
| `Content` | 带工具调用时可空 | 只透传，**不判失败** |
| `ToolCalls` | 可空 | 只透传；**执行权归 US-4 的 `ToolExecutor`**（本 US 不执行、不解释参数） |
| `ReasoningContent` | 可空但**不得丢** | 原样回到调用方；两跳用例逐字断言 |
| `ProviderExtra` | 可空 | 协议未在消息模型显式建模的未知字段：出站**平铺** / 入站**收集**（两端保真，经**两跳**断言） |
| `Usage` | **可空** | 供应商未返回 → 三列 0、**仍判成功**（`usage` 可空不是失败）；三 token 列显式平铺（列是平的，结构体也平——不嵌 `Usage`） |

## 实体 5：审计行（`llm_calls`）——状态机

**载体**：SQLite 表（US-1 已建齐，本 US **首次写入**）；列 / 状态字面量以契约文件为准；**唯一 SQL 入口** = `internal/storage/llm_calls.go`（审计 SQL 只出现在 `internal/storage`——全局不变量 3）。

**两个写入形态**（结构体即列的同构映射）：

| 形态 | 内容 | 状态列 |
| --- | --- | --- |
| **预写**（`BeginCall`） | 只含调用前可知字段（关联键 + 供应商 + 生效模型 + 快照 + 创建时间） | **不带状态字段**——由仓储的 SQL **固定写「进行中」**，不由调用方传入（从源头消除传错状态的可能） |
| **终态回填**（`FinishCall`） | 成败（只由状态表达，不设布尔列）+ 三 token 列 + 耗时 + 失败原文 + 失败类别 | 终态：成功 / 失败 + 类别 |

**状态机**：

```text
   BeginCall（SQL 固定写）
        │
        ▼
   ┌──────────┐   调用返回 + 终态回填成功       ┌───────────┐
   │ pending  │ ─────────────────────────────▶ │ success   │
   │（进行中）│                                └───────────┘
   │          │   调用失败 + 终态回填成功       ┌───────────┐
   │          │ ─────────────────────────────▶ │ failed    │（+ 失败类别）
   └────┬─────┘                                └───────────┘
        │
        │ 终态回填失败 → 只记 ERROR，行**留在 pending**（不伪造终态）
        │ 崩溃 / 强杀 → 持久化形态同样是 pending（无半写）
        │
        │ 启动恢复归一化（**归 US-4**，契约 §4.3.2 第 1 步）
        ▼
   ┌──────────┐
   │ unknown  │ ← 本 US 只保证窗口形态可被识别（pending 行 + 创建时间）
   └──────────┘
```

| # | 规则 | 判据（可测） |
| --- | --- | --- |
| 1 | **留痕先于发出** | 假 Provider 在调用**内部**当场查库：此刻该次调用的 `pending` 行必须已存在 |
| 2 | **预写失败 → 拒绝发出** | 预写失败 → 客户端被调用次数 = **0**、ERROR 日志、返回错误 |
| 3 | **终态只写一次** | 终态更新只对**仍处在进行中**的行生效；非进行中行 → 报错并点名该行 id、**不覆盖** |
| 4 | **终态回填失败不改写调用结果** | 行留 `pending` + ERROR 日志；**返回的仍是调用本身的错误**（取消路径亦然——取消链保留） |
| 5 | **两处写入与取消解耦** | 预写与终态回填都在剥除取消信号、独立 5 秒的上下文里执行；**调用前 ctx 已取消仍先落 `pending` 行**（终态 `failed` + `timeout`） |
| 6 | **失败原文截断** | 落库前截断 **4 KB** 且**不切断 UTF-8 字符**（两处独立：客户端出口 + 落库前） |
| 7 | **状态字面量固定写** | 预写行的状态列值恒为「进行中」——由仓储 SQL 写定，调用方无权传入 |
| 8 | **时间列语义** | 创建时间列存 Unix 毫秒 UTC（契约迁移纪律 4） |
| 9 | **无行 = 确定没执行** | 路由失败 / 关联缺失 / 模型不可解析（三道拒绝都在预写**之前**）→ 零审计行；反之凡发出过调用必有 `pending` 行；**不得**为未发出的调用合成审计行（会污染恢复判据） |

## 实体 6：失败类别（error_kind）与错误面

**本 US 取值面三类**（七类字面量的全集以 [docs/contracts/run-and-errors.md](../../docs/contracts/run-and-errors.md) §7.3 为准；类别常量定义在 `core`，后续 US 的工具侧沿用同一套七类）：

| 类别 | 触发 |
| --- | --- |
| `external_error` | 供应商 4xx / 5xx、网络不可达 / DNS / TLS、响应不可解析、3xx **未跟随** |
| `timeout` | 调用被取消或截止——两个上下文错误**归并**为 `timeout`，但**错误链区分保留**（`errors.Is` 仍可判具体原因） |
| `unknown` | **兜底**：既非客户端产出的安全错误、错误链上也无上下文错误的异常错误（如第三方实现抛回的裸错误）；**不是**恢复路径专属 |

**错误面（两类）**：

| 载体 | 语义 | 出口 |
| --- | --- | --- |
| `core.ErrProviderNotConfigured` | 请求的供应商不在显式映射表内（查表不中——**不**遍历推断、**不**取「唯一的那个」当默认） | `ProviderService.Chat`；对外映射位 = `NOT_CONFIGURED`（US-9） |
| `provider.CallError{Provider, Status, Kind, Message, err}` | 客户端边界产出的**安全调用错误**：文案已脱敏（抹 key 回显、截断 4 KB、不含请求 URL 与响应正文全文）、类别已判好、**原始链经 `Unwrap` 保留** | `OpenAIClient.Chat` 的四条返回路径全部经此出口 |

**判定链**：错误经 `errors.Is` / `errors.As` 判卷（测试以此断言，文案仅面向用户）；`errors.Is(err, context.Canceled / context.DeadlineExceeded)` 经 `CallError.Unwrap` 仍然成立。

## 实体关系总览

```text
basalt.yaml providers[]（实体 1，有序数组）
      │ 逐条构造（装配时一次）
      ▼
显式映射表 clients / models（实体 2，只读）
      │ 查表（键 = 请求的 Provider）
      ▼
ChatRequest（实体 3）──▶ ProviderClient.Chat ──▶ ChatResponse（实体 4）
      │                                              │
      │ 预写（先）                                    │ 终态回填（后）
      ▼                                              ▼
llm_calls 行：pending ────────────────────────▶ success / failed(+类别)（实体 5）
      （失败类别经实体 6 的分类器判定；失败原文两处截断）
```
