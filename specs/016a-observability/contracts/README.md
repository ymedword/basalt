# US-16A Contract References

语义以 docs/TechnicalDesign.md §5.6 为准；HTTP字面量以 docs/contracts/openapi.yaml 为准；v1 DDL以 docs/contracts/schema.pg.sql 为准，新对象/改变以 internal/storage/postgres/migrations/0006_observability.sql 为准；运行期枚举在 internal/diagnostics/contract.go。

这里只保留索引，不复制第二份契约。新API、配置、枚举、空值及错误语义在 plan / data-model 中有来源与任务映射，实施时一致性测试逐项约束。schema.pg.sql和历史迁移不得修改。
