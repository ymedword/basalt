# 快速验证指南：US-1 工程地基

**Feature**: [spec.md](./spec.md) | **Plan**: [plan.md](./plan.md) | **Date**: 2026-10-15

本文件给出**可复制执行**的验证步骤，用于证明本 feature 已交付且可工作。实现细节见 [tasks.md](./tasks.md) 与颗粒度文档 §3；实体与状态机见 [data-model.md](./data-model.md)。

## 前置条件

| 项 | 判据 | 复核命令 |
| --- | --- | --- |
| Go 工具链 | 与 `toolchain` 行一致（1.27.2） | `go version` |
| GNU Make | 就位 | `make --version` |
| C 编译器 | `-race` 所需 | `gcc --version` |
| 四个检查工具 | 固定版本（gofumpt v0.12.0 / staticcheck v0.8.2 / gosec v2.29.0 / govulncheck v1.9.0） | `gofumpt --version` 等 |
| 模块代理 | 本机直连上游不可达 | 命令显式带 `GOPROXY=https://goproxy.cn` |
| Docker（可选） | 仅 tzdata 隔离组需要 | `docker version` |

## 场景 1：一条命令验证整个工程（P1）

```bash
make check
```

**预期**：七目标（fmt / lint / sec / test / test-integration / cover / build）依次通过，退出码 0。

负向对照（判卷必须真的在判）：

```bash
printf 'package main\nfunc  Broken( {\n' > ./zz_syntax_probe.go   # 语法错误文件
make fmt    # 预期：非零退出（判卷先查退出码）
rm ./zz_syntax_probe.go
```

```bash
gofmt -l . > /dev/null            # 空输出 = 已格式化
printf 'package main\n\nfunc probe( ){ }\n' > ./zz_fmt_probe.go
make fmt    # 预期：非零退出并点名该文件（判卷再判输出）
rm ./zz_fmt_probe.go
```

版本注入与单二进制：

```bash
./bin/basalt --version        # 预期：输出包含 git describe 派生的版本标识
CGO_ENABLED=0 go build ./...  # 预期：零错误（-race 不在本命令内）
```

## 场景 2：一键初始化且二跑幂等（P2）

```bash
mkdir -p /tmp/basalt-demo && cd /tmp/basalt-demo
basalt init                   # 预期：输出 Created 清单 + 退出码 0
```

**预期工作区**：`.basalt/` 下 6 个目录（`agents` `skills` `memory` `logs` `archive` `sessions`）+ 5 个文件（`AGENTS.md` `SOUL.md` `USER.md` `mcp_servers.yaml` `memory/MEMORY.md`）；`.basalt/basalt.db` 就绪（11 张表 + `schema_version` 一行）。

```bash
basalt init                   # 二跑：预期 Created 为空、无报错、退出码 0
```

幂等的逐字节证据：

```bash
find .basalt -type f -exec sha256sum {} + | sort > /tmp/after.txt
# 与首跑后的同一份清单对比：无差异（手工改过的文件也不被覆盖）
```

## 场景 3：无配置诊断（P3）

```bash
cd /tmp/basalt-demo
basalt status                 # 预期：三节报告齐备；节 2 报「未发现配置（路径明示）」；退出码 0
```

异常态对照（各自点名原因、退出码 1）：

```bash
mkdir -p /tmp/basalt-empty && cd /tmp/basalt-empty && basalt status   # 未初始化工作区
cd /tmp/basalt-demo && printf 'providers:\n  - api_key: sk-live-PLAINTEXT\n' > basalt.yaml
basalt status                 # 明文凭证：预期拒绝；三路输出均不含明文值；退出码 1
```

## 场景 4：tzdata 隔离组（CI 必跑步骤）

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.27.2 bash -c '
  set -e
  rm -rf /usr/share/zoneinfo "$(go env GOROOT)/lib/time/zoneinfo.zip"
  test ! -e /usr/share/zoneinfo && test ! -e "$(go env GOROOT)/lib/time/zoneinfo.zip"
  go test -tags=tzisolated ./cmd/basalt -run "TestTZData_Isolated" -count=1
'
```

**预期**：主组通过（内嵌时区库生效）；对照探针**以「时区加载失败」失败**（进程退出码 3 + `TZ_LOAD_FAILED` 标记）——其它失败原因（启动失败 / 崩溃 / 任意退出码）**不算**对照成立。

无 Docker 时：该组**不得**记「通过」，如实登记为未验证（`make check` 不含它）。

## 参考

- 需求与验收场景：[spec.md](./spec.md)
- 实体与状态机：[data-model.md](./data-model.md)
- 命令面契约：[contracts/cli-contract.md](./contracts/cli-contract.md)
- 存储能力面契约：[contracts/storage-api.md](./contracts/storage-api.md)
- 模块级细节（Harness 映射表 / 人工项）：[docs/us/us1-foundation.md](../../docs/us/us1-foundation.md) §4 / §6
