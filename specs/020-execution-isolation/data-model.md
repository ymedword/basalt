# US20 数据模型

SandboxExecution 与 tool_invocations 通过 tool_invocation_id 一对一，外键 CASCADE。字段包含 run/agent/instance/profile/runtime/image/container 身份、状态与时间、产物数量/字节及清理状态；正式 DDL 由 docs/contracts/isolation.pg.sql 承载。

执行状态：creating → running → finished/canceled/timeout/failed/unknown。创建前失败为 failed，结果无法确认必须 unknown；未知不可重放。

清理状态：begin 写 pending，finish/对账仅写 cleaned/failed。任务与派生清理容器、输出目录、staging 全释放才 cleaned。

占容量：state in creating/running OR cleanup_state in pending/failed。unknown+cleaned 不占容量。未清理的历史组受保护，cleaned 随审计删除。

准入启动读取一次 baseline，同锁内 active+baseline 判 N；acquire 配对 release；清理失败额外 pin 转 baseline。

产物 Name/Size/SHA256，仅顶层普通文件；发布显式携带源目录和实例根，在实例锁内统计、校验、复制及失败回滚。
