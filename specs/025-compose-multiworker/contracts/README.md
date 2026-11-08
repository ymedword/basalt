# US25 契约材料

本目录只做指针：权威契约在 `docs/contracts/`，实施时逐条对照；契约与颗粒度文档冲突时以契约固定字面量为准，并在颗粒度文档记录差异裁决。

**本 US 不改 `docs/contracts/**`**：不新增端点、不新增错误码、不新增 `AGENT.md` frontmatter 字段、不改 `schema.pg.sql` 冻结基线，`SupportedSchemaVersion` 保持 12。Demo 准备复用既有端点的既有审计；首次建钥沿用既有管理操作。

## 复用的权威契约

- `docs/contracts/agent-md.md`：`knowledge:` 只读绑定的字面量与校验规则；Demo 准备只经 `ApplyChanges` 改写该字段，不手写 YAML、不新增字段。
- `docs/contracts/openapi.yaml`：`GET/POST /api/v1/collections`、`POST /api/v1/collections/{id}/documents`、`GET /api/v1/agents/{name}/content`、`GET/POST /api/v1/agents/{name}/files`、`GET /api/v1/health`——全部为既有端点，只读复用；worker 列表不经 HTTP，US-24 只交付 CLI `basalt workers list`（只读 DB 端口）。
- `docs/contracts/run-and-errors.md`：Worker / 控制面共享的 Run 终态与审计归一化语义（US-24 已冻结），本 US 只做部署验收，不改判据。
- `docs/contracts/schema.sql` / `docs/contracts/schema.pg.sql`：冻结基线；`HasRuntimeHistory` 只读既有 `runs` / `task_executions` / `agent_lifecycle` / `kb_documents`，`management_operations` 排除。

## 本 US 新增的部署工件（不是产品运行时契约）

- 种子归档内 `seed-manifest.json`（由 `docker/seed-workspace.sh` 在打包时生成，不作为独立仓库文件入库）：`format_version=1`、`created_at`、`source_instance_id`、`files[] {path,size,sha256}`；字段与排除项见模块卡 §3.3，形状见 [data-model.md](../data-model.md) §4。
- 部署变量 `BASALT_ROLE` / `BASALT_CONFIG_TEMPLATE` / `BASALT_MASTER_KEY_FILE` / `BASALT_WORKER_NAME` / `BASALT_DB_BIND`：只用于 Compose / entrypoint，**不是** `basalt.yaml` 的产品配置键（Q-25-01）。
- `docker/compose.multiworker.yaml`：单文件 `profiles: control|worker`；不改 `docker/compose.yaml`（US-18 单实例默认）。
- `docker/basalt.worker.yaml`：Worker 单文件配置模板；必须可独立解析（FR-25-33），不含 `http` / `callers`。
- `release/support-matrix.json`：**schema 与判卷逻辑不改**；拓扑只写在发布说明与 `docs/Operations.md` 的文字里（Q-25-13）。

## 错误与退出码（部署面）

| 场景 | 约定 |
| --- | --- |
| Worker 启动预检失败（缺种子 / 缺密钥 / 不可读 / 权限过宽） | 退出 **78**，不产生新实例、不写业务表 |
| `demo workflow prepare` 部分成功 | 退出 **3**，逐项结果可读、`Partial=true` |
| `demo workflow prepare` 硬失败（模板 / Agent 未安装、server 非法、token 缺失） | 退出 1，提示先安装并启用 |
| `demo workflow prepare` 绑定冲突 | 非零退出，内容不变 |
| `basalt secrets check-key` 校验失败 | 非 0，输出原因；不打印密钥内容 |

实现时逐条对照上列文件；`specs/025-*` 的 `data-model.md` 与 `quickstart.md` 只作设计说明与验证指南，不构成第二份契约。
