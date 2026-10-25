-- ticket-insights 模板的样例库结构（内容为虚构数据，不含真实客户或凭据）。
-- Sample schema for the ticket-insights template; every row it holds is fictional.
--
-- 为什么**故意不加** CHECK / NOT NULL：
-- 这个库要同时装得下客服系统导出的真实毛病——`status` 空串、`priority` 出现约定外
-- 的取值。加上约束这些行就进不来，模板「待确认」那一段也就永远没有东西可写。
-- The constraints are deliberately absent so messy exports can be imported, which is
-- exactly what the "待确认" section is there to surface.
--
-- 由 examples/shared/scripts/make_sample_db.py 建库并灌入 fixtures/rows.json；
-- `.db` 是生成物，不提交。

CREATE TABLE tickets (
    row_id     INTEGER PRIMARY KEY,   -- 导入序号；报告里的「第 N 行」用它
    ticket_id  TEXT    NOT NULL,      -- 工单号（来源内唯一）
    created_at TEXT,                  -- 建单时间，ISO 8601（YYYY-MM-DD HH:MM）
    channel    TEXT,                  -- 来源渠道：app / web / phone
    subject    TEXT,                  -- 工单标题；分类**只**看这一列
    priority   TEXT,                  -- 约定值：low / normal / high；其他值进「待确认」
    status     TEXT                   -- 约定值：open / closed；其他值进「待确认」
);
