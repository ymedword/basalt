# Contract: release CLI 与发布目录

**Date**: 2026-11-02 | **Spec**: [../spec.md](../spec.md)

本契约只约束发布工具的命令行与文件布局，不新增产品 API、配置键或数据库表。

## `scripts/releasekit` 子命令

```text
release preflight  --repo <dir> --version X.Y.Z --tag vX.Y.Z --mode rehearsal|final
release build      --repo <dir> --version X.Y.Z --target <goos>/<goarch> --tier full|binary --out <dir>
release package    --binary <path> --version X.Y.Z --target <goos>/<goarch> --tier full|binary --runtime <dir> --out <dir>
release verify-package --archive <path> --version X.Y.Z
release assemble   --artifact-dir <dir> --evidence-dir <dir> --version X.Y.Z --tag vX.Y.Z \
                   --mode rehearsal|final --out <dir> --notes <path> [--signing-key <path>] [--public-key <path>]
release verify     --release-dir <dir> [--public-key <path>] [--require-signature]
release smoke      --archive <path> --dsn <dsn> --workspace <dir>
release upgrade    --old <archive> --new <archive> --dsn <dsn> --workspace <dir> --config <path>
release repro      --release-dir <dir>
```

- 所有命令同步执行；`context` 只承载取消与截止。
- `package --tier full` 要求 `release/runtime-manifest.json` 声明的组件在 `--runtime` 下齐全；缺清单或缺任一组件即非零退出并列出缺项，不得生成标为 full 的部分 runtime 归档（2026-11-02 补充）。
- `upgrade` 是受限验证入口：未完成真实宿主「停服 → 升级 → 启动 → 失败恢复」闭环验证，不作为生产升级的唯一或默认路径；生产升级按 `docs/Operations.md` §5.1 / §5.2（2026-11-02 补充）。
- `assemble --mode final` 缺 `--signing-key` 或 `--public-key` 立即失败。
- `assemble --mode rehearsal` 可无签名，记录 `signing.status=not_signed`。
- 退出码：0 成功，1 判据失败，2 用法错误。

## `cmd/basalt-verify`

```text
basalt-verify --release-dir <dir> --public-key <path> [--record <path>] [--sums <path>]
```

只读发布目录，校验 `SHA256SUMS`、`SHA256SUMS.sig` 与 `release-record.json.sig`；
缺签名、错密钥、哈希不符均非零退出。不访问网络、不读取源码、不读取
`trust.json` 作为信任根。

## 发布目录布局

```text
dist/release/X.Y.Z/
  basalt-X.Y.Z-<goos>-<goarch>.zip|tar.gz
  basalt-X.Y.Z-<goos>-<goarch>.sbom.cdx.json
  basalt-X.Y.Z-<goos>-<goarch>.provenance.json
  basalt-verify-X.Y.Z-<goos>-<goarch>[.exe]
  evidence/evidence-<goos>-<goarch>.json
  release-manifest.json
  release-record.json
  release-record.json.sig
  SHA256SUMS
  SHA256SUMS.sig
  support-matrix.json
  notes.md
  VERIFY.txt
```

`SHA256SUMS` 覆盖除自身与 `SHA256SUMS.sig` 外的全部文件；每行
`<sha256>  <relative-name>`，按文件名排序。

## 签名顺序

1. 写 `release-record.json`
2. 写 `release-record.json.sig`
3. 写 `SHA256SUMS`（包含第 2 步的 `.sig`）
4. 写 `SHA256SUMS.sig`
5. `VerifyRelease` 验证上述全部内容

不得先写 `SHA256SUMS` 再补记录签名；`VerifyRelease` 不得只看 `SHA256SUMS.sig`。

## 私有快照 CLI

```text
privatebackup snapshot --repo <dir> --out <file.tar.gz> [--include <rel> ...]
privatebackup verify   --archive <file.tar.gz>
privatebackup restore  --archive <file.tar.gz> --dest <dir>
privatebackup inspect  --archive <file.tar.gz>
```

输出必须在 worktree 之外；清单只含相对路径；恢复拒绝绝对路径、`..` 与符号链接逃逸。
