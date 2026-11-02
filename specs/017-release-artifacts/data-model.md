# Data Model: 制品与版本发布

**Date**: 2026-11-02 | **Spec**: [spec.md](spec.md)

发布工具只写文件，不新增数据库表。以下结构以 JSON 文件落盘；字段名即 JSON 标签。

## Inputs

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| `version` | string | `X.Y.Z`，不含 `v` |
| `tag` | string | `vX.Y.Z`；final 必须指向 HEAD |
| `mode` | string | `rehearsal` / `final` |
| `commit` | string | 40 位十六进制 |
| `repo` | string | 仓库根；不写入发布记录 |
| `started_at` | time | RFC3339 UTC |

## Target

| 字段 | 类型 | 约束 |
| --- | --- | --- |
| `goos` | string | `windows` / `linux` / `darwin` |
| `goarch` | string | `amd64` / `arm64` |
| `tier` | string | `full` / `binary` |

## BuildResult

`target`、`binary`（绝对路径，不落盘）、`version`、`commit`、`go_version`、
`cgo_enabled`、`sha256`。

## PackageResult

| 字段 | 说明 |
| --- | --- |
| `name` | `basalt-<version>-<goos>-<goarch>.<ext>` |
| `archive` | 归档绝对路径（不写进记录） |
| `target` | Target |
| `sha256` | 归档摘要 |
| `binary_sha256` | 包内二进制摘要 |
| `file_count` | 包内文件数 |

## Evidence

`goos`、`goarch`、`tier`、`status`（`tested` / `compiled-only` / `not-tested`）、
`checks`（非空字符串列表）、`reason`（非 tested 时必填）、`archive_sha256`。

## SupportRow

`goos`、`goarch`、`tier`、`status`、`checks`、`reason`。`tested` 必须有检查 ID；
`compiled-only` / `not-tested` 必须有原因。

## ReleaseRecord

| 字段 | 约束 |
| --- | --- |
| `schema` | `1` |
| `record_id` | `<version>-<mode>-<commit12>` |
| `version` / `tag` / `mode` / `commit` | 与 Inputs 一致 |
| `started_at` / `finished_at` | RFC3339 UTC |
| `toolchain` | `go_version`、`releasekit_version` |
| `artifacts` | `name`、`goos`、`goarch`、`tier`、`sha256`、`sbom_sha256`、`provenance_sha256` |
| `evidence` | SupportRow 列表 |
| `signing` | `status`、`algorithm`、`public_key_fingerprint`、`signed_files` |
| `reproducible` | `checked`、`equal`、`second_build_sha256` |
| `support_matrix_sha256` / `notes_sha256` | 文件哈希 |
| `limitations` | 字符串列表 |

发布记录不得出现绝对路径、用户名、主机名、token、私钥内容。

## VerifyPolicy / VerifyReport

`VerifyPolicy`: `public_key`、`trust_file`、`require_signature`。
`VerifyReport`: `verified_files`、`verified_signatures`、`signed`、`errors`。

## Private Snapshot Manifest

| 字段 | 约束 |
| --- | --- |
| `schema` | `1` |
| `created_at` | RFC3339 UTC |
| `files` | `path`（相对路径）、`sha256`、`mode`、`size` |
| `includes` | 生成时使用的相对路径 / glob 列表 |
