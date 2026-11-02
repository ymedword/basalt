# Quickstart: 制品与版本发布

**Date**: 2026-11-02 | **Plan**: [plan.md](plan.md)

## 前置

- Go 1.27.3（`go version` 由 `toolchain` 自动选择）。
- 发布构建：干净工作区、`CGO_ENABLED=0`。
- 升级/恢复：PostgreSQL 17、`pg_dump` / `pg_restore`（或既有 `basalt db` 命令）。
- 签名：仅使用临时密钥做验证；生产密钥由用户保管。

## 1. 构建与打包（当前宿主）

```sh
export CGO_ENABLED=0
go build -trimpath -o /tmp/basalt ./cmd/basalt
go run ./scripts/releasekit package \
  --binary /tmp/basalt --version 0.1.0 \
  --target windows/amd64 --tier binary --out /tmp/out
go run ./scripts/releasekit verify-package \
  --archive /tmp/out/basalt-0.1.0-windows-amd64.zip --version 0.1.0
```

预期：归档生成；`verify-package` 退出 0，包内 `MANIFEST.json` 与文件哈希一致。

## 2. 预演汇总（无签名）

```sh
go run ./scripts/releasekit assemble \
  --artifact-dir /tmp/artifacts --evidence-dir /tmp/evidence \
  --version 0.1.0 --tag v0.1.0 --mode rehearsal \
  --out /tmp/release --notes release/notes/TEMPLATE.md
go run ./scripts/releasekit verify --release-dir /tmp/release
```

预期：`release-record.json` 的 `mode=rehearsal`、`signing.status=not_signed`；
`verify` 通过哈希校验，不要求签名。

## 3. 正式汇总（临时密钥演练）

```sh
go run ./scripts/releasekit assemble \
  --artifact-dir /tmp/artifacts --evidence-dir /tmp/evidence \
  --version 0.1.0 --tag v0.1.0 --mode final \
  --out /tmp/release-final --notes release/notes/TEMPLATE.md \
  --signing-key /tmp/test-ed25519.pem --public-key /tmp/test-ed25519.pub
go run ./cmd/basalt-verify --release-dir /tmp/release-final --public-key /tmp/test-ed25519.pub
```

预期：`release-record.json.sig` 与 `SHA256SUMS.sig` 存在；篡改任一归档后
`basalt-verify` 非零退出。

## 4. 私有快照

```sh
go run ./scripts/privatebackup snapshot --repo . --out /tmp/private-backup.tar.gz \
  --include .git/info/exclude --include CLAUDE.local.md --include career
go run ./scripts/privatebackup verify --archive /tmp/private-backup.tar.gz
go run ./scripts/privatebackup restore --archive /tmp/private-backup.tar.gz --dest /tmp/restore
```

预期：恢复后逐文件 SHA-256 与清单一致；归档外路径被拒绝。

## 5. Harness

```sh
go test -count=1 ./scripts/releasekit ./scripts/privatebackup ./scripts/release-package
go test -race -count=1 ./scripts/releasekit ./scripts/privatebackup
sh scripts/check-acceptance.sh --stage harness/stages/us17-base.json
```

e2e（需要 `BASALT_TEST_PG_DSN` 与真实归档）：

```sh
BASALT_E2E_ARTIFACT=/abs/basalt-0.1.0-windows-amd64.zip \
BASALT_E2E_VERSION=0.1.0 \
go test -tags=e2e -count=1 ./internal/e2e -run '^TestUS17_Artifact_'
```
