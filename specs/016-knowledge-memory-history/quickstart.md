# Quickstart — US-16 验证指南

**Feature**: `016-knowledge-memory-history`｜**Date**: 2026-10-28

> 本文只写**怎么验证**，不写实现细节。前置条件与命令以实际实施后的 Makefile / stage 为准；未落地前按下述口径准备环境。

## 1. 前置条件

- Go 工具链：构建版本由 `go.mod` 的 `toolchain go1.27.3` 固定；`GOPROXY=https://goproxy.cn`。
- PostgreSQL：16/17，已安装 `vector`（0.8.x）与 `pg_trgm`；**独立**的测试库，**不得**指向既有 Basalt 实例库。
- 环境变量（示例，值不进仓库、不进日志）：

  ```text
  BASALT_TEST_PG_DSN=postgres://<user>:${PGPASSWORD}@<host>:<port>/<test_db>?sslmode=disable
  ```

  该 DSN 只用于测试与迁移演练；生产配置走 `${ENV_VAR}` 占位并在加载期解析，残留即拒绝启动。

## 2. 单元面验证（不访问数据库，但**不能**据此判定 `make check`）

```text
go test ./internal/core ./internal/memory ./internal/web ./internal/cli   # 单元面
go test -race ./...                                                       # 竞态（与发布构建分属不同命令）
CGO_ENABLED=0 go build ./...                                              # 单二进制构建
```

预期：单元面通过；`-race` 通过；`CGO_ENABLED=0` 构建成功。

**但 `make check` 整体依赖 PostgreSQL**（D-01 已批准判据面）：`test-integration` 与 `make cover` 在缺 `BASALT_TEST_PG_DSN` 时**失败而非跳过**。无库环境**不得**判定 `make check` 通过，也**不得**以 build tag、stage 子集或降级 SQLite 换取绿态；上面三条命令只作开发期快速反馈，**不构成**验收证据。

## 3. 依赖真实 PostgreSQL 的验证

```text
go test -tags=integration ./internal/storage/postgres ./internal/callers ./internal/knowledge ./internal/history ./internal/compaction ./internal/diagnostics
sh scripts/check-acceptance.sh --stage harness/stages/us16.json    # 实施后生成
```

预期：

- 建表幂等（连续两次迁移无变更）、版本不符拒绝启动、advisory lock 串行。
- 知识库：摄取 → 检索 → 引用解析 → 删除 / 撤权后不再出现。
- 调用方：非法 / 吊销 / 越权 / 超 QPS / 超并发 / 超配额逐项拒绝，被拒请求**零占用**。
- 压缩：切点落在完整工具组边界，摘要可重建，撤权内容不重新注入。
- 诊断：写失败不阻塞业务；审计仍 fail-closed。

## 4. 迁移演练（D-27 口径）

```text
# 0) 先读参考
#    --workspace 指工作区根 <WorkDir>/.basalt（库 = <WorkDir>/.basalt/basalt.db，
#    名册 = <WorkDir>/.basalt/instance.json），**不是** <WorkDir> 本身；
#    basalt.yaml 是 .basalt 的同级部署配置，不属于工作区资产。
#    **演练只用临时副本**，不得指向既有 Basalt 实例。

# 1) 用脚本生成代表性 SQLite v6 库（覆盖 33 张表与边界编码）
# 2) 导出
basalt db export --workspace <ws> --out <pkg>
# 3) 校验导出包（含身份 / 名册 / 清单哈希）
basalt db verify --handoff <pkg>
# 4) 导入到测试 PG
basalt db import --handoff <pkg>
# 5) 逐表核对：行数 / 主键集合 / 关键字段逐值
# 6) 切换演练 + 失败注入（中途失败 → 三维判据 → 停止自动回退）
```

预期：一致率 100%；失败注入时给出**停止**与人工指引，不自动回退；已发出外部写列出补偿清单。

## 5. 端到端 Demo

```text
go test -tags=e2e -count=1 ./internal/e2e -run TestUS16Demo
```

预期：一次 Run 完成「检索 → 引用 → 回答 → 诊断事件」全链路；管理台可见装配记录与证据缺口标记。

## 6. 未验证项（本文件成文时）

- 真实 PostgreSQL 运行期、迁移命令、全部具名测试：**未运行**。
- 百炼 / DeepSeek / GitHub / 飞书真实调用：**未运行**（凭据存在 ≠ 有效）。
- D-05 / D-06 / D-07 / D-24 / D-27 / D-29 / D-30 参数：**未冻结**。
