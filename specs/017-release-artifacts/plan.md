# Implementation Plan: 制品与版本发布

**Branch**: `017-release-artifacts` | **Date**: 2026-11-02 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/017-release-artifacts/spec.md`；事实主源为
`docs/us/us17-release-artifacts.md` R6（人工 review ① 已通过）。

## Summary

在现有 `scripts/release-package` 的确定性打包基础上，新增三个标准库工具：
`scripts/releasekit`（构建、打包、校验和、Ed25519 签名、SBOM、记录、矩阵、升级恢复编排）、
`cmd/basalt-verify`（仅下载目录校验器）、`scripts/privatebackup`（私有资产快照/恢复）。
四平台归档由各宿主构建，汇总机只做静态验包与哈希核对；发布签名与对外发布仍归用户。

## Technical Context

**Language/Version**: Go 1.27.3（`go.mod` 的 `toolchain go1.27.3` 与 CI `setup-go` 固定；
`go 1.27` 只是最低要求）。

**Primary Dependencies**: 仅 Go 标准库；`debug/buildinfo`、`crypto/ed25519`、
`archive/zip`、`archive/tar`、`compress/gzip`、`encoding/json`、`os/exec`。
**不新增** `go.mod` / `go.sum` 坐标。

**Storage**: PostgreSQL 17（升级/恢复的数据库对象）；发布目录为文件系统
`dist/release/<version>/`，私有快照写 worktree 之外的 tar.gz。

**Testing**: Go `testing`；source 层单元测试，process 层 e2e（`-tags=e2e`）按
`harness/stages/us17-*.json` 的 `required_tests` 逐名命中；无跳过替代必验项。

**Target Platform**: 构建目标 `windows/amd64`、`linux/amd64`、`linux/arm64`、
`darwin/arm64`；工具自身在四平台编译，CI 矩阵按目标宿主构建。

**Project Type**: 单 Go module + `internal/` 分包；发布工具放 `scripts/` 与 `cmd/`，
不进入产品运行时二进制。

**Performance Goals**: 单目标打包 < 2 分钟（本机实测为准）；发布目录校验与归档数量线性；
不引入网络请求。

**Constraints**: `CGO_ENABLED=0`、`-trimpath`、版本 `X.Y.Z` 且 tag `vX.Y.Z`；
归档确定性（固定 mtime、排序、规范 mode、gzip mtime=0）；路径白名单与内容秘密扫描
分开；`SHA256SUMS` 覆盖除自身与自身签名外的全部文件；Ed25519 分离签名；
私有资产不得进入归档或远端。

**Scale/Scope**: 四平台 × 两档（full 仅 windows/linux amd64，binary 其余）；
18 个 check、五个 stage；单机升级/恢复一次处理一个实例。

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| 原则 | 检查 | 结论 |
| --- | --- | --- |
| I 自实现 ReAct 循环 | 不新增 Agent 框架坐标 | PASS：`go.mod` 零新增 |
| II LLM 客户端只做协议调用 | 不改产品运行时 | PASS：仅新增独立工具与测试 |
| III Provider 显式映射 | 不触碰 | PASS |
| IV 一个目录一个 Agent | 不触碰 | PASS |
| V 审计 fail-closed | 不改审计路径；发布记录不是审计记录 | PASS |
| VI 授权与执行分离 | 不触碰 | PASS |
| VII 同步执行 / context 仅取消截止追踪 | 发布工具同步执行，`context` 只传取消/截止 | PASS |
| VIII 单二进制纪律 | `CGO_ENABLED=0`；`-race` 与发布构建分开；零新增依赖 | PASS |
| IX 不用 ORM，迁移只追加 | 不改 schema / 迁移 | PASS |
| X 范围卡紧 | 不含安装器、自动更新、容器、多节点、托管发布 | PASS |

**Post-design re-check**: 设计仅新增 `scripts/releasekit`、`cmd/basalt-verify`、
`scripts/privatebackup`、`release/` 模板、CI 工作流与 stage；不修改 `cmd/basalt/**`
产品逻辑、`internal/**`（测试除外）、`go.mod` / `go.sum`、契约。**PASS**。

## Project Structure

### Documentation (this feature)

```text
specs/017-release-artifacts/
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/
│   └── release-cli.md
├── tasks.md
└── run-state.md
```

### Source Code (repository root)

```text
scripts/releasekit/          # 新包：标准库发布工具
├── types.go
├── inputs.go
├── build.go
├── package.go
├── checksum.go
├── sign.go
├── verify.go
├── sbom.go
├── provenance.go
├── manifest.go
├── matrix.go
├── record.go
├── evidence.go
├── upgrade.go
├── release.go
├── main.go                  # release CLI
└── *_test.go

cmd/basalt-verify/main.go    # 仅下载目录校验器

scripts/privatebackup/       # 私有资产快照/恢复 CLI
├── snapshot.go
├── archive.go
├── restore.go
├── main.go
└── backup_test.go

scripts/release-package/     # 既有：增量补 CheckReleaseBinaryForTarget
release/
├── package-VERIFY.txt
├── notes/TEMPLATE.md
├── support-matrix.json
└── go-licenses.json

.github/workflows/release.yml
harness/stages/us17-base.json
harness/stages/us17-windows.json
harness/stages/us17-linux.json
harness/stages/us17-linux-arm64.json
harness/stages/us17-aggregate.json
internal/e2e/us17_*_test.go  # -tags=e2e
```

**Structure Decision**: 发布工具与产品运行时解耦；`scripts/releasekit` 不 import
`internal/**`，`cmd/basalt-verify` 只依赖标准库；升级/恢复通过现有 `basalt` 命令与
PostgreSQL 工具编排，不复制迁移引擎。

## Complexity Tracking

无宪法违规，无需豁免。

## 2026-11-02 范围调整

用户明确同意 US-17 从简收口：只交付发布基础工具，首个正式版本在 US-25 验收后由用户触发。计划相应调整：

- `package.go` 增加 full 档完整性硬门禁：按 `release/runtime-manifest.json` 逐项核对目标平台组件，缺项拒绝出包并提示 `--tier binary`；`package_test.go` 覆盖缺清单、缺组件与齐全三种路径。
- `releasekit upgrade` 保持实现但标为受限入口（usage + stderr），`docs/Operations.md` §5.1 / §8.1 明确生产升级走人工步骤；不补建完整自动升级系统。
- 四平台真实宿主、真实 full 制品、升级／恢复实测、正式签名与下载侧独立验证延期到首次正式发布前，登记在模块卡 §6.7、`run-state.md` §4 / §7；本次收口只复跑受影响 check 与必要门禁，复用仍有效的已有证据。
