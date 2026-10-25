-- D3 biz-daily 的样例库结构（内容为虚构数据，不含真实客户、订单或凭据）。
-- Sample schema for the D3 demo; every row it holds is fictional.
--
-- 为什么**故意不加** UNIQUE / CHECK / NOT NULL 约束：
-- 这个样例库要同时装得下「缺失 / 重复 / 非法值」三类坏数据，数据质量检查才有东西
-- 可查。约束会把坏数据挡在入库之前，检查脚本就永远只能报 0。
-- The constraints are deliberately absent: the sample DB must be able to hold the
-- three anomaly classes the quality check is supposed to catch.
--
-- 由 examples/shared/scripts/make_sample_db.py 建库并灌入 fixtures/rows.json；
-- `.db` 是生成物，不提交（见 README「固定输入」）。

CREATE TABLE orders (
    row_id     INTEGER PRIMARY KEY,   -- 导入序号；判断「重复以谁为准」只用它
    order_id   TEXT    NOT NULL,      -- 业务单号；**故意不唯一**，重复本身就是一类异常
    order_date TEXT,                  -- 下单日期，ISO 8601（YYYY-MM-DD）
    region     TEXT,                  -- 大区：华东 / 华北 / 华南 / 西南
    channel    TEXT,                  -- 渠道：直营 / 经销 / 电商
    amount     REAL,                  -- 金额（SQLite 动态类型：此列可存文本，正是一类异常）
    status     TEXT,                  -- paid / refunded / pending；其他值按非法处理
    note       TEXT
);
