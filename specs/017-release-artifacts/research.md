# Research: 制品与版本发布

**Date**: 2026-11-02 | **Plan**: [plan.md](plan.md)

本文件只记录实现选型，不替代模块卡；所有事实以
`docs/us/us17-release-artifacts.md` R6 为准。

## D1. 打包实现：扩展既有 `release-package` 还是自包含？

**Decision**: 保留 `scripts/release-package` 作为既有发布包入口，新增
`scripts/releasekit` 自包含实现四平台归档；两者复用确定性归档原则，不互相 import。

**Rationale**: `scripts/release-package` 是 `package main`，且强制“当前宿主 + 当前
架构 + 完整 runtime 清单+许可证扫描”；四平台发布需要跨目标归档、binary 档和签名
记录，直接改造会扩大既有 US-14 验收面。新包只依赖标准库，按同一固定时间戳/排序
规则生成 `.zip` / `.tar.gz`。

**Alternatives**: 把 `release-package` 改成可导入库（会动既有发布路径与测试）；
用 `go run ./scripts/release-package` 逐平台调用（跨平台仍不可行，且生产签名链路
依赖 Go 工具链）。

## D2. 四平台构建如何在单机与 CI 间分工？

**Decision**: `release build --target <goos>/<goarch>` 可在任意宿主交叉构建
`binary` 档；`full` 档只允许在匹配宿主构建（`HostMatches` 失败即拒绝）。CI 矩阵
按目标宿主运行 full，交叉构建两个 binary；汇总机只 `assemble`，不执行非宿主
二进制。

**Rationale**: Go 交叉构建 `CGO_ENABLED=0` 可靠；`full` 档需要匹配宿主验证 runtime
组件与启动，不能在其他宿主伪造证据。

## D3. 升级/恢复如何编排？

**Decision**: `releasekit` 的 `RunUpgrade` 按固定顺序执行：停止旧版（调用方提供
停止命令或已停止）→ `pg_dump` + 复制 workspace + 复制配置/密钥三对象 →
保留 `basalt.old` → 解压新归档替换 → 启动新版并等待 `/health` → 失败则
`pg_restore` + 文件还原 + `basalt.old` 回装。数据库操作通过外部 `pg_dump` /
`pg_restore`（与既有 US-16 drill 同口径），不实现第二套迁移引擎。

**Rationale**: 项目已有 `basalt db export/import/verify/switch` 与 PostgreSQL
drill；发布升级是运维编排，复用数据库原生工具最稳。测试环境缺 `pg_dump` 时
e2e 失败而非跳过。

## D4. 签名格式

**Decision**: Ed25519 分离签名，签名对象是文件原始字节；私钥 PKCS#8 PEM，公钥
PKIX PEM；`.sig` 为 64 字节原始签名。`release-record.json.sig` 先写，
`SHA256SUMS` 再包含它，最后写 `SHA256SUMS.sig`。

**Rationale**: 标准库 `crypto/ed25519` + `encoding/pem` 足够；不引入 minisign /
cosign。`basalt-verify` 与 OpenSSL 均可验证。

## D5. 私有快照格式

**Decision**: tar.gz，条目为相对路径 `manifest.json` + `files/...`；清单记录每个
文件的 SHA-256、mode；恢复时先校验清单与路径（拒绝绝对路径、`..`、符号链接逃逸），
再逐文件核对哈希。

**Rationale**: 标准库可读写；清单随归档走，恢复不依赖源工作区。

## D6. Harness

**Decision**: 18 个 check 按模块卡 §4.5 原样落五个 stage；`required_tests` 逐名
命中；`level` 只用 `source|process|external`；Go 检查一律 `-json -count=1`。

**Rationale**: `scripts/acceptance` 的 stage lint 只接受这些字段；缺 DSN / 缺平台
必须失败，不用 `t.Skip` 冒充通过。
