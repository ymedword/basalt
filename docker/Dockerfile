# Basalt 运行镜像：单个 Go 二进制 + 内嵌管理台 + 模板与许可证。
# 不随镜像提供 Node / Python / 内置 MCP 组件（C-14 只宣称 binary 档能力）。
# Basalt runtime image: one Go binary with the embedded console, the config template
# and the license. No Node, Python or built-in MCP components are shipped.

# 底包 digest 冻结在 release/image-lock.json；releasekit 构建时用
# --build-arg BASE_IMAGE=<lock.app_base> 覆盖，这里的默认值必须与锁文件逐字一致
# （CheckImageLock 判卷，漂移即红）。
# The base digest is frozen in release/image-lock.json; releasekit overrides it via
# --build-arg and this default must stay byte-identical to the lock file.
ARG BASE_IMAGE=alpine:3.23@sha256:ff7f0c727dd796aa36787bfcd9a64bf35a6ce72a73451b3359fde849b6703d63
FROM ${BASE_IMAGE}

# ca-certificates 是产品运行期访问外部模型 / 工具的证书来源；busybox 的 wget 供
# 容器健康检查使用。构建期一次性安装，容器启动不下载任何东西（C-12）。
# ca-certificates backs outbound TLS at runtime; busybox wget serves the health
# check. Everything is installed at build time so startup never downloads.
RUN apk add --no-cache ca-certificates

# 命名卷首次创建时继承镜像内目录的属主与权限：预建两个挂载点并交给应用 UID
# （10001 是固定数字 UID，镜像内没有同名用户也成立）。
# Named volumes inherit the ownership of their image-side mount point on first
# creation, so both mount points are pre-created for the fixed numeric UID.
RUN mkdir -p /data /var/lib/basalt /task /output && chown 10001:10001 /data /var/lib/basalt

# Worker 共享主密钥的挂载点必须在镜像内预建：`read_only: true` 的根文件系统里
# Docker 无法为 bind mount 现建目标，缺了它 Worker 直接起不来。权限与
# `LoadMasterKey` 的**直接父目录**判据一致（组 / 其他位为零）——默认 0755 的
# `/run/secrets` 做不到这一点，所以密钥不走那条路。
# The worker's key mount point is pre-created because Docker cannot create a bind
# target on a read-only rootfs; 0700 matches LoadMasterKey's parent-directory rule.
RUN mkdir -p /run/basalt-secrets && chown 10001:10001 /run/basalt-secrets && chmod 0700 /run/basalt-secrets

COPY basalt /opt/basalt/basalt
COPY entrypoint.sh /opt/basalt/entrypoint.sh
COPY basalt.yaml /opt/basalt/basalt.yaml
# Worker 模板与两个运维脚本：entrypoint 按 BASALT_ROLE 选模板，脚本由部署者在宿主机
# 侧执行（导出主密钥 / 导出与导入种子），进镜像只为随制品一起分发、版本对齐。
# The worker template plus the two operator scripts ship with the artifact so the
# entrypoint's role switch and the host-side procedures stay version-matched.
COPY basalt.worker.yaml /opt/basalt/basalt.worker.yaml
COPY export-master-key.sh seed-workspace.sh /opt/basalt/
COPY LICENSE /opt/basalt/LICENSE

# releasekit 暂存时已带上可执行位；这里显式再设一次，让直接 docker build 的
# 产物与暂存路径一致（Windows 上创建的上下文不带 POSIX 位）。
# releasekit already stages the execute bits; setting them again keeps a direct
# docker build identical to the staged one (Windows-created contexts lose POSIX bits).
RUN chmod 0755 /opt/basalt/basalt /opt/basalt/entrypoint.sh \
    /opt/basalt/export-master-key.sh /opt/basalt/seed-workspace.sh

USER 10001:10001
WORKDIR /data
ENTRYPOINT ["/opt/basalt/entrypoint.sh"]
CMD ["serve"]
