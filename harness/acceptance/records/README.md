# 验收记录发布面

本目录存放**已发布**的验收记录与同次运行报告。记录一经发布**只追加、不覆盖**；历史记录与报告**不得**静默改写（需要新结论时另发新记录）。

## 文件配对

| 路径 | 内容 | 说明 |
| --- | --- | --- |
| `records/<record_id>.json` | 运行记录（`record.json`） | 运行记录 ID = 运行目录名；人工项记录 ID = `<运行目录名>-<item>` 或 `backfill-<UTC 时间戳>` |
| `records/reports/<record_id>.json` | 同次运行报告（`report.json`） | 与记录**同一次运行**，发布时原样拷贝，不得再修改 |
| `../ledger.md` | 记录索引 | 每发布一条记录追加一行，列序固定 |

## 发布步骤（顺序不可交换）

1. **先**跑执行绑定校验：记录声称的 `passed` 必须能在同次报告的同 `check` 具名测试（`passed_tests`）中找到；`run_id` / 源码指纹 / 基线提交 / 脏标记 / 平台 / 架构 / 被测二进制哈希逐项相等。绑定不通过**不得**落盘。
2. 拷入 `records/<record_id>.json` 与 `records/reports/<record_id>.json`（文件名相同、内容为同次运行原件）。
3. 在 `../ledger.md` 末尾追加一行；`record` 列链接 `records/<record_id>.json`。
4. 同一 `record_id` 已存在即停止——运行记录碰撞只能换新目录重跑，人工项记录可另取新 ID。

## 脱敏要求

- 记录、报告与台账中所有路径为**仓库相对路径**、`/` 分隔；不得出现盘符、前导 `/`、反斜杠、Unix 家目录路径。
- 不得出现凭证模式（`sk-*`、`xox[baprs]-*`、`ghp_*`、`AKIA*` 等）。
- 外部接收方只允许出现 `scheme://host`；webhook 的路径、查询串与 userinfo 一律拒绝。
- 截图与日志摘要不得包含密钥、用户数据或 webhook 完整地址。

## 原始日志

逐 check 原始日志留在 `.loop/runs/<run>/`，**不入库**，保留至人工 review ② 结束。发布面只保留报告（含 `checks[].log` 的仓库相对路径与 `passed_tests` / `skipped_tests`）。

## 校验

发布面由 `scripts/acceptance` 的 `TestRecords_*` / `TestLedger_*` / `TestStages_*` 机器判卷：记录与报告配对、台账与记录一一对应、三视图冲突显式、`records/` 与 `ledger.md` 不计入源码指纹。
