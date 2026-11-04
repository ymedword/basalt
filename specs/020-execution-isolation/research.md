# US20 实施研究

- Decision：复用 ToolEnv.ToolInvocationID 和 ToolExecutor pending。Rationale：US19 后字段仍存在；Alternative：另造标识或审计路径拒绝。
- Decision：PG 追加 0009。Rationale：当前 SupportedSchemaVersion=8，最高 0008_flow.sql；Alternative：改历史迁移拒绝。
- Decision：Docker CLI 单后端。Rationale：已审无新依赖，WSL 已注册 runc/runsc；Alternative：SDK/Podman/microVM 越界。
- Decision：清单构建后外置、二进制仅公钥。Rationale：避免镜像 digest 自引用；Alternative：把批准 digest 编入镜像拒绝。
- Decision：复用既有发布公钥。Rationale：独立只读研究核对 Ed25519 公钥文件 SHA256 `e4b2c79467f61ebe71a27a3be939f2ee1b5b0244b4bad2f49f9f360793ae580e` 与交接记录一致；未读取私钥；Alternative：生成新信任根拒绝。
- Decision：内部 helper 显式补齐依赖与源目录。Rationale：伪签名缺少实现输入；Alternative：全局状态或业务 context 禁止。
- Decision：WSL 内非 root 原生测试。Rationale：跨 UID 权限断言必须真实成立；Alternative：Windows 远程调 Docker 不作 Linux 证据。
- Decision：输出扩容后重验两条不等式。Rationale：是否同时提高总预算取决于余量；Alternative：机械同时提高两值没有数学依据。

实际容器行为、候选镜像与独立 PG 验收在实现阶段取证，不将工具存在写成行为通过。

独立研究补充：所有 create 路径显式 `--pull=never`，避免 inspect 后镜像被删除时 Docker 默认 pull=missing 外拉；清单先合并与签名，再进入 SHA256SUMS 和 VerifyRelease。US19 FlowAdapterFor 的工具白名单不扩权，run_code 与既有 shell 一样不自动获得 Flow 资格。
