# US20 契约接线

- 静态 isolation 段与独立 execution.allow_run_code，未知键拒绝，正式定义见 docs/contracts/isolation.md。
- Command.Isolation、CommandResult.Execution、ActionRunCode="RUN_CODE" 为 additive；两接口签名不变。
- PG sandbox_executions 与两索引，迁移和正式 isolation.pg.sql 同源。
- HTTP 只加 SandboxView.isolation、RunDetail.isolation_executions；空数组不为 null，无隔离写端点；启用时 reader 缺失拒绝装配。
- CLI `basalt isolation check` 不触库、不执行用户代码，被绑定档不可用则非零退出。
- 八类 error_kind 与四类出网 policy_denial_reason 不变；隔离 reason 闭集：config_missing、not_ready、policy_denied、admission_rejected、admission_unavailable、image_untrusted、input_rejected、create_failed、create_unknown、start_failed、wait_failed、state_unknown、artifact_rejected、artifact_publish_failed、limit_exceeded、canceled、timeout、orphan_cleaned、cleanup_failed。
- 清单 schema 2 保留 app_base/postgres、新增 sandbox.oci/gvisor；源锁允许空，已签名运行清单要求完整合法引用，Ed25519 分离签名复用 releasekit。
