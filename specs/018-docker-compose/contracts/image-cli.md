# 契约：镜像构建与校验 CLI

**Feature**: US-18 | **Plan**: [../plan.md](../plan.md) | **模块卡**: §2.10 / §3.1

本契约描述 US-18 新增的两个可对外调用面：`releasekit image` 与 `basalt-verify --require-image-digest`。二者都是发布工具面，**不是**产品运行期 API；不改变 `docs/contracts/` 内的产品契约。

## 1. `releasekit image build`

```text
releasekit image build \
  --repo . \
  --version X.Y.Z \
  --binary dist/build/basalt \
  --base-ref <alpine@sha256:…> \
  --out dist/images \
  --record dist/images/image-record.json
```

| 参数 | 必填 | 默认 | 语义 |
| --- | --- | --- | --- |
| `--repo` | 否 | `.` | 仓库根；Dockerfile / entrypoint / 配置模板 / LICENSE 以此为基准 |
| `--version` | 是 | — | 镜像版本；必须与二进制 `basalt --version` 输出一致 |
| `--binary` | 是 | — | 已构建的 `linux/amd64`、`CGO_ENABLED=0` 二进制 |
| `--base-ref` | 是 | — | 运行阶段基础镜像引用（含 digest）；空值或与锁文件不一致即失败 |
| `--out` | 否 | `dist/images` | 输出目录；SBOM 与 provenance 落此 |
| `--record` | 否 | `<out>/image-record.json` | 镜像记录路径 |
| `--dockerfile` | 否 | `<repo>/docker/Dockerfile` | 构建上下文中的 Dockerfile |
| `--entrypoint` | 否 | `<repo>/docker/entrypoint.sh` | 容器入口脚本 |
| `--config` | 否 | `<repo>/docker/basalt.yaml` | 容器配置模板 |
| `--license` | 否 | `<repo>/LICENSE` | 许可证文件 |

**执行顺序（不得重排）**:

1. `ReadImageLock` 并核对 `--base-ref` == `lock.AppBase`；
2. `CheckBinaryTarget --binary`（buildinfo 核对 `linux/amd64`、`CGO_ENABLED=0`）；
3. `CheckImageContext` 扫描构建上下文（文件名与内容双重判据）；
4. `docker build --build-arg BASE_IMAGE=<digest>`；
5. 镜像内 `--entrypoint /opt/basalt/basalt … --version` 与 `sha256sum /opt/basalt/basalt`；
6. 生成 `ImageRecord`、SBOM、provenance，写记录。

**输出**: `image-record.json`（见 [data-model](../data-model.md)）、`image-sbom.cdx.json`（CycloneDX 1.5）、`image-provenance.json`（自定义 v1）。镜像 tag 规则 `<repo>:<version>-linux-amd64`，本地预演 `repo=basalt`。

**失败语义**: 任一步失败 → 非零退出、错误点名文件或字段；步骤 4 之后失败必须删除本地镜像（不让无记录镜像留存）；记录里的 `sbom_path` / `provenance_path` 是**记录所在目录内的文件名**，不带目录前缀。

## 2. `releasekit image verify`

```text
releasekit image verify \
  --record dist/images/image-record.json \
  --require-local-image
```

| 参数 | 必填 | 语义 |
| --- | --- | --- |
| `--record` | 是 | 镜像记录路径 |
| `--require-local-image` | 否 | 要求 `docker image inspect <tag>` 存在且 Id == `local_id` |
| `--require-repo-digest` | 否 | 要求 `repo_digest` 非空且为 `sha256:<64 hex>` |

校验项：`ImageRecord.Validate`（必填 / 平台 / tier / 哈希格式）、SBOM 与 provenance 文件存在且哈希一致、本地镜像一致性（可选）、`repo_digest` 格式（可选）。退出码 `0` 通过、`1` 失败。

## 3. `releasekit assemble --image-record`

```text
releasekit assemble \
  --artifact-dir dist/build \
  --image-record dist/images/image-record.json \
  --out dist/release/X.Y.Z
```

- `--image-record` 可重复；每条记录先 `LoadImageRecords`（`Validate` + 文件哈希）。
- 复制 SBOM / provenance 进发布目录时**保持文件名**；重名即失败，不静默覆盖。
- 矩阵追加 `kind=image` 行：`tested` 必须有检查 ID，`compiled-only` / `not-tested` 必须有原因；同 `(goos,goarch,kind)` 重复即失败。
- `--image-evidence <检查 ID>` 可重复，只写本次发布**实际执行过**的镜像验收检查；给出后镜像行才是 `tested + 检查 ID`，不给则按记录的 scan 状态写 `not-tested` 并注明原因（没扫描不得写成没测试）。记录已带 `evidence` 时命令行必须给出完全一致的集合，不一致即失败，不做静默覆盖。
- `ReleaseRecord.Images` 与 `ReleaseManifest.Images` 由 `AssembleRelease` 写入，且在 `WriteChecksums` 与签名**之前**完成；不提供“汇总后补字段”的第二条路径。
- 失败时不得产出带镜像字段但未覆盖哈希 / 签名的发布目录。

## 4. `basalt-verify --require-image-digest`

```text
basalt-verify --release-dir <dir> [--require-image-digest]
```

- 默认 `false`；为 `true` 时映射 `VerifyPolicy.RequireRepoDigest`，要求每条 `images[].repo_digest` 非空且匹配 `sha256:<64 hex>`。
- 无镜像记录的旧发布目录**不**因该 flag 失败（无镜像可查）。
- 只要存在 `images[]` 或矩阵 `kind=image` 行，就必须全部通过；矩阵镜像行与 `images[]` 任一方孤儿即失败。
- 矩阵 `kind=image` 行**不得**进入既有归档行循环（`crossCheckRecord` 只处理 `kind` 为空的行）。

## 5. 兼容性

| 输入 | 行为 |
| --- | --- |
| 旧 record / manifest（无 `images[]`） | 仍合法；`omitempty` 不产生空数组 |
| 旧 support-matrix（无 `kind`） | 按归档行处理，语义不变 |
| 旧 `basalt-verify` 调用（无新 flag） | 行为不变 |
| 新记录在旧校验器上 | 镜像字段被忽略；本 US 不承诺跨版本校验器前向兼容 |
