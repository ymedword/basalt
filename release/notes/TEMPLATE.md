# Basalt __VERSION__ 发布说明

## 摘要

本次发布交付 __VERSION__ 版本的四平台归档。正式发布前请按「校验与签名」核对产物。

## 变更

- 填写本版本的变更条目。

## 升级步骤

1. 停止旧服务，备份 PostgreSQL、workspace、配置与密钥。
2. 解压对应平台归档，替换 `basalt` 二进制。
3. 运行 `basalt db migrate`，启动服务并检查 `/api/v1/health`。
4. 失败时按「回滚」恢复旧二进制与备份。

## 部署支持矩阵

| 形态 | 状态 | 说明 |
| --- | --- | --- |
| Compose 多 Worker（`docker/compose.multiworker.yaml`） | 已支持 | 一个控制面 `serve` + 至少一个 Worker `gateway` + 共享 PostgreSQL；单宿主编排按 Operations §8.3 |
| `LocalRunner` 执行档 | 已支持 | 默认执行档，本版本唯一验证过的工具执行方式 |
| K8s 单实例 / 多 Worker | 未验证 | 规划项，不得用本版本证据冒充 |
| 数据库 HA（主备 / 自动故障转移） | 未验证 | 不在本版本范围；PostgreSQL 单实例是唯一受支持形态 |
| 真实双宿主（A + B 云服务器） | 未验证 | 编排与自动化按单宿主验证；两宿主实测在首发后补充 |

容器隔离（OCI / gVisor）是实例级 opt-in，不在首发支持矩阵内；开启后 5 个 daily 脚本受影响，4 个 Flow 角色只读不受影响，可回退 local。

多 Worker 拓扑升级：先停全部旧进程，备份数据库、每个工作区、密钥与旧镜像，新控制面先迁移，再启动新 Worker；同 schema 的两个版本之间不宣称制品版本门禁，"不得新旧混跑"是部署纪律。回滚是整组恢复（数据库 + 每个工作区 + 主密钥 + 旧镜像），只恢复数据库不足以回滚。

## 已知问题

- 填写已知限制。

## 校验与签名

只使用下载目录中的 `basalt-verify`，但公钥与校验器哈希必须从发布页 / 运维记录等「带外」渠道核对，下载目录里的同名文件不能当作信任根：

```text
basalt-verify --release-dir <下载目录> --public-key <带外取得的公钥>
```

`SHA256SUMS` 覆盖发布目录中的全部文件，签名在 `SHA256SUMS.sig` 与 `release-record.json.sig`。

## 回滚

停止新服务，恢复升级前的 PostgreSQL、workspace、配置、主密钥与 `basalt.old`，再启动旧版。
