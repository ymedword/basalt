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
