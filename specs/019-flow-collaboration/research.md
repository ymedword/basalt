# US-19 已审设计决定

## 执行与依赖

- Decision：单机 PG 持久顺序状态机；普通 Run 保持同步。
- Rationale：受理确认与中断事实持久，固定有界 Worker，未知副作用不自动重放。
- Alternatives considered：同步短流程不能覆盖长任务；Temporal／并行图／分布式接管超出范围。

## 唯一路径与 G-FT

- Decision：core 消费窄端口，Tool 节点使用 AgentService、共享工具组助手、ToolExecutor 及同一 app 准入。
- Rationale：不旁路身份／快照／历史／审计／配额；固定工具没有模型判断，必须保存结构化真实执行结局，不能解析自然语言当 success。
- Alternatives considered：flow 直接调用 Tool.Execute 或复制消息追加会违反 II／V。实际 Claude 设计／探针复核仍是写分支前置。

## Schema 与新依赖

- Decision：既有 jsonschema/v6 v6.0.3 从测试扩到 tool 生产适配器，无新增坐标或版本调整。
- Rationale：core 只标准库；显式批准子集、拒远程引用／未知关键字、限制深度／大小、精确数值与重复键预检。
- Alternatives considered：切换全部旧工具校验扩大兼容影响，未选。

## 授权及效果

- Decision：原 caller 与 Key 固定，当前权限只能收紧；Agent 仅可信只读工具，写节点冻结输入。
- Rationale：不让后台生命周期变成管理员、不重推理复现未知写。
- Alternatives considered：真实模型质量暂未授权；本地 12 项 mock 是控制协议证据，不能宣称质量优势。
