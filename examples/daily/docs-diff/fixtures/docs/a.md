# 值班手册（示例）

> 本文件是 Basalt D4 `docs-diff` 的示例夹具：内容为虚构，不含真实人员、账号或凭据。
> This is a fictional fixture for the Basalt docs-diff demo; it contains no real people, accounts or credentials.

## 交接班

1. 交班人在 09:00 前更新值班记录，写明未闭环事项与联系人。
2. 接班人确认告警通道可用，再接手记录。
3. 未闭环事项超过一天，升级给值班负责人。

## 升级路径

| 级别 | 触发条件 | 通知对象 |
| --- | --- | --- |
| P1 | 核心链路不可用 | 值班负责人 + 研发负责人 |
| P2 | 部分功能降级 | 值班负责人 |
| P3 | 单点异常，有绕行方案 | 当班同学 |
