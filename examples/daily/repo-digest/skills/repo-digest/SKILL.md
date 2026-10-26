---
name: repo-digest
description: 仓库日报技能——PR / Issue / Release 三源计数口径、待跟进与待确认写法、日报排版
---

# 仓库日报技能（L2 正文）

这份技能规定「日报怎么写」，**不负责算数**：所有数字来自脚本写出的 `digest.json`。
你负责读它、解释它、按下面的固定格式交付；**不得**心算、不得补造、不得改口径。

## 一、先读结论，再读细节

1. 先看 `message`：它是脚本给出的唯一结论句。`本次无新增…` 就是当天结论，
   日报不得加戏。
2. 再看 `sources` 的三块（`pull_requests` / `issues` / `releases`）：`total`（列表
   总数）、`new`（晚于游标的新条目）、`processed`（本次真正计入的条目）、`counts`
   （分桶计数）、`held_back`（读不出关键字段、未计入的条目）。
3. 最后看 `followups`（待跟进）与 `unconfirmed`（待确认）。

## 二、口径（照用，不得自行改写）

| 名称 | 含义 |
| --- | --- |
| `new` | 晚于游标（Release 用 `updated_at`，键缺失时用 `published_at`）的条目数；首跑没有游标，全部算新 |
| `processed` | 本次真正计入统计的条目数；恒等式 `processed + len(held_back) = new` |
| `held_back` | 连身份（编号）或时间游标（`updated_at`，Release 键缺失时用 `published_at`）都读不出来的条目：不计入统计，并且**挡住游标** |
| `with_unconfirmed_fields` | 已计入统计、但有业务字段无法确认的条目数 |
| `followups` | 按固定规则筛出的待跟进项（见第四节） |
| `unconfirmed` | 缺字段或字段解析失败的条目：`field` + `reason`（`missing` / `unparseable`） |

**「无法确认」不是「没有」**：字段缺失或类型不对 → 写「待确认」；显式 `null`
（如 `merged_at: null`）才表示「关闭且未合并」。两者不得混写、不得互相冒充。

**分类桶**（`counts` 的键）：

| 来源 | 桶 |
| --- | --- |
| PR | `open` / `draft_open` / `merged` / `closed_unmerged` / `unknown` |
| Issue | `open` / `closed` / `unknown` |
| Release | `published` / `prerelease` / `draft` / `unpublished` / `unknown` |

`unknown` = 关键字段读不出来（例如 closed PR 没有 `merged_at` 键、非草稿 release
的 `published_at` 不可解析）。看到 `unknown` 就去 `unconfirmed` 找对应条目，
**不得**把它并进相邻的桶里凑整。

## 三、日报排版（`digest.md`）

```text
# 仓库日报 · <仓库> · <observed_at 的日期>

结论：<message 原文>

## 计数
| 来源 | 新增 | 计入 | 待跟进 | 待确认字段 |
| --- | --- | --- | --- | --- |
| PR | … | … | … | … |
| Issue | … | … | … | … |
| Release | … | … | … | … |

## 待跟进
- [<来源> #<编号>] <标题> —— <原因>（<age_days> 天）<url>

## 待确认
- [<来源> #<编号>] <标题> —— 字段 `<field>`：<missing | unparseable>
```

规则：

- 「待跟进」/「待确认」没有内容时写「无」，**不要**编条目凑数；
- 每条都带编号与来源 URL，方便点开核对；
- 数字照抄 `digest.json`，不四舍五入、不合并同类项、不换算单位；
- 来源不可达或工具缺失时，在该来源的计数行写「不可用」，不要写 0 冒充「没有」。
  0 是事实，不可用是缺数据，两者不得混写。

## 四、待跟进与待确认的判定（解释用，判定值看 `digest.json`）

| 原因码 | 含义 |
| --- | --- |
| `stale_open` | 非草稿 PR 仍开着，创建至今 ≥ `window.stale_days` 天 |
| `unanswered_open` | 开着的 Issue 一条评论都没有，创建至今 ≥ `window.stale_days` 天 |
| `long_open` | 开着的 Issue 创建至今 ≥ `window.long_open_days` 天 |

阈值取 `digest.json` 的 `window`（缺省 7 / 30 天，可用参数覆盖）。同一条命中多条
规则时脚本只给一条，**不要把原因叠加写**。

## 五、失败与缺数据

- 脚本非零退出：日报只写「失败」段——`stderr` 原文 + 「本次未产生可信数据」；
  **不得**自己估算数字补齐。
- `held_back` 非空：日报末尾补一行「本次有 N 条未计入（原因：`<reason>`），游标未
  越过它们」，下一轮会重读到。
- 某个 MCP 工具缺失：写「该来源不可用」，其余来源照常出数。

## 六、L3 参考

字段级定义、输入形态、`digest.json` 全量字段、游标不变量、固定夹具的期望读数、
六段证据的 `schedule_deliveries` 查询，见**本文件同目录**的 `REFERENCE.md`——即 L1
卡片给出的发布目录（`.basalt/versions/skills/repo-digest/<uid>/<no>/`）里的那一份，
不是工作副本。
