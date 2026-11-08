Basalt {{VERSION}}（{{PLATFORM}} 完整包）
==========================================

1. 解压后进入本目录。
2. 首次启动：Windows 运行 `basalt.exe serve`，Linux 运行 `./basalt serve`。
   - 首次启动会在本目录生成 basalt.yaml 与 .basalt/ 工作区；按启动日志补齐
     database.dsn 与 callers.ingress 六项后重新启动，才会生成访问密码。
   - 打开 http://127.0.0.1:8080/console ，用访问密码登录。
   - 访问密码文件（实例私有目录，不在程序目录内）：
       Windows: %LOCALAPPDATA%\basalt\instances\<实例>\admin-token
       Linux:   ${XDG_DATA_HOME:-$HOME/.local/share}/basalt/instances/<实例>/admin-token
     实例 ID 见 .basalt/instance.json 的 instance_id。
3. 随包组件在 runtime/：Node、Python、DBHub、GitHub MCP、Fetch 与自研 docs-readonly。
   首次启动会把三条免凭据 MCP 条目写入 .basalt/mcp_servers.yaml；需要 GitHub 凭据的
   条目只在进程环境存在 GITHUB_PERSONAL_ACCESS_TOKEN 或 GITHUB_TOKEN 时写入。
   配置文件里只出现 ${VAR} 占位，不写凭据值。
4. 第三方组件版本与许可证见 LICENSES/THIRD-PARTY-NOTICES.md；产品许可见 LICENSE。
5. 8080 被占用时先释放端口；监听地址以生成的 basalt.yaml 为准。
6. 首次配置按固定顺序：模型连接 → Agent 绑定与启用 → 知识库（先建集合、摄取，
   再给 Agent 加只读绑定）→ 记忆确认（4 条）→ flows.enabled → 模板发布 → 手动建任务。
   全新目录也可以先执行 `init` 再首次 `serve`：`init` 按同一顺序写出配置、首次
   进度与名册，最后才建 SQLite 源库；`init` 本身不生成主密钥、不发放访问密码。
   详细清单见运维手册「首次配置清单」（docs/Operations.md §1.5）。
7. 托管凭据需要主密钥：首启因缺入口限流或数据库未就绪而退出后，重试会续做开箱安装
   并补发主密钥（资格：无记录、无密钥、零托管密文、零业务历史且 auto_init 未关闭）。
   已有业务历史、显式关闭 auto_init 或处于恢复态时改为人工处理：在控制台「高级：
   实例加密诊断」初始化，或停机执行 basalt secrets init。不得删名册或生成第二把密钥。
8. Compose 多 Worker（可选，双宿主参考部署）：docker/compose.multiworker.yaml 用
   profile control / worker 起一个控制面（serve）与至少一个 Worker（gateway），
   共享同一个 PostgreSQL；Worker 无 HTTP 面、无 admin token。
   - 启动顺序：先起控制面（预检通过、数据库健康），确认管理面可用后再起 Worker；
     Worker 服务没有 depends_on，顺序由部署者保证。
   - 主密钥：控制面导出后复制到 Worker 宿主，只读挂到 /run/basalt-secrets/master_key；
     Worker 不得自行生成主密钥。
   - DSN：Worker 侧执行 init-secrets.sh secrets --copy-from <控制面 secrets 目录>
     --db-host <控制面私网地址>，复制控制面的 DSN，不得自行生成新密码。
   - 种子：控制面停机后先 assets install（运行中的实例持锁，装不了），再导出工作区
     种子并导入 Worker 的 workspace 卷；归档带逐文件 size 与 SHA-256 清单，篡改、
     截断、路径穿越与符号链接一律拒绝。种子不含 basalt.yaml：Worker 的 providers /
     embedding / flows 必须与控制面一致。
   完整步骤见运维手册 docs/Operations.md §8.3。
9. 容器隔离（OCI / gVisor）是实例级 opt-in，默认关闭，不在首发支持矩阵内。开启后
   依赖工具执行的 5 个 daily 脚本会执行失败，4 个 Flow 角色只读原文、不受影响；
   要回退 local 把 sandbox.* 改回默认档并重启即可。本包不声称容器隔离已经验证。

数据位置：工作区 .basalt/ 落程序目录；主密钥与访问密码在用户数据目录，不在程序目录。
升级时替换程序与 runtime/，保留 .basalt/ 与用户数据目录。
